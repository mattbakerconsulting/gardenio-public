package events

import "context"

// NoopPublisher is a Publisher that does nothing (used when NATS is not configured).
type NoopPublisher struct{}

func (n *NoopPublisher) Publish(ctx context.Context, topic string, event any) error {
	return nil
}

func (n *NoopPublisher) Close() error {
	return nil
}
