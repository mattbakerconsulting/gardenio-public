// Package bridge — config-driven, project-agnostic Jira write-policy framework.
//
// This is the unification of the two legacy gates (jira_project_gate.go's
// ProjectSyncGate and jira_allowlist.go's AllowlistGate) into a single policy
// keyed on (project, role, field, visibility), driven by data rather than
// compiled-in constants (epic ka-0000003475, task ka-0000007252).
//
// # Why
//
// The legacy design special-cases FSM in three hardcoded places: the
// allow/block project sets (jira_mentions.go), a `const fsmProjectKey = "FSM"`
// plus a bespoke toggle branch (jira_project_gate.go), and hardcoded role
// tiers (jira_allowlist.go). That means field-level write control is FSM-only
// and lives in Go source. This framework makes the policy a value: any project
// can carry a per-role field allowlist and an atomic sync toggle, and the
// policy can be shipped/edited as data (the design routes it through the
// existing jira-poller-style config bead with SSE hot-reload).
//
// # Invariants (carried over from the legacy gates — all fail-closed)
//
//   - Deny-by-default: a project must be present AND writable; a role must
//     resolve to a policy; a field must be in that policy; a comment's
//     visibility must be permitted. Any miss denies.
//   - No role is granted public (customer-facing) comments by default.
//   - enforce drops denied writes; shadow logs-but-allows (safe rollout).
//   - A nil store denies everything.
//   - This is orthogonal to the bead→ticket scope guard (jira_scope.go), which
//     is still ANDed by the callers.
//
// # Outbox pivot (epic ka-0000002067)
//
// FSM is no longer a writable project in any form — not directly, not behind a
// toggle. The gardenio bot holds ZERO FSM write permission at the Jira layer, and
// every FSM-bound write is redirected to an append-only intent in the internal
// "Gardenio FSM Outbox" (GBFSM) project, which operator-owned Automation rules apply
// to the real FSM ticket. So the writable set is {PE, DEVOPS, GBFSM}: PE/DEVOPS
// always writable, GBFSM writable only behind the "outbox" toggle (JIRA_OUTBOX_
// ENABLED, default OFF, shadow-first until E2E gate ka-0000000600 passes). The
// former "fsm" toggle and the FSM project entry are removed permanently; the FSM
// key resolves to "unknown project" and is denied in every toggle state.
package bridge

import (
	"log/slog"
	"maps"
	"sort"
	"strings"
)

// ProjectPolicy is the write policy for a single Jira project: whether the
// bridge may write to it at all (optionally gated by a named toggle), and the
// per-role field allowlist that applies to writes on this project.
type ProjectPolicy struct {
	// Writable is the deny-by-default master switch for the project. False
	// means the bridge never writes to this project regardless of role.
	Writable bool
	// Spawnable, when true, makes the project @gardenio-mention SPAWN-eligible
	// (and, by the shared projectSpawnable predicate, reply-eligible) WITHOUT
	// making it outbound-Writable. It is the axis that widens the mention
	// surface (ka-0000005656) without also opening the poller/sync-back/squawk
	// write paths, which key on Writable alone. A Writable project is always
	// spawnable too, so PE/DEVOPS/GBFSM (Writable:true) need not set this; it is
	// for mention-only projects added via JIRA_SPAWN_PROJECTS, expressed as
	// {Spawnable:true, Writable:false}. FSM and unknown projects set neither and
	// stay denied on both axes.
	Spawnable bool
	// SyncToggle, when non-empty, names an entry in WritePolicy.Toggles that
	// must also be enabled for the project to be writable. The GBFSM outbox is
	// expressed as {Writable:true, SyncToggle:"outbox"} so it is writable only
	// when the "outbox" toggle is on — letting the sandbox→prod cutover flip one
	// switch atomically. Empty means "no extra gate" (writable iff Writable).
	SyncToggle string
	// Roles maps a beads role name to its field allowlist FOR THIS PROJECT. A
	// role absent here falls back to DefaultRole; if that is nil too, the role
	// is unknown and denied (fail closed).
	Roles map[string]RolePolicy
	// DefaultRole, when non-nil, is the policy applied to roles not present in
	// Roles. Nil (the default) means unlisted roles are denied.
	DefaultRole *RolePolicy
}

// WritePolicy is the whole config-driven write policy: the per-project
// policies, the named toggles they reference, and the enforcement mode.
type WritePolicy struct {
	// Projects maps a Jira project key (e.g. "PE", "GBFSM") to its policy. A
	// project absent here is unknown and denied (deny-by-default). FSM is
	// deliberately absent — it is never writable (epic ka-0000002067).
	Projects map[string]ProjectPolicy
	// Toggles holds named on/off switches referenced by ProjectPolicy.SyncToggle
	// (e.g. {"outbox": false}). A toggle referenced but absent is treated as off.
	Toggles map[string]bool
	// Mode selects enforce (drop denied writes) vs shadow (log-but-allow).
	Mode AllowlistMode
}

// projectWritable reports whether project may be written to under this policy:
// it must be present, Writable, and (if it names a toggle) that toggle on.
func (p WritePolicy) projectWritable(project string) bool {
	pp, ok := p.Projects[project]
	if !ok || !pp.Writable {
		return false
	}
	if pp.SyncToggle != "" && !p.Toggles[pp.SyncToggle] {
		return false
	}
	return true
}

// projectSpawnable reports whether an agent may be SPAWNED for a mention on
// project — a strictly weaker gate than projectWritable: the project must be
// present and either Writable OR Spawnable, but its SyncToggle is IGNORED.
// Spawning an agent from a @gardenio mention performs no outbound Jira write (the
// mention path holds no JiraClient — TestJiraMention_NoJiraWriteSurface), so it
// must not be coupled to the write toggle that arms the outbox. GBFSM
// ({Writable:true, SyncToggle:"outbox"}) is therefore spawn-eligible in BOTH
// toggle states. A mention-only project ({Spawnable:true, Writable:false},
// added via JIRA_SPAWN_PROJECTS) is spawn-eligible without being outbound-
// Writable — projectWritable still returns false for it, so the poller/
// sync-back/squawk paths never write there (ka-0000005656). FSM and unknown
// projects — absent from Projects — remain denied (ka-0000001940).
func (p WritePolicy) projectSpawnable(project string) bool {
	pp, ok := p.Projects[project]
	return ok && (pp.Writable || pp.Spawnable)
}

// resolveRole returns the policy that applies to role on this project: the
// explicit entry if present, else DefaultRole. ok is false when neither exists
// (the role is unknown ⇒ fail closed).
func (pp ProjectPolicy) resolveRole(role string) (RolePolicy, bool) {
	if rp, ok := pp.Roles[role]; ok {
		return rp, true
	}
	if pp.DefaultRole != nil {
		return *pp.DefaultRole, true
	}
	return RolePolicy{}, false
}

// outboxToggle is the name of the coordinated toggle gating GBFSM writability.
// It replaces the removed "fsm" toggle (epic ka-0000002067). Wired from
// JIRA_OUTBOX_ENABLED (default OFF, shadow-first).
const outboxToggle = "outbox"

// DefaultWritePolicy returns the starting policy for the outbox model. The
// writable set is {PE, DEVOPS, GBFSM}: PE and DEVOPS are always writable, the GBFSM
// outbox is writable only when outboxEnabled (the "outbox" toggle). FSM is NOT
// in the set — it is never writable, in any toggle state (the bot holds no FSM
// write permission; FSM-bound writes flow through GBFSM intents instead). Every
// writable project shares the per-role tiers from DefaultFieldAllowlist, so a
// role's permitted FIELD on GBFSM is exactly the intent it may emit: fsm-triage's
// {labels, versions} tier maps to Add-Label + Set-Version intents (no comment
// intent). Mode defaults to enforce.
func DefaultWritePolicy(outboxEnabled bool) WritePolicy {
	// Same role tiers the legacy AllowlistGate uses, shared (read-only) across
	// projects so a role's field surface is uniform whether it writes PE/DEVOPS
	// directly or emits a GBFSM outbox intent.
	tiers := map[string]RolePolicy(DefaultFieldAllowlist())
	proj := func(toggle string) ProjectPolicy {
		return ProjectPolicy{Writable: true, SyncToggle: toggle, Roles: tiers}
	}
	return WritePolicy{
		Projects: map[string]ProjectPolicy{
			"PE":             proj(""),
			"DEVOPS":         proj(""),
			outboxProjectKey: proj(outboxToggle), // GBFSM — behind the outbox toggle
		},
		Toggles: map[string]bool{outboxToggle: outboxEnabled},
		Mode:    AllowlistModeEnforce,
	}
}

// WithSpawnProjects returns p with each key in projects folded in as a
// mention-only spawn-eligible project — {Spawnable:true, Roles:tiers} — WITHOUT
// making it outbound-Writable. It is how JIRA_SPAWN_PROJECTS widens the mention
// surface at the bridge wiring layer without touching DefaultWritePolicy's
// signature (17+ probe call-sites pass DefaultWritePolicy(false) directly).
// Keys are normalized via jiraProjectKey and trimmed; empty entries are
// skipped. A key already present (PE/DEVOPS/GBFSM) is left UNCHANGED — its
// existing Writable/SyncToggle policy wins, so this never downgrades a writable
// project to spawn-only nor grafts the tiers onto one that already has them.
// tiers is the per-role field allowlist the reply path needs (AllowReplyComment
// → resolveRole); pass DefaultFieldAllowlist() so an internal reply on the new
// project resolves a role instead of failing closed. p's Projects map is copied
// (never mutated in place) so callers sharing the base policy are unaffected.
func (p WritePolicy) WithSpawnProjects(projects []string, tiers map[string]RolePolicy) WritePolicy {
	if len(projects) == 0 {
		return p
	}
	next := make(map[string]ProjectPolicy, len(p.Projects)+len(projects))
	maps.Copy(next, p.Projects)
	for _, raw := range projects {
		key := jiraProjectKey(strings.TrimSpace(raw))
		if key == "" {
			continue
		}
		if _, exists := next[key]; exists {
			continue // existing PE/DEVOPS/GBFSM policy wins — never downgrade
		}
		next[key] = ProjectPolicy{Spawnable: true, Roles: tiers}
	}
	p.Projects = next
	return p
}

// WritePolicyStore evaluates outbound Jira writes against a WritePolicy. The
// zero value is not usable; construct via NewWritePolicyStore. A nil store
// denies everything (the safest failure).
type WritePolicyStore struct {
	policy WritePolicy
	logger *slog.Logger
}

// NewWritePolicyStore builds a store over policy. A nil logger is replaced with
// the slog default.
func NewWritePolicyStore(policy WritePolicy, logger *slog.Logger) *WritePolicyStore {
	if logger == nil {
		logger = slog.Default()
	}
	return &WritePolicyStore{policy: policy, logger: logger}
}

// Mode reports the store's enforcement mode (shadow for a nil store).
func (s *WritePolicyStore) Mode() AllowlistMode {
	if s == nil {
		return AllowlistModeShadow
	}
	return s.policy.Mode
}

// Projects returns the sorted set of project keys the policy knows (diagnostics).
func (s *WritePolicyStore) Projects() []string {
	if s == nil {
		return nil
	}
	out := make([]string, 0, len(s.policy.Projects))
	for k := range s.policy.Projects {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// decide is the pure, mode-independent verdict for one write. isComment selects
// whether visibility is checked. useSpawnGate relaxes ONLY the project axis from
// projectWritable to projectSpawnable (SyncToggle ignored) — the reply/comment
// path sets it because an internal reply on the ticket is not an FSM/outbox
// write and must not be coupled to the outbox toggle (ka-0000007628), exactly as
// AllowSpawn decoupled spawn from the same toggle (ka-0000001940). It leaves
// unknown-project, role, field-allowlist, and public-comment ceiling checks
// byte-for-byte unchanged, so it can never widen role/field/visibility. It never
// default-allows.
func (s *WritePolicyStore) decide(jiraKey, role string, field JiraWriteField, vis CommentVisibility, isComment, useSpawnGate bool) Decision {
	d := Decision{Role: role, Field: field, Visibility: vis}
	if s == nil {
		d.Reason = "nil policy store"
		return d
	}
	project := jiraProjectKey(strings.TrimSpace(jiraKey))
	if project == "" {
		d.Reason = "empty or malformed jira key"
		return d
	}
	pp, ok := s.policy.Projects[project]
	if !ok {
		d.Reason = "unknown project"
		return d
	}
	// Project axis: writable by default; the reply path uses the strictly-weaker
	// spawnable predicate so GBFSM (Writable, SyncToggle:"outbox") is reply-eligible
	// in both toggle states while FSM/unknown (absent) stay denied.
	projectOK := s.policy.projectWritable(project)
	if useSpawnGate {
		projectOK = s.policy.projectSpawnable(project)
	}
	if !projectOK {
		d.Reason = "project not writable (or sync toggle off)"
		return d
	}
	role = strings.TrimSpace(role)
	if role == "" {
		d.Reason = "empty role"
		return d
	}
	pol, ok := pp.resolveRole(role)
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
	if isComment {
		if !knownCommentVisibilities[vis] {
			d.Reason = "unknown comment visibility"
			return d
		}
		if vis == CommentPublic && !pol.AllowPublicComment {
			d.Reason = "public comment not permitted for role"
			return d
		}
	}
	d.Allowed = true
	d.Reason = "allowed"
	return d
}

// AllowProject reports whether the bridge may write to the project owning
// jiraKey at all. Mirrors the legacy ProjectSyncGate.AllowProject so it can
// replace it 1:1 at call sites that gate the project separately.
func (s *WritePolicyStore) AllowProject(jiraKey string) bool {
	if s == nil {
		return false
	}
	return s.policy.projectWritable(jiraProjectKey(strings.TrimSpace(jiraKey)))
}

// AllowSpawn reports whether an agent may be spawned for a @gardenio mention on
// the project owning jiraKey. It is the SPAWN-eligibility gate — deliberately
// decoupled from AllowProject (the WRITE-eligibility gate): the project must be
// present and Writable, but its outbox SyncToggle is ignored. Spawning from a
// mention performs no outbound Jira write (the mention path holds no JiraClient,
// asserted by TestJiraMention_NoJiraWriteSurface), so gating it on the write
// toggle wrongly blocked GBFSM mentions in prod whenever JIRA_OUTBOX_ENABLED was
// off. GBFSM is now spawn-eligible in both toggle states; FSM and unknown
// projects (absent from the policy) stay denied (fail closed). A nil store denies
// everything (ka-0000001940).
func (s *WritePolicyStore) AllowSpawn(jiraKey string) bool {
	if s == nil {
		return false
	}
	return s.policy.projectSpawnable(jiraProjectKey(strings.TrimSpace(jiraKey)))
}

// AllowReplyProject is the mode-independent project pre-gate for the reply path,
// the reply-side analog of AllowProject: a bound-ticket internal reply is
// permitted only on a reply-eligible project. It shares AllowSpawn's predicate
// (projectSpawnable — present and Writable, SyncToggle IGNORED) because a reply,
// like a spawn, is not an FSM/outbox write and must not be coupled to the outbox
// toggle (ka-0000007628). Reply and spawn are distinct operations that happen to
// share this eligibility predicate today; TestAllowReplyProject_MirrorsAllowSpawn
// pins the parity so they cannot silently diverge. Being mode-independent (never
// routed through apply), this verdict is NOT softened by shadow mode — so the
// reply resolver consulting it FIRST keeps FSM/unknown projects denied in every
// mode, exactly as the write path relies on AllowProject. A nil store denies
// everything (fail closed).
func (s *WritePolicyStore) AllowReplyProject(jiraKey string) bool {
	if s == nil {
		return false
	}
	return s.policy.projectSpawnable(jiraProjectKey(strings.TrimSpace(jiraKey)))
}

// AllowField reports whether a non-comment write of field, by role, to the
// project owning jiraKey should proceed — the full verdict (project + role +
// field), with mode + logging applied.
func (s *WritePolicyStore) AllowField(jiraKey, role string, field JiraWriteField) bool {
	return s.apply(s.decide(jiraKey, role, field, "", false, false))
}

// AllowComment reports whether a comment write of the given visibility, by
// role, to the project owning jiraKey should proceed — the full verdict, with
// mode + logging applied.
func (s *WritePolicyStore) AllowComment(jiraKey, role string, vis CommentVisibility) bool {
	return s.apply(s.decide(jiraKey, role, FieldComment, vis, true, false))
}

// AllowReplyComment reports whether an agent-initiated internal reply (an
// operator-invoked comment on the agent's OWN bound ticket, via `gb jira reply`)
// of the given visibility, by role, to the project owning jiraKey should proceed
// — the full verdict with mode + logging applied, but with the project axis on
// the SPAWN gate (projectSpawnable, SyncToggle ignored) instead of the write
// gate. It is to AllowComment what AllowSpawn is to AllowProject: a reply is not
// an FSM/outbox write, so it must not be gated on the outbox toggle. Role,
// field-allowlist, and the AllowPublicComment ceiling are unchanged from
// AllowComment, so GBFSM internal replies are permitted in both toggle states
// while FSM/unknown projects and public replies stay denied (fail closed).
// A nil store denies everything (ka-0000007628).
func (s *WritePolicyStore) AllowReplyComment(jiraKey, role string, vis CommentVisibility) bool {
	return s.apply(s.decide(jiraKey, role, FieldComment, vis, true, true))
}

// apply turns a Decision into a proceed/drop boolean, honoring the mode and
// logging denials so bridge escapes stay observable.
func (s *WritePolicyStore) apply(d Decision) bool {
	// A nil store denies everything (fail closed) regardless of mode — shadow
	// is a rollout aid for a *configured* store, not a license for the absent
	// one to allow writes.
	if s == nil {
		return false
	}
	if d.Allowed {
		return true
	}
	if s.policy.Mode == AllowlistModeShadow {
		s.logger.Warn("jira-write-policy: would reject write (shadow mode — allowing)",
			"role", d.Role, "field", d.Field, "visibility", d.Visibility, "reason", d.Reason)
		return true
	}
	s.logger.Warn("jira-write-policy: rejected out-of-policy write",
		"role", d.Role, "field", d.Field, "visibility", d.Visibility, "reason", d.Reason)
	return false
}
