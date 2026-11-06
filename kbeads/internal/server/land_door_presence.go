package server

import (
	"context"
	"net/http"

	"gardenio/kbeads/internal/presence"
)

// A land agent's presence (gd-2iqSr9ANOH). A tenant's laptop agent runs gb
// through the land door, which never serves the hook routes: POST
// /v1/hooks/emit takes its actor from the body (a tenant could mark any fleet
// agent alive), runs git in the daemon on a body-supplied cwd and the
// session-end advice hooks, and dispatches onto the fleet bus; POST
// /v1/hooks/publish writes any bus subject. So the door records presence
// itself: every request a land agent token makes through its door marks that
// agent seen, under the name its token was minted for, in its land. gb on a
// land door sends its SessionStart and heartbeat as GET /v1/me, so an agent
// busy in its editor stays on the roster.

// landDoorPresenceEvent is the last_event a door request records.
const landDoorPresenceEvent = "door"

// landDoorPresenceMiddleware records a land agent token's request through
// its land door in the presence tracker. It runs inside landDoorMiddleware,
// so the token is already admitted to the door's land.
func (s *BeadsServer) landDoorPresenceMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.Presence != nil {
			if land, name, ok := landDoorAgent(r.Context()); ok {
				s.Presence.RecordHookEvent(presence.HookEvent{Actor: name, HookType: landDoorPresenceEvent, Land: land})
			}
		}
		next.ServeHTTP(w, r)
	})
}

// landDoorAgent returns the land and agent name of a per-agent token's
// request through its land door. The name is the token's subject, the name
// the D.17 mint gave the agent, never a header or body.
func landDoorAgent(ctx context.Context) (land, name string, ok bool) {
	land, door := landDoorFromContext(ctx)
	if !door {
		return "", "", false
	}
	if _, agent := agentBeadIDFromContext(ctx); !agent {
		return "", "", false
	}
	name = AuthIdentityFromContext(ctx).Subject
	return land, name, name != ""
}

// rosterLand returns the land a roster caller is held to: the land whose
// door it came through, or a land principal's own land. A land sees only its
// own agents; the fleet's actors, their working directories and sessions are
// not its to read.
func rosterLand(ctx context.Context) (string, bool) {
	if land, ok := landDoorFromContext(ctx); ok {
		return land, true
	}
	if land, ok := landPrincipalFromContext(ctx); ok && authMethodFromContext(ctx) == authMethodLand {
		return land, true
	}
	return "", false
}
