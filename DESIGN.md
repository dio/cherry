# Cherry Design

This document records stable design guidance for Cherry delivery. Cherry keeps
the root package boundary narrow: product-owned systems normalize source records
into `cherry.Input`, then Cherry builds immutable zstd bundles that enforcement
points open and query.

## Recommended Delivery

Use a single bundle when policy volume and rebuild cadence are modest. Use mapped
split for production deployments where user-key routes, BYOK secret refs, rate
policy, or MCP profile bindings churn often enough that rebuilding and loading a
full bundle is too costly.

Mapped split keeps the pack format unchanged. The control plane publishes one
`mapped-split-v1` map plus normal Cherry bundle artifacts:

```text
shared / low churn:
  llm-generic
    providers, models, default/platform LLM routes, platform secret refs

  mcp-servers
    MCP server catalog, direct s/<server> paths, platform secret refs

partitioned / high churn:
  llm-user-key-{000..N}
    principal routes, BYOK secret refs, route/rate overrides

  mcp-user-profile-{000..N}
    profile paths, selected tools, user/profile secret refs
```

The map lists scope kind, selected scope ID, concrete scopes, generation ID, map
revision, partition algorithm/counts, and each component URL/checksum/size.
Component URLs should be immutable and readable before the map revision is
published.

## Producer Contract

Cherry does not classify source diffs. The producer explicitly classifies a
change as generic LLM policy, MCP server policy, one principal/key partition, or
one MCP profile partition, then uses `MappedSplitSpec` to compute the affected
component name.

Every component is still built as:

```text
filtered cherry.Input -> BuildWithManifest -> NewBundle -> EncodeBundleZstd
```

Each component should carry the same scope kind, selected scope ID, concrete
scopes, and generation ID. Partition bundles may duplicate provider/model or MCP
server catalog rows because current route and tool records use reader-local
table IDs.

## Enforcement-Point Contract

The EP fetches the next map, compares every component ref with the active mapped
view, and fetches only missing or stale refs. A ref is reusable only when
generation ID, URL, checksum, and size all match the active reader.

Fetched bundles are opened with `OpenBundleZstd` and validated against the map:
scope kind, selected scope ID, concrete scopes, generation ID, checksum, and
size. Omitted partition refs are absent in the next view; the EP must not keep
serving an omitted partition from the previous view.

The active mapped view is immutable and swapped atomically. Any cache above
`Reader` must live outside the reader and be cleared when a generation or
partition reader changes.

LLM queries first route to the principal's `llm-user-key-*` partition and fall
back to `llm-generic` using the configured default principal slug. MCP queries
route `s/<server>` paths to `mcp-servers`; other paths route to their
`mcp-user-profile-*` partition.

String IDs are reader-local. Production wrappers should either materialize
strings through the reader that produced the IDs or return source-aware ID
results.

## Open Design Edges

Mapped split is stable, but these choices remain product policy:

- MCP partition key: path suffix is the default; products may choose owner,
  profile ID, or tenant ID when that keeps related paths together.
- Partition count: fixed counts are simple, but resizing moves keys unless a
  future consistent-hashing or virtual-bucket scheme is introduced.
- Sparse load policy: an EP can require all partitions before publishing or
  allow sparse arrays that reject missing high-churn partitions.
- Split-map signing: Cherry expects the trusted delivery layer to sign or verify
  the map, then validate bundle checksums against it.
