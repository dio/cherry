package cherry

import "testing"

func TestReaderResolveLLM(t *testing.T) {
	input := testPackInput(2, 3)
	blob, err := Build(input)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	reader, err := Open(blob)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	got, ok := reader.ResolveLLM("workspace2", "slug:2:3", "gpt-4o-mini")
	if !ok {
		t.Fatal("ResolveLLM() ok = false, want true")
	}
	if got.Provider != "openai" || got.Model != "gpt-4o-mini" || got.Rate.RPM != 300 {
		t.Fatalf("ResolveLLM() = %#v, want openai/gpt-4o-mini/rpm=300", got)
	}
}

func TestReaderResolveLLMIDs(t *testing.T) {
	blob, err := Build(testPackInput(1, 2))
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	reader, err := Open(blob)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	got, ok := reader.ResolveLLMIDs("workspace1", "slug:1:2", "gpt-4o-mini")
	if !ok {
		t.Fatal("ResolveLLMIDs() ok = false, want true")
	}
	if reader.String(got.ProviderSID) != "openai" || reader.String(got.ModelSID) != "gpt-4o-mini" {
		t.Fatalf("ResolveLLMIDs() provider/model = %q/%q", reader.String(got.ProviderSID), reader.String(got.ModelSID))
	}
	if got.Rate.RPM != 300 || reader.String(got.Rate.OnExceedSID) != "reject" {
		t.Fatalf("ResolveLLMIDs() rate = %#v", got.Rate)
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
		{ID: "gpt-4o-mini", Provider: "openai", Name: "gpt-4o-mini"},
	}
	mcpServers := []MCPServer{
		{ID: "github", Endpoint: "https://api.github.com"},
		{ID: "kiwi", Endpoint: "https://mcp.kiwi.com"},
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
						{ExposedName: "github__list-repos", Server: "github", Tool: "list-repos"},
						{ExposedName: "kiwi__search-flight", Server: "kiwi", Tool: "search-flight"},
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
