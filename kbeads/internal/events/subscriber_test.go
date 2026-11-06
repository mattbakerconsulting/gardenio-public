package events

import (
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// startTestNATS starts an embedded NATS server and returns its client URL.
func startTestNATS(t *testing.T) string {
	t.Helper()
	opts := &natsserver.Options{Host: "127.0.0.1", Port: -1}
	srv, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatalf("starting embedded NATS: %v", err)
	}
	srv.Start()
	t.Cleanup(srv.Shutdown)
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("embedded NATS not ready")
	}
	return srv.ClientURL()
}

func TestNATSSubscriber_ReceivesMessages(t *testing.T) {
	url := startTestNATS(t)

	pub, err := NewNATSPublisher(url)
	if err != nil {
		t.Fatalf("creating publisher: %v", err)
	}
	defer pub.Close()

	sub, err := NewNATSSubscriber(url)
	if err != nil {
		t.Fatalf("creating subscriber: %v", err)
	}
	defer sub.Close()

	ch, cancel, err := sub.Subscribe("beads.>")
	if err != nil {
		t.Fatalf("subscribing: %v", err)
	}
	defer cancel()

	// Publish after subscribing.
	if err := pub.conn.Publish("beads.bead.created", []byte(`{"id":"1"}`)); err != nil {
		t.Fatalf("publishing: %v", err)
	}
	pub.conn.Flush()

	select {
	case msg := <-ch:
		if string(msg) != `{"id":"1"}` {
			t.Errorf("got %q, want %q", msg, `{"id":"1"}`)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for message")
	}
}

func TestNATSSubscriber_Cancel(t *testing.T) {
	url := startTestNATS(t)

	sub, err := NewNATSSubscriber(url)
	if err != nil {
		t.Fatalf("creating subscriber: %v", err)
	}
	defer sub.Close()

	ch, cancel, err := sub.Subscribe("beads.>")
	if err != nil {
		t.Fatalf("subscribing: %v", err)
	}

	cancel()

	// Channel should be closed.
	_, ok := <-ch
	if ok {
		t.Fatal("expected channel to be closed after cancel")
	}
}

func TestNATSSubscriber_WildcardTopicMatching(t *testing.T) {
	url := startTestNATS(t)

	pub, err := NewNATSPublisher(url)
	if err != nil {
		t.Fatalf("creating publisher: %v", err)
	}
	defer pub.Close()

	sub, err := NewNATSSubscriber(url)
	if err != nil {
		t.Fatalf("creating subscriber: %v", err)
	}
	defer sub.Close()

	ch, cancel, err := sub.Subscribe("beads.>")
	if err != nil {
		t.Fatalf("subscribing: %v", err)
	}
	defer cancel()

	topics := []string{"beads.bead.created", "beads.bead.updated", "beads.label.added"}
	for i, topic := range topics {
		data := []byte(`{"n":` + string(rune('0'+i)) + `}`)
		if err := pub.conn.Publish(topic, data); err != nil {
			t.Fatalf("publishing to %s: %v", topic, err)
		}
	}
	pub.conn.Flush()

	for i := range len(topics) {
		select {
		case <-ch:
			// received
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for message %d", i)
		}
	}
}

func TestNATSSubscriber_ImplementsSubscriber(t *testing.T) {
	var _ Subscriber = (*NATSSubscriber)(nil)
}

func TestNATSSubscriber_DoubleCancel(t *testing.T) {
	url := startTestNATS(t)

	sub, err := NewNATSSubscriber(url)
	if err != nil {
		t.Fatalf("creating subscriber: %v", err)
	}
	defer sub.Close()

	_, cancel, err := sub.Subscribe("beads.>")
	if err != nil {
		t.Fatalf("subscribing: %v", err)
	}

	// Calling cancel twice should not panic.
	cancel()
	cancel()
}

func TestNATSSubscriber_CancelDuringMessages(t *testing.T) {
	url := startTestNATS(t)

	pub, err := NewNATSPublisher(url)
	if err != nil {
		t.Fatalf("creating publisher: %v", err)
	}
	defer pub.Close()

	sub, err := NewNATSSubscriber(url)
	if err != nil {
		t.Fatalf("creating subscriber: %v", err)
	}
	defer sub.Close()

	ch, cancel, err := sub.Subscribe("beads.>")
	if err != nil {
		t.Fatalf("subscribing: %v", err)
	}

	// Publish 100 messages concurrently with cancel.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			_ = pub.conn.Publish("beads.bead.created", []byte(`{"id":"x"}`))
		}
		pub.conn.Flush()
	}()

	// Cancel while messages are being sent -- must not panic.
	cancel()
	<-done

	// Channel should be closed.
	_, ok := <-ch
	if ok {
		t.Fatal("expected channel to be closed after cancel")
	}
}

func TestNATSSubscriber_ReconnectHandler(t *testing.T) {
	url := startTestNATS(t)

	reconnected := make(chan struct{}, 1)
	sub, err := NewNATSSubscriber(url,
		nats.ReconnectHandler(func(_ *nats.Conn) {
			select {
			case reconnected <- struct{}{}:
			default:
			}
		}),
	)
	if err != nil {
		t.Fatalf("creating subscriber: %v", err)
	}
	defer sub.Close()

	// Verify the handler option was accepted (connection is alive).
	if !sub.conn.IsConnected() {
		t.Fatal("expected subscriber to be connected")
	}
}
