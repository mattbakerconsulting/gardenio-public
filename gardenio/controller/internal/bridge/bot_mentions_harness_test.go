package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/slack-go/slack/slackevents"

	"gardenio/controller/internal/beadsapi"
)

func TestParseHarnessOverride(t *testing.T) {
	cases := []struct {
		in, wantH, wantRest string
	}{
		{"--harness codex fix it", "codex", "fix it"},
		{"--harness=codex fix it", "codex", "fix it"},
		{"harness:codex fix it", "codex", "fix it"},
		{"--harness", "", "--harness"},
		{"harness: fix it", "", "harness: fix it"},
		{"fix --harness codex", "", "fix --harness codex"},
	}
	for _, c := range cases {
		h, rest := parseHarnessOverride(c.in)
		if h != c.wantH || rest != c.wantRest {
			t.Errorf("parseHarnessOverride(%q) = (%q,%q), want (%q,%q)", c.in, h, rest, c.wantH, c.wantRest)
		}
	}
}

func TestParseModelHarnessFlags_EitherOrder(t *testing.T) {
	cases := []struct {
		in, wantModel, wantHarness, wantRest string
	}{
		{"--model opus fix", "opus", "", "fix"},
		{"--harness codex fix", "", "codex", "fix"},
		{"--harness codex --model gpt-5-codex fix", "gpt-5-codex", "codex", "fix"},
		{"--model=gpt-5-codex --harness=codex fix it", "gpt-5-codex", "codex", "fix it"},
		{"fix --model opus", "", "", "fix --model opus"},
	}
	for _, c := range cases {
		m, h, rest := parseModelHarnessFlags(c.in)
		if m != c.wantModel || h != c.wantHarness || rest != c.wantRest {
			t.Errorf("parseModelHarnessFlags(%q) = (%q,%q,%q), want (%q,%q,%q)", c.in, m, h, rest, c.wantModel, c.wantHarness, c.wantRest)
		}
	}
}

func TestResolveHarnessModel(t *testing.T) {
	cases := []struct {
		name, harness, model string
		want                 harnessSelection
		unrecognized         bool
		errContains          string
	}{
		{name: "nothing", want: harnessSelection{}},
		{name: "claude alias unchanged", model: "opus", want: harnessSelection{Model: "claude-opus-5-5"}},
		{name: "model codex shorthand", model: "codex", want: harnessSelection{Harness: "codex"}},
		{name: "model Codex case-insensitive", model: " Codex ", want: harnessSelection{Harness: "codex"}},
		{name: "harness codex alone", harness: "codex", want: harnessSelection{Harness: "codex"}},
		{name: "codex model id", harness: "codex", model: "gpt-5-codex", want: harnessSelection{Harness: "codex", Model: "gpt-5-codex"}},
		{name: "claude alias on codex rejected", harness: "codex", model: "opus", errContains: "Claude model"},
		{name: "claude full id on codex rejected", harness: "codex", model: "claude-opus-4-7", errContains: "Claude model"},
		{name: "junk on codex rejected", harness: "codex", model: "fable!", errContains: "Unrecognized Codex model"},
		{name: "codex id with no harness rejected", model: "gpt-5-codex", errContains: "--harness codex"},
		{name: "unknown harness", harness: "gpt", errContains: "Unrecognized harness"},
		{name: "model gemini shorthand", model: "gemini", want: harnessSelection{Harness: "gemini"}},
		{name: "harness gemini alone", harness: "gemini", want: harnessSelection{Harness: "gemini"}},
		{name: "gemini model id", harness: "gemini", model: "gemini-2.5-pro", want: harnessSelection{Harness: "gemini", Model: "gemini-2.5-pro"}},
		{name: "claude alias on gemini rejected", harness: "gemini", model: "opus", errContains: "Claude model"},
		{name: "codex id on gemini rejected", harness: "gemini", model: "gpt-5-codex", errContains: "Codex model"},
		{name: "junk on gemini rejected", harness: "gemini", model: "fable!", errContains: "Unrecognized Gemini model"},
		{name: "gemini id with no harness rejected", model: "gemini-2.5-pro", errContains: "--harness gemini"},
		{name: "gemini id on codex rejected", harness: "codex", model: "gemini-2.5-pro", errContains: "Gemini model and cannot run on the Codex"},
		{name: "gemini id case-insensitive", harness: "gemini", model: " Gemini-2.5-Pro ", want: harnessSelection{Harness: "gemini", Model: "gemini-2.5-pro"}},
		{name: "gemini conflicts with claude harness", harness: "claude", model: "gemini", errContains: "Conflicting"},
		{name: "conflict", harness: "claude", model: "codex", errContains: "Conflicting"},
		{name: "explicit claude harness + alias", harness: "claude", model: "sonnet", want: harnessSelection{Harness: "claude", Model: "claude-sonnet-5"}},
		{name: "unrecognized claude-side token falls through", model: "llama3", unrecognized: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, unrec, errMsg := resolveHarnessModel(c.harness, c.model)
			if c.errContains != "" {
				if !strings.Contains(errMsg, c.errContains) {
					t.Fatalf("errMsg = %q, want contains %q", errMsg, c.errContains)
				}
				return
			}
			if errMsg != "" {
				t.Fatalf("unexpected error %q", errMsg)
			}
			if got != c.want || unrec != c.unrecognized {
				t.Errorf("got (%+v, unrecognized=%v), want (%+v, %v)", got, unrec, c.want, c.unrecognized)
			}
		})
	}
}

// roleDaemon adds the optional ListRoleBeads surface to the shared mock.
type roleDaemon struct {
	*mockDaemon
	mu    sync.Mutex
	roles map[string]beadsapi.RoleInfo
}

func (d *roleDaemon) ListRoleBeads(context.Context) (map[string]beadsapi.RoleInfo, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.roles, nil
}

func mentionHarness(t *testing.T, daemon BeadClient, text string) (posted []string) {
	t.Helper()
	var mu sync.Mutex
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/conversations.replies" {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "messages": []map[string]any{}})
			return
		}
		_ = r.ParseForm()
		mu.Lock()
		posted = append(posted, r.Form.Get("text"))
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message_ts": "1234.5678"})
	}))
	defer slackSrv.Close()
	state, err := NewStateManager(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	bot := newTestBot(daemon, slackSrv)
	bot.state = state
	bot.botUserID = "U-BOT"
	bot.handleAppMention(context.Background(), &slackevents.AppMentionEvent{
		User: "U123", Text: text, TimeStamp: "7777.8888", Channel: "C-GARDENIO",
	})
	mu.Lock()
	defer mu.Unlock()
	return append([]string(nil), posted...)
}

func TestHandleAppMention_HarnessOverride(t *testing.T) {
	cases := []struct {
		name        string
		text        string
		roles       map[string]beadsapi.RoleInfo
		wantSpawn   bool
		wantHarness string
		wantModel   string
		wantMsg     string
	}{
		{
			name:        "model codex on opted-in role",
			text:        "<@U-BOT> --model codex fix the bug",
			roles:       map[string]beadsapi.RoleInfo{"thread": {AllowedHarnesses: []string{"codex"}}},
			wantSpawn:   true,
			wantHarness: "codex",
		},
		{
			name:        "harness codex with codex model",
			text:        "<@U-BOT> --harness codex --model gpt-5-codex fix the bug",
			roles:       map[string]beadsapi.RoleInfo{"thread": {Harness: "codex"}},
			wantSpawn:   true,
			wantHarness: "codex",
			wantModel:   "gpt-5-codex",
		},
		{
			name:    "claude-only role refuses codex",
			text:    "<@U-BOT> --harness codex fix the bug",
			roles:   map[string]beadsapi.RoleInfo{"thread": {}},
			wantMsg: "claude-only",
		},
		{
			name:    "claude alias on codex rejected",
			text:    "<@U-BOT> --harness codex --model opus fix the bug",
			roles:   map[string]beadsapi.RoleInfo{"thread": {AllowedHarnesses: []string{"codex"}}},
			wantMsg: "Claude model",
		},
		{
			name:      "plain claude spawn unchanged",
			text:      "<@U-BOT> --model opus fix the bug",
			roles:     map[string]beadsapi.RoleInfo{"thread": {}},
			wantSpawn: true,
			wantModel: "claude-opus-5-5",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			md := newMockDaemon()
			md.seedProjectWithChannel("gardenio", "C-GARDENIO")
			posted := mentionHarness(t, &roleDaemon{mockDaemon: md, roles: tc.roles}, tc.text)

			agents := filterAgentBeads(md.beads)
			if tc.wantSpawn != (len(agents) == 1) {
				t.Fatalf("agent beads = %d, wantSpawn=%v (posted: %q)", len(agents), tc.wantSpawn, posted)
			}
			for _, b := range agents {
				if got := b.Fields["harness"]; got != tc.wantHarness {
					t.Errorf("harness field = %q, want %q", got, tc.wantHarness)
				}
				if got := b.Fields["model"]; got != tc.wantModel {
					t.Errorf("model field = %q, want %q", got, tc.wantModel)
				}
			}
			if tc.wantMsg != "" && !strings.Contains(strings.Join(posted, "\n"), tc.wantMsg) {
				t.Errorf("expected a Slack post containing %q, got %q", tc.wantMsg, posted)
			}
		})
	}
}

func TestStripLeadingOverrideFlags_Harness(t *testing.T) {
	got, raw := stripLeadingOverrideFlags("--harness codex keep going")
	if got != "keep going" || raw != "codex" {
		t.Errorf("got (%q,%q), want (%q,%q)", got, raw, "keep going", "codex")
	}
}

// ADR 0005 §5 D: aliases live in the per-harness profile table. None ship yet
// (live pass pending), so this pins both the empty state and the lookup path.
func TestHarnessModelProfiles_Aliases(t *testing.T) {
	for h, p := range harnessModelProfiles {
		if len(p.Aliases) != 0 {
			t.Errorf("%s ships aliases %v before ids are fixed by a live pass", h, p.Aliases)
		}
	}
	old := harnessModelProfiles[beadsapi.HarnessGemini]
	t.Cleanup(func() { harnessModelProfiles[beadsapi.HarnessGemini] = old })
	p := old
	p.Aliases = map[string]string{"flash": "gemini-3.8-flash"}
	harnessModelProfiles[beadsapi.HarnessGemini] = p
	got, _, errMsg := resolveHarnessModel("gemini", "flash")
	if errMsg != "" || got.Model != "gemini-3.8-flash" {
		t.Errorf("alias lookup: got (%+v, %q)", got, errMsg)
	}
	if _, _, errMsg := resolveHarnessModel("codex", "flash"); !strings.Contains(errMsg, "Unrecognized Codex model") {
		t.Errorf("a Gemini alias must not leak onto Codex, got %q", errMsg)
	}
}
