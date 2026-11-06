package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"time"

	"gardenio/kbeads/internal/client"
)

// operatedByBeadCached memoizes the agent-bead operator lookup for the lifetime
// of the process — but ONLY a successful (non-empty, human-shaped) resolution.
// An empty result is deliberately NOT cached so a later call re-resolves.
//
// Why not sync.Once: a resumed/warm pool member is spawned into standby with an
// EMPTY operated_by on its own agent bead, and the human operator is stamped
// onto that bead only AFTER the pool assigns it. A long-lived pod process that
// happened to resolve defaultOperatedBy BEFORE assignment would, under
// sync.Once, cache that pre-assignment "" for its whole life and 400 every
// attributed write with "operator could not be resolved" — exactly the
// resumed-pool attribution bug (ka-0000002362). Caching only the non-empty
// resolution lets the next call, once the bead carries the operator, pick it
// up. Short-lived kd/gb CLI invocations (Claude-driven, post-assignment) still
// resolve correctly on their single call.
var (
	operatedByBeadMu     sync.Mutex
	operatedByBeadCached string
)

// operatedByFromAgentBead resolves the human operator from the calling agent's
// own bead. It is the 4th-precedence fallback in defaultOperatedBy: used only
// when neither $BEADS_OPERATED_BY nor the credstore yielded a value.
//
// Prewarmed pods are spawned before a human task is attached, so the
// controller has not (yet) set BEADS_OPERATED_BY in the process environment —
// but the agent bead, identified by KD_AGENT_ID / BOAT_AGENT_BEAD_ID, already
// carries fields.operated_by (the human who triggered the work). Reading it
// here lets kd stamp the right operator and avoids the daemon's 400
// "operator could not be resolved" on the first live-state write.
//
// The bead value is returned ONLY when it is a well-formed human-operator id
// (slack:U… or oidc:…@…), mirroring the server's isHumanOperator guard
// (internal/server/audit.go). Agent-shaped names and system:/fleet: sentinels
// are rejected so this fallback can never attribute a write to a non-human
// identity.
func operatedByFromAgentBead() string {
	operatedByBeadMu.Lock()
	defer operatedByBeadMu.Unlock()
	if operatedByBeadCached != "" {
		return operatedByBeadCached
	}
	if v := resolveOperatedByFromAgentBead(); v != "" {
		operatedByBeadCached = v
	}
	return operatedByBeadCached
}

func resolveOperatedByFromAgentBead() string {
	agentBeadID := os.Getenv("KD_AGENT_ID")
	if agentBeadID == "" {
		agentBeadID = os.Getenv("BOAT_AGENT_BEAD_ID")
	}
	if agentBeadID == "" {
		return ""
	}

	token := defaultToken()
	c := client.NewHTTPClient(defaultHTTPURL(), token)
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	bead, err := c.GetBead(ctx, agentBeadID)
	if err != nil || bead == nil {
		return ""
	}
	candidate := operatedByFieldFromBead(bead.Fields)
	if isHumanOperatorID(candidate) {
		return candidate
	}
	return ""
}

// operatedByFieldFromBead extracts the operated_by field from a bead's raw
// JSON fields blob. Returns "" when the blob is empty, malformed, or has no
// operated_by entry.
//
// Field values are decoded per-key as json.RawMessage rather than into a
// map[string]string, because the daemon stamps non-string values into the
// same fields blob (e.g. litellm_spend_baseline is a JSON number — see
// gardenio/controller/cmd/controller/reconcile_session_spend.go). A strict
// map[string]string unmarshal errors on the FIRST non-string sibling and
// drops EVERY field, so on a real prewarmed agent bead operated_by came back
// "" and the fallback 400'd with "operator could not be resolved" even though
// the bead carried a valid human operator. Decoding only the operated_by
// value as a string is immune to the types of unrelated sibling fields.
// (gb's beadsapi.ParseFieldsJSON solves the same problem with a two-pass
// map[string]any fallback; this is the ka-module-local equivalent.)
func operatedByFieldFromBead(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return ""
	}
	rawVal, ok := fields["operated_by"]
	if !ok {
		return ""
	}
	var val string
	if err := json.Unmarshal(rawVal, &val); err != nil {
		// operated_by present but not a JSON string — not a usable
		// human-operator id.
		return ""
	}
	return strings.TrimSpace(val)
}

// isHumanOperatorID reports whether v is a well-formed human-operator id —
// either "slack:U<alnum>" (Slack user id) or "oidc:<local>@<domain>" (an email
// subject). Sentinels (system:*, fleet:*), bare agent names, and anything else
// return false. This is a client-side mirror of the daemon's isHumanOperator
// guard (internal/server/audit.go) so the prewarmed-pod bead fallback never
// stamps a non-human identity as the operator.
func isHumanOperatorID(v string) bool {
	if v == "" {
		return false
	}
	if isSlackUserOperator(v) {
		return true
	}
	const oidcPrefix = "oidc:"
	if strings.HasPrefix(v, oidcPrefix) {
		rest := v[len(oidcPrefix):]
		// Require an @ with a non-empty local part and a dotted domain.
		at := strings.IndexByte(rest, '@')
		if at <= 0 || at == len(rest)-1 {
			return false
		}
		domain := rest[at+1:]
		if !strings.Contains(domain, ".") || strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") {
			return false
		}
		return true
	}
	return false
}

// isSlackUserOperator reports whether v is a "slack:U<alnum>" operator id.
// Mirrors the daemon's extractSlackUserID: the subject must start with 'U',
// be uppercase-alphanumeric, and be no longer than 64 chars.
func isSlackUserOperator(v string) bool {
	const prefix = "slack:"
	if !strings.HasPrefix(v, prefix) {
		return false
	}
	rest := v[len(prefix):]
	if rest == "" || rest[0] != 'U' || len(rest) > 64 {
		return false
	}
	for _, r := range rest {
		switch {
		case r >= '0' && r <= '9', r >= 'A' && r <= 'Z':
		default:
			return false
		}
	}
	return true
}
