package bridge

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
)

// getGlobalInstructions extracts the claude-instructions:global section
// from configBeadEntries(). Returns the map of section name → content.
func getGlobalInstructions(t *testing.T) map[string]string {
	t.Helper()
	entries := configBeadEntries()
	raw, ok := entries["config:claude-instructions:global"]
	if !ok {
		t.Fatal("configBeadEntries missing config:claude-instructions:global")
	}
	sections, ok := raw.(map[string]string)
	if !ok {
		t.Fatal("config:claude-instructions:global is not map[string]string")
	}
	return sections
}

// TestConfigBeadEntries_EpicWorkflow verifies the workflows section documents
// the epic + child task creation pattern. Regression test for 1b488bfd.
func TestConfigBeadEntries_EpicWorkflow(t *testing.T) {
	sections := getGlobalInstructions(t)
	workflows := sections["workflows"]
	if workflows == "" {
		t.Fatal("workflows section is empty")
	}

	checks := []struct {
		needle string
		reason string
	}{
		{"--type=epic", "should document --type=epic for creating epics"},
		{"parent-child", "should document parent-child dependency type"},
		{"blocks", "should document blocks dependency type"},
		{"child tasks", "should explain child task workflow"},
		{"kd dep add", "should include kd dep add command"},
	}
	for _, c := range checks {
		if !strings.Contains(workflows, c.needle) {
			t.Errorf("workflows: %s (missing %q)", c.reason, c.needle)
		}
	}
}

// TestConfigBeadEntries_CommandsEpicType verifies the commands section
// includes epic as a valid bead type. Regression test for 1b488bfd.
func TestConfigBeadEntries_CommandsEpicType(t *testing.T) {
	sections := getGlobalInstructions(t)
	commands := sections["commands"]
	if commands == "" {
		t.Fatal("commands section is empty")
	}

	checks := []struct {
		needle string
		reason string
	}{
		{"epic", "should mention epic type"},
		{"parent-child", "should document parent-child dependency type"},
		{"-l project:", "should document project label flag"},
		{"-d '", "should document -d description flag"},
	}
	for _, c := range checks {
		if !strings.Contains(commands, c.needle) {
			t.Errorf("commands: %s (missing %q)", c.reason, c.needle)
		}
	}
}

// TestConfigBeadEntries_CoreRulesWorkBreakdown verifies the core_rules section
// includes the work breakdown directive. Regression test for 1b488bfd.
func TestConfigBeadEntries_CoreRulesWorkBreakdown(t *testing.T) {
	sections := getGlobalInstructions(t)
	coreRules := sections["core_rules"]
	if coreRules == "" {
		t.Fatal("core_rules section is empty")
	}

	if !strings.Contains(coreRules, "epic") {
		t.Error("core_rules should mention epics in work breakdown directive")
	}
	if !strings.Contains(coreRules, "child task") {
		t.Error("core_rules should mention child tasks in work breakdown directive")
	}
}

// TestConfigBeadEntries_SessionCloseDeliverableLinking verifies the
// session_close checklist tells crew to link deliverables into the data plane
// (gb mr link for MRs, typed-mention/kd dep add for filed beads). Regression
// test for gd-pKyyQCylRq: free-text-only artifact reporting leaves MRs and
// filed beads untraceable.
func TestConfigBeadEntries_SessionCloseDeliverableLinking(t *testing.T) {
	sections := getGlobalInstructions(t)
	sessionClose := sections["session_close"]
	if sessionClose == "" {
		t.Fatal("session_close section is empty")
	}

	checks := []struct {
		needle string
		reason string
	}{
		{"gb mr link", "should tell crew to link opened MRs with gb mr link"},
		{"kd dep add", "should tell crew to link filed beads with kd dep add"},
		{"parent:", "should mention typed-mention parent: for filed beads"},
		{"blocks:", "should mention typed-mention blocks: for filed beads"},
	}
	for _, c := range checks {
		if !strings.Contains(sessionClose, c.needle) {
			t.Errorf("session_close: %s (missing %q)", c.reason, c.needle)
		}
	}
}

// TestConfigBeadEntries_AllSectionsNonEmpty verifies that every section in the
// global claude-instructions is present and non-empty.
func TestConfigBeadEntries_AllSectionsNonEmpty(t *testing.T) {
	sections := getGlobalInstructions(t)
	expectedKeys := []string{
		"prime_header", "session_close", "core_rules", "commands",
		"workflows", "decisions", "session_resumption", "lifecycle",
		"stop_gate", "stop_gate_blocked",
	}
	for _, key := range expectedKeys {
		if sections[key] == "" {
			t.Errorf("global claude-instructions missing or empty section %q", key)
		}
	}
}

// TestConfigBeadEntries_NudgePromptsTemplateVars verifies that nudge prompts
// use Go template variables (not hardcoded values) for context injection.
func TestConfigBeadEntries_NudgePromptsTemplateVars(t *testing.T) {
	entries := configBeadEntries()
	raw, ok := entries["config:nudge-prompts:global"]
	if !ok {
		t.Fatal("configBeadEntries missing config:nudge-prompts:global")
	}
	prompts, ok := raw.(map[string]string)
	if !ok {
		t.Fatal("config:nudge-prompts:global is not map[string]string")
	}

	// All prompt keys that should exist.
	for _, key := range []string{"thread", "adhoc", "default", "prewarmed", "gardener"} {
		if prompts[key] == "" {
			t.Errorf("nudge-prompts missing key %q", key)
		}
	}

	// Adhoc should use template vars.
	adhoc := prompts["adhoc"]
	if !strings.Contains(adhoc, "{{.TaskHint}}") {
		t.Error("adhoc prompt should contain {{.TaskHint}}")
	}
	if !strings.Contains(adhoc, "{{.BoatPrompt}}") {
		t.Error("adhoc prompt should contain {{.BoatPrompt}}")
	}

	// Thread should use {{.Project}} or {{.ProjectHint}}.
	thread := prompts["thread"]
	if !strings.Contains(thread, "{{.Project}}") && !strings.Contains(thread, "{{.ProjectHint}}") {
		t.Error("thread prompt should contain {{.Project}} or {{.ProjectHint}}")
	}
}

// TestConfigBeadEntries_AdhocDeliveryClauseDeliveryModeNeutral guards
// gd-4lQ6FBbHhe: the global adhoc nudge's delivery instruction hard-asserted
// "deliver via a feature branch + PR (never push to main)", which is FALSE for
// push-to-main publish roles (e.g. daily-bundle, which pushes draft candidates
// straight to the dailbert repo main with no MR). A careless crew could waste a
// cycle opening a spurious MR or refuse to push and stall the publish runway.
// The clause must be delivery-mode-neutral: keep feature-branch+PR as the
// DEFAULT, but acknowledge that some roles push directly to main, and defer to
// the task/role contract rather than commanding "never push to main".
func TestConfigBeadEntries_AdhocDeliveryClauseDeliveryModeNeutral(t *testing.T) {
	entries := configBeadEntries()
	raw := entries["config:nudge-prompts:global"].(map[string]string)
	adhoc := raw["adhoc"]

	// The absolute prohibition that contradicted publish roles must be gone.
	if strings.Contains(adhoc, "never push to main") {
		t.Errorf("adhoc delivery clause must not hard-assert 'never push to main' — it contradicts push-to-main publish roles (gd-4lQ6FBbHhe); got: %q", adhoc)
	}
	// Feature branch + PR must remain the stated default for the common case.
	if !strings.Contains(adhoc, "feature branch + PR") {
		t.Errorf("adhoc delivery clause should keep feature branch + PR as the default, got: %q", adhoc)
	}
	// The clause must acknowledge the push-to-main / no-MR path so a publish
	// crew is not misdirected into opening a spurious MR.
	if !strings.Contains(adhoc, "push") || !strings.Contains(adhoc, "main") {
		t.Errorf("adhoc delivery clause should acknowledge the push-to-main path for publish roles, got: %q", adhoc)
	}
	// It should defer to the task/role contract rather than a blanket command.
	if !strings.Contains(adhoc, "contract") {
		t.Errorf("adhoc delivery clause should defer to the task/role contract, got: %q", adhoc)
	}
}

// TestConfigBeadEntries_TeachGBPR guards gd-NHxY3uJfF6: crews read the spawn
// nudges and the Session Close preamble, not claudemd_default.md, so those are
// where `gb pr open` / `gb pr ready` must be taught. gb pr shells to gh and these
// prompts are global across projects, so each keeps the GitLab
// gitlab_mr_merge path alongside the GitHub one.
func TestConfigBeadEntries_TeachGBPR(t *testing.T) {
	entries := configBeadEntries()
	nudges := entries["config:nudge-prompts:global"].(map[string]string)
	instr := entries["config:claude-instructions:global"].(map[string]string)

	for name, text := range map[string]string{
		"nudge adhoc":   nudges["adhoc"],
		"nudge default": nudges["default"],
		"session_close": instr["session_close"],
	} {
		for _, want := range []string{"gb pr open", "gb pr ready", "GitHub", "GitLab", "gitlab_mr_merge", "--close-on-execute"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s should mention %q (gd-NHxY3uJfF6), got: %q", name, want, text)
			}
		}
	}
	if thread := nudges["thread"]; !strings.Contains(thread, "gb pr open") || !strings.Contains(thread, "gb pr ready") {
		t.Errorf("thread nudge should name gb pr open / gb pr ready for its PR, got: %q", thread)
	}
}

// TestConfigBeadEntries_ThreadAgentEscalatesOverOpenPR guards gd-R9MVtPhw8j:
// the thread nudge said "open a PR ... close any bead you opened, then gb
// stop", and 23 of 64 orphaned PRs came from thread agents doing just that. A
// thread agent with a PR files the merge escalation (--requesting-task
// --close-on-execute), links the PR, and leaves the bead open. A thread agent
// has no task_id on its agent bead, so gb pr open/ready need --task.
func TestConfigBeadEntries_ThreadAgentEscalatesOverOpenPR(t *testing.T) {
	entries := configBeadEntries()
	nudges := entries["config:nudge-prompts:global"].(map[string]string)
	threadInstr := entries["config:claude-instructions:role:thread"].(map[string]string)

	nudge := nudges["thread"]
	for _, want := range []string{
		"gb pr open --task <id>", "gb pr ready --task <id>",
		"--requesting-task <id> --close-on-execute", "gitlab_mr_merge", "gb mr link",
		"Never `kd close` a bead whose PR is still open",
	} {
		if !strings.Contains(nudge, want) {
			t.Errorf("thread nudge should say %q (gd-R9MVtPhw8j), got: %q", want, nudge)
		}
	}
	if strings.Contains(nudge, "close any bead you opened") {
		t.Errorf("thread nudge still tells the agent to close its bead over an open PR (gd-R9MVtPhw8j): %q", nudge)
	}

	commands := threadInstr["commands"]
	for _, want := range []string{"leave the bead open", "gb pr ready --task <id>", "--requesting-task <id> --close-on-execute", "gb mr link"} {
		if !strings.Contains(commands, want) {
			t.Errorf("thread workflow should say %q (gd-R9MVtPhw8j), got: %q", want, commands)
		}
	}
	if strings.Contains(commands, "commit, push, and close the bead") {
		t.Errorf("thread workflow still closes the bead over an open PR (gd-R9MVtPhw8j): %q", commands)
	}
	if !strings.Contains(threadInstr["lifecycle"], "its bead stays open") {
		t.Errorf("thread lifecycle should say a PR's bead stays open (gd-R9MVtPhw8j), got: %q", threadInstr["lifecycle"])
	}
}

// TestConfigBeadEntries_TeachGBWaitCI guards gd-PwyvuF0fHp: crews held Bash
// calls for up to the 1h ceiling polling `gh run watch` / `gh run list` because
// no prompt named `gb wait ci`. The spawn nudges and Session Close teach the
// push -> gb wait ci -> gb stop -> gb wait ci --result loop instead.
func TestConfigBeadEntries_TeachGBWaitCI(t *testing.T) {
	entries := configBeadEntries()
	nudges := entries["config:nudge-prompts:global"].(map[string]string)
	instr := entries["config:claude-instructions:global"].(map[string]string)

	for name, text := range map[string]string{
		"nudge adhoc":   nudges["adhoc"],
		"nudge default": nudges["default"],
		"session_close": instr["session_close"],
	} {
		for _, want := range []string{"gb wait ci --repo", "git rev-parse HEAD", "gb stop", "gb wait ci --result", "gh run watch"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s should mention %q (gd-PwyvuF0fHp), got: %q", name, want, text)
			}
		}
	}
}

// TestConfigBeadEntries_RoleOverridesExist verifies all expected role overrides
// are present in configBeadEntries().
func TestConfigBeadEntries_RoleOverridesExist(t *testing.T) {
	entries := configBeadEntries()
	roles := []string{"thread", "gardener", "polecat", "local"}
	for _, role := range roles {
		key := "config:claude-instructions:role:" + role
		raw, ok := entries[key]
		if !ok {
			t.Errorf("missing role override %q", key)
			continue
		}
		sections, ok := raw.(map[string]string)
		if !ok {
			t.Errorf("%q is not map[string]string", key)
			continue
		}
		if sections["lifecycle"] == "" {
			t.Errorf("role %q missing lifecycle section", role)
		}
	}
}

// TestConfigBeadEntries_CrewWorkflows pins the role:crew claude-instructions
// workflows section. Crew agents are task-spawned so they don't need the global
// "Common Workflows" finding-work loop or epic-breakdown recipe. However they DO
// need discovery commands (kd search, gb advice show) so they look up prior work
// and context before executing — the absence of these caused ka-0000006883
// (crew agent executed in isolation without any beads discovery).
//
// The override replaces the global workflows with a crew-specific discovery section.
// Crew must still INHERIT all other sections from global — assert the override
// touches ONLY workflows.
func TestConfigBeadEntries_CrewWorkflows(t *testing.T) {
	entries := configBeadEntries()
	raw, ok := entries["config:claude-instructions:role:crew"]
	if !ok {
		t.Fatal("configBeadEntries missing config:claude-instructions:role:crew")
	}
	sections, ok := raw.(map[string]string)
	if !ok {
		t.Fatal("config:claude-instructions:role:crew is not map[string]string")
	}

	// workflows must be present to override the global section.
	wf, present := sections["workflows"]
	if !present {
		t.Error("role:crew override must set workflows explicitly (present) to override the global; missing key inherits it")
	}

	// The crew workflows section must surface discovery commands so agents
	// look up related prior work and advice before executing their task.
	if !strings.Contains(wf, "kd search") {
		t.Error("crew workflows must include kd search for discovery")
	}
	if !strings.Contains(wf, "gb advice show") {
		t.Error("crew workflows must include gb advice show for discovery")
	}

	// The crew workflows must NOT include the finding-work loop from global
	// (crew is task-spawned and does not need gb ready / gb news to find work).
	if strings.Contains(wf, "gb ready") {
		t.Error("crew workflows must not include gb ready (crew is task-spawned, not work-seeking)")
	}

	// The trim must be surgical: crew inherits every other section from global.
	// Overriding e.g. lifecycle or stop_gate here would silently strip crew's
	// stop/decision contract — assert the override carries no other keys.
	for k := range sections {
		if k != "workflows" {
			t.Errorf("role:crew override should trim ONLY workflows, but also sets %q — crew must inherit that section from global", k)
		}
	}

	// Guard against the global workflows being renamed in a way that would
	// make the crew override a silent no-op. The global must still have the
	// finding-work content that crew's override replaces.
	globalWorkflows := getGlobalInstructions(t)["workflows"]
	if !strings.Contains(globalWorkflows, "Starting work") || !strings.Contains(globalWorkflows, "gb ready") {
		t.Errorf("global workflows no longer contains the finding-work loop this crew override targets; re-point the override (got: %q)", globalWorkflows)
	}
}

// TestCapabilityProbe_NeverAssertsMergeDenial is the regression guard for bug
// ka-0000007570: the fleet repeatedly "convinced itself it is not allowed to
// merge gardenio MRs" because this probe — which lands in EVERY agent's cached
// system prompt at boot, above all advice — used to assert the flat, false,
// project-blind clause "CANNOT approve or self-merge protected main".
//
// It is false: on Acme/Platform/gardenio the bot is access_level=30 and
// protected main carries merge_access_levels=[{30,"Developers + Maintainers"}]
// with no approval rules, so agents can merge. The clause nonetheless kept
// regenerating merge escalations that no operator ever needed to action, and
// agents cited it to bypass the gitlab_mr_merge capability guard.
//
// The probe must therefore never hand an agent a blanket merge denial. It may
// only report what it PROBED, and any unknown must fall back to "check per-MR",
// not to "you cannot".
func TestCapabilityProbe_NeverAssertsMergeDenial(t *testing.T) {
	cmd := capabilityProbeCmd

	// Phrases that assert a denial the probe has not established.
	for _, banned := range []string{
		"CANNOT approve or self-merge",
		"cannot merge protected main)",
		"CANNOT merge",
		"is not allowed to merge",
		"operator-gated",
	} {
		if strings.Contains(cmd, banned) {
			t.Errorf("capabilityProbeCmd asserts a merge denial it did not probe: %q\n"+
				"The probe reaches every agent above all advice; a blanket denial here "+
				"regenerates the ka-0000007570 misapprehension fleet-wide.", banned)
		}
	}

	// It must actually probe live merge authority rather than assume it.
	for _, required := range []string{
		"protected_branches/",                     // reads the real branch-protection rule
		"merge_access_levels",                     // ... specifically the merge access levels
		"permissions.project_access.access_level", // ... and this token's role
	} {
		if !strings.Contains(cmd, required) {
			t.Errorf("capabilityProbeCmd no longer probes live merge authority: missing %q", required)
		}
	}

	// The unknown path must point at the authoritative per-MR answer and must
	// explicitly refuse to conclude "cannot merge".
	if !strings.Contains(cmd, "user.can_merge") {
		t.Error("capabilityProbeCmd must name user.can_merge as the per-MR authority when the branch probe is inconclusive")
	}
	if !strings.Contains(cmd, "never assume you cannot merge") {
		t.Error("capabilityProbeCmd must tell the agent not to assume a merge denial when unconfirmed")
	}

	// Capability must stay separated from policy, so this probe can never be
	// read as overriding a project rule like monorepo's "humans merge their own
	// things" (monorepo !2641 also reports user.can_merge=true — the difference
	// between the two repos is policy, not capability).
	if !strings.Contains(cmd, "project POLICY, not capability") {
		t.Error("capabilityProbeCmd must defer the should-I question to project policy")
	}

	// Branch protection is only half the merge story (bug ka-0000005160).
	// MEASURED 2026-08-27: monorepo grants merge at access_level 30 and the bot
	// IS 30, so the probe above prints PERMITS — but monorepo also requires an
	// approval the bot cannot give itself, and neither user.can_merge nor
	// merge_status reveals it (!2646: can_merge=true, merge_status=can_be_merged,
	// detailed_merge_status=not_approved). Without the approval-rule probe the
	// boot preamble teaches monorepo agents that a merge will work when it
	// provably will not, and detailed_merge_status is the only field that says so.
	// gardenio has zero approval rules, so the note never fires there.
	for _, required := range []string{
		"approval_rules",        // measures the live approval requirement
		"approvals_required",    // ... reading the actual count
		"detailed_merge_status", // ... and names the ONLY field that reports it
		"cannot approve your own MR",
	} {
		if !strings.Contains(cmd, required) {
			t.Errorf("capabilityProbeCmd must measure approval rules alongside branch protection: missing %q", required)
		}
	}
}

// shortTitleExample is model.ShortTitleExample (kbeads/internal/model, not
// importable here): the rendered example the kd flag help, the daemon's gate
// error and the advice line share (plan 2026-10-14-short-titles D4).
const shortTitleExample = "forward duration_ms via gb hook relay"

// TestInitPrompts_ShortTitleCreateLine pins the Essential Commands `kd create`
// line, global and thread variants: it names --short-title and shows the
// shared rendered example, so agents author the field before the gate
// enforces (gd-tBIIJcbHas).
func TestInitPrompts_ShortTitleCreateLine(t *testing.T) {
	if n := len([]rune(shortTitleExample)); n > 40 {
		t.Fatalf("shortTitleExample is %d runes, over the 40-rune limit", n)
	}
	entries := configBeadEntries()
	for _, key := range []string{"config:claude-instructions:global", "config:claude-instructions:role:thread"} {
		sections, ok := entries[key].(map[string]string)
		if !ok {
			t.Fatalf("%s is not map[string]string", key)
		}
		var line string
		for _, l := range strings.Split(sections["commands"], "\n") {
			if strings.HasPrefix(l, "- `kd create \"...\" --type=task|bug|feature") {
				line = l
				break
			}
		}
		if line == "" {
			t.Fatalf("%s commands: no `kd create \"...\"` line", key)
		}
		for _, needle := range []string{"--short-title", shortTitleExample, "the name humans see"} {
			if !strings.Contains(line, needle) {
				t.Errorf("%s kd create line missing %q:\n%s", key, needle, line)
			}
		}
	}
}

// TestInitPrompts_EveryKdCreateExampleHasShortTitle: every literal `kd create
// '<title>'` example the preamble teaches passes --short-title, so an agent
// copying one is not refused under BEADS_SHORT_TITLE_GATE=enforce.
func TestInitPrompts_EveryKdCreateExampleHasShortTitle(t *testing.T) {
	var texts []string
	for key, raw := range configBeadEntries() {
		switch v := raw.(type) {
		case string:
			texts = append(texts, key+"\x00"+v)
		case map[string]string:
			for section, s := range v {
				texts = append(texts, key+"/"+section+"\x00"+s)
			}
		}
	}
	for _, kv := range texts {
		where, text, _ := strings.Cut(kv, "\x00")
		lines := strings.Split(text, "\n")
		for i, l := range lines {
			for _, open := range []string{"kd create '", "kd create \""} {
				if !strings.Contains(l, open) {
					continue
				}
				// A shell example may continue on the next line (trailing `\`).
				stmt := l
				for j := i; strings.HasSuffix(strings.TrimSpace(lines[j]), "\\") && j+1 < len(lines); j++ {
					stmt += lines[j+1]
				}
				if !strings.Contains(stmt, "--short-title") {
					t.Errorf("%s: kd create example without --short-title:\n%s", where, stmt)
				}
			}
		}
	}
}

// TestEnsureConfigs_SeedsLocalRolePrompts: EnsureConfigs seeds both role:local
// beads (gd-rIHQbpZR0Y), so `BOAT_ROLE=local gb prime` and `gb nudge-prompt`
// resolve them through the global < project < role:local merge chain. The
// text must cover the bead workflow, the thread grammar and the role's limits.
func TestEnsureConfigs_SeedsLocalRolePrompts(t *testing.T) {
	setter := &mockConfigSetter{}
	if err := EnsureConfigs(context.Background(), setter, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("EnsureConfigs: %v", err)
	}
	seeded := func(title string) map[string]string {
		t.Helper()
		for _, b := range setter.beads {
			if b.Title != title || len(b.Labels) != 1 || b.Labels[0] != "role:local" {
				continue
			}
			var m map[string]string
			if err := json.Unmarshal([]byte(b.Description), &m); err != nil {
				t.Fatalf("unmarshal %s role:local: %v", title, err)
			}
			return m
		}
		t.Fatalf("EnsureConfigs did not seed a %s bead labelled role:local", title)
		return nil
	}

	instr := seeded("claude-instructions")
	wantInstr := map[string][]string{
		// The bead workflow and the thread grammar.
		"workflows": {"kd claim", "git switch -c", "gh pr create", "gb mr link", "kd close", "gb stop", "gb squawk", "gb decision create", "button", "gb yield"},
		// The role's limits and the laptop lifecycle.
		"lifecycle": {"owner's credentials", "permissions", "gb escalation create", "--requesting-task", "gb agent start --local --role local --task"},
	}
	for section, wants := range wantInstr {
		for _, w := range wants {
			if !strings.Contains(instr[section], w) {
				t.Errorf("claude-instructions role:local %s missing %q", section, w)
			}
		}
	}
	// Only the two sections are overridden; the rest inherit from global.
	if len(instr) != len(wantInstr) {
		t.Errorf("claude-instructions role:local overrides %d sections, want %d (workflows, lifecycle)", len(instr), len(wantInstr))
	}

	nudges := seeded("nudge-prompts")
	for _, key := range []string{"default", "adhoc"} {
		for _, w := range []string{"owner", "kd claim", "gb mr link", "gb stop", "gb squawk", "gb decision create", "gb escalation create", "{{.TaskHint}}"} {
			if !strings.Contains(nudges[key], w) {
				t.Errorf("nudge-prompts role:local %s missing %q", key, w)
			}
		}
	}
	if !strings.Contains(nudges["adhoc"], "{{.BoatPrompt}}") {
		t.Error("nudge-prompts role:local adhoc must carry {{.BoatPrompt}}")
	}
	if !strings.Contains(nudges["default"], "{{.ProjectHint}}") {
		t.Error("nudge-prompts role:local default must carry {{.ProjectHint}}")
	}
}

// TestConfigBeadEntries_ResearchBeforeTests pins the operator policy from
// Slack thread gd-UJiGnNn7Mc (2026-10-30): "research is cheap, running tests is
// expensive". Every agent's core rules must tell it to read the CI log and
// state a hypothesis before running tests, and then to run only the minimum
// subset — never a whole suite on a pod.
func TestConfigBeadEntries_ResearchBeforeTests(t *testing.T) {
	instr := configBeadEntries()["config:claude-instructions:global"].(map[string]string)
	core := instr["core_rules"]
	for _, want := range []string{
		"Research is cheap, running tests is expensive",
		"--log-failed",
		"hypothesis",
		"minimum subset",
		"-run TestName",
		"full suites belong in CI",
	} {
		if !strings.Contains(core, want) {
			t.Errorf("core_rules should mention %q (gd-WL5Rq7g47y), got: %q", want, core)
		}
	}
}

// TestConfigBeadEntries_DescribeEveryBashCall: an agent's Bash description is its caption in the garden; one
// without it shows blank (gd-Ty2eNoZipX), so every agent's core rules ask for it.
func TestConfigBeadEntries_DescribeEveryBashCall(t *testing.T) {
	core := configBeadEntries()["config:claude-instructions:global"].(map[string]string)["core_rules"]
	for _, want := range []string{"Describe every Bash call", "`description`", "caption in the garden"} {
		if !strings.Contains(core, want) {
			t.Errorf("core_rules should mention %q (gd-Ty2eNoZipX), got: %q", want, core)
		}
	}
}

// TestConfigBeadEntries_CapabilityGapEscalation pins the companion policy from
// the same thread: an agent that lacks an access or capability files an
// escalation for PERMANENT redress on behalf of its successors, makes the
// operational case, and is not asked to make the security case.
func TestConfigBeadEntries_CapabilityGapEscalation(t *testing.T) {
	instr := configBeadEntries()["config:claude-instructions:global"].(map[string]string)
	for name, text := range map[string]string{
		"core_rules": instr["core_rules"],
		"decisions":  instr["decisions"],
	} {
		for _, want := range []string{"permanent redress", "successors", "security case", "every agent's duty"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s should mention %q (gd-WL5Rq7g47y), got: %q", name, want, text)
			}
		}
	}
	if !strings.Contains(instr["core_rules"], "gb escalation create") {
		t.Error("core_rules should name `gb escalation create` as the redress path")
	}
}
