package beadsapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestListAccountBeads(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify query params.
		if got := r.URL.Query().Get("type"); got != "account" {
			t.Errorf("expected type=account, got %q", got)
		}

		resp := listBeadsResponse{
			Beads: []beadJSON{
				{
					ID:    "acct-1",
					Title: "claude-max-litellm",
					Type:  "account",
					Fields: json.RawMessage(`{
						"provider":"anthropic",
						"base_url":"http://litellm.devops:4000",
						"proxy_url":"http://claudemore.devops:8100",
						"credential":"coopmux:claude-max",
						"model":"anthropic-claude",
						"max_budget":"100",
						"fallback":"claude-api-key",
						"litellm_key":"sk-fake-key-12345"
					}`),
				},
				{
					ID:    "acct-2",
					Title: "claude-api-key",
					Type:  "account",
					Fields: json.RawMessage(`{
						"provider":"anthropic",
						"credential":"secret:claude-api-key"
					}`),
				},
			},
			Total: 2,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	accounts, err := c.ListAccountBeads(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(accounts) != 2 {
		t.Fatalf("expected 2 accounts, got %d", len(accounts))
	}

	acct, ok := accounts["claude-max-litellm"]
	if !ok {
		t.Fatal("expected account 'claude-max-litellm' in map")
	}
	if acct.Provider != "anthropic" {
		t.Errorf("provider: got %q, want %q", acct.Provider, "anthropic")
	}
	if acct.BaseURL != "http://litellm.devops:4000" {
		t.Errorf("base_url: got %q, want %q", acct.BaseURL, "http://litellm.devops:4000")
	}
	if acct.ProxyURL != "http://claudemore.devops:8100" {
		t.Errorf("proxy_url: got %q, want %q", acct.ProxyURL, "http://claudemore.devops:8100")
	}
	if acct.Credential != "coopmux:claude-max" {
		t.Errorf("credential: got %q, want %q", acct.Credential, "coopmux:claude-max")
	}
	if acct.Model != "anthropic-claude" {
		t.Errorf("model: got %q, want %q", acct.Model, "anthropic-claude")
	}
	if acct.MaxBudget != "100" {
		t.Errorf("max_budget: got %q, want %q", acct.MaxBudget, "100")
	}
	if acct.Fallback != "claude-api-key" {
		t.Errorf("fallback: got %q, want %q", acct.Fallback, "claude-api-key")
	}
	if acct.LiteLLMKey != "sk-fake-key-12345" {
		t.Errorf("litellm_key: got %q, want %q", acct.LiteLLMKey, "sk-fake-key-12345")
	}

	// Minimal account (no optional fields).
	acct2, ok := accounts["claude-api-key"]
	if !ok {
		t.Fatal("expected account 'claude-api-key' in map")
	}
	if acct2.BaseURL != "" {
		t.Errorf("expected empty base_url for minimal account, got %q", acct2.BaseURL)
	}
	if acct2.ProxyURL != "" {
		t.Errorf("expected empty proxy_url for minimal account, got %q", acct2.ProxyURL)
	}
	if acct2.LiteLLMKey != "" {
		t.Errorf("expected empty litellm_key for minimal account, got %q", acct2.LiteLLMKey)
	}
}

func TestListAccountBeads_SkipsEmptyTitle(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := listBeadsResponse{
			Beads: []beadJSON{
				{
					ID:     "acct-empty",
					Title:  "",
					Type:   "account",
					Fields: json.RawMessage(`{"provider":"anthropic","credential":"env:ANTHROPIC_API_KEY"}`),
				},
			},
			Total: 1,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	accounts, err := c.ListAccountBeads(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(accounts) != 0 {
		t.Errorf("expected 0 accounts (empty title skipped), got %d", len(accounts))
	}
}

func TestListAccountBeadsByProject(t *testing.T) {
	beads := []beadJSON{
		// Matches project:gardenio, status=active (explicit).
		{
			ID:     "acct-1",
			Title:  "zeta-account",
			Type:   "account",
			Labels: []string{"project:gardenio"},
			Fields: json.RawMessage(`{"provider":"anthropic","credential":"env:A","status":"active"}`),
		},
		// Matches project:gardenio AND project:monorepo, empty status = active.
		{
			ID:     "acct-2",
			Title:  "alpha-account",
			Type:   "account",
			Labels: []string{"project:gardenio", "project:monorepo"},
			Fields: json.RawMessage(`{"provider":"anthropic","credential":"env:B"}`),
		},
		// Labeled for project:gardenio but exhausted — must be filtered out.
		{
			ID:     "acct-3",
			Title:  "burnt-account",
			Type:   "account",
			Labels: []string{"project:gardenio"},
			Fields: json.RawMessage(`{"provider":"anthropic","credential":"env:C","status":"exhausted"}`),
		},
		// Different project — must not appear.
		{
			ID:     "acct-4",
			Title:  "other-account",
			Type:   "account",
			Labels: []string{"project:other"},
			Fields: json.RawMessage(`{"provider":"anthropic","credential":"env:D"}`),
		},
		// No project labels — global, not returned.
		{
			ID:     "acct-5",
			Title:  "global-account",
			Type:   "account",
			Fields: json.RawMessage(`{"provider":"anthropic","credential":"env:E"}`),
		},
		// Empty title — skipped.
		{
			ID:     "acct-empty",
			Title:  "",
			Type:   "account",
			Labels: []string{"project:gardenio"},
			Fields: json.RawMessage(`{"provider":"anthropic"}`),
		},
		// Malformed fields — ParseFieldsJSON returns empty map, so status=""→active.
		// (Must still be valid JSON at the envelope level so json.Encoder can marshal it.)
		{
			ID:     "acct-malformed",
			Title:  "malformed-account",
			Type:   "account",
			Labels: []string{"project:gardenio"},
			Fields: json.RawMessage(`[]`),
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("type"); got != "account" {
			t.Errorf("expected type=account, got %q", got)
		}
		_ = json.NewEncoder(w).Encode(listBeadsResponse{Beads: beads, Total: len(beads)})
	}))
	defer srv.Close()
	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}

	t.Run("multi match sorted alphabetically", func(t *testing.T) {
		got, err := c.ListAccountBeadsByProject(context.Background(), "gardenio")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// Expected: alpha-account, malformed-account, zeta-account
		// (burnt-account filtered by status, empty-title skipped).
		wantNames := []string{"alpha-account", "malformed-account", "zeta-account"}
		if len(got) != len(wantNames) {
			t.Fatalf("expected %d accounts, got %d: %+v", len(wantNames), len(got), got)
		}
		for i, name := range wantNames {
			if got[i].Name != name {
				t.Errorf("pos %d: got %q, want %q", i, got[i].Name, name)
			}
		}
	})

	t.Run("single match", func(t *testing.T) {
		got, err := c.ListAccountBeadsByProject(context.Background(), "monorepo")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 1 || got[0].Name != "alpha-account" {
			t.Errorf("expected [alpha-account], got %+v", got)
		}
	})

	t.Run("no match", func(t *testing.T) {
		got, err := c.ListAccountBeadsByProject(context.Background(), "nonexistent")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("expected 0 accounts, got %d: %+v", len(got), got)
		}
	})

	t.Run("empty project rejected", func(t *testing.T) {
		_, err := c.ListAccountBeadsByProject(context.Background(), "")
		if err == nil {
			t.Fatal("expected error for empty project, got nil")
		}
	})
}

func TestListAccountBeadsByProject_PrimaryMarkerOrdersFirst(t *testing.T) {
	beads := []beadJSON{
		{
			ID:     "acct-alpha",
			Title:  "alpha",
			Type:   "account",
			Labels: []string{"project:gardenio"},
			Fields: json.RawMessage(`{"provider":"anthropic","credential":"env:A"}`),
		},
		{
			ID:     "acct-zebra",
			Title:  "zebra",
			Type:   "account",
			Labels: []string{"project:gardenio"},
			Fields: json.RawMessage(`{"provider":"anthropic","credential":"env:Z","primary":"true"}`),
		},
		{
			ID:     "acct-beta",
			Title:  "beta",
			Type:   "account",
			Labels: []string{"project:gardenio"},
			Fields: json.RawMessage(`{"provider":"anthropic","credential":"env:B"}`),
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(listBeadsResponse{Beads: beads, Total: len(beads)})
	}))
	defer srv.Close()
	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}

	got, err := c.ListAccountBeadsByProject(context.Background(), "gardenio")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Expected order: zebra (primary marker), alpha, beta.
	wantNames := []string{"zebra", "alpha", "beta"}
	if len(got) != len(wantNames) {
		t.Fatalf("expected %d accounts, got %d: %+v", len(wantNames), len(got), got)
	}
	for i, name := range wantNames {
		if got[i].Name != name {
			t.Errorf("pos %d: got %q, want %q", i, got[i].Name, name)
		}
	}
}

func TestListProjectBeads_ParsesAccountField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := listBeadsResponse{
			Beads: []beadJSON{
				{
					ID:    "proj-1",
					Title: "my-project",
					Type:  "project",
					Fields: json.RawMessage(`{
						"prefix":"kd",
						"git_url":"https://github.com/org/repo.git",
						"account":"claude-max-litellm"
					}`),
				},
			},
			Total: 1,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	projects, err := c.ListProjectBeads(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	p, ok := projects["my-project"]
	if !ok {
		t.Fatal("expected project 'my-project' in map")
	}
	if p.Account != "claude-max-litellm" {
		t.Errorf("account: got %q, want %q", p.Account, "claude-max-litellm")
	}
}

func TestListProjectBeads_ParsesGithubAppFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(listBeadsResponse{
			Beads: []beadJSON{
				{ID: "proj-1", Title: "tomato-patch", Type: "project", Fields: json.RawMessage(`{
					"land_kind":"tenant","github_app":" lands ","github_installation_id":"9001","nats_epoch":"3"}`)},
				{ID: "proj-2", Title: "bad-pin", Type: "project", Fields: json.RawMessage(`{
					"github_app":"lands","github_installation_id":"nine"}`)},
			},
			Total: 2,
		})
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	projects, err := c.ListProjectBeads(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p := projects["tomato-patch"]; p.GithubApp != "lands" || p.GithubInstallationID != 9001 || p.LandKind != "tenant" || p.NatsEpoch != "3" {
		t.Errorf("tomato-patch = app %q installation %d kind %q nats_epoch %q", p.GithubApp, p.GithubInstallationID, p.LandKind, p.NatsEpoch)
	}
	if p := projects["bad-pin"]; p.GithubInstallationID != 0 {
		t.Errorf("a non-numeric installation id must parse as unset, got %d", p.GithubInstallationID)
	}
}
