package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func TestPRKey(t *testing.T) {
	cases := map[string]string{
		"https://github.com/mattbakerconsulting/gardenio/pull/560":             "github.com/mattbakerconsulting/gardenio#560",
		"Merge https://github.com/MattBakerConsulting/gardenio/pull/560 once":  "github.com/mattbakerconsulting/gardenio#560",
		"https://github.com/mattbakerconsulting/gardenio/pull/560/files":       "github.com/mattbakerconsulting/gardenio#560",
		"https://gitlab.com/acme/platform/monorepo/-/merge_requests/2646":      "gitlab.com/acme/platform/monorepo#2646",
		"https://gitlab.com/acme/platform/monorepo/-/merge_requests/2646#note": "gitlab.com/acme/platform/monorepo#2646",
		"Merge PR #560: in-garden bead view":                                   "",
		"":                                                                     "",
	}
	for in, want := range cases {
		if got := prKey(in); got != want {
			t.Errorf("prKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// The six duplicate pairs of 2026-10-19 each spelled the same PR differently
// on the two filings. Every pair must reduce to one key.
func TestMergeEscalationPRURL_DuplicatePairsShareAKey(t *testing.T) {
	const ctx560 = "Merge https://github.com/mattbakerconsulting/gardenio/pull/560 once CI is green."
	pairs := []struct {
		name                 string
		specA, specB         map[string]any
		titleA, titleB, ctxB string
		ctxA                 string
	}{
		{
			name:   "empty spec then mr_url (gd-o4qHbD4Q8P / gd-zTyBlvV4At)",
			specA:  map[string]any{},
			specB:  map[string]any{"mr_url": "https://github.com/mattbakerconsulting/gardenio/pull/560"},
			titleA: "Merge PR #560: in-garden bead view", titleB: "Merge PR #560: in-garden bead view",
			ctxA: ctx560, ctxB: ctx560,
		},
		{
			name:  "pr key with merge_after then mr_url (gd-2NcHetEbul / gd-WR4ziTcixq)",
			specA: map[string]any{"pr": "https://github.com/mattbakerconsulting/gardenio/pull/554", "merge_after": "https://github.com/mattbakerconsulting/gardenio/pull/552"},
			specB: map[string]any{"mr_url": "https://github.com/mattbakerconsulting/gardenio/pull/554", "merge_after": "https://github.com/mattbakerconsulting/gardenio/pull/552"},
		},
	}
	for _, p := range pairs {
		a := prKey(mergeEscalationPRURL(p.specA, "", p.titleA, p.ctxA))
		b := prKey(mergeEscalationPRURL(p.specB, "", p.titleB, p.ctxB))
		if a == "" || a != b {
			t.Errorf("%s: keys %q vs %q, want one non-empty key", p.name, a, b)
		}
	}
	// merge_after names a different PR and must never become the key.
	spec := map[string]any{"merge_after": "https://github.com/mattbakerconsulting/gardenio/pull/552"}
	if got := mergeEscalationPRURL(spec, "", "Merge PR #554", "https://github.com/mattbakerconsulting/gardenio/pull/554"); !strings.HasSuffix(got, "/pull/554") {
		t.Errorf("merge_after leaked into the key: got %q, want PR 554", got)
	}
}

func TestFillMergeSpecURL(t *testing.T) {
	const u = "https://github.com/mattbakerconsulting/gardenio/pull/560"
	spec := map[string]any{}
	if !fillMergeSpecURL(spec, u) || spec["mr_url"] != u {
		t.Fatalf("empty spec not filled: %v", spec)
	}
	named := map[string]any{"pr": "https://github.com/mattbakerconsulting/gardenio/pull/1"}
	if fillMergeSpecURL(named, u) {
		t.Errorf("spec that already names its PR was overwritten: %v", named)
	}
	gitlabIID := map[string]any{"project": "acme/platform/monorepo", "mr_iid": 2646}
	if fillMergeSpecURL(gitlabIID, u) {
		t.Errorf("project+iid spec was overwritten: %v", gitlabIID)
	}
	if fillMergeSpecURL(map[string]any{}, "") {
		t.Error("filled with an empty URL")
	}
}

// The spec names a GitHub PR, which the GitLab-only guard cannot check. It
// must not print the "names no MR … add mr_url" warning: that warning is what
// sent crews back to file every merge escalation a second time.
func TestCheckMergeEscalation_GitHubPRIsNotWarnedAsMissing(t *testing.T) {
	spec := map[string]any{"mr_url": "https://github.com/mattbakerconsulting/gardenio/pull/560"}
	d := checkMergeEscalationCapability(context.Background(), spec, "")
	if d.Refuse || d.Warn {
		t.Errorf("GitHub PR spec got %+v, want a silent pass", d)
	}
}

// fakeEscalationDaemon is a stateful stand-in for the beads daemon: POST
// /v1/beads stores the bead, and the list endpoint returns what was stored.
type fakeEscalationDaemon struct {
	mu      sync.Mutex
	created []map[string]any
}

func (f *fakeEscalationDaemon) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.URL.Path == "/v1/beads" && r.Method == "POST":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		id := fmt.Sprintf("gd-esc-%d", len(f.created)+1)
		body["id"] = id
		body["status"] = "open"
		f.created = append(f.created, body)
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": id})
	case r.URL.Path == "/v1/beads" && r.Method == "GET":
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": f.created, "total": len(f.created)})
	case strings.HasSuffix(r.URL.Path, "/dependencies"):
		if r.Method == "POST" {
			w.WriteHeader(201)
		}
		_ = json.NewEncoder(w).Encode([]any{})
	case strings.HasPrefix(r.URL.Path, "/v1/beads/") && r.Method == "GET":
		id := strings.TrimPrefix(r.URL.Path, "/v1/beads/")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "type": "task", "status": "in_progress"})
	default:
		_ = json.NewEncoder(w).Encode(map[string]any{})
	}
}

// Two creates for the same PR, filed the way crews filed them on 2026-10-19
// (first with an empty spec and the URL only in --context, then again with
// operation_spec.mr_url), leave exactly one open escalation, and the second
// create reports the first's id.
func TestEscalationCreate_MergeIsIdempotentPerPR(t *testing.T) {
	t.Setenv("KD_AGENT_ID", "")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	fake := &fakeEscalationDaemon{}
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer srv.Close()

	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	defer func() { daemon = oldDaemon }()
	stubDraftLookup(t, false, nil)

	const prURL = "https://github.com/mattbakerconsulting/gardenio/pull/560"
	cmd := escalationCreateCmd
	cmd.SetContext(context.Background())
	defer func() {
		for _, f := range []string{"title", "operation-type", "operation-spec", "context", "requesting-task"} {
			cmd.Flags().Set(f, "")
		}
		cmd.Flags().Set("close-on-execute", "false")
	}()

	run := func(spec string) string {
		t.Helper()
		cmd.Flags().Set("title", "Merge PR #560: in-garden bead view (gd-BdhucNqv0i)")
		cmd.Flags().Set("operation-type", "gitlab_mr_merge")
		cmd.Flags().Set("operation-spec", spec)
		cmd.Flags().Set("context", "Merge "+prURL+" once CI is green.")
		cmd.Flags().Set("requesting-task", "gd-BdhucNqv0i")
		cmd.Flags().Set("close-on-execute", "true")
		out := captureStdout(t, func() error { return cmd.RunE(cmd, nil) })
		return strings.TrimSpace(string(out))
	}

	first := run("")
	second := run(`{"mr_url":"` + prURL + `"}`)

	if len(fake.created) != 1 {
		t.Fatalf("daemon got %d creates, want 1", len(fake.created))
	}
	if first != "escalation_id=gd-esc-1" || second != first {
		t.Errorf("stdout: first %q, second %q; want both escalation_id=gd-esc-1", first, second)
	}
	// The first filing already carries mr_url, so there was nothing to refile for.
	fields, _ := fake.created[0]["fields"].(map[string]any)
	spec, _ := fields["operation_spec"].(map[string]any)
	if spec["mr_url"] != prURL {
		t.Errorf("first escalation operation_spec = %v, want mr_url %s", fields["operation_spec"], prURL)
	}

	// A different PR is a different escalation.
	cmd.Flags().Set("title", "Merge PR #561")
	cmd.Flags().Set("operation-spec", `{"mr_url":"https://github.com/mattbakerconsulting/gardenio/pull/561"}`)
	cmd.Flags().Set("context", "")
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("create #561: %v", err)
	}
	if len(fake.created) != 2 {
		t.Errorf("daemon got %d creates after a second PR, want 2", len(fake.created))
	}
}

// The refusal tells a crew with a new head what to do instead of refiling
// (session sweep gd-KBdgGYAZOP): comment the head on the open escalation.
func TestReportExistingMergeEscalation_SaysWhatANewHeadNeeds(t *testing.T) {
	var stdout, stderr bytes.Buffer
	b := &beadsapi.BeadDetail{ID: "gd-esc-1", Status: "open"}
	reportExistingMergeEscalation(&stdout, &stderr, b, "github.com/o/r#1285")
	if got := stdout.String(); got != "escalation_id=gd-esc-1\n" {
		t.Errorf("stdout = %q, want escalation_id=gd-esc-1", got)
	}
	for _, want := range []string{
		"already open for github.com/o/r#1285",
		"a new push needs no new escalation",
		`kd comment add gd-esc-1 "Head is now <sha>`,
		"gb stop",
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr missing %q:\n%s", want, stderr.String())
		}
	}
}
