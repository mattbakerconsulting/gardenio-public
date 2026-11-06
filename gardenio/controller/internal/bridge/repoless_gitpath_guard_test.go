package bridge

import (
	"bytes"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func TestRepoPathsInPrompt(t *testing.T) {
	tests := []struct {
		name   string
		prompt string
		want   []string
	}{
		{"empty", "", nil},
		{"no path", "Drive the epic to completion and close its children.", nil},
		{"prose slash not a path", "Handle the read/write and and/or cases carefully.", nil},
		{
			"distinctive top-level dir path",
			"Read your playbook at gardenio/docs/gardener-first-mate-prompt.md first.",
			[]string{"gardenio/docs/gardener-first-mate-prompt.md"},
		},
		{
			"distinctive top-level dir without file",
			"The design lives under gardenio/docs and the code under gardenio/controller.",
			[]string{"gardenio/docs", "gardenio/controller"},
		},
		{
			"nested source file by extension",
			"The guard is in internal/bridge/triggers.go near the fire loop.",
			[]string{"internal/bridge/triggers.go"},
		},
		{
			"glob path",
			"You run repo-less — do NOT try to open gardenio/docs/*.md, those 404.",
			[]string{"gardenio/docs/*.md"},
		},
		{
			"multiple distinct paths deduped",
			"See gardenio/docs/a.md and gardenio/docs/a.md and coop/crates/cli/main.rs.",
			[]string{"gardenio/docs/a.md", "coop/crates/cli/main.rs"},
		},
		{
			"url is excluded",
			"Open the MR at https://gitlab.com/acme/platform/gardenio/-/merge_requests/1.",
			nil,
		},
		{
			"url excluded but real path kept",
			"See https://gitlab.com/x/y and also kbeads/cmd/kd/main.go for the CLI.",
			[]string{"kbeads/cmd/kd/main.go"},
		},
		{
			"path in backticks",
			"Read `gardenio/docs/triggers.md` for the schema.",
			[]string{"gardenio/docs/triggers.md"},
		},
		{
			// A common-word top-level dir (scripts/docs/policy) is detected only
			// when it names an actual FILE (extension-anchored), never as bare
			// prose like "scripts/or".
			"common-word dir named as a file",
			"Run scripts/release.sh to cut a tag.",
			[]string{"scripts/release.sh"},
		},
		{
			"boundary after equals sign",
			"Pass --file=gardenio/docs/x.md to the tool.",
			[]string{"gardenio/docs/x.md"},
		},
		{
			"comma-joined paths both captured",
			"Read gardenio/docs/a.md,coop/crates/cli/b.rs before starting.",
			[]string{"gardenio/docs/a.md", "coop/crates/cli/b.rs"},
		},
		// --- FALSE-POSITIVE guards (must return nil) ---
		{"go import path not flagged", "Use github.com/spf13/cobra for the CLI.", nil},
		{"go import path with .go suffix not flagged", `import "github.com/x/y/z.go" there.`, nil},
		{"golang.org module path not flagged", "See golang.org/x/tools/cmd/goimports.go upstream.", nil},
		{"version string not a path", "Bump to version 1.2.3 today.", nil},
		{"date not a path", "The date 2026-07-11 is the cutoff.", nil},
		{"single-segment prose slash", "Handle read/write and and/or and N/A gracefully.", nil},
		{"docker image ref not a path", "registry.gitlab.com/acme/platform/gardenio/agent:latest is the image.", nil},
		{"common-word dir prose not flagged", "the policy/procedure is unclear and the docs/pages read fine.", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RepoPathsInPrompt(tt.prompt)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("RepoPathsInPrompt(%q)\n got: %#v\nwant: %#v", tt.prompt, got, tt.want)
			}
		})
	}
}

func TestUnreachableRepoPaths(t *testing.T) {
	const pathPrompt = "Read gardenio/docs/gardener-first-mate-prompt.md before starting."
	tests := []struct {
		name        string
		prompt      string
		project     string
		action      string
		wantPaths   []string
		unreachable bool
	}{
		{
			name:        "repo-less spawn naming a path is unreachable",
			prompt:      pathPrompt,
			project:     "",
			action:      "spawn_agent",
			wantPaths:   []string{"gardenio/docs/gardener-first-mate-prompt.md"},
			unreachable: true,
		},
		{
			name:        "has project -> pod clones -> reachable",
			prompt:      pathPrompt,
			project:     "gardenio",
			action:      "spawn_agent",
			wantPaths:   []string{"gardenio/docs/gardener-first-mate-prompt.md"},
			unreachable: false,
		},
		{
			name:        "apply_formula spawns a repo-less driver -> unreachable",
			prompt:      pathPrompt,
			project:     "",
			action:      "apply_formula",
			wantPaths:   []string{"gardenio/docs/gardener-first-mate-prompt.md"},
			unreachable: true,
		},
		{
			name:        "non-spawn action (notify) never unreachable",
			prompt:      pathPrompt,
			project:     "",
			action:      "notify",
			wantPaths:   []string{"gardenio/docs/gardener-first-mate-prompt.md"},
			unreachable: false,
		},
		{
			name:        "create_bead action never unreachable",
			prompt:      pathPrompt,
			project:     "",
			action:      "create_bead",
			wantPaths:   []string{"gardenio/docs/gardener-first-mate-prompt.md"},
			unreachable: false,
		},
		{
			name:        "no path -> nothing to warn",
			prompt:      "Drive the epic to done.",
			project:     "",
			action:      "spawn_agent",
			wantPaths:   nil,
			unreachable: false,
		},
		{
			name:        "repo-less-aware disclaimer suppresses the warn",
			prompt:      "You run repo-less — do NOT try to open gardenio/docs/*.md, those 404 at runtime. Read gb advice instead.",
			project:     "",
			action:      "spawn_agent",
			wantPaths:   []string{"gardenio/docs/*.md"},
			unreachable: false,
		},
		{
			name:        "no repo phrasing also suppresses",
			prompt:      "This pod has no repo checked out, so gardenio/docs/x.md is unavailable; use gb advice show.",
			project:     "",
			action:      "spawn_agent",
			wantPaths:   []string{"gardenio/docs/x.md"},
			unreachable: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			paths, unreachable := UnreachableRepoPaths(tt.prompt, tt.project, tt.action)
			if !reflect.DeepEqual(paths, tt.wantPaths) {
				t.Errorf("paths\n got: %#v\nwant: %#v", paths, tt.wantPaths)
			}
			if unreachable != tt.unreachable {
				t.Errorf("unreachable = %v, want %v", unreachable, tt.unreachable)
			}
		})
	}
}

func TestFieldsUnreachableRepoPaths(t *testing.T) {
	fields := map[string]any{
		"action":  "spawn_agent",
		"prompt":  "Read gardenio/docs/playbook.md first.",
		"enabled": true, // non-string sibling must not confuse extraction
	}
	paths, unreachable := FieldsUnreachableRepoPaths(fields)
	if !unreachable {
		t.Fatalf("expected unreachable for repo-less spawn naming a path, got false (paths=%v)", paths)
	}
	if len(paths) != 1 || paths[0] != "gardenio/docs/playbook.md" {
		t.Fatalf("paths = %v, want [gardenio/docs/playbook.md]", paths)
	}

	// Adding a project makes it reachable.
	fields["project"] = "gardenio"
	if _, unreachable := FieldsUnreachableRepoPaths(fields); unreachable {
		t.Fatalf("expected reachable once project is set")
	}
}

func TestBeadUnreachableRepoPaths(t *testing.T) {
	if _, unreachable := BeadUnreachableRepoPaths(nil); unreachable {
		t.Fatalf("nil bead must not be unreachable")
	}
	bead := &beadsapi.BeadDetail{
		Fields: map[string]string{
			"action": "spawn_agent",
			"prompt": "See gardenio/docs/first-mate.md for the full playbook.",
			// project field absent -> repo-less
		},
	}
	paths, unreachable := BeadUnreachableRepoPaths(bead)
	if !unreachable {
		t.Fatalf("expected unreachable, got false (paths=%v)", paths)
	}
	if len(paths) != 1 || paths[0] != "gardenio/docs/first-mate.md" {
		t.Fatalf("paths = %v", paths)
	}
}

func TestRepoPathWarning(t *testing.T) {
	msg := RepoPathWarning("ka-0000000498", []string{"gardenio/docs/b.md", "gardenio/docs/a.md"})
	// Paths sorted for stability.
	if !strings.Contains(msg, "gardenio/docs/a.md, gardenio/docs/b.md") {
		t.Errorf("expected sorted path list in warning, got:\n%s", msg)
	}
	if !strings.Contains(msg, "ka-0000000498") {
		t.Errorf("expected trigger id in warning, got:\n%s", msg)
	}
	if !strings.Contains(msg, "gb advice show") {
		t.Errorf("expected advice-inline remediation guidance, got:\n%s", msg)
	}
	// Empty trigger id renders <new>.
	if got := RepoPathWarning("", []string{"gardenio/docs/a.md"}); !strings.Contains(got, "<new>") {
		t.Errorf("expected <new> placeholder for empty id, got:\n%s", got)
	}
}

func TestWarnRepolessRepoPaths_WarnsOnce(t *testing.T) {
	const marker = "repo-less trigger names git path"

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	e := NewTriggerEngine(TriggerEngineConfig{Logger: logger})

	trig := TriggerDef{
		ID:     "ka-0000000498",
		Title:  "First Mate (repo-less)",
		Action: TriggerActionSpawnAgent,
		// Project empty -> repo-less.
		Prompt: "Read gardenio/docs/gardener-first-mate-prompt.md before starting.",
	}

	e.warnRepolessRepoPaths(trig)
	if got := strings.Count(buf.String(), marker); got != 1 {
		t.Fatalf("expected exactly 1 warning, got %d:\n%s", got, buf.String())
	}
	// Second evaluation of the same trigger must NOT re-warn.
	e.warnRepolessRepoPaths(trig)
	if got := strings.Count(buf.String(), marker); got != 1 {
		t.Fatalf("expected warn-once, got %d", got)
	}

	// A trigger WITH a project never warns (pod clones the repo).
	buf.Reset()
	e.warnRepolessRepoPaths(TriggerDef{
		ID:      "gd-gEO6FKXkmb",
		Action:  TriggerActionSpawnAgent,
		Project: "gardenio",
		Prompt:  "Read gardenio/docs/x.md.",
	})
	if buf.Len() != 0 {
		t.Fatalf("project-backed trigger should not warn, got:\n%s", buf.String())
	}

	// A repo-less-aware prompt never warns (the corrected fix shape).
	buf.Reset()
	e.warnRepolessRepoPaths(TriggerDef{
		ID:     "gd-acknowledged",
		Action: TriggerActionSpawnAgent,
		Prompt: "You run repo-less — do NOT open gardenio/docs/x.md, it 404s. Use gb advice show.",
	})
	if buf.Len() != 0 {
		t.Fatalf("repo-less-aware trigger should not warn, got:\n%s", buf.String())
	}

	// The repo-less warn key namespace is distinct from the unbacked-prose warn,
	// so BOTH can fire for the same trigger id.
	buf.Reset()
	both := TriggerDef{
		ID:     "gd-doubleFootgun",
		Action: TriggerActionSpawnAgent,
		Prompt: "Run budget: 24 runs. Read gardenio/docs/x.md first.",
	}
	e.warnUnbackedGardenerProse(both)
	e.warnRepolessRepoPaths(both)
	if got := strings.Count(buf.String(), "unbacked gardener prose"); got != 1 {
		t.Errorf("expected unbacked-prose warn, got %d:\n%s", got, buf.String())
	}
	if got := strings.Count(buf.String(), marker); got != 1 {
		t.Errorf("expected repo-less warn, got %d:\n%s", got, buf.String())
	}
}
