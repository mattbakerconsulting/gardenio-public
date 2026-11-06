// coverage-patch is the W2b-2 command-line entry point: it consumes the
// merged LCOV from W3 (2a), a unified-diff between merge-base and HEAD,
// per-component baselines from W2b-6, and any Coverage-Bypass trailer
// in the MR commits (W2b-5), runs the patch-cov library (W2b-1), and
// emits a `patch-cov-result.json` artifact plus a process exit code
// (0 = pass / bypassed, 1 = fail). ci.yml's `go` job runs it on every
// ready PR, once per module arm, advisory (continue-on-error) for now;
// --summary appends the verdict to the job's step summary, which stands
// in for the GitLab comment coverage-bot used to post (gd-roE5roXBKQ).
// The rollout is in gardenio/docs/code-reduction-targets.md, "Ratchets".
//
// Spec: docs/research/coverage-phase2b-design.md §4.
//
// Synthetic-bypass support (§4.5): the harness invokes
// `--synthetic-bypass=tooling-broken --synthetic-detail="<msg>"` from
// any failure path that "couldn't compute" — empty LCOV, missing
// merge-base, dispatch broken — to flip the gate to a green pass with
// the bypass surfaced in the bot comment. We never silently auto-pass
// from inside the library: the harness is responsible for choosing
// when to apply a synthetic bypass.
//
// Failure-mode mapping (§4.5):
//
//	Failure                                  → CLI handling
//	-------------------------------------------------------------------------
//	merged LCOV missing/empty                → harness invokes with --synthetic-bypass=tooling-broken
//	merge-base/diff missing                  → harness invokes with --synthetic-bypass=tooling-broken
//	baselines.json missing                   → hard fail (exit 1) — operator must re-run W2b-6
//	patch-cov library panic                  → recovered to exit 2 with a stderr trace (loud)
//	per-component below threshold            → exit 1
//	per-component below floor                → exempt for that component
//
// Phase-1 advisory rollout (§4.4): the binary's exit code is the same in
// advisory and required modes — what changes is whether the workflow
// step carries continue-on-error. No --advisory flag here.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"gardenio/controller/internal/coverage/baselines"
	"gardenio/controller/internal/coverage/bypass"
	"gardenio/controller/internal/coverage/components"
	"gardenio/controller/internal/coverage/diff"
	"gardenio/controller/internal/coverage/lcov"
	"gardenio/controller/internal/coverage/patch"
)

// ResultJSON is the on-disk schema written to --output. Consumed by the
// W2b-3 PR comment bot (`gb coverage-comment`). Stable contract — bump
// SchemaVersion if a field is removed or its semantics change.
type ResultJSON struct {
	SchemaVersion  int             `json:"schema_version"`
	ComputedAt     time.Time       `json:"computed_at"`
	Pass           bool            `json:"pass"`
	Components     []ComponentJSON `json:"components"`
	Files          []FileJSON      `json:"files"`
	Bypass         *BypassJSON     `json:"bypass,omitempty"`
	UnknownReasons []string        `json:"bypass_unknown_reasons,omitempty"`
}

type ComponentJSON struct {
	Component  string  `json:"component"`
	Executable int     `json:"executable_lines"`
	Covered    int     `json:"covered_lines"`
	Percent    float64 `json:"percent"`
	Threshold  float64 `json:"threshold"`
	Exempt     bool    `json:"exempt"`
	Pass       bool    `json:"pass"`
}

type FileJSON struct {
	Path       string  `json:"path"`
	Component  string  `json:"component"`
	Executable int     `json:"executable_lines"`
	Covered    int     `json:"covered_lines"`
	Percent    float64 `json:"percent"`
}

type BypassJSON struct {
	Reason          string `json:"reason"`
	Author          string `json:"author,omitempty"`
	CommitSHA       string `json:"commit_sha,omitempty"`
	RawTrailer      string `json:"raw_trailer,omitempty"`
	Synthetic       bool   `json:"synthetic"`
	SyntheticDetail string `json:"synthetic_detail,omitempty"`
}

// commitInputJSON mirrors the GitLab MR commits API surface we care
// about. Only `id`, `author_name`, and `message` are read (case-tolerant
// alias `sha` accepted for command-line ergonomics).
type commitInputJSON struct {
	ID         string `json:"id"`
	SHA        string `json:"sha"`
	AuthorName string `json:"author_name"`
	Author     string `json:"author"`
	Message    string `json:"message"`
}

func main() {
	lcovPath := flag.String("lcov", "", "path to coverage.merged.lcov (W3 / 2a output)")
	diffPath := flag.String("diff", "", "path to unified diff (git diff --unified=0 <merge-base>...HEAD)")
	baselinesPath := flag.String("baselines", "", "path to baselines.json (cmd/coverage-baselines output; CI reads scripts/ratchets/coverage-baselines.json)")
	commitsJSONPath := flag.String("commits-json", "", "path to MR commits JSON ([{id, author_name, message}, ...]) — from GitLab API")
	outputPath := flag.String("output", "patch-cov-result.json", "where to write patch-cov-result.json")
	syntheticBypass := flag.String("synthetic-bypass", "", "if set (e.g. 'tooling-broken'), force-emit a synthetic bypass and exit 0; bypasses LCOV/diff parsing")
	syntheticDetail := flag.String("synthetic-detail", "", "free-form context for the synthetic bypass (surfaced in bot comment)")
	summaryPath := flag.String("summary", "", "if set, append a markdown verdict to this file (e.g. $GITHUB_STEP_SUMMARY)")
	summaryTitle := flag.String("summary-title", "Coverage / Patch", "heading for the --summary markdown")
	flag.Parse()

	defer recoverAndExit2()

	res, err := run(*lcovPath, *diffPath, *baselinesPath, *commitsJSONPath, *syntheticBypass, *syntheticDetail)
	if err != nil {
		log.Fatalf("coverage-patch: %v", err)
	}

	if err := writeResult(*outputPath, res); err != nil {
		log.Fatalf("coverage-patch: write output: %v", err)
	}

	logHumanSummary(res)
	if *summaryPath != "" {
		if err := appendMarkdownSummary(*summaryPath, *summaryTitle, res); err != nil {
			log.Fatalf("coverage-patch: write summary: %v", err)
		}
	}

	if res.Pass {
		os.Exit(0)
	}
	os.Exit(1)
}

func run(lcovPath, diffPath, baselinesPath, commitsJSONPath, syntheticBypass, syntheticDetail string) (*ResultJSON, error) {
	if syntheticBypass != "" {
		return syntheticResult(syntheticBypass, syntheticDetail), nil
	}

	if lcovPath == "" {
		return nil, fmt.Errorf("--lcov is required (or pass --synthetic-bypass for the tooling-broken path)")
	}
	if diffPath == "" {
		return nil, fmt.Errorf("--diff is required")
	}
	if baselinesPath == "" {
		return nil, fmt.Errorf("--baselines is required (W2b-6 output) — see docs/research/coverage-phase2b-design.md §4.5")
	}

	mergedLCOV, err := lcov.ParseFile(lcovPath)
	if err != nil {
		return nil, fmt.Errorf("parse lcov %s: %w", lcovPath, err)
	}
	df, err := diff.ParseFile(diffPath)
	if err != nil {
		return nil, fmt.Errorf("parse diff %s: %w", diffPath, err)
	}
	pBaselines, err := loadBaselines(baselinesPath)
	if err != nil {
		return nil, fmt.Errorf("load baselines %s: %w", baselinesPath, err)
	}

	pBypass, unknownReasons, err := loadBypass(commitsJSONPath)
	if err != nil {
		// Per bypass package contract: any parse error means "no bypass
		// granted; gate evaluates normally". We surface the reason in
		// the output JSON for the bot comment but DO NOT short-circuit
		// the gate.
		log.Printf("coverage-patch: bypass parse warning: %v (treating as no bypass)", err)
	}

	result := patch.Compute(mergedLCOV, df, pBaselines, pBypass)
	return toResultJSON(result, unknownReasons, false, ""), nil
}

func writeResult(path string, res *ResultJSON) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(res); err != nil {
		return err
	}
	return f.Close()
}

// syntheticResult builds a green pass with a synthetic bypass record.
// Used by --synthetic-bypass to handle the §4.5 "couldn't compute"
// failure modes (no LCOV, no merge-base, dispatch broken).
func syntheticResult(reason, detail string) *ResultJSON {
	return &ResultJSON{
		SchemaVersion: 1,
		ComputedAt:    time.Now().UTC(),
		Pass:          true,
		Components:    []ComponentJSON{},
		Files:         []FileJSON{},
		Bypass: &BypassJSON{
			Reason:          reason,
			Synthetic:       true,
			SyntheticDetail: detail,
		},
	}
}

func loadBaselines(path string) (*patch.Baselines, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out baselines.Output
	if err := json.NewDecoder(f).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	pb := &patch.Baselines{Components: map[components.Component]patch.ComponentBaseline{}}
	for c, cb := range out.Components {
		pb.Components[c] = patch.ComponentBaseline{
			BaselinePct:  cb.BaselinePct,
			ThresholdPct: cb.ThresholdPct,
		}
	}
	return pb, nil
}

func loadBypass(path string) (*patch.Bypass, []string, error) {
	if path == "" {
		return nil, nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(f)
	if err != nil {
		return nil, nil, err
	}
	var commits []commitInputJSON
	if err := json.Unmarshal(raw, &commits); err != nil {
		return nil, nil, fmt.Errorf("decode commits json: %w", err)
	}

	bcommits := make([]bypass.Commit, 0, len(commits))
	for _, c := range commits {
		sha := c.ID
		if sha == "" {
			sha = c.SHA
		}
		author := c.AuthorName
		if author == "" {
			author = c.Author
		}
		bcommits = append(bcommits, bypass.Commit{
			SHA:     sha,
			Author:  author,
			Message: c.Message,
		})
	}

	parsed, perr := bypass.Parse(bcommits)
	if perr != nil {
		return nil, nil, perr
	}
	if parsed == nil {
		return nil, nil, nil
	}
	return &patch.Bypass{
		Reason:     string(parsed.Reason),
		Author:     parsed.Author,
		CommitSHA:  parsed.CommitSHA,
		RawTrailer: parsed.RawTrailer,
	}, parsed.UnknownReasons, nil
}

// toResultJSON projects patch.Result onto the on-disk schema, sanitizing
// NaN/Inf so downstream JSON consumers don't have to special-case them.
// A NaN percent (denominator 0) becomes 0.0 — the Exempt flag carries
// the "no signal" semantics, not the percent.
func toResultJSON(r patch.Result, unknownReasons []string, synthetic bool, syntheticDetail string) *ResultJSON {
	out := &ResultJSON{
		SchemaVersion:  1,
		ComputedAt:     time.Now().UTC(),
		Pass:           r.Pass,
		UnknownReasons: unknownReasons,
	}
	for _, c := range r.Components {
		out.Components = append(out.Components, ComponentJSON{
			Component:  string(c.Component),
			Executable: c.Executable,
			Covered:    c.Covered,
			Percent:    sanitize(c.Percent),
			Threshold:  c.Threshold,
			Exempt:     c.Exempt,
			Pass:       c.Pass,
		})
	}
	for _, f := range r.Files {
		out.Files = append(out.Files, FileJSON{
			Path:       f.Path,
			Component:  string(f.Component),
			Executable: f.Executable,
			Covered:    f.Covered,
			Percent:    sanitize(f.Percent),
		})
	}
	if r.Bypass != nil {
		out.Bypass = &BypassJSON{
			Reason:          r.Bypass.Reason,
			Author:          r.Bypass.Author,
			CommitSHA:       r.Bypass.CommitSHA,
			RawTrailer:      r.Bypass.RawTrailer,
			Synthetic:       synthetic,
			SyntheticDetail: syntheticDetail,
		}
	}
	if out.Components == nil {
		out.Components = []ComponentJSON{}
	}
	if out.Files == nil {
		out.Files = []FileJSON{}
	}
	return out
}

func sanitize(x float64) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return 0
	}
	return x
}

// logHumanSummary writes a short summary to stdout — CI surfaces job
// stdout in the run log so operators can see the gate result without
// downloading the JSON artifact.
func logHumanSummary(r *ResultJSON) {
	verdict := "PASS"
	if !r.Pass {
		verdict = "FAIL"
	}
	fmt.Printf("Coverage / Patch: %s\n", verdict)
	if r.Bypass != nil {
		marker := "real"
		if r.Bypass.Synthetic {
			marker = "synthetic"
		}
		fmt.Printf("  bypass: %s (%s)", r.Bypass.Reason, marker)
		if r.Bypass.SyntheticDetail != "" {
			fmt.Printf(" — %s", r.Bypass.SyntheticDetail)
		}
		fmt.Println()
	}
	for _, c := range r.Components {
		var status string
		switch {
		case c.Exempt:
			status = "exempt (under floor)"
		case c.Pass:
			status = fmt.Sprintf("PASS  %.1f%% ≥ %.1f%%", c.Percent, c.Threshold)
		default:
			status = fmt.Sprintf("FAIL  %.1f%% < %.1f%%", c.Percent, c.Threshold)
		}
		fmt.Printf("  %-12s exec=%d covered=%d  %s\n", c.Component, c.Executable, c.Covered, status)
	}
	for _, w := range r.UnknownReasons {
		fmt.Printf("  ignored unknown bypass reason: %q\n", w)
	}
}

// summaryMaxFiles caps the lowest-coverage file list in the markdown
// summary; the full list is in the JSON artifact.
const summaryMaxFiles = 10

// appendMarkdownSummary appends the verdict as markdown to path, created
// if absent. GitHub renders $GITHUB_STEP_SUMMARY on the run page, so this
// is where a reviewer reads the gate. Components with no changed
// executable lines are left out: in a per-module arm they belong to the
// other arm's LCOV and would only read as noise.
func appendMarkdownSummary(path, title string, r *ResultJSON) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.WriteString(f, markdownSummary(title, r)); err != nil {
		return err
	}
	return f.Close()
}

func markdownSummary(title string, r *ResultJSON) string {
	var b strings.Builder
	verdict := "PASS"
	if !r.Pass {
		verdict = "FAIL"
	}
	fmt.Fprintf(&b, "### %s: %s\n\n", title, verdict)
	if r.Bypass != nil {
		fmt.Fprintf(&b, "Bypassed: `%s`", r.Bypass.Reason)
		if r.Bypass.SyntheticDetail != "" {
			fmt.Fprintf(&b, " (%s)", r.Bypass.SyntheticDetail)
		}
		b.WriteString("\n\n")
	}
	var rows []ComponentJSON
	for _, c := range r.Components {
		if c.Executable > 0 {
			rows = append(rows, c)
		}
	}
	if len(rows) == 0 {
		b.WriteString("No changed executable lines in this LCOV.\n\n")
		return b.String()
	}
	b.WriteString("| component | changed lines | covered | patch | threshold | result |\n|---|---:|---:|---:|---:|---|\n")
	for _, c := range rows {
		status := "pass"
		switch {
		case c.Exempt:
			status = fmt.Sprintf("exempt (under %d lines)", patch.LineFloor)
		case !c.Pass:
			status = "**fail**"
		}
		fmt.Fprintf(&b, "| %s | %d | %d | %.1f%% | %.1f%% | %s |\n",
			c.Component, c.Executable, c.Covered, c.Percent, c.Threshold, status)
	}
	b.WriteString("\n")
	var low []FileJSON
	for _, fl := range r.Files {
		if fl.Covered < fl.Executable {
			low = append(low, fl)
		}
	}
	if len(low) > 0 {
		b.WriteString("Changed lines not covered, lowest first:\n\n| file | covered / changed |\n|---|---:|\n")
		for i, fl := range low {
			if i == summaryMaxFiles {
				fmt.Fprintf(&b, "| … %d more in patch-cov-result.json | |\n", len(low)-i)
				break
			}
			fmt.Fprintf(&b, "| `%s` | %d / %d |\n", fl.Path, fl.Covered, fl.Executable)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// recoverAndExit2 turns a library panic into exit 2 with a stderr trace.
// Per §4.5: "Patch-cov library panic → CI job fails (exit 2) which
// surfaces as Coverage / Patch red with a stderr trace. We want this
// loud — library bugs must not silently auto-pass."
func recoverAndExit2() {
	if rec := recover(); rec != nil {
		fmt.Fprintf(os.Stderr, "coverage-patch: PANIC: %v\n", rec)
		fmt.Fprintln(os.Stderr, string(debug.Stack()))
		os.Exit(2)
	}
}
