package bridge

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// mockThreadPoster records durable thread messages posted via
// ThreadMessagePoster so tests can assert the "Resumed in Xs" confirmation.
type mockThreadPoster struct {
	mu    sync.Mutex
	posts []threadPost
}

type threadPost struct {
	channel  string
	threadTS string
	text     string
}

func (m *mockThreadPoster) PostThreadMessage(_ context.Context, channelID, threadTS, text string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.posts = append(m.posts, threadPost{channel: channelID, threadTS: threadTS, text: text})
}

func (m *mockThreadPoster) all() []threadPost {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]threadPost(nil), m.posts...)
}

// TestHandleSessionStart_PostsResumeConfirmation verifies that a cold thread-agent
// resume armed with a channel + thread_ts posts a durable "Resumed in Xs"
// confirmation to that thread on first proof-of-life. This is the honest
// replacement for the old misleading "nudge failed … next cycle" warning
// (ka-0000005084, ka-0000002949).
func TestHandleSessionStart_PostsResumeConfirmation(t *testing.T) {
	mgr := NewAgentStreamManager(&mockStreamClient{}, testLogger())
	poster := &mockThreadPoster{}
	mgr.SetThreadMessagePoster(poster)
	ctx := context.Background()

	mgr.MarkColdResume("resume-agent", time.Now().Add(-3*time.Second), "thread", "runc", "C-THREAD", "ts-root")
	mgr.StartSession(ctx, "resume-agent", "C-THREAD", "ts-root", "U1")
	mgr.HandleSessionStart(ctx, "resume-agent", "resume")

	posts := poster.all()
	if len(posts) != 1 {
		t.Fatalf("expected 1 resume confirmation post, got %d: %+v", len(posts), posts)
	}
	got := posts[0]
	if got.channel != "C-THREAD" || got.threadTS != "ts-root" {
		t.Errorf("resume post targeted %q/%q, want C-THREAD/ts-root", got.channel, got.threadTS)
	}
	if !strings.Contains(got.text, "Resumed in") || !strings.Contains(got.text, "s.") {
		t.Errorf("resume post text = %q, want a 'Resumed in Xs.' confirmation", got.text)
	}

	// A second SessionStart must NOT double-post — the armed stamp is consumed.
	mgr.HandleSessionStart(ctx, "resume-agent", "resume")
	if posts := poster.all(); len(posts) != 1 {
		t.Errorf("resume confirmation double-posted: got %d posts, want 1", len(posts))
	}
}

// TestHandleSessionStart_NoResumePostWithoutThreadAnchor verifies that a cold
// resume with no channel/thread_ts anchor (e.g. a non-thread respawn) records
// its metrics but posts NO confirmation — there is no thread to post into.
func TestHandleSessionStart_NoResumePostWithoutThreadAnchor(t *testing.T) {
	mgr := NewAgentStreamManager(&mockStreamClient{}, testLogger())
	poster := &mockThreadPoster{}
	mgr.SetThreadMessagePoster(poster)
	ctx := context.Background()

	// Empty channel/threadTS — the metrics tee still runs, but no post.
	mgr.MarkColdResume("anchorless-agent", time.Now().Add(-1*time.Second), "thread", "runc", "", "")
	mgr.StartSession(ctx, "anchorless-agent", "C1", "ts-1", "U1")
	mgr.HandleSessionStart(ctx, "anchorless-agent", "resume")

	if posts := poster.all(); len(posts) != 0 {
		t.Errorf("expected no resume post without a thread anchor, got %d: %+v", len(posts), posts)
	}
}

// TestFormatResumeElapsed pins the resume-duration rendering: one decimal below
// a minute, coarser above.
func TestFormatResumeElapsed(t *testing.T) {
	cases := []struct {
		seconds float64
		want    string
	}{
		{0.4, "0.4s"},
		{3.42, "3.4s"},
		{59.9, "59.9s"},
		{65, "1m 5s"},
		{120, "2m"},
	}
	for _, c := range cases {
		if got := formatResumeElapsed(c.seconds); got != c.want {
			t.Errorf("formatResumeElapsed(%v) = %q, want %q", c.seconds, got, c.want)
		}
	}
}
