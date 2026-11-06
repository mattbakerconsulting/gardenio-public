package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

// ── pure helpers ───────────────────────────────────────────────────────────

func TestFirstNonEmpty_Variants(t *testing.T) {
	if got := firstNonEmpty("", "", "a", "b"); got != "a" {
		t.Errorf("first-non-empty: %q", got)
	}
	if got := firstNonEmpty(""); got != "" {
		t.Errorf("all empty: %q", got)
	}
	if got := firstNonEmpty("first"); got != "first" {
		t.Errorf("single: %q", got)
	}
}

func TestUniqueStrings_Variants(t *testing.T) {
	got := uniqueStrings([]string{"a", "", "b", "a", "c", "b"})
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("want %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("idx %d: want %q got %q", i, want[i], got[i])
		}
	}
	if g := uniqueStrings(nil); len(g) != 0 {
		t.Errorf("nil: %v", g)
	}
}

func TestTriggerSourceFromLabels_Extra(t *testing.T) {
	if got := triggerSourceFromLabels([]string{"trigger:gitlab", "project:gardenio"}); got != "gitlab" {
		t.Errorf("gitlab: %q", got)
	}
	if got := triggerSourceFromLabels([]string{"project:gardenio"}); got != "?" {
		t.Errorf("no match: %q", got)
	}
	if got := triggerSourceFromLabels([]string{"trigger:disabled", "trigger:jira"}); got != "jira" {
		t.Errorf("skip disabled: %q", got)
	}
}

func TestGenerateTriggerTitle_Variants(t *testing.T) {
	cases := []struct {
		name   string
		source string
		fields map[string]any
		want   string
	}{
		{"gitlab-pipeline", "trigger:gitlab", map[string]any{"event_type": "pipeline", "project_path": "a/b", "pipeline_status": "failed"}, "GitLab pipeline trigger: a/b (failed)"},
		{"gitlab-mr", "trigger:gitlab", map[string]any{"event_type": "merge_request", "project_path": "x/y"}, "GitLab MR trigger: x/y"},
		{"gitlab-other", "trigger:gitlab", map[string]any{"project_path": "p/q"}, "GitLab trigger: p/q"},
		{"jira-with-project", "trigger:jira", map[string]any{"jira_project": "DEVOPS"}, "JIRA trigger: DEVOPS"},
		{"jira-no-project", "trigger:jira", map[string]any{}, "JIRA trigger"},
		{"webhook", "trigger:webhook", map[string]any{"webhook_token": "abc123"}, "Webhook trigger: abc123"},
		{"unknown", "trigger:something", map[string]any{}, "Trigger"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := generateTriggerTitle(tc.source, tc.fields); got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

// ── printTriggerCreated ────────────────────────────────────────────────────

func TestPrintTriggerCreated_Plain(t *testing.T) {
	old := jsonOutput
	jsonOutput = false
	t.Cleanup(func() { jsonOutput = old })

	cmd := &cobra.Command{}
	var out strings.Builder
	cmd.SetOut(&out)
	printTriggerCreated(cmd, "gd-T1", "gitlab", map[string]string{"project": "a/b", "empty": ""})
	s := out.String()
	if !strings.Contains(s, "Created gitlab trigger gd-T1") {
		t.Errorf("missing header: %q", s)
	}
	if !strings.Contains(s, "project: a/b") {
		t.Errorf("missing field: %q", s)
	}
	if strings.Contains(s, "empty:") {
		t.Errorf("empty field leaked: %q", s)
	}
	if !strings.Contains(s, "Disable with: gb trigger disable gd-T1") {
		t.Errorf("missing disable hint: %q", s)
	}
}

func TestPrintTriggerCreated_JSON(t *testing.T) {
	old := jsonOutput
	jsonOutput = true
	t.Cleanup(func() { jsonOutput = old })

	cmd := &cobra.Command{}
	printTriggerCreated(cmd, "gd-T2", "jira", map[string]string{"project": "p"})
}

// ── setTriggerEnabled (httptest) ───────────────────────────────────────────

func TestSetTriggerEnabled_OK(t *testing.T) {
	var patched bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/beads/"):
			// Return an existing bead with empty fields.
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "gd-T1", "title": "t", "fields": json.RawMessage(`{}`),
			})
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/v1/beads/"):
			patched = true
			w.WriteHeader(http.StatusOK)
			_, _ = io.Copy(io.Discard, r.Body)
		default:
			http.Error(w, "unexpected", http.StatusBadRequest)
		}
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	// Suppress stdout from success path by setting jsonOutput.
	old := jsonOutput
	jsonOutput = true
	t.Cleanup(func() { jsonOutput = old })

	if err := setTriggerEnabled(context.Background(), "gd-T1", true, triggerEnableOptions{}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if !patched {
		t.Error("expected PATCH")
	}

	jsonOutput = false
	if err := setTriggerEnabled(context.Background(), "gd-T1", false, triggerEnableOptions{}); err != nil {
		t.Fatalf("disable err: %v", err)
	}
}

func TestSetTriggerEnabled_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	if err := setTriggerEnabled(context.Background(), "gd-T1", true, triggerEnableOptions{}); err == nil {
		t.Error("expected error")
	}
}

// ── createTriggerBead (httptest) ───────────────────────────────────────────

func TestCreateTriggerBead_OK(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/beads" {
			_ = json.NewDecoder(r.Body).Decode(&body)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "gd-CREATED"})
			return
		}
		http.Error(w, "unexpected", http.StatusBadRequest)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	id, err := createTriggerBead(context.Background(), "", "trigger:jira",
		map[string]any{"project": "gardenio", "jira_project": "DEVOPS"},
		[]string{"jira:DEVOPS"},
		[]string{"user:me"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if id != "gd-CREATED" {
		t.Errorf("id: %q", id)
	}
	labels, _ := body["labels"].([]any)
	foundProject := false
	for _, l := range labels {
		if l == "project:gardenio" {
			foundProject = true
		}
	}
	if !foundProject {
		t.Errorf("expected project label, got %v", labels)
	}
}

func TestCreateTriggerBead_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	_, err := createTriggerBead(context.Background(), "my-title", "trigger:webhook",
		map[string]any{"webhook_token": "abc"}, nil, nil)
	if err == nil {
		t.Error("expected error")
	}
}

// ── runTriggerList (httptest) ──────────────────────────────────────────────

func TestRunTriggerList_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	old := triggerListSource
	triggerListSource = ""
	t.Cleanup(func() { triggerListSource = old })

	cmd := &cobra.Command{}
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetContext(context.Background())
	if err := runTriggerList(cmd, nil); err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(out.String(), "No triggers found") {
		t.Errorf("unexpected output: %q", out.String())
	}
}

func TestRunTriggerList_Populated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"beads": []map[string]any{
				{
					"id": "gd-T1", "title": "gitlab trigger", "status": "open",
					"labels": []string{"trigger:gitlab"},
					"fields": map[string]any{"enabled": "true"},
				},
				{
					"id": "gd-T2", "title": "disabled", "status": "closed",
					"labels": []string{"trigger:jira"},
					"fields": map[string]any{"enabled": "false"},
				},
			},
			"total": 2,
		})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	old := triggerListSource
	triggerListSource = "gitlab"
	t.Cleanup(func() { triggerListSource = old })

	cmd := &cobra.Command{}
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetContext(context.Background())
	if err := runTriggerList(cmd, nil); err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(out.String(), "gd-T1") || !strings.Contains(out.String(), "gd-T2") {
		t.Errorf("missing bead IDs in output: %q", out.String())
	}
}

func TestRunTriggerList_JSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	oldJ := jsonOutput
	jsonOutput = true
	t.Cleanup(func() { jsonOutput = oldJ })

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runTriggerList(cmd, nil); err != nil {
		t.Fatalf("err: %v", err)
	}
}

// ── runTriggerCreate* handlers ────────────────────────────────────────────

func stubCreateBeadServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/beads" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "gd-NEW"})
			return
		}
		http.Error(w, "unexpected", http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRunTriggerCreateGitLabPipeline_MissingProject(t *testing.T) {
	old := gitlabPipelineProject
	gitlabPipelineProject = ""
	t.Cleanup(func() { gitlabPipelineProject = old })

	if err := runTriggerCreateGitLabPipeline(&cobra.Command{}, nil); err == nil {
		t.Error("expected error for missing project")
	}
}

func TestRunTriggerCreateGitLabPipeline_MissingStatus(t *testing.T) {
	oldP, oldS := gitlabPipelineProject, gitlabPipelineStatus
	gitlabPipelineProject = "g/h"
	gitlabPipelineStatus = ""
	t.Cleanup(func() { gitlabPipelineProject, gitlabPipelineStatus = oldP, oldS })

	if err := runTriggerCreateGitLabPipeline(&cobra.Command{}, nil); err == nil {
		t.Error("expected error for missing status")
	}
}

func TestRunTriggerCreateGitLabPipeline_OK(t *testing.T) {
	srv := stubCreateBeadServer(t)
	swapDaemon(t, srv.URL)

	oldP, oldS, oldR := gitlabPipelineProject, gitlabPipelineStatus, gitlabPipelineRef
	gitlabPipelineProject = "a/b"
	gitlabPipelineStatus = "failed"
	gitlabPipelineRef = "main"
	t.Cleanup(func() {
		gitlabPipelineProject, gitlabPipelineStatus, gitlabPipelineRef = oldP, oldS, oldR
	})

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runTriggerCreateGitLabPipeline(cmd, nil); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestRunTriggerCreateGitLabMR_MissingProject(t *testing.T) {
	old := gitlabMRProject
	gitlabMRProject = ""
	t.Cleanup(func() { gitlabMRProject = old })

	if err := runTriggerCreateGitLabMR(&cobra.Command{}, nil); err == nil {
		t.Error("expected error")
	}
}

func TestRunTriggerCreateGitLabMR_OK(t *testing.T) {
	srv := stubCreateBeadServer(t)
	swapDaemon(t, srv.URL)

	oldP, oldS := gitlabMRProject, gitlabMRState
	gitlabMRProject = "a/b"
	gitlabMRState = "opened"
	t.Cleanup(func() { gitlabMRProject, gitlabMRState = oldP, oldS })

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runTriggerCreateGitLabMR(cmd, nil); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestRunTriggerCreateJira_MissingJQL(t *testing.T) {
	old := jiraJQL
	jiraJQL = ""
	t.Cleanup(func() { jiraJQL = old })

	if err := runTriggerCreateJira(&cobra.Command{}, nil); err == nil {
		t.Error("expected error")
	}
}

func TestRunTriggerCreateJira_OK(t *testing.T) {
	srv := stubCreateBeadServer(t)
	swapDaemon(t, srv.URL)

	oldJ, oldP, oldO := jiraJQL, jiraProject, jiraOpts
	jiraJQL = "project = DEVOPS"
	jiraProject = "DEVOPS"
	jiraOpts = commonTriggerOpts{}
	t.Cleanup(func() { jiraJQL, jiraProject, jiraOpts = oldJ, oldP, oldO })

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runTriggerCreateJira(cmd, nil); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestRunTriggerCreateWebhook_MissingToken(t *testing.T) {
	old := webhookToken
	webhookToken = ""
	t.Cleanup(func() { webhookToken = old })

	if err := runTriggerCreateWebhook(&cobra.Command{}, nil); err == nil {
		t.Error("expected error")
	}
}

func TestRunTriggerCreateWebhook_OK(t *testing.T) {
	srv := stubCreateBeadServer(t)
	swapDaemon(t, srv.URL)

	oldT, oldS, oldC := webhookToken, webhookSecret, webhookConditions
	webhookToken = "deploy-failed"
	webhookSecret = "s3cret"
	webhookConditions = []string{"event.type:==:deploy_failed"}
	t.Cleanup(func() {
		webhookToken, webhookSecret, webhookConditions = oldT, oldS, oldC
	})

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runTriggerCreateWebhook(cmd, nil); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestRunTriggerCreateWebhook_BadCondition(t *testing.T) {
	srv := stubCreateBeadServer(t)
	swapDaemon(t, srv.URL)

	oldT, oldC := webhookToken, webhookConditions
	webhookToken = "tok"
	webhookConditions = []string{"no-colon"}
	t.Cleanup(func() { webhookToken, webhookConditions = oldT, oldC })

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runTriggerCreateWebhook(cmd, nil); err == nil {
		t.Error("expected parse error")
	}
}

func TestRunTriggerList_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runTriggerList(cmd, nil); err == nil {
		t.Error("expected error")
	}
}

// ── news filters ──────────────────────────────────────────────────────────

func TestFilterOutAssignee(t *testing.T) {
	beads := []*beadsapi.BeadDetail{
		{ID: "a", Assignee: "gardenio-me"},
		{ID: "b", Assignee: "other"},
		{ID: "c", Assignee: "PROJECT/gardenio-me"},
		{ID: "d", Assignee: ""},
	}
	got := filterOutAssignee(beads, "gardenio-me")
	if len(got) != 2 {
		t.Fatalf("want 2, got %d: %+v", len(got), got)
	}
	if got[0].ID != "b" || got[1].ID != "d" {
		t.Errorf("unexpected filter: %+v", got)
	}
}

func TestFilterRecentlyClosed(t *testing.T) {
	now := time.Now()
	beads := []*beadsapi.BeadDetail{
		{ID: "fresh", UpdatedAt: now.Add(-30 * time.Minute)},
		{ID: "stale", UpdatedAt: now.Add(-48 * time.Hour)},
		{ID: "zero"}, // zero time → kept
	}
	got := filterRecentlyClosed(beads, 1*time.Hour)
	if len(got) != 2 {
		t.Fatalf("want 2, got %d: %+v", len(got), got)
	}
	ids := map[string]bool{got[0].ID: true, got[1].ID: true}
	if !ids["fresh"] || !ids["zero"] {
		t.Errorf("unexpected: %+v", got)
	}
}

// ── slack helpers (pure) ──────────────────────────────────────────────────

func TestResolveBridgeURL(t *testing.T) {
	t.Setenv("SLACK_BRIDGE_URL", "")
	if got := resolveBridgeURL(); got != "" {
		t.Errorf("empty env: %q", got)
	}
	t.Setenv("SLACK_BRIDGE_URL", "http://bridge:9000")
	if got := resolveBridgeURL(); got != "http://bridge:9000" {
		t.Errorf("set env: %q", got)
	}
}

func TestPrintChannelMarkdown_Empty(t *testing.T) {
	printChannelMarkdown(nil, 0, false, nil)
}

func TestPrintChannelMarkdown_Truncate(t *testing.T) {
	msgs := make([]slackChannelMessage, 50)
	for i := range msgs {
		msgs[i] = slackChannelMessage{Author: "alice", Text: strings.Repeat("x", 100)}
	}
	printChannelMarkdown(msgs, 200, false, nil)
}

func TestPrintChannelMarkdown_BotFilter(t *testing.T) {
	msgs := []slackChannelMessage{
		{Author: "bot-a", Text: "ignored", IsBot: true},
		{Author: "alice", Text: "kept"},
	}
	// Default maxChars branch (<=0) sets 8000; includeBots=false filters bots.
	printChannelMarkdown(msgs, 0, false, nil)
	// includeBots=true keeps bot lines.
	printChannelMarkdown(msgs, 0, true, nil)
}

func TestPrintSearchMarkdown(t *testing.T) {
	res := []slackSearchResult{
		{
			Message: slackChannelMessage{Author: "alice", Text: "hit1", ThreadTS: "123.456"},
			Before:  []slackChannelMessage{{Author: "bob", Text: "prev"}},
			After:   []slackChannelMessage{{Author: "carol", Text: "next"}},
		},
		{
			Message: slackChannelMessage{Author: "dave", Text: "hit2"},
		},
	}
	printSearchMarkdown(res, 0)
	// Truncation branch.
	long := make([]slackSearchResult, 50)
	for i := range long {
		long[i] = slackSearchResult{Message: slackChannelMessage{Author: "a", Text: strings.Repeat("q", 500)}}
	}
	printSearchMarkdown(long, 200)
}

// ── resolveChannelName (httptest) ─────────────────────────────────────────

func TestResolveChannelName_Found(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/slack/channels" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode([]slackChannelInfo{
			{ID: "C1", Name: "general"},
			{ID: "C2", Name: "alerts"},
		})
	}))
	defer srv.Close()

	got, err := resolveChannelName(srv.URL, "alerts")
	if err != nil || got != "C2" {
		t.Errorf("got %q err=%v", got, err)
	}
}

func TestResolveChannelName_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]slackChannelInfo{})
	}))
	defer srv.Close()

	if _, err := resolveChannelName(srv.URL, "nope"); err == nil {
		t.Error("expected not-found error")
	}
}

func TestResolveChannelName_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	if _, err := resolveChannelName(srv.URL, "general"); err == nil {
		t.Error("expected HTTP error")
	}
}

func TestResolveChannelName_BadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer srv.Close()

	if _, err := resolveChannelName(srv.URL, "general"); err == nil {
		t.Error("expected decode error")
	}
}

func TestResolveChannelName_ConnRefused(t *testing.T) {
	if _, err := resolveChannelName("http://127.0.0.1:1", "x"); err == nil {
		t.Error("expected connection error")
	}
}

// ── runSlackChannels (httptest) ───────────────────────────────────────────

func TestRunSlackChannels_NoBridge(t *testing.T) {
	t.Setenv("SLACK_BRIDGE_URL", "")
	cmd := &cobra.Command{}
	if err := runSlackChannels(cmd, nil); err == nil {
		t.Error("expected missing-bridge error")
	}
}

func TestRunSlackChannels_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]slackChannelInfo{
			{ID: "C1", Name: "general", Topic: "stuff"},
			{ID: "C2", Name: "private", Private: true},
		})
	}))
	defer srv.Close()
	t.Setenv("SLACK_BRIDGE_URL", srv.URL)

	cmd := &cobra.Command{}
	if err := runSlackChannels(cmd, nil); err != nil {
		t.Fatalf("err: %v", err)
	}
}

func TestRunSlackChannels_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]slackChannelInfo{})
	}))
	defer srv.Close()
	t.Setenv("SLACK_BRIDGE_URL", srv.URL)

	cmd := &cobra.Command{}
	if err := runSlackChannels(cmd, nil); err != nil {
		t.Fatalf("err: %v", err)
	}
}

func TestRunSlackChannels_JSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]slackChannelInfo{{ID: "C1", Name: "g"}})
	}))
	defer srv.Close()
	t.Setenv("SLACK_BRIDGE_URL", srv.URL)

	oldJ := jsonOutput
	jsonOutput = true
	t.Cleanup(func() { jsonOutput = oldJ })

	cmd := &cobra.Command{}
	if err := runSlackChannels(cmd, nil); err != nil {
		t.Fatalf("err: %v", err)
	}
}

func TestRunSlackChannels_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	t.Setenv("SLACK_BRIDGE_URL", srv.URL)

	cmd := &cobra.Command{}
	if err := runSlackChannels(cmd, nil); err == nil {
		t.Error("expected error")
	}
}

func TestRunSlackChannels_BadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("bad"))
	}))
	defer srv.Close()
	t.Setenv("SLACK_BRIDGE_URL", srv.URL)

	cmd := &cobra.Command{}
	if err := runSlackChannels(cmd, nil); err == nil {
		t.Error("expected decode error")
	}
}
