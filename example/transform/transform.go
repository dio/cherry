package transform

import (
	"fmt"
	"math"
	"reflect"
	"sort"

	cherry "github.com/dio/cherry"
	"github.com/dio/cherry/example/source"
)

type ScopeKind string

const (
	ScopeKindWorkspace ScopeKind = "workspace"
	ScopeKindProject   ScopeKind = "project"
)

type Selection struct {
	Kind ScopeKind
	ID   string
}

type Result struct {
	Selection Selection
	Input     cherry.Input
	Scopes    []string
}

func BuildPackInput(fixture source.Fixture, selection Selection) (Result, error) {
	workspaceIDs, err := workspaceIDsForSelection(fixture, selection)
	if err != nil {
		return Result{}, err
	}

	scopes := make([]cherry.Scope, 0, len(workspaceIDs))
	for _, workspaceID := range workspaceIDs {
		scope, err := buildScope(fixture, workspaceID)
		if err != nil {
			return Result{}, err
		}
		scopes = append(scopes, scope)
	}

	return Result{
		Selection: selection,
		Input: cherry.Input{
			Providers:  packProviders(fixture),
			Models:     fixture.Models,
			MCPServers: packMCPServers(fixture),
			Scopes:     scopes,
		},
		Scopes: workspaceIDs,
	}, nil
}

func buildScope(fixture source.Fixture, workspaceID string) (cherry.Scope, error) {
	workspace, ok := FindWorkspace(fixture, workspaceID)
	if !ok {
		return cherry.Scope{}, fmt.Errorf("unknown workspace %q", workspaceID)
	}

	users := map[string]source.User{}
	for _, user := range fixture.Users {
		users[user.ID] = user
	}

	scope := cherry.Scope{
		ID:          workspaceID,
		Principals:  []cherry.Principal{},
		MCPProfiles: []cherry.MCPProfile{},
	}
	for _, key := range fixture.Keys {
		if !KeySelectableInWorkspace(fixture, key, workspace) {
			continue
		}
		user, ok := users[key.UserID]
		if !ok {
			return cherry.Scope{}, fmt.Errorf("key %q references unknown user %q", key.ID, key.UserID)
		}
		principal, err := principalForKey(fixture, key, user)
		if err != nil {
			return cherry.Scope{}, fmt.Errorf("key %q: %w", key.ID, err)
		}
		scope.Principals = append(scope.Principals, principal)
	}
	sort.Slice(scope.Principals, func(i, j int) bool {
		return scope.Principals[i].Slug < scope.Principals[j].Slug
	})

	for _, server := range fixture.MCPServers {
		tools := make([]cherry.MCPToolBinding, 0, len(server.Tools))
		for _, tool := range server.Tools {
			tools = append(tools, cherry.MCPToolBinding{
				ExposedName: server.ID + "__" + tool,
				Server:      server.ID,
				Tool:        tool,
				SecretRef:   server.SecretRef,
				AuthType:    server.AuthType,
			})
		}
		scope.MCPProfiles = append(scope.MCPProfiles, cherry.MCPProfile{
			Path:  "s/" + server.ID,
			Tools: tools,
		})
	}

	for _, profile := range fixture.Profiles {
		if profile.WorkspaceID != workspaceID {
			continue
		}
		tools, err := profileTools(fixture, profile)
		if err != nil {
			return cherry.Scope{}, fmt.Errorf("profile %q: %w", profile.ID, err)
		}
		scope.MCPProfiles = append(scope.MCPProfiles, cherry.MCPProfile{
			Path:  profile.Path,
			Tools: tools,
		})
	}
	sort.Slice(scope.MCPProfiles, func(i, j int) bool {
		return scope.MCPProfiles[i].Path < scope.MCPProfiles[j].Path
	})
	return scope, nil
}

func principalForKey(fixture source.Fixture, key source.Key, user source.User) (cherry.Principal, error) {
	rules := []source.Rule{orgDefaultRule()}
	for _, tagID := range user.TagIDs {
		if tag, ok := fixture.Tags[tagID]; ok {
			rules = append(rules, tag.LLMRule)
		}
	}
	for _, tagID := range key.TagIDs {
		if tag, ok := fixture.Tags[tagID]; ok {
			rules = append(rules, tag.LLMRule)
		}
	}
	rules = append(rules, source.Rule{
		ID:          "key:" + key.ID,
		Specificity: source.SpecificityKey,
		Overrides: map[string]source.RouteNode{
			"gpt-4o-mini": target("openai", "gpt-4o-mini-2", ""),
		},
	})

	modelRoutes := map[string]cherry.RoutePlan{}
	for _, model := range fixture.Models {
		route, err := resolveModelRoute(fixture, rules, model.ID)
		if err != nil {
			return cherry.Principal{}, err
		}
		modelRoutes[model.ID] = route
	}
	limit, err := resolveRateLimit(rules)
	if err != nil {
		return cherry.Principal{}, err
	}
	return cherry.Principal{
		Slug:        key.Slug,
		ModelRoutes: modelRoutes,
		Rate:        limit,
	}, nil
}

func resolveModelRoute(fixture source.Fixture, rules []source.Rule, requestedModel string) (cherry.RoutePlan, error) {
	model, ok := FindModel(fixture, requestedModel)
	if !ok {
		return cherry.RoutePlan{}, fmt.Errorf("unknown requested model %q", requestedModel)
	}
	base := cherry.RoutePlan{Kind: cherry.RouteKindTarget, Provider: model.Provider, Model: model.ID}
	baseProvider, _ := FindProvider(fixture, model.Provider)
	base.SecretRef = baseProvider.SecretRef

	modelNode, err := highestOverride(rules, requestedModel)
	if err != nil {
		return cherry.RoutePlan{}, err
	}
	if modelNode != nil {
		return routeFromNode(fixture, *modelNode, base)
	}

	providerNode, err := highestOverride(rules, "@"+model.Provider, "provider:"+model.Provider)
	if err != nil {
		return cherry.RoutePlan{}, err
	}
	if providerNode != nil {
		return routeFromNode(fixture, *providerNode, base)
	}
	return base, nil
}

func highestOverride(rules []source.Rule, keys ...string) (*source.RouteNode, error) {
	var found *source.RouteNode
	var specificity source.Specificity
	for _, rule := range rules {
		overrides := ruleOverrides(rule)
		for _, key := range keys {
			node, ok := overrides[key]
			if !ok {
				continue
			}
			if err := validateNode(node); err != nil {
				return nil, fmt.Errorf("rule %q override %q: %w", rule.ID, key, err)
			}
			if found != nil && rule.Specificity == specificity && !reflect.DeepEqual(*found, node) {
				return nil, fmt.Errorf("conflicting same-specificity override for %q", key)
			}
			if found == nil || rule.Specificity >= specificity {
				copy := node
				found = &copy
				specificity = rule.Specificity
			}
			break
		}
	}
	return found, nil
}

func ruleOverrides(rule source.Rule) map[string]source.RouteNode {
	if len(rule.RoutingOverrides) == 0 {
		return rule.Overrides
	}
	if len(rule.Overrides) == 0 {
		return rule.RoutingOverrides
	}
	merged := map[string]source.RouteNode{}
	for key, value := range rule.Overrides {
		merged[key] = value
	}
	for key, value := range rule.RoutingOverrides {
		merged[key] = value
	}
	return merged
}

func routeFromNode(fixture source.Fixture, node source.RouteNode, base cherry.RoutePlan) (cherry.RoutePlan, error) {
	if err := validateNode(node); err != nil {
		return cherry.RoutePlan{}, err
	}
	switch node.Kind {
	case "target":
		return routeFromTarget(fixture, node, base)
	case "chain":
		return routeFromChain(fixture, node, base)
	case "split":
		return routeFromSplit(fixture, node, base)
	default:
		return cherry.RoutePlan{}, fmt.Errorf("unsupported route node kind %q", node.Kind)
	}
}

func routeFromTarget(fixture source.Fixture, node source.RouteNode, base cherry.RoutePlan) (cherry.RoutePlan, error) {
	route := base
	route.Kind = cherry.RouteKindTarget
	route.Retry = nil
	route.Children = nil
	route.Split = nil
	providerChanged := false
	if node.Target.Provider != "" {
		providerChanged = node.Target.Provider != route.Provider
		route.Provider = node.Target.Provider
	}
	if node.Target.Model != "" {
		route.Model = node.Target.Model
	}
	if node.Target.Model == "" && node.Target.Name != "" {
		route.Model = node.Target.Name
	}
	if node.Target.SecretRef != "" {
		route.SecretRef = node.Target.SecretRef
	}
	if _, ok := FindProvider(fixture, route.Provider); !ok {
		return cherry.RoutePlan{}, fmt.Errorf("unknown provider %q", route.Provider)
	}
	if _, ok := FindModel(fixture, route.Model); !ok {
		return cherry.RoutePlan{}, fmt.Errorf("unknown model %q", route.Model)
	}
	if route.SecretRef == "" || (node.Target.SecretRef == "" && providerChanged) {
		provider, _ := FindProvider(fixture, route.Provider)
		route.SecretRef = provider.SecretRef
	}
	return route, nil
}

func routeFromChain(fixture source.Fixture, node source.RouteNode, base cherry.RoutePlan) (cherry.RoutePlan, error) {
	if len(node.Chain) == 0 {
		return cherry.RoutePlan{}, fmt.Errorf("chain route node must not be empty")
	}
	route := cherry.RoutePlan{Kind: cherry.RouteKindChain}
	if node.Retry != nil {
		route.Retry = &cherry.RetryPolicy{
			RetryOn:         node.Retry.RetryOn,
			PerTryTimeoutMS: uint32(node.Retry.PerTryTimeoutMS),
		}
	}
	for index, child := range node.Chain {
		compiled, err := routeFromNode(fixture, child, base)
		if err != nil {
			return cherry.RoutePlan{}, fmt.Errorf("chain[%d]: %w", index, err)
		}
		route.Children = append(route.Children, compiled)
	}
	return route, nil
}

func routeFromSplit(fixture source.Fixture, node source.RouteNode, base cherry.RoutePlan) (cherry.RoutePlan, error) {
	if len(node.Split) == 0 {
		return cherry.RoutePlan{}, fmt.Errorf("split route node must not be empty")
	}
	route := cherry.RoutePlan{Kind: cherry.RouteKindSplit}
	for index, weighted := range node.Split {
		compiled, err := routeFromNode(fixture, weighted.Node, base)
		if err != nil {
			return cherry.RoutePlan{}, fmt.Errorf("split[%d]: %w", index, err)
		}
		if weighted.Weight <= 0 {
			return cherry.RoutePlan{}, fmt.Errorf("split[%d]: weight must be positive", index)
		}
		route.Split = append(route.Split, cherry.WeightedRoutePlan{
			Weight: uint32(weighted.Weight),
			Plan:   compiled,
		})
	}
	return route, nil
}

func resolveRateLimit(rules []source.Rule) (cherry.RatePolicy, error) {
	var found *source.RateLimitPolicy
	var specificity source.Specificity
	for _, rule := range rules {
		if rule.RateLimit == nil {
			continue
		}
		if found != nil && rule.Specificity == specificity && !reflect.DeepEqual(*found, *rule.RateLimit) {
			return cherry.RatePolicy{}, fmt.Errorf("conflicting same-specificity rate limit")
		}
		if found == nil || rule.Specificity >= specificity {
			copy := *rule.RateLimit
			found = &copy
			specificity = rule.Specificity
		}
	}
	if found == nil {
		return cherry.RatePolicy{}, nil
	}
	return cherry.RatePolicy{
		USDPerDayCents: uint64(math.Round(found.USDPerDay * 100)),
		RPM:            uint32(found.RPM),
		OnExceed:       found.OnExceed,
	}, nil
}

func profileTools(fixture source.Fixture, profile source.Profile) ([]cherry.MCPToolBinding, error) {
	tools := []cherry.MCPToolBinding{}
	for _, serverID := range sortedKeys(profile.Tools) {
		spec := profile.Tools[serverID]
		server, ok := FindMCPServer(fixture, serverID)
		if !ok {
			return nil, fmt.Errorf("unknown mcp server %q", serverID)
		}
		available := map[string]bool{}
		for _, tool := range server.Tools {
			available[tool] = true
		}
		for _, tool := range spec.Include {
			if !available[tool] {
				return nil, fmt.Errorf("unknown mcp tool %q on server %q", tool, serverID)
			}
			authType, secretRef := effectiveMCPAuth(server, profile.Auth, spec.Auth)
			tools = append(tools, cherry.MCPToolBinding{
				ExposedName: serverID + "__" + tool,
				Server:      serverID,
				Tool:        tool,
				SecretRef:   secretRef,
				AuthType:    authType,
			})
		}
	}
	return tools, nil
}

func effectiveMCPAuth(server source.MCPServer, profileAuth source.AuthConfig, toolAuth source.AuthConfig) (string, string) {
	authType := server.AuthType
	secretRef := server.SecretRef
	if profileAuth.Type != "" {
		authType = profileAuth.Type
		if profileAuth.Type == "none" {
			secretRef = ""
		}
	}
	if profileAuth.SecretRef != "" {
		secretRef = profileAuth.SecretRef
	}
	if toolAuth.Type != "" {
		authType = toolAuth.Type
		if toolAuth.Type == "none" {
			secretRef = ""
		}
	}
	if toolAuth.SecretRef != "" {
		secretRef = toolAuth.SecretRef
	}
	return authType, secretRef
}

func workspaceIDsForSelection(fixture source.Fixture, selection Selection) ([]string, error) {
	switch selection.Kind {
	case ScopeKindWorkspace:
		if _, ok := FindWorkspace(fixture, selection.ID); !ok {
			return nil, fmt.Errorf("unknown workspace %q", selection.ID)
		}
		return []string{selection.ID}, nil
	case ScopeKindProject:
		ids := []string{}
		for _, workspace := range fixture.Workspaces {
			if workspace.ProjectID == selection.ID {
				ids = append(ids, workspace.ID)
			}
		}
		if len(ids) == 0 {
			return nil, fmt.Errorf("project %q has no workspaces", selection.ID)
		}
		sort.Strings(ids)
		return ids, nil
	default:
		return nil, fmt.Errorf("unsupported scope kind %q", selection.Kind)
	}
}

func KeySelectableInWorkspace(fixture source.Fixture, key source.Key, workspace source.Workspace) bool {
	if !keyVisibleInWorkspace(key, workspace) {
		return false
	}
	for _, assignment := range fixture.UserAssignments {
		if assignment.UserID != key.UserID {
			continue
		}
		switch assignment.Scope {
		case source.UserAssignmentScopeProject:
			if key.Scope == source.KeyScopeProject && assignment.ProjectID == key.ProjectID && workspace.ProjectID == assignment.ProjectID {
				return true
			}
		case source.UserAssignmentScopeWorkspace:
			if key.Scope == source.KeyScopeWorkspace && assignment.WorkspaceID == key.WorkspaceID && workspace.ID == assignment.WorkspaceID {
				return true
			}
		}
	}
	return false
}

func keyVisibleInWorkspace(key source.Key, workspace source.Workspace) bool {
	switch key.Scope {
	case source.KeyScopeProject:
		return key.ProjectID == workspace.ProjectID
	case source.KeyScopeWorkspace:
		return key.WorkspaceID == workspace.ID
	default:
		return false
	}
}

func FindWorkspace(fixture source.Fixture, id string) (source.Workspace, bool) {
	for _, workspace := range fixture.Workspaces {
		if workspace.ID == id {
			return workspace, true
		}
	}
	return source.Workspace{}, false
}

func FindModel(fixture source.Fixture, id string) (source.Model, bool) {
	for _, model := range fixture.Models {
		if model.ID == id {
			return model, true
		}
	}
	return source.Model{}, false
}

func FindProvider(fixture source.Fixture, id string) (source.Provider, bool) {
	for _, provider := range fixture.Providers {
		if provider.ID == id {
			return provider, true
		}
	}
	return source.Provider{}, false
}

func FindMCPServer(fixture source.Fixture, id string) (source.MCPServer, bool) {
	for _, server := range fixture.MCPServers {
		if server.ID == id {
			return server, true
		}
	}
	return source.MCPServer{}, false
}

func packProviders(fixture source.Fixture) []cherry.Provider {
	byID := map[string]cherry.Provider{}
	for _, provider := range fixture.Providers {
		byID[provider.ID] = cherry.Provider{
			ID:            provider.ID,
			Kind:          provider.Kind,
			BackendSchema: providerBackendSchema(provider.Kind, provider.BackendSchema),
			Endpoint:      provider.Endpoint,
			SecretRef:     provider.SecretRef,
			AuthType:      provider.AuthType,
			PathPrefix:    provider.PathPrefix,
		}
	}
	for _, model := range fixture.Models {
		if model.Provider == "" {
			continue
		}
		if _, ok := byID[model.Provider]; ok {
			continue
		}
		byID[model.Provider] = cherry.Provider{
			ID:            model.Provider,
			Kind:          model.Provider,
			BackendSchema: model.Provider,
		}
	}
	providers := make([]cherry.Provider, 0, len(byID))
	for _, provider := range byID {
		providers = append(providers, provider)
	}
	sort.Slice(providers, func(i, j int) bool { return providers[i].ID < providers[j].ID })
	return providers
}

func providerBackendSchema(kind string, backendSchema string) string {
	if backendSchema != "" {
		return backendSchema
	}
	return kind
}

func packMCPServers(fixture source.Fixture) []cherry.MCPServer {
	servers := make([]cherry.MCPServer, 0, len(fixture.MCPServers))
	for _, server := range fixture.MCPServers {
		servers = append(servers, cherry.MCPServer{
			ID:        server.ID,
			Endpoint:  server.Endpoint,
			SecretRef: server.SecretRef,
			AuthType:  server.AuthType,
		})
	}
	return servers
}

func orgDefaultRule() source.Rule {
	return source.Rule{
		ID:          "org-default",
		Specificity: source.SpecificityOrg,
		Overrides:   map[string]source.RouteNode{},
		RateLimit: &source.RateLimitPolicy{
			USDPerDay: 500,
			RPM:       300,
			OnExceed:  "reject",
		},
	}
}

func target(provider string, model string, secretRef string) source.RouteNode {
	return source.RouteNode{
		Kind: "target",
		Target: &source.Target{
			Provider:  provider,
			Model:     model,
			SecretRef: secretRef,
		},
	}
}

func validateNode(node source.RouteNode) error {
	var populated int
	if node.Kind == "target" || node.Target != nil {
		populated++
	}
	if node.Kind == "chain" || len(node.Chain) > 0 {
		populated++
	}
	if node.Kind == "split" || len(node.Split) > 0 {
		populated++
	}
	if populated != 1 {
		return fmt.Errorf("route node must contain exactly one of target, chain, or split")
	}
	switch node.Kind {
	case "target":
		if node.Target == nil {
			return fmt.Errorf("target is required")
		}
	case "chain":
		if len(node.Chain) == 0 {
			return fmt.Errorf("chain route node must not be empty")
		}
		for index, child := range node.Chain {
			if err := validateNode(child); err != nil {
				return fmt.Errorf("chain[%d]: %w", index, err)
			}
		}
	case "split":
		if len(node.Split) == 0 {
			return fmt.Errorf("split route node must not be empty")
		}
		for index, weighted := range node.Split {
			if weighted.Weight <= 0 {
				return fmt.Errorf("split[%d] weight must be positive", index)
			}
			if err := validateNode(weighted.Node); err != nil {
				return fmt.Errorf("split[%d]: %w", index, err)
			}
		}
	default:
		return fmt.Errorf("unsupported route node kind %q", node.Kind)
	}
	if node.Kind == "target" && node.Target == nil {
		return fmt.Errorf("target is required")
	}
	return nil
}

func sortedKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
