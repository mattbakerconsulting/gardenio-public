package gardenhub

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// The Workbench panels under the Commons UX rubric's stress, empty and degraded fixtures (gardenio/docs/commons-ux-rubric.md
// §1; gd-wSb1jRJnJm). A request carrying X-UX-Fixture asks the card world's Workbench readers (card_browser_test.go) for
// another world, as X-UX-Writer asks for a writer: "stress" F1 long human text, F2 unbroken tokens, F3 many rows, F4 emoji
// and non-Latin and F6 references the daemon does not have; "empty" a land with nothing in it yet; "down" a beads daemon that
// does not answer (WB-15). The commons-ux-acceptance skill's shoot.js rows set it; the goldens and the browser walks never
// do, so what they guard is the fixture farm as it was.
const uxFixtureHeader = "X-UX-Fixture"

type uxFixtureKey struct{}

// withUXFixture puts the request's X-UX-Fixture in its context, where the readers below look for it.
func withUXFixture(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v := r.Header.Get(uxFixtureHeader); v != "" {
			r = r.WithContext(context.WithValue(r.Context(), uxFixtureKey{}, v))
		}
		h.ServeHTTP(w, r)
	})
}

func uxFixture(ctx context.Context) string { v, _ := ctx.Value(uxFixtureKey{}).(string); return v }

// errUXDown is what a reader returns under X-UX-Fixture: down, as the daemon's client does when nothing answers: the
// handlers answer 502.
var errUXDown = errors.New("beads daemon: dial tcp 10.0.0.7:9090: connect: connection refused")

// The rubric's stress strings: F1 a 200-char title and a 90-char agent name, F2 a 120-char URL, a 40-char SHA, a branch and a
// 60-char path, F4 a ZWJ emoji, a flag, VS16, CJK, Arabic and a combining accent.
var (
	uxURL    = "https://gitlab.example.com/gardenio/controller/-/merge_requests/4242/diffs?commit_id=0123456789abcdef0123456789abcdef01"
	uxSHA    = "0123456789abcdef0123456789abcdef01234567"
	uxBranch = "agent/some-very-long-branch-name/gd-AbCdEfGhIj"
	uxPath   = "gardenio/controller/internal/gardenhub/web/workbench.js:1292"
	uxAgent  = "gardenio-crew-" + strings.Repeat("very-long-agent-name-", 4)[:76]
	uxF4     = "🧑‍🌾 Farmer note, 🇳🇿 flag, ⚠️ warning with VS16, 田中花子 and مريم reviewed it; café"
	uxTokens = "See " + uxURL + " at " + uxSHA + " on " + uxBranch + ", gd-AbCdEfGhIj, in " + uxPath + "."
)

// uxLong is prefix run out to n bytes of plain words: an F1 title or body.
func uxLong(prefix string, n int) string {
	s := prefix + strings.Repeat("a long human title that keeps going ", n/36+1)
	return strings.TrimSpace(s[:n])
}

// uxNth is the i-th of many rows' text: the first is F1, the second F2, the third F4, the rest short and numbered.
func uxNth(i int, short string) string {
	switch i {
	case 0:
		return uxLong("Stress: ", 200)
	case 1:
		return uxURL
	case 2:
		return uxF4
	}
	return short + " " + strconv.Itoa(i+1)
}

type uxQueues struct{ QueueReader }

func (u uxQueues) Queue(ctx context.Context, kind string, all bool) (*QueueDoc, error) {
	switch uxFixture(ctx) {
	case "down":
		return nil, errUXDown
	case "empty":
		return &QueueDoc{All: all, Scoped: true, Rows: []QueueRow{}}, nil
	case "stress":
		return stressQueue(kind, all, goldenNow), nil
	}
	return u.QueueReader.Queue(ctx, kind, all)
}

// stressQueue is a tab of 36 rows (F3): the first with a 200-char title and a 1,200-char text, the second a URL for a title
// and the tokens in its text, the third F4; every decision asks with fourteen options, the first long and with a URL; the
// agent and task of every third row are ids the daemon does not have (F6).
func stressQueue(kind string, all bool, now time.Time) *QueueDoc {
	typ := map[string]string{QueueDecisions: "decision", QueueEscalations: "escalation"}[kind]
	d := &QueueDoc{All: all, Scoped: true, More: 164, InHand: "12 in hand by " + uxAgent}
	for i := 0; i < 36; i++ {
		t := typ
		if t == "" {
			t = []string{"task", "epic", "bug"}[i%3]
		}
		r := QueueRow{ID: "gd-UxQueue" + strconv.Itoa(100+i), Title: uxNth(i, "Queue row"), Kind: t, Type: t, Project: "gardenio", Status: "open",
			// the attention order (sortQueue) puts what needs the viewer first, oldest first: the F1, F2 and F4 rows lead
			Priority: i % 5, Since: now.Add(-time.Duration((36-i)*37) * time.Minute).Unix(), NeedsYou: i < 3 || i%2 == 0, Why: "asked of you", Actor: uxAgent}
		if i == 0 {
			r.Text = strings.Repeat("A long body sentence so the text runs well past what a row shows. ", 18)
		} else if i == 1 {
			r.Text = uxTokens
		}
		if i%3 == 0 {
			r.Agent, r.Task = &QueueRef{ID: "gd-MissingAg1"}, &QueueRef{ID: "gd-MissingTk1"}
		} else {
			r.Agent, r.Task = &QueueRef{ID: "gd-agentBeadW", Title: uxAgent}, &QueueRef{ID: "gd-EdgeIdP002", Title: uxLong("Task: ", 120)}
		}
		switch t {
		case "decision":
			for o := 0; o < 14; o++ {
				opt := QueueOption{ID: "o" + strconv.Itoa(o), Text: "Option " + strconv.Itoa(o+1)}
				if o == 0 {
					opt.Text, opt.Label = uxLong("Ship it: ", 120), "Merge "+uxURL+" now"
				}
				r.Options = append(r.Options, opt)
			}
		case "escalation":
			r.Operation, r.Audience = "gitlab_mr_merge_with_a_very_long_operation_type_name", "owner"
		default:
			r.Pinned, r.MR, r.MRURL = i == 0, "opened", uxURL
		}
		d.Rows = append(d.Rows, r)
	}
	return d
}

type uxSearch struct{ SearchReader }

func (u uxSearch) Search(ctx context.Context, q string) (*SearchDoc, error) {
	switch uxFixture(ctx) {
	case "down":
		return nil, errUXDown
	case "empty":
		return &SearchDoc{Mode: SearchSemantic}, nil
	case "stress":
		now := goldenNow
		d := &SearchDoc{Mode: SearchLexical, Note: "Semantic search is off on the beads daemon (" + uxURL + "), so these are word matches only."}
		for i := 0; i < 40; i++ {
			h := SearchHit{ID: "gd-UxHit" + strconv.Itoa(1000+i), Title: uxNth(i, "Search hit"), Type: []string{"task", "epic", "bug", "decision"}[i%4],
				Status: "open", Project: "gardenio", Priority: i % 5, Updated: now.Add(-time.Duration(i) * time.Hour).Unix(), Exact: i == 0, Plan: i == 3}
			if i < 3 {
				h.Text = strings.Repeat("A long body sentence so the hit's text runs well past what a row shows. ", 17) + uxTokens
			}
			if i == 4 {
				h.Project = uxAgent // F6: a project the farm does not draw
			}
			d.Hits = append(d.Hits, h)
		}
		return d, nil
	}
	return u.SearchReader.Search(ctx, q)
}

type uxBundles struct{ BundleReader }

func (u uxBundles) Bundle(ctx context.Context, id string) (*BundleDoc, error) {
	switch uxFixture(ctx) {
	case "down":
		return nil, errUXDown
	case "empty":
		return &BundleDoc{ID: id, Title: "A bundle with no documents yet", Kind: "plan_narrative", Status: "open", Docs: []BundlePart{}}, nil
	case "stress":
		return stressBundle(id), nil
	}
	return u.BundleReader.Bundle(ctx, id)
}

// stressBundle is a bundle of 30 documents (F3): F1 a 200-char title and description, F2 tokens in markdown, a wide csv and a
// code line with no break, F4 a document in emoji and Arabic, F6 a parent and an image the daemon does not have.
func stressBundle(id string) *BundleDoc {
	d := &BundleDoc{ID: id, Title: uxLong("Bundle: ", 200), Kind: "plan_narrative_with_a_long_kind_name", Status: "open",
		Description: uxLong("What the bundle says: ", 600) + " " + uxTokens,
		Parent:      &BundleRef{ID: "gd-MissingEp1"},
		MR:          &BundleMR{URL: uxURL, Number: "4242", State: "opened", Title: uxLong("feat(gardenhub): ", 150)},
		More:        12}
	d.Docs = append(d.Docs,
		BundlePart{ID: "gd-UxBndDoc01", Title: uxLong("Document: ", 200), Format: "md", Content: "# " + uxLong("A heading that keeps going: ", 140) + "\n\n" +
			strings.Repeat("A long paragraph sentence so the document runs well past the fold. ", 12) + "\n\n" + uxTokens + "\n\n- " + uxF4 + "\n- " + uxPath},
		BundlePart{ID: "gd-UxBndDoc02", Title: "Unbroken tokens", Format: "text", Content: uxURL + "\n" + uxSHA + "\n" + uxBranch + "\n" + uxPath},
		BundlePart{ID: "gd-UxBndDoc03", Title: "A wide table", Format: "csv", Content: "Surface,Phone,Desktop,URL,SHA\n" +
			"workbench,pass,pass," + uxURL + "," + uxSHA + "\n\"" + uxF4 + "\",fail,pass,,\n"},
		BundlePart{ID: "gd-UxBndDoc04", Title: "A long code line", Format: "code", Language: "go", Content: "func f() string { return \"" + uxURL + uxURL + "\" }\n"},
		BundlePart{ID: "gd-UxBndDoc05", Title: "🧑‍🌾 مريم 田中花子 café", Format: "md", Content: uxF4},
		BundlePart{ID: "gd-UxBndDoc06", Title: "A picture that is not there", Format: "image", URL: "/image/coop/sessions/images/missing-404.png"},
	)
	for i := len(d.Docs); i < 30; i++ {
		d.Docs = append(d.Docs, BundlePart{ID: "gd-UxBndDoc" + strconv.Itoa(10+i), Title: "Document " + strconv.Itoa(i+1), Format: "text", Content: "Short document " + strconv.Itoa(i+1) + "."})
	}
	return d
}

type uxPlans struct{ PlanReader }

func (u uxPlans) Plan(ctx context.Context, id string) (*PlanDoc, error) {
	switch uxFixture(ctx) {
	case "down":
		return nil, errUXDown
	case "empty":
		return &PlanDoc{ID: id, Title: "A plan with nothing poured yet", Project: "gardenio", Status: "open", State: "draft", Plan: true}, nil
	case "stress":
		return stressPlan(id, goldenNow), nil
	}
	return u.PlanReader.Plan(ctx, id)
}

func (u uxPlans) Pour(ctx context.Context, id string) (*PourDoc, error) {
	if uxFixture(ctx) == "down" {
		return nil, errUXDown
	}
	return u.PlanReader.Pour(ctx, id)
}

// stressPlan is a cockpit under F1-F6: a 200-char plan title; twelve phases, the first with 34 steps (F3) whose first three
// are F1, F2 and F4 and whose blockers are beads the daemon does not have (F6); a decision with fourteen options and a long
// prompt; four escalations; a crew of 32 with 90-char names; a narrative of tokens.
func stressPlan(id string, now time.Time) *PlanDoc {
	ago := func(d time.Duration) int64 { return now.Add(-d).Unix() }
	d := &PlanDoc{ID: id, Title: uxLong("Plan: ", 200), Project: "gardenio", Status: "in_progress", State: "building", Plan: true,
		Total: 120, Closed: 37, Pct: 31, LastChange: ago(3 * time.Minute), More: true}
	for p := 0; p < 12; p++ {
		ph := PlanPhase{ID: "gd-UxPh" + strconv.Itoa(10+p), Number: p + 1, Title: uxNth(p, "Phase"), Status: "in_progress", Total: 10, Done: p % 10,
			Formula: "release-train-with-a-long-formula-name"}
		n := 2
		if p == 0 {
			n = 34
		}
		for s := 0; s < n; s++ {
			st := PlanStep{ID: "gd-UxSt" + strconv.Itoa(1000+p*100+s), Number: p*100 + s + 1, Title: uxNth(s, "Step"), Type: []string{"task", "bug", "feature"}[s%3],
				Status: []string{"in_progress", "open", "closed"}[s%3], Closed: s%3 == 2, Priority: s % 5, Assignee: uxAgent, Updated: ago(time.Duration(s) * time.Hour)}
			if s%4 == 0 {
				st.MR = &PlanMR{State: "opened", URL: uxURL}
			}
			if s%5 == 1 {
				st.BlockedBy = []PlanBlocker{{ID: "gd-MissingBk1"}, {ID: "gd-UxEsc1", Title: uxLong("Merge ", 120), Escalation: true, Operation: "gitlab_mr_merge"}}
			}
			ph.Steps = append(ph.Steps, st)
		}
		d.Phases = append(d.Phases, ph)
	}
	d.Steps = []PlanStep{{ID: "gd-UxLoose1", Number: 999, Title: uxURL, Type: "chore", Status: "open", Priority: 2}}
	dec := PlanDecision{ID: "gd-UxDec1", Title: uxLong("Decide: ", 200), Prompt: strings.Repeat("A long prompt sentence that asks a careful question. ", 20) + uxTokens,
		Status: "open", RequestedBy: uxAgent, Since: ago(time.Hour), Mine: true}
	for o := 0; o < 14; o++ {
		dec.Options = append(dec.Options, QueueOption{ID: "o" + strconv.Itoa(o), Text: uxNth(o, "Option")})
	}
	d.Decisions = []PlanDecision{dec, {ID: "gd-UxDec2", Title: uxF4, Status: "closed", Resolved: true, Chosen: "o1", RequestedBy: "gd-MissingAg1",
		Options: []QueueOption{{ID: "o1", Text: "مريم"}, {ID: "o2", Text: "田中花子"}}}}
	for e := 0; e < 4; e++ {
		d.Escalations = append(d.Escalations, PlanEscalation{ID: "gd-UxEsc" + strconv.Itoa(e+1), Title: uxNth(e, "Escalation"),
			Operation: "gitlab_mr_merge_with_a_very_long_operation_type_name", Audience: "owner", Since: ago(time.Duration(e+1) * time.Hour)})
	}
	crew := &PlanCrew{Total: 32, Working: 20, Idle: 4, Done: 6, Failed: 2}
	for c := 0; c < 32; c++ {
		m := PlanCrewMember{ID: "gd-UxCrew" + strconv.Itoa(10+c), Name: uxAgent[:76] + "-" + strconv.Itoa(c), Role: "crew", State: "working", Bucket: "working", Live: true,
			Step: &QueueRef{ID: "gd-UxSt1000", Title: uxLong("Step: ", 200)}, StepN: 1, Since: ago(time.Duration(c) * time.Minute)}
		if c%8 == 7 {
			m.Bucket, m.State, m.Live, m.Outcome, m.Step = "failed", "failed", false, "fail", &QueueRef{ID: "gd-MissingSt1"}
		}
		crew.Members = append(crew.Members, m)
	}
	d.Crew = crew
	d.Narrative = PlanNarrative{Summary: uxLong("The plan in words: ", 900) + "\n\n" + uxTokens + "\n\n" + uxF4, SummaryClipped: true,
		Bundle: "gd-MissingBn1", BundleTitle: uxLong("Bundle: ", 160),
		Docs: []PlanNarrativeDoc{{ID: "gd-UxNarr1", Headline: uxLong("Design: ", 160), Format: "markdown", Content: "## " + uxURL + "\n\n- " + uxSHA + "\n- " + uxPath, Clipped: true}}}
	return d
}

type uxProjects struct{ ProjectReader }

func (u uxProjects) Project(ctx context.Context, name string) (*ProjectDoc, error) {
	switch uxFixture(ctx) {
	case "down":
		return nil, errUXDown
	case "empty":
		return &ProjectDoc{Name: name, Agents: []ProjectAgent{}, Gardeners: []ProjectGardener{}, Accounts: []ProjectAccount{}, Growing: []ProjectBead{},
			Recent: []ProjectBead{}, Decisions: []QueueRow{}, Escalations: []QueueRow{}, Blocked: []ProjectBead{}}, nil
	case "stress":
		return stressProject(name), nil
	}
	return u.ProjectReader.Project(ctx, name)
}

// stressProject is a project with 40 of every list (F3): 90-char agent names and 200-char task titles (F1), a URL for a goal
// and a git URL (F2), F4 names, tasks the daemon does not have (F6), and the stress Queues' decisions and escalations.
func stressProject(name string) *ProjectDoc {
	f2 := func(v float64) *float64 { return &v }
	now := goldenNow
	d := &ProjectDoc{Name: name, CanEdit: true, More: map[string]int{"agents": 85, "recent": 240},
		Config: &ProjectConfig{BeadID: "gd-ProjGarden1", Status: "open", GitURL: uxURL, DefaultBranch: uxBranch, Image: "123456789012.dkr.ecr.us-east-1.amazonaws.com/gardenio/agent:" + uxSHA,
			Account: uxAgent, Model: "claude-opus", CPURequest: "500m", MemoryLimit: "2Gi", BudgetUSD: "123456", BudgetPeriod: "month",
			Secrets:  []ProjectSecret{{Env: "A_VERY_LONG_ENVIRONMENT_VARIABLE_NAME_FOR_A_TOKEN", Secret: "gardenio-" + uxSHA, Key: "token"}},
			Findings: []ProjectFinding{{Field: "memory_request", Severity: "warning", Message: uxLong("A finding: ", 200)}, {Severity: "error", Message: uxTokens}}},
		Spend: &ProjectSpend{TodayUSD: 1234.56, WeekUSD: 98765.43, TotalUSD: 1234567.89, Has: true},
		Plot: &FarmCostProject{Project: name, Has: true, HasBudget: true, BudgetUSD: f2(123456), Period: BudgetMonth, TodayUSD: f2(1234.56), WeekUSD: f2(98765.43),
			SpentUSD: f2(120000), RemainingUSD: f2(3456)}}
	for i := 0; i < 40; i++ {
		n := strconv.Itoa(i + 1)
		a := ProjectAgent{ID: "gd-UxAgent" + n, Name: uxAgent[:76] + "-" + n, Role: "crew", State: []string{"working", "idle", "failed", "done"}[i%4],
			Status: "in_progress", TaskID: "gd-UxTask" + n, TaskTitle: uxNth(i, "Task"), Ago: n + "m"}
		if i > 2 && i%3 == 2 { // past the F1, F2 and F4 rows
			a.TaskID, a.TaskTitle = "gd-MissingTk1", ""
		}
		d.Agents = append(d.Agents, a)
		d.Gardeners = append(d.Gardeners, ProjectGardener{ID: "gd-UxGard" + n, Name: uxNth(i, "gardener"), Kind: "trigger", Status: "active", Enabled: i%2 == 0,
			Goal: uxNth((i+1)%40, "Keep going"), Runs: i * 37})
		d.Accounts = append(d.Accounts, ProjectAccount{Name: uxNth(i, "account"), Provider: "anthropic", Model: "claude-opus", Status: "active",
			TodayUSD: f2(float64(i) * 12.5), WeekUSD: f2(float64(i) * 87.25), TotalUSD: f2(float64(i) * 4321.5)})
		b := ProjectBead{ID: "gd-UxBead" + n, Title: uxNth(i, "Bead"), Type: "task", Status: "in_progress", Ago: n + "h"}
		d.Growing, d.Recent = append(d.Growing, b), append(d.Recent, b)
		b.Status = "blocked"
		d.Blocked = append(d.Blocked, b)
	}
	d.Decisions = stressQueue(QueueDecisions, true, now).Rows
	d.Escalations = stressQueue(QueueEscalations, true, now).Rows
	return d
}

type uxGardeners struct{ GardenerReader }

func (u uxGardeners) Gardener(ctx context.Context, id string) (*GardenerDoc, error) {
	switch uxFixture(ctx) {
	case "down":
		return nil, errUXDown
	case "empty":
		return &GardenerDoc{ID: id, Title: "A gardener that has not run yet", Name: "new gardener", Project: "gardenio", Status: "open", Enabled: true, Mode: "timed",
			Successors: []string{}, Runs: []GardenerRun{}, Beads: []GardenerLane{}, Config: &GardenerConfig{}}, nil
	case "stress":
		return stressGardener(id), nil
	}
	return u.GardenerReader.Gardener(ctx, id)
}

// stressGardener is a gardener loop under F1-F6: a 200-char name and goal, a 1,200-char prompt full of tokens, 25 runs with
// 90-char names and long headlines (F3), four lanes of 30 beads, twelve successors, a run whose task is not in the daemon.
func stressGardener(id string) *GardenerDoc {
	prompt := strings.Repeat("Walk every crew pull request to green and merge what is ready. ", 16) + uxTokens
	d := &GardenerDoc{ID: id, Title: "Gardener: " + uxLong("", 190), Name: uxLong("Gardener: ", 200), Project: "gardenio", Status: "open", Enabled: true, Mode: "timed",
		Goal: uxLong("Goal: ", 200) + " " + uxURL, Prompt: prompt, Chasing: uxF4,
		Wakes: []string{"Every 20 minutes: */20 * * * * (UTC)", uxLong("When ", 160)}, Stops: []string{"ends the loop: after 4000 runs", uxTokens},
		OnLoopEnd: uxLong("On the loop's end: ", 140), Cron: "*/20 * * * *", Timezone: "Pacific/Chatham", NextFire: "in 7m",
		RunsSpawned: 12345, RunsCompleted: 12001, MaxRuns: 40000, RunBudget: 40000, Failures: 7, RunCount: 25, LiveCount: 3, DidNotRun: 4,
		EngineCount: "12345 spawned, 12001 finished (lifetime tally; older runs pruned)", Spend: "$123,456.78", SpendStale: true, Overdue: "overdue by 3h", Silent: "silent for 2d",
		Owner: "مريم 田中花子", InFlight: true, LastRun: "gd-UxRun100", More: map[string]int{"runs": 12320},
		Config: &GardenerConfig{Prompt: prompt, TerminalGoal: uxLong("Terminal goal: ", 200), RunBudget: "40000", MaxRuns: "40000", RunTimeout: "30m", Cron: "*/20 * * * *",
			StopJSON: "{\n  \"max_runs\": 40000,\n  \"url\": \"" + uxURL + "\"\n}"}}
	for i := 0; i < 12; i++ {
		d.Successors = append(d.Successors, "phase=review · goal="+uxNth(i, "Successor"))
	}
	for i := 0; i < 25; i++ {
		n := strconv.Itoa(100 + i)
		r := GardenerRun{ID: "gd-UxRun" + n, Name: uxAgent[:76] + "-" + n, Label: "run " + n, Headline: uxNth(i, "Merged PR"), Cost: "$" + n + ".42", Turns: n + " turns",
			Ago: strconv.Itoa(i) + "h", TaskID: "gd-UxTask" + n, TaskTitle: uxNth((i+1)%25, "Task")}
		switch {
		case i < 3:
			r.State, r.Live = "working", true
		case i%5 == 0:
			r.Outcome, r.TaskID, r.TaskTitle = "fail", "gd-MissingTk1", ""
		case i%7 == 0:
			r.DidNotRun = true
		default:
			r.Outcome = "ok"
		}
		d.Runs = append(d.Runs, r)
	}
	for _, l := range []struct{ lane, label, status string }{{"in_progress", "In progress", "in_progress"}, {"blocked", "Blocked", "blocked"}, {"open", "Open", "open"}, {"done", "Done", "closed"}} {
		ln := GardenerLane{Lane: l.lane, Label: l.label, Count: 130}
		for i := 0; i < 30; i++ {
			ln.Beads = append(ln.Beads, ProjectBead{ID: "gd-UxLn" + l.lane[:2] + strconv.Itoa(i), Title: uxNth(i, l.label), Type: "task", Status: l.status})
		}
		d.Beads = append(d.Beads, ln)
	}
	return d
}

type uxSettings struct{ SettingsReader }

func (u uxSettings) Settings(ctx context.Context) (*SettingsDoc, error) {
	switch uxFixture(ctx) {
	case "down":
		return nil, errUXDown
	case "empty": // a viewer with no user bead yet
		return &SettingsDoc{Email: "new@example.com", Projects: []string{}, Roles: []string{"crew"}}, nil
	case "stress":
		d := &SettingsDoc{Email: "a.very.long.operator.address." + strings.Repeat("x", 60) + "@example-with-a-long-domain.com", HasBead: true, BeadID: "gd-UserOp0001",
			Spawn:    SettingsSpawn{Project: "gardenio", Role: "crew", Prompt: strings.Repeat("Work {{id}}: {{title}}. ", 30) + uxTokens, PerSelection: "one-per-bead"},
			Identity: SettingsIdentity{DisplayName: "مريم 田中花子 🧑‍🌾 " + uxLong("", 80), Pronouns: "they/them", Team: uxAgent, SlackChannelID: "C0C3RH33A81C0C3RH33A81C0C3RH33A81"},
			Roles:    []string{"crew", "gardener", "thread", "infra", "plan-gardener", "release-captain", "a-role-with-a-very-long-name-for-a-select"}}
		d.Projects = []string{"gardenio", uxAgent}
		for i := 0; i < 40; i++ {
			d.Projects = append(d.Projects, "project-"+strconv.Itoa(i+1))
		}
		return d, nil
	}
	return u.SettingsReader.Settings(ctx)
}

// uxBeads is the bead view's reader with X-UX-Fixture: down answering the bead but not its comments or links, the soft
// degradation (WB-15) the panel names in Partial. It embeds the card world's reader, so every other reader it is asked for
// as (beadLister, the dormancy reader) still answers.
type uxBeads struct{ *adoptFake }

func (u uxBeads) GetComments(ctx context.Context, id string) ([]beadsapi.Comment, error) {
	if uxFixture(ctx) == "down" {
		return nil, errUXDown
	}
	return u.adoptFake.GetComments(ctx, id)
}

func (u uxBeads) GetDependencies(ctx context.Context, id string) ([]beadsapi.Dependency, error) {
	if uxFixture(ctx) == "down" {
		return nil, errUXDown
	}
	return u.adoptFake.GetDependencies(ctx, id)
}

func (u uxBeads) GetReverseDependencies(ctx context.Context, id string) ([]beadsapi.Dependency, error) {
	if uxFixture(ctx) == "down" {
		return nil, errUXDown
	}
	return u.adoptFake.GetReverseDependencies(ctx, id)
}
