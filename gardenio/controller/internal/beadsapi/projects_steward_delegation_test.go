package beadsapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// SetStewardDelegation posts the list to the owner-only endpoint on the
// caller's user bearer, so the daemon judges the land owner, not the
// controller's token.
func TestSetStewardDelegation(t *testing.T) {
	var gotAuth, gotMethod, gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotMethod, gotPath = r.Header.Get("Authorization"), r.Method, r.URL.Path
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		_, _ = w.Write([]byte(`{"land":"acme","operation_types":["github_pr_merge"]}`))
	}))
	defer srv.Close()
	c := newTestClientWithToken(t, srv, "read-only-token")

	ctx := WithUserBearer(context.Background(), "owner-jwt")
	if err := c.SetStewardDelegation(ctx, "acme", []string{"github_pr_merge"}); err != nil {
		t.Fatalf("SetStewardDelegation: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/v1/lands/acme/steward-delegation" {
		t.Fatalf("got %s %s, want POST /v1/lands/acme/steward-delegation", gotMethod, gotPath)
	}
	if gotAuth != "Bearer owner-jwt" {
		t.Fatalf("Authorization = %q, want the user bearer", gotAuth)
	}
	if want := map[string]any{"operation_types": []any{"github_pr_merge"}}; !reflect.DeepEqual(gotBody, want) {
		t.Fatalf("body = %v, want %v", gotBody, want)
	}

	// A revoke sends an empty list, never null.
	if err := c.SetStewardDelegation(ctx, "acme", nil); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if want := map[string]any{"operation_types": []any{}}; !reflect.DeepEqual(gotBody, want) {
		t.Fatalf("revoke body = %v, want %v", gotBody, want)
	}
}

func TestSetStewardDelegation_ReturnsDaemonRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"only the owner of land acme (or a gardenio-admin) may set its steward delegation"}`))
	}))
	defer srv.Close()
	c := newTestClientWithToken(t, srv, "read-only-token")

	err := c.SetStewardDelegation(WithUserBearer(context.Background(), "member-jwt"), "acme", []string{"github_pr_merge"})
	if err == nil || !strings.Contains(err.Error(), "setting steward delegation for acme") {
		t.Fatalf("err = %v, want the wrapped refusal", err)
	}
}
