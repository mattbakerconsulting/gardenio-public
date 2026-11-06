package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

// gd-upYRyQkrbW: `kd show` accepts multiple ids so callers stop writing N+1
// shell loops over bead sets. The contract these tests lock in:
//   - single-id output is byte-identical to the pre-batch command (both text
//     and --json: a JSON object, not an array);
//   - multiple ids render each bead in sequence (blank-line separated in text
//     mode; a JSON array with --json);
//   - a missing id in a batch does NOT abort — found beads still render, the
//     missing id is reported on stderr, and the command exits non-zero.

// wireShowMock swaps the package-level client + flags for the duration of a
// test and restores them after. Mirrors the setup the phase-3 / comment-cap
// tests do inline.
func wireShowMock(t *testing.T, mc *mockBeadsClient) {
	t.Helper()
	prevClient := beadsClient
	prevJSON := jsonOutput
	prevTTY := isStdoutTTY
	prevOnly := showMentionsOnly
	prevAll := showAllComments
	t.Cleanup(func() {
		beadsClient = prevClient
		jsonOutput = prevJSON
		isStdoutTTY = prevTTY
		showMentionsOnly = prevOnly
		showAllComments = prevAll
	})
	beadsClient = mc
	jsonOutput = false
	isStdoutTTY = func() bool { return false }
	showMentionsOnly = false
	showAllComments = false
}

// captureShowStreams runs fn with both os.Stdout and os.Stderr swapped for
// pipes and returns what each received. The batch path writes missing-id
// diagnostics to stderr, so tests need to see both streams independently.
func captureShowStreams(t *testing.T, fn func() error) (stdout, stderr string, runErr error) {
	t.Helper()
	rOut, wOut, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe stdout: %v", err)
	}
	rErr, wErr, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe stderr: %v", err)
	}
	prevOut, prevErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = wOut, wErr
	t.Cleanup(func() { os.Stdout, os.Stderr = prevOut, prevErr })

	outDone := make(chan struct{})
	errDone := make(chan struct{})
	var outBuf, errBuf bytes.Buffer
	go func() { _, _ = outBuf.ReadFrom(rOut); close(outDone) }()
	go func() { _, _ = errBuf.ReadFrom(rErr); close(errDone) }()

	runErr = fn()
	_ = wOut.Close()
	_ = wErr.Close()
	<-outDone
	<-errDone
	return outBuf.String(), errBuf.String(), runErr
}

// TestShow_SingleID_JSON_IsObject locks in that a single-id --json render is a
// JSON object (not wrapped in an array) — the byte-level contract existing
// machine consumers depend on.
func TestShow_SingleID_JSON_IsObject(t *testing.T) {
	mc := newMockClient()
	id := "gd-solo00001"
	mc.Beads[id] = &model.Bead{ID: id, Title: "solo", Type: model.TypeTask, Status: model.StatusOpen}
	wireShowMock(t, mc)
	jsonOutput = true

	out, _, err := captureShowStreams(t, func() error {
		return showCmd.RunE(showCmd, []string{id})
	})
	if err != nil {
		t.Fatalf("RunE: %v", err)
	}

	var obj map[string]any
	if uErr := json.Unmarshal([]byte(out), &obj); uErr != nil {
		t.Fatalf("single-id --json must be a JSON object; unmarshal failed: %v\noutput:\n%s", uErr, out)
	}
	if obj["id"] != id {
		t.Errorf("expected id %q in object; got %v\noutput:\n%s", id, obj["id"], out)
	}
	// A leading '[' would mean we regressed to always emitting an array.
	if strings.HasPrefix(strings.TrimSpace(out), "[") {
		t.Errorf("single-id --json must not be an array; got:\n%s", out)
	}
}

// TestShow_MultiID_JSON_IsArray verifies that multiple ids emit a JSON array
// with one object per bead, in argument order.
func TestShow_MultiID_JSON_IsArray(t *testing.T) {
	mc := newMockClient()
	a, b := "gd-arr000001", "gd-arr000002"
	mc.Beads[a] = &model.Bead{ID: a, Title: "first", Type: model.TypeTask, Status: model.StatusOpen}
	mc.Beads[b] = &model.Bead{ID: b, Title: "second", Type: model.TypeTask, Status: model.StatusOpen}
	wireShowMock(t, mc)
	jsonOutput = true

	out, _, err := captureShowStreams(t, func() error {
		return showCmd.RunE(showCmd, []string{a, b})
	})
	if err != nil {
		t.Fatalf("RunE: %v", err)
	}

	var arr []map[string]any
	if uErr := json.Unmarshal([]byte(out), &arr); uErr != nil {
		t.Fatalf("multi-id --json must be a JSON array; unmarshal failed: %v\noutput:\n%s", uErr, out)
	}
	if len(arr) != 2 {
		t.Fatalf("expected 2 beads in array; got %d\noutput:\n%s", len(arr), out)
	}
	if arr[0]["id"] != a || arr[1]["id"] != b {
		t.Errorf("array must preserve argument order [%s, %s]; got [%v, %v]", a, b, arr[0]["id"], arr[1]["id"])
	}
}

// TestShow_MultiID_JSON_ChildrenCount verifies the per-bead children_count
// wrapper survives inside the array surface (same encoding as the single-id
// wrapper).
func TestShow_MultiID_JSON_ChildrenCount(t *testing.T) {
	mc := newMockClient()
	epic, leaf := "gd-epicarr01", "gd-leafarr01"
	mc.Beads[epic] = &model.Bead{ID: epic, Title: "epic", Type: model.TypeEpic, Status: model.StatusOpen}
	mc.Beads[leaf] = &model.Bead{ID: leaf, Title: "leaf", Type: model.TypeTask, Status: model.StatusOpen}
	mc.RevDeps[epic] = []*model.Dependency{
		{BeadID: "gd-c1", DependsOnID: epic, Type: model.DepParentChild},
		{BeadID: "gd-c2", DependsOnID: epic, Type: model.DepParentChild},
	}
	wireShowMock(t, mc)
	jsonOutput = true

	out, _, err := captureShowStreams(t, func() error {
		return showCmd.RunE(showCmd, []string{epic, leaf})
	})
	if err != nil {
		t.Fatalf("RunE: %v", err)
	}

	var arr []map[string]any
	if uErr := json.Unmarshal([]byte(out), &arr); uErr != nil {
		t.Fatalf("unmarshal: %v\noutput:\n%s", uErr, out)
	}
	if len(arr) != 2 {
		t.Fatalf("expected 2 beads; got %d\n%s", len(arr), out)
	}
	// json numbers decode to float64.
	if cc, ok := arr[0]["children_count"].(float64); !ok || cc != 2 {
		t.Errorf("epic entry must carry children_count=2; got %v\n%s", arr[0]["children_count"], out)
	}
	if _, present := arr[1]["children_count"]; present {
		t.Errorf("leaf entry must omit children_count when 0; got %v\n%s", arr[1]["children_count"], out)
	}
}

// TestShow_MultiID_Text_SeparatesBlocks verifies both beads render in text
// mode, in order, separated by a blank line.
func TestShow_MultiID_Text_SeparatesBlocks(t *testing.T) {
	mc := newMockClient()
	a, b := "gd-txt000001", "gd-txt000002"
	mc.Beads[a] = &model.Bead{ID: a, Title: "alpha bead", Type: model.TypeTask, Status: model.StatusOpen}
	mc.Beads[b] = &model.Bead{ID: b, Title: "bravo bead", Type: model.TypeTask, Status: model.StatusOpen}
	wireShowMock(t, mc)

	out, _, err := captureShowStreams(t, func() error {
		return showCmd.RunE(showCmd, []string{a, b})
	})
	if err != nil {
		t.Fatalf("RunE: %v", err)
	}
	if !strings.Contains(out, "alpha bead") || !strings.Contains(out, "bravo bead") {
		t.Errorf("both beads must render; got:\n%s", out)
	}
	idxA := strings.Index(out, a)
	idxB := strings.Index(out, b)
	if !(idxA >= 0 && idxA < idxB) {
		t.Errorf("beads must render in argument order (a before b); idxA=%d idxB=%d\n%s", idxA, idxB, out)
	}
	// Two ID: lines — one header per bead.
	if n := strings.Count(out, "ID:          "); n != 2 {
		t.Errorf("expected 2 header blocks; got %d\n%s", n, out)
	}
	// A blank line separates the two blocks: the second header is preceded by
	// "\n\nID:".
	if !strings.Contains(out, "\n\nID:          "+b) {
		t.Errorf("expected a blank line before the second bead block; got:\n%q", out)
	}
}

// TestShow_MultiID_MissingID_PartialThenNonZero is the core error-semantics
// contract: a missing id in a batch does not abort — the found bead renders on
// stdout, the missing id is reported on stderr, and RunE returns an error
// (which the main() wrapper maps to a non-zero exit).
func TestShow_MultiID_MissingID_PartialThenNonZero(t *testing.T) {
	mc := newMockClient()
	found, missing := "gd-found0001", "gd-missing01"
	mc.Beads[found] = &model.Bead{ID: found, Title: "i exist", Type: model.TypeTask, Status: model.StatusOpen}
	// missing is intentionally absent from mc.Beads → GetBead returns an error.
	wireShowMock(t, mc)

	out, errOut, runErr := captureShowStreams(t, func() error {
		return showCmd.RunE(showCmd, []string{found, missing})
	})

	if runErr == nil {
		t.Fatalf("expected a non-nil error when a batch id is missing; got nil\nstdout:\n%s", out)
	}
	if !strings.Contains(runErr.Error(), missing) {
		t.Errorf("summary error should name the missing id %q; got %q", missing, runErr.Error())
	}
	// Partial output: the found bead still rendered.
	if !strings.Contains(out, "i exist") {
		t.Errorf("found bead must still render despite the missing sibling; stdout:\n%s", out)
	}
	// The missing id is reported on stderr, not stdout.
	if !strings.Contains(errOut, missing) {
		t.Errorf("missing id must be reported on stderr; stderr:\n%s", errOut)
	}
	if strings.Contains(out, missing) {
		t.Errorf("missing-id diagnostic must not pollute stdout; stdout:\n%s", out)
	}
}

// TestShow_SingleID_Missing_ErrorSurfaceUnchanged verifies the single-id
// not-found path keeps its pre-batch error surface: the error is returned
// (wrapped as "getting bead <id>: …") and nothing is written to stderr by
// the command itself.
func TestShow_SingleID_Missing_ErrorSurfaceUnchanged(t *testing.T) {
	mc := newMockClient()
	id := "gd-ghost0001"
	wireShowMock(t, mc)

	out, errOut, runErr := captureShowStreams(t, func() error {
		return showCmd.RunE(showCmd, []string{id})
	})
	if runErr == nil {
		t.Fatalf("expected an error for a missing single id; got nil")
	}
	if !strings.HasPrefix(runErr.Error(), "getting bead "+id+": ") {
		t.Errorf("single-id error surface changed; got %q", runErr.Error())
	}
	if out != "" {
		t.Errorf("no stdout expected on single-id miss; got:\n%s", out)
	}
	// The single-id path returns the error for cobra to print; it must not
	// also write its own stderr diagnostic (that's the batch-only behavior).
	if errOut != "" {
		t.Errorf("single-id path must not write its own stderr line; got:\n%s", errOut)
	}
}

// TestShow_MultiID_AllMissing_NoStdout_NonZero verifies that when every id in
// a batch is missing, stdout stays empty and RunE still errors.
func TestShow_MultiID_AllMissing_NoStdout_NonZero(t *testing.T) {
	mc := newMockClient()
	wireShowMock(t, mc)

	out, errOut, runErr := captureShowStreams(t, func() error {
		return showCmd.RunE(showCmd, []string{"gd-gba2xvvymh", "gd-6g442ewuhh"})
	})
	if runErr == nil {
		t.Fatalf("expected an error when all batch ids are missing")
	}
	if strings.Contains(runErr.Error(), "2 of 2") == false {
		t.Errorf("summary error should count 2 of 2 missing; got %q", runErr.Error())
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("no beads found → no stdout; got:\n%s", out)
	}
	for _, id := range []string{"gd-gba2xvvymh", "gd-6g442ewuhh"} {
		if !strings.Contains(errOut, id) {
			t.Errorf("both missing ids should be reported on stderr; missing %q in:\n%s", id, errOut)
		}
	}
}

// TestShow_MultiID_AllMissing_JSON_EmptyArray locks in the JSON-surface
// counterpart to the text case above: when every id in a --json batch is
// missing, stdout is exactly the valid empty array `[]` (not nothing), stderr
// carries the per-id diagnostics, and RunE still errors. Emitting `[]` is the
// intentional contract — it mirrors `kd list --json` (which prints `[]` for
// zero results) and keeps the batch surface valid JSON so
// `kd show a b --json | jq …` never chokes on empty input. The non-zero exit
// plus the stderr lines are the error signal, not an empty stdout.
func TestShow_MultiID_AllMissing_JSON_EmptyArray(t *testing.T) {
	mc := newMockClient()
	wireShowMock(t, mc)
	jsonOutput = true

	out, errOut, runErr := captureShowStreams(t, func() error {
		return showCmd.RunE(showCmd, []string{"gd-gba2xvvymh", "gd-6g442ewuhh"})
	})
	if runErr == nil {
		t.Fatalf("expected an error when all batch ids are missing")
	}
	if strings.TrimSpace(out) != "[]" {
		t.Errorf("all-missing --json batch stdout must be the empty array %q; got:\n%q", "[]", out)
	}
	// Still valid, parseable JSON — the contract that makes this better than
	// empty stdout for jq consumers.
	var arr []map[string]any
	if uErr := json.Unmarshal([]byte(out), &arr); uErr != nil {
		t.Fatalf("all-missing --json stdout must parse as a JSON array; got %v\noutput:\n%s", uErr, out)
	}
	if len(arr) != 0 {
		t.Errorf("all-missing array must be empty; got %d entries", len(arr))
	}
	for _, id := range []string{"gd-gba2xvvymh", "gd-6g442ewuhh"} {
		if !strings.Contains(errOut, id) {
			t.Errorf("both missing ids should be reported on stderr; missing %q in:\n%s", id, errOut)
		}
	}
}

// TestShow_MultiID_MentionsOnly_PerID verifies --mentions-only iterates all
// ids and never calls GetBead (the cheap-pipe contract), for the batch case.
func TestShow_MultiID_MentionsOnly_PerID(t *testing.T) {
	mc := newMockClient()
	a, b := "gd-mo000001", "gd-mo000002"
	blocks := "blocks"
	mc.Mentions[a] = []*model.BeadReferenceSummary{
		{ID: "gd-AAA", SourceField: "description", RelationshipType: &blocks},
	}
	mc.Mentions[b] = []*model.BeadReferenceSummary{
		{ID: "gd-BBB", SourceField: "description", RelationshipType: &blocks},
	}
	wireShowMock(t, mc)
	showMentionsOnly = true

	out, _, err := captureShowStreams(t, func() error {
		return showCmd.RunE(showCmd, []string{a, b})
	})
	if err != nil {
		t.Fatalf("RunE: %v", err)
	}
	for _, want := range []string{"gd-AAA", "gd-BBB"} {
		if !strings.Contains(out, want) {
			t.Errorf("--mentions-only batch missing %q; got:\n%s", want, out)
		}
	}
	if len(mc.GetBeadCalls) != 0 {
		t.Errorf("--mentions-only must not call GetBead; got %d calls", len(mc.GetBeadCalls))
	}
}
