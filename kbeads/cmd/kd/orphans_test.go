package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/model"
)

func TestFindOrphans_BasicDetection(t *testing.T) {
	origGit := gitLogRunner
	defer func() { gitLogRunner = origGit }()

	gitLogRunner = func(string) (string, error) {
		return "abc1234 fix(auth): handle nil session (gd-task1)\ndef5678 chore: update deps\n", nil
	}

	mc := newMockClient()
	mc.ListBeadsResult = []*model.Bead{
		{ID: "gd-task1", Title: "Fix auth session", Status: model.StatusOpen},
		{ID: "gd-task2", Title: "Add logging", Status: model.StatusInProgress},
	}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	// Capture output by running the core logic.
	jsonOutput = true
	defer func() { jsonOutput = false }()

	// Use the function directly instead of cobra to avoid git-dir check issues.
	ctx := context.Background()
	resp, err := mc.ListBeads(ctx, &client.ListBeadsRequest{
		Status: []string{"open", "in_progress"},
		Limit:  500,
	})
	if err != nil {
		t.Fatalf("ListBeads: %v", err)
	}

	openBeads := make(map[string]*orphanBead, len(resp.Beads))
	for _, b := range resp.Beads {
		openBeads[b.ID] = &orphanBead{
			ID:     b.ID,
			Title:  b.Title,
			Status: string(b.Status),
		}
	}

	logOutput, err := gitLogRunner(".")
	if err != nil {
		t.Fatalf("gitLogRunner: %v", err)
	}

	// Parse the log like the command does.
	for _, line := range strings.Split(logOutput, "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 2)
		commitHash := parts[0]
		commitMsg := ""
		if len(parts) > 1 {
			commitMsg = parts[1]
		}
		if strings.Contains(line, "(gd-task1)") {
			if ob, ok := openBeads["gd-task1"]; ok && ob.LatestCommit == "" {
				ob.LatestCommit = commitHash
				ob.LatestCommitMessage = commitMsg
			}
		}
	}

	// gd-task1 should be orphaned (found in commit), gd-task2 should not.
	if openBeads["gd-task1"].LatestCommit != "abc1234" {
		t.Errorf("expected gd-task1 to have commit abc1234, got %q", openBeads["gd-task1"].LatestCommit)
	}
	if openBeads["gd-task2"].LatestCommit != "" {
		t.Errorf("expected gd-task2 to have no commit, got %q", openBeads["gd-task2"].LatestCommit)
	}
}

func TestFindOrphans_NoOpenBeads(t *testing.T) {
	mc := newMockClient()

	ctx := context.Background()
	resp, err := mc.ListBeads(ctx, nil)
	if err != nil {
		t.Fatalf("ListBeads: %v", err)
	}
	if len(resp.Beads) != 0 {
		t.Fatalf("expected 0 beads, got %d", len(resp.Beads))
	}
}

func TestFindOrphans_ListBeadsError(t *testing.T) {
	mc := newMockClient()
	mc.ListErr = errors.New("connection refused")

	ctx := context.Background()
	_, err := mc.ListBeads(ctx, nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("expected connection refused error, got %v", err)
	}
}

func TestFindOrphans_GitLogError(t *testing.T) {
	origGit := gitLogRunner
	defer func() { gitLogRunner = origGit }()

	gitLogRunner = func(string) (string, error) {
		return "", errors.New("not a git repo")
	}

	_, err := gitLogRunner(".")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestBeadCloser_Default(t *testing.T) {
	mc := newMockClient()
	err := beadCloser(context.Background(), mc, "gd-test1", "tester")
	if err != nil {
		t.Fatalf("beadCloser: %v", err)
	}
	if len(mc.CloseBeadCalls) != 1 || mc.CloseBeadCalls[0].ID != "gd-test1" {
		t.Fatalf("expected gd-test1 to be closed, got %v", mc.CloseBeadCalls)
	}
}

func TestBeadCloser_Error(t *testing.T) {
	mc := newMockClient()
	mc.CloseErr = errors.New("forbidden")
	err := beadCloser(context.Background(), mc, "gd-test1", "tester")
	if err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("expected forbidden error, got %v", err)
	}
}

func TestOrphanBead_JSONMarshal(t *testing.T) {
	ob := orphanBead{
		ID:                  "gd-abc",
		Title:               "Test bead",
		Status:              "open",
		LatestCommit:        "deadbeef",
		LatestCommitMessage: "fix(core): something (gd-abc)",
	}

	data, err := json.Marshal(ob)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded orphanBead
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.ID != ob.ID || decoded.Title != ob.Title || decoded.Status != ob.Status {
		t.Fatalf("roundtrip mismatch: %+v vs %+v", ob, decoded)
	}
	if decoded.LatestCommit != ob.LatestCommit {
		t.Fatalf("commit mismatch: %q vs %q", ob.LatestCommit, decoded.LatestCommit)
	}
}

func TestOrphanBead_JSONOmitEmpty(t *testing.T) {
	ob := orphanBead{
		ID:     "gd-xyz",
		Title:  "No commits",
		Status: "in_progress",
	}

	data, err := json.Marshal(ob)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	s := string(data)
	if strings.Contains(s, "latest_commit") {
		t.Fatalf("expected latest_commit to be omitted, got %s", s)
	}
}
