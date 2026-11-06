package events

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
	"github.com/nats-io/nats.go"
)

func TestNoopPublisher_Publish(t *testing.T) {
	pub := &NoopPublisher{}
	err := pub.Publish(context.Background(), TopicBeadCreated, BeadCreated{})
	if err != nil {
		t.Fatalf("NoopPublisher.Publish returned unexpected error: %v", err)
	}
}

func TestNoopPublisher_Close(t *testing.T) {
	pub := &NoopPublisher{}
	err := pub.Close()
	if err != nil {
		t.Fatalf("NoopPublisher.Close returned unexpected error: %v", err)
	}
}

func TestNoopPublisher_ImplementsPublisher(t *testing.T) {
	var _ Publisher = (*NoopPublisher)(nil)
}

func TestNATSPublisher_ImplementsPublisher(t *testing.T) {
	var _ Publisher = (*NATSPublisher)(nil)
}

// TestNATSPublisher_NoServer verifies that the publisher does NOT fail
// startup when NATS is unreachable (e.g. DNS not yet propagated during a
// fresh helm install). The connection is created in reconnecting state and
// should retry indefinitely. Regression for ka-0000000321.
func TestNATSPublisher_NoServer(t *testing.T) {
	// A nonexistent DNS name mirrors the chart-install race: the kbeads
	// pod schedules before the NATS Service DNS resolves.
	pub, err := NewNATSPublisher("nats://this-host-does-not-resolve.invalid:4222")
	if err != nil {
		t.Fatalf("NewNATSPublisher must not fail when NATS is unreachable: %v", err)
	}
	defer pub.Close()
	if pub.Conn().IsConnected() {
		t.Fatal("expected conn to be in reconnecting state, not connected")
	}
}

// TestNATSSubscriber_NoServer mirrors the publisher regression: subscriber
// construction must not fail when the broker is unreachable. Regression
// for ka-0000000321.
func TestNATSSubscriber_NoServer(t *testing.T) {
	sub, err := NewNATSSubscriber("nats://this-host-does-not-resolve.invalid:4222")
	if err != nil {
		t.Fatalf("NewNATSSubscriber must not fail when NATS is unreachable: %v", err)
	}
	defer sub.Close()
	if sub.Conn().IsConnected() {
		t.Fatal("expected conn to be in reconnecting state, not connected")
	}
}

func TestNATSPublisher_Publish(t *testing.T) {
	url := startTestNATS(t)

	pub, err := NewNATSPublisher(url)
	if err != nil {
		t.Fatalf("creating publisher: %v", err)
	}
	defer pub.Close()

	// Subscribe to capture published messages.
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connecting subscriber: %v", err)
	}
	defer nc.Close()

	ch := make(chan *nats.Msg, 1)
	sub, err := nc.ChanSubscribe(TopicBeadCreated, ch)
	if err != nil {
		t.Fatalf("subscribing: %v", err)
	}
	defer sub.Unsubscribe() //nolint:errcheck
	nc.Flush()

	event := BeadCreated{Bead: &model.Bead{ID: "gd-pub1", Title: "Test"}}
	if err := pub.Publish(context.Background(), TopicBeadCreated, event); err != nil {
		t.Fatalf("Publish error: %v", err)
	}
	pub.conn.Flush()

	select {
	case msg := <-ch:
		var got BeadCreated
		if err := json.Unmarshal(msg.Data, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got.Bead.ID != "gd-pub1" {
			t.Errorf("got bead ID=%q, want %q", got.Bead.ID, "gd-pub1")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for published message")
	}
}

func TestNATSPublisher_PublishMultipleTopics(t *testing.T) {
	url := startTestNATS(t)

	pub, err := NewNATSPublisher(url)
	if err != nil {
		t.Fatalf("creating publisher: %v", err)
	}
	defer pub.Close()

	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connecting subscriber: %v", err)
	}
	defer nc.Close()

	ch := make(chan *nats.Msg, 4)
	sub, err := nc.ChanSubscribe("beads.>", ch)
	if err != nil {
		t.Fatalf("subscribing: %v", err)
	}
	defer sub.Unsubscribe() //nolint:errcheck
	nc.Flush()

	for _, tc := range []struct {
		topic string
		event any
	}{
		{TopicBeadCreated, BeadCreated{Bead: &model.Bead{ID: "gd-1"}}},
		{TopicBeadDeleted, BeadDeleted{BeadID: "gd-2"}},
		{TopicLabelAdded, LabelAdded{BeadID: "gd-1", Label: "urgent"}},
		{TopicCommentAdded, CommentAdded{Comment: &model.Comment{ID: 1, BeadID: "gd-1"}}},
	} {
		if err := pub.Publish(context.Background(), tc.topic, tc.event); err != nil {
			t.Fatalf("Publish(%s): %v", tc.topic, err)
		}
	}
	pub.conn.Flush()

	for i := 0; i < 4; i++ {
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for message %d", i)
		}
	}
}

func TestNATSPublisher_FleetMetadata(t *testing.T) {
	url := startTestNATS(t)

	pub, err := NewNATSPublisher(url)
	if err != nil {
		t.Fatalf("creating publisher: %v", err)
	}
	defer pub.Close()

	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connecting subscriber: %v", err)
	}
	defer nc.Close()

	ch := make(chan *nats.Msg, 5)
	sub, err := nc.ChanSubscribe("beads.>", ch)
	if err != nil {
		t.Fatalf("subscribing: %v", err)
	}
	defer sub.Unsubscribe() //nolint:errcheck
	nc.Flush()

	// Publish events that carry fleet metadata.
	tests := []struct {
		topic string
		event any
		fleet string
	}{
		{TopicLabelAdded, LabelAdded{BeadID: "gd-1", Label: "urgent", Fleet: "qa-fleet"}, "qa-fleet"},
		{TopicDependencyAdded, DependencyAdded{Dependency: &model.Dependency{BeadID: "gd-1", DependsOnID: "gd-2"}, Fleet: "staging"}, "staging"},
		{TopicCommentAdded, CommentAdded{Comment: &model.Comment{ID: 1, BeadID: "gd-1"}, Fleet: "prod"}, "prod"},
		{TopicBeadDeleted, BeadDeleted{BeadID: "gd-3", Fleet: "default"}, "default"},
		{TopicDependencyRemoved, DependencyRemoved{BeadID: "gd-1", DependsOnID: "gd-2", Type: "blocks", Fleet: "qa-fleet"}, "qa-fleet"},
	}

	for _, tc := range tests {
		if err := pub.Publish(context.Background(), tc.topic, tc.event); err != nil {
			t.Fatalf("Publish(%s): %v", tc.topic, err)
		}
	}
	pub.conn.Flush()

	for i, tc := range tests {
		select {
		case msg := <-ch:
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(msg.Data, &raw); err != nil {
				t.Fatalf("unmarshal message %d: %v", i, err)
			}
			fleetJSON, ok := raw["fleet"]
			if !ok {
				t.Errorf("message %d (%s): missing fleet field", i, tc.topic)
				continue
			}
			var fleet string
			if err := json.Unmarshal(fleetJSON, &fleet); err != nil {
				t.Fatalf("unmarshal fleet %d: %v", i, err)
			}
			if fleet != tc.fleet {
				t.Errorf("message %d (%s): fleet=%q, want %q", i, tc.topic, fleet, tc.fleet)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for message %d", i)
		}
	}
}

func TestNATSPublisher_Close(t *testing.T) {
	url := startTestNATS(t)

	pub, err := NewNATSPublisher(url)
	if err != nil {
		t.Fatalf("creating publisher: %v", err)
	}

	if err := pub.Close(); err != nil {
		t.Fatalf("Close error: %v", err)
	}

	// Publishing after close should fail.
	err = pub.Publish(context.Background(), TopicBeadCreated, BeadCreated{})
	if err == nil {
		t.Error("expected error publishing after close")
	}
}

// TestAuthOptionsShape pins the auth mode selection (ka-0000000002): with a
// username the secret is sent as that user's PASSWORD (multi-user server
// entries only support user/password — a bare token can never match one);
// token-only keeps legacy token auth; empty yields no options.
func TestAuthOptionsShape(t *testing.T) {
	if got := len(AuthOptions("", "")); got != 0 {
		t.Errorf("AuthOptions(\"\",\"\") = %d options, want 0", got)
	}
	var o nats.Options
	for _, opt := range AuthOptions("infra", "s3cret") {
		if err := opt(&o); err != nil {
			t.Fatal(err)
		}
	}
	if o.User != "infra" || o.Password != "s3cret" || o.Token != "" {
		t.Errorf("user+token: user=%q pass=%q token=%q; want UserInfo only", o.User, o.Password, o.Token)
	}
	o = nats.Options{}
	for _, opt := range AuthOptions("", "s3cret") {
		if err := opt(&o); err != nil {
			t.Fatal(err)
		}
	}
	if o.Token != "s3cret" || o.User != "" {
		t.Errorf("token only: user=%q token=%q; want legacy token", o.User, o.Token)
	}
}
