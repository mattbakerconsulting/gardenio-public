package gardenhub

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// A bird's caption (step 2 of plan gd-jMl4oF2cJO; spec 2026-10-28-bird-captions-design.md section 5): made without a
// model from the few tool_input fields that say what a call is for, and scrubbed.

func TestCaptionOfSources(t *testing.T) {
	long := strings.Repeat("abcdefgh ", 12)
	for _, c := range []struct {
		tool    string
		in      toolFields
		want    string
		wantSrc string
	}{
		// Row 1: the description, as the harness wrote it (the section 2.3 samples).
		{"Bash", toolFields{Command: "kd show x", Description: "Show task, claim it, and read team news"}, "Show task, claim it, and read team news", CapSrcDescription},
		{"Bash", toolFields{Description: "Poll CI status for three refreshed PRs"}, "Poll CI status for three refreshed PRs", CapSrcDescription},
		{"Bash", toolFields{Description: "Find /w/ handling in beads-viewer on main"}, "Find /w/ handling in beads-viewer on main", CapSrcDescription},
		{"Monitor", toolFields{Description: "Watch the CI run"}, "Watch the CI run", CapSrcDescription},
		{"run_shell_command", toolFields{Command: "echo hi", Description: "mock tool call"}, "mock tool call", CapSrcDescription}, // Gemini
		// Row 8 waits for Haiku (step 3): a Bash call with no description (Codex's always) is empty.
		{"Bash", toolFields{Command: "cd /w && ls"}, "", CapSrcNone},
		{"Bash", toolFields{Description: "   "}, "", CapSrcNone},
		// Row 2: a file, repo-relative inside the checkout, else its basename.
		{"Read", toolFields{FilePath: "/home/agent/bot/gardenio/work/gardenio/controller/internal/gardenhub/web/birds.js"}, "Read gardenio/controller/internal/gardenhub/web/birds.js", CapSrcPath},
		{"Edit", toolFields{FilePath: "/home/agent/bot/gardenio/work/go.work"}, "Edit go.work", CapSrcPath},
		{"MultiEdit", toolFields{FilePath: "/workspace/src/main.rs"}, "Edit src/main.rs", CapSrcPath},
		{"Write", toolFields{FilePath: "/home/agent/.claude-tmp/claude-1000/x/scratchpad/desc_samples.txt"}, "Write desc_samples.txt", CapSrcPath},
		{"NotebookEdit", toolFields{NotebookPath: "/tmp/n.ipynb"}, "Edit n.ipynb", CapSrcPath},
		{"read_file", toolFields{FilePath: "/work/README.md"}, "Read README.md", CapSrcPath}, // Gemini
		{"Read", toolFields{}, "", CapSrcNone},
		// Rows 3 and 4.
		{"Grep", toolFields{Pattern: "func captionOf"}, `Search for "func captionOf"`, CapSrcPattern},
		{"Grep", toolFields{Pattern: long}, `Search for "abcdefgh abcdefgh abcdefgh abcdefgh…"`, CapSrcPattern},
		{"Glob", toolFields{Pattern: "**/*_test.go"}, "Find files **/*_test.go", CapSrcPattern},
		{"WebFetch", toolFields{URL: "https://user:pw@docs.example.com:8443/a/b?token=x#y"}, "Fetch docs.example.com", CapSrcHost},
		{"WebFetch", toolFields{URL: "not a url"}, "", CapSrcNone},
		{"WebSearch", toolFields{Query: "go regexp word boundary"}, "Search the web: go regexp word boundary", CapSrcHost},
		// Row 5: the hawk's caption.
		{"Agent", toolFields{Description: "Explore gardenhub transcript endpoints"}, "Explore gardenhub transcript endpoints", CapSrcAgent},
		{"Task", toolFields{Description: "Review the diff"}, "Review the diff", CapSrcAgent},
		// Row 6: a fixed phrase.
		{"ToolSearch", toolFields{Query: "select:Monitor"}, "Load tools", CapSrcBuiltin},
		{"Skill", toolFields{Skill: "code-review"}, "Use skill code-review", CapSrcBuiltin},
		{"Monitor", toolFields{Command: "tail -f x"}, "Watch a background command", CapSrcBuiltin},
		{"ScheduleWakeup", toolFields{}, "Schedule a wake-up", CapSrcBuiltin},
		{"TaskStop", toolFields{}, "Stop a background task", CapSrcBuiltin},
		{"Artifact", toolFields{}, "Work on an artifact", CapSrcBuiltin},
		{"ListAgents", toolFields{}, "List agents", CapSrcBuiltin},
		{"CronCreate", toolFields{}, "Schedule a task", CapSrcBuiltin},
		// Row 7: an MCP tool in words; its input is never read.
		{"mcp__slack__post_message", toolFields{Description: "hunter2-SECRET"}, "slack: post message", CapSrcMCP},
		{"mcp__claude_ai_Slack__slack-send", toolFields{}, "claude ai Slack: slack send", CapSrcMCP},
		{"mcp____x", toolFields{}, "", CapSrcNone},
		// Row 9.
		{"SomethingNew", toolFields{Description: "x"}, "", CapSrcNone},
	} {
		got, src := captionOf(c.tool, c.in)
		if got != c.want || src != c.wantSrc {
			t.Errorf("captionOf(%q, %+v) = %q (%s), want %q (%s)", c.tool, c.in, got, src, c.want, c.wantSrc)
		}
	}
}

// TestCaptionToolLabel: the tool label of an uncaptioned call keeps the names captionOf knows and folds the rest, so
// gardenio_farm_uncaptioned_total stays bounded.
func TestCaptionToolLabel(t *testing.T) {
	for tool, want := range map[string]string{
		"Bash": "Bash", "Read": "Read", "run_shell_command": "run_shell_command", "Artifact": "Artifact",
		"mcp__slack__post_message": "mcp", "SomethingNew": "other", "": "other",
	} {
		if got := captionToolLabel(tool); got != want {
			t.Errorf("captionToolLabel(%q) = %q, want %q", tool, got, want)
		}
	}
}

func TestScrubCaption(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		// Shape: one line, no control characters, no surrounding quotes or backticks.
		{"  Run   the\ttests\n\x00again  ", "Run the tests again"},
		{"`go vet` the package", "`go vet` the package"},
		{"'single'", "single"},
		{`"Quoted description"`, "Quoted description"},
		// Secrets: toollatency.RedactText.
		{"Call API with token=hunter2abc", "Call API with token=[REDACTED]"},
		{"Auth with Bearer PLANTEDTOKEN now", "Auth with Bearer [REDACTED] now"},
		{"Use sk-ant-PLANTED123456 key", "Use [REDACTED] key"},
		// Paths: repo-relative in a checkout, else the basename; a URL route is kept.
		{"Diff /home/agent/bot/gardenio/work/gardenio/docs/a.md against main", "Diff gardenio/docs/a.md against main"},
		{"Count lines in /home/agent/.claude-tmp/claude-1000/-home-agent-bot-gardenio-work/2cb1/scratchpad/desc_samples.txt", "Count lines in desc_samples.txt"},
		{"Tail /tmp/build.log and /proc/meminfo.", "Tail build.log and /proc/meminfo."},
		{"Read ~/.ssh/config", "Read config"},
		{"Check out=/home/agent/private-path/out.txt", "Check out=out.txt"},
		{"Escape /home/agent/bot/gardenio/work/../../.kube/config", "Escape config"},
		{"Find /w/ handling and /api/slack routes", "Find /w/ handling and /api/slack routes"},
		// URLs: the host only; a PR number stays.
		{"Open https://github.com/x/y/pull/9 for #1282 and PR 1287", "Open github.com for #1282 and PR 1287"},
		// Opaque tokens and emails.
		{"Decode aGVsbG8gd29ybGQgdGhpcyBpcyBhIGxvbmcgYmFzZTY0IHN0cmluZw== now", "Decode … now"},
		{"Checkout 3c6a2ba8e5fa7461c327a624e71267fa27aa3ed4", "Checkout …"},
		{"Checkout 3c6a2ba8", "Checkout 3c6a2ba8"},
		{"Edit gardenio/controller/internal/gardenhub/activity_view_test.go", "Edit gardenio/controller/internal/gardenhub/activity_view_test.go"},
		{"Ping matthew.baker@example.com about it", "Ping about it"},
		// Bead ids are kept here: maskScoped takes them out for a scoped viewer.
		{"Show gd-gCHKhIROvB and ka-0000004089", "Show gd-gCHKhIROvB and ka-0000004089"},
	} {
		if got := scrubCaption(c.in); got != c.want {
			t.Errorf("scrubCaption(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestScrubCaptionLength(t *testing.T) {
	// The cap: 80 runes, cut at a word, ending in ….
	long := "Trial-merge main into #1282 in a detached worktree, then rebuild the gardenhub package and rerun every test"
	got := scrubCaption(long)
	if n := utf8.RuneCountInString(got); n > captionMax || !strings.HasSuffix(got, "…") || !strings.HasPrefix(long, strings.TrimSuffix(got, "…")) {
		t.Errorf("long caption %q (%d runes)", got, n)
	}
	if strings.HasSuffix(strings.TrimSuffix(got, "…"), "rebui") {
		t.Errorf("cut inside a word: %q", got)
	}
	// A repo path that would overflow is shortened to its last two segments.
	deep := "Compare /home/agent/bot/gardenio/work/gardenio/controller/internal/gardenhub/web/birds.js with the golden one on main"
	if got := scrubCaption(deep); got != "Compare web/birds.js with the golden one on main" {
		t.Errorf("deep path: %q", got)
	}
	if got := scrubCaption(strings.Repeat("é", 200)); utf8.RuneCountInString(got) != captionMax {
		t.Errorf("a wordless caption is cut to %d runes: %d", captionMax, utf8.RuneCountInString(got))
	}
}

func TestMaskScoped(t *testing.T) {
	for in, want := range map[string]string{
		"Show gd-gCHKhIROvB and claim it":    "Show a bead and claim it",
		"Read ka-0000004089 advice":          "Read a bead advice",
		"Close gd-gCHKhIROvB, gd-4coyNrCKd9": "Close a bead, a bead",
		"Poll CI for #1282":                  "Poll CI for #1282",
		"":                                   "",
		// Fleet spend never reaches a scoped viewer (gd-Kvh3fdmFg0).
		"Check fleet spend $412.50 today":  "Check fleet spend an amount today",
		"Compare US$1,200 against 3.5 USD": "Compare an amount against an amount",
		"Budget is $2k a day":              "Budget is an amount a day",
		// How the fleet is built: cluster hosts, addresses, AWS accounts.
		"Curl gardenio-beads.gardenio.svc.cluster.local:8080 health":          "Curl a host health",
		"Query gardenio-nats.gardenio.svc for streams":                        "Query a host for streams",
		"Query 10.43.244.129:9090 metrics":                                    "Query an address metrics",
		"Push to 123456789012.dkr.ecr.us-east-1.amazonaws.com/gardenio/agent": "Push to an AWS host/gardenio/agent",
		"Read arn:aws:iam::123456789012:role/gardenio-admin trust":            "Read an AWS resource trust",
		"Check account 123456789012 quota":                                    "Check account an account quota",
		// Captions from the 2026-10-31 sample that must pass as they are.
		"Fetch github.com":                                 "Fetch github.com",
		"Confirm coopmux.transport.posture unread":         "Confirm coopmux.transport.posture unread",
		"Release record grows 13 KB (762,800 → 775,812 B)": "Release record grows 13 KB (762,800 → 775,812 B)",
		"Check out release 2026.303.1 and diff values":     "Check out release 2026.303.1 and diff values",
		"Read web/birds.js":                                "Read web/birds.js",
		"Remove /v1/blocked handler and route":             "Remove /v1/blocked handler and route",
	} {
		if got := maskScoped(in); got != want {
			t.Errorf("maskScoped(%q) = %q, want %q", in, got, want)
		}
	}
}
