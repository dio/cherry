package cherry

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSnapshotPolicyPeriodicReactsToPrincipalBindingChange(t *testing.T) {
	decision := DefaultSnapshotPolicy().Decide([]SnapshotChange{
		{
			Kind:          SnapshotChangePrincipalBinding,
			ScopeID:       "workspace2",
			PrincipalSlug: "slug:key2",
			Reason:        "key binding changed",
		},
		{
			Kind:          SnapshotChangePrincipalRoute,
			ScopeID:       "workspace1",
			PrincipalSlug: "slug:key1",
		},
		{
			Kind:          SnapshotChangePrincipalRoute,
			ScopeID:       "workspace1",
			PrincipalSlug: "slug:key1",
		},
	})

	assert.True(t, decision.TakeSnapshot)
	assert.Equal(t, "reactive change: key binding changed", decision.Reason)
	assert.Equal(t, []string{"workspace1", "workspace2"}, decision.ChangedScopes)
}

func TestSnapshotPolicyPeriodicDefersStaticCatalogChange(t *testing.T) {
	decision := DefaultSnapshotPolicy().Decide([]SnapshotChange{
		{Kind: SnapshotChangeStaticCatalog, ScopeID: "workspace1"},
	})

	assert.False(t, decision.TakeSnapshot)
	assert.Equal(t, "periodic cadence", decision.Reason)
	assert.Equal(t, []string{"workspace1"}, decision.ChangedScopes)
}

func TestSnapshotPolicyReactiveCadenceSnapshotsAnyChange(t *testing.T) {
	policy := SnapshotPolicy{Cadence: SnapshotCadenceReactive}

	decision := policy.Decide([]SnapshotChange{
		{Kind: SnapshotChangeStaticCatalog},
	})

	assert.True(t, decision.TakeSnapshot)
	assert.Equal(t, "reactive cadence", decision.Reason)
	assert.Empty(t, decision.ChangedScopes)
}

func TestSnapshotPolicyAllowsDisablingReactiveInterrupts(t *testing.T) {
	policy := SnapshotPolicy{
		Cadence:       SnapshotCadencePeriodic,
		ReactiveKinds: []SnapshotChangeKind{},
	}

	decision := policy.Decide([]SnapshotChange{
		{Kind: SnapshotChangeSecretRef, ScopeID: "workspace1"},
	})

	assert.False(t, decision.TakeSnapshot)
	assert.Equal(t, "periodic cadence", decision.Reason)
	assert.Equal(t, []string{"workspace1"}, decision.ChangedScopes)
}
