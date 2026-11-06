package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

var sweepTestNow = time.Date(2026, 10, 30, 3, 0, 0, 0, time.UTC)

func sweepSession(id, agent string, start time.Time, fields map[string]string) *beadsapi.BeadDetail {
	f := map[string]string{"agent": agent, "agent_bead_id": "gd-" + agent, "started_at": start.Format(time.RFC3339)}
	for k, v := range fields {
		f[k] = v
	}
	return &beadsapi.BeadDetail{ID: id, Type: "session", Fields: f}
}

func sweepBash(agent, session, cmd, stdout string, ts time.Time) sweepHookEvent {
	in, _ := json.Marshal(map[string]string{"command": cmd, "description": "run " + cmd})
	out, _ := json.Marshal(map[string]string{"stdout": stdout})
	return sweepHookEvent{Agent: agent, AgentBeadID: "gd-" + agent, SessionID: session, Event: "PostToolUse",
		ToolName: "Bash", ToolInput: in, ToolResponse: out, TS: ts}
}

func TestDetectRestartLoops(t *testing.T) {
	var sessions []*beadsapi.BeadDetail
	// looper: 5 sessions ~10 minutes apart, the 5juu shape.
	for i := range 5 {
		sessions = append(sessions, sweepSession("s-loop-"+string(rune('a'+i)), "looper", sweepTestNow.Add(-2*time.Hour+time.Duration(i)*10*time.Minute), map[string]string{"restart_cause": "crash"}))
	}
	// steady: 4 sessions, but never more than 3 in any hour.
	for i := range 4 {
		sessions = append(sessions, sweepSession("s-steady-"+string(rune('a'+i)), "steady", sweepTestNow.Add(-6*time.Hour+time.Duration(i)*25*time.Minute), nil))
	}
	got := detectRestartLoops(sweepInput{Now: sweepTestNow, Since: 24 * time.Hour, Sessions: sessions}, 3)
	if len(got) != 1 || got[0].Agent != "looper" || got[0].Count != 5 {
		t.Fatalf("want one looper finding with 5 sessions, got %+v", got)
	}
	if !strings.Contains(got[0].Detail, "crash=5") || got[0].AgentBeadID != "gd-looper" {
		t.Errorf("finding lacks cause counts or agent bead: %+v", got[0])
	}
}

// TestDetectRestartLoopsCleanStops: a thread agent resumed after each clean
// gb stop is a stop -> reply -> resume cycle, not a restart loop
// (gd-fKRCOO3O8h); a gb stop that 403'd or failed leaves the next session
// counted.
func TestDetectRestartLoopsCleanStops(t *testing.T) {
	deny := "Error: HTTP 403: token not authorized for this bead's project"
	// eightPerHour returns 8 sessions 7 minutes apart and, before each next
	// session, a gb stop whose outcome stopOK picks by index.
	eightPerHour := func(stopOK func(i int) (event, stdout string, ok bool)) sweepInput {
		in := sweepInput{Now: sweepTestNow, Since: 3 * time.Hour}
		for i := range 8 {
			start := sweepTestNow.Add(-2*time.Hour + time.Duration(i)*7*time.Minute)
			in.Sessions = append(in.Sessions, sweepSession(fmt.Sprintf("s-%d", i), "thread", start, nil))
			event, stdout, ok := stopOK(i)
			if !ok {
				continue
			}
			ev := sweepBash("thread", "uuid", "cd /w && gb stop --wrapup-file /tmp/w.json", stdout, start.Add(5*time.Minute))
			ev.Event = event
			in.Hooks = append(in.Hooks, ev)
		}
		return in
	}
	clean := func(int) (string, string, bool) { return "PostToolUse", "stopped", true }
	if got := detectRestartLoops(eightPerHour(clean), 3); len(got) != 0 {
		t.Errorf("clean stops between sessions: want no finding, got %+v", got)
	}

	denied := func(int) (string, string, bool) { return "PostToolUse", deny, true }
	if got := detectRestartLoops(eightPerHour(denied), 3); len(got) != 1 || got[0].Count != 8 {
		t.Errorf("403'd stops: want one 8-session finding, got %+v", got)
	}
	failed := func(int) (string, string, bool) { return "PostToolUseFailure", "", true }
	if got := detectRestartLoops(eightPerHour(failed), 3); len(got) != 1 || got[0].Count != 8 {
		t.Errorf("failed stops: want one 8-session finding, got %+v", got)
	}

	// Mixed: sessions 1-5 follow a clean stop, 6 and 7 follow a 403'd one and
	// session 0 has no predecessor, so 3 count: not over the limit of 3 ...
	mixed := func(i int) (string, string, bool) {
		if i >= 5 {
			return "PostToolUse", deny, true
		}
		return "PostToolUse", "stopped", true
	}
	if got := detectRestartLoops(eightPerHour(mixed), 3); len(got) != 0 {
		t.Errorf("mixed, 3 unclean: want no finding, got %+v", got)
	}
	// ... while a 4th unclean one (session 3 follows no gb stop at all, the
	// missing-events case) crosses it.
	in := eightPerHour(mixed)
	s2stop := parseSweepTime(in.Sessions[2].Fields["started_at"]).Add(5 * time.Minute)
	in.Hooks = slices.DeleteFunc(in.Hooks, func(ev sweepHookEvent) bool { return ev.TS.Equal(s2stop) })
	got := detectRestartLoops(in, 3)
	if len(got) != 1 || got[0].Count != 4 || !slices.Equal(got[0].Sessions, []string{"s-0", "s-3", "s-6", "s-7"}) {
		t.Fatalf("mixed, 4 unclean: want s-0 s-3 s-6 s-7, got %+v", got)
	}
	if !strings.Contains(got[0].Detail, "4 after a clean stop and 0 after a predecessor before the bus replay not counted") {
		t.Errorf("detail lacks the clean-stop count: %q", got[0].Detail)
	}

	// A restart_cause always counts, even after a clean stop.
	in = eightPerHour(clean)
	for _, s := range in.Sessions {
		s.Fields["restart_cause"] = "oom"
	}
	if got := detectRestartLoops(in, 3); len(got) != 1 || got[0].Count != 8 {
		t.Errorf("oom restarts after clean stops: want one 8-session finding, got %+v", got)
	}
}

// TestDetectRestartLoopsBusWindow: a 24h sweep replays only the last few hours
// of the bus, so a predecessor that started before the replay's first event
// has no stop trace to read. That is unknown, not unclean (gd-QZe4Dmf8a4: a
// crew parked on gb wait ci and woken by escalations read as a loop).
func TestDetectRestartLoopsBusWindow(t *testing.T) {
	// fivePerHour returns 5 sessions 10 minutes apart from start, with no gb
	// stop on the bus, and one unrelated bus event at busFrom.
	fivePerHour := func(start, busFrom time.Time, fields map[string]string) sweepInput {
		in := sweepInput{Now: sweepTestNow, Since: 24 * time.Hour}
		for i := range 5 {
			f := map[string]string{}
			for k, v := range fields {
				f[k] = v
			}
			in.Sessions = append(in.Sessions, sweepSession(fmt.Sprintf("s-%d", i), "parked", start.Add(time.Duration(i)*10*time.Minute), f))
		}
		in.Hooks = []sweepHookEvent{sweepBash("other", "uuid", "git status", "", busFrom)}
		return in
	}
	busFrom := sweepTestNow.Add(-5 * time.Hour)

	before := fivePerHour(sweepTestNow.Add(-10*time.Hour), busFrom, nil)
	if got := detectRestartLoops(before, 3); len(got) != 0 {
		t.Errorf("predecessors before the bus window: want no finding, got %+v", got)
	}

	inside := fivePerHour(sweepTestNow.Add(-2*time.Hour), busFrom, nil)
	if got := detectRestartLoops(inside, 3); len(got) != 1 || got[0].Count != 5 {
		t.Errorf("predecessors inside the bus window, no stop trace: want one 5-session finding, got %+v", got)
	}

	caused := fivePerHour(sweepTestNow.Add(-10*time.Hour), busFrom, map[string]string{"restart_cause": "crash"})
	if got := detectRestartLoops(caused, 3); len(got) != 1 || got[0].Count != 5 {
		t.Errorf("restart_cause before the bus window: want one 5-session finding, got %+v", got)
	}

	// The window starts mid-loop: s-0..s-2 start before the bus, so s-1..s-3
	// (whose predecessors predate it) are unknown; s-0 and s-4 count.
	straddle := fivePerHour(sweepTestNow.Add(-2*time.Hour), sweepTestNow.Add(-2*time.Hour+25*time.Minute), nil)
	got := detectRestartLoops(straddle, 1)
	if len(got) != 1 || !slices.Equal(got[0].Sessions, []string{"s-0", "s-4"}) {
		t.Fatalf("bus window mid-loop: want s-0 s-4, got %+v", got)
	}
	if !strings.Contains(got[0].Detail, "0 after a clean stop and 3 after a predecessor before the bus replay not counted") {
		t.Errorf("detail lacks the pre-window count: %q", got[0].Detail)
	}
}

func TestDetectAuthStorms(t *testing.T) {
	agent := &beadsapi.BeadDetail{ID: "gd-crew", Fields: map[string]string{"agent": "crew", "task_id": "gd-task1"}}
	deny := "Error: HTTP 403: token not authorized for this bead's project"
	var hooks []sweepHookEvent
	for i := range 3 {
		hooks = append(hooks, sweepBash("crew", "sess-1", "kd show gd-task1", deny, sweepTestNow.Add(time.Duration(i)*time.Minute)))
	}
	// Code search output that merely contains "HTTP 403: " must not count.
	for range 5 {
		hooks = append(hooks, sweepBash("crew", "sess-2", `grep -rn "HTTP 403: " kbeads/`, `client.go:1: "HTTP 403: gd-task1"`, sweepTestNow))
	}
	// A 403 on someone else's bead is not an own-bead storm.
	for range 5 {
		hooks = append(hooks, sweepBash("crew", "sess-3", "kd close gd-other", deny, sweepTestNow))
	}
	// gb stop acts on the caller's own bead with no id on the command line.
	for range 3 {
		hooks = append(hooks, sweepBash("crew", "sess-4", "cd /w && gb stop --wrapup '{}'", deny, sweepTestNow))
	}
	for i := range hooks {
		hooks[i].AgentBeadID = "gd-crew"
	}
	got := detectAuthStorms(sweepInput{Agents: []*beadsapi.BeadDetail{agent}, Hooks: hooks}, 3)
	if len(got) != 2 {
		t.Fatalf("want sess-1 and sess-4, got %+v", got)
	}
	if got[0].Sessions[0] != "sess-1" || got[0].BeadID != "gd-task1" || got[0].Count != 3 {
		t.Errorf("sess-1 finding wrong: %+v", got[0])
	}
	if got[1].Sessions[0] != "sess-4" || got[1].BeadID != "gd-crew" {
		t.Errorf("sess-4 finding wrong: %+v", got[1])
	}
	if !strings.Contains(got[0].Detail, "HTTP 403: token not authorized") {
		t.Errorf("detail lacks the denial sample: %q", got[0].Detail)
	}
}

func TestDetectRepeatCallsIgnoresDescription(t *testing.T) {
	var hooks []sweepHookEvent
	for i := range 5 {
		ev := sweepBash("crew", "s1", "gh run list --branch x", "", sweepTestNow)
		in, _ := json.Marshal(map[string]string{"command": "gh run list --branch x", "description": strings.Repeat("d", i+1)})
		ev.ToolInput = in
		hooks = append(hooks, ev)
	}
	for range 4 {
		hooks = append(hooks, sweepBash("crew", "s1", "git status", "", sweepTestNow))
	}
	got := detectRepeatCalls(sweepInput{Hooks: hooks}, 5)
	if len(got) != 1 || got[0].Count != 5 || !strings.Contains(got[0].Detail, "gh run list --branch x") {
		t.Fatalf("want one 5x gh run list finding, got %+v", got)
	}
}

func TestDetectOOMs(t *testing.T) {
	in := sweepInput{
		Sessions: []*beadsapi.BeadDetail{
			sweepSession("s1", "a", sweepTestNow, map[string]string{"exit_code": "137"}),
			sweepSession("s2", "b", sweepTestNow, map[string]string{"exit_code": "143", "restart_cause": "sigterm"}),
		},
		Agents: []*beadsapi.BeadDetail{
			{ID: "gd-a", Fields: map[string]string{"agent": "a", "last_coop_exit_code": "137"}}, // already reported via s1
			{ID: "gd-c", Fields: map[string]string{"agent": "c", "last_coop_exit_code": "137", "last_coop_exit_reason": "OOMKilled"}},
		},
	}
	got := detectOOMs(in)
	if len(got) != 2 || got[0].Agent != "a" || got[1].Agent != "c" {
		t.Fatalf("want a (session) and c (agent), got %+v", got)
	}
}

func TestDetectDupComments(t *testing.T) {
	in := sweepInput{Now: sweepTestNow, Since: 24 * time.Hour, Comments: map[string][]beadsapi.Comment{
		"gd-t": {
			{Author: "crew", Text: "Still waiting on CI.", CreatedAt: sweepTestNow.Add(-time.Hour)},
			{Author: "crew", Text: "Still  waiting on CI.\n", CreatedAt: sweepTestNow.Add(-30 * time.Minute)},
			{Author: "other", Text: "Still waiting on CI.", CreatedAt: sweepTestNow.Add(-20 * time.Minute)},
			{Author: "crew", Text: "old", CreatedAt: sweepTestNow.Add(-48 * time.Hour)},
			{Author: "crew", Text: "old", CreatedAt: sweepTestNow.Add(-10 * time.Minute)},
		},
	}}
	got := detectDupComments(in)
	if len(got) != 1 || got[0].Agent != "crew" || got[0].Count != 2 || got[0].BeadID != "gd-t" {
		t.Fatalf("want one crew dup on gd-t, got %+v", got)
	}
}

func TestDetectSlowCallsKinds(t *testing.T) {
	calls, _ := json.Marshal([]beadsapi.SessionSlowCall{
		{Tool: "Bash", DurationMs: 3600_000, Args: "gh run watch 123 --exit-status"},
		{Tool: "Bash", DurationMs: 1900_000, Args: "GARDENHUB_GOLDENS=1 go test ./internal/gardenhub/"},
		{Tool: "Bash", DurationMs: 1500_000, Args: "GOFLAGS=-p=1 go build ./cmd/gb"},
		{Tool: "Bash", DurationMs: 900_000, Args: "until ! pgrep -f x; do sleep 5; done"},
		{Tool: "Bash", DurationMs: 700_000, Args: "python3 fix.py"},
		{Tool: "Bash", DurationMs: 30_000, Args: "gh run watch 1"},
	})
	in := sweepInput{Sessions: []*beadsapi.BeadDetail{sweepSession("s1", "a", sweepTestNow, map[string]string{"slowest_calls": string(calls)})}}
	got := detectSlowCalls(in, sweepThresholds{Slow: 10 * time.Minute})
	want := []string{"ci-wait", "goldens", "build-test", "wait-on-process", "other"}
	if len(got) != len(want) {
		t.Fatalf("want %d slow findings, got %+v", len(want), got)
	}
	for i, k := range want {
		if got[i].Kind != k {
			t.Errorf("finding %d: kind %q, want %q (%s)", i, got[i].Kind, k, got[i].Detail)
		}
	}
}

func TestDetectHarnessGaps(t *testing.T) {
	wrap := func(gaps string) string {
		b, _ := json.Marshal(map[string]string{"harness_gaps": gaps, "timestamp": sweepTestNow.Format(time.RFC3339)})
		return string(b)
	}
	in := sweepInput{Agents: []*beadsapi.BeadDetail{
		{ID: "gd-1", Fields: map[string]string{"agent": "one", "wrapup": wrap("The installed gb (2026.302.2) has no pr command, so I used gh. The PAT cannot rerun jobs (403).")}},
		{ID: "gd-2", Fields: map[string]string{"agent": "two", "wrapup": wrap("No nginx or docker daemon in the pod; I pulled the .deb by hand")}},
		{ID: "gd-3", Fields: map[string]string{"agent": "three", "wrapup": wrap("gh pr checks fails with this PAT (statusCheckRollup not accessible).")}},
		{ID: "gd-4", Fields: map[string]string{"agent": "four", "wrapup": wrap("no actionlint in the pod (fetched the release binary)")}},
	}}
	got := detectHarnessGaps(in)
	if len(got) != 3 {
		t.Fatalf("want one, two and four (missing tools), not the permission gaps; got %+v", got)
	}
	want := []struct{ agent, kind string }{{"one", "gb pr"}, {"two", "nginx or docker"}, {"four", "actionlint"}}
	for i, w := range want {
		if got[i].Agent != w.agent || got[i].Kind != w.kind {
			t.Errorf("finding %d: got %s/%q, want %s/%q", i, got[i].Agent, got[i].Kind, w.agent, w.kind)
		}
	}
}

func TestDetectRetriggerChurnCountsPushesOnly(t *testing.T) {
	at := func(i int) time.Time { return sweepTestNow.Add(time.Duration(i) * time.Minute) }
	var hooks []sweepHookEvent
	// desk: trial merges never pushed, and an unpushed merge of main.
	for i := range 4 {
		hooks = append(hooks, sweepBash("desk", "s-desk", "cd $T/pr-1; git -C $T/pr-1 merge --no-commit --no-ff origin/main >/dev/null 2>&1 && echo CLEAN; git merge --abort", "CLEAN", at(i)))
	}
	hooks = append(hooks, sweepBash("desk", "s-desk", "git merge --no-edit origin/main", "", at(5)))
	hooks = append(hooks, sweepBash("desk", "s-desk", "git merge-base HEAD origin/main; git push origin HEAD:agent/x", "", at(6)))
	// crew: an empty commit pushed three times, the last push in its own call.
	hooks = append(hooks,
		sweepBash("crew", "s-crew", `git commit -q --allow-empty -m "ci: retrigger" && git push -q`, "", at(1)),
		sweepBash("crew", "s-crew", `git commit -q --allow-empty -m "ci: retrigger" && git push -q 2>&1 | tail -1`, "", at(2)),
		sweepBash("crew", "s-crew", `git commit -q --allow-empty -m "ci: retrigger"`, "", at(3)),
		sweepBash("crew", "s-crew", `git push -q`, "", at(4)),
		sweepBash("crew", "s-crew", `git push -q`, "", at(5)),
	)
	got := detectRetriggerChurn(sweepInput{Now: sweepTestNow, Since: 24 * time.Hour, Hooks: hooks})
	if len(got) != 1 || got[0].Agent != "crew" || got[0].Count != 3 {
		t.Fatalf("want one crew finding with 3 pushes (no desk finding), got %+v", got)
	}
}

// TestSweepReadOnlyCallsSkipResponseMatchers: a grep that echoes the docs'
// sample EACCES text is not the browser install failing; the install is.
func TestSweepReadOnlyCallsSkipResponseMatchers(t *testing.T) {
	const eacces = "Failed to install browsers\nError: EACCES: permission denied, mkdir '/ms-playwright/__dirlock'"
	cases := []struct {
		cmd  string
		want bool
	}{
		{`grep -n "EACCES" gardenio/docs/session-sweep.md | head -5`, false},
		{`cd /home/agent/bot/gardenio/work && git grep -nE "EACCES|ms-playwright" -- Makefile 2>&1 | tail -3`, false},
		{`sed -n 30,45p gardenio/docs/session-sweep.md; cat Makefile 2>/dev/null | grep -A8 browser-deps`, false},
		{`npx playwright install chromium`, true},
		{`cd gardenio/tests/gardenhub-browser && npx playwright install 2>&1 | tail -4`, true},
		{`grep -c x Makefile > /home/agent/out.txt`, true},
		{`cat $(go env GOPATH)/x`, true},
		{`sed -i 's/a/b/' Makefile`, true},
		{`find . -name '*.md' -exec grep -l EACCES {} +`, true},
	}
	for _, c := range cases {
		ev := sweepBash("crew", "s-"+c.cmd, c.cmd, eacces, sweepTestNow)
		got := len(detectHookSignatures(sweepInput{Now: sweepTestNow, Since: 24 * time.Hour, Hooks: []sweepHookEvent{ev}})) == 1
		if got != c.want {
			t.Errorf("%q: finding = %v, want %v", c.cmd, got, c.want)
		}
	}
	read := sweepHookEvent{Agent: "crew", SessionID: "s-read", Event: "PostToolUse", ToolName: "Read",
		ToolInput: json.RawMessage(`{"file_path":"gardenio/docs/session-sweep.md"}`), ToolResponse: json.RawMessage(strconv.Quote(eacces)), TS: sweepTestNow}
	if got := detectHookSignatures(sweepInput{Now: sweepTestNow, Since: 24 * time.Hour, Hooks: []sweepHookEvent{read}}); len(got) != 0 {
		t.Errorf("a Read of the docs is not the install failing: %+v", got)
	}
}

func TestParseSweepBusLine(t *testing.T) {
	line := `data:{"stream":"hooks","type":"PostToolUseFailure","ts":"2026-10-30T02:07:34Z","payload":{"agent":"a","agent_bead_id":"gd-a","session_id":"s","tool_name":"Bash","tool_input":{"command":"kd show x"},"error":"Exit code 1\nHTTP 403: nope"}}`
	ev, ts, keep := parseSweepBusLine(line)
	if !keep || ev.Event != "PostToolUseFailure" || ev.SessionID != "s" || ts.IsZero() || ev.TS.IsZero() {
		t.Fatalf("bad parse: %+v %v %v", ev, ts, keep)
	}
	if !strings.Contains(sweepResponseText(ev), "HTTP 403: nope") {
		t.Errorf("error text not in response text")
	}
	_, ts, keep = parseSweepBusLine(`data:{"type":"CwdChanged","ts":"2026-10-30T02:07:35Z","payload":{}}`)
	if keep || ts.IsZero() {
		t.Errorf("non-tool event should report ts but not keep")
	}
	if _, _, keep := parseSweepBusLine(": keepalive"); keep {
		t.Errorf("comment line kept")
	}
}

// sweepFakeDaemon is the slice of the beads daemon --file touches: list bugs
// by type, labels, status and closed_after, create a bug, read and add comments, add a dependency.
type sweepFakeDaemon struct {
	mu       sync.Mutex
	bugs     []map[string]any
	comments map[string][]beadsapi.Comment
	posted   int // comments added
}

func newSweepFakeDaemon(t *testing.T) *sweepFakeDaemon {
	t.Helper()
	d := &sweepFakeDaemon{comments: map[string][]beadsapi.Comment{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		switch {
		case r.Method == http.MethodGet && path == "/v1/beads":
			q := r.URL.Query()
			want := strings.Split(q.Get("labels"), ",")
			closedAfter, _ := time.Parse(time.RFC3339, q.Get("closed_after"))
			out := []map[string]any{}
			for _, b := range d.bugs {
				if q.Get("type") != "bug" || !labelsHaveAll(b["labels"].([]string), want) {
					continue
				}
				if q.Get("status") != "" && !slices.Contains(strings.Split(q.Get("status"), ","), b["status"].(string)) {
					continue
				}
				if at, _ := b["closed_at"].(time.Time); !closedAfter.IsZero() && !at.After(closedAfter) {
					continue
				}
				out = append(out, b)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": out, "total": len(out)})
		case r.Method == http.MethodPost && path == "/v1/beads":
			var req beadsapi.CreateBeadRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			b := map[string]any{"id": fmt.Sprintf("gd-bug%d", len(d.bugs)+1), "type": req.Type, "status": "open",
				"title": req.Title, "description": req.Description, "labels": req.Labels}
			d.bugs = append(d.bugs, b)
			_ = json.NewEncoder(w).Encode(b)
		case strings.HasSuffix(path, "/comments"):
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/v1/beads/"), "/comments")
			if r.Method == http.MethodPost {
				var c beadsapi.Comment
				_ = json.NewDecoder(r.Body).Decode(&c)
				d.comments[id] = append(d.comments[id], c)
				d.posted++
				_, _ = w.Write([]byte("{}"))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"comments": d.comments[id]})
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/dependencies"):
			_, _ = w.Write([]byte("{}"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	old, oldProject, oldParent := daemon, sweepProject, sweepParent
	daemon, sweepProject, sweepParent = c, "gardenio", "gd-plan"
	t.Cleanup(func() { daemon, sweepProject, sweepParent = old, oldProject, oldParent })
	return d
}

func labelsHaveAll(have, want []string) bool {
	for _, w := range want {
		if !slices.Contains(have, w) {
			return false
		}
	}
	return true
}

// sweepDriftInput builds one sweep's input. extra grows every count (more
// loop sessions, denials, repeats, duplicate comments) and agedOut drops the
// loop's oldest session, the way a later sweep sees the same evidence.
func sweepDriftInput(now time.Time, extra int, agedOut bool) sweepInput {
	in := sweepInput{Now: now, Since: 24 * time.Hour, Comments: map[string][]beadsapi.Comment{}}
	in.Agents = []*beadsapi.BeadDetail{{ID: "gd-crew", Fields: map[string]string{"agent": "crew", "task_id": "gd-task1"}}}
	loopStart := sweepTestNow.Add(-3 * time.Hour)
	for i := range 5 + extra {
		if agedOut && i == 0 {
			continue
		}
		in.Sessions = append(in.Sessions, sweepSession(fmt.Sprintf("s-loop-%d", i), "looper", loopStart.Add(time.Duration(i)*8*time.Minute), map[string]string{"restart_cause": "crash"}))
	}
	in.Sessions = append(in.Sessions, sweepSession("s-oom", "crew", sweepTestNow.Add(-time.Hour), map[string]string{"exit_code": "137"}))
	// Denials cycle through three commands so no one of them also crosses
	// the repeat-call bar as the count grows.
	denied := []string{"kd show gd-task1", "kd update gd-task1 --status=in_progress", "kd close gd-task1"}
	for i := range 3 + extra {
		in.Hooks = append(in.Hooks, sweepBash("crew", "sess-auth", denied[i%len(denied)], "HTTP 403: token not authorized", sweepTestNow.Add(time.Duration(i)*time.Second)))
	}
	for i := range 5 + extra {
		in.Hooks = append(in.Hooks, sweepBash("crew", "sess-auth", "gh run list --branch x", "", sweepTestNow.Add(time.Duration(i)*time.Second)))
	}
	for i := range in.Hooks {
		in.Hooks[i].AgentBeadID = "gd-crew"
	}
	for i := range 2 + extra {
		in.Comments["gd-task1"] = append(in.Comments["gd-task1"], beadsapi.Comment{Author: "crew", Text: "Still waiting on CI.", CreatedAt: sweepTestNow.Add(-time.Duration(i+1) * time.Minute)})
	}
	return in
}

// TestSweepFileCommentsOnlyNewEvidence runs --file twice over real detector
// output whose counts drift between sweeps (the lossy bus replay, a loop that
// keeps going, old sessions aging out) and asserts the second pass is silent.
func TestSweepFileCommentsOnlyNewEvidence(t *testing.T) {
	d := newSweepFakeDaemon(t)
	th := sweepThresholds{RestartMax: 3, AuthMin: 3, RepeatMin: 5, Slow: 10 * time.Minute}
	report := func(in sweepInput) sweepReport {
		return sweepReport{Now: in.Now, Since: "24h0m0s", Findings: sweepDetect(in, th)}
	}
	first := report(sweepDriftInput(sweepTestNow, 0, false))
	filed, err := fileSweepFindings(context.Background(), first)
	if err != nil {
		t.Fatalf("first --file: %v", err)
	}
	want := []string{sweepRestartLoop, sweepAuthStorm, sweepOOM, sweepRepeatCall, sweepDupComment}
	if len(filed) != len(want) {
		t.Fatalf("first pass: want %d bugs created, got %+v", len(want), filed)
	}
	for i, p := range want {
		if filed[i].Pattern != p || filed[i].Action != "created" {
			t.Errorf("first pass %d: got %+v, want %s created", i, filed[i], p)
		}
	}

	// Next cycle: every count moved and the loop's oldest session aged out.
	second := report(sweepDriftInput(sweepTestNow.Add(30*time.Minute), 4, true))
	details := map[string]string{}
	for _, f := range first.Findings {
		details[f.Pattern] = f.Detail
	}
	for _, f := range second.Findings {
		if f.Pattern != sweepOOM && details[f.Pattern] == f.Detail {
			t.Fatalf("%s detail did not drift, so the test proves nothing: %q", f.Pattern, f.Detail)
		}
	}
	filed, err = fileSweepFindings(context.Background(), second)
	if err != nil {
		t.Fatalf("second --file: %v", err)
	}
	for _, fl := range filed {
		if fl.Action != "unchanged" {
			t.Errorf("second pass re-reported the same evidence: %+v", fl)
		}
	}
	if d.posted != 0 {
		t.Fatalf("second pass posted %d comment(s), want none: %+v", d.posted, d.comments)
	}

	// Real new evidence (another session repeating a call) still gets a comment.
	in := sweepDriftInput(sweepTestNow.Add(time.Hour), 4, true)
	for range 5 {
		ev := sweepBash("crew", "sess-new", "git status", "", sweepTestNow)
		ev.AgentBeadID = "gd-crew"
		in.Hooks = append(in.Hooks, ev)
	}
	filed, err = fileSweepFindings(context.Background(), report(in))
	if err != nil {
		t.Fatalf("third --file: %v", err)
	}
	if d.posted != 1 {
		t.Fatalf("third pass: want one comment for the new repeat-call, got %d: %+v", d.posted, filed)
	}
	for _, fl := range filed {
		if fl.Pattern == sweepRepeatCall && (fl.Action != "commented" || fl.New != 1) {
			t.Errorf("repeat-call: want 1 new piece commented, got %+v", fl)
		}
	}
}

// TestSweepFileSkipsClosedBugEvidence: a gardener closed every sweep bug
// (stale, false positive), so the next sweep over the same evidence files
// nothing, while a new key still files a fresh bug and a bug closed before the
// window no longer holds its evidence back.
func TestSweepFileSkipsClosedBugEvidence(t *testing.T) {
	d := newSweepFakeDaemon(t)
	th := sweepThresholds{RestartMax: 3, AuthMin: 3, RepeatMin: 5, Slow: 10 * time.Minute}
	report := func(in sweepInput) sweepReport {
		return sweepReport{Now: in.Now, Since: "24h0m0s", Findings: sweepDetect(in, th)}
	}
	filed, err := fileSweepFindings(context.Background(), report(sweepDriftInput(sweepTestNow, 0, false)))
	if err != nil {
		t.Fatalf("first --file: %v", err)
	}
	created := map[string]string{}
	for _, fl := range filed {
		created[fl.Pattern] = fl.BeadID
	}
	for _, b := range d.bugs {
		b["status"], b["closed_at"] = "closed", sweepTestNow.Add(time.Hour)
		if slices.Contains(b["labels"].([]string), "session-sweep:"+sweepOOM) {
			b["closed_at"] = sweepTestNow.Add(-24 * time.Hour)
		}
	}
	bugs := len(d.bugs)

	in := sweepDriftInput(sweepTestNow.Add(2*time.Hour), 4, true)
	for range 5 {
		ev := sweepBash("crew", "sess-new", "git status", "", sweepTestNow)
		ev.AgentBeadID = "gd-crew"
		in.Hooks = append(in.Hooks, ev)
	}
	filed, err = fileSweepFindings(context.Background(), report(in))
	if err != nil {
		t.Fatalf("second --file: %v", err)
	}
	for _, fl := range filed {
		switch fl.Pattern {
		case sweepRepeatCall:
			if fl.Action != "created" || fl.New != 1 {
				t.Errorf("repeat-call: want a fresh bug for the one new key, got %+v", fl)
			}
		case sweepOOM:
			if fl.Action != "created" {
				t.Errorf("oom: its bug closed before the window, so it should file again, got %+v", fl)
			}
		default:
			if fl.Action != "unchanged" || fl.BeadID != created[fl.Pattern] {
				t.Errorf("%s: want unchanged naming closed %s, got %+v", fl.Pattern, created[fl.Pattern], fl)
			}
		}
	}
	if len(d.bugs) != bugs+2 {
		t.Fatalf("want 2 fresh bugs (repeat-call, oom), got %d", len(d.bugs)-bugs)
	}
	if desc := d.bugs[len(d.bugs)-1]["description"].(string); !strings.Contains(desc, "Evidence (1)") {
		t.Errorf("fresh repeat-call bug should carry only the new evidence:\n%s", desc)
	}
}

func TestSweepSeenKeysCapped(t *testing.T) {
	var many []sweepFinding
	for i := range sweepSeenMax + 10 {
		many = append(many, sweepFinding{Pattern: sweepSlowCall, Agent: string(rune(i))})
	}
	keys := joinSeen(nil, many)
	if n := len(strings.Split(keys, ",")); n != sweepSeenMax {
		t.Errorf("seen list not capped: %d", n)
	}
	if got := sweepSeenKeys("x\n\n`sweep-keys: "+keys+"`", []beadsapi.Comment{{Text: "no keys here"}}); len(got) != sweepSeenMax {
		t.Errorf("sweepSeenKeys read back %d keys, want %d", len(got), sweepSeenMax)
	}
}

// TestDetectInterruptCrashesFromSessionFields covers the session bead's
// interrupt fields, which only beads closed by a gb with gd-3HAERLWMLu carry,
// so no archived fixture has them yet.
func TestDetectInterruptCrashesFromSessionFields(t *testing.T) {
	start := sweepTestNow.Add(-time.Hour)
	at := func(d time.Duration) string { return start.Add(d).Format(time.RFC3339) }
	crash := func(id string, f map[string]string) *beadsapi.BeadDetail {
		fields := map[string]string{"exit_code": "143", "restart_cause": "crash", "tool_call_count": "11", "ended_at": at(71 * time.Second)}
		for k, v := range f {
			fields[k] = v
		}
		return sweepSession(id, id, start, fields)
	}
	in := sweepInput{Now: sweepTestNow, Since: 24 * time.Hour, Sessions: []*beadsapi.BeadDetail{
		// Interrupted between turns, exit 143 five seconds later.
		crash("interrupted", map[string]string{"interrupt_count": "1", "last_interrupt_at": at(66 * time.Second)}),
		// Counted and none: gb leaves interrupt_count out when it is 0, so a
		// crash ending after the cutoff with no interrupt is not this class,
		// even though its shape matches the bead-only fallback.
		sweepSession("counted-none", "counted-none", sweepInterruptCountedSince.Add(time.Hour), map[string]string{
			"exit_code": "143", "restart_cause": "crash", "tool_call_count": "11",
			"ended_at": sweepInterruptCountedSince.Add(time.Hour + 71*time.Second).Format(time.RFC3339),
		}),
		// The interrupt was long before the exit.
		crash("interrupt-early", map[string]string{"interrupt_count": "1", "last_interrupt_at": at(0), "ended_at": at(20 * time.Minute)}),
		// An interrupt, then a graceful stop.
		crash("interrupt-exit-0", map[string]string{"interrupt_count": "1", "last_interrupt_at": at(66 * time.Second), "exit_code": "0"}),
	}}
	var got []string
	for _, f := range detectInterruptCrashes(in) {
		got = append(got, f.Agent+": "+f.Detail)
	}
	if len(got) != 1 || !strings.HasPrefix(got[0], "interrupted: user interrupt at ") {
		t.Errorf("want one user-interrupt finding for interrupted, got %q", got)
	}
}

// TestSweepPodTerminateIsNotInterruptCrash: a pod terminate's preStop and
// coop's shutdown drain press Escape, which reads as a user interrupt
// (gd-Ga8iHjLaYR). The session is skipped when gb labelled it sigterm, or when
// an older gb labelled it crash but its agent bead closed context_cancelled
// right after it; killed-mid-task then counts a sigterm session whose pod was
// terminated mid-work.
func TestSweepPodTerminateIsNotInterruptCrash(t *testing.T) {
	start := sweepTestNow.Add(-time.Hour)
	at := func(d time.Duration) time.Time { return start.Add(d) }
	sess := func(id, cause string, end time.Duration) *beadsapi.BeadDetail {
		return sweepSession(id, id, start, map[string]string{
			"exit_code": "143", "restart_cause": cause, "tool_call_count": "11", "interrupt_count": "1",
			"last_interrupt_at": at(end - 4*time.Second).Format(time.RFC3339), "ended_at": at(end).Format(time.RFC3339),
			"session_log": "/logs/" + id + ".jsonl",
		})
	}
	agent := func(id, reason string, closed time.Time) *beadsapi.BeadDetail {
		return &beadsapi.BeadDetail{ID: "gd-" + id, Type: "agent", Status: "closed", ClosedAt: closed.Format(time.RFC3339Nano),
			Fields: map[string]string{"agent": id, "close_reason": reason}}
	}
	const involuntary = "context_cancelled_involuntary: pod terminated mid-work before gb done; reopened gd-x"
	in := sweepInput{Now: sweepTestNow, Since: 24 * time.Hour,
		Sessions: []*beadsapi.BeadDetail{
			sess("old-gb-pod-killed", "crash", 2*time.Minute),
			sess("new-gb-pod-killed", "sigterm", 20*time.Minute),
			sess("sigterm-no-close", "sigterm", 20*time.Minute),
			sess("crash-late-close", "crash", 2*time.Minute),
		},
		Agents: []*beadsapi.BeadDetail{
			agent("old-gb-pod-killed", involuntary, at(2*time.Minute+time.Second)),
			agent("new-gb-pod-killed", involuntary, at(20*time.Minute+time.Second)),
			// Closed context_cancelled, but long after this session: a later
			// incarnation's teardown, so this crash is still flagged.
			agent("crash-late-close", involuntary, at(time.Hour)),
		},
	}
	for _, id := range []string{"new-gb-pod-killed", "sigterm-no-close"} {
		in.Hooks = append(in.Hooks, sweepBash(id, id, "go build ./...", "", at(10*time.Minute)))
	}
	var interrupts []string
	for _, f := range detectInterruptCrashes(in) {
		interrupts = append(interrupts, f.Agent)
	}
	if !slices.Equal(interrupts, []string{"crash-late-close"}) {
		t.Errorf("interrupt-then-crash flagged %q, want only crash-late-close", interrupts)
	}
	var killed []string
	for _, f := range detectKilledMidTask(in) {
		killed = append(killed, f.Agent+"["+f.Kind+"]")
	}
	if !slices.Equal(killed, []string{"new-gb-pod-killed[sigterm]"}) {
		t.Errorf("killed-mid-task flagged %q, want only new-gb-pod-killed[sigterm]", killed)
	}
}

func TestSweepRefusedCaller(t *testing.T) {
	const refusal = `HTTP 403: ownership denied: update on bead gd-x requires created_by, assignee, owner, or admin role — you are `
	for _, c := range []struct {
		text, agent string
		want        bool
	}{
		{refusal + `"thread-1"; this bead is created_by="thread-0"`, "thread-1", true},
		{refusal + `\"thread-1\"; this bead is created_by=\"thread-0\"`, "thread-1", true}, // relay-cut JSON prefix
		{refusal + `"thread-0"; this bead is created_by="thread-1"`, "thread-1", false},
		{refusal + `"thread-12"; this bead is created_by="thread-0"`, "thread-1", false},
		{refusal + `"thread-1"`, "", false},
		{`HTTP 403: ownership denied: update on bead gd-x requires ...`, "thread-1", false},
	} {
		if got := sweepRefusedCaller(c.text, c.agent); got != c.want {
			t.Errorf("sweepRefusedCaller(%q, %q) = %v, want %v", c.text, c.agent, got, c.want)
		}
	}
}
