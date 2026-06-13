# Cherry REPL

Package `github.com/dio/cherry/repl` provides an embeddable diagnostic command
executor for opened Cherry bundles.

It is a consumer-side layer over the root Cherry reader APIs. It does not build
bundles, fetch bundles, authenticate operators, authorize scopes, choose lanes,
or choose a transport. Embedding applications provide a `Backend`, optional
runtime `Context`, and whatever terminal, HTTP, or RPC loop they want.

## Boundary

Cherry's root package still owns only pack and bundle primitives. The REPL
package is for diagnostics and operator tooling after a bundle is already
opened.

Keep these outside this package:

- source fixture schemas
- tenancy joins and ownership checks
- key or JWT verification
- Orange lane resolution
- bundle download or delivery
- RPC transport and production authorization
- secret material resolution

The REPL may display secret refs because they are stored in Cherry bundles for
diagnostics and enforcement setup. It must never resolve refs to secret bytes.

## Core API

```go
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

type Result struct {
    Continue bool
    Text     string
    Scope    string
    Lane     string
    Data     any
}

func NewSession(cfg Config) (*Session, error)
func (s *Session) Execute(ctx context.Context, line string) (Result, error)
func (s *Session) ActiveScope() string
func (s *Session) SetScope(ctx context.Context, scope string) error
```

`Execute` is the embedding point. A terminal can call it for each user-entered
line, an HTTP handler can call it once per request, and tests can call it
directly.

`Result.Text` is the human-readable output. `Result.Data` carries the structured
value when the command naturally has one. `Result.Continue` is `false` for
`quit` and `exit`.

## Backend

Commands call the `Backend` interface instead of depending directly on a local
reader. That lets the same command surface run over either:

- a local `cherry.OpenedBundle`, through `NewLocalBackend`
- a product-owned remote diagnostic API that adapts to `Backend`

The local backend is the reference implementation:

```go
opened, err := cherry.OpenBundleZstd(payload)
if err != nil {
    return err
}

session, err := repl.NewSession(repl.Config{
    Backend:      repl.NewLocalBackend(opened),
    DefaultScope: defaultScope(opened.Metadata.Scopes),
    Context: repl.Context{
        Lane:             "default",
        SnapshotVersion:  version,
        SnapshotChecksum: checksum,
        Source:           "yamlclient",
    },
})
if err != nil {
    return err
}

result, err := session.Execute(ctx, "summary")
```

Use `OpenLocalBundle(path)` for local files.

## Lane And Scope

The REPL intentionally keeps embedding metadata separate from Cherry query
state:

- `Context.Lane` is embedding metadata. In Orange, it is the snapshot-manager key
  that selects the active snapshot stream.
- `Session.ActiveScope()` is the Cherry enforcement scope inside the opened
  bundle. It is passed to `Reader.ResolveLLM*`, `Reader.ResolveMCP*`, and
  inspector APIs.

HTTP embeddings can stay stateless by sending the current scope in each request
and returning `Result.Scope` to the caller. Terminal embeddings can keep the
scope in a long-lived `Session` and let users run `use <scope>`.

## Commands

The command language matches the development REPL used by the Cherry example:

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

`reload` only works when `Config.Reload` is set. The reload hook returns a new
backend plus updated context; `Session` keeps the current scope if that scope is
still present in the new bundle.

## Orange PoC

Orange integrates this package in two development examples:

- `examples/yamlclient --repl` downloads a snapshot through Orange's fetch API,
  opens the bundle client-side, and runs an interactive REPL over that local
  reader.
- `examples/yamlserver /debug/repl` opens the current server-side snapshot for
  the development lane and executes one stateless REPL command per request.

Those examples are intentionally not a production security model. Production
remote REPL access must authenticate the operator, authorize the requested lane
and Cherry scope, rate-limit diagnostic requests, redact fields according to
product policy, and audit command kind, actor, scope, generation, target, result,
and latency.
