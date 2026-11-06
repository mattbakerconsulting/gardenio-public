package bridge

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// newRouteSlackServer answers conversations.info with the name "name-<ID>" so
// the explainer's channel names can be asserted; anything else is ok:true.
func newRouteSlackServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/conversations.info") {
			_ = r.ParseForm()
			id := r.Form.Get("channel")
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": map[string]any{"id": id, "name": "name-" + id}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func getRoute(t *testing.T, bot *Bot, query url.Values) (int, routeExplainResponse) {
	t.Helper()
	mux := http.NewServeMux()
	NewRouteExplainAPI(bot).RegisterRoutes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/slack/route?"+query.Encode(), nil))
	var resp routeExplainResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v (%s)", err, rec.Body.String())
		}
	}
	return rec.Code, resp
}

func candidate(resp routeExplainResponse, rule string) (RouteCandidate, bool) {
	for _, c := range resp.Candidates {
		if c.Rule == rule {
			return c, true
		}
	}
	return RouteCandidate{}, false
}

// TestRouteExplain_EachRule drives GET /api/slack/route through every rung of
// resolveChannel's ladder: the winning rule is named, its channel is what
// resolveChannel returns, and every rule is listed as a candidate.
func TestRouteExplain_EachRule(t *testing.T) {
	const agent = "a"
	cases := []struct {
		name        string
		router      *Router
		project     string
		role        string
		spawnCh     string
		inheritedBy *string
		wantRule    string
		wantCh      string
		// alsoApplies is a lower rule that would have applied but lost.
		alsoApplies string
	}{
		{"router override", NewRouter(RouterConfig{Overrides: map[string]string{agent: "C-override"}}), "gardenio", "cicd", "C-spawn", nil, RouteRuleRouter, "C-override", RouteRuleSpawnChannel},
		{"router pattern on project/role/name", NewRouter(RouterConfig{Channels: map[string]string{"gardenio/cicd/*": "C-pattern"}}), "gardenio", "cicd", "C-spawn", nil, RouteRuleRouter, "C-pattern", RouteRuleSpawnChannel},
		{"router override on project/role/name", NewRouter(RouterConfig{Overrides: map[string]string{"gardenio/cicd/" + agent: "C-override"}}), "gardenio", "cicd", "", nil, RouteRuleRouter, "C-override", RouteRuleRoleChannel},
		{"explicit pin", nil, "gardenio", "cicd", "C-spawn", nil, RouteRuleSpawnChannel, "C-spawn", RouteRuleRoleChannel},
		{"same-role inherited pin", nil, "gardenio", "cicd", "C-spawn", strPtr("cicd"), RouteRuleSpawnChannel, "C-spawn", RouteRuleRoleChannel},
		{"role channel beats different-role inherit", nil, "gardenio", "cicd", "C-spawn", strPtr("gardener"), RouteRuleRoleChannel, "C-cicd", RouteRuleInheritedSpawn},
		{"inherited spawn channel", nil, "gardenio", "research", "C-spawn", strPtr("gardener"), RouteRuleInheritedSpawn, "C-spawn", RouteRuleProjectPrimary},
		{"project primary", nil, "gardenio", "research", "", nil, RouteRuleProjectPrimary, "C-primary", RouteRuleBotDefault},
		{"router default", NewRouter(RouterConfig{DefaultChannel: "C-router-default"}), "", "", "", nil, RouteRuleRouterDefault, "C-router-default", RouteRuleBotDefault},
		{"bot default", nil, "", "", "", nil, RouteRuleBotDefault, "C-default", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			daemon := newMockDaemon()
			seedProjectWithRoleMap(daemon, "gardenio", "C-primary", map[string]string{"C-cicd": "cicd"})
			bot := newTestBot(daemon, newRouteSlackServer(t))
			bot.agentThreadBound = map[string]bool{}
			bot.channel = "C-default"
			bot.router = tc.router
			bot.agentProject[agent] = tc.project
			bot.agentRole[agent] = tc.role
			if tc.spawnCh != "" {
				bot.agentSpawnChannel[agent] = tc.spawnCh
			}
			if tc.inheritedBy != nil {
				bot.agentSpawnInheritedBy[agent] = *tc.inheritedBy
			}

			code, resp := getRoute(t, bot, url.Values{"agent": {"gardenio/cicd/" + agent}})
			if code != http.StatusOK {
				t.Fatalf("status = %d", code)
			}
			if resp.Agent != agent {
				t.Errorf("agent = %q, want short name %q", resp.Agent, agent)
			}
			if resp.Rule != tc.wantRule || resp.Channel != tc.wantCh {
				t.Errorf("won by %s/%s, want %s/%s", resp.Rule, resp.Channel, tc.wantRule, tc.wantCh)
			}
			if got := bot.resolveChannel(agent); got != resp.Channel {
				t.Errorf("resolveChannel = %q, explainer = %q: they must agree", got, resp.Channel)
			}
			if resp.ChannelName != "name-"+tc.wantCh {
				t.Errorf("channel_name = %q", resp.ChannelName)
			}
			if len(resp.Candidates) != 7 {
				t.Fatalf("candidates = %d, want all 7 rules", len(resp.Candidates))
			}
			won := 0
			for _, c := range resp.Candidates {
				if c.Won {
					won++
					if c.Rule != tc.wantRule {
						t.Errorf("candidate %s marked won", c.Rule)
					}
				}
				if c.Detail == "" {
					t.Errorf("candidate %s has no detail", c.Rule)
				}
			}
			if won != 1 {
				t.Errorf("%d candidates won, want 1", won)
			}
			if tc.alsoApplies != "" {
				if c, ok := candidate(resp, tc.alsoApplies); !ok || !c.Applies || c.Won {
					t.Errorf("losing candidate %s = %+v, want applies and not won", tc.alsoApplies, c)
				}
			}
		})
	}
}

// Router patterns are written against project/role/name while the bridge keys
// agents by short name (gd-H74pBA3OyK): a crew pattern routes every crew agent
// of the project, a short-name override still beats it, and an agent with no
// cached project and role cannot match a multi-segment pattern.
func TestRouteExplain_RouterPatternUsesFullIdentity(t *testing.T) {
	daemon := newMockDaemon()
	bot := newTestBot(daemon, newRouteSlackServer(t))
	bot.agentThreadBound = map[string]bool{}
	bot.channel = "C-default"
	bot.router = NewRouter(RouterConfig{
		DefaultChannel: "C-router-default",
		Channels:       map[string]string{"proj/crew/*": "C-crew"},
		Overrides:      map[string]string{"broken-out": "C-break"},
	})
	for _, a := range []string{"plain", "broken-out"} {
		bot.agentProject[a] = "proj"
		bot.agentRole[a] = "crew"
	}
	bot.agentProject["unknown-role"] = "proj"

	cases := []struct {
		agent, wantCh, wantRule, wantDetail string
	}{
		{"plain", "C-crew", RouteRuleRouter, "matched proj/crew/*"},
		{"broken-out", "C-break", RouteRuleRouter, "matched (override)"},
		{"unknown-role", "C-router-default", RouteRuleRouterDefault, "router default channel"},
	}
	for _, tc := range cases {
		t.Run(tc.agent, func(t *testing.T) {
			code, resp := getRoute(t, bot, url.Values{"agent": {tc.agent}})
			if code != http.StatusOK {
				t.Fatalf("status = %d", code)
			}
			if resp.Rule != tc.wantRule || resp.Channel != tc.wantCh {
				t.Fatalf("won by %s/%s, want %s/%s", resp.Rule, resp.Channel, tc.wantRule, tc.wantCh)
			}
			if c, _ := candidate(resp, tc.wantRule); c.Detail != tc.wantDetail {
				t.Errorf("%s detail = %q, want %q", tc.wantRule, c.Detail, tc.wantDetail)
			}
			if got := bot.resolveChannel(tc.agent); got != tc.wantCh {
				t.Errorf("resolveChannel = %q, want %q", got, tc.wantCh)
			}
		})
	}
}

// The hot path stops at the winner; the explainer's full walk must not change
// which rule wins.
func TestExplainChannelRoute_ShortCircuitMatchesFull(t *testing.T) {
	daemon := newMockDaemon()
	seedProjectWithRoleMap(daemon, "gardenio", "C-primary", map[string]string{"C-cicd": "cicd"})
	bot := newTestBot(daemon, newRouteSlackServer(t))
	bot.channel = "C-default"
	bot.agentProject["a"], bot.agentRole["a"] = "gardenio", "cicd"
	bot.agentSpawnChannel["a"] = "C-spawn"

	short, full := bot.explainChannelRoute("a", false), bot.explainChannelRoute("a", true)
	if short.Rule != full.Rule || short.Channel != full.Channel {
		t.Fatalf("short %s/%s != full %s/%s", short.Rule, short.Channel, full.Rule, full.Channel)
	}
	if len(short.Candidates) != 2 || len(full.Candidates) != 7 {
		t.Errorf("candidates short=%d full=%d, want 2 and 7", len(short.Candidates), len(full.Candidates))
	}
}

// A bead id resolves to the agent whose routing its cards follow.
func TestRouteExplain_BeadResolvesAgent(t *testing.T) {
	daemon := newMockDaemon()
	seedProjectWithRoleMap(daemon, "gardenio", "C-primary", map[string]string{"C-cicd": "cicd"})
	daemon.mu.Lock()
	daemon.beads["gd-agentBead"] = &beadsapi.BeadDetail{ID: "gd-agentBead", Type: "agent", Fields: map[string]string{"agent": "worker"}}
	daemon.beads["gd-task"] = &beadsapi.BeadDetail{ID: "gd-task", Type: "task", Assignee: "worker"}
	daemon.beads["gd-decision"] = &beadsapi.BeadDetail{ID: "gd-decision", Type: "decision", Assignee: "gardenio/crew/worker"}
	daemon.beads["gd-esc"] = &beadsapi.BeadDetail{ID: "gd-esc", Type: "escalation", Assignee: "operator", Fields: map[string]string{"requesting_agent": "gd-agentBead"}}
	daemon.beads["gd-orphan"] = &beadsapi.BeadDetail{ID: "gd-orphan", Type: "task"}
	daemon.mu.Unlock()

	bot := newTestBot(daemon, newRouteSlackServer(t))
	bot.agentThreadBound = map[string]bool{}
	bot.channel = "C-default"
	bot.agentProject["worker"], bot.agentRole["worker"] = "gardenio", "cicd"

	for _, id := range []string{"gd-agentBead", "gd-task", "gd-decision", "gd-esc"} {
		code, resp := getRoute(t, bot, url.Values{"bead": {id}})
		if code != http.StatusOK {
			t.Fatalf("%s: status %d", id, code)
		}
		if resp.Agent != "worker" || resp.Bead != id || resp.Rule != RouteRuleRoleChannel || resp.Channel != "C-cicd" {
			t.Errorf("%s: agent=%q bead=%q rule=%s channel=%s", id, resp.Agent, resp.Bead, resp.Rule, resp.Channel)
		}
	}
	if code, _ := getRoute(t, bot, url.Values{"bead": {"gd-orphan"}}); code != http.StatusUnprocessableEntity {
		t.Errorf("bead with no agent: status %d, want 422", code)
	}
}

// With no cached routing the explainer reloads it from the agent bead, as a
// decision or escalation card does, and says so.
func TestRouteExplain_ColdCacheRefreshesFromAgentBead(t *testing.T) {
	daemon := newMockDaemon()
	seedProjectWithRoleMap(daemon, "gardenio", "C-primary", map[string]string{"C-cicd": "cicd"})
	daemon.mu.Lock()
	daemon.beads["cold"] = &beadsapi.BeadDetail{ID: "gd-cold", Type: "agent", Fields: map[string]string{
		"agent": "cold", "project": "gardenio", "role": "crew", "slack_spawn_channel": "C-thread",
	}}
	daemon.mu.Unlock()
	bot := newTestBot(daemon, newRouteSlackServer(t))
	bot.channel = "C-default"

	_, resp := getRoute(t, bot, url.Values{"agent": {"cold"}})
	if !resp.CacheRefreshed {
		t.Error("cache_refreshed = false, want true")
	}
	if resp.Rule != RouteRuleSpawnChannel || resp.Channel != "C-thread" || resp.SpawnChannel != "C-thread" {
		t.Errorf("rule=%s channel=%s spawn=%s", resp.Rule, resp.Channel, resp.SpawnChannel)
	}
	if resp.Project != "gardenio" || resp.Role != "crew" {
		t.Errorf("inputs project=%q role=%q", resp.Project, resp.Role)
	}
}

// A thread agent that stopped before the last bridge restart has no thread
// mark in memory; the explainer recovers it from the (closed) agent bead
// instead of reporting thread_bound=false (gd-ckbFu30SZ1).
func TestRouteExplain_ColdThreadAgentIsThreadBound(t *testing.T) {
	daemon := newMockDaemon()
	daemon.enforceActiveStatusScoping = true
	daemon.mu.Lock()
	daemon.beads["thread-1793204818-241769"] = &beadsapi.BeadDetail{ID: "gd-Gt6fWu2aZ9", Type: "agent", Status: "closed", Fields: map[string]string{
		"agent": "thread-1793204818-241769", "project": "gardenio", "role": "crew",
		"slack_spawn_channel":  "C-research",
		"slack_thread_channel": "C-research", "slack_thread_ts": "1793204818.241769",
	}}
	daemon.mu.Unlock()
	bot := newTestBot(daemon, newRouteSlackServer(t))
	bot.channel = "C-default"

	_, resp := getRoute(t, bot, url.Values{"agent": {"thread-1793204818-241769"}})
	if !resp.CacheRefreshed {
		t.Error("cache_refreshed = false, want true")
	}
	if !resp.ThreadBound {
		t.Error("thread_bound = false for a bead with slack_thread_channel/ts, want true")
	}
	if resp.Rule != RouteRuleSpawnChannel || resp.Channel != "C-research" {
		t.Errorf("rule=%s channel=%s", resp.Rule, resp.Channel)
	}
}

// The response says whether the agent is thread-bound and where its card is,
// which can differ from where the rules point.
func TestRouteExplain_ThreadBoundAndCardLocation(t *testing.T) {
	daemon := newMockDaemon()
	bot := newTestBot(daemon, newRouteSlackServer(t))
	bot.channel = "C-default"
	bot.agentProject["t"], bot.agentRole["t"] = "none", "crew"
	bot.agentThreadBound = map[string]bool{"t": true}
	bot.agentCards["t"] = MessageRef{ChannelID: "C-card", Timestamp: "111.222"}

	_, resp := getRoute(t, bot, url.Values{"agent": {"t"}})
	if !resp.ThreadBound || !resp.CardFound || resp.CardChannel != "C-card" || resp.CardTS != "111.222" {
		t.Errorf("thread_bound=%v card=%v %s/%s", resp.ThreadBound, resp.CardFound, resp.CardChannel, resp.CardTS)
	}
	if resp.CardChannelName != "name-C-card" {
		t.Errorf("card_channel_name = %q", resp.CardChannelName)
	}
}

func TestRouteExplain_BadRequests(t *testing.T) {
	bot := newTestBot(newMockDaemon(), newRouteSlackServer(t))
	for _, q := range []url.Values{{}, {"agent": {"a"}, "bead": {"gd-x"}}} {
		if code, _ := getRoute(t, bot, q); code != http.StatusBadRequest {
			t.Errorf("%v: status %d, want 400", q, code)
		}
	}
	mux := http.NewServeMux()
	NewRouteExplainAPI(bot).RegisterRoutes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/slack/route?agent=a", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: status %d, want 405", rec.Code)
	}
}
