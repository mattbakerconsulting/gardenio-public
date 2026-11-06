package main

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// TestResolveSlackMentions_ResolvesKnownTokens is the unit-level guard for the
// /people recent-requests mention resolver (ka-0000003119, ka-0000005382): a
// `<@U…>` token whose id is in the resolver renders as `@Name`; a valid id NOT
// in the (healthy) resolver renders as the neutral `@external-user` placeholder
// so no raw bracketed Slack id reaches a human; a lowercase placeholder or an
// empty resolver leaves the text exactly as written.
func TestResolveSlackMentions_ResolvesKnownTokens(t *testing.T) {
	r := slackMentionResolver{
		"U0PERSON001": "Hong Jamie Cole",
		"U0OPERATOR1": "gardenio",
	}
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "bare token resolves",
			in:   "<@U0PERSON001> welcome to the team!",
			want: "@Hong Jamie Cole welcome to the team!",
		},
		{
			name: "labeled token resolves via id, label ignored",
			in:   "cc <@U0OPERATOR1|gardenio-bot> please look",
			want: "cc @gardenio please look",
		},
		{
			name: "multiple tokens in one body",
			in:   "<@U0PERSON001> and <@U0OPERATOR1> both",
			want: "@Hong Jamie Cole and @gardenio both",
		},
		{
			name: "unknown external id → neutral placeholder (never leak a raw <@U…>, never invent a name)",
			in:   "<@U000UNKNOWN0> hi",
			want: "@external-user hi",
		},
		{
			name: "external id mid-sentence with other text",
			in:   "<@U0PERSON006> @Lee Morgan I set these",
			want: "@external-user @Lee Morgan I set these",
		},
		{
			name: "lowercase placeholder junk untouched (not a valid id, matcher never fires)",
			in:   "ping <@Uxxxx> and <@Uxxx> stand-ins",
			want: "ping <@Uxxxx> and <@Uxxx> stand-ins",
		},
		{
			name: "no token at all is a no-op",
			in:   "just a plain ask with no mention",
			want: "just a plain ask with no mention",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveSlackMentions(c.in, r); got != c.want {
				t.Errorf("resolveSlackMentions(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestResolveSlackMentions_NilResolverIsNoOp pins that a nil/empty resolver (the
// load-error degrade path) returns the input unchanged rather than panicking or
// stripping tokens.
func TestResolveSlackMentions_NilResolverIsNoOp(t *testing.T) {
	in := "<@U0PERSON001> welcome to the team!"
	if got := resolveSlackMentions(in, nil); got != in {
		t.Errorf("nil resolver must be a no-op: got %q, want %q", got, in)
	}
	if got := resolveSlackMentions(in, slackMentionResolver{}); got != in {
		t.Errorf("empty resolver must be a no-op: got %q, want %q", got, in)
	}
}

// TestBuildSlackMentionResolver_SkipsSentinelRows verifies the id→name map is
// built only from alias rows carrying a genuine display name — the resync
// sentinel rows (empty Display, synthetic slack-U…@noreply email) are skipped
// so a token for one of them stays raw rather than becoming a second opaque id.
func TestBuildSlackMentionResolver_SkipsSentinelRows(t *testing.T) {
	aliases := []beadsapi.SlackOperatorAlias{
		{OperatorID: 1, Subject: "U0PERSON001", Display: "Hong Jamie Cole", Email: "hong@x"},
		{OperatorID: 2, Subject: "U0PERSON002", Display: "", Email: "slack-U0PERSON002@noreply.kbeads.local"},
		{OperatorID: 3, Subject: "  ", Display: "Ghost", Email: "ghost@x"}, // blank subject
	}
	r := buildSlackMentionResolver(aliases)
	if got := r["U0PERSON001"]; got != "Hong Jamie Cole" {
		t.Errorf("named alias must be indexed: got %q", got)
	}
	if _, ok := r["U0PERSON002"]; ok {
		t.Error("sentinel row with empty display must be skipped")
	}
	if len(r) != 1 {
		t.Errorf("expected exactly 1 indexed alias, got %d: %v", len(r), r)
	}
}

// TestBuildSlackMentionResolver_EmptyInput returns nil (a benign no-op
// resolver) when there are no aliases or none carry a name.
func TestBuildSlackMentionResolver_EmptyInput(t *testing.T) {
	if r := buildSlackMentionResolver(nil); r != nil {
		t.Errorf("nil input must yield nil resolver, got %v", r)
	}
	onlySentinels := []beadsapi.SlackOperatorAlias{
		{OperatorID: 1, Subject: "U0PERSON002", Display: ""},
	}
	if r := buildSlackMentionResolver(onlySentinels); r != nil {
		t.Errorf("all-sentinel input must yield nil resolver, got %v", r)
	}
}

// TestHandlePeople_ResolvesSlackMentionTokens is the end-to-end regression for
// ka-0000003119: the exact leak the Users-CAMP audit flagged — Austin Tobitt's
// card rendering the raw snippet `<@U0PERSON001> welcome to the team!` — must
// now render the resolved `@Hong Jamie Cole` and carry NO raw `<@U…>` token
// anywhere in the recent-requests strip.
func TestHandlePeople_ResolvesSlackMentionTokens(t *testing.T) {
	f := peopleFixture()
	// The alias feed resolves the mentioned user even though they have no
	// slug'd directory row — this is the case ListOperators alone misses.
	f.slackAliasesResp = &beadsapi.ListSlackOperatorAliasesResponse{
		Aliases: []beadsapi.SlackOperatorAlias{
			{OperatorID: 500, Subject: "U0PERSON001", Display: "Hong Jamie Cole", Email: "hong@x"},
		},
	}
	ptr := func(n int64) *int64 { return &n }
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-austin-msg": {
			ID:           "gd-austin-msg",
			Type:         "message",
			Labels:       []string{"slack-thread-message"},
			OperatedByID: ptr(42), // Alice's row — carries the ask
			Fields: map[string]string{
				"text":      "<@U0PERSON001> welcome to the team!",
				"thread_id": "gd-thread-austin",
			},
			CreatedAt: time.Now().Add(-1 * time.Minute),
		},
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/people", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// The resolved @name renders...
	if !strings.Contains(body, "@Hong Jamie Cole welcome to the team!") {
		t.Errorf("expected resolved '@Hong Jamie Cole welcome to the team!' in /people, body: %s", body)
	}
	// ...and NO raw Slack mention token survives (HTML-escaped `<@U…>`), which is
	// the exact defect string the audit's repro grep looks for.
	if strings.Contains(body, "&lt;@U0PERSON001&gt;") || strings.Contains(body, "<@U0PERSON001>") {
		t.Errorf("raw Slack mention token leaked into /people recent-requests strip (ka-0000003119), body: %s", body)
	}
}

// TestHandlePeople_ExternalMentionRendersNeutralPlaceholder is the end-to-end
// regression for ka-0000005382: the exact leak the Users-CAMP audit flagged on
// Perry Tate's card — a recent-request snippet whose `<@U0PERSON006>` is a
// genuinely external id (present in neither roster nor alias feed). With a
// HEALTHY resolver loaded, that token must render as the neutral
// `@external-user` placeholder and NO raw `<@U…>` (escaped or not) may survive
// anywhere in the recent-requests strip.
func TestHandlePeople_ExternalMentionRendersNeutralPlaceholder(t *testing.T) {
	f := peopleFixture()
	// A healthy resolver is loaded — but it does NOT contain the external id.
	f.slackAliasesResp = &beadsapi.ListSlackOperatorAliasesResponse{
		Aliases: []beadsapi.SlackOperatorAlias{
			{OperatorID: 500, Subject: "U0PERSON001", Display: "Hong Jamie Cole", Email: "hong@x"},
		},
	}
	ptr := func(n int64) *int64 { return &n }
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-paulo-msg": {
			ID:           "gd-paulo-msg",
			Type:         "message",
			Labels:       []string{"slack-thread-message"},
			OperatedByID: ptr(42),
			Fields: map[string]string{
				"text":      "<@U0PERSON006> @Lee Morgan I set these",
				"thread_id": "gd-thread-paulo",
			},
			CreatedAt: time.Now().Add(-1 * time.Minute),
		},
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/people", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// The neutral placeholder renders...
	if !strings.Contains(body, "@external-user") {
		t.Errorf("expected neutral '@external-user' placeholder in /people, body: %s", body)
	}
	// ...and NO raw external Slack id survives — the exact defect string the
	// audit's repro grep (`&lt;@U[A-Z0-9]+&gt;`) looks for.
	if strings.Contains(body, "&lt;@U0PERSON006&gt;") || strings.Contains(body, "<@U0PERSON006>") {
		t.Errorf("raw external Slack mention token leaked into /people recent-requests strip (ka-0000005382), body: %s", body)
	}
}

// TestHandlePeople_UnknownMentionStaysRawButNeverErrors guards the degrade
// path: a token whose id resolves to nothing (and an alias-load error) must
// leave the page rendering with the raw token rather than 500-ing.
func TestHandlePeople_UnknownMentionStaysRawButNeverErrors(t *testing.T) {
	f := peopleFixture()
	f.slackAliasesErr = errors.New("boom") // resolver load fails → nil resolver
	ptr := func(n int64) *int64 { return &n }
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-msg": {
			ID:           "gd-msg",
			Type:         "message",
			Labels:       []string{"slack-thread-message"},
			OperatedByID: ptr(42),
			Fields:       map[string]string{"text": "<@U0OPERATOR1> ping", "thread_id": "gd-t"},
			CreatedAt:    time.Now(),
		},
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/people", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("alias-load failure must degrade gracefully, got %d body=%s", w.Code, w.Body.String())
	}
	// Raw token survives (escaped) — degraded, but not broken.
	if !strings.Contains(w.Body.String(), "&lt;@U0OPERATOR1&gt;") {
		t.Errorf("expected raw token to survive the degrade path, body: %s", w.Body.String())
	}
}

// TestLookupSlackMentionResolver_SingleRoundTrip pins that resolving mentions
// costs exactly ONE ListSlackOperatorAliases call per /people render — the
// same single-round-trip posture as the page's other page-level lookups (no
// per-row / per-token fan-out).
func TestLookupSlackMentionResolver_SingleRoundTrip(t *testing.T) {
	f := peopleFixture()
	f.slackAliasesResp = &beadsapi.ListSlackOperatorAliasesResponse{
		Aliases: []beadsapi.SlackOperatorAlias{
			{OperatorID: 500, Subject: "U0PERSON001", Display: "Hong Jamie Cole"},
		},
	}
	ptr := func(n int64) *int64 { return &n }
	beads := map[string]*beadsapi.BeadDetail{}
	// Several messages across operators, each with a mention — a per-token or
	// per-row resolver would blow the call count well past 1.
	for i, op := range []int64{42, 99, 42, 99} {
		id := "gd-m" + string(rune('a'+i))
		beads[id] = &beadsapi.BeadDetail{
			ID: id, Type: "message", Labels: []string{"slack-thread-message"},
			OperatedByID: ptr(op),
			Fields:       map[string]string{"text": "<@U0PERSON001> hi", "thread_id": "gd-t" + string(rune('a'+i))},
			CreatedAt:    time.Now().Add(-time.Duration(i) * time.Minute),
		}
	}
	f.beads = beads
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/people", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if f.slackAliasesCalls != 1 {
		t.Errorf("expected exactly 1 ListSlackOperatorAliases call per render, got %d "+
			"(mention resolution must not fan out per-row/per-token)", f.slackAliasesCalls)
	}
}

// compile-time assurance the resolver helper is reachable with a context (the
// handler path uses s.lookupSlackMentionResolver(ctx)).
var _ = context.Background
