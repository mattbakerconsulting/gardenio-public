package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// reopenMockDaemon is a minimal in-memory daemon that serves the three
// endpoints reopenOrphanedClaimedTask exercises: ListAssignedTask
// (GET /v1/beads?status=in_progress&assignee=…&kind=issue), UpdateBead
// (PATCH /v1/beads/<id>, which may carry a nested "fields" object for the
// resume_s3_key handoff) and AddComment (POST /v1/beads/<id>/comments).
type reopenMockDaemon struct {
	// assignedTask is returned by the ListAssignedTask query. nil => no claim.
	assignedTask map[string]any
	// leadingTasks are further claims returned ahead of assignedTask.
	leadingTasks []map[string]any

	// ownBeadFields, when set, is returned for GET /v1/beads/<ownBeadID> — the
	// read currentS3SessionKeyForTask does to resolve the persisted
	// s3_session_key in the hook-process path. nil => GET returns a bead with
	// no fields (fresh pod: falls back to own bead id).
	ownBeadID     string
	ownBeadFields map[string]any

	// captured mutations
	patchBody   map[string]any // full reopen PATCH body (status/assignee + optional "fields")
	patchedID   string
	commentBody map[string]any
	commentedID string
}

// patchFields returns the nested "fields" object from the captured reopen
// PATCH, or nil when none was sent.
func (m *reopenMockDaemon) patchFields() map[string]any {
	if m.patchBody == nil {
		return nil
	}
	f, _ := m.patchBody["fields"].(map[string]any)
	return f
}

func (m *reopenMockDaemon) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads":
			// ListAssignedTask query. Honor the status/kind filter contract.
			beads := []map[string]any{}
			if r.URL.Query().Get("status") == "in_progress" &&
				r.URL.Query().Get("kind") == "issue" {
				beads = append(beads, m.leadingTasks...)
				if m.assignedTask != nil {
					beads = append(beads, m.assignedTask)
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": beads, "total": len(beads)})

		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/beads/"):
			// currentS3SessionKeyForTask's read of the OWN agent bead (hook-process
			// path resolving the persisted s3_session_key).
			id := strings.TrimPrefix(r.URL.Path, "/v1/beads/")
			fields := map[string]any{}
			if id == m.ownBeadID && m.ownBeadFields != nil {
				fields = m.ownBeadFields
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": id, "type": "agent", "status": "open", "fields": fields,
			})

		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/v1/beads/"):
			m.patchedID = strings.TrimPrefix(r.URL.Path, "/v1/beads/")
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &m.patchBody)
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			m.commentedID = strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/beads/"), "/comments")
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &m.commentBody)
			w.WriteHeader(http.StatusOK)

		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
			w.WriteHeader(http.StatusInternalServerError)
		}
	}
}

// clearS3ResumeEnv scrubs the S3 + agent-identity env keys that leak from a
// live agent pod into `go test` so the resume-chain code paths
// (reopenOrphanedClaimedTask handoff, resolveCanonicalS3Key) behave
// identically to hermetic CI. Individual tests opt INTO the S3 branch by
// setting COOP_S3_BUCKET / BOAT_TASK_ID explicitly after calling this.
func clearS3ResumeEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"COOP_S3_BUCKET", "COOP_S3_PREFIX", "COOP_S3_REGION", "COOP_S3_SESSION_KEY",
		"KD_AGENT_ID", "BOAT_AGENT_BEAD_ID", "BOAT_TASK_ID",
	} {
		t.Setenv(k, "")
	}
}

// withActor sets the package-global actor for the duration of a test.
func withActor(t *testing.T, name string) {
	t.Helper()
	old := actor
	actor = name
	t.Cleanup(func() { actor = old })
}

// withFastReopenRetries shrinks reopenOrphanedClaimedTask's retry knobs so a
// test exercising the ListAssignedTask-failure path doesn't actually wait out
// the production retry delay.
func withFastReopenRetries(t *testing.T) {
	t.Helper()
	oldAttempts, oldDelay := reopenLookupMaxAttempts, reopenLookupRetryDelay
	reopenLookupMaxAttempts = 3
	reopenLookupRetryDelay = time.Millisecond
	t.Cleanup(func() {
		reopenLookupMaxAttempts, reopenLookupRetryDelay = oldAttempts, oldDelay
	})
}

// TestReopenOrphanedClaimedTask_ReopensInProgressTask verifies the core
// ka-0000003464 item-3 fix: when a stop-gate close fires while the agent still
// holds a claimed in_progress task, the task is returned to the ready queue
// (status=open, assignee cleared) with an audit comment — not left orphaned.
func TestReopenOrphanedClaimedTask_ReopensInProgressTask(t *testing.T) {
	mock := &reopenMockDaemon{
		assignedTask: map[string]any{
			"id":       "gd-TASK1",
			"title":    "eTMF spec fixer step 3",
			"kind":     "issue",
			"type":     "task",
			"status":   "in_progress",
			"assignee": "gardenio-crew-abcd",
		},
	}
	srv := httptest.NewServer(mock.handler(t))
	defer srv.Close()
	swapDaemon(t, srv.URL)
	withActor(t, "gardenio-crew-abcd")
	clearS3ResumeEnv(t) // hermetic: no S3 => no resume_s3_key handoff branch

	got, lookupFailed := reopenOrphanedClaimedTask(context.Background(), "cooldown")

	if got != "gd-TASK1" {
		t.Fatalf("reopenOrphanedClaimedTask = %q, want gd-TASK1", got)
	}
	if lookupFailed {
		t.Errorf("expected lookupFailed=false on a successful lookup")
	}
	if mock.patchedID != "gd-TASK1" {
		t.Errorf("expected PATCH on gd-TASK1, got %q", mock.patchedID)
	}
	if s, _ := mock.patchBody["status"].(string); s != "open" {
		t.Errorf("expected status=open in patch, got %v", mock.patchBody["status"])
	}
	// Assignee must be cleared so the ready queue re-offers the work.
	if a, ok := mock.patchBody["assignee"]; !ok || a != "" {
		t.Errorf("expected assignee cleared (\"\") in patch, got %v (present=%v)", a, ok)
	}
	// With no S3 configured there must be no resume_s3_key handoff.
	if f := mock.patchFields(); f != nil {
		if _, ok := f[resumeS3KeyField]; ok {
			t.Errorf("no S3 bucket configured — resume_s3_key must not be stamped, got %v", f[resumeS3KeyField])
		}
	}
	// An audit comment must explain the involuntary reopen.
	if mock.commentedID != "gd-TASK1" {
		t.Errorf("expected an audit comment on gd-TASK1, got %q", mock.commentedID)
	}
	if txt, _ := mock.commentBody["text"].(string); !strings.Contains(txt, "Reopened by stop-gate") {
		t.Errorf("audit comment should explain the reopen, got: %q", txt)
	}
}

// TestReopenOrphanedClaimedTask_ListAssignedTaskErrorLogs verifies gd-k3kzZeIlT0
// item 2: a ListAssignedTask failure (as opposed to a clean "no claim" empty
// result) is logged rather than silently treated the same as no-claim. Before
// this fix the two cases were indistinguishable after the fact — a genuinely
// claimed task that gb failed to look up here left no trace of why the
// involuntary-death reopen never fired, only the stale-reaper minutes later.
func TestReopenOrphanedClaimedTask_ListAssignedTaskErrorLogs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/beads" {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)
	withActor(t, "gardenio-crew-abcd")
	clearS3ResumeEnv(t)
	withFastReopenRetries(t)

	var got string
	var lookupFailed bool
	logs := captureLogOutput(t, func() {
		got, lookupFailed = reopenOrphanedClaimedTask(context.Background(), "cooldown")
	})

	if got != "" {
		t.Errorf("reopenOrphanedClaimedTask on ListAssignedTask error = %q, want empty", got)
	}
	if !lookupFailed {
		t.Errorf("expected lookupFailed=true when ListAssignedTask errors out, so the caller does not mistake this for a verified no-claim")
	}
	if !strings.Contains(logs, "ListAssignedTask") || !strings.Contains(logs, "gardenio-crew-abcd") {
		t.Errorf("expected a logged warning naming ListAssignedTask and the actor, got: %q", logs)
	}
}

// TestReopenOrphanedClaimedTask_RetriesThenSucceeds verifies the retry loop:
// a ListAssignedTask call that fails on its first attempts but succeeds
// within reopenLookupMaxAttempts must still reopen the task and report
// lookupFailed=false — a transient blip during SIGTERM teardown must not be
// conflated with a genuine lookup failure.
func TestReopenOrphanedClaimedTask_RetriesThenSucceeds(t *testing.T) {
	mock := &reopenMockDaemon{
		assignedTask: map[string]any{
			"id":       "gd-TASK1",
			"title":    "survives a flaky lookup",
			"kind":     "issue",
			"type":     "task",
			"status":   "in_progress",
			"assignee": "gardenio-crew-abcd",
		},
	}
	mockHandler := mock.handler(t)
	var listAttempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/beads" {
			listAttempts++
			if listAttempts < 3 {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
		}
		mockHandler(w, r)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)
	withActor(t, "gardenio-crew-abcd")
	clearS3ResumeEnv(t)
	withFastReopenRetries(t)

	got, lookupFailed := reopenOrphanedClaimedTask(context.Background(), "cooldown")

	if got != "gd-TASK1" {
		t.Fatalf("reopenOrphanedClaimedTask = %q, want gd-TASK1", got)
	}
	if lookupFailed {
		t.Errorf("expected lookupFailed=false once a retry succeeds")
	}
	if listAttempts != 3 {
		t.Errorf("expected exactly 3 ListAssignedTask attempts, got %d", listAttempts)
	}
}

// TestReopenOrphanedClaimedTask_NoClaimNoop verifies that when the agent holds
// no in_progress task, nothing is mutated (a clean idle-out).
func TestReopenOrphanedClaimedTask_NoClaimNoop(t *testing.T) {
	mock := &reopenMockDaemon{assignedTask: nil}
	srv := httptest.NewServer(mock.handler(t))
	defer srv.Close()
	swapDaemon(t, srv.URL)
	withActor(t, "gardenio-crew-abcd")
	clearS3ResumeEnv(t)

	if got, lookupFailed := reopenOrphanedClaimedTask(context.Background(), "cooldown"); got != "" || lookupFailed {
		t.Errorf("reopenOrphanedClaimedTask with no claim = (%q, %v), want (\"\", false)", got, lookupFailed)
	}
	if mock.patchedID != "" {
		t.Errorf("no task should be patched when there is no claim, patched %q", mock.patchedID)
	}
}

// TestReopenOrphanedClaimedTask_IncrementsAttemptCount verifies the reopen patch
// carries reopen_attempts incremented from the value already on the task bead —
// the running total that survives respawns (ka-0000002985 DEFECT 3). A task
// reopened twice before must go out with reopen_attempts=3.
func TestReopenOrphanedClaimedTask_IncrementsAttemptCount(t *testing.T) {
	mock := &reopenMockDaemon{
		assignedTask: map[string]any{
			"id":       "gd-TASK1",
			"title":    "loops forever",
			"kind":     "issue",
			"type":     "task",
			"status":   "in_progress",
			"assignee": "gardenio-crew-abcd",
			"fields":   map[string]any{beadsapi.ReopenAttemptsField: "2"},
		},
	}
	srv := httptest.NewServer(mock.handler(t))
	defer srv.Close()
	swapDaemon(t, srv.URL)
	withActor(t, "gardenio-crew-abcd")
	clearS3ResumeEnv(t)

	if got, _ := reopenOrphanedClaimedTask(context.Background(), "cooldown"); got != "gd-TASK1" {
		t.Fatalf("reopenOrphanedClaimedTask = %q, want gd-TASK1", got)
	}
	if s, _ := mock.patchBody["status"].(string); s != "open" {
		t.Errorf("expected status=open (still under cap), got %v", mock.patchBody["status"])
	}
	if f := mock.patchFields(); f == nil || f[beadsapi.ReopenAttemptsField] != "3" {
		t.Errorf("expected reopen_attempts=3 in patch, got %v", mock.patchFields())
	}
	if f := mock.patchFields(); f[beadsapi.ReapedAssigneeField] != "gardenio-crew-abcd" {
		t.Errorf("expected %s=gardenio-crew-abcd in patch, got %v", beadsapi.ReapedAssigneeField, f)
	}
}

// TestReopenOrphanedClaimedTask_SkipsAwaitsRelease: a claim with
// awaits_release=true is done and its release closes it, so the stop-gate
// leaves it in_progress; a plain claim held alongside it is still reopened
// (gd-aaGqLb2cUk).
func TestReopenOrphanedClaimedTask_SkipsAwaitsRelease(t *testing.T) {
	shipped := map[string]any{
		"id":       "gd-SHIPPED",
		"kind":     "issue",
		"type":     "task",
		"status":   "in_progress",
		"assignee": "gardenio-crew-abcd",
		"fields":   map[string]any{beadsapi.AwaitsReleaseField: "true"},
	}
	t.Run("alone", func(t *testing.T) {
		mock := &reopenMockDaemon{assignedTask: shipped}
		srv := httptest.NewServer(mock.handler(t))
		defer srv.Close()
		swapDaemon(t, srv.URL)
		withActor(t, "gardenio-crew-abcd")
		clearS3ResumeEnv(t)

		got, lookupFailed := reopenOrphanedClaimedTask(context.Background(), "cooldown")
		if got != "" || lookupFailed {
			t.Fatalf("reopenOrphanedClaimedTask = (%q, %v), want (\"\", false)", got, lookupFailed)
		}
		if mock.patchedID != "" {
			t.Errorf("awaits_release task must not be patched, got PATCH on %q", mock.patchedID)
		}
	})
	t.Run("with a plain claim", func(t *testing.T) {
		mock := &reopenMockDaemon{
			leadingTasks: []map[string]any{shipped},
			assignedTask: map[string]any{
				"id":       "gd-TASK1",
				"kind":     "issue",
				"type":     "task",
				"status":   "in_progress",
				"assignee": "gardenio-crew-abcd",
			},
		}
		srv := httptest.NewServer(mock.handler(t))
		defer srv.Close()
		swapDaemon(t, srv.URL)
		withActor(t, "gardenio-crew-abcd")
		clearS3ResumeEnv(t)

		if got, _ := reopenOrphanedClaimedTask(context.Background(), "cooldown"); got != "gd-TASK1" {
			t.Fatalf("reopenOrphanedClaimedTask = %q, want gd-TASK1", got)
		}
		if mock.patchedID != "gd-TASK1" {
			t.Errorf("expected PATCH on gd-TASK1, got %q", mock.patchedID)
		}
	})
}

// TestReopenOrphanedClaimedTask_ParksAtCap verifies the DEFECT 3 safety valve:
// once a task has been reopened MaxReopenAttempts times, the next involuntary
// death PARKS it (status=blocked, cap fields stamped, human-facing audit comment)
// instead of returning it to the ready queue to fail the same way again. This is
// what breaks the 2026-08 site-deployment loop (137 reopens of 3 items).
func TestReopenOrphanedClaimedTask_ParksAtCap(t *testing.T) {
	atCap := strconv.Itoa(beadsapi.MaxReopenAttempts) // next attempt would exceed the cap
	mock := &reopenMockDaemon{
		assignedTask: map[string]any{
			"id":       "gd-TASK1",
			"title":    "un-runnable work",
			"kind":     "issue",
			"type":     "task",
			"status":   "in_progress",
			"assignee": "gardenio-crew-abcd",
			"fields":   map[string]any{beadsapi.ReopenAttemptsField: atCap},
		},
	}
	srv := httptest.NewServer(mock.handler(t))
	defer srv.Close()
	swapDaemon(t, srv.URL)
	withActor(t, "gardenio-crew-abcd")
	clearS3ResumeEnv(t)

	// Capped => the task is NOT returned to the queue: reopen returns "".
	if got, _ := reopenOrphanedClaimedTask(context.Background(), "unauthorized: authentication_failed"); got != "" {
		t.Fatalf("reopenOrphanedClaimedTask at cap = %q, want empty (parked, not reopened)", got)
	}
	if mock.patchedID != "gd-TASK1" {
		t.Fatalf("expected PATCH on gd-TASK1, got %q", mock.patchedID)
	}
	if s, _ := mock.patchBody["status"].(string); s != "blocked" {
		t.Errorf("expected status=blocked (parked), got %v", mock.patchBody["status"])
	}
	if a, ok := mock.patchBody["assignee"]; !ok || a != "" {
		t.Errorf("expected assignee cleared on park, got %v (present=%v)", a, ok)
	}
	f := mock.patchFields()
	if f == nil {
		t.Fatal("expected cap fields in park patch, got none")
	}
	if f[beadsapi.ReapedAssigneeField] != "gardenio-crew-abcd" {
		t.Errorf("expected %s=gardenio-crew-abcd on park, got %v", beadsapi.ReapedAssigneeField, f[beadsapi.ReapedAssigneeField])
	}
	if f[beadsapi.ReopenCappedField] != "true" {
		t.Errorf("expected reopen_capped=true, got %v", f[beadsapi.ReopenCappedField])
	}
	if got := f[beadsapi.ReopenAttemptsField]; got != strconv.Itoa(beadsapi.MaxReopenAttempts+1) {
		t.Errorf("expected reopen_attempts=%d, got %v", beadsapi.MaxReopenAttempts+1, got)
	}
	if r, _ := f[beadsapi.ReopenCapReasonField].(string); !strings.Contains(r, "authentication_failed") {
		t.Errorf("expected cap reason to record the death reason, got %v", f[beadsapi.ReopenCapReasonField])
	}
	// The human-facing audit comment (auto-forwarded to Slack) must explain the stop.
	if txt, _ := mock.commentBody["text"].(string); !strings.Contains(txt, "Reopen cap reached") {
		t.Errorf("park comment should explain the cap, got: %q", txt)
	}
}

// TestMaxReopenAttempts_EnvOverride verifies GB_MAX_REOPEN_ATTEMPTS overrides the
// default cap, and that a non-positive/malformed value falls back to the default
// (the cap is a safety valve and must not be silently disabled by a bad value).
func TestMaxReopenAttempts_EnvOverride(t *testing.T) {
	cases := []struct {
		val  string
		want int
	}{
		{"", beadsapi.MaxReopenAttempts},
		{"3", 3},
		{"0", beadsapi.MaxReopenAttempts},
		{"-5", beadsapi.MaxReopenAttempts},
		{"garbage", beadsapi.MaxReopenAttempts},
	}
	for _, c := range cases {
		t.Setenv("GB_MAX_REOPEN_ATTEMPTS", c.val)
		if got := maxReopenAttempts(); got != c.want {
			t.Errorf("maxReopenAttempts() with GB_MAX_REOPEN_ATTEMPTS=%q = %d, want %d", c.val, got, c.want)
		}
	}
}

// TestReopenOrphanedClaimedTask_UnknownActorNoop verifies the guard: an unset /
// "unknown" actor short-circuits before any daemon call.
func TestReopenOrphanedClaimedTask_UnknownActorNoop(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("daemon should not be called for an unknown actor; got %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)
	withActor(t, "unknown")

	if got, lookupFailed := reopenOrphanedClaimedTask(context.Background(), "cooldown"); got != "" || lookupFailed {
		t.Errorf("reopenOrphanedClaimedTask(unknown actor) = (%q, %v), want (\"\", false)", got, lookupFailed)
	}
}

// resetCanonicalS3Key clears the process-global canonical-key memo so an
// S3-branch test starts from the own-bead-id default, and restores it on
// cleanup. resolveCanonicalS3Key/agentS3SessionKey share this pointer.
func resetCanonicalS3Key(t *testing.T) {
	t.Helper()
	old := canonicalS3Key.Load()
	canonicalS3Key.Store(nil)
	t.Cleanup(func() { canonicalS3Key.Store(old) })
}

// TestReopenOrphanedClaimedTask_StampsResumeS3Key is the WRITER half of the
// transcript-continuity fix (ka-0000003160): when S3 is configured, the
// involuntary reopen must stamp resume_s3_key = this pod's S3 session key onto
// the task bead, IN THE SAME reopen PATCH, so the gardener-respawned successor
// (new bead id, same task_id) can follow it back to the canonical prefix
// instead of cold-starting and orphaning the transcript.
func TestReopenOrphanedClaimedTask_StampsResumeS3Key(t *testing.T) {
	mock := &reopenMockDaemon{
		assignedTask: map[string]any{
			"id":       "gd-TASK9",
			"title":    "crew work killed mid-turn",
			"kind":     "issue",
			"type":     "task",
			"status":   "in_progress",
			"assignee": "gardenio-crew-orig",
		},
		// Own bead carries no s3_session_key (never resumed) → own bead id.
		ownBeadID: "gd-ORIGAGENT",
	}
	srv := httptest.NewServer(mock.handler(t))
	defer srv.Close()
	swapDaemon(t, srv.URL)
	withActor(t, "gardenio-crew-orig")

	// Configure S3 + this pod's own bead id, and no inherited canonical key —
	// so the handoff key is the pod's OWN bead id (the original transcript
	// prefix). A fresh (non-resumed) crew that dies is the common case.
	clearS3ResumeEnv(t)
	t.Setenv("COOP_S3_BUCKET", "gardenio-sessions-prod")
	t.Setenv("KD_AGENT_ID", "gd-ORIGAGENT")
	resetCanonicalS3Key(t)

	got, _ := reopenOrphanedClaimedTask(context.Background(), "context_cancelled")
	if got != "gd-TASK9" {
		t.Fatalf("reopenOrphanedClaimedTask = %q, want gd-TASK9", got)
	}
	// The reopen and the handoff must land in ONE PATCH on the task.
	if mock.patchedID != "gd-TASK9" {
		t.Errorf("expected PATCH on gd-TASK9, got %q", mock.patchedID)
	}
	if s, _ := mock.patchBody["status"].(string); s != "open" {
		t.Errorf("expected status=open in reopen patch, got %v", mock.patchBody["status"])
	}
	fields := mock.patchFields()
	if fields == nil {
		t.Fatalf("expected a nested fields object carrying resume_s3_key, got none (body=%v)", mock.patchBody)
	}
	if key, _ := fields[resumeS3KeyField].(string); key != "gd-ORIGAGENT" {
		t.Errorf("resume_s3_key = %v, want gd-ORIGAGENT (this pod's own S3 session key)", fields[resumeS3KeyField])
	}
}

// TestReopenOrphanedClaimedTask_ResumeS3KeyFollowsInheritedCanonical verifies
// the chain property: when THIS pod is itself a resume (it adopted a canonical
// key from a prior life), the handoff key it writes is that INHERITED canonical
// key — not its own (new) bead id. This keeps a chain of N involuntary kills
// all pointing at the ONE original transcript prefix rather than a moving
// target.
func TestReopenOrphanedClaimedTask_ResumeS3KeyFollowsInheritedCanonical(t *testing.T) {
	mock := &reopenMockDaemon{
		assignedTask: map[string]any{
			"id":       "gd-TASK9",
			"title":    "crew work killed again",
			"kind":     "issue",
			"type":     "task",
			"status":   "in_progress",
			"assignee": "gardenio-crew-second",
		},
	}
	srv := httptest.NewServer(mock.handler(t))
	defer srv.Close()
	swapDaemon(t, srv.URL)
	withActor(t, "gardenio-crew-second")

	clearS3ResumeEnv(t)
	t.Setenv("COOP_S3_BUCKET", "gardenio-sessions-prod")
	t.Setenv("KD_AGENT_ID", "gd-SECONDAGENT") // this pod's own (new) bead id
	resetCanonicalS3Key(t)
	// Simulate having resumed from the ORIGINAL transcript prefix.
	canonical := "gd-ORIGAGENT"
	canonicalS3Key.Store(&canonical)

	if got, _ := reopenOrphanedClaimedTask(context.Background(), "context_cancelled"); got != "gd-TASK9" {
		t.Fatalf("reopenOrphanedClaimedTask = %q, want gd-TASK9", got)
	}
	fields := mock.patchFields()
	if fields == nil {
		t.Fatalf("expected resume_s3_key handoff, got no fields (body=%v)", mock.patchBody)
	}
	if key, _ := fields[resumeS3KeyField].(string); key != "gd-ORIGAGENT" {
		t.Errorf("resume_s3_key = %v, want gd-ORIGAGENT (inherited canonical, not own bead id)", fields[resumeS3KeyField])
	}
}

// TestReopenOrphanedClaimedTask_ResumeS3KeyFromPersistedField covers the
// hook-PROCESS path: reopenOrphanedClaimedTask also runs in the separate `gb`
// Claude-Stop-hook subprocess, where resolveCanonicalS3Key never ran so the
// in-memory canonicalS3Key memo is nil. A RESUMED pod dying via that path must
// STILL hand off the canonical key — read back from the persisted
// s3_session_key field resolveCanonicalS3Key stamped on the own bead at boot —
// not its own (new) bead id. Otherwise the successor resumes an empty prefix.
func TestReopenOrphanedClaimedTask_ResumeS3KeyFromPersistedField(t *testing.T) {
	mock := &reopenMockDaemon{
		assignedTask: map[string]any{
			"id":       "gd-TASK9",
			"title":    "resumed crew killed via hook",
			"kind":     "issue",
			"type":     "task",
			"status":   "in_progress",
			"assignee": "gardenio-crew-second",
		},
		// This pod resumed: boot stamped s3_session_key=<original> on its bead.
		ownBeadID:     "gd-SECONDAGENT",
		ownBeadFields: map[string]any{"s3_session_key": "gd-ORIGAGENT"},
	}
	srv := httptest.NewServer(mock.handler(t))
	defer srv.Close()
	swapDaemon(t, srv.URL)
	withActor(t, "gardenio-crew-second")

	clearS3ResumeEnv(t)
	t.Setenv("COOP_S3_BUCKET", "gardenio-sessions-prod")
	t.Setenv("KD_AGENT_ID", "gd-SECONDAGENT")
	resetCanonicalS3Key(t) // memo nil — the hook process never populated it

	if got, _ := reopenOrphanedClaimedTask(context.Background(), "context_cancelled"); got != "gd-TASK9" {
		t.Fatalf("reopenOrphanedClaimedTask = %q, want gd-TASK9", got)
	}
	fields := mock.patchFields()
	if fields == nil {
		t.Fatalf("expected resume_s3_key handoff, got no fields (body=%v)", mock.patchBody)
	}
	if key, _ := fields[resumeS3KeyField].(string); key != "gd-ORIGAGENT" {
		t.Errorf("resume_s3_key = %v, want gd-ORIGAGENT (persisted s3_session_key, not own bead id)", fields[resumeS3KeyField])
	}
}

// TestSynthesizeFallbackWrapUp_InvoluntaryNoToolCalls verifies item 3a for the
// race where the tool-call counter has not flushed yet (toolCalls==0) but the
// close is marked involuntary because a claimed task had to be reopened: the
// synthesized tldr must NOT read "idled out".
func TestSynthesizeFallbackWrapUp_InvoluntaryNoToolCalls(t *testing.T) {
	bead := &beadsapi.BeadDetail{
		Title:  "crew identity",
		Fields: map[string]string{"agent": "gardenio-crew-abcd"},
		// total_tool_calls intentionally absent → beadToolCallCount == 0.
	}
	out := synthesizeFallbackWrapUp("cooldown",
		"stop_gate_cooldown_involuntary: claude exited mid-work without gb done; reopened gd-TASK1", bead)
	var wu WrapUp
	if err := json.Unmarshal([]byte(out), &wu); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if strings.Contains(wu.TLDR, "idled out") {
		t.Errorf("involuntary close must not claim idled out, got: %q", wu.TLDR)
	}
	if !strings.Contains(strings.ToLower(wu.TLDR), "involuntar") {
		t.Errorf("involuntary close tldr should say so, got: %q", wu.TLDR)
	}
	if !strings.Contains(wu.TLDR, "gardenio-crew-abcd") {
		t.Errorf("tldr should still carry the agent identity, got: %q", wu.TLDR)
	}
}
