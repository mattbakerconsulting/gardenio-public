package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func TestDetectNudgeType(t *testing.T) {
	tests := []struct {
		name     string
		env      map[string]string
		assigned bool // standbyAssignedSentinel present?
		expected string
	}{
		{"default", nil, false, "default"},
		{"thread", map[string]string{"SLACK_THREAD_CHANNEL": "C123"}, false, "thread"},
		{"adhoc", map[string]string{"BOAT_PROMPT": "do something"}, false, "adhoc"},
		{"prewarmed unassigned", map[string]string{"BOAT_AGENT_STATE": "prewarmed"}, false, "prewarmed"},
		// A still-parked prewarmed pod keeps the standby prompt even if thread
		// env happens to be set — it has not been handed work yet.
		{"prewarmed unassigned over thread", map[string]string{
			"BOAT_AGENT_STATE":     "prewarmed",
			"SLACK_THREAD_CHANNEL": "C123",
		}, false, "prewarmed"},
		// Once the pool manager assigns work (sentinel present), the sticky
		// BOAT_AGENT_STATE=prewarmed env must no longer shadow the real nudge:
		// an assigned thread agent gets the thread prompt (ka-0000004623).
		{"prewarmed assigned to thread", map[string]string{
			"BOAT_AGENT_STATE":     "prewarmed",
			"SLACK_THREAD_CHANNEL": "C123",
		}, true, "thread"},
		// Assigned to ad-hoc work via a pre-assigned prompt.
		{"prewarmed assigned to adhoc", map[string]string{
			"BOAT_AGENT_STATE": "prewarmed",
			"BOAT_PROMPT":      "do something",
		}, true, "adhoc"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Redirect the sentinel into a temp dir so cases don't collide and
			// no real /tmp state leaks in. Restore afterward.
			orig := standbyAssignedSentinelPath
			standbyAssignedSentinelPath = filepath.Join(t.TempDir(), "standby_assigned")
			defer func() { standbyAssignedSentinelPath = orig }()
			if tt.assigned {
				if err := os.WriteFile(standbyAssignedSentinelPath, []byte("1"), 0644); err != nil {
					t.Fatalf("write sentinel: %v", err)
				}
			}

			// Clear relevant env vars.
			for _, k := range []string{"SLACK_THREAD_CHANNEL", "BOAT_PROMPT", "BOAT_AGENT_STATE"} {
				t.Setenv(k, "")
			}
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			got := detectNudgeType()
			if got != tt.expected {
				t.Errorf("detectNudgeType() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestSubstituteNudgeVars(t *testing.T) {
	vars := nudgeVars{
		Project:      "myproject",
		Role:         "gardener",
		ProjectHint:  " focus on myproject",
		TaskHint:     " task gd-123",
		MonorepoHint: " repos hint",
		BoatPrompt:   "do the thing",
	}

	tmpl := "Project: {{.Project}}, role:{{.Role}}, hint:{{.ProjectHint}}, task:{{.TaskHint}}, mono:{{.MonorepoHint}}, prompt:{{.BoatPrompt}}"
	got := substituteNudgeVars(tmpl, vars)
	expected := "Project: myproject, role:gardener, hint: focus on myproject, task: task gd-123, mono: repos hint, prompt:do the thing"

	if got != expected {
		t.Errorf("substituteNudgeVars() =\n%s\nwant:\n%s", got, expected)
	}
}

func TestBuildNudgeVars_RoleFromEnv(t *testing.T) {
	t.Setenv("BOAT_ROLE", "worker")
	t.Setenv("BOAT_TASK_ID", "")
	t.Setenv("BOAT_PROMPT", "")
	t.Setenv("BOAT_REFERENCE_REPOS", "")
	vars := buildNudgeVars()
	if vars.Role != "worker" {
		t.Errorf("Role = %q, want %q", vars.Role, "worker")
	}
}

func TestBuildNudgeVars_TaskHintFromEnv(t *testing.T) {
	t.Setenv("BOAT_TASK_ID", "gd-abc123")
	t.Setenv("BOAT_PROMPT", "")
	t.Setenv("BOAT_REFERENCE_REPOS", "")
	vars := buildNudgeVars()
	if !strings.Contains(vars.TaskHint, "gd-abc123") {
		t.Error("TaskHint should include BOAT_TASK_ID")
	}
}

func TestBuildNudgeVars_ClosedTaskSkipped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1/beads/gd-closed") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":     "gd-closed",
				"title":  "Old task",
				"status": "closed",
				"type":   "task",
			})
			return
		}
		http.Error(w, "not found", 404)
	}))
	defer srv.Close()

	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	defer func() { daemon = oldDaemon }()

	t.Setenv("BOAT_TASK_ID", "gd-closed")
	t.Setenv("BOAT_PROMPT", "")
	t.Setenv("BOAT_REFERENCE_REPOS", "")
	vars := buildNudgeVars()
	if vars.TaskHint != "" {
		t.Errorf("TaskHint should be empty for closed task, got: %q", vars.TaskHint)
	}
}

// TestBuildNudgeVars_DeferredTaskSkipped: a pre-assigned DEFERRED bead is
// unclaimable (kd claim rejects status=deferred just like closed), so its
// taskHint must be dropped — otherwise the spawn opens with a guaranteed
// claim error (ka-0000006195).
func TestBuildNudgeVars_DeferredTaskSkipped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1/beads/gd-deferred") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":     "gd-deferred",
				"title":  "Parked task",
				"status": "deferred",
				"type":   "task",
			})
			return
		}
		http.Error(w, "not found", 404)
	}))
	defer srv.Close()

	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	defer func() { daemon = oldDaemon }()

	t.Setenv("BOAT_TASK_ID", "gd-deferred")
	t.Setenv("BOAT_PROMPT", "")
	t.Setenv("BOAT_REFERENCE_REPOS", "")
	vars := buildNudgeVars()
	if vars.TaskHint != "" {
		t.Errorf("TaskHint should be empty for deferred task, got: %q", vars.TaskHint)
	}
}

func TestBuildNudgeVars_OpenTaskIncluded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1/beads/gd-open") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":     "gd-open",
				"title":  "Active task",
				"status": "open",
				"type":   "task",
			})
			return
		}
		http.Error(w, "not found", 404)
	}))
	defer srv.Close()

	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	defer func() { daemon = oldDaemon }()

	t.Setenv("BOAT_TASK_ID", "gd-open")
	t.Setenv("BOAT_PROMPT", "")
	t.Setenv("BOAT_REFERENCE_REPOS", "")
	vars := buildNudgeVars()
	if !strings.Contains(vars.TaskHint, "gd-open") {
		t.Errorf("TaskHint should include open task ID, got: %q", vars.TaskHint)
	}
}

// TestStepDispatchTarget parses the target step id out of a plan step-dispatch
// nudge and rejects any prompt that is not one — the classifier effectiveBoatPrompt
// gates on (ka-0000002376).
func TestStepDispatchTarget(t *testing.T) {
	cases := []struct {
		name   string
		prompt string
		wantID string
		wantOK bool
	}{
		{
			"full dispatch with title",
			`Please dispatch a crew agent to work step ka-0000002000 ("Chat composer: Enter sends") now — it's ready to start.`,
			"ka-0000002000", true,
		},
		{
			"dispatch without title",
			"Please dispatch a crew agent to work step gd-abc123 now — it's ready to start.",
			"gd-abc123", true,
		},
		{"leading/trailing space tolerated", "  Please dispatch a crew agent to work step gd-xyz  ", "gd-xyz", true},
		{"plain operator message is not a dispatch", "also the text I send doesn't always clear?", "", false},
		{"empty prompt", "", "", false},
		{"prefix but no id", "Please dispatch a crew agent to work step ", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, ok := stepDispatchTarget(tc.prompt)
			if ok != tc.wantOK || id != tc.wantID {
				t.Errorf("stepDispatchTarget(%q) = (%q, %v), want (%q, %v)", tc.prompt, id, ok, tc.wantID, tc.wantOK)
			}
		})
	}
}

// TestEffectiveBoatPrompt_DropsClosedStepDispatch: a persisted step-dispatch
// prompt whose target step is already closed must resolve to "" so a respawn
// does not re-emit the dead dispatch (ka-0000002376). A dispatch for a still-open
// step, and any non-dispatch prompt, are preserved.
func TestEffectiveBoatPrompt_DropsClosedStepDispatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/beads/gd-9em9s3brsg"):
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "gd-9em9s3brsg", "status": "closed", "type": "task"})
		case strings.HasPrefix(r.URL.Path, "/v1/beads/gd-deferredstep"):
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "gd-deferredstep", "status": "deferred", "type": "task"})
		case strings.HasPrefix(r.URL.Path, "/v1/beads/gd-openstep"):
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "gd-openstep", "status": "open", "type": "task"})
		default:
			http.Error(w, "not found", 404)
		}
	}))
	defer srv.Close()

	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	defer func() { daemon = oldDaemon }()

	closedSeed := `Please dispatch a crew agent to work step gd-9em9s3brsg ("done") now — it's ready to start.`
	deferredSeed := "Please dispatch a crew agent to work step gd-deferredstep now — it's ready to start."
	openSeed := "Please dispatch a crew agent to work step gd-openstep now — it's ready to start."
	plainMsg := "please tweak the header copy, it reads awkwardly"

	cases := []struct {
		name   string
		prompt string
		want   string
	}{
		{"closed step dispatch dropped", closedSeed, ""},
		{"deferred step dispatch dropped", deferredSeed, ""},
		{"open step dispatch preserved", openSeed, openSeed},
		{"plain operator message preserved", plainMsg, plainMsg},
		{"empty prompt stays empty", "", ""},
		// Unknown step id (daemon 404) is preserved — never silently swallow a
		// possibly-live dispatch on a lookup miss.
		{"unknown step id preserved", "Please dispatch a crew agent to work step gd-missing now — it's ready to start.", "Please dispatch a crew agent to work step gd-missing now — it's ready to start."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BOAT_PROMPT", tc.prompt)
			if got := effectiveBoatPrompt(); got != tc.want {
				t.Errorf("effectiveBoatPrompt() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDetectNudgeType_ClosedStepDispatchFallsToDefault: when the only BOAT_PROMPT
// is a stale closed-step dispatch, the nudge type must fall back to "default"
// rather than "adhoc" — otherwise every respawn re-runs the dead dispatch seed
// (ka-0000002376).
func TestDetectNudgeType_ClosedStepDispatchFallsToDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/v1/beads/gd-9em9s3brsg") {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "gd-9em9s3brsg", "status": "closed", "type": "task"})
			return
		}
		http.Error(w, "not found", 404)
	}))
	defer srv.Close()

	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	defer func() { daemon = oldDaemon }()

	// Neutralize the prewarmed/thread selectors so BOAT_PROMPT is the deciding input.
	orig := standbyAssignedSentinelPath
	standbyAssignedSentinelPath = filepath.Join(t.TempDir(), "standby_assigned")
	defer func() { standbyAssignedSentinelPath = orig }()
	t.Setenv("BOAT_AGENT_STATE", "")
	t.Setenv("SLACK_THREAD_CHANNEL", "")
	t.Setenv("BOAT_PROMPT", `Please dispatch a crew agent to work step gd-9em9s3brsg now — it's ready to start.`)

	if got := detectNudgeType(); got != "default" {
		t.Errorf("detectNudgeType() = %q, want %q (stale closed-step dispatch must not select adhoc)", got, "default")
	}
}

// TestBuildNudgeVars_ClosedStepDispatchPromptDropped: end-to-end, buildNudgeVars
// must leave BoatPrompt empty for a stale closed-step dispatch so the adhoc
// template renders no dispatch tail (ka-0000002376).
func TestBuildNudgeVars_ClosedStepDispatchPromptDropped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/v1/beads/gd-9em9s3brsg") {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "gd-9em9s3brsg", "status": "closed", "type": "task"})
			return
		}
		http.Error(w, "not found", 404)
	}))
	defer srv.Close()

	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	defer func() { daemon = oldDaemon }()

	t.Setenv("BOAT_TASK_ID", "")
	t.Setenv("BOAT_REFERENCE_REPOS", "")
	t.Setenv("BOAT_PROMPT", `Please dispatch a crew agent to work step gd-9em9s3brsg now — it's ready to start.`)
	vars := buildNudgeVars()
	if vars.BoatPrompt != "" {
		t.Errorf("BoatPrompt should be empty for a closed-step dispatch, got: %q", vars.BoatPrompt)
	}
}

// TestTaskHintFor_BranchesOnType: the pre-assignment hint must NOT tell an
// epic-anchored agent to `kd claim` (kd claim hard-rejects type=epic), while
// keeping the claim wording for claimable types. Guards ka-0000005855.
func TestTaskHintFor_BranchesOnType(t *testing.T) {
	cases := []struct {
		name       string
		beadType   string
		wantClaim  bool   // hint should contain "kd claim"
		wantSubstr string // a distinctive phrase the hint must contain
	}{
		{"task claims", "task", true, "pre-assigned to task"},
		{"bug claims", "bug", true, "pre-assigned to task"},
		{"feature claims", "feature", true, "pre-assigned to task"},
		// Unknown/empty types are treated as claimable — kd only rejects epic,
		// so the safe default keeps the existing claim wording.
		{"empty type claims", "", true, "pre-assigned to task"},
		{"epic anchors", "epic", false, "anchored to standing epic"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := taskHintFor("gd-xyz789", tc.beadType)
			if !strings.Contains(got, "gd-xyz789") {
				t.Errorf("hint should reference the bead ID, got: %q", got)
			}
			if hasClaim := strings.Contains(got, "kd claim"); hasClaim != tc.wantClaim {
				t.Errorf("type=%q: contains `kd claim` = %v, want %v (hint: %q)", tc.beadType, hasClaim, tc.wantClaim, got)
			}
			if !strings.Contains(got, tc.wantSubstr) {
				t.Errorf("type=%q: hint should contain %q, got: %q", tc.beadType, tc.wantSubstr, got)
			}
		})
	}
}

// TestBuildNudgeVars_EpicAnchorNoClaim: end-to-end, a pre-assigned type=epic
// bead must produce a TaskHint with anchor language and no `kd claim` — the
// guaranteed-error path from ka-0000005855.
func TestBuildNudgeVars_EpicAnchorNoClaim(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1/beads/gd-epic") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":     "gd-epic",
				"title":  "Standing lane epic",
				"status": "open",
				"type":   "epic",
			})
			return
		}
		http.Error(w, "not found", 404)
	}))
	defer srv.Close()

	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	defer func() { daemon = oldDaemon }()

	t.Setenv("BOAT_TASK_ID", "gd-epic")
	t.Setenv("BOAT_PROMPT", "")
	t.Setenv("BOAT_REFERENCE_REPOS", "")
	vars := buildNudgeVars()
	if strings.Contains(vars.TaskHint, "kd claim") {
		t.Errorf("epic TaskHint must not tell the agent to `kd claim`, got: %q", vars.TaskHint)
	}
	if !strings.Contains(vars.TaskHint, "anchored to standing epic") {
		t.Errorf("epic TaskHint should use anchor language, got: %q", vars.TaskHint)
	}
	if !strings.Contains(vars.TaskHint, "gd-epic") {
		t.Errorf("epic TaskHint should reference the epic ID, got: %q", vars.TaskHint)
	}
}

// TestReferenceReposHintFrom_InitCloneLayout: repos live at the init-clone
// sibling path ({botHome}/{project}/repos). The hint must name that path with
// the `ls ../repos/` command and must NOT warn against it. (The ka-0000005484
// bug told workspace pods `ls repos/` "will look empty" — the inverse of truth.)
func TestReferenceReposHintFrom_InitCloneLayout(t *testing.T) {
	botHome := t.TempDir()
	cwd := t.TempDir()
	project := "myproj"
	initRepos := filepath.Join(botHome, project, "repos")
	if err := os.MkdirAll(initRepos, 0o755); err != nil {
		t.Fatal(err)
	}
	// cwd/repos does NOT exist, so the init-clone candidate must win.
	got := referenceReposHintFrom(botHome, project, cwd, "")
	if !strings.Contains(got, "ls ../repos/") {
		t.Errorf("init-clone hint should tell agent to run `ls ../repos/`, got: %q", got)
	}
	if !strings.Contains(got, initRepos) {
		t.Errorf("hint should name the absolute init-clone path %q, got: %q", initRepos, got)
	}
	if strings.Contains(got, "will look empty") || strings.Contains(got, "too shallow") {
		t.Errorf("hint must not warn against the path that actually exists, got: %q", got)
	}
	if !strings.Contains(got, "primary repo is your current working directory") {
		t.Errorf("hint should retain the primary-repo sentence, got: %q", got)
	}
}

// TestReferenceReposHintFrom_WorkspaceLayout: repos live under cwd/repos
// (workspace/job-mode). The hint must name `ls repos/` and the cwd/repos path,
// even though the init-clone candidate is probed first (it does not exist).
func TestReferenceReposHintFrom_WorkspaceLayout(t *testing.T) {
	botHome := t.TempDir() // {botHome}/{project}/repos intentionally absent
	cwd := t.TempDir()
	project := "myproj"
	wsRepos := filepath.Join(cwd, "repos")
	if err := os.MkdirAll(wsRepos, 0o755); err != nil {
		t.Fatal(err)
	}
	got := referenceReposHintFrom(botHome, project, cwd, "")
	if !strings.Contains(got, "ls repos/") {
		t.Errorf("workspace hint should tell agent to run `ls repos/`, got: %q", got)
	}
	if !strings.Contains(got, wsRepos) {
		t.Errorf("hint should name the absolute workspace repos path %q, got: %q", wsRepos, got)
	}
	// Must not describe the (non-existent) init-clone `ls ../repos/` command.
	if strings.Contains(got, "ls ../repos/") {
		t.Errorf("workspace hint must not point at the non-existent sibling path, got: %q", got)
	}
	if !strings.Contains(got, "primary repo is your current working directory") {
		t.Errorf("hint should retain the primary-repo sentence, got: %q", got)
	}
}

// TestReferenceReposHintFrom_PrefersInitCloneWhenCwdIsWorkDir: on a real
// init-clone pod, cwd IS the {botHome}/{project}/work checkout and BOTH the
// sibling {botHome}/{project}/repos and a {cwd}/repos subdir may exist. The
// `ls ../repos/` sibling form is provably correct from that cwd, so it must win.
func TestReferenceReposHintFrom_PrefersInitCloneWhenCwdIsWorkDir(t *testing.T) {
	botHome := t.TempDir()
	project := "myproj"
	// cwd is exactly the init-clone checkout path so `ls ../repos/` resolves to
	// the sibling reference repos.
	cwd := filepath.Join(botHome, project, "work")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	initRepos := filepath.Join(botHome, project, "repos")
	if err := os.MkdirAll(initRepos, 0o755); err != nil {
		t.Fatal(err)
	}
	// A {cwd}/repos subdir also present — it must NOT shadow the sibling.
	if err := os.MkdirAll(filepath.Join(cwd, "repos"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := referenceReposHintFrom(botHome, project, cwd, "")
	if !strings.Contains(got, "ls ../repos/") || !strings.Contains(got, initRepos) {
		t.Errorf("when cwd is the init-clone work dir, the sibling `ls ../repos/` should win, got: %q", got)
	}
}

// TestReferenceReposHintFrom_WorkspaceCwdWithStaleInitClone reproduces
// ka-0000005591: a workspace/job-mode pod (cwd is a workspace root, NOT the
// init-clone {project}/work checkout) whose real reference repos live at
// {cwd}/repos, while a stale {botHome}/{project}/repos ALSO exists. The bug was
// that the stale sibling shadowed the real repos and the hint emitted the bare
// `ls ../repos/` botHome form, which is wrong from a workspace cwd. The hint must
// point at {cwd}/repos with `ls repos/` instead.
func TestReferenceReposHintFrom_WorkspaceCwdWithStaleInitClone(t *testing.T) {
	botHome := t.TempDir()
	cwd := t.TempDir() // a workspace root, NOT {botHome}/{project}/work
	project := "monorepo"
	// Stale init-clone leftover.
	if err := os.MkdirAll(filepath.Join(botHome, project, "repos"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The repos the agent actually reads, under its cwd.
	wsRepos := filepath.Join(cwd, "repos")
	if err := os.MkdirAll(wsRepos, 0o755); err != nil {
		t.Fatal(err)
	}
	got := referenceReposHintFrom(botHome, project, cwd, "")
	// Must list repos from the agent's actual cwd, not the bare botHome sibling.
	if !strings.Contains(got, "ls repos/") || !strings.Contains(got, wsRepos) {
		t.Errorf("workspace-cwd hint should list {cwd}/repos via `ls repos/`, got: %q", got)
	}
	if strings.Contains(got, "ls ../repos/") {
		t.Errorf("workspace-cwd hint must NOT emit the botHome `ls ../repos/` form (ka-0000005591), got: %q", got)
	}
}

// TestReferenceReposHintFrom_NeitherExists: when no candidate dir exists yet
// (nudge resolved before clones finished), the hint must name ALL candidates so
// the agent probes rather than trusting a single guess.
func TestReferenceReposHintFrom_NeitherExists(t *testing.T) {
	botHome := t.TempDir()
	cwd := t.TempDir()
	project := "someproj"
	got := referenceReposHintFrom(botHome, project, cwd, "")

	if !strings.Contains(got, filepath.Join(botHome, project, "repos")) {
		t.Errorf("fallback hint should name the init-clone candidate, got: %q", got)
	}
	if !strings.Contains(got, filepath.Join(cwd, "repos")) {
		t.Errorf("fallback hint should name the workspace candidate, got: %q", got)
	}
	if !strings.Contains(got, "depending on pod layout") {
		t.Errorf("fallback hint should signal layout uncertainty, got: %q", got)
	}
	if !strings.Contains(got, "primary repo is your current working directory") {
		t.Errorf("fallback hint should retain the primary-repo sentence, got: %q", got)
	}
}

// TestReferenceReposHintFrom_KDWorkspaceCandidate: KD_WORKSPACE/repos is probed
// when it differs from cwd, and wins when it is the one that exists.
func TestReferenceReposHintFrom_KDWorkspaceCandidate(t *testing.T) {
	botHome := t.TempDir()
	cwd := t.TempDir()
	ws := t.TempDir()
	kdRepos := filepath.Join(ws, "repos")
	if err := os.MkdirAll(kdRepos, 0o755); err != nil {
		t.Fatal(err)
	}
	got := referenceReposHintFrom(botHome, "someproj", cwd, ws)
	if !strings.Contains(got, kdRepos) {
		t.Errorf("hint should name the existing KD_WORKSPACE repos path %q, got: %q", kdRepos, got)
	}
}

// TestReferenceReposHintFrom_DeduplicatesKDWorkspaceEqualCwd: KD_WORKSPACE==cwd
// collapses to one candidate; cwd/repos appears exactly once.
func TestReferenceReposHintFrom_DeduplicatesKDWorkspaceEqualCwd(t *testing.T) {
	botHome := t.TempDir()
	cwd := t.TempDir()
	got := referenceReposHintFrom(botHome, "", cwd, cwd)
	target := filepath.Join(cwd, "repos")
	if first, last := strings.Index(got, target), strings.LastIndex(got, target); first == -1 {
		t.Fatalf("expected cwd/repos in hint, got: %q", got)
	} else if first != last {
		t.Errorf("cwd/repos should appear once (deduped with KD_WORKSPACE), got: %q", got)
	}
}

// TestReferenceReposHintFrom_Degenerate: no project and no cwd -> generic hint
// with no crash and the primary-repo sentence retained.
func TestReferenceReposHintFrom_Degenerate(t *testing.T) {
	got := referenceReposHintFrom("", "", "", "")
	if !strings.Contains(got, "repos") {
		t.Errorf("degenerate hint should still mention repos, got: %q", got)
	}
	if !strings.Contains(got, "primary repo is your current working directory") {
		t.Errorf("degenerate hint should retain the primary-repo sentence, got: %q", got)
	}
}

// TestBuildNudgeVars_MonorepoHintLayoutAware: end-to-end, buildNudgeVars must
// emit a hint pointing at the repos dir that actually exists under cwd when
// BOAT_REFERENCE_REPOS is set and the pod is in workspace layout.
func TestBuildNudgeVars_MonorepoHintLayoutAware(t *testing.T) {
	origCwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(origCwd) }()

	if err := os.MkdirAll(filepath.Join(tmp, "repos"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("KD_PROJECT", "")
	t.Setenv("BOAT_PROJECT", "")
	t.Setenv("KD_WORKSPACE", "")
	t.Setenv("BOAT_TASK_ID", "")
	t.Setenv("BOAT_PROMPT", "")
	t.Setenv("BOAT_REFERENCE_REPOS", "some=https://example.com/repo:main")

	vars := buildNudgeVars()
	if !strings.Contains(vars.MonorepoHint, "ls repos/") {
		t.Errorf("MonorepoHint should point at cwd/repos for workspace layout, got: %q", vars.MonorepoHint)
	}
}

// TestBuildNudgeVars_NoMonorepoHintWithoutRefRepos: no reference-repos env ->
// no monorepo hint at all.
func TestBuildNudgeVars_NoMonorepoHintWithoutRefRepos(t *testing.T) {
	t.Setenv("BOAT_TASK_ID", "")
	t.Setenv("BOAT_PROMPT", "")
	t.Setenv("BOAT_REFERENCE_REPOS", "")
	vars := buildNudgeVars()
	if vars.MonorepoHint != "" {
		t.Errorf("MonorepoHint should be empty without BOAT_REFERENCE_REPOS, got: %q", vars.MonorepoHint)
	}
}
