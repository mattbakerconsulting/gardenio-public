package server

import (
	"net/http"

	"gardenio/kbeads/internal/eventbus"
)

// busStatusResponse is the JSON response from GET /v1/bus/status.
type busStatusResponse struct {
	JetStreamEnabled bool     `json:"jetstream_enabled"`
	HandlerCount     int      `json:"handler_count"`
	Streams          []string `json:"streams"`
	Handlers         []string `json:"handlers,omitempty"`
}

// handleBusStatus handles GET /v1/bus/status.
// Returns the current state of the event bus: JetStream connectivity, streams, handlers.
func (s *BeadsServer) handleBusStatus(w http.ResponseWriter, _ *http.Request) {
	resp := busStatusResponse{
		Streams: eventbus.StreamNames,
	}

	if s.bus != nil {
		resp.JetStreamEnabled = s.bus.JetStreamEnabled()
		handlers := s.bus.Handlers()
		resp.HandlerCount = len(handlers)
		for _, h := range handlers {
			resp.Handlers = append(resp.Handlers, h.ID())
		}
	}

	writeJSON(w, http.StatusOK, resp)
}
