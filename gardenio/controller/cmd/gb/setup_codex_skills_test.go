package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// skillsTree builds a primary repo and one sibling ("alpha") with skills, plus a
// matching agents/commands entry that must NOT reach Codex.
func skillsTree(t *testing.T) (primary string, siblings []string) {
	t.Helper()
	base := t.TempDir()
	primary = filepath.Join(base, "work")
	alpha := filepath.Join(base, "repos", "alpha")
	for _, n := range []string{"keep", "video"} {
		writeExtDir(t, primary, "skills", n)
	}
	writeExtDir(t, alpha, "skills", "deploy")
	writeExtDir(t, primary, "agents", "reviewer")
	writeExtDir(t, primary, "commands", "ship")
	return primary, []string{alpha}
}

func TestLinkCodexSkills_AllowAllLinksPrimaryBareSiblingPrefixed(t *testing.T) {
	primary, siblings := skillsTree(t)
	home := t.TempDir()
	linkCodexSkills(home, primary, siblings, extensionFilter{})

	if got, want := linkedNames(t, home, "skills"), []string{"alpha-deploy", "keep", "video"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("skills = %v, want %v", got, want)
	}
	// The link must resolve to a real SKILL.md, which is what Codex reads.
	if _, err := os.Stat(filepath.Join(home, "skills", "keep", "SKILL.md")); err != nil {
		t.Errorf("linked skill has no readable SKILL.md: %v", err)
	}
	for _, sub := range []string{"agents", "commands"} {
		if _, err := os.Stat(filepath.Join(home, sub)); !os.IsNotExist(err) {
			t.Errorf("%s must not be linked into CODEX_HOME (err=%v)", sub, err)
		}
	}
}

// The role toggle gates Codex exactly as it gates Claude: same filter, same
// linked names, deny wins.
func TestLinkCodexSkills_HonoursRoleToggle(t *testing.T) {
	primary, siblings := skillsTree(t)
	roles := map[string]beadsapi.RoleInfo{
		"crew": {EnabledSkills: []string{"keep", "video", "alpha-deploy"}, DisabledSkills: []string{"video"}},
	}
	filter := fetchRoleExtensionFilters(context.Background(), fakeRoleLister{roles: roles}, "crew").skills
	home := t.TempDir()
	linkCodexSkills(home, primary, siblings, filter)

	if got, want := linkedNames(t, home, "skills"), []string{"alpha-deploy", "keep"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("skills = %v, want %v (video denied, deny wins over allow)", got, want)
	}
}

// Claude and Codex must agree for every role shape, since both consume one
// filter; this guards the "same toggles" requirement against a future fork.
func TestLinkCodexSkills_MatchesClaudeSet(t *testing.T) {
	roles := map[string]beadsapi.RoleInfo{
		"narrow": {EnabledSkills: []string{"keep"}},
		"deny":   {DisabledSkills: []string{"alpha-deploy"}},
	}
	for _, role := range []string{"", "narrow", "deny", "narrow,deny", "ghost"} {
		t.Run("role="+role, func(t *testing.T) {
			primary, siblings := skillsTree(t)
			filters := fetchRoleExtensionFilters(context.Background(), fakeRoleLister{roles: roles}, role)
			claudeDir, codexHome := t.TempDir(), t.TempDir()
			linkClaudeExtensions(claudeDir, primary, siblings, filters)
			linkCodexSkills(codexHome, primary, siblings, filters.skills)
			if c, x := linkedNames(t, claudeDir, "skills"), linkedNames(t, codexHome, "skills"); !reflect.DeepEqual(c, x) {
				t.Errorf("claude %v != codex %v", c, x)
			}
		})
	}
}

// A restart with a tightened toggle drops the now-disabled link, but leaves
// Codex's own real skill dirs and foreign symlinks alone.
func TestLinkCodexSkills_PrunesStaleRepoLinksOnly(t *testing.T) {
	primary, siblings := skillsTree(t)
	home := t.TempDir()
	linkCodexSkills(home, primary, siblings, extensionFilter{})

	system := filepath.Join(home, "skills", ".system", "imagegen")
	if err := os.MkdirAll(system, 0o755); err != nil {
		t.Fatal(err)
	}
	foreign := t.TempDir()
	if err := os.Symlink(foreign, filepath.Join(home, "skills", "installed")); err != nil {
		t.Fatal(err)
	}

	tight := extensionFilter{enabled: map[string]bool{"keep": true}, disabled: map[string]bool{}}
	linkCodexSkills(home, primary, siblings, tight)

	if got, want := linkedNames(t, home, "skills"), []string{".system", "installed", "keep"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("skills = %v, want %v", got, want)
	}
	if _, err := os.Stat(system); err != nil {
		t.Errorf("real .system dir was removed: %v", err)
	}
}

func TestLinkCodexSkills_NoSkillsLeavesHomeEmpty(t *testing.T) {
	base := t.TempDir()
	home := t.TempDir()
	linkCodexSkills(home, filepath.Join(base, "work"), nil, extensionFilter{})
	if _, err := os.Stat(filepath.Join(home, "skills")); !os.IsNotExist(err) {
		t.Errorf("expected no skills dir for a repo without skills (err=%v)", err)
	}
}

func TestSymlinkCodexSkills_NoProjectIsNoop(t *testing.T) {
	t.Setenv("BOAT_PROJECT", "")
	home := t.TempDir()
	symlinkCodexSkills(home, extensionFilter{})
	if _, err := os.Stat(filepath.Join(home, "skills")); !os.IsNotExist(err) {
		t.Errorf("expected no skills dir without BOAT_PROJECT (err=%v)", err)
	}
}
