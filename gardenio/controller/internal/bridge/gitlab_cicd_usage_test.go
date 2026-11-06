package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestGetCIMinutesUsage_ParsesGraphQL confirms GetCIMinutesUsage POSTs the
// query with Bearer auth to /api/graphql and decodes the nested month/project
// usage shape, sorted newest-first and capped at lookback.
func TestGetCIMinutesUsage_ParsesGraphQL(t *testing.T) {
	var gotAuth, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":{"ciMinutesUsage":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[
			{"month":"June 2026","monthIso8601":"2026-06-01","minutes":300,"projects":{"pageInfo":{"hasNextPage":false},"nodes":[]}},
			{"month":"July 2026","monthIso8601":"2026-07-01","minutes":420,"projects":{"pageInfo":{"hasNextPage":false},"nodes":[
				{"minutes":250,"project":{"name":"gardenio","fullPath":"Acme/Platform/gardenio"}},
				{"minutes":170,"project":{"name":"helm","fullPath":"Acme/Platform/acme-helm-chart"}}
			]}}
		]}}}`)
	}))
	defer srv.Close()

	c := NewGitLabClient(GitLabClientConfig{BaseURL: srv.URL, Token: "tok", Logger: slog.Default()})
	months, err := c.GetCIMinutesUsage(context.Background(), "gid://gitlab/Group/1", 12)
	if err != nil {
		t.Fatalf("GetCIMinutesUsage: %v", err)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer tok")
	}
	if gotPath != "/api/graphql" {
		t.Errorf("path = %q, want /api/graphql", gotPath)
	}
	if !strings.Contains(gotBody, "ciMinutesUsage") {
		t.Errorf("body missing query: %q", gotBody)
	}
	if len(months) != 2 {
		t.Fatalf("months = %d, want 2", len(months))
	}
	// Sorted newest-first: July before June.
	if months[0].MonthISO8601 != "2026-07-01" {
		t.Errorf("months[0] = %q, want 2026-07-01 (newest first)", months[0].MonthISO8601)
	}
	if months[0].Minutes != 420 {
		t.Errorf("July minutes = %v, want 420", months[0].Minutes)
	}
	if len(months[0].Projects) != 2 {
		t.Fatalf("July projects = %d, want 2", len(months[0].Projects))
	}
	if months[0].Projects[0].FullPath != "Acme/Platform/gardenio" || months[0].Projects[0].Minutes != 250 {
		t.Errorf("July project[0] = %+v, want gardenio/250", months[0].Projects[0])
	}
}

// TestGetCIMinutesUsage_Lookback caps the result to lookbackMonths.
func TestGetCIMinutesUsage_Lookback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"data":{"ciMinutesUsage":{"pageInfo":{"hasNextPage":false},"nodes":[
			{"monthIso8601":"2026-07-01","minutes":1},
			{"monthIso8601":"2026-06-01","minutes":2},
			{"monthIso8601":"2026-05-01","minutes":3}
		]}}}`)
	}))
	defer srv.Close()

	c := NewGitLabClient(GitLabClientConfig{BaseURL: srv.URL, Token: "t"})
	months, err := c.GetCIMinutesUsage(context.Background(), "gid://x", 2)
	if err != nil {
		t.Fatalf("GetCIMinutesUsage: %v", err)
	}
	if len(months) != 2 {
		t.Fatalf("months = %d, want 2 (lookback cap)", len(months))
	}
	if months[0].MonthISO8601 != "2026-07-01" || months[1].MonthISO8601 != "2026-06-01" {
		t.Errorf("wrong two newest months: %q, %q", months[0].MonthISO8601, months[1].MonthISO8601)
	}
}

// TestGetCIMinutesUsage_GraphQLError surfaces a GraphQL-level error (the shape
// the bridge GAT returns until the group-owner token lands).
func TestGetCIMinutesUsage_GraphQLError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"errors":[{"message":"The resource that you are attempting to access does not exist or you don't have permission"}]}`)
	}))
	defer srv.Close()

	c := NewGitLabClient(GitLabClientConfig{BaseURL: srv.URL, Token: "t"})
	_, err := c.GetCIMinutesUsage(context.Background(), "gid://x", 12)
	if err == nil {
		t.Fatal("expected error for GraphQL errors payload")
	}
	if !strings.Contains(err.Error(), "permission") {
		t.Errorf("error should carry the GraphQL message, got %q", err.Error())
	}
}

// TestGetCIMinutesUsage_FollowsCursor walks a two-page cursor response.
func TestGetCIMinutesUsage_FollowsCursor(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req struct {
			Variables map[string]any `json:"variables"`
		}
		_ = json.Unmarshal(b, &req)
		calls++
		if req.Variables["after"] == nil {
			fmt.Fprint(w, `{"data":{"ciMinutesUsage":{"pageInfo":{"hasNextPage":true,"endCursor":"CUR2"},"nodes":[{"monthIso8601":"2026-07-01","minutes":10}]}}}`)
			return
		}
		if req.Variables["after"] != "CUR2" {
			t.Errorf("second page after = %v, want CUR2", req.Variables["after"])
		}
		fmt.Fprint(w, `{"data":{"ciMinutesUsage":{"pageInfo":{"hasNextPage":false},"nodes":[{"monthIso8601":"2026-06-01","minutes":20}]}}}`)
	}))
	defer srv.Close()

	c := NewGitLabClient(GitLabClientConfig{BaseURL: srv.URL, Token: "t"})
	months, err := c.GetCIMinutesUsage(context.Background(), "gid://x", 12)
	if err != nil {
		t.Fatalf("GetCIMinutesUsage: %v", err)
	}
	if calls != 2 {
		t.Errorf("graphql calls = %d, want 2 (cursor walk)", calls)
	}
	if len(months) != 2 {
		t.Fatalf("months = %d, want 2", len(months))
	}
}

// TestGetGroupComputeQuota_ParsesREST confirms the group quota fields decode.
func TestGetGroupComputeQuota_ParsesREST(t *testing.T) {
	var gotToken, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("PRIVATE-TOKEN")
		gotPath = r.URL.Path
		fmt.Fprint(w, `{"shared_runners_minutes_limit":1000,"extra_shared_runners_minutes_limit":500}`)
	}))
	defer srv.Close()

	c := NewGitLabClient(GitLabClientConfig{BaseURL: srv.URL, Token: "tok"})
	q, err := c.GetGroupComputeQuota(context.Background(), "Acme")
	if err != nil {
		t.Fatalf("GetGroupComputeQuota: %v", err)
	}
	if gotToken != "tok" {
		t.Errorf("PRIVATE-TOKEN = %q, want tok", gotToken)
	}
	if gotPath != "/api/v4/groups/Acme" {
		t.Errorf("path = %q, want /api/v4/groups/Acme", gotPath)
	}
	if q.Base == nil || *q.Base != 1000 {
		t.Errorf("base = %v, want 1000", q.Base)
	}
	if q.Additional == nil || *q.Additional != 500 {
		t.Errorf("additional = %v, want 500", q.Additional)
	}
}

// TestGetGroupComputeQuota_UnlimitedNulls confirms null quota fields decode to
// nil pointers (the "unlimited" case the collector treats as no finite quota).
func TestGetGroupComputeQuota_UnlimitedNulls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"shared_runners_minutes_limit":null,"extra_shared_runners_minutes_limit":null}`)
	}))
	defer srv.Close()

	c := NewGitLabClient(GitLabClientConfig{BaseURL: srv.URL, Token: "t"})
	q, err := c.GetGroupComputeQuota(context.Background(), "Acme")
	if err != nil {
		t.Fatalf("GetGroupComputeQuota: %v", err)
	}
	if q.Base != nil || q.Additional != nil {
		t.Errorf("expected nil quota pointers for null limits, got base=%v additional=%v", q.Base, q.Additional)
	}
}
