package cherry

import "testing"

var packBuildSink []byte

func BenchmarkReaderResolveLLM(b *testing.B) {
	const principals = 100000
	blob, err := Build(testPackInput(1, principals))
	if err != nil {
		b.Fatalf("Build() error = %v", err)
	}
	reader, err := Open(blob)
	if err != nil {
		b.Fatalf("Open() error = %v", err)
	}

	b.ReportAllocs()
	var found bool
	for b.Loop() {
		_, found = reader.ResolveLLM("workspace1", "slug:1:77777", "gpt-4o-mini")
	}
	if !found {
		b.Fatal("ResolveLLM() ok = false, want true")
	}
}

func BenchmarkReaderResolveLLMIDs(b *testing.B) {
	const principals = 100000
	blob, err := Build(testPackInput(1, principals))
	if err != nil {
		b.Fatalf("Build() error = %v", err)
	}
	reader, err := Open(blob)
	if err != nil {
		b.Fatalf("Open() error = %v", err)
	}

	b.ReportAllocs()
	var found bool
	var providerID uint32
	for b.Loop() {
		var ids LLMIDs
		ids, found = reader.ResolveLLMIDs("workspace1", "slug:1:77777", "gpt-4o-mini")
		providerID = ids.ProviderID
	}
	if !found || providerID != 0 {
		b.Fatalf("ResolveLLMIDs() found=%v providerID=%d, want true/0", found, providerID)
	}
}

func BenchmarkCherryPackCatalogScale(b *testing.B) {
	const providers = 500
	for _, models := range []int{10, 100, 500, 1000, 5000} {
		b.Run("providers="+itoa(providers)+"/models="+itoa(models), func(b *testing.B) {
			benchmarkBuild(b, benchmarkCatalogInput(providers, models))
		})
	}
}

func BenchmarkCherryPackRouteScale(b *testing.B) {
	cases := []struct {
		name       string
		principals int
		models     int
	}{
		{name: "route_entries=100", principals: 100, models: 1},
		{name: "route_entries=1000", principals: 100, models: 10},
		{name: "route_entries=10000", principals: 1000, models: 10},
		{name: "route_entries=100000", principals: 10000, models: 10},
		{name: "route_entries=1000000", principals: 100000, models: 10},
	}
	for _, shape := range []routeScaleShape{
		routeScaleSimpleTarget,
		routeScaleFallbackChain2,
		routeScaleFallbackChain3,
		routeScaleWeightedSplit2,
		routeScaleBYOKUniqueSecret,
		routeScaleBYOKProviderSecretAlways,
		routeScaleBYOKProviderSecretPrefer,
		routeScaleSharedRatePolicy,
		routeScaleUniqueRatePolicy,
	} {
		b.Run(shape.String(), func(b *testing.B) {
			for _, tc := range cases {
				b.Run(tc.name+"/principals="+itoa(tc.principals)+"/models="+itoa(tc.models), func(b *testing.B) {
					benchmarkBuild(b, benchmarkRouteInput(tc.principals, tc.models, shape))
				})
			}
		})
	}
}

func BenchmarkCherryPackMCPProfileScale(b *testing.B) {
	cases := []struct {
		profiles        int
		toolsPerProfile int
	}{
		{profiles: 100, toolsPerProfile: 10},
		{profiles: 1000, toolsPerProfile: 10},
		{profiles: 10000, toolsPerProfile: 10},
		{profiles: 1000, toolsPerProfile: 100},
	}
	for _, shape := range []mcpProfileScaleShape{
		mcpProfileScaleSharedToolsets,
		mcpProfileScaleUniqueToolsets,
		mcpProfileScaleUniqueSecretRefs,
	} {
		b.Run(shape.String(), func(b *testing.B) {
			for _, tc := range cases {
				b.Run("profiles="+itoa(tc.profiles)+"/tools_per_profile="+itoa(tc.toolsPerProfile), func(b *testing.B) {
					benchmarkBuild(b, benchmarkMCPProfileInput(tc.profiles, tc.toolsPerProfile, shape))
				})
			}
		})
	}
}

func BenchmarkCherryPackRealMixed(b *testing.B) {
	benchmarkBuild(b, benchmarkRealMixedInput(
		100, // workspaces
		100, // principals per workspace
		10,  // models per principal
		100, // MCP profiles per workspace
		10,  // tools per profile
	))
}

func BenchmarkCherryPackLLMChurnOnly(b *testing.B) {
	benchmarkBuild(b, benchmarkRouteInput(100000, 10, routeScaleBYOKProviderSecretPrefer))
}

func BenchmarkCherryPackMCPChurnOnly(b *testing.B) {
	benchmarkBuild(b, benchmarkMCPProfileInput(10000, 10, mcpProfileScaleUniqueToolsets))
}

func BenchmarkCherryPackClusterSplitCandidate(b *testing.B) {
	const (
		workspaces             = 100
		principalsPerWorkspace = 100
		modelsPerPrincipal     = 10
		mcpProfilesPerScope    = 100
		toolsPerProfile        = 10
	)
	b.Run("combined/workspaces=100/routes=100000/mcp_profiles=10000/tools=10", func(b *testing.B) {
		benchmarkBuild(b, benchmarkRealMixedInput(
			workspaces,
			principalsPerWorkspace,
			modelsPerPrincipal,
			mcpProfilesPerScope,
			toolsPerProfile,
		))
	})
	b.Run("llm-only/workspaces=100/routes=100000", func(b *testing.B) {
		benchmarkBuild(b, benchmarkRealLLMInput(
			workspaces,
			principalsPerWorkspace,
			modelsPerPrincipal,
		))
	})
	b.Run("mcp-only/workspaces=100/mcp_profiles=10000/tools=10", func(b *testing.B) {
		benchmarkBuild(b, benchmarkRealMCPInput(
			workspaces,
			mcpProfilesPerScope,
			toolsPerProfile,
		))
	})
}

func benchmarkBuild(b *testing.B, input Input) {
	b.Helper()
	setupBlob, err := Build(input)
	if err != nil {
		b.Fatalf("Build() setup error = %v", err)
	}
	manifest, err := ReadManifest(setupBlob)
	if err != nil {
		b.Fatalf("ReadManifest() error = %v", err)
	}
	bundleBytes, err := EncodeBundleZstd(NewBundle("benchmark", "benchmark", nil, setupBlob, manifest))
	if err != nil {
		b.Fatalf("EncodeBundleZstd() error = %v", err)
	}

	b.ReportAllocs()
	var blob []byte
	b.ResetTimer()
	for range b.N {
		blob, err = Build(input)
		if err != nil {
			b.Fatalf("Build() error = %v", err)
		}
	}
	b.StopTimer()
	packBuildSink = blob
	b.ReportMetric(float64(len(setupBlob)), "blob_bytes")
	b.ReportMetric(float64(len(bundleBytes)), "zstd_bundle_bytes")
	reportPackClusterMetrics(b, setupBlob)
}

func reportPackClusterMetrics(b *testing.B, blob []byte) {
	b.Helper()
	b.ReportMetric(float64(sectionBytes(blob, headerStringsOff, headerProvidersOff)), "strings_bytes")
	b.ReportMetric(float64(sectionBytes(blob, headerProvidersOff, headerRoutesOff)+sectionBytes(blob, headerMCPServersOff, headerMCPToolsetsOff)), "catalog_bytes")
	b.ReportMetric(float64(sectionBytes(blob, headerRoutesOff, headerMCPServersOff)+sectionBytes(blob, headerPrincipalsOff, headerMCPPathsOff)), "llm_policy_bytes")
	b.ReportMetric(float64(sectionBytes(blob, headerMCPToolsetsOff, headerScopesOff)+sectionBytes(blob, headerMCPPathsOff, 0)), "mcp_policy_bytes")
	b.ReportMetric(float64(sectionBytes(blob, headerScopesOff, headerPrincipalsOff)), "scope_index_bytes")
}

func sectionBytes(blob []byte, startHeaderOff int, endHeaderOff int) int {
	start := int(u32(blob[startHeaderOff : startHeaderOff+4]))
	end := len(blob)
	if endHeaderOff != 0 {
		end = int(u32(blob[endHeaderOff : endHeaderOff+4]))
	}
	return end - start
}

type routeScaleShape int

const (
	routeScaleSimpleTarget routeScaleShape = iota
	routeScaleFallbackChain2
	routeScaleFallbackChain3
	routeScaleWeightedSplit2
	routeScaleBYOKUniqueSecret
	routeScaleBYOKProviderSecretAlways
	routeScaleBYOKProviderSecretPrefer
	routeScaleSharedRatePolicy
	routeScaleUniqueRatePolicy
)

func (s routeScaleShape) String() string {
	switch s {
	case routeScaleSimpleTarget:
		return "simple-target-shared"
	case routeScaleFallbackChain2:
		return "fallback-chain-2-shared"
	case routeScaleFallbackChain3:
		return "fallback-chain-3-shared"
	case routeScaleWeightedSplit2:
		return "weighted-split-2-shared"
	case routeScaleBYOKUniqueSecret:
		return "byok-target-unique-secret"
	case routeScaleBYOKProviderSecretAlways:
		return "byok-provider-secret-always"
	case routeScaleBYOKProviderSecretPrefer:
		return "byok-provider-secret-prefer"
	case routeScaleSharedRatePolicy:
		return "rate-policy-shared"
	case routeScaleUniqueRatePolicy:
		return "rate-policy-unique"
	default:
		return "unknown"
	}
}

type mcpProfileScaleShape int

const (
	mcpProfileScaleSharedToolsets mcpProfileScaleShape = iota
	mcpProfileScaleUniqueToolsets
	mcpProfileScaleUniqueSecretRefs
)

func (s mcpProfileScaleShape) String() string {
	switch s {
	case mcpProfileScaleSharedToolsets:
		return "shared-toolsets"
	case mcpProfileScaleUniqueToolsets:
		return "unique-toolsets"
	case mcpProfileScaleUniqueSecretRefs:
		return "unique-secret-refs"
	default:
		return "unknown"
	}
}

func benchmarkCatalogInput(providerCount int, modelCount int) Input {
	providers := benchmarkProviders(providerCount)
	models := benchmarkModels(providerCount, modelCount)
	modelRoutes := make(map[string]RoutePlan, modelCount)
	for i := range modelCount {
		providerID := benchmarkProviderID(i % providerCount)
		modelID := benchmarkModelID(i)
		modelRoutes[modelID] = RoutePlan{Provider: providerID, Model: modelID}
	}
	return Input{
		Providers: providers,
		Models:    models,
		Scopes: []Scope{{
			ID: "workspace1",
			Principals: []Principal{{
				Slug:        "slug:catalog",
				ModelRoutes: modelRoutes,
				Rate:        benchmarkSharedRatePolicy(),
			}},
		}},
	}
}

func benchmarkRouteInput(principalCount int, modelCount int, shape routeScaleShape) Input {
	const providerCount = 3
	providers := benchmarkProviders(providerCount)
	models := benchmarkModels(providerCount, modelCount)
	principals := make([]Principal, 0, principalCount)
	for principalIndex := range principalCount {
		modelRoutes := make(map[string]RoutePlan, modelCount)
		for modelIndex := range modelCount {
			modelID := benchmarkModelID(modelIndex)
			modelRoutes[modelID] = benchmarkRoutePlan(modelIndex, principalIndex, shape)
		}
		rate := benchmarkSharedRatePolicy()
		if shape == routeScaleUniqueRatePolicy {
			rate = RatePolicy{
				USDPerDayCents: uint64(50000 + principalIndex),
				RPM:            uint32(100 + principalIndex%1000),
				OnExceed:       "reject",
			}
		}
		principals = append(principals, Principal{
			Slug:        "slug:route:" + itoa(principalIndex),
			ModelRoutes: modelRoutes,
			Rate:        rate,
		})
	}
	return Input{
		Providers: providers,
		Models:    models,
		Scopes: []Scope{{
			ID:         "workspace1",
			Principals: principals,
		}},
	}
}

func benchmarkRoutePlan(modelIndex int, principalIndex int, shape routeScaleShape) RoutePlan {
	modelID := benchmarkModelID(modelIndex)
	providerID := benchmarkProviderID(modelIndex % 3)
	target := RoutePlan{Kind: RouteKindTarget, Provider: providerID, Model: modelID}
	switch shape {
	case routeScaleSimpleTarget, routeScaleSharedRatePolicy, routeScaleUniqueRatePolicy:
		return target
	case routeScaleFallbackChain2:
		return RoutePlan{
			Kind: RouteKindChain,
			Retry: &RetryPolicy{
				RetryOn:         "connect-failure,reset,5xx",
				PerTryTimeoutMS: 1000,
			},
			Children: []RoutePlan{
				target,
				{Kind: RouteKindTarget, Provider: benchmarkProviderID((modelIndex + 1) % 3), Model: modelID},
			},
		}
	case routeScaleFallbackChain3:
		return RoutePlan{
			Kind: RouteKindChain,
			Retry: &RetryPolicy{
				RetryOn:         "connect-failure,reset,5xx",
				PerTryTimeoutMS: 1000,
			},
			Children: []RoutePlan{
				target,
				{Kind: RouteKindTarget, Provider: benchmarkProviderID((modelIndex + 1) % 3), Model: modelID},
				{Kind: RouteKindTarget, Provider: benchmarkProviderID((modelIndex + 2) % 3), Model: modelID},
			},
		}
	case routeScaleWeightedSplit2:
		return RoutePlan{
			Kind: RouteKindSplit,
			Split: []WeightedRoutePlan{
				{Weight: 90, Plan: target},
				{Weight: 10, Plan: RoutePlan{Kind: RouteKindTarget, Provider: benchmarkProviderID((modelIndex + 1) % 3), Model: modelID}},
			},
		}
	case routeScaleBYOKUniqueSecret:
		target.SecretRef = "env://USER_" + itoa(principalIndex) + "_MODEL_" + itoa(modelIndex)
		return target
	case routeScaleBYOKProviderSecretAlways:
		target.SecretRef = benchmarkProviderSecretRef(principalIndex, providerID)
		return target
	case routeScaleBYOKProviderSecretPrefer:
		target.SecretRef = benchmarkProviderSecretRef(principalIndex, providerID)
		return RoutePlan{
			Kind: RouteKindChain,
			Retry: &RetryPolicy{
				RetryOn:         "401,connect-failure,reset,5xx",
				PerTryTimeoutMS: 1000,
			},
			Children: []RoutePlan{
				target,
				{Kind: RouteKindTarget, Provider: providerID, Model: modelID},
			},
		}
	default:
		return target
	}
}

func benchmarkProviderSecretRef(principalIndex int, providerID string) string {
	return "env://USER_" + itoa(principalIndex) + "_PROVIDER_" + providerID
}

func benchmarkMCPProfileInput(profileCount int, toolsPerProfile int, shape mcpProfileScaleShape) Input {
	const providerCount = 1
	return Input{
		Providers:  benchmarkProviders(providerCount),
		Models:     benchmarkModels(providerCount, 1),
		MCPServers: benchmarkMCPServers(toolsPerProfile),
		Scopes: []Scope{{
			ID: "workspace1",
			Principals: []Principal{{
				Slug: "slug:mcp",
				Route: RoutePlan{
					Provider: benchmarkProviderID(0),
					Model:    benchmarkModelID(0),
				},
				Rate: benchmarkSharedRatePolicy(),
			}},
			MCPProfiles: benchmarkMCPProfiles(0, profileCount, toolsPerProfile, shape),
		}},
	}
}

func benchmarkRealMixedInput(workspaceCount int, principalsPerWorkspace int, modelCount int, profilesPerWorkspace int, toolsPerProfile int) Input {
	return Input{
		Providers:  benchmarkProviders(3),
		Models:     benchmarkModels(3, modelCount),
		MCPServers: benchmarkMCPServers(toolsPerProfile),
		Scopes: benchmarkRealScopes(
			workspaceCount,
			principalsPerWorkspace,
			modelCount,
			profilesPerWorkspace,
			toolsPerProfile,
			true,
			true,
		),
	}
}

func benchmarkRealLLMInput(workspaceCount int, principalsPerWorkspace int, modelCount int) Input {
	return Input{
		Providers: benchmarkProviders(3),
		Models:    benchmarkModels(3, modelCount),
		Scopes: benchmarkRealScopes(
			workspaceCount,
			principalsPerWorkspace,
			modelCount,
			0,
			0,
			true,
			false,
		),
	}
}

func benchmarkRealMCPInput(workspaceCount int, profilesPerWorkspace int, toolsPerProfile int) Input {
	return Input{
		MCPServers: benchmarkMCPServers(toolsPerProfile),
		Scopes: benchmarkRealScopes(
			workspaceCount,
			0,
			0,
			profilesPerWorkspace,
			toolsPerProfile,
			false,
			true,
		),
	}
}

func benchmarkRealScopes(workspaceCount int, principalsPerWorkspace int, modelCount int, profilesPerWorkspace int, toolsPerProfile int, includeLLM bool, includeMCP bool) []Scope {
	scopes := make([]Scope, 0, workspaceCount)
	for workspaceIndex := range workspaceCount {
		scope := Scope{ID: "workspace-" + itoa(workspaceIndex)}
		if includeLLM {
			scope.Principals = benchmarkRealPrincipals(workspaceIndex*principalsPerWorkspace, principalsPerWorkspace, modelCount)
		}
		if includeMCP {
			scope.MCPProfiles = benchmarkMCPProfiles(workspaceIndex*profilesPerWorkspace, profilesPerWorkspace, toolsPerProfile, mcpProfileScaleUniqueToolsets)
		}
		scopes = append(scopes, scope)
	}
	return scopes
}

func benchmarkRealPrincipals(principalBase int, principalCount int, modelCount int) []Principal {
	principals := make([]Principal, 0, principalCount)
	for i := range principalCount {
		principalIndex := principalBase + i
		modelRoutes := make(map[string]RoutePlan, modelCount)
		for modelIndex := range modelCount {
			modelID := benchmarkModelID(modelIndex)
			modelRoutes[modelID] = benchmarkRoutePlan(modelIndex, principalIndex, routeScaleBYOKProviderSecretPrefer)
		}
		principals = append(principals, Principal{
			Slug:        "slug:real:" + itoa(principalIndex),
			ModelRoutes: modelRoutes,
			Rate:        benchmarkSharedRatePolicy(),
		})
	}
	return principals
}

func benchmarkMCPServers(count int) []MCPServer {
	servers := make([]MCPServer, 0, count)
	for i := range count {
		serverID := benchmarkMCPServerID(i)
		servers = append(servers, MCPServer{
			ID:        serverID,
			Endpoint:  "https://" + serverID + ".example.com",
			AuthType:  "bearer",
			SecretRef: "env://MCP_" + itoa(i),
		})
	}
	return servers
}

func benchmarkMCPProfiles(profileBase int, profileCount int, toolsPerProfile int, shape mcpProfileScaleShape) []MCPProfile {
	profiles := make([]MCPProfile, 0, profileCount)
	for i := range profileCount {
		profileIndex := profileBase + i
		tools := make([]MCPToolBinding, 0, toolsPerProfile)
		for toolIndex := range toolsPerProfile {
			serverID := benchmarkMCPServerID(toolIndex)
			exposedName := serverID + "__tool_" + itoa(toolIndex)
			toolName := "tool_" + itoa(toolIndex)
			secretRef := "env://MCP_PROFILE_SHARED"
			switch shape {
			case mcpProfileScaleUniqueToolsets:
				exposedName = serverID + "__profile_" + itoa(profileIndex) + "_tool_" + itoa(toolIndex)
			case mcpProfileScaleUniqueSecretRefs:
				secretRef = "env://MCP_PROFILE_" + itoa(profileIndex)
			}
			tools = append(tools, MCPToolBinding{
				ExposedName: exposedName,
				Server:      serverID,
				Tool:        toolName,
				AuthType:    "bearer",
				SecretRef:   secretRef,
			})
		}
		profiles = append(profiles, MCPProfile{
			Path:  "profile-" + itoa(profileIndex),
			Tools: tools,
		})
	}
	return profiles
}

func benchmarkProviders(count int) []Provider {
	providers := make([]Provider, 0, count)
	for i := range count {
		providerID := benchmarkProviderID(i)
		providers = append(providers, Provider{
			ID:        providerID,
			Kind:      "openai",
			Endpoint:  "https://" + providerID + ".example.com",
			SecretRef: "env://PROVIDER_" + itoa(i),
			AuthType:  "bearer",
		})
	}
	return providers
}

func benchmarkModels(providerCount int, count int) []Model {
	models := make([]Model, 0, count)
	for i := range count {
		modelID := benchmarkModelID(i)
		models = append(models, Model{
			ID:           modelID,
			Provider:     benchmarkProviderID(i % providerCount),
			Name:         modelID + "-backend",
			Mode:         "chat",
			Capabilities: []string{"tool_choice"},
			MetadataJSON: `{"mode":"chat"}`,
		})
	}
	return models
}

func benchmarkProviderID(index int) string {
	return "provider-" + itoa(index)
}

func benchmarkModelID(index int) string {
	return "model-" + itoa(index)
}

func benchmarkMCPServerID(index int) string {
	return "mcp-server-" + itoa(index)
}

func benchmarkSharedRatePolicy() RatePolicy {
	return RatePolicy{
		USDPerDayCents: 50000,
		RPM:            300,
		OnExceed:       "reject",
	}
}
