package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// ciWaitAgentBead is the stopping agent's own bead carrying a CI wait, as the
// parkFixture handle map serves it at GET /v1/beads/<id>.
func ciWaitAgentBead(id, state, task string) map[string]any {
	return map[string]any{
		"id": id, "type": "agent", "status": "in_progress",
		"fields": map[string]string{
			beadsapi.FieldCIWaitState: state,
			beadsapi.FieldCIWaitKey:   "joyousaversion/gardenio@" + testCISHA,
			beadsapi.FieldCIWaitTask:  task,
		},
	}
}

// TestCheckClaimedWork_ParkedOnCIWait covers the third findParkHandle branch
// (plan wake-on-ci-completion.md §2.1): a claimed task named by a PENDING CI
// wait on the stopping agent's own bead is parked — no handle bead, no
// blocks-edge — while every other wait shape still trips the gate.
func TestCheckClaimedWork_ParkedOnCIWait(t *testing.T) {
	oldActor := actor
	actor = "alice"
	t.Cleanup(func() { actor = oldActor })

	claimed := []map[string]any{{
		"id": "gd-WORK01", "title": "pushed, waiting on CI",
		"status": "in_progress", "kind": "issue", "assignee": "alice",
	}}

	t.Run("pending wait naming the task -> gate satisfied", func(t *testing.T) {
		parkFixture(t, claimed, nil, map[string]map[string]any{
			"gd-AGENT01": ciWaitAgentBead("gd-AGENT01", beadsapi.CIWaitStatePending, "gd-WORK01"),
		})
		if err := checkClaimedWork(context.Background(), "gd-AGENT01"); err != nil {
			t.Errorf("task parked on a pending CI wait must not trip the gate, got: %v", err)
		}
		if got := findParkHandle(context.Background(), "gd-WORK01", "gd-AGENT01"); got != "gd-AGENT01" {
			t.Errorf("findParkHandle = %q, want the agent bead itself", got)
		}
	})

	for _, tc := range []struct {
		name  string
		agent map[string]any
	}{
		{"wait names a different task", ciWaitAgentBead("gd-AGENT01", beadsapi.CIWaitStatePending, "gd-OTHER")},
		{"wait names no task", ciWaitAgentBead("gd-AGENT01", beadsapi.CIWaitStatePending, "")},
		{"wait already resolved", ciWaitAgentBead("gd-AGENT01", beadsapi.CIWaitStateResolved, "gd-WORK01")},
		{"wait timed out", ciWaitAgentBead("gd-AGENT01", beadsapi.CIWaitStateTimedOut, "gd-WORK01")},
		{"wait consumed", ciWaitAgentBead("gd-AGENT01", beadsapi.CIWaitStateConsumed, "gd-WORK01")},
		{"agent bead unreadable", nil},
	} {
		t.Run(tc.name+" -> gate still fires", func(t *testing.T) {
			handles := map[string]map[string]any{}
			if tc.agent != nil {
				handles["gd-AGENT01"] = tc.agent
			}
			parkFixture(t, claimed, nil, handles)
			err := checkClaimedWork(context.Background(), "gd-AGENT01")
			if err == nil {
				t.Fatal("gate must still fire; got nil (CI-wait exemption widened into a bypass)")
			}
			if !strings.Contains(err.Error(), "gd-WORK01") {
				t.Errorf("headline should name the unparked bead, got: %q", err.Error())
			}
		})
	}

	// Another agent's pending wait on the same task does not park THIS agent's
	// claim: the lookup reads only the stopping agent's own bead.
	t.Run("another agent's wait -> gate still fires", func(t *testing.T) {
		parkFixture(t, claimed, nil, map[string]map[string]any{
			"gd-OTHER99": ciWaitAgentBead("gd-OTHER99", beadsapi.CIWaitStatePending, "gd-WORK01"),
			"gd-AGENT01": {"id": "gd-AGENT01", "type": "agent", "status": "in_progress"},
		})
		if err := checkClaimedWork(context.Background(), "gd-AGENT01"); err == nil {
			t.Fatal("a wait on another agent's bead must not park this claim")
		}
	})
}

// TestCheckCIWaitConcluded covers the "CI already concluded" refusal: a stop
// after the wait resolved or timed out would park on a wake already spent.
func TestCheckCIWaitConcluded(t *testing.T) {
	for _, tc := range []struct {
		state   string
		refused bool
	}{
		{"", false},
		{beadsapi.CIWaitStatePending, false},
		{beadsapi.CIWaitStateConsumed, false},
		{beadsapi.CIWaitStateResolved, true},
		{beadsapi.CIWaitStateTimedOut, true},
	} {
		t.Run("state="+tc.state, func(t *testing.T) {
			err := checkCIWaitConcluded(beadsapi.CIWait{State: tc.state, Key: "o/r@" + testCISHA})
			if (err != nil) != tc.refused {
				t.Fatalf("refused = %v, want %v (err: %v)", err != nil, tc.refused, err)
			}
			if tc.refused && !strings.Contains(err.Error(), "gb wait ci --result") {
				t.Errorf("refusal should name the remedy, got: %q", err.Error())
			}
		})
	}

	// The gate reads the wait off the agent bead; an unreadable bead fails open.
	t.Run("loadCIWait reads the agent bead and fails open", func(t *testing.T) {
		parkFixture(t, nil, nil, map[string]map[string]any{
			"gd-AGENT01": ciWaitAgentBead("gd-AGENT01", beadsapi.CIWaitStateResolved, "gd-WORK01"),
		})
		if err := checkCIWaitConcluded(loadCIWait(context.Background(), "gd-AGENT01")); err == nil {
			t.Error("a resolved wait on the agent bead must refuse the stop")
		}
		if err := checkCIWaitConcluded(loadCIWait(context.Background(), "gd-MISSING")); err != nil {
			t.Errorf("an unreadable agent bead must fail open, got: %v", err)
		}
	})
}

// TestApplyCIWaitPark covers the revivable close: a pending wait closes the bead
// successor=same (the dormant-identity marker event-wake keys on) and stamps
// next_respawn_at one poll interval out, so the timer wake-scanner revives it
// even though no bridge resolver ever concludes the wait (gd-PwyvuF0fHp).
func TestApplyCIWaitPark(t *testing.T) {
	now := time.Date(2026, 10, 30, 5, 0, 0, 0, time.UTC)
	pending := beadsapi.CIWait{
		State: beadsapi.CIWaitStatePending, Key: "o/r@" + testCISHA,
		Deadline: now.Add(defaultCIWaitTimeout).Format(time.RFC3339),
	}

	t.Run("pending -> successor=same, next_respawn_at one poll out", func(t *testing.T) {
		fields := map[string]string{"agent_state": "done", "gate_satisfied_by": "operator"}
		if !applyCIWaitPark(fields, pending, false, now) {
			t.Fatal("a pending wait must park the close")
		}
		if fields["successor"] != SuccessorSame {
			t.Errorf("successor = %q, want %q", fields["successor"], SuccessorSame)
		}
		if fields["close_reason"] != CloseReasonCIWaitParked {
			t.Errorf("close_reason = %q, want %q", fields["close_reason"], CloseReasonCIWaitParked)
		}
		if want := now.Add(ciWaitPollInterval).Format(time.RFC3339); fields[AgentBeadFieldNextRespawnAt] != want {
			t.Errorf("%s = %q, want %q (nothing else wakes a CI park)", AgentBeadFieldNextRespawnAt, fields[AgentBeadFieldNextRespawnAt], want)
		}
	})

	for _, tc := range []struct {
		name             string
		wait             beadsapi.CIWait
		goalUnachievable bool
	}{
		{"no wait", beadsapi.CIWait{}, false},
		{"resolved wait", beadsapi.CIWait{State: beadsapi.CIWaitStateResolved}, false},
		{"consumed wait", beadsapi.CIWait{State: beadsapi.CIWaitStateConsumed}, false},
		{"pending but goal unachievable", pending, true},
	} {
		t.Run(tc.name+" -> terminal close unchanged", func(t *testing.T) {
			fields := map[string]string{"agent_state": "done"}
			if applyCIWaitPark(fields, tc.wait, tc.goalUnachievable, now) {
				t.Fatal("must not park")
			}
			if _, ok := fields["successor"]; ok {
				t.Errorf("successor stamped on a terminal close: %q", fields["successor"])
			}
			if _, ok := fields[AgentBeadFieldNextRespawnAt]; ok {
				t.Errorf("%s stamped on a terminal close", AgentBeadFieldNextRespawnAt)
			}
		})
	}
}

// TestCIWaitNextWake: the timer wake is one poll interval out, clamped to the
// deadline (never in the past), and falls back to the interval on a bad stamp.
func TestCIWaitNextWake(t *testing.T) {
	now := time.Date(2026, 10, 30, 5, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, deadline string
		want           time.Time
	}{
		{"far deadline", now.Add(2 * time.Hour).Format(time.RFC3339), now.Add(ciWaitPollInterval)},
		{"deadline inside the interval", now.Add(10 * time.Minute).Format(time.RFC3339), now.Add(10 * time.Minute)},
		{"deadline passed", now.Add(-time.Minute).Format(time.RFC3339), now},
		{"unreadable deadline", "soon", now.Add(ciWaitPollInterval)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ciWaitNextWake(beadsapi.CIWait{Deadline: tc.deadline}, now)
			if want := tc.want.Format(time.RFC3339); got != want {
				t.Errorf("next wake = %s, want %s", got, want)
			}
		})
	}
}

// TestSettleCIWaitForStop covers the stop-side GitHub check (gd-3iOqZlkZLV): a
// woken agent that goes straight to `gb stop` on a wait whose runs have all
// concluded gets the concluded state recorded, so the stop is refused with
// `gb wait ci --result` instead of re-parking on a wake already spent.
func TestSettleCIWaitForStop(t *testing.T) {
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	past := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	pendingWait := stopPendingWaitFields
	concluded := `{"workflow_runs":[
		{"id":11,"name":"ci","run_attempt":1,"status":"completed","conclusion":"failure"},
		{"id":12,"name":"checks","run_attempt":1,"status":"completed","conclusion":"success"}]}`
	running := `{"workflow_runs":[
		{"id":11,"name":"ci","run_attempt":1,"status":"completed","conclusion":"failure"},
		{"id":12,"name":"checks","run_attempt":1,"status":"in_progress","conclusion":null}]}`

	t.Run("CI concluded -> resolved recorded, stop refused, task no longer parked", func(t *testing.T) {
		fakeCIRunsGH(t, concluded)
		f := newCIWaitAgentFixture(t, "gd-AGENT01", pendingWait(future))
		settleCIWaitForStop(context.Background(), "gd-AGENT01")
		if got := f.fields[beadsapi.FieldCIWaitState]; got != beadsapi.CIWaitStateResolved {
			t.Fatalf("state = %q, want resolved", got)
		}
		if !strings.Contains(f.fields[beadsapi.FieldCIWaitResult], `"conclusion":"failure"`) {
			t.Errorf("result not recorded: %q", f.fields[beadsapi.FieldCIWaitResult])
		}
		w := loadCIWait(context.Background(), "gd-AGENT01")
		if err := checkCIWaitConcluded(w); err == nil || !strings.Contains(err.Error(), "gb wait ci --result") {
			t.Errorf("stop must be refused with the --result remedy, got: %v", err)
		}
		fields := map[string]string{}
		if applyCIWaitPark(fields, w, false, time.Now()) {
			t.Error("a concluded wait must not park the close")
		}
	})

	t.Run("deadline passed with CI running -> timed_out recorded", func(t *testing.T) {
		fakeCIRunsGH(t, running)
		f := newCIWaitAgentFixture(t, "gd-AGENT01", pendingWait(past))
		settleCIWaitForStop(context.Background(), "gd-AGENT01")
		if got := f.fields[beadsapi.FieldCIWaitState]; got != beadsapi.CIWaitStateTimedOut {
			t.Fatalf("state = %q, want timed_out", got)
		}
	})

	t.Run("CI still running -> nothing written, still parks", func(t *testing.T) {
		fakeCIRunsGH(t, running)
		f := newCIWaitAgentFixture(t, "gd-AGENT01", pendingWait(future))
		settleCIWaitForStop(context.Background(), "gd-AGENT01")
		if f.patches != 0 {
			t.Errorf("a still-running wait wrote %d PATCH(es)", f.patches)
		}
		if !loadCIWait(context.Background(), "gd-AGENT01").Pending() {
			t.Error("wait must stay pending")
		}
	})

	t.Run("GitHub unreadable -> nothing written (fails open to the park)", func(t *testing.T) {
		old := prGHRun
		prGHRun = func(context.Context, ...string) (string, error) { return "", errors.New("gh: no auth") }
		t.Cleanup(func() { prGHRun = old })
		f := newCIWaitAgentFixture(t, "gd-AGENT01", pendingWait(future))
		settleCIWaitForStop(context.Background(), "gd-AGENT01")
		if f.patches != 0 || f.fields[beadsapi.FieldCIWaitState] != beadsapi.CIWaitStatePending {
			t.Errorf("a GitHub error must leave the wait pending; patches=%d state=%q", f.patches, f.fields[beadsapi.FieldCIWaitState])
		}
	})

	t.Run("no pending wait -> GitHub not asked", func(t *testing.T) {
		gh := fakeCIRunsGH(t, concluded)
		f := newCIWaitAgentFixture(t, "gd-AGENT01", map[string]string{
			beadsapi.FieldCIWaitState: beadsapi.CIWaitStateConsumed,
			beadsapi.FieldCIWaitKey:   "o/r@" + testCISHA,
		})
		settleCIWaitForStop(context.Background(), "gd-AGENT01")
		if *gh != "" || f.patches != 0 {
			t.Errorf("gh called %q, patches %d; want neither", *gh, f.patches)
		}
	})
}

// stopPendingWaitFields is the agent bead of a stop on a pending wait for
// o/r@testCISHA, requested by task gd-WORK01.
func stopPendingWaitFields(deadline string) map[string]string {
	return map[string]string{
		beadsapi.FieldCIWaitState:    beadsapi.CIWaitStatePending,
		beadsapi.FieldCIWaitKey:      "o/r@" + testCISHA,
		beadsapi.FieldCIWaitTask:     "gd-WORK01",
		beadsapi.FieldCIWaitDeadline: deadline,
	}
}

// TestSettleCIWaitForStopCancelsDeadWait: a pending wait no run will conclude
// (its task closed, or its sha has no runs and the PR moved on) is recorded
// cancelled at stop, so the stop neither parks nor is refused (gd-lHWcyCRj1Q).
func TestSettleCIWaitForStopCancelsDeadWait(t *testing.T) {
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	running := `{"workflow_runs":[
		{"id":11,"name":"ci","run_attempt":1,"status":"completed","conclusion":"failure"},
		{"id":12,"name":"checks","run_attempt":1,"status":"in_progress","conclusion":null}]}`
	pendingWait := stopPendingWaitFields

	t.Run("task closed -> cancelled recorded, stop neither parks nor is refused", func(t *testing.T) {
		fakeCIRunsGH(t, running)
		f := newCIWaitAgentFixture(t, "gd-AGENT01", pendingWait(future))
		f.others["gd-WORK01"] = "closed"
		settleCIWaitForStop(context.Background(), "gd-AGENT01")
		if got := f.fields[beadsapi.FieldCIWaitState]; got != beadsapi.CIWaitStateCancelled {
			t.Fatalf("state = %q, want cancelled", got)
		}
		w := loadCIWait(context.Background(), "gd-AGENT01")
		if err := checkCIWaitConcluded(w); err != nil {
			t.Errorf("a cancelled wait must not refuse the stop: %v", err)
		}
		if applyCIWaitPark(map[string]string{}, w, false, time.Now()) {
			t.Error("a cancelled wait must not park the close")
		}
	})

	t.Run("no runs and the PR merged at another head -> cancelled", func(t *testing.T) {
		fakeCIGH(t, `{"workflow_runs":[]}`, mergedElsewhere)
		f := newCIWaitAgentFixture(t, "gd-AGENT01", pendingWait(future))
		settleCIWaitForStop(context.Background(), "gd-AGENT01")
		if got := f.fields[beadsapi.FieldCIWaitState]; got != beadsapi.CIWaitStateCancelled {
			t.Fatalf("state = %q, want cancelled", got)
		}
	})
}

// TestCIWaitWakeHint: a session resumed with an unread CI wait leads its work
// prompt with `gb wait ci --result` (gd-3iOqZlkZLV); no wait, a consumed wait
// or no agent bead adds nothing.
func TestCIWaitWakeHint(t *testing.T) {
	for _, tc := range []struct {
		state string
		hint  bool
	}{
		{"", false},
		{beadsapi.CIWaitStateConsumed, false},
		{beadsapi.CIWaitStatePending, true},
		{beadsapi.CIWaitStateResolved, true},
		{beadsapi.CIWaitStateTimedOut, true},
	} {
		t.Run("state="+tc.state, func(t *testing.T) {
			got := ciWaitWakeHintFor(beadsapi.CIWait{State: tc.state, Key: "o/r@" + testCISHA})
			if (got != "") != tc.hint {
				t.Fatalf("hint = %q, want present=%v", got, tc.hint)
			}
			if tc.hint && (!strings.Contains(got, "gb wait ci --result") || !strings.Contains(got, "o/r@"+testCISHA)) {
				t.Errorf("hint should name the key and the --result step, got: %q", got)
			}
		})
	}

	t.Run("ciWaitWakeHint reads the wait off this pod's agent bead", func(t *testing.T) {
		newCIWaitAgentFixture(t, "gd-AGENT01", map[string]string{
			beadsapi.FieldCIWaitState: beadsapi.CIWaitStatePending,
			beadsapi.FieldCIWaitKey:   "o/r@" + testCISHA,
		})
		t.Setenv("BOAT_AGENT_BEAD_ID", "gd-AGENT01")
		if got := ciWaitWakeHint(); !strings.HasPrefix(got, "You were parked on `gb wait ci` for o/r@"+testCISHA) {
			t.Errorf("hint = %q, want the pending wait's lead-in", got)
		}
		t.Setenv("BOAT_AGENT_BEAD_ID", "gd-MISSING")
		if got := ciWaitWakeHint(); got != "" {
			t.Errorf("unreadable agent bead -> no hint, got: %q", got)
		}
		t.Setenv("BOAT_AGENT_BEAD_ID", "")
		if got := ciWaitWakeHint(); got != "" {
			t.Errorf("no agent bead -> no hint, got: %q", got)
		}
	})
}
