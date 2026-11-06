package beadsapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"testing"
)

// TestListAgentBeadsLean_SendsProjection pins gd-zXdZq4Traf: the lean agent
// listing asks the daemon to omit description and the LeanAgentOmitFields
// keys, keeps every other filter of ListAgentBeads, and plain ListAgentBeads
// omits only description (its reconciler caller needs fields.prompt).
func TestListAgentBeadsLean_SendsProjection(t *testing.T) {
	var got url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		_ = json.NewEncoder(w).Encode(listBeadsResponse{Beads: []beadJSON{{
			ID: "gd-a", Type: "agent", Status: "open",
			Notes:  "pod_name: p-0",
			Fields: json.RawMessage(`{"agent":"a","project":"gardenio","role":"crew"}`),
		}}, Total: 1})
	}))
	defer srv.Close()
	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}

	beads, err := LeanAgentLister{c}.ListAgentBeads(context.Background())
	if err != nil {
		t.Fatalf("lean list: %v", err)
	}
	if got.Get("omit") != "description" || got.Get("omit_fields") != "prompt,wrapup" {
		t.Errorf("lean projection not sent: %v", got)
	}
	if got.Get("type") != "agent" || got.Get("limit") == "" {
		t.Errorf("lean list must keep ListAgentBeads' filters: %v", got)
	}
	if len(beads) != 1 || beads[0].Metadata["pod_name"] != "p-0" {
		t.Errorf("notes must still merge into metadata: %+v", beads)
	}

	if _, err := c.ListAgentBeads(context.Background()); err != nil {
		t.Fatalf("full list: %v", err)
	}
	if got.Get("omit") != "description" || got.Has("omit_fields") {
		t.Errorf("ListAgentBeads must omit only description (AgentBead has no field for it): %v", got)
	}
}

// TestListBeadsFiltered_Omit pins the ListBeadsQuery.Omit / OmitFields
// encoding, and that a zero query sends neither param.
func TestListBeadsFiltered_Omit(t *testing.T) {
	var got url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		_ = json.NewEncoder(w).Encode(listBeadsResponse{})
	}))
	defer srv.Close()
	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}

	if _, err := c.ListBeadsFiltered(context.Background(), ListBeadsQuery{
		Types: []string{"trigger"}, Omit: []string{"description", "notes"}, OmitFields: []string{"prompt"},
	}); err != nil {
		t.Fatal(err)
	}
	if got.Get("omit") != "description,notes" || got.Get("omit_fields") != "prompt" {
		t.Errorf("omit params: %v", got)
	}

	if _, err := c.ListBeadsFiltered(context.Background(), ListBeadsQuery{Types: []string{"trigger"}}); err != nil {
		t.Fatal(err)
	}
	if got.Has("omit") || got.Has("omit_fields") {
		t.Errorf("default query must send no projection: %v", got)
	}
}

// TestListBeadsQuery_OnlyFields pins the gd-6PXLuR3juD projection: OnlyFields
// sends fields_only plus the omit/omit_fields fallback for an older daemon,
// never omits a key it was asked to keep, and leaves q itself untouched.
func TestListBeadsQuery_OnlyFields(t *testing.T) {
	var got url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		_ = json.NewEncoder(w).Encode(listBeadsResponse{})
	}))
	defer srv.Close()
	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}

	base := ListBeadsQuery{Types: []string{"agent"}}
	if _, err := c.ListBeadsFiltered(context.Background(), base.OnlyFields("total_cost_usd", "agent")); err != nil {
		t.Fatal(err)
	}
	if got.Get("fields_only") != "total_cost_usd,agent" {
		t.Errorf("fields_only = %q", got.Get("fields_only"))
	}
	if got.Get("omit") != "description,notes" || got.Get("omit_fields") != "prompt,wrapup,slowest_calls,gardener,captain" {
		t.Errorf("fallback projection: %v", got)
	}
	if base.FieldsOnly != nil || base.Omit != nil {
		t.Errorf("OnlyFields must not mutate its receiver: %+v", base)
	}

	q := base.OnlyFields("gardener", "prompt")
	if slices.Contains(q.OmitFields, "gardener") || slices.Contains(q.OmitFields, "prompt") {
		t.Errorf("a kept key must not be in the fallback omit list: %v", q.OmitFields)
	}
}

// TestListBeadsQuery_NoFields pins the zero-key projection: one absent key in
// fields_only (a bare fields_only= is ignored by the daemon) plus the same
// fallback omit as OnlyFields.
func TestListBeadsQuery_NoFields(t *testing.T) {
	q := ListBeadsQuery{Types: []string{"task"}}.NoFields()
	if !slices.Equal(q.FieldsOnly, []string{noFieldsKey}) {
		t.Errorf("FieldsOnly = %v", q.FieldsOnly)
	}
	if !slices.Equal(q.Omit, []string{"description", "notes"}) || !slices.Contains(q.OmitFields, "prompt") {
		t.Errorf("fallback projection: omit=%v omit_fields=%v", q.Omit, q.OmitFields)
	}
}
