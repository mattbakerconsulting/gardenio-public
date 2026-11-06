package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// Tests for the per-role extension toggles — skills (Adam feedback #1, epic
// ka-0000004989) and sub-agents (ka-0000006908): the extensionFilter decision
// helper, its wiring into linkExtensionSubdir (skills + agents each gated by
// their own filter — commands always link), and the fetchRoleExtensionFilters
// multi-role union that reads enabled_skills / disabled_skills and
// enabled_agents / disabled_agents off role beads.

// extensionFilter.allows: deny wins over allow; an empty allow-list is unrestricted.
func TestSkillFilterAllows(t *testing.T) {
	cases := []struct {
		name    string
		enabled []string
		disable []string
		linked  string
		want    bool
	}{
		{"zero filter allows everything", nil, nil, "anything", true},
		{"allow-list hit", []string{"a", "b"}, nil, "a", true},
		{"allow-list miss", []string{"a", "b"}, nil, "c", false},
		{"deny-list hit", nil, []string{"x"}, "x", false},
		{"deny-list miss", nil, []string{"x"}, "y", true},
		{"deny wins over allow", []string{"a"}, []string{"a"}, "a", false},
		{"allow-list present, name not in it", []string{"a"}, []string{"z"}, "b", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := extensionFilter{enabled: map[string]bool{}, disabled: map[string]bool{}}
			for _, s := range tc.enabled {
				f.enabled[s] = true
			}
			for _, s := range tc.disable {
				f.disabled[s] = true
			}
			if got := f.allows(tc.linked); got != tc.want {
				t.Errorf("allows(%q) with enabled=%v disabled=%v = %v, want %v",
					tc.linked, tc.enabled, tc.disable, got, tc.want)
			}
		})
	}
}

// extensionFilter.active: true iff the filter would ever exclude an entry.
func TestSkillFilterActive(t *testing.T) {
	if (extensionFilter{}).active() {
		t.Error("zero filter reported active")
	}
	if !(extensionFilter{enabled: map[string]bool{"a": true}}).active() {
		t.Error("allow-list filter not reported active")
	}
	if !(extensionFilter{disabled: map[string]bool{"a": true}}).active() {
		t.Error("deny-list filter not reported active")
	}
}

// linkExtensionSubdir with a deny-list skips exactly the disabled skill and
// links the rest. Names are matched on the LINKED (post-namespacing) name, so a
// deny of the bare primary name and the "{repo}-"-prefixed sibling name each hit.
func TestLinkExtensionSubdirSkillDenyList(t *testing.T) {
	base := t.TempDir()
	primary := filepath.Join(base, "work")
	repoA := filepath.Join(base, "repos", "alpha")

	writeExtDir(t, primary, "skills", "keep")
	writeExtDir(t, primary, "skills", "drop")  // denied by bare name
	writeExtDir(t, repoA, "skills", "sibling") // -> alpha-sibling
	writeExtDir(t, repoA, "skills", "dropsib") // -> alpha-dropsib, denied

	filter := extensionFilter{
		enabled:  map[string]bool{},
		disabled: map[string]bool{"drop": true, "alpha-dropsib": true},
	}
	claudeDir := filepath.Join(t.TempDir(), ".claude")
	linkExtensionSubdir(claudeDir, "skills", primary, []string{repoA}, filter)

	got := linkedNames(t, claudeDir, "skills")
	want := []string{"alpha-sibling", "keep"}
	if len(got) != len(want) {
		t.Fatalf("linked = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("linked = %v, want %v", got, want)
		}
	}
}

// linkExtensionSubdir with a non-empty allow-list links ONLY the named skills.
func TestLinkExtensionSubdirSkillAllowList(t *testing.T) {
	base := t.TempDir()
	primary := filepath.Join(base, "work")
	repoA := filepath.Join(base, "repos", "alpha")

	writeExtDir(t, primary, "skills", "onboarding")
	writeExtDir(t, primary, "skills", "video")
	writeExtDir(t, repoA, "skills", "deploy") // -> alpha-deploy

	// Allow only "onboarding" and the namespaced "alpha-deploy".
	filter := extensionFilter{
		enabled:  map[string]bool{"onboarding": true, "alpha-deploy": true},
		disabled: map[string]bool{},
	}
	claudeDir := filepath.Join(t.TempDir(), ".claude")
	linkExtensionSubdir(claudeDir, "skills", primary, []string{repoA}, filter)

	got := linkedNames(t, claudeDir, "skills")
	want := []string{"alpha-deploy", "onboarding"}
	if len(got) != len(want) {
		t.Fatalf("linked = %v, want %v (allow-list should drop 'video')", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("linked = %v, want %v", got, want)
		}
	}
}

// NEG-1: a deny keyed on the UN-namespaced sibling name does NOT match the
// LINKED "{repo}-name". Operators must use the namespaced name to target a
// sibling skill, so denying the bare basename is a no-op for that sibling.
func TestLinkExtensionSubdirSkillDenyUnnamespaced(t *testing.T) {
	base := t.TempDir()
	primary := filepath.Join(base, "work")
	repoA := filepath.Join(base, "repos", "alpha")

	writeExtDir(t, repoA, "skills", "sibling") // links as alpha-sibling

	// Deny the bare "sibling" (wrong: the linked name is "alpha-sibling").
	filter := extensionFilter{
		enabled:  map[string]bool{},
		disabled: map[string]bool{"sibling": true},
	}
	claudeDir := filepath.Join(t.TempDir(), ".claude")
	linkExtensionSubdir(claudeDir, "skills", primary, []string{repoA}, filter)

	got := linkedNames(t, claudeDir, "skills")
	if len(got) != 1 || got[0] != "alpha-sibling" {
		t.Fatalf("linked = %v, want [alpha-sibling] (un-namespaced deny must NOT match the linked name)", got)
	}
}

// NEG-3: an allow-list naming ONLY non-existent skills links ZERO skills
// (fail-closed on a typo), and disabling a phantom skill is a no-op — the two
// real skills still link.
func TestLinkExtensionSubdirSkillPhantomNames(t *testing.T) {
	base := t.TempDir()
	primary := filepath.Join(base, "work")

	writeExtDir(t, primary, "skills", "onboarding")
	writeExtDir(t, primary, "skills", "video")

	// Allow-list of only non-existent skills → nothing links.
	allowPhantom := extensionFilter{
		enabled:  map[string]bool{"ghost": true, "phantom": true},
		disabled: map[string]bool{},
	}
	claudeDir := filepath.Join(t.TempDir(), ".claude")
	linkExtensionSubdir(claudeDir, "skills", primary, nil, allowPhantom)
	if got := linkedNames(t, claudeDir, "skills"); len(got) != 0 {
		t.Fatalf("phantom allow-list linked %v, want [] (fail-closed on typo)", got)
	}

	// Deny a phantom skill → both real skills still link.
	denyPhantom := extensionFilter{
		enabled:  map[string]bool{},
		disabled: map[string]bool{"nonexistent": true},
	}
	claudeDir2 := filepath.Join(t.TempDir(), ".claude")
	linkExtensionSubdir(claudeDir2, "skills", primary, nil, denyPhantom)
	got := linkedNames(t, claudeDir2, "skills")
	want := []string{"onboarding", "video"}
	if len(got) != len(want) {
		t.Fatalf("deny-phantom linked = %v, want %v (deny of a phantom must be a no-op)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("deny-phantom linked = %v, want %v", got, want)
		}
	}
}

// INT-3: the toggle matches sibling skills by their NAMESPACED linked name. A
// deny of the bare "deploy" drops the PRIMARY deploy but NOT a sibling's
// "otherrepo-deploy" — same base name, different linked name.
func TestLinkExtensionSubdirSkillDenyBareKeepsSibling(t *testing.T) {
	base := t.TempDir()
	primary := filepath.Join(base, "work")
	repoA := filepath.Join(base, "repos", "otherrepo")

	writeExtDir(t, primary, "skills", "deploy") // bare "deploy"
	writeExtDir(t, repoA, "skills", "deploy")   // -> otherrepo-deploy
	writeExtDir(t, repoA, "skills", "ship")     // -> otherrepo-ship

	// Deny only the bare primary "deploy".
	filter := extensionFilter{
		enabled:  map[string]bool{},
		disabled: map[string]bool{"deploy": true},
	}
	claudeDir := filepath.Join(t.TempDir(), ".claude")
	linkExtensionSubdir(claudeDir, "skills", primary, []string{repoA}, filter)

	got := linkedNames(t, claudeDir, "skills")
	want := []string{"otherrepo-deploy", "otherrepo-ship"}
	if len(got) != len(want) {
		t.Fatalf("linked = %v, want %v (bare deny drops only primary 'deploy', keeps namespaced siblings)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("linked = %v, want %v", got, want)
		}
	}
}

// linkClaudeExtensions applies the skills filter to SKILLS ONLY — agents and
// commands always link even when the role names them in disabled_skills (the
// skill toggle governs skills, not other extension kinds).
func TestLinkClaudeExtensionsFilterSkillsOnly(t *testing.T) {
	base := t.TempDir()
	primary := filepath.Join(base, "work")

	// A skill, an agent, and a command all sharing the name "shared".
	writeExtDir(t, primary, "skills", "shared")
	writeExtDir(t, primary, "skills", "keep")
	writeExtFile(t, primary, "agents", "shared.md")
	writeExtFile(t, primary, "commands", "shared.md")

	ws := filepath.Join(base, "ws")
	claudeDir := filepath.Join(ws, ".claude")
	// Deny the "shared" skill; agents/commands named "shared.md" must survive.
	filters := extensionFilters{
		skills: extensionFilter{enabled: map[string]bool{}, disabled: map[string]bool{"shared": true}},
	}
	linkClaudeExtensions(claudeDir, primary, nil, filters)

	// The disabled skill is dropped; the other skill survives.
	if names := linkedNames(t, claudeDir, "skills"); len(names) != 1 || names[0] != "keep" {
		t.Errorf("skills = %v, want [keep] (disabled 'shared' must be dropped)", names)
	}
	// Agents + commands ignore the skill filter entirely.
	if names := linkedNames(t, claudeDir, "agents"); len(names) != 1 || names[0] != "shared.md" {
		t.Errorf("agents = %v, want [shared.md] (skill filter must not touch agents)", names)
	}
	if names := linkedNames(t, claudeDir, "commands"); len(names) != 1 || names[0] != "shared.md" {
		t.Errorf("commands = %v, want [shared.md] (skill filter must not touch commands)", names)
	}
}

// linkClaudeExtensions applies the agents filter to AGENTS ONLY — the sub-agent
// toggle (ka-0000006908) drops linked agents by their LINKED file name without
// touching skills or commands, and matching sibling agents requires the
// namespaced "{repo}-" name just like the skill toggle.
func TestLinkClaudeExtensionsAgentToggle(t *testing.T) {
	base := t.TempDir()
	primary := filepath.Join(base, "work")
	repoA := filepath.Join(base, "repos", "alpha")

	// A skill, agents, and a command; "shared.md" exists as agent AND command.
	writeExtDir(t, primary, "skills", "shared")
	writeExtFile(t, primary, "agents", "shared.md")
	writeExtFile(t, primary, "agents", "keep.md")
	writeExtFile(t, repoA, "agents", "shared.md") // -> alpha-shared.md
	writeExtFile(t, primary, "commands", "shared.md")

	ws := filepath.Join(base, "ws")
	claudeDir := filepath.Join(ws, ".claude")
	// Deny the bare "shared.md" agent; the skill, the command, and the
	// namespaced sibling agent must all survive.
	filters := extensionFilters{
		agents: extensionFilter{enabled: map[string]bool{}, disabled: map[string]bool{"shared.md": true}},
	}
	linkClaudeExtensions(claudeDir, primary, []string{repoA}, filters)

	got := linkedNames(t, claudeDir, "agents")
	want := []string{"alpha-shared.md", "keep.md"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("agents = %v, want %v (bare deny drops only the primary agent)", got, want)
	}
	// Skills + commands ignore the agent filter entirely.
	if names := linkedNames(t, claudeDir, "skills"); len(names) != 1 || names[0] != "shared" {
		t.Errorf("skills = %v, want [shared] (agent filter must not touch skills)", names)
	}
	if names := linkedNames(t, claudeDir, "commands"); len(names) != 1 || names[0] != "shared.md" {
		t.Errorf("commands = %v, want [shared.md] (agent filter must not touch commands)", names)
	}
}

// An agents ALLOW-LIST links only the named agents — the wholesale-drop shape
// for a terminal Bash-only role is an allow-list naming nothing that exists
// (or denying every repo agent); here a single-entry allow-list drops the rest.
func TestLinkClaudeExtensionsAgentAllowList(t *testing.T) {
	base := t.TempDir()
	primary := filepath.Join(base, "work")

	writeExtFile(t, primary, "agents", "reviewer.md")
	writeExtFile(t, primary, "agents", "deployer.md")

	claudeDir := filepath.Join(base, "ws", ".claude")
	filters := extensionFilters{
		agents: extensionFilter{enabled: map[string]bool{"reviewer.md": true}, disabled: map[string]bool{}},
	}
	linkClaudeExtensions(claudeDir, primary, nil, filters)

	if got := linkedNames(t, claudeDir, "agents"); len(got) != 1 || got[0] != "reviewer.md" {
		t.Errorf("agents = %v, want [reviewer.md] (allow-list must drop deployer.md)", got)
	}
}

// fakeRoleLister is a minimal roleBeadLister for the fetch tests.
type fakeRoleLister struct {
	roles map[string]beadsapi.RoleInfo
	err   error
}

func (f fakeRoleLister) ListRoleBeads(_ context.Context) (map[string]beadsapi.RoleInfo, error) {
	return f.roles, f.err
}

func TestFetchRoleSkillFilter(t *testing.T) {
	roles := map[string]beadsapi.RoleInfo{
		"crew": {
			Name:           "crew",
			EnabledSkills:  []string{"onboarding", "video"},
			DisabledSkills: []string{"deploy"},
		},
		"triage": {
			Name:           "triage",
			EnabledSkills:  []string{"grafana"},
			DisabledSkills: []string{"video"},
		},
	}
	lister := fakeRoleLister{roles: roles}
	ctx := context.Background()

	// Single role → its own enabled/disabled sets.
	f := fetchRoleExtensionFilters(ctx, lister, "crew").skills
	if !f.enabled["onboarding"] || !f.enabled["video"] || f.enabled["grafana"] {
		t.Errorf("crew enabled = %v, want {onboarding,video}", f.enabled)
	}
	if !f.disabled["deploy"] {
		t.Errorf("crew disabled = %v, want {deploy}", f.disabled)
	}

	// Multi-role UNION: allow-lists union, deny wins across roles.
	// video is enabled by crew but disabled by triage → deny wins → excluded.
	f = fetchRoleExtensionFilters(ctx, lister, "crew,triage").skills
	for _, s := range []string{"onboarding", "video", "grafana"} {
		if !f.enabled[s] {
			t.Errorf("union enabled missing %q: %v", s, f.enabled)
		}
	}
	if !f.disabled["deploy"] || !f.disabled["video"] {
		t.Errorf("union disabled = %v, want {deploy,video}", f.disabled)
	}
	if f.allows("video") {
		t.Error("video allowed despite triage disabling it (deny must win across roles)")
	}
	if !f.allows("onboarding") {
		t.Error("onboarding blocked despite being in the union allow-list")
	}
	// grafana is in the allow-list union, deploy is not and is denied.
	if !f.allows("grafana") {
		t.Error("grafana blocked despite being in the union allow-list")
	}
	if f.allows("deploy") {
		t.Error("deploy allowed despite deny-list")
	}
}

// The agents toggle resolves through the same fetch with its own fields, and
// the two toggles stay independent: a role restricting skills leaves its
// agents filter allow-all, and vice versa.
func TestFetchRoleExtensionFiltersAgents(t *testing.T) {
	roles := map[string]beadsapi.RoleInfo{
		"gardener-cron": {
			Name:           "gardener-cron",
			DisabledAgents: []string{"reviewer.md", "helm-deployer.md"},
		},
		"crew": {
			Name:          "crew",
			EnabledSkills: []string{"code-review"},
		},
	}
	lister := fakeRoleLister{roles: roles}
	ctx := context.Background()

	filters := fetchRoleExtensionFilters(ctx, lister, "gardener-cron")
	if filters.skills.active() {
		t.Errorf("skills filter = %+v, want inactive (role sets only the agent toggle)", filters.skills)
	}
	if !filters.agents.disabled["reviewer.md"] || !filters.agents.disabled["helm-deployer.md"] {
		t.Errorf("agents disabled = %v, want {reviewer.md,helm-deployer.md}", filters.agents.disabled)
	}
	if filters.agents.allows("reviewer.md") {
		t.Error("reviewer.md allowed despite the role's disabled_agents")
	}
	if !filters.agents.allows("other.md") {
		t.Error("other.md blocked despite an empty agents allow-list")
	}

	// The converse: a skills-only role leaves the agents filter allow-all.
	filters = fetchRoleExtensionFilters(ctx, lister, "crew")
	if filters.agents.active() {
		t.Errorf("agents filter = %+v, want inactive (role sets only the skill toggle)", filters.agents)
	}
	if !filters.skills.active() {
		t.Error("skills filter inactive despite the role's enabled_skills")
	}

	// Multi-role: the agent deny-list unions with an unrestricted co-role
	// exactly like the skill toggle (deny still gates).
	filters = fetchRoleExtensionFilters(ctx, lister, "gardener-cron,crew")
	if filters.agents.allows("reviewer.md") {
		t.Error("reviewer.md allowed in multi-role despite gardener-cron's deny")
	}
}

// fetchRoleExtensionFilters degrades to allow-all (empty) filters on the edge
// cases: empty role string, nil lister, a daemon error, and an unknown role.
func TestFetchRoleSkillFilterDegrades(t *testing.T) {
	ctx := context.Background()
	roles := map[string]beadsapi.RoleInfo{"crew": {EnabledSkills: []string{"a"}, DisabledAgents: []string{"b.md"}}}

	check := func(name string, filters extensionFilters) {
		t.Helper()
		if filters.skills.active() {
			t.Errorf("%s produced an active skills filter", name)
		}
		if filters.agents.active() {
			t.Errorf("%s produced an active agents filter", name)
		}
	}
	// Empty role string → empty filters, no lister call needed.
	check("empty role string", fetchRoleExtensionFilters(ctx, fakeRoleLister{roles: roles}, ""))
	// Nil lister → empty filters.
	check("nil lister", fetchRoleExtensionFilters(ctx, nil, "crew"))
	// Daemon error → empty filters (best-effort).
	check("daemon error", fetchRoleExtensionFilters(ctx, fakeRoleLister{err: errors.New("boom")}, "crew"))
	// Unknown role (no bead) → empty filters.
	check("unknown role", fetchRoleExtensionFilters(ctx, fakeRoleLister{roles: roles}, "ghost"))
}

// unionExtensionFilter unions each role's EFFECTIVE entry set: a role with no
// allow-list (or no bead) grants EVERY entry, so pairing it with a restrictive
// co-role must NOT strip the unrestricted role's entries — the combined allow-list
// collapses to unrestricted, while deny-lists still union and gate. This is the
// deliberate divergence from account_whitelist (which narrows): the toggles
// fail toward granting the capability an operator gave a co-role.
func TestFetchRoleSkillFilterMixedAllowList(t *testing.T) {
	ctx := context.Background()
	roles := map[string]beadsapi.RoleInfo{
		// Restrictive role: only "x" allowed, "bad" denied.
		"scoped": {EnabledSkills: []string{"x"}, DisabledSkills: []string{"bad"}},
		// Unrestricted role: no allow-list → grants everything, denies "danger".
		"broad": {DisabledSkills: []string{"danger"}},
	}

	// scoped + broad: broad is unrestricted, so the allow-list collapses — every
	// skill is allowed EXCEPT the unioned deny-list ("bad", "danger").
	f := fetchRoleExtensionFilters(ctx, fakeRoleLister{roles: roles}, "scoped,broad").skills
	if len(f.enabled) != 0 {
		t.Errorf("allow-list = %v, want empty (broad is unrestricted → union is the universe)", f.enabled)
	}
	if !f.allows("x") || !f.allows("y") || !f.allows("anything-else") {
		t.Error("a skill outside the scoped allow-list was blocked despite an unrestricted co-role")
	}
	// Deny-lists still union and win.
	if !f.disabled["bad"] || !f.disabled["danger"] {
		t.Errorf("deny-list = %v, want {bad,danger} unioned across roles", f.disabled)
	}
	if f.allows("bad") {
		t.Error("bad allowed despite scoped's deny-list (deny must win even when allow-list collapses)")
	}
	if f.allows("danger") {
		t.Error("danger allowed despite broad's deny-list")
	}

	// A single restrictive role (no unrestricted co-role) still restricts — the
	// collapse only triggers when SOME role is unrestricted.
	only := fetchRoleExtensionFilters(ctx, fakeRoleLister{roles: roles}, "scoped").skills
	if only.allows("y") {
		t.Error("single scoped role should still restrict to its allow-list")
	}
	if !only.allows("x") {
		t.Error("single scoped role blocked its own allowed skill")
	}
}
