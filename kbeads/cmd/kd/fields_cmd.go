package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/model"
	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fieldsCmd introspects a bead type's schema so an author can see which typed
// fields the type accepts BEFORE a write is rejected. It closes the
// discoverability gap behind ka-0000002423: when the field is `slack_channel`
// and an agent typed `slack_thread_channel`, the write 400'd with a bare
// "unknown field", the agent concluded the capability didn't exist, and filed
// a redundant feature (ka-0000003647). `kd fields trigger | grep slack_channel`
// answers "what is this field actually called?" in one command.
var fieldsCmd = &cobra.Command{
	Use:     "fields <type>",
	Short:   "Show the typed fields a bead type accepts",
	GroupID: "beads",
	Args:    cobra.ExactArgs(1),
	Long: `Show the typed fields a bead type accepts.

Prints a table of NAME | TYPE | REQUIRED | VALUES for the given bead type's
schema, so you can see the exact field names before a create/update rejects an
unknown one. Use --json for machine-readable output.

Examples:
  kd fields trigger              # list the fields type:trigger accepts
  kd fields trigger | grep slack # find the right slack_* field name
  kd fields task --json          # machine-readable schema`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := context.Background()

		typeName := strings.TrimSpace(args[0])
		if typeName == "" {
			return fmt.Errorf("type name is required")
		}
		// Mirror the create path: a deprecated alias (e.g. `template`) resolves
		// to its canonical type so `kd fields template` shows the formula schema
		// rather than 404ing on a key that only exists post-alias.
		if canon, ok := model.TypeAliases[model.BeadType(typeName)]; ok {
			typeName = string(canon)
		}

		key := "type:" + typeName
		cfg, err := beadsClient.GetConfig(ctx, key)
		if err != nil {
			if isConfigNotFound(err) {
				return unknownFieldsTypeError(ctx, typeName)
			}
			return fmt.Errorf("fetching schema for type %q: %w", typeName, err)
		}

		var tc model.TypeConfig
		if err := json.Unmarshal(cfg.Value, &tc); err != nil {
			return fmt.Errorf("parsing schema for type %q: %w", typeName, err)
		}

		if jsonOutput {
			return printFieldsJSON(typeName, &tc)
		}
		printFieldsTable(typeName, &tc)
		return nil
	},
}

// fieldsJSON is the stable machine-readable shape emitted by --json. Using a
// typed struct (rather than a map) keeps the key set uniform — every key,
// including allow_additional_fields, is always present with a stable type —
// and preserves a deterministic key order.
type fieldsJSON struct {
	Type                  string           `json:"type"`
	Kind                  string           `json:"kind"`
	AllowAdditionalFields bool             `json:"allow_additional_fields"`
	Fields                []model.FieldDef `json:"fields"`
}

// printFieldsJSON emits the schema as a stable machine-readable object. The
// fields keep schema declaration order (not sorted) so the JSON mirrors the
// authoritative config; FieldDef already carries json tags
// (name/type/required/values/min/max).
func printFieldsJSON(typeName string, tc *model.TypeConfig) error {
	out := fieldsJSON{
		Type:                  typeName,
		Kind:                  string(tc.Kind),
		AllowAdditionalFields: tc.AllowAdditionalFields,
		Fields:                tc.Fields,
	}
	emitJSON(out)
	return nil
}

// printFieldsTable renders the human-facing NAME | TYPE | REQUIRED | VALUES
// table. Rows follow schema declaration order so related fields stay grouped
// as the schema author arranged them.
func printFieldsTable(typeName string, tc *model.TypeConfig) {
	fmt.Printf("Type:   %s\n", typeName)
	fmt.Printf("Kind:   %s\n", tc.Kind)
	if tc.AllowAdditionalFields {
		fmt.Println("Schema: open (accepts additional fields beyond those listed)")
	}
	fmt.Printf("Fields: %d\n\n", len(tc.Fields))

	if len(tc.Fields) == 0 {
		fmt.Println("(no typed fields declared)")
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tTYPE\tREQUIRED\tVALUES")
	for _, f := range tc.Fields {
		required := ""
		if f.Required {
			required = "yes"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", f.Name, f.Type, required, fieldValuesHint(f))
	}
	_ = w.Flush()
}

// fieldValuesHint renders the VALUES column: the allowed set for an enum, or an
// inclusive integer range/bound when Min/Max are set. Empty for everything else.
func fieldValuesHint(f model.FieldDef) string {
	if len(f.Values) > 0 {
		return strings.Join(f.Values, " | ")
	}
	switch {
	case f.Min != nil && f.Max != nil:
		return fmt.Sprintf("%d..%d", *f.Min, *f.Max)
	case f.Min != nil:
		return fmt.Sprintf(">= %d", *f.Min)
	case f.Max != nil:
		return fmt.Sprintf("<= %d", *f.Max)
	}
	return ""
}

// isConfigNotFound reports whether err is the daemon's "no such config" signal,
// across both transports: HTTP surfaces a *client.APIError with StatusCode 404;
// gRPC surfaces a status with codes.NotFound.
func isConfigNotFound(err error) bool {
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == http.StatusNotFound
	}
	return status.Code(err) == codes.NotFound
}

// unknownFieldsTypeError builds the error for `kd fields <bogus>`: it lists the
// available type names so the caller can pick a real one, rather than leaving
// them to guess. The known set is the builtin floor + aliases (no network),
// widened with the live `type:*` registry so custom types are included too.
func unknownFieldsTypeError(ctx context.Context, typeName string) error {
	known := localKnownTypes()
	mergeRegistryTypes(ctx, known)

	valid := sortedKnownTypeNames(known)
	return fmt.Errorf("unknown bead type %q; available types: %s", typeName, strings.Join(valid, ", "))
}
