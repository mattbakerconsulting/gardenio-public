package bridge

import (
	"log/slog"
	"os"
	"strings"
)

// human_operator.go centralizes the gardenio-side half of the "real (human)
// operator for every bead" invariant (epic ka-0000004835, task ka-0000002839).
//
// The daemon (kbeads) fails CLOSED on every audited write when no HUMAN
// operator can be attributed: `resolveAuditOperatorRequired` rejects anything
// that is not `slack:U…` or `oidc:<local>@<domain>` (kbeads
// internal/server/audit.go isHumanOperator). Autonomous spawn paths
// (scheduler / gardener / trigger / gitlab / jira) historically stamped
// `spawned_by: trigger:<id>` but no usable human `operated_by`, so the spawned
// pod booted with BEADS_OPERATED_BY unset, the resolver dead-ended on the
// non-agent trigger bead, and the agent could not even file the escalation
// reporting it was blocked (origin bug ka-0000002128).
//
// This file gives the controller / slack-bridge a single predicate matching
// the daemon's, plus a staged gate so we can stamp aggressively, observe in
// `warn`, and only fail-close (`enforce`) once the daemon-side system:machine
// sink (ka-0000006425) and rollout metrics (ka-0000003187) are in place. This
// staging is mandated by the epic design §5 to avoid the fail-closed
// amplification that broke gasbox (ka-0000005858 RC3).

// IsHumanOperator reports whether v is a well-formed HUMAN-operator id — the
// only two shapes the kbeads daemon accepts for an audit-bearing write:
//
//   - "slack:U<alnum>"        — a Slack user id
//   - "oidc:<local>@<domain>" — an email subject (domain must be dotted)
//
// Sentinels (system:*, fleet:*), provider ids that are NOT human-mapped
// (jira:<accountId>, gitlab:<id>), bare agent names, and anything else return
// false. This is an exact mirror of kbeads internal/server/audit.go
// isHumanOperator (+ auto_inherit.go extractSlackUserID); keep the two in
// lockstep — a value this accepts but the daemon rejects re-introduces the
// "stamped a non-human operator that fails closed downstream" bug.
func IsHumanOperator(v string) bool {
	if v == "" {
		return false
	}
	if _, ok := extractSlackUserIDOperator(v); ok {
		return true
	}
	const oidcPrefix = "oidc:"
	if rest, ok := strings.CutPrefix(v, oidcPrefix); ok {
		// require an @ with a non-empty local part + dotted domain
		at := strings.IndexByte(rest, '@')
		if at <= 0 || at == len(rest)-1 {
			return false
		}
		domain := rest[at+1:]
		if !strings.Contains(domain, ".") ||
			strings.HasPrefix(domain, ".") ||
			strings.HasSuffix(domain, ".") {
			return false
		}
		return true
	}
	return false
}

// extractSlackUserIDOperator mirrors kbeads auto_inherit.go extractSlackUserID:
// "slack:U<alnum>" with a leading 'U', length-bounded, [0-9A-Z] only.
func extractSlackUserIDOperator(operatedBy string) (string, bool) {
	const prefix = "slack:"
	rest, ok := strings.CutPrefix(operatedBy, prefix)
	if !ok {
		return "", false
	}
	if rest == "" || rest[0] != 'U' || len(rest) > 64 {
		return "", false
	}
	for _, r := range rest {
		switch {
		case r >= '0' && r <= '9', r >= 'A' && r <= 'Z':
		default:
			return "", false
		}
	}
	return rest, true
}

// HumanOperatorMode selects how strictly the controller / slack-bridge enforce
// the human-operator invariant on autonomous spawn paths.
type HumanOperatorMode int

const (
	// HumanOperatorOff disables the gate entirely: spawns proceed and the
	// client-level system-operator defaults remain in place. This is the
	// pre-rollout-safe escape hatch.
	HumanOperatorOff HumanOperatorMode = iota
	// HumanOperatorWarn (the default) stamps the resolved human aggressively
	// and LOGS when a spawn path cannot resolve a human, but never refuses and
	// keeps the system-operator defaults so no live service write hard-400s.
	// This is the observation window for the rollout metrics (ka-0000003187).
	HumanOperatorWarn
	// HumanOperatorEnforce REFUSES a spawn when no human operator is
	// resolvable and drops the client-level system-operator defaults so a
	// missing human is a loud refusal, not a silent sentinel. Flip here only
	// once the daemon-side system:machine sink (ka-0000006425) and zero
	// unexpected refusals are confirmed.
	HumanOperatorEnforce
)

func (m HumanOperatorMode) String() string {
	switch m {
	case HumanOperatorOff:
		return "off"
	case HumanOperatorEnforce:
		return "enforce"
	default:
		return "warn"
	}
}

// humanOperatorModeEnvVar is the env var read by HumanOperatorModeFromEnv.
const humanOperatorModeEnvVar = "GARDENIO_REQUIRE_HUMAN_OPERATOR"

// ParseHumanOperatorMode maps a string to a HumanOperatorMode. Unknown / empty
// values default to warn — the safe-but-observed posture, never enforce, so a
// typo can never silently start hard-refusing live spawns.
func ParseHumanOperatorMode(s string) HumanOperatorMode {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "off", "disabled", "0", "false":
		return HumanOperatorOff
	case "enforce", "strict", "refuse":
		return HumanOperatorEnforce
	default:
		return HumanOperatorWarn
	}
}

// HumanOperatorModeFromEnv reads GARDENIO_REQUIRE_HUMAN_OPERATOR. Default warn.
func HumanOperatorModeFromEnv() HumanOperatorMode {
	return ParseHumanOperatorMode(os.Getenv(humanOperatorModeEnvVar))
}

// triggerDefaultOperatorEnvVar names the designated HUMAN operator a
// TriggerEngine attributes an autonomous fire to when the trigger bead itself
// carries no walkable human operator (its author is a sentinel/agent). This is
// the TriggerEngine analogue of JIRA_BRIDGE_DEFAULT_OPERATOR — which only feeds
// the @-mention JiraAgentResolver, NOT the JQL/bead/schedule fireAction path
// (the gap that booted fsm-triage agents unattributed, ka-0000006023).
const triggerDefaultOperatorEnvVar = "TRIGGER_ENGINE_DEFAULT_OPERATOR"

// TriggerDefaultOperatorFromEnv reads TRIGGER_ENGINE_DEFAULT_OPERATOR and
// returns it ONLY when it is a well-formed human-operator id; any other value
// (empty, sentinel, jira:/gitlab: provider id, bare name) is dropped to "" so
// a misconfigured value can never re-introduce the "stamped a non-human
// operator that the daemon fails closed on" bug (ka-0000002128). An empty
// result means "no fallback configured" — the engine then behaves exactly as
// before this knob existed.
func TriggerDefaultOperatorFromEnv() string {
	v := strings.TrimSpace(os.Getenv(triggerDefaultOperatorEnvVar))
	if v == "" || !IsHumanOperator(v) {
		return ""
	}
	return v
}

// gateSpawnOperator decides whether an autonomous spawn may proceed given the
// operator it managed to resolve. It returns (operatedBy, nil) to proceed, or
// ("", err) to REFUSE the spawn.
//
//   - If operatedBy passes IsHumanOperator, the spawn always proceeds.
//   - Otherwise behavior depends on mode:
//     off     → proceed with operatedBy unchanged (no attribution required).
//     warn    → proceed but LOG the gap (operatedBy is returned as-is so any
//     non-human value is still recorded for observability; downstream
//     daemon attribution / fallback is unchanged from today).
//     enforce → REFUSE with errNoHumanOperator.
//
// path/agent are used only for log/error context.
func gateSpawnOperator(mode HumanOperatorMode, logger *slog.Logger, path, agent, operatedBy string) (string, error) {
	if IsHumanOperator(operatedBy) {
		return operatedBy, nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	switch mode {
	case HumanOperatorOff:
		return operatedBy, nil
	case HumanOperatorEnforce:
		logger.Warn("human-operator gate: refusing spawn — no resolvable human operator",
			"path", path, "agent", agent, "resolved_operated_by", operatedBy, "mode", mode.String())
		return "", &noHumanOperatorError{path: path, agent: agent, resolved: operatedBy}
	default: // warn
		logger.Warn("human-operator gate: spawn has no resolvable human operator (warn — allowing)",
			"path", path, "agent", agent, "resolved_operated_by", operatedBy, "mode", mode.String())
		return operatedBy, nil
	}
}

// noHumanOperatorError is returned by gateSpawnOperator in enforce mode.
type noHumanOperatorError struct {
	path     string
	agent    string
	resolved string
}

func (e *noHumanOperatorError) Error() string {
	resolved := e.resolved
	if resolved == "" {
		resolved = "<none>"
	}
	return "refusing to spawn agent " + e.agent + " via " + e.path +
		": no resolvable human operator (resolved=" + resolved +
		"); set " + humanOperatorModeEnvVar + "=warn to allow, or attribute a human via the trigger/mention author"
}

// stampHumanOwner names the agent's human operated_by as its owner when it has
// none, as a trigger stamps the human it spawned for (triggers_actions.go): the
// person who asked owns the thread agent, and the card's Owner line reads it.
// A synthetic or unresolved operator stamps nothing.
func stampHumanOwner(fields map[string]string) {
	if fields["owner"] != "" {
		return
	}
	if op := fields["operated_by"]; IsHumanOperator(op) {
		fields["owner"] = op
	}
}
