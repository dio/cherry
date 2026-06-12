---
name: integrate-cherry
description: Integrate Cherry into producer or enforcement-point consumer code. Use when mapping external normalized tenancy/policy data into github.com/dio/cherry Input and zstd bundles, or when loading/querying Cherry bundles in an EP for LLM or MCP enforcement.
---

# Integrate Cherry

## Goal

Integrate Cherry at either side of the bundle boundary:

- Producer: external source records become `cherry.Input`, then a zstd bundle.
- Consumer/EP: delivered zstd bytes become an opened `cherry.Reader`, then LLM/MCP routing decisions.

Cherry is deliberately narrow. Keep product-specific tenancy, ownership, rule
merge, verifier, and source schema logic outside the root package.

## Decide The Side

If the task mentions source records, normalized rows, projects/workspaces, keys,
tags, profiles, fanout, or compilation, use the producer workflow.

If the task mentions enforcement point, loading a bundle, resolving LLM/MCP
requests, verifier output, active generation, or query latency, use the consumer
workflow.

If the task spans CP and EP, implement the producer first, then consume the exact
encoded bytes in the EP path.

## Producer Workflow

1. Read `AGENTS.md` and the "Producing A Bundle" section in `README.md`.
2. Keep source-specific structs and joins outside the root package.
3. Select concrete enforcement scopes, commonly workspaces.
4. For each scope, emit final normalized rows:
   - `cherry.Provider`
   - `cherry.Model`
   - `cherry.MCPServer`
   - `cherry.Principal`
   - `cherry.MCPProfile`
5. Prefer `Principal.ModelRoutes`; each requested model should point to the final selected `RoutePlan`.
6. Put effective MCP auth and secret refs on `MCPToolBinding`.
7. Build and encode:

   ```go
   blob, manifest, err := cherry.BuildWithManifest(input)
   if err != nil {
       return err
   }

   bundle := cherry.NewBundle(scopeKind, scopeID, scopes, blob, manifest)
   payload, err := cherry.EncodeBundleZstd(bundle)
   if err != nil {
       return err
   }
   ```

## Consumer Workflow

1. Fetch or receive bundle bytes from the control plane.
2. Open with manifest validation:

   ```go
   opened, err := cherry.OpenBundleZstd(payload)
   if err != nil {
       return err
   }
   reader := opened.Reader
   ```

3. Store the opened bundle or reader behind an active generation pointer. Swap the
   pointer atomically when a new bundle is loaded.
4. Select the active scope from enforcement context. For project bundles,
   `opened.Metadata.Scopes` lists contained workspace scopes.
5. For LLM requests, run key/JWT verification before Cherry and pass the resulting principal slug:

   ```go
   ids, ok := reader.ResolveLLMIDs(scopeID, principalSlug, requestedModel)
   if !ok {
       return reject()
   }
   ```

6. For MCP tool calls, resolve the path suffix and exposed tool:

   ```go
   tool, ok := reader.ResolveMCPToolIDs(scopeID, pathSuffix, exposedTool)
   if !ok {
       return reject()
   }
   ```

7. Materialize strings only when needed:

   ```go
   provider := reader.String(ids.ProviderSID)
   secretRef := reader.String(ids.SecretSID)
   upstreamTool := reader.String(tool.ToolSID)
   ```

## Rules Of Thumb

- Do not make Cherry infer project/workspace reachability.
- Do not put mutable rate-limit counters inside `Reader`.
- Do not store secret values; store secret refs.
- Do not keep stale hot-cache entries across generation swaps.
- Use ID-returning APIs on the hot path and inspector APIs for diagnostics.
- Keep bundle preparation and EP consumption tests connected by real encoded bytes when possible.

## Validation Checklist

Producer:

- project-level principals appear only in allowed scopes
- workspace-level principals appear only in their workspace scope
- keys do not leak across projects
- final model routes reflect external rule precedence before calling Cherry
- MCP auth overrides appear in `MCPToolBinding`
- `BuildWithManifest` rejects unknown provider/model/server references

Consumer:

- corrupt bundles fail to open
- unsupported versions fail to open
- missing scope/principal/model/tool rejects
- LLM route returns expected provider/model/secret ref
- MCP route returns expected upstream server/tool/auth/secret ref
- generation swap clears or invalidates any wrapper cache

## Reference

For concrete producer and consumer code shapes, read `references/mapping.md`.
