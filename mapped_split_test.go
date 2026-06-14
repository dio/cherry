package cherry

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMappedSplitSpecBundleKeys(t *testing.T) {
	spec := MappedSplitSpec{
		LLMUserKeyPartitions:     4,
		MCPUserProfilePartitions: 8,
	}

	llmGeneric, err := spec.CatalogBundle(MappedSplitLaneLLMGeneric)
	require.NoError(t, err)
	assert.Equal(t, "llm-generic", llmGeneric.Component())
	assert.False(t, llmGeneric.IsPartitioned())

	mcpServers, err := spec.CatalogBundle(MappedSplitLaneMCPServers)
	require.NoError(t, err)
	assert.Equal(t, "mcp-servers", mcpServers.Component())
	assert.False(t, mcpServers.IsPartitioned())

	llmKey, err := spec.LLMUserKeyBundle("slug:key-a")
	require.NoError(t, err)
	assert.Equal(t, MappedSplitLaneLLMUserKey, llmKey.Lane)
	assert.True(t, llmKey.IsPartitioned())
	assert.Regexp(t, `^llm-user-key-[0-9]{3}$`, llmKey.Component())

	mcpProfile, err := spec.MCPUserProfileBundle("profile-a")
	require.NoError(t, err)
	assert.Equal(t, MappedSplitLaneMCPUserProfile, mcpProfile.Lane)
	assert.True(t, mcpProfile.IsPartitioned())
	assert.Regexp(t, `^mcp-user-profile-[0-9]{3}$`, mcpProfile.Component())
}

func TestMappedSplitSpecAffectedBundle(t *testing.T) {
	spec := MappedSplitSpec{
		LLMUserKeyPartitions:     16,
		MCPUserProfilePartitions: 16,
	}

	key, err := spec.AffectedBundle(MappedSplitChange{Kind: MappedSplitChangeLLMGeneric})
	require.NoError(t, err)
	assert.Equal(t, "llm-generic", key.Component())

	key, err = spec.AffectedBundle(MappedSplitChange{
		Kind:          MappedSplitChangeLLMUserKey,
		PrincipalSlug: "slug:key-a",
	})
	require.NoError(t, err)
	assert.Equal(t, MappedSplitLaneLLMUserKey, key.Lane)
	assert.GreaterOrEqual(t, key.Partition, 0)
	assert.Less(t, key.Partition, spec.LLMUserKeyPartitions)

	key, err = spec.AffectedBundle(MappedSplitChange{
		Kind:       MappedSplitChangeMCPUserProfile,
		PathSuffix: "profile-a",
	})
	require.NoError(t, err)
	assert.Equal(t, MappedSplitLaneMCPUserProfile, key.Lane)
	assert.GreaterOrEqual(t, key.Partition, 0)
	assert.Less(t, key.Partition, spec.MCPUserProfilePartitions)
}

func TestMappedSplitSpecValidation(t *testing.T) {
	assert.NoError(t, (MappedSplitSpec{
		LLMUserKeyPartitions:     1,
		MCPUserProfilePartitions: 1,
	}).Validate())
	assert.Error(t, (MappedSplitSpec{
		LLMUserKeyPartitions:     0,
		MCPUserProfilePartitions: 1,
	}).Validate())
	assert.Error(t, (MappedSplitSpec{
		LLMUserKeyPartitions:     1,
		MCPUserProfilePartitions: 0,
	}).Validate())

	spec := MappedSplitSpec{
		LLMUserKeyPartitions:     4,
		MCPUserProfilePartitions: 4,
	}
	_, err := spec.CatalogBundle(MappedSplitLaneLLMUserKey)
	assert.Error(t, err)
	_, err = spec.AffectedBundle(MappedSplitChange{Kind: MappedSplitChangeLLMUserKey})
	assert.Error(t, err)
	_, err = spec.AffectedBundle(MappedSplitChange{Kind: MappedSplitChangeMCPUserProfile})
	assert.Error(t, err)
	_, err = spec.AffectedBundle(MappedSplitChange{Kind: "unknown"})
	assert.Error(t, err)
}

func TestMappedSplitSpecPartitionIsStable(t *testing.T) {
	spec := MappedSplitSpec{
		LLMUserKeyPartitions:     64,
		MCPUserProfilePartitions: 64,
	}

	first, err := spec.LLMUserKeyPartition("slug:key-a")
	require.NoError(t, err)
	second, err := spec.LLMUserKeyPartition("slug:key-a")
	require.NoError(t, err)
	assert.Equal(t, first, second)

	profile, err := spec.MCPUserProfilePartition("profile-a")
	require.NoError(t, err)
	assert.GreaterOrEqual(t, profile, 0)
	assert.Less(t, profile, 64)
}
