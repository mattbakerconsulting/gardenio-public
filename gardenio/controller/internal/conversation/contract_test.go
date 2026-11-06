package conversation_test

import (
	"testing"

	"gardenio/controller/internal/conversation"
	"gardenio/controller/internal/conversation/surfacetest"
)

// TestRecorderContract runs the Surface contract against the Recorder fake, so
// the fake the router tests lean on behaves like the Slack surface does (the
// same contract runs against it over the slackmock twin in internal/bridge).
func TestRecorderContract(t *testing.T) {
	for _, name := range []string{conversation.SurfaceSlack, conversation.SurfaceCommons} {
		t.Run(name, func(t *testing.T) {
			surfacetest.RunContract(t, surfacetest.Harness{
				New: func(t *testing.T) (conversation.Surface, surfacetest.Lookup) {
					r := surfacetest.NewRecorder(name)
					return r, r.Lookup
				},
				Channel: conversation.ChannelRef{ID: "bd-chan", Land: "gardenio", Slack: "C0CONTRACT"},
				To:      conversation.Principal{ID: "oidc:op@example.com", SlackUser: "U0OP"},
				Trigger: conversation.Trigger{Surface: name, TriggerID: "trig-1", Channel: "C0CONTRACT"},
			})
		})
	}
}
