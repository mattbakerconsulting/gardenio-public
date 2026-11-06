// `gb widget preview` renders a free-htmx widget against a sample
// payload. Output goes to stdout (default) or a one-shot beads-viewer
// URL when --browser is set.
//
// Exit codes (ka-0000002102 §3.1):
//
//	0 — render OK; sanitizer made no rejections.
//	1 — template compile error.
//	2 — render-time error (nil-deref, range over nil, …).
//	3 — sanitizer stripped one or more elements / attributes.
//	4 — payload failed schema validation.
//
// The sanitizer integration is wired through `internal/widgets` once
// FreeHtmxPolicy is plumbed (ka-0000002043 in flight). Until then,
// `--no-sanitize` is the default in CLI preview and a warning is
// printed; the comment surface in P2 will land the policy.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"os"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v5"
	"github.com/spf13/cobra"
)

// previewExitCode is the numeric exit code matching the runbook table.
type previewExitCode int

const (
	previewOK             previewExitCode = 0
	previewCompileError   previewExitCode = 1
	previewRenderError    previewExitCode = 2
	previewSanitized      previewExitCode = 3
	previewSchemaMismatch previewExitCode = 4
)

// previewTemplateScope mirrors the prod free-htmx template scope used by
// `compileHtmxRenderer` in `cmd/beads-viewer/widgets_htmx.go`. Keeping the
// two structurally identical (Payload map + Widget meta) is what lets a
// template authored against `.Payload.<field>` render the same way under
// `gb widget preview` and under the prod rich-report dispatcher
// (ka-0000005250). Adding a field here requires the matching field on the
// prod `htmxTemplateScope`.
type previewTemplateScope struct {
	Payload map[string]any
	Widget  previewWidgetMeta
}

// previewWidgetMeta parallels prod `htmxWidgetMeta`: just the widget-instance
// id so templates can emit stable target ids. Under preview there is no real
// instance, so we use the doc slug as a stand-in.
type previewWidgetMeta struct {
	ID string
}

// renderWidgetPreview renders doc.Template against payload and returns
// the rendered HTML, the exit code, and a human message. The function
// is side-effect-free so tests can drive it without touching stdout.
func renderWidgetPreview(doc *widgetDoc, payload []byte, schemaCheck bool) (string, previewExitCode, error) {
	// 1. Schema validation (when both schema and payload are present).
	if schemaCheck && strings.TrimSpace(doc.Schema) != "" && len(payload) > 0 {
		if err := validatePayloadAgainstSchema([]byte(doc.Schema), payload); err != nil {
			return "", previewSchemaMismatch, fmt.Errorf("payload failed schema validation: %v", err)
		}
	}

	// 2. Compile.
	name := doc.Slug
	if name == "" {
		name = "widget"
	}
	tpl, err := template.New(name).Funcs(previewFuncMap()).Parse(doc.Template)
	if err != nil {
		return "", previewCompileError, fmt.Errorf("template parse error: %v", err)
	}

	// 3. Decode payload into a map and wrap in the same scope shape prod
	// uses, so `.Payload.<field>` resolves identically here and in
	// `compileHtmxRenderer`. An empty payload becomes an empty map; a
	// non-object root payload is reported as a schema mismatch (matching
	// prod `decodePayload`, which requires a JSON object).
	payloadMap := map[string]any{}
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &payloadMap); err != nil {
			return "", previewSchemaMismatch, fmt.Errorf("payload is not a JSON object: %v", err)
		}
		if payloadMap == nil {
			payloadMap = map[string]any{}
		}
	}
	scope := previewTemplateScope{
		Payload: payloadMap,
		Widget:  previewWidgetMeta{ID: doc.Slug},
	}

	// 4. Wrap in a render-time recover to catch nil-derefs / range-nil.
	var buf bytes.Buffer
	renderErr := func() (renderErr error) {
		defer func() {
			if r := recover(); r != nil {
				renderErr = fmt.Errorf("template render panicked: %v", r)
			}
		}()
		return tpl.Execute(&buf, scope)
	}()
	if renderErr != nil {
		return "", previewRenderError, renderErr
	}

	return buf.String(), previewOK, nil
}

// validatePayloadAgainstSchema is a thin wrapper around
// santhosh-tekuri/jsonschema/v5 — kbeads ships the same library on the
// daemon side (ka-0000007359) so CLI behaviour matches the daemon.
func validatePayloadAgainstSchema(schemaRaw, payloadRaw []byte) error {
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("widget://schema.json", strings.NewReader(string(schemaRaw))); err != nil {
		return fmt.Errorf("invalid schema: %v", err)
	}
	sch, err := compiler.Compile("widget://schema.json")
	if err != nil {
		return fmt.Errorf("schema compile: %v", err)
	}
	var payload any
	if err := json.Unmarshal(payloadRaw, &payload); err != nil {
		return fmt.Errorf("invalid payload JSON: %v", err)
	}
	return sch.Validate(payload)
}

// previewFuncMap exposes the URL helpers documented in
// ka-0000007136 §5.2 as no-op stubs so a template body that uses them
// at least renders correctly under preview. The real renderer-side
// helpers live alongside the htmx route surface (P3 / P5) and produce
// production URLs.
func previewFuncMap() template.FuncMap {
	return template.FuncMap{
		"panelURL": func(beadID, name string) string {
			return fmt.Sprintf("/htmx/bead/%s/panel/%s", beadID, name)
		},
		"actionURL": func(name string) string {
			return fmt.Sprintf("/htmx/action/%s", name)
		},
		"apiURL": func(parts ...string) string {
			return "/htmx/api/" + strings.Join(parts, "/")
		},
		"decisionOptionURL": func(decisionID, optionID string) string {
			return fmt.Sprintf("/htmx/decision/%s/option/%s", decisionID, optionID)
		},
		"default": func(fallback, val any) any {
			if val == nil {
				return fallback
			}
			if s, ok := val.(string); ok && s == "" {
				return fallback
			}
			return val
		},
	}
}

// readPayloadFlag reads a --payload flag value into raw JSON bytes. If
// flagValue is empty, falls back to doc.Sample.
func readPayloadFlag(flagValue string, doc *widgetDoc) ([]byte, error) {
	if flagValue == "" {
		if strings.TrimSpace(doc.Sample) != "" {
			return []byte(doc.Sample), nil
		}
		return nil, nil
	}
	return readDataArg(flagValue)
}

// ── widget preview command ─────────────────────────────────────────────

var widgetPreviewCmd = &cobra.Command{
	Use:   "preview <slug-or-id|file>",
	Short: "Render a free-htmx widget template against a sample payload",
	Long: `Render a widget template + sample payload to HTML for inspection.

Three input modes (matching gb widget lint):

  gb widget preview /tmp/deploy-lag.md   — local file (4-section format)
  gb widget preview <slug>               — load by widget-type slug
  gb widget preview ka-XYZ               — load by bead id

Exit codes:
  0  render OK; sanitizer made no rejections
  1  template compile error
  2  render-time error
  3  sanitizer stripped one or more elements / attributes
  4  payload failed schema validation

Notes:
  --browser opens a one-shot URL on beads-viewer. When $BROWSER is unset
  the URL is printed for the operator to open manually.

  --no-sanitize is strictly diagnostic; the gardener audit rejects any
  closing artifact that still uses --no-sanitize.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		doc, err := resolveWidgetInput(cmd, args[0])
		if err != nil {
			return err
		}
		payloadFlag, _ := cmd.Flags().GetString("payload")
		out, _ := cmd.Flags().GetString("out")
		browser, _ := cmd.Flags().GetBool("browser")
		_, _ = cmd.Flags().GetBool("operator")
		noSanitize, _ := cmd.Flags().GetBool("no-sanitize")
		_, _ = cmd.Flags().GetBool("with-csp")

		payload, err := readPayloadFlag(payloadFlag, doc)
		if err != nil {
			return fmt.Errorf("reading --payload: %w", err)
		}

		if browser {
			return openWidgetPreviewBrowser(doc)
		}

		html, code, renderErr := renderWidgetPreview(doc, payload, true)
		if renderErr != nil {
			fmt.Fprintln(os.Stderr, renderErr.Error())
			os.Exit(int(code))
		}

		if !noSanitize {
			// Sanitizer integration depends on FreeHtmxPolicy
			// (ka-0000002043). When that lands, fold the sanitization step
			// here and return previewSanitized when the policy stripped
			// anything.
			fmt.Fprintln(os.Stderr, "warning: FreeHtmxPolicy sanitizer is not yet wired in this binary; preview HTML is unsanitized — track ka-0000002043.")
		} else {
			fmt.Fprintln(os.Stderr, "warning: --no-sanitize is strictly diagnostic; closing artifacts that use it will be rejected by the gardener audit.")
		}

		if out == "" || out == "-" {
			fmt.Print(html)
		} else if err := os.WriteFile(out, []byte(html), 0o644); err != nil {
			return fmt.Errorf("writing --out %q: %w", out, err)
		}
		return nil
	},
}

func openWidgetPreviewBrowser(doc *widgetDoc) error {
	if doc.Slug == "" {
		return fmt.Errorf("--browser requires a registered widget-type slug (file-only previews print to stdout)")
	}
	url := fmt.Sprintf("/widget-types/%s", doc.Slug)
	if base := strings.TrimSpace(os.Getenv("BEADS_VIEWER_URL")); base != "" {
		url = strings.TrimRight(base, "/") + url
	}
	if browser := os.Getenv("BROWSER"); browser != "" {
		fmt.Fprintf(os.Stderr, "would open %s with $BROWSER=%s\n", url, browser)
	}
	fmt.Println(url)
	return nil
}
