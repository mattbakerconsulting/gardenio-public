package server

import (
	"os"
	"strings"
)

// authoring_human_gate.go carries the staged warn→enforce authoring-time
// human-operator gate mode (task ka-0000003187) — extracted from audit.go to
// keep that file inside the quench size budget. The gate itself is applied at
// the createBead choke point (beads.go) and consulted by the resolver ladder
// (audit.go, via authoringHumanGateEnabled).

// authoringHumanGateMode is the staged rollout posture of the authoring-time
// human-operator gate (task ka-0000003187). It mirrors the gardenio-side
// bridge.HumanOperatorMode so operators reason about one warn→enforce ladder
// across both halves of the "real operator for every bead" directive.
type authoringHumanGateMode int

const (
	// authoringGateOff — the pre-rollout escape hatch. The resolver ladder and
	// createBead behave exactly as before this change: fleet/legacy sentinels
	// still bind, system:<source> overrides are honored verbatim, and
	// autonomous trigger/schedule authoring is never refused.
	authoringGateOff authoringHumanGateMode = iota
	// authoringGateWarn — the observation window. Behavior is IDENTICAL to off
	// (zero behavior change: no write hard-refuses and the collapsed resolver
	// ladder stays dark) EXCEPT the createBead authoring gate records M9
	// (gardenio_beads_authoring_human_gate_total{action="allowed_warn"}) for every
	// autonomous bead whose resolved operator is non-human. That counter is
	// the exact residual a flip to enforce would start refusing — the signal
	// the rollout gardener reads before committing to the flip.
	authoringGateWarn
	// authoringGateEnforce — the fail-closed terminal. The collapsed resolver
	// ladder (OIDC → X-Operated-By → spawn-chain human → REFUSE, with no
	// fleet/legacy sentinel terminal branch and system:<source> folded into
	// the single system:machine sink) is live AND the createBead authoring
	// gate REFUSES an autonomous bead with a non-human operator. This is the
	// documented "fail-closed amplification" step (bundle ka-0000006634 §5),
	// flipped only once M8/M9 show zero unexpected residual across the fleet.
	authoringGateEnforce
)

// authoringHumanGateModeFromEnv parses BEADS_AUTHORING_HUMAN_GATE into the
// staged mode. The legacy truthy set ("1"/"true"/"yes"/"on") maps to enforce
// so the flag's meaning is unchanged for anyone who already flipped it; the
// new "warn" value selects the observation window; everything else (including
// empty and any typo) is OFF — a typo can never silently start refusing live
// autonomous authoring.
func authoringHumanGateModeFromEnv() authoringHumanGateMode {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("BEADS_AUTHORING_HUMAN_GATE"))) {
	case "1", "true", "yes", "on", "enforce":
		return authoringGateEnforce
	case "warn", "observe", "dry-run":
		return authoringGateWarn
	}
	return authoringGateOff
}

// authoringHumanGateEnabled reports whether the collapsed resolver ladder
// (OIDC → X-Operated-By → spawn-chain human → REFUSE, with no fleet/legacy
// sentinel terminal branch) is ENFORCED. Only enforce mode collapses the
// ladder — warn is a pure observation window with zero behavior change — so
// every resolver-side caller (fleet-sentinel branch, sawLegacy rescue,
// system-override tail) keeps its exact pre-change behavior in off AND warn.
//
// Gated by BEADS_AUTHORING_HUMAN_GATE; default OFF. Per the design's staged
// warn→enforce rollout (task ka-0000003187) the daemon ships the new behavior
// dark and the flag is flipped to enforce only once the operated_by_source
// (M8) and authoring-gate (M9) metrics show zero unexpected residual across
// the fleet.
func authoringHumanGateEnabled() bool {
	return authoringHumanGateModeFromEnv() == authoringGateEnforce
}
