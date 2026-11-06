package main

import (
	"context"
	"fmt"
	"strings"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/model"
)

// subFieldEdit is one parsed non-clobbering aggregate-field edit, produced from
// a --field-set / --field-unset-key / --field-append / --field-remove flag and
// sent to the daemon's atomic edit endpoint (ka-0000007767). Op is one of the
// client.EditSubFieldRequest ops: "set"/"unset-key" (JSON-map field) or
// "append"/"remove" (CSV field).
type subFieldEdit struct {
	field string
	req   *client.EditSubFieldRequest
}

// parseFieldSet parses a `--field-set <field>.<key>=<value>` spec into a map
// SET edit. The field and key are split on the FIRST '.', the value on the
// FIRST '=' after it — so a value may contain '=' and '.', and only the map key
// is taken from between the first '.' and first '='. Example:
// "channel_roles.C123=ops-lead" → field=channel_roles key=C123 value=ops-lead.
func parseFieldSet(spec string) (subFieldEdit, error) {
	dot := strings.IndexByte(spec, '.')
	eq := strings.IndexByte(spec, '=')
	if dot <= 0 || eq < 0 || eq < dot {
		return subFieldEdit{}, fmt.Errorf("invalid --field-set %q: expected <field>.<key>=<value>", spec)
	}
	field := spec[:dot]
	key := spec[dot+1 : eq]
	value := spec[eq+1:]
	if field == "" || key == "" {
		return subFieldEdit{}, fmt.Errorf("invalid --field-set %q: field and key must be non-empty", spec)
	}
	return subFieldEdit{field: field, req: &client.EditSubFieldRequest{Op: "set", Key: key, Value: value}}, nil
}

// parseFieldUnsetKey parses a `--field-unset-key <field>.<key>` spec into a map
// UNSET edit. The field and key are split on the FIRST '.'.
func parseFieldUnsetKey(spec string) (subFieldEdit, error) {
	dot := strings.IndexByte(spec, '.')
	if dot <= 0 || dot == len(spec)-1 {
		return subFieldEdit{}, fmt.Errorf("invalid --field-unset-key %q: expected <field>.<key>", spec)
	}
	return subFieldEdit{field: spec[:dot], req: &client.EditSubFieldRequest{Op: "unset-key", Key: spec[dot+1:]}}, nil
}

// parseFieldCSV parses a `<field>=<element>` spec (from --field-append /
// --field-remove) into a CSV edit with the given op ("append" or "remove").
func parseFieldCSV(spec, op string) (subFieldEdit, error) {
	field, element, ok := splitField(spec)
	if !ok || element == "" {
		return subFieldEdit{}, fmt.Errorf("invalid --field-%s %q: expected <field>=<element>", op, spec)
	}
	return subFieldEdit{field: field, req: &client.EditSubFieldRequest{Op: op, Element: element}}, nil
}

// collectSubFieldEdits reads the four sub-field-edit flags off updateCmd's flag
// set and returns the parsed edits in a stable order (set, unset-key, append,
// remove). Returns nil when none are set.
func collectSubFieldEdits(getStringArray func(name string) []string) ([]subFieldEdit, error) {
	var edits []subFieldEdit
	for _, spec := range getStringArray("field-set") {
		e, err := parseFieldSet(spec)
		if err != nil {
			return nil, err
		}
		edits = append(edits, e)
	}
	for _, spec := range getStringArray("field-unset-key") {
		e, err := parseFieldUnsetKey(spec)
		if err != nil {
			return nil, err
		}
		edits = append(edits, e)
	}
	for _, spec := range getStringArray("field-append") {
		e, err := parseFieldCSV(spec, "append")
		if err != nil {
			return nil, err
		}
		edits = append(edits, e)
	}
	for _, spec := range getStringArray("field-remove") {
		e, err := parseFieldCSV(spec, "remove")
		if err != nil {
			return nil, err
		}
		edits = append(edits, e)
	}
	return edits, nil
}

// applySubFieldEdits sends each parsed edit to the daemon's atomic edit endpoint
// in order and returns the bead as of the last successful edit (nil if none).
func applySubFieldEdits(ctx context.Context, id string, edits []subFieldEdit) (*model.Bead, error) {
	var last *model.Bead
	for _, e := range edits {
		b, err := beadsClient.EditBeadSubField(ctx, id, e.field, e.req)
		if err != nil {
			return nil, fmt.Errorf("editing %s (%s): %w", e.field, e.req.Op, err)
		}
		last = b
	}
	return last, nil
}
