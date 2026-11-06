// Package glass holds the server side of the Lands Glass: the relay that lets
// a browser watch, nudge and attach to a live agent session under /garden.
//
// This file is step 6.1 of the Glass plan
// (docs/superpowers/specs/2026-10-19-lands-workbench-glass-design.md §3.4 #3):
// a Go port of coopmux's edge tier table, so the relay can refuse a call
// before forwarding it and coopmux re-checks the same table afterwards as
// defence in depth. The two tables must not drift, so a shared fixture
// (coop/crates/mux/tests/fixtures/edge_tiers.json) is asserted by both the
// Go test in this package and the Rust test in coop/crates/mux/src/transport.
//
// The Rust originals are `edge_required_role` and
// `edge_session_frame_required_role` in coop/crates/mux/src/transport/auth.rs.
// Keep the branch order here identical to theirs: the fixture pins the
// outcomes, the comments explain why each branch sits where it does.
package glass

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"

	"gardenio/controller/internal/oidcauth"
)

// Tier is the minimum gardenio realm role a coopmux route or control frame
// needs. Higher tiers include every lower tier (admin > operator > viewer),
// matching oidcauth's role rank.
type Tier int

const (
	// TierViewer may read: screens, replay, state, usage, the mux feed.
	TierViewer Tier = iota + 1
	// TierOperator may also type, resize, nudge and respond.
	TierOperator
	// TierAdmin may also inject raw bytes, send signals, launch, delete and
	// manage credentials. Anything unrecognised fails closed to this tier.
	TierAdmin
)

// Role returns the gardenio realm role name coopmux and Keycloak use for the
// tier, suitable for oidcauth.HasMinimumRole.
func (t Tier) Role() string {
	switch t {
	case TierViewer:
		return oidcauth.RoleViewer
	case TierOperator:
		return oidcauth.RoleOperator
	default:
		return oidcauth.RoleAdmin
	}
}

// String returns the realm role name; it is the wire form of a tier.
func (t Tier) String() string { return t.Role() }

// Allows reports whether a principal holding roles meets the tier. It uses
// the same rank ladder as every other gardenio RBAC gate, so a project member
// (no rank) never passes even TierViewer.
func (t Tier) Allows(roles []string) bool {
	return oidcauth.HasMinimumRole(roles, t.Role())
}

// TierForRole maps a gardenio realm role name back to its tier. ok is false
// for any name outside the viewer/operator/admin ladder.
func TierForRole(role string) (t Tier, ok bool) {
	switch role {
	case oidcauth.RoleViewer:
		return TierViewer, true
	case oidcauth.RoleOperator:
		return TierOperator, true
	case oidcauth.RoleAdmin:
		return TierAdmin, true
	default:
		return 0, false
	}
}

// RouteTier is the port of coopmux `edge_required_role`: the minimum tier for
// one edge-authenticated coopmux route, keyed by HTTP method and path.
//
// Unknown reads default to viewer; unknown mutations default to admin so a
// newly added control endpoint cannot silently inherit operator authority.
func RouteTier(method, path string) Tier {
	if path == "/api/v1/credentials" || strings.HasPrefix(path, "/api/v1/credentials/") {
		return TierAdmin
	}

	if path == "/ws/mux" {
		// Subscribe/unsubscribe are viewer operations. Input and resize
		// messages are checked inside the multiplexed feed.
		return TierViewer
	}
	if strings.HasPrefix(path, "/ws/") {
		// Per-session sockets carry raw upstream control messages, so the
		// upgrade itself is mutation-capable. The finer per-frame tier
		// (admin-reserved raw/signal/shutdown vs operator input/resize) is
		// SessionFrameTier.
		return TierOperator
	}
	if path == "/sse/mux/send" {
		// Carries both viewer-safe subscribe/unsubscribe and operator-only
		// input/resize; the handler decodes the body and applies the finer
		// check before routing it to the feed.
		return TierViewer
	}
	if strings.HasPrefix(path, "/sse/") {
		// The per-session send twin (POST /sse/session/{id}/send) forwards
		// the same raw control frames as /ws/{id}, so OPERATOR is a floor
		// only; SessionFrameTier applies per body.
		if strings.EqualFold(method, "GET") {
			return TierViewer
		}
		return TierOperator
	}

	if path == "/api/v1/sessions" && strings.EqualFold(method, "POST") {
		return TierAdmin
	}
	if path == "/api/v1/sessions/launch" && strings.EqualFold(method, "POST") {
		return TierAdmin
	}
	if strings.HasPrefix(path, "/api/v1/sessions/") {
		if strings.EqualFold(method, "DELETE") {
			return TierAdmin
		}
		if strings.EqualFold(method, "POST") {
			if strings.HasSuffix(path, "/agent/nudge") ||
				strings.HasSuffix(path, "/agent/respond") ||
				strings.HasSuffix(path, "/input") ||
				strings.HasSuffix(path, "/input/keys") ||
				strings.HasSuffix(path, "/input/mouse") ||
				strings.HasSuffix(path, "/resize") {
				return TierOperator
			}
			// Raw bytes and uploads are deliberately held for admin.
			return TierAdmin
		}
	}

	if strings.EqualFold(method, "GET") ||
		strings.EqualFold(method, "HEAD") ||
		strings.EqualFold(method, "OPTIONS") {
		return TierViewer
	}
	return TierAdmin
}

// SessionFrameTier is the port of coopmux `edge_session_frame_required_role`:
// the minimum tier for one control frame carried on a per-session channel
// (/ws/{id} or POST /sse/session/{id}/send).
//
// RouteTier only gates the per-session upgrade at operator, but each frame is
// forwarded verbatim to the upstream coop /ws, which executes admin-reserved
// actions: raw byte injection (input:send:raw), signals including SIGKILL
// (signal:send) and lifecycle control (shutdown, session:restart,
// session:switch). The REST matrix reserves the equivalents for admin, so the
// same boundary is mirrored per frame. Plain terminal input and resize stay
// operator; read-only queries are viewer; any unrecognised or config-mutating
// frame fails closed to admin.
//
// The per-session protocol is an internally tagged object keyed on "event"
// (coop/crates/cli/src/transport/ws_msg.rs ClientMessage); only the tag is
// decoded. A body that does not parse, or that omits "event", fails closed.
func SessionFrameTier(body []byte) Tier {
	event, ok := frameEvent(body)
	if !ok {
		return TierAdmin
	}

	switch event {
	// Read-only queries and connection keep-alive.
	case "health:get", "ready:get", "screen:get", "replay:get", "status:get", "agent:get",
		"stop:config:get", "config:start:get", "transcript:list", "transcript:get",
		"transcript:catchup", "recording:get", "recording:catchup", "usage:get",
		"profiles:list", "profiles:mode", "ping":
		return TierViewer

	// Operator terminal actions, mirroring the REST matrix where /input,
	// /input/keys, /input/mouse, /agent/nudge and /agent/respond are operator.
	// A mouse frame names a button and a cell; coop builds the report bytes.
	case "input:send", "keys:send", "mouse:send", "resize", "nudge", "respond", "stop:resolve":
		return TierOperator

	// Admin-reserved: raw byte injection, signals and lifecycle control.
	case "input:send:raw", "signal:send", "shutdown", "session:restart", "session:switch":
		return TierAdmin

	// Unknown or config-mutating frames (recording:put, profiles:register,
	// profiles:mode:set, config:put:get, stop:config:put, auth, ...) fail
	// closed.
	default:
		return TierAdmin
	}
}

// frameEvent extracts the top-level "event" string from a control frame with
// serde's strictness rather than encoding/json's leniency: the key must match
// exactly (no case folding), appear once, and hold a string; the body must be
// one JSON object with nothing after it. ok is false whenever serde_json would
// have failed to decode the Rust FrameTag, so both sides fail closed together.
func frameEvent(body []byte) (event string, ok bool) {
	dec := json.NewDecoder(bytes.NewReader(body))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return "", false
	}
	var found *string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return "", false
		}
		key, isKey := tok.(string)
		if !isKey {
			return "", false
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return "", false
		}
		if key != "event" {
			continue
		}
		if found != nil {
			// serde rejects a duplicate field.
			return "", false
		}
		var s *string
		if err := json.Unmarshal(raw, &s); err != nil || s == nil {
			return "", false
		}
		found = s
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return "", false
	}
	if _, err := dec.Token(); err != io.EOF {
		return "", false
	}
	if found == nil {
		return "", false
	}
	return *found, true
}
