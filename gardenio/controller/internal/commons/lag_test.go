package commons

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/commons/commonstest"
)

// stamped is commonstest.Payload with the bead's updated_at set, as kbeads
// sends it.
func stamped(t *testing.T, b *beadsapi.BeadDetail, updatedAt string) json.RawMessage {
	t.Helper()
	var p map[string]map[string]any
	if err := json.Unmarshal(commonstest.Payload(b), &p); err != nil {
		t.Fatal(err)
	}
	p["bead"]["updated_at"] = updatedAt
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// lagSamples returns sample count and sum per land/kind of the hub's histogram.
func lagSamples(t *testing.T, hub *Hub) map[string][2]float64 {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(hub.LagCollector())
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][2]float64{}
	for _, mf := range mfs {
		if mf.GetName() != "gardenio_commons_event_lag_seconds" {
			continue
		}
		for _, m := range mf.GetMetric() {
			out[labelOf(m, "land")+"/"+labelOf(m, "kind")] = [2]float64{float64(m.GetHistogram().GetSampleCount()), m.GetHistogram().GetSampleSum()}
		}
	}
	return out
}

func labelOf(m *dto.Metric, name string) string {
	for _, l := range m.GetLabel() {
		if l.GetName() == name {
			return l.GetValue()
		}
	}
	return ""
}

// The Commons event latency SLI (Lands Track B §5.8): one sample per event,
// write to broadcast, by land and kind. A bad or missing stamp never costs
// the event, and a clock behind kbeads' reads as zero.
func TestHubObservesEventLag(t *testing.T) {
	d := commonstest.TwoLands(t0)
	now := t0.Add(time.Hour)
	src := make(liveSource, 16)
	hub := NewHub(src.open, d, nil)
	hub.now = func() time.Time { return now }
	hub.Start()
	defer hub.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	admin, _ := hub.Subscribe(ctx, adminScope)

	send := func(id, updatedAt string) {
		src <- beadsapi.SSEEvent{Event: "beads.bead.updated", Data: stamped(t, d.Beads[id], updatedAt)}
	}
	send("gd-msg1", now.Add(-3*time.Second).Format(time.RFC3339Nano))
	send("cl-msg1", now.Add(-500*time.Millisecond).Format(time.RFC3339Nano))
	send("cl-card1", now.Add(time.Second).Format(time.RFC3339Nano)) // kbeads' clock ahead
	send("cl-rep1", "not a time")
	send("gd-chgen", "")

	for i := 0; i < 5; i++ {
		select {
		case <-admin:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d of 5 events delivered", i)
		}
	}
	got := lagSamples(t, hub)
	want := map[string][2]float64{
		"gardenio/message":   {1, 3},
		"claimlayer/message": {1, 0.5},
		"claimlayer/card":    {1, 0},
	}
	if len(got) != len(want) {
		t.Errorf("samples %v, want %v", got, want)
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %v, want %v", k, got[k], w)
		}
	}
}

func TestClassifyWrittenStamp(t *testing.T) {
	d := commonstest.TwoLands(t0)
	at := t0.Add(5 * time.Minute)
	inv, ok := classify(stamped(t, d.Beads["cl-msg1"], at.Format(time.RFC3339Nano)))
	if !ok || !inv.written.Equal(at) {
		t.Errorf("classify kept written=%v ok=%v, want %v", inv.written, ok, at)
	}
	// A card's subject carries no write time of its own: its cards are not
	// new writes.
	if inv, _ := classify(stamped(t, d.Beads["cl-agent1"], at.Format(time.RFC3339Nano))); !inv.written.IsZero() {
		t.Errorf("a subject invalidation must carry no write time, got %v", inv.written)
	}
	var nilHub *Hub
	if nilHub.LagCollector() != nil {
		t.Error("a nil hub has no collector")
	}
}
