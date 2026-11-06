// Server-side dedup-ack gate for decision and escalation creates.
//
// Gardeners (scheduled or trigger-spawned agents) repeatedly file DUPLICATE
// type=decision and type=escalation beads about the same subject — e.g. MR
// !2982 accumulated two stale open decisions before a third gardener firing
// self-merged it. Each duplicate pollutes the operator's inbox and dilutes
// the signal of the ones that matter.
//
// This gate makes the daemon reject a decision/escalation create when one or
// more OPEN beads of the same type already exist in the same project, UNLESS
// the caller echoes a server-computed hash of that open list. The contract:
//
//	create decision → 409 dedup_ack_required + { open: [...], hash: "…" }
//	create decision --saw-open-decisions=<hash> → proceeds iff hash matches
//	  the CURRENT open snapshot (a stale hash re-rejects with the fresh list,
//	  which is how the race is closed: new decisions appearing mid-loop change
//	  the hash and force a fresh look).
//
// Same machinery for both types; the queues are independent (a decision
// create is gated only on open decisions, never on open escalations).
//
// Design-of-record: kbeads/docs/research-dedup-ack-gate.md (epic
// ka-0000004599). This is T1 (server). The CLI rendering + retry is T2.
//
// The gate is behind BEADS_DEDUP_GATE_ENABLED (default OFF) so it ships dark
// and is flipped on per the rollout plan (§13) only after gardener prompts
// (T4) teach the retry-with-hash step. With the flag OFF, createBead behaves
// exactly as before. Force=true (CLI --force, escalation replay) always
// bypasses the gate and is WARN-logged for audit.
package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"gardenio/kbeads/internal/model"
)

// dedupAckErrorCode is the stable machine-readable code embedded in the
// rejection body. Distinct from operator_refusal (a 400 attribution failure)
// — this is a 409 precondition the caller can satisfy by re-submitting with
// the acknowledged hash. The kd CLI (T2) keys on this code to render the open
// list and compose the retry.
const dedupAckErrorCode = "dedup_ack_required"

// dedupAckListCap bounds the open list embedded in the rejection body so a
// project with hundreds of open decisions does not produce a multi-megabyte
// response. When the true open count exceeds the cap the body carries the cap
// most-RECENT entries plus Truncated=true and TotalOpen=<n>. The HASH is
// always computed over the FULL open list (never the truncated view) so the
// ack token still round-trips deterministically. §9 of the design.
const dedupAckListCap = 100

// dedupAckReplayBypassKey marks a context whose createBead call is an
// escalation replay (§9): the dedup-ack gate is skipped because the operator's
// approval of the escalation already constitutes the "look at the open list".
// Scoped to the dedup gate alone so the pre-existing ack-only decision guard
// is unaffected. Distinct unexported type avoids context-key collisions.
type dedupAckReplayBypassKeyType struct{}

var dedupAckReplayBypassKey = dedupAckReplayBypassKeyType{}

// withDedupAckReplayBypass returns a context that opts its createBead call out
// of the dedup-ack gate (escalation replay path, §9).
func withDedupAckReplayBypass(ctx context.Context) context.Context {
	return context.WithValue(ctx, dedupAckReplayBypassKey, true)
}

// dedupAckReplayBypass reports whether ctx is an escalation-replay context.
func dedupAckReplayBypass(ctx context.Context) bool {
	v, _ := ctx.Value(dedupAckReplayBypassKey).(bool)
	return v
}

// dedupAckOpenBead is one entry in the rejection's open list. AgeSeconds is
// convenience-only (helps a human triage which duplicate is stale) and is NOT
// part of the hash — a bead's age changes every second and would make the
// token un-echoable.
type dedupAckOpenBead struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	AgeSeconds int64  `json:"age_seconds"`
}

// dedupAckError is the structured rejection returned when the gate fires. The
// HTTP transport serializes it as a 409 body; gRPC maps it to
// FailedPrecondition. It carries everything the caller needs to look and
// retry: the readable open list, the opaque hash, and a copy-pasteable hint.
type dedupAckError struct {
	BeadType  string             `json:"bead_type"`
	Project   string             `json:"project"`
	Hash      string             `json:"hash"`
	Open      []dedupAckOpenBead `json:"open"`
	Truncated bool               `json:"truncated"`
	TotalOpen int                `json:"total_open"`
	RetryWith string             `json:"retry_with"`
}

// Error implements error. The message is human-first; machine consumers key
// on Code() / the structured fields, never on this string.
func (e *dedupAckError) Error() string {
	return fmt.Sprintf(
		"%d open %s(s) already exist in project %q that you have not acknowledged — "+
			"re-submit with %s to confirm you looked at the existing list first",
		e.TotalOpen, e.BeadType, e.Project, e.RetryWith,
	)
}

// Code returns the stable machine-readable code, for the transport layers.
func (e *dedupAckError) Code() string { return dedupAckErrorCode }

// dedupAckGateEnabled reports whether the gate is active. Default OFF: the
// gate ships dark and is flipped per the rollout plan (§13) once gardener
// prompts teach the retry-with-hash step. Mirrors authoringHumanGateEnabled.
func dedupAckGateEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("BEADS_DEDUP_GATE_ENABLED"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// dedupAckGateProjects returns the optional per-project allowlist. When
// BEADS_DEDUP_GATE_PROJECTS is set (comma-separated project names), the gate
// applies ONLY to those projects even when globally enabled — the staged
// rollout knob (§13). An empty/unset value means "all projects" (subject to
// the global enable flag). Returns nil when unset.
func dedupAckGateProjects() map[string]bool {
	raw := strings.TrimSpace(os.Getenv("BEADS_DEDUP_GATE_PROJECTS"))
	if raw == "" {
		return nil
	}
	out := make(map[string]bool)
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out[p] = true
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// computeOpenListHash returns the acknowledgement token for a set of open
// beads: sha256 over sorted "<id>\t<title>\n" lines, hex-encoded. Title
// stability is sufficient (§2) — title changes are rare, whereas status churn
// is exactly what we want the caller to look at, not what should force a
// re-ack. Deterministic regardless of the store's return order because we
// sort by id first. The list passed here is always the FULL open list, never
// the display-capped view.
func computeOpenListHash(beads []*model.Bead) string {
	ids := make([]string, len(beads))
	byID := make(map[string]string, len(beads))
	for i, b := range beads {
		ids[i] = b.ID
		byID[b.ID] = b.Title
	}
	sort.Strings(ids)
	h := sha256.New()
	for _, id := range ids {
		fmt.Fprintf(h, "%s\t%s\n", id, byID[id])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// resolveDedupProject returns the project name for the bead being created,
// preferring an explicit project:<name> label and falling back to the
// fields.project value. Returns "" when neither resolves — the gate SKIPS
// project-less beads (§7): cross-fleet/infra beads have no queue to dedup
// against and synthesizing a "global" bucket would falsely collide unrelated
// work.
func resolveDedupProject(bead *model.Bead) string {
	if p := projectFromLabels(bead.Labels); p != "" && p != "unknown" {
		return p
	}
	return projectFromFields(bead.Fields)
}

// runDedupAckGate is the createBead entry point for the dedup-ack gate. It
// resolves the acknowledgement hash (dedicated input field, falling back to
// fields.saw_open_<type> for self-composed retries), honors Force as a
// WARN-logged bypass, and delegates to applyDedupAckGate. Called for both
// decision and escalation creates.
func (s *BeadsServer) runDedupAckGate(ctx context.Context, bead *model.Bead, in createBeadInput, beadType model.BeadType) error {
	if dedupAckReplayBypass(ctx) {
		// §9: escalation replay opts out entirely. No WARN — this is an
		// expected, operator-approved path, not a user bypassing the gate.
		return nil
	}
	if in.Force {
		// Force (CLI --force, escalation replay context) always bypasses. Log
		// at WARN only when the gate WOULD have fired had it not been bypassed,
		// so audit noise stays proportional to real bypasses (§11). The gate's
		// enable/project checks are cheap; the store list only runs when the
		// gate is enabled for this project.
		if dedupAckGateEnabled() {
			slog.Warn("dedup-ack gate bypassed via force",
				"actor", actorFromContext(ctx),
				"bead_type", string(beadType),
				"project", resolveDedupProject(bead),
				"bead", "ka-0000004599",
			)
		}
		return nil
	}
	return s.applyDedupAckGate(ctx, bead, in.SawOpenHash, beadType)
}

// applyDedupAckGate runs the dedup-ack gate for a decision/escalation bead
// being created. It returns:
//   - nil when the gate is disabled, the bead has no project, there are no
//     open beads of its type, or the caller echoed the current hash.
//   - *dedupAckError (a 409-class rejection) when open beads exist and the
//     caller did not acknowledge the current snapshot.
//
// Force is handled by the caller (createBead) — this function is only reached
// when Force is false. Each rejection logs at INFO so gate effectiveness is
// measurable (§11).
func (s *BeadsServer) applyDedupAckGate(ctx context.Context, bead *model.Bead, sawHash string, beadType model.BeadType) error {
	if !dedupAckGateEnabled() {
		return nil
	}

	project := resolveDedupProject(bead)
	if project == "" {
		// §7: no project → no queue to dedup against → skip.
		return nil
	}
	if allow := dedupAckGateProjects(); allow != nil && !allow[project] {
		// Per-project allowlist set and this project is not on it.
		return nil
	}

	// Fetch the open beads of the SAME type in the SAME project. Cross-type is
	// deliberate (§6): decisions and escalations are independent operator
	// surfaces. Fleet is scoped to the caller's fleet (the default ListBeads
	// behavior) — a decision in another fleet is a different queue.
	filter := model.BeadFilter{
		Type:   []model.BeadType{beadType},
		Status: []model.Status{model.StatusOpen, model.StatusInProgress},
		Labels: []string{"project:" + project},
		Fleet:  FleetFromContext(ctx),
		Sort:   "-created_at",
	}
	open, _, err := s.store.ListBeads(ctx, filter)
	if err != nil {
		return fmt.Errorf("dedup-ack gate: failed to list open %s beads: %w", beadType, err)
	}
	if len(open) == 0 {
		// §14 empty-list scenario: no ack required when the queue is empty.
		return nil
	}

	hash := computeOpenListHash(open)
	if sawHash != "" && strings.EqualFold(strings.TrimSpace(sawHash), hash) {
		// Caller looked at the current snapshot → let the create proceed.
		return nil
	}

	actor := actorFromContext(ctx)
	slog.Info("dedup-ack gate rejected create",
		"actor", actor,
		"bead_type", string(beadType),
		"project", project,
		"open_count", len(open),
		"had_stale_ack", sawHash != "",
		"bead", "ka-0000004599",
	)

	return buildDedupAckError(beadType, project, hash, open)
}

// buildDedupAckError assembles the structured rejection. The open list is
// capped for display (§9) but the hash was already computed over the full
// list by the caller.
func buildDedupAckError(beadType model.BeadType, project, hash string, open []*model.Bead) *dedupAckError {
	now := time.Now().UTC()
	display := open
	truncated := false
	if len(display) > dedupAckListCap {
		display = display[:dedupAckListCap]
		truncated = true
	}
	entries := make([]dedupAckOpenBead, len(display))
	for i, b := range display {
		age := int64(now.Sub(b.CreatedAt).Seconds())
		if age < 0 {
			age = 0
		}
		entries[i] = dedupAckOpenBead{ID: b.ID, Title: b.Title, AgeSeconds: age}
	}
	return &dedupAckError{
		BeadType:  string(beadType),
		Project:   project,
		Hash:      hash,
		Open:      entries,
		Truncated: truncated,
		TotalOpen: len(open),
		// CLI flag renders with hyphens (--saw-open-decisions); the request
		// FIELD is the snake_case saw_open_<type>. §16 wire format.
		RetryWith: fmt.Sprintf("--saw-open-%s=%s", dedupAckFlagSuffix(beadType), hash),
	}
}

// dedupAckFlagSuffix maps a bead type to its plural flag suffix:
// decision → "decisions", escalation → "escalations".
func dedupAckFlagSuffix(beadType model.BeadType) string {
	return string(beadType) + "s"
}
