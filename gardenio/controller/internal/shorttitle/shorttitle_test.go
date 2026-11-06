package shorttitle

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestDerive pins the heuristics against titles sampled from the live daemon
// on 2026-10-11 (thread gd-8Gxghn6hkt). Each expectation is what a human
// reader would want to see as the bead's short name.
func TestDerive(t *testing.T) {
	cases := []struct {
		name     string
		beadType string
		title    string
		want     string
	}{
		{
			name:     "plan epic: plain title survives",
			beadType: "epic",
			title:    "Attribute agent slowness to specific tool calls and commands",
			want:     "Attribute agent slowness to specific…",
		},
		{
			name:     "decision: self-ref id stripped, trailing question kept",
			beadType: "decision",
			title:    "Plan-gardener gd-Oc3GVm9gc6 hits its 8-run budget this cycle — extend watchdog coverage?",
			want:     "extend watchdog coverage?",
		},
		{
			name:     "task: plan ordinal stripped",
			beadType: "task",
			title:    "P0: forward duration_ms + tool_use_id through gb hook relay",
			want:     "forward duration_ms + tool_use_id…",
		},
		{
			name:     "task: ordinal stripped, first clause before dash",
			beadType: "task",
			title:    "P5: surface tool/command latency — Grafana panels + gb tools top",
			want:     "surface tool/command latency",
		},
		{
			name:     "escalation: reference prefix is the name",
			beadType: "escalation",
			title:    "Merge PR #209: P4 per-session slowest tool calls (gd-YcmtujOTrC)",
			want:     "Merge PR #209",
		},
		{
			name:     "bug: component prefix stripped, trailing parenthetical dropped",
			beadType: "bug",
			title:    "beads-viewer: admin writes always 403 (static token can't carry gardenio-admin since gd-ZjiSd9Kzio)",
			want:     "admin writes always 403",
		},
		{
			name:     "bug: nested component path prefix stripped",
			beadType: "bug",
			title:    "slack-bridge: themeFor does not cache a failed project listing (N listings per card during a daemon outage)",
			want:     "themeFor does not cache a failed…",
		},
		{
			name:     "schedule run: name + HH:MM",
			beadType: "task",
			title:    "[2026-10-11T15:20] Schedule: cicd watchdog: 20-min GitHub Actions stall/red-main preflight",
			want:     "cicd watchdog 15:20",
		},
		{
			name:     "schedule run: multi-word name",
			beadType: "task",
			title:    "[2026-10-11T15:07] Schedule: Merge desk: review + merge crew PRs from open github_pr_merge escalations",
			want:     "Merge desk 15:07",
		},
		{
			name:     "agent: handle, never the title shape",
			beadType: "agent",
			title:    "gardenio-p3-measure-the-u4n",
			want:     "@gardenio-p3-measure-the-u4n",
		},
		{
			name:     "agent: empty title derives nothing",
			beadType: "agent",
			title:    "  ",
			want:     "",
		},
		{
			name:     "thread bead: bridge prefix stripped",
			beadType: "task",
			title:    "Slack thread: messages sent to slack often contain links to beads.. the thing is often",
			want:     "messages sent to slack often contain…",
		},
		{
			name:     "release bug: trailing parenthetical dropped, version kept",
			beadType: "bug",
			title:    "release-deploy 2026.284.4: helm upgrade failed (atomic rollback applied)",
			want:     "release-deploy 2026.284.4",
		},
		{
			name:     "empty title derives nothing",
			beadType: "task",
			title:    "   ",
			want:     "",
		},
		{
			name:     "title that is only an id derives nothing",
			beadType: "task",
			title:    "gd-abcdef",
			want:     "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Derive(tc.beadType, tc.title)
			if got != tc.want {
				t.Fatalf("Derive(%q, %q)\n got %q\nwant %q", tc.beadType, tc.title, got, tc.want)
			}
			if n := utf8.RuneCountInString(got); n > Max {
				t.Fatalf("short title %q is %d runes, over %d", got, n, Max)
			}
		})
	}
}

func TestDerive_NeverContainsBeadID(t *testing.T) {
	titles := []string{
		"Plan-gardener gd-Oc3GVm9gc6 watchdog gd-Lh590iWd8Y budget",
		"ka-0000001234 fix the thing (ka-0000005678)",
		"gd-abc: colon after id",
	}
	for _, title := range titles {
		if got := Derive("task", title); beadID.MatchString(got) {
			t.Errorf("Derive(%q) leaked an id: %q", title, got)
		}
	}
}

func TestDerive_LongHandleBounded(t *testing.T) {
	got := Derive("agent", strings.Repeat("a", 60))
	if n := utf8.RuneCountInString(got); n > Max {
		t.Fatalf("agent short title %q is %d runes, over %d", got, n, Max)
	}
}

func TestClip(t *testing.T) {
	sentence := "a sixty rune sentence that must be cut at a word boundary ok"
	if n := utf8.RuneCountInString(sentence); n != 60 {
		t.Fatalf("fixture is %d runes, want 60", n)
	}
	cases := []struct {
		in   string
		max  int
		want string
	}{
		{"forward duration_ms through gb hook", 40, "forward duration_ms through gb hook"},
		{sentence, 40, "a sixty rune sentence that must be cut…"},
		{"  collapse \n\t whitespace  ", 40, "collapse whitespace"},
		{"", 40, ""},
		{"short", 10, "short"},
		{"exactly ten", 11, "exactly ten"},
		{"cut at a word boundary please", 14, "cut at a word…"},
		{"cut-at-a-word-boundary-please", 14, "cut-at-a-word…"},
		{"trailing comma, here we go", 16, "trailing comma…"},
		{"ünïcödé rünes are counted not bytes", 14, "ünïcödé rünes…"},
		{"anything", 0, ""},
	}
	for _, tc := range cases {
		got := Clip(tc.in, tc.max)
		if got != tc.want {
			t.Errorf("Clip(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
		}
		if n := utf8.RuneCountInString(got); n > tc.max {
			t.Errorf("Clip(%q, %d) = %q is %d runes, over the bound", tc.in, tc.max, got, n)
		}
	}
}

func TestGated(t *testing.T) {
	for _, typ := range []string{"task", "bug", "feature", "epic", "decision", "escalation", "agent"} {
		if !Gated(typ) {
			t.Errorf("Gated(%q) = false, want true", typ)
		}
	}
	for _, typ := range []string{"", "session", "message", "trigger", "schedule", "mail", "bundle", "config", "chore"} {
		if Gated(typ) {
			t.Errorf("Gated(%q) = true, want false", typ)
		}
	}
}

func TestHandle(t *testing.T) {
	cases := map[string]string{
		"gardenio-task-6-controller-scx":  "gardenio-task-6-controller-scx",
		"@gardenio-task-6-controller-scx": "gardenio-task-6-controller-scx",
		"  ":                              "",
	}
	for in, want := range cases {
		if got := Handle(in); got != want {
			t.Errorf("Handle(%q) = %q, want %q", in, got, want)
		}
	}
	if n := utf8.RuneCountInString(Handle(strings.Repeat("a", 60))); n > Max {
		t.Errorf("Handle of a 60-rune name is %d runes, over %d", n, Max)
	}
}
