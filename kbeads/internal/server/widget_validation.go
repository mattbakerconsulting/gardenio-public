// Free-htmx widget validation hooks.
//
// Two pre-write validators run at createBead/updateBead time:
//
//   - validateWidgetTypeFields enforces the htmx contract on type=widget-type
//     beads: when renderer_kind=htmx, template_source must be set and must
//     parse as a Go html/template. Catching template parse errors at write
//     time keeps broken widgets from ever reaching the renderer (and from
//     blocking daemon startup, where the same compile pass runs fail-loud
//     across every widget-type bead).
//
//   - validateWidgetInstanceFields runs JSON-Schema validation on the
//     widget-instance `data` payload against the `schema` declared on the
//     active widget-type bead identified by (widget_type slug, widget_version).
//     Schema lookup uses the standard Fields filter on ListBeads. When no
//     schema is set on the type bead, validation is a no-op (legacy compat
//     per gardenio/docs/research/free-htmx-widget-wire-shape.md §3).
//
// The plan-of-record is gardenio/plans/free-htmx-widgets-plan.md (ka-0000002807);
// the wire-shape research is gardenio/docs/research/free-htmx-widget-wire-shape.md.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"strings"

	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/store"
	"github.com/santhosh-tekuri/jsonschema/v5"
)

// widgetTypeFields is the decoded subset of widget-type fields the
// validators care about.
type widgetTypeFields struct {
	Slug           string          `json:"slug"`
	WidgetVersion  string          `json:"widget_version"`
	RendererKind   string          `json:"renderer_kind"`
	TemplateSource string          `json:"template_source"`
	TemplateEngine string          `json:"template_engine"`
	Schema         json.RawMessage `json:"schema"`
}

// widgetInstanceFields is the decoded subset of widget-instance fields the
// validators care about.
type widgetInstanceFields struct {
	WidgetType    string          `json:"widget_type"`
	WidgetVersion string          `json:"widget_version"`
	Data          json.RawMessage `json:"data"`
}

// rendererKind constants.
const (
	rendererKindBuiltin = "builtin"
	rendererKindHTMX    = "htmx"
)

// templateEngineGoHTMLV1 is the only supported template engine.
const templateEngineGoHTMLV1 = "go-html-template-v1"

// validateWidgetTypeFields enforces the htmx contract on widget-type writes.
// When renderer_kind defaults to "" or "builtin", template_source is ignored.
// When renderer_kind=htmx, template_source must be present and must compile
// under html/template. Returns a user-facing error suitable for inputError.
func validateWidgetTypeFields(fields json.RawMessage) error {
	if len(fields) == 0 {
		return nil
	}
	var f widgetTypeFields
	if err := json.Unmarshal(fields, &f); err != nil {
		// Generic ValidateTypeFields already rejects malformed JSON; defensive.
		return nil
	}
	rk := f.RendererKind
	if rk == "" {
		rk = rendererKindBuiltin
	}
	if rk != rendererKindHTMX {
		// builtin renderers ship with the daemon; nothing to compile.
		return nil
	}
	if strings.TrimSpace(f.TemplateSource) == "" {
		return fmt.Errorf("widget-type renderer_kind=htmx requires template_source")
	}
	engine := f.TemplateEngine
	if engine == "" {
		engine = templateEngineGoHTMLV1
	}
	if engine != templateEngineGoHTMLV1 {
		return fmt.Errorf("widget-type template_engine=%q not supported (only %q)",
			engine, templateEngineGoHTMLV1)
	}
	if _, err := template.New(f.Slug).Parse(f.TemplateSource); err != nil {
		return fmt.Errorf("widget-type template_source failed to compile: %v", err)
	}
	return nil
}

// validateWidgetInstanceFields looks up the active widget-type bead by
// (widget_type slug, widget_version) and validates the instance's data
// payload against its schema. Returns nil when no matching widget-type
// bead exists OR when the type bead has no schema (legacy compat).
//
// Lookup uses ListBeads with a Fields filter; the (slug, widget_version)
// pair is treated as the unique-enough natural key for v1. If multiple
// type beads match (e.g. concurrent edits, no version pin), the first
// returned wins — sufficient for v1 since the renderer applies the same
// best-match strategy.
func validateWidgetInstanceFields(ctx context.Context, s store.Store, fields json.RawMessage) error {
	if len(fields) == 0 {
		return nil
	}
	var inst widgetInstanceFields
	if err := json.Unmarshal(fields, &inst); err != nil {
		return nil
	}
	if inst.WidgetType == "" {
		// generic ValidateTypeFields enforces the required-ness; defensive.
		return nil
	}
	filter := model.BeadFilter{
		Type:   []model.BeadType{"widget-type"},
		Fields: map[string]string{"slug": inst.WidgetType},
		Limit:  10,
		Fleet:  model.FleetAll,
	}
	if inst.WidgetVersion != "" {
		filter.Fields["widget_version"] = inst.WidgetVersion
	}
	beads, _, err := s.ListBeads(ctx, filter)
	if err != nil {
		return fmt.Errorf("widget-instance: failed to lookup widget-type %q: %w",
			inst.WidgetType, err)
	}
	if len(beads) == 0 {
		// Unknown widget-type slug — reject so typos surface at write time
		// rather than rendering a blank widget downstream.
		return fmt.Errorf("widget-instance: unknown widget_type=%q (no matching widget-type bead)",
			inst.WidgetType)
	}
	var typeFields widgetTypeFields
	if len(beads[0].Fields) > 0 {
		if err := json.Unmarshal(beads[0].Fields, &typeFields); err != nil {
			// Malformed type bead is a separate fault; skip schema validation.
			return nil
		}
	}
	if len(typeFields.Schema) == 0 || strings.TrimSpace(string(typeFields.Schema)) == "null" {
		// No schema declared — accept by contract.
		return nil
	}
	if err := validateAgainstJSONSchema(typeFields.Schema, inst.Data); err != nil {
		return fmt.Errorf("widget-instance data failed schema validation for widget_type=%q: %v",
			inst.WidgetType, err)
	}
	return nil
}

// validateAgainstJSONSchema compiles the JSON-Schema document and validates
// the payload against it. Both arguments are raw JSON. A nil/empty payload
// is treated as JSON null for validation purposes so a "required" schema
// reliably rejects missing data.
func validateAgainstJSONSchema(schemaRaw, payloadRaw json.RawMessage) error {
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("widget://schema.json", strings.NewReader(string(schemaRaw))); err != nil {
		return fmt.Errorf("invalid schema: %v", err)
	}
	sch, err := compiler.Compile("widget://schema.json")
	if err != nil {
		return fmt.Errorf("schema compile: %v", err)
	}
	var payload any
	body := payloadRaw
	if len(body) == 0 {
		body = json.RawMessage("null")
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return fmt.Errorf("invalid data JSON: %v", err)
	}
	if err := sch.Validate(payload); err != nil {
		return err
	}
	return nil
}

// CompileAllWidgetTemplates walks every type=widget-type bead with
// renderer_kind=htmx and parses its template_source. Called at daemon
// startup so a malformed template is surfaced immediately rather than at
// the first render request. Returns an aggregated error describing every
// failure (one line per bead) so operators see all bad widgets in a single
// log entry.
func CompileAllWidgetTemplates(ctx context.Context, s store.Store) error {
	beads, _, err := s.ListBeads(ctx, model.BeadFilter{
		Type:  []model.BeadType{"widget-type"},
		Fleet: model.FleetAll,
		Limit: 10000,
	})
	if err != nil {
		return fmt.Errorf("widget startup compile: list widget-type beads: %w", err)
	}
	var failures []string
	for _, b := range beads {
		if len(b.Fields) == 0 {
			continue
		}
		var f widgetTypeFields
		if err := json.Unmarshal(b.Fields, &f); err != nil {
			failures = append(failures, fmt.Sprintf("%s: malformed fields JSON: %v", b.ID, err))
			continue
		}
		rk := f.RendererKind
		if rk == "" {
			rk = rendererKindBuiltin
		}
		if rk != rendererKindHTMX {
			continue
		}
		if strings.TrimSpace(f.TemplateSource) == "" {
			failures = append(failures, fmt.Sprintf("%s (slug=%s): renderer_kind=htmx but template_source is empty", b.ID, f.Slug))
			continue
		}
		if _, err := template.New(f.Slug).Parse(f.TemplateSource); err != nil {
			failures = append(failures, fmt.Sprintf("%s (slug=%s): template parse error: %v", b.ID, f.Slug, err))
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("widget startup compile: %d failure(s):\n  %s",
			len(failures), strings.Join(failures, "\n  "))
	}
	return nil
}
