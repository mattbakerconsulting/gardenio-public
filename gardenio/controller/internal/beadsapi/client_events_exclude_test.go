package beadsapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestListGlobalEvents_SendsExcludeTypes(t *testing.T) {
	var got string
	var present bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query().Get("exclude_types")
		_, present = r.URL.Query()["exclude_types"]
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"events":[],"has_more":false,"next_cursor":""}`))
	}))
	defer srv.Close()
	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}

	if _, err := c.ListGlobalEvents(context.Background(), ListGlobalEventsRequest{ExcludeTypes: []string{"agent", "trigger"}}); err != nil {
		t.Fatal(err)
	}
	if got != "agent,trigger" {
		t.Errorf("exclude_types = %q, want agent,trigger", got)
	}

	if _, err := c.ListGlobalEvents(context.Background(), ListGlobalEventsRequest{}); err != nil {
		t.Fatal(err)
	}
	if present {
		t.Error("exclude_types must be omitted when ExcludeTypes is empty")
	}
}
