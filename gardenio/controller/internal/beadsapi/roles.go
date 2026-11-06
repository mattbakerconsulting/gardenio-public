package beadsapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
)

// roleConfigScore ranks a role bead by how much high-signal override config it
// carries. model, account and harness are the override fields most likely to encode
// operator intent — they determine the LLM cost and provider — and are exactly
// what a silent shadow drops, so a bead that sets any of them outranks one that sets
// neither. This is the discriminator the bare lowest-bead-ID winner ignored: a
// model-less duplicate created earlier would shadow a later model-pinned bead,
// so gardener-delivery ran opus despite a haiku pin (bug ka-0000004910). Other
// override fields (resources, sidecars, session_resume) deliberately do NOT
// contribute — they were present on the shadowing bead in that incident, so
// scoring on field *count* would have re-picked the wrong winner.
func roleConfigScore(fields map[string]string) int {
	score := 0
	if strings.TrimSpace(fields["model"]) != "" {
		score++
	}
	if strings.TrimSpace(fields["account"]) != "" {
		score++
	}
	if strings.TrimSpace(fields["harness"]) != "" {
		score++
	}
	return score
}

// roleCandidate is the running winner for a role name while ListRoleBeads folds
// over duplicate-title beads. The winner is chosen deterministically —
// independent of daemon return order — by roleCandidateBeats.
type roleCandidate struct {
	id        string
	score     int
	updatedAt time.Time
}

// roleCandidateBeats reports whether cand should replace the incumbent winner
// for a duplicate role name. Ordering (all deterministic, total because bead
// IDs are unique):
//  1. higher roleConfigScore wins — a model/account-bearing bead is never
//     shadowed by one that sets neither (the ka-0000004910 fix);
//  2. then the more recently updated bead wins — a fresher operator edit
//     reflects newer intent;
//  3. then the lexicographically-lower bead ID wins — the original stable
//     tiebreak, preserved so equal-signal duplicates still resolve the same way
//     on every refresh (no intermittent blanking of a role's overrides).
func roleCandidateBeats(cand, prior roleCandidate) bool {
	if cand.score != prior.score {
		return cand.score > prior.score
	}
	if !cand.updatedAt.Equal(prior.updatedAt) {
		return cand.updatedAt.After(prior.updatedAt)
	}
	return cand.id < prior.id
}

// parseTriBool parses a tri-state boolean field where empty means "unset"
// (caller falls back to a default), "true" / "1" returns &true, and any
// other non-empty value (including "false" / "0") returns &false.
// Used for fields where we need to distinguish "not configured" from "false".
func parseTriBool(raw string) *bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	v := raw == "true" || raw == "1"
	return &v
}

// parseStringList parses a field that may be either a JSON array of strings
// or a comma-separated string. Whitespace around entries is trimmed and empty
// entries are dropped. Returns nil for empty input or unparseable values.
func parseStringList(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if strings.HasPrefix(raw, "[") {
		var list []string
		if err := json.Unmarshal([]byte(raw), &list); err == nil {
			out := make([]string, 0, len(list))
			for _, s := range list {
				if s = strings.TrimSpace(s); s != "" {
					out = append(out, s)
				}
			}
			if len(out) == 0 {
				return nil
			}
			return out
		}
		// Fall through to comma-split on malformed JSON.
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, s := range parts {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// RoleInfo represents a role from a daemon type:role config bead.
// Roles provide pod-level overrides that sit between project and agent
// specificity in the layering order: global < project < role < agent.
type RoleInfo struct {
	Name           string // Role name (from bead title)
	Image          string // Override agent image for this role
	StorageClass   string // Override PVC storage class
	ServiceAccount string // Override K8s ServiceAccount

	// RuntimeClassName pins pods with this role to a Kubernetes RuntimeClass
	// (e.g. "runsc" for gVisor sandboxing). Empty = inherit the cluster default
	// runtime. Parsed from the "runtime_class" field on role beads. Used by the
	// gVisor freeze/thaw PoC to isolate test pods on a sandboxed runtime without
	// touching prod thread/gardener/crew pods.
	RuntimeClassName string
	DockerEnabled    bool // Enable Docker-in-Docker sidecar
	PostgresEnabled  bool // Enable PostgreSQL sidecar
	NeedsK8sAPI      bool // Mount ServiceAccount token (default false; see ka-0000006344)
	// Verifier is the "verifier" field: the role's pods get the chart's
	// read-only verifier ServiceAccount and its token (config
	// VerifierServiceAccount) unless the role names its own service_account.
	// P9.2, gd-6ujfeTZfNL.
	Verifier bool

	// MachineClient is the "machine_client" field: a Keycloak machine client
	// whose secret the controller mounts into this role's pods, only when the
	// id is on GARDENIO_AGENT_MACHINE_CLIENT_MOUNTS (escalation routing Task
	// 15, C5).
	MachineClient string

	// OpenZiti per-pod overlay access (epic ka-0000000990). Parsed from the
	// "ziti_*" fields on role beads. Role-level settings take precedence over
	// project-level (global < project < role < agent), mirroring the other
	// sidecar toggles.
	ZitiAccessEnabled bool   // "ziti_access" == "true"
	ZitiServiceID     string // "ziti_service_id" — resolved OpenZiti service id
	ZitiBind          bool   // "ziti_bind" == "true" (default Dial)
	ZitiTTL           string // "ziti_ttl" — requested lease duration
	ZitiOptional      bool   // "ziti_optional" == "true" — failed mint is non-fatal

	// ExecutionTarget selects the runtime backend for agents with this role.
	// "" / "kubernetes" (default) follows the existing pod path; "lambda_microvm"
	// routes to the AWS Lambda MicroVM backend (Network Fabric handoff nfd!15,
	// epic ka-0000005010). Role takes precedence over project; per-agent metadata
	// overrides both (precedence: global < project < role < agent). Parsed from
	// the "execution_target" field on role beads — this is the natural home for
	// opting the gardenio-microvms role into MicroVM execution.
	ExecutionTarget string

	// SessionResume controls whether agents with this role attempt to resume
	// their previous coop session on pod restart. nil = inherit project setting
	// (which itself defaults to true when a workspace PVC is mounted);
	// &true / &false = explicit opt-in/out. Role takes precedence over project.
	// Parsed from the "session_resume" field on role beads.
	SessionResume *bool

	// Ultracode opts agents with this role into (or out of) ultracode/workflow
	// mode. nil = unset (inherit project, then leave the pod's resolved
	// claude-settings alone); &true / &false = explicit opt-in/out. Role takes
	// precedence over project; per-agent metadata overrides both (precedence:
	// global < project < role < agent). Parsed from the "ultracode" field on
	// role beads. This is the natural home for opting the gardenio-microvms role
	// into ultracode so its role-default MicroVM agents get workflows+subagents
	// without touching the --ultracode Slack flag (ka-0000001714). Ultracode is
	// genuinely tri-state, so an unset layer is SKIPPED (not read as off) when
	// resolving — otherwise a role opt-in could never override a project default.
	Ultracode *bool

	// Resource overrides (Kubernetes quantity strings).
	CPURequest    string
	CPULimit      string
	MemoryRequest string
	MemoryLimit   string

	// Per-role secret overrides.
	Secrets []SecretEntry
	// Per-role plain env vars.
	EnvVars []EnvEntry
	// Per-role extra env vars from env_json field.
	EnvOverrides map[string]string
	// Per-role Claude config overrides.
	ClaudeConfig map[string]json.RawMessage

	// Account references a type:account bead by name. When set, overrides the
	// project's account for agents spawned with this role — useful for pinning
	// a role (e.g. fsm-triage) to a specific LLM provider regardless of which
	// project it runs in.
	Account string

	// Model overrides the model for agents spawned with this role, independent
	// of which account they use. Applied after applyAccountConfig in the
	// precedence chain global < project < role < agent, so a single account
	// (one OAuth cred, one LiteLLM key allowing N models) can back fable/opus/
	// sonnet roles by setting model on the role bead — no account cloning
	// (epic ka-0000005488). Empty = inherit the account's default model.
	// Parsed from the "model" field on role beads.
	Model string

	// Harness selects the agent CLI pods with this role run ("claude" or
	// "codex"). Empty = inherit the project's harness, then the controller
	// default (claude). Role supersedes project; per-agent metadata supersedes
	// both. Parsed (unvalidated) from the "harness" field on role beads; the
	// controller validates it at spawn so a bad value fails that spawn with a
	// readable error instead of blanking the whole role cache.
	Harness string

	// AllowedHarnesses lists extra agent CLIs ("claude", "codex", "gemini") a per-spawn
	// Slack `--harness`/`--model codex` may select on this role, beyond the
	// role's own resolved harness (Harness, else the project's, else claude).
	// Empty = the role is pinned to its resolved harness (so a default role is
	// claude-only). Parsed from the comma-separated "allowed_harnesses" field;
	// unknown names are dropped here and re-checked by the bridge.
	AllowedHarnesses []string

	// AccountWhitelist restricts which accounts pods with this role may use.
	// When non-empty, the project's round-robin account pool is intersected
	// with this list at pod-spawn time; if the intersection is empty the pod
	// starts without BOAT_ACCOUNT (fail-closed). Empty means no restriction.
	AccountWhitelist []string

	// NoAccountSpill opts a role OUT of the role-pin exhaustion spill: when its
	// pinned account is capacity-exhausted at spawn and the project has other
	// healthy accounts, the controller normally spills to the healthy spread
	// pool instead of parking the agent queued. A role that MUST stay on its
	// pinned account sets no_account_spill=true to keep block-and-wait behavior.
	// Empty/false = spill allowed (the safer default, ka-0000001307). Parsed from
	// the "no_account_spill" field on role beads.
	NoAccountSpill bool

	// EnabledSkills and DisabledSkills are the per-role skill toggle (Adam
	// feedback #1, epic ka-0000004989). They name the LINKED skill name Claude
	// Code discovers under .claude/skills/ — primary-repo skills by their bare
	// name, sibling-repo skills prefixed "{repo}-" (symlinkClaudeExtensions).
	//
	// EnabledSkills is an ALLOW-LIST: when non-empty, ONLY the named skills are
	// linked into the workspace. Empty means no restriction — every discovered
	// skill is enabled by default (mirrors AccountWhitelist's empty=unrestricted).
	// DisabledSkills is a DENY-LIST: a named skill is never linked, even if it
	// also appears in EnabledSkills (deny wins).
	//
	// These are consumed IN-POD by `gb setup claude` (symlinkClaudeExtensions),
	// which reads role beads directly from the daemon — they are NOT propagated
	// through the controller's pod spec, so config.RoleInfo intentionally omits
	// them. Parsed leniently (JSON array OR comma-separated string) like
	// AccountWhitelist.
	EnabledSkills  []string
	DisabledSkills []string

	// EnabledAgents and DisabledAgents are the per-role sub-agent toggle
	// (ka-0000006908) — the "agents" subdir analogue of the skill toggle, with
	// identical semantics: EnabledAgents is an allow-list (non-empty = ONLY the
	// named agents link; empty = all discovered agents enabled), DisabledAgents
	// is a deny-list (deny wins). They name the LINKED agent file Claude Code
	// discovers under .claude/agents/ — primary-repo entries by their bare file
	// name (e.g. "reviewer.md"), sibling-repo entries prefixed "{repo}-". Like
	// the skill toggle they are consumed in-pod by `gb setup claude` and NOT
	// propagated through the controller's pod spec. Lets a terminal Bash-only
	// role (e.g. a mechanical cron gardener) shed repo-provided sub-agents it
	// never spawns; note it only gates repo-linked agents, not the harness
	// built-ins (Explore/Plan/general-purpose/...).
	EnabledAgents  []string
	DisabledAgents []string

	// Privileged mirrors the daemon's spawn-authz gate (roleIsPrivileged,
	// kbeads/internal/server/spawn_authz.go): true when a non-human caller
	// spawning this role must go through the escalation-approve path rather
	// than spawning directly. Parsed from the "privileged" field on role
	// beads. Read-only mirror for display (e.g. the agent detail page's
	// spawn-provenance badge, gd-jbY5UiWlN4) — the daemon remains the sole
	// authority that enforces it.
	Privileged bool
}

// ListRoleBeads queries the daemon for role beads (type=role) and extracts
// role metadata from fields. Returns a map of role name -> RoleInfo.
func (c *Client) ListRoleBeads(ctx context.Context) (map[string]RoleInfo, error) {
	resp, err := c.listBeads(ctx, []string{"role"}, activeStatuses)
	if err != nil {
		return nil, fmt.Errorf("listing role beads: %w", err)
	}

	roles := make(map[string]RoleInfo)
	// seen maps a role name to the candidate bead that currently owns it, so a
	// duplicate type:role title resolves deterministically (see
	// roleCandidateBeats: config-signal, then recency, then lowest bead ID)
	// instead of letting map iteration order decide — mirrors the project
	// duplicate-name guard in projects.go.
	seen := make(map[string]roleCandidate)
	for _, b := range resp.Beads {
		name := b.Title
		if name == "" {
			continue
		}
		fields := b.fieldsMap()
		info := RoleInfo{
			Name:              name,
			Image:             fields["image"],
			StorageClass:      fields["storage_class"],
			ServiceAccount:    fields["service_account"],
			RuntimeClassName:  fields["runtime_class"],
			DockerEnabled:     fields["docker"] == "true",
			PostgresEnabled:   fields["postgres"] == "true",
			NeedsK8sAPI:       fields["needs_k8s_api"] == "true",
			Verifier:          fields["verifier"] == "true",
			MachineClient:     strings.TrimSpace(fields["machine_client"]),
			ZitiAccessEnabled: fields["ziti_access"] == "true",
			ZitiServiceID:     fields["ziti_service_id"],
			ZitiBind:          fields["ziti_bind"] == "true",
			ZitiTTL:           fields["ziti_ttl"],
			ZitiOptional:      fields["ziti_optional"] == "true",
			ExecutionTarget:   fields["execution_target"],
			SessionResume:     parseTriBool(fields["session_resume"]),
			Ultracode:         parseTriBool(fields["ultracode"]),
			CPURequest:        fields["cpu_request"],
			CPULimit:          fields["cpu_limit"],
			MemoryRequest:     fields["memory_request"],
			MemoryLimit:       fields["memory_limit"],
			Account:           fields["account"],
			Model:             fields["model"],
			Harness:           strings.TrimSpace(fields["harness"]),
			AllowedHarnesses:  parseAllowedHarnesses(fields["allowed_harnesses"]),
			NoAccountSpill:    fields["no_account_spill"] == "true",
			Privileged:        fields["privileged"] == "true",
		}
		// Parse per-role secrets from JSON field.
		if raw := fields["secrets"]; raw != "" {
			var secrets []SecretEntry
			if json.Unmarshal([]byte(raw), &secrets) == nil {
				info.Secrets = secrets
			}
		}
		// Parse per-role plain env vars from JSON field.
		if raw := fields["env"]; raw != "" {
			var envVars []EnvEntry
			if json.Unmarshal([]byte(raw), &envVars) == nil {
				info.EnvVars = envVars
			}
		}
		// Parse env_json field.
		if raw := fields["env_json"]; raw != "" {
			var envMap map[string]string
			if json.Unmarshal([]byte(raw), &envMap) == nil {
				info.EnvOverrides = envMap
			}
		}
		// Parse inline Claude config overrides from JSON field.
		if raw := fields["claude_config"]; raw != "" {
			var cfg map[string]json.RawMessage
			if json.Unmarshal([]byte(raw), &cfg) == nil {
				info.ClaudeConfig = cfg
			}
		}
		// Parse account_whitelist from JSON array or comma-separated string.
		if raw := fields["account_whitelist"]; raw != "" {
			info.AccountWhitelist = parseStringList(raw)
		}
		// Parse the per-role skill toggle (JSON array or comma-separated string).
		if raw := fields["enabled_skills"]; raw != "" {
			info.EnabledSkills = parseStringList(raw)
		}
		if raw := fields["disabled_skills"]; raw != "" {
			info.DisabledSkills = parseStringList(raw)
		}
		// Parse the per-role sub-agent toggle (same lenient shapes).
		if raw := fields["enabled_agents"]; raw != "" {
			info.EnabledAgents = parseStringList(raw)
		}
		if raw := fields["disabled_agents"]; raw != "" {
			info.DisabledAgents = parseStringList(raw)
		}
		cand := roleCandidate{
			id:        b.ID,
			score:     roleConfigScore(fields),
			updatedAt: parseTimestamp(b.UpdatedAt),
		}
		// Collision guard: if another role bead already claimed this title,
		// keep a deterministic winner (roleCandidateBeats: prefer the bead that
		// carries model/account override config, then the more recently updated
		// bead, then the lowest bead ID) instead of letting map iteration order
		// decide, and warn so an operator removes the stray duplicate. Treating
		// role titles as authoritative vocabulary means a duplicate must resolve
		// the same way on every refresh, otherwise a sparse duplicate could
		// intermittently blank a role's overrides — or, as in bug ka-0000004910,
		// a model-less duplicate could silently shadow an operator's model pin.
		if prior, dup := seen[name]; dup {
			winner, loser := prior, cand
			if roleCandidateBeats(cand, prior) {
				winner, loser = cand, prior
			}
			slog.Warn("duplicate role-name bead: shadowing avoided, picking deterministic winner",
				"role", name, "winner_bead", winner.id, "shadowed_bead", loser.id,
				"winner_config_score", winner.score, "shadowed_config_score", loser.score)
			seen[name] = winner
			// Only overwrite the stored info when the new bead is the winner.
			if winner.id == b.ID {
				roles[name] = info
			}
			continue
		}
		seen[name] = cand
		roles[name] = info
	}

	return roles, nil
}

// ValidateAgentRoles cross-checks the role named by each active type:agent
// bead against the authoritative role vocabulary (the set of type:role bead
// titles in roles) and returns one warning string per agent whose role has no
// matching type:role bead. Roles are kept as free strings — an unknown role is
// a soft referential warning, not an error — so the controller still spawns
// the agent (applyRoleDefaults simply finds no overrides). builtinRoles names
// roles that are first-class in code but may not have a type:role config bead
// seeded; they never trigger a warning. The returned slice is sorted for
// deterministic logging/test output.
//
// Design note (ka-0000001425, design doc §8.5): we deliberately keep role as a
// validated free string rather than a hard enum. Roles are created
// dynamically (init.go seeds role:job/crew/reviewer/gardener
// labels, and operators add type:role beads ad hoc), so a hard enum would
// reject legitimate new roles before their config bead lands. A soft warning
// surfaces typos and missing config beads without blocking spawns.
func ValidateAgentRoles(agents []AgentBead, roles map[string]RoleInfo, builtinRoles map[string]bool) []string {
	var warnings []string
	for _, a := range agents {
		role := strings.TrimSpace(a.Role)
		if role == "" {
			continue
		}
		if _, ok := roles[role]; ok {
			continue
		}
		if builtinRoles != nil && builtinRoles[role] {
			continue
		}
		warnings = append(warnings, fmt.Sprintf(
			"agent %q references role %q with no type:role bead (unknown role vocabulary)",
			a.AgentName, role))
	}
	sort.Strings(warnings)
	return warnings
}
