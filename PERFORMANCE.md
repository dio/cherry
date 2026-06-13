# Cherry Pack Performance Tracking

This is an ad hoc tracking document for Cherry pack performance research. It is
not a stable architecture document. Promote durable conclusions into `README.md`
or package godoc once the measurements and optimization direction settle.

## Highlights

- Provider/model catalog scale is not the main risk. Route/profile churn is the
  production-sensitive path.
- First no-format-change optimization cut temporary allocation materially:
  simple shared target routes at 1M route entries dropped from ~1.3 GB/op to
  ~341 MB/op in smoke runs.
- BYOK-heavy route entries remain the largest concern. At 1M route entries,
  smoke runs are still around ~2 GB/op after chunk 1.
- Final raw pack blobs are much smaller than temporary allocation, but still
  matter for transport. Example smoke sizes:
  - simple shared target, 1M route entries: ~25.9 MB raw pack, ~9.8 MB zstd
    bundle
  - unique BYOK target secret refs, 100k route entries: ~7.7 MB raw pack, ~2.0
    MB zstd bundle
  - MCP unique toolsets, 10k profiles x 10 tools: ~6.1 MB raw pack, ~0.8 MB
    zstd bundle
- CPU is probably acceptable at moderate route scale, but 1M route-entry rebuilds
  are not cheap enough for frequent reloads. Allocation/GC remains the primary
  optimization target.
- A typed struct-key route dedupe experiment reduced allocation count but
  regressed CPU badly; do not reintroduce it without a better hashing strategy.
- V2 incremental updates are documented as a future architecture path, preferably
  via immutable static catalog packs plus mutable policy/profile overlays rather
  than in-place blob patching.

## Context

Cherry builds compact immutable enforcement packs from normalized `cherry.Input`.
In Plum hot reload, Cherry pack build sits on the reload path:

```text
source snapshot -> transformer -> cherry.Input -> cherry.BuildWithManifest
  -> bundle encode -> bundle open -> generation publish -> atomic pointer swap
```

Provider and model catalogs are expected to be mostly static in production.
The higher-churn surface is principal and policy data:

- user key to principal route bindings
- fallback chains
- weighted splits
- BYOK secret refs
- rate policies
- MCP profiles and tool bindings

That means provider/model scale is still useful as a baseline, but the primary
reload risk is route/profile churn and the temporary allocation generated during
full rebuild.

## Initial Finding

A one-iteration benchmark smoke on an Apple M1 showed catalog scale is much
smaller than route scale. For example, 500 providers and 5,000 models built in
tens of milliseconds in the smoke run.

The concerning result is route-entry scale. For 1,000,000 principal-model route
entries, the final pack blob was only tens of MB, but the cumulative temporary
allocation per build reached GB-scale:

```text
simple shared target routes:
  blob:    ~25.9 MB
  alloc:   ~1.3 GB/op
  allocs:  ~6.0 M/op

unique BYOK target secret refs:
  blob:    ~77.9 MB
  alloc:   ~2.7 GB/op
  allocs:  ~8.0 M/op
```

This does not mean Cherry retains multi-GB resident memory after the build. It
means the builder is generating a lot of temporary garbage while walking and
deduplicating routes, constructing route keys, expanding principal entries,
sorting, interning strings, and writing tables. The likely production impact is
reload-time CPU and GC pressure during full rebuild.

These are smoke numbers only. They are useful for direction, not for pass/fail
decisions.

## Benchmarks Added

`pack_bench_test.go` now includes:

- `BenchmarkCherryPackCatalogScale`
- `BenchmarkCherryPackRouteScale`
- `BenchmarkCherryPackMCPProfileScale`

The benchmarks report:

- `ns/op`
- `B/op`
- `allocs/op`
- `blob_bytes`
- `zstd_bundle_bytes`

### Catalog Scale

Purpose: measure mostly static provider/model catalog cost.

Cases:

```text
providers=500, models=10
providers=500, models=100
providers=500, models=500
providers=500, models=1000
providers=500, models=5000
```

### Route Scale

Purpose: measure mutable principal-route cost.

Cases:

```text
route_entries=100
route_entries=1,000
route_entries=10,000
route_entries=100,000
route_entries=1,000,000
```

Shapes:

```text
simple-target-shared
fallback-chain-2-shared
fallback-chain-3-shared
weighted-split-2-shared
byok-target-unique-secret
rate-policy-shared
rate-policy-unique
```

### MCP Profile Scale

Purpose: measure mutable MCP profile/toolset cost.

Cases:

```text
profiles=100, tools_per_profile=10
profiles=1,000, tools_per_profile=10
profiles=10,000, tools_per_profile=10
profiles=1,000, tools_per_profile=100
```

Shapes:

```text
shared-toolsets
unique-toolsets
unique-secret-refs
```

## Measurement Protocol

Use repeated benchmark runs before drawing conclusions:

```sh
go test -run '^$' \
  -bench '^BenchmarkCherryPack(CatalogScale|RouteScale|MCPProfileScale)$' \
  -benchmem \
  -benchtime=3s \
  -count=30 \
  ./... | tee /tmp/cherry-pack-scale.txt
```

Record for each benchmark:

```text
p50 ns/op
p99 ns/op
B/op
allocs/op
blob_bytes
zstd_bundle_bytes
Go version
GOOS/GOARCH
CPU
GOMAXPROCS
command
```

`benchstat` is useful for comparing before/after changes, but p50/p99 should be
computed from repeated raw samples.

## Profiling Protocol

Profile the slowest route and MCP cases before choosing an optimization:

```sh
go test -run '^$' \
  -bench 'BenchmarkCherryPackRouteScale/.*/route_entries=1000000' \
  -benchtime=10s \
  -cpuprofile=/tmp/cherry-route.cpu \
  -memprofile=/tmp/cherry-route.mem \
  ./...

go tool pprof -top /tmp/cherry-route.cpu
go tool pprof -alloc_objects -top /tmp/cherry-route.mem
```

Repeat for the slowest MCP profile case:

```sh
go test -run '^$' \
  -bench 'BenchmarkCherryPackMCPProfileScale/.*/profiles=10000' \
  -benchtime=10s \
  -cpuprofile=/tmp/cherry-mcp.cpu \
  -memprofile=/tmp/cherry-mcp.mem \
  ./...

go tool pprof -top /tmp/cherry-mcp.cpu
go tool pprof -alloc_objects -top /tmp/cherry-mcp.mem
```

## Decision Rules

Do not add Plum hot-reload timing assertions until Cherry route/profile scale is
characterized.

If catalog scale is slow but route/profile scale is acceptable, defer catalog
optimization because providers and models rarely change.

If route scale has high p99 latency or GB-scale allocation at realistic
principal-route counts, open a Cherry optimization issue focused on route churn.

If MCP profile scale has high p99 latency or excessive allocation at realistic
profile/tool counts, open a Cherry optimization issue focused on profile churn.

## Optimization Candidates

These are hypotheses to validate with profiles, not decisions:

- reduce `routeKey` temporary string allocation
- split route shape from per-principal secret/rate metadata
- preserve route dedupe when BYOK secret refs vary
- precompute or intern route keys using a structured hash instead of strings
- cache static provider/model/MCP server sections by input hash
- split mostly static catalog tables from mutable route/profile tables
- incrementally rebuild changed scopes or principals
- shard bundles by scope/workspace to avoid rebuilding unrelated scopes
- reduce temporary slices and maps in principal/profile expansion

## V2 Direction: Incremental Updates

Incremental updates are a plausible v2 architecture, but should avoid in-place
mutation of an existing pack blob. The current pack is offset-based,
checksum-protected, and intentionally immutable once opened by a `Reader`.
Patching it in place would complicate validation, concurrency, and generation
swap semantics.

The safer v2 shape is immutable replacement at smaller granularity:

```text
static catalog pack + mutable policy/profile overlay pack(s)
```

Potential split:

- static pack: providers, models, MCP server catalog, mostly static strings
- mutable overlay: scopes, principals, route bindings, rate policies, MCP
  profiles, tool bindings
- optional sharding: one overlay per scope/workspace

Expected benefits:

- provider/model catalog changes do not force route/profile rebuilds
- one user key or BYOK change does not rebuild unrelated scopes
- `Reader` remains immutable
- generation swap remains atomic
- old generations can still be safely held by in-flight requests

Open design questions:

- whether overlays are exposed as a new root API or hidden behind a higher-level
  EP view
- how manifests/checksums compose across base and overlay packs
- whether route tables can reference static model/provider tables by stable IDs
- whether bundle delivery should ship one combined envelope or multiple blobs
- how to preserve deterministic inspector APIs across base and overlays

Do not start v2 until the current full-build allocation reductions are measured.
If full rebuild becomes acceptable at realistic route/profile scale, v2 may be
unnecessary for the near term.

## Optimization Log

### Chunk 1: Carry Compiled IDs Into Table Writers

Status: implemented.

Change:

- `internRoute` now stores child route IDs in an internal `compiledRoute`.
- the first build pass records principal route IDs in `compiledScope`.
- the first build pass records MCP profile toolset IDs in `compiledScope`.
- `writeRoutes` no longer recomputes `routeKey(child)` for chain/split
  children.
- `writeScopes` no longer recomputes `routeKey(principal.route)`.
- `writeScopes` no longer recomputes `toolsetKey(canonicalToolset(...))`.

This preserves the binary format and public API.

Benchmark metrics now also report `zstd_bundle_bytes`, computed outside the
timed loop. `blob_bytes` is the raw pack size; `zstd_bundle_bytes` is the
transported zstd bundle size.

Focused benchmark smoke, before -> after:

```text
simple shared target, 100k route entries:
  alloc:   ~136 MB/op -> ~36 MB/op
  allocs:  ~600k/op  -> ~300k/op
  time:    ~116 ms/op -> ~92-100 ms/op

simple shared target, 1M route entries:
  alloc:   ~1.3 GB/op -> ~341 MB/op
  allocs:  ~6.0M/op   -> ~3.0M/op
  time:    ~1.6-2.0s/op -> ~1.2-1.3s/op

fallback chain 3, 100k route entries:
  alloc:   ~168 MB/op -> ~52 MB/op
  allocs:  ~1.0M/op   -> ~500k/op
  time:    ~145-152 ms/op -> mostly ~112-132 ms/op

unique BYOK target secret refs, 100k route entries:
  alloc:   ~277 MB/op -> ~196 MB/op
  allocs:  ~781k/op   -> ~391k/op
  time:    ~176-213 ms/op -> ~146-156 ms/op

unique BYOK target secret refs, 1M route entries:
  alloc:   ~2.7 GB/op -> ~2.0 GB/op
  allocs:  ~8.0M/op   -> ~4.0M/op
  time:    ~3.0-4.0s/op -> ~3.1-3.4s/op

MCP shared toolsets, 10k profiles x 10 tools:
  alloc:   ~94 MB/op -> ~58 MB/op
  allocs:  ~250k/op  -> ~140k/op
  time:    ~40-52 ms/op -> ~27-28 ms/op

MCP unique toolsets, 10k profiles x 10 tools:
  alloc:   ~135 MB/op -> ~100 MB/op
  allocs:  ~231k/op   -> ~131k/op
  time:    ~83-88 ms/op -> ~64-71 ms/op
```

Remaining concern: unique BYOK route entries still allocate around 2 GB/op at
1M route entries. The next optimization should focus on the route-key and route
dedupe model for per-principal secret refs.

### Chunk 2: Precompute Scope-Local Lookup Hashes

Status: implemented.

Change:

- compiled principal entries now store `principalLookupHash`.
- compiled MCP profile entries now store path hash.
- `writeScopes` sorts and writes those precomputed hashes instead of
  recalculating them in sort comparators and table emission.

This preserves the binary format and public API.

This chunk targets repeated hashing inside scope-local sorting and writing, not
route-key allocation. Focused smoke runs were noisy; keep this as a low-risk CPU
cleanup and rely on the full repeated benchmark set for a stable before/after.

Rejected experiment: using a typed struct key for target route dedupe greatly
reduced allocation count, but regressed CPU because every route lookup had to
hash multiple string fields. Do not reintroduce that approach without a better
hashing strategy and benchmark proof.

## Current Status

- Benchmarks added in `pack_bench_test.go`.
- First no-format-change allocation reduction implemented in `pack.go`.
- Scope-local lookup hash precomputation implemented in `pack.go`.
- `go test ./...` passes.
- `make format` passes.
- `make lint` passes.
- Full `-benchtime=3s -count=30` characterization is still pending.
- CPU and allocation profiles for the remaining BYOK-heavy case are still
  pending.
