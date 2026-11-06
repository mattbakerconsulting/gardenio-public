package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/server"
)

// fakeConfigLister serves ListConfigs from an in-memory key -> value map, with
// the store's "<namespace>:%" prefix semantics.
type fakeConfigLister struct {
	rows map[string]string
	err  error
}

func (f *fakeConfigLister) ListConfigs(_ context.Context, namespace string) ([]*model.Config, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []*model.Config
	for k, v := range f.rows {
		if strings.HasPrefix(k, namespace+":") {
			out = append(out, &model.Config{Key: k, Value: []byte(v)})
		}
	}
	return out, nil
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestDecodePersistedBridgeTokens(t *testing.T) {
	now := time.Date(2026, 10, 23, 14, 0, 0, 0, time.UTC)
	configs := []*model.Config{
		{Key: "bridge_token:agent-gd-a", Value: []byte(`{"token":"tok-a","project":"gardenio","subject":"agent:a","expires_at":"2026-10-24T14:00:00Z"}`)},
		{Key: "bridge_token:agent-gd-old", Value: []byte(`{"token":"tok-old","expires_at":"2026-10-22T14:00:00Z"}`)},
		{Key: "fleet_token:legacy", Value: []byte(`"tok-legacy"`)},
		{Key: "fleet_token:agent-gd-a", Value: []byte(`"tok-shadowed"`)},
		{Key: "bridge_token:broken", Value: []byte(`{"project":"x"}`)},
		{Key: "type:task", Value: []byte(`{}`)},
	}
	live, expired := decodePersistedBridgeTokens(configs, now)

	got := map[string]persistedBridgeToken{}
	for _, tok := range live {
		got[tok.Token] = tok
	}
	if len(live) != 2 {
		t.Fatalf("want 2 live tokens (tok-a, tok-legacy), got %+v", live)
	}
	a := got["tok-a"]
	if a.Name != "agent-gd-a" || a.Project != "gardenio" || a.Subject != "agent:a" || a.ExpiresAt.IsZero() || a.Legacy {
		t.Fatalf("tok-a decoded wrong: %+v", a)
	}
	if l := got["tok-legacy"]; l.Name != "legacy" || !l.Legacy {
		t.Fatalf("legacy bare-string row decoded wrong: %+v", l)
	}
	if _, ok := got["tok-shadowed"]; ok {
		t.Fatal("a legacy row shadowed by a canonical row must be skipped")
	}
	if len(expired) != 1 || expired[0].Name != "agent-gd-old" {
		t.Fatalf("want agent-gd-old reported expired, got %+v", expired)
	}
}

// TestResyncBridgeTokens_RollingUpdateOverlap is the gd-PPzLrS2k8e incident:
// the new daemon loads the store at startup, then the controller registers an
// agent's token on the old daemon pod (still serving during the roll). The row
// is persisted but the new daemon never had it, so every call with that token
// got 401 invalid token for the pod's whole life. One re-sync pass must make
// the token valid, scoped as persisted.
func TestResyncBridgeTokens_RollingUpdateOverlap(t *testing.T) {
	now := time.Now()
	store := &fakeConfigLister{rows: map[string]string{
		"bridge_token:agent-gd-early": `{"token":"tok-early","project":"gardenio"}`,
	}}

	// Startup: the new daemon loads what is persisted now.
	fleetCfg := server.NewFleetConfig("default", nil)
	if _, err := resyncBridgeTokens(context.Background(), store, fleetCfg, quietLogger(), now); err != nil {
		t.Fatal(err)
	}

	// The old pod registers the watchdog's token after that load.
	store.rows["bridge_token:agent-gd-hCdfUO9Ua3"] = `{"token":"tok-watchdog","project":"gardenio","subject":"agent:watchdog","expires_at":"` +
		now.Add(24*time.Hour).UTC().Format(time.RFC3339Nano) + `"}`

	handler := server.AuthMiddleware(fleetCfg, nil, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	call := func() int {
		req := httptest.NewRequest(http.MethodGet, "/v1/beads", nil)
		req.Header.Set("Authorization", "Bearer tok-watchdog")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := call(); code != http.StatusUnauthorized {
		t.Fatalf("before the re-sync the token is unknown to this daemon: want 401, got %d", code)
	}

	added, err := resyncBridgeTokens(context.Background(), store, fleetCfg, quietLogger(), now)
	if err != nil {
		t.Fatal(err)
	}
	if added != 1 {
		t.Fatalf("want exactly the missing token added, got %d", added)
	}
	if code := call(); code != http.StatusOK {
		t.Fatalf("after the re-sync the agent's token must authenticate: want 200, got %d", code)
	}
	if fleetCfg.ResolveFleet("tok-watchdog") != "agent-gd-hCdfUO9Ua3" || fleetCfg.ResolveProject("tok-watchdog") != "gardenio" {
		t.Fatal("re-synced token must keep its persisted fleet and project scope")
	}

	// A second pass is a no-op.
	if added, _ := resyncBridgeTokens(context.Background(), store, fleetCfg, quietLogger(), now); added != 0 {
		t.Fatalf("second pass must add nothing, added %d", added)
	}
}

func TestResyncBridgeTokens_RevokedRowSurvivingIsNotReadded(t *testing.T) {
	store := &fakeConfigLister{rows: map[string]string{
		"bridge_token:agent-gd-done": `{"token":"tok-done","project":"gardenio"}`,
	}}
	fleetCfg := server.NewFleetConfig("default", nil)
	if _, err := resyncBridgeTokens(context.Background(), store, fleetCfg, quietLogger(), time.Now()); err != nil {
		t.Fatal(err)
	}
	// Revoke in memory; the row delete "failed", so the row is still there.
	fleetCfg.RevokeBridgeToken("agent-gd-done")
	if added, _ := resyncBridgeTokens(context.Background(), store, fleetCfg, quietLogger(), time.Now()); added != 0 || fleetCfg.HasToken("tok-done") {
		t.Fatal("a token revoked on this daemon must not come back from a surviving row")
	}
}

func TestResyncBridgeTokens_SkipsExpired(t *testing.T) {
	now := time.Now()
	store := &fakeConfigLister{rows: map[string]string{
		"bridge_token:agent-gd-stale": `{"token":"tok-stale","expires_at":"` + now.Add(-time.Minute).UTC().Format(time.RFC3339Nano) + `"}`,
	}}
	fleetCfg := server.NewFleetConfig("default", nil)
	if added, _ := resyncBridgeTokens(context.Background(), store, fleetCfg, quietLogger(), now); added != 0 || fleetCfg.HasToken("tok-stale") {
		t.Fatal("an expired persisted token must not be registered")
	}
}

func TestResyncBridgeTokens_ListError(t *testing.T) {
	store := &fakeConfigLister{err: errors.New("db down")}
	if _, err := resyncBridgeTokens(context.Background(), store, server.NewFleetConfig("default", nil), quietLogger(), time.Now()); err == nil {
		t.Fatal("a store error must be returned so the loop logs it")
	}
}
