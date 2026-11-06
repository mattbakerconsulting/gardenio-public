package main

// hook_assignment.go — renders the `## Assignment` block for `gb hook prime`.
//
// The block is `kd show <agent bead>` with any oversized field value elided.
// It exists because the agent config bead's `prompt:` field IS the whole agent
// prompt, so a raw `kd show` re-printed the entire spawn prompt a second time
// into a fresh session — ~17.7KB of duplicated input tokens on every spawn for
// a gardener bead, on gardeners that fire every 20-30m (ka-0000006516).

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"unicode/utf8"
)

// assignmentFieldMaxBytes is the ceiling for a single field value rendered into
// the `## Assignment` block. Values above it are replaced by a first-line
// preview plus a pointer to the full text.
//
// 512 is calibrated against the live corpus: only three field keys ever exceed
// 200 bytes (`prompt`, `wrapup`, `schedule_terminal_goal`). It elides `prompt`
// (median 1.8KB, max 17.7KB) and `wrapup` (median 1.2KB) while preserving the
// orientation-useful `schedule_terminal_goal` (median 148B) intact.
const assignmentFieldMaxBytes = 512

// assignmentPreviewRunes caps the retained head of an elided value. The preview
// is what lets a reader tell WHICH prompt/wrapup was elided.
const assignmentPreviewRunes = 120

// runKD shells `kd` with args and returns its stdout. Variable so tests can
// stub the two `kd show` round-trips without a live daemon.
var runKD = func(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "kd", args...).Output()
}

// renderAssignmentBlock returns the body of the `## Assignment` block for
// beadID, or "" when the bead cannot be shown.
//
// It starts from the `kd show` TEXT rendering rather than rebuilding the block
// from `--json`, because the text output carries sections the JSON does not:
// `Depends On` / `Children` / `Mentions`. For an agent bead the `Depends On`
// edge IS the assigned task, so a JSON-rebuilt block would drop the single most
// useful line in it.
//
// Two independent trims run over the text: elideBulkyAssignmentFields collapses
// oversized FIELD values (ka-0000006516), and capAssignmentComments caps the
// Comments section (ka-0000001998). They touch disjoint sections, and fields run
// first so a giant `wrapup` field that echoes a `Comments:` header is elided
// before the comment cap ever scans for one.
func renderAssignmentBlock(ctx context.Context, beadID string) string {
	out, err := runKD(ctx, "show", beadID)
	if err != nil || len(out) == 0 {
		return ""
	}
	text := elideBulkyAssignmentFields(ctx, beadID, string(out))
	return capAssignmentComments(text, beadID)
}

// elideBulkyAssignmentFields replaces oversized field values in the `kd show`
// text with a preview + pointer. See elideAssignmentFieldValues for how each
// replacement is anchored.
//
// On any failure to read the structured fields, the text is returned as-is.
func elideBulkyAssignmentFields(ctx context.Context, beadID, text string) string {
	// Fields are fetched as map[string]any, not map[string]string: agent beads
	// carry numeric fields (context_tokens, live_turns) that fail a
	// string-typed unmarshal and would drop every field.
	raw, err := runKD(ctx, "show", beadID, "--jq", ".fields")
	if err != nil || len(raw) == 0 {
		return text
	}
	var fields map[string]any
	if json.Unmarshal(raw, &fields) != nil {
		return text
	}
	return elideAssignmentFieldValues(text, fields, beadID)
}

// elideAssignmentFieldValues is the pure core of the elision: given the rendered
// text and the bead's structured fields, it replaces each oversized string value
// with its elided form.
//
// Each replacement is anchored on the FIELD LINE `kd show` emits for that key —
// "\n  " + key padded to 24 + " " + value — never on the value alone. Anchoring
// on the value alone is unsound in three ways this avoids:
//
//   - One field's value can CONTAIN another's. A chain-spawned gardener gets
//     "Terminal goal: <goal>" written into its prompt (bridge/
//     gardener_chain_spawn.go) while schedule_terminal_goal stores the same text,
//     so the goal is a verbatim substring of the prompt. Replacing the goal
//     first splices the marker INSIDE the prompt's value, after which the
//     prompt no longer matches and its whole body survives.
//   - Go randomizes map iteration order, so which of those two ran first — and
//     therefore whether the output was correct — varied run to run.
//   - A value can also appear in the Description or a Comment body, which
//     `kd show` prints outside the Fields section.
//
// Keying the anchor makes the result deterministic and independent of iteration
// order, and confines every write to one field's own line.
func elideAssignmentFieldValues(text string, fields map[string]any, beadID string) string {
	// Confine the search to the region at/after the "Fields:" header so a value
	// echoed in the Description can never be the one rewritten.
	head, tail, ok := splitAtFieldsSection(text)
	if !ok {
		return text
	}

	for k, v := range fields {
		// Only string values are elided. Strings render byte-verbatim, so the
		// anchor is exact; numbers/bools/composites render through
		// output.go's formatFieldValue (compact JSON for composites), which
		// would have to be replicated byte-for-byte to anchor on. Non-string
		// values are also small in practice: across the live corpus the only
		// one over the threshold is a decision bead's `options` array (~886B
		// rendered), and a decision bead is never an agent bead, so it is not
		// reachable through BOAT_AGENT_BEAD_ID. Left intact deliberately.
		s, ok := v.(string)
		if !ok || len(s) <= assignmentFieldMaxBytes {
			continue
		}
		// Mirrors output.go's "  %-24s %s\n" field-line layout.
		prefix := fmt.Sprintf("\n  %-24s ", k+":")
		tail = strings.Replace(tail, prefix+s, prefix+elidedFieldValue(s, beadID), 1)
	}
	return head + tail
}

// splitAtFieldsSection splits text just before the "Fields:" header line so
// callers can confine a replacement to the Fields section and everything after
// it. ok is false when there is no Fields section, in which case the caller must
// leave the text alone — there are no field lines to anchor to, and any value
// match would be in the Description.
func splitAtFieldsSection(text string) (head, tail string, ok bool) {
	const marker = "\nFields:\n"
	i := strings.Index(text, marker)
	if i < 0 {
		return text, "", false
	}
	return text[:i], text[i:], true
}

// elidedFieldValue renders the replacement for an oversized field value: a
// single-line head preview plus the byte count and where to read the rest.
//
// The result is never longer than the value it replaces: eliding must not make
// the block bigger. A rune-capped preview alone does not guarantee that — 120
// four-byte runes plus the marker outweigh a value only just over the byte
// threshold — so the preview is capped by bytes as well.
func elidedFieldValue(value, beadID string) string {
	marker := fmt.Sprintf("[%s elided — full text: kd show %s]", humanBytes(len(value)), beadID)
	const sep = " … "

	preview := value
	if i := strings.IndexByte(preview, '\n'); i >= 0 {
		preview = preview[:i]
	}
	preview = truncateRunes(preview, assignmentPreviewRunes)
	// Byte budget that keeps preview+sep+marker no longer than the original.
	if budget := len(value) - len(sep) - len(marker); len(preview) > budget {
		preview = truncateBytes(preview, budget)
	}
	preview = strings.TrimSpace(preview)

	// A value whose first line is blank yields an empty preview; emitting the
	// marker alone avoids a leading "… " with nothing before it.
	if preview == "" {
		return marker
	}
	return preview + sep + marker
}

// truncateBytes cuts s to at most n bytes without splitting a multi-byte rune.
// n <= 0 yields "".
func truncateBytes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	// Back off to a rune boundary. Invalid UTF-8 is left as-is: DecodeLastRune
	// reports width 1 for a bare continuation byte, so this always terminates.
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// truncateRunes cuts s to at most n runes, never splitting a multi-byte rune.
// n <= 0 yields "". Invalid UTF-8 bytes each count as one rune (RuneError),
// which is what the range-over-string loop below already does.
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}

// assignmentMaxComments caps how many trailing comments survive into the
// `## Assignment` block. `kd show` already caps its own display at the last 30
// (commentDisplayLimit), but the block wants far tighter: a respawned agent
// needs its assignment — title/status/role/Depends On — not the full comment
// history of its predecessors. The most-recent handful carry any live
// orientation (who dispatched it, the latest operator note); older ones are a
// `kd show` away.
const assignmentMaxComments = 3

// assignmentCommentMaxBytes is the ceiling for a single retained comment's
// rendered block. Above it, only a first-line preview + pointer survives
// (via elidedFieldValue), matching how oversized field values are handled.
//
// 240 is calibrated against the live corpus: a bridge dispatch/spawn notice
// renders at ~200B and is orientation-useful ("who spawned me, on what bead"),
// so it passes through intact; the bulk this elides is the agent `gb stop`
// wrap-up prose (median ~1.2KB, up to 3.5KB) the bridge re-injects as a comment
// even though its source `wrapup` field was just elided (ka-0000001998).
const assignmentCommentMaxBytes = 240

// commentIndent is the two-space indent `kd show` prefixes every comment line
// with (formatShowComment: "  [ts] author: …").
const commentIndent = "  "

// capAssignmentComments trims the `kd show` Comments section down to the last
// assignmentMaxComments entries, eliding any retained entry whose rendered block
// exceeds assignmentCommentMaxBytes to a first-line preview + pointer. The rest
// of the text — header, Fields, Depends On, Children, and the trailing Mentions
// sections — is returned byte-for-byte.
//
// On any structural surprise (no Comments section, no parseable entries) the
// text is returned unchanged, mirroring the field-elision fallbacks.
func capAssignmentComments(text, beadID string) string {
	const marker = "\nComments:\n"

	// Anchor the search AFTER the "Created By:" line. `kd show` emits the
	// Description and every Fields value BEFORE it, so a value there that
	// happens to contain a literal "Comments:" header can never be mistaken
	// for the real section, which always follows Created By / Depends On.
	searchFrom := 0
	if i := strings.Index(text, "\nCreated By:"); i >= 0 {
		searchFrom = i
	}
	rel := strings.Index(text[searchFrom:], marker)
	if rel < 0 {
		return text // no Comments section
	}
	bodyStart := searchFrom + rel + len(marker)

	// The Comments section runs until the next top-level section — Mentions,
	// Mentioned by, or a mentions-lookup-failed hint — or to EOF when comments
	// are the last section. Take the earliest such boundary at/after bodyStart.
	// A comment body that quotes one of these headers could cut early; that
	// only truncates prose we are already truncating, so the risk is benign.
	bodyEnd := len(text)
	for _, next := range []string{"\nMentions:\n", "\nMentioned by:\n", "\n(mentions lookup failed", "\n(mentioned-by lookup failed"} {
		if i := strings.Index(text[bodyStart:], next); i >= 0 && bodyStart+i < bodyEnd {
			bodyEnd = bodyStart + i
		}
	}

	entries := splitCommentEntries(text[bodyStart:bodyEnd])
	if len(entries) == 0 {
		return text
	}
	return text[:bodyStart] + renderCappedComments(entries, beadID) + text[bodyEnd:]
}

// splitCommentEntries breaks the rendered Comments-section body into individual
// comment blocks. Each block opens with a `  [YYYY-MM-DD HH:MM:SS]` timestamp
// line (formatShowComment's prefix, shared by the salvage-fold line) and runs
// until the next such line; continuation lines of a multi-line comment attach to
// the block they follow. Leading blank lines and the "… N older comments
// suppressed" footer `kd show` may append are dropped — the cap here supersedes
// that footer with its own pointer.
func splitCommentEntries(body string) []string {
	var entries []string
	var cur strings.Builder
	started := false
	flush := func() {
		if started {
			entries = append(entries, cur.String())
			cur.Reset()
			started = false
		}
	}
	for line := range strings.SplitSeq(body, "\n") {
		switch {
		case hasCommentTimestampPrefix(line):
			flush()
			started = true
			cur.WriteString(line)
		case isSuppressedFooter(line):
			// kd show's own truncation footer — never carry it forward.
		case started:
			cur.WriteByte('\n')
			cur.WriteString(line)
		}
	}
	flush()
	return entries
}

// renderCappedComments emits the retained tail of comment entries: a one-line
// "older elided" pointer when any were dropped, then the last
// assignmentMaxComments entries, each verbatim if small or reduced to a
// first-line preview + pointer (elidedFieldValue) when oversized. Every emitted
// block ends in a single newline, matching `kd show`'s per-comment layout.
func renderCappedComments(entries []string, beadID string) string {
	var sb strings.Builder
	if dropped := len(entries) - assignmentMaxComments; dropped > 0 {
		fmt.Fprintf(&sb, "  [%d older comment(s) elided — see: kd show %s]\n", dropped, beadID)
		entries = entries[dropped:]
	}
	for _, e := range entries {
		e = strings.TrimRight(e, "\n")
		if len(e) > assignmentCommentMaxBytes {
			// elidedFieldValue keeps the entry's own "[ts] author:" head as the
			// preview and appends the byte-count + pointer marker. It TrimSpaces
			// the preview, dropping the entry's leading "  " indent, so re-add it
			// to keep elided lines aligned with the verbatim comments around them.
			sb.WriteString(commentIndent)
			sb.WriteString(elidedFieldValue(e, beadID))
		} else {
			sb.WriteString(e)
		}
		sb.WriteByte('\n')
	}
	return sb.String()
}

// hasCommentTimestampPrefix reports whether line opens with the
// "  [2006-01-02 15:04:05" timestamp that formatShowComment (and the
// salvage-fold line) emits, marking the start of a comment entry. The closing
// "]" is intentionally NOT required so the fold line — which continues with a
// "–<ts>]" range — is still recognized as an entry start.
func hasCommentTimestampPrefix(line string) bool {
	// 'd' marks a digit position; every other byte is matched literally.
	const shape = "  [dddd-dd-dd dd:dd:dd"
	if len(line) < len(shape) {
		return false
	}
	for i := range len(shape) {
		if shape[i] == 'd' {
			if line[i] < '0' || line[i] > '9' {
				return false
			}
		} else if line[i] != shape[i] {
			return false
		}
	}
	return true
}

// isSuppressedFooter reports whether line is `kd show`'s own
// "  … N older comments suppressed …" truncation footer.
func isSuppressedFooter(line string) bool {
	return strings.HasPrefix(line, "  … ") && strings.Contains(line, "older comments suppressed")
}

// humanBytes formats a byte count for the elision marker.
func humanBytes(n int) string {
	if n < 1024 {
		return fmt.Sprintf("%dB", n)
	}
	return fmt.Sprintf("%.1fKB", float64(n)/1024)
}
