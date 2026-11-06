package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// listFromSite is the caller frame beadListSite must name.
func listFromSite(t *testing.T, c *beadsapi.Client) {
	t.Helper()
	if _, err := c.ListBeadsFiltered(context.Background(), beadsapi.ListBeadsQuery{Types: []string{"task"}}); err != nil {
		t.Fatal(err)
	}
}

func TestBeadListSite_NamesTheCallingFunction(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})
	}))
	defer srv.Close()
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	var site string
	c.SetOnBeadList(func(context.Context, time.Duration, error) { site = beadListSite() })
	listFromSite(t, c)
	if site != "listFromSite" {
		t.Fatalf("site = %q, want listFromSite", site)
	}
}

func TestBeadListOutcome_NamesTheErrorClass(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{nil, "ok"},
		{fmt.Errorf("list: %w", context.Canceled), "canceled"},
		{context.DeadlineExceeded, "timeout"},
		{beadsapi.ErrUserBearerRequiredForRead, "no_bearer"},
		{fmt.Errorf("commons: %w", &beadsapi.APIError{StatusCode: http.StatusTooManyRequests}), "429"},
		{&beadsapi.APIError{StatusCode: http.StatusForbidden}, "403"},
		{&beadsapi.APIError{StatusCode: http.StatusBadRequest}, "4xx"},
		{&beadsapi.APIError{StatusCode: http.StatusServiceUnavailable}, "5xx"},
		{errors.New("dial tcp: refused"), "error"},
	} {
		if got := beadListOutcome(tc.err); got != tc.want {
			t.Errorf("beadListOutcome(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}
