package captioner

import (
	"strings"
	"testing"
)

func TestPrepareCommand(t *testing.T) {
	cases := []struct{ in, want string }{
		{"git status", "git status"},
		{"cd /home/agent/bot/gardenio/work && git log --oneline -3", "git log --oneline -3"},
		{"cd /home/agent/bot/gardenio/work/gardenio/controller && cd internal; go vet ./...", "go vet ./..."},
		{"S=/home/agent/.claude-tmp/claude-1000/x/scratchpad; cat $S/desc_samples.txt", "cat $S/desc_samples.txt"},
		{"export T=/tmp/run && wc -l /tmp/run/out.log", "wc -l out.log"},
		// The repo checkout keeps its relative path; anything else is its basename.
		{"sed -n 1,40p /home/agent/bot/gardenio/work/gardenio/controller/internal/gardenhub/activity.go",
			"sed -n 1,40p gardenio/controller/internal/gardenhub/activity.go"},
		{"cat ~/.config/gh/hosts.yml", "cat hosts.yml"},
		{"grep -rn x --include=*.go /home/agent/bot", "grep -rn x --include=*.go bot"},
		// URL routes are kept, URLs are cut to their host.
		{`curl -s http://localhost:8080/api/slack`, `curl -s localhost:8080`},
		{`gh pr view https://github.com/acme/app/pull/1287 --json state`, `gh pr view github.com --json state`},
		{`grep -n "/w/" web/garden.js`, `grep -n "/w/" web/garden.js`},
		// Secrets, opaque tokens and emails go.
		{`curl -H "Authorization: Bearer fake-token-abc" x`, `curl -H "Authorization: Bearer [REDACTED] x`}, // RedactText's Bearer rule takes the quote too
		{"echo aGVsbG8gd29ybGQgdGhpcyBpcyBh/bG9uZyB0b2tlbjEyMw==", "echo …"},
		{`GITHUB_TOKEN=ghp_faketoken0123456789 gh api user`, `GITHUB_TOKEN=[REDACTED] gh api user`},
		{"echo deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", "echo …"},
		{"git log --author=someone@example.com", "git log --author="},
		{"printf 'a\tb\nc'\x07", "printf 'a b c'"},
		{"cd /tmp &&", ""},
	}
	for _, c := range cases {
		if got := PrepareCommand(c.in); got != c.want {
			t.Errorf("PrepareCommand(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}

func TestPrepareCommandCut(t *testing.T) {
	got := PrepareCommand("echo " + strings.Repeat("é ", 400))
	if len(got) > maxCommandBytes {
		t.Fatalf("%d bytes, want at most %d", len(got), maxCommandBytes)
	}
	if !strings.HasPrefix(got, "echo é") || strings.ContainsRune(got, '�') {
		t.Fatalf("cut off a rune boundary: %q", got[len(got)-8:])
	}
}

func TestScrubReply(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Show recent commits", "Show recent commits"},
		{`"Check pull request state"`, "Check pull request state"},
		{"`List pods`", "List pods"},
		{"Read /home/agent/bot/gardenio/work/gardenio/controller/internal/gardenhub/web/birds.js", "Read web/birds.js"},
		{"Count lines in /tmp/x/out.log", "Count lines in out.log"},
		{"Fetch https://example.com/a/b?c=d", "Fetch example.com"},
		{"Use token sk-ant-0123456789abcdef", "Use token [REDACTED]"},
	}
	for _, c := range cases {
		if got := Scrub(c.in); got != c.want {
			t.Errorf("Scrub(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCommandOfReadsOnlyTheCommand(t *testing.T) {
	in := []byte(`{"command":"ls -la","description":"","cwd":"/home/agent/secret","api_key":"x","run_in_background":true}`)
	if got := CommandOf(in); got != "ls -la" {
		t.Fatalf("CommandOf = %q", got)
	}
	if got := CommandOf([]byte(`"truncated {"command":"ls`)); got != "" {
		t.Fatalf("CommandOf(truncated string) = %q, want empty", got)
	}
}
