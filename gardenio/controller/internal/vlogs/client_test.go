package vlogs

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestComponentsAndAllowlist(t *testing.T) {
	comps := Components()
	if len(comps) == 0 {
		t.Fatal("Components() returned empty allowlist")
	}
	// Sorted + contains the four core debug targets and the two webhook doors.
	for _, want := range []string{"controller", "slack-bridge", "trigger-engine", "grafana-bridge", "jira-bridge", "gitlab-bridge"} {
		if !IsAllowed(want) {
			t.Errorf("expected %q to be allowed", want)
		}
	}
	for i := 1; i < len(comps); i++ {
		if comps[i-1] > comps[i] {
			t.Errorf("Components() not sorted: %v", comps)
		}
	}
	if IsAllowed("etcd") || IsAllowed("kube-apiserver") || IsAllowed("") {
		t.Error("arbitrary/empty components must NOT be allowed")
	}
}

func TestNormalizeClampsAndValidates(t *testing.T) {
	// Disallowed component is a hard error.
	if _, _, _, err := (QueryOptions{Component: "not-a-component"}).normalize(); err == nil {
		t.Fatal("expected error for disallowed component")
	}

	tests := []struct {
		name      string
		in        QueryOptions
		wantSince time.Duration
		wantLimit int
	}{
		{"defaults", QueryOptions{Component: "controller"}, DefaultSince, DefaultLimit},
		{"below-min-since", QueryOptions{Component: "controller", Since: time.Second}, MinSince, DefaultLimit},
		{"above-max-since", QueryOptions{Component: "controller", Since: 72 * time.Hour}, MaxSince, DefaultLimit},
		{"above-max-limit", QueryOptions{Component: "controller", Limit: 99999}, DefaultSince, MaxLimit},
		{"in-range", QueryOptions{Component: "controller", Since: 30 * time.Minute, Limit: 50}, 30 * time.Minute, 50},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src, since, limit, err := tt.in.normalize()
			if err != nil {
				t.Fatalf("normalize: %v", err)
			}
			if src != "controller" {
				t.Errorf("source = %q, want controller", src)
			}
			if since != tt.wantSince {
				t.Errorf("since = %v, want %v", since, tt.wantSince)
			}
			if limit != tt.wantLimit {
				t.Errorf("limit = %v, want %v", limit, tt.wantLimit)
			}
		})
	}
}

func TestBuildLogsQL(t *testing.T) {
	got, err := BuildLogsQL(QueryOptions{Component: "slack-bridge", Since: 15 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{source="slack-bridge"} _time:15m`; got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	// Grep is quoted as an exact phrase; hour window trims to "1h".
	got, err = BuildLogsQL(QueryOptions{Component: "controller", Since: time.Hour, Grep: "spawn failed"})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{source="controller"} _time:1h "spawn failed"`; got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	// A grep with quotes/backslashes cannot inject raw LogsQL — it stays quoted.
	got, _ = BuildLogsQL(QueryOptions{Component: "controller", Grep: `a"b\c`})
	if !strings.Contains(got, `"a\"b\\c"`) {
		t.Errorf("grep not safely quoted: %q", got)
	}

	// Disallowed component errors.
	if _, err := BuildLogsQL(QueryOptions{Component: "bogus"}); err == nil {
		t.Error("expected error for disallowed component")
	}
}

func TestQueryRoundTrip(t *testing.T) {
	var gotQuery, gotLimit, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/select/logsql/query" {
			http.Error(w, "wrong path", http.StatusNotFound)
			return
		}
		gotCT = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		vals, _ := parseForm(string(body))
		gotQuery = vals["query"]
		gotLimit = vals["limit"]
		// VictoriaLogs returns newline-delimited JSON.
		io.WriteString(w, `{"_time":"2026-08-29T00:00:00Z","_msg":"hello","source":"controller","level":"INFO"}`+"\n")
		io.WriteString(w, "\n")            // blank line skipped
		io.WriteString(w, `not-json`+"\n") // malformed line skipped
		io.WriteString(w, `{"_time":"2026-08-29T00:00:01Z","_msg":"world","source":"controller"}`+"\n")
	}))
	defer srv.Close()

	c := New(srv.URL, srv.Client())
	lines, err := c.Query(context.Background(), QueryOptions{Component: "controller", Since: 15 * time.Minute, Grep: "hi", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if gotCT != "application/x-www-form-urlencoded" {
		t.Errorf("content-type = %q", gotCT)
	}
	if want := `{source="controller"} _time:15m "hi"`; gotQuery != want {
		t.Errorf("server saw query %q, want %q", gotQuery, want)
	}
	if gotLimit != "100" {
		t.Errorf("server saw limit %q, want 100", gotLimit)
	}
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2 (blank + malformed skipped)", len(lines))
	}
	if lines[0].Msg != "hello" || lines[0].Source != "controller" {
		t.Errorf("line0 = %+v", lines[0])
	}
	if lines[0].Raw["level"] != "INFO" {
		t.Errorf("raw record not preserved: %+v", lines[0].Raw)
	}
}

func TestQueryCapsResultCount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Return more lines than the requested limit; the client must cap.
		for i := 0; i < 10; i++ {
			io.WriteString(w, `{"_msg":"line","source":"controller"}`+"\n")
		}
	}))
	defer srv.Close()

	c := New(srv.URL, srv.Client())
	lines, err := c.Query(context.Background(), QueryOptions{Component: "controller", Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want cap of 3", len(lines))
	}
}

func TestQueryDisallowedComponent(t *testing.T) {
	c := New("http://unused", http.DefaultClient)
	if _, err := c.Query(context.Background(), QueryOptions{Component: "etcd"}); err == nil {
		t.Fatal("expected error querying disallowed component")
	}
}

func TestQueryNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := New(srv.URL, srv.Client())
	if _, err := c.Query(context.Background(), QueryOptions{Component: "controller"}); err == nil {
		t.Fatal("expected error on 500")
	}
}

func TestQueryNoEndpoint(t *testing.T) {
	c := New("", http.DefaultClient)
	if _, err := c.Query(context.Background(), QueryOptions{Component: "controller"}); err == nil {
		t.Fatal("expected error with empty endpoint")
	}
}

// parseForm is a tiny x-www-form-urlencoded parser for the test assertion (avoids
// pulling net/url just to re-decode what the client encoded).
func parseForm(body string) (map[string]string, error) {
	out := map[string]string{}
	for _, pair := range strings.Split(body, "&") {
		kv := strings.SplitN(pair, "=", 2)
		if len(kv) != 2 {
			continue
		}
		k, err := url.QueryUnescape(kv[0])
		if err != nil {
			return nil, err
		}
		v, err := url.QueryUnescape(kv[1])
		if err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, nil
}

// Agent-facing reads are pinned to the default tenant, so records written to
// another tenant (the k3s API audit log, gd-ZrPUxj8wyU) are never returned.
func TestQuery_PinsDefaultTenant(t *testing.T) {
	var acct, proj string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		acct, proj = r.Header.Get("AccountID"), r.Header.Get("ProjectID")
	}))
	defer srv.Close()
	c := New(srv.URL, srv.Client())
	if _, err := c.Query(context.Background(), QueryOptions{Component: Components()[0]}); err != nil {
		t.Fatalf("Query: %v", err)
	}
	if acct != "0" || proj != "0" {
		t.Errorf("tenant headers = %q/%q, want 0/0", acct, proj)
	}
}
