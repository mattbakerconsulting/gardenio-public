package main

// init_s3_restore.go — S3 session restore integration for gb init.
//
// With sessions keyed by agent bead ID (ka-0000002795), coop downloads
// prior session state from s3://<bucket>/<agent-bead-id>/ on every start.
// gb init only needs to wire COOP_S3_SESSION_KEY=<agent-bead-id> on the
// coop process; coop owns the resume decision end-to-end.
//
// The helpers here are now narrow: agentS3SessionKey returns the bead ID
// used as the S3 key, and fetchLastSessionID/sessionIDPattern are retained
// for stdout-tail uploads (init_stdout_tail.go) which still address by
// session UUID.

import (
	"context"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
)

// sessionIDPattern matches a Claude/coop session UUID. Used to reject the
// "session" sentinel that a buggy prepare_resume once produced.
var sessionIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// resumeS3KeyField is the field on a TASK bead that carries the canonical S3
// session key across an involuntary crew respawn. The dying pod writes it (see
// reopenOrphanedClaimedTask, hook.go); the next pod reads it in
// resolveCanonicalS3Key at boot. Kept as a named constant so the writer and
// reader can never drift.
const resumeS3KeyField = "resume_s3_key"

// canonicalS3Key holds the resolved S3 session key for this pod once
// resolveCanonicalS3Key has run. nil until resolved; a non-nil pointer to ""
// is never stored (resolution only sets it to a non-empty inherited key).
// Read by agentS3SessionKey so every S3 caller in the pod shares one key.
var canonicalS3Key atomic.Pointer[string]

// resolveCanonicalS3Key looks up the canonical S3 session key for this pod and,
// when it differs from the pod's own bead ID, adopts it: it memoizes the key
// for agentS3SessionKey AND stamps s3_session_key on the pod's own agent bead
// so a transcript lookup keyed on the NEW bead ID (deathtranscript
// SessionKeyForBead, the beads-viewer/forensics path) resolves to the same
// canonical prefix.
//
// The canonical key lives on the worked TASK bead (resume_s3_key), the one
// identity stable across an involuntary crew respawn. It is set by the dying
// pod's reopenOrphanedClaimedTask. Resolution order:
//
//   - No task in scope, no daemon, no bucket, or task has no resume_s3_key →
//     leave canonicalS3Key nil so agentS3SessionKey falls back to the own bead
//     ID (fresh spawn / first life — unchanged behavior).
//   - resume_s3_key present and != own bead ID → adopt it (the respawn case).
//   - resume_s3_key present and == own bead ID → no-op (already canonical).
//
// Best-effort and idempotent: any daemon error is logged and leaves the pod on
// its own-bead-id key (a cold start, i.e. today's behavior — never worse). Must
// run BEFORE buildCoopEnv wires COOP_S3_SESSION_KEY and before the first
// registerSessionBead resume probe.
func resolveCanonicalS3Key(ctx context.Context) {
	if daemon == nil || os.Getenv("COOP_S3_BUCKET") == "" {
		return
	}
	taskID := os.Getenv("BOAT_TASK_ID")
	if taskID == "" {
		return
	}
	own := agentOwnBeadID()
	task, err := daemon.GetBead(ctx, taskID)
	if err != nil || task == nil {
		if err != nil {
			logInit("resolveCanonicalS3Key: could not read task %s: %v (cold-starting on own bead id)", taskID, err)
		}
		return
	}
	key := task.Fields[resumeS3KeyField]
	if key == "" || key == own {
		return
	}
	canonicalS3Key.Store(&key)
	logInit("resolveCanonicalS3Key: adopting canonical S3 key %s from task %s (own bead %s) — resuming prior transcript chain", key, taskID, own)

	// Point transcript lookups keyed on THIS (new) bead id at the canonical
	// prefix. Best-effort: a write failure only degrades viewer/forensics
	// lookup, not the resume itself (coop already uses the key via
	// agentS3SessionKey → COOP_S3_SESSION_KEY).
	if own != "" {
		if err := daemon.UpdateBeadFields(ctx, own, map[string]string{"s3_session_key": key}); err != nil {
			logInit("resolveCanonicalS3Key: failed to stamp s3_session_key=%s on own bead %s: %v", key, own, err)
		}
	}

	// Consume the breadcrumb: clear resume_s3_key from the task now that this
	// pod owns the chain. This is what keeps a CLEAN completion from leaving a
	// stale resume pointer — if this pod finishes the task with `gb done`, the
	// task closes with no resume_s3_key, so a later unrelated reopen of the same
	// bead starts fresh instead of resurrecting a finished transcript. The chain
	// still survives repeated involuntary kills: reopenOrphanedClaimedTask
	// re-stamps the (memoized) canonical key on every death, so the NEXT respawn
	// re-adopts it. Best-effort — a failure only risks a stale pointer that the
	// re-stamp would overwrite anyway.
	if err := daemon.UpdateBeadFields(ctx, taskID, map[string]string{resumeS3KeyField: ""}); err != nil {
		logInit("resolveCanonicalS3Key: failed to clear resume_s3_key on task %s: %v", taskID, err)
	}
}

// currentS3SessionKeyForTask returns the S3 session key THIS pod is using, for
// the dying-pod writer (reopenOrphanedClaimedTask) to persist onto the task
// bead. It is the canonical key when one was inherited (so a chain of N
// respawns all point at the ORIGINAL prefix, never a moving target), else the
// pod's own bead ID. Returns "" when there is no bead in scope.
//
// It must be correct in TWO processes: the in-pod restart-loop teardown (same
// process as gb init, where the canonicalS3Key memo is set) AND the separate
// `gb` Claude-Stop-hook subprocess (where resolveCanonicalS3Key never ran, so
// the memo is nil). Relying on the memo alone would make a RESUMED pod that
// dies via the hook path stamp its OWN (new) bead id — but coop uploaded that
// life's transcript under the CANONICAL key, so the successor would resume an
// empty prefix and cold-start. Resolution, most-authoritative first:
//
//  1. the in-memory canonicalS3Key memo (init process fast path);
//  2. the persisted s3_session_key field on the pod's OWN agent bead, which
//     resolveCanonicalS3Key stamped at boot when it adopted a canonical key
//     (this is what makes the hook process correct);
//  3. the pod's own bead id (a fresh, never-resumed pod — its transcript IS
//     under its own bead id).
func currentS3SessionKeyForTask(ctx context.Context) string {
	if k := canonicalS3Key.Load(); k != nil && *k != "" {
		return *k
	}
	own := agentOwnBeadID()
	if own != "" && daemon != nil {
		if bead, err := daemon.GetBead(ctx, own); err == nil && bead != nil {
			if persisted := bead.Fields["s3_session_key"]; persisted != "" {
				return persisted
			}
		}
	}
	return own
}

// agentOwnBeadID returns THIS pod's own agent bead ID — the value the
// controller stamped into KD_AGENT_ID / BOAT_AGENT_BEAD_ID at pod-spec time
// (podmanager manager.go:1704/1707). It is a NEW id on every respawn, so it
// is the right key for live-bead field ops (last_session_id read/write) that
// address the agent's OWN bead, but the WRONG key for the S3 session prefix
// after an involuntary respawn — see agentS3SessionKey.
func agentOwnBeadID() string {
	return envOr("KD_AGENT_ID", os.Getenv("BOAT_AGENT_BEAD_ID"))
}

// agentS3SessionKey returns the S3 session-prefix component coop should
// upload/download under, so a session belongs to an agent rather than a pod.
//
// Normally this is the pod's own agent bead ID (agentOwnBeadID). But a crew
// agent that was respawned after an involuntary kill mints a BRAND-NEW bead
// ID every time (the respawn is gardener-LLM-driven — `gb spawn --task <id>`
// — so nothing carries the old id forward), while the prior transcript sits
// in S3 under the ORIGINAL bead ID. Keying coop on the new id probes an empty
// prefix (archive-empty) and the agent cold-starts, orphaning the transcript
// (ka-0000003160).
//
// The one field that IS stable across the respawn is the worked task's bead
// ID (BOAT_TASK_ID = the task_id field, identical on old and new agent beads,
// because the gardener re-spawns with the same --task). resolveCanonicalS3Key
// records the S3 key of the dying pod onto that task bead (resume_s3_key) so
// the next pod can follow one hop back to the canonical, non-moving prefix.
// When such a key is present it OVERRIDES the own-bead-id key here; otherwise
// we fall back to the own bead ID (fresh spawn / no prior life).
//
// The result is memoized on first resolution so every caller in a single pod
// (buildCoopEnv, salvage, stdout-tail, resume-check) agrees on one key.
func agentS3SessionKey() string {
	if k := canonicalS3Key.Load(); k != nil {
		return *k
	}
	return agentOwnBeadID()
}

// s3ResumeIntended reports whether gb init has both an S3 bucket and an
// agent-scoped session key — the only configuration in which coop will
// attempt to download a prior session for --resume. When this is false,
// gb init has effectively decided "fresh": no S3 hand-off is possible and
// any local PVC .jsonl is just leftover state that would trigger Claude
// Code's own auto-resume picker. See ka-0000004439.
func s3ResumeIntended() bool {
	return os.Getenv("COOP_S3_BUCKET") != "" && agentS3SessionKey() != ""
}

// warmResumeS3KeyField is the field the pool stamps on a warm-assigned agent
// bead (poolmanager AssignPrewarmed) carrying the prior thread agent's S3
// session key — the transcript this warm follow-up should resume. Kept as a
// named constant so the controller writer and this pod-side reader can never
// drift. See ka-0000000983 and the warm-pool coop-resume graft.
const warmResumeS3KeyField = "warm_resume_s3_key"

// maybeAdoptWarmResumeKey implements the pod side of the warm-pool coop-resume
// graft (ka-0000000983). A prewarmed pool pod boots coop under its OWN empty S3
// prefix, so a warm follow-up would start a fresh session with no prior
// transcript. When the pool assigns this pod to a thread that has a prior
// transcript, it stamps warm_resume_s3_key on this bead; adopting that key as
// the canonical S3 prefix and relaunching coop makes the follow-up resume the
// real conversation on the already-warm pod.
//
// Returns true iff coop must be relaunched to pick up a newly-adopted key:
//   - the bead carries a non-empty warm_resume_s3_key, AND
//   - an S3 bucket is configured (else COOP_S3_SESSION_KEY is never wired, so a
//     relaunch would resume nothing), AND
//   - that key is not ALREADY the adopted canonical key (idempotency: on the
//     post-relaunch launch canonicalS3Key already equals the target, so this
//     returns false and the session proceeds to injectPrompt normally).
//
// On a true return it stores the key in canonicalS3Key so the next
// buildCoopEnv wires COOP_S3_SESSION_KEY=<prior transcript prefix>. Best-effort:
// a nil daemon, missing bead, or absent field is a quiet no-op (false), leaving
// today's fresh-warm-session behavior intact.
func maybeAdoptWarmResumeKey(ctx context.Context, beadID string) bool {
	if daemon == nil || beadID == "" {
		return false
	}
	if os.Getenv("COOP_S3_BUCKET") == "" {
		return false
	}
	bead, err := daemon.GetBead(ctx, beadID)
	if err != nil || bead == nil {
		return false
	}
	key := strings.TrimSpace(bead.Fields[warmResumeS3KeyField])
	if key == "" {
		return false
	}
	// Idempotency: on the relaunched (resuming) session the key is already the
	// canonical prefix, so do NOT relaunch again — proceed to injectPrompt.
	if cur := canonicalS3Key.Load(); cur != nil && *cur == key {
		return false
	}
	canonicalS3Key.Store(&key)

	// Continue the transcript chain: coop will upload THIS session's transcript
	// under <key>/, not under this pod's own bead id. Stamp s3_session_key on the
	// pod's own bead so (a) a later respawn's resolveWarmResumeS3Key finds the
	// continued prefix via the field rather than a now-empty own-id prefix, and
	// (b) transcript/forensics lookups keyed on this bead id resolve to the same
	// prefix — exactly the stamp resolveCanonicalS3Key performs on adoption.
	// Best-effort: a write failure only degrades the NEXT respawn's chaining and
	// viewer lookup, never this session's resume (coop already uses the key via
	// canonicalS3Key → agentS3SessionKey → COOP_S3_SESSION_KEY).
	if own := agentOwnBeadID(); own != "" && own != key {
		if err := daemon.UpdateBeadFields(ctx, own, map[string]string{"s3_session_key": key}); err != nil {
			logInit("maybeAdoptWarmResumeKey: failed to stamp s3_session_key=%s on own bead %s: %v", key, own, err)
		}
	}

	logInit("maybeAdoptWarmResumeKey: adopting warm-pool resume key %s (bead %s) — relaunching coop to resume prior transcript", key, beadID)
	return true
}

// fetchLastSessionID reads last_session_id from the agent bead. Used by
// stdout-tail uploads to address the tail under a stable session UUID
// across restarts. Returns "" when daemon is unreachable, the bead has no
// such field, or no agent bead ID is in scope.
func fetchLastSessionID(ctx context.Context) string {
	if daemon == nil {
		return ""
	}
	// last_session_id is a live field on the pod's OWN agent bead, not on the
	// canonical-S3-key bead — address it by the own bead ID so a respawn that
	// inherited a canonical key (agentS3SessionKey) does not misread it off the
	// original agent's bead.
	beadID := agentOwnBeadID()
	if beadID == "" {
		return ""
	}
	bead, err := daemon.GetBead(ctx, beadID)
	if err != nil || bead == nil {
		return ""
	}
	return bead.Fields["last_session_id"]
}

// stampLastSessionID writes the live coop session UUID onto the agent bead's
// last_session_id field (ka-0000000352). Nothing else in the fleet wrote this
// field — it was declared in the bead schema and READ by the beads-viewer (to
// nudge a live session via coopmux, render the live PTY, and forward operator
// input) but never populated, so every live-nudge fell through to the
// wake/spawn path and messages typed into a RUNNING agent (notably the /plans
// assistant chat) were silently dropped. Stamping it here — the first moment
// gb init learns the session UUID from the coop API — closes that gap for
// every agent pod.
//
// Best-effort: a nil daemon, no agent bead in scope, or a write error is
// swallowed (logged, not fatal) — the session runs fine without the field; the
// only cost is the live-nudge downgrade this fix removes. No-op when sid is
// empty. Idempotent: writing the same UUID again is harmless, so a restart that
// re-resolves the same session just re-affirms it.
func stampLastSessionID(ctx context.Context, sid string) {
	if daemon == nil || sid == "" {
		return
	}
	// Write to the pod's OWN agent bead (the beads-viewer live-nudge reads
	// last_session_id off the live agent bead it is rendering), not the
	// canonical-S3-key bead a respawn may have inherited.
	beadID := agentOwnBeadID()
	if beadID == "" {
		return
	}
	if err := daemon.UpdateBeadFields(ctx, beadID, map[string]string{"last_session_id": sid}); err != nil {
		logInit("stampLastSessionID: failed to write last_session_id=%s on %s: %v", sid, beadID, err)
	}
}
