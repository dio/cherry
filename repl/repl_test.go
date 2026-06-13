package repl

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	cherry "github.com/dio/cherry"
)

func TestSessionExecuteAbsorbsLaneAndScope(t *testing.T) {
	opened := openTestBundle(t)
	session, err := NewSession(Config{
		Backend:      NewLocalBackend(opened),
		DefaultScope: "prod",
		Context: Context{
			Lane:             "default",
			SnapshotVersion:  7,
			SnapshotChecksum: "abc123",
			Source:           "test",
		},
	})
	require.NoError(t, err)

	result, err := session.Execute(context.Background(), "summary")
	require.NoError(t, err)
	require.True(t, result.Continue)
	require.Equal(t, "prod", result.Scope)
	require.Equal(t, "default", result.Lane)
	require.Contains(t, result.Text, "lane: default")
	require.Contains(t, result.Text, "active_scope: prod")

	result, err = session.Execute(context.Background(), "llm slug:alice gpt-4o-mini")
	require.NoError(t, err)
	require.Contains(t, result.Text, "lane: default")
	require.Contains(t, result.Text, "scope: prod")
	require.Contains(t, result.Text, "principal: slug:alice")
	require.Contains(t, result.Text, "provider=openai")
}

func TestSessionExecuteUseScope(t *testing.T) {
	opened := openTestBundle(t)
	session, err := NewSession(Config{
		Backend: NewLocalBackend(opened),
		Context: Context{Lane: "default"},
	})
	require.NoError(t, err)

	result, err := session.Execute(context.Background(), "llm slug:alice gpt-4o-mini")
	require.NoError(t, err)
	require.Contains(t, result.Text, "no active scope")

	result, err = session.Execute(context.Background(), "use prod")
	require.NoError(t, err)
	require.Equal(t, "prod", result.Scope)
	require.Contains(t, result.Text, "using scope prod")

	result, err = session.Execute(context.Background(), "mcp call github github__list_repos")
	require.NoError(t, err)
	require.Contains(t, result.Text, "lane: default")
	require.Contains(t, result.Text, "scope: prod")
	require.Contains(t, result.Text, "server=github")
}

func openTestBundle(t *testing.T) cherry.OpenedBundle {
	t.Helper()

	input := cherry.Input{
		Providers: []cherry.Provider{
			{
				ID:        "openai",
				Kind:      "openai",
				Endpoint:  "https://api.openai.example",
				SecretRef: "env://OPENAI_API_KEY",
				AuthType:  "bearer",
			},
		},
		Models: []cherry.Model{
			{
				ID:       "gpt-4o-mini",
				Provider: "openai",
				Name:     "gpt-4o-mini",
				Mode:     "chat",
			},
		},
		MCPServers: []cherry.MCPServer{
			{
				ID:        "github",
				Endpoint:  "https://mcp.github.example",
				SecretRef: "sm://github-token",
				AuthType:  "bearer",
			},
		},
		Scopes: []cherry.Scope{
			{
				ID: "prod",
				Principals: []cherry.Principal{
					{
						Slug: "slug:alice",
						ModelRoutes: map[string]cherry.RoutePlan{
							"gpt-4o-mini": {
								Kind:     cherry.RouteKindTarget,
								Provider: "openai",
								Model:    "gpt-4o-mini",
							},
						},
					},
				},
				MCPProfiles: []cherry.MCPProfile{
					{
						Path: "github",
						Tools: []cherry.MCPToolBinding{
							{
								ExposedName: "github__list_repos",
								Server:      "github",
								Tool:        "list_repos",
								SecretRef:   "sm://github-token",
								AuthType:    "bearer",
							},
						},
					},
				},
			},
		},
	}

	blob, manifest, err := cherry.BuildWithManifest(input)
	require.NoError(t, err)
	bundle := cherry.NewBundle("lane", "default", []string{"prod"}, blob, manifest)
	payload, err := cherry.EncodeBundleZstd(bundle)
	require.NoError(t, err)
	opened, err := cherry.OpenBundleZstd(payload)
	require.NoError(t, err)
	require.True(t, strings.Contains(opened.Metadata.ScopeID, "default"))
	return opened
}
