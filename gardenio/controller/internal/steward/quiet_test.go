package steward

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/plangardener"
	"gardenio/controller/internal/scheduler"
	"gardenio/controller/internal/steward/gardenerwatch"
)

// The home steward watches its plans' gardeners (gd-bo2RUmtpHb). The watch
// itself, its asks and its wake are gardenerwatch's, tested there; these are
// the steward's side: LoadLand reads it for the home garden only, the prompt
// shows it, and the home steward's wakes carry its entry.

var quietNow = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

// quietFarm is the home garden at at with one building plan whose event-mode
// gardener last ran three hours before, two steps ready.
func quietFarm(at time.Time) *fakeDaemon {
	d := newHomeFarm()
	d.parent, d.waiting = map[string]string{}, map[string]bool{}
	d.beads = append(d.beads, homePlan("gd-q1", "Quiet one", "building"), &beadsapi.BeadDetail{
		ID: "gd-qg1", Type: "trigger", Title: "gardener gd-q1", Status: "open", CreatedAt: at.Add(-72 * time.Hour),
		Labels: []string{"trigger:multi", "role:gardener", plangardener.GardenerEpicLabel("gd-q1"), plangardener.MarkerLabel},
		Fields: map[string]string{"enabled": "true", "last_run": at.Add(-3 * time.Hour).Format(time.RFC3339)},
	})
	homeStep(d, "gd-qs1a", "gd-q1", "open", false)
	homeStep(d, "gd-qs1b", "gd-q1", "open", false)
	return d
}

func TestRenderPrompt_QuietPlans(t *testing.T) {
	st, err := LoadLand(context.Background(), quietFarm(quietNow), home, quietNow, time.Time{})
	if err != nil {
		t.Fatalf("LoadLand: %v", err)
	}
	if len(st.Land.Quiet) != 1 || st.Land.Quiet[0].Plan != "gd-q1" || st.Land.Quiet[0].Ask != gardenerwatch.AskFire {
		t.Fatalf("quiet = %+v, want gd-q1 to fire", st.Land.Quiet)
	}
	// The farm's own gd-plan has no gardener: the gardener-less list's.
	if !slices.ContainsFunc(st.Land.Stalled, func(s gardenerwatch.StalledPlan) bool { return s.ID == "gd-plan" }) {
		t.Error("gd-plan left the gardener-less list")
	}
	st.Land.WakeEntry = WakeQuietGardeners
	p, err := RenderPrompt(st.Land)
	if err != nil {
		t.Fatal(err)
	}
	section := "Plan gardeners to see to (building, with a live gardener that is quiet while\nsteps are ready, or still timed):\n- " +
		gardenerwatch.Describe(st.Land.Quiet[0]) + "\n\n"
	for _, want := range []string{
		section,
		"Woken by: " + WakeQuietGardeners,
		"- " + WakeQuietGardeners + `: a plan's gardener is quiet or still timed`,
		"SEE TO QUIET AND TIMED GARDENERS", "has not run for 2 hours",
		"Run the line's Fix yourself",
		`kd comment add <plan> "steward: gardener <gardener> quiet <time> with <n> steps ready (<steps>); fired it"`,
		`ending "; nudging" wakes its gardener`, "run the line's Nudge exactly as written",
		"at most 2 times a day", "answered 2 nudges today, but the steps named",
		"also tell the owner once in #general", "switches the gardener to event\n     mode",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("home prompt lacks %q", want)
		}
	}
	// The owner asked (11-02) for no decisions that ask them to run a command.
	for _, gone := range []string{"fire it?", "I fired it", "put it in event mode?", "I switched it", "You cannot fire a gardener"} {
		if strings.Contains(p, gone) {
			t.Errorf("home prompt still has %q", gone)
		}
	}
}

// A tenant land's steward runs its own plans' steps: no watch, no wake.
func TestQuietWatch_HomeOnly(t *testing.T) {
	st, err := LoadLand(context.Background(), newFarm(), land, quietNow, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	p, _ := RenderPrompt(st.Land)
	if len(st.Land.Quiet) != 0 || strings.Contains(p, "Plan gardeners to see to") || strings.Contains(p, WakeQuietGardeners) {
		t.Fatal("a tenant land is shown the quiet-gardener watch")
	}
	if slices.ContainsFunc(WakeTriggers(land, chans, DigestCron(land, DefaultDigestHour), "UTC"),
		func(e scheduler.TriggerEntry) bool { return e.Label == WakeQuietGardeners }) {
		t.Fatal("a tenant steward has the quiet-gardeners wake")
	}
}

// failReady fails the watch's ready-step read only.
type failReady struct{ *fakeDaemon }

func (f failReady) ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	if q.Unassigned {
		return nil, errors.New("daemon unavailable")
	}
	return f.fakeDaemon.ListBeadsFiltered(ctx, q)
}

// A watch that cannot be read is said; the rest of the prompt renders.
func TestRenderPrompt_QuietUnread(t *testing.T) {
	st, err := LoadLand(context.Background(), failReady{quietFarm(quietNow)}, home, quietNow, time.Time{})
	if err != nil {
		t.Fatalf("LoadLand failed with the watch: %v", err)
	}
	p, _ := RenderPrompt(st.Land)
	if !strings.Contains(p, "Plan gardeners to see to: could not be read (quiet_gardeners: reading plan gd-q1's ready steps") ||
		!strings.Contains(p, "Plans with no live gardener") {
		t.Fatalf("unread watch:\n%s", p)
	}
}

// quietEntry is the one quiet-gardeners entry of entries.
func quietEntry(t *testing.T, entries []scheduler.TriggerEntry) scheduler.TriggerEntry {
	t.Helper()
	var found []scheduler.TriggerEntry
	for _, e := range entries {
		if e.Label == WakeQuietGardeners {
			found = append(found, e)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d quiet-gardeners entries, want 1", len(found))
	}
	return found[0]
}

func TestHomeWakeTriggers_CarryTheQuietWatch(t *testing.T) {
	e := quietEntry(t, HomeWakeTriggers(home, homeChans, DigestCron(home, DefaultDigestHour), "UTC"))
	want, _ := json.Marshal(gardenerwatch.Entry(home, "UTC"))
	if got, _ := json.Marshal(e); string(got) != string(want) {
		t.Fatalf("entry = %s, want %s", got, want)
	}
}

// The live home steward was bound before the watch (gd-q7lDNY0L94: mention,
// decision, notes, opened, digest). --bind brings it to a fresh bind's wakes,
// once.
func TestUpgradeHomeWakes_AddsTheWatchToABoundSteward(t *testing.T) {
	fresh := HomeWakeTriggers(home, homeChans, DigestCron(home, DefaultDigestHour), "America/Los_Angeles")
	var old []scheduler.TriggerEntry
	for _, e := range fresh {
		if e.Label != WakePersonJoined && e.Label != WakeQuietGardeners {
			old = append(old, e)
		}
	}
	up, changed := UpgradeHomeWakes(home, old)
	want, _ := json.Marshal(fresh)
	got, _ := json.Marshal(up)
	if !changed || string(got) != string(want) {
		t.Fatalf("upgraded (changed %v):\n%s\nwant a fresh bind's:\n%s", changed, got, want)
	}
	if again, changed := UpgradeHomeWakes(home, up); changed || len(again) != len(up) {
		t.Fatal("an up-to-date home steward was upgraded again")
	}
	if _, changed := UpgradeHomeWakes(home, nil); changed {
		t.Fatal("a steward with no triggers (timed mode) was written")
	}
	// A tenant steward's upgrade adds no watch.
	if tenant, _ := UpgradeWakes(home, old); slices.ContainsFunc(tenant, func(e scheduler.TriggerEntry) bool { return e.Label == WakeQuietGardeners }) {
		t.Fatal("UpgradeWakes added the home watch")
	}
}

func TestProvisionHook_UpgradeHomeWritesOnce(t *testing.T) {
	d := newHomeFarm()
	h := ProvisionHook{Client: getterFarm{d}, Spec: BindSpec{CreatedBy: "matthew-laptop"}}
	trig, err := h.Bind(context.Background(), home, "")
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	entries, _ := scheduler.ParseTriggers(trig.Fields["triggers"])
	entries = slices.DeleteFunc(entries, func(e scheduler.TriggerEntry) bool { return e.Label == WakeQuietGardeners })
	raw, _ := json.Marshal(entries)
	trig.Fields["triggers"] = string(raw)
	d.writes = nil
	if wrote, err := h.UpgradeHome(context.Background(), home, trig); err != nil || !wrote || len(d.writes) != 1 {
		t.Fatalf("an old home steward: wrote %v, writes %v, err %v", wrote, d.writes, err)
	}
	up, _ := scheduler.ParseTriggers(trig.Fields["triggers"])
	quietEntry(t, up)
	if wrote, err := h.UpgradeHome(context.Background(), home, trig); err != nil || wrote || len(d.writes) != 1 {
		t.Fatalf("an up-to-date home steward was written: %v %v", d.writes, err)
	}
}

func TestIsHome(t *testing.T) {
	d := newHomeFarm()
	for l, want := range map[string]bool{home: true, land: false, "ops": false} {
		if got, err := IsHome(context.Background(), d, l); err != nil || got != want {
			t.Errorf("IsHome(%s) = %v, %v; want %v", l, got, err, want)
		}
	}
}
