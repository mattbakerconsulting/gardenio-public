package bridge

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"

	"gardenio/controller/internal/beadsapi"
)

// TestUnfurlWirePayload drives the real slack-go chat.unfurl call against a
// local server and asserts the exact "unfurls" form value Slack would receive.
//
// Slack answers chat.unfurl with cannot_parse_attachment when an unfurl mixes
// Block Kit blocks with legacy attachment fields (color, fallback, title, ...).
// Release 2026.284.1 sent {"color":..,"fallback":..,"blocks":[..]} and every
// unfurl in #gardenio-agents failed that way, so this pins the payload to
// {"<shared url>": {"blocks": [...]}} with nothing else alongside the blocks.
func TestUnfurlWirePayload(t *testing.T) {
	var (
		mu      sync.Mutex
		gotForm = map[string]string{}
		gotPath string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		mu.Lock()
		gotPath = r.URL.Path
		for _, k := range []string{"channel", "ts", "unfurls"} {
			gotForm[k] = r.PostForm.Get(k)
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	const (
		host    = "gardenio.clonk.tv"
		channel = "C0C3RH33A81"
		ts      = "1791692382.252129"
	)
	sharedURL := "https://" + host + "/beads/gd-6usF0sT6fh"
	b := &Bot{
		api:    slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/")),
		logger: slog.Default(),
		daemon: &fakeBatchDaemon{beads: map[string]*beadsapi.BeadDetail{
			"gd-6usF0sT6fh": {
				ID:          "gd-6usF0sT6fh",
				Title:       "Apply updated Slack app manifest: fix unfurl_domains <to> gardenio.clonk.tv & more",
				Type:        "escalation",
				Status:      "in_progress",
				Assignee:    "laptop-claude",
				Description: "PR #184 fixes gardenio/slack/manifest.json's unfurl_domains\n\nfrom the stale placeholder. " + strings.Repeat("x", 600),
			},
		}},
		beadsViewerURL:     "https://" + host,
		linkUnfurlChannels: newStringSet([]string{channel}),
	}
	b.handleLinkShared(context.Background(), &slackevents.LinkSharedEvent{
		Channel:          channel,
		MessageTimeStamp: ts,
		Links:            []slackevents.SharedLinks{sharedLink(host, sharedURL)},
	})

	mu.Lock()
	defer mu.Unlock()
	if gotPath != "/chat.unfurl" {
		t.Fatalf("request path = %q, want /chat.unfurl", gotPath)
	}
	if gotForm["channel"] != channel || gotForm["ts"] != ts {
		t.Errorf("channel/ts = %q/%q, want %q/%q", gotForm["channel"], gotForm["ts"], channel, ts)
	}

	var unfurls map[string]map[string]json.RawMessage
	if err := json.Unmarshal([]byte(gotForm["unfurls"]), &unfurls); err != nil {
		t.Fatalf("unfurls is not a JSON object of objects: %v\n%s", err, gotForm["unfurls"])
	}
	if len(unfurls) != 1 {
		t.Fatalf("unfurls has %d keys, want 1: %s", len(unfurls), gotForm["unfurls"])
	}
	card, ok := unfurls[sharedURL]
	if !ok {
		t.Fatalf("unfurls key is not the shared url %q: %s", sharedURL, gotForm["unfurls"])
	}
	// Blocks only: no legacy attachment field may ride alongside them.
	for k := range card {
		if k != "blocks" {
			t.Errorf("unfurl carries field %q next to blocks; Slack rejects that with cannot_parse_attachment", k)
		}
	}

	var blocks []struct {
		Type     string `json:"type"`
		BlockID  *string
		Text     *struct{ Type, Text string }
		Elements []struct{ Type, Text string }
	}
	if err := json.Unmarshal(card["blocks"], &blocks); err != nil {
		t.Fatalf("blocks: %v\n%s", err, card["blocks"])
	}
	if len(blocks) == 0 {
		t.Fatal("blocks is empty")
	}
	var raw []map[string]json.RawMessage
	_ = json.Unmarshal(card["blocks"], &raw)
	for i, blk := range blocks {
		if bid, ok := raw[i]["block_id"]; ok && string(bid) == `""` {
			t.Errorf("block %d has an empty block_id", i)
		}
		switch blk.Type {
		case "section":
			if blk.Text == nil || blk.Text.Type != "mrkdwn" || strings.TrimSpace(blk.Text.Text) == "" {
				t.Errorf("section block %d has no mrkdwn text: %s", i, card["blocks"])
			} else if n := len(blk.Text.Text); n > 3000 {
				t.Errorf("section block %d text is %d chars, Slack's limit is 3000", i, n)
			}
		case "context":
			if len(blk.Elements) == 0 || len(blk.Elements) > 10 {
				t.Errorf("context block %d has %d elements, want 1..10", i, len(blk.Elements))
			}
			for j, el := range blk.Elements {
				if el.Type != "mrkdwn" || strings.TrimSpace(el.Text) == "" {
					t.Errorf("context block %d element %d is empty or not mrkdwn: %+v", i, j, el)
				}
			}
		default:
			t.Errorf("block %d has unexpected type %q", i, blk.Type)
		}
	}
	if !strings.Contains(string(card["blocks"]), sharedURL+"|") {
		t.Errorf("card does not link back to the shared url: %s", card["blocks"])
	}
}

// TestUnfurlWirePayload_TwoLinksWithPlan pins the exact form-encoded "unfurls"
// parameter for the message that failed live on 2026.284.4: a bridge notice
// whose two bare ids were linkified into viewer URLs, one of them a plan.
//
// The plan card used to append a slack_file image block (its badge, uploaded
// through files.upload v2 without a channel). chat.unfurl answered the whole
// call with cannot_parse_attachment, so neither link got a preview. The same
// two cards without the image block returned ok against the real API. This
// test asserts the byte-exact wire value, and that no file is uploaded.
func TestUnfurlWirePayload_TwoLinksWithPlan(t *testing.T) {
	var (
		mu       sync.Mutex
		rawBody  string
		gotPaths []string
		srvURL   string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotPaths = append(gotPaths, r.URL.Path)
		if r.URL.Path == "/chat.unfurl" {
			b, _ := io.ReadAll(r.Body)
			rawBody = string(b)
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		// Serve a working files.upload v2 flow, so a card that tried to upload
		// and embed a badge would succeed here and be caught below.
		switch r.URL.Path {
		case "/files.getUploadURLExternal":
			_, _ = w.Write([]byte(`{"ok":true,"upload_url":"` + srvURL + `/upload","file_id":"F0BADGE"}`))
		case "/files.completeUploadExternal":
			_, _ = w.Write([]byte(`{"ok":true,"files":[{"id":"F0BADGE","title":"Plan badge"}]}`))
		default:
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer srv.Close()
	srvURL = srv.URL

	const (
		host    = "gardenio.clonk.tv"
		channel = "C0C3RH33A81"
		ts      = "1791734548.149499"
		planURL = "https://" + host + "/gd-clqFLDCM1H"
		trigURL = "https://" + host + "/gd-EkDlOh3ycR"
	)
	b := &Bot{
		api:    slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/")),
		logger: slog.Default(),
		daemon: &fakeBatchDaemon{beads: map[string]*beadsapi.BeadDetail{
			"gd-clqFLDCM1H": {
				ID: "gd-clqFLDCM1H", Type: "epic", Status: "open",
				Labels: []string{"plan", "project:gardenio"},
				Title:  "Rename 'gardener' role to 'gardener' across gardenio repo",
			},
			"gd-EkDlOh3ycR": {
				ID: "gd-EkDlOh3ycR", Type: "trigger", Status: "open",
				Labels: []string{"plan-gardener", "role:gardener"},
				Title:  "Plan gardener for gd-clqFLDCM1H (Rename 'gardener' role to 'gardener' across gardenio repo)",
			},
		}},
		beadsViewerURL:     "https://" + host,
		linkUnfurlChannels: newStringSet([]string{channel}),
	}
	b.handleLinkShared(context.Background(), &slackevents.LinkSharedEvent{
		Channel:          channel,
		MessageTimeStamp: ts,
		Links:            []slackevents.SharedLinks{sharedLink(host, planURL), sharedLink(host, trigURL)},
	})

	mu.Lock()
	defer mu.Unlock()
	if len(gotPaths) != 1 || gotPaths[0] != "/chat.unfurl" {
		t.Fatalf("Slack calls = %v, want exactly [/chat.unfurl] (no files.* upload)", gotPaths)
	}

	// The exact JSON Slack receives: an object keyed by the URLs exactly as
	// link_shared sent them, each value {"blocks":[...]} with text blocks only.
	const wantUnfurls = `{` +
		`"https://gardenio.clonk.tv/gd-EkDlOh3ycR":{"blocks":[` +
		`{"type":"section","text":{"type":"mrkdwn","text":"\u003chttps://gardenio.clonk.tv/gd-EkDlOh3ycR|Plan gardener for gd-clqFLDCM1H (Rename 'gardener' role to 'gardener' across gardenio repo)\u003e"}},` +
		`{"type":"context","elements":[{"type":"mrkdwn","text":"🪹 ` + "`trigger`" + `  ·  🫘 open"}]}]},` +
		`"https://gardenio.clonk.tv/gd-clqFLDCM1H":{"blocks":[` +
		`{"type":"section","text":{"type":"mrkdwn","text":"\u003chttps://gardenio.clonk.tv/gd-clqFLDCM1H|Rename 'gardener' role to 'gardener' across gardenio repo\u003e"}},` +
		`{"type":"context","elements":[{"type":"mrkdwn","text":"🗺️ ` + "`plan`" + `  ·  🫘 open"}]}]}` +
		`}`

	// Assert the form-encoded parameter byte for byte, as it goes on the wire:
	// unfurls is ONE form field holding the JSON object, encoded once.
	var gotEncoded string
	for _, kv := range strings.Split(rawBody, "&") {
		if v, ok := strings.CutPrefix(kv, "unfurls="); ok {
			gotEncoded = v
		}
	}
	if want := url.QueryEscape(wantUnfurls); gotEncoded != want {
		got, _ := url.QueryUnescape(gotEncoded)
		t.Fatalf("form-encoded unfurls mismatch\n got: %s\nwant: %s\n(decoded got: %s)", gotEncoded, want, got)
	}
	if strings.Contains(rawBody, "slack_file") || strings.Contains(rawBody, "%22image%22") {
		t.Errorf("unfurl carries an image block; chat.unfurl rejects it with cannot_parse_attachment: %s", rawBody)
	}
}
