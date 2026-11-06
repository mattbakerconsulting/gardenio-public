package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	gardeniometrics "gardenio/controller/internal/metrics"
)

// salvageMockDaemon is a minimal in-memory daemon covering every endpoint the
// involuntary-salvage path in closeAgentBeadBestEffort exercises:
//   - GET  /v1/beads/<agentBead>            (GetBead: alreadyClosed + tool-call guard)
//   - GET  /v1/beads?status=in_progress&…   (ListAssignedTask: worked-task lookup)
//   - PATCH /v1/beads/<task>                (UpdateBead: reopen)
//   - POST /v1/beads/<task>/comments        (AddComment: audit trail)
//   - POST /v1/beads/<agentBead>/close      (CloseBead: capture close fields)
type salvageMockDaemon struct {
	agentBeadID string
	// agentBead is the JSON body returned for GetBead(agentBeadID).
	agentBead map[string]any
	// assignedTask is returned by ListAssignedTask; nil => no claim held.
	assignedTask map[string]any
	// listFails makes every ListAssignedTask call return HTTP 500.
	listFails bool

	closeBody    map[string]any
	closePosts   atomic.Int64
	reopenPatch  map[string]any
	reopenedID   string
	commentText  string
	commentPosts atomic.Int64
}

func (m *salvageMockDaemon) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads/"+m.agentBeadID:
			_ = json.NewEncoder(w).Encode(m.agentBead)

		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads":
			if m.listFails {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			beads := []map[string]any{}
			if m.assignedTask != nil &&
				r.URL.Query().Get("status") == "in_progress" &&
				r.URL.Query().Get("kind") == "issue" {
				beads = append(beads, m.assignedTask)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": beads, "total": len(beads)})

		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/v1/beads/"):
			m.reopenedID = strings.TrimPrefix(r.URL.Path, "/v1/beads/")
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &m.reopenPatch)
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			m.commentPosts.Add(1)
			body, _ := io.ReadAll(r.Body)
			var c map[string]any
			_ = json.Unmarshal(body, &c)
			m.commentText, _ = c["text"].(string)
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodPost && r.URL.Path == "/v1/beads/"+m.agentBeadID+"/close":
			m.closePosts.Add(1)
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &m.closeBody)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": m.agentBeadID})

		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}
}

// TestCloseAgentBeadBestEffort_InvoluntaryReopensAndSynthesizesWrapup is the
// end-to-end EOL-salvage Phase 2 assertion (ka-0000002059): an involuntary
// context_cancelled SIGTERM close, while the agent still holds a claimed
// in_progress issue, must (a) reopen that task to the ready queue with an audit
// comment, and (b) close the agent bead with a synthesized wrap-up stamped
// `context_cancelled_involuntary` — so gardener-audit sees wrapup_present=true
// and the kill is distinguishable from a crash.
func TestCloseAgentBeadBestEffort_InvoluntaryReopensAndSynthesizesWrapup(t *testing.T) {
	const beadID = "gd-agent-involuntary"
	mock := &salvageMockDaemon{
		agentBeadID: beadID,
		agentBead: map[string]any{
			"id":     beadID,
			"type":   "agent",
			"status": "open",
			"fields": map[string]any{"agent": "gardenio-crew-xyz"},
		},
		assignedTask: map[string]any{
			"id":       "gd-WORKED1",
			"title":    "wire the thing",
			"kind":     "issue",
			"type":     "task",
			"status":   "in_progress",
			"assignee": "gardenio-crew-xyz",
		},
	}
	srv := httptest.NewServer(mock.handler(t))
	t.Cleanup(srv.Close)
	swapDaemon(t, srv.URL)
	withActor(t, "gardenio-crew-xyz")
	neutralizeSlackEnv(t)

	t.Setenv("KD_AGENT_ID", beadID)
	t.Setenv("BOAT_AGENT_BEAD_ID", "")

	closeAgentBeadBestEffort("context_cancelled")

	// The worked task was returned to the ready queue.
	if mock.reopenedID != "gd-WORKED1" {
		t.Fatalf("expected worked task gd-WORKED1 reopened, got %q", mock.reopenedID)
	}
	if s, _ := mock.reopenPatch["status"].(string); s != "open" {
		t.Errorf("expected status=open on reopen, got %v", mock.reopenPatch["status"])
	}
	if a, ok := mock.reopenPatch["assignee"]; !ok || a != "" {
		t.Errorf("expected assignee cleared on reopen, got %v (present=%v)", a, ok)
	}
	if mock.commentPosts.Load() == 0 || !strings.Contains(mock.commentText, "Reopened by stop-gate") {
		t.Errorf("expected an audit comment on reopen, got posts=%d text=%q", mock.commentPosts.Load(), mock.commentText)
	}

	// The agent bead was closed exactly once, state=done (graceful termination).
	if got := mock.closePosts.Load(); got != 1 {
		t.Fatalf("expected exactly 1 close POST, got %d", got)
	}
	if st, _ := mock.closeBody["agent_state"].(string); st != "done" {
		t.Errorf("expected agent_state=done, got %v", mock.closeBody["agent_state"])
	}

	// The close reason is stamped _involuntary and keeps the context_cancelled
	// prefix (so downstream HasPrefix readers stay compatible), and names the
	// reopened task.
	cr, _ := mock.closeBody["close_reason"].(string)
	if !strings.HasPrefix(cr, "context_cancelled") {
		t.Errorf("close_reason must keep the context_cancelled prefix, got %q", cr)
	}
	if !strings.Contains(cr, "_involuntary") {
		t.Errorf("close_reason must be marked _involuntary, got %q", cr)
	}
	if !strings.Contains(cr, "gd-WORKED1") {
		t.Errorf("close_reason should name the reopened task, got %q", cr)
	}

	// A fallback wrap-up was synthesized (gardener-audit wrapup_present=true) and
	// reads as an involuntary mid-work death, not an idle-out.
	rawWU, _ := mock.closeBody["wrapup"].(string)
	if rawWU == "" {
		t.Fatalf("expected a synthesized wrapup on the close, got none")
	}
	var wu WrapUp
	if err := json.Unmarshal([]byte(rawWU), &wu); err != nil {
		t.Fatalf("wrapup is not valid JSON: %v\n%s", err, rawWU)
	}
	if strings.Contains(strings.ToLower(wu.TLDR), "idled out") {
		t.Errorf("involuntary close must not read 'idled out', got %q", wu.TLDR)
	}
	if !strings.Contains(strings.ToLower(wu.TLDR), "involuntar") {
		t.Errorf("involuntary close tldr should say so, got %q", wu.TLDR)
	}
}

// TestCloseAgentBeadBestEffort_InvoluntaryNoClaimStampsUnclaimed verifies the
// C9 classification (ka-0000001146) layered on the ka-0000008254 contract: a
// busy-but-taskless teardown (tool calls on the bead, no claimed issue task) is
// still NOT stamped `_involuntary` — that marker requires a genuinely-reopened
// orphaned ISSUE task, preserving the MR !5571 precision fix — but no longer
// vanishes from the deaths counter either: it gets the distinct
// `context_cancelled_unclaimed` reason, which the metric counts as its own
// family. The wrap-up is still synthesized and reports the tool calls honestly.
func TestCloseAgentBeadBestEffort_InvoluntaryNoClaimStampsUnclaimed(t *testing.T) {
	const beadID = "gd-agent-busy-noclaim"
	mock := &salvageMockDaemon{
		agentBeadID: beadID,
		agentBead: map[string]any{
			"id":     beadID,
			"type":   "agent",
			"status": "open",
			"fields": map[string]any{"total_tool_calls": "42"},
		},
		assignedTask: nil, // no claimed task
	}
	srv := httptest.NewServer(mock.handler(t))
	t.Cleanup(srv.Close)
	swapDaemon(t, srv.URL)
	withActor(t, "gardenio-crew-noclaim")
	neutralizeSlackEnv(t)

	t.Setenv("KD_AGENT_ID", beadID)
	t.Setenv("BOAT_AGENT_BEAD_ID", "")

	closeAgentBeadBestEffort("context_cancelled")

	if mock.reopenedID != "" {
		t.Errorf("no task should be reopened when none is claimed, reopened %q", mock.reopenedID)
	}
	if got := mock.closePosts.Load(); got != 1 {
		t.Fatalf("expected exactly 1 close POST, got %d", got)
	}
	cr, _ := mock.closeBody["close_reason"].(string)
	if !strings.HasPrefix(cr, "context_cancelled_unclaimed") {
		t.Errorf("busy taskless teardown must stamp the context_cancelled_unclaimed reason (C9 ka-0000001146), got %q", cr)
	}
	if strings.Contains(cr, "_involuntary") {
		t.Errorf("tool-calls>0 alone must NOT stamp _involuntary (ka-0000008254), got %q", cr)
	}
	rawWU, _ := mock.closeBody["wrapup"].(string)
	if rawWU == "" {
		t.Fatalf("expected a synthesized wrapup, got none")
	}
	var wu WrapUp
	if err := json.Unmarshal([]byte(rawWU), &wu); err != nil {
		t.Fatalf("wrapup is not valid JSON: %v\n%s", err, rawWU)
	}
	// 42 tool calls → the honest "after N tool calls" headline, never idled-out.
	if strings.Contains(strings.ToLower(wu.TLDR), "idled out") {
		t.Errorf("busy teardown must not read 'idled out', got %q", wu.TLDR)
	}
}

// TestCloseAgentBeadBestEffort_ClaimLookupFailedCountsAsInvoluntary is the
// gd-TYjMaxCMZR regression on the SIGTERM teardown path: when ListAssignedTask
// fails every retry, the close reason must still count as an involuntary death
// under its own family, so the kill reaches
// gardenio_agent_involuntary_deaths_total and the Slack death report.
func TestCloseAgentBeadBestEffort_ClaimLookupFailedCountsAsInvoluntary(t *testing.T) {
	const beadID = "gd-agent-lookup-failed"
	mock := &salvageMockDaemon{
		agentBeadID: beadID,
		agentBead: map[string]any{
			"id":     beadID,
			"type":   "agent",
			"status": "open",
			"fields": map[string]any{"total_tool_calls": "42"},
		},
		listFails: true,
	}
	srv := httptest.NewServer(mock.handler(t))
	t.Cleanup(srv.Close)
	swapDaemon(t, srv.URL)
	withActor(t, "gardenio-crew-lookupfail")
	withFastReopenRetries(t)
	neutralizeSlackEnv(t)

	t.Setenv("KD_AGENT_ID", beadID)
	t.Setenv("BOAT_AGENT_BEAD_ID", "")

	closeAgentBeadBestEffort("context_cancelled")

	if got := mock.closePosts.Load(); got != 1 {
		t.Fatalf("expected exactly 1 close POST, got %d", got)
	}
	cr, _ := mock.closeBody["close_reason"].(string)
	if !strings.HasPrefix(cr, "context_cancelled_claim_lookup_failed") {
		t.Errorf("failed claim lookup must stamp context_cancelled_claim_lookup_failed (tool calls must not downgrade it to _unclaimed), got %q", cr)
	}
	fam, ok := gardeniometrics.InvoluntaryDeathFamily(cr)
	if !ok || fam != gardeniometrics.FamilyContextCancelledClaimLookupFailed {
		t.Errorf("InvoluntaryDeathFamily(%q) = (%q, %t), want (%q, true)", cr, fam, ok, gardeniometrics.FamilyContextCancelledClaimLookupFailed)
	}
}

// TestCloseAgentBeadBestEffort_CleanDoneCloseNeverInvoluntary is the direct
// ka-0000008254 regression: an agent that already closed its own bead cleanly
// with agent_state=done — but left BOTH close_reason and wrapup empty (a bare
// `gb stop` without --wrapup, the common gardener-loop recycle shape) — must NOT
// be reopened or re-stamped `context_cancelled_involuntary` by the teardown
// path. A finished, self-closed agent is by definition not an involuntary death.
func TestCloseAgentBeadBestEffort_CleanDoneCloseNeverInvoluntary(t *testing.T) {
	const beadID = "gd-agent-clean-done"
	var closePosts atomic.Int64
	var listCalled atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads/"+beadID:
			// Already closed cleanly: agent_state=done, but no close_reason and
			// no wrapup (bare `gb stop`). A gardener that just ran its watchdog
			// pass has tool calls recorded — the OLD tool-calls>0 fallback would
			// have wrongly stamped this _involuntary.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":     beadID,
				"type":   "agent",
				"status": "closed",
				"fields": map[string]any{
					"agent_state":      "done",
					"total_tool_calls": "137",
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads":
			listCalled.Store(true)
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/beads/"+beadID+"/close":
			closePosts.Add(1)
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": beadID})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	swapDaemon(t, srv.URL)
	withActor(t, "gardenio-plan-gardener-xyz")
	neutralizeSlackEnv(t)

	t.Setenv("KD_AGENT_ID", beadID)
	t.Setenv("BOAT_AGENT_BEAD_ID", "")

	closeAgentBeadBestEffort("context_cancelled")

	// The guard must return BEFORE the reopen/close block: no task lookup, no
	// overwriting close POST.
	if listCalled.Load() {
		t.Error("a clean agent_state=done close must not run the reopen/salvage path")
	}
	if got := closePosts.Load(); got != 0 {
		t.Errorf("a clean agent_state=done close must not be overwritten, got %d close POSTs", got)
	}
}

// TestCloseAgentBeadBestEffort_InvoluntaryZeroWorkStaysPlain verifies the
// no-work idle case: an involuntary teardown with no claimed task and zero tool
// calls closes with a synthesized wrap-up but keeps the PLAIN context_cancelled
// close reason (no _involuntary marker), so a genuine zero-work idle is not
// dressed up as a mid-work death.
func TestCloseAgentBeadBestEffort_InvoluntaryZeroWorkStaysPlain(t *testing.T) {
	const beadID = "gd-agent-idle"
	mock := &salvageMockDaemon{
		agentBeadID: beadID,
		agentBead: map[string]any{
			"id":     beadID,
			"type":   "agent",
			"status": "open",
			"fields": map[string]any{}, // no tool calls, no agent name
		},
		assignedTask: nil,
	}
	srv := httptest.NewServer(mock.handler(t))
	t.Cleanup(srv.Close)
	swapDaemon(t, srv.URL)
	withActor(t, "gardenio-crew-idle")
	neutralizeSlackEnv(t)

	t.Setenv("KD_AGENT_ID", beadID)
	t.Setenv("BOAT_AGENT_BEAD_ID", "")

	closeAgentBeadBestEffort("context_cancelled")

	if got := mock.closePosts.Load(); got != 1 {
		t.Fatalf("expected exactly 1 close POST, got %d", got)
	}
	cr, _ := mock.closeBody["close_reason"].(string)
	if cr != "context_cancelled" {
		t.Errorf("zero-work idle should keep the plain context_cancelled reason, got %q", cr)
	}
	if strings.Contains(cr, "_involuntary") {
		t.Errorf("zero-work idle must NOT be marked involuntary, got %q", cr)
	}
	// Still synthesizes a fallback wrap-up so the card is not metadata-only.
	if rawWU, _ := mock.closeBody["wrapup"].(string); rawWU == "" {
		t.Errorf("expected a synthesized wrapup even for a zero-work idle")
	}
}

// TestCloseAgentBeadBestEffort_MaxRestartsSkipsSalvage verifies that a genuine
// crash (max_restarts_reached, state=failed) is NOT run through the salvage
// path: no task is reopened, no wrap-up is synthesized, and the bare close keeps
// the crash reason. Salvage is scoped to the graceful context_cancelled case.
func TestCloseAgentBeadBestEffort_MaxRestartsSkipsSalvage(t *testing.T) {
	const beadID = "gd-agent-crashed"
	var reopenCalled atomic.Bool
	var closeBody map[string]any
	var closePosts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads/"+beadID:
			// Bead is still open with a claimed-looking history — the salvage
			// path, if wrongly taken, would try to reopen/synthesize.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":     beadID,
				"type":   "agent",
				"status": "open",
				"fields": map[string]any{"total_tool_calls": "10"},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads":
			// ListAssignedTask — reaching here means the salvage path ran.
			reopenCalled.Store(true)
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/beads/"+beadID+"/close":
			closePosts.Add(1)
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &closeBody)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": beadID})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	swapDaemon(t, srv.URL)
	withActor(t, "gardenio-crew-crashed")
	neutralizeSlackEnv(t)

	t.Setenv("KD_AGENT_ID", beadID)
	t.Setenv("BOAT_AGENT_BEAD_ID", "")

	closeAgentBeadBestEffort("max_restarts_reached")

	if reopenCalled.Load() {
		t.Errorf("a crash close must not run the reopen/salvage path")
	}
	if got := closePosts.Load(); got != 1 {
		t.Fatalf("expected exactly 1 close POST, got %d", got)
	}
	if st, _ := closeBody["agent_state"].(string); st != "failed" {
		t.Errorf("a crash close should keep agent_state=failed, got %v", closeBody["agent_state"])
	}
	if cr, _ := closeBody["close_reason"].(string); cr != "max_restarts_reached" {
		t.Errorf("a crash close should keep the bare crash reason, got %q", cr)
	}
	if _, ok := closeBody["wrapup"]; ok {
		t.Errorf("a crash close must not synthesize a wrap-up, got %v", closeBody["wrapup"])
	}
}
