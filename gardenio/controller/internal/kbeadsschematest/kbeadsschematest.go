// Package kbeadsschematest lets controller tests check what they write against
// the bead type configs kbeads builds in (kbeads/internal/server/config.go).
//
// The controller and kbeads are separate modules, so a field or enum value the
// controller writes can go missing from kbeads' schema without any build
// breaking; kbeads then answers the write with HTTP 400 "unknown field" or
// "must be one of", and only in the live cluster. gd-AQ9iCiKxML hit this
// three times on one land (idempotency_key on type:project, provision_step
// "namespace", spend_limit_usd on type:account). Tests use Fields to fail on
// that drift instead.
//
// Like the coop lockstep canary in cmd/gb, it reads the kbeads source relative
// to this file and skips when that source is absent (a controller-only
// checkout), so it can only catch real drift.
package kbeadsschematest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"
)

// Field is one field of a builtin type config.
type Field struct {
	Name     string   `json:"name"`
	Type     string   `json:"type"`
	Required bool     `json:"required"`
	Values   []string `json:"values"`
}

// Fields returns the fields of kbeads' builtin config for typ (for example
// "plot_application"), keyed by name.
func Fields(t testing.TB, typ string) map[string]Field {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Skip("runtime.Caller unavailable; cannot locate kbeads source")
	}
	// From gardenio/controller/internal/kbeadsschematest/ the monorepo root is
	// four levels up.
	path := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "..", "kbeads", "internal", "server", "config.go")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("kbeads source not present at %s (%v); skipping schema check", path, err)
	}
	key := regexp.QuoteMeta("type:" + typ)
	re := regexp.MustCompile(`"` + key + `":\s*\{Key:\s*"` + key + `",\s*Value:\s*json\.RawMessage\(` + "`([^`]*)`")
	m := re.FindSubmatch(src)
	if m == nil {
		t.Fatalf("no builtin config for type:%s in %s; did its form change?", typ, path)
	}
	var cfg struct {
		Fields []Field `json:"fields"`
	}
	if err := json.Unmarshal(m[1], &cfg); err != nil {
		t.Fatalf("type:%s config in %s is not JSON: %v", typ, path, err)
	}
	out := make(map[string]Field, len(cfg.Fields))
	for _, f := range cfg.Fields {
		out[f.Name] = f
	}
	return out
}

// RequireFields fails t for each name that type typ does not declare.
func RequireFields(t testing.TB, typ string, names ...string) {
	t.Helper()
	fields := Fields(t, typ)
	for _, n := range names {
		if _, ok := fields[n]; !ok {
			t.Errorf("type:%s does not declare field %q; kbeads would reject the write with HTTP 400 (add it to kbeads/internal/server/config.go)", typ, n)
		}
	}
}

// RequireEnumValues fails t for each value that the enum field of type typ
// does not allow.
func RequireEnumValues(t testing.TB, typ, field string, values ...string) {
	t.Helper()
	f, ok := Fields(t, typ)[field]
	if !ok {
		t.Fatalf("type:%s does not declare field %q", typ, field)
	}
	allowed := make(map[string]bool, len(f.Values))
	for _, v := range f.Values {
		allowed[v] = true
	}
	for _, v := range values {
		if !allowed[v] {
			t.Errorf("type:%s field %s does not allow %q (allows %v); kbeads would reject the write with HTTP 400 (add it to kbeads/internal/server/config.go)", typ, field, v, f.Values)
		}
	}
}
