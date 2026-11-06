package commons

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/commons/commonstest"
)

// stewardSamples returns the sample count and sum per land of the steward
// response histogram, and each land's cumulative count at le=900.
func stewardSamples(t *testing.T, c prometheus.Collector) (map[string][2]float64, map[string]uint64) {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(c)
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	out, under := map[string][2]float64{}, map[string]uint64{}
	for _, mf := range mfs {
		if mf.GetName() != "gardenio_lands_steward_response_seconds" {
			continue
		}
		for _, m := range mf.GetMetric() {
			land := labelOf(m, "land")
			out[land] = [2]float64{float64(m.GetHistogram().GetSampleCount()), m.GetHistogram().GetSampleSum()}
			for _, b := range m.GetHistogram().GetBucket() {
				if b.GetUpperBound() == 900 {
					under[land] = b.GetCumulativeCount()
				}
			}
		}
	}
	return out, under
}

// stewardLand is a tenant land acme with #general (ac-gen) and #crew (ac-crew),
// a wish head in #general, and another land beta with its own #general.
func stewardLand() *commonstest.Daemon {
	d := &commonstest.Daemon{}
	acme, beta := []string{"project:acme"}, []string{"project:beta"}
	for _, b := range []*beadsapi.BeadDetail{
		{ID: "ac-gen", Type: "channel", Status: "open", Labels: acme, Fields: map[string]string{"name": "general", "channel_kind": "general"}},
		{ID: "ac-crew", Type: "channel", Status: "open", Labels: acme, Fields: map[string]string{"name": "crew", "channel_kind": "crew"}},
		{ID: "ac-wish", Type: "task", Status: "open", Labels: []string{"thread", "project:acme"}, Fields: map[string]string{"channel_id": "ac-gen"}},
		{ID: "bt-gen", Type: "channel", Status: "open", Labels: beta, Fields: map[string]string{"name": "general", "channel_kind": "general"}},
	} {
		d.Put(b)
	}
	return d
}

// msgAt is a message bead of land at minute m: channel and thread may be "".
func msgAt(land, id, author, channel, thread string, m int) *beadsapi.BeadDetail {
	f := map[string]string{"author": author}
	if channel != "" {
		f["channel_id"] = channel
	}
	if thread != "" {
		f["thread_id"] = thread
	}
	return &beadsapi.BeadDetail{ID: id, Type: "message", Status: "closed", Labels: []string{"project:" + land},
		CreatedAt: t0.Add(time.Duration(m) * time.Minute), Fields: f}
}

// One sample per human #general message, at the land steward's first post
// after it at the top level of #general or in the message's thread. Agents'
// messages, other channels, other lands' stewards and a steward post older
// than the message start or answer nothing.
func TestStewardResponse(t *testing.T) {
	d := stewardLand()
	s := newStewardSLI()
	ctx := context.Background()
	steward := "agent:acme-steward-k3x"
	feed := func(land string, b *beadsapi.BeadDetail) {
		d.Put(b)
		s.message(ctx, d, land, b, t0)
	}

	feed("acme", msgAt("acme", "h1", "oidc:ann@acme.example", "ac-gen", "", 0))       // top level
	feed("acme", msgAt("acme", "h2", "oidc:bo@acme.example", "", "ac-wish", 1))       // reply, channel from the head
	feed("acme", msgAt("acme", "h3", "oidc:ann@acme.example", "ac-crew", "", 2))      // not #general
	feed("acme", msgAt("acme", "a1", "agent:acme-lands-gardener-x", "ac-gen", "", 3)) // an agent, not a person
	feed("beta", msgAt("beta", "b1", "oidc:cy@beta.example", "bt-gen", "", 4))
	if len(s.pending) != 3 {
		t.Fatalf("pending %v, want h1 h2 b1", s.pending)
	}

	// Another land's steward and a card answer nothing.
	feed("acme", msgAt("acme", "x1", "agent:beta-steward-q", "ac-gen", "", 5))
	card := msgAt("acme", "c1", steward, "ac-gen", "", 5)
	card.Fields["kind"] = "card"
	feed("acme", card)
	// A reply in h1's thread at minute 10 answers h1 only.
	feed("acme", msgAt("acme", "s1", steward, "ac-gen", "h1", 10))
	// A second answer to h1 is not a second sample.
	feed("acme", msgAt("acme", "s2", steward, "ac-gen", "h1", 11))
	// A reply in the wish thread answers h2, 29 minutes on.
	feed("acme", msgAt("acme", "s3", steward, "", "ac-wish", 30))

	got, under := stewardSamples(t, s.hist)
	if got["acme"] != [2]float64{2, 600 + 29*60} || under["acme"] != 1 {
		t.Errorf("acme samples %v (%d within 900s), want 2 summing 2340, 1 within", got["acme"], under["acme"])
	}
	if _, ok := got["beta"]; ok {
		t.Errorf("beta's message is unanswered, got %v", got["beta"])
	}

	// A steward top-level post answers every waiting message of the channel,
	// replies included, but not one written after it.
	feed("acme", msgAt("acme", "h4", "oidc:ann@acme.example", "ac-gen", "", 40))
	feed("acme", msgAt("acme", "h5", "oidc:bo@acme.example", "ac-gen", "h4", 41))
	feed("acme", msgAt("acme", "h6", "oidc:bo@acme.example", "ac-gen", "", 45))
	feed("acme", msgAt("acme", "s4", steward, "ac-gen", "", 42))
	if got, _ := stewardSamples(t, s.hist); got["acme"][0] != 4 {
		t.Errorf("acme samples %v, want 4", got["acme"])
	}
	if _, ok := s.pending["h6"]; !ok || len(s.pending) != 2 {
		t.Errorf("pending %v, want h6 and b1", s.pending)
	}

	// Silence: past two hours a message is observed at the cap.
	s.expire(t0.Add(119 * time.Minute))
	if len(s.pending) != 2 {
		t.Fatalf("nothing is past the cap yet, pending %v", s.pending)
	}
	s.expire(t0.Add(4*time.Minute + stewardResponseCap))
	got, under = stewardSamples(t, s.hist)
	if got["beta"] != [2]float64{1, 7200} || under["beta"] != 0 {
		t.Errorf("beta samples %v, want one at 7200", got["beta"])
	}
	s.expire(t0.Add(45*time.Minute + stewardResponseCap))
	if got, _ := stewardSamples(t, s.hist); got["acme"][0] != 5 || len(s.pending) != 0 {
		t.Errorf("acme samples %v pending %v, want 5 and none", got["acme"], s.pending)
	}
}

// The hub feeds only creation events of messages to the SLI.
func TestHubObservesStewardResponse(t *testing.T) {
	d := stewardLand()
	src := make(liveSource, 16)
	hub := NewHub(src.open, d, nil)
	hub.now = func() time.Time { return t0.Add(time.Hour) }
	hub.Start()
	defer hub.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	admin, _ := hub.Subscribe(ctx, adminScope)

	send := func(topic string, b *beadsapi.BeadDetail) {
		d.Put(b)
		src <- beadsapi.SSEEvent{Event: topic, Data: commonstest.Payload(b)}
		select {
		case <-admin:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s not delivered", b.ID)
		}
	}
	send("beads.bead.created", msgAt("acme", "h1", "oidc:ann@acme.example", "ac-gen", "", 0))
	send("beads.bead.updated", msgAt("acme", "h2", "oidc:ann@acme.example", "ac-gen", "", 1))
	send("beads.bead.created", msgAt("acme", "s1", "agent:acme-steward-k3x", "ac-gen", "", 5))
	// The worker feeds the SLI after the broadcast: one more event through
	// means s1 is done.
	send("beads.bead.updated", msgAt("acme", "h2", "oidc:ann@acme.example", "ac-gen", "", 1))

	got, _ := stewardSamples(t, hub.StewardResponseCollector())
	if got["acme"] != [2]float64{1, 300} {
		t.Errorf("acme samples %v, want one at 300", got["acme"])
	}
	var nilHub *Hub
	if nilHub.StewardResponseCollector() != nil {
		t.Error("a nil hub has no collector")
	}
}
