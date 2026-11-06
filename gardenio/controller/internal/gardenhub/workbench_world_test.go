package gardenhub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/commons"
	"gardenio/controller/internal/commons/commonstest"
)

// The Commons v2 workbench's roots in the golden farm's Commons (plan gd-lRaCxsiLvC step 4): a plan in two phases with a
// steward, its narrative and its asks; a plain epic; and #general's gd-wishPorch as the thread they were asked in. All in
// gardenio, on the daemon the Commons goldens read, so every Commons shot shows one farm.
const (
	wbPlan, wbPhase, wbEpic, wbThread, wbCrew = "gd-wbPlan", "gd-wbPhase1", "gd-wbEpic", "gd-wishPorch", "gd-wbCrew3"
	// gardenio's steward (steward view S2, gd-LisW87zrnf): its trigger and its live run
	wbSteward, wbStewRun = "gd-wbSteward", "gd-wbStewRun"
)

func workbenchWorld(d *commonstest.Daemon, now time.Time) {
	at := func(m int) time.Time { return now.Add(-time.Duration(m) * time.Minute) }
	put := func(id, typ, status, title string, m int, f ...string) {
		fields := map[string]string{}
		for i := 0; i+1 < len(f); i += 2 {
			fields[f[i]] = f[i+1]
		}
		labels := []string{"project:gardenio"}
		if fields["labels"] != "" {
			labels = append(labels, strings.Split(fields["labels"], ",")...)
			delete(fields, "labels")
		}
		d.Put(&beadsapi.BeadDetail{ID: id, Type: typ, Status: status, Title: title, Labels: labels, CreatedAt: at(m), UpdatedAt: at(m), Fields: fields,
			Description: "Build " + title + " inside the step-1 budgets, and prove it with the harness."})
	}
	put(wbPlan, "epic", "open", "Commons v2: a workbench for any bead and its children", 300, "labels", "plan", "plan_state", "building",
		"phase_order", `["gd-wbPhase1","gd-wbPhase2"]`, "step_order", `["gd-wbS7"]`, "thread_id", wbThread)
	put("gd-wbBundle", "bundle", "open", "Plan narrative: Commons v2", 299)
	put("gd-wbDoc", "doc", "open", "Commons v2: plan narrative", 299, "format", "md", "content", "## What and why\n\n"+
		"The band gives way to a workbench that opens on any bead with children: a plan and its steps, a phase, an epic, a thread.\n\n"+
		"| # | Step | State |\n|---|---|---|\n| 1 | Latency budgets | Done |\n| 2 | One stream | Done |\n| 3 | The node model | Building |\n")
	d.Link("gd-wbBundle", wbPlan, "parent-child")
	d.Link("gd-wbDoc", "gd-wbBundle", "parent-child")
	put("gd-wbGard", "trigger", "open", "Plan gardener for Commons v2", 299, "labels", commons.GardenerEpicLabel+wbPlan+","+commons.PlanGardenerLabel, "enabled", "true")
	steps := [][3]string{{"gd-wbS1", "Latency budgets and the harness", "closed"}, {"gd-wbS2", "One stream that carries content", "closed"},
		{"gd-wbS3", "The node model: any bead and its children", "in_progress"}, {"gd-wbS4", "The workbench on the desktop", "open"},
		{"gd-wbS5", "The transcript as documents", "open"}, {"gd-wbS6", "The phone: a push stack", "open"}, {"gd-wbS7", "The perf gate in CI", "open"}}
	put(wbPhase, "epic", "open", "Phase 1: the stream and the model", 298, "step_order", `["gd-wbS1","gd-wbS2","gd-wbS3","gd-wbS4"]`)
	put("gd-wbPhase2", "epic", "open", "Phase 2: the workbench", 297, "step_order", `["gd-wbS5","gd-wbS6"]`)
	d.Link(wbPhase, wbPlan, "parent-child")
	d.Link("gd-wbPhase2", wbPlan, "parent-child")
	for i, s := range steps {
		parent := []string{wbPhase, wbPhase, wbPhase, wbPhase, "gd-wbPhase2", "gd-wbPhase2", wbPlan}[i]
		put(s[0], "task", s[2], s[1], 290-i, "thread_id", wbThread)
		d.Link(s[0], parent, "parent-child")
		if i > 0 {
			d.Link(s[0], steps[i-1][0], "blocks")
		}
		if s[2] == "open" {
			continue
		}
		crew, state := fmt.Sprintf("gd-wbCrew%d", i+1), map[bool]string{true: "done", false: "working"}[s[2] == "closed"]
		put(crew, "agent", map[bool]string{true: "closed", false: "open"}[s[2] == "closed"], "gardenio-crew-"+s[0][5:], 280-i,
			"agent", "gardenio-crew-"+s[0][5:], "role", "crew", "agent_state", state, "thread_id", wbThread)
		d.Link(crew, s[0], "assigned")
		mr := "gd-wbMr" + s[0][5:]
		put(mr, "merge-request", map[bool]string{true: "closed", false: "open"}[s[2] == "closed"], fmt.Sprintf("PR #%d: %s", 1795+i, s[1]), 270-i,
			"mr_url", fmt.Sprintf("https://github.com/acme/app/pull/%d", 1795+i), "mr_state", map[bool]string{true: "merged", false: "opened"}[s[2] == "closed"])
		d.Link(s[0], mr, "produces")
	}
	put("gd-wbAsk", "decision", "open", "Serve jobs from memory or from the agent bead?", 200, "prompt", "Serve jobs from memory or from the agent bead?")
	d.Link("gd-wbS3", "gd-wbAsk", "blocks")
	put(wbEpic, "epic", "open", "Chart lints again", 260, "thread_id", wbThread)
	for i, t := range []string{"Fix the values schema", "Pin the lint image", "Add the box values to the schema", "Lint in the chart job"} {
		put(fmt.Sprintf("gd-wbEt%d", i+1), "task", map[bool]string{true: "closed", false: "open"}[i < 2], t, 259-i, "thread_id", wbThread)
		d.Link(fmt.Sprintf("gd-wbEt%d", i+1), wbEpic, "parent-child")
	}
	// the rubric's stress (F1-F4, F6) for the commons-ux-acceptance shoot's commonsplanstress
	put("gd-wbStress", "epic", "open", strings.Repeat("A very long epic title that goes on, ", 5)+"and on", 250)
	for i := range 40 {
		t := []string{"Fix https://github.com/acme/app/blob/9f3c2a1e8b7d6c5f4e3a2b1c0d9e8f7a6b5c4d3e/internal/commons/node.go#L256", "提交工作台 🚀👩🏽‍💻", "إصلاح الشجرة",
			"Rename feature/workbench-desktop-tree-rail-and-rails-for-every-root", "Short"}[i%5] + fmt.Sprintf(" (%d)", i+1)
		put(fmt.Sprintf("gd-wbX%d", i+1), "task", []string{"open", "in_progress", "closed", "blocked"}[i%4], t, 249-i)
		d.Link(fmt.Sprintf("gd-wbX%d", i+1), "gd-wbStress", "parent-child")
	}
	d.Link("gd-wbGone", "gd-wbStress", "parent-child")

	// gardenio's steward (steward view S2): its trigger, its live run, a decision it asks and one answered, and two #general
	// threads that address it, the first answered by it and by a crew, the second by it alone. Older than every other
	// #general message, and the viewer's own, so no unread count moves.
	put(wbSteward, "trigger", "open", "gardenio steward", 900, "labels", "steward,gardener-land:gardenio", "enabled", "true")
	put(wbStewRun, "agent", "open", "gardenio-steward-k3x9", 20, "agent", "gardenio-steward-k3x9", "role", "steward", "agent_state", "working")
	for _, dc := range [][3]string{{"gd-wbStewAsk", "open", "Start phase 2 now, or after the perf gate?"}, {"gd-wbStewDone", "closed", "Keep the band until step 8?"}} {
		d.Put(&beadsapi.BeadDetail{ID: dc[0], Type: "decision", Status: dc[1], Title: dc[2], Labels: []string{"project:gardenio"}, CreatedBy: "gardenio-steward-k3x9",
			CreatedAt: at(400), UpdatedAt: at(400), Fields: map[string]string{"prompt": dc[2]}})
	}
	for _, m := range [][5]string{
		{"gd-msgSw1", "", "oidc:matt@example.com", "@steward what is stalled in Commons v2, and why?", "430"},
		{"gd-msgSw1a", "gd-msgSw1", "agent:gardenio-steward-k3x9", "Step 3 waits on your answer: serve jobs from memory or from the agent bead. Its crew is idle until then; the pane it builds on is gd-b4IDsCKWbB.", "428"},
		{"gd-msgSw1b", "gd-msgSw1", "agent:gardenio-29-the-commons-nec", "I can take step 4 once step 3 lands.", "425"},
		{"gd-msgSw2", "", "oidc:matt@example.com", "@steward hold chart merges until Friday's release, please.", "410"},
		{"gd-msgSw2a", "gd-msgSw2", "agent:gardenio-steward-k3x9", "Added to my Holds: no chart merges before Friday's release.", "408"},
	} {
		ago, _ := strconv.Atoi(m[4])
		f := map[string]string{"channel_id": "gd-chGeneral", "author": m[2], "text": m[3]}
		if m[1] != "" {
			f["thread_id"] = m[1]
		}
		d.Put(&beadsapi.BeadDetail{ID: m[0], Type: "message", Status: "closed", Title: m[3], Labels: []string{"project:gardenio"}, CreatedAt: at(ago), UpdatedAt: at(ago), Fields: f})
	}
}

// wbStewards is the Commons' Stewards seam for the fixture (beads-viewer's reads steward.LoadLand): gardenio's steward, at
// work on a run, tending the plan and a stalled epic, with a step parked at the purse wall and its notes; and the stress
// land's (stewardStress, gd-r7JiBQdZNS). Any other land but the home garden ("") has none.
type wbStewards struct{ now func() time.Time }

func (s wbStewards) Steward(_ context.Context, land string) (*commons.StewardLand, error) {
	if land == swLand {
		return stewardStress(s.now()), nil
	}
	if land != "" && land != "gardenio" {
		return nil, nil
	}
	day := func(n int) string { return s.now().AddDate(0, 0, n).UTC().Format(time.DateOnly) }
	return &commons.StewardLand{ID: wbSteward, Name: "gardenio-steward", Land: "gardenio", Home: true, General: "gd-chGeneral",
		State: commons.StewardWorking, Reason: "Tending Commons v2: step 3 waits on an answer", Run: wbStewRun, RunsToday: 3, MaxRunsPerDay: 12, Spend: "$1.84 today",
		NotesID: "gd-wbNotes", Notes: "## Holds\n- no chart merges before Friday's release\n- never merge a PR that touches billing\n\n" +
			"## Answers (newest first)\n- jobs are served from memory (matt)\n\n## Promises\n- " + day(-2) + ": report on Commons v2 step 3\n- " + day(7) +
			": tidy the plan narrative\n\n## In flight\n- Commons v2: a crew on step 3\n",
		Plans: []commons.StewardPlan{{ID: wbPlan, Title: "Commons v2: a workbench for any bead and its children", State: "building"},
			{ID: wbEpic, Title: "Chart lints again", State: "open", Stalled: true, Why: "no gardener: nothing moved in 2 days"}},
		Parked: []commons.StewardStep{{ID: "gd-wbS4", Title: "The workbench on the desktop", Agent: "gardenio-crew-wbS4"}}}, nil
}

// workbenchNamed puts on the garden's bead reader (the one GET /garden/names reads, not the Commons daemon) a bead under no
// tree, named by its short title: the id the workbench browser test shows that nothing it loaded titled (gd-QTukIA8Nvv).
func workbenchNamed(beads *fakeBeads) {
	beads.beads["gd-wbNamedOne"] = &beadsapi.BeadDetail{ID: "gd-wbNamedOne", Type: "task", Status: "open", Labels: []string{"project:gardenio"},
		Title: "A bead the workbench names only through GET /garden/names", ShortTitle: "named by short title"}
}

// workbenchJobs is the jobs record with the step-3 crew's last calls in it, two done and one running.
func workbenchJobs(d *commonstest.Daemon, now time.Time) *commons.Jobs {
	j := commons.NewJobs(d, nil, nil)
	j.Start()
	for i, e := range []map[string]any{
		{"event": "PostToolUse", "tool_name": "Bash", "tool_input": map[string]any{"command": "go test ./internal/commons/ -run TestNode -count=1"}, "duration_ms": 2300, "tool_response": map[string]any{"stdout": "ok  \tgardenio/controller/internal/commons\t2.284s"}},
		{"event": "PostToolUse", "tool_name": "Bash", "tool_input": map[string]any{"command": "scripts/ratchets/line-budget.sh check"}, "duration_ms": 41000, "tool_response": map[string]any{"stdout": "line-budget: all 204 Go packages are within budget"}},
		{"event": "PreToolUse", "tool_name": "Bash", "tool_input": map[string]any{"command": "golangci-lint run ./internal/commons/..."}},
	} {
		e["agent_bead_id"], e["tool_use_id"], e["ts"] = wbCrew, fmt.Sprintf("tu%d", i), now.Add(time.Duration(i-3)*time.Minute).Format(time.RFC3339Nano)
		b, _ := json.Marshal(e)
		j.Observe(b)
	}
	b, _ := json.Marshal(map[string]any{"event": "PostToolUse", "tool_name": "Bash", "tool_input": map[string]any{"command": "kd show gd-wbS3"}, "duration_ms": 640,
		"agent_bead_id": wbStewRun, "tool_use_id": "tus1", "ts": now.Add(-2 * time.Minute).Format(time.RFC3339Nano), "tool_response": map[string]any{"stdout": "gd-wbS3 The node model: in_progress"}})
	j.Observe(b)
	// the stress steward's run (gd-r7JiBQdZNS): one call with F2 tokens in its command and its output
	b, _ = json.Marshal(map[string]any{"event": "PostToolUse", "tool_name": "Bash", "tool_input": map[string]any{"command": "gh pr checks " + uxURL + " --sha " + uxSHA},
		"duration_ms": 1200, "agent_bead_id": swRun, "tool_use_id": "tusw1", "ts": now.Add(-time.Minute).Format(time.RFC3339Nano), "tool_response": map[string]any{"stdout": uxTokens}})
	j.Observe(b)
	for end := time.Now().Add(5 * time.Second); (j.Count(wbCrew) < 3 || j.Count(wbStewRun) < 1 || j.Count(swRun) < 1) && time.Now().Before(end); {
		time.Sleep(5 * time.Millisecond)
	}
	return j
}

// The workbench in a real browser (Commons v2 step 4, gd-sXB0CyaqTA): testdata/workbench_browser.js walks its keys, the row
// menu, find and jump, Up and Back, the farm and back, the composer, a node event patching its row, and (step 5) the
// holder's state on the reply line and an agent's live head.
func TestWorkbenchUnderChromium(t *testing.T) {
	underChromium(t, "workbench_browser.js", func() *httptest.Server { return workbenchRig(t) })
}

// The workbench kept instant (step 6, gd-Txos7COlj0): testdata/workbench_fast_browser.js checks the optimistic send and its
// Retry, drafts per node, warm switches and prefetch by their requests, the typing signal and unread marks from the stream.
func TestWorkbenchFastUnderChromium(t *testing.T) {
	underChromium(t, "workbench_fast_browser.js", func() *httptest.Server { return workbenchRig(t) })
}

// The optimistic send's edges (gd-JpYV1FWtnZ): testdata/workbench_send_browser.js checks that a Retry of a send that was only
// slow writes one message, and that a send refused or held after its node was left leaves no stuck row.
func TestWorkbenchSendUnderChromium(t *testing.T) {
	underChromium(t, "workbench_send_browser.js", func() *httptest.Server { return workbenchRig(t) })
}

// The steward view on the desktop (steward view S2, gd-LisW87zrnf): testdata/workbench_steward_browser.js checks the
// Steward row and g s, the steward root's head, tree, rails and conversation, Holds edited in place, "Ask the steward about
// this" and its send, and the steward's own addresses.
func TestWorkbenchStewardUnderChromium(t *testing.T) {
	underChromium(t, "workbench_steward_browser.js", func() *httptest.Server { return workbenchRig(t) })
}

// The band's old addresses (gd-3NuXXEFSgk): testdata/commons_old_address_browser.js checks that a channel's address and
// /garden/c land on the steward in a browser, plain and under the goldens' pin, which fulfills Commons navigations itself.
func TestCommonsOldAddressUnderChromium(t *testing.T) {
	underChromium(t, "commons_old_address_browser.js", func() *httptest.Server {
		return cardWorld(t, func() time.Time { return goldenNow }, goldenPorch()...)
	}, strconv.FormatInt(goldenNow.UnixMilli(), 10))
}

// The steward on a phone (steward view S3, gd-qV1nCdpbVA): testdata/workbench_steward_phone_browser.js checks the Steward row
// tops the tree screen, the steward screen it pushes and its Thread / Jobs / Tending / Asks strip, Back up the stack, the
// farm card's entry and a send.
func TestWorkbenchStewardPhoneUnderChromium(t *testing.T) {
	underChromium(t, "workbench_steward_phone_browser.js", func() *httptest.Server { return workbenchRig(t) })
}

// workbenchRig is the card world's Commons with the workbench roots, live, and two test routes that write as kbeads would,
// putting the bead in the Commons daemon and its event on the stream:
//
//	POST /test/wb/node?id=&status=&state=  moves a bead (state: its agent_state)
//	POST /test/wb/say?thread=&text=        says text in #general's thread, as Flo
//	GET  /test/wb/sent?text=               how many message beads say text
//	GET  /test/wb/msg?text=                the fields of the message bead that says text
func workbenchRig(t *testing.T) *httptest.Server {
	srv, rig := cardWorldRig(t, time.Now, "", nil, nil, true)
	rig.mux.HandleFunc("POST /test/wb/node", func(w http.ResponseWriter, r *http.Request) {
		b, _ := rig.commons.GetBead(r.Context(), r.URL.Query().Get("id"))
		if b == nil {
			http.NotFound(w, r)
			return
		}
		b.Status = r.URL.Query().Get("status")
		if st := r.URL.Query().Get("state"); st != "" {
			b.Fields["agent_state"] = st
		}
		rig.commons.Put(b)
		rig.commonsEvents <- beadsapi.SSEEvent{Event: "beads.bead.updated", Data: commonstest.Payload(b)}
	})
	var said atomic.Int64
	rig.mux.HandleFunc("POST /test/wb/say", func(w http.ResponseWriter, r *http.Request) {
		head, _ := rig.commons.GetBead(beadsapi.WithUserBearer(r.Context(), commonsViewer), r.URL.Query().Get("thread"))
		if head == nil {
			http.NotFound(w, r)
			return
		}
		now := time.Now()
		b := &beadsapi.BeadDetail{ID: fmt.Sprintf("gd-msgWbSay%d", said.Add(1)), Type: "message", Status: "closed", Title: r.URL.Query().Get("text"), CreatedAt: now, UpdatedAt: now, Labels: head.Labels,
			Fields: map[string]string{"channel_id": head.Fields["channel_id"], "author": "oidc:flo@example.com", "text": r.URL.Query().Get("text"), "thread_id": head.ID}}
		rig.commons.Put(b)
		rig.beads.put(b) // the bead view reads it too (cardWorldRig)
		rig.commons.Log("beads.bead.created", b, now)
		rig.commonsEvents <- beadsapi.SSEEvent{Event: "beads.bead.created", Data: commonstest.Payload(b)}
	})
	rig.mux.HandleFunc("GET /test/wb/sent", func(w http.ResponseWriter, r *http.Request) {
		res, _ := rig.commons.ListBeadsFiltered(beadsapi.WithUserBearer(r.Context(), commonsViewer),
			beadsapi.ListBeadsQuery{Types: []string{"message"}, Fields: map[string]string{"text": r.URL.Query().Get("text")}})
		fmt.Fprint(w, len(res.Beads))
	})
	rig.mux.HandleFunc("GET /test/wb/msg", func(w http.ResponseWriter, r *http.Request) {
		res, _ := rig.commons.ListBeadsFiltered(beadsapi.WithUserBearer(r.Context(), commonsViewer),
			beadsapi.ListBeadsQuery{Types: []string{"message"}, Fields: map[string]string{"text": r.URL.Query().Get("text")}})
		if res == nil || len(res.Beads) != 1 {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(res.Beads[0].Fields)
	})
	return srv
}

// The workbench on a phone (step 7, gd-DhGFLKK0tj): testdata/workbench_phone_browser.js walks the push stack, the tree one
// level at a time, the node's tabs, Back one level up in the bar and in history, a deep link's level above, and the composer
// on the keyboard.
func TestWorkbenchPhoneUnderChromium(t *testing.T) {
	underChromium(t, "workbench_phone_browser.js", func() *httptest.Server { return workbenchRig(t) })
}

// The live head's Nudge in a real browser (gd-1iJEc1nYqm): testdata/workbench_nudge_browser.js opens an agent's node,
// checks page-nudge.js waits for the first Nudge, and sends a nudge from the box it opens in place.
func TestWorkbenchNudgeUnderChromium(t *testing.T) {
	underChromium(t, "workbench_nudge_browser.js", func() *httptest.Server {
		srv, _ := cardWorldRig(t, time.Now, "", nil, nil, true)
		return srv
	})
}

// The agent head's Wake / Restart in a real browser (gd-7DyHdnx8fC): testdata/workbench_wake_browser.js opens the down
// gardener gd-gDownB's node, whose head carries the farm's reason (commons Handler.Reason), and restarts it from there:
// asked once, a refusal said in place, then the restart, posted to its wake route. A viewer who may not restart gardeners
// (no X-UX-Waker) reads the reason and is offered nothing.
func TestWorkbenchWakeUnderChromium(t *testing.T) {
	underChromium(t, "workbench_wake_browser.js", func() *httptest.Server {
		srv, rig := cardWorldRig(t, time.Now, "", nil, nil, true)
		rig.commons.Put(&beadsapi.BeadDetail{ID: "gd-gDownB", Type: "agent", Status: "in_progress", Title: "g-down-b", Labels: []string{"project:gardenio"},
			Fields: map[string]string{"agent": "g-down-b", "role": "crew", "agent_state": "failed", "task_id": "gd-planB1a", "task_title": "Step one of plan B"}})
		return srv
	})
}

// The workbench's pure helpers (web/commons2.js) under node: the node a path names, find's rows, the Plan rail, the holder.
func TestWorkbenchLogic(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := `
var C = require(process.argv[1]), assert = require("assert");
assert.strictEqual(C.route("/v/garden/c/n/gd-wbPlan", "/v"), "gd-wbPlan"); assert.strictEqual(C.route("/garden/c/n/a%20b/", ""), "a b");
assert.strictEqual(C.route("/garden/c/gardenio/general", ""), ""); assert.strictEqual(C.route("/garden/c/n/x/y", ""), "");
assert.strictEqual(C.href("/v", "gd-a b"), "/v/garden/c/n/gd-a%20b");
var rows = [{ id: "p", title: "Plan", up: -1 }, { id: "ph", title: "Phase one", up: 0 }, { id: "s1", title: "Budgets", up: 1, type: "task" },
  { id: "c1", title: "crew", up: 2, type: "agent", state: "done" }, { id: "c2", title: "crew two", up: 2, type: "agent", state: "working" },
  { id: "s2", title: "Phone stack", up: 1 }, { id: "s9", title: "Perf gate", up: 0 }];
assert.deepStrictEqual(C.found(rows, "PHONE"), [0, 1, 5], "a match keeps its path, any case");
assert.deepStrictEqual(C.found(rows, " "), [0, 1, 2, 3, 4, 5, 6], "blank finds every row");
assert.deepStrictEqual(C.siblings(rows, 5), [2, 5]); assert.deepStrictEqual(C.siblings(rows, 0), [1, 6], "the root's are its children");
assert.strictEqual(C.holder(rows, 2), "c2", "the agent at work"); assert.strictEqual(C.holder(rows, 3), "c1"); assert.strictEqual(C.holder(rows, 6), "");
assert.ok(C.down(rows, 1) && C.down(rows, 2) === false && !C.down(rows, 0), "work under it opens one down; a step held by its agents is pushed");
assert.ok(C.down([{ id: "e" }, { id: "t", up: 0, more: "3" }], 1), "rows the fragment did not send");
assert.strictEqual(C.step([1, 2, 5], 2, 1), 5); assert.strictEqual(C.step([1, 2, 5], 5, 1), 5, "held at the end"); assert.strictEqual(C.step([1, 2], 9, -1), 1);
// the steward (steward view S2): its address, the ask's draft, @steward first, and the Holds line a save refuses
assert.strictEqual(C.stewardRoute("/v/garden/c/s/gardenio", "/v"), "gardenio"); assert.strictEqual(C.stewardRoute("/garden/c/s", ""), "");
assert.strictEqual(C.stewardRoute("/garden/c/s/a%20b/", ""), "a b"); assert.strictEqual(C.stewardRoute("/garden/c/n/gd-wbPlan", ""), null);
assert.strictEqual(C.stewardRoute("/garden/c/s/a/b", ""), null); assert.strictEqual(C.route("/garden/c/s/gardenio", ""), "");
assert.strictEqual(C.stewardHref("/v", ""), "/v/garden/c/s"); assert.strictEqual(C.stewardHref("", "a b"), "/garden/c/s/a%20b");
assert.strictEqual(C.askDraft("Chart lints again", "gd-wbEpic"), "@steward about Chart lints again (gd-wbEpic): ");
assert.strictEqual(C.askDraft("", "gd-wbEpic"), "@steward about gd-wbEpic: ");
assert.strictEqual(C.stewardText("  why is it stalled? "), "@steward why is it stalled?");
assert.strictEqual(C.stewardText("hey @Steward, why?"), "hey @Steward, why?", "it names the steward already");
assert.strictEqual(C.stewardText("mail me@steward.example"), "@steward mail me@steward.example", "an address is not a mention");
assert.strictEqual(C.badHold("- a\n## Answers\n- b"), "## Answers"); assert.strictEqual(C.badHold("- a\n### sub\n ## x"), "");
`
	js, _ := filepath.Abs(filepath.Join("web", "commons2.js"))
	if out, err := exec.Command(node, "-e", script, js).CombinedOutput(); err != nil {
		t.Fatalf("node workbench logic test failed: %v\n%s", err, out)
	}
}

// The agent head's Wake / Restart under node (gd-7DyHdnx8fC): the gardener its reason line names (commons2.js wakeOf, from
// the line's data-* and words) and the lines wake.js draws under it, a button for a sleeping, down or stale gardener and
// none for a parked or starting one; the question names its agent and its task.
func TestWorkbenchWakeLines(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := `
var C = require(process.argv[1]), W = require(process.argv[2]), assert = require("assert");
function line(kind, task) { return C.wakeOf({ reason: kind, id: "gd-g", agent: "gardenio-crew-g", task: task || "" }, "Failed: its pod was lost"); }
assert.deepStrictEqual(line("down", "Fix the fence"), { id: "gd-g", agent: "gardenio-crew-g", task_title: "Fix the fence", reason: { kind: "down", text: "Failed: its pod was lost" } });
assert.strictEqual(C.wakeOf({}, "x").reason, null, "no reason, no gardener to wake");
assert.deepStrictEqual(W.lines(line("down")), [["wk", [{ text: "Restart…", cls: "btn gwake", id: "gd-g", btn: 1 }]]]);
assert.deepStrictEqual(W.lines(line("sleeping")), [["wk", [{ text: "Wake…", cls: "btn gwake", id: "gd-g", btn: 1 }]]]);
assert.deepStrictEqual(W.lines(line("stale")), [["wk", [{ text: "Restart…", cls: "btn gwake", id: "gd-g", btn: 1 }]]]);
["parked", "starting"].forEach(function (k) { assert.deepStrictEqual(W.lines(line(k)), [], k + ": answered in the Mailbox, or on its way"); });
assert.deepStrictEqual(W.lines(line("down", "Fix the fence"), { step: "ask" })[0], ["wk", "Restart gardenio-crew-g? It will resume its task Fix the fence."]);
assert.deepStrictEqual(W.lines(line("sleeping"), { step: "ask" })[1][1].map(function (b) { return b.cls; }), ["btn gwake-yes", "btn gwake-no"]);
assert.deepStrictEqual(W.lines(line("down"), { step: "busy" }), [["wk", "Restarting gardenio-crew-g…"]]);
assert.deepStrictEqual(W.lines(line("down"), W.outcome(200, { ok: true }, line("down"))), [["wk", "Restarted. It shows as starting on the next update."]]);
`
	c2, _ := filepath.Abs(filepath.Join("web", "commons2.js"))
	wk, _ := filepath.Abs(filepath.Join("web", "wake.js"))
	if out, err := exec.Command(node, "-e", script, c2, wk).CombinedOutput(); err != nil {
		t.Fatalf("node workbench wake test failed: %v\n%s", err, out)
	}
}

// A workbench address gets the workbench's own page (node.html): the garden's stylesheet and one bundle with commons2.js,
// none of the farm's, under an ETag of its own.
func TestNodeAddressGetsTheWorkbenchPage(t *testing.T) {
	_, farm := servedPage(t)
	h := &Handler{Hub: New(newFake(), "/v", nil), BasePath: "/v"}
	for _, p := range []string{"/garden/c/s", "/garden/c/s/gardenio"} { // a steward's address too (gd-LisW87zrnf)
		rec := httptest.NewRecorder()
		h.ServePage(rec, httptest.NewRequest("GET", p, nil))
		if !strings.Contains(rec.Body.String(), `class="wb2-page"`) {
			t.Errorf("%s does not get the workbench page", p)
		}
	}
	rec := httptest.NewRecorder()
	h.ServePage(rec, httptest.NewRequest("GET", "/garden/c/n/gd-wbPlan", nil))
	body := rec.Body.String()
	for _, want := range []string{pageAssets.byName["page-node.js"].file, pageAssets.byName["page.css"].file, `class="wb2-page"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the workbench page lacks %s", want)
		}
	}
	if strings.Contains(body, `src="/v`+assetsPath+pageAssets.byName["page-nudge.js"].file) {
		t.Error("the workbench page loads page-nudge.js cold: it is loaded by the live head's first Nudge")
	}
	for _, farmOnly := range []string{"page-scene.", "page-farm.", `id="board"`} {
		if strings.Contains(body, farmOnly) {
			t.Errorf("the workbench page carries the farm's %s", farmOnly)
		}
	}
	if e := rec.Header().Get("ETag"); !strings.HasPrefix(e, `W/"n`) || e == farm.Header().Get("ETag") {
		t.Errorf("ETag %q, want the workbench page's own", e)
	}
}
