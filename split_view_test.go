package cherry

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSplitViewRoutesLLMAndMCPToSeparateReaders(t *testing.T) {
	input := testPackInput(1, 1)
	view := testSplitView(t, llmOnlyInput(input), mcpOnlyInput(input))

	llm, ok := view.ResolveLLM("workspace1", "slug:1:1", "gpt-4o-mini")
	require.True(t, ok)
	assert.Equal(t, "openai", llm.Provider)
	assert.Equal(t, "gpt-4o-mini", llm.Model)

	tool, ok := view.ResolveMCPToolIDs("workspace1", "profile-dev-tools", "github__list-repos")
	require.True(t, ok)
	assert.Equal(t, "github", view.MCPString(tool.ServerSID))
	assert.Equal(t, "list-repos", view.MCPString(tool.ToolSID))

	_, ok = view.ResolveMCPToolIDs("workspace1", "profile-dev-tools", "missing")
	assert.False(t, ok)
	_, ok = view.ResolveLLM("workspace1", "slug:missing", "gpt-4o-mini")
	assert.False(t, ok)
}

func TestSplitViewKeepsLLMAndMCPStringTablesSeparate(t *testing.T) {
	input := testPackInput(1, 1)
	view := testSplitView(t, llmOnlyInput(input), mcpOnlyInput(input))

	llm, ok := view.ResolveLLMIDs("workspace1", "slug:1:1", "gpt-4o-mini")
	require.True(t, ok)
	assert.Equal(t, "openai", view.LLMString(llm.ProviderSID))

	tool, ok := view.ResolveMCPToolIDs("workspace1", "profile-dev-tools", "github__list-repos")
	require.True(t, ok)
	assert.Equal(t, "github", view.MCPString(tool.ServerSID))
	assert.NotEqual(t, "github", view.LLMString(tool.ServerSID))
	assert.NotEqual(t, "openai", view.MCPString(llm.ProviderSID))
}

func TestSplitViewGenerationSwapKeepsOldViewReadable(t *testing.T) {
	input := testPackInput(1, 1)
	llmInput := llmOnlyInput(input)
	mcpInputV1 := mcpOnlyInput(input)
	mcpInputV2 := mcpOnlyInput(input)
	mcpInputV2.MCPServers[0].Endpoint = "https://api.github.example/v2"
	mcpInputV2.Scopes[0].MCPProfiles[0].Tools[0].SecretRef = "env://GITHUB_PROFILE_TOKEN_V2"

	oldView := testSplitView(t, llmInput, mcpInputV1)
	newView := testSplitView(t, llmInput, mcpInputV2)

	oldTool, ok := oldView.ResolveMCPToolIDs("workspace1", "profile-dev-tools", "github__list-repos")
	require.True(t, ok)
	assert.Equal(t, "https://api.github.com", oldView.MCPString(oldTool.ServerEndpointSID))
	assert.Equal(t, "env://GITHUB_PROFILE_TOKEN", oldView.MCPString(oldTool.SecretSID))

	newTool, ok := newView.ResolveMCPToolIDs("workspace1", "profile-dev-tools", "github__list-repos")
	require.True(t, ok)
	assert.Equal(t, "https://api.github.example/v2", newView.MCPString(newTool.ServerEndpointSID))
	assert.Equal(t, "env://GITHUB_PROFILE_TOKEN_V2", newView.MCPString(newTool.SecretSID))

	oldLLM, ok := oldView.ResolveLLM("workspace1", "slug:1:1", "gpt-4o-mini")
	require.True(t, ok)
	newLLM, ok := newView.ResolveLLM("workspace1", "slug:1:1", "gpt-4o-mini")
	require.True(t, ok)
	assert.Equal(t, oldLLM, newLLM)
}

func TestOpenSplitBundleZstdOpensPairedBundles(t *testing.T) {
	input := testPackInput(1, 1)
	llmCompressed, llmManifest := testEncodedBundle(t, "workspace", "workspace1", []string{"workspace1"}, "gen-1", llmOnlyInput(input))
	mcpCompressed, mcpManifest := testEncodedBundle(t, "workspace", "workspace1", []string{"workspace1"}, "gen-1", mcpOnlyInput(input))

	opened, err := OpenSplitBundleZstdWithOptions(llmCompressed, mcpCompressed, SplitBundleOptions{
		GenerationID:         "gen-1",
		LLMPackChecksum:      llmManifest.Checksum,
		MCPPackChecksum:      mcpManifest.Checksum,
		RequiredLLMProviders: []string{"openai"},
		RequiredLLMModels:    []string{"gpt-4o-mini"},
		RequiredMCPServers:   []string{"github"},
	})
	require.NoError(t, err)
	assert.Equal(t, llmManifest, opened.LLM.Metadata.PackManifest)
	assert.Equal(t, mcpManifest, opened.MCP.Metadata.PackManifest)
	assert.NotEqual(t, opened.LLM.Metadata.PackManifest.Checksum, opened.MCP.Metadata.PackManifest.Checksum)

	llm, ok := opened.View.ResolveLLM("workspace1", "slug:1:1", "gpt-4o-mini")
	require.True(t, ok)
	assert.Equal(t, "openai", llm.Provider)

	tool, ok := opened.View.ResolveMCPToolIDs("workspace1", "profile-dev-tools", "github__list-repos")
	require.True(t, ok)
	assert.Equal(t, "github", opened.View.MCPString(tool.ServerSID))
}

func TestOpenSplitBundleZstdRejectsIncompatibleBundles(t *testing.T) {
	input := testPackInput(1, 1)
	llmCompressed, llmManifest := testEncodedBundle(t, "workspace", "workspace1", []string{"workspace1"}, "gen-1", llmOnlyInput(input))

	t.Run("scope id mismatch", func(t *testing.T) {
		mcpCompressed, _ := testEncodedBundle(t, "workspace", "workspace2", []string{"workspace1"}, "gen-1", mcpOnlyInput(input))
		_, err := OpenSplitBundleZstd(llmCompressed, mcpCompressed)
		require.ErrorContains(t, err, "scope id mismatch")
	})

	t.Run("scopes mismatch", func(t *testing.T) {
		mcpCompressed, _ := testEncodedBundle(t, "workspace", "workspace1", []string{"workspace2"}, "gen-1", mcpOnlyInput(input))
		_, err := OpenSplitBundleZstd(llmCompressed, mcpCompressed)
		require.ErrorContains(t, err, "scopes mismatch")
	})

	t.Run("generation mismatch", func(t *testing.T) {
		mcpCompressed, _ := testEncodedBundle(t, "workspace", "workspace1", []string{"workspace1"}, "gen-2", mcpOnlyInput(input))
		_, err := OpenSplitBundleZstd(llmCompressed, mcpCompressed)
		require.ErrorContains(t, err, "generation mismatch")
	})

	t.Run("expected checksum mismatch", func(t *testing.T) {
		mcpCompressed, _ := testEncodedBundle(t, "workspace", "workspace1", []string{"workspace1"}, "gen-1", mcpOnlyInput(input))
		_, err := OpenSplitBundleZstdWithOptions(llmCompressed, mcpCompressed, SplitBundleOptions{
			GenerationID:    "gen-1",
			LLMPackChecksum: llmManifest.Checksum + 1,
		})
		require.ErrorContains(t, err, "llm checksum mismatch")
	})

	t.Run("catalog expectation mismatch", func(t *testing.T) {
		mcpCompressed, _ := testEncodedBundle(t, "workspace", "workspace1", []string{"workspace1"}, "gen-1", mcpOnlyInput(input))
		_, err := OpenSplitBundleZstdWithOptions(llmCompressed, mcpCompressed, SplitBundleOptions{
			GenerationID:         "gen-1",
			RequiredLLMProviders: []string{"missing"},
		})
		require.ErrorContains(t, err, "missing llm provider")
	})
}

func testSplitView(t *testing.T, llmInput Input, mcpInput Input) SplitView {
	t.Helper()
	llmBlob, err := Build(llmInput)
	require.NoError(t, err)
	llmReader, err := Open(llmBlob)
	require.NoError(t, err)
	mcpBlob, err := Build(mcpInput)
	require.NoError(t, err)
	mcpReader, err := Open(mcpBlob)
	require.NoError(t, err)
	return NewSplitView(llmReader, mcpReader)
}

func testEncodedBundle(
	t *testing.T,
	scopeKind string,
	scopeID string,
	scopes []string,
	generationID string,
	input Input,
) ([]byte, Manifest) {
	t.Helper()
	blob, manifest, err := BuildWithManifest(input)
	require.NoError(t, err)
	bundle := NewBundle(scopeKind, scopeID, scopes, blob, manifest)
	bundle.Metadata.GenerationID = generationID
	compressed, err := EncodeBundleZstd(bundle)
	require.NoError(t, err)
	return compressed, manifest
}

func llmOnlyInput(input Input) Input {
	out := Input{
		Providers: append([]Provider(nil), input.Providers...),
		Models:    append([]Model(nil), input.Models...),
	}
	out.Scopes = make([]Scope, 0, len(input.Scopes))
	for _, scope := range input.Scopes {
		outScope := Scope{
			ID:         scope.ID,
			Principals: make([]Principal, 0, len(scope.Principals)),
		}
		for _, principal := range scope.Principals {
			outPrincipal := principal
			if principal.ModelRoutes != nil {
				outPrincipal.ModelRoutes = make(map[string]RoutePlan, len(principal.ModelRoutes))
				for modelID, route := range principal.ModelRoutes {
					outPrincipal.ModelRoutes[modelID] = route
				}
			}
			outScope.Principals = append(outScope.Principals, outPrincipal)
		}
		out.Scopes = append(out.Scopes, outScope)
	}
	return out
}

func mcpOnlyInput(input Input) Input {
	out := Input{
		MCPServers: append([]MCPServer(nil), input.MCPServers...),
	}
	out.Scopes = make([]Scope, 0, len(input.Scopes))
	for _, scope := range input.Scopes {
		outScope := Scope{
			ID:          scope.ID,
			MCPProfiles: make([]MCPProfile, 0, len(scope.MCPProfiles)),
		}
		for _, profile := range scope.MCPProfiles {
			outProfile := profile
			outProfile.Tools = append([]MCPToolBinding(nil), profile.Tools...)
			outScope.MCPProfiles = append(outScope.MCPProfiles, outProfile)
		}
		out.Scopes = append(out.Scopes, outScope)
	}
	return out
}
