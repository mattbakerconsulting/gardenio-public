package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestInitCloneWorkDir(t *testing.T) {
	if got := initCloneWorkDir(""); got != "" {
		t.Errorf("empty project: got %q, want \"\"", got)
	}
	want := filepath.Join("/home/agent/bot", "myproj", "work")
	if got := initCloneWorkDir("myproj"); got != want {
		t.Errorf("initCloneWorkDir(\"myproj\") = %q, want %q", got, want)
	}
}

// TestRunSetupReposSkipsWhenInitCloneRan verifies the crashloop fix
// (ka-0000000649): when init-clone already materialized the primary at
// /home/agent/bot/{project}/work/.git, runSetupRepos must early-return WITHOUT
// touching the network or creating {workspace}/repos — otherwise it re-clones
// the multi-GB primary on coop's startup-probe critical path and crashloops the
// pod on slow-egress clusters. The guard keys on .../work/.git, so this holds
// even for a single-repo project that has no .../repos dir.
func TestRunSetupReposSkipsWhenInitCloneRan(t *testing.T) {
	const project = "testproj-initclone-guard"
	primaryGit := filepath.Join("/home/agent/bot", project, "work", ".git")
	if err := os.MkdirAll(primaryGit, 0o755); err != nil {
		t.Skipf("cannot create %s (sandbox restriction): %v", primaryGit, err)
	}
	t.Cleanup(func() { os.RemoveAll(filepath.Join("/home/agent/bot", project)) })

	t.Setenv("BOAT_PROJECT", project)
	// A bogus BOAT_PROJECTS the guard must NOT reach — if the early-return
	// regresses, resolveReposFromProjectBead/parseBoatProjectsEnv would try to
	// clone this and the {workspace}/repos dir would be created below.
	t.Setenv("BOAT_PROJECTS", project+"=https://gitlab.invalid/should/never-clone.git:zz")

	workspace := t.TempDir()
	if err := runSetupRepos(context.Background(), workspace); err != nil {
		t.Fatalf("runSetupRepos returned error, want nil (early-return): %v", err)
	}
	// The early-return happens before MkdirAll(reposDir), so {workspace}/repos
	// must not exist — proof no clone was attempted.
	if _, err := os.Stat(filepath.Join(workspace, "repos")); !os.IsNotExist(err) {
		t.Errorf("expected no {workspace}/repos (early-return before clone), stat err = %v", err)
	}
}

func TestInitCloneCacheDir(t *testing.T) {
	t.Run("empty project or name returns empty", func(t *testing.T) {
		if got := initCloneCacheDir("", "monorepo"); got != "" {
			t.Errorf("empty project: got %q, want \"\"", got)
		}
		if got := initCloneCacheDir("monorepo", ""); got != "" {
			t.Errorf("empty name: got %q, want \"\"", got)
		}
	})

	t.Run("missing cache returns empty", func(t *testing.T) {
		// /home/agent/bot/<random>/repos/<random> won't exist in CI.
		if got := initCloneCacheDir("nonexistent-proj-xyz", "nonexistent-repo-xyz"); got != "" {
			t.Errorf("missing cache: got %q, want \"\"", got)
		}
	})

	t.Run("present cache with .git is returned", func(t *testing.T) {
		// initCloneCacheDir hardcodes the /home/agent/bot prefix, so this
		// only exercises the present-cache branch when that path is writable
		// (true on agent pods, may not be in all CI sandboxes).
		base := filepath.Join("/home/agent/bot", "testproj-cachecheck", "repos", "somerepo")
		gitDir := filepath.Join(base, ".git")
		if err := os.MkdirAll(gitDir, 0o755); err != nil {
			t.Skipf("cannot create %s (sandbox restriction): %v", gitDir, err)
		}
		t.Cleanup(func() { os.RemoveAll(filepath.Join("/home/agent/bot", "testproj-cachecheck")) })

		if got := initCloneCacheDir("testproj-cachecheck", "somerepo"); got != base {
			t.Errorf("present cache: got %q, want %q", got, base)
		}
	})
}

func TestParseBoatProjectsEnv(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []repoCloneEntry
	}{
		{
			name: "empty",
			raw:  "",
			want: nil,
		},
		{
			name: "single entry with prefix",
			raw:  "gardenio=https://github.com/joyousaversion/gardenio.git:kd",
			want: []repoCloneEntry{{Name: "gardenio", URL: "https://github.com/joyousaversion/gardenio.git"}},
		},
		{
			name: "two entries",
			raw:  "gardenio=https://github.com/joyousaversion/gardenio.git:kd,monorepo=https://gitlab.com/Acme/Platform/monorepo:PE",
			want: []repoCloneEntry{
				{Name: "gardenio", URL: "https://github.com/joyousaversion/gardenio.git"},
				{Name: "monorepo", URL: "https://gitlab.com/Acme/Platform/monorepo"},
			},
		},
		{
			name: "no prefix",
			raw:  "myrepo=https://github.com/org/repo",
			want: []repoCloneEntry{{Name: "myrepo", URL: "https://github.com/org/repo"}},
		},
		{
			name: "spaces around entries",
			raw:  " foo=https://host/foo:x , bar=https://host/bar:y ",
			want: []repoCloneEntry{
				{Name: "foo", URL: "https://host/foo"},
				{Name: "bar", URL: "https://host/bar"},
			},
		},
		{
			name: "malformed entry skipped",
			raw:  "noeq,good=https://host/good:x",
			want: []repoCloneEntry{{Name: "good", URL: "https://host/good"}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseBoatProjectsEnv(tc.raw)
			if len(got) != len(tc.want) {
				t.Fatalf("len: got %d, want %d\n  got:  %+v\n  want: %+v", len(got), len(tc.want), got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("entry[%d]: got %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestRepoNameFromURL(t *testing.T) {
	tests := []struct {
		url  string
		want string
	}{
		{"https://github.com/org/my-repo.git", "my-repo"},
		{"https://gitlab.com/Acme/Platform/monorepo", "monorepo"},
		{"https://github.com/org/repo", "repo"},
	}
	for _, tc := range tests {
		got := repoNameFromURL(tc.url)
		if got != tc.want {
			t.Errorf("repoNameFromURL(%q) = %q, want %q", tc.url, got, tc.want)
		}
	}
}
