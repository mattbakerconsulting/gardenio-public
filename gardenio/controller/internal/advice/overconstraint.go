package advice

// overconstraint.go implements the QUALITATIVE advice-hygiene checks behind
// `gb advice lint --over-constraint` — the durable regression guard for the
// Opus/Fable-5 context audit (P4 of epic ka-0000007337, task ka-0000004072,
// under the guardrails epic ka-0000006604).
//
// Where the targeting-hygiene checks in lint.go answer "will this bead be
// delivered to the right agents?", these checks answer a different question the
// byte/token budgets are blind to: "is this bead OVER-CONSTRAINED?" The "new
// rules of context engineering for Claude-5-generation models" favour trusting
// the model's judgment over stacking emphatic shout-directives, so text can fit
// every byte/token budget and still hurt a judgment-first model. This linter
// flags three over-constraint smells per bead:
//
//  1. rules-over-judgment framing — phrases that replace judgment with a
//     no-discretion absolute ("under no circumstances", "no exceptions",
//     "strictly forbidden", "follow it exactly", …).
//  2. directive-density spikes — too many emphatic imperative directives
//     (MUST / NEVER / DO NOT / ALWAYS / CRITICAL / …) for the bead's length.
//  3. system-prompt / tool-description redundancy — the bead restates a chunk
//     of the injected workflow-context (the config beads every agent already
//     reads), so it pays context twice for the same rule.
//
// All findings are WARN severity: they are smells to review, not delivery
// breakage. The durable CI enforcement is a frozen-baseline test that audits
// the committed advice snapshot through these same functions and fails when the
// finding count climbs (advice_over_constraint_budget_test.go), the direct
// analog of prime_directive_density_budget_test.go and real_catalog_budget_test.go.

import (
	"regexp"
	"sort"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// Over-constraint check identifiers, stable for --json consumers.
const (
	OverConstraintRulesOverJudgment = "over-constraint:rules-over-judgment"
	OverConstraintDirectiveDensity  = "over-constraint:directive-density"
	OverConstraintRedundancy        = "over-constraint:redundancy"
)

// OverConstraintFinding is one over-constraint smell flagged on one scanned
// unit — an advice bead, or one instruction surface of a gardener trigger bead.
// Line is the 1-indexed line within the scanned unit's line-text (the advice
// DESCRIPTION, or the trigger prompt surface) where the smell was found (0 for
// a whole-unit metric like directive density, or a title-only judgment hit).
// Snippet is the offending text (trimmed/elided), Rule is the human name of the
// constraint the unit trips, and Message is the actionable one-liner.
type OverConstraintFinding struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// Field names the sub-surface a finding came from when the scanned unit has
	// more than one (a gardener trigger's "prompt" / "terminal_goal" / "gardener.*"
	// segments). Empty for an advice bead, whose whole title+description is one
	// unit.
	Field    string   `json:"field,omitempty"`
	Labels   []string `json:"labels,omitempty"`
	Check    string   `json:"check"`
	Severity string   `json:"severity"`
	Line     int      `json:"line"`
	Snippet  string   `json:"snippet"`
	Rule     string   `json:"rule"`
	Message  string   `json:"message"`
	// DirectiveCount / DirectiveDensity are populated only for the
	// directive-density check so --json consumers can chart the trend.
	DirectiveCount   int     `json:"directive_count,omitempty"`
	DirectiveDensity float64 `json:"directive_density,omitempty"`
	// BaselineDelta / BaselineFloor are populated only when a directive-density
	// finding is diffed against a persisted baseline (DiffOverConstraintBaseline):
	// BaselineDelta classifies the surface (new / climbed / same) and
	// BaselineFloor is the recorded floor it was compared against (0 for a new
	// surface). Empty/zero when no baseline diff was run.
	BaselineDelta BaselineDelta `json:"baseline_delta,omitempty"`
	BaselineFloor int           `json:"baseline_floor,omitempty"`
}

// OverConstraintConfig holds the tunable thresholds for the three checks so the
// CLI and the frozen-baseline test agree on one source of truth. Get a
// production-tuned set from DefaultOverConstraintConfig.
type OverConstraintConfig struct {
	// Directive-density thresholds. A bead is flagged when its emphatic
	// directive count is >= HighCount (absolute bloat, flagged regardless of
	// length), OR its density is >= DensityPer1k AND its count is >=
	// DensityMinCount. The min-count arm keeps a short bead with only one or
	// two directives from tripping the density arm on a small denominator,
	// while still flagging a genuinely dense short bead (4+ shout-directives in
	// a couple of sentences is exactly the smell).
	HighCount       int
	DensityPer1k    float64
	DensityMinCount int

	// Redundancy thresholds. A bead is flagged when >= RedundancyFraction of
	// its ShingleK-word shingles also appear in the injected reference corpus,
	// provided the bead has >= RedundancyMinShingles shingles (so a one-line
	// bead that happens to echo a stock phrase isn't flagged).
	ShingleK              int
	RedundancyMinShingles int
	RedundancyFraction    float64
}

// DefaultOverConstraintConfig returns the production thresholds, calibrated
// 2026-07-25 against the committed advice snapshot (315 beads): the real corpus
// tops out at 13 directives / ~10.3 per-1k density / 0.125 reference overlap, so
// these thresholds surface the current worst offenders for review while leaving
// headroom below the corpus max — a NEW bead more over-constrained than today's
// worst trips the guard.
func DefaultOverConstraintConfig() OverConstraintConfig {
	return OverConstraintConfig{
		HighCount:       8,
		DensityPer1k:    6.0,
		DensityMinCount: 4,

		ShingleK:              6,
		RedundancyMinShingles: 40,
		RedundancyFraction:    0.30,
	}
}

// overConstraintMarkers is the emphatic-directive marker set, aligned with
// scripts/directive-density.py and prime_directive_density_budget_test.go: the
// PRIMARY set (MUST / NEVER / DO NOT / CRITICAL / Prohibited) extended with the
// other emphatic uppercase markers (ALWAYS / IMPORTANT / REQUIRED / WARNING).
// "DO NOT" is matched case-insensitively on NOT because these corpora
// overwhelmingly write the emphatic negative as "Do NOT" / "do NOT".
var overConstraintMarkers = []*regexp.Regexp{
	regexp.MustCompile(`\bMUST\b`),
	regexp.MustCompile(`\bNEVER\b`),
	regexp.MustCompile(`\b[Dd][Oo] NOT\b`),
	regexp.MustCompile(`\bALWAYS\b`),
	regexp.MustCompile(`\bCRITICAL\b`),
	regexp.MustCompile(`\bProhibited\b`),
	regexp.MustCompile(`\bIMPORTANT\b`),
	regexp.MustCompile(`\bREQUIRED\b`),
	regexp.MustCompile(`\bWARNING\b`),
}

// judgmentPhrase is one rules-over-judgment phrase pattern and its human name.
type judgmentPhrase struct {
	rule string
	re   *regexp.Regexp
}

// judgmentPhrases are the no-discretion absolutes that signal the author is
// replacing the model's judgment with a rigid rule. Matched case-insensitively.
// Kept deliberately conservative — each pattern is a phrase that, in an
// injected-every-session advice bead, reads as "do not think, just obey", the
// exact framing the Claude-5 context rules caution against. Rewrite the flagged
// rule to state the positive intent and the WHY, and let the model apply it.
var judgmentPhrases = []judgmentPhrase{
	{"under-no-circumstances", regexp.MustCompile(`(?i)under no circumstances`)},
	{"no-exceptions", regexp.MustCompile(`(?i)\bno exceptions\b`)},
	{"without-exception", regexp.MustCompile(`(?i)\bwithout exception\b`)},
	{"strictly-forbidden", regexp.MustCompile(`(?i)\bstrictly (forbidden|prohibited)\b`)},
	{"never-ever", regexp.MustCompile(`(?i)\bnever,? ever\b`)},
	{"you-must-always", regexp.MustCompile(`(?i)\byou must always\b`)},
	{"no-judgment", regexp.MustCompile(`(?i)do not (use|apply|exercise)[^.\n]{0,24}judg`)},
	{"follow-exactly", regexp.MustCompile(`(?i)follow (this|these|them|it)[^.\n]{0,24}exactly`)},
	{"no-discretion", regexp.MustCompile(`(?i)\b(are|is) (never )?(permitted|allowed) to\b`)},
}

// CountDirectiveMarkers returns the number of emphatic-directive markers in the
// text. Exported so the CLI and tests share one counting definition.
func CountDirectiveMarkers(text string) int {
	n := 0
	for _, re := range overConstraintMarkers {
		n += len(re.FindAllString(text, -1))
	}
	return n
}

// overConstraintText is the text scanned for a bead: title + description. The
// title is included because an emphatic directive in the hook line ("Every
// gardener MUST …") is the part injected into every session.
func overConstraintText(b *beadsapi.BeadDetail) string {
	if b.Title == "" {
		return b.Description
	}
	return b.Title + "\n" + b.Description
}

// wordRE splits text into lowercase alphanumeric words for shingling.
var wordRE = regexp.MustCompile(`[a-z0-9]+`)

// shingleWords lowercases and word-splits text.
func shingleWords(text string) []string {
	return wordRE.FindAllString(strings.ToLower(text), -1)
}

// shingleSet returns the set of k-word shingles (as joined strings) for the
// word slice. Fewer than k words yields an empty set.
func shingleSet(words []string, k int) map[string]struct{} {
	if k < 1 || len(words) < k {
		return map[string]struct{}{}
	}
	out := make(map[string]struct{}, len(words)-k+1)
	for i := 0; i+k <= len(words); i++ {
		out[strings.Join(words[i:i+k], " ")] = struct{}{}
	}
	return out
}

// firstLineMatching returns the 1-indexed line number and trimmed text of the
// first description line matching re, or (0, "") if none. Line numbering is
// over the DESCRIPTION only (what an author edits), matching author intuition.
func firstLineMatching(description string, re *regexp.Regexp) (int, string) {
	for i, ln := range strings.Split(description, "\n") {
		if re.MatchString(ln) {
			return i + 1, elide(strings.TrimSpace(ln), 120)
		}
	}
	return 0, ""
}

// firstLineWithAnyMarker returns the 1-indexed line and trimmed text of the
// first description line carrying any emphatic-directive marker.
func firstLineWithAnyMarker(description string) (int, string) {
	for i, ln := range strings.Split(description, "\n") {
		for _, re := range overConstraintMarkers {
			if re.MatchString(ln) {
				return i + 1, elide(strings.TrimSpace(ln), 120)
			}
		}
	}
	return 0, ""
}

// elide truncates s to at most n runes, appending an ellipsis when clipped.
func elide(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// LintOverConstraint runs the three over-constraint checks over the advice
// corpus and returns findings ordered by input position (a bead may yield
// several). Closed, expired, and fixture beads are skipped — they are never
// delivered, so their framing cannot reach an agent.
//
// refCorpus is the injected workflow-context text (the config beads
// bridge.EnsureConfigs seeds — the "system prompt" every agent already reads)
// used by the redundancy check. Pass "" to skip the redundancy check entirely —
// the delivery path and unit tests that do not have the config text handy call
// it that way, mirroring how a nil liveRoles skips the dead-role check in
// LintAdvice.
//
// A zero-value cfg is replaced with DefaultOverConstraintConfig so callers can
// pass OverConstraintConfig{} to mean "use defaults".
func LintOverConstraint(beads []*beadsapi.BeadDetail, refCorpus string, cfg OverConstraintConfig) []OverConstraintFinding {
	if cfg == (OverConstraintConfig{}) {
		cfg = DefaultOverConstraintConfig()
	}

	var refShingles map[string]struct{}
	if refCorpus != "" {
		refShingles = shingleSet(shingleWords(refCorpus), cfg.ShingleK)
	}

	var findings []OverConstraintFinding
	for _, b := range beads {
		if b == nil || b.Status == "closed" || IsExpired(b) || IsFixture(b) {
			continue
		}

		// An advice bead is one scanned unit: the whole-bead scan text is
		// title+description (an emphatic directive in the hook line is injected
		// too), while line/snippet reporting is anchored to the DESCRIPTION —
		// what an author edits. Field is empty (an advice bead has no
		// sub-surfaces). meta carries the bead identity onto each finding.
		meta := OverConstraintFinding{ID: b.ID, Title: b.Title, Labels: b.Labels}
		findings = append(findings,
			scanOverConstraint(overConstraintText(b), b.Description, refShingles, cfg, meta)...)
	}
	return findings
}

// scanOverConstraint runs the three over-constraint checks against ONE unit of
// text and returns its findings. It is the single shared implementation the
// advice-bead lint (LintOverConstraint) and the gardener-trigger-prompt lint
// (LintTriggerPromptOverConstraint) both call, so the two paths can never drift
// on what "over-constrained" means.
//
//   - scanText is the full text scored for the judgment-phrase and
//     directive-density checks (for advice: title+description; for a trigger
//     surface: the surface text itself).
//   - lineText is the text the reported Line/Snippet are anchored to — usually
//     the same as scanText, but the advice path passes the description alone so
//     line numbers match what an author edits (the title is line 0).
//   - refShingles is the pre-computed shingle set of the injected reference
//     corpus for the redundancy check; nil skips that check entirely.
//   - meta carries the scanned unit's identity (ID / Title / Labels / Field);
//     every returned finding is a copy of meta with the check-specific fields
//     filled in.
//
// The redundancy check shingles lineText (not scanText) so the title is not
// double-counted, matching the advice path's original behaviour.
func scanOverConstraint(scanText, lineText string, refShingles map[string]struct{}, cfg OverConstraintConfig, meta OverConstraintFinding) []OverConstraintFinding {
	var findings []OverConstraintFinding

	// 1. rules-over-judgment framing — one finding per distinct phrase pattern
	//    that matches (so a unit stacking several absolutes is flagged for
	//    each), pointing at the first line carrying it.
	for _, jp := range judgmentPhrases {
		loc := jp.re.FindStringIndex(scanText)
		if loc == nil {
			continue
		}
		line, snippet := firstLineMatching(lineText, jp.re)
		if line == 0 {
			// The match is outside lineText (e.g. in the advice title, which is
			// not part of the description line-text). Report the matched phrase
			// itself as the snippet at line 0.
			snippet = elide(strings.TrimSpace(scanText[loc[0]:loc[1]]), 120)
		}
		f := meta
		f.Check, f.Severity, f.Line, f.Snippet, f.Rule = OverConstraintRulesOverJudgment, LintWarn, line, snippet, jp.rule
		f.Message = "rules-over-judgment framing (" + jp.rule + ") — state the positive intent and the WHY, then trust the model to apply it, rather than an absolute no-discretion command"
		findings = append(findings, f)
	}

	// 2. directive-density spike — a whole-unit metric (line points at the
	//    first directive-bearing line for context).
	count := CountDirectiveMarkers(scanText)
	tokens := len(scanText) / TokenBytesRatio
	density := 0.0
	if tokens > 0 {
		density = float64(count) / float64(tokens) * 1000
	}
	spike := count >= cfg.HighCount ||
		(density >= cfg.DensityPer1k && count >= cfg.DensityMinCount)
	if spike {
		line, snippet := firstLineWithAnyMarker(lineText)
		f := meta
		f.Check, f.Severity, f.Line, f.Snippet, f.Rule = OverConstraintDirectiveDensity, LintWarn, line, snippet, "directive-density"
		f.Message = "directive-density spike — too many emphatic imperatives for this unit's length; rewrite negatives as positive intent and drop redundant shout-directives"
		f.DirectiveCount, f.DirectiveDensity = count, round2(density)
		findings = append(findings, f)
	}

	// 3. system-prompt / tool-description redundancy.
	if refShingles != nil {
		unitShingles := shingleSet(shingleWords(lineText), cfg.ShingleK)
		if len(unitShingles) >= cfg.RedundancyMinShingles {
			overlap := 0
			for sh := range unitShingles {
				if _, ok := refShingles[sh]; ok {
					overlap++
				}
			}
			frac := float64(overlap) / float64(len(unitShingles))
			if frac >= cfg.RedundancyFraction {
				line, snippet := firstRedundantLine(lineText, refShingles, cfg.ShingleK)
				f := meta
				f.Check, f.Severity, f.Line, f.Snippet, f.Rule = OverConstraintRedundancy, LintWarn, line, snippet, "system-prompt-redundancy"
				f.Message = "restates the injected workflow-context (every agent already reads it) — replace the duplicated text with a one-line pointer instead of paying context twice"
				f.DirectiveDensity = round2(frac * 100) // percent overlap, for --json
				findings = append(findings, f)
			}
		}
	}
	return findings
}

// TriggerPromptSurface is one named instruction-text slice of a gardener trigger
// bead — the authored `prompt` / `terminal_goal` / `gardener.*` fields, NEVER
// the engine-owned run history. The caller (cmd/gb, which owns the bead schema)
// extracts these and hands them here so the advice package stays free of the
// trigger-bead field layout.
type TriggerPromptSurface struct {
	// ID / Title identify the trigger bead the surface belongs to.
	ID    string
	Title string
	// Labels are the bead's labels, copied onto each finding for filtering.
	Labels []string
	// Field names the sub-surface ("prompt", "terminal_goal", "gardener.name",
	// …) so a finding points at the exact authored field to edit.
	Field string
	// Text is the surface's instruction text, scored and line-anchored as one
	// unit (title-less: a trigger surface has no separate hook line).
	Text string
}

// LintTriggerPromptOverConstraint runs the SAME three over-constraint checks
// LintOverConstraint runs over advice beads — but over the instruction surfaces
// of gardener trigger prompts, the surface the P2 post-remediation re-audit
// (ka-0000002501) identified as the single hottest injected surface once
// init_prompts.go was de-constrained (finding N1, ka-0000004887). A gardener
// trigger prompt is injected into every gardener the trigger spawns, every
// cycle, so a shout-directive stack there is paid on a hot, high-frequency path
// the advice-only lint could never see (it scans type=advice beads, not the
// authored prompt field of type=trigger beads).
//
// Each surface is one scanned unit: its Text is both the scan text and the
// line-anchor text (a trigger surface has no separate title/hook line to fold
// in, unlike an advice bead). refCorpus is the injected workflow-context (the
// config beads every agent reads); pass "" to skip the redundancy check.
//
// A zero-value cfg is replaced with DefaultOverConstraintConfig, matching
// LintOverConstraint, so callers can pass OverConstraintConfig{} for defaults.
func LintTriggerPromptOverConstraint(surfaces []TriggerPromptSurface, refCorpus string, cfg OverConstraintConfig) []OverConstraintFinding {
	if cfg == (OverConstraintConfig{}) {
		cfg = DefaultOverConstraintConfig()
	}

	var refShingles map[string]struct{}
	if refCorpus != "" {
		refShingles = shingleSet(shingleWords(refCorpus), cfg.ShingleK)
	}

	var findings []OverConstraintFinding
	for _, s := range surfaces {
		if strings.TrimSpace(s.Text) == "" {
			continue
		}
		meta := OverConstraintFinding{ID: s.ID, Title: s.Title, Labels: s.Labels, Field: s.Field}
		findings = append(findings,
			scanOverConstraint(s.Text, s.Text, refShingles, cfg, meta)...)
	}
	return findings
}

// firstRedundantLine returns the 1-indexed line and trimmed text of the
// description line with the most reference-corpus shingle overlap — the most
// representative duplicated line to show the author.
func firstRedundantLine(description string, ref map[string]struct{}, k int) (int, string) {
	lines := strings.Split(description, "\n")
	bestLine, bestOverlap := 0, 0
	var bestText string
	for i, ln := range lines {
		sh := shingleSet(shingleWords(ln), k)
		overlap := 0
		for s := range sh {
			if _, ok := ref[s]; ok {
				overlap++
			}
		}
		if overlap > bestOverlap {
			bestOverlap, bestLine = overlap, i+1
			bestText = elide(strings.TrimSpace(ln), 120)
		}
	}
	return bestLine, bestText
}

// round2 rounds to two decimal places for stable JSON/log output.
func round2(f float64) float64 {
	return float64(int(f*100+0.5)) / 100
}

// SortOverConstraintFindings orders findings for stable, human-scannable
// output: by check, then by directive count / overlap descending (worst first
// within a check), then by bead ID. Callers that need input order should not
// call this.
func SortOverConstraintFindings(f []OverConstraintFinding) {
	sort.SliceStable(f, func(i, j int) bool {
		if f[i].Check != f[j].Check {
			return f[i].Check < f[j].Check
		}
		if f[i].DirectiveCount != f[j].DirectiveCount {
			return f[i].DirectiveCount > f[j].DirectiveCount
		}
		if f[i].ID != f[j].ID {
			return f[i].ID < f[j].ID
		}
		return f[i].Field < f[j].Field
	})
}
