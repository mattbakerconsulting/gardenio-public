package contract

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"

	"gardenio/lands/contract/landsv1"
)

// Golden fixtures live at fixtures/<Schema>/<case>.json, one directory per
// object schema in openapi.yaml. The same files are type-checked against the
// TypeScript binding (ts/fixtures.check.ts), so one set serves both clients.
//
// fixtures/_invalid/<Schema>/<case>.json must FAIL validation; they prove the
// checks below are not vacuous.
const (
	fixturesDir = "fixtures"
	invalidDir  = "_invalid"
	specURL     = "file:///lands/contract/openapi.json"
)

// goTypes binds each object schema to its generated Go type. A schema missing
// here fails TestEverySchemaHasFixtures, so a new noun cannot ship untested.
var goTypes = map[string]func() any{
	"Land":             func() any { return new(landsv1.Land) },
	"Plot":             func() any { return new(landsv1.Plot) },
	"PlotPage":         func() any { return new(landsv1.PlotPage) },
	"Member":           func() any { return new(landsv1.Member) },
	"MemberPage":       func() any { return new(landsv1.MemberPage) },
	"Ticket":           func() any { return new(landsv1.Ticket) },
	"TicketPage":       func() any { return new(landsv1.TicketPage) },
	"TicketCreate":     func() any { return new(landsv1.TicketCreate) },
	"TicketUpdate":     func() any { return new(landsv1.TicketUpdate) },
	"TicketClose":      func() any { return new(landsv1.TicketClose) },
	"Comment":          func() any { return new(landsv1.Comment) },
	"CommentCreate":    func() any { return new(landsv1.CommentCreate) },
	"DecisionOption":   func() any { return new(landsv1.DecisionOption) },
	"DecisionEvidence": func() any { return new(landsv1.DecisionEvidence) },
	"Decision":         func() any { return new(landsv1.Decision) },
	"DecisionPage":     func() any { return new(landsv1.DecisionPage) },
	"DecisionResolve":  func() any { return new(landsv1.DecisionResolve) },
	"DecisionCancel":   func() any { return new(landsv1.DecisionCancel) },
	"Escalation":       func() any { return new(landsv1.Escalation) },
	"EscalationPage":   func() any { return new(landsv1.EscalationPage) },
	"EscalationCreate": func() any { return new(landsv1.EscalationCreate) },
	"PlanStep":         func() any { return new(landsv1.PlanStep) },
	"Plan":             func() any { return new(landsv1.Plan) },
	"PlanPage":         func() any { return new(landsv1.PlanPage) },
	"PlanAnswer":       func() any { return new(landsv1.PlanAnswer) },
	"Thread":           func() any { return new(landsv1.Thread) },
	"ThreadPage":       func() any { return new(landsv1.ThreadPage) },
	"Message":          func() any { return new(landsv1.Message) },
	"MessagePage":      func() any { return new(landsv1.MessagePage) },
	"MessageCreate":    func() any { return new(landsv1.MessageCreate) },
	"Budget":           func() any { return new(landsv1.Budget) },
	"BudgetDay":        func() any { return new(landsv1.BudgetDay) },
	"BudgetHistory":    func() any { return new(landsv1.BudgetHistory) },
	"Agent":            func() any { return new(landsv1.Agent) },
	"AgentPage":        func() any { return new(landsv1.AgentPage) },
	"AgentCreate":      func() any { return new(landsv1.AgentCreate) },
	"AgentGrant":       func() any { return new(landsv1.AgentGrant) },
	"AgentRefresh":     func() any { return new(landsv1.AgentRefresh) },
	"ReleaseItem":      func() any { return new(landsv1.ReleaseItem) },
	"Deployment":       func() any { return new(landsv1.Deployment) },
	"Release":          func() any { return new(landsv1.Release) },
	"ReleasePage":      func() any { return new(landsv1.ReleasePage) },
	"DeploymentReport": func() any { return new(landsv1.DeploymentReport) },
	"Event":            func() any { return new(landsv1.Event) },
	"Problem":          func() any { return new(landsv1.Problem) },
}

// eventObjects is the schema each event type carries (EventType's
// description in openapi.yaml). TestClosedEnums pins the set of types.
var eventObjects = map[landsv1.EventType]string{
	landsv1.TicketUpdated:    "Ticket",
	landsv1.DecisionOpened:   "Decision",
	landsv1.DecisionResolved: "Decision",
	landsv1.EscalationOpened: "Escalation",
	landsv1.MessagePosted:    "Message",
	landsv1.PlanUpdated:      "Plan",
	landsv1.BudgetUpdated:    "Budget",
	landsv1.ReleaseCut:       "Release",
	landsv1.ReleaseDeployed:  "Release",
}

type validator struct {
	schemas map[string]*jsonschema.Schema
}

func newValidator(t *testing.T) *validator {
	t.Helper()
	raw, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	// YAML -> JSON -> the validator's own number model.
	js, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(js))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020) // OpenAPI 3.1's dialect
	c.AssertFormat()
	if err := c.AddResource(specURL, inst); err != nil {
		t.Fatal(err)
	}
	v := &validator{schemas: map[string]*jsonschema.Schema{}}
	for name := range goTypes {
		s, err := c.Compile(specURL + "#/components/schemas/" + name)
		if err != nil {
			t.Fatalf("compile %s: %v", name, err)
		}
		v.schemas[name] = s
	}
	return v
}

// check runs every guard on one fixture: the JSON Schema, the strict Go
// round-trip, and (for events) the type-to-object binding.
func (v *validator) check(schema string, raw []byte) error {
	if err := v.validate(schema, raw); err != nil {
		return err
	}
	if schema == "Event" {
		var ev struct {
			Type   landsv1.EventType `json:"type"`
			Object json.RawMessage   `json:"object"`
		}
		if err := json.Unmarshal(raw, &ev); err != nil {
			return err
		}
		obj, ok := eventObjects[ev.Type]
		if !ok {
			return fmt.Errorf("event type %q has no object schema", ev.Type)
		}
		if err := v.validate(obj, ev.Object); err != nil {
			return fmt.Errorf("%s object: %w", ev.Type, err)
		}
	}
	return nil
}

func (v *validator) validate(schema string, raw []byte) error {
	s, ok := v.schemas[schema]
	if !ok {
		return fmt.Errorf("no schema %q", schema)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	if err := s.Validate(inst); err != nil {
		return fmt.Errorf("schema: %w", err)
	}
	return roundTrip(goTypes[schema](), raw)
}

// roundTrip decodes raw into the generated Go type, rejecting unknown fields,
// re-encodes it, and requires the same JSON back: the Go binding neither drops
// nor invents a field the fixture has.
func roundTrip(dst any, raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("go decode: %w", err)
	}
	out, err := json.Marshal(dst)
	if err != nil {
		return fmt.Errorf("go encode: %w", err)
	}
	var want, got any
	if err := json.Unmarshal(raw, &want); err != nil {
		return err
	}
	if err := json.Unmarshal(out, &got); err != nil {
		return err
	}
	if !reflect.DeepEqual(want, got) {
		return fmt.Errorf("go round-trip changed the document:\n want %s\n  got %s", bytes.TrimSpace(raw), out)
	}
	return nil
}

// fixtureFiles maps schema name -> fixture paths under dir.
func fixtureFiles(t *testing.T, dir string) map[string][]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == invalidDir {
			continue
		}
		files, err := filepath.Glob(filepath.Join(dir, e.Name(), "*.json"))
		if err != nil {
			t.Fatal(err)
		}
		sort.Strings(files)
		out[e.Name()] = files
	}
	return out
}

func objectSchemas(t *testing.T) []string {
	t.Helper()
	var out []string
	for name, s := range loadSpec(t)["components"].(node)["schemas"].(node) {
		if s.(node)["type"] == "object" {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func TestEverySchemaHasFixtures(t *testing.T) {
	files := fixtureFiles(t, fixturesDir)
	objects := objectSchemas(t)
	for _, name := range objects {
		if _, ok := goTypes[name]; !ok {
			t.Errorf("schema %s has no entry in goTypes", name)
		}
		if len(files[name]) == 0 {
			t.Errorf("schema %s has no golden fixture under %s/%s/", name, fixturesDir, name)
		}
	}
	for name := range goTypes {
		if !slices.Contains(objects, name) {
			t.Errorf("goTypes has %s, which is not an object schema in %s", name, specPath)
		}
	}
	for name := range files {
		if !slices.Contains(objects, name) {
			t.Errorf("%s/%s/ names no object schema in %s", fixturesDir, name, specPath)
		}
	}
	for _, et := range wantEventTypes {
		if _, ok := eventObjects[landsv1.EventType(et)]; !ok {
			t.Errorf("event type %s has no entry in eventObjects", et)
		}
	}
}

func TestEveryEventTypeHasAFixture(t *testing.T) {
	seen := map[string]bool{}
	for _, f := range fixtureFiles(t, fixturesDir)["Event"] {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var ev struct{ Type string }
		if err := json.Unmarshal(raw, &ev); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		seen[ev.Type] = true
	}
	for _, et := range wantEventTypes {
		if !seen[et] {
			t.Errorf("no fixture under %s/Event/ for event type %s", fixturesDir, et)
		}
	}
}

func TestEveryProblemCodeHasAFixture(t *testing.T) {
	seen := map[string]bool{}
	for _, f := range fixtureFiles(t, fixturesDir)["Problem"] {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var p landsv1.Problem
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		seen[string(p.Code)] = true
		if p.Type != "urn:lands:problem:"+string(p.Code) {
			t.Errorf("%s: type %q does not match code %q", f, p.Type, p.Code)
		}
	}
	for _, c := range wantProblemCodes {
		if !seen[c] {
			t.Errorf("no fixture under %s/Problem/ for code %s", fixturesDir, c)
		}
	}
}

func TestFixturesValidate(t *testing.T) {
	v := newValidator(t)
	for schema, files := range fixtureFiles(t, fixturesDir) {
		for _, f := range files {
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			if err := v.check(schema, raw); err != nil {
				t.Errorf("%s: %v", f, err)
			}
		}
	}
}

func TestInvalidFixturesFail(t *testing.T) {
	v := newValidator(t)
	files := fixtureFiles(t, filepath.Join(fixturesDir, invalidDir))
	if len(files) == 0 {
		t.Fatalf("no negative fixtures under %s/%s", fixturesDir, invalidDir)
	}
	for schema, fs := range files {
		for _, f := range fs {
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			err = v.check(schema, raw)
			if err == nil {
				t.Errorf("%s: want a validation failure, got none", f)
				continue
			}
			var ve *jsonschema.ValidationError
			t.Logf("%s rejected (schema error: %v): %s", f, errors.As(err, &ve), firstLine(err))
		}
	}
}

func firstLine(err error) string {
	s, _, _ := strings.Cut(err.Error(), "\n")
	return s
}
