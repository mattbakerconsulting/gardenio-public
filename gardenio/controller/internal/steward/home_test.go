package steward

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
	"gardenio/controller/internal/lands/purse"
	"gardenio/controller/internal/scheduler"
)

// The home garden's steward (step 9.13): the operator land on the Commons
// whose operator is a person gets a steward like a tenant land's, with an
// unmetered purse and a wake list that leaves #general and #machines to the
// fleet.

const (
	home         = "gardenio"
	homeOwner    = "oidc:owner@example.com"
	homeGeneral  = "gd-general"
	homeMachines = "gd-machines"
)

var homeChans = Channels{General: homeGeneral, Machines: homeMachines}

// newHomeFarm is newFarm with the home garden: an operator land with no
// land_kind (as the live gardenio project bead has none), a Commons and a
// person as its operator. "ops" is an operator land that is no home garden.
func newHomeFarm() *fakeDaemon {
	d := newFarm()
	d.beads = slices.DeleteFunc(d.beads, func(b *beadsapi.BeadDetail) bool { return b.ID == "kd-proj" })
	d.beads = append(d.beads,
		&beadsapi.BeadDetail{ID: "gd-proj", Type: "project", Title: home, Status: "open", Labels: []string{"project:" + home},
			OperatedBy: homeOwner, OperatedByDisplay: "The Owner",
			Fields: map[string]string{"git_url": "https://github.com/acme/gardenio", "land_default_channel": homeGeneral}},
		&beadsapi.BeadDetail{ID: "ops-proj", Type: "project", Title: "ops", Status: "open", Labels: []string{"project:ops"},
			Fields: map[string]string{"land_kind": "operator", "git_url": "https://github.com/acme/ops"}},
		&beadsapi.BeadDetail{ID: homeGeneral, Type: "channel", Title: "#general", Status: "open", Labels: []string{"project:" + home}, Fields: map[string]string{"name": "general"}},
		&beadsapi.BeadDetail{ID: homeMachines, Type: "channel", Title: "#machines", Status: "open", Labels: []string{"project:" + home}, Fields: map[string]string{"name": MachinesChannel}},
		&beadsapi.BeadDetail{ID: "gd-plan", Type: "epic", Title: "Ship the steward", Status: "open",
			Labels: []string{"project:" + home, "plan"}, Fields: map[string]string{"plan_state": "building"}},
	)
	return d
}

func TestBind_HomeGarden(t *testing.T) {
	d := newHomeFarm()
	res, err := Bind(context.Background(), d, home, BindSpec{CreatedBy: "matthew-laptop"})
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if res.Adopted || !res.NotesCreated || len(d.created) != 2 {
		t.Fatalf("want a fresh steward and its notes, got %+v (%d creates)", res, len(d.created))
	}
	var labels []string
	for _, e := range res.Triggers {
		labels = append(labels, e.Label)
	}
	if want := []string{WakeMention, WakeDecision, WakeNotes, WakeOpened, WakeQuietGardeners, WakePersonJoined, WakeDigest}; !slices.Equal(labels, want) {
		t.Fatalf("home wakes = %v, want %v", labels, want)
	}
	req := d.created[1]
	if req.Type != "trigger" || !slices.Contains(req.Labels, LandLabel(home)) || !slices.Contains(req.Labels, "project:"+home) {
		t.Fatalf("created %s %v", req.Type, req.Labels)
	}
	var fields map[string]any
	if err := json.Unmarshal(req.Fields, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["role"] != Role || fields["project"] != home {
		t.Fatalf("fields %v", fields)
	}
	p, _ := fields["prompt"].(string)
	for _, want := range []string{
		"steward of the land gardenio",
		"Purse: unmetered (" + purse.UnmeteredReason + ")",
		"This is the home garden",
		"- The Owner (owner): " + homeOwner,
		"- gd-plan: Ship the steward [building]",
		"3. RUN APPROVED PLANS.",
		"never kill one",
		"5. NO WALL.",
		"6. WRITE THE LAND'S ADVICE.",
		"gb escalation list --held-by steward:gardenio",
		"project:gardenio, and its role:, agent: or g<n>: selectors stay inside",
		"approve anything but an advice ask for gardenio (duty 6)",
		"Spawn or kill any agent",
		"write in #general: the owner is the operator",
		"ask the owner, who sets it",
		"you may, alone, " + homeAloneBase + ";",
		"how to ask: mention @steward in any channel of the land",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("home prompt lacks %q", want)
		}
	}
	for _, never := range []string{
		"SPAWN GARDENERS WITHIN THE PURSE", "PARK AT THE WALL", "gb spawn --role", "gb lands purse",
		"credits remaining", "estimated at", "#support", "Live agents allowed at once", "spawn gardeners", "within the purse",
	} {
		if strings.Contains(p, never) {
			t.Errorf("home prompt says %q:\n%s", never, p)
		}
	}
}

func TestBind_TenantPromptHasNoHomeGarden(t *testing.T) {
	d := newHomeFarm()
	if _, err := Bind(context.Background(), d, land, BindSpec{}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	var fields map[string]any
	_ = json.Unmarshal(d.created[len(d.created)-1].Fields, &fields)
	p, _ := fields["prompt"].(string)
	for _, want := range []string{"3. SPAWN GARDENERS WITHIN THE PURSE.", "5. PARK AT THE WALL.", "Live agents allowed at once, you included: 3.", "write in #support"} {
		if !strings.Contains(p, want) {
			t.Errorf("tenant prompt lacks %q", want)
		}
	}
	if strings.Contains(p, "home garden") || strings.Contains(p, "RUN APPROVED PLANS") || strings.Contains(p, "WRITE THE LAND'S ADVICE") {
		t.Errorf("tenant prompt carries the home garden's duties:\n%s", p)
	}
}

func TestBind_RefusesAnOperatorLandThatIsNotHome(t *testing.T) {
	_, err := Bind(context.Background(), newHomeFarm(), "ops", BindSpec{})
	if err == nil || !strings.Contains(err.Error(), "not the home garden") {
		t.Fatalf("err = %v, want the operator-land refusal", err)
	}
}

func TestReadPurse_HomeIsUnmetered(t *testing.T) {
	d := newHomeFarm()
	now := time.Now()
	p := ReadPurse(context.Background(), d, home, now)
	if !p.IsUnmetered() || p.Blocked || p.RemainingCredits != nil || p.Reason != purse.UnmeteredReason {
		t.Fatalf("home purse %+v, want unmetered and never blocked", p)
	}
	if got := DecideStepSpawn(p.CreditsOnly(), Estimate{}, 0, 0); got.Code != SpawnPurseUnknown {
		// The steward's own check is never asked for the home garden (it
		// never gb spawns there); this pins that unmetered is not "ok".
		t.Fatalf("DecideStepSpawn on an unmetered purse = %+v", got)
	}
	if ops := ReadPurse(context.Background(), d, "ops", now); ops.State != purse.StateUnknown || !ops.Blocked {
		t.Fatalf("an operator land that is not home reads %+v, want unknown", ops)
	}
	if e := LandStepEstimate(context.Background(), d, home, now); e.Credits != 0 {
		t.Fatalf("home step estimate = %v, want none", e)
	}
}

func TestHomeWakeTriggers(t *testing.T) {
	entries := HomeWakeTriggers(home, homeChans, DigestCron(home, DefaultDigestHour), "UTC")
	for _, w := range []struct {
		name  string
		topic string
		bead  bridge.BeadEvent
		want  string
	}{
		{"the owner's @steward in #general", "beads.bead.created",
			message("h1", homeGeneral, homeOwner, "@steward what is in flight?", nil), WakeMention},
		{"the owner's @steward in #crew", "beads.bead.created",
			message("h2", "gd-crew", homeOwner, "@steward hello", nil), WakeMention},
		{"the owner talking to @gardenio in #general", "beads.bead.created",
			message("h3", homeGeneral, homeOwner, "@gardenio fix the build", nil), ""},
		{"fleet traffic in #machines", "beads.bead.created",
			message("h4", homeMachines, "agent:gardenio-fix-x1y", "PR #12 opened", nil), ""},
		{"an agent's @steward", "beads.bead.created",
			message("h5", homeMachines, "agent:gardenio-fix-x1y", "@steward look", nil), ""},
		{"a home steward run's decision resolved", "beads.bead.closed",
			bridge.BeadEvent{ID: "d1", Type: "decision", Status: "closed", CreatedBy: home + "-steward-ab12"}, WakeDecision},
		{"the owner edits the notes", "beads.bead.updated",
			bridge.BeadEvent{ID: "n1", Type: "doc", Title: NotesTitle(home), Status: "open", Actor: "owner@example.com"}, WakeNotes},
	} {
		t.Run(w.name, func(t *testing.T) {
			got := firedOn(t, entries, w.topic, inLand(w.bead, home))
			var want []string
			if w.want != "" {
				want = []string{w.want}
			}
			if !slices.Equal(got, want) {
				t.Fatalf("fired %q, want %q", got, want)
			}
		})
	}
	if got := firedOn(t, entries, "beads.bead.created", inLand(message("h6", homeGeneral, homeOwner, "@steward hi", nil), land)); len(got) != 0 {
		t.Fatalf("another land's mention woke the home steward: %v", got)
	}
	var cron int
	for _, e := range entries {
		if e.Kind == scheduler.TriggerKindCron && e.Label == WakeDigest {
			cron++
		}
	}
	if cron != 1 {
		t.Fatalf("home wakes have %d digest crons, want 1", cron)
	}
}

func TestFormatDigest_Unmetered(t *testing.T) {
	d := &Digest{Land: home, AsOf: evalAt, Since: evalAt.Add(-24 * time.Hour), Unmetered: true, Gardeners: 1}
	out := FormatDigest(d)
	if !strings.Contains(out, "Credits: none; the land is operator-paid") || strings.Contains(out, "granted") || strings.Contains(out, "remaining") {
		t.Fatalf("unmetered digest credits line:\n%s", out)
	}
	if strings.Contains(d.PostCommand(), "--used") {
		t.Fatalf("unmetered digest records used credits: %s", d.PostCommand())
	}
}
