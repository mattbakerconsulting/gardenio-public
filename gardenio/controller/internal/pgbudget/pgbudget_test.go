package pgbudget

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation"
)

// fakeVM answers each (expr, time) with fixture series; anything else is an
// empty vector.
type fakeVM map[string][]Sample

func (f fakeVM) Query(_ context.Context, expr string, at time.Time) ([]Sample, error) {
	return f[fmt.Sprintf("%s@%d", expr, at.Unix())], nil
}

func (f fakeVM) put(expr string, at time.Time, s ...Sample) {
	f[fmt.Sprintf("%s@%d", expr, at.Unix())] = s
}

func stmt(db, id, query string, v float64) Sample {
	return Sample{Labels: map[string]string{"datname": db, "queryid": id, "query": query}, Value: v}
}

func table(db, rel string, v float64) Sample {
	return Sample{Labels: map[string]string{"datname": db, "schemaname": "public", "relname": rel}, Value: v}
}

var asOf = time.Date(2026, 11, 4, 8, 0, 0, 0, time.UTC)

// fixture is a day of exporter series. The LiteLLM tag scan is over budget
// both days (5.6 s mean, 3,000 calls); ListBeads is over its share only
// today; the pg_dump COPY is the biggest statement but exempt. events grows
// 600 MB a day both days; history only today.
func fixture() fakeVM {
	vm := fakeVM{}
	vm.put(qStatementExec, asOf,
		stmt("beads", "11", "COPY public.events (id) TO stdout", 30000),
		stmt("litellm", "22", "SELECT jsonb_array_elements_text(request_tags) AS individual_request_tag", 16800),
		stmt("beads", "33", "SELECT $6 AS total_count, beads.id", 15000),
		stmt("beads", "44", "SELECT COUNT(*) FROM beads", 6000),
		stmt("keycloak", "55", "SELECT 1", 2000),
		stmt("beads", "66", "SELECT label FROM labels", 1200),
	)
	vm.put(qStatementCalls, asOf,
		stmt("beads", "11", "COPY public.events (id) TO stdout", 12),
		stmt("litellm", "22", "SELECT jsonb_array_elements_text(request_tags) AS individual_request_tag", 3000),
		stmt("beads", "33", "SELECT $6 AS total_count, beads.id", 200000),
		stmt("beads", "44", "SELECT COUNT(*) FROM beads", 90000),
		stmt("keycloak", "55", "SELECT 1", 50000),
		stmt("beads", "66", "SELECT label FROM labels", 40000),
	)
	vm.put(qStatementExec, asOf.Add(-day),
		stmt("beads", "11", "COPY public.events (id) TO stdout", 28000),
		stmt("litellm", "22", "SELECT jsonb_array_elements_text(request_tags) AS individual_request_tag", 15000),
		stmt("beads", "33", "SELECT $6 AS total_count, beads.id", 9000),
		stmt("beads", "44", "SELECT COUNT(*) FROM beads", 7000),
		stmt("keycloak", "55", "SELECT 1", 2000),
	)
	vm.put(qStatementCalls, asOf.Add(-day),
		stmt("beads", "11", "COPY public.events (id) TO stdout", 12),
		stmt("litellm", "22", "SELECT jsonb_array_elements_text(request_tags) AS individual_request_tag", 3000),
		stmt("beads", "33", "SELECT $6 AS total_count, beads.id", 150000),
		stmt("beads", "44", "SELECT COUNT(*) FROM beads", 95000),
		stmt("keycloak", "55", "SELECT 1", 50000),
	)
	vm.put(qCallers, asOf,
		Sample{Labels: map[string]string{"datname": "litellm", "queryid": "22", "application_name": "gardenio-controller"}, Value: 4},
		Sample{Labels: map[string]string{"datname": "litellm", "queryid": "22", "application_name": "litellm"}, Value: 1},
	)
	vm.put(qTableBytes, asOf, table("beads", "events", 4.2e9), table("beads", "history", 300e6), table("beads", "labels", 1e9), table("litellm", "spend", 2e9))
	vm.put(qTableBytes, asOf.Add(-day), table("beads", "events", 3.6e9), table("beads", "history", 200e6), table("beads", "labels", 0.98e9))
	vm.put(qTableBytes, asOf.Add(-2*day), table("beads", "events", 3.0e9), table("beads", "history", 195e6), table("beads", "labels", 0.97e9))
	vm.put(qFired, asOf,
		Sample{Labels: map[string]string{"alertname": "PostgresSlowFrequentQuery", "budget": "slow-query", "datname": "litellm", "queryid": "22"}, Value: 1},
		Sample{Labels: map[string]string{"alertname": "PostgresTableGrowth", "budget": "table-growth", "datname": "beads", "schemaname": "public", "relname": "events"}, Value: 1},
	)
	return vm
}

func TestGather_RanksAndJudgesEachDay(t *testing.T) {
	r, err := Gather(context.Background(), fixture(), DefaultBudgets(), asOf)
	if err != nil {
		t.Fatal(err)
	}
	if r.TotalSeconds != 71000 || r.PrevTotalSeconds != 61000 {
		t.Errorf("totals = %v / %v, want 71000 / 61000", r.TotalSeconds, r.PrevTotalSeconds)
	}
	var ids []string
	for _, s := range r.TopStatements(TopN) {
		ids = append(ids, s.QueryID)
	}
	if want := []string{"11", "22", "33", "44", "55"}; !slices.Equal(ids, want) {
		t.Errorf("top 5 = %v, want %v", ids, want)
	}
	byID := map[string]Statement{}
	for _, s := range r.Statements {
		byID[s.QueryID] = s
	}
	if s := byID["11"]; len(s.Today.Over) != 0 {
		t.Errorf("the pg_dump COPY is exempt from the budgets, got over %v", s.Today.Over)
	}
	if s := byID["22"]; len(s.Today.Over) != 2 || len(s.Yesterday.Over) != 2 || !slices.Equal(s.Callers, []string{"gardenio-controller", "litellm"}) {
		t.Errorf("tag scan: over today %v, yesterday %v, callers %v; want share and slow both days, both callers", s.Today.Over, s.Yesterday.Over, s.Callers)
	}
	if s := byID["33"]; len(s.Today.Over) != 1 || len(s.Yesterday.Over) != 0 {
		t.Errorf("ListBeads: 21%% today, 15%% yesterday; got over %v / %v", s.Today.Over, s.Yesterday.Over)
	}
	if s := byID["66"]; s.Seen {
		t.Errorf("a statement absent a day back must not be judged for that day")
	}

	var growth []string
	for _, tb := range r.TopGrowth(TopN) {
		growth = append(growth, tb.Rel)
	}
	if want := []string{"events", "history", "labels"}; !slices.Equal(growth, want) {
		t.Errorf("growth order = %v, want %v (spend has no size a day back)", growth, want)
	}

	var keys []string
	for _, b := range r.Breaches() {
		keys = append(keys, b.Key)
	}
	if want := []string{"query:litellm:22", "table:beads.public.events"}; !slices.Equal(keys, want) {
		t.Errorf("breaches two days running = %v, want %v", keys, want)
	}
	if len(r.Fired) != 2 || r.Fired[0].String() != "PostgresSlowFrequentQuery datname=litellm queryid=22" {
		t.Errorf("fired = %v", r.Fired)
	}
}

// fakeStore is an in-memory daemon for the filer and the runner.
type fakeStore struct {
	mu    sync.Mutex
	beads []*beadsapi.BeadDetail
	reqs  []beadsapi.CreateBeadRequest
	now   time.Time
}

func (f *fakeStore) CreateBead(_ context.Context, req beadsapi.CreateBeadRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if req.IdempotencyKey != "" {
		for i, r := range f.reqs {
			if r.IdempotencyKey == req.IdempotencyKey {
				return f.beads[i].ID, nil
			}
		}
	}
	var fields map[string]string
	_ = json.Unmarshal(req.Fields, &fields)
	b := &beadsapi.BeadDetail{ID: fmt.Sprintf("gd-new%04d", len(f.beads)), Type: req.Type, Title: req.Title, Status: "open", Labels: req.Labels, Fields: fields, CreatedAt: f.now}
	f.beads, f.reqs = append(f.beads, b), append(f.reqs, req)
	return b.ID, nil
}

func (f *fakeStore) put(b *beadsapi.BeadDetail) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.beads, f.reqs = append(f.beads, b), append(f.reqs, beadsapi.CreateBeadRequest{})
}

func (f *fakeStore) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*beadsapi.BeadDetail
	for _, b := range f.beads {
		if len(q.Types) > 0 && !slices.Contains(q.Types, b.Type) || len(q.Statuses) > 0 && !slices.Contains(q.Statuses, b.Status) {
			continue
		}
		ok := true
		for _, l := range q.Labels {
			ok = ok && slices.Contains(b.Labels, l)
		}
		for k, v := range q.Fields {
			ok = ok && b.Fields[k] == v
		}
		if !q.CreatedAfter.IsZero() && b.CreatedAt.Before(q.CreatedAfter) {
			ok = false
		}
		if !q.ClosedAfter.IsZero() {
			at, err := time.Parse(time.RFC3339, b.ClosedAt)
			ok = ok && err == nil && at.After(q.ClosedAfter)
		}
		if ok {
			out = append(out, b)
		}
	}
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return &beadsapi.ListBeadsResult{Beads: out, Total: len(out)}, nil
}

func (f *fakeStore) GetBead(_ context.Context, id string) (*beadsapi.BeadDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, b := range f.beads {
		if b.ID == id {
			return b, nil
		}
	}
	return nil, fmt.Errorf("no bead %s", id)
}

func (f *fakeStore) UpdateBeadFields(_ context.Context, id string, fields map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, b := range f.beads {
		if b.ID == id {
			for k, v := range fields {
				b.Fields[k] = v
			}
		}
	}
	return nil
}

func (f *fakeStore) tasks() []*beadsapi.BeadDetail {
	var out []*beadsapi.BeadDetail
	for _, b := range f.beads {
		if b.Type == "task" {
			out = append(out, b)
		}
	}
	return out
}

// TestFile_SeededBreachFilesExactlyOne is the acceptance case: the tag scan
// over budget two days running gives one task, with its numbers, the query
// text and its callers, under the epic; the table breach gives the other;
// ListBeads, over budget only today, gives none. Running again files nothing.
func TestFile_SeededBreachFilesExactlyOne(t *testing.T) {
	r, err := Gather(context.Background(), fixture(), DefaultBudgets(), asOf)
	if err != nil {
		t.Fatal(err)
	}
	st := &fakeStore{now: asOf}
	f := &Filer{Store: st, Land: "gardenio", Epic: "gd-PkZRCb0BuB", Actor: "gardenio-controller", Budgets: DefaultBudgets()}
	filed, capped, err := f.File(context.Background(), r, asOf)
	if err != nil {
		t.Fatal(err)
	}
	if len(filed) != 2 || capped != 0 {
		t.Fatalf("filed %v, capped %d; want the tag scan and events, nothing capped", filed, capped)
	}
	var q beadsapi.CreateBeadRequest
	for _, req := range st.reqs {
		var fields map[string]string
		_ = json.Unmarshal(req.Fields, &fields)
		if fields[FieldKey] == "query:litellm:22" {
			q = req
		}
	}
	for _, want := range []string{
		"`litellm`", "`22`", "SELECT jsonb_array_elements_text(request_tags)",
		"gardenio-controller, litellm", "| exec time | 16,800 s (24% of all) | 15,000 s (25% of all) |",
		"| calls | 3,000 | 3,000 |", "| mean | 5.6 s | 5.0 s |",
	} {
		if !strings.Contains(q.Description, want) {
			t.Errorf("task description missing %q:\n%s", want, q.Description)
		}
	}
	if q.ParentBeadID != "gd-PkZRCb0BuB" || q.Type != "task" || !slices.Contains(q.Labels, "project:gardenio") || !slices.Contains(q.Labels, TaskLabel) {
		t.Errorf("task must be a gardenio pg-budget task under the epic, got %+v", q)
	}
	if n := len([]rune(q.ShortTitle)); n == 0 || n > 40 {
		t.Errorf("short title %q must be 1-40 runes", q.ShortTitle)
	}

	// Same day, and the next: the open tasks hold.
	for _, at := range []time.Time{asOf.Add(time.Hour), asOf.Add(day)} {
		again, _, err := f.File(context.Background(), r, at)
		if err != nil || len(again) != 0 {
			t.Errorf("at %s: refiled %v (err %v); an open task must dedupe", at, again, err)
		}
	}
	if n := len(st.tasks()); n != 2 {
		t.Errorf("%d tasks in the store, want 2", n)
	}
}

func TestFile_CapsAtThreePerDay(t *testing.T) {
	r := &Report{AsOf: asOf}
	for i := range 5 {
		over := Day{ExecSeconds: float64(1000 - i), Over: []string{"x"}}
		r.Statements = append(r.Statements, Statement{Datname: "beads", QueryID: fmt.Sprint(i), Query: "SELECT", Today: over, Yesterday: over, Seen: true})
	}
	st := &fakeStore{now: asOf}
	// One filed yesterday evening counts against today's cap.
	st.put(&beadsapi.BeadDetail{ID: "gd-old", Type: "task", Status: "open", Labels: []string{TaskLabel}, Fields: map[string]string{FieldKey: "query:beads:9"}, CreatedAt: asOf.Add(-10 * time.Hour)})
	f := &Filer{Store: st, Land: "gardenio", Budgets: DefaultBudgets()}
	filed, capped, err := f.File(context.Background(), r, asOf)
	if err != nil {
		t.Fatal(err)
	}
	if len(filed) != 2 || capped != 3 || filed[0].Key != "query:beads:0" || filed[1].Key != "query:beads:1" {
		t.Errorf("filed %v, capped %d; want the two heaviest, three capped", filed, capped)
	}
}

func TestFile_RefilesOnlyAfterAClosedTaskAges(t *testing.T) {
	over := Day{ExecSeconds: 100, Over: []string{"x"}}
	r := &Report{AsOf: asOf, Statements: []Statement{{Datname: "beads", QueryID: "7", Today: over, Yesterday: over, Seen: true}}}
	for _, c := range []struct {
		closedAgo time.Duration
		want      int
	}{{2 * day, 0}, {8 * day, 1}} {
		st := &fakeStore{now: asOf}
		st.put(&beadsapi.BeadDetail{ID: "gd-fixed", Type: "task", Status: "closed", ClosedAt: asOf.Add(-c.closedAgo).Format(time.RFC3339),
			Labels: []string{TaskLabel}, Fields: map[string]string{FieldKey: "query:beads:7"}, CreatedAt: asOf.Add(-20 * day)})
		filed, _, err := (&Filer{Store: st, Land: "gardenio"}).File(context.Background(), r, asOf)
		if err != nil || len(filed) != c.want {
			t.Errorf("closed %s ago: filed %v (err %v), want %d", c.closedAgo, filed, err, c.want)
		}
	}
}

// TestRunOnce_PostsTheReportOnceADay: the post goes to the land's #machines
// with the top-5 list and the filed tasks, and a second run that day (a
// restarted controller) posts nothing.
func TestRunOnce_PostsTheReportOnceADay(t *testing.T) {
	st := &fakeStore{now: asOf}
	st.put(&beadsapi.BeadDetail{ID: "gd-machines1", Type: "channel", Status: "open", Labels: []string{"project:gardenio"},
		Fields: map[string]string{"name": "machines", conversation.ChannelKind: conversation.ChannelKindMachines}})
	r := &Runner{Query: fixture(), Store: st, Budgets: DefaultBudgets(), Land: "gardenio", Epic: "gd-PkZRCb0BuB", Hour: 8, Actor: "gardenio-controller"}
	run := asOf.Add(37 * time.Minute)
	for range 2 {
		if err := r.RunOnce(context.Background(), run); err != nil {
			t.Fatal(err)
		}
	}
	var posts []*beadsapi.BeadDetail
	for _, b := range st.beads {
		if b.Type == "message" {
			posts = append(posts, b)
		}
	}
	if len(posts) != 1 {
		t.Fatalf("%d posts, want 1", len(posts))
	}
	p := posts[0]
	if p.Fields[conversation.BeadChannelID] != "gd-machines1" || p.Fields[FieldReportDay] != "2026-11-04" || p.Fields[conversation.BeadAuthor] != "gardenio-controller" {
		t.Errorf("post fields = %v", p.Fields)
	}
	text := p.Fields[conversation.BeadText]
	for _, want := range []string{
		"Postgres efficiency, the 24h to 08:00Z 4 Nov",
		"Statement time 71,000 s, +16% on the day before.",
		"1. beads 30,000 s (42%, +7%) · 12 calls · 2,500 s mean",
		"2. litellm 16,800 s (24%, +12%) · 3,000 calls · 5.6 s mean · gardenio-controller, litellm · over budget:",
		"5. keycloak 2,000 s (3%, flat)",
		"1. beads.public.events 3.9 GiB, +572 MiB (day before +572 MiB) · over budget: grew over 500 MiB",
		"- PostgresTableGrowth datname=beads relname=events schemaname=public",
		"- gd-new0001 filed for queryid 22 (litellm)",
		"- gd-new0002 filed for beads.public.events",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("report missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "6. ") {
		t.Errorf("report lists more than 5 statements:\n%s", text)
	}
}

func TestTick_WaitsForTheHour(t *testing.T) {
	st := &fakeStore{now: asOf}
	st.put(&beadsapi.BeadDetail{ID: "gd-machines1", Type: "channel", Status: "open", Labels: []string{"project:gardenio"},
		Fields: map[string]string{conversation.ChannelKind: conversation.ChannelKindMachines}})
	clock := asOf.Add(-time.Minute)
	r := &Runner{Query: fixture(), Store: st, Land: "gardenio", Hour: 8, Actor: "x", now: func() time.Time { return clock }}
	r.tick(context.Background())
	if len(st.beads) != 1 {
		t.Fatalf("ran before the hour: %d beads", len(st.beads))
	}
	clock = asOf.Add(time.Minute)
	r.tick(context.Background())
	if r.done != "2026-11-04" {
		t.Errorf("done = %q after the hour", r.done)
	}
}

func TestParseBudgets(t *testing.T) {
	b, err := ParseBudgets(`{"shareRatio":0.3,"growthBytes":1073741824,"severity":"warn","excludeQueryRegex":"COPY .*|VACUUM.*"}`)
	if err != nil {
		t.Fatal(err)
	}
	if b.ShareRatio != 0.3 || b.GrowthBytes != 1<<30 || b.SlowMeanSeconds != 1 {
		t.Errorf("budgets = %+v", b)
	}
	if re := b.excludeRe(); !re.MatchString("VACUUM x") || re.MatchString("SELECT 1 /* COPY */") {
		t.Errorf("exclude must be an anchored match like PromQL's !~")
	}
	if _, err := ParseBudgets(`{"excludeQueryRegex":"("}`); err == nil {
		t.Error("a bad regex must be refused")
	}
}

func TestParseVector_DropsNonFinite(t *testing.T) {
	got, err := parseVector([]byte(`{"status":"success","data":{"resultType":"vector","result":[
		{"metric":{"datname":"beads"},"value":[1,"2.5"]},{"metric":{"datname":"x"},"value":[1,"NaN"]}]}}`))
	if err != nil || len(got) != 1 || got[0].Value != 2.5 || got[0].Labels["datname"] != "beads" {
		t.Errorf("parseVector = %v, %v", got, err)
	}
}
