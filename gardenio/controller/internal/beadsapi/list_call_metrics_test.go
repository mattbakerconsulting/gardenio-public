package beadsapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"gardenio/controller/internal/beadsapi"
)

// listRequests reads gardenio_beadsapi_list_requests_total for one label set.
func listRequests(t *testing.T, site, api, noTotal string) float64 {
	t.Helper()
	reg := prometheus.NewRegistry()
	reg.MustRegister(beadsapi.ListRequestsCollector())
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			got := map[string]string{}
			for _, l := range m.GetLabel() {
				got[l.GetName()] = l.GetValue()
			}
			if got["site"] == site && got["api"] == api && got["no_total"] == noTotal {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// TestListRequestsCounted_BySiteAndAPI pins gd-jd0BGkgmV5: each GET /v1/beads
// list is counted under the function that called beadsapi (site), the
// beadsapi method it entered (api) and its no_total flag; a single-bead GET
// is not a list.
func TestListRequestsCounted_BySiteAndAPI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/beads" {
			_, _ = w.Write([]byte(`{"beads":[],"total":0}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "gd-a", "type": "task", "status": "open"})
	}))
	defer srv.Close()
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	ctx := context.Background()
	const site = "internal/beadsapi_test.TestListRequestsCounted_BySiteAndAPI"

	before := listRequests(t, site, "(*Client).ListBeadsFiltered", "false")
	beforeNT := listRequests(t, site, "(*Client).ListBeadsFiltered", "true")
	beforeAssigned := listRequests(t, site, "(*Client).ListAssignedTasks", "false")

	if _, err := c.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{Types: []string{"task"}}); err != nil {
		t.Fatalf("list: %v", err)
	}
	if _, err := c.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{Types: []string{"task"}, NoTotal: true}); err != nil {
		t.Fatalf("list no_total: %v", err)
	}
	if _, err := c.ListAssignedTasks(ctx, "crew-a"); err != nil {
		t.Fatalf("assigned: %v", err)
	}
	if _, err := c.GetBead(ctx, "gd-a"); err != nil {
		t.Fatalf("get: %v", err)
	}

	if d := listRequests(t, site, "(*Client).ListBeadsFiltered", "false") - before; d != 1 {
		t.Errorf("ListBeadsFiltered without no_total counted %v times, want 1", d)
	}
	if d := listRequests(t, site, "(*Client).ListBeadsFiltered", "true") - beforeNT; d != 1 {
		t.Errorf("ListBeadsFiltered with no_total counted %v times, want 1", d)
	}
	if d := listRequests(t, site, "(*Client).ListAssignedTasks", "false") - beforeAssigned; d != 1 {
		t.Errorf("ListAssignedTasks counted %v times, want 1", d)
	}
	if n := listRequests(t, site, "(*Client).GetBead", "false"); n != 0 {
		t.Errorf("GetBead is not a list, counted %v", n)
	}
}
