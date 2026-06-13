package cherry

import "testing"

func BenchmarkReaderResolveMCPToolIDs(b *testing.B) {
	input := testPackInput(1, 1)
	blob, err := Build(input)
	if err != nil {
		b.Fatalf("Build() error = %v", err)
	}
	reader, err := Open(blob)
	if err != nil {
		b.Fatalf("Open() error = %v", err)
	}

	b.ReportAllocs()
	var found bool
	var serverID uint32
	for b.Loop() {
		var ids MCPToolIDs
		ids, found = reader.ResolveMCPToolIDs("workspace1", "profile-dev-tools", "github__list-repos")
		serverID = ids.ServerID
	}
	if !found || serverID != 0 {
		b.Fatalf("ResolveMCPToolIDs() found=%v serverID=%d, want true/0", found, serverID)
	}
}

func BenchmarkSplitViewResolveLLMIDs(b *testing.B) {
	input := testPackInput(1, 100000)
	view := benchmarkSplitView(b, llmOnlyInput(input), mcpOnlyInput(input))

	b.ReportAllocs()
	var found bool
	var providerID uint32
	for b.Loop() {
		var ids LLMIDs
		ids, found = view.ResolveLLMIDs("workspace1", "slug:1:77777", "gpt-4o-mini")
		providerID = ids.ProviderID
	}
	if !found || providerID != 0 {
		b.Fatalf("ResolveLLMIDs() found=%v providerID=%d, want true/0", found, providerID)
	}
}

func BenchmarkSplitViewResolveMCPToolIDs(b *testing.B) {
	input := testPackInput(1, 1)
	view := benchmarkSplitView(b, llmOnlyInput(input), mcpOnlyInput(input))

	b.ReportAllocs()
	var found bool
	var serverID uint32
	for b.Loop() {
		var ids MCPToolIDs
		ids, found = view.ResolveMCPToolIDs("workspace1", "profile-dev-tools", "github__list-repos")
		serverID = ids.ServerID
	}
	if !found || serverID != 0 {
		b.Fatalf("ResolveMCPToolIDs() found=%v serverID=%d, want true/0", found, serverID)
	}
}

func BenchmarkOpenSplitBundleZstd(b *testing.B) {
	input := testPackInput(1, 1000)
	llmCompressed, llmManifest := benchmarkEncodedBundle(b, "workspace", "workspace1", []string{"workspace1"}, "gen-1", llmOnlyInput(input))
	mcpCompressed, mcpManifest := benchmarkEncodedBundle(b, "workspace", "workspace1", []string{"workspace1"}, "gen-1", mcpOnlyInput(input))
	options := SplitBundleOptions{
		GenerationID:         "gen-1",
		LLMPackChecksum:      llmManifest.Checksum,
		MCPPackChecksum:      mcpManifest.Checksum,
		RequiredLLMProviders: []string{"openai"},
		RequiredLLMModels:    []string{"gpt-4o-mini"},
		RequiredMCPServers:   []string{"github"},
	}

	b.ReportAllocs()
	var opened OpenedSplitBundle
	var err error
	for b.Loop() {
		opened, err = OpenSplitBundleZstdWithOptions(llmCompressed, mcpCompressed, options)
	}
	if err != nil {
		b.Fatalf("OpenSplitBundleZstdWithOptions() error = %v", err)
	}
	if _, ok := opened.View.ResolveLLMIDs("workspace1", "slug:1:777", "gpt-4o-mini"); !ok {
		b.Fatal("opened split view cannot resolve LLM")
	}
	if _, ok := opened.View.ResolveMCPToolIDs("workspace1", "profile-dev-tools", "github__list-repos"); !ok {
		b.Fatal("opened split view cannot resolve MCP")
	}
}

func benchmarkSplitView(b *testing.B, llmInput Input, mcpInput Input) SplitView {
	b.Helper()
	llmBlob, err := Build(llmInput)
	if err != nil {
		b.Fatalf("Build(llm) error = %v", err)
	}
	llmReader, err := Open(llmBlob)
	if err != nil {
		b.Fatalf("Open(llm) error = %v", err)
	}
	mcpBlob, err := Build(mcpInput)
	if err != nil {
		b.Fatalf("Build(mcp) error = %v", err)
	}
	mcpReader, err := Open(mcpBlob)
	if err != nil {
		b.Fatalf("Open(mcp) error = %v", err)
	}
	return NewSplitView(llmReader, mcpReader)
}

func benchmarkEncodedBundle(
	b *testing.B,
	scopeKind string,
	scopeID string,
	scopes []string,
	generationID string,
	input Input,
) ([]byte, Manifest) {
	b.Helper()
	blob, manifest, err := BuildWithManifest(input)
	if err != nil {
		b.Fatalf("BuildWithManifest() error = %v", err)
	}
	bundle := NewBundle(scopeKind, scopeID, scopes, blob, manifest)
	bundle.Metadata.GenerationID = generationID
	compressed, err := EncodeBundleZstd(bundle)
	if err != nil {
		b.Fatalf("EncodeBundleZstd() error = %v", err)
	}
	return compressed, manifest
}
