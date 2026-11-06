package main

// stop-gate-wait (survey class, gd-fSdUbOmf3g): an agent ends its turn while a
// background job or subagents run, the stop-gate blocks the stop, and the agent
// then holds the turn with a foreground poll (a gh run loop, tail --pid, sleep)
// instead of ending it and waking on the notification. The block is on the bus
// as a Stop event carrying stop_gate_decision (hook_stopgate_publish.go).

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

const sweepStopGateWait = "stop-gate-wait"

const (
	// sweepStopGateWaitMin is how long a session may hold a blocked turn in
	// foreground waits before it is a finding. One short wait on a test
	// that is about to finish, then real work, is healthy.
	sweepStopGateWaitMin = 2 * time.Minute
	// sweepStopGateWaitLead is how soon after the block (or after the
	// previous wait) a wait must start to count as holding the turn.
	sweepStopGateWaitLead = 2 * time.Minute
)

// sweepPidWait is a foreground wait on one of the agent's own processes,
// which slowCallKind files under other.
var sweepPidWait = regexp.MustCompile(`\btail\s+(?:-\S+\s+)*--pid\b`)

// sweepForegroundWait reports whether a tool call is a foreground wait: a Bash
// call that polls CI or waits on a process.
func sweepForegroundWait(ev sweepHookEvent) bool {
	cmd := sweepCommand(ev)
	if cmd == "" {
		return false
	}
	switch slowCallKind(cmd) {
	case "ci-wait", "wait-on-process":
		return true
	}
	return sweepPidWait.MatchString(cmd)
}

// sweepStopGateHold is how long one session held a blocked turn in foreground
// waits.
type sweepStopGateHold struct {
	SessionID string
	First     sweepHookEvent // the first block the hold followed
	Blocks    int
	Waited    time.Duration
	Waits     int
	Longest   sweepHookEvent
	Killed    int  // waits that never returned and were closed at the session's end
	Live      bool // a wait was still running when the sweep ran
}

// sweepWaitSpan is one foreground wait: a completed call measured from its
// PostToolUse duration_ms, or an open one (a PreToolUse with no result).
type sweepWaitSpan struct {
	ev         sweepHookEvent
	start, end time.Time
	open, live bool
}

// sweepOpenWaitEnd is when an open wait that started at ts in session sid
// stopped: the session bead's ended_at, else its closed_at, else the sweep's
// now (live: the session is still running). ok is false when no session bead
// covers ts, since the wait's end is then unknown.
func sweepOpenWaitEnd(in sweepInput, sid string, ts time.Time) (end time.Time, live, ok bool) {
	for _, b := range in.Sessions {
		if sweepHookSession(b) != sid {
			continue
		}
		start, ended := sweepSessionSpan(b)
		if ended.IsZero() {
			ended = parseSweepTime(b.ClosedAt)
		}
		if (!start.IsZero() && ts.Before(start)) || (!ended.IsZero() && ended.Before(ts)) {
			continue
		}
		if ended.IsZero() || (!in.Now.IsZero() && ended.After(in.Now)) {
			return in.Now, true, true
		}
		return ended, false, true
	}
	return time.Time{}, false, false
}

// sweepStopGateHolds measures, per session with a stop-gate block, the
// foreground waits that held the blocked turn: each wait starting within
// sweepStopGateWaitLead of a block (or of the previous wait's end) while the
// latest verdict is a block. A PreToolUse of a wait with no PostToolUse or
// PostToolUseFailure of the same tool_use_id is an open wait, closed at the
// session's end (gd-c6q4LOONzY). Pairing is by tool_use_id only, so a wait
// that returned is counted once, from its PostToolUse.
func sweepStopGateHolds(in sweepInput) []sweepStopGateHold {
	gates := map[string][]sweepHookEvent{}
	for _, g := range in.StopGates {
		gates[g.SessionID] = append(gates[g.SessionID], g)
	}
	waits := map[string][]sweepWaitSpan{}
	done := map[string]bool{}
	for _, ev := range in.Hooks {
		if ev.ToolUseID != "" {
			done[ev.SessionID+"\x00"+ev.ToolUseID] = true
		}
		if len(gates[ev.SessionID]) > 0 && ev.DurationMs > 0 && sweepForegroundWait(ev) {
			waits[ev.SessionID] = append(waits[ev.SessionID],
				sweepWaitSpan{ev: ev, start: ev.TS.Add(-time.Duration(ev.DurationMs) * time.Millisecond), end: ev.TS})
		}
	}
	for _, ev := range in.PreTools {
		if ev.ToolUseID == "" || done[ev.SessionID+"\x00"+ev.ToolUseID] || len(gates[ev.SessionID]) == 0 || !sweepForegroundWait(ev) {
			continue
		}
		end, live, ok := sweepOpenWaitEnd(in, ev.SessionID, ev.TS)
		if !ok || !end.After(ev.TS) {
			continue
		}
		waits[ev.SessionID] = append(waits[ev.SessionID], sweepWaitSpan{ev: ev, start: ev.TS, end: end, open: true, live: live})
	}
	sessions := make([]string, 0, len(waits))
	for sid := range waits {
		sessions = append(sessions, sid)
	}
	sort.Strings(sessions)
	var out []sweepStopGateHold
	for _, sid := range sessions {
		gs, ws := gates[sid], waits[sid]
		sort.SliceStable(gs, func(i, j int) bool { return gs[i].TS.Before(gs[j].TS) })
		sort.SliceStable(ws, func(i, j int) bool { return ws[i].start.Before(ws[j].start) })
		var (
			h       = sweepStopGateHold{SessionID: sid}
			longest time.Duration
			first   *sweepHookEvent
			blocks  = map[time.Time]bool{}
			gi      int
			verdict *sweepHookEvent
			anchor  time.Time
		)
		for _, w := range ws {
			for gi < len(gs) && !gs[gi].TS.After(w.start) {
				verdict = &gs[gi]
				anchor = gs[gi].TS
				gi++
			}
			if verdict == nil || verdict.StopGateDecision != stopGateDecisionBlock || w.start.Sub(anchor) > sweepStopGateWaitLead {
				continue
			}
			d := w.end.Sub(w.start)
			h.Waited += d
			h.Waits++
			if d > longest {
				longest, h.Longest = d, w.ev
			}
			if w.live {
				h.Live = true
			} else if w.open {
				h.Killed++
			}
			if first == nil {
				first = verdict
			}
			blocks[verdict.TS] = true
			anchor = w.end
		}
		if first == nil {
			continue
		}
		h.First, h.Blocks = *first, len(blocks)
		out = append(out, h)
	}
	return out
}

// detectStopGateWaits flags a session that, after a stop-gate block, spent
// sweepStopGateWaitMin or more in foreground waits before its turn was
// allowed to end. Count is the seconds waited.
func detectStopGateWaits(in sweepInput) []sweepFinding {
	var out []sweepFinding
	for _, h := range sweepStopGateHolds(in) {
		if h.Waited < sweepStopGateWaitMin {
			continue
		}
		reason := h.First.Reason
		if reason == "" {
			reason = "no reason given"
		}
		var tail string
		if h.Killed > 0 {
			tail += fmt.Sprintf(", %d killed before it returned", h.Killed)
		}
		if h.Live {
			tail += ", still waiting"
		}
		out = append(out, sweepFinding{
			Pattern: sweepStopGateWait, Agent: h.First.Agent, AgentBeadID: h.First.AgentBeadID, Sessions: []string{h.SessionID},
			Count: int(h.Waited / time.Second), At: h.First.TS, Identity: h.SessionID,
			Detail: fmt.Sprintf("blocked %d time(s) (first %s: %s), then held the turn %s in %d foreground wait(s)%s, longest: %s",
				h.Blocks, h.First.TS.UTC().Format("15:04:05Z"), truncateRunes(reason, 80), h.Waited.Round(time.Second), h.Waits, tail,
				truncateRunes(strings.Join(strings.Fields(sweepCommand(h.Longest)), " "), 120)),
		})
	}
	return out
}
