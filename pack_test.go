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
