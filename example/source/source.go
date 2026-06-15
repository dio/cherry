package source

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	cherry "github.com/dio/cherry"
)

//go:embed testdata/example_fixture.yaml
var fixtureFiles embed.FS

type Fixture struct {
	OrgID           string           `yaml:"org_id"`
	Projects        []Project        `yaml:"projects"`
	Workspaces      []Workspace      `yaml:"workspaces"`
	Users           []User           `yaml:"users"`
	UserAssignments []UserAssignment `yaml:"user_assignments"`
	Keys            []Key            `yaml:"keys"`
	Tags            map[string]Tag   `yaml:"tags"`
	Profiles        []Profile        `yaml:"profiles"`
	Models          []Model          `yaml:"models"`
	Providers       []Provider       `yaml:"providers"`
	MCPServers      []MCPServer      `yaml:"mcp_servers"`
}

type Project struct {
	ID    string `yaml:"id"`
	OrgID string `yaml:"org_id"`
}

type Workspace struct {
	ID        string `yaml:"id"`
	ProjectID string `yaml:"project_id"`
}

type User struct {
	ID     string   `yaml:"id"`
	OrgID  string   `yaml:"org_id"`
	TagIDs []string `yaml:"tag_ids"`
}

type UserAssignmentScope string

const (
	UserAssignmentScopeProject   UserAssignmentScope = "project"
	UserAssignmentScopeWorkspace UserAssignmentScope = "workspace"
)

type UserAssignment struct {
	UserID      string              `yaml:"user_id"`
	Scope       UserAssignmentScope `yaml:"scope"`
	ProjectID   string              `yaml:"project_id"`
	WorkspaceID string              `yaml:"workspace_id"`
}

type KeyScope string

const (
	KeyScopeProject   KeyScope = "project"
	KeyScopeWorkspace KeyScope = "workspace"
)

type Key struct {
	ID          string   `yaml:"id"`
	Slug        string   `yaml:"slug"`
	UserID      string   `yaml:"user_id"`
	ProjectID   string   `yaml:"project_id"`
	WorkspaceID string   `yaml:"workspace_id"`
	Scope       KeyScope `yaml:"scope"`
	TagIDs      []string `yaml:"tag_ids"`
}

type Tag struct {
	ID      string `yaml:"id"`
	LLMRule Rule   `yaml:"llm_rule"`
}

type Rule struct {
	ID               string               `yaml:"id"`
	Specificity      Specificity          `yaml:"specificity"`
	Overrides        map[string]RouteNode `yaml:"overrides"`
	RoutingOverrides map[string]RouteNode `yaml:"routing_overrides"`
	RateLimit        *RateLimitPolicy     `yaml:"rate_limit"`
}

type Specificity int

const (
	SpecificityOrg Specificity = iota
	SpecificityProject
	SpecificityWorkspace
	SpecificityTag
	SpecificityUser
	SpecificityKey
)

func (s *Specificity) UnmarshalYAML(unmarshal func(any) error) error {
	var value string
	if err := unmarshal(&value); err != nil {
		return err
	}
	switch value {
	case "org":
		*s = SpecificityOrg
	case "project":
		*s = SpecificityProject
	case "workspace":
		*s = SpecificityWorkspace
	case "tag":
		*s = SpecificityTag
	case "user":
		*s = SpecificityUser
	case "key":
		*s = SpecificityKey
	default:
		return fmt.Errorf("unknown specificity %q", value)
	}
	return nil
}

func (s Specificity) MarshalYAML() (any, error) {
	switch s {
	case SpecificityOrg:
		return "org", nil
	case SpecificityProject:
		return "project", nil
	case SpecificityWorkspace:
		return "workspace", nil
	case SpecificityTag:
		return "tag", nil
	case SpecificityUser:
		return "user", nil
	case SpecificityKey:
		return "key", nil
	default:
		return int(s), nil
	}
}

type RouteNode struct {
	Kind     string              `yaml:"kind"`
	Target   *Target             `yaml:"target"`
	Chain    []RouteNode         `yaml:"chain"`
	Split    []WeightedRouteNode `yaml:"split"`
	Retry    *RetryPolicy        `yaml:"retry"`
	OnStatus []int               `yaml:"on_status"`
}

type WeightedRouteNode struct {
	Weight int       `yaml:"weight"`
	Node   RouteNode `yaml:"node"`
}

type RetryPolicy struct {
	RetryOn         string `yaml:"retry_on"`
	PerTryTimeoutMS int    `yaml:"per_try_timeout_ms"`
}

type Target struct {
	Provider  string `yaml:"provider"`
	Model     string `yaml:"model"`
	Name      string `yaml:"name"`
	SecretRef string `yaml:"secret_ref"`
}

func (n *RouteNode) UnmarshalYAML(value *yaml.Node) error {
	var aux struct {
		Kind     string             `yaml:"kind"`
		Target   *Target            `yaml:"target"`
		Chain    *routeChildrenNode `yaml:"chain"`
		Split    *routeChildrenNode `yaml:"split"`
		Retry    *RetryPolicy       `yaml:"retry"`
		OnStatus []int              `yaml:"on_status"`
	}
	if err := value.Decode(&aux); err != nil {
		return err
	}
	*n = RouteNode{
		Kind:     aux.Kind,
		Target:   aux.Target,
		Retry:    aux.Retry,
		OnStatus: aux.OnStatus,
	}
	if aux.Chain != nil {
		n.Kind = firstNonEmpty(n.Kind, "chain")
		n.Retry = aux.Chain.Retry
		n.Chain = aux.Chain.RouteChildren
	}
	if aux.Split != nil {
		n.Kind = firstNonEmpty(n.Kind, "split")
		n.Retry = aux.Split.Retry
		n.Split = aux.Split.WeightedChildren
	}
	if n.Target != nil {
		n.Kind = firstNonEmpty(n.Kind, "target")
	}
	return nil
}

type routeChildrenNode struct {
	Retry            *RetryPolicy
	RouteChildren    []RouteNode
	WeightedChildren []WeightedRouteNode
}

func (n *routeChildrenNode) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.SequenceNode:
		return value.Decode(&n.RouteChildren)
	case yaml.MappingNode:
		var aux struct {
			Retry    *RetryPolicy        `yaml:"retry"`
			Children []WeightedRouteNode `yaml:"children"`
		}
		if err := value.Decode(&aux); err != nil {
			return err
		}
		n.Retry = aux.Retry
		n.WeightedChildren = aux.Children
		n.RouteChildren = make([]RouteNode, 0, len(aux.Children))
		for _, child := range aux.Children {
			n.RouteChildren = append(n.RouteChildren, child.Node)
		}
		return nil
	default:
		return fmt.Errorf("route children must be a sequence or mapping")
	}
}

func (n *WeightedRouteNode) UnmarshalYAML(value *yaml.Node) error {
	var aux struct {
		Weight int                `yaml:"weight"`
		Node   *RouteNode         `yaml:"node"`
		Target *Target            `yaml:"target"`
		Chain  *routeChildrenNode `yaml:"chain"`
		Split  *routeChildrenNode `yaml:"split"`
	}
	if err := value.Decode(&aux); err != nil {
		return err
	}
	n.Weight = aux.Weight
	switch {
	case aux.Node != nil:
		n.Node = *aux.Node
	case aux.Target != nil:
		n.Node = RouteNode{Kind: "target", Target: aux.Target}
	case aux.Chain != nil:
		n.Node = RouteNode{
			Kind:  "chain",
			Retry: aux.Chain.Retry,
			Chain: aux.Chain.RouteChildren,
		}
	case aux.Split != nil:
		n.Node = RouteNode{
			Kind:  "split",
			Retry: aux.Split.Retry,
			Split: aux.Split.WeightedChildren,
		}
	default:
		return fmt.Errorf("weighted route node requires node, target, chain, or split")
	}
	return nil
}

type RateLimitPolicy struct {
	USDPerDay float64 `yaml:"usd_per_day"`
	RPM       int     `yaml:"rpm"`
	OnExceed  string  `yaml:"on_exceed"`
}

type Profile struct {
	ID          string              `yaml:"id"`
	Slug        string              `yaml:"slug"`
	Path        string              `yaml:"path"`
	WorkspaceID string              `yaml:"workspace_id"`
	Auth        AuthConfig          `yaml:"auth"`
	Tools       map[string]ToolSpec `yaml:"tools"`
}

type AuthConfig struct {
	Type      string `yaml:"type"`
	SecretRef string `yaml:"secret_ref"`
}

type ToolSpec struct {
	Auth    AuthConfig `yaml:"auth"`
	Include []string   `yaml:"include"`
}

type Model = cherry.Model

type Provider struct {
	ID                string                     `yaml:"id"`
	Kind              string                     `yaml:"kind"`
	BackendSchema     string                     `yaml:"backend_schema"`
	Endpoint          string                     `yaml:"endpoint"`
	SecretRef         string                     `yaml:"secret_ref"`
	AuthType          string                     `yaml:"auth_type"`
	PathPrefix        string                     `yaml:"path_prefix"`
	RequestMutations  map[string]ProviderMutator `yaml:"request_mutations"`
	ResponseMutations map[string]ProviderMutator `yaml:"response_mutations"`
}

type ProviderMutator struct {
	Headers map[string]string `yaml:"headers"`
	Body    map[string]string `yaml:"body"`
}

type MCPServer struct {
	ID        string   `yaml:"id"`
	Endpoint  string   `yaml:"endpoint"`
	SecretRef string   `yaml:"secret_ref"`
	AuthType  string   `yaml:"auth_type"`
	Tools     []string `yaml:"tools"`
}

type rawModelCatalog struct {
	Models []json.RawMessage `json:"models"`
}

type rawModelRow struct {
	Model                     string                    `json:"model"`
	Provider                  string                    `json:"provider"`
	Mode                      string                    `json:"mode"`
	IsEnabled                 bool                      `json:"isEnabled"`
	Capabilities              []string                  `json:"capabilities"`
	Modalities                cherry.ModelModalities    `json:"modalities"`
	AdditionalPricePerMillion cherry.ModelCatalogObject `json:"additionalPricePerMillion"`
	Limits                    cherry.ModelCatalogObject `json:"limits"`
}

type rawProviderCatalog struct {
	Providers []rawProviderRow `json:"providers"`
}

type rawProviderRow struct {
	Name       string `json:"name"`
	APIBaseURL string `json:"apiBaseUrl"`
	AuthType   string `json:"authType"`
	IsEnabled  bool   `json:"isEnabled"`
}

type rawMCPCatalogObject struct {
	Servers []rawMCPServerRow `json:"servers"`
}

type rawMCPServerRow struct {
	ID             string          `json:"id"`
	URL            string          `json:"url"`
	TarsURL        string          `json:"tarsUrl"`
	Authentication string          `json:"authentication"`
	RequiresAuth   bool            `json:"requiresAuth"`
	Tools          []rawMCPToolRow `json:"tools"`
}

type rawMCPToolRow struct {
	Name string `json:"name"`
}

func ExampleFixture() Fixture {
	data, err := fixtureFiles.ReadFile("testdata/example_fixture.yaml")
	if err != nil {
		panic(fmt.Sprintf("read embedded example fixture: %v", err))
	}
	fixture, err := decodeFixture(data)
	if err != nil {
		panic(fmt.Sprintf("decode embedded example fixture: %v", err))
	}
	return fixture
}

func LoadFixtureYAML(path string) (Fixture, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Fixture{}, fmt.Errorf("read fixture yaml %q: %w", path, err)
	}
	return decodeFixture(data)
}

func LoadModelCatalogJSON(path string) ([]Model, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read model catalog json %q: %w", path, err)
	}
	var catalog rawModelCatalog
	if err := json.Unmarshal(data, &catalog); err != nil {
		return nil, fmt.Errorf("decode model catalog json %q: %w", path, err)
	}
	models := make([]Model, 0, len(catalog.Models))
	for _, raw := range catalog.Models {
		var row rawModelRow
		if err := json.Unmarshal(raw, &row); err != nil {
			return nil, fmt.Errorf("decode model catalog row: %w", err)
		}
		if !row.IsEnabled || row.Model == "" || row.Provider == "" {
			continue
		}
		models = append(models, Model{
			ID:                        row.Model,
			Provider:                  row.Provider,
			Name:                      row.Model,
			Mode:                      row.Mode,
			Capabilities:              row.Capabilities,
			Modalities:                row.Modalities,
			AdditionalPricePerMillion: row.AdditionalPricePerMillion,
			Limits:                    row.Limits,
			MetadataJSON:              string(raw),
		})
	}
	return models, nil
}

func LoadProviderCatalogJSON(path string) ([]Provider, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read provider catalog json %q: %w", path, err)
	}
	var catalog rawProviderCatalog
	if err := json.Unmarshal(data, &catalog); err != nil {
		return nil, fmt.Errorf("decode provider catalog json %q: %w", path, err)
	}
	providers := make([]Provider, 0, len(catalog.Providers))
	for _, row := range catalog.Providers {
		if row.Name == "" {
			continue
		}
		providers = append(providers, Provider{
			ID:       row.Name,
			Kind:     row.Name,
			Endpoint: row.APIBaseURL,
			AuthType: row.AuthType,
		})
	}
	return providers, nil
}

func LoadMCPCatalogJSON(path string) ([]MCPServer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read mcp catalog json %q: %w", path, err)
	}
	rows, err := decodeMCPCatalog(data)
	if err != nil {
		return nil, fmt.Errorf("decode mcp catalog json %q: %w", path, err)
	}
	servers := make([]MCPServer, 0, len(rows))
	for _, row := range rows {
		if row.ID == "" {
			continue
		}
		tools := make([]string, 0, len(row.Tools))
		for _, tool := range row.Tools {
			if tool.Name != "" {
				tools = append(tools, tool.Name)
			}
		}
		servers = append(servers, MCPServer{
			ID:       row.ID,
			Endpoint: firstNonEmpty(row.URL, row.TarsURL),
			AuthType: mcpAuthType(row.Authentication, row.RequiresAuth),
			Tools:    tools,
		})
	}
	return servers, nil
}

func decodeMCPCatalog(data []byte) ([]rawMCPServerRow, error) {
	var rows []rawMCPServerRow
	if err := json.Unmarshal(data, &rows); err == nil {
		return rows, nil
	}
	var object rawMCPCatalogObject
	if err := json.Unmarshal(data, &object); err != nil {
		return nil, err
	}
	return object.Servers, nil
}

func MergeModels(base []Model, overlay []Model) []Model {
	merged := make([]Model, 0, len(base)+len(overlay))
	seen := map[string]int{}
	for _, model := range base {
		seen[model.ID] = len(merged)
		merged = append(merged, model)
	}
	for _, model := range overlay {
		if index, ok := seen[model.ID]; ok {
			merged[index] = model
			continue
		}
		seen[model.ID] = len(merged)
		merged = append(merged, model)
	}
	return merged
}

func MergeProviders(base []Provider, overlay []Provider) []Provider {
	merged := make([]Provider, 0, len(base)+len(overlay))
	seen := map[string]int{}
	for _, provider := range base {
		seen[provider.ID] = len(merged)
		merged = append(merged, provider)
	}
	for _, provider := range overlay {
		if index, ok := seen[provider.ID]; ok {
			merged[index] = mergeProvider(merged[index], provider)
			continue
		}
		seen[provider.ID] = len(merged)
		merged = append(merged, provider)
	}
	return merged
}

func mergeProvider(base Provider, overlay Provider) Provider {
	if overlay.Kind != "" {
		base.Kind = overlay.Kind
	}
	if overlay.BackendSchema != "" {
		base.BackendSchema = overlay.BackendSchema
	}
	if overlay.Endpoint != "" {
		base.Endpoint = overlay.Endpoint
	}
	if overlay.SecretRef != "" {
		base.SecretRef = overlay.SecretRef
	}
	if overlay.AuthType != "" {
		base.AuthType = overlay.AuthType
	}
	if overlay.PathPrefix != "" {
		base.PathPrefix = overlay.PathPrefix
	}
	if len(overlay.RequestMutations) > 0 {
		base.RequestMutations = overlay.RequestMutations
	}
	if len(overlay.ResponseMutations) > 0 {
		base.ResponseMutations = overlay.ResponseMutations
	}
	return base
}

func MergeMCPServers(base []MCPServer, overlay []MCPServer) []MCPServer {
	merged := make([]MCPServer, 0, len(base)+len(overlay))
	seen := map[string]int{}
	for _, server := range base {
		seen[server.ID] = len(merged)
		merged = append(merged, server)
	}
	for _, server := range overlay {
		if index, ok := seen[server.ID]; ok {
			merged[index] = mergeMCPServer(merged[index], server)
			continue
		}
		seen[server.ID] = len(merged)
		merged = append(merged, server)
	}
	return merged
}

func mergeMCPServer(base MCPServer, overlay MCPServer) MCPServer {
	if overlay.Endpoint != "" {
		base.Endpoint = overlay.Endpoint
	}
	if overlay.SecretRef != "" {
		base.SecretRef = overlay.SecretRef
	}
	if overlay.AuthType != "" {
		base.AuthType = overlay.AuthType
	}
	if len(overlay.Tools) > 0 {
		base.Tools = overlay.Tools
	}
	return base
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func mcpAuthType(authentication string, requiresAuth bool) string {
	switch authentication {
	case "", "Open":
		if requiresAuth {
			return "bearer"
		}
		return "none"
	case "Bearer Token":
		return "bearer"
	default:
		if requiresAuth {
			return "bearer"
		}
		return authentication
	}
}

func decodeFixture(data []byte) (Fixture, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var fixture Fixture
	if err := decoder.Decode(&fixture); err != nil {
		return Fixture{}, fmt.Errorf("decode fixture yaml: %w", err)
	}
	return fixture, nil
}
