---
name: integrate-cherry
description: Integrate Cherry into producer or enforcement-point consumer code. Use when mapping external normalized tenancy/policy data into github.com/dio/cherry Input and zstd bundles, or when loading/querying Cherry bundles in an EP for LLM or MCP enforcement.
---

# Integrate Cherry

## Goal

Integrate Cherry at either side of the bundle boundary:

- Producer: external source records become `cherry.Input`, then a zstd bundle.
- Split producer: the same source selection becomes separate LLM and MCP
  `cherry.Input` values, then two zstd bundles with compatible metadata.
- Consumer/EP: delivered zstd bytes become an opened `cherry.Reader` or
  `cherry.SplitView`, then LLM/MCP routing decisions.

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

Prefer split LLM/MCP delivery when the task mentions independent rebuilds,
clustered policy, MCP-only churn, LLM-only churn, catalog split, or production
generation swaps. Prefer a combined bundle for the simplest integration path or
when the deployment cannot atomically swap paired artifacts.

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
5. Preserve model catalog fields needed by the EP, such as pricing, limits, and
   capabilities, in `cherry.Model.MetadataJSON` and `cherry.Model.Capabilities`.
6. Prefer `Principal.ModelRoutes`; each requested model should point to the compiled `RoutePlan`, which may be a target, chain, or split.
7. Provide enough MCP profile/server data for Cherry to answer initialize, list, and call routing.
8. Build and encode:

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

9. For split LLM/MCP delivery, build both artifacts from the same source
   selection but filtered normalized inputs:

   ```go
   llmInput := cherry.Input{
       Providers: input.Providers,
       Models:    input.Models,
       Scopes:    make([]cherry.Scope, 0, len(input.Scopes)),
   }
   mcpInput := cherry.Input{
       MCPServers: input.MCPServers,
       Scopes:     make([]cherry.Scope, 0, len(input.Scopes)),
   }

   for _, scope := range input.Scopes {
       llmInput.Scopes = append(llmInput.Scopes, cherry.Scope{
           ID:         scope.ID,
           Principals: scope.Principals,
       })
       mcpInput.Scopes = append(mcpInput.Scopes, cherry.Scope{
           ID:          scope.ID,
           MCPProfiles: scope.MCPProfiles,
       })
   }
   ```

   Then call `BuildWithManifest`, `NewBundle`, set the same
   `Bundle.Metadata.GenerationID` when using strict paired rollout checks, and
   `EncodeBundleZstd` for each artifact.

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

3. Store the opened bundle, reader, or split view behind an active generation
   pointer. Swap the pointer atomically when a new generation is loaded.
4. Select the active scope from enforcement context. For project bundles,
   `opened.Metadata.Scopes` lists contained workspace scopes.
5. For LLM requests, run key/JWT verification before Cherry and pass the resulting principal slug:

   ```go
   plan, ok := reader.ResolveLLMPlanIDs(scopeID, principalSlug, requestedModel)
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

7. For MCP initialize, resolve the upstream server set:

   ```go
   init, ok := reader.ResolveMCPInitializeIDs(scopeID, pathSuffix)
   if !ok {
       return reject()
   }
   ```

8. Materialize strings only when needed:

   ```go
   provider := reader.String(ids.ProviderSID)
   secretRef := reader.String(ids.SecretSID)
   upstreamTool := reader.String(tool.ToolSID)
   ```

9. For model catalog queries, use the loaded reader directly:

   ```go
   providers := reader.Providers()
   supportsImages := reader.ModelCapability(modelID, "image_generation")
   model, ok := reader.ResolveModel(modelID)
   modelsJSON, err := reader.V1ModelsJSON()
   providerModelsJSON, err := reader.V1ModelsJSONForProvider(providerID)
   ```

10. For split LLM/MCP delivery, open both artifacts together and query the
    composed view:

    ```go
    opened, err := cherry.OpenSplitBundleZstdWithOptions(llmPayload, mcpPayload, cherry.SplitBundleOptions{
        GenerationID: "generation-2026-06-14T12:00:00Z",
    })
    if err != nil {
        return err
    }
    view := opened.View

    plan, ok := view.ResolveLLMPlanIDs(scopeID, principalSlug, requestedModel)
    tool, ok := view.ResolveMCPToolIDs(scopeID, pathSuffix, exposedTool)
    ```

    String IDs are reader-local. Use `view.LLMString` for IDs returned by LLM
    methods and `view.MCPString` for IDs returned by MCP methods.

## Rules Of Thumb

- Do not make Cherry infer project/workspace reachability.
- Do not put mutable rate-limit counters inside `Reader`.
- Do not store secret values; store secret refs.
- Do not keep stale hot-cache entries across generation swaps.
- Use ID-returning APIs on the hot path and inspector APIs for diagnostics.
- Use split bundles to avoid rebuilding unrelated LLM or MCP sections when
  those policy clusters churn independently.
- Do not require matching pack manifests for split bundles; compatible split
  artifacts have the same scope kind, selected scope ID, concrete scopes, and
  optional shared generation/catalog expectations.
- Use `ResolveMCPInitializeIDs` for MCP initialize, `ResolveMCPIDs` for tools/list, and `ResolveMCPToolIDs` for tools/call.
- Use `Providers`, `ResolveModel`, `ModelCapability`, `V1ModelsJSON`, and
  `V1ModelsJSONForProvider` for provider/model catalog inspection and
  compatibility endpoints.
- Keep bundle preparation and EP consumption tests connected by real encoded bytes when possible.
- For split delivery tests, open the exact paired zstd bytes with
  `OpenSplitBundleZstdWithOptions` and assert both LLM and MCP queries through
  the resulting `SplitView`.

## Validation Checklist

Producer:

- project-level principals appear only in allowed scopes
- workspace-level principals appear only in their workspace scope
- keys do not leak across projects
- final model routes reflect external rule precedence before calling Cherry
- MCP initialize returns every upstream server behind a path with auth and secret refs
- `BuildWithManifest` rejects unknown provider/model/server references
- split LLM input contains providers, models, scopes, and principals, but no MCP
  servers or profiles
- split MCP input contains MCP servers, scopes, and MCP profiles, but no LLM
  providers, models, or principals
- split bundles produced from the same source selection carry matching
  `ScopeKind`, `ScopeID`, `Scopes`, and rollout `GenerationID` when strict
  generation checks are used

Consumer:

- corrupt bundles fail to open
- unsupported versions fail to open
- missing scope/principal/model/tool rejects
- LLM route returns expected provider/model/secret ref
- MCP initialize/list/call return expected upstream server/tool/auth/secret ref
- generation swap clears or invalidates any wrapper cache
- split bundle pair rejects mismatched scope kind, selected scope, concrete
  scopes, generation, or required catalog entries
- split view materializes IDs from the correct reader-local string table

## Reference

For concrete producer and consumer code shapes, read `references/mapping.md`.
