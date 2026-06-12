package transform

import (
	"testing"

	cherry "github.com/dio/cherry"
	"github.com/dio/cherry/example/source"
)

func TestBuildPackInputProjectFanout(t *testing.T) {
	result, err := BuildPackInput(source.ExampleFixture(), Selection{
		Kind: ScopeKindProject,
		ID:   "project1",
	})
	if err != nil {
		t.Fatalf("BuildPackInput() error = %v", err)
	}
	if got, want := result.Scopes, []string{"workspace1", "workspace2"}; !equalStrings(got, want) {
		t.Fatalf("scopes = %v, want %v", got, want)
	}

	reader := openPack(t, result.Input)
	if _, ok := reader.ResolveLLMIDs("workspace1", "slug:project1", "claude-haiku-4-5"); !ok {
		t.Fatal("project1 key missing from workspace1")
	}
	if _, ok := reader.ResolveLLMIDs("workspace2", "slug:project1", "claude-haiku-4-5"); !ok {
		t.Fatal("project1 key missing from workspace2")
	}
	if _, ok := reader.ResolveLLMIDs("workspace3", "slug:project1", "claude-haiku-4-5"); ok {
		t.Fatal("project1 key leaked into workspace3")
	}
	if _, ok := reader.ResolveLLMIDs("workspace2", "slug:workspace1", "claude-haiku-4-5"); ok {
		t.Fatal("workspace1 key leaked into workspace2")
	}
}

func TestBuildPackInputRulesAndMCPAuth(t *testing.T) {
	result, err := BuildPackInput(source.ExampleFixture(), Selection{
		Kind: ScopeKindWorkspace,
		ID:   "workspace1",
	})
	if err != nil {
		t.Fatalf("BuildPackInput() error = %v", err)
	}
	reader := openPack(t, result.Input)

	llm, ok := reader.ResolveLLMIDs("workspace1", "slug:project1", "claude-haiku-4-5")
	if !ok {
		t.Fatal("ResolveLLMIDs() ok = false")
	}
	if got := reader.String(llm.ProviderSID); got != "openai" {
		t.Fatalf("provider = %q, want openai", got)
	}
	if got := reader.String(llm.SecretSID); got != "env://MARKETING_OPENAI_KEY" {
		t.Fatalf("secret = %q, want marketing key", got)
	}
	if llm.Rate.RPM != 60 {
		t.Fatalf("rpm = %d, want 60", llm.Rate.RPM)
	}

	tool, ok := reader.ResolveMCPToolIDs("workspace1", "profile-dev-tools", "github__list-repos")
	if !ok {
		t.Fatal("ResolveMCPToolIDs() ok = false")
	}
	if got := reader.String(tool.AuthTypeSID); got != "bearer" {
		t.Fatalf("auth type = %q, want bearer", got)
	}
	if got := reader.String(tool.SecretSID); got != "env://GITHUB_TOKEN" {
		t.Fatalf("secret = %q, want env://GITHUB_TOKEN", got)
	}

	init, ok := reader.ResolveMCPInitialize("workspace1", "profile-kiwi-and-github")
	if !ok {
		t.Fatal("ResolveMCPInitialize() ok = false")
	}
	if len(init.Servers) != 2 {
		t.Fatalf("initialize servers = %d, want 2", len(init.Servers))
	}

	directTool, ok := reader.ResolveMCPToolIDs("workspace1", "s/github", "github__list-repos")
	if !ok {
		t.Fatal("direct server ResolveMCPToolIDs() ok = false")
	}
	if got := reader.String(directTool.SecretSID); got != "env://GITHUB_MCP_TOKEN" {
		t.Fatalf("direct server secret = %q, want env://GITHUB_MCP_TOKEN", got)
	}
}

func TestBuildPackInputAdvancedRoutingExamples(t *testing.T) {
	fixture := source.ExampleFixture()
	project1, err := BuildPackInput(fixture, Selection{
		Kind: ScopeKindProject,
		ID:   "project1",
	})
	if err != nil {
		t.Fatalf("BuildPackInput(project1) error = %v", err)
	}
	reader := openPack(t, project1.Input)

	byokAlways, ok := reader.ResolveLLMIDs("workspace1", "slug:workspace1-byok-always", "claude-sonnet-4-5")
	if !ok {
		t.Fatal("BYOK always ResolveLLMIDs() ok = false")
	}
	if got := reader.String(byokAlways.SecretSID); got != "env://WORKSPACE1_BYOK_ANTHROPIC_KEY" {
		t.Fatalf("BYOK always secret = %q, want workspace BYOK secret", got)
	}
	if got := reader.String(byokAlways.ProviderSID); got != "anthropic" {
		t.Fatalf("BYOK always provider = %q, want anthropic", got)
	}

	byokPreferred, ok := reader.ResolveLLMIDs("workspace1", "slug:workspace1-byok-preferred", "gpt-4o-mini-2")
	if !ok {
		t.Fatal("BYOK preferred ResolveLLMIDs() ok = false")
	}
	if got := reader.String(byokPreferred.SecretSID); got != "env://ANTHROPIC_API_KEY" {
		t.Fatalf("BYOK preferred primary secret = %q, want user key", got)
	}
	if got := reader.String(byokPreferred.ProviderSID); got != "openai" {
		t.Fatalf("BYOK preferred primary provider = %q, want openai", got)
	}
	byokPreferredPlan, ok := reader.ResolveLLMPlan("workspace1", "slug:workspace1-byok-preferred", "gpt-4o-mini-2")
	if !ok {
		t.Fatal("BYOK preferred ResolveLLMPlan() ok = false")
	}
	if byokPreferredPlan.Plan.Kind != cherry.RouteKindChain || len(byokPreferredPlan.Plan.Children) != 2 {
		t.Fatalf("BYOK preferred plan = %#v, want two-child chain", byokPreferredPlan.Plan)
	}

	split, ok := reader.ResolveLLMIDs("workspace2", "slug:workspace2", "claude-haiku-4-5")
	if !ok {
		t.Fatal("split ResolveLLMIDs() ok = false")
	}
	if got := reader.String(split.ProviderSID); got != "openai" {
		t.Fatalf("split materialized provider = %q, want highest-weight openai", got)
	}
	if got := reader.String(split.ModelSID); got != "gpt-4o-mini" {
		t.Fatalf("split materialized model = %q, want gpt-4o-mini", got)
	}
	splitPlan, ok := reader.ResolveLLMPlan("workspace2", "slug:workspace2", "claude-haiku-4-5")
	if !ok {
		t.Fatal("split ResolveLLMPlan() ok = false")
	}
	if splitPlan.Plan.Kind != cherry.RouteKindSplit || len(splitPlan.Plan.Children) != 2 || splitPlan.Plan.Children[0].Weight == 0 {
		t.Fatalf("split plan = %#v, want weighted split", splitPlan.Plan)
	}

	project2, err := BuildPackInput(fixture, Selection{
		Kind: ScopeKindProject,
		ID:   "project2",
	})
	if err != nil {
		t.Fatalf("BuildPackInput(project2) error = %v", err)
	}
	project2Reader := openPack(t, project2.Input)
	autofallback, ok := project2Reader.ResolveLLMIDs("workspace3", "slug:project2", "claude-sonnet-4-5")
	if !ok {
		t.Fatal("autofallback chain ResolveLLMIDs() ok = false")
	}
	if got := project2Reader.String(autofallback.ProviderSID); got != "fallback_p1" {
		t.Fatalf("autofallback primary provider = %q, want fallback_p1", got)
	}
	if got := project2Reader.String(autofallback.ModelSID); got != "claude-haiku-4-5" {
		t.Fatalf("autofallback primary model = %q, want claude-haiku-4-5", got)
	}
	if got := project2Reader.String(autofallback.SecretSID); got != "literal://fake" {
		t.Fatalf("autofallback primary secret = %q, want fallback provider default", got)
	}
	autofallbackPlan, ok := project2Reader.ResolveLLMPlan("workspace3", "slug:project2", "claude-sonnet-4-5")
	if !ok {
		t.Fatal("autofallback chain ResolveLLMPlan() ok = false")
	}
	if autofallbackPlan.Plan.Kind != cherry.RouteKindChain || len(autofallbackPlan.Plan.Children) != 4 {
		t.Fatalf("autofallback plan = %#v, want four-child chain", autofallbackPlan.Plan)
	}
	last := autofallbackPlan.Plan.Children[3].Plan
	if last.Provider != "vertex_anthropic" || last.Model != "claude-opus-4@20250514" {
		t.Fatalf("autofallback final child = %#v, want vertex anthropic opus", last)
	}
}

func openPack(t *testing.T, input cherry.Input) cherry.Reader {
	t.Helper()
	blob, manifest, err := cherry.BuildWithManifest(input)
	if err != nil {
		t.Fatalf("BuildWithManifest() error = %v", err)
	}
	reader, err := cherry.OpenWithManifest(blob, manifest)
	if err != nil {
		t.Fatalf("OpenWithManifest() error = %v", err)
	}
	return reader
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
