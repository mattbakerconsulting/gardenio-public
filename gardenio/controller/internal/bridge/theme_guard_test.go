package bridge

// theme_guard_test.go is the Phase 4 regression guard (gd-yIFqlezCXc, epic
// gd-XFizxz9y3Y / gd-Z3Sgg8W5Ih). Slack output must reach its glyphs through
// the selected Theme (theme.go) so a Business channel never shows a Garden
// emoji and vice versa. A bare ":rocket:" or "🚀" string literal in non-test
// bridge code bypasses that, so this test parses every non-test file in the
// package and fails on any such literal that is not in the allowlist below.
//
// The allowlist is a ratchet, per file, in both directions:
//   - a file gaining a literal fails ("themed it? use Theme.Semantic/Tool/...;
//     genuinely chrome? add it to the allowlist with a reason");
//   - a file losing one also fails until its Max is lowered, so a themed
//     surface can never silently regress to a bare literal inside the headroom.
//
// Entries are classified: guardVocabulary (the glyph tables themselves),
// guardParse (literals that are matched against rather than emitted),
// guardChrome (deliberately unthemed, with the reason) and guardDebt (a
// Slack surface still to be wired through themeFor; shrink these to zero).

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

type guardKind string

const (
	guardVocabulary guardKind = "vocabulary"
	guardParse      guardKind = "parse"
	guardChrome     guardKind = "chrome"
	guardDebt       guardKind = "debt"
)

type guardEntry struct {
	Max  int
	Kind guardKind
	Why  string
}

// emojiLiteralAllowlist maps a bridge source file to the number of
// shortcode/emoji tokens its string literals may contain. Keys are base names
// in this package directory.
var emojiLiteralAllowlist = map[string]guardEntry{
	"theme.go":                         {63, guardVocabulary, "the Garden/Business glyph tables themselves"},
	"bot_agents.go":                    {10, guardParse, "legacyOutcomeShortcodes: retired shortcodes matched and mapped to a lifecycle state, never emitted"},
	"bot_message_subtype.go":           {7, guardParse, "legacy shortcode prefixes matched when classifying already-posted messages, never emitted"},
	"spawn_vector.go":                  {8, guardParse, "spawnIcon* are theme-neutral cache keys; spawnVectorGlyph swaps in the channel theme's glyph before posting"},
	"gardener_epic_reaper.go":          {1, guardChrome, "operator-alert chrome, left unthemed by the gardenio/plans/slack-greenhouse-alert-triage.md triage"},
	"gardener_gate_guard.go":           {1, guardChrome, "bead comment, CLI output or agent prompt text, not a Slack surface (gardenio/plans/slack-greenhouse-alert-triage.md)"},
	"gardener_liveness.go":             {2, guardChrome, "operator-alert chrome, left unthemed by the gardenio/plans/slack-greenhouse-alert-triage.md triage"},
	"gardener_pouring_stall.go":        {1, guardChrome, "operator-alert chrome, left unthemed by the gardenio/plans/slack-greenhouse-alert-triage.md triage"},
	"closed_enabled_drain.go":          {1, guardChrome, "operator-alert chrome, left unthemed by the gardenio/plans/slack-greenhouse-alert-triage.md triage"},
	"dark_agent_budget.go":             {1, guardChrome, "bead comment, CLI output or agent prompt text, not a Slack surface (gardenio/plans/slack-greenhouse-alert-triage.md)"},
	"escalation_operator_dm.go":        {1, guardChrome, "channel-less operator DM, so there is no channel theme to resolve (operator chrome)"},
	"escalation_reaper.go":             {1, guardChrome, "bead comment written by a reaper/flagger, not a Slack surface"},
	"escalation_review_flag.go":        {1, guardChrome, "bead comment written by a reaper/flagger, not a Slack surface"},
	"gitlab_mention_catchup.go":        {1, guardChrome, "Jira/GitLab comment or description, another system's markup, not a Slack surface (gardenio/plans/slack-greenhouse-alert-triage.md)"},
	"gitlab_mentions.go":               {2, guardChrome, "Jira/GitLab comment or description, another system's markup, not a Slack surface (gardenio/plans/slack-greenhouse-alert-triage.md)"},
	"gitlab_mr_schema_flag.go":         {1, guardChrome, "Jira/GitLab comment or description, another system's markup, not a Slack surface (gardenio/plans/slack-greenhouse-alert-triage.md)"},
	"gitlab_mrdesc.go":                 {6, guardChrome, "Jira/GitLab comment or description, another system's markup, not a Slack surface (gardenio/plans/slack-greenhouse-alert-triage.md)"},
	"gitlab_sync.go":                   {3, guardChrome, "Jira/GitLab comment or description, another system's markup, not a Slack surface (gardenio/plans/slack-greenhouse-alert-triage.md)"},
	"gitlab_upstream_acceptance.go":    {2, guardChrome, "Jira/GitLab comment or description, another system's markup, not a Slack surface (gardenio/plans/slack-greenhouse-alert-triage.md)"},
	"jira_completion_ack.go":           {4, guardChrome, "Jira/GitLab comment or description, another system's markup, not a Slack surface (gardenio/plans/slack-greenhouse-alert-triage.md)"},
	"jira_spawn_ack.go":                {1, guardChrome, "Jira/GitLab comment or description, another system's markup, not a Slack surface (gardenio/plans/slack-greenhouse-alert-triage.md)"},
	"jira_status_transition.go":        {1, guardChrome, "Jira/GitLab comment or description, another system's markup, not a Slack surface (gardenio/plans/slack-greenhouse-alert-triage.md)"},
	"release_drought.go":               {3, guardChrome, "operator-alert chrome, left unthemed by the gardenio/plans/slack-greenhouse-alert-triage.md triage"},
	"repoless_gitpath_guard.go":        {1, guardChrome, "bead comment, CLI output or agent prompt text, not a Slack surface (gardenio/plans/slack-greenhouse-alert-triage.md)"},
	"sla_nudge_notify.go":              {2, guardChrome, "posts to the operator escalation queue, not a project channel (gardenio/plans/slack-greenhouse-alert-triage.md)"},
	"stale_revival_review_escalate.go": {1, guardChrome, "operator-alert chrome, left unthemed by the gardenio/plans/slack-greenhouse-alert-triage.md triage"},
	"thread_task_autoclose.go":         {2, guardChrome, "bead-comment instruction naming the stop reaction the user adds, not an emitted glyph"},
	"trigger_check_liveness.go":        {1, guardChrome, "operator-alert chrome, left unthemed by the gardenio/plans/slack-greenhouse-alert-triage.md triage"},
	"trigger_credential_wedge.go":      {1, guardChrome, "operator-alert chrome, left unthemed by the gardenio/plans/slack-greenhouse-alert-triage.md triage"},
	"trigger_dark_lane.go":             {3, guardChrome, "operator-alert chrome, left unthemed by the gardenio/plans/slack-greenhouse-alert-triage.md triage"},
	"uncovered_open_lane_rollup.go":    {1, guardChrome, "operator-alert chrome, left unthemed by the gardenio/plans/slack-greenhouse-alert-triage.md triage"},
	"bot.go":                           {5, guardDebt, "still carries bare shortcodes/emoji: wire through themeFor(channel) or record why it is chrome (gd-Z3Sgg8W5Ih follow-up)"},
	"bot_agent_death.go":               {3, guardDebt, "still carries bare shortcodes/emoji: wire through themeFor(channel) or record why it is chrome (gd-Z3Sgg8W5Ih follow-up)"},
	"bot_agent_kill.go":                {1, guardDebt, "still carries bare shortcodes/emoji: wire through themeFor(channel) or record why it is chrome (gd-Z3Sgg8W5Ih follow-up)"},
	"bot_beadactivity.go":              {4, guardDebt, "still carries bare shortcodes/emoji: wire through themeFor(channel) or record why it is chrome (gd-Z3Sgg8W5Ih follow-up)"},
	"bot_channel_invite.go":            {2, guardDebt, "still carries bare shortcodes/emoji: wire through themeFor(channel) or record why it is chrome (gd-Z3Sgg8W5Ih follow-up)"},
	"bot_concierge.go":                 {4, guardDebt, "still carries bare shortcodes/emoji: wire through themeFor(channel) or record why it is chrome (gd-Z3Sgg8W5Ih follow-up)"},
	"bot_decisions.go":                 {3, guardDebt, "still carries bare shortcodes/emoji: wire through themeFor(channel) or record why it is chrome (gd-Z3Sgg8W5Ih follow-up)"},
	"bot_decisions_dm.go":              {1, guardDebt, "still carries bare shortcodes/emoji: wire through themeFor(channel) or record why it is chrome (gd-Z3Sgg8W5Ih follow-up)"},
	"bot_decisions_permission.go":      {2, guardDebt, "still carries bare shortcodes/emoji: wire through themeFor(channel) or record why it is chrome (gd-Z3Sgg8W5Ih follow-up)"},
	"bot_escalations.go":               {7, guardDebt, "still carries bare shortcodes/emoji: wire through themeFor(channel) or record why it is chrome (gd-Z3Sgg8W5Ih follow-up)"},
	"bot_human_survey.go":              {6, guardDebt, "still carries bare shortcodes/emoji: wire through themeFor(channel) or record why it is chrome (gd-Z3Sgg8W5Ih follow-up)"},
	"bot_human_survey_manual.go":       {4, guardDebt, "still carries bare shortcodes/emoji: wire through themeFor(channel) or record why it is chrome (gd-Z3Sgg8W5Ih follow-up)"},
	"bot_mention_summarize.go":         {2, guardDebt, "still carries bare shortcodes/emoji: wire through themeFor(channel) or record why it is chrome (gd-Z3Sgg8W5Ih follow-up)"},
	"bot_mentions.go":                  {35, guardDebt, "still carries bare shortcodes/emoji: wire through themeFor(channel) or record why it is chrome (gd-Z3Sgg8W5Ih follow-up)"},
	"bot_person_identity.go":           {4, guardDebt, "refusal strings (:lock:/:warning:) added by the person-identity work (PR #250) after the Phase 4 wiring; theme them once the channel reaches the resolver (gd-Z3Sgg8W5Ih follow-up)"},
	"bot_permission.go":                {7, guardDebt, "still carries bare shortcodes/emoji: wire through themeFor(channel) or record why it is chrome (gd-Z3Sgg8W5Ih follow-up)"},
	"bot_reactions.go":                 {3, guardDebt, "still carries bare shortcodes/emoji: wire through themeFor(channel) or record why it is chrome (gd-Z3Sgg8W5Ih follow-up)"},
	"bot_thread_forward.go":            {3, guardDebt, "still carries bare shortcodes/emoji: wire through themeFor(channel) or record why it is chrome (gd-Z3Sgg8W5Ih follow-up)"},
	"bot_unreleased.go":                {1, guardDebt, "still carries bare shortcodes/emoji: wire through themeFor(channel) or record why it is chrome (gd-Z3Sgg8W5Ih follow-up)"},
	"canvas.go":                        {1, guardDebt, "still carries bare shortcodes/emoji: wire through themeFor(channel) or record why it is chrome (gd-Z3Sgg8W5Ih follow-up)"},
	"chat.go":                          {2, guardDebt, "still carries bare shortcodes/emoji: wire through themeFor(channel) or record why it is chrome (gd-Z3Sgg8W5Ih follow-up)"},
	"comment_forward.go":               {2, guardDebt, "still carries bare shortcodes/emoji: wire through themeFor(channel) or record why it is chrome (gd-Z3Sgg8W5Ih follow-up)"},
	"cross_operator_notify.go":         {1, guardDebt, "still carries bare shortcodes/emoji: wire through themeFor(channel) or record why it is chrome (gd-Z3Sgg8W5Ih follow-up)"},
	"decision_message_mirrors.go":      {1, guardDebt, "still carries bare shortcodes/emoji: wire through themeFor(channel) or record why it is chrome (gd-Z3Sgg8W5Ih follow-up)"},
	"slack.go":                         {3, guardDebt, "still carries bare shortcodes/emoji: wire through themeFor(channel) or record why it is chrome (gd-Z3Sgg8W5Ih follow-up)"},
	"squawk.go":                        {1, guardDebt, "still carries bare shortcodes/emoji: wire through themeFor(channel) or record why it is chrome (gd-Z3Sgg8W5Ih follow-up)"},
}

// shortcodeNotEmoji are ":word:" tokens that appear in string literals as
// key separators (dedup keys, bead-id suffixes), not as Slack emoji.
var shortcodeNotEmoji = map[string]bool{
	":violated:": true, ":body:": true, ":approval:": true,
	":child-of:": true, ":xlinks:": true,
}

func isShortcodeNameRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '+' || r == '-'
}

func isWordByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_'
}

// shortcodeTokens returns every Slack ":shortcode:" in s. A shortcode is a
// colon-delimited lowercase name with at least one letter whose outer colons
// are not glued to a word character, which keeps "view:agents:active",
// "12:30:05" and "%s:%s" out.
func shortcodeTokens(s string) []string {
	var out []string
	lastClose := -1 // index of the previous token's closing colon, which may open the next
	for i := 0; i < len(s); i++ {
		if s[i] != ':' || (i > 0 && isWordByte(s[i-1]) && i != lastClose) {
			continue
		}
		j := i + 1
		hasLetter := false
		for j < len(s) && isShortcodeNameRune(rune(s[j])) {
			if s[j] >= 'a' && s[j] <= 'z' || (j == i+1 && (s[j] == '+' || s[j] == '-')) {
				hasLetter = true // letters, or the :+1: / :-1: thumbs
			}
			j++
		}
		if j == i+1 || j >= len(s) || s[j] != ':' || !hasLetter {
			continue
		}
		if j+1 < len(s) && isWordByte(s[j+1]) {
			continue
		}
		tok := s[i : j+1]
		if !shortcodeNotEmoji[tok] {
			out = append(out, tok)
		}
		lastClose = j
		i = j - 1
	}
	return out
}

// isEmojiRune reports whether r is a pictographic emoji code point. Plain
// typographic marks (arrows, bullets, dashes, ✓/✗, box and geometric
// shapes other than the emoji-presentation ones) are deliberately not
// emoji: Business theme glyphs use them.
func isEmojiRune(r rune) bool {
	switch {
	case r >= 0x1F000 && r <= 0x1FAFF:
		return true
	case r >= 0x2600 && r <= 0x27BF:
		switch r {
		case 0x2605, 0x2606, 0x2713, 0x2714, 0x2715, 0x2716, 0x2717, 0x2718, 0x270E:
			return false // ★ ☆ ✓ ✔ ✕ ✖ ✗ ✘ ✎: text marks
		}
		return true
	case r == 0x2B50, r == 0x2B55, r == 0x2B1B, r == 0x2B1C,
		r == 0x2139, r == 0x203C, r == 0x2049, r == 0x24C2,
		r == 0x25AA, r == 0x25AB, r == 0x25B6, r == 0x25C0,
		r >= 0x25FB && r <= 0x25FE,
		r >= 0x231A && r <= 0x231B, r >= 0x23E9 && r <= 0x23F3,
		r >= 0x23F8 && r <= 0x23FA,
		r == 0x3030, r == 0x303D, r == 0x3297, r == 0x3299:
		return true
	}
	return false
}

// emojiTokens returns each emoji cluster in s (an emoji rune plus any
// variation selectors, ZWJ-joined partners and keycap marks).
func emojiTokens(s string) []string {
	var out []string
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		if !isEmojiRune(rs[i]) {
			continue
		}
		j := i + 1
		for j < len(rs) {
			switch {
			case rs[j] == 0xFE0F || rs[j] == 0x20E3 || (rs[j] >= 0x1F3FB && rs[j] <= 0x1F3FF):
				j++
			case rs[j] == 0x200D && j+1 < len(rs) && isEmojiRune(rs[j+1]):
				j += 2
			default:
				goto done
			}
		}
	done:
		out = append(out, string(rs[i:j]))
		i = j - 1
	}
	return out
}

type guardHit struct {
	Pos   token.Position
	Token string
}

// scanEmojiLiterals parses every non-test .go file directly in dir and
// returns the shortcode/emoji tokens found in string literals, by file. It
// reads literals through the AST, so comments are never matched.
func scanEmojiLiterals(t testing.TB, dir string) map[string][]guardHit {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	hits := map[string][]guardHit{}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		base := filepath.Base(path)
		ast.Inspect(f, func(n ast.Node) bool {
			if _, ok := n.(*ast.ImportSpec); ok {
				return false
			}
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			for _, tok := range append(shortcodeTokens(s), emojiTokens(s)...) {
				hits[base] = append(hits[base], guardHit{Pos: fset.Position(lit.Pos()), Token: tok})
			}
			return true
		})
	}
	return hits
}

func TestNoHardCodedEmojiLiterals(t *testing.T) {
	hits := scanEmojiLiterals(t, ".")

	if os.Getenv("THEME_GUARD_DUMP") != "" {
		var names []string
		for f := range hits {
			names = append(names, f)
		}
		sort.Strings(names)
		for _, f := range names {
			fmt.Printf("%s\t%d\n", f, len(hits[f]))
		}
	}

	var files []string
	seen := map[string]bool{}
	for f := range hits {
		files = append(files, f)
		seen[f] = true
	}
	for f := range emojiLiteralAllowlist {
		if !seen[f] {
			files = append(files, f)
		}
	}
	sort.Strings(files)

	for _, f := range files {
		got := len(hits[f])
		entry, listed := emojiLiteralAllowlist[f]
		switch {
		case !listed:
			t.Errorf("%s: %d hard-coded Slack shortcode/emoji literal(s) outside the allowlist:\n%s"+
				"Resolve the glyph through the channel's Theme (themeFor(ctx, channel).Semantic/Tool/Reaction/Glyph — "+
				"see theme.go) so Garden and Business channels stay consistent. If it is genuinely not a themeable "+
				"Slack surface, add %q to emojiLiteralAllowlist in theme_guard_test.go with a reason.",
				f, got, formatGuardHits(hits[f]), f)
		case got > entry.Max:
			t.Errorf("%s: %d shortcode/emoji literal(s), allowlist permits %d (%s: %s). New literal(s):\n%s"+
				"Theme them instead of raising the allowlist.",
				f, got, entry.Max, entry.Kind, entry.Why, formatGuardHits(hits[f]))
		case got < entry.Max:
			t.Errorf("%s: %d shortcode/emoji literal(s) but the allowlist still permits %d — lower Max to %d "+
				"(or delete the entry at 0) so the ratchet holds.", f, got, entry.Max, got)
		}
	}
}

// TestEmojiLiteralAllowlistEntries keeps the allowlist honest: every entry
// names a real file, has a kind and a reason, and a Max above zero.
func TestEmojiLiteralAllowlistEntries(t *testing.T) {
	for f, e := range emojiLiteralAllowlist {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("allowlist entry %q: %v", f, err)
		}
		if strings.HasSuffix(f, "_test.go") {
			t.Errorf("allowlist entry %q: test files are never scanned", f)
		}
		switch e.Kind {
		case guardVocabulary, guardParse, guardChrome, guardDebt:
		default:
			t.Errorf("allowlist entry %q: unknown kind %q", f, e.Kind)
		}
		if strings.TrimSpace(e.Why) == "" {
			t.Errorf("allowlist entry %q: missing reason", f)
		}
		if e.Max <= 0 {
			t.Errorf("allowlist entry %q: Max %d — delete the entry once the file is clean", f, e.Max)
		}
	}
}

func formatGuardHits(hits []guardHit) string {
	var b strings.Builder
	for _, h := range hits {
		fmt.Fprintf(&b, "  %s:%d  %s\n", h.Pos.Filename, h.Pos.Line, h.Token)
	}
	return b.String()
}

// TestEmojiLiteralDetector pins the detector itself so the guard cannot rot
// into never matching (or matching key separators).
func TestEmojiLiteralDetector(t *testing.T) {
	short := []struct {
		in   string
		want int
	}{
		{":rocket: Spawning agent", 1},
		{"a :white_check_mark: b :x: c", 2},
		{":+1: and :-1:", 2},
		{":warning::lock:", 2},
		{"view:agents:active", 0},
		{"config:prerun:role:gardener", 0},
		{"12:30:05", 0},
		{"%s:%s:%s", 0},
		{"fixture:e2e:", 0},
		{":violated:", 0},
		{"id:body:x", 0},
		{"no colons here", 0},
		{"<@U123>: said :", 0},
	}
	for _, c := range short {
		if got := len(shortcodeTokens(c.in)); got != c.want {
			t.Errorf("shortcodeTokens(%q) = %d, want %d", c.in, got, c.want)
		}
	}
	emoji := []struct {
		in   string
		want int
	}{
		{"🚀 shipped", 1},
		{"✅ ❌ ⚠️", 3},
		{"⏰ ⏸ ▶️ ⏹", 4},
		{"🧑‍🌾 farmer", 1}, // ZWJ sequence is one cluster
		{"ℹ️ note", 1},
		{"→ ← ↻ ● ■ ◆ ✓ ✗ ✎ — … ·", 0}, // Business text marks
		{"[OK] plain", 0},
	}
	for _, c := range emoji {
		if got := len(emojiTokens(c.in)); got != c.want {
			t.Errorf("emojiTokens(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}
