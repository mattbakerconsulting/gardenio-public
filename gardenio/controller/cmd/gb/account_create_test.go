package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// ── parseCredential (scheme validation) ────────────────────────────────────

func TestParseCredential(t *testing.T) {
	tests := []struct {
		name       string
		provider   string
		cred       string
		wantScheme string
		wantName   string
		wantErr    bool
	}{
		{"secret", "", "secret:gardenio-openai-key", "secret", "gardenio-openai-key", false},
		{"secret with token key", "", "secret:foo#token", "secret", "foo", false},
		{"coopmux", "", "coopmux:gardenio2", "coopmux", "gardenio2", false},
		{"env", "", "env:OPENAI_API_KEY", "env", "OPENAI_API_KEY", false},
		{"empty", "", "", "", "", true},
		{"no target", "", "secret:", "", "", true},
		{"no colon", "", "secretfoo", "", "", true},
		{"unknown scheme", "", "vault:foo", "", "", true},
		{"secret wrong key", "", "secret:foo#apikey", "", "", true},
		{"codex auth.json", "openai", "secret:gardenio-codex-auth#auth.json", "secret", "gardenio-codex-auth", false},
		{"codex key defaults", "openai", "secret:gardenio-codex-auth", "secret", "gardenio-codex-auth", false},
		{"codex wrong key", "openai", "secret:gardenio-codex-auth#token", "", "", true},
		{"codex non-secret scheme", "openai", "env:OPENAI_API_KEY", "", "", true},
		{"auth.json on non-openai", "anthropic", "secret:foo#auth.json", "", "", true},
		{"secret empty name with key", "", "secret:#token", "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseCredential(tt.provider, tt.cred)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Scheme != tt.wantScheme {
				t.Errorf("scheme = %q, want %q", got.Scheme, tt.wantScheme)
			}
			if got.Name != tt.wantName {
				t.Errorf("name = %q, want %q", got.Name, tt.wantName)
			}
		})
	}
}

// ── isBedrockRouted / looksLikeOAuthSecret (guard predicates) ──────────────

func TestIsBedrockRouted(t *testing.T) {
	cases := []struct {
		provider, model string
		want            bool
	}{
		{"bedrock", "", true},
		{"bedrock-anthropic", "", true},
		{"Bedrock", "", true},
		{"anthropic", "bedrock-claude-opus", true},
		{"anthropic", "claude-opus-4-8-oauth", false},
		{"openai", "gpt-4o", false},
	}
	for _, c := range cases {
		if got := isBedrockRouted(c.provider, c.model); got != c.want {
			t.Errorf("isBedrockRouted(%q,%q) = %v, want %v", c.provider, c.model, got, c.want)
		}
	}
}

func TestLooksLikeOAuthSecret(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"gardenio-claude-oauth-gardenio3", true},
		{"gardenio-claude-OAuth-matt", true},
		{"litellm-gardenio-key", false},
		{"gardenio-openai-key", false},
	}
	for _, c := range cases {
		if got := looksLikeOAuthSecret(c.name); got != c.want {
			t.Errorf("looksLikeOAuthSecret(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

// ── runAccountCreate: Bedrock-vs-OAuth guard (advice ka-0000005094) ─────────

func TestRunAccountCreate_BedrockOAuthRejected(t *testing.T) {
	// No daemon calls should be reached — the guard fires before create.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("daemon should not be called when the Bedrock guard rejects; got %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	setAccountCreateFlags(t, accountFlags{
		project:    "gardenio",
		provider:   "bedrock",
		credential: "secret:gardenio-claude-oauth-gardenio3",
		model:      "bedrock-claude-opus",
	})

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	err := runAccountCreate(cmd, []string{"litellm-bedrock-bad"})
	if err == nil {
		t.Fatal("expected Bedrock-vs-OAuth rejection")
	}
	if !strings.Contains(err.Error(), "ka-0000005094") {
		t.Errorf("error should cite advice ka-0000005094, got: %v", err)
	}
}

func TestRunAccountCreate_BedrockModelOAuthRejected(t *testing.T) {
	// Bedrock detected via model prefix even though provider is "anthropic".
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("daemon should not be called; got %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	setAccountCreateFlags(t, accountFlags{
		project:    "gardenio",
		provider:   "anthropic",
		credential: "secret:gardenio-claude-oauth-gardenio3",
		model:      "bedrock-claude-opus",
	})

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runAccountCreate(cmd, []string{"bad"}); err == nil {
		t.Fatal("expected rejection when model is bedrock- and credential is an OAuth secret")
	}
}

func TestRunAccountCreate_BedrockWithNonOAuthSecretOK(t *testing.T) {
	// The documented workaround: Bedrock account pointing at a non-OAuth secret
	// must be allowed.
	var created bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/beads":
			created = true
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "gd-NEW"})
		default:
			// ListAccountBeads dup check
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})
		}
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	setAccountCreateFlags(t, accountFlags{
		project:    "gardenio",
		provider:   "bedrock",
		credential: "secret:litellm-gardenio-key",
		model:      "bedrock-claude-opus",
	})

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runAccountCreate(cmd, []string{"litellm-bedrock"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !created {
		t.Error("expected account bead to be created")
	}
}

// ── runAccountCreate: flag + scheme validation ─────────────────────────────

func TestRunAccountCreate_MissingFlags(t *testing.T) {
	cases := []accountFlags{
		{provider: "openai", credential: "secret:x"}, // no project
		{project: "p", credential: "secret:x"},       // no provider
		{project: "p", provider: "openai"},           // no credential
	}
	for i, c := range cases {
		setAccountCreateFlags(t, c)
		cmd := &cobra.Command{}
		cmd.SetContext(context.Background())
		if err := runAccountCreate(cmd, []string{"acct"}); err == nil {
			t.Errorf("case %d: expected error for missing required flag", i)
		}
	}
}

func TestRunAccountCreate_BadCredentialScheme(t *testing.T) {
	setAccountCreateFlags(t, accountFlags{
		project:    "p",
		provider:   "openai",
		credential: "vault:secret-path",
	})
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runAccountCreate(cmd, []string{"acct"}); err == nil {
		t.Error("expected error for unsupported credential scheme")
	}
}

func TestRunAccountCreate_DuplicateRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"beads": []map[string]any{
				{"id": "gd-EX", "title": "dup-acct", "type": "account", "status": "open",
					"labels": []string{"project:gardenio"}, "fields": json.RawMessage(`{"provider":"openai"}`)},
			}, "total": 1,
		})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	setAccountCreateFlags(t, accountFlags{
		project:    "gardenio",
		provider:   "openai",
		credential: "secret:gardenio-openai-key",
	})
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runAccountCreate(cmd, []string{"dup-acct"}); err == nil {
		t.Error("expected error for duplicate account name")
	}
}

func TestRunAccountCreate_OK(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/beads":
			_ = json.NewDecoder(r.Body).Decode(&body)
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "gd-NEW"})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})
		}
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	setAccountCreateFlags(t, accountFlags{
		project:    "gardenio",
		provider:   "openai",
		credential: "secret:gardenio-openai-key",
		model:      "gpt-4o",
	})
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runAccountCreate(cmd, []string{"gardenio-openai"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if body["title"] != "gardenio-openai" {
		t.Errorf("title = %v, want gardenio-openai", body["title"])
	}
	if body["type"] != "account" {
		t.Errorf("type = %v, want account", body["type"])
	}
	labels, _ := body["labels"].([]any)
	if len(labels) != 1 || labels[0] != "project:gardenio" {
		t.Errorf("labels = %v, want [project:gardenio]", body["labels"])
	}
	// Fields are sent as a JSON-encoded object; confirm provider + credential.
	rawFields, _ := json.Marshal(body["fields"])
	var fields map[string]string
	_ = json.Unmarshal(rawFields, &fields)
	if fields["provider"] != "openai" || fields["credential"] != "secret:gardenio-openai-key" {
		t.Errorf("fields = %v", fields)
	}
	if fields["model"] != "gpt-4o" {
		t.Errorf("model field = %q, want gpt-4o", fields["model"])
	}
}

func TestRunAccountCreate_CodexSubscription(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/beads" {
			_ = json.NewDecoder(r.Body).Decode(&body)
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "gd-NEW"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	setAccountCreateFlags(t, accountFlags{
		project:    "gardenio",
		provider:   "openai",
		credential: "secret:gardenio-codex-auth#auth.json",
		model:      "gpt-5-codex",
	})
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runAccountCreate(cmd, []string{"gardenio-codex"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rawFields, _ := json.Marshal(body["fields"])
	var fields map[string]string
	_ = json.Unmarshal(rawFields, &fields)
	if fields["billing_type"] != "subscription" {
		t.Errorf("billing_type = %q, want subscription", fields["billing_type"])
	}
	if fields["credential"] != "secret:gardenio-codex-auth#auth.json" {
		t.Errorf("credential = %q", fields["credential"])
	}
	for _, k := range []string{"base_url", "litellm_key"} {
		if _, ok := fields[k]; ok {
			t.Errorf("field %s must not be set on a Codex subscription account", k)
		}
	}
}

func TestRunAccountCreate_CodexRejectsLiteLLM(t *testing.T) {
	setAccountCreateFlags(t, accountFlags{
		project:    "gardenio",
		provider:   "openai",
		credential: "secret:gardenio-codex-auth#auth.json",
		litellmKey: "sk-x",
	})
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runAccountCreate(cmd, []string{"gardenio-codex"}); err == nil {
		t.Error("expected error: --litellm-key does not apply to a Codex subscription account")
	}
}

// ── test helpers ───────────────────────────────────────────────────────────

type accountFlags struct {
	project, provider, credential, model, baseURL, litellmKey, fallback string
}

func setAccountCreateFlags(t *testing.T, f accountFlags) {
	t.Helper()
	accountCreateProject = f.project
	accountCreateProvider = f.provider
	accountCreateCredential = f.credential
	accountCreateModel = f.model
	accountCreateBaseURL = f.baseURL
	accountCreateLiteLLMKey = f.litellmKey
	accountCreateFallback = f.fallback
	t.Cleanup(func() {
		accountCreateProject = ""
		accountCreateProvider = ""
		accountCreateCredential = ""
		accountCreateModel = ""
		accountCreateBaseURL = ""
		accountCreateLiteLLMKey = ""
		accountCreateFallback = ""
	})
}
