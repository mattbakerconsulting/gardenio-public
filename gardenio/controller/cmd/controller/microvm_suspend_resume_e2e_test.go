package main

import (
	"context"
	"net/http"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/config"
	gardeniometrics "gardenio/controller/internal/metrics"
	"gardenio/controller/internal/microvm"
	"gardenio/controller/internal/podmanager"
	"gardenio/controller/internal/subscriber"
)

// This file is the end-to-end suspend/resume PROOF the fix-now acceptance bar
// requires (decision gd-kymiq5dVge, task gd-5NehzMXXO7). It exercises the full
// lambda_microvm thread-agent life story — idle→suspend-hook→S3-session-survival
// →resume — across the TWO real controller production seams that own it, sharing
// one bead-state so the handoff between them is genuine (not two isolated unit
// tests):
//
//   1. gb stop  → subscriber.AgentStop → microvmTeardown(...) SUSPENDS the VM and
//      LEAVES THE BEAD OPEN at agent_state=stopping (the resume target), and
//      records gardenio_microvm_suspends_total{outcome=suspended}.
//   2. re-mention → POST /api/v1/microvm/resume → handleMicrovmResume(...) reads
//      that SAME open bead, resumes the SAME runtime_id in place (continuity —
//      same VM, so disk + the S3-restored transcript survive), and records
//      gardenio_resume_latency_seconds{path=microvm-resume} on proof-of-life.
//
// The two landed blocker fixes this proof stands on:
//   - ka-0000002917 (idle-suspend-kill): the substrate no longer SIGTERM-kills a
//     working agent ~13s into the turn — the graceful AgentStop is what drives
//     the suspend, and the VM survives it. (The AWS-side half — raising the
//     IdlePolicy idle window above the VM lifetime so AWS never idle-reaps
//     mid-turn — is proven at the substrate level in
//     internal/microvm: TestE2E_MicrovmSuspendResume_SubstrateContinuity.)
//   - ka-0000001013 (S3 session persistence): the per-run S3 session credentials
//     that back the transcript restore MUST survive standby — the suspend path
//     deliberately does NOT revoke them (asserted in the substrate test and by
//     TestLifecycle_Suspend_DoesNotRevokeCredentials). This E2E's resume reads
//     the SAME runtime_id, so the resumed VM keeps those creds and its restored
//     session.

// e2eMicrovmSubstrate is a single fake AWS/bead substrate that satisfies BOTH
// controller seams — microvmTeardowner (Suspend/Terminate on gb stop) and
// microvmResumer (Resume + rehydration gate on re-mention) — over ONE shared
// bead-state map. That shared state is what makes this an E2E rather than two
// unit tests: the fields the suspend seam writes are the exact fields the resume
// seam (through the real handleMicrovmResume gates) reads back. Its Suspend/Resume
// mutations mirror the real microvm.Lifecycle transitions (lifecycle.go) — the
// only necessarily-faked part is the AWS call itself; the routing, gating, metric
// recording, and runtime_id handoff all run the real production code.
type e2eMicrovmSubstrate struct {
	available   bool
	rehydrateOn bool
	awaitDone   chan struct{}

	// agentToBead maps a Slack-thread agent name to its open agent bead ID, so
	// FindAgentBead (the resume handler's bead-read surface) resolves the same
	// bead the suspend seam mutated.
	agentToBead map[string]string
	// beads is the shared, mutable open-bead field-set, keyed by bead ID.
	beads map[string]map[string]string

	suspended  []string // bead IDs suspended, in order
	resumed    []string // bead IDs resumed, in order
	terminated []string // bead IDs terminated, in order
}

func (s *e2eMicrovmSubstrate) Available() bool { return s.available }

// Suspend mirrors microvm.Lifecycle.Suspend: SuspendMicrovm (never Terminate),
// stamp runtime_state=SUSPENDED + the runtime_suspended_at standby anchor on the
// STILL-OPEN bead (agent_state stays stopping — the resume target), and DO NOT
// revoke per-run/S3 session credentials (they must survive standby).
func (s *e2eMicrovmSubstrate) Suspend(_ context.Context, view microvm.AgentBeadView, _ string) error {
	s.suspended = append(s.suspended, view.ID)
	b := s.beads[view.ID]
	b["runtime_state"] = "SUSPENDED"
	b["runtime_suspended_at"] = time.Now().UTC().Format(time.RFC3339)
	// agent_state is left at "stopping" and the bead is NOT closed — exactly the
	// resume-target shape step 3's handler gates on.
	return nil
}

func (s *e2eMicrovmSubstrate) Terminate(_ context.Context, view microvm.AgentBeadView, _ string) error {
	s.terminated = append(s.terminated, view.ID)
	return nil
}

// Resume mirrors microvm.Lifecycle.Resume: ResumeMicrovm on the SAME runtime_id,
// flip the bead out of frozen-awaiting-thaw (runtime_state=PENDING; agent_state
// →resuming when the proof-of-life gate is on, else →working), clear the
// standby-age anchor, and LEAVE THE BEAD OPEN (a close would terminate the VM).
func (s *e2eMicrovmSubstrate) Resume(_ context.Context, view microvm.AgentBeadView) error {
	s.resumed = append(s.resumed, view.ID)
	b := s.beads[view.ID]
	b["runtime_state"] = "PENDING"
	b["runtime_suspended_at"] = ""
	if s.rehydrateOn {
		b["agent_state"] = "resuming"
	} else {
		b["agent_state"] = "working"
	}
	return nil
}

func (s *e2eMicrovmSubstrate) ResumeRehydrateEnabled() bool { return s.rehydrateOn }

// AwaitRehydration is the fail-closed proof-of-life gate the handler runs on a
// detached context after Resume when the gate is on. On proof-of-life it settles
// the bead to working and reports Released=true, which is what makes the handler
// record the resume-latency sample.
func (s *e2eMicrovmSubstrate) AwaitRehydration(_ context.Context, view microvm.AgentBeadView, _ string, _ time.Time) microvm.RehydrationOutcome {
	s.beads[view.ID]["agent_state"] = "working"
	if s.awaitDone != nil {
		close(s.awaitDone)
	}
	return microvm.RehydrationOutcome{Released: true, Reason: "e2e proof-of-life"}
}

func (s *e2eMicrovmSubstrate) IsRehydrating(string) bool { return false }

// FindAgentBead resolves the agent's SHARED open bead — the same map the suspend
// seam mutated — so the resume handler's gates (is-microvm, agent_state=stopping,
// runtime_id present) see the real post-suspend state.
func (s *e2eMicrovmSubstrate) FindAgentBead(_ context.Context, agentName string) (*beadsapi.BeadDetail, error) {
	beadID, ok := s.agentToBead[agentName]
	if !ok {
		return nil, nil
	}
	// Return a snapshot copy so the handler reads a stable view.
	fields := map[string]string{}
	for k, v := range s.beads[beadID] {
		fields[k] = v
	}
	return &beadsapi.BeadDetail{ID: beadID, Type: "agent", Status: "open", Fields: fields}, nil
}

// TestE2E_MicrovmSuspendResume_FullCycle drives one lambda_microvm thread agent
// through gb stop → suspend → re-mention → resume across the two real controller
// seams, sharing one bead-state, and asserts the whole outcome contract:
// session continuity (same runtime_id), bead stays open the entire time, and
// BOTH headline metrics increment (microvm_suspends_total + the microvm-resume
// latency sample) — the acceptance signals named on gd-5NehzMXXO7.
func TestE2E_MicrovmSuspendResume_FullCycle(t *testing.T) {
	const (
		agentName = "thread-1784500000-123456"
		beadID    = "gd-mvm-e2e"
		vmID      = "mvm-e2e-vm0" // the ONE VM that must survive the whole cycle
		channel   = "C0BHRQ6PS3F" // #gardenio-microvms
		threadTS  = "1784500000.123456"
	)

	// A live MicroVM thread agent mid-turn: RUNNING, bound to a Slack thread,
	// carrying its persisted runtime_id (VM0). agent_state=stopping is the state
	// the in-VM `gb stop` writes (BOAT_FREEZE_ELIGIBLE=true → AgentStop, not close).
	sub := &e2eMicrovmSubstrate{
		available:   true,
		rehydrateOn: true,
		awaitDone:   make(chan struct{}),
		agentToBead: map[string]string{agentName: beadID},
		beads: map[string]map[string]string{
			beadID: {
				"agent":            agentName,
				"execution_target": podmanager.ExecutionTargetLambdaMicroVM,
				"agent_state":      "stopping",
				"runtime_id":       vmID,
				"runtime_state":    "RUNNING",
				"role":             "gardenio-microvms",
			},
		},
	}
	rm, reg := newTestResumeMetrics(t)

	// --- Seam 1: gb stop → microvmTeardown SUSPENDS (bead left open) -----------
	cfg := &config.Config{MicrovmSuspendEnabled: true}
	stopEvent := subscriber.Event{
		Type:      subscriber.AgentStop,
		Project:   "gardenio",
		Role:      "gardenio-microvms",
		AgentName: agentName,
		BeadID:    beadID,
		Metadata: map[string]string{
			"execution_target":     podmanager.ExecutionTargetLambdaMicroVM,
			"agent_state":          "stopping",
			"runtime_id":           vmID,
			"runtime_state":        "RUNNING",
			"slack_thread_channel": channel,
			"slack_thread_ts":      threadTS,
		},
	}
	out := microvmTeardown(context.Background(), discardLogger(), cfg, stopEvent, beadID, sub, rm)

	if out != microvmTeardownSuspended {
		t.Fatalf("gb stop teardown outcome = %v, want suspended", out)
	}
	if len(sub.suspended) != 1 || sub.suspended[0] != beadID {
		t.Fatalf("Suspend calls = %v, want [%s]", sub.suspended, beadID)
	}
	if len(sub.terminated) != 0 {
		t.Fatalf("a suspended thread agent must NOT be terminated (VM is the resume target), got %v", sub.terminated)
	}
	// The bead must be left OPEN in the resume-target shape: agent_state=stopping,
	// runtime_id preserved, runtime_state=SUSPENDED. If suspend closed the bead or
	// dropped runtime_id, the resume handler below would (correctly) refuse.
	if got := sub.beads[beadID]["agent_state"]; got != "stopping" {
		t.Fatalf("post-suspend agent_state = %q, want stopping (resume target)", got)
	}
	if got := sub.beads[beadID]["runtime_state"]; got != "SUSPENDED" {
		t.Fatalf("post-suspend runtime_state = %q, want SUSPENDED", got)
	}
	if got := sub.beads[beadID]["runtime_id"]; got != vmID {
		t.Fatalf("post-suspend runtime_id = %q, want %s preserved (continuity anchor)", got, vmID)
	}
	if got := microvmSuspendCount(t, reg, "gardenio-microvms", gardeniometrics.MicrovmSuspendOutcomeSuspended); got != 1 {
		t.Fatalf("microvm_suspends_total{outcome=suspended} = %d, want 1", got)
	}

	// --- Seam 2: re-mention → handleMicrovmResume RESUMES the SAME VM ----------
	h := handleMicrovmResume(sub, sub, rm, discardLogger())
	rr := doMicrovmResume(t, h, `{"agent":"`+agentName+`"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("resume status = %d, want 200 (body=%q)", rr.Code, rr.Body.String())
	}
	resp := decodeResumeResp(t, rr)
	if !resp.Resumed {
		t.Fatalf("resumed = false, want true (reason=%q)", resp.Reason)
	}
	// Continuity: the resume must target the SAME VM the suspend left — never a
	// fresh RunMicrovm. This is the whole point of native suspend/resume.
	if resp.MicrovmID != vmID {
		t.Fatalf("resumed MicrovmID = %q, want the suspended VM %q (session continuity)", resp.MicrovmID, vmID)
	}
	if len(sub.resumed) != 1 || sub.resumed[0] != beadID {
		t.Fatalf("Resume calls = %v, want [%s]", sub.resumed, beadID)
	}
	if len(sub.terminated) != 0 {
		t.Fatalf("resume must not terminate any VM, got %v", sub.terminated)
	}
	// The bead is still OPEN and cleared of the standby anchor.
	if got := sub.beads[beadID]["runtime_suspended_at"]; got != "" {
		t.Fatalf("post-resume runtime_suspended_at = %q, want cleared", got)
	}

	// The step-5 proof-of-life rendezvous runs on a detached goroutine; wait for
	// it, then assert the microvm-resume latency sample landed (the second
	// headline acceptance metric).
	select {
	case <-sub.awaitDone:
	case <-time.After(2 * time.Second):
		t.Fatal("AwaitRehydration proof-of-life did not run within 2s")
	}
	if !waitForMicrovmResumeSample(t, reg, "gardenio-microvms") {
		t.Fatal("no gardenio_resume_latency_seconds{path=microvm-resume,role=gardenio-microvms} sample recorded on resume proof-of-life")
	}
	if got := sub.beads[beadID]["agent_state"]; got != "working" {
		t.Fatalf("final agent_state = %q, want working (multi-turn conversation can continue on the same VM)", got)
	}
}

// TestE2E_MicrovmSuspendResume_KillDoesNotSuspend is the negative control: a
// forced AgentKill (not a graceful stop) terminates the VM — there is no wake to
// serve — so the resume path must then find no suspended bead. Proves the E2E
// suspend is gated on the graceful-stop signal, not any teardown.
func TestE2E_MicrovmSuspendResume_KillDoesNotSuspend(t *testing.T) {
	const (
		agentName = "thread-kill"
		beadID    = "gd-mvm-kill"
		vmID      = "mvm-kill-vm"
	)
	sub := &e2eMicrovmSubstrate{
		available:   true,
		agentToBead: map[string]string{agentName: beadID},
		beads: map[string]map[string]string{
			beadID: {
				"execution_target": podmanager.ExecutionTargetLambdaMicroVM,
				"agent_state":      "stopping",
				"runtime_id":       vmID,
				"runtime_state":    "RUNNING",
				"role":             "gardenio-microvms",
			},
		},
	}
	cfg := &config.Config{MicrovmSuspendEnabled: true}
	killEvent := subscriber.Event{
		Type:      subscriber.AgentKill,
		Role:      "gardenio-microvms",
		AgentName: agentName,
		BeadID:    beadID,
		Metadata: map[string]string{
			"execution_target":     podmanager.ExecutionTargetLambdaMicroVM,
			"agent_state":          "stopping",
			"runtime_id":           vmID,
			"runtime_state":        "RUNNING",
			"slack_thread_channel": "C0BHRQ6PS3F",
			"slack_thread_ts":      "1784500000.123456",
		},
	}
	out := microvmTeardown(context.Background(), discardLogger(), cfg, killEvent, beadID, sub, nil)
	if out != microvmTeardownTerminated {
		t.Fatalf("AgentKill teardown outcome = %v, want terminated", out)
	}
	if len(sub.suspended) != 0 {
		t.Fatalf("AgentKill must not suspend, got %v", sub.suspended)
	}
	if len(sub.terminated) != 1 {
		t.Fatalf("AgentKill must terminate, got %v", sub.terminated)
	}
}
