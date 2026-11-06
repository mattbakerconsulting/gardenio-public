package advice

import (
	"regexp"

	"gardenio/controller/internal/beadsapi"
)

// supersedesPattern matches a "supersedes" word (or a near morphological
// variant: supersede / superseded / superseding) followed within 200 chars of
// non-sentence-terminating text by a ka-style bead ID. The intent is to catch
// natural prose like "This supersedes the earlier advice (ka-0000004853)" as
// well as terser typed-mention forms like "supersedes: ka-X" or
// "supersedes(ka-X)". Case-insensitive.
//
// The 200-char window is deliberately wider than the sentence boundary check
// (`[^.\n]`) — we stop at the first period or newline so that a later sentence
// mentioning an unrelated ka-ID cannot be mis-attributed as superseded.
var supersedesPattern = regexp.MustCompile(`(?i)\bsupersed(?:e|es|ed|ing)\b[^.\n]{0,200}?\b((?:gd|ka)-[A-Za-z0-9]+)\b`)

// ExtractSupersededIDs scans an advice bead's description for
// "supersedes ka-XXX" references and returns the IDs that the bead claims to
// supersede. Order is preserved; duplicates are removed.
//
// Detection is intentionally text-based rather than schema-based so authors do
// not need a typed-mention syntax — plain English ("supersedes the earlier
// advice (ka-XXX)") is enough. See bug ka-0000007289 for the motivating case.
func ExtractSupersededIDs(b *beadsapi.BeadDetail) []string {
	if b == nil || b.Description == "" {
		return nil
	}
	matches := supersedesPattern.FindAllStringSubmatch(b.Description, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(matches))
	var out []string
	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		id := m[1]
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// SupersededOpenIDs walks a pool of advice beads and returns, for each OPEN
// bead that another OPEN bead in the same pool explicitly supersedes (via a
// "supersedes ka-XXX" reference — see ExtractSupersededIDs), a mapping from the
// superseded bead's ID to the ID of the (first) superseder that retired it.
//
// This is the COMPACTION counterpart to ApplySupersessions. ApplySupersessions
// only HIDES a superseded bead from an agent's loadout at read time; the bead
// itself stays open and keeps consuming the advice-pool cap (R1) and the byte
// budget. Returning the superseded set lets the advice sweeper CLOSE those beads
// (preserving history) so the open pool actually shrinks — the highest-leverage,
// lowest-risk compaction, because these beads are already invisible to agents.
//
// Guards, all designed so a close is only ever proposed when it is unambiguously
// safe:
//   - A bead never supersedes itself.
//   - The superseder must be present and open in the pool (we never retire a
//     bead in favour of one that is itself already closed/absent).
//   - The superseded bead must be present and open in the pool (nothing to close
//     otherwise).
//   - Fixtures (IsFixture) and expired beads (IsExpired) are ignored on BOTH
//     sides: an E2E fixture can never trigger a real close, and an about-to-be
//     -swept expired bead is never used as the authoritative superseder.
//   - Mutual supersession (A supersedes B AND B supersedes A) is left ALONE —
//     the winner is ambiguous, so both stay open for human/agent curation rather
//     than silently losing content.
//
// A supersession CHAIN (A supersedes B, B supersedes C) correctly retires both
// B and C: B being a superseder of C does not protect B from being retired by A.
func SupersededOpenIDs(beads []*beadsapi.BeadDetail) map[string]string {
	// Index the eligible (open, non-fixture, non-expired) beads by ID so both
	// the superseder-open and superseded-open guards are cheap set lookups.
	eligible := make(map[string]*beadsapi.BeadDetail, len(beads))
	for _, b := range beads {
		if b == nil || b.ID == "" {
			continue
		}
		if b.Status == "closed" || IsFixture(b) || IsExpired(b) {
			continue
		}
		eligible[b.ID] = b
	}

	out := make(map[string]string)
	for id, superseder := range eligible {
		for _, target := range ExtractSupersededIDs(superseder) {
			if target == id {
				continue // never supersede yourself
			}
			if _, ok := eligible[target]; !ok {
				continue // target already closed/absent — nothing to compact
			}
			if _, already := out[target]; already {
				continue // keep the first superseder of record
			}
			// Mutual supersession is ambiguous — leave both open.
			if supersedesID(eligible[target], id) {
				continue
			}
			out[target] = id
		}
	}
	return out
}

// supersedesID reports whether bead b explicitly supersedes the given ID.
func supersedesID(b *beadsapi.BeadDetail, id string) bool {
	for _, s := range ExtractSupersededIDs(b) {
		if s == id {
			return true
		}
	}
	return false
}

// ApplySupersessions filters matched advice to drop any bead whose ID is
// listed as superseded by another matched bead via "supersedes ka-XXX" text
// in the superseder's description. A bead cannot supersede itself.
//
// The intent is a structural fix to the advice budget cutoff: when project
// advice explicitly overrides a global advice for its project, that global
// advice should not also be rendered (mixed signals) and should not consume
// budget that would otherwise drop the project superseder. Authors no longer
// need to game the priority/confidence field to keep an override visible —
// the supersession graph handles it. See bug ka-0000007289.
//
// Preserves input order. Returns the input slice unchanged when no
// supersession edges are present.
func ApplySupersessions(matched []MatchedAdvice) []MatchedAdvice {
	if len(matched) < 2 {
		return matched
	}
	superseded := make(map[string]bool)
	for _, m := range matched {
		if m.Bead == nil {
			continue
		}
		for _, id := range ExtractSupersededIDs(m.Bead) {
			if id == m.Bead.ID {
				// A bead claiming to supersede itself is a no-op; skip so
				// we never drop the superseder.
				continue
			}
			superseded[id] = true
		}
	}
	if len(superseded) == 0 {
		return matched
	}
	out := make([]MatchedAdvice, 0, len(matched))
	for _, m := range matched {
		if m.Bead != nil && superseded[m.Bead.ID] {
			continue
		}
		out = append(out, m)
	}
	return out
}
