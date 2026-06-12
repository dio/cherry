package source

import (
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestLoadModelCatalogJSON(t *testing.T) {
	models, err := LoadModelCatalogJSON("testdata/catalogs/models.json")
	require.NoError(t, err)
	require.NotEmpty(t, models)
	var gpt5 Model
	for _, model := range models {
		if model.ID == "gpt-5" {
			gpt5 = model
			break
		}
	}
	require.NotEmpty(t, gpt5.ID)
	require.Equal(t, "openai", gpt5.Provider)
	require.NotEmpty(t, gpt5.MetadataJSON)
	require.Contains(t, gpt5.Capabilities, "image_generation")
}

func TestLoadProviderCatalogJSON(t *testing.T) {
	providers, err := LoadProviderCatalogJSON("testdata/catalogs/providers.json")
	require.NoError(t, err)
	require.NotEmpty(t, providers)
	var openai Provider
	for _, provider := range providers {
		if provider.ID == "openai" {
			openai = provider
			break
		}
	}
	require.NotEmpty(t, openai.Endpoint)
}

func TestLoadMCPCatalogJSON(t *testing.T) {
	servers, err := LoadMCPCatalogJSON("testdata/catalogs/mcp-catalog-data-with-tools.json")
	require.NoError(t, err)
	var aws MCPServer
	for _, server := range servers {
		if server.ID == "aws-knowledge" {
			aws = server
			break
		}
	}
	require.NotEmpty(t, aws.Endpoint)
	require.Equal(t, "none", aws.AuthType)
	require.Contains(t, aws.Tools, "aws___list_regions")
}

func TestLoadMCPCatalogJSONWithoutToolsShape(t *testing.T) {
	servers, err := LoadMCPCatalogJSON("testdata/catalogs/mcp-catalog-data.json")
	require.NoError(t, err)
	require.NotEmpty(t, servers)
}

func TestMergeProvidersPreservesSecretRefs(t *testing.T) {
	got := MergeProviders(
		[]Provider{{ID: "openai", Kind: "openai", SecretRef: "env://OPENAI_API_KEY"}},
		[]Provider{{ID: "openai", Kind: "openai", Endpoint: "https://api.openai.com"}},
	)
	require.Len(t, got, 1)
	require.Equal(t, "env://OPENAI_API_KEY", got[0].SecretRef)
	require.NotEmpty(t, got[0].Endpoint)
}

func TestRouteNodeUnmarshalNestedTree(t *testing.T) {
	const payload = `
chain:
  retry:
    retry_on: "401,5xx"
    per_try_timeout_ms: 10000
  children:
    - target:
        provider: openai
        model: gpt-4o-mini
    - weight: 25
      split:
        children:
          - weight: 80
            target:
              provider: fallback
              model: gpt-fallback
          - weight: 20
            chain:
              retry:
                retry_on: "connect-failure"
                per_try_timeout_ms: 2000
              children:
                - target:
                    provider: fallback
                    model: gpt-fallback
`
	var node RouteNode
	require.NoError(t, yaml.Unmarshal([]byte(payload), &node))
	require.Equal(t, "chain", node.Kind)
	require.Len(t, node.Chain, 2)
	require.Equal(t, "target", node.Chain[0].Kind)
	require.Equal(t, "split", node.Chain[1].Kind)
	require.Len(t, node.Chain[1].Split, 2)
	require.Equal(t, 80, node.Chain[1].Split[0].Weight)
	require.Equal(t, "target", node.Chain[1].Split[0].Node.Kind)
	require.Equal(t, "chain", node.Chain[1].Split[1].Node.Kind)
}
