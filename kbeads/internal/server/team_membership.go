package server

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"time"

	"gardenio/kbeads/internal/model"
)

// teamMembershipCacheKey is one operator resolved with one set of token
// groups: oidc_groups matching makes the groups part of the answer.
type teamMembershipCacheKey struct {
	operatorID int64
	groups     string
}

func newTeamMembershipCacheKey(operatorID int64, groups []string) teamMembershipCacheKey {
	sorted := slices.Clone(groups)
	slices.Sort(sorted)
	return teamMembershipCacheKey{operatorID: operatorID, groups: strings.Join(sorted, "\x00")}
}

// teamMembershipCacheEntry holds one cached resolution.
type teamMembershipCacheEntry struct {
	cachedAt time.Time
	teams    []string
}

// teamMembershipCache memoises a caller's team membership for a bounded TTL,
// per ADR docs/adr/taints.md §8.3: "the set is small and is cached per
// request" — a cache hit performs no store query at all (no type:team list,
// no operator_aliases lookup).
type teamMembershipCache struct {
	ttl time.Duration
	now func() time.Time

	mu      sync.Mutex
	entries map[teamMembershipCacheKey]*teamMembershipCacheEntry

	// loaderCalls counts store queries triggered by a cache miss. For tests.
	loaderCalls int64
}

func newTeamMembershipCache(ttl time.Duration) *teamMembershipCache {
	return &teamMembershipCache{
		ttl:     ttl,
		now:     time.Now,
		entries: make(map[teamMembershipCacheKey]*teamMembershipCacheEntry),
	}
}

// get returns the cached team set for key and true on a live hit.
func (c *teamMembershipCache) get(key teamMembershipCacheKey) ([]string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || c.now().Sub(e.cachedAt) >= c.ttl {
		return nil, false
	}
	return e.teams, true
}

// set installs a freshly-resolved team set for key.
func (c *teamMembershipCache) set(key teamMembershipCacheKey, teams []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = &teamMembershipCacheEntry{cachedAt: c.now(), teams: teams}
}

// LoaderCalls returns the cumulative store-query count. For tests.
func (c *teamMembershipCache) LoaderCalls() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.loaderCalls
}

// initTeamMembershipCache lazily wires the cache on first use.
func (s *BeadsServer) initTeamMembershipCache() {
	if s.teamMembershipCache == nil {
		s.teamMembershipCache = newTeamMembershipCache(identityCacheTTL)
	}
}

// teamFields is the parsed shape of a type:team bead's Fields JSON that
// TeamsOf consults. display/owners/projects are not read here — per ADR
// docs/adr/taints.md §8.1 they are informational and grant nothing by
// themselves.
type teamFields struct {
	Members    []json.RawMessage `json:"members"`
	OIDCGroups []string          `json:"oidc_groups"`
}

// TeamsOf resolves the teams the calling principal belongs to, per ADR
// docs/adr/taints.md §8.1: a team is in the result if the caller's operator
// id is listed in that team's members (after operator_aliases resolution),
// or if the caller's OIDC token carries a group listed in that team's
// oidc_groups.
//
// Agents are never team members: any identity other than an OIDC-
// authenticated human (static token — including an agent bead subject —
// fleet token, edge, or anonymous) resolves to no operator-alias identity
// via operatorAliasForIdentity. Such a caller falls back to the human
// relayed via a sanitized X-Operated-By header, but the relay counts only from
// a bridge fleet subject (relayTrusted, P7 Task 3); the static token's relay
// is attribution only (same trust boundary as decisionAttribution in
// http_decisions.go). With neither, the result is empty without a store query. An agent's effective
// teams for grant resolution come from its spawn-chain root human (§5.1);
// that resolution is the CALLER's responsibility, not TeamsOf's.
//
// The result is cached per resolved operator id for identityCacheTTL; a
// cache hit performs no store query.
func (s *BeadsServer) TeamsOf(ctx context.Context) ([]string, error) {
	return s.teamsOf(ctx, true)
}

// TeamsOfDirect resolves the teams the DIRECTLY authenticated caller belongs
// to — the same membership rule as TeamsOf, but with no fallback to the
// caller-supplied X-Operated-By relay (operatedByHeaderFromContext).
//
// Reserved for gates that use team membership as an ACCESS CONTROL (not
// merely attribution): the relay is a shared static/fleet token's own claim
// about who is behind it, unverified by the server, so honoring it here
// would let any caller holding that token name an arbitrary team member and
// pass the gate — the same class of gap scrubApprover
// (http_scrub_requests.go) was written to close for scrub-request approval,
// and PR #147's team-approver check (handleApproveEscalation) closes here.
// A caller not directly authenticated as OIDC/edge (operatorAliasForIdentity)
// always resolves to no teams via this path.
func (s *BeadsServer) TeamsOfDirect(ctx context.Context) ([]string, error) {
	return s.teamsOf(ctx, false)
}

func (s *BeadsServer) teamsOf(ctx context.Context, allowRelay bool) ([]string, error) {
	id := AuthIdentityFromContext(ctx)
	callerKind, callerSubject := operatorAliasForIdentity(id)
	if callerKind == "" && allowRelay {
		if op := s.trustedRelayOperator(ctx); op != "" {
			callerKind, callerSubject = splitOperator(op)
		}
	}
	if callerKind == "" {
		return nil, nil
	}

	callerOp, err := s.store.LookupOperatorAlias(ctx, callerKind, callerSubject)
	if err != nil || callerOp == 0 {
		// No operator row yet for this identity — nothing to have joined a
		// team with. Not an error for this read path.
		return nil, nil
	}

	return s.teamsForOperator(ctx, callerOp, GroupsFromContext(ctx))
}

// teamsForOperator resolves the teams operatorID belongs to, the same
// membership rule TeamsOf documents (member of `members`, or `oidc_groups`
// intersects groups), factored out so a caller resolving on behalf of
// someone OTHER than the live request identity — grantsOf's agent branch,
// resolving the spawn-chain root human's teams (ADR docs/adr/taints.md
// §5.1) — can supply an explicit operator id instead of one derived from
// ctx. groups is normally GroupsFromContext(ctx) for the live caller; a
// resolved-on-behalf-of operator has no live OIDC token to read groups from,
// so callers resolving for someone else pass nil, matching only via
// `members` (oidc_groups membership cannot be reconstructed after the fact
// from a stored operator id alone).
//
// The result is cached per operator id and groups for identityCacheTTL; a
// cache hit performs no store query.
func (s *BeadsServer) teamsForOperator(ctx context.Context, operatorID int64, groups []string) ([]string, error) {
	s.initTeamMembershipCache()
	key := newTeamMembershipCacheKey(operatorID, groups)
	if cached, ok := s.teamMembershipCache.get(key); ok {
		return cached, nil
	}

	beads, _, err := s.store.ListBeads(ctx, model.BeadFilter{
		Type:  []model.BeadType{model.TypeTeam},
		Kind:  []model.Kind{model.KindConfig},
		Limit: 1000,
	})
	if err != nil {
		return nil, err
	}

	groupSet := make(map[string]bool, len(groups))
	for _, g := range groups {
		groupSet[g] = true
	}

	var teams []string
	for _, b := range beads {
		var f teamFields
		if len(b.Fields) > 0 {
			if err := json.Unmarshal(b.Fields, &f); err != nil {
				continue
			}
		}
		if teamMatchesGroups(f.OIDCGroups, groupSet) || s.teamMatchesMember(ctx, f.Members, operatorID) {
			teams = append(teams, b.ID)
		}
	}

	s.teamMembershipCache.mu.Lock()
	s.teamMembershipCache.loaderCalls++
	s.teamMembershipCache.mu.Unlock()
	s.teamMembershipCache.set(key, teams)
	return teams, nil
}

// teamMembershipFieldKeys are the type:team Fields keys a team's own owners
// may write without gardenio-admin role, per ADR docs/adr/taints.md §9.1's
// viewer team-page acceptance criterion ("membership edit controls...
// functional only for the team's own owners and platform admins, matching
// p1-team's write policy"). Scoped to membership only: a non-admin owner may
// add/remove members and owners, but not rename the team, edit
// display/oidc_groups/projects, or touch any other bead column — those still
// require gardenio-admin via the ordinary TypeConfig.WritePolicy gate.
var teamMembershipFieldKeys = []string{"members", "owners"}

// authorizeTeamMembershipBypass reports whether a WritePolicy-denied update on
// a type:team bead should be let through anyway because the caller is one of
// the team's OWN owners editing only membership (members/owners). Called from
// updateBead when authorizeWriteTitled's min_role check (TypeConfig.WritePolicy,
// gardenio-admin by default) has already denied a non-admin caller — it never
// widens access for any other bead type or any other field, and an admin
// never needs it (authorizeWriteTitled already let them through).
func (s *BeadsServer) authorizeTeamMembershipBypass(ctx context.Context, bead *model.Bead, in *updateBeadInput) bool {
	if bead.Type != model.TypeTeam {
		return false
	}
	if !patchOnlyChangesFields(teamMembershipFieldKeys, in, bead.Fields) {
		return false
	}
	return s.callerIsTeamOwner(ctx, bead)
}

// callerIsTeamOwner reports whether the authenticated caller resolves — via
// the same operator-alias path TeamsOf uses (a real OIDC/edge identity) — to
// an entry in bead's own `owners` field. Mirrors teamsForOperator's
// member-matching exactly, against owners instead of members.
//
// Deliberately does NOT fall back to a relayed X-Operated-By header the way
// TeamsOf does for the viewer's read path. That header is caller-supplied
// and, unlike a read, this identity feeds a WRITE-authorization decision
// (authorizeTeamMembershipBypass): a fleet/static token could set the
// header to name any team's owner and rewrite that team's membership with
// no authenticated tie to that human at all (operator review on PR #145).
// Requiring operatorAliasForIdentity to resolve directly means only a
// caller who themselves authenticated as that OIDC/edge identity — not one
// merely asserting it via a header — can exercise the bypass.
func (s *BeadsServer) callerIsTeamOwner(ctx context.Context, bead *model.Bead) bool {
	id := AuthIdentityFromContext(ctx)
	callerKind, callerSubject := operatorAliasForIdentity(id)
	if callerKind == "" {
		return false
	}
	callerOp, err := s.store.LookupOperatorAlias(ctx, callerKind, callerSubject)
	if err != nil || callerOp == 0 {
		return false
	}

	var f struct {
		Owners []json.RawMessage `json:"owners"`
	}
	if len(bead.Fields) > 0 {
		if err := json.Unmarshal(bead.Fields, &f); err != nil {
			return false
		}
	}
	return s.teamMatchesMember(ctx, f.Owners, callerOp)
}

// validateTeamOwnersNotEmptied rejects a type:team update that removes the
// last entry from `owners`. An owner-less team is unrecoverable through the
// normal viewer/CLI membership-edit path: authorizeTeamMembershipBypass and
// the viewer's own edit gate both key off the caller appearing in `owners`,
// so once it is empty no non-admin caller can ever add an owner back
// (operator review on PR #145, ADR docs/adr/taints.md §9.1).
//
// Only enforced when the patch itself sets the `owners` key — a fields PATCH
// that never mentions owners (e.g. editing `display`) is left alone even if
// the team already carries a legacy empty/absent owners list, matching the
// grandfathering the update path already applies to other structured fields.
func validateTeamOwnersNotEmptied(patchFields, mergedFields json.RawMessage) error {
	var patch map[string]json.RawMessage
	if err := json.Unmarshal(patchFields, &patch); err != nil {
		return nil
	}
	if _, touched := patch["owners"]; !touched {
		return nil
	}

	var merged struct {
		Owners []json.RawMessage `json:"owners"`
	}
	if len(mergedFields) > 0 {
		_ = json.Unmarshal(mergedFields, &merged)
	}
	if len(merged.Owners) == 0 {
		return inputError("owners cannot be emptied: a team must always have at least one owner")
	}
	return nil
}

// teamMatchesGroups reports whether any of a team's oidc_groups is present in
// the caller's token groups.
func teamMatchesGroups(oidcGroups []string, callerGroups map[string]bool) bool {
	for _, g := range oidcGroups {
		if callerGroups[g] {
			return true
		}
	}
	return false
}

// teamMatchesMember reports whether callerOp appears in a team's members
// list, after operator_aliases resolution (ADR §8.1: "members is a list of
// operator ids (or emails, resolved through operator_aliases ... and stored
// as ids)"). Each entry is read as either:
//
//   - a JSON number — the write-time-resolved operators(id) form; compared
//     to callerOp directly, with no store round trip.
//   - a JSON string — an operator alias not yet resolved at write time:
//     "oidc:name@example.com" / "slack:U…" (scheme-prefixed, as splitOperator
//     parses) or a bare email (defaulted to kind "oidc", mirroring
//     operatorAliasForIdentity's own bare-email default). Resolved through
//     LookupOperatorAlias and compared to callerOp.
//
// Either shape resolves to the same operators(id) for the same human, which
// is exactly what lets a Slack alias and an OIDC email in `members` both
// match one member (ka-ORaCUV7C6g acceptance criterion).
func (s *BeadsServer) teamMatchesMember(ctx context.Context, members []json.RawMessage, callerOp int64) bool {
	for _, raw := range members {
		var asNumber int64
		if err := json.Unmarshal(raw, &asNumber); err == nil {
			if asNumber == callerOp {
				return true
			}
			continue
		}
		var asString string
		if err := json.Unmarshal(raw, &asString); err != nil || asString == "" {
			continue
		}
		kind, subject := splitOperator(asString)
		if kind == "" {
			kind, subject = "oidc", asString
		}
		op, err := s.store.LookupOperatorAlias(ctx, kind, subject)
		if err != nil || op == 0 {
			continue
		}
		if op == callerOp {
			return true
		}
	}
	return false
}
