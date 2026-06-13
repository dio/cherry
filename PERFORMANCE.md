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

Profile the slowest route and MCP cases before choosing an optimization.

Go rejects `-cpuprofile` when `go test` targets multiple packages. Run CPU and
memory benchmark profiles against the package that owns the benchmark instead of
`./...`. The route and MCP scale benchmarks live in the root package, so use
`.`:

```sh
go test -run '^$' \
  -bench 'BenchmarkCherryPackRouteScale/.*/route_entries=1000000' \
  -benchtime=10s \
  -cpuprofile=/tmp/cherry-route.cpu \
  -memprofile=/tmp/cherry-route.mem \
  .

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
  .

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

### Reactive Snapshot Scenario

The current production cadence may remain periodic, but high-priority mutable
events should interrupt that cadence and publish a fresh immutable generation.
Cherry should still start after the external source system has normalized the
event:

```text
source key/secret/profile event
  -> external watcher/verifier/transformer
  -> normalized SnapshotChange
  -> SnapshotPolicy.Decide
  -> rebuild affected bundle or future overlay shard
  -> encode/open/validate
  -> atomic generation swap
```

For example, if key material changes, Cherry must not watch, verify, or store
that material. The upstream system should map the event to a normalized mutable
change such as `SnapshotChangePrincipalBinding` when the verified principal
membership changes, or `SnapshotChangeSecretRef` when the effective credential
reference changes. Under `DefaultSnapshotPolicy`, those changes request an
immediate snapshot even when the normal cadence is `periodic`.

Static catalog events, such as provider/model metadata refreshes, can wait for
the next periodic snapshot unless the caller opts into `SnapshotCadenceReactive`
or adds that kind to `ReactiveKinds`.

### BYOK Route Allocation Insight

Unique BYOK route entries are expensive primarily because the current route
dedupe identity combines route shape with credential binding. Target route keys
include:

```text
provider + model + secret_ref
```

The original `byok-target-unique-secret` benchmark is a stress shape where the
input emits a different secret ref for every principal/requested-model pair. That
is not the expected product-default BYOK shape. In the real UI, a user typically
sets one BYOK credential per provider, such as one Anthropic key and one OpenAI
key, then chooses a policy:

- `ALWAYS`: use the user's provider key
- `PREFER`: try the user's provider key and fall back to the platform provider
  key when the BYOK attempt fails

Rules can still express per-model overrides, so the stress benchmark remains
valid as an upper bound, but the benchmark matrix now also includes
provider-level BYOK shapes:

```text
byok-provider-secret-always
byok-provider-secret-prefer
```

Even with provider-level credential refs, the current format still duplicates
target route records by principal/requested-model because the secret ref is part
of target route identity. Provider-level normalization reduces unique secret
strings from roughly principal/model cardinality to principal/provider
cardinality, but it does not fully recover route-shape dedupe.

The route tree shape is not the first-order problem in that benchmark because
the worst stress case is already a single target node. In the real `PREFER`
provider-key shape, trees do amplify the issue: the platform fallback target is
shared, but the chain parent remains unique because its BYOK child is unique.
The root issue is still coupling route-shape dedupe to per-principal credential
metadata.

Near-term producer guidance:

- normalize credential refs to the coarsest semantically correct level before
  building `cherry.Input`
- prefer one principal/provider credential ref over principal/model refs when
  the same BYOK credential applies to all models for that provider
- do not collapse refs that represent genuinely different credentials or
  different enforcement semantics

Likely root-package optimization direction:

```text
route_shape(provider, model, tree)
principal_route_binding(principal, requested_model, route_shape_id, secret_ref_id, rate_id)
```

That split would let Cherry dedupe the static route shape while keeping secret
refs and rate policies in mutable binding metadata. For chain and split plans,
the same idea may require a credential overlay keyed by route leaf or child
position so shared trees can still carry per-principal BYOK refs without
duplicating the full route tree.

Focused provider-level BYOK benchmark, 2026-06-13:

```text
byok-provider-secret-always, 100k route entries:
  time: 532.3 ms/op
  alloc: 183.7 MB/op
  allocs: 401k/op
  blob: 6.2 MB raw, 1.6 MB zstd

byok-provider-secret-always, 1M route entries:
  time: 3.61 s/op
  alloc: 1.82 GB/op
  allocs: 4.0M/op
  blob: 62.0 MB raw, 16.2 MB zstd

byok-provider-secret-prefer, 100k route entries:
  time: 723.8 ms/op
  alloc: 340.2 MB/op
  allocs: 1.3M/op
  blob: 10.2 MB raw, 2.1 MB zstd

byok-provider-secret-prefer, 1M route entries:
  time: 30.0 s/op
  alloc: 3.35 GB/op
  allocs: 13.0M/op
  blob: 102.0 MB raw, 20.5 MB zstd
```

Raw output:

```text
/tmp/cherry-byok-provider-bench-20260613204349.txt
```

These provider-level results replace the earlier interpretation that the
per-model stress shape was the only large BYOK concern. Provider-level
normalization is still the right producer behavior, but it does not solve the
builder allocation problem by itself. The `PREFER` shape is the clearest
realistic driver for V2 route-shape/binding separation because it combines
per-principal provider credentials with fallback-chain trees.

After chunk 3, route shape and binding metadata are separated inside the pack.
The before numbers above are retained as the baseline that motivated the format
change.

### Route Shape / Binding Separation Plan

Goal: let Cherry dedupe route topology independently from per-principal mutable
metadata such as BYOK credential refs and rate policies.

Current target route identity:

```text
target(provider_id, model_id, secret_ref_id)
```

Desired split:

```text
route_shape(provider_id, model_id, tree)
principal_route_binding(scope, principal, requested_model, route_shape_id, credential_refs, rate_policy_id)
```

For `ALWAYS` provider BYOK:

```text
shape #7:
  target anthropic/claude-haiku

binding:
  principal slug:user-a, requested_model claude-haiku
  route_shape_id #7
  credential slot target = env://USER_A_ANTHROPIC
```

For `PREFER` provider BYOK:

```text
shape #12:
  chain retry_on=401,connect-failure,reset,5xx
    child 0: target anthropic/claude-haiku credential_slot=byok
    child 1: target anthropic/claude-haiku credential_slot=platform

binding:
  principal slug:user-a, requested_model claude-haiku
  route_shape_id #12
  credential slot byok = env://USER_A_ANTHROPIC
  credential slot platform = env://ANTHROPIC_PLATFORM_KEY
```

Implementation phases:

1. Add measurement-only benchmarks and profiles for realistic provider-level
   `ALWAYS` and `PREFER` BYOK. Status: benchmark cases added.
2. Format bump:
   - add route shape records that store provider/model topology and tree child
     relationships
   - add principal route binding records that point to shape IDs, rate IDs, and
     credential binding records
   - add credential slot records for target leaves or stable leaf ordinals
   - update `Open`, `validateOffsets`, manifest/version checks, round-trip tests,
     and inspector tests
3. Preserve hot-path APIs:
   - `ResolveLLMPlanIDs` should return the same effective provider/model/secret
     refs, but materialize them by applying binding credential slots to the
     shared route shape
   - `ResolveLLMIDs` can keep returning the first executable target with the
     effective secret ref
   - string-materializing diagnostics should remain stable
4. Build V2 overlay/shard support on top of the split:
   - static/base pack owns provider/model catalogs and reusable route shapes
   - mutable overlay owns scopes, principal route bindings, credential slots,
     rate policies, MCP profile bindings, and MCP credential refs
   - scope-sharded overlays can replace only affected workspaces when a key or
     secret-ref changes
5. Validation criteria:
   - provider-level `ALWAYS` and `PREFER` BYOK allocation drops materially at
     100k and 1M route entries
   - simple shared route and catalog benchmarks do not regress
   - route resolution outputs are byte-for-byte equivalent in tests for target,
     chain, split, BYOK `ALWAYS`, and BYOK `PREFER`
   - old immutable generations remain safe for in-flight requests during swaps

Risks and constraints:

- credential slot ordinals must be deterministic so inspector APIs are stable
- chain/split route shapes need a clear way to identify which target leaf a
  binding credential applies to
- route-shape dedupe must not collapse routes with different retry semantics,
  provider/model targets, child order, or split weights
- this is a binary format change once it affects persisted tables, so it should
  bump the internal pack version and update all validation paths

### Chunk 3: Split Route Shape From Credential Binding

Status: implemented.

Change:

- pack format version bumped from 5 to 6
- principal route records now include credential slot count and offset
- credential slot records store target-leaf ordinal and secret-ref SID
- target route shape keys now exclude actual secret refs
- route traversal applies principal binding credential slots by deterministic
  target ordinal
- `PREFER` chains can share topology even when the BYOK child and platform
  fallback child use the same provider/model with different secret refs
- hot-path and diagnostic APIs preserve effective secret-ref behavior

Focused benchmark command:

```sh
go test -run '^$' \
  -bench '^BenchmarkCherryPackRouteScale/byok-provider-secret-(always|prefer)/' \
  -benchmem \
  -benchtime=3s \
  -count=3 \
  ./...
```

Raw output:

```text
/tmp/cherry-byok-provider-after-shape-binding-20260613205732.txt
```

Representative p50 after results:

```text
byok-provider-secret-always, 100k route entries:
  time: 83.6 ms/op
  alloc: 76.4 MB/op
  allocs: 700k/op
  blob: 5.4 MB raw, 1.5 MB zstd

byok-provider-secret-always, 1M route entries:
  time: 1.29 s/op
  alloc: 732.8 MB/op
  allocs: 7.0M/op
  blob: 54.0 MB raw, 15.8 MB zstd

byok-provider-secret-prefer, 100k route entries:
  time: 101.1 ms/op
  alloc: 92.4 MB/op
  allocs: 1.1M/op
  blob: 5.4 MB raw, 1.6 MB zstd

byok-provider-secret-prefer, 1M route entries:
  time: 1.54 s/op
  alloc: 892.8 MB/op
  allocs: 11.0M/op
  blob: 54.0 MB raw, 15.8 MB zstd
```

Before -> after p50 deltas:

```text
byok-provider-secret-always, 1M route entries:
  time: 3.61 s/op -> 1.29 s/op
  alloc: 1.82 GB/op -> 732.8 MB/op
  blob: 62.0 MB raw -> 54.0 MB raw

byok-provider-secret-prefer, 1M route entries:
  time: 30.0 s/op -> 1.54 s/op
  alloc: 3.35 GB/op -> 892.8 MB/op
  blob: 102.0 MB raw -> 54.0 MB raw
```

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

### BYOK Provider Profile: 2026-06-13 After Shape/Binding Split

Status: measured.

Focused profile command requested `./...`, but Go rejects `-cpuprofile` with
multiple packages. The equivalent root-package command was used because the
benchmark lives in `github.com/dio/cherry`:

```sh
go test -run '^$' \
  -bench '^BenchmarkCherryPackRouteScale/byok-provider-secret-(always|prefer)/route_entries=1000000' \
  -benchmem \
  -benchtime=10s \
  -memprofile=/tmp/cherry-byok-provider.mem \
  -cpuprofile=/tmp/cherry-byok-provider.cpu \
  .
```

Benchmark sample from the profile run:

```text
byok-provider-secret-always, 1M route entries:
  time: 1.18 s/op
  alloc: 732.8 MB/op
  allocs: 7.0M/op
  blob: 54.0 MB raw, 15.8 MB zstd

byok-provider-secret-prefer, 1M route entries:
  time: 1.82 s/op
  alloc: 892.8 MB/op
  allocs: 11.0M/op
  blob: 54.0 MB raw, 15.8 MB zstd
```

Allocation object profile:

```text
strings.(*Builder).WriteString: 77.5% flat objects
appendRouteCredentialSlots:     52.2% cumulative objects
internRoute:                    38.7% cumulative objects
routeKey/writeRouteKey:         78.1% cumulative objects
benchmarkRouteInput:             9.0% cumulative objects
```

Allocation space profile:

```text
Build:                      89.1% cumulative bytes
writeScopes:                37.3% cumulative bytes
routeKey/writeRouteKey:     23.6% cumulative bytes
appendRouteCredentialSlots: 20.0% cumulative bytes
builder.stringID:            7.3% flat bytes
benchmarkRouteInput:         8.0% cumulative bytes
```

CPU profile:

```text
Build:                      63.2% cumulative samples
GC/runtime work:            dominant flat samples
writeScopes:                23.8% cumulative samples
routeKey/writeRouteKey:     15.6% cumulative samples
appendRouteCredentialSlots:  8.9% cumulative samples
sort.Slice:                  8.7% cumulative samples
```

Interpretation:

- benchmark input construction is visible, especially provider secret-ref and
  model/provider ID strings, but it is not the dominant allocation source
- the clearest remaining builder issue is route-key construction
- `internRoute` still needs route keys for shape dedupe, but
  `appendRouteCredentialSlots` rebuilds target route keys only to validate that
  the shape exists after `internRoute` already succeeded
- removing that redundant credential-slot route-key validation is the next
  focused optimization to test

### Chunk 4: Remove Redundant Credential-Slot Route-Key Validation

Status: implemented.

Change:

- `routeCredentialSlots` no longer receives `routeIDs`.
- `appendRouteCredentialSlots` no longer rebuilds `routeKey(target)` for each
  credential-bearing target leaf.
- route existence validation remains in `internRoute`, which is called
  immediately before credential-slot extraction for each principal/requested
  model route.

Focused benchmark command:

```sh
go test -run '^$' \
  -bench '^BenchmarkCherryPackRouteScale/byok-provider-secret-(always|prefer)/route_entries=1000000' \
  -benchmem \
  -benchtime=3s \
  -count=3 \
  .
```

Raw output:

```text
/tmp/cherry-byok-provider-after-credential-slot-key-removal-20260613211700.txt
```

Representative p50 before -> after:

```text
byok-provider-secret-always, 1M route entries:
  time:   1.29 s/op -> 0.953 s/op
  alloc:  732.8 MB/op -> 652.8 MB/op
  allocs: 7.0M/op -> 4.0M/op
  blob:   54.0 MB raw -> 54.0 MB raw

byok-provider-secret-prefer, 1M route entries:
  time:   1.54 s/op -> 1.51 s/op
  alloc:  892.8 MB/op -> 732.8 MB/op
  allocs: 11.0M/op -> 5.0M/op
  blob:   54.0 MB raw -> 54.0 MB raw
```

Interpretation:

- the change removes the largest allocation-count source identified in the
  profile without changing the persisted format
- `PREFER` allocation count is now much closer to `ALWAYS`; the remaining gap is
  mostly from the extra chain/fallback route traversal and benchmark route-plan
  construction
- remaining allocation space is now more likely to be dominated by structural
  temporary storage: compiled principal entries, credential slot slices, string
  interning for secret refs, table buffer growth, and sorting copies

### BYOK Provider Profile: 2026-06-13 After Chunk 4

Status: measured.

Focused profile command:

```sh
go test -run '^$' \
  -bench '^BenchmarkCherryPackRouteScale/byok-provider-secret-(always|prefer)/route_entries=1000000' \
  -benchmem \
  -benchtime=10s \
  -memprofile=/tmp/cherry-byok-provider-after-chunk4.mem \
  -cpuprofile=/tmp/cherry-byok-provider-after-chunk4.cpu \
  .
```

Benchmark sample from the profile run:

```text
byok-provider-secret-always, 1M route entries:
  time: 0.881 s/op
  alloc: 652.8 MB/op
  allocs: 4.0M/op
  blob: 54.0 MB raw, 15.8 MB zstd

byok-provider-secret-prefer, 1M route entries:
  time: 1.34 s/op
  alloc: 732.8 MB/op
  allocs: 5.0M/op
  blob: 54.0 MB raw, 15.8 MB zstd
```

Allocation object profile:

```text
Build:                       84.6% cumulative objects
routeKey/writeRouteKey:      63.3% cumulative objects
internRoute:                 63.3% cumulative objects
appendRouteCredentialSlots:  21.3% flat objects
benchmarkRouteInput:         15.4% cumulative objects
```

Allocation space profile:

```text
Build:                  88.0% cumulative bytes
writeScopes:            41.5% cumulative bytes
bytes.growSlice:        36.1% flat bytes
routeKey/writeRouteKey: 15.2% cumulative bytes
internRoute:            15.2% cumulative bytes
builder.stringID:        8.2% flat bytes
```

Line-level allocation space:

```text
Build compiled principal entry slice:      2.62 GB flat
writeScopes copied principal entry slice:  2.62 GB flat
writeScopes principalData writes/growth:   1.68 GB cumulative at putU64
writeScopes credentialData writes/growth:  432 MB cumulative at credential putU32
```

Interpretation:

- route-key construction remains the largest allocation-count source, but a
  route-key redesign is structurally broader than the next available fix
- `writeScopes` copies all compiled principal entries before sorting even though
  compiled scopes are temporary and are not used after table writing
- scope-local table buffers also grow incrementally despite known fixed record
  widths
- the next focused optimization is to sort compiled scope entries in place and
  pre-grow principal, credential, and MCP path table buffers

### Chunk 5: Sort Scope Writer Tables In Place

Status: implemented.

Change:

- `writeScopes` sorts compiled principal route entries in place instead of
  allocating a copied slice for each scope.
- `writeScopes` sorts compiled MCP profile entries in place for the same reason.
- principal, credential, and MCP path table buffers are pre-grown from known
  fixed record counts.
- this preserves the binary format and public API; compiled scope data is
  temporary and is not used after table emission.

Focused benchmark command:

```sh
go test -run '^$' \
  -bench '^BenchmarkCherryPackRouteScale/byok-provider-secret-(always|prefer)/route_entries=1000000' \
  -benchmem \
  -benchtime=3s \
  -count=3 \
  .
```

Raw output:

```text
/tmp/cherry-byok-provider-after-scope-writer-20260613213000.txt
```

Representative p50 before -> after:

```text
byok-provider-secret-always, 1M route entries:
  time:   0.953 s/op -> 0.825 s/op
  alloc:  652.8 MB/op -> 504.9 MB/op
  allocs: 4.0M/op -> 4.0M/op
  blob:   54.0 MB raw -> 54.0 MB raw

byok-provider-secret-prefer, 1M route entries:
  time:   1.51 s/op -> 1.20 s/op
  alloc:  732.8 MB/op -> 584.9 MB/op
  allocs: 5.0M/op -> 5.0M/op
  blob:   54.0 MB raw -> 54.0 MB raw
```

Interpretation:

- sorting in place removed the largest avoidable allocation-space source from
  `writeScopes`
- exact table buffer growth reduced bytes allocated without changing allocation
  count materially
- the next remaining allocation-count target is route-key construction in
  `internRoute`, but that needs a more careful route-shape key redesign and is
  not part of this chunk

### BYOK Provider Profile: 2026-06-13 After Chunk 5

Status: measured.

Focused profile command:

```sh
go test -run '^$' \
  -bench '^BenchmarkCherryPackRouteScale/byok-provider-secret-(always|prefer)/route_entries=1000000' \
  -benchmem \
  -benchtime=10s \
  -memprofile=/tmp/cherry-byok-provider-after-chunk5.mem \
  -cpuprofile=/tmp/cherry-byok-provider-after-chunk5.cpu \
  .
```

Benchmark sample from the profile run:

```text
byok-provider-secret-always, 1M route entries:
  time: 0.818 s/op
  alloc: 504.9 MB/op
  allocs: 4.0M/op
  blob: 54.0 MB raw, 15.8 MB zstd

byok-provider-secret-prefer, 1M route entries:
  time: 1.04 s/op
  alloc: 584.9 MB/op
  allocs: 5.0M/op
  blob: 54.0 MB raw, 15.8 MB zstd
```

Allocation object profile:

```text
Build:                       86.1% cumulative objects
routeKey/writeRouteKey:      62.8% cumulative objects
internRoute:                 62.8% cumulative objects
appendRouteCredentialSlots:  23.2% flat objects
benchmarkRouteInput:         13.9% cumulative objects
```

Allocation space profile:

```text
Build:                  85.9% cumulative bytes
bytes.growSlice:        38.0% flat bytes
writeScopes:            28.2% cumulative bytes
routeKey/writeRouteKey: 18.6% cumulative bytes
internRoute:            18.6% cumulative bytes
builder.stringID:        9.9% flat bytes
```

CPU profile:

```text
Build:                  68.1% cumulative samples
writeScopes:            25.7% cumulative samples
internRoute:            12.7% cumulative samples
routeKey/writeRouteKey: 12.4% cumulative samples
sort.Slice:             11.2% cumulative samples
```

Interpretation:

- parallelizing the current shape would reduce some wall time but retain the
  dominant allocation pattern and add synchronization or merge complexity around
  route dedupe and string interning
- the smallest route-key fix is target-route dedupe by numeric provider/model
  IDs, because target shape identity is fixed-width after provider/model
  validation and does not require recursive string construction
- chain and split route keys can stay string-based for this chunk; a full
  variable-length structural key needs a separate design

### Chunk 6: Numeric Target And Short Chain Route Keys

Status: implemented.

Change:

- target route shapes now dedupe with a compact numeric key:
  `providerID + modelID`
- short chain route shapes with up to three children now dedupe with a compact
  numeric key:
  `retry SID + timeout + child route IDs`
- longer chain and split routes keep the existing string route-key path
- this preserves the persisted format and public API; it only changes temporary
  builder dedupe keys

Focused benchmark command:

```sh
go test -run '^$' \
  -bench '^BenchmarkCherryPackRouteScale/byok-provider-secret-(always|prefer)/route_entries=1000000' \
  -benchmem \
  -benchtime=3s \
  -count=3 \
  .
```

Raw output:

```text
/tmp/cherry-byok-provider-after-target-chain-interner-20260613214500.txt
```

Representative p50 before -> after:

```text
byok-provider-secret-always, 1M route entries:
  time:   0.825 s/op -> 0.726 s/op
  alloc:  504.9 MB/op -> 424.9 MB/op
  allocs: 4.0M/op -> 1.0M/op
  blob:   54.0 MB raw -> 54.0 MB raw

byok-provider-secret-prefer, 1M route entries:
  time:   1.20 s/op -> 1.06 s/op
  alloc:  584.9 MB/op -> 424.9 MB/op
  allocs: 5.0M/op -> 1.0M/op
  blob:   54.0 MB raw -> 54.0 MB raw
```

Interpretation:

- target route keys were the dominant allocation-count source for `ALWAYS`
- recursive chain route keys kept `PREFER` expensive after the target-only
  change; using child route IDs for short chain identity fixes that realistic
  provider-BYOK fallback shape
- parallelization is still secondary: the serial builder now avoids the largest
  route-key allocation pattern, and concurrency would need deterministic merge
  rules for route IDs, string IDs, and scope-local indexes

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

### Benchmark Run: 2026-06-13 Complete Matrix

Status: completed.

Command:

```sh
go test -run '^$' \
  -bench '^BenchmarkCherryPack(CatalogScale|RouteScale|MCPProfileScale)$' \
  -benchmem \
  -benchtime=3s \
  -count=3 \
  ./...
```

Environment:

```text
Go:          go1.26.2
GOOS/GOARCH: darwin/arm64
CPU:         Apple M1
GOMAXPROCS: 8
Raw output:  /tmp/cherry-pack-bench-complete-20260613202120.txt
Elapsed:     root package 946.810s
```

Representative p50 results from the three samples:

```text
catalog, 500 providers x 5k models:
  time: 19.7 ms/op
  alloc: 10.9 MB/op
  allocs: 25.3k/op
  blob: 626.7 KB raw, 219.1 KB zstd

simple shared target routes, 1M route entries:
  time: 1.27 s/op
  alloc: 357.2 MB/op
  allocs: 3.0M/op
  blob: 26.0 MB raw, 9.8 MB zstd

unique rate policies, 1M route entries:
  time: 2.14 s/op
  alloc: 378.6 MB/op
  allocs: 3.0M/op
  blob: 27.6 MB raw, 10.2 MB zstd

unique BYOK target secret refs, 100k route entries:
  time: 280.0 ms/op
  alloc: 197.9 MB/op
  allocs: 391k/op
  blob: 7.7 MB raw, 2.0 MB zstd

unique BYOK target secret refs, 1M route entries:
  time: 3.44 s/op
  alloc: 2.0 GB/op
  allocs: 4.0M/op
  blob: 77.9 MB raw, 20.0 MB zstd

MCP shared toolsets, 10k profiles x 10 tools:
  time: 104.7 ms/op
  alloc: 58.4 MB/op
  allocs: 140k/op
  blob: 320.7 KB raw, 141.1 KB zstd

MCP unique toolsets, 10k profiles x 10 tools:
  time: 205.1 ms/op
  alloc: 99.8 MB/op
  allocs: 131k/op
  blob: 6.1 MB raw, 803.3 KB zstd

MCP unique secret refs, 10k profiles x 10 tools:
  time: 85.5 ms/op
  alloc: 74.4 MB/op
  allocs: 139k/op
  blob: 2.7 MB raw, 349.1 KB zstd
```

Interpretation:

- catalog rebuild cost remains secondary compared with route/profile churn
- unique BYOK route entries remain the largest allocation concern by a wide
  margin
- unique rate policies add modest cost relative to simple target routes, but do
  not resemble the BYOK secret-ref allocation profile
- MCP profile churn is meaningful at 10k-profile scale, especially unique
  toolsets, but still below the 1M BYOK-heavy LLM route case
- this supports the V2 direction: a key or secret-ref event should trigger an
  immediate immutable snapshot for the affected mutable surface, and future
  overlay or scope sharding should avoid rebuilding unrelated catalog and route
  data

## Current Status

- Benchmarks added in `pack_bench_test.go`.
- First no-format-change allocation reduction implemented in `pack.go`.
- Scope-local lookup hash precomputation implemented in `pack.go`.
- V2 snapshot planning started with `SnapshotPolicy`: callers can keep a
  periodic cadence for static catalog churn while requesting an immediate
  immutable snapshot for normalized mutable changes such as principal binding,
  route, secret-ref, rate policy, and MCP profile/tool binding changes.
- Route shape / credential binding separation implemented in pack format version
  6.
- Complete `-benchtime=3s -count=3` benchmark matrix captured on 2026-06-13.
- `go test ./...` passes.
- `make format` passes.
- `make lint` passes.
- Full `-benchtime=3s -count=30` characterization is still pending.
- CPU and allocation profiles for the remaining BYOK-heavy case are still
  pending.
