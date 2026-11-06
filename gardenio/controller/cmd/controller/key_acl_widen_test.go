package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

func TestWidenModels(t *testing.T) {
	tests := []struct {
		name        string
		current     []string
		served      []string
		wantMerged  []string
		wantChanged bool
	}{
		{
			name:        "rule 1: empty current stays unrestricted, never narrowed",
			current:     nil,
			served:      []string{"claude-opus-5-oauth", "anthropic/claude-haiku-4-5"},
			wantMerged:  nil,
			wantChanged: false,
		},
		{
			name:        "rule 2: non-empty current merges with served, sorted+deduped",
			current:     []string{"claude-opus-4-8-oauth"},
			served:      []string{"claude-opus-5-oauth", "claude-opus-4-8-oauth"},
			wantMerged:  []string{"claude-opus-4-8-oauth", "claude-opus-5-oauth"},
			wantChanged: true,
		},
		{
			name:        "rule 2: nothing new served means unchanged",
			current:     []string{"claude-opus-4-8-oauth", "claude-sonnet-5-oauth"},
			served:      []string{"claude-opus-4-8-oauth"},
			wantMerged:  []string{"claude-opus-4-8-oauth", "claude-sonnet-5-oauth"},
			wantChanged: false,
		},
		{
			name:        "rule 3: served missing an existing entry never removes it",
			current:     []string{"claude-opus-4-8-oauth", "retired-group-oauth"},
			served:      []string{"claude-opus-5-oauth"},
			wantMerged:  []string{"claude-opus-4-8-oauth", "claude-opus-5-oauth", "retired-group-oauth"},
			wantChanged: true,
		},
		{
			name:        "rule 5: only -oauth or anthropic/ served ids are added",
			current:     []string{"claude-opus-4-8-oauth"},
			served:      []string{"claude-opus-5-oauth", "anthropic/claude-haiku-4-5", "bedrock-glm-5", "text-embedding-3-large"},
			wantMerged:  []string{"anthropic/claude-haiku-4-5", "claude-opus-4-8-oauth", "claude-opus-5-oauth"},
			wantChanged: true,
		},
		{
			name:        "rule 5: no anthropic chat groups served leaves it unchanged",
			current:     []string{"claude-opus-4-8-oauth"},
			served:      []string{"bedrock-glm-5", "text-embedding-3-large"},
			wantMerged:  []string{"claude-opus-4-8-oauth"},
			wantChanged: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			merged, changed := widenModels(tc.current, tc.served)
			sort.Strings(merged)
			if changed != tc.wantChanged {
				t.Errorf("changed = %v, want %v", changed, tc.wantChanged)
			}
			if !reflect.DeepEqual(merged, tc.wantMerged) {
				t.Errorf("merged = %v, want %v", merged, tc.wantMerged)
			}
		})
	}
}

// widenACLLitellmHandler simulates the two LiteLLM endpoints widenKeyACL uses:
// GET /key/info (current model allow-list) and POST /key/update (write the
// merged list). requests records every request path so a test can assert
// whether /key/update was called at all.
func widenACLLitellmHandler(t *testing.T, currentModels []string, updateBody *map[string]any, requests *[]string) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*requests = append(*requests, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/key/info":
			json.NewEncoder(w).Encode(map[string]any{
				"info": map[string]any{"models": currentModels},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/key/update":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			*updateBody = body
			json.NewEncoder(w).Encode(map[string]any{"key": body["key"]})
		default:
			http.NotFound(w, r)
		}
	})
}

func newLitellmOnlyKeyACLWidener(t *testing.T, litellm http.Handler) *keyACLWidener {
	t.Helper()
	srv := httptest.NewServer(litellm)
	t.Cleanup(srv.Close)
	return &keyACLWidener{
		litellmURL: srv.URL,
		masterKey:  "test-master-key",
		httpClient: &http.Client{Timeout: 5 * time.Second},
		logger:     slog.Default(),
	}
}

func TestWidenKeyACL_PostsExactMergedBody(t *testing.T) {
	var requests []string
	var updateBody map[string]any
	handler := widenACLLitellmHandler(t, []string{"claude-opus-4-8-oauth"}, &updateBody, &requests)
	k := newLitellmOnlyKeyACLWidener(t, handler)

	served := []string{"claude-opus-5-oauth", "anthropic/claude-haiku-4-5", "bedrock-glm-5"}
	if err := k.widenKeyACL(context.Background(), "acct-1", "vk-test", served); err != nil {
		t.Fatalf("widenKeyACL: %v", err)
	}

	wantPaths := []string{"GET /key/info", "POST /key/update"}
	if !reflect.DeepEqual(requests, wantPaths) {
		t.Fatalf("requests = %v, want %v", requests, wantPaths)
	}

	if updateBody["key"] != "vk-test" {
		t.Errorf("update body key = %v, want vk-test", updateBody["key"])
	}
	gotModels, ok := updateBody["models"].([]any)
	if !ok {
		t.Fatalf("update body models missing or wrong type: %v", updateBody["models"])
	}
	var got []string
	for _, m := range gotModels {
		got = append(got, m.(string))
	}
	sort.Strings(got)
	want := []string{"anthropic/claude-haiku-4-5", "claude-opus-4-8-oauth", "claude-opus-5-oauth"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("update body models = %v, want %v", got, want)
	}
}

func TestWidenKeyACL_NoRequestSentWhenUnchanged(t *testing.T) {
	var requests []string
	var updateBody map[string]any
	handler := widenACLLitellmHandler(t, []string{"claude-opus-4-8-oauth"}, &updateBody, &requests)
	k := newLitellmOnlyKeyACLWidener(t, handler)

	// Nothing new: served only repeats what current already has.
	served := []string{"claude-opus-4-8-oauth"}
	if err := k.widenKeyACL(context.Background(), "acct-1", "vk-test", served); err != nil {
		t.Fatalf("widenKeyACL: %v", err)
	}

	wantPaths := []string{"GET /key/info"}
	if !reflect.DeepEqual(requests, wantPaths) {
		t.Fatalf("requests = %v, want %v (no /key/update should be sent when unchanged)", requests, wantPaths)
	}
	if updateBody != nil {
		t.Errorf("update body should be empty, got %v", updateBody)
	}
}

func TestWidenKeyACL_NoRequestSentWhenCurrentEmpty(t *testing.T) {
	var requests []string
	var updateBody map[string]any
	handler := widenACLLitellmHandler(t, nil, &updateBody, &requests)
	k := newLitellmOnlyKeyACLWidener(t, handler)

	served := []string{"claude-opus-5-oauth"}
	if err := k.widenKeyACL(context.Background(), "acct-1", "vk-unrestricted", served); err != nil {
		t.Fatalf("widenKeyACL: %v", err)
	}

	wantPaths := []string{"GET /key/info"}
	if !reflect.DeepEqual(requests, wantPaths) {
		t.Fatalf("requests = %v, want %v (unrestricted key must never be narrowed by a write)", requests, wantPaths)
	}
}

// TestQueryKeyModels_TransportErrorDoesNotLeakKey guards against logging a
// virtual key: /key/info carries the key in its query string, and Go's
// *url.Error text embeds the full request URL.
func TestQueryKeyModels_TransportErrorDoesNotLeakKey(t *testing.T) {
	const secret = "sk-SECRETVALUE-do-not-log"
	kr := &keyACLWidener{
		litellmURL: "http://127.0.0.1:1", // nothing listens on port 1
		masterKey:  "test-master-key",
		httpClient: &http.Client{Timeout: 2 * time.Second},
		logger:     slog.Default(),
	}

	_, err := kr.queryKeyModels(context.Background(), secret)
	if err == nil {
		t.Fatal("expected a transport error, got nil")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("error leaks the virtual key: %v", err)
	}

	// Positive control: the unredacted transport error really does carry the
	// key, so the assertion above is testing something.
	_, rawErr := kr.httpClient.Get(kr.litellmURL + "/key/info?key=" + secret)
	if rawErr == nil || !strings.Contains(rawErr.Error(), secret) {
		t.Fatalf("positive control failed: raw transport error does not contain the key: %v", rawErr)
	}
}

func TestQueryKeyModels_ErrorBodyDoesNotLeakKey(t *testing.T) {
	const secret = "sk-SECRETVALUE-echoed"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"key not found: ` + r.URL.Query().Get("key") + `"}`))
	}))
	t.Cleanup(srv.Close)
	kr := &keyACLWidener{litellmURL: srv.URL, masterKey: "m", httpClient: srv.Client(), logger: slog.Default()}

	_, err := kr.queryKeyModels(context.Background(), secret)
	if err == nil {
		t.Fatal("expected an HTTP error, got nil")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("error leaks the virtual key: %v", err)
	}
	if !strings.Contains(err.Error(), "[redacted]") {
		t.Errorf("expected the echoed key to be redacted, got: %v", err)
	}
}

// newTestKeyACLWidener wires a widener to a daemon stub and a LiteLLM handler,
// both behind httptest servers.
func newTestKeyACLWidener(t *testing.T, daemon *daemonStub, litellm http.Handler) *keyACLWidener {
	t.Helper()
	daemonSrv := httptest.NewServer(daemon.handler())
	t.Cleanup(daemonSrv.Close)
	litellmSrv := httptest.NewServer(litellm)
	t.Cleanup(litellmSrv.Close)

	client, err := beadsapi.New(beadsapi.Config{HTTPAddr: daemonSrv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return newKeyACLWidener(client, litellmSrv.URL, "test-master-key", 6*time.Hour, slog.Default())
}

// widenLoopLitellmHandler serves every LiteLLM endpoint one reconcile pass can
// touch and records each request as "METHOD /path".
func widenLoopLitellmHandler(t *testing.T, served []string, fail bool, requests *[]string) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*requests = append(*requests, r.Method+" "+r.URL.Path)
		switch {
		case r.URL.Path == "/v1/models":
			if fail {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			data := make([]map[string]any, 0, len(served))
			for _, id := range served {
				data = append(data, map[string]any{"id": id})
			}
			json.NewEncoder(w).Encode(map[string]any{"data": data})
		case r.URL.Path == "/key/info":
			json.NewEncoder(w).Encode(map[string]any{
				"info": map[string]any{"models": []string{"claude-opus-5-oauth"}},
			})
		case r.URL.Path == "/key/update":
			json.NewEncoder(w).Encode(map[string]any{"key": "vk-new"})
		default:
			http.NotFound(w, r)
		}
	})
}

func countRequests(requests []string, want string) int {
	n := 0
	for _, r := range requests {
		if r == want {
			n++
		}
	}
	return n
}

// A pass widens every account key and never writes an account bead.
func TestKeyACLWidener_WidensKeysWithoutBeadWrites(t *testing.T) {
	daemon := &daemonStub{beads: []map[string]any{
		makeDaemonBead("bd-acct1", "gardenio-prod", "vk-old"),
		makeDaemonBead("bd-acct2", "gardenio-nokey", ""),
	}}
	var requests []string
	w := newTestKeyACLWidener(t, daemon, widenLoopLitellmHandler(t,
		[]string{"claude-opus-5-oauth", "claude-opus-5-5-oauth"}, false, &requests))

	w.reconcile(context.Background())

	if n := countRequests(requests, "GET /key/info"); n != 1 {
		t.Errorf("GET /key/info calls = %d, want 1 (keyless account must be skipped; requests: %v)", n, requests)
	}
	if n := countRequests(requests, "POST /key/update"); n != 1 {
		t.Errorf("POST /key/update calls = %d, want 1 (requests: %v)", n, requests)
	}
	if updates := daemon.getUpdates(); len(updates) != 0 {
		t.Errorf("widen pass wrote %d bead update(s), want 0", len(updates))
	}
}

// A served-model fetch failure skips the whole pass: no key is read or widened.
func TestKeyACLWidener_ServedFetchFailureSkipsPass(t *testing.T) {
	daemon := &daemonStub{beads: []map[string]any{
		makeDaemonBead("bd-acct1", "gardenio-prod", "vk-old"),
	}}
	var requests []string
	w := newTestKeyACLWidener(t, daemon, widenLoopLitellmHandler(t, nil, true, &requests))

	w.reconcile(context.Background())

	if want := []string{"GET /v1/models"}; !reflect.DeepEqual(requests, want) {
		t.Errorf("requests = %v, want %v", requests, want)
	}
}
