package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	gardeniometrics "gardenio/controller/internal/metrics"
	"gardenio/controller/internal/microvm"
	"gardenio/controller/internal/podmanager"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// fakeResumer is a microvmResumer that records Resume calls and can be toggled
// available/unavailable or made to fail, so the resume endpoint can be tested
// without an AWS backend.
type fakeResumer struct {
	available      bool
	err            error
	resumed        []string // bead IDs resumed, in order
	rehydrateOn    bool     // ResumeRehydrateEnabled return
	awaited        []string // agent names AwaitRehydration was called for
	awaitDone      chan struct{}
	rehydratingSet map[string]bool // IsRehydrating truth table
}

func (f *fakeResumer) Available() bool { return f.available }

func (f *fakeResumer) Resume(_ context.Context, view microvm.AgentBeadView) error {
	if f.err != nil {
		return f.err
	}
	f.resumed = append(f.resumed, view.ID)
	return nil
}

func (f *fakeResumer) ResumeRehydrateEnabled() bool { return f.rehydrateOn }

func (f *fakeResumer) AwaitRehydration(_ context.Context, _ microvm.AgentBeadView, agentName string, _ time.Time) microvm.RehydrationOutcome {
	f.awaited = append(f.awaited, agentName)
	if f.awaitDone != nil {
		close(f.awaitDone)
	}
	return microvm.RehydrationOutcome{Released: true, Reason: "fake"}
}

func (f *fakeResumer) IsRehydrating(agentName string) bool {
	return f.rehydratingSet[agentName]
}

// fakeBeadFinder is an agentBeadFinder returning a canned bead (or an error).
type fakeBeadFinder struct {
	bead *beadsapi.BeadDetail
	err  error
}

func (f *fakeBeadFinder) FindAgentBead(_ context.Context, _ string) (*beadsapi.BeadDetail, error) {
	return f.bead, f.err
}

// suspendedMicrovmBead is the shape step 2's suspend path leaves on the agent
// bead: an OPEN bead, lambda_microvm, agent_state=stopping, with a runtime_id.
func suspendedMicrovmBead() *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID:     "gd-mvm-agent",
		Title:  "thread-1234-5678",
		Type:   "agent",
		Status: "open",
		Fields: map[string]string{
			"agent":            "thread-1234-5678",
			"execution_target": podmanager.ExecutionTargetLambdaMicroVM,
			"agent_state":      "stopping",
			"runtime_id":       "mvm-abc123",
			"runtime_state":    "SUSPENDED",
		},
	}
}

func doMicrovmResume(t *testing.T, h http.HandlerFunc, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
		"/api/v1/microvm/resume", bytes.NewReader([]byte(body)))
	rr := httptest.NewRecorder()
	h(rr, req)
	return rr
}

func decodeResumeResp(t *testing.T, rr *httptest.ResponseRecorder) microvmResumeResponse {
	t.Helper()
	var resp microvmResumeResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body=%q)", err, rr.Body.String())
	}
	return resp
}

// TestHandleMicrovmResume_ResumesSuspendedAgent: the happy path — a suspended
// MicroVM agent's VM is resumed in place and the handler reports resumed:true
// with the runtime_id, so the bridge skips its close+remint+pool path.
func TestHandleMicrovmResume_ResumesSuspendedAgent(t *testing.T) {
	res := &fakeResumer{available: true}
	finder := &fakeBeadFinder{bead: suspendedMicrovmBead()}
	h := handleMicrovmResume(res, finder, nil, discardLogger())

	rr := doMicrovmResume(t, h, `{"agent":"thread-1234-5678"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	resp := decodeResumeResp(t, rr)
	if !resp.Resumed {
		t.Fatalf("resumed = false, want true (reason=%q)", resp.Reason)
	}
	if resp.MicrovmID != "mvm-abc123" {
		t.Errorf("microvm_id = %q, want mvm-abc123", resp.MicrovmID)
	}
	if len(res.resumed) != 1 || res.resumed[0] != "gd-mvm-agent" {
		t.Errorf("Resume called with %v, want [gd-mvm-agent]", res.resumed)
	}
}

// TestHandleMicrovmResume_NotApplicable covers every routing-unchanged case: the
// handler must report resumed:false (200) so the bridge falls through to its
// normal respawn, never a 4xx/5xx it would treat as fatal.
func TestHandleMicrovmResume_NotApplicable(t *testing.T) {
	notMicrovm := suspendedMicrovmBead()
	notMicrovm.Fields["execution_target"] = "" // kubernetes agent

	notSuspended := suspendedMicrovmBead()
	notSuspended.Fields["agent_state"] = "done"

	noRuntimeID := suspendedMicrovmBead()
	noRuntimeID.Fields["runtime_id"] = ""

	cases := map[string]struct {
		res    *fakeResumer
		finder *fakeBeadFinder
		reason string
	}{
		"backend off": {
			res:    &fakeResumer{available: false},
			finder: &fakeBeadFinder{bead: suspendedMicrovmBead()},
		},
		"bead not found": {
			res:    &fakeResumer{available: true},
			finder: &fakeBeadFinder{err: errors.New("not found")},
		},
		"not a microvm agent": {
			res:    &fakeResumer{available: true},
			finder: &fakeBeadFinder{bead: notMicrovm},
		},
		"not suspended": {
			res:    &fakeResumer{available: true},
			finder: &fakeBeadFinder{bead: notSuspended},
		},
		"no runtime_id": {
			res:    &fakeResumer{available: true},
			finder: &fakeBeadFinder{bead: noRuntimeID},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := handleMicrovmResume(tc.res, tc.finder, nil, discardLogger())
			rr := doMicrovmResume(t, h, `{"agent":"thread-1234-5678"}`)
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (routing-unchanged signal)", rr.Code)
			}
			if resp := decodeResumeResp(t, rr); resp.Resumed {
				t.Errorf("resumed = true, want false for %q", name)
			}
			if len(tc.res.resumed) != 0 {
				t.Errorf("Resume must not be called for %q, got %v", name, tc.res.resumed)
			}
		})
	}
}

// TestHandleMicrovmResume_BackendErrorIs500: a genuine backend.Resume failure is
// a 500 so the bridge can distinguish it from a not-applicable no-op. The bridge
// treats non-200 as "fall through" too, and resume is idempotent, so a retry is
// safe.
func TestHandleMicrovmResume_BackendErrorIs500(t *testing.T) {
	res := &fakeResumer{available: true, err: errors.New("throttled")}
	finder := &fakeBeadFinder{bead: suspendedMicrovmBead()}
	h := handleMicrovmResume(res, finder, nil, discardLogger())

	rr := doMicrovmResume(t, h, `{"agent":"thread-1234-5678"}`)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 on backend resume error", rr.Code)
	}
	if resp := decodeResumeResp(t, rr); resp.Resumed {
		t.Error("resumed = true, want false on backend error")
	}
}

// TestHandleMicrovmResume_BadRequests: an unparseable body or an empty agent
// name is a 400 (the caller must name an agent), mirroring handleThawStatus.
func TestHandleMicrovmResume_BadRequests(t *testing.T) {
	h := handleMicrovmResume(&fakeResumer{available: true}, &fakeBeadFinder{bead: suspendedMicrovmBead()}, nil, discardLogger())

	t.Run("garbage body", func(t *testing.T) {
		rr := doMicrovmResume(t, h, `not json`)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 on invalid body", rr.Code)
		}
	})
	t.Run("empty agent", func(t *testing.T) {
		rr := doMicrovmResume(t, h, `{"agent":"  "}`)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 on empty agent", rr.Code)
		}
	})
}

// --- Step 5: resume rehydration launch + status endpoint --------------------

// TestHandleMicrovmResume_LaunchesRehydration: when the rehydration gate is on,
// a successful resume reports rehydrating:true AND launches AwaitRehydration for
// the agent (the fail-closed proof-of-life rendezvous). The bridge still routes
// on resumed:true.
func TestHandleMicrovmResume_LaunchesRehydration(t *testing.T) {
	res := &fakeResumer{available: true, rehydrateOn: true, awaitDone: make(chan struct{})}
	bead := suspendedMicrovmBead()
	bead.Fields["role"] = "thread"
	finder := &fakeBeadFinder{bead: bead}
	// Real ResumeMetrics + registry so we can assert the step-6 resume-latency
	// sample lands on proof-of-life (the fake AwaitRehydration returns Released=true).
	rm := gardeniometrics.NewResumeMetrics("e2e")
	reg := prometheus.NewRegistry()
	if err := rm.Register(reg); err != nil {
		t.Fatalf("register resume metrics: %v", err)
	}
	h := handleMicrovmResume(res, finder, rm, discardLogger())

	rr := doMicrovmResume(t, h, `{"agent":"thread-1234-5678"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	resp := decodeResumeResp(t, rr)
	if !resp.Resumed {
		t.Error("resumed = false, want true")
	}
	if !resp.Rehydrating {
		t.Error("rehydrating = false, want true when the gate is on")
	}
	// AwaitRehydration runs on a detached goroutine — wait for it to fire.
	select {
	case <-res.awaitDone:
	case <-time.After(2 * time.Second):
		t.Fatal("AwaitRehydration was never launched")
	}
	if len(res.awaited) != 1 || res.awaited[0] != "thread-1234-5678" {
		t.Errorf("AwaitRehydration called for %v, want [thread-1234-5678] (the agent name)", res.awaited)
	}
	// The proof-confirmed resume must have recorded exactly one
	// resume_latency_seconds{path=microvm-resume,role=thread} sample. The record
	// happens inside the same detached goroutine AFTER AwaitRehydration returns,
	// so poll briefly for it to be observed.
	if !waitForMicrovmResumeSample(t, reg, "thread") {
		t.Error("no gardenio_resume_latency_seconds{path=microvm-resume,role=thread} sample recorded on proof-of-life")
	}
}

// waitForMicrovmResumeSample polls reg for a resume_latency_seconds histogram
// series with path=microvm-resume + the given role and a non-zero sample count.
// The step-6 observation happens on a detached goroutine right after
// AwaitRehydration returns, so a short poll avoids a sleep-based race.
func waitForMicrovmResumeSample(t *testing.T, reg *prometheus.Registry, role string) bool {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mfs, err := reg.Gather()
		if err != nil {
			t.Fatalf("gather: %v", err)
		}
		for _, mf := range mfs {
			if mf.GetName() != "gardenio_resume_latency_seconds" {
				continue
			}
			for _, m := range mf.GetMetric() {
				if labelValue(m, "path") == gardeniometrics.ResumePathMicrovmResume &&
					labelValue(m, "role") == role &&
					m.GetHistogram().GetSampleCount() > 0 {
					return true
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func labelValue(m *dto.Metric, name string) string {
	for _, l := range m.GetLabel() {
		if l.GetName() == name {
			return l.GetValue()
		}
	}
	return ""
}

// TestHandleMicrovmResume_GateOffNoRehydration: with the gate off (rollback), a
// successful resume reports rehydrating:false and never launches the rendezvous.
func TestHandleMicrovmResume_GateOffNoRehydration(t *testing.T) {
	res := &fakeResumer{available: true, rehydrateOn: false}
	finder := &fakeBeadFinder{bead: suspendedMicrovmBead()}
	h := handleMicrovmResume(res, finder, nil, discardLogger())

	rr := doMicrovmResume(t, h, `{"agent":"thread-1234-5678"}`)
	resp := decodeResumeResp(t, rr)
	if !resp.Resumed {
		t.Error("resumed = false, want true")
	}
	if resp.Rehydrating {
		t.Error("rehydrating = true, want false when the gate is off")
	}
	// Give any (erroneous) goroutine a moment; none should have run.
	time.Sleep(20 * time.Millisecond)
	if len(res.awaited) != 0 {
		t.Errorf("gate-off resume must not launch AwaitRehydration, got %v", res.awaited)
	}
}

func doResumeStatus(t *testing.T, h http.HandlerFunc, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet,
		"/api/v1/microvm/resume/status"+query, nil)
	rr := httptest.NewRecorder()
	h(rr, req)
	return rr
}

// TestHandleMicrovmResumeStatus reports the in-flight rehydration state.
func TestHandleMicrovmResumeStatus(t *testing.T) {
	res := &fakeResumer{available: true, rehydratingSet: map[string]bool{"thread-live": true}}
	h := handleMicrovmResumeStatus(res, discardLogger())

	t.Run("in-flight", func(t *testing.T) {
		rr := doResumeStatus(t, h, "?agent=thread-live")
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rr.Code)
		}
		var resp microvmResumeStatusResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !resp.Rehydrating || resp.Agent != "thread-live" {
			t.Errorf("resp = %+v, want rehydrating:true agent:thread-live", resp)
		}
	})
	t.Run("settled/unknown", func(t *testing.T) {
		rr := doResumeStatus(t, h, "?agent=thread-gone")
		var resp microvmResumeStatusResponse
		_ = json.Unmarshal(rr.Body.Bytes(), &resp)
		if resp.Rehydrating {
			t.Error("unknown agent should report rehydrating:false")
		}
	})
	t.Run("empty agent 400", func(t *testing.T) {
		rr := doResumeStatus(t, h, "?agent=")
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 on empty agent", rr.Code)
		}
	})
	t.Run("backend off", func(t *testing.T) {
		off := handleMicrovmResumeStatus(&fakeResumer{available: false}, discardLogger())
		rr := doResumeStatus(t, off, "?agent=thread-live")
		var resp microvmResumeStatusResponse
		_ = json.Unmarshal(rr.Body.Bytes(), &resp)
		if resp.Rehydrating {
			t.Error("backend off should report rehydrating:false")
		}
	})
}
