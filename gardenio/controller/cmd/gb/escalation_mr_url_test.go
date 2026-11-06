package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// A test run on a PR branch must never resolve that branch's real PR through gh.
func init() {
	escalationBranchPRURL = func(context.Context) string { return "" }
}

func stubBranchPR(t *testing.T, url string) {
	t.Helper()
	old := escalationBranchPRURL
	escalationBranchPRURL = func(context.Context) string { return url }
	t.Cleanup(func() { escalationBranchPRURL = old })
}

const (
	pr964 = "https://github.com/mattbakerconsulting/gardenio/pull/964"
	pr966 = "https://github.com/mattbakerconsulting/gardenio/pull/966"
)

func TestResolveMergePRURL_Precedence(t *testing.T) {
	none := func() string { return "" }
	is := func(u string) func() string { return func() string { return u } }
	cases := []struct {
		name               string
		spec               map[string]any
		branch, task       func() string
		title, context     string
		wantURL, wantSouce string
	}{
		{"spec wins", map[string]any{"mr_url": pr964}, is(pr966), is(pr966), pr966, pr966, pr964, mergePRFromSpec},
		{"branch beats title and task", nil, is(pr964), is(pr966), "Merge " + pr966, "", pr964, mergePRFromBranch},
		{"one title URL beats the task", nil, none, is(pr966), "Merge " + pr964, "", pr964, mergePRFromTitle},
		{"two title URLs fall through", nil, none, is(pr966), pr964 + " and " + pr966, "", pr966, mergePRFromTask},
		{"same PR twice in the title is one", nil, none, is(pr966), pr964 + " (" + pr964 + ")", "", pr964, mergePRFromTitle},
		{"task before context", nil, none, is(pr966), "Merge PR #964", pr964, pr966, mergePRFromTask},
		{"context last", nil, none, none, "Merge it", "see " + pr964, pr964, mergePRFromContext},
		{"nothing", nil, none, none, "Merge it", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u, src := resolveMergePRURL(tc.spec, tc.branch, tc.task, tc.title, tc.context)
			if u != tc.wantURL || src != tc.wantSouce {
				t.Errorf("got (%q, %q), want (%q, %q)", u, src, tc.wantURL, tc.wantSouce)
			}
		})
	}
	// A spec that names its PR costs no gh or daemon call.
	boom := func() string { t.Fatal("lookup ran although the spec names the PR"); return "" }
	resolveMergePRURL(map[string]any{"mr_url": pr964}, boom, boom, "", "")
}

func TestPRNumbersIn(t *testing.T) {
	cases := map[string][]int{
		"Merge PR #964: kbeads labels N+1":       {964},
		"Merge " + pr966 + " (stacked on #964)":  {966, 964},
		"MR !12 and PR 13":                       {12, 13},
		"merge-request 4, gd-AbC123, it&#39;s":   nil,
		"https://github.com/o/r/issues#3 is not": nil,
	}
	for in, want := range cases {
		got := prNumbersIn(in)
		if len(got) != len(want) {
			t.Errorf("prNumbersIn(%q) = %v, want %v", in, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("prNumbersIn(%q) = %v, want %v", in, got, want)
			}
		}
	}
}

func TestCheckMergePRNamed(t *testing.T) {
	ok := []struct{ prURL, source, title, context string }{
		{pr964, mergePRFromSpec, "Merge PR #964: labels", "Merge " + pr964 + " once green."},
		{pr964, mergePRFromBranch, "Merge PR #964 (stacked on #962)", "after #962 merges"},
		{pr964, mergePRFromTask, "Merge the labels fix", ""},
		{"", "", "Merge PR #964", ""},
	}
	for _, c := range ok {
		if err := checkMergePRNamed(c.prURL, c.source, c.title, c.context); err != nil {
			t.Errorf("checkMergePRNamed(%q, %q, %q): %v, want nil", c.prURL, c.title, c.context, err)
		}
	}
	refused := []struct{ prURL, source, title, context, want string }{
		// gd-05MVg33gqA: the title is about #964, the task's last link was #966.
		{pr966, mergePRFromTask, "Merge PR #964: kbeads labels N+1", "", "the title names PR #964"},
		{pr966, mergePRFromSpec, "Merge PR #964", "", "--mr-url"},
		// gd-VeZIGcrSx0: three PRs named, one silently kept.
		{pr966, mergePRFromTask, "Merge PRs #964, #965 and #966", "", "one escalation per PR"},
		{pr966, mergePRFromSpec, "Merge the labels fix", "Merge " + pr964 + " once green.", "--context names PR #964 but not #966"},
	}
	for _, c := range refused {
		err := checkMergePRNamed(c.prURL, c.source, c.title, c.context)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("checkMergePRNamed(%q, %q, %q) = %v, want an error containing %q", c.prURL, c.title, c.context, err, c.want)
		}
	}
}

func TestSetSpecMRURL_ReplacesEveryPRKey(t *testing.T) {
	spec := map[string]any{"mr_url": pr966, "pr": pr966, "url": pr966, "merge_after": pr964}
	setSpecMRURL(spec, pr964)
	if spec["mr_url"] != pr964 || spec["pr"] != nil || spec["url"] != nil || spec["merge_after"] != pr964 {
		t.Errorf("spec = %v, want mr_url #964 alone and merge_after kept", spec)
	}
	gl := map[string]any{"project": "acme/app", "iid": float64(7)}
	setSpecMRURL(gl, pr964)
	if len(gl) != 1 || gl["mr_url"] != pr964 {
		t.Errorf("GitLab spec = %v, want only mr_url", gl)
	}
}

// taskMRDaemon is fakeEscalationDaemon whose requesting task carries an mr_url.
func taskMRDaemon(t *testing.T, taskMRURL string) *fakeEscalationDaemon {
	t.Helper()
	t.Setenv("KD_AGENT_ID", "")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	t.Setenv("BOAT_PROJECT", "")
	fake := &fakeEscalationDaemon{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/v1/beads/gd-cP9tODid8j" {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "gd-cP9tODid8j", "type": "task", "status": "in_progress",
				"fields": map[string]any{"mr_url": taskMRURL}})
			return
		}
		fake.handler(w, r)
	}))
	t.Cleanup(srv.Close)
	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	t.Cleanup(func() { daemon = oldDaemon })
	stubDraftLookup(t, false, nil)
	return fake
}

func createdMRURL(t *testing.T, fake *fakeEscalationDaemon) any {
	t.Helper()
	if len(fake.created) != 1 {
		t.Fatalf("daemon got %d creates, want 1", len(fake.created))
	}
	fields, _ := fake.created[0]["fields"].(map[string]any)
	spec, _ := fields["operation_spec"].(map[string]any)
	return spec["mr_url"]
}

// Regression for gd-05MVg33gqA: the B6b crew filed "Merge PR #964" with
// --requesting-task whose last-linked PR was #966, and the escalation went out
// naming #966. Now that create is refused, and --mr-url files the right PR.
func TestEscalationCreate_TaskMRURLNeverOverridesTheTitlesPR(t *testing.T) {
	fake := taskMRDaemon(t, pr966)
	cmd := escalationCreateCmd
	cmd.SetContext(context.Background())
	defer resetEscalationCreateFlags(cmd)
	set := func() {
		cmd.Flags().Set("title", "Merge PR #964: kbeads labels N+1")
		cmd.Flags().Set("operation-type", "gitlab_mr_merge")
		cmd.Flags().Set("requesting-task", "gd-cP9tODid8j")
	}

	set()
	err := cmd.RunE(cmd, nil)
	if err == nil || !strings.Contains(err.Error(), "the title names PR #964") || !strings.Contains(err.Error(), "/pull/966") {
		t.Fatalf("err = %v, want the #964-vs-#966 refusal", err)
	}
	if len(fake.created) != 0 {
		t.Fatalf("daemon got %d creates, want 0", len(fake.created))
	}

	set()
	cmd.Flags().Set("mr-url", pr964)
	if out := strings.TrimSpace(string(captureStdout(t, func() error { return cmd.RunE(cmd, nil) }))); out != "escalation_id=gd-esc-1" {
		t.Fatalf("stdout %q, want escalation_id=gd-esc-1", out)
	}
	if got := createdMRURL(t, fake); got != pr964 {
		t.Errorf("operation_spec.mr_url = %v, want %s", got, pr964)
	}
}

// The current branch's PR beats the task's last-linked one.
func TestEscalationCreate_BranchPRBeatsTaskMRURL(t *testing.T) {
	fake := taskMRDaemon(t, pr966)
	stubBranchPR(t, pr964)
	cmd := escalationCreateCmd
	cmd.SetContext(context.Background())
	defer resetEscalationCreateFlags(cmd)
	cmd.Flags().Set("title", "Merge the kbeads labels fix")
	cmd.Flags().Set("operation-type", "gitlab_mr_merge")
	cmd.Flags().Set("requesting-task", "gd-cP9tODid8j")
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	if got := createdMRURL(t, fake); got != pr964 {
		t.Errorf("operation_spec.mr_url = %v, want the branch PR %s", got, pr964)
	}
}

func TestEscalationCreate_MRURLFlagChecks(t *testing.T) {
	taskMRDaemon(t, "")
	cmd := escalationCreateCmd
	cmd.SetContext(context.Background())
	defer resetEscalationCreateFlags(cmd)
	cases := []struct{ opType, spec, mrURL, want string }{
		{"aws_iam", "", pr964, "applies only to a merge escalation"},
		{"gitlab_mr_merge", "", "not a url", "is not a GitHub PR or GitLab MR URL"},
		{"gitlab_mr_merge", `{"mr_url":"` + pr966 + `"}`, pr964, "name different PRs"},
	}
	for _, c := range cases {
		resetEscalationCreateFlags(cmd)
		cmd.Flags().Set("title", "Merge it")
		cmd.Flags().Set("operation-type", c.opType)
		cmd.Flags().Set("operation-spec", c.spec)
		cmd.Flags().Set("mr-url", c.mrURL)
		if err := cmd.RunE(cmd, nil); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s --mr-url %q: err = %v, want %q", c.opType, c.mrURL, err, c.want)
		}
	}
}

// amendDaemon serves one escalation bead and records the PATCH and comment.
type amendDaemon struct {
	mu       sync.Mutex
	bead     map[string]any
	patches  []map[string]any
	comments []string
	refuse   bool
	// open is what GET /v1/beads lists: the other open escalations.
	open []map[string]any
}

func (d *amendDaemon) handler(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case r.Method == "GET" && r.URL.Path == "/v1/beads/gd-esc-9":
		_ = json.NewEncoder(w).Encode(d.bead)
	case r.Method == "PATCH" && r.URL.Path == "/v1/beads/gd-esc-9":
		if d.refuse {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "approval and routing fields (...) are immutable on an escalation after create"})
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		d.patches = append(d.patches, body)
		_ = json.NewEncoder(w).Encode(d.bead)
	case r.Method == "GET" && r.URL.Path == "/v1/beads":
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": d.open, "total": len(d.open)})
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/comments"):
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		text, _ := body["text"].(string)
		d.comments = append(d.comments, text)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
	default:
		_ = json.NewEncoder(w).Encode(map[string]any{})
	}
}

func runAmend(t *testing.T, d *amendDaemon, flags map[string]string) (string, error) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(d.handler))
	defer srv.Close()
	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	defer func() { daemon = oldDaemon }()
	cmd := escalationAmendCmd
	defer func() {
		cmd.Flags().Set("mr-url", "")
		cmd.Flags().Set("operation-spec", "")
	}()
	for k, v := range flags {
		cmd.Flags().Set(k, v)
	}
	return runPRCmd(cmd, "gd-esc-9")
}

func amendBead(status string, fields map[string]any) map[string]any {
	f := map[string]any{"operation_type": "gitlab_mr_merge", "operation_spec": map[string]any{"mr_url": pr966, "merge_after": "x"}}
	for k, v := range fields {
		f[k] = v
	}
	return map[string]any{"id": "gd-esc-9", "type": "escalation", "status": status, "title": "Merge PR #964", "fields": f}
}

func TestEscalationAmend_FixesTheMRURL(t *testing.T) {
	d := &amendDaemon{bead: amendBead("open", nil)}
	out, err := runAmend(t, d, map[string]string{"mr-url": pr964})
	if err != nil {
		t.Fatalf("amend: %v", err)
	}
	if !strings.Contains(out, "escalation_id=gd-esc-9") {
		t.Errorf("stdout %q, want escalation_id=gd-esc-9", out)
	}
	if len(d.patches) != 1 {
		t.Fatalf("got %d PATCHes, want 1", len(d.patches))
	}
	fields, _ := d.patches[0]["fields"].(map[string]any)
	spec, _ := fields["operation_spec"].(map[string]any)
	if spec["mr_url"] != pr964 || spec["merge_after"] != "x" || len(fields) != 1 {
		t.Errorf("PATCH fields = %v, want only operation_spec with mr_url #964 and merge_after kept", fields)
	}
	if len(d.comments) != 1 || !strings.Contains(d.comments[0], "/pull/966") || !strings.Contains(d.comments[0], "/pull/964") {
		t.Errorf("comments = %v, want one recording old and new spec", d.comments)
	}
}

func TestEscalationAmend_Refusals(t *testing.T) {
	cases := []struct {
		name  string
		bead  map[string]any
		flags map[string]string
		want  string
	}{
		{"no flag", amendBead("open", nil), map[string]string{}, "exactly one of"},
		{"both flags", amendBead("open", nil), map[string]string{"mr-url": pr964, "operation-spec": "{}"}, "exactly one of"},
		{"not a merge", amendBead("open", map[string]any{"operation_type": "aws_iam"}), map[string]string{"mr-url": pr964}, "only a merge escalation"},
		{"closed", amendBead("closed", nil), map[string]string{"mr-url": pr964}, "only while it is open"},
		{"claimed", amendBead("open", map[string]any{"claimed_by": "merge-desk"}), map[string]string{"mr-url": pr964}, "already claimed, handed up or approved"},
		{"handed up", amendBead("open", map[string]any{"handed_up_by": "merge-desk"}), map[string]string{"mr-url": pr964}, "already claimed, handed up or approved"},
		{"approved", amendBead("open", map[string]any{"approved_at": "2026-10-30T00:00:00Z"}), map[string]string{"mr-url": pr964}, "already claimed, handed up or approved"},
		{"bad spec", amendBead("open", nil), map[string]string{"operation-spec": "[1]"}, "JSON object"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := &amendDaemon{bead: c.bead}
			_, err := runAmend(t, d, c.flags)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
			if len(d.patches) != 0 {
				t.Errorf("got %d PATCHes, want 0", len(d.patches))
			}
		})
	}
	// Repointing at a PR another open merge escalation names would leave two
	// open escalations for one PR, which create refuses too.
	other := amendBead("open", map[string]any{"operation_spec": map[string]any{"mr_url": pr964}})
	other["id"] = "gd-esc-other"
	d := &amendDaemon{bead: amendBead("open", nil), open: []map[string]any{other}}
	if _, err := runAmend(t, d, map[string]string{"mr-url": pr964}); err == nil || !strings.Contains(err.Error(), "gd-esc-other is already open") {
		t.Fatalf("duplicate: err = %v, want the already-open refusal", err)
	}
	if len(d.patches) != 0 {
		t.Errorf("duplicate: got %d PATCHes, want 0", len(d.patches))
	}

	// The daemon has the last word (filer only): its refusal is explained.
	d = &amendDaemon{bead: amendBead("open", nil), refuse: true}
	if _, err := runAmend(t, d, map[string]string{"mr-url": pr964}); err == nil || !strings.Contains(err.Error(), "only the filer") {
		t.Fatalf("daemon refusal: err = %v, want the filer-only explanation", err)
	}
}
