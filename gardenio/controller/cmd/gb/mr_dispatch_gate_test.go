package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"

	"github.com/spf13/cobra"
)

// dispatchGateStubDaemon stands up an httptest daemon serving the endpoints
// runMRDispatchGate touches: GetBead, GetDependencies (forward), and
// BatchGetBeads. The graph is a single task with the given forward deps and the
// given blocker beads.
func dispatchGateStubDaemon(t *testing.T, taskID string, deps []beadsapi.Dependency, blockers map[string]map[string]any) *beadsapi.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/dependencies"):
			// GET /v1/beads/{id}/dependencies — forward deps of the task.
			depRows := make([]map[string]any, 0, len(deps))
			for _, d := range deps {
				depRows = append(depRows, map[string]any{
					"bead_id": d.BeadID, "depends_on_id": d.DependsOnID, "type": d.Type,
				})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"dependencies": depRows})
		case strings.HasPrefix(path, "/v1/batch/beads"):
			// GET /v1/batch/beads?ids=... — blocker bead details.
			ids := strings.Split(r.URL.Query().Get("ids"), ",")
			results := make([]map[string]any, 0, len(ids))
			for _, id := range ids {
				if b, ok := blockers[id]; ok {
					results = append(results, map[string]any{"id": id, "bead": b})
				} else {
					results = append(results, map[string]any{"id": id, "error": "not found"})
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
		case strings.HasPrefix(path, "/v1/beads/"):
			// GET /v1/beads/{id} — the task bead itself (a plain task, so
			// resolveDispatchGateTaskBead inspects its deps directly).
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": taskID, "type": "task", "status": "open", "title": "gated task",
				"fields": map[string]string{},
			})
		default:
			http.Error(w, "unexpected path "+path, http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	return c
}

// TestRunMRDispatchGate_MayDispatchJSON exercises the exit-0 (may-dispatch) path
// end-to-end through the CLI handler: a task with a CLOSED decision blocker is
// genuinely actionable, so the gate returns may_dispatch=true and the handler
// does NOT call os.Exit. Asserts the resolver walks task deps and the JSON
// projection is correct.
func TestRunMRDispatchGate_MayDispatchJSON(t *testing.T) {
	c := dispatchGateStubDaemon(t, "gd-task",
		[]beadsapi.Dependency{{BeadID: "gd-task", DependsOnID: "gd-dec", Type: "blocks"}},
		map[string]map[string]any{
			// A closed decision no longer blocks → dispatch.
			"gd-dec": {"id": "gd-dec", "type": "decision", "status": "closed", "title": "resolved"},
		},
	)
	oldDaemon, oldJSON := daemon, jsonOutput
	daemon, jsonOutput = c, true
	t.Cleanup(func() { daemon, jsonOutput = oldDaemon, oldJSON })

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())

	raw := captureStdout(t, func() error { return runMRDispatchGate(cmd, []string{"gd-task"}) })

	var got struct {
		TaskID      string   `json:"task_id"`
		MayDispatch bool     `json:"may_dispatch"`
		Reason      string   `json:"reason"`
		BlockerIDs  []string `json:"blocker_ids"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal output %q: %v", string(raw), err)
	}
	if got.TaskID != "gd-task" {
		t.Errorf("task_id = %q, want gd-task", got.TaskID)
	}
	if !got.MayDispatch {
		t.Errorf("may_dispatch = false, want true (blocker is closed); reason=%q", got.Reason)
	}
	if len(got.BlockerIDs) != 0 {
		t.Errorf("blocker_ids = %v, want empty", got.BlockerIDs)
	}
}

// TestResolveDispatchGateTaskBead_PlainTask confirms a plain task bead id
// resolves to itself (the deps-directly path), not through the MR-bead walk.
func TestResolveDispatchGateTaskBead_PlainTask(t *testing.T) {
	c := dispatchGateStubDaemon(t, "gd-task", nil, nil)
	oldDaemon := daemon
	daemon = c
	t.Cleanup(func() { daemon = oldDaemon })

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())

	got, err := resolveDispatchGateTaskBead(cmd, "gd-task")
	if err != nil {
		t.Fatalf("resolveDispatchGateTaskBead: %v", err)
	}
	if got == nil || got.ID != "gd-task" {
		t.Fatalf("resolved bead = %+v, want id gd-task", got)
	}
}

// refCandidateStubDaemon serves GET /v1/beads returning the given beads, so
// gatherDispatchGateRefCandidates can be exercised end-to-end. It asserts the
// helper asks for BOTH handle types in a single query (type=decision,escalation)
// so the one-call efficiency contract is covered, then echoes the beads back.
func refCandidateStubDaemon(t *testing.T, beads []map[string]any) *beadsapi.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/v1/beads" {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
			return
		}
		if typ := r.URL.Query().Get("type"); typ != "decision,escalation" {
			t.Errorf("expected a single type=decision,escalation query, got type=%q", typ)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": beads, "total": len(beads)})
	}))
	t.Cleanup(srv.Close)
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	return c
}

// TestGatherDispatchGateRefCandidates confirms the helper issues ONE bounded
// listing for both handle types and returns them — the durable, non-paginated
// replacement for the old `kd list ... | jq contains` first-page scan.
func TestGatherDispatchGateRefCandidates(t *testing.T) {
	c := refCandidateStubDaemon(t, []map[string]any{
		{"id": "gd-dec1", "type": "decision", "status": "open", "title": "Merge MR !6428"},
		{"id": "gd-esc1", "type": "escalation", "status": "open", "title": "approve + merge !6428"},
	})
	oldDaemon := daemon
	daemon = c
	t.Cleanup(func() { daemon = oldDaemon })

	got := gatherDispatchGateRefCandidates(context.Background())
	if len(got) != 2 {
		t.Fatalf("gatherDispatchGateRefCandidates returned %d beads, want 2 (one decision + one escalation)", len(got))
	}
	ids := map[string]bool{}
	for _, b := range got {
		ids[b.ID] = true
	}
	if !ids["gd-dec1"] || !ids["gd-esc1"] {
		t.Errorf("candidate ids = %v, want both gd-dec1 and gd-esc1", ids)
	}
}

// TestGatherDispatchGateRefCandidates_QueryErrorFailsOpen confirms a daemon error
// yields no candidates (empty slice) rather than aborting — the reference signal
// degrades to the blocking-edge signal alone.
func TestGatherDispatchGateRefCandidates_QueryErrorFailsOpen(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	oldDaemon := daemon
	daemon = c
	t.Cleanup(func() { daemon = oldDaemon })

	if got := gatherDispatchGateRefCandidates(context.Background()); len(got) != 0 {
		t.Errorf("expected no candidates on daemon error, got %d", len(got))
	}
}

// TestHumanizeGateAge covers the compact "Xh Ym" / "Ym" rendering used in the
// aging-nudge message.
func TestHumanizeGateAge(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{2 * time.Hour, "2h 0m"},
		{2*time.Hour + 5*time.Minute, "2h 5m"},
		{90 * time.Minute, "1h 30m"},
		{45 * time.Minute, "45m"},
		{20*time.Second + 45*time.Minute, "45m"}, // rounds to nearest minute (down)
		{40*time.Second + 45*time.Minute, "46m"}, // rounds to nearest minute (up)
		{0, "0m"},
	}
	for _, tc := range cases {
		if got := humanizeGateAge(tc.d); got != tc.want {
			t.Errorf("humanizeGateAge(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

// TestFormatDispatchGateNudge asserts the operator-facing comment carries the
// three facts acceptance criterion 3 requires: the MR URL, the decision id, and
// the time since gating began.
func TestFormatDispatchGateNudge(t *testing.T) {
	in := bridge.DispatchGateInput{
		MRURL: "https://gitlab.com/Acme/Platform/gardenio/-/merge_requests/6428",
		MRIID: 6428,
	}
	nudge := &bridge.DispatchGateNudge{
		HandleID:   "ka-0000004748",
		HandleType: "decision",
		GateAge:    2*time.Hour + 30*time.Minute,
	}
	got := formatDispatchGateNudge(in, nudge)
	for _, want := range []string{
		"https://gitlab.com/Acme/Platform/gardenio/-/merge_requests/6428", // MR URL
		"ka-0000004748", // decision id
		"2h 30m",        // time since gating began
		"decision",      // handle type
	} {
		if !strings.Contains(got, want) {
			t.Errorf("nudge comment missing %q; got:\n%s", want, got)
		}
	}
}

// TestFormatDispatchGateNudge_FallsBackToBangRef confirms an MR with no resolved
// URL still names the MR by !iid rather than emitting a bare "this MR".
func TestFormatDispatchGateNudge_FallsBackToBangRef(t *testing.T) {
	in := bridge.DispatchGateInput{MRIID: 6428}
	nudge := &bridge.DispatchGateNudge{HandleID: "gd-dec", HandleType: "decision", GateAge: 3 * time.Hour}
	got := formatDispatchGateNudge(in, nudge)
	if !strings.Contains(got, "!6428") {
		t.Errorf("expected a !6428 bang-ref when URL is empty; got:\n%s", got)
	}
}

// nudgeRecordingDaemon records the comment POST and field PATCH
// maybePostDispatchGateNudge issues, so the write path can be asserted without a
// live daemon. GET /v1/beads/{id} returns the handle (so UpdateBeadFields can
// read existing fields), POST .../comments records the comment, PATCH records the
// stamped fields. failComment / failPatch flip the respective endpoint to 500.
type nudgeRecordingDaemon struct {
	srv           *httptest.Server
	gotComment    string
	gotCommentTo  string
	gotPatchField map[string]any
	failComment   bool
	failPatch     bool
}

func newNudgeRecordingDaemon(t *testing.T) (*beadsapi.Client, *nudgeRecordingDaemon) {
	t.Helper()
	rec := &nudgeRecordingDaemon{}
	rec.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/comments"):
			if rec.failComment {
				http.Error(w, "comment boom", http.StatusInternalServerError)
				return
			}
			var body struct {
				Author string `json:"author"`
				Text   string `json:"text"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			rec.gotComment = body.Text
			rec.gotCommentTo = strings.TrimSuffix(strings.TrimPrefix(path, "/v1/beads/"), "/comments")
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		case r.Method == http.MethodPatch && strings.HasPrefix(path, "/v1/beads/"):
			if rec.failPatch {
				http.Error(w, "patch boom", http.StatusInternalServerError)
				return
			}
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if f, ok := body["fields"].(map[string]any); ok {
				rec.gotPatchField = f
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		case r.Method == http.MethodGet && strings.HasPrefix(path, "/v1/beads/"):
			// The handle bead UpdateBeadFields reads before patching. No existing
			// fields, so the marker is a fresh key.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "gd-dec", "type": "decision", "status": "open", "fields": map[string]string{},
			})
		default:
			http.Error(w, "unexpected "+r.Method+" "+path, http.StatusNotFound)
		}
	}))
	t.Cleanup(rec.srv.Close)
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: rec.srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	return c, rec
}

// TestMaybePostDispatchGateNudge_PostsAndStamps confirms the happy path: the
// comment lands on the handle bead AND the one-shot marker is stamped, and the
// helper reports posted=true.
func TestMaybePostDispatchGateNudge_PostsAndStamps(t *testing.T) {
	c, rec := newNudgeRecordingDaemon(t)
	oldDaemon := daemon
	daemon = c
	t.Cleanup(func() { daemon = oldDaemon })

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	in := bridge.DispatchGateInput{
		MRURL: "https://gitlab.com/Acme/Platform/gardenio/-/merge_requests/6428",
		MRIID: 6428,
		Now:   time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC),
	}
	nudge := &bridge.DispatchGateNudge{HandleID: "gd-dec", HandleType: "decision", GateAge: 3 * time.Hour}

	posted := maybePostDispatchGateNudge(context.Background(), cmd, in, nudge)
	if !posted {
		t.Fatalf("maybePostDispatchGateNudge returned false, want true (comment succeeded)")
	}
	if rec.gotCommentTo != "gd-dec" {
		t.Errorf("comment posted to %q, want gd-dec", rec.gotCommentTo)
	}
	if !strings.Contains(rec.gotComment, "6428") {
		t.Errorf("comment missing MR ref; got %q", rec.gotComment)
	}
	if rec.gotPatchField[bridge.DispatchGateNudgedAtField] == nil {
		t.Errorf("one-shot marker %q not stamped; patch fields=%v", bridge.DispatchGateNudgedAtField, rec.gotPatchField)
	}
}

// TestMaybePostDispatchGateNudge_NilNudgeNoop confirms a nil nudge is a no-op
// (no writes, returns false).
func TestMaybePostDispatchGateNudge_NilNudgeNoop(t *testing.T) {
	c, rec := newNudgeRecordingDaemon(t)
	oldDaemon := daemon
	daemon = c
	t.Cleanup(func() { daemon = oldDaemon })

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if posted := maybePostDispatchGateNudge(context.Background(), cmd, bridge.DispatchGateInput{}, nil); posted {
		t.Errorf("expected no-op false for a nil nudge, got true")
	}
	if rec.gotComment != "" {
		t.Errorf("expected no comment for a nil nudge, got %q", rec.gotComment)
	}
}

// TestMaybePostDispatchGateNudge_CommentFailsSkipsStamp confirms that when the
// comment write fails, the one-shot marker is NOT stamped — so a later cycle
// retries the nudge rather than silently dropping it — and the helper reports
// posted=false.
func TestMaybePostDispatchGateNudge_CommentFailsSkipsStamp(t *testing.T) {
	c, rec := newNudgeRecordingDaemon(t)
	rec.failComment = true
	oldDaemon := daemon
	daemon = c
	t.Cleanup(func() { daemon = oldDaemon })

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetErr(new(strings.Builder))
	nudge := &bridge.DispatchGateNudge{HandleID: "gd-dec", HandleType: "decision", GateAge: 3 * time.Hour}

	posted := maybePostDispatchGateNudge(context.Background(), cmd, bridge.DispatchGateInput{MRIID: 6428, Now: time.Now()}, nudge)
	if posted {
		t.Errorf("expected posted=false when the comment fails, got true")
	}
	if rec.gotPatchField != nil {
		t.Errorf("expected NO marker stamp when the comment failed (retry next cycle), got %v", rec.gotPatchField)
	}
}
