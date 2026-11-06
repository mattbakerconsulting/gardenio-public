package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func TestFetchLastSessionID_ReturnsSavedID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" && r.URL.Path == "/v1/beads/gd-agent-1" {
			json.NewEncoder(w).Encode(map[string]any{
				"id":     "gd-agent-1",
				"title":  "test agent",
				"status": "open",
				"type":   "agent",
				"fields": map[string]string{
					"last_session_id": "sess-abc-123",
				},
			})
			return
		}
		http.Error(w, "not found", 404)
	}))
	defer srv.Close()

	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	defer func() { daemon = oldDaemon }()

	t.Setenv("KD_AGENT_ID", "gd-agent-1")

	sid := fetchLastSessionID(context.Background())
	if sid != "sess-abc-123" {
		t.Errorf("fetchLastSessionID() = %q, want %q", sid, "sess-abc-123")
	}
}

func TestFetchLastSessionID_NoField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":     "gd-agent-2",
			"title":  "test agent",
			"status": "open",
			"type":   "agent",
			"fields": map[string]string{},
		})
	}))
	defer srv.Close()

	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	defer func() { daemon = oldDaemon }()

	t.Setenv("KD_AGENT_ID", "gd-agent-2")

	sid := fetchLastSessionID(context.Background())
	if sid != "" {
		t.Errorf("fetchLastSessionID() = %q, want empty", sid)
	}
}

func TestFetchLastSessionID_NoDaemon(t *testing.T) {
	oldDaemon := daemon
	daemon = nil
	defer func() { daemon = oldDaemon }()

	t.Setenv("KD_AGENT_ID", "gd-agent-3")

	sid := fetchLastSessionID(context.Background())
	if sid != "" {
		t.Errorf("fetchLastSessionID() = %q, want empty when daemon is nil", sid)
	}
}

func TestFetchLastSessionID_NoAgentBeadID(t *testing.T) {
	t.Setenv("KD_AGENT_ID", "")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")

	sid := fetchLastSessionID(context.Background())
	if sid != "" {
		t.Errorf("fetchLastSessionID() = %q, want empty when no agent ID", sid)
	}
}

func TestStampLastSessionID_WritesField(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// The write leg — capture the field-update request so we can assert the
		// session UUID is stamped onto the right bead.
		if r.Method == http.MethodPatch || r.Method == http.MethodPut || r.Method == http.MethodPost {
			gotPath = r.URL.Path
			buf := make([]byte, r.ContentLength)
			_, _ = r.Body.Read(buf)
			gotBody = string(buf)
			json.NewEncoder(w).Encode(map[string]any{"id": "gd-agent-1", "type": "agent", "status": "open"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"id": "gd-agent-1", "title": "test agent", "status": "open", "type": "agent",
			"fields": map[string]string{},
		})
	}))
	defer srv.Close()

	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	defer func() { daemon = oldDaemon }()

	t.Setenv("KD_AGENT_ID", "gd-agent-1")

	stampLastSessionID(context.Background(), "11111111-2222-3333-4444-555555555555")

	if gotPath == "" {
		t.Fatalf("stampLastSessionID made no write request")
	}
	if !contains(gotBody, "last_session_id") || !contains(gotBody, "11111111-2222-3333-4444-555555555555") {
		t.Errorf("write body %q did not carry last_session_id=<uuid>", gotBody)
	}
}

func TestStampLastSessionID_NoOpOnEmptyOrNil(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		json.NewEncoder(w).Encode(map[string]any{"id": "x", "type": "agent", "status": "open"})
	}))
	defer srv.Close()

	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	defer func() { daemon = oldDaemon }()

	// Empty session id → no write.
	t.Setenv("KD_AGENT_ID", "gd-agent-1")
	stampLastSessionID(context.Background(), "")
	if called {
		t.Errorf("stampLastSessionID wrote for an empty session id")
	}

	// No agent bead in scope → no write.
	called = false
	t.Setenv("KD_AGENT_ID", "")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	stampLastSessionID(context.Background(), "11111111-2222-3333-4444-555555555555")
	if called {
		t.Errorf("stampLastSessionID wrote with no agent bead id in scope")
	}

	// Nil daemon → no panic, no write.
	daemon = nil
	t.Setenv("KD_AGENT_ID", "gd-agent-1")
	stampLastSessionID(context.Background(), "11111111-2222-3333-4444-555555555555")
}

func TestAgentS3SessionKey(t *testing.T) {
	// These subtests assert the own-bead-id fallback, so the canonical memo
	// must be clear (a prior test may have set it — it is process-global).
	resetCanonicalS3Key(t)

	t.Run("KD_AGENT_ID wins", func(t *testing.T) {
		t.Setenv("KD_AGENT_ID", "gd-agent-primary")
		t.Setenv("BOAT_AGENT_BEAD_ID", "gd-agent-fallback")
		if got := agentS3SessionKey(); got != "gd-agent-primary" {
			t.Errorf("agentS3SessionKey() = %q, want %q", got, "gd-agent-primary")
		}
	})

	t.Run("BOAT_AGENT_BEAD_ID fallback", func(t *testing.T) {
		t.Setenv("KD_AGENT_ID", "")
		t.Setenv("BOAT_AGENT_BEAD_ID", "gd-agent-fallback")
		if got := agentS3SessionKey(); got != "gd-agent-fallback" {
			t.Errorf("agentS3SessionKey() = %q, want %q", got, "gd-agent-fallback")
		}
	})

	t.Run("empty when neither set", func(t *testing.T) {
		t.Setenv("KD_AGENT_ID", "")
		t.Setenv("BOAT_AGENT_BEAD_ID", "")
		if got := agentS3SessionKey(); got != "" {
			t.Errorf("agentS3SessionKey() = %q, want empty", got)
		}
	})

	t.Run("canonical memo overrides own bead id", func(t *testing.T) {
		t.Setenv("KD_AGENT_ID", "gd-new-bead")
		canonical := "gd-original-bead"
		canonicalS3Key.Store(&canonical)
		t.Cleanup(func() { canonicalS3Key.Store(nil) })
		if got := agentS3SessionKey(); got != "gd-original-bead" {
			t.Errorf("agentS3SessionKey() = %q, want gd-original-bead (canonical wins over own bead id)", got)
		}
	})
}

// s3RestoreReaderMock is a tiny daemon serving the GET task bead +
// UpdateBeadFields (GET+PATCH) calls resolveCanonicalS3Key makes.
type s3RestoreReaderMock struct {
	taskFields map[string]string // fields returned for GET /v1/beads/<taskID>
	taskID     string

	// captured PATCHes keyed by bead id → merged fields
	patched map[string]map[string]any
}

func (m *s3RestoreReaderMock) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	if m.patched == nil {
		m.patched = map[string]map[string]any{}
	}
	return func(w http.ResponseWriter, r *http.Request) {
		id := ""
		if len(r.URL.Path) > len("/v1/beads/") {
			id = r.URL.Path[len("/v1/beads/"):]
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			fields := map[string]string{}
			if id == m.taskID && m.taskFields != nil {
				fields = m.taskFields
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": id, "type": "task", "status": "open", "fields": fields,
			})
		case http.MethodPatch:
			var body map[string]any
			buf := make([]byte, r.ContentLength)
			_, _ = r.Body.Read(buf)
			_ = json.Unmarshal(buf, &body)
			if f, ok := body["fields"].(map[string]any); ok {
				m.patched[id] = f
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "type": "task", "status": "open"})
		default:
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	}
}

// TestResolveCanonicalS3Key_AdoptsPriorKeyFromTask is the READER half of the
// transcript-continuity fix (ka-0000003160): a respawned crew pod (new bead id)
// must follow the task bead's resume_s3_key back to the canonical prefix,
// memoize it for agentS3SessionKey (→ COOP_S3_SESSION_KEY), stamp s3_session_key
// on its OWN bead so a lookup by the new bead id resolves, and consume the
// breadcrumb so a later clean completion cannot resurrect a finished transcript.
func TestResolveCanonicalS3Key_AdoptsPriorKeyFromTask(t *testing.T) {
	mock := &s3RestoreReaderMock{
		taskID:     "gd-TASKX",
		taskFields: map[string]string{"resume_s3_key": "gd-ORIGAGENT"},
	}
	srv := httptest.NewServer(mock.handler(t))
	defer srv.Close()

	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	defer func() { daemon = oldDaemon }()

	t.Setenv("COOP_S3_BUCKET", "gardenio-sessions-prod")
	t.Setenv("BOAT_TASK_ID", "gd-TASKX")
	t.Setenv("KD_AGENT_ID", "gd-NEWAGENT") // this pod's fresh (new) bead id
	t.Setenv("BOAT_AGENT_BEAD_ID", "gd-NEWAGENT")
	resetCanonicalS3Key(t)

	resolveCanonicalS3Key(context.Background())

	// coop's S3 key is now the canonical prefix, not the new bead id.
	if got := agentS3SessionKey(); got != "gd-ORIGAGENT" {
		t.Errorf("agentS3SessionKey() = %q, want gd-ORIGAGENT after adoption", got)
	}
	// s3_session_key stamped on the OWN (new) bead so viewer/forensics lookup
	// keyed on the new id resolves to the canonical prefix.
	if own := mock.patched["gd-NEWAGENT"]; own == nil || own["s3_session_key"] != "gd-ORIGAGENT" {
		t.Errorf("expected s3_session_key=gd-ORIGAGENT stamped on own bead, got %v", own)
	}
	// resume_s3_key breadcrumb consumed (cleared to "") on the task bead.
	if task := mock.patched["gd-TASKX"]; task == nil {
		t.Errorf("expected resume_s3_key cleared on task bead, got no PATCH")
	} else if v, ok := task["resume_s3_key"]; !ok || v != "" {
		t.Errorf("expected resume_s3_key cleared to \"\" on task, got %v (present=%v)", v, ok)
	}
}

// TestResolveCanonicalS3Key_NoOpWhenNoPriorKey verifies a fresh spawn (task has
// no resume_s3_key) leaves the pod on its own-bead-id key and writes nothing.
func TestResolveCanonicalS3Key_NoOpWhenNoPriorKey(t *testing.T) {
	mock := &s3RestoreReaderMock{taskID: "gd-TASKY", taskFields: map[string]string{}}
	srv := httptest.NewServer(mock.handler(t))
	defer srv.Close()

	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	defer func() { daemon = oldDaemon }()

	t.Setenv("COOP_S3_BUCKET", "gardenio-sessions-prod")
	t.Setenv("BOAT_TASK_ID", "gd-TASKY")
	t.Setenv("KD_AGENT_ID", "gd-H9NY4RPMXB")
	t.Setenv("BOAT_AGENT_BEAD_ID", "gd-H9NY4RPMXB")
	resetCanonicalS3Key(t)

	resolveCanonicalS3Key(context.Background())

	if got := agentS3SessionKey(); got != "gd-H9NY4RPMXB" {
		t.Errorf("agentS3SessionKey() = %q, want gd-H9NY4RPMXB (unchanged, no prior key)", got)
	}
	if len(mock.patched) != 0 {
		t.Errorf("expected no PATCH on a fresh spawn, got %v", mock.patched)
	}
}

// TestResolveCanonicalS3Key_NoOpWhenKeyEqualsOwn verifies the idempotent case:
// a task whose resume_s3_key already equals this pod's own bead id (already
// canonical) is a no-op — no adoption, no writes.
func TestResolveCanonicalS3Key_NoOpWhenKeyEqualsOwn(t *testing.T) {
	mock := &s3RestoreReaderMock{
		taskID:     "gd-TASKZ",
		taskFields: map[string]string{"resume_s3_key": "gd-SAMEAGENT"},
	}
	srv := httptest.NewServer(mock.handler(t))
	defer srv.Close()

	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	defer func() { daemon = oldDaemon }()

	t.Setenv("COOP_S3_BUCKET", "gardenio-sessions-prod")
	t.Setenv("BOAT_TASK_ID", "gd-TASKZ")
	t.Setenv("KD_AGENT_ID", "gd-SAMEAGENT")
	t.Setenv("BOAT_AGENT_BEAD_ID", "gd-SAMEAGENT")
	resetCanonicalS3Key(t)

	resolveCanonicalS3Key(context.Background())

	if got := agentS3SessionKey(); got != "gd-SAMEAGENT" {
		t.Errorf("agentS3SessionKey() = %q, want gd-SAMEAGENT", got)
	}
	if len(mock.patched) != 0 {
		t.Errorf("expected no PATCH when key already equals own bead id, got %v", mock.patched)
	}
}

// TestResolveCanonicalS3Key_NoOpWithoutBucketOrTask verifies the gates: no S3
// bucket, or no task in scope, short-circuits with no daemon call.
func TestResolveCanonicalS3Key_NoOpWithoutBucketOrTask(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("daemon must not be called: %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	defer func() { daemon = oldDaemon }()
	resetCanonicalS3Key(t)

	t.Run("no bucket", func(t *testing.T) {
		t.Setenv("COOP_S3_BUCKET", "")
		t.Setenv("BOAT_TASK_ID", "gd-TASK")
		resolveCanonicalS3Key(context.Background())
	})
	t.Run("no task", func(t *testing.T) {
		t.Setenv("COOP_S3_BUCKET", "gardenio-sessions-prod")
		t.Setenv("BOAT_TASK_ID", "")
		resolveCanonicalS3Key(context.Background())
	})
}
