package beadsapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWhoami_ParsesResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/v1/whoami" {
			t.Errorf("expected path /v1/whoami, got %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"operator_id": "alice@example.com",
			"source":      "auth",
		})
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	resp, err := c.Whoami(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.OperatorID != "alice@example.com" {
		t.Errorf("OperatorID = %q, want alice@example.com", resp.OperatorID)
	}
	if resp.Source != "auth" {
		t.Errorf("Source = %q, want auth", resp.Source)
	}
}

func TestWhoami_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	if _, err := c.Whoami(context.Background()); err == nil {
		t.Fatal("expected error on 500")
	}
}

func TestLinkOperatorAlias_SendsRequestAndParsesResponse(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody LinkOperatorAliasRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		_ = json.NewEncoder(w).Encode(LinkOperatorAliasResponse{
			OperatorID:    42,
			Kind:          "slack",
			Subject:       "U0OPERATOR1",
			Merged:        true,
			MergedFromID:  17,
			RowsRepointed: 9,
		})
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	resp, err := c.LinkOperatorAlias(context.Background(), LinkOperatorAliasRequest{
		Kind:    "slack",
		Subject: "U0OPERATOR1",
		Source:  "identity-link",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("expected POST, got %s", gotMethod)
	}
	if gotPath != "/v1/operators/me/aliases" {
		t.Errorf("expected path /v1/operators/me/aliases, got %s", gotPath)
	}
	if gotBody.Kind != "slack" || gotBody.Subject != "U0OPERATOR1" || gotBody.Source != "identity-link" {
		t.Errorf("unexpected request body: %+v", gotBody)
	}

	if resp.OperatorID != 42 {
		t.Errorf("OperatorID = %d, want 42", resp.OperatorID)
	}
	if !resp.Merged || resp.MergedFromID != 17 || resp.RowsRepointed != 9 {
		t.Errorf("unexpected merge fields: %+v", resp)
	}
}

func TestLinkOperatorAlias_AlreadyLinked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(LinkOperatorAliasResponse{
			OperatorID:    42,
			Kind:          "slack",
			Subject:       "U1",
			AlreadyLinked: true,
		})
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	resp, err := c.LinkOperatorAlias(context.Background(), LinkOperatorAliasRequest{
		Kind: "slack", Subject: "U1", Source: "identity-link",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp.AlreadyLinked {
		t.Errorf("AlreadyLinked = false, want true")
	}
	if resp.Merged {
		t.Errorf("Merged = true, want false on idempotent replay")
	}
}

func TestLinkOperatorAlias_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"oidc required"}`))
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	_, err := c.LinkOperatorAlias(context.Background(), LinkOperatorAliasRequest{
		Kind: "slack", Subject: "U1", Source: "identity-link",
	})
	if err == nil {
		t.Fatal("expected error on 403")
	}
}

// MeAs sends exactly the bearer it is given, never the client's own token,
// and refuses to send nothing.
func TestMeAs_SendsOnlyTheGivenBearer(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]any{"operator_id": "machine:gardenio-slack-bridge", "source": "token"})
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client(), token: "client-own-token"}
	resp, err := c.MeAs(context.Background(), "caller-token")
	if err != nil {
		t.Fatalf("MeAs: %v", err)
	}
	if gotPath != "/v1/me" || gotAuth != "Bearer caller-token" {
		t.Errorf("sent %s with Authorization %q, want /v1/me with the caller's bearer", gotPath, gotAuth)
	}
	if resp.OperatorID != "machine:gardenio-slack-bridge" {
		t.Errorf("OperatorID = %q", resp.OperatorID)
	}
	if _, err := c.MeAs(context.Background(), ""); err == nil {
		t.Error("MeAs with no bearer must fail without sending")
	}
}
