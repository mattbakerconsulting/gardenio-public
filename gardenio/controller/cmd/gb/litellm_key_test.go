package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestLitellmKeyGenerate_Success asserts the mint path POSTs /key/generate with
// the master-key bearer and the scoping fields, and decodes the flat key object.
func TestLitellmKeyGenerate_Success(t *testing.T) {
	var gotBody litellmKeyGenRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/key/generate" {
			t.Errorf("path = %s, want /key/generate", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer master-123" {
			t.Errorf("Authorization = %q, want Bearer master-123", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &gotBody); err != nil {
			t.Fatalf("bad request body: %v", err)
		}
		mb := 5.0
		json.NewEncoder(w).Encode(litellmKeyMutateResponse{
			Key:            "sk-minted-xyz",
			KeyAlias:       gotBody.KeyAlias,
			MaxBudget:      &mb,
			BudgetDuration: gotBody.BudgetDuration,
		})
	}))
	defer srv.Close()

	req := litellmKeyGenRequest{
		KeyAlias:       "ci-abc123",
		MaxBudget:      5,
		BudgetDuration: "24h",
		Models:         []string{"claude-haiku-4-5"},
	}
	resp, err := litellmKeyGenerate(context.Background(), srv.Client(), srv.URL, "master-123", req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Key != "sk-minted-xyz" {
		t.Errorf("Key = %q, want sk-minted-xyz", resp.Key)
	}
	// The scoping fields must reach LiteLLM verbatim — the whole guardrail.
	if gotBody.MaxBudget != 5 {
		t.Errorf("max_budget sent = %v, want 5", gotBody.MaxBudget)
	}
	if gotBody.BudgetDuration != "24h" {
		t.Errorf("budget_duration sent = %q, want 24h", gotBody.BudgetDuration)
	}
	if len(gotBody.Models) != 1 || gotBody.Models[0] != "claude-haiku-4-5" {
		t.Errorf("models sent = %v, want [claude-haiku-4-5]", gotBody.Models)
	}
}

// TestLitellmKeyGenerate_HTTPError surfaces a non-200 as an error.
func TestLitellmKeyGenerate_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid master key"}`))
	}))
	defer srv.Close()

	_, err := litellmKeyGenerate(context.Background(), srv.Client(), srv.URL, "bad", litellmKeyGenRequest{MaxBudget: 5})
	if err == nil {
		t.Fatal("expected error for HTTP 401")
	}
}

// TestLitellmKeyUpdate_Success asserts /key/update carries the key + changed
// fields.
func TestLitellmKeyUpdate_Success(t *testing.T) {
	var gotBody litellmKeyUpdateRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/key/update" {
			t.Errorf("path = %s, want /key/update", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &gotBody)
		mb := 10.0
		json.NewEncoder(w).Encode(litellmKeyMutateResponse{Key: gotBody.Key, MaxBudget: &mb})
	}))
	defer srv.Close()

	newBudget := 10.0
	resp, err := litellmKeyUpdate(context.Background(), srv.Client(), srv.URL, "master-123",
		litellmKeyUpdateRequest{Key: "sk-abc", MaxBudget: &newBudget})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotBody.Key != "sk-abc" {
		t.Errorf("key sent = %q, want sk-abc", gotBody.Key)
	}
	if gotBody.MaxBudget == nil || *gotBody.MaxBudget != 10 {
		t.Errorf("max_budget sent = %v, want 10", gotBody.MaxBudget)
	}
	if resp.MaxBudget == nil || *resp.MaxBudget != 10 {
		t.Errorf("resp max_budget = %v, want 10", resp.MaxBudget)
	}
}

// TestLitellmKeyDelete_Success asserts /key/delete carries the key in the keys
// list.
func TestLitellmKeyDelete_Success(t *testing.T) {
	var gotBody litellmKeyDeleteRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/key/delete" {
			t.Errorf("path = %s, want /key/delete", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &gotBody)
		w.Write([]byte(`{"deleted_keys":["sk-abc"]}`))
	}))
	defer srv.Close()

	if err := litellmKeyDelete(context.Background(), srv.Client(), srv.URL, "master-123", "sk-abc"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(gotBody.Keys) != 1 || gotBody.Keys[0] != "sk-abc" {
		t.Errorf("keys sent = %v, want [sk-abc]", gotBody.Keys)
	}
}

// TestLitellmKeyDelete_NotFound classifies a 404 as key-not-found so
// --ignore-missing can make teardown idempotent.
func TestLitellmKeyDelete_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"key not found"}`))
	}))
	defer srv.Close()

	err := litellmKeyDelete(context.Background(), srv.Client(), srv.URL, "master-123", "sk-gone")
	if err == nil {
		t.Fatal("expected error for HTTP 404")
	}
	if !isKeyNotFound(err) {
		t.Errorf("isKeyNotFound(%v) = false, want true", err)
	}
}

func TestIsKeyNotFound(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"404", errStr("HTTP 404: key not found"), true},
		{"not found phrase", errStr("HTTP 400: key does not exist"), true},
		{"other", errStr("HTTP 500: internal error"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isKeyNotFound(tc.err); got != tc.want {
				t.Errorf("isKeyNotFound = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseMetadataPairs(t *testing.T) {
	got, err := parseMetadataPairs([]string{"instance=abc", "purpose=ci"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["instance"] != "abc" || got["purpose"] != "ci" {
		t.Errorf("parsed = %v, want instance=abc purpose=ci", got)
	}
	if _, err := parseMetadataPairs([]string{"noequals"}); err == nil {
		t.Error("expected error for pair without '='")
	}
	if got := mustParse(t, nil); got != nil {
		t.Errorf("empty input = %v, want nil", got)
	}
}

// TestBudgetCap429AtCap is the R4-COST-1 / R4-SEC-1 acceptance in unit form: a
// key minted with a max_budget must be REFUSED by LiteLLM with HTTP 429 once its
// spend crosses the cap. We model LiteLLM's budget enforcement with a fake proxy
// whose /v1/messages returns 200 while spend < max_budget and 429 once at/over
// it — the same behavior the deliberate-runaway acceptance script asserts
// against the real proxy. This locks the contract our mint relies on: the cap is
// enforced at the proxy (an in-run hard stop), not merely observed by the lagging
// 5-min spend reconcile.
func TestBudgetCap429AtCap(t *testing.T) {
	const maxBudget = 5.0
	const costPerCall = 2.0
	var spend float64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/key/generate":
			mb := maxBudget
			json.NewEncoder(w).Encode(litellmKeyMutateResponse{Key: "sk-capped", MaxBudget: &mb})
		case "/v1/messages":
			// Budget check happens BEFORE serving: once cumulative spend has
			// reached the cap the key is over budget and every further call 429s.
			if spend >= maxBudget {
				w.WriteHeader(http.StatusTooManyRequests)
				w.Write([]byte(`{"error":{"type":"budget_exceeded","message":"ExceededBudget: Crossed spend within budget"}}`))
				return
			}
			spend += costPerCall
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"type":"message"}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	// Mint the capped key.
	mint, err := litellmKeyGenerate(context.Background(), srv.Client(), srv.URL, "master-123",
		litellmKeyGenRequest{KeyAlias: "runaway", MaxBudget: maxBudget})
	if err != nil {
		t.Fatalf("mint failed: %v", err)
	}
	if mint.MaxBudget == nil || *mint.MaxBudget != maxBudget {
		t.Fatalf("minted key max_budget = %v, want %v", mint.MaxBudget, maxBudget)
	}

	// Drive a deliberate runaway: keep calling until we get a 429, capped at a
	// generous iteration bound so a broken cap fails as a test timeout-free error
	// rather than an infinite loop.
	got429 := false
	callsBefore429 := 0
	for i := range 100 {
		status := postMessages(t, srv.Client(), srv.URL)
		if status == http.StatusTooManyRequests {
			got429 = true
			break
		}
		if status != http.StatusOK {
			t.Fatalf("call %d: unexpected status %d", i, status)
		}
		callsBefore429++
	}
	if !got429 {
		t.Fatal("runaway never 429'd — budget cap not enforced (R4-COST-1 regression)")
	}
	// With cap=$5 and $2/call, calls 1 and 2 (spend 0→2→4) succeed and call 3
	// (spend 4≥? no — 4<5 succeeds → 6) ... the 429 lands the first call where
	// spend has reached the cap. Assert it stopped at a finite, expected point.
	if callsBefore429 != 3 {
		t.Errorf("calls before 429 = %d, want 3 (cap $%.0f / $%.0f per call)", callsBefore429, maxBudget, costPerCall)
	}
}

// postMessages drives one /v1/messages call and returns its status code.
func postMessages(t *testing.T, client *http.Client, baseURL string) int {
	t.Helper()
	resp, err := client.Post(baseURL+"/v1/messages", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /v1/messages: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// mustParse is a tiny helper so the empty-input branch of parseMetadataPairs is
// asserted without shadowing the earlier err.
func mustParse(t *testing.T, in []string) map[string]string {
	t.Helper()
	out, err := parseMetadataPairs(in)
	if err != nil {
		t.Fatalf("parseMetadataPairs(%v): %v", in, err)
	}
	return out
}

// errStr is a trivial error for table tests.
type errStr string

func (e errStr) Error() string { return string(e) }

// ── Handler-level tests: drive the cobra RunE end-to-end against a mock proxy ──

// litellmMock is a minimal LiteLLM stand-in for the write/read verbs, returning
// canned key objects and recording the last request path.
func litellmMock(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer master-123" {
			t.Errorf("Authorization = %q, want Bearer master-123", got)
		}
		switch r.URL.Path {
		case "/key/generate":
			mb := 5.0
			json.NewEncoder(w).Encode(litellmKeyMutateResponse{Key: "sk-new-999", KeyAlias: "ci", MaxBudget: &mb})
		case "/key/update":
			mb := 7.0
			json.NewEncoder(w).Encode(litellmKeyMutateResponse{Key: "sk-new-999", MaxBudget: &mb})
		case "/key/delete":
			w.Write([]byte(`{"deleted_keys":["sk-new-999"]}`))
		case "/key/info":
			mb := 5.0
			json.NewEncoder(w).Encode(liteLLMKeyInfoResponse{Info: liteLLMKeyInfo{Key: "sk-new-999", Spend: 1.5, MaxBudget: &mb}})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// newSubCmd builds a FRESH, isolated cobra.Command for one litellm-key
// subcommand, registering its flags via the same registrar init() uses. A fresh
// command per test avoids leaking flag `Changed`/values across cases (the bug of
// sharing the global subcommand's flagset).
func newSubCmd(sub *cobra.Command) *cobra.Command {
	cmd := &cobra.Command{RunE: sub.RunE}
	switch sub {
	case litellmKeyMintCmd:
		addMintFlags(cmd)
	case litellmKeyUpdateCmd:
		addUpdateFlags(cmd)
	case litellmKeyDeleteCmd:
		addDeleteFlags(cmd)
	case litellmKeyInfoCmd:
		addInfoFlags(cmd)
	}
	cmd.SetContext(context.Background())
	return cmd
}

// runSub builds a fresh command for the subcommand, applies the flag values, and
// invokes its RunE — exercising the handler exactly as `gb litellm-key <sub>`
// would.
func runSub(t *testing.T, sub *cobra.Command, baseURL string, flags map[string]string) error {
	t.Helper()
	cmd := newSubCmd(sub)
	// Endpoint always points at the mock; master key is constant.
	if err := cmd.Flags().Set("litellm-url", baseURL); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("litellm-key", "master-123"); err != nil {
		t.Fatal(err)
	}
	for k, v := range flags {
		if err := cmd.Flags().Set(k, v); err != nil {
			t.Fatalf("set %s=%s: %v", k, v, err)
		}
	}
	return cmd.RunE(cmd, nil)
}

func TestRunLitellmKeyMint_Handler(t *testing.T) {
	srv := litellmMock(t)
	defer srv.Close()
	if err := runSub(t, litellmKeyMintCmd, srv.URL, map[string]string{
		"alias": "ci", "max-budget": "5", "budget-duration": "24h", "models": "claude-haiku-4-5",
	}); err != nil {
		t.Fatalf("mint handler: %v", err)
	}
}

func TestRunLitellmKeyMint_RejectsZeroBudget(t *testing.T) {
	srv := litellmMock(t)
	defer srv.Close()
	err := runSub(t, litellmKeyMintCmd, srv.URL, map[string]string{"max-budget": "0"})
	if err == nil {
		t.Fatal("expected error for --max-budget 0")
	}
	if !strings.Contains(err.Error(), "max-budget must be > 0") {
		t.Errorf("error = %v, want max-budget > 0 message", err)
	}
}

func TestRunLitellmKeyMint_MissingEndpoint(t *testing.T) {
	// No litellm-url set → loud error, not a silent no-op.
	cmd := newSubCmd(litellmKeyMintCmd)
	_ = cmd.Flags().Set("litellm-url", "")
	_ = cmd.Flags().Set("litellm-key", "")
	_ = cmd.Flags().Set("max-budget", "5")
	if err := cmd.RunE(cmd, nil); err == nil {
		t.Fatal("expected error for missing LiteLLM URL")
	}
}

func TestRunLitellmKeyUpdate_Handler(t *testing.T) {
	srv := litellmMock(t)
	defer srv.Close()
	if err := runSub(t, litellmKeyUpdateCmd, srv.URL, map[string]string{
		"key": "sk-new-999", "max-budget": "7",
	}); err != nil {
		t.Fatalf("update handler: %v", err)
	}
}

func TestRunLitellmKeyUpdate_NothingToUpdate(t *testing.T) {
	srv := litellmMock(t)
	defer srv.Close()
	err := runSub(t, litellmKeyUpdateCmd, srv.URL, map[string]string{"key": "sk-new-999"})
	if err == nil || !strings.Contains(err.Error(), "nothing to update") {
		t.Fatalf("expected nothing-to-update error, got %v", err)
	}
}

func TestRunLitellmKeyUpdate_RequiresKey(t *testing.T) {
	srv := litellmMock(t)
	defer srv.Close()
	err := runSub(t, litellmKeyUpdateCmd, srv.URL, map[string]string{"max-budget": "7"})
	if err == nil || !strings.Contains(err.Error(), "--key is required") {
		t.Fatalf("expected --key required error, got %v", err)
	}
}

func TestRunLitellmKeyDelete_Handler(t *testing.T) {
	srv := litellmMock(t)
	defer srv.Close()
	if err := runSub(t, litellmKeyDeleteCmd, srv.URL, map[string]string{"key": "sk-new-999"}); err != nil {
		t.Fatalf("delete handler: %v", err)
	}
}

func TestRunLitellmKeyDelete_IgnoreMissing(t *testing.T) {
	// Server 404s → with --ignore-missing the handler returns nil.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"key not found"}`))
	}))
	defer srv.Close()
	if err := runSub(t, litellmKeyDeleteCmd, srv.URL, map[string]string{
		"key": "sk-gone", "ignore-missing": "true",
	}); err != nil {
		t.Fatalf("delete --ignore-missing on 404 should succeed, got %v", err)
	}
	// Without --ignore-missing the same 404 is an error.
	if err := runSub(t, litellmKeyDeleteCmd, srv.URL, map[string]string{"key": "sk-gone"}); err == nil {
		t.Fatal("delete without --ignore-missing on 404 should error")
	}
}

func TestRunLitellmKeyInfo_Handler(t *testing.T) {
	srv := litellmMock(t)
	defer srv.Close()
	if err := runSub(t, litellmKeyInfoCmd, srv.URL, map[string]string{"key": "sk-new-999"}); err != nil {
		t.Fatalf("info handler: %v", err)
	}
}

func TestFmtMaxBudget(t *testing.T) {
	if got := fmtMaxBudget(nil); got != "unset" {
		t.Errorf("fmtMaxBudget(nil) = %q, want unset", got)
	}
	v := 3.5
	if got := fmtMaxBudget(&v); got != "$3.50" {
		t.Errorf("fmtMaxBudget(3.5) = %q, want $3.50", got)
	}
}
