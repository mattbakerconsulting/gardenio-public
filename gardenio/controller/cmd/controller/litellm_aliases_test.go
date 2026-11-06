package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchModelGroupAliases(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/get/config/callbacks" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("unexpected auth header: %s", r.Header.Get("Authorization"))
		}
		json.NewEncoder(w).Encode(map[string]any{
			"router_settings": map[string]any{
				"model_group_alias": map[string]string{
					"existing-model": "existing-target",
				},
			},
		})
	}))
	defer srv.Close()

	aliases, err := fetchModelGroupAliases(context.Background(), srv.URL, "test-key")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(aliases) != 1 {
		t.Errorf("expected 1 alias, got %d", len(aliases))
	}
	if aliases["existing-model"] != "existing-target" {
		t.Errorf("expected existing-target, got %s", aliases["existing-model"])
	}
}

func TestFetchModelGroupAliasesEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"router_settings": map[string]any{
				"model_group_alias": map[string]string{},
			},
		})
	}))
	defer srv.Close()

	aliases, err := fetchModelGroupAliases(context.Background(), srv.URL, "test-key")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(aliases) != 0 {
		t.Errorf("expected 0 aliases, got %d", len(aliases))
	}
}

func TestUpdateModelGroupAliases(t *testing.T) {
	var gotBody litellmConfigUpdate
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/config/update" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("unexpected auth header: %s", r.Header.Get("Authorization"))
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decoding request body: %v", err)
		}
		json.NewEncoder(w).Encode(map[string]string{"message": "Config updated successfully"})
	}))
	defer srv.Close()

	aliases := map[string]string{
		"claude-sonnet-4-6": "claude-sonnet-4-6-oauth",
		"claude-haiku-4-5":  "anthropic/claude-haiku-4-5",
	}
	err := updateModelGroupAliases(context.Background(), srv.URL, "test-key", aliases)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(gotBody.RouterSettings.ModelGroupAlias) != 2 {
		t.Errorf("expected 2 aliases in request, got %d", len(gotBody.RouterSettings.ModelGroupAlias))
	}
	if gotBody.RouterSettings.ModelGroupAlias["claude-sonnet-4-6"] != "claude-sonnet-4-6-oauth" {
		t.Errorf("wrong alias for claude-sonnet-4-6: %s", gotBody.RouterSettings.ModelGroupAlias["claude-sonnet-4-6"])
	}
}

func TestClaudeCodeModelAliasesComplete(t *testing.T) {
	// Verify that all expected model alias entries exist.
	expected := []string{
		"claude-sonnet-4-6",
		"claude-sonnet-4-6-20250514",
		"claude-haiku-4-5",
		"claude-haiku-4-5-20251001",
		"claude-opus-5",
		"claude-opus-5-5",
		"claude-opus-4-8",
		"claude-opus-4-7",
		"claude-opus-4-6",
		"claude-opus-4-6-20260205",
		"claude-fable-5",
	}
	for _, name := range expected {
		if _, ok := claudeCodeModelAliases[name]; !ok {
			t.Errorf("missing alias for %s", name)
		}
	}
}
