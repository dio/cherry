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
