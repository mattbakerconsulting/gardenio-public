package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation/surfacetest"
)

// asCaller serves h with mark applied to every request's context, as
// bridgeRBAC marks a call it admitted.
func asCaller(h http.Handler, mark func(context.Context) context.Context) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(mark(r.Context())))
	})
}

// asToken serves h as calls on c's per-agent token.
func asToken(h http.Handler, c AgentCaller) http.Handler {
	return asCaller(h, func(ctx context.Context) context.Context { return WithAgentCaller(ctx, c) })
}

// seedLand adds a project bead for land with the given land_kind.
func seedLand(d *mockDaemon, land, kind string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.beads["gd-proj-"+land] = &beadsapi.BeadDetail{ID: "gd-proj-" + land, Type: "project", Status: "open", Title: land,
		Fields: map[string]string{"land_kind": kind}}
}

// sayBody is a say body in the shape gb sends, with agent "" left out.
func sayBody(agent string, extra map[string]string) map[string]string {
	body := map[string]string{"text": "hi", "land": "gardenio"}
	if agent != "" {
		body["agent"] = agent
	}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

// Under a per-agent token the agent is the token's (gd-P5wjHDweih): a say or
// a read whose ?agent= or body agent names another agent is refused before
// anything is read or posted, on the conversation routes and the legacy alias
// alike. A request that names the token's own agent, in either form, or names
// none (the steward's gb say on its token alone) goes through.
func TestAgentTokenSpoofedAgentRefused(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	a, daemon := newSayAPI(t, twin)
	daemon.beads["gd-agent1"] = &beadsapi.BeadDetail{ID: "gd-agent1", Type: "agent", Status: "open",
		Labels: []string{"project:gardenio"}, Fields: map[string]string{"agent": "crew-say"}}
	h := asToken(a.legacyMux(), AgentCaller{Agent: "crew-say", BeadID: "gd-agent1", Land: "gardenio"})
	coords := "channel=" + sayChannel + "&ts=" + sayRootTS

	for _, spoof := range []string{"crew-other", "gardenio/crew/crew-other"} {
		if rr, _ := postSay(t, h, sayBody(spoof, map[string]string{"thread": sayHeadID})); rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), "crew-other") {
			t.Errorf("say as %s on crew-say's token: %d %s, want a 403 naming it", spoof, rr.Code, rr.Body)
		}
		for _, target := range []string{
			"/api/conversation/thread?agent=" + spoof + "&thread=" + sayHeadID,
			"/api/conversation/thread?agent=" + spoof + "&" + coords,
			"/api/slack/threads?agent=" + spoof + "&" + coords,
		} {
			if rr := get(h, target); rr.Code != http.StatusForbidden {
				t.Errorf("%s on crew-say's token: %d %s, want 403", target, rr.Code, rr.Body)
			}
		}
	}
	if n := len(twin.Calls()); n != 0 {
		t.Fatalf("Slack calls = %d, want none for a refused call", n)
	}

	for _, agent := range []string{"crew-say", "gardenio/crew/crew-say", ""} {
		if rr, resp := postSay(t, h, sayBody(agent, map[string]string{"thread": sayHeadID, "text": "hi as " + agent})); rr.Code != http.StatusOK || resp.Thread != sayHeadID {
			t.Errorf("say naming %q on crew-say's token: %d %s, want 200", agent, rr.Code, rr.Body)
		}
	}
	for _, target := range []string{
		"/api/conversation/thread?thread=" + sayHeadID,
		"/api/conversation/thread?agent=crew-say&" + coords,
		"/api/slack/threads?" + coords,
	} {
		if rr := get(h, target); rr.Code != http.StatusOK {
			t.Errorf("%s on crew-say's token: %d %s, want 200", target, rr.Code, rr.Body)
		}
	}
	for _, m := range sayMessageBeads(daemon) {
		if m.Fields["author"] != "" && !strings.Contains(m.Fields["author"], "crew-say") {
			t.Errorf("message bead %s author %q, want crew-say", m.ID, m.Fields["author"])
		}
	}
}

// The land is the token's project, not what the bridge has cached for the
// name, nor the land gb reports: a token bound to another land reaches
// gardenio's thread by no name, and a token bound to gardenio reaches it
// whatever the cache says.
func TestAgentTokenLandMismatch(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	a, _ := newSayAPI(t, twin)
	coords := "channel=" + sayChannel + "&ts=" + sayRootTS

	other := asToken(a.legacyMux(), AgentCaller{Agent: "crew-say", Land: "other"})
	for name, extra := range map[string]map[string]string{
		"thread":        {"thread": sayHeadID},
		"channel bead":  {"channel": sayChanBead},
		"Slack channel": {"channel": sayChannel},
	} {
		if rr, _ := postSay(t, other, sayBody("crew-say", extra)); rr.Code != http.StatusForbidden {
			t.Errorf("say by %s on a token of land other: %d %s, want 403", name, rr.Code, rr.Body)
		}
	}
	for _, target := range []string{
		"/api/conversation/thread?thread=" + sayHeadID,
		"/api/conversation/thread?" + coords,
		"/api/slack/threads?" + coords,
	} {
		if rr := get(other, target); rr.Code != http.StatusForbidden {
			t.Errorf("%s on a token of land other: %d %s, want 403", target, rr.Code, rr.Body)
		}
	}
	if n := len(twin.Calls()); n != 0 {
		t.Fatalf("Slack calls = %d, want none for a refused call", n)
	}

	a.bot.agentProject["crew-say"] = "other"
	own := asToken(a.legacyMux(), AgentCaller{Agent: "crew-say", Land: "gardenio"})
	if rr, _ := postSay(t, own, sayBody("crew-say", map[string]string{"thread": sayHeadID, "land": "other"})); rr.Code != http.StatusOK {
		t.Errorf("say on a token of gardenio, cached in other: %d %s, want 200", rr.Code, rr.Body)
	}
	if rr := get(own, "/api/conversation/thread?thread="+sayHeadID); rr.Code != http.StatusOK {
		t.Errorf("read on a token of gardenio, cached in other: %d %s, want 200", rr.Code, rr.Body)
	}
}

// A tenant land's token reaches no other land: not by bead, not by Slack
// coordinates, and not a thread in a Slack channel no land claims. Its own
// thread (bound on the agent bead the token names, never one found by name)
// it reaches wherever it is.
func TestAgentTokenTenantLandCannotReachAnotherLand(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	a, daemon := newSayAPI(t, twin)
	seedLand(daemon, "acre", "tenant")
	tenantBead := &beadsapi.BeadDetail{ID: "gd-steward1", Type: "agent", Status: "open",
		Labels: []string{"project:acre"}, Fields: map[string]string{"agent": "steward"}}
	daemon.beads[tenantBead.ID] = tenantBead
	// An agent of the same name elsewhere, bound to gardenio's thread: the
	// token's agent is not it.
	daemon.beads["steward"] = &beadsapi.BeadDetail{ID: "gd-steward-other", Type: "agent", Status: "open",
		Labels: []string{"project:gardenio"}, Fields: map[string]string{"agent": "steward", "slack_thread_channel": sayChannel, "slack_thread_ts": sayRootTS}}
	h := asToken(a.legacyMux(), AgentCaller{Agent: "steward", BeadID: tenantBead.ID, Land: "acre"})
	coords := "channel=" + sayChannel + "&ts=" + sayRootTS

	for name, extra := range map[string]map[string]string{
		"thread":                  {"thread": sayHeadID},
		"channel bead":            {"channel": sayChanBead},
		"Slack thread":            {"channel": sayChannel, "thread_ts": sayRootTS},
		"unclaimed Slack channel": {"channel": "C0NOLAND", "thread_ts": sayRootTS},
	} {
		if rr, _ := postSay(t, h, sayBody("steward", extra)); rr.Code != http.StatusForbidden {
			t.Errorf("tenant say by %s: %d %s, want 403", name, rr.Code, rr.Body)
		}
	}
	for _, target := range []string{
		"/api/conversation/thread?thread=" + sayHeadID,
		"/api/conversation/thread?" + coords,
		"/api/slack/threads?" + coords,
		"/api/slack/threads?channel=C0NOLAND&ts=" + sayRootTS,
	} {
		if rr := get(h, target); rr.Code != http.StatusForbidden {
			t.Errorf("tenant read %s: %d %s, want 403", target, rr.Code, rr.Body)
		}
	}
	if n := len(twin.Calls()); n != 0 {
		t.Fatalf("Slack calls = %d, want none for a refused call", n)
	}

	tenantBead.Fields["slack_thread_channel"], tenantBead.Fields["slack_thread_ts"] = sayChannel, sayRootTS
	if rr, _ := postSay(t, h, sayBody("steward", map[string]string{"channel": sayChannel, "thread_ts": sayRootTS})); rr.Code != http.StatusOK {
		t.Errorf("tenant say in its own thread: %d %s, want 200", rr.Code, rr.Body)
	}
	if rr := get(h, "/api/conversation/thread?"+coords); rr.Code != http.StatusOK {
		t.Errorf("tenant read of its own thread: %d %s, want 200", rr.Code, rr.Body)
	}
	if rr, _ := postSay(t, h, sayBody("steward", map[string]string{"thread": sayHeadID})); rr.Code != http.StatusForbidden {
		t.Errorf("tenant say in its own thread by head id, another land: %d %s, want 403", rr.Code, rr.Body)
	}
}

// An operator land's token says by Slack coordinates in another land's thread
// as before, except in a tenant land's thread, where it says only in its own.
func TestAgentTokenSayIntoTenantThread(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	a, daemon := newSayAPI(t, twin)
	byCoords := sayBody("crew-say", map[string]string{"channel": sayChannel, "thread_ts": sayRootTS})
	h := asToken(a.mux(), AgentCaller{Agent: "crew-say", Land: "other"})

	if rr, _ := postSay(t, h, byCoords); rr.Code != http.StatusOK {
		t.Fatalf("operator land's Slack thread by coordinates: %d %s, want 200", rr.Code, rr.Body)
	}
	seedLand(daemon, "gardenio", "tenant")
	if rr, _ := postSay(t, h, sayBody("crew-say", map[string]string{"channel": sayChannel, "thread_ts": sayRootTS, "text": "again"})); rr.Code != http.StatusForbidden {
		t.Fatalf("tenant land's Slack thread by coordinates: %d %s, want 403", rr.Code, rr.Body)
	}
	sm, err := NewStateManager(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	a.bot.state = sm
	if err := sm.SetThreadAgent(sayChannel, sayRootTS, "other/crew/crew-say"); err != nil {
		t.Fatal(err)
	}
	if rr, _ := postSay(t, h, sayBody("crew-say", map[string]string{"channel": sayChannel, "thread_ts": sayRootTS, "text": "own"})); rr.Code != http.StatusOK {
		t.Fatalf("its own thread in a tenant land: %d %s, want 200", rr.Code, rr.Body)
	}
}

// The shared bearer names an agent, a claim any holder can make, so it
// reaches operator lands only: a named agent of a tenant land, and a target in
// one, are refused before anything is read or posted. An OIDC admin is not
// fenced.
func TestSharedBearerRefusedForTenantLand(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	a, daemon := newSayAPI(t, twin)
	mux := a.legacyMux()
	coords := "channel=" + sayChannel + "&ts=" + sayRootTS
	seedLand(daemon, "gardenio", "tenant")

	// crew-say is an agent of the tenant land.
	if rr, _ := postSay(t, mux, sayAgentBody(map[string]string{"thread": sayHeadID, "text": "hi"})); rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), AgentTokenHeader) {
		t.Errorf("shared bearer, tenant agent's say: %d %s, want a 403 pointing at the agent token", rr.Code, rr.Body)
	}
	for _, target := range []string{readConv + "thread=" + sayHeadID, readConv + coords, readLegacy + coords} {
		if rr := get(mux, target); rr.Code != http.StatusForbidden {
			t.Errorf("shared bearer, tenant agent's read %s: %d %s, want 403", target, rr.Code, rr.Body)
		}
	}

	// An operator land's agent bound to the tenant land's thread: the wall
	// would let it read its own thread, the fence does not.
	a.bot.agentProject["crew-op"] = "other"
	daemon.beads["crew-op"] = &beadsapi.BeadDetail{ID: "gd-op1", Type: "agent", Status: "open",
		Labels: []string{"project:other"}, Fields: map[string]string{"agent": "crew-op", "slack_thread_channel": sayChannel, "slack_thread_ts": sayRootTS}}
	for _, target := range []string{"/api/conversation/thread?agent=crew-op&" + coords, "/api/slack/threads?agent=crew-op&" + coords} {
		if rr := get(mux, target); rr.Code != http.StatusForbidden {
			t.Errorf("shared bearer, tenant thread %s: %d %s, want 403", target, rr.Code, rr.Body)
		}
	}
	if rr, _ := postSay(t, mux, sayBody("crew-op", map[string]string{"channel": sayChannel, "thread_ts": sayRootTS})); rr.Code != http.StatusForbidden {
		t.Errorf("shared bearer, say in a tenant thread: %d %s, want 403", rr.Code, rr.Body)
	}
	if n := len(twin.Calls()); n != 0 {
		t.Fatalf("Slack calls = %d, want none for a refused call", n)
	}

	op := asCaller(mux, WithOperatorCaller)
	if rr := get(op, readConv+coords); rr.Code != http.StatusOK {
		t.Errorf("OIDC admin read in a tenant land: %d %s, want 200", rr.Code, rr.Body)
	}

	// The same calls in an operator land go through as they always have.
	seedLand(daemon, "gardenio", beadsapi.LandKindOperator)
	if rr, _ := postSay(t, mux, sayAgentBody(map[string]string{"thread": sayHeadID, "text": "hi"})); rr.Code != http.StatusOK {
		t.Errorf("shared bearer, operator land say: %d %s, want 200", rr.Code, rr.Body)
	}
	if rr := get(mux, "/api/slack/threads?agent=crew-op&"+coords); rr.Code != http.StatusOK {
		t.Errorf("shared bearer, operator land read of its own thread: %d %s, want 200", rr.Code, rr.Body)
	}
}

// With the shared bearer retired (10.3), it says and reads nothing on these
// routes, named agent or not; an agent's token and an OIDC admin still do.
func TestSharedBearerRetired(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	a, _ := newSayAPI(t, twin)
	a.SetSharedBearerAgents(false)
	mux := a.legacyMux()
	coords := "channel=" + sayChannel + "&ts=" + sayRootTS

	if rr, _ := postSay(t, mux, sayAgentBody(map[string]string{"thread": sayHeadID, "text": "hi"})); rr.Code != http.StatusUnauthorized {
		t.Errorf("retired bearer say: %d %s, want 401", rr.Code, rr.Body)
	}
	for _, target := range []string{readConv + coords, readLegacy + coords, "/api/slack/threads?" + coords} {
		if rr := get(mux, target); rr.Code != http.StatusUnauthorized {
			t.Errorf("retired bearer %s: %d %s, want 401", target, rr.Code, rr.Body)
		}
	}
	if n := len(twin.Calls()); n != 0 {
		t.Fatalf("Slack calls = %d, want none", n)
	}

	tok := asToken(mux, AgentCaller{Agent: "crew-say", Land: "gardenio"})
	if rr, _ := postSay(t, tok, sayBody("", map[string]string{"thread": sayHeadID})); rr.Code != http.StatusOK {
		t.Errorf("agent token say: %d %s, want 200", rr.Code, rr.Body)
	}
	if rr := get(tok, "/api/slack/threads?"+coords); rr.Code != http.StatusOK {
		t.Errorf("agent token legacy read: %d %s, want 200", rr.Code, rr.Body)
	}
	op := asCaller(mux, WithOperatorCaller)
	if rr := get(op, readConv+coords); rr.Code != http.StatusOK {
		t.Errorf("OIDC admin read: %d %s, want 200", rr.Code, rr.Body)
	}
}

// fakeMe is a daemon GET /v1/me that knows tokens by value.
type fakeMe struct {
	calls atomic.Int32
	me    map[string]beadsapi.MeResponse
	fail  bool
}

func (f *fakeMe) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.calls.Add(1)
	if r.URL.Path != "/v1/me" {
		http.NotFound(w, r)
		return
	}
	if f.fail {
		http.Error(w, "boom", http.StatusInternalServerError)
		return
	}
	me, ok := f.me[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
	if !ok {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	_ = json.NewEncoder(w).Encode(me)
}

func TestAgentTokenVerifier(t *testing.T) {
	f := &fakeMe{me: map[string]beadsapi.MeResponse{
		"agent-tok":  {OperatorID: "steward", Source: "token", TokenProject: "acre", Agent: "steward", AgentBead: "gd-steward1"},
		"static-tok": {OperatorID: "crew-say", Source: "token"},
	}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	v := NewAgentTokenVerifier(srv.URL+"/", nil)
	now := time.Unix(1_800_000_000, 0)
	v.now = func() time.Time { return now }
	ctx := context.Background()

	c, err := v.Verify(ctx, "agent-tok")
	if err != nil || c != (AgentCaller{Agent: "steward", BeadID: "gd-steward1", Land: "acre"}) {
		t.Fatalf("agent token = %+v, %v", c, err)
	}
	if _, err := v.Verify(ctx, "agent-tok"); err != nil || f.calls.Load() != 1 {
		t.Fatalf("cached agent token: err %v, daemon calls %d, want 1", err, f.calls.Load())
	}
	now = now.Add(agentTokenCacheTTL)
	if _, err := v.Verify(ctx, "agent-tok"); err != nil || f.calls.Load() != 2 {
		t.Fatalf("expired cache entry: err %v, daemon calls %d, want 2", err, f.calls.Load())
	}

	if _, err := v.Verify(ctx, "static-tok"); !errors.Is(err, ErrNotAgentToken) {
		t.Errorf("a token that names no agent: %v, want ErrNotAgentToken", err)
	}
	for range 2 {
		if _, err := v.Verify(ctx, "revoked-tok"); !errors.Is(err, ErrAgentTokenRejected) {
			t.Errorf("an unknown token: %v, want ErrAgentTokenRejected", err)
		}
	}
	if n := f.calls.Load(); n != 5 {
		t.Errorf("daemon calls = %d, want 5 (refusals are not cached)", n)
	}
	if _, err := v.Verify(ctx, ""); !errors.Is(err, ErrAgentTokenRejected) {
		t.Errorf("empty token: %v, want ErrAgentTokenRejected", err)
	}

	f.fail = true
	if _, err := v.Verify(ctx, "other-tok"); err == nil || errors.Is(err, ErrNotAgentToken) || errors.Is(err, ErrAgentTokenRejected) {
		t.Errorf("daemon down: %v, want an unavailable error", err)
	}
}

// The verifier's cache is bounded.
func TestAgentTokenVerifierCacheBounded(t *testing.T) {
	f := &fakeMe{me: map[string]beadsapi.MeResponse{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	v := NewAgentTokenVerifier(srv.URL, nil)
	for i := range agentTokenCacheMax + 10 {
		tok := "tok-" + string(rune('a'+i%26)) + strings.Repeat("x", i)
		f.me[tok] = beadsapi.MeResponse{Agent: "a"}
		if _, err := v.Verify(context.Background(), tok); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(v.cache); n > agentTokenCacheMax {
		t.Errorf("cache size = %d, want at most %d", n, agentTokenCacheMax)
	}
}

func TestIsAgentTokenRoute(t *testing.T) {
	for _, c := range []struct {
		method, path string
		want         bool
	}{
		{http.MethodPost, "/api/conversation/say", true},
		{http.MethodGet, "/api/conversation/thread", true},
		{http.MethodGet, "/api/slack/threads", true},
		{http.MethodPost, "/api/permission/callback", true},
		{http.MethodGet, "/api/permission/callback", false},
		{http.MethodGet, "/api/conversation/say", false},
		{http.MethodPost, "/api/slack/threads/reply", false},
		{http.MethodGet, "/api/slack/channels", false},
	} {
		if got := IsAgentTokenRoute(c.method, c.path); got != c.want {
			t.Errorf("%s %s = %v, want %v", c.method, c.path, got, c.want)
		}
	}
}
