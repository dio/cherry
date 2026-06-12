# AGENTS.md

This repository is `github.com/dio/cherry`. Cherry is a Go library for building,
delivering, opening, and querying compact enforcement bundles.

## Core Boundary

Cherry starts after an external system has already normalized and selected data.
Do not move these responsibilities into the root package:

- tenancy joins
- ownership checks
- key or JWT verification
- project/workspace reachability
- rule authoring or rule merge from product objects
- source database schema assumptions
- secret material resolution

The root package accepts `cherry.Input` and writes a compact immutable pack. The
enforcement point opens that pack and queries it with verified values such as:

```text
scope + principal_slug + requested_model
scope + mcp_path + exposed_tool
```

The external system owns this pipeline before Cherry:

```text
source records -> transformer -> normalized cherry.Input -> bundle delivery
```

## Package Layout

Keep the root small and importable:

```text
go.mod
go.sum
README.md
AGENTS.md
pack.go
pack_bundle.go
pack_test.go
pack_bench_test.go
example/
  README.md
  main.go
  source/
  transform/
.agents/
  skills/
```

Root package name is `cherry`, even though the main binary format file is named
`pack.go`. Use "pack" as the artifact/format noun and "Cherry" as the library
name.

## Root Package Rules

- Keep root code free of example fixture types and external product schemas.
- Keep persisted delivery format bundle-based; do not add JSON snapshots as a
  parallel runtime contract.
- Preserve the hot-path ID-returning APIs:
  - `ResolveLLMIDs`
  - `ResolveMCPIDs`
  - `ResolveMCPToolIDs`
- Keep string-materializing APIs and inspector APIs available for diagnostics:
  - `ResolveLLM`
  - `ResolveMCP`
  - `ScopeIDs`
  - `PrincipalRoutes`
  - `MCPPaths`
- Store secret refs only. Never store secret material in the pack.
- Treat `Reader` as an immutable view over its blob. Do not add mutable request
  counters or runtime caches inside it.
- If adding a hot cache, keep it outside `Reader` and clear it on generation swap.

## Binary Format Changes

When changing `pack.go` layout constants, section order, or record fields:

- update header comments and godoc
- update validation in `Open` and `validateOffsets`
- update pack round-trip tests
- update inspector tests when records become visible through inspector APIs
- consider whether `BundleFormatVersion` or the internal pack version must change

Use deterministic ordering for emitted tables. Query correctness should not depend
on Go map iteration order.

## Example Directory

The `example` directory may simulate an external system. It can contain fixture
schemas, fanout logic, CLI commands, and walkthrough docs. Keep that code out of
the root package unless it is a generic pack/query primitive.

The example transformer is allowed to:

- select workspace or project scope
- fan a project selection out to workspace scopes
- resolve keys/users/tags/profiles from fixture data
- produce `cherry.Input`

The root package should only see normalized `cherry.Input`.

## Documentation

- Root `README.md` documents the library and enforcement-point consumption.
- `example/README.md` documents example fixtures, walkthroughs, and CLI commands.
- Do not put example-specific command walkthroughs back into the root README.
- Keep exported root symbols documented with godoc comments.

## Testing

Run before finishing changes:

```sh
go test ./...
```

For behavior smoke checks:

```sh
go run ./example
go run ./example pack project project1 example/source/testdata/example_fixture.yaml /tmp/project1.cherry.zst
printf 'use workspace1\nllm slug:project1 claude-haiku-4-5\nmcp profile-dev-tools github__list-repos\nquit\n' \
  | go run ./example repl /tmp/project1.cherry.zst
```

For performance checks:

```sh
go test -bench=. ./...
go run ./example stress-pack 100000 10000 2
```

Use focused tests for:

- manifest/checksum/version validation
- LLM ID resolution
- MCP tool ID resolution
- inspector enumeration
- project fanout and workspace isolation in the example transformer
- auth/secret-ref behavior for MCP profile bindings

## Local Agent Skill

Use `.agents/skills/integrate-cherry` when asked to connect Cherry to producer, transformer, or enforcement-point consumer code. That skill describes how to keep
source/transformer logic outside the library while producing `cherry.Input`, and
how to load/query delivered bundles in an EP.
