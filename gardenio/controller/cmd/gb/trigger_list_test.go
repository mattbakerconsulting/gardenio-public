package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

// triggerListWireBead is the minimal bead shape the /v1/beads fake serves for
// the runTriggerList tests.
type triggerListWireBead struct {
	ID     string            `json:"id"`
	Title  string            `json:"title"`
	Type   string            `json:"type"`
	Status string            `json:"status"`
	Labels []string          `json:"labels,omitempty"`
	Fields map[string]string `json:"fields,omitempty"`
}

// setupTriggerListDaemon points the global daemon at a fake /v1/beads server
// that returns `beads` with the reported `total`. It captures the query of the
// last request so a caller can assert the limit/offset it forwarded. When total
// exceeds len(beads) the response mimics a page-capped daemon (truncation).
func setupTriggerListDaemon(t *testing.T, beads []triggerListWireBead, total int) *url.Values {
	t.Helper()
	captured := &url.Values{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/beads", func(w http.ResponseWriter, r *http.Request) {
		*captured = r.URL.Query()
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": beads, "total": total})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	old := daemon
	daemon = c
	t.Cleanup(func() { daemon = old })
	return captured
}

// runTriggerListCapture runs runTriggerList with the given flag values, capturing
// the command's stdout (cmd.Print* target) and the process stderr (the --json
// truncation note target). Flags are reset to their declared defaults afterward.
func runTriggerListCapture(t *testing.T, jsonMode bool, limit, offset int) (stdout, stderr string, err error) {
	t.Helper()

	oldJSON, oldLimit, oldOffset := jsonOutput, triggerListLimit, triggerListOffset
	jsonOutput, triggerListLimit, triggerListOffset = jsonMode, limit, offset
	t.Cleanup(func() {
		jsonOutput, triggerListLimit, triggerListOffset = oldJSON, oldLimit, oldOffset
	})

	// Capture cmd.Print* via the command's OutOrStdout writer (the human table
	// path). A bare cobra command has a nil Context(), so seed one for
	// ListBeadsFiltered.
	var outBuf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&outBuf)
	cmd.SetContext(context.Background())

	// printJSON writes the bare array to the real os.Stdout, and the --json
	// truncation note goes to os.Stderr — capture both via pipes.
	oldStdout, oldStderr := os.Stdout, os.Stderr
	rOut, wOut, pOut := os.Pipe()
	if pOut != nil {
		t.Fatalf("os.Pipe: %v", pOut)
	}
	rErr, wErr, pErr := os.Pipe()
	if pErr != nil {
		t.Fatalf("os.Pipe: %v", pErr)
	}
	os.Stdout, os.Stderr = wOut, wErr
	t.Cleanup(func() { os.Stdout, os.Stderr = oldStdout, oldStderr })

	err = runTriggerList(cmd, nil)

	_ = wOut.Close()
	_ = wErr.Close()
	outBytes, _ := io.ReadAll(rOut)
	errBytes, _ := io.ReadAll(rErr)
	os.Stdout, os.Stderr = oldStdout, oldStderr

	// The human path writes to the cmd buffer; the JSON path writes to
	// os.Stdout. Only one is populated per run, so concatenation is safe.
	return outBuf.String() + string(outBytes), string(errBytes), err
}

// TestRunTriggerList_DefaultLimit pins that with no --limit the fetch forwards
// the sensible default (500), not the old silent 200 cap.
func TestRunTriggerList_DefaultLimit(t *testing.T) {
	captured := setupTriggerListDaemon(t,
		[]triggerListWireBead{{ID: "gd-t1", Title: "t1", Type: "trigger", Status: "open", Labels: []string{"trigger:gitlab"}}},
		1,
	)

	if _, _, err := runTriggerListCapture(t, false, defaultTriggerListLimit, 0); err != nil {
		t.Fatalf("runTriggerList: %v", err)
	}
	if got := captured.Get("limit"); got != "500" {
		t.Errorf("forwarded limit = %q, want 500 (default, replaces the old silent 200 cap)", got)
	}
}

// TestRunTriggerList_LimitOffsetForwarded pins that --limit/--offset thread into
// the query params so callers can page.
func TestRunTriggerList_LimitOffsetForwarded(t *testing.T) {
	captured := setupTriggerListDaemon(t,
		[]triggerListWireBead{{ID: "gd-t1", Title: "t1", Type: "trigger", Status: "open"}},
		50,
	)

	if _, _, err := runTriggerListCapture(t, false, 10, 20); err != nil {
		t.Fatalf("runTriggerList: %v", err)
	}
	if got := captured.Get("limit"); got != "10" {
		t.Errorf("forwarded limit = %q, want 10", got)
	}
	if got := captured.Get("offset"); got != "20" {
		t.Errorf("forwarded offset = %q, want 20", got)
	}
}

// TestRunTriggerList_TruncatedFooter pins the human total footer: when the page
// is short of the true total, the footer surfaces the accurate "N of M" count
// so the reader can distinguish truncation from reality.
func TestRunTriggerList_TruncatedFooter(t *testing.T) {
	beads := []triggerListWireBead{
		{ID: "gd-t1", Title: "t1", Type: "trigger", Status: "open", Labels: []string{"trigger:gitlab"}},
		{ID: "gd-t2", Title: "t2", Type: "trigger", Status: "open", Labels: []string{"trigger:jira"}},
	}
	// Truncating daemon: true total 316 > 2 returned.
	setupTriggerListDaemon(t, beads, 316)
	stdout, _, err := runTriggerListCapture(t, false, 2, 0)
	if err != nil {
		t.Fatalf("runTriggerList: %v", err)
	}
	if !strings.Contains(stdout, "2 of 316 triggers") {
		t.Errorf("expected truncation footer '2 of 316 triggers', got:\n%s", stdout)
	}
}

// TestRunTriggerList_NoFooterWhenComplete pins that a full-fleet page prints NO
// footer — the footer is only interesting when truncated.
func TestRunTriggerList_NoFooterWhenComplete(t *testing.T) {
	beads := []triggerListWireBead{
		{ID: "gd-t1", Title: "t1", Type: "trigger", Status: "open"},
		{ID: "gd-t2", Title: "t2", Type: "trigger", Status: "open"},
	}
	setupTriggerListDaemon(t, beads, 2)
	stdout, _, err := runTriggerListCapture(t, false, 500, 0)
	if err != nil {
		t.Fatalf("runTriggerList: %v", err)
	}
	if strings.Contains(stdout, "of 2 triggers") {
		t.Errorf("expected no footer when the page is complete, got:\n%s", stdout)
	}
}

// TestRunTriggerList_JSONBareArrayWithStderrNote pins that --json keeps stdout a
// bare JSON array (no envelope) AND emits the truncation note to stderr so a
// `--json | jq length` that under-counts is never a silent lie.
func TestRunTriggerList_JSONBareArrayWithStderrNote(t *testing.T) {
	beads := []triggerListWireBead{
		{ID: "gd-t1", Title: "t1", Type: "trigger", Status: "open"},
	}
	setupTriggerListDaemon(t, beads, 316)

	stdout, stderr, err := runTriggerListCapture(t, true, 1, 0)
	if err != nil {
		t.Fatalf("runTriggerList: %v", err)
	}
	// stdout must be a bare JSON array (ka-0000007254 — no envelope).
	var arr []map[string]any
	if e := json.Unmarshal([]byte(stdout), &arr); e != nil {
		t.Fatalf("stdout is not a bare JSON array: %v\nstdout=%s", e, stdout)
	}
	if len(arr) != 1 {
		t.Errorf("expected 1 element in bare array, got %d", len(arr))
	}
	// stderr carries the truncation note with the true total and paging hint.
	if !strings.Contains(stderr, "1 of 316 triggers") {
		t.Errorf("expected stderr truncation note '1 of 316 triggers', got stderr=%q", stderr)
	}
}

// TestRunTriggerList_JSONNoStderrNoteWhenComplete pins that a complete --json
// page emits nothing on stderr (no false truncation note).
func TestRunTriggerList_JSONNoStderrNoteWhenComplete(t *testing.T) {
	beads := []triggerListWireBead{{ID: "gd-t1", Title: "t1", Type: "trigger", Status: "open"}}
	setupTriggerListDaemon(t, beads, 1)

	_, stderr, err := runTriggerListCapture(t, true, 500, 0)
	if err != nil {
		t.Fatalf("runTriggerList: %v", err)
	}
	if strings.TrimSpace(stderr) != "" {
		t.Errorf("expected no stderr note on a complete page, got stderr=%q", stderr)
	}
}

// TestRunTriggerList_NegativeFlagsRejected pins that a negative --limit/--offset
// fails loudly rather than being forwarded.
func TestRunTriggerList_NegativeFlagsRejected(t *testing.T) {
	setupTriggerListDaemon(t, nil, 0)

	if _, _, err := runTriggerListCapture(t, false, -1, 0); err == nil {
		t.Error("expected error for negative --limit, got nil")
	}
	if _, _, err := runTriggerListCapture(t, false, 0, -5); err == nil {
		t.Error("expected error for negative --offset, got nil")
	}
}
