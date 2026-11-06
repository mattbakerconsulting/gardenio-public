package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"gardenio/controller/internal/advice"
	"gardenio/controller/internal/beadsapi"
)

func TestIsInlineAdvice(t *testing.T) {
	tests := []struct {
		name   string
		labels []string
		want   bool
	}{
		{"with inline:true", []string{"global", "inline:true"}, true},
		{"without inline label", []string{"global", "project:gardenio"}, false},
		{"empty labels", nil, false},
		{"inline:false is not inline", []string{"inline:false"}, false},
		{"reference:true does not imply inline", []string{"reference:true"}, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := &beadsapi.BeadDetail{Labels: tc.labels}
			if got := isInlineAdvice(b); got != tc.want {
				t.Errorf("isInlineAdvice(%v) = %v, want %v", tc.labels, got, tc.want)
			}
		})
	}
}

func TestAdviceExcerpt(t *testing.T) {
	tests := []struct {
		name  string
		input string
		max   int
		want  string
	}{
		{"empty", "", 500, ""},
		{"short adds period", "do the thing", 500, "do the thing."},
		{"short keeps terminal punctuation", "do it!", 500, "do it!"},
		{"flattens newlines", "rule line one\npayload line two", 500, "rule line one payload line two."},
		{"collapses whitespace runs", "a\n\n   b\tc", 500, "a b c."},
		{
			name:  "cuts on sentence boundary in back half",
			input: "Short one. This second clause runs well beyond the cap for sure.",
			max:   14,
			want:  "Short one. …",
		},
		{
			name:  "cuts on word boundary when no sentence break",
			input: "alpha bravo charlie delta echo foxtrot golf hotel india juliet",
			max:   20,
			want:  "alpha bravo charlie …",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := advice.Excerpt(tc.input, tc.max); got != tc.want {
				t.Errorf("advice.Excerpt(%q, %d) = %q, want %q", tc.input, tc.max, got, tc.want)
			}
		})
	}
}

func TestFirstLine(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"hello world", "hello world"},
		{"line one\nline two", "line one"},
		{"  trimmed  \nsecond", "trimmed"},
		{"", ""},
		{"\nonly newline", "only newline"},
	}

	for _, tc := range tests {
		got := firstLine(tc.input)
		if got != tc.want {
			t.Errorf("firstLine(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// renderAdviceItem exercises the per-item rendering used by outputAdvice without
// standing up a daemon. It delegates to the production renderers so the two
// paths cannot drift.
func renderAdviceItem(w *bytes.Buffer, header string, b *beadsapi.BeadDetail) {
	if isInlineAdvice(b) {
		renderInlineAdviceItem(w, header, b)
		return
	}
	renderAbbrevAdviceItem(w, header, b)
}

// TestOutputAdvice_AbbreviatedByDefault asserts that a bead with no `inline:true`
// label renders as an abbreviated pointer (title + summary + `gb advice show`)
// — the new default after flipping the rendering model.
func TestOutputAdvice_AbbreviatedByDefault(t *testing.T) {
	bead := &beadsapi.BeadDetail{
		ID:          "gd-test123",
		Title:       "Query Guide for VictoriaTraces",
		Description: "Use TraceQL to search traces.\nSupports regex and field-level queries.\nSee docs for details.",
		Labels:      []string{"global"},
	}

	var buf bytes.Buffer
	renderAdviceItem(&buf, "Global", bead)
	got := buf.String()

	if !strings.Contains(got, "`gb advice show gd-test123`") {
		t.Errorf("default render missing lookup command:\n%s", got)
	}
	if !strings.Contains(got, "Use TraceQL to search traces.") {
		t.Errorf("default render missing one-line summary:\n%s", got)
	}
	if strings.Contains(got, "[Ref]") {
		t.Errorf("default render should not emit [Ref] prefix:\n%s", got)
	}
	// A short body now renders in full as a flattened excerpt (newlines
	// collapsed to spaces) so the actionable payload on line 2+ is not lost.
	if !strings.Contains(got, "Supports regex and field-level queries.") {
		t.Errorf("default render dropped line-2 payload it should now include:\n%s", got)
	}
	if strings.Contains(got, "\n  Supports") {
		t.Errorf("abbreviated body should be flattened inline, not indented multi-line:\n%s", got)
	}
}

// TestOutputAdvice_LongBodyTruncates asserts that a body exceeding the summary
// watermark is cut on a boundary with a trailing ellipsis, while the pointer is
// still emitted so the agent can fetch the rest.
func TestOutputAdvice_LongBodyTruncates(t *testing.T) {
	t.Setenv("GB_PRIME_ADVICE_SUMMARY_CHARS", "120")
	bead := &beadsapi.BeadDetail{
		ID:    "gd-long01",
		Title: "Long advice",
		Description: "First actionable sentence with the rule. Second sentence adds the concrete command form. " +
			"Third sentence is rationale that should fall past the watermark and not be rendered inline.",
		Labels: []string{"global"},
	}

	var buf bytes.Buffer
	renderAdviceItem(&buf, "Global", bead)
	got := buf.String()

	if !strings.Contains(got, "…") {
		t.Errorf("long body should be truncated with an ellipsis:\n%s", got)
	}
	if !strings.Contains(got, "`gb advice show gd-long01`") {
		t.Errorf("truncated render missing pointer:\n%s", got)
	}
	if strings.Contains(got, "rationale that should fall past") {
		t.Errorf("render leaked content past the watermark:\n%s", got)
	}
}

// TestOutputAdvice_InlineRendersFullText asserts that a bead carrying the
// `inline:true` label renders its full (cleaned) description inline, for
// policy-critical rules agents MUST see every session.
func TestOutputAdvice_InlineRendersFullText(t *testing.T) {
	bead := &beadsapi.BeadDetail{
		ID:          "gd-inline01",
		Title:       "NEVER write to FSM JIRA project",
		Description: "Only PE and DEVOPS projects accept writes.\nFSM is read-only for all agents.",
		Labels:      []string{"global", "inline:true"},
	}

	var buf bytes.Buffer
	renderAdviceItem(&buf, "Global", bead)
	got := buf.String()

	if !strings.Contains(got, "Only PE and DEVOPS projects accept writes.") {
		t.Errorf("inline render missing full description line 1:\n%s", got)
	}
	if !strings.Contains(got, "FSM is read-only for all agents.") {
		t.Errorf("inline render missing full description line 2:\n%s", got)
	}
	if strings.Contains(got, "gb advice show") {
		t.Errorf("inline render should not emit the pointer command:\n%s", got)
	}
}

// TestOutputAdvice_MixedInlineAndAbbreviated renders both forms in one pass
// and asserts each bead gets the correct treatment without interference.
func TestOutputAdvice_MixedInlineAndAbbreviated(t *testing.T) {
	abbrev := &beadsapi.BeadDetail{
		ID:          "gd-abbrev01",
		Title:       "Use kubectl logs for live debugging",
		Description: "Stream logs with kubectl logs -f <pod>.\nMore tips in docs.",
		Labels:      []string{"global"},
	}
	inline := &beadsapi.BeadDetail{
		ID:          "gd-inline02",
		Title:       "kubectl set image requires decision checkpoint",
		Description: "Use gb decision before running kubectl set image on live deployments.",
		Labels:      []string{"global", "inline:true"},
	}

	var buf bytes.Buffer
	renderAdviceItem(&buf, "Global", abbrev)
	renderAdviceItem(&buf, "Global", inline)
	got := buf.String()

	// Abbreviated: pointer present, and a short body now renders inline in full.
	if !strings.Contains(got, "`gb advice show gd-abbrev01`") {
		t.Errorf("mixed render missing pointer for abbreviated bead:\n%s", got)
	}
	if !strings.Contains(got, "More tips in docs") {
		t.Errorf("mixed render dropped short abbreviated body it should include:\n%s", got)
	}

	// Inline: body present, pointer absent for this specific bead.
	if !strings.Contains(got, "Use gb decision before running kubectl set image") {
		t.Errorf("mixed render missing inline bead body:\n%s", got)
	}
	if strings.Contains(got, "`gb advice show gd-inline02`") {
		t.Errorf("mixed render should not emit pointer for inline bead:\n%s", got)
	}
}

func TestIsHippoMirror(t *testing.T) {
	tests := []struct {
		name string
		b    *beadsapi.BeadDetail
		want bool
	}{
		{
			name: "source:hippo-global label",
			b:    &beadsapi.BeadDetail{Labels: []string{"global", "source:hippo-global"}},
			want: true,
		},
		{
			name: "source field = hippo-global",
			b:    &beadsapi.BeadDetail{Labels: []string{"global"}, Fields: map[string]string{"source": "hippo-global"}},
			want: true,
		},
		{
			name: "source:hippo-memory label (Q/A gardener replay)",
			b:    &beadsapi.BeadDetail{Labels: []string{"g0:project:gardenio", "source:hippo-memory"}},
			want: true,
		},
		{
			name: "source field = hippo-memory",
			b:    &beadsapi.BeadDetail{Labels: []string{"g0:project:gardenio"}, Fields: map[string]string{"source": "hippo-memory"}},
			want: true,
		},
		{
			name: "plain human-authored advice",
			b:    &beadsapi.BeadDetail{Labels: []string{"global"}},
			want: false,
		},
		{
			name: "no labels, no fields",
			b:    &beadsapi.BeadDetail{},
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isHippoMirror(tc.b); got != tc.want {
				t.Errorf("isHippoMirror() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestFilterOutHippoMirrors asserts the guardrail drops every legacy hippo
// mirror bead (both hippo-global and hippo-memory variants) so none reach
// Claude's context via gb prime, even if re-created.
func TestFilterOutHippoMirrors(t *testing.T) {
	// Synthesize a match set mixing legacy mirror beads (hippo-global and
	// hippo-memory variants) with human-authored beads.
	mirrors := []*beadsapi.BeadDetail{
		{ID: "gd-m2phmRJiPk", Title: "JIRA writes: PE and DEVOPS only", Labels: []string{"global", "source:hippo-global"}},
		{ID: "gd-Ul8lShthSK", Title: "kubectl set image requires decision checkpoint", Labels: []string{"global", "source:hippo-global"}},
		{ID: "gd-vLh65zAZEG", Title: "mirror via source field only", Labels: []string{"global"}, Fields: map[string]string{"source": "hippo-global"}},
		{ID: "gd-KhXU08vmwd", Title: "Q: Gardener cycle (2026-04-16T15:55Z) ... → A: ack", Labels: []string{"g0:project:gardenio", "g0:role:gardener", "source:hippo-memory"}},
		{ID: "gd-XvKniKdsQN", Title: "Q: Gardener cycle complete ... → A: Acknowledge", Labels: []string{"g0:project:gardenio", "g0:role:gardener", "source:hippo-memory"}},
	}
	humans := []*beadsapi.BeadDetail{
		{ID: "gd-bPZIusr7R6", Title: "kubectl set image (human)", Labels: []string{"global"}},
		{ID: "gd-UXSqirHuDc", Title: "JIRA project write scope (human)", Labels: []string{"project:monorepo", "role:monorepo-gardener"}},
	}

	var matched []advice.MatchedAdvice
	for _, b := range mirrors {
		matched = append(matched, advice.MatchedAdvice{Bead: b, Scope: "global", ScopeHeader: "Global"})
	}
	for _, b := range humans {
		matched = append(matched, advice.MatchedAdvice{Bead: b, Scope: "global", ScopeHeader: "Global"})
	}

	got := filterOutHippoMirrors(matched)
	if len(got) != len(humans) {
		t.Fatalf("filter kept %d beads, want %d (dropped all %d mirrors)", len(got), len(humans), len(mirrors))
	}

	// Verify no mirror ID survives.
	for _, m := range got {
		if isHippoMirror(m.Bead) {
			t.Errorf("filter leaked mirror bead %s (labels=%v fields=%v)", m.Bead.ID, m.Bead.Labels, m.Bead.Fields)
		}
	}

	// Verify all non-mirror beads are preserved.
	wantIDs := map[string]bool{"gd-bPZIusr7R6": true, "gd-UXSqirHuDc": true}
	for _, m := range got {
		delete(wantIDs, m.Bead.ID)
	}
	if len(wantIDs) != 0 {
		t.Errorf("filter dropped non-mirror beads: %v", wantIDs)
	}
}

func TestStripTitlePrefix(t *testing.T) {
	tests := []struct {
		name        string
		title       string
		description string
		want        string
	}{
		{
			name:        "exact title+newline prefix",
			title:       "My Rule",
			description: "My Rule\nBody line one\nBody line two",
			want:        "Body line one\nBody line two",
		},
		{
			name:        "title with trailing spaces on desc line",
			title:       "My Rule",
			description: "My Rule   \nBody",
			want:        "Body",
		},
		{
			name:        "title with leading spaces on desc line",
			title:       "My Rule",
			description: "  My Rule\nBody",
			want:        "Body",
		},
		{
			name:        "no prefix match",
			title:       "My Rule",
			description: "Unrelated body",
			want:        "Unrelated body",
		},
		{
			name:        "leading blank line stripped after prefix",
			title:       "My Rule",
			description: "My Rule\n\nBody",
			want:        "Body",
		},
		{
			name:        "multi-line title stripped",
			title:       "Rule line A\nRule line B",
			description: "Rule line A\nRule line B\nBody",
			want:        "Body",
		},
		{
			name:        "empty title pass-through",
			title:       "",
			description: "Body",
			want:        "Body",
		},
		{
			name:        "desc shorter than multi-line title",
			title:       "Line A\nLine B\nLine C",
			description: "Line A\nLine B",
			want:        "Line A\nLine B",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := advice.StripTitlePrefix(tc.title, tc.description)
			if got != tc.want {
				t.Errorf("advice.StripTitlePrefix(%q, %q) = %q, want %q", tc.title, tc.description, got, tc.want)
			}
		})
	}
}

func TestCollapseConsecutiveDuplicateLines(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"no duplicates", "a\nb\nc", "a\nb\nc"},
		{"adjacent duplicates collapsed", "a\na\nb", "a\nb"},
		{"three in a row", "x\nx\nx\ny", "x\ny"},
		{"non-adjacent duplicates kept", "a\nb\na", "a\nb\na"},
		{"empty lines collapsed", "a\n\n\nb", "a\n\nb"},
		{"empty input", "", ""},
		{"single line", "only", "only"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := advice.CollapseConsecutiveDuplicateLines(tc.in)
			if got != tc.want {
				t.Errorf("advice.CollapseConsecutiveDuplicateLines(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestCleanAdviceDescription(t *testing.T) {
	tests := []struct {
		name        string
		title       string
		description string
		want        string
	}{
		{"empty description", "Title", "", ""},
		{"description equals title", "Title", "Title", ""},
		{"title-prefixed description", "Title", "Title\nBody\nMore", "Body\nMore"},
		{"no title prefix kept as-is", "Title", "Different body\nline2", "Different body\nline2"},
		{"consecutive duplicates collapsed", "Title", "Title\nline\nline\nend", "line\nend"},
		{"title-only description with trailing whitespace", "Title", "Title   ", ""},
		{"description is just title then blank lines", "Title", "Title\n\n\n", ""},
		{
			name:        "3x repeated mirror bead body collapses aggressively",
			title:       "JIRA writes: PE and DEVOPS only",
			description: "JIRA writes: PE and DEVOPS only\nJIRA writes: PE and DEVOPS only\nJIRA writes: PE and DEVOPS only\nactual body",
			want:        "actual body",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := advice.CleanDescription(tc.title, tc.description)
			if got != tc.want {
				t.Errorf("advice.CleanDescription(%q, %q) = %q, want %q", tc.title, tc.description, got, tc.want)
			}
		})
	}
}

// TestRenderAdviceItem_TitleAppearsExactlyOnce asserts the end-to-end render
// format used in outputAdvice does not duplicate the title, even when the
// description begins with the title verbatim (the common case that produced
// the duplicate-rendering bug fixed in gd-rgn2GmMrwR).
func TestRenderAdviceItem_TitleAppearsExactlyOnce(t *testing.T) {
	cases := []struct {
		name        string
		title       string
		description string
	}{
		{
			name:        "description starts with title+newline",
			title:       "Tests must exercise the actual bug fix",
			description: "Tests must exercise the actual bug fix\nA test that passes without the fix applied is not a regression test.",
		},
		{
			name:        "description equals title",
			title:       "Use gb squawk for progress updates",
			description: "Use gb squawk for progress updates",
		},
		{
			name:        "description has three identical leading lines",
			title:       "kubectl set image requires decision checkpoint",
			description: "kubectl set image requires decision checkpoint\nkubectl set image requires decision checkpoint\nkubectl set image requires decision checkpoint\nUse gb decision before running kubectl set image.",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			fmt.Fprintf(&buf, "**[Global]** %s\n", tc.title)
			desc := advice.CleanDescription(tc.title, tc.description)
			if desc != "" {
				for _, line := range strings.Split(desc, "\n") {
					fmt.Fprintf(&buf, "  %s\n", line)
				}
			}

			rendered := buf.String()
			count := strings.Count(rendered, tc.title)
			if count != 1 {
				t.Errorf("title appeared %d times in rendered output, want exactly 1\n---\n%s---", count, rendered)
			}
		})
	}
}

// TestOutputAdvice_HippoGlobalMirrorNeverRendered asserts the baseline-vs-fixed
// size invariant for the advice section: mirror beads with source:hippo-global
// MUST NOT appear in gb prime output, and the rendered size is strictly less
// than the unfiltered baseline.
//
// This test pins the behavior that shrinks the advice section in gb prime.
// If it fails, the hippo→advice leak has re-opened.
func TestOutputAdvice_HippoGlobalMirrorNeverRendered(t *testing.T) {
	// Representative malformed mirror bead — the title bleeds into the
	// description (this is how outputAdvice would emit ~3× the content).
	mirrorTitle := "JIRA writes: PE and DEVOPS only — all others READ-ONLY\nJIRA write access: o..."
	mirrorDesc := "JIRA writes: PE and DEVOPS only — all others READ-ONLY\nJIRA write access: o...\nJIRA writes: PE and DEVOPS only — all others READ-ONLY\nJIRA write access: only PE (Engineering) and DEVOPS projects are authorized."

	matched := []advice.MatchedAdvice{
		{
			Bead: &beadsapi.BeadDetail{
				ID:          "gd-m2phmRJiPk",
				Title:       mirrorTitle,
				Description: mirrorDesc,
				Labels:      []string{"global", "source:hippo-global"},
				Fields:      map[string]string{"source": "hippo-global"},
			},
			Scope: "global", ScopeHeader: "Global",
		},
		{
			Bead: &beadsapi.BeadDetail{
				ID:          "gd-bPZIusr7R6",
				Title:       "kubectl set image requires decision checkpoint",
				Description: "Use gb decision before running kubectl set image on live deployments.",
				Labels:      []string{"global"},
			},
			Scope: "global", ScopeHeader: "Global",
		},
	}

	// Baseline: render without the guardrail (simulates the pre-fix state).
	var baseline bytes.Buffer
	for _, m := range matched {
		baseline.WriteString("**[Global]** " + m.Bead.Title + "\n  " + m.Bead.Description + "\n\n")
	}
	baselineSize := baseline.Len()

	// Fixed: filter mirrors first, then render the same way.
	filtered := filterOutHippoMirrors(matched)
	var fixed bytes.Buffer
	for _, m := range filtered {
		fixed.WriteString("**[Global]** " + m.Bead.Title + "\n  " + m.Bead.Description + "\n\n")
	}
	fixedSize := fixed.Len()

	if fixedSize >= baselineSize {
		t.Errorf("advice section size did not shrink: baseline=%d fixed=%d (mirror leak not plugged)",
			baselineSize, fixedSize)
	}
	if bytes.Contains(fixed.Bytes(), []byte("gd-m2phmRJiPk")) {
		t.Errorf("mirror bead ID leaked into rendered output")
	}
	if bytes.Contains(fixed.Bytes(), []byte("JIRA writes: PE and DEVOPS only")) {
		// This specific phrase is the mirror's malformed title — it must
		// not appear. (Human-authored equivalents with different titles
		// are fine; this test uses a disjoint human bead.)
		t.Errorf("mirror bead content leaked into rendered output")
	}
	if !bytes.Contains(fixed.Bytes(), []byte("kubectl set image")) {
		t.Errorf("human-authored advice was dropped by filter")
	}
}

// TestAdviceExcerpt_RuneBoundaryMultibyte is the F7 (byte-vs-rune) regression
// guard from the advice-truncation e2e test design (ka-0000003643, doc
// gardenio/docs/advice-truncation-e2e-test-design.md §F7).
//
// advice.Excerpt caps the per-item summary by rune count (its `max` argument is
// runes, not bytes) while the budget cascade in renderAdviceItemsWithLimits
// accounts in bytes. That unit mismatch is only safe because advice.Excerpt cuts
// on rune / word boundaries via []rune indexing — never mid-rune. This test
// pins that invariant against a multibyte-heavy body (emoji + CJK): whatever
// advice.Excerpt returns must be (a) valid UTF-8 with (b) no U+FFFD replacement
// char and (c) exactly `max` runes' worth of prefix or less, never a partial
// trailing rune. A future refactor that switched advice.Excerpt to byte-slicing
// (`s[:max]`) would corrupt the agent's context invisibly and this test would
// catch it.
func TestAdviceExcerpt_RuneBoundaryMultibyte(t *testing.T) {
	// Each of these runes is multi-byte in UTF-8: the emoji are 4 bytes, the
	// CJK ideographs 3 bytes. A byte-index cut at almost any offset would land
	// mid-rune and produce invalid UTF-8. Built long enough (and space-free
	// within each token) that the rune cap must bite.
	//
	// 🚀 = U+1F680 (4 bytes), 日本語 = 3 bytes each, 🔥 = U+1F525 (4 bytes).
	tokens := make([]string, 0, 60)
	for i := range 60 {
		tokens = append(tokens, fmt.Sprintf("規則%d🚀日本語のルール🔥", i))
	}
	body := strings.Join(tokens, " ")

	// Sanity: the raw body is far longer than any cap we test, so truncation
	// genuinely happens (an untruncated excerpt could pass (a)/(b) vacuously).
	if utf8.RuneCountInString(body) < 200 {
		t.Fatalf("test body too short (%d runes) to force the rune cap", utf8.RuneCountInString(body))
	}

	for _, max := range []int{10, 33, 50, 128} {
		t.Run(fmt.Sprintf("max=%d", max), func(t *testing.T) {
			got := advice.Excerpt(body, max)

			// (a) Valid UTF-8 end-to-end.
			if !utf8.ValidString(got) {
				t.Fatalf("advice.Excerpt returned invalid UTF-8 at max=%d: %q", max, got)
			}
			// (b) No replacement char — a mid-rune byte cut would surface one.
			if strings.ContainsRune(got, '�') {
				t.Fatalf("advice.Excerpt output contains U+FFFD (mid-rune cut) at max=%d: %q", max, got)
			}
			// (c) The excerpt (minus the trailing " …" ellipsis marker the
			//     cascade appends on truncation) never exceeds `max` runes, so
			//     the rune cap is honored — it did not overshoot into bytes.
			trimmed := strings.TrimSuffix(got, " …")
			if n := utf8.RuneCountInString(trimmed); n > max {
				t.Errorf("excerpt body = %d runes, exceeds cap %d: %q", n, max, got)
			}
			// The result must be non-empty for a non-empty body.
			if got == "" {
				t.Errorf("advice.Excerpt returned empty for a non-empty multibyte body at max=%d", max)
			}
		})
	}
}

// TestRenderAbbrevAdviceItem_MultibyteBodyValidUTF8 proves the F7 invariant one
// layer up: the *rendered* abbreviated item (title + summary + pointer), the
// exact bytes gb prime writes, stays valid UTF-8 when the summary is cut. This
// is the render-path companion to the advice.Excerpt unit above — it guards
// against a boundary bug in renderAbbrevAdviceItem's string assembly, not just
// in the excerpt function.
func TestRenderAbbrevAdviceItem_MultibyteBodyValidUTF8(t *testing.T) {
	t.Setenv(primeAdviceSummaryEnv, "40") // force the cap to bite
	bead := &beadsapi.BeadDetail{
		ID:    "gd-utf8rune",
		Title: "多字节规则 🚀 multibyte rule",
		Description: strings.Repeat("これはとても長い日本語のアドバイス本文です🔥。", 20) +
			"\nsupplementary payload line 🚀🚀🚀",
		Labels: []string{"global"},
	}

	var buf bytes.Buffer
	renderAbbrevAdviceItem(&buf, "Global", bead)
	got := buf.Bytes()

	if !utf8.Valid(got) {
		t.Fatalf("rendered abbreviated item is not valid UTF-8:\n%q", got)
	}
	if bytes.ContainsRune(got, '�') {
		t.Fatalf("rendered abbreviated item contains U+FFFD (mid-rune cut):\n%q", got)
	}
	if !bytes.Contains(got, []byte("`gb advice show gd-utf8rune`")) {
		t.Errorf("rendered item missing pointer:\n%s", got)
	}
}
