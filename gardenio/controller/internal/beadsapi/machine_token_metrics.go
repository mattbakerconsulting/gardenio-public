package beadsapi

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// machineTokenUnavailableTotal counts every Token call that returned an
// auth_unavailable error: the client secret was unreadable or empty (the
// Secret mount is optional, so a Secret nobody created lands here), the token
// endpoint refused or was unreachable, or the endpoint returned an empty
// token. Each count is a daemon call that was NOT sent — the write fails
// closed and is never substituted with the static token — so a component whose
// evaluation loop keeps beating can still be doing none of its writes. A
// cached-but-unexpired token served through a refresh failure is a success and
// is not counted.
//
// Registered against the default gatherer (promauto) so any component that
// serves it (trigger-engine :9092 /metrics) exposes it with no wiring.
var machineTokenUnavailableTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "gardenio_machine_token_unavailable_total",
	Help: "Machine-principal token requests that failed closed with auth_unavailable (client secret unreadable/empty, token endpoint refused, empty token). Each is a daemon call that was not sent.",
}, []string{"client"})

// machineForbiddenTotal counts every 403 the daemon returned to a request that
// carried this machine principal's token (OnForbidden). The daemon-side
// gardenio_authz_denials_total has no caller label, so this is the per-principal
// view: a component whose role grants do not cover its writes (or whose token
// predates a newly attached role) shows up here by client. Alerts read it with
// increase().
var machineForbiddenTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "gardenio_machine_forbidden_total",
	Help: "403 responses the beads daemon returned to requests carrying this machine principal's token (authz deny). Each is a write that was not applied.",
}, []string{"client"})

// machineTokenMissingRoleTotal counts token fetches whose realm roles lacked a
// role the component declared it needs (WithExpectedRoles): Keycloak had not
// attached the role to the service account yet.
var machineTokenMissingRoleTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "gardenio_machine_token_missing_role_total",
	Help: "Machine-principal token fetches whose realm roles lacked an expected gardenio-machine-* role (re-fetched after 30s instead of at half-life).",
}, []string{"client", "role"})
