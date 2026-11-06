// The scrub-policy evaluator (docs/adr/taints.md §3, §6.3): first-match rule
// selection over a taint's scrub_policy.rules, shared by every call site that
// needs to know what approval mode a scrub-request resolves to.
//
// §6.3 requires the evaluator to "run at request create [to preview the
// mode] and re-run at approve [to authorize]" and get the SAME answer both
// times given an unchanged policy. The design here makes that true by
// construction rather than by convention: EvaluateScrubPolicy/
// ResolveScrubMode are pure functions of a ScrubRuleContext, and
// scrubRuleContextForRequest builds that context only from data that is
// frozen at create (kind, scope.older_than_days — see
// scrubRequestImmutableFields) or derived from data that never changes after
// it is written (requested_by's own spawn-chain shape). Preview (create) and
// authorize (approve) therefore differ only in WHEN they call the same
// function, never in what it computes.
package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	"gardenio/kbeads/internal/model"
)

// ScrubRuleContext is the resolved predicate input a scrub_policy rule's
// `when` clause is matched against (docs/adr/taints.md §3's worked example:
// kind, older_than_days, requester_is_chain_root, attested). Both call sites
// build one of these from the same underlying scrub-request data — see
// scrubRuleContextForRequest.
type ScrubRuleContext struct {
	// Kind is the request's kind: "detaint" or "purge".
	Kind string
	// OlderThanDays is the request's scope.older_than_days, or nil when the
	// request used the explicit bead_ids scope form. A rule's own
	// older_than_days is a FLOOR ("purge beads at least this old"): it
	// matches when the request's own threshold is at least as strict, i.e.
	// OlderThanDays != nil && *OlderThanDays >= the rule's value.
	OlderThanDays *int
	// RequesterIsChainRoot reports whether the request's requester sits at
	// the root of its own spawn chain (a human, or an agent whose own
	// fields.spawned_by is empty/a sentinel) rather than a nested/relay
	// agent several hops down. See requesterIdentityIsChainRoot.
	RequesterIsChainRoot bool
	// Attested reports whether the request currently carries a non-null
	// attestation (§6.4). Always false at create — attestation is written
	// later, only by the formula's attestation step.
	Attested bool
}

// scrubRuleMatches reports whether every key in when is satisfied by ctx.
// An empty/malformed when never matches. A when clause naming any predicate
// key this evaluator does not recognize ALSO never matches — failing closed
// onto the next rule (or the stricter default) rather than silently
// matching a rule whose author meant to narrow it further than this
// evaluator understands.
func scrubRuleMatches(when json.RawMessage, ctx ScrubRuleContext) bool {
	if len(strings.TrimSpace(string(when))) == 0 {
		return false
	}
	var want map[string]json.RawMessage
	if err := json.Unmarshal(when, &want); err != nil {
		return false
	}
	for key, raw := range want {
		switch key {
		case "kind":
			var v string
			if err := json.Unmarshal(raw, &v); err != nil || v != ctx.Kind {
				return false
			}
		case "older_than_days":
			var v int
			if err := json.Unmarshal(raw, &v); err != nil {
				return false
			}
			if ctx.OlderThanDays == nil || *ctx.OlderThanDays < v {
				return false
			}
		case "requester_is_chain_root":
			var v bool
			if err := json.Unmarshal(raw, &v); err != nil || v != ctx.RequesterIsChainRoot {
				return false
			}
		case "attested":
			var v bool
			if err := json.Unmarshal(raw, &v); err != nil || v != ctx.Attested {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// EvaluateScrubPolicy picks the first rule in policy.Rules whose `when`
// matches ctx — first-match, not most-specific and not last-match
// (docs/adr/taints.md §3). Returns (nil, false) when no rule matches, which
// callers resolve to policy.Default (never "auto" — enforced by
// ValidateScrubPolicy at SetTaint time).
func EvaluateScrubPolicy(policy *model.ScrubPolicy, ctx ScrubRuleContext) (*model.ScrubRule, bool) {
	if policy == nil {
		return nil, false
	}
	for i := range policy.Rules {
		if scrubRuleMatches(policy.Rules[i].When, ctx) {
			return &policy.Rules[i], true
		}
	}
	return nil, false
}

// ResolveScrubMode is the convenience form most callers want: the effective
// mode and approvers after first-match rule selection, falling to
// policy.Default/Approvers when no rule matches (docs/adr/taints.md §3).
//
// This intentionally stops at rule selection: the max_beads/max_per_day cap
// check §6.3 describes ("over cap falls through to the next rule") requires
// counting resolved_bead_ids and the taint's audit history, which belongs to
// the approve/execute path (gd-DyWyWFxnzC), not to rule selection itself.
func ResolveScrubMode(policy *model.ScrubPolicy, ctx ScrubRuleContext) (model.ScrubMode, []string) {
	if rule, ok := EvaluateScrubPolicy(policy, ctx); ok {
		return rule.Mode, rule.Approvers
	}
	if policy == nil {
		return "", nil
	}
	return policy.Default, policy.Approvers
}

// scrubRuleContextForRequest builds the ScrubRuleContext for a scrub-request
// identified by kind/olderThanDays/attested/requestedBy — the same four
// inputs are available, unchanged, at create (from the just-validated create
// body) and at approve (read back off the stored bead's Fields), so the two
// call sites agree by construction.
func (s *BeadsServer) scrubRuleContextForRequest(ctx context.Context, kind string, olderThanDays *int, attested bool, requestedBy string) ScrubRuleContext {
	return ScrubRuleContext{
		Kind:                 kind,
		OlderThanDays:        olderThanDays,
		Attested:             attested,
		RequesterIsChainRoot: s.requesterIdentityIsChainRoot(ctx, requestedBy),
	}
}

// requesterIdentityIsChainRoot reports whether requestedBy — a scrub-
// request's requested_by, server-stamped from actorFromContext (the raw
// AuthIdentity.Subject: an OIDC email, an agent bead id, "static", or a
// fleet name) — names a principal at the root of its own spawn chain.
//
// A human subject (contains "@") is trivially the root: humans have no
// spawn chain. An agent bead id is the root when its own
// fields.spawned_by is empty or a sentinel (scheduler:/trigger:/system:/
// fleet:/escalation-bridge — isSentinelSpawner); it is NOT the root when
// spawned_by names another agent, i.e. a nested/relay agent filed the
// request on the root's behalf. This mirrors resolveSpawnChainRoot's
// one-hop spawner check without walking further up the chain — "is THIS
// principal the root", not "who is the human at the top".
//
// Deliberately a function of stored data only (requestedBy plus that
// agent's own bead, both immutable after create/spawn), never of the LIVE
// caller's auth context — so re-deriving it at approve time, possibly by a
// different caller, agrees with the value implicitly used to preview at
// create.
func (s *BeadsServer) requesterIdentityIsChainRoot(ctx context.Context, requestedBy string) bool {
	if requestedBy == "" {
		return false
	}
	if strings.Contains(requestedBy, "@") {
		return true
	}
	if s == nil || s.store == nil {
		return false
	}
	bead, err := s.store.GetBead(ctx, requestedBy)
	if err != nil || bead == nil {
		return false
	}
	spawner := agentSpawnedByField(bead)
	return spawner == "" || isSentinelSpawner(spawner)
}

// logScrubRequestPolicyPreview runs the evaluator at create time to PREVIEW
// the mode the request would resolve to if approval ran right now
// (docs/adr/taints.md §6.3). This is informational only: type=scrub-request
// deliberately carries no mode/preview field (mirroring the ADR's "no
// detaint_mode field on the request" rule — the requester never gets to
// stamp an approval outcome, even a non-binding one), so the result is
// logged, not persisted. The authoritative call happens later, at approve,
// re-running the same evaluator over the (by then possibly attested)
// request.
func (s *BeadsServer) logScrubRequestPolicyPreview(ctx context.Context, taint *model.Taint, kind string, rawScope json.RawMessage, requestedBy string) {
	if taint == nil {
		return
	}
	var policy model.ScrubPolicy
	if err := json.Unmarshal(taint.ScrubPolicy, &policy); err != nil {
		slog.Warn("scrub_policy_preview_unparseable", "taint", taint.Name, "error", err)
		return
	}
	ruleCtx := s.scrubRuleContextForRequest(ctx, kind, scrubRequestScopeOlderThanDays(rawScope), false, requestedBy)
	mode, _ := ResolveScrubMode(&policy, ruleCtx)
	slog.Info("scrub_policy_preview",
		"taint", taint.Name,
		"kind", kind,
		"requester_is_chain_root", ruleCtx.RequesterIsChainRoot,
		"preview_mode", string(mode),
	)
}

// scrubRequestScopeOlderThanDays extracts scope.older_than_days from a
// scrub-request's raw scope JSON, returning nil when the scope used the
// explicit bead_ids form or failed to parse (resolveScrubRequestScope has
// already validated a well-formed scope by the time this runs; a parse
// failure here just means "no threshold to match on", never a hard error —
// this is a best-effort preview, not the authoritative scope resolution).
func scrubRequestScopeOlderThanDays(rawScope json.RawMessage) *int {
	var scope scrubRequestScope
	if err := json.Unmarshal(rawScope, &scope); err != nil {
		return nil
	}
	return scope.OlderThanDays
}
