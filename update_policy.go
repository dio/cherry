package cherry

import "sort"

// SnapshotCadence describes how a caller normally publishes new immutable
// bundle generations.
type SnapshotCadence string

const (
	// SnapshotCadencePeriodic means the caller normally publishes snapshots on a
	// timer or poll loop, while selected high-priority changes may still request
	// an immediate snapshot.
	SnapshotCadencePeriodic SnapshotCadence = "periodic"
	// SnapshotCadenceReactive means every observed change should request an
	// immediate snapshot.
	SnapshotCadenceReactive SnapshotCadence = "reactive"
)

// SnapshotChangeKind classifies a normalized change after the external system
// has already handled source records, tenancy, key verification, and rule merge.
type SnapshotChangeKind string

const (
	// SnapshotChangeUnknown is for callers that cannot classify the change.
	SnapshotChangeUnknown SnapshotChangeKind = "unknown"
	// SnapshotChangeStaticCatalog covers mostly static provider, model, or MCP
	// server catalog metadata.
	SnapshotChangeStaticCatalog SnapshotChangeKind = "static_catalog"
	// SnapshotChangeScope covers adding or removing an enforcement scope.
	SnapshotChangeScope SnapshotChangeKind = "scope"
	// SnapshotChangePrincipalBinding covers verifier-visible principal
	// membership or reachability changes. A changed external key should usually
	// be mapped to this kind after verification and normalization.
	SnapshotChangePrincipalBinding SnapshotChangeKind = "principal_binding"
	// SnapshotChangePrincipalRoute covers an LLM route change for a principal.
	SnapshotChangePrincipalRoute SnapshotChangeKind = "principal_route"
	// SnapshotChangeRatePolicy covers immutable rate policy metadata changes.
	SnapshotChangeRatePolicy SnapshotChangeKind = "rate_policy"
	// SnapshotChangeSecretRef covers effective provider, route, MCP server, or
	// MCP tool secret-ref changes. It must never carry secret material.
	SnapshotChangeSecretRef SnapshotChangeKind = "secret_ref"
	// SnapshotChangeMCPProfile covers adding or removing an MCP path/profile.
	SnapshotChangeMCPProfile SnapshotChangeKind = "mcp_profile"
	// SnapshotChangeMCPToolBinding covers the tool bindings under an MCP profile.
	SnapshotChangeMCPToolBinding SnapshotChangeKind = "mcp_tool_binding"
)

// SnapshotChange is a normalized change signal that can be used to decide
// whether to publish an immutable snapshot before the next periodic cadence.
//
// ScopeID, PrincipalSlug, and MCPPath are optional routing labels for callers
// that maintain scope-sharded overlays. They are not used for verification.
type SnapshotChange struct {
	Kind          SnapshotChangeKind
	ScopeID       string
	PrincipalSlug string
	MCPPath       string
	Reason        string
}

// SnapshotPolicy decides when observed normalized changes should request a new
// immutable snapshot generation.
type SnapshotPolicy struct {
	// Cadence defaults to SnapshotCadencePeriodic when empty.
	Cadence SnapshotCadence
	// ReactiveKinds lists changes that should interrupt a periodic cadence. A nil
	// slice uses DefaultReactiveSnapshotKinds. An empty non-nil slice disables
	// reactive interrupts under SnapshotCadencePeriodic.
	ReactiveKinds []SnapshotChangeKind
}

// SnapshotDecision is the result of evaluating observed changes against a
// SnapshotPolicy.
type SnapshotDecision struct {
	TakeSnapshot  bool
	Reason        string
	ChangedScopes []string
}

// DefaultReactiveSnapshotKinds returns mutable policy/profile change kinds that
// should normally interrupt a periodic snapshot cadence.
func DefaultReactiveSnapshotKinds() []SnapshotChangeKind {
	return []SnapshotChangeKind{
		SnapshotChangeScope,
		SnapshotChangePrincipalBinding,
		SnapshotChangePrincipalRoute,
		SnapshotChangeRatePolicy,
		SnapshotChangeSecretRef,
		SnapshotChangeMCPProfile,
		SnapshotChangeMCPToolBinding,
	}
}

// DefaultSnapshotPolicy returns Cherry's recommended starting point for V2-style
// incremental update planning: keep a periodic cadence for static catalog churn,
// but react immediately to mutable policy/profile surfaces.
func DefaultSnapshotPolicy() SnapshotPolicy {
	return SnapshotPolicy{
		Cadence:       SnapshotCadencePeriodic,
		ReactiveKinds: nil,
	}
}

// Decide evaluates whether the observed changes should trigger a new immutable
// snapshot before the next periodic cadence.
func (p SnapshotPolicy) Decide(changes []SnapshotChange) SnapshotDecision {
	if len(changes) == 0 {
		return SnapshotDecision{Reason: "no changes"}
	}

	changedScopes := changedScopeIDs(changes)
	cadence := p.Cadence
	if cadence == "" {
		cadence = SnapshotCadencePeriodic
	}
	if cadence == SnapshotCadenceReactive {
		return SnapshotDecision{
			TakeSnapshot:  true,
			Reason:        "reactive cadence",
			ChangedScopes: changedScopes,
		}
	}

	reactiveKinds := p.ReactiveKinds
	if reactiveKinds == nil {
		reactiveKinds = DefaultReactiveSnapshotKinds()
	}
	reactive := snapshotKindSet(reactiveKinds)
	for _, change := range changes {
		if reactive[change.Kind] {
			reason := string(change.Kind)
			if change.Reason != "" {
				reason = change.Reason
			}
			return SnapshotDecision{
				TakeSnapshot:  true,
				Reason:        "reactive change: " + reason,
				ChangedScopes: changedScopes,
			}
		}
	}

	return SnapshotDecision{
		Reason:        "periodic cadence",
		ChangedScopes: changedScopes,
	}
}

func snapshotKindSet(kinds []SnapshotChangeKind) map[SnapshotChangeKind]bool {
	set := make(map[SnapshotChangeKind]bool, len(kinds))
	for _, kind := range kinds {
		set[kind] = true
	}
	return set
}

func changedScopeIDs(changes []SnapshotChange) []string {
	seen := make(map[string]bool)
	for _, change := range changes {
		if change.ScopeID == "" || seen[change.ScopeID] {
			continue
		}
		seen[change.ScopeID] = true
	}

	scopes := make([]string, 0, len(seen))
	for scopeID := range seen {
		scopes = append(scopes, scopeID)
	}
	sort.Strings(scopes)
	return scopes
}
