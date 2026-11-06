package contract

import (
	"bytes"
	"encoding/json"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Scenario fixtures (contract design gd-EWSKPH7pa5 §3 and §8 step 10, task
// gd-VMGMkafIBm) live at scenarios/<name>.json. A golden fixture proves a
// shape; a scenario proves a meaning: what a land holds, in the land's words
// (given), and what each call must answer about it (steps). The conformance
// suite (../gl/conformance_test.go, ci.yml job lands-conformance) seeds each
// given into a real daemon and replays the steps through the land door.
// Parity proves a field exists; only a replay catches a rename that keeps the
// old field and empties a list with a 200.
//
// A step's answer is a whole contract document, held to its schema here.
// Three things vary between runs and are not compared literally: bind names
// the pointers that hold ids, which equal the id the given seeded under that
// name or, the first time an unseeded name is met, are captured for later
// steps; any names pointers whose value is the server's own (timestamps). A
// step's match says how the answer is found:
//
//	exact     the response is the answer
//	listed    the response is a page with an item equal to the answer
//	unlisted  the response is a page with no item whose bound ids are the answer's
const scenariosDir = "scenarios"

// givenName is a given line that seeds a named record: "ticket: an open task
// of the land". The replay seeds it under that name.
var givenName = regexp.MustCompile(`^([a-z_]+): `)

// placeholder is a {name} in a step's call.
var placeholder = regexp.MustCompile(`\{([a-z_]+)\}`)

// wantScenarios are the scenarios the design names (gd-EWSKPH7pa5 §3).
var wantScenarios = []string{"open_decision", "blocked_ticket", "crew_after_rename"}

type scenario struct {
	Scenario string         `json:"scenario"`
	About    string         `json:"about"`
	Given    []string       `json:"given"`
	Steps    []scenarioStep `json:"steps"`
}

type scenarioStep struct {
	Say        string            `json:"say"`
	When       string            `json:"when,omitempty"`
	Call       string            `json:"call"`
	BodySchema string            `json:"body_schema,omitempty"`
	Body       json.RawMessage   `json:"body,omitempty"`
	Status     int               `json:"status"`
	Schema     string            `json:"schema"`
	Match      string            `json:"match"`
	Answer     json.RawMessage   `json:"answer"`
	Bind       map[string]string `json:"bind,omitempty"`
	Any        []string          `json:"any,omitempty"`
}

var scenarioMatches = []string{"exact", "listed", "unlisted"}

func loadScenarios(t *testing.T) map[string]scenario {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(scenariosDir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]scenario{}
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var sc scenario
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&sc); err != nil {
			t.Errorf("%s: %v", p, err)
			continue
		}
		if want := strings.TrimSuffix(filepath.Base(p), ".json"); sc.Scenario != want {
			t.Errorf("%s: scenario %q, want %q (the file's name)", p, sc.Scenario, want)
		}
		out[sc.Scenario] = sc
	}
	return out
}

// callPatterns maps each spec operation to a matcher on the path after
// /lands/v1/lands/{land}: "GET /tickets/{ticket}" and the like.
func callPatterns(t *testing.T) map[string]*regexp.Regexp {
	t.Helper()
	const prefix = "/lands/v1/lands/{land}"
	ops := map[string]*regexp.Regexp{}
	for p, item := range loadSpec(t)["paths"].(node) {
		rest, ok := strings.CutPrefix(p, prefix)
		if !ok {
			continue
		}
		pat := regexp.MustCompile(`\\\{[a-z_]+\\\}`).ReplaceAllString(regexp.QuoteMeta(rest), `[^/]+`)
		for m := range item.(node) {
			if slices.Contains(httpMethods, m) {
				ops[strings.ToUpper(m)+" "+rest] = regexp.MustCompile("^" + pat + "$")
			}
		}
	}
	return ops
}

// jsonPointer reads a "/a/0/b" pointer out of a decoded document.
func jsonPointer(doc any, ptr string) (any, bool) {
	if !strings.HasPrefix(ptr, "/") {
		return nil, false
	}
	cur := doc
	for _, tok := range strings.Split(ptr[1:], "/") {
		switch v := cur.(type) {
		case map[string]any:
			x, ok := v[tok]
			if !ok {
				return nil, false
			}
			cur = x
		case []any:
			i, err := strconv.Atoi(tok)
			if err != nil || i < 0 || i >= len(v) {
				return nil, false
			}
			cur = v[i]
		default:
			return nil, false
		}
	}
	return cur, true
}

func TestScenarios(t *testing.T) {
	v := newValidator(t)
	ops := callPatterns(t)
	all := loadScenarios(t)
	for _, name := range wantScenarios {
		if _, ok := all[name]; !ok {
			t.Errorf("no scenario %s under %s/: the design names it", name, scenariosDir)
		}
	}
	for name, sc := range all {
		if sc.About == "" || len(sc.Given) == 0 || len(sc.Steps) == 0 {
			t.Errorf("%s: about, given and steps are all required", name)
		}
		known := map[string]bool{}
		for _, g := range sc.Given {
			if m := givenName.FindStringSubmatch(g); m != nil {
				known[m[1]] = true
			}
		}
		for i, st := range sc.Steps {
			where := name + " step " + strconv.Itoa(i+1)
			for _, m := range placeholder.FindAllStringSubmatch(st.Call, -1) {
				if !known[m[1]] {
					t.Errorf("%s: call names {%s}, which no given seeds and no earlier step binds", where, m[1])
				}
			}
			for _, n := range st.Bind {
				known[n] = true
			}
			if st.Say == "" {
				t.Errorf("%s: say what the step shows", where)
			}
			method, target, _ := strings.Cut(st.Call, " ")
			u, err := url.Parse(target)
			if err != nil {
				t.Errorf("%s: call %q: %v", where, st.Call, err)
				continue
			}
			matched := false
			for op, re := range ops {
				if strings.HasPrefix(op, method+" ") && re.MatchString(u.Path) {
					matched = true
				}
			}
			if !matched {
				t.Errorf("%s: call %q is no operation in %s", where, st.Call, specPath)
			}
			if (len(st.Body) > 0) != (st.BodySchema != "") {
				t.Errorf("%s: body and body_schema go together", where)
			} else if len(st.Body) > 0 {
				if err := v.check(st.BodySchema, st.Body); err != nil {
					t.Errorf("%s: body: %v", where, err)
				}
			}
			if st.Status < 200 || st.Status > 299 {
				t.Errorf("%s: status %d; a scenario states answers, the problem fixtures state refusals", where, st.Status)
			}
			if !slices.Contains(scenarioMatches, st.Match) {
				t.Errorf("%s: match %q, want one of %v", where, st.Match, scenarioMatches)
			}
			if st.Match != "exact" && !strings.HasPrefix(st.Call, "GET ") {
				t.Errorf("%s: %s reads a list, so its call is a GET", where, st.Match)
			}
			if _, ok := goTypes[st.Schema+"Page"]; st.Match != "exact" && !ok {
				t.Errorf("%s: %s reads a page of %s, and there is no %sPage", where, st.Match, st.Schema, st.Schema)
			}
			if err := v.check(st.Schema, st.Answer); err != nil {
				t.Errorf("%s: answer: %v", where, err)
				continue
			}
			var doc any
			if err := json.Unmarshal(st.Answer, &doc); err != nil {
				t.Fatal(err)
			}
			if len(st.Bind) == 0 {
				t.Errorf("%s: bind no id, and the replay cannot tell this land's answer from another's", where)
			}
			for _, p := range append(slices.Sorted(maps.Keys(st.Bind)), st.Any...) {
				if _, ok := jsonPointer(doc, p); !ok {
					t.Errorf("%s: pointer %q is not in the answer", where, p)
				}
			}
			for _, p := range st.Any {
				if _, ok := st.Bind[p]; ok {
					t.Errorf("%s: pointer %q is both bound and any", where, p)
				}
			}
		}
	}
}
