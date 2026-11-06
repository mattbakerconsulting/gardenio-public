package server

import (
	"encoding/json"
	"fmt"
	"net/url"
)

// validateReproFields enforces the repro-harness cross-field constraint that
// isn't expressible in a TypeConfig FieldDef alone: when gate_passed=true, the
// repro_evidence field MUST carry a valid absolute URL. The type:task and
// type:molecule schemas (config.go reproFields) already type-check each field
// in isolation (gate_passed is a boolean, repro_evidence/repro_ticket are
// strings); this adds the "a passed gate must cite evidence, and that evidence
// must be a real URL" rule the repro-harness formula (ka-0000002084) relies on
// so the fsm-triage routing gate never advances on an unbacked pass
// (ka-0000002579).
//
// Called from createBead and updateBead after generic field validation, for
// task and molecule beads. On update the daemon has already patch-merged the
// incoming fields into the stored blob (beads.go), so `fields` is the effective
// post-merge state — a PATCH that sets gate_passed=true is validated against the
// repro_evidence already on the bead, and a PATCH that only sets repro_evidence
// is validated against the already-stored gate_passed. When gate_passed is not
// true (false, null, or absent) the rule is inert, so legacy beads and
// in-progress repros are never wedged.
func validateReproFields(fields json.RawMessage) error {
	if len(fields) == 0 {
		return nil
	}
	var f struct {
		GatePassed    *bool  `json:"gate_passed"`
		ReproEvidence string `json:"repro_evidence"`
	}
	if err := json.Unmarshal(fields, &f); err != nil {
		// Generic validation already rejects malformed JSON; defensive.
		return nil
	}
	if f.GatePassed == nil || !*f.GatePassed {
		return nil
	}
	if f.ReproEvidence == "" {
		return fmt.Errorf("repro_evidence is required when gate_passed=true (cite the video/screenshot evidence URL backing the passed gate)")
	}
	if !isValidEvidenceURL(f.ReproEvidence) {
		return fmt.Errorf("repro_evidence must be a valid URL when gate_passed=true (got %q)", f.ReproEvidence)
	}
	return nil
}

// isValidEvidenceURL reports whether s parses as an absolute URL with an
// http/https scheme and a host. This is deliberately strict about scheme and
// host (a bare "example.com" or "ka-abc123" is rejected) so the evidence field
// holds a real, followable link — the mandatory video/screenshot artifact the
// repro-harness gate demands (ka-0000005631, ka-0000003732) — rather than a
// ticket id or free-form note, which belong in repro_ticket.
func isValidEvidenceURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	return u.Host != ""
}
