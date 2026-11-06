// `gb widget validate` runs the full pre-publish gate against a widget-
// type bead (or a file). Strict mode enforces the publish checklist
// (ka-0000002102 §3.1):
//
//   - schema parses
//   - template parses
//   - lint findings = 0 (errors); warnings allowed
//   - sample payload validates against schema
//   - sample render succeeds (no panic)
//   - widget_version is set
//   - description has a `## Summary` section
//   - URL helpers are used (no literal /htmx/... strings — warning when --strict)
//
// Validate is idempotent and read-only. `gb widget publish` runs it
// internally as the final gate before flipping the stage label.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// validateResult is the structured findings shape (one entry per check
// that ran) for --json output.
type validateResult struct {
	Slug     string         `json:"slug"`
	Version  string         `json:"version,omitempty"`
	Findings []lintFinding  `json:"findings"`
	Strict   bool           `json:"strict"`
	OK       bool           `json:"ok"`
	Errors   int            `json:"errors"`
	Warnings int            `json:"warnings"`
	Extras   map[string]any `json:"extras,omitempty"`
}

// validateWidget runs every check and returns a structured result.
func validateWidget(doc *widgetDoc, strict bool) validateResult {
	findings := lintWidgetTemplate(doc)

	if strict && strings.TrimSpace(doc.Version) == "" {
		findings = append(findings, lintFinding{
			Rule: "widget-version", Severity: "error",
			Message: "widget_version must be set on a published widget-type",
		})
	}
	if strict && strings.TrimSpace(doc.Summary) == "" {
		findings = append(findings, lintFinding{
			Rule: "summary", Severity: "error",
			Message: "description requires a `## Summary` section",
		})
	}

	// Sample-payload schema check (treats invalid payload as fatal in
	// strict mode, advisory otherwise).
	if s := strings.TrimSpace(doc.Schema); s != "" && strings.TrimSpace(doc.Sample) != "" {
		if err := validatePayloadAgainstSchema([]byte(s), []byte(doc.Sample)); err != nil {
			sev := "warn"
			if strict {
				sev = "error"
			}
			findings = append(findings, lintFinding{
				Rule: "sample-schema", Severity: sev,
				Message: fmt.Sprintf("sample payload failed schema validation: %v", err),
			})
		}
	}

	// Sample render check.
	if strings.TrimSpace(doc.Template) != "" {
		var payload []byte
		if strings.TrimSpace(doc.Sample) != "" {
			payload = []byte(doc.Sample)
		}
		_, code, err := renderWidgetPreview(doc, payload, false)
		if err != nil && code != previewSchemaMismatch {
			findings = append(findings, lintFinding{
				Rule: "sample-render", Severity: "error",
				Message: fmt.Sprintf("sample render failed (exit code %d): %v", int(code), err),
			})
		}
	}

	res := validateResult{
		Slug:     doc.Slug,
		Version:  doc.Version,
		Findings: findings,
		Strict:   strict,
	}
	for _, f := range findings {
		if f.Severity == "error" {
			res.Errors++
		} else {
			res.Warnings++
		}
	}
	res.OK = res.Errors == 0
	return res
}

// ── widget validate command ───────────────────────────────────────────

var widgetValidateCmd = &cobra.Command{
	Use:   "validate <slug-or-id|file>",
	Short: "Run the full pre-publish validation gate against a widget-type",
	Long: `Run every check `+"`gb widget publish`"+` runs, but without flipping
the stage label. Use --strict to enforce the publish gate (widget_version
set, summary present, sample payload validates against schema, etc).

Output:
  - Default: human-readable findings on stderr; exit 0 on success.
  - --json:  structured result on stdout.

Combine with --json from CI / scripts; combine without to drive the
agent inner loop.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		doc, err := resolveWidgetInput(cmd, args[0])
		if err != nil {
			return err
		}
		strict, _ := cmd.Flags().GetBool("strict")
		emitJSON, _ := cmd.Flags().GetBool("json")

		res := validateWidget(doc, strict)
		if emitJSON || jsonOutput {
			b, err := json.MarshalIndent(res, "", "  ")
			if err != nil {
				return err
			}
			fmt.Println(string(b))
		} else {
			for _, f := range res.Findings {
				fmt.Fprintln(os.Stderr, f.String())
			}
			if res.OK {
				fmt.Fprintf(os.Stderr, "PASS — %d warning(s)\n", res.Warnings)
			} else {
				fmt.Fprintf(os.Stderr, "FAIL — %d error(s), %d warning(s)\n", res.Errors, res.Warnings)
			}
		}
		if !res.OK {
			os.Exit(1)
		}
		return nil
	},
}
