package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

// The cross-land negative matrix, daemon half (Lands Track B step 10.6,
// gd-USqiG2sdPU; spec §5.6 "Proving the fence"). Two tenant lands: A is
// claimlayer, B is bravo. Every route registerRoutes registers is called as
// each of A's identities, with every path parameter filled with one of B's
// beads, B's land or B's member, through the bare daemon and through A's land
// door. Every call must hold four things:
//
//   - never data: no response body carries crossLandMarker, which is in every
//     one of B's beads (title, description, fields and comments);
//   - never a write: B's beads, labels, comments and dependencies are the same
//     after the call as before it, and no new bead lands in B;
//   - never a 2xx on a route that names B's bead or land in its path;
//   - never an existence oracle: on such a route, the answer for B's bead is
//     the answer for a bead that does not exist (the same call with every one
//     of B's values swapped for a missing one, crossLandMissing), status and
//     body alike. The spec's "404, never 403" is this rule: a 403 for B's bead
//     beside a 404 for a missing one tells A that B's bead exists. The routes
//     that still differ are pinned in crossLandExistenceOracles; the list may
//     only shrink (a stale entry fails the test).
//
// The positive control (TestCrossLandMatrix_ControlReachesB) runs the reads
// in crossLandControlReads as B's own member and owner and requires B's
// marker back: a walk whose fills never reached B's data would pass the
// negatives for nothing.
//
// The second walk, a write on A's own bead whose body or query names B's
// bead, is TestCrossLandMatrix_OwnBeadNamesB (cross_land_matrix_body_test.go).
//
// What the daemon alone cannot show (GitHub tokens, LiteLLM spend, the pod
// network, Keycloak's realm defaults) is in the live script the 10.10 check-in
// runs, gardenio/scripts/lands-fence-matrix.sh.

const (
	crossLandA      = "claimlayer"
	crossLandB      = "bravo"
	crossLandMarker = "BRAVO-FENCE-MARKER"

	tokBravoMember = "bravo-member-tok"
	tokBravoOwner  = "bravo-owner-tok"
	tokLandBravo   = "land-bravo-tok"
	tokBravoAgent  = "bravo-agent-tok"
	tokClaimAgent  = "claimlayer-agent-tok"
	tokClaimLaptop = "claimlayer-laptop-agent-tok"
	bravoEmail     = "bravo@example.com"
)

// B's beads, one per family a path parameter names.
const (
	bvTask       = "bv-task"
	bvDecision   = "bv-decision"
	bvEscalation = "bv-escalation"
	bvRelease    = "bv-release"
	bvPlan       = "bv-plan"
	bvChannel    = "bv-channel"
	bvThread     = "bv-thread"
	bvAgent      = "bv-agent"
	bvAccount    = "acct-bravo"
	bvProject    = "proj-bravo"
)

// crossLandFills maps a path parameter to the value of B's that fills it. The
// {id} parameter is filled per route family by crossLandIDFill.
var crossLandFills = map[string]string{
	"land":        crossLandB,
	"ticket":      bvTask,
	"decision":    bvDecision,
	"escalation":  bvEscalation,
	"release":     bvRelease,
	"plan":        bvPlan,
	"thread":      bvThread,
	"agent":       bvAgent,
	"member":      bravoEmail,
	"plot":        crossLandB,
	"name":        bvAgent,
	"field":       "notes",
	"label":       "project:" + crossLandB,
	"comment_id":  "1",
	"operator_id": "1",
	"principal":   bravoEmail,
	"gate_id":     "decision",
	"chart_id":    "x",
	"slug":        crossLandB,
	"token":       "bravo-device",
	"bridge":      "land-" + crossLandB,
	"fleet":       crossLandB,
	"image":       "evidence.png",
	"key...":      "bravo",
}

// crossLandNamesB are the parameters whose fill names one of B's beads, B's
// land or B's member, so the route's answer must not tell A it exists.
var crossLandNamesB = map[string]bool{
	"id": true, "land": true, "ticket": true, "decision": true, "escalation": true,
	"release": true, "plan": true, "thread": true, "agent": true, "member": true,
	"plot": true, "name": true,
}

// crossLandIDFill picks which of B's beads fills {id} for a route.
func crossLandIDFill(pattern string) string {
	switch {
	case strings.Contains(pattern, "/v1/decisions/"):
		return bvDecision
	case strings.Contains(pattern, "/v1/escalations/"):
		return bvEscalation
	case strings.Contains(pattern, "/v1/releases/"):
		return bvRelease
	case strings.Contains(pattern, "/v1/agents/"):
		return bvAgent
	}
	return bvTask
}

var crossLandParamRe = regexp.MustCompile(`\{([a-z_.]+)\}`)

// crossLandPath splits pattern into its method and a concrete path with every
// parameter filled from B, and reports whether the path names B.
func crossLandPath(pattern string) (method, path string, namesB bool) {
	method, path, _ = strings.Cut(pattern, " ")
	path = crossLandParamRe.ReplaceAllStringFunc(path, func(m string) string {
		name := m[1 : len(m)-1]
		namesB = namesB || crossLandNamesB[name]
		if name == "id" {
			return crossLandIDFill(pattern)
		}
		return crossLandFills[name]
	})
	return method, path, namesB
}

// crossLandBody is the body every write carries: each key a handler might
// read, set to something that would show in B's beads if it were applied.
func crossLandBody() map[string]any {
	return map[string]any{
		"title":           "written from A",
		"description":     "written from A",
		"type":            "task",
		"status":          "closed",
		"labels":          []string{"project:" + crossLandB},
		"label":           "written-from-a",
		"project":         crossLandB,
		"land":            crossLandB,
		"text":            "written from A",
		"body":            "written from A",
		"answer":          "written from A",
		"chosen":          "a",
		"reason":          "written from A",
		"members":         []string{"cl@example.com"},
		"ids":             []string{bvTask},
		"depends_on_id":   bvDecision,
		"agent_bead_id":   bvAgent,
		"parent_bead_id":  bvPlan,
		"target_bead_id":  bvTask,
		"bead_id":         bvTask,
		"bead_ids":        []string{bvTask},
		"source_bead_ids": []string{bvTask},
		"rated_bead_id":   bvTask,
		"target_plan":     bvPlan,
		"plan_id":         bvPlan,
		"fields":          map[string]any{"notes": "written from A", "channel_id": bvChannel},
		"value":           "written from A",
		"result":          "written from A",
	}
}

// crossLandCaller is one of A's identities. door is the land door header it
// carries ("" for the bare daemon).
type crossLandCaller struct {
	name, token, door string
}

func crossLandCallers() []crossLandCaller {
	var out []crossLandCaller
	for _, c := range []crossLandCaller{
		{name: "A member", token: tokScopedWriter},
		{name: "A owner", token: tokScopedOwner},
		{name: "A guest", token: tokScopedGuest},
		{name: "A land principal", token: tokLandClaimlayer},
		{name: "A agent", token: tokClaimAgent},
		{name: "A own-machine agent", token: tokClaimLaptop},
	} {
		out = append(out, c)
		c.name += " via A's door"
		c.door = crossLandA
		out = append(out, c)
	}
	return out
}

// crossLandMissing maps each of B's values in a path or body to one nothing
// carries, longest first so B's land inside its email or label is swapped
// whole.
var crossLandMissing = []string{
	bravoEmail, "nobody@example.com",
	"project:" + crossLandB, "project:zulu",
	"land-" + crossLandB, "land-zulu",
	bvEscalation, "bv-gone-escalation",
	bvDecision, "bv-gone-decision",
	bvRelease, "bv-gone-release",
	bvChannel, "bv-gone-channel",
	bvThread, "bv-gone-thread",
	bvAgent, "bv-gone-agent",
	bvTask, "bv-gone-task",
	bvPlan, "bv-gone-plan",
	crossLandB, "zulu",
}

var (
	crossLandToMissing   = strings.NewReplacer(crossLandMissing...)
	crossLandFromMissing = strings.NewReplacer(crossLandSwap(crossLandMissing)...)
	crossLandNoiseRe     = regexp.MustCompile(`"correlation_id":"[^"]*"`)
)

func crossLandSwap(pairs []string) []string {
	out := make([]string, 0, len(pairs))
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, pairs[i+1], pairs[i])
	}
	return out
}

// crossLandExistenceOracles pins the routes on which A's answer for B's bead
// differs from its answer for a missing one (a 403 beside a 404, a 403 beside
// an empty 200, a type check naming B's bead's type). gd-7APTAIhdC5 drained
// it: every by-id gate now answers a bead the caller cannot see as a missing
// one (beadHiddenFromCaller). Keep it empty; a route added here is a debt to
// drain, and a pinned route that no longer differs fails the test until its
// line goes. None may return B's data or write to B; the walk checks that on
// every route, pinned or not.
var crossLandExistenceOracles = map[string]bool{}

// crossLandControlReads are routes B's own member must read B's marker
// through, so the fills are known to reach B's data.
var crossLandControlReads = []string{
	"GET /v1/beads/{id}",
	"GET /v1/beads/{id}/comments",
	"GET /v1/decisions/{id}",
	"GET /v1/escalations/{id}",
	"GET /v1/batch/beads",
	"GET /v1/lands/v1/lands/{land}/tickets/{ticket}",
}

// crossLandConfigReads are config reads the route walk cannot express (a
// query string) or fills with a harmless key: B's land bearer row and the
// other sensitive namespaces, which the read gate refuses below OIDC admin
// (gd-zdo4DIeGWR). Each must answer A non-2xx.
var crossLandConfigReads = []string{
	"/v1/configs?namespace=bridge_token",
	"/v1/configs?namespace=fleet_token",
	"/v1/configs?namespace=bus.handler",
	"/v1/configs?namespace=bus.handler.",
	"/v1/configs/bridge_token:land-" + crossLandB,
	"/v1/configs/fleet_token:land-" + crossLandB,
	"/v1/configs/bus.handler." + crossLandB,
}

// seedCrossLands seeds A and B as tenant lands. Every one of B's beads carries
// crossLandMarker.
func seedCrossLands(t *testing.T) *scopedFixture {
	t.Helper()
	f := newScopedFixture(t)
	f.seedTenantLand(crossLandA, nil)
	f.seedTenantLand(crossLandB, nil)
	f.ms.beads[bvProject].Description = crossLandMarker
	f.seedLandAgent("cl-agent-1", "steward", crossLandA, tokClaimAgent)
	f.seedOwnMachineAgent("cl-laptop-1", "cl-laptop", crossLandA, tokClaimLaptop)
	f.seedLandAgent(bvAgent, "steward", crossLandB, tokBravoAgent)
	registerLandPrincipal(f.fc, tokLandBravo, crossLandB)
	f.keycloak.tokens[tokBravoMember] = tokenInfo{email: bravoEmail, roles: []string{RoleProjectMember}, groups: []string{"/project-" + crossLandB}, azp: testBrowserClient}
	f.keycloak.tokens[tokBravoOwner] = tokenInfo{email: "bvo@example.com", roles: []string{RoleProjectMember}, groups: []string{"/project-" + crossLandB + "/owners"}, azp: testBrowserClient}

	label := "project:" + crossLandB
	seed := func(id string, typ model.BeadType, kind model.Kind, fields map[string]any) {
		fields["notes"] = crossLandMarker
		raw, _ := json.Marshal(fields)
		f.ms.beads[id] = &model.Bead{
			ID: id, Title: crossLandMarker + " " + id, Description: crossLandMarker,
			Type: typ, Kind: kind, Status: model.StatusOpen, Fleet: "default",
			Fields: raw, Labels: []string{label}, CreatedBy: "oidc:" + bravoEmail,
			CreatedAt: time.Now().Add(-time.Hour), UpdatedAt: time.Now().Add(-time.Hour),
		}
		f.ms.labels[id] = []string{label}
	}
	seed(bvTask, "task", model.KindIssue, map[string]any{})
	seed(bvDecision, model.TypeDecision, model.KindData, map[string]any{"prompt": crossLandMarker, "options": []map[string]any{{"id": "a", "label": crossLandMarker}}})
	seed(bvEscalation, model.TypeEscalation, model.KindData, map[string]any{"operation_type": "github_pr_merge", "operation_spec": map[string]any{}, "context": crossLandMarker})
	seed(bvRelease, model.TypeRelease, model.KindData, map[string]any{"tag": crossLandMarker})
	seed(bvPlan, model.TypeEpic, model.KindIssue, map[string]any{})
	f.ms.labels[bvPlan] = []string{label, "plan"}
	seed(bvChannel, typeChannel, model.KindData, map[string]any{"name": "general", "channel_kind": "general", "members": []string{bravoEmail}})
	seed(bvThread, "message", model.KindData, map[string]any{"text": crossLandMarker, "channel_id": bvChannel})
	seed(bvAccount, model.TypeAccount, model.KindConfig, map[string]any{"litellm_key": crossLandMarker, "total_cost_usd": "12"})
	// The agent bead seedLandAgent left: give it the marker too.
	f.ms.beads[bvAgent].Title = crossLandMarker + " " + bvAgent
	f.ms.beads[bvAgent].Description = crossLandMarker
	f.ms.comments[bvTask] = append(f.ms.comments[bvTask], &model.Comment{ID: 1, BeadID: bvTask, Author: bravoEmail, Text: crossLandMarker, CreatedAt: time.Now()})
	f.ms.deps[bvTask] = append(f.ms.deps[bvTask], &model.Dependency{BeadID: bvTask, DependsOnID: bvDecision, Type: "blocks"})
	// B's persisted bearers and a bus handler, as the daemon stores them.
	for _, k := range []string{"bridge_token:land-" + crossLandB, "fleet_token:land-" + crossLandB, "bus.handler." + crossLandB} {
		f.ms.configs[k] = &model.Config{Key: k, Value: json.RawMessage(`{"token":"` + crossLandMarker + `"}`)}
	}
	return f
}

// crossLandDo is landDo with a deadline: a stream route (the SSE stream, a
// window's live feed) never ends, so the call keeps whatever arrived within
// crossLandStreamWait and returns it as the body.
func crossLandDo(t *testing.T, f *scopedFixture, token, door, method, path string, body any) (int, string) {
	t.Helper()
	return crossLandDoHeaders(t, f, token, door, method, path, body, nil)
}

// crossLandDoHeaders is crossLandDo with extra request headers.
func crossLandDoHeaders(t *testing.T, f *scopedFixture, token, door, method, path string, body any, headers map[string]string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), crossLandStreamWait)
	defer cancel()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, f.ts.URL+path, r)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if door != "" {
		req.Header.Set(LandDoorHeader, door)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body) // a deadline mid-stream keeps the bytes read so far
	return resp.StatusCode, string(b)
}

// crossLandStreamWait bounds one call of the walk; only a stream route is
// still talking when it runs out.
const crossLandStreamWait = 2 * time.Second

// crossLandSnapshot renders B's beads, labels, comments and dependencies, and
// every bead labelled for B, so a write reaching B shows as a diff.
func crossLandSnapshot(f *scopedFixture) string {
	f.ms.beadsMu.Lock()
	defer f.ms.beadsMu.Unlock()
	type row struct {
		Bead     *model.Bead
		Labels   []string
		Comments []*model.Comment
		Deps     []*model.Dependency
	}
	var ids []string
	for id := range f.ms.beads {
		if strings.HasPrefix(id, "bv-") || id == bvAccount || id == bvProject || slices.Contains(f.ms.labels[id], "project:"+crossLandB) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	rows := map[string]row{}
	for _, id := range ids {
		rows[id] = row{Bead: f.ms.beads[id], Labels: f.ms.labels[id], Comments: f.ms.comments[id], Deps: f.ms.deps[id]}
	}
	raw, _ := json.Marshal(rows)
	return string(raw)
}

func TestCrossLandMatrix(t *testing.T) {
	patterns := registeredRoutePatterns()
	f := seedCrossLands(t)
	before := crossLandSnapshot(f)

	oracles := map[string]bool{}
	for _, c := range crossLandCallers() {
		for _, pattern := range patterns {
			method, path, namesB := crossLandPath(pattern)
			var body any
			if method != http.MethodGet && method != http.MethodDelete {
				body = crossLandBody()
			}
			code, resp := crossLandDo(t, f, c.token, c.door, method, path, body)
			where := c.name + " " + method + " " + path

			if strings.Contains(resp, crossLandMarker) {
				t.Errorf("%s: %d, the body carries B's data: %.300s", where, code, resp)
			}
			if after := crossLandSnapshot(f); after != before {
				t.Errorf("%s: %d, B's beads changed:\nbefore %s\nafter  %s", where, code, before, after)
				f = seedCrossLands(t)
				before = crossLandSnapshot(f)
				continue
			}
			if !namesB {
				continue
			}
			if code/100 == 2 {
				t.Errorf("%s: %d on a route naming B, want it answered as a missing bead is: %.300s", where, code, resp)
				continue
			}
			var missingBody any
			if body != nil {
				raw, _ := json.Marshal(body)
				_ = json.Unmarshal([]byte(crossLandToMissing.Replace(string(raw))), &missingBody)
			}
			mCode, mResp := crossLandDo(t, f, c.token, c.door, method, crossLandToMissing.Replace(path), missingBody)
			got := crossLandNoiseRe.ReplaceAllString(resp, "")
			missing := crossLandNoiseRe.ReplaceAllString(crossLandFromMissing.Replace(mResp), "")
			if code != mCode || got != missing {
				oracles[pattern] = true
				if !crossLandExistenceOracles[pattern] {
					t.Errorf("%s: B's bead answers %d %.200s\n  a missing one answers %d %.200s\n  so A can tell B's bead exists (or pin %q in crossLandExistenceOracles)",
						where, code, got, mCode, missing, pattern)
				}
			}
		}
	}
	for _, c := range crossLandCallers() {
		for _, path := range crossLandConfigReads {
			code, resp := crossLandDo(t, f, c.token, c.door, http.MethodGet, path, nil)
			if code/100 == 2 || strings.Contains(resp, crossLandMarker) {
				t.Errorf("%s GET %s: %d %.300s, want it refused without B's row", c.name, path, code, resp)
			}
		}
	}
	for pattern := range crossLandExistenceOracles {
		if !oracles[pattern] {
			t.Errorf("crossLandExistenceOracles pins %q, which now answers B's bead as a missing one: delete its line", pattern)
		}
	}
}

// The positive control: B's own member and owner read B's marker through the
// routes in crossLandControlReads, with the same fills.
func TestCrossLandMatrix_ControlReachesB(t *testing.T) {
	f := seedCrossLands(t)
	for _, tok := range []string{tokBravoMember, tokBravoOwner} {
		for _, pattern := range crossLandControlReads {
			method, path, _ := crossLandPath(pattern)
			if pattern == "GET /v1/batch/beads" {
				path += "?ids=" + bvTask
			}
			code, resp := crossLandDo(t, f, tok, "", method, path, nil)
			if code != http.StatusOK || !strings.Contains(resp, crossLandMarker) {
				t.Errorf("%s %s %s: %d %.300s, want 200 with B's marker", tok, method, path, code, resp)
			}
		}
	}
}

// A tenant agent of A, in its own land, creates and updates only the types
// landsTenantWrites lists: every other known type, and one no registry knows,
// is refused (gd-bRLiEzvMsy). An update is refused with the lands_tenant
// denial; a create may meet the type's field validation first (it runs before
// authz), so it is held only to writing nothing. A listed type may still be
// refused by its own gate (agent is opSpawn, escalation is opEscalationFile);
// that refusal is not this one.
func TestCrossLandMatrix_TenantAgentWritesOnlyAllowedTypes(t *testing.T) {
	const refusal = "a tenant agent writes only its land's work"
	f := seedCrossLands(t)
	makeTierRoleBead(f.ms, "gd-role-steward", landsStewardRole, `{"capability_tiers":["lands-tenant"]}`)
	label := "project:" + crossLandA
	types := append(allWritableTypeCandidates(), "reaction-rule", "widget-type", "dashboard", "custom-type-added-later")
	for _, typ := range types {
		code, resp := f.do(t, tokClaimAgent, http.MethodPost, "/v1/beads", map[string]any{
			"title": "tenant " + string(typ), "type": typ, "kind": "data", "labels": []string{label},
			"fields": map[string]any{"project": crossLandA},
		})
		refused := code == http.StatusForbidden && strings.Contains(resp, refusal)
		switch {
		case landsTenantWrites[typ]["create"] && refused:
			t.Errorf("create %s: %d %.200s; want it past the type gate", typ, code, resp)
		case !landsTenantWrites[typ]["create"] && (code/100 == 2 || crossLandHasTitle(f, "tenant "+string(typ)) || (code == http.StatusForbidden && !refused)):
			t.Errorf("create %s: %d %.200s; want it refused by the type gate or before it, writing nothing", typ, code, resp)
		}

		id := "cl-own-" + string(typ)
		f.seedTyped(id, typ, label)
		code, resp = f.do(t, tokClaimAgent, http.MethodPatch, "/v1/beads/"+id, map[string]any{"description": "steered"})
		if refused := code == http.StatusForbidden && strings.Contains(resp, refusal); refused == landsTenantWrites[typ]["update"] {
			t.Errorf("update %s: %d %.200s; want refused=%v", typ, code, resp, !landsTenantWrites[typ]["update"])
		}
		if !landsTenantWrites[typ]["update"] && f.ms.beads[id].Description == "steered" {
			t.Errorf("update %s: refused, but the bead changed", typ)
		}
		// The field routes write past write_policy; the type gate holds there too.
		code, resp = f.do(t, tokClaimAgent, http.MethodPost, "/v1/beads/"+id+"/fields/notes_csv/edit", map[string]any{"op": "append", "element": "steered"})
		if refused := code == http.StatusForbidden && strings.Contains(resp, refusal); refused == landsTenantWrites[typ]["update"] {
			t.Errorf("field edit %s: %d %.200s; want refused=%v", typ, code, resp, !landsTenantWrites[typ]["update"])
		}
	}
}

func crossLandHasTitle(f *scopedFixture, title string) bool {
	for _, b := range f.ms.beads {
		if b.Title == title {
			return true
		}
	}
	return false
}

// The door half of the walk reaches A: A's own-machine agent is admitted
// through A's door, so its negatives above are the fence and not the door's
// refusal. A's pod agent (a steward, whose token lives on a pod) is refused
// at the door on every route, as it is refused at the edge (gd-Utbyy4xJRX).
func TestCrossLandMatrix_DoorAdmitsOwnMachineAgentOnly(t *testing.T) {
	f := seedCrossLands(t)
	if code, resp := crossLandDo(t, f, tokClaimLaptop, crossLandA, http.MethodGet, "/v1/me", nil); code != http.StatusOK || !strings.Contains(resp, `"agent_bead":"cl-laptop-1"`) {
		t.Errorf("A's own-machine agent GET /v1/me through A's door: %d %.300s, want 200 naming cl-laptop-1", code, resp)
	}
	for _, path := range []string{"/v1/me", "/v1/beads?limit=1", "/v1/beads/" + bvTask} {
		if code, resp := crossLandDo(t, f, tokClaimAgent, crossLandA, http.MethodGet, path, nil); code != http.StatusForbidden || !strings.Contains(resp, "land door:") {
			t.Errorf("A's pod agent GET %s through A's door: %d %.300s, want the land door's 403", path, code, resp)
		}
	}
}

// Every parameter a registered route uses has a fill, so a new parameter
// cannot leave a route walking a literal "{x}" path.
func TestCrossLandMatrix_EveryParamFilled(t *testing.T) {
	for _, pattern := range registeredRoutePatterns() {
		for _, m := range crossLandParamRe.FindAllStringSubmatch(pattern, -1) {
			if _, ok := crossLandFills[m[1]]; !ok && m[1] != "id" {
				t.Errorf("%s: no crossLandFills entry for {%s}", pattern, m[1])
			}
		}
	}
}

// A tenant agent reaches only its own gates: another land's agent, and an
// agent that does not exist, both answer 404, and nothing is written. A fleet
// agent (on an operator project) is unchanged.
func TestTenantAgentGatesOwnOnly(t *testing.T) {
	f := seedCrossLands(t)
	f.seedLandAgent("gd-agent-1", "crew", "gardenio", tokGardenioAgent)
	for _, id := range []string{bvAgent, "bv-gone-agent"} {
		for _, c := range []struct{ method, path string }{
			{http.MethodGet, "/v1/agents/" + id + "/gates"},
			{http.MethodPost, "/v1/agents/" + id + "/gates/decision/satisfy"},
			{http.MethodDelete, "/v1/agents/" + id + "/gates/decision"},
		} {
			if code, body := f.do(t, tokClaimAgent, c.method, c.path, nil); code != http.StatusNotFound {
				t.Errorf("A agent %s %s: %d %s, want 404", c.method, c.path, code, body)
			}
		}
	}
	if code, body := f.do(t, tokClaimAgent, http.MethodPost, "/v1/agents/cl-agent-1/gates/decision/satisfy", nil); code != http.StatusOK {
		t.Errorf("A agent satisfies its own gate: %d %s, want 200", code, body)
	}
	if code, body := f.do(t, tokGardenioAgent, http.MethodGet, "/v1/agents/"+bvAgent+"/gates", nil); code != http.StatusOK {
		t.Errorf("fleet agent lists another agent's gates: %d %s, want 200", code, body)
	}
}
