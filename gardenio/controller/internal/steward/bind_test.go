package steward

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
	"gardenio/controller/internal/lands/watcher"
	"gardenio/controller/internal/scheduler"
)

const (
	land     = "tomato-patch"
	general  = "tp-general"
	crew     = "tp-crew"
	machines = "tp-machines"
)

var chans = Channels{General: general, Machines: machines}

// wake is one event as its producer writes it, delivered on its SSE topic.
type wake struct {
	name  string
	topic string
	bead  bridge.BeadEvent
	// want is the one wake entry it fires, or "" for none.
	want string
}

func message(id, channel, author, text string, extra map[string]string) bridge.BeadEvent {
	f := map[string]string{"channel_id": channel, "author": author, "text": text, "kind": "text"}
	for k, v := range extra {
		f[k] = v
	}
	return bridge.BeadEvent{ID: id, Type: "message", Status: "closed", CreatedBy: author, Fields: f}
}

// wakes is every event a land sees that matters to the steward, and the near
// misses that must not wake it.
func wakes() []wake {
	return []wake{
		{"human message in #general", "beads.bead.created",
			message("m1", general, "oidc:ada@example.com", "can we add a watering schedule?", nil), WakeGeneral},
		{"@steward in #general wakes once, as a #general message", "beads.bead.created",
			message("m2", general, "oidc:ada@example.com", "@steward hello", nil), WakeGeneral},
		{"@steward mention in #crew", "beads.bead.created",
			message("m3", crew, "oidc:bob@example.com", "@steward what is in flight?", nil), WakeMention},
		{"steward run's decision resolved", "beads.bead.closed",
			bridge.BeadEvent{ID: "d1", Type: "decision", Status: "closed", CreatedBy: land + "-steward-ab12",
				Fields: map[string]string{"chosen": "approve"}}, WakeDecision},
		{"credit grant", "beads.bead.created",
			bridge.BeadEvent{ID: "g1", Type: "credit_grant", Status: "closed", CreatedBy: "oidc:op@example.com",
				Fields: map[string]string{"land": land, "amount_credits": "50"}}, WakeCreditGrant},
		{"purse threshold notice", "beads.bead.created",
			message("m4", general, watcher.Author, "The purse is running low", map[string]string{"kind": "notice", "origin": watcher.OriginNotice}), WakePurseThreshold},
		{"lands-gardener finished", "beads.bead.closed",
			bridge.BeadEvent{ID: "a1", Type: "agent", Status: "closed", Labels: []string{"role:" + GardenerRole}}, WakeGardenerFinished},

		{"the refill post (the grant wakes it)", "beads.bead.created",
			message("m5", general, watcher.Author, "The purse was refilled", map[string]string{"kind": "notice", "origin": watcher.OriginRefill}), ""},
		{"the steward's own post in #general", "beads.bead.created",
			message("m6", general, "agent:"+land+"-steward-ab12", "Hello, I am the steward", nil), ""},
		{"a human message in #crew without a mention", "beads.bead.created",
			message("m7", crew, "oidc:bob@example.com", "pushed a fix", nil), ""},
		{"a #general message updated (its surface refs written back)", "beads.bead.updated",
			message("m8", general, "oidc:ada@example.com", "can we add a watering schedule?", nil), ""},
		{"a lands-gardener's own decision resolved", "beads.bead.closed",
			bridge.BeadEvent{ID: "d2", Type: "decision", Status: "closed", CreatedBy: land + "-fix-the-tests-x1y"}, ""},
		{"a steward run closed (not a gardener)", "beads.bead.closed",
			bridge.BeadEvent{ID: "a2", Type: "agent", Status: "closed", Labels: []string{"role:" + Role}}, ""},

		{"a crew's post in #machines", "beads.bead.created",
			message("m9", machines, "agent:"+land+"-fix-the-tests-x1y", "PR #12 opened", nil), WakeMachines},
		{"the steward's own post in #machines", "beads.bead.created",
			message("m10", machines, "agent:"+land+"-steward-ab12", "spawned a gardener on tp-3", nil), ""},
		{"a person in #machines without a mention", "beads.bead.created",
			message("m11", machines, "oidc:ada@example.com", "interesting", nil), ""},
		{"a person's @steward in #machines wakes once, as a mention", "beads.bead.created",
			message("m12", machines, "oidc:ada@example.com", "@steward why did that fail?", nil), WakeMention},
		{"a purse notice in #machines wakes once, as a threshold", "beads.bead.created",
			message("m13", machines, watcher.Author, "The purse is half spent", map[string]string{"kind": "notice", "origin": watcher.OriginNotice}), WakePurseThreshold},
		{"a person edits the steward's notes", "beads.bead.updated",
			bridge.BeadEvent{ID: "n1", Type: "note", Title: NotesTitle(land), Status: "open", Actor: "ada@example.com"}, WakeNotes},
		{"the steward writes its own notes", "beads.bead.updated",
			bridge.BeadEvent{ID: "n1", Type: "note", Title: NotesTitle(land), Status: "open", Actor: land + "-steward-ab12"}, ""},
		{"a farm snap in #general wakes once, as a #general message", "beads.bead.created",
			message("m16", general, "oidc:ada@example.com", "", snapFields()), WakeGeneral},
		{"a farm snap with @steward in #crew wakes once, as a mention", "beads.bead.created",
			message("m17", crew, "oidc:bob@example.com", "@steward is this bed dry?", snapFields()), WakeMention},
		{"a farm snap with no mention in #crew", "beads.bead.created",
			message("m18", crew, "oidc:bob@example.com", "", snapFields()), ""},
		{"an agent's farm snap in #general", "beads.bead.created",
			message("m19", general, "agent:"+land+"-fix-the-tests-x1y", "", snapFields()), ""},
		{"the provisioner's wake post in #machines wakes once, as the land opening", "beads.bead.created",
			message("m14", machines, OpenedAuthor, OpenedText(land), map[string]string{"kind": "notice", "origin": OriginLandOpened}), WakeOpened},
		{"a land_opened post by anyone else", "beads.bead.created",
			message("m15", machines, "agent:"+land+"-fix-the-tests-x1y", "land is open", map[string]string{"origin": OriginLandOpened}), ""},
		{"the provisioner's person-joined post in #machines wakes once, as a person joining", "beads.bead.created",
			message("m20", machines, OpenedAuthor, JoinedText("Ada Lovelace", "guest"), map[string]string{"kind": "notice", "origin": OriginPersonJoined}), WakePersonJoined},
		{"a person_joined post by anyone else", "beads.bead.created",
			message("m21", machines, "agent:"+land+"-fix-the-tests-x1y", "Ada joined", map[string]string{"origin": OriginPersonJoined}), ""},
	}
}

func inLand(b bridge.BeadEvent, name string) *bridge.BeadEvent {
	b.Labels = append(slices.Clone(b.Labels), "project:"+name)
	return &b
}

// fired is the labels of the wake entries one event fires, each entry
// evaluated on its own so an event that matched two would show both.
// evalAt is the instant the wake tests evaluate triggers at: a fixed time
// outside every land's digest hour (DefaultDigestHour, 8 UTC), so a CI run
// that happens to start at 08:xxZ does not see the daily-digest cron fire and
// mistake it for a bead-event wake (main 090aa288 failed
// TestSnapToStewardEndToEnd at 08:04Z on 2026-10-24 with "bean-field's
// steward woke on tomato-patch's snap: [daily-digest]").
var evalAt = time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)

func fired(t *testing.T, topic string, b *bridge.BeadEvent) []string {
	t.Helper()
	return firedOn(t, WakeTriggers(land, chans, DigestCron(land, DefaultDigestHour), "UTC"), topic, b)
}

// firedOn is fired for a given wake list.
func firedOn(t *testing.T, entries []scheduler.TriggerEntry, topic string, b *bridge.BeadEvent) []string {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var out []string
	for i, e := range entries {
		if e.Kind == scheduler.TriggerKindCron {
			continue
		}
		src := bridge.NewMultiTriggerSource(logger, nil)
		src.InjectBeadEvent(topic, b)
		tctx := scheduler.TriggerEvalContext{TriggerID: "steward", Project: land, Triggers: []scheduler.TriggerEntry{e}}
		now := evalAt
		decs := scheduler.EvaluateTriggersAll(context.Background(), src.Engine(), nil, logger, tctx, now)
		if e.Debounce != "" && len(decs) == 0 {
			// A debounced entry fires once the burst has settled.
			d, _ := time.ParseDuration(e.Debounce)
			decs = scheduler.EvaluateTriggersAll(context.Background(), src.Engine(), nil, logger, tctx, now.Add(d+time.Minute))
		}
		if len(decs) > 1 {
			t.Errorf("entry %d (%s) fired %d times for one event", i, e.Label, len(decs))
		}
		if len(decs) > 0 {
			out = append(out, e.Label)
		}
	}
	return out
}

func TestWakeTriggers_EachEventWakesOnce(t *testing.T) {
	for _, w := range wakes() {
		t.Run(w.name, func(t *testing.T) {
			got := fired(t, w.topic, inLand(w.bead, land))
			var want []string
			if w.want != "" {
				want = []string{w.want}
			}
			if !slices.Equal(got, want) {
				t.Fatalf("fired %q, want %q", got, want)
			}
		})
	}
}

func TestWakeTriggers_AnotherLandNeverWakes(t *testing.T) {
	for _, w := range wakes() {
		t.Run(w.name, func(t *testing.T) {
			if got := fired(t, w.topic, inLand(w.bead, "bean-field")); len(got) != 0 {
				t.Fatalf("an event in another land fired %q", got)
			}
			// A label-less bead is in no land at all.
			b := w.bead
			if got := fired(t, w.topic, &b); len(got) != 0 {
				t.Fatalf("an event in no land fired %q", got)
			}
		})
	}
}

func TestWakeTriggers_MachinesBurstWakesOnce(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var entry scheduler.TriggerEntry
	for _, e := range WakeTriggers(land, chans, DigestCron(land, DefaultDigestHour), "UTC") {
		if e.Label == WakeMachines {
			entry = e
		}
	}
	src := bridge.NewMultiTriggerSource(logger, nil)
	for i, text := range []string{"PR #12 opened", "CI red on #12", "CI green on #12"} {
		src.InjectBeadEvent("beads.bead.created", inLand(message(fmt.Sprintf("mb%d", i), machines, "agent:"+land+"-fix-x1y", text, nil), land))
	}
	tctx := scheduler.TriggerEvalContext{TriggerID: "steward", Project: land, Triggers: []scheduler.TriggerEntry{entry}}
	t0, fires := time.Now(), 0
	for m := 0; m < 30; m++ {
		fires += len(scheduler.EvaluateTriggersAll(context.Background(), src.Engine(), nil, logger, tctx, t0.Add(time.Duration(m)*time.Minute)))
		src.Engine().DrainEvents() // the engine drains the buffer after every cycle
	}
	if fires != 1 {
		t.Fatalf("a burst of three #machines posts fired %d wakes in 30 minutes, want 1", fires)
	}
}

func TestWakeTriggers_NoMachinesChannelNoMachinesWake(t *testing.T) {
	for _, e := range WakeTriggers(land, Channels{General: general}, DigestCron(land, DefaultDigestHour), "UTC") {
		if e.Label == WakeMachines {
			t.Fatal("a land with no #machines got a #machines wake")
		}
	}
}

func TestWakeTriggers_DailyDigestFiresOnceADay(t *testing.T) {
	cron := DigestCron(land, DefaultDigestHour)
	if cron != DigestCron(land, DefaultDigestHour) || !strings.HasSuffix(cron, " 8 * * *") {
		t.Fatalf("digest cron %q: want a stable daily cron at 08", cron)
	}
	entries := WakeTriggers(land, chans, cron, "UTC")
	digest := entries[len(entries)-1]
	if digest.Kind != scheduler.TriggerKindCron || digest.Label != WakeDigest {
		t.Fatalf("last entry = %+v, want the digest cron", digest)
	}
	if _, err := scheduler.ParseCron(cron); err != nil {
		t.Fatalf("ParseCron(%q): %v", cron, err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	day := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	// It fired yesterday; the engine records each fire's time as last_run.
	last := day.Add(-24 * time.Hour).Add(DefaultDigestHour * time.Hour).Add(time.Duration(digestMinute(t, cron)) * time.Minute)
	fires := 0
	for m := time.Duration(0); m < 24*time.Hour; m += time.Minute {
		tctx := scheduler.TriggerEvalContext{TriggerID: "steward", Timezone: "UTC", Triggers: []scheduler.TriggerEntry{digest}, LastRun: last}
		if dec := scheduler.EvaluateCronEntry(tctx, digest, 0, day.Add(m), logger); dec.Fire {
			fires++
			last = dec.Time
		}
	}
	if fires != 1 {
		t.Fatalf("digest fired %d times in a day (cron %q), want 1", fires, cron)
	}
}

func digestMinute(t *testing.T, cron string) int {
	t.Helper()
	var m int
	if _, err := fmt.Sscanf(cron, "%d ", &m); err != nil {
		t.Fatalf("digest cron %q has no minute: %v", cron, err)
	}
	return m
}

// fakeDaemon is a small farm: the tenant land, an operator land, and what
// Bind and Retire write.
type fakeDaemon struct {
	beads   []*beadsapi.BeadDetail
	created []beadsapi.CreateBeadRequest
	// writes are the retire path's updates, closes and comments, in order.
	writes []string
	// parent is each bead's parent-child parent, for a Parent query; waiting
	// are the beads with an open blocking dep, left out of a NoOpenDeps one.
	parent  map[string]string
	waiting map[string]bool
	// comments are each bead's comments, for GetComments.
	comments map[string][]beadsapi.Comment
}

// GetComments serves the farm's comments, as the quiet-gardener watch reads
// a plan's nudges.
func (d *fakeDaemon) GetComments(_ context.Context, id string) ([]beadsapi.Comment, error) {
	return d.comments[id], nil
}

// under reports whether id is in root's subtree, root excluded.
func (d *fakeDaemon) under(id, root string) bool {
	for p := d.parent[id]; p != ""; p = d.parent[p] {
		if p == root {
			return true
		}
	}
	return false
}

func (d *fakeDaemon) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	var out []*beadsapi.BeadDetail
	if q.Offset > 0 {
		return &beadsapi.ListBeadsResult{}, nil
	}
	for _, b := range d.beads {
		if (len(q.Types) == 0 || slices.Contains(q.Types, b.Type)) &&
			(len(q.Statuses) == 0 || slices.Contains(q.Statuses, b.Status)) &&
			!slices.ContainsFunc(q.Labels, func(l string) bool { return !slices.Contains(b.Labels, l) }) &&
			(q.Parent == "" || d.under(b.ID, q.Parent)) && (!q.NoOpenDeps || !d.waiting[b.ID]) &&
			(!q.Unassigned || b.Assignee == "") && fieldsMatch(b, q.Fields) {
			out = append(out, b)
		}
	}
	return &beadsapi.ListBeadsResult{Beads: out, Total: len(out)}, nil
}

// fieldsMatch is the daemon's field_filter: each key equal to its value.
func fieldsMatch(b *beadsapi.BeadDetail, want map[string]string) bool {
	for k, v := range want {
		if b.Fields[k] != v {
			return false
		}
	}
	return true
}

func (d *fakeDaemon) CreateBead(_ context.Context, req beadsapi.CreateBeadRequest) (string, error) {
	d.created = append(d.created, req)
	var fields map[string]any
	_ = json.Unmarshal(req.Fields, &fields)
	f := map[string]string{}
	for k, v := range fields {
		if s, ok := v.(string); ok {
			f[k] = s
		}
	}
	id := "tp-trig-1"
	if req.Type != "trigger" {
		id = fmt.Sprintf("tp-%s-%d", req.Type, len(d.created))
	}
	d.beads = append(d.beads, &beadsapi.BeadDetail{ID: id, Type: req.Type, Title: req.Title, Status: "open", Labels: req.Labels, Fields: f})
	return id, nil
}

func newFarm() *fakeDaemon {
	return &fakeDaemon{beads: []*beadsapi.BeadDetail{
		{ID: "tp-proj", Type: "project", Title: land, Status: "open", Labels: []string{"project:" + land}, Fields: map[string]string{
			"land_kind": "tenant", "git_url": "https://github.com/ada/tomatoes.git", "max_live_agents": "3",
			"land_default_channel": general, "account": "lands-" + land}},
		{ID: "kd-proj", Type: "project", Title: "gardenio", Status: "open", Fields: map[string]string{"land_kind": "operator"}},
		{ID: "tp-plan", Type: "epic", Title: "Add a watering schedule", Status: "open",
			Labels: []string{"project:" + land, "plan"}, Fields: map[string]string{"plan_state": "building"}},
		{ID: general, Type: "channel", Title: "#general", Status: "open", Labels: []string{"project:" + land}, Fields: map[string]string{"name": "general"}},
		{ID: machines, Type: "channel", Title: "#machines", Status: "open", Labels: []string{"project:" + land}, Fields: map[string]string{"name": MachinesChannel}},
	}}
}

func TestBind_MintsTheLandsSteward(t *testing.T) {
	d := newFarm()
	res, err := Bind(context.Background(), d, land, BindSpec{CreatedBy: "gardenio-provisioner"})
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if res.Adopted || res.ID != "tp-trig-1" || len(d.created) != 2 {
		t.Fatalf("want one fresh steward and its notes, got %+v (%d creates)", res, len(d.created))
	}
	// The notes are born with the steward, empty sections (step 9.2a).
	notes := d.created[0]
	if notes.Type != "doc" || notes.Title != NotesTitle(land) || !slices.Equal(notes.Labels, []string{"project:" + land}) ||
		!res.NotesCreated || res.NotesID != "tp-doc-1" {
		t.Fatalf("notes bead %+v, result %+v", notes, res)
	}
	var nf map[string]string
	if err := json.Unmarshal(notes.Fields, &nf); err != nil || nf["format"] != "md" || nf["content"] != EmptyNotes(land) {
		t.Fatalf("notes fields %v (%v)", nf, err)
	}
	req := d.created[1]
	if req.Type != "trigger" || req.Title != "Steward (tomato-patch)" {
		t.Fatalf("created %s %q", req.Type, req.Title)
	}
	for _, l := range []string{"trigger:multi", "role:gardener", MarkerLabel, "gardener-land:" + land, "project:" + land, "perpetual:true"} {
		if !slices.Contains(req.Labels, l) {
			t.Errorf("labels %v missing %q", req.Labels, l)
		}
	}
	var fields map[string]any
	if err := json.Unmarshal(req.Fields, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["role"] != Role || fields["action"] != "spawn_agent" || fields["project"] != land || fields["enabled"] != true {
		t.Fatalf("fields %v", fields)
	}
	if p, _ := fields["prompt"].(string); !strings.Contains(p, "steward of the land tomato-patch") || !strings.Contains(p, "- tp-plan: Add a watering schedule [building]") ||
		!strings.Contains(p, "steward-notes:tomato-patch (tp-doc-1)") || !strings.Contains(p, "## Answers (newest first)") {
		t.Fatalf("prompt not rendered for the land:\n%s", p)
	}
	entries, err := scheduler.ParseTriggers(fields["triggers"].(string))
	if err != nil || len(entries) != 11 {
		t.Fatalf("triggers = %d entries (%v), want 11", len(entries), err)
	}
	blob := fields["gardener"].(string)
	if v := bridge.ValidateGardenerBlob(blob); v != nil {
		t.Fatalf("blob not bounded: %v", v)
	}
	var g bridge.GardenerSpec
	if err := json.Unmarshal([]byte(blob), &g); err != nil {
		t.Fatal(err)
	}
	if !g.Standing || !g.OneSpawnAtATime || g.MaxRunsPerDay != DefaultMaxRunsPerDay || g.AssignBead != "" {
		t.Fatalf("gardener spec %+v", g)
	}

	again, err := Bind(context.Background(), d, land, BindSpec{})
	if err != nil || !again.Adopted || again.ID != res.ID || again.NotesID != "tp-doc-1" || again.NotesCreated || len(d.created) != 2 {
		t.Fatalf("a second bind adopts: %+v err=%v creates=%d", again, err, len(d.created))
	}
}

func TestBind_Refuses(t *testing.T) {
	t.Run("an operator land", func(t *testing.T) {
		if _, err := Bind(context.Background(), newFarm(), "gardenio", BindSpec{}); err == nil {
			t.Fatal("bound a steward to an operator land")
		}
	})
	t.Run("no such land", func(t *testing.T) {
		if _, err := Bind(context.Background(), newFarm(), "bean-field", BindSpec{}); err == nil {
			t.Fatal("bound a steward to a land that does not exist")
		}
	})
	t.Run("no #general yet", func(t *testing.T) {
		d := newFarm()
		delete(d.beads[0].Fields, "land_default_channel")
		if _, err := Bind(context.Background(), d, land, BindSpec{}); err == nil || !strings.Contains(err.Error(), ErrNoGeneral.Error()) {
			t.Fatalf("err = %v, want ErrNoGeneral", err)
		}
		if len(d.created) != 0 {
			t.Fatal("wrote a steward with no #general to wake on")
		}
	})
}

func TestTriggerPromptRenderer_RendersTheLandAtTheWake(t *testing.T) {
	d := newFarm()
	render := TriggerPromptRenderer(d)
	trig := bridge.TriggerDef{ID: "tp-trig-1", Role: Role, Project: land, Labels: []string{MarkerLabel, LandLabel(land)}}
	before, err := render(context.Background(), trig, bridge.FireEvent{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	d.beads = append(d.beads, &beadsapi.BeadDetail{ID: "tp-plan-2", Type: "epic", Title: "Prune the vines", Status: "open",
		Labels: []string{"project:" + land, "plan"}})
	after, err := render(context.Background(), trig, bridge.FireEvent{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(before, "Prune the vines") || !strings.Contains(after, "- tp-plan-2: Prune the vines") {
		t.Fatalf("a plan written after the bind shows at the next wake:\n%s", after)
	}
	if _, err := render(context.Background(), bridge.TriggerDef{ID: "x", Role: Role, Labels: []string{MarkerLabel}}, bridge.FireEvent{}); err == nil {
		t.Fatal("rendered a steward with no land")
	}
	if _, err := render(context.Background(), bridge.TriggerDef{ID: "y", Role: "crew", Project: land, Labels: trig.Labels}, bridge.FireEvent{}); err == nil {
		t.Fatal("rendered the land playbook for a trigger that spawns another role")
	}
}

func TestTriggerPromptRenderer_LandMustBeTheProject(t *testing.T) {
	d := newFarm()
	render := TriggerPromptRenderer(d)
	for name, trig := range map[string]bridge.TriggerDef{
		"another land's label":      {ID: "a", Role: Role, Project: "other-land", Labels: []string{MarkerLabel, LandLabel(land)}},
		"a second land's label":     {ID: "b", Role: Role, Project: land, Labels: []string{MarkerLabel, LandLabel("other-land"), LandLabel(land)}},
		"the project's label twice": {ID: "c", Role: Role, Project: land, Labels: []string{MarkerLabel, LandLabel(land), LandLabel(land)}},
		"no project":                {ID: "d", Role: Role, Labels: []string{MarkerLabel, LandLabel(land)}},
	} {
		if got, err := render(context.Background(), trig, bridge.FireEvent{}); err == nil {
			t.Fatalf("%s: rendered land state into a steward of another project:\n%s", name, got)
		}
	}
}

func machinesPost(id string, at time.Time, text string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{ID: id, Type: "message", Title: text, Status: "closed", CreatedAt: at,
		Labels: []string{"project:" + land}, Fields: map[string]string{"channel_id": machines, "author": "agent:" + land + "-fix-x1y", "text": text}}
}

func TestTriggerPromptRenderer_NotesThenMachinesDelta(t *testing.T) {
	d := newFarm()
	now := time.Now().UTC()
	prev := now.Add(-2 * time.Hour)
	d.beads = append(d.beads,
		machinesPost("mo", now.Add(-3*time.Hour), "before the last run"),
		machinesPost("mn", now.Add(-time.Hour), "PR #12 opened"),
		&beadsapi.BeadDetail{ID: "tp-run-1", Type: "agent", Status: "closed", CreatedAt: prev},
	)
	trig := bridge.TriggerDef{ID: "tp-trig-1", Role: Role, Project: land, Labels: []string{MarkerLabel, LandLabel(land)},
		Gardener: &bridge.GardenerSpec{LastAgentID: "tp-run-1"}}
	render := TriggerPromptRenderer(getterFarm{d})

	got, err := render(context.Background(), trig, bridge.FireEvent{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "There is no steward-notes:tomato-patch bead yet;") {
		t.Fatalf("no notes bead yet:\n%s", got)
	}
	if !strings.Contains(got, "PR #12 opened") || strings.Contains(got, "before the last run") {
		t.Fatalf("the delta starts at the previous run:\n%s", got)
	}

	// With notes, the mark in them wins over the previous run.
	d.beads = append(d.beads, &beadsapi.BeadDetail{ID: "tp-notes", Type: "note", Title: NotesTitle(land), Status: "open",
		Labels: []string{"project:" + land}, Description: "## Holds\n- hold PR 12 until ada has read it",
		Fields: map[string]string{NotesHighWaterField: now.Add(-30 * time.Minute).Format(time.RFC3339)}})
	got, err = render(context.Background(), trig, bridge.FireEvent{})
	if err != nil {
		t.Fatal(err)
	}
	notes, snapshot := strings.Index(got, "hold PR 12 until ada has read it"), strings.Index(got, "## The land now")
	if notes < 0 || snapshot < 0 || notes > snapshot {
		t.Fatalf("the notes render in full before the land snapshot:\n%s", got)
	}
	if strings.Contains(got, "PR #12 opened") || !strings.Contains(got, "#machines since your last run\n\nNothing new.") {
		t.Fatalf("nothing in #machines after the notes' mark:\n%s", got)
	}

	// A busy channel is cut to the newest lines.
	for i := range machinesLines + 5 {
		d.beads = append(d.beads, machinesPost(fmt.Sprintf("mb%d", i), now.Add(-10*time.Minute+time.Duration(i)*time.Second), fmt.Sprintf("line %d", i)))
	}
	got, err = render(context.Background(), trig, bridge.FireEvent{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "(5 older messages since your last run are not shown.)") ||
		strings.Contains(got, "line 4\n") || !strings.Contains(got, fmt.Sprintf("line %d", machinesLines+4)) {
		t.Fatalf("want the newest %d lines and a count of the rest:\n%s", machinesLines, got)
	}
}

// getterFarm adds the single-bead read the renderer uses for the previous run.
type getterFarm struct{ *fakeDaemon }

func (g getterFarm) GetBead(_ context.Context, id string) (*beadsapi.BeadDetail, error) {
	for _, b := range g.beads {
		if b.ID == id {
			return b, nil
		}
	}
	return nil, nil
}
