package beadsapi

// Restart-in-place request field keys (gd-LUmZlr6CKM). The garden (beads-viewer, internal/gardenhub/wake.go) restarts a
// stale gardener whose pod is still up, but it can reach neither the agent's coop nor the controller (beads-viewer's
// egress NetworkPolicy allows the beads daemon only), so it asks through the agent bead: it writes FieldRestartRequestedAt
// and FieldRestartRequestedBy, the controller sees the write on its event stream (cmd/controller farm_restart.go), asks
// coop to restart the session in place (POST /api/v1/session/restart, the stuck remedy's non-destructive path) and
// answers on the same bead: FieldRestartAnswered carries the request stamp it answers and FieldRestartResult says how it
// went. Declared on kbeads' builtin type:agent (internal/server/config.go).
const (
	// FieldRestartRequestedAt is the RFC3339Nano time of the request. It is also the request's identity: the answer
	// copies it into FieldRestartAnswered, so a request is pending while the two differ.
	FieldRestartRequestedAt = "restart_requested_at"

	// FieldRestartRequestedBy is the operator who asked, as the viewer names them (oidc:<email>).
	FieldRestartRequestedBy = "restart_requested_by"

	// FieldRestartAnswered is the FieldRestartRequestedAt value of the last request the controller answered.
	FieldRestartAnswered = "restart_answered"

	// FieldRestartResult is how that answer went: RestartResultOK, or a sentence saying why the session was not restarted.
	FieldRestartResult = "restart_result"

	// RestartResultOK is the FieldRestartResult of a session coop took the restart for.
	RestartResultOK = "ok"
)
