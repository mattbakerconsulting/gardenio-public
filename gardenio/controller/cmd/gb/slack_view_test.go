package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/pflag"
)

const slackViewFixtureDir = "../../../../tools/slack-view/test/fixture"

// The embedded script must be the one tools/slack-view ships and tests.
func TestSlackViewScriptInSync(t *testing.T) {
	src, err := os.ReadFile("../../../../tools/slack-view/slack-view.mjs")
	if err != nil {
		t.Skipf("tools/slack-view not in this checkout: %v", err)
	}
	if !bytes.Equal(src, slackViewScript) {
		t.Fatal("slack_view.mjs differs from tools/slack-view/slack-view.mjs; run: cp tools/slack-view/slack-view.mjs gardenio/controller/cmd/gb/slack_view.mjs")
	}
}

// slackViewEnv skips unless node and a playwright Chromium are usable, then
// returns a throwaway storageState file.
func slackViewEnv(t *testing.T) (state string) {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed")
	}
	if _, err := os.Stat(slackViewFixtureDir); err != nil {
		t.Skip("slack-view fixtures not in this checkout")
	}
	probe := exec.Command("node", "-e", "const p=require('/usr/local/lib/node_modules/playwright');process.exit(require('fs').existsSync(p.chromium.executablePath())?0:1)")
	if err := probe.Run(); err != nil && os.Getenv("CHROME_PATH") == "" {
		t.Skip("playwright chromium not available")
	}
	state = filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(state, []byte(`{"cookies":[],"origins":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return state
}

func runSlackViewCLI(t *testing.T, args ...string) (stdout string, err error) {
	t.Helper()
	var out bytes.Buffer
	slackViewCmd.SetOut(&out)
	slackViewCmd.SetErr(&bytes.Buffer{})
	t.Cleanup(func() {
		slackViewCmd.SetOut(nil)
		slackViewCmd.SetErr(nil)
		slackViewCmd.Flags().VisitAll(func(f *pflag.Flag) { _ = f.Value.Set(f.DefValue); f.Changed = false })
	})
	if err := slackViewCmd.ParseFlags(args); err != nil {
		t.Fatal(err)
	}
	err = runSlackView(slackViewCmd, slackViewCmd.Flags().Args())
	return out.String(), err
}

func fixtureURL(t *testing.T, name string) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join(slackViewFixtureDir, name))
	if err != nil {
		t.Fatal(err)
	}
	return "file://" + abs
}

func TestSlackViewCapturesFixture(t *testing.T) {
	state := slackViewEnv(t)
	out := filepath.Join(t.TempDir(), "out")
	stdout, err := runSlackViewCLI(t, "--storage-state", state, "--out", out, "--settle", "0s", "--json", fixtureURL(t, "channel.html"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		OutDir       string `json:"outDir"`
		MessagesPath string `json:"messagesPath"`
		Messages     []struct {
			Author string `json:"author"`
			Text   string `json:"text"`
		} `json:"messages"`
		Screenshots []struct {
			Path string `json:"path"`
		} `json:"screenshots"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("stdout is not the JSON contract: %v\n%s", err, stdout)
	}
	if doc.OutDir != out || doc.MessagesPath != filepath.Join(out, "messages.json") {
		t.Errorf("outDir/messagesPath = %q, %q", doc.OutDir, doc.MessagesPath)
	}
	if len(doc.Screenshots) != 4 {
		t.Fatalf("want 4 screenshots, got %d", len(doc.Screenshots))
	}
	for _, s := range doc.Screenshots {
		b, err := os.ReadFile(s.Path)
		if err != nil || !bytes.HasPrefix(b, []byte("\x89PNG")) {
			t.Errorf("%s is not a PNG (%v)", s.Path, err)
		}
	}
	if len(doc.Messages) != 3 || doc.Messages[0].Author != "gardenio" || !strings.HasPrefix(doc.Messages[0].Text, "Plan is built") {
		t.Errorf("unexpected messages: %+v", doc.Messages)
	}
}

func TestSlackViewSummaryOutput(t *testing.T) {
	state := slackViewEnv(t)
	out := filepath.Join(t.TempDir(), "out")
	stdout, err := runSlackViewCLI(t, "--storage-state", state, "--out", out, "--settle", "0s", fixtureURL(t, "channel.html"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"captured 3 message(s)", filepath.Join(out, "desktop-dark.png"), filepath.Join(out, "messages.json")} {
		if !strings.Contains(stdout, want) {
			t.Errorf("summary lacks %q:\n%s", want, stdout)
		}
	}
}

func TestSlackViewPassesThroughExitCodes(t *testing.T) {
	state := slackViewEnv(t)
	cases := []struct {
		name string
		args []string
		code int
	}{
		{"signed out", []string{"--timeout", "3s", fixtureURL(t, "signin.html")}, 3},
		{"no messages", []string{"--timeout", "1s", "--message-selector", ".nothing-here", fixtureURL(t, "channel.html")}, 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"--storage-state", state, "--out", filepath.Join(t.TempDir(), "o")}, tc.args...)
			_, err := runSlackViewCLI(t, args...)
			var ec *exitCodeError
			if !errors.As(err, &ec) || ec.code != tc.code {
				t.Fatalf("want exit %d, got %v", tc.code, err)
			}
		})
	}
}

func TestSlackViewRequiresSession(t *testing.T) {
	t.Setenv(slackViewStorageStateEnv, "")
	t.Setenv("SLACK_STORAGE_STATE", "")
	_, err := runSlackViewCLI(t, "C0123456789")
	var ec *exitCodeError
	if !errors.As(err, &ec) || ec.code != 2 {
		t.Fatalf("want usage exit 2, got %v", err)
	}
	if !strings.Contains(err.Error(), "Slack viewer session not provisioned") {
		t.Errorf("error should say the session is not provisioned, got %q", err)
	}
}

// An env var pointing at a file that is not there (Secret mounted optionally and
// not yet provisioned) gets the same clear message, before node is even looked for.
func TestSlackViewMissingSessionFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "storage-state.json")
	t.Setenv(slackViewStorageStateEnv, missing)
	_, err := runSlackViewCLI(t, "C0123456789")
	var ec *exitCodeError
	if !errors.As(err, &ec) || ec.code != 2 {
		t.Fatalf("want usage exit 2, got %v", err)
	}
	if !strings.Contains(err.Error(), "Slack viewer session not provisioned") || !strings.Contains(err.Error(), missing) {
		t.Errorf("error should say not provisioned and name the path, got %q", err)
	}
}
