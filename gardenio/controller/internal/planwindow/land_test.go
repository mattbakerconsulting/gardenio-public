package planwindow

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func loadLand(t *testing.T, name string) LandPayload {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "land", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	p, err := DecodeLand(f)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func landNames(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("testdata", "land", "*.json"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no land fixtures: %v", err)
	}
	var names []string
	for _, p := range paths {
		names = append(names, strings.TrimSuffix(filepath.Base(p), ".json"))
	}
	return names
}

func sceneJSON(t *testing.T, s Scene) []byte {
	t.Helper()
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(b, '\n')
}

// TestLandGoldenMatchesKbeads: testdata/land/daemon.json is LW.1's land view
// golden (kbeads/internal/server/testdata/window_land_view.golden.json) byte
// for byte, so the decoder reads what the daemon really sends, and a change
// on either side of the contract fails here first.
func TestLandGoldenMatchesKbeads(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "kbeads", "internal", "server", "testdata", "window_land_view.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join("testdata", "land", "daemon.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Error("testdata/land/daemon.json differs from kbeads' window_land_view.golden.json; copy that file over this one and rerun go test -run TestSceneGoldens -update")
	}
	p := loadLand(t, "daemon")
	if p.Land != "claimlayer" || len(p.Plans) != 3 || p.OpenPlans != 2 || p.ExpiresAt.IsZero() || p.UpdatedAt.IsZero() {
		t.Fatalf("land view = %+v", p)
	}
	first := p.Plans[0]
	if first.Title != "The plan" || first.State != "building" || first.CurrentPhase != 1 || first.StepsDone != 1 || first.StepsTotal != 3 ||
		len(first.Phases) != 1 || len(first.Phases[0].Steps) != 2 || first.Phases[0].Steps[1].DoneAt == nil || len(first.Steps) != 1 ||
		first.OpenEscalations != 1 || len(first.Crew) != 1 || !first.Crew[0].Live || first.UpdatedAt.IsZero() {
		t.Errorf("first plan = %+v", first)
	}
	if second := p.Plans[1]; second.Heraldry != "🔥🧭📦" || second.ShortTitle != "second plan" || second.OpenDecisions != 1 || len(second.Decisions) != 1 || second.Decisions[0] != "Ship it?" {
		t.Errorf("second plan = %+v", second)
	}
	if v := CleanLand(p); len(v.Withheld) != 0 {
		t.Errorf("the daemon's golden withheld %+v", v.Withheld)
	}
}

func TestDecodeLand_Rejects(t *testing.T) {
	plan, err := os.ReadFile(filepath.Join("testdata", "payload", "daemon.json"))
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"a plan window's payload": string(plan),
		"no land":                 `{"land":"  ","plans":[]}`,
		"broken":                  `{"land":`,
		"over the cap":            `{"land":"x","plans":[],"pad":"` + strings.Repeat("a", MaxPayloadBytes) + `"}`,
	} {
		if _, err := DecodeLand(strings.NewReader(body)); err == nil {
			t.Errorf("%s decoded as a land view", name)
		}
	}
}

// TestCleanLand_Withholds: a hit replaces the field, is listed for the
// owner's preview by path, and is counted by field and rule; the rest is
// trimmed and normalised.
func TestCleanLand_Withholds(t *testing.T) {
	id := `re:\b(gd|ka)-[A-Za-z0-9]{10}\b`
	stepBefore := testutil.ToFloat64(WithheldTotal.WithLabelValues(FieldStepTitle, id))

	v := CleanLand(loadLand(t, "mixed"))

	want := []Withheld{
		{Path: "plans[0].phases[1].steps[1].title", Field: FieldStepTitle, Rule: id},
		{Path: "plans[0].crew[2].name", Field: FieldAgentName, Rule: "bead"},
		{Path: "plans[3].title", Field: FieldPlanTitle, Rule: RuleEmail},
		{Path: "plans[3].steps[1].title", Field: FieldStepTitle, Rule: "daemon"},
		{Path: "plans[3].crew[1].name", Field: FieldAgentName, Rule: "bead"},
	}
	if !reflect.DeepEqual(v.Withheld, want) {
		t.Errorf("Withheld =\n%+v\nwant\n%+v", v.Withheld, want)
	}
	if d := testutil.ToFloat64(WithheldTotal.WithLabelValues(FieldStepTitle, id)) - stepBefore; d != 1 {
		t.Errorf("step_title/id counted %v; want 1", d)
	}
	st := v.Plans[0].Phases[1].Steps[1]
	if st.Title != WithheldText || st.ShortTitle != "rotate the token" {
		t.Errorf("planted step = %+v; want its title withheld and its clean short title kept", st)
	}
	if v.Land != "Orchard" || v.Plans[3].Title != WithheldText || v.Plans[3].ShortTitle != "water rights" {
		t.Errorf("land %q, plan 3 %q / %q", v.Land, v.Plans[3].Title, v.Plans[3].ShortTitle)
	}
	if s := v.Plans[3].Steps[0]; s.Status != "open" || s.Kind != "task" {
		t.Errorf("an unknown status and kind = %+v; want open and task", s)
	}
	if !v.Plans[4].Withheld || len(v.Plans[4].Phases) != 0 || v.Plans[4].Crew == nil || v.Plans[4].Decisions == nil {
		t.Errorf("a headline-only plan = %+v; want withheld kept and empty lists, not nulls", v.Plans[4])
	}

	// A secret-shaped title planted into the daemon's own golden is withheld
	// the same way, and the payload it came from is left alone.
	p := loadLand(t, "daemon")
	p.Plans[1].Title = "Rotate the key in gd-Q1w2E3r4T5 tonight"
	p.Plans[1].Decisions = []string{"Mail admin@example.org?"}
	v = CleanLand(p)
	if v.Plans[1].Title != WithheldText || v.Plans[1].Decisions[0] != WithheldText || len(v.Withheld) != 2 {
		t.Errorf("planted golden: title %q, decisions %q, withheld %+v", v.Plans[1].Title, v.Plans[1].Decisions, v.Withheld)
	}
	if p.Plans[1].Title == WithheldText {
		t.Error("CleanLand changed its input")
	}
	if s := ToScene(v); strings.Contains(string(sceneJSON(t, s)), "gd-Q1w2E3r4T5") || strings.Contains(string(sceneJSON(t, s)), "admin@") {
		t.Error("the scene carries a withheld field")
	}
}

// TestSceneGoldens holds the Scene of every land fixture against its golden
// in testdata/scene (go test -run TestSceneGoldens -update rewrites them).
func TestSceneGoldens(t *testing.T) {
	for _, name := range landNames(t) {
		t.Run(name, func(t *testing.T) {
			got := sceneJSON(t, ToScene(CleanLand(loadLand(t, name))))
			path := filepath.Join("testdata", "scene", name+".scene.json")
			if *update {
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (go test -run TestSceneGoldens -update writes it)", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("scene for %s differs from %s (go test -run TestSceneGoldens -update rewrites it)\ngot:\n%s", name, path, got)
			}
		})
	}
}

func TestToScene(t *testing.T) {
	s := ToScene(CleanLand(loadLand(t, "mixed")))
	var plans, seeds, closed []string
	for _, p := range s.Plans {
		plans = append(plans, p.ID)
	}
	for _, sd := range s.Seeds {
		seeds = append(seeds, sd.ID+":"+sd.State)
	}
	for _, c := range s.ClosedPlans.Shown {
		closed = append(closed, c.ID)
	}
	if got := strings.Join(plans, ","); got != "plan-1,plan-4,plan-5" {
		t.Errorf("plans = %s; want the open plans being built", got)
	}
	if got := strings.Join(seeds, ","); got != "plan-2:frosted,plan-3:ready" {
		t.Errorf("seeds = %s; want the draft frosted and the ready plan staked", got)
	}
	if got := strings.Join(closed, ","); got != "plan-6" || s.ClosedPlans.Total != 1 {
		t.Errorf("closed plans = %s (total %d)", got, s.ClosedPlans.Total)
	}

	p := s.Plans[0]
	if p.Project != "Orchard" || p.Created != 1 || p.Updated == 0 || p.Activity != p.Updated || p.Heraldry != "🌳🍎🧺" {
		t.Errorf("plan headline = %+v", p)
	}
	if p.Counts != (SceneCounts{InProgress: 1, Blocked: 1, Deferred: 1, Closed: 2}) || p.OpenDecisions != 1 || p.OpenEscalations != 1 {
		t.Errorf("plan counts = %+v, decisions %d, escalations %d", p.Counts, p.OpenDecisions, p.OpenEscalations)
	}
	dig, plant := p.Phases[0], p.Phases[1]
	if dig.ID != "plan-1/phase-1" || len(dig.Tasks) != 0 || len(dig.Closed) != 2 || dig.Closed[1].ID != "plan-1/phase-1/step-2" || dig.Closed[1].Type != "chore" || dig.Closed[1].Updated == 0 {
		t.Errorf("closed phase = %+v", dig)
	}
	if len(plant.Tasks) != 3 || plant.Tasks[1].State != "blocked" || plant.Tasks[1].Title != WithheldText || plant.Tasks[2].Created != 5 {
		t.Errorf("open phase = %+v", plant)
	}
	if len(p.Waiting) != 1 || p.Waiting[0] != (SceneWait{ID: "plan-1/decision-1", Title: "Which apples first?", Kind: "decision", State: "unanswered", Human: true}) {
		t.Errorf("waiting = %+v; want the open decision only, no escalation by name", p.Waiting)
	}

	flat := s.Plans[1]
	if len(flat.Phases) != 0 || len(flat.Unphased.Tasks) != 2 || flat.Unphased.Tasks[0].ID != "plan-4/step-1" || flat.State != "" {
		t.Errorf("flat plan = %+v", flat)
	}
	if staked := s.Seeds[1]; staked.Tasks != 2 || len(staked.Steps) != 2 || staked.Steps[0] != (SceneSeedStep{ID: "plan-3/step-2", Title: "Frame the walls", N: 1, Status: "open"}) || staked.ShortTitle != "build the shed" {
		t.Errorf("ready seed = %+v; want its open steps", staked)
	}
	if frosted := s.Seeds[0]; frosted.Tasks != 2 || frosted.Steps != nil || frosted.ShortTitle != "" {
		t.Errorf("draft seed = %+v; want counts only", frosted)
	}

	type who struct {
		bucket, plot string
	}
	got := map[string]who{}
	for _, g := range s.Gardeners {
		plot := "home"
		if g.Plot != nil {
			plot = g.Plot.Plan
		}
		got[g.ID+" "+g.Agent] = who{g.Bucket, plot}
		if g.Role != "crew" || g.Project != "Orchard" || g.Task != "" || g.Reason != nil {
			t.Errorf("gardener = %+v", g)
		}
	}
	want := map[string]who{
		"crew-1 crew-a":          {"alive", "plan-1"},
		"crew-2 crew-b":          {"alive", "plan-3"}, // not live on plan-1, live on the staked plan-3
		"crew-3 " + WithheldText: {"alive", "plan-1"},
		"crew-4 " + WithheldText: {"dormant", "plan-4"}, // a withheld handle is never merged
		"crew-5 crew-c":          {"dormant", "home"},   // on a closed plan only
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("gardeners = %+v\nwant %+v", got, want)
	}
}

// sceneForbidden are farm document keys a land window's scene never carries:
// the purse, the wishes, the meadow, the piles, the meta, people and links.
var sceneForbidden = map[string]bool{
	"cost": true, "usd": true, "purse": true, "wishes": true, "meadow": true, "piles": true, "meta": true, "produce": true,
	"operator": true, "owner": true, "assignee": true, "href": true, "chat": true, "url": true, "mr": true, "summary": true,
	"decision": true, "thread_title": true, "task_title": true, "reason_text": true, "description": true, "email": true,
}

// TestScene_NeverList walks every key and string of every fixture's scene.
func TestScene_NeverList(t *testing.T) {
	for _, name := range landNames(t) {
		var doc any
		if err := json.Unmarshal(sceneJSON(t, ToScene(CleanLand(loadLand(t, name)))), &doc); err != nil {
			t.Fatal(err)
		}
		var walk func(path string, x any)
		walk = func(path string, x any) {
			switch x := x.(type) {
			case map[string]any:
				for k, v := range x {
					if sceneForbidden[k] {
						t.Errorf("%s: scene key %s.%s", name, path, k)
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
			}
		}
		walk("$", doc)
	}
}

// sceneFarmTypes pairs each Scene type with the gardenhub farm type it is a
// subset of.
var sceneFarmTypes = map[reflect.Type]string{
	reflect.TypeOf(ScenePlan{}):        "FarmPlan",
	reflect.TypeOf(ScenePhase{}):       "FarmPhase",
	reflect.TypeOf(SceneCounts{}):      "FarmCounts",
	reflect.TypeOf(SceneTask{}):        "FarmTask",
	reflect.TypeOf(SceneClosedStep{}):  "FarmClosedStep",
	reflect.TypeOf(SceneWait{}):        "FarmWait",
	reflect.TypeOf(SceneSeed{}):        "FarmSeed",
	reflect.TypeOf(SceneSeedStep{}):    "FarmSeedStep",
	reflect.TypeOf(SceneGardener{}):    "FarmGardener",
	reflect.TypeOf(ScenePlot{}):        "FarmPlot",
	reflect.TypeOf(SceneClosedPlans{}): "FarmClosedPlans",
	reflect.TypeOf(SceneClosedPlan{}):  "FarmClosedPlan",
}

// TestSceneKeysAreFarmKeys: every key of every Scene type is a key of its
// gardenhub Farm* type, read from gardenhub/farm_model.go's source (the
// package is a leaf, so it parses the file rather than import it), and
// Scene's own keys are farm document keys. A farm key renamed or dropped
// fails here, before world.js reads a key the scene no longer sends.
func TestSceneKeysAreFarmKeys(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), filepath.Join("..", "gardenhub", "farm_model.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	farm := map[string]map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok {
			return true
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			return true
		}
		keys := map[string]bool{}
		for _, fl := range st.Fields.List {
			if fl.Tag == nil {
				continue
			}
			tag, _ := strconv.Unquote(fl.Tag.Value)
			if name, _, _ := strings.Cut(reflect.StructTag(tag).Get("json"), ","); name != "" && name != "-" {
				keys[name] = true
			}
		}
		farm[ts.Name.Name] = keys
		return false
	})
	for typ, farmName := range sceneFarmTypes {
		keys, ok := farm[farmName]
		if !ok {
			t.Errorf("gardenhub/farm_model.go has no type %s", farmName)
			continue
		}
		for i := 0; i < typ.NumField(); i++ {
			if name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ","); !keys[name] {
				t.Errorf("%s.%s is %q, which %s does not have", typ.Name(), typ.Field(i).Name, name, farmName)
			}
		}
	}
	// The document's own keys (gardenhub Farm.snapshotLocked).
	doc := map[string]bool{"plans": true, "seeds": true, "gardeners": true, "closed_plans": true}
	st := reflect.TypeOf(Scene{})
	for i := 0; i < st.NumField(); i++ {
		if name, _, _ := strings.Cut(st.Field(i).Tag.Get("json"), ","); !doc[name] {
			t.Errorf("Scene.%s is %q, not a farm document key", st.Field(i).Name, name)
		}
	}
}

// TestSceneLaysOutUnderNode: the mixed fixture's scene goes through
// gardenhub's world.js as it is (testdata/scene_layout.js): one biome for the
// land, a plot per plan being built and per ready seed, a bed per phase, a
// cell per step, the draft in the nursery and the closed plan in the
// orchard, and the live crew on their plots.
func TestSceneLaysOutUnderNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("GARDENIO_NODE_REQUIRED") == "1" {
			t.Fatalf("GARDENIO_NODE_REQUIRED=1 and node is not on PATH: %v", err)
		}
		t.Skip("node not on PATH")
	}
	world, err := filepath.Abs(filepath.Join("..", "gardenhub", "web", "world.js"))
	if err != nil {
		t.Fatal(err)
	}
	scene := filepath.Join(t.TempDir(), "scene.json")
	if err := os.WriteFile(scene, sceneJSON(t, ToScene(CleanLand(loadLand(t, "mixed")))), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, filepath.Join("testdata", "scene_layout.js"), world, scene).CombinedOutput()
	if err != nil {
		t.Fatalf("node scene_layout.js failed: %v\n%s", err, out)
	}
	type bed struct {
		Title  string   `json:"title"`
		Cells  []string `json:"cells"`
		Done   int      `json:"done"`
		Total  int      `json:"total"`
		Closed bool     `json:"closed"`
	}
	type plot struct {
		ID     string `json:"id"`
		Staked bool   `json:"staked"`
		Needs  int    `json:"needs"`
		Beds   []bed  `json:"beds"`
	}
	var r struct {
		Biome struct {
			Key   string `json:"key"`
			Name  string `json:"name"`
			Needs int    `json:"needs"`
		} `json:"biome"`
		Plots     []plot            `json:"plots"`
		Nursery   []string          `json:"nursery"`
		Orchard   []string          `json:"orchard"`
		Gardeners map[string]string `json:"gardeners"`
		Board     struct {
			Head string   `json:"head"`
			Rows []string `json:"rows"`
		} `json:"board"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if r.Biome.Key != "orchard" || r.Biome.Name != "Orchard" || r.Biome.Needs != 4 {
		t.Errorf("biome = %+v; want the land, needing the open decisions and escalations of the plans being built (a staked seed names no decision)", r.Biome)
	}
	var ids []string
	for _, p := range r.Plots {
		ids = append(ids, p.ID)
	}
	if got := strings.Join(ids, ","); got != "plan-1,plan-3,plan-4,plan-5" {
		t.Errorf("plots = %s; want the plans being built and the staked seed, in the land's order", got)
	}
	want := []bed{
		{Title: "Dig", Cells: []string{"plan-1/phase-1/step-1", "plan-1/phase-1/step-2"}, Done: 2, Total: 2, Closed: true},
		{Title: "Plant", Cells: []string{"plan-1/phase-2/step-1", "plan-1/phase-2/step-2", "plan-1/phase-2/step-3"}, Done: 0, Total: 3},
	}
	if len(r.Plots) == 4 {
		if p := r.Plots[0]; !reflect.DeepEqual(p.Beds, want) || p.Needs != 2 {
			t.Errorf("plan-1's plot = %+v\nwant beds %+v and 2 needs", p, want)
		}
		if p := r.Plots[1]; !p.Staked || len(p.Beds) != 1 || strings.Join(p.Beds[0].Cells, ",") != "plan-3/step-2,plan-3/step-3" {
			t.Errorf("the staked seed's plot = %+v", p)
		}
		if p := r.Plots[2]; len(p.Beds) != 1 || p.Beds[0].Total != 2 || p.Needs != 2 {
			t.Errorf("the flat plan's plot = %+v", p)
		}
		if p := r.Plots[3]; len(p.Beds) != 0 {
			t.Errorf("the headline-only plan's plot = %+v; want no beds", p)
		}
	}
	if strings.Join(r.Nursery, ",") != "plan-2" || strings.Join(r.Orchard, ",") != "plan-6" {
		t.Errorf("nursery %v, orchard %v", r.Nursery, r.Orchard)
	}
	if g := r.Gardeners; !reflect.DeepEqual(g, map[string]string{"crew-1": "plan-1", "crew-2": "plan-3", "crew-3": "plan-1", "crew-4": "home", "crew-5": "home"}) {
		t.Errorf("gardeners = %v; want the live crew on their plots and the rest (crew-4 is not live) at the barn", g)
	}
	if !strings.HasPrefix(r.Board.Head, "4 plans") || len(r.Board.Rows) != 4 {
		t.Errorf("board = %+v", r.Board)
	}
}
