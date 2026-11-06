package bridge

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/mailproto"
)

// newTestBotWithRespawnHook is a Bot over daemon whose watchMailDelivery
// respawn goes to hook, and whose Slack posts land on a test server whose
// message texts posted() returns.
func newTestBotWithRespawnHook(t *testing.T, daemon *mockDaemon, hook func(agent string)) (*Bot, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var posts []string
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(r.URL.Path, "chat.postMessage") {
			mu.Lock()
			posts = append(posts, string(body))
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"ts":"171.2"}`))
	}))
	t.Cleanup(slackSrv.Close)
	b := newTestBot(daemon, slackSrv)
	b.respawnHook = hook
	return b, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), posts...)
	}
}

func shortMailRespawn(t *testing.T) {
	t.Helper()
	orig := MailRespawnAfter
	MailRespawnAfter = 10 * time.Millisecond
	t.Cleanup(func() { MailRespawnAfter = orig })
}

func TestWatchMailDelivery_PodAgent_OpenMailAfterDeadline_Respawns(t *testing.T) {
	shortMailRespawn(t)
	daemon := newMockDaemon()
	daemon.beads["crew-x"] = &beadsapi.BeadDetail{ID: "gd-a1", Title: "crew-x", Status: "open", Fields: map[string]string{"agent_state": "working", "execution_target": "k8s"}}
	daemon.beads["gd-m1"] = &beadsapi.BeadDetail{ID: "gd-m1", Type: "mail", Status: "open", Assignee: "crew-x"}
	respawned := make(chan string, 1)
	b, _ := newTestBotWithRespawnHook(t, daemon, func(agent string) { respawned <- agent })

	b.watchMailDelivery(context.Background(), "gd-m1", "crew-x", "C1", "171.1", "hello", "U1")
	select {
	case got := <-respawned:
		if got != "crew-x" {
			t.Fatalf("respawned %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("pod agent with undelivered urgent mail must be respawned")
	}
	// The respawn re-injects the text, so the mail is closed rather than
	// drained into the resumed session a second time.
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.closed) != 1 || daemon.closed[0].BeadID != "gd-m1" {
		t.Fatalf("closed = %+v, want the undelivered mail gd-m1 closed", daemon.closed)
	}
}

func TestWatchMailDelivery_LocalAgent_NeverRespawns(t *testing.T) {
	// gb agent start --local writes "laptop"; "local" is the spec's spelling.
	for _, target := range []string{"laptop", "local"} {
		t.Run(target, func(t *testing.T) { testWatchMailDeliveryLocalAgent(t, target) })
	}
}

func testWatchMailDeliveryLocalAgent(t *testing.T, target string) {
	shortMailRespawn(t)
	daemon := newMockDaemon()
	daemon.beads["local-laptop"] = &beadsapi.BeadDetail{ID: "gd-a2", Title: "local-laptop", Status: "open", Fields: map[string]string{"agent_state": "paused", "execution_target": target}}
	daemon.beads["gd-m2"] = &beadsapi.BeadDetail{ID: "gd-m2", Type: "mail", Status: "open", Assignee: "local-laptop"}
	respawned := make(chan string, 1)
	b, posted := newTestBotWithRespawnHook(t, daemon, func(agent string) { respawned <- agent })

	b.watchMailDelivery(context.Background(), "gd-m2", "local-laptop", "C1", "171.1", "hello", "U1")
	select {
	case <-respawned:
		t.Fatal("a local agent is never respawned (D5: hold on disconnect)")
	case <-time.After(100 * time.Millisecond):
	}
	posts := posted()
	if len(posts) != 1 || !strings.Contains(posts[0], "Work+is+held") && !strings.Contains(posts[0], "Work is held") {
		t.Fatalf("want one held note in the thread, got %q", posts)
	}
}

func TestWatchMailDelivery_MailRead_NoAction(t *testing.T) {
	shortMailRespawn(t)
	daemon := newMockDaemon()
	daemon.beads["crew-x"] = &beadsapi.BeadDetail{ID: "gd-a1", Title: "crew-x", Status: "open", Fields: map[string]string{"agent_state": "working"}}
	daemon.beads["gd-m3"] = &beadsapi.BeadDetail{ID: "gd-m3", Type: "mail", Status: "closed", Assignee: "crew-x"}
	respawned := make(chan string, 1)
	b, _ := newTestBotWithRespawnHook(t, daemon, func(agent string) { respawned <- agent })

	b.watchMailDelivery(context.Background(), "gd-m3", "crew-x", "C1", "171.1", "hello", "U1")
	select {
	case <-respawned:
		t.Fatal("read mail must not trigger respawn")
	case <-time.After(100 * time.Millisecond):
	}
}

// Mail stays open until the agent reads it; the subscriber's delivered_at
// stamp is what says a live pod took it.
func TestWatchMailDelivery_MailDeliveredButUnread_NoAction(t *testing.T) {
	shortMailRespawn(t)
	daemon := newMockDaemon()
	daemon.beads["crew-x"] = &beadsapi.BeadDetail{ID: "gd-a1", Title: "crew-x", Status: "open", Fields: map[string]string{"agent_state": "working"}}
	daemon.beads["gd-m4"] = &beadsapi.BeadDetail{ID: "gd-m4", Type: "mail", Status: "open", Assignee: "crew-x",
		Fields: map[string]string{mailproto.FieldDeliveredAt: "2026-10-29T03:00:00Z"}}
	respawned := make(chan string, 1)
	b, _ := newTestBotWithRespawnHook(t, daemon, func(agent string) { respawned <- agent })

	b.watchMailDelivery(context.Background(), "gd-m4", "crew-x", "C1", "171.1", "hello", "U1")
	select {
	case <-respawned:
		t.Fatal("mail a live pod took must not trigger respawn")
	case <-time.After(100 * time.Millisecond):
	}
}

// A booting agent's subscriber drains unread mail when it starts; closing its
// bead to respawn would orphan the real pod (ka-0000000292).
func TestWatchMailDelivery_StartingUp_NoAction(t *testing.T) {
	shortMailRespawn(t)
	daemon := newMockDaemon()
	daemon.beads["crew-x"] = &beadsapi.BeadDetail{ID: "gd-a1", Title: "crew-x", Status: "open", Fields: map[string]string{"agent_state": "spawning"}}
	daemon.beads["gd-m5"] = &beadsapi.BeadDetail{ID: "gd-m5", Type: "mail", Status: "open", Assignee: "crew-x"}
	respawned := make(chan string, 1)
	b, _ := newTestBotWithRespawnHook(t, daemon, func(agent string) { respawned <- agent })

	b.watchMailDelivery(context.Background(), "gd-m5", "crew-x", "C1", "171.1", "hello", "U1")
	select {
	case <-respawned:
		t.Fatal("a starting-up agent must not be respawned")
	case <-time.After(100 * time.Millisecond):
	}
}
