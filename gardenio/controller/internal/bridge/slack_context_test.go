package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

func TestSlackContextMrkdwn(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"prose passes through", "*Approve* grants <https://x|y> & more\n", "*Approve* grants <https://x|y> & more\n"},
		{"span escaped", "land `a<b>&c`.", "land `a&lt;b&gt;&amp;c`."},
		{"long delimiter", "``a`b``", "`aˋb`"},
		{"padded span", "`` `x` ``", "`ˋxˋ`"},
		{"span across lines", "`a\nb`", "`a b`"},
		{"unmatched run is literal", "``a`", "``a`"},
		{"escaped backtick is literal", "\\`a`", "\\`a`"},
		{"one-line triple is a span", "```x```", "`x`"},
		{"long fence", "````go\nx```<y>\n````\nafter", "```\nxˋˋˋ&lt;y&gt;\n```\nafter"},
		{"tilde fence", "~~~\n`<!channel>`\n~~~", "```\nˋ&lt;!channel&gt;ˋ\n```"},
		{"short closer does not close", "````\n```\n<!here>\n````", "```\nˋˋˋ\n&lt;!here&gt;\n```"},
		{"unclosed fence runs to the end", "a\n```\n<!here>", "a\n```\n&lt;!here&gt;\n```"},
		{"indented fence", "- item\n  ```\n  <x>\n  ```", "- item\n```\n  &lt;x&gt;\n```"},
	} {
		if got := slackContextMrkdwn(tc.in); got != tc.want {
			t.Errorf("%s: slackContextMrkdwn(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

func TestFitSlackContext(t *testing.T) {
	if got := fitSlackContext("short", 10, "…"); got != "short" {
		t.Errorf("context that fits changed: %q", got)
	}
	// A cut inside a block closes it again.
	in := "intro\n```\n" + strings.Repeat("&amp;", 200) + "\n```\nafter"
	got := fitSlackContext(in, 120, " …")
	if n := utf8.RuneCountInString(got); n > 120 {
		t.Errorf("fitted context is %d runes, limit 120", n)
	}
	if !strings.HasSuffix(got, "\n``` …") {
		t.Errorf("cut inside a block left it open: %q", got)
	}
	// A cut prefers the last line break, so it splits no span or escape.
	in = "first line\n" + strings.Repeat("x", 50) + "\nlast `<span>` line that will not fit"
	got = fitSlackContext(in, 80, "…")
	if got != "first line\n"+strings.Repeat("x", 50)+"…" {
		t.Errorf("cut not on the line break: %q", got)
	}
}

// slackLiveMrkdwn is what Slack renders live of mrkdwn text: everything
// outside its ``` blocks and its one-backtick spans. An unclosed ``` is text.
func slackLiveMrkdwn(s string) string {
	parts := strings.Split(s, "```")
	var live strings.Builder
	for i, p := range parts {
		if i%2 == 1 && i < len(parts)-1 {
			continue
		}
		if i%2 == 1 {
			live.WriteString("```")
		}
		for _, line := range strings.Split(p, "\n") {
			spans := strings.Split(line, "`")
			for j, sp := range spans {
				if j%2 == 0 || j == len(spans)-1 {
					live.WriteString(sp)
				}
			}
			live.WriteByte('\n')
		}
	}
	return live.String()
}

// gateCodeBlock and gateInlineCode fence an applicant's value as the Lands
// Gate does (cmd/beads-viewer lands_apply.go mdCodeBlock and mdInlineCode): a
// CommonMark fence one backtick longer than the longest run in the value.
func gateCodeBlock(s string) string {
	fence := strings.Repeat("`", max(3, longestRun(s)+1))
	return fence + "\n" + s + "\n" + fence
}

func gateInlineCode(s string) string {
	fence := strings.Repeat("`", longestRun(s)+1)
	if strings.HasPrefix(s, "`") || strings.HasSuffix(s, "`") {
		s = " " + s + " "
	}
	return fence + s + fence
}

func longestRun(s string) int {
	best, n := 0, 0
	for _, r := range s {
		if r == '`' {
			n++
			best = max(best, n)
		} else {
			n = 0
		}
	}
	return best
}

// TestNotifyDecision_ApplicantTextStaysCode is gd-TyXgeI2OGT: a Lands
// application's decision carries the applicant's email and purpose fenced as
// Markdown code, and the card posted them as mrkdwn unescaped. A purpose with
// ``` closed Slack's block and rendered live; <!channel> and
// <https://evil|Approve this application> are Slack control sequences even in
// code. What Slack receives must keep every applicant word in code, escaped.
func TestNotifyDecision_ApplicantTextStaysCode(t *testing.T) {
	const marker = "*PWNED*"
	attacks := map[string]struct{ email, purpose string }{
		"fence breakout":         {"a@example.com", "Hello\n```\n" + marker + " <!channel> <https://evil.example|Approve this application>\n```\nbye"},
		"bare control sequences": {"a@example.com", marker + " <!here> <@U0EVIL> & <https://evil.example|click>"},
		"inline backticks":       {"`<!channel>` " + marker, "`" + marker + "` ``" + marker + "``"},
		"cut inside the block":   {"a@example.com", marker + "\n<!channel>\n" + strings.Repeat("&", 900)},
	}
	for name, a := range attacks {
		t.Run(name, func(t *testing.T) {
			ctxText := "This application asks for a land called `bravo`.\n\n" +
				"- Applicant: " + gateInlineCode(a.email) + " (GitHub `octocat`)\n" +
				"- Repository: https://github.com/octocat/hello (public)\n" +
				"\nWhat they want to do:\n\n" + gateCodeBlock(a.purpose) + "\n" +
				"\nApprove grants $5 credits on Sonnet 5.5. Deny's reply is the reason the applicant reads."

			var mu sync.Mutex
			var posted string
			slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/chat.postMessage" {
					_ = r.ParseForm()
					mu.Lock()
					posted = r.FormValue("blocks")
					mu.Unlock()
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "C123", "ts": "1111.2222"})
			}))
			defer slackSrv.Close()
			bot := newTestBot(newMockDaemon(), slackSrv)
			bot.channel = "C123"
			if err := bot.NotifyDecision(context.Background(), BeadEvent{
				ID: "gd-app-dec", Type: "decision", Title: "Lands application", Priority: 2,
				Fields: map[string]string{"prompt": "Approve this land?", "options": `["approve","deny"]`, "context": ctxText},
			}); err != nil {
				t.Fatalf("NotifyDecision: %v", err)
			}

			mu.Lock()
			defer mu.Unlock()
			var blocks []struct {
				Text struct{ Text string } `json:"text"`
			}
			if err := json.Unmarshal([]byte(posted), &blocks); err != nil {
				t.Fatalf("blocks: %v\n%s", err, posted)
			}
			section := ""
			for _, b := range blocks {
				if strings.Contains(b.Text.Text, "What they want to do") {
					section = b.Text.Text
				}
			}
			if section == "" {
				t.Fatalf("no context section posted: %s", posted)
			}
			for _, seq := range []string{"<!channel>", "<!here>", "<@U0EVIL>", "<https://evil.example"} {
				if strings.Contains(section, seq) {
					t.Errorf("context carries the control sequence %s:\n%s", seq, section)
				}
			}
			if live := slackLiveMrkdwn(section); strings.Contains(live, "PWNED") {
				t.Errorf("applicant text renders as live mrkdwn:\n%s", live)
			}
			if !strings.Contains(section, "PWNED") {
				t.Errorf("applicant text was dropped, not quoted:\n%s", section)
			}
		})
	}
}

// TestNotifyNewEscalation_ContextCodeEscaped: an escalation's context gets the
// same treatment as a decision's.
func TestNotifyNewEscalation_ContextCodeEscaped(t *testing.T) {
	var mu sync.Mutex
	var posted string
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postMessage" {
			_ = r.ParseForm()
			mu.Lock()
			posted = r.FormValue("blocks")
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "C123", "ts": "1111.2222"})
	}))
	defer slackSrv.Close()
	bot := newTestBot(newMockDaemon(), slackSrv)
	bot.channel = "C123"
	if err := bot.NotifyNewEscalation(context.Background(), BeadEvent{
		ID: "gd-esc-code", Type: "escalation", Title: "Tenant escalation", Priority: 2,
		Fields: map[string]string{"operation_type": "gitlab_mr_merge", "context": "Their note:\n\n````\n```\n<!channel>\n````"},
	}); err != nil {
		t.Fatalf("NotifyNewEscalation: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	// The payload is JSON, which writes < > & as \u escapes: read it back.
	var blocks []any
	if err := json.Unmarshal([]byte(posted), &blocks); err != nil {
		t.Fatalf("blocks: %v\n%s", err, posted)
	}
	text, _ := json.Marshal(blocks)
	var plain strings.Builder
	walkMrkdwn(blocks, func(s string) string { plain.WriteString(s + "\n"); return s })
	if got := plain.String(); strings.Contains(got, "<!channel>") || !strings.Contains(got, "&lt;!channel&gt;") {
		t.Errorf("escalation context code not escaped: %s", text)
	}
}
