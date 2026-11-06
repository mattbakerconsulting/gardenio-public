package landmap

import (
	"bufio"
	"os"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode"

	"gardenio/lands/contract/landsv1"
)

// specPath is the contract, read by path the way the server's parity tests
// read other modules' sources.
const specPath = "../../../lands/contract/openapi.yaml"

// TestVerbTable_CoversContract: one Verbs row per operation of the generated
// server interface, so a contract operation added later has no route until
// someone writes its row.
func TestVerbTable_CoversContract(t *testing.T) {
	var ops []string
	it := reflect.TypeOf((*landsv1.ServerInterface)(nil)).Elem()
	for i := 0; i < it.NumMethod(); i++ {
		n := []rune(it.Method(i).Name)
		n[0] = unicode.ToLower(n[0])
		ops = append(ops, string(n))
	}
	var rows []string
	for _, v := range Verbs {
		if slices.Contains(rows, v.Op) {
			t.Errorf("verb %s has two rows", v.Op)
		}
		rows = append(rows, v.Op)
		served := v.Deferred == ""
		if served && v.Action == "" {
			t.Errorf("verb %s: served with no action", v.Op)
		}
		if served && v.Action != ActionRead && v.Action != ActionLand && len(v.Types) == 0 {
			t.Errorf("verb %s: a write with no bead types", v.Op)
		}
		if served && v.Action == ActionLand && (!v.Land || len(v.Types) > 0 || len(v.routes) > 0) {
			t.Errorf("verb %s: an ActionLand verb acts only through Lands", v.Op)
		}
		if served && len(v.routes) == 0 && !v.Land {
			t.Errorf("verb %s: served, but calls nothing", v.Op)
		}
		if !served && (len(v.routes) > 0 || v.Action != "") {
			t.Errorf("verb %s: deferred, but has routes or an action", v.Op)
		}
	}
	slices.Sort(ops)
	slices.Sort(rows)
	if !slices.Equal(ops, rows) {
		t.Errorf("verb table %v\n  != contract operations %v", rows, ops)
	}
}

// Every route in the table is used by some verb, and every route a verb
// lists is in the table.
func TestRouteTable_EveryRouteUsed(t *testing.T) {
	used := map[routeID]bool{}
	for _, v := range Verbs {
		for _, r := range v.routes {
			used[r] = true
		}
	}
	for id, r := range routeTable {
		if r.Method == "" || !strings.HasPrefix(r.Pattern, "/v1/") {
			t.Errorf("route %d is malformed: %q", id, r)
		}
		if !used[routeID(id)] {
			t.Errorf("route %s is in the table but no verb lists it", r)
		}
	}
}

// jsonKeys lists a struct's JSON keys, nested struct and slice-of-struct
// keys as "<outer>.<inner>".
func jsonKeys(t reflect.Type, prefix string) []string {
	var out []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		k, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if k == "" || k == "-" {
			continue
		}
		out = append(out, prefix+k)
		ft := f.Type
		if ft.Kind() == reflect.Slice {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct && ft.PkgPath() == "" {
			out = append(out, jsonKeys(ft, prefix+k+".")...)
		}
	}
	return out
}

// TestRecords_KeysAreInTables: what the map decodes off the daemon is what
// its tables say it reads.
func TestRecords_KeysAreInTables(t *testing.T) {
	var top []string
	for _, r := range TypeRows {
		for _, f := range r.Fields {
			if !strings.HasPrefix(f, "fields.") {
				top = append(top, f)
			}
		}
	}
	for _, k := range jsonKeys(reflect.TypeOf(beadRecord{}), "") {
		if k != "fields" && !slices.Contains(top, k) {
			t.Errorf("beadRecord decodes %q, which no TypeRow reads", k)
		}
	}
	for _, k := range jsonKeys(reflect.TypeOf(commentRecord{}), "") {
		if !slices.Contains(commentKeys, k) {
			t.Errorf("commentRecord decodes %q, not in commentKeys", k)
		}
	}
	for _, k := range jsonKeys(reflect.TypeOf(meRecord{}), "") {
		if !slices.Contains(meKeys, k) {
			t.Errorf("meRecord decodes %q, not in meKeys", k)
		}
	}
	for _, k := range jsonKeys(reflect.TypeOf(membersRecord{}), "") {
		if !slices.Contains(memberKeys, k) {
			t.Errorf("membersRecord decodes %q, not in memberKeys", k)
		}
	}
	for typ, prefix := range map[reflect.Type]string{
		reflect.TypeOf(releaseListRecord{}): "", reflect.TypeOf(releaseRecord{}): "",
		reflect.TypeOf(releaseItemRecord{}): "contents.", reflect.TypeOf(deploymentRecord{}): "deployments.",
	} {
		for _, k := range jsonKeys(typ, prefix) {
			if !slices.Contains(releaseKeys, k) {
				t.Errorf("%s decodes %q, not in releaseKeys", typ.Name(), k)
			}
		}
	}
	for _, k := range jsonKeys(reflect.TypeOf(decisionOption{}), "fields.options.") {
		if !slices.Contains([]string{"id", "label", "short", "description", "artifact_type"}, strings.TrimPrefix(k, "fields.options.")) {
			t.Errorf("decisionOption decodes %q", k)
		}
	}
}

func TestTypeRows(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range TypeRows {
		if seen[string(r.Type)] {
			t.Errorf("type %s has two rows", r.Type)
		}
		seen[string(r.Type)] = true
		if r.Noun == "" && (len(r.Fields) > 0 || len(r.Writes) > 0) {
			t.Errorf("hidden type %s reads or writes keys", r.Type)
		}
		if r.Noun != "" && len(r.Fields) == 0 {
			t.Errorf("visible type %s reads nothing", r.Type)
		}
	}
	for _, v := range Verbs {
		for _, typ := range v.Types {
			if r, ok := RowFor(typ); !ok || r.Noun == "" {
				t.Errorf("verb %s touches %s, which has no visible row", v.Op, typ)
			}
		}
	}
}

// specEnum reads one enum's values from the contract.
func specEnum(t *testing.T, name string) []string {
	t.Helper()
	f, err := os.Open(specPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []string
	in, inEnum := false, false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "    "+name+":":
			in = true
		case in && strings.TrimSpace(line) == "enum:":
			inEnum = true
		case in && inEnum && strings.HasPrefix(strings.TrimSpace(line), "- "):
			out = append(out, strings.TrimPrefix(strings.TrimSpace(line), "- "))
		case in && inEnum:
			return out
		case in && strings.HasPrefix(line, "    ") && !strings.HasPrefix(line, "     "):
			t.Fatalf("%s: no block enum in %s", name, specPath)
		}
	}
	t.Fatalf("%s not found in %s", name, specPath)
	return nil
}

func TestEventTypes_AreTheContractEnum(t *testing.T) {
	var got []string
	for _, e := range EventTypes {
		if !e.Type.Valid() {
			t.Errorf("event type %q is not in the contract", e.Type)
		}
		if e.Noun == "" {
			t.Errorf("event %s has no noun", e.Type)
		}
		got = append(got, string(e.Type))
	}
	want := specEnum(t, "EventType")
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("EventTypes %v != contract EventType %v", got, want)
	}
}

func TestProblemCodes_AreTheContractEnum(t *testing.T) {
	var got []string
	for c := range problemShape {
		got = append(got, string(c))
	}
	want := specEnum(t, "ProblemCode")
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("problemShape %v != contract ProblemCode %v", got, want)
	}
	for st := 400; st < 600; st++ {
		if c := codeForStatus(st); !c.Valid() {
			t.Errorf("status %d maps to %q", st, c)
		}
	}
}

// TestOnlyLandmapImportsTheContract is the go list half of the dependency
// guard (depguard in .golangci.yml is the lint half): no kbeads package but
// this one imports the contract, and this one imports nothing of kbeads but
// the bead model.
func TestOnlyLandmapImportsTheContract(t *testing.T) {
	cmd := exec.Command("go", "list", "-f", `{{.ImportPath}}{{range .Imports}} {{.}}{{end}}{{range .TestImports}} {{.}}{{end}}{{range .XTestImports}} {{.}}{{end}}`, "gardenio/kbeads/...")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	const self = "gardenio/kbeads/internal/landmap"
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		n++
		pkg, imports := fields[0], fields[1:]
		for _, imp := range imports {
			if strings.HasPrefix(imp, "gardenio/lands/") && pkg != self {
				t.Errorf("%s imports %s; only %s may import the Lands contract", pkg, imp, self)
			}
			if pkg == self && strings.HasPrefix(imp, "gardenio/") && !strings.HasPrefix(imp, "gardenio/lands/contract/") && imp != "gardenio/lands/contract" &&
				imp != "gardenio/kbeads/internal/model" {
				t.Errorf("%s imports %s; the map reaches the daemon only through its routes", self, imp)
			}
		}
	}
	if n < 20 {
		t.Fatalf("go list saw %d packages; the walk is broken", n)
	}
}
