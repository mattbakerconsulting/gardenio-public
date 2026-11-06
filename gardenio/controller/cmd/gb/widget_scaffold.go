// `gb widget scaffold-htmx` prints a starter free-htmx widget-type bead
// body. Two flavours of output:
//
//   - default: write a 4-section Markdown artifact (Summary / Schema /
//     Sample payload / Template) to stdout or to --out.
//   - --commit: read a 4-section artifact from stdin (or --in) and create
//     the widget-type bead via the daemon. The bead lands at
//     widget-stage:draft, renderer_kind=htmx, template_engine=
//     go-html-template-v1.
//
// The scaffolded body is opinionated enough that `gb widget lint` and
// `gb widget preview` should pass against it before the agent edits a
// single character — gives a clean baseline to iterate from.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

// scaffoldField parses a "name:type" entry from --with-fields.
type scaffoldField struct {
	Name string
	Type string // string|integer|number|boolean|array|object
}

func parseScaffoldFields(spec string) ([]scaffoldField, error) {
	if strings.TrimSpace(spec) == "" {
		return nil, nil
	}
	out := make([]scaffoldField, 0, 4)
	for raw := range strings.SplitSeq(spec, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		name, typ, ok := strings.Cut(raw, ":")
		name = strings.TrimSpace(name)
		typ = strings.TrimSpace(typ)
		if !ok || name == "" {
			return nil, fmt.Errorf("invalid --with-fields entry %q (want name:type)", raw)
		}
		if typ == "" {
			typ = "string"
		}
		switch typ {
		case "string", "integer", "number", "boolean", "array", "object":
		default:
			return nil, fmt.Errorf("invalid --with-fields entry %q: unsupported type %q", raw, typ)
		}
		out = append(out, scaffoldField{Name: name, Type: typ})
	}
	return out, nil
}

// scaffoldHTMXDoc returns a 4-section markdown artifact for the given
// slug + flags. summary may be empty; flags control optional payload
// extras (--with-live, --with-action).
func scaffoldHTMXDoc(slug, summary string, fields []scaffoldField, withLive, withAction bool) string {
	if summary == "" {
		summary = fmt.Sprintf("TODO: short description of the %s widget.", slug)
	}

	// JSON Schema with required / properties.
	schema := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{},
		"properties":           map[string]any{},
	}
	required := schema["required"].([]string)
	props := schema["properties"].(map[string]any)
	if len(fields) == 0 {
		// Sensible default: a single headline string.
		fields = []scaffoldField{{Name: "headline", Type: "string"}}
	}
	for _, f := range fields {
		props[f.Name] = map[string]any{"type": f.Type}
		required = append(required, f.Name)
	}
	schema["required"] = required
	schemaJSON, _ := json.MarshalIndent(schema, "", "  ")

	// Sample payload populated with type-appropriate placeholders.
	sample := map[string]any{}
	for _, f := range fields {
		sample[f.Name] = scaffoldSampleValue(f.Type, f.Name)
	}
	sampleJSON, _ := json.MarshalIndent(sample, "", "  ")

	// Template body. Field refs are emitted as `.Payload.<field>` to match
	// the prod free-htmx scope (`htmxTemplateScope.Payload`) in
	// `cmd/beads-viewer/widgets_htmx.go`. This is the same shape `gb widget
	// preview` evaluates against (ka-0000005250), so a scaffolded widget
	// that lints + previews clean also renders correctly in prod with no
	// `.Payload`/`.<field>` retranslation.
	var tpl strings.Builder
	fmt.Fprintf(&tpl, `<section id="widget-%s" class="widget widget-%s">`+"\n", slug, slug)
	tpl.WriteString(`  <header class="widget-header">` + "\n")
	tpl.WriteString(`    <h3 class="widget-title">{{ .Payload.headline | default "" }}</h3>` + "\n")
	tpl.WriteString(`  </header>` + "\n")
	tpl.WriteString(`  <div class="widget-body">` + "\n")
	for _, f := range fields {
		fmt.Fprintf(&tpl, `    <div class="widget-row"><span class="widget-key">%s:</span> <span class="widget-val">{{ .Payload.%s }}</span></div>`+"\n",
			f.Name, f.Name)
	}
	tpl.WriteString(`  </div>` + "\n")
	if withLive {
		fmt.Fprintf(&tpl,
			`  <div hx-get="{{ apiURL "widget" "%s" "live" }}" hx-trigger="load, every 30s" hx-target="this" hx-swap="outerHTML"></div>`+"\n",
			slug)
	}
	if withAction {
		fmt.Fprintf(&tpl,
			`  <button hx-post="{{ actionURL "%s" }}" hx-target="closest .widget" hx-swap="outerHTML">Refresh</button>`+"\n",
			slug)
	}
	tpl.WriteString(`</section>` + "\n")

	var b strings.Builder
	fmt.Fprintf(&b, "# Widget: %s\n\n", slug)
	fmt.Fprintf(&b, "## Summary\n\n%s\n\n", summary)
	fmt.Fprintf(&b, "## Schema\n\n```json\n%s\n```\n\n", schemaJSON)
	fmt.Fprintf(&b, "## Sample payload\n\n```json\n%s\n```\n\n", sampleJSON)
	fmt.Fprintf(&b, "## Template\n\n```html\n%s```\n", tpl.String())
	return b.String()
}

func scaffoldSampleValue(typ, name string) any {
	switch typ {
	case "string":
		return fmt.Sprintf("example %s", name)
	case "integer":
		return 0
	case "number":
		return 0.0
	case "boolean":
		return false
	case "array":
		return []any{}
	case "object":
		return map[string]any{}
	}
	return nil
}

// ── widget scaffold-htmx command ───────────────────────────────────────

var widgetScaffoldHTMXCmd = &cobra.Command{
	Use:   "scaffold-htmx <slug>",
	Short: "Print a starter free-htmx widget-type bead body (or --commit it)",
	Long: `Generate a 4-section Markdown artifact (Summary / Schema / Sample
payload / Template) for a free-htmx widget. Use --out to write to a file,
or --commit to create the widget-type bead from an already-edited file
on stdin.

Examples:
  # Print to stdout, redirect to a file the agent will edit.
  gb widget scaffold-htmx deploy-lag \
      --summary "Time-since-last-deploy + lag warnings" \
      --with-fields "service:string,lag_minutes:integer,last_deploy_at:string" \
      --with-live > /tmp/deploy-lag.md

  # Commit the edited artifact as a widget-type bead.
  gb widget scaffold-htmx deploy-lag --commit < /tmp/deploy-lag.md`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		slug := strings.TrimSpace(args[0])
		if slug == "" {
			return fmt.Errorf("slug must be non-empty")
		}
		commit, _ := cmd.Flags().GetBool("commit")
		out, _ := cmd.Flags().GetString("out")
		in, _ := cmd.Flags().GetString("in")
		summary, _ := cmd.Flags().GetString("summary")
		fieldsSpec, _ := cmd.Flags().GetString("with-fields")
		withLive, _ := cmd.Flags().GetBool("with-live")
		withAction, _ := cmd.Flags().GetBool("with-action")
		fromSlug, _ := cmd.Flags().GetString("from")
		project, _ := cmd.Flags().GetString("project")
		version, _ := cmd.Flags().GetString("version")

		// Only --commit creates a widget-type bead; printing a starter
		// artifact to stdout writes nothing, so it stays ungated.
		if commit {
			if err := checkWidgetDeprecated(cmd, "widget-type"); err != nil {
				return err
			}
			return commitScaffoldedWidget(cmd, slug, in, project, version)
		}

		if fromSlug != "" {
			return scaffoldFromExisting(cmd, slug, fromSlug, summary, out)
		}

		fields, err := parseScaffoldFields(fieldsSpec)
		if err != nil {
			return err
		}
		body := scaffoldHTMXDoc(slug, summary, fields, withLive, withAction)
		return writeScaffoldOutput(body, out)
	},
}

func writeScaffoldOutput(body, out string) error {
	if out == "" || out == "-" {
		_, err := io.WriteString(os.Stdout, body)
		return err
	}
	return os.WriteFile(out, []byte(body), 0o644)
}

// scaffoldFromExisting copies the schema + template from an existing
// widget-type bead and emits a fresh artifact under the new slug. Useful
// for "evolve a similar widget into a sibling".
func scaffoldFromExisting(cmd *cobra.Command, newSlug, fromSlug, summary, out string) error {
	all, err := listWidgetTypesBySlug(cmd, fromSlug)
	if err != nil {
		return err
	}
	if len(all) == 0 {
		return fmt.Errorf("no widget-type bead with slug %q to copy from", fromSlug)
	}
	src := pickWidgetTypeVersion(all, "")
	if summary == "" {
		summary = fmt.Sprintf("TODO: forked from %s — %s.", fromSlug, src.Fields["summary"])
	}
	schema := strings.TrimSpace(src.Fields["schema"])
	if schema == "" {
		schema = `{"type":"object","additionalProperties":false}`
	}
	tpl := src.Fields["template_source"]
	if strings.TrimSpace(tpl) == "" {
		return fmt.Errorf("widget-type %s has no template_source to copy", fromSlug)
	}
	// Best-effort sample by inspecting schema's required.
	sample := sampleFromSchemaRaw([]byte(schema))

	var b strings.Builder
	fmt.Fprintf(&b, "# Widget: %s\n\n", newSlug)
	fmt.Fprintf(&b, "## Summary\n\n%s\n\n", summary)
	fmt.Fprintf(&b, "## Schema\n\n```json\n%s\n```\n\n", strings.TrimSpace(schema))
	if sample != "" {
		fmt.Fprintf(&b, "## Sample payload\n\n```json\n%s\n```\n\n", sample)
	}
	fmt.Fprintf(&b, "## Template\n\n```html\n%s\n```\n", tpl)
	return writeScaffoldOutput(b.String(), out)
}

// sampleFromSchemaRaw is a tiny variant of sampleFromSchema that takes
// raw JSON instead of a bead. Returns an indented JSON sample that
// satisfies the schema's `required` clause, or empty string if no
// scaffold can be built.
func sampleFromSchemaRaw(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		return ""
	}
	out := map[string]any{}
	required, _ := schema["required"].([]any)
	props, _ := schema["properties"].(map[string]any)
	for _, r := range required {
		name, _ := r.(string)
		if name == "" {
			continue
		}
		var def map[string]any
		if props != nil {
			def, _ = props[name].(map[string]any)
		}
		out[name] = sampleValueFromDef(def)
	}
	if len(out) == 0 {
		return ""
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return ""
	}
	return string(b)
}

func commitScaffoldedWidget(cmd *cobra.Command, slug, in, project, version string) error {
	var body []byte
	var err error
	if in == "" || in == "-" {
		body, err = io.ReadAll(os.Stdin)
	} else {
		body, err = os.ReadFile(in)
	}
	if err != nil {
		return fmt.Errorf("reading scaffold artifact: %w", err)
	}
	doc, err := parseWidgetDoc(body)
	if err != nil {
		return err
	}
	if strings.TrimSpace(doc.Template) == "" {
		return fmt.Errorf("scaffold artifact has no `## Template` section")
	}
	// Run lint up-front so we don't ship a broken bead.
	doc.Slug = slug
	if version != "" {
		doc.Version = version
	}
	findings := lintWidgetTemplate(doc)
	for _, f := range findings {
		if f.Severity == "error" {
			fmt.Fprintln(os.Stderr, f.String())
		}
	}
	for _, f := range findings {
		if f.Severity == "error" {
			return fmt.Errorf("lint failed; refusing to commit. Fix the artifact and retry")
		}
	}

	fields := map[string]any{
		"slug":            slug,
		"renderer_kind":   "htmx",
		"template_engine": "go-html-template-v1",
		"template_source": doc.Template,
	}
	if version == "" {
		version = "1"
	}
	fields["widget_version"] = version
	if doc.Summary != "" {
		fields["summary"] = doc.Summary
	}
	if strings.TrimSpace(doc.Schema) != "" {
		// Schema is JSON — preserve as raw JSON.
		fields["schema"] = json.RawMessage(doc.Schema)
	}
	fieldsJSON, err := json.Marshal(fields)
	if err != nil {
		return fmt.Errorf("encoding fields: %w", err)
	}

	labels := []string{"widget-stage:draft"}
	if project != "" {
		labels = append(labels, "project:"+project)
	}

	id, err := daemon.CreateBead(cmd.Context(), beadsapi.CreateBeadRequest{
		Title:       fmt.Sprintf("Widget: %s", slug),
		Type:        "widget-type",
		Kind:        "data",
		Description: string(body),
		Labels:      labels,
		CreatedBy:   actor,
		Fields:      fieldsJSON,
	})
	if err != nil {
		return fmt.Errorf("creating widget-type bead: %w", err)
	}
	if jsonOutput {
		printJSON(map[string]any{
			"id":      id,
			"slug":    slug,
			"version": version,
			"stage":   "draft",
		})
		return nil
	}
	fmt.Printf("Created widget-type %s (slug=%s, version=%s, stage=draft)\n", id, slug, version)
	return nil
}
