package spendlogs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"
)

func testClient(masterKey string) *Client {
	c := New(masterKey, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	c.Throttle = 0 // no pacing in tests
	return c
}

// mkRow builds a synthetic row with the tag set the podspec stamps.
func mkRow(id, agent, role, project, model string, spend float64, start time.Time) Row {
	var tags []string
	if agent != "" {
		tags = append(tags, "agent:"+agent)
	}
	if role != "" {
		tags = append(tags, "role:"+role)
	}
	if project != "" {
		tags = append(tags, "project:"+project)
	}
	if model != "" {
		tags = append(tags, "model:"+model)
	}
	tags = append(tags, "User-Agent: claude-cli") // always present on live rows
	return Row{
		RequestID:   id,
		Spend:       spend,
		StartTime:   start.UTC().Format("2006-01-02T15:04:05.000+00:00"),
		Status:      "success",
		RequestTags: tags,
	}
}

// spendLogsHandler serves /spend/logs/v2 over a fixed row set, mimicking the
// live endpoint's envelope, page_size cap, and asc/desc sort. It asserts the
// request shape the library must send (naive-UTC dates, asc sort).
type spendLogsHandler struct {
	t    *testing.T
	rows []Row
	// failPage, when > 0, 500s that page number.
	failPage int
	requests int
}

func (h *spendLogsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.requests++
	if r.Header.Get("Authorization") != "Bearer test-master-key" {
		http.Error(w, `{"error":{"message":"Authentication Error","code":"401"}}`, http.StatusUnauthorized)
		return
	}
	switch r.URL.Path {
	case "/spend/logs/v2":
	default:
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	// The live server 400s RFC3339 and 422s page_size > 100 — enforce both so
	// a regression in the request builder fails loudly here.
	for _, k := range []string{"start_date", "end_date"} {
		if v := q.Get(k); len(v) != len("2006-01-02 15:04:05") || v[10] != ' ' {
			h.t.Errorf("%s=%q: live endpoint accepts only 'YYYY-MM-DD HH:MM:SS'", k, v)
		}
	}
	pageSize, _ := strconv.Atoi(q.Get("page_size"))
	if pageSize < 1 || pageSize > 100 {
		http.Error(w, `{"detail":[{"type":"less_than_equal","loc":["query","page_size"]}]}`, http.StatusUnprocessableEntity)
		return
	}
	if q.Get("sort_by") != "startTime" || q.Get("sort_order") != "asc" {
		h.t.Errorf("expected sort_by=startTime sort_order=asc, got %q %q — pagination is unstable under live writes otherwise", q.Get("sort_by"), q.Get("sort_order"))
	}
	pageNum, _ := strconv.Atoi(q.Get("page"))
	if pageNum < 1 {
		pageNum = 1
	}
	if pageNum == h.failPage {
		http.Error(w, "boom", http.StatusInternalServerError)
		return
	}

	// Serve rows filtered by the requested window (inclusive bounds — the
	// library widens by 1s and re-filters strictly client-side).
	start, _ := time.Parse(spendLogTimeFormat, q.Get("start_date"))
	end, _ := time.Parse(spendLogTimeFormat, q.Get("end_date"))
	var inWindow []Row
	for _, row := range h.rows {
		ts, ok := parseRowTime(row.StartTime)
		if !ok {
			h.t.Fatalf("fixture row %q has unparseable startTime %q", row.RequestID, row.StartTime)
		}
		if !ts.Before(start) && !ts.After(end) {
			inWindow = append(inWindow, row)
		}
	}
	total := len(inWindow)
	totalPages := (total + pageSize - 1) / pageSize
	lo := (pageNum - 1) * pageSize
	hi := min(lo+pageSize, total)
	var data []Row
	if lo < total {
		data = inWindow[lo:hi]
	}
	_ = json.NewEncoder(w).Encode(page{
		Data: data, Page: pageNum, PageSize: pageSize, Total: total, TotalPages: totalPages,
	})
}

func TestProjectRollupsAndWindow(t *testing.T) {
	base := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	h := &spendLogsHandler{t: t, rows: []Row{
		mkRow("r1", "agent-a", "crew", "gardenio", "claude-fable-5-oauth", 1.0, base.Add(1*time.Minute)),
		mkRow("r2", "agent-a", "crew", "gardenio", "claude-fable-5-oauth", 0.5, base.Add(2*time.Minute)),
		mkRow("r3", "agent-b", "gardener", "gardenio", "claude-fable-5-oauth", 2.0, base.Add(3*time.Minute)),
		mkRow("r4", "agent-c", "crew", "monorepo", "claude-haiku-4-5", 0.25, base.Add(4*time.Minute)),
		// No agent tag at all → UntaggedAgentUSD.
		{RequestID: "r5", Spend: 4.0, StartTime: base.Add(5 * time.Minute).Format(time.RFC3339), RequestTags: []string{"User-Agent: curl"}},
		// Outside the window on both sides — must not book.
		mkRow("r6", "agent-a", "crew", "gardenio", "claude-fable-5-oauth", 100, base.Add(-time.Hour)),
		mkRow("r7", "agent-a", "crew", "gardenio", "claude-fable-5-oauth", 100, base.Add(time.Hour)),
		// Exactly at the half-open `to` boundary: inside the 1s-widened server
		// window, so it IS served — the strict client-side filter must drop it.
		mkRow("r8", "agent-a", "crew", "gardenio", "claude-fable-5-oauth", 100, base.Add(10*time.Minute)),
	}}
	srv := httptest.NewServer(h)
	defer srv.Close()

	c := testClient("test-master-key")
	proj, err := c.Project(context.Background(), []string{srv.URL}, base, base.Add(10*time.Minute))
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if got, want := proj.TotalUSD, 7.75; math.Abs(got-want) > 1e-9 {
		t.Errorf("TotalUSD = %v, want %v", got, want)
	}
	if got := proj.Requests; got != 5 {
		t.Errorf("Requests = %d, want 5", got)
	}
	if got, want := proj.AgentUSD("agent-a"), 1.5; math.Abs(got-want) > 1e-9 {
		t.Errorf("agent-a = %v, want %v", got, want)
	}
	if got := proj.Agents["agent-a"].Requests; got != 2 {
		t.Errorf("agent-a requests = %d, want 2", got)
	}
	if got, want := proj.Agents["agent-a"].First, base.Add(1*time.Minute); !got.Equal(want) {
		t.Errorf("agent-a First = %v, want %v", got, want)
	}
	if got, want := proj.Agents["agent-a"].Last, base.Add(2*time.Minute); !got.Equal(want) {
		t.Errorf("agent-a Last = %v, want %v", got, want)
	}
	if got, want := proj.Roles["crew"], 1.75; math.Abs(got-want) > 1e-9 {
		t.Errorf("role crew = %v, want %v", got, want)
	}
	if got, want := proj.Projects["gardenio"], 3.5; math.Abs(got-want) > 1e-9 {
		t.Errorf("project gardenio = %v, want %v", got, want)
	}
	if got, want := proj.Models["claude-fable-5-oauth"], 3.5; math.Abs(got-want) > 1e-9 {
		t.Errorf("model claude-fable-5-oauth = %v, want %v", got, want)
	}
	if got, want := proj.UntaggedAgentUSD, 4.0; math.Abs(got-want) > 1e-9 {
		t.Errorf("UntaggedAgentUSD = %v, want %v", got, want)
	}
	// Identity: sum(agents) + untagged == total (single agent tag per row).
	var agentSum float64
	for _, a := range proj.Agents {
		agentSum += a.USD
	}
	if math.Abs(agentSum+proj.UntaggedAgentUSD-proj.TotalUSD) > 1e-9 {
		t.Errorf("attribution identity violated: agents %v + untagged %v != total %v", agentSum, proj.UntaggedAgentUSD, proj.TotalUSD)
	}
	if len(proj.Proxies) != 1 || proj.Proxies[0].OutOfWindow == 0 {
		t.Errorf("expected proxy stats with OutOfWindow > 0 (1s-widened server window), got %+v", proj.Proxies)
	}
}

func TestProjectPaginatesAndDedupes(t *testing.T) {
	base := time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC)
	var rows []Row
	for i := range 250 {
		rows = append(rows, mkRow(fmt.Sprintf("r%03d", i), "agent-x", "crew", "gardenio", "m", 0.01, base.Add(time.Duration(i)*time.Second)))
	}
	// A duplicate request_id (page-shift re-read under live writes) must book once.
	rows = append(rows, rows[0])
	h := &spendLogsHandler{t: t, rows: rows}
	srv := httptest.NewServer(h)
	defer srv.Close()

	c := testClient("test-master-key")
	proj, err := c.Project(context.Background(), []string{srv.URL}, base, base.Add(time.Hour))
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if got, want := proj.AgentUSD("agent-x"), 2.50; math.Abs(got-want) > 1e-9 {
		t.Errorf("agent-x = %v, want %v (dedup by request_id)", got, want)
	}
	st := proj.Proxies[0]
	if st.Pages != 3 {
		t.Errorf("Pages = %d, want 3 (251 rows / 100 per page)", st.Pages)
	}
	if st.Duplicates != 1 {
		t.Errorf("Duplicates = %d, want 1", st.Duplicates)
	}
	if st.ReportedTotal != 251 || st.RowsFetched != 251 {
		t.Errorf("ReportedTotal/RowsFetched = %d/%d, want 251/251", st.ReportedTotal, st.RowsFetched)
	}
}

func TestProjectMergesProxiesAndDedupesURLs(t *testing.T) {
	base := time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC)
	h1 := &spendLogsHandler{t: t, rows: []Row{mkRow("p1r1", "agent-m", "crew", "gardenio", "m", 1.0, base.Add(time.Minute))}}
	h2 := &spendLogsHandler{t: t, rows: []Row{mkRow("p2r1", "agent-m", "crew", "gardenio", "m", 2.0, base.Add(2*time.Minute))}}
	s1, s2 := httptest.NewServer(h1), httptest.NewServer(h2)
	defer s1.Close()
	defer s2.Close()

	c := testClient("test-master-key")
	// s1 listed twice (trailing slash variant) — must sweep once.
	proj, err := c.Project(context.Background(), []string{s1.URL, s1.URL + "/", s2.URL}, base, base.Add(time.Hour))
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if got, want := proj.AgentUSD("agent-m"), 3.0; math.Abs(got-want) > 1e-9 {
		t.Errorf("agent-m = %v, want %v (summed across proxies)", got, want)
	}
	if len(proj.Proxies) != 2 {
		t.Errorf("Proxies = %d, want 2 (URL dedup)", len(proj.Proxies))
	}
}

func TestProjectFailsClosed(t *testing.T) {
	base := time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC)
	var rows []Row
	for i := range 150 {
		rows = append(rows, mkRow(fmt.Sprintf("r%03d", i), "agent-x", "crew", "gardenio", "m", 0.01, base.Add(time.Duration(i)*time.Second)))
	}
	h := &spendLogsHandler{t: t, rows: rows, failPage: 2}
	srv := httptest.NewServer(h)
	defer srv.Close()

	c := testClient("test-master-key")
	if _, err := c.Project(context.Background(), []string{srv.URL}, base, base.Add(time.Hour)); err == nil {
		t.Fatal("Project must fail when any page fetch fails — a partial sum presented as complete defeats the audit")
	}

	// MaxPages exceeded is an error, not a silent truncation.
	h.failPage = 0
	c.MaxPages = 1
	_, err := c.Project(context.Background(), []string{srv.URL}, base, base.Add(time.Hour))
	if !errors.Is(err, ErrTooManyPages) {
		t.Fatalf("err = %v, want ErrTooManyPages", err)
	}

	// Empty/invalid inputs.
	c.MaxPages = 0
	if _, err := c.Project(context.Background(), nil, base, base.Add(time.Hour)); err == nil {
		t.Fatal("Project with no proxies must error")
	}
	if _, err := c.Project(context.Background(), []string{srv.URL}, base, base); err == nil {
		t.Fatal("Project with empty window must error")
	}
}

// TestProjectLiveFixture decodes a recorded /spend/logs/v2 page (LiteLLM
// v1.84.8, captured 2026-07-19, metadata blobs stripped) so the row/envelope
// decoding is pinned to the real wire shape, not just to mkRow's synthesis.
func TestProjectLiveFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/v2_page_live.json")
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(raw)
	}))
	defer srv.Close()

	from := time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC)
	c := testClient("test-master-key")
	proj, err := c.Project(context.Background(), []string{srv.URL}, from, from.Add(time.Hour))
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if proj.Requests != 5 {
		t.Fatalf("Requests = %d, want 5", proj.Requests)
	}
	// Values cross-checked against the capture with jq at record time.
	if got, want := proj.AgentUSD("gardenio-formula-ux-enhancement-gardener-wae2"), 0.8945825+0.08383374999999998; math.Abs(got-want) > 1e-9 {
		t.Errorf("gardener-wae2 = %v, want %v", got, want)
	}
	if got, want := proj.Projects["gardenio"], proj.TotalUSD; math.Abs(got-want) > 1e-9 {
		t.Errorf("project gardenio = %v, want the full page total %v", got, want)
	}
	if got, want := proj.Models["claude-fable-5-oauth"], proj.TotalUSD; math.Abs(got-want) > 1e-9 {
		t.Errorf("model group = %v, want %v", got, want)
	}
	if proj.UntaggedAgentUSD != 0 {
		t.Errorf("UntaggedAgentUSD = %v, want 0 (every captured row is agent-tagged)", proj.UntaggedAgentUSD)
	}
}

func TestParseRowTime(t *testing.T) {
	cases := []struct {
		in string
		ok bool
	}{
		{"2026-07-19T00:59:54.825+00:00", true}, // live shape
		{"2026-07-19T00:59:54Z", true},
		{"2026-07-19T00:59:54", true},
		{"2026-07-19", true},
		{"", false},
		{"not-a-time", false},
	}
	for _, tc := range cases {
		if _, ok := parseRowTime(tc.in); ok != tc.ok {
			t.Errorf("parseRowTime(%q) ok = %v, want %v", tc.in, ok, tc.ok)
		}
	}
}
