package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// sweepGate is a stop-gate verdict for session at ts.
func sweepGate(agent, session, decision string, ts time.Time) sweepHookEvent {
	return sweepHookEvent{Agent: agent, AgentBeadID: "gd-" + agent, SessionID: session, Event: "Stop",
		StopGateDecision: decision, Reason: "decision: decision point offered before session end", TS: ts}
}

// sweepWait is a foreground Bash call that ended at end after d.
func sweepWait(agent, session, cmd string, end time.Time, d time.Duration) sweepHookEvent {
	ev := sweepBash(agent, session, cmd, "", end)
	ev.DurationMs = d.Milliseconds()
	return ev
}

// sweepPreWait is the PreToolUse of a foreground Bash call started at ts.
func sweepPreWait(agent, session, id, cmd string, ts time.Time) sweepHookEvent {
	ev := sweepBash(agent, session, cmd, "", ts)
	ev.Event, ev.ToolResponse, ev.ToolUseID = "PreToolUse", nil, id
	return ev
}

// sweepSessionBead is the session bead of bus session sid, started at start;
// ended and closed are RFC 3339 or "".
func sweepSessionBead(sid string, start time.Time, ended, closed string) *beadsapi.BeadDetail {
	f := map[string]string{"session_log": "/home/agent/.claude/projects/w/" + sid + ".jsonl", "started_at": start.Format(time.RFC3339)}
	if ended != "" {
		f["ended_at"] = ended
	}
	return &beadsapi.BeadDetail{ID: "gd-sess-" + sid, Type: "session", ClosedAt: closed, Fields: f}
}

func TestDetectStopGateWaits(t *testing.T) {
	t0 := sweepTestNow.Add(-2 * time.Hour)
	poll := "until [ $(gh run list --commit abc --json status -q length) = 0 ]; do sleep 20; done"
	in := sweepInput{
		StopGates: []sweepHookEvent{
			sweepGate("held", "s-held", "block", t0),
			sweepGate("short", "s-short", "block", t0),
			sweepGate("allowed", "s-allowed", "block", t0),
			sweepGate("allowed", "s-allowed", "allow", t0.Add(30*time.Second)),
			sweepGate("late", "s-late", "block", t0),
		},
		Hooks: []sweepHookEvent{
			// held: two chained CI polls right after the block, 4 min in all.
			sweepWait("held", "s-held", poll, t0.Add(65*time.Second), 60*time.Second),
			sweepWait("held", "s-held", "tail --pid=42 -f /dev/null", t0.Add(4*time.Minute+10*time.Second), 3*time.Minute),
			// short: a 40 s wait, then real work.
			sweepWait("short", "s-short", "tail --pid=42 -f /dev/null", t0.Add(45*time.Second), 40*time.Second),
			sweepWait("short", "s-short", "go test ./cmd/gb/", t0.Add(10*time.Minute), 8*time.Minute),
			// allowed: the wait starts after the gate let the turn end.
			sweepWait("allowed", "s-allowed", poll, t0.Add(10*time.Minute), 9*time.Minute),
			// late: the wait starts long after the block, so it is not the hold.
			sweepWait("late", "s-late", poll, t0.Add(20*time.Minute), 5*time.Minute),
			// no gate verdicts at all: never this class.
			sweepWait("nogate", "s-nogate", poll, t0.Add(10*time.Minute), 9*time.Minute),
		},
	}
	got := detectStopGateWaits(in)
	if len(got) != 1 {
		t.Fatalf("got %d findings, want 1: %+v", len(got), got)
	}
	f := got[0]
	if f.Pattern != sweepStopGateWait || f.Agent != "held" || f.Count != 240 || f.Identity != "s-held" || !f.At.Equal(t0) {
		t.Errorf("finding = %+v", f)
	}
}

// A Stop event the gate did not publish is dropped; a gate verdict goes to
// StopGates, never Hooks, so the tool-call detectors cannot count it.
func TestParseSweepBusLine_StopGate(t *testing.T) {
	line := func(p map[string]any) string {
		b, _ := json.Marshal(map[string]any{"type": "Stop", "ts": "2026-10-30T01:40:11Z", "payload": p})
		return "data:" + string(b)
	}
	if _, _, keep := parseSweepBusLine(line(map[string]any{"session_id": "s", "event": "Stop"})); keep {
		t.Error("a relay Stop without stop_gate_decision was kept")
	}
	ev, _, keep := parseSweepBusLine(line(map[string]any{"session_id": "s", "event": "Stop", "stop_gate_decision": "block", "reason": "r"}))
	if !keep || ev.StopGateDecision != "block" || ev.Reason != "r" || ev.Event != "Stop" {
		t.Fatalf("gate verdict not kept: %+v %v", ev, keep)
	}
	var in sweepInput
	for range 6 {
		in.addBusEvents([]sweepHookEvent{ev})
	}
	if len(in.Hooks) != 0 || len(in.StopGates) != 6 {
		t.Errorf("hooks %d, stop gates %d", len(in.Hooks), len(in.StopGates))
	}
	if fs := detectRepeatCalls(in, defaultSweepThresholds.RepeatMin); len(fs) != 0 {
		t.Errorf("repeat-call counted stop-gate verdicts: %+v", fs)
	}
}

// The hq0 fixture's poll was killed 97 s after it started (03:26:14.918 to the
// session's ended_at 03:27:52): measured from its PreToolUse, under the bar.
func TestSweepStopGateHolds_KilledHq0(t *testing.T) {
	_, in := loadSweepFixture(t, filepath.Join(sweepFixtureDir, "stop-gate-wait", "negative-hq0-crew-ci-poll-after-block"))
	hs := sweepStopGateHolds(in)
	if len(hs) != 1 {
		t.Fatalf("got %d holds, want 1: %+v", len(hs), hs)
	}
	if h := hs[0]; h.Waited.Round(time.Second) != 97*time.Second || h.Waits != 1 || h.Killed != 1 || h.Live || h.Blocks != 1 {
		t.Errorf("hold = %+v, want 97s in 1 killed wait", h)
	}
	if fs := detectStopGateWaits(in); len(fs) != 0 {
		t.Errorf("a 97 s hold is under the bar: %+v", fs)
	}
}

// An open wait (a PreToolUse with no result of the same tool_use_id) is closed
// at the session's ended_at, else closed_at, else the sweep's now.
func TestDetectStopGateWaits_OpenWaits(t *testing.T) {
	t0 := sweepTestNow.Add(-2 * time.Hour)
	poll := "until [ $(gh run list --commit abc --json status -q length) = 0 ]; do sleep 20; done"
	at := func(d time.Duration) string { return t0.Add(d).Format(time.RFC3339) }
	tests := []struct {
		name    string
		session *beadsapi.BeadDetail
		pre     []sweepHookEvent
		hooks   []sweepHookEvent
		now     time.Time
		count   int    // seconds; 0: no finding
		detail  string // in the finding's detail
		waits   int    // waits in the hold
	}{{
		name:    "killed, closed at ended_at",
		session: sweepSessionBead("s", t0.Add(-time.Hour), at(3*time.Minute), at(4*time.Minute)),
		pre:     []sweepHookEvent{sweepPreWait("a", "s", "toolu_k", poll, t0.Add(10*time.Second))},
		count:   170, detail: "1 killed before it returned", waits: 1,
	}, {
		name:    "killed, closed at closed_at",
		session: sweepSessionBead("s", t0.Add(-time.Hour), "", at(4*time.Minute)),
		pre:     []sweepHookEvent{sweepPreWait("a", "s", "toolu_k", poll, t0.Add(10*time.Second))},
		count:   230, detail: "1 killed before it returned", waits: 1,
	}, {
		name:    "live, closed at now",
		session: sweepSessionBead("s", t0.Add(-time.Hour), "", ""),
		pre:     []sweepHookEvent{sweepPreWait("a", "s", "toolu_k", poll, t0.Add(10*time.Second))},
		now:     t0.Add(5 * time.Minute),
		count:   290, detail: "still waiting", waits: 1,
	}, {
		// Paired by tool_use_id: the 90 s wait is counted once, from its
		// PostToolUse, not again as an open wait running to ended_at.
		name:    "returned, counted once",
		session: sweepSessionBead("s", t0.Add(-time.Hour), at(30*time.Minute), ""),
		pre:     []sweepHookEvent{sweepPreWait("a", "s", "toolu_p", poll, t0.Add(10*time.Second))},
		hooks: []sweepHookEvent{func() sweepHookEvent {
			ev := sweepWait("a", "s", poll, t0.Add(100*time.Second), 90*time.Second)
			ev.ToolUseID = "toolu_p"
			return ev
		}()},
		waits: 1,
	}, {
		name:    "failed, counted once",
		session: sweepSessionBead("s", t0.Add(-time.Hour), at(30*time.Minute), ""),
		pre:     []sweepHookEvent{sweepPreWait("a", "s", "toolu_f", poll, t0.Add(10*time.Second))},
		hooks: []sweepHookEvent{func() sweepHookEvent {
			ev := sweepBash("a", "s", poll, "", t0.Add(20*time.Second))
			ev.Event, ev.ToolUseID = "PostToolUseFailure", "toolu_f"
			return ev
		}()},
	}, {
		// Pairing is by tool_use_id only: a wait without one is never open.
		name:    "no tool_use_id",
		session: sweepSessionBead("s", t0.Add(-time.Hour), at(30*time.Minute), ""),
		pre:     []sweepHookEvent{sweepPreWait("a", "s", "", poll, t0.Add(10*time.Second))},
	}, {
		name: "no session bead, end unknown",
		pre:  []sweepHookEvent{sweepPreWait("a", "s", "toolu_k", poll, t0.Add(10*time.Second))},
		now:  t0.Add(time.Hour),
	}, {
		name:    "not a wait",
		session: sweepSessionBead("s", t0.Add(-time.Hour), at(30*time.Minute), ""),
		pre:     []sweepHookEvent{sweepPreWait("a", "s", "toolu_k", "go test ./cmd/gb/", t0.Add(10*time.Second))},
	}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			now := tc.now
			if now.IsZero() {
				now = sweepTestNow
			}
			in := sweepInput{Now: now, StopGates: []sweepHookEvent{sweepGate("a", "s", "block", t0)}, PreTools: tc.pre, Hooks: tc.hooks}
			if tc.session != nil {
				in.Sessions = []*beadsapi.BeadDetail{tc.session}
			}
			var waits int
			for _, h := range sweepStopGateHolds(in) {
				waits += h.Waits
			}
			if waits != tc.waits {
				t.Errorf("waits = %d, want %d", waits, tc.waits)
			}
			got := detectStopGateWaits(in)
			if tc.count == 0 {
				if len(got) != 0 {
					t.Errorf("want no finding, got %+v", got)
				}
				return
			}
			if len(got) != 1 || got[0].Count != tc.count || got[0].Pattern != sweepStopGateWait || !strings.Contains(got[0].Detail, tc.detail) {
				t.Errorf("want one finding of %d s with %q, got %+v", tc.count, tc.detail, got)
			}
		})
	}
}

// A PreToolUse goes to PreTools, never Hooks: every other detector reads
// Hooks as completed calls with duration_ms.
func TestAddBusEvents_PreToolUse(t *testing.T) {
	ev, _, keep := parseSweepBusLine(`data:{"type":"PreToolUse","ts":"2026-10-30T03:26:14Z","payload":{"session_id":"s","tool_name":"Bash","tool_use_id":"toolu_x","tool_input":{"command":"sleep 60"}}}`)
	if !keep || ev.Event != "PreToolUse" || ev.ToolUseID != "toolu_x" {
		t.Fatalf("PreToolUse not kept: %+v %v", ev, keep)
	}
	var in sweepInput
	in.addBusEvents([]sweepHookEvent{ev})
	if len(in.Hooks) != 0 || len(in.PreTools) != 1 {
		t.Errorf("hooks %d, pre-tools %d", len(in.Hooks), len(in.PreTools))
	}
}
