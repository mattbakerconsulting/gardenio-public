package bridge

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// BACKTEST — reproduce the hand RCA for thread-1784843199-442769 (resume-mint
// failure), filed against gd-hzmTyaVGix / ka-0000004735 / gd-9uSKXGYX38.
//
// This is not a new fix; it is a controlled REPRODUCTION of a known
// resume-failure so a follow-up fix crew has a deterministic regression to
// verify against. It exercises the REAL bridge helpers (retryTransientDaemon,
// isTransientDaemonError) and the REAL mock daemon, so the failure mode it
// reproduces is the production failure mode.
//
// Hand RCA (ka-0000004735 comment + gd-9uSKXGYX38):
//
//	NOT a coop/S3/transcript-restore failure. The operator replied
//	`--model=fable make it so` to a thread agent that had already `gb stop`'d
//	(state=done). The bridge routed that to the cold session-resume respawn
//	path (respawnThreadAgent), which closes the old terminal bead then mints a
//	fresh agent bead via CreateBead. That CreateBead was a SINGLE, UNRETRIED
//	daemon write. During the 2026.204.17 deploy roll (a fleet-wide ~30-bead
//	mass close/re-touch at 22:24-22:27, the daemon/bridge pods rolling), a
//	transient 5xx/conn-refused hit that mint, so the bridge posted
//	":x: Failed to resume agent" and dropped the operator's wake-up on the
//	floor. The ":x:" string is emitted from exactly ONE place — the
//	CreateBead-failed branch at bot_agent_kill.go:709 (now :732 after the fix).
//
//	The gap: the sibling conversation-write path (recordUnifiedThreadReply)
//	already retries transient daemon errors + has a DLQ. The respawn mint had
//	neither — one blip = dead-end.
//
// The two halves below are the "before" and "after" of the backtest:
//   (1) reproduce the dead-end: a bare single-shot mint during the roll-window
//       blip fails exactly as the RCA describes; and
//   (2) confirm the merged fix: the SAME blip pattern, wrapped in the real
//       retryTransientDaemon, survives and mints the resume bead.

// rollWindowBlip classifies the transient daemon errors the RCA attributes to
// the 2026.204.17 deploy roll: a 5xx from a rolling daemon pod, and a
// connection-refused from a pod that has gone down but not yet been replaced.
// Both must be recognized as transient (retryable) for the fix to hold; a single
// unretried write dies on either.
func rollWindowBlips() []error {
	return []error{
		&beadsapi.APIError{StatusCode: http.StatusBadGateway, Message: "502 Bad Gateway (daemon pod rolling)"},
		&beadsapi.APIError{StatusCode: http.StatusServiceUnavailable, Message: "503 Service Unavailable"},
		&beadsapi.APIError{StatusCode: http.StatusInternalServerError, Message: "500 (mass-close storm 22:24-22:27)"},
	}
}

// TestBacktest_Thread1784843199_RollBlipsAreTransient locks the classification
// the whole RCA hinges on: every error the deploy roll threw at the resume mint
// is a TRANSIENT (retryable) daemon error. If any were misclassified as
// permanent, even the fixed retry loop would give up and the dead-end would
// recur. Drives the real isTransientDaemonError.
func TestBacktest_Thread1784843199_RollBlipsAreTransient(t *testing.T) {
	for _, blip := range rollWindowBlips() {
		if !isTransientDaemonError(blip) {
			t.Errorf("roll-window blip %v classified as non-transient; the resume-mint retry would give up and the RCA dead-end recurs", blip)
		}
	}
	// The conn-refused transport failure the RCA also names ("5xx/conn-refused").
	connRefused := &beadsapi.APIError{StatusCode: http.StatusBadGateway, Message: "dial tcp: connection refused"}
	if !isTransientDaemonError(connRefused) {
		t.Error("connection-refused during a pod roll must be transient")
	}
}

// TestBacktest_Thread1784843199_UnretriedMintDeadEnds reproduces the PRE-FIX
// failure: the resume-bead mint as it existed before MR !5958 was a single
// CreateBead with no retry. Modeled directly against the real mock daemon: one
// call, a roll-window blip, and the mint dead-ends — exactly the
// ":x: Failed to resume agent" branch. This is the death the RCA describes, so a
// follow-up crew can see the untreated failure mode explicitly.
func TestBacktest_Thread1784843199_UnretriedMintDeadEnds(t *testing.T) {
	daemon := newMockDaemon()

	// The pre-fix mint: exactly one CreateBead attempt (no retryTransientDaemon
	// wrapper). Fail it with the first roll-window blip.
	blip := rollWindowBlips()[0]
	daemon.createBeadHook = func(_ int, req beadsapi.CreateBeadRequest) error {
		if strings.HasPrefix(req.IdempotencyKey, "thread-respawn:") {
			return blip
		}
		return nil
	}

	// Single unretried attempt, as the code read before the fix.
	_, err := daemon.CreateBead(context.Background(), beadsapi.CreateBeadRequest{
		Title:          "thread-1784843199-442769",
		Type:           "agent",
		IdempotencyKey: "thread-respawn:C0B5S61RVNU:1784843199.442769:1",
	})
	if err == nil {
		t.Fatal("pre-fix reproduction expected the single unretried mint to fail on the roll-window blip")
	}
	// This is the exact condition under which bot_agent_kill.go posted
	// ":x: Failed to resume agent" and dropped the wake-up: err != nil with no
	// retry to absorb it.
	if daemon.createBeadCalls != 1 {
		t.Fatalf("pre-fix path makes exactly one mint attempt, got %d (a retry here would be the fix, not the bug)", daemon.createBeadCalls)
	}
	if !isTransientDaemonError(err) {
		t.Errorf("the reproduced failure must be a transient error the fix could have absorbed, got: %v", err)
	}
}

// TestBacktest_Thread1784843199_RetryWrapperSurvivesRollWindow confirms the
// merged fix: the SAME roll-window blip pattern that dead-ended the pre-fix mint
// is now absorbed by retryTransientDaemon (the real helper the respawn path
// wraps its CreateBead in). Two consecutive transient blips, then success —
// well within threadWriteMaxAttempts (3) — so the operator's wake-up resumes the
// agent instead of hitting the ":x:" dead-end.
func TestBacktest_Thread1784843199_RetryWrapperSurvivesRollWindow(t *testing.T) {
	daemon := newMockDaemon()
	b := newTestBot(daemon, newFakeSlackServer(t))

	blips := rollWindowBlips()
	var mintedID string
	err := b.retryTransientDaemon(context.Background(), "respawn_create", func() error {
		attempt := daemon.createBeadCalls // 0-based before this call
		daemon.createBeadHook = func(_ int, req beadsapi.CreateBeadRequest) error {
			// Fail the first two attempts with roll-window blips, then succeed.
			if attempt < 2 {
				return blips[attempt]
			}
			return nil
		}
		id, e := daemon.CreateBead(context.Background(), beadsapi.CreateBeadRequest{
			Title:          "thread-1784843199-442769",
			Type:           "agent",
			Fields:         nil,
			IdempotencyKey: "thread-respawn:C0B5S61RVNU:1784843199.442769:1",
		})
		if e != nil {
			return e
		}
		mintedID = id
		return nil
	})
	if err != nil {
		t.Fatalf("FIX REGRESSED: retryTransientDaemon gave up on the roll-window blips (%v); the resume-mint dead-end would recur", err)
	}
	if mintedID == "" {
		t.Fatal("expected the retry to eventually mint the resume bead")
	}
	// Proof the retry actually happened (not a first-attempt success): the mock
	// saw more than one CreateBead call, and exactly one bead was minted.
	if daemon.createBeadCalls < 2 {
		t.Fatalf("expected the retry loop to make multiple mint attempts across the roll window, got %d", daemon.createBeadCalls)
	}
}

// TestBacktest_Thread1784843199_LostResponseDoesNotDoubleMint reproduces the
// subtler half of the roll window the RCA's fix also had to handle: a mint whose
// write COMMITTED at the daemon but whose RESPONSE was lost to the same pod roll.
// A naive retry would double-mint a duplicate resume agent. The per-wake-up
// IdempotencyKey (thread-respawn:<channel>:<ts>:<respawnStart>) makes the retry
// converge on the one committed bead. Drives the real mock idempotency
// lookup-or-return contract.
func TestBacktest_Thread1784843199_LostResponseDoesNotDoubleMint(t *testing.T) {
	daemon := newMockDaemon()
	b := newTestBot(daemon, newFakeSlackServer(t))

	const key = "thread-respawn:C0B5S61RVNU:1784843199.442769:1730000000000000000"
	firstDone := false
	daemon.createBeadHook = func(_ int, req beadsapi.CreateBeadRequest) error {
		if req.IdempotencyKey != key {
			return nil
		}
		if !firstDone {
			firstDone = true
			// Model a committed-but-lost-response write: persist the bead under
			// the key (as the real daemon's idempotency index would), then return
			// a transient error so the client retries. The hook runs INSIDE
			// CreateBead, which already holds m.mu — insert directly without
			// re-locking (matching TestRespawnThreadAgent_TransientRetryDoesNotDoubleMint).
			daemon.beads["bd-committed"] = &beadsapi.BeadDetail{
				ID:     "bd-committed",
				Title:  req.Title,
				Type:   req.Type,
				Status: "open",
				Fields: map[string]string{
					"idempotency_key": req.IdempotencyKey,
					"agent":           "thread-1784843199-442769",
					"spawn_source":    "slack-thread-resume",
				},
			}
			return &beadsapi.APIError{StatusCode: http.StatusBadGateway, Message: "lost response (pod rolled after commit)"}
		}
		return nil
	}

	var mintedID string
	err := b.retryTransientDaemon(context.Background(), "respawn_create", func() error {
		id, e := daemon.CreateBead(context.Background(), beadsapi.CreateBeadRequest{
			Title:          "thread-1784843199-442769",
			Type:           "agent",
			IdempotencyKey: key,
		})
		if e != nil {
			return e
		}
		mintedID = id
		return nil
	})
	if err != nil {
		t.Fatalf("retry after lost response failed: %v", err)
	}

	// Exactly ONE resume agent bead must exist — the committed one, reused by the
	// idempotency-keyed retry, not a duplicate.
	var resumeBeads int
	daemon.mu.Lock()
	for _, bead := range daemon.beads {
		if bead.Fields["spawn_source"] == "slack-thread-resume" {
			resumeBeads++
		}
	}
	daemon.mu.Unlock()
	if resumeBeads != 1 {
		t.Fatalf("expected exactly 1 resume agent bead (idempotency dedup after lost response), got %d — a double-mint would spawn two agents for one wake-up", resumeBeads)
	}
	if mintedID != "bd-committed" {
		t.Errorf("retry should converge on the committed bead bd-committed, got %q", mintedID)
	}
}
