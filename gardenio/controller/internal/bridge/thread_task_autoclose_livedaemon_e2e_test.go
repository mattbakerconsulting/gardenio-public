//go:build e2e

// thread_task_autoclose_livedaemon_e2e_test.go — LIVE-DAEMON e2e for the
// thread-task idle-hint DEMOTION path (bead ka-0000007208, parent
// ka-0000000958; flag flip ka-0000000287).
//
// ── The gap this closes ───────────────────────────────────────────────────────
//
// MR 7d35eb2ce (ka-0000000287) did TWO things: it added a 9-scenario Go UNIT
// suite (thread_task_autoclose_demoted_test.go, over a mockDaemon) AND it flipped
// slackBridge.slack.threadTaskHintInsteadOfClose: true in
// the RWX-era e2e deploy-values — so the LIVE behaviour of the thread-task
// auto-closer in the exact namespace the e2e suite runs against just inverted:
// the death rule now DEMOTES an idle wish to an actionable hint and leaves it
// OPEN, instead of closing it. Nothing in EITHER e2e suite asserted either the
// old or the new behaviour (a selector grep for `gardenio-idle-hint`, `idle-hint`,
// `HintInsteadOfClose`, `threadTaskHint` returned 0 hits across
// tests/e2e/ and tests/beads-viewer-e2e/), and the "auto-close" shell modules
// (test-auto-close-cascade.sh, test-stale-reaper.sh) cover DIFFERENT subsystems
// (the parent-child close cascade + the stale-AGENT reaper) — a `thread_task`
// grep inside them is empty.
//
// ── Why a LIVE-DAEMON go:build e2e test, not (a) a shell module or (b) the mock
//    unit suite ──────────────────────────────────────────────────────────────
//
//	(a) The demotion logic lives in the BRIDGE (ThreadTaskAutoCloser), driven by
//	    a 30m close window + a 15m backfill sweep with NO admin/sweep-trigger
//	    endpoint — the same "far past a shard's wall-clock budget" constraint that
//	    makes escalation-auto-reaper a probe rather than a live-bridge leg. A
//	    shell test cannot wait out the window, and cannot reach runCheck/sweepOnce
//	    (both unexported), so it could only re-implement the contract in bash.
//	(b) The mock unit suite already asserts all 9 demoted scenarios — but over a
//	    mockDaemon that RE-IMPLEMENTS the daemon. Per the sibling escalation
//	    /result live-daemon backfill (58054fe9f): "a mock re-implementing the
//	    daemon is not evidence the daemon does it." Three real daemon semantics
//	    this path DEPENDS ON are exactly what the mock cannot model, and each was
//	    empirically confirmed against the live e2e daemon while authoring
//	    this test:
//	      1. A comment write does NOT bump the bead's updated_at (the bead's
//	         updated_at stays fixed at create-time while the comment carries a
//	         LATER created_at). alreadyHintedThisEpisode's dedupe compares the
//	         hint's created_at against max(bead.UpdatedAt, newest non-hint
//	         comment) — so this exact semantic is what makes assertion 3 (dedupe)
//	         and assertion 4 (fresh-episode re-hint) deterministic on prod. A mock
//	         that bumped updated_at on comment would make the dedupe read
//	         "activity newer than hint" and mask a regression.
//	      2. A write with NO operator resolves 400 operator_refusal; the closer's
//	         autoCloseOperatorContext is the ONLY thing keeping the AddComment
//	         write from fail-closing (the client-level SetSystemOperator default
//	         was removed, ka-0000006293). This test drives the REAL per-call
//	         attribution against the REAL daemon fail-close gate.
//	      3. The live type=task schema REJECTS the triage_reason field (400
//	         "unknown field"). markNeedsTriage's field write is best-effort by
//	         design, so the wish still stays OPEN + needs-triage LABEL + triage
//	         COMMENT (the durable signals the sweep keys off) — this is the "assert
//	         the shape prod actually emits" lesson (ka-0000001521): the label +
//	         comment are load-bearing, the field is not present on this deployment.
//
// So this drives the REAL ThreadTaskAutoCloser.runCheck (the same event-path code
// the bridge runs in prod) against a REAL beadsapi.Client pointed at the live
// e2e daemon — production code, real write/read seam, no reimplementation.
//
// ── Event path only (runCheck), never the sweep ──────────────────────────────
//
// runCheck is scoped to a SINGLE thread bead id — the one this test seeded — so
// it is HERMETIC on a shared daemon: it touches ONLY our bead. The backfill
// sweepOnce, by contrast, lists EVERY open source:slack task bead (Limit 1000)
// and would post idle-hints on ambient prod wishes — never hermetic against a
// live shared daemon, so this test deliberately never calls it. The event path
// still exercises every one of the five contract points the task names; the
// sweep-path dedupe/positive-evidence legs stay in the mock unit suite where they
// can be driven safely.
//
// Hermeticity (ka-0000000958 cycle-26 standing rule — seed the pressure, never
// read ambient state): every assertion runs against a bead THIS test created
// under a unique synthetic slack-thread:{channel}:{ts} label, and keys ONLY on
// that bead id. No ambient idle thread-task is assumed to exist.
//
// It is `//go:build e2e`-tagged (invisible to `go build ./...`, `go test ./...`,
// and golangci-lint — none pass `-tags e2e`) so it never inflates the default
// build/lint/test surface, mirroring subagentstop_e2e_test.go. It runs ONLY when
// the threadtask-idle-hint module invokes it via
// `go test -tags e2e -run TestThreadTaskIdleHintDemotionLiveDaemonE2E`, and
// SKIPs LOUD (t.Skip) when BEADS_HTTP_URL/token are unset or the daemon is
// unreachable — a missing daemon must never be read as a pass.

package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// e2eOperator is the human-shaped operator the seeded thread bead carries so the
// closer's autoCloseOperatorContext takes the WithOperator (X-Operated-By) path.
// U0OPERATOR1 is the operator id already present across the gardenio control plane
// (it is the operated_by on this crew's own assignment); using it keeps the
// attribution write from fail-closing on any namespace the suite runs against.
const e2eOperator = "slack:U0OPERATOR1"

// liveE2EClient builds a real beadsapi.Client against the daemon the e2e shard
// exports (BEADS_HTTP_URL + BEADS_AUTH_TOKEN/BEADS_TOKEN), or SKIPs LOUD.
func liveE2EClient(t *testing.T) *beadsapi.Client {
	t.Helper()
	addr := firstNonEmpty(os.Getenv("BEADS_HTTP_URL"), os.Getenv("BEADS_HTTP_ADDR"))
	if addr == "" {
		t.Skip("SKIP (LOUD): BEADS_HTTP_URL/BEADS_HTTP_ADDR unset — this run did NOT assert the thread-task idle-hint demotion against a live daemon")
	}
	token := firstNonEmpty(os.Getenv("BEADS_AUTH_TOKEN"), os.Getenv("BEADS_TOKEN"), os.Getenv("BEADS_DAEMON_TOKEN"))
	client, err := beadsapi.New(beadsapi.Config{HTTPAddr: addr, Token: token})
	if err != nil {
		t.Fatalf("beadsapi.New(%q): %v", addr, err)
	}
	// Reachability probe: a live-daemon test must SKIP (not FAIL) when the daemon
	// is simply absent (local dev, un-opted-in namespace), but must proceed once
	// it is reachable. GetBead of a non-existent id against a HEALTHY daemon
	// returns an HTTP 404 ("bead not found") — which PROVES the daemon answered,
	// so a 404 is the reachable signal, NOT a skip. Only a connection-level error
	// (dial refused / DNS failure / timeout — no "HTTP <code>" in the message)
	// means the daemon is genuinely absent, and only THAT skips.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = client.GetBead(ctx, "gd-e2e-reachability-probe-nonexistent")
	if err != nil && !strings.Contains(err.Error(), "HTTP ") {
		t.Skipf("SKIP (LOUD): beads daemon at %s not reachable (%v) — this run did NOT assert the idle-hint demotion", addr, err)
	}
	return client
}

// liveAutoCloser builds the REAL ThreadTaskAutoCloser (which only hints,
// gd-Zhcpn9IZVl) against the live daemon client. nowFunc is a fixed clock so
// the activity-relative dedupe comparison is deterministic within a test.
func liveAutoCloser(client *beadsapi.Client, now time.Time) *ThreadTaskAutoCloser {
	c := NewThreadTaskAutoCloser(ThreadTaskAutoCloserConfig{
		Daemon: client,
		Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	})
	c.nowFunc = func() time.Time { return now }
	return c
}

// uniqueThread returns a synthetic (channel, ts) binding unique to this run so
// the seeded bead can never collide with an ambient prod thread. The ts is a
// real "seconds.micros"-shaped value (isValidThreadBinding requires the dot) but
// scoped to a private E2E channel prefix the fleet never uses.
func uniqueThread(tag string) (channel, threadTS string) {
	now := time.Now().UTC()
	channel = "CE2E" + tag // Slack channel ids start with C; this prefix is test-only
	threadTS = fmt.Sprintf("%d.%06d", now.Unix(), now.Nanosecond()/1000)
	return channel, threadTS
}

// seedLiveThreadTask creates a REAL open type=task thread-wish bead on the live
// daemon carrying the canonical source:slack + slack-thread:{channel}:{ts}
// labels and the thread binding fields, attributed to a human-shaped operator so
// downstream writes never fail-close. Returns its id; registers cleanup that
// closes it (best-effort) so the test never leaks an open wish onto the shared
// daemon.
func seedLiveThreadTask(t *testing.T, client *beadsapi.Client, channel, threadTS string) string {
	t.Helper()
	ctx := beadsapi.WithOperator(context.Background(), e2eOperator)
	fields := fmt.Sprintf(`{"slack_thread_channel":%q,"slack_thread_ts":%q,"slack_user_id":"U0OPERATOR1"}`, channel, threadTS)
	id, err := client.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Type:      "task",
		Title:     "e2e idle-hint demotion wish " + channel + "/" + threadTS,
		CreatedBy: "e2e-threadtask-idle-hint",
		Labels:    []string{"source:slack", threadBeadLabel(channel, threadTS), "e2e:true", "project:e2e-test"},
		Fields:    []byte(fields),
	})
	if err != nil {
		t.Fatalf("seed live thread task: %v", err)
	}
	if id == "" {
		t.Fatal("seed live thread task: empty id returned")
	}
	t.Cleanup(func() {
		_ = client.CloseBead(beadsapi.WithOperator(context.Background(), e2eOperator), id, map[string]string{})
	})
	return id
}

// beadStatus / labels / comments helpers read straight from the live daemon.
func liveStatus(t *testing.T, client *beadsapi.Client, id string) string {
	t.Helper()
	b, err := client.GetBead(context.Background(), id)
	if err != nil {
		t.Fatalf("GetBead(%s): %v", id, err)
	}
	if b == nil {
		t.Fatalf("GetBead(%s): nil (bead vanished)", id)
	}
	return b.Status
}

func liveHasLabel(t *testing.T, client *beadsapi.Client, id, label string) bool {
	t.Helper()
	b, err := client.GetBead(context.Background(), id)
	if err != nil || b == nil {
		t.Fatalf("GetBead(%s): %v (bead=%v)", id, err, b)
	}
	for _, l := range b.Labels {
		if l == label {
			return true
		}
	}
	return false
}

// countLiveHints returns how many idle-hint comments (carrying idleHintMarker)
// the live daemon has recorded for id.
func countLiveHints(t *testing.T, client *beadsapi.Client, id string) int {
	t.Helper()
	cs, err := client.GetComments(context.Background(), id)
	if err != nil {
		t.Fatalf("GetComments(%s): %v", id, err)
	}
	n := 0
	for _, c := range cs {
		if strings.Contains(c.Text, idleHintMarker) {
			n++
		}
	}
	return n
}

func countLiveMarker(t *testing.T, client *beadsapi.Client, id, marker string) int {
	t.Helper()
	cs, err := client.GetComments(context.Background(), id)
	if err != nil {
		t.Fatalf("GetComments(%s): %v", id, err)
	}
	n := 0
	for _, c := range cs {
		if strings.Contains(c.Text, marker) {
			n++
		}
	}
	return n
}

// TestThreadTaskIdleHintDemotionLiveDaemonE2E drives the REAL demoted-world event
// path (runCheck) against the LIVE daemon across the five contract points named
// in ka-0000007208. Each sub-test seeds its OWN thread wish and asserts only
// against that bead id, so the whole test is hermetic on a shared daemon.
func TestThreadTaskIdleHintDemotionLiveDaemonE2E(t *testing.T) {
	client := liveE2EClient(t)
	ctx := context.Background()

	// ── 1. DEMOTION (the headline inversion) ─────────────────────────────────
	// A done agent on a quiet thread posts a SINGLE idle-hint COMMENT and the
	// wish stays status=open: the death rule no longer kills.
	t.Run("demotion-hint-leaves-open", func(t *testing.T) {
		channel, threadTS := uniqueThread("D1")
		id := seedLiveThreadTask(t, client, channel, threadTS)
		c := liveAutoCloser(client, time.Now())

		c.runCheck(ctx, "e2e-agent-1", id, channel, threadTS, false /*not failed*/)

		if got := liveStatus(t, client, id); got != "open" {
			t.Fatalf("wish must stay OPEN after idle hint (death-rule demotion), got %q", got)
		}
		if got := countLiveHints(t, client, id); got != 1 {
			t.Fatalf("expected exactly 1 idle-hint comment on the live daemon, got %d", got)
		}
	})

	// ── 2. MARKER + affordance (assert on the machine token, not the prose) ───
	// The hint comment carries the [gardenio-idle-hint] dedupe marker on its own
	// trailing line AND the in-place ⏹️ close affordance. Per source, the marker
	// rides LAST after a \n\n separator so the prose can evolve — assert the
	// marker + the affordance token, never the surrounding sentence.
	t.Run("marker-and-close-affordance", func(t *testing.T) {
		channel, threadTS := uniqueThread("D2")
		id := seedLiveThreadTask(t, client, channel, threadTS)
		c := liveAutoCloser(client, time.Now())

		c.runCheck(ctx, "e2e-agent-2", id, channel, threadTS, false)

		cs, err := client.GetComments(ctx, id)
		if err != nil {
			t.Fatalf("GetComments: %v", err)
		}
		var hint string
		for _, cm := range cs {
			if strings.Contains(cm.Text, idleHintMarker) {
				hint = cm.Text
			}
		}
		if hint == "" {
			t.Fatalf("no idle-hint comment carrying %q found on the live daemon (%d comments)", idleHintMarker, len(cs))
		}
		// Marker rides LAST, after a \n\n separator — the dedupe contract.
		if !strings.HasSuffix(strings.TrimRight(hint, "\n"), idleHintMarker) {
			t.Errorf("idle-hint marker must ride last (after a separator); got body:\n%q", hint)
		}
		if !strings.Contains(hint, "black_square_for_stop") {
			t.Errorf("idle hint must carry the ⏹️ in-place close affordance, got:\n%q", hint)
		}
		// It must NOT claim the wish was closed — the whole point of the demotion.
		if strings.Contains(hint, "Auto-closed") {
			t.Errorf("idle hint must not claim the wish was closed, got:\n%q", hint)
		}
	})

	// ── 3. DEDUPE / no-spam (failure mode 1b-6) ──────────────────────────────
	// Two runCheck passes within the SAME idle episode post exactly ONE hint.
	// This is the assertion that leans on the empirically-confirmed live semantic
	// that a comment write does NOT bump the bead's updated_at — so the second
	// pass sees "a hint already sits at-or-after the last real activity" and stays
	// silent, instead of re-hinting because the comment moved the clock forward.
	t.Run("dedupe-one-hint-per-episode", func(t *testing.T) {
		channel, threadTS := uniqueThread("D3")
		id := seedLiveThreadTask(t, client, channel, threadTS)
		c := liveAutoCloser(client, time.Now())

		c.runCheck(ctx, "e2e-agent-3", id, channel, threadTS, false)
		c.runCheck(ctx, "e2e-agent-3", id, channel, threadTS, false)

		if got := countLiveHints(t, client, id); got != 1 {
			t.Fatalf("two checks in one idle episode must produce exactly 1 hint (dedupe), got %d", got)
		}
		if got := liveStatus(t, client, id); got != "open" {
			t.Fatalf("wish must stay open across the deduped checks, got %q", got)
		}
	})

	// ── 4. FRESH episode re-hints ────────────────────────────────────────────
	// After the first hint, a REAL human comment lands NEWER than the hint,
	// opening a new idle episode. The next check IS allowed to hint again.
	// Dedupe is activity-relative, not a permanent gag. This too depends on the
	// live created_at ordering of comments being monotonic and on updated_at NOT
	// moving on the hint write.
	t.Run("fresh-activity-reopens-episode", func(t *testing.T) {
		channel, threadTS := uniqueThread("D4")
		id := seedLiveThreadTask(t, client, channel, threadTS)
		c := liveAutoCloser(client, time.Now())

		// Episode 1: two checks → one hint (deduped).
		c.runCheck(ctx, "e2e-agent-4", id, channel, threadTS, false)
		c.runCheck(ctx, "e2e-agent-4", id, channel, threadTS, false)
		if got := countLiveHints(t, client, id); got != 1 {
			t.Fatalf("episode 1: expected 1 hint, got %d", got)
		}

		// A human replies — a non-hint comment strictly newer than the hint opens
		// episode 2. Attributed to a human operator so the write does not
		// fail-close, exactly as a real Slack reply would arrive.
		humanCtx := beadsapi.WithOperator(ctx, e2eOperator)
		// Guarantee a strictly-later created_at than the episode-1 hint.
		time.Sleep(1100 * time.Millisecond)
		if err := client.AddComment(humanCtx, id, "U0OPERATOR1", "still need this?"); err != nil {
			t.Fatalf("seed human reply comment: %v", err)
		}

		// Episode 2: the next check may hint again.
		c.runCheck(ctx, "e2e-agent-4", id, channel, threadTS, false)
		if got := countLiveHints(t, client, id); got != 2 {
			t.Fatalf("episode 2: expected 2 total hints after fresh activity, got %d", got)
		}
		if got := liveStatus(t, client, id); got != "open" {
			t.Fatalf("wish must remain open across both episodes, got %q", got)
		}
	})

	// ── 5. FAILED-agent carve-out (1b-7) — the distinct branch ────────────────
	// A failed agent holds the wish OPEN and marks it needs-triage: a crash is
	// the opposite of "looks done". It must NOT be
	// idle-hinted and must NOT be closed. The durable signals the sweep keys off
	// are the needs-triage LABEL + the triage COMMENT (carrying triageMarker) —
	// asserted here against the live daemon. NB: the live type=task schema rejects
	// the triage_reason FIELD (confirmed 400 "unknown field"); markNeedsTriage's
	// field write is best-effort, so we deliberately assert the label + comment +
	// open status (what actually lands), not the field (which does not exist on
	// this deployment). This is exactly the divergence a mock daemon would hide.
	t.Run("failed-agent-needs-triage-stays-open", func(t *testing.T) {
		channel, threadTS := uniqueThread("D5")
		id := seedLiveThreadTask(t, client, channel, threadTS)
		c := liveAutoCloser(client, time.Now())

		c.runCheck(ctx, "e2e-agent-5", id, channel, threadTS, true /*failed*/)

		if got := liveStatus(t, client, id); got != "open" {
			t.Fatalf("a FAILED-agent wish must stay OPEN (needs-triage, not closed), got %q", got)
		}
		if !liveHasLabel(t, client, id, triageLabel) {
			t.Errorf("expected the %q label on the live bead after a failed agent", triageLabel)
		}
		// Exactly one triage comment, and NOT an idle hint.
		if got := countLiveHints(t, client, id); got != 0 {
			t.Errorf("a failure must never be idle-hinted, got %d idle-hint comments", got)
		}
		if got := countLiveMarker(t, client, id, triageMarker); got != 1 {
			t.Errorf("expected exactly 1 triage comment carrying %q, got %d", triageMarker, got)
		}
	})
}
