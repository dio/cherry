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
    source_test.go
    testdata/example_fixture.yaml
    testdata/catalogs/
      models.json
      providers.json
      mcp-catalog-data-with-tools.json
      mcp-catalog-data.json
  transform/
    transform.go
```

`source` loads an editable fixture containing orgs, projects, workspaces, users,
keys, tags, LLM models/providers, MCP servers, and MCP profiles.

The `testdata/catalogs` files are copied seed/catalog examples. The example
loader treats them as external source data and transforms them into normalized
Cherry rows:

- `models.json` becomes enabled `cherry.Model` rows with pricing, limits, and
  capabilities preserved in `MetadataJSON`.
- `providers.json` becomes provider endpoint metadata. Fixture secret refs are
  preserved when catalog provider rows do not include secrets.
- `mcp-catalog-data-with-tools.json` becomes MCP server rows with tool pools.
- `mcp-catalog-data.json` exercises the alternate MCP catalog shape without
  tools.

`transform` applies the external-system decisions:

- workspace and project scope selection
- project-to-workspace fanout
- key reachability
- user and key tag inheritance
- rule precedence
- route tree validation and materialization for target/chain/split examples
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

Run the mapped split demo:

```sh
go run ./example mapped-split-demo --partitions 4 --generation gen-demo \
  project project1 example/source/testdata/example_fixture.yaml
```

This command keeps everything in memory so you can see both halves together. The
producer half builds `llm-generic`, `mcp-servers`, partitioned `llm-user-key-*`,
and partitioned `mcp-user-profile-*` bundles, then prints a `mapped-split-v1`
document. The consumer half opens the map, opens the listed zstd bundles as
Cherry readers, and runs LLM/MCP queries that show which lane or partition
answered.

The demo uses `cherry.MappedSplitSpec` for lane constants, component names, and
partition selection. Producer code uses it while assigning principals and MCP
profiles to bundle partitions; consumer code uses the same spec while choosing
which reader to query. This is the recommended integration shape for high-churn
bundle delivery.

It also prints an N+1 map revision where one `llm-user-key-*` partition URL and
checksum change, one `mcp-user-profile-*` partition is omitted, and the consumer
opens the revised map to show the updated LLM route and removed MCP profile. The
consumer side compares the revised map with the active view and fetches only
missing or stale bundle refs; unchanged refs are reused from the already-opened
readers.

Pack one project with the copied LLM/MCP catalogs:

```sh
go run ./example pack \
  --models example/source/testdata/catalogs/models.json \
  --providers example/source/testdata/catalogs/providers.json \
  --mcp-catalog example/source/testdata/catalogs/mcp-catalog-data-with-tools.json \
  project project1 example/source/testdata/example_fixture.yaml /tmp/project1-catalogs.cherry.zst
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
llm principals
mcp paths
mcp paths --tools
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

The fixture also includes richer routing override examples. Cherry’s compact
pack stores the compiled route tree, so the REPL can show the exact fallback or
split policy an enforcement point would execute.

BYOK always uses a plain target override and does not fall back to a platform key:

```text
use workspace1
llm slug:workspace1-byok-always claude-sonnet-4-5
```

BYOK preferred uses a provider anchor and chain retry. In this fixture the
`@openai` override tries a user secret first and falls back to the provider
default on 401:

```text
use workspace1
llm slug:workspace1-byok-preferred gpt-4o-mini-2
```

The BYOK-always example uses `env://WORKSPACE1_BYOK_ANTHROPIC_KEY`; the
BYOK-preferred example prints a two-child chain that tries
`env://ANTHROPIC_API_KEY` first and then falls back to the OpenAI provider
default in the fixture.

Weighted split example:

```text
use workspace2
llm slug:workspace2 claude-haiku-4-5
```

The REPL prints both split children and their weights.

Autofallback from any Anthropic request uses an `@anthropic` provider anchor and
a Fraser-style chain:

```text
use workspace3
llm slug:project2 claude-sonnet-4-5
```

The chain contains three fallback providers plus a final Vertex Anthropic target
using `name: claude-opus-4@20250514`.

Inspect model catalog data from the loaded bundle:

```text
llm providers
llm models --provider=openai
llm capability gpt-5 image_generation
llm model gpt-5
llm models
```

`llm models` prints the simulated `/v1/models` response derived from the packed
catalog metadata.

Resolve an MCP tool:

```text
mcp profile-dev-tools github__list-repos
```

The result includes the upstream server, upstream tool name, auth type, and secret
ref selected by the profile.

Resolve MCP initialize for a multi-server profile:

```text
mcp initialize profile-kiwi-and-github
```

The result includes the upstream server set behind the profile, including
endpoint, auth type, and secret ref for each server.

Resolve MCP list and call operations:

```text
mcp list profile=profile-kiwi-and-github
mcp call profile=profile-kiwi-and-github github__list-repos
mcp initialize server=github
mcp initialize server=aws-knowledge
mcp call server=aws-knowledge aws-knowledge__aws___list_regions
```

The same commands can be scripted:

```sh
printf 'use workspace1\nllm slug:project1 claude-haiku-4-5\nmcp initialize profile-kiwi-and-github\nmcp call profile-kiwi-and-github github__list-repos\nquit\n' \
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
scope + MCP path -> packed MCP initialize server set
```

## Stress Command

The stress command builds synthetic pack input, opens the reader, and measures
heap deltas plus lookup latency:

```sh
go run ./example stress-pack 1000000 100000 1
```

This is useful for checking the shape of memory growth and lookup cost when the
number of principals or profiles is large.
