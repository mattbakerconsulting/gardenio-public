package server

import (
	"context"

	"gardenio/kbeads/internal/model"
)

// callerClient names the door ctx's call came through, so a record of what a
// person's login did says whether the person or an agent on that login did it:
// the OAuth client of an OIDC token (its azp; gardenio-cli is a `kd login`), or
// runner:<id> for a local runner, whose subject reads as its owner
// (local-agents spec §4.2 D6). A machine principal's is its own client; a
// fleet or edge caller has none (P10.D gd-4LKA42orC2, gd-EmqfpYQ3NZ).
func callerClient(ctx context.Context) string {
	if c := AuthIdentityFromContext(ctx).ClientID; c != "" {
		return c
	}
	if id := RunnerIDFromContext(ctx); id != "" && authMethodFromContext(ctx) == authMethodRunner {
		return "runner:" + id
	}
	return ""
}

// closedClientField is the mutation_history field of the row a close writes
// beside its status row, naming callerClient.
const closedClientField = "closed_client"

// recordCloseClient writes the closed_client row for a close by ctx's caller,
// and nothing when callerClient is "" or the caller is a machine, whose actor
// (machine:<client>) already names it. Not an ownership signal: the status row
// beside it already flips dibs.
func (s *BeadsServer) recordCloseClient(ctx context.Context, beadID, actor string) {
	if AuthIdentityFromContext(ctx).Type == "machine" {
		return
	}
	if c := callerClient(ctx); c != "" {
		s.recordMutationNoDibs(ctx, beadID, actor, closedClientField, "", c, model.MutationTypeUpdate)
	}
}
