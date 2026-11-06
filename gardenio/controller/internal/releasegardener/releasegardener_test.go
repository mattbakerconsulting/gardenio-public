package releasegardener

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
	"gardenio/controller/internal/scheduler"
)

// fakeDaemon answers the trigger and advice lists from beads, records creates
// and field writes.
type fakeDaemon struct {
	beads   []*beadsapi.BeadDetail
	created []beadsapi.CreateBeadRequest
	updates map[string]map[string]any
}

func (f *fakeDaemon) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	out := &beadsapi.ListBeadsResult{}
	for _, b := range f.beads {
		if !slices.Contains(q.Types, b.Type) {
			continue
		}
		if !slices.ContainsFunc(q.Labels, func(l string) bool { return !slices.Contains(b.Labels, l) }) {
			out.Beads = append(out.Beads, b)
		}
	}
	return out, nil
}

func (f *fakeDaemon) CreateBead(_ context.Context, req beadsapi.CreateBeadRequest) (string, error) {
	f.created = append(f.created, req)
	return "gd-trig", nil
}

func (f *fakeDaemon) UpdateBeadFieldsTyped(_ context.Context, id string, fields map[string]any) error {
	if f.updates == nil {
		f.updates = map[string]map[string]any{}
	}
	f.updates[id] = fields
	return nil
}

func playbook() *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{ID: "gd-advice", Type: "advice", Labels: []string{PlaybookLabel}}
}

func openRelease() *beadsapi.Release {
	return &beadsapi.Release{ID: "gd-Rel1", Project: "gardenio", Status: "open"}
}

func TestEnsureCreatesBoundedGardener(t *testing.T) {
	d := &fakeDaemon{beads: []*beadsapi.BeadDetail{playbook()}}
	res, err := Ensure(context.Background(), d, openRelease(), Spec{CreatedBy: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	if res.ID != "gd-trig" || res.Adopted || !res.Named {
		t.Fatalf("result %+v, want a created gardener the release names", res)
	}
	if got := d.updates["gd-Rel1"][ReleaseField]; got != "gd-trig" {
		t.Errorf("release %s = %v, want gd-trig", ReleaseField, got)
	}
	if len(d.created) != 1 {
		t.Fatalf("%d creates, want 1", len(d.created))
	}
	req := d.created[0]
	for _, l := range []string{"role:gardener", MarkerLabel, ReleaseLabel("gd-Rel1"), "project:gardenio", "trigger:multi"} {
		if !slices.Contains(req.Labels, l) {
			t.Errorf("labels %v lack %s", req.Labels, l)
		}
	}
	var f map[string]any
	if err := json.Unmarshal(req.Fields, &f); err != nil {
		t.Fatal(err)
	}
	if f["role"] != SpawnRole || f["project"] != "gardenio" || f["enabled"] != true {
		t.Errorf("role %v project %v enabled %v", f["role"], f["project"], f["enabled"])
	}
	if p, _ := f["prompt"].(string); !strings.Contains(p, "gb advice show gd-advice") || !strings.Contains(p, "gd-Rel1") {
		t.Errorf("prompt does not point at the playbook for the release:\n%s", p)
	}
	blob, _ := f["gardener"].(string)
	if v := bridge.ValidateGardenerBlob(blob); v != nil {
		t.Errorf("blob fails the bounds guard: %v", v)
	}
	var spec bridge.GardenerSpec
	if err := json.Unmarshal([]byte(blob), &spec); err != nil || spec.AssignBead != "gd-Rel1" || !spec.OneSpawnAtATime {
		t.Errorf("blob %s: assign_bead or one_spawn_at_a_time wrong (%v)", blob, err)
	}
	entries, err := scheduler.ParseTriggers(f["triggers"].(string))
	if err != nil || len(entries) != 2 {
		t.Fatalf("triggers %v (%v), want the release entry and the cut windows", entries, err)
	}
	if entries[1].Cron != CutWindowCron || entries[1].Timezone != "UTC" {
		t.Errorf("cut window entry %+v", entries[1])
	}
}

func TestEnsureAdoptsPinnedGardener(t *testing.T) {
	pinned := &beadsapi.BeadDetail{ID: "gd-old", Type: "trigger", Labels: []string{ReleaseLabel("gd-Rel1")}}
	d := &fakeDaemon{beads: []*beadsapi.BeadDetail{playbook(), pinned}}
	rel := openRelease()
	rel.GardenerID = "gd-old"
	res, err := Ensure(context.Background(), d, rel, Spec{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Adopted || res.ID != "gd-old" || res.Named || len(d.created) != 0 || len(d.updates) != 0 {
		t.Errorf("result %+v, %d creates, %d updates: want the pinned gardener and no write", res, len(d.created), len(d.updates))
	}
}

func TestEnsureRefuses(t *testing.T) {
	live := openRelease()
	live.Status = "live"
	if _, err := Ensure(context.Background(), &fakeDaemon{beads: []*beadsapi.BeadDetail{playbook()}}, live, Spec{}); err == nil {
		t.Error("a live release got a gardener")
	}
	d := &fakeDaemon{}
	if _, err := Ensure(context.Background(), d, openRelease(), Spec{}); err == nil || len(d.created) != 0 {
		t.Errorf("no playbook advice: err %v, %d creates; want a refusal and no trigger", err, len(d.created))
	}
}
