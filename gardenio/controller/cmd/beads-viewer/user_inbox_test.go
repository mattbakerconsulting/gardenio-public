package main

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// mkMsg is a tiny helper for building message/mail beads in inbox tests.
func mkInboxBead(id, typ, status string, created time.Time, labels []string, fields map[string]string, title, desc string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID:          id,
		Type:        typ,
		Status:      status,
		CreatedAt:   created,
		UpdatedAt:   created,
		Labels:      labels,
		Fields:      fields,
		Title:       title,
		Description: desc,
	}
}

// TestAssembleUserInbox_DirectionAndOrder verifies the merged feed stamps the
// right direction per population and renders oldest→newest.
func TestAssembleUserInbox_DirectionAndOrder(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	t0 := now.Add(-3 * time.Hour)
	t1 := now.Add(-2 * time.Hour)
	t2 := now.Add(-1 * time.Hour)

	agentMail := mkInboxBead("gd-mail1", "mail", "closed", t0,
		[]string{"from:gardener-x"}, nil, "Release done", "the release shipped")
	userReply := mkInboxBead("gd-reply1", "task", "open", t1,
		[]string{"slack-thread-reply"}, map[string]string{"operated_by": "oidc:matt@x"}, "Thread: thanks", "thanks team")
	agentSquawk := mkInboxBead("gd-sq1", "message", "closed", t2,
		[]string{"squawk", "from:gardener-x"}, map[string]string{"text": "build green"}, "", "")

	sources := []inboxSource{
		{beads: []*beadsapi.BeadDetail{agentMail, agentSquawk}, direction: directionFromAgent, incoming: true},
		{beads: []*beadsapi.BeadDetail{userReply}, direction: directionFromUser, incoming: false},
	}
	chat, _, senders := assembleUserInbox(sources, "", now, time.Time{})

	if !chat.HasMessages || chat.Total != 3 {
		t.Fatalf("expected 3 messages, got total=%d has=%v", chat.Total, chat.HasMessages)
	}
	// Oldest→newest: mail(t0), reply(t1), squawk(t2).
	wantIDs := []string{"gd-mail1", "gd-reply1", "gd-sq1"}
	wantDir := []string{directionFromAgent, directionFromUser, directionFromAgent}
	for i, m := range chat.Messages {
		if m.ID != wantIDs[i] {
			t.Errorf("msg[%d] id = %q, want %q", i, m.ID, wantIDs[i])
		}
		if m.Direction != wantDir[i] {
			t.Errorf("msg[%d] direction = %q, want %q", i, m.Direction, wantDir[i])
		}
	}
	// Mail kind badge + from-label author.
	if chat.Messages[0].Kind != "mail" {
		t.Errorf("mail kind = %q, want mail", chat.Messages[0].Kind)
	}
	if chat.Messages[0].Author != "gardener-x" {
		t.Errorf("mail author = %q, want gardener-x", chat.Messages[0].Author)
	}
	// Senders are de-duped + sorted (gardener-x once, plus the user-reply author).
	if len(senders) == 0 {
		t.Errorf("expected non-empty sender set")
	}
}

// TestAssembleUserInbox_Unread covers the unread rules: open mail unread, closed
// mail read, squawk after last-visit unread, squawk before read, outgoing never
// unread.
func TestAssembleUserInbox_Unread(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	lastVisit := now.Add(-90 * time.Minute)

	openMail := mkInboxBead("gd-m-open", "mail", "open", now.Add(-3*time.Hour), []string{"from:a"}, nil, "hi", "body")
	closedMail := mkInboxBead("gd-m-closed", "mail", "closed", now.Add(-3*time.Hour), []string{"from:a"}, nil, "hi", "body")
	newSquawk := mkInboxBead("gd-sq-new", "message", "closed", now.Add(-30*time.Minute), []string{"squawk"}, map[string]string{"text": "new"}, "", "")
	oldSquawk := mkInboxBead("gd-sq-old", "message", "closed", now.Add(-2*time.Hour), []string{"squawk"}, map[string]string{"text": "old"}, "", "")
	userReply := mkInboxBead("gd-reply", "task", "open", now.Add(-10*time.Minute), []string{"slack-thread-reply"}, nil, "Thread: x", "x")

	sources := []inboxSource{
		{beads: []*beadsapi.BeadDetail{openMail, closedMail, newSquawk, oldSquawk}, direction: directionFromAgent, incoming: true},
		{beads: []*beadsapi.BeadDetail{userReply}, direction: directionFromUser, incoming: false},
	}
	chat, unread, _ := assembleUserInbox(sources, "", now, lastVisit)

	want := map[string]bool{
		"gd-m-open":   true,  // open mail
		"gd-m-closed": false, // closed mail = read
		"gd-sq-new":   true,  // after last visit
		"gd-sq-old":   false, // before last visit
		"gd-reply":    false, // outgoing never unread
	}
	got := map[string]bool{}
	for _, m := range chat.Messages {
		got[m.ID] = m.Unread
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("unread[%s] = %v, want %v", id, got[id], w)
		}
	}
	if unread != 2 {
		t.Errorf("unread count = %d, want 2", unread)
	}
}

// TestAssembleUserInbox_BodiesAlwaysRendered locks in the flat-org transparency
// rule (ka-0000003111): there is NO cross-user redaction. Every line's body —
// mail, the operator's own outgoing reply, and agent squawks alike — renders in
// full regardless of who is viewing. This is the inverse of the old §3.6
// behaviour, which hid mail/outgoing bodies for a non-self viewer.
func TestAssembleUserInbox_BodiesAlwaysRendered(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	mail := mkInboxBead("gd-m", "mail", "open", now.Add(-3*time.Hour), []string{"from:a"}, nil, "Subject only", "FULL MAIL BODY")
	userReply := mkInboxBead("gd-r", "task", "open", now.Add(-2*time.Hour), []string{"slack-thread-reply"}, nil, "Thread: t", "FULL REPLY BODY")
	squawk := mkInboxBead("gd-s", "message", "closed", now.Add(-time.Hour), []string{"squawk"}, map[string]string{"text": "public squawk"}, "", "")

	sources := []inboxSource{
		{beads: []*beadsapi.BeadDetail{mail, squawk}, direction: directionFromAgent, incoming: true},
		{beads: []*beadsapi.BeadDetail{userReply}, direction: directionFromUser, incoming: false},
	}
	chat, _, _ := assembleUserInbox(sources, "", now, time.Time{})

	bodies := map[string]string{}
	for _, m := range chat.Messages {
		bodies[m.ID] = string(m.BodyHTML)
	}
	if !strings.Contains(bodies["gd-m"], "FULL MAIL BODY") {
		t.Errorf("mail body must render in full (no redaction), got %q", bodies["gd-m"])
	}
	if !strings.Contains(bodies["gd-r"], "FULL REPLY BODY") {
		t.Errorf("outgoing reply body must render in full (no redaction), got %q", bodies["gd-r"])
	}
	if !strings.Contains(bodies["gd-s"], "public squawk") {
		t.Errorf("agent squawk body must render in full, got %q", bodies["gd-s"])
	}
}

// TestAssembleUserInbox_Dedup ensures a bead appearing in two populations is
// rendered once.
func TestAssembleUserInbox_Dedup(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	b := mkInboxBead("gd-dup", "message", "closed", now.Add(-time.Hour), []string{"squawk"}, map[string]string{"text": "once"}, "", "")
	sources := []inboxSource{
		{beads: []*beadsapi.BeadDetail{b}, direction: directionFromAgent, incoming: true},
		{beads: []*beadsapi.BeadDetail{b}, direction: directionFromAgent, incoming: true},
	}
	chat, _, _ := assembleUserInbox(sources, "", now, time.Time{})
	if chat.Total != 1 {
		t.Fatalf("expected dedup to 1, got %d", chat.Total)
	}
}

// TestAssembleUserInbox_Empty returns a zero view when there are no beads.
func TestAssembleUserInbox_Empty(t *testing.T) {
	chat, unread, senders := assembleUserInbox(nil, "", time.Now(), time.Time{})
	if chat.HasMessages || chat.Total != 0 || unread != 0 || senders != nil {
		t.Errorf("expected empty view, got has=%v total=%d unread=%d senders=%v",
			chat.HasMessages, chat.Total, unread, senders)
	}
}

// TestGroupInboxByAgent_GroupsAndOrders is the core redesign coverage
// (ka-0000001181): the flat oldest→newest feed folds into one card per author,
// cards order by most-recent activity (freshest counterpart first), and each
// card's own message list stays oldest→newest.
func TestGroupInboxByAgent_GroupsAndOrders(t *testing.T) {
	// Global chat-log order (oldest→newest), as assembleUserInbox emits:
	//   a1(alpha) → b1(beta) → a2(alpha) → c1(gamma) → b2(beta)
	// Last-appearance order: gamma(idx3) < alpha(idx2) < beta(idx4) ⇒
	// freshest-first cards = beta, gamma, alpha.
	msgs := []chatMessage{
		{ID: "a1", Author: "alpha", Ago: "5h", TimestampRFC: "t-a1"},
		{ID: "b1", Author: "beta", Ago: "4h", TimestampRFC: "t-b1"},
		{ID: "a2", Author: "alpha", Ago: "3h", TimestampRFC: "t-a2"},
		{ID: "c1", Author: "gamma", Ago: "2h", TimestampRFC: "t-c1"},
		{ID: "b2", Author: "beta", Ago: "1h", TimestampRFC: "t-b2"},
	}
	cards := groupInboxByAgent(msgs)

	if len(cards) != 3 {
		t.Fatalf("expected 3 per-agent cards, got %d", len(cards))
	}
	// Freshest-first card ordering.
	wantAuthors := []string{"beta", "gamma", "alpha"}
	for i, w := range wantAuthors {
		if cards[i].Author != w {
			t.Errorf("card[%d].Author = %q, want %q", i, cards[i].Author, w)
		}
	}
	// alpha's card holds both its lines, oldest→newest.
	var alpha inboxAgentCard
	for _, c := range cards {
		if c.Author == "alpha" {
			alpha = c
		}
	}
	if alpha.Count != 2 || len(alpha.Messages) != 2 {
		t.Fatalf("alpha card count = %d / msgs = %d, want 2/2", alpha.Count, len(alpha.Messages))
	}
	if alpha.Messages[0].ID != "a1" || alpha.Messages[1].ID != "a2" {
		t.Errorf("alpha messages out of chat-log order: %q,%q", alpha.Messages[0].ID, alpha.Messages[1].ID)
	}
	// Card latest-activity stamp = newest line's Ago/RFC.
	if alpha.LatestAgo != "3h" || alpha.LatestRFC != "t-a2" {
		t.Errorf("alpha latest = %q/%q, want 3h/t-a2", alpha.LatestAgo, alpha.LatestRFC)
	}
}

// TestGroupInboxByAgent_UnreadPerCard verifies unread lines tally into the
// owning card's UnreadCount and nowhere else.
func TestGroupInboxByAgent_UnreadPerCard(t *testing.T) {
	msgs := []chatMessage{
		{ID: "x1", Author: "alpha", Unread: true},
		{ID: "x2", Author: "alpha", Unread: false},
		{ID: "y1", Author: "beta", Unread: true},
		{ID: "y2", Author: "beta", Unread: true},
	}
	byAuthor := map[string]inboxAgentCard{}
	for _, c := range groupInboxByAgent(msgs) {
		byAuthor[c.Author] = c
	}
	if got := byAuthor["alpha"].UnreadCount; got != 1 {
		t.Errorf("alpha UnreadCount = %d, want 1", got)
	}
	if got := byAuthor["beta"].UnreadCount; got != 2 {
		t.Errorf("beta UnreadCount = %d, want 2", got)
	}
}

// TestGroupInboxByAgent_Empty returns nil for an empty feed (mirrors the
// assembleUserInbox empty contract).
func TestGroupInboxByAgent_Empty(t *testing.T) {
	if cards := groupInboxByAgent(nil); cards != nil {
		t.Errorf("expected nil cards for empty feed, got %v", cards)
	}
}

// TestGroupInboxByAgent_UnknownAuthor keeps an empty-author line as its own
// card rather than dropping it (the template renders "" as "unknown").
func TestGroupInboxByAgent_UnknownAuthor(t *testing.T) {
	cards := groupInboxByAgent([]chatMessage{
		{ID: "n1", Author: ""},
		{ID: "n2", Author: ""},
	})
	if len(cards) != 1 || cards[0].Author != "" || cards[0].Count != 2 {
		t.Fatalf("expected one empty-author card of 2, got %+v", cards)
	}
}

// TestHandleUserInbox_RendersAgentCards asserts the /u/{slug}/inbox page groups
// the feed into per-agent cards in the rendered HTML: two distinct authors ⇒ two
// cards, each tagged with its counterpart handle (redesign ka-0000001181).
func TestHandleUserInbox_RendersAgentCards(t *testing.T) {
	now := time.Now().UTC()
	f := newFake()
	f.slugResp = map[string]*beadsapi.LookupOperatorBySlugResponse{
		"alice": {OperatorID: 42, Found: true},
	}
	// Two message beads from two distinct counterparts. They flow through the
	// inbox populations (deduped) and must land in two separate cards.
	f.listResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{
		mkInboxBead("gd-msg-a", "message", "closed", now.Add(-2*time.Hour),
			[]string{"squawk"}, map[string]string{"source_agent": "gardener-a", "text": "from A"}, "", ""),
		mkInboxBead("gd-msg-b", "message", "closed", now.Add(-1*time.Hour),
			[]string{"squawk"}, map[string]string{"source_agent": "gardener-b", "text": "from B"}, "", ""),
	}}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/u/alice/inbox", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	if n := strings.Count(body, `class="user-inbox-card"`); n != 2 {
		t.Errorf("expected 2 per-agent cards, got %d; body=%s", n, body)
	}
	for _, agent := range []string{`data-agent="gardener-a"`, `data-agent="gardener-b"`} {
		if !strings.Contains(body, agent) {
			t.Errorf("expected card tagged %s; body=%s", agent, body)
		}
	}
	// Both message bodies still render in full (the "show bodies" half stays satisfied).
	for _, want := range []string{"from A", "from B"} {
		if !strings.Contains(body, want) {
			t.Errorf("expected message body %q rendered; body=%s", want, body)
		}
	}
}

// TestMailHelpers covers mailFromLabel + messageKind + messageAuthor for mail.
func TestMailHelpers(t *testing.T) {
	mail := &beadsapi.BeadDetail{Type: "mail", Labels: []string{"delivery:interrupt", "from:agent-bob"}}
	if got := mailFromLabel(mail); got != "agent-bob" {
		t.Errorf("mailFromLabel = %q, want agent-bob", got)
	}
	if got := messageKind(mail); got != "mail" {
		t.Errorf("messageKind = %q, want mail", got)
	}
	if got := messageAuthor(mail); got != "agent-bob" {
		t.Errorf("messageAuthor = %q, want agent-bob", got)
	}
	// No from: label → mailFromLabel empty, falls through.
	plain := &beadsapi.BeadDetail{Type: "message", Fields: map[string]string{"source_agent": "gardener-z"}}
	if mailFromLabel(plain) != "" {
		t.Errorf("mailFromLabel should be empty for non-mail without from: label")
	}
	if messageAuthor(plain) != "gardener-z" {
		t.Errorf("messageAuthor should fall through to source_agent")
	}
}

// TestParseInboxLastVisit covers the field reader's tolerant parsing.
func TestParseInboxLastVisit(t *testing.T) {
	want := time.Date(2026, 6, 10, 11, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		bead *beadsapi.BeadDetail
		zero bool
	}{
		{"nil", nil, true},
		{"nil-fields", &beadsapi.BeadDetail{}, true},
		{"missing", &beadsapi.BeadDetail{Fields: map[string]string{"x": "y"}}, true},
		{"garbage", &beadsapi.BeadDetail{Fields: map[string]string{inboxLastVisitField: "not-a-time"}}, true},
		{"valid", &beadsapi.BeadDetail{Fields: map[string]string{inboxLastVisitField: want.Format(time.RFC3339)}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseInboxLastVisit(tc.bead)
			if tc.zero && !got.IsZero() {
				t.Errorf("expected zero, got %v", got)
			}
			if !tc.zero && !got.Equal(want) {
				t.Errorf("got %v, want %v", got, want)
			}
		})
	}
}

// TestHandleUserInbox_RendersPage asserts /u/{slug}/inbox returns 200 with the
// inbox section for a resolvable slug, and 404 for an unknown one.
func TestHandleUserInbox_RendersPage(t *testing.T) {
	f := newFake()
	f.slugResp = map[string]*beadsapi.LookupOperatorBySlugResponse{
		"alice": {OperatorID: 42, Found: true},
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/u/alice/inbox", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `id="inbox"`) {
		t.Errorf("expected inbox section in body")
	}

	req = httptest.NewRequest("GET", "/u/ghost/inbox", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 404 {
		t.Fatalf("expected 404 for unknown slug, got %d", w.Code)
	}
}

// TestHandleUserHomeSectionInbox_LazyFragment asserts the lazy section endpoint
// returns the inbox partial fragment.
func TestHandleUserHomeSectionInbox_LazyFragment(t *testing.T) {
	f := newFake()
	f.slugResp = map[string]*beadsapi.LookupOperatorBySlugResponse{
		"alice": {OperatorID: 42, Found: true},
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/u/alice/section/inbox", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `id="inbox"`) {
		t.Errorf("expected inbox section fragment, got %s", w.Body.String())
	}
}

// TestHandleUserHome_IncludesInboxLazySection asserts the homepage now wires the
// inbox lazy section placeholder.
// TestHandleUserHome_FoldsInboxIntoSpine asserts messages no longer live in a
// separate lazy inbox card on /u/{slug} — they interleave into the single
// merged activity spine (bug ka-0000002041). The standalone /section/inbox
// route is retained (the Users-FLAT Legibility capture fetches it directly),
// so this test checks (a) the homepage no longer ships the lazy inbox shell,
// and (b) the section route still renders the inbox partial.
func TestHandleUserHome_FoldsInboxIntoSpine(t *testing.T) {
	f := newFake()
	f.slugResp = map[string]*beadsapi.LookupOperatorBySlugResponse{
		"alice": {OperatorID: 42, Found: true},
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/u/alice", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// No separate lazy inbox card on the merged homepage.
	if strings.Contains(body, `hx-get="/u/alice/section/inbox"`) {
		t.Errorf("lazy inbox shell should be gone (messages fold into the spine); body=%s", body)
	}
	// The merged activity spine is present.
	if !strings.Contains(body, `id="activity"`) {
		t.Errorf("expected the merged activity spine; body=%s", body)
	}
	// The standalone inbox section route is retained for the Legibility capture.
	wSec := httptest.NewRecorder()
	mux.ServeHTTP(wSec, httptest.NewRequest("GET", "/u/alice/section/inbox", nil))
	if wSec.Code != 200 {
		t.Fatalf("expected 200 from /section/inbox, got %d", wSec.Code)
	}
	if !strings.Contains(wSec.Body.String(), `id="inbox"`) {
		t.Errorf("expected inbox partial from /section/inbox route; body=%s", wSec.Body.String())
	}
}
