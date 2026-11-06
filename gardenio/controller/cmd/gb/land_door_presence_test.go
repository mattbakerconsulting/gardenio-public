package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// landDoorPresenceStub is a land door at /land/acme that answers GET /v1/me
// and 404s every other route, as the door does the hook routes. It records
// each path asked for.
func landDoorPresenceStub(t *testing.T) func() []string {
	t.Helper()
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.Path)
		mu.Unlock()
		if r.Method == http.MethodGet && r.URL.Path == "/land/acme/v1/me" {
			_, _ = w.Write([]byte(`{"agent_bead":"gd-a1","token_project":"acme"}`))
			return
		}
		http.Error(w, `{"error":"not found on the land door"}`, http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	swapDaemon(t, srv.URL+"/land/acme")
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(paths)
	}
}

// On a land door the hooks never ask for a hook route, which the door does
// not serve (gd-2iqSr9ANOH): SessionStart, the heartbeat and every Claude
// hook emit are a GET /v1/me, the door's presence, answered at once and
// never blocking; Stop and the relay send nothing.
func TestLandDoor_HooksBeatPresenceThroughMe(t *testing.T) {
	paths := landDoorPresenceStub(t)
	ctx := context.Background()

	resp, err := emitHookWithRetry(ctx, beadsapi.EmitHookRequest{HookType: "Stop", Actor: "studio-x7rq"})
	if err != nil || resp == nil || resp.Block {
		t.Fatalf("emitHookWithRetry on a land door = %+v, %v; want an allow at once", resp, err)
	}
	for _, hook := range []string{"SessionStart", "PreToolUse", "Stop"} {
		if err := emitAgentHook(ctx, "gd-a1", "studio-x7rq", hook, t.TempDir()); err != nil {
			t.Errorf("emitAgentHook(%s) on a land door: %v", hook, err)
		}
	}
	if err := runHookRelay(nil); err != nil {
		t.Errorf("hook relay on a land door: %v", err)
	}

	want := []string{"GET /land/acme/v1/me", "GET /land/acme/v1/me", "GET /land/acme/v1/me"}
	if got := paths(); !slices.Equal(got, want) {
		t.Errorf("land door requests = %v, want %v (no hook route, nothing for Stop or the relay)", got, want)
	}
	if got := localMailPoll(); got != landDoorMailPoll {
		t.Errorf("localMailPoll on a land door = %s, want %s", got, landDoorMailPoll)
	}
}

// Off a land door the hooks still emit to /v1/hooks/emit and mail streams.
func TestLandDoor_FleetHooksUnchanged(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Method+" "+r.URL.Path)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	if err := emitAgentHook(context.Background(), "gd-a1", "crew-1", "SessionStart", t.TempDir()); err != nil {
		t.Fatalf("emitAgentHook: %v", err)
	}
	if want := []string{"POST /v1/hooks/emit"}; !slices.Equal(got, want) {
		t.Errorf("fleet requests = %v, want %v", got, want)
	}
	if p := localMailPoll(); p != 0 {
		t.Errorf("localMailPoll off a land door = %s, want 0 (stream)", p)
	}
}
