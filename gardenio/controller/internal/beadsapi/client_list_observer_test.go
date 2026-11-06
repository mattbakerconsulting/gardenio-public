package beadsapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSetOnBeadList_TimesListsOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/beads" {
			_ = json.NewEncoder(w).Encode(listBeadsResponse{Total: 0})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "kd-1"})
	}))
	defer srv.Close()
	c := newTestClient(srv)
	var calls int
	c.SetOnBeadList(func(_ context.Context, took time.Duration, err error) {
		calls++
		if err != nil || took <= 0 {
			t.Errorf("observer got took=%v err=%v", took, err)
		}
	})
	if _, err := c.ListBeadsFiltered(context.Background(), ListBeadsQuery{Types: []string{"task"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetBead(context.Background(), "kd-1"); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("observer calls = %d, want 1 (the list, not the show)", calls)
	}
}

func TestIsBeadListCall(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		want         bool
	}{
		{"GET", "/v1/beads", true},
		{"GET", "/v1/beads?type=task", true},
		{"POST", "/v1/beads", false},
		{"GET", "/v1/beads/kd-1", false},
		{"GET", "/v1/beads-batch", false},
	} {
		if got := isBeadListCall(tc.method, tc.path); got != tc.want {
			t.Errorf("isBeadListCall(%s %s) = %v, want %v", tc.method, tc.path, got, tc.want)
		}
	}
}
