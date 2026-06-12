package transform

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	cherry "github.com/dio/cherry"
	"github.com/dio/cherry/example/source"
)

func TestBuildPackInputProjectFanout(t *testing.T) {
	result, err := BuildPackInput(source.ExampleFixture(), Selection{
		Kind: ScopeKindProject,
		ID:   "project1",
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"workspace1", "workspace2"}, result.Scopes)

	reader := openPack(t, result.Input)
	_, ok := reader.ResolveLLMIDs("workspace1", "slug:project1", "claude-haiku-4-5")
	require.True(t, ok)
	_, ok = reader.ResolveLLMIDs("workspace2", "slug:project1", "claude-haiku-4-5")
	require.True(t, ok)
	_, ok = reader.ResolveLLMIDs("workspace3", "slug:project1", "claude-haiku-4-5")
	require.False(t, ok)
	_, ok = reader.ResolveLLMIDs("workspace2", "slug:workspace1", "claude-haiku-4-5")
	require.False(t, ok)
}

func TestBuildPackInputRulesAndMCPAuth(t *testing.T) {
	result, err := BuildPackInput(source.ExampleFixture(), Selection{
		Kind: ScopeKindWorkspace,
		ID:   "workspace1",
	})
	require.NoError(t, err)
	reader := openPack(t, result.Input)

	llm, ok := reader.ResolveLLMIDs("workspace1", "slug:project1", "claude-haiku-4-5")
	require.True(t, ok)
	assert.Equal(t, "openai", reader.String(llm.ProviderSID))
	assert.Equal(t, "env://MARKETING_OPENAI_KEY", reader.String(llm.SecretSID))
	assert.Equal(t, uint32(60), llm.Rate.RPM)

	tool, ok := reader.ResolveMCPToolIDs("workspace1", "profile-dev-tools", "github__list-repos")
	require.True(t, ok)
	assert.Equal(t, "bearer", reader.String(tool.AuthTypeSID))
	assert.Equal(t, "env://GITHUB_TOKEN", reader.String(tool.SecretSID))

	init, ok := reader.ResolveMCPInitialize("workspace1", "profile-kiwi-and-github")
	require.True(t, ok)
	require.Len(t, init.Servers, 2)

	directTool, ok := reader.ResolveMCPToolIDs("workspace1", "s/github", "github__list-repos")
	require.True(t, ok)
	assert.Equal(t, "env://GITHUB_MCP_TOKEN", reader.String(directTool.SecretSID))
}

func TestBuildPackInputAdvancedRoutingExamples(t *testing.T) {
	fixture := source.ExampleFixture()
	project1, err := BuildPackInput(fixture, Selection{
		Kind: ScopeKindProject,
		ID:   "project1",
	})
	require.NoError(t, err)
	reader := openPack(t, project1.Input)

	byokAlways, ok := reader.ResolveLLMIDs("workspace1", "slug:workspace1-byok-always", "claude-sonnet-4-5")
	require.True(t, ok)
	assert.Equal(t, "env://WORKSPACE1_BYOK_ANTHROPIC_KEY", reader.String(byokAlways.SecretSID))
	assert.Equal(t, "anthropic", reader.String(byokAlways.ProviderSID))

	byokPreferred, ok := reader.ResolveLLMIDs("workspace1", "slug:workspace1-byok-preferred", "gpt-4o-mini-2")
	require.True(t, ok)
	assert.Equal(t, "env://ANTHROPIC_API_KEY", reader.String(byokPreferred.SecretSID))
	assert.Equal(t, "openai", reader.String(byokPreferred.ProviderSID))
	byokPreferredPlan, ok := reader.ResolveLLMPlan("workspace1", "slug:workspace1-byok-preferred", "gpt-4o-mini-2")
	require.True(t, ok)
	require.Equal(t, cherry.RouteKindChain, byokPreferredPlan.Plan.Kind)
	require.Len(t, byokPreferredPlan.Plan.Children, 2)

	split, ok := reader.ResolveLLMIDs("workspace2", "slug:workspace2", "claude-haiku-4-5")
	require.True(t, ok)
	assert.Equal(t, "openai", reader.String(split.ProviderSID))
	assert.Equal(t, "gpt-4o-mini", reader.String(split.ModelSID))
	splitPlan, ok := reader.ResolveLLMPlan("workspace2", "slug:workspace2", "claude-haiku-4-5")
	require.True(t, ok)
	require.Equal(t, cherry.RouteKindSplit, splitPlan.Plan.Kind)
	require.Len(t, splitPlan.Plan.Children, 2)
	require.NotZero(t, splitPlan.Plan.Children[0].Weight)

	project2, err := BuildPackInput(fixture, Selection{
		Kind: ScopeKindProject,
		ID:   "project2",
	})
	require.NoError(t, err)
	project2Reader := openPack(t, project2.Input)
	autofallback, ok := project2Reader.ResolveLLMIDs("workspace3", "slug:project2", "claude-sonnet-4-5")
	require.True(t, ok)
	assert.Equal(t, "fallback_p1", project2Reader.String(autofallback.ProviderSID))
	assert.Equal(t, "claude-haiku-4-5", project2Reader.String(autofallback.ModelSID))
	assert.Equal(t, "literal://fake", project2Reader.String(autofallback.SecretSID))
	autofallbackPlan, ok := project2Reader.ResolveLLMPlan("workspace3", "slug:project2", "claude-sonnet-4-5")
	require.True(t, ok)
	require.Equal(t, cherry.RouteKindChain, autofallbackPlan.Plan.Kind)
	require.Len(t, autofallbackPlan.Plan.Children, 4)
	last := autofallbackPlan.Plan.Children[3].Plan
	assert.Equal(t, "vertex_anthropic", last.Provider)
	assert.Equal(t, "claude-opus-4@20250514", last.Model)
}

func openPack(t *testing.T, input cherry.Input) cherry.Reader {
	t.Helper()
	blob, manifest, err := cherry.BuildWithManifest(input)
	require.NoError(t, err)
	reader, err := cherry.OpenWithManifest(blob, manifest)
	require.NoError(t, err)
	return reader
}
