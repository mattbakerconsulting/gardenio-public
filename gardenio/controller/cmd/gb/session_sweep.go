package main

// gb session sweep — a repeatable fleet-wide pass over the last day of session
// and agent beads that flags slow and bad agent patterns (plan gd-xV14ol0GhR,
// task gd-S6ce5ycn45). The founding case is crew-gardenio-infra-5juu: an agent
// whose own token 403'd on its bead, so it re-ran a finished task every ~10
// minutes for hours and nobody noticed.
//
// Inputs, all read-only and fetched once:
//   - session beads created in the window (exit_code, restart_cause,
//     slowest_calls, started_at),
//   - agent beads updated in the window (wrapup, task_id, last_coop_exit_code),
//   - a bounded replay of the HOOK_EVENTS bus (per-call tool_input and
//     tool_response, keyed by session_id; the bus keeps only a few hours, so
//     the report states the window it actually covered),
//   - comments on the agents' task beads.
//
// The detectors are pure functions over those inputs (sweepDetect), so the
// thresholds are unit-tested without a daemon. --file turns the report into at
// most one open bug per pattern, labelled session-sweep:<pattern>: an existing
// open bug gets a comment listing only evidence it has not seen before, so the
// gardener can run this every cycle without filing duplicates.

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
)

var sessionSweepCmd = &cobra.Command{
	Use:   "sweep",
	Short: "Flag slow and bad agent-session patterns across the fleet (restart loops, 401/403 storms, OOMs, ...)",
	Long: `Sweep the last day of session and agent beads (plus a bounded replay of the
HOOK_EVENTS bus) for patterns that waste pods, tokens and operator attention:

  restart-loop   more than --restart-max sessions for one agent bead inside any hour
  auth-storm     --auth-min or more kd/gb calls in one session answered
                 "HTTP 401:"/"HTTP 403:" that touch the agent's own bead or task
                 (or a gb lifecycle verb: stop, yield, prime, decision, ...)
  repeat-call    the same tool call (tool + input) --repeat-min or more times
                 in one session
  slow-call      a tool call (slowest_calls, or the bus's duration_ms) at or
                 over --slow (--wait-slow for wait-on-process), grouped by kind
                 (ci-wait, wait-on-process, goldens, build-test, other)
  oom            a session exit_code 137 (or an OOM restart_cause / agent
                 last_coop_exit_code 137)
  dup-comment    one agent posting the same comment text twice or more on one
                 task inside the window
  harness-gap    a wrap-up whose harness_gaps names a missing tool, command,
                 subcommand or binary

Survey classes (gardenio/docs/session-sweep-survey.md), from the bus unless
noted: gh-token-scope (also wrap-ups), ownership-403, infra-access-gap,
browser-deps-eacces, pkill-self-match, tmpdir-unset, wrapup-quoting,
stop-refused-schedule, retrigger-churn, bead-reread; and, joining session beads
to the bus, stop-did-not-take, killed-mid-task, interrupt-then-crash; and
noop-followthrough and dispatch-collision (agent wrap-ups); and stop-gate-wait (a
stop-gate block followed by 2+ min of foreground waits, from the gate's Stop
events).

Read-only by default. --file upserts one open bug per pattern (label
session-sweep:<pattern>) under --parent, commenting only evidence the bug has
not seen yet, so it is safe to run every gardener cycle.

Recipe and false-positive notes: gardenio/docs/session-sweep.md.`,
	RunE: runSessionSweep,
}

var (
	sweepSince      time.Duration
	sweepRestartMax int
	sweepAuthMin    int
	sweepRepeatMin  int
	sweepSlow       time.Duration
	sweepWaitSlow   time.Duration
	sweepBusTimeout time.Duration
	sweepNoBus      bool
	sweepFile       bool
	sweepParent     string
	sweepProject    string
)

func init() {
	sessionCmd.AddCommand(sessionSweepCmd)
	f := sessionSweepCmd.Flags()
	f.DurationVar(&sweepSince, "since", 24*time.Hour, "look back this far")
	f.IntVar(&sweepRestartMax, "restart-max", defaultSweepThresholds.RestartMax, "restart-loop: flag an agent bead with MORE than this many sessions inside one hour")
	f.IntVar(&sweepAuthMin, "auth-min", defaultSweepThresholds.AuthMin, "auth-storm: flag a session with at least this many own-bead 401/403s")
	f.IntVar(&sweepRepeatMin, "repeat-min", defaultSweepThresholds.RepeatMin, "repeat-call: flag a tool call made at least this many times in one session")
	f.DurationVar(&sweepSlow, "slow", defaultSweepThresholds.Slow, "slow-call: flag a tool call at or over this duration")
	f.DurationVar(&sweepWaitSlow, "wait-slow", defaultSweepThresholds.WaitSlow, "slow-call: the lower bar for wait-on-process calls (foreground waits on the agent's own job)")
	f.DurationVar(&sweepBusTimeout, "bus-timeout", 30*time.Second, "upper bound on the HOOK_EVENTS replay (it stops earlier once caught up)")
	f.BoolVar(&sweepNoBus, "no-bus", false, "skip the HOOK_EVENTS replay (the bus-only patterns then report nothing)")
	f.BoolVar(&sweepFile, "file", false, "file or update one open bug per pattern with the evidence")
	f.StringVar(&sweepParent, "parent", "", "with --file: parent-child link new bugs under this bead (e.g. the plan epic)")
	f.StringVar(&sweepProject, "project", "", "with --file: project label for new bugs (default $BOAT_PROJECT, else gardenio)")
}

// Pattern keys. Each is one bug when --file is set.
const (
	sweepRestartLoop = "restart-loop"
	sweepAuthStorm   = "auth-storm"
	sweepRepeatCall  = "repeat-call"
	sweepSlowCall    = "slow-call"
	sweepOOM         = "oom"
	sweepDupComment  = "dup-comment"
	sweepHarnessGap  = "harness-gap"
)

// sweepPatternOrder fixes the report and filing order.
var sweepPatternOrder = []string{
	sweepRestartLoop, sweepAuthStorm, sweepOOM, sweepRepeatCall, sweepDupComment, sweepSlowCall, sweepHarnessGap,
	sweepGHTokenScope, sweepOwnership403, sweepInfraAccessGap, sweepBrowserDeps, sweepPkillSelf, sweepTmpdirUnset,
	sweepWrapupQuoting, sweepStopRefusedSched, sweepRetriggerChurn, sweepBeadReread,
	sweepStopNotTaken, sweepKilledMid, sweepInterruptCrash, sweepNoopFollow,
	sweepDispatchCollision, sweepStopGateWait,
}

var sweepPatternTitles = map[string]string{
	sweepRestartLoop: "agents restart more than the hourly limit (restart loop)",
	sweepAuthStorm:   "sessions hit repeated 401/403s on their own bead or task",
	sweepRepeatCall:  "sessions repeat the same tool call many times",
	sweepSlowCall:    "tool calls run longer than the slow threshold",
	sweepOOM:         "sessions exit OOM-killed (137)",
	sweepDupComment:  "agents post duplicate comments on one task",
	sweepHarnessGap:  "wrap-ups name a missing tool or command",

	sweepGHTokenScope:     "the GitHub token cannot read check runs or re-run jobs",
	sweepOwnership403:     "agents are refused writes to their own task (ownership denied)",
	sweepInfraAccessGap:   "the infra reader ServiceAccount is refused a resource a check needs",
	sweepBrowserDeps:      "browser deps install fails with EACCES on /ms-playwright",
	sweepPkillSelf:        "pkill -f kills the agent's own shell (exit 144)",
	sweepTmpdirUnset:      "writes to $TMPDIR land at / (TMPDIR unset)",
	sweepWrapupQuoting:    "inline gb stop --wrapup JSON breaks shell quoting",
	sweepStopRefusedSched: "scheduled runs' gb stop is refused for their own schedule bead",
	sweepRetriggerChurn:   "sessions push a merge of main or an empty commit 3+ times to re-run CI",
	sweepBeadReread:       "sessions re-read the same bead with kd show 4+ times",
	sweepStopNotTaken:     "sessions idle 30+ min after their last call (gb stop did not take)",
	sweepKilledMid:        "sessions killed mid-task by a credential switch or crash",
	sweepInterruptCrash:   "sessions exit 143 right after a user interrupt",
	sweepNoopFollow:       "escalation/decision wakes find nothing left to do",
	sweepStopGateWait:     "blocked stops are held with a foreground wait",

	sweepDispatchCollision: "agents are dispatched to work another agent's PR already did",
}

var sweepPatternShortTitles = map[string]string{
	sweepRestartLoop: "fix agent restart loops",
	sweepAuthStorm:   "stop own-bead 401/403 storms",
	sweepRepeatCall:  "stop sessions repeating one tool call",
	sweepSlowCall:    "speed up tool calls over the slow limit",
	sweepOOM:         "stop OOM-killed agent sessions",
	sweepDupComment:  "stop duplicate comments on tasks",
	sweepHarnessGap:  "supply tools wrap-ups say are missing",

	sweepGHTokenScope:     "let the GitHub token read checks, re-run",
	sweepOwnership403:     "let agents update their own task",
	sweepInfraAccessGap:   "close infra reader RBAC gaps",
	sweepBrowserDeps:      "fix /ms-playwright EACCES in agent pods",
	sweepPkillSelf:        "stop pkill -f killing the agent shell",
	sweepTmpdirUnset:      "set TMPDIR in agent pods",
	sweepWrapupQuoting:    "stop inline wrap-up JSON quoting breaks",
	sweepStopRefusedSched: "let scheduled runs stop with their claim",
	sweepRetriggerChurn:   "cut CI retrigger churn",
	sweepBeadReread:       "stop repeated kd show of one bead",
	sweepStopNotTaken:     "make a failed gb stop end the pod",
	sweepKilledMid:        "stop credential switches killing work",
	sweepInterruptCrash:   "stop interrupts crashing agent sessions",
	sweepNoopFollow:       "skip wakes for already-finished tasks",
	sweepStopGateWait:     "end blocked turns instead of polling",

	sweepDispatchCollision: "stop dispatching work a PR already did",
}

// sweepFinding is one piece of evidence for a pattern.
type sweepFinding struct {
	Pattern     string    `json:"pattern"`
	Kind        string    `json:"kind,omitempty"` // slow-call kind or harness-gap subject
	Agent       string    `json:"agent"`
	AgentBeadID string    `json:"agent_bead_id,omitempty"`
	Sessions    []string  `json:"sessions,omitempty"` // session bead ids or coop session uuids
	BeadID      string    `json:"bead_id,omitempty"`  // the bead the pattern touched (task, comment target)
	Count       int       `json:"count"`
	At          time.Time `json:"at"`
	Detail      string    `json:"detail"`
	// Identity is what makes this finding the same evidence from one sweep to
	// the next: each detector sets it from stable ids only (a session id, a
	// tool + input, a comment's text), never from a count or anything else
	// the window can grow or shrink.
	Identity string `json:"-"`
}

// key identifies a finding across sweeps, so --file comments only new evidence.
// It hashes Pattern, Kind, Agent and Identity only. Count, Detail, Sessions
// and BeadID are left out on purpose: they carry live counts and lists that
// drift between sweeps (the bus replay is lossy, a restart loop's window
// shifts as old sessions age out), and the same evidence must keep its key.
func (f sweepFinding) key() string {
	h := sha256.Sum256([]byte(strings.Join([]string{f.Pattern, f.Kind, f.Agent, f.Identity}, "\x00")))
	return hex.EncodeToString(h[:6])
}

// sweepHookEvent is the subset of one relayed HOOK_EVENTS payload the sweep
// reads: a PostToolUse/PostToolUseFailure, or a stop-gate verdict (Stop).
type sweepHookEvent struct {
	Agent        string          `json:"agent"`
	AgentBeadID  string          `json:"agent_bead_id"`
	SessionID    string          `json:"session_id"`
	Event        string          `json:"event"`
	ToolName     string          `json:"tool_name"`
	ToolInput    json.RawMessage `json:"tool_input"`
	ToolResponse json.RawMessage `json:"tool_response"`
	ToolUseID    string          `json:"tool_use_id"`
	DurationMs   int64           `json:"duration_ms"` // PostToolUse only
	Error        string          `json:"error"`
	TS           time.Time       `json:"ts"`
	// StopGateDecision ("block"/"allow") and Reason are set on the Stop
	// events gb hook stop-gate publishes (gd-fSdUbOmf3g).
	StopGateDecision string `json:"stop_gate_decision"`
	Reason           string `json:"reason"`

	// ToolResponseExcerpt is the error lines and last line the relay kept
	// from past tool_response's 1024-byte cut (gd-YaYOmNpbnW).
	ToolResponseExcerpt string `json:"tool_response_excerpt"`
}

type sweepInput struct {
	Now      time.Time
	Since    time.Duration
	Sessions []*beadsapi.BeadDetail
	Agents   []*beadsapi.BeadDetail
	Hooks    []sweepHookEvent
	// StopGates are the stop-gate's verdicts from the bus, kept apart from
	// Hooks so the tool-call detectors never see them.
	StopGates []sweepHookEvent
	// PreTools are the PreToolUse events, kept apart from Hooks (completed
	// calls with duration_ms): stop-gate-wait pairs them by tool_use_id to
	// find a wait that was killed before it returned (gd-c6q4LOONzY).
	PreTools []sweepHookEvent
	// Comments maps a task bead id to its comments.
	Comments map[string][]beadsapi.Comment
}

type sweepThresholds struct {
	RestartMax int
	AuthMin    int
	RepeatMin  int
	Slow       time.Duration
	// WaitSlow is the lower slow-call bar for wait-on-process calls: a
	// foreground loop waiting on the agent's own background job is pure idle
	// time from its fifth minute (survey class idle-wait).
	WaitSlow time.Duration
}

// defaultSweepThresholds are the flag defaults; the fixture test runs with them.
var defaultSweepThresholds = sweepThresholds{RestartMax: 3, AuthMin: 3, RepeatMin: 5, Slow: 10 * time.Minute, WaitSlow: 5 * time.Minute}

type sweepReport struct {
	Now           time.Time      `json:"now"`
	Since         string         `json:"since"`
	Sessions      int            `json:"sessions"`
	Agents        int            `json:"agents"`
	HookEvents    int            `json:"hook_events"`
	HookFrom      *time.Time     `json:"hook_from,omitempty"` // earliest replayed event: auth-storm/repeat-call cover only from here
	HookTo        *time.Time     `json:"hook_to,omitempty"`   // latest replayed event
	TasksScanned  int            `json:"tasks_scanned"`
	ElapsedMs     int64          `json:"elapsed_ms"`
	Counts        map[string]int `json:"counts"`
	Findings      []sweepFinding `json:"findings"`
	Filed         []sweepFiled   `json:"filed,omitempty"`
	FetchWarnings []string       `json:"fetch_warnings,omitempty"`
}

type sweepFiled struct {
	Pattern string `json:"pattern"`
	BeadID  string `json:"bead_id"`
	Action  string `json:"action"` // created | commented | unchanged
	New     int    `json:"new"`
}

func runSessionSweep(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	start := time.Now()
	in, warnings, err := fetchSweepInput(ctx, start.UTC(), sweepSince)
	if err != nil {
		return err
	}
	th := sweepThresholds{RestartMax: sweepRestartMax, AuthMin: sweepAuthMin, RepeatMin: sweepRepeatMin, Slow: sweepSlow, WaitSlow: sweepWaitSlow}
	findings := sweepDetect(in, th)

	rep := sweepReport{
		Now: in.Now, Since: sweepSince.String(),
		Sessions: len(in.Sessions), Agents: len(in.Agents), HookEvents: len(in.Hooks), TasksScanned: len(in.Comments),
		Counts: map[string]int{}, Findings: findings, FetchWarnings: warnings,
	}
	if len(in.Hooks) > 0 {
		from, to := in.Hooks[0].TS, in.Hooks[len(in.Hooks)-1].TS
		rep.HookFrom, rep.HookTo = &from, &to
	}
	for _, f := range findings {
		rep.Counts[f.Pattern]++
	}
	if sweepFile {
		filed, err := fileSweepFindings(ctx, rep)
		rep.Filed = filed
		if err != nil {
			rep.ElapsedMs = time.Since(start).Milliseconds()
			printSweepReport(cmd.OutOrStdout(), rep)
			return err
		}
	}
	rep.ElapsedMs = time.Since(start).Milliseconds()

	if jsonOutput {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	printSweepReport(cmd.OutOrStdout(), rep)
	return nil
}

// fetchSweepInput loads everything the detectors read. The bead lists and the
// bus replay run concurrently; comment fetches fan out over the agents' tasks.
// A failing bus replay or comment fetch degrades to a warning (those detectors
// then see nothing) rather than failing the whole sweep.
func fetchSweepInput(ctx context.Context, now time.Time, since time.Duration) (sweepInput, []string, error) {
	in := sweepInput{Now: now, Since: since, Comments: map[string][]beadsapi.Comment{}}
	cutoff := now.Add(-since)
	var warnings []string
	var mu sync.Mutex
	warn := func(format string, a ...any) {
		mu.Lock()
		warnings = append(warnings, fmt.Sprintf(format, a...))
		mu.Unlock()
	}

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		res, err := daemon.ListAllBeadsFiltered(gctx, beadsapi.ListBeadsQuery{Types: []string{"session"}, CreatedAfter: cutoff})
		if err != nil {
			return fmt.Errorf("listing session beads: %w", err)
		}
		in.Sessions = res.Beads
		return nil
	})
	g.Go(func() error {
		res, err := daemon.ListAllBeadsFiltered(gctx, beadsapi.ListBeadsQuery{Types: []string{"agent"}, UpdatedAfter: cutoff})
		if err != nil {
			return fmt.Errorf("listing agent beads: %w", err)
		}
		in.Agents = res.Beads
		return nil
	})
	if !sweepNoBus {
		g.Go(func() error {
			evs, err := replaySweepHookEvents(gctx, cutoff, now, sweepBusTimeout)
			if err != nil {
				warn("HOOK_EVENTS replay: %v (auth-storm and repeat-call skipped)", err)
			}
			in.addBusEvents(evs)
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return in, warnings, err
	}

	tasks := map[string]bool{}
	for _, a := range in.Agents {
		if t := a.Fields["task_id"]; t != "" {
			tasks[t] = true
		}
	}
	ids := make([]string, 0, len(tasks))
	for t := range tasks {
		ids = append(ids, t)
	}
	sort.Strings(ids)
	cg, cctx := errgroup.WithContext(ctx)
	cg.SetLimit(4)
	var failed int
	var firstErr error
	for _, id := range ids {
		cg.Go(func() error {
			cs, err := daemon.GetComments(cctx, id)
			if err != nil {
				// One retry: a burst of reads occasionally trips a transient
				// daemon error; a bead that stays unreadable is skipped.
				select {
				case <-cctx.Done():
				case <-time.After(time.Second):
					cs, err = daemon.GetComments(cctx, id)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failed++
				if firstErr == nil {
					firstErr = err
				}
				return nil
			}
			in.Comments[id] = cs
			return nil
		})
	}
	_ = cg.Wait()
	if failed > 0 {
		warn("comments: %d of %d task beads unreadable, dup-comment skips them (first: %v)", failed, len(ids), firstErr)
	}
	return in, warnings, nil
}

// replaySweepHookEvents replays the HOOK_EVENTS bus from cutoff and keeps the
// PreToolUse/PostToolUse/PostToolUseFailure payloads and stop-gate verdicts. The endpoint replays history and
// then tails live with no "caught up" marker, so the read stops at the first
// event stamped at or after `now`, after a quiet gap, or at the timeout.
func replaySweepHookEvents(ctx context.Context, cutoff, now time.Time, timeout time.Duration) ([]sweepHookEvent, error) {
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	base := strings.TrimRight(httpURL, "/")
	reqURL := fmt.Sprintf("%s/v1/bus/events?stream=hooks&since=%s", base, url.QueryEscape(cutoff.UTC().Format(time.RFC3339)))
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	tok := tokenFlag
	if tok == "" {
		tok = defaultToken()
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if rctx.Err() != nil {
			return nil, nil
		}
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	lines := make(chan string, 256)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
		for sc.Scan() {
			select {
			case lines <- sc.Text():
			case <-rctx.Done():
				return
			}
		}
	}()

	const quiet = 3 * time.Second
	idle := time.NewTimer(quiet)
	defer idle.Stop()
	var out []sweepHookEvent
	for {
		select {
		case <-rctx.Done():
			return out, nil
		case <-idle.C:
			return out, nil
		case line, ok := <-lines:
			if !ok {
				return out, nil
			}
			idle.Reset(quiet)
			ev, ts, keep := parseSweepBusLine(line)
			if !ts.IsZero() && !ts.Before(now) {
				return out, nil
			}
			if keep {
				out = append(out, ev)
			}
		}
	}
}

// sweepBusEventTypes are the bus event types the sweep keeps. A Stop is kept
// only when the stop-gate published it (it carries stop_gate_decision).
var sweepBusEventTypes = map[string]bool{"PreToolUse": true, "PostToolUse": true, "PostToolUseFailure": true, "Stop": true}

// addBusEvents files bus events: stop-gate verdicts in StopGates, tool-call
// starts in PreTools, completed tool calls in Hooks.
func (in *sweepInput) addBusEvents(evs []sweepHookEvent) {
	for _, ev := range evs {
		switch ev.Event {
		case "Stop":
			in.StopGates = append(in.StopGates, ev)
		case "PreToolUse":
			in.PreTools = append(in.PreTools, ev)
		default:
			in.Hooks = append(in.Hooks, ev)
		}
	}
}

// parseSweepBusLine decodes one SSE line. It returns the event's timestamp for
// any bus event (so the caller can detect it has caught up) and keep=true only
// for a tool-call start or result or a stop-gate verdict.
func parseSweepBusLine(line string) (sweepHookEvent, time.Time, bool) {
	data, ok := strings.CutPrefix(line, "data:")
	if !ok {
		return sweepHookEvent{}, time.Time{}, false
	}
	var evt struct {
		Type    string         `json:"type"`
		TS      time.Time      `json:"ts"`
		Payload sweepHookEvent `json:"payload"`
	}
	if json.Unmarshal([]byte(data), &evt) != nil {
		return sweepHookEvent{}, time.Time{}, false
	}
	if !sweepBusEventTypes[evt.Type] || (evt.Type == "Stop" && evt.Payload.StopGateDecision == "") {
		return sweepHookEvent{}, evt.TS, false
	}
	evt.Payload.Event = evt.Type
	if evt.Payload.TS.IsZero() {
		evt.Payload.TS = evt.TS
	}
	return evt.Payload, evt.TS, evt.Payload.SessionID != ""
}

// sweepDetect runs every detector and returns findings in pattern order, then
// by descending count.
func sweepDetect(in sweepInput, th sweepThresholds) []sweepFinding {
	var out []sweepFinding
	out = append(out, detectRestartLoops(in, th.RestartMax)...)
	out = append(out, detectAuthStorms(in, th.AuthMin)...)
	out = append(out, detectOOMs(in)...)
	out = append(out, detectRepeatCalls(in, th.RepeatMin)...)
	out = append(out, detectDupComments(in)...)
	out = append(out, detectSlowCalls(in, th)...)
	out = append(out, detectHarnessGaps(in)...)
	out = append(out, detectHookSignatures(in)...)
	out = append(out, detectRetriggerChurn(in)...)
	out = append(out, detectTokenScopeWrapups(in)...)
	out = append(out, detectOwnership403s(in)...)
	out = append(out, detectBeadRereads(in)...)
	out = append(out, detectStopsNotTaken(in)...)
	out = append(out, detectKilledMidTask(in)...)
	out = append(out, detectInterruptCrashes(in)...)
	out = append(out, detectNoopFollowthroughs(in)...)
	out = append(out, detectStopGateWaits(in)...)
	out = append(out, detectDispatchCollisions(in)...)
	rank := map[string]int{}
	for i, p := range sweepPatternOrder {
		rank[p] = i
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Pattern != out[j].Pattern {
			return rank[out[i].Pattern] < rank[out[j].Pattern]
		}
		return out[i].Count > out[j].Count
	})
	return out
}

func parseSweepTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

// detectRestartLoops flags an agent bead with more than max sessions starting
// inside any one-hour window. It groups by agent bead, not agent name: a
// gardener or nursery agent woken for each decision, escalation or schedule
// gets a fresh agent bead per wake, and the survey's three false positives
// (epfq, 911c-nursery-backfill-6wz, close-the-orphans-zm8) were such wakes,
// at most 3 an hour per bead. A real loop (5juu, bi5m, vdzc: a stop that never
// lands, so the pod resumes the same bead every ~10 minutes) stays on one.
//
// Only sessions that follow an unclean end count toward max: a session whose
// predecessor on the same bead ended on a gb stop or gb yield that succeeded
// is a stop -> reply -> resume cycle, not a restart (gd-fKRCOO3O8h). A session
// with a restart_cause always counts. One whose predecessor left no gb stop /
// gb yield on the bus counts only when the predecessor started inside the
// replay: before the replay's first event a missing stop is unknown, not
// unclean, and parked crews woken several times an hour read as loops there
// (gd-QZe4Dmf8a4). An empty replay sets no window, so every missing stop counts.
func detectRestartLoops(in sweepInput, max int) []sweepFinding {
	type sess struct {
		id    string
		start time.Time
		cause string
	}
	byBead := map[string][]sess{}
	agentOf := map[string]string{}
	for _, b := range in.Sessions {
		a := b.Fields["agent"]
		t := parseSweepTime(b.Fields["started_at"])
		if a == "" || t.IsZero() {
			continue
		}
		key := b.Fields["agent_bead_id"]
		if key == "" {
			key = "agent:" + a
		}
		byBead[key] = append(byBead[key], sess{b.ID, t, b.Fields["restart_cause"]})
		agentOf[key] = a
	}
	stops := sweepStopCalls(in)
	busFrom := sweepBusFrom(in)
	var out []sweepFinding
	for key, all := range byBead {
		a := agentOf[key]
		bead := strings.TrimPrefix(key, "agent:")
		if bead == a {
			bead = ""
		}
		sort.Slice(all, func(i, j int) bool { return all[i].start.Before(all[j].start) })
		var ss []sess
		var clean, unknown int
		for i, s := range all {
			if i > 0 && s.cause == "" {
				prev := all[i-1].start
				last := sweepLastStop(stops[key], prev, s.start)
				if last != nil && sweepStopSucceeded(*last) {
					clean++
					continue
				}
				if last == nil && prev.Before(busFrom) {
					unknown++
					continue
				}
			}
			ss = append(ss, s)
		}
		bestN, bestI, j := 0, 0, 0
		for i := range ss {
			for ss[i].start.Sub(ss[j].start) > time.Hour {
				j++
			}
			if n := i - j + 1; n > bestN {
				bestN, bestI = n, i
			}
		}
		if bestN <= max {
			continue
		}
		win := ss[bestI-bestN+1 : bestI+1]
		causes := map[string]int{}
		var ids []string
		for _, s := range win {
			ids = append(ids, s.id)
			c := s.cause
			if c == "" {
				c = "none"
			}
			causes[c]++
		}
		out = append(out, sweepFinding{
			Pattern: sweepRestartLoop, Agent: a, AgentBeadID: bead, Sessions: ids, Count: bestN,
			// One loop per agent bead: the busiest hour's first session moves
			// forward as old sessions age out of --since, so it is not stable.
			Identity: key,
			At:       win[0].start,
			Detail: fmt.Sprintf("%d sessions in one hour from %s (%d in window, %d after a clean stop and %d after a predecessor before the bus replay not counted); restart causes %s",
				bestN, win[0].start.Format("15:04Z"), len(all), clean, unknown, formatCounts(causes)),
		})
	}
	return out
}

// sweepStopOrYieldCall is a gb stop or gb yield in command position.
var sweepStopOrYieldCall = regexp.MustCompile(`(?:^|[;&|(\n]\s*)gb\s+(?:stop|yield)\b`)

// sweepStopCalls indexes the completed gb stop / gb yield calls on the bus by
// restart-loop key (agent bead id, else "agent:"+name), in time order.
// Session beads carry no per-session hook id (session_log is "s3:<agent
// bead>"), so a call is tied to its session by time, not by session id.
func sweepStopCalls(in sweepInput) map[string][]sweepHookEvent {
	out := map[string][]sweepHookEvent{}
	for _, ev := range in.Hooks {
		if !sweepStopOrYieldCall.MatchString(sweepCommand(ev)) {
			continue
		}
		key := ev.AgentBeadID
		if key == "" {
			key = "agent:" + ev.Agent
		}
		out[key] = append(out[key], ev)
	}
	for _, evs := range out {
		sort.SliceStable(evs, func(i, j int) bool { return evs[i].TS.Before(evs[j].TS) })
	}
	return out
}

// sweepLastStop returns the last gb stop / gb yield call in [from, to), or nil.
func sweepLastStop(stops []sweepHookEvent, from, to time.Time) *sweepHookEvent {
	var last *sweepHookEvent
	for i := range stops {
		if t := stops[i].TS; !t.Before(from) && t.Before(to) {
			last = &stops[i]
		}
	}
	return last
}

// sweepStopSucceeded reports whether a gb stop / gb yield call completed (not
// PostToolUseFailure) and was not answered 401/403.
func sweepStopSucceeded(ev sweepHookEvent) bool {
	return ev.Event == "PostToolUse" && !sweepAuthDenied.MatchString(sweepResponseText(ev))
}

// sweepBusFrom is the timestamp of the earliest replayed bus event, or the
// zero time when the replay is empty (no-bus, or it failed).
func sweepBusFrom(in sweepInput) time.Time {
	var from time.Time
	for _, evs := range [][]sweepHookEvent{in.Hooks, in.PreTools, in.StopGates} {
		for _, ev := range evs {
			if !ev.TS.IsZero() && (from.IsZero() || ev.TS.Before(from)) {
				from = ev.TS
			}
		}
	}
	return from
}

func formatCounts(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s=%d", k, m[k])
	}
	return strings.Join(parts, " ")
}

var (
	// sweepAuthDenied matches the kd and gb client error shape for a 401/403
	// ("HTTP 403: token not authorized for this bead's project").
	sweepAuthDenied = regexp.MustCompile(`HTTP 40[13]: `)
	// sweepRunsKdGb matches a shell command that invokes kd or gb itself (not a
	// grep for those words): at the start, after a separator, or in $( ).
	sweepRunsKdGb = regexp.MustCompile(`(?:^|[;&|(\n]|\$\()\s*(?:[A-Z_]+=\S*\s+)*(?:kd|gb)\s`)
	// sweepSelfVerb matches gb verbs that act on the caller's own agent bead.
	sweepSelfVerb = regexp.MustCompile(`\bgb\s+(?:stop|yield|prime|decision|escalation|squawk|gate|wrapup)\b`)
)

// sweepCommand returns the Bash command of a hook event, or "". The relay cuts
// a tool_input over 1024 bytes to a string of its JSON prefix; "command" sorts
// first, so that string starts {"command":" and the text after it is the
// command's head (still JSON-escaped), which is what the detectors match on.
func sweepCommand(ev sweepHookEvent) string {
	if ev.ToolName != "Bash" {
		return ""
	}
	var in struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(ev.ToolInput, &in) == nil {
		return in.Command
	}
	var cut string
	if json.Unmarshal(ev.ToolInput, &cut) == nil {
		if rest, ok := strings.CutPrefix(cut, `{"command":"`); ok {
			return strings.NewReplacer(`\"`, `"`, `\n`, "\n", `\u0026`, "&", `\u003c`, "<", `\u003e`, ">", `\\`, `\`).Replace(rest)
		}
	}
	return ""
}

// sweepResponseText flattens a tool_response (a string, or Bash's
// {stdout,stderr} object), the excerpt kept from past its cut and a failure's
// error into one string.
func sweepResponseText(ev sweepHookEvent) string {
	var b strings.Builder
	var s string
	if json.Unmarshal(ev.ToolResponse, &s) == nil {
		b.WriteString(s)
	} else {
		var o struct {
			Stdout string `json:"stdout"`
			Stderr string `json:"stderr"`
		}
		if json.Unmarshal(ev.ToolResponse, &o) == nil {
			b.WriteString(o.Stdout)
			b.WriteString("\n")
			b.WriteString(o.Stderr)
		}
	}
	b.WriteString("\n")
	b.WriteString(ev.ToolResponseExcerpt)
	b.WriteString("\n")
	b.WriteString(ev.Error)
	return b.String()
}

// detectAuthStorms flags a session with at least min kd/gb calls answered 401
// or 403 that touched the agent's own bead or task (named in the command or
// the error) or ran a gb verb that acts on the caller's own bead.
func detectAuthStorms(in sweepInput, min int) []sweepFinding {
	taskOf := map[string]string{}
	for _, a := range in.Agents {
		taskOf[a.ID] = a.Fields["task_id"]
	}
	type acc struct {
		ev       sweepHookEvent
		n        int
		first    time.Time
		sample   string
		touchesT bool
	}
	bySess := map[string]*acc{}
	var order []string
	for _, ev := range in.Hooks {
		cmd := sweepCommand(ev)
		if cmd == "" || !sweepRunsKdGb.MatchString(cmd) {
			continue
		}
		resp := sweepResponseText(ev)
		loc := sweepAuthDenied.FindStringIndex(resp)
		if loc == nil {
			continue
		}
		task := taskOf[ev.AgentBeadID]
		own := sweepSelfVerb.MatchString(cmd)
		touchesTask := false
		for _, id := range []string{ev.AgentBeadID, task} {
			if id != "" && (strings.Contains(cmd, id) || strings.Contains(resp, id)) {
				own = true
				touchesTask = touchesTask || id == task
			}
		}
		if !own {
			continue
		}
		a := bySess[ev.SessionID]
		if a == nil {
			a = &acc{ev: ev, first: ev.TS, sample: firstLine(resp[loc[0]:])}
			bySess[ev.SessionID] = a
			order = append(order, ev.SessionID)
		}
		a.n++
		a.touchesT = a.touchesT || touchesTask
	}
	var out []sweepFinding
	for _, sid := range order {
		a := bySess[sid]
		if a.n < min {
			continue
		}
		target := a.ev.AgentBeadID
		if a.touchesT {
			target = taskOf[a.ev.AgentBeadID]
		}
		out = append(out, sweepFinding{
			Pattern: sweepAuthStorm, Agent: a.ev.Agent, AgentBeadID: a.ev.AgentBeadID, Sessions: []string{sid},
			BeadID: target, Count: a.n, At: a.first, Identity: sid,
			Detail: fmt.Sprintf("%d own-bead kd/gb calls denied, first: %s", a.n, truncateRunes(a.sample, 160)),
		})
	}
	return out
}

// detectOOMs flags sessions that exited 137 or carry an OOM restart cause, and
// agents whose last coop exit was 137 with no session bead to show for it.
//
// A session the kill took before the controller stamped it (gd-DgSORP1h8P) is
// retired with no exit code. It is flagged when its container restarted under
// it (a resumed session on the same agent bead and host started before it
// ended) and the agent's wrap-up names the memory limit. The restart alone is
// any hard kill: the epfq scheduled plan gardener shows it with no OOM.
func detectOOMs(in sweepInput) []sweepFinding {
	var out []sweepFinding
	seen := map[string]bool{}
	for _, b := range in.Sessions {
		ec := b.Fields["exit_code"]
		cause := strings.ToLower(b.Fields["restart_cause"])
		if ec != "137" && !strings.Contains(cause, "oom") {
			continue
		}
		a := b.Fields["agent"]
		seen[a] = true
		out = append(out, sweepFinding{
			Pattern: sweepOOM, Agent: a, AgentBeadID: b.Fields["agent_bead_id"], Sessions: []string{b.ID}, Count: 1,
			Identity: "session:" + b.ID,
			At:       parseSweepTime(b.Fields["ended_at"]),
			Detail:   fmt.Sprintf("session exit_code=%s restart_cause=%s role=%s", ec, b.Fields["restart_cause"], b.Fields["role"]),
		})
	}
	oomWrapup := map[string]string{}
	for _, a := range in.Agents {
		if w, ok := parseSweepWrapup(a.Fields["wrapup"]); ok {
			if m := sweepOOMWrapup.FindString(w.HarnessGaps + "\n" + w.TLDR + "\n" + w.Accomplishments); m != "" {
				oomWrapup[a.ID] = m
			}
		}
	}
	for _, b := range in.Sessions {
		bead, a := b.Fields["agent_bead_id"], b.Fields["agent"]
		if seen[a] || oomWrapup[bead] == "" || b.Fields["status"] != "retired" || b.Fields["exit_code"] != "" {
			continue
		}
		start, end := sweepSessionSpan(b)
		for _, n := range in.Sessions {
			ns, _ := sweepSessionSpan(n)
			if n.Fields["agent_bead_id"] != bead || n.Fields["resumed"] != "true" || n.Fields["hostname"] != b.Fields["hostname"] ||
				!ns.After(start) || !ns.Before(end) {
				continue
			}
			seen[a] = true
			out = append(out, sweepFinding{
				Pattern: sweepOOM, Agent: a, AgentBeadID: bead, Sessions: []string{b.ID, n.ID}, Count: 1,
				Identity: "session:" + b.ID,
				At:       ns,
				Detail: fmt.Sprintf("session retired with no exit code; its container restarted under it (resumed session %s at %s); wrap-up: %q",
					n.ID, ns.UTC().Format("15:04Z"), oomWrapup[bead]),
			})
			break
		}
	}
	for _, a := range in.Agents {
		name := a.Fields["agent"]
		if a.Fields["last_coop_exit_code"] != "137" || seen[name] {
			continue
		}
		out = append(out, sweepFinding{
			Pattern: sweepOOM, Agent: name, AgentBeadID: a.ID, BeadID: a.Fields["task_id"], Count: 1,
			Identity: "agent:" + a.ID,
			Detail:   fmt.Sprintf("agent last_coop_exit_code=137 (%s)", a.Fields["last_coop_exit_reason"]),
		})
	}
	return out
}

// sweepOOMWrapup matches a wrap-up naming the pod's memory limit as what ended
// a session: "likely the 2Gi pod OOM", "probably out of memory".
var sweepOOMWrapup = regexp.MustCompile(`(?i)\bOOM(?:[- ]?kill(?:ed)?)?\b|out of memory|\b2\s?Gi\b[^.]*\blimit`)

// detectRepeatCalls flags a session that made the same tool call (tool name +
// input, ignoring Bash's free-text description) at least min times.
func detectRepeatCalls(in sweepInput, min int) []sweepFinding {
	type acc struct {
		ev    sweepHookEvent
		n     int
		first time.Time
		label string
		ident string
	}
	counts := map[string]*acc{}
	var order []string
	for _, ev := range in.Hooks {
		label := ev.ToolName
		norm := string(ev.ToolInput)
		var m map[string]any
		if json.Unmarshal(ev.ToolInput, &m) == nil {
			delete(m, "description")
			if b, err := json.Marshal(m); err == nil { // map keys marshal sorted
				norm = string(b)
			}
			if c, ok := m["command"].(string); ok {
				label = c
			} else if p, ok := m["file_path"].(string); ok {
				label = ev.ToolName + " " + p
			}
		}
		k := ev.SessionID + "\x00" + ev.ToolName + "\x00" + norm
		a := counts[k]
		if a == nil {
			a = &acc{ev: ev, first: ev.TS, label: label, ident: k}
			counts[k] = a
			order = append(order, k)
		}
		a.n++
	}
	var out []sweepFinding
	for _, k := range order {
		a := counts[k]
		if a.n < min {
			continue
		}
		out = append(out, sweepFinding{
			Pattern: sweepRepeatCall, Agent: a.ev.Agent, AgentBeadID: a.ev.AgentBeadID, Sessions: []string{a.ev.SessionID},
			Count: a.n, At: a.first, Identity: a.ident,
			Detail: fmt.Sprintf("%s called %d times: %s", a.ev.ToolName, a.n, truncateRunes(strings.Join(strings.Fields(a.label), " "), 160)),
		})
	}
	return out
}

// detectDupComments flags one author posting the same (whitespace-normalised)
// text two or more times on one task inside the window.
func detectDupComments(in sweepInput) []sweepFinding {
	cutoff := in.Now.Add(-in.Since)
	ids := make([]string, 0, len(in.Comments))
	for id := range in.Comments {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var out []sweepFinding
	for _, id := range ids {
		type acc struct {
			n     int
			first time.Time
			text  string
		}
		groups := map[string]*acc{}
		var order []string
		for _, c := range in.Comments[id] {
			if c.CreatedAt.Before(cutoff) || c.Author == "" {
				continue
			}
			norm := strings.Join(strings.Fields(c.Text), " ")
			if norm == "" {
				continue
			}
			k := c.Author + "\x00" + norm
			g := groups[k]
			if g == nil {
				g = &acc{first: c.CreatedAt, text: norm}
				groups[k] = g
				order = append(order, k)
			}
			g.n++
		}
		for _, k := range order {
			g := groups[k]
			if g.n < 2 {
				continue
			}
			author, _, _ := strings.Cut(k, "\x00")
			out = append(out, sweepFinding{
				Pattern: sweepDupComment, Agent: author, BeadID: id, Count: g.n, At: g.first,
				Identity: id + "\x00" + g.text,
				Detail:   fmt.Sprintf("same comment %d times: %q", g.n, truncateRunes(g.text, 140)),
			})
		}
	}
	return out
}

// sweepSlowKinds sorts a slow call by the first regex that matches. A wait
// loop (until/while … sleep, pgrep, tail --pid) is checked right after CI
// polls, so a loop waiting on the agent's own golden or go test run is filed
// as the wait it is, not as the job it waits on (survey class idle-wait).
// `make commons-ux-shoot` is a golden shoot, not a build.
var sweepSlowKinds = []struct {
	kind string
	re   *regexp.Regexp
}{
	{"ci-wait", regexp.MustCompile(`gh run (?:watch|list|view)|gh pr checks|actions/runs|check-runs|gb wait-ci|statusCheckRollup`)},
	{"wait-on-process", regexp.MustCompile(`\b(?:until|while)\b[^\n]*?;\s*do\b[^\n]*?\bsleep\b|\bpgrep\b|tail --pid`)},
	{"goldens", regexp.MustCompile(`(?i)golden|playwright|gardenhub-browser|TestGarden|ux-shoot`)},
	{"build-test", regexp.MustCompile(`\bgo (?:test|build|vet)\b|\bmake\b|\bcargo\b|\bnpm (?:install|test|run|ci)\b|\bhelm (?:lint|template|unittest)\b|\bgolangci-lint\b`)},
	{"wait-on-process", regexp.MustCompile(`\bsleep\b|tail -f /dev/null|\bgb yield\b|\bwait\b|\bgb decision create\b`)},
}

func slowCallKind(args string) string {
	for _, k := range sweepSlowKinds {
		if k.re.MatchString(args) {
			return k.kind
		}
	}
	return "other"
}

// detectSlowCalls flags each tool call at or over the slow bar (WaitSlow for
// wait-on-process calls). It reads the session beads' slowest_calls and, for
// the many sessions that end retired with no slowest_calls, the bus's
// PostToolUse duration_ms; a call in both is counted once (by tool_use_id).
func detectSlowCalls(in sweepInput, th sweepThresholds) []sweepFinding {
	bar := func(kind string) time.Duration {
		if kind == "wait-on-process" && th.WaitSlow > 0 && th.WaitSlow < th.Slow {
			return th.WaitSlow
		}
		return th.Slow
	}
	var out []sweepFinding
	seen := map[string]bool{}
	for _, b := range in.Sessions {
		for _, c := range beadsapi.ParseSessionSlowCalls(b.Fields["slowest_calls"]) {
			d := time.Duration(c.DurationMs) * time.Millisecond
			kind := slowCallKind(c.Args)
			if d < bar(kind) {
				continue
			}
			if c.ToolUseID != "" {
				seen[c.ToolUseID] = true
			}
			args := strings.Join(strings.Fields(c.Args), " ")
			out = append(out, sweepFinding{
				Pattern: sweepSlowCall, Kind: kind, Agent: b.Fields["agent"], AgentBeadID: b.Fields["agent_bead_id"],
				Sessions: []string{b.ID}, Count: int(d / time.Second), At: parseSweepTime(c.TS),
				Identity: strings.Join([]string{b.ID, c.Tool, c.TS, c.Args}, "\x00"),
				Detail:   fmt.Sprintf("%s %s: %s", c.Tool, d.Round(time.Second), truncateRunes(args, 160)),
			})
		}
	}
	for _, ev := range in.Hooks {
		d := time.Duration(ev.DurationMs) * time.Millisecond
		if ev.DurationMs <= 0 || (ev.ToolUseID != "" && seen[ev.ToolUseID]) {
			continue
		}
		args := sweepCommand(ev)
		if args == "" {
			args = string(ev.ToolInput)
		}
		kind := slowCallKind(args)
		if d < bar(kind) {
			continue
		}
		args = strings.Join(strings.Fields(args), " ")
		out = append(out, sweepFinding{
			Pattern: sweepSlowCall, Kind: kind, Agent: ev.Agent, AgentBeadID: ev.AgentBeadID,
			Sessions: []string{ev.SessionID}, Count: int(d / time.Second), At: ev.TS,
			// The same key a slowest_calls entry would give is not available
			// (no session bead id), so the tool_use_id stands in.
			Identity: strings.Join([]string{ev.SessionID, ev.ToolName, ev.ToolUseID}, "\x00"),
			Detail:   fmt.Sprintf("%s %s: %s", ev.ToolName, d.Round(time.Second), truncateRunes(args, 160)),
		})
	}
	return out
}

var (
	// sweepMissingTool matches a harness_gaps sentence that names something
	// absent from the pod or CLI, as opposed to a permission it lacks.
	sweepMissingTool = regexp.MustCompile(`(?i)\b(?:has|have|had) no [\w./ -]{0,30}?(?:command|subcommand|verb|flag|tool|binary|cli)\b` +
		`|\bno [\w./-]+(?: or [\w./-]+)?(?: \w+)? (?:in|on) the pod\b` +
		`|\bunknown (?:command|flag|subcommand)\b` +
		`|\b(?:is not|isn't|not) installed\b|\bnot on (?:the )?PATH\b` +
		`|\bmissing (?:a |the )?(?:tool|command|binary|cli|subcommand|verb)\b` +
		`|\bno (?:way|tool|command|verb|subcommand) to\b` +
		`|\b(?:does|do) not exist\b`)
	// sweepToolSubject pulls a CLI verb ("gb pr", "gh run") out of the match's
	// sentence to group evidence; sweepNoSubject falls back to "no <thing>".
	sweepToolSubject = regexp.MustCompile(`\b(?:gb|kd|gh|helm|kubectl|coop) [a-z][a-z0-9-]+`)
	sweepNoSubject   = regexp.MustCompile(`(?i)\bno ([\w.-]+(?: or [\w.-]+)?)`)
	// sweepCLINoVerb catches "gb (2026.302.2) has no pr command" -> "gb pr".
	sweepCLINoVerb = regexp.MustCompile(`\b(gb|kd|gh)\b[^;]{0,40}?\bno ([a-z][a-z0-9-]*) (?:command|subcommand|verb)`)
	// sweepNotInstalledSubject catches "gofumpt and staticcheck are not
	// installed" -> "gofumpt" (the survey found these grouped under no subject).
	sweepNotInstalledSubject = regexp.MustCompile(`^(?:The\s+)?([A-Za-z][\w.-]*)\b[^.]*?\b(?:is|are|was|were)(?: not|n't) installed`)
	// sweepPermissionGap marks a sentence about a refused permission, which
	// is an access ask for the operator, not a missing tool ("No way to
	// re-run a flaky CI job with the pod GitHub token (403 …)").
	sweepPermissionGap = regexp.MustCompile(`(?i)\b40[13]\b|\bpermission\b|not accessible|not authori[sz]ed|\bforbidden\b|\bdenied\b`)
)

// detectHarnessGaps flags wrap-ups whose harness_gaps names a missing tool.
func detectHarnessGaps(in sweepInput) []sweepFinding {
	var out []sweepFinding
	for _, a := range in.Agents {
		raw := a.Fields["wrapup"]
		if raw == "" {
			continue
		}
		var w struct {
			HarnessGaps string `json:"harness_gaps"`
			Timestamp   string `json:"timestamp"`
		}
		if json.Unmarshal([]byte(raw), &w) != nil || w.HarnessGaps == "" || !in.wrapupInWindow(w.Timestamp) {
			continue
		}
		for _, sentence := range splitSentences(w.HarnessGaps) {
			if !sweepMissingTool.MatchString(sentence) || sweepPermissionGap.MatchString(sentence) {
				continue
			}
			subject := ""
			if m := sweepCLINoVerb.FindStringSubmatch(sentence); m != nil {
				subject = m[1] + " " + m[2]
			} else if subject = sweepToolSubject.FindString(sentence); subject == "" {
				if m := sweepNotInstalledSubject.FindStringSubmatch(sentence); m != nil {
					subject = strings.ToLower(m[1])
				}
			}
			if subject == "" {
				if m := sweepNoSubject.FindStringSubmatch(sentence); m != nil {
					subject = strings.ToLower(m[1])
				}
			}
			out = append(out, sweepFinding{
				Pattern: sweepHarnessGap, Kind: subject, Agent: a.Fields["agent"], AgentBeadID: a.ID, BeadID: a.Fields["task_id"],
				Count: 1, At: parseSweepTime(w.Timestamp), Detail: truncateRunes(sentence, 220),
				Identity: a.ID + "\x00" + sentence,
			})
		}
	}
	return out
}

// splitSentences splits on ". ", "; " and newlines, keeping the text whole
// enough to read as evidence.
func splitSentences(s string) []string {
	s = strings.NewReplacer("; ", "\n", ". ", ".\n").Replace(s)
	var out []string
	for _, p := range strings.Split(s, "\n") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func printSweepReport(w io.Writer, rep sweepReport) {
	fmt.Fprintf(w, "Session sweep %s, last %s: %d sessions, %d agents, %d task comment threads, %d hook events",
		rep.Now.Format(time.RFC3339), rep.Since, rep.Sessions, rep.Agents, rep.TasksScanned, rep.HookEvents)
	if rep.HookFrom != nil {
		fmt.Fprintf(w, " (bus %s to %s)", rep.HookFrom.UTC().Format("2006-01-02 15:04Z"), rep.HookTo.UTC().Format("15:04Z"))
	}
	fmt.Fprintf(w, "; %.1fs\n", float64(rep.ElapsedMs)/1000)
	for _, warn := range rep.FetchWarnings {
		fmt.Fprintf(w, "warning: %s\n", warn)
	}
	if len(rep.Findings) == 0 {
		fmt.Fprintln(w, "No findings.")
	}
	byPattern := map[string][]sweepFinding{}
	for _, f := range rep.Findings {
		byPattern[f.Pattern] = append(byPattern[f.Pattern], f)
	}
	for _, p := range sweepPatternOrder {
		fs := byPattern[p]
		if len(fs) == 0 {
			continue
		}
		fmt.Fprintf(w, "\n== %s (%d): %s\n", p, len(fs), sweepPatternTitles[p])
		tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
		for _, f := range fs {
			where := strings.Join(f.Sessions, ",")
			if f.BeadID != "" {
				where = strings.Trim(f.BeadID+" "+where, " ")
			}
			kind := ""
			if f.Kind != "" {
				kind = "[" + f.Kind + "] "
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s%s\n", f.Agent, truncateRunes(where, 60), kind, f.Detail)
		}
		tw.Flush()
	}
	for _, fl := range rep.Filed {
		fmt.Fprintf(w, "filed %s: %s %s (%d new)\n", fl.Pattern, fl.Action, fl.BeadID, fl.New)
	}
}

// --- filing ---

// A bug remembers which findings it has seen in a trailing "sweep-keys:" line
// on its description and on each sweep comment (bug beads have a fixed field
// schema, so there is no field to hold them). sweepSeenMax caps one line.
const sweepSeenMax = 400

var sweepKeysLine = regexp.MustCompile(`sweep-keys: ([0-9a-f,]+)`)

// sweepSeenKeys collects every key a bug's description and comments carry.
func sweepSeenKeys(desc string, comments []beadsapi.Comment) []string {
	texts := []string{desc}
	for _, c := range comments {
		texts = append(texts, c.Text)
	}
	var keys []string
	for _, t := range texts {
		for _, m := range sweepKeysLine.FindAllStringSubmatch(t, -1) {
			keys = append(keys, strings.Split(m[1], ",")...)
		}
	}
	return keys
}

func fileSweepFindings(ctx context.Context, rep sweepReport) ([]sweepFiled, error) {
	project := sweepProject
	if project == "" {
		project = os.Getenv("BOAT_PROJECT")
	}
	if project == "" {
		project = "gardenio"
	}
	byPattern := map[string][]sweepFinding{}
	for _, f := range rep.Findings {
		byPattern[f.Pattern] = append(byPattern[f.Pattern], f)
	}
	var filed []sweepFiled
	for _, p := range sweepPatternOrder {
		fs := byPattern[p]
		if len(fs) == 0 {
			continue
		}
		res, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
			Types: []string{"bug"}, Statuses: []string{"open", "in_progress", "blocked", "deferred"},
			Labels: []string{"project:" + project, "session-sweep:" + p}, Sort: "created_at", Limit: 5,
		})
		if err != nil {
			return filed, fmt.Errorf("looking up the open %s bug: %w", p, err)
		}
		if len(res.Beads) > 0 {
			bug := res.Beads[0]
			comments, err := daemon.GetComments(ctx, bug.ID)
			if err != nil {
				return filed, err
			}
			fresh := unseenFindings(fs, sweepSeenKeys(bug.Description, comments))
			if len(fresh) == 0 {
				filed = append(filed, sweepFiled{Pattern: p, BeadID: bug.ID, Action: "unchanged"})
				continue
			}
			body := fmt.Sprintf("Session sweep %s found %d new piece(s) of evidence (window %s, `gb session sweep`):\n\n%s\n\n`sweep-keys: %s`",
				rep.Now.Format("2006-01-02 15:04Z"), len(fresh), rep.Since, renderSweepEvidence(fresh), joinSeen(nil, fresh))
			if err := daemon.AddComment(ctx, bug.ID, actor, body); err != nil {
				return filed, err
			}
			filed = append(filed, sweepFiled{Pattern: p, BeadID: bug.ID, Action: "commented", New: len(fresh)})
			continue
		}
		closed, err := sweepClosedKeys(ctx, rep, project, p)
		if err != nil {
			return filed, fmt.Errorf("looking up the closed %s bugs: %w", p, err)
		}
		var fresh []sweepFinding
		for _, f := range fs {
			if closed[f.key()] == "" {
				fresh = append(fresh, f)
			}
		}
		if len(fresh) == 0 {
			filed = append(filed, sweepFiled{Pattern: p, BeadID: closed[fs[0].key()], Action: "unchanged"})
			continue
		}
		fs = fresh
		desc := fmt.Sprintf("Filed by `gb session sweep` (%s, window %s). Pattern **%s**: %s.\n\n"+
			"Evidence (%d):\n\n%s\n\nRe-run: `gb session sweep --since %s`. Later sweeps comment only new evidence here. "+
			"Close this bug when the cause is fixed or judged a false positive (say which, and why, in the close reason); "+
			"evidence already here is then not filed again, and a later recurrence files a fresh bug. Recipe: gardenio/docs/session-sweep.md.\n\n`sweep-keys: %s`",
			rep.Now.Format("2006-01-02 15:04Z"), rep.Since, p, sweepPatternTitles[p], len(fs), renderSweepEvidence(fs), rep.Since, joinSeen(nil, fs))
		id, err := daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
			Title:       "Session sweep: " + sweepPatternTitles[p],
			ShortTitle:  sweepPatternShortTitles[p],
			Type:        "bug",
			Description: desc,
			Labels:      []string{"project:" + project, "session-sweep", "session-sweep:" + p},
			Priority:    2,
			CreatedBy:   actor,
		})
		if err != nil {
			return filed, fmt.Errorf("filing the %s bug: %w", p, err)
		}
		if sweepParent != "" {
			if err := daemon.AddDependency(ctx, id, sweepParent, "parent-child", actor); err != nil {
				return filed, fmt.Errorf("linking %s under %s: %w", id, sweepParent, err)
			}
		}
		filed = append(filed, sweepFiled{Pattern: p, BeadID: id, Action: "created", New: len(fs)})
	}
	return filed, nil
}

// sweepClosedKeys maps each sweep-key carried by the pattern's bugs closed
// inside the sweep window to the bug that carries it. Evidence a gardener
// closed (fixed, stale or a false positive) is then not filed again while it
// stays in the window; a finding with a new key still files a fresh bug
// (gd-puMsI5w1kN).
func sweepClosedKeys(ctx context.Context, rep sweepReport, project, pattern string) (map[string]string, error) {
	window, err := time.ParseDuration(rep.Since)
	if err != nil {
		window = sweepSince
	}
	res, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types: []string{"bug"}, Statuses: []string{"closed"},
		Labels:      []string{"project:" + project, "session-sweep:" + pattern},
		ClosedAfter: rep.Now.Add(-window), Sort: "created_at", Limit: 10,
	})
	if err != nil {
		return nil, err
	}
	keys := map[string]string{}
	for _, bug := range res.Beads {
		comments, err := daemon.GetComments(ctx, bug.ID)
		if err != nil {
			return nil, err
		}
		for _, k := range sweepSeenKeys(bug.Description, comments) {
			if keys[k] == "" {
				keys[k] = bug.ID
			}
		}
	}
	return keys, nil
}

func unseenFindings(fs []sweepFinding, seen []string) []sweepFinding {
	have := map[string]bool{}
	for _, k := range seen {
		have[k] = true
	}
	var out []sweepFinding
	for _, f := range fs {
		if !have[f.key()] {
			out = append(out, f)
		}
	}
	return out
}

func joinSeen(seen []string, add []sweepFinding) string {
	all := append([]string{}, seen...)
	for _, f := range add {
		all = append(all, f.key())
	}
	if len(all) > sweepSeenMax {
		all = all[len(all)-sweepSeenMax:]
	}
	return strings.Join(all, ",")
}

// renderSweepEvidence renders findings as a markdown list, grouped by kind when
// the pattern has kinds (slow-call kinds, harness-gap subjects).
func renderSweepEvidence(fs []sweepFinding) string {
	var b strings.Builder
	byKind := map[string][]sweepFinding{}
	var kinds []string
	for _, f := range fs {
		if _, ok := byKind[f.Kind]; !ok {
			kinds = append(kinds, f.Kind)
		}
		byKind[f.Kind] = append(byKind[f.Kind], f)
	}
	sort.SliceStable(kinds, func(i, j int) bool { return len(byKind[kinds[i]]) > len(byKind[kinds[j]]) })
	for _, k := range kinds {
		if k != "" {
			fmt.Fprintf(&b, "**%s** (%d)\n", k, len(byKind[k]))
		}
		for _, f := range byKind[k] {
			fmt.Fprintf(&b, "- %s", f.Agent)
			if f.AgentBeadID != "" {
				fmt.Fprintf(&b, " (%s)", f.AgentBeadID)
			}
			if f.BeadID != "" {
				fmt.Fprintf(&b, " on %s", f.BeadID)
			}
			if len(f.Sessions) > 0 {
				fmt.Fprintf(&b, ", sessions %s", strings.Join(f.Sessions, " "))
			}
			if !f.At.IsZero() {
				fmt.Fprintf(&b, ", %s", f.At.UTC().Format("01-02 15:04Z"))
			}
			fmt.Fprintf(&b, ": %s\n", strings.ReplaceAll(f.Detail, "`", "'"))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
