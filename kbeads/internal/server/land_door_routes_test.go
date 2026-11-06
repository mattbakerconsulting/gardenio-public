package server

import (
	"encoding/json"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

// Door D.18 (gd-nvvTcZeBZD): the land door reaches exactly the routes in
// land_door_routes.txt. These tests hold the list to the server (each route
// registered and open to a land principal), drive every kd and gb verb the
// list names through the door with a land token, and check that every other
// route is 404 there.

func TestLandDoorRoutes_Parse(t *testing.T) {
	if landDoorRoutesErr != nil {
		t.Fatal(landDoorRoutesErr)
	}
	if len(landDoorRoutes) < 40 {
		t.Fatalf("only %d land door routes parsed", len(landDoorRoutes))
	}
	for _, bad := range []string{
		"GET /v1/beads extra",
		"get /v1/beads",
		"GET /beads",
		"GET /v1/beads\nGET /v1/beads",
	} {
		if _, err := parseLandDoorRoutes(bad); err == nil {
			t.Errorf("parseLandDoorRoutes(%q) accepted it", bad)
		}
	}
	if got, err := parseLandDoorRoutes("# c\n\nGET  /v1/me   # who\n"); err != nil || !got["GET /v1/me"] || len(got) != 1 {
		t.Errorf("comments and spacing: got %v, %v", got, err)
	}
}

// landDoorNeverOpen are the route families the land door must never reach,
// whatever the list says: token issuing, admin, the machine-only hooks and
// embeddings, the daemon's own SSE streams, configs and session gates.
var landDoorNeverOpen = regexp.MustCompile(`^[A-Z]+ /v1/(bridge-tokens|fleet-tokens|auth|admin|hooks|embeddings|events/stream|bus/events|configs|agents/\{id\}/gates)(/|$)`)

// Every listed route is a registered route a land principal may reach, and
// none of them is in a family the door keeps closed.
func TestLandDoorRoutes_RegisteredAndScoped(t *testing.T) {
	patterns := registeredRoutePatterns()
	for route := range landDoorRoutes {
		if !slices.Contains(patterns, route) {
			t.Errorf("land_door_routes.txt lists %q, which registerRoutes does not register", route)
			continue
		}
		if c := scopedRouteClasses[route]; c != classFiltered && c != classScopedSafe {
			t.Errorf("land_door_routes.txt lists %q, which is closed to scoped principals (class %d)", route, c)
		}
		if landDoorNeverOpen.MatchString(route) {
			t.Errorf("land_door_routes.txt lists %q, a token, admin, machine-only, SSE, config or gate route", route)
		}
	}
	// Positive control for the family check.
	for _, r := range []string{"POST /v1/bridge-tokens", "POST /v1/hooks/execute", "GET /v1/events/stream", "POST /v1/agents/{id}/gates/{gate_id}/satisfy", "POST /v1/auth/refresh"} {
		if !landDoorNeverOpen.MatchString(r) {
			t.Errorf("landDoorNeverOpen misses %s", r)
		}
	}
}

// gl speaks the Lands API through the door, so every contract route is
// listed: a verb added to the map and not here would be 404 for every tenant.
func TestLandDoorRoutes_LandsAPIComplete(t *testing.T) {
	for _, p := range registeredRoutePatterns() {
		if strings.Contains(p, " /v1/lands/v1/") && !landDoorRoutes[p] {
			t.Errorf("Lands API route %q is not in land_door_routes.txt", p)
		}
	}
}

// landDoorStep is one request a verb makes.
type landDoorStep struct {
	method, path string
	body         any
	// want is the status the daemon answers; 0 means any 2xx. A non-2xx
	// want records a refusal the handler (not the door) makes.
	want int
	// agent sends the step with the land agent's per-agent token
	// (tokLandAgent) rather than the land principal's: the agent loop's own
	// verbs, which act on the agent's own bead (land_door_agent.go).
	agent bool
	// owner sends the step with the land owner's own land token
	// (tokLandAudOwner): gb plan share is a human's verb, and only the
	// owner tier manages a land's windows (windowManageDenial).
	owner bool
}

// landDoorVerbs are the kd and gb verbs a land agent runs, each as the
// requests it makes (kd recorded through a logging proxy, gb traced in
// gardenio/controller/cmd/gb). cl-1 is a claimlayer task; the steps create
// what they need.
func landDoorVerbs() map[string][]landDoorStep {
	task := map[string]any{"title": "agent task", "type": "task", "labels": []string{"project:claimlayer"}}
	return map[string][]landDoorStep{
		"kd show": {
			{method: "GET", path: "/v1/beads/cl-1"},
			{method: "GET", path: "/v1/beads/cl-1/dependents"},
			{method: "GET", path: "/v1/batch/beads?ids=cl-1"},
			{method: "GET", path: "/v1/beads/cl-1/mentions?limit=200"},
			{method: "GET", path: "/v1/beads/cl-1/mentioned-by?limit=200"},
		},
		"kd list":   {{method: "GET", path: "/v1/beads?status=open&labels=project:claimlayer&limit=5"}},
		"kd search": {{method: "GET", path: "/v1/beads?search=agent&limit=20"}},
		"kd search --semantic": {
			// The fixture runs without an embedding backend.
			{method: "POST", path: "/v1/search/semantic", body: map[string]any{"query": "agent"}, want: http.StatusServiceUnavailable},
		},
		"kd children":     {{method: "GET", path: "/v1/beads/cl-1"}, {method: "GET", path: "/v1/beads/cl-1/dependents"}},
		"kd dep list":     {{method: "GET", path: "/v1/beads/cl-1/dependencies"}},
		"kd comment list": {{method: "GET", path: "/v1/beads/cl-1/comments"}},
		"kd create":       {{method: "POST", path: "/v1/beads", body: task}},
		"kd claim":        {{method: "GET", path: "/v1/beads?assignee=land-agent&status=in_progress"}, {method: "PATCH", path: "/v1/beads/cl-1", body: map[string]any{"assignee": "land-agent", "status": "in_progress"}}},
		"kd update":       {{method: "PATCH", path: "/v1/beads/cl-1", body: map[string]any{"priority": 3}}},
		"kd comment add":  {{method: "POST", path: "/v1/beads/cl-1/comments", body: map[string]any{"text": "progress"}}},
		"kd dep add":      {{method: "POST", path: "/v1/beads/cl-1/dependencies", body: map[string]any{"depends_on_id": "cl-2", "type": "blocks"}}},
		"kd dep remove":   {{method: "DELETE", path: "/v1/beads/cl-1/dependencies?depends_on_id=cl-2&type=blocks"}},
		"kd label add":    {{method: "POST", path: "/v1/beads/cl-1/labels", body: map[string]any{"label": "scratch"}}},
		"kd label remove": {{method: "DELETE", path: "/v1/beads/cl-1/labels/scratch"}},
		"kd close":        {{method: "GET", path: "/v1/beads/cl-2"}, {method: "POST", path: "/v1/beads/cl-2/close", body: map[string]any{}}},
		"kd reopen":       {{method: "PATCH", path: "/v1/beads/cl-2", body: map[string]any{"status": "open"}}},
		"gb thread":       {{method: "GET", path: "/v1/me"}},
		// gb agent start --local's SessionStart and 5-minute heartbeat: on a
		// land door gb sends them as GET /v1/me, and the door records the
		// agent's presence (land_door_presence.go).
		"gb agent start --local": {{method: "GET", path: "/v1/me", agent: true}},
		"gb prime": {
			{method: "GET", path: "/v1/agents-by-name/land-agent"},
			{method: "GET", path: "/v1/agents/roster"},
			{method: "GET", path: "/v1/beads?type=advice&status=open"},
		},
		"gb ready": {{method: "GET", path: "/v1/beads?status=open&labels=project:claimlayer"}},
		"gb news":  {{method: "GET", path: "/v1/beads?status=in_progress"}, {method: "GET", path: "/v1/batch/beads?ids=cl-1"}},
		"gb say": {
			{method: "POST", path: "/v1/beads", body: map[string]any{"title": "status", "type": "message", "labels": []string{"project:claimlayer"}}},
		},
		"gb decision create": {
			// The steward's plan decision to the land's owner (Track B design
			// §4): the agent on its own token, and the land principal
			// (tierLand create). Answering it stays a human's
			// (TestLandDoorRoutes_AgentToken).
			{method: "POST", path: "/v1/beads", body: landDoorDecision, agent: true},
			{method: "POST", path: "/v1/beads", body: landDoorDecision},
		},
		"gb escalation create": {
			{method: "GET", path: "/v1/beads?type=escalation&status=open,in_progress&limit=100"},
			{method: "POST", path: "/v1/beads", body: map[string]any{
				"title": "please merge", "type": "escalation", "kind": "data", "labels": []string{"project:claimlayer"},
				"fields": map[string]any{"operation_type": "manual_operator_action", "operation_spec": map[string]any{"x": 1}},
			}},
			{method: "POST", path: "/v1/beads/cl-1/labels", body: map[string]any{"label": "close-on-escalation-execute"}},
		},
		"gb yield": {{method: "GET", path: "/v1/beads/cl-1"}},
		"gb stop": {
			// gb stop records its wrap-up on, and closes, its own agent bead,
			// so it runs on the agent's per-agent token: a land principal names
			// no agent, and tierLand gets nothing on type agent
			// (TestLandDoorRoutes_AgentToken pins both refusals).
			{method: "GET", path: "/v1/beads?assignee=land-agent&status=in_progress", agent: true},
			{method: "GET", path: "/v1/beads/cl-1/dependencies", agent: true},
			{method: "PATCH", path: "/v1/beads/cl-agent", body: map[string]any{"fields": map[string]any{"stop_requested": "true"}}, agent: true},
			{method: "POST", path: "/v1/beads/cl-agent/close", body: map[string]any{}, agent: true},
		},
		"gb wake": {{method: "GET", path: "/v1/agents-by-name/land-agent"}},
		// The window routes refuse the land principal and the agent in the
		// handler (403, not the door's 404); the owner's land token mints,
		// lists and revokes (TestLandDoorRoutes_Windows cuts it to its land).
		"gb plan share": {
			{method: "POST", path: "/v1/windows", body: map[string]any{"plan_id": doorPlan}, want: http.StatusForbidden},
			{method: "POST", path: "/v1/windows", body: map[string]any{"plan_id": doorPlan}, want: http.StatusForbidden, agent: true},
			{method: "POST", path: "/v1/windows", body: map[string]any{"plan_id": doorPlan}, owner: true},
		},
		"gb plan share list": {
			{method: "GET", path: "/v1/windows?land=claimlayer", want: http.StatusForbidden},
			{method: "GET", path: "/v1/windows?land=claimlayer", owner: true},
		},
		"gb plan share revoke": {
			{method: "DELETE", path: "/v1/windows/" + doorWindow, want: http.StatusForbidden, agent: true},
			{method: "DELETE", path: "/v1/windows/" + doorWindow, owner: true},
		},
		// The cockpit Share panel's preview (PW.4): the owner checks the
		// page a window would show before minting one.
		"Share panel preview": {
			{method: "GET", path: "/v1/windows/preview?plan=" + doorPlan, want: http.StatusForbidden},
			{method: "GET", path: "/v1/windows/preview?plan=" + doorPlan, want: http.StatusForbidden, agent: true},
			{method: "GET", path: "/v1/windows/preview?plan=" + doorPlan, owner: true},
		},
		// gb lands share (LW.5): a window on the whole land, on the same
		// routes with a land for its target; list and revoke are gb plan
		// share's. The owner's land token mints and previews; the land
		// principal and the agent are refused in the handler.
		"gb lands share": {
			{method: "POST", path: "/v1/windows", body: map[string]any{"land": "claimlayer"}, want: http.StatusForbidden},
			{method: "POST", path: "/v1/windows", body: map[string]any{"land": "claimlayer"}, want: http.StatusForbidden, agent: true},
			{method: "POST", path: "/v1/windows", body: map[string]any{"land": "claimlayer"}, owner: true},
		},
		"gb lands share preview": {
			{method: "GET", path: "/v1/windows/preview?land=claimlayer", want: http.StatusForbidden},
			{method: "GET", path: "/v1/windows/preview?land=claimlayer", want: http.StatusForbidden, agent: true},
			{method: "GET", path: "/v1/windows/preview?land=claimlayer", owner: true},
		},
		"gl whoami": {{method: "GET", path: "/v1/lands/v1/lands/claimlayer/whoami"}},
	}
}

// landDoorDecision is the decision gb decision create files.
var landDoorDecision = map[string]any{
	"title": "which?", "type": "decision", "labels": []string{"project:claimlayer"},
	"fields": map[string]any{"prompt": "which?", "options": []map[string]any{{"id": "a", "label": "A"}, {"id": "b", "label": "B"}}},
}

// tokLandAudOwner is the owner of claimlayer (group /project-claimlayer/owners)
// on a token minted for claimlayer, as gb plan share sends it through the door.
const tokLandAudOwner = "landaud-claimlayer-owner-tok"

// doorPlan is a claimlayer plan epic and doorWindow a live window on it.
const (
	doorPlan   = "cl-door-plan"
	doorWindow = "cl-door-window"
)

// seedDoorWindows seeds the land owner's land token, a claimlayer plan, a
// live window on it, and the same in other (ot-door-plan, ot-door-window),
// and each land's type:project bead, a land window's target.
func (f *scopedFixture) seedDoorWindows() {
	f.keycloak.tokens[tokLandAudOwner] = tokenInfo{
		email: "clo@example.com", roles: []string{RoleProjectMember}, azp: testBrowserClient,
		groups: []string{"/project-claimlayer/owners", "/project-other/owners"}, aud: []string{"land-claimlayer"},
	}
	for land, ids := range map[string][2]string{"claimlayer": {doorPlan, doorWindow}, "other": {"ot-door-plan", "ot-door-window"}} {
		f.seedTyped(ids[0], model.TypeEpic, "project:"+land)
		f.seedTyped(ids[1], typeWindow, "project:"+land)
		f.ms.beads[ids[1]].Kind = model.KindConfig
		f.ms.beads[ids[1]].Fields = []byte(`{"target":"` + ids[0] + `","land":"` + land + `","token_hash":"x","expires_at":"2099-01-01T00:00:00Z","created_by":"x"}`)
		proj := "proj-door-" + land
		f.seedTyped(proj, "project", "")
		f.ms.beads[proj].Title, f.ms.beads[proj].Kind = land, model.KindConfig
	}
}

// tokLandAgent is the per-agent token of claimlayer's agent bead cl-agent, as
// the controller mints it: fleet agent-<bead id>, bound to the project.
// tokLandAgent2 is its sibling cl-agent2's.
const (
	tokLandAgent  = "agent-claimlayer-cl-agent-tok"
	tokLandAgent2 = "agent-claimlayer-cl-agent2-tok"
)

// seedDoorAgents seeds claimlayer as a tenant land with its own-machine
// agent beads cl-agent (agent land-agent) and cl-agent2, and their per-agent
// tokens.
func (f *scopedFixture) seedDoorAgents() {
	f.seedTenantLand("claimlayer", nil)
	f.seedOwnMachineAgent("cl-agent", "land-agent", "claimlayer", tokLandAgent)
	f.seedOwnMachineAgent("cl-agent2", "other-agent", "claimlayer", tokLandAgent2)
}

// seedOwnMachineAgent seeds agent bead id (agent name) in land as the Lands
// API's MintAgent writes it (isLandAgent), and registers its per-agent token.
func (f *scopedFixture) seedOwnMachineAgent(id, name, land, tok string) {
	f.seedLandAgent(id, landsGardenerRole, land, tok)
	b := f.ms.beads[id]
	b.Title = name
	b.Fields, _ = json.Marshal(map[string]string{
		"agent": name, "role": landsGardenerRole, "project": land,
		"execution_target": "laptop", "spawn_source": landAgentSpawnSource,
	})
	b.Labels = []string{"project:" + land, "execution_target:laptop"}
	f.ms.labels[id] = b.Labels
}

// landDoorMux matches a request to its land_door_routes.txt pattern the way
// the daemon's mux does.
func landDoorMux(t *testing.T) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	for route := range landDoorRoutes {
		mux.HandleFunc(route, func(http.ResponseWriter, *http.Request) {})
	}
	return mux
}

// The CI test the task asks for: every kd and gb verb a land agent runs goes
// through the land door with a land token and is answered by its handler, and
// every agent-loop route on the list is driven by some verb (the Lands API
// routes are gl's, held complete by TestLandDoorRoutes_LandsAPIComplete).
func TestLandDoorRoutes_AgentVerbs(t *testing.T) {
	f := newScopedFixture(t)
	f.seedTyped("cl-2", model.TypeTask, "project:claimlayer")
	f.seedDoorAgents()
	f.seedDoorWindows()
	mux := landDoorMux(t)

	used := map[string]bool{}
	verbs := landDoorVerbs()
	names := make([]string, 0, len(verbs))
	for name := range verbs {
		names = append(names, name)
	}
	sort.Strings(names) // kd create and kd dep add run before kd dep remove
	for _, name := range names {
		for _, s := range verbs[name] {
			req, _ := http.NewRequest(s.method, s.path, nil)
			_, pattern := mux.Handler(req)
			if pattern == "" {
				t.Errorf("%s: %s %s is on no land_door_routes.txt route", name, s.method, s.path)
				continue
			}
			used[pattern] = true
			tok := tokLandClaimlayer
			switch {
			case s.agent:
				tok = tokLandAgent
			case s.owner:
				tok = tokLandAudOwner
			}
			code, body := f.landDo(t, tok, "claimlayer", s.method, s.path, s.body)
			ok := code == s.want || (s.want == 0 && code >= 200 && code < 300)
			if !ok || strings.Contains(body, landDoorRouteClosed) {
				t.Errorf("%s: %s %s through the land door: %d %s (want %d)", name, s.method, s.path, code, body, s.want)
			}
		}
	}
	for route := range landDoorRoutes {
		if !strings.Contains(route, " /v1/lands/v1/") && !used[route] {
			t.Errorf("land_door_routes.txt lists %q but no agent verb here uses it", route)
		}
	}

	// The hook routes stay off the door (gd-2iqSr9ANOH): emit takes its
	// actor from the body and runs git and advice hooks in the daemon,
	// publish writes any bus subject, execute runs advice hook commands. A
	// land agent's presence is its own door traffic instead.
	for _, route := range []string{"/v1/hooks/emit", "/v1/hooks/publish", "/v1/hooks/execute"} {
		body := map[string]any{"actor": "fleet-crew", "hook_type": "SessionStart", "subject": "hooks.x", "payload": map[string]any{}}
		if code, got := f.landDo(t, tokLandAgent, "claimlayer", http.MethodPost, route, body); code != http.StatusNotFound || !strings.Contains(got, landDoorRouteClosed) {
			t.Errorf("POST %s through the land door as its agent: %d %s, want the door's 404", route, code, got)
		}
	}
}

// An agent behind the land door (gd-mh9ANzCHIE): its per-agent token passes
// only its own land's door, and through it writes only its own agent bead,
// spawns nothing and answers no decision. The land principal stays without
// any agent-bead write.
func TestLandDoorRoutes_AgentToken(t *testing.T) {
	f := newScopedFixture(t)
	f.seedDoorAgents()
	f.seedTyped("cl-dec", model.TypeDecision, "project:claimlayer")
	const (
		tokUnbound    = "agent-unbound-tok"
		tokPrivileged = "agent-privileged-tok"
		tokOtherAgent = "agent-other-tok"
	)
	f.fc.RegisterTokenFull(tokUnbound, agentFleetPrefix+"cl-agent", "", nil, nil, time.Time{}, "cl-agent", false)
	f.fc.RegisterTokenFull(tokPrivileged, agentFleetPrefix+"cl-agent", "claimlayer", nil, nil, time.Time{}, "cl-agent", true)
	f.seedLandAgent("other-agent-1", "crew", "other", tokOtherAgent)
	// A pod agent of claimlayer (its token could be taken from a pod), and an
	// own-machine agent of an operator land: neither is a tenant's laptop
	// agent, so neither passes a door (gd-Utbyy4xJRX).
	const (
		tokPodAgent     = "agent-claimlayer-pod-tok"
		tokOperatorDoor = "agent-opland-laptop-tok"
	)
	f.seedLandAgent("cl-pod-agent", "crew", "claimlayer", tokPodAgent)
	f.seedTyped("proj-opland", model.TypeProject, "")
	f.ms.beads["proj-opland"].Title, f.ms.beads["proj-opland"].Kind = "opland", model.KindConfig
	f.seedOwnMachineAgent("op-agent", "op-agent", "opland", tokOperatorDoor)
	if code, body := f.landDo(t, tokOperatorDoor, "", http.MethodGet, "/v1/me", nil); code != http.StatusOK {
		t.Fatalf("operator land's agent straight at the daemon: %d %s, want 200", code, body)
	}

	// GET /v1/me is what the edge and landgate ask about an opaque bearer.
	code, body := f.landDo(t, tokLandAgent, "claimlayer", http.MethodGet, "/v1/me", nil)
	var me meJSON
	if err := json.Unmarshal([]byte(body), &me); code != http.StatusOK || err != nil || me.AgentBead != "cl-agent" || me.TokenProject != "claimlayer" {
		t.Errorf("GET /v1/me through claimlayer's door as its agent: %d %s, want 200 naming cl-agent in claimlayer", code, body)
	}
	for _, c := range []struct{ tok, door, why string }{
		{tokLandAgent, "other", "another land's door"},
		{tokOtherAgent, "claimlayer", "an agent of another land"},
		{tokUnbound, "claimlayer", "an agent token bound to no project"},
		{tokPrivileged, "claimlayer", "a privileged agent token"},
		{tokPodAgent, "claimlayer", "a pod agent's token"},
		{tokOperatorDoor, "opland", "an agent token bound to an operator land"},
	} {
		if code, body := f.landDo(t, c.tok, c.door, http.MethodGet, "/v1/me", nil); code != http.StatusForbidden || !strings.Contains(body, "land door:") {
			t.Errorf("%s: GET /v1/me through %s's door: %d %s, want the land door's 403", c.why, c.door, code, body)
		}
	}

	for _, s := range []struct {
		landDoorStep
		tok, why string
	}{
		{landDoorStep{method: "PATCH", path: "/v1/beads/cl-agent2", body: map[string]any{"fields": map[string]any{"stop_requested": "true"}}}, tokLandAgent, "own agent bead"},
		{landDoorStep{method: "POST", path: "/v1/beads/cl-agent2/close", body: map[string]any{}}, tokLandAgent, "own agent bead"},
		{landDoorStep{method: "POST", path: "/v1/beads/cl-agent2/comments", body: map[string]any{"text": "x"}}, tokLandAgent, "own agent bead"},
		{landDoorStep{method: "POST", path: "/v1/beads/cl-agent2/labels", body: map[string]any{"label": "x"}}, tokLandAgent, "own agent bead"},
		{landDoorStep{method: "POST", path: "/v1/beads", body: map[string]any{
			"title": "child", "type": "agent", "labels": []string{"project:claimlayer"},
			"fields": map[string]any{"agent": "child", "role": "crew", "project": "claimlayer"},
		}}, tokLandAgent, "spawns no agent"},
		{landDoorStep{method: "PATCH", path: "/v1/beads/cl-agent", body: map[string]any{"fields": map[string]any{"role": "infra"}}}, tokLandAgent, "spawns no agent"},
		// Answering a decision stays a human's: the agent's token
		// (decisionResolverAllowed; its bare close only withdraws) and the
		// land principal (tierLand has no update or close on decisions) are
		// both refused.
		{landDoorStep{method: "PATCH", path: "/v1/beads/cl-dec", body: map[string]any{"fields": map[string]any{"chosen": "a"}}}, tokLandAgent, "cannot answer a decision"},
		{landDoorStep{method: "POST", path: "/v1/beads/cl-dec/close", body: map[string]any{}}, tokLandClaimlayer, "may not close"},
		{landDoorStep{method: "PATCH", path: "/v1/beads/cl-dec", body: map[string]any{"fields": map[string]any{"chosen": "a"}}}, tokLandClaimlayer, "may not update"},
		// No blanket agent rights for tierLand: one land process must not
		// stop every agent in the land.
		{landDoorStep{method: "PATCH", path: "/v1/beads/cl-agent", body: map[string]any{"fields": map[string]any{"stop_requested": "true"}}}, tokLandClaimlayer, "may not update beads of type agent"},
		{landDoorStep{method: "POST", path: "/v1/beads/cl-agent/close", body: map[string]any{}}, tokLandClaimlayer, "may not close beads of type agent"},
	} {
		if code, body := f.landDo(t, s.tok, "claimlayer", s.method, s.path, s.body); code != http.StatusForbidden || !strings.Contains(body, s.why) {
			t.Errorf("%s %s through the land door as %s: %d %s, want 403 (%s)", s.method, s.path, s.tok, code, body, s.why)
		}
	}
	if st := f.ms.beads["cl-agent2"].Status; st != model.StatusOpen {
		t.Errorf("sibling agent bead cl-agent2 is %s after the refused writes", st)
	}
}

// landDoorNoAuthRoutes are the routes AuthMiddleware serves without a
// credential: the refresh-token exchange, runner registration and the health
// probes.
var landDoorNoAuthRoutes = map[string]bool{
	"POST /v1/auth/refresh":     true,
	"GET /v1/health":            true,
	"POST /v1/runners/register": true,
	"GET /v1/health/live":       true,
}

// concretePath fills a mux pattern's wildcards with a placeholder segment.
func concretePath(pattern string) string {
	return regexp.MustCompile(`\{[^}]+\}`).ReplaceAllString(pattern, "x")
}

// Every registered route the list does not name is 404 through the land
// door, for the land's own token, before its handler runs. Straight at the
// daemon the same token gets the route's ordinary answer.
func TestLandDoorRoutes_UnlistedAre404(t *testing.T) {
	f := newScopedFixture(t)
	n := 0
	for _, p := range registeredRoutePatterns() {
		if landDoorRoutes[p] {
			continue
		}
		method, path, _ := strings.Cut(p, " ")
		path = concretePath(path)
		code, body := f.landDo(t, tokLandClaimlayer, "claimlayer", method, path, map[string]any{})
		if landDoorNoAuthRoutes[p] {
			// No credential is read on these, so landDoorMiddleware refuses
			// the door before the mux (the edge 404s them first).
			if code != http.StatusForbidden || !strings.Contains(body, "land door:") {
				t.Errorf("%s through the land door: %d %s, want the land door's 403", p, code, body)
			}
			continue
		}
		if code != http.StatusNotFound || !strings.Contains(body, landDoorRouteClosed) {
			t.Errorf("%s (as %s %s) through the land door: %d %s, want 404", p, method, path, code, body)
		}
		n++
	}
	if n < 100 {
		t.Fatalf("only %d unlisted routes checked", n)
	}
	// Positive control: without the door the same token reaches the route
	// gate, which refuses an admin-only route with 403, not the door's 404.
	if code, body := f.landDo(t, tokLandClaimlayer, "", http.MethodGet, "/v1/configs", nil); code != http.StatusForbidden || strings.Contains(body, landDoorRouteClosed) {
		t.Errorf("GET /v1/configs straight at the daemon: %d %s, want the route gate's 403", code, body)
	}
}

// The cross-land negative (10.6) on the agent's routes: through its own door
// the land token sees nothing of another land, and through another land's
// door it is refused outright.
func TestLandDoorRoutes_CrossLand(t *testing.T) {
	f := newScopedFixture(t)
	if code, _ := f.landDo(t, tokLandClaimlayer, "claimlayer", http.MethodGet, "/v1/beads/"+beadOther, nil); code != http.StatusForbidden && code != http.StatusNotFound {
		t.Errorf("GET another land's bead through claimlayer's door: %d, want refused", code)
	}
	if code, _ := f.landDo(t, tokLandClaimlayer, "claimlayer", http.MethodPost, "/v1/beads/"+beadOther+"/comments", map[string]any{"text": "x"}); code < 400 {
		t.Errorf("comment on another land's bead through claimlayer's door: %d, want refused", code)
	}
	if code, ids := f.landListIDs(t, tokLandClaimlayer, "claimlayer", "/v1/beads?limit=100"); code != http.StatusOK || !slices.Equal(ids, []string{beadClaimlayer}) {
		t.Errorf("list through claimlayer's door: %d %v, want only %s", code, ids, beadClaimlayer)
	}
	for _, s := range []landDoorStep{
		{method: "GET", path: "/v1/beads"},
		{method: "GET", path: "/v1/beads/" + beadOther},
		{method: "POST", path: "/v1/beads", body: map[string]any{"title": "x", "type": "task", "labels": []string{"project:other"}}},
		{method: "GET", path: "/v1/lands/v1/lands/other/whoami"},
	} {
		if code, body := f.landDo(t, tokLandClaimlayer, "other", s.method, s.path, s.body); code != http.StatusForbidden {
			t.Errorf("%s %s through other's door with claimlayer's token: %d %s, want 403", s.method, s.path, code, body)
		}
	}
}

// gb plan share through the land door (gd-mDDACEAhzV): the owner's land
// token manages and previews (gd-lYiXMouRdM) its own land's windows, on a
// plan or on the whole land (gb lands share, LW.5 gd-aUQRspcbeS), and nothing
// of another land's, even one it also owns, and the window bearer's view
// route is not on the door.
func TestLandDoorRoutes_Windows(t *testing.T) {
	f := newScopedFixture(t)
	f.seedDoorWindows()

	code, body := f.landDo(t, tokLandAudOwner, "claimlayer", http.MethodPost, "/v1/windows", map[string]any{"plan_id": doorPlan})
	var minted createWindowResponse
	if err := json.Unmarshal([]byte(body), &minted); code != http.StatusCreated || err != nil || minted.Window.Land != "claimlayer" || minted.Token == "" {
		t.Fatalf("mint on claimlayer's plan through its door as its owner: %d %s, want 201", code, body)
	}
	code, body = f.landDo(t, tokLandAudOwner, "claimlayer", http.MethodGet, "/v1/windows?land=claimlayer", nil)
	var list struct{ Windows []windowRecord }
	if err := json.Unmarshal([]byte(body), &list); code != http.StatusOK || err != nil || len(list.Windows) != 2 {
		t.Errorf("list claimlayer's windows through its door: %d %s, want 200 with 2", code, body)
	}
	for _, w := range list.Windows {
		if w.Land != "claimlayer" {
			t.Errorf("list through claimlayer's door returned %s's window %s", w.Land, w.ID)
		}
	}
	if strings.Contains(body, minted.Token) || strings.Contains(body, "token_hash") {
		t.Errorf("list carries a token or its hash: %s", body)
	}
	if code, body := f.landDo(t, tokLandAudOwner, "claimlayer", http.MethodDelete, "/v1/windows/"+minted.Window.ID, nil); code != http.StatusOK || !strings.Contains(body, `"state":"revoked"`) {
		t.Errorf("revoke the minted window through claimlayer's door: %d %s, want 200 revoked", code, body)
	}

	// The preview (PW.4) shows the owner its own plan and mints nothing.
	windows := func() (n int) {
		for _, b := range f.ms.beads {
			if b.Type == typeWindow {
				n++
			}
		}
		return n
	}
	before := windows()
	code, body = f.landDo(t, tokLandAudOwner, "claimlayer", http.MethodGet, "/v1/windows/preview?plan="+doorPlan, nil)
	if code != http.StatusOK || !strings.Contains(body, doorPlan) {
		t.Errorf("preview claimlayer's plan through its door as its owner: %d %s, want 200 naming %s", code, body, doorPlan)
	}
	if strings.Contains(body, "token_hash") {
		t.Errorf("preview carries a token hash: %s", body)
	}
	if n := windows(); n != before {
		t.Errorf("preview through the door minted a window: %d windows, was %d", n, before)
	}

	// A window on the whole land (LW.5, gb lands share and the land's
	// Share tab): the owner previews, mints, lists and revokes one on its
	// own land through its door, on the same routes.
	code, body = f.landDo(t, tokLandAudOwner, "claimlayer", http.MethodGet, "/v1/windows/preview?land=claimlayer", nil)
	if code != http.StatusOK || !strings.Contains(body, `"land":"claimlayer"`) || strings.Contains(body, "token_hash") {
		t.Errorf("preview claimlayer's land through its door as its owner: %d %s, want 200 naming the land", code, body)
	}
	if n := windows(); n != before {
		t.Errorf("a land preview through the door minted a window: %d windows, was %d", n, before)
	}
	code, body = f.landDo(t, tokLandAudOwner, "claimlayer", http.MethodPost, "/v1/windows", map[string]any{"land": "claimlayer", "expires_in_days": 7})
	var landMinted createWindowResponse
	if err := json.Unmarshal([]byte(body), &landMinted); code != http.StatusCreated || err != nil || landMinted.Window.TargetKind != windowTargetLand || landMinted.Window.Land != "claimlayer" || landMinted.Window.PlanID != "" || landMinted.Token == "" {
		t.Fatalf("mint on claimlayer's land through its door as its owner: %d %s, want 201 a land window", code, body)
	}
	code, body = f.landDo(t, tokLandAudOwner, "claimlayer", http.MethodGet, "/v1/windows?land=claimlayer", nil)
	if code != http.StatusOK || !strings.Contains(body, `"id":"`+landMinted.Window.ID+`","target_kind":"land"`) || strings.Contains(body, landMinted.Token) {
		t.Errorf("list claimlayer's windows through its door: %d %s, want its land window, without the token", code, body)
	}
	if code, body := f.landDo(t, tokLandAudOwner, "claimlayer", http.MethodDelete, "/v1/windows/"+landMinted.Window.ID, nil); code != http.StatusOK || !strings.Contains(body, `"state":"revoked"`) {
		t.Errorf("revoke the land window through claimlayer's door: %d %s, want 200 revoked", code, body)
	}

	// The token also owns other, but was minted for claimlayer: other's
	// plan, list and window are cut away through claimlayer's door, and
	// other's door refuses the token outright.
	for _, s := range []struct {
		landDoorStep
		door string
	}{
		{landDoorStep{method: "POST", path: "/v1/windows", body: map[string]any{"plan_id": "ot-door-plan"}, want: http.StatusNotFound}, "claimlayer"},
		{landDoorStep{method: "GET", path: "/v1/windows?land=other", want: http.StatusForbidden}, "claimlayer"},
		{landDoorStep{method: "DELETE", path: "/v1/windows/ot-door-window", want: http.StatusNotFound}, "claimlayer"},
		{landDoorStep{method: "GET", path: "/v1/windows?land=claimlayer", want: http.StatusForbidden}, "other"},
		{landDoorStep{method: "POST", path: "/v1/windows", body: map[string]any{"plan_id": "ot-door-plan"}, want: http.StatusForbidden}, "other"},
		{landDoorStep{method: "GET", path: "/v1/windows/preview?plan=ot-door-plan", want: http.StatusNotFound}, "claimlayer"},
		{landDoorStep{method: "GET", path: "/v1/windows/preview?plan=ot-door-plan", want: http.StatusForbidden}, "other"},
		{landDoorStep{method: "POST", path: "/v1/windows", body: map[string]any{"land": "other"}, want: http.StatusNotFound}, "claimlayer"},
		{landDoorStep{method: "GET", path: "/v1/windows/preview?land=other", want: http.StatusNotFound}, "claimlayer"},
		{landDoorStep{method: "POST", path: "/v1/windows", body: map[string]any{"land": "claimlayer"}, want: http.StatusForbidden}, "other"},
		{landDoorStep{method: "GET", path: "/v1/windows/preview?land=claimlayer", want: http.StatusForbidden}, "other"},
	} {
		if code, body := f.landDo(t, tokLandAudOwner, s.door, s.method, s.path, s.body); code != s.want {
			t.Errorf("%s %s through %s's door as claimlayer's owner: %d %s, want %d", s.method, s.path, s.door, code, body, s.want)
		}
	}
	if f.ms.beads["ot-door-window"].Status != model.StatusOpen {
		t.Error("other's window was revoked through claimlayer's door")
	}

	// A plain member's land token manages no windows; the view route is
	// the door's 404 whoever asks.
	f.keycloak.tokens["landaud-claimlayer-member-tok"] = tokenInfo{
		email: "clw@example.com", roles: []string{RoleProjectMember}, azp: testBrowserClient,
		groups: []string{"/project-claimlayer"}, aud: []string{"land-claimlayer"},
	}
	if code, body := f.landDo(t, "landaud-claimlayer-member-tok", "claimlayer", http.MethodPost, "/v1/windows", map[string]any{"plan_id": doorPlan}); code != http.StatusForbidden {
		t.Errorf("mint through the door as a member: %d %s, want 403", code, body)
	}
	if code, body := f.landDo(t, "landaud-claimlayer-member-tok", "claimlayer", http.MethodGet, "/v1/windows/preview?plan="+doorPlan, nil); code != http.StatusForbidden {
		t.Errorf("preview through the door as a member: %d %s, want 403", code, body)
	}
	if code, body := f.landDo(t, "landaud-claimlayer-member-tok", "claimlayer", http.MethodPost, "/v1/windows", map[string]any{"land": "claimlayer"}); code != http.StatusForbidden {
		t.Errorf("land window mint through the door as a member: %d %s, want 403", code, body)
	}
	if code, body := f.landDo(t, "landaud-claimlayer-member-tok", "claimlayer", http.MethodGet, "/v1/windows/preview?land=claimlayer", nil); code != http.StatusForbidden {
		t.Errorf("land preview through the door as a member: %d %s, want 403", code, body)
	}
	if code, body := f.landDo(t, tokLandAudOwner, "claimlayer", http.MethodGet, "/v1/windows/self/view", nil); code != http.StatusNotFound || !strings.Contains(body, landDoorRouteClosed) {
		t.Errorf("GET /v1/windows/self/view through the door: %d %s, want the door's 404", code, body)
	}
}
