package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// warmResumeBeadMock serves the GET (own bead) + PATCH (s3_session_key stamp)
// that maybeAdoptWarmResumeKey issues, and records the fields PATCHed onto each
// bead id.
type warmResumeBeadMock struct {
	beadID     string
	beadFields map[string]string

	mu      sync.Mutex
	patched map[string]map[string]any
}

func (m *warmResumeBeadMock) handler(t *testing.T) http.HandlerFunc {
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
			if id == m.beadID && m.beadFields != nil {
				fields = m.beadFields
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": id, "type": "agent", "status": "open", "fields": fields,
			})
		case http.MethodPatch:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if f, ok := body["fields"].(map[string]any); ok {
				m.mu.Lock()
				m.patched[id] = f
				m.mu.Unlock()
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "type": "agent", "status": "open"})
		default:
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	}
}

func (m *warmResumeBeadMock) patchedFor(id string) map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.patched[id]
}

// TestMaybeAdoptWarmResumeKey_Adopts verifies the pod side of the warm-pool
// coop-resume graft (ka-0000000983): when the assigned bead carries a
// warm_resume_s3_key and an S3 bucket is configured, the pod adopts that key as
// the canonical S3 prefix (so coop resumes the prior transcript), stamps
// s3_session_key on its own bead for chain continuity, and requests a relaunch.
func TestMaybeAdoptWarmResumeKey_Adopts(t *testing.T) {
	mock := &warmResumeBeadMock{
		beadID:     "gd-POOLPOD",
		beadFields: map[string]string{"warm_resume_s3_key": "gd-PRIOR-transcript"},
	}
	srv := httptest.NewServer(mock.handler(t))
	defer srv.Close()

	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	defer func() { daemon = oldDaemon }()

	t.Setenv("COOP_S3_BUCKET", "gardenio-sessions-prod")
	t.Setenv("KD_AGENT_ID", "gd-POOLPOD")
	t.Setenv("BOAT_AGENT_BEAD_ID", "gd-POOLPOD")
	resetCanonicalS3Key(t)

	if !maybeAdoptWarmResumeKey(context.Background(), "gd-POOLPOD") {
		t.Fatal("expected maybeAdoptWarmResumeKey to return true (relaunch requested)")
	}
	// coop's S3 key is now the prior transcript prefix, not the pool pod's own id.
	if got := agentS3SessionKey(); got != "gd-PRIOR-transcript" {
		t.Errorf("agentS3SessionKey() = %q, want gd-PRIOR-transcript after adoption", got)
	}
	// s3_session_key stamped on the pool pod's OWN bead for chain continuity.
	if own := mock.patchedFor("gd-POOLPOD"); own == nil || own["s3_session_key"] != "gd-PRIOR-transcript" {
		t.Errorf("expected s3_session_key=gd-PRIOR-transcript stamped on own bead, got %v", own)
	}
}

// TestMaybeAdoptWarmResumeKey_Idempotent verifies the second (post-relaunch)
// call is a no-op: the key is already the canonical prefix, so no relaunch is
// requested and the resumed session proceeds to injectPrompt.
func TestMaybeAdoptWarmResumeKey_Idempotent(t *testing.T) {
	mock := &warmResumeBeadMock{
		beadID:     "gd-POOLPOD2",
		beadFields: map[string]string{"warm_resume_s3_key": "gd-PRIOR-2"},
	}
	srv := httptest.NewServer(mock.handler(t))
	defer srv.Close()

	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	defer func() { daemon = oldDaemon }()

	t.Setenv("COOP_S3_BUCKET", "gardenio-sessions-prod")
	t.Setenv("KD_AGENT_ID", "gd-POOLPOD2")
	t.Setenv("BOAT_AGENT_BEAD_ID", "gd-POOLPOD2")
	resetCanonicalS3Key(t)

	if !maybeAdoptWarmResumeKey(context.Background(), "gd-POOLPOD2") {
		t.Fatal("first call should adopt + request relaunch")
	}
	if maybeAdoptWarmResumeKey(context.Background(), "gd-POOLPOD2") {
		t.Error("second call should be a no-op (key already adopted), not request another relaunch")
	}
}

// TestMaybeAdoptWarmResumeKey_NoOpWithoutKeyOrBucket verifies the gates: no
// stamped key, or no S3 bucket, short-circuits with false (fresh warm session,
// today's behavior) so the feature is inert by default.
func TestMaybeAdoptWarmResumeKey_NoOpWithoutKeyOrBucket(t *testing.T) {
	t.Run("no-key", func(t *testing.T) {
		mock := &warmResumeBeadMock{beadID: "gd-NP1", beadFields: map[string]string{}}
		srv := httptest.NewServer(mock.handler(t))
		defer srv.Close()
		oldDaemon := daemon
		daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
		defer func() { daemon = oldDaemon }()

		t.Setenv("COOP_S3_BUCKET", "gardenio-sessions-prod")
		t.Setenv("KD_AGENT_ID", "gd-NP1")
		t.Setenv("BOAT_AGENT_BEAD_ID", "gd-NP1")
		resetCanonicalS3Key(t)

		if maybeAdoptWarmResumeKey(context.Background(), "gd-NP1") {
			t.Error("expected false when no warm_resume_s3_key is stamped")
		}
	})

	t.Run("no-bucket", func(t *testing.T) {
		mock := &warmResumeBeadMock{
			beadID:     "gd-NP2",
			beadFields: map[string]string{"warm_resume_s3_key": "gd-PRIOR-x"},
		}
		srv := httptest.NewServer(mock.handler(t))
		defer srv.Close()
		oldDaemon := daemon
		daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
		defer func() { daemon = oldDaemon }()

		t.Setenv("COOP_S3_BUCKET", "") // no bucket → COOP_S3_SESSION_KEY never wired
		t.Setenv("KD_AGENT_ID", "gd-NP2")
		t.Setenv("BOAT_AGENT_BEAD_ID", "gd-NP2")
		resetCanonicalS3Key(t)

		if maybeAdoptWarmResumeKey(context.Background(), "gd-NP2") {
			t.Error("expected false when no S3 bucket is configured")
		}
	})
}
