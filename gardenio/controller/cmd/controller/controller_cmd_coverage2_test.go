package main

// Part 2 of controller/cmd/controller coverage tests. Split from
// controller_cmd_coverage_test.go to keep individual file size under
// quench's 800-line budget.

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"gardenio/controller/internal/config"
	"gardenio/controller/internal/coopmux"
	"gardenio/controller/internal/subscriber"
)

// ── agenttoken.go: provisionAgentToken + revokeAgentToken ────────────────

// mockFleetTokenServer handles the POST /v1/fleet-tokens and DELETE endpoints.
func mockFleetTokenServer(t *testing.T, failRegister bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/fleet-tokens":
			if failRegister {
				http.Error(w, "denied", http.StatusForbidden)
				return
			}
			// Mirror the real daemon's with_refresh:true response shape:
			// provisionAgentToken now requires a refresh_token in the body
			// since C1.3 wired it through the agent secret.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"fleet":"agent-mock","refresh_token":"refresh-mock","refresh_token_expires_at":"2099-01-01T00:00:00Z"}`))
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/fleet-tokens/"):
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("{}"))
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestProvisionAgentToken_Success(t *testing.T) {
	srv := mockFleetTokenServer(t, false)
	defer srv.Close()

	daemon := newClient(t, srv)
	k8s := fake.NewSimpleClientset()

	name, created, err := provisionAgentToken(context.Background(), discardLogger(), daemon, k8s,
		"gd-abc123", "crew-abc", "gardenio", "gardenio-system", nil)
	if err != nil {
		t.Fatalf("provisionAgentToken: %v", err)
	}
	if !created {
		t.Error("created = false, want true for a fresh Secret")
	}
	if name != "agent-token-gd-abc123" {
		t.Errorf("secret name = %q, want agent-token-gd-abc123", name)
	}
	// Secret should exist in fake cluster.
	s, err := k8s.CoreV1().Secrets("gardenio-system").
		Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected secret to exist: %v", err)
	}
	if s.StringData["token"] == "" {
		t.Error("token not set on secret")
	}
	if s.StringData["refresh_token"] != "refresh-mock" {
		t.Errorf("refresh_token = %q, want refresh-mock", s.StringData["refresh_token"])
	}
}

func TestProvisionAgentToken_RegisterFails(t *testing.T) {
	srv := mockFleetTokenServer(t, true)
	defer srv.Close()

	daemon := newClient(t, srv)
	k8s := fake.NewSimpleClientset()

	_, _, err := provisionAgentToken(context.Background(), discardLogger(), daemon, k8s,
		"gd-x", "crew-x", "proj", "ns", nil)
	if err == nil {
		t.Fatal("expected error when RegisterBridgeToken fails")
	}
}

func TestProvisionAgentToken_AlreadyExists(t *testing.T) {
	srv := mockFleetTokenServer(t, false)
	defer srv.Close()

	// Pre-create the secret that Create() would otherwise produce. The
	// fake client returns an AlreadyExists error, which must be tolerated.
	pre := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-token-gd-dup",
			Namespace: "ns",
		},
		StringData: map[string]string{"token": "OLD-TOKEN", "refresh_token": "OLD-REFRESH"},
	}
	k8s := fake.NewSimpleClientset(pre)
	daemon := newClient(t, srv)

	name, created, err := provisionAgentToken(context.Background(), discardLogger(), daemon, k8s,
		"gd-dup", "crew-dup", "proj", "ns", nil)
	if err != nil {
		t.Fatalf("expected AlreadyExists to be tolerated: %v", err)
	}
	if name != "agent-token-gd-dup" {
		t.Errorf("unexpected secret name: %s", name)
	}
	if created {
		t.Error("created = true, want false when the Secret already existed")
	}
	// Re-mint must REPLACE the stored pair: the daemon registered a new token,
	// and a Secret still holding the old one hands the next pod a stale token.
	got, err := k8s.CoreV1().Secrets("ns").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	if got.StringData["token"] == "" || got.StringData["token"] == "OLD-TOKEN" {
		t.Errorf("token = %q, want a freshly minted token (not the old one)", got.StringData["token"])
	}
	if got.StringData["refresh_token"] != "refresh-mock" {
		t.Errorf("refresh_token = %q, want refresh-mock", got.StringData["refresh_token"])
	}
}

func TestRevokeAgentToken(t *testing.T) {
	srv := mockFleetTokenServer(t, false)
	defer srv.Close()
	daemon := newClient(t, srv)

	// With an existing secret — happy path.
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-token-gd-1",
			Namespace: "ns",
		},
	}
	k8s := fake.NewSimpleClientset(s)
	revokeAgentToken(context.Background(), discardLogger(), daemon, k8s, "gd-1", "ns", nil, revocationTypeDespawn, time.Time{})

	// Without an existing secret — NotFound is tolerated.
	k8s2 := fake.NewSimpleClientset()
	revokeAgentToken(context.Background(), discardLogger(), daemon, k8s2, "gd-2", "ns", nil, revocationTypeDespawn, time.Time{})

	// Verify delete actually removed the first secret.
	_, err := k8s.CoreV1().Secrets("ns").Get(context.Background(), "agent-token-gd-1", metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("expected secret deleted, got err=%v", err)
	}
}

// ── litellm_aliases.go ───────────────────────────────────────────────────

func TestResolveLiteLLMAdminKey(t *testing.T) {
	k8s := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "litellm-key", Namespace: "ns"},
		Data:       map[string][]byte{"token": []byte("secret-token  ")},
	})
	tok, err := resolveLiteLLMAdminKey(context.Background(), k8s, "ns", "litellm-key")
	if err != nil {
		t.Fatalf("resolveLiteLLMAdminKey: %v", err)
	}
	if tok != "secret-token" {
		t.Errorf("token = %q, want 'secret-token'", tok)
	}
}

func TestResolveLiteLLMAdminKey_EmptyName(t *testing.T) {
	k8s := fake.NewSimpleClientset()
	if _, err := resolveLiteLLMAdminKey(context.Background(), k8s, "ns", ""); err == nil {
		t.Error("expected error with empty secret name")
	}
}

func TestResolveLiteLLMAdminKey_NotFound(t *testing.T) {
	k8s := fake.NewSimpleClientset()
	if _, err := resolveLiteLLMAdminKey(context.Background(), k8s, "ns", "missing"); err == nil {
		t.Error("expected error when secret missing")
	}
}

func TestResolveLiteLLMAdminKey_EmptyToken(t *testing.T) {
	k8s := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "ns"},
		Data:       map[string][]byte{"token": []byte("   ")},
	})
	if _, err := resolveLiteLLMAdminKey(context.Background(), k8s, "ns", "s"); err == nil {
		t.Error("expected error when token is empty after trim")
	}
}

func TestSyncLiteLLMModelAliases_NoURL(t *testing.T) {
	cfg := &config.Config{LiteLLMURL: ""}
	// Should early-return with no calls.
	syncLiteLLMModelAliases(context.Background(), discardLogger(), cfg, fake.NewSimpleClientset())
}

func TestSyncLiteLLMModelAliases_ResolveKeyFails(t *testing.T) {
	cfg := &config.Config{
		LiteLLMURL:         "http://not-called",
		Namespace:          "ns",
		LiteLLMAdminSecret: "missing",
	}
	syncLiteLLMModelAliases(context.Background(), discardLogger(), cfg, fake.NewSimpleClientset())
}

func TestSyncLiteLLMModelAliases_FetchFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg := &config.Config{
		LiteLLMURL:         srv.URL,
		Namespace:          "ns",
		LiteLLMAdminSecret: "s",
	}
	k8s := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "ns"},
		Data:       map[string][]byte{"token": []byte("tok")},
	})
	syncLiteLLMModelAliases(context.Background(), discardLogger(), cfg, k8s)
}

func TestSyncLiteLLMModelAliases_AllPresent(t *testing.T) {
	// Fetch returns all required aliases — no update call expected.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/get/config/callbacks") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		present := maps.Clone(claudeCodeModelAliases)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"router_settings": map[string]any{"model_group_alias": present},
		})
	}))
	defer srv.Close()

	cfg := &config.Config{
		LiteLLMURL:         srv.URL,
		Namespace:          "ns",
		LiteLLMAdminSecret: "s",
	}
	k8s := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "ns"},
		Data:       map[string][]byte{"token": []byte("tok")},
	})
	syncLiteLLMModelAliases(context.Background(), discardLogger(), cfg, k8s)
}

func TestSyncLiteLLMModelAliases_AddsMissing(t *testing.T) {
	var updateCalled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/get/config/callbacks"):
			// Return empty aliases — all entries are "missing".
			_ = json.NewEncoder(w).Encode(map[string]any{
				"router_settings": map[string]any{"model_group_alias": map[string]string{}},
			})
		case strings.HasSuffix(r.URL.Path, "/config/update"):
			updateCalled = true
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("{}"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cfg := &config.Config{
		LiteLLMURL:         srv.URL,
		Namespace:          "ns",
		LiteLLMAdminSecret: "s",
	}
	k8s := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "ns"},
		Data:       map[string][]byte{"token": []byte("tok")},
	})
	syncLiteLLMModelAliases(context.Background(), discardLogger(), cfg, k8s)
	if !updateCalled {
		t.Error("expected /config/update to be called")
	}
}

func TestSyncLiteLLMModelAliases_UpdateFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/get/config/callbacks"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"router_settings": map[string]any{"model_group_alias": map[string]string{}},
			})
		case strings.HasSuffix(r.URL.Path, "/config/update"):
			http.Error(w, "nope", http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	cfg := &config.Config{
		LiteLLMURL:         srv.URL,
		Namespace:          "ns",
		LiteLLMAdminSecret: "s",
	}
	k8s := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "ns"},
		Data:       map[string][]byte{"token": []byte("tok")},
	})
	syncLiteLLMModelAliases(context.Background(), discardLogger(), cfg, k8s)
}

// ── main.go: refreshRoleCache ────────────────────────────────────────────

func TestRefreshRoleCache(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q, _ := url.ParseQuery(r.URL.RawQuery)
		if got := q.Get("type"); got != "role" {
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"beads": []map[string]any{
				{
					"id": "r1", "title": "crew", "type": "role",
					"fields": json.RawMessage(`{"image":"img:1","cpu_limit":"1","memory_limit":"1Gi"}`),
				},
			},
			"total": 1,
		})
	}))
	defer srv.Close()

	daemon := newClient(t, srv)
	cfg := &config.Config{RoleCache: map[string]config.RoleCacheEntry{}}
	refreshRoleCache(context.Background(), discardLogger(), daemon, cfg)
	if _, ok := cfg.RoleCache["crew"]; !ok {
		t.Errorf("expected crew role in cache, got %+v", cfg.RoleCache)
	}
}

func TestRefreshRoleCache_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	daemon := newClient(t, srv)
	cfg := &config.Config{RoleCache: map[string]config.RoleCacheEntry{}}
	// Must not panic; cache stays empty.
	refreshRoleCache(context.Background(), discardLogger(), daemon, cfg)
	if len(cfg.RoleCache) != 0 {
		t.Errorf("expected unchanged cache on error")
	}
}

// ── syncSingleAccountToCoopmux ───────────────────────────────────────────

// mockCoopmux returns an *httptest.Server that stubs the coopmux credential
// broker endpoints used by sync helpers. The returned client is backed by
// the test server URL.
func mockCoopmux(t *testing.T) (*coopmux.Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/credentials/new"),
			strings.HasSuffix(r.URL.Path, "/credentials/set"),
			strings.HasSuffix(r.URL.Path, "/credentials/distribute"):
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/credentials/status"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]coopmux.AccountStatus{})
		default:
			if r.Method == http.MethodDelete {
				w.WriteHeader(http.StatusOK)
				return
			}
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return coopmux.New(coopmux.Config{URL: srv.URL}), srv
}

func TestSyncSingleAccountToCoopmux_MissingTitle(t *testing.T) {
	k8s := fake.NewSimpleClientset()
	mux, _ := mockCoopmux(t)
	ev := subscriber.Event{BeadID: "x", Metadata: map[string]string{}}
	syncSingleAccountToCoopmux(context.Background(), discardLogger(), ev, k8s, "ns", mux, nil, "")
}

func TestSyncSingleAccountToCoopmux_EmptyCredential(t *testing.T) {
	k8s := fake.NewSimpleClientset()
	mux, _ := mockCoopmux(t)
	ev := subscriber.Event{BeadID: "x", Metadata: map[string]string{"title": "acct"}}
	syncSingleAccountToCoopmux(context.Background(), discardLogger(), ev, k8s, "ns", mux, nil, "")
}

func TestSyncSingleAccountToCoopmux_NonSecret(t *testing.T) {
	k8s := fake.NewSimpleClientset()
	mux, _ := mockCoopmux(t)
	ev := subscriber.Event{BeadID: "x", Metadata: map[string]string{
		"title":      "acct",
		"credential": "env:FOO",
	}}
	syncSingleAccountToCoopmux(context.Background(), discardLogger(), ev, k8s, "ns", mux, nil, "")
}

func TestSyncSingleAccountToCoopmux_SecretNotFound(t *testing.T) {
	k8s := fake.NewSimpleClientset()
	mux, _ := mockCoopmux(t)
	ev := subscriber.Event{BeadID: "x", Metadata: map[string]string{
		"title":      "acct",
		"credential": "secret:missing",
	}}
	syncSingleAccountToCoopmux(context.Background(), discardLogger(), ev, k8s, "ns", mux, nil, "")
}

func TestSyncSingleAccountToCoopmux_EmptyToken(t *testing.T) {
	k8s := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "ns"},
		Data:       map[string][]byte{"token": []byte("   ")},
	})
	mux, _ := mockCoopmux(t)
	ev := subscriber.Event{BeadID: "x", Metadata: map[string]string{
		"title":      "acct",
		"credential": "secret:s",
	}}
	syncSingleAccountToCoopmux(context.Background(), discardLogger(), ev, k8s, "ns", mux, nil, "")
}

func TestSyncSingleAccountToCoopmux_Success(t *testing.T) {
	k8s := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "ns"},
		Data:       map[string][]byte{"token": []byte("tok-123")},
	})
	mux, _ := mockCoopmux(t)
	notifier := &credSyncNotifier{logger: discardLogger()}
	ev := subscriber.Event{BeadID: "x", Metadata: map[string]string{
		"title":       "acct",
		"credential":  "secret:s",
		"provider":    "claude",
		"model":       "claude-opus",
		"litellm_key": "vk-1",
	}}
	syncSingleAccountToCoopmux(context.Background(), discardLogger(), ev, k8s, "ns", mux, notifier, "")
}

func TestSyncSingleAccountToCoopmux_PushFails(t *testing.T) {
	k8s := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "ns"},
		Data:       map[string][]byte{"token": []byte("tok-123")},
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "broken", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	mux := coopmux.New(coopmux.Config{URL: srv.URL})
	notifier := &credSyncNotifier{logger: discardLogger()}
	ev := subscriber.Event{BeadID: "x", Metadata: map[string]string{
		"title":      "acct",
		"credential": "secret:s",
	}}
	syncSingleAccountToCoopmux(context.Background(), discardLogger(), ev, k8s, "ns", mux, notifier, "")
}

// ── syncAccountsToCoopmux ─────────────────────────────────────────────────

func TestSyncAccountsToCoopmux_ListError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	daemon := newClient(t, srv)
	k8s := fake.NewSimpleClientset()
	mux, _ := mockCoopmux(t)
	notifier := &credSyncNotifier{logger: discardLogger()}
	ok := syncAccountsToCoopmux(context.Background(), discardLogger(), daemon, k8s, "ns", mux, notifier, "", "test")
	if ok {
		t.Error("expected false on list error")
	}
}

func TestSyncAccountsToCoopmux_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"beads":[],"total":0}`))
	}))
	t.Cleanup(srv.Close)
	daemon := newClient(t, srv)
	k8s := fake.NewSimpleClientset()
	mux, _ := mockCoopmux(t)
	notifier := &credSyncNotifier{logger: discardLogger()}
	ok := syncAccountsToCoopmux(context.Background(), discardLogger(), daemon, k8s, "ns", mux, notifier, "", "test")
	if !ok {
		t.Error("expected true for empty accounts")
	}
}

func TestSyncAccountsToCoopmux_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := map[string]any{
			"beads": []map[string]any{
				{
					"id": "a1", "title": "acct1", "type": "account", "status": "open",
					"labels": []string{},
					"fields": map[string]any{"credential": "secret:s1", "provider": "claude"},
				},
				{
					"id": "a2", "title": "acct2", "type": "account", "status": "open",
					"labels": []string{},
					"fields": map[string]any{"credential": "env:FOO"},
				},
				{
					"id": "a3", "title": "acct3", "type": "account", "status": "open",
					"labels": []string{},
					"fields": map[string]any{"credential": ""},
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
	mux, _ := mockCoopmux(t)
	notifier := &credSyncNotifier{logger: discardLogger()}
	ok := syncAccountsToCoopmux(context.Background(), discardLogger(), daemon, k8s, "ns", mux, notifier, "", "test")
	if !ok {
		t.Error("expected true on success")
	}
}

func TestSyncAccountsToCoopmux_SecretMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := map[string]any{
			"beads": []map[string]any{
				{
					"id": "a1", "title": "acct1", "type": "account", "status": "open",
					"labels": []string{},
					"fields": map[string]any{"credential": "secret:missing"},
				},
			},
			"total": 1,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	daemon := newClient(t, srv)
	k8s := fake.NewSimpleClientset()
	mux, _ := mockCoopmux(t)
	notifier := &credSyncNotifier{logger: discardLogger()}
	ok := syncAccountsToCoopmux(context.Background(), discardLogger(), daemon, k8s, "ns", mux, notifier, "", "test")
	if !ok {
		t.Error("expected true even when secret missing (skipped gracefully)")
	}
}

func TestSyncAccountsToCoopmux_EmptyToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := map[string]any{
			"beads": []map[string]any{
				{
					"id": "a1", "title": "acct1", "type": "account", "status": "open",
					"labels": []string{},
					"fields": map[string]any{"credential": "secret:s1"},
				},
			},
			"total": 1,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	daemon := newClient(t, srv)
	k8s := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "ns"},
		Data:       map[string][]byte{"token": []byte("  ")},
	})
	mux, _ := mockCoopmux(t)
	notifier := &credSyncNotifier{logger: discardLogger()}
	ok := syncAccountsToCoopmux(context.Background(), discardLogger(), daemon, k8s, "ns", mux, notifier, "", "test")
	if !ok {
		t.Error("expected true when all tokens empty (no sync)")
	}
}

// ── accountExhaustionListener.Start ──────────────────────────────────────

func TestAccountExhaustionListener_StartNATSDownWaits(t *testing.T) {
	// NATS down at boot: Start keeps retrying until ctx ends rather than
	// returning a connect error that leaves the listener dead (gd-XTyUibxpls).
	l := &accountExhaustionListener{logger: discardLogger()}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := l.Start(ctx, "nats://127.0.0.1:1", "", ""); err != nil {
		t.Errorf("Start with NATS down = %v, want nil after ctx ends", err)
	}
}

// ── handlePoolAssign ─────────────────────────────────────────────────────

func TestHandlePoolAssign_NilPool(t *testing.T) {
	h := handlePoolAssign(nil, discardLogger())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/pool/assign", strings.NewReader(`{}`))
	h(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("code = %d, want 404", rec.Code)
	}
}
