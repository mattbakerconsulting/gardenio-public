package beadsapi

import (
	"encoding/json"
	"log/slog"
	"strings"
)

// Canonical gardener-ownership stamping (persistent-agents Phase 1, task
// ka-0000006626, design memo ka-0000001420 §2.3 / §3.1).
//
// Every agent bead is stamped a canonical `gardener_id` AT SPAWN so the viewer's
// "Unowned" bucket is empty by construction — no GC, no backfill-driven
// reparenting race. Enforcement is soft and at-spawn only: this stamps a value,
// it never gates the write and never mutates the dependency graph.
//
// Resolution is a strict priority order (first non-empty wins):
//
//  1. An already-set `gardener_id` — honored verbatim. The successor path
//     (gb stop --successor=new) and any explicit caller inherit/override this;
//     re-stamping would clobber a deliberate value, so a present id is final.
//  2. A REAL gardener id: the gardener-loop / trigger the agent was spawned by,
//     read from `schedule_id` then `trigger_id` (the dual-key the viewer's
//     legacy fallback already resolves). Both hold the gardener (trigger) bead
//     id; a gardener-spawned crew is owned by its gardener.
//  3. A per-`spawn_source` SYNTHETIC gardener id for ad-hoc spawns that have no
//     real gardener (slack-thread, gitlab-mr-review, jira-mention, concierge,
//     successor, manual, …): `synthetic-gardener:<spawn_source>`. This buckets
//     every ad-hoc vector under one stable owner per source instead of leaving
//     it unowned.
//  4. A final generic synthetic id (`syntheticGardenerUnknown`) when even
//     spawn_source is absent, so the invariant "no agent is ever unowned" holds
//     unconditionally — a spawn path that forgot to set spawn_source still lands
//     in a named bucket rather than the empty-owner sink.
const (
	// syntheticGardenerPrefix namespaces a synthetic (source-derived) gardener id
	// so it never collides with a real gardener (trigger) bead id (gd-…) and is
	// trivially recognizable as synthetic by readers.
	syntheticGardenerPrefix = "synthetic-gardener:"
	// syntheticGardenerUnknown is the last-resort owner for an agent carrying no
	// gardener and no spawn_source at all. Its existence is what makes the
	// "Unowned empty by construction" invariant unconditional.
	syntheticGardenerUnknown = syntheticGardenerPrefix + "unknown"
)

// resolveGardenerID computes the canonical gardener_id for an agent bead from its
// spawn-context fields, following the priority order documented above. It is
// pure (no I/O) and total: it always returns a non-empty owner, so a caller can
// stamp the result unconditionally.
func resolveGardenerID(fields map[string]string) string {
	// Tier 1 — an explicit / inherited gardener_id is authoritative.
	if v := strings.TrimSpace(fields["gardener_id"]); v != "" {
		return v
	}
	// Tier 2 — a real gardener (trigger) the agent was spawned by. schedule_id
	// and trigger_id both carry the gardener bead id; schedule_id is the
	// gardener-loop alias and takes precedence when both are present.
	for _, k := range []string{"schedule_id", "trigger_id"} {
		if v := strings.TrimSpace(fields[k]); v != "" {
			return v
		}
	}
	// Tier 3 — a per-spawn_source synthetic gardener for ad-hoc vectors.
	if src := strings.TrimSpace(fields["spawn_source"]); src != "" {
		return syntheticGardenerPrefix + src
	}
	// Tier 4 — generic synthetic owner: nothing is ever left unowned.
	return syntheticGardenerUnknown
}

// DeriveGardenerID is the exported, read-only projection of resolveGardenerID.
// It computes the canonical gardener_id an agent bead WOULD carry from its
// spawn-context fields, without mutating anything. Callers that only want to
// display or filter on gardener ownership (e.g. the agent roster) use this
// instead of touching the create-path stamping helpers. It is pure and total:
// it always returns a non-empty owner, mirroring the at-spawn stamp so a
// legacy bead written before the stamp existed still resolves to the same
// owner the stamp would have assigned (gardener_id ∨ schedule_id/trigger_id ∨
// synthetic).
func DeriveGardenerID(fields map[string]string) string {
	return resolveGardenerID(fields)
}

// IsSyntheticGardenerID reports whether a gardener_id is a SYNTHETIC owner —
// i.e. an ad-hoc/unknown bucket (`synthetic-gardener:<source>`) rather than a
// real gardener (trigger) bead id. An agent whose gardener_id is synthetic has
// no real gardener: it is the "leak" the roster's --no-gardener detector
// surfaces. An empty id is treated as synthetic (no real gardener).
func IsSyntheticGardenerID(id string) bool {
	return strings.HasPrefix(strings.TrimSpace(id), syntheticGardenerPrefix) ||
		strings.TrimSpace(id) == ""
}

// stampGardenerID resolves and writes the canonical gardener_id into the given
// field map when it is not already set, returning whether a value was written.
// The map must be non-nil. A pre-existing gardener_id is left untouched (tier 1),
// so the call is idempotent and safe to run on every agent-bead create.
func stampGardenerID(fields map[string]string) bool {
	if fields == nil {
		return false
	}
	if strings.TrimSpace(fields["gardener_id"]) != "" {
		return false
	}
	fields["gardener_id"] = resolveGardenerID(fields)
	return true
}

// stampAgentGardenerID is the create-choke-point wrapper: it stamps a canonical
// gardener_id onto an agent-bead create's raw JSON fields and returns the
// (possibly rewritten) fields payload for the outgoing request. It is a no-op —
// returning rawFields unchanged — for any non-agent bead type or when the fields
// already carry a gardener_id.
//
// Fail-open by design: if the fields payload is present but not decodable as a
// string map (an unexpected shape), it is passed through untouched rather than
// dropped, so a malformed edge case can never wedge a spawn. This mirrors the
// best-effort posture of the surrounding SpawnAgent dedup/label writes.
func stampAgentGardenerID(beadType string, rawFields json.RawMessage) json.RawMessage {
	if beadType != "agent" {
		return rawFields
	}

	fields := map[string]string{}
	if len(rawFields) > 0 {
		if err := json.Unmarshal(rawFields, &fields); err != nil {
			// Non-string-map fields payload: leave it exactly as-is. Agent beads
			// are always created with a string-map fields payload, so this only
			// trips on a genuinely unexpected caller — pass through, don't drop.
			slog.Warn("gardener_id stamp: agent fields not a string map, skipping",
				"error", err)
			return rawFields
		}
	}

	if !stampGardenerID(fields) {
		return rawFields
	}

	stamped, err := json.Marshal(fields)
	if err != nil {
		// Re-marshal of a string map cannot realistically fail; if it somehow
		// does, fall back to the original payload rather than corrupting the
		// request.
		slog.Warn("gardener_id stamp: re-marshal failed, using original fields",
			"error", err)
		return rawFields
	}
	return stamped
}
