package main

// init_stdout_tail.go — capture last N kB of coop stdout/stderr for post-mortems.
//
// When an agent pod is recycled (liveness probe, OOM, eviction) all of coop's
// stdout/stderr is lost: kubectl logs --previous returns nothing once the pod
// is gone. Without that residue, exit-code 1 / context_cancelled close reasons
// give no signal about *why* the agent process died.
//
// This file adds a fixed-size ring buffer that tees coop's stdout/stderr while
// the session is running, plus a fast-crash detector. When ≥3 sub-30s exits
// occur in a sliding window (or a context_cancelled / max_restarts shutdown is
// imminent), the tail is uploaded to:
//
//   s3://<COOP_S3_BUCKET>/<COOP_S3_PREFIX>/<agent-bead-id>/<session-id>/stdout-tail.txt
//
// This matches coop's per-spawn S3 layout (see coop/docs/s3-session-lifecycle.md):
// every artifact for one spawn lives under <bead>/<coop-uuid>/, so a tail can
// be joined to its spawn directly by S3 prefix without cross-referencing the
// agent bead's last_session_id. When no agent bead ID is in scope (legacy /
// ad-hoc invocations) the tail falls back to the flat <session-id>/ layout.
//
// Same bucket/prefix that coop already uses for transcripts and session
// metadata — see audit.go for the read side.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"gardenio/controller/internal/coopapi"
)

// stdoutTailBytes is the cap on bytes retained per session. 256 kB is enough
// to catch the typical "auth failed" / "MCP server load failed" / panic
// trailer without bloating S3.
const stdoutTailBytes = 256 * 1024

// fastCrashThreshold is the number of sub-fastCrashWindow exits required
// inside fastCrashLookback to trigger a tail upload.
const (
	fastCrashThreshold = 3
	fastCrashWindow    = 30 * time.Second
	fastCrashLookback  = 5 * time.Minute
)

// tailBuffer is a thread-safe ring buffer that retains the last N bytes
// written. It satisfies io.Writer so it can be passed to MultiWriter alongside
// os.Stdout / os.Stderr.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func newTailBuffer(max int) *tailBuffer {
	return &tailBuffer{max: max}
}

// Write appends p to the buffer and trims to max bytes. Always returns
// (len(p), nil) — failures here would silently break the tee, and there is no
// useful recovery.
func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(p) >= t.max {
		t.buf = append(t.buf[:0], p[len(p)-t.max:]...)
		return len(p), nil
	}
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

// Snapshot returns a copy of the current buffered tail.
func (t *tailBuffer) Snapshot() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]byte, len(t.buf))
	copy(out, t.buf)
	return out
}

// Len returns the current buffered byte count.
func (t *tailBuffer) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.buf)
}

// Reset drops all buffered bytes. Used between coop sessions so the tail
// only carries the latest session's output.
func (t *tailBuffer) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = t.buf[:0]
}

// crashTracker records coop session durations and reports when a fast-crash
// pattern (≥threshold exits shorter than window, all within lookback) emerges.
type crashTracker struct {
	mu        sync.Mutex
	threshold int
	window    time.Duration
	lookback  time.Duration
	now       func() time.Time
	crashes   []time.Time
}

func newCrashTracker(threshold int, window, lookback time.Duration) *crashTracker {
	return &crashTracker{
		threshold: threshold,
		window:    window,
		lookback:  lookback,
		now:       time.Now,
	}
}

// Record stores an exit. Returns true when the count of sub-window exits in
// the last `lookback` reaches the threshold. When triggered, the tracker
// resets so the same burst is not reported twice.
func (c *crashTracker) Record(elapsed time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if elapsed >= c.window {
		// Long-running session resets the streak.
		c.crashes = c.crashes[:0]
		return false
	}
	now := c.now()
	c.crashes = append(c.crashes, now)
	cutoff := now.Add(-c.lookback)
	pruned := c.crashes[:0]
	for _, t := range c.crashes {
		if t.After(cutoff) {
			pruned = append(pruned, t)
		}
	}
	c.crashes = pruned
	if len(c.crashes) >= c.threshold {
		c.crashes = c.crashes[:0]
		return true
	}
	return false
}

// stdoutTailKey builds the S3 key for a stdout-tail upload. When agentBeadID
// is non-empty, the key is <prefix>/<agent-bead-id>/<sessionID>/stdout-tail.txt
// so it slots into the same <bead>/<spawn>/ directory coop writes to. Empty
// agent bead ID falls back to the legacy flat <prefix>/<sessionID>/ layout.
func stdoutTailKey(prefix, agentBeadID, sessionID string) string {
	if agentBeadID == "" {
		return fmt.Sprintf("%s/%s/stdout-tail.txt", prefix, sessionID)
	}
	return fmt.Sprintf("%s/%s/%s/stdout-tail.txt", prefix, agentBeadID, sessionID)
}

// uploadStdoutTail PUTs tail to s3://<bucket>/<prefix>/<agent-bead-id>/<sessionID>/stdout-tail.txt
// when an agent bead ID is in scope, mirroring coop's per-spawn key layout so
// the tail sits next to the spawn's transcripts/meta. Falls back to the legacy
// flat <prefix>/<sessionID>/ when no agent bead ID is available.
//
// Returns (uploaded, error). uploaded is false (with nil err) when S3 is not
// configured, the session ID is empty, or the buffer is empty — these are
// quiet no-ops, not failures.
func uploadStdoutTail(ctx context.Context, sessionID string, tail []byte, reason string) (bool, error) {
	bucket := s3Bucket()
	if bucket == "" {
		return false, nil
	}
	if sessionID == "" {
		return false, nil
	}
	if len(tail) == 0 {
		return false, nil
	}

	key := stdoutTailKey(s3Prefix(), agentS3SessionKey(), sessionID)

	client, err := newS3Client(ctx)
	if err != nil {
		return false, fmt.Errorf("s3 client: %w", err)
	}

	putCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	_, err = client.PutObject(putCtx, &s3.PutObjectInput{
		Bucket:      aws.String(bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(tail),
		ContentType: aws.String("text/plain; charset=utf-8"),
		Metadata: map[string]string{
			"reason":     reason,
			"tail-bytes": fmt.Sprintf("%d", len(tail)),
		},
	})
	if err != nil {
		return false, fmt.Errorf("put stdout-tail: %w", err)
	}
	return true, nil
}

// persistStdoutTailBestEffort uploads the tail and logs the outcome. It never
// returns an error — this runs on shutdown paths where there is no recovery.
func persistStdoutTailBestEffort(ctx context.Context, sessionID string, tail []byte, reason string) {
	if len(tail) == 0 {
		return
	}
	uploaded, err := uploadStdoutTail(ctx, sessionID, tail, reason)
	if err != nil {
		logInit("stdout-tail: upload failed (%s, sid=%s): %v", reason, sessionID, err)
		return
	}
	if uploaded {
		logInit("stdout-tail: uploaded %d bytes to s3://%s/%s (reason=%s)",
			len(tail), s3Bucket(), stdoutTailKey(s3Prefix(), agentS3SessionKey(), sessionID), reason)
	}
}

// agentBeadClosedOutOfProcess reports whether the agent bead was closed by a
// DIFFERENT process on a terminal DEATH path while this supervisor loop was
// between coop sessions, returning the close_reason that classified the death.
//
// The stop-gate Stop-hook (`gb hook stop-gate` → closeAgentBeadOnExit:
// cooldown / escape_hatch / rate_limited) closes the agent bead from outside
// this process and — unlike `gb stop`, the idle-TTL despawn, and the reconciler
// despawn, all of which set stop_requested (caught by the isStopRequested branch
// above) — leaves stop_requested UNSET. So when coop exits, neither the
// ctx.Err() nor the isStopRequested branch fires and the restart loop respawns
// coop, whose first act is tailBuf.Reset() (init_agent_loop.go) — wiping the
// just-exited session's stdout tail before any post-mortem ships it. Those are
// exactly the beads that carry a SYNTHESIZED wrap-up (the agent never explained
// itself), i.e. the deaths a stdout tail is designed for (ka-0000004483).
// Detecting the close here lets the loop ship the tail and exit cleanly instead
// of respawning against an already-closed bead.
//
// Returns ("", false) for a still-open bead, a clean self-close (`gb stop` /
// `gb done` leave close_reason empty), or any lookup error — none of those
// should ship a tail or short-circuit the restart loop.
func agentBeadClosedOutOfProcess(ctx context.Context) (string, bool) {
	agentBeadID := envOr("KD_AGENT_ID", os.Getenv("BOAT_AGENT_BEAD_ID"))
	if agentBeadID == "" || daemon == nil {
		return "", false
	}
	bead, err := daemon.GetBead(ctx, agentBeadID)
	if err != nil || bead == nil {
		return "", false
	}
	if bead.Status != "closed" {
		return "", false
	}
	// A clean `gb stop` / `gb done` closes the bead with agent_state=done and an
	// empty close_reason (a real wrap-up, if any, lives under the wrapup field).
	// Only a terminal death path stamps a close_reason, and those are the closes
	// worth a post-mortem tail.
	if strings.TrimSpace(bead.Fields["close_reason"]) == "" {
		return "", false
	}
	return strings.TrimSpace(bead.Fields["close_reason"]), true
}

// tailReasonFromCloseReason reduces a bead close_reason to the short classifier
// token used as the S3 tail's `reason` metadata, so an out-of-process tail reads
// consistently with the in-process labels (fast_crash, context_cancelled, …).
// close_reason on the death paths is formatted "<token>: <human detail>"
// (e.g. "stop_gate_cooldown_involuntary: claude exited mid-work…"); keep the
// leading token and drop the prose. A reason with no colon is returned as-is.
func tailReasonFromCloseReason(closeReason string) string {
	if i := strings.IndexByte(closeReason, ':'); i >= 0 {
		return strings.TrimSpace(closeReason[:i])
	}
	return strings.TrimSpace(closeReason)
}

// derefSessionID safely loads the value from an atomic.Pointer[string].
// Returns "" when the pointer holds nil.
func derefSessionID(p *atomic.Pointer[string]) string {
	if p == nil {
		return ""
	}
	if s := p.Load(); s != nil {
		return *s
	}
	return ""
}

// captureSessionID polls the coop API until the session ID is reported, then
// stores it in sink so the surrounding restart loop can use it as the S3 key
// for tail uploads even after coop has exited. Best-effort; runs as a
// goroutine scoped to the session context.
func captureSessionID(ctx context.Context, coop *coopapi.Client, sink *atomic.Pointer[string]) {
	if sink == nil {
		return
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if time.Now().After(deadline) {
			return
		}
		status, err := coop.GetSessionStatus(ctx)
		if err != nil || status == nil {
			continue
		}
		if status.SessionID == "" {
			continue
		}
		if !sessionIDPattern.MatchString(status.SessionID) {
			// Avoid storing the "session" sentinel that a buggy
			// prepare_resume once produced — same defense as
			// storeCoopSessionID.
			return
		}
		sid := status.SessionID
		sink.Store(&sid)
		// Persist the session UUID onto the agent bead so the beads-viewer can
		// reach a LIVE session — nudge it via coopmux, render its PTY, forward
		// operator input. Without this stamp the viewer's last_session_id read
		// is always empty and every message to a running agent (e.g. the /plans
		// assistant chat) falls through to the wake path and is dropped
		// (ka-0000000352). Best-effort; must not block the tail-key capture.
		stampLastSessionID(ctx, sid)
		return
	}
}

// resolveTailSessionID returns the best available session ID for a tail
// upload: prefer the session ID just observed in this loop iteration, fall
// back to the last_session_id stored on the agent bead, then to the env-var
// hostname so a tail uploaded mid-restart-loop is at least findable.
func resolveTailSessionID(ctx context.Context, current string) string {
	if current != "" {
		return current
	}
	if last := fetchLastSessionID(ctx); last != "" {
		return last
	}
	if h, _ := os.Hostname(); h != "" {
		return "pod-" + h
	}
	return ""
}
