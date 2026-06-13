package repl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	cherry "github.com/dio/cherry"
)

// Context describes the embedding runtime around a loaded Cherry bundle.
// Lane is the Orange snapshot lane; Scope is the Cherry enforcement scope.
type Context struct {
	Lane             string `json:"lane,omitempty" yaml:"lane,omitempty"`
	SnapshotVersion  uint64 `json:"snapshot_version,omitempty" yaml:"snapshot_version,omitempty"`
	SnapshotChecksum string `json:"snapshot_checksum,omitempty" yaml:"snapshot_checksum,omitempty"`
	Source           string `json:"source,omitempty" yaml:"source,omitempty"`
}

// Config configures a Session.
type Config struct {
	Backend      Backend
	DefaultScope string
	Context      Context
	Reload       func(context.Context) (Backend, Context, error)
}

// Result is the result of executing one REPL line.
type Result struct {
	Continue bool   `json:"continue"`
	Text     string `json:"text"`
	Scope    string `json:"scope,omitempty"`
	Lane     string `json:"lane,omitempty"`
	Data     any    `json:"data,omitempty"`
}

// Backend is the diagnostic/query surface used by the REPL command executor.
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

// Session holds the active REPL state for a caller.
type Session struct {
	backend Backend
	scope   string
	ctx     Context
	reload  func(context.Context) (Backend, Context, error)
}

// NewSession creates a REPL session.
func NewSession(cfg Config) (*Session, error) {
	if cfg.Backend == nil {
		return nil, errors.New("repl backend is required")
	}
	return &Session{
		backend: cfg.Backend,
		scope:   cfg.DefaultScope,
		ctx:     cfg.Context,
		reload:  cfg.Reload,
	}, nil
}

// ActiveScope returns the selected Cherry enforcement scope.
func (s *Session) ActiveScope() string {
	return s.scope
}

// SetScope selects the active Cherry enforcement scope.
func (s *Session) SetScope(ctx context.Context, scope string) error {
	scopes, err := s.backend.Scopes(ctx)
	if err != nil {
		return err
	}
	if !contains(scopes, scope) {
		return fmt.Errorf("scope %q is not in this bundle; scopes=%s", scope, strings.Join(scopes, ","))
	}
	s.scope = scope
	return nil
}

// Execute runs one command line.
func (s *Session) Execute(ctx context.Context, line string) (Result, error) {
	line = strings.TrimSpace(line)
	if line == "" {
		return s.result("", nil), nil
	}
	fields := strings.Fields(line)
	switch fields[0] {
	case "quit", "exit":
		return Result{Continue: false, Scope: s.scope, Lane: s.ctx.Lane}, nil
	case "help":
		return s.result(helpText(), nil), nil
	case "summary":
		return s.summary(ctx)
	case "scopes":
		scopes, err := s.backend.Scopes(ctx)
		if err != nil {
			return Result{}, err
		}
		return s.result(mustYAML(scopes), scopes), nil
	case "use":
		return s.use(ctx, fields)
	case "reload":
		return s.reloadCommand(ctx)
	case "inspect":
		return s.inspect(ctx, fields)
	case "llm":
		return s.llm(ctx, fields)
	case "mcp":
		return s.mcp(ctx, fields)
	default:
		return s.result(fmt.Sprintf("unknown command %q; type help\n", fields[0]), nil), nil
	}
}

func (s *Session) result(text string, data any) Result {
	return Result{Continue: true, Text: text, Scope: s.scope, Lane: s.ctx.Lane, Data: data}
}

func (s *Session) summary(ctx context.Context) (Result, error) {
	md, err := s.backend.Metadata(ctx)
	if err != nil {
		return Result{}, err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "pack: %s scope=%s:%s\n", md.FormatVersion, md.ScopeKind, md.ScopeID)
	if s.ctx.Lane != "" {
		fmt.Fprintf(&b, "  lane: %s\n", s.ctx.Lane)
	}
	if s.ctx.SnapshotVersion != 0 {
		fmt.Fprintf(&b, "  snapshot_version: %d\n", s.ctx.SnapshotVersion)
	}
	if s.ctx.SnapshotChecksum != "" {
		fmt.Fprintf(&b, "  snapshot_checksum: %s\n", s.ctx.SnapshotChecksum)
	}
	if s.ctx.Source != "" {
		fmt.Fprintf(&b, "  source: %s\n", s.ctx.Source)
	}
	fmt.Fprintf(&b, "  scopes: %s\n", strings.Join(md.Scopes, ", "))
	if s.scope != "" {
		fmt.Fprintf(&b, "  active_scope: %s\n", s.scope)
	}
	fmt.Fprintf(&b, "  raw_bytes: %d\n", md.PackManifest.SizeBytes)
	fmt.Fprintf(&b, "  manifest_version: %d\n", md.PackManifest.FormatVersion)
	fmt.Fprintf(&b, "  manifest_checksum: %d\n", md.PackManifest.Checksum)
	return s.result(b.String(), md), nil
}

func (s *Session) use(ctx context.Context, fields []string) (Result, error) {
	if len(fields) != 2 {
		return s.result("usage: use <scope>\n", nil), nil
	}
	scopes, err := s.backend.Scopes(ctx)
	if err != nil {
		return Result{}, err
	}
	if !contains(scopes, fields[1]) {
		return s.result(fmt.Sprintf("scope %q is not in this bundle; scopes=%s\n", fields[1], strings.Join(scopes, ",")), nil), nil
	}
	s.scope = fields[1]
	return s.result(fmt.Sprintf("using scope %s\n", s.scope), s.scope), nil
}

func (s *Session) reloadCommand(ctx context.Context) (Result, error) {
	if s.reload == nil {
		return s.result("reload is not configured\n", nil), nil
	}
	backend, replCtx, err := s.reload(ctx)
	if err != nil {
		return s.result(fmt.Sprintf("reload failed: %v\n", err), nil), nil
	}
	s.backend = backend
	s.ctx = replCtx
	scopes, err := s.backend.Scopes(ctx)
	if err != nil {
		return Result{}, err
	}
	if !contains(scopes, s.scope) {
		s.scope = defaultScope(scopes)
	}
	return s.result(fmt.Sprintf("reloaded scopes=%s\n", strings.Join(scopes, ",")), scopes), nil
}

func (s *Session) inspect(ctx context.Context, fields []string) (Result, error) {
	if len(fields) != 2 {
		return s.result("usage: inspect <metadata|principals|mcp|all>\n", nil), nil
	}
	switch fields[1] {
	case "metadata":
		md, err := s.backend.Metadata(ctx)
		if err != nil {
			return Result{}, err
		}
		return s.result(mustYAML(md), md), nil
	case "principals":
		scope, ok := s.activeScope()
		if !ok {
			return s.result("no active scope; run use <scope>\n", nil), nil
		}
		routes, err := s.backend.PrincipalRoutes(ctx, scope)
		if err != nil {
			return Result{}, err
		}
		return s.result(mustYAML(routes), routes), nil
	case "mcp":
		scope, ok := s.activeScope()
		if !ok {
			return s.result("no active scope; run use <scope>\n", nil), nil
		}
		paths, err := s.backend.MCPPaths(ctx, scope)
		if err != nil {
			return Result{}, err
		}
		return s.result(mustYAML(paths), paths), nil
	case "all":
		scope, ok := s.activeScope()
		if !ok {
			return s.result("no active scope; run use <scope>\n", nil), nil
		}
		md, err := s.backend.Metadata(ctx)
		if err != nil {
			return Result{}, err
		}
		routes, err := s.backend.PrincipalRoutes(ctx, scope)
		if err != nil {
			return Result{}, err
		}
		paths, err := s.backend.MCPPaths(ctx, scope)
		if err != nil {
			return Result{}, err
		}
		out := struct {
			Context    Context                 `yaml:"context"`
			Metadata   cherry.BundleMetadata   `yaml:"metadata"`
			Principals []cherry.PrincipalRoute `yaml:"principals"`
			MCP        []cherry.MCPPath        `yaml:"mcp"`
		}{s.ctx, md, routes, paths}
		return s.result(mustYAML(out), out), nil
	default:
		return s.result(fmt.Sprintf("unknown inspect area %q\n", fields[1]), nil), nil
	}
}

func (s *Session) llm(ctx context.Context, fields []string) (Result, error) {
	if len(fields) >= 2 {
		switch fields[1] {
		case "principals":
			return s.llmPrincipals(ctx, fields)
		case "providers":
			return s.llmProviders(ctx, fields)
		case "models":
			return s.llmModels(ctx, fields)
		case "model":
			return s.llmModel(ctx, fields)
		case "capability":
			return s.llmCapability(ctx, fields)
		}
	}
	if len(fields) != 3 && len(fields) != 4 {
		return s.result("usage: llm [scope] <principal-slug> <model> | llm principals [scope] | llm providers | llm models [--provider=name] | llm model <model> | llm capability <model> <capability>\n", nil), nil
	}
	scope, offset, ok, err := s.scopeAndOffset(ctx, fields, 3)
	if err != nil {
		return Result{}, err
	}
	if !ok {
		return s.result(scope, nil), nil
	}
	principalSlug := fields[offset]
	requestedModel := fields[offset+1]
	plan, found, err := s.backend.ResolveLLMPlan(ctx, scope, principalSlug, requestedModel)
	if err != nil {
		return Result{}, err
	}
	if !found {
		text := fmt.Sprintf("rejected: no LLM route for lane=%s scope=%s principal=%s model=%s\n", s.ctx.Lane, scope, principalSlug, requestedModel)
		hint, err := s.llmRejectHint(ctx, scope, principalSlug)
		if err != nil {
			return Result{}, err
		}
		return s.result(text+hint, nil), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "lane: %s\n", s.ctx.Lane)
	fmt.Fprintf(&b, "scope: %s\n", scope)
	fmt.Fprintf(&b, "principal: %s\n", principalSlug)
	fmt.Fprintf(&b, "requested_model: %s\n", requestedModel)
	b.WriteString("route_plan:\n")
	printLLMRoutePlan(&b, plan.Plan, 2)
	fmt.Fprintf(&b, "rate_limit: usd_per_day=%.2f rpm=%d on_exceed=%s\n", float64(plan.Rate.USDPerDayCents)/100, plan.Rate.RPM, plan.Rate.OnExceed)
	return s.result(b.String(), plan), nil
}

func (s *Session) llmRejectHint(ctx context.Context, scope string, principalSlug string) (string, error) {
	principals, err := s.backend.LLMPrincipals(ctx, scope)
	if err != nil || len(principals) == 0 {
		return "", err
	}
	var b strings.Builder
	b.WriteString("available_principals:\n")
	for _, principal := range principals {
		fmt.Fprintf(&b, "  - %s\n", principal.PrincipalSlug)
	}
	if !strings.HasPrefix(principalSlug, "slug:") {
		b.WriteString("hint: pass the verified principal slug from the bundle, not the source key id; use `llm principals` to list slugs\n")
	}
	scopes, err := s.backend.Scopes(ctx)
	if err != nil {
		return "", err
	}
	if len(scopes) > 1 {
		fmt.Fprintf(&b, "hint: active scope is %s; bundle scopes=%s\n", scope, strings.Join(scopes, ","))
	}
	return b.String(), nil
}

func printLLMRoutePlan(b *strings.Builder, plan cherry.LLMRoutePlan, indent int) {
	pad := strings.Repeat(" ", indent)
	switch plan.Kind {
	case cherry.RouteKindTarget:
		fmt.Fprintf(b, "%starget:\n", pad)
		fmt.Fprintf(
			b,
			"%s  provider=%s kind=%s model=%s model_name=%s endpoint=%s secret_ref=%s\n",
			pad,
			plan.Provider,
			plan.ProviderKind,
			plan.Model,
			plan.ModelName,
			plan.Endpoint,
			plan.SecretRef,
		)
	case cherry.RouteKindChain:
		fmt.Fprintf(b, "%schain:\n", pad)
		if plan.RetryOn != "" || plan.PerTryTimeoutMS != 0 {
			fmt.Fprintf(b, "%s  retry_on: %s\n", pad, plan.RetryOn)
			fmt.Fprintf(b, "%s  per_try_timeout_ms: %d\n", pad, plan.PerTryTimeoutMS)
		}
		fmt.Fprintf(b, "%s  children:\n", pad)
		for _, child := range plan.Children {
			fmt.Fprintf(b, "%s    -\n", pad)
			printLLMRoutePlan(b, child.Plan, indent+6)
		}
	case cherry.RouteKindSplit:
		fmt.Fprintf(b, "%ssplit:\n", pad)
		fmt.Fprintf(b, "%s  children:\n", pad)
		for _, child := range plan.Children {
			fmt.Fprintf(b, "%s    - weight: %d\n", pad, child.Weight)
			printLLMRoutePlan(b, child.Plan, indent+6)
		}
	default:
		fmt.Fprintf(b, "%sunknown: %s\n", pad, plan.Kind)
	}
}

func (s *Session) llmPrincipals(ctx context.Context, fields []string) (Result, error) {
	scope, ok, err := s.optionalScope(ctx, fields, "llm principals [scope]")
	if err != nil {
		return Result{}, err
	}
	if !ok {
		return s.result(scope, nil), nil
	}
	principals, err := s.backend.LLMPrincipals(ctx, scope)
	if err != nil {
		return Result{}, err
	}
	type principalSummary struct {
		ScopeID             string `yaml:"scope_id" json:"scope_id"`
		PrincipalSlug       string `yaml:"principal_slug" json:"principal_slug"`
		RequestedModelCount int    `yaml:"requested_model_count" json:"requested_model_count"`
	}
	summaries := make([]principalSummary, 0, len(principals))
	for _, principal := range principals {
		summaries = append(summaries, principalSummary{
			ScopeID:             principal.ScopeID,
			PrincipalSlug:       principal.PrincipalSlug,
			RequestedModelCount: len(principal.RequestedModels),
		})
	}
	return s.result(mustYAML(summaries), summaries), nil
}

func (s *Session) llmProviders(ctx context.Context, fields []string) (Result, error) {
	if len(fields) != 2 {
		return s.result("usage: llm providers\n", nil), nil
	}
	providers, err := s.backend.Providers(ctx)
	if err != nil {
		return Result{}, err
	}
	return s.result(mustYAML(providers), providers), nil
}

func (s *Session) llmModels(ctx context.Context, fields []string) (Result, error) {
	providerID, ok := parseProviderOnlyArgs(fields[2:])
	if !ok {
		return s.result("usage: llm models [--provider=<provider>]\n", nil), nil
	}
	payload, err := s.backend.V1ModelsJSON(ctx, providerID)
	if err != nil {
		return Result{}, err
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, payload, "", "  "); err != nil {
		return Result{}, err
	}
	return s.result(pretty.String()+"\n", json.RawMessage(payload)), nil
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

func (s *Session) llmModel(ctx context.Context, fields []string) (Result, error) {
	modelID, providerID, ok := parseLLMModelArgs(fields[2:])
	if !ok {
		return s.result("usage: llm model <model> [--provider=<provider>] | llm model --provider=<provider>\n", nil), nil
	}
	if providerID != "" && modelID == "" {
		models, err := s.backend.Models(ctx)
		if err != nil {
			return Result{}, err
		}
		filtered := modelsForProvider(models, providerID)
		if len(filtered) == 0 {
			return s.result(fmt.Sprintf("no models for provider %q\n", providerID), nil), nil
		}
		return s.result(mustYAML(filtered), filtered), nil
	}
	model, found, err := s.backend.ResolveModel(ctx, modelID)
	if err != nil {
		return Result{}, err
	}
	if !found {
		return s.result(fmt.Sprintf("unknown model %q\n", modelID), nil), nil
	}
	if providerID != "" && model.Provider != providerID {
		return s.result(fmt.Sprintf("model %q belongs to provider %q, not %q\n", modelID, model.Provider, providerID), nil), nil
	}
	return s.result(mustYAML(model), model), nil
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

func (s *Session) llmCapability(ctx context.Context, fields []string) (Result, error) {
	if len(fields) != 4 {
		return s.result("usage: llm capability <model> <capability>\n", nil), nil
	}
	supported, err := s.backend.ModelCapability(ctx, fields[2], fields[3])
	if err != nil {
		return Result{}, err
	}
	out := struct {
		Model      string `yaml:"model" json:"model"`
		Capability string `yaml:"capability" json:"capability"`
		Supported  bool   `yaml:"supported" json:"supported"`
	}{fields[2], fields[3], supported}
	return s.result(mustYAML(out), out), nil
}

func (s *Session) mcp(ctx context.Context, fields []string) (Result, error) {
	if len(fields) < 2 {
		return s.result("usage: mcp [scope] <path> [tool] | mcp <paths|initialize|list|call> [scope] <path|profile=name|server=name> [tool]\n", nil), nil
	}
	switch fields[1] {
	case "paths":
		return s.mcpPaths(ctx, fields)
	case "initialize":
		return s.mcpInitialize(ctx, fields)
	case "list":
		return s.mcpList(ctx, fields)
	case "call":
		return s.mcpCall(ctx, fields)
	}
	if len(fields) > 4 {
		return s.result("usage: mcp [scope] <path> [tool]\n", nil), nil
	}
	scope := s.scope
	pathOffset := 1
	scopes, err := s.backend.Scopes(ctx)
	if err != nil {
		return Result{}, err
	}
	if len(fields) >= 3 && contains(scopes, fields[1]) {
		scope = fields[1]
		pathOffset = 2
	}
	if scope == "" {
		return s.result("no active scope; run use <scope>\n", nil), nil
	}
	path := normalizeMCPTarget(fields[pathOffset])
	if len(fields) > pathOffset+1 {
		toolName := fields[pathOffset+1]
		tool, found, err := s.backend.ResolveMCPTool(ctx, scope, path, toolName)
		if err != nil {
			return Result{}, err
		}
		if !found {
			return s.result(fmt.Sprintf("rejected: no MCP tool for lane=%s scope=%s path=%s tool=%s\n", s.ctx.Lane, scope, path, toolName), nil), nil
		}
		return s.result(formatMCPTool(s.ctx.Lane, scope, path, "tool", toolName, tool), tool), nil
	}
	result, found, err := s.backend.ResolveMCP(ctx, scope, path)
	if err != nil {
		return Result{}, err
	}
	if !found {
		return s.result(fmt.Sprintf("rejected: no MCP path for lane=%s scope=%s path=%s\n", s.ctx.Lane, scope, path), nil), nil
	}
	return s.result(formatMCPResult(s.ctx.Lane, scope, result), result), nil
}

func (s *Session) mcpPaths(ctx context.Context, fields []string) (Result, error) {
	scope, showTools, ok, err := s.optionalScopeWithTools(ctx, fields, "mcp paths [scope] [--tools]")
	if err != nil {
		return Result{}, err
	}
	if !ok {
		return s.result(scope, nil), nil
	}
	paths, err := s.backend.MCPPaths(ctx, scope)
	if err != nil {
		return Result{}, err
	}
	type pathSummary struct {
		ScopeID   string   `yaml:"scope_id" json:"scope_id"`
		Path      string   `yaml:"path" json:"path"`
		ToolCount int      `yaml:"tool_count" json:"tool_count"`
		Tools     []string `yaml:"tools,omitempty" json:"tools,omitempty"`
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
	return s.result(mustYAML(summaries), summaries), nil
}

func (s *Session) mcpInitialize(ctx context.Context, fields []string) (Result, error) {
	scope, path, ok, err := s.mcpCommandScopeAndPath(ctx, fields, "mcp initialize [scope] <path|profile=name|server=name>")
	if err != nil {
		return Result{}, err
	}
	if !ok {
		return s.result(scope, nil), nil
	}
	result, found, err := s.backend.ResolveMCPInitialize(ctx, scope, path)
	if err != nil {
		return Result{}, err
	}
	if !found {
		return s.result(fmt.Sprintf("rejected: no MCP path for lane=%s scope=%s path=%s\n", s.ctx.Lane, scope, path), nil), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "lane: %s\n", s.ctx.Lane)
	fmt.Fprintf(&b, "scope: %s\n", scope)
	fmt.Fprintf(&b, "path: %s\n", result.Path)
	b.WriteString("initialize_servers:\n")
	for _, server := range result.Servers {
		fmt.Fprintf(&b, "  server=%s endpoint=%s auth_type=%s secret_ref=%s\n", server.Server, server.Endpoint, server.AuthType, server.SecretRef)
	}
	return s.result(b.String(), result), nil
}

func (s *Session) mcpList(ctx context.Context, fields []string) (Result, error) {
	scope, path, ok, err := s.mcpCommandScopeAndPath(ctx, fields, "mcp list [scope] <path|profile=name|server=name>")
	if err != nil {
		return Result{}, err
	}
	if !ok {
		return s.result(scope, nil), nil
	}
	result, found, err := s.backend.ResolveMCP(ctx, scope, path)
	if err != nil {
		return Result{}, err
	}
	if !found {
		return s.result(fmt.Sprintf("rejected: no MCP path for lane=%s scope=%s path=%s\n", s.ctx.Lane, scope, path), nil), nil
	}
	return s.result(formatMCPResult(s.ctx.Lane, scope, result), result), nil
}

func (s *Session) mcpCall(ctx context.Context, fields []string) (Result, error) {
	if len(fields) != 4 && len(fields) != 5 {
		return s.result("usage: mcp call [scope] <path|profile=name|server=name> <tool>\n", nil), nil
	}
	scope := s.scope
	pathOffset := 2
	if len(fields) == 5 {
		scopes, err := s.backend.Scopes(ctx)
		if err != nil {
			return Result{}, err
		}
		if !contains(scopes, fields[2]) {
			return s.result(fmt.Sprintf("scope %q is not in this bundle\n", fields[2]), nil), nil
		}
		scope = fields[2]
		pathOffset = 3
	}
	if scope == "" {
		return s.result("no active scope; run use <scope>\n", nil), nil
	}
	path := normalizeMCPTarget(fields[pathOffset])
	toolName := fields[pathOffset+1]
	tool, found, err := s.backend.ResolveMCPTool(ctx, scope, path, toolName)
	if err != nil {
		return Result{}, err
	}
	if !found {
		return s.result(fmt.Sprintf("rejected: no MCP tool for lane=%s scope=%s path=%s tool=%s\n", s.ctx.Lane, scope, path, toolName), nil), nil
	}
	return s.result(formatMCPTool(s.ctx.Lane, scope, path, "call", toolName, tool), tool), nil
}

func formatMCPResult(lane string, scope string, result cherry.MCPResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "lane: %s\n", lane)
	fmt.Fprintf(&b, "scope: %s\n", scope)
	fmt.Fprintf(&b, "path: %s\n", result.Path)
	b.WriteString("tools:\n")
	for _, tool := range result.Tools {
		fmt.Fprintf(&b, "  %s -> server=%s endpoint=%s upstream_tool=%s auth_type=%s secret_ref=%s\n", tool.ExposedName, tool.Server, tool.ServerEndpoint, tool.Tool, tool.AuthType, tool.SecretRef)
	}
	return b.String()
}

func formatMCPTool(lane string, scope string, path string, label string, toolName string, tool cherry.MCPTool) string {
	return fmt.Sprintf("lane: %s\nscope: %s\npath: %s\n%s: %s -> server=%s endpoint=%s upstream_tool=%s auth_type=%s secret_ref=%s\n",
		lane,
		scope,
		path,
		label,
		toolName,
		tool.Server,
		tool.ServerEndpoint,
		tool.Tool,
		tool.AuthType,
		tool.SecretRef,
	)
}

func (s *Session) mcpCommandScopeAndPath(ctx context.Context, fields []string, usage string) (string, string, bool, error) {
	if len(fields) != 3 && len(fields) != 4 {
		return "usage: " + usage + "\n", "", false, nil
	}
	scope := s.scope
	pathOffset := 2
	if len(fields) == 4 {
		scopes, err := s.backend.Scopes(ctx)
		if err != nil {
			return "", "", false, err
		}
		if !contains(scopes, fields[2]) {
			return fmt.Sprintf("scope %q is not in this bundle\n", fields[2]), "", false, nil
		}
		scope = fields[2]
		pathOffset = 3
	}
	if scope == "" {
		return "no active scope; run use <scope>\n", "", false, nil
	}
	return scope, normalizeMCPTarget(fields[pathOffset]), true, nil
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

func (s *Session) activeScope() (string, bool) {
	if s.scope == "" {
		return "", false
	}
	return s.scope, true
}

func (s *Session) scopeAndOffset(ctx context.Context, fields []string, noScopeLen int) (string, int, bool, error) {
	if len(fields) == noScopeLen {
		if s.scope == "" {
			return "no active scope; run use <scope>\n", 0, false, nil
		}
		return s.scope, 1, true, nil
	}
	scopes, err := s.backend.Scopes(ctx)
	if err != nil {
		return "", 0, false, err
	}
	if !contains(scopes, fields[1]) {
		return fmt.Sprintf("scope %q is not in this bundle\n", fields[1]), 0, false, nil
	}
	return fields[1], 2, true, nil
}

func (s *Session) optionalScope(ctx context.Context, fields []string, usage string) (string, bool, error) {
	if len(fields) != 2 && len(fields) != 3 {
		return "usage: " + usage + "\n", false, nil
	}
	if len(fields) == 2 {
		if s.scope == "" {
			return "no active scope; run use <scope>\n", false, nil
		}
		return s.scope, true, nil
	}
	scopes, err := s.backend.Scopes(ctx)
	if err != nil {
		return "", false, err
	}
	if !contains(scopes, fields[2]) {
		return fmt.Sprintf("scope %q is not in this bundle\n", fields[2]), false, nil
	}
	return fields[2], true, nil
}

func (s *Session) optionalScopeWithTools(ctx context.Context, fields []string, usage string) (string, bool, bool, error) {
	scope := s.scope
	showTools := false
	if len(fields) < 2 || len(fields) > 4 {
		return "usage: " + usage + "\n", false, false, nil
	}
	scopes, err := s.backend.Scopes(ctx)
	if err != nil {
		return "", false, false, err
	}
	for _, field := range fields[2:] {
		if field == "--tools" {
			showTools = true
			continue
		}
		if scope != "" && scope != s.scope {
			return "usage: " + usage + "\n", false, false, nil
		}
		if !contains(scopes, field) {
			return fmt.Sprintf("scope %q is not in this bundle\n", field), false, false, nil
		}
		scope = field
	}
	if scope == "" {
		return "no active scope; run use <scope>\n", false, false, nil
	}
	return scope, showTools, true, nil
}

func helpText() string {
	return strings.Join([]string{
		"commands:",
		"  summary                         print bundle, lane, and scope metadata",
		"  scopes                          list Cherry scopes in the loaded bundle",
		"  use <scope>                     select the active Cherry enforcement scope",
		"  llm [scope] <slug> <model>      resolve an LLM request",
		"  llm principals [scope]          list LLM principal slugs and model counts",
		"  llm providers                   list packed LLM providers",
		"  llm models [--provider=name]    print simulated /v1/models catalog",
		"  llm model <model>               inspect one packed model",
		"  mcp paths [scope] [--tools]     list MCP paths, optionally with tool names",
		"  mcp initialize [scope] <target> resolve upstream MCP servers for initialize",
		"  mcp list [scope] <target>       list exposed MCP tools for a target",
		"  mcp call [scope] <target> <tool> resolve one MCP tool call",
		"  mcp [scope] <path> [tool]       compatibility form for list or call",
		"  inspect metadata                print bundle metadata",
		"  inspect principals              dump principal/model routes for active scope",
		"  inspect mcp                     dump MCP paths and tool bindings for active scope",
		"  inspect all                     print all inspectable data for active scope",
		"  reload                          reload when the embedding app configured it",
		"  quit                            exit",
		"",
	}, "\n")
}

// LocalBackend adapts a Cherry opened bundle to the REPL backend interface.
type LocalBackend struct {
	opened cherry.OpenedBundle
}

// NewLocalBackend creates a backend for an opened bundle.
func NewLocalBackend(opened cherry.OpenedBundle) LocalBackend {
	return LocalBackend{opened: opened}
}

// OpenLocalBundle reads and opens a zstd Cherry bundle from disk.
func OpenLocalBundle(path string) (LocalBackend, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return LocalBackend{}, err
	}
	opened, err := cherry.OpenBundleZstd(data)
	if err != nil {
		return LocalBackend{}, err
	}
	return NewLocalBackend(opened), nil
}

func (b LocalBackend) Metadata(context.Context) (cherry.BundleMetadata, error) {
	return b.opened.Metadata, nil
}

func (b LocalBackend) Scopes(context.Context) ([]string, error) {
	scopes := b.opened.Reader.ScopeIDs()
	return scopes, nil
}

func (b LocalBackend) LLMPrincipals(_ context.Context, scope string) ([]cherry.PrincipalInfo, error) {
	principals, ok := b.opened.Reader.Principals(scope)
	if !ok {
		return nil, fmt.Errorf("unknown scope %q", scope)
	}
	return principals, nil
}

func (b LocalBackend) ResolveLLMPlan(_ context.Context, scope string, principalSlug string, modelID string) (cherry.LLMPlan, bool, error) {
	plan, ok := b.opened.Reader.ResolveLLMPlan(scope, principalSlug, modelID)
	return plan, ok, nil
}

func (b LocalBackend) Providers(context.Context) ([]cherry.ProviderInfo, error) {
	return b.opened.Reader.Providers(), nil
}

func (b LocalBackend) Models(context.Context) ([]cherry.ModelInfo, error) {
	return b.opened.Reader.Models(), nil
}

func (b LocalBackend) ResolveModel(_ context.Context, modelID string) (cherry.ModelInfo, bool, error) {
	model, ok := b.opened.Reader.ResolveModel(modelID)
	return model, ok, nil
}

func (b LocalBackend) ModelCapability(_ context.Context, modelID string, capability string) (bool, error) {
	return b.opened.Reader.ModelCapability(modelID, capability), nil
}

func (b LocalBackend) V1ModelsJSON(_ context.Context, providerID string) ([]byte, error) {
	if providerID == "" {
		return b.opened.Reader.V1ModelsJSON()
	}
	return b.opened.Reader.V1ModelsJSONForProvider(providerID)
}

func (b LocalBackend) MCPPaths(_ context.Context, scope string) ([]cherry.MCPPath, error) {
	paths, ok := b.opened.Reader.MCPPaths(scope)
	if !ok {
		return nil, fmt.Errorf("unknown scope %q", scope)
	}
	return paths, nil
}

func (b LocalBackend) ResolveMCP(_ context.Context, scope string, path string) (cherry.MCPResult, bool, error) {
	result, ok := b.opened.Reader.ResolveMCP(scope, path)
	return result, ok, nil
}

func (b LocalBackend) ResolveMCPInitialize(_ context.Context, scope string, path string) (cherry.MCPInitializeResult, bool, error) {
	result, ok := b.opened.Reader.ResolveMCPInitialize(scope, path)
	return result, ok, nil
}

func (b LocalBackend) ResolveMCPTool(_ context.Context, scope string, path string, exposedTool string) (cherry.MCPTool, bool, error) {
	toolIDs, ok := b.opened.Reader.ResolveMCPToolIDs(scope, path, exposedTool)
	if !ok {
		return cherry.MCPTool{}, false, nil
	}
	return cherry.MCPTool{
		ExposedName:    b.opened.Reader.String(toolIDs.ExposedNameSID),
		Server:         b.opened.Reader.String(toolIDs.ServerSID),
		ServerEndpoint: b.opened.Reader.String(toolIDs.ServerEndpointSID),
		Tool:           b.opened.Reader.String(toolIDs.ToolSID),
		SecretRef:      b.opened.Reader.String(toolIDs.SecretSID),
		AuthType:       b.opened.Reader.String(toolIDs.AuthTypeSID),
	}, true, nil
}

func (b LocalBackend) PrincipalRoutes(_ context.Context, scope string) ([]cherry.PrincipalRoute, error) {
	routes, ok := b.opened.Reader.PrincipalRoutes(scope)
	if !ok {
		return nil, fmt.Errorf("unknown scope %q", scope)
	}
	return routes, nil
}

func mustYAML(value any) string {
	data, err := yaml.Marshal(value)
	if err != nil {
		return fmt.Sprintf("marshal yaml: %v\n", err)
	}
	return string(data)
}

func defaultScope(scopes []string) string {
	if len(scopes) == 1 {
		return scopes[0]
	}
	return ""
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
