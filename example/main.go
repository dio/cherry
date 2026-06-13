package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/chzyer/readline"
	"gopkg.in/yaml.v3"

	cherry "github.com/dio/cherry"
	"github.com/dio/cherry/example/source"
	"github.com/dio/cherry/example/transform"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		printFixtureTree(source.ExampleFixture())
		return nil
	}
	switch args[0] {
	case "pack":
		return runPack(args[1:])
	case "repl":
		return runREPL(args[1:])
	case "split-check":
		return runSplitCheck(args[1:])
	case "stress-pack":
		return runStressPack(args[1:])
	case "help", "-h", "--help":
		printHelp()
		return nil
	default:
		fixture, err := source.LoadFixtureYAML(args[0])
		if err != nil {
			return err
		}
		printFixtureTree(fixture)
		return nil
	}
}

func printHelp() {
	fmt.Println("usage:")
	fmt.Println("  go run ./example")
	fmt.Println("  go run ./example [fixture.yaml]")
	fmt.Println("  go run ./example pack [--cluster combined|llm|mcp] [--generation id] [--models models.json] [--providers providers.json] [--mcp-catalog catalog.json] <workspace|project> <id> <fixture.yaml> [out.zst]")
	fmt.Println("  go run ./example repl <cherry.zst>")
	fmt.Println("  go run ./example split-check [--generation id] <llm.zst> <mcp.zst>")
	fmt.Println("  go run ./example stress-pack <principals-per-scope> <queries> [scopes]")
}

func runPack(args []string) error {
	flags, args, err := parsePackFlags(args)
	if err != nil {
		return err
	}
	if len(args) != 3 && len(args) != 4 {
		return fmt.Errorf("usage: pack [--cluster combined|llm|mcp] [--generation id] [--models models.json] [--providers providers.json] [--mcp-catalog catalog.json] <workspace|project> <id> <fixture.yaml> [out.zst]")
	}
	scopeKind := transform.ScopeKind(args[0])
	if scopeKind != transform.ScopeKindWorkspace && scopeKind != transform.ScopeKindProject {
		return fmt.Errorf("invalid scope kind %q; want workspace or project", args[0])
	}
	scopeID := args[1]
	fixture, err := source.LoadFixtureYAML(args[2])
	if err != nil {
		return err
	}
	if flags.modelsPath != "" {
		models, err := source.LoadModelCatalogJSON(flags.modelsPath)
		if err != nil {
			return err
		}
		fixture.Models = source.MergeModels(fixture.Models, models)
	}
	if flags.providersPath != "" {
		providers, err := source.LoadProviderCatalogJSON(flags.providersPath)
		if err != nil {
			return err
		}
		fixture.Providers = source.MergeProviders(fixture.Providers, providers)
	}
	if flags.mcpCatalogPath != "" {
		servers, err := source.LoadMCPCatalogJSON(flags.mcpCatalogPath)
		if err != nil {
			return err
		}
		fixture.MCPServers = source.MergeMCPServers(fixture.MCPServers, servers)
	}
	outPath := fmt.Sprintf("%s-%s.pack.zst", scopeKind, scopeID)
	if flags.cluster != packClusterCombined {
		outPath = fmt.Sprintf("%s-%s.%s.pack.zst", scopeKind, scopeID, flags.cluster)
	}
	if len(args) == 4 {
		outPath = args[3]
	}

	result, err := transform.BuildPackInput(fixture, transform.Selection{Kind: scopeKind, ID: scopeID})
	if err != nil {
		return err
	}
	input := inputForCluster(result.Input, flags.cluster)
	blob, manifest, err := cherry.BuildWithManifest(input)
	if err != nil {
		return err
	}
	bundle := cherry.NewBundle(string(scopeKind), scopeID, result.Scopes, blob, manifest)
	bundle.Metadata.GenerationID = flags.generationID
	compressed, err := cherry.EncodeBundleZstd(bundle)
	if err != nil {
		return err
	}
	if err := os.WriteFile(outPath, compressed, 0644); err != nil {
		return err
	}
	fmt.Printf(
		"wrote %s cluster=%s generation=%s scope=%s:%s scopes=%s providers=%d models=%d mcp_servers=%d raw_bytes=%d\n",
		outPath,
		flags.cluster,
		flags.generationID,
		scopeKind,
		scopeID,
		strings.Join(result.Scopes, ","),
		len(input.Providers),
		len(input.Models),
		len(input.MCPServers),
		len(blob),
	)
	return nil
}

type packCluster string

const (
	packClusterCombined packCluster = "combined"
	packClusterLLM      packCluster = "llm"
	packClusterMCP      packCluster = "mcp"
)

type packFlags struct {
	modelsPath     string
	providersPath  string
	mcpCatalogPath string
	cluster        packCluster
	generationID   string
}

func parsePackFlags(args []string) (packFlags, []string, error) {
	out := make([]string, 0, len(args))
	flags := packFlags{cluster: packClusterCombined}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--models":
			if i+1 >= len(args) {
				return packFlags{}, nil, fmt.Errorf("--models requires a path")
			}
			flags.modelsPath = args[i+1]
			i++
		case "--providers":
			if i+1 >= len(args) {
				return packFlags{}, nil, fmt.Errorf("--providers requires a path")
			}
			flags.providersPath = args[i+1]
			i++
		case "--mcp-catalog":
			if i+1 >= len(args) {
				return packFlags{}, nil, fmt.Errorf("--mcp-catalog requires a path")
			}
			flags.mcpCatalogPath = args[i+1]
			i++
		case "--cluster":
			if i+1 >= len(args) {
				return packFlags{}, nil, fmt.Errorf("--cluster requires combined, llm, or mcp")
			}
			flags.cluster = packCluster(args[i+1])
			if flags.cluster != packClusterCombined && flags.cluster != packClusterLLM && flags.cluster != packClusterMCP {
				return packFlags{}, nil, fmt.Errorf("invalid --cluster %q; want combined, llm, or mcp", args[i+1])
			}
			i++
		case "--generation":
			if i+1 >= len(args) {
				return packFlags{}, nil, fmt.Errorf("--generation requires an id")
			}
			flags.generationID = args[i+1]
			i++
		default:
			out = append(out, args[i])
		}
	}
	return flags, out, nil
}

func inputForCluster(input cherry.Input, cluster packCluster) cherry.Input {
	switch cluster {
	case packClusterLLM:
		out := cherry.Input{
			Providers: append([]cherry.Provider(nil), input.Providers...),
			Models:    append([]cherry.Model(nil), input.Models...),
			Scopes:    make([]cherry.Scope, 0, len(input.Scopes)),
		}
		for _, scope := range input.Scopes {
			outScope := cherry.Scope{
				ID:         scope.ID,
				Principals: append([]cherry.Principal(nil), scope.Principals...),
			}
			out.Scopes = append(out.Scopes, outScope)
		}
		return out
	case packClusterMCP:
		out := cherry.Input{
			MCPServers: append([]cherry.MCPServer(nil), input.MCPServers...),
			Scopes:     make([]cherry.Scope, 0, len(input.Scopes)),
		}
		for _, scope := range input.Scopes {
			outScope := cherry.Scope{
				ID:          scope.ID,
				MCPProfiles: append([]cherry.MCPProfile(nil), scope.MCPProfiles...),
			}
			out.Scopes = append(out.Scopes, outScope)
		}
		return out
	default:
		return input
	}
}

func runREPL(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: repl <cherry.zst>")
	}
	opened, err := loadPack(args[0])
	if err != nil {
		return err
	}
	session := replSession{
		path:  args[0],
		pack:  opened,
		scope: defaultScope(opened.Metadata.Scopes),
	}
	fmt.Printf("loaded pack scope=%s:%s scopes=%s raw_bytes=%d\n", opened.Metadata.ScopeKind, opened.Metadata.ScopeID, strings.Join(opened.Metadata.Scopes, ","), len(opened.Blob))
	if session.scope != "" {
		fmt.Printf("using scope %s\n", session.scope)
	}
	fmt.Println("commands: summary, scopes, use <scope>, llm principals|providers|models|model|capability, mcp paths|initialize|list|call, inspect <metadata|principals|mcp|all>, reload, help, quit")

	rl, err := readline.NewEx(&readline.Config{Prompt: "cherry> "})
	if err != nil {
		return err
	}
	defer func() {
		_ = rl.Close()
	}()
	for {
		line, err := rl.Readline()
		if errors.Is(err, io.EOF) || errors.Is(err, readline.ErrInterrupt) {
			return nil
		}
		if err != nil {
			return err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !session.handle(line) {
			return nil
		}
	}
}

func runSplitCheck(args []string) error {
	generationID := ""
	filtered := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--generation":
			if i+1 >= len(args) {
				return fmt.Errorf("--generation requires an id")
			}
			generationID = args[i+1]
			i++
		default:
			filtered = append(filtered, args[i])
		}
	}
	if len(filtered) != 2 {
		return fmt.Errorf("usage: split-check [--generation id] <llm.zst> <mcp.zst>")
	}
	opened, err := loadSplitPack(filtered[0], filtered[1], cherry.SplitBundleOptions{GenerationID: generationID})
	if err != nil {
		return err
	}
	fmt.Printf("loaded split scope=%s:%s scopes=%s generation=%s\n",
		opened.LLM.Metadata.ScopeKind,
		opened.LLM.Metadata.ScopeID,
		strings.Join(opened.LLM.Metadata.Scopes, ","),
		opened.LLM.Metadata.GenerationID,
	)
	fmt.Printf("  llm_raw_bytes: %d\n", len(opened.LLM.Blob))
	fmt.Printf("  llm_manifest_checksum: %d\n", opened.LLM.Metadata.PackManifest.Checksum)
	fmt.Printf("  llm_providers: %d\n", len(opened.View.Providers()))
	fmt.Printf("  llm_models: %d\n", len(opened.View.Models()))
	fmt.Printf("  mcp_raw_bytes: %d\n", len(opened.MCP.Blob))
	fmt.Printf("  mcp_manifest_checksum: %d\n", opened.MCP.Metadata.PackManifest.Checksum)
	fmt.Printf("  mcp_servers: %d\n", len(opened.View.MCPServers()))
	return nil
}

type replSession struct {
	path  string
	pack  cherry.OpenedBundle
	scope string
}

func (s *replSession) handle(line string) bool {
	fields := strings.Fields(line)
	switch fields[0] {
	case "quit", "exit":
		return false
	case "help":
		printREPLHelp()
	case "summary":
		printPackSummary(s.pack, s.scope)
	case "scopes":
		printYAML(s.pack.Reader.ScopeIDs())
	case "use":
		if len(fields) != 2 {
			fmt.Println("usage: use <scope>")
			return true
		}
		if !contains(s.pack.Metadata.Scopes, fields[1]) {
			fmt.Printf("scope %q is not in this bundle; scopes=%s\n", fields[1], strings.Join(s.pack.Metadata.Scopes, ","))
			return true
		}
		s.scope = fields[1]
		fmt.Printf("using scope %s\n", s.scope)
	case "reload":
		opened, err := loadPack(s.path)
		if err != nil {
			fmt.Printf("reload failed: %v\n", err)
			return true
		}
		s.pack = opened
		if !contains(opened.Metadata.Scopes, s.scope) {
			s.scope = defaultScope(opened.Metadata.Scopes)
		}
		fmt.Printf("reloaded scopes=%s\n", strings.Join(opened.Metadata.Scopes, ","))
	case "inspect":
		s.inspect(fields)
	case "llm":
		s.llm(fields)
	case "mcp":
		s.mcp(fields)
	default:
		fmt.Printf("unknown command %q; type help\n", fields[0])
	}
	return true
}

func (s *replSession) inspect(fields []string) {
	if len(fields) != 2 {
		fmt.Println("usage: inspect <metadata|principals|mcp|all>")
		return
	}
	switch fields[1] {
	case "metadata":
		printYAML(s.pack.Metadata)
	case "principals":
		scope := s.activeScope()
		if scope == "" {
			return
		}
		routes, ok := s.pack.Reader.PrincipalRoutes(scope)
		if !ok {
			fmt.Printf("unknown scope %q\n", scope)
			return
		}
		printYAML(routes)
	case "mcp":
		scope := s.activeScope()
		if scope == "" {
			return
		}
		paths, ok := s.pack.Reader.MCPPaths(scope)
		if !ok {
			fmt.Printf("unknown scope %q\n", scope)
			return
		}
		printYAML(paths)
	case "all":
		scope := s.activeScope()
		if scope == "" {
			return
		}
		routes, _ := s.pack.Reader.PrincipalRoutes(scope)
		paths, _ := s.pack.Reader.MCPPaths(scope)
		printYAML(struct {
			Metadata   cherry.BundleMetadata   `yaml:"metadata"`
			Principals []cherry.PrincipalRoute `yaml:"principals"`
			MCP        []cherry.MCPPath        `yaml:"mcp"`
		}{s.pack.Metadata, routes, paths})
	default:
		fmt.Printf("unknown inspect area %q\n", fields[1])
	}
}

func (s *replSession) llm(fields []string) {
	if len(fields) >= 2 {
		switch fields[1] {
		case "principals":
			s.llmPrincipals(fields)
			return
		case "providers":
			s.llmProviders(fields)
			return
		case "models":
			s.llmModels(fields)
			return
		case "model":
			s.llmModel(fields)
			return
		case "capability":
			s.llmCapability(fields)
			return
		}
	}
	if len(fields) != 3 && len(fields) != 4 {
		fmt.Println("usage: llm [scope] <principal-slug> <model> | llm principals [scope] | llm providers | llm models [--provider=name] | llm model <model> | llm capability <model> <capability>")
		return
	}
	scope, offset := s.scopeAndOffset(fields, 3)
	if scope == "" {
		return
	}
	principalSlug := fields[offset]
	requestedModel := fields[offset+1]
	plan, ok := s.pack.Reader.ResolveLLMPlan(scope, principalSlug, requestedModel)
	if !ok {
		fmt.Printf("rejected: no LLM route for scope=%s principal=%s model=%s\n", scope, principalSlug, requestedModel)
		s.printLLMRejectHint(scope, principalSlug)
		return
	}
	fmt.Printf("scope: %s\n", scope)
	fmt.Printf("principal: %s\n", principalSlug)
	fmt.Printf("requested_model: %s\n", requestedModel)
	fmt.Println("route_plan:")
	printLLMRoutePlan(s.pack.Reader, plan.Plan, 2)
	fmt.Printf("rate_limit: usd_per_day=%.2f rpm=%d on_exceed=%s\n", float64(plan.Rate.USDPerDayCents)/100, plan.Rate.RPM, plan.Rate.OnExceed)
}

func (s *replSession) printLLMRejectHint(scope string, principalSlug string) {
	principals, ok := s.pack.Reader.Principals(scope)
	if !ok || len(principals) == 0 {
		return
	}
	fmt.Println("available_principals:")
	for _, principal := range principals {
		fmt.Printf("  - %s\n", principal.PrincipalSlug)
	}
	if !strings.HasPrefix(principalSlug, "slug:") {
		fmt.Println("hint: pass the verified principal slug from the bundle, not the source key id; use `llm principals` to list slugs")
	}
	if len(s.pack.Metadata.Scopes) > 1 {
		fmt.Printf("hint: active scope is %s; bundle scopes=%s\n", scope, strings.Join(s.pack.Metadata.Scopes, ","))
	}
}

func printLLMRoutePlan(reader cherry.Reader, plan cherry.LLMRoutePlan, indent int) {
	pad := strings.Repeat(" ", indent)
	switch plan.Kind {
	case cherry.RouteKindTarget:
		fmt.Printf("%starget:\n", pad)
		fmt.Printf(
			"%s  provider=%s kind=%s model=%s model_name=%s endpoint=%s secret_ref=%s\n",
			pad,
			plan.Provider,
			plan.ProviderKind,
			plan.Model,
			plan.ModelName,
			plan.Endpoint,
			plan.SecretRef,
		)
		printResolvedModelMetadataIndented(reader, plan.Model, indent+2)
	case cherry.RouteKindChain:
		fmt.Printf("%schain:\n", pad)
		if plan.RetryOn != "" || plan.PerTryTimeoutMS != 0 {
			fmt.Printf("%s  retry_on: %s\n", pad, plan.RetryOn)
			fmt.Printf("%s  per_try_timeout_ms: %d\n", pad, plan.PerTryTimeoutMS)
		}
		fmt.Printf("%s  children:\n", pad)
		for _, child := range plan.Children {
			fmt.Printf("%s    -\n", pad)
			printLLMRoutePlan(reader, child.Plan, indent+6)
		}
	case cherry.RouteKindSplit:
		fmt.Printf("%ssplit:\n", pad)
		fmt.Printf("%s  children:\n", pad)
		for _, child := range plan.Children {
			fmt.Printf("%s    - weight: %d\n", pad, child.Weight)
			printLLMRoutePlan(reader, child.Plan, indent+6)
		}
	default:
		fmt.Printf("%sunknown: %s\n", pad, plan.Kind)
	}
}

func printResolvedModelMetadataIndented(reader cherry.Reader, modelID string, indent int) {
	pad := strings.Repeat(" ", indent)
	model, ok := reader.ResolveModel(modelID)
	if !ok {
		return
	}
	fmt.Printf("%smodel_catalog:\n", pad)
	fmt.Printf("%s  mode: %s\n", pad, model.Mode)
	if len(model.Capabilities) == 0 {
		fmt.Printf("%s  capabilities: []\n", pad)
	} else {
		fmt.Printf("%s  capabilities:\n", pad)
		for _, capability := range model.Capabilities {
			fmt.Printf("%s    - %s\n", pad, capability)
		}
	}
	metadata := compactModelMetadata(model.MetadataJSON)
	if metadata.ContextWindow != 0 {
		fmt.Printf("%s  context_window: %d\n", pad, metadata.ContextWindow)
	}
	if metadata.MaxOutputTokens != 0 {
		fmt.Printf("%s  max_output_tokens: %d\n", pad, metadata.MaxOutputTokens)
	}
	if metadata.InputPricePerMillion != "" || metadata.OutputPricePerMillion != "" {
		fmt.Printf("%s  price_per_million:\n", pad)
		if metadata.InputPricePerMillion != "" {
			fmt.Printf("%s    input: %s\n", pad, metadata.InputPricePerMillion)
		}
		if metadata.CachedPricePerMillion != "" {
			fmt.Printf("%s    cached: %s\n", pad, metadata.CachedPricePerMillion)
		}
		if metadata.CachingPricePerMillion != "" {
			fmt.Printf("%s    caching: %s\n", pad, metadata.CachingPricePerMillion)
		}
		if metadata.OutputPricePerMillion != "" {
			fmt.Printf("%s    output: %s\n", pad, metadata.OutputPricePerMillion)
		}
	}
}

type compactLLMModelMetadata struct {
	ContextWindow          int64
	MaxOutputTokens        int64
	InputPricePerMillion   string
	OutputPricePerMillion  string
	CachedPricePerMillion  string
	CachingPricePerMillion string
}

func compactModelMetadata(metadataJSON string) compactLLMModelMetadata {
	if metadataJSON == "" {
		return compactLLMModelMetadata{}
	}
	var raw struct {
		ContextWindow                int64           `json:"contextWindow"`
		InputTokensPricePerMillion   flexibleString  `json:"inputTokensPricePerMillion"`
		OutputTokensPricePerMillion  flexibleString  `json:"outputTokensPricePerMillion"`
		CachedTokensPricePerMillion  flexibleString  `json:"cachedTokensPricePerMillion"`
		CachingTokensPricePerMillion flexibleString  `json:"cachingTokensPricePerMillion"`
		Limits                       json.RawMessage `json:"limits"`
	}
	if err := json.Unmarshal([]byte(metadataJSON), &raw); err != nil {
		return compactLLMModelMetadata{}
	}
	var limits struct {
		MaxOutputTokens int64 `json:"max_output_tokens"`
	}
	if len(raw.Limits) > 0 {
		_ = json.Unmarshal(raw.Limits, &limits)
	}
	return compactLLMModelMetadata{
		ContextWindow:          raw.ContextWindow,
		MaxOutputTokens:        limits.MaxOutputTokens,
		InputPricePerMillion:   string(raw.InputTokensPricePerMillion),
		OutputPricePerMillion:  string(raw.OutputTokensPricePerMillion),
		CachedPricePerMillion:  string(raw.CachedTokensPricePerMillion),
		CachingPricePerMillion: string(raw.CachingTokensPricePerMillion),
	}
}

type flexibleString string

func (s *flexibleString) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		return nil
	}
	var value string
	if len(data) > 0 && data[0] == '"' {
		if err := json.Unmarshal(data, &value); err != nil {
			return err
		}
	} else {
		value = string(data)
	}
	*s = flexibleString(value)
	return nil
}

func (s *replSession) llmPrincipals(fields []string) {
	scope, ok := s.optionalScope(fields, "llm principals [scope]")
	if !ok {
		return
	}
	principals, found := s.pack.Reader.Principals(scope)
	if !found {
		fmt.Printf("unknown scope %q\n", scope)
		return
	}
	type principalSummary struct {
		ScopeID             string `yaml:"scope_id"`
		PrincipalSlug       string `yaml:"principal_slug"`
		RequestedModelCount int    `yaml:"requested_model_count"`
	}
	summaries := make([]principalSummary, 0, len(principals))
	for _, principal := range principals {
		summaries = append(summaries, principalSummary{
			ScopeID:             principal.ScopeID,
			PrincipalSlug:       principal.PrincipalSlug,
			RequestedModelCount: len(principal.RequestedModels),
		})
	}
	printYAML(summaries)
}

func (s *replSession) llmProviders(fields []string) {
	if len(fields) != 2 {
		fmt.Println("usage: llm providers")
		return
	}
	printYAML(s.pack.Reader.Providers())
}

func (s *replSession) llmModels(fields []string) {
	providerID, ok := parseProviderOnlyArgs(fields[2:])
	if !ok {
		fmt.Println("usage: llm models [--provider=<provider>]")
		return
	}
	var (
		payload []byte
		err     error
	)
	if providerID == "" {
		payload, err = s.pack.Reader.V1ModelsJSON()
	} else {
		payload, err = s.pack.Reader.V1ModelsJSONForProvider(providerID)
	}
	if err != nil {
		fmt.Printf("models failed: %v\n", err)
		return
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, payload, "", "  "); err != nil {
		fmt.Println(string(payload))
		return
	}
	fmt.Println(pretty.String())
}

func parseProviderOnlyArgs(args []string) (string, bool) {
	var providerID string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--provider":
			if i+1 >= len(args) || args[i+1] == "" {
				return "", false
			}
			providerID = args[i+1]
			i++
		case strings.HasPrefix(arg, "--provider="):
			providerID = strings.TrimPrefix(arg, "--provider=")
			if providerID == "" {
				return "", false
			}
		default:
			return "", false
		}
	}
	return providerID, true
}

func (s *replSession) llmModel(fields []string) {
	modelID, providerID, ok := parseLLMModelArgs(fields[2:])
	if !ok {
		fmt.Println("usage: llm model <model> [--provider=<provider>] | llm model --provider=<provider>")
		return
	}
	if providerID != "" && modelID == "" {
		models := modelsForProvider(s.pack.Reader.Models(), providerID)
		if len(models) == 0 {
			fmt.Printf("no models for provider %q\n", providerID)
			return
		}
		printYAML(models)
		return
	}
	model, found := s.pack.Reader.ResolveModel(modelID)
	if !found {
		fmt.Printf("unknown model %q\n", modelID)
		return
	}
	if providerID != "" && model.Provider != providerID {
		fmt.Printf("model %q belongs to provider %q, not %q\n", modelID, model.Provider, providerID)
		return
	}
	printYAML(model)
}

func parseLLMModelArgs(args []string) (string, string, bool) {
	var modelID string
	var providerID string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--provider":
			if i+1 >= len(args) || args[i+1] == "" {
				return "", "", false
			}
			providerID = args[i+1]
			i++
		case strings.HasPrefix(arg, "--provider="):
			providerID = strings.TrimPrefix(arg, "--provider=")
			if providerID == "" {
				return "", "", false
			}
		case strings.HasPrefix(arg, "--"):
			return "", "", false
		default:
			if modelID != "" {
				return "", "", false
			}
			modelID = arg
		}
	}
	if modelID == "" && providerID == "" {
		return "", "", false
	}
	return modelID, providerID, true
}

func modelsForProvider(models []cherry.ModelInfo, providerID string) []cherry.ModelInfo {
	filtered := make([]cherry.ModelInfo, 0)
	for _, model := range models {
		if model.Provider == providerID {
			filtered = append(filtered, model)
		}
	}
	return filtered
}

func (s *replSession) llmCapability(fields []string) {
	if len(fields) != 4 {
		fmt.Println("usage: llm capability <model> <capability>")
		return
	}
	printYAML(struct {
		Model      string `yaml:"model"`
		Capability string `yaml:"capability"`
		Supported  bool   `yaml:"supported"`
	}{
		Model:      fields[2],
		Capability: fields[3],
		Supported:  s.pack.Reader.ModelCapability(fields[2], fields[3]),
	})
}

func (s *replSession) mcp(fields []string) {
	if len(fields) < 2 {
		fmt.Println("usage: mcp [scope] <path> [tool] | mcp <paths|initialize|list|call> [scope] <path|profile=name|server=name> [tool]")
		return
	}
	switch fields[1] {
	case "paths":
		s.mcpPaths(fields)
		return
	case "initialize":
		s.mcpInitialize(fields)
		return
	case "list":
		s.mcpList(fields)
		return
	case "call":
		s.mcpCall(fields)
		return
	}
	if len(fields) > 4 {
		fmt.Println("usage: mcp [scope] <path> [tool]")
		return
	}
	scope := s.scope
	pathOffset := 1
	if len(fields) >= 3 && contains(s.pack.Metadata.Scopes, fields[1]) {
		scope = fields[1]
		pathOffset = 2
	}
	if scope == "" {
		fmt.Println("no active scope; run use <scope>")
		return
	}
	path := normalizeMCPTarget(fields[pathOffset])
	if len(fields) > pathOffset+1 {
		toolName := fields[pathOffset+1]
		tool, ok := s.pack.Reader.ResolveMCPToolIDs(scope, path, toolName)
		if !ok {
			fmt.Printf("rejected: no MCP tool for scope=%s path=%s tool=%s\n", scope, path, toolName)
			return
		}
		fmt.Printf("scope: %s\n", scope)
		fmt.Printf("path: %s\n", path)
		fmt.Printf("tool: %s -> server=%s endpoint=%s upstream_tool=%s auth_type=%s secret_ref=%s\n",
			toolName,
			s.pack.Reader.String(tool.ServerSID),
			s.pack.Reader.String(tool.ServerEndpointSID),
			s.pack.Reader.String(tool.ToolSID),
			s.pack.Reader.String(tool.AuthTypeSID),
			s.pack.Reader.String(tool.SecretSID),
		)
		return
	}
	result, ok := s.pack.Reader.ResolveMCP(scope, path)
	if !ok {
		fmt.Printf("rejected: no MCP path for scope=%s path=%s\n", scope, path)
		return
	}
	fmt.Printf("scope: %s\n", scope)
	fmt.Printf("path: %s\n", result.Path)
	fmt.Println("tools:")
	for _, tool := range result.Tools {
		fmt.Printf("  %s -> server=%s endpoint=%s upstream_tool=%s auth_type=%s secret_ref=%s\n", tool.ExposedName, tool.Server, tool.ServerEndpoint, tool.Tool, tool.AuthType, tool.SecretRef)
	}
}

func (s *replSession) mcpPaths(fields []string) {
	scope, showTools, ok := s.optionalScopeWithTools(fields, "mcp paths [scope] [--tools]")
	if !ok {
		return
	}
	paths, found := s.pack.Reader.MCPPaths(scope)
	if !found {
		fmt.Printf("unknown scope %q\n", scope)
		return
	}
	type pathSummary struct {
		ScopeID   string   `yaml:"scope_id"`
		Path      string   `yaml:"path"`
		ToolCount int      `yaml:"tool_count"`
		Tools     []string `yaml:"tools,omitempty"`
	}
	summaries := make([]pathSummary, 0, len(paths))
	for _, path := range paths {
		tools := make([]string, 0, len(path.Tools))
		if showTools {
			for _, tool := range path.Tools {
				tools = append(tools, tool.ExposedName)
			}
		}
		summaries = append(summaries, pathSummary{
			ScopeID:   path.ScopeID,
			Path:      path.Path,
			ToolCount: len(path.Tools),
			Tools:     tools,
		})
	}
	printYAML(summaries)
}

func (s *replSession) mcpInitialize(fields []string) {
	scope, path, ok := s.mcpCommandScopeAndPath(fields, "mcp initialize [scope] <path|profile=name|server=name>")
	if !ok {
		return
	}
	result, found := s.pack.Reader.ResolveMCPInitialize(scope, path)
	if !found {
		fmt.Printf("rejected: no MCP path for scope=%s path=%s\n", scope, path)
		return
	}
	fmt.Printf("scope: %s\n", scope)
	fmt.Printf("path: %s\n", result.Path)
	fmt.Println("initialize_servers:")
	for _, server := range result.Servers {
		fmt.Printf("  server=%s endpoint=%s auth_type=%s secret_ref=%s\n", server.Server, server.Endpoint, server.AuthType, server.SecretRef)
	}
}

func (s *replSession) mcpList(fields []string) {
	scope, path, ok := s.mcpCommandScopeAndPath(fields, "mcp list [scope] <path|profile=name|server=name>")
	if !ok {
		return
	}
	result, found := s.pack.Reader.ResolveMCP(scope, path)
	if !found {
		fmt.Printf("rejected: no MCP path for scope=%s path=%s\n", scope, path)
		return
	}
	fmt.Printf("scope: %s\n", scope)
	fmt.Printf("path: %s\n", result.Path)
	fmt.Println("tools:")
	for _, tool := range result.Tools {
		fmt.Printf("  %s -> server=%s endpoint=%s upstream_tool=%s auth_type=%s secret_ref=%s\n", tool.ExposedName, tool.Server, tool.ServerEndpoint, tool.Tool, tool.AuthType, tool.SecretRef)
	}
}

func (s *replSession) mcpCall(fields []string) {
	if len(fields) != 4 && len(fields) != 5 {
		fmt.Println("usage: mcp call [scope] <path|profile=name|server=name> <tool>")
		return
	}
	scope := s.scope
	pathOffset := 2
	if len(fields) == 5 {
		if !contains(s.pack.Metadata.Scopes, fields[2]) {
			fmt.Printf("scope %q is not in this bundle\n", fields[2])
			return
		}
		scope = fields[2]
		pathOffset = 3
	}
	if scope == "" {
		fmt.Println("no active scope; run use <scope>")
		return
	}
	path := normalizeMCPTarget(fields[pathOffset])
	toolName := fields[pathOffset+1]
	tool, found := s.pack.Reader.ResolveMCPToolIDs(scope, path, toolName)
	if !found {
		fmt.Printf("rejected: no MCP tool for scope=%s path=%s tool=%s\n", scope, path, toolName)
		return
	}
	fmt.Printf("scope: %s\n", scope)
	fmt.Printf("path: %s\n", path)
	fmt.Printf("call: %s -> server=%s endpoint=%s upstream_tool=%s auth_type=%s secret_ref=%s\n",
		toolName,
		s.pack.Reader.String(tool.ServerSID),
		s.pack.Reader.String(tool.ServerEndpointSID),
		s.pack.Reader.String(tool.ToolSID),
		s.pack.Reader.String(tool.AuthTypeSID),
		s.pack.Reader.String(tool.SecretSID),
	)
}

func (s *replSession) mcpCommandScopeAndPath(fields []string, usage string) (string, string, bool) {
	if len(fields) != 3 && len(fields) != 4 {
		fmt.Println("usage: " + usage)
		return "", "", false
	}
	scope := s.scope
	pathOffset := 2
	if len(fields) == 4 {
		if !contains(s.pack.Metadata.Scopes, fields[2]) {
			fmt.Printf("scope %q is not in this bundle\n", fields[2])
			return "", "", false
		}
		scope = fields[2]
		pathOffset = 3
	}
	if scope == "" {
		fmt.Println("no active scope; run use <scope>")
		return "", "", false
	}
	return scope, normalizeMCPTarget(fields[pathOffset]), true
}

func normalizeMCPTarget(value string) string {
	switch {
	case strings.HasPrefix(value, "server="):
		return "s/" + strings.TrimPrefix(value, "server=")
	case strings.HasPrefix(value, "profile="):
		return strings.TrimPrefix(value, "profile=")
	default:
		return value
	}
}

func (s *replSession) activeScope() string {
	if s.scope == "" {
		fmt.Println("no active scope; run use <scope>")
	}
	return s.scope
}

func (s *replSession) scopeAndOffset(fields []string, noScopeLen int) (string, int) {
	if len(fields) == noScopeLen {
		if s.scope == "" {
			fmt.Println("no active scope; run use <scope>")
			return "", 0
		}
		return s.scope, 1
	}
	if !contains(s.pack.Metadata.Scopes, fields[1]) {
		fmt.Printf("scope %q is not in this bundle\n", fields[1])
		return "", 0
	}
	return fields[1], 2
}

func (s *replSession) optionalScope(fields []string, usage string) (string, bool) {
	if len(fields) != 2 && len(fields) != 3 {
		fmt.Println("usage: " + usage)
		return "", false
	}
	if len(fields) == 2 {
		if s.scope == "" {
			fmt.Println("no active scope; run use <scope>")
			return "", false
		}
		return s.scope, true
	}
	if !contains(s.pack.Metadata.Scopes, fields[2]) {
		fmt.Printf("scope %q is not in this bundle\n", fields[2])
		return "", false
	}
	return fields[2], true
}

func (s *replSession) optionalScopeWithTools(fields []string, usage string) (string, bool, bool) {
	scope := s.scope
	showTools := false
	if len(fields) < 2 || len(fields) > 4 {
		fmt.Println("usage: " + usage)
		return "", false, false
	}
	for _, field := range fields[2:] {
		if field == "--tools" {
			showTools = true
			continue
		}
		if scope != "" && scope != s.scope {
			fmt.Println("usage: " + usage)
			return "", false, false
		}
		if !contains(s.pack.Metadata.Scopes, field) {
			fmt.Printf("scope %q is not in this bundle\n", field)
			return "", false, false
		}
		scope = field
	}
	if scope == "" {
		fmt.Println("no active scope; run use <scope>")
		return "", false, false
	}
	return scope, showTools, true
}

func printREPLHelp() {
	fmt.Println("commands:")
	fmt.Println("  summary                         print bundle metadata")
	fmt.Println("  scopes                          list scopes in the loaded bundle")
	fmt.Println("  use <scope>                     select the active enforcement scope")
	fmt.Println("  llm [scope] <slug> <model>      resolve an LLM request")
	fmt.Println("  llm principals [scope]          list LLM principal slugs and model counts")
	fmt.Println("  llm providers                   list packed LLM providers")
	fmt.Println("  llm models [--provider=name]    print simulated /v1/models catalog")
	fmt.Println("  llm model <model>               inspect one packed model")
	fmt.Println("  mcp paths [scope] [--tools]     list MCP paths, optionally with tool names")
	fmt.Println("  mcp initialize [scope] <target> resolve upstream MCP servers for initialize")
	fmt.Println("  mcp list [scope] <target>       list exposed MCP tools for a target")
	fmt.Println("  mcp call [scope] <target> <tool> resolve one MCP tool call")
	fmt.Println("  mcp [scope] <path> [tool]       compatibility form for list or call")
	fmt.Println("  inspect metadata                print bundle metadata")
	fmt.Println("  inspect principals              dump principal/model routes for active scope")
	fmt.Println("  inspect mcp                     dump MCP paths and tool bindings for active scope")
	fmt.Println("  inspect all                     print all inspectable data for active scope")
	fmt.Println("  reload                          reload the pack file")
	fmt.Println("  quit                            exit")
}

func loadPack(path string) (cherry.OpenedBundle, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return cherry.OpenedBundle{}, err
	}
	return cherry.OpenBundleZstd(data)
}

func loadSplitPack(llmPath string, mcpPath string, options cherry.SplitBundleOptions) (cherry.OpenedSplitBundle, error) {
	llmData, err := os.ReadFile(llmPath)
	if err != nil {
		return cherry.OpenedSplitBundle{}, err
	}
	mcpData, err := os.ReadFile(mcpPath)
	if err != nil {
		return cherry.OpenedSplitBundle{}, err
	}
	return cherry.OpenSplitBundleZstdWithOptions(llmData, mcpData, options)
}

func defaultScope(scopes []string) string {
	if len(scopes) == 1 {
		return scopes[0]
	}
	return ""
}

func printPackSummary(opened cherry.OpenedBundle, activeScope string) {
	fmt.Printf("pack: %s scope=%s:%s\n", opened.Metadata.FormatVersion, opened.Metadata.ScopeKind, opened.Metadata.ScopeID)
	fmt.Printf("  scopes: %s\n", strings.Join(opened.Metadata.Scopes, ", "))
	if activeScope != "" {
		fmt.Printf("  active_scope: %s\n", activeScope)
	}
	fmt.Printf("  raw_bytes: %d\n", len(opened.Blob))
	fmt.Printf("  manifest_version: %d\n", opened.Metadata.PackManifest.FormatVersion)
	fmt.Printf("  manifest_checksum: %d\n", opened.Metadata.PackManifest.Checksum)
}

func runStressPack(args []string) error {
	if len(args) != 2 && len(args) != 3 {
		return fmt.Errorf("usage: stress-pack <principals-per-scope> <queries> [scopes]")
	}
	principals, err := strconv.Atoi(args[0])
	if err != nil || principals <= 0 {
		return fmt.Errorf("invalid principals-per-scope %q", args[0])
	}
	queries, err := strconv.Atoi(args[1])
	if err != nil || queries <= 0 {
		return fmt.Errorf("invalid queries %q", args[1])
	}
	scopes := 1
	if len(args) == 3 {
		scopes, err = strconv.Atoi(args[2])
		if err != nil || scopes <= 0 {
			return fmt.Errorf("invalid scopes %q", args[2])
		}
	}

	runtime.GC()
	beforeBuild := heapAlloc()
	start := time.Now()
	input := buildStressInput(scopes, principals)
	blob, manifest, err := cherry.BuildWithManifest(input)
	if err != nil {
		return err
	}
	buildElapsed := time.Since(start)
	afterBuild := heapAlloc()

	runtime.GC()
	beforeOpen := heapAlloc()
	reader, err := cherry.OpenWithManifest(blob, manifest)
	if err != nil {
		return err
	}
	afterOpen := heapAlloc()

	latencies := make([]int64, 0, queries)
	queryStart := time.Now()
	var found int
	for i := 0; i < queries; i++ {
		scopeID := "workspace" + strconv.Itoa(i%scopes+1)
		principalID := i%principals + 1
		principalSlug := fmt.Sprintf("slug:%d:%d", i%scopes+1, principalID)
		lookupStart := time.Now()
		_, ok := reader.ResolveLLMIDs(scopeID, principalSlug, "gpt-4o-mini")
		latencies = append(latencies, time.Since(lookupStart).Nanoseconds())
		if ok {
			found++
		}
	}
	queryElapsed := time.Since(queryStart)
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })

	fmt.Printf("stress_pack:\n")
	fmt.Printf("  scopes: %d\n", scopes)
	fmt.Printf("  principals_per_scope: %d\n", principals)
	fmt.Printf("  total_principals: %d\n", scopes*principals)
	fmt.Printf("  queries: %d\n", queries)
	fmt.Printf("  found: %d\n", found)
	fmt.Printf("  build_time_ms: %d\n", buildElapsed.Milliseconds())
	fmt.Printf("  raw_pack_bytes: %d\n", len(blob))
	fmt.Printf("  manifest_version: %d\n", manifest.FormatVersion)
	fmt.Printf("  manifest_checksum: %d\n", manifest.Checksum)
	fmt.Printf("  heap_build_delta_bytes: %d\n", int64(afterBuild)-int64(beforeBuild))
	fmt.Printf("  heap_open_delta_bytes: %d\n", int64(afterOpen)-int64(beforeOpen))
	fmt.Printf("  reader_blob_bytes: %d\n", reader.BlobSize())
	fmt.Printf("  query_total_ms: %d\n", queryElapsed.Milliseconds())
	fmt.Printf("  query_avg_ns: %d\n", queryElapsed.Nanoseconds()/int64(queries))
	fmt.Printf("  query_p50_ns: %d\n", percentile(latencies, 50))
	fmt.Printf("  query_p95_ns: %d\n", percentile(latencies, 95))
	fmt.Printf("  query_p99_ns: %d\n", percentile(latencies, 99))
	return nil
}

func buildStressInput(scopeCount int, principalsPerScope int) cherry.Input {
	providers := []cherry.Provider{{ID: "openai", Kind: "openai", Endpoint: "https://api.openai.com", SecretRef: "env://OPENAI_API_KEY"}}
	models := []cherry.Model{{ID: "gpt-4o-mini", Provider: "openai", Name: "gpt-4o-mini"}}
	mcpServers := []cherry.MCPServer{{ID: "github", Endpoint: "https://api.github.com"}, {ID: "kiwi", Endpoint: "https://mcp.kiwi.com"}}
	scopes := make([]cherry.Scope, 0, scopeCount)
	for scopeIndex := 0; scopeIndex < scopeCount; scopeIndex++ {
		scope := cherry.Scope{
			ID:         fmt.Sprintf("workspace%d", scopeIndex+1),
			Principals: make([]cherry.Principal, 0, principalsPerScope),
			MCPProfiles: []cherry.MCPProfile{{
				Path: "profile-dev-tools",
				Tools: []cherry.MCPToolBinding{
					{ExposedName: "github__list-repos", Server: "github", Tool: "list-repos"},
					{ExposedName: "kiwi__search-flight", Server: "kiwi", Tool: "search-flight"},
				},
			}},
		}
		for principalIndex := 0; principalIndex < principalsPerScope; principalIndex++ {
			scope.Principals = append(scope.Principals, cherry.Principal{
				Slug:  fmt.Sprintf("slug:%d:%d", scopeIndex+1, principalIndex+1),
				Route: cherry.RoutePlan{Provider: "openai", Model: "gpt-4o-mini"},
				Rate:  cherry.RatePolicy{USDPerDayCents: 50000, RPM: 300, OnExceed: "reject"},
			})
		}
		scopes = append(scopes, scope)
	}
	return cherry.Input{Providers: providers, Models: models, MCPServers: mcpServers, Scopes: scopes}
}

func printFixtureTree(fixture source.Fixture) {
	root := treeNode{Label: "org " + fixture.OrgID}
	for _, project := range fixture.Projects {
		projectNode := treeNode{Label: "project " + project.ID}
		for _, workspace := range workspacesForProject(fixture, project.ID) {
			workspaceNode := treeNode{Label: "workspace " + workspace.ID}
			for _, key := range fixture.Keys {
				if transform.KeySelectableInWorkspace(fixture, key, workspace) {
					workspaceNode.Children = append(workspaceNode.Children, treeNode{
						Label: fmt.Sprintf("key %s slug=%s owner=%s scope=%s tags=%s", key.ID, key.Slug, key.UserID, key.Scope, strings.Join(key.TagIDs, ",")),
					})
				}
			}
			for _, profile := range fixture.Profiles {
				if profile.WorkspaceID == workspace.ID {
					workspaceNode.Children = append(workspaceNode.Children, treeNode{
						Label: fmt.Sprintf("profile %s path=%s slug=%s", profile.ID, profile.Path, profile.Slug),
					})
				}
			}
			projectNode.Children = append(projectNode.Children, workspaceNode)
		}
		root.Children = append(root.Children, projectNode)
	}
	llmNode := treeNode{Label: "llm"}
	for _, model := range fixture.Models {
		llmNode.Children = append(llmNode.Children, treeNode{Label: fmt.Sprintf("model %s provider=%s", model.ID, model.Provider)})
	}
	root.Children = append(root.Children, llmNode)
	mcpNode := treeNode{Label: "mcp"}
	for _, server := range fixture.MCPServers {
		mcpNode.Children = append(mcpNode.Children, treeNode{Label: fmt.Sprintf("server %s tools=%s", server.ID, strings.Join(server.Tools, ","))})
	}
	root.Children = append(root.Children, mcpNode)
	printTree(root)
}

type treeNode struct {
	Label    string
	Children []treeNode
}

func printTree(root treeNode) {
	fmt.Println(root.Label)
	for i, child := range root.Children {
		printTreeNode(child, "", i == len(root.Children)-1)
	}
}

func printTreeNode(node treeNode, prefix string, last bool) {
	connector := "|- "
	nextPrefix := prefix + "|  "
	if last {
		connector = "`- "
		nextPrefix = prefix + "   "
	}
	fmt.Println(prefix + connector + node.Label)
	for i, child := range node.Children {
		printTreeNode(child, nextPrefix, i == len(node.Children)-1)
	}
}

func workspacesForProject(fixture source.Fixture, projectID string) []source.Workspace {
	workspaces := []source.Workspace{}
	for _, workspace := range fixture.Workspaces {
		if workspace.ProjectID == projectID {
			workspaces = append(workspaces, workspace)
		}
	}
	return workspaces
}

func printYAML(value any) {
	data, err := yaml.Marshal(value)
	if err != nil {
		fmt.Printf("marshal yaml: %v\n", err)
		return
	}
	fmt.Print(string(data))
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func heapAlloc() uint64 {
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return stats.HeapAlloc
}

func percentile(sortedValues []int64, p int) int64 {
	if len(sortedValues) == 0 {
		return 0
	}
	index := (len(sortedValues)*p + 99) / 100
	if index <= 0 {
		index = 1
	}
	if index > len(sortedValues) {
		index = len(sortedValues)
	}
	return sortedValues[index-1]
}
