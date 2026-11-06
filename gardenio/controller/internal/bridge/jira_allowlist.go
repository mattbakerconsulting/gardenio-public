// Package bridge — per-role Jira field-write allowlist.
//
// Jira's native permission model is operation-level, not field-level: it cannot
// express "this agent may edit field A but not field B". Field-grained control
// therefore lives in the bridge, not in Jira (epic ka-0000004904). This file
// holds the policy primitive: a per-beads-role allowlist of which governed
// outbound writes the bridge will perform, plus a gate that the outbound sync
// path consults to drop/reject out-of-policy writes.
//
// Scope of "field": the bridge has no raw Jira field-edit path. Every outbound
// mutation flows through one of the five JiraClient write methods (AddComment,
// TransitionIssue, AddLabels, AssignIssue, AddRemoteLink). Those five governed
// writes are the units the allowlist keys on — see JiraWriteField. Comments
// carry an extra dimension: JSM distinguishes an internal note from a public
// (customer-facing) reply via the sd.public.comment comment property, and the
// allowlist gates public comments separately (see CommentVisibility).
//
// Invariants (acceptance criteria of ka-0000004121):
//   - FAIL CLOSED: an unknown role, an unknown field, or a field absent from the
//     acting role's policy yields no write. The policy decision functions never
//     default-allow.
//   - This allowlist GATES the still-existing governed PE/DEVOPS writes; it adds
//     no new write surface and re-enables nothing the read-only-mirror contract
//     (advice ka-0000003234) forbids. No role is granted public-comment by
//     default — autonomous agents post internal notes only.
package bridge

import (
	"log/slog"
	"sort"
	"strings"
)

// JiraWriteField identifies one governed outbound write the bridge can make to a
// Jira issue. These map 1:1 onto the JiraClient mutation methods; the allowlist
// is keyed on them because the bridge has no finer-grained field-edit path.
type JiraWriteField string

const (
	// FieldComment — AddComment / AddCommentWithVisibility.
	FieldComment JiraWriteField = "comment"
	// FieldTransition — TransitionIssue (status change).
	FieldTransition JiraWriteField = "transition"
	// FieldLabels — AddLabels.
	FieldLabels JiraWriteField = "labels"
	// FieldAssignee — AssignIssue.
	FieldAssignee JiraWriteField = "assignee"
	// FieldRemoteLink — AddRemoteLink (e.g. a GitLab MR link).
	FieldRemoteLink JiraWriteField = "remotelink"
	// FieldVersions — the Affects-Version write. Under the outbox model (epic
	// ka-0000002067) an fsm-triage versions write is emitted as a GBFSM Set-Version
	// intent rather than a direct SetAffectsVersions call; the field is what the
	// policy gates, and it maps 1:1 to the Set-Version intent type.
	FieldVersions JiraWriteField = "versions"
	// FieldAttachment — AddAttachment, the multipart upload that puts
	// screenshot bytes ON the issue so an ADF media node can render them
	// inline (ka-0000001730). Gated separately from FieldComment because
	// attaching bytes is a materially different act from writing text: an
	// attachment is durable, downloadable, and (unlike a comment) can carry
	// PHI, so a role allowed to comment is not automatically allowed to
	// attach.
	FieldAttachment JiraWriteField = "attachment"
)

// knownJiraWriteFields is the closed universe of governed writes. A field
// outside this set is "unknown" and always fails closed (criterion 4) — this is
// what makes a typo or a future-but-unwired write a no-op rather than a
// silently-permitted bypass.
var knownJiraWriteFields = map[JiraWriteField]bool{
	FieldComment:    true,
	FieldTransition: true,
	FieldLabels:     true,
	FieldAssignee:   true,
	FieldRemoteLink: true,
	FieldVersions:   true,
	FieldAttachment: true,
}

// CommentVisibility distinguishes a JSM internal note from a public,
// customer-facing reply. It maps directly onto the sd.public.comment comment
// property: internal ⇒ {"internal": true}, public ⇒ {"internal": false}.
type CommentVisibility string

const (
	// CommentInternal is an internal note — visible only to agents/staff.
	CommentInternal CommentVisibility = "internal"
	// CommentPublic is a public reply visible to the external customer/reporter.
	CommentPublic CommentVisibility = "public"
)

// knownCommentVisibilities bounds the visibility values the gate accepts; any
// other value fails closed rather than being treated as internal.
var knownCommentVisibilities = map[CommentVisibility]bool{
	CommentInternal: true,
	CommentPublic:   true,
}

// RolePolicy is the set of governed writes a single beads role may perform.
type RolePolicy struct {
	// Fields is the set of write fields permitted for the role. A field absent
	// from this set (or present with value false) is denied.
	Fields map[JiraWriteField]bool
	// AllowPublicComment authorizes the role to post public (customer-facing)
	// JSM replies. When false the role may still post internal notes (if
	// FieldComment is allowed) but never public ones. Defaults to false for
	// every role in DefaultFieldAllowlist — the mirror contract forbids
	// autonomous external replies.
	AllowPublicComment bool
}

// FieldAllowlist maps a beads role name to its write policy. A role absent from
// the map is unknown and denied everything (fail closed).
type FieldAllowlist map[string]RolePolicy

// allFieldsPolicy builds a RolePolicy granting every governed write, internal
// comments only (no public replies). This is the "broad" tier used for the
// autonomous work roles that legitimately drive DEVOPS/PE tickets today.
func allFieldsPolicy() RolePolicy {
	fields := make(map[JiraWriteField]bool, len(knownJiraWriteFields))
	for f := range knownJiraWriteFields {
		fields[f] = true
	}
	return RolePolicy{Fields: fields, AllowPublicComment: false}
}

// internalNoteOnlyPolicy builds a RolePolicy that may post internal notes and
// nothing else — no transitions, labels, assignment, remote links, or public
// replies. This models the restricted, FSM-scoped tier from the epic
// (ka-0000004904): an agent that can leave a status note but cannot otherwise
// mutate the ticket or speak to the customer.
func internalNoteOnlyPolicy() RolePolicy {
	return RolePolicy{
		Fields:             map[JiraWriteField]bool{FieldComment: true},
		AllowPublicComment: false,
	}
}

// triageFieldsPolicy builds the fsm-triage tier: labels + versions ONLY, and no
// comments at all (internal or public). Under the outbox model (epic
// ka-0000002067) the permitted FIELD maps 1:1 to the GBFSM intent type the triage
// path may emit:
//
//   - labels   → Add-Label intents (the classification labels: gardenio-triaged,
//     sev-{0..3}, gxp-{none,low,med,high}), one intent per label.
//   - versions → a Set-Version intent (the Affects-Version).
//
// Deliberately NOT granted: comment (⇒ NO Internal-Comment intent), transition,
// assignee, remotelink. AllowPublicComment is false (belt-and-suspenders:
// FieldComment is absent, so no comment intent can pass anyway). Triage
// classifies the ticket's metadata; it does not speak on the ticket, move it, or
// reassign it. Every other role's surface is unchanged.
func triageFieldsPolicy() RolePolicy {
	return RolePolicy{
		Fields: map[JiraWriteField]bool{
			FieldLabels:   true,
			FieldVersions: true,
		},
		AllowPublicComment: false,
	}
}

// DefaultFieldAllowlist returns the starting per-role policy. It is intentionally
// conservative:
//
//   - The autonomous work roles that already sync to the allowlisted PE/DEVOPS
//     projects (crew, gardener variants, jira-mention, thread, triage) get the
//     broad tier: all governed writes, internal comments only.
//   - The restricted tier (restricted, fsm-agent) may post internal notes only.
//   - fsm-triage gets the triage tier: labels + versions ONLY, no comments — its
//     writes route through the gardenio2 bridge (ka-0000003103 / Option A).
//   - Every other role is unknown ⇒ denied everything (fail closed).
//   - NO role is granted public comments — the epic's hard line ("internal
//     comment yes, external/customer reply no") and the read-only-mirror
//     contract both forbid autonomous customer-facing replies.
//
// This is a sane default, not a frozen contract: callers may construct an
// AllowlistGate with a tuned FieldAllowlist as the fleet's roles evolve.
func DefaultFieldAllowlist() FieldAllowlist {
	broadRoles := []string{
		"crew",
		"gardener",
		"gardener-delivery",
		"gardener-release-owner",
		"jira-mention",
		"thread",
		"triage",
	}
	al := make(FieldAllowlist, len(broadRoles)+3)
	for _, r := range broadRoles {
		al[r] = allFieldsPolicy()
	}
	// Restricted tier: internal status notes only. Kept for fsm-agent and any
	// role that only needs to leave a status note.
	al["restricted"] = internalNoteOnlyPolicy()
	al["fsm-agent"] = internalNoteOnlyPolicy()
	// fsm-triage tier: labels + versions ONLY (epic ka-0000002067). The triage
	// classification (gardenio-triaged/sev-N/gxp-X labels + Affects-Version) is
	// emitted as GBFSM outbox intents (Add-Label + Set-Version) — the bot never
	// touches FSM. The labels/versions FIELDS map 1:1 to those intent types; all
	// three fail-closed gates (project=GBFSM + allowlist + scope) apply. No comment,
	// transition, assignee, or remotelink surface: triage classifies metadata, it
	// does not speak on the ticket.
	al["fsm-triage"] = triageFieldsPolicy()
	return al
}

// AllowlistMode selects how the gate treats a policy denial.
type AllowlistMode int

const (
	// AllowlistModeEnforce drops/rejects denied writes (fail closed). This is
	// the spec behavior and the library default.
	AllowlistModeEnforce AllowlistMode = iota
	// AllowlistModeShadow logs would-be denials but allows the write through.
	// Used for a safe sandbox-first rollout (epic ka-0000004904: validate in
	// acme-sandbox before enforcing in prod) — the operator flips to
	// enforce once the shadow logs look right.
	AllowlistModeShadow
)

// AllowlistModeFromString parses an operator-supplied mode string. Unknown or
// empty values fall back to shadow — the safe rollout default, so a typo in the
// env var never silently switches production into hard enforcement.
func AllowlistModeFromString(s string) AllowlistMode {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "enforce", "enforced", "block", "reject":
		return AllowlistModeEnforce
	default:
		return AllowlistModeShadow
	}
}

// String renders the mode for logs.
func (m AllowlistMode) String() string {
	if m == AllowlistModeEnforce {
		return "enforce"
	}
	return "shadow"
}

// Decision is the pure, mode-independent policy verdict for one write. It always
// fails closed: Allowed is true only when the role is known, the field is known,
// the field is in the role's policy, and (for comments) the visibility is
// permitted.
type Decision struct {
	Allowed    bool
	Role       string
	Field      JiraWriteField
	Visibility CommentVisibility // only meaningful when Field == FieldComment
	Reason     string            // human-readable explanation (for logs/tests)
}

// AllowlistGate evaluates write requests against a FieldAllowlist. The zero value
// is not usable; construct via NewAllowlistGate / DefaultAllowlistGate.
type AllowlistGate struct {
	allow  FieldAllowlist
	mode   AllowlistMode
	logger *slog.Logger
}

// NewAllowlistGate constructs a gate over the given allowlist and mode. A nil
// allowlist denies everything (the safest failure). A nil logger is replaced
// with the slog default.
func NewAllowlistGate(allow FieldAllowlist, mode AllowlistMode, logger *slog.Logger) *AllowlistGate {
	if logger == nil {
		logger = slog.Default()
	}
	if allow == nil {
		allow = FieldAllowlist{}
	}
	return &AllowlistGate{allow: allow, mode: mode, logger: logger}
}

// DefaultAllowlistGate builds an enforcing gate over DefaultFieldAllowlist. The
// library default is enforce so the fail-closed invariant holds out of the box;
// the production wiring opts into shadow for rollout safety.
func DefaultAllowlistGate(logger *slog.Logger) *AllowlistGate {
	return NewAllowlistGate(DefaultFieldAllowlist(), AllowlistModeEnforce, logger)
}

// Mode reports the gate's enforcement mode.
func (g *AllowlistGate) Mode() AllowlistMode { return g.mode }

// Roles returns the sorted set of roles the gate knows about (for diagnostics).
func (g *AllowlistGate) Roles() []string {
	out := make([]string, 0, len(g.allow))
	for r := range g.allow {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// decideField is the pure policy verdict for a non-comment write (or a comment
// considered only as the "may this role comment at all" question). It never
// default-allows: unknown role, unknown field, or a field outside the role's
// policy all return Allowed=false.
func (g *AllowlistGate) decideField(role string, field JiraWriteField) Decision {
	d := Decision{Role: role, Field: field}

	role = strings.TrimSpace(role)
	if role == "" {
		d.Reason = "empty role"
		return d
	}
	pol, ok := g.allow[role]
	if !ok {
		d.Reason = "unknown role"
		return d
	}
	if !knownJiraWriteFields[field] {
		d.Reason = "unknown field"
		return d
	}
	if !pol.Fields[field] {
		d.Reason = "field not in role allowlist"
		return d
	}
	d.Allowed = true
	d.Reason = "allowed"
	return d
}

// decideComment is the pure policy verdict for a comment write of a given
// visibility. It first requires that the role may comment at all, then gates
// public comments on the role's AllowPublicComment flag. An unrecognized
// visibility fails closed.
func (g *AllowlistGate) decideComment(role string, vis CommentVisibility) Decision {
	d := g.decideField(role, FieldComment)
	d.Visibility = vis
	if !d.Allowed {
		return d
	}
	if !knownCommentVisibilities[vis] {
		d.Allowed = false
		d.Reason = "unknown comment visibility"
		return d
	}
	if vis == CommentPublic {
		// decideField already proved the role exists.
		if !g.allow[role].AllowPublicComment {
			d.Allowed = false
			d.Reason = "public comment not permitted for role"
			return d
		}
	}
	return d
}

// AllowField reports whether a non-comment write should proceed, applying the
// gate's mode and logging any denial. In enforce mode a denied write returns
// false (dropped); in shadow mode it logs and returns true.
func (g *AllowlistGate) AllowField(role string, field JiraWriteField) bool {
	return g.apply(g.decideField(role, field))
}

// AllowComment reports whether a comment write of the given visibility should
// proceed, applying mode + logging. Internal notes pass whenever the role may
// comment; public replies additionally require AllowPublicComment.
func (g *AllowlistGate) AllowComment(role string, vis CommentVisibility) bool {
	return g.apply(g.decideComment(role, vis))
}

// apply turns a policy Decision into a proceed/drop boolean, honoring the mode
// and logging every denial with role+field so bridge escapes are observable
// (criterion 2).
func (g *AllowlistGate) apply(d Decision) bool {
	if d.Allowed {
		return true
	}
	if g.mode == AllowlistModeShadow {
		g.logger.Warn("jira-allowlist: would reject write (shadow mode — allowing)",
			"role", d.Role, "field", d.Field, "visibility", d.Visibility, "reason", d.Reason)
		return true
	}
	g.logger.Warn("jira-allowlist: rejected out-of-policy write",
		"role", d.Role, "field", d.Field, "visibility", d.Visibility, "reason", d.Reason)
	return false
}
