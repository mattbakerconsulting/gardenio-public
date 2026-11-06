// Package bridge — escalation "MR-landed, apply-pending" review flag
// (ka-0000002563).
//
// This is the third escalation detector, the COMPLEMENT to the auto-reaper
// (escalation_reaper.go). The reaper auto-CLOSES two kinds of escalation: a
// pure approve/merge ask whose MR merged (verb-allowlist), and one the operator
// resolved out-of-band on the bead (orchestration witness). It deliberately
// refuses to close everything else — most importantly the "merge THEN apply"
// shape, where the referenced MR landing is only step 1 of an N-step ask and a
// genuine apply/mint/deploy step still stands (ka-0000001987, ka-0000000449,
// ka-0000005964, ka-0000007362). Auto-closing those would silently drop a live
// P0, so under-closing is the load-bearing safe direction.
//
// But under-closing left a real gap the deep-dive measured (bundle
// ka-0000002006): NOTHING tells the operator the code half landed. The MR gets
// merged in the GitLab UI, no reverse event reaches the bead, and the pingback
// loop (escalation_pingback_loop.go) keeps re-notifying the STALE full ask
// forever — 28 wasted Slack nags across 8 stale escalations, one nagged 19
// times. The escalation text still says "approve + merge MR !N" long after !N
// merged.
//
// The review flag closes that gap WITHOUT closing the bead. When a non-reapable
// escalation's referenced MR is now merged (or closed/declined), the flag flips
// the bead ONCE into a needs-review posture:
//
//   - a one-shot audit comment: "MR !N is merged; the apply/mint step this
//     escalation also asks for may still be outstanding — verify the acceptance
//     criterion, then close (or leave open if the apply is still pending)";
//   - mr_landed / mr_landed_state / mr_landed_iid / mr_landed_at fields, the
//     idempotency latch — a flagged bead is never re-flagged, and the pingback
//     loop skips a bead carrying mr_landed=true, so the stale-text nag stops;
//   - a `needs-review` label so it surfaces distinctly on the operator queue;
//   - ONE review-specific pingback request (the same requested/notified
//     handshake the SSE watcher honors), so the operator gets exactly one
//     actionable "the code half is done, decide the rest" signal instead of the
//     endless stale re-notifies.
//
// WHY THIS NEVER CLOSES (the same principle as the reaper's verb guard):
//   - A merged MR does NOT imply the escalation is done. The flag makes no claim
//     about the apply step's acceptance — it only reports the MR fact and hands
//     the decision to a human. Closing would be the false-reap the reaper's
//     package doc warns against.
//   - It fires ONLY on a merged/closed MR the flag resolved and re-verified live
//     against GitLab. A read error, a 404, or a still-`opened` MR is a no-op, so
//     the flag can never manufacture a review state from its own stale read
//     (advice ka-0000000519).
//
// ORDER: flagMRLandedApplyPending runs in evaluateBead AFTER both reap detectors
// decline. If the reaper would auto-close (pure approve/merge, MR merged), that
// wins and the bead is gone before the flag looks at it — the flag only ever
// sees the asks the reaper leaves open. This keeps the two detectors from
// fighting: reap when safe to close, flag-for-review when not.

package bridge

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

const (
	// reviewFlagActor attributes the flag's audit comment and field writes.
	reviewFlagActor = "escalation-review-flag"

	// needsReviewLabel is stamped on a flagged escalation so the operator queue
	// can surface "the MR landed, verify the rest" distinctly from an untouched
	// stalled ask.
	needsReviewLabel = "needs-review"
)

// mr_landed* fields record the needs-review flip. mrLandedField is the
// idempotency latch: once "true", the escalation has already been flipped and
// neither the flag nor the pingback loop touches it again (no re-comment, no
// re-nag). Its companions record which MR / state / when.
const (
	mrLandedField      = "mr_landed"
	mrLandedStateField = "mr_landed_state"
	mrLandedIIDField   = "mr_landed_iid"
	mrLandedAtField    = "mr_landed_at"
	mrLandedURLField   = "mr_landed_url"
)

// mrBangRefPattern matches a GitLab bang-reference to an MR — the human shape
// the real escalations use in their title / operation_spec, e.g.
// "example-org/site-deployment!1168" or a bare "!6218". Group 1 is the
// optional project path, group 2 the IID. A bare "!N" yields project="".
var mrBangRefPattern = regexp.MustCompile(`([A-Za-z0-9._/-]+)?!(\d+)\b`)

// resolveReviewMRRef finds the MR an apply/merge escalation references, using a
// BROADER net than the reaper's resolveMRRef because the real merge-then-apply
// beads rarely populate the tidy structured fields the reaper expects. It tries,
// in order:
//
//  1. a full merge_requests/<iid> URL in a structured field, title, or
//     description (host + path + iid — the only shape that carries a host);
//  2. a structured numeric iid (iid / mr_iid / merge_request_iid /
//     merge_request / mr) paired with a project path (project / mr_project /
//     project_path / repo);
//  3. a `project!iid` or bare `!iid` bang-reference in the title / description /
//     operation_spec, paired with the structured project path when the bang had
//     none.
//
// Returns ok=false when no (project, iid) pair can be assembled — the flag then
// leaves the escalation untouched. Because the flag NEVER closes, a
// mis-resolved ref is benign: the live re-verify GET simply finds no merged MR
// and the flag no-ops.
func resolveReviewMRRef(spec map[string]any, title, description, rawSpec string) (reapRef, bool) {
	// 1. A full MR URL anywhere → host + path + iid.
	for _, s := range []string{
		stringField(spec, "mr_url"),
		stringField(spec, "merge_request_url"),
		title,
		description,
		rawSpec,
	} {
		if u := mrURLInText.FindString(s); u != "" {
			if m := ParseMRURL(u); m != nil {
				return reapRef{Host: m.Host, ProjectPath: m.ProjectPath, IID: m.IID}, true
			}
		}
	}

	proj := firstStringField(spec, "project", "mr_project", "project_path", "repo")

	// 2. A structured numeric iid + a project path.
	if iid := firstIntField(spec, "iid", "mr_iid", "merge_request_iid", "merge_request", "mr"); iid > 0 && proj != "" {
		return reapRef{ProjectPath: proj, IID: iid}, true
	}

	// 3. A bang-reference (project!iid or bare !iid) in prose or the raw spec.
	if bangProj, bangIID, ok := parseMRBangRef(title, description, rawSpec); ok {
		p := bangProj
		if p == "" {
			p = proj
		}
		if p != "" {
			return reapRef{ProjectPath: p, IID: bangIID}, true
		}
	}

	return reapRef{}, false
}

// parseMRBangRef returns the first `project!iid` (or bare `!iid`) bang-reference
// found across the given strings. project is "" for a bare "!iid". The project
// capture is only kept when it looks like a GitLab path (contains a '/'), so a
// stray "foo!5" in prose does not masquerade as a project — a bare "!5" is still
// accepted and paired with the structured project by the caller.
func parseMRBangRef(sources ...string) (project string, iid int, ok bool) {
	for _, s := range sources {
		for _, m := range mrBangRefPattern.FindAllStringSubmatch(s, -1) {
			n, err := strconv.Atoi(m[2])
			if err != nil || n <= 0 {
				continue
			}
			p := m[1]
			if p != "" && !strings.Contains(p, "/") {
				p = ""
			}
			return p, n, true
		}
	}
	return "", 0, false
}

// flagMRLandedApplyPending flips a non-reapable escalation into a needs-review
// posture when its referenced MR is now merged or closed. It is the last
// detector evaluateBead runs, and it NEVER closes the bead — it only records
// that the MR landed, labels the bead for review, and requests exactly one
// review pingback. Returns true when it acted (or would have, in dry-run).
func (r *EscalationReaper) flagMRLandedApplyPending(ctx context.Context, b *beadsapi.BeadDetail) bool {
	if !r.reviewFlag {
		return false
	}
	fields := b.Fields
	if fields == nil {
		fields = map[string]string{}
	}

	// Idempotency latch: a bead already flipped to needs-review is never touched
	// again — that is what stops the stale-text nag.
	if fields[mrLandedField] == "true" {
		return false
	}

	rawSpec := fields["operation_spec"]
	spec := parseOperationSpec(rawSpec)
	ref, ok := resolveReviewMRRef(spec, b.Title, b.Description, rawSpec)
	if !ok {
		return false
	}

	// Dual-GitLab safety: never GET a foreign-host MR against this bridge's API
	// (mirrors the reaper). Only applies when the ref carries a host.
	if r.host != "" && ref.Host != "" && ref.Host != r.host {
		return false
	}

	// Re-verify live: the MR must actually be merged/closed RIGHT NOW. A
	// still-opened MR, a 404, or a transport error is a no-op — the flag never
	// acts on its own stale read.
	mr, err := r.gitlab.GetMergeRequestByPath(ctx, ref.ProjectPath, ref.IID)
	if err != nil {
		r.logger.Debug("escalation review-flag: MR fetch failed — not flagging this sweep",
			"id", b.ID, "project", ref.ProjectPath, "iid", ref.IID, "error", err)
		return false
	}
	if mr.State != "merged" && mr.State != "closed" {
		return false
	}

	mrURL := mr.WebURL
	if mrURL == "" {
		mrURL = mrURLFor(ref)
	}
	return r.applyReviewFlag(ctx, b, ref, mr.State, mrURL)
}

// applyReviewFlag performs the needs-review flip in one ATOMIC field write —
// the latch (mr_landed) and the ONE review-pingback request go together, so a
// partial failure can never latch the bead (silencing it forever) without also
// delivering the operator signal: if the write fails, nothing is stamped and the
// next sweep retries the whole flip cleanly. The audit comment and needs-review
// label are best-effort decorations AFTER the commit point (a missing comment
// still leaves the machine-readable mr_landed_* fields and the pingback, so the
// operator is never left without a signal).
func (r *EscalationReaper) applyReviewFlag(ctx context.Context, b *beadsapi.BeadDetail, ref reapRef, mrState, mrURL string) bool {
	nowStr := r.now().UTC().Format(time.RFC3339)

	if r.dryRun {
		r.logger.Info("escalation review-flag: WOULD flip to needs-review (dry-run)",
			"id", b.ID, "project", ref.ProjectPath, "iid", ref.IID, "mr_state", mrState)
		return true
	}

	// One atomic patch: the mr_landed latch + companions AND the review-pingback
	// request. Writing mr_landed=true is what makes the flip idempotent and —
	// because both the review flag and the auto-backoff pingback loop skip
	// mr_landed beads — what pauses the stale re-notify. The pingback fields use
	// the same requested/notified handshake the SSE watcher
	// (escalations.go maybeNotifyPingback) honors, so the operator gets exactly
	// one actionable signal on the escalation card; no further nags follow.
	total := 0
	if v := b.Fields["pingback_count"]; v != "" {
		total, _ = strconv.Atoi(v)
	}
	flip := map[string]string{
		mrLandedField:      "true",
		mrLandedStateField: mrState,
		mrLandedIIDField:   strconv.Itoa(ref.IID),
		mrLandedAtField:    nowStr,

		"pingback_requested_at": nowStr,
		"pingback_source":       "review-flag",
		"pingback_by":           reviewFlagActor,
		"pingback_count":        strconv.Itoa(total + 1),
		"pingback_reason": fmt.Sprintf(
			"MR !%d is %s — the apply/mint step may still be outstanding; verify acceptance then close",
			ref.IID, mrState),
	}
	if mrURL != "" {
		flip[mrLandedURLField] = mrURL
	}
	if err := r.daemon.UpdateBeadFields(ctx, b.ID, flip); err != nil {
		r.logger.Error("escalation review-flag: field stamp failed — leaving bead untouched for retry",
			"id", b.ID, "error", err)
		return false
	}

	// Best-effort audit comment (the fields above are the durable record).
	verb := "merged"
	if mrState == "closed" {
		verb = "closed/declined"
	}
	comment := fmt.Sprintf(
		"🔎 Flagged for review by %s: MR !%d is now `%s` (%s), but this escalation was NOT "+
			"auto-closed. A merged MR often satisfies only the code half of a merge-then-apply ask — "+
			"the apply / mint / deploy step this escalation also requests may still be outstanding "+
			"(the ka-0000000449 lesson: prod terraform can run and still not create the resource). "+
			"Verify the escalation's OWN acceptance criterion, then close if done — or leave it open "+
			"if the apply is still pending. Re-notification is paused until then, so this replaces the "+
			"stale 'approve + merge' nag with one actionable signal (ka-0000002563).",
		reviewFlagActor, ref.IID, mrState, verb)
	if mrURL != "" {
		comment += "\nMR: " + mrURL
	}
	if err := r.daemon.AddComment(ctx, b.ID, reviewFlagActor, comment); err != nil {
		r.logger.Warn("escalation review-flag: audit comment failed (fields + pingback already stamped)",
			"id", b.ID, "error", err)
	}

	// Surface on the operator queue as a review item. A label add is idempotent
	// server-side, so a re-run is harmless.
	if err := r.daemon.AddLabel(ctx, b.ID, needsReviewLabel); err != nil {
		r.logger.Warn("escalation review-flag: label add failed",
			"id", b.ID, "error", err)
	}

	r.logger.Info("escalation flagged for review: MR landed, apply may be pending",
		"id", b.ID, "project", ref.ProjectPath, "iid", ref.IID, "mr_state", mrState)
	return true
}
