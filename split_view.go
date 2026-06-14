package cherry

import (
	"fmt"
	"slices"
	"strings"
)

// OpenedSplitBundle is the result of opening paired LLM and MCP bundle
// artifacts. It retains each opened component bundle and exposes the composed
// immutable SplitView for enforcement queries.
type OpenedSplitBundle struct {
	LLM  OpenedBundle
	MCP  OpenedBundle
	View SplitView
}

// SplitBundleOptions are optional compatibility requirements checked while
// opening paired LLM/MCP bundles. Zero values keep the default behavior: validate
// each bundle's own manifest plus the shared scope selection.
type SplitBundleOptions struct {
	GenerationID         string
	LLMPackChecksum      uint64
	MCPPackChecksum      uint64
	RequiredLLMProviders []string
	RequiredLLMModels    []string
	RequiredMCPServers   []string
}

// SplitView composes independently built LLM and MCP readers into one immutable
// enforcement-point view. It does not change the pack format; callers can swap a
// new SplitView atomically while in-flight requests continue using the previous
// readers.
//
// String IDs returned by LLM methods belong to the LLM reader, and string IDs
// returned by MCP methods belong to the MCP reader. Use LLMString and MCPString
// to materialize IDs from the correct string table.
type SplitView struct {
	llm Reader
	mcp Reader
}

// NewSplitView returns an immutable view over independently opened LLM and MCP
// readers. The readers are copied by value and still reference their original
// immutable blobs.
func NewSplitView(llm Reader, mcp Reader) SplitView {
	return SplitView{
		llm: llm,
		mcp: mcp,
	}
}

// OpenSplitBundleZstd opens paired LLM and MCP bundle artifacts using the
// existing single-bundle envelope and validates that they describe the same
// control-plane selection and concrete scope set.
func OpenSplitBundleZstd(llmCompressed []byte, mcpCompressed []byte) (OpenedSplitBundle, error) {
	return OpenSplitBundleZstdWithOptions(llmCompressed, mcpCompressed, SplitBundleOptions{})
}

// OpenSplitBundleZstdWithOptions opens paired LLM and MCP bundle artifacts and
// validates optional generation, checksum, and catalog expectations.
func OpenSplitBundleZstdWithOptions(
	llmCompressed []byte,
	mcpCompressed []byte,
	options SplitBundleOptions,
) (OpenedSplitBundle, error) {
	llm, err := OpenBundleZstd(llmCompressed)
	if err != nil {
		return OpenedSplitBundle{}, fmt.Errorf("open llm bundle: %w", err)
	}
	mcp, err := OpenBundleZstd(mcpCompressed)
	if err != nil {
		return OpenedSplitBundle{}, fmt.Errorf("open mcp bundle: %w", err)
	}
	if err := ValidateSplitBundleCompatibility(llm.Metadata, mcp.Metadata); err != nil {
		return OpenedSplitBundle{}, err
	}
	if err := validateSplitBundleOptions(llm.Metadata, mcp.Metadata, options); err != nil {
		return OpenedSplitBundle{}, err
	}
	opened := OpenedSplitBundle{
		LLM:  llm,
		MCP:  mcp,
		View: NewSplitView(llm.Reader, mcp.Reader),
	}
	if err := validateSplitCatalogs(opened.View, options); err != nil {
		return OpenedSplitBundle{}, err
	}
	return opened, nil
}

// ValidateSplitBundleCompatibility verifies that paired LLM and MCP bundles can
// be composed as one SplitView generation. It intentionally does not require
// matching pack manifests: independent policy clusters are expected to have
// different blobs, checksums, and sizes.
func ValidateSplitBundleCompatibility(llm BundleMetadata, mcp BundleMetadata) error {
	if llm.ScopeKind != mcp.ScopeKind {
		return fmt.Errorf("split bundle scope kind mismatch: llm=%q mcp=%q", llm.ScopeKind, mcp.ScopeKind)
	}
	if llm.ScopeID != mcp.ScopeID {
		return fmt.Errorf("split bundle scope id mismatch: llm=%q mcp=%q", llm.ScopeID, mcp.ScopeID)
	}
	llmScopes := append([]string(nil), llm.Scopes...)
	mcpScopes := append([]string(nil), mcp.Scopes...)
	slices.Sort(llmScopes)
	slices.Sort(mcpScopes)
	if !slices.Equal(llmScopes, mcpScopes) {
		return fmt.Errorf("split bundle scopes mismatch: llm=%v mcp=%v", llmScopes, mcpScopes)
	}
	if llm.GenerationID != "" && mcp.GenerationID != "" && llm.GenerationID != mcp.GenerationID {
		return fmt.Errorf("split bundle generation mismatch: llm=%q mcp=%q", llm.GenerationID, mcp.GenerationID)
	}
	return nil
}

func validateSplitBundleOptions(llm BundleMetadata, mcp BundleMetadata, options SplitBundleOptions) error {
	if options.GenerationID != "" {
		if llm.GenerationID != options.GenerationID {
			return fmt.Errorf("split bundle llm generation mismatch: got %q want %q", llm.GenerationID, options.GenerationID)
		}
		if mcp.GenerationID != options.GenerationID {
			return fmt.Errorf("split bundle mcp generation mismatch: got %q want %q", mcp.GenerationID, options.GenerationID)
		}
	}
	if options.LLMPackChecksum != 0 && llm.PackManifest.Checksum != options.LLMPackChecksum {
		return fmt.Errorf("split bundle llm checksum mismatch: got %d want %d", llm.PackManifest.Checksum, options.LLMPackChecksum)
	}
	if options.MCPPackChecksum != 0 && mcp.PackManifest.Checksum != options.MCPPackChecksum {
		return fmt.Errorf("split bundle mcp checksum mismatch: got %d want %d", mcp.PackManifest.Checksum, options.MCPPackChecksum)
	}
	return nil
}

func validateSplitCatalogs(view SplitView, options SplitBundleOptions) error {
	for _, providerID := range options.RequiredLLMProviders {
		if _, ok := view.ResolveProvider(providerID); !ok {
			return fmt.Errorf("split bundle missing llm provider %q", providerID)
		}
	}
	for _, modelID := range options.RequiredLLMModels {
		if _, ok := view.ResolveModel(modelID); !ok {
			return fmt.Errorf("split bundle missing llm model %q", modelID)
		}
	}
	for _, serverID := range options.RequiredMCPServers {
		if _, ok := view.ResolveMCPServer(serverID); !ok {
			return fmt.Errorf("split bundle missing mcp server %q", serverID)
		}
	}
	return nil
}

// LLMReader returns the underlying LLM reader for inspector APIs not mirrored by
// SplitView.
func (v SplitView) LLMReader() Reader {
	return v.llm
}

// MCPReader returns the underlying MCP reader for inspector APIs not mirrored by
// SplitView.
func (v SplitView) MCPReader() Reader {
	return v.mcp
}

// LLMString returns an LLM-reader string-table value.
func (v SplitView) LLMString(id uint32) string {
	return v.llm.String(id)
}

// MCPString returns an MCP-reader string-table value.
func (v SplitView) MCPString(id uint32) string {
	return v.mcp.String(id)
}

// ResolveLLMPlanIDs resolves an LLM request against the LLM reader.
func (v SplitView) ResolveLLMPlanIDs(scopeID string, principalSlug string, modelID string) (LLMPlanIDs, bool) {
	return v.llm.ResolveLLMPlanIDs(scopeID, principalSlug, modelID)
}

// ResolveLLMIDs resolves an LLM request against the LLM reader.
func (v SplitView) ResolveLLMIDs(scopeID string, principalSlug string, modelID string) (LLMIDs, bool) {
	return v.llm.ResolveLLMIDs(scopeID, principalSlug, modelID)
}

// ResolveLLM resolves and materializes an LLM request against the LLM reader.
func (v SplitView) ResolveLLM(scopeID string, principalSlug string, modelID string) (LLMResult, bool) {
	return v.llm.ResolveLLM(scopeID, principalSlug, modelID)
}

// ResolveLLMPlan resolves and materializes an LLM route plan against the LLM
// reader.
func (v SplitView) ResolveLLMPlan(scopeID string, principalSlug string, modelID string) (LLMPlan, bool) {
	return v.llm.ResolveLLMPlan(scopeID, principalSlug, modelID)
}

// ResolveProvider returns provider catalog metadata from the LLM reader.
func (v SplitView) ResolveProvider(providerID string) (ProviderInfo, bool) {
	return v.llm.ResolveProvider(providerID)
}

// Providers returns provider catalog metadata from the LLM reader.
func (v SplitView) Providers() []ProviderInfo {
	return v.llm.Providers()
}

// ResolveModel returns model catalog metadata from the LLM reader.
func (v SplitView) ResolveModel(modelID string) (ModelInfo, bool) {
	return v.llm.ResolveModel(modelID)
}

// Models returns model catalog metadata from the LLM reader.
func (v SplitView) Models() []ModelInfo {
	return v.llm.Models()
}

// ModelCapability reports model capability from the LLM reader.
func (v SplitView) ModelCapability(modelID string, capability string) bool {
	return v.llm.ModelCapability(modelID, capability)
}

// V1ModelsJSON renders model catalog metadata from the LLM reader.
func (v SplitView) V1ModelsJSON() ([]byte, error) {
	return v.llm.V1ModelsJSON()
}

// V1ModelsJSONForProvider renders provider-filtered model catalog metadata from
// the LLM reader.
func (v SplitView) V1ModelsJSONForProvider(providerID string) ([]byte, error) {
	return v.llm.V1ModelsJSONForProvider(providerID)
}

// ResolveMCPIDs resolves an MCP path against the MCP reader.
func (v SplitView) ResolveMCPIDs(scopeID string, pathSuffix string) (MCPResultIDs, bool) {
	return v.mcp.ResolveMCPIDs(scopeID, pathSuffix)
}

// ResolveMCPInitializeIDs resolves MCP initialize servers against the MCP
// reader.
func (v SplitView) ResolveMCPInitializeIDs(scopeID string, pathSuffix string) (MCPInitializeIDs, bool) {
	return v.mcp.ResolveMCPInitializeIDs(scopeID, pathSuffix)
}

// ResolveMCPToolIDs resolves one MCP tool against the MCP reader.
func (v SplitView) ResolveMCPToolIDs(scopeID string, pathSuffix string, exposedTool string) (MCPToolIDs, bool) {
	return v.mcp.ResolveMCPToolIDs(scopeID, pathSuffix, exposedTool)
}

// ResolveMCPInitialize materializes MCP initialize servers from the MCP reader.
func (v SplitView) ResolveMCPInitialize(scopeID string, pathSuffix string) (MCPInitializeResult, bool) {
	return v.mcp.ResolveMCPInitialize(scopeID, pathSuffix)
}

// ResolveMCP materializes an MCP path lookup from the MCP reader.
func (v SplitView) ResolveMCP(scopeID string, pathSuffix string) (MCPResult, bool) {
	return v.mcp.ResolveMCP(scopeID, pathSuffix)
}

// ResolveMCPServer returns MCP server catalog metadata from the MCP reader.
func (v SplitView) ResolveMCPServer(serverID string) (MCPServerInfo, bool) {
	return v.mcp.ResolveMCPServer(serverID)
}

// MCPServers returns MCP server catalog metadata from the MCP reader.
func (v SplitView) MCPServers() []MCPServerInfo {
	return v.mcp.MCPServers()
}

// LayeredLLMSource identifies which LLM reader produced a layered split result.
type LayeredLLMSource string

const (
	// LayeredLLMSourceGeneric identifies the mostly static generic/default LLM
	// reader.
	LayeredLLMSourceGeneric LayeredLLMSource = "llm_generic"
	// LayeredLLMSourceKey identifies the per-key LLM override reader.
	LayeredLLMSourceKey LayeredLLMSource = "llm_key"
)

// LayeredMCPSource identifies which MCP reader produced a layered split result.
type LayeredMCPSource string

const (
	// LayeredMCPSourceServers identifies the mostly static direct MCP server
	// reader for paths such as "s/github".
	LayeredMCPSourceServers LayeredMCPSource = "mcp_servers"
	// LayeredMCPSourceProfiles identifies the MCP profile reader for paths such
	// as "profile-dev-tools".
	LayeredMCPSourceProfiles LayeredMCPSource = "mcp_profiles"
)

// LayeredSplitViewOptions configure fallback behavior for a four-layer split
// view.
type LayeredSplitViewOptions struct {
	// LLMDefaultPrincipalSlug is used in the generic LLM reader when the per-key
	// reader has no route for the verified principal. When empty, the generic
	// reader is queried with the verified principal slug.
	LLMDefaultPrincipalSlug string
}

// OpenedLayeredSplitBundle is the result of opening four independently rebuilt
// bundle artifacts for generic LLM policy, per-key LLM overrides, direct MCP
// server paths, and MCP profile paths.
type OpenedLayeredSplitBundle struct {
	LLMGeneric  OpenedBundle
	LLMKeys     OpenedBundle
	MCPServers  OpenedBundle
	MCPProfiles OpenedBundle
	View        LayeredSplitView
}

// LayeredSplitBundleOptions are optional compatibility requirements checked
// while opening a four-layer split.
type LayeredSplitBundleOptions struct {
	GenerationID            string
	LLMGenericPackChecksum  uint64
	LLMKeysPackChecksum     uint64
	MCPServersPackChecksum  uint64
	MCPProfilesPackChecksum uint64
	LLMDefaultPrincipalSlug string
	RequiredLLMProviders    []string
	RequiredLLMModels       []string
	RequiredMCPServers      []string
}

// LayeredLLMPlanIDs is an LLM plan result plus the reader layer that owns its
// string IDs.
type LayeredLLMPlanIDs struct {
	LLMPlanIDs
	Source                LayeredLLMSource
	ResolvedPrincipalSlug string
}

// LayeredLLMIDs is an executable LLM target result plus the reader layer that
// owns its string IDs.
type LayeredLLMIDs struct {
	LLMIDs
	Source                LayeredLLMSource
	ResolvedPrincipalSlug string
}

// LayeredMCPResultIDs is an MCP path result plus the reader layer that owns its
// string IDs.
type LayeredMCPResultIDs struct {
	MCPResultIDs
	Source LayeredMCPSource
}

// LayeredMCPInitializeIDs is an MCP initialize result plus the reader layer that
// owns its string IDs.
type LayeredMCPInitializeIDs struct {
	MCPInitializeIDs
	Source LayeredMCPSource
}

// LayeredMCPToolIDs is an MCP tool result plus the reader layer that owns its
// string IDs.
type LayeredMCPToolIDs struct {
	MCPToolIDs
	Source LayeredMCPSource
}

// LayeredSplitView composes four independently rebuilt readers:
// generic/default LLM policy, per-key LLM overrides, direct MCP server paths,
// and MCP profiles. It does not change the pack format; each layer remains a
// normal immutable Reader.
type LayeredSplitView struct {
	llmGeneric              Reader
	llmKeys                 Reader
	mcpServers              Reader
	mcpProfiles             Reader
	llmDefaultPrincipalSlug string
}

// NewLayeredSplitView returns an immutable four-layer view. The readers are
// copied by value and still reference their original immutable blobs.
func NewLayeredSplitView(
	llmGeneric Reader,
	llmKeys Reader,
	mcpServers Reader,
	mcpProfiles Reader,
	options LayeredSplitViewOptions,
) LayeredSplitView {
	return LayeredSplitView{
		llmGeneric:              llmGeneric,
		llmKeys:                 llmKeys,
		mcpServers:              mcpServers,
		mcpProfiles:             mcpProfiles,
		llmDefaultPrincipalSlug: options.LLMDefaultPrincipalSlug,
	}
}

// OpenLayeredSplitBundleZstd opens four split bundle artifacts using default
// compatibility checks.
func OpenLayeredSplitBundleZstd(
	llmGenericCompressed []byte,
	llmKeysCompressed []byte,
	mcpServersCompressed []byte,
	mcpProfilesCompressed []byte,
) (OpenedLayeredSplitBundle, error) {
	return OpenLayeredSplitBundleZstdWithOptions(
		llmGenericCompressed,
		llmKeysCompressed,
		mcpServersCompressed,
		mcpProfilesCompressed,
		LayeredSplitBundleOptions{},
	)
}

// OpenLayeredSplitBundleZstdWithOptions opens four split bundle artifacts and
// validates optional generation, checksum, and catalog expectations.
func OpenLayeredSplitBundleZstdWithOptions(
	llmGenericCompressed []byte,
	llmKeysCompressed []byte,
	mcpServersCompressed []byte,
	mcpProfilesCompressed []byte,
	options LayeredSplitBundleOptions,
) (OpenedLayeredSplitBundle, error) {
	llmGeneric, err := OpenBundleZstd(llmGenericCompressed)
	if err != nil {
		return OpenedLayeredSplitBundle{}, fmt.Errorf("open llm generic bundle: %w", err)
	}
	llmKeys, err := OpenBundleZstd(llmKeysCompressed)
	if err != nil {
		return OpenedLayeredSplitBundle{}, fmt.Errorf("open llm keys bundle: %w", err)
	}
	mcpServers, err := OpenBundleZstd(mcpServersCompressed)
	if err != nil {
		return OpenedLayeredSplitBundle{}, fmt.Errorf("open mcp servers bundle: %w", err)
	}
	mcpProfiles, err := OpenBundleZstd(mcpProfilesCompressed)
	if err != nil {
		return OpenedLayeredSplitBundle{}, fmt.Errorf("open mcp profiles bundle: %w", err)
	}

	metadata := []namedBundleMetadata{
		{name: "llm_generic", metadata: llmGeneric.Metadata},
		{name: "llm_keys", metadata: llmKeys.Metadata},
		{name: "mcp_servers", metadata: mcpServers.Metadata},
		{name: "mcp_profiles", metadata: mcpProfiles.Metadata},
	}
	if err := validateLayeredSplitCompatibility(metadata); err != nil {
		return OpenedLayeredSplitBundle{}, err
	}
	if err := validateLayeredSplitBundleOptions(metadata, options); err != nil {
		return OpenedLayeredSplitBundle{}, err
	}

	opened := OpenedLayeredSplitBundle{
		LLMGeneric:  llmGeneric,
		LLMKeys:     llmKeys,
		MCPServers:  mcpServers,
		MCPProfiles: mcpProfiles,
		View: NewLayeredSplitView(
			llmGeneric.Reader,
			llmKeys.Reader,
			mcpServers.Reader,
			mcpProfiles.Reader,
			LayeredSplitViewOptions{LLMDefaultPrincipalSlug: options.LLMDefaultPrincipalSlug},
		),
	}
	if err := validateLayeredSplitCatalogs(opened.View, options); err != nil {
		return OpenedLayeredSplitBundle{}, err
	}
	return opened, nil
}

// LLMGenericReader returns the generic/default LLM reader.
func (v LayeredSplitView) LLMGenericReader() Reader {
	return v.llmGeneric
}

// LLMKeysReader returns the per-key LLM override reader.
func (v LayeredSplitView) LLMKeysReader() Reader {
	return v.llmKeys
}

// MCPServersReader returns the direct MCP server path reader.
func (v LayeredSplitView) MCPServersReader() Reader {
	return v.mcpServers
}

// MCPProfilesReader returns the MCP profile reader.
func (v LayeredSplitView) MCPProfilesReader() Reader {
	return v.mcpProfiles
}

// LLMString returns an LLM string-table value from the reader that produced a
// layered LLM result.
func (v LayeredSplitView) LLMString(source LayeredLLMSource, id uint32) string {
	return v.llmReader(source).String(id)
}

// MCPString returns an MCP string-table value from the reader that produced a
// layered MCP result.
func (v LayeredSplitView) MCPString(source LayeredMCPSource, id uint32) string {
	return v.mcpReader(source).String(id)
}

// ResolveLLMPlanIDs resolves a per-key LLM override first, then falls back to
// the generic LLM reader.
func (v LayeredSplitView) ResolveLLMPlanIDs(
	scopeID string,
	principalSlug string,
	modelID string,
) (LayeredLLMPlanIDs, bool) {
	if ids, ok := v.llmKeys.ResolveLLMPlanIDs(scopeID, principalSlug, modelID); ok {
		return LayeredLLMPlanIDs{
			LLMPlanIDs:            ids,
			Source:                LayeredLLMSourceKey,
			ResolvedPrincipalSlug: principalSlug,
		}, true
	}

	genericSlug := principalSlug
	if v.llmDefaultPrincipalSlug != "" {
		genericSlug = v.llmDefaultPrincipalSlug
	}
	ids, ok := v.llmGeneric.ResolveLLMPlanIDs(scopeID, genericSlug, modelID)
	if !ok {
		return LayeredLLMPlanIDs{}, false
	}
	return LayeredLLMPlanIDs{
		LLMPlanIDs:            ids,
		Source:                LayeredLLMSourceGeneric,
		ResolvedPrincipalSlug: genericSlug,
	}, true
}

// ResolveLLMIDs resolves a per-key LLM override first, then falls back to the
// first executable target in the generic LLM route.
func (v LayeredSplitView) ResolveLLMIDs(scopeID string, principalSlug string, modelID string) (LayeredLLMIDs, bool) {
	plan, ok := v.ResolveLLMPlanIDs(scopeID, principalSlug, modelID)
	if !ok {
		return LayeredLLMIDs{}, false
	}
	target, ok := firstTargetPlanIDs(plan.Plan)
	if !ok {
		return LayeredLLMIDs{}, false
	}
	return LayeredLLMIDs{
		LLMIDs: LLMIDs{
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
		},
		Source:                plan.Source,
		ResolvedPrincipalSlug: plan.ResolvedPrincipalSlug,
	}, true
}

// ResolveLLM materializes an LLM target from the reader layer that resolved it.
func (v LayeredSplitView) ResolveLLM(scopeID string, principalSlug string, modelID string) (LLMResult, bool) {
	ids, ok := v.ResolveLLMIDs(scopeID, principalSlug, modelID)
	if !ok {
		return LLMResult{}, false
	}
	return LLMResult{
		PrincipalSlug: principalSlug,
		Provider:      v.LLMString(ids.Source, ids.ProviderSID),
		ProviderKind:  v.LLMString(ids.Source, ids.KindSID),
		Endpoint:      v.LLMString(ids.Source, ids.EndpointSID),
		Model:         v.LLMString(ids.Source, ids.ModelSID),
		ModelName:     v.LLMString(ids.Source, ids.ModelNameSID),
		SecretRef:     v.LLMString(ids.Source, ids.SecretSID),
		Rate: RatePolicy{
			USDPerDayCents: ids.Rate.USDPerDayCents,
			RPM:            ids.Rate.RPM,
			OnExceed:       v.LLMString(ids.Source, ids.Rate.OnExceedSID),
		},
	}, true
}

// ResolveLLMPlan materializes a complete LLM route tree from the reader layer
// that resolved it.
func (v LayeredSplitView) ResolveLLMPlan(scopeID string, principalSlug string, modelID string) (LLMPlan, bool) {
	ids, ok := v.ResolveLLMPlanIDs(scopeID, principalSlug, modelID)
	if !ok {
		return LLMPlan{}, false
	}
	reader := v.llmReader(ids.Source)
	return LLMPlan{
		PrincipalSlug:  principalSlug,
		RequestedModel: modelID,
		Plan:           reader.materializeLLMRoutePlan(ids.Plan),
		Rate: RatePolicy{
			USDPerDayCents: ids.Rate.USDPerDayCents,
			RPM:            ids.Rate.RPM,
			OnExceed:       reader.String(ids.Rate.OnExceedSID),
		},
	}, true
}

// ResolveProvider returns provider catalog metadata from the generic LLM reader.
func (v LayeredSplitView) ResolveProvider(providerID string) (ProviderInfo, bool) {
	return v.llmGeneric.ResolveProvider(providerID)
}

// Providers returns provider catalog metadata from the generic LLM reader.
func (v LayeredSplitView) Providers() []ProviderInfo {
	return v.llmGeneric.Providers()
}

// ResolveModel returns model catalog metadata from the generic LLM reader.
func (v LayeredSplitView) ResolveModel(modelID string) (ModelInfo, bool) {
	return v.llmGeneric.ResolveModel(modelID)
}

// Models returns model catalog metadata from the generic LLM reader.
func (v LayeredSplitView) Models() []ModelInfo {
	return v.llmGeneric.Models()
}

// ModelCapability reports model capability from the generic LLM reader.
func (v LayeredSplitView) ModelCapability(modelID string, capability string) bool {
	return v.llmGeneric.ModelCapability(modelID, capability)
}

// V1ModelsJSON renders model catalog metadata from the generic LLM reader.
func (v LayeredSplitView) V1ModelsJSON() ([]byte, error) {
	return v.llmGeneric.V1ModelsJSON()
}

// V1ModelsJSONForProvider renders provider-filtered model catalog metadata from
// the generic LLM reader.
func (v LayeredSplitView) V1ModelsJSONForProvider(providerID string) ([]byte, error) {
	return v.llmGeneric.V1ModelsJSONForProvider(providerID)
}

// ResolveMCPIDs resolves direct server paths from the MCP server reader and
// profile paths from the MCP profile reader.
func (v LayeredSplitView) ResolveMCPIDs(scopeID string, pathSuffix string) (LayeredMCPResultIDs, bool) {
	source := mcpSourceForPath(pathSuffix)
	ids, ok := v.mcpReader(source).ResolveMCPIDs(scopeID, pathSuffix)
	if !ok {
		return LayeredMCPResultIDs{}, false
	}
	return LayeredMCPResultIDs{
		MCPResultIDs: ids,
		Source:       source,
	}, true
}

// ResolveMCPInitializeIDs resolves MCP initialize servers from the selected MCP
// reader layer.
func (v LayeredSplitView) ResolveMCPInitializeIDs(scopeID string, pathSuffix string) (LayeredMCPInitializeIDs, bool) {
	source := mcpSourceForPath(pathSuffix)
	ids, ok := v.mcpReader(source).ResolveMCPInitializeIDs(scopeID, pathSuffix)
	if !ok {
		return LayeredMCPInitializeIDs{}, false
	}
	return LayeredMCPInitializeIDs{
		MCPInitializeIDs: ids,
		Source:           source,
	}, true
}

// ResolveMCPToolIDs resolves one MCP tool from the selected MCP reader layer.
func (v LayeredSplitView) ResolveMCPToolIDs(
	scopeID string,
	pathSuffix string,
	exposedTool string,
) (LayeredMCPToolIDs, bool) {
	source := mcpSourceForPath(pathSuffix)
	ids, ok := v.mcpReader(source).ResolveMCPToolIDs(scopeID, pathSuffix, exposedTool)
	if !ok {
		return LayeredMCPToolIDs{}, false
	}
	return LayeredMCPToolIDs{
		MCPToolIDs: ids,
		Source:     source,
	}, true
}

// ResolveMCPInitialize materializes MCP initialize servers from the selected MCP
// reader layer.
func (v LayeredSplitView) ResolveMCPInitialize(
	scopeID string,
	pathSuffix string,
) (MCPInitializeResult, bool) {
	ids, ok := v.ResolveMCPInitializeIDs(scopeID, pathSuffix)
	if !ok {
		return MCPInitializeResult{}, false
	}
	reader := v.mcpReader(ids.Source)
	servers := make([]MCPUpstreamServer, 0, len(ids.Servers))
	for _, server := range ids.Servers {
		servers = append(servers, MCPUpstreamServer{
			Server:    reader.String(server.ServerSID),
			Endpoint:  reader.String(server.EndpointSID),
			SecretRef: reader.String(server.SecretSID),
			AuthType:  reader.String(server.AuthTypeSID),
		})
	}
	return MCPInitializeResult{
		Path:    reader.String(ids.PathSID),
		Auth:    reader.materializeMCPProfileAuth(ids.Auth),
		Servers: servers,
	}, true
}

// ResolveMCP materializes an MCP path lookup from the selected MCP reader layer.
func (v LayeredSplitView) ResolveMCP(scopeID string, pathSuffix string) (MCPResult, bool) {
	ids, ok := v.ResolveMCPIDs(scopeID, pathSuffix)
	if !ok {
		return MCPResult{}, false
	}
	reader := v.mcpReader(ids.Source)
	tools := make([]MCPTool, 0, len(ids.Tools))
	for _, tool := range ids.Tools {
		tools = append(tools, MCPTool{
			ExposedName:    reader.String(tool.ExposedNameSID),
			Server:         reader.String(tool.ServerSID),
			ServerEndpoint: reader.String(tool.ServerEndpointSID),
			Tool:           reader.String(tool.ToolSID),
			SecretRef:      reader.String(tool.SecretSID),
			AuthType:       reader.String(tool.AuthTypeSID),
		})
	}
	return MCPResult{
		Path:  reader.String(ids.PathSID),
		Auth:  reader.materializeMCPProfileAuth(ids.Auth),
		Tools: tools,
	}, true
}

// ResolveMCPServer returns MCP server catalog metadata from the static MCP
// server reader.
func (v LayeredSplitView) ResolveMCPServer(serverID string) (MCPServerInfo, bool) {
	return v.mcpServers.ResolveMCPServer(serverID)
}

// MCPServers returns MCP server catalog metadata from the static MCP server
// reader.
func (v LayeredSplitView) MCPServers() []MCPServerInfo {
	return v.mcpServers.MCPServers()
}

func (v LayeredSplitView) llmReader(source LayeredLLMSource) Reader {
	if source == LayeredLLMSourceKey {
		return v.llmKeys
	}
	return v.llmGeneric
}

func (v LayeredSplitView) mcpReader(source LayeredMCPSource) Reader {
	if source == LayeredMCPSourceProfiles {
		return v.mcpProfiles
	}
	return v.mcpServers
}

func mcpSourceForPath(pathSuffix string) LayeredMCPSource {
	if strings.HasPrefix(pathSuffix, "s/") {
		return LayeredMCPSourceServers
	}
	return LayeredMCPSourceProfiles
}

type namedBundleMetadata struct {
	name     string
	metadata BundleMetadata
}

func validateLayeredSplitCompatibility(bundles []namedBundleMetadata) error {
	if len(bundles) == 0 {
		return nil
	}
	base := bundles[0]
	baseScopes := append([]string(nil), base.metadata.Scopes...)
	slices.Sort(baseScopes)
	generationName := ""
	generationID := ""
	for _, bundle := range bundles[1:] {
		if base.metadata.ScopeKind != bundle.metadata.ScopeKind {
			return fmt.Errorf(
				"layered split bundle scope kind mismatch: %s=%q %s=%q",
				base.name,
				base.metadata.ScopeKind,
				bundle.name,
				bundle.metadata.ScopeKind,
			)
		}
		if base.metadata.ScopeID != bundle.metadata.ScopeID {
			return fmt.Errorf(
				"layered split bundle scope id mismatch: %s=%q %s=%q",
				base.name,
				base.metadata.ScopeID,
				bundle.name,
				bundle.metadata.ScopeID,
			)
		}
		scopes := append([]string(nil), bundle.metadata.Scopes...)
		slices.Sort(scopes)
		if !slices.Equal(baseScopes, scopes) {
			return fmt.Errorf(
				"layered split bundle scopes mismatch: %s=%v %s=%v",
				base.name,
				baseScopes,
				bundle.name,
				scopes,
			)
		}
	}
	for _, bundle := range bundles {
		if bundle.metadata.GenerationID == "" {
			continue
		}
		if generationID == "" {
			generationName = bundle.name
			generationID = bundle.metadata.GenerationID
			continue
		}
		if generationID != bundle.metadata.GenerationID {
			return fmt.Errorf(
				"layered split bundle generation mismatch: %s=%q %s=%q",
				generationName,
				generationID,
				bundle.name,
				bundle.metadata.GenerationID,
			)
		}
	}
	return nil
}

func validateLayeredSplitBundleOptions(bundles []namedBundleMetadata, options LayeredSplitBundleOptions) error {
	if options.GenerationID != "" {
		for _, bundle := range bundles {
			if bundle.metadata.GenerationID != options.GenerationID {
				return fmt.Errorf(
					"layered split bundle %s generation mismatch: got %q want %q",
					bundle.name,
					bundle.metadata.GenerationID,
					options.GenerationID,
				)
			}
		}
	}
	expectedChecksums := map[string]uint64{
		"llm_generic":  options.LLMGenericPackChecksum,
		"llm_keys":     options.LLMKeysPackChecksum,
		"mcp_servers":  options.MCPServersPackChecksum,
		"mcp_profiles": options.MCPProfilesPackChecksum,
	}
	for _, bundle := range bundles {
		expected := expectedChecksums[bundle.name]
		if expected == 0 {
			continue
		}
		if bundle.metadata.PackManifest.Checksum != expected {
			return fmt.Errorf(
				"layered split bundle %s checksum mismatch: got %d want %d",
				bundle.name,
				bundle.metadata.PackManifest.Checksum,
				expected,
			)
		}
	}
	return nil
}

func validateLayeredSplitCatalogs(view LayeredSplitView, options LayeredSplitBundleOptions) error {
	for _, providerID := range options.RequiredLLMProviders {
		if _, ok := view.ResolveProvider(providerID); !ok {
			return fmt.Errorf("layered split bundle missing llm provider %q", providerID)
		}
	}
	for _, modelID := range options.RequiredLLMModels {
		if _, ok := view.ResolveModel(modelID); !ok {
			return fmt.Errorf("layered split bundle missing llm model %q", modelID)
		}
	}
	for _, serverID := range options.RequiredMCPServers {
		if _, ok := view.ResolveMCPServer(serverID); !ok {
			return fmt.Errorf("layered split bundle missing mcp server %q", serverID)
		}
	}
	return nil
}
