package bridge

import (
	"testing"

	"github.com/slack-go/slack"
)

// TestClassifyBotMessage covers the noise buckets reported on ka-0000002440:
// squawks, bead-mirrors (created/claimed/closed/comment), Block-Kit interactive
// fallbacks, and the post-stop survey rate card. Conversational bot replies
// (e.g. gb slack reply posts) must come back untagged so the read-thread
// filter keeps them visible alongside human messages.
func TestClassifyBotMessage(t *testing.T) {
	tests := []struct {
		name string
		text string
		msg  slack.Msg
		want string
	}{
		{
			// Post-ka-0000007955 the squawk scaffolding renders the plain word
			// "agent" (no coopmux link) — the classifier keys off the emoji
			// prefix, so it still tags it as squawk noise.
			name: "squawk",
			text: ":speech_balloon: agent: making progress on X",
			want: SubtypeSquawk,
		},
		{
			name: "bead-mirror created",
			text: ":pencil2: Created task: *<https://beads/...|gd-AAA>*",
			want: SubtypeBeadMirror,
		},
		{
			name: "bead-mirror claimed",
			text: ":arrow_right: Claimed task: *<https://beads/...|gd-AAA>*",
			want: SubtypeBeadMirror,
		},
		{
			name: "bead-mirror created-and-claimed",
			text: ":arrow_right: Created & claimed task: *<https://beads/...|gd-AAA>*",
			want: SubtypeBeadMirror,
		},
		{
			name: "bead-mirror closed",
			text: ":white_check_mark: Closed task: *<https://beads/...|gd-AAA>*",
			want: SubtypeBeadMirror,
		},
		{
			// Post-ka-0000007955 the comment-forward scaffolding renders the
			// plain word "agent" (no coopmux link).
			name: "bead-mirror comment",
			text: ":memo: agent commented on `gd-AAA`:\n\nLooks good",
			want: SubtypeBeadMirror,
		},
		{
			name: "interactive fallback text",
			text: "This message contains interactive elements.",
			msg:  slack.Msg{Text: "This message contains interactive elements."},
			want: SubtypeInteractive,
		},
		{
			name: "interactive blocks-only post",
			text: "Some title rendered from blocks",
			msg: slack.Msg{
				// msg.Text is empty; extracted text came from blocks.
				Blocks: slack.Blocks{BlockSet: []slack.Block{
					slack.NewSectionBlock(slack.NewTextBlockObject("mrkdwn", "Some title rendered from blocks", false, false), nil, nil),
				}},
			},
			want: SubtypeInteractive,
		},
		{
			name: "survey rate card",
			text: "Rate this agent session (gd-AAA)",
			want: SubtypeSurvey,
		},
		{
			name: "conversational bot reply (gb slack reply)",
			text: "Yes, the migration is safe — confirmed by reading the lock log.",
			msg:  slack.Msg{Text: "Yes, the migration is safe — confirmed by reading the lock log."},
			want: "",
		},
		{
			name: "human-like memo without lifecycle verb is not a bead-mirror",
			text: ":memo: a quick note about something",
			msg:  slack.Msg{Text: ":memo: a quick note about something"},
			want: "",
		},
		{
			name: "empty text empty blocks",
			text: "",
			msg:  slack.Msg{},
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Default the msg.Text to the rendered text when the test didn't
			// override it — most cases don't need a custom raw payload.
			if tc.msg.Text == "" && len(tc.msg.Blocks.BlockSet) == 0 {
				tc.msg.Text = tc.text
			}
			got := classifyBotMessage(tc.text, tc.msg)
			if got != tc.want {
				t.Fatalf("classifyBotMessage(%q) = %q, want %q", tc.text, got, tc.want)
			}
		})
	}
}

// TestClassifyBotMessage_NoiseRatioRegression seeds a representative slice of
// messages from a typical agent thread (1 human reply + 4 noise buckets) and
// confirms the classifier tags every noise bucket while leaving the human
// reply and a conversational bot reply untagged. This is the property the
// ka-0000002440 acceptance criterion turns on: callers can drop noise and
// keep "actually wrote a reply" content.
func TestClassifyBotMessage_NoiseRatioRegression(t *testing.T) {
	noisy := []string{
		// Squawk + comment-forward render the plain word "agent" post-ka-0000007955.
		":speech_balloon: agent: starting work",
		":arrow_right: Created & claimed task: *<https://beads/...|gd-XYZ>*",
		":memo: agent commented on `gd-XYZ`:\n\nDone with phase 1",
		":white_check_mark: Closed task: *<https://beads/...|gd-XYZ>*",
		"Rate this agent session (gd-XYZ)",
	}
	for _, n := range noisy {
		got := classifyBotMessage(n, slack.Msg{Text: n})
		if got == "" {
			t.Errorf("expected noise classification for %q, got empty", n)
		}
	}

	// Conversational bot reply (e.g. via `gb slack reply`).
	conversational := "Yes — I think we should hold off on the merge."
	if got := classifyBotMessage(conversational, slack.Msg{Text: conversational}); got != "" {
		t.Errorf("expected conversational reply to be untagged, got %q", got)
	}
}
