package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestAttributionAttrs(t *testing.T) {
	get := func(target string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, target, nil)
		r.Header.Set("X-Actor", "agent-1")
		r.Header.Set("User-Agent", "gb/1.0")
		return r
	}
	vals := func(r *http.Request) map[string]string {
		m := map[string]string{}
		for _, a := range requestAttributionAttrs(r) {
			m[a.Key] = a.Value.String()
		}
		return m
	}

	m := vals(get("/v1/beads?status=open&limit=50"))
	if m["query"] != "status=open&limit=50" || m["actor"] != "agent-1" || m["user_agent"] != "gb/1.0" {
		t.Fatalf("list read attrs = %v", m)
	}
	if _, ok := vals(get("/v1/beads/gd-1?x=1"))["query"]; ok {
		t.Fatalf("query must only be logged for the list route")
	}
	long := vals(get("/v1/beads?q=" + strings.Repeat("a", 1000)))["query"]
	if len(long) != attrQueryMax {
		t.Fatalf("query len = %d, want capped at %d", len(long), attrQueryMax)
	}
	if got := requestAttributionAttrs(httptest.NewRequest(http.MethodGet, "/v1/health", nil)); len(got) != 1 {
		// httptest sets no User-Agent; only absent attrs expected
		if len(got) != 0 {
			t.Fatalf("bare request attrs = %v", got)
		}
	}
}
