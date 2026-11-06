package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// TestBuildReadyQueryRequestsParkedOnDecision pins that `gb ready` always opts
// into the daemon's parked_on_decision projection so every row carries the
// "an open operator decision names this bead in its prose" signal
// (ka-0000003797). Without it the 8 escalated rows in bug ka-0000007469
// rendered as ordinary claimable work.
func TestBuildReadyQueryRequestsParkedOnDecision(t *testing.T) {
	q := buildReadyQuery(readyOptions{Project: "gardenio", Limit: 20})
	if !q.WithParkedOnDecision {
		t.Error("WithParkedOnDecision = false, want true — gb ready must surface the parked-on-decision signal per row")
	}
}

// TestReadyParkedBadge pins the compact inline suffix: it names the decision id
// when known, and degrades to a generic marker when the id is absent (old
// daemon / projection not populated) — never silently empty when parked.
func TestReadyParkedBadge(t *testing.T) {
	if got := readyParkedBadge("gd-decX"); got != "[parked: gd-decX]" {
		t.Errorf("badge = %q, want [parked: gd-decX]", got)
	}
	if got := readyParkedBadge(""); !strings.Contains(got, "parked") {
		t.Errorf("badge with empty id = %q, want a generic parked marker", got)
	}
}

// TestReadyParkedNote pins that the second-line note names the decision and
// tells the reader to check it before claiming or mutating — the exact guard
// bug ka-0000007469 was missing.
func TestReadyParkedNote(t *testing.T) {
	note := readyParkedNote("gd-decX")
	for _, frag := range []string{"gd-decX", "PARKED", "claiming"} {
		if !strings.Contains(note, frag) {
			t.Errorf("note = %q, want it to contain %q", note, frag)
		}
	}
	// Empty id still yields a conservative warning, never a bare/empty note.
	if generic := readyParkedNote(""); !strings.Contains(generic, "PARKED") {
		t.Errorf("note with empty id = %q, want a conservative PARKED warning", generic)
	}
}

// readyParkedWireBead is the minimal JSON shape the fake daemon returns for the
// ready-render test.
type readyParkedWireBead struct {
	ID                     string `json:"id"`
	Title                  string `json:"title"`
	Type                   string `json:"type"`
	Status                 string `json:"status"`
	ParkedOnDecision       bool   `json:"parked_on_decision,omitempty"`
	ParkedOnDecisionBeadID string `json:"parked_on_decision_bead_id,omitempty"`
}

// TestReadyRenderMarksParkedRow drives `gb ready` end-to-end against a fake
// daemon and asserts:
//   - a row the daemon flags parked_on_decision renders the [parked: <id>]
//     badge + the ⚠ PARKED note naming the decision;
//   - a row NOT flagged renders neither (no false positive) — the acceptance
//     criterion "no false positives on beads with no operator gates".
func TestReadyRenderMarksParkedRow(t *testing.T) {
	parked := readyParkedWireBead{
		ID: "gd-parked1", Title: "parked task", Type: "task", Status: "open",
		ParkedOnDecision: true, ParkedOnDecisionBeadID: "gd-decOPEN",
	}
	free := readyParkedWireBead{
		ID: "gd-free1", Title: "free task", Type: "task", Status: "open",
	}

	var sawParkedProjection bool
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/beads", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		// The ready query is the no_open_deps list; the satisfy-gate and
		// liveness helpers issue their own type=decision / type=agent queries.
		// Assert the projection only on the ready query, and return the parked
		// rows only there so the helper calls do not echo them back.
		if q.Get("no_open_deps") == "true" {
			if q.Get("with_parked_on_decision") == "true" {
				sawParkedProjection = true
			}
			rows := []readyParkedWireBead{parked, free}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": rows, "total": len(rows)})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": []readyParkedWireBead{}, "total": 0})
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

	out := captureReadyStdout(t, func() {
		cmd := readyCmd
		cmd.SetContext(context.Background())
		// Human (non-JSON) render path; default project scope off so the fake
		// daemon's rows are not narrowed by the server-side exclusive_project filter.
		_ = cmd.Flags().Set("all-projects", "true")
		t.Cleanup(func() { _ = cmd.Flags().Set("all-projects", "false") })
		if err := cmd.RunE(cmd, nil); err != nil {
			t.Fatalf("ready RunE: %v", err)
		}
	})

	if !sawParkedProjection {
		t.Error("ready query did not request with_parked_on_decision=true")
	}
	if !strings.Contains(out, "[parked: gd-decOPEN]") {
		t.Errorf("output missing parked badge for gd-parked1:\n%s", out)
	}
	if !strings.Contains(out, "PARKED behind open decision gd-decOPEN") {
		t.Errorf("output missing parked note for gd-parked1:\n%s", out)
	}
	// No false positive: the free row must carry no parked marker.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "gd-free1") && strings.Contains(line, "parked") {
			t.Errorf("free row must not be marked parked, got: %q", line)
		}
	}
}

// TestReadyRenderParkedBadgeClearsWhenDecisionCloses is the disappearance half
// of the acceptance criterion: once the daemon stops flagging the row (the
// operator resolved+closed the decision, so parked_on_decision goes false), the
// badge and note are gone. We model that as a second render where the same row
// comes back un-flagged.
func TestReadyRenderParkedBadgeClearsWhenDecisionCloses(t *testing.T) {
	var flagged bool
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/beads", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("no_open_deps") != "true" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": []readyParkedWireBead{}, "total": 0})
			return
		}
		row := readyParkedWireBead{ID: "gd-row", Title: "task", Type: "task", Status: "open"}
		if flagged {
			row.ParkedOnDecision = true
			row.ParkedOnDecisionBeadID = "gd-dec"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": []readyParkedWireBead{row}, "total": 1})
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

	render := func() string {
		return captureReadyStdout(t, func() {
			cmd := readyCmd
			cmd.SetContext(context.Background())
			_ = cmd.Flags().Set("all-projects", "true")
			t.Cleanup(func() { _ = cmd.Flags().Set("all-projects", "false") })
			if err := cmd.RunE(cmd, nil); err != nil {
				t.Fatalf("ready RunE: %v", err)
			}
		})
	}

	// Open decision → parked.
	flagged = true
	if got := render(); !strings.Contains(got, "[parked: gd-dec]") {
		t.Errorf("with open decision, expected parked badge, got:\n%s", got)
	}
	// Decision closed → daemon no longer flags → badge disappears.
	flagged = false
	if got := render(); strings.Contains(got, "parked") {
		t.Errorf("after decision closes, parked badge must disappear, got:\n%s", got)
	}
}

// captureReadyStdout runs fn with os.Stdout redirected to a pipe and returns
// everything written. Mirrors the capture idiom used across the gb tests.
func captureReadyStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	old := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	fn()
	_ = w.Close()
	os.Stdout = old
	return <-done
}
