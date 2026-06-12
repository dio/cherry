package source

import "testing"

func TestLoadModelCatalogJSON(t *testing.T) {
	models, err := LoadModelCatalogJSON("testdata/catalogs/models.json")
	if err != nil {
		t.Fatalf("LoadModelCatalogJSON() error = %v", err)
	}
	if len(models) == 0 {
		t.Fatal("LoadModelCatalogJSON() returned no enabled models")
	}
	var gpt5 Model
	for _, model := range models {
		if model.ID == "gpt-5" {
			gpt5 = model
			break
		}
	}
	if gpt5.ID == "" {
		t.Fatal("enabled model gpt-5 not found")
	}
	if gpt5.Provider != "openai" || gpt5.MetadataJSON == "" {
		t.Fatalf("gpt-5 = %#v, want provider and metadata", gpt5)
	}
	if !contains(gpt5.Capabilities, "image_generation") {
		t.Fatalf("gpt-5 capabilities = %#v, want image_generation", gpt5.Capabilities)
	}
}

func TestLoadProviderCatalogJSON(t *testing.T) {
	providers, err := LoadProviderCatalogJSON("testdata/catalogs/providers.json")
	if err != nil {
		t.Fatalf("LoadProviderCatalogJSON() error = %v", err)
	}
	if len(providers) == 0 {
		t.Fatal("LoadProviderCatalogJSON() returned no providers")
	}
	var openai Provider
	for _, provider := range providers {
		if provider.ID == "openai" {
			openai = provider
			break
		}
	}
	if openai.Endpoint == "" {
		t.Fatalf("openai provider = %#v, want endpoint", openai)
	}
}

func TestLoadMCPCatalogJSON(t *testing.T) {
	servers, err := LoadMCPCatalogJSON("testdata/catalogs/mcp-catalog-data-with-tools.json")
	if err != nil {
		t.Fatalf("LoadMCPCatalogJSON() error = %v", err)
	}
	var aws MCPServer
	for _, server := range servers {
		if server.ID == "aws-knowledge" {
			aws = server
			break
		}
	}
	if aws.Endpoint == "" || aws.AuthType != "none" {
		t.Fatalf("aws-knowledge = %#v, want open endpoint", aws)
	}
	if !contains(aws.Tools, "aws___list_regions") {
		t.Fatalf("aws-knowledge tools missing aws___list_regions: %#v", aws.Tools)
	}
}

func TestLoadMCPCatalogJSONWithoutToolsShape(t *testing.T) {
	servers, err := LoadMCPCatalogJSON("testdata/catalogs/mcp-catalog-data.json")
	if err != nil {
		t.Fatalf("LoadMCPCatalogJSON() error = %v", err)
	}
	if len(servers) == 0 {
		t.Fatal("LoadMCPCatalogJSON() returned no servers")
	}
}

func TestMergeProvidersPreservesSecretRefs(t *testing.T) {
	got := MergeProviders(
		[]Provider{{ID: "openai", Kind: "openai", SecretRef: "env://OPENAI_API_KEY"}},
		[]Provider{{ID: "openai", Kind: "openai", Endpoint: "https://api.openai.com"}},
	)
	if len(got) != 1 || got[0].SecretRef != "env://OPENAI_API_KEY" || got[0].Endpoint == "" {
		t.Fatalf("MergeProviders() = %#v, want endpoint with preserved secret ref", got)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
