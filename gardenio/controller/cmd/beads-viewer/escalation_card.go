package main

import (
	"encoding/json"
	"html/template"
	"sort"
	"strings"
	"time"

	"github.com/yuin/goldmark"

	"gardenio/controller/internal/beadsapi"
)

// escalationPrimaryFields are the escalation fields the rich card renders in
// its own dedicated slots (header chips, context prose, target links, approval
// trail). They are suppressed from the card's "other fields" tail so the same
// value never reads twice on one page.
var escalationPrimaryFields = map[string]bool{
	"operation_type":    true,
	"operation_spec":    true,
	"context":           true,
	"sensitivity":       true,
	"audience":          true,
	"requesting_agent":  true,
	"requesting_task":   true,
	"requesting_plan":   true,
	"approved_by":       true,
	"approved_by_email": true,
	"approved_at":       true,
	"approval_method":   true,
	"rejected_by":       true,
	"rejected_at":       true,
	"rejection_reason":  true,
	"result_status":     true,
	"result":            true,
	"executor":          true,
	"execution_target":  true,
	// spawn_role marks the escalation as a privileged-role spawn request
	// (gd-AWTT7AroK4) and is rendered by the dedicated spawn block below
	// instead of the generic tail. policy_rule is reserved for the
	// spawn_policy auto-approval evaluator (gd-vmLhgqydEa, not yet built) —
	// suppressed here too so it never double-renders once that lands.
	"spawn_role":  true,
	"policy_rule": true,
	// The ServiceAccount snapshot taken at approval (gd-SyEYkqe601): a JSON
	// blob, useless in the generic tail; its live diff renders on the spawned
	// agent's page (agent_permission_diff.html).
	"permissions_at_approval": true,
	"snapshot_at":             true,
	// Routing (escalation routing D1): rendered by the card's routing panel.
	"held_by":        true,
	"execute_by":     true,
	"approvers":      true,
	"operator_only":  true,
	"approved_as":    true,
	"claimed_by":     true,
	"claimed_at":     true,
	"handed_up_by":   true,
	"handed_up_at":   true,
	"hand_up_reason": true,
}

// escalationTarget is one clickable thing the operator is being asked to act
// on — the MR to merge, the dashboard to look at, the run URL to inspect. It
// exists because the operator complaint that started this work (ka-0000006884)
// was precisely that an escalation's `mr_url` rendered as unclickable text
// buried inside a raw JSON blob.
type escalationTarget struct {
	// Label is the human-readable target ("site-deployment!1274"), not the URL.
	Label string
	// Href is the absolute target URL.
	Href string
	// Kind drives the icon/styling: "mr" | "link".
	Kind string
	// Note is optional secondary text rendered beside the link (branch name,
	// project path).
	Note string
}

// escalationFieldRow is one row of the card's "other fields" tail — everything
// on the bead that the dedicated slots above did not already render.
type escalationFieldRow struct {
	Key   string
	Label string
	Value template.HTML
}

// escalationView is the per-type view model for `escalation` beads, rendered by
// templates/partials/type_escalation.html.
type escalationView struct {
	BeadID string
	// Audience is who the escalation is for: owner, operator or desk
	// (escalation_audience.go). AudienceForced: it needs credentials or is
	// secret, so it is the owner's whatever its field says. AudienceDefault: no
	// filer or router chose it; it is operation_type's default. RouteTo is the
	// other audiences, and CanRoute shows the re-route control (an operator or
	// admin, on an open escalation that is not forced). None of these gate
	// approval.
	Audience        string
	AudienceForced  bool
	AudienceDefault bool
	RouteTo         []string
	CanRoute        bool
	// OperationType is the raw operation_type ("gitlab_mr_merge"); OperationLabel
	// is its humanized form for the header chip.
	OperationType  string
	OperationLabel string
	Sensitivity    string
	Status         string
	// ContextHTML is the `context` field rendered as markdown + linkified — the
	// agent's explanation of what it needs and why it could not do it itself.
	ContextHTML template.HTML
	// Targets are the clickable action targets extracted from operation_spec.
	Targets []escalationTarget
	// SpecHTML is the whole operation_spec rendered as a nested key/value list
	// (every leaf URL clickable), shown in a collapsed <details> beneath the
	// targets so the exact machine-readable spec stays auditable.
	SpecHTML template.HTML
	// HasSpec is false when the bead carries no operation_spec at all (an
	// information-only escalation), so the template can skip the whole block.
	HasSpec bool

	RequestingAgent string
	RequestingTask  string
	RequestingPlan  string
	// Origin is the resolved agent / task / plan strip (request_origin.go):
	// the agent by NAME (requesting_agent holds a bead id), the task by title,
	// the plan by title. Populated by callers with a daemon handle
	// (injectTypeView, renderEscalationPartial); buildEscalationView is pure.
	Origin requestOrigin

	// Spawn-request fields (gd-hmzdQ5N6ix): populated when spawn_role is set,
	// i.e. this escalation is a privileged-role spawn denial routed through
	// authorizeNonHumanSpawn (kbeads spawn_authz.go). IsSpawn switches the
	// template to a dedicated "spawn request" block instead of the generic
	// operation dump, matching design doc gd-sGW5Y1B8sg §7 (role, project,
	// target namespace, requester + chain-root human, rule matched or
	// approver awaited).
	IsSpawn        bool
	SpawnRole      string
	SpawnProject   string
	SpawnAgentName string
	// SpawnNamespace is populated only when operation_spec happens to carry a
	// namespace-shaped field. No current type:agent/role/project schema or
	// spawn create body sets one (verified against kbeads/internal/server
	// config.go and the spawn_authz E2E fixtures), so this is almost always
	// empty today; the field exists so the card picks the value up for free
	// once a namespace concept lands, with no further card-code change.
	SpawnNamespace string
	// PolicyRule is set once the spawn_policy auto-approval evaluator
	// (gd-vmLhgqydEa, not yet built) starts stamping policy_rule on an
	// escalation it auto-resolved. Empty today for every escalation in
	// production, so the template always reads "awaiting approver" until
	// that evaluator ships.
	PolicyRule string
	// RequesterChainRootHTML is the requesting agent's chain-root human,
	// reusing the same operator-chip rendering as the rest of the viewer
	// (operator_chip.go) rather than a bespoke spawn-only lookup: the
	// server already resolves this chain for every bead as
	// OperatedBy/OperatedByDisplay/OperatedByEmail/OperatedBySlug.
	RequesterChainRootHTML template.HTML

	// Routing (D1): who may approve, who does the work, who it waits on now.
	// All server-stamped. An unstamped escalation has none of them; the head's
	// #756 audience chip is what it shows (Global Constraint 7).
	HeldBy         string
	HeldByLabel    string
	Approvers      []string // labels, in policy order; nil when unstamped
	OperatorOnly   bool
	ExecuteByLabel string
	RoutingRule    string // policy_rule: "escalation#<i>" or "floor:<reason>"
	ApprovedAs     string // label of the lane an approval was made as
	ClaimedBy      string
	ClaimedAt      string
	HandedUpBy     string
	HandedUpAt     string
	HandUpReason   string
	// LaneNote says in words whether the escalation waits on the owner, so an
	// owner who may approve a coordinator-held item sees it is not theirs, and
	// that an approval with no cluster dispatch runs nothing (gd-cuTEquiXqq).
	LaneNote string
	// LaneOffline says the presence lane holding it is away ("laptop offline
	// since 22:06Z"), from the daemon's held_by_presence (gd-t41kYFbLUt).
	LaneOffline string

	// Approval trail. Approved/Rejected are mutually exclusive terminal states.
	Approved        bool
	ApprovedBy      string
	ApprovedAt      string
	ApprovalMethod  string
	Rejected        bool
	RejectedBy      string
	RejectionReason string
	// ResultStatus / Executor describe the auto-replay the daemon ran after
	// approval ("success" / "failed"), so the operator sees whether the merge
	// they approved actually landed.
	ResultStatus string
	Executor     string
	ResultHTML   template.HTML

	// Fields is the tail of non-primary bead fields, rendered with the same
	// rich fieldValue treatment as the generic dump.
	Fields []escalationFieldRow

	// Open is true while the escalation is still actionable (status open or
	// in_progress and no approval/rejection recorded).
	Open bool
	// CanApprove gates the approve/reject chips: the daemon's can-approve
	// allowed it and the escalation is open (escalationApproveGate).
	CanApprove bool
	// ApproveBlockedReason explains a false CanApprove on an otherwise-open
	// escalation ("sign in", the daemon's can-approve reason), so the
	// operator is told why rather than shown nothing.
	ApproveBlockedReason string
	// Error renders inline at the top of the card after a failed approve/reject.
	Error string
}

// buildEscalationView shapes an escalation bead into the rich card view model.
// Returns nil for a nil bead. md may be nil (markdown degrades to linkified
// plain text).
func buildEscalationView(bead *beadsapi.BeadDetail, md goldmark.Markdown, basePath string, now time.Time) *escalationView {
	if bead == nil {
		return nil
	}
	f := bead.Fields
	v := &escalationView{
		BeadID:          bead.ID,
		OperationType:   strings.TrimSpace(f["operation_type"]),
		Sensitivity:     strings.TrimSpace(f["sensitivity"]),
		Status:          bead.Status,
		RequestingAgent: strings.TrimSpace(f["requesting_agent"]),
		RequestingTask:  strings.TrimSpace(f["requesting_task"]),
		RequestingPlan:  strings.TrimSpace(f["requesting_plan"]),
		ApprovedBy:      strings.TrimSpace(firstNonEmpty(f, "approved_by_email", "approved_by")),
		ApprovedAt:      strings.TrimSpace(f["approved_at"]),
		ApprovalMethod:  strings.TrimSpace(f["approval_method"]),
		RejectedBy:      strings.TrimSpace(f["rejected_by"]),
		RejectionReason: strings.TrimSpace(firstNonEmpty(f, "rejection_reason", "reject_reason")),
		ResultStatus:    strings.TrimSpace(f["result_status"]),
		Executor:        strings.TrimSpace(f["executor"]),
	}
	v.OperationLabel = humanizeFieldKey(v.OperationType)
	v.Approved = v.ApprovedBy != ""
	v.Rejected = v.RejectedBy != "" || (bead.Status == "closed" && v.RejectionReason != "")
	v.Open = (bead.Status == "open" || bead.Status == "in_progress") && !v.Rejected
	v.HeldBy = beadsapi.EscalationHeldBy(bead)
	v.HeldByLabel = beadsapi.LaneLabel(v.HeldBy)
	var approvers []string
	if raw := strings.TrimSpace(f["approvers"]); raw != "" && json.Unmarshal([]byte(raw), &approvers) == nil {
		for _, a := range approvers {
			v.Approvers = append(v.Approvers, beadsapi.LaneLabel(a))
		}
	}
	v.OperatorOnly = strings.TrimSpace(f["operator_only"]) == "true"
	if ex := beadsapi.EscalationExecuteBy(bead); ex != "" {
		v.ExecuteByLabel = beadsapi.LaneLabel(ex)
	}
	if as := strings.TrimSpace(f["approved_as"]); as != "" {
		v.ApprovedAs = beadsapi.LaneLabel(as)
	}
	v.ClaimedBy, v.ClaimedAt = strings.TrimSpace(f["claimed_by"]), strings.TrimSpace(f["claimed_at"])
	v.HandedUpBy, v.HandedUpAt = strings.TrimSpace(f["handed_up_by"]), strings.TrimSpace(f["handed_up_at"])
	v.HandUpReason = strings.TrimSpace(f["hand_up_reason"])
	v.LaneNote = escalationLaneNote(v, f)
	audienceView(v, f)

	if ctx := strings.TrimSpace(f["context"]); ctx != "" {
		v.ContextHTML = fieldMarkdownHTML(md, ctx, basePath)
	}
	if spec := strings.TrimSpace(f["operation_spec"]); spec != "" {
		v.HasSpec = true
		if h, ok := fieldJSONHTML(spec, basePath, 0); ok {
			v.SpecHTML = h
		} else {
			v.SpecHTML = fieldMarkdownHTML(md, spec, basePath)
		}
		v.Targets = escalationTargetsFromSpec(spec)
	}
	if res := strings.TrimSpace(f["result"]); res != "" {
		if h, ok := fieldJSONHTML(res, basePath, 0); ok {
			v.ResultHTML = h
		} else {
			v.ResultHTML = fieldMarkdownHTML(md, res, basePath)
		}
	}

	v.SpawnRole = strings.TrimSpace(f["spawn_role"])
	v.IsSpawn = v.SpawnRole != ""
	// policy_rule is routing's ("escalation#<i>", "floor:<reason>") or the
	// spawn_policy evaluator's; only the latter belongs in the spawn block.
	rule := strings.TrimSpace(f["policy_rule"])
	if strings.HasPrefix(rule, "escalation#") || strings.HasPrefix(rule, "floor:") {
		v.RoutingRule = rule
		rule = ""
	}
	if v.IsSpawn {
		v.PolicyRule = rule
		if spec := strings.TrimSpace(f["operation_spec"]); spec != "" {
			v.SpawnProject, v.SpawnAgentName, v.SpawnNamespace = parseSpawnCreateSpec(spec)
		}
		v.RequesterChainRootHTML = beadOperatorChip(bead, basePath, "")
	}

	// Tail: every field the dedicated slots did not consume, rendered with the
	// same rich treatment as the generic dump so nothing is silently dropped.
	keys := make([]string, 0, len(f))
	for k := range f {
		if escalationPrimaryFields[k] || strings.TrimSpace(f[k]) == "" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v.Fields = append(v.Fields, escalationFieldRow{
			Key:   k,
			Label: humanizeFieldKey(k),
			Value: renderFieldValue(md, f[k], basePath, now),
		})
	}
	return v
}

// escalationTargetsFromSpec pulls the clickable action targets out of an
// operation_spec JSON blob. The GitLab MR shape (mr_url + project + mr_iid,
// written by `gb escalation create --operation-type gitlab_mr_merge`) gets a
// first-class "project!iid" pill; any other absolute http(s) URL anywhere in
// the spec becomes a plain link keyed by its field name. Returns nil when the
// spec is unparseable or carries no URL — the template then renders only the
// spec detail block.
func escalationTargetsFromSpec(spec string) []escalationTarget {
	var m map[string]any
	dec := json.NewDecoder(strings.NewReader(spec))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return nil
	}
	var out []escalationTarget
	seen := map[string]bool{}

	// First-class MR target.
	if mrURL, ok := specString(m, "mr_url"); ok {
		if href, ok2 := bareFieldURL(mrURL); ok2 {
			label := mrTargetLabel(m, href)
			note := ""
			if br, ok3 := specString(m, "branch"); ok3 {
				note = br
			}
			out = append(out, escalationTarget{Label: label, Href: href, Kind: "mr", Note: note})
			seen[href] = true
		}
	}

	// Any other URL-valued key, in stable key order.
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		s, ok := specString(m, k)
		if !ok {
			continue
		}
		href, ok := bareFieldURL(strings.TrimSpace(s))
		if !ok || seen[href] {
			continue
		}
		seen[href] = true
		out = append(out, escalationTarget{Label: humanizeFieldKey(k), Href: href, Kind: "link", Note: href})
	}
	return out
}

// specString reads a string-valued key out of a decoded operation_spec.
func specString(m map[string]any, key string) (string, bool) {
	s, ok := m[key].(string)
	if !ok {
		return "", false
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	return s, true
}

// mrTargetLabel builds the "site-deployment!1274" pill label from an MR spec,
// falling back to the URL when project/iid are absent. Only the last path
// segment of the project is used — "example-org/site-deployment" reads as
// "site-deployment", which is how the team names it in Slack (ka-0000000014's
// name-things-by-name spirit).
func mrTargetLabel(m map[string]any, href string) string {
	project, _ := specString(m, "project")
	if project != "" {
		if i := strings.LastIndex(project, "/"); i >= 0 && i+1 < len(project) {
			project = project[i+1:]
		}
	}
	iid := ""
	switch t := m["mr_iid"].(type) {
	case string:
		iid = strings.TrimSpace(t)
	case json.Number:
		iid = t.String()
	}
	switch {
	case project != "" && iid != "":
		return project + "!" + iid
	case iid != "":
		return "!" + iid
	case project != "":
		return project
	}
	return href
}

// parseSpawnCreateSpec pulls project/agent-name/namespace out of a spawn
// escalation's operation_spec, which is the raw agent-bead create request
// body written by handleEscalation (kbeads write_authz.go): the top-level
// `title` is the agent name, and `project`/`role` live nested under
// `fields` — a different shape from the flat MR spec parsed by
// escalationTargetsFromSpec above, so it gets its own decoder. No current
// schema sends a namespace-shaped value (see SpawnNamespace doc comment),
// but several plausible key spellings are checked so the card picks one up
// automatically if a future spec starts sending it.
func parseSpawnCreateSpec(spec string) (project, agentName, namespace string) {
	var body struct {
		Title  string `json:"title"`
		Fields struct {
			Project   string `json:"project"`
			Namespace string `json:"namespace"`
			Target    string `json:"target_namespace"`
			Pod       string `json:"pod_namespace"`
		} `json:"fields"`
	}
	if err := json.Unmarshal([]byte(spec), &body); err != nil {
		return "", "", ""
	}
	agentName = strings.TrimSpace(body.Title)
	project = strings.TrimSpace(body.Fields.Project)
	namespace = firstNonEmptyStr(body.Fields.Namespace, body.Fields.Target, body.Fields.Pod)
	return project, agentName, namespace
}
