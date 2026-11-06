package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/workflowhost"

	"github.com/spf13/cobra"
)

// wfDaemonStub is a beads daemon stub that MERGES PATCHed fields into the
// stored bead, so read-after-write (lease claim verification, rehydrate state
// reads) behaves like the real server. The shared agentDaemonStub does not
// merge, so workflow re-entrancy tests need this.
type wfDaemonStub struct {
	mu     sync.Mutex
	fields map[string]map[string]string // beadID -> fields
}

func newWFStub() *wfDaemonStub {
	return &wfDaemonStub{fields: map[string]map[string]string{}}
}

func (s *wfDaemonStub) setBead(id string, fields map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := map[string]string{}
	maps.Copy(cp, fields)
	s.fields[id] = cp
}

func (s *wfDaemonStub) get(id string) map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := map[string]string{}
	maps.Copy(cp, s.fields[id])
	return cp
}

func (s *wfDaemonStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1/beads/"):
			id := strings.TrimPrefix(r.URL.Path, "/v1/beads/")
			s.mu.Lock()
			f, ok := s.fields[id]
			s.mu.Unlock()
			if !ok {
				http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{
				"id": id, "title": id, "type": "task", "status": "in_progress",
				"fields": f,
			})

		case r.Method == "PATCH" && strings.HasPrefix(r.URL.Path, "/v1/beads/"):
			id := strings.TrimPrefix(r.URL.Path, "/v1/beads/")
			body, _ := io.ReadAll(r.Body)
			var patch struct {
				Fields map[string]any `json:"fields"`
			}
			json.Unmarshal(body, &patch)
			s.mu.Lock()
			if s.fields[id] == nil {
				s.fields[id] = map[string]string{}
			}
			for k, v := range patch.Fields {
				switch vv := v.(type) {
				case string:
					s.fields[id][k] = vv
				default:
					b, _ := json.Marshal(vv)
					s.fields[id][k] = string(b)
				}
			}
			s.mu.Unlock()
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{"fields": patch.Fields})

		default:
			http.NotFound(w, r)
		}
	})
}

func setupWFDaemon(t *testing.T, stub *wfDaemonStub) {
	t.Helper()
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	old := daemon
	daemon = c
	t.Cleanup(func() { daemon = old })
}

// wfCmd returns a cobra command with a background context and a captured stdout
// buffer for assertions.
func wfCmd() (*cobra.Command, *bytes.Buffer) {
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	return cmd, buf
}

// resetWFFlags restores the package-level workflow flag globals to defaults
// between tests (they are shared cobra flag targets).
func resetWFFlags(t *testing.T) {
	t.Helper()
	oldRun, oldScript, oldS3, oldPod, oldTTL, oldForce, oldJSON, oldRestore :=
		wfRunID, wfScriptPath, wfS3URI, wfPodID, wfLeaseTTL, wfForce, jsonOutput, journalRestore
	wfRunID, wfScriptPath, wfS3URI, wfPodID = "", "", "", ""
	wfLeaseTTL = workflowhost.DefaultLeaseTTL
	wfForce = false
	jsonOutput = false
	t.Cleanup(func() {
		wfRunID, wfScriptPath, wfS3URI, wfPodID, wfLeaseTTL, wfForce, jsonOutput, journalRestore =
			oldRun, oldScript, oldS3, oldPod, oldTTL, oldForce, oldJSON, oldRestore
	})
}

func TestWorkflowPersist_RunIDFirst(t *testing.T) {
	resetWFFlags(t)
	stub := newWFStub()
	stub.setBead("gd-step", map[string]string{"workflow_script_path": "wf.js"})
	setupWFDaemon(t, stub)

	wfRunID = "wf_abc"
	wfS3URI = "s3://bucket/wf_abc/"
	cmd, _ := wfCmd()
	if err := runWorkflowPersist(cmd, []string{"gd-step"}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	f := stub.get("gd-step")
	if f["workflow_run_id"] != "wf_abc" {
		t.Errorf("workflow_run_id = %q, want wf_abc", f["workflow_run_id"])
	}
	if f["workflow_s3_uri"] != "s3://bucket/wf_abc/" {
		t.Errorf("workflow_s3_uri = %q", f["workflow_s3_uri"])
	}
	// script_path set earlier must survive the merge.
	if f["workflow_script_path"] != "wf.js" {
		t.Errorf("workflow_script_path lost: %q", f["workflow_script_path"])
	}
}

func TestWorkflowPersist_Empty(t *testing.T) {
	resetWFFlags(t)
	stub := newWFStub()
	stub.setBead("gd-step", map[string]string{})
	setupWFDaemon(t, stub)

	cmd, _ := wfCmd()
	if err := runWorkflowPersist(cmd, []string{"gd-step"}); err == nil {
		t.Fatal("expected error when no fields supplied")
	}
}

// TestWorkflowRehydrate_RestartResumePath is the headline restart case:
// the step bead carries run_id + script_path + s3_uri and no lease. Rehydrate
// must (a) acquire the lease, (b) restore the journal from s3 BEFORE resume,
// and (c) emit a resumable plan naming the runId.
func TestWorkflowRehydrate_RestartResumePath(t *testing.T) {
	resetWFFlags(t)
	stub := newWFStub()
	stub.setBead("gd-step", map[string]string{
		"workflow_run_id":      "wf_9",
		"workflow_script_path": "wf.js",
		"workflow_s3_uri":      "s3://bucket/wf_9/",
	})
	setupWFDaemon(t, stub)

	// Record restore ordering vs the emitted resume plan.
	var restoredURI string
	restoreCalled := false
	journalRestore = func(ctx context.Context, s3URI string) error {
		restoreCalled = true
		restoredURI = s3URI
		return nil
	}

	wfPodID = "pod-b"
	jsonOutput = true
	cmd, buf := wfCmd()
	if err := runWorkflowRehydrate(cmd, []string{"gd-step"}); err != nil {
		t.Fatalf("rehydrate: %v", err)
	}

	if !restoreCalled {
		t.Fatal("journal restore was not called on the resume path")
	}
	if restoredURI != "s3://bucket/wf_9/" {
		t.Errorf("restored uri = %q, want s3://bucket/wf_9/", restoredURI)
	}

	var res rehydrateResult
	if err := json.Unmarshal(buf.Bytes(), &res); err != nil {
		t.Fatalf("decoding result: %v\noutput: %s", err, buf.String())
	}
	if res.Mode != "resume" || !res.Resumable {
		t.Errorf("expected resumable resume mode, got %+v", res)
	}
	if res.RunID != "wf_9" {
		t.Errorf("RunID = %q, want wf_9", res.RunID)
	}
	if !res.JournalRestored {
		t.Error("expected JournalRestored=true")
	}

	// The lease must now be held by pod-b.
	lease, _ := workflowhost.ParseLease(stub.get("gd-step")["workflow_lease"])
	if lease == nil || lease.PodID != "pod-b" {
		t.Errorf("expected lease held by pod-b, got %+v", lease)
	}
}

// TestWorkflowRehydrate_LeasePreventsDoubleDispatch: a live lease from pod-a
// must make pod-b refuse — no journal restore, no resume plan, non-nil error.
func TestWorkflowRehydrate_LeasePreventsDoubleDispatch(t *testing.T) {
	resetWFFlags(t)
	stub := newWFStub()
	liveLease, _ := workflowhost.MarshalLease(
		workflowhost.NewLease("pod-a", "wf_9", time.Now().UTC().Add(-1*time.Minute)))
	stub.setBead("gd-step", map[string]string{
		"workflow_run_id":      "wf_9",
		"workflow_script_path": "wf.js",
		"workflow_s3_uri":      "s3://bucket/wf_9/",
		"workflow_lease":       liveLease,
	})
	setupWFDaemon(t, stub)

	restoreCalled := false
	journalRestore = func(ctx context.Context, s3URI string) error {
		restoreCalled = true
		return nil
	}

	wfPodID = "pod-b"
	cmd, _ := wfCmd()
	err := runWorkflowRehydrate(cmd, []string{"gd-step"})
	if err == nil {
		t.Fatal("expected rehydrate to be refused while pod-a holds a live lease")
	}
	if !strings.Contains(err.Error(), "refused") {
		t.Errorf("error = %q, want it to mention refusal", err.Error())
	}
	if restoreCalled {
		t.Error("journal restore must NOT run when the lease is refused")
	}
	// pod-a's lease must be untouched.
	lease, _ := workflowhost.ParseLease(stub.get("gd-step")["workflow_lease"])
	if lease == nil || lease.PodID != "pod-a" {
		t.Errorf("pod-a lease was overwritten: %+v", lease)
	}
}

func TestWorkflowRehydrate_StaleLeaseTakeover(t *testing.T) {
	resetWFFlags(t)
	stub := newWFStub()
	staleLease, _ := workflowhost.MarshalLease(
		workflowhost.NewLease("pod-a", "wf_9", time.Now().UTC().Add(-2*time.Hour)))
	stub.setBead("gd-step", map[string]string{
		"workflow_run_id":      "wf_9",
		"workflow_script_path": "wf.js",
		"workflow_lease":       staleLease,
	})
	setupWFDaemon(t, stub)
	journalRestore = func(ctx context.Context, s3URI string) error { return nil }

	wfPodID = "pod-b"
	cmd, _ := wfCmd()
	if err := runWorkflowRehydrate(cmd, []string{"gd-step"}); err != nil {
		t.Fatalf("expected stale-lease takeover to succeed: %v", err)
	}
	lease, _ := workflowhost.ParseLease(stub.get("gd-step")["workflow_lease"])
	if lease == nil || lease.PodID != "pod-b" {
		t.Errorf("expected pod-b to take over stale lease, got %+v", lease)
	}
}

func TestWorkflowRehydrate_ColdStartNoRunID(t *testing.T) {
	resetWFFlags(t)
	stub := newWFStub()
	stub.setBead("gd-step", map[string]string{"workflow_script_path": "wf.js"})
	setupWFDaemon(t, stub)

	restoreCalled := false
	journalRestore = func(ctx context.Context, s3URI string) error {
		restoreCalled = true
		return nil
	}
	jsonOutput = true
	wfPodID = "pod-b"
	cmd, buf := wfCmd()
	if err := runWorkflowRehydrate(cmd, []string{"gd-step"}); err != nil {
		t.Fatalf("cold start rehydrate: %v", err)
	}
	if restoreCalled {
		t.Error("cold start must not restore a journal")
	}
	var res rehydrateResult
	json.Unmarshal(buf.Bytes(), &res)
	if res.Mode != "cold-start" || res.Resumable {
		t.Errorf("expected cold-start, got %+v", res)
	}
	// Lease is still claimed even on cold start.
	lease, _ := workflowhost.ParseLease(stub.get("gd-step")["workflow_lease"])
	if lease == nil || lease.PodID != "pod-b" {
		t.Errorf("expected lease claimed on cold start, got %+v", lease)
	}
}

func TestWorkflowLease_AcquireThenRefuseOther(t *testing.T) {
	resetWFFlags(t)
	stub := newWFStub()
	stub.setBead("gd-step", map[string]string{"workflow_run_id": "wf_9"})
	setupWFDaemon(t, stub)

	// pod-a acquires.
	wfPodID = "pod-a"
	cmd, _ := wfCmd()
	if err := runWorkflowLeaseAcquire(cmd, []string{"gd-step"}); err != nil {
		t.Fatalf("pod-a acquire: %v", err)
	}
	// pod-b is refused.
	wfPodID = "pod-b"
	cmd2, _ := wfCmd()
	if err := runWorkflowLeaseAcquire(cmd2, []string{"gd-step"}); err == nil {
		t.Fatal("expected pod-b to be refused while pod-a holds the lease")
	}
}

func TestWorkflowLease_Release(t *testing.T) {
	resetWFFlags(t)
	stub := newWFStub()
	held, _ := workflowhost.MarshalLease(workflowhost.NewLease("pod-a", "wf_9", time.Now().UTC()))
	stub.setBead("gd-step", map[string]string{"workflow_lease": held})
	setupWFDaemon(t, stub)

	// pod-b cannot release without --force.
	wfPodID = "pod-b"
	cmd, _ := wfCmd()
	if err := runWorkflowLeaseRelease(cmd, []string{"gd-step"}); err == nil {
		t.Fatal("expected release to be refused for non-holder")
	}
	// holder releases.
	wfPodID = "pod-a"
	cmd2, _ := wfCmd()
	if err := runWorkflowLeaseRelease(cmd2, []string{"gd-step"}); err != nil {
		t.Fatalf("holder release: %v", err)
	}
	// After release the field holds a tombstone that ParseLease reads as "no lease".
	lease, _ := workflowhost.ParseLease(stub.get("gd-step")["workflow_lease"])
	if lease != nil {
		t.Errorf("lease not cleared, got %+v", lease)
	}
}
