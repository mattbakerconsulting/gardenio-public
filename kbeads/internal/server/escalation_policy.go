package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"gardenio/kbeads/internal/model"
)

// escalationPolicyType is the singleton config bead that routes escalations
// (escalation-routing plan D7). The newest open one is active; none means
// defaultEscalationPolicy.
const escalationPolicyType = "escalation_policy"

type escalationPolicyWhen struct {
	OperationTypes []string `json:"operation_types,omitempty"`
	// OperationClasses names registry classes (escalation_op_registry.go).
	OperationClasses []string `json:"operation_classes,omitempty"`
	Projects         []string `json:"projects,omitempty"`
	Lands            []string `json:"lands,omitempty"` // "*" = any tenant land
	Sensitivity      []string `json:"sensitivity,omitempty"`
}

type escalationPolicyRule struct {
	When      escalationPolicyWhen `json:"when"`
	Approvers []string             `json:"approvers"`  // lane refs; may contain "{land}"
	ExecuteBy string               `json:"execute_by"` // lane ref; may contain "{land}"
	// HoldBy holds the escalation at create without being an approver (D7):
	// it may claim and hand up, never approve. Empty: the first staffed approver.
	HoldBy string `json:"hold_by,omitempty"`
}

type escalationCoordinator struct {
	Principals []string `json:"principals"` // "machine:<id>" | "role:<name>"
	// Audience is #756's audience for an escalation this coordinator holds
	// (D11): "desk" or "operator" (the default). Never "owner".
	Audience string `json:"audience,omitempty"`
}

type escalationPolicy struct {
	Rules        []escalationPolicyRule           `json:"rules"`
	Coordinators map[string]escalationCoordinator `json:"coordinators"`
}

// escalationFacts are what a rule matches on. Land is set only when Project is
// a tenant land (land_kind == "tenant").
type escalationFacts struct{ OperationType, Project, Land, Sensitivity string }

// escalationRoute is what the policy's rules say. The D3 floor is not here:
// it is escalationForcedReason (escalation_audience_authz.go), applied by
// escalationRouteFieldsFor, so there is one floor for routing and audience.
type escalationRoute struct {
	Rule         string // "escalation#<i>" (1-based) | "floor:no_matching_rule"
	Approvers    []string
	ExecuteBy    string
	HoldBy       string
	OperatorOnly bool // only floor:no_matching_rule
}

// laneAudience is D11's mapping, the only one: the #756 audience an escalation
// held by ref has. The read side never recomputes it; it reads the stamp. An
// unknown ref or coordinator fails toward the owner (Global Constraint 4).
func laneAudience(ref string, p escalationPolicy) string {
	lane, err := parseLaneRef(ref)
	if err != nil {
		return audienceOwner
	}
	switch lane.Kind {
	case laneOperator, laneLandOwner:
		return audienceOwner
	case laneSteward:
		return audienceDesk
	case laneRequester, laneServer:
		return audienceOperator
	case laneCoordinator:
		c, ok := p.Coordinators[lane.Name]
		if !ok {
			return audienceOwner
		}
		if c.Audience == audienceDesk {
			return audienceDesk
		}
		return audienceOperator
	}
	return audienceOwner
}

// defaultEscalationPolicy is D7, used while no escalation_policy bead exists.
func defaultEscalationPolicy() escalationPolicy {
	merges := []string{"github_pr_merge", "gitlab_mr_merge"}
	return escalationPolicy{
		Rules: []escalationPolicyRule{
			{When: escalationPolicyWhen{OperationTypes: merges, Projects: []string{"gardenio"}}, Approvers: []string{"coordinator:merge-desk", "coordinator:laptop", "operator"}, ExecuteBy: "coordinator:merge-desk"},
			{When: escalationPolicyWhen{OperationTypes: merges, Lands: []string{"*"}}, Approvers: []string{"steward:{land}", "land-owner:{land}"}, ExecuteBy: "requester"},
			{When: escalationPolicyWhen{OperationTypes: []string{"manual_owner_action", "soak_verification"}}, Approvers: []string{"operator"}, ExecuteBy: "operator"},
			{When: escalationPolicyWhen{OperationTypes: []string{"create", "update", "close", "grant"}}, Approvers: []string{"operator"}, ExecuteBy: "server", HoldBy: "coordinator:laptop"},
			{Approvers: []string{"operator"}, ExecuteBy: "coordinator:laptop", HoldBy: "coordinator:laptop"},
		},
		Coordinators: map[string]escalationCoordinator{
			"merge-desk": {Principals: []string{"role:reviewer"}, Audience: audienceDesk},
			"laptop":     {Principals: []string{"machine:gardenio-laptop-operator"}, Audience: audienceOperator},
		},
	}
}

func policyListHas(list []string, v string) bool {
	return slices.ContainsFunc(list, func(e string) bool { return strings.EqualFold(strings.TrimSpace(e), v) })
}

func (w escalationPolicyWhen) empty() bool {
	return len(w.OperationTypes) == 0 && len(w.OperationClasses) == 0 && len(w.Projects) == 0 && len(w.Lands) == 0 && len(w.Sensitivity) == 0
}

func (w escalationPolicyWhen) matches(f escalationFacts) bool {
	// operation_types names spellings, never their aliases: only
	// operation_classes matches through the registry, so an alias joins a
	// rule only by an escalation_policy_write.
	op := normalizeOperationType(f.OperationType)
	if len(w.OperationTypes) > 0 && !policyListHas(w.OperationTypes, op) {
		return false
	}
	if len(w.OperationClasses) > 0 && !slices.Contains(w.OperationClasses, operationClassOf(op)) {
		return false
	}
	if len(w.Projects) > 0 && (f.Project == "" || !slices.Contains(w.Projects, f.Project)) {
		return false
	}
	if len(w.Lands) > 0 {
		if f.Land == "" || (!slices.Contains(w.Lands, "*") && !slices.Contains(w.Lands, f.Land)) {
			return false
		}
	}
	if len(w.Sensitivity) > 0 && !policyListHas(w.Sensitivity, strings.ToLower(strings.TrimSpace(f.Sensitivity))) {
		return false
	}
	return true
}

func templateLand(ref, land string) string { return strings.ReplaceAll(ref, "{land}", land) }

// evaluateEscalationPolicy routes f by the first matching rule. It does not
// apply the D3 floor; escalationRouteFieldsFor does, from
// escalationForcedReason. Nothing matching fails closed to an operator-only
// route.
func evaluateEscalationPolicy(p escalationPolicy, f escalationFacts) escalationRoute {
	for i, rule := range p.Rules {
		if !rule.When.matches(f) {
			continue
		}
		approvers := make([]string, 0, len(rule.Approvers))
		for _, a := range rule.Approvers {
			approvers = append(approvers, templateLand(a, f.Land))
		}
		return escalationRoute{Rule: fmt.Sprintf("escalation#%d", i+1), Approvers: approvers, ExecuteBy: templateLand(rule.ExecuteBy, f.Land), HoldBy: templateLand(rule.HoldBy, f.Land)}
	}
	return escalationRoute{Rule: "floor:no_matching_rule", Approvers: []string{string(laneOperator)}, ExecuteBy: string(laneOperator), OperatorOnly: true}
}

func strictDecode(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// parseEscalationPolicy decodes a policy bead's fields. rules and coordinators
// are decoded strictly, so a misspelt key is an error rather than a rule that
// silently matches everything.
func parseEscalationPolicy(fields json.RawMessage) (escalationPolicy, error) {
	var top struct {
		Rules        json.RawMessage `json:"rules"`
		Coordinators json.RawMessage `json:"coordinators"`
	}
	if len(fields) == 0 {
		return escalationPolicy{}, fmt.Errorf("no fields")
	}
	if err := json.Unmarshal(fields, &top); err != nil {
		return escalationPolicy{}, err
	}
	if len(top.Rules) == 0 || len(top.Coordinators) == 0 {
		return escalationPolicy{}, fmt.Errorf("rules and coordinators are both required")
	}
	var p escalationPolicy
	if err := strictDecode(top.Rules, &p.Rules); err != nil {
		return escalationPolicy{}, fmt.Errorf("rules: %w", err)
	}
	if err := strictDecode(top.Coordinators, &p.Coordinators); err != nil {
		return escalationPolicy{}, fmt.Errorf("coordinators: %w", err)
	}
	return p, nil
}

// validateEscalationPolicy returns the 400 reason for a malformed policy, or "".
func validateEscalationPolicy(fields json.RawMessage) string {
	p, err := parseEscalationPolicy(fields)
	if err != nil {
		return "escalation_policy is malformed: " + err.Error()
	}
	if len(p.Rules) == 0 {
		return "escalation_policy needs at least one rule"
	}
	for name, c := range p.Coordinators {
		if !validLaneName(name) {
			return fmt.Sprintf("escalation_policy coordinator name %q is invalid", name)
		}
		if len(c.Principals) == 0 {
			return fmt.Sprintf("escalation_policy coordinator %q lists no principals", name)
		}
		if c.Audience != "" && c.Audience != audienceDesk && c.Audience != audienceOperator {
			return fmt.Sprintf("escalation_policy coordinator %q audience must be desk or operator", name)
		}
		for _, pr := range c.Principals {
			kind, rest, ok := strings.Cut(pr, ":")
			if !ok || (kind != "machine" && kind != "role") || strings.TrimSpace(rest) == "" {
				return fmt.Sprintf("escalation_policy coordinator %q principal %q must be machine:<client-id> or role:<role>", name, pr)
			}
		}
	}
	for i, r := range p.Rules {
		n := i + 1
		landRule := len(r.When.Lands) > 0
		for _, c := range r.When.OperationClasses {
			if !isOperationClass(c) {
				return fmt.Sprintf("escalation_policy rule %d operation class %q is not registered", n, c)
			}
		}
		if len(r.Approvers) == 0 {
			return fmt.Sprintf("escalation_policy rule %d lists no approvers", n)
		}
		for _, a := range r.Approvers {
			if reason := validatePolicyLaneRef(p, a, landRule, true); reason != "" {
				return fmt.Sprintf("escalation_policy rule %d approver %q: %s", n, a, reason)
			}
		}
		if reason := validatePolicyLaneRef(p, r.ExecuteBy, landRule, false); reason != "" {
			return fmt.Sprintf("escalation_policy rule %d execute_by %q: %s", n, r.ExecuteBy, reason)
		}
		if r.HoldBy != "" {
			if reason := validatePolicyLaneRef(p, r.HoldBy, landRule, true); reason != "" {
				return fmt.Sprintf("escalation_policy rule %d hold_by %q: %s", n, r.HoldBy, reason)
			}
			if lane, _ := parseLaneRef(templateLand(r.HoldBy, "land")); lane.Kind == laneLegacy {
				return fmt.Sprintf("escalation_policy rule %d hold_by %q: a legacy entry cannot hold an escalation", n, r.HoldBy)
			}
		}
	}
	if !p.Rules[len(p.Rules)-1].When.empty() {
		return "escalation_policy's last rule must have an empty when (the catch-all)"
	}
	return ""
}

func validatePolicyLaneRef(p escalationPolicy, ref string, landRule, approver bool) string {
	if strings.Contains(ref, "{land}") && !landRule {
		return "{land} is only valid in a rule with when.lands"
	}
	lane, err := parseLaneRef(templateLand(ref, "land"))
	if err != nil {
		return err.Error()
	}
	switch lane.Kind {
	case laneLegacy:
		if !approver {
			return "legacy approver entries are valid in approvers only"
		}
	case laneServer, laneRequester:
		if approver {
			return string(lane.Kind) + " may execute but never approve"
		}
	case laneCoordinator:
		if _, ok := p.Coordinators[lane.Name]; !ok {
			return fmt.Sprintf("no coordinator named %q", lane.Name)
		}
	}
	return ""
}

// activeEscalationPolicy returns the newest open escalation_policy bead, or the
// built-in D7 when there is none. A malformed bead is an error, never a
// fallback to D7: callers fail closed.
func (s *BeadsServer) activeEscalationPolicy(ctx context.Context) (escalationPolicy, error) {
	beads, _, err := s.store.ListBeads(ctx, model.BeadFilter{
		Type:   []model.BeadType{escalationPolicyType},
		Kind:   []model.Kind{"config"},
		Status: []model.Status{model.StatusOpen},
		Sort:   "-created_at",
		Limit:  1,
	})
	if err != nil {
		return escalationPolicy{}, fmt.Errorf("listing escalation_policy: %w", err)
	}
	if len(beads) == 0 {
		return defaultEscalationPolicy(), nil
	}
	p, err := parseEscalationPolicy(beads[0].Fields)
	if err != nil {
		return escalationPolicy{}, fmt.Errorf("escalation_policy %s: %w", beads[0].ID, err)
	}
	return p, nil
}

// escalationFactsFor reads the routing facts from an escalation. Several
// project labels match no project rule; a land lookup failure means no land.
// Both fall to the catch-all (operator), never to a wider lane.
func (s *BeadsServer) escalationFactsFor(ctx context.Context, b *model.Bead) escalationFacts {
	sens, _, _ := exactFieldString(b.Fields, "sensitivity")
	f := escalationFacts{
		OperationType: normalizeOperationType(parseSpawnEscalationFields(b).OperationType),
		Sensitivity:   strings.ToLower(strings.TrimSpace(sens)),
	}
	switch projects := projectLabelValues(b.Labels); len(projects) {
	case 0:
		f.Project = projectFromFields(b.Fields)
	case 1:
		f.Project = projects[0]
	default:
		slog.Warn("escalation routing: several project labels; no project rule applies", "escalation", b.ID, "projects", projects)
	}
	if f.Project != "" {
		tenant, err := s.isTenantLand(ctx, f.Project)
		if err != nil {
			slog.Warn("escalation routing: land lookup failed; routing as a non-land project", "escalation", b.ID, "project", f.Project, "error", err)
		} else if tenant {
			f.Land = f.Project
		}
	}
	return f
}

// escalationRouteFieldsFor computes the stamped routing fields for b, audience
// included (D11 rule 2). A server-stamped approvers list already on b
// (handleEscalation's spawn rule) is kept unless a floor reason applies
// (CONTRACT ISSUE 7). The floor is escalationForcedReason, #756's forced-owner
// rule extended (D3), so routing and audience can never disagree about it.
func (s *BeadsServer) escalationRouteFieldsFor(ctx context.Context, b *model.Bead) (map[string]any, error) {
	f := s.escalationFactsFor(ctx, b)
	forced := s.escalationForcedReason(ctx, b)
	p, err := s.activeEscalationPolicy(ctx)
	if err != nil {
		if !isEscalationFloorReason(forced) {
			return nil, err
		}
		slog.Warn("escalation routing: policy unreadable; a floor escalation routes on the built-in default", "escalation", b.ID, "error", err)
		p = defaultEscalationPolicy()
	}
	route := evaluateEscalationPolicy(p, f)
	approvers, operatorOnly, rule := route.Approvers, route.OperatorOnly, route.Rule
	if isEscalationFloorReason(forced) {
		// A merge the desk named an operator class (P10.F4) is still the
		// desk's to merge once the owner approves.
		if forced != operatorClassMergeReason {
			route.ExecuteBy = string(laneOperator)
			if _, ok := p.Coordinators["laptop"]; ok {
				route.ExecuteBy = "coordinator:laptop" // CONTRACT ISSUE 17
			}
		}
		approvers, operatorOnly, rule = []string{string(laneOperator)}, true, "floor:"+forced
	} else if land := s.escalationLandPeople(ctx, b); land != "" {
		// The floor's one exception (P10.C) is routed in code beside it: the
		// land's owners approve, the Keycloak change runs where the floor's does.
		route.ExecuteBy, route.HoldBy = string(laneOperator), ""
		if _, ok := p.Coordinators["laptop"]; ok {
			route.ExecuteBy = "coordinator:laptop"
		}
		approvers, operatorOnly, rule = []string{laneRef{Kind: laneLandOwner, Name: land}.String()}, false, landPeopleRule
	} else if existing, err := escalationApprovers(b); err == nil && len(existing) > 0 {
		approvers = existing
	}
	asked, _, _ := exactFieldString(b.Fields, escalationAudienceField) // #756 stamped it already
	var held string
	switch {
	case forced != "" || asked == audienceOwner || operatorOnly:
		held = s.firstOwnerLane(b, approvers)
	case route.HoldBy != "":
		held = route.HoldBy // staffed or not (D11 rule 2); the idle hand-up bounds an unstaffed holder
	default:
		held = s.firstStaffedLane(ctx, b, approvers, p)
	}
	fields := map[string]any{
		"approvers":             approvers,
		"execute_by":            route.ExecuteBy,
		"held_by":               held,
		"held_at":               time.Now().UTC().Format(time.RFC3339),
		escalationAudienceField: laneAudience(held, p),
		"operator_only":         operatorOnly,
		"policy_rule":           rule,
	}
	s.noteOfflineHolder(ctx, b, fields, p)
	return fields, nil
}

// firstOwnerLane is the first owner-audience lane among approvers (operator or
// a land-owner), else operator: where a forced or raised escalation is held.
func (s *BeadsServer) firstOwnerLane(esc *model.Bead, approvers []string) string {
	for _, a := range approvers {
		if lane, err := parseLaneRef(a); err == nil && (lane.Kind == laneOperator || lane.Kind == laneLandOwner) {
			return lane.String()
		}
	}
	return string(laneOperator)
}

// laneForAudience is D11 rule 3: the lane an escalation moves to when it is
// routed to audience a. Owner: the first owner-audience approver, else
// operator. Desk or operator: the first staffed approver lane of that
// audience, else a policy coordinator of that audience (it holds without
// approving), else an error (the route endpoint's 409). The coordinator is the
// escalation's execute_by, then its stamped rule's hold_by, when that lane has
// audience a, and only then the first in name order: an audience can have
// several coordinators, and only the executor's lane can act on the
// escalation (gd-OBwxpCcMBY).
func (s *BeadsServer) laneForAudience(ctx context.Context, esc *model.Bead, a string, p escalationPolicy) (string, error) {
	approvers, _ := escalationApprovers(esc)
	if a == audienceOwner {
		return s.firstOwnerLane(esc, approvers), nil
	}
	for _, ref := range approvers {
		lane, err := parseLaneRef(ref)
		if err != nil || lane.Kind == laneLegacy || laneAudience(lane.String(), p) != a {
			continue
		}
		if staffed, err := s.laneStaffed(ctx, lane, esc, p); err == nil && staffed {
			return lane.String(), nil
		}
	}
	executeBy, _, _ := exactFieldString(esc.Fields, "execute_by")
	for _, ref := range []string{executeBy, policyRuleHoldBy(esc, p)} {
		if lane, err := parseLaneRef(ref); err == nil && lane.Kind == laneCoordinator && laneAudience(lane.String(), p) == a {
			return lane.String(), nil
		}
	}
	names := make([]string, 0, len(p.Coordinators))
	for name := range p.Coordinators {
		names = append(names, name)
	}
	slices.Sort(names) // deterministic last resort
	for _, name := range names {
		if ref := "coordinator:" + name; laneAudience(ref, p) == a {
			return ref, nil
		}
	}
	return "", fmt.Errorf("no %s lane can hold this escalation", a)
}

// policyRuleHoldBy is the hold_by of the policy rule stamped on esc
// ("escalation#<i>"), or "" when esc names no rule of p.
func policyRuleHoldBy(esc *model.Bead, p escalationPolicy) string {
	if rule := stampedPolicyRule(esc, p); rule != nil {
		return rule.HoldBy
	}
	return ""
}

// stampEscalationRoute writes the route, audience included, into b.Fields
// (the create path). On a policy error b keeps #756's audience and no
// held_by (Global Constraint 7), and the sweep retries.
func (s *BeadsServer) stampEscalationRoute(ctx context.Context, b *model.Bead) error {
	fields, err := s.escalationRouteFieldsFor(ctx, b)
	if err != nil {
		slog.Warn("escalation routing: policy unreadable; escalation left unrouted (#756 audience until the sweep)", "escalation", b.ID, "error", err)
		return err
	}
	for k, v := range fields {
		b.Fields = setFieldValue(b.Fields, k, v)
	}
	return nil
}

const (
	escalationRouteSweepCap      = 500
	escalationRouteSweepInterval = time.Minute
	// escalationRouteAdvisoryLockKey serializes the sweep across replicas.
	// It must differ from every other key (decision aging 0001, audit retention
	// 0002, slow tool calls 0003, spawn policy 0004).
	escalationRouteAdvisoryLockKey int64 = 0x6265_6164_0005
)

// StartEscalationRouteSweep stamps open escalations that have no held_by
// (filed before routing shipped, or while the policy was unreadable). It is
// always on, and it only writes escalations that are missing held_by.
func (s *BeadsServer) StartEscalationRouteSweep(ctx context.Context) {
	go s.runEscalationRouteSweep(ctx, escalationRouteSweepInterval)
}

func (s *BeadsServer) runEscalationRouteSweep(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	s.sweepUnroutedEscalations(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sweepUnroutedEscalations(ctx)
		}
	}
}

func (s *BeadsServer) sweepUnroutedEscalations(ctx context.Context) {
	acquired, err := s.store.TryAdvisoryLock(ctx, escalationRouteAdvisoryLockKey, s.sweepUnroutedEscalationsLocked)
	if err != nil {
		slog.Warn("escalation routing: advisory lock error", "error", err)
		return
	}
	if !acquired {
		slog.Info("escalation routing: skipping tick, another replica holds the sweep lock")
	}
}

// sweepUnroutedEscalationsLocked is the sweep body. Errors are logged and
// retried next tick, so it always returns nil.
func (s *BeadsServer) sweepUnroutedEscalationsLocked(ctx context.Context) error {
	beads, _, err := s.store.ListBeads(ctx, model.BeadFilter{
		Type:   []model.BeadType{model.TypeEscalation},
		Status: []model.Status{model.StatusOpen, model.StatusInProgress},
		Limit:  escalationRouteSweepCap,
		Sort:   "created_at",
	})
	if err != nil {
		slog.Warn("escalation routing: failed to list open escalations", "error", err)
		return nil
	}
	if len(beads) == escalationRouteSweepCap {
		slog.Warn("escalation routing: sweep hit the working-set cap; remainder deferred", "cap", escalationRouteSweepCap)
	}
	stamped, handedUp := 0, 0
	idle := escalationLaneIdleHandUp()
	for _, b := range beads {
		if held, _, _ := exactFieldString(b.Fields, "held_by"); strings.TrimSpace(held) != "" {
			if s.maybeIdleHandUp(ctx, b, idle) {
				handedUp++
			}
			continue
		}
		fields, err := s.escalationRouteFieldsFor(ctx, b)
		if err != nil {
			slog.Warn("escalation routing: policy unreadable; sweep stops this tick", "escalation", b.ID, "error", err)
			return nil
		}
		// D11 rule 4: an audience #756 stamped (or an operator re-routed) is
		// kept; the lane is chosen within it. Forced escalations are already
		// owner-held by escalationRouteFieldsFor.
		if had, _, _ := exactFieldString(b.Fields, escalationAudienceField); validEscalationAudience(had) && had != fields[escalationAudienceField] && s.escalationForcedReason(ctx, b) == "" {
			p, _ := s.activeEscalationPolicy(ctx) // read just now by escalationRouteFieldsFor
			// The lane is chosen against the route being stamped (its
			// approvers, execute_by and policy_rule), not the bare bead.
			routed := *b
			for k, v := range fields {
				routed.Fields = setFieldValue(routed.Fields, k, v)
			}
			if lane, err := s.laneForAudience(ctx, &routed, had, p); err == nil {
				fields["held_by"], fields[escalationAudienceField] = lane, had
			} else {
				slog.Warn("escalation routing: sweep cannot keep the stamped audience; using the policy's lane", "escalation", b.ID, "audience", had, "held_by", fields["held_by"], "error", err)
			}
		}
		if err := s.mergeBeadFields(ctx, b.ID, fields); err != nil {
			slog.Warn("escalation routing: failed to stamp route", "escalation", b.ID, "error", err)
			continue
		}
		stamped++
	}
	slog.Info("escalation routing: sweep complete", "considered", len(beads), "stamped", stamped, "idle_handed_up", handedUp)
	return nil
}
