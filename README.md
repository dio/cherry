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
belong in the bundle and which principal slugs, models, MCP servers, and MCP
profiles are valid in each scope.

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
            ID:           "gpt-4o-mini",
            Provider:     "openai",
            Name:         "gpt-4o-mini",
            Mode:         "chat",
            Capabilities: []string{"vision", "tool_choice"},
            MetadataJSON: `{"model":"gpt-4o-mini","inputTokensPricePerMillion":"0.15","capabilities":["vision","tool_choice"]}`,
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

For production rebuild isolation, a producer can also publish separate LLM and
MCP artifacts from the same source selection. Each artifact still uses the same
bundle envelope; the producer only filters `cherry.Input` before building:

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
llmBundle.Metadata.GenerationID = "generation-2026-06-14T12:00:00Z"
mcpBundle.Metadata.GenerationID = "generation-2026-06-14T12:00:00Z"

llmBundleBytes, err := cherry.EncodeBundleZstd(llmBundle)
if err != nil {
    return err
}
mcpBundleBytes, err := cherry.EncodeBundleZstd(mcpBundle)
if err != nil {
    return err
}
```

This keeps MCP-only changes from rebuilding LLM policy/catalog sections, and
LLM-only changes from rebuilding MCP sections. The external producer still owns
which source records belong to each cluster; Cherry only packs the normalized
rows it receives.

If the control plane normally publishes bundles on a periodic cadence, use
`SnapshotPolicy` to decide which normalized mutable changes should interrupt
that cadence and publish a fresh immutable snapshot immediately. External key
watching and verification stay outside Cherry; after that system observes a key
change, it can map the event to a normalized principal or secret-ref change:

```go
decision := cherry.DefaultSnapshotPolicy().Decide([]cherry.SnapshotChange{
    {
        Kind:          cherry.SnapshotChangePrincipalBinding,
        ScopeID:       "workspace1",
        PrincipalSlug: "slug:project1",
        Reason:        "key binding changed",
    },
})
if decision.TakeSnapshot {
    // Rebuild the affected bundle or scope overlay, encode it, open it, and
    // publish it with the normal atomic generation swap.
}
```

Model catalog records can carry opaque normalized metadata. A producer can read
its raw model catalog, keep pricing, limits, modalities, and capabilities in
`Model.MetadataJSON`, and pass selected capability names in `Model.Capabilities`.
Cherry validates and packs that data but does not define the source catalog
schema.

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

LLM request resolution after an external verifier has produced a principal slug.
Use `ResolveLLMPlanIDs` when the enforcement point needs to execute fallback
chains or weighted splits:

```go
plan, ok := reader.ResolveLLMPlanIDs("workspace1", "slug:project1", "claude-haiku-4-5")
if !ok {
    // reject: no route for this scope, principal, and requested model
}

// plan.Plan is target, chain, or split. Target children contain provider,
// model, endpoint, and effective secret-ref IDs.
```

`ResolveLLMIDs` remains available for callers that only need the first
executable target from the compiled route tree.

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

MCP initialize resolution:

```go
init, ok := reader.ResolveMCPInitialize("workspace1", "profile-dev-tools")
if !ok {
    // reject: no MCP path for this scope
}

for _, server := range init.Servers {
    connect(server.Endpoint, server.AuthType, server.SecretRef)
}
```

For diagnostics and admin surfaces, the reader also exposes inspector methods:

```go
scopes := reader.ScopeIDs()
llmPrincipals, ok := reader.Principals("workspace1")
principals, ok := reader.PrincipalRoutes("workspace1")
mcpPaths, ok := reader.MCPPaths("workspace1")
```

Model catalog and MCP server queries are available from the same loaded bundle:

```go
model, ok := reader.ResolveModel("gpt-4o-mini")
supportsImages := reader.ModelCapability("gpt-5", "image_generation")
providers := reader.Providers()
provider, ok := reader.ResolveProvider("openai")
modelsJSON, err := reader.V1ModelsJSON()
openAIModelsJSON, err := reader.V1ModelsJSONForProvider("openai")
servers := reader.MCPServers()
server, ok := reader.ResolveMCPServer("github")
```

`V1ModelsJSON` renders the packed catalog in a `/v1/models` response shape while
preserving pricing and capability-derived fields from the normalized metadata.

The example directory includes fixture loaders for seed-style model/provider
catalogs and MCP catalogs. Those loaders are example producer code, not root
package schema contracts.

## Split LLM/MCP View

An enforcement point can also consume independently delivered LLM and MCP
bundles without changing the single-bundle envelope:

```go
opened, err := cherry.OpenSplitBundleZstdWithOptions(llmBundleBytes, mcpBundleBytes, cherry.SplitBundleOptions{
    GenerationID: "generation-2026-06-14T12:00:00Z",
})
if err != nil {
    return err
}

view := opened.View
```

`OpenSplitBundleZstd` and `OpenSplitBundleZstdWithOptions` open each artifact
with `OpenBundleZstd`, then validate that both bundles describe the same
control-plane selection and concrete scope set. The LLM and MCP pack manifests
are allowed to differ because the policy clusters are expected to rebuild
independently.

For stricter rollout checks, set `Bundle.Metadata.GenerationID` on each bundle
and use `OpenSplitBundleZstdWithOptions` with expected generation, component
checksums, or required catalog entries:

```go
opened, err := cherry.OpenSplitBundleZstdWithOptions(llmBundleBytes, mcpBundleBytes, cherry.SplitBundleOptions{
    GenerationID:         "generation-2026-06-14T12:00:00Z",
    LLMPackChecksum:      expectedLLMChecksum,
    MCPPackChecksum:      expectedMCPChecksum,
    RequiredLLMProviders: []string{"openai"},
    RequiredLLMModels:    []string{"gpt-4o-mini"},
    RequiredMCPServers:   []string{"github"},
})
if err != nil {
    return err
}
```

LLM calls use the LLM reader:

```go
llm, ok := view.ResolveLLMIDs("workspace1", "slug:project1", "claude-haiku-4-5")
if !ok {
    // reject
}

provider := view.LLMString(llm.ProviderSID)
```

MCP calls use the MCP reader:

```go
tool, ok := view.ResolveMCPToolIDs("workspace1", "profile-dev-tools", "github__list-repos")
if !ok {
    // reject
}

server := view.MCPString(tool.ServerSID)
```

String IDs are reader-local. Use `LLMString` for IDs returned by LLM methods and
`MCPString` for IDs returned by MCP methods. Materialized helpers such as
`ResolveLLM` and `ResolveMCP` handle this internally.

## Boundary

Cherry does not own tenancy joins, key verification, ownership checks, rule
authoring, or project/workspace fanout. Those belong to the external system.

The intended pipeline is:

```text
source records -> transformer -> normalized pack input -> pack bundle -> EP reader
```

See [example/README.md](example/README.md) for the source-to-pack example,
walkthrough, and CLI commands.
