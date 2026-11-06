package server

import (
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// chartCatalogGolden pins every field of every chartCatalog() entry. It was
// generated from the Go-literal catalog before the charts moved to
// charts_catalog.yaml (gd-Wl6LuCnvlH), so TestChartCatalog_Golden is the proof
// that the embedded data loads to exactly what the code held before.
//
// Regenerate after an INTENTIONAL catalog change:
//
//	GB_UPDATE_GOLDEN=1 go test ./internal/server/ -run TestChartCatalog_Golden
//
// then review the diff in the golden file before committing.
const chartCatalogGolden = "testdata/chart_catalog.golden.txt"

func TestChartCatalog_Golden(t *testing.T) {
	got := dumpChartCatalog(chartCatalog())
	if os.Getenv("GB_UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(chartCatalogGolden, []byte(got), 0o644); err != nil {
			t.Fatalf("update golden %s: %v", chartCatalogGolden, err)
		}
		t.Logf("updated golden %s (%d bytes)", chartCatalogGolden, len(got))
		return
	}
	want, err := os.ReadFile(chartCatalogGolden)
	if err != nil {
		t.Fatalf("read golden %s: %v (regenerate with GB_UPDATE_GOLDEN=1)", chartCatalogGolden, err)
	}
	if got != string(want) {
		gl, wl := strings.Split(got, "\n"), strings.Split(string(want), "\n")
		for i := 0; i < len(gl) || i < len(wl); i++ {
			var g, w string
			if i < len(gl) {
				g = gl[i]
			}
			if i < len(wl) {
				w = wl[i]
			}
			if g != w {
				t.Fatalf("chart catalog drifted from %s at line %d:\n  want: %s\n  got:  %s\n"+
					"If the change is INTENTIONAL, regenerate with GB_UPDATE_GOLDEN=1 and review the diff.",
					chartCatalogGolden, i+1, w, g)
			}
		}
	}
}

// dumpChartCatalog renders the catalog one field per line, sorted by chart id
// and map key, with the dynamic type of every interface value spelled out so an
// int default decoding as a float64 (or a string) shows up as a diff.
func dumpChartCatalog(cat map[string]ChartDef) string {
	ids := make([]string, 0, len(cat))
	for id := range cat {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var b strings.Builder
	for _, id := range ids {
		fmt.Fprintf(&b, "[%s]\n", id)
		dumpValue(&b, "", reflect.ValueOf(cat[id]))
	}
	return b.String()
}

func dumpValue(b *strings.Builder, path string, v reflect.Value) {
	switch v.Kind() {
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			dumpValue(b, path+"."+v.Type().Field(i).Name, v.Field(i))
		}
	case reflect.Map:
		if v.IsNil() {
			fmt.Fprintf(b, "%s = nil\n", path)
			return
		}
		keys := v.MapKeys()
		sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
		fmt.Fprintf(b, "%s = map(len %d)\n", path, len(keys))
		for _, k := range keys {
			dumpValue(b, fmt.Sprintf("%s[%q]", path, k.String()), v.MapIndex(k))
		}
	case reflect.Slice:
		if v.IsNil() {
			fmt.Fprintf(b, "%s = nil\n", path)
			return
		}
		fmt.Fprintf(b, "%s = slice(len %d)\n", path, v.Len())
		for i := 0; i < v.Len(); i++ {
			dumpValue(b, fmt.Sprintf("%s[%d]", path, i), v.Index(i))
		}
	case reflect.Interface:
		if v.IsNil() {
			fmt.Fprintf(b, "%s = nil\n", path)
			return
		}
		fmt.Fprintf(b, "%s = %T(%#v)\n", path, v.Elem().Interface(), v.Elem().Interface())
	default:
		fmt.Fprintf(b, "%s = %#v\n", path, v.Interface())
	}
}

// TestChartCatalog_CallsAreIndependent pins that chartCatalog() hands each
// caller its own maps and slices, as the Go-literal catalog did: the decoded
// YAML is cached, so a shallow copy would let one caller corrupt the next.
func TestChartCatalog_CallsAreIndependent(t *testing.T) {
	a := chartCatalog()
	for id, d := range a {
		d.Surfaces[0] = "mutated"
		for k := range d.Params {
			delete(d.Params, k)
		}
		for i := range d.ExtraSeries {
			d.ExtraSeries[i].QueryTemplate = "mutated"
			for k := range d.ExtraSeries[i].AllowedLabels {
				delete(d.ExtraSeries[i].AllowedLabels, k)
			}
		}
		a[id] = d
	}
	if got, want := dumpChartCatalog(chartCatalog()), mustReadGolden(t); got != want {
		t.Fatal("mutating one chartCatalog() result changed the next call's result")
	}
}

func mustReadGolden(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(chartCatalogGolden)
	if err != nil {
		t.Fatalf("read golden %s: %v", chartCatalogGolden, err)
	}
	return string(b)
}

func TestLoadChartCatalog_Rejects(t *testing.T) {
	for name, doc := range map[string]string{
		"unknown key":          "- id: a\n  titel: typo\n",
		"unknown fragment":     "- id: a\n  query_template: 'sum(${nope})'\n",
		"unknown func":         "- id: a\n  query_template: '${nope:1h}'\n",
		"const used as func":   "- id: a\n  query_template: '${realBillAgentLevel:1h}'\n",
		"duplicate id":         "- id: a\n- id: a\n",
		"missing id":           "- title: no id\n",
		"extra series bad ref": "- id: a\n  extra_series:\n    - name: x\n      query_template: '${nope}'\n",
	} {
		if _, err := loadChartCatalog([]byte(doc)); err == nil {
			t.Errorf("%s: loadChartCatalog accepted %q", name, doc)
		}
	}
}
