package events

// Subscriber receives events from the event bus.
type Subscriber interface {
	// Subscribe delivers raw event payloads on the returned channel.
	// Call the returned cancel function to unsubscribe and close the channel.
	Subscribe(topic string) (<-chan []byte, func(), error)
	Close() error
}
