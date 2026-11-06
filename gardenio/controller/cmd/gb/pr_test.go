package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

// fakePRDaemon is a stateful beads daemon for the gb pr tests: it stores the
// escalations `gb escalation create` files (and lists them back, so the
// per-PR merge dedupe sees them), the fields patched onto the task bead, and
// the comments added to it.
type fakePRDaemon struct {
	mu          sync.Mutex
	escalations []map[string]any
	taskFields  map[string]any
	comments    []string
}

func (f *fakePRDaemon) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.URL.Path == "/v1/beads" && r.Method == http.MethodPost:
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		id := fmt.Sprintf("gd-esc-%d", len(f.escalations)+1)
		body["id"] = id
		body["status"] = "open"
		f.escalations = append(f.escalations, body)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": id})
	case r.URL.Path == "/v1/beads" && r.Method == http.MethodGet:
		// Honour the status filter, so an escalation the desk closed is not
		// found by the open-merge dedupe.
		beads := f.escalations
		if st := r.URL.Query().Get("status"); st != "" {
			beads = nil
			for _, e := range f.escalations {
				if slices.Contains(strings.Split(st, ","), e["status"].(string)) {
					beads = append(beads, e)
				}
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": beads, "total": len(beads)})
	case strings.HasSuffix(r.URL.Path, "/dependencies"):
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"dependencies": []any{}})
	case strings.HasSuffix(r.URL.Path, "/comments") && r.Method == http.MethodPost:
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.comments = append(f.comments, body["text"])
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{})
	case strings.HasPrefix(r.URL.Path, "/v1/beads/") && r.Method == http.MethodPatch:
		var body struct {
			Fields map[string]any `json:"fields"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		for k, v := range body.Fields {
			f.taskFields[k] = v
		}
		_ = json.NewEncoder(w).Encode(map[string]any{})
	case strings.HasPrefix(r.URL.Path, "/v1/beads/") && r.Method == http.MethodGet:
		id := strings.TrimPrefix(r.URL.Path, "/v1/beads/")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "type": "task", "status": "in_progress", "fields": f.taskFields})
	default:
		_ = json.NewEncoder(w).Encode(map[string]any{})
	}
}

// fakeGH stands in for gh: one PR on branch "feature" whose head is
// "abc123", changing files. `pr ready` and `pr ready --undo` flip its draft
// state; `pr merge --auto` arms auto-merge (armErr fails it) and `pr merge
// --disable-auto` disarms it. With keepAutoOnUndo the undo leaves auto-merge
// armed, as GitHub might.
type fakeGH struct {
	draft          bool
	files          []string
	autoMerge      bool
	keepAutoOnUndo bool
	armErr         error
	calls          [][]string
}

const fakePRURL = "https://github.com/mattbakerconsulting/gardenio/pull/42"

func (g *fakeGH) run(_ context.Context, args ...string) (string, error) {
	g.calls = append(g.calls, args)
	switch {
	case len(args) >= 2 && args[0] == "pr" && args[1] == "create":
		return "Creating draft pull request\n" + fakePRURL + "\n", nil
	case len(args) >= 2 && args[0] == "pr" && args[1] == "view" && slices.Contains(args, "files,changedFiles"):
		var files []map[string]string
		for _, f := range g.files {
			files = append(files, map[string]string{"path": f})
		}
		b, _ := json.Marshal(map[string]any{"files": files, "changedFiles": len(files)})
		return string(b), nil
	case len(args) >= 2 && args[0] == "pr" && args[1] == "view" && slices.Contains(args, "url,autoMergeRequest"):
		var amr any
		if g.autoMerge {
			amr = map[string]string{"mergeMethod": "MERGE"}
		}
		b, _ := json.Marshal(map[string]any{"url": fakePRURL, "autoMergeRequest": amr})
		return string(b), nil
	case len(args) >= 2 && args[0] == "pr" && args[1] == "view":
		b, _ := json.Marshal(prView{Number: 42, URL: fakePRURL, Title: "add gb pr", IsDraft: g.draft, HeadRefName: "feature", HeadRefOid: "abc123"})
		return string(b), nil
	case len(args) >= 2 && args[0] == "pr" && args[1] == "ready":
		g.draft = slices.Contains(args, "--undo")
		if g.draft && !g.keepAutoOnUndo {
			g.autoMerge = false
		}
		return "", nil
	case len(args) >= 2 && args[0] == "pr" && args[1] == "merge" && slices.Contains(args, "--disable-auto"):
		g.autoMerge = false
		return "", nil
	case len(args) >= 2 && args[0] == "pr" && args[1] == "merge" && slices.Contains(args, "--auto"):
		if g.armErr != nil {
			return "", g.armErr
		}
		g.autoMerge = true
		return "", nil
	}
	return "", fmt.Errorf("fakeGH: unexpected gh %v", args)
}

func (g *fakeGH) called(sub string) int {
	n := 0
	for _, c := range g.calls {
		if len(c) >= 2 && c[1] == sub && !slices.Contains(c, "--undo") {
			n++
		}
	}
	return n
}

// setupPRTest points daemon at a fake, fakes gh and git (current branch
// branch, local HEAD head), and runs `gb escalation create` in-process
// against the fake daemon, so the real merge dedupe path runs.
func setupPRTest(t *testing.T, branch, head string) (*fakePRDaemon, *fakeGH) {
	t.Helper()
	t.Setenv("KD_AGENT_ID", "")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	t.Setenv("BOAT_PROJECT", "")
	t.Setenv("BEADS_OPERATED_BY", "")

	fd := &fakePRDaemon{taskFields: map[string]any{}}
	srv := httptest.NewServer(http.HandlerFunc(fd.handler))
	t.Cleanup(srv.Close)
	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})

	gh := &fakeGH{draft: true, files: []string{"gardenio/controller/cmd/gb/pr.go"}}
	oldGH, oldGit, oldGB := prGHRun, prGitRun, prGBRun
	prGHRun = gh.run
	prGitRun = func(_ context.Context, args ...string) (string, error) {
		switch strings.Join(args, " ") {
		case "rev-parse --abbrev-ref HEAD":
			return branch + "\n", nil
		case "rev-parse HEAD":
			return head + "\n", nil
		}
		return "", fmt.Errorf("fake git: unexpected %v", args)
	}
	prGBRun = func(ctx context.Context, args ...string) (string, error) {
		if len(args) < 2 || args[0] != "escalation" || args[1] != "create" {
			return "", fmt.Errorf("fake gb: unexpected %v", args)
		}
		c := escalationCreateCmd
		resetEscalationCreateFlags(c)
		if err := c.ParseFlags(args[2:]); err != nil {
			return "", err
		}
		c.SetContext(ctx)
		return string(captureStdout(t, func() error { return c.RunE(c, nil) })), nil
	}
	t.Cleanup(func() {
		daemon = oldDaemon
		prGHRun, prGitRun, prGBRun = oldGH, oldGit, oldGB
		resetEscalationCreateFlags(escalationCreateCmd)
		for _, c := range []*cobra.Command{prOpenCmd, prReadyCmd} {
			c.Flags().Set("task", "")
		}
		prOpenCmd.Flags().Set("title", "")
		prOpenCmd.Flags().Set("body-file", "")
		prOpenCmd.Flags().Set("base", "main")
	})
	return fd, gh
}

func resetEscalationCreateFlags(c *cobra.Command) {
	for _, f := range []string{"title", "operation-type", "operation-spec", "context", "requesting-task", "mr-url"} {
		c.Flags().Set(f, "")
	}
	c.Flags().Set("close-on-execute", "false")
}

// runPRCmd runs one gb pr subcommand's RunE and returns its stdout.
func runPRCmd(c *cobra.Command, args ...string) (string, error) {
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetContext(context.Background())
	defer c.SetOut(nil)
	err := c.RunE(c, args)
	return out.String(), err
}

func (g *fakeGH) createArgs(t *testing.T) []string {
	t.Helper()
	for _, c := range g.calls {
		if len(c) >= 2 && c[0] == "pr" && c[1] == "create" {
			return c
		}
	}
	t.Fatalf("gh pr create never ran; calls: %v", g.calls)
	return nil
}

func flagValue(args []string, name string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func TestPRCommandListsOpenReadyDraft(t *testing.T) {
	var names []string
	for _, c := range prCmd.Commands() {
		names = append(names, c.Name())
	}
	for _, want := range []string{"open", "ready", "draft"} {
		if !slices.Contains(names, want) {
			t.Errorf("gb pr subcommands = %v, missing %q", names, want)
		}
	}
	if !slices.Contains(rootCmd.Commands(), prCmd) {
		t.Error("gb pr is not registered on the root command")
	}
}

func TestPROpen_AlwaysPassesDraft(t *testing.T) {
	fd, gh := setupPRTest(t, "feature", "abc123")
	prOpenCmd.Flags().Set("title", "add gb pr")
	prOpenCmd.Flags().Set("task", "gd-task")

	out, err := runPRCmd(prOpenCmd)
	if err != nil {
		t.Fatalf("gb pr open: %v", err)
	}
	args := gh.createArgs(t)
	if !slices.Contains(args, "--draft") {
		t.Errorf("gh pr create args = %v, want --draft", args)
	}
	if flagValue(args, "--base") != "main" {
		t.Errorf("gh pr create --base = %q, want main", flagValue(args, "--base"))
	}
	if strings.TrimSpace(out) != fakePRURL {
		t.Errorf("stdout = %q, want the PR URL", out)
	}
	if fd.taskFields["mr_url"] != fakePRURL || fd.taskFields["forge"] != "github" {
		t.Errorf("task fields = %v, want mr_url %s and forge github", fd.taskFields, fakePRURL)
	}
}

func TestPROpen_AppendsMissingTrailers(t *testing.T) {
	_, gh := setupPRTest(t, "feature", "abc123")
	t.Setenv("KD_AGENT_ID", "gd-agent")
	t.Setenv("BEADS_OPERATED_BY", "oidc:op@example.com")
	bodyFile := filepath.Join(t.TempDir(), "body.md")
	if err := os.WriteFile(bodyFile, []byte("## Summary\n- add gb pr\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	prOpenCmd.Flags().Set("title", "add gb pr")
	prOpenCmd.Flags().Set("body-file", bodyFile)
	prOpenCmd.Flags().Set("task", "gd-task")

	if _, err := runPRCmd(prOpenCmd); err != nil {
		t.Fatalf("gb pr open: %v", err)
	}
	body := flagValue(gh.createArgs(t), "--body")
	for _, want := range []string{"## Summary\n- add gb pr\n", "\nOperator: oidc:op@example.com", "\nBead: gd-task\n", "\nAgent: gd-agent\n"} {
		if !strings.Contains(body, want) {
			t.Errorf("PR body %q lacks %q", body, want)
		}
	}
}

// Bead: names the agent bead's task_id when --task is absent, never the
// agent bead itself: that closes at gb stop, and the PR sweeper's C1 would
// then close a live PR (gd-yPJzwLZjrB).
func TestPROpen_BeadTrailerNamesAgentTask(t *testing.T) {
	fd, gh := setupPRTest(t, "feature", "abc123")
	t.Setenv("KD_AGENT_ID", "gd-agent")
	fd.taskFields["task_id"] = "gd-task"
	prOpenCmd.Flags().Set("title", "add gb pr")

	if _, err := runPRCmd(prOpenCmd); err != nil {
		t.Fatalf("gb pr open: %v", err)
	}
	body := flagValue(gh.createArgs(t), "--body")
	if want := "\n\nBead: gd-task\nAgent: gd-agent\n"; !strings.HasSuffix(body, want) {
		t.Errorf("PR body %q does not end with %q", body, want)
	}
}

func TestPROpen_BeadTrailerFallsBackToAgent(t *testing.T) {
	_, gh := setupPRTest(t, "feature", "abc123")
	t.Setenv("KD_AGENT_ID", "gd-agent")
	prOpenCmd.Flags().Set("title", "add gb pr")

	if _, err := runPRCmd(prOpenCmd); err != nil {
		t.Fatalf("gb pr open: %v", err)
	}
	body := flagValue(gh.createArgs(t), "--body")
	if !strings.HasSuffix(body, "\n\nBead: gd-agent\n") || strings.Contains(body, "Agent:") {
		t.Errorf("PR body %q: with no task, want only a Bead: gd-agent trailer", body)
	}
}

func TestWithPRTrailers_KeepsExistingTrailers(t *testing.T) {
	body := "## Summary\n- x\n\nOperator: Pat (oidc:pat)\nBead: gd-a\nAgent: gd-x\n"
	if got := withPRTrailers(body, "Operator: oidc:other", "gd-b", "gd-c"); got != body {
		t.Errorf("withPRTrailers rewrote a body that has every trailer:\n%q", got)
	}
	got := withPRTrailers("## Summary\n- x\n\nBead: gd-a\n", "Operator: oidc:pat", "gd-b", "")
	if want := "## Summary\n- x\n\nBead: gd-a\n\nOperator: oidc:pat\n"; got != want {
		t.Errorf("withPRTrailers = %q, want %q", got, want)
	}
}

func TestWithPRTrailers_BeadIsTaskAgentIsAgent(t *testing.T) {
	for _, tc := range []struct{ task, agent, want string }{
		{"gd-task", "gd-agent", "x\n\nBead: gd-task\nAgent: gd-agent\n"},
		{"", "gd-agent", "x\n\nBead: gd-agent\n"},
		{"gd-task", "", "x\n\nBead: gd-task\n"},
		{"gd-same", "gd-same", "x\n\nBead: gd-same\n"},
		{"", "", "x\n"},
	} {
		if got := withPRTrailers("x\n", "", tc.task, tc.agent); got != tc.want {
			t.Errorf("withPRTrailers(task %q, agent %q) = %q, want %q", tc.task, tc.agent, got, tc.want)
		}
	}
}

func TestPROpen_RefusesOnMain(t *testing.T) {
	_, gh := setupPRTest(t, "main", "abc123")
	prOpenCmd.Flags().Set("title", "add gb pr")
	if _, err := runPRCmd(prOpenCmd); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("gb pr open on main: err = %v, want a refusal", err)
	}
	if len(gh.calls) != 0 {
		t.Errorf("gh ran on main: %v", gh.calls)
	}
}

// A PR the desk sent back while it stayed ready gets its merge escalation
// re-filed with no draft/ready toggle: no gh pr ready and no arming, so no new
// CI run starts (gd-0kbTgvCrFF).
func TestPRReady_ReadyPRRefilesEscalationWithoutToggle(t *testing.T) {
	fd, gh := setupPRTest(t, "feature", "abc123")
	gh.draft = false
	prReadyCmd.Flags().Set("task", "gd-task")

	out, err := runPRCmd(prReadyCmd)
	if err != nil {
		t.Fatalf("gb pr ready on a ready PR: %v", err)
	}
	if gh.called("ready") != 0 || len(gh.mergeArgs()) != 0 {
		t.Errorf("ready PR: gh pr ready ran %d times, gh pr merge %d times; want 0 and 0", gh.called("ready"), len(gh.mergeArgs()))
	}
	if len(fd.escalations) != 1 {
		t.Fatalf("ready PR: %d escalations filed, want 1", len(fd.escalations))
	}
	fields, _ := fd.escalations[0]["fields"].(map[string]any)
	spec, _ := fields["operation_spec"].(map[string]any)
	if fields["operation_type"] != "gitlab_mr_merge" || spec["mr_url"] != fakePRURL || fields["requesting_task"] != "gd-task" || fields["close_requesting_task"] != true {
		t.Errorf("escalation fields = %v, want a close-on-execute gitlab_mr_merge for %s requested by gd-task", fields, fakePRURL)
	}
	if !strings.Contains(out, "escalation_id=gd-esc-1") {
		t.Errorf("stdout = %q, want escalation_id=gd-esc-1", out)
	}
	if want := []string{"re-filed merge escalation gd-esc-1 (PR already ready)"}; !slices.Equal(fd.comments, want) {
		t.Errorf("task comments = %q, want %q", fd.comments, want)
	}
}

// Re-filing on a ready PR whose escalation is still open reuses it; once the
// desk has closed it (sent the PR back), the next re-file files a new one.
func TestPRReady_ReadyPRReusesOpenEscalation(t *testing.T) {
	fd, gh := setupPRTest(t, "feature", "abc123")
	gh.draft = false
	prReadyCmd.Flags().Set("task", "gd-task")

	for i := range 2 {
		if _, err := runPRCmd(prReadyCmd); err != nil {
			t.Fatalf("gb pr ready #%d: %v", i+1, err)
		}
	}
	if len(fd.escalations) != 1 {
		t.Fatalf("two re-files with the escalation open: %d escalations, want 1", len(fd.escalations))
	}
	want := "re-filed merge escalation gd-esc-1 (PR already ready)"
	if len(fd.comments) != 2 || fd.comments[1] != want {
		t.Errorf("task comments = %q, want the second to be %q", fd.comments, want)
	}

	fd.escalations[0]["status"] = "closed"
	out, err := runPRCmd(prReadyCmd)
	if err != nil {
		t.Fatalf("gb pr ready after the desk closed the escalation: %v", err)
	}
	if len(fd.escalations) != 2 || !strings.Contains(out, "escalation_id=gd-esc-2") {
		t.Errorf("after the desk closed it: %d escalations, stdout %q; want 2 and escalation_id=gd-esc-2", len(fd.escalations), out)
	}
	if gh.called("ready") != 0 {
		t.Errorf("gh pr ready ran %d times on a ready PR, want 0", gh.called("ready"))
	}
}

func TestPRReady_UnpushedHeadErrors(t *testing.T) {
	fd, gh := setupPRTest(t, "feature", "def456")
	prReadyCmd.Flags().Set("task", "gd-task")

	_, err := runPRCmd(prReadyCmd)
	if err == nil || !strings.Contains(err.Error(), "not pushed") {
		t.Fatalf("gb pr ready with unpushed HEAD: err = %v, want \"not pushed\"", err)
	}
	if gh.called("ready") != 0 || len(fd.escalations) != 0 {
		t.Errorf("unpushed HEAD: gh pr ready ran %d times, %d escalations filed; want 0 and 0", gh.called("ready"), len(fd.escalations))
	}
}

// ready → draft → ready files one merge escalation: the second ready goes
// through `gb escalation create`, whose per-PR dedupe returns the first.
func TestPRReady_FilesOneEscalationWhenCalledTwice(t *testing.T) {
	fd, gh := setupPRTest(t, "feature", "abc123")
	prReadyCmd.Flags().Set("task", "gd-task")

	first, err := runPRCmd(prReadyCmd)
	if err != nil {
		t.Fatalf("first gb pr ready: %v", err)
	}
	// Positive control: the first ready does file one.
	if len(fd.escalations) != 1 {
		t.Fatalf("after the first ready: %d escalations, want 1", len(fd.escalations))
	}
	if !strings.Contains(first, "escalation_id=gd-esc-1") {
		t.Errorf("first ready stdout = %q, want escalation_id=gd-esc-1", first)
	}
	// gd-PwyvuF0fHp: ready names the parked CI wait for its head, not a poll.
	if want := "gb wait ci --repo mattbakerconsulting/gardenio --sha abc123"; !strings.Contains(first, want) {
		t.Errorf("first ready stdout = %q, want %q", first, want)
	}
	fields, _ := fd.escalations[0]["fields"].(map[string]any)
	spec, _ := fields["operation_spec"].(map[string]any)
	if fields["operation_type"] != "gitlab_mr_merge" || spec["mr_url"] != fakePRURL || fields["requesting_task"] != "gd-task" {
		t.Errorf("escalation fields = %v, want a gitlab_mr_merge for %s requested by gd-task", fields, fakePRURL)
	}

	if _, err := runPRCmd(prDraftCmd); err != nil {
		t.Fatalf("gb pr draft: %v", err)
	}
	if !gh.draft {
		t.Fatal("gb pr draft left the PR ready")
	}
	if _, err := runPRCmd(prReadyCmd); err != nil {
		t.Fatalf("second gb pr ready: %v", err)
	}

	if len(fd.escalations) != 1 {
		t.Errorf("after ready, draft, ready: %d escalations, want 1", len(fd.escalations))
	}
	if gh.called("ready") != 2 {
		t.Errorf("gh pr ready ran %d times, want 2", gh.called("ready"))
	}
	want := "PR #42 marked ready; auto-merge armed; merge escalation gd-esc-1"
	if len(fd.comments) != 2 || fd.comments[0] != want || fd.comments[1] != want {
		t.Errorf("task comments = %q, want %q twice", fd.comments, want)
	}
}

func TestPRRepoFromURL(t *testing.T) {
	for url, want := range map[string]string{
		fakePRURL:                                   "mattbakerconsulting/gardenio",
		"https://github.com/o/r/pull/7/files":       "o/r",
		"https://gitlab.com/o/r/-/merge_requests/7": "",
		"https://github.com/o/r":                    "",
	} {
		if got := prRepoFromURL(url); got != want {
			t.Errorf("prRepoFromURL(%q) = %q, want %q", url, got, want)
		}
	}
}

func (g *fakeGH) mergeArgs() [][]string {
	var out [][]string
	for _, c := range g.calls {
		if len(c) >= 2 && c[0] == "pr" && c[1] == "merge" {
			out = append(out, c)
		}
	}
	return out
}

// gd-HNTG3YPBRs: a PR off every (d) path is armed for auto-merge pinned to its
// head, never with --admin, and the merge escalation is still filed.
func TestPRReady_ArmsAutoMergeOffDeskPaths(t *testing.T) {
	fd, gh := setupPRTest(t, "feature", "abc123")
	prReadyCmd.Flags().Set("task", "gd-task")

	out, err := runPRCmd(prReadyCmd)
	if err != nil {
		t.Fatalf("gb pr ready: %v", err)
	}
	merges := gh.mergeArgs()
	if len(merges) != 1 {
		t.Fatalf("gh pr merge calls = %v, want one", merges)
	}
	args := merges[0]
	for _, want := range []string{fakePRURL, "--auto", "--merge", "--delete-branch"} {
		if !slices.Contains(args, want) {
			t.Errorf("gh pr merge args = %v, want %s", args, want)
		}
	}
	if flagValue(args, "--match-head-commit") != "abc123" {
		t.Errorf("gh pr merge --match-head-commit = %q, want abc123", flagValue(args, "--match-head-commit"))
	}
	if slices.Contains(args, "--admin") {
		t.Errorf("gh pr merge args = %v: never --admin", args)
	}
	if !gh.autoMerge || !strings.Contains(out, "auto-merge armed") {
		t.Errorf("auto-merge armed = %v, stdout = %q; want armed and said so", gh.autoMerge, out)
	}
	if len(fd.escalations) != 1 {
		t.Errorf("%d escalations, want 1: the escalation is the backstop", len(fd.escalations))
	}
}

func TestPRReady_DeskPathIsNotArmed(t *testing.T) {
	fd, gh := setupPRTest(t, "feature", "abc123")
	gh.files = []string{"gardenio/controller/cmd/gb/pr.go", "gardenio/helm/gardenio/values.yaml"}
	prReadyCmd.Flags().Set("task", "gd-task")

	out, err := runPRCmd(prReadyCmd)
	if err != nil {
		t.Fatalf("gb pr ready: %v", err)
	}
	if m := gh.mergeArgs(); len(m) != 0 {
		t.Errorf("gh pr merge ran on a (d)-path PR: %v", m)
	}
	want := "auto-merge not armed: touches gardenio/helm/gardenio/values.yaml"
	if !strings.Contains(out, want) {
		t.Errorf("stdout = %q, want %q", out, want)
	}
	if len(fd.escalations) != 1 {
		t.Errorf("%d escalations, want 1", len(fd.escalations))
	}
	if len(fd.comments) != 1 || !strings.Contains(fd.comments[0], want) {
		t.Errorf("task comments = %q, want one naming %q", fd.comments, want)
	}
}

// A failed arm warns and still files the escalation.
func TestPRReady_ArmFailureStillFilesEscalation(t *testing.T) {
	fd, gh := setupPRTest(t, "feature", "abc123")
	gh.armErr = fmt.Errorf("auto-merge is not allowed for this repository")
	prReadyCmd.Flags().Set("task", "gd-task")

	out, err := runPRCmd(prReadyCmd)
	if err != nil {
		t.Fatalf("gb pr ready: %v", err)
	}
	if !strings.Contains(out, "auto-merge not armed: auto-merge is not allowed") {
		t.Errorf("stdout = %q, want the arm failure", out)
	}
	if len(fd.escalations) != 1 || !strings.Contains(out, "escalation_id=gd-esc-1") {
		t.Errorf("%d escalations, stdout %q; want the backstop filed", len(fd.escalations), out)
	}
}

// An unreadable file list (gh caps it at 100) fails closed: not armed.
func TestPRReady_NoFileListIsNotArmed(t *testing.T) {
	fd, gh := setupPRTest(t, "feature", "abc123")
	gh.files = nil
	prReadyCmd.Flags().Set("task", "gd-task")

	if _, err := runPRCmd(prReadyCmd); err != nil {
		t.Fatalf("gb pr ready: %v", err)
	}
	if m := gh.mergeArgs(); len(m) != 0 {
		t.Errorf("gh pr merge ran without a file list: %v", m)
	}
	if len(fd.escalations) != 1 {
		t.Errorf("%d escalations, want 1", len(fd.escalations))
	}
}

// gb pr draft leaves no armed auto-merge, whether or not GitHub drops it on
// the undo.
func TestPRDraft_DisarmsAutoMerge(t *testing.T) {
	for _, keep := range []bool{false, true} {
		t.Run(fmt.Sprintf("githubKeepsAuto=%v", keep), func(t *testing.T) {
			_, gh := setupPRTest(t, "feature", "abc123")
			gh.keepAutoOnUndo = keep
			prReadyCmd.Flags().Set("task", "gd-task")
			if _, err := runPRCmd(prReadyCmd); err != nil {
				t.Fatalf("gb pr ready: %v", err)
			}
			if !gh.autoMerge {
				t.Fatal("positive control: gb pr ready did not arm auto-merge")
			}
			out, err := runPRCmd(prDraftCmd)
			if err != nil {
				t.Fatalf("gb pr draft: %v", err)
			}
			if gh.autoMerge {
				t.Error("gb pr draft left auto-merge armed")
			}
			if disarmed := strings.Contains(out, "auto-merge disarmed"); disarmed != keep {
				t.Errorf("stdout = %q; said disarmed = %v, want %v", out, disarmed, keep)
			}
		})
	}
}

func TestPRDeskPath(t *testing.T) {
	for path, want := range map[string]bool{
		".github/workflows/ci.yml":                  true,
		"gardenio/terraform/main.tf":                true,
		"deploy-values/prod.yaml":                   true,
		"gardenio/helm/gardenio/values.yaml":        true,
		"kbeads/internal/server/write_authz.go":     true,
		"kbeads/internal/server/spawn_authz.go":     true,
		"gardenio/controller/internal/auth/oidc.go": true,
		"kbeads/internal/server/interceptors.go":    true,
		"gardenio/controller/cmd/secrets/main.go":   true,
		"gardenio/controller/cmd/gb/pr.go":          false,
		"docs/merge-desk.md":                        false,
		".github/CODEOWNERS":                        false,
		"gardenio/helmfile.yaml":                    false,
	} {
		if got := prDeskPath(path); got != want {
			t.Errorf("prDeskPath(%q) = %v, want %v", path, got, want)
		}
	}
}
