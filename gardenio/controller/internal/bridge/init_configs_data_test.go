package bridge

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// initConfigsGolden holds one line per init_configs.yaml entry: the key, the Go
// type it decodes into, and its JSON (what EnsureConfigs sends the daemon). It
// was generated from the Go map literal configs() held before the entries moved
// to YAML (gd-Wl6LuCnvlH), so a pass means the data loads exactly as the code
// held it. After an intentional change to init_configs.yaml, regenerate with
//
//	GB_UPDATE_GOLDEN=1 go test ./internal/bridge/ -run TestInitConfigs_Golden
//
// then review the diff in the golden file before committing.
const initConfigsGolden = "testdata/init_configs.golden.txt"

func TestInitConfigs_Golden(t *testing.T) {
	m, err := decodeInitConfigs(initConfigsYAML)
	if err != nil {
		t.Fatal(err)
	}
	got := renderInitConfigs(t, m)
	if os.Getenv("GB_UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(initConfigsGolden, []byte(got), 0o644); err != nil {
			t.Fatalf("update golden %s: %v", initConfigsGolden, err)
		}
		t.Logf("updated golden %s (%d bytes)", initConfigsGolden, len(got))
		return
	}
	want, err := os.ReadFile(initConfigsGolden)
	if err != nil {
		t.Fatalf("read golden %s: %v (regenerate with GB_UPDATE_GOLDEN=1)", initConfigsGolden, err)
	}
	gotLines, wantLines := strings.Split(got, "\n"), strings.Split(string(want), "\n")
	for i := 0; i < len(gotLines) || i < len(wantLines); i++ {
		var g, w string
		if i < len(gotLines) {
			g = gotLines[i]
		}
		if i < len(wantLines) {
			w = wantLines[i]
		}
		if g != w {
			t.Errorf("init_configs.yaml differs from %s at line %d:\n got: %s\nwant: %s\n"+
				"If the change is INTENTIONAL, regenerate with GB_UPDATE_GOLDEN=1 and review the diff.",
				initConfigsGolden, i+1, g, w)
			return
		}
	}
}

func renderInitConfigs(t *testing.T, m map[string]any) string {
	t.Helper()
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		j, err := json.Marshal(m[k])
		if err != nil {
			t.Fatalf("marshal %q: %v", k, err)
		}
		fmt.Fprintf(&b, "%s\t%s\t%s\n", k, reflect.TypeOf(m[k]).Name(), j)
	}
	return b.String()
}

// TestInitConfigs_ConfigsCarriesEveryEntry pins that configs() still returns
// every YAML entry with its typed value, so callers that type-assert an entry
// (e.g. configs()["type:agent"].(TypeConfig)) keep working.
func TestInitConfigs_ConfigsCarriesEveryEntry(t *testing.T) {
	m, err := decodeInitConfigs(initConfigsYAML)
	if err != nil {
		t.Fatal(err)
	}
	all := configs()
	for k, v := range m {
		if got, ok := all[k]; !ok || reflect.TypeOf(got) != reflect.TypeOf(v) {
			t.Errorf("configs()[%q] = %T, want %T", k, got, v)
		}
	}
}

func TestDecodeInitConfigs_Strict(t *testing.T) {
	for _, tc := range []struct {
		name, yaml, want string
	}{
		{"unknown field", "\"type:x\":\n  kind: config\n  colour: red\n", "colour"},
		{"unknown field in a field def", "\"type:x\":\n  fields:\n    - {name: a, type: string, requird: true}\n", "requird"},
		{"repeated key", "\"view:x\":\n  sort: a\n\"view:x\":\n  sort: b\n", "defined"},
		{"unknown prefix", "\"widget:x\":\n  kind: config\n", "prefix"},
		{"not a mapping", "- a\n", "not a mapping"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeInitConfigs([]byte(tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

// configs is loadConfigs for tests that read one entry. A decode error yields
// an empty map, so those tests fail on the missing entry; TestInitConfigs_Golden
// reports the error itself.
func configs() map[string]any {
	m, err := loadConfigs()
	if err != nil {
		return map[string]any{}
	}
	return m
}

// TestLoadConfigs pins that the embedded data loads without error.
func TestLoadConfigs(t *testing.T) {
	if _, err := loadConfigs(); err != nil {
		t.Fatal(err)
	}
}
