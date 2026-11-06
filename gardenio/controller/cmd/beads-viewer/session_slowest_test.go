package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func TestBuildSessionSlowCalls(t *testing.T) {
	bead := &beadsapi.BeadDetail{Type: "session", Fields: map[string]string{
		"slowest_calls": `[
			{"ts":"2026-10-11T04:00:05Z","tool":"Read","duration_ms":900,"source":"claude","args":"{\"file_path\":\"/x\"}"},
			{"ts":"2026-10-11T04:00:01Z","tool":"Bash","duration_ms":125000,"source":"derived","cmd_class":"build","args":"make build","truncated_args":true}
		]`,
	}}
	rows := buildSessionSlowCalls(bead)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	// Re-sorted slowest-first regardless of stored order.
	if rows[0].Tool != "Bash" || rows[0].Rank != 1 || rows[0].Duration != "2m 05s" {
		t.Errorf("row0 = %+v", rows[0])
	}
	if !rows[0].Derived || !rows[0].Truncated || rows[0].CmdClass != "build" || rows[0].Args != "make build" || rows[0].When != "04:00:01" {
		t.Errorf("row0 flags = %+v", rows[0])
	}
	if rows[1].Duration != "900ms" || rows[1].Derived {
		t.Errorf("row1 = %+v", rows[1])
	}
}

func TestBuildSessionSlowCalls_AbsentOrMalformed(t *testing.T) {
	for name, v := range map[string]string{"absent": "", "malformed": "{not json", "empty": "[]"} {
		bead := &beadsapi.BeadDetail{Fields: map[string]string{}}
		if v != "" {
			bead.Fields["slowest_calls"] = v
		}
		if rows := buildSessionSlowCalls(bead); rows != nil {
			t.Errorf("%s: rows = %v, want nil", name, rows)
		}
	}
}

func TestFmtCallDurationMs(t *testing.T) {
	for ms, want := range map[int64]string{0: "0ms", 999: "999ms", 1000: "1.0s", 12_400: "12.4s", 60_000: "1m 00s", 185_000: "3m 05s"} {
		if got := fmtCallDurationMs(ms); got != want {
			t.Errorf("fmtCallDurationMs(%d) = %q, want %q", ms, got, want)
		}
	}
}

// TestBeadShow_SessionSlowestCalls renders a session bead end to end: the
// Slowest tool calls table with the full redacted command line, and no raw
// slowest_calls blob duplicated in the generic Fields dump.
func TestBeadShow_SessionSlowestCalls(t *testing.T) {
	f := newFake()
	f.beads["gd-sess"] = &beadsapi.BeadDetail{
		ID:     "gd-sess",
		Title:  "session for agent-1",
		Type:   "session",
		Status: "closed",
		Fields: map[string]string{
			"status": "completed",
			"slowest_calls": `[{"ts":"2026-10-11T04:00:01Z","tool":"Bash","duration_ms":41200,"source":"claude",` +
				`"cmd_class":"build","args":"make build && go test ./... -run 'A|B'"}]`,
		},
	}
	mux := mkServer(t, f)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/gd-sess", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{"Slowest tool calls", "41.2s", "build", "make build &amp;&amp; go test ./... -run &#39;A|B&#39;"} {
		if !strings.Contains(body, want) {
			t.Errorf("session page missing %q\nbody: %s", want, body)
		}
	}
	if strings.Contains(body, `title="slowest_calls"`) {
		t.Error("raw slowest_calls blob duplicated in the Fields dump")
	}
}

func TestBeadShow_SessionWithoutSlowestCalls(t *testing.T) {
	f := newFake()
	f.beads["gd-sess2"] = &beadsapi.BeadDetail{ID: "gd-sess2", Title: "s", Type: "session", Status: "closed", Fields: map[string]string{"status": "completed"}}
	mux := mkServer(t, f)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/gd-sess2", nil))
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "Slowest tool calls") {
		t.Fatalf("status=%d, section should be absent", w.Code)
	}
}
