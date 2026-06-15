package cherry

import (
	"encoding/binary"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReaderResolveLLM(t *testing.T) {
	input := testPackInput(2, 3)
	blob, err := Build(input)
	require.NoError(t, err)
	reader, err := Open(blob)
	require.NoError(t, err)

	got, ok := reader.ResolveLLM("workspace2", "slug:2:3", "gpt-4o-mini")
	require.True(t, ok)
	assert.Equal(t, "openai", got.Provider)
	assert.Equal(t, "gpt-4o-mini", got.Model)
	assert.Equal(t, uint32(300), got.Rate.RPM)
}

func TestReaderResolveLLMIDs(t *testing.T) {
	blob, err := Build(testPackInput(1, 2))
	require.NoError(t, err)
	reader, err := Open(blob)
	require.NoError(t, err)

	got, ok := reader.ResolveLLMIDs("workspace1", "slug:1:2", "gpt-4o-mini")
	require.True(t, ok)
	assert.Equal(t, "openai", reader.String(got.ProviderSID))
	assert.Equal(t, "gpt-4o-mini", reader.String(got.ModelSID))
	assert.Equal(t, uint32(300), got.Rate.RPM)
	assert.Equal(t, "reject", reader.String(got.Rate.OnExceedSID))
}

func TestReaderResolveLLMPlan(t *testing.T) {
	input := testPackInput(1, 1)
	input.Providers = append(input.Providers,
		Provider{ID: "fallback", Kind: "openai", Endpoint: "https://fallback.example", SecretRef: "env://FALLBACK_KEY"},
	)
	input.Models = append(input.Models,
		Model{ID: "gpt-fallback", Provider: "fallback", Name: "gpt-fallback", Mode: "chat"},
	)
	input.Scopes[0].Principals[0].ModelRoutes = map[string]RoutePlan{
		"gpt-4o-mini": {
			Kind: RouteKindChain,
			Retry: &RetryPolicy{
				RetryOn:         "401,5xx",
				PerTryTimeoutMS: 10000,
			},
			Children: []RoutePlan{
				{Kind: RouteKindTarget, Provider: "openai", Model: "gpt-4o-mini", SecretRef: "env://USER_OPENAI_KEY"},
				{Kind: RouteKindTarget, Provider: "fallback", Model: "gpt-fallback"},
			},
		},
	}
	blob, err := Build(input)
	require.NoError(t, err)
	reader, err := Open(blob)
	require.NoError(t, err)

	plan, ok := reader.ResolveLLMPlan("workspace1", "slug:1:1", "gpt-4o-mini")
	require.True(t, ok)
	require.Equal(t, RouteKindChain, plan.Plan.Kind)
	require.Equal(t, "401,5xx", plan.Plan.RetryOn)
	require.Len(t, plan.Plan.Children, 2)
	assert.Equal(t, "env://USER_OPENAI_KEY", plan.Plan.Children[0].Plan.SecretRef)
	assert.Equal(t, "env://FALLBACK_KEY", plan.Plan.Children[1].Plan.SecretRef)

	ids, ok := reader.ResolveLLMIDs("workspace1", "slug:1:1", "gpt-4o-mini")
	require.True(t, ok)
	assert.Equal(t, "env://USER_OPENAI_KEY", reader.String(ids.SecretSID))
}

func TestReaderProviderBYOKAlwaysDedupesRouteShape(t *testing.T) {
	input := testPackInput(1, 1)
	input.Scopes[0].Principals = []Principal{
		{
			Slug: "slug:user-a",
			ModelRoutes: map[string]RoutePlan{
				"gpt-4o-mini": {
					Kind:      RouteKindTarget,
					Provider:  "openai",
					Model:     "gpt-4o-mini",
					SecretRef: "env://USER_A_OPENAI",
				},
			},
		},
		{
			Slug: "slug:user-b",
			ModelRoutes: map[string]RoutePlan{
				"gpt-4o-mini": {
					Kind:      RouteKindTarget,
					Provider:  "openai",
					Model:     "gpt-4o-mini",
					SecretRef: "env://USER_B_OPENAI",
				},
			},
		},
	}

	blob, err := Build(input)
	require.NoError(t, err)
	reader, err := Open(blob)
	require.NoError(t, err)

	assert.Equal(t, uint32(1), reader.sectionCount(reader.routesOff))

	userA, ok := reader.ResolveLLMIDs("workspace1", "slug:user-a", "gpt-4o-mini")
	require.True(t, ok)
	assert.Equal(t, "env://USER_A_OPENAI", reader.String(userA.SecretSID))

	userB, ok := reader.ResolveLLMIDs("workspace1", "slug:user-b", "gpt-4o-mini")
	require.True(t, ok)
	assert.Equal(t, "env://USER_B_OPENAI", reader.String(userB.SecretSID))
}

func TestReaderProviderBYOKPreferDedupesRouteShapeAndKeepsPlatformFallback(t *testing.T) {
	input := testPackInput(1, 1)
	input.Scopes[0].Principals = []Principal{
		{
			Slug: "slug:user-a",
			ModelRoutes: map[string]RoutePlan{
				"gpt-4o-mini": providerBYOKPreferRoute("env://USER_A_OPENAI"),
			},
		},
		{
			Slug: "slug:user-b",
			ModelRoutes: map[string]RoutePlan{
				"gpt-4o-mini": providerBYOKPreferRoute("env://USER_B_OPENAI"),
			},
		},
	}

	blob, err := Build(input)
	require.NoError(t, err)
	reader, err := Open(blob)
	require.NoError(t, err)

	assert.Equal(t, uint32(2), reader.sectionCount(reader.routesOff))

	userA, ok := reader.ResolveLLMPlan("workspace1", "slug:user-a", "gpt-4o-mini")
	require.True(t, ok)
	require.Len(t, userA.Plan.Children, 2)
	assert.Equal(t, "env://USER_A_OPENAI", userA.Plan.Children[0].Plan.SecretRef)
	assert.Equal(t, "env://OPENAI_API_KEY", userA.Plan.Children[1].Plan.SecretRef)

	userB, ok := reader.ResolveLLMPlan("workspace1", "slug:user-b", "gpt-4o-mini")
	require.True(t, ok)
	require.Len(t, userB.Plan.Children, 2)
	assert.Equal(t, "env://USER_B_OPENAI", userB.Plan.Children[0].Plan.SecretRef)
	assert.Equal(t, "env://OPENAI_API_KEY", userB.Plan.Children[1].Plan.SecretRef)
}

func TestReaderShortChainRouteKeysKeepRetryAndChildOrder(t *testing.T) {
	input := testPackInput(1, 1)
	input.Providers = append(input.Providers,
		Provider{ID: "fallback", Kind: "openai", Endpoint: "https://fallback.example", SecretRef: "env://FALLBACK_KEY"},
	)
	input.Models = append(input.Models,
		Model{ID: "gpt-fallback", Provider: "fallback", Name: "gpt-fallback", Mode: "chat"},
	)
	input.Scopes[0].Principals = []Principal{
		{
			Slug: "slug:user-a",
			ModelRoutes: map[string]RoutePlan{
				"gpt-4o-mini": {
					Kind: RouteKindChain,
					Retry: &RetryPolicy{
						RetryOn:         "401",
						PerTryTimeoutMS: 1000,
					},
					Children: []RoutePlan{
						{Kind: RouteKindTarget, Provider: "openai", Model: "gpt-4o-mini", SecretRef: "env://USER_A_OPENAI"},
						{Kind: RouteKindTarget, Provider: "fallback", Model: "gpt-fallback"},
					},
				},
			},
		},
		{
			Slug: "slug:user-b",
			ModelRoutes: map[string]RoutePlan{
				"gpt-4o-mini": {
					Kind: RouteKindChain,
					Retry: &RetryPolicy{
						RetryOn:         "5xx",
						PerTryTimeoutMS: 2000,
					},
					Children: []RoutePlan{
						{Kind: RouteKindTarget, Provider: "fallback", Model: "gpt-fallback", SecretRef: "env://USER_B_FALLBACK"},
						{Kind: RouteKindTarget, Provider: "openai", Model: "gpt-4o-mini"},
					},
				},
			},
		},
	}

	blob, err := Build(input)
	require.NoError(t, err)
	reader, err := Open(blob)
	require.NoError(t, err)

	assert.Equal(t, uint32(4), reader.sectionCount(reader.routesOff))

	userA, ok := reader.ResolveLLMPlan("workspace1", "slug:user-a", "gpt-4o-mini")
	require.True(t, ok)
	require.Equal(t, RouteKindChain, userA.Plan.Kind)
	assert.Equal(t, "401", userA.Plan.RetryOn)
	assert.Equal(t, uint32(1000), userA.Plan.PerTryTimeoutMS)
	require.Len(t, userA.Plan.Children, 2)
	assert.Equal(t, "openai", userA.Plan.Children[0].Plan.Provider)
	assert.Equal(t, "env://USER_A_OPENAI", userA.Plan.Children[0].Plan.SecretRef)
	assert.Equal(t, "fallback", userA.Plan.Children[1].Plan.Provider)
	assert.Equal(t, "env://FALLBACK_KEY", userA.Plan.Children[1].Plan.SecretRef)

	userB, ok := reader.ResolveLLMPlan("workspace1", "slug:user-b", "gpt-4o-mini")
	require.True(t, ok)
	require.Equal(t, RouteKindChain, userB.Plan.Kind)
	assert.Equal(t, "5xx", userB.Plan.RetryOn)
	assert.Equal(t, uint32(2000), userB.Plan.PerTryTimeoutMS)
	require.Len(t, userB.Plan.Children, 2)
	assert.Equal(t, "fallback", userB.Plan.Children[0].Plan.Provider)
	assert.Equal(t, "env://USER_B_FALLBACK", userB.Plan.Children[0].Plan.SecretRef)
	assert.Equal(t, "openai", userB.Plan.Children[1].Plan.Provider)
	assert.Equal(t, "env://OPENAI_API_KEY", userB.Plan.Children[1].Plan.SecretRef)
}

func TestReaderCredentialSlotOverflowPreservesSplitSecrets(t *testing.T) {
	input := testPackInput(1, 1)
	input.Providers = append(input.Providers,
		Provider{ID: "fallback", Kind: "openai", Endpoint: "https://fallback.example", SecretRef: "env://FALLBACK_KEY"},
	)
	input.Models = append(input.Models,
		Model{ID: "gpt-fallback", Provider: "fallback", Name: "gpt-fallback", Mode: "chat"},
	)
	input.Scopes[0].Principals = []Principal{
		{
			Slug: "slug:user-a",
			ModelRoutes: map[string]RoutePlan{
				"gpt-4o-mini": {
					Kind: RouteKindSplit,
					Split: []WeightedRoutePlan{
						{
							Weight: 70,
							Plan: RoutePlan{
								Kind:      RouteKindTarget,
								Provider:  "openai",
								Model:     "gpt-4o-mini",
								SecretRef: "env://USER_A_OPENAI",
							},
						},
						{
							Weight: 30,
							Plan: RoutePlan{
								Kind:      RouteKindTarget,
								Provider:  "fallback",
								Model:     "gpt-fallback",
								SecretRef: "env://USER_A_FALLBACK",
							},
						},
					},
				},
			},
		},
	}

	blob, err := Build(input)
	require.NoError(t, err)
	reader, err := Open(blob)
	require.NoError(t, err)

	plan, ok := reader.ResolveLLMPlan("workspace1", "slug:user-a", "gpt-4o-mini")
	require.True(t, ok)
	require.Equal(t, RouteKindSplit, plan.Plan.Kind)
	require.Len(t, plan.Plan.Children, 2)
	assert.Equal(t, uint32(70), plan.Plan.Children[0].Weight)
	assert.Equal(t, "env://USER_A_OPENAI", plan.Plan.Children[0].Plan.SecretRef)
	assert.Equal(t, uint32(30), plan.Plan.Children[1].Weight)
	assert.Equal(t, "env://USER_A_FALLBACK", plan.Plan.Children[1].Plan.SecretRef)

	ids, ok := reader.ResolveLLMIDs("workspace1", "slug:user-a", "gpt-4o-mini")
	require.True(t, ok)
	assert.Equal(t, "env://USER_A_OPENAI", reader.String(ids.SecretSID))
}

func providerBYOKPreferRoute(secretRef string) RoutePlan {
	return RoutePlan{
		Kind: RouteKindChain,
		Retry: &RetryPolicy{
			RetryOn:         "401,connect-failure,reset,5xx",
			PerTryTimeoutMS: 1000,
		},
		Children: []RoutePlan{
			{
				Kind:      RouteKindTarget,
				Provider:  "openai",
				Model:     "gpt-4o-mini",
				SecretRef: secretRef,
			},
			{
				Kind:     RouteKindTarget,
				Provider: "openai",
				Model:    "gpt-4o-mini",
			},
		},
	}
}

func TestReaderManifestAndBoundsHelpers(t *testing.T) {
	blob, manifest, err := BuildWithManifest(testPackInput(1, 1))
	require.NoError(t, err)

	gotManifest, err := ReadManifest(blob)
	require.NoError(t, err)
	assert.Equal(t, manifest, gotManifest)

	require.NoError(t, ValidateManifest(blob, manifest))

	reader, err := Open(blob)
	require.NoError(t, err)
	reader.stringsOff = 0
	require.Error(t, reader.validateOffsets())

	corrupt := append([]byte{}, blob...)
	put32(corrupt[headerStringsOff:headerStringsOff+4], 0)
	binary.LittleEndian.PutUint64(corrupt[headerChecksumOff:headerChecksumOff+8], checksum(corrupt[headerSize:]))
	_, err = Open(corrupt)
	require.Error(t, err)

	require.Zero(t, reader.read32(-1))
	require.Zero(t, reader.read64(-1))
}

func TestReaderResolveModelMetadata(t *testing.T) {
	blob, err := Build(testPackInput(1, 1))
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	reader, err := Open(blob)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	model, ok := reader.ResolveModel("gpt-4o-mini")
	if !ok {
		t.Fatal("ResolveModel() ok = false, want true")
	}
	if model.Mode != "chat" || model.MetadataJSON == "" {
		t.Fatalf("ResolveModel() = %#v, want mode and metadata", model)
	}
	assert.Equal(t, []string{"text", "image"}, model.Modalities.Input)
	assert.Equal(t, []string{"text"}, model.Modalities.Output)
	require.JSONEq(t, `0.42`, string(model.AdditionalPricePerMillion["web_search_per_thousand_sources"]))
	require.JSONEq(t, `16384`, string(model.Limits["max_output_tokens"]))
	if !reader.ModelCapability("gpt-4o-mini", "vision") {
		t.Fatal("ModelCapability(vision) = false, want true")
	}
	if reader.ModelCapability("gpt-4o-mini", "image_generation") {
		t.Fatal("ModelCapability(image_generation) = true, want false")
	}
}

func TestReaderProviders(t *testing.T) {
	blob, err := Build(testPackInput(1, 1))
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	reader, err := Open(blob)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	provider, ok := reader.ResolveProvider("openai")
	if !ok {
		t.Fatal("ResolveProvider() ok = false, want true")
	}
	if provider.Kind != "openai" || provider.Endpoint != "https://api.openai.com" || provider.SecretRef != "env://OPENAI_API_KEY" {
		t.Fatalf("ResolveProvider() = %#v", provider)
	}
	providers := reader.Providers()
	if len(providers) != 1 || providers[0].ID != "openai" {
		t.Fatalf("Providers() = %#v, want openai", providers)
	}
}

func TestReaderProviderDescriptions(t *testing.T) {
	input := testPackInput(1, 1)
	input.Providers = append(input.Providers,
		Provider{
			ID:        "anthropic",
			Kind:      "anthropic",
			Endpoint:  "https://api.anthropic.com",
			AuthType:  "anthropic",
			SecretRef: "env://ANTHROPIC_API_KEY",
			Extra: map[string]string{
				"anthropic_version": "2023-06-01",
			},
		},
		Provider{
			ID:       "bedrock",
			Kind:     "openai",
			Endpoint: "https://bedrock-runtime.us-east-1.amazonaws.com",
			AuthType: "aws",
			Extra: map[string]string{
				"aws_region": "us-east-1",
			},
		},
		Provider{
			ID:       "gemini",
			Kind:     "openai",
			Endpoint: "https://generativelanguage.googleapis.com",
			AuthType: "gemini",
		},
		Provider{
			ID:       "vertex_anthropic",
			Kind:     "anthropic",
			Endpoint: "https://us-east5-aiplatform.googleapis.com",
			AuthType: "gcp",
			Extra: map[string]string{
				"anthropic_version": "vertex-2023-10-16",
				"gcp_location":      "us-east5",
			},
		},
	)
	input.Models = append(input.Models,
		Model{ID: "claude-haiku-4-5", Provider: "anthropic", Name: "claude-haiku-4-5-20251001", Mode: "chat"},
		Model{ID: "amazon.nova-lite-v1:0", Provider: "bedrock", Name: "amazon.nova-lite-v1:0", Mode: "chat"},
		Model{ID: "gemini-2.5-flash", Provider: "gemini", Name: "gemini-2.5-flash", Mode: "chat"},
		Model{ID: "vertex/claude-opus-4", Provider: "vertex_anthropic", Name: "claude-opus-4@20250514", Mode: "chat"},
	)

	blob, err := Build(input)
	require.NoError(t, err)
	reader, err := Open(blob)
	require.NoError(t, err)

	descriptions := reader.ProviderDescriptions()
	require.Len(t, descriptions, 5)
	byID := map[string]ProviderDescription{}
	for _, description := range descriptions {
		byID[description.ID] = description
	}

	assert.Equal(t, "bearer", byID["openai"].AuthType)
	assert.Equal(t, []string{"gpt-4o-mini"}, byID["openai"].ModelIDs)
	assert.Equal(t, "anthropic", byID["anthropic"].AuthType)
	assert.Equal(t, []string{"claude-haiku-4-5"}, byID["anthropic"].ModelIDs)
	assert.Equal(t, "aws", byID["bedrock"].AuthType)
	assert.Equal(t, "us-east-1", byID["bedrock"].Extra["aws_region"])
	assert.Equal(t, "gemini", byID["gemini"].AuthType)
	assert.Equal(t, "gcp", byID["vertex_anthropic"].AuthType)
	assert.Equal(t, "us-east5", byID["vertex_anthropic"].Extra["gcp_location"])
}

// TestProviderPathPrefixRoundTrip guards against the pre-intern bug where a
// non-empty PathPrefix or AuthType that appears only in a provider record would
// be written with an ID beyond the frozen string table, making it unreadable.
func TestProviderPathPrefixRoundTrip(t *testing.T) {
	input := testPackInput(1, 1)
	// Inject a provider whose PathPrefix and AuthType are unique strings that
	// would be missed by the pre-intern loop if it didn't include them.
	input.Providers = append(input.Providers, Provider{
		ID:         "custom",
		Kind:       "openai",
		Endpoint:   "https://custom.example.com",
		SecretRef:  "env://CUSTOM_KEY",
		AuthType:   "custom-auth",
		PathPrefix: "/api/v2",
	})
	// Add a model to satisfy the scope route requirements.
	input.Models = append(input.Models, Model{
		ID: "custom-model", Provider: "custom", Name: "custom-model", Mode: "chat",
	})

	blob, err := Build(input)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	r, err := Open(blob)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	info, ok := r.ResolveProvider("custom")
	if !ok {
		t.Fatal("ResolveProvider(custom) not found")
	}
	if info.PathPrefix != "/api/v2" {
		t.Errorf("PathPrefix = %q, want %q", info.PathPrefix, "/api/v2")
	}
	if info.AuthType != "custom-auth" {
		t.Errorf("AuthType = %q, want %q", info.AuthType, "custom-auth")
	}
}

func TestProviderExtraRoundTrip(t *testing.T) {
	input := testPackInput(1, 1)
	input.Providers[0].Extra = map[string]string{
		"anthropic_version": "2023-06-01",
		"aws_region":        "us-east-1",
	}

	blob, err := Build(input)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	reader, err := Open(blob)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	info, ok := reader.ResolveProvider(input.Providers[0].ID)
	if !ok {
		t.Fatalf("ResolveProvider(%q) = false", input.Providers[0].ID)
	}
	if got := info.Extra["anthropic_version"]; got != "2023-06-01" {
		t.Fatalf("anthropic_version = %q", got)
	}
	if got := info.Extra["aws_region"]; got != "us-east-1" {
		t.Fatalf("aws_region = %q", got)
	}
}

func TestReaderMCPServers(t *testing.T) {
	blob, err := Build(testPackInput(1, 1))
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	reader, err := Open(blob)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	servers := reader.MCPServers()
	if len(servers) != 2 {
		t.Fatalf("MCPServers() len = %d, want 2", len(servers))
	}
	if servers[0].ID != "github" || servers[1].ID != "kiwi" {
		t.Fatalf("MCPServers() IDs = [%s, %s], want [github, kiwi]", servers[0].ID, servers[1].ID)
	}

	info, ok := reader.ResolveMCPServer("github")
	if !ok {
		t.Fatal("ResolveMCPServer(\"github\") ok = false, want true")
	}
	if info.Endpoint != "https://api.github.com" || info.AuthType != "bearer" || info.SecretRef != "env://GITHUB_MCP_TOKEN" {
		t.Fatalf("ResolveMCPServer(\"github\") = %#v", info)
	}

	_, ok = reader.ResolveMCPServer("missing")
	if ok {
		t.Fatal("ResolveMCPServer(\"missing\") ok = true, want false")
	}
}

func TestReaderOpensPackWithoutMCPEntries(t *testing.T) {
	input := testPackInput(1, 1)
	input.MCPServers = nil
	for i := range input.Scopes {
		input.Scopes[i].MCPProfiles = nil
	}

	blob, err := Build(input)
	require.NoError(t, err)
	// This is the boundary case that used to make Open reject LLM-only packs:
	// there are no MCP path records, so the MCP path index starts at EOF.
	require.Equal(t, uint32(len(blob)), u32(blob[headerMCPPathsOff:headerMCPPathsOff+4]))

	reader, err := Open(blob)
	require.NoError(t, err)

	_, ok := reader.ResolveLLMIDs("workspace1", "slug:1:1", "gpt-4o-mini")
	require.True(t, ok)
	assert.Empty(t, reader.MCPServers())

	paths, ok := reader.MCPPaths("workspace1")
	require.True(t, ok)
	assert.Empty(t, paths)

	_, ok = reader.ResolveMCPIDs("workspace1", "profile-dev-tools")
	require.False(t, ok)
}

func TestReaderV1ModelsJSON(t *testing.T) {
	blob, err := Build(testPackInput(1, 1))
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	reader, err := Open(blob)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	payload, err := reader.V1ModelsJSON()
	if err != nil {
		t.Fatalf("V1ModelsJSON() error = %v", err)
	}
	var got v1ModelsResponse
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("unmarshal V1ModelsJSON() error = %v", err)
	}
	if got.Object != "list" || len(got.Data) != 1 {
		t.Fatalf("V1ModelsJSON() = %#v, want one model list", got)
	}
	model := got.Data[0]
	if model.ID != "gpt-4o-mini" || !model.SupportsVision || model.SupportsReasoning {
		t.Fatalf("V1ModelsJSON() model = %#v, want vision without reasoning", model)
	}
	if model.InputPrice != "0.00000015" || model.ContextWindow != 128000 || model.MaxOutputTokens != 16384 {
		t.Fatalf("V1ModelsJSON() pricing/limits = %#v", model)
	}

	filtered, err := reader.V1ModelsJSONForProvider("missing")
	if err != nil {
		t.Fatalf("V1ModelsJSONForProvider() error = %v", err)
	}
	var missing v1ModelsResponse
	if err := json.Unmarshal(filtered, &missing); err != nil {
		t.Fatalf("unmarshal filtered V1ModelsJSON() error = %v", err)
	}
	if len(missing.Data) != 0 {
		t.Fatalf("V1ModelsJSONForProvider(missing) data = %#v, want empty", missing.Data)
	}
}

func TestReaderResolveMCPProfile(t *testing.T) {
	blob, err := Build(testPackInput(1, 1))
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	reader, err := Open(blob)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	ids, ok := reader.ResolveMCPIDs("workspace1", "profile-dev-tools")
	if !ok {
		t.Fatal("ResolveMCPIDs() ok = false, want true")
	}
	if len(ids.Tools) != 2 {
		t.Fatalf("ResolveMCPIDs() tools = %d, want 2", len(ids.Tools))
	}
	if reader.String(ids.Tools[0].ExposedNameSID) != "github__list-repos" {
		t.Fatalf("first exposed tool = %q", reader.String(ids.Tools[0].ExposedNameSID))
	}

	got, ok := reader.ResolveMCP("workspace1", "profile-dev-tools")
	if !ok {
		t.Fatal("ResolveMCP() ok = false, want true")
	}
	if got.Tools[0].Server != "github" || got.Tools[0].Tool != "list-repos" {
		t.Fatalf("ResolveMCP() first tool = %#v", got.Tools[0])
	}

	tool, ok := reader.ResolveMCPToolIDs("workspace1", "profile-dev-tools", "kiwi__search-flight")
	if !ok {
		t.Fatal("ResolveMCPToolIDs() ok = false, want true")
	}
	if reader.String(tool.ServerSID) != "kiwi" || reader.String(tool.ToolSID) != "search-flight" {
		t.Fatalf("ResolveMCPToolIDs() = %q/%q", reader.String(tool.ServerSID), reader.String(tool.ToolSID))
	}
}

func TestReaderDedupesMCPToolsetsAcrossInputOrder(t *testing.T) {
	input := testPackInput(1, 1)
	tools := input.Scopes[0].MCPProfiles[0].Tools
	input.Scopes[0].MCPProfiles = append(input.Scopes[0].MCPProfiles, MCPProfile{
		Path: "profile-dev-tools-reordered",
		Tools: []MCPToolBinding{
			tools[1],
			tools[0],
		},
	})

	blob, err := Build(input)
	require.NoError(t, err)
	reader, err := Open(blob)
	require.NoError(t, err)

	assert.Equal(t, uint32(1), reader.sectionCount(reader.mcpToolsetsOff))

	got, ok := reader.ResolveMCPToolIDs("workspace1", "profile-dev-tools-reordered", "github__list-repos")
	require.True(t, ok)
	assert.Equal(t, "github", reader.String(got.ServerSID))
	assert.Equal(t, "list-repos", reader.String(got.ToolSID))
}

func TestReaderResolveMCPInitialize(t *testing.T) {
	blob, err := Build(testPackInput(1, 1))
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	reader, err := Open(blob)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	got, ok := reader.ResolveMCPInitialize("workspace1", "profile-dev-tools")
	if !ok {
		t.Fatal("ResolveMCPInitialize() ok = false, want true")
	}
	if len(got.Servers) != 2 {
		t.Fatalf("ResolveMCPInitialize() servers = %d, want 2", len(got.Servers))
	}
	if got.Servers[0].Server != "github" || got.Servers[0].Endpoint != "https://api.github.com" {
		t.Fatalf("first server = %#v, want github", got.Servers[0])
	}
	if got.Servers[0].AuthType != "bearer" || got.Servers[0].SecretRef != "env://GITHUB_PROFILE_TOKEN" {
		t.Fatalf("first server auth = %#v, want profile github auth", got.Servers[0])
	}
	if got.Servers[1].Server != "kiwi" || got.Servers[1].SecretRef != "env://KIWI_MCP_TOKEN" {
		t.Fatalf("second server = %#v, want kiwi default auth", got.Servers[1])
	}
}

func TestReaderResolveMCPProfileAuth(t *testing.T) {
	input := testPackInput(1, 1)
	auth := MCPProfileAuth{
		Type:      "oauth-token",
		Provider:  "builtin",
		SecretRef: "env://MCP_OAUTH_CLIENT_SECRET",
		OAuth: MCPOAuthConfig{
			TokenEndpoint: "https://auth.example.com/oauth/token",
			ClientID:      "github-client",
			Audience:      "https://api.github.com",
			Scopes:        []string{"repo:read", "user:email"},
		},
	}
	input.Scopes[0].MCPProfiles[0].Auth = auth

	blob, err := Build(input)
	require.NoError(t, err)
	reader, err := Open(blob)
	require.NoError(t, err)

	init, ok := reader.ResolveMCPInitialize("workspace1", "profile-dev-tools")
	require.True(t, ok)
	require.Len(t, init.Servers, 2)
	assert.Equal(t, auth, init.Auth)

	mcp, ok := reader.ResolveMCP("workspace1", "profile-dev-tools")
	require.True(t, ok)
	assert.Equal(t, auth, mcp.Auth)

	ids, ok := reader.ResolveMCPIDs("workspace1", "profile-dev-tools")
	require.True(t, ok)
	assert.Equal(t, "oauth-token", reader.String(ids.Auth.TypeSID))
	assert.Equal(t, "builtin", reader.String(ids.Auth.ProviderSID))
	assert.Equal(t, "env://MCP_OAUTH_CLIENT_SECRET", reader.String(ids.Auth.SecretSID))
	assert.Equal(t, "https://auth.example.com/oauth/token", reader.String(ids.Auth.OAuth.TokenEndpointSID))
	assert.Equal(t, "github-client", reader.String(ids.Auth.OAuth.ClientIDSID))
	assert.Equal(t, "https://api.github.com", reader.String(ids.Auth.OAuth.AudienceSID))

	paths, ok := reader.MCPPaths("workspace1")
	require.True(t, ok)
	require.NotEmpty(t, paths)
	assert.Equal(t, auth, paths[0].Auth)
}

func TestBuildRejectsConflictingMCPInitializeAuth(t *testing.T) {
	input := testPackInput(1, 1)
	input.Scopes[0].MCPProfiles[0].Tools = append(input.Scopes[0].MCPProfiles[0].Tools, MCPToolBinding{
		ExposedName: "github__other",
		Server:      "github",
		Tool:        "other",
		SecretRef:   "env://OTHER_GITHUB_TOKEN",
		AuthType:    "bearer",
	})
	if _, err := Build(input); err == nil {
		t.Fatal("Build() error = nil, want conflicting MCP auth error")
	}
}

func TestBuildRejectsConflictingMCPInitializeAuthSmallAndLargeToolsets(t *testing.T) {
	for _, tc := range []struct {
		name      string
		toolCount int
	}{
		{name: "small", toolCount: 3},
		{name: "large", toolCount: smallMCPToolsetAuthLinearLimit + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := testPackInput(1, 1)
			tools := []MCPToolBinding{{
				ExposedName: "github__first",
				Server:      "github",
				Tool:        "first",
				SecretRef:   "env://GITHUB_PROFILE_TOKEN",
				AuthType:    "bearer",
			}}
			for i := 0; i < tc.toolCount-2; i++ {
				tools = append(tools, MCPToolBinding{
					ExposedName: "kiwi__tool_" + itoa(i),
					Server:      "kiwi",
					Tool:        "tool_" + itoa(i),
					SecretRef:   "env://KIWI_MCP_TOKEN",
					AuthType:    "bearer",
				})
			}
			tools = append(tools, MCPToolBinding{
				ExposedName: "github__second",
				Server:      "github",
				Tool:        "second",
				SecretRef:   "env://OTHER_GITHUB_TOKEN",
				AuthType:    "bearer",
			})
			input.Scopes[0].MCPProfiles[0].Tools = tools

			_, err := Build(input)
			require.Error(t, err)
			assert.Contains(t, err.Error(), `mcp profile "profile-dev-tools" has conflicting auth for server "github"`)
		})
	}
}

func TestManifestValidation(t *testing.T) {
	blob, manifest, err := BuildWithManifest(testPackInput(1, 1))
	if err != nil {
		t.Fatalf("BuildWithManifest() error = %v", err)
	}
	if _, err := OpenWithManifest(blob, manifest); err != nil {
		t.Fatalf("OpenWithManifest() error = %v", err)
	}

	badVersion := manifest
	badVersion.FormatVersion++
	if _, err := OpenWithManifest(blob, badVersion); err == nil {
		t.Fatal("OpenWithManifest() version error = nil, want error")
	}

	badSize := manifest
	badSize.SizeBytes++
	if _, err := OpenWithManifest(blob, badSize); err == nil {
		t.Fatal("OpenWithManifest() size error = nil, want error")
	}

	corrupt := append([]byte{}, blob...)
	corrupt[len(corrupt)-1] ^= 0xff
	if _, err := OpenWithManifest(corrupt, manifest); err == nil {
		t.Fatal("OpenWithManifest() checksum error = nil, want error")
	}
	if _, err := Open(corrupt); err == nil {
		t.Fatal("Open() checksum error = nil, want error")
	}
}

func TestOpenRejectsOldPackVersion(t *testing.T) {
	blob, err := Build(testPackInput(1, 1))
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	put32(blob[headerVersionOff:headerVersionOff+4], 1)
	binary.LittleEndian.PutUint64(blob[headerChecksumOff:headerChecksumOff+8], checksum(blob[headerSize:]))

	if _, err := Open(blob); err == nil {
		t.Fatal("Open() error = nil, want unsupported old version")
	}
}

func TestBundleZstdRoundTrip(t *testing.T) {
	blob, manifest, err := BuildWithManifest(testPackInput(1, 1))
	if err != nil {
		t.Fatalf("BuildWithManifest() error = %v", err)
	}
	bundle := NewBundle("workspace", "workspace1", []string{"workspace1"}, blob, manifest)
	compressed, err := EncodeBundleZstd(bundle)
	if err != nil {
		t.Fatalf("EncodeBundleZstd() error = %v", err)
	}

	opened, err := OpenBundleZstd(compressed)
	if err != nil {
		t.Fatalf("OpenBundleZstd() error = %v", err)
	}
	if opened.Metadata.ScopeKind != "workspace" || opened.Metadata.ScopeID != "workspace1" {
		t.Fatalf("metadata = %#v", opened.Metadata)
	}
	if _, ok := opened.Reader.ResolveLLMIDs("workspace1", "slug:1:1", "gpt-4o-mini"); !ok {
		t.Fatal("ResolveLLMIDs() ok = false, want true")
	}
}

func TestReaderInspector(t *testing.T) {
	blob, err := Build(testPackInput(1, 1))
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	reader, err := Open(blob)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	if got, want := reader.ScopeIDs(), []string{"workspace1"}; len(got) != 1 || got[0] != want[0] {
		t.Fatalf("ScopeIDs() = %v, want %v", got, want)
	}
	routes, ok := reader.PrincipalRoutes("workspace1")
	if !ok {
		t.Fatal("PrincipalRoutes() ok = false")
	}
	if len(routes) != 1 || routes[0].PrincipalSlug != "slug:1:1" || routes[0].RequestedModel != "gpt-4o-mini" {
		t.Fatalf("PrincipalRoutes() = %#v", routes)
	}
	principals, ok := reader.Principals("workspace1")
	if !ok {
		t.Fatal("Principals() ok = false")
	}
	if len(principals) != 1 || principals[0].PrincipalSlug != "slug:1:1" || len(principals[0].RequestedModels) != 1 {
		t.Fatalf("Principals() = %#v", principals)
	}
	paths, ok := reader.MCPPaths("workspace1")
	if !ok {
		t.Fatal("MCPPaths() ok = false")
	}
	if len(paths) != 1 || paths[0].Path != "profile-dev-tools" || len(paths[0].Tools) != 2 {
		t.Fatalf("MCPPaths() = %#v", paths)
	}
}

func TestReaderResolveLLMMissing(t *testing.T) {
	blob, err := Build(testPackInput(1, 1))
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	reader, err := Open(blob)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	tests := []struct {
		name      string
		scope     string
		principal string
		model     string
	}{
		{name: "missing scope", scope: "workspace2", principal: "slug:1:1", model: "gpt-4o-mini"},
		{name: "missing principal", scope: "workspace1", principal: "missing", model: "gpt-4o-mini"},
		{name: "missing model", scope: "workspace1", principal: "slug:1:1", model: "missing"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, ok := reader.ResolveLLM(tt.scope, tt.principal, tt.model)
			if ok {
				t.Fatal("ResolveLLM() ok = true, want false")
			}
		})
	}
}

func TestReaderResolveLLMManyScopes(t *testing.T) {
	const (
		scopes     = 3
		principals = 1000
	)
	blob, err := Build(testPackInput(scopes, principals))
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	reader, err := Open(blob)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	for scopeID := 1; scopeID <= scopes; scopeID++ {
		for principalID := 1; principalID <= principals; principalID += 137 {
			_, ok := reader.ResolveLLM(
				"workspace"+itoa(scopeID),
				"slug:"+itoa(scopeID)+":"+itoa(principalID),
				"gpt-4o-mini",
			)
			if !ok {
				t.Fatalf("ResolveLLM() missing workspace%d slug:%d:%d", scopeID, scopeID, principalID)
			}
		}
	}
}

func testPackInput(scopeCount int, principalsPerScope int) Input {
	providers := []Provider{
		{ID: "openai", Kind: "openai", Endpoint: "https://api.openai.com", SecretRef: "env://OPENAI_API_KEY"},
	}
	models := []Model{
		{
			ID:           "gpt-4o-mini",
			Provider:     "openai",
			Name:         "gpt-4o-mini",
			Mode:         "chat",
			Capabilities: []string{"vision", "tool_choice"},
			Modalities: ModelModalities{
				Input:  []string{"text", "image"},
				Output: []string{"text"},
			},
			AdditionalPricePerMillion: ModelCatalogObject{
				"web_search_per_thousand_sources": json.RawMessage(`0.42`),
			},
			Limits: ModelCatalogObject{
				"max_output_tokens": json.RawMessage(`16384`),
			},
			MetadataJSON: `{"model":"gpt-4o-mini","inputTokensPricePerMillion":"0.1500000000","contextWindow":128000,"capabilities":["vision","tool_choice"],"limits":{"max_output_tokens":16384}}`,
		},
	}
	mcpServers := []MCPServer{
		{ID: "github", Endpoint: "https://api.github.com", AuthType: "bearer", SecretRef: "env://GITHUB_MCP_TOKEN"},
		{ID: "kiwi", Endpoint: "https://mcp.kiwi.com", AuthType: "bearer", SecretRef: "env://KIWI_MCP_TOKEN"},
	}
	scopes := make([]Scope, 0, scopeCount)
	for scopeIndex := 0; scopeIndex < scopeCount; scopeIndex++ {
		scope := Scope{
			ID:         "workspace" + itoa(scopeIndex+1),
			Principals: make([]Principal, 0, principalsPerScope),
			MCPProfiles: []MCPProfile{
				{
					Path: "profile-dev-tools",
					Tools: []MCPToolBinding{
						{ExposedName: "github__list-repos", Server: "github", Tool: "list-repos", AuthType: "bearer", SecretRef: "env://GITHUB_PROFILE_TOKEN"},
						{ExposedName: "kiwi__search-flight", Server: "kiwi", Tool: "search-flight", AuthType: "bearer", SecretRef: "env://KIWI_MCP_TOKEN"},
					},
				},
			},
		}
		for principalIndex := 0; principalIndex < principalsPerScope; principalIndex++ {
			scope.Principals = append(scope.Principals, Principal{
				Slug: "slug:" + itoa(scopeIndex+1) + ":" + itoa(principalIndex+1),
				Route: RoutePlan{
					Provider: "openai",
					Model:    "gpt-4o-mini",
				},
				Rate: RatePolicy{
					USDPerDayCents: 50000,
					RPM:            300,
					OnExceed:       "reject",
				},
			})
		}
		scopes = append(scopes, scope)
	}
	return Input{
		Providers:  providers,
		Models:     models,
		MCPServers: mcpServers,
		Scopes:     scopes,
	}
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for value > 0 {
		i--
		buf[i] = byte('0' + value%10)
		value /= 10
	}
	return string(buf[i:])
}
