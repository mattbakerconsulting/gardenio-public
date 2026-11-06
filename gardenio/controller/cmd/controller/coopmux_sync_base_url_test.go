package main

// Tests for base-URL propagation from account beads into the coopmux
// credential sync (ka-0000002623): rotation profiles staged by coopmux must
// carry ANTHROPIC_BASE_URL so a cross-endpoint switch (e.g. onto a Bedrock
// account behind a different LiteLLM proxy) authenticates against the right
// endpoint instead of keeping the session's spawn-time base URL.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"gardenio/controller/internal/coopmux"
	"gardenio/controller/internal/subscriber"
)

func TestResolveAccountBaseURL(t *testing.T) {
	tests := []struct {
		name                          string
		proxyURL, baseURL, defaultURL string
		want                          string
	}{
		{"proxy wins", "http://claudemore:8100", "http://litellm:4000", "http://default:4000", "http://claudemore:8100"},
		{"base when no proxy", "", "http://litellm:4000", "http://default:4000", "http://litellm:4000"},
		{"default when neither", "", "", "http://default:4000", "http://default:4000"},
		{"empty when nothing", "", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveAccountBaseURL(tt.proxyURL, tt.baseURL, tt.defaultURL); got != tt.want {
				t.Errorf("resolveAccountBaseURL(%q, %q, %q) = %q, want %q",
					tt.proxyURL, tt.baseURL, tt.defaultURL, got, tt.want)
			}
		})
	}
}

// captureCoopmuxSetBodies returns a stub coopmux server that records the
// base_url of every /credentials/set body, keyed by account name.
func captureCoopmuxSetBodies(t *testing.T) (*coopmux.Client, func() map[string]string) {
	t.Helper()
	var mu sync.Mutex
	baseURLs := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/credentials/set") {
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			baseURLs[body["account"]] = body["base_url"]
			mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	snapshot := func() map[string]string {
		mu.Lock()
		defer mu.Unlock()
		out := make(map[string]string, len(baseURLs))
		for k, v := range baseURLs {
			out[k] = v
		}
		return out
	}
	return coopmux.New(coopmux.Config{URL: srv.URL}), snapshot
}

// The bulk sync must resolve each account's base URL the same way the
// pod-spawn path does: proxy_url > base_url > controller LiteLLM default.
func TestSyncAccountsToCoopmux_BaseURLResolved(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := map[string]any{
			"beads": []map[string]any{
				{
					"id": "a1", "title": "with-base", "type": "account", "status": "open",
					"labels": []string{},
					"fields": map[string]any{
						"credential": "secret:s1", "provider": "claude",
						"base_url": "https://litellm.app.devops.acme.example",
					},
				},
				{
					"id": "a2", "title": "with-proxy", "type": "account", "status": "open",
					"labels": []string{},
					"fields": map[string]any{
						"credential": "secret:s1", "provider": "claude",
						"base_url":  "https://litellm.app.devops.acme.example",
						"proxy_url": "http://claudemore:8100",
					},
				},
				{
					"id": "a3", "title": "no-base", "type": "account", "status": "open",
					"labels": []string{},
					"fields": map[string]any{"credential": "secret:s1", "provider": "claude"},
				},
			},
			"total": 3,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	daemon := newClient(t, srv)
	k8s := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "ns"},
		Data:       map[string][]byte{"token": []byte("tok-123")},
	})
	mux, snapshot := captureCoopmuxSetBodies(t)
	notifier := &credSyncNotifier{logger: discardLogger()}

	ok := syncAccountsToCoopmux(context.Background(), discardLogger(), daemon, k8s, "ns", mux, notifier, "http://gardenio-litellm:4000", "test")
	if !ok {
		t.Fatal("expected sync to succeed")
	}

	baseURLs := snapshot()
	if got := baseURLs["with-base"]; got != "https://litellm.app.devops.acme.example" {
		t.Errorf("with-base: base_url = %q, want account base_url", got)
	}
	if got := baseURLs["with-proxy"]; got != "http://claudemore:8100" {
		t.Errorf("with-proxy: base_url = %q, want proxy_url to win", got)
	}
	if got := baseURLs["no-base"]; got != "http://gardenio-litellm:4000" {
		t.Errorf("no-base: base_url = %q, want controller LiteLLM default", got)
	}
}

// The single-account (SSE-driven) sync resolves the base URL from the event
// metadata the subscriber now forwards (base_url / proxy_url fields).
func TestSyncSingleAccountToCoopmux_BaseURLResolved(t *testing.T) {
	k8s := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "ns"},
		Data:       map[string][]byte{"token": []byte("tok-123")},
	})
	mux, snapshot := captureCoopmuxSetBodies(t)
	ev := subscriber.Event{BeadID: "x", Metadata: map[string]string{
		"title":      "bedrock-acct",
		"credential": "secret:s",
		"provider":   "claude",
		"model":      "bedrock-glm-5",
		"base_url":   "https://litellm.app.devops.acme.example",
	}}
	syncSingleAccountToCoopmux(context.Background(), discardLogger(), ev, k8s, "ns", mux, nil, "http://gardenio-litellm:4000")

	if got := snapshot()["bedrock-acct"]; got != "https://litellm.app.devops.acme.example" {
		t.Errorf("base_url = %q, want the account's own base_url", got)
	}
}
