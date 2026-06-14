# Cherry Integration Reference

## Boundary

Cherry receives normalized rows and emits/reads compact bundles. It should not
know how to answer product questions such as "which workspaces can this project
key reach?" The external transformer answers that and places resulting principals in
the selected `cherry.Scope` records.

## Producer: Minimal Input

```go
input := cherry.Input{
    Providers: []cherry.Provider{
        {ID: "openai", Kind: "openai", Endpoint: "https://api.openai.com", SecretRef: "env://OPENAI_API_KEY"},
    },
    Models: []cherry.Model{
        {
            ID:           "gpt-4o-mini",
            Provider:     "openai",
            Name:         "gpt-4o-mini",
            Mode:         "chat",
            Capabilities: []string{"vision", "tool_choice"},
            MetadataJSON: `{"model":"gpt-4o-mini","inputTokensPricePerMillion":"0.15","capabilities":["vision","tool_choice"]}`,
        },
    },
    MCPServers: []cherry.MCPServer{
        {ID: "github", Endpoint: "https://api.github.com", AuthType: "bearer", SecretRef: "env://GITHUB_TOKEN"},
    },
    Scopes: []cherry.Scope{
        {
            ID: "workspace1",
            Principals: []cherry.Principal{
                {
                    Slug: "slug:project1",
                    ModelRoutes: map[string]cherry.RoutePlan{
                        "gpt-4o-mini": {
                            Provider:  "openai",
                            Model:     "gpt-4o-mini",
                            SecretRef: "env://OPENAI_API_KEY",
                        },
                    },
                    Rate: cherry.RatePolicy{
                        USDPerDayCents: 5000,
                        RPM:            60,
                        OnExceed:       "reject",
                    },
                },
            },
            MCPProfiles: []cherry.MCPProfile{
                {
                    Path: "profile-dev-tools",
                    Tools: []cherry.MCPToolBinding{
                        {
                            ExposedName: "github__list-repos",
                            Server:      "github",
                            Tool:        "list-repos",
                            AuthType:    "bearer",
                            SecretRef:   "env://GITHUB_TOKEN",
                        },
                    },
                },
            },
        },
    },
}
```

## Producer: Bundle Delivery

```go
blob, manifest, err := cherry.BuildWithManifest(input)
if err != nil {
    return err
}

bundle := cherry.NewBundle(
    "workspace",
    "workspace1",
    []string{"workspace1"},
    blob,
    manifest,
)

payload, err := cherry.EncodeBundleZstd(bundle)
if err != nil {
    return err
}
```

## Producer: Split LLM/MCP Bundle Delivery

Use split delivery when LLM policy/catalog records and MCP profile/catalog
records rebuild independently. The control plane still starts from one normalized
source selection, then filters it into two `cherry.Input` values.

```go
llmInput := cherry.Input{
    Providers: input.Providers,
    Models:    input.Models,
    Scopes:    make([]cherry.Scope, 0, len(input.Scopes)),
}
mcpInput := cherry.Input{
    MCPServers: input.MCPServers,
    Scopes:     make([]cherry.Scope, 0, len(input.Scopes)),
}

for _, scope := range input.Scopes {
    llmInput.Scopes = append(llmInput.Scopes, cherry.Scope{
        ID:         scope.ID,
        Principals: scope.Principals,
    })
    mcpInput.Scopes = append(mcpInput.Scopes, cherry.Scope{
        ID:          scope.ID,
        MCPProfiles: scope.MCPProfiles,
    })
}

llmBlob, llmManifest, err := cherry.BuildWithManifest(llmInput)
if err != nil {
    return err
}
mcpBlob, mcpManifest, err := cherry.BuildWithManifest(mcpInput)
if err != nil {
    return err
}

llmBundle := cherry.NewBundle("workspace", "workspace1", []string{"workspace1"}, llmBlob, llmManifest)
mcpBundle := cherry.NewBundle("workspace", "workspace1", []string{"workspace1"}, mcpBlob, mcpManifest)
llmBundle.Metadata.GenerationID = generationID
mcpBundle.Metadata.GenerationID = generationID

llmPayload, err := cherry.EncodeBundleZstd(llmBundle)
if err != nil {
    return err
}
mcpPayload, err := cherry.EncodeBundleZstd(mcpBundle)
if err != nil {
    return err
}
```

The two pack manifests should normally differ. Do not force equal checksums or
blob sizes across LLM and MCP artifacts.

## Consumer: Loading

```go
opened, err := cherry.OpenBundleZstd(payload)
if err != nil {
    return err
}

reader := opened.Reader
```

For project bundles, choose a concrete enforcement scope from
`opened.Metadata.Scopes` before querying.

## Consumer: Split Loading

```go
opened, err := cherry.OpenSplitBundleZstdWithOptions(llmPayload, mcpPayload, cherry.SplitBundleOptions{
    GenerationID:         generationID,
    RequiredLLMProviders: []string{"openai"},
    RequiredLLMModels:    []string{"gpt-4o-mini"},
    RequiredMCPServers:   []string{"github"},
})
if err != nil {
    return err
}

view := opened.View
```

`OpenSplitBundleZstdWithOptions` validates each bundle manifest, then validates
that both artifacts describe the same scope selection. Keep the resulting
`SplitView` behind the same kind of active generation pointer used for a
combined `Reader`.

## Consumer: MCP Initialize

```go
init, ok := reader.ResolveMCPInitialize("workspace1", "profile-dev-tools")
if !ok {
    return reject()
}

for _, server := range init.Servers {
    endpoint := server.Endpoint
    authType := server.AuthType
    secretRef := server.SecretRef
}
```

Use this for MCP `initialize`. It returns every upstream server needed by the path.

## Consumer: LLM Query

```go
plan, ok := reader.ResolveLLMPlanIDs("workspace1", "slug:project1", "gpt-4o-mini")
if !ok {
    return reject()
}

// plan.Plan is target, chain, or split. Use reader.String on target node IDs
// only when provider, model, endpoint, or secret-ref strings are needed.
```

The principal slug should come from a verifier outside Cherry.

With a split view, call the same LLM method on `view` and materialize returned
string IDs with `view.LLMString`.

## Consumer: Model Catalog Query

```go
providers := reader.Providers()
model, ok := reader.ResolveModel("gpt-4o-mini")
supportsImages := reader.ModelCapability("gpt-5", "image_generation")
payload, err := reader.V1ModelsJSON()
openAIModels, err := reader.V1ModelsJSONForProvider("openai")
```

The producer should normalize external model catalogs into `cherry.Model` rows.
Keep catalog details needed by the EP, such as pricing and limits, in
`MetadataJSON`; Cherry packs that metadata and derives `/v1/models` projection
fields from it.

With a split view, provider/model catalog methods read from the LLM bundle.

## Consumer: MCP Query

```go
tool, ok := reader.ResolveMCPToolIDs("workspace1", "profile-dev-tools", "github__list-repos")
if !ok {
    return reject()
}

server := reader.String(tool.ServerSID)
upstreamTool := reader.String(tool.ToolSID)
authType := reader.String(tool.AuthTypeSID)
secretRef := reader.String(tool.SecretSID)
```

With a split view, call `view.ResolveMCPToolIDs` and materialize returned string
IDs with `view.MCPString`.

## Consumer: Inspection

```go
scopes := reader.ScopeIDs()
principals, ok := reader.PrincipalRoutes("workspace1")
mcpPaths, ok := reader.MCPPaths("workspace1")
```

Use inspector APIs for diagnostics and admin views, not the hot path.

## Common Mistakes

- Passing source database rows directly into Cherry.
- Making Cherry infer project/workspace reachability.
- Using one global scope when enforcement is workspace-scoped.
- Storing API keys or bearer tokens as secret values.
- Adding mutable rate-limit counters to `Reader`.
- Reusing hot-cache entries after swapping to a new bundle generation.
- Relying on Go map iteration order when preparing inputs.
- Materializing split-view LLM string IDs with the MCP reader, or MCP string IDs
  with the LLM reader.
- Treating split LLM and MCP pack manifests as if they should match.
