package bridge

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestThreadReplyLeakDetectorSeriesArePrewarmed pins the fix for ka-0000003771.
// The System Health "Thread-reply write-failure & DROP rate (silent-durability
// leak)" panel queries gardenio_thread_reply_write_failures_total and
// gardenio_thread_reply_dlq_dropped_total — labeled CounterVecs. A Prometheus
// *Vec exports NO series for a label combination until it is first observed, so a
// fleet that has never dropped a reply exported the parent family with zero
// children: the series were ABSENT and the leak detector rendered an empty green,
// silently dead exactly when it was meant to read all-clear. The package init()
// must pre-warm every known label so the detector is a flat-zero baseline that
// steps up the instant a real failure increments it.
//
// The counters are package globals shared with the DLQ tests, so this asserts on
// the SET of series present (which the pre-warm fixes to be non-empty regardless
// of failures) rather than on their values (which other tests mutate). No test in
// the package introduces a label outside these pre-warmed sets, so the child
// counts are stable across test order.
func TestThreadReplyLeakDetectorSeriesArePrewarmed(t *testing.T) {
	// write-failure children: one per write stage (head_resolve/marshal/create).
	// This CounterVec is dedicated to the thread-reply write path, so its child
	// count is exactly the three pre-warmed stages.
	if got := testutil.CollectAndCount(threadReplyWriteFailures); got != 3 {
		t.Errorf("write-failure series count = %d, want 3 (head_resolve/marshal/create pre-warmed so the leak panel is not empty-green)", got)
	}
	// dlq-dropped children: one per terminal drop reason emitted by the write +
	// DLQ paths. Also dedicated, so exactly the four pre-warmed reasons.
	if got := testutil.CollectAndCount(threadReplyDLQDropped); got != 4 {
		t.Errorf("dlq-dropped series count = %d, want 4 (nontransient/no_state/overflow/exhausted pre-warmed)", got)
	}
	// write-retry children: this CounterVec is SHARED with the respawn path
	// (bot_agent_kill.go labels it respawn_revive/respawn_create too), so the total
	// child count is not thread-reply-specific. Assert only that the two
	// thread-reply stages are present (>= 2), not an exact count. marshal is
	// deterministic and never retried, so it has no retry series.
	if got := testutil.CollectAndCount(threadReplyWriteRetries); got < 2 {
		t.Errorf("write-retry series count = %d, want >= 2 (head_resolve/create pre-warmed)", got)
	}
}
