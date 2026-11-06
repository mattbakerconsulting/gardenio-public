package bridge

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGrafanaClient_Search(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/search" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("type"); got != "dash-db" {
			t.Errorf("expected type=dash-db, got %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok123" {
			t.Errorf("missing/bad bearer: %q", got)
		}
		_, _ = w.Write([]byte(`[{"uid":"a","title":"A","type":"dash-db"},{"uid":"b","title":"B","type":"dash-db"}]`))
	}))
	defer srv.Close()

	c := NewGrafanaClient(GrafanaClientConfig{BaseURL: srv.URL + "/", Token: "tok123", Logger: slog.Default()})
	refs, err := c.Search(context.Background())
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(refs) != 2 || refs[0].UID != "a" || refs[1].Title != "B" {
		t.Fatalf("unexpected refs: %+v", refs)
	}
}

func TestGrafanaClient_SearchError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`boom`))
	}))
	defer srv.Close()
	c := NewGrafanaClient(GrafanaClientConfig{BaseURL: srv.URL})
	if _, err := c.Search(context.Background()); err == nil {
		t.Fatal("expected error on 500")
	}
}

func TestGrafanaClient_DashboardByUID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/dashboards/uid/dash1" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"meta":{"version":7},"dashboard":{"uid":"dash1","title":"D","version":7,"panels":[]}}`))
	}))
	defer srv.Close()

	c := NewGrafanaClient(GrafanaClientConfig{BaseURL: srv.URL})
	model, version, err := c.DashboardByUID(context.Background(), "dash1")
	if err != nil {
		t.Fatalf("get uid: %v", err)
	}
	if version != 7 {
		t.Fatalf("expected version 7, got %d", version)
	}
	meta, err := ParseDashboardMeta(model)
	if err != nil || meta.UID != "dash1" {
		t.Fatalf("unexpected model: %s (%v)", model, err)
	}
}

func TestGrafanaClient_DashboardByUID_MetaFallback(t *testing.T) {
	// meta.version=0 → fall back to the model's embedded version.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"meta":{"version":0},"dashboard":{"uid":"d","title":"D","version":3}}`))
	}))
	defer srv.Close()
	c := NewGrafanaClient(GrafanaClientConfig{BaseURL: srv.URL})
	_, version, err := c.DashboardByUID(context.Background(), "d")
	if err != nil {
		t.Fatalf("get uid: %v", err)
	}
	if version != 3 {
		t.Fatalf("expected fallback version 3, got %d", version)
	}
}

func TestGrafanaClient_DashboardByUID_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := NewGrafanaClient(GrafanaClientConfig{BaseURL: srv.URL})
	model, version, err := c.DashboardByUID(context.Background(), "gone")
	if err != nil {
		t.Fatalf("404 should not be an error, got %v", err)
	}
	if model != nil || version != 0 {
		t.Fatalf("expected nil model/0 version on 404, got %s / %d", model, version)
	}
}

func TestGrafanaClient_DashboardByUID_EmptyDashboard(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"meta":{"version":1}}`)) // no dashboard field
	}))
	defer srv.Close()
	c := NewGrafanaClient(GrafanaClientConfig{BaseURL: srv.URL})
	if _, _, err := c.DashboardByUID(context.Background(), "x"); err == nil {
		t.Fatal("expected error when dashboard field is empty")
	}
}

func TestGrafanaClient_DashboardByUID_BadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`not json`))
	}))
	defer srv.Close()
	c := NewGrafanaClient(GrafanaClientConfig{BaseURL: srv.URL})
	if _, _, err := c.DashboardByUID(context.Background(), "x"); err == nil {
		t.Fatal("expected decode error")
	}
}

func TestGrafanaClient_SearchBadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{not-an-array}`))
	}))
	defer srv.Close()
	c := NewGrafanaClient(GrafanaClientConfig{BaseURL: srv.URL})
	if _, err := c.Search(context.Background()); err == nil {
		t.Fatal("expected decode error on bad search json")
	}
}

func TestSnippetTruncates(t *testing.T) {
	long := strings.Repeat("x", 500)
	if got := snippet([]byte(long)); len(got) <= 200 || !strings.HasSuffix(got, "…") {
		t.Fatalf("expected truncated snippet with ellipsis, got len=%d", len(got))
	}
	if got := snippet([]byte("  short  ")); got != "short" {
		t.Fatalf("expected trimmed short snippet, got %q", got)
	}
}

func TestParseConflictPolicy(t *testing.T) {
	cases := map[string]ConflictPolicy{
		"beads-wins":   ConflictBeadsWins,
		"grafana-wins": ConflictGrafanaWins,
		"manual":       ConflictManual,
		"":             ConflictGrafanaWins, // default
		"nonsense":     ConflictGrafanaWins, // default
	}
	for in, want := range cases {
		if got := ParseConflictPolicy(in); got != want {
			t.Errorf("ParseConflictPolicy(%q) = %q, want %q", in, got, want)
		}
	}
}
