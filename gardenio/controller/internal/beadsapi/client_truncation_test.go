package beadsapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// The tests here pin the fix for the invisible 1000-row default list limit
// (ka-0000003547, class ka-0000003555). They assert on the REQUESTS the client
// makes (does it send an explicit limit?) and on the LOGS it emits (does a
// silent daemon truncation become loud?), not merely on the returned rows — a
// correct-looking result can still come from a query that silently rode the
// default cap, so the request/observability shape is the thing under test.
// Compare TestFleetArchetypesPaged_NeverFetchesUnbounded in internal/advice.

// --- H3: the typed "list all active X" choke point sends an explicit limit ---

// TestListBeadsWithFields_SendsExplicitLimit is the regression guard for H3:
// listBeadsWithFields (the funnel for ListAgentBeads / ListTaskBeads /
// ListDecisionBeads / …) must send an explicit, non-zero limit so it never
// silently inherits the daemon's defaultListBeadsLimit. reconciler.go builds
// the desired pod set from ListAgentBeads, so a silent cap here is a
// correctness bug. Reverting the `q.Set("limit", …)` line drops the param and
// fails this test.
func TestListBeadsWithFields_SendsExplicitLimit(t *testing.T) {
	var gotLimit string
	var sawLimitParam bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q, _ := url.ParseQuery(r.URL.RawQuery)
		if v, ok := q["limit"]; ok {
			sawLimitParam = true
			gotLimit = v[0]
		}
		_ = json.NewEncoder(w).Encode(listBeadsResponse{Beads: nil, Total: 0})
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	if _, err := c.ListAgentBeads(context.Background()); err != nil {
		t.Fatalf("ListAgentBeads: %v", err)
	}

	if !sawLimitParam {
		t.Fatal("ListAgentBeads sent no limit param — it will silently ride the daemon's 1000 default (ka-0000003547 H3)")
	}
	n, err := strconv.Atoi(gotLimit)
	if err != nil {
		t.Fatalf("limit param %q is not an integer: %v", gotLimit, err)
	}
	if n <= 0 {
		t.Errorf("limit param = %d; an unlimited request lets the daemon choose the page size", n)
	}
	if n != clientListAllLimit {
		t.Errorf("limit param = %d, want clientListAllLimit=%d", n, clientListAllLimit)
	}
}

// TestListBeadsWithFields_WarnsOnTruncation pins that the typed helper path
// turns a silent daemon truncation (Total > returned rows) into a loud WARN.
func TestListBeadsWithFields_WarnsOnTruncation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// One row returned, but the daemon reports far more matched — the
		// silent-truncation shape.
		_ = json.NewEncoder(w).Encode(listBeadsResponse{
			Beads: []beadJSON{{ID: "gd-1", Type: "agent", Status: "open",
				Fields: json.RawMessage(`{"agent":"a","project":"p","role":"crew"}`)}},
			Total: 5000,
		})
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	logs := withCapturedSlog(func() {
		if _, err := c.ListAgentBeads(context.Background()); err != nil {
			t.Fatalf("ListAgentBeads: %v", err)
		}
	})
	if !strings.Contains(logs, "silently truncated") {
		t.Errorf("expected a truncation WARN when Total(5000) > returned(1), got logs: %q", logs)
	}
}

// TestListBeadsWithFields_NoWarnWhenComplete guards the boundary: a complete
// page (Total == returned) must not warn, or every healthy list would be noisy.
func TestListBeadsWithFields_NoWarnWhenComplete(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(listBeadsResponse{
			Beads: []beadJSON{{ID: "gd-1", Type: "agent", Status: "open",
				Fields: json.RawMessage(`{"agent":"a","project":"p","role":"crew"}`)}},
			Total: 1,
		})
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	logs := withCapturedSlog(func() {
		if _, err := c.ListAgentBeads(context.Background()); err != nil {
			t.Fatalf("ListAgentBeads: %v", err)
		}
	})
	if strings.Contains(logs, "silently truncated") {
		t.Errorf("a complete page (Total == returned) must not warn, got logs: %q", logs)
	}
}

// --- Cross-cutting WARN net on ListBeadsFiltered ---

// TestListBeadsFiltered_WarnsOnOmittedLimitTruncation pins the cross-cutting
// net: an omitted-Limit query (the ~40 call sites whose intent is "everything")
// that the daemon silently capped must produce a loud WARN. This is the single
// change that surfaces all the latent truncations at once.
func TestListBeadsFiltered_WarnsOnOmittedLimitTruncation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(listBeadsResponse{
			Beads: []beadJSON{{ID: "gd-1"}},
			Total: 1000,
		})
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	logs := withCapturedSlog(func() {
		// No Limit set — the caller means "give me everything".
		if _, err := c.ListBeadsFiltered(context.Background(), ListBeadsQuery{
			Types: []string{"task"},
		}); err != nil {
			t.Fatalf("ListBeadsFiltered: %v", err)
		}
	})
	if !strings.Contains(logs, "silently truncated") {
		t.Errorf("expected a truncation WARN for an omitted-Limit query capped by the daemon, got: %q", logs)
	}
}

// TestListBeadsFiltered_NoWarnOnPaging pins that a deliberately-bounded query —
// an explicit Limit, as every paged walk uses — does NOT warn even though its
// page is shorter than Total. Paging callers (FleetArchetypesPaged,
// spend_settlement_sweep, jira_backfill) legitimately see Total > len on every
// non-final page; warning there would be pure noise and would punish the
// correct fix pattern. This is the false-positive guard.
func TestListBeadsFiltered_NoWarnOnPaging(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(listBeadsResponse{
			Beads: []beadJSON{{ID: "gd-1"}, {ID: "gd-2"}},
			Total: 5000, // far more than this page — normal for a paged walk
		})
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	logs := withCapturedSlog(func() {
		if _, err := c.ListBeadsFiltered(context.Background(), ListBeadsQuery{
			Types:  []string{"agent"},
			Limit:  2,
			Offset: 0,
		}); err != nil {
			t.Fatalf("ListBeadsFiltered: %v", err)
		}
	})
	if strings.Contains(logs, "silently truncated") {
		t.Errorf("an explicit-Limit (paged) query must NOT warn on Total > len — that is expected paging, got: %q", logs)
	}
}

// TestListBeadsFiltered_NoWarnOnCountOnly pins that a count-only query does not
// warn: it returns Total with a nil Beads by design, so len < Total is
// intrinsic to the query, not a truncation.
func TestListBeadsFiltered_NoWarnOnCountOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(listBeadsResponse{Beads: nil, Total: 4242})
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	logs := withCapturedSlog(func() {
		res, err := c.ListBeadsFiltered(context.Background(), ListBeadsQuery{
			Types:     []string{"task"},
			CountOnly: true,
		})
		if err != nil {
			t.Fatalf("ListBeadsFiltered: %v", err)
		}
		if res.Total != 4242 {
			t.Errorf("count-only Total = %d, want 4242", res.Total)
		}
	})
	if strings.Contains(logs, "silently truncated") {
		t.Errorf("a count-only query must NOT warn (nil Beads is by design), got: %q", logs)
	}
}

// TestListBeadsFiltered_NoWarnWhenComplete guards the omitted-Limit boundary:
// when the daemon returns the whole match set (Total == len), no warning fires.
func TestListBeadsFiltered_NoWarnWhenComplete(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(listBeadsResponse{
			Beads: []beadJSON{{ID: "gd-1"}, {ID: "gd-2"}},
			Total: 2,
		})
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	logs := withCapturedSlog(func() {
		if _, err := c.ListBeadsFiltered(context.Background(), ListBeadsQuery{
			Types: []string{"task"},
		}); err != nil {
			t.Fatalf("ListBeadsFiltered: %v", err)
		}
	})
	if strings.Contains(logs, "silently truncated") {
		t.Errorf("a complete omitted-Limit result (Total == len) must not warn, got: %q", logs)
	}
}
