package bridge

// Event-wake path (persistent-agents Phase 3, task ka-0000000027): the bridge
// revives a dormant identity in place when an external event lands on it (a
// mention, a task assignment, a decision or escalation it filed being
// resolved). The primitive lives in internal/agentwake, which cmd/github-bridge
// shares for the CI wake (gd-T7aFA3tFoW); these names keep the bridge's
// callers and tests unchanged.

import (

	"gardenio/controller/internal/agentwake"
)

// eventWakeClient is the daemon subset the event-wake path needs.
type eventWakeClient = agentwake.EventWakeClient
