# Cherry REPL Library Design

## Status

PoC implemented in `github.com/dio/cherry/repl` and integrated with Orange's
`examples/yamlclient` and `examples/yamlserver`.

## Context

Cherry currently has an interactive REPL in `example/main.go`. It is useful for
debugging a packed bundle, but it is not importable by another Go program. That
limits two important workflows:

- Embedding the same rule-inspection commands in a client, for example
  `/Users/dio/src/dio/orange/examples/yamlclient/main.go`, after the client
  downloads a Cherry bundle.
- Running the same command surface against an RPC service when the bundle remains
  server-side, which is useful for quick production debugging without copying the
  bundle locally.

The goal is to make the REPL an importable library while preserving Cherry's core
boundary: the root package still only builds, opens, and queries normalized
bundles. Product-specific auth, tenancy, source schemas, and bundle delivery stay
outside `github.com/dio/cherry`.

## Goals

- Provide an importable REPL package that can be embedded by CLIs and small debug
  clients.
- Keep the command language consistent with the existing example REPL:
  `summary`, `scopes`, `use`, `llm`, `mcp`, `inspect`, `reload`, `help`, `quit`.
- Support a local backend backed by `cherry.OpenedBundle`.
- Support a remote backend backed by RPC calls to a server that owns the active
  bundle.
- Keep terminal dependencies such as `readline` outside the root `cherry`
  package.
- Make command execution scriptable so tests and CI can exercise the REPL without
  an interactive terminal.
- Make production debugging safe by default: explicit authorization, auditability,
  redaction controls, and no secret material in responses.

## Non-Goals

- Do not move fixture loaders, transformer code, project fanout, key
  verification, or source schema assumptions into the root package.
- Do not add mutable caches, counters, or request state to `cherry.Reader`.
- Do not make Cherry itself perform RPC, authentication, authorization, or bundle
  download.
- Do not introduce JSON snapshots as a parallel runtime contract. Local mode
  still opens the zstd bundle with `cherry.OpenBundleZstd`.
- Do not require every consumer to use the terminal UI. The parser/executor
  should be usable directly.

## Proposed Package Layout

Add a sibling package under the module:

```text
repl/
  doc.go
  session.go
  command.go
  render.go
  local.go
  remote.go
  terminal.go
  session_test.go
  render_test.go
```

This keeps the root package small and importable. The existing example command
can become a thin wrapper around `github.com/dio/cherry/repl`, while
`example/source` and `example/transform` remain example-only producer code.

The root package continues to expose the pack and bundle APIs. The REPL package
is a consumer/diagnostics layer built on top of those APIs.

## Core API

The REPL should split command execution from terminal I/O:

```go
package repl

type Session struct {
    // unexported state:
    // active scope, backend, renderer, reload hook, redaction policy
}

type Config struct {
    Backend      Backend
    DefaultScope string
    Context      Context
    Reload       func(context.Context) (Backend, Context, error)
}

type Context struct {
    Lane             string
    SnapshotVersion  uint64
    SnapshotChecksum string
    Source           string
}

func NewSession(cfg Config) (*Session, error)

func (s *Session) Execute(ctx context.Context, line string) (Result, error)
func (s *Session) ActiveScope() string
func (s *Session) SetScope(ctx context.Context, scope string) error
```

`Execute` is the key embedding point. A CLI can call it for each user-entered
line, and tests can call it directly. The initial PoC keeps terminal loops in
embedding code rather than shipping a terminal runner in Cherry.

`Result` should carry structured data and rendered text:

```go
type Result struct {
    Continue bool
    Text     string
    Scope    string
    Lane     string
    Data     any
}
```

`Continue` is `false` for `quit` and `exit`. `Data` lets callers build their own
UI later without reparsing text. `Text` preserves the current terminal workflow.

## Backend Interface

Commands should not know whether data is local or remote. They should call a
small backend interface shaped around Cherry's diagnostics surface:

```go
type Backend interface {
    Metadata(ctx context.Context) (cherry.BundleMetadata, error)
    Scopes(ctx context.Context) ([]string, error)

    LLMPrincipals(ctx context.Context, scope string) ([]cherry.PrincipalInfo, error)
    ResolveLLMPlan(ctx context.Context, scope string, principalSlug string, modelID string) (cherry.LLMPlan, bool, error)

    Providers(ctx context.Context) ([]cherry.ProviderInfo, error)
    Models(ctx context.Context) ([]cherry.ModelInfo, error)
    ResolveModel(ctx context.Context, modelID string) (cherry.ModelInfo, bool, error)
    ModelCapability(ctx context.Context, modelID string, capability string) (bool, error)
    V1ModelsJSON(ctx context.Context, providerID string) ([]byte, error)

    MCPPaths(ctx context.Context, scope string) ([]cherry.MCPPath, error)
    ResolveMCP(ctx context.Context, scope string, path string) (cherry.MCPResult, bool, error)
    ResolveMCPInitialize(ctx context.Context, scope string, path string) (cherry.MCPInitializeResult, bool, error)
    ResolveMCPTool(ctx context.Context, scope string, path string, exposedTool string) (cherry.MCPTool, bool, error)

    PrincipalRoutes(ctx context.Context, scope string) ([]cherry.PrincipalRoute, error)
}
```

The local implementation is a thin adapter over `cherry.OpenedBundle`:

```go
func NewLocalBackend(opened cherry.OpenedBundle) Backend
func OpenLocalBundle(path string) (Backend, error)
```

The remote implementation is a client adapter over a product-defined RPC
contract:

```go
type RemoteClient interface {
    Do(ctx context.Context, req Request) (Response, error)
}

func NewRemoteBackend(client RemoteClient) Backend
```

The REPL package should define neutral request/response structs, but not choose
HTTP, gRPC, connect-go, MCP, or any product auth mechanism. Orange can adapt its
own transport to `RemoteClient`.

## Local Embedded Flow

For a client that downloads the bundle, such as
`/Users/dio/src/dio/orange/examples/yamlclient/main.go`, the flow is:

```go
payload, err := downloadBundle(ctx)
if err != nil {
    return err
}

opened, err := cherry.OpenBundleZstd(payload)
if err != nil {
    return err
}

cfg := repl.Config{
    Backend:      repl.NewLocalBackend(opened),
    DefaultScope: defaultScope(opened.Metadata.Scopes),
    Context: repl.Context{
        Lane:             fetched.Metadata.Lane,
        SnapshotVersion:  fetched.Version,
        SnapshotChecksum: hex.EncodeToString(fetched.Checksum),
        Source:           "yamlclient",
    },
}

session, err := repl.NewSession(cfg)
if err != nil {
    return err
}
result, err := session.Execute(ctx, "summary")
```

In this mode, commands are fully client-side. Query semantics match the
enforcement point because both use the same `cherry.Reader` APIs.

The `reload` command can be enabled by providing a `Reload` function that
downloads the latest bundle and returns a new backend plus updated context. If
`Reload` is nil, the command returns a clear "reload is not configured" message.

## Lane And Scope

The REPL absorbs both terms without making Cherry understand Orange tenancy:

- `Context.Lane` is embedding metadata. In Orange it is the snapshot-manager key
  that selects the active snapshot stream.
- `Session.ActiveScope` is the Cherry enforcement scope inside the opened
  bundle. It is the value passed to `Reader.ResolveLLM*`, `Reader.ResolveMCP*`,
  and inspector APIs.

The stateless HTTP PoC sends the current scope in each request and returns the
resulting scope in each response. A terminal or UI can keep that scope
client-side between calls.

## Remote RPC Flow

For production debugging, the client may not receive bundle bytes. Instead, it
connects to a server that owns the current bundle generation:

```text
operator terminal
  -> orange debug client
  -> authorized RPC
  -> server active generation
  -> cherry.Reader inspector/query APIs
```

The server exposes only diagnostic methods equivalent to the `Backend`
interface. It should not expose arbitrary bundle reads unless that is explicitly
authorized by the product.

Important remote behavior:

- Each request includes an optional generation token returned by `metadata`.
  The server can reject stale generation tokens or include the current generation
  in every response.
- Each request includes scope explicitly after `use <scope>`. The server should
  independently authorize the operator for that scope.
- The server should execute the same root `Reader` methods as local mode:
  `ResolveLLMPlan`, `ResolveMCP`, `ResolveMCPInitialize`,
  `ResolveMCPToolIDs`, `PrincipalRoutes`, `MCPPaths`, and catalog inspection.
- The server should return secret refs only, never secret material. If the
  product considers secret refs sensitive, the server can redact them before
  returning.
- The server should audit command kind, actor, scope, generation, target
  principal/path/model/tool, success/reject, and latency.

## Command Compatibility

The first library version should preserve the current command language:

```text
summary
scopes
use <scope>
llm [scope] <principal-slug> <model>
llm principals [scope]
llm providers
llm models [--provider=name]
llm model <model> [--provider=name]
llm capability <model> <capability>
mcp paths [scope] [--tools]
mcp initialize [scope] <path|profile=name|server=name>
mcp list [scope] <path|profile=name|server=name>
mcp call [scope] <path|profile=name|server=name> <tool>
mcp [scope] <path> [tool]
inspect metadata
inspect principals
inspect mcp
inspect all
reload
help
quit
```

Parsing should be separate from execution:

```go
type Command struct {
    Name string
    Args []string
}

func Parse(line string) (Command, error)
```

The initial parser can keep the existing `strings.Fields` behavior. A later
version can add quoting if users need spaces in values.

## Rendering

The current PoC returns rendered human text in `Result.Text` and keeps a
structured value in `Result.Data` where practical. The command text intentionally
matches the example REPL style: plain text for summaries and resolutions, YAML
for inspector lists, and pretty JSON for `/v1/models` projections.

Future UI-specific renderers can be added after Orange and Plum integrations
show which outputs need a stable structured contract.

## Security And Operations

Production remote REPL access should be treated as a privileged diagnostic
surface.

Required server-side controls:

- Authenticate the operator before any REPL RPC.
- Authorize every requested scope and command class.
- Rate-limit and timeout diagnostic requests.
- Redact or omit fields according to product policy. Cherry stores secret refs,
  not secret material, but refs may still reveal deployment details.
- Audit all commands and rejected lookups.
- Avoid exposing raw bundle download unless the operator is explicitly allowed
  to receive the bundle.

Recommended client-side controls:

- Show the connected environment and active scope in prompt metadata when the
  embedding CLI supports it.
- Default to redacted secret refs.
- Require an explicit flag to print unredacted secret refs, if the server allows
  that at all.

## Migration Plan

1. Add `repl` package with `Session`, `Backend`, local backend, parser, renderer,
   and terminal runner.
2. Move command logic out of `example/main.go` into `repl`, preserving the
   existing output where practical.
3. Replace `runREPL` in `example/main.go` with a wrapper that opens a local
   bundle, creates a `repl.Session`, and feeds terminal lines to `Execute`.
4. Add scriptable tests for the command executor using a local bundle built from
   focused test input.
5. Add an in-memory fake remote client test to prove the same commands work
   against the remote backend.
6. Add Orange integration in `/Users/dio/src/dio/orange/examples/yamlclient`:
   first local downloaded-bundle mode, then remote RPC mode once the server
   diagnostic endpoint exists.

## Testing Plan

Unit tests:

- `Parse` accepts existing command forms and rejects malformed forms with stable
  usage messages.
- `Session.Execute` handles `use`, missing active scope, unknown scope, `quit`,
  and `reload`.
- Local backend returns the same results as direct `cherry.Reader` calls.
- Rendered output is deterministic for YAML and pretty JSON.

Integration tests:

- Build a real bundle with `cherry.BuildWithManifest`, wrap it with
  `NewBundle`, encode/decode with zstd, and run scripted commands through
  `Session.Execute`.
- Verify LLM route plans, model catalog output, MCP initialize, MCP list, MCP
  call, and inspector dumps.
- Verify remote backend command behavior through a fake `RemoteClient`.

Smoke checks:

```sh
go test ./...
go run ./example pack project project1 example/source/testdata/example_fixture.yaml /tmp/project1.cherry.zst
printf 'use workspace1\nllm slug:project1 claude-haiku-4-5\nmcp initialize profile-kiwi-and-github\nquit\n' \
  | go run ./example repl /tmp/project1.cherry.zst
```

## Open Questions

- Should the REPL package live at `github.com/dio/cherry/repl` or
  `github.com/dio/cherry/cherryrepl`? `repl` is shorter and idiomatic inside this
  module, but `cherryrepl` avoids name collisions in consumers.
- Should remote mode expose one generic `Execute` RPC or typed RPC methods?
  Typed methods are easier to authorize and audit. Generic `Execute` is easier to
  wire but couples the server to command text.
- Should secret refs be redacted in local mode by default? For production-like
  clients, yes. For local fixture debugging, unredacted refs are useful.
- Should command output stabilize as a documented contract? The safer contract is
  structured `Result.Data`; terminal text can remain human-oriented.

## Recommendation

Build the library around typed command execution and a typed `Backend`
interface. Keep local mode as the reference implementation over
`cherry.OpenedBundle`. Let remote mode adapt product-owned RPC to the same
backend interface, with typed methods instead of a generic "run this command on
the server" endpoint.

This gives Orange a straightforward embedded REPL for downloaded bundles now,
while leaving room for an authorized production debug RPC without weakening
Cherry's package boundary.
