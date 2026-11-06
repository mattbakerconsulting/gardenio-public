// Server-side spawn-policy evaluator: auto-approval rules for privileged-role
// agent spawns (gd-vmLhgqydEa, design doc gd-sGW5Y1B8sg §4, epic
// gd-qbBJGHiGHo phase 2).
//
// PROBLEM: spawn_authz.go denies a non-human caller's privileged-role agent
// spawn as escalatable (authorizeNonHumanSpawn); handleEscalation
// (write_authz.go) turns that into a type:escalation bead carrying
// spawn_role, and today EVERY one of those sits until a human calls
// POST /v1/escalations/{id}/approve. For a privileged role a project spawns
// routinely and safely under a known cap (the gc-dogfood k8s-live crew being
// the first case), that is pure operator toil with zero judgment being
// exercised — the same cap-check a human would perform every time.
//
// FIX: a `type:spawn_policy` config bead, one per privileged role, declares
// first-match rules keyed on {project, namespace, requester_role}. Each rule
// is either "auto" (bounded by required max_per_day/max_concurrent caps) or
// "human" (an explicit "still needs a person" declaration, optionally naming
// the approvers who alone may approve it; see spawnApprovalDenial). The evaluator below runs synchronously right
// after a matching escalation is created (write_authz.go handleEscalation)
// and again on a periodic sweep — the sweep is what lets a request that was
// over-cap on arrival get re-examined once the day rolls over or a
// concurrent spawn closes.
//
// A role with NO spawn_policy bead is left completely untouched — opt-in per
// role, mirroring decision_aging.go's opt-in-per-decision philosophy: this
// evaluator only ever narrows what already required a human, never widens
// it. When a policy bead DOES exist for the role, first-match wins:
//   - an "auto" rule that matches and is within its caps auto-approves,
//     stamping approved_by=system:spawn-policy / approval_method=policy /
//     policy_rule=<role>#<index> and replaying the spawn exactly as an
//     operator approval would (replayEscalation, http_escalations.go —
//     which already treats a "system:"-prefixed approved_by as
//     spawn_approval_mode "auto" via spawnApprovalMode).
//   - an "auto" rule that matches but is OVER cap does not consume the
//     match: evaluation falls through to the next rule.
//   - a "human" rule that matches stops evaluation and leaves the escalation
//     exactly as-is for an operator.
//   - if every rule is exhausted with no acting match, the escalation is
//     denied and closed — the policy bead's existence is itself a statement
//     that this role's spawns are enumerated, so an unenumerated request is
//     refused rather than silently left pending.
//
// Ships dark behind BEADS_SPAWN_POLICY_ENABLED (default OFF), mirroring
// decision_aging.go.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// spawnPolicyActor is the responder/approver recorded on an auto-approved or
// auto-denied spawn escalation. Distinct non-human sentinel, mirroring
// decisionAgingActor — and the strings.HasPrefix(approvedBy, "system:") check
// in spawnApprovalMode (http_escalations.go) already special-cases anything
// under this prefix as spawn_approval_mode "auto".
const spawnPolicyActor = "system:spawn-policy"

// spawnPolicyAdvisoryLockKey serializes the sweep across replicas, mirroring
// decisionAgingAdvisoryLockKey. Postgres advisory locks share one 64-bit
// keyspace per database — this constant must stay distinct from every other
// TryAdvisoryLock key in the daemon (decisionAgingAdvisoryLockKey included);
// TestAdvisoryLockKeysAreUnique enforces it.
const spawnPolicyAdvisoryLockKey int64 = 0x6265_6164_0004

// neverAutoApproveRoles are privileged roles whose spawns a human must approve
// every time (plan gd-xiV5hyhPrG P6, gd-sAMar9tn5B). infra-admin binds
// cluster-admin. This is a code-level set, not policy-bead data, so no bead
// write (a hand-edited spawn_policy included) can add an auto path for them.
// Enforced in two places: validateSpawnPolicyFields refuses an auto rule at
// write time, and evaluateSpawnEscalation never acts on these roles. Approval
// itself needs an OIDC human through an approval-capable client
// (escalationApprover). handleEscalation does NOT stamp requires_four_eyes on
// these escalations: a single-human org can never satisfy it (gd-TXbrvA9E6F).
var neverAutoApproveRoles = map[string]struct{}{
	"infra-admin": {},
}

// isNeverAutoApproveRole reports whether ANY element of role (parseRoleList)
// is a never-auto role, so "crew,infra-admin" is held to the same human-only
// path as "infra-admin" (gd-Yhbc5wdjwj).
func isNeverAutoApproveRole(role string) bool {
	for _, r := range parseRoleList(role) {
		if _, ok := neverAutoApproveRoles[r]; ok {
			return true
		}
	}
	return false
}

// spawnPolicySweepCap bounds the open-escalation working set fetched per
// sweep tick, mirroring decisionAgingSweepCap's OOM guard.
const spawnPolicySweepCap = 500

// spawnPolicySweepInterval is deliberately much shorter than
// decision_aging's 24h: the acceptance bar (gd-vmLhgqydEa) is "auto-approves
// within one tick", and unlike stale-decision aging this sweep is also the
// retry path for a request that was over-cap on arrival.
const spawnPolicySweepInterval = 30 * time.Second

// spawnPolicyEnabled reports whether the evaluator is active. Default OFF —
// ships dark until a rollout opts in, mirroring decisionAgingEnabled.
func spawnPolicyEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("BEADS_SPAWN_POLICY_ENABLED"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// SpawnPolicyEnabled is the exported gate check for the serve entrypoint,
// which lives in package main and cannot see the unexported helper.
func SpawnPolicyEnabled() bool { return spawnPolicyEnabled() }

// StartSpawnPolicySweep launches the background sweep goroutine. No-op path
// (including the flag re-check) mirrors StartDecisionAging.
func (s *BeadsServer) StartSpawnPolicySweep(ctx context.Context) {
	go s.runSpawnPolicySweep(ctx, spawnPolicySweepInterval)
}

func (s *BeadsServer) runSpawnPolicySweep(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	s.sweepPendingSpawnEscalations(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sweepPendingSpawnEscalations(ctx)
		}
	}
}

// sweepPendingSpawnEscalations re-evaluates every open, not-yet-resolved
// privileged-spawn escalation. Serialized across replicas by
// spawnPolicyAdvisoryLockKey, mirroring sweepStaleDecisions: a replica that
// loses the lock race skips the tick entirely and tries again next tick.
func (s *BeadsServer) sweepPendingSpawnEscalations(ctx context.Context) {
	if !spawnPolicyEnabled() {
		return
	}
	acquired, err := s.store.TryAdvisoryLock(ctx, spawnPolicyAdvisoryLockKey, s.sweepPendingSpawnEscalationsLocked)
	if err != nil {
		slog.Warn("spawn policy: advisory lock error", "error", err)
		return
	}
	if !acquired {
		slog.Info("spawn policy: skipping tick, another replica holds the sweep lock")
	}
}

// sweepPendingSpawnEscalationsLocked is the sweep body, run only while
// spawnPolicyAdvisoryLockKey is held by this replica. Errors are logged
// internally (best-effort, retried next tick) so this always returns nil.
func (s *BeadsServer) sweepPendingSpawnEscalationsLocked(ctx context.Context) error {
	beads, _, err := s.store.ListBeads(ctx, model.BeadFilter{
		Type:   []model.BeadType{model.TypeEscalation},
		Status: []model.Status{model.StatusOpen},
		Limit:  spawnPolicySweepCap,
		Sort:   "created_at",
	})
	if err != nil {
		slog.Warn("spawn policy: failed to list open escalations", "error", err)
		return nil
	}
	if len(beads) == spawnPolicySweepCap {
		slog.Warn("spawn policy: sweep hit the working-set cap; remainder deferred to next sweep",
			"cap", spawnPolicySweepCap)
	}

	candidates := 0
	for _, b := range beads {
		if !isPendingSpawnEscalation(b) {
			continue
		}
		candidates++
		s.evaluateSpawnEscalation(ctx, b)
	}
	slog.Info("spawn policy: sweep complete", "considered", len(beads), "spawn_candidates", candidates)
	return nil
}

// spawnEscalationFields is the subset of an escalation bead's fields the
// evaluator reads. Mirrors the anonymous struct replayEscalation decodes.
type spawnEscalationFields struct {
	OperationType   string          `json:"operation_type"`
	OperationSpec   json.RawMessage `json:"operation_spec"`
	RequestingAgent string          `json:"requesting_agent"`
	SpawnRole       string          `json:"spawn_role"`
	ApprovedBy      string          `json:"approved_by"`
}

func parseSpawnEscalationFields(b *model.Bead) spawnEscalationFields {
	var f spawnEscalationFields
	if b == nil || len(b.Fields) == 0 {
		return f
	}
	_ = json.Unmarshal(b.Fields, &f)
	return f
}

// isPendingSpawnEscalation reports whether b is an open, unresolved
// privileged-agent-spawn escalation — the only shape this evaluator acts on.
func isPendingSpawnEscalation(b *model.Bead) bool {
	if b == nil || b.Type != model.TypeEscalation || b.Status != model.StatusOpen {
		return false
	}
	f := parseSpawnEscalationFields(b)
	return f.OperationType == "create" && f.SpawnRole != "" && f.ApprovedBy == ""
}

// spawnPolicyMatchContext is the {project, namespace, requester_role} triple
// a spawn_policy rule's `when` clause matches against (gd-vmLhgqydEa).
type spawnPolicyMatchContext struct {
	Project       string
	Namespace     string
	RequesterRole string
}

// spawnPolicyRuleWhen names the match keys for one rule. An empty/absent key
// is a wildcard for that dimension.
type spawnPolicyRuleWhen struct {
	Project       string `json:"project,omitempty"`
	Namespace     string `json:"namespace,omitempty"`
	RequesterRole string `json:"requester_role,omitempty"`
}

func (w spawnPolicyRuleWhen) matches(ctx spawnPolicyMatchContext) bool {
	if w.Project != "" && w.Project != ctx.Project {
		return false
	}
	if w.Namespace != "" && w.Namespace != ctx.Namespace {
		return false
	}
	if w.RequesterRole != "" && w.RequesterRole != ctx.RequesterRole {
		return false
	}
	return true
}

// spawnPolicyRule is one first-match rule on a type:spawn_policy bead.
// MaxPerDay/MaxConcurrent are required (and validated at write time,
// validateSpawnPolicyFields) whenever Mode is "auto"; Approvers on a "human"
// rule names who may approve — the
// escalation still waits for an actual approve call, which only a listed
// approver may make (enforced in handleApproveEscalation).
type spawnPolicyRule struct {
	When          spawnPolicyRuleWhen `json:"when"`
	Mode          string              `json:"mode"`
	MaxPerDay     int                 `json:"max_per_day,omitempty"`
	MaxConcurrent int                 `json:"max_concurrent,omitempty"`
	Approvers     []string            `json:"approvers,omitempty"`
	// FourEyes (human-mode rules only, default false) makes handleEscalation
	// stamp requires_four_eyes on a matching escalation, so the requester's
	// own operator cannot approve it. validateSpawnPolicyFields refuses it
	// unless the rule lists at least two distinct approvers, so the stamp is
	// always satisfiable (gd-kvrZdjItZV).
	FourEyes bool `json:"four_eyes,omitempty"`
}

// spawnPolicyConfig is the decoded shape of a type:spawn_policy bead's
// fields blob.
type spawnPolicyConfig struct {
	Role  string            `json:"role"`
	Rules []spawnPolicyRule `json:"rules"`
}

func parseSpawnPolicyRules(fields json.RawMessage) ([]spawnPolicyRule, error) {
	if len(fields) == 0 {
		return nil, nil
	}
	var cfg spawnPolicyConfig
	if err := json.Unmarshal(fields, &cfg); err != nil {
		return nil, err
	}
	return cfg.Rules, nil
}

// lookupSpawnPolicy finds the type:spawn_policy bead for role, keyed by its
// `role` field — mirrors lookupSpawnPermissions' email-keyed lookup of a
// type:user bead. Returns (nil, nil) when no policy bead exists for role.
func (s *BeadsServer) lookupSpawnPolicy(ctx context.Context, role string) (*model.Bead, error) {
	if role == "" {
		return nil, nil
	}
	beads, _, err := s.store.ListBeads(ctx, model.BeadFilter{
		Type:   []model.BeadType{"spawn_policy"},
		Kind:   []model.Kind{"config"},
		Status: []model.Status{model.StatusOpen},
		Fields: map[string]string{"role": role},
		Limit:  1,
	})
	if err != nil {
		return nil, err
	}
	if len(beads) == 0 {
		return nil, nil
	}
	return beads[0], nil
}

// resolveRequesterContext resolves the requester_role and namespace match
// keys from the REQUESTING agent's own type:agent bead (found the same way
// http_graph.go enriches agent nodes: Fields{"agent": name}).
//
// requestingAgent must be the escalation bead's CreatedBy, never its
// fields.requesting_agent: CreatedBy is derived server-side from the
// authenticated caller for every creation path (beads.go's AUDIT §C3 choke
// point) and is immutable, whereas requesting_agent is only guaranteed
// server-stamped for a static-token agent's own escalation
// (escalation_requesting_agent.go) — an OIDC/fleet caller's
// requesting_agent is left as client-supplied and can name an arbitrary
// agent to borrow its role/namespace match keys.
//
// The namespace match key deliberately reads the requesting agent's own
// pod_namespace, not any namespace of the agent being spawned: at
// escalation-creation time the target agent does not exist yet, so it has no
// namespace of its own. The rule is "where the requesting work already
// runs" — e.g. a gardener running in the gc-dogfood namespace requesting a
// privileged k8s-live spawn for that same namespace.
func (s *BeadsServer) resolveRequesterContext(ctx context.Context, requestingAgent string) (requesterRole, namespace string) {
	if requestingAgent == "" {
		return "", ""
	}
	beads, _, err := s.store.ListBeads(ctx, model.BeadFilter{
		Type:   []model.BeadType{"agent"},
		Fields: map[string]string{"agent": requestingAgent},
		Limit:  1,
	})
	if err != nil || len(beads) == 0 {
		return "", ""
	}
	var f struct {
		Role         string `json:"role"`
		PodNamespace string `json:"pod_namespace"`
	}
	if len(beads[0].Fields) > 0 {
		_ = json.Unmarshal(beads[0].Fields, &f)
	}
	return f.Role, f.PodNamespace
}

// evaluateSpawnEscalationSynchronous is the create-time entry point called
// from write_authz.go's handleEscalation, giving the evaluator a same-request
// look at a freshly created spawn escalation instead of waiting for the next
// sweep tick. Takes spawnPolicyAdvisoryLockKey — the same lock
// sweepPendingSpawnEscalations serializes on — so a create-time evaluation
// can never race a concurrent sweep tick (or another concurrent create) over
// the same cap count: without this, two escalations landing close together
// could each observe "still under max_concurrent" via
// spawnPolicyWithinCaps' read-then-act check and both auto-approve,
// overshooting the declared cap. Losing the lock race is not an error — the
// escalation is simply left for the next sweep tick to pick up, mirroring
// sweepPendingSpawnEscalations' own skip-on-contention behavior.
func (s *BeadsServer) evaluateSpawnEscalationSynchronous(ctx context.Context, bead *model.Bead) {
	if !spawnPolicyEnabled() {
		return
	}
	acquired, err := s.store.TryAdvisoryLock(ctx, spawnPolicyAdvisoryLockKey, func(lockedCtx context.Context) error {
		s.evaluateSpawnEscalation(lockedCtx, bead)
		return nil
	})
	if err != nil {
		slog.Warn("spawn policy: advisory lock error on create-time evaluation", "escalation", bead.ID, "error", err)
		return
	}
	if !acquired {
		slog.Info("spawn policy: create-time evaluation skipped, sweep lock held elsewhere; next sweep tick will pick it up",
			"escalation", bead.ID)
	}
}

// evaluateSpawnEscalation is the system:spawn-policy evaluator body. Called,
// while holding spawnPolicyAdvisoryLockKey, from
// evaluateSpawnEscalationSynchronous (create-time) and
// sweepPendingSpawnEscalationsLocked (sweep tick) — never call this directly
// from a new call site without holding that lock first. Best-effort: every
// failure path logs and leaves the escalation for a human rather than risk a
// wrong auto-approval.
func (s *BeadsServer) evaluateSpawnEscalation(ctx context.Context, bead *model.Bead) {
	if !spawnPolicyEnabled() {
		return
	}
	if !isPendingSpawnEscalation(bead) {
		return
	}
	// Four-eyes (escalation_approval_authz.go) demands an approver distinct
	// from the requester; a system auto-approval cannot satisfy that intent,
	// so any spawn escalation explicitly flagged requires_four_eyes is always
	// left for a human, regardless of what the policy would otherwise do.
	// handleEscalation does not set the flag for never-auto roles; it is for
	// explicit, policy-driven use.
	if escalationRequiresFourEyes(bead) {
		return
	}

	f := parseSpawnEscalationFields(bead)

	// Behind validateSpawnPolicyFields, this is the runtime stop for a
	// never-auto role: it is left untouched for a human even if an auto rule
	// reached the store some other way (a direct SQL write, a rule that
	// predates the guard). Deliberately also skips the
	// "no rule matched, deny and close" path below: nothing here may act.
	if isNeverAutoApproveRole(f.SpawnRole) {
		slog.Info("spawn policy: never-auto role left for a human", "role", f.SpawnRole, "escalation", bead.ID)
		return
	}

	// A spawn_policy bead is keyed by ONE role. A comma-joined role list has
	// no single policy that speaks for all its elements, so it is always left
	// for a human, and a single role is looked up by its parsed (trimmed)
	// name, the form the policy bead's `role` field holds (gd-Yhbc5wdjwj).
	roles := parseRoleList(f.SpawnRole)
	if len(roles) != 1 {
		slog.Info("spawn policy: multi-role spawn left for a human", "role", f.SpawnRole, "escalation", bead.ID)
		return
	}

	policy, err := s.lookupSpawnPolicy(ctx, roles[0])
	if err != nil {
		slog.Warn("spawn policy: failed to look up policy bead", "role", f.SpawnRole, "escalation", bead.ID, "error", err)
		return
	}
	if policy == nil {
		// No declared policy for this role: opt-in model, leave untouched
		// for a human — absence of config is not itself a denial.
		return
	}

	rules, err := parseSpawnPolicyRules(policy.Fields)
	if err != nil {
		slog.Warn("spawn policy: malformed rules on policy bead", "policy_bead", policy.ID, "role", f.SpawnRole, "error", err)
		return
	}

	// The escalation's operation_spec is client-supplied (mirrored verbatim
	// from the original denied request body, write_authz.go handleEscalation)
	// and is what replayEscalation actually replays on auto-approval. Without
	// this check a requester whose spawn_role matches a wildcard auto rule
	// could get an arbitrary create replayed — e.g. operation_spec
	// {Type:"team", members:[attacker]} — auto-approved and stamped
	// approved_by=system:spawn-policy despite spawn_role never describing a
	// "team" create at all. Only an "agent" create whose own role field
	// matches the escalation's spawn_role is a spawn this evaluator may act
	// on; anything else is left for a human.
	var spec createBeadInput
	if err := json.Unmarshal(f.OperationSpec, &spec); err != nil ||
		spec.Type != "agent" || roleFromFields(spec.Fields) != f.SpawnRole {
		return
	}
	project := projectFromFields(spec.Fields)
	requesterRole, namespace := s.resolveRequesterContext(ctx, bead.CreatedBy)

	matchCtx := spawnPolicyMatchContext{
		Project:       project,
		Namespace:     namespace,
		RequesterRole: requesterRole,
	}

	for i, rule := range rules {
		if !rule.When.matches(matchCtx) {
			continue
		}
		switch rule.Mode {
		case "human":
			// Explicit match on a human-mode rule: stop evaluating, leave
			// the escalation exactly as it is for an operator.
			slog.Info("spawn policy: matched human-mode rule, leaving for operator",
				"escalation", bead.ID, "role", f.SpawnRole, "rule_index", i)
			return
		case "auto":
			within, err := s.spawnPolicyWithinCaps(ctx, roles[0], project, rule)
			if err != nil {
				slog.Warn("spawn policy: cap check failed, leaving for operator",
					"escalation", bead.ID, "role", f.SpawnRole, "rule_index", i, "error", err)
				return
			}
			if !within {
				// Over cap: this rule does not consume the match — keep
				// looking at subsequent rules (e.g. a human catch-all).
				slog.Info("spawn policy: rule matched but is over cap, falling through",
					"escalation", bead.ID, "role", f.SpawnRole, "rule_index", i)
				continue
			}
			s.autoApproveSpawnEscalation(ctx, bead, spawnPolicyRuleLabel(roles[0], i))
			return
		default:
			slog.Warn("spawn policy: rule has unknown mode, skipping",
				"role", f.SpawnRole, "rule_index", i, "mode", rule.Mode)
		}
	}

	// Rules exhausted with no acting match (no human-mode match, no
	// within-cap auto match): the policy bead's existence is itself a
	// statement that this role's spawns are enumerated, so an unenumerated
	// request is denied rather than left silently pending
	// (gd-vmLhgqydEa acceptance: "no match denies").
	s.denySpawnEscalation(ctx, bead, "no spawn_policy rule matched this request")
}

// spawnPolicyRuleLabel builds the policy_rule provenance string stamped on
// an auto-approved escalation.
func spawnPolicyRuleLabel(role string, ruleIndex int) string {
	return fmt.Sprintf("%s#%d", role, ruleIndex)
}

// spawnPolicyWithinCaps reports whether auto-approving under rule would stay
// within its declared max_concurrent and max_per_day budgets. Both counts are
// scoped to (role, project) — spawned agent beads carrying
// spawn_approval_mode=="auto" — so gc-dogfood's budget is independent of any
// other project sharing the same rule. validateSpawnPolicyFields guarantees
// every auto-mode rule declares both caps as positive integers, so both are
// checked unconditionally.
func (s *BeadsServer) spawnPolicyWithinCaps(ctx context.Context, role, project string, rule spawnPolicyRule) (bool, error) {
	fields := map[string]string{"role": role, "spawn_approval_mode": "auto"}
	if project != "" {
		fields["project"] = project
	}

	_, concurrentTotal, err := s.store.ListBeads(ctx, model.BeadFilter{
		Type:   []model.BeadType{"agent"},
		Status: activeAgentStatuses,
		Fields: fields,
		Limit:  1,
	})
	if err != nil {
		return false, err
	}
	if concurrentTotal >= rule.MaxConcurrent {
		return false, nil
	}

	startOfDay := time.Now().UTC().Truncate(24 * time.Hour)
	_, dailyTotal, err := s.store.ListBeads(ctx, model.BeadFilter{
		Type:         []model.BeadType{"agent"},
		Fields:       fields,
		CreatedAfter: &startOfDay,
		Limit:        1,
	})
	if err != nil {
		return false, err
	}
	if dailyTotal >= rule.MaxPerDay {
		return false, nil
	}

	return true, nil
}

// autoApproveSpawnEscalation stamps the same approval fields
// handleApproveEscalation records for a human approval, transitions the
// escalation to in_progress, and replays the denied spawn — mirroring the
// human approve path exactly except for the actor (system:spawn-policy) and
// the approval_method/policy_rule provenance. replayEscalation
// (http_escalations.go) already treats a "system:"-prefixed approved_by as
// spawn_approval_mode "auto" (spawnApprovalMode) and closes the escalation
// itself on a successful replay, so no further bookkeeping happens here.
func (s *BeadsServer) autoApproveSpawnEscalation(ctx context.Context, bead *model.Bead, ruleLabel string) {
	// P7 Task 12: the same one-executor claim as a human approve, before any
	// write. A spawn is a server replay; anything else is left for a human.
	if d := escalationDispatchOf(bead); d != dispatchServer {
		slog.Warn("spawn policy: not auto-approving a non-server dispatch", "escalation", bead.ID, "dispatch", d)
		return
	}
	dispatch, proceed, err := s.claimEscalationDispatch(ctx, bead)
	if err != nil || !proceed {
		slog.Warn("spawn policy: not replaying", "escalation", bead.ID, "dispatch", dispatch, "proceed", proceed, "error", err)
		return
	}
	extra := map[string]any{
		"approved_by":     spawnPolicyActor,
		"approved_at":     time.Now().UTC().Format(time.RFC3339),
		"approval_method": "policy",
		"policy_rule":     ruleLabel,
	}
	s.addSpawnPermissionSnapshot(ctx, bead, extra)
	if err := s.mergeEscalationFieldsWithHistory(ctx, bead, spawnPolicyActor, extra); err != nil {
		slog.Warn("spawn policy: failed to merge approval fields", "escalation", bead.ID, "error", err)
		return
	}

	// Re-fetch to pick up the merged approval fields BEFORE the status
	// transition below — mergeBeadFields wrote a fresh fields blob via its
	// own GetBead+UpdateBead round trip that the stale `bead` pointer never
	// saw, so reusing `bead` for the next UpdateBead call would clobber the
	// merge. Mirrors handleApproveEscalation's identical re-fetch-before-
	// transition ordering.
	refreshed, err := s.store.GetBead(ctx, bead.ID)
	if err != nil || refreshed == nil {
		refreshed = bead
	}

	if refreshed.Status == model.StatusOpen {
		refreshed.Status = model.StatusInProgress
		if err := s.store.UpdateBead(ctx, refreshed); err != nil {
			slog.Warn("spawn policy: failed to transition escalation to in_progress", "escalation", bead.ID, "error", err)
			return
		}
		s.recordAndPublish(ctx, events.TopicBeadUpdated, refreshed.ID, spawnPolicyActor, events.BeadUpdated{
			Bead:    refreshed,
			Changes: map[string]any{"status": string(model.StatusInProgress), "approved_by": spawnPolicyActor},
		})
	}

	result := s.replayEscalation(ctx, refreshed, spawnPolicyActor, nil)
	slog.Info("spawn policy: auto-approved spawn escalation",
		"escalation", bead.ID, "rule", ruleLabel, "replay", result)
}

// denySpawnEscalation closes a spawn escalation as denied when a
// type:spawn_policy bead exists for the role but no rule in it applies to
// this request. Mirrors handleRejectEscalation's merge+close+audit sequence
// with the system actor in place of an operator identity.
func (s *BeadsServer) denySpawnEscalation(ctx context.Context, bead *model.Bead, reason string) {
	extra := map[string]any{
		"result_status": "failure",
		"result":        map[string]string{"error": "denied", "reason": reason},
	}
	if err := s.mergeEscalationFieldsWithHistory(ctx, bead, spawnPolicyActor, extra); err != nil {
		slog.Warn("spawn policy: failed to merge denial fields", "escalation", bead.ID, "error", err)
	}

	s.auditEscalationRejection(ctx, bead.ID, spawnPolicyActor, reason)

	closed, err := s.store.CloseBead(ctx, bead.ID, spawnPolicyActor)
	if err != nil || closed == nil {
		slog.Warn("spawn policy: failed to close denied escalation", "escalation", bead.ID, "error", err)
		return
	}
	s.recordAndPublish(ctx, events.TopicBeadClosed, closed.ID, spawnPolicyActor, events.BeadClosed{
		Bead:     closed,
		ClosedBy: spawnPolicyActor,
	})
	slog.Info("spawn policy: denied spawn escalation, no rule matched", "escalation", bead.ID, "reason", reason)
}

// validateSpawnPolicyFields validates a type:spawn_policy bead's `rules` at
// write time (gd-vmLhgqydEa acceptance: "rule without caps refused at
// write"). An auto-mode rule with no positive max_per_day/max_concurrent
// would auto-approve an unbounded number of privileged spawns, so — unlike
// the soft-warning tier in project_role_validation.go — this is always a
// hard reject.
func validateSpawnPolicyFields(fields json.RawMessage) string {
	var cfg spawnPolicyConfig
	if len(fields) > 0 {
		if err := json.Unmarshal(fields, &cfg); err != nil {
			return fmt.Sprintf("spawn_policy rules field is malformed JSON: %v", err)
		}
	}
	for i, rule := range cfg.Rules {
		switch rule.Mode {
		case "auto":
			if rule.FourEyes {
				return fmt.Sprintf(
					"spawn_policy rule %d sets four_eyes on an auto rule: four_eyes applies only to mode \"human\"", i)
			}
			if isNeverAutoApproveRole(cfg.Role) {
				return fmt.Sprintf(
					"spawn_policy rule %d has mode \"auto\" but role %q is never auto-approved: every spawn needs a human (use mode \"human\")",
					i, cfg.Role)
			}
			if rule.MaxPerDay <= 0 || rule.MaxConcurrent <= 0 {
				return fmt.Sprintf(
					"spawn_policy rule %d has mode \"auto\" but does not declare positive max_per_day and max_concurrent — an auto rule must cap itself",
					i)
			}
		case "human":
			// No caps required — an explicit "still needs a person" rule.
			if rule.FourEyes && len(distinctSpawnPolicyApprovers(rule.Approvers)) < 2 {
				return fmt.Sprintf(
					"spawn_policy rule %d sets four_eyes but lists fewer than 2 distinct approvers: a four-eyes rule needs a second human who is not the requester",
					i)
			}
		case "":
			return fmt.Sprintf("spawn_policy rule %d is missing a mode (must be \"auto\" or \"human\")", i)
		default:
			return fmt.Sprintf("spawn_policy rule %d has unknown mode %q (must be \"auto\" or \"human\")", i, rule.Mode)
		}
	}
	return ""
}

// spawnPolicyApprover is one human approver identity from a rule's approvers
// list, split the way splitOperator splits an operator string.
type spawnPolicyApprover struct{ Kind, Subject string }

// distinctSpawnPolicyApprovers returns the distinct human approvers in
// approvers (a bare entry is an OIDC email), deduplicated case-insensitively.
// Subject case is preserved because operator-alias lookup is case-sensitive
// (Slack ids). "team:" entries name a group, not a human, so they never count
// toward the two-approver minimum of a four_eyes rule.
func distinctSpawnPolicyApprovers(approvers []string) []spawnPolicyApprover {
	seen := map[string]struct{}{}
	var out []spawnPolicyApprover
	for _, a := range approvers {
		a = strings.TrimSpace(a)
		kind, subject := splitOperator(a)
		if kind == "" {
			kind, subject = "oidc", a
		}
		if kind == "team" || subject == "" {
			continue
		}
		key := strings.ToLower(kind + ":" + subject)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, spawnPolicyApprover{Kind: kind, Subject: subject})
	}
	return out
}

// validateSpawnPolicy is the write-time gate for a type:spawn_policy bead:
// validateSpawnPolicyFields plus, for every four_eyes rule, a check that its
// approvers resolve to at least two DISTINCT operators. Two aliases linked to
// one operator row (e.g. a Slack id and an OIDC email of the same person) are
// one human, so they would still leave the rule unsatisfiable. An alias with no
// operator row counts as its own identity. Failing here, at write, means
// approval time never meets a four-eyes stamp nobody else can satisfy.
func (s *BeadsServer) validateSpawnPolicy(ctx context.Context, fields json.RawMessage) string {
	if reason := validateSpawnPolicyFields(fields); reason != "" {
		return reason
	}
	rules, err := parseSpawnPolicyRules(fields)
	if err != nil {
		return ""
	}
	for i, rule := range rules {
		if !rule.FourEyes {
			continue
		}
		operators := map[string]struct{}{}
		for _, a := range distinctSpawnPolicyApprovers(rule.Approvers) {
			id, err := s.store.LookupOperatorAlias(ctx, a.Kind, a.Subject)
			if err == nil && id != 0 {
				operators[fmt.Sprintf("op:%d", id)] = struct{}{}
			} else {
				operators[strings.ToLower(a.Kind+":"+a.Subject)] = struct{}{}
			}
		}
		if len(operators) < 2 {
			return fmt.Sprintf(
				"spawn_policy rule %d sets four_eyes but its approvers resolve to fewer than 2 distinct operators", i)
		}
	}
	return ""
}

// matchingHumanSpawnRule returns the first-matching rule for a spawn request
// when that rule is human-mode, or nil. It mirrors the rule selection of
// evaluateSpawnEscalation (single role, agent create whose role equals
// spawnRole, first matching `when`), so what is stamped on the escalation
// follows the rule that would govern it. Anything unresolvable returns nil.
// requesterSubject is the authenticated creator of the escalation, never a
// client-supplied field.
func (s *BeadsServer) matchingHumanSpawnRule(ctx context.Context, spawnRole string, operationSpec []byte, requesterSubject string) *spawnPolicyRule {
	roles := parseRoleList(spawnRole)
	if len(roles) != 1 {
		return nil
	}
	policy, err := s.lookupSpawnPolicy(ctx, roles[0])
	if err != nil || policy == nil {
		return nil
	}
	rules, err := parseSpawnPolicyRules(policy.Fields)
	if err != nil {
		return nil
	}
	var spec createBeadInput
	if err := json.Unmarshal(operationSpec, &spec); err != nil ||
		spec.Type != "agent" || roleFromFields(spec.Fields) != spawnRole {
		return nil
	}
	requesterRole, namespace := s.resolveRequesterContext(ctx, requesterSubject)
	matchCtx := spawnPolicyMatchContext{
		Project:       projectFromFields(spec.Fields),
		Namespace:     namespace,
		RequesterRole: requesterRole,
	}
	for i := range rules {
		if !rules[i].When.matches(matchCtx) {
			continue
		}
		// An auto match never carries four_eyes or approvers, and one that is
		// over cap falls through at evaluation time, so only a human match
		// governs approval.
		if rules[i].Mode == "human" {
			return &rules[i]
		}
	}
	return nil
}
