package coopmux

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNew_EmptyURL(t *testing.T) {
	c := New(Config{URL: ""})
	if c != nil {
		t.Fatal("expected nil client for empty URL")
	}
}

func TestNew_ValidURL(t *testing.T) {
	c := New(Config{URL: "http://localhost:8080", AuthToken: "test-token"})
	if c == nil {
		t.Fatal("expected non-nil client")
	}
	if c.baseURL != "http://localhost:8080" {
		t.Errorf("expected baseURL=http://localhost:8080, got %q", c.baseURL)
	}
	if c.authToken != "test-token" {
		t.Errorf("expected authToken=test-token, got %q", c.authToken)
	}
}

func TestNew_TrailingSlash(t *testing.T) {
	c := New(Config{URL: "http://localhost:8080/"})
	if c == nil {
		t.Fatal("expected non-nil client")
	}
	if c.baseURL != "http://localhost:8080" {
		t.Errorf("expected trailing slash trimmed, got %q", c.baseURL)
	}
}

func TestSyncAccounts_Success(t *testing.T) {
	var registered, credSet []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)

		// Verify auth header.
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("expected auth header, got %q", r.Header.Get("Authorization"))
		}

		switch r.URL.Path {
		case "/api/v1/credentials/new":
			registered = append(registered, body["name"])
			w.WriteHeader(http.StatusOK)
		case "/api/v1/credentials/set":
			credSet = append(credSet, body["account"])
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := New(Config{URL: srv.URL, AuthToken: "test-token"})
	synced, failed, err := c.SyncAccounts(context.Background(), []AccountSync{
		{Name: "acct1", Provider: "claude", Token: "token1"},
		{Name: "acct2", Provider: "claude", Token: "token2"},
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if synced != 2 {
		t.Errorf("expected synced=2, got %d", synced)
	}
	if failed != 0 {
		t.Errorf("expected failed=0, got %d", failed)
	}
	if len(registered) != 2 {
		t.Errorf("expected 2 registrations, got %d", len(registered))
	}
	if len(credSet) != 2 {
		t.Errorf("expected 2 credential sets, got %d", len(credSet))
	}
}

func TestSyncAccounts_EmptyToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("should not make any requests for empty tokens")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(Config{URL: srv.URL})
	synced, failed, err := c.SyncAccounts(context.Background(), []AccountSync{
		{Name: "acct1", Provider: "claude", Token: ""},
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if synced != 0 {
		t.Errorf("expected synced=0, got %d", synced)
	}
	if failed != 1 {
		t.Errorf("expected failed=1, got %d", failed)
	}
}

func TestSyncAccounts_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal error"))
	}))
	defer srv.Close()

	c := New(Config{URL: srv.URL})
	synced, failed, err := c.SyncAccounts(context.Background(), []AccountSync{
		{Name: "acct1", Provider: "claude", Token: "token1"},
	})

	if err == nil {
		t.Fatal("expected error")
	}
	if synced != 0 {
		t.Errorf("expected synced=0, got %d", synced)
	}
	if failed != 1 {
		t.Errorf("expected failed=1, got %d", failed)
	}
}

func TestSyncAccounts_DefaultProvider(t *testing.T) {
	var receivedProvider string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/credentials/new" {
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			receivedProvider = body["provider"]
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(Config{URL: srv.URL})
	c.SyncAccounts(context.Background(), []AccountSync{
		{Name: "acct1", Provider: "", Token: "token1"},
	})

	if receivedProvider != "claude" {
		t.Errorf("expected default provider=claude, got %q", receivedProvider)
	}
}

func TestDistribute_Success(t *testing.T) {
	var receivedAccount string
	var receivedSwitch bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/credentials/distribute" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("expected auth header, got %q", r.Header.Get("Authorization"))
		}

		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		receivedAccount, _ = body["account"].(string)
		receivedSwitch, _ = body["switch"].(bool)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(Config{URL: srv.URL, AuthToken: "test-token"})
	err := c.Distribute(context.Background(), "fallback-account")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if receivedAccount != "fallback-account" {
		t.Errorf("expected account=fallback-account, got %q", receivedAccount)
	}
	if !receivedSwitch {
		t.Error("expected switch=true")
	}
}

func TestDistribute_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal error"))
	}))
	defer srv.Close()

	c := New(Config{URL: srv.URL})
	err := c.Distribute(context.Background(), "some-account")
	if err == nil {
		t.Fatal("expected error for server error response")
	}
}

func TestDistribute_Unreachable(t *testing.T) {
	c := New(Config{URL: "http://localhost:1"}) // nothing listening
	err := c.Distribute(context.Background(), "some-account")
	if err == nil {
		t.Fatal("expected error for unreachable server")
	}
}

func TestSyncAccounts_Unreachable(t *testing.T) {
	c := New(Config{URL: "http://localhost:1"}) // nothing listening
	synced, failed, err := c.SyncAccounts(context.Background(), []AccountSync{
		{Name: "acct1", Provider: "claude", Token: "token1"},
	})

	if err == nil {
		t.Fatal("expected error for unreachable server")
	}
	if synced != 0 {
		t.Errorf("expected synced=0, got %d", synced)
	}
	if failed != 1 {
		t.Errorf("expected failed=1, got %d", failed)
	}
}

func TestSyncSingleAccount_Success(t *testing.T) {
	var registered, credSet bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)

		switch r.URL.Path {
		case "/api/v1/credentials/new":
			if body["name"] != "acct1" {
				t.Errorf("expected name=acct1, got %q", body["name"])
			}
			if body["provider"] != "anthropic" {
				t.Errorf("expected provider=anthropic, got %q", body["provider"])
			}
			registered = true
			w.WriteHeader(http.StatusOK)
		case "/api/v1/credentials/set":
			if body["account"] != "acct1" {
				t.Errorf("expected account=acct1, got %q", body["account"])
			}
			if body["token"] != "my-token" {
				t.Errorf("expected token=my-token, got %q", body["token"])
			}
			credSet = true
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := New(Config{URL: srv.URL, AuthToken: "test-token"})
	err := c.SyncSingleAccount(context.Background(), AccountSync{
		Name:     "acct1",
		Provider: "anthropic",
		Token:    "my-token",
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !registered {
		t.Error("expected account registration call")
	}
	if !credSet {
		t.Error("expected credential set call")
	}
}

func TestSyncSingleAccount_EmptyToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("should not make any requests for empty tokens")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(Config{URL: srv.URL})
	err := c.SyncSingleAccount(context.Background(), AccountSync{
		Name:     "acct1",
		Provider: "claude",
		Token:    "",
	})

	if err == nil {
		t.Fatal("expected error for empty token")
	}
}

func TestSyncSingleAccount_RegisterFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/credentials/new" {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("register failed"))
			return
		}
		t.Error("should not reach credential set after registration failure")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(Config{URL: srv.URL})
	err := c.SyncSingleAccount(context.Background(), AccountSync{
		Name:     "acct1",
		Provider: "claude",
		Token:    "token1",
	})

	if err == nil {
		t.Fatal("expected error for registration failure")
	}
}

func TestSyncAccounts_AlreadyExistsIsIdempotent(t *testing.T) {
	var credSetCalls int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/credentials/new":
			// Simulate coopmux returning "already exists" on re-registration.
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":{"code":"BAD_REQUEST","message":"account already exists: acct1"}}`))
		case "/api/v1/credentials/set":
			credSetCalls++
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := New(Config{URL: srv.URL})
	synced, failed, err := c.SyncAccounts(context.Background(), []AccountSync{
		{Name: "acct1", Provider: "claude", Token: "token1"},
	})

	if err != nil {
		t.Fatalf("expected no error for already-exists, got: %v", err)
	}
	if synced != 1 {
		t.Errorf("expected synced=1, got %d", synced)
	}
	if failed != 0 {
		t.Errorf("expected failed=0, got %d", failed)
	}
	if credSetCalls != 1 {
		t.Errorf("expected 1 credential set call after already-exists, got %d", credSetCalls)
	}
}

func TestSyncSingleAccount_AlreadyExistsIsIdempotent(t *testing.T) {
	var credSetCalled bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/credentials/new":
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":{"code":"BAD_REQUEST","message":"account already exists: acct1"}}`))
		case "/api/v1/credentials/set":
			credSetCalled = true
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := New(Config{URL: srv.URL})
	err := c.SyncSingleAccount(context.Background(), AccountSync{
		Name:     "acct1",
		Provider: "claude",
		Token:    "token1",
	})

	if err != nil {
		t.Fatalf("expected no error for already-exists, got: %v", err)
	}
	if !credSetCalled {
		t.Error("expected credential set to be called after already-exists registration")
	}
}

func TestSyncSingleAccount_SetCredentialFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/credentials/new":
			w.WriteHeader(http.StatusOK)
		case "/api/v1/credentials/set":
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("set failed"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := New(Config{URL: srv.URL})
	err := c.SyncSingleAccount(context.Background(), AccountSync{
		Name:     "acct1",
		Provider: "claude",
		Token:    "token1",
	})

	if err == nil {
		t.Fatal("expected error for credential set failure")
	}
}

func TestRegisterAccount_ModelPropagated(t *testing.T) {
	var receivedBody map[string]string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/credentials/new" {
			json.NewDecoder(r.Body).Decode(&receivedBody)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(Config{URL: srv.URL})
	err := c.SyncSingleAccount(context.Background(), AccountSync{
		Name:     "acct1",
		Provider: "claude",
		Token:    "token1",
		Model:    "claude-opus-4-7",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if receivedBody["model"] != "claude-opus-4-7" {
		t.Errorf("expected model=claude-opus-4-7, got %q", receivedBody["model"])
	}
}

func TestSetCredential_LiteLLMKeyPropagated(t *testing.T) {
	var receivedBody map[string]string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/credentials/set" {
			json.NewDecoder(r.Body).Decode(&receivedBody)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(Config{URL: srv.URL})
	err := c.SyncSingleAccount(context.Background(), AccountSync{
		Name:       "acct1",
		Provider:   "claude",
		Token:      "token1",
		LiteLLMKey: "sk-litellm-123",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if receivedBody["litellm_key"] != "sk-litellm-123" {
		t.Errorf("expected litellm_key=sk-litellm-123, got %q", receivedBody["litellm_key"])
	}
}

// ka-0000002623: base_url must reach coopmux so rotation profiles carry
// ANTHROPIC_BASE_URL and a cross-endpoint switch authenticates correctly.
func TestSetCredential_BaseURLPropagated(t *testing.T) {
	var receivedBody map[string]string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/credentials/set" {
			json.NewDecoder(r.Body).Decode(&receivedBody)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(Config{URL: srv.URL})
	err := c.SyncSingleAccount(context.Background(), AccountSync{
		Name:     "acct1",
		Provider: "claude",
		Token:    "token1",
		BaseURL:  "http://gardenio-litellm:4000",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if receivedBody["base_url"] != "http://gardenio-litellm:4000" {
		t.Errorf("expected base_url=http://gardenio-litellm:4000, got %q", receivedBody["base_url"])
	}
}

// An empty BaseURL must be omitted from the payload entirely, not sent as "",
// so coopmux keeps any previously stored endpoint.
func TestSetCredential_EmptyBaseURLOmitted(t *testing.T) {
	var receivedBody map[string]string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/credentials/set" {
			json.NewDecoder(r.Body).Decode(&receivedBody)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(Config{URL: srv.URL})
	err := c.SyncSingleAccount(context.Background(), AccountSync{
		Name:     "acct1",
		Provider: "claude",
		Token:    "token1",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, present := receivedBody["base_url"]; present {
		t.Errorf("expected base_url to be omitted, got %q", receivedBody["base_url"])
	}
}

func TestRemoveAccount(t *testing.T) {
	tests := []struct {
		name       string
		account    string
		statusCode int
		respBody   string
		authToken  string
		wantErr    bool
		wantPath   string
	}{
		{
			name:       "success",
			account:    "acct1",
			statusCode: http.StatusOK,
			authToken:  "test-token",
			wantErr:    false,
			wantPath:   "/api/v1/credentials/acct1",
		},
		{
			name:       "not_found",
			account:    "missing",
			statusCode: http.StatusNotFound,
			respBody:   "not found",
			wantErr:    true,
			wantPath:   "/api/v1/credentials/missing",
		},
		{
			name:       "server_error",
			account:    "acct2",
			statusCode: http.StatusInternalServerError,
			respBody:   "boom",
			wantErr:    true,
			wantPath:   "/api/v1/credentials/acct2",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath, gotMethod, gotAuth string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotMethod = r.Method
				gotAuth = r.Header.Get("Authorization")
				w.WriteHeader(tc.statusCode)
				if tc.respBody != "" {
					w.Write([]byte(tc.respBody))
				}
			}))
			defer srv.Close()

			c := New(Config{URL: srv.URL, AuthToken: tc.authToken})
			err := c.RemoveAccount(context.Background(), tc.account)
			if (err != nil) != tc.wantErr {
				t.Fatalf("RemoveAccount err=%v, wantErr=%v", err, tc.wantErr)
			}
			if gotMethod != http.MethodDelete {
				t.Errorf("expected DELETE, got %s", gotMethod)
			}
			if gotPath != tc.wantPath {
				t.Errorf("expected path=%s, got %s", tc.wantPath, gotPath)
			}
			if tc.authToken != "" && gotAuth != "Bearer "+tc.authToken {
				t.Errorf("expected auth=Bearer %s, got %q", tc.authToken, gotAuth)
			}
		})
	}
}

func TestRemoveAccount_Unreachable(t *testing.T) {
	c := New(Config{URL: "http://localhost:1"})
	if err := c.RemoveAccount(context.Background(), "acct1"); err == nil {
		t.Fatal("expected error for unreachable server")
	}
}

func TestListAccounts(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		respBody   string
		wantErr    bool
		wantNames  []string
	}{
		{
			name:       "success_multiple",
			statusCode: http.StatusOK,
			respBody:   `[{"name":"acct1","provider":"claude"},{"name":"acct2","provider":"claude"}]`,
			wantNames:  []string{"acct1", "acct2"},
		},
		{
			name:       "success_empty",
			statusCode: http.StatusOK,
			respBody:   `[]`,
			wantNames:  []string{},
		},
		{
			name:       "http_error",
			statusCode: http.StatusInternalServerError,
			respBody:   "boom",
			wantErr:    true,
		},
		{
			name:       "unauthorized",
			statusCode: http.StatusUnauthorized,
			respBody:   "unauthorized",
			wantErr:    true,
		},
		{
			name:       "bad_json",
			statusCode: http.StatusOK,
			respBody:   `not json`,
			wantErr:    true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/credentials/status" {
					t.Errorf("unexpected path: %s", r.URL.Path)
				}
				if r.Method != http.MethodGet {
					t.Errorf("expected GET, got %s", r.Method)
				}
				if r.Header.Get("Authorization") != "Bearer tok" {
					t.Errorf("expected auth header, got %q", r.Header.Get("Authorization"))
				}
				w.WriteHeader(tc.statusCode)
				w.Write([]byte(tc.respBody))
			}))
			defer srv.Close()

			c := New(Config{URL: srv.URL, AuthToken: "tok"})
			names, err := c.ListAccounts(context.Background())
			if (err != nil) != tc.wantErr {
				t.Fatalf("ListAccounts err=%v, wantErr=%v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if len(names) != len(tc.wantNames) {
				t.Fatalf("expected %d names, got %d (%v)", len(tc.wantNames), len(names), names)
			}
			for i, n := range tc.wantNames {
				if names[i] != n {
					t.Errorf("names[%d]=%q, want %q", i, names[i], n)
				}
			}
		})
	}
}

func TestListAccounts_Unreachable(t *testing.T) {
	c := New(Config{URL: "http://localhost:1"})
	if _, err := c.ListAccounts(context.Background()); err == nil {
		t.Fatal("expected error for unreachable server")
	}
}

func TestListAccounts_InvalidURL(t *testing.T) {
	// baseURL contains control char -> http.NewRequestWithContext fails.
	c := &Client{baseURL: "http://\x7f", httpClient: &http.Client{}}
	if _, err := c.ListAccounts(context.Background()); err == nil {
		t.Fatal("expected error for invalid URL")
	}
}

func TestDoRequest_InvalidURL(t *testing.T) {
	c := &Client{baseURL: "http://\x7f", httpClient: &http.Client{}}
	if err := c.RemoveAccount(context.Background(), "acct1"); err == nil {
		t.Fatal("expected error for invalid URL")
	}
}

func TestSyncAccounts_RegisterThenSetFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/credentials/new":
			w.WriteHeader(http.StatusOK)
		case "/api/v1/credentials/set":
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("set failed"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := New(Config{URL: srv.URL})
	synced, failed, err := c.SyncAccounts(context.Background(), []AccountSync{
		{Name: "acct1", Provider: "claude", Token: "token1"},
	})
	if err == nil {
		t.Fatal("expected error when set credential fails")
	}
	if synced != 0 {
		t.Errorf("expected synced=0, got %d", synced)
	}
	if failed != 1 {
		t.Errorf("expected failed=1, got %d", failed)
	}
}
