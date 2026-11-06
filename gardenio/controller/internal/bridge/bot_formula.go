package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"gardenio/controller/internal/beadsapi"

	"github.com/slack-go/slack"
)

// formulaVarDef matches the kbeads FormulaVarDef type.
type formulaVarDef struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Required    bool     `json:"required,omitempty"`
	Default     string   `json:"default,omitempty"`
	Type        string   `json:"type,omitempty"`
	Enum        []string `json:"enum,omitempty"`
}

// formulaStep matches the kbeads FormulaStep type.
type formulaStep struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Description string   `json:"description,omitempty"`
	Type        string   `json:"type,omitempty"`
	Priority    *int     `json:"priority,omitempty"`
	Labels      []string `json:"labels,omitempty"`
	DependsOn   []string `json:"depends_on,omitempty"`
	Assignee    string   `json:"assignee,omitempty"`
	Condition   string   `json:"condition,omitempty"`
	// Role is the legacy SINGULAR target-role spelling. The canonical kbeads
	// field is Roles (plural) — see kbeads/internal/formula/types.go Step.Roles,
	// which `kd formula apply` reads. Every real formula in the fleet writes
	// `roles`; this struct only ever read `role`, so per-step roles were
	// silently dropped on the bridge pour path (ka-0000001730). Both are parsed
	// now; effectiveRole() prefers the plural.
	Role string `json:"role,omitempty"`
	// Roles is the canonical plural spelling. Multi-role steps take the FIRST
	// entry as the dispatch target (the rest still land as role: labels), since
	// a pod runs exactly one role.
	Roles           []string `json:"roles,omitempty"`
	Project         string   `json:"project,omitempty"`           // target project (empty = inherit molecule project)
	SuggestNewAgent bool     `json:"suggest_new_agent,omitempty"` // hint that a dedicated agent should handle this step
}

// effectiveRole returns the step's dispatch role: the first entry of the
// canonical plural `roles`, falling back to the legacy singular `role`.
// Empty means "inherit the molecule's driver role".
func (s formulaStep) effectiveRole() string {
	for _, r := range s.Roles {
		if r = strings.TrimSpace(r); r != "" {
			return r
		}
	}
	return strings.TrimSpace(s.Role)
}

// allRoles returns every role the step declares (plural entries first, then
// the legacy singular if it adds anything), deduped and trimmed. These all
// become `role:<x>` labels on the step bead so role-scoped queries and advice
// matching see them; only effectiveRole() drives dispatch.
func (s formulaStep) allRoles() []string {
	seen := make(map[string]bool)
	var out []string
	for _, r := range append(append([]string(nil), s.Roles...), s.Role) {
		r = strings.TrimSpace(r)
		if r == "" || seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	return out
}

// formulaFields holds the parsed vars and steps from a formula bead's fields.
type formulaFields struct {
	Vars  []formulaVarDef `json:"vars"`
	Steps []formulaStep   `json:"steps"`
}

// handleFormulaCommand processes the /formula slash command.
//
// Usage:
//
//	/formula                              — list available formulas
//	/formula list                         — list available formulas
//	/formula show <id-or-name>            — show formula details
//	/formula pour <id-or-name> [--var k=v ...] — instantiate as persistent molecule
//	/formula wisp <id-or-name> [--var k=v ...] — instantiate as ephemeral wisp
func (b *Bot) handleFormulaCommand(ctx context.Context, cmd slack.SlashCommand) {
	th := b.themeFor(ctx, cmd.ChannelID)
	args := splitQuotedArgs(strings.TrimSpace(cmd.Text))

	subcommand := "list"
	if len(args) > 0 {
		subcommand = strings.ToLower(args[0])
	}

	switch subcommand {
	case "list", "ls":
		b.handleFormulaList(ctx, cmd)
	case "show":
		if len(args) < 2 {
			b.postEphemeral(cmd, th.Semantic(SemFail)+" Usage: `/formula show <id-or-name>`")
			return
		}
		b.handleFormulaShow(ctx, cmd, args[1])
	case "pour":
		if len(args) < 2 {
			b.postEphemeral(cmd, th.Semantic(SemFail)+" Usage: `/formula pour <id-or-name> [--var key=value ...]`")
			return
		}
		b.handleFormulaPour(ctx, cmd, args[1:], false)
	case "wisp":
		if len(args) < 2 {
			b.postEphemeral(cmd, th.Semantic(SemFail)+" Usage: `/formula wisp <id-or-name> [--var key=value ...]`")
			return
		}
		b.handleFormulaPour(ctx, cmd, args[1:], true)
	case "help":
		b.postEphemeral(cmd, strings.Join([]string{
			th.Semantic(SemNote) + " */formula* — manage reusable work formulas",
			"",
			"`/formula` or `/formula list` — list available formulas",
			"`/formula show <id>` — show formula details (vars, steps)",
			"`/formula pour <id> [--var k=v ...]` — instantiate as persistent molecule",
			"`/formula wisp <id> [--var k=v ...]` — instantiate as ephemeral wisp",
			"`/formula help` — show this help",
		}, "\n"))
	default:
		b.postEphemeral(cmd, fmt.Sprintf(th.Semantic(SemFail)+" Unknown subcommand %q. Use `/formula help` for usage.", subcommand))
	}
}

// handleFormulaList lists available formulas.
func (b *Bot) handleFormulaList(ctx context.Context, cmd slack.SlashCommand) {
	th := b.themeFor(ctx, cmd.ChannelID)
	result, err := b.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"formula"},
		Statuses: []string{"open"},
		Limit:    25,
	})
	if err != nil {
		b.logger.Error("formula list: failed to list formulas", "error", err)
		b.postEphemeral(cmd, th.Semantic(SemFail)+" Failed to list formulas")
		return
	}

	if result.Total == 0 {
		b.postEphemeral(cmd, th.Semantic(SemNote)+" No formulas found. Create one with `kd formula create`.")
		return
	}

	blocks := []slack.Block{
		slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn",
				fmt.Sprintf("%s *%d Formula%s*", th.Semantic(SemNote), result.Total, plural(result.Total)),
				false, false),
			nil, nil),
		slack.NewDividerBlock(),
	}

	for _, f := range result.Beads {
		// Parse vars/steps counts from fields.
		ff := parseFormulaFields(f)
		line := fmt.Sprintf("*%s*\n%s", b.beadTitleLink(f.ID, f.Title), f.ID)
		if len(ff.Vars) > 0 || len(ff.Steps) > 0 {
			line += fmt.Sprintf(" · %d var%s, %d step%s",
				len(ff.Vars), plural(len(ff.Vars)),
				len(ff.Steps), plural(len(ff.Steps)))
		}
		line += fmt.Sprintf("\n/formula pour %s", f.ID)
		blocks = append(blocks,
			slack.NewSectionBlock(
				slack.NewTextBlockObject("mrkdwn", line, false, false),
				nil, nil))
	}

	if result.Total > len(result.Beads) {
		blocks = append(blocks,
			slack.NewContextBlock("",
				slack.NewTextBlockObject("mrkdwn",
					fmt.Sprintf("_...and %d more_", result.Total-len(result.Beads)),
					false, false)))
	}

	_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
		slack.MsgOptionBlocks(blocks...))
}

// handleFormulaShow shows details of a formula.
func (b *Bot) handleFormulaShow(ctx context.Context, cmd slack.SlashCommand, idOrName string) {
	th := b.themeFor(ctx, cmd.ChannelID)
	formula, err := b.resolveFormula(ctx, idOrName)
	if err != nil {
		b.postEphemeral(cmd, fmt.Sprintf(th.Semantic(SemFail)+" %s", err.Error()))
		return
	}

	ff := parseFormulaFields(formula)

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%s *%s*\n%s\n", th.Semantic(SemNote), b.beadTitleLink(formula.ID, formula.Title), formula.ID))

	if formula.Description != "" {
		desc := formula.Description
		if len(desc) > 200 {
			desc = desc[:197] + "..."
		}
		sb.WriteString(fmt.Sprintf("\n%s\n", desc))
	}

	if len(ff.Vars) > 0 {
		sb.WriteString("\n*Variables:*\n")
		for _, v := range ff.Vars {
			req := ""
			if v.Required {
				req = " _(required)_"
			}
			def := ""
			if v.Default != "" {
				def = fmt.Sprintf(" [default: `%s`]", v.Default)
			}
			enum := ""
			if len(v.Enum) > 0 {
				enum = fmt.Sprintf(" {%s}", strings.Join(v.Enum, ", "))
			}
			desc := ""
			if v.Description != "" {
				desc = " — " + v.Description
			}
			sb.WriteString(fmt.Sprintf("  `{{%s}}`%s%s%s%s\n", v.Name, req, def, enum, desc))
		}
	}

	if len(ff.Steps) > 0 {
		sb.WriteString("\n*Steps:*\n")
		for _, s := range ff.Steps {
			typ := s.Type
			if typ == "" {
				typ = "task"
			}
			deps := ""
			if len(s.DependsOn) > 0 {
				deps = fmt.Sprintf(" (after: %s)", strings.Join(s.DependsOn, ", "))
			}
			transition := ""
			if s.Project != "" {
				transition += " → project:" + s.Project
			}
			if roles := s.allRoles(); len(roles) > 0 {
				if transition == "" {
					transition += " →"
				} else {
					transition += ","
				}
				transition += " role:" + strings.Join(roles, "+")
			}
			if s.SuggestNewAgent {
				transition += " " + th.Semantic(SemSpawn) + " new agent"
			}
			sb.WriteString(fmt.Sprintf("  `%s` %s [%s]%s%s\n", s.ID, s.Title, typ, deps, transition))
		}
	}

	sb.WriteString(fmt.Sprintf("\n/formula pour %s", formula.ID))
	b.postEphemeral(cmd, sb.String())
}

// handleFormulaPour instantiates a formula as a molecule (pour) or wisp.
func (b *Bot) handleFormulaPour(ctx context.Context, cmd slack.SlashCommand, args []string, ephemeral bool) {
	th := b.themeFor(ctx, cmd.ChannelID)
	idOrName := args[0]

	// Parse --var flags from remaining args.
	varPairs := make(map[string]string)
	for i := 1; i < len(args); i++ {
		if (args[i] == "--var" || args[i] == "-v") && i+1 < len(args) {
			i++
			k, v, ok := splitVarPair(args[i])
			if !ok {
				b.postEphemeral(cmd, fmt.Sprintf(th.Semantic(SemFail)+" Invalid --var %q: expected key=value", args[i]))
				return
			}
			varPairs[k] = v
		} else if strings.HasPrefix(args[i], "--var=") {
			pair := strings.TrimPrefix(args[i], "--var=")
			k, v, ok := splitVarPair(pair)
			if !ok {
				b.postEphemeral(cmd, fmt.Sprintf(th.Semantic(SemFail)+" Invalid --var %q: expected key=value", pair))
				return
			}
			varPairs[k] = v
		}
	}

	// Resolve the formula.
	formula, err := b.resolveFormula(ctx, idOrName)
	if err != nil {
		b.postEphemeral(cmd, fmt.Sprintf(th.Semantic(SemFail)+" %s", err.Error()))
		return
	}

	ff := parseFormulaFields(formula)
	if len(ff.Steps) == 0 {
		b.postEphemeral(cmd, fmt.Sprintf(th.Semantic(SemFail)+" Formula %s has no steps", b.beadLink(formula.ID, fmt.Sprintf("`%s`", formula.ID))))
		return
	}

	// Apply defaults and validate required vars.
	for _, vd := range ff.Vars {
		if _, ok := varPairs[vd.Name]; !ok {
			if vd.Default != "" {
				varPairs[vd.Name] = vd.Default
			} else if vd.Required {
				b.postEphemeral(cmd, fmt.Sprintf(th.Semantic(SemFail)+" Required variable `{{%s}}` not provided. Use `--var %s=<value>`", vd.Name, vd.Name))
				return
			}
		}
		// Validate enum constraint.
		if len(vd.Enum) > 0 {
			if val, ok := varPairs[vd.Name]; ok {
				valid := false
				for _, e := range vd.Enum {
					if val == e {
						valid = true
						break
					}
				}
				if !valid {
					b.postEphemeral(cmd, fmt.Sprintf(th.Semantic(SemFail)+" Variable `{{%s}}` value %q not in allowed values: %v", vd.Name, val, vd.Enum))
					return
				}
			}
		}
	}

	// Expand steps: substitute variables, evaluate conditions.
	type expandedStep struct {
		formulaStep
		skip bool
	}

	expanded := make([]expandedStep, 0, len(ff.Steps))
	for _, s := range ff.Steps {
		es := expandedStep{formulaStep: s}
		if s.Condition != "" {
			if !formulaEvalCondition(s.Condition, varPairs) {
				es.skip = true
			}
		}
		es.Title = formulaSubstituteVars(s.Title, varPairs)
		es.Description = formulaSubstituteVars(s.Description, varPairs)
		es.Assignee = formulaSubstituteVars(s.Assignee, varPairs)
		es.Role = formulaSubstituteVars(s.Role, varPairs)
		es.Project = formulaSubstituteVars(s.Project, varPairs)
		expanded = append(expanded, es)
	}

	// Filter skipped steps.
	skipped := make(map[string]bool)
	for _, es := range expanded {
		if es.skip {
			skipped[es.ID] = true
		}
	}

	var active []expandedStep
	for _, es := range expanded {
		if es.skip {
			continue
		}
		var filteredDeps []string
		for _, dep := range es.DependsOn {
			if !skipped[dep] {
				filteredDeps = append(filteredDeps, dep)
			}
		}
		es.DependsOn = filteredDeps
		active = append(active, es)
	}

	if len(active) == 0 {
		b.postEphemeral(cmd, th.Semantic(SemFail)+" All steps were filtered by conditions; nothing to create")
		return
	}

	phase := "molecule"
	if ephemeral {
		phase = "wisp"
	}

	// Collect active steps for instantiation.
	var activeSteps []formulaStep
	for _, es := range active {
		activeSteps = append(activeSteps, es.formulaStep)
	}

	// Resolve project from channel.
	project := b.projectFromChannel(ctx, cmd.ChannelID)

	// If this slash command was invoked from a Slack thread, resolve (or
	// create) the canonical thread bead so the molecule can record a
	// `triggered-by` lineage edge back to it. The thread-bead lookup uses
	// the synchronous ctx (so its 30s budget bounds the work) while the
	// molecule/step creation runs in the background.
	triggerBeadID := ""
	if threadTS := SlashThreadTSFromContext(ctx); threadTS != "" && isValidThreadBinding(cmd.ChannelID, threadTS) {
		tbf := threadBeadFields{
			Channel:      cmd.ChannelID,
			ThreadTS:     threadTS,
			SpawnChannel: cmd.ChannelID,
			OpenerUserID: cmd.UserID,
		}
		id, _, tbErr := b.getOrCreateThreadBead(ctx, project, tbf)
		if tbErr != nil {
			// Thread-bead failures must not block the pour — log and
			// proceed without lineage. The user can still see the molecule.
			b.logger.Warn("formula pour: failed to resolve thread bead",
				"channel", cmd.ChannelID, "thread_ts", threadTS, "error", tbErr)
		} else {
			triggerBeadID = id
		}
	}

	// Run instantiation asynchronously — creating many beads can be slow.
	go func() {
		molID, stepCount, err := b.instantiateFormulaSteps(
			context.Background(), formula, activeSteps, varPairs, ephemeral, project, triggerBeadID)
		if err != nil {
			b.logger.Error("formula pour: instantiation failed", "formula", formula.ID, "error", err)
			b.postEphemeral(cmd, fmt.Sprintf(th.Semantic(SemFail)+" Failed to create %s: %s", phase, err.Error()))
			return
		}
		b.logger.Info("formula poured via Slack",
			"formula", formula.ID, "molecule", molID, "steps", stepCount,
			"phase", phase, "user", cmd.UserID)

		// Spawn an agent to work on the molecule.
		agentMsg := ""
		if project != "" {
			agentName := ""
			if formula.Fields != nil {
				agentName = formula.Fields["assigned_agent"]
			}
			if agentName == "" {
				molTitle := formulaSubstituteVars(formula.Title, varPairs)
				agentName = generateAgentName(project, molTitle)
			}
			// Drive the molecule with the role the formula declares, not a
			// hardcoded "crew" — see formulaDriverRole. activeSteps is the
			// condition-filtered, var-substituted step set, so step-1 role
			// fallback reflects what was actually poured.
			driverRole := formulaDriverRole(formula, activeSteps)
			agentID, spawnErr := b.daemon.SpawnAgent(
				context.Background(), agentName, project, molID, driverRole, "")
			if spawnErr != nil {
				b.logger.Error("formula pour: failed to spawn agent",
					"formula", formula.ID, "molecule", molID, "error", spawnErr)
			} else {
				b.logger.Info("formula pour: spawned agent for molecule",
					"formula", formula.ID, "molecule", molID, "agent", agentID, "agentName", agentName, "role", driverRole)
				agentMsg = fmt.Sprintf("\n%s Agent `%s` (role `%s`) spawned to work on it.", th.BeadType["agent"], agentName, driverRole)
			}
		}

		molLink := b.beadLink(molID, fmt.Sprintf("`%s`", molID))
		b.postEphemeral(cmd, fmt.Sprintf(
			"%s Created %s %s from formula *%s*\n%d step%s instantiated. Use `kd mol show %s` for details.%s",
			th.Semantic(SemOK), phase, molLink, b.beadTitleLink(formula.ID, formula.Title), stepCount, plural(stepCount), molID, agentMsg))
	}()

	b.postEphemeral(cmd, fmt.Sprintf(th.Glyph(LifecycleWorking)+" Pouring formula *%s* (%d steps)...", b.beadTitleLink(formula.ID, formula.Title), len(active)))
}

// instantiateFormulaSteps creates the molecule bead and child step beads.
//
// triggerBeadID, when non-empty, records a `triggered-by` lineage edge from
// the molecule to the trigger source (e.g. the Slack thread bead from which a
// `/formula pour` was issued). Failures writing the lineage edge are logged
// but do not abort the pour — the molecule and step beads remain valid.
func (b *Bot) instantiateFormulaSteps(
	ctx context.Context,
	formula *beadsapi.BeadDetail,
	steps []formulaStep,
	vars map[string]string,
	ephemeral bool,
	project string,
	triggerBeadID string,
) (string, int, error) {
	// Slack `/formula pour` has no upstream molecule to chain from, so no
	// cross-pour anchor (the empty string). Cross-pour edges are authored on the
	// apply_formula trigger path, where the upstream emit step is known.
	molID, n, err := instantiateFormula(ctx, b.daemon, b.logger,
		formula, steps, vars, ephemeral, project, "slack-bridge", "")
	if err != nil {
		return molID, n, err
	}

	// Record the trigger-source lineage edge (e.g. Slack thread bead) so the
	// molecule's provenance is queryable via the closed-core `triggered-by`
	// dep type. See kbeads model.DepTriggeredBy.
	if triggerBeadID != "" {
		if err := b.daemon.AddDependency(ctx, molID, triggerBeadID, "triggered-by", "slack-bridge"); err != nil {
			b.logger.Warn("formula pour: failed to add triggered-by dep",
				"molecule", molID, "trigger_bead", triggerBeadID, "error", err)
		}
	}
	return molID, n, nil
}

// resolveFormula finds a formula by ID or by name search.
func (b *Bot) resolveFormula(ctx context.Context, idOrName string) (*beadsapi.BeadDetail, error) {
	// Try direct ID lookup first.
	if strings.HasPrefix(idOrName, "gd-") || strings.HasPrefix(idOrName, "ka-") {
		bead, err := b.daemon.GetBead(ctx, idOrName)
		if err != nil {
			return nil, fmt.Errorf("formula %q not found: %s", idOrName, err.Error())
		}
		if bead.Type != "formula" && bead.Type != "template" {
			return nil, fmt.Errorf("bead `%s` is type %q, not formula", idOrName, bead.Type)
		}
		return bead, nil
	}

	// Search by name.
	result, err := b.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"formula"},
		Statuses: []string{"open"},
		Search:   idOrName,
		Limit:    5,
	})
	if err != nil {
		return nil, fmt.Errorf("searching formulas: %s", err.Error())
	}
	if result.Total == 0 {
		return nil, fmt.Errorf("no formula found matching %q", idOrName)
	}
	if result.Total > 1 {
		var names []string
		for _, f := range result.Beads {
			names = append(names, fmt.Sprintf("%s (%s)", b.beadLink(f.ID, fmt.Sprintf("`%s`", f.ID)), f.Title))
		}
		return nil, fmt.Errorf("multiple formulas match %q: %s — use the ID instead", idOrName, strings.Join(names, ", "))
	}
	return result.Beads[0], nil
}

// parseFormulaFields extracts vars and steps from a formula bead's fields.
func parseFormulaFields(bead *beadsapi.BeadDetail) formulaFields {
	var ff formulaFields
	if bead.Fields == nil {
		return ff
	}

	// Fields is map[string]string — vars and steps are JSON strings.
	if raw, ok := bead.Fields["vars"]; ok {
		_ = json.Unmarshal([]byte(raw), &ff.Vars)
	}
	if raw, ok := bead.Fields["steps"]; ok {
		_ = json.Unmarshal([]byte(raw), &ff.Steps)
	}
	return ff
}

// formulaDefaultDriverRole is the spawn role for a molecule's driver agent when
// the formula does not name one — the generalist that walks a whole molecule.
// Formulas that DO declare a role (e.g. the FSM fix/verify/publish formulas)
// override this via formulaDriverRole.
const formulaDefaultDriverRole = "crew"

// formulaDriverRole resolves the role for the ONE driver agent a pour spawns on
// the molecule. It reads the formula's own declaration, falling back to the
// generalist "crew" when the formula names no role.
//
// WHY THIS EXISTS. Both pour paths (Slack `/formula pour` and the apply_formula
// trigger) used to spawn the molecule driver with the role hardcoded "crew",
// ignoring the role the formula declares. So pouring fix-proof
// (default_roles=[monorepo-fix]) produced a `crew` driver with NONE of the
// monorepo-fix role's overrides (model, skills, advice, channel) — the declared
// role was data nobody consumed at the molecule level, the same class of bug
// the step-role dispatcher closed one level down (ka-0000001730, bug
// ka-0000001755). This is the pour-time half of the §7→§8 role handoff: the
// driver that FIRST works the molecule now carries the formula's role.
//
// Resolution order, most specific first:
//   - default_roles[0] — the canonical formula-level role (kbeads Fields.DefaultRoles).
//     First entry wins; a pod runs exactly one role.
//   - The molecule's own step 1 role, when the formula declares no default_roles
//     but step 1 does — the driver walks into step 1 first, so its role is the
//     right generalization when nothing more specific is set.
//   - "crew" — the historical default, preserved for every formula that names no
//     role at all (the seed-recipe / audit formulas that WANT a generalist driver).
func formulaDriverRole(formula *beadsapi.BeadDetail, steps []formulaStep) string {
	if formula != nil && formula.Fields != nil {
		if r := firstNonEmptyRole(parseRolesField(formula.Fields["default_roles"])); r != "" {
			return r
		}
	}
	if len(steps) > 0 {
		if r := steps[0].effectiveRole(); r != "" {
			return r
		}
	}
	return formulaDefaultDriverRole
}

// parseRolesField decodes a formula field that carries a role list. The daemon
// renders such a field as a JSON array string (`["monorepo-fix"]`) in the
// map[string]string Fields projection (beadsapi.ParseFieldsJSON re-marshals
// non-string values), so try JSON first, then fall back to a bare single-role
// string for tolerance.
func parseRolesField(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var roles []string
	if err := json.Unmarshal([]byte(raw), &roles); err == nil {
		return roles
	}
	return []string{raw}
}

// firstNonEmptyRole returns the first trimmed non-empty entry, or "".
func firstNonEmptyRole(roles []string) string {
	for _, r := range roles {
		if r = strings.TrimSpace(r); r != "" {
			return r
		}
	}
	return ""
}

// formulaBuildStepLabels builds the label set for a step bead, handling
// project overrides and per-step label merging.
func formulaBuildStepLabels(molLabels, stepLabels []string, stepProject, molProject string) []string {
	seen := make(map[string]bool)
	var merged []string

	// Start with molecule labels, but replace project label if step overrides it.
	for _, l := range molLabels {
		if strings.HasPrefix(l, "project:") && stepProject != molProject {
			continue // skip molecule's project label; we'll add the step's project
		}
		seen[l] = true
		merged = append(merged, l)
	}

	// Add step-specific project label if overridden.
	if stepProject != molProject && stepProject != "" {
		projLabel := "project:" + stepProject
		if !seen[projLabel] {
			seen[projLabel] = true
			merged = append(merged, projLabel)
		}
	}

	// Merge per-step labels.
	for _, l := range stepLabels {
		if !seen[l] {
			seen[l] = true
			merged = append(merged, l)
		}
	}

	return merged
}

// formulaVarPattern matches {{variable}} placeholders.
var formulaVarPattern = regexp.MustCompile(`\{\{(\w+)\}\}`)

// formulaSubstituteVars replaces {{name}} placeholders with values from vars.
func formulaSubstituteVars(s string, vars map[string]string) string {
	return formulaVarPattern.ReplaceAllStringFunc(s, func(match string) string {
		name := match[2 : len(match)-2]
		if val, ok := vars[name]; ok {
			return val
		}
		return match
	})
}

// formulaEvalCondition evaluates a simple condition string against variables.
func formulaEvalCondition(cond string, vars map[string]string) bool {
	cond = strings.TrimSpace(cond)

	if strings.HasPrefix(cond, "!") {
		return !formulaEvalCondition(strings.TrimPrefix(cond, "!"), vars)
	}

	for _, op := range []string{"!=", "=="} {
		if parts := strings.SplitN(cond, op, 2); len(parts) == 2 {
			left := formulaSubstituteVars(strings.TrimSpace(parts[0]), vars)
			right := strings.TrimSpace(parts[1])
			if op == "==" {
				return left == right
			}
			return left != right
		}
	}

	resolved := formulaSubstituteVars(cond, vars)
	if resolved != cond {
		return resolved != "" && resolved != "false" && resolved != "0"
	}
	if formulaVarPattern.MatchString(cond) {
		return false
	}
	return resolved != ""
}

// splitVarPair splits "key=value" into (key, value, true) or ("", "", false).
func splitVarPair(s string) (string, string, bool) {
	idx := strings.IndexByte(s, '=')
	if idx < 1 {
		return "", "", false
	}
	return s[:idx], s[idx+1:], true
}

// plural returns "s" if n != 1.
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// postEphemeral is a convenience wrapper for sending ephemeral messages.
func (b *Bot) postEphemeral(cmd slack.SlashCommand, text string) {
	_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
		slack.MsgOptionText(text, false))
}
