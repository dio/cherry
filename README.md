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

`DESIGN.md` summarizes the stable mapped-split delivery design for high-churn
user-key and MCP profile policy.

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

For production delivery, Cherry recommends mapped split when route/profile churn
or bundle size makes full-generation rebuilds too expensive. A mapped split
generation publishes a small control map plus normal zstd Cherry bundles:

```text
llm-generic                  low-churn providers, models, default routes
mcp-servers                  low-churn MCP server catalog and s/<server> paths
llm-user-key-{000..N}        partitioned principal routes, BYOK, rate policy
mcp-user-profile-{000..N}    partitioned MCP profile paths and tool bindings
```

Use `MappedSplitSpec` to keep producer and enforcement-point code on the same
stable lane names and partition math:

```go
spec := cherry.MappedSplitSpec{
    LLMUserKeyPartitions:     64,
    MCPUserProfilePartitions: 64,
}

affected, err := spec.AffectedBundle(cherry.MappedSplitChange{
    Kind:          cherry.MappedSplitChangeLLMUserKey,
    PrincipalSlug: "slug:project1",
})
if err != nil {
    return err
}

// Rebuild affected.Component(), for example llm-user-key-003.
```

Cherry does not infer source-record diffs. The control plane classifies whether
a change belongs to generic LLM policy, MCP servers, a key-specific LLM route,
or an MCP profile, then `MappedSplitSpec` computes the exact component. See
`DESIGN.md` and `go run ./example mapped-split-demo ...` for the producer and EP
shape. On the EP side, compare each new map ref with the active view by
generation, URL, checksum, and size; fetch only missing or stale refs and reuse
unchanged opened readers.

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
its raw model catalog, promote fields Cherry needs for stable query semantics,
and keep the full normalized row in `Model.MetadataJSON`.

For a seed row such as `gpt-image-2`, the field boundary is:

| Source field | Cherry field | Notes |
| --- | --- | --- |
| `model` | `Model.ID`, `Model.Name` | Logical requested model ID and upstream model name. |
| `provider` | `Model.Provider` | Must reference an `Input.Providers` entry. |
| `mode` | `Model.Mode` | First-class catalog metadata, e.g. `chat`, `responses`, `image_generation`. |
| `capabilities` | `Model.Capabilities` | First-class capability list used by `ModelCapability`. |
| `modalities` | `Model.Modalities` | First-class input/output modality lists for request shaping and diagnostics. |
| `additionalPricePerMillion` | `Model.AdditionalPricePerMillion` | First-class open catalog object for provider- and mode-specific price dimensions. |
| `limits` | `Model.Limits` | First-class open catalog object for provider- and mode-specific request limits. |
| Full enabled row | `Model.MetadataJSON` | Opaque JSON for source-specific fields and future catalog changes. |

Fields such as `backendUrls`, `metadata.aliases`, `metadata.options`,
`metadata.source_url`, `metadata.description`, and `metadata.display_name` are
preserved in `Model.MetadataJSON`. They are not first-class pack fields today
because Cherry does not make routing or compatibility decisions from them.

`AdditionalPricePerMillion` and `Limits` intentionally use open JSON objects:
the seed schema allows provider- and mode-specific keys such as
`image_tokens`, `image_generation`, `max_edge_px`, `edge_multiple_px`,
`max_total_pixels`, or `max_long_edge_to_short_edge_ratio`. This keeps those
objects queryable without hard-coding every provider-specific nested shape.
`V1ModelsJSON` uses the typed `Limits` object for `max_output_tokens` and still
uses `MetadataJSON` for source pricing fields needed by the OpenAI-compatible
model-list projection.

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

## Boundary

Cherry does not own tenancy joins, key verification, ownership checks, rule
authoring, or project/workspace fanout. Those belong to the external system.

The intended pipeline is:

```text
source records -> transformer -> normalized pack input -> pack bundle -> EP reader
```

See [example/README.md](example/README.md) for the source-to-pack example,
walkthrough, and CLI commands.
