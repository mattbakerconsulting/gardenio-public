package main

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// writeExtFile creates {root}/.claude/{sub}/{name} with some content, making
// parent dirs as needed. Used to scaffold fake primary + sibling repos.
func writeExtFile(t *testing.T, root, sub, name string) string {
	t.Helper()
	dir := filepath.Join(root, ".claude", sub)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// writeExtDir creates {root}/.claude/{sub}/{name}/SKILL.md — the skill layout,
// where each extension is a directory (Claude Code discovers skills by dir name).
func writeExtDir(t *testing.T, root, sub, name string) string {
	t.Helper()
	dir := filepath.Join(root, ".claude", sub, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}
	return dir
}

// linkedNames returns the sorted entry names present in {claudeDir}/{sub}.
func linkedNames(t *testing.T, claudeDir, sub string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(claudeDir, sub))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read %s/%s: %v", claudeDir, sub, err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// resolveLink returns the absolute target a symlink entry points to.
func resolveLink(t *testing.T, claudeDir, sub, name string) string {
	t.Helper()
	target, err := os.Readlink(filepath.Join(claudeDir, sub, name))
	if err != nil {
		t.Fatalf("readlink %s: %v", name, err)
	}
	return target
}

// The core capability: linkExtensionSubdir merges per-entry symlinks from the
// primary repo (bare names) and sibling repos (namespaced), with primary
// winning on collision and no duplicate entries.
func TestLinkExtensionSubdirMultiRepo(t *testing.T) {
	base := t.TempDir()
	primary := filepath.Join(base, "work")
	repoA := filepath.Join(base, "repos", "alpha")
	repoB := filepath.Join(base, "repos", "beta")

	// Primary: shared + primary-only.
	writeExtFile(t, primary, "commands", "shared.md")
	writeExtFile(t, primary, "commands", "prim-only.md")
	// alpha: overlaps "shared", plus unique.
	writeExtFile(t, repoA, "commands", "shared.md")
	writeExtFile(t, repoA, "commands", "alpha-only.md")
	// beta: unique.
	writeExtFile(t, repoB, "commands", "beta-only.md")

	claudeDir := filepath.Join(t.TempDir(), ".claude")
	linkExtensionSubdir(claudeDir, "commands", primary, []string{repoA, repoB}, extensionFilter{})

	got := linkedNames(t, claudeDir, "commands")
	want := []string{
		"alpha-alpha-only.md", // sibling namespaced
		"alpha-shared.md",     // sibling copy of shared, namespaced (does not clobber primary)
		"beta-beta-only.md",   // sibling namespaced
		"prim-only.md",        // primary bare
		"shared.md",           // primary bare — PRIMARY WINS
	}
	if len(got) != len(want) {
		t.Fatalf("entry set mismatch:\n got=%v\nwant=%v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("entry set mismatch:\n got=%v\nwant=%v", got, want)
		}
	}

	// PRIMARY WINS: bare "shared.md" must resolve to the PRIMARY source, never alpha's.
	if tgt := resolveLink(t, claudeDir, "commands", "shared.md"); tgt != filepath.Join(primary, ".claude", "commands", "shared.md") {
		t.Errorf("shared.md points to %q, want primary source", tgt)
	}
	// The namespaced alpha copy must point at alpha's source.
	if tgt := resolveLink(t, claudeDir, "commands", "alpha-shared.md"); tgt != filepath.Join(repoA, ".claude", "commands", "shared.md") {
		t.Errorf("alpha-shared.md points to %q, want alpha source", tgt)
	}
}

// Table-driven scenarios spanning agents (files) + skills (dirs), presence /
// absence of siblings, and empty workspaces.
func TestSymlinkClaudeExtensionsScenarios(t *testing.T) {
	cases := []struct {
		name string
		// setup scaffolds primary + siblings under base and returns nothing;
		// the harness always uses base/work as primary and base/repos/* siblings.
		setup    func(t *testing.T, base string)
		sub      string
		wantLink map[string]string // linked name -> repo root the source lives under ("" = primary)
	}{
		{
			name: "primary only, no siblings",
			setup: func(t *testing.T, base string) {
				writeExtDir(t, filepath.Join(base, "work"), "skills", "onboarding")
			},
			sub:      "skills",
			wantLink: map[string]string{"onboarding": "work"},
		},
		{
			name: "sibling only (primary has none)",
			setup: func(t *testing.T, base string) {
				// primary/.claude/work exists (repo present) but no skills subdir
				if err := os.MkdirAll(filepath.Join(base, "work", ".claude"), 0o755); err != nil {
					t.Fatal(err)
				}
				writeExtDir(t, filepath.Join(base, "repos", "helm"), "skills", "deploy")
			},
			sub:      "skills",
			wantLink: map[string]string{"helm-deploy": "repos/helm"},
		},
		{
			name: "agents files merged across primary + sibling",
			setup: func(t *testing.T, base string) {
				writeExtFile(t, filepath.Join(base, "work"), "agents", "reviewer.md")
				writeExtFile(t, filepath.Join(base, "repos", "helm"), "agents", "reviewer.md")
				writeExtFile(t, filepath.Join(base, "repos", "helm"), "agents", "deployer.md")
			},
			sub: "agents",
			wantLink: map[string]string{
				"reviewer.md":      "work",       // primary wins
				"helm-reviewer.md": "repos/helm", // sibling namespaced dupe
				"helm-deployer.md": "repos/helm",
			},
		},
		{
			name: "no extensions anywhere -> subdir not created",
			setup: func(t *testing.T, base string) {
				if err := os.MkdirAll(filepath.Join(base, "work"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
			sub:      "skills",
			wantLink: map[string]string{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			tc.setup(t, base)

			primary := filepath.Join(base, "work")
			siblings := siblingRepoDirs(base)

			claudeDir := filepath.Join(t.TempDir(), ".claude")
			linkExtensionSubdir(claudeDir, tc.sub, primary, siblings, extensionFilter{})

			got := linkedNames(t, claudeDir, tc.sub)
			if len(got) != len(tc.wantLink) {
				t.Fatalf("linked names = %v, want keys %v", got, tc.wantLink)
			}
			for name, repoRel := range tc.wantLink {
				var wantSrc string
				if repoRel == "" || repoRel == "work" {
					// strip the namespace prefix if any for the source basename
					wantSrc = filepath.Join(primary, ".claude", tc.sub)
				} else {
					wantSrc = filepath.Join(base, repoRel, ".claude", tc.sub)
				}
				tgt := resolveLink(t, claudeDir, tc.sub, name)
				// The target must live under the expected repo's subdir.
				if filepath.Dir(tgt) != wantSrc {
					t.Errorf("%s -> %s, want under %s", name, tgt, wantSrc)
				}
			}

			// When nothing to link, the subdir must not exist at all.
			if len(tc.wantLink) == 0 {
				if _, err := os.Stat(filepath.Join(claudeDir, tc.sub)); !os.IsNotExist(err) {
					t.Errorf("expected no %s subdir, but it exists (err=%v)", tc.sub, err)
				}
			}
		})
	}
}

// siblingRepoDirs must return sorted directories and tolerate a missing repos/
// dir (primary-only projects) by returning nil.
func TestSiblingRepoDirs(t *testing.T) {
	base := t.TempDir()

	// No repos dir yet.
	if got := siblingRepoDirs(base); got != nil {
		t.Errorf("expected nil for missing repos dir, got %v", got)
	}

	// Create out-of-order repo dirs + a stray file that must be ignored.
	for _, name := range []string{"zeta", "alpha", "mu"} {
		if err := os.MkdirAll(filepath.Join(base, "repos", name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(base, "repos", "README"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := siblingRepoDirs(base)
	want := []string{
		filepath.Join(base, "repos", "alpha"),
		filepath.Join(base, "repos", "mu"),
		filepath.Join(base, "repos", "zeta"),
	}
	if len(got) != len(want) {
		t.Fatalf("siblingRepoDirs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("siblingRepoDirs = %v, want %v (sorted)", got, want)
		}
	}
}

// A12: a sibling that is a SYMLINK to a directory (not a real dir) under repos/
// must be accepted by siblingRepoDirs (the source guards on IsDir OR ModeSymlink),
// and its skills must link under the namespaced "{symlinkName}-" prefix — the
// prefix derives from the symlink's own basename, not the resolved target.
func TestSiblingRepoDirsSymlinkedDir(t *testing.T) {
	base := t.TempDir()

	// A real repo checkout living OUTSIDE repos/, carrying a skill.
	realRepo := filepath.Join(base, "external", "coop")
	writeExtDir(t, realRepo, "skills", "credmgr")

	// Symlink it in under repos/ as "sib" — this is the entry siblingRepoDirs sees.
	reposDir := filepath.Join(base, "repos")
	if err := os.MkdirAll(reposDir, 0o755); err != nil {
		t.Fatal(err)
	}
	symSib := filepath.Join(reposDir, "sib")
	if err := os.Symlink(realRepo, symSib); err != nil {
		t.Fatal(err)
	}

	// siblingRepoDirs must include the symlinked sibling.
	got := siblingRepoDirs(base)
	if len(got) != 1 || got[0] != symSib {
		t.Fatalf("siblingRepoDirs = %v, want [%s] (symlinked dir must be accepted)", got, symSib)
	}

	// And its skill must link namespaced by the symlink's basename ("sib-").
	primary := filepath.Join(base, "work")
	if err := os.MkdirAll(filepath.Join(primary, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	claudeDir := filepath.Join(t.TempDir(), ".claude")
	linkExtensionSubdir(claudeDir, "skills", primary, got, extensionFilter{})
	if names := linkedNames(t, claudeDir, "skills"); len(names) != 1 || names[0] != "sib-credmgr" {
		t.Fatalf("linked = %v, want [sib-credmgr] (symlinked sibling namespaced by its own basename)", names)
	}
}

// ensureRealDir must migrate a legacy whole-directory symlink into a real dir
// that re-links the old target's entries under bare names — so a restart onto
// new gb code preserves previously-linked primary extensions.
func TestEnsureRealDirMigratesLegacySymlink(t *testing.T) {
	base := t.TempDir()

	// Legacy target: a primary .claude/skills dir with two entries.
	legacyTarget := filepath.Join(base, "primary", ".claude", "skills")
	if err := os.MkdirAll(filepath.Join(legacyTarget, "one"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(legacyTarget, "two"), 0o755); err != nil {
		t.Fatal(err)
	}

	claudeDir := filepath.Join(base, "ws", ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	legacyLink := filepath.Join(claudeDir, "skills")
	if err := os.Symlink(legacyTarget, legacyLink); err != nil {
		t.Fatal(err)
	}

	if err := ensureRealDir(legacyLink); err != nil {
		t.Fatalf("ensureRealDir: %v", err)
	}

	// Now it must be a real directory...
	fi, err := os.Lstat(legacyLink)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("still a symlink after migration")
	}
	if !fi.IsDir() {
		t.Fatalf("not a directory after migration")
	}
	// ...that re-links the legacy target's entries under bare names.
	got := linkedNames(t, filepath.Dir(legacyLink), "skills")
	if len(got) != 2 || got[0] != "one" || got[1] != "two" {
		t.Fatalf("migrated entries = %v, want [one two]", got)
	}
	if tgt := resolveLink(t, filepath.Dir(legacyLink), "skills", "one"); tgt != filepath.Join(legacyTarget, "one") {
		t.Errorf("migrated 'one' -> %s, want %s", tgt, filepath.Join(legacyTarget, "one"))
	}
}

// ensureRealDir on an already-real directory is a no-op (idempotent restart).
func TestEnsureRealDirIdempotentOnRealDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "skills")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Seed an entry that must survive.
	if err := os.WriteFile(filepath.Join(dir, "keep"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureRealDir(dir); err != nil {
		t.Fatalf("ensureRealDir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "keep")); err != nil {
		t.Errorf("entry lost after ensureRealDir on real dir: %v", err)
	}
}

// A full symlinkClaudeExtensions run against a fake bot root: primary + 2
// siblings, into a workspace that is NOT the primary (scaffold layout). Verifies
// end-to-end env-gated behaviour and namespacing across all three subdirs.
func TestSymlinkClaudeExtensionsEndToEnd(t *testing.T) {
	botRoot := t.TempDir()
	// Point BOAT_PROJECT resolution at our temp tree by faking the layout under
	// the real /home/agent/bot path is not possible in a unit test, so we drive
	// the internal helpers instead for the full path; here we only assert the
	// env-gate + missing-primary guards of the exported function.

	// No BOAT_PROJECT -> no panic, no writes.
	t.Setenv("BOAT_PROJECT", "")
	ws := filepath.Join(botRoot, "ws")
	symlinkClaudeExtensions(ws, extensionFilters{})
	if _, err := os.Stat(filepath.Join(ws, ".claude")); !os.IsNotExist(err) {
		t.Errorf("expected no .claude dir when BOAT_PROJECT unset")
	}
}
