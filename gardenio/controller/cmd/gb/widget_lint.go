// `gb widget lint` runs fast, offline, static checks against a free-htmx
// widget template. Lint is the inner loop: agents iterate the template
// + schema + sample on disk and run lint after every save before they
// pay for a round trip to the kbeads daemon (`gb widget validate`) or
// the renderer (`gb widget preview`).
//
// Rules enforced (ka-0000000776 §"Validation: where it runs" layer 1 +
// ka-0000000873 §2 banned-attr surface):
//
//   - The template body parses under html/template.
//   - Banned attributes are absent: hx-on*, hx-include, hx-headers,
//     hx-ext, hx-vars, style=, srcdoc=, formaction=.
//   - Every literal hx-get / hx-post / hx-put / hx-delete URL either
//     starts with /htmx/ or is produced by a known URL helper
//     (panelURL / actionURL / apiURL / decisionOptionURL).
//   - Every hx-target is `this`, `closest …`, `find …`, or a
//     #widget-* / #w-* id (stable namespacing).
//   - The schema parses as JSON when present.
//   - The sample payload parses as JSON when present.
//
// Lint is advisory; daemon-side write hooks
// (kbeads/internal/server/widget_validation.go) are the authoritative
// gate. Lint exists to surface the same failures one round-trip earlier.
package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"os"
	"regexp"
	"strings"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

// lintFinding is one violation surfaced by `gb widget lint`. Severity is
// "error" (fatal) or "warn" (advisory). Line is 1-based; 0 when the
// rule applies to the whole document.
type lintFinding struct {
	Rule     string `json:"rule"`
	Severity string `json:"severity"`
	Line     int    `json:"line,omitempty"`
	Message  string `json:"message"`
}

func (f lintFinding) String() string {
	loc := ""
	if f.Line > 0 {
		loc = fmt.Sprintf(":%d", f.Line)
	}
	return fmt.Sprintf("[%s] %s%s — %s", f.Severity, f.Rule, loc, f.Message)
}

// bannedAttrPattern matches every banned attribute as a single word; the
// match is case-insensitive and anchored to a word boundary so attribute
// names such as `data-style-foo` don't false-positive on `style=`.
var bannedAttrPattern = regexp.MustCompile(`(?i)(?:^|[\s/<])(hx-on[a-z:\-]*|hx-include|hx-headers|hx-ext|hx-vars|style|srcdoc|formaction)\s*=`)

// hxURLAttrPattern captures every literal URL passed to an hx-get /
// hx-post / hx-put / hx-delete attribute. Group 1 is the attribute name,
// group 2 is the URL value (stripped of surrounding quotes).
var hxURLAttrPattern = regexp.MustCompile(`(?i)(hx-get|hx-post|hx-put|hx-delete)\s*=\s*"([^"]*)"`)

// hxTargetPattern captures every hx-target value.
var hxTargetPattern = regexp.MustCompile(`(?i)hx-target\s*=\s*"([^"]*)"`)

// urlHelperPattern matches a Go template expression that calls one of
// the URL helpers documented in ka-0000007136 §5.2. Whitespace is
// permissive.
var urlHelperPattern = regexp.MustCompile(`\{\{\s*(panelURL|actionURL|apiURL|decisionOptionURL)\b`)

// allowedTargetPrefixes are the static target-selector forms approved by
// the runbook (§ Stage 2 — Lint).
var allowedTargetPrefixes = []string{
	"this",
	"closest ",
	"find ",
	"#widget-",
	"#w-",
}

// lintWidgetTemplate runs all static checks over a template body, schema
// blob and sample payload. Empty schema / sample / template are tolerated
// (caller decides whether to escalate to error).
func lintWidgetTemplate(doc *widgetDoc) []lintFinding {
	var findings []lintFinding

	// 1. Template parse.
	if strings.TrimSpace(doc.Template) == "" {
		findings = append(findings, lintFinding{
			Rule: "template-missing", Severity: "error",
			Message: "no `## Template` section found (or empty body)",
		})
	} else {
		name := doc.Slug
		if name == "" {
			name = "widget"
		}
		if _, err := template.New(name).Funcs(previewFuncMap()).Parse(doc.Template); err != nil {
			findings = append(findings, lintFinding{
				Rule: "template-parse", Severity: "error",
				Message: err.Error(),
			})
		}
	}

	// 2. Banned attributes.
	for i, line := range strings.Split(doc.Template, "\n") {
		if m := bannedAttrPattern.FindStringSubmatch(line); m != nil {
			findings = append(findings, lintFinding{
				Rule: "banned-attr", Severity: "error", Line: i + 1,
				Message: fmt.Sprintf("attribute %q is forbidden by FreeHtmxPolicy (ka-0000000873 §2)", strings.ToLower(m[1])),
			})
		}
	}

	// 3. URL prefix on hx-* attributes.
	for i, line := range strings.Split(doc.Template, "\n") {
		for _, m := range hxURLAttrPattern.FindAllStringSubmatch(line, -1) {
			attr, url := m[1], strings.TrimSpace(m[2])
			if urlHelperPattern.MatchString(url) {
				continue
			}
			if !strings.HasPrefix(url, "/htmx/") {
				findings = append(findings, lintFinding{
					Rule: "url-prefix", Severity: "error", Line: i + 1,
					Message: fmt.Sprintf("%s value %q must begin with /htmx/ or use a URL helper (panelURL / actionURL / apiURL / decisionOptionURL)", attr, url),
				})
				continue
			}
			// Soft-warn on literal /htmx/... — runbook §3 prefers helpers.
			findings = append(findings, lintFinding{
				Rule: "url-prefix-helper", Severity: "warn", Line: i + 1,
				Message: fmt.Sprintf("%s uses a literal /htmx/ URL — prefer a URL helper for compile-time checking (ka-0000007136 §5.2)", attr),
			})
		}
	}

	// 4. Stable hx-target selectors.
	for i, line := range strings.Split(doc.Template, "\n") {
		for _, m := range hxTargetPattern.FindAllStringSubmatch(line, -1) {
			val := strings.TrimSpace(m[1])
			if val == "" {
				continue
			}
			ok := false
			for _, p := range allowedTargetPrefixes {
				if val == p || strings.HasPrefix(val, p) {
					ok = true
					break
				}
			}
			if !ok {
				findings = append(findings, lintFinding{
					Rule: "hx-target", Severity: "error", Line: i + 1,
					Message: fmt.Sprintf("hx-target %q must be `this`, `closest …`, `find …`, or a #widget-/#w- id", val),
				})
			}
		}
	}

	// 5. Schema parses.
	if s := strings.TrimSpace(doc.Schema); s != "" {
		var v any
		if err := json.Unmarshal([]byte(s), &v); err != nil {
			findings = append(findings, lintFinding{
				Rule: "schema-parse", Severity: "error",
				Message: fmt.Sprintf("schema is not valid JSON: %v", err),
			})
		}
	}

	// 6. Sample payload parses.
	if s := strings.TrimSpace(doc.Sample); s != "" {
		var v any
		if err := json.Unmarshal([]byte(s), &v); err != nil {
			findings = append(findings, lintFinding{
				Rule: "sample-parse", Severity: "error",
				Message: fmt.Sprintf("sample payload is not valid JSON: %v", err),
			})
		}
	}

	return findings
}

// printFindings emits findings in either human or JSON form. Returns the
// number of error-severity findings (i.e. exit-code-relevant).
func printFindings(findings []lintFinding, asJSON bool) int {
	errs := 0
	for _, f := range findings {
		if f.Severity == "error" {
			errs++
		}
	}
	if asJSON {
		printJSON(map[string]any{
			"findings": findings,
			"errors":   errs,
			"warnings": len(findings) - errs,
		})
		return errs
	}
	if len(findings) == 0 {
		fmt.Println("OK — no findings.")
		return 0
	}
	for _, f := range findings {
		fmt.Fprintln(os.Stderr, f.String())
	}
	fmt.Fprintf(os.Stderr, "\n%d finding(s) (%d error, %d warning)\n",
		len(findings), errs, len(findings)-errs)
	return errs
}

// ── widget lint command ────────────────────────────────────────────────

var widgetLintCmd = &cobra.Command{
	Use:   "lint <slug-or-id|file>",
	Short: "Run fast static checks against a free-htmx widget template",
	Long: `Lint a free-htmx widget template against the FreeHtmxPolicy contract.

Three input modes:

  gb widget lint /tmp/deploy-lag.md      — local file (4-section format)
  gb widget lint -                       — read from stdin
  gb widget lint <slug>                  — load the widget-type bead by slug
  gb widget lint ka-XYZ                  — load by bead id

Lint enforces every rule the FreeHtmxPolicy sanitizer would later
enforce, so a clean lint avoids surprise rejections at preview / publish
time. Findings print to stderr; exit code is the number of error-
severity findings.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		doc, err := resolveWidgetInput(cmd, args[0])
		if err != nil {
			return err
		}
		findings := lintWidgetTemplate(doc)
		errs := printFindings(findings, jsonOutput)
		if errs > 0 {
			os.Exit(1)
		}
		return nil
	},
}

// resolveWidgetInput is the shared "is this a file path, stdin, or a
// bead slug/id?" resolver used by lint / preview / validate. The rule
// is: path containing `/`, `\`, ending in `.md`/`.html`/`.tmpl`, or
// starting with `.` is a file. `-` is stdin. Anything starting with
// `gd-` is a bead id; otherwise a widget-type slug.
func resolveWidgetInput(cmd *cobra.Command, arg string) (*widgetDoc, error) {
	if arg == "-" || isFileyArg(arg) {
		return loadWidgetDoc(arg)
	}
	if (strings.HasPrefix(arg, "gd-") || strings.HasPrefix(arg, "ka-")) {
		bead, err := daemon.GetBead(cmd.Context(), arg)
		if err != nil {
			return nil, fmt.Errorf("loading widget-type bead %s: %w", arg, err)
		}
		return widgetDocFromBead(bead), nil
	}
	all, err := listWidgetTypesBySlug(cmd, arg)
	if err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return nil, fmt.Errorf("no widget-type bead with slug %q (and %q is not a file path)", arg, arg)
	}
	return widgetDocFromBead(pickWidgetTypeVersion(all, "")), nil
}

func isFileyArg(arg string) bool {
	if strings.ContainsAny(arg, "/\\") {
		return true
	}
	if strings.HasPrefix(arg, ".") {
		return true
	}
	for _, ext := range []string{".md", ".html", ".tmpl", ".tpl"} {
		if strings.HasSuffix(arg, ext) {
			return true
		}
	}
	return false
}

// widgetDocFromBead populates a widgetDoc from a stored widget-type bead.
// The schema and template come from typed fields.
func widgetDocFromBead(b *beadsapi.BeadDetail) *widgetDoc {
	if b == nil {
		return &widgetDoc{}
	}
	doc := &widgetDoc{
		Slug:     b.Fields["slug"],
		Version:  b.Fields["widget_version"],
		Summary:  b.Fields["summary"],
		Schema:   b.Fields["schema"],
		Template: b.Fields["template_source"],
	}
	if doc.Summary == "" {
		doc.Summary = b.Title
	}
	return doc
}
