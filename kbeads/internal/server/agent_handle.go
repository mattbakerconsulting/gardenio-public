package server

import (
	"context"
	"log/slog"
	"strconv"
	"strings"

	"gardenio/kbeads/internal/model"
)

// Stable, human-meaningful agent_handle naming (ka-0000002870, persistent-agents
// epic ka-0000006566 §4).
//
// Every type=agent bead created with a resolved gardener_id gets a display handle
// of the form `<gardener-slug>-<N>`, where N is a per-gardener monotonic sequence
// allocated by the store (AllocateAgentHandleSeq) from a table decoupled from
// bead lifecycle — so the counter NEVER resets when old agent beads are pruned.
// This replaces the opaque `thread-<unixts>-<n>` name (e.g.
// `thread-1780422372-432609`) as the human-facing identity on cards and in the
// viewer, while the raw `agent` name field stays the machine key.

const (
	// syntheticGardenerPrefix is stripped from a gardener_id before slugifying so
	// `synthetic-gardener:slack` yields the handle prefix `slack`, not
	// `synthetic-gardener-slack`. Kept in sync with the controller-side stamp
	// (gardenio/controller/internal/beadsapi/gardener_id_stamp.go).
	syntheticGardenerPrefix = "synthetic-gardener:"

	// agentHandlePrefixMaxLen caps the slugified gardener prefix so a pathological
	// gardener_id cannot produce an unbounded handle. The `-<N>` suffix is added
	// on top of this.
	agentHandlePrefixMaxLen = 32

	// agentHandleFallbackPrefix is used when a gardener_id slugifies to the empty
	// string (all punctuation), so a handle is always well-formed.
	agentHandleFallbackPrefix = "agent"

	// agentHandleField / agentHandlePrefixField / agentGardenerIDField are the
	// fields.* keys read and written on the type=agent bead.
	agentHandleField       = "agent_handle"
	agentHandlePrefixField = "agent_handle_prefix"
	agentGardenerIDField   = "gardener_id"
)

// slugifyHandlePart lowercases s and collapses every run of non-[a-z0-9]
// characters into a single '-', trimming leading/trailing '-' and capping the
// result at agentHandlePrefixMaxLen. Returns "" when nothing survives.
func slugifyHandlePart(s string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
		if b.Len() >= agentHandlePrefixMaxLen && !prevDash {
			break
		}
	}
	return strings.Trim(b.String(), "-")
}

// handlePrefixFromGardenerID derives the human-meaningful slug for a gardener_id:
// the `synthetic-gardener:` prefix is dropped (so the spawn-source is what shows),
// then the remainder is slugified. Falls back to agentHandleFallbackPrefix when
// the gardener_id has no slug-able content.
func handlePrefixFromGardenerID(gardenerID string) string {
	src := strings.TrimSpace(gardenerID)
	src = strings.TrimPrefix(src, syntheticGardenerPrefix)
	slug := slugifyHandlePart(src)
	if slug == "" {
		return agentHandleFallbackPrefix
	}
	return slug
}

// formatAgentHandle builds the final `<prefix>-<seq>` handle. prefix is
// slugified defensively (a caller-supplied agent_handle_prefix is untrusted
// prose); an empty slug falls back to agentHandleFallbackPrefix.
func formatAgentHandle(prefix string, seq int64) string {
	slug := slugifyHandlePart(prefix)
	if slug == "" {
		slug = agentHandleFallbackPrefix
	}
	return slug + "-" + strconv.FormatInt(seq, 10)
}

// maybeAllocateAgentHandle assigns a stable monotonic agent_handle to a
// type=agent bead that carries a resolved gardener_id and does not already have
// one. It is BEST-EFFORT: allocation runs on s.store (its own atomic statement,
// outside any create transaction), so a counter-table failure logs and is
// swallowed rather than blocking agent creation — a handle is a display
// convenience, not a correctness invariant. A "burned" sequence on a later
// create failure only leaves a harmless gap; monotonicity is preserved.
//
// The handle is NOT reallocated when the caller already supplied agent_handle:
// respawnThreadAgent reuses one name across incarnations, and passing the prior
// handle through keeps it stable across a respawn.
func (s *BeadsServer) maybeAllocateAgentHandle(ctx context.Context, bead *model.Bead) {
	if bead.Type != "agent" {
		return
	}
	fields := parseFieldsToStringMap(bead.Fields)
	if strings.TrimSpace(fields[agentHandleField]) != "" {
		return // already set (explicit or respawn passthrough) — keep it stable
	}
	gardenerID := strings.TrimSpace(fields[agentGardenerIDField])
	if gardenerID == "" {
		return // no gardener to scope the counter to
	}

	prefix := strings.TrimSpace(fields[agentHandlePrefixField])
	if prefix == "" {
		prefix = handlePrefixFromGardenerID(gardenerID)
	}

	seq, err := s.store.AllocateAgentHandleSeq(ctx, gardenerID)
	if err != nil {
		// Best-effort: a display handle is not worth failing agent creation
		// over. Log and continue with no agent_handle on this bead.
		slog.WarnContext(ctx, "agent_handle allocation failed; creating agent without a handle",
			"gardener_id", gardenerID, "error", err)
		return
	}
	bead.Fields = setField(bead.Fields, agentHandleField, formatAgentHandle(prefix, seq))
}
