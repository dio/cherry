package cherry

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMappedSplitBuildOpenQueryAndPartitionSwap(t *testing.T) {
	const (
		generationID = "gen-mapped-1"
		scopeKind    = "workspace"
		scopeID      = "workspace1"
		partitions   = 4
		defaultSlug  = "slug:default"
	)

	input := testMappedSplitInput("env://OPENAI_KEY_A")
	mapBytes, store := buildTestMappedSplit(
		t,
		scopeKind,
		scopeID,
		[]string{scopeID},
		generationID,
		defaultSlug,
		partitions,
		input,
	)

	view, stats := openTestMappedSplit(t, mapBytes, nil, store.fetch)
	assert.Equal(t, testMappedSplitOpenStats{Fetched: 10}, stats)
	var splitMap testSplitMap
	require.NoError(t, json.Unmarshal(mapBytes, &splitMap))
	spec := MappedSplitSpec{
		LLMUserKeyPartitions:     partitions,
		MCPUserProfilePartitions: partitions,
	}

	keyA, source, ok := view.resolveLLM(scopeID, "slug:key-a", "gpt-4o-mini")
	require.True(t, ok)
	assert.Equal(t, testLLMUserKey(t, spec, "slug:key-a").Component(), source)
	assert.Equal(t, "env://OPENAI_KEY_A", keyA.SecretRef)
	assert.Equal(t, uint32(15), keyA.Rate.RPM)

	defaulted, source, ok := view.resolveLLM(scopeID, "slug:no-override", "gpt-4o-mini")
	require.True(t, ok)
	assert.Equal(t, "llm-generic", source)
	assert.Equal(t, "env://OPENAI_PLATFORM", defaulted.SecretRef)
	assert.Equal(t, uint32(300), defaulted.Rate.RPM)

	serverTool, source, ok := view.resolveMCPTool(scopeID, "s/github", "github__list-repos")
	require.True(t, ok)
	assert.Equal(t, "mcp-servers", source)
	assert.Equal(t, "env://GITHUB_PLATFORM", serverTool.SecretRef)

	profileTool, source, ok := view.resolveMCPTool(scopeID, "profile-a", "github__list-repos")
	require.True(t, ok)
	assert.Equal(t, testMCPUserProfile(t, spec, "profile-a").Component(), source)
	assert.Equal(t, "env://GITHUB_PROFILE_A", profileTool.SecretRef)

	profileB, source, ok := view.resolveMCPTool(scopeID, "profile-b", "github__list-repos")
	require.True(t, ok)
	removedMCPPartition := testMCPUserProfile(t, spec, "profile-b").Partition
	assert.Equal(t, testMCPUserProfile(t, spec, "profile-b").Component(), source)
	assert.Equal(t, "env://GITHUB_PROFILE_B", profileB.SecretRef)

	updatedInput := testMappedSplitInput("env://OPENAI_KEY_A_V2")
	updatedLLMKey := testLLMUserKey(t, spec, "slug:key-a")
	updatedPartition := updatedLLMKey.Partition
	updatedBundle := buildTestLLMPartitionBundle(
		t,
		scopeKind,
		scopeID,
		[]string{scopeID},
		generationID,
		updatedInput,
		partitions,
		updatedPartition,
	)
	updatedOpened, err := OpenBundleZstd(updatedBundle)
	require.NoError(t, err)
	view = view.withLLMPartition(updatedPartition, updatedOpened.Reader)

	keyA, source, ok = view.resolveLLM(scopeID, "slug:key-a", "gpt-4o-mini")
	require.True(t, ok)
	assert.Equal(t, updatedLLMKey.Component(), source)
	assert.Equal(t, "env://OPENAI_KEY_A_V2", keyA.SecretRef)

	keyB, source, ok := view.resolveLLM(scopeID, "slug:key-b", "gpt-4o-mini")
	require.True(t, ok)
	assert.Equal(t, testLLMUserKey(t, spec, "slug:key-b").Component(), source)
	assert.Equal(t, "env://OPENAI_KEY_B", keyB.SecretRef)

	nextMap := cloneTestSplitMap(t, splitMap)
	nextMap.MapRevision = 2
	nextURL := "mem://" + generationID + "/" + updatedLLMKey.Component() + "-v2.zst"
	nextManifest, err := readTestBundleManifest(updatedBundle)
	require.NoError(t, err)
	store[nextURL] = updatedBundle
	replaceTestPartitionRef(
		t,
		nextMap.PartitionBundles[string(MappedSplitLaneLLMUserKey)],
		updatedPartition,
		testPartitionBundleRef{
			Partition: updatedPartition,
			URL:       nextURL,
			Checksum:  nextManifest.Checksum,
			Size:      nextManifest.SizeBytes,
		},
	)
	nextMap.PartitionBundles[string(MappedSplitLaneMCPUserProfile)] = removeTestPartitionRef(
		nextMap.PartitionBundles[string(MappedSplitLaneMCPUserProfile)],
		removedMCPPartition,
	)
	nextMapBytes, err := json.MarshalIndent(nextMap, "", "  ")
	require.NoError(t, err)

	nextView, stats := openTestMappedSplit(t, nextMapBytes, &view, store.fetch)
	assert.Equal(t, testMappedSplitOpenStats{
		Fetched: 1,
		Reused:  8,
		Omitted: 1,
	}, stats)
	keyA, source, ok = nextView.resolveLLM(scopeID, "slug:key-a", "gpt-4o-mini")
	require.True(t, ok)
	assert.Equal(t, updatedLLMKey.Component(), source)
	assert.Equal(t, "env://OPENAI_KEY_A_V2", keyA.SecretRef)

	_, source, ok = nextView.resolveMCPTool(scopeID, "profile-b", "github__list-repos")
	assert.False(t, ok)
	assert.Empty(t, source)

	defaulted, source, ok = nextView.resolveLLM(scopeID, "slug:key-b", "gpt-4o-mini")
	require.True(t, ok)
	assert.Equal(t, testLLMUserKey(t, spec, "slug:key-b").Component(), source)
	assert.Equal(t, "env://OPENAI_KEY_B", defaulted.SecretRef)
}

type testSplitMap struct {
	FormatVersion           string                              `json:"format_version"`
	ScopeKind               string                              `json:"scope_kind"`
	ScopeID                 string                              `json:"scope_id"`
	Scopes                  []string                            `json:"scopes"`
	GenerationID            string                              `json:"generation_id"`
	MapRevision             int                                 `json:"map_revision"`
	LLMDefaultPrincipalSlug string                              `json:"llm_default_principal_slug"`
	Partitioning            map[string]testPartitionSpec        `json:"partitioning"`
	Bundles                 map[string]testBundleRef            `json:"bundles"`
	PartitionBundles        map[string][]testPartitionBundleRef `json:"partition_bundles"`
}

type testPartitionSpec struct {
	Algorithm  string `json:"algorithm"`
	Key        string `json:"key"`
	Partitions int    `json:"partitions"`
}

type testBundleRef struct {
	URL      string `json:"url"`
	Checksum uint64 `json:"checksum"`
	Size     uint64 `json:"size"`
}

type testPartitionBundleRef struct {
	Partition int    `json:"partition"`
	URL       string `json:"url"`
	Checksum  uint64 `json:"checksum"`
	Size      uint64 `json:"size"`
}

type testBundleStore map[string][]byte

func (s testBundleStore) fetch(url string) ([]byte, error) {
	payload, ok := s[url]
	if !ok {
		return nil, fmt.Errorf("missing bundle %q", url)
	}
	return payload, nil
}

type testMappedSplitView struct {
	generationID            string
	llmDefaultPrincipalSlug string
	spec                    MappedSplitSpec
	llmGenericRef           testBundleRef
	mcpServersRef           testBundleRef
	llmUserKeyRefs          []testBundleRef
	mcpUserProfileRefs      []testBundleRef
	llmGeneric              Reader
	mcpServers              Reader
	llmUserKey              []Reader
	mcpUserProfile          []Reader
}

type testMappedSplitOpenStats struct {
	Fetched int
	Reused  int
	Omitted int
}

func buildTestMappedSplit(
	t *testing.T,
	scopeKind string,
	scopeID string,
	scopes []string,
	generationID string,
	defaultSlug string,
	partitions int,
	input Input,
) ([]byte, testBundleStore) {
	t.Helper()
	spec := MappedSplitSpec{
		LLMUserKeyPartitions:     partitions,
		MCPUserProfilePartitions: partitions,
	}
	require.NoError(t, spec.Validate())

	store := testBundleStore{}
	splitMap := testSplitMap{
		FormatVersion:           "mapped-split-v1",
		ScopeKind:               scopeKind,
		ScopeID:                 scopeID,
		Scopes:                  append([]string(nil), scopes...),
		GenerationID:            generationID,
		MapRevision:             1,
		LLMDefaultPrincipalSlug: defaultSlug,
		Partitioning: map[string]testPartitionSpec{
			string(MappedSplitLaneLLMUserKey): {
				Algorithm:  "fnv1a64",
				Key:        "principal_slug",
				Partitions: partitions,
			},
			string(MappedSplitLaneMCPUserProfile): {
				Algorithm:  "fnv1a64",
				Key:        "path_suffix",
				Partitions: partitions,
			},
		},
		Bundles:          map[string]testBundleRef{},
		PartitionBundles: map[string][]testPartitionBundleRef{},
	}

	addBundle := func(component string, layerInput Input) testBundleRef {
		t.Helper()
		url := "mem://" + generationID + "/" + component + ".zst"
		payload, manifest := buildTestBundle(
			t,
			scopeKind,
			scopeID,
			scopes,
			generationID,
			layerInput,
		)
		store[url] = payload
		return testBundleRef{
			URL:      url,
			Checksum: manifest.Checksum,
			Size:     manifest.SizeBytes,
		}
	}

	llmGeneric, err := spec.CatalogBundle(MappedSplitLaneLLMGeneric)
	require.NoError(t, err)
	mcpServers, err := spec.CatalogBundle(MappedSplitLaneMCPServers)
	require.NoError(t, err)

	splitMap.Bundles[string(MappedSplitLaneLLMGeneric)] = addBundle(
		llmGeneric.Component(),
		testLLMGenericInput(input, defaultSlug),
	)
	splitMap.Bundles[string(MappedSplitLaneMCPServers)] = addBundle(
		mcpServers.Component(),
		testMCPServersInput(input),
	)

	for partition := range partitions {
		key := MappedSplitBundleKey{Lane: MappedSplitLaneLLMUserKey, Partition: partition}
		ref := addBundle(
			key.Component(),
			testLLMPartitionInput(input, partitions, partition),
		)
		splitMap.PartitionBundles[string(MappedSplitLaneLLMUserKey)] = append(
			splitMap.PartitionBundles[string(MappedSplitLaneLLMUserKey)],
			testPartitionBundleRef{
				Partition: partition,
				URL:       ref.URL,
				Checksum:  ref.Checksum,
				Size:      ref.Size,
			},
		)

		key = MappedSplitBundleKey{Lane: MappedSplitLaneMCPUserProfile, Partition: partition}
		ref = addBundle(
			key.Component(),
			testMCPProfilePartitionInput(input, partitions, partition),
		)
		splitMap.PartitionBundles[string(MappedSplitLaneMCPUserProfile)] = append(
			splitMap.PartitionBundles[string(MappedSplitLaneMCPUserProfile)],
			testPartitionBundleRef{
				Partition: partition,
				URL:       ref.URL,
				Checksum:  ref.Checksum,
				Size:      ref.Size,
			},
		)
	}

	mapBytes, err := json.MarshalIndent(splitMap, "", "  ")
	require.NoError(t, err)
	return mapBytes, store
}

func openTestMappedSplit(
	t *testing.T,
	mapBytes []byte,
	previous *testMappedSplitView,
	fetch func(string) ([]byte, error),
) (testMappedSplitView, testMappedSplitOpenStats) {
	t.Helper()
	var splitMap testSplitMap
	require.NoError(t, json.Unmarshal(mapBytes, &splitMap))
	require.Equal(t, "mapped-split-v1", splitMap.FormatVersion)

	llmGenericRef := splitMap.Bundles[string(MappedSplitLaneLLMGeneric)]
	mcpServersRef := splitMap.Bundles[string(MappedSplitLaneMCPServers)]
	llmGeneric, llmGenericStats := openTestBundleRef(
		t,
		splitMap,
		llmGenericRef,
		previousTestBundleReader(previous, MappedSplitLaneLLMGeneric, -1),
		fetch,
	)
	mcpServers, mcpServersStats := openTestBundleRef(
		t,
		splitMap,
		mcpServersRef,
		previousTestBundleReader(previous, MappedSplitLaneMCPServers, -1),
		fetch,
	)
	llmPartitions, llmPartitionRefs, llmPartitionStats := openTestPartitionRefs(
		t,
		splitMap,
		splitMap.Partitioning[string(MappedSplitLaneLLMUserKey)],
		splitMap.PartitionBundles[string(MappedSplitLaneLLMUserKey)],
		previous,
		MappedSplitLaneLLMUserKey,
		fetch,
	)
	mcpPartitions, mcpPartitionRefs, mcpPartitionStats := openTestPartitionRefs(
		t,
		splitMap,
		splitMap.Partitioning[string(MappedSplitLaneMCPUserProfile)],
		splitMap.PartitionBundles[string(MappedSplitLaneMCPUserProfile)],
		previous,
		MappedSplitLaneMCPUserProfile,
		fetch,
	)
	stats := mergeTestMappedSplitOpenStats(
		llmGenericStats,
		mcpServersStats,
		llmPartitionStats,
		mcpPartitionStats,
	)
	return testMappedSplitView{
		generationID:            splitMap.GenerationID,
		llmDefaultPrincipalSlug: splitMap.LLMDefaultPrincipalSlug,
		spec: MappedSplitSpec{
			LLMUserKeyPartitions:     splitMap.Partitioning[string(MappedSplitLaneLLMUserKey)].Partitions,
			MCPUserProfilePartitions: splitMap.Partitioning[string(MappedSplitLaneMCPUserProfile)].Partitions,
		},
		llmGenericRef:      llmGenericRef,
		mcpServersRef:      mcpServersRef,
		llmUserKeyRefs:     llmPartitionRefs,
		mcpUserProfileRefs: mcpPartitionRefs,
		llmGeneric:         llmGeneric.Reader,
		mcpServers:         mcpServers.Reader,
		llmUserKey:         llmPartitions,
		mcpUserProfile:     mcpPartitions,
	}, stats
}

func openTestBundleRef(
	t *testing.T,
	splitMap testSplitMap,
	ref testBundleRef,
	previous testCachedBundleReader,
	fetch func(string) ([]byte, error),
) (OpenedBundle, testMappedSplitOpenStats) {
	t.Helper()
	if previous.matches(splitMap.GenerationID, ref) {
		return OpenedBundle{Reader: previous.Reader}, testMappedSplitOpenStats{Reused: 1}
	}
	payload, err := fetch(ref.URL)
	require.NoError(t, err)
	opened, err := OpenBundleZstd(payload)
	require.NoError(t, err)
	require.Equal(t, splitMap.ScopeKind, opened.Metadata.ScopeKind)
	require.Equal(t, splitMap.ScopeID, opened.Metadata.ScopeID)
	require.ElementsMatch(t, splitMap.Scopes, opened.Metadata.Scopes)
	require.Equal(t, splitMap.GenerationID, opened.Metadata.GenerationID)
	require.Equal(t, ref.Checksum, opened.Metadata.PackManifest.Checksum)
	require.Equal(t, ref.Size, opened.Metadata.PackManifest.SizeBytes)
	return opened, testMappedSplitOpenStats{Fetched: 1}
}

func openTestPartitionRefs(
	t *testing.T,
	splitMap testSplitMap,
	spec testPartitionSpec,
	refs []testPartitionBundleRef,
	previous *testMappedSplitView,
	lane MappedSplitLane,
	fetch func(string) ([]byte, error),
) ([]Reader, []testBundleRef, testMappedSplitOpenStats) {
	t.Helper()
	require.Equal(t, "fnv1a64", spec.Algorithm)
	readers := make([]Reader, spec.Partitions)
	nextRefs := make([]testBundleRef, spec.Partitions)
	seen := make([]bool, spec.Partitions)
	stats := testMappedSplitOpenStats{}
	for _, ref := range refs {
		require.GreaterOrEqual(t, ref.Partition, 0)
		require.Less(t, ref.Partition, spec.Partitions)
		nextRef := testBundleRef{
			URL:      ref.URL,
			Checksum: ref.Checksum,
			Size:     ref.Size,
		}
		opened, openStats := openTestBundleRef(
			t,
			splitMap,
			nextRef,
			previousTestBundleReader(previous, lane, ref.Partition),
			fetch,
		)
		stats = mergeTestMappedSplitOpenStats(stats, openStats)
		seen[ref.Partition] = true
		nextRefs[ref.Partition] = nextRef
		readers[ref.Partition] = opened.Reader
	}
	if previous != nil && previous.generationID == splitMap.GenerationID {
		for partition := range spec.Partitions {
			if seen[partition] {
				continue
			}
			if previousTestBundleReader(previous, lane, partition).Ref.URL != "" {
				stats.Omitted++
			}
		}
	}
	return readers, nextRefs, stats
}

type testCachedBundleReader struct {
	GenerationID string
	Ref          testBundleRef
	Reader       Reader
}

func (r testCachedBundleReader) matches(generationID string, ref testBundleRef) bool {
	return r.GenerationID == generationID && r.Ref == ref && r.Ref.URL != ""
}

func previousTestBundleReader(
	previous *testMappedSplitView,
	lane MappedSplitLane,
	partition int,
) testCachedBundleReader {
	if previous == nil {
		return testCachedBundleReader{}
	}
	switch lane {
	case MappedSplitLaneLLMGeneric:
		return testCachedBundleReader{
			GenerationID: previous.generationID,
			Ref:          previous.llmGenericRef,
			Reader:       previous.llmGeneric,
		}
	case MappedSplitLaneMCPServers:
		return testCachedBundleReader{
			GenerationID: previous.generationID,
			Ref:          previous.mcpServersRef,
			Reader:       previous.mcpServers,
		}
	case MappedSplitLaneLLMUserKey:
		if partition >= 0 && partition < len(previous.llmUserKeyRefs) && partition < len(previous.llmUserKey) {
			return testCachedBundleReader{
				GenerationID: previous.generationID,
				Ref:          previous.llmUserKeyRefs[partition],
				Reader:       previous.llmUserKey[partition],
			}
		}
	case MappedSplitLaneMCPUserProfile:
		if partition >= 0 && partition < len(previous.mcpUserProfileRefs) && partition < len(previous.mcpUserProfile) {
			return testCachedBundleReader{
				GenerationID: previous.generationID,
				Ref:          previous.mcpUserProfileRefs[partition],
				Reader:       previous.mcpUserProfile[partition],
			}
		}
	}
	return testCachedBundleReader{}
}

func mergeTestMappedSplitOpenStats(stats ...testMappedSplitOpenStats) testMappedSplitOpenStats {
	var out testMappedSplitOpenStats
	for _, stat := range stats {
		out.Fetched += stat.Fetched
		out.Reused += stat.Reused
		out.Omitted += stat.Omitted
	}
	return out
}

func (v testMappedSplitView) resolveLLM(
	scopeID string,
	principalSlug string,
	modelID string,
) (LLMResult, string, bool) {
	key, err := v.spec.LLMUserKeyBundle(principalSlug)
	if err != nil {
		return LLMResult{}, "", false
	}
	partition := key.Partition
	if partition >= len(v.llmUserKey) {
		return LLMResult{}, "", false
	}
	reader := v.llmUserKey[partition]
	if ids, ok := reader.ResolveLLMIDs(scopeID, principalSlug, modelID); ok {
		return materializeTestLLM(reader, principalSlug, ids), key.Component(), true
	}
	ids, ok := v.llmGeneric.ResolveLLMIDs(scopeID, v.llmDefaultPrincipalSlug, modelID)
	if !ok {
		return LLMResult{}, "", false
	}
	return materializeTestLLM(v.llmGeneric, principalSlug, ids), "llm-generic", true
}

func (v testMappedSplitView) resolveMCPTool(
	scopeID string,
	pathSuffix string,
	exposedTool string,
) (MCPTool, string, bool) {
	if strings.HasPrefix(pathSuffix, "s/") {
		tool, ok := v.mcpServers.ResolveMCPToolIDs(scopeID, pathSuffix, exposedTool)
		if !ok {
			return MCPTool{}, "", false
		}
		return materializeTestMCPTool(v.mcpServers, tool), "mcp-servers", true
	}
	key, err := v.spec.MCPUserProfileBundle(pathSuffix)
	if err != nil {
		return MCPTool{}, "", false
	}
	partition := key.Partition
	if partition >= len(v.mcpUserProfile) {
		return MCPTool{}, "", false
	}
	reader := v.mcpUserProfile[partition]
	tool, ok := reader.ResolveMCPToolIDs(scopeID, pathSuffix, exposedTool)
	if !ok {
		return MCPTool{}, "", false
	}
	return materializeTestMCPTool(reader, tool), key.Component(), true
}

func (v testMappedSplitView) withLLMPartition(partition int, reader Reader) testMappedSplitView {
	next := v
	next.llmUserKey = append([]Reader(nil), v.llmUserKey...)
	next.llmUserKey[partition] = reader
	return next
}

func materializeTestLLM(reader Reader, principalSlug string, ids LLMIDs) LLMResult {
	return LLMResult{
		PrincipalSlug: principalSlug,
		Provider:      reader.String(ids.ProviderSID),
		ProviderKind:  reader.String(ids.KindSID),
		Endpoint:      reader.String(ids.EndpointSID),
		Model:         reader.String(ids.ModelSID),
		ModelName:     reader.String(ids.ModelNameSID),
		SecretRef:     reader.String(ids.SecretSID),
		Rate: RatePolicy{
			USDPerDayCents: ids.Rate.USDPerDayCents,
			RPM:            ids.Rate.RPM,
			OnExceed:       reader.String(ids.Rate.OnExceedSID),
		},
	}
}

func materializeTestMCPTool(reader Reader, ids MCPToolIDs) MCPTool {
	return MCPTool{
		ExposedName:    reader.String(ids.ExposedNameSID),
		Server:         reader.String(ids.ServerSID),
		ServerEndpoint: reader.String(ids.ServerEndpointSID),
		Tool:           reader.String(ids.ToolSID),
		SecretRef:      reader.String(ids.SecretSID),
		AuthType:       reader.String(ids.AuthTypeSID),
	}
}

func buildTestLLMPartitionBundle(
	t *testing.T,
	scopeKind string,
	scopeID string,
	scopes []string,
	generationID string,
	input Input,
	partitions int,
	partition int,
) []byte {
	t.Helper()
	payload, _ := buildTestBundle(
		t,
		scopeKind,
		scopeID,
		scopes,
		generationID,
		testLLMPartitionInput(input, partitions, partition),
	)
	return payload
}

func readTestBundleManifest(payload []byte) (Manifest, error) {
	bundle, err := DecodeBundleZstd(payload)
	if err != nil {
		return Manifest{}, err
	}
	return bundle.Metadata.PackManifest, nil
}

func cloneTestSplitMap(t *testing.T, splitMap testSplitMap) testSplitMap {
	t.Helper()
	data, err := json.Marshal(splitMap)
	require.NoError(t, err)
	var cloned testSplitMap
	require.NoError(t, json.Unmarshal(data, &cloned))
	return cloned
}

func replaceTestPartitionRef(
	t *testing.T,
	refs []testPartitionBundleRef,
	partition int,
	next testPartitionBundleRef,
) {
	t.Helper()
	for i, ref := range refs {
		if ref.Partition != partition {
			continue
		}
		refs[i] = next
		return
	}
	t.Fatalf("partition %d not found", partition)
}

func removeTestPartitionRef(
	refs []testPartitionBundleRef,
	partition int,
) []testPartitionBundleRef {
	out := make([]testPartitionBundleRef, 0, len(refs))
	for _, ref := range refs {
		if ref.Partition == partition {
			continue
		}
		out = append(out, ref)
	}
	return out
}

func buildTestBundle(
	t *testing.T,
	scopeKind string,
	scopeID string,
	scopes []string,
	generationID string,
	input Input,
) ([]byte, Manifest) {
	t.Helper()
	blob, manifest, err := BuildWithManifest(input)
	require.NoError(t, err)
	bundle := NewBundle(scopeKind, scopeID, scopes, blob, manifest)
	bundle.Metadata.GenerationID = generationID
	payload, err := EncodeBundleZstd(bundle)
	require.NoError(t, err)
	return payload, manifest
}

func testLLMUserKey(t *testing.T, spec MappedSplitSpec, principalSlug string) MappedSplitBundleKey {
	t.Helper()
	key, err := spec.LLMUserKeyBundle(principalSlug)
	require.NoError(t, err)
	return key
}

func testMCPUserProfile(t *testing.T, spec MappedSplitSpec, pathSuffix string) MappedSplitBundleKey {
	t.Helper()
	key, err := spec.MCPUserProfileBundle(pathSuffix)
	require.NoError(t, err)
	return key
}

func testMappedSplitInput(keyASecret string) Input {
	return Input{
		Providers: []Provider{
			{
				ID:        "openai",
				Kind:      "openai",
				Endpoint:  "https://api.openai.com",
				SecretRef: "env://OPENAI_PLATFORM",
			},
		},
		Models: []Model{
			{
				ID:       "gpt-4o-mini",
				Provider: "openai",
				Name:     "gpt-4o-mini",
				Mode:     "chat",
			},
		},
		MCPServers: []MCPServer{
			{
				ID:        "github",
				Endpoint:  "https://api.github.com",
				AuthType:  "bearer",
				SecretRef: "env://GITHUB_PLATFORM",
			},
		},
		Scopes: []Scope{{
			ID: "workspace1",
			Principals: []Principal{
				testPrincipal("slug:key-a", keyASecret, 15),
				testPrincipal("slug:key-b", "env://OPENAI_KEY_B", 25),
			},
			MCPProfiles: []MCPProfile{
				{
					Path: "s/github",
					Tools: []MCPToolBinding{{
						ExposedName: "github__list-repos",
						Server:      "github",
						Tool:        "list-repos",
						SecretRef:   "env://GITHUB_PLATFORM",
						AuthType:    "bearer",
					}},
				},
				{
					Path: "profile-a",
					Tools: []MCPToolBinding{{
						ExposedName: "github__list-repos",
						Server:      "github",
						Tool:        "list-repos",
						SecretRef:   "env://GITHUB_PROFILE_A",
						AuthType:    "bearer",
					}},
				},
				{
					Path: "profile-b",
					Tools: []MCPToolBinding{{
						ExposedName: "github__list-repos",
						Server:      "github",
						Tool:        "list-repos",
						SecretRef:   "env://GITHUB_PROFILE_B",
						AuthType:    "bearer",
					}},
				},
			},
		}},
	}
}

func testPrincipal(slug string, secretRef string, rpm uint32) Principal {
	return Principal{
		Slug: slug,
		ModelRoutes: map[string]RoutePlan{
			"gpt-4o-mini": {
				Provider:  "openai",
				Model:     "gpt-4o-mini",
				SecretRef: secretRef,
			},
		},
		Rate: RatePolicy{
			USDPerDayCents: 5000,
			RPM:            rpm,
			OnExceed:       "reject",
		},
	}
}

func testLLMGenericInput(input Input, defaultSlug string) Input {
	out := Input{
		Providers: append([]Provider(nil), input.Providers...),
		Models:    append([]Model(nil), input.Models...),
		Scopes:    make([]Scope, 0, len(input.Scopes)),
	}
	routes := make(map[string]RoutePlan, len(input.Models))
	for _, model := range input.Models {
		routes[model.ID] = RoutePlan{
			Provider: model.Provider,
			Model:    model.ID,
		}
	}
	for _, scope := range input.Scopes {
		out.Scopes = append(out.Scopes, Scope{
			ID: scope.ID,
			Principals: []Principal{{
				Slug:        defaultSlug,
				ModelRoutes: cloneTestRouteMap(routes),
				Rate: RatePolicy{
					USDPerDayCents: 50000,
					RPM:            300,
					OnExceed:       "reject",
				},
			}},
		})
	}
	return out
}

func testLLMPartitionInput(input Input, partitions int, partition int) Input {
	out := Input{
		Providers: append([]Provider(nil), input.Providers...),
		Models:    append([]Model(nil), input.Models...),
		Scopes:    make([]Scope, 0, len(input.Scopes)),
	}
	for _, scope := range input.Scopes {
		outScope := Scope{
			ID:         scope.ID,
			Principals: make([]Principal, 0, len(scope.Principals)),
		}
		for _, principal := range scope.Principals {
			spec := MappedSplitSpec{LLMUserKeyPartitions: partitions, MCPUserProfilePartitions: 1}
			actualPartition, err := spec.LLMUserKeyPartition(principal.Slug)
			if err != nil || actualPartition != partition {
				continue
			}
			outPrincipal := principal
			outPrincipal.ModelRoutes = cloneTestRouteMap(principal.ModelRoutes)
			outScope.Principals = append(outScope.Principals, outPrincipal)
		}
		out.Scopes = append(out.Scopes, outScope)
	}
	return out
}

func testMCPServersInput(input Input) Input {
	out := Input{
		MCPServers: append([]MCPServer(nil), input.MCPServers...),
		Scopes:     make([]Scope, 0, len(input.Scopes)),
	}
	for _, scope := range input.Scopes {
		outScope := Scope{
			ID:          scope.ID,
			MCPProfiles: make([]MCPProfile, 0, len(scope.MCPProfiles)),
		}
		for _, profile := range scope.MCPProfiles {
			if strings.HasPrefix(profile.Path, "s/") {
				outScope.MCPProfiles = append(outScope.MCPProfiles, cloneTestMCPProfile(profile))
			}
		}
		out.Scopes = append(out.Scopes, outScope)
	}
	return out
}

func testMCPProfilePartitionInput(input Input, partitions int, partition int) Input {
	out := Input{
		MCPServers: append([]MCPServer(nil), input.MCPServers...),
		Scopes:     make([]Scope, 0, len(input.Scopes)),
	}
	for _, scope := range input.Scopes {
		outScope := Scope{
			ID:          scope.ID,
			MCPProfiles: make([]MCPProfile, 0, len(scope.MCPProfiles)),
		}
		for _, profile := range scope.MCPProfiles {
			spec := MappedSplitSpec{LLMUserKeyPartitions: 1, MCPUserProfilePartitions: partitions}
			actualPartition, err := spec.MCPUserProfilePartition(profile.Path)
			if strings.HasPrefix(profile.Path, "s/") ||
				err != nil ||
				actualPartition != partition {
				continue
			}
			outScope.MCPProfiles = append(outScope.MCPProfiles, cloneTestMCPProfile(profile))
		}
		out.Scopes = append(out.Scopes, outScope)
	}
	return out
}

func cloneTestRouteMap(routes map[string]RoutePlan) map[string]RoutePlan {
	out := make(map[string]RoutePlan, len(routes))
	for modelID, route := range routes {
		out[modelID] = route
	}
	return out
}

func cloneTestMCPProfile(profile MCPProfile) MCPProfile {
	return MCPProfile{
		Path:  profile.Path,
		Tools: append([]MCPToolBinding(nil), profile.Tools...),
	}
}

func Example_mappedSplitMapShape() {
	splitMap := testSplitMap{
		FormatVersion:           "mapped-split-v1",
		ScopeKind:               "project",
		ScopeID:                 "project1",
		Scopes:                  []string{"workspace1", "workspace2"},
		GenerationID:            "gen42",
		MapRevision:             1,
		LLMDefaultPrincipalSlug: "slug:default",
		Partitioning: map[string]testPartitionSpec{
			"llm-user-key": {
				Algorithm:  "fnv1a64",
				Key:        "principal_slug",
				Partitions: 64,
			},
			"mcp-user-profile": {
				Algorithm:  "fnv1a64",
				Key:        "path_suffix",
				Partitions: 64,
			},
		},
		Bundles: map[string]testBundleRef{
			"llm-generic": {
				URL:      "/cherry/v1/bundles/project/project1/gen42/llm-generic.zst",
				Checksum: 1001,
				Size:     4096,
			},
			"mcp-servers": {
				URL:      "/cherry/v1/bundles/project/project1/gen42/mcp-servers.zst",
				Checksum: 1002,
				Size:     2048,
			},
		},
		PartitionBundles: map[string][]testPartitionBundleRef{
			"llm-user-key": {
				{
					Partition: 0,
					URL:       "/cherry/v1/bundles/project/project1/gen42/llm-user-key-000.zst",
					Checksum:  2000,
					Size:      1024,
				},
			},
			"mcp-user-profile": {
				{
					Partition: 0,
					URL:       "/cherry/v1/bundles/project/project1/gen42/mcp-user-profile-000.zst",
					Checksum:  3000,
					Size:      1024,
				},
			},
		},
	}
	data, _ := json.Marshal(splitMap)
	fmt.Println(len(data) > 0)
	// Output: true
}
