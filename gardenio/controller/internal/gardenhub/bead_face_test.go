package gardenhub

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/decisionview/decisiontest"
)

// faceBeads adds one bead per typed face (WB-1, bead_face.go) to the bead view's reader only: like stressBead they are on
// no plot, so the farm and the other goldens never see them, but /garden?bead=<id> opens each for its face's golden. Times
// count back from now, so the goldens' frozen clock reads the same ages on every run.
func faceBeads(beads *fakeBeads, now time.Time) {
	at := func(ago time.Duration) string { return now.Add(-ago).UTC().Format(time.RFC3339) }
	add := func(id, typ, status, title string, ago time.Duration, fields map[string]string, labels ...string) *beadsapi.BeadDetail {
		b := &beadsapi.BeadDetail{ID: id, Type: typ, Status: status, Title: title, Priority: 2, Labels: append([]string{"project:gardenio"}, labels...),
			Fields: fields, CreatedBy: "mo", CreatedAt: now.Add(-ago - time.Hour), UpdatedAt: now.Add(-ago)}
		beads.beads[id] = b
		return b
	}
	child := func(kid, parent string) {
		beads.deps = append(beads.deps, beadsapi.Dependency{BeadID: kid, DependsOnID: parent, Type: "parent-child"})
	}

	w := add("gd-FaceWork01", "task", "in_progress", "Typed faces for the bead panel", 20*time.Minute, map[string]string{
		"mr_url": "https://github.com/acme/garden/pull/57", "mr_iid": "57", "mr_state": "opened", "ci_status": "running", "due_at": at(-50 * time.Hour)})
	w.Assignee, w.Owner, w.Description = "gardenio-53-typed-faces", "mo", "One renderer per type, a golden for each."
	// A task bearing arms the daemon would refuse it (heraldry spec R6), as an older row may: its view shows none, so
	// the facework golden has no Heraldry section (heraldry step 3.4).
	w.Heraldry = "red 🔑 . . . ."

	// The epic bears arms (heraldry steps 3.4, 5.5b): the faceepic golden opens on its Heraldry section, the row picture 64 px
	// high and its name in words, and facearms on Change arms, the row of three. Plan F wears the same emoji in another order,
	// so the control's live register names a look-alike; neither shares a charge with the farm's plan P (golden_browser_test.go).
	add("gd-FaceEpic01", "epic", "in_progress", "5. Workbench: beads, plans cockpit, bundles", 3*time.Hour, map[string]string{}).Heraldry = "🔨🧰📜"
	for i, st := range []string{"closed", "closed", "in_progress", "blocked", "open"} {
		id := "gd-FaceEpicK" + string(rune('1'+i))
		add(id, "task", st, "Workbench step "+string(rune('1'+i)), time.Duration(i+1)*time.Hour, map[string]string{})
		child(id, "gd-FaceEpic01")
	}

	add("gd-FacePlan01", "epic", "open", "Lands: the game, Track A", 5*time.Hour, map[string]string{
		"plan_state": "ready", "phase_order": `["gd-FacePlanP1","gd-FacePlanP2","gd-FacePlanP3"]`}, "plan").Heraldry = "🧰🔨📜"

	ag := add("gd-FaceAgent01", "agent", "in_progress", "gardenio-53-typed-faces", 2*time.Minute, map[string]string{
		"agent": "gardenio-53-typed-faces", "agent_state": "working", "role": "crew", "project": "gardenio", "mode": "crew",
		"resolved_harness": "claude", "task_id": "gd-FaceWork01", "spawned_by": "gardenio-plan-gardener", "live_cost_usd": "1.37", "live_turns": "42"})
	ag.Description = "Assigned to task: gd-FaceWork01"
	// An agent on an armed line (gd-ASLjQPkHJz): its task is a step of the armed epic, so its bead view draws the epic's run
	// before its title and before the task on the Tending row. gd-FaceAgent01's task has no line, so it shows none.
	add("gd-FaceAgentL1", "agent", "in_progress", "gardenio-58-agent-beads", 4*time.Minute, map[string]string{
		"agent": "gardenio-58-agent-beads", "agent_state": "working", "role": "crew", "project": "gardenio", "mode": "crew",
		"resolved_harness": "claude", "task_id": "gd-FaceEpicK3", "spawned_by": "gardenio-plan-gardener", "live_cost_usd": "0.42", "live_turns": "9"})

	opts := `[{"id":"golden","short":"Golden each","label":"A golden for every face, phone and desktop","artifact_type":"pull_request"},` +
		`{"id":"sample","short":"Sample","label":"Goldens for the four busiest faces only"},{"id":"none","label":"No goldens yet"}]`
	add("gd-FaceDecide1", "decision", "open", "How many face goldens?", 5*time.Minute, map[string]string{
		"prompt": "Every face could get a golden at both widths in both themes, or only the busiest four. Which?", "options": opts,
		"context":                  "## Cost\n\nA golden per face is **44 shots**; see gd-FaceWork01.",
		"requesting_agent_bead_id": "gd-FaceAgent01", "requesting_task": "gd-FaceWork01"})
	// The acceptance decision (decisiontest, gd-DiU8SB6TAM) as the bead panel shows it, the screenshots on its --evidence
	// task (goldenEvidence, which the goldens' Mailbox decision names too) under the context, above the answers.
	gate := add("gd-FaceGateDec", "decision", "open", decisiontest.Title, 5*time.Minute, decisiontest.Fields(goldenEvidence))
	gate.CreatedBy = "lands-gate"
	ev := add(goldenEvidence, "task", "in_progress", decisiontest.EvidenceTitle, 20*time.Minute, map[string]string{})
	ev.Description = ""
	beads.comments[goldenEvidence] = decisiontest.Comments(now)
	add("gd-FaceDecide2", "decision", "closed", "Where do the faces render?", 3*time.Hour, map[string]string{
		"prompt": "Above the description, or folded under the fields?", "options": `[{"id":"above","short":"Above"},{"id":"fold","short":"Folded"}]`,
		"chosen": "Above", "response_text": "Above, like the bead page's type partials.", "responded_by": "mo@example.com", "responded_at": at(2 * time.Hour),
		"requesting_agent_bead_id": "gd-FaceAgent01"})

	add("gd-FaceEscal01", "escalation", "open", "Merge PR #57", 10*time.Minute, map[string]string{
		"operation_type": "gitlab_mr_merge", "sensitivity": "normal", "context": "CI is green on PR #57 and the faces were graded accept; it needs a merge.",
		"requesting_agent": "gd-FaceAgent01", "requesting_task": "gd-FaceWork01",
		"operation_spec": `{"mr_url":"https://github.com/acme/garden/pull/57","project":"garden","docs":"https://garden.example/docs/faces"}`})
	add("gd-FaceEscal02", "escalation", "closed", "Merge PR #55", 4*time.Hour, map[string]string{
		"operation_type": "gitlab_mr_merge", "context": "PR #55 is green.", "requesting_agent": "gd-FaceAgent01",
		"approved_by_email": "mo@example.com", "approved_at": at(3 * time.Hour), "approval_method": "web_ui", "result_status": "success",
		"result": `{"summary":"Merged PR #55 into main."}`, "executor": "escalation-gd-faceescal02",
		"operation_spec": `{"mr_url":"https://github.com/acme/garden/pull/55"}`})

	m := add("gd-FaceMsg0001", "message", "open", "Faces are up for review", 30*time.Minute, map[string]string{
		"text": "Faces are up for review on PR #57: every type has its own section now. gd-FaceWork01 has the details.", "thread_id": "gd-FaceWork01"},
		"from:gardenio-53-typed-faces", "squawk")
	m.Description, m.CreatedAt = "Faces are up for review", now.Add(-30*time.Minute)

	add("gd-FaceBundle1", "bundle", "open", "Workbench narrative", 6*time.Hour, map[string]string{})
	for i, d := range [][3]string{{"2", "Why typed faces", "markdown"}, {"1", "The face block", "markdown"}, {"3", "What is left for 5.4", "text"}} {
		id := "gd-FaceBndDoc" + string(rune('1'+i))
		add(id, "doc", "open", "doc "+d[1], time.Hour, map[string]string{"position": d[0], "headline": d[1], "format": d[2]})
		child(id, "gd-FaceBundle1")
	}

	add("gd-FaceReport1", "report", "closed", "UX grade: typed faces", time.Hour, map[string]string{
		"report_type": "ux_grade", "format": "markdown", "content": "Graded 11 faces at phone and desktop, light and dark.\n\nC1-C12 pass; see gd-FaceWork01."})

	add("gd-FaceLoop001", "trigger", "open", "plan gardener for Lands", 15*time.Minute, map[string]string{
		"enabled": "true", "cron": "*/30 * * * *", "timezone": "UTC",
		"gardener": `{"name":"gardenio-plan-gardener","terminal_goal":"Every step of the plan merged and graded.","runs_spawned":12,"runs_completed":11,` +
			`"max_runs":40,"max_runs_per_day":24,"runs_day":"` + now.UTC().Format("2006-01-02") + `","runs_today":3,"last_agent_id":"gd-FaceAgent01"}`},
		"role:gardener")

	add("gd-FaceMR00001", "merge-request", "open", "feat(gardenhub): typed faces", 25*time.Minute, map[string]string{
		"url": "https://github.com/acme/garden/pull/57", "state": "opened", "pipeline_status": "success", "source_branch": "agent/53-typed-faces",
		"target_branch": "main", "author": "gardenio-53-typed-faces", "approvals_given": "1", "approvals_required": "2", "unresolved_threads": "2",
		"last_commit_sha": "4f2a9c1e77d0b3aa", "last_commit_message": "feat(gardenhub): a golden for every face"})

	add("gd-FaceGeneric", "advice", "open", "Pods OOM on local full Go builds", 48*time.Hour, map[string]string{
		"scope": "project:gardenio", "severity": "high", "applies_to": "role:crew"})
}

// reportBeads adds a screenshot report as an agent files one (gb decision report / gb bundle add, format=markdown) to the bead
// view's reader only, for the commons-ux-acceptance skill's report surface (task 3.7, gd-GJ3xhSQsdh): headings, captioned
// screenshots through the viewer's /image/ proxy at laptop and phone sizes (absolute and root-relative), a list, a table, a
// quote, code, a picture from a host the proxy does not serve, and F2 an unbroken URL. The images are fixtureImage's, sized
// by their key.
func reportBeads(beads *fakeBeads, now time.Time) {
	img := "https://garden.example/image/coop/sessions/images/"
	content := "# P3 screenshot report: butterflies, fireflies, textures, season\n\n" +
		"The live look on **2026.298.1**, at laptop (1280 px) and phone (390 px) widths, by day and by night. Every shot is a " +
		"crop, so the small life is visible; the dev-bypass banner is cropped out of each.\n\n" +
		"## Butterflies by day (3.2)\n\n" +
		"![Butterflies over plan A's plot at Plan zoom, one mid-flight](" + img + "p3-butterfly-day-1280x800.png)\n\n" +
		"![The hover pause: a butterfly holds still while the cursor is on its plot](" + img + "p3-butterfly-pause-1280x800.png \"hover pause\")\n\n" +
		"🦋 Butterflies, 🧑‍🌾 the farmer, ⚠️ a warning; مريم and José (Jose\u0301, a combining accent) reviewed the café shots.\n\n" +
		"- Two to five per plot, never over a sign.\n- Reduced motion *off* for these shots; with it on they sit still.\n" +
		"  - The pause lasts while the cursor stays.\n\n" +
		"## Fireflies by night\n\n" +
		"![Fireflies over the meadow at night, phone](/image/coop/sessions/images/p3-firefly-night-390x844.png)\n\n" +
		"## What P3 changed\n\n" +
		"| Step | What | PR |\n|---|---|---|\n| 3.1 | Textures, gems, pewter and clay figures | #1001 |\n" +
		"| 3.4 | October accents: leaves, turned trees, the rosehip vine | #1012 |\n| 3.3 | Auto skin by the clock | #1009 |\n\n" +
		"> The 3.3 Auto skin follows the viewer's clock, so a night shot needs no setting.\n\n" +
		"1. Open the plot.\n2. Press `i` for the bin, then the report.\n\n" +
		"---\n\n" +
		"A panel from outside the garden stays a link: ![Grafana frame time](https://grafana.example.com/render/d-solo/abc.png). " +
		"Shots were taken with `make commons-ux-shoot`; the run is gd-FaceWork01, and the long link is " +
		"https://gitlab.example.com/gardenio/controller/-/merge_requests/4242/diffs?commit_id=0123456789abcdef0123456789abcdef01234567."
	beads.beads["gd-Report3p6"] = &beadsapi.BeadDetail{ID: "gd-Report3p6", Type: "report", Status: "closed", Priority: 2,
		Title: "P3 screenshot report: butterflies, fireflies, textures, season", Labels: []string{"project:gardenio"},
		CreatedBy: "gardenio-36-p3-screenshot", CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-time.Hour),
		Fields: map[string]string{"report_type": "screenshots", "format": "markdown", "content": content}}
}

// faceStressBeads adds the Commons UX rubric's stress fixtures (gardenio/docs/commons-ux-rubric.md §1) for the busiest faces,
// to the bead view's reader only, for the commons-ux-acceptance skill's face*stress surfaces (never goldens): F1 a 200-char
// title, a 1,200-char prompt and a 90-char agent name, F2 an unbroken URL, SHA and branch, F3 fourteen options, F4 emoji and
// non-Latin, F6 an asker and a task missing from the daemon.
func faceStressBeads(beads *fakeBeads, now time.Time) {
	url := "https://gitlab.example.com/gardenio/controller/-/merge_requests/4242/diffs?commit_id=0123456789abcdef0123456789abcdef01234567"
	agent := "gardenio-crew-" + strings.Repeat("very-long-agent-name-", 4)[:76]
	long := strings.Repeat("A long question sentence that keeps the reader busy for a while. ", 18)[:1150] + " 🧑‍🌾 مريم café?"
	var opts []string
	for i := 0; i < 14; i++ {
		opts = append(opts, `{"id":"o`+string(rune('a'+i))+`","short":"Option `+string(rune('A'+i))+` 🇳🇿","label":"Ship it on agent/some-very-long-branch-name/gd-AbCdEfGhIj after `+url+`"}`)
	}
	add := func(b *beadsapi.BeadDetail) {
		b.CreatedAt, b.UpdatedAt, b.Labels = now.Add(-time.Hour), now.Add(-5*time.Minute), []string{"project:gardenio"}
		beads.beads[b.ID] = b
	}
	add(&beadsapi.BeadDetail{ID: "gd-FaceStressD1", Type: "decision", Status: "open", Title: "Stress: " + strings.Repeat("a long decision title that keeps going ", 6)[:192],
		Fields: map[string]string{"prompt": long, "options": "[" + strings.Join(opts, ",") + "]", "requesting_agent_bead_id": "gd-MissingAg01", "requesting_task": "gd-MissingTk01"}})
	add(&beadsapi.BeadDetail{ID: "gd-FaceStressE1", Type: "escalation", Status: "open", Title: "Stress: merge " + url,
		Fields: map[string]string{"operation_type": "gitlab_mr_merge", "context": "Head SHA 0123456789abcdef0123456789abcdef01234567 on agent/some-very-long-branch-name/gd-AbCdEfGhIj is green. " + long,
			"requesting_agent": agent, "operation_spec": `{"mr_url":"` + url + `"}`}})
	add(&beadsapi.BeadDetail{ID: "gd-FaceStressA1", Type: "agent", Status: "in_progress", Title: agent,
		Fields: map[string]string{"agent": agent, "agent_state": "rate_limited", "role": "crew", "project": "gardenio", "task_id": "gd-StressF1F2",
			"total_cost_usd": "12.5", "wrapup": `{"tldr":"Merged ` + url + ` after 田中花子 reviewed; café 🧑‍🌾"}`}})
}

// faceFixture is the bead view's reader with only the face beads in it.
func faceFixture() *fakeBeads {
	f := &fakeBeads{beads: map[string]*beadsapi.BeadDetail{}, comments: map[string][]beadsapi.Comment{}}
	faceBeads(f, time.Unix(1_790_000_000, 0))
	releaseBeads(f, time.Unix(1_790_000_000, 0))
	return f
}

// Each MUST type gets its face, keyed by kind, and any other type none (the generic fields table stands): WB-1.
func TestBeadFaceKinds(t *testing.T) {
	f := faceFixture()
	for id, want := range map[string]string{
		"gd-FaceWork01": "work", "gd-FaceEpic01": "epic", "gd-FacePlan01": "plan", "gd-FaceDecide1": "decision", "gd-FaceEscal01": "escalation",
		"gd-FaceAgent01": "agent", "gd-FaceMsg0001": "message", "gd-FaceBundle1": "bundle", "gd-FaceReport1": "report", "gd-FaceLoop001": "gardener",
		"gd-FaceMR00001": "mr", "gd-FaceGeneric": "", "gd-FaceBndDoc1": "", "gd-FaceRelease1": "release",
	} {
		doc, err := readBead(context.Background(), f, id)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		got := ""
		if doc.Face != nil {
			got = doc.Face.Kind
		}
		if got != want {
			t.Errorf("%s: face %q, want %q", id, got, want)
		}
	}
	for _, c := range []struct {
		b    *beadsapi.BeadDetail
		want string
	}{
		{&beadsapi.BeadDetail{Type: "bug"}, "work"}, {&beadsapi.BeadDetail{Type: "chore"}, "work"}, {&beadsapi.BeadDetail{Type: "feature"}, "work"},
		{&beadsapi.BeadDetail{Type: "schedule", Fields: map[string]string{"gardener": `{"name":"g"}`}}, "gardener"},
		{&beadsapi.BeadDetail{Type: "schedule"}, ""}, {&beadsapi.BeadDetail{Type: "trigger"}, ""},
		{&beadsapi.BeadDetail{Type: "agent", Fields: map[string]string{"gardener": "x"}}, "agent"},
	} {
		if got := faceKindOf(c.b); got != c.want {
			t.Errorf("faceKindOf(%s %v) = %q, want %q", c.b.Type, c.b.Fields, got, c.want)
		}
	}
}

// What each face reads, with the beads it names titled in the same pass.
func TestBeadFaceFields(t *testing.T) {
	f := faceFixture()
	read := func(id string) *beadFace {
		t.Helper()
		doc, err := readBead(context.Background(), f, id)
		if err != nil || doc.Face == nil {
			t.Fatalf("%s: %v %+v", id, err, doc)
		}
		return doc.Face
	}
	if w := read("gd-FaceWork01").Work; w.Assignee != "gardenio-53-typed-faces" || w.MR == nil || w.MR.Number != "57" || w.MR.Pipeline != "running" || w.Due == 0 || w.MR.Author != "" {
		t.Errorf("work face: %+v %+v", w, w.MR)
	}
	if e := read("gd-FaceEpic01").Epic; e.Total != 5 || !reflect.DeepEqual(e.Counts, map[string]int{"closed": 2, "in_progress": 1, "blocked": 1, "open": 1}) {
		t.Errorf("epic face: %+v", e)
	}
	if p := read("gd-FacePlan01").Plan; p.State != "ready" || p.Phases != 3 {
		t.Errorf("plan face: %+v", p)
	}
	d := read("gd-FaceDecide1").Decision
	if d.State != "open" || !d.Open || len(d.Options) != 3 || d.Options[0].Artifact != "pull_request" || d.Options[2].Label != "No goldens yet" {
		t.Errorf("open decision: %+v", d)
	}
	if !strings.HasPrefix(d.Context, "## Cost") {
		t.Errorf("open decision's context is its body: %q", d.Context)
	}
	older := &beadsapi.BeadDetail{ID: "gd-FaceDecideD", Type: "decision", Status: "open", Description: "Older writers put it here.", Fields: map[string]string{"prompt": "Q?"}}
	f.beads[older.ID] = older
	// the face shows it as its body and the panel skips its Description section, but the doc keeps it: the Edit form
	// (beadwrite.js) loads d.description, and a blank one there saves over the whole context (gd-OSI1uK35Wa)
	if doc, err := readBead(context.Background(), f, older.ID); err != nil || doc.Description != older.Description || !doc.DescriptionIsContext || doc.Face.Decision.Context != "Older writers put it here." {
		t.Errorf("a description that is the decision's context is its face's body, flagged, and still the editor's text: %v %+v", err, doc)
	}
	long := &beadsapi.BeadDetail{ID: "gd-FaceDecideL", Type: "decision", Status: "open", Description: strings.Repeat("x", beadDescMax+1), Fields: map[string]string{"prompt": "Q?"}}
	f.beads[long.ID] = long
	if doc, err := readBead(context.Background(), f, long.ID); err != nil || !doc.DescriptionClipped || !doc.DescriptionIsContext {
		t.Errorf("a context too long to edit keeps the editor's too-long guard: %v %+v", err, doc)
	}
	if doc, _ := readBead(context.Background(), f, "gd-FaceDecide1"); doc == nil || doc.DescriptionIsContext {
		t.Errorf("a context from the context field leaves the description its own section: %+v", doc)
	}
	wb, _ := os.ReadFile(filepath.Join("web", "workbench.js"))
	if !strings.Contains(string(wb), "!x.description_is_context && x.description") {
		t.Error("workbench.js no longer skips the Description section a decision's context already fills")
	}
	if doc, _ := readBead(context.Background(), f, "gd-FaceDecide1"); doc != nil {
		for _, fl := range doc.Fields {
			if fl.K == "context" {
				t.Errorf("a decision's context is its body, not a row in the fields fold: %+v", fl)
			}
		}
	}
	if a := d.AskedBy; a == nil || a.Agent == nil || a.Agent.Title != "gardenio-53-typed-faces" || a.Task == nil || a.Task.Title != "Typed faces for the bead panel" {
		t.Errorf("asked by: %+v", d.AskedBy)
	}
	// chosen names the option by its short (a Slack write path), so it is matched back to its id
	if d := read("gd-FaceDecide2").Decision; d.State != "answered" || d.Open || d.Chosen != "above" || d.Answer == "" || d.By != "mo@example.com" || d.At == 0 {
		t.Errorf("answered decision: %+v", d)
	}
	e := read("gd-FaceEscal01").Escalation
	if e.State != "pending" || !e.Open || e.Op != "gitlab_mr_merge" || !reflect.DeepEqual(e.Targets, []string{"https://github.com/acme/garden/pull/57", "https://garden.example/docs/faces"}) {
		t.Errorf("pending escalation: %+v", e)
	}
	if e := read("gd-FaceEscal02").Escalation; e.State != "done" || e.Open || e.By != "mo@example.com" || e.Result != "Merged PR #55 into main." || e.Method != "web_ui" || e.Executor != faceExecutor {
		t.Errorf("done escalation: %+v", e)
	}
	if a := read("gd-FaceAgent01").Agent; !a.Live || a.CostUSD != 1.37 || a.Turns != 42 || a.Task == nil || a.Task.Title == "" || a.Role != "crew" {
		t.Errorf("agent face: %+v", a)
	}
	if m := read("gd-FaceMsg0001").Message; m.Via != "squawk" || m.From != "gardenio-53-typed-faces" || m.Text == "" || m.Thread == nil {
		t.Errorf("message face: %+v", m)
	}
	var titles []string
	for _, d := range read("gd-FaceBundle1").Bundle.Docs {
		titles = append(titles, d.Title)
	}
	if !reflect.DeepEqual(titles, []string{"The face block", "Why typed faces", "What is left for 5.4"}) {
		t.Errorf("bundle docs in position order: %v", titles)
	}
	if r := read("gd-FaceReport1").Report; r.Type != "ux_grade" || r.Format != "markdown" || !strings.Contains(r.Content, "C1-C12") {
		t.Errorf("report face: %+v", r)
	}
	if g := read("gd-FaceLoop001").Gardener; !g.Enabled || g.RunsSpawned != 12 || g.MaxPerDay != 24 || g.RunsToday != 3 || g.LastRun == nil || g.LastRun.Title == "" || g.Goal == "" {
		t.Errorf("gardener face: %+v", g)
	}
	if m := read("gd-FaceMR00001").MR; m.State != "opened" || m.Pipeline != "success" || m.Approvals != "1/2" || m.Commit != "4f2a9c1e feat(gardenhub): a golden for every face" || m.Author == "" {
		t.Errorf("mr face: %+v", m)
	}
}

// The decision and escalation states the faces say, from the fields each lifecycle writes.
func TestBeadFaceStates(t *testing.T) {
	dec := func(status string, f map[string]string) *faceDecision {
		f["options"] = `[{"id":"a","short":"Yes","label":"Yes please"},"Maybe"]`
		return decisionFace(&beadsapi.BeadDetail{Type: "decision", Status: status, Fields: f})
	}
	for _, c := range []struct {
		d            *faceDecision
		state, pick  string
		answer, open string
	}{
		{dec("open", map[string]string{}), "open", "", "", "open"},
		{dec("open", map[string]string{"chosen": "yes please"}), "answered", "a", "", ""},      // a label, any case
		{dec("closed", map[string]string{"chosen": "ship it"}), "answered", "", "ship it", ""}, // matches no option: a free answer
		{dec("closed", map[string]string{"chosen": "dismissed", "cancel_reason": "stale"}), "dismissed", "", "stale", ""},
		{dec("closed", map[string]string{"canceled": "true", "canceled_by": "mo"}), "dismissed", "", "", ""},
		{dec("closed", map[string]string{"chosen": "_expired"}), "expired", "", "", ""},
		{dec("closed", map[string]string{}), "answered", "", "", ""},
	} {
		if c.d.State != c.state || c.d.Chosen != c.pick || c.d.Answer != c.answer || c.d.Open != (c.open == "open") {
			t.Errorf("decision %+v: want %s/%s/%q open=%v", c.d, c.state, c.pick, c.answer, c.open == "open")
		}
	}
	if d := dec("open", map[string]string{}); len(d.Options) != 2 || d.Options[1].Label != "Maybe" || d.Options[1].ID != "" {
		t.Errorf("a bare string option keeps its label and has no id: %+v", d.Options)
	}
	esc := func(status string, f map[string]string) *faceEscalation {
		return escalationFace(&beadsapi.BeadDetail{Type: "escalation", Status: status, Fields: f})
	}
	for _, c := range []struct {
		e     *faceEscalation
		state string
		open  bool
	}{
		{esc("open", map[string]string{}), "pending", true},
		{esc("in_progress", map[string]string{"approved_by": "mo"}), "approved", false},
		{esc("closed", map[string]string{"approved_by": "mo", "result_status": "failure", "result": "boom"}), "failed", false},
		{esc("closed", map[string]string{"rejected_by": "mo", "rejection_reason": "not now"}), "rejected", false},
		{esc("closed", map[string]string{"result_status": "failure", "result": `{"error":"rejected"}`}), "rejected", false},
		{esc("closed", map[string]string{}), "pending", false},
	} {
		if c.e.State != c.state || c.e.Open != c.open {
			t.Errorf("escalation %+v: want %s open=%v", c.e, c.state, c.open)
		}
	}
	if s := EscalationState(&beadsapi.BeadDetail{Type: "escalation", Status: "in_progress", Fields: map[string]string{"approved_by": "mo"}}); s != "approved" {
		t.Errorf("EscalationState of an approved, in_progress escalation = %q, want approved", s)
	}
	if s := EscalationState(nil); s != "" {
		t.Errorf("EscalationState(nil) = %q", s)
	}
	if e := esc("closed", map[string]string{"approved_by": "mo", "result_status": "failure", "result": "boom"}); e.Result != "boom" {
		t.Errorf("a failed run's result as written: %q", e.Result)
	}
}

// The face block's JSON is keyed by kind: {"kind":"decision","decision":{...}}, and a generic bead sends none.
func TestBeadFaceJSON(t *testing.T) {
	f := faceFixture()
	doc, _ := readBead(context.Background(), f, "gd-FaceDecide1")
	raw, _ := json.Marshal(doc)
	var m struct {
		Face map[string]json.RawMessage `json:"face"`
	}
	if err := json.Unmarshal(raw, &m); err != nil || string(m.Face["kind"]) != `"decision"` || m.Face["decision"] == nil || len(m.Face) != 2 {
		t.Fatalf("face block: %s", raw)
	}
	doc, _ = readBead(context.Background(), f, "gd-FaceGeneric")
	if raw, _ = json.Marshal(doc); strings.Contains(string(raw), `"face"`) {
		t.Fatalf("a generic bead has no face: %s", raw)
	}
}

// workbench.js's faces run under node over the documents bead.go serves (WB-1): each kind's view says its state in words,
// a decision's chosen option is marked in words too, an open ask carries the answer box web/answer.js drives (an
// escalation's only Approve and Reject), a link to a kind the Workbench does not show yet goes to the viewer's page, and a
// bead with no face (or a kind with no renderer) gets no view, so the generic fields table stands.
func TestBeadFacesUnderNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	f := faceFixture()
	docs := map[string]*beadDoc{}
	for id := range f.beads {
		if strings.HasPrefix(id, "gd-Face") {
			d, err := readBead(context.Background(), f, id)
			if err != nil {
				t.Fatal(err)
			}
			docs[id] = d
		}
	}
	raw, _ := json.Marshal(docs)
	script := `
var V = require(process.argv[1]), assert = require("assert"), D = JSON.parse(process.argv[2]);
var now = 1790000000, ctx = function (id, has) { return { id: id, now: now, base: "/v", has: function (k) { return (has || []).indexOf(k) >= 0; } }; };
function view(id, has) { return V.faceView(D[id], ctx(id, has)); }
function text(parts) { return (parts || []).map(function (p) { return typeof p === "string" ? p : p.t || p.sm || p.b || ""; }).join(""); }
function row(v, k) { var r = (v.rows || []).filter(function (r) { return r[0] === k; })[0]; return r ? text(r[1]) : null; }
var kinds = ["work", "epic", "plan", "decision", "escalation", "agent", "message", "bundle", "report", "gardener", "mr"];
assert.deepStrictEqual(Object.keys(V.FACES).sort(), kinds.slice().sort(), "one renderer per MUST kind");

var w = view("gd-FaceWork01");
assert.strictEqual(w.h, "Work");
assert.strictEqual(row(w, "Who"), "gardenio-53-typed-faces");
assert.strictEqual(row(w, "Pull request"), "#57 · opened · checks running");
assert.strictEqual(row(w, "Due"), "in 2d");

var e = view("gd-FaceEpic01");
assert.strictEqual(e.state, "2 of 5 children closed");
assert.strictEqual(row(e, "Still open"), "1 growing · 1 blocked · 1 planted");

var p = view("gd-FacePlan01");
assert.strictEqual(p.state, "Ready: waiting on the owner to approve the build.", "who it waits on, named");
assert.strictEqual(p.tone, "ask");
assert.strictEqual(row(p, "Phases"), "3");
assert.strictEqual(p.links, undefined, "no plan kind yet: no link (never the viewer's cockpit, gd-qie6tPtdpi)");
assert.strictEqual(row(V.faceView(D["gd-FacePlan01"], Object.assign(ctx("gd-FacePlan01"), { mapped: true })), "Phases"), null, "a plan on the map lists its phases above the face");
assert.deepStrictEqual(view("gd-FacePlan01", ["plan"]).links, [{ t: "Open the cockpit", go: "/v/garden?plan=gd-FacePlan01", aria: undefined }], "once the plan kind is shown: its panel");

var d = view("gd-FaceDecide1");
assert.strictEqual(d.state, "Waiting on the operator to answer.", "who it waits on, named");
assert.strictEqual(V.faceView(D["gd-FaceDecide1"], Object.assign(ctx("gd-FaceDecide1"), { mine: true })).state, "Waiting on you to answer.", "in the viewer's Mailbox: you");
assert.strictEqual(d.tone, "ask");
assert.strictEqual(row(d, "Asked by"), "gardenio-53-typed-faces for Typed faces for the bead panel");
assert.deepStrictEqual(d.items.map(function (i) { return text(i.parts); }), ["Golden eachA golden for every face, phone and desktop→ pull request", "SampleGoldens for the four busiest faces only", "No goldens yet"]);
assert.deepStrictEqual(d.acts, { kind: "decision", id: "gd-FaceDecide1", options: [
  { id: "golden", text: "Golden each", label: "A golden for every face, phone and desktop" }, { id: "sample", text: "Sample", label: "Goldens for the four busiest faces only" }, { id: "none", text: "No goldens yet", label: "No goldens yet" }] });

assert.deepStrictEqual(d.body.map(function (b) { return b.t; }), ["h", "p"], "the context is the decision's body, as Markdown");
assert.strictEqual(d.noBody, undefined);
assert.strictEqual(V.faceView({ face: { kind: "decision", decision: { state: "open", open: true } } }, ctx("x")).noBody, "No context was given with this question.", "an open ask with no context says so");
assert.deepStrictEqual(V.faceView({ face: { kind: "decision", decision: { state: "open", open: true, evidence: [{ url: "https://v/image/a.png", from: "gd-t" }] } } }, ctx("x")).evidence, [{ url: "https://v/image/a.png", from: "gd-t" }], "the evidence reaches the strip under the context (gd-Pi7C83262N)");
assert.strictEqual(d.evidence, undefined, "a decision with no evidence draws no strip");

var d2 = view("gd-FaceDecide2");
assert.strictEqual(d2.noBody, undefined, "a closed decision with no context says nothing of it");
assert.strictEqual(d2.state, "Answered: Above (by mo@example.com · 2h ago).");
assert.strictEqual(d2.acts, undefined, "an answered decision has no answer box");
assert.deepStrictEqual(d2.items.map(function (i) { return [i.cls, text(i.parts)]; }), [["chosen", "Abovechosen"], ["", "Folded"]], "the chosen option said in words, not colour alone");
assert.deepStrictEqual(d2.texts, [["Answer", "Above, like the bead page's type partials."]]);

var x = view("gd-FaceEscal01");
assert.strictEqual(x.state, "Waiting on the merge desk to approve.", "a gitlab_mr_merge is the desk's by default");
assert.strictEqual(row(x, "Operation"), "gitlab mr merge");
assert.strictEqual(row(x, "Acts on"), "github.com/…/pull/57 · garden.example/docs/faces", "each link under its short label");
assert.deepStrictEqual(x.acts, { kind: "escalation", id: "gd-FaceEscal01", title: "" });
var x2 = view("gd-FaceEscal02");
assert.strictEqual(x2.state, "Done: approved by mo@example.com 3h ago.");
assert.strictEqual(row(x2, "Approved via"), "web ui");
assert.strictEqual(x2.tone, "ok");
assert.deepStrictEqual(x2.texts, [["Result", "Merged PR #55 into main."]]);
assert.strictEqual(row(x2, "Run by"), "the escalation executor", "the synthetic escalation-<id> agent is never shown by name");
assert.strictEqual(x2.acts, undefined);

var a = view("gd-FaceAgent01");
assert.strictEqual(a.h, "Gardener");
assert.strictEqual(a.state, "working");
assert.strictEqual(row(a, "Tending"), "Typed faces for the bead panel");
assert.strictEqual(row(a, "Spent so far"), "$1.37 · 42 turns");
assert.strictEqual(a.links, undefined, "no Glass yet: no Watch (never the viewer's terminal page)");
assert.deepStrictEqual(view("gd-FaceAgent01", ["glass"]).links, [{ t: "Watch live", go: "/v/garden?glass=gd-FaceAgent01", aria: "Watch gardenio-53-typed-faces live" }], "the Glass once it is shown");
var ac = V.faceView(D["gd-FaceAgent01"], Object.assign(ctx("gd-FaceAgent01", ["glass", "costs"]), { costs: true }));
assert.deepStrictEqual(ac.links[1], { t: "See its charts", go: "/v/garden?costs=agent%3Agardenio-53-typed-faces", aria: "See the charts of gardenio-53-typed-faces" }, "its charts in the Costs panel for a viewer who may read them (step 4.2)");
assert.strictEqual(view("gd-FaceAgent01", ["glass", "costs"]).links.length, 1, "none for a viewer who may not");

assert.strictEqual(a.lead, "", "no wrap-up yet: nothing leads");
assert.deepStrictEqual(V.faceView({ face: { kind: "decision", decision: { state: "open", asked_by: { agent: { id: "gd-MissingAg01" } } } } }, ctx("d")).rows[0][1],
  [{ bead: "gd-MissingAg01", t: "a bead not visible to you" }], "a bead the daemon did not return says so, never a bare id");
var m = view("gd-FaceMsg0001");
assert.strictEqual(row(m, "From"), "gardenio-53-typed-faces");
assert.strictEqual(row(m, "Sent"), "a squawk · 30m ago");
assert.strictEqual(row(m, "In thread"), null, "Part of wish names the thread over the face (gd-UXDp9GUeMB)");
assert.strictEqual(D["gd-FaceMsg0001"].kin.head.title, "Typed faces for the bead panel");
assert.ok(/^Faces are up for review on PR #57/.test(m.lead), "the message itself leads, above who sent it: " + m.lead);
assert.strictEqual(m.texts, undefined, "and is not repeated under a Text label");

// the bead's pull request button (renderActs): a verb that says where it goes, and none where the face links it already
assert.strictEqual(V.prButton(D["gd-FaceWork01"]), null, "a work face's Pull request row links it already");
assert.deepStrictEqual(V.prButton({ fields: [{ k: "mr_url", v: "https://github.com/acme/garden/pull/57" }] }),
  { url: "https://github.com/acme/garden/pull/57", t: "Open PR #57", aria: "Open pull request #57 in a new tab" });
assert.strictEqual(V.prButton({ fields: [{ k: "mr_url", v: "https://git.example/x/-/merge_requests/9" }] }).t, "Open PR #9");
assert.strictEqual(V.prButton({ fields: [{ k: "pr_url", v: "https://example.com/review" }] }).t, "Open the pull request");
assert.strictEqual(V.prButton({ fields: [{ k: "mr_url", v: "javascript:alert(1)" }] }), null);
assert.strictEqual(V.prButton({ harvest: {}, face: { kind: "work", work: { mr: { url: "https://github.com/a/b/pull/3" } } }, fields: [{ k: "mr_url", v: "https://github.com/a/b/pull/3" }] }).t,
  "Open PR #3", "a harvested step hides its work face, so the button stays");

var b = view("gd-FaceBundle1");
assert.strictEqual(b.h, "Documents (3)");
assert.deepStrictEqual(b.items.map(function (i) { return text(i.parts); }), ["1The face blockmarkdown", "2Why typed facesmarkdown", "3What is left for 5.4text"]);
assert.strictEqual(b.links, undefined);
assert.strictEqual(view("gd-FaceBundle1", ["bundle"]).links[0].go, "/v/garden?bundle=gd-FaceBundle1");
assert.strictEqual(view("gd-FaceLoop001").links, undefined);
assert.strictEqual(view("gd-FaceLoop001", ["gardener"]).links[0].go, "/v/garden?gardener=gd-FaceLoop001");

var r = view("gd-FaceReport1");
assert.strictEqual(row(r, "Kind"), "ux grade");
assert.strictEqual(row(r, "Format"), null, "markdown, the usual format, is not said");
assert.deepStrictEqual(r.texts, [], "a markdown report is its document, not a folded text");
assert.deepStrictEqual(r.doc.map(function (b) { return b.t; }), ["p", "p"]);
var plain = V.faceView({ face: { kind: "report", report: { format: "text", content: "# not a heading" } } }, ctx("x"));
assert.deepStrictEqual([row(plain, "Format"), plain.texts, plain.doc], ["text", [["Content", "# not a heading"]], undefined], "another format stays text");

var g = view("gd-FaceLoop001");
assert.strictEqual(g.h, "Gardener loop");
assert.strictEqual(g.state, "Running.");
assert.strictEqual(row(g, "Today"), "3 of 24 runs");
assert.strictEqual(row(g, "Runs"), "12 spawned · 11 finished · cap 40");
assert.strictEqual(row(g, "Last run"), "gardenio-53-typed-faces");
assert.strictEqual(V.kindWord(D["gd-FaceLoop001"], "trigger"), "gardener loop");
// a parked loop says which park and links the live blocker (gardenerPark), as an ask
var park = function (kind) { return V.faceView({ face: { kind: "gardener", gardener: { enabled: true, dormant: { id: "gd-Esc", title: "Merge PR #9" }, dormant_kind: kind } } }, ctx("gd-L")); };
assert.deepStrictEqual([park("operator_wedge").state, row(park("operator_wedge"), "Waits on"), park("operator_wedge").tone], ["Parked: every open step waits on the operator.", "Merge PR #9", "ask"]);
assert.strictEqual(park("mr_merged").state, "Parked until a pull request merges.");
assert.strictEqual(row(g, "Waits on"), null, "a running loop waits on nothing");
// a plan names its gardener loop, and its park as the gardener face says it (planGardener, gardenerPark)
var planG = function (plan) { return V.faceView({ face: { kind: "plan", plan: Object.assign({ state: "building", phases: 2, gardener: { id: "gd-L", title: "plan gardener" } }, plan) } }, ctx("gd-P")); };
assert.deepStrictEqual([planG({}).state, row(planG({}), "Gardener"), row(planG({}), "Waits on"), planG({}).tone], ["Building.", "plan gardener", null, ""], "a running gardener: named, no park");
assert.strictEqual(row(planG({ gardener_state: "paused" }), "Gardener"), "plan gardener · paused");
var parkedPlan = planG({ dormant: { id: "gd-Esc", title: "Merge PR #9" }, dormant_kind: "operator_wedge" });
assert.deepStrictEqual([parkedPlan.state, row(parkedPlan, "Waits on"), parkedPlan.tone], ["Building. Its gardener is parked: every open step waits on the operator.", "Merge PR #9", "ask"]);
assert.strictEqual(planG({ dormant: { id: "gd-MR" }, dormant_kind: "mr_merged" }).state, "Building. Its gardener is parked until a pull request merges.");
assert.strictEqual(row(p, "Gardener"), null, "a plan with no gardener bound names none");

var mr = view("gd-FaceMR00001");
assert.strictEqual(mr.state, "Opened · checks passed; waiting on 1 more approval and 2 open threads.", "what it waits on, in words");
assert.strictEqual(mr.tone, "ask");
assert.strictEqual(row(mr, "Branches"), "agent/53-typed-faces → main");
assert.strictEqual(row(mr, "Approvals"), "1/2");
assert.strictEqual(V.kindWord(D["gd-FaceMR00001"], "merge-request"), "merge request");

assert.strictEqual(view("gd-FaceGeneric"), null, "no face: the generic fields table");
assert.strictEqual(V.faceView({ face: { kind: "molecule", molecule: {} } }, ctx("x")), null, "a kind with no renderer");
assert.strictEqual(V.faceView({}, ctx("x")), null);
assert.strictEqual(V.kindWord(D["gd-FaceAgent01"], "agent"), "gardener", "an agent is a gardener, as before");
// a link out from bead data is http(s) or plain text, never a javascript: href in the /garden origin (gd-fubODCnFyd)
function urls(v) { var u = []; (v.rows || []).forEach(function (r) { r[1].forEach(function (p) { if (p && p.url) u.push(p.url); }); }); return u; }
var evil = "javascript:alert(1)";
var badMR = V.faceView({ face: { kind: "mr", mr: { url: evil, title: "Fix it" } } }, ctx("x"));
assert.deepStrictEqual([urls(badMR), row(badMR, "Link")], [[], "Fix it"], "an MR face's javascript: url is its label, unlinked");
var badWork = V.faceView({ face: { kind: "work", work: { mr: { url: " JavaScript:alert(1)", number: "57", state: "opened" } } } }, ctx("x"));
assert.deepStrictEqual([urls(badWork), row(badWork, "Pull request")], [[], "#57 · opened"], "a work face's MR link");
var badEsc = V.faceView({ face: { kind: "escalation", escalation: { state: "open", targets: ["data:text/html,x", "https://example.com/a"] } } }, ctx("x"));
assert.deepStrictEqual(urls(badEsc), ["https://example.com/a"], "an escalation's targets: only the http(s) one links");
assert.deepStrictEqual([V.safeURL(evil), V.safeURL("HTTPS://ok.example/x"), V.safeURL(undefined)], ["", "HTTPS://ok.example/x", ""]);
assert.strictEqual(V.mrURL({ fields: [{ k: "mr_url", v: evil }] }), "");
// fold: about 600 characters before a fold, cut at a word and marked, never inside a code point
var short = V.fold("a short text");
assert.deepStrictEqual(short, { head: "a short text", rest: "" });
var long = V.fold("word ".repeat(200) + "🧑‍🌾 end");
assert.ok(Array.from(long.head).length <= 481 && /word…$/.test(long.head), "cut at a word, marked: " + long.head.slice(-12));
assert.strictEqual(long.total, Array.from("word ".repeat(200) + "🧑‍🌾 end").length);
assert.ok(long.rest.length > 0 && /end$/.test(long.rest));
var emoji = V.fold("🧑‍🌾".repeat(400));
assert.ok(!/\uD83E$/.test(emoji.head.slice(0, -1)), "no half surrogate before the ellipsis");
console.log("ok");
`
	js, _ := filepath.Abs(filepath.Join("web", "workbench.js"))
	out, err := exec.Command(node, "-e", script, js, string(raw)).CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "ok" {
		t.Fatalf("node: %v\n%s", err, out)
	}
}

// A report's markdown (task 3.7, gd-GJ3xhSQsdh) read into the blocks the bead view builds node by node: the screenshot report
// fixture (reportBeads) as an agent files it, and the edges an agent's markdown reaches (raw HTML stays text, a script link is
// no link, snake_case and arithmetic stay text, a hand-nested list nests, a picture only the proxy serves loads).
func TestReportMarkdownUnderNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	f := &fakeBeads{beads: map[string]*beadsapi.BeadDetail{}, comments: map[string][]beadsapi.Comment{}}
	reportBeads(f, time.Unix(1_790_000_000, 0))
	d, err := readBead(context.Background(), f, "gd-Report3p6")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(d)
	script := `
var V = require(process.argv[1]), assert = require("assert"), D = JSON.parse(process.argv[2]);
var ctx = { id: D.id, title: D.title, now: 1790000000, base: "/v", has: function () { return false; } };
var doc = V.faceView(D, Object.assign({}, ctx, { title: "Another title" })).doc, kinds = doc.map(function (b) { return b.t + (b.n || ""); });
assert.deepStrictEqual(kinds, ["h1", "p", "h2", "p", "p", "p", "ul", "h2", "p", "h2", "table", "quote", "ol", "hr", "p"]);
assert.strictEqual(V.mdPlain(doc[0].c), "P3 screenshot report: butterflies, fireflies, textures, season");
assert.deepStrictEqual(V.faceView(D, ctx).doc.slice(0, 1), doc.slice(1, 2), "a first heading that is the bead's title is not said twice");
assert.deepStrictEqual(doc[1].c[1], { k: "b", c: [{ k: "text", s: "2026.298.1" }] }, "strong");
assert.deepStrictEqual(doc[4].c, [{ k: "img", src: "https://garden.example/image/coop/sessions/images/p3-butterfly-pause-1280x800.png", alt: "The hover pause: a butterfly holds still while the cursor is on its plot", title: "hover pause" }], "a picture alone in its paragraph, with its title");
assert.strictEqual(V.mdPicSrc("/v", doc[3].c[0].src), "/v/image/coop/sessions/images/p3-butterfly-day-1280x800.png", "through the proxy under the page's base");
assert.strictEqual(V.mdPicSrc("/v", doc[8].c[0].src), "/v/image/coop/sessions/images/p3-firefly-night-390x844.png", "a root-relative /image/ path too");
assert.strictEqual(doc[6].items.length, 2);
assert.deepStrictEqual(doc[6].items[1].map(function (b) { return b.t; }), ["p", "ul"], "a hand-nested list (two spaces) nests");
assert.deepStrictEqual(doc[10].head.map(V.mdPlain), ["Step", "What", "PR"]);
assert.strictEqual(doc[10].rows.length, 3);
assert.strictEqual(doc[12].items.length, 2);
var last = doc[14].c, out = last.filter(function (n) { return n.k === "img"; })[0];
assert.strictEqual(V.mdPicSrc("/v", out.src), "", "a picture the proxy does not serve is not loaded");
assert.ok(last.some(function (n) { return n.k === "code" && n.s === "make commons-ux-shoot"; }));

function md(s) { return JSON.stringify(V.markdown(s)); }
function md2(s) { return JSON.stringify(V.markdown(s, true)); }
assert.strictEqual(md("<img src=x onerror=alert(1)>"), JSON.stringify([{ t: "p", c: [{ k: "text", s: "<img src=x onerror=alert(1)>" }] }]), "raw HTML stays text");
assert.strictEqual(md("credits=<n> per run"), JSON.stringify([{ t: "p", c: [{ k: "text", s: "credits=<n> per run" }] }]), "a placeholder is kept (gd-oNoa0zf55W)");
assert.strictEqual(V.safeHref("javascript:alert(1)"), "", "a script link is no link");
assert.strictEqual(V.safeHref("//evil.example/x"), "", "nor a protocol-relative one");
assert.strictEqual(V.safeHref("/garden?bead=gd-AbCdEfGhIj"), "/garden?bead=gd-AbCdEfGhIj");
assert.strictEqual(V.mdPicSrc("", "data:image/png;base64,AAAA"), "");
assert.strictEqual(md("snake_case_name and 2 * 3 * 4"), JSON.stringify([{ t: "p", c: [{ k: "text", s: "snake_case_name and 2 * 3 * 4" }] }]));
assert.strictEqual(md("a \\*not em\\*"), JSON.stringify([{ t: "p", c: [{ k: "text", s: "a *not em*" }] }]), "an escape");
assert.strictEqual(md("Title\n==="), JSON.stringify([{ t: "h", n: 1, c: [{ k: "text", s: "Title" }] }]), "setext");
assert.strictEqual(md("#hashtag"), JSON.stringify([{ t: "p", c: [{ k: "text", s: "#hashtag" }] }]), "no space, no heading");
assert.strictEqual(md("~~~\n<b>x</b>\n~~~"), JSON.stringify([{ t: "code", s: "<b>x</b>" }]));
assert.strictEqual(md("[a [nested] label](https://x.example/a_(b))"), JSON.stringify([{ t: "p", c: [{ k: "a", href: "https://x.example/a_(b)", c: [{ k: "text", s: "a [nested] label" }] }] }]));
assert.strictEqual(md("line one\nline two"), JSON.stringify([{ t: "p", c: [{ k: "text", s: "line one line two" }] }]), "a soft break is a space");
assert.strictEqual(md2("Applicant: ada\nCredits asked for: 40"), JSON.stringify([{ t: "p", c: [{ k: "text", s: "Applicant: ada" }, { k: "br" }, { k: "text", s: "Credits asked for: 40" }] }]), "a decision context breaks every line, as gardenmd does (gd-gKXlbPRw8i)");
assert.strictEqual(md("a\nb"), JSON.stringify([{ t: "p", c: [{ k: "text", s: "a b" }] }]), "and leaves the next report's soft breaks alone");
assert.deepStrictEqual(V.markdown("- a\n\n  more\n- b")[0].items.map(function (it) { return it.map(function (b) { return V.mdPlain(b.c); }); }), [["a", "more"], ["b"]], "an item's second paragraph");
assert.deepStrictEqual(V.markdown(""), []);
console.log("ok");
`
	js, _ := filepath.Abs(filepath.Join("web", "workbench.js"))
	out, err := exec.Command(node, "-e", script, js, string(raw)).CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "ok" {
		t.Fatalf("node: %v\n%s", err, out)
	}
}
