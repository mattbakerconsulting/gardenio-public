package beadsapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Tests for the short_title column on the bead wire shape (gd-V5nGseRS5f).
// The daemon side lands separately; these pin that the client carries the
// key both ways and that an absent key decodes to "" (the bridge then falls
// back to fields.short_title and the heuristics).

func TestShortTitle_GetBeadDecodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"gd-x","title":"t","short_title":"s"}`))
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	got, err := c.GetBead(context.Background(), "gd-x")
	if err != nil {
		t.Fatalf("GetBead: %v", err)
	}
	if got.ShortTitle != "s" {
		t.Errorf("ShortTitle = %q, want s", got.ShortTitle)
	}
}

func TestShortTitle_AbsentDecodesEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"gd-x","title":"t"}`))
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	got, err := c.GetBead(context.Background(), "gd-x")
	if err != nil {
		t.Fatalf("GetBead: %v", err)
	}
	if got.ShortTitle != "" {
		t.Errorf("ShortTitle = %q, want empty", got.ShortTitle)
	}
}

func TestShortTitle_BatchGetBeadsCarriesIt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results":[
			{"id":"gd-a","bead":{"id":"gd-a","title":"A","type":"task","short_title":"short a"}}
		]}`))
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	out, err := c.BatchGetBeads(context.Background(), []string{"gd-a"})
	if err != nil {
		t.Fatalf("BatchGetBeads: %v", err)
	}
	if out["gd-a"] == nil || out["gd-a"].ShortTitle != "short a" {
		t.Errorf("out[gd-a] = %+v, want ShortTitle \"short a\"", out["gd-a"])
	}
}

func TestShortTitle_CreateBeadRequestMarshals(t *testing.T) {
	b, err := json.Marshal(CreateBeadRequest{Title: "t", Type: "task", ShortTitle: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"short_title":"s"`) {
		t.Errorf("marshalled %s, want \"short_title\":\"s\"", b)
	}

	// Empty is unset and omitted, so a daemon that predates the column never
	// sees the key.
	b, err = json.Marshal(CreateBeadRequest{Title: "t", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "short_title") {
		t.Errorf("marshalled %s, want no short_title key", b)
	}
}

func TestShortTitle_UpdateBeadSendsIt(t *testing.T) {
	for _, tc := range []struct {
		name       string
		req        func() UpdateBeadRequest
		wantShort  any
		wantOnlyIf bool
	}{
		{
			name: "replace",
			req: func() UpdateBeadRequest {
				s := "new short"
				return UpdateBeadRequest{ShortTitle: &s}
			},
			wantShort: "new short",
		},
		{
			name: "fill only if empty",
			req: func() UpdateBeadRequest {
				s := "fill"
				return UpdateBeadRequest{ShortTitle: &s, OnlyIfShortTitleEmpty: true}
			},
			wantShort:  "fill",
			wantOnlyIf: true,
		},
		{
			name: "guard alone sends nothing",
			req: func() UpdateBeadRequest {
				t := "title"
				return UpdateBeadRequest{Title: &t, OnlyIfShortTitleEmpty: true}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotBody map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(body, &gotBody)
				w.WriteHeader(http.StatusNoContent)
			}))
			defer srv.Close()

			c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
			if err := c.UpdateBead(context.Background(), "gd-x", tc.req()); err != nil {
				t.Fatalf("UpdateBead: %v", err)
			}
			if gotBody["short_title"] != tc.wantShort {
				t.Errorf("short_title = %v, want %v (body %v)", gotBody["short_title"], tc.wantShort, gotBody)
			}
			_, hasGuard := gotBody["only_if_short_title_empty"]
			if hasGuard != tc.wantOnlyIf {
				t.Errorf("only_if_short_title_empty present = %v, want %v (body %v)", hasGuard, tc.wantOnlyIf, gotBody)
			}
		})
	}
}

func TestShortTitle_ListBeadsFilteredMissingParam(t *testing.T) {
	for _, tc := range []struct {
		name      string
		missing   bool
		wantParam bool
	}{
		{"set emits the param", true, true},
		{"unset omits the param", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotQuery string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotQuery = r.URL.RawQuery
				_ = json.NewEncoder(w).Encode(listBeadsResponse{Beads: nil, Total: 0})
			}))
			defer srv.Close()

			c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
			if _, err := c.ListBeadsFiltered(context.Background(), ListBeadsQuery{
				Statuses:          []string{"open"},
				ShortTitleMissing: tc.missing,
				Limit:             20,
			}); err != nil {
				t.Fatalf("ListBeadsFiltered: %v", err)
			}
			if got := strings.Contains(gotQuery, "short_title_missing=true"); got != tc.wantParam {
				t.Errorf("query %q contains short_title_missing=true = %v, want %v", gotQuery, got, tc.wantParam)
			}
		})
	}
}

// captureCreates serves a stub daemon that records the short_title sent on
// every POST /v1/beads (nil when the key is absent). Every other request gets
// an empty list or an empty object, which is enough for SpawnAgent's dedup
// lookup and best-effort label writes.
func captureCreates(t *testing.T) (*Client, *[]*string) {
	t.Helper()
	got := &[]*string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/beads" {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decoding create body: %v", err)
			}
			var st *string
			if v, ok := body["short_title"].(string); ok {
				st = &v
			}
			*got = append(*got, st)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"gd-new"}`))
			return
		}
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"beads":[],"total":0}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	return &Client{baseURL: srv.URL, httpClient: srv.Client()}, got
}

// TestCreateBeadDetail_DefaultsShortTitle pins the single create choke point
// (Task 6 of plan gd-YZATAIOhfW): a gated type with no short_title gets the
// derived one, an explicit value is never overridden, and an ungated type is
// left for the bridge's render-time heuristics.
func TestCreateBeadDetail_DefaultsShortTitle(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  CreateBeadRequest
		want *string // nil: no short_title key on the wire
	}{
		{
			name: "task: derived from the title",
			req:  CreateBeadRequest{Type: "task", Title: "beads-viewer: show short_title in list rows (gd-1DvGBBCE31)"},
			want: ptr("show short_title in list rows"),
		},
		{
			name: "explicit value is untouched",
			req:  CreateBeadRequest{Type: "task", Title: "beads-viewer: show short_title in list rows", ShortTitle: "my own name"},
			want: ptr("my own name"),
		},
		{
			name: "escalation: the ref prefix is the name",
			req:  CreateBeadRequest{Type: "escalation", Title: "Merge PR #295: first-admin hook brute-force flag (gd-DAy1iWvO4y…)"},
			want: ptr("Merge PR #295"),
		},
		{
			name: "decision: the trailing question is kept",
			req:  CreateBeadRequest{Type: "decision", Title: "cicd watchdog fired 3 times overnight — extend watchdog coverage?"},
			want: ptr("extend watchdog coverage?"),
		},
		{
			name: "thread head: bridge prefix stripped",
			req:  CreateBeadRequest{Type: "task", Title: "Slack thread: messages sent to slack often contain links to beads.. the thing is often"},
			want: ptr("messages sent to slack often contain…"),
		},
		{
			name: "agent: the handle without @",
			req:  CreateBeadRequest{Type: "agent", Title: "gardenio-task-6-controller-scx"},
			want: ptr("gardenio-task-6-controller-scx"),
		},
		{
			name: "message: not gated, no key",
			req:  CreateBeadRequest{Type: "message", Title: "beads-viewer: show short_title in list rows"},
			want: nil,
		},
		{
			name: "gated but nothing usable derives: no key",
			req:  CreateBeadRequest{Type: "task", Title: "gd-abcdef"},
			want: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, got := captureCreates(t)
			if _, err := c.CreateBeadDetail(context.Background(), tc.req); err != nil {
				t.Fatalf("CreateBeadDetail: %v", err)
			}
			if len(*got) != 1 {
				t.Fatalf("want 1 create, got %d", len(*got))
			}
			switch st := (*got)[0]; {
			case tc.want == nil && st != nil:
				t.Errorf("short_title = %q, want absent", *st)
			case tc.want != nil && st == nil:
				t.Errorf("short_title absent, want %q", *tc.want)
			case tc.want != nil && *st != *tc.want:
				t.Errorf("short_title = %q, want %q", *st, *tc.want)
			}
		})
	}
}

// TestSpawnAgent_ShortTitleIsHandle: the agent bead SpawnAgent mints carries
// its handle as the short title.
func TestSpawnAgent_ShortTitleIsHandle(t *testing.T) {
	c, got := captureCreates(t)
	if _, err := c.SpawnAgent(context.Background(), "gardenio-task-6-controller-scx", "gardenio", "", "crew", ""); err != nil {
		t.Fatalf("SpawnAgent: %v", err)
	}
	if len(*got) != 1 || (*got)[0] == nil {
		t.Fatalf("want 1 create carrying short_title, got %v", *got)
	}
	if st := *(*got)[0]; st != "gardenio-task-6-controller-scx" {
		t.Errorf("short_title = %q, want the handle", st)
	}
}

func ptr(s string) *string { return &s }
