package events

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
)

// NATSPublisher publishes events to NATS subjects.
// Connect to BEADS_NATS_URL, publish JSON-encoded events to the given topic.
type NATSPublisher struct {
	conn *nats.Conn
}

// AuthOptions returns the nats.Option auth set for an optional username +
// secret pair. With a username the secret is sent as that user's PASSWORD —
// multi-user server config (nats.auth.subjectIsolation) only supports
// user/password entries, and a bare token can never match one
// (ka-0000000002). Without a username the secret is sent as a legacy token.
func AuthOptions(user, token string) []nats.Option {
	switch {
	case user != "" && token != "":
		return []nats.Option{nats.UserInfo(user, token)}
	case token != "":
		return []nats.Option{nats.Token(token)}
	default:
		return nil
	}
}

func NewNATSPublisher(url string, opts ...nats.Option) (*NATSPublisher, error) {
	defaults := []nats.Option{
		// RetryOnFailedConnect keeps the daemon alive when NATS is not yet
		// reachable (e.g. DNS not propagated during a fresh helm install).
		// Without it, an initial DNS lookup failure aborts startup and the
		// pod crashloops, which under `helm --atomic` rolls the release back.
		// See ka-0000000321.
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(time.Second),
		nats.ConnectHandler(func(nc *nats.Conn) {
			slog.Info("NATS publisher connected", "url", nc.ConnectedUrl())
		}),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			slog.Warn("NATS publisher disconnected", "err", err)
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			slog.Info("NATS publisher reconnected", "url", nc.ConnectedUrl())
		}),
	}
	nc, err := nats.Connect(url, append(defaults, opts...)...)
	if err != nil {
		return nil, fmt.Errorf("connecting to NATS at %s: %w", url, err)
	}
	return &NATSPublisher{conn: nc}, nil
}

func (p *NATSPublisher) Publish(ctx context.Context, topic string, event any) error {
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshaling event: %w", err)
	}
	return p.conn.Publish(topic, data)
}

// Conn returns the underlying NATS connection. Used by serve.go to create
// a JetStream context for the eventbus without opening a second connection.
func (p *NATSPublisher) Conn() *nats.Conn {
	return p.conn
}

func (p *NATSPublisher) Close() error {
	p.conn.Close()
	return nil
}

// NATSSubscriber subscribes to events from NATS subjects.
type NATSSubscriber struct {
	conn *nats.Conn
}

// NewNATSSubscriber connects to NATS with automatic reconnection support.
// Extra nats.Option values (e.g. disconnect/reconnect handlers) can be appended.
func NewNATSSubscriber(url string, opts ...nats.Option) (*NATSSubscriber, error) {
	defaults := []nats.Option{
		// Mirror NewNATSPublisher: keep the daemon alive while NATS DNS is
		// still propagating. See ka-0000000321.
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(time.Second),
	}
	nc, err := nats.Connect(url, append(defaults, opts...)...)
	if err != nil {
		return nil, fmt.Errorf("connecting to NATS at %s: %w", url, err)
	}
	return &NATSSubscriber{conn: nc}, nil
}

// Subscribe returns a channel that receives raw event payloads for the given
// topic (supports NATS wildcards like "beads.>"). Call the returned cancel
// function to unsubscribe and close the channel.
func (s *NATSSubscriber) Subscribe(topic string) (<-chan []byte, func(), error) {
	ch := make(chan []byte, 64)

	var (
		mu     sync.Mutex
		closed bool
		once   sync.Once
	)

	sub, err := s.conn.Subscribe(topic, func(msg *nats.Msg) {
		mu.Lock()
		defer mu.Unlock()
		if closed {
			return
		}
		select {
		case ch <- msg.Data:
		default:
			// Drop message if channel is full to avoid blocking the NATS client.
		}
	})
	if err != nil {
		close(ch)
		return nil, nil, fmt.Errorf("subscribing to %s: %w", topic, err)
	}
	// Flush ensures the subscription is registered on the server before
	// returning, so that messages published on other connections are routed.
	if err := s.conn.Flush(); err != nil {
		_ = sub.Unsubscribe()
		close(ch)
		return nil, nil, fmt.Errorf("flushing subscription: %w", err)
	}

	cancel := func() {
		once.Do(func() {
			_ = sub.Unsubscribe()
			mu.Lock()
			closed = true
			mu.Unlock()
			// Drain remaining messages so senders don't block, then close.
			for {
				select {
				case <-ch:
				default:
					close(ch)
					return
				}
			}
		})
	}

	return ch, cancel, nil
}

func (s *NATSSubscriber) Close() error {
	s.conn.Close()
	return nil
}

// Conn returns the underlying NATS connection. Callers use it to inspect
// connection state (e.g. wait for IsConnected before issuing operations
// that would block on a not-yet-connected client).
func (s *NATSSubscriber) Conn() *nats.Conn {
	return s.conn
}
