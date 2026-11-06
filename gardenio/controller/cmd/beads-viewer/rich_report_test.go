package main

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

func TestBuildRichReportView_FiltersAndOrdersChildren(t *testing.T) {
	parentChild := "parent-child"
	related := "related"

	parent := &beadsapi.BeadDetail{
		ID:    "gd-parent00001",
		Type:  "rich-report",
		Title: "Active Incident: study-web 502s",
		Fields: map[string]string{
			"report_kind": "active-incident",
			"headline":    "study-web 502s in america-prod",
			"severity":    "P1",
			"source_ref":  "alert:study-web-502",
		},
	}

	mentionedBy := []beadsapi.BeadReferenceSummary{
		// widget-instance child, position 2 — second.
		{ID: "gd-widget00002", Type: "widget-instance", RelationshipType: &parentChild},
		// widget-instance child, position 1 — first.
		{ID: "gd-widget00001", Type: "widget-instance", RelationshipType: &parentChild},
		// widget-instance, but only `related` — must be ignored.
		{ID: "gd-widget00003", Type: "widget-instance", RelationshipType: &related},
		// task referencing the rich-report — must be ignored.
		{ID: "gd-szke4nl8sp", Type: "task", RelationshipType: &parentChild},
		// untyped mention — must be ignored.
		{ID: "gd-0ubhwq6s17", Type: "widget-instance"},
	}

	fake := &fakeDaemon{beads: map[string]*beadsapi.BeadDetail{
		"gd-widget00001": {
			ID:   "gd-widget00001",
			Type: "widget-instance",
			Fields: map[string]string{
				"widget_type": "alert-summary",
				"position":    "1",
				"headline":    "Alert: 502 spike",
				"data":        `{"count":42,"severity":"P1"}`,
			},
		},
		"gd-widget00002": {
			ID:   "gd-widget00002",
			Type: "widget-instance",
			Fields: map[string]string{
				"widget_type": "rca-evidence",
				"position":    "2",
				"data":        `{"hypothesis":"DB pool exhaustion"}`,
			},
		},
	}}

	v := buildRichReportView(context.Background(), parent, nil, mentionedBy, fake, nil)
	if v == nil {
		t.Fatal("buildRichReportView returned nil")
	}
	if v.Headline != "study-web 502s in america-prod" {
		t.Errorf("Headline=%q, want incident headline", v.Headline)
	}
	if v.ReportKind != "active-incident" || v.Severity != "P1" {
		t.Errorf("metadata not propagated: kind=%q severity=%q", v.ReportKind, v.Severity)
	}
	if got, want := len(v.Widgets), 2; got != want {
		t.Fatalf("got %d widgets, want %d (filter dropped wrong rows)", got, want)
	}
	if v.Widgets[0].BeadID != "gd-widget00001" || v.Widgets[1].BeadID != "gd-widget00002" {
		t.Errorf("ordering wrong: %s, %s — expected position-asc", v.Widgets[0].BeadID, v.Widgets[1].BeadID)
	}
	if v.Widgets[0].WidgetType != "alert-summary" {
		t.Errorf("first widget type=%q, want alert-summary", v.Widgets[0].WidgetType)
	}
	if !strings.Contains(v.Widgets[0].DataJSON, `"count": 42`) {
		t.Errorf("first widget DataJSON=%q, want pretty-printed", v.Widgets[0].DataJSON)
	}
	if v.Widgets[0].Headline != "Alert: 502 spike" {
		t.Errorf("first widget headline override missing: %q", v.Widgets[0].Headline)
	}
}

func TestBuildRichReportView_DegradesOnFetchError(t *testing.T) {
	parentChild := "parent-child"

	parent := &beadsapi.BeadDetail{
		ID:     "gd-parent00001",
		Type:   "rich-report",
		Fields: map[string]string{"headline": "Test"},
	}
	mentionedBy := []beadsapi.BeadReferenceSummary{
		{ID: "gd-widget00001", Type: "widget-instance", RelationshipType: &parentChild, Title: "child summary"},
	}

	// fakeDaemon with no beads — GetBead returns 404.
	fake := &fakeDaemon{beads: map[string]*beadsapi.BeadDetail{}}

	v := buildRichReportView(context.Background(), parent, nil, mentionedBy, fake, nil)
	if v == nil || len(v.Widgets) != 1 {
		t.Fatalf("expected 1 placeholder card on fetch error, got %+v", v)
	}
	if v.Widgets[0].BeadID != "gd-widget00001" {
		t.Errorf("placeholder card lost the bead id: %q", v.Widgets[0].BeadID)
	}
	if v.Widgets[0].Headline != "child summary" {
		t.Errorf("placeholder card should fall back to ref title, got %q", v.Widgets[0].Headline)
	}
	if v.Widgets[0].WidgetType != "" || v.Widgets[0].DataJSON != "" {
		t.Errorf("placeholder card should have empty type/data on fetch error, got %+v", v.Widgets[0])
	}
}

// TestBuildRichReportView_StageChipsForAllStages confirms the rich-report
// card surfaces a chip for every widget-stage:* label — including
// widget-stage:stable. The chip is the operator-visible signal that a
// widget-type has been promoted (Stage-B B4 ka-0000002878).
func TestBuildRichReportView_StageChipsForAllStages(t *testing.T) {
	parentChild := "parent-child"
	parent := &beadsapi.BeadDetail{
		ID:     "gd-parent00001",
		Type:   "rich-report",
		Fields: map[string]string{"headline": "stage-chip test"},
	}
	mentionedBy := []beadsapi.BeadReferenceSummary{
		{ID: "gd-wi-stable0", Type: "widget-instance", RelationshipType: &parentChild},
		{ID: "gd-wi-draft00", Type: "widget-instance", RelationshipType: &parentChild},
		{ID: "gd-wi-deprec0", Type: "widget-instance", RelationshipType: &parentChild},
	}

	fake := &fakeDaemon{
		beads: map[string]*beadsapi.BeadDetail{
			"gd-wi-stable0": {ID: "gd-wi-stable0", Type: "widget-instance", Fields: map[string]string{"widget_type": "promoted-htmx", "position": "1"}},
			"gd-wi-draft00": {ID: "gd-wi-draft00", Type: "widget-instance", Fields: map[string]string{"widget_type": "draft-htmx", "position": "2"}},
			"gd-wi-deprec0": {ID: "gd-wi-deprec0", Type: "widget-instance", Fields: map[string]string{"widget_type": "retired-htmx", "position": "3"}},
		},
		listResp: &beadsapi.ListBeadsResult{
			Beads: []*beadsapi.BeadDetail{
				{ID: "gd-wt-stable0", Type: "widget-type", Labels: []string{"widget-stage:stable"}, Fields: map[string]string{"slug": "promoted-htmx"}},
				{ID: "gd-wt-draft00", Type: "widget-type", Labels: []string{"widget-stage:draft"}, Fields: map[string]string{"slug": "draft-htmx"}},
				{ID: "gd-wt-deprec0", Type: "widget-type", Labels: []string{"widget-stage:deprecated"}, Fields: map[string]string{"slug": "retired-htmx"}},
			},
			Total: 3,
		},
	}

	v := buildRichReportView(context.Background(), parent, nil, mentionedBy, fake, nil)
	if v == nil || len(v.Widgets) != 3 {
		t.Fatalf("expected 3 widget cards, got %+v", v)
	}
	bySlug := map[string]widgetCard{}
	for _, c := range v.Widgets {
		bySlug[c.WidgetType] = c
	}

	stable := bySlug["promoted-htmx"]
	if stable.Stage != "stable" {
		t.Errorf("stable card: Stage=%q, want stable", stable.Stage)
	}
	if stable.StageChip == "" {
		t.Error("stable card: StageChip is empty — chip would not render in beads-viewer")
	}
	if stable.StageClass != "stable" {
		t.Errorf("stable card: StageClass=%q, want stable (drives .chip-stage-stable CSS)", stable.StageClass)
	}

	if bySlug["draft-htmx"].StageChip != "DRAFT" {
		t.Errorf("draft card: StageChip=%q, want DRAFT", bySlug["draft-htmx"].StageChip)
	}
	if bySlug["retired-htmx"].StageChip != "DEPRECATED" {
		t.Errorf("deprecated card: StageChip=%q, want DEPRECATED", bySlug["retired-htmx"].StageChip)
	}
}

// slowFetcher is a widgetChildFetcher that simulates per-call latency so
// the test can prove concurrent hydration is happening (wall-clock budget
// would be N * delay if serial, ~delay if perfectly concurrent up to the
// fan-out cap).
type slowFetcher struct {
	mu      sync.Mutex
	beads   map[string]*beadsapi.BeadDetail
	delay   time.Duration
	maxLive int
	live    int
	peakObs int
	calls   int
}

func (f *slowFetcher) GetBead(_ context.Context, id string) (*beadsapi.BeadDetail, error) {
	f.mu.Lock()
	f.calls++
	f.live++
	if f.live > f.peakObs {
		f.peakObs = f.live
	}
	if f.maxLive > 0 && f.live > f.maxLive {
		f.live--
		f.mu.Unlock()
		return nil, errors.New("synthetic over-cap")
	}
	f.mu.Unlock()

	time.Sleep(f.delay)

	f.mu.Lock()
	f.live--
	b := f.beads[id]
	f.mu.Unlock()
	if b == nil {
		return nil, errors.New("not found")
	}
	return b, nil
}

func TestBuildRichReportView_ParallelHydration(t *testing.T) {
	parentChild := "parent-child"
	const n = 16

	parent := &beadsapi.BeadDetail{
		ID:     "gd-parent00001",
		Type:   "rich-report",
		Fields: map[string]string{"headline": "stress"},
	}
	mentionedBy := make([]beadsapi.BeadReferenceSummary, 0, n)
	beads := make(map[string]*beadsapi.BeadDetail, n)
	for i := 0; i < n; i++ {
		id := "gd-w" + strconv.Itoa(i)
		mentionedBy = append(mentionedBy, beadsapi.BeadReferenceSummary{
			ID: id, Type: "widget-instance", RelationshipType: &parentChild,
		})
		beads[id] = &beadsapi.BeadDetail{
			ID:   id,
			Type: "widget-instance",
			Fields: map[string]string{
				"widget_type": "alert-summary",
				"position":    strconv.Itoa(i),
				"data":        `{"i":` + strconv.Itoa(i) + `}`,
			},
		}
	}

	const callDelay = 25 * time.Millisecond
	fetcher := &slowFetcher{beads: beads, delay: callDelay}

	start := time.Now()
	v := buildRichReportView(context.Background(), parent, nil, mentionedBy, fetcher, nil)
	elapsed := time.Since(start)

	if got := len(v.Widgets); got != n {
		t.Fatalf("got %d widgets, want %d", got, n)
	}
	for i, c := range v.Widgets {
		if c.Position != i {
			t.Errorf("widgets[%d].Position=%d, want %d (sort failed)", i, c.Position, i)
		}
	}
	// Serial floor: n * callDelay = 400ms. Parallel ceiling
	// (cap=widgetHydrationConcurrency=8): 2 batches * delay = 50ms + scheduling.
	// Generous bound to avoid flaky CI: must beat the serial floor by >2x.
	serialFloor := time.Duration(n) * callDelay
	if elapsed >= serialFloor/2 {
		t.Errorf("elapsed=%v, expected substantially less than serial floor %v — fan-out not effective", elapsed, serialFloor)
	}
	// Concurrency cap respected: peak in-flight must not exceed the cap.
	if fetcher.peakObs > widgetHydrationConcurrency {
		t.Errorf("peak in-flight=%d exceeds cap=%d — fan-out unbounded", fetcher.peakObs, widgetHydrationConcurrency)
	}
	// And it actually used parallelism — peak should be > 1 for n=16.
	if fetcher.peakObs < 2 {
		t.Errorf("peak in-flight=%d, expected >1 — hydration ran serially", fetcher.peakObs)
	}
	if fetcher.calls != n {
		t.Errorf("got %d GetBead calls, want %d", fetcher.calls, n)
	}
}

func TestPrettyJSON_InvalidPassesThrough(t *testing.T) {
	out := prettyJSON("{not json")
	if out != "{not json" {
		t.Errorf("prettyJSON should pass invalid JSON through verbatim, got %q", out)
	}
}

// TestBuildRichReportView_WidgetSpan covers the widget_span field read path
// added by epic ka-0000003810 (width affordances). Known values pass through;
// unknown / empty values normalize to empty so the template falls back to the
// default auto-fill grid placement.
func TestBuildRichReportView_WidgetSpan(t *testing.T) {
	parentChild := "parent-child"
	parent := &beadsapi.BeadDetail{
		ID:     "gd-parent00001",
		Type:   "rich-report",
		Fields: map[string]string{"headline": "span test"},
	}
	mentionedBy := []beadsapi.BeadReferenceSummary{
		{ID: "gd-wi-full000", Type: "widget-instance", RelationshipType: &parentChild},
		{ID: "gd-wi-half000", Type: "widget-instance", RelationshipType: &parentChild},
		{ID: "gd-wi-auto000", Type: "widget-instance", RelationshipType: &parentChild},
		{ID: "gd-wi-bogus00", Type: "widget-instance", RelationshipType: &parentChild},
	}
	fake := &fakeDaemon{beads: map[string]*beadsapi.BeadDetail{
		"gd-wi-full000": {ID: "gd-wi-full000", Type: "widget-instance", Fields: map[string]string{"widget_type": "t", "position": "1", "widget_span": "full"}},
		"gd-wi-half000": {ID: "gd-wi-half000", Type: "widget-instance", Fields: map[string]string{"widget_type": "t", "position": "2", "widget_span": "half"}},
		"gd-wi-auto000": {ID: "gd-wi-auto000", Type: "widget-instance", Fields: map[string]string{"widget_type": "t", "position": "3"}},
		"gd-wi-bogus00": {ID: "gd-wi-bogus00", Type: "widget-instance", Fields: map[string]string{"widget_type": "t", "position": "4", "widget_span": "quintuple"}},
	}}

	v := buildRichReportView(context.Background(), parent, nil, mentionedBy, fake, nil)
	if v == nil || len(v.Widgets) != 4 {
		t.Fatalf("expected 4 widget cards, got %+v", v)
	}
	byID := map[string]widgetCard{}
	for _, c := range v.Widgets {
		byID[c.BeadID] = c
	}
	if got := byID["gd-wi-full000"].Span; got != "full" {
		t.Errorf("full span: got %q, want full", got)
	}
	if got := byID["gd-wi-half000"].Span; got != "half" {
		t.Errorf("half span: got %q, want half", got)
	}
	if got := byID["gd-wi-auto000"].Span; got != "" {
		t.Errorf("auto (unset) span: got %q, want empty string", got)
	}
	if got := byID["gd-wi-bogus00"].Span; got != "" {
		t.Errorf("bogus span value should normalize to empty: got %q", got)
	}
}
