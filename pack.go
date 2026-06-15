// Package cherry contains a compact enforcement bundle writer and reader.
//
// A control plane uses Build or BuildWithManifest to turn normalized routing
// rows into one immutable byte blob. An enforcement point uses Open,
// OpenWithManifest, or OpenBundleZstd to validate that blob and query it in
// place. Reader intentionally does not inflate the pack into one Go map/object
// per principal, MCP profile, or rate-limit rule. Its hot-path methods binary
// search fixed-width indexes and return integer IDs into shared tables.
//
// The package starts at the normalized-data boundary. It does not perform
// tenancy joins, key verification, ownership checks, or rule merging. Those
// decisions belong to the system that prepares Input.
package cherry

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

const (
	magic              = "OPK1"
	currentPackVersion = uint32(9)
	headerSize         = 64

	// Header layout:
	//   0:4   magic "OPK1"
	//   4:8   format version
	//   8:16  FNV-1a checksum of blob[headerSize:]
	//   16:20 strings table offset
	//   20:24 providers table offset
	//   24:28 models table offset
	//   28:32 route shape table offset
	//   32:36 rate policies table offset
	//   36:40 MCP servers table offset
	//   40:44 MCP toolsets table offset
	//   44:48 scopes table offset
	//   48:52 principals index offset
	//   52:56 MCP paths index offset
	//   56:64 reserved
	headerMagicOff       = 0
	headerVersionOff     = 4
	headerChecksumOff    = 8
	headerStringsOff     = 16
	headerProvidersOff   = 20
	headerModelsOff      = 24
	headerRoutesOff      = 28
	headerRatesOff       = 32
	headerMCPServersOff  = 36
	headerMCPToolsetsOff = 40
	headerScopesOff      = 44
	headerPrincipalsOff  = 48
	headerMCPPathsOff    = 52

	principalLen      = 32 // hash64(slug + "\x00" + requestedModel), slugSID, routeID, rateID, requestedModelID, credentialCount, credentialOffset
	credentialLen     = 8  // target ordinal, secretSID
	modelLen          = 44 // hash64, idSID, providerID, nameSID, modeSID, capabilitiesSID, modalitiesSID, additionalPriceSID, limitsSID, metadataSID
	providerLen       = 28 // idSID, kindSID, endpointSID, secretSID, authTypeSID, pathPrefixSID, extraSID
	routeLen          = 24 // kind, target fields or child count/offset/retry fields
	routeChildLen     = 8  // weight, childRouteID; weight is zero for chain children
	rateLen           = 16 // usdCents, rpm, onExceedSID
	mcpServerLen      = 16 // idSID, endpointSID, secretSID, authTypeSID
	mcpToolsetLen     = 8  // bindingCount, bindingOffset
	mcpToolBindingLen = 20 // exposedSID, serverID, toolSID, secretSID, authTypeSID
	scopeLen          = 20 // sid, principalCount, principalOffset, mcpPathCount, mcpPathOffset
	mcpPathLen        = 44 // hash64, pathSID, toolsetID, profile auth typeSID, providerSID, secretSID, oauth tokenEndpointSID, clientIDSID, audienceSID, scopesSID

	smallMCPToolsetAuthLinearLimit = 16
)

const (
	routeKindTargetID uint32 = 1
	routeKindChainID  uint32 = 2
	routeKindSplitID  uint32 = 3
)

// RouteKind identifies the shape of an LLM route plan node.
type RouteKind string

const (
	// RouteKindTarget sends the request to one concrete provider/model target.
	RouteKindTarget RouteKind = "target"
	// RouteKindChain tries child route plans in order. Retry describes when the
	// next child should be attempted by the enforcement point.
	RouteKindChain RouteKind = "chain"
	// RouteKindSplit chooses among weighted child route plans.
	RouteKindSplit RouteKind = "split"
)

// Provider describes an upstream LLM provider available to compiled LLM routes.
// SecretRef is a reference to secret material, not the secret material itself.
// AuthType identifies the credential scheme used to authenticate requests to
// this provider (e.g. "bearer"). Empty string is treated as "bearer" by
// enforcement points for backwards compatibility.
type Provider struct {
	ID         string
	Kind       string
	Endpoint   string
	SecretRef  string
	AuthType   string
	PathPrefix string
	Extra      map[string]string
}

// Model describes a logical model name accepted by enforcement requests.
// Provider must reference an entry in Input.Providers. Name is the upstream model
// name sent to the selected provider after routing has been resolved.
//
// Mode, Capabilities, Modalities, AdditionalPricePerMillion, and Limits preserve
// normalized catalog data for model listing, capability checks, request shaping,
// and cost calculation. MetadataJSON is intentionally opaque to Cherry so
// producers can retain aliases, options, descriptions, source URLs, and future
// provider-specific fields without forcing those fields into the binary schema.
type Model struct {
	ID                        string
	Provider                  string
	Name                      string
	Mode                      string
	Capabilities              []string
	Modalities                ModelModalities
	AdditionalPricePerMillion ModelCatalogObject
	Limits                    ModelCatalogObject
	MetadataJSON              string
}

// ModelModalities describes the input and output modalities supported by a
// model catalog entry.
type ModelModalities struct {
	Input  []string `json:"input,omitempty"`
	Output []string `json:"output,omitempty"`
}

// ModelCatalogObject is an open normalized model-catalog object. It is used for
// source fields whose key sets vary by provider or mode, such as limits and
// additional price dimensions.
type ModelCatalogObject map[string]json.RawMessage

// RetryPolicy describes retry/fallback behavior attached to a chain route node.
// Cherry stores this metadata verbatim; the enforcement point decides how to
// interpret RetryOn values such as "401" or "connect-failure,reset,5xx".
type RetryPolicy struct {
	RetryOn         string
	PerTryTimeoutMS uint32
}

// WeightedRoutePlan is one weighted child under a split route node.
type WeightedRoutePlan struct {
	Weight uint32
	Plan   RoutePlan
}

// RoutePlan is a compiled LLM route tree for a principal and requested model.
// A zero Kind with Provider/Model set is treated as a target for compatibility
// with simple callers. The route must already reflect external rule precedence
// and overrides.
type RoutePlan struct {
	Kind     RouteKind
	Provider string
	Model    string
	// SecretRef overrides the selected provider default for target nodes. It is
	// always a reference to secret material, never the material itself.
	SecretRef string
	Retry     *RetryPolicy
	Children  []RoutePlan
	Split     []WeightedRoutePlan
}

// RatePolicy is immutable rate-limit metadata attached to a principal route.
// Mutable counters are intentionally not stored in the pack.
type RatePolicy struct {
	USDPerDayCents uint64
	RPM            uint32
	OnExceed       string
}

// Principal describes the compiled LLM routes for one verified principal slug in
// one scope. The verifier that turns key or token material into Slug is outside
// this package.
type Principal struct {
	Slug string
	// ModelRoutes maps requested model ID to the already-compiled final target
	// route for that request. Route is kept as a compatibility/default helper for
	// small tests and synthetic inputs that only need one model route.
	ModelRoutes map[string]RoutePlan
	Route       RoutePlan
	Rate        RatePolicy
}

// MCPServer describes an upstream MCP server that can be referenced by profile
// tool bindings. SecretRef and AuthType are defaults that a profile binding may
// override before data reaches this package.
type MCPServer struct {
	ID        string
	Endpoint  string
	SecretRef string
	AuthType  string
}

// MCPToolBinding maps an exposed tool name to an upstream MCP server/tool pair.
// Profile routes with multiple servers typically expose names such as
// "github__list-repos" to keep the upstream identity unambiguous.
type MCPToolBinding struct {
	ExposedName string
	Server      string
	Tool        string
	SecretRef   string
	AuthType    string
}

// MCPProfileAuth describes the client-facing authentication required before a
// request may use an MCP profile path. Type is a normalized mode such as "none",
// "api-key", or "oauth-token"; Provider optionally selects the Plum auth
// provider implementation for that type, such as "builtin" or "custom".
// SecretRef is a reference only and must never contain secret material.
type MCPProfileAuth struct {
	Type      string
	Provider  string
	SecretRef string
	OAuth     MCPOAuthConfig
}

// MCPOAuthConfig stores normalized OAuth settings for MCP profile auth. Secret
// material is still represented only by SecretRef on the containing profile
// auth record.
type MCPOAuthConfig struct {
	TokenEndpoint string
	ClientID      string
	Audience      string
	Scopes        []string
}

// MCPProfile describes one MCP path suffix within a scope and the tools exposed
// through that path. Path is the normalized suffix after the HTTP layer strips
// the MCP prefix.
type MCPProfile struct {
	Path  string
	Auth  MCPProfileAuth
	Tools []MCPToolBinding
}

// Scope is one enforcement partition in the pack, commonly a workspace. A
// project bundle can contain multiple workspace scopes.
type Scope struct {
	ID          string
	Principals  []Principal
	MCPProfiles []MCPProfile
}

// Input is the normalized data accepted by Build. Callers are responsible for
// preparing only the providers, models, servers, scopes, principals, and profiles
// that are valid for the bundle being built.
type Input struct {
	Providers  []Provider
	Models     []Model
	MCPServers []MCPServer
	Scopes     []Scope
}

// Reader is an immutable, in-place view of a validated pack blob. A Reader is
// safe to share between goroutines as long as callers do not mutate the
// underlying blob slice passed to Open.
type Reader struct {
	blob           []byte
	stringsOff     uint32
	providersOff   uint32
	modelsOff      uint32
	routesOff      uint32
	ratesOff       uint32
	mcpServersOff  uint32
	mcpToolsetsOff uint32
	scopesOff      uint32
	principalsOff  uint32
	mcpPathsOff    uint32
}

// LLMIDs is the allocation-minimizing data-path result. It returns integer IDs
// into the pack tables and string table. Callers that only need to dispatch to an
// already-interned provider/model can avoid materializing strings entirely.
type LLMIDs struct {
	PrincipalSID uint32
	ProviderID   uint32
	ProviderSID  uint32
	KindSID      uint32
	EndpointSID  uint32
	ModelID      uint32
	ModelSID     uint32
	ModelNameSID uint32
	SecretSID    uint32
	RouteID      uint32
	RateID       uint32
	Rate         RatePolicyIDs
}

type RatePolicyIDs struct {
	USDPerDayCents uint64
	RPM            uint32
	OnExceedSID    uint32
}

// LLMRouteChildIDs is one child of a materialized route-plan node. Weight is
// zero for ordered chain children and non-zero for split children.
type LLMRouteChildIDs struct {
	Weight uint32
	Plan   LLMRoutePlanIDs
}

// LLMRoutePlanIDs is the ID-returning representation of an LLM route tree. It
// is the preferred data-path API when the enforcement point needs fallback or
// weighted split behavior without string materialization.
type LLMRoutePlanIDs struct {
	RouteID         uint32
	Kind            RouteKind
	ProviderID      uint32
	ProviderSID     uint32
	KindSID         uint32
	EndpointSID     uint32
	ModelID         uint32
	ModelSID        uint32
	ModelNameSID    uint32
	SecretSID       uint32
	RetryOnSID      uint32
	PerTryTimeoutMS uint32
	Children        []LLMRouteChildIDs
}

// LLMPlanIDs is the complete ID-returning result for an LLM lookup, including
// the requested principal/model index entry, immutable rate-limit metadata, and
// the route tree selected for enforcement.
type LLMPlanIDs struct {
	PrincipalSID      uint32
	RequestedModelID  uint32
	RequestedModelSID uint32
	RouteID           uint32
	RateID            uint32
	Rate              RatePolicyIDs
	Plan              LLMRoutePlanIDs
}

// LLMResult is the string-materialized form of an LLM lookup. It is convenient
// for tests, CLIs, and diagnostics; hot paths should prefer LLMIDs.
type LLMResult struct {
	PrincipalSlug string
	Provider      string
	ProviderKind  string
	Endpoint      string
	Model         string
	ModelName     string
	SecretRef     string
	Rate          RatePolicy
}

// LLMRouteChild is one materialized route-plan child. Weight is zero for chain
// children and non-zero for split children.
type LLMRouteChild struct {
	Weight uint32
	Plan   LLMRoutePlan
}

// LLMRoutePlan is the string-materialized representation of an LLM route tree.
// Target nodes populate Provider/Model/SecretRef fields. Chain and split nodes
// populate Children.
type LLMRoutePlan struct {
	Kind            RouteKind
	Provider        string
	ProviderKind    string
	Endpoint        string
	Model           string
	ModelName       string
	SecretRef       string
	RetryOn         string
	PerTryTimeoutMS uint32
	Children        []LLMRouteChild
}

// LLMPlan is the string-materialized complete route plan for an LLM lookup.
type LLMPlan struct {
	PrincipalSlug  string
	RequestedModel string
	Plan           LLMRoutePlan
	Rate           RatePolicy
}

// ProviderInfo is the string-materialized metadata for one provider stored in
// the pack. It is intended for diagnostics and EP setup inspection.
type ProviderInfo struct {
	ID         string
	Kind       string
	Endpoint   string
	SecretRef  string
	AuthType   string
	PathPrefix string
	Extra      map[string]string
}

// ModelInfo is the string-materialized metadata for one model stored in the
// pack. MetadataJSON is the raw normalized catalog object supplied by the
// producer.
type ModelInfo struct {
	ID                        string
	Provider                  string
	Name                      string
	Mode                      string
	Capabilities              []string
	Modalities                ModelModalities
	AdditionalPricePerMillion ModelCatalogObject
	Limits                    ModelCatalogObject
	MetadataJSON              string
}

// MCPToolIDs is the allocation-minimizing representation of one MCP tool
// binding. String fields are returned as string-table IDs and can be materialized
// with Reader.String when needed.
type MCPToolIDs struct {
	ExposedNameSID    uint32
	ServerID          uint32
	ServerSID         uint32
	ServerEndpointSID uint32
	ToolSID           uint32
	SecretSID         uint32
	AuthTypeSID       uint32
}

// MCPResultIDs is the allocation-minimizing representation of an MCP path lookup.
// Tools contains the precomputed effective allowset for the resolved path.
type MCPResultIDs struct {
	PathSID   uint32
	ToolsetID uint32
	Auth      MCPProfileAuthIDs
	Tools     []MCPToolIDs
}

// MCPUpstreamServerIDs is the allocation-minimizing representation of one
// upstream MCP server needed to initialize a resolved MCP path.
type MCPUpstreamServerIDs struct {
	ServerID    uint32
	ServerSID   uint32
	EndpointSID uint32
	SecretSID   uint32
	AuthTypeSID uint32
}

// MCPProfileAuthIDs is the string-table ID form of MCPProfileAuth.
type MCPProfileAuthIDs struct {
	TypeSID     uint32
	ProviderSID uint32
	SecretSID   uint32
	OAuth       MCPOAuthConfigIDs
}

// MCPOAuthConfigIDs is the string-table ID form of MCPOAuthConfig.
type MCPOAuthConfigIDs struct {
	TokenEndpointSID uint32
	ClientIDSID      uint32
	AudienceSID      uint32
	ScopesSID        uint32
}

// MCPInitializeIDs is the allocation-minimizing result for MCP initialize. It
// contains the upstream servers behind a path, including the effective auth and
// secret refs selected for that path.
type MCPInitializeIDs struct {
	PathSID uint32
	Auth    MCPProfileAuthIDs
	Servers []MCPUpstreamServerIDs
}

// MCPTool is the string-materialized form of one MCP tool binding.
type MCPTool struct {
	ExposedName    string
	Server         string
	ServerEndpoint string
	Tool           string
	SecretRef      string
	AuthType       string
}

// MCPResult is the string-materialized form of an MCP path lookup.
type MCPResult struct {
	Path  string
	Auth  MCPProfileAuth
	Tools []MCPTool
}

// MCPUpstreamServer is the string-materialized form of one upstream server
// needed to initialize a resolved MCP path.
type MCPUpstreamServer struct {
	Server    string
	Endpoint  string
	SecretRef string
	AuthType  string
}

// MCPInitializeResult is the string-materialized result for MCP initialize.
type MCPInitializeResult struct {
	Path    string
	Auth    MCPProfileAuth
	Servers []MCPUpstreamServer
}

// PrincipalRoute is an inspector record for one principal/requested-model route
// stored in a scope. It is derived from the packed indexes and is not used by the
// hot request path.
type PrincipalRoute struct {
	ScopeID        string
	PrincipalSlug  string
	RequestedModel string
	RouteKind      RouteKind
	Provider       string
	Model          string
	SecretRef      string
	Rate           RatePolicy
}

// PrincipalInfo is an inspector record for one principal slug in a scope and
// the requested model IDs it can route.
type PrincipalInfo struct {
	ScopeID         string
	PrincipalSlug   string
	RequestedModels []string
}

// MCPPath is an inspector record for one MCP path and its effective tool
// bindings in a scope.
type MCPPath struct {
	ScopeID string
	Path    string
	Auth    MCPProfileAuth
	Tools   []MCPTool
}

// MCPServerInfo is the string-materialized metadata for one upstream MCP server
// stored in the pack. It is intended for diagnostics and EP setup inspection.
// SecretRef is a ref only; secret material is never stored in the pack.
type MCPServerInfo struct {
	ID        string
	Endpoint  string
	SecretRef string
	AuthType  string
}

// Manifest is the minimal external metadata needed to reject stale or corrupted
// blobs before the reader is made visible to the enforcement data path. A real CP
// envelope would add generation IDs, project/workspace labels, timestamps, and a
// signature. The pack package keeps only the fields it can validate locally.
type Manifest struct {
	FormatVersion uint32
	Checksum      uint64
	SizeBytes     uint64
}

// Build encodes normalized enforcement data into a compact immutable pack blob.
//
// The input must already be scoped and compiled by the caller. Build validates
// table references, rejects unknown providers/models/MCP servers, interns shared
// strings, and writes fixed-width indexes for LLM and MCP lookups.
//
// The returned blob can be opened with Open. Use BuildWithManifest when the blob
// will be delivered across a control-plane/enforcement-point boundary and the
// receiver should validate size, checksum, and format version before use.
func Build(input Input) ([]byte, error) {
	builder := newBuilder()
	providerIDs := map[string]uint32{}
	modelIDs := map[string]uint32{}
	mcpServerIDs := map[string]uint32{}
	routeIDs := newRouteInterner()
	routes := []compiledRoute{}
	rateIDs := map[RatePolicy]uint32{}
	toolsetIDs := map[uint64][]uint32{}
	toolsets := [][]MCPToolBinding{}
	compiledScopes := make([]compiledScope, 0, len(input.Scopes))

	providers := sortedProviders(input.Providers)
	for i, provider := range providers {
		providerIDs[provider.ID] = uint32(i)
		builder.stringID(provider.ID)
		builder.stringID(provider.Kind)
		builder.stringID(provider.Endpoint)
		builder.stringID(provider.SecretRef)
		builder.stringID(provider.AuthType)
		builder.stringID(provider.PathPrefix)
		builder.stringID(stringMapJSON(provider.Extra))
	}

	models := sortedModels(input.Models)
	for i, model := range models {
		if _, ok := providerIDs[model.Provider]; !ok {
			return nil, fmt.Errorf("model %q references unknown provider %q", model.ID, model.Provider)
		}
		modelIDs[model.ID] = uint32(i)
		builder.stringID(model.ID)
		builder.stringID(model.Name)
		builder.stringID(model.Mode)
		builder.stringID(capabilitiesKey(model.Capabilities))
		builder.stringID(modalitiesJSON(model.Modalities))
		builder.stringID(catalogObjectJSON(model.AdditionalPricePerMillion))
		builder.stringID(catalogObjectJSON(model.Limits))
		builder.stringID(model.MetadataJSON)
	}

	mcpServers := sortedMCPServers(input.MCPServers)
	for i, server := range mcpServers {
		mcpServerIDs[server.ID] = uint32(i)
		builder.stringID(server.ID)
		builder.stringID(server.Endpoint)
		builder.stringID(server.SecretRef)
		builder.stringID(server.AuthType)
	}

	for _, scope := range input.Scopes {
		compiled := compiledScope{
			id:               scope.ID,
			principalEntries: make([]compiledPrincipalEntry, 0, countPrincipalRoutes(scope.Principals)),
			mcpProfiles:      make([]compiledMCPProfile, 0, len(scope.MCPProfiles)),
		}
		builder.stringID(scope.ID)
		for _, principal := range scope.Principals {
			builder.stringID(principal.Slug)
			builder.stringID(principal.Rate.OnExceed)
			for requestedModel, route := range principalRoutes(principal) {
				if _, ok := modelIDs[requestedModel]; !ok {
					return nil, fmt.Errorf("principal %q references unknown requested model %q", principal.Slug, requestedModel)
				}
				routeID, err := internRoute(route, builder, providerIDs, modelIDs, routeIDs, &routes)
				if err != nil {
					return nil, fmt.Errorf("principal %q route for %q: %w", principal.Slug, requestedModel, err)
				}
				credentialSlots, err := routeCredentialSlots(route)
				if err != nil {
					return nil, fmt.Errorf("principal %q route for %q: %w", principal.Slug, requestedModel, err)
				}
				if credentialSlots.count > 0 {
					builder.stringID(credentialSlots.first.secretRef)
					for _, slot := range credentialSlots.extra {
						builder.stringID(slot.secretRef)
					}
				}
				lookupHash := principalLookupHash(principal.Slug, requestedModel)
				compiled.principalEntries = append(compiled.principalEntries, compiledPrincipalEntry{
					slug:                principal.Slug,
					requestedModel:      requestedModel,
					lookupHash:          lookupHash,
					routeID:             routeID,
					rate:                principal.Rate,
					credentialSlotCount: credentialSlots.count,
					credentialSlot0:     credentialSlots.first,
					credentialSlotExtra: credentialSlots.extra,
				})
			}
			if _, ok := rateIDs[principal.Rate]; !ok {
				rateIDs[principal.Rate] = uint32(len(rateIDs))
			}
		}
		for _, profile := range scope.MCPProfiles {
			builder.stringID(profile.Path)
			internMCPProfileAuth(builder, profile.Auth)
			canonicalTools := canonicalToolset(profile.Tools)
			var serverAuth map[string]MCPToolBinding
			if len(canonicalTools) > smallMCPToolsetAuthLinearLimit {
				serverAuth = make(map[string]MCPToolBinding, len(canonicalTools))
			}
			toolsetHash := newToolsetHash()
			for toolIndex, tool := range canonicalTools {
				serverID, ok := mcpServerIDs[tool.Server]
				if !ok {
					return nil, fmt.Errorf("mcp profile %q references unknown server %q", profile.Path, tool.Server)
				}
				if serverAuth == nil {
					if err := validateMCPToolAuthLinear(profile.Path, canonicalTools[:toolIndex], tool); err != nil {
						return nil, err
					}
				} else {
					if existing, ok := serverAuth[tool.Server]; ok &&
						!sameMCPToolAuth(existing, tool) {
						return nil, fmt.Errorf("mcp profile %q has conflicting auth for server %q", profile.Path, tool.Server)
					}
					serverAuth[tool.Server] = tool
				}
				toolsetHash = hashToolsetID(toolsetHash, builder.stringID(tool.ExposedName))
				toolsetHash = hashToolsetID(toolsetHash, serverID)
				toolsetHash = hashToolsetID(toolsetHash, builder.stringID(tool.Tool))
				toolsetHash = hashToolsetID(toolsetHash, builder.stringID(tool.SecretRef))
				toolsetHash = hashToolsetID(toolsetHash, builder.stringID(tool.AuthType))
			}
			toolsetID := internMCPToolset(toolsetHash, canonicalTools, toolsetIDs, &toolsets)
			pathHash := hashString(profile.Path)
			compiled.mcpProfiles = append(compiled.mcpProfiles, compiledMCPProfile{
				path:      profile.Path,
				pathHash:  pathHash,
				toolsetID: toolsetID,
				auth:      profile.Auth,
			})
		}
		compiledScopes = append(compiledScopes, compiled)
	}

	var out bytes.Buffer
	out.Grow(headerSize + len(input.Scopes)*scopeLen)
	out.Write(make([]byte, headerSize))

	stringsOff := uint32(out.Len())
	writeStrings(&out, builder.strings)
	providersOff := uint32(out.Len())
	writeProviders(&out, builder, providers)
	modelsOff := uint32(out.Len())
	writeModels(&out, builder, models, providerIDs)
	routesOff := uint32(out.Len())
	writeRoutes(&out, builder, routes, providerIDs, modelIDs)
	ratesOff := uint32(out.Len())
	writeRates(&out, builder, rateIDs)
	mcpServersOff := uint32(out.Len())
	writeMCPServers(&out, builder, mcpServers)
	mcpToolsetsOff := uint32(out.Len())
	writeMCPToolsets(&out, builder, toolsets, mcpServerIDs)
	scopesOff := uint32(out.Len())
	principalsOff, mcpPathsOff := writeScopes(&out, builder, compiledScopes, rateIDs)

	blob := out.Bytes()
	copy(blob[headerMagicOff:headerMagicOff+4], []byte(magic))
	put32(blob[headerVersionOff:headerVersionOff+4], currentPackVersion)
	put32(blob[headerStringsOff:headerStringsOff+4], stringsOff)
	put32(blob[headerProvidersOff:headerProvidersOff+4], providersOff)
	put32(blob[headerModelsOff:headerModelsOff+4], modelsOff)
	put32(blob[headerRoutesOff:headerRoutesOff+4], routesOff)
	put32(blob[headerRatesOff:headerRatesOff+4], ratesOff)
	put32(blob[headerMCPServersOff:headerMCPServersOff+4], mcpServersOff)
	put32(blob[headerMCPToolsetsOff:headerMCPToolsetsOff+4], mcpToolsetsOff)
	put32(blob[headerScopesOff:headerScopesOff+4], scopesOff)
	put32(blob[headerPrincipalsOff:headerPrincipalsOff+4], principalsOff)
	put32(blob[headerMCPPathsOff:headerMCPPathsOff+4], mcpPathsOff)
	binary.LittleEndian.PutUint64(blob[headerChecksumOff:headerChecksumOff+8], checksum(blob[headerSize:]))
	return blob, nil
}

// Open validates a pack blob and returns an immutable Reader over the same byte
// slice. The caller must keep blob alive and must not mutate it while any Reader
// is in use.
//
// Open checks the magic header, format version, checksum, and section offsets. It
// does not validate an external envelope; use OpenWithManifest for delivered
// bundles that carry manifest metadata.
func Open(blob []byte) (Reader, error) {
	if len(blob) < headerSize || string(blob[headerMagicOff:headerMagicOff+4]) != magic {
		return Reader{}, errors.New("invalid pack header")
	}
	if u32(blob[headerVersionOff:headerVersionOff+4]) != currentPackVersion {
		return Reader{}, fmt.Errorf("unsupported pack version %d", u32(blob[headerVersionOff:headerVersionOff+4]))
	}
	wantChecksum := binary.LittleEndian.Uint64(blob[headerChecksumOff : headerChecksumOff+8])
	if gotChecksum := checksum(blob[headerSize:]); gotChecksum != wantChecksum {
		return Reader{}, fmt.Errorf("pack checksum mismatch")
	}
	reader := Reader{
		blob:           blob,
		stringsOff:     u32(blob[headerStringsOff : headerStringsOff+4]),
		providersOff:   u32(blob[headerProvidersOff : headerProvidersOff+4]),
		modelsOff:      u32(blob[headerModelsOff : headerModelsOff+4]),
		routesOff:      u32(blob[headerRoutesOff : headerRoutesOff+4]),
		ratesOff:       u32(blob[headerRatesOff : headerRatesOff+4]),
		mcpServersOff:  u32(blob[headerMCPServersOff : headerMCPServersOff+4]),
		mcpToolsetsOff: u32(blob[headerMCPToolsetsOff : headerMCPToolsetsOff+4]),
		scopesOff:      u32(blob[headerScopesOff : headerScopesOff+4]),
		principalsOff:  u32(blob[headerPrincipalsOff : headerPrincipalsOff+4]),
		mcpPathsOff:    u32(blob[headerMCPPathsOff : headerMCPPathsOff+4]),
	}
	if err := reader.validateOffsets(); err != nil {
		return Reader{}, err
	}
	return reader, nil
}

// BuildWithManifest builds a pack blob and returns the metadata needed to
// validate that exact blob before opening it in an enforcement point.
func BuildWithManifest(input Input) ([]byte, Manifest, error) {
	blob, err := Build(input)
	if err != nil {
		return nil, Manifest{}, err
	}
	manifest, err := ReadManifest(blob)
	if err != nil {
		return nil, Manifest{}, err
	}
	return blob, manifest, nil
}

// ReadManifest extracts the self-describing metadata from a complete pack blob.
// It reads only the pack header and does not perform a full Open validation.
func ReadManifest(blob []byte) (Manifest, error) {
	if len(blob) < headerSize || string(blob[headerMagicOff:headerMagicOff+4]) != magic {
		return Manifest{}, errors.New("invalid pack header")
	}
	return Manifest{
		FormatVersion: u32(blob[headerVersionOff : headerVersionOff+4]),
		Checksum:      binary.LittleEndian.Uint64(blob[headerChecksumOff : headerChecksumOff+8]),
		SizeBytes:     uint64(len(blob)),
	}, nil
}

// OpenWithManifest validates externally delivered manifest metadata before
// opening the blob itself. This mirrors the enforcement-point load path where the
// control plane sends both the compact content and an envelope describing it.
func OpenWithManifest(blob []byte, manifest Manifest) (Reader, error) {
	if err := ValidateManifest(blob, manifest); err != nil {
		return Reader{}, err
	}
	return Open(blob)
}

// ValidateManifest checks whether manifest describes blob. It verifies the pack
// format version, byte size, and checksum over the content after the fixed
// header. It does not validate table offsets; Open performs that structural
// validation.
func ValidateManifest(blob []byte, manifest Manifest) error {
	if manifest.FormatVersion != currentPackVersion {
		return fmt.Errorf("unsupported manifest pack version %d", manifest.FormatVersion)
	}
	if manifest.SizeBytes != uint64(len(blob)) {
		return fmt.Errorf("manifest size mismatch: manifest=%d blob=%d", manifest.SizeBytes, len(blob))
	}
	if len(blob) < headerSize {
		return errors.New("invalid pack header")
	}
	if got := checksum(blob[headerSize:]); got != manifest.Checksum {
		return fmt.Errorf("manifest checksum mismatch")
	}
	return nil
}

// ResolveLLMPlanIDs resolves an LLM request to the compiled route tree as
// integer IDs into pack tables.
//
// scopeID identifies the enforcement scope, principalSlug is the verified
// principal produced by an external verifier, and modelID is the requested model
// from the LLM request body. Target nodes in the returned plan can be
// dereferenced with String or used directly by callers that intern provider,
// model, endpoint, and secret-ref tables elsewhere.
//
// The boolean is false when the scope, principal/model route, or requested model
// is not present in the pack.
func (r Reader) ResolveLLMPlanIDs(scopeID string, principalSlug string, modelID string) (LLMPlanIDs, bool) {
	scopeIndex, ok := r.findScope(scopeID)
	if !ok {
		return LLMPlanIDs{}, false
	}
	principal, ok := r.findPrincipal(scopeIndex, principalSlug, modelID)
	if !ok {
		return LLMPlanIDs{}, false
	}
	requestedModelID, ok := r.findModel(modelID)
	if !ok {
		return LLMPlanIDs{}, false
	}
	var targetOrdinal uint32
	plan, ok := r.routePlanIDs(principal.routeID, principal, &targetOrdinal)
	if !ok {
		return LLMPlanIDs{}, false
	}
	return LLMPlanIDs{
		PrincipalSID:      principal.slugSID,
		RequestedModelID:  requestedModelID,
		RequestedModelSID: r.model(requestedModelID).idSID,
		RouteID:           principal.routeID,
		RateID:            principal.rateID,
		Rate:              r.rateIDs(principal.rateID),
		Plan:              plan,
	}, true
}

// ResolveLLMIDs resolves an LLM request to the first executable target in the
// compiled route tree. It is retained for callers that only understand a single
// provider/model target. Enforcement points that implement fallback or weighted
// split behavior should use ResolveLLMPlanIDs instead.
func (r Reader) ResolveLLMIDs(scopeID string, principalSlug string, modelID string) (LLMIDs, bool) {
	plan, ok := r.ResolveLLMPlanIDs(scopeID, principalSlug, modelID)
	if !ok {
		return LLMIDs{}, false
	}
	target, ok := firstTargetPlanIDs(plan.Plan)
	if !ok {
		return LLMIDs{}, false
	}
	return LLMIDs{
		PrincipalSID: plan.PrincipalSID,
		ProviderID:   target.ProviderID,
		ProviderSID:  target.ProviderSID,
		KindSID:      target.KindSID,
		EndpointSID:  target.EndpointSID,
		ModelID:      target.ModelID,
		ModelSID:     target.ModelSID,
		ModelNameSID: target.ModelNameSID,
		SecretSID:    target.SecretSID,
		RouteID:      target.RouteID,
		RateID:       plan.RateID,
		Rate:         plan.Rate,
	}, true
}

// ResolveLLM resolves an LLM request and materializes the result as strings. It
// is easier to inspect than ResolveLLMIDs but allocates for string results and is
// therefore better suited to diagnostics, tests, and CLIs than the hot path.
func (r Reader) ResolveLLM(scopeID string, principalSlug string, modelID string) (LLMResult, bool) {
	ids, ok := r.ResolveLLMIDs(scopeID, principalSlug, modelID)
	if !ok {
		return LLMResult{}, false
	}
	return LLMResult{
		PrincipalSlug: principalSlug,
		Provider:      r.String(ids.ProviderSID),
		ProviderKind:  r.String(ids.KindSID),
		Endpoint:      r.String(ids.EndpointSID),
		Model:         r.String(ids.ModelSID),
		ModelName:     r.String(ids.ModelNameSID),
		SecretRef:     r.String(ids.SecretSID),
		Rate: RatePolicy{
			USDPerDayCents: ids.Rate.USDPerDayCents,
			RPM:            ids.Rate.RPM,
			OnExceed:       r.String(ids.Rate.OnExceedSID),
		},
	}, true
}

// ResolveLLMPlan resolves an LLM request and materializes the complete compiled
// route tree as strings. It is intended for diagnostics and example code; hot
// paths should prefer ResolveLLMPlanIDs.
func (r Reader) ResolveLLMPlan(scopeID string, principalSlug string, modelID string) (LLMPlan, bool) {
	ids, ok := r.ResolveLLMPlanIDs(scopeID, principalSlug, modelID)
	if !ok {
		return LLMPlan{}, false
	}
	return LLMPlan{
		PrincipalSlug:  principalSlug,
		RequestedModel: modelID,
		Plan:           r.materializeLLMRoutePlan(ids.Plan),
		Rate: RatePolicy{
			USDPerDayCents: ids.Rate.USDPerDayCents,
			RPM:            ids.Rate.RPM,
			OnExceed:       r.String(ids.Rate.OnExceedSID),
		},
	}, true
}

// ResolveProvider returns catalog metadata for one provider ID.
func (r Reader) ResolveProvider(providerID string) (ProviderInfo, bool) {
	count := r.sectionCount(r.providersOff)
	for id := uint32(0); id < count; id++ {
		info := r.providerInfo(id)
		if info.ID == providerID {
			return info, true
		}
	}
	return ProviderInfo{}, false
}

// Providers returns all provider catalog entries in deterministic order by
// provider ID.
func (r Reader) Providers() []ProviderInfo {
	count := r.sectionCount(r.providersOff)
	providers := make([]ProviderInfo, 0, count)
	for id := uint32(0); id < count; id++ {
		providers = append(providers, r.providerInfo(id))
	}
	sort.Slice(providers, func(i, j int) bool {
		return providers[i].ID < providers[j].ID
	})
	return providers
}

// ResolveMCPServer returns catalog metadata for one MCP server ID.
func (r Reader) ResolveMCPServer(serverID string) (MCPServerInfo, bool) {
	count := r.sectionCount(r.mcpServersOff)
	for id := uint32(0); id < count; id++ {
		info := r.mcpServerInfo(id)
		if info.ID == serverID {
			return info, true
		}
	}
	return MCPServerInfo{}, false
}

// MCPServers returns all upstream MCP server catalog entries in deterministic
// order by server ID.
func (r Reader) MCPServers() []MCPServerInfo {
	count := r.sectionCount(r.mcpServersOff)
	servers := make([]MCPServerInfo, 0, count)
	for id := uint32(0); id < count; id++ {
		servers = append(servers, r.mcpServerInfo(id))
	}
	sort.Slice(servers, func(i, j int) bool {
		return servers[i].ID < servers[j].ID
	})
	return servers
}

// ResolveModel returns catalog metadata for one model ID.
func (r Reader) ResolveModel(modelID string) (ModelInfo, bool) {
	id, ok := r.findModel(modelID)
	if !ok {
		return ModelInfo{}, false
	}
	return r.modelInfo(id), true
}

// Models returns all model catalog entries in deterministic order by model ID.
func (r Reader) Models() []ModelInfo {
	count := r.sectionCount(r.modelsOff)
	models := make([]ModelInfo, 0, count)
	for id := uint32(0); id < count; id++ {
		models = append(models, r.modelInfo(id))
	}
	sort.Slice(models, func(i, j int) bool {
		return models[i].ID < models[j].ID
	})
	return models
}

// ModelCapability reports whether a model has capability in its normalized
// catalog metadata.
func (r Reader) ModelCapability(modelID string, capability string) bool {
	info, ok := r.ResolveModel(modelID)
	if !ok {
		return false
	}
	for _, candidate := range info.Capabilities {
		if candidate == capability {
			return true
		}
	}
	return false
}

// V1ModelsJSON renders the loaded model catalog in an OpenAI-compatible
// /v1/models response shape. It is derived from Model.MetadataJSON when present
// and falls back to the indexed model fields otherwise.
func (r Reader) V1ModelsJSON() ([]byte, error) {
	return v1ModelsJSON(r.Models())
}

// V1ModelsJSONForProvider renders a /v1/models response shape containing only
// models whose base provider matches providerID.
func (r Reader) V1ModelsJSONForProvider(providerID string) ([]byte, error) {
	models := []ModelInfo{}
	for _, model := range r.Models() {
		if model.Provider == providerID {
			models = append(models, model)
		}
	}
	return v1ModelsJSON(models)
}

func v1ModelsJSON(models []ModelInfo) ([]byte, error) {
	data := make([]v1Model, 0, len(models))
	for _, model := range models {
		data = append(data, v1ModelFromModelInfo(model))
	}
	return json.Marshal(v1ModelsResponse{
		Object: "list",
		Data:   data,
	})
}

// ResolveMCPIDs resolves an MCP path suffix to the effective toolset IDs for a
// scope. Use this when the caller needs to list or inspect the whole MCP profile.
// Use ResolveMCPToolIDs when forwarding a single tool call.
func (r Reader) ResolveMCPIDs(scopeID string, pathSuffix string) (MCPResultIDs, bool) {
	scopeIndex, ok := r.findScope(scopeID)
	if !ok {
		return MCPResultIDs{}, false
	}
	path, ok := r.findMCPPath(scopeIndex, pathSuffix)
	if !ok {
		return MCPResultIDs{}, false
	}
	return MCPResultIDs{
		PathSID:   path.pathSID,
		ToolsetID: path.toolsetID,
		Auth:      path.auth,
		Tools:     r.toolset(path.toolsetID),
	}, true
}

// ResolveMCPInitializeIDs resolves an MCP path to the upstream server set needed
// for MCP initialize. Each server appears once with the effective auth and secret
// ref selected for that path.
func (r Reader) ResolveMCPInitializeIDs(scopeID string, pathSuffix string) (MCPInitializeIDs, bool) {
	result, ok := r.ResolveMCPIDs(scopeID, pathSuffix)
	if !ok {
		return MCPInitializeIDs{}, false
	}
	servers := make([]MCPUpstreamServerIDs, 0, len(result.Tools))
	seen := map[uint32]bool{}
	for _, tool := range result.Tools {
		if seen[tool.ServerID] {
			continue
		}
		seen[tool.ServerID] = true
		servers = append(servers, MCPUpstreamServerIDs{
			ServerID:    tool.ServerID,
			ServerSID:   tool.ServerSID,
			EndpointSID: tool.ServerEndpointSID,
			SecretSID:   tool.SecretSID,
			AuthTypeSID: tool.AuthTypeSID,
		})
	}
	sort.Slice(servers, func(i, j int) bool {
		return r.String(servers[i].ServerSID) < r.String(servers[j].ServerSID)
	})
	return MCPInitializeIDs{
		PathSID: result.PathSID,
		Auth:    result.Auth,
		Servers: servers,
	}, true
}

// ResolveMCPToolIDs resolves one exposed tool name without materializing the
// whole profile toolset. Use ResolveMCPIDs when the caller needs to list a
// profile; use this method on the MCP forwarding path.
func (r Reader) ResolveMCPToolIDs(scopeID string, pathSuffix string, exposedTool string) (MCPToolIDs, bool) {
	scopeIndex, ok := r.findScope(scopeID)
	if !ok {
		return MCPToolIDs{}, false
	}
	path, ok := r.findMCPPath(scopeIndex, pathSuffix)
	if !ok {
		return MCPToolIDs{}, false
	}
	count, offset := r.toolsetRecord(path.toolsetID)
	for i := uint32(0); i < count; i++ {
		entryBase := int(offset) + int(i)*mcpToolBindingLen
		exposedSID := r.read32(entryBase)
		if !r.stringEqual(exposedSID, exposedTool) {
			continue
		}
		serverID := r.read32(entryBase + 4)
		server := r.mcpServer(serverID)
		return MCPToolIDs{
			ExposedNameSID:    exposedSID,
			ServerID:          serverID,
			ServerSID:         server.idSID,
			ServerEndpointSID: server.endpointSID,
			ToolSID:           r.read32(entryBase + 8),
			SecretSID:         r.read32(entryBase + 12),
			AuthTypeSID:       r.read32(entryBase + 16),
		}, true
	}
	return MCPToolIDs{}, false
}

// ResolveMCPInitialize materializes the upstream server set needed for MCP
// initialize. Use ResolveMCPInitializeIDs on lower-allocation data paths.
func (r Reader) ResolveMCPInitialize(scopeID string, pathSuffix string) (MCPInitializeResult, bool) {
	ids, ok := r.ResolveMCPInitializeIDs(scopeID, pathSuffix)
	if !ok {
		return MCPInitializeResult{}, false
	}
	servers := make([]MCPUpstreamServer, 0, len(ids.Servers))
	for _, server := range ids.Servers {
		servers = append(servers, MCPUpstreamServer{
			Server:    r.String(server.ServerSID),
			Endpoint:  r.String(server.EndpointSID),
			SecretRef: r.String(server.SecretSID),
			AuthType:  r.String(server.AuthTypeSID),
		})
	}
	return MCPInitializeResult{
		Path:    r.String(ids.PathSID),
		Auth:    r.materializeMCPProfileAuth(ids.Auth),
		Servers: servers,
	}, true
}

// ResolveMCP materializes an MCP path lookup as strings. It is intended for
// diagnostics and control surfaces; use ResolveMCPIDs or ResolveMCPToolIDs for
// lower-allocation data paths.
func (r Reader) ResolveMCP(scopeID string, pathSuffix string) (MCPResult, bool) {
	ids, ok := r.ResolveMCPIDs(scopeID, pathSuffix)
	if !ok {
		return MCPResult{}, false
	}
	tools := make([]MCPTool, 0, len(ids.Tools))
	for _, tool := range ids.Tools {
		tools = append(tools, MCPTool{
			ExposedName:    r.String(tool.ExposedNameSID),
			Server:         r.String(tool.ServerSID),
			ServerEndpoint: r.String(tool.ServerEndpointSID),
			Tool:           r.String(tool.ToolSID),
			SecretRef:      r.String(tool.SecretSID),
			AuthType:       r.String(tool.AuthTypeSID),
		})
	}
	return MCPResult{
		Path:  r.String(ids.PathSID),
		Auth:  r.materializeMCPProfileAuth(ids.Auth),
		Tools: tools,
	}, true
}

// String returns the string-table value for id. It returns an empty string for an
// invalid string ID, matching the internal reader behavior.
func (r Reader) String(id uint32) string {
	return r.string(id)
}

// BlobSize reports the number of bytes retained by this Reader.
func (r Reader) BlobSize() int {
	return len(r.blob)
}

// ScopeIDs returns all scope IDs stored in the pack in sorted order.
func (r Reader) ScopeIDs() []string {
	count := r.sectionCount(r.scopesOff)
	base := int(r.scopesOff) + 4
	ids := make([]string, 0, count)
	for i := uint32(0); i < count; i++ {
		ids = append(ids, r.string(r.read32(base+int(i)*scopeLen)))
	}
	sort.Strings(ids)
	return ids
}

func (r Reader) providerInfo(id uint32) ProviderInfo {
	provider := r.provider(id)
	return ProviderInfo{
		ID:         r.String(provider.idSID),
		Kind:       r.String(provider.kindSID),
		Endpoint:   r.String(provider.endpointSID),
		SecretRef:  r.String(provider.secretSID),
		AuthType:   r.String(provider.authTypeSID),
		PathPrefix: r.String(provider.pathPrefixSID),
		Extra:      parseStringMap(r.String(provider.extraSID)),
	}
}

func (r Reader) mcpServerInfo(id uint32) MCPServerInfo {
	server := r.mcpServer(id)
	return MCPServerInfo{
		ID:        r.String(server.idSID),
		Endpoint:  r.String(server.endpointSID),
		SecretRef: r.String(server.secretSID),
		AuthType:  r.String(server.authTypeSID),
	}
}

func (r Reader) materializeMCPProfileAuth(ids MCPProfileAuthIDs) MCPProfileAuth {
	return MCPProfileAuth{
		Type:      r.String(ids.TypeSID),
		Provider:  r.String(ids.ProviderSID),
		SecretRef: r.String(ids.SecretSID),
		OAuth:     r.materializeMCPOAuthConfig(ids.OAuth),
	}
}

func (r Reader) materializeMCPOAuthConfig(ids MCPOAuthConfigIDs) MCPOAuthConfig {
	return MCPOAuthConfig{
		TokenEndpoint: r.String(ids.TokenEndpointSID),
		ClientID:      r.String(ids.ClientIDSID),
		Audience:      r.String(ids.AudienceSID),
		Scopes:        splitOAuthScopes(r.String(ids.ScopesSID)),
	}
}

func (r Reader) modelInfo(id uint32) ModelInfo {
	model := r.model(id)
	provider := r.provider(model.providerID)
	return ModelInfo{
		ID:                        r.String(model.idSID),
		Provider:                  r.String(provider.idSID),
		Name:                      r.String(model.nameSID),
		Mode:                      r.String(model.modeSID),
		Capabilities:              splitCapabilities(r.String(model.capabilitiesSID)),
		Modalities:                splitModalities(r.String(model.modalitiesSID)),
		AdditionalPricePerMillion: splitCatalogObject(r.String(model.additionalPriceSID)),
		Limits:                    splitCatalogObject(r.String(model.limitsSID)),
		MetadataJSON:              r.String(model.metadataSID),
	}
}

// PrincipalRoutes returns inspector records for every principal/requested-model
// route in scopeID. The boolean is false when the scope is not present.
func (r Reader) PrincipalRoutes(scopeID string) ([]PrincipalRoute, bool) {
	scope, ok := r.findScope(scopeID)
	if !ok {
		return nil, false
	}
	routes := make([]PrincipalRoute, 0, scope.principalCount)
	base := int(scope.principalOffset)
	for i := uint32(0); i < scope.principalCount; i++ {
		entryBase := base + int(i)*principalLen
		principal := principalRef{
			slugSID:          r.read32(entryBase + 8),
			routeID:          r.read32(entryBase + 12),
			rateID:           r.read32(entryBase + 16),
			credentialCount:  r.read32(entryBase + 24),
			credentialOffset: r.read32(entryBase + 28),
		}
		var targetOrdinal uint32
		plan, ok := r.routePlanIDs(principal.routeID, principal, &targetOrdinal)
		if !ok {
			continue
		}
		target, ok := firstTargetPlanIDs(plan)
		if !ok {
			continue
		}
		routes = append(routes, PrincipalRoute{
			ScopeID:        scopeID,
			PrincipalSlug:  r.string(principal.slugSID),
			RequestedModel: r.string(r.read32(entryBase + 20)),
			RouteKind:      plan.Kind,
			Provider:       r.string(target.ProviderSID),
			Model:          r.string(target.ModelSID),
			SecretRef:      r.string(target.SecretSID),
			Rate:           ratePolicyFromIDs(r, r.rateIDs(principal.rateID)),
		})
	}
	sort.Slice(routes, func(i, j int) bool {
		if routes[i].PrincipalSlug == routes[j].PrincipalSlug {
			return routes[i].RequestedModel < routes[j].RequestedModel
		}
		return routes[i].PrincipalSlug < routes[j].PrincipalSlug
	})
	return routes, true
}

// Principals returns unique principal slugs in scopeID with their requested
// model allowsets. The boolean is false when the scope is not present.
func (r Reader) Principals(scopeID string) ([]PrincipalInfo, bool) {
	routes, ok := r.PrincipalRoutes(scopeID)
	if !ok {
		return nil, false
	}
	bySlug := map[string][]string{}
	for _, route := range routes {
		bySlug[route.PrincipalSlug] = append(bySlug[route.PrincipalSlug], route.RequestedModel)
	}
	principals := make([]PrincipalInfo, 0, len(bySlug))
	for slug, models := range bySlug {
		sort.Strings(models)
		principals = append(principals, PrincipalInfo{
			ScopeID:         scopeID,
			PrincipalSlug:   slug,
			RequestedModels: models,
		})
	}
	sort.Slice(principals, func(i, j int) bool {
		return principals[i].PrincipalSlug < principals[j].PrincipalSlug
	})
	return principals, true
}

// MCPPaths returns inspector records for every MCP path in scopeID. The boolean
// is false when the scope is not present.
func (r Reader) MCPPaths(scopeID string) ([]MCPPath, bool) {
	scope, ok := r.findScope(scopeID)
	if !ok {
		return nil, false
	}
	paths := make([]MCPPath, 0, scope.mcpPathCount)
	base := int(scope.mcpPathOffset)
	for i := uint32(0); i < scope.mcpPathCount; i++ {
		entryBase := base + int(i)*mcpPathLen
		path := mcpPathRef{
			pathSID:   r.read32(entryBase + 8),
			toolsetID: r.read32(entryBase + 12),
			auth:      r.mcpProfileAuth(entryBase + 16),
		}
		result, ok := r.ResolveMCP(scopeID, r.string(path.pathSID))
		if !ok {
			continue
		}
		paths = append(paths, MCPPath{
			ScopeID: scopeID,
			Path:    result.Path,
			Auth:    result.Auth,
			Tools:   result.Tools,
		})
	}
	sort.Slice(paths, func(i, j int) bool {
		return paths[i].Path < paths[j].Path
	})
	return paths, true
}

func ratePolicyFromIDs(r Reader, ids RatePolicyIDs) RatePolicy {
	return RatePolicy{
		USDPerDayCents: ids.USDPerDayCents,
		RPM:            ids.RPM,
		OnExceed:       r.string(ids.OnExceedSID),
	}
}

// builder interns strings while Build walks normalized input. Binary tables store
// uint32 string IDs instead of repeating string bytes in every record.
type builder struct {
	stringIndex map[string]uint32
	strings     []string
}

type compiledRoute struct {
	plan          RoutePlan
	chainChildIDs []uint32
	splitChildIDs []uint32
}

type compiledScope struct {
	id               string
	principalEntries []compiledPrincipalEntry
	mcpProfiles      []compiledMCPProfile
}

type compiledPrincipalEntry struct {
	slug                string
	requestedModel      string
	lookupHash          uint64
	routeID             uint32
	rate                RatePolicy
	credentialSlotCount uint32
	credentialSlot0     compiledCredentialSlot
	credentialSlotExtra []compiledCredentialSlot
}

type compiledCredentialSlot struct {
	targetOrdinal uint32
	secretRef     string
}

type compiledCredentialSlots struct {
	count uint32
	first compiledCredentialSlot
	extra []compiledCredentialSlot
}

type compiledMCPProfile struct {
	path      string
	pathHash  uint64
	toolsetID uint32
	auth      MCPProfileAuth
}

type routeInterner struct {
	targetIDs map[targetRouteKey]uint32
	chainIDs  map[chainRouteKey]uint32
	splitIDs  map[splitRouteKey]uint32
	routeIDs  map[string]uint32
}

type targetRouteKey struct {
	providerID uint32
	modelID    uint32
}

type chainRouteKey struct {
	retrySID   uint32
	timeoutMS  uint32
	child0     uint32
	child1     uint32
	child2     uint32
	childCount uint8
	hasRetry   bool
}

type splitRouteKey struct {
	child0     uint32
	child1     uint32
	child2     uint32
	weight0    uint32
	weight1    uint32
	weight2    uint32
	childCount uint8
}

func newBuilder() *builder {
	return &builder{
		stringIndex: map[string]uint32{"": 0},
		strings:     []string{""},
	}
}

func newRouteInterner() *routeInterner {
	return &routeInterner{
		targetIDs: map[targetRouteKey]uint32{},
		chainIDs:  map[chainRouteKey]uint32{},
		splitIDs:  map[splitRouteKey]uint32{},
		routeIDs:  map[string]uint32{},
	}
}

func (b *builder) stringID(value string) uint32 {
	if id, ok := b.stringIndex[value]; ok {
		return id
	}
	id := uint32(len(b.strings))
	b.stringIndex[value] = id
	b.strings = append(b.strings, value)
	return id
}

func internMCPProfileAuth(b *builder, auth MCPProfileAuth) {
	b.stringID(auth.Type)
	b.stringID(auth.Provider)
	b.stringID(auth.SecretRef)
	b.stringID(auth.OAuth.TokenEndpoint)
	b.stringID(auth.OAuth.ClientID)
	b.stringID(auth.OAuth.Audience)
	b.stringID(oauthScopesKey(auth.OAuth.Scopes))
}

func sortedProviders(values []Provider) []Provider {
	out := append([]Provider{}, values...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func sortedModels(values []Model) []Model {
	out := append([]Model{}, values...)
	sort.Slice(out, func(i, j int) bool {
		leftHash := hashString(out[i].ID)
		rightHash := hashString(out[j].ID)
		if leftHash == rightHash {
			return out[i].ID < out[j].ID
		}
		return leftHash < rightHash
	})
	return out
}

func sortedMCPServers(values []MCPServer) []MCPServer {
	out := append([]MCPServer{}, values...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// canonicalToolset gives semantically identical profile toolsets one stable
// representation so Build can deduplicate them.
func canonicalToolset(values []MCPToolBinding) []MCPToolBinding {
	if mcpToolsetSorted(values) {
		return values
	}
	out := append([]MCPToolBinding{}, values...)
	sort.Slice(out, func(i, j int) bool { return mcpToolBindingLess(out[i], out[j]) })
	return out
}

func mcpToolsetSorted(values []MCPToolBinding) bool {
	for i := 1; i < len(values); i++ {
		if mcpToolBindingLess(values[i], values[i-1]) {
			return false
		}
	}
	return true
}

func mcpToolBindingLess(left MCPToolBinding, right MCPToolBinding) bool {
	if left.ExposedName != right.ExposedName {
		return left.ExposedName < right.ExposedName
	}
	if left.Server != right.Server {
		return left.Server < right.Server
	}
	if left.Tool != right.Tool {
		return left.Tool < right.Tool
	}
	if left.SecretRef != right.SecretRef {
		return left.SecretRef < right.SecretRef
	}
	if left.AuthType != right.AuthType {
		return left.AuthType < right.AuthType
	}
	return false
}

func validateMCPToolAuthLinear(profilePath string, previous []MCPToolBinding, tool MCPToolBinding) error {
	for _, existing := range previous {
		if existing.Server == tool.Server && !sameMCPToolAuth(existing, tool) {
			return fmt.Errorf("mcp profile %q has conflicting auth for server %q", profilePath, tool.Server)
		}
	}
	return nil
}

func sameMCPToolAuth(left MCPToolBinding, right MCPToolBinding) bool {
	return left.SecretRef == right.SecretRef &&
		left.AuthType == right.AuthType
}

// principalRoutes expands the compatibility Route field into the same shape as
// ModelRoutes. New callers should prefer ModelRoutes so each requested model can
// have an explicit final target.
func principalRoutes(principal Principal) map[string]RoutePlan {
	if len(principal.ModelRoutes) > 0 {
		return principal.ModelRoutes
	}
	if principal.Route.Model == "" {
		return map[string]RoutePlan{}
	}
	return map[string]RoutePlan{
		principal.Route.Model: principal.Route,
	}
}

func countPrincipalRoutes(principals []Principal) int {
	count := 0
	for _, principal := range principals {
		count += len(principalRoutes(principal))
	}
	return count
}

func normalizeRoutePlan(route RoutePlan) RoutePlan {
	if route.Kind == "" {
		route.Kind = RouteKindTarget
	}
	if route.Kind == RouteKindTarget {
		route.Retry = nil
		route.Children = nil
		route.Split = nil
	}
	return route
}

func internRoute(
	route RoutePlan,
	b *builder,
	providerIDs map[string]uint32,
	modelIDs map[string]uint32,
	routeIDs *routeInterner,
	routes *[]compiledRoute,
) (uint32, error) {
	normalized := normalizeRoutePlan(route)
	if normalized.Kind == RouteKindTarget {
		modelID, ok := modelIDs[normalized.Model]
		if !ok {
			return 0, fmt.Errorf("references unknown target model %q", normalized.Model)
		}
		providerID, ok := providerIDs[normalized.Provider]
		if !ok {
			return 0, fmt.Errorf("references unknown provider %q", normalized.Provider)
		}
		key := targetRouteKey{providerID: providerID, modelID: modelID}
		if id, ok := routeIDs.targetIDs[key]; ok {
			return id, nil
		}
		compiled := compiledRoute{plan: normalized}
		compiled.plan.SecretRef = ""
		id := uint32(len(*routes))
		routeIDs.targetIDs[key] = id
		*routes = append(*routes, compiled)
		return id, nil
	}

	if normalized.Kind == RouteKindChain && len(normalized.Children) <= 3 {
		return internShortChainRoute(normalized, b, providerIDs, modelIDs, routeIDs, routes)
	}
	if normalized.Kind == RouteKindSplit && len(normalized.Split) <= 3 {
		return internShortSplitRoute(normalized, b, providerIDs, modelIDs, routeIDs, routes)
	}

	key := routeKey(normalized)
	if id, ok := routeIDs.routeIDs[key]; ok {
		return id, nil
	}
	compiled := compiledRoute{plan: normalized}
	switch normalized.Kind {
	case RouteKindChain:
		if len(normalized.Children) == 0 {
			return 0, errors.New("chain route node must not be empty")
		}
		if normalized.Retry != nil {
			b.stringID(normalized.Retry.RetryOn)
		}
		compiled.chainChildIDs = make([]uint32, 0, len(normalized.Children))
		for index, child := range normalized.Children {
			childID, err := internRoute(child, b, providerIDs, modelIDs, routeIDs, routes)
			if err != nil {
				return 0, fmt.Errorf("chain[%d]: %w", index, err)
			}
			compiled.chainChildIDs = append(compiled.chainChildIDs, childID)
		}
	case RouteKindSplit:
		if len(normalized.Split) == 0 {
			return 0, errors.New("split route node must not be empty")
		}
		compiled.splitChildIDs = make([]uint32, 0, len(normalized.Split))
		for index, child := range normalized.Split {
			if child.Weight == 0 {
				return 0, fmt.Errorf("split[%d]: weight must be positive", index)
			}
			childID, err := internRoute(child.Plan, b, providerIDs, modelIDs, routeIDs, routes)
			if err != nil {
				return 0, fmt.Errorf("split[%d]: %w", index, err)
			}
			compiled.splitChildIDs = append(compiled.splitChildIDs, childID)
		}
	default:
		return 0, fmt.Errorf("unsupported route node kind %q", normalized.Kind)
	}
	id := uint32(len(*routes))
	routeIDs.routeIDs[key] = id
	*routes = append(*routes, compiled)
	return id, nil
}

func internShortChainRoute(
	route RoutePlan,
	b *builder,
	providerIDs map[string]uint32,
	modelIDs map[string]uint32,
	routeIDs *routeInterner,
	routes *[]compiledRoute,
) (uint32, error) {
	if len(route.Children) == 0 {
		return 0, errors.New("chain route node must not be empty")
	}
	key := chainRouteKey{childCount: uint8(len(route.Children))}
	if route.Retry != nil {
		key.hasRetry = true
		key.retrySID = b.stringID(route.Retry.RetryOn)
		key.timeoutMS = route.Retry.PerTryTimeoutMS
	}

	var childIDs [3]uint32
	for index, child := range route.Children {
		childID, err := internRoute(child, b, providerIDs, modelIDs, routeIDs, routes)
		if err != nil {
			return 0, fmt.Errorf("chain[%d]: %w", index, err)
		}
		childIDs[index] = childID
	}
	key.child0 = childIDs[0]
	key.child1 = childIDs[1]
	key.child2 = childIDs[2]
	if id, ok := routeIDs.chainIDs[key]; ok {
		return id, nil
	}

	compiled := compiledRoute{
		plan:          route,
		chainChildIDs: append([]uint32(nil), childIDs[:len(route.Children)]...),
	}
	id := uint32(len(*routes))
	routeIDs.chainIDs[key] = id
	*routes = append(*routes, compiled)
	return id, nil
}

func internShortSplitRoute(
	route RoutePlan,
	b *builder,
	providerIDs map[string]uint32,
	modelIDs map[string]uint32,
	routeIDs *routeInterner,
	routes *[]compiledRoute,
) (uint32, error) {
	if len(route.Split) == 0 {
		return 0, errors.New("split route node must not be empty")
	}
	key := splitRouteKey{childCount: uint8(len(route.Split))}
	var childIDs [3]uint32
	var weights [3]uint32
	for index, child := range route.Split {
		if child.Weight == 0 {
			return 0, fmt.Errorf("split[%d]: weight must be positive", index)
		}
		childID, err := internRoute(child.Plan, b, providerIDs, modelIDs, routeIDs, routes)
		if err != nil {
			return 0, fmt.Errorf("split[%d]: %w", index, err)
		}
		childIDs[index] = childID
		weights[index] = child.Weight
	}
	key.child0 = childIDs[0]
	key.child1 = childIDs[1]
	key.child2 = childIDs[2]
	key.weight0 = weights[0]
	key.weight1 = weights[1]
	key.weight2 = weights[2]
	if id, ok := routeIDs.splitIDs[key]; ok {
		return id, nil
	}

	compiled := compiledRoute{
		plan:          route,
		splitChildIDs: append([]uint32(nil), childIDs[:len(route.Split)]...),
	}
	id := uint32(len(*routes))
	routeIDs.splitIDs[key] = id
	*routes = append(*routes, compiled)
	return id, nil
}

func routeKey(route RoutePlan) string {
	route = normalizeRoutePlan(route)
	var builder strings.Builder
	writeRouteKey(&builder, route)
	return builder.String()
}

func writeRouteKey(builder *strings.Builder, route RoutePlan) {
	route = normalizeRoutePlan(route)
	builder.WriteString(string(route.Kind))
	builder.WriteByte('\x00')
	switch route.Kind {
	case RouteKindTarget:
		builder.WriteString(route.Provider)
		builder.WriteByte('\x00')
		builder.WriteString(route.Model)
	case RouteKindChain:
		if route.Retry != nil {
			builder.WriteString(route.Retry.RetryOn)
			builder.WriteByte('\x00')
			builder.WriteString(strconv.FormatUint(uint64(route.Retry.PerTryTimeoutMS), 10))
		}
		for _, child := range route.Children {
			builder.WriteByte('\x00')
			writeRouteKey(builder, child)
		}
	case RouteKindSplit:
		for _, child := range route.Split {
			builder.WriteByte('\x00')
			builder.WriteString(strconv.FormatUint(uint64(child.Weight), 10))
			builder.WriteByte('\x00')
			writeRouteKey(builder, child.Plan)
		}
	}
}

func routeCredentialSlots(route RoutePlan) (compiledCredentialSlots, error) {
	var slots compiledCredentialSlots
	var targetOrdinal uint32
	if err := appendRouteCredentialSlots(route, &targetOrdinal, &slots); err != nil {
		return compiledCredentialSlots{}, err
	}
	return slots, nil
}

func appendRouteCredentialSlots(
	route RoutePlan,
	targetOrdinal *uint32,
	slots *compiledCredentialSlots,
) error {
	normalized := normalizeRoutePlan(route)
	switch normalized.Kind {
	case RouteKindTarget:
		if normalized.SecretRef != "" {
			slots.append(compiledCredentialSlot{
				targetOrdinal: *targetOrdinal,
				secretRef:     normalized.SecretRef,
			})
		}
		(*targetOrdinal)++
	case RouteKindChain:
		for index, child := range normalized.Children {
			if err := appendRouteCredentialSlots(child, targetOrdinal, slots); err != nil {
				return fmt.Errorf("chain[%d]: %w", index, err)
			}
		}
	case RouteKindSplit:
		for index, child := range normalized.Split {
			if err := appendRouteCredentialSlots(child.Plan, targetOrdinal, slots); err != nil {
				return fmt.Errorf("split[%d]: %w", index, err)
			}
		}
	default:
		return fmt.Errorf("unsupported route node kind %q", normalized.Kind)
	}
	return nil
}

func (slots *compiledCredentialSlots) append(slot compiledCredentialSlot) {
	if slots.count == 0 {
		slots.first = slot
	} else {
		slots.extra = append(slots.extra, slot)
	}
	slots.count++
}

func capabilitiesKey(values []string) string {
	if len(values) == 0 {
		return ""
	}
	out := append([]string{}, values...)
	sort.Strings(out)
	return strings.Join(out, "\x00")
}

func splitCapabilities(value string) []string {
	if value == "" {
		return nil
	}
	return strings.Split(value, "\x00")
}

func modalitiesJSON(value ModelModalities) string {
	if len(value.Input) == 0 && len(value.Output) == 0 {
		return ""
	}
	data, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(data)
}

func splitModalities(value string) ModelModalities {
	if value == "" {
		return ModelModalities{}
	}
	var out ModelModalities
	_ = json.Unmarshal([]byte(value), &out)
	return out
}

func catalogObjectJSON(value ModelCatalogObject) string {
	if value == nil {
		return ""
	}
	data, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(data)
}

func splitCatalogObject(value string) ModelCatalogObject {
	if value == "" {
		return nil
	}
	var out ModelCatalogObject
	if err := json.Unmarshal([]byte(value), &out); err != nil {
		return nil
	}
	for key, raw := range out {
		out[key] = append(json.RawMessage(nil), raw...)
	}
	return out
}

func internMCPToolset(
	hash uint64,
	canonicalTools []MCPToolBinding,
	toolsetIDs map[uint64][]uint32,
	toolsets *[][]MCPToolBinding,
) uint32 {
	for _, id := range toolsetIDs[hash] {
		if mcpToolsetsEqual(canonicalTools, (*toolsets)[id]) {
			return id
		}
	}
	id := uint32(len(*toolsets))
	toolsetIDs[hash] = append(toolsetIDs[hash], id)
	*toolsets = append(*toolsets, canonicalTools)
	return id
}

func newToolsetHash() uint64 {
	return 14695981039346656037
}

func hashToolsetID(hash uint64, id uint32) uint64 {
	hash ^= uint64(id)
	hash *= 1099511628211
	hash ^= uint64(id >> 8)
	hash *= 1099511628211
	hash ^= uint64(id >> 16)
	hash *= 1099511628211
	hash ^= uint64(id >> 24)
	hash *= 1099511628211
	hash ^= 0
	hash *= 1099511628211
	return hash
}

func mcpToolsetsEqual(left []MCPToolBinding, right []MCPToolBinding) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i].ExposedName != right[i].ExposedName ||
			left[i].Server != right[i].Server ||
			left[i].Tool != right[i].Tool ||
			!sameMCPToolAuth(left[i], right[i]) {
			return false
		}
	}
	return true
}

func oauthScopesKey(scopes []string) string {
	if len(scopes) == 0 {
		return ""
	}
	return strings.Join(scopes, "\x00")
}

func splitOAuthScopes(value string) []string {
	if value == "" {
		return nil
	}
	return strings.Split(value, "\x00")
}

func stringMapJSON(values map[string]string) string {
	if len(values) == 0 {
		return ""
	}
	data, err := json.Marshal(values)
	if err != nil {
		return ""
	}
	return string(data)
}

func parseStringMap(value string) map[string]string {
	if value == "" {
		return nil
	}
	out := map[string]string{}
	if err := json.Unmarshal([]byte(value), &out); err != nil {
		return nil
	}
	return out
}

// writeStrings writes the shared string table as count, data length, offsets,
// and contiguous string bytes. The offsets array has count+1 entries so string
// length is derived from adjacent offsets.
func writeStrings(out *bytes.Buffer, stringsTable []string) {
	putU32(out, uint32(len(stringsTable)))
	dataLen := 0
	for _, value := range stringsTable {
		dataLen += len(value)
	}
	out.Grow(4 + (len(stringsTable)+1)*4 + dataLen)
	putU32(out, uint32(dataLen))
	offset := 0
	for _, value := range stringsTable {
		putU32(out, uint32(offset))
		offset += len(value)
	}
	putU32(out, uint32(offset))
	for _, value := range stringsTable {
		out.WriteString(value)
	}
}

// writeProviders writes fixed-width provider records in provider ID order.
func writeProviders(out *bytes.Buffer, b *builder, providers []Provider) {
	putU32(out, uint32(len(providers)))
	for _, provider := range providers {
		putU32(out, b.stringID(provider.ID))
		putU32(out, b.stringID(provider.Kind))
		putU32(out, b.stringID(provider.Endpoint))
		putU32(out, b.stringID(provider.SecretRef))
		putU32(out, b.stringID(provider.AuthType))
		putU32(out, b.stringID(provider.PathPrefix))
		putU32(out, b.stringID(stringMapJSON(provider.Extra)))
	}
}

// writeModels writes fixed-width model records sorted by lookup hash. The reader
// binary-searches this table while validating requested models.
func writeModels(out *bytes.Buffer, b *builder, models []Model, providerIDs map[string]uint32) {
	putU32(out, uint32(len(models)))
	for _, model := range models {
		putU64(out, hashString(model.ID))
		putU32(out, b.stringID(model.ID))
		putU32(out, providerIDs[model.Provider])
		putU32(out, b.stringID(model.Name))
		putU32(out, b.stringID(model.Mode))
		putU32(out, b.stringID(capabilitiesKey(model.Capabilities)))
		putU32(out, b.stringID(modalitiesJSON(model.Modalities)))
		putU32(out, b.stringID(catalogObjectJSON(model.AdditionalPricePerMillion)))
		putU32(out, b.stringID(catalogObjectJSON(model.Limits)))
		putU32(out, b.stringID(model.MetadataJSON))
	}
}

// writeRoutes writes deduplicated LLM route trees. Principal index records
// reference this table by route ID. Chain and split nodes point into a child
// table stored immediately after the fixed-width route records.
func writeRoutes(out *bytes.Buffer, b *builder, routes []compiledRoute, providerIDs map[string]uint32, modelIDs map[string]uint32) {
	putU32(out, uint32(len(routes)))
	records := make([]routeRef, len(routes))
	var children bytes.Buffer
	baseOffset := uint32(out.Len() + len(routes)*routeLen)
	for i, compiled := range routes {
		route := compiled.plan
		switch route.Kind {
		case RouteKindTarget:
			records[i] = routeRef{
				kind:       routeKindTargetID,
				providerID: providerIDs[route.Provider],
				modelID:    modelIDs[route.Model],
				secretSID:  b.stringID(route.SecretRef),
			}
		case RouteKindChain:
			records[i] = routeRef{
				kind:            routeKindChainID,
				childCount:      uint32(len(route.Children)),
				childOffset:     baseOffset + uint32(children.Len()),
				retryOnSID:      retryOnSID(b, route.Retry),
				perTryTimeoutMS: retryTimeout(route.Retry),
			}
			for _, childID := range compiled.chainChildIDs {
				putU32(&children, 0)
				putU32(&children, childID)
			}
		case RouteKindSplit:
			records[i] = routeRef{
				kind:        routeKindSplitID,
				childCount:  uint32(len(route.Split)),
				childOffset: baseOffset + uint32(children.Len()),
			}
			for childIndex, child := range route.Split {
				putU32(&children, child.Weight)
				putU32(&children, compiled.splitChildIDs[childIndex])
			}
		}
	}
	for _, record := range records {
		putU32(out, record.kind)
		switch record.kind {
		case routeKindTargetID:
			putU32(out, record.providerID)
			putU32(out, record.modelID)
			putU32(out, record.secretSID)
			putU32(out, 0)
			putU32(out, 0)
		case routeKindChainID:
			putU32(out, record.childCount)
			putU32(out, record.childOffset)
			putU32(out, record.retryOnSID)
			putU32(out, record.perTryTimeoutMS)
			putU32(out, 0)
		case routeKindSplitID:
			putU32(out, record.childCount)
			putU32(out, record.childOffset)
			putU32(out, 0)
			putU32(out, 0)
			putU32(out, 0)
		}
	}
	out.Write(children.Bytes())
}

func retryOnSID(b *builder, retry *RetryPolicy) uint32 {
	if retry == nil {
		return b.stringID("")
	}
	return b.stringID(retry.RetryOn)
}

func retryTimeout(retry *RetryPolicy) uint32 {
	if retry == nil {
		return 0
	}
	return retry.PerTryTimeoutMS
}

// writeRates writes deduplicated immutable rate-limit metadata. Runtime counters
// are intentionally outside the pack.
func writeRates(out *bytes.Buffer, b *builder, rateIDs map[RatePolicy]uint32) {
	putU32(out, uint32(len(rateIDs)))
	rates := make([]RatePolicy, len(rateIDs))
	for rate, id := range rateIDs {
		rates[id] = rate
	}
	for _, rate := range rates {
		putU64(out, rate.USDPerDayCents)
		putU32(out, rate.RPM)
		putU32(out, b.stringID(rate.OnExceed))
	}
}

// writeMCPServers writes fixed-width upstream MCP server records.
func writeMCPServers(out *bytes.Buffer, b *builder, servers []MCPServer) {
	putU32(out, uint32(len(servers)))
	for _, server := range servers {
		putU32(out, b.stringID(server.ID))
		putU32(out, b.stringID(server.Endpoint))
		putU32(out, b.stringID(server.SecretRef))
		putU32(out, b.stringID(server.AuthType))
	}
}

// writeMCPToolsets writes deduplicated MCP toolsets followed by their contiguous
// tool binding records.
func writeMCPToolsets(out *bytes.Buffer, b *builder, toolsets [][]MCPToolBinding, serverIDs map[string]uint32) {
	putU32(out, uint32(len(toolsets)))
	totalBindings := 0
	for _, toolset := range toolsets {
		totalBindings += len(toolset)
	}
	out.Grow(len(toolsets)*mcpToolsetLen + totalBindings*mcpToolBindingLen)
	baseOffset := uint32(out.Len() + len(toolsets)*mcpToolsetLen)
	bindingOffset := baseOffset
	for _, toolset := range toolsets {
		putU32(out, uint32(len(toolset)))
		putU32(out, bindingOffset)
		bindingOffset += uint32(len(toolset) * mcpToolBindingLen)
	}
	for _, toolset := range toolsets {
		for _, binding := range toolset {
			putU32(out, b.stringID(binding.ExposedName))
			putU32(out, serverIDs[binding.Server])
			putU32(out, b.stringID(binding.Tool))
			putU32(out, b.stringID(binding.SecretRef))
			putU32(out, b.stringID(binding.AuthType))
		}
	}
}

// writeScopes writes scope records plus the two scope-local indexes they point
// at: principal/model routes and MCP path suffixes.
func writeScopes(out *bytes.Buffer, b *builder, scopes []compiledScope, rateIDs map[RatePolicy]uint32) (uint32, uint32) {
	putU32(out, uint32(len(scopes)))
	scopeRecords := make([]scopeRef, len(scopes))
	totalPrincipalRecords := 0
	totalCredentialRecords := 0
	totalMCPPathRecords := 0
	for i := range scopes {
		principalEntries := scopes[i].principalEntries
		sort.Slice(principalEntries, func(i, j int) bool {
			if principalEntries[i].lookupHash == principalEntries[j].lookupHash {
				if principalEntries[i].slug == principalEntries[j].slug {
					return principalEntries[i].requestedModel < principalEntries[j].requestedModel
				}
				return principalEntries[i].slug < principalEntries[j].slug
			}
			return principalEntries[i].lookupHash < principalEntries[j].lookupHash
		})
		totalPrincipalRecords += len(principalEntries)
		for _, principal := range principalEntries {
			totalCredentialRecords += int(principal.credentialSlotCount)
		}
		totalMCPPathRecords += len(scopes[i].mcpProfiles)
	}
	out.Grow(len(scopes)*scopeLen + totalPrincipalRecords*principalLen + totalCredentialRecords*credentialLen + totalMCPPathRecords*mcpPathLen)
	scopeRecordsStart := out.Len()
	out.Write(make([]byte, len(scopes)*scopeLen))
	principalsOff := uint32(out.Len())
	credentialsOff := principalsOff + uint32(totalPrincipalRecords*principalLen)
	credentialIndex := 0

	for i := range scopes {
		principalEntries := scopes[i].principalEntries
		scopeRecords[i] = scopeRef{
			sid:             b.stringID(scopes[i].id),
			principalCount:  uint32(len(principalEntries)),
			principalOffset: uint32(out.Len()),
		}
		for _, principal := range principalEntries {
			putU64(out, principal.lookupHash)
			putU32(out, b.stringID(principal.slug))
			putU32(out, principal.routeID)
			putU32(out, rateIDs[principal.rate])
			putU32(out, b.stringID(principal.requestedModel))
			putU32(out, principal.credentialSlotCount)
			putU32(out, credentialsOff+uint32(credentialIndex*credentialLen))
			credentialIndex += int(principal.credentialSlotCount)
		}
	}

	for i := range scopes {
		for _, principal := range scopes[i].principalEntries {
			if principal.credentialSlotCount > 0 {
				putU32(out, principal.credentialSlot0.targetOrdinal)
				putU32(out, b.stringID(principal.credentialSlot0.secretRef))
			}
			for _, slot := range principal.credentialSlotExtra {
				putU32(out, slot.targetOrdinal)
				putU32(out, b.stringID(slot.secretRef))
			}
		}
	}

	mcpPathsOff := uint32(out.Len())
	for i := range scopes {
		profiles := scopes[i].mcpProfiles
		sort.Slice(profiles, func(i, j int) bool {
			if profiles[i].pathHash == profiles[j].pathHash {
				return profiles[i].path < profiles[j].path
			}
			return profiles[i].pathHash < profiles[j].pathHash
		})
		scopeRecords[i].mcpPathCount = uint32(len(profiles))
		scopeRecords[i].mcpPathOffset = uint32(out.Len())
		for _, profile := range profiles {
			putU64(out, profile.pathHash)
			putU32(out, b.stringID(profile.path))
			putU32(out, profile.toolsetID)
			putU32(out, b.stringID(profile.auth.Type))
			putU32(out, b.stringID(profile.auth.Provider))
			putU32(out, b.stringID(profile.auth.SecretRef))
			putU32(out, b.stringID(profile.auth.OAuth.TokenEndpoint))
			putU32(out, b.stringID(profile.auth.OAuth.ClientID))
			putU32(out, b.stringID(profile.auth.OAuth.Audience))
			putU32(out, b.stringID(oauthScopesKey(profile.auth.OAuth.Scopes)))
		}
	}

	blob := out.Bytes()
	for i, record := range scopeRecords {
		base := scopeRecordsStart + i*scopeLen
		put32(blob[base:base+4], record.sid)
		put32(blob[base+4:base+8], record.principalCount)
		put32(blob[base+8:base+12], record.principalOffset)
		put32(blob[base+12:base+16], record.mcpPathCount)
		put32(blob[base+16:base+20], record.mcpPathOffset)
	}
	return principalsOff, mcpPathsOff
}

type scopeRef struct {
	sid             uint32
	principalCount  uint32
	principalOffset uint32
	mcpPathCount    uint32
	mcpPathOffset   uint32
}

type principalRef struct {
	slugSID          uint32
	routeID          uint32
	rateID           uint32
	credentialCount  uint32
	credentialOffset uint32
}

type mcpPathRef struct {
	pathSID   uint32
	toolsetID uint32
	auth      MCPProfileAuthIDs
}

type modelRef struct {
	idSID              uint32
	providerID         uint32
	nameSID            uint32
	modeSID            uint32
	capabilitiesSID    uint32
	modalitiesSID      uint32
	additionalPriceSID uint32
	limitsSID          uint32
	metadataSID        uint32
}

type providerRef struct {
	idSID         uint32
	kindSID       uint32
	endpointSID   uint32
	secretSID     uint32
	authTypeSID   uint32
	pathPrefixSID uint32
	extraSID      uint32
}

type routeRef struct {
	kind            uint32
	providerID      uint32
	modelID         uint32
	secretSID       uint32
	childCount      uint32
	childOffset     uint32
	retryOnSID      uint32
	perTryTimeoutMS uint32
}

type rawModelMetadata struct {
	Model                        string          `json:"model"`
	Provider                     string          `json:"provider"`
	UpdatedAt                    string          `json:"updatedAt"`
	InputTokensPricePerMillion   *catalogDecimal `json:"inputTokensPricePerMillion"`
	OutputTokensPricePerMillion  *catalogDecimal `json:"outputTokensPricePerMillion"`
	CachedTokensPricePerMillion  *catalogDecimal `json:"cachedTokensPricePerMillion"`
	CachingTokensPricePerMillion *catalogDecimal `json:"cachingTokensPricePerMillion"`
	ContextWindow                *int64          `json:"contextWindow"`
	Capabilities                 []string        `json:"capabilities"`
	Limits                       json.RawMessage `json:"limits"`
}

type catalogDecimal string

func (d *catalogDecimal) UnmarshalJSON(data []byte) error {
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
	*d = catalogDecimal(value)
	return nil
}

type rawModelLimits struct {
	MaxOutputTokens int64 `json:"max_output_tokens"`
}

type v1ModelsResponse struct {
	Object string    `json:"object"`
	Data   []v1Model `json:"data"`
}

type v1Model struct {
	ID                  string `json:"id"`
	Object              string `json:"object"`
	Created             int64  `json:"created"`
	OwnedBy             string `json:"owned_by"`
	InputPrice          string `json:"input_price"`
	CachingPrice        string `json:"caching_price"`
	CachedPrice         string `json:"cached_price"`
	OutputPrice         string `json:"output_price"`
	MaxOutputTokens     int64  `json:"max_output_tokens"`
	ContextWindow       int64  `json:"context_window"`
	SupportsCaching     bool   `json:"supports_caching"`
	SupportsVision      bool   `json:"supports_vision"`
	SupportsComputerUse bool   `json:"supports_computer_use"`
	SupportsReasoning   bool   `json:"supports_reasoning"`
}

func v1ModelFromModelInfo(model ModelInfo) v1Model {
	out := v1Model{
		ID:      model.ID,
		Object:  "model",
		OwnedBy: "system",
	}
	var raw rawModelMetadata
	if model.MetadataJSON != "" {
		_ = json.Unmarshal([]byte(model.MetadataJSON), &raw)
	}
	if raw.Model != "" {
		out.ID = raw.Model
	}
	out.Created = unixSeconds(raw.UpdatedAt)
	out.InputPrice = pricePerToken(raw.InputTokensPricePerMillion)
	out.CachingPrice = pricePerToken(raw.CachingTokensPricePerMillion)
	out.CachedPrice = pricePerToken(raw.CachedTokensPricePerMillion)
	out.OutputPrice = pricePerToken(raw.OutputTokensPricePerMillion)
	if raw.ContextWindow != nil {
		out.ContextWindow = *raw.ContextWindow
	}
	if value, ok := catalogObjectInt64(model.Limits, "max_output_tokens"); ok {
		out.MaxOutputTokens = value
	}
	var limits rawModelLimits
	if out.MaxOutputTokens == 0 && len(raw.Limits) > 0 {
		_ = json.Unmarshal(raw.Limits, &limits)
		out.MaxOutputTokens = limits.MaxOutputTokens
	}
	capabilities := model.Capabilities
	if len(raw.Capabilities) > 0 {
		capabilities = raw.Capabilities
	}
	out.SupportsCaching = out.CachingPrice != "0" || out.CachedPrice != "0"
	out.SupportsVision = hasCapability(capabilities, "vision")
	out.SupportsComputerUse = hasCapability(capabilities, "computer_use")
	out.SupportsReasoning = hasCapability(capabilities, "reasoning")
	return out
}

func unixSeconds(value string) int64 {
	if value == "" {
		return 0
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return 0
	}
	return parsed.Unix()
}

func pricePerToken(value *catalogDecimal) string {
	if value == nil || *value == "" {
		return "0"
	}
	pricePerMillion, err := decimal.NewFromString(string(*value))
	if err != nil {
		return "0"
	}
	return trimDecimal(pricePerMillion.Div(decimal.NewFromInt(1000000)).StringFixed(12))
}

func trimDecimal(value string) string {
	value = strings.TrimRight(value, "0")
	value = strings.TrimRight(value, ".")
	if value == "" {
		return "0"
	}
	return value
}

func catalogObjectInt64(object ModelCatalogObject, key string) (int64, bool) {
	raw, ok := object[key]
	if !ok || len(raw) == 0 || string(raw) == "null" {
		return 0, false
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err == nil {
		if value, err := number.Int64(); err == nil {
			return value, true
		}
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return 0, false
	}
	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, false
	}
	return value, true
}

func hasCapability(capabilities []string, capability string) bool {
	for _, candidate := range capabilities {
		if candidate == capability {
			return true
		}
	}
	return false
}

func (r Reader) findScope(scopeID string) (scopeRef, bool) {
	count := r.sectionCount(r.scopesOff)
	base := int(r.scopesOff) + 4
	for i := uint32(0); i < count; i++ {
		ref := scopeRef{
			sid:             r.read32(base + int(i)*scopeLen),
			principalCount:  r.read32(base + int(i)*scopeLen + 4),
			principalOffset: r.read32(base + int(i)*scopeLen + 8),
			mcpPathCount:    r.read32(base + int(i)*scopeLen + 12),
			mcpPathOffset:   r.read32(base + int(i)*scopeLen + 16),
		}
		if r.stringEqual(ref.sid, scopeID) {
			return ref, true
		}
	}
	return scopeRef{}, false
}

func (r Reader) findPrincipal(scope scopeRef, slug string, requestedModel string) (principalRef, bool) {
	targetHash := principalLookupHash(slug, requestedModel)
	base := int(scope.principalOffset)
	index := sort.Search(int(scope.principalCount), func(i int) bool {
		return r.read64(base+i*principalLen) >= targetHash
	})
	for i := index; i < int(scope.principalCount); i++ {
		entryBase := base + i*principalLen
		hash := r.read64(entryBase)
		if hash != targetHash {
			break
		}
		sid := r.read32(entryBase + 8)
		requestedModelID := r.read32(entryBase + 20)
		if r.stringEqual(sid, slug) && r.stringEqual(requestedModelID, requestedModel) {
			return principalRef{
				slugSID:          sid,
				routeID:          r.read32(entryBase + 12),
				rateID:           r.read32(entryBase + 16),
				credentialCount:  r.read32(entryBase + 24),
				credentialOffset: r.read32(entryBase + 28),
			}, true
		}
	}
	return principalRef{}, false
}

func (r Reader) findMCPPath(scope scopeRef, path string) (mcpPathRef, bool) {
	targetHash := hashString(path)
	base := int(scope.mcpPathOffset)
	index := sort.Search(int(scope.mcpPathCount), func(i int) bool {
		return r.read64(base+i*mcpPathLen) >= targetHash
	})
	for i := index; i < int(scope.mcpPathCount); i++ {
		entryBase := base + i*mcpPathLen
		hash := r.read64(entryBase)
		if hash != targetHash {
			break
		}
		sid := r.read32(entryBase + 8)
		if r.stringEqual(sid, path) {
			return mcpPathRef{
				pathSID:   sid,
				toolsetID: r.read32(entryBase + 12),
				auth:      r.mcpProfileAuth(entryBase + 16),
			}, true
		}
	}
	return mcpPathRef{}, false
}

func (r Reader) mcpProfileAuth(base int) MCPProfileAuthIDs {
	return MCPProfileAuthIDs{
		TypeSID:     r.read32(base),
		ProviderSID: r.read32(base + 4),
		SecretSID:   r.read32(base + 8),
		OAuth: MCPOAuthConfigIDs{
			TokenEndpointSID: r.read32(base + 12),
			ClientIDSID:      r.read32(base + 16),
			AudienceSID:      r.read32(base + 20),
			ScopesSID:        r.read32(base + 24),
		},
	}
}

func (r Reader) findModel(modelID string) (uint32, bool) {
	targetHash := hashString(modelID)
	count := r.sectionCount(r.modelsOff)
	base := int(r.modelsOff) + 4
	index := sort.Search(int(count), func(i int) bool {
		return r.read64(base+i*modelLen) >= targetHash
	})
	for i := index; i < int(count); i++ {
		entryBase := base + i*modelLen
		hash := r.read64(entryBase)
		if hash != targetHash {
			break
		}
		if r.stringEqual(r.read32(entryBase+8), modelID) {
			return uint32(i), true
		}
	}
	return 0, false
}

func (r Reader) model(id uint32) modelRef {
	base := int(r.modelsOff) + 4 + int(id)*modelLen
	return modelRef{
		idSID:              r.read32(base + 8),
		providerID:         r.read32(base + 12),
		nameSID:            r.read32(base + 16),
		modeSID:            r.read32(base + 20),
		capabilitiesSID:    r.read32(base + 24),
		modalitiesSID:      r.read32(base + 28),
		additionalPriceSID: r.read32(base + 32),
		limitsSID:          r.read32(base + 36),
		metadataSID:        r.read32(base + 40),
	}
}

func (r Reader) provider(id uint32) providerRef {
	base := int(r.providersOff) + 4 + int(id)*providerLen
	return providerRef{
		idSID:         r.read32(base),
		kindSID:       r.read32(base + 4),
		endpointSID:   r.read32(base + 8),
		secretSID:     r.read32(base + 12),
		authTypeSID:   r.read32(base + 16),
		pathPrefixSID: r.read32(base + 20),
		extraSID:      r.read32(base + 24),
	}
}

func (r Reader) route(id uint32) routeRef {
	base := int(r.routesOff) + 4 + int(id)*routeLen
	kind := r.read32(base)
	switch kind {
	case routeKindTargetID:
		return routeRef{
			kind:       kind,
			providerID: r.read32(base + 4),
			modelID:    r.read32(base + 8),
			secretSID:  r.read32(base + 12),
		}
	case routeKindChainID:
		return routeRef{
			kind:            kind,
			childCount:      r.read32(base + 4),
			childOffset:     r.read32(base + 8),
			retryOnSID:      r.read32(base + 12),
			perTryTimeoutMS: r.read32(base + 16),
		}
	case routeKindSplitID:
		return routeRef{
			kind:        kind,
			childCount:  r.read32(base + 4),
			childOffset: r.read32(base + 8),
		}
	default:
		return routeRef{kind: kind}
	}
}

func (r Reader) routePlanIDs(routeID uint32, principal principalRef, targetOrdinal *uint32) (LLMRoutePlanIDs, bool) {
	route := r.route(routeID)
	switch route.kind {
	case routeKindTargetID:
		provider := r.provider(route.providerID)
		model := r.model(route.modelID)
		secretSID := route.secretSID
		if credentialSID, ok := r.credentialSecretSID(principal, *targetOrdinal); ok {
			secretSID = credentialSID
		}
		(*targetOrdinal)++
		if r.stringEqual(secretSID, "") {
			secretSID = provider.secretSID
		}
		return LLMRoutePlanIDs{
			RouteID:      routeID,
			Kind:         RouteKindTarget,
			ProviderID:   route.providerID,
			ProviderSID:  provider.idSID,
			KindSID:      provider.kindSID,
			EndpointSID:  provider.endpointSID,
			ModelID:      route.modelID,
			ModelSID:     model.idSID,
			ModelNameSID: model.nameSID,
			SecretSID:    secretSID,
		}, true
	case routeKindChainID:
		children, ok := r.routeChildren(route, principal, targetOrdinal)
		if !ok {
			return LLMRoutePlanIDs{}, false
		}
		return LLMRoutePlanIDs{
			RouteID:         routeID,
			Kind:            RouteKindChain,
			RetryOnSID:      route.retryOnSID,
			PerTryTimeoutMS: route.perTryTimeoutMS,
			Children:        children,
		}, true
	case routeKindSplitID:
		children, ok := r.routeChildren(route, principal, targetOrdinal)
		if !ok {
			return LLMRoutePlanIDs{}, false
		}
		return LLMRoutePlanIDs{
			RouteID:  routeID,
			Kind:     RouteKindSplit,
			Children: children,
		}, true
	default:
		return LLMRoutePlanIDs{}, false
	}
}

func (r Reader) routeChildren(route routeRef, principal principalRef, targetOrdinal *uint32) ([]LLMRouteChildIDs, bool) {
	children := make([]LLMRouteChildIDs, 0, route.childCount)
	for i := uint32(0); i < route.childCount; i++ {
		entryBase := int(route.childOffset) + int(i)*routeChildLen
		weight := r.read32(entryBase)
		childRouteID := r.read32(entryBase + 4)
		child, ok := r.routePlanIDs(childRouteID, principal, targetOrdinal)
		if !ok {
			return nil, false
		}
		children = append(children, LLMRouteChildIDs{
			Weight: weight,
			Plan:   child,
		})
	}
	return children, true
}

func (r Reader) credentialSecretSID(principal principalRef, targetOrdinal uint32) (uint32, bool) {
	for i := uint32(0); i < principal.credentialCount; i++ {
		entryBase := int(principal.credentialOffset) + int(i)*credentialLen
		if r.read32(entryBase) == targetOrdinal {
			return r.read32(entryBase + 4), true
		}
	}
	return 0, false
}

func firstTargetPlanIDs(plan LLMRoutePlanIDs) (LLMRoutePlanIDs, bool) {
	switch plan.Kind {
	case RouteKindTarget:
		return plan, true
	case RouteKindChain:
		if len(plan.Children) == 0 {
			return LLMRoutePlanIDs{}, false
		}
		return firstTargetPlanIDs(plan.Children[0].Plan)
	case RouteKindSplit:
		if len(plan.Children) == 0 {
			return LLMRoutePlanIDs{}, false
		}
		best := plan.Children[0]
		for _, child := range plan.Children[1:] {
			if child.Weight > best.Weight {
				best = child
			}
		}
		return firstTargetPlanIDs(best.Plan)
	default:
		return LLMRoutePlanIDs{}, false
	}
}

func (r Reader) materializeLLMRoutePlan(plan LLMRoutePlanIDs) LLMRoutePlan {
	out := LLMRoutePlan{
		Kind:            plan.Kind,
		RetryOn:         r.String(plan.RetryOnSID),
		PerTryTimeoutMS: plan.PerTryTimeoutMS,
	}
	if plan.Kind == RouteKindTarget {
		out.Provider = r.String(plan.ProviderSID)
		out.ProviderKind = r.String(plan.KindSID)
		out.Endpoint = r.String(plan.EndpointSID)
		out.Model = r.String(plan.ModelSID)
		out.ModelName = r.String(plan.ModelNameSID)
		out.SecretRef = r.String(plan.SecretSID)
		return out
	}
	for _, child := range plan.Children {
		out.Children = append(out.Children, LLMRouteChild{
			Weight: child.Weight,
			Plan:   r.materializeLLMRoutePlan(child.Plan),
		})
	}
	return out
}

func (r Reader) rateIDs(id uint32) RatePolicyIDs {
	base := int(r.ratesOff) + 4 + int(id)*rateLen
	return RatePolicyIDs{
		USDPerDayCents: r.read64(base),
		RPM:            r.read32(base + 8),
		OnExceedSID:    r.read32(base + 12),
	}
}

func (r Reader) toolset(id uint32) []MCPToolIDs {
	count, offset := r.toolsetRecord(id)
	tools := make([]MCPToolIDs, 0, count)
	for i := uint32(0); i < count; i++ {
		entryBase := int(offset) + int(i)*mcpToolBindingLen
		serverID := r.read32(entryBase + 4)
		server := r.mcpServer(serverID)
		tools = append(tools, MCPToolIDs{
			ExposedNameSID:    r.read32(entryBase),
			ServerID:          serverID,
			ServerSID:         server.idSID,
			ServerEndpointSID: server.endpointSID,
			ToolSID:           r.read32(entryBase + 8),
			SecretSID:         r.read32(entryBase + 12),
			AuthTypeSID:       r.read32(entryBase + 16),
		})
	}
	return tools
}

func (r Reader) toolsetRecord(id uint32) (uint32, uint32) {
	base := int(r.mcpToolsetsOff) + 4 + int(id)*mcpToolsetLen
	return r.read32(base), r.read32(base + 4)
}

func (r Reader) mcpServer(id uint32) struct {
	idSID       uint32
	endpointSID uint32
	secretSID   uint32
	authTypeSID uint32
} {
	base := int(r.mcpServersOff) + 4 + int(id)*mcpServerLen
	return struct {
		idSID       uint32
		endpointSID uint32
		secretSID   uint32
		authTypeSID uint32
	}{
		idSID:       r.read32(base),
		endpointSID: r.read32(base + 4),
		secretSID:   r.read32(base + 8),
		authTypeSID: r.read32(base + 12),
	}
}

func (r Reader) string(id uint32) string {
	start, end, ok := r.stringBounds(id)
	if !ok {
		return ""
	}
	return string(r.blob[start:end])
}

func (r Reader) stringEqual(id uint32, value string) bool {
	start, end, ok := r.stringBounds(id)
	if !ok {
		return false
	}
	if end-start != len(value) {
		return false
	}
	for i := 0; i < len(value); i++ {
		if r.blob[start+i] != value[i] {
			return false
		}
	}
	return true
}

func (r Reader) stringBounds(id uint32) (int, int, bool) {
	base := int(r.stringsOff)
	count := r.read32(base)
	dataLen := r.read32(base + 4)
	if id >= count {
		return 0, 0, false
	}
	offsetBase := base + 8
	start := r.read32(offsetBase + int(id)*4)
	end := r.read32(offsetBase + int(id+1)*4)
	dataBase := offsetBase + int(count+1)*4
	if end > dataLen || start > end {
		return 0, 0, false
	}
	return dataBase + int(start), dataBase + int(end), true
}

func (r Reader) sectionCount(offset uint32) uint32 {
	return r.read32(int(offset))
}

func (r Reader) read32(offset int) uint32 {
	if offset < 0 || offset+4 > len(r.blob) {
		return 0
	}
	return binary.LittleEndian.Uint32(r.blob[offset : offset+4])
}

func (r Reader) read64(offset int) uint64 {
	if offset < 0 || offset+8 > len(r.blob) {
		return 0
	}
	return binary.LittleEndian.Uint64(r.blob[offset : offset+8])
}

func (r Reader) validateOffsets() error {
	// These offsets point to top-level sections. Every top-level section starts
	// with a uint32 count, even when the section has zero records, so the offset
	// itself may not be EOF.
	tableOffsets := []uint32{
		r.stringsOff,
		r.providersOff,
		r.modelsOff,
		r.routesOff,
		r.ratesOff,
		r.mcpServersOff,
		r.mcpToolsetsOff,
		r.scopesOff,
	}
	for _, offset := range tableOffsets {
		if int(offset) < headerSize || int(offset)+4 > len(r.blob) {
			return fmt.Errorf("invalid pack offset %d", offset)
		}
	}

	// These offsets point into scope-local index data. Unlike the top-level
	// sections above, the indexes have no standalone count word; each scope
	// record stores its own count. If all scopes have zero MCP profiles, the MCP
	// path index is empty and its offset is exactly len(blob), which is valid.
	indexOffsets := []uint32{
		r.principalsOff,
		r.mcpPathsOff,
	}
	for _, offset := range indexOffsets {
		if int(offset) < headerSize || int(offset) > len(r.blob) {
			return fmt.Errorf("invalid pack offset %d", offset)
		}
	}
	if err := r.validateCredentialOffsets(); err != nil {
		return err
	}
	return nil
}

func (r Reader) validateCredentialOffsets() error {
	count := r.sectionCount(r.scopesOff)
	base := int(r.scopesOff) + 4
	credentialsStart := r.principalsOff
	for scopeIndex := uint32(0); scopeIndex < count; scopeIndex++ {
		principalCount := r.read32(base + int(scopeIndex)*scopeLen + 4)
		principalOffset := r.read32(base + int(scopeIndex)*scopeLen + 8)
		principalEnd := principalOffset + principalCount*principalLen
		if principalEnd > credentialsStart {
			credentialsStart = principalEnd
		}
	}
	for scopeIndex := uint32(0); scopeIndex < count; scopeIndex++ {
		scope := scopeRef{
			principalCount:  r.read32(base + int(scopeIndex)*scopeLen + 4),
			principalOffset: r.read32(base + int(scopeIndex)*scopeLen + 8),
		}
		for principalIndex := uint32(0); principalIndex < scope.principalCount; principalIndex++ {
			entryBase := int(scope.principalOffset) + int(principalIndex)*principalLen
			credentialCount := r.read32(entryBase + 24)
			credentialOffset := r.read32(entryBase + 28)
			if credentialCount == 0 {
				continue
			}
			credentialEnd := int(credentialOffset) + int(credentialCount)*credentialLen
			if credentialOffset < credentialsStart || credentialOffset > r.mcpPathsOff || credentialEnd > int(r.mcpPathsOff) {
				return fmt.Errorf("invalid credential offset %d", credentialOffset)
			}
		}
	}
	return nil
}

func hashString(value string) uint64 {
	var hash uint64 = 14695981039346656037
	for i := 0; i < len(value); i++ {
		hash ^= uint64(value[i])
		hash *= 1099511628211
	}
	return hash
}

func principalLookupHash(slug string, requestedModel string) uint64 {
	var hash uint64 = 14695981039346656037
	for i := 0; i < len(slug); i++ {
		hash ^= uint64(slug[i])
		hash *= 1099511628211
	}
	hash ^= 0
	hash *= 1099511628211
	for i := 0; i < len(requestedModel); i++ {
		hash ^= uint64(requestedModel[i])
		hash *= 1099511628211
	}
	return hash
}

func checksum(data []byte) uint64 {
	var hash uint64 = 14695981039346656037
	for _, value := range data {
		hash ^= uint64(value)
		hash *= 1099511628211
	}
	return hash
}

func putU32(out *bytes.Buffer, value uint32) {
	var buf [4]byte
	put32(buf[:], value)
	out.Write(buf[:])
}

func putU64(out *bytes.Buffer, value uint64) {
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], value)
	out.Write(buf[:])
}

func put32(dst []byte, value uint32) {
	binary.LittleEndian.PutUint32(dst, value)
}

func u32(src []byte) uint32 {
	return binary.LittleEndian.Uint32(src)
}
