package server

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strconv"

	"gardenio/kbeads/internal/model"
)

// This file implements the third read-gate check ADR docs/adr/taints.md §5.1
// adds on top of the existing two project-scope gates (read_authz_scope.go):
// a caller may read a bead only when it holds a grant for every taint the
// bead carries — taintsOf(bead) ⊆ grantsOf(caller). It narrows; it never
// widens what the project gates already allow.
//
// The primary static bearer token (Type=="static", Subject=="static") gets
// no special exemption here: it falls through to grantsOf like any other
// principal, same as every other caller. ADR §13 OQ1's migration-window
// escape (BEADS_TAINT_PRIMARY_TOKEN_GRANTS=*, decision gd-KUlo9gSoRh) was
// removed in phase 3 (gd-3KUNlqC1SQ) once bridges held fleet-token grants of
// their own.

// taintGateGrants resolves the caller's taint-gate state for the current
// request. exempt is true when the gate does not apply at all — the taints
// registry is empty (ADR §10.1's zero-configuration short-circuit) — in
// which case grants is always nil and every bead passes regardless of what
// it carries. When exempt is false, grants is the caller's actual (possibly
// empty) grant set, to be compared against a bead's own taints.
//
// On a store error resolving the grant set, grants/exempt are the zero value
// and err is non-nil; callers must fail CLOSED (deny / filter out) rather
// than treat a lookup failure as an allow, since this is a security boundary
// (unlike the pre-existing project-scope gates, which fail open for
// backward compatibility — ADR docs/adr/taints.md §11 invariant #9 and the
// "narrows, never widens" framing rule that out for the taint gate).
func (s *BeadsServer) taintGateGrants(ctx context.Context) (grants []int64, exempt bool, err error) {
	nonEmpty, err := s.taintRegistryNonEmpty(ctx)
	if err != nil {
		return nil, false, err
	}
	if !nonEmpty {
		return nil, true, nil
	}
	grants, err = s.grantsOf(ctx)
	if err != nil {
		return nil, false, err
	}
	return grants, false, nil
}

// grantsOf resolves the set of taint ids the calling principal currently
// holds a grant for, per ADR docs/adr/taints.md §5.1, cached per principal
// for grantSetCacheTTL (taint_grant_cache.go). grantsOf always
// returns a concrete grant set (nil/empty means the caller holds no grants
// at all, a deny for any tainted bead). A bare primary static token
// (Type=="static", Subject=="static") falls into the default case below: it
// resolves no operator alias of its own, so it holds no grants unless it is
// relaying a human's identity via X-Operated-By — the same viewer-relay path
// every other static/fleet caller uses.
func (s *BeadsServer) grantsOf(ctx context.Context) ([]int64, error) {
	// An agent on its own per-agent token is an agent principal (keyed by its
	// bead id), not a service fleet: it holds the same grants as the static-token
	// agent it replaces, including its spawn-chain root's.
	if ref, ok := actingAgentRef(ctx); ok {
		return s.grantSetCache.load(agentGrantSetKey(ref), func() ([]int64, error) {
			return s.grantsOfAgent(ctx, ref)
		}, nil)
	}
	id := AuthIdentityFromContext(ctx)
	if id.Type == "fleet" && id.Subject != "" {
		return s.grantSetCache.load(fleetGrantSetKey(id.Subject), func() ([]int64, error) {
			return s.store.GetGrantedTaintIDs(ctx, model.PrincipalFleet, []string{id.Subject})
		}, nil)
	}
	kind, subject := s.grantOperatorAlias(ctx)
	if kind == "" {
		return nil, nil
	}
	groups := GroupsFromContext(ctx)
	return s.grantSetCache.load(operatorGrantSetKey(kind, subject, groups), func() ([]int64, error) {
		return s.grantsOfOperator(ctx, kind, subject, groups)
	}, nil)
}

// grantOperatorAlias names the OIDC/edge human whose grants the current
// request holds: the caller itself, or, for a bridge relaying one via the
// sanitized X-Operated-By header, the relayed human (the relay counts only
// from a bridge fleet subject, relayTrusted, P7 Task 3; the static token's
// relay is attribution only). kind is "" when the request resolves to no
// operator, which holds no grants.
func (s *BeadsServer) grantOperatorAlias(ctx context.Context) (kind, subject string) {
	kind, subject = operatorAliasForIdentity(AuthIdentityFromContext(ctx))
	if kind == "" {
		if op := s.trustedRelayOperator(ctx); op != "" {
			kind, subject = splitOperator(op)
		}
	}
	return kind, subject
}

// grantsOfOperator resolves the grant set of the human operator alias
// kind/subject (see grantOperatorAlias): its own taint_grants rows, plus every
// team it belongs to (§8.1, matched on groups too), plus implicit ownership
// of taints those teams own (§10.1: "creating a taint grants the owner team
// implicitly"). An alias with no operator row holds no grants.
func (s *BeadsServer) grantsOfOperator(ctx context.Context, kind, subject string, groups []string) ([]int64, error) {
	opID, err := s.store.LookupOperatorAlias(ctx, kind, subject)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if opID == 0 {
		// No operator_aliases row for this identity (sql.ErrNoRows, or a
		// store that returns (0, nil)) — nothing to have been granted,
		// matching TeamsOf's identical convention one layer up.
		return nil, nil
	}
	return s.grantsOfOperatorID(ctx, opID, groups)
}

// grantsOfOperatorID resolves the effective grant set for a KNOWN operator
// id: its own taint_grants rows, plus its teams' taint_grants rows, plus
// implicit ownership grants for taints those teams own. groups is the live
// OIDC token's group claims for team oidc_groups matching (see
// teamsForOperator) — pass nil when resolving on behalf of an operator whose
// live token isn't available (grantsOfAgent's spawn-chain root case), which
// narrows that resolution to `members`-list matches only.
func (s *BeadsServer) grantsOfOperatorID(ctx context.Context, operatorID int64, groups []string) ([]int64, error) {
	own, err := s.store.GetGrantedTaintIDs(ctx, model.PrincipalOperator, []string{strconv.FormatInt(operatorID, 10)})
	if err != nil {
		return nil, err
	}

	teams, err := s.teamsForOperator(ctx, operatorID, groups)
	if err != nil {
		return nil, err
	}
	if len(teams) == 0 {
		return own, nil
	}

	teamGrants, err := s.store.GetGrantedTaintIDs(ctx, model.PrincipalTeam, teams)
	if err != nil {
		return nil, err
	}
	ownedByTeams, err := s.store.GetTaintIDsOwnedByTeams(ctx, teams)
	if err != nil {
		return nil, err
	}
	return unionTaintIDs(own, teamGrants, ownedByTeams), nil
}

// grantsOfAgent resolves the grant set of a static-token caller whose
// subject is an agent bead id: its own taint_grants rows, plus — "an agent
// may read what the human who started it may read" — the full effective
// grant set of its spawn-chain root human (own rows, teams, and team
// ownership), resolved via the same spawn-chain walk auto_owner.go uses for
// Owner defaulting, starting from agentBeadID (not the caller, which at
// escalation replay is the approver). An agent with no resolvable spawn-chain
// root (orphaned, or rooted at a non-human sentinel) holds only its own rows.
//
// The root's grants serve the read gate only. A restrict taint is tolerated by
// the agent's own rows alone (agentOwnGrants, ADR §15.7), so a grant to the
// operator never makes every agent they started a trusted subject.
func (s *BeadsServer) grantsOfAgent(ctx context.Context, agentBeadID string) ([]int64, error) {
	own, err := s.store.GetGrantedTaintIDs(ctx, model.PrincipalAgent, []string{agentBeadID})
	if err != nil {
		return nil, err
	}

	rootIdentity, ok := s.resolveSpawnChainRootOf(ctx, agentBeadID)
	if !ok {
		return own, nil
	}
	kind, subject := splitOperator(rootIdentity)
	if kind == "" {
		return own, nil
	}
	opID, err := s.store.LookupOperatorAlias(ctx, kind, subject)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if opID == 0 {
		// No operator_aliases row for the spawn-chain-root human (sql.ErrNoRows,
		// or a store that returns (0, nil)) — nothing to have been granted to
		// them, so the agent holds only its own rows.
		return own, nil
	}

	rootGrants, err := s.grantsOfOperatorID(ctx, opID, nil)
	if err != nil {
		return nil, err
	}
	return unionTaintIDs(own, rootGrants), nil
}

// agentOwnGrants is the taint ids granted to agentBeadID itself
// (principal_kind agent), with nothing inherited from its spawn-chain root.
// It is what tolerates a restrict taint: the restricted-ops check and the
// §15.3 trusted-subject skip on read.
func (s *BeadsServer) agentOwnGrants(ctx context.Context, agentBeadID string) ([]int64, error) {
	return s.grantSetCache.load(agentOwnGrantSetKey(agentBeadID), func() ([]int64, error) {
		return s.store.GetGrantedTaintIDs(ctx, model.PrincipalAgent, []string{agentBeadID})
	}, nil)
}

// unionTaintIDs merges any number of taint id slices into a deduplicated
// slice. A nil/empty result is returned as nil (not an empty non-nil slice)
// so callers that check len()==0 vs. non-nil consistently see "no grants" as
// nil, matching every other grant-resolution branch in this file.
func unionTaintIDs(sets ...[]int64) []int64 {
	seen := make(map[int64]bool)
	for _, set := range sets {
		for _, id := range set {
			seen[id] = true
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]int64, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	return out
}

// applyTaintGateToFilter installs the caller's taint-gate predicate onto
// filter, per ADR docs/adr/taints.md §5.1's NOT EXISTS list-filter predicate.
// A gate-exempt caller or an empty taints registry leaves filter untouched
// (TaintGrantIDs stays nil: no predicate emitted). A store error resolving
// the grant set fails CLOSED — filter.TaintGrantIDs is set to an empty,
// non-nil slice, excluding every tainted bead — logged rather than
// propagated, since scopeListFilter's callers do not thread errors through
// this helper (matching its existing project-scope-gate error handling).
func (s *BeadsServer) applyTaintGateToFilter(ctx context.Context, filter model.BeadFilter) model.BeadFilter {
	grants, exempt, err := s.taintGateGrants(ctx)
	if err != nil {
		slog.Warn("taint read gate: failed to resolve caller grants, failing closed", "error", err)
		empty := []int64{}
		filter.TaintGrantIDs = &empty
		return filter
	}
	if exempt {
		return filter
	}
	filter.TaintGrantIDs = &grants
	return filter
}

// beadTaintGateState is s.taintGateGrants(ctx), hoisted once per request so a
// batch/list-style loop over many beads pays for grant resolution exactly
// once regardless of how many beads it checks — the same reason
// beadReadAuthorized takes a pre-resolved allowedOIDC rather than calling
// oidcAllowedProjects per row.
type beadTaintGateState struct {
	grants []int64
	exempt bool
}

// resolveBeadTaintGateState hoists s.taintGateGrants(ctx) for reuse across a
// per-bead loop. Callers that also need each bead's own taint ids should
// check state.exempt first: when exempt is true (gate-exempt caller, or an
// empty taints registry), skip the GetTaintsForBeads batch call entirely —
// this is what keeps a zero-config normal read off the bead_taints table.
func (s *BeadsServer) resolveBeadTaintGateState(ctx context.Context) (beadTaintGateState, error) {
	grants, exempt, err := s.taintGateGrants(ctx)
	if err != nil {
		return beadTaintGateState{}, err
	}
	return beadTaintGateState{grants: grants, exempt: exempt}, nil
}

// allows reports whether taintIDs (a single bead's taints) passes this
// resolved gate state.
func (st beadTaintGateState) allows(taintIDs []int64) bool {
	return st.exempt || taintIDsSubsetOfGrants(taintIDs, st.grants)
}

// subresourceTaintDenied resolves the caller's taint gate state and reports
// whether beadID itself is denied by it — i.e. beadID carries a taint the
// caller holds no grant for. It is the third gate on the single-bead reads
// that do not route through ListBeads/SemanticSearch: GetBead,
// subresourceParentReadable (every per-bead sub-resource) and
// authorizeBeadRead (decision/escalation/release by id), ADR
// docs/adr/taints.md §5.1.
//
// A denial does not itself write a response: a sub-resource read answers it
// as a missing parent (writeSubresourceMissing), a by-id read as 404 (§5.3
// "denials do not leak existence"). A store error
// resolving the gate fails CLOSED (denied=true), matching every other taint
// gate error path in this file.
func (s *BeadsServer) subresourceTaintDenied(ctx context.Context, beadID string) (denied bool, err error) {
	taintGate, err := s.resolveBeadTaintGateState(ctx)
	if err != nil {
		return true, err
	}
	if taintGate.exempt {
		return false, nil
	}
	taintIDs, err := s.store.GetTaintsForBead(ctx, beadID)
	if err != nil {
		return true, err
	}
	return !taintGate.allows(taintIDs), nil
}

// gateReferenceRows applies the taint read gate (§5.1) to the bead each
// reference row names, which a row's title, type and status come from. With
// drop, a denied row is removed (a mentioned-by row is the denied bead);
// without, it is blanked to a dangling reference (a mentions target is named
// in the parent's own text, so only what the join added is withheld). It
// returns the rows kept and how many were dropped.
func (s *BeadsServer) gateReferenceRows(ctx context.Context, rows []*model.BeadReferenceSummary, drop bool) ([]*model.BeadReferenceSummary, int, error) {
	gate, err := s.resolveBeadTaintGateState(ctx)
	if err != nil {
		return nil, 0, err
	}
	if gate.exempt {
		return rows, 0, nil
	}
	taintsByBead, err := s.store.GetTaintsForBeads(ctx, referenceSummaryIDs(rows))
	if err != nil {
		return nil, 0, err
	}
	kept := make([]*model.BeadReferenceSummary, 0, len(rows))
	for _, row := range rows {
		switch {
		case gate.allows(taintsByBead[row.ID]):
			kept = append(kept, row)
		case !drop:
			kept = append(kept, &model.BeadReferenceSummary{ID: row.ID, SourceField: row.SourceField, RelationshipType: row.RelationshipType})
		}
	}
	return kept, len(rows) - len(kept), nil
}

// taintIDsSubsetOfGrants reports whether every id in taintIDs is present in
// grants — the literal taintsOf(bead) ⊆ grantsOf(caller) test, factored out
// so a per-bead loop that has already hoisted a single taintGateGrants(ctx)
// call can check each bead in O(1) without re-resolving the caller's grants.
func taintIDsSubsetOfGrants(taintIDs []int64, grants []int64) bool {
	if len(taintIDs) == 0 {
		return true
	}
	granted := make(map[int64]bool, len(grants))
	for _, g := range grants {
		granted[g] = true
	}
	for _, t := range taintIDs {
		if !granted[t] {
			return false
		}
	}
	return true
}
