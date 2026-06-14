---
name: integrate-cherry
description: Integrate Cherry into producer or enforcement-point consumer code. Use when mapping external normalized tenancy/policy data into github.com/dio/cherry Input and zstd bundles, or when loading/querying Cherry bundles in an EP for LLM or MCP enforcement.
---

# Integrate Cherry

## Supported Shapes

Use this skill when connecting Cherry to a control-plane producer, bundle server,
or enforcement-point consumer.

Cherry has two supported integration shapes:

- **Single bundle**: one `cherry.Input` becomes one immutable zstd bundle. This
  is the default and simplest production path.
- **Mapped split**: one control-plane split map points to one low-churn
  `llm-generic` bundle, one low-churn `mcp-servers` bundle, and partitioned
  high-churn `llm-user-key-*` and `mcp-user-profile-*` bundles.

Do not recommend a fixed four-bundle layout such as `llm-generic`,
`llm-keys`, `mcp-servers`, and `mcp-profiles`. If the user wants bundle
splitting, guide them to mapped split. If they do not need mapped split, guide
them to a single bundle.

Cherry remains deliberately narrow. Keep these responsibilities outside the root
package:

- source schema reads
- tenancy joins
- ownership checks
- key/JWT verification
- rule authoring and rule precedence
- project/workspace reachability
- secret material resolution

The boundary is:

```text
source records -> external transformer -> cherry.Input -> Cherry bundle bytes
```

The EP boundary is:

```text
verified request values -> Cherry Reader/query -> route/auth decision data
```

## Choose The Shape

Choose **single bundle** when:

- bundle size and rebuild cadence are acceptable
- atomic full-generation swaps are simple enough
- the EP wants one reader and one string table
- the product is still early in policy volume or churn

Choose **mapped split** when:

- default LLM routing changes less often, but per-key routes/rates/BYOK churn
  often
- MCP server catalog and `/mcp/s/<server>` paths change less often, but
  `/mcp/<profile>` paths churn often
- high-churn user-key or profile policy is large enough that partition rebuilds
  matter
- the EP can load a map, open multiple immutable readers, and atomically swap a
  composed mapped view

Mapped split lanes:

```text
shared / low churn:
  llm-generic
    providers
    models
    default/platform routes
    platform secret refs

  mcp-servers
    MCP server catalog
    direct s/<server> paths
    platform secret refs

partitioned / high churn:
  llm-user-key-{000..N}
    principal/key routes
    routing overrides
    BYOK secret refs
    rate policies

  mcp-user-profile-{000..N}
    profile paths
    selected tools
    user/profile secret refs
```

Use `cherry.MappedSplitSpec` for lane names, component names, and partition
selection. Do not copy hash math into integrator code.

## Single Bundle Server Workflow

Use this when implementing the producer or bundle server for one bundle.

1. Read source records in product-owned code.
2. Perform tenancy, ownership, reachability, rule precedence, key lookup, and
   secret-ref selection outside Cherry.
3. Produce final normalized rows:
   - `cherry.Provider`
   - `cherry.Model`
   - `cherry.MCPServer`
   - `cherry.Principal`
   - `cherry.MCPProfile`
4. Preserve model catalog fields the EP needs in `Model.MetadataJSON` and
   `Model.Capabilities`.
5. Prefer `Principal.ModelRoutes` when one principal can route multiple
   requested model IDs.
6. Store secret refs only. Never put secret material in `cherry.Input`.
7. Build one immutable bundle:

   ```go
   blob, manifest, err := cherry.BuildWithManifest(input)
   if err != nil {
       return err
   }

   bundle := cherry.NewBundle(scopeKind, scopeID, scopes, blob, manifest)
   bundle.Metadata.GenerationID = generationID

   payload, err := cherry.EncodeBundleZstd(bundle)
   if err != nil {
       return err
   }
   ```

8. Publish the payload behind a generation-aware URL or object key.
9. Move the server's `current` pointer only after the payload is written and
   readable.

Recommended single-bundle `current` shape:

```json
{
  "scope_kind": "project",
  "scope_id": "project1",
  "scopes": ["workspace1", "workspace2"],
  "generation_id": "gen42",
  "bundle": {
    "url": "/cherry/v1/bundles/project/project1/gen42/bundle.zst",
    "checksum": 1001,
    "size": 4096
  }
}
```

## Single Bundle EP Workflow

Use this when implementing the enforcement point for one bundle.

1. Fetch `current`.
2. Fetch the bundle URL from `current`.
3. Open and validate:

   ```go
   opened, err := cherry.OpenBundleZstd(payload)
   if err != nil {
       return err
   }
   if opened.Metadata.GenerationID != generationID {
       return fmt.Errorf("generation mismatch")
   }
   if opened.Metadata.PackManifest.Checksum != expectedChecksum {
       return fmt.Errorf("checksum mismatch")
   }
   ```

4. Publish `opened.Reader` behind the active generation pointer.
5. Never mutate `Reader`; swap the whole active view on generation changes.
6. Clear any wrapper cache on generation swap.

LLM hot path:

```go
ids, ok := reader.ResolveLLMPlanIDs(scopeID, principalSlug, requestedModel)
if !ok {
    return reject()
}
secretRef := reader.String(ids.SecretSID)
```

MCP initialize:

```go
init, ok := reader.ResolveMCPInitializeIDs(scopeID, pathSuffix)
if !ok {
    return reject()
}
```

MCP tool call:

```go
tool, ok := reader.ResolveMCPToolIDs(scopeID, pathSuffix, exposedTool)
if !ok {
    return reject()
}
serverID := reader.String(tool.ServerSID)
```

Use string-materializing APIs such as `ResolveLLM`, `ResolveMCPInitialize`, and
`ResolveMCP` for diagnostics or low-QPS tooling. Prefer ID-returning APIs on the
request path.

## Mapped Split Server Workflow

Use this when implementing the producer or bundle server for high-churn mapped
split.

The server publishes:

```text
split map:
  format_version
  scope_kind
  scope_id
  scopes
  generation_id
  map_revision
  llm_default_principal_slug
  partitioning
  bundles
  partition_bundles

shared low-churn bundles:
  llm-generic
  mcp-servers

partition bundles:
  llm-user-key-000
  llm-user-key-001
  ...
  mcp-user-profile-000
  mcp-user-profile-001
  ...
```

Initialize the spec:

```go
spec := cherry.MappedSplitSpec{
    LLMUserKeyPartitions:     64,
    MCPUserProfilePartitions: 64,
}
if err := spec.Validate(); err != nil {
    return err
}
```

Build low-churn lanes:

```go
llmGeneric, err := spec.CatalogBundle(cherry.MappedSplitLaneLLMGeneric)
if err != nil {
    return err
}
mcpServers, err := spec.CatalogBundle(cherry.MappedSplitLaneMCPServers)
if err != nil {
    return err
}

// Build llmGeneric.Component() from default LLM routes, providers, models,
// scopes, and a default principal such as slug:default.
// Build mcpServers.Component() from MCP servers and direct s/<server> paths.
// These bundles are low-churn, not permanent; rebuild them whenever their
// defaults, catalogs, server paths, or platform secret refs change.
```

Build LLM user-key partitions:

```go
for _, principal := range principals {
    key, err := spec.LLMUserKeyBundle(principal.Slug)
    if err != nil {
        return err
    }

    // Add this principal only to key.Partition.
    // Build and publish key.Component(), for example llm-user-key-003.
}
```

Build MCP profile partitions:

```go
for _, profile := range profiles {
    if strings.HasPrefix(profile.Path, "s/") {
        continue
    }

    key, err := spec.MCPUserProfileBundle(profile.Path)
    if err != nil {
        return err
    }

    // Add this profile only to key.Partition.
    // Build and publish key.Component(), for example mcp-user-profile-011.
}
```

Every component is still a normal Cherry bundle:

```text
filtered cherry.Input -> BuildWithManifest -> NewBundle -> EncodeBundleZstd
```

Each component should carry the same `ScopeKind`, `ScopeID`, concrete `Scopes`,
and `GenerationID`. The split map records each component URL, checksum, and
size.

## Mapped Split Change Handling

Build does not detect source-record diffs. The producer classifies changes
explicitly, then asks `MappedSplitSpec` which component is affected.

Use this mapping:

- `MappedSplitChangeLLMGeneric`: provider/model/default route/platform LLM
  secret-ref changes; rebuild `llm-generic`
- `MappedSplitChangeMCPServers`: MCP server catalog/direct server/platform MCP
  secret-ref changes; rebuild `mcp-servers`
- `MappedSplitChangeLLMUserKey`: principal route/BYOK/rate changes; rebuild one
  `llm-user-key-*` partition
- `MappedSplitChangeMCPUserProfile`: profile path/tool/secret changes; rebuild
  one `mcp-user-profile-*` partition

Example:

```go
affected, err := spec.AffectedBundle(cherry.MappedSplitChange{
    Kind:          cherry.MappedSplitChangeLLMUserKey,
    PrincipalSlug: principalSlug,
})
if err != nil {
    return err
}

// Rebuild affected.Component(), publish its immutable URL/checksum, then
// publish a new split map revision.
```

For an N+1 update:

1. Build the changed component first.
2. Store it at a new immutable URL.
3. Create a new split map with `map_revision + 1`.
4. Keep unchanged component refs pointing at old immutable URLs.
5. Replace the changed partition ref with the new URL/checksum.
6. Omit, replace, or point unused partitions at empty valid bundles according to
   the map policy.
7. Publish the new map after all referenced component URLs are readable.

## Mapped Split EP Workflow

Mapped split currently uses normal opened `Reader` values plus a product-owned
composed view. The EP should not blindly fetch every bundle on every map
revision. It should diff the next map against the active view and fetch only
missing or stale component refs.

A component ref is reusable when all of these match the active opened reader:

```text
generation_id
component URL
pack checksum
pack size
```

A component ref is missing when the active view has no reader for that lane or
partition. A component ref is stale when the lane/partition exists but any of
generation, URL, checksum, or size changed. Missing or stale refs must be
fetched, opened, and validated before publishing the next view.

1. Fetch the split map.
2. Validate the map envelope through the trusted delivery layer.
3. Build `MappedSplitSpec` from the map's partition counts.
4. Compare `llm-generic` and `mcp-servers` refs with the active view; reuse
   matching readers and fetch stale or missing refs.
5. Compare every listed `llm-user-key-*` and `mcp-user-profile-*` partition ref
   with the active view; reuse matching readers and fetch stale or missing refs.
6. Treat omitted partition refs as absent in the next view. Do not keep serving
   a removed partition just because the old reader is still available.
7. Open fetched payloads with `OpenBundleZstd`.
8. Validate every newly opened bundle against the map:
   - scope kind
   - selected scope ID
   - concrete scope list
   - generation ID
   - pack checksum
   - pack size
9. Build an immutable product-owned view:

   ```text
   llmDefaultPrincipalSlug string
   spec MappedSplitSpec
   refs for each lane/partition
   llmGeneric Reader
   mcpServers Reader
   llmUserKey []Reader
   mcpUserProfile []Reader
   ```

10. Publish the new view atomically.
11. Copy slices before replacing readers. Never mutate the active view or a
    `Reader` in place.

EP change detection sketch:

```go
func reusable(activeRef, nextRef BundleRef, activeGeneration, nextGeneration string) bool {
    return activeGeneration == nextGeneration &&
        activeRef.URL == nextRef.URL &&
        activeRef.Checksum == nextRef.Checksum &&
        activeRef.Size == nextRef.Size
}

if reusable(active.llmGenericRef, next.Bundles["llm-generic"], active.generationID, next.GenerationID) {
    nextView.llmGeneric = active.llmGeneric
} else {
    nextView.llmGeneric = fetchOpenValidate(next.Bundles["llm-generic"])
}

for _, ref := range next.PartitionBundles["llm-user-key"] {
    if reusable(active.llmUserKeyRefs[ref.Partition], ref.BundleRef(), active.generationID, next.GenerationID) {
        nextView.llmUserKey[ref.Partition] = active.llmUserKey[ref.Partition]
        continue
    }
    nextView.llmUserKey[ref.Partition] = fetchOpenValidate(ref.BundleRef())
}
```

The example `mapped-split-demo` prints this behavior during the N+1 update:
unchanged refs are reused, the changed partition is fetched, and the omitted MCP
profile partition is not carried into the next view.

Mapped split LLM query:

```go
key, err := view.spec.LLMUserKeyBundle(principalSlug)
if err != nil {
    return reject()
}

reader := view.llmUserKey[key.Partition]
ids, ok := reader.ResolveLLMPlanIDs(scopeID, principalSlug, requestedModel)
if ok {
    // Materialize strings from reader.
    return allow(ids)
}

ids, ok = view.llmGeneric.ResolveLLMPlanIDs(
    scopeID,
    view.llmDefaultPrincipalSlug,
    requestedModel,
)
if !ok {
    return reject()
}
```

Mapped split MCP query:

```go
if strings.HasPrefix(pathSuffix, "s/") {
    ids, ok := view.mcpServers.ResolveMCPToolIDs(scopeID, pathSuffix, exposedTool)
    if !ok {
        return reject()
    }
    return allow(ids)
}

key, err := view.spec.MCPUserProfileBundle(pathSuffix)
if err != nil {
    return reject()
}

reader := view.mcpUserProfile[key.Partition]
ids, ok := reader.ResolveMCPToolIDs(scopeID, pathSuffix, exposedTool)
if !ok {
    return reject()
}
```

The reader that produced the IDs owns the string table. Materialize strings from
that reader.

## Validation Checklist

Producer:

- source records are filtered and authorized before `cherry.Input`
- project/workspace fanout is complete before `BuildWithManifest`
- final route precedence is already resolved
- only secret refs are present
- `BuildWithManifest` rejects unknown provider/model/server references
- single-bundle generation document points at one immutable bundle
- mapped split map points at immutable component URLs with checksums and sizes
- mapped split low-churn lanes contain only shared defaults/server paths
- mapped split partition lanes contain only rows for that partition
- `MappedSplitSpec` is used for component names and partitioning

EP:

- corrupt bundles fail to open
- unsupported versions fail to open
- scope kind, selected scope, concrete scopes, generation, checksum, and size are
  validated against the trusted document or map
- missing scope/principal/model/tool rejects
- LLM route returns expected provider/model/secret ref
- MCP initialize/list/call return expected upstream server/tool/auth/secret ref
- generation swap clears any wrapper cache
- mapped split queries materialize strings from the reader that produced IDs
- mapped split map updates fetch only missing or stale component refs and reuse
  unchanged opened readers
- omitted mapped split partition refs stop resolving after the next view is
  published

## References

- `README.md`: root package overview and single-bundle basics
- `DESIGN.md`: mapped split map shape, build/read simulation, and N+1 update
  summary
- `example/README.md`: runnable CLI examples
- `mapped_split_integration_test.go`: executable mapped split producer/consumer
  simulation
- `example/main.go`: `mapped-split-demo` CLI
