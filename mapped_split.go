package cherry

import (
	"fmt"
	"hash/fnv"
)

// MappedSplitLane identifies a stable mapped-split delivery lane.
type MappedSplitLane string

const (
	// MappedSplitLaneLLMGeneric is the low-churn lane for default LLM routing,
	// providers, models, and platform secret refs.
	MappedSplitLaneLLMGeneric MappedSplitLane = "llm-generic"
	// MappedSplitLaneMCPServers is the low-churn lane for direct MCP server
	// paths such as /mcp/s/<server> and platform secret refs.
	MappedSplitLaneMCPServers MappedSplitLane = "mcp-servers"
	// MappedSplitLaneLLMUserKey is the partitioned lane for key-specific LLM
	// routes, BYOK secret refs, and rate policies.
	MappedSplitLaneLLMUserKey MappedSplitLane = "llm-user-key"
	// MappedSplitLaneMCPUserProfile is the partitioned lane for user/profile MCP
	// paths and selected tools.
	MappedSplitLaneMCPUserProfile MappedSplitLane = "mcp-user-profile"
)

// MappedSplitCatalogPartition marks a non-partitioned catalog/default mapped
// split bundle key.
const MappedSplitCatalogPartition = -1

// MappedSplitBundleKey identifies one bundle component in the mapped-split
// layout.
type MappedSplitBundleKey struct {
	Lane      MappedSplitLane
	Partition int
}

// Component returns the component name used in split-map bundle URLs.
func (k MappedSplitBundleKey) Component() string {
	if !k.IsPartitioned() {
		return string(k.Lane)
	}
	return fmt.Sprintf("%s-%03d", k.Lane, k.Partition)
}

// IsPartitioned reports whether the bundle key points at a partitioned lane.
func (k MappedSplitBundleKey) IsPartitioned() bool {
	return k.Partition != MappedSplitCatalogPartition
}

// MappedSplitSpec holds the partition counts for the mapped-split layout.
type MappedSplitSpec struct {
	LLMUserKeyPartitions     int
	MCPUserProfilePartitions int
}

// Validate verifies that configured partitioned lanes can be addressed.
func (s MappedSplitSpec) Validate() error {
	if s.LLMUserKeyPartitions <= 0 {
		return fmt.Errorf("llm user-key partitions must be positive")
	}
	if s.MCPUserProfilePartitions <= 0 {
		return fmt.Errorf("mcp user-profile partitions must be positive")
	}
	return nil
}

// CatalogBundle returns a bundle key for a low-churn catalog/default lane.
func (s MappedSplitSpec) CatalogBundle(lane MappedSplitLane) (MappedSplitBundleKey, error) {
	switch lane {
	case MappedSplitLaneLLMGeneric, MappedSplitLaneMCPServers:
		return MappedSplitBundleKey{Lane: lane, Partition: MappedSplitCatalogPartition}, nil
	case MappedSplitLaneLLMUserKey, MappedSplitLaneMCPUserProfile:
		return MappedSplitBundleKey{}, fmt.Errorf("lane %q is partitioned", lane)
	default:
		return MappedSplitBundleKey{}, fmt.Errorf("unknown mapped split lane %q", lane)
	}
}

// LLMUserKeyPartition returns the deterministic partition for a principal slug.
func (s MappedSplitSpec) LLMUserKeyPartition(principalSlug string) (int, error) {
	if s.LLMUserKeyPartitions <= 0 {
		return 0, fmt.Errorf("llm user-key partitions must be positive")
	}
	if principalSlug == "" {
		return 0, fmt.Errorf("principal slug is required")
	}
	return mappedSplitPartition(principalSlug, s.LLMUserKeyPartitions), nil
}

// MCPUserProfilePartition returns the deterministic partition for an MCP path
// suffix.
func (s MappedSplitSpec) MCPUserProfilePartition(pathSuffix string) (int, error) {
	if s.MCPUserProfilePartitions <= 0 {
		return 0, fmt.Errorf("mcp user-profile partitions must be positive")
	}
	if pathSuffix == "" {
		return 0, fmt.Errorf("mcp path suffix is required")
	}
	return mappedSplitPartition(pathSuffix, s.MCPUserProfilePartitions), nil
}

// LLMUserKeyBundle returns the partitioned bundle key for a principal slug.
func (s MappedSplitSpec) LLMUserKeyBundle(principalSlug string) (MappedSplitBundleKey, error) {
	partition, err := s.LLMUserKeyPartition(principalSlug)
	if err != nil {
		return MappedSplitBundleKey{}, err
	}
	return MappedSplitBundleKey{Lane: MappedSplitLaneLLMUserKey, Partition: partition}, nil
}

// MCPUserProfileBundle returns the partitioned bundle key for an MCP path
// suffix.
func (s MappedSplitSpec) MCPUserProfileBundle(pathSuffix string) (MappedSplitBundleKey, error) {
	partition, err := s.MCPUserProfilePartition(pathSuffix)
	if err != nil {
		return MappedSplitBundleKey{}, err
	}
	return MappedSplitBundleKey{Lane: MappedSplitLaneMCPUserProfile, Partition: partition}, nil
}

// MappedSplitChangeKind identifies the explicitly classified change lane for
// affected-bundle planning.
type MappedSplitChangeKind string

const (
	// MappedSplitChangeLLMGeneric means default LLM routing, providers, models,
	// or platform LLM secret refs changed.
	MappedSplitChangeLLMGeneric MappedSplitChangeKind = "llm-generic"
	// MappedSplitChangeMCPServers means direct MCP server catalog/path policy or
	// platform MCP secret refs changed.
	MappedSplitChangeMCPServers MappedSplitChangeKind = "mcp-servers"
	// MappedSplitChangeLLMUserKey means a principal/key-specific LLM route,
	// secret ref, or rate policy changed.
	MappedSplitChangeLLMUserKey MappedSplitChangeKind = "llm-user-key"
	// MappedSplitChangeMCPUserProfile means a user/profile MCP path or selected
	// tool binding changed.
	MappedSplitChangeMCPUserProfile MappedSplitChangeKind = "mcp-user-profile"
)

// MappedSplitChange describes an already-classified producer change. Cherry
// uses it only to compute the affected bundle; source-record inference remains
// outside the root package.
type MappedSplitChange struct {
	Kind          MappedSplitChangeKind
	PrincipalSlug string
	PathSuffix    string
}

// AffectedBundle returns the mapped split bundle touched by an explicitly
// classified producer change.
func (s MappedSplitSpec) AffectedBundle(change MappedSplitChange) (MappedSplitBundleKey, error) {
	switch change.Kind {
	case MappedSplitChangeLLMGeneric:
		return s.CatalogBundle(MappedSplitLaneLLMGeneric)
	case MappedSplitChangeMCPServers:
		return s.CatalogBundle(MappedSplitLaneMCPServers)
	case MappedSplitChangeLLMUserKey:
		return s.LLMUserKeyBundle(change.PrincipalSlug)
	case MappedSplitChangeMCPUserProfile:
		return s.MCPUserProfileBundle(change.PathSuffix)
	default:
		return MappedSplitBundleKey{}, fmt.Errorf("unknown mapped split change kind %q", change.Kind)
	}
}

func mappedSplitPartition(value string, partitions int) int {
	h := fnv.New64a()
	_, _ = h.Write([]byte(value))
	return int(h.Sum64() % uint64(partitions))
}
