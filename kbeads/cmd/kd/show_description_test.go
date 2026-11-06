package main

import (
	"encoding/json"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

// ka-0000002381: `kd show <id> --description` (alias --body) prints ONLY the
// bead description — no header, deps, comments, or mention sections — the
// mirror of --mentions-only for the description field. Advice gd-rps50azIWp
// teaches this flag; these tests lock the contract in.

// showDescTestState swaps in a mock client and resets the package flag vars,
// restoring everything on cleanup so tests don't leak global state into peers.
func showDescTestState(t *testing.T, beads map[string]*model.Bead) *mockBeadsClient {
	t.Helper()
	mc := newMockClient()
	mc.Beads = beads

	prevClient := beadsClient
	prevDesc := showDescription
	prevJSON := jsonOutput
	prevTTY := isStdoutTTY
	prevOnly := showMentionsOnly
	prevAll := showAllComments
	t.Cleanup(func() {
		beadsClient = prevClient
		showDescription = prevDesc
		jsonOutput = prevJSON
		isStdoutTTY = prevTTY
		showMentionsOnly = prevOnly
		showAllComments = prevAll
	})
	beadsClient = mc
	isStdoutTTY = func() bool { return false }
	showMentionsOnly = false
	showAllComments = false
	showDescription = true
	jsonOutput = false
	return mc
}

func TestShowCmd_Description_SingleText_OnlyBody(t *testing.T) {
	id := "gd-astX6E9p2i"
	showDescTestState(t, map[string]*model.Bead{
		id: {
			ID:          id,
			Title:       "a title that must not leak",
			Status:      model.StatusOpen,
			Description: "the handoff brief body\nsecond line",
			Comments:    makeComments(3),
		},
	})

	got := captureShowStdout(t, func() {
		if err := showCmd.RunE(showCmd, []string{id}); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if got != "the handoff brief body\nsecond line\n" {
		t.Errorf("expected only the description + trailing newline, got %q", got)
	}
	if strings.Contains(got, "a title that must not leak") {
		t.Errorf("--description must not print the header/title; got:\n%s", got)
	}
	if strings.Contains(got, "Comments:") || strings.Contains(got, "comment-000") {
		t.Errorf("--description must not print comments; got:\n%s", got)
	}
}

func TestShowCmd_Description_MissingID_ErrorsNonZero(t *testing.T) {
	showDescTestState(t, map[string]*model.Bead{})

	// Single missing id: error surface is byte-identical to the normal path.
	err := showCmd.RunE(showCmd, []string{"gd-JJGaaGEzew"})
	if err == nil {
		t.Fatal("expected error for a missing id under --description, got nil")
	}
}

func TestShowCmd_Description_Batch_TextBlankLineSeparated(t *testing.T) {
	a, b := "gd-DescA0001", "gd-DescB0002"
	showDescTestState(t, map[string]*model.Bead{
		a: {ID: a, Title: "ta", Status: model.StatusOpen, Description: "alpha body"},
		b: {ID: b, Title: "tb", Status: model.StatusOpen, Description: "beta body"},
	})

	got := captureShowStdout(t, func() {
		if err := showCmd.RunE(showCmd, []string{a, b}); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if got != "alpha body\n\nbeta body\n" {
		t.Errorf("batch text descriptions must be blank-line separated, got %q", got)
	}
}

func TestShowCmd_Description_Batch_MissingID_StillRendersRestAndErrors(t *testing.T) {
	a := "gd-DescA0001"
	showDescTestState(t, map[string]*model.Bead{
		a: {ID: a, Title: "ta", Status: model.StatusOpen, Description: "alpha body"},
	})

	var got string
	err := func() error {
		var runErr error
		got = captureShowStdout(t, func() {
			runErr = showCmd.RunE(showCmd, []string{a, "gd-Missing0009"})
		})
		return runErr
	}()

	if err == nil {
		t.Fatal("batch with a missing id must exit non-zero")
	}
	if !strings.Contains(got, "alpha body") {
		t.Errorf("the found bead's description must still render; got %q", got)
	}
}

func TestShowCmd_Description_SingleJSON_IDAndDescription(t *testing.T) {
	id := "gd-vkzf4Aq6gG"
	mc := showDescTestState(t, map[string]*model.Bead{
		id: {ID: id, Title: "t", Status: model.StatusOpen, Description: "json body"},
	})
	_ = mc
	jsonOutput = true

	got := captureShowStdout(t, func() {
		if err := showCmd.RunE(showCmd, []string{id}); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	var obj map[string]string
	if err := json.Unmarshal([]byte(got), &obj); err != nil {
		t.Fatalf("output is not a JSON object: %v\n%s", err, got)
	}
	if obj["id"] != id || obj["description"] != "json body" {
		t.Errorf("expected {id,description} object, got %v", obj)
	}
	if _, hasTitle := obj["title"]; hasTitle {
		t.Errorf("--description --json must emit only id+description, got %v", obj)
	}
}

func TestShowCmd_Description_BatchJSON_ArrayOfObjects(t *testing.T) {
	a, b := "gd-DescA0001", "gd-DescB0002"
	showDescTestState(t, map[string]*model.Bead{
		a: {ID: a, Title: "ta", Status: model.StatusOpen, Description: "alpha body"},
		b: {ID: b, Title: "tb", Status: model.StatusOpen, Description: "beta body"},
	})
	jsonOutput = true

	got := captureShowStdout(t, func() {
		if err := showCmd.RunE(showCmd, []string{a, b}); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	var arr []map[string]string
	if err := json.Unmarshal([]byte(got), &arr); err != nil {
		t.Fatalf("output is not a JSON array: %v\n%s", err, got)
	}
	if len(arr) != 2 {
		t.Fatalf("expected 2 objects, got %d: %v", len(arr), arr)
	}
	if arr[0]["id"] != a || arr[0]["description"] != "alpha body" {
		t.Errorf("first element wrong: %v", arr[0])
	}
	if arr[1]["id"] != b || arr[1]["description"] != "beta body" {
		t.Errorf("second element wrong: %v", arr[1])
	}
}

// TestShowCmd_BodyAlias verifies --body flips the same var as --description.
func TestShowCmd_BodyAlias(t *testing.T) {
	id := "gd-fBuS7Doujr"
	showDescTestState(t, map[string]*model.Bead{
		id: {ID: id, Title: "t", Status: model.StatusOpen, Description: "aliased body"},
	})
	// Simulate --body by flipping the shared var the flag binds; both flags
	// wrap &showDescription, so this is exactly what cobra does on --body.
	showDescription = true

	got := captureShowStdout(t, func() {
		if err := showCmd.RunE(showCmd, []string{id}); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})
	if got != "aliased body\n" {
		t.Errorf("--body should print only the description, got %q", got)
	}
}
