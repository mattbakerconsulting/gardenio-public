package bridge

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
)

// TestEnsureConfigs_SeedsViewType guards the type:view registration
// (ka-0000008041). The beads-viewer "My Views" page (cmd/beads-viewer/views.go)
// creates saved-view beads with type=view and lists them with a type=view
// filter. Once the kbeads daemon validates the --type filter server-side, an
// unregistered "view" type would 400 both the create AND the list query,
// silently breaking the whole saved-views feature. This test fails if the
// registration is dropped or its schema drifts from savedViewSpec.toBeadFields.
func TestEnsureConfigs_SeedsViewType(t *testing.T) {
	setter := &mockConfigSetter{}
	if err := EnsureConfigs(context.Background(), setter, slog.Default()); err != nil {
		t.Fatalf("EnsureConfigs: %v", err)
	}

	b := findBead(setter.beads, "type:view", []string{"global"})
	if b == nil {
		t.Fatal("expected config bead type:view")
	}

	var tc TypeConfig
	if err := json.Unmarshal([]byte(b.Description), &tc); err != nil {
		t.Fatalf("unmarshal type:view: %v", err)
	}

	if tc.Kind != "config" {
		t.Errorf("kind: got %q, want %q", tc.Kind, "config")
	}

	// The schema must be a superset of every field savedViewSpec.toBeadFields
	// stamps, or the create path 400s "unknown field" and no view is ever saved.
	wantFields := []string{
		"source",
		"project",
		"types",
		"statuses",
		"labels_filter",
		"search",
		"sort",
		"columns",
	}
	got := make(map[string]bool, len(tc.Fields))
	for _, f := range tc.Fields {
		got[f.Name] = true
	}
	for _, name := range wantFields {
		if !got[name] {
			t.Errorf("missing field %q in type:view", name)
		}
	}
}
