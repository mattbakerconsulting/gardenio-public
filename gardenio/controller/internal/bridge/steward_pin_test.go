package bridge_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
	"gardenio/controller/internal/scheduler"
	"gardenio/controller/internal/steward"
)

// stewardFarm is a tenant land with its #general, and what Bind writes.
type stewardFarm struct {
	beads   []*beadsapi.BeadDetail
	created []beadsapi.CreateBeadRequest
}

func (d *stewardFarm) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	var out []*beadsapi.BeadDetail
	if q.Offset > 0 {
		return &beadsapi.ListBeadsResult{}, nil
	}
	for _, b := range d.beads {
		if (len(q.Types) == 0 || slices.Contains(q.Types, b.Type)) &&
			(len(q.Statuses) == 0 || slices.Contains(q.Statuses, b.Status)) &&
			!slices.ContainsFunc(q.Labels, func(l string) bool { return !slices.Contains(b.Labels, l) }) {
			out = append(out, b)
		}
	}
	return &beadsapi.ListBeadsResult{Beads: out, Total: len(out)}, nil
}

func (d *stewardFarm) CreateBead(_ context.Context, req beadsapi.CreateBeadRequest) (string, error) {
	d.created = append(d.created, req)
	id := fmt.Sprintf("tp-%s-%d", req.Type, len(d.created))
	d.beads = append(d.beads, &beadsapi.BeadDetail{ID: id, Type: req.Type, Title: req.Title, Status: "open", Labels: req.Labels})
	return id, nil
}

// TestSteward_PastItsWallClockIsRenewedNotClosed pins gd-UgZueN6Nk7: a land's
// steward, exactly as steward.Bind writes it, evaluated 721h after it was
// made (past the bridge.GardenerMaxWallClock hard_stop the bounded-stop guard
// requires), is renewed in place and not closed. Bind marks it standing twice
// over (Standing in the blob, perpetual:true on the labels); a change that
// loses both would close every land's steward after 30 days.
func TestSteward_PastItsWallClockIsRenewedNotClosed(t *testing.T) {
	bead := boundStewardBead(t, "tomato-patch")
	now := time.Now()
	created := now.Add(-(bridge.GardenerMaxWallClock + time.Hour))
	scheduler.SetWallClockGrandfatherEpochForTest(t, created.Add(-time.Hour))
	bead.CreatedAt = created

	closed, written := bridge.EvaluateTriggerBeadForTest(t, bead)
	if closed {
		t.Fatalf("the engine closed a steward %s after it was bound; a standing steward is renewed in place", now.Sub(created))
	}
	if written == nil || written.WallClockSince.Before(now) {
		t.Fatalf("gardener write %+v: want the wall clock renewed from now", written)
	}
}

// TestSteward_NotGatedBySchedulesOff pins gd-gCN4m4DYxc's exception: gardener
// triggers honour `/schedule off <project>` whatever source loads them, but a
// land's steward, exactly as steward.Bind writes it, does not. Tenant lands are
// provisioned with schedules_enabled=false, so a steward gated by it would go
// quiet in every such land; it was never gated before event-mode plan
// gardeners, and its own controls are the land freeze and lands-pause.
func TestSteward_NotGatedBySchedulesOff(t *testing.T) {
	bead := boundStewardBead(t, "tomato-patch")
	trig := bridge.ParseTriggerDef(bead, "multi")
	if trig.Gardener == nil {
		t.Fatal("steward.Bind wrote no gardener block; the steward exception would be untested")
	}
	if bridge.SkippedBySchedulesOffForTest("multi", trig) {
		t.Errorf("a steward is skipped when its land's schedules are off (labels %v); it must keep firing", trig.Labels)
	}
	// The same gardener without the steward marker is a plain gardener and IS
	// gated, so the assertion above is about the marker and not the source.
	trig.Labels = slices.DeleteFunc(slices.Clone(trig.Labels), func(l string) bool { return l == steward.MarkerLabel })
	if !bridge.SkippedBySchedulesOffForTest("multi", trig) {
		t.Error("a trigger:multi gardener without the steward marker is not gated by /schedule off <project>")
	}
}

// boundStewardBead binds a steward on a fresh tenant land with steward.Bind and
// returns its trigger bead as the daemon hands it back (every field a string).
func boundStewardBead(t *testing.T, land string) *beadsapi.BeadDetail {
	t.Helper()
	d := &stewardFarm{beads: []*beadsapi.BeadDetail{
		{ID: "tp-proj", Type: "project", Title: land, Status: "open", Labels: []string{"project:" + land}, Fields: map[string]string{
			"land_kind": "tenant", "git_url": "https://github.com/ada/tomatoes.git", "land_default_channel": "tp-general",
		}},
		{ID: "tp-general", Type: "channel", Title: "#general", Status: "open", Labels: []string{"project:" + land}, Fields: map[string]string{"name": "general"}},
	}}
	res, err := steward.Bind(context.Background(), d, land, steward.BindSpec{CreatedBy: "gardenio-controller"})
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	i := slices.IndexFunc(d.created, func(r beadsapi.CreateBeadRequest) bool { return r.Type == "trigger" })
	if i < 0 {
		t.Fatalf("Bind wrote no trigger: %+v", d.created)
	}
	req := d.created[i]
	var raw map[string]any
	if err := json.Unmarshal(req.Fields, &raw); err != nil {
		t.Fatal(err)
	}
	// The daemon hands every field back as a string.
	fields := map[string]string{}
	for k, v := range raw {
		if s, ok := v.(string); ok {
			fields[k] = s
		} else {
			fields[k] = fmt.Sprint(v)
		}
	}

	return &beadsapi.BeadDetail{
		ID: res.ID, Type: "trigger", Title: req.Title, Status: "open",
		Labels: req.Labels, Fields: fields, CreatedAt: time.Now(), CreatedBy: req.CreatedBy,
	}
}
