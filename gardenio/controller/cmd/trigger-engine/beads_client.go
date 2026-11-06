package main

import "gardenio/controller/internal/beadsapi"

// newBeadsClient builds the beads daemon client used for every write the
// trigger-engine makes: trigger stamps and closes, agent spawns, run-task
// creates and closes, formula pours, the orphan / auto-intake / needs-gardener
// sweeps and the notice publisher.
//
// machineTS, when non-nil, is the engine's own Keycloak service-account
// credential (beadsapi.MachineTokenSourceFromEnv) and takes precedence over
// token on every request, so the daemon sees machine:<clientId>
// (gd-Hc34EENbO9). nil keeps the static token: today's behavior. The engine's
// X-Operated-By / X-System-Operator stamps (ctxWithTriggerOperator,
// WithSystemOperator) are still sent and still honoured on the static token;
// on the machine token the daemon ignores them and derives attribution itself
// (trigger lineage from fields.trigger_id, else the machine subject).
func newBeadsClient(httpAddr, token string, machineTS beadsapi.TokenSource) (*beadsapi.Client, error) {
	return beadsapi.New(beadsapi.Config{
		HTTPAddr:    httpAddr,
		Token:       token,
		TokenSource: machineTS,
	})
}
