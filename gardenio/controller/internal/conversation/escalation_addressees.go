package conversation

import (
	"log/slog"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// EscalationAddressee is one set of people an escalation is addressed to: the
// operator (the gardenio owner) or one land's owners.
type EscalationAddressee struct {
	Lane string // "operator" | "land-owner:<land>"
	Land string // set for a land-owner lane
}

// EscalationAddressees says which people an owner-audience escalation's
// held_by addresses (escalation routing D9): who, once #756's audience has
// said "the owner's". Used for DMs (bridge) and Commons placement (Lands).
// Callers check beadsapi.EscalationForOwner first; this does not. A
// coordinator, steward, requester or server lane addresses nobody: it is an
// agent's queue (and is never owner audience once stamped, D11).
//
// land is the escalation's own project. A land-owner lane naming any other
// land addresses nobody and is logged: it is stale or forged, and the wrong
// land's owners must never hear about it. The tenant/operator kind of a land
// (land_kind) is not consulted here; it is already decided by whoever stamped
// held_by.
func EscalationAddressees(heldBy, land string) []EscalationAddressee {
	held := strings.TrimSpace(heldBy)
	if held == "" {
		held = beadsapi.LaneOperator
	}
	switch kind, name, _ := strings.Cut(held, ":"); {
	case held == beadsapi.LaneOperator:
		return []EscalationAddressee{{Lane: beadsapi.LaneOperator}}
	case kind == "land-owner":
		if name == "" || name != strings.TrimSpace(land) {
			slog.Warn("escalation held_by names another land's owners; addressing nobody",
				"held_by", held, "land", land)
			return nil
		}
		return []EscalationAddressee{{Lane: held, Land: name}}
	}
	return nil
}
