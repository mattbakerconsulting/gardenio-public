package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func TestResolveProject(t *testing.T) {
	cases := []struct {
		name, flag, kd, boat, want string
	}{
		{"flag wins", "flagproj", "kdproj", "boatproj", "flagproj"},
		{"kd over boat", "", "kdproj", "boatproj", "kdproj"},
		{"boat alone", "", "", "boatproj", "boatproj"},
		{"all empty", "", "", "", ""},
		{"whitespace trimmed", "", "  kdproj  ", "", "kdproj"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("KD_PROJECT", tc.kd)
			t.Setenv("BOAT_PROJECT", tc.boat)
			if got := resolveProject(tc.flag); got != tc.want {
				t.Errorf("resolveProject(%q) with KD=%q BOAT=%q = %q, want %q", tc.flag, tc.kd, tc.boat, got, tc.want)
			}
		})
	}
}

// knownAccts is a tiny helper to build the live-account set the validator
// resolves --account against.
func knownAccts(names ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(names))
	for _, n := range names {
		m[n] = struct{}{}
	}
	return m
}

// validScaffold returns a scaffold that passes validation, so each test can
// mutate exactly one field to exercise one failure mode.
func validScaffold() projectScaffold {
	return projectScaffold{
		Name:          "acme",
		Prefix:        "kd",
		GitURL:        "https://gitlab.com/acme/widgets.git",
		DefaultBranch: "main",
		Account:       "acme-anthropic",
		Repos: []beadsapi.RepoEntry{
			{URL: "https://gitlab.com/acme/widgets.git", Role: "primary", Branch: "main"},
		},
	}
}

func TestValidateProjectScaffold_HappyPath(t *testing.T) {
	sc := validScaffold()
	if problems := validateProjectScaffold(sc, knownAccts("acme-anthropic")); len(problems) != 0 {
		t.Fatalf("expected no problems, got: %v", problems)
	}
}

func TestValidateProjectScaffold_MissingRequired(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*projectScaffold)
		wantPart string
	}{
		{
			name:     "empty name",
			mutate:   func(s *projectScaffold) { s.Name = "" },
			wantPart: "project name",
		},
		{
			name:     "Project: prefix in name",
			mutate:   func(s *projectScaffold) { s.Name = "Project: acme" },
			wantPart: `must not start with "Project: "`,
		},
		{
			name:     "missing git_url",
			mutate:   func(s *projectScaffold) { s.GitURL = ""; s.Repos = nil },
			wantPart: "--git-url is required",
		},
		{
			name:     "missing prefix",
			mutate:   func(s *projectScaffold) { s.Prefix = "" },
			wantPart: "--prefix is required",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sc := validScaffold()
			tt.mutate(&sc)
			problems := validateProjectScaffold(sc, knownAccts("acme-anthropic"))
			// Single-fault isolation: each mutation should trip exactly the one
			// problem it targets, not a cascade. Asserting the count guards
			// against an unrelated check firing on the same scaffold.
			if len(problems) != 1 || !strings.Contains(problems[0], tt.wantPart) {
				t.Fatalf("expected exactly one problem containing %q, got: %v", tt.wantPart, problems)
			}
		})
	}
}

func TestValidateProjectScaffold_BadGitURL(t *testing.T) {
	for _, bad := range []string{"widgets", "acme/widgets", "/local/path", "ftp://x"} {
		sc := validScaffold()
		sc.GitURL = bad
		// Keep repos[0] valid so the only complaint is about the URL shape.
		sc.Repos[0].URL = "https://gitlab.com/acme/widgets.git"
		problems := validateProjectScaffold(sc, knownAccts("acme-anthropic"))
		if !containsSubstr(problems, "does not look like a git remote URL") {
			t.Errorf("git_url %q: expected URL-shape complaint, got: %v", bad, problems)
		}
	}
}

func TestValidateProjectScaffold_GoodGitURLs(t *testing.T) {
	for _, good := range []string{
		"https://gitlab.com/acme/widgets.git",
		"http://internal.example/group/repo.git",
		"ssh://git@host/group/repo.git",
		"git@gitlab.com:acme/widgets.git",
		"git@self-hosted.example:team/repo.git", // host NOT pinned to gitlab.com
	} {
		if !looksLikeGitURL(good) {
			t.Errorf("expected %q to look like a git URL", good)
		}
	}
}

func TestValidateProjectScaffold_DanglingAccount(t *testing.T) {
	sc := validScaffold()
	sc.Account = "ghost-account"
	problems := validateProjectScaffold(sc, knownAccts("acme-anthropic"))
	if !containsSubstr(problems, "does not resolve to a live type:account bead") {
		t.Fatalf("expected dangling-account complaint, got: %v", problems)
	}

	// When the account list could not be loaded (nil), the dangling check is
	// skipped — the rest of the scaffold is still valid.
	if problems := validateProjectScaffold(sc, nil); containsSubstr(problems, "does not resolve") {
		t.Fatalf("dangling check should be skipped when knownAccounts is nil, got: %v", problems)
	}
}

func TestValidateProjectScaffold_EmptyAccountIsValid(t *testing.T) {
	// An empty --account is allowed at create time (the success output tells the
	// operator to wire one). It must NOT trip the dangling-reference check.
	sc := validScaffold()
	sc.Account = ""
	if problems := validateProjectScaffold(sc, knownAccts("acme-anthropic")); len(problems) != 0 {
		t.Fatalf("empty account should be valid, got: %v", problems)
	}
}

func TestValidateProjectScaffold_ReposShape(t *testing.T) {
	t.Run("empty url in entry", func(t *testing.T) {
		sc := validScaffold()
		sc.Repos = []beadsapi.RepoEntry{{URL: "", Role: "primary"}}
		problems := validateProjectScaffold(sc, knownAccts("acme-anthropic"))
		if !containsSubstr(problems, "empty url") {
			t.Fatalf("expected empty-url complaint, got: %v", problems)
		}
	})

	t.Run("no primary entry", func(t *testing.T) {
		sc := validScaffold()
		sc.Repos = []beadsapi.RepoEntry{
			{URL: "https://gitlab.com/acme/ref.git", Role: "reference", Branch: "main"},
		}
		problems := validateProjectScaffold(sc, knownAccts("acme-anthropic"))
		if !containsSubstr(problems, "no primary entry") {
			t.Fatalf("expected no-primary complaint, got: %v", problems)
		}
	})

	t.Run("unset role counts as primary", func(t *testing.T) {
		sc := validScaffold()
		sc.Repos = []beadsapi.RepoEntry{
			{URL: "https://gitlab.com/acme/widgets.git", Branch: "main"}, // no Role
		}
		problems := validateProjectScaffold(sc, knownAccts("acme-anthropic"))
		if containsSubstr(problems, "no primary entry") {
			t.Fatalf("unset role should count as primary, got: %v", problems)
		}
	})

	t.Run("non-empty but malformed repo url is flagged", func(t *testing.T) {
		// A repos[] URL that is non-empty but not a real remote (e.g. carried
		// in via --from) would clone-fail at spawn with no up-front signal —
		// validation must catch it, not just the empty case.
		sc := validScaffold()
		sc.Repos = []beadsapi.RepoEntry{
			{URL: "acme/widgets", Role: "primary", Branch: "main"},
		}
		problems := validateProjectScaffold(sc, knownAccts("acme-anthropic"))
		if !containsSubstr(problems, "does not look like a git remote URL") {
			t.Fatalf("expected repo-url-shape complaint, got: %v", problems)
		}
	})
}

func TestValidateProjectScaffold_EmptyDefaultBranch(t *testing.T) {
	// A --from clone of a source bead lacking default_branch leaves it empty;
	// init-clone would then clone a nonexistent ref. Validation must reject it.
	sc := validScaffold()
	sc.DefaultBranch = ""
	// Keep repos[0] branch non-empty so the only complaint is the project-level
	// default_branch, isolating the check under test.
	problems := validateProjectScaffold(sc, knownAccts("acme-anthropic"))
	if !containsSubstr(problems, "--default-branch is required") {
		t.Fatalf("expected empty default_branch complaint, got: %v", problems)
	}
}

func TestShapeCheckRepos(t *testing.T) {
	t.Run("valid array", func(t *testing.T) {
		if err := shapeCheckRepos(`[{"url":"https://x/y.git","role":"primary"}]`); err != nil {
			t.Fatalf("expected valid, got: %v", err)
		}
	})
	t.Run("object instead of array", func(t *testing.T) {
		if err := shapeCheckRepos(`{"url":"https://x/y.git"}`); err == nil {
			t.Fatal("expected error for object-shaped repos")
		}
	})
	t.Run("string array silently dropped by controller", func(t *testing.T) {
		// A []string is exactly the shape the controller's lax json.Unmarshal
		// into []RepoEntry rejects (and therefore silently drops). We must flag it.
		if err := shapeCheckRepos(`["https://x/y.git"]`); err == nil {
			t.Fatal("expected error for string-array repos")
		}
	})
	t.Run("trailing garbage", func(t *testing.T) {
		if err := shapeCheckRepos(`[{"url":"https://x/y.git"}] trailing`); err == nil {
			t.Fatal("expected error for trailing data")
		}
	})
}

func TestProjectScaffoldFields(t *testing.T) {
	sc := validScaffold()
	fields, err := sc.fields()
	if err != nil {
		t.Fatalf("fields(): %v", err)
	}
	for k, want := range map[string]string{
		"prefix":         "kd",
		"git_url":        "https://gitlab.com/acme/widgets.git",
		"default_branch": "main",
		"account":        "acme-anthropic",
	} {
		if fields[k] != want {
			t.Errorf("fields[%q] = %q, want %q", k, fields[k], want)
		}
	}

	// repos round-trips back into []RepoEntry exactly (the controller parser
	// must be able to read what we write).
	var repos []beadsapi.RepoEntry
	if err := json.Unmarshal([]byte(fields["repos"]), &repos); err != nil {
		t.Fatalf("repos field is not valid []RepoEntry JSON: %v", err)
	}
	if len(repos) != 1 || repos[0].URL != sc.GitURL || repos[0].Role != "primary" {
		t.Fatalf("repos round-trip mismatch: %+v", repos)
	}
}

func TestProjectScaffoldFields_OmitsEmpty(t *testing.T) {
	// Only the fields the operator actually set should be emitted, so the bead
	// stays minimal and legacy-compatible (no empty account=, etc.).
	sc := projectScaffold{
		Name:   "bare",
		Prefix: "kd",
		GitURL: "https://gitlab.com/acme/widgets.git",
	}
	fields, err := sc.fields()
	if err != nil {
		t.Fatalf("fields(): %v", err)
	}
	if _, ok := fields["account"]; ok {
		t.Errorf("account should be omitted when empty, got: %q", fields["account"])
	}
	if _, ok := fields["repos"]; ok {
		t.Errorf("repos should be omitted when empty, got: %q", fields["repos"])
	}
	if _, ok := fields["default_branch"]; ok {
		t.Errorf("default_branch should be omitted when empty")
	}
}

func TestApplyProjectSource(t *testing.T) {
	src := beadsapi.ProjectInfo{
		Name:          "acme", // must NOT be copied
		Prefix:        "ac",
		GitURL:        "https://gitlab.com/acme/old.git",
		DefaultBranch: "trunk",
		Account:       "acme-anthropic",
		Repos: []beadsapi.RepoEntry{
			{URL: "https://gitlab.com/acme/old.git", Role: "primary", Branch: "trunk"},
		},
	}
	sc := projectScaffold{Name: "acme-staging"}
	applyProjectSource(&sc, src)

	if sc.Name != "acme-staging" {
		t.Errorf("name should not be overwritten by source: %q", sc.Name)
	}
	if sc.Prefix != "ac" || sc.GitURL != src.GitURL || sc.DefaultBranch != "trunk" || sc.Account != "acme-anthropic" {
		t.Errorf("cloned fields mismatch: %+v", sc)
	}
	if len(sc.Repos) != 1 || sc.Repos[0].URL != src.GitURL {
		t.Errorf("repos not cloned: %+v", sc.Repos)
	}

	// The clone must be a copy, not an alias — mutating the scaffold's repos
	// must not reach back into the source slice.
	sc.Repos[0].URL = "mutated"
	if src.Repos[0].URL == "mutated" {
		t.Error("applyProjectSource aliased the source repos slice instead of copying it")
	}
}

func TestEnsurePrimaryRepo(t *testing.T) {
	t.Run("synthesizes when empty", func(t *testing.T) {
		sc := projectScaffold{GitURL: "https://gitlab.com/acme/widgets.git", DefaultBranch: "main"}
		ensurePrimaryRepo(&sc, "primary")
		if len(sc.Repos) != 1 || sc.Repos[0].URL != sc.GitURL || sc.Repos[0].Role != "primary" || sc.Repos[0].Branch != "main" {
			t.Fatalf("expected one synthesized primary entry, got: %+v", sc.Repos)
		}
	})

	t.Run("discards a stale cloned primary in favour of git_url+default_branch (the --from footgun)", func(t *testing.T) {
		// Cloned-from repos[] points at the OLD repo on the OLD branch with a
		// stale Name; the git_url + default_branch override must win wholesale,
		// because init-clone reads repos[] before falling back to git_url.
		sc := projectScaffold{
			GitURL:        "https://gitlab.com/acme/new.git",
			DefaultBranch: "main",
			Repos: []beadsapi.RepoEntry{
				{URL: "https://gitlab.com/acme/old.git", Role: "primary", Branch: "trunk", Name: "old"},
			},
		}
		ensurePrimaryRepo(&sc, "primary")
		if len(sc.Repos) != 1 {
			t.Fatalf("expected exactly one canonical primary, got: %+v", sc.Repos)
		}
		got := sc.Repos[0]
		if got.URL != "https://gitlab.com/acme/new.git" || got.Branch != "main" || got.Name != "" {
			t.Fatalf("stale primary not fully replaced: %+v", got)
		}
	})

	t.Run("folds two primary-eligible entries into one canonical primary", func(t *testing.T) {
		sc := projectScaffold{
			GitURL:        "https://gitlab.com/acme/widgets.git",
			DefaultBranch: "main",
			Repos: []beadsapi.RepoEntry{
				{URL: "https://gitlab.com/acme/a.git", Role: "primary", Branch: "main"},
				{URL: "https://gitlab.com/acme/b.git", Role: "", Branch: "main"}, // also primary-eligible
			},
		}
		ensurePrimaryRepo(&sc, "primary")
		if len(sc.Repos) != 1 {
			t.Fatalf("two primary-eligible entries should collapse to one, got: %+v", sc.Repos)
		}
		if sc.Repos[0].URL != sc.GitURL {
			t.Errorf("canonical primary should point at git_url, got: %+v", sc.Repos[0])
		}
	})

	t.Run("preserves reference repos in order after the primary", func(t *testing.T) {
		sc := projectScaffold{
			GitURL:        "https://gitlab.com/acme/main.git",
			DefaultBranch: "main",
			Repos: []beadsapi.RepoEntry{
				{URL: "https://gitlab.com/acme/ref.git", Role: "reference", Branch: "main"},
			},
		}
		ensurePrimaryRepo(&sc, "primary")
		if len(sc.Repos) != 2 {
			t.Fatalf("expected 2 repos (primary + reference), got: %+v", sc.Repos)
		}
		if sc.Repos[0].Role != "primary" || sc.Repos[0].URL != sc.GitURL {
			t.Errorf("expected canonical primary at index 0, got: %+v", sc.Repos[0])
		}
		if sc.Repos[1].Role != "reference" || sc.Repos[1].URL != "https://gitlab.com/acme/ref.git" {
			t.Errorf("reference repo was disturbed: %+v", sc.Repos[1])
		}
	})

	t.Run("empty git_url is a no-op", func(t *testing.T) {
		sc := projectScaffold{}
		ensurePrimaryRepo(&sc, "primary")
		if len(sc.Repos) != 0 {
			t.Fatalf("expected no repos for empty git_url, got: %+v", sc.Repos)
		}
	})

	t.Run("primary branch tracks default_branch, not a stale pin", func(t *testing.T) {
		// The new project's default_branch is authoritative for the primary —
		// a cloned primary's pinned branch must NOT leak through.
		sc := projectScaffold{
			GitURL:        "https://gitlab.com/acme/widgets.git",
			DefaultBranch: "main",
			Repos: []beadsapi.RepoEntry{
				{URL: "https://gitlab.com/acme/widgets.git", Role: "primary", Branch: "develop"},
			},
		}
		ensurePrimaryRepo(&sc, "primary")
		if sc.Repos[0].Branch != "main" {
			t.Errorf("primary branch should track default_branch, got: %q", sc.Repos[0].Branch)
		}
	})
}

func TestProjectScopeLabels(t *testing.T) {
	t.Run("single project:<current> label, no double-label", func(t *testing.T) {
		t.Setenv("KD_PROJECT", "gardenio")
		t.Setenv("BOAT_PROJECT", "")
		oldPrime := primeProject
		primeProject = ""
		t.Cleanup(func() { primeProject = oldPrime })

		labels := projectScopeLabels()
		if len(labels) != 1 || labels[0] != "project:gardenio" {
			t.Fatalf("expected single project:gardenio label, got: %v", labels)
		}
	})

	t.Run("no project resolves to no label", func(t *testing.T) {
		t.Setenv("KD_PROJECT", "")
		t.Setenv("BOAT_PROJECT", "")
		oldPrime := primeProject
		primeProject = ""
		t.Cleanup(func() { primeProject = oldPrime })

		if labels := projectScopeLabels(); labels != nil {
			t.Fatalf("expected nil labels when no project, got: %v", labels)
		}
	})
}

func TestPrintProjectCreateSuccess(t *testing.T) {
	var buf bytes.Buffer
	sc := validScaffold()
	printProjectCreateSuccess(&buf, "gd-NEW123", sc)
	out := buf.String()

	for _, want := range []string{
		"gd-NEW123",            // the new bead id
		"acme-git-credentials", // secret-prefix wiring guidance
		"GITLAB_TOKEN",         // the magic env name
		"--type=role",          // role-bead next step
		"ka-0000005921",        // runbook reference
		"BOAT_PROJECT=acme",    // env-based spawn hint (project is not a spawn flag)
		"gb start <name> acme", // explicit-name spawn alternative
	} {
		if !strings.Contains(out, want) {
			t.Errorf("success output missing %q\n---\n%s", want, out)
		}
	}

	// The spawn hint must never teach the invalid `gb spawn --project` flag: that
	// stale pattern is what re-seeds gardener trigger prompts flagged by
	// `gb lint trigger-prompts` (ka-0000006674). `gb spawn` has no --project flag.
	if strings.Contains(out, "gb spawn --project") {
		t.Errorf("success output teaches invalid `gb spawn --project` flag\n---\n%s", out)
	}
}

func TestPrintProjectCreateSuccess_NoAccountAddsAccountStep(t *testing.T) {
	var buf bytes.Buffer
	sc := validScaffold()
	sc.Account = ""
	printProjectCreateSuccess(&buf, "gd-NEW123", sc)
	out := buf.String()
	if !strings.Contains(out, "--type=account") {
		t.Errorf("expected an account-creation step when --account is empty\n---\n%s", out)
	}
}

// containsSubstr reports whether any string in problems contains sub.
func containsSubstr(problems []string, sub string) bool {
	for _, p := range problems {
		if strings.Contains(p, sub) {
			return true
		}
	}
	return false
}
