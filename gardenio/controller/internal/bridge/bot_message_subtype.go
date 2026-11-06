package bridge

import (
	"strings"

	"github.com/slack-go/slack"
)

// Bot-message subtype tags surfaced via the Slack thread/channel/search read API.
// Callers (notably `gb slack thread` / `read-thread` / `channel` / `search`) use
// these to filter out high-noise auto-posts (squawks, bead lifecycle mirrors,
// Block-Kit-only interactive cards, agent-survey rate cards) while still
// preserving conversational bot replies (e.g. `gb slack reply` posts).
//
// The classifier is a heuristic over the rendered text + raw Slack message
// because the bridge has many independent post sites (squawk.go, comment_forward.go,
// bot_beadactivity.go, bot_human_survey.go, agent cards, decision modals, ...);
// tagging each post site with a stable subtype at write time would require a
// schema-breaking change to slack-go's MsgOption shape. Classifying at read
// time keeps the change local and backwards-compatible with existing posts.
const (
	// SubtypeSquawk: `:speech_balloon: <agent>: <text>` posts from the squawk
	// stream. Source: squawk.go ("formatted := :speech_balloon: %s: %s").
	SubtypeSquawk = "squawk"

	// SubtypeBeadMirror: bead-lifecycle notifications mirrored into the thread.
	// Sources: bot_beadactivity.go (`:pencil2:` Created, `:arrow_right:` Claimed
	// / Created & claimed, `:white_check_mark:` Closed) and comment_forward.go
	// (`:memo: <agent> commented on <bead>`).
	SubtypeBeadMirror = "bead-mirror"

	// SubtypeInteractive: Block-Kit-only posts whose plain-text payload is the
	// Slack-default fallback "This message contains interactive elements."
	// Agent cards, decision modals, and similar UI render through here.
	SubtypeInteractive = "interactive"

	// SubtypeSurvey: the post-stop "Rate this agent session" rate card.
	// Source: bot_human_survey.go / bot_human_survey_manual.go.
	SubtypeSurvey = "survey"
)

// classifyBotMessage returns a stable subtype tag for noisy bridge auto-posts,
// or "" for human messages and conversational bot replies. The decision is
// made on the rendered text (already extracted via extractMessageText) plus
// the raw Slack message for fallback-text and block-presence heuristics.
//
// Only bot messages are passed in; the caller gates on isBot.
func classifyBotMessage(text string, msg slack.Msg) string {
	trimmed := strings.TrimSpace(text)

	// Survey rate-card. The block-kit content includes "Rate this agent
	// session" plus a list of buttons; both extracted text and Slack's
	// fallback text begin with this prefix.
	if strings.HasPrefix(trimmed, "Rate this agent session") {
		return SubtypeSurvey
	}

	// Slack's auto-fallback for messages posted with blocks but no fallback
	// text. We also catch the case where the raw msg.Text is the fallback
	// even if extractMessageText pulled richer text from blocks (some agent
	// cards do this).
	rawTrim := strings.TrimSpace(msg.Text)
	if rawTrim == "This message contains interactive elements." {
		return SubtypeInteractive
	}
	// Block-Kit-only posts (no Text, blocks present) — these are interactive
	// UI cards even when extractMessageText fishes content from sections.
	if rawTrim == "" && len(msg.Blocks.BlockSet) > 0 {
		return SubtypeInteractive
	}

	// Squawk scaffolding: `:speech_balloon: <agentLink>: <body>` — or, once the
	// producer is themed, the channel theme's note glyph in its place.
	if strings.HasPrefix(trimmed, ":speech_balloon:") || hasThemedPrefix(trimmed, SemNote, "") {
		return SubtypeSquawk
	}

	// Bead-lifecycle mirrors. We match on the leading emoji + the lifecycle
	// verb so we don't accidentally swallow a human message that happens to
	// begin with `:memo:` or `:arrow_right:`.
	if isBeadMirror(trimmed) {
		return SubtypeBeadMirror
	}

	return ""
}

// hasThemedPrefix reports whether text opens with any theme's glyph for the
// semantic key followed by suffix. The classifier reads messages posted into
// channels of either theme, so it must recognise every theme's glyph, not just
// the legacy shortcode the producers emit today.
func hasThemedPrefix(text, key, suffix string) bool {
	for _, th := range []Theme{Garden, Business} {
		if g := th.Semantic(key); g != "" && strings.HasPrefix(text, g+suffix) {
			return true
		}
	}
	return false
}

// isBeadMirror returns true when the text begins with a known bead-lifecycle
// emoji + verb pair. Keeping this strict avoids classifying arbitrary human
// emoji-prefixed messages as noise.
func isBeadMirror(trimmed string) bool {
	prefixes := []string{
		":pencil2: Created ",
		":arrow_right: Claimed ",
		":arrow_right: Created & claimed ",
		":white_check_mark: Closed ",
		":memo: ", // ":memo: <agent> commented on …"
	}
	if hasThemedPrefix(trimmed, SemOK, " Closed ") {
		return true
	}
	for _, p := range prefixes {
		if strings.HasPrefix(trimmed, p) {
			// For :memo:, additionally require " commented on " to avoid
			// catching a human ":memo: note about X" message.
			if p == ":memo: " && !strings.Contains(trimmed, " commented on ") {
				continue
			}
			return true
		}
	}
	return false
}

// isUtteranceSubtype reports whether a Slack message event of this subtype is
// something a person said, which the reply and mention handlers take. Most
// subtypes are not (edits, deletes, joins, bot posts), but a couple DO carry a
// genuine user utterance and must be treated like a normal reply:
//   - `thread_broadcast`: a thread reply sent with "Also send to channel" —
//     text/user/ThreadTimeStamp are populated like a normal reply, otherwise
//     the bound agent never gets nudged (ka-0000001650).
//   - `file_share`: a file/image upload. Slack dispatches a threaded upload
//     over the Events API with this subtype (any caption rides in ev.Text).
//     Without letting it through, an image-only or file+caption follow-up in
//     a wish thread never reaches handleThreadForward, so it never becomes a
//     /threads bubble nor nudges the agent (ka-0000003513).
func isUtteranceSubtype(subtype string) bool {
	return subtype == "" || subtype == "thread_broadcast" || subtype == "file_share"
}
