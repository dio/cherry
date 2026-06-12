# Cherry

Cherry is a compact packed-configuration reader and writer for enforcement
points. It is designed for a control plane to deliver one immutable bundle, and
for an enforcement point to load that bundle into memory and query it without
inflating one Go object per key, route, or MCP profile.

The public API is the root Go package:

```go
import cherry "github.com/dio/cherry"
```

## Repository Layout

```text
go.mod
go.sum
README.md
pack.go
pack_bundle.go
pack_test.go
pack_bench_test.go
example/
  README.md
  main.go
  source/
  transform/
```

`pack.go` contains the compact table format and query engine. `pack_bundle.go`
adds the zstd bundle envelope used for control-plane to enforcement-point
delivery.

## Producing A Bundle

The producer is the control-plane-side code that owns source selection and turns
already-normalized external records into `cherry.Input`. It decides which scopes
belong in the bundle, which principal slugs are valid in each scope, and which
final LLM/MCP policies have already won after rule merge.

Cherry expects that work to be done before `BuildWithManifest`:

```text
source records
  -> external transformer
  -> cherry.Input
  -> BuildWithManifest
  -> NewBundle
  -> EncodeBundleZstd
  -> delivery bytes
```

Minimal shape:

```go
input := cherry.Input{
    Providers: []cherry.Provider{
        {
            ID:        "openai",
            Kind:      "openai",
            Endpoint:  "https://api.openai.com",
            SecretRef: "env://OPENAI_API_KEY",
        },
    },
    Models: []cherry.Model{
        {
            ID:       "gpt-4o-mini",
            Provider: "openai",
            Name:     "gpt-4o-mini",
        },
    },
    MCPServers: []cherry.MCPServer{
        {
            ID:        "github",
            Endpoint:  "https://api.github.com",
            AuthType:  "bearer",
            SecretRef: "env://GITHUB_TOKEN",
        },
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

bundleBytes, err := cherry.EncodeBundleZstd(bundle)
if err != nil {
    return err
}
```

The resulting `bundleBytes` are what the control plane stores or serves to an
enforcement point.

## Enforcement Point Consumption

The enforcement point fetches bytes from the control plane, opens the bundle, and
stores the returned reader behind its active generation pointer.

```go
bundleBytes, err := fetchCurrentBundle()
if err != nil {
    return err
}

opened, err := cherry.OpenBundleZstd(bundleBytes)
if err != nil {
    return err
}

reader := opened.Reader
```

LLM request resolution after an external verifier has produced a principal slug:

```go
ids, ok := reader.ResolveLLMIDs("workspace1", "slug:project1", "claude-haiku-4-5")
if !ok {
    // reject: no route for this scope, principal, and requested model
}

provider := reader.String(ids.ProviderSID)
targetModel := reader.String(ids.ModelSID)
secretRef := reader.String(ids.SecretSID)
```

MCP tool resolution:

```go
tool, ok := reader.ResolveMCPToolIDs("workspace1", "profile-dev-tools", "github__list-repos")
if !ok {
    // reject: no tool binding for this scope, path, and exposed tool name
}

server := reader.String(tool.ServerSID)
upstreamTool := reader.String(tool.ToolSID)
authType := reader.String(tool.AuthTypeSID)
secretRef := reader.String(tool.SecretSID)
```

For diagnostics and admin surfaces, the reader also exposes inspector methods:

```go
scopes := reader.ScopeIDs()
principals, ok := reader.PrincipalRoutes("workspace1")
mcpPaths, ok := reader.MCPPaths("workspace1")
```

## Boundary

Cherry does not own tenancy joins, key verification, ownership checks, rule
authoring, or project/workspace fanout. Those belong to the external system.

The intended pipeline is:

```text
source records -> transformer -> normalized pack input -> pack bundle -> EP reader
```

See [example/README.md](example/README.md) for the source-to-pack example,
walkthrough, and CLI commands.
