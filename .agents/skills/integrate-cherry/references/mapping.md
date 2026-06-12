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
        {ID: "gpt-4o-mini", Provider: "openai", Name: "gpt-4o-mini"},
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

## Consumer: LLM Query

```go
ids, ok := reader.ResolveLLMIDs("workspace1", "slug:project1", "gpt-4o-mini")
if !ok {
    return reject()
}

provider := reader.String(ids.ProviderSID)
model := reader.String(ids.ModelSID)
secretRef := reader.String(ids.SecretSID)
```

The principal slug should come from a verifier outside Cherry.

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
