// Command jira-syncback-probe is a TEST-ONLY driver that exercises the
// jira-bridge SYNC-BACK write path (JiraSync.handleUpdated / handleClosed) end
// to end, hermetically, so the e2e suite can assert the deny-by-default project
// allowlist AND the source:jira-dark early-return WITHOUT standing up a live
// jira-bridge, a live Jira, or a real daemon.
//
// ── Why a probe, not a live-bridge module ─────────────────────────────────
//
// The jira-bridge is OFF in the e2e namespace, so there is no live sync-back watcher to observe, and the sync-back handlers
// (handleUpdated / handleClosed) are UNEXPORTED — a package-main test can only
// reach them the way production does: by registering the REAL JiraSync on the
// REAL SSEStream (JiraSync.RegisterHandlers) and feeding it real SSE bead
// frames. This probe does exactly that, following the blessed cmd/jira-conv-probe
// / cmd/trigger-probe precedent ("real bridge component + stub Jira, no product
// image, no pods"):
//
//   - a REAL bridge.JiraSync built with the SAME config-driven WritePolicy
//     production wires (bridge.DefaultWritePolicy(false) — outbox toggle OFF,
//     enforce mode: writable set {PE, DEVOPS}, GBFSM toggle-gated, FSM and every
//     arbitrary project denied), registered on a REAL bridge.SSEStream;
//   - a STUB Jira (httptest.Server) that RECORDS every outbound REST request per
//     issue key, so "did the sync-back write?" is observed as a recorded call —
//     exactly as the sandbox-Jira in the outbox tests records writes;
//   - a STUB SSE server that emits the leg's bead frames then holds the
//     connection open, so the stream dispatches them through the real handlers.
//
// The result is a faithful wire-level exercise of the real handler code (SSE
// decode → ParseBeadEvent → dark-marker early-return → deny-by-default project
// gate → in-scope → per-role field/comment allowlist) with ZERO pods, ZERO
// shared state, and ZERO sandbox rig — green today and a hard gate.
//
// ── Why every leg carries its own positive control (anti-vacuous) ──────────
//
// A "0 outbound writes" assertion is worthless if the sync engine is a no-op in
// this run — a broken JiraSync that writes NOTHING would pass every deny/dark
// leg. So each leg emits its deny/dark target AND an ALLOWLISTED positive-control
// bead into the SAME live engine, and asserts BOTH: the target key received ZERO
// Jira requests AND the control key received writes. The control proves the
// engine was live and that the deny/dark rule specifically suppressed a write
// that would OTHERWISE have happened — the dark-jira-fence "leg-5 control"
// discipline (a fence assertion that also passes under a global outage is
// vacuous). The dark legs are the sharpest: target and control are BOTH PE
// (allowlisted) keys, so the ONLY difference is the source:jira-dark label —
// proving the dark marker, not the project gate, is what stops the write.
//
// ── Deterministic barrier (no sleep-race) ──────────────────────────────────
//
// SSEStream.dispatch() runs an event's handlers concurrently, wg.Wait()s them,
// and ONLY THEN persists the event id via StateManager.SetLastEventID (sse.go).
// readEvents dispatches events strictly in order. So once GetLastEventID equals
// the LAST frame's id, every handler for that frame AND all prior frames has
// returned — a precise "all writes have been made-or-suppressed" barrier that
// needs no sleep and cannot race the assertion.
//
// Each leg prints one line "LEG <name>: PASS|FAIL <detail>" and the probe exits
// non-zero if any leg fails, so the wrapping shell module surfaces a per-leg
// breakdown and reddens the shard on any leg. It is NOT part of any release
// Makefile/Dockerfile target — it is built from source on demand inside the e2e
// shard, exactly like trigger-probe / jira-conv-probe, and never ships.
//
// Legs (task ka-0000005719, coverage-backfill epic ka-0000000958; enforcement
// bead ka-0000005143, commit f30cae2):
//
//	1  updated-deny-nonallowlisted  an arbitrary agent-chosen key (ACME) synced
//	                                bead → handleUpdated makes ZERO Jira writes;
//	                                an allowlisted PE control in the same run DOES.
//	2  closed-deny-nonallowlisted   same, via handleClosed.
//	3  updated-deny-fsm             the explicitly-blocked FSM project → ZERO
//	                                writes in handleUpdated (block wins); PE control writes.
//	4  closed-deny-fsm              same, via handleClosed.
//	5  updated-dark-early-return    a source:jira-dark bead carrying an ALLOWLISTED
//	                                PE key + claim + MR + mr_merged → handleUpdated
//	                                early-returns with ZERO writes (dark wins over
//	                                the allowlist); a non-dark PE control writes.
//	6  closed-dark-early-return     same, via handleClosed.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/bridge"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	legs := []struct {
		name string
		run  func(*slog.Logger) legResult
	}{
		{"updated-deny-nonallowlisted", runUpdatedDenyNonAllowlisted},
		{"closed-deny-nonallowlisted", runClosedDenyNonAllowlisted},
		{"updated-deny-fsm", runUpdatedDenyFSM},
		{"closed-deny-fsm", runClosedDenyFSM},
		{"updated-dark-early-return", runUpdatedDarkEarlyReturn},
		{"closed-dark-early-return", runClosedDarkEarlyReturn},
	}

	failures := 0
	for _, leg := range legs {
		res := leg.run(logger)
		status := "PASS"
		if !res.ok {
			status = "FAIL"
			failures++
		}
		fmt.Printf("LEG %s: %s %s\n", leg.name, status, res.detail)
	}

	if failures > 0 {
		fmt.Printf("jira-syncback-probe: %d/%d legs FAILED\n", failures, len(legs))
		os.Exit(1)
	}
	fmt.Printf("jira-syncback-probe: all %d legs passed\n", len(legs))
}

// legResult is the outcome of one sync-back leg.
type legResult struct {
	ok     bool
	detail string
}

func pass(format string, args ...any) legResult {
	return legResult{ok: true, detail: fmt.Sprintf(format, args...)}
}

func fail(format string, args ...any) legResult {
	return legResult{ok: false, detail: fmt.Sprintf(format, args...)}
}

// syncRole is the beads role stamped on every bead the probe emits. crew carries
// the full field allowlist (DefaultFieldAllowlist), so an ALLOWLISTED-project
// control bead's claim/MR/close writes pass the per-role write policy — the deny
// and dark legs must therefore fail on the project/dark rule, never on the role.
const syncRole = "crew"

// darkLabel mirrors bridge.jiraDarkLabel (unexported) — the source:jira-dark
// marker whose presence makes handleUpdated/handleClosed early-return before any
// project gating (ka-0000005143). A local literal follows the cmd/jira-conv-probe
// precedent (its mentionTrigger const) and keeps this probe test-only with zero
// production surface. If production ever changes the marker value, the dark legs
// go red (a dark-labelled bead would no longer early-return and WOULD write back)
// — the intended drift signal.
const darkLabel = "source:jira-dark"

// ─────────────────────────────────────────────────────────────────────────────
// Leg 1: handleUpdated denies a bead bound to an arbitrary, non-allowlisted JIRA
//        project (deny-by-default), while an allowlisted PE control still writes.
// ─────────────────────────────────────────────────────────────────────────────

func runUpdatedDenyNonAllowlisted(logger *slog.Logger) legResult {
	const (
		denyKey = "ACME-42"  // not on the allow-list, not blocked — deny by default
		ctlKey  = "PE-42001" // allowlisted control
	)
	return runUpdatedDenyLeg(logger, denyKey, ctlKey, nil,
		"non-allowlisted project %s wrote back (deny-by-default breached)")
}

// ─────────────────────────────────────────────────────────────────────────────
// Leg 2: handleClosed denies the same non-allowlisted project on close.
// ─────────────────────────────────────────────────────────────────────────────

func runClosedDenyNonAllowlisted(logger *slog.Logger) legResult {
	const (
		denyKey = "ACME-43"
		ctlKey  = "PE-42002"
	)
	return runClosedDenyLeg(logger, denyKey, ctlKey, nil,
		"non-allowlisted project %s wrote back on close (deny-by-default breached)")
}

// ─────────────────────────────────────────────────────────────────────────────
// Leg 3: handleUpdated denies the explicitly-blocked FSM project — an explicit
//        block wins over any allow-list, and FSM is never in the writable set.
// ─────────────────────────────────────────────────────────────────────────────

func runUpdatedDenyFSM(logger *slog.Logger) legResult {
	const (
		denyKey = "FSM-500" // customer-facing service desk — permanently non-writable
		ctlKey  = "PE-42003"
	)
	return runUpdatedDenyLeg(logger, denyKey, ctlKey, nil,
		"blocked project %s wrote back (FSM block breached — customer-facing leak)")
}

// ─────────────────────────────────────────────────────────────────────────────
// Leg 4: handleClosed denies the FSM project on close.
// ─────────────────────────────────────────────────────────────────────────────

func runClosedDenyFSM(logger *slog.Logger) legResult {
	const (
		denyKey = "FSM-501"
		ctlKey  = "PE-42004"
	)
	return runClosedDenyLeg(logger, denyKey, ctlKey, nil,
		"blocked project %s wrote back on close (FSM block breached)")
}

// ─────────────────────────────────────────────────────────────────────────────
// Leg 5: handleUpdated early-returns on the source:jira-dark marker BEFORE any
//        project gating — even for an ALLOWLISTED (PE) key carrying a claim, an
//        MR, and mr_merged. The control is also PE, so the ONLY difference is the
//        dark label: this proves the dark marker (not the project gate) stops the
//        write, so dark-universe work can never leak upstream.
// ─────────────────────────────────────────────────────────────────────────────

func runUpdatedDarkEarlyReturn(logger *slog.Logger) legResult {
	const (
		darkKey = "PE-70000" // ALLOWLISTED key — only the dark label suppresses it
		ctlKey  = "PE-70001"
	)
	return runUpdatedDenyLeg(logger, darkKey, ctlKey, []string{darkLabel},
		"dark-marked bead (allowlisted key %s) wrote back — the source:jira-dark early-return leaked dark work upstream")
}

// ─────────────────────────────────────────────────────────────────────────────
// Leg 6: handleClosed early-returns on the source:jira-dark marker for an
//        allowlisted PE key.
// ─────────────────────────────────────────────────────────────────────────────

func runClosedDarkEarlyReturn(logger *slog.Logger) legResult {
	const (
		darkKey = "PE-70002"
		ctlKey  = "PE-70003"
	)
	return runClosedDenyLeg(logger, darkKey, ctlKey, []string{darkLabel},
		"dark-marked bead (allowlisted key %s) wrote back on close — dark early-return leaked upstream")
}

// ─────────────────────────────────────────────────────────────────────────────
// Shared leg drivers. Each builds a fresh stub Jira + a real JiraSync + a real
// SSEStream, emits the target (deny/dark) frame AND an allowlisted control frame,
// waits for the deterministic dispatch barrier, then asserts the target key made
// ZERO Jira requests and the control key made writes.
// ─────────────────────────────────────────────────────────────────────────────

// runUpdatedDenyLeg drives an `updated` deny/dark scenario. denyLabels are extra
// labels stamped on the target bead (nil for a project-deny leg; the dark label
// for a dark leg). breachFmt takes the denied key and describes the failure.
func runUpdatedDenyLeg(logger *slog.Logger, denyKey, ctlKey string, denyLabels []string, breachFmt string) legResult {
	stub := newStubJira()
	defer stub.Close()
	js := newProbeSync(stub, logger)

	// Frame 1: the deny/dark target — a claim + MR-linked update that WOULD write
	// back were it allowlisted and non-dark.
	target := beadFrame{
		id:     "bd-deny-" + denyKey,
		typ:    "task",
		title:  "[" + denyKey + "] should not sync",
		status: "in_progress",
		labels: append([]string{"source:jira", "jira:" + denyKey}, denyLabels...),
		fields: map[string]string{
			"jira_key": denyKey, "role": syncRole, "mr_url": "https://gitlab.com/x/-/merge_requests/1", "mr_merged": "true",
		},
		assignee: "agent-worker-x",
	}
	// Frame 2: the allowlisted PE control — identical claim + MR shape, so it
	// DOES write back and proves the engine was live in this very run.
	control := beadFrame{
		id:     "bd-ctl-" + ctlKey,
		typ:    "task",
		title:  "[" + ctlKey + "] should sync",
		status: "in_progress",
		labels: []string{"source:jira", "jira:" + ctlKey},
		fields: map[string]string{
			"jira_key": ctlKey, "role": syncRole, "mr_url": "https://gitlab.com/x/-/merge_requests/2",
		},
		assignee: "agent-worker-y",
	}

	if !driveStream(logger, js, "beads.bead.updated", []beadFrame{target, control}) {
		return fail("events never fully dispatched through the sync handlers (barrier timeout) — assertion would be vacuous")
	}
	return assertDenyAndControl(stub, denyKey, ctlKey, breachFmt)
}

// runClosedDenyLeg drives a `closed` deny/dark scenario.
func runClosedDenyLeg(logger *slog.Logger, denyKey, ctlKey string, denyLabels []string, breachFmt string) legResult {
	stub := newStubJira()
	defer stub.Close()
	js := newProbeSync(stub, logger)

	target := beadFrame{
		id:       "bd-deny-" + denyKey,
		typ:      "task",
		title:    "[" + denyKey + "] closed, should not sync",
		status:   "closed",
		labels:   append([]string{"source:jira", "jira:" + denyKey}, denyLabels...),
		fields:   map[string]string{"jira_key": denyKey, "role": syncRole},
		closedBy: "agent-worker-x",
	}
	control := beadFrame{
		id:       "bd-ctl-" + ctlKey,
		typ:      "task",
		title:    "[" + ctlKey + "] closed, should sync",
		status:   "closed",
		labels:   []string{"source:jira", "jira:" + ctlKey},
		fields:   map[string]string{"jira_key": ctlKey, "role": syncRole},
		closedBy: "agent-worker-y",
	}

	if !driveStream(logger, js, "beads.bead.closed", []beadFrame{target, control}) {
		return fail("close events never fully dispatched (barrier timeout) — assertion would be vacuous")
	}
	return assertDenyAndControl(stub, denyKey, ctlKey, breachFmt)
}

// assertDenyAndControl is the shared verdict: the denied/dark key made ZERO Jira
// requests, and the allowlisted control key made at least one write.
func assertDenyAndControl(stub *stubJira, denyKey, ctlKey, breachFmt string) legResult {
	if n := stub.requestCount(denyKey); n != 0 {
		return fail(breachFmt+" (%d Jira request(s) escaped)", denyKey, n)
	}
	ctlWrites := stub.requestCount(ctlKey)
	if ctlWrites == 0 {
		return fail("positive control %s made 0 Jira writes — the sync-back engine was inert, so the 0-writes assertion for %s is VACUOUS",
			ctlKey, denyKey)
	}
	return pass("deny/dark key %s made 0 writes; allowlisted control %s wrote back (%d requests) — engine live, denial specific",
		denyKey, ctlKey, ctlWrites)
}

// newProbeSync builds a JiraSync wired EXACTLY as production wires it: the same
// config-driven write policy (DefaultWritePolicy(false) — outbox toggle off,
// enforce mode), which is the sole decision path (project + role + field).
func newProbeSync(stub *stubJira, logger *slog.Logger) *bridge.JiraSync {
	return bridge.NewJiraSync(bridge.JiraSyncConfig{
		Jira:        stub.client(logger),
		Logger:      logger,
		WritePolicy: bridge.NewWritePolicyStore(bridge.DefaultWritePolicy(false), logger),
	})
}

// driveStream registers js (the JiraSync) on a real SSEStream, points it at a stub SSE server
// emitting the given frames on `topic`, starts it, and blocks until the LAST
// frame's id is persisted — the precise post-dispatch barrier (see file header).
// Returns false if the barrier is not reached within the timeout.
func driveStream(logger *slog.Logger, js *bridge.JiraSync, topic string, frames []beadFrame) bool {
	// A StateManager backed by a temp file gives us the persisted last-event-id
	// that dispatch() only writes AFTER wg.Wait() — the barrier we poll on. The
	// path must NOT pre-exist: NewStateManager tolerates a missing file
	// (os.IsNotExist) but errors on an empty/invalid one, so we hand it a
	// not-yet-created path inside a temp dir rather than a freshly-truncated file.
	stateDir, err := os.MkdirTemp("", "jira-syncback-probe-state-*")
	if err != nil {
		return false
	}
	defer os.RemoveAll(stateDir)
	statePath := stateDir + "/state.json"
	state, err := bridge.NewStateManager(statePath)
	if err != nil {
		return false
	}

	sse := newStubSSE(topic, frames)
	defer sse.Close()

	stream := bridge.NewSSEStream(bridge.SSEStreamConfig{
		BeadsHTTPAddr: sse.URL,
		Topics:        []string{topic},
		Logger:        logger,
		State:         state,
	})
	js.RegisterHandlers(stream)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() { _ = stream.Start(ctx) }()

	finalID := frames[len(frames)-1].id
	ok := waitFor(8*time.Second, func() bool { return state.GetLastEventID() == finalID })
	cancel()
	return ok
}

// ─────────────────────────────────────────────────────────────────────────────
// Stub Jira: an httptest server that records EVERY outbound REST request per
// issue key so a leg can assert exact write counts. Answers the sync-back
// endpoints (comment / transitions / remotelink) so an allowlisted control
// actually completes its writes.
// ─────────────────────────────────────────────────────────────────────────────

type stubJira struct {
	srv *httptest.Server
	mu  sync.Mutex
	// perKey counts EVERY request (any method) observed for an issue key. The
	// sync-back handlers make ZERO Jira calls for a denied/dark bead (they
	// return before any REST call), so a denied key's count must be 0.
	perKey map[string]int
}

func newStubJira() *stubJira {
	s := &stubJira{perKey: map[string]int{}}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

func (s *stubJira) Close() { s.srv.Close() }

func (s *stubJira) client(logger *slog.Logger) *bridge.JiraClient {
	return bridge.NewJiraClient(bridge.JiraClientConfig{
		BaseURL: s.srv.URL, Email: "probe@example.com", APIToken: "probe", Logger: logger,
	})
}

func (s *stubJira) requestCount(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.perKey[key]
}

// handle records the request under its issue key, then answers the endpoint so
// the control bead's write chain (claim comment → transition → MR link) runs to
// completion. A GET on /transitions returns a list containing the transitions
// the claim/merged paths look up, so TransitionIssue finds a match.
func (s *stubJira) handle(w http.ResponseWriter, r *http.Request) {
	key := jiraKeyFromIssuePath(r.URL.Path)
	if key != "" {
		s.mu.Lock()
		s.perKey[key]++
		s.mu.Unlock()
	}
	_, _ = io.Copy(io.Discard, r.Body)

	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/transitions"):
		writeJSON(w, map[string]any{"transitions": []map[string]any{
			{"id": "31", "name": "In Progress", "to": map[string]string{"name": "In Progress"}},
			{"id": "41", "name": "Review", "to": map[string]string{"name": "Review"}},
		}})
	case strings.HasSuffix(r.URL.Path, "/comment"):
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, map[string]any{"id": "stub-comment"})
	case strings.HasSuffix(r.URL.Path, "/remotelink"):
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, map[string]any{"id": 1})
	case strings.HasSuffix(r.URL.Path, "/transitions"):
		w.WriteHeader(http.StatusNoContent)
	default:
		writeJSON(w, map[string]any{})
	}
}

// jiraKeyFromIssuePath extracts KEY from /rest/api/3/issue/KEY[/...].
func jiraKeyFromIssuePath(path string) string {
	const prefix = "/rest/api/3/issue/"
	rest := strings.TrimPrefix(path, prefix)
	if rest == path {
		return "" // not an issue path
	}
	if idx := strings.Index(rest, "/"); idx > 0 {
		return rest[:idx]
	}
	return rest
}

// ─────────────────────────────────────────────────────────────────────────────
// Stub SSE server: emits the given bead frames (each with a sequential id) on
// the requested topic, then holds the connection open until the client's request
// context is cancelled — so the stream dispatches every frame without hitting an
// early EOF + reconnect (mirrors cmd/jira-conv-probe's stubSSE).
// ─────────────────────────────────────────────────────────────────────────────

// beadFrame is one SSE bead event the probe emits. id is assigned at emit time.
type beadFrame struct {
	id       string
	typ      string
	title    string
	status   string
	labels   []string
	fields   map[string]string
	assignee string
	closedBy string
}

type stubSSE struct {
	*httptest.Server
}

func newStubSSE(topic string, frames []beadFrame) *stubSSE {
	// Assign sequential, non-empty ids ("1".."N"); dispatch persists the id only
	// when it is non-empty, and the barrier polls for the final id.
	for i := range frames {
		frames[i].id = fmt.Sprintf("%d", i+1)
	}
	s := &stubSSE{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no flush", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		for _, f := range frames {
			fmt.Fprintf(w, "id:%s\n", f.id)
			fmt.Fprintf(w, "event:%s\n", topic)
			fmt.Fprintf(w, "data:%s\n\n", beadEventData(topic, f))
			flusher.Flush()
		}
		// Hold the connection open until the client cancels (post-barrier), so the
		// stream never sees an EOF mid-dispatch and never reconnect-spams.
		<-r.Context().Done()
	}))
	return s
}

// beadEventData builds the kbeads SSE `data:` JSON for one bead frame. A closed
// event carries closed_by at the wrapper level (mirrors the daemon payload).
func beadEventData(topic string, f beadFrame) string {
	bead := map[string]any{
		"id":     f.id,
		"type":   f.typ,
		"title":  f.title,
		"status": f.status,
		"labels": f.labels,
		"fields": f.fields,
	}
	if f.assignee != "" {
		bead["assignee"] = f.assignee
	}
	wrapper := map[string]any{"bead": bead}
	if topic == "beads.bead.closed" && f.closedBy != "" {
		wrapper["closed_by"] = f.closedBy
	}
	b, _ := json.Marshal(wrapper)
	return string(b)
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers.
// ─────────────────────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// waitFor polls cond every 10ms until it is true or the deadline elapses.
func waitFor(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}
