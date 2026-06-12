# Cherry Example

This example shows the full path from source data to a loaded enforcement bundle.
The source and transformer packages stand in for code that a real external system
would own.

## Files

```text
example/
  main.go
  source/
    source.go
    testdata/example_fixture.yaml
  transform/
    transform.go
```

`source` loads an editable fixture containing orgs, projects, workspaces, users,
keys, tags, LLM models/providers, MCP servers, and MCP profiles.

`transform` applies the external-system decisions:

- workspace and project scope selection
- project-to-workspace fanout
- key reachability
- user and key tag inheritance
- rule precedence
- MCP profile and auth selection
- conversion into `cherry.Input`

The root `cherry` package receives only normalized rows.

## Walkthrough

Print the fixture tree:

```sh
go run ./example
```

You should see `project1` with `workspace1` and `workspace2`, and `project2` with
`workspace3`. The project-level key for `project1` appears under both project1
workspaces and not under `workspace3`.

Pack one workspace:

```sh
go run ./example pack workspace workspace1 example/source/testdata/example_fixture.yaml /tmp/workspace1.cherry.zst
```

Pack one project:

```sh
go run ./example pack project project1 example/source/testdata/example_fixture.yaml /tmp/project1.cherry.zst
```

The project command writes one bundle that contains the workspace scopes selected
by the transformer:

```text
workspace1,workspace2
```

Start the enforcement REPL:

```sh
go run ./example repl /tmp/project1.cherry.zst
```

Select a scope and inspect the loaded bundle:

```text
use workspace1
summary
inspect metadata
inspect principals
inspect mcp
```

Resolve an LLM request:

```text
llm slug:project1 claude-haiku-4-5
```

The `marketing` tag in the fixture routes Anthropic requests to OpenAI for the
tagged principal and carries a tag-specific rate limit.

Resolve an MCP tool:

```text
mcp profile-dev-tools github__list-repos
```

The result includes the upstream server, upstream tool name, auth type, and secret
ref selected by the profile.

The same commands can be scripted:

```sh
printf 'use workspace1\nllm slug:project1 claude-haiku-4-5\nmcp profile-dev-tools github__list-repos\nquit\n' \
  | go run ./example repl /tmp/project1.cherry.zst
```

## Enforcement Model

The verifier runs before Cherry. It validates key or token material and produces a
principal slug, such as `slug:project1`. The enforcement point also knows its
scope from deployment context or request context, such as `workspace1`.

The hot path query is then:

```text
scope + principal_slug + requested_model -> packed LLM route
scope + MCP path + exposed_tool -> packed MCP upstream binding
```

## Stress Command

The stress command builds synthetic pack input, opens the reader, and measures
heap deltas plus lookup latency:

```sh
go run ./example stress-pack 1000000 100000 1
```

This is useful for checking the shape of memory growth and lookup cost when the
number of principals or profiles is large.
