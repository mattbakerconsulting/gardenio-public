package bridge

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"

	"gardenio/controller/internal/beadsapi"
)

func TestExtractBeadID(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{"beads path", "https://prod.gardenio.acme.example/beads/gd-HE8w8hlfyz", "gd-HE8w8hlfyz"},
		{"plans path", "https://prod.gardenio.acme.example/plans/gd-abc123", "gd-abc123"},
		{"agents path", "https://prod.gardenio.acme.example/agents/gd-DUTyuoQu0P", "gd-DUTyuoQu0P"},
		{"bundle path", "https://prod.gardenio.acme.example/beads/bundle/gd-6oGmI8DXn2", "gd-6oGmI8DXn2"},
		{"trailing subpath", "https://prod.gardenio.acme.example/beads/gd-abc123/comments", "gd-abc123"},
		{"trailing slash", "https://prod.gardenio.acme.example/beads/gd-abc123/", "gd-abc123"},
		{"query and fragment", "https://prod.gardenio.acme.example/beads/gd-abc123?x=1#top", "gd-abc123"},
		{"no bead id", "https://prod.gardenio.acme.example/plans", ""},
		{"root", "https://prod.gardenio.acme.example/", ""},
		{"non-kd segment", "https://prod.gardenio.acme.example/beads/not-a-bead", ""},
		{"kd prefix but bad chars", "https://prod.gardenio.acme.example/beads/gd-abc_123", ""},
		{"unparseable", "://bad url", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractBeadID(tt.url); got != tt.want {
				t.Errorf("extractBeadID(%q) = %q, want %q", tt.url, got, tt.want)
			}
		})
	}
}

func TestSelectUnfurlLinks(t *testing.T) {
	const host = "prod.gardenio.acme.example"
	l := func(domain, url string) slackevents.SharedLinks {
		return slackevents.SharedLinks{Domain: domain, URL: url}
	}

	t.Run("filters off-domain links", func(t *testing.T) {
		urlToID, ids := selectUnfurlLinks([]slackevents.SharedLinks{
			l(host, "https://prod.gardenio.acme.example/beads/gd-aaa"),
			l("evil.com", "https://evil.com/beads/gd-bbb"),
			l("github.com", "https://github.com/x/y"),
		}, host)
		if len(urlToID) != 1 || urlToID["https://prod.gardenio.acme.example/beads/gd-aaa"] != "gd-aaa" {
			t.Fatalf("urlToID = %v, want single gd-aaa", urlToID)
		}
		if len(ids) != 1 || ids[0] != "gd-aaa" {
			t.Fatalf("ids = %v, want [gd-aaa]", ids)
		}
	})

	t.Run("dedupes repeated urls and ids", func(t *testing.T) {
		urlToID, ids := selectUnfurlLinks([]slackevents.SharedLinks{
			l(host, "https://prod.gardenio.acme.example/beads/gd-aaa"),
			l(host, "https://prod.gardenio.acme.example/beads/gd-aaa"),
			l(host, "https://prod.gardenio.acme.example/plans/gd-aaa"), // same id, different url
		}, host)
		if len(urlToID) != 2 {
			t.Fatalf("urlToID = %v, want 2 distinct urls", urlToID)
		}
		if len(ids) != 1 || ids[0] != "gd-aaa" {
			t.Fatalf("ids = %v, want single fetch of gd-aaa", ids)
		}
	})

	t.Run("skips links without a bead id", func(t *testing.T) {
		urlToID, ids := selectUnfurlLinks([]slackevents.SharedLinks{
			l(host, "https://prod.gardenio.acme.example/plans"),
			l(host, "https://prod.gardenio.acme.example/beads/gd-aaa"),
		}, host)
		if len(urlToID) != 1 || len(ids) != 1 {
			t.Fatalf("urlToID=%v ids=%v, want just gd-aaa", urlToID, ids)
		}
	})

	t.Run("case-insensitive domain match", func(t *testing.T) {
		urlToID, _ := selectUnfurlLinks([]slackevents.SharedLinks{
			l("PROD.Gardenio.acme.example", "https://prod.gardenio.acme.example/beads/gd-aaa"),
		}, host)
		if len(urlToID) != 1 {
			t.Fatalf("urlToID = %v, want case-insensitive match", urlToID)
		}
	})

	t.Run("caps at maxUnfurlLinks", func(t *testing.T) {
		var links []slackevents.SharedLinks
		for i := range maxUnfurlLinks + 5 {
			links = append(links, l(host, "https://prod.gardenio.acme.example/beads/gd-id"+string(rune('a'+i))))
		}
		urlToID, _ := selectUnfurlLinks(links, host)
		if len(urlToID) != maxUnfurlLinks {
			t.Fatalf("len(urlToID) = %d, want cap %d", len(urlToID), maxUnfurlLinks)
		}
	})
}

func TestViewerHost(t *testing.T) {
	tests := []struct {
		name      string
		viewerURL string
		want      string
	}{
		{"prod", "https://prod.gardenio.acme.example/beads", "prod.gardenio.acme.example"},
		{"trailing slash trimmed at ctor", "https://prod.gardenio.acme.example", "prod.gardenio.acme.example"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := &Bot{beadsViewerURL: tt.viewerURL}
			if got := b.viewerHost(); got != tt.want {
				t.Errorf("viewerHost() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestBuildUnfurlAttachment_AssigneeBusiness pins the Business assignee mark:
// the [ASSIGNEE] tag replaces the Garden glyph and the legacy shortcode.
func TestBuildUnfurlAttachment_AssigneeBusiness(t *testing.T) {
	b := &Bot{beadsViewerURL: "https://prod.gardenio.acme.example/beads"}
	bead := &beadsapi.BeadDetail{ID: "gd-abc", Title: "T", Type: "task", Status: "open", Assignee: "matt"}
	text := renderBlocksText(b.buildUnfurlAttachment(Business, bead, "https://prod.gardenio.acme.example/beads/gd-abc"))
	if !strings.Contains(text, "[ASSIGNEE] matt") {
		t.Errorf("Business card missing assignee tag: %s", text)
	}
	if strings.Contains(text, Garden.Semantic(SemAssignee)) || strings.Contains(text, "bust_in_silhouette") {
		t.Errorf("Business card leaked Garden/legacy assignee glyph: %s", text)
	}
}

func TestBuildUnfurlAttachment(t *testing.T) {
	b := &Bot{beadsViewerURL: "https://prod.gardenio.acme.example/beads"}

	t.Run("epic renders type badge, title link, status, assignee, snippet", func(t *testing.T) {
		bead := &beadsapi.BeadDetail{
			ID:          "gd-HE8w8hlfyz",
			Title:       "Slack link unfurling for gardenio",
			Type:        "epic",
			Status:      "open",
			Assignee:    "matt",
			Description: "When a human pastes a gardenio link,\n\nthe app posts a rich card.",
		}
		att := b.buildUnfurlAttachment(Garden, bead, "https://prod.gardenio.acme.example/beads/gd-HE8w8hlfyz")

		text := renderBlocksText(att)
		for _, want := range []string{
			"🌻 `epic`", // Garden epic badge glued to its type word
			"Slack link unfurling for gardenio",
			"https://prod.gardenio.acme.example/beads/gd-HE8w8hlfyz|", // title links to the shared url
			"open",
			"🐝 matt",                     // Garden assignee glyph glued to the name
			"the app posts a rich card.", // snippet, whitespace-collapsed
		} {
			if !strings.Contains(text, want) {
				t.Errorf("card text missing %q\n---\n%s", want, text)
			}
		}
		// Snippet must be single-line (newlines collapsed).
		if strings.Contains(text, "gardenio link,\n\n") {
			t.Errorf("snippet not whitespace-collapsed: %q", text)
		}
	})

	t.Run("no assignee, empty description", func(t *testing.T) {
		bead := &beadsapi.BeadDetail{ID: "gd-abc", Title: "Bare", Type: "task", Status: "in_progress"}
		att := b.buildUnfurlAttachment(Garden, bead, "https://prod.gardenio.acme.example/beads/gd-abc")
		text := renderBlocksText(att)
		if strings.Contains(text, "bust_in_silhouette") || strings.Contains(text, Garden.Semantic(SemAssignee)) {
			t.Errorf("expected no assignee chip, got: %s", text)
		}
		if !strings.Contains(text, "in progress") {
			t.Errorf("status not humanized: %s", text)
		}
	})

	t.Run("title falls back to id", func(t *testing.T) {
		bead := &beadsapi.BeadDetail{ID: "gd-xyz", Type: "bug", Status: "closed"}
		att := b.buildUnfurlAttachment(Garden, bead, "https://prod.gardenio.acme.example/beads/gd-xyz")
		if !strings.Contains(renderBlocksText(att), "gd-xyz") {
			t.Errorf("expected id as title fallback: %s", renderBlocksText(att))
		}
	})
}

// TestBuildUnfurlAttachmentTypes locks in the per-type badge/emoji and per-status
// rendering for the other entity surfaces a shared link can carry (task, bug,
// agent). TestBuildUnfurlAttachment already covers epic in depth; this table just
// asserts the type-driven bits differ correctly across types + statuses.
func TestBuildUnfurlAttachmentTypes(t *testing.T) {
	b := &Bot{beadsViewerURL: "https://prod.gardenio.acme.example/beads"}

	tests := []struct {
		name        string
		bead        *beadsapi.BeadDetail
		wantBadge   string // type badge (glyph glued to the type word) in the meta line
		wantStatEmo string // status indicator glyph
		wantStatus  string // humanized status text
	}{
		{
			name:        "task open",
			bead:        &beadsapi.BeadDetail{ID: "gd-t1", Title: "Wire the thing", Type: "task", Status: "open"},
			wantBadge:   "🌱 `task`",
			wantStatEmo: Garden.Glyph(LifecycleSpawning),
			wantStatus:  "open",
		},
		{
			name:        "bug blocked",
			bead:        &beadsapi.BeadDetail{ID: "gd-b1", Title: "Panic on nil bead", Type: "bug", Status: "blocked"},
			wantBadge:   "🐛 `bug`",
			wantStatEmo: Garden.Glyph(LifecycleBlocked),
			wantStatus:  "blocked",
		},
		{
			name:        "agent in_progress",
			bead:        &beadsapi.BeadDetail{ID: "gd-a1", Title: "crew-1", Type: "agent", Status: "in_progress"},
			wantBadge:   "🤖 `agent`",
			wantStatEmo: Garden.Glyph(LifecycleWorking),
			wantStatus:  "in progress", // humanized: underscore -> space
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			linkURL := "https://prod.gardenio.acme.example/beads/" + tt.bead.ID
			att := b.buildUnfurlAttachment(Garden, tt.bead, linkURL)
			text := renderBlocksText(att)
			for _, want := range []string{
				tt.bead.Title,
				tt.wantBadge,
				tt.wantStatEmo + " " + tt.wantStatus, // glyph glued to its label
			} {
				if !strings.Contains(text, want) {
					t.Errorf("card text missing %q\n---\n%s", want, text)
				}
			}
		})
	}
}

// fakeBatchDaemon is a BeadClient stub that also implements batchBeadFetcher.
type fakeBatchDaemon struct {
	BeadClient
	beads      map[string]*beadsapi.BeadDetail
	batchCalls int
	lastIDs    []string
	projects   map[string]beadsapi.ProjectInfo // for themeFor; nil -> Garden
}

func (f *fakeBatchDaemon) ListProjectBeads(context.Context) (map[string]beadsapi.ProjectInfo, error) {
	return f.projects, nil
}

func (f *fakeBatchDaemon) BatchGetBeads(_ context.Context, ids []string) (map[string]*beadsapi.BeadDetail, error) {
	f.batchCalls++
	f.lastIDs = ids
	out := make(map[string]*beadsapi.BeadDetail)
	for _, id := range ids {
		if b, ok := f.beads[id]; ok {
			out[id] = b
		}
	}
	return out, nil
}

func TestBatchGetBeadsPrefersBatchFetcher(t *testing.T) {
	daemon := &fakeBatchDaemon{beads: map[string]*beadsapi.BeadDetail{
		"gd-a": {ID: "gd-a", Title: "A", Type: "task", Status: "open"},
	}}
	b := &Bot{daemon: daemon, logger: slog.Default()}
	got := b.batchGetBeads(context.Background(), []string{"gd-a", "gd-missing"})
	if daemon.batchCalls != 1 {
		t.Fatalf("batchCalls = %d, want 1", daemon.batchCalls)
	}
	if len(got) != 1 || got["gd-a"] == nil {
		t.Fatalf("got = %v, want just gd-a (missing id absent)", got)
	}
}

// fakePerIDDaemon implements only BeadClient (no BatchGetBeads).
type fakePerIDDaemon struct {
	BeadClient
	beads    map[string]*beadsapi.BeadDetail
	getCalls int
}

func (f *fakePerIDDaemon) GetBead(_ context.Context, id string) (*beadsapi.BeadDetail, error) {
	f.getCalls++
	if b, ok := f.beads[id]; ok {
		return b, nil
	}
	return nil, nil
}

func TestBatchGetBeadsFallsBackToPerID(t *testing.T) {
	daemon := &fakePerIDDaemon{beads: map[string]*beadsapi.BeadDetail{
		"gd-a": {ID: "gd-a"},
		"gd-b": {ID: "gd-b"},
	}}
	b := &Bot{daemon: daemon, logger: slog.Default()}
	got := b.batchGetBeads(context.Background(), []string{"gd-a", "gd-b", "gd-missing"})
	if daemon.getCalls != 3 {
		t.Fatalf("getCalls = %d, want 3", daemon.getCalls)
	}
	if len(got) != 2 {
		t.Fatalf("got = %v, want 2 present beads", got)
	}
}

// sharedLink is a tiny helper for building link_shared event links in tests.
func sharedLink(domain, url string) slackevents.SharedLinks {
	return slackevents.SharedLinks{Domain: domain, URL: url}
}

// TestBuildUnfurls exercises the handler-orchestration seam extracted from
// handleLinkShared: given a link_shared event, it selects viewer-domain links,
// hydrates their bead ids via the stub daemon, and returns the url->attachment
// map that chat.unfurl would post. It asserts the map shape and that ids the
// daemon doesn't resolve (unknown / deleted) hit the `bead == nil { continue }`
// skip and produce no entry — while a resolvable bead (even a closed one, which
// carries no status gate) does render a card.
func TestBuildUnfurls(t *testing.T) {
	const host = "prod.gardenio.acme.example"
	newBot := func(beads map[string]*beadsapi.BeadDetail) *Bot {
		return &Bot{
			daemon:         &fakeBatchDaemon{beads: beads},
			beadsViewerURL: "https://prod.gardenio.acme.example/beads",
			logger:         slog.Default(),
		}
	}
	ev := func(links ...slackevents.SharedLinks) *slackevents.LinkSharedEvent {
		return &slackevents.LinkSharedEvent{
			Channel:          "C123",
			MessageTimeStamp: "1784055212.286079",
			Links:            links,
		}
	}

	t.Run("one card per resolvable shared url; unknown id skipped", func(t *testing.T) {
		b := newBot(map[string]*beadsapi.BeadDetail{
			"gd-open":   {ID: "gd-open", Title: "Open task", Type: "task", Status: "open"},
			"gd-closed": {ID: "gd-closed", Title: "Closed bug", Type: "bug", Status: "closed"},
			// gd-gone is intentionally absent -> daemon returns no entry -> skipped.
		})
		openURL := "https://prod.gardenio.acme.example/beads/gd-open"
		closedURL := "https://prod.gardenio.acme.example/plans/gd-closed"
		goneURL := "https://prod.gardenio.acme.example/beads/gd-gone"
		unfurls := b.buildUnfurls(context.Background(), ev(
			sharedLink(host, openURL),
			sharedLink(host, closedURL),
			sharedLink(host, goneURL),
		))

		if len(unfurls) != 2 {
			t.Fatalf("len(unfurls) = %d, want 2 (gd-gone skipped)", len(unfurls))
		}
		if _, ok := unfurls[goneURL]; ok {
			t.Errorf("unknown id produced a card; want no entry for %q", goneURL)
		}
		// Map is keyed by the exact shared URL, and a closed bead still renders.
		if _, ok := unfurls[openURL]; !ok {
			t.Errorf("missing card for %q", openURL)
		}
		if _, ok := unfurls[closedURL]; !ok {
			t.Errorf("missing card for closed bead %q", closedURL)
		}
	})

	t.Run("distinct urls for the same id each get their own card", func(t *testing.T) {
		b := newBot(map[string]*beadsapi.BeadDetail{
			"gd-x": {ID: "gd-x", Title: "Shared", Type: "epic", Status: "open"},
		})
		beadsURL := "https://prod.gardenio.acme.example/beads/gd-x"
		plansURL := "https://prod.gardenio.acme.example/plans/gd-x"
		unfurls := b.buildUnfurls(context.Background(), ev(
			sharedLink(host, beadsURL),
			sharedLink(host, plansURL),
		))
		if len(unfurls) != 2 {
			t.Fatalf("len(unfurls) = %d, want 2 distinct urls", len(unfurls))
		}
		// Each card links back to the exact URL the human shared.
		if !strings.Contains(renderBlocksText(unfurls[plansURL]), plansURL+"|") {
			t.Errorf("card for %q does not link back to its shared url", plansURL)
		}
	})

	t.Run("no resolvable links -> empty map", func(t *testing.T) {
		b := newBot(map[string]*beadsapi.BeadDetail{"gd-x": {ID: "gd-x", Type: "task", Status: "open"}})
		unfurls := b.buildUnfurls(context.Background(), ev(
			sharedLink("evil.com", "https://evil.com/beads/gd-x"),        // off-domain
			sharedLink(host, "https://prod.gardenio.acme.example/plans"), // no bead id
		))
		if len(unfurls) != 0 {
			t.Fatalf("len(unfurls) = %d, want 0", len(unfurls))
		}
	})

	t.Run("viewer url unconfigured -> nil map", func(t *testing.T) {
		b := &Bot{daemon: &fakeBatchDaemon{}, logger: slog.Default()} // no beadsViewerURL
		if got := b.buildUnfurls(context.Background(), ev(
			sharedLink(host, "https://prod.gardenio.acme.example/beads/gd-x"),
		)); got != nil {
			t.Fatalf("buildUnfurls = %v, want nil when viewer host unset", got)
		}
	})
}

// TestHandleLinkShared covers the outer orchestration guards around buildUnfurls:
// nil receiver/event safety, the compose-box-preview skip (a UUID message_ts),
// and that a nil api makes the handler a no-op rather than panicking. The
// url->attachment payload itself is asserted in TestBuildUnfurls.
func TestHandleLinkShared(t *testing.T) {
	link := sharedLink("prod.gardenio.acme.example", "https://prod.gardenio.acme.example/beads/gd-x")

	t.Run("nil receiver and nil event are safe", func(t *testing.T) {
		var nilBot *Bot
		nilBot.handleLinkShared(context.Background(), &slackevents.LinkSharedEvent{})
		b := &Bot{logger: slog.Default()}
		b.handleLinkShared(context.Background(), nil)
	})

	t.Run("compose-box preview (uuid message_ts) is skipped without a daemon fetch", func(t *testing.T) {
		daemon := &fakeBatchDaemon{beads: map[string]*beadsapi.BeadDetail{
			"gd-x": {ID: "gd-x", Type: "task", Status: "open"},
		}}
		b := &Bot{
			daemon:             daemon,
			beadsViewerURL:     "https://prod.gardenio.acme.example/beads",
			linkUnfurlChannels: map[string]bool{"C123": true},
			logger:             slog.Default(),
		}
		b.handleLinkShared(context.Background(), &slackevents.LinkSharedEvent{
			Channel:          "C123",
			MessageTimeStamp: "b6d8f1a0-0000-0000-0000-000000000000", // UUID, not a real ts
			Links:            []slackevents.SharedLinks{link},
		})
		if daemon.batchCalls != 0 {
			t.Errorf("batchCalls = %d, want 0 (compose-box preview must not fetch)", daemon.batchCalls)
		}
	})

	t.Run("nil api makes a sent-message link a no-op (no panic)", func(t *testing.T) {
		b := &Bot{
			daemon:             &fakeBatchDaemon{beads: map[string]*beadsapi.BeadDetail{"gd-x": {ID: "gd-x", Type: "task", Status: "open"}}},
			beadsViewerURL:     "https://prod.gardenio.acme.example/beads",
			linkUnfurlChannels: map[string]bool{"C123": true},
			logger:             slog.Default(),
		}
		b.handleLinkShared(context.Background(), &slackevents.LinkSharedEvent{
			Channel:          "C123",
			MessageTimeStamp: "1784055212.286079",
			Links:            []slackevents.SharedLinks{link},
		})
	})

	t.Run("channel not in allowlist is skipped without a daemon fetch", func(t *testing.T) {
		daemon := &fakeBatchDaemon{beads: map[string]*beadsapi.BeadDetail{
			"gd-x": {ID: "gd-x", Type: "task", Status: "open"},
		}}
		b := &Bot{
			daemon:             daemon,
			beadsViewerURL:     "https://prod.gardenio.acme.example/beads",
			linkUnfurlChannels: map[string]bool{"C-allowed": true},
			logger:             slog.Default(),
		}
		b.handleLinkShared(context.Background(), &slackevents.LinkSharedEvent{
			Channel:          "C-other", // not allowlisted
			MessageTimeStamp: "1784055212.286079",
			Links:            []slackevents.SharedLinks{link},
		})
		if daemon.batchCalls != 0 {
			t.Errorf("batchCalls = %d, want 0 (unlisted channel must not fetch)", daemon.batchCalls)
		}
	})

	t.Run("empty allowlist disables unfurling everywhere (no fetch)", func(t *testing.T) {
		daemon := &fakeBatchDaemon{beads: map[string]*beadsapi.BeadDetail{
			"gd-x": {ID: "gd-x", Type: "task", Status: "open"},
		}}
		b := &Bot{
			daemon:         daemon,
			beadsViewerURL: "https://prod.gardenio.acme.example/beads",
			// linkUnfurlChannels nil -> off everywhere (the default).
			logger: slog.Default(),
		}
		b.handleLinkShared(context.Background(), &slackevents.LinkSharedEvent{
			Channel:          "C123",
			MessageTimeStamp: "1784055212.286079",
			Links:            []slackevents.SharedLinks{link},
		})
		if daemon.batchCalls != 0 {
			t.Errorf("batchCalls = %d, want 0 (empty allowlist must not fetch)", daemon.batchCalls)
		}
	})
}

// TestLinkUnfurlEnabledForChannel covers the per-channel gate directly: the
// default (nil/empty allowlist) is OFF everywhere, and only allowlisted
// channels enable unfurling.
func TestLinkUnfurlEnabledForChannel(t *testing.T) {
	var nilBot *Bot
	if nilBot.linkUnfurlEnabledForChannel("C123") {
		t.Error("nil bot must report unfurl disabled")
	}

	off := &Bot{} // nil allowlist
	if off.linkUnfurlEnabledForChannel("C123") {
		t.Error("empty allowlist must report unfurl disabled everywhere")
	}

	on := &Bot{linkUnfurlChannels: newStringSet([]string{"C123", " C456 ", ""})}
	if !on.linkUnfurlEnabledForChannel("C123") {
		t.Error("allowlisted channel C123 must report unfurl enabled")
	}
	if !on.linkUnfurlEnabledForChannel("C456") {
		t.Error("allowlisted channel C456 (trimmed) must report unfurl enabled")
	}
	if on.linkUnfurlEnabledForChannel("C999") {
		t.Error("unlisted channel C999 must report unfurl disabled")
	}
	if on.linkUnfurlEnabledForChannel("") {
		t.Error("blank entries must not enable an empty channel id")
	}
}

// renderBlocksText flattens an attachment's section/context block text so tests
// can assert on rendered content without reaching into Block Kit internals.
func renderBlocksText(att slack.Attachment) string {
	var sb strings.Builder
	for _, blk := range att.Blocks.BlockSet {
		switch b := blk.(type) {
		case *slack.SectionBlock:
			if b.Text != nil {
				sb.WriteString(b.Text.Text)
				sb.WriteString("\n")
			}
		case *slack.ContextBlock:
			for _, el := range b.ContextElements.Elements {
				if txt, ok := el.(*slack.TextBlockObject); ok {
					sb.WriteString(txt.Text)
					sb.WriteString("\n")
				}
			}
		}
	}
	return sb.String()
}
