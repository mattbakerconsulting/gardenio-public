package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strings"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

// builtinRoles mirrors the controller's builtinRoles map
// (controller/cmd/controller/project_role_cache.go). These roles are first-class in
// controller/bridge code (seeded as role:<name> labels and used on the
// pod-spawn path) but may not have a type:role config bead. They never trigger
// an unknown-role warning. TestBuiltinRolesParity asserts this stays in sync
// with the controller copy so the two never silently drift.
//
// Kept as a package-level var (not a const list) so `gb role list` and the
// spawn-time soft warning share ONE source of truth for what counts as a
// builtin — the whole point of this command is to make the role vocabulary
// visible, so it must agree with the controller's own notion of it.
var builtinRoles = map[string]bool{
	"thread":   true,
	"crew":     true,
	"gardener": true,
	"reviewer": true,
	"job":      true,
	// The unified plan-agent role (epic ka-0000000081) — mirrors the
	// controller's entry so a fresh env without the type:role bead
	// (ka-0000003712) doesn't warn on plan-gardener spawns.
	"plan-gardener": true,
}

// roleCmd is the parent for role-vocabulary orchestration. It enumerates the
// role vocabulary (`gb role list`) and scaffolds the decoupled beads a new role
// needs (`gb role scaffold`). Editing existing roles still happens through the
// beads-viewer role hub and `kd`.
var roleCmd = &cobra.Command{
	Use:     "role",
	Short:   "Inspect and scaffold agent roles",
	GroupID: "orchestration",
}

var roleListCmd = &cobra.Command{
	Use:   "list",
	Short: "List the known role vocabulary (builtins + type:role + config-only)",
	Long: `List every role an agent can be spawned with, merging the three sources a role
name can come from:

  - builtin roles  — first-class in controller/bridge code (thread, crew,
    gardener, reviewer, job, plan-gardener); always valid even without a
    config bead.
  - type:role beads — operator-authored pod overrides (image, model, account,
    sidecars) resolved by the controller at spawn time.
  - config-only roles — named only by a role:<name> label on a config bead, with
    no type:role bead. Roles are validated free strings, so these spawn pods
    perfectly well; they simply cannot set pod overrides, and they trip the
    unknown-role spawn warning. They were invisible to this command until
    ka-0000006836 — one such role had spawned 1729 agents unnoticed.

For each role it also reports whether a per-role instructions override exists
(a config:claude-instructions:role:<name> bead). A role WITHOUT one is not an
error — it simply runs on the GLOBAL instruction defaults. The audit flagged
that silent fallback as a footgun: a typo'd or
half-scaffolded role spawns a pod that looks configured but runs generic
instructions. This column surfaces it.`,
	Args: cobra.NoArgs,
	RunE: runRoleList,
}

// role-scaffold flags.
var (
	roleScaffoldModel          string
	roleScaffoldAccount        string
	roleScaffoldImage          string
	roleScaffoldFrom           string
	roleScaffoldEnabled        []string
	roleScaffoldDisabled       []string
	roleScaffoldEnabledAgents  []string
	roleScaffoldDisabledAgents []string
	roleScaffoldNoInstr        bool
	roleScaffoldDryRun         bool
)

var roleScaffoldCmd = &cobra.Command{
	Use:   "scaffold <name>",
	Short: "Stub the type:role + instructions beads a new role needs",
	Long: `Create the decoupled beads a brand-new role needs, in one shot:

  1. a type:role bead (pod overrides — image/model/account/sidecars), and
  2. a config:claude-instructions:role:<name> bead so the role does NOT
     silently fall through to the global instruction defaults.

Both writes are idempotent: an existing bead of either shape is left untouched
and reported as "exists". With --from <role>, the type:role overrides are cloned
from an existing role bead (credentials excluded). Pass --no-instructions to
create only the type:role bead.

  gb role scaffold fsm-triage --model claude-haiku-4-5 --account fsm-anthropic

After scaffolding, spawn an agent with it:

  gb spawn --role fsm-triage`,
	Args: cobra.ExactArgs(1),
	RunE: runRoleScaffold,
}

// roleRow is one row of `gb role list` — a role name plus the facts that decide
// whether spawning it does what the operator expects.
type roleRow struct {
	Name string `json:"name"`
	// Source is "builtin", "bead", "both", or "config-only": where the role's
	// validity comes from. A "builtin" role has no type:role bead (pod overrides
	// are code defaults); a "bead" role is operator-authored; "both" means a
	// config bead augments a builtin. "config-only" is a role that exists ONLY
	// as a role:<name> label on config beads — it spawns pods fine (role is a
	// free string) but has no type:role bead, so it can set no pod overrides and
	// trips the unknown-role spawn warning. See collectRoleRows.
	Source string `json:"source"`
	// HasInstructions is true when a config:claude-instructions:role:<name>
	// bead exists. When false, the role runs on global instruction defaults.
	HasInstructions bool `json:"has_instructions"`
	// Overrides is the compact set of pod overrides the type:role bead sets
	// (image/model/account), for the OVERRIDES column. Empty for a pure builtin.
	Overrides string `json:"overrides,omitempty"`
}

func runRoleList(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()

	rows, err := collectRoleRows(ctx, daemon)
	if err != nil {
		return err
	}

	if jsonOutput {
		printJSON(rows)
		return nil
	}
	printRoleRows(os.Stdout, rows)
	return nil
}

// roleDataSource is the daemon surface `gb role list` needs. Declaring it as an
// interface (satisfied by *beadsapi.Client) keeps collectRoleRows unit-testable
// with a fake, mirroring the configBeadLister pattern.
type roleDataSource interface {
	ListRoleBeads(ctx context.Context) (map[string]beadsapi.RoleInfo, error)
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
}

// collectRoleRows merges the THREE sources a role name can come from — builtins,
// type:role beads, and role:<name> labels on config beads — and annotates each
// with whether it carries a per-role instructions bead. It is best-effort on the
// config lookup: if listing config beads fails, every role is reported as
// instructions-unknown (HasInstructions=false) rather than failing the whole
// command, because the role/builtin enumeration is still useful on its own.
//
// The config-bead layer is a NAME SOURCE, not just an annotation. A role can be
// defined purely by label-scoped config beads (`role:<name>` on a
// claude-instructions bead) with no type:role bead at all; the controller spawns
// it happily because roles are validated free strings (beadsapi.ValidateAgentRoles).
// Listing only builtins + type:role beads therefore UNDER-REPORTS the live role
// vocabulary — the audit found 22 such roles, one of which (gardener-bugfix) had
// spawned 1729 agents while being invisible to this command. Since the whole
// point of `gb role list` is to make the vocabulary visible, a role that really
// spawns agents must appear, flagged as "config-only" so the operator can see it
// sets no pod overrides and will trip the unknown-role spawn warning.
func collectRoleRows(ctx context.Context, src roleDataSource) ([]roleRow, error) {
	roleBeads, err := src.ListRoleBeads(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing role beads: %w", err)
	}

	instr, configRoles := rolesFromConfigBeads(ctx, src)

	// Union of builtin names, type:role bead names, and names that exist only
	// as a role:<name> label on a config bead.
	names := make(map[string]struct{})
	for n := range builtinRoles {
		names[n] = struct{}{}
	}
	for n := range roleBeads {
		names[n] = struct{}{}
	}
	for n := range configRoles {
		names[n] = struct{}{}
	}

	rows := make([]roleRow, 0, len(names))
	for name := range names {
		_, isBuiltin := builtinRoles[name]
		info, hasBead := roleBeads[name]

		source := "bead"
		switch {
		case isBuiltin && hasBead:
			source = "both"
		case isBuiltin:
			source = "builtin"
		case !hasBead:
			// Neither a builtin nor a type:role bead — this name is known only
			// because a config bead is scoped to it.
			source = "config-only"
		}

		row := roleRow{
			Name:            name,
			Source:          source,
			HasInstructions: instr[name],
		}
		if hasBead {
			row.Overrides = summariseRoleOverrides(info)
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	return rows, nil
}

// rolesFromConfigBeads scans the open type=config beads once and returns two
// sets:
//
//	instr — roles with a config:claude-instructions:role:<name> bead, i.e. a
//	        per-role instruction override. The instruction bead is a type=config
//	        bead titled "claude-instructions" carrying a "role:<name>" label
//	        (init_ensure.go parseEntryKey: config:claude-instructions:role:thread
//	        → title "claude-instructions", label ["role:thread"]).
//	all   — every role named by a role:<name> label on ANY config bead, whatever
//	        its title. This is the role-vocabulary source that makes config-only
//	        roles visible; scoping it to claude-instructions beads alone would
//	        still miss a role defined by, say, only a "context" config bead.
//
// One listing serves both, so adding the vocabulary source costs no extra query.
// On a listing error both sets come back empty, so callers degrade to "builtins
// + type:role beads only" rather than crashing — the same fail-open posture the
// instructions column already had.
func rolesFromConfigBeads(ctx context.Context, src roleDataSource) (instr, all map[string]bool) {
	instr, all = map[string]bool{}, map[string]bool{}
	res, err := src.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"config"},
		Statuses: []string{"open"},
		Limit:    500,
	})
	if err != nil || res == nil {
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not list config beads to check role instructions: %v\n", err)
		}
		return instr, all
	}
	for _, b := range res.Beads {
		for _, role := range rolesFromLabels(b.Labels) {
			all[role] = true
			if b.Title == "claude-instructions" {
				instr[role] = true
			}
		}
	}
	return instr, all
}

// rolesFromLabels extracts the role names from a config bead's labels, i.e. the
// suffix of each "role:<name>" label. A bead may legitimately carry no role
// label (a global-scoped override), in which case it contributes nothing.
func rolesFromLabels(labels []string) []string {
	var roles []string
	for _, l := range labels {
		if name := strings.TrimPrefix(l, "role:"); name != l && name != "" {
			roles = append(roles, name)
		}
	}
	return roles
}

// summariseRoleOverrides renders the pod overrides a type:role bead sets into a
// compact "k=v k=v" string for the list's OVERRIDES column. Only the overrides
// most relevant to "will this role spawn the agent I expect" are shown; the full
// set lives on the role hub (/role/<name>).
func summariseRoleOverrides(info beadsapi.RoleInfo) string {
	var parts []string
	if info.Image != "" {
		parts = append(parts, "image="+info.Image)
	}
	if info.Model != "" {
		parts = append(parts, "model="+info.Model)
	}
	if info.Account != "" {
		parts = append(parts, "account="+info.Account)
	}
	return strings.Join(parts, " ")
}

// printRoleRows renders the role table to w.
func printRoleRows(w io.Writer, rows []roleRow) {
	if len(rows) == 0 {
		fmt.Fprintln(w, "no roles found")
		return
	}
	// SOURCE is padded to 11 to fit "config-only" without shunting the columns
	// right of it out of alignment.
	fmt.Fprintf(w, "%-26s  %-11s  %-16s  %s\n", "ROLE", "SOURCE", "INSTRUCTIONS", "OVERRIDES")
	configOnly := 0
	for _, r := range rows {
		instr := "global-default"
		if r.HasInstructions {
			instr = "custom"
		}
		if r.Source == "config-only" {
			configOnly++
		}
		fmt.Fprintf(w, "%-26s  %-11s  %-16s  %s\n", r.Name, r.Source, instr, orDash(r.Overrides))
	}
	fmt.Fprintln(w, "\nRoles marked global-default run on the GLOBAL instruction set (no per-role")
	fmt.Fprintln(w, "override). Use 'gb role scaffold <name>' to add one. Runbook: ka-0000005921.")
	if configOnly > 0 {
		fmt.Fprintf(w, "\n%d role(s) are config-only: known only from a role:<name> label on a config\n", configOnly)
		fmt.Fprintln(w, "bead, with no type:role bead. They spawn (roles are free strings) but can set no")
		fmt.Fprintln(w, "pod overrides and warn on spawn. 'gb role scaffold <name>' adds the missing bead")
		fmt.Fprintln(w, "and leaves existing instructions untouched.")
	}
}

func runRoleScaffold(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	name := strings.TrimSpace(args[0])

	if !isValidRoleName(name) {
		return fmt.Errorf("invalid role name %q — use lowercase letters, digits, and hyphens only", name)
	}

	// Build the type:role override field set. --from clones an existing role
	// bead's overrides first; explicit flags then win over the clone.
	roleFields := map[string]string{}
	if roleScaffoldFrom != "" {
		roles, err := daemon.ListRoleBeads(ctx)
		if err != nil {
			return fmt.Errorf("cloning from role %q: %w", roleScaffoldFrom, err)
		}
		src, ok := roles[roleScaffoldFrom]
		if !ok {
			return fmt.Errorf("cloning from role %q: no type:role bead with that name", roleScaffoldFrom)
		}
		applyRoleSource(roleFields, src)
	}
	if cmd.Flags().Changed("model") {
		setOrDelete(roleFields, "model", roleScaffoldModel)
	}
	if cmd.Flags().Changed("account") {
		setOrDelete(roleFields, "account", roleScaffoldAccount)
	}
	if cmd.Flags().Changed("image") {
		setOrDelete(roleFields, "image", roleScaffoldImage)
	}
	// Per-role skill toggle (Adam feedback #1, epic ka-0000004989). Written as a
	// comma-separated string, which parseStringList (beadsapi.ListRoleBeads)
	// reads back the same as a JSON array. An explicit empty flag clears a
	// cloned value (setOrDelete deletes on empty), matching --model "".
	if cmd.Flags().Changed("enabled-skills") {
		setOrDelete(roleFields, "enabled_skills", strings.Join(roleScaffoldEnabled, ","))
	}
	if cmd.Flags().Changed("disabled-skills") {
		setOrDelete(roleFields, "disabled_skills", strings.Join(roleScaffoldDisabled, ","))
	}
	// Per-role sub-agent toggle (ka-0000006908), same shapes as the skill toggle.
	if cmd.Flags().Changed("enabled-agents") {
		setOrDelete(roleFields, "enabled_agents", strings.Join(roleScaffoldEnabledAgents, ","))
	}
	if cmd.Flags().Changed("disabled-agents") {
		setOrDelete(roleFields, "disabled_agents", strings.Join(roleScaffoldDisabledAgents, ","))
	}

	project := defaultGBProject()
	plan := roleScaffoldPlan{
		Name:           name,
		Project:        project,
		RoleFields:     roleFields,
		WithInstr:      !roleScaffoldNoInstr,
		InstrLabel:     "role:" + name,
		InstrValueJSON: defaultRoleInstructionsJSON(name),
	}

	if roleScaffoldDryRun {
		fmt.Fprintf(os.Stdout, "dry-run: would scaffold role %q (no beads written)\n", name)
		printRoleScaffoldPlan(os.Stdout, plan)
		return nil
	}

	return executeRoleScaffold(ctx, daemon, plan)
}

// roleScaffoldPlan is the resolved, about-to-be-written shape for a scaffold —
// kept as a struct so --dry-run prints exactly what executeRoleScaffold writes.
type roleScaffoldPlan struct {
	Name           string
	Project        string
	RoleFields     map[string]string
	WithInstr      bool
	InstrLabel     string
	InstrValueJSON string
}

// roleScaffoldWriter is the daemon surface the scaffold needs: list (to detect
// existing beads for idempotency) + create.
type roleScaffoldWriter interface {
	ListRoleBeads(ctx context.Context) (map[string]beadsapi.RoleInfo, error)
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
	CreateBead(ctx context.Context, req beadsapi.CreateBeadRequest) (string, error)
}

// executeRoleScaffold creates the type:role bead and (unless suppressed) the
// per-role instructions bead, skipping whichever already exists. It prints a
// per-bead created/exists line and a final next-step hint.
func executeRoleScaffold(ctx context.Context, w roleScaffoldWriter, plan roleScaffoldPlan) error {
	// 1. type:role bead — skip if one already owns this name.
	roles, err := w.ListRoleBeads(ctx)
	if err != nil {
		return fmt.Errorf("checking for existing role bead: %w", err)
	}
	if _, exists := roles[plan.Name]; exists {
		fmt.Fprintf(os.Stdout, "  type:role bead          exists (left untouched)\n")
	} else {
		var fieldsJSON json.RawMessage
		if len(plan.RoleFields) > 0 {
			raw, err := json.Marshal(plan.RoleFields)
			if err != nil {
				return fmt.Errorf("marshalling role fields: %w", err)
			}
			fieldsJSON = json.RawMessage(raw)
		}
		id, err := w.CreateBead(ctx, beadsapi.CreateBeadRequest{
			Title:     plan.Name,
			Type:      "role",
			Kind:      "config",
			CreatedBy: actor,
			Labels:    roleScopeLabels(plan.Project),
			Fields:    fieldsJSON,
		})
		if err != nil {
			return fmt.Errorf("creating role bead: %w", err)
		}
		fmt.Fprintf(os.Stdout, "  type:role bead          created %s\n", id)
	}

	// 2. config:claude-instructions:role:<name> bead — skip if one already
	// carries the role label, so the role does not silently use global defaults.
	if plan.WithInstr {
		exists, err := instructionsBeadExists(ctx, w, plan.Name)
		if err != nil {
			return fmt.Errorf("checking for existing instructions bead: %w", err)
		}
		if exists {
			fmt.Fprintf(os.Stdout, "  instructions bead       exists (left untouched)\n")
		} else {
			id, err := w.CreateBead(ctx, beadsapi.CreateBeadRequest{
				Title:       "claude-instructions",
				Type:        "config",
				Kind:        "config",
				CreatedBy:   actor,
				Description: plan.InstrValueJSON,
				Labels:      []string{plan.InstrLabel},
			})
			if err != nil {
				return fmt.Errorf("creating instructions bead: %w", err)
			}
			fmt.Fprintf(os.Stdout, "  instructions bead       created %s\n", id)
		}
	}

	fmt.Fprintf(os.Stdout, "\nRole %q scaffolded. Spawn an agent with it:\n", plan.Name)
	fmt.Fprintf(os.Stdout, "  gb spawn --role %s\n", plan.Name)
	if !plan.WithInstr {
		fmt.Fprintf(os.Stdout, "\nNote: no instructions bead created (--no-instructions) — agents with this\n")
		fmt.Fprintf(os.Stdout, "role run on the GLOBAL instruction defaults.\n")
	}
	return nil
}

// instructionsBeadExists reports whether an open config bead titled
// "claude-instructions" already carries the role:<name> label.
func instructionsBeadExists(ctx context.Context, w roleScaffoldWriter, role string) (bool, error) {
	res, err := w.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"config"},
		Statuses: []string{"open"},
		Limit:    500,
	})
	if err != nil {
		return false, err
	}
	if res == nil {
		return false, nil
	}
	want := "role:" + role
	for _, b := range res.Beads {
		if b.Title != "claude-instructions" {
			continue
		}
		if slices.Contains(b.Labels, want) {
			return true, nil
		}
	}
	return false, nil
}

// applyRoleSource copies the scaffold-owned pod overrides from an existing role
// into the new role's field map. Like applyProjectSource, it deliberately
// carries only the minimal override set (image/model/account) — never secrets —
// so a clone cannot silently inherit another role's credentials.
func applyRoleSource(fields map[string]string, src beadsapi.RoleInfo) {
	if src.Image != "" {
		fields["image"] = src.Image
	}
	if src.Model != "" {
		fields["model"] = src.Model
	}
	if src.Account != "" {
		fields["account"] = src.Account
	}
	// The per-role skill/agent toggles are not credentials (they name extension
	// dirs), so they are safe to clone — a cloned role inherits the source's
	// enabled/disabled lists unless an explicit --enabled-skills/
	// --disabled-skills/--enabled-agents/--disabled-agents flag overrides.
	// Stored comma-separated, the same shape the scaffold writes.
	if len(src.EnabledSkills) > 0 {
		fields["enabled_skills"] = strings.Join(src.EnabledSkills, ",")
	}
	if len(src.DisabledSkills) > 0 {
		fields["disabled_skills"] = strings.Join(src.DisabledSkills, ",")
	}
	if len(src.EnabledAgents) > 0 {
		fields["enabled_agents"] = strings.Join(src.EnabledAgents, ",")
	}
	if len(src.DisabledAgents) > 0 {
		fields["disabled_agents"] = strings.Join(src.DisabledAgents, ",")
	}
}

// setOrDelete sets key=val, or removes key when val is empty — so an explicit
// `--model ""` clears a cloned value rather than writing an empty field.
func setOrDelete(m map[string]string, key, val string) {
	if strings.TrimSpace(val) == "" {
		delete(m, key)
		return
	}
	m[key] = val
}

// roleScopeLabels returns the label set for a new type:role bead. A role bead is
// scoped under the current project (the gardenio convention mirrored by
// projectScopeLabels). Returns nil when no project is resolvable, leaving the
// role bead globally scoped rather than emitting an empty "project:" label.
func roleScopeLabels(project string) []string {
	if project == "" {
		return nil
	}
	return []string{"project:" + project}
}

// defaultRoleInstructionsJSON returns the seed value for a new role's
// claude-instructions config bead. The instruction config category merges this
// on top of the global defaults by section key (ResolveConfigBeads), so a
// minimal non-empty seed is enough to register the role as having a custom
// override — the operator then edits the bead to specialise it. We seed a
// `lifecycle` section that restates the ephemeral-agent contract scoped to the
// new role, which is the section most roles actually want to override.
func defaultRoleInstructionsJSON(name string) string {
	v := map[string]string{
		"lifecycle": "## " + name + " — Role Lifecycle\n\n" +
			"You are a **" + name + "** agent. Your task is described in your bead;\n" +
			"read it with `kd show`, claim it with `kd claim <id>`, do the work, and\n" +
			"deliver per your role contract — the default is a feature branch + PR (do NOT\n" +
			"push to main), though some publish roles push drafts directly to main. On GitHub,\n" +
			"open the PR with `gb pr open` (a draft) and finish with `gb pr ready`, which files\n" +
			"the merge escalation. When you have done\n" +
			"what you can this turn, call `gb stop` — you are respawned when there is\n" +
			"more for you to do.\n\n" +
			"_Edit this config:claude-instructions:role:" + name + " bead to specialise\n" +
			"the role's commands/workflows/lifecycle; sections you omit fall through to\n" +
			"the global defaults._\n",
	}
	raw, _ := json.Marshal(v)
	return string(raw)
}

// printRoleScaffoldPlan renders the resolved scaffold for --dry-run.
func printRoleScaffoldPlan(w io.Writer, plan roleScaffoldPlan) {
	fmt.Fprintf(w, "  role name:    %s\n", plan.Name)
	fmt.Fprintf(w, "  project:      %s\n", orDash(plan.Project))
	if len(plan.RoleFields) > 0 {
		keys := make([]string, 0, len(plan.RoleFields))
		for k := range plan.RoleFields {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var parts []string
		for _, k := range keys {
			parts = append(parts, k+"="+plan.RoleFields[k])
		}
		fmt.Fprintf(w, "  overrides:    %s\n", strings.Join(parts, " "))
	} else {
		fmt.Fprintf(w, "  overrides:    -\n")
	}
	if plan.WithInstr {
		fmt.Fprintf(w, "  instructions: config:claude-instructions:%s\n", plan.InstrLabel)
	} else {
		fmt.Fprintf(w, "  instructions: (none — runs on global defaults)\n")
	}
}

// isValidRoleName reports whether s is a valid role name (lowercase letters,
// digits, hyphens). Mirrors spawnIsValidAgentName — roles flow into the same
// label/pod-spec machinery, so the same character class applies.
func isValidRoleName(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
			return false
		}
	}
	return true
}

// roleWarning returns a soft warning string when role is neither a builtin nor
// backed by a type:role bead, or the empty string when the role is known. It is
// the CLI mirror of the bridge's validateProject / ValidateAgentRoles soft
// check: an unknown role is NOT rejected (roles are validated free strings by
// design — see beadsapi.ValidateAgentRoles), but the operator gets a one-line
// signal so a typo doesn't silently spawn a misconfigured pod. A daemon error
// degrades to "no warning" (fail-open) so role listing never blocks a spawn.
func roleWarning(ctx context.Context, src roleDataSource, role string) string {
	role = strings.TrimSpace(role)
	if role == "" || builtinRoles[role] {
		return ""
	}
	roles, err := src.ListRoleBeads(ctx)
	if err != nil {
		return "" // fail-open: never block a spawn on a role-listing error
	}
	if _, ok := roles[role]; ok {
		return ""
	}
	// No type:role bead — but the role may still be REAL, defined only by
	// role:<name>-labelled config beads. Those two cases deserve different
	// warnings: a typo is a mistake to fix, a config-only role is a known shape
	// that merely can't carry pod overrides. Reporting the second as "unknown"
	// trains operators to ignore the warning, which then hides the typos too.
	if _, configOnly := rolesFromConfigBeads(ctx, src); configOnly[role] {
		return fmt.Sprintf(
			"note: role %q is config-only — its instructions/context config beads exist, but it has no\n"+
				"  type:role bead, so it can set no pod overrides (image/model/account). Run\n"+
				"  'gb role scaffold %s' to add one (existing instructions are left untouched).",
			role, role)
	}
	return fmt.Sprintf(
		"warning: unknown role %q — not a builtin, no type:role bead, and no role:%s config bead;\n"+
			"  the pod will spawn on global defaults. Run 'gb role list' to see known roles, or\n"+
			"  'gb role scaffold %s' to create one.",
		role, role, role)
}

func init() {
	roleScaffoldCmd.Flags().StringVar(&roleScaffoldModel, "model", "", "model override for the role (e.g. claude-haiku-4-5)")
	roleScaffoldCmd.Flags().StringVar(&roleScaffoldAccount, "account", "", "type:account bead the role pins its LLM credential to")
	roleScaffoldCmd.Flags().StringVar(&roleScaffoldImage, "image", "", "agent image override for the role")
	roleScaffoldCmd.Flags().StringVar(&roleScaffoldFrom, "from", "", "clone pod overrides from an existing role bead (by name)")
	roleScaffoldCmd.Flags().StringSliceVar(&roleScaffoldEnabled, "enabled-skills", nil, "allow-list of skills to link into the workspace (repeatable / comma-separated; empty = all discovered skills enabled)")
	roleScaffoldCmd.Flags().StringSliceVar(&roleScaffoldDisabled, "disabled-skills", nil, "deny-list of skills never linked into the workspace (repeatable / comma-separated; deny wins over the allow-list)")
	roleScaffoldCmd.Flags().StringSliceVar(&roleScaffoldEnabledAgents, "enabled-agents", nil, "allow-list of sub-agents to link into the workspace (repeatable / comma-separated; empty = all discovered agents enabled)")
	roleScaffoldCmd.Flags().StringSliceVar(&roleScaffoldDisabledAgents, "disabled-agents", nil, "deny-list of sub-agents never linked into the workspace (repeatable / comma-separated; deny wins over the allow-list)")
	roleScaffoldCmd.Flags().BoolVar(&roleScaffoldNoInstr, "no-instructions", false, "do not create the per-role instructions bead (role runs on global defaults)")
	roleScaffoldCmd.Flags().BoolVar(&roleScaffoldDryRun, "dry-run", false, "resolve and print the scaffold without creating beads")

	roleCmd.AddCommand(roleListCmd)
	roleCmd.AddCommand(roleScaffoldCmd)
	rootCmd.AddCommand(roleCmd)
}
