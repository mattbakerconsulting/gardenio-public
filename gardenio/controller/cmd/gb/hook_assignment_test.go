package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

// gardenerPrompt is a stand-in for the real ~17.7KB agent prompt carried in an
// agent config bead's `prompt:` field. It deliberately contains lines that MIMIC
// a `kd show` field-key line ("  ALIVE_AGENTS: jq -r ...") — those appear in the
// real gardener-bugfix prompt and are why the elision replaces a known key's
// exact rendered field line instead of parsing the text for field-shaped lines.
func gardenerPrompt() string {
	var sb strings.Builder
	sb.WriteString("You are the Gardenio gardener-bugfix gardener. Cheap ORCHESTRATOR, every 30m\n")
	sb.WriteString("  ALIVE_AGENTS: jq -r '.agents[] | select(.alive) | .name' /tmp/news.json\n")
	sb.WriteString("  CANDIDATES:   jq -r '[.[]|select(.mr_state==\"opened\")]' /tmp/mrs.json\n")
	for i := range 200 {
		fmt.Fprintf(&sb, "step %d: do the thing and report back to the operator\n", i)
	}
	return sb.String()
}

// showText renders a bead the way `kd show` does: a header block, then a
// "Fields:" section using the "  %-24s %s" layout, then the trailing sections
// (Depends On / Mentions) that only the TEXT output carries.
func showText(fields map[string]string, order []string) string {
	var sb strings.Builder
	sb.WriteString("ID:          gd-cap01\n")
	sb.WriteString("Title:       gardenio-gardener-bugfix-dispatch\n")
	sb.WriteString("Type:        agent\nKind:        config\nStatus:      open\n")
	sb.WriteString("Fields:\n")
	for _, k := range order {
		fmt.Fprintf(&sb, "  %-24s %s\n", k+":", fields[k])
	}
	sb.WriteString("Created By:  scheduler\n\nDepends On:\n  gd-task99 (assigned) [in_progress] Fix the thing\n")
	return sb.String()
}

func TestElidedFieldValue_KeepsPreviewAndPointer(t *testing.T) {
	got := elidedFieldValue(gardenerPrompt(), "gd-cap01")

	if strings.Contains(got, "\n") {
		t.Errorf("elided value must be single-line, got %q", got)
	}
	if !strings.Contains(got, "You are the Gardenio gardener-bugfix gardener") {
		t.Errorf("elided value lost its head preview: %q", got)
	}
	if !strings.Contains(got, "kd show gd-cap01") {
		t.Errorf("elided value lost the pointer to the full text: %q", got)
	}
	// The bulk must actually be gone.
	if strings.Contains(got, "step 199") {
		t.Errorf("elided value still carries the body: %q", got)
	}
}

func TestElideBulkyAssignmentFields_ElidesPromptKeepsSmallFields(t *testing.T) {
	prompt := gardenerPrompt()
	fields := map[string]string{
		"agent":                  "gardenio-gardener-bugfix-dispatch-minl",
		"role":                   "gardener",
		"schedule_terminal_goal": strings.Repeat("g", 148),
		"prompt":                 prompt,
	}
	order := []string{"agent", "prompt", "role", "schedule_terminal_goal"}
	text := showText(fields, order)

	got := elideAssignmentFieldValues(text, toAny(fields), "gd-cap01")

	if strings.Contains(got, "step 199") {
		t.Error("prompt body survived elision")
	}
	if len(got) >= len(text) {
		t.Errorf("expected shrink, before=%d after=%d", len(text), len(got))
	}
	// Small, orientation-useful fields must be untouched.
	for _, want := range []string{
		"gardenio-gardener-bugfix-dispatch-minl",
		strings.Repeat("g", 148),
		"role:",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("elision dropped a small field value %q", want)
		}
	}
	// The text-only trailing sections must survive — this is why the block is
	// built from `kd show` text and not rebuilt from --json.
	if !strings.Contains(got, "Depends On:") || !strings.Contains(got, "gd-task99") {
		t.Errorf("elision dropped the Depends On section:\n%s", got)
	}
	if !strings.Contains(got, "Title:       gardenio-gardener-bugfix-dispatch") {
		t.Error("elision damaged the header block")
	}
}

// A field value that also appears verbatim in the Description must not be
// rewritten there — kd show prints Description BEFORE the Fields section.
func TestElideBulkyAssignmentFields_DoesNotRewriteDescription(t *testing.T) {
	prompt := gardenerPrompt()
	body := "ID:          gd-cap01\nDescription: " + prompt + "\n" +
		fmt.Sprintf("Fields:\n  %-24s %s\n", "prompt:", prompt)

	got := elideAssignmentFieldValues(body, map[string]any{"prompt": prompt}, "gd-cap01")

	descEnd := strings.Index(got, "\nFields:\n")
	if descEnd < 0 {
		t.Fatalf("Fields section vanished:\n%s", got)
	}
	if !strings.Contains(got[:descEnd], "step 199") {
		t.Error("description was rewritten; only the Fields section may be elided")
	}
	if strings.Contains(got[descEnd:], "step 199") {
		t.Error("field value in the Fields section was not elided")
	}
}

// Robustness: when the value cannot be located the text must come back
// byte-identical rather than corrupted.
func TestElideBulkyAssignmentFields_UnmatchedValueLeavesTextIntact(t *testing.T) {
	text := showText(map[string]string{"role": "gardener"}, []string{"role"})

	got := elideAssignmentFieldValues(text, map[string]any{
		"prompt": strings.Repeat("not-in-the-text ", 100),
	}, "gd-cap01")

	if got != text {
		t.Errorf("unmatched value must leave text unchanged\nbefore:\n%s\nafter:\n%s", text, got)
	}
}

// Non-string values (agent beads carry numeric context_tokens / live_turns) must
// never be touched: they render through a different formatting path.
func TestElideBulkyAssignmentFields_IgnoresNonStringValues(t *testing.T) {
	text := showText(map[string]string{"context_tokens": "60910"}, []string{"context_tokens"})

	got := elideAssignmentFieldValues(text, map[string]any{
		"context_tokens": float64(60910),
	}, "gd-cap01")

	if got != text {
		t.Error("numeric field must pass through untouched")
	}
	if !strings.Contains(got, "60910") {
		t.Error("numeric field value was lost")
	}
}

func TestElideBulkyAssignmentFields_ThresholdBoundary(t *testing.T) {
	atLimit := strings.Repeat("a", assignmentFieldMaxBytes)
	overLimit := strings.Repeat("b", assignmentFieldMaxBytes+1)
	fields := map[string]string{"at": atLimit, "over": overLimit}
	text := showText(fields, []string{"at", "over"})

	got := elideAssignmentFieldValues(text, toAny(fields), "gd-cap01")

	if !strings.Contains(got, atLimit) {
		t.Errorf("value exactly at the %dB limit must be kept", assignmentFieldMaxBytes)
	}
	if strings.Contains(got, overLimit) {
		t.Errorf("value over the %dB limit must be elided", assignmentFieldMaxBytes)
	}
}

// The whole point of the change: a gardener-sized assignment block collapses from
// multi-KB to a small one.
func TestElideBulkyAssignmentFields_GardenerBlockShrinksDramatically(t *testing.T) {
	prompt := gardenerPrompt()
	fields := map[string]string{"prompt": prompt, "wrapup": strings.Repeat("w", 2810)}
	text := showText(fields, []string{"prompt", "wrapup"})

	got := elideAssignmentFieldValues(text, toAny(fields), "gd-cap01")

	if len(got) > 1024 {
		t.Errorf("elided block still large: %dB (was %dB)", len(got), len(text))
	}
	t.Logf("assignment block: %dB -> %dB (%.1f%% saved)",
		len(text), len(got), 100*(1-float64(len(got))/float64(len(text))))
}

func TestTruncateRunes_DoesNotSplitMultibyte(t *testing.T) {
	got := truncateRunes("héllo—wörld", 4)
	if got != "héll" {
		t.Errorf("truncateRunes = %q, want %q", got, "héll")
	}
	// Truncating an all-multibyte string must stay valid UTF-8.
	if got := truncateRunes(strings.Repeat("—", 10), 3); got != "———" {
		t.Errorf("truncateRunes on multibyte = %q", got)
	}
}

// Real beads in the live corpus carry field values that are not valid UTF-8, so
// the preview must degrade gracefully rather than panic or emit a mangled tail.
func TestElideBulkyAssignmentFields_InvalidUTF8Value(t *testing.T) {
	bad := "head \xff\xfe invalid " + strings.Repeat("\xff", assignmentFieldMaxBytes)
	fields := map[string]string{"prompt": bad}
	text := showText(fields, []string{"prompt"})

	got := elideAssignmentFieldValues(text, toAny(fields), "gd-cap01")

	if strings.Contains(got, bad) {
		t.Error("oversized invalid-UTF8 value was not elided")
	}
	if !strings.Contains(got, "kd show gd-cap01") {
		t.Errorf("elision marker missing: %q", got)
	}
}

// REGRESSION: one field's value can CONTAIN another's. A chain-spawned gardener
// gets "Terminal goal: <goal>" written into its prompt (bridge/
// gardener_chain_spawn.go:432) while schedule_terminal_goal stores the same text.
// Value-anchored replacement elided the goal first, splicing the marker inside
// the prompt's value so the prompt no longer matched and its whole body
// survived — and because Go randomizes map order, it broke only ~19% of runs.
func TestElideBulkyAssignmentFields_NestedValuesAreDeterministic(t *testing.T) {
	goal := "STANDING gardener. " + strings.Repeat("Drive every open child of gd-EPIC to closure. ", 14)
	if len(goal) <= assignmentFieldMaxBytes {
		t.Fatalf("setup: goal must be oversized, got %dB", len(goal))
	}
	prompt := "You are the Gardenio gardener-bugfix gardener.\n" +
		strings.Repeat("step: do the thing and report back\n", 400) +
		"\n## Loop exit\n\nTerminal goal: " + goal + "\n" +
		strings.Repeat("tail line of the playbook\n", 100)

	fields := map[string]string{
		"agent":                  "gardenio-gardener-bugfix-dispatch-minl",
		"prompt":                 prompt,
		"schedule_terminal_goal": goal,
	}
	order := []string{"agent", "prompt", "schedule_terminal_goal"}
	text := showText(fields, order)

	// Many iterations: map iteration order varies per run, so a single pass can
	// pass by luck.
	first := ""
	for i := range 300 {
		got := elideAssignmentFieldValues(text, toAny(fields), "gd-cap01")
		if strings.Contains(got, "step: do the thing") {
			t.Fatalf("run %d: prompt body survived elision (%dB -> %dB)", i, len(text), len(got))
		}
		if strings.Contains(got, "Terminal goal: STANDING gardener. … [") {
			t.Fatalf("run %d: marker spliced INSIDE the prompt value", i)
		}
		if i == 0 {
			first = got
		} else if got != first {
			t.Fatalf("run %d: output is not deterministic across map order", i)
		}
	}
	// Both oversized fields must be independently elided.
	if n := strings.Count(first, "elided — full text:"); n != 2 {
		t.Errorf("want 2 elision markers, got %d:\n%s", n, first)
	}
}

// REGRESSION: an agent bead's Description IS its spawn prompt (beadsapi
// SpawnAgent sets req.Description = customPrompt), so a prompt that shows a
// `kd show` example puts a literal "Fields:" line into the Description. That
// mis-split the head/tail and rewrote the description instead of the field.
func TestElideBulkyAssignmentFields_FieldsMarkerInDescription(t *testing.T) {
	goal := strings.Repeat("Close every open child of gd-EPIC and report. ", 14)
	prompt := "Read your bead with `kd show $BOAT_AGENT_BEAD_ID`; it looks like:\n" +
		"\nFields:\n  role:  gardener\n\n" +
		"Then work the queue.\n\n## Loop exit\n\nTerminal goal: " + goal + "\n"

	fields := map[string]string{"prompt": prompt, "schedule_terminal_goal": goal}
	text := "ID:          gd-cap01\nTitle:       cap\nDescription: " + prompt + "\n" +
		showFieldsOnly(fields, []string{"prompt", "schedule_terminal_goal"})

	got := elideAssignmentFieldValues(text, toAny(fields), "gd-cap01")

	realFields := strings.LastIndex(got, "\nFields:\n")
	if realFields < 0 {
		t.Fatalf("Fields section vanished:\n%s", got)
	}
	if desc := got[:realFields]; strings.Contains(desc, "elided — full text:") {
		t.Error("description was rewritten; only a real field line may be elided")
	}
	if tail := got[realFields:]; strings.Contains(tail, "schedule_terminal_goal:      "+goal) {
		t.Error("the real schedule_terminal_goal field line was not elided")
	}
}

// A bead with no Fields section at all must come back untouched: there is no
// field line to anchor to, so any value match would be in the Description.
func TestElideBulkyAssignmentFields_NoFieldsSection(t *testing.T) {
	v := strings.Repeat("description body line\n", 60)
	text := "ID:          gd-cap01\nDescription: " + v + "\nCreated By:  x\n"

	if got := elideAssignmentFieldValues(text, map[string]any{"prompt": v}, "gd-cap01"); got != text {
		t.Errorf("text without a Fields section must be unchanged:\n%s", got)
	}
}

// Two distinct keys holding the IDENTICAL oversized value must each be elided.
func TestElideBulkyAssignmentFields_DuplicateIdenticalValues(t *testing.T) {
	v := strings.Repeat("identical body line\n", 60)
	fields := map[string]string{"prompt": v, "wrapup": v}
	text := showText(fields, []string{"prompt", "wrapup"})

	got := elideAssignmentFieldValues(text, toAny(fields), "gd-cap01")

	if strings.Contains(got, "identical body line\nidentical body line") {
		t.Errorf("bulk survived for a duplicated value:\n%s", got)
	}
	if n := strings.Count(got, "elided — full text:"); n != 2 {
		t.Errorf("want 2 elision markers, got %d:\n%s", n, got)
	}
}

func TestTruncateRunes_Boundaries(t *testing.T) {
	cases := []struct {
		s    string
		n    int
		want string
	}{
		{"", 0, ""},
		{"abc", 0, ""},
		{"abc", -1, ""},
		{"abc", 3, "abc"},
		{"abc", 4, "abc"},
		{"héllo", 2, "hé"},
		{"a\xffb", 2, "a\xff"},
		{"\xff\xfe\xfd", 2, "\xff\xfe"},
		{"\xe4\xb8", 1, "\xe4"}, // truncated 3-byte sequence
	}
	for _, c := range cases {
		if got := truncateRunes(c.s, c.n); got != c.want {
			t.Errorf("truncateRunes(%q, %d) = %q, want %q", c.s, c.n, got, c.want)
		}
	}
}

// A value whose first line is blank must not render a marker with a dangling
// leading "… ".
func TestElidedFieldValue_EmptyFirstLinePreview(t *testing.T) {
	got := elidedFieldValue("\n\n"+strings.Repeat("body\n", 200), "gd-cap01")

	if strings.HasPrefix(got, " ") || strings.HasPrefix(got, "…") {
		t.Errorf("empty preview must emit the marker alone, got %q", got)
	}
	if !strings.HasPrefix(got, "[") {
		t.Errorf("want marker-only rendering, got %q", got)
	}
}

// stubKD replaces the `kd` shell-out for the duration of a test. handler
// receives the args passed to kd and returns the stdout it should produce.
func stubKD(t *testing.T, handler func(args []string) ([]byte, error)) {
	t.Helper()
	prev := runKD
	runKD = func(_ context.Context, args ...string) ([]byte, error) { return handler(args) }
	t.Cleanup(func() { runKD = prev })
}

func TestRenderAssignmentBlock_ElidesViaBothKDCalls(t *testing.T) {
	prompt := gardenerPrompt()
	fields := map[string]string{"role": "gardener", "prompt": prompt}
	text := showText(fields, []string{"prompt", "role"})

	var calls [][]string
	stubKD(t, func(args []string) ([]byte, error) {
		calls = append(calls, args)
		if slices.Contains(args, "--jq") {
			b, _ := json.Marshal(fields)
			return b, nil
		}
		return []byte(text), nil
	})

	got := renderAssignmentBlock(context.Background(), "gd-cap01")

	if strings.Contains(got, "step 199") {
		t.Error("prompt body survived elision")
	}
	if !strings.Contains(got, "elided — full text: kd show gd-cap01") {
		t.Errorf("missing elision marker:\n%s", got)
	}
	if !strings.Contains(got, "Depends On:") {
		t.Error("dropped the Depends On section")
	}
	if len(calls) != 2 {
		t.Errorf("want 2 kd calls (text + --jq fields), got %d: %v", len(calls), calls)
	}
}

// A failing `kd show` must yield "" so the caller prints no block at all —
// matching the pre-change behavior for an unknown bead.
func TestRenderAssignmentBlock_ShowFailureYieldsEmpty(t *testing.T) {
	stubKD(t, func([]string) ([]byte, error) { return nil, errors.New("boom") })

	if got := renderAssignmentBlock(context.Background(), "gd-nope"); got != "" {
		t.Errorf("want empty string on kd failure, got %q", got)
	}
}

// If the text renders but the structured field fetch fails, the block must fall
// back to the raw text rather than being dropped or corrupted.
func TestRenderAssignmentBlock_FieldsFetchFailureFallsBackToRawText(t *testing.T) {
	text := showText(map[string]string{"role": "gardener"}, []string{"role"})

	stubKD(t, func(args []string) ([]byte, error) {
		if slices.Contains(args, "--jq") {
			return nil, errors.New("daemon down")
		}
		return []byte(text), nil
	})

	if got := renderAssignmentBlock(context.Background(), "gd-cap01"); got != text {
		t.Errorf("want raw text fallback, got:\n%s", got)
	}
}

// Malformed field JSON must also fall back to the raw text.
func TestRenderAssignmentBlock_MalformedFieldsJSONFallsBack(t *testing.T) {
	text := showText(map[string]string{"role": "gardener"}, []string{"role"})

	stubKD(t, func(args []string) ([]byte, error) {
		if slices.Contains(args, "--jq") {
			return []byte("{not json"), nil
		}
		return []byte(text), nil
	})

	if got := renderAssignmentBlock(context.Background(), "gd-cap01"); got != text {
		t.Errorf("want raw text fallback on bad JSON, got:\n%s", got)
	}
}

// Eliding must never make the block BIGGER. A rune-capped preview alone does
// not guarantee that: 120 four-byte runes plus the marker outweigh a value only
// just over the byte threshold (521B -> 530B before the byte cap was added).
func TestElidedFieldValue_NeverGrowsTheValue(t *testing.T) {
	cases := []string{
		strings.Repeat("\U0001F600", 120) + "\n" + strings.Repeat("x", 40), // wide first line, barely oversized
		strings.Repeat("\U0001F600", 200),
		strings.Repeat("a", assignmentFieldMaxBytes+1),
		strings.Repeat("é", 300),
		"\n\n" + strings.Repeat("b", assignmentFieldMaxBytes+1), // empty preview
	}
	for _, v := range cases {
		got := elidedFieldValue(v, "gd-cap01")
		if len(got) > len(v) {
			t.Errorf("elision grew the value: %dB -> %dB", len(v), len(got))
		}
		if strings.Contains(got, "\n") {
			t.Errorf("elided value must stay single-line: %q", got)
		}
		// A valid-UTF8 input must not be cut mid-rune.
		if utf8.ValidString(v) && !utf8.ValidString(got) {
			t.Errorf("elision produced invalid UTF-8 from valid input: %q", got)
		}
	}
}

func TestTruncateBytes_RespectsRuneBoundaries(t *testing.T) {
	cases := []struct {
		s    string
		n    int
		want string
	}{
		{"abc", 0, ""},
		{"abc", -1, ""},
		{"abc", 2, "ab"},
		{"abc", 9, "abc"},
		{"héllo", 2, "h"},      // would split é (bytes 1-2)
		{"héllo", 3, "hé"},     // exact boundary
		{"\U0001F600x", 3, ""}, // 4-byte rune cannot fit in 3
	}
	for _, c := range cases {
		if got := truncateBytes(c.s, c.n); got != c.want {
			t.Errorf("truncateBytes(%q, %d) = %q, want %q", c.s, c.n, got, c.want)
		}
	}
	// Must terminate on invalid UTF-8 (bare continuation bytes).
	if got := truncateBytes("\x80\x80\x80", 2); len(got) > 2 {
		t.Errorf("truncateBytes on invalid UTF-8 = %q", got)
	}
}

// comment renders one comment the way `kd show` (formatShowComment) does:
// "  [ts] author: text", where text may itself contain newlines.
func comment(ts, author, text string) string {
	return fmt.Sprintf("  [%s] %s: %s", ts, author, text)
}

// showTextWithComments renders a minimal `kd show` bead carrying a Depends On
// section, a Comments section built from the given entries, and the trailing
// Mentions sections — the exact surround capAssignmentComments must preserve.
func showTextWithComments(comments []string, withMentions bool) string {
	var sb strings.Builder
	sb.WriteString("ID:          gd-cap01\nTitle:       gardenio-crew\n")
	sb.WriteString("Type:        agent\nKind:        config\nStatus:      open\n")
	sb.WriteString("Created By:  scheduler\nCreated At:  2026-08-03 09:00:00\n\n")
	sb.WriteString("Depends On:\n  gd-task99 (assigned) [in_progress] Fix the thing\n")
	sb.WriteString("\nComments:\n")
	for _, c := range comments {
		sb.WriteString(c)
		sb.WriteByte('\n')
	}
	if withMentions {
		sb.WriteString("\nMentions:\n  untyped: 1 (gd-task99)\n")
		sb.WriteString("\nMentioned by:\n  untyped: 2 (gd-a, gd-b)\n")
	}
	return sb.String()
}

// A wrap-up-sized comment: a first line plus multi-KB of prose, the dominant
// Comments-section byte source the fix targets.
func wrapupComment(ts, author string) string {
	return comment(ts, author, "gb stop wrap-up:\n\nTLDR: did the thing\n"+
		strings.Repeat("Accomplishment line describing what was merged and closed.\n", 60))
}

func TestCapAssignmentComments_KeepsLastNElidesOversizedPreservesSurround(t *testing.T) {
	comments := []string{
		comment("2026-08-03 09:01:00", "gardener", "dispatched crew on gd-task99"),
		wrapupComment("2026-08-03 09:10:00", "crew-alpha"),
		comment("2026-08-03 09:20:00", "gardener", "second dispatch note, short and useful"),
		wrapupComment("2026-08-03 09:30:00", "crew-beta"),
	}
	text := showTextWithComments(comments, true)

	got := capAssignmentComments(text, "gd-cap01")

	// The two multi-KB wrap-up bodies must be gone.
	if strings.Contains(got, "Accomplishment line describing") {
		t.Error("wrap-up body survived the comment cap")
	}
	// The oldest comment (beyond the last N=3) must be dropped with a pointer.
	if strings.Contains(got, "dispatched crew on gd-task99") {
		t.Error("oldest comment past the cap was retained")
	}
	if !strings.Contains(got, "older comment(s) elided — see: kd show gd-cap01") {
		t.Errorf("missing dropped-comment pointer:\n%s", got)
	}
	// The retained oversized wrap-ups keep a first-line preview + pointer.
	if strings.Count(got, "elided — full text: kd show gd-cap01") != 2 {
		t.Errorf("want 2 elided wrap-up previews:\n%s", got)
	}
	if !strings.Contains(got, "gb stop wrap-up:") {
		t.Error("elided wrap-up lost its first-line preview")
	}
	// Elided comment lines keep the kd show two-space indent so they align with
	// the verbatim comments around them.
	if !strings.Contains(got, "\n  [2026-08-03 09:30:00] crew-beta: gb stop wrap-up:") {
		t.Errorf("elided comment lost its leading indent:\n%s", got)
	}
	// A small retained comment stays verbatim.
	if !strings.Contains(got, "second dispatch note, short and useful") {
		t.Error("small retained comment was damaged")
	}
	// The surrounding sections must be byte-preserved.
	for _, want := range []string{
		"Depends On:\n  gd-task99 (assigned)",
		"\nMentions:\n  untyped: 1 (gd-task99)",
		"\nMentioned by:\n  untyped: 2 (gd-a, gd-b)",
		"Title:       gardenio-crew",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("cap damaged surrounding text, missing %q:\n%s", want, got)
		}
	}
	if len(got) >= len(text) {
		t.Errorf("expected shrink, before=%d after=%d", len(text), len(got))
	}
}

// Comments as the LAST section (no Mentions) must still cap cleanly and end in a
// single trailing newline like `kd show` does.
func TestCapAssignmentComments_LastSectionNoMentions(t *testing.T) {
	comments := []string{
		comment("2026-08-03 09:01:00", "gardener", "note one"),
		wrapupComment("2026-08-03 09:10:00", "crew"),
	}
	text := showTextWithComments(comments, false)

	got := capAssignmentComments(text, "gd-cap01")

	if strings.Contains(got, "Accomplishment line describing") {
		t.Error("wrap-up body survived when comments were the last section")
	}
	if !strings.HasSuffix(got, "\n") {
		t.Errorf("capped block must end in a newline:\n%q", got[max(0, len(got)-40):])
	}
	if !strings.Contains(got, "note one") {
		t.Error("small comment dropped when it should be retained")
	}
}

// Fewer than the cap: nothing is dropped, no pointer, small comments untouched.
func TestCapAssignmentComments_UnderCapUnchangedSmallComments(t *testing.T) {
	comments := []string{
		comment("2026-08-03 09:01:00", "gardener", "alpha note"),
		comment("2026-08-03 09:02:00", "gardener", "beta note"),
	}
	text := showTextWithComments(comments, true)

	got := capAssignmentComments(text, "gd-cap01")

	if got != text {
		t.Errorf("under-cap all-small comments must be unchanged\nbefore:\n%s\nafter:\n%s", text, got)
	}
}

// A bead with no Comments section must come back byte-identical.
func TestCapAssignmentComments_NoCommentsSection(t *testing.T) {
	text := showText(map[string]string{"role": "crew"}, []string{"role"})

	if got := capAssignmentComments(text, "gd-cap01"); got != text {
		t.Errorf("text without a Comments section must be unchanged:\n%s", got)
	}
}

// A field value (or Description) that quotes a literal "Comments:" header before
// the Created By line must NOT be mistaken for the real section.
func TestCapAssignmentComments_DoesNotMatchCommentsHeaderInField(t *testing.T) {
	// The field body embeds a fake "\nComments:\n" plus a timestamp-shaped line.
	fake := "example output:\nComments:\n  [2026-08-03 00:00:00] ghost: not a real comment\n"
	var sb strings.Builder
	sb.WriteString("ID:          gd-cap01\nDescription: ")
	sb.WriteString(fake)
	sb.WriteString("\nFields:\n")
	fmt.Fprintf(&sb, "  %-24s %s\n", "prompt:", fake)
	sb.WriteString("Created By:  scheduler\n\nComments:\n")
	sb.WriteString(comment("2026-08-03 09:10:00", "crew", "real comment\n"+strings.Repeat("bulk\n", 100)))
	sb.WriteByte('\n')
	text := sb.String()

	got := capAssignmentComments(text, "gd-cap01")

	// The ghost line before Created By must survive untouched (not elided).
	if !strings.Contains(got, "ghost: not a real comment") {
		t.Error("a Comments header quoted in a field/description was wrongly treated as the section")
	}
	// The REAL comment's bulk must be elided.
	if strings.Contains(got, "bulk\nbulk") {
		t.Errorf("the real comment body was not capped:\n%s", got)
	}
	if !strings.Contains(got, "elided — full text: kd show gd-cap01") {
		t.Errorf("real oversized comment lost its elision marker:\n%s", got)
	}
}

// The salvage-fold synthetic line opens with a timestamp range; it must be
// recognized as its own entry boundary, not swallowed into a neighbor.
func TestCapAssignmentComments_SalvageFoldLineIsAnEntry(t *testing.T) {
	comments := []string{
		"  [2026-08-03 09:00:00–2026-08-03 09:05:00] 3× involuntary_termination salvage notices (kd show --all-comments to expand)",
		comment("2026-08-03 09:10:00", "gardener", "note A"),
		comment("2026-08-03 09:20:00", "gardener", "note B"),
		comment("2026-08-03 09:30:00", "gardener", "note C"),
	}
	text := showTextWithComments(comments, true)

	got := capAssignmentComments(text, "gd-cap01")

	// 4 entries, cap 3 -> the fold line is the one dropped, leaving a pointer.
	if strings.Contains(got, "involuntary_termination salvage") {
		t.Error("fold line was not treated as a droppable entry")
	}
	if !strings.Contains(got, "1 older comment(s) elided") {
		t.Errorf("want exactly 1 dropped entry:\n%s", got)
	}
	for _, w := range []string{"note A", "note B", "note C"} {
		if !strings.Contains(got, w) {
			t.Errorf("retained comment %q missing:\n%s", w, got)
		}
	}
}

// kd show's own "… N older comments suppressed" footer must never ride into the
// capped block.
func TestCapAssignmentComments_DropsSuppressedFooter(t *testing.T) {
	comments := []string{
		comment("2026-08-03 09:10:00", "gardener", "note A"),
		comment("2026-08-03 09:20:00", "gardener", "note B"),
		comment("2026-08-03 09:30:00", "gardener", "note C"),
		"  … 7 older comments suppressed. Use --all-comments to view full history.",
	}
	text := showTextWithComments(comments, true)

	got := capAssignmentComments(text, "gd-cap01")

	if strings.Contains(got, "older comments suppressed") {
		t.Errorf("kd show suppressed footer leaked into the capped block:\n%s", got)
	}
}

func TestHasCommentTimestampPrefix(t *testing.T) {
	cases := []struct {
		line string
		want bool
	}{
		{"  [2026-08-03 09:01:17] author: hi", true},
		{"  [2026-08-03 09:00:00–2026-08-03 09:05:00] 3× notices", true}, // fold line
		{"  [2026-08-03 09:01", false},                                   // too short (truncated timestamp)
		{"  continuation line of a comment body", false},
		{"  [not-a-timestamp] x", false},
		{" [2026-08-03 09:01:17] author: hi", false}, // one-space indent
		{"", false},
	}
	for _, c := range cases {
		if got := hasCommentTimestampPrefix(c.line); got != c.want {
			t.Errorf("hasCommentTimestampPrefix(%q) = %v, want %v", c.line, got, c.want)
		}
	}
}

// Integration: renderAssignmentBlock must cap comments AND elide fields in one
// pass, over the two kd shell-outs, while preserving Depends On.
func TestRenderAssignmentBlock_CapsCommentsAndElidesFields(t *testing.T) {
	fields := map[string]string{"role": "crew", "prompt": gardenerPrompt()}
	comments := []string{
		comment("2026-08-03 09:01:00", "gardener", "dispatched crew"),
		wrapupComment("2026-08-03 09:10:00", "crew"),
	}
	// Splice a Comments + Mentions tail onto the field-bearing showText.
	base := showText(fields, []string{"prompt", "role"})
	text := base + "\nComments:\n" + comments[0] + "\n" + comments[1] + "\n" +
		"\nMentions:\n  untyped: 1 (gd-task99)\n"

	stubKD(t, func(args []string) ([]byte, error) {
		if slices.Contains(args, "--jq") {
			b, _ := json.Marshal(fields)
			return b, nil
		}
		return []byte(text), nil
	})

	got := renderAssignmentBlock(context.Background(), "gd-cap01")

	if strings.Contains(got, "step 199") {
		t.Error("prompt field body survived elision")
	}
	if strings.Contains(got, "Accomplishment line describing") {
		t.Error("wrap-up comment body survived the cap")
	}
	if !strings.Contains(got, "Depends On:") || !strings.Contains(got, "gd-task99") {
		t.Errorf("dropped the Depends On section:\n%s", got)
	}
	if !strings.Contains(got, "\nMentions:\n") {
		t.Error("dropped the Mentions section")
	}
}

func showFieldsOnly(fields map[string]string, order []string) string {
	var sb strings.Builder
	sb.WriteString("Fields:\n")
	for _, k := range order {
		fmt.Fprintf(&sb, "  %-24s %s\n", k+":", fields[k])
	}
	sb.WriteString("Created By:  scheduler\n")
	return sb.String()
}

func toAny(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
