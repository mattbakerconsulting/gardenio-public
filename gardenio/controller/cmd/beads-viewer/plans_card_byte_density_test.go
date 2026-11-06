package main

import (
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

// byteDensityTestNow pins the clock so the seed plan's head-row freshness stamp
// renders a STABLE compact age. The seed's newest activity is its epic
// UpdatedAt (2026-07-01 10:00 UTC, see seedPlansFake); fmtTimeAgo floors to a
// "d" unit, so any now in [2026-08-10 10:00, 2026-08-11 10:00) renders exactly
// "40d". Pinned at 2026-08-10 12:00 UTC — without this the stamp aged off "40d"
// on 2026-08-11 and TestPlansCard_ByteDensity_VisibleOutputUnchanged reds every
// MR fleet-wide on a wall-clock-dependent assertion (gd-3XmNaaXSIn).
var byteDensityTestNow = time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)

// plans_card_byte_density_test.go — iter-50 (ka-0000005352): the FIRST byte-level
// density pass on the /plans index card. Every prior iteration (1-49) changed a
// VISIBLE signal or a CSS rule; this one changes zero visible output. It trims the
// template-emitted whitespace that ships raw on every card, every render.
//
// THE MEASURE (measure-first, seed-rendered card, pre-iter-50): the plan_card
// partial rendered ~3528 bytes, of which ~21.5% was whitespace and 58 of 108
// lines were BLANK — emitted by unfired {{if}} conditionals and standalone
// control/comment lines in plan_card.html. beads-viewer serves NO gzip/deflate
// middleware (grep -ri content-encoding cmd/beads-viewer is empty), so that
// whitespace is not masked on the wire — it is paid on every one of the ~72-123
// live board cards, every page render and every SSE single-row re-render.
//
// THE FIX: Go text/template {{- -}} trim markers on the purely-structural lines
// (standalone {{if}}/{{end}}/{{with}}/comments — leading+trailing trim) and the
// single-line conditional-content lines ({{if C}}<chip>{{end}} — LEADING trim
// only). Every multi-child card container is display:flex; gap (.plan-card-head,
// .plan-card-meta, .plan-progress, .plan-taskgrid-row/-squares, .plan-card-step,
// .plan-card-crew*), so inter-tag whitespace is a collapsed text node that
// contributes NO visible spacing — trimming it is visually inert. The result is
// ~12.5% fewer bytes per card with byte-identical VISIBLE output.
//
// These tests guard the win two ways: a density-floor + no-blank-line-regrowth
// structural guard (so a future edit re-loosening the template reds here), and a
// correctness guard on the ONE trap the mechanical pass could hit — the
// multi-line escalation tooltip title, whose inter-title newlines must survive
// (a stray trailing -}} on its {{range}} block-open would jam the titles).

// planCardHTML renders the served /plans index and returns the single <li
// class="plan-card"> ... </li> block for the seed plan.
func planCardHTML(t *testing.T, f *fakeDaemon) string {
	t.Helper()
	mux := mkServerWithNow(t, f, func() time.Time { return byteDensityTestNow })
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans", "matt@acme.example"))
	if w.Code != 200 {
		t.Fatalf("GET /plans: expected 200, got %d", w.Code)
	}
	re := regexp.MustCompile(`(?s)<li class="plan-card".*?</li>`)
	m := re.FindString(w.Body.String())
	if m == "" {
		t.Fatalf("no plan-card <li> found in /plans body")
	}
	return m
}

// TestPlansCard_ByteDensity_NoBlankLineRegrowth proves the iter-50 trim pass is
// present and stays present. It keys on the exact quantity the trim targets:
// REMOVABLE inter-tag whitespace — the bytes between a `>` and the next `<` that
// are pure whitespace, which a flex/gap container collapses to nothing visible
// and which the browser never renders. That is the honest measure of the win,
// independent of the real indentation on content lines (which is NOT removable
// and which a crude non-whitespace-fraction check would wrongly penalize).
//
// Pre-iter-50 the seed card carried ~605 bytes of removable inter-tag whitespace
// (raw 3528 → normalized 2923); post-trim ~236 (raw 3159). A future refactor
// that drops the trim markers regrows that gap and reds here. Also guards the
// blank-line count (pre-trim 58 of 108 lines; post-trim 9 of 49).
func TestPlansCard_ByteDensity_NoBlankLineRegrowth(t *testing.T) {
	card := planCardHTML(t, seedPlansFake())

	// Removable inter-tag whitespace = raw bytes − normalized bytes, where the
	// normalized form collapses every pure-whitespace run between tags. This is
	// EXACTLY the whitespace the trim markers eliminate and the browser ignores.
	norm := regexp.MustCompile(`>\s+<`).ReplaceAllString(card, "><")
	removable := len(card) - len(norm)
	if removable > 320 {
		t.Errorf("plan card carries %d bytes of removable inter-tag whitespace — the iter-50 trim has regressed; expected <=320 (pre-trim baseline was ~605, post-trim ~236)", removable)
	}

	// Blank-line count: pre-trim the seed card rendered 58 blank lines of 108; the
	// trim pass drops the vast majority. A regression back toward the loose
	// template (dozens of blanks from untrimmed {{if}} lines) reds here.
	lines := strings.Split(card, "\n")
	blank := 0
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			blank++
		}
	}
	if blank > 15 {
		t.Errorf("plan card has %d blank lines (of %d) — the iter-50 whitespace trim has regressed; expected <=15 (pre-trim baseline was 58)", blank, len(lines))
	}
}

// TestPlansCard_ByteDensity_VisibleOutputUnchanged is the crux invariant: trimming
// template whitespace must not change the VISIBLE card. It normalizes the rendered
// card by collapsing every run of whitespace BETWEEN tags (>...<) to nothing —
// exactly the whitespace the browser itself collapses in a flex/gap container —
// and asserts the visible token stream is intact: every chip, its text, and its
// order survive. This is the property the trim pass preserves and a bad trim
// (eating a text node, reordering, dropping a chip) would break.
func TestPlansCard_ByteDensity_VisibleOutputUnchanged(t *testing.T) {
	card := planCardHTML(t, seedPlansFake())
	norm := regexp.MustCompile(`>\s+<`).ReplaceAllString(card, "><")

	// The visible tokens the seed card must still render, in order. These are the
	// exact user-facing strings across the meta row, progress row, step chip, and
	// dispatch button — if the trim ate a text node or jammed two together, one of
	// these ordered substrings would vanish.
	wantInOrder := []string{
		`>40d</span>`,      // freshness stamp — head-row compact age (iter-52; seed is 40d stale). Precedes the meta strip now.
		`>in_progress<`,    // status chip
		`>P0<`,             // priority pill
		`>gardenio<`,       // project chip
		`>1 to answer<`,    // decision chip
		`⚠ stalled `,       // stall badge headline
		`· no gardener`,    // stall reason
		`>0/1 steps · 0%<`, // progress label
		`>next<`,           // step-kind pill
		`>#1<`,             // step number
		`>Wire the route<`, // step title
		`>not started<`,    // plan-agent chip
		`Dispatch #1 `,     // dispatch button
	}
	last := 0
	for _, tok := range wantInOrder {
		idx := strings.Index(norm[last:], tok)
		if idx < 0 {
			t.Errorf("normalized card lost visible token %q (trim jammed or ate a text node)", tok)
			continue
		}
		last += idx + len(tok)
	}
}

// TestPlansCard_ByteDensity_EscalationTooltipNewlinesPreserved is the correctness
// guard on the ONE trap the mechanical trim pass could hit. The ⛔ escalation
// badge's title attribute lists each escalation on its OWN line via a {{range
// .Escalations.Escalations}} block whose opening action ends line 170 and whose
// body (the "\n{{.Title}}...") begins line 171. A naive trim that stamped a
// trailing `-}}` on that {{range}} block-OPEN would eat the newline separating
// the escalation titles, collapsing a multi-escalation tooltip into one jammed
// run. The iter-50 pass gives conditional-content lines LEADING trim only for
// exactly this reason. This asserts a 2-escalation card still renders the titles
// newline-separated inside the tooltip.
func TestPlansCard_ByteDensity_EscalationTooltipNewlinesPreserved(t *testing.T) {
	f := seedPlansFake()
	// Two open escalations blocking the plan's leftover open step → the badge
	// renders "×2" with both titles in the title attribute, one per line.
	f.beads["gd-step1"].Status = "open"
	seedPlanEscalation(f, "gd-esc1", "gd-step1", "Approve AND merge the fix", "gitlab_mr_merge", "internal")
	seedPlanEscalation(f, "gd-esc2", "gd-step1", "Grant the IAM role", "aws_iam", "internal")

	card := planCardHTML(t, f)

	if !strings.Contains(card, "plan-esc-badge") {
		t.Fatalf("expected the ⛔ escalation badge to render on a live plan with 2 open escalations")
	}
	// The two escalation titles must appear on SEPARATE lines inside the tooltip —
	// i.e. a newline sits between them. If a stray trailing trim ate the {{range}}
	// body newline, the titles would be adjacent on one line.
	both := regexp.MustCompile(`(?s)Approve AND merge the fix.*?\n.*?Grant the IAM role`)
	if !both.MatchString(card) {
		t.Errorf("escalation tooltip titles are not newline-separated — a trim marker ate the {{range}} body newline (the multi-line tooltip trap)")
	}
	// Belt-and-suspenders: the two titles must NOT be jammed onto one line with no
	// newline between them (the exact bad-trim failure mode).
	jammed := regexp.MustCompile(`Approve AND merge the fix[^\n]*Grant the IAM role`)
	if jammed.MatchString(card) {
		t.Errorf("escalation titles are jammed onto one line — the {{range}} body newline was trimmed away")
	}
}
