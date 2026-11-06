package bridge

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/slack-go/slack"
)

// kdRules returns the rule set for `gd-XXX` only — used by the original
// test cases that were written before JIRA/GitLab linkifying existed.
func kdRules(viewer string) []linkRule {
	return buildLinkRules(LinkifyConfig{BeadsViewerURL: viewer})
}

func TestLinkifyText_NoRulesPassthrough(t *testing.T) {
	got := linkifyText("see gd-abc123 for context", nil)
	if got != "see gd-abc123 for context" {
		t.Fatalf("expected passthrough, got %q", got)
	}
}

func TestLinkifyText_PlainID(t *testing.T) {
	got := linkifyText("see gd-abc123 for context", kdRules("https://viewer"))
	want := "see <https://viewer/gd-abc123|gd-abc123> for context"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestLinkifyText_TrimsTrailingSlash(t *testing.T) {
	got := linkifyText("gd-x1", kdRules("https://viewer/"))
	want := "<https://viewer/gd-x1|gd-x1>"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestLinkifyText_MultipleIDs(t *testing.T) {
	got := linkifyText("gd-a and gd-b but not gd-", kdRules("https://v"))
	want := "<https://v/gd-a|gd-a> and <https://v/gd-b|gd-b> but not gd-"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestLinkifyText_SkipsExistingMrkdwnLinks(t *testing.T) {
	cases := []string{
		"check <https://other/gd-foo|gd-foo>",
		"ping <@U123> about gd-",
		"hi <#C123|general>",
	}
	for _, in := range cases {
		got := linkifyText(in, kdRules("https://v"))
		if got != in {
			t.Errorf("input %q: expected unchanged, got %q", in, got)
		}
	}
}

func TestLinkifyText_LinkifiesAroundExistingSpans(t *testing.T) {
	in := "before gd-x1 then <@U123> then gd-y2 end"
	want := "before <https://v/gd-x1|gd-x1> then <@U123> then <https://v/gd-y2|gd-y2> end"
	got := linkifyText(in, kdRules("https://v"))
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestLinkifyText_NoMatchInsideURLPath(t *testing.T) {
	in := "see <https://viewer/gd-abc?x=1|details>"
	got := linkifyText(in, kdRules("https://viewer"))
	if got != in {
		t.Fatalf("got %q, want %q", got, in)
	}
}

// TestLinkifyText_NoMatchInsideBareURL guards against ka-0000004666: a
// gd-XXX inside a bare http(s) URL (e.g. an agent posting the viewer URL
// directly via `gb slack reply`) must not be wrapped, otherwise the result
// is nested mrkdwn `<URL/<URL/gd-X|gd-X>>` after Slack normalises the
// surrounding bare URL.
func TestLinkifyText_NoMatchInsideBareURL(t *testing.T) {
	cases := []string{
		"see https://viewer/gd-abc for details",
		"https://viewer/gd-abc",
		"link: https://viewer/path/gd-abc?x=1#frag end",
	}
	for _, in := range cases {
		got := linkifyText(in, kdRules("https://viewer"))
		if got != in {
			t.Errorf("input %q: expected unchanged, got %q", in, got)
		}
	}
}

// TestLinkifyText_LinkifiesAroundBareURL confirms that gd-XXX outside a
// bare URL is still linkified — the URL is the only protected region.
func TestLinkifyText_LinkifiesAroundBareURL(t *testing.T) {
	in := "gd-x1 then https://viewer/gd-abc then gd-y2"
	want := "<https://v/gd-x1|gd-x1> then https://viewer/gd-abc then <https://v/gd-y2|gd-y2>"
	got := linkifyText(in, kdRules("https://v"))
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestLinkifyText_JiraIssueKeys_OpenMatcher(t *testing.T) {
	rules := buildLinkRules(LinkifyConfig{JiraBaseURL: "https://x.atlassian.net"})
	got := linkifyText("fix PIH-1234 today", rules)
	want := "fix <https://x.atlassian.net/browse/PIH-1234|PIH-1234> today"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestLinkifyText_JiraIssueKeys_KeyAllowlist(t *testing.T) {
	rules := buildLinkRules(LinkifyConfig{
		JiraBaseURL:     "https://x.atlassian.net",
		JiraProjectKeys: []string{"PIH", "INGEST"},
	})
	in := "PIH-1 and INGEST-99 but not OTHER-7"
	got := linkifyText(in, rules)
	if !strings.Contains(got, "<https://x.atlassian.net/browse/PIH-1|PIH-1>") {
		t.Fatalf("PIH-1 not linkified: %q", got)
	}
	if !strings.Contains(got, "<https://x.atlassian.net/browse/INGEST-99|INGEST-99>") {
		t.Fatalf("INGEST-99 not linkified: %q", got)
	}
	if strings.Contains(got, "OTHER-7|") {
		t.Fatalf("OTHER-7 should not be linkified: %q", got)
	}
}

func TestLinkifyText_JiraDoesNotMatchKbeadsID(t *testing.T) {
	// The lowercase `gd-` form must not collide with the JIRA matcher even
	// when JIRA is enabled — JIRA keys are uppercase.
	rules := buildLinkRules(LinkifyConfig{
		BeadsViewerURL: "https://v",
		JiraBaseURL:    "https://x.atlassian.net",
	})
	got := linkifyText("see gd-abc123", rules)
	want := "see <https://v/gd-abc123|gd-abc123>"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestLinkifyText_GitlabMRRefs(t *testing.T) {
	rules := buildLinkRules(LinkifyConfig{GitlabBaseURL: "https://gitlab.com"})
	got := linkifyText("rebase acme/platform/gardenio!415 please", rules)
	want := "rebase <https://gitlab.com/acme/platform/gardenio/-/merge_requests/415|acme/platform/gardenio!415> please"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestLinkifyText_GitlabMRRefs_RequiresGroup(t *testing.T) {
	// Bare `!123` without a group is too ambiguous to safely linkify.
	rules := buildLinkRules(LinkifyConfig{GitlabBaseURL: "https://gitlab.com"})
	got := linkifyText("see !415 in the project", rules)
	if strings.Contains(got, "merge_requests/415") {
		t.Fatalf("bare !N must not match: %q", got)
	}
}

func TestLinkifyText_AllRulesTogether(t *testing.T) {
	rules := buildLinkRules(LinkifyConfig{
		BeadsViewerURL:  "https://v",
		JiraBaseURL:     "https://x.atlassian.net",
		JiraProjectKeys: []string{"PIH"},
		GitlabBaseURL:   "https://gitlab.com",
	})
	in := "gd-abc, PIH-9, and acme/platform/gardenio!42 — all in one"
	got := linkifyText(in, rules)
	for _, sub := range []string{
		"<https://v/gd-abc|gd-abc>",
		"<https://x.atlassian.net/browse/PIH-9|PIH-9>",
		"<https://gitlab.com/acme/platform/gardenio/-/merge_requests/42|acme/platform/gardenio!42>",
	} {
		if !strings.Contains(got, sub) {
			t.Errorf("missing %q in %q", sub, got)
		}
	}
}

func TestLinkifyBlocksJSON_LinkifiesMrkdwnText(t *testing.T) {
	in := `[{"type":"section","text":{"type":"mrkdwn","text":"see gd-abc"}}]`
	got := linkifyBlocksJSON(in, kdRules("https://v"))
	if !strings.Contains(got, "<https://v/gd-abc|gd-abc>") {
		t.Fatalf("expected linkified bead, got %s", got)
	}
}

func TestLinkifyBlocksJSON_SkipsPlainText(t *testing.T) {
	in := `[{"type":"section","text":{"type":"plain_text","text":"see gd-abc"}}]`
	got := linkifyBlocksJSON(in, kdRules("https://v"))
	if strings.Contains(got, "<https://v/") {
		t.Fatalf("plain_text should not be linkified, got %s", got)
	}
}

func TestLinkifyBlocksJSON_InvalidPassthrough(t *testing.T) {
	in := `not json gd-abc`
	got := linkifyBlocksJSON(in, kdRules("https://v"))
	if got != in {
		t.Fatalf("expected passthrough, got %q", got)
	}
}

func TestLinkifyBlocksJSON_NestedFields(t *testing.T) {
	in := `[{"type":"section","fields":[{"type":"mrkdwn","text":"gd-foo"},{"type":"plain_text","text":"gd-bar"}]}]`
	got := linkifyBlocksJSON(in, kdRules("https://v"))
	if !strings.Contains(got, "<https://v/gd-foo|gd-foo>") {
		t.Fatalf("expected mrkdwn field linkified, got %s", got)
	}
	if strings.Contains(got, "<https://v/gd-bar") {
		t.Fatalf("plain_text should not be linkified, got %s", got)
	}
}

func TestRewriteFormBody_LinkifiesText(t *testing.T) {
	body := url.Values{"text": {"see gd-x1"}, "channel": {"C1"}}.Encode()
	out, ok := rewriteFormBody([]byte(body), kdRules("https://v"))
	if !ok {
		t.Fatal("expected change")
	}
	vals, _ := url.ParseQuery(out)
	if got := vals.Get("text"); got != "see <https://v/gd-x1|gd-x1>" {
		t.Fatalf("text = %q", got)
	}
	if vals.Get("channel") != "C1" {
		t.Fatalf("channel changed unexpectedly")
	}
}

func TestRewriteFormBody_NoChange(t *testing.T) {
	body := url.Values{"text": {"plain"}}.Encode()
	if _, ok := rewriteFormBody([]byte(body), kdRules("https://v")); ok {
		t.Fatal("expected no change")
	}
}

func TestRewriteFormBody_LinkifiesBlocks(t *testing.T) {
	blocks := `[{"type":"section","text":{"type":"mrkdwn","text":"gd-z9"}}]`
	body := url.Values{"blocks": {blocks}}.Encode()
	out, ok := rewriteFormBody([]byte(body), kdRules("https://v"))
	if !ok {
		t.Fatal("expected change")
	}
	vals, _ := url.ParseQuery(out)
	if !strings.Contains(vals.Get("blocks"), "<https://v/gd-z9|gd-z9>") {
		t.Fatalf("blocks = %q", vals.Get("blocks"))
	}
}

func TestSlackLinkifyTransport_RewritesPostMessage(t *testing.T) {
	var captured string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"channel":"C1","ts":"1.2","message":{"text":"x"}}`))
	}))
	defer srv.Close()

	httpClient := newSlackLinkifyClient(http.DefaultClient, kdRules("https://v"))
	api := slack.New("xoxb-test",
		slack.OptionAPIURL(srv.URL+"/"),
		slack.OptionHTTPClient(httpClient),
	)

	_, _, err := api.PostMessage("C1",
		slack.MsgOptionText("hello gd-abc world", false),
		slack.MsgOptionBlocks(slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", "see gd-def too", false, false),
			nil, nil,
		)),
	)
	if err != nil {
		t.Fatalf("PostMessage: %v", err)
	}
	vals, _ := url.ParseQuery(captured)
	if got := vals.Get("text"); got != "hello <https://v/gd-abc|gd-abc> world" {
		t.Fatalf("text = %q", got)
	}
	blocks := vals.Get("blocks")
	if !strings.Contains(blocks, "<https://v/gd-def|gd-def>") {
		t.Fatalf("blocks = %s", blocks)
	}
	var parsed any
	if err := json.Unmarshal([]byte(blocks), &parsed); err != nil {
		t.Fatalf("blocks not valid JSON: %v", err)
	}
}

func TestSlackLinkifyTransport_PassthroughForOtherMethods(t *testing.T) {
	var captured string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"user":{"id":"U1","name":"x"}}`))
	}))
	defer srv.Close()

	httpClient := newSlackLinkifyClient(http.DefaultClient, kdRules("https://v"))
	api := slack.New("xoxb-test",
		slack.OptionAPIURL(srv.URL+"/"),
		slack.OptionHTTPClient(httpClient),
	)
	_, _ = api.GetUserInfo("U1")
	if strings.Contains(captured, "gd-") {
		t.Fatalf("captured = %q", captured)
	}
}

func TestSlackLinkifyTransport_NoRulesNoOp(t *testing.T) {
	var captured string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"channel":"C1","ts":"1.2","message":{"text":"x"}}`))
	}))
	defer srv.Close()

	httpClient := newSlackLinkifyClient(http.DefaultClient, nil)
	api := slack.New("xoxb-test",
		slack.OptionAPIURL(srv.URL+"/"),
		slack.OptionHTTPClient(httpClient),
	)
	_, _, _ = api.PostMessage("C1", slack.MsgOptionText("gd-abc", false))
	vals, _ := url.ParseQuery(captured)
	if vals.Get("text") != "gd-abc" {
		t.Fatalf("text = %q", vals.Get("text"))
	}
}

// TestLinkifyBlocksJSON_SparesNodesLinksWouldPushOverTheLimit is the
// gd-fthcee65T7 guard: linkifying a section the card builder clamped to 2900
// chars must not carry it past Slack's 3000, nor a context element past 2000.
func TestLinkifyBlocksJSON_SparesNodesLinksWouldPushOverTheLimit(t *testing.T) {
	section := strings.Repeat("row gd-abc1234 ok. ", 160)[:2890]
	element := strings.Repeat("see gd-abc1234 ", 130)[:1880]
	in, _ := json.Marshal([]any{
		map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": section}},
		map[string]any{"type": "context", "elements": []any{map[string]any{"type": "mrkdwn", "text": element}}},
		map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": "short gd-abc1234"}},
	})
	got := linkifyBlocksJSON(string(in), kdRules("https://gardenio.clonk.tv/beads"))
	var out []map[string]any
	if err := json.Unmarshal([]byte(got), &out); err != nil {
		t.Fatal(err)
	}
	if s := out[0]["text"].(map[string]any)["text"].(string); s != section {
		t.Errorf("long section rewritten to %d chars", len(s))
	}
	if s := out[1]["elements"].([]any)[0].(map[string]any)["text"].(string); s != element {
		t.Errorf("long context element rewritten to %d chars", len(s))
	}
	if s := out[2]["text"].(map[string]any)["text"].(string); !strings.Contains(s, "<https://gardenio.clonk.tv/beads/gd-abc1234|gd-abc1234>") {
		t.Errorf("short section not linkified: %q", s)
	}
}
