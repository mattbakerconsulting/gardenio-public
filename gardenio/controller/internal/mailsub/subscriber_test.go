package mailsub

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/coopapi"
	"gardenio/controller/internal/mailproto"
)

// TestMain shrinks the tunables once, before any subscriber goroutine runs,
// so no test writes a package variable another test's goroutine is reading.
func TestMain(m *testing.M) {
	DeliverRetryDelay = time.Millisecond
	InitialBackoff = time.Millisecond
	MaxBackoff = 5 * time.Millisecond
	RedrainInitial = time.Millisecond
	RedrainMax = 5 * time.Millisecond
	os.Exit(m.Run())
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// fakeDaemon hands out streams[i] on the i-th subscribe (the last one again
// once they run out) and records every subscribe's agent scope and cursor.
type fakeDaemon struct {
	mu   sync.Mutex
	open []*beadsapi.BeadDetail
	// closedKills answers the closed control:kill query; closedQueries
	// records each one.
	closedKills   []*beadsapi.BeadDetail
	closedQueries []beadsapi.ListBeadsQuery
	streams       []chan beadsapi.SSEEvent
	subs          int
	agent         string
	cursors       []string
	closed        []string
	// closeFields is the fields map of each CloseBead call, by mail id.
	closeFields map[string]map[string]string
	// updated is the fields map of each UpdateBeadFields call, by mail id.
	updated map[string]map[string]string
	// reassigned is the new assignee of each Reassign call, by mail id;
	// reassignErr fails every Reassign.
	reassigned  map[string]string
	reassignErr error
}

func newFakeDaemon(streams int) *fakeDaemon {
	d := &fakeDaemon{}
	for range streams {
		d.streams = append(d.streams, make(chan beadsapi.SSEEvent, 8))
	}
	return d
}

func (f *fakeDaemon) EventStreamScoped(_ context.Context, _, agentBead, lastEventID string) (<-chan beadsapi.SSEEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.agent = agentBead
	f.cursors = append(f.cursors, lastEventID)
	ch := f.streams[min(f.subs, len(f.streams)-1)]
	f.subs++
	return ch, nil
}

func (f *fakeDaemon) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	src := f.open
	if slices.Contains(q.Statuses, "closed") {
		f.closedQueries = append(f.closedQueries, q)
		src = f.closedKills
	}
	var out []*beadsapi.BeadDetail
	for _, b := range src {
		if b.Assignee == q.Assignee {
			out = append(out, b)
		}
	}
	return &beadsapi.ListBeadsResult{Beads: out}, nil
}

func (f *fakeDaemon) GetBead(_ context.Context, id string) (*beadsapi.BeadDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, b := range f.open {
		if b.ID == id {
			return b, nil
		}
	}
	return nil, nil
}

func (f *fakeDaemon) UpdateBeadFields(_ context.Context, id string, fields map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.updated == nil {
		f.updated = map[string]map[string]string{}
	}
	f.updated[id] = fields
	return nil
}

func (f *fakeDaemon) CloseBead(_ context.Context, id string, fields map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = append(f.closed, id)
	if f.closeFields == nil {
		f.closeFields = map[string]map[string]string{}
	}
	f.closeFields[id] = fields
	return nil
}

func (f *fakeDaemon) Reassign(_ context.Context, id, assignee string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reassignErr != nil {
		return f.reassignErr
	}
	if f.reassigned == nil {
		f.reassigned = map[string]string{}
	}
	f.reassigned[id] = assignee
	for _, b := range f.open {
		if b.ID == id {
			b.Assignee = assignee
		}
	}
	return nil
}

type fakeCoop struct {
	mu       sync.Mutex
	nudges   []string
	calls    int
	failures int
	// refusals answers that many nudges delivered=false, as coop does for
	// an agent in a prompt.
	refusals int
	shutdown bool
}

func (c *fakeCoop) Nudge(_ context.Context, msg string) (*coopapi.NudgeResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.failures > 0 {
		c.failures--
		return nil, context.DeadlineExceeded
	}
	if c.refusals > 0 {
		c.refusals--
		return &coopapi.NudgeResponse{Reason: "agent is prompt"}, nil
	}
	c.nudges = append(c.nudges, msg)
	return &coopapi.NudgeResponse{Delivered: true}, nil
}

func (c *fakeCoop) Shutdown(_ context.Context) error {
	c.mu.Lock()
	c.shutdown = true
	c.mu.Unlock()
	return nil
}

func (c *fakeCoop) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.nudges)
}

func mailEvent(id, assignee string, labels ...string) beadsapi.SSEEvent {
	b := map[string]any{"id": id, "type": "mail", "title": "hello", "assignee": assignee, "labels": labels}
	data, _ := json.Marshal(map[string]any{"bead": b})
	return beadsapi.SSEEvent{ID: id, Event: "beads.bead.created", Data: data}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

func start(t *testing.T, cfg Config) {
	t.Helper()
	if cfg.Logger == nil {
		cfg.Logger = quiet
	}
	cfg.AgentBead, cfg.AgentName = "gd-agent1", "local-laptop"
	s := New(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = s.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
}

func TestFormatNudge_InterruptInlinesBody(t *testing.T) {
	m := beadsapi.BeadDetail{ID: "gd-m1", Title: "Decision answered", Description: "Option B", Labels: []string{"from:slack-bridge", "delivery:interrupt"}}
	if got, want := FormatNudge(m), "Mail from slack-bridge [gd-m1]: Decision answered\n\nOption B"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	plain := FormatNudge(beadsapi.BeadDetail{ID: "gd-m2", Title: "FYI", Labels: []string{"from:captain"}})
	if plain != "New mail from captain: FYI — run 'kd show gd-m2' to read" {
		t.Fatalf("plain nudge wrong: %q", plain)
	}
	if got := FormatNudge(beadsapi.BeadDetail{ID: "gd-m3", Title: "x"}); got != "New mail from unknown: x — run 'kd show gd-m3' to read" {
		t.Fatalf("senderless nudge wrong: %q", got)
	}
}

func TestSubscriber_DeliversOwnMailOnly(t *testing.T) {
	d := newFakeDaemon(1)
	c := &fakeCoop{}
	start(t, Config{Daemon: d, Coop: c})

	d.streams[0] <- mailEvent("gd-m2", "crew-other", "from:bridge")
	d.streams[0] <- beadsapi.SSEEvent{ID: "7", Data: json.RawMessage(`{"bead":{"id":"gd-t1","type":"task","assignee":"local-laptop"}}`)}
	d.streams[0] <- mailEvent("gd-m1", "local-laptop", "from:bridge")
	waitFor(t, func() bool { return c.count() == 1 })
	time.Sleep(20 * time.Millisecond)
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.nudges) != 1 || c.nudges[0] != "New mail from bridge: hello — run 'kd show gd-m1' to read" {
		t.Fatalf("nudges = %q, want only gd-m1's", c.nudges)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.agent != "gd-agent1" {
		t.Fatalf("stream not scoped to own bead: %q", d.agent)
	}
}

// The daemon's fields are any JSON. A streamed mail with a non-string field
// is still delivered from the stream: decoding it into BeadDetail's string
// map used to fail the whole event, which was then dropped without a log line
// (gd-D8jkm7eyEO).
func TestSubscriber_StreamedMailWithNonStringFieldIsDelivered(t *testing.T) {
	d := newFakeDaemon(1)
	c := &fakeCoop{}
	start(t, Config{Daemon: d, Coop: c})

	d.streams[0] <- beadsapi.SSEEvent{ID: "3", Data: json.RawMessage(
		`{"bead":{"id":"gd-m1","type":"mail","title":"hello","assignee":"local-laptop","labels":["from:bridge"],"fields":{"escalation_id":"gd-e1","attempts":2,"spec":{"a":1}}}}`)}
	waitFor(t, func() bool { return c.count() == 1 })
	waitFor(t, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.updated["gd-m1"][mailproto.FieldDeliveredAt] != ""
	})
}

// The stream is scoped to this agent, so any mail event it hands over that
// the subscriber cannot deliver itself (undecodable, or naming an assignee
// other than AgentName) is followed by a drain, which reads the mail over
// REST (gd-D8jkm7eyEO).
func TestSubscriber_UndeliverableStreamEventDrains(t *testing.T) {
	for name, evt := range map[string]beadsapi.SSEEvent{
		"undecodable":    {ID: "5", Data: json.RawMessage(`{"bead":{"id":"gd-m1","priority":"high"}}`)},
		"other assignee": mailEvent("gd-m1", "Local-Laptop", "from:bridge"),
	} {
		t.Run(name, func(t *testing.T) {
			d := newFakeDaemon(1)
			c := &fakeCoop{}
			start(t, Config{Daemon: d, Coop: c})
			waitFor(t, func() bool {
				d.mu.Lock()
				defer d.mu.Unlock()
				return d.subs == 1
			})
			time.Sleep(20 * time.Millisecond) // the connect drain ran on an empty inbox

			d.mu.Lock()
			d.open = []*beadsapi.BeadDetail{{ID: "gd-m1", Type: "mail", Title: "hello", Assignee: "local-laptop", Status: "open", Labels: []string{"from:bridge"}}}
			d.mu.Unlock()
			d.streams[0] <- evt
			waitFor(t, func() bool { return c.count() == 1 })
		})
	}
}

// Mail that arrived while disconnected (and fell out of the replay ring) is
// delivered exactly once by the connect drain, even when the same mail then
// also arrives live.
func TestSubscriber_DrainOnConnect_DeliversOnceByID(t *testing.T) {
	d := newFakeDaemon(1)
	d.open = []*beadsapi.BeadDetail{{ID: "gd-old", Type: "mail", Title: "missed", Assignee: "local-laptop", Status: "open", Labels: []string{"from:captain"}}}
	c := &fakeCoop{}
	start(t, Config{Daemon: d, Coop: c})

	waitFor(t, func() bool { return c.count() == 1 })
	d.streams[0] <- mailEvent("gd-old", "local-laptop", "from:captain")
	time.Sleep(20 * time.Millisecond)
	if c.count() != 1 {
		t.Fatalf("mail delivered twice: %v", c.nudges)
	}
}

// A dropped stream is reopened with the last seen event id, and the reconnect
// drain does not redeliver what the first connection already delivered.
func TestSubscriber_ReconnectsWithLastEventID(t *testing.T) {
	d := newFakeDaemon(2)
	c := &fakeCoop{}
	start(t, Config{Daemon: d, Coop: c})

	evt := mailEvent("gd-m1", "local-laptop", "from:bridge")
	evt.ID = "41"
	d.mu.Lock()
	d.open = []*beadsapi.BeadDetail{{ID: "gd-m1", Type: "mail", Title: "hello", Assignee: "local-laptop", Status: "open"}}
	d.mu.Unlock()
	d.streams[0] <- evt
	waitFor(t, func() bool { return c.count() == 1 })
	close(d.streams[0])

	waitFor(t, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return len(d.cursors) == 2
	})
	d.streams[1] <- mailEvent("gd-m2", "local-laptop", "from:bridge")
	waitFor(t, func() bool { return c.count() == 2 })
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cursors[0] != "" || d.cursors[1] != "41" {
		t.Fatalf("cursors = %q, want [\"\" \"41\"]", d.cursors)
	}
}

func TestSubscriber_RetriesThenSucceeds(t *testing.T) {
	d := newFakeDaemon(1)
	c := &fakeCoop{failures: 2}
	start(t, Config{Daemon: d, Coop: c})

	d.streams[0] <- mailEvent("gd-m1", "local-laptop", "from:bridge")
	waitFor(t, func() bool { return c.count() == 1 })
}

func TestDeliver_GivesUpAfterThreeAttemptsAndLeavesOpen(t *testing.T) {
	d := newFakeDaemon(1)
	c := &fakeCoop{failures: 3}
	s := New(Config{Daemon: d, Coop: c, AgentBead: "gd-agent1", AgentName: "local-laptop", Logger: quiet})
	m := beadsapi.BeadDetail{ID: "gd-m3", Type: "mail", Assignee: "local-laptop", Title: "x"}
	if err := s.deliver(context.Background(), m); err == nil {
		t.Fatal("expected delivery error after 3 failed attempts")
	}
	if c.calls != DeliverAttempts || c.count() != 0 {
		t.Fatalf("calls=%d delivered=%d, want %d attempts and none delivered", c.calls, c.count(), DeliverAttempts)
	}
	if len(d.closed) != 0 {
		t.Fatalf("undelivered mail was closed: %v", d.closed)
	}
	if len(d.updated) != 0 {
		t.Fatalf("undelivered mail was stamped: %v", d.updated)
	}
	// Not marked delivered: the next drain tries again.
	if err := s.deliver(context.Background(), m); err != nil || c.count() != 1 {
		t.Fatalf("retry on next drain: err=%v delivered=%d", err, c.count())
	}
}

// Mail that reaches the pod before its coop is up (agent still spawning, or
// coop restarting inside `gb init`) outlasts deliver's three tries. It is
// re-drained until coop comes up, then delivered and stamped, so the bridge
// does not respawn a healthy pod over it.
func TestSubscriber_CoopUpAfterRetryWindow_ConnectDrainRedelivers(t *testing.T) {
	d := newFakeDaemon(1)
	d.open = []*beadsapi.BeadDetail{{ID: "gd-m1", Type: "mail", Title: "hello", Assignee: "local-laptop", Status: "open"}}
	c := &fakeCoop{failures: 3*DeliverAttempts + 1}
	start(t, Config{Daemon: d, Coop: c})

	waitFor(t, func() bool { return c.count() == 1 })
	waitFor(t, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.updated["gd-m1"][mailproto.FieldDeliveredAt] != ""
	})
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.calls != 3*DeliverAttempts+2 {
		t.Fatalf("coop calls = %d, want %d (three failed drains, one failed try, then delivered)", c.calls, 3*DeliverAttempts+2)
	}
}

func TestSubscriber_CoopUpAfterRetryWindow_LiveMailRedelivered(t *testing.T) {
	d := newFakeDaemon(1)
	c := &fakeCoop{}
	start(t, Config{Daemon: d, Coop: c})
	// Let the connect drain finish on an empty inbox before coop goes down.
	waitFor(t, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return len(d.closedQueries) == 1
	})

	c.mu.Lock()
	c.failures = 2*DeliverAttempts + 1
	c.mu.Unlock()
	d.mu.Lock()
	d.open = []*beadsapi.BeadDetail{{ID: "gd-m2", Type: "mail", Title: "hello", Assignee: "local-laptop", Status: "open"}}
	d.mu.Unlock()
	d.streams[0] <- mailEvent("gd-m2", "local-laptop", "from:slack-bridge")

	waitFor(t, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.updated["gd-m2"][mailproto.FieldDeliveredAt] != ""
	})
	// Delivered once; later drains skip it.
	time.Sleep(20 * time.Millisecond)
	if c.count() != 1 {
		t.Fatalf("mail delivered %d times, want once", c.count())
	}
}

// Mail coop took is stamped delivered_at (it stays open until the agent reads
// it), so the bridge can tell it from mail no pod is there to take; control
// mail and mail already stamped are not.
func TestDeliver_StampsDeliveredAt(t *testing.T) {
	d := newFakeDaemon(1)
	c := &fakeCoop{}
	s := New(Config{Daemon: d, Coop: c, AgentBead: "gd-agent1", AgentName: "local-laptop", Logger: quiet})
	ctx := context.Background()
	if err := s.deliver(ctx, beadsapi.BeadDetail{ID: "gd-m1", Type: "mail", Assignee: "local-laptop", Title: "x"}); err != nil {
		t.Fatal(err)
	}
	stamped := d.updated["gd-m1"][mailproto.FieldDeliveredAt]
	if _, err := time.Parse(time.RFC3339, stamped); err != nil {
		t.Fatalf("delivered_at = %q, want an RFC 3339 time", stamped)
	}
	_ = s.deliver(ctx, beadsapi.BeadDetail{ID: "gd-m2", Type: "mail", Assignee: "local-laptop", Title: "y",
		Fields: map[string]string{mailproto.FieldDeliveredAt: "2026-10-29T00:00:00Z"}})
	_ = s.deliver(ctx, beadsapi.BeadDetail{ID: "gd-c1", Type: "mail", Assignee: "local-laptop", Title: "z",
		Labels: []string{"control:escalation-approved"}})
	if _, ok := d.updated["gd-m2"]; ok {
		t.Fatal("already-stamped mail was stamped again")
	}
	if _, ok := d.updated["gd-c1"]; ok {
		t.Fatal("control mail was stamped")
	}
}

// A nudge coop answers delivered=false (agent in a prompt) is not a delivery:
// the mail is neither marked nor stamped, so the next drain offers it again
// (gd-inbwLZfEYe).
func TestDeliver_RefusedLeftUndeliveredThenAccepted(t *testing.T) {
	d := newFakeDaemon(1)
	c := &fakeCoop{refusals: 1}
	s := New(Config{Daemon: d, Coop: c, AgentBead: "gd-agent1", AgentName: "local-laptop", Logger: quiet})
	m := beadsapi.BeadDetail{ID: "gd-m1", Type: "mail", Assignee: "local-laptop", Title: "x"}
	if err := s.deliver(context.Background(), m); !errors.Is(err, errRefused) {
		t.Fatalf("refused nudge: err = %v, want errRefused", err)
	}
	if c.calls != 1 || len(d.updated) != 0 {
		t.Fatalf("refused nudge: calls=%d stamped=%v, want one call and no stamp", c.calls, d.updated)
	}
	if err := s.deliver(context.Background(), m); err != nil {
		t.Fatalf("accepted nudge: %v", err)
	}
	if c.count() != 1 || d.updated["gd-m1"][mailproto.FieldDeliveredAt] == "" {
		t.Fatalf("accepted nudge: delivered=%d stamp=%v, want delivered and stamped", c.count(), d.updated)
	}
}

func TestSubscriber_RefusedMailRedrainedUntilAccepted(t *testing.T) {
	d := newFakeDaemon(1)
	d.open = []*beadsapi.BeadDetail{{ID: "gd-m1", Type: "mail", Title: "hello", Assignee: "local-laptop", Status: "open"}}
	c := &fakeCoop{refusals: 3}
	start(t, Config{Daemon: d, Coop: c})

	waitFor(t, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.updated["gd-m1"][mailproto.FieldDeliveredAt] != ""
	})
	time.Sleep(20 * time.Millisecond)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.calls != 4 || len(c.nudges) != 1 {
		t.Fatalf("calls=%d delivered=%d, want three refusals then one delivery", c.calls, len(c.nudges))
	}
}

func TestSubscriber_ControlKill_ClosesMailShutsDownCoopThenExits(t *testing.T) {
	d := newFakeDaemon(1)
	c := &fakeCoop{}
	exited := make(chan struct{}, 1)
	start(t, Config{Daemon: d, Coop: c, Exit: func() { exited <- struct{}{} }})

	d.streams[0] <- mailEvent("gd-k1", "local-laptop", "from:slack-bridge", "control:kill")
	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		t.Fatal("Exit not called after control:kill")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.shutdown {
		t.Fatal("coop.Shutdown not called")
	}
	if len(c.nudges) != 0 {
		t.Fatalf("kill must not be delivered as a nudge: %v", c.nudges)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.closed) != 1 || d.closed[0] != "gd-k1" {
		t.Fatalf("closed = %v, want the kill mail closed", d.closed)
	}
	// The daemon refuses the agent's token a close of control mail that does
	// not record the kill was acted on.
	if ts := d.closeFields["gd-k1"][mailproto.FieldControlActed]; ts == "" {
		t.Fatalf("kill mail closed without %s: %v", mailproto.FieldControlActed, d.closeFields["gd-k1"])
	} else if _, err := time.Parse(time.RFC3339, ts); err != nil {
		t.Fatalf("%s = %q, want RFC 3339: %v", mailproto.FieldControlActed, ts, err)
	}
}

func TestSubscriber_OtherControlGoesToOnControl(t *testing.T) {
	d := newFakeDaemon(1)
	c := &fakeCoop{}
	got := make(chan string, 1)
	start(t, Config{Daemon: d, Coop: c, OnControl: func(_ context.Context, verb string, m beadsapi.BeadDetail) {
		got <- verb + " " + m.ID
	}})

	d.streams[0] <- mailEvent("gd-e1", "local-laptop", "from:daemon", "control:escalation-approved")
	select {
	case v := <-got:
		if v != "escalation-approved gd-e1" {
			t.Fatalf("OnControl got %q", v)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnControl not called")
	}
	if c.count() != 0 {
		t.Fatalf("control mail must not be nudged: %v", c.nudges)
	}
}

// The session can close its own kill mail, stamping control_acted, before a
// drain reads it. The next drain still finds it among the closed kill mail
// and stops the agent, and leaves the close as it is.
func TestSubscriber_ControlKill_ClosedByOtherIsStillHonoured(t *testing.T) {
	d := newFakeDaemon(2)
	c := &fakeCoop{}
	exited := make(chan struct{}, 1)
	start(t, Config{Daemon: d, Coop: c, Exit: func() { exited <- struct{}{} }})

	waitFor(t, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return len(d.closedQueries) == 1
	})
	d.mu.Lock()
	d.closedKills = []*beadsapi.BeadDetail{{ID: "gd-k2", Type: "mail", Title: "Stop", Assignee: "local-laptop", Status: "closed", Labels: []string{"from:slack-bridge", "control:kill"}, Fields: map[string]string{mailproto.FieldControlActed: "2026-10-29T00:00:00Z"}}}
	d.mu.Unlock()
	close(d.streams[0])

	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		t.Fatal("Exit not called for kill mail the session closed")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.shutdown {
		t.Fatal("coop.Shutdown not called")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.closed) != 0 {
		t.Fatalf("already-closed kill mail closed again: %v", d.closed)
	}
	q := d.closedQueries[0]
	if !slices.Equal(q.Labels, []string{"control:kill"}) || q.Assignee != "local-laptop" || q.Sort != "-created_at" || !slices.Equal(q.Types, []string{"mail"}) {
		t.Fatalf("closed kill query = %+v, want this agent's control:kill mail, newest first", q)
	}
}

// Kill mail already closed when the subscriber starts was acted on by an
// earlier session: a resumed session neither stops on it now nor on a later
// reconnect, and still gets its open mail.
func TestSubscriber_ControlKill_ClosedBeforeStartIsBaseline(t *testing.T) {
	d := newFakeDaemon(2)
	d.closedKills = []*beadsapi.BeadDetail{{ID: "gd-kold", Type: "mail", Title: "Stop", Assignee: "local-laptop", Status: "closed", Labels: []string{"from:slack-bridge", "control:kill"}}}
	d.open = []*beadsapi.BeadDetail{{ID: "gd-m1", Type: "mail", Title: "hello", Assignee: "local-laptop", Status: "open", Labels: []string{"from:captain"}}}
	c := &fakeCoop{}
	exited := make(chan struct{}, 1)
	start(t, Config{Daemon: d, Coop: c, Exit: func() { exited <- struct{}{} }})

	waitFor(t, func() bool { return c.count() == 1 })
	close(d.streams[0])
	waitFor(t, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return len(d.closedQueries) == 2
	})
	time.Sleep(20 * time.Millisecond)
	select {
	case <-exited:
		t.Fatal("resumed session stopped on kill mail closed before it started")
	default:
	}
}

// Through a land door (no event stream) the subscriber polls: it never opens
// the stream, delivers mail that arrives between polls once, and retries
// mail coop did not take on the next poll (gd-2iqSr9ANOH).
func TestSubscriber_PollDeliversWithoutStream(t *testing.T) {
	d := newFakeDaemon(0)
	c := &fakeCoop{failures: DeliverAttempts}
	d.open = []*beadsapi.BeadDetail{{ID: "gd-p1", Type: "mail", Title: "first", Assignee: "local-laptop", Status: "open", Labels: []string{"from:captain"}}}
	start(t, Config{Daemon: d, Coop: c, Poll: 10 * time.Millisecond})

	waitFor(t, func() bool { return c.count() == 1 })
	d.mu.Lock()
	d.open = append(d.open, &beadsapi.BeadDetail{ID: "gd-p2", Type: "mail", Title: "second", Assignee: "local-laptop", Status: "open", Labels: []string{"from:captain"}})
	d.mu.Unlock()
	waitFor(t, func() bool { return c.count() == 2 })
	time.Sleep(40 * time.Millisecond)
	if c.count() != 2 {
		t.Fatalf("mail delivered more than once across polls: %v", c.nudges)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.subs != 0 {
		t.Fatalf("poll mode opened the event stream %d times", d.subs)
	}
}

// Mail the daemon parked for this agent's runner (runner:<id>, sent while no
// agent was joined on it) is re-addressed to the agent, then delivered.
func TestSubscriber_Drain_ClaimsParkedRunnerMail(t *testing.T) {
	d := newFakeDaemon(1)
	d.open = []*beadsapi.BeadDetail{{ID: "gd-p1", Type: "mail", Title: "Escalation gd-e1 approved: shell", Assignee: "runner:gd-r1", Status: "open", Labels: []string{"from:kbeads", "control:escalation-approved"}, Fields: map[string]string{"escalation_id": "gd-e1"}}}
	c := &fakeCoop{}
	got := make(chan string, 2)
	s := New(Config{
		Daemon: d, Coop: c, AgentBead: "gd-agent1", AgentName: "local-laptop", RunnerID: "gd-r1", Logger: quiet,
		OnControl: func(_ context.Context, verb string, m beadsapi.BeadDetail) {
			got <- verb + ":" + m.Fields["escalation_id"] + ":" + m.Assignee
		},
	})
	if err := s.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case v := <-got:
		if v != "escalation-approved:gd-e1:local-laptop" {
			t.Fatalf("got %q", v)
		}
	case <-time.After(time.Second):
		t.Fatal("parked runner mail not delivered on drain")
	}
	d.mu.Lock()
	reassigned := d.reassigned["gd-p1"]
	d.mu.Unlock()
	if reassigned != "local-laptop" {
		t.Fatalf("parked mail must be re-addressed to the agent, got %q", reassigned)
	}
	// The re-addressed mail is now the agent's own; a second drain lists it
	// under AgentName and must not hand it over again.
	if err := s.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case v := <-got:
		t.Fatalf("claimed mail delivered twice: %q", v)
	case <-time.After(20 * time.Millisecond):
	}
}

// Parked mail is not delivered when it cannot be re-addressed (another
// agent on the runner, or the daemon, refused): it stays parked for a
// later drain, and the error is returned so the caller re-drains.
func TestSubscriber_Drain_ParkedRunnerMailNotDeliveredWhenClaimFails(t *testing.T) {
	d := newFakeDaemon(1)
	d.open = []*beadsapi.BeadDetail{{ID: "gd-p1", Type: "mail", Assignee: "runner:gd-r1", Status: "open", Labels: []string{"from:kbeads", "control:escalation-approved"}, Fields: map[string]string{"escalation_id": "gd-e1"}}}
	d.reassignErr = context.DeadlineExceeded
	var calls int
	s := New(Config{
		Daemon: d, Coop: &fakeCoop{}, AgentName: "local-laptop", RunnerID: "gd-r1", Logger: quiet,
		OnControl: func(context.Context, string, beadsapi.BeadDetail) { calls++ },
	})
	if err := s.Drain(context.Background()); err == nil {
		t.Fatal("a failed claim must be returned so the caller re-drains")
	}
	if calls != 0 {
		t.Fatalf("unclaimed parked mail delivered %d times", calls)
	}
}

// Without a RunnerID the subscriber never lists runner mail.
func TestSubscriber_Drain_NoRunnerIgnoresParkedMail(t *testing.T) {
	d := newFakeDaemon(1)
	d.open = []*beadsapi.BeadDetail{{ID: "gd-p1", Type: "mail", Assignee: "runner:gd-r1", Status: "open", Labels: []string{"control:escalation-approved"}}}
	var calls int
	s := New(Config{
		Daemon: d, Coop: &fakeCoop{}, AgentName: "local-laptop", Logger: quiet,
		OnControl: func(context.Context, string, beadsapi.BeadDetail) { calls++ },
	})
	if err := s.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 0 || len(d.reassigned) != 0 {
		t.Fatalf("runner mail touched without a RunnerID: calls=%d reassigned=%v", calls, d.reassigned)
	}
}
