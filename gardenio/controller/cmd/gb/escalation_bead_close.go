package main

import (
	"fmt"
	"strings"
)

// beadCloseOperationTypes are the escalation operation types whose approved
// executor kbeads lets close the beads the escalation names
// (kbeads/internal/server/escalation_close_grant.go, gd-Ti5JGqdhpd).
var beadCloseOperationTypes = map[string]bool{
	"kbeads_bead_close": true,
	"bead_close":        true,
}

// checkBeadCloseSpec refuses a bead-close escalation whose operation_spec
// names no bead. kbeads reads the beads the executor may close from
// operation_spec.bead_ids (immutable once filed), never from --script or
// --requesting-task, so an escalation filed with an empty spec is approved and
// then fails with an ownership 403 on every close (gd-Z7Pz2reaBG).
func checkBeadCloseSpec(opType string, spec any) error {
	if !beadCloseOperationTypes[strings.ToLower(strings.TrimSpace(opType))] {
		return nil
	}
	m, _ := spec.(map[string]any)
	ids, _ := m["bead_ids"].([]any)
	named := 0
	for _, id := range ids {
		s, ok := id.(string)
		if !ok {
			return fmt.Errorf("--operation-spec bead_ids must be a list of bead id strings")
		}
		if strings.TrimSpace(s) != "" {
			named++
		}
	}
	if named == 0 {
		return fmt.Errorf("a %s escalation must name the beads to close in --operation-spec, e.g. "+
			`--operation-spec '{"bead_ids":["gd-AAA","gd-BBB"]}'`+
			": the approved executor may close exactly those beads and nothing else", opType)
	}
	return nil
}
