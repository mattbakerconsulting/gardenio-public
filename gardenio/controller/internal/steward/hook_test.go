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
	"gardenio/controller/internal/scheduler"
)

func TestProvisionHook_BindsTheStewardOnceAndReturnsItsTrigger(t *testing.T) {
	d := newFarm()
	h := ProvisionHook{Client: getterFarm{d}, Spec: BindSpec{CreatedBy: "gardenio-controller"}}
	trig, err := h.Bind(context.Background(), land, "oidc:op@example.com")
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if trig.ID != "tp-trig-1" || trig.Type != "trigger" || !slices.Contains(trig.Labels, LandLabel(land)) {
		t.Fatalf("trigger = %+v", trig)
	}
	// The bind creates the notes bead (step 9.2a), then the trigger.
	creates := len(d.created)
	if i := slices.IndexFunc(d.created, func(r beadsapi.CreateBeadRequest) bool { return r.Type == "trigger" }); i < 0 || d.created[i].CreatedBy != "gardenio-controller" {
		t.Errorf("trigger create = %d in %+v", i, d.created)
	}
	again, err := h.Bind(context.Background(), land, "")
	if err != nil || again.ID != trig.ID || len(d.created) != creates {
		t.Fatalf("a second bind adopts: %+v err=%v creates=%d", again, err, len(d.created))
	}
}

func TestProvisionHook_WakePostsInMachines(t *testing.T) {
	d := newFarm()
	h := ProvisionHook{Client: getterFarm{d}}
	if err := h.Wake(context.Background(), land, "lands-provision:app1:steward-wake"); err != nil {
		t.Fatalf("Wake: %v", err)
	}
	if len(d.created) != 1 {
		t.Fatalf("%d creates, want one post", len(d.created))
	}
	req := d.created[0]
	var f map[string]string
	if err := json.Unmarshal(req.Fields, &f); err != nil {
		t.Fatal(err)
	}
	if req.Type != "message" || req.Status != "closed" || req.IdempotencyKey != "lands-provision:app1:steward-wake" ||
		!slices.Equal(req.Labels, []string{"project:" + land}) || req.CreatedBy != OpenedAuthor {
		t.Fatalf("post = %+v", req)
	}
	if f["channel_id"] != machines || f["origin"] != OriginLandOpened || f["author"] != OpenedAuthor || f["kind"] != "notice" ||
		!strings.Contains(f["text"], "Land "+land+" is open") {
		t.Fatalf("post fields = %v", f)
	}
	// What the post is as an event wakes the steward's land-opened entry,
	// and only it.
	ev := message("w1", f["channel_id"], f["author"], f["text"], map[string]string{"kind": f["kind"], "origin": f["origin"]})
	if got := fired(t, "beads.bead.created", inLand(ev, land)); !slices.Equal(got, []string{WakeOpened}) {
		t.Fatalf("the wake post fires %v, want [%s]", got, WakeOpened)
	}
}

func TestProvisionHook_WakeFallsBackToGeneral(t *testing.T) {
	d := newFarm()
	d.beads = slices.DeleteFunc(d.beads, func(b *beadsapi.BeadDetail) bool { return b.ID == machines })
	if err := (ProvisionHook{Client: getterFarm{d}}).Wake(context.Background(), land, "k"); err != nil {
		t.Fatalf("Wake: %v", err)
	}
	var f map[string]string
	_ = json.Unmarshal(d.created[0].Fields, &f)
	if f["channel_id"] != general {
		t.Fatalf("posted in %q, want #general when the land has no #machines", f["channel_id"])
	}
}

func (d *fakeDaemon) bead(id string) *beadsapi.BeadDetail {
	for _, b := range d.beads {
		if b.ID == id {
			return b
		}
	}
	return nil
}

func (d *fakeDaemon) UpdateBeadFieldsTyped(_ context.Context, id string, fields map[string]any) error {
	b := d.bead(id)
	if b == nil {
		return fmt.Errorf("no bead %s", id)
	}
	for k, v := range fields {
		b.Fields[k] = fmt.Sprint(v)
		d.writes = append(d.writes, fmt.Sprintf("update:%s:%s=%v", id, k, v))
	}
	return nil
}

func (d *fakeDaemon) CloseBead(_ context.Context, id string, _ map[string]string) error {
	b := d.bead(id)
	if b == nil {
		return fmt.Errorf("no bead %s", id)
	}
	b.Status = "closed"
	d.writes = append(d.writes, "close:"+id)
	return nil
}

func (d *fakeDaemon) AddComment(_ context.Context, id, author, text string) error {
	d.writes = append(d.writes, fmt.Sprintf("comment:%s:%s:%s", id, author, text))
	return nil
}

// loaderFarm is the farm as trigger-engine's trigger:multi loader reads it.
// The embedded client is nil: a load that did more than list would panic.
type loaderFarm struct {
	bridge.TriggerBeadClient
	d *fakeDaemon
}

func (l loaderFarm) ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	return l.d.ListBeadsFiltered(ctx, q)
}

// loadedDigests is the daily-digest crons of the steward triggers
// trigger-engine would load from the farm.
func loadedDigests(t *testing.T, d *fakeDaemon) []string {
	t.Helper()
	defs, err := bridge.NewMultiTriggerSource(slog.New(slog.NewTextHandler(io.Discard, nil)), nil).
		LoadTriggers(context.Background(), loaderFarm{d: d})
	if err != nil {
		t.Fatalf("LoadTriggers: %v", err)
	}
	var out []string
	for _, def := range defs {
		for _, e := range def.Triggers {
			if e.Kind == scheduler.TriggerKindCron && e.Label == WakeDigest {
				out = append(out, def.ID+" "+e.Cron)
			}
		}
	}
	return out
}

func TestProvisionHook_RetireDisarmsAndClosesTheSteward(t *testing.T) {
	d := newFarm()
	h := ProvisionHook{Client: getterFarm{d}, Spec: BindSpec{CreatedBy: "gardenio-controller"}}
	trig, err := h.Bind(context.Background(), land, "")
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	// Another land's steward, which closing this land must leave alone.
	other := &beadsapi.BeadDetail{ID: "tp-trig-other", Type: "trigger", Title: "Steward (beta)", Status: "open",
		Labels: []string{"trigger:multi", "role:gardener", MarkerLabel, LandLabel("beta"), "project:beta"}, Fields: map[string]string{}}
	d.beads = append(d.beads, other)
	if got := loadedDigests(t, d); len(got) != 1 || !strings.HasPrefix(got[0], trig.ID+" ") {
		t.Fatalf("before the close trigger-engine loads digests %v, want the steward's", got)
	}

	if err := h.Retire(context.Background(), land, "Retired: the operator closed land "+land+"."); err != nil {
		t.Fatalf("Retire: %v", err)
	}
	want := []string{
		"update:" + trig.ID + ":enabled=false",
		"close:" + trig.ID,
		"comment:" + trig.ID + ":gardenio-controller:Retired: the operator closed land " + land + ".",
	}
	if !slices.Equal(d.writes, want) {
		t.Fatalf("writes = %v, want %v", d.writes, want)
	}
	if b := d.bead(trig.ID); b.Status != "closed" || b.Fields["enabled"] != "false" {
		t.Fatalf("steward = %s enabled=%q", b.Status, b.Fields["enabled"])
	}
	if other.Status != "open" {
		t.Fatal("another land's steward was closed")
	}
	// Its cron no longer fires: trigger-engine loads open triggers only.
	if got := loadedDigests(t, d); len(got) != 0 {
		t.Fatalf("after the close trigger-engine still loads digests %v", got)
	}
	if s, err := FindSteward(context.Background(), d, land); err != nil || s != nil {
		t.Fatalf("FindSteward after the close = %+v, %v", s, err)
	}

	// Again (a re-run after a crash) finds nothing open and writes nothing.
	d.writes = nil
	if err := h.Retire(context.Background(), land, "again"); err != nil || len(d.writes) != 0 {
		t.Fatalf("a second Retire: err=%v writes=%v", err, d.writes)
	}
}

func TestLoadLand_ReadsTheGrant(t *testing.T) {
	d := newFarm()
	st, err := LoadLand(context.Background(), d, land, time.Now(), time.Time{})
	if err != nil || st.Land.Authority != AuthorityAsk {
		t.Fatalf("unset grant = %q (%v), want ask", st.Land.Authority, err)
	}
	d.beads[0].Fields[AuthorityField] = "merge-on-green"
	if st, _ = LoadLand(context.Background(), d, land, time.Now(), time.Time{}); st.Land.Authority != AuthorityMergeOnGreen {
		t.Fatalf("grant = %q, want merge-on-green", st.Land.Authority)
	}
}

func TestProvisionHook_GreetPostsInMachinesAndWakesOnce(t *testing.T) {
	d := newFarm()
	h := ProvisionHook{Client: getterFarm{d}}
	if err := h.Greet(context.Background(), land, "Ada Lovelace", "member", "lands-invite:inv1:greet"); err != nil {
		t.Fatalf("Greet: %v", err)
	}
	if len(d.created) != 1 {
		t.Fatalf("%d creates, want one post", len(d.created))
	}
	req := d.created[0]
	var f map[string]string
	if err := json.Unmarshal(req.Fields, &f); err != nil {
		t.Fatal(err)
	}
	if req.Type != "message" || req.Status != "closed" || req.IdempotencyKey != "lands-invite:inv1:greet" ||
		!slices.Equal(req.Labels, []string{"project:" + land}) || req.CreatedBy != OpenedAuthor {
		t.Fatalf("post = %+v", req)
	}
	if f["channel_id"] != machines || f["origin"] != OriginPersonJoined || f["author"] != OpenedAuthor || f["kind"] != "notice" ||
		f["text"] != "Ada Lovelace joined the land as a member. Its steward is woken to greet them in #general." {
		t.Fatalf("post fields = %v", f)
	}
	ev := message("w1", f["channel_id"], f["author"], f["text"], map[string]string{"kind": f["kind"], "origin": f["origin"]})
	if got := fired(t, "beads.bead.created", inLand(ev, land)); !slices.Equal(got, []string{WakePersonJoined}) {
		t.Fatalf("the greet post fires %v, want [%s]", got, WakePersonJoined)
	}
	// The home garden's steward hears it too.
	if got := firedOn(t, HomeWakeTriggers(land, chans, DigestCron(land, DefaultDigestHour), "UTC"), "beads.bead.created", inLand(ev, land)); !slices.Equal(got, []string{WakePersonJoined}) {
		t.Fatalf("on the home garden the greet post fires %v, want [%s]", got, WakePersonJoined)
	}
}

func TestJoinedText_NamesNobodyWhenThereIsNoName(t *testing.T) {
	if got := JoinedText("  ", ""); got != "A newcomer joined the land as a guest. Its steward is woken to greet them in #general." {
		t.Fatalf("JoinedText = %q", got)
	}
}

func TestUpgradeWakes_BringsAnOldStewardUpToAFreshBind(t *testing.T) {
	for name, fresh := range map[string][]scheduler.TriggerEntry{
		"tenant": WakeTriggers(land, chans, DigestCron(land, DefaultDigestHour), "UTC"),
		"home":   HomeWakeTriggers(land, chans, DigestCron(land, DefaultDigestHour), "UTC"),
	} {
		t.Run(name, func(t *testing.T) {
			// A steward bound before step 9.14: no person-joined entry, and a
			// #machines delta that does not keep its post out.
			var old []scheduler.TriggerEntry
			for _, e := range fresh {
				if e.Label == WakePersonJoined {
					continue
				}
				if e.Label == WakeMachines {
					m := *e.Match
					m.All = m.All[:len(m.All)-1]
					e.Match = &m
				}
				old = append(old, e)
			}
			post := inLand(message("w1", machines, OpenedAuthor, JoinedText("Ada", "guest"), map[string]string{"kind": "notice", "origin": OriginPersonJoined}), land)
			if name == "tenant" {
				if got := firedOn(t, old, "beads.bead.created", post); !slices.Equal(got, []string{WakeMachines}) {
					t.Fatalf("before the upgrade the post fires %v, want only the delta", got)
				}
			}
			up, changed := UpgradeWakes(land, old)
			if !changed {
				t.Fatal("an old steward was not upgraded")
			}
			want, _ := json.Marshal(fresh)
			got, _ := json.Marshal(up)
			if string(got) != string(want) {
				t.Fatalf("upgraded:\n%s\nwant a fresh bind's:\n%s", got, want)
			}
			if got := firedOn(t, up, "beads.bead.created", post); !slices.Equal(got, []string{WakePersonJoined}) {
				t.Fatalf("after the upgrade the post fires %v, want [%s]", got, WakePersonJoined)
			}
			if _, changed := UpgradeWakes(land, up); changed {
				t.Fatal("an up-to-date steward was upgraded again")
			}
		})
	}
}

func TestUpgradeWakes_LeavesAStewardWithNoTriggersAlone(t *testing.T) {
	// gardenermode's timed mode clears triggers to "", which parses to nil.
	entries, err := scheduler.ParseTriggers("")
	if err != nil {
		t.Fatalf("ParseTriggers: %v", err)
	}
	for _, in := range [][]scheduler.TriggerEntry{entries, {}} {
		if up, changed := UpgradeWakes(land, in); changed || len(up) != 0 {
			t.Fatalf("UpgradeWakes(%v) = %v, %v; want nothing written", in, up, changed)
		}
	}
}

func TestProvisionHook_UpgradeWritesOnlyAnOldSteward(t *testing.T) {
	d := newFarm()
	h := ProvisionHook{Client: getterFarm{d}, Spec: BindSpec{CreatedBy: "gardenio-controller"}}
	trig, err := h.Bind(context.Background(), land, "")
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	d.writes = nil
	if err := h.Upgrade(context.Background(), land, trig); err != nil || len(d.writes) != 0 {
		t.Fatalf("a fresh steward was written: %v err=%v", d.writes, err)
	}
	entries, _ := scheduler.ParseTriggers(trig.Fields["triggers"])
	entries = slices.DeleteFunc(entries, func(e scheduler.TriggerEntry) bool { return e.Label == WakePersonJoined })
	raw, _ := json.Marshal(entries)
	trig.Fields["triggers"] = string(raw)
	if err := h.Upgrade(context.Background(), land, trig); err != nil || len(d.writes) != 1 {
		t.Fatalf("an old steward: writes %v err=%v", d.writes, err)
	}
	up, _ := scheduler.ParseTriggers(trig.Fields["triggers"])
	if !slices.ContainsFunc(up, func(e scheduler.TriggerEntry) bool { return e.Label == WakePersonJoined }) {
		t.Fatalf("upgraded triggers = %s", trig.Fields["triggers"])
	}
}
