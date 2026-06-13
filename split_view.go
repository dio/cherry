package cherry

import (
	"fmt"
	"slices"
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
