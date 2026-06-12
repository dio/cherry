package source

import (
	"bytes"
	"embed"
	"fmt"
	"os"

	cherry "github.com/dio/cherry"
	"gopkg.in/yaml.v3"
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
	ID          string               `yaml:"id"`
	Specificity Specificity          `yaml:"specificity"`
	Overrides   map[string]RouteNode `yaml:"overrides"`
	RateLimit   *RateLimitPolicy     `yaml:"rate_limit"`
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
	Kind   string              `yaml:"kind"`
	Target *Target             `yaml:"target"`
	Chain  []RouteNode         `yaml:"chain"`
	Split  []WeightedRouteNode `yaml:"split"`
}

type WeightedRouteNode struct {
	Weight int       `yaml:"weight"`
	Node   RouteNode `yaml:"node"`
}

type Target struct {
	Provider  string `yaml:"provider"`
	Model     string `yaml:"model"`
	SecretRef string `yaml:"secret_ref"`
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
	Endpoint          string                     `yaml:"endpoint"`
	SecretRef         string                     `yaml:"secret_ref"`
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

func decodeFixture(data []byte) (Fixture, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var fixture Fixture
	if err := decoder.Decode(&fixture); err != nil {
		return Fixture{}, fmt.Errorf("decode fixture yaml: %w", err)
	}
	return fixture, nil
}
