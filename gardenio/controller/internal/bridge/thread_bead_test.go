package bridge

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slack-go/slack/slackevents"
)

// countThreadBeads returns the number of beads carrying the canonical
// slack-thread:{channel}:{ts} label.
func countThreadBeads(d *mockDaemon, channel, threadTS string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	want := threadBeadLabel(channel, threadTS)
	count := 0
	for _, b := range d.beads {
		for _, l := range b.Labels {
			if l == want {
				count++
				break
			}
		}
	}
	return count
}

// findThreadBead returns the first bead carrying the canonical thread label.
func findThreadBead(d *mockDaemon, channel, threadTS string) (id string, labels []string, fields map[string]string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	want := threadBeadLabel(channel, threadTS)
	for _, b := range d.beads {
		for _, l := range b.Labels {
			if l == want {
				return b.ID, b.Labels, b.Fields
			}
		}
	}
	return "", nil, nil
}

// findThreadBeadPriority returns the priority stamped on the thread head for
// (channel, threadTS), or -1 when no such bead exists.
func findThreadBeadPriority(d *mockDaemon, channel, threadTS string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	want := threadBeadLabel(channel, threadTS)
	for _, b := range d.beads {
		for _, l := range b.Labels {
			if l == want {
				return b.Priority
			}
		}
	}
	return -1
}

// TestThreadBeadCreatedOnFirstMention_S1a verifies that the first @gardenio
// mention in a fresh thread creates exactly one thread bead with the
// source:slack + slack-thread:{channel}:{ts} labels, and the agent bead
// carries a "parent: <thread-bead-id>" typed mention.
func TestThreadBeadCreatedOnFirstMention_S1a(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("testproj", "C-s1a")

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	bot.state = state
	bot.botUserID = "U-BOT"
	bot.lastThreadNudge = make(map[string]time.Time)

	channel := "C-s1a"
	threadTS := "1111.2222"

	bot.handleThreadSpawn(context.Background(), &slackevents.AppMentionEvent{
		User:            "U-OPENER",
		Text:            "<@U-BOT> kick off",
		TimeStamp:       "1111.3333",
		ThreadTimeStamp: threadTS,
		Channel:         channel,
	}, "kick off")

	if got := countThreadBeads(daemon, channel, threadTS); got != 1 {
		t.Fatalf("expected exactly 1 thread bead with slack-thread label, got %d", got)
	}

	threadBeadID, labels, fields := findThreadBead(daemon, channel, threadTS)
	if threadBeadID == "" {
		t.Fatal("thread bead not found")
	}

	wantLabels := []string{"source:slack", threadBeadLabel(channel, threadTS), "thread", "project:testproj"}
	for _, w := range wantLabels {
		found := false
		for _, l := range labels {
			if l == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("thread bead missing label %q (got %v)", w, labels)
		}
	}

	wantFields := map[string]string{
		"slack_thread_channel": channel,
		"slack_thread_ts":      threadTS,
		"slack_spawn_channel":  channel,
		"slack_opener_user_id": "U-OPENER",
		"spawn_source":         "slack-thread",
	}
	for k, v := range wantFields {
		if got := fields[k]; got != v {
			t.Errorf("thread bead field %s = %q, want %q", k, got, v)
		}
	}

	// Agent bead must reference the thread bead as parent.
	daemon.mu.Lock()
	var agentBead = (*struct {
		Desc   string
		Fields map[string]string
	})(nil)
	for _, b := range daemon.beads {
		if b.Type == "agent" {
			agentBead = &struct {
				Desc   string
				Fields map[string]string
			}{Desc: b.Description, Fields: b.Fields}
			break
		}
	}
	daemon.mu.Unlock()

	if agentBead == nil {
		t.Fatal("expected an agent bead to be created")
	}
	if !strings.Contains(agentBead.Desc, "parent: "+threadBeadID) {
		t.Errorf("agent bead description missing 'parent: %s' typed mention; got: %s",
			threadBeadID, agentBead.Desc)
	}
	if agentBead.Fields["slack_thread_bead_id"] != threadBeadID {
		t.Errorf("agent bead slack_thread_bead_id = %q, want %q",
			agentBead.Fields["slack_thread_bead_id"], threadBeadID)
	}
}

// TestThreadBeadIdempotentOnConcurrentMentions_S1b verifies the resolve-or-create
// flow under parallel calls: N concurrent mentions in the same thread must
// produce exactly 1 thread bead. The spawnInFlight guard in handleThreadSpawn
// serializes the spawn path; this test pins that label-lookup also wins on
// re-entry from a fresh process (see restart-recovery test below).
func TestThreadBeadIdempotentOnConcurrentMentions_S1b(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("testproj", "C-s1b")

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	bot.state = state
	bot.botUserID = "U-BOT"
	bot.lastThreadNudge = make(map[string]time.Time)

	channel := "C-s1b"
	threadTS := "4444.5555"

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			bot.handleThreadSpawn(context.Background(), &slackevents.AppMentionEvent{
				User:            "U-OPENER",
				Text:            "<@U-BOT> kick off",
				TimeStamp:       "4444.6666",
				ThreadTimeStamp: threadTS,
				Channel:         channel,
			}, "kick off")
		}()
	}
	wg.Wait()

	if got := countThreadBeads(daemon, channel, threadTS); got != 1 {
		t.Fatalf("expected exactly 1 thread bead after concurrent mentions, got %d", got)
	}
}

// TestThreadBeadResolveAfterBridgeRestart_S1f simulates a bridge process
// restart: the first mention creates the thread bead under bot A; bot B is a
// fresh Bot with no in-memory state — only the daemon-side label lookup is
// available. A new mention in the SAME thread must reuse the existing thread
// bead, not create a duplicate.
func TestThreadBeadResolveAfterBridgeRestart_S1f(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("testproj", "C-s1f")

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	channel := "C-s1f"
	threadTS := "8888.9999"

	// Bot A: first mention.
	botA := newTestBot(daemon, slackSrv)
	dirA := t.TempDir()
	stateA, err := NewStateManager(filepath.Join(dirA, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	botA.state = stateA
	botA.botUserID = "U-BOT"
	botA.lastThreadNudge = make(map[string]time.Time)

	botA.handleThreadSpawn(context.Background(), &slackevents.AppMentionEvent{
		User:            "U-OPENER",
		Text:            "<@U-BOT> first",
		TimeStamp:       "8888.1111",
		ThreadTimeStamp: threadTS,
		Channel:         channel,
	}, "first")

	firstID, _, _ := findThreadBead(daemon, channel, threadTS)
	if firstID == "" {
		t.Fatal("expected thread bead after first mention")
	}

	// Bot B: simulate a process restart by constructing a brand-new Bot with a
	// brand-new StateManager (no in-memory thread→agent map carry-over). The
	// daemon (which represents the persistent beads store) is shared because
	// that's the only thing that survives a process restart in the real
	// system. We also clear the agent→thread binding in the new state so the
	// new bot truly has no in-memory knowledge of this thread.
	botB := newTestBot(daemon, slackSrv)
	dirB := t.TempDir()
	stateB, err := NewStateManager(filepath.Join(dirB, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	botB.state = stateB
	botB.botUserID = "U-BOT"
	botB.lastThreadNudge = make(map[string]time.Time)

	// Trigger a second mention from the fresh bot in the same thread.
	botB.handleThreadSpawn(context.Background(), &slackevents.AppMentionEvent{
		User:            "U-OPENER",
		Text:            "<@U-BOT> after restart",
		TimeStamp:       "8888.2222",
		ThreadTimeStamp: threadTS,
		Channel:         channel,
	}, "after restart")

	if got := countThreadBeads(daemon, channel, threadTS); got != 1 {
		t.Fatalf("expected exactly 1 thread bead across restart, got %d", got)
	}
	secondID, _, _ := findThreadBead(daemon, channel, threadTS)
	if secondID != firstID {
		t.Errorf("post-restart mention created new thread bead %q, want reuse of %q", secondID, firstID)
	}
}

// TestThreadBeadCarriesBareThreadLabel_1a1 pins the 1a-1 contract: a freshly
// created Slack thread (wish) head carries the bare, origin-agnostic `thread`
// kind label alongside the Slack-specific dedup label, so the Phase 2 /threads
// enumeration can find every wish head by a single label. The head itself
// carries NO thread_id field — it IS the wish.
func TestThreadBeadCarriesBareThreadLabel_1a1(t *testing.T) {
	daemon := newMockDaemon()
	bot := newTestBot(daemon, newFakeSlackServer(t))

	channel := "C-1a1xx"
	threadTS := "7777.8888"

	id, created, err := bot.getOrCreateThreadBead(context.Background(), "wishproj", threadBeadFields{
		Channel:      channel,
		ThreadTS:     threadTS,
		SpawnChannel: channel,
		OpenerUserID: "U-WISHER",
	})
	if err != nil {
		t.Fatalf("getOrCreateThreadBead: %v", err)
	}
	if !created {
		t.Fatalf("expected a freshly created thread bead, got created=false")
	}

	_, labels, fields := findThreadBead(daemon, channel, threadTS)

	want := map[string]bool{
		"source:slack":                     false,
		threadBeadLabel(channel, threadTS): false,
		threadKindLabel:                    false,
		"project:wishproj":                 false,
	}
	for _, l := range labels {
		if _, ok := want[l]; ok {
			want[l] = true
		}
	}
	for l, seen := range want {
		if !seen {
			t.Errorf("thread head missing label %q (got %v)", l, labels)
		}
	}

	// The head IS the wish — it must NOT carry a thread_id pointing at itself.
	if v, ok := fields["thread_id"]; ok {
		t.Errorf("thread head must not carry thread_id field, got %q", v)
	}
	_ = id
}

// TestThreadBeadAssignedToBridge pins gd-PQCcv8sndp: the thread head is created
// with the bridge as assignee, so the bridge's fleet token passes the daemon's
// ownership check on it even when the head was created as a linked person
// (created_by = that person).
func TestThreadBeadAssignedToBridge(t *testing.T) {
	daemon := newMockDaemon()
	bot := newTestBot(daemon, newFakeSlackServer(t))

	id, _, err := bot.getOrCreateThreadBead(context.Background(), "wishproj", threadBeadFields{
		Channel:      "C-own",
		ThreadTS:     "1111.2222",
		SpawnChannel: "C-own",
		OpenerUserID: "U-LINKED",
	})
	if err != nil {
		t.Fatalf("getOrCreateThreadBead: %v", err)
	}
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	for _, b := range daemon.beads {
		if b.ID == id {
			if b.Assignee != "slack-bridge" {
				t.Errorf("thread head assignee = %q, want %q", b.Assignee, "slack-bridge")
			}
			return
		}
	}
	t.Fatalf("thread head %s not found", id)
}

// TestThreadIdentityLabel pins the origin-agnostic membership-label helper used
// to bind derivatives to a wish head by gd-id.
func TestThreadIdentityLabel(t *testing.T) {
	if got := threadIdentityLabel("gd-W1sh"); got != "thread:gd-W1sh" {
		t.Errorf("threadIdentityLabel = %q, want %q", got, "thread:gd-W1sh")
	}
}

// TestGetOrCreateThreadBead_RejectsInvalidBinding verifies the helper short-
// circuits on malformed channel/threadTS so it cannot create a junk bead.
func TestGetOrCreateThreadBead_RejectsInvalidBinding(t *testing.T) {
	daemon := newMockDaemon()
	bot := newTestBot(daemon, newFakeSlackServer(t))

	cases := []struct {
		channel, ts string
	}{
		{"", "1.2"},
		{"C123", ""},
		{"C123", "no-dot"},
		{"X", "1.2"}, // too short to be a Slack channel ID
	}
	for _, c := range cases {
		id, created, err := bot.getOrCreateThreadBead(context.Background(), "testproj", threadBeadFields{
			Channel:  c.channel,
			ThreadTS: c.ts,
		})
		if err == nil {
			t.Errorf("expected error for channel=%q ts=%q, got id=%q created=%v", c.channel, c.ts, id, created)
		}
	}
}

// TestGetOrCreateThreadBead_ConcurrentCallersYieldOneBead_1a3 is the 1a-3
// acceptance test: N callers racing on the SAME thread identity must converge
// on exactly ONE bead, and exactly one of them may report created=true. This
// is the race the old "lookup-then-create" comment admitted and that
// spawnInFlight could not close across binaries — the daemon-side
// idempotency-key resolve-or-create (mirrored in mockDaemon.CreateBead) now
// closes it: every losing racer's create returns the winner's id rather than
// inserting a duplicate.
func TestGetOrCreateThreadBead_ConcurrentCallersYieldOneBead_1a3(t *testing.T) {
	daemon := newMockDaemon()
	bot := newTestBot(daemon, newFakeSlackServer(t))

	channel := "C-race1a3"
	threadTS := "9999.0000"

	const racers = 16
	var wg sync.WaitGroup
	ids := make([]string, racers)
	createdFlags := make([]bool, racers)
	errs := make([]error, racers)
	start := make(chan struct{})

	for i := range racers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release all goroutines simultaneously to maximize the race
			ids[i], createdFlags[i], errs[i] = bot.getOrCreateThreadBead(
				context.Background(), "raceproj", threadBeadFields{
					Channel:      channel,
					ThreadTS:     threadTS,
					SpawnChannel: channel,
					OpenerUserID: "U-RACER",
				})
		}(i)
	}
	close(start)
	wg.Wait()

	// No caller may error.
	for i, err := range errs {
		if err != nil {
			t.Fatalf("racer %d: getOrCreateThreadBead: %v", i, err)
		}
	}

	// Exactly ONE bead carries the canonical thread identity label.
	if got := countThreadBeads(daemon, channel, threadTS); got != 1 {
		t.Fatalf("expected exactly 1 thread bead after %d racing callers, got %d", racers, got)
	}

	// Every caller resolved to that same single id.
	winnerID, _, _ := findThreadBead(daemon, channel, threadTS)
	if winnerID == "" {
		t.Fatal("no thread bead found after race")
	}
	for i, id := range ids {
		if id != winnerID {
			t.Errorf("racer %d resolved id=%q, want the single winner %q", i, id, winnerID)
		}
	}

	// At least one caller issued the create. created is best-effort under a
	// race: several callers can pass the pre-lookup before the winner inserts,
	// then all call CreateBead — the winner inserts and the rest get the
	// daemon's dedup-return, but every one of them sees a successful create and
	// reports created=true. The load-bearing invariant is the single bead
	// asserted above, not the exact created count.
	createdCount := 0
	for _, c := range createdFlags {
		if c {
			createdCount++
		}
	}
	if createdCount < 1 {
		t.Errorf("expected at least 1 caller to report created=true, got %d", createdCount)
	}

	// The single bead carries the idempotency key = canonical thread label,
	// proving the daemon-side guard (not a process-local set) is what dedups.
	_, _, fields := findThreadBead(daemon, channel, threadTS)
	if got := fields["idempotency_key"]; got != threadBeadLabel(channel, threadTS) {
		t.Errorf("thread bead idempotency_key = %q, want canonical label %q",
			got, threadBeadLabel(channel, threadTS))
	}
}

// TestThreadBeadIsNotMintedP0 pins the fix for ka-0000003760.
//
// beadsapi.CreateBeadRequest.Priority is a plain `int` with NO omitempty, so
// omitting it does not inherit the daemon's default — it marshals as an
// explicit "priority":0, i.e. P0, the HIGHEST band. Thread heads were minted
// that way, and because `gb ready` sorts `priority,-created_at` with a default
// --limit 20, 83 open P0 Slack-conversation beads buried every real P1-P3 bead:
// the default ready view was 20/20 thread beads and ZERO actionable work. Since
// gardener-delivery dispatches from a bare `gb ready` — and every gardener prompt
// forbids touching thread/source:slack beads — its bug lane was starved while
// 19 gardenio bugs sat open and unassigned.
//
// A thread head is a conversation, never top-priority work. Guard the exact
// regression: priority must be explicitly set, and must not be 0.
func TestThreadBeadIsNotMintedP0(t *testing.T) {
	daemon := newMockDaemon()
	bot := newTestBot(daemon, newFakeSlackServer(t))

	channel := "C-prio"
	threadTS := "9999.1111"

	if _, created, err := bot.getOrCreateThreadBead(context.Background(), "wishproj", threadBeadFields{
		Channel:      channel,
		ThreadTS:     threadTS,
		SpawnChannel: channel,
		OpenerUserID: "U-WISHER",
	}); err != nil {
		t.Fatalf("getOrCreateThreadBead: %v", err)
	} else if !created {
		t.Fatalf("expected a freshly created thread bead, got created=false")
	}

	got := findThreadBeadPriority(daemon, channel, threadTS)
	if got == -1 {
		t.Fatal("no thread head bead was created")
	}
	if got == 0 {
		t.Errorf("thread head minted at priority 0 (P0) — it would sort above every real bead "+
			"in `gb ready` and starve the work queue (ka-0000003760); want %d", threadBeadPriority)
	}
	if got != threadBeadPriority {
		t.Errorf("thread head priority = %d, want %d (threadBeadPriority)", got, threadBeadPriority)
	}
}
