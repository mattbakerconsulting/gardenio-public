package advice

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

func TestListOpenAdvice_ExcludesClosed(t *testing.T) {
	// Simulate a server that returns both open and closed advice beads
	// (e.g., if the server-side status filter has a bug).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"beads": []map[string]any{
				{"id": "gd-open1", "title": "Open advice", "status": "open", "type": "advice", "kind": "data", "labels": []string{"global"}},
				{"id": "gd-closed1", "title": "Closed advice", "status": "closed", "type": "advice", "kind": "data", "labels": []string{"global"}},
				{"id": "gd-open2", "title": "Another open", "status": "open", "type": "advice", "kind": "data", "labels": []string{"global"}},
			},
			"total": 3,
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client, err := beadsapi.New(beadsapi.Config{
		HTTPAddr: srv.URL,
	})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}
	defer client.Close()

	beads, err := ListOpenAdvice(context.Background(), client)
	if err != nil {
		t.Fatalf("ListOpenAdvice: %v", err)
	}

	// Should only return the 2 open beads, filtering out the closed one.
	if len(beads) != 2 {
		t.Errorf("expected 2 open beads, got %d", len(beads))
	}
	for _, b := range beads {
		if b.Status == "closed" {
			t.Errorf("closed bead %s should not be returned by ListOpenAdvice", b.ID)
		}
	}
}

func TestTokenize(t *testing.T) {
	tests := []struct {
		input string
		want  map[string]bool
	}{
		{"Check for duplicate JIRA tickets", map[string]bool{"check": true, "for": true, "duplicate": true, "jira": true, "tickets": true}},
		{"", map[string]bool{}},
		{"a b c", map[string]bool{}}, // all words <= 2 chars
	}
	for _, tt := range tests {
		got := tokenize(tt.input)
		for word, shouldExist := range tt.want {
			if shouldExist && !got[word] {
				t.Errorf("tokenize(%q): missing expected word %q", tt.input, word)
			}
			if !shouldExist && got[word] {
				t.Errorf("tokenize(%q): should not contain %q", tt.input, word)
			}
		}
	}
}

func TestJaccardSimilarity(t *testing.T) {
	tests := []struct {
		a, b map[string]bool
		want float64
	}{
		{map[string]bool{"a": true, "b": true}, map[string]bool{"a": true, "b": true}, 1.0},
		{map[string]bool{"a": true, "b": true}, map[string]bool{"c": true, "d": true}, 0.0},
		{map[string]bool{"a": true, "b": true}, map[string]bool{"a": true, "c": true}, 1.0 / 3.0},
		{map[string]bool{}, map[string]bool{}, 0.0},
	}
	for _, tt := range tests {
		got := jaccardSimilarity(tt.a, tt.b)
		if got < tt.want-0.01 || got > tt.want+0.01 {
			t.Errorf("jaccardSimilarity(%v, %v) = %.3f, want %.3f", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestFindSimilar(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"beads": []map[string]any{
				{"id": "gd-1", "title": "Check for duplicate JIRA tickets before starting work", "status": "open", "type": "advice", "kind": "data", "labels": []string{"global"}},
				{"id": "gd-2", "title": "Use API trigger for deploys", "status": "open", "type": "advice", "kind": "data", "labels": []string{"global"}},
			},
			"total": 2,
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}
	defer client.Close()

	// Should find the duplicate-checking advice as similar.
	similar, err := FindSimilar(context.Background(), client, "Check for existing MRs, duplicate tickets, and other agents before starting work")
	if err != nil {
		t.Fatalf("FindSimilar: %v", err)
	}
	if len(similar) == 0 {
		t.Fatal("expected at least 1 similar result, got 0")
	}
	if similar[0].Bead.ID != "gd-1" {
		t.Errorf("expected top similar to be gd-1, got %s", similar[0].Bead.ID)
	}

	// Completely unrelated title should find nothing.
	similar, err = FindSimilar(context.Background(), client, "Configure Playwright video recording settings")
	if err != nil {
		t.Fatalf("FindSimilar: %v", err)
	}
	if len(similar) != 0 {
		t.Errorf("expected 0 similar results for unrelated title, got %d", len(similar))
	}
}

func TestIsExpired(t *testing.T) {
	past := time.Now().Add(-1 * time.Hour).Format(time.RFC3339)
	future := time.Now().Add(1 * time.Hour).Format(time.RFC3339)

	tests := []struct {
		name   string
		fields map[string]string
		want   bool
	}{
		{"past expires_at is expired", map[string]string{"expires_at": past}, true},
		{"future expires_at is not expired", map[string]string{"expires_at": future}, false},
		{"missing expires_at is not expired", map[string]string{}, false},
		{"empty expires_at is not expired", map[string]string{"expires_at": ""}, false},
		{"malformed expires_at is not expired", map[string]string{"expires_at": "not-a-date"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bead := &beadsapi.BeadDetail{Fields: tt.fields}
			got := IsExpired(bead)
			if got != tt.want {
				t.Errorf("IsExpired(%v) = %v, want %v", tt.fields, got, tt.want)
			}
		})
	}
}

func TestListOpenAdvice_FiltersExpired(t *testing.T) {
	past := time.Now().Add(-1 * time.Hour).Format(time.RFC3339)
	future := time.Now().Add(1 * time.Hour).Format(time.RFC3339)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"beads": []map[string]any{
				{"id": "gd-past", "title": "Past", "status": "open", "type": "advice", "kind": "data", "labels": []string{"global"}, "fields": map[string]any{"expires_at": past}},
				{"id": "gd-future", "title": "Future", "status": "open", "type": "advice", "kind": "data", "labels": []string{"global"}, "fields": map[string]any{"expires_at": future}},
				{"id": "gd-missing", "title": "Missing", "status": "open", "type": "advice", "kind": "data", "labels": []string{"global"}},
				{"id": "gd-malformed", "title": "Malformed", "status": "open", "type": "advice", "kind": "data", "labels": []string{"global"}, "fields": map[string]any{"expires_at": "not-a-date"}},
			},
			"total": 4,
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}
	defer client.Close()

	beads, err := ListOpenAdvice(context.Background(), client)
	if err != nil {
		t.Fatalf("ListOpenAdvice: %v", err)
	}

	gotIDs := map[string]bool{}
	for _, b := range beads {
		gotIDs[b.ID] = true
	}
	if gotIDs["gd-past"] {
		t.Errorf("expected gd-past (expired) to be filtered out")
	}
	for _, id := range []string{"gd-future", "gd-missing", "gd-malformed"} {
		if !gotIDs[id] {
			t.Errorf("expected %s to pass through, but it was filtered", id)
		}
	}
}

func TestIsFixture(t *testing.T) {
	tests := []struct {
		name   string
		labels []string
		want   bool
	}{
		{"empty", nil, false},
		{"no fixture", []string{"global", "project:gardenio", "role:crew"}, false},
		{"fixture:e2e:advice", []string{"project:gardenio", "fixture:e2e:advice"}, true},
		{"fixture:e2e", []string{"global", "fixture:e2e"}, true},
		{"kind:fixture", []string{"global", "kind:fixture"}, true},
		{"fixture: prefix in group", []string{"g0:fixture:e2e", "g0:project:gardenio"}, true},
		{"unrelated kind", []string{"kind:bug"}, false},
		{"fixture word in title only", []string{"global"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := &beadsapi.BeadDetail{Labels: tt.labels}
			if got := IsFixture(b); got != tt.want {
				t.Errorf("IsFixture(%v) = %v, want %v", tt.labels, got, tt.want)
			}
		})
	}
}

func TestListAdviceForAgent_ExcludesFixture(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// First call (ListOpenAdvice) returns advice; subsequent calls
		// (agent-bead lookup for enrichment) get an empty result.
		resp := map[string]any{
			"beads": []map[string]any{
				{"id": "gd-real", "title": "Real advice", "status": "open", "type": "advice", "kind": "data", "labels": []string{"global"}},
				{"id": "gd-x0T8h0nEGd", "title": "[E2E fixture] advice bead", "status": "open", "type": "advice", "kind": "data", "labels": []string{"project:gardenio", "fixture:e2e:advice"}},
				{"id": "gd-other-fixture", "title": "Another fixture", "status": "open", "type": "advice", "kind": "data", "labels": []string{"global", "kind:fixture"}},
			},
			"total": 3,
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}
	defer client.Close()

	matched, _, err := ListAdviceForAgent(context.Background(), client, "gardenio/crews/test-bot")
	if err != nil {
		t.Fatalf("ListAdviceForAgent: %v", err)
	}

	for _, m := range matched {
		if IsFixture(m.Bead) {
			t.Errorf("fixture advice %s leaked into agent loadout (labels=%v)", m.Bead.ID, m.Bead.Labels)
		}
		if m.Bead.ID == "gd-x0T8h0nEGd" || m.Bead.ID == "gd-other-fixture" {
			t.Errorf("fixture advice %s should be excluded", m.Bead.ID)
		}
	}
	// gd-real should still be there.
	var sawReal bool
	for _, m := range matched {
		if m.Bead.ID == "gd-real" {
			sawReal = true
		}
	}
	if !sawReal {
		t.Errorf("expected gd-real to be in the loadout, got %d matches", len(matched))
	}
}

func TestListOpenAdvice_AllOpen(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify the request includes status=open filter.
		status := r.URL.Query().Get("status")
		if status != "open" {
			t.Errorf("expected status=open query param, got %q", status)
		}

		resp := map[string]any{
			"beads": []map[string]any{
				{"id": "gd-1", "title": "Advice 1", "status": "open", "type": "advice", "kind": "data", "labels": []string{"global"}},
				{"id": "gd-2", "title": "Advice 2", "status": "open", "type": "advice", "kind": "data", "labels": []string{"role:crew"}},
			},
			"total": 2,
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client, err := beadsapi.New(beadsapi.Config{
		HTTPAddr: srv.URL,
	})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}
	defer client.Close()

	beads, err := ListOpenAdvice(context.Background(), client)
	if err != nil {
		t.Fatalf("ListOpenAdvice: %v", err)
	}

	if len(beads) != 2 {
		t.Errorf("expected 2 beads, got %d", len(beads))
	}
}

// pagingLister records every ListBeadsFiltered call and serves agent beads from a
// fixed corpus using the query's Limit/Offset, so tests can assert the WALK shape
// (page sizes, offsets, page count) and not merely the final result.
type pagingLister struct {
	corpus []*beadsapi.BeadDetail
	calls  []beadsapi.ListBeadsQuery
	err    error
}

func (p *pagingLister) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	p.calls = append(p.calls, q)
	if p.err != nil {
		return nil, p.err
	}
	start := min(q.Offset, len(p.corpus))
	end := min(start+q.Limit, len(p.corpus))
	return &beadsapi.ListBeadsResult{Beads: p.corpus[start:end], Total: len(p.corpus)}, nil
}

// agentBeadFixture builds a type=agent bead carrying project/role as custom
// fields, the shape agentBeadAttr reads first.
func agentBeadFixture(project, role string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{Fields: map[string]string{"project": project, "role": role}}
}

// TestFleetArchetypesPaged_NeverFetchesUnbounded is the regression guard for
// ka-0000006669: the archetype derivation must never ask the daemon for the whole
// 21-day agent-bead window in one shot. That is what allocated 66.4 MiB into a
// 256Mi controller, OOMKilled it, and took the cost gauges dark for ~7.75h.
//
// It asserts on the REQUESTS, not the result, because a correct-looking archetype
// set can still be derived by an unbounded fetch — the memory profile is the thing
// under test.
func TestFleetArchetypesPaged_NeverFetchesUnbounded(t *testing.T) {
	// 1,200 beads = 3 pages at the 500 page size, so paging is actually exercised.
	corpus := make([]*beadsapi.BeadDetail, 0, 1200)
	for i := range 1200 {
		corpus = append(corpus, agentBeadFixture("gardenio", []string{"crew", "gardener", "thread"}[i%3]))
	}
	lister := &pagingLister{corpus: corpus}

	got, err := FleetArchetypesPaged(context.Background(), lister, time.Now())
	if err != nil {
		t.Fatalf("FleetArchetypesPaged: %v", err)
	}

	if len(lister.calls) == 0 {
		t.Fatal("no daemon calls recorded")
	}
	for i, c := range lister.calls {
		if c.Limit > FleetArchetypePageSize {
			t.Errorf("call %d requested Limit=%d, exceeds page size %d — an unbounded fetch would re-open ka-0000006669",
				i, c.Limit, FleetArchetypePageSize)
		}
		if c.Limit <= 0 {
			t.Errorf("call %d requested Limit=%d; an unlimited request lets the daemon choose the page size", i, c.Limit)
		}
		if want := i * FleetArchetypePageSize; c.Offset != want {
			t.Errorf("call %d Offset = %d, want %d (pages must advance or the walk repeats/skips rows)", i, c.Offset, want)
		}
		// created_at is immutable, so it is the only sort that gives a stable
		// page sequence while new agent beads are being created underneath us.
		if c.Sort != "created_at" {
			t.Errorf("call %d Sort = %q, want \"created_at\" for a stable paged walk", i, c.Sort)
		}
	}

	// 1200 beads / 500 per page = 3 pages; the 3rd is short, so the walk stops.
	if len(lister.calls) != 3 {
		t.Errorf("made %d calls, want 3 (1200 beads / %d per page)", len(lister.calls), FleetArchetypePageSize)
	}

	// The whole point: a bounded walk still derives the complete archetype set.
	want := []Archetype{
		{Project: "gardenio", Role: "crew"},
		{Project: "gardenio", Role: "gardener"},
		{Project: "gardenio", Role: "thread"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d archetypes %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("archetype[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

// TestFleetArchetypesPaged_MatchesSingleShot pins that paging is a pure
// refactor of the derivation: folding page-by-page returns byte-identical output
// to deriving from the whole corpus at once (same set, same canonical order).
func TestFleetArchetypesPaged_MatchesSingleShot(t *testing.T) {
	corpus := []*beadsapi.BeadDetail{}
	for i := range 750 { // spans a page boundary
		corpus = append(corpus,
			agentBeadFixture([]string{"gardenio", "monorepo"}[i%2], []string{"crew", "gardener-bugfix"}[i%2]))
	}

	paged, err := FleetArchetypesPaged(context.Background(), &pagingLister{corpus: corpus}, time.Now())
	if err != nil {
		t.Fatalf("FleetArchetypesPaged: %v", err)
	}
	single := ArchetypesFromAgentBeads(corpus)

	if len(paged) != len(single) {
		t.Fatalf("paged got %d archetypes %v, single-shot got %d %v", len(paged), paged, len(single), single)
	}
	for i := range single {
		if paged[i] != single[i] {
			t.Errorf("archetype[%d]: paged = %v, single-shot = %v", i, paged[i], single[i])
		}
	}
}

// TestFleetArchetypesPaged_StopsOnShortPage guards the termination condition: a
// corpus smaller than one page must cost exactly one request, not a walk to the
// max-pages cap.
func TestFleetArchetypesPaged_StopsOnShortPage(t *testing.T) {
	lister := &pagingLister{corpus: []*beadsapi.BeadDetail{agentBeadFixture("gardenio", "crew")}}
	if _, err := FleetArchetypesPaged(context.Background(), lister, time.Now()); err != nil {
		t.Fatalf("FleetArchetypesPaged: %v", err)
	}
	if len(lister.calls) != 1 {
		t.Errorf("made %d calls for a 1-bead corpus, want 1 — a short page means the walk is done", len(lister.calls))
	}
}

// TestFleetArchetypesPaged_WindowIsBounded pins that the walk actually constrains
// the query to FleetArchetypeWindow. Without CreatedAfter the daemon would be asked
// for every agent bead ever created.
func TestFleetArchetypesPaged_WindowIsBounded(t *testing.T) {
	now := time.Now()
	lister := &pagingLister{corpus: []*beadsapi.BeadDetail{agentBeadFixture("gardenio", "crew")}}
	if _, err := FleetArchetypesPaged(context.Background(), lister, now); err != nil {
		t.Fatalf("FleetArchetypesPaged: %v", err)
	}
	got := lister.calls[0].CreatedAfter
	if got.IsZero() {
		t.Fatal("CreatedAfter is zero — the walk would scan every agent bead ever created")
	}
	if want := now.Add(-FleetArchetypeWindow); !got.Equal(want) {
		t.Errorf("CreatedAfter = %v, want %v", got, want)
	}
}

// legacyAdvicePoolCap is the old single-shot candidate-pool cap that F10
// (ka-0000005760) removed. Kept as a named constant only so the invariant test
// below can assert the fetch no longer carries it and that the returned pool
// exceeds it.
const legacyAdvicePoolCap = 500

// TestListOpenAdvice_NoSilentTruncation is the F10 remediation invariant
// (ka-0000005760; supersedes the old TestListOpenAdvice_PoolLimit
// characterization from advice-truncation design ka-0000003643 §F10).
//
// It pins the real no-silent-drop guarantee: ListOpenAdvice returns the ENTIRE
// open-advice set even when that set is larger than the old 500-item cap. The
// candidate pool must never be truncated BEFORE matching / supersession / budget
// run — that drop was invisible to every budget metric and truncation footer and
// failed CLOSED (advice silently missing) as the corpus grew.
//
// It drives the real ListOpenAdvice → client → HTTP path against an httptest
// daemon that pages correctly (honors offset+limit, reports the full Total). The
// corpus is deliberately larger than the retired 500 cap so a regression back to
// a single capped fetch would drop the excess and fail this test.
func TestListOpenAdvice_NoSilentTruncation(t *testing.T) {
	const corpusSize = 650 // > the retired 500 cap: a capped fetch would drop 150

	// Build the full corpus of distinct open advice beads once.
	corpus := make([]map[string]any, 0, corpusSize)
	for i := range corpusSize {
		corpus = append(corpus, map[string]any{
			"id": "gd-pool" + strconv.Itoa(i), "title": "Pool advice " + strconv.Itoa(i),
			"status": "open", "type": "advice", "kind": "data", "labels": []string{"global"},
		})
	}

	var sawExplicit500 bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("limit") == strconv.Itoa(legacyAdvicePoolCap) {
			sawExplicit500 = true // the retired single-shot cap must never reappear
		}
		// Honor offset+limit like the real daemon so the paged walk exhausts.
		limit := corpusSize
		if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 && n < limit {
			limit = n
		}
		offset := 0
		if n, err := strconv.Atoi(q.Get("offset")); err == nil && n > 0 {
			offset = n
		}
		if offset > corpusSize {
			offset = corpusSize
		}
		end := min(offset+limit, corpusSize)
		w.Header().Set("Content-Type", "application/json")
		// Total always reflects the FULL match, even on a partial page.
		json.NewEncoder(w).Encode(map[string]any{"beads": corpus[offset:end], "total": corpusSize})
	}))
	defer srv.Close()

	client, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}
	defer client.Close()

	got, err := ListOpenAdvice(context.Background(), client)
	if err != nil {
		t.Fatalf("ListOpenAdvice: %v", err)
	}

	// The whole open set comes back — nothing dropped upstream of budgeting.
	if len(got) != corpusSize {
		t.Errorf("pool size = %d, want %d — a %d-bead open-advice corpus must be returned in FULL; "+
			"a smaller result means the candidate pool was silently truncated (F10 regression)",
			len(got), corpusSize, corpusSize)
	}
	// And it must not have gone back to the retired single-shot 500 cap.
	if sawExplicit500 {
		t.Errorf("candidate-pool fetch carried the retired limit=%d cap — ListOpenAdvice must page "+
			"the whole open set, not re-cap at 500 (F10, ka-0000005760)", legacyAdvicePoolCap)
	}
	// Every returned bead must be distinct (paged walk dedups by id).
	seen := make(map[string]struct{}, len(got))
	for _, b := range got {
		if _, dup := seen[b.ID]; dup {
			t.Errorf("duplicate bead %q in paged result — dedup broken", b.ID)
		}
		seen[b.ID] = struct{}{}
	}
}
