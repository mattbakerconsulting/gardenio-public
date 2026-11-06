package main

import (
	"strings"
	"testing"

	"gardenio/controller/internal/advice"
	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

// testSurface builds a minimal cobra tree that mirrors the parts of the real
// `gb` surface the lint reasons about: a `decision` GROUP with real subcommands
// (create/reflag), a leaf `stop` with a --wrapup flag, a leaf `yield`, a leaf
// `spawn` with --task/--role, a `squawk` leaf, an `escalation` group, and a
// `slack` group with a `reply` leaf. This lets the tests exercise the same
// classifier the CLI uses without a live daemon.
func testSurface() *commandSurface {
	root := &cobra.Command{Use: "gb"}
	root.PersistentFlags().Bool("json", false, "")

	decision := &cobra.Command{Use: "decision"}
	decision.AddCommand(&cobra.Command{Use: "create"})
	reflag := &cobra.Command{Use: "reflag"}
	reflag.Flags().String("reason", "", "")
	decision.AddCommand(reflag)
	decision.AddCommand(&cobra.Command{Use: "report"})
	root.AddCommand(decision)

	esc := &cobra.Command{Use: "escalation"}
	esc.AddCommand(&cobra.Command{Use: "create"})
	escReflag := &cobra.Command{Use: "reflag"}
	escReflag.Flags().String("reason", "", "")
	esc.AddCommand(escReflag)
	root.AddCommand(esc)

	slack := &cobra.Command{Use: "slack"}
	slack.AddCommand(&cobra.Command{Use: "reply"})
	root.AddCommand(slack)

	stop := &cobra.Command{Use: "stop"}
	stop.Flags().String("wrapup", "", "")
	root.AddCommand(stop)

	spawn := &cobra.Command{Use: "spawn"}
	spawn.Flags().String("task", "", "")
	spawn.Flags().String("role", "", "")
	root.AddCommand(spawn)

	root.AddCommand(&cobra.Command{Use: "yield"})
	root.AddCommand(&cobra.Command{Use: "squawk"})
	root.AddCommand(&cobra.Command{Use: "crew"})
	news := &cobra.Command{Use: "news"}
	news.Flags().Bool("active-only", false, "")
	root.AddCommand(news)

	return buildCommandSurface(root)
}

func triggerBead(id, prompt string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID:     id,
		Title:  "gardener " + id,
		Type:   "trigger",
		Status: "open",
		Fields: map[string]string{"prompt": prompt},
	}
}

// The canonical regression: a synthetic trigger prompt teaching the retired
// `gb decision pingback --thread` step must be flagged (bug ka-0000007245 class).
func TestLintTriggerPrompts_FlagsPingback(t *testing.T) {
	beads := []*beadsapi.BeadDetail{
		triggerBead("gd-x", "Step 3: gb decision pingback <id> --thread <source-thread> to re-surface."),
	}
	findings := lintTriggerPrompts(beads, testSurface())
	if len(findings) != 1 {
		t.Fatalf("want exactly 1 finding, got %d: %+v", len(findings), findings)
	}
	f := findings[0]
	if f.Kind != "subcommand" || f.Token != "gb decision pingback" {
		t.Fatalf("want subcommand drift on `gb decision pingback`, got kind=%q token=%q", f.Kind, f.Token)
	}
	if f.ID != "gd-x" || f.Field != "prompt" {
		t.Fatalf("want id=gd-x field=prompt, got id=%q field=%q", f.ID, f.Field)
	}
}

// The valid replacement `gb decision reflag <id> --reason '...'` must NOT be
// flagged — this is the exact usage the backfill converged on, and a false
// positive here would red every clean trigger in CI (ka-0000003064).
func TestLintTriggerPrompts_ReflagIsClean(t *testing.T) {
	beads := []*beadsapi.BeadDetail{
		triggerBead("gd-x", "For each stale decision: gb decision reflag <id> --reason '...' (bumps priority)."),
	}
	if findings := lintTriggerPrompts(beads, testSurface()); len(findings) != 0 {
		t.Fatalf("reflag usage should be clean, got %+v", findings)
	}
}

// Prose that mentions a group name followed by an English word must NOT be
// flagged. These are verbatim shapes pulled from the live corpus that an
// earlier, looser discriminator false-flagged (ka-0000003064).
func TestLintTriggerPrompts_ProseNotFlagged(t *testing.T) {
	cases := []string{
		"If a human can do it, file a gb decision or gb escalation and gb yield.",
		"Release merges need an operator gb decision first.",
		"Raise a gb decision when human/privileged action is needed.",
		"Escalate (gb escalation or a decision) rather than guessing.",
		"Open a gb decision for a genuine architectural fork.",
		"Post via gb squawk the source thread with a link.",
		"Do NOT gb stop with goal_reached unless verified.",
	}
	surface := testSurface()
	for _, prompt := range cases {
		beads := []*beadsapi.BeadDetail{triggerBead("gd-p", prompt)}
		if findings := lintTriggerPrompts(beads, surface); len(findings) != 0 {
			t.Errorf("prose %q should be clean, got %+v", prompt, findings)
		}
	}
}

// A flag belonging to a LATER `gb` command on the same line must not be
// misattributed to an earlier command. "gb yield … then gb stop --wrapup" must
// not read --wrapup as a flag of `yield` (ka-0000003064).
func TestLintTriggerPrompts_NoFlagBleedAcrossCommands(t *testing.T) {
	beads := []*beadsapi.BeadDetail{
		triggerBead("gd-x", "If open decision: gb yield (blocks). Else post via gb slack reply, then gb stop --wrapup '{}'."),
	}
	if findings := lintTriggerPrompts(beads, testSurface()); len(findings) != 0 {
		t.Fatalf("flags must bind to their own command, got %+v", findings)
	}
}

// A genuinely invalid flag on a real command IS flagged (ka-0000003064).
func TestLintTriggerPrompts_FlagsUnknownFlag(t *testing.T) {
	beads := []*beadsapi.BeadDetail{
		triggerBead("gd-x", "Dispatch with gb spawn --project monorepo --task <id>."),
	}
	findings := lintTriggerPrompts(beads, testSurface())
	if len(findings) != 1 || findings[0].Kind != "flag" || findings[0].Token != "gb spawn --project" {
		t.Fatalf("want flag drift on `gb spawn --project`, got %+v", findings)
	}
}

// A valid flag on a real command is clean, and flags in a quoted VALUE never
// leak into the command's own flag set (ka-0000003064).
func TestLintTriggerPrompts_ValidFlagAndQuotedValueClean(t *testing.T) {
	beads := []*beadsapi.BeadDetail{
		triggerBead("gd-a", "gb spawn --task <id> --role crew"),
		triggerBead("gd-b", "gb stop --wrapup '{\"tldr\":\"used --project once\"}'"),
	}
	if findings := lintTriggerPrompts(beads, testSurface()); len(findings) != 0 {
		t.Fatalf("valid flags / quoted values should be clean, got %+v", findings)
	}
}

// Run-history surfaces must be ignored: a `progress_state` field mentioning the
// retired command is append-only agent prose, NOT an instruction, and must not
// be scanned. Only fields.prompt / terminal_goal / gardener.* are instructions
// (ka-0000003064).
func TestLintTriggerPrompts_IgnoresRunHistory(t *testing.T) {
	b := &beadsapi.BeadDetail{
		ID:     "gd-h",
		Type:   "trigger",
		Status: "open",
		Fields: map[string]string{
			// No instruction surface mentions the retired command.
			"prompt": "Run the playbook: gb decision reflag <id> --reason '...'.",
			// Engine-owned run history that legitimately mentions it.
			"progress_state": `{"runs":[{"progress_summary":"no gb decision pingback <id> due this cycle"}]}`,
			"run_log":        `gb decision pingback <id> was attempted`,
		},
	}
	if findings := lintTriggerPrompts([]*beadsapi.BeadDetail{b}, testSurface()); len(findings) != 0 {
		t.Fatalf("run history must not be scanned, got %+v", findings)
	}
}

// The gardener-spec instruction fields ARE scanned (terminal_goal / exit_condition
// / name), but the engine-owned progress_state nested in the same blob is not
// (ka-0000003064).
func TestLintTriggerPrompts_ScansGardenerSpecInstructionsOnly(t *testing.T) {
	b := &beadsapi.BeadDetail{
		ID:     "gd-c",
		Type:   "trigger",
		Status: "open",
		Fields: map[string]string{
			"gardener": `{"terminal_goal":"loop until done; use gb decision pingback <id> if stale","progress_state":{"runs":[{"progress_summary":"gb decision pingback <id> not needed"}]}}`,
		},
	}
	findings := lintTriggerPrompts([]*beadsapi.BeadDetail{b}, testSurface())
	if len(findings) != 1 || findings[0].Field != "gardener.terminal_goal" || findings[0].Token != "gb decision pingback" {
		t.Fatalf("want single gardener.terminal_goal finding on pingback, got %+v", findings)
	}
}

// A bead-embedded substring that merely ends in "gb " (e.g. "ngb ") must not
// anchor a match; only a real `gb ` token boundary does (ka-0000003064).
func TestLintTriggerPrompts_AnchorBoundary(t *testing.T) {
	beads := []*beadsapi.BeadDetail{
		triggerBead("gd-x", "The debugging pipeline (dbngb decision pingback) is unrelated."),
	}
	if findings := lintTriggerPrompts(beads, testSurface()); len(findings) != 0 {
		t.Fatalf("non-boundary `gb` must not anchor, got %+v", findings)
	}
}

// A backtick-wrapped command IS anchored (a common markdown shape in prompts;
// ka-0000003064).
func TestLintTriggerPrompts_BacktickAnchored(t *testing.T) {
	beads := []*beadsapi.BeadDetail{
		triggerBead("gd-x", "Re-surface with `gb decision pingback <id>`."),
	}
	findings := lintTriggerPrompts(beads, testSurface())
	if len(findings) != 1 || findings[0].Token != "gb decision pingback" {
		t.Fatalf("want pingback finding on backticked command, got %+v", findings)
	}
}

// buildCommandSurface must record groups vs leaves and per-command flags.
func TestBuildCommandSurface(t *testing.T) {
	s := testSurface()
	if !s.subcommands["decision"] || !s.subcommands["decision reflag"] {
		t.Errorf("expected decision + decision reflag in surface")
	}
	if !s.groups["decision"] {
		t.Errorf("decision should be a group")
	}
	if s.groups["stop"] {
		t.Errorf("stop is a leaf, not a group")
	}
	if !s.flags["stop"]["wrapup"] {
		t.Errorf("stop should carry --wrapup")
	}
	if !s.flags["decision reflag"]["reason"] {
		t.Errorf("decision reflag should carry --reason")
	}
	// Every command inherits the persistent root --json flag.
	if !s.flags["stop"]["json"] {
		t.Errorf("persistent --json should be inherited by stop")
	}
	// help/completion are excluded from the authored surface.
	if s.subcommands["help"] || s.subcommands["completion"] {
		t.Errorf("help/completion must be excluded")
	}
}

// Findings are sorted deterministically by id, field, token (ka-0000003064).
func TestLintTriggerPrompts_StableSort(t *testing.T) {
	beads := []*beadsapi.BeadDetail{
		triggerBead("gd-b", "gb decision pingback <id>"),
		triggerBead("gd-a", "gb spawn --project x <id>"),
	}
	findings := lintTriggerPrompts(beads, testSurface())
	if len(findings) != 2 {
		t.Fatalf("want 2 findings, got %d: %+v", len(findings), findings)
	}
	if findings[0].ID != "gd-a" || findings[1].ID != "gd-b" {
		t.Fatalf("findings not sorted by id: %+v", findings)
	}
}

// Nil beads and beads with no instruction surface are skipped without panic
// (ka-0000003064).
func TestLintTriggerPrompts_EmptyAndNil(t *testing.T) {
	beads := []*beadsapi.BeadDetail{
		nil,
		{ID: "gd-empty", Type: "trigger", Status: "open", Fields: map[string]string{}},
	}
	if findings := lintTriggerPrompts(beads, testSurface()); len(findings) != 0 {
		t.Fatalf("nil/empty beads should yield no findings, got %+v", findings)
	}
}

// ── over-constraint mode (--over-constraint) ─────────────────────────────────

// findingsByCheck indexes over-constraint findings by check id for assertion.
func ocFindingsByCheck(fs []advice.OverConstraintFinding) map[string][]advice.OverConstraintFinding {
	out := map[string][]advice.OverConstraintFinding{}
	for _, f := range fs {
		out[f.Check] = append(out[f.Check], f)
	}
	return out
}

// The N1 regression (ka-0000004887): a shout-directive-dense gardener trigger
// prompt — the exact smell the P2 re-audit found on the live gardener-delivery
// trigger — is flagged as a directive-density spike, with the finding pointing
// at the prompt field.
func TestTriggerPromptOverConstraint_DirectiveDensitySpike(t *testing.T) {
	beads := []*beadsapi.BeadDetail{
		triggerBead("gd-hot",
			"You MUST review each MR. NEVER merge a human branch. Do NOT rebase. "+
				"ALWAYS wrap up. This is CRITICAL and IMPORTANT and REQUIRED."),
	}
	surfaces := triggerPromptSurfaces(beads)
	findings := advice.LintTriggerPromptOverConstraint(surfaces, "", advice.OverConstraintConfig{})
	byCheck := ocFindingsByCheck(findings)
	dens := byCheck[advice.OverConstraintDirectiveDensity]
	if len(dens) == 0 {
		t.Fatalf("expected a directive-density finding, got %+v", findings)
	}
	f := dens[0]
	if f.ID != "gd-hot" || f.Field != "prompt" {
		t.Fatalf("want id=gd-hot field=prompt, got id=%q field=%q", f.ID, f.Field)
	}
	if f.Severity != advice.LintWarn {
		t.Errorf("want WARN severity, got %q", f.Severity)
	}
	if f.DirectiveCount == 0 {
		t.Errorf("directive-density finding missing DirectiveCount: %+v", f)
	}
}

// A rules-over-judgment absolute in a trigger prompt is flagged, pointing at
// the offending line within the prompt surface.
func TestTriggerPromptOverConstraint_RulesOverJudgment(t *testing.T) {
	beads := []*beadsapi.BeadDetail{
		triggerBead("gd-abs",
			"Dispatch a crew per orphan MR.\nUnder no circumstances merge it yourself."),
	}
	surfaces := triggerPromptSurfaces(beads)
	findings := advice.LintTriggerPromptOverConstraint(surfaces, "", advice.OverConstraintConfig{})
	byCheck := ocFindingsByCheck(findings)
	judg := byCheck[advice.OverConstraintRulesOverJudgment]
	if len(judg) != 1 {
		t.Fatalf("expected 1 rules-over-judgment finding, got %+v", findings)
	}
	if judg[0].Line != 2 {
		t.Errorf("want the finding anchored to line 2 of the prompt, got line %d", judg[0].Line)
	}
	if judg[0].Rule != "under-no-circumstances" {
		t.Errorf("want rule under-no-circumstances, got %q", judg[0].Rule)
	}
}

// A clean, judgment-framed trigger prompt (the shape the reframed gardener
// playbook template produces) yields no findings.
func TestTriggerPromptOverConstraint_CleanPromptClean(t *testing.T) {
	beads := []*beadsapi.BeadDetail{
		triggerBead("gd-clean",
			"For each open child without a live owner, spawn a crew. Reserve gb yield "+
				"for a decision you own and need a human to resolve; otherwise gb stop and "+
				"let the scheduler respawn you on the next trigger."),
	}
	surfaces := triggerPromptSurfaces(beads)
	if findings := advice.LintTriggerPromptOverConstraint(surfaces, "", advice.OverConstraintConfig{}); len(findings) != 0 {
		t.Fatalf("clean judgment-framed prompt should yield no findings, got %+v", findings)
	}
}

// The over-constraint scan reaches EVERY authored instruction surface — the
// terminal_goal field and the gardener-spec's name/terminal_goal — not just the
// flat prompt. Each hit reports the exact field so an author knows where to
// edit.
func TestTriggerPromptOverConstraint_ScansAllSurfaces(t *testing.T) {
	dense := "MUST a. NEVER b. Do NOT c. ALWAYS d. CRITICAL e."
	beads := []*beadsapi.BeadDetail{
		{
			ID: "gd-multi", Title: "gardener gd-multi", Type: "trigger", Status: "open",
			Fields: map[string]string{
				"terminal_goal": dense,
				"gardener":      `{"name":"` + dense + `","terminal_goal":"clean goal"}`,
			},
		},
	}
	surfaces := triggerPromptSurfaces(beads)
	findings := advice.LintTriggerPromptOverConstraint(surfaces, "", advice.OverConstraintConfig{})
	fields := map[string]bool{}
	for _, f := range findings {
		if f.Check == advice.OverConstraintDirectiveDensity {
			fields[f.Field] = true
		}
	}
	for _, want := range []string{"terminal_goal", "gardener.name"} {
		if !fields[want] {
			t.Errorf("expected a directive-density finding on field %q, got fields %v (findings %+v)", want, fields, findings)
		}
	}
}

// Engine-owned run history (run_log / progress_state / runs_*) inside the
// gardener blob is NEVER scanned — it is append-only agent prose that
// legitimately contains shout-words, so scanning it would manufacture false
// positives. Only the authored fields (name / terminal_goal / exit_condition)
// are surfaced, matching the drift lint's instructionSurfaces contract.
func TestTriggerPromptOverConstraint_SkipsRunHistory(t *testing.T) {
	beads := []*beadsapi.BeadDetail{
		{
			ID: "gd-hist", Title: "gardener gd-hist", Type: "trigger", Status: "open",
			Fields: map[string]string{
				"gardener": `{"name":"clean gardener","run_log":[{"wrapup_summary":"MUST NEVER Do NOT ALWAYS CRITICAL IMPORTANT REQUIRED WARNING"}],"progress_state":"NEVER MUST Do NOT ALWAYS CRITICAL"}`,
			},
		},
	}
	surfaces := triggerPromptSurfaces(beads)
	if findings := advice.LintTriggerPromptOverConstraint(surfaces, "", advice.OverConstraintConfig{}); len(findings) != 0 {
		t.Fatalf("engine run-history must not be scanned, got %+v", findings)
	}
}

// A trigger prompt that restates a long run of the injected reference corpus is
// flagged for redundancy; passing an empty ref corpus skips the check.
func TestTriggerPromptOverConstraint_Redundancy(t *testing.T) {
	ref := "The gardenio controller is the single source of truth for agent credentials. " +
		"Account beads define credential accounts with a provider an env key and an api key. " +
		"The controller watches account bead events and syncs them to the coopmux credential broker " +
		"which stores the api keys and distributes them to sessions as profiles with pool load balancing."
	beads := []*beadsapi.BeadDetail{
		triggerBead("gd-red",
			"The gardenio controller is the single source of truth for agent credentials. "+
				"Account beads define credential accounts with a provider an env key and an api key. "+
				"The controller watches account bead events and syncs them to the coopmux credential broker "+
				"which then hands the keys to each running session at startup."),
	}
	surfaces := triggerPromptSurfaces(beads)

	withRef := advice.LintTriggerPromptOverConstraint(surfaces, ref, advice.OverConstraintConfig{})
	var redFound bool
	for _, f := range withRef {
		if f.Check == advice.OverConstraintRedundancy {
			redFound = true
		}
	}
	if !redFound {
		t.Errorf("expected a redundancy finding against the ref corpus, got %+v", withRef)
	}

	noRef := advice.LintTriggerPromptOverConstraint(surfaces, "", advice.OverConstraintConfig{})
	for _, f := range noRef {
		if f.Check == advice.OverConstraintRedundancy {
			t.Errorf("empty ref corpus should skip redundancy check, got %+v", f)
		}
	}
}

// --- reflag type-mismatch check (gd-jFIy6RqhAk) ---

// The exact live defect (ka-0000005655): a surface that teaches `gb decision
// reflag <id>` for freshening beads that its own text says include escalations,
// without teaching `gb escalation reflag`, is a type-incompatibility hit — a
// decision reflag on a type=escalation bead is runtime-rejected.
func TestLintTriggerPrompts_ReflagTypeMismatch_Flagged(t *testing.T) {
	prompt := "b. Escalations or decisions gating downstream steps that are stale " +
		"(>4h old, not yet reflagged): gb decision reflag <id> --reason '...'."
	findings := lintTriggerPrompts([]*beadsapi.BeadDetail{triggerBead("gd-9", prompt)}, testSurface())
	if len(findings) != 1 {
		t.Fatalf("want exactly 1 finding, got %d: %+v", len(findings), findings)
	}
	f := findings[0]
	if f.Kind != "type-mismatch" || f.Token != "gb decision reflag" {
		t.Fatalf("want type-mismatch on `gb decision reflag`, got kind=%q token=%q", f.Kind, f.Token)
	}
	if f.ID != "gd-9" || f.Field != "prompt" {
		t.Fatalf("want id=gd-9 field=prompt, got id=%q field=%q", f.ID, f.Field)
	}
}

// The CORRECT fixed shape — freshen BY TYPE, teaching BOTH verbs — must stay
// clean even though it mentions escalations and uses `gb decision reflag`. This
// is the HEAD plangardener template; flagging it would red the very fix.
func TestLintTriggerPrompts_ReflagByType_Clean(t *testing.T) {
	prompt := "Escalations or decisions gating steps that are stale: freshen by bead type — " +
		"gb decision reflag <id> --reason '...' for type=decision, " +
		"gb escalation reflag <id> --reason '...' for type=escalation."
	if findings := lintTriggerPrompts([]*beadsapi.BeadDetail{triggerBead("gd-ok", prompt)}, testSurface()); len(findings) != 0 {
		t.Fatalf("by-type freshening should be clean, got %+v", findings)
	}
}

// `gb decision reflag` with NO escalation mention in the surface is a normal,
// correct decision-only instruction and must NOT be flagged by this check.
func TestLintTriggerPrompts_ReflagDecisionOnly_Clean(t *testing.T) {
	prompt := "For each decision awaiting=human older than 4h: gb decision reflag <id> --reason '...' (bumps priority)."
	if findings := lintTriggerPrompts([]*beadsapi.BeadDetail{triggerBead("gd-d", prompt)}, testSurface()); len(findings) != 0 {
		t.Fatalf("decision-only reflag should be clean, got %+v", findings)
	}
}

// The load-bearing false-positive guard (the standard gardener template shape):
// a decision-ONLY step 3 reflag, plus an escalation word FAR away in unrelated
// `gb yield` prose (>window chars), must NOT be flagged. This is the exact live
// shape (ka-0000002632 etc.) that a whole-surface mention test flagged falsely.
func TestLintTriggerPrompts_ReflagEscalationFarProse_Clean(t *testing.T) {
	filler := strings.Repeat("x ", 300) // ~600 chars, well past the window
	prompt := "3. For each decision awaiting=human older than 4h: gb decision reflag <id> --reason '...'.\n" +
		filler +
		"\nIf you are genuinely wedged on something only a human can do, file a gb decision or gb escalation and gb yield."
	if findings := lintTriggerPrompts([]*beadsapi.BeadDetail{triggerBead("gd-far", prompt)}, testSurface()); len(findings) != 0 {
		t.Fatalf("far-away escalation prose should not flag a decision-only reflag, got %+v", findings)
	}
}

// Prose that mentions escalations and the word "reflag" but does NOT use
// `gb decision reflag` command-shaped must not be flagged — same prose/command
// discriminator the existence check uses.
func TestLintTriggerPrompts_ReflagTypeMismatch_ProseNotFlagged(t *testing.T) {
	cases := []string{
		"Stale escalations or decisions should be reflagged so the operator sees them again.",
		"When an escalation is stale, reflag it toward P0.",
		"Escalations gate downstream steps; keep decisions fresh.",
	}
	surface := testSurface()
	for _, prompt := range cases {
		beads := []*beadsapi.BeadDetail{triggerBead("gd-p", prompt)}
		if findings := lintTriggerPrompts(beads, surface); len(findings) != 0 {
			t.Errorf("prose %q should be clean, got %+v", prompt, findings)
		}
	}
}

// The escalation mention must be the WORD "escalation", not a substring buried
// in a larger alphanumeric token — the \b guard keeps "descalationism" from
// triggering the type-mismatch check on an otherwise decision-only prompt.
func TestLintTriggerPrompts_ReflagTypeMismatch_WordBoundary(t *testing.T) {
	prompt := "Handle descalationism per policy. For decisions: gb decision reflag <id> --reason '...'."
	if findings := lintTriggerPrompts([]*beadsapi.BeadDetail{triggerBead("gd-w", prompt)}, testSurface()); len(findings) != 0 {
		t.Fatalf("word-boundary guard should keep this clean, got %+v", findings)
	}
}
