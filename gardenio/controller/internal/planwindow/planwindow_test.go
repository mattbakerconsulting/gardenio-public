package planwindow

import (
	"bytes"
	"encoding/json"
	"flag"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

var update = flag.Bool("update", false, "rewrite the plan.json goldens in testdata/plan/")

func loadPayload(t *testing.T, name string) Payload {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "payload", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	p, err := Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func fixtureNames(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("testdata", "payload", "*.json"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no payload fixtures: %v", err)
	}
	var names []string
	for _, p := range paths {
		names = append(names, strings.TrimSuffix(filepath.Base(p), ".json"))
	}
	return names
}

func planJSON(t *testing.T, v View) []byte {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(b, '\n')
}

// TestGoldens holds plan.json for every payload fixture against its golden.
func TestGoldens(t *testing.T) {
	for _, name := range fixtureNames(t) {
		t.Run(name, func(t *testing.T) {
			got := planJSON(t, Build(loadPayload(t, name)))
			path := filepath.Join("testdata", "plan", name+".plan.json")
			if *update {
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (go test -run TestGoldens -update writes it)", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("plan.json for %s differs from %s (go test -run TestGoldens -update rewrites it)\ngot:\n%s", name, path, got)
			}
		})
	}
}

// TestDenylistMatchesContract: the embedded list is the contract's, byte for
// byte.
func TestDenylistMatchesContract(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "lands", "contract", "hygiene_denylist.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if denylistText != string(want) {
		t.Error("hygiene_denylist.txt differs from lands/contract/hygiene_denylist.txt; copy the contract's file over this one")
	}
}

func TestDenylistParses(t *testing.T) {
	r, err := parseDenylist(denylistText)
	if err != nil {
		t.Fatal(err)
	}
	if len(r) < 5 {
		t.Errorf("parsed %d rules; the list has more", len(r))
	}
	if _, err := parseDenylist("# only a comment\n\n"); err == nil {
		t.Error("an empty list parsed")
	}
	if _, err := parseDenylist("re:(unclosed\n"); err == nil {
		t.Error("a bad regex parsed")
	}
}

func TestCheck(t *testing.T) {
	for _, tc := range []struct {
		in   string
		rule string
	}{
		{"", ""},
		{"Offer and claim a packet", ""},
		{"Read a Bead", "bead"},
		{"the KBEADS window bearer", "bead"},
		{"follow up on gd-pz87vf3xsB", `re:\b(gd|ka)-[A-Za-z0-9]{10}\b`},
		{"GET /v1/decisions/x", `re:/v1/(decisions|escalations|events|bus|search|accounts|plans|threads)\b`},
		{"ask someone@example.com", RuleEmail},
		{"write to a.b+c@sub.example.org today", RuleEmail},
		{"@gardenio tend", ""},
	} {
		rule, hit := Check(tc.in)
		if hit != (tc.rule != "") || rule != tc.rule {
			t.Errorf("Check(%q) = %q, %v; want %q", tc.in, rule, hit, tc.rule)
		}
	}
}

// TestBuild_Withholds: a hit replaces the field, is listed for the preview by
// path, and is counted by field and rule.
func TestBuild_Withholds(t *testing.T) {
	stepBefore := testutil.ToFloat64(WithheldTotal.WithLabelValues(FieldStepTitle, "bead"))
	emailBefore := testutil.ToFloat64(WithheldTotal.WithLabelValues(FieldNarrative, RuleEmail))

	v := Build(loadPayload(t, "flat"))

	if got := v.Phases[0].Steps[0].Title; got != WithheldText {
		t.Errorf("step 0 title = %q; want it withheld", got)
	}
	if got := v.Phases[0].Steps[1].Title; got != "PW.2 Read model" {
		t.Errorf("step 1 title = %q; want it kept", got)
	}
	id := `re:\b(gd|ka)-[A-Za-z0-9]{10}\b`
	want := []Withheld{
		{Path: "phases[0].steps[0].title", Field: FieldStepTitle, Rule: "bead"},
		{Path: "phases[0].steps[0].short_title", Field: FieldStepTitle, Rule: "bead"},
		{Path: "phases[0].steps[3].title", Field: FieldStepTitle, Rule: id},
		{Path: "phases[0].steps[4].title", Field: FieldStepTitle, Rule: RuleEmail},
		{Path: "crew[0].name", Field: FieldAgentName, Rule: "bead"},
		{Path: "recent[1].title", Field: FieldRecentTitle, Rule: "bead"},
		{Path: "headline", Field: FieldNarrative, Rule: "daemon"},
		{Path: "narrative[0].blocks[2]", Field: FieldNarrative, Rule: "daemon"},
		{Path: "narrative[0].blocks[3]", Field: FieldNarrative, Rule: id},
		{Path: "narrative[0].blocks[4]", Field: FieldNarrative, Rule: RuleEmail},
	}
	if !reflect.DeepEqual(v.Withheld, want) {
		t.Errorf("Withheld =\n%+v\nwant\n%+v", v.Withheld, want)
	}
	if d := testutil.ToFloat64(WithheldTotal.WithLabelValues(FieldStepTitle, "bead")) - stepBefore; d != 2 {
		t.Errorf("step_title/bead counted %v; want 2", d)
	}
	if d := testutil.ToFloat64(WithheldTotal.WithLabelValues(FieldNarrative, RuleEmail)) - emailBefore; d != 1 {
		t.Errorf("narrative/email counted %v; want 1", d)
	}
	if strings.Contains(string(planJSON(t, v)), "phases[0]") {
		t.Error("plan.json carries the Withheld list; it is the owner's preview only")
	}
}

// forbiddenKeys are payload-side or daemon-side keys plan.json must never
// carry (PW-D4's never list).
var forbiddenKeys = map[string]bool{
	"id": true, "description": true, "prompt": true, "context": true, "comments": true,
	"created_by": true, "operated_by": true, "requested_by": true, "assignee": true, "owner": true,
	"email": true, "url": true, "mr_url": true, "session_id": true, "spend": true, "purse": true,
	"sensitivity": true, "operation_type": true, "subject": true, "fields": true, "labels": true,
}

var beadID = regexp.MustCompile(`\b(gd|ka)-[A-Za-z0-9]{10}\b`)

// operationTypes are the fixtures' escalation operation types; plan.json
// names what an escalation needs, never its type.
var operationTypes = []string{"github_pr_merge", "gitlab_mr_merge", "helm_deploy", "keycloak_idp_rollout"}

// TestPlanJSON_NeverList walks every key and string of every fixture's
// plan.json.
func TestPlanJSON_NeverList(t *testing.T) {
	for _, name := range fixtureNames(t) {
		var doc any
		if err := json.Unmarshal(planJSON(t, Build(loadPayload(t, name))), &doc); err != nil {
			t.Fatal(err)
		}
		var walk func(path string, x any)
		walk = func(path string, x any) {
			switch x := x.(type) {
			case map[string]any:
				for k, v := range x {
					if forbiddenKeys[k] {
						t.Errorf("%s: plan.json key %s.%s", name, path, k)
					}
					walk(path+"."+k, v)
				}
			case []any:
				for i, v := range x {
					walk(path+"["+strconv.Itoa(i)+"]", v)
				}
			case string:
				if rule, hit := Check(x); hit {
					t.Errorf("%s: %s = %q breaks %s", name, path, x, rule)
				}
				if beadID.MatchString(x) || strings.Contains(x, "://") {
					t.Errorf("%s: %s = %q holds an id or a URL", name, path, x)
				}
				for _, op := range operationTypes {
					if strings.Contains(x, op) {
						t.Errorf("%s: %s = %q names an operation type", name, path, x)
					}
				}
			}
		}
		walk("$", doc)
	}
}

func TestBuild_Phased(t *testing.T) {
	v := Build(loadPayload(t, "phased"))
	if v.State != "building" || v.StateLabel != "Building" {
		t.Errorf("state = %q %q", v.State, v.StateLabel)
	}
	if v.Progress != (Progress{Done: 3, Total: 8, Percent: 37}) {
		t.Errorf("progress = %+v", v.Progress)
	}
	if v.Phase != "The swap table" {
		t.Errorf("current phase = %q", v.Phase)
	}
	var statuses []string
	for _, ph := range v.Phases {
		statuses = append(statuses, ph.Status)
	}
	if got := strings.Join(statuses, ","); got != "done,working,todo" {
		t.Errorf("phase statuses = %s", got)
	}
	if v.Waiting.Escalations.Text != "2 waiting, 1 needs a deploy, 1 needs a merge" || v.Waiting.Escalations.Total != 3 {
		t.Errorf("escalations = %+v", v.Waiting.Escalations)
	}
	var states []string
	for _, d := range v.Waiting.Decisions {
		states = append(states, d.State)
	}
	if got := strings.Join(states, ","); got != "open,resolved,cancelled" || v.Waiting.Decisions[1].Chosen != "Spring and summer" {
		t.Errorf("decisions = %+v; want open, then resolved with its choice, then cancelled", v.Waiting.Decisions)
	}
	if c := v.Crew; len(c) != 3 || !c[0].Live || !c[1].Live || c[2].Live || c[0].Name > c[1].Name {
		t.Errorf("crew = %+v; want live first, then by name", c)
	}
	if len(v.Recent) != 10 || v.Recent[0].Kind != KindEscalationOpened || v.Recent[0].Title != "a merge" {
		t.Errorf("recent = %+v; want the daemon's ten, escalations named by what they need", v.Recent)
	}
	if len(v.Withheld) != 0 {
		t.Errorf("the clean fixture withheld %+v", v.Withheld)
	}
}

func TestBuild_Recent(t *testing.T) {
	v := Build(loadPayload(t, "flat"))
	if len(v.Recent) != RecentLimit {
		t.Fatalf("%d recent rows; want %d", len(v.Recent), RecentLimit)
	}
	for i := 1; i < len(v.Recent); i++ {
		if v.Recent[i].At.After(v.Recent[i-1].At) {
			t.Errorf("recent[%d] is newer than recent[%d]", i, i-1)
		}
	}
	if r := v.Recent[0]; r.Kind != KindEscalationOpened || r.Title != "an operator" {
		t.Errorf("newest = %+v; want the unsorted escalation first, an unknown kind dropped", r)
	}
	for _, r := range v.Recent {
		if r.Title == "Clean step 1" || r.Title == "No time" || r.Kind == "comment_added" {
			t.Errorf("kept %+v", r)
		}
	}

	// Ties keep the daemon's order.
	at := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	v = Build(Payload{Recent: []ChangeFacts{
		{At: at, Kind: KindStepClosed, Subject: "first"},
		{At: at, Kind: KindStepClosed, Subject: "second"},
	}})
	if v.Recent[0].Title != "first" || v.Recent[1].Title != "second" {
		t.Errorf("tied rows = %+v", v.Recent)
	}
}

func TestBuild_Flat(t *testing.T) {
	v := Build(loadPayload(t, "flat"))
	if len(v.Phases) != 1 || v.Phases[0].Title != "" || len(v.Phases[0].Steps) != 5 {
		t.Fatalf("phases = %+v; want one untitled phase with every step", v.Phases)
	}
	if v.Phase != "" || v.Progress != (Progress{Done: 1, Total: 5, Percent: 20}) {
		t.Errorf("phase %q progress %+v", v.Phase, v.Progress)
	}
	if k := v.Phases[0].Steps[4].Kind; k != "task" {
		t.Errorf("kind = %q; a type off the allowlist reads task", k)
	}
	if v.Waiting.Escalations.Text != "3 waiting, 2 need a merge" || v.Waiting.Escalations.Total != 4 {
		t.Errorf("escalations = %+v", v.Waiting.Escalations)
	}
	if d := v.Waiting.Decisions; len(d) != 2 || d[1].State != DecisionOpen {
		t.Errorf("decisions = %+v; an unknown state reads open", d)
	}
	if len(v.Crew) != 2 {
		t.Errorf("crew = %+v; an agent with no name is dropped", v.Crew)
	}
	if len(v.Narrative) != 1 || len(v.Narrative[0].Blocks) != 6 || v.Narrative[0].Blocks[0] != "# Lands: the Window" {
		t.Errorf("narrative = %+v; want one doc of six blocks, the empty doc dropped", v.Narrative)
	}
}

func TestBuild_Empty(t *testing.T) {
	v := Build(loadPayload(t, "empty"))
	if v.State != "draft" || v.Progress != (Progress{}) || v.Phase != "" {
		t.Errorf("view = %+v", v)
	}
	if len(v.Phases) != 1 || v.Phases[0].Steps == nil || v.Crew == nil || v.Recent == nil || v.Narrative == nil || v.Waiting.Decisions == nil {
		t.Error("an empty plan must still give the page empty lists, not nulls")
	}
}

// TestBuild_Fallback: with no steps_total the read model counts the steps and
// finds the current phase itself; otherwise the daemon's numbers stand.
func TestBuild_Fallback(t *testing.T) {
	p := loadPayload(t, "phased")
	p.Plan.StepsDone, p.Plan.StepsTotal, p.Plan.CurrentPhase = 0, 0, 0
	if v := Build(p); v.Progress != (Progress{Done: 3, Total: 8, Percent: 37}) || v.Phase != "The swap table" {
		t.Errorf("fallback progress %+v phase %q", v.Progress, v.Phase)
	}

	p = loadPayload(t, "phased")
	p.Plan.StepsDone, p.Plan.StepsTotal, p.Plan.CurrentPhase = 4, 9, 3
	if v := Build(p); v.Progress != (Progress{Done: 4, Total: 9, Percent: 44}) || v.Phase != "Launch" {
		t.Errorf("daemon progress %+v phase %q; the daemon's numbers win", v.Progress, v.Phase)
	}

	p.Plan.CurrentPhase = 0
	if v := Build(p); v.Phase != "" {
		t.Errorf("current_phase 0 gave phase %q; want none", v.Phase)
	}
	p.Plan.CurrentPhase = 7
	if v := Build(p); v.Phase != "The swap table" {
		t.Errorf("an out-of-range current_phase gave %q; want the read model's own", v.Phase)
	}
	p.Plan.StepsDone = 12
	if v := Build(p); v.Progress.Done != 9 || v.Progress.Percent != 100 {
		t.Errorf("steps_done over steps_total = %+v; want it clamped", v.Progress)
	}
}

func TestBuild_UnknownState(t *testing.T) {
	for _, s := range []string{"", "active", "weird"} {
		if v := Build(Payload{Plan: PlanFacts{State: s}}); v.State != "active" || v.StateLabel != "Active" {
			t.Errorf("state %q -> %q %q", s, v.State, v.StateLabel)
		}
	}
}

func TestBuild_StepsBesidePhases(t *testing.T) {
	v := Build(Payload{
		Phases: []PhaseFacts{{Title: "Only", Steps: []StepFacts{{Title: "placed"}}}, {Title: "Empty and closed", Status: "closed"}},
		Steps:  []StepFacts{{Title: "loose"}},
	})
	if len(v.Phases) != 3 || v.Phases[2].Title != OtherStepsTitle || len(v.Phases[2].Steps) != 1 {
		t.Errorf("phases = %+v; want the top-level steps in a trailing %q phase", v.Phases, OtherStepsTitle)
	}
	if v.Phases[1].Status != StatusDone {
		t.Errorf("a closed phase with no steps reads %q", v.Phases[1].Status)
	}
}

// TestDaemonGoldenMatchesKbeads: testdata/payload/daemon.json is PW.1's
// windowView golden (kbeads/internal/server/testdata/window_view.golden.json)
// byte for byte, so TestGoldens decodes and builds what the daemon really
// sends, and a change on either side of the contract fails here first.
func TestDaemonGoldenMatchesKbeads(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "kbeads", "internal", "server", "testdata", "window_view.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join("testdata", "payload", "daemon.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Error("testdata/payload/daemon.json differs from kbeads' window_view.golden.json; copy that file over this one and rerun go test -run TestGoldens -update")
	}
	v := Build(loadPayload(t, "daemon"))
	if len(v.Phases) != 2 || v.Phases[0].Title != "Phase one" || v.Phases[1].Title != OtherStepsTitle || len(v.Phases[1].Steps) != 1 {
		t.Errorf("phases = %+v; want the daemon's one phase, then its top-level step under %q", v.Phases, OtherStepsTitle)
	}
	if v.Progress != (Progress{Done: 1, Total: 3, Percent: 33}) || v.Phase != "Phase one" {
		t.Errorf("progress = %+v, phase %q; want the daemon's", v.Progress, v.Phase)
	}
	if v.Waiting.Escalations.Text != "1 waiting, 1 needs a merge" || len(v.Recent) != 4 || len(v.Withheld) != 0 {
		t.Errorf("waiting = %+v, recent = %+v, withheld = %+v", v.Waiting, v.Recent, v.Withheld)
	}
}

func TestDecode_Cap(t *testing.T) {
	big := `{"narrative":"` + strings.Repeat("a", MaxPayloadBytes) + `"}`
	if _, err := Decode(strings.NewReader(big)); err == nil {
		t.Error("a payload over the cap decoded")
	}
	if _, err := Decode(strings.NewReader("{")); err == nil {
		t.Error("a broken payload decoded")
	}
}

// TestLeaf: the package imports nothing from this repo.
func TestLeaf(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); strings.HasPrefix(p, "gardenio/") {
				t.Errorf("%s imports %s; planwindow is a leaf", name, p)
			}
		}
	}
}
