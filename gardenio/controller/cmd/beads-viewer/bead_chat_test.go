package main

// Unit tests for the bead-show message-bead chat-thread view-model
// (feature ka-0000007791 follow-on). These exercise the PURE builders in
// bead_chat.go without a daemon: the message-ref collection + dedup, the
// constellation-ref strip, the newest-N cap, and the oldest→newest ordering.

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

func msgRef(id, typ string) beadsapi.BeadReferenceSummary {
	return beadsapi.BeadReferenceSummary{ID: id, Type: typ}
}

// TestCollectMessageRefIDs_FiltersAndDedups verifies only type=message refs are
// collected, self is excluded, and an id appearing in both ref sets (forward +
// reverse) is returned once.
func TestCollectMessageRefIDs_FiltersAndDedups(t *testing.T) {
	self := "gd-self"
	forward := []beadsapi.BeadReferenceSummary{
		msgRef("gd-m1", "message"),
		msgRef("gd-task", "task"), // non-message — dropped
		msgRef(self, "message"),   // self — dropped
	}
	reverse := []beadsapi.BeadReferenceSummary{
		msgRef("gd-m1", "message"), // dup of forward — collapsed
		msgRef("gd-m2", "message"),
		msgRef("gd-agent", "agent"), // non-message — dropped
	}

	got := collectMessageRefIDs(self, forward, reverse)
	want := []string{"gd-m1", "gd-m2"}
	if len(got) != len(want) {
		t.Fatalf("collectMessageRefIDs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("collectMessageRefIDs[%d] = %q, want %q (full %v)", i, got[i], want[i], got)
		}
	}
}

// TestCollectMessageRefIDs_Empty returns an empty slice (not nil-panic) for no refs.
func TestCollectMessageRefIDs_Empty(t *testing.T) {
	if got := collectMessageRefIDs("gd-self"); len(got) != 0 {
		t.Errorf("collectMessageRefIDs(none) = %v, want empty", got)
	}
}

// TestStripMessageRefs drops only the message-type refs, preserving order.
func TestStripMessageRefs(t *testing.T) {
	in := []beadsapi.BeadReferenceSummary{
		msgRef("gd-a", "task"),
		msgRef("gd-m", "message"),
		msgRef("gd-b", "agent"),
		msgRef("gd-m2", "message"),
		msgRef("gd-c", "decision"),
	}
	got := stripMessageRefs(in)
	want := []string{"gd-a", "gd-b", "gd-c"}
	if len(got) != len(want) {
		t.Fatalf("stripMessageRefs len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i].ID != want[i] {
			t.Errorf("stripMessageRefs[%d].ID = %q, want %q", i, got[i].ID, want[i])
		}
	}
}

// TestBuildBeadChat_OrdersOldestToNewestAndProjects checks the chat view orders
// messages oldest→newest, projects literal text + author + kind, and reports the
// right counts. Un-hydrated / non-message ids are skipped.
func TestBuildBeadChat_OrdersOldestToNewestAndProjects(t *testing.T) {
	base := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	beads := map[string]*beadsapi.BeadDetail{
		"gd-newer": {
			ID: "gd-newer", Type: "message", Status: "closed",
			Labels:    []string{"squawk"},
			Fields:    map[string]string{"text": "second line", "source_agent": "agent-b"},
			CreatedAt: base.Add(10 * time.Minute),
		},
		"gd-older": {
			ID: "gd-older", Type: "message", Status: "closed",
			Fields:    map[string]string{"text": "first line", "source_agent": "agent-a"},
			CreatedAt: base,
		},
		"gd-notmsg": {ID: "gd-notmsg", Type: "task", Title: "not a message"},
		// gd-missing intentionally absent from the map (un-hydrated).
	}
	ids := []string{"gd-newer", "gd-older", "gd-notmsg", "gd-missing"}

	view := buildBeadChat(ids, beads, "/base", base.Add(time.Hour), LinkifyOpts{})
	if !view.HasMessages {
		t.Fatal("expected HasMessages=true")
	}
	if view.Shown != 2 || view.Total != 2 {
		t.Errorf("Shown=%d Total=%d, want 2/2", view.Shown, view.Total)
	}
	if view.OlderHidden {
		t.Errorf("OlderHidden=true unexpected for 2 messages")
	}
	// Oldest first.
	if view.Messages[0].ID != "gd-older" || view.Messages[1].ID != "gd-newer" {
		t.Errorf("order = [%s, %s], want [gd-older, gd-newer]", view.Messages[0].ID, view.Messages[1].ID)
	}
	if got := string(view.Messages[0].BodyHTML); !strings.Contains(got, "first line") {
		t.Errorf("first message body = %q, want it to contain 'first line'", got)
	}
	if view.Messages[0].Author != "agent-a" {
		t.Errorf("first author = %q, want agent-a", view.Messages[0].Author)
	}
	if view.Messages[1].Kind != "squawk" {
		t.Errorf("second kind = %q, want squawk", view.Messages[1].Kind)
	}
	if !strings.HasPrefix(view.Messages[0].Href, "/base/") {
		t.Errorf("href = %q, want /base/ prefix", view.Messages[0].Href)
	}
}

// TestBuildBeadChat_StampsDirection verifies the bead-view chat panel gets
// agent-vs-user attribution: a human Slack reply (slack-thread-reply label,
// slack-bridge author) lands from-user; an agent squawk (source_agent) lands
// from-agent (ka-0000000676).
func TestBuildBeadChat_StampsDirection(t *testing.T) {
	base := time.Date(2026, 7, 3, 9, 0, 0, 0, time.UTC)
	beads := map[string]*beadsapi.BeadDetail{
		"gd-agent": {
			ID: "gd-agent", Type: "message", Status: "closed",
			Labels:    []string{"from:agent-a", "squawk"},
			Fields:    map[string]string{"text": "on it", "source_agent": "agent-a"},
			CreatedBy: "agent-a", OperatedBy: "slack:U07",
			CreatedAt: base,
		},
		"gd-human": {
			ID: "gd-human", Type: "message", Status: "closed",
			Labels:    []string{"slack-thread-reply", "slack-thread-message"},
			Fields:    map[string]string{"text": "thanks, will check"},
			CreatedBy: "slack-bridge", OperatedBy: "oidc:matt@x", OperatedByDisplay: "Matt",
			CreatedAt: base.Add(time.Minute),
		},
	}
	ids := []string{"gd-agent", "gd-human"}
	view := buildBeadChat(ids, beads, "/base", base.Add(time.Hour), LinkifyOpts{})
	if view.Shown != 2 {
		t.Fatalf("Shown=%d, want 2", view.Shown)
	}
	byID := map[string]chatMessage{}
	for _, m := range view.Messages {
		byID[m.ID] = m
	}
	if byID["gd-agent"].Direction != directionFromAgent {
		t.Errorf("agent squawk Direction=%q, want %q", byID["gd-agent"].Direction, directionFromAgent)
	}
	if byID["gd-human"].Direction != directionFromUser {
		t.Errorf("human reply Direction=%q, want %q", byID["gd-human"].Direction, directionFromUser)
	}
	if byID["gd-human"].Kind != "slack" {
		t.Errorf("human reply Kind=%q, want slack", byID["gd-human"].Kind)
	}
	if byID["gd-agent"].Kind != "squawk" {
		t.Errorf("agent squawk Kind=%q, want squawk", byID["gd-agent"].Kind)
	}
}

// TestBuildBeadChat_CapsToNewest verifies the newest mapChatMaxMessages survive
// the cap and the dropped older count is reported.
func TestBuildBeadChat_CapsToNewest(t *testing.T) {
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	n := mapChatMaxMessages + 7
	beads := make(map[string]*beadsapi.BeadDetail, n)
	ids := make([]string, 0, n)
	for i := range n {
		id := "gd-m" + strconv.Itoa(i)
		ids = append(ids, id)
		beads[id] = &beadsapi.BeadDetail{
			ID: id, Type: "message", Status: "closed",
			Fields:    map[string]string{"text": "line " + strconv.Itoa(i)},
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
		}
	}
	view := buildBeadChat(ids, beads, "/base", base.Add(1000*time.Hour), LinkifyOpts{})
	if view.Total != n {
		t.Errorf("Total=%d, want %d", view.Total, n)
	}
	if view.Shown != mapChatMaxMessages {
		t.Errorf("Shown=%d, want %d", view.Shown, mapChatMaxMessages)
	}
	if !view.OlderHidden || view.HiddenCount != 7 {
		t.Errorf("OlderHidden=%v HiddenCount=%d, want true/7", view.OlderHidden, view.HiddenCount)
	}
	// The newest message (highest index) must survive at the tail.
	last := view.Messages[len(view.Messages)-1].ID
	if last != "gd-m"+strconv.Itoa(n-1) {
		t.Errorf("last shown = %q, want newest gd-m%d", last, n-1)
	}
}

// TestBuildBeadChat_EmptyInputs returns a zero view (panel hidden) for no ids
// or all-missing hydration.
func TestBuildBeadChat_EmptyInputs(t *testing.T) {
	if v := buildBeadChat(nil, nil, "/base", time.Now(), LinkifyOpts{}); v.HasMessages {
		t.Errorf("buildBeadChat(nil, LinkifyOpts{}) HasMessages=true, want false")
	}
	ids := []string{"gd-x"}
	beads := map[string]*beadsapi.BeadDetail{"gd-x": {ID: "gd-x", Type: "task"}}
	if v := buildBeadChat(ids, beads, "/base", time.Now(), LinkifyOpts{}); v.HasMessages {
		t.Errorf("buildBeadChat(non-message only, LinkifyOpts{}) HasMessages=true, want false")
	}
}

// TestAgentAuthoredMessagesQuery verifies the agent-authored-message fetch
// queries by the `from:<title>` label across both message + mail types,
// newest-first, capped at the panel limit (ka-0000002848).
func TestAgentAuthoredMessagesQuery(t *testing.T) {
	q := agentAuthoredMessagesQuery("my-agent-9xb")
	wantTypes := []string{"message", "mail"}
	if len(q.Types) != len(wantTypes) {
		t.Fatalf("Types = %v, want %v", q.Types, wantTypes)
	}
	for i, w := range wantTypes {
		if q.Types[i] != w {
			t.Errorf("Types[%d] = %q, want %q", i, q.Types[i], w)
		}
	}
	if len(q.Labels) != 1 || q.Labels[0] != "from:my-agent-9xb" {
		t.Errorf("Labels = %v, want [from:my-agent-9xb]", q.Labels)
	}
	if q.Sort != "-created_at" {
		t.Errorf("Sort = %q, want -created_at (newest-first)", q.Sort)
	}
	if q.Limit != mapChatMaxMessages {
		t.Errorf("Limit = %d, want %d", q.Limit, mapChatMaxMessages)
	}
}

// TestIsChatRenderableBeadDetail verifies the broadened render gate accepts both
// squawk (type=message) and agent-sent mail (type=mail), rejecting everything
// else and nil (ka-0000002848).
func TestIsChatRenderableBeadDetail(t *testing.T) {
	cases := []struct {
		name string
		bead *beadsapi.BeadDetail
		want bool
	}{
		{"message", &beadsapi.BeadDetail{Type: "message"}, true},
		{"mail", &beadsapi.BeadDetail{Type: "mail"}, true},
		{"task", &beadsapi.BeadDetail{Type: "task"}, false},
		{"agent", &beadsapi.BeadDetail{Type: "agent"}, false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		if got := isChatRenderableBeadDetail(tc.bead); got != tc.want {
			t.Errorf("isChatRenderableBeadDetail(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestBuildBeadChat_RendersMail verifies agent-sent mail (type=mail, carrying a
// `from:<name>` label and no source_agent) renders as a chat line with the
// "mail" kind badge and the from-label author — the agent-authored path feeds
// these in alongside squawks (ka-0000002848).
func TestBuildBeadChat_RendersMail(t *testing.T) {
	base := time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC)
	beads := map[string]*beadsapi.BeadDetail{
		"gd-mail": {
			ID: "gd-mail", Type: "mail", Status: "open",
			Labels:    []string{"from:agent-x"},
			Title:     "a mail subject",
			Fields:    map[string]string{"text": "the mail body"},
			CreatedAt: base,
		},
	}
	view := buildBeadChat([]string{"gd-mail"}, beads, "/base", base.Add(time.Hour), LinkifyOpts{})
	if !view.HasMessages || view.Shown != 1 {
		t.Fatalf("expected one rendered mail line, got HasMessages=%v Shown=%d", view.HasMessages, view.Shown)
	}
	m := view.Messages[0]
	if m.Kind != "mail" {
		t.Errorf("Kind = %q, want mail", m.Kind)
	}
	if m.Author != "agent-x" {
		t.Errorf("Author = %q, want agent-x (from label)", m.Author)
	}
	if got := string(m.BodyHTML); !strings.Contains(got, "the mail body") {
		t.Errorf("BodyHTML = %q, want it to contain 'the mail body'", got)
	}
}
