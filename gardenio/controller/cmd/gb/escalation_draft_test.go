package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// stubDraftLookup fakes gh for the draft check: `gh pr view` answers with a
// PR whose draft state is draft, or fails with err when err is set.
func stubDraftLookup(t *testing.T, draft bool, err error) *int {
	t.Helper()
	calls := 0
	old := prGHRun
	prGHRun = func(_ context.Context, args ...string) (string, error) {
		calls++
		if len(args) < 2 || args[0] != "pr" || args[1] != "view" {
			t.Errorf("draft check ran unexpected gh %v", args)
		}
		if err != nil {
			return "", err
		}
		b, _ := json.Marshal(prView{Number: 77, URL: "https://github.com/mattbakerconsulting/gardenio/pull/77", IsDraft: draft})
		return string(b), nil
	}
	t.Cleanup(func() { prGHRun = old })
	return &calls
}

const draftTestPRURL = "https://github.com/mattbakerconsulting/gardenio/pull/77"

func TestCheckMergeEscalationDraft_DraftRefused(t *testing.T) {
	stubDraftLookup(t, true, nil)
	var warn bytes.Buffer
	err := checkMergeEscalationDraft(context.Background(), draftTestPRURL, &warn)
	if err == nil {
		t.Fatal("draft PR: got nil, want a refusal")
	}
	want := "PR #77 is a draft; run 'gb pr ready' (it files this escalation for you)"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}

// Positive control: a ready PR goes through, silently.
func TestCheckMergeEscalationDraft_ReadyAllowed(t *testing.T) {
	calls := stubDraftLookup(t, false, nil)
	var warn bytes.Buffer
	if err := checkMergeEscalationDraft(context.Background(), draftTestPRURL, &warn); err != nil {
		t.Fatalf("ready PR refused: %v", err)
	}
	if *calls != 1 {
		t.Errorf("gh pr view ran %d times, want 1 (the check must actually look)", *calls)
	}
	if warn.Len() != 0 {
		t.Errorf("ready PR printed %q, want nothing", warn.String())
	}
}

// Fails open: when gh cannot answer, the escalation is allowed with a warning.
func TestCheckMergeEscalationDraft_LookupErrorAllowedWithWarning(t *testing.T) {
	stubDraftLookup(t, false, errors.New("gh: not logged in"))
	var warn bytes.Buffer
	if err := checkMergeEscalationDraft(context.Background(), draftTestPRURL, &warn); err != nil {
		t.Fatalf("lookup error refused the escalation: %v", err)
	}
	if !strings.Contains(warn.String(), "WARNING") || !strings.Contains(warn.String(), draftTestPRURL) {
		t.Errorf("warning = %q, want a WARNING naming %s", warn.String(), draftTestPRURL)
	}
}

// A GitLab MR (or no URL at all) is not a GitHub PR: gh is never asked.
func TestCheckMergeEscalationDraft_NonGitHubNotChecked(t *testing.T) {
	calls := stubDraftLookup(t, true, nil)
	for _, u := range []string{"", "https://gitlab.com/group/proj/-/merge_requests/12"} {
		if err := checkMergeEscalationDraft(context.Background(), u, &bytes.Buffer{}); err != nil {
			t.Errorf("%q: %v", u, err)
		}
	}
	if *calls != 0 {
		t.Errorf("gh pr view ran %d times for non-GitHub URLs, want 0", *calls)
	}
}

// End to end through `gb escalation create`: a draft PR files nothing, a
// ready PR files its escalation.
func TestEscalationCreate_DraftPRRefusedReadyPRFiled(t *testing.T) {
	t.Setenv("KD_AGENT_ID", "")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	t.Setenv("BOAT_PROJECT", "")
	fake := &fakeEscalationDaemon{}
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer srv.Close()

	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	defer func() { daemon = oldDaemon }()

	cmd := escalationCreateCmd
	cmd.SetContext(context.Background())
	defer resetEscalationCreateFlags(cmd)
	set := func() {
		cmd.Flags().Set("title", "Merge PR #77: draft check")
		cmd.Flags().Set("operation-type", "gitlab_mr_merge")
		cmd.Flags().Set("operation-spec", `{"mr_url":"`+draftTestPRURL+`"}`)
	}

	stubDraftLookup(t, true, nil)
	set()
	err := cmd.RunE(cmd, nil)
	if err == nil || !strings.Contains(err.Error(), "is a draft; run 'gb pr ready'") {
		t.Fatalf("draft PR: err = %v, want the draft refusal", err)
	}
	if len(fake.created) != 0 {
		t.Fatalf("draft PR: daemon got %d creates, want 0", len(fake.created))
	}

	stubDraftLookup(t, false, nil)
	set()
	out := strings.TrimSpace(string(captureStdout(t, func() error { return cmd.RunE(cmd, nil) })))
	if out != "escalation_id=gd-esc-1" || len(fake.created) != 1 {
		t.Errorf("ready PR: stdout %q, %d creates; want escalation_id=gd-esc-1 and 1", out, len(fake.created))
	}
}
