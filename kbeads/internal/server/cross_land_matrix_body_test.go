package server

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/model"
)

// The cross-land matrix's second walk (gd-Mpy0lOLACZ): a write on A's OWN
// bead, land or member whose body (or query) names one of B's beads. Every
// write route, and every route in crossLandQueries, is called as each of A's
// identities with every path parameter filled with A's own value
// (crossLandOwnPath) and crossLandBody naming B. The same call naming a
// missing bead instead (crossLandToMissing over the body and query) must
// answer the same, status and body alike, or A can tell B's bead exists;
// the routes that still differ are pinned in crossLandBodyOracles, which may
// only shrink. Like the first walk, no call may return B's data or change B,
// and no call may leave an edge from A's bead to B's.
//
// The B call and the missing call run against two fixtures seeded alike, in
// lockstep, so an allowed write on A's bead (a close, a comment) leaves both
// in the same state for the next route.

// A's own beads, one per family a path parameter names; A's task is cl-1.
const (
	clDecision   = "cl-decision"
	clEscalation = "cl-escalation"
	clRelease    = "cl-release"
	clPlan       = "cl-plan"
	clChannel    = "cl-channel"
	clThread     = "cl-thread"
	clAgent      = "cl-agent-1"
	clEmail      = "clw@example.com"
)

// crossLandOwnFills maps a path parameter to A's own value. Parameters that
// name nobody keep their crossLandFills value.
var crossLandOwnFills = map[string]string{
	"land":        crossLandA,
	"ticket":      beadClaimlayer,
	"decision":    clDecision,
	"escalation":  clEscalation,
	"release":     clRelease,
	"plan":        clPlan,
	"thread":      clThread,
	"agent":       clAgent,
	"member":      clEmail,
	"plot":        crossLandA,
	"name":        clAgent,
	"label":       "project:" + crossLandA,
	"comment_id":  "1",
	"principal":   clEmail,
	"slug":        crossLandA,
	"token":       "claimlayer-device",
	"bridge":      "land-" + crossLandA,
	"fleet":       crossLandA,
	"key...":      crossLandA,
	"operator_id": "1",
}

// crossLandQueries are the query strings that name B's bead on routes that
// read it from the query rather than the body.
var crossLandQueries = map[string]string{
	"DELETE /v1/beads/{id}/dependencies": "?type=blocks&depends_on_id=" + bvDecision,
	"GET /v1/batch/beads":                "?ids=" + bvTask,
}

// crossLandBodyOracles pins the routes on which a write on A's own bead
// answers B's bead differently from a missing one. Keep it empty; a route
// added here is a debt to drain.
var crossLandBodyOracles = map[string]bool{}

// crossLandOwnIDFill picks which of A's beads fills {id} for a route.
func crossLandOwnIDFill(pattern string) string {
	switch {
	case strings.Contains(pattern, "/v1/decisions/"):
		return clDecision
	case strings.Contains(pattern, "/v1/escalations/"):
		return clEscalation
	case strings.Contains(pattern, "/v1/releases/"):
		return clRelease
	case strings.Contains(pattern, "/v1/agents/"):
		return clAgent
	}
	return beadClaimlayer
}

// crossLandOwnPath splits pattern into its method and a concrete path with
// every parameter filled with A's own value.
func crossLandOwnPath(pattern string) (method, path string) {
	method, path, _ = strings.Cut(pattern, " ")
	path = crossLandParamRe.ReplaceAllStringFunc(path, func(m string) string {
		name := m[1 : len(m)-1]
		if name == "id" {
			return crossLandOwnIDFill(pattern)
		}
		if v, ok := crossLandOwnFills[name]; ok {
			return v
		}
		return crossLandFills[name]
	})
	return method, path
}

// seedCrossLandOwn gives A a bead in each family the path parameters name,
// and a comment on its task, none carrying B's marker.
func seedCrossLandOwn(f *scopedFixture) {
	label := "project:" + crossLandA
	seed := func(id string, typ model.BeadType, kind model.Kind, fields map[string]any) {
		raw, _ := json.Marshal(fields)
		f.ms.beads[id] = &model.Bead{
			ID: id, Title: id, Type: typ, Kind: kind, Status: model.StatusOpen, Fleet: "default",
			Fields: raw, Labels: []string{label}, CreatedBy: "oidc:" + clEmail,
			CreatedAt: time.Now().Add(-time.Hour), UpdatedAt: time.Now().Add(-time.Hour),
		}
		f.ms.labels[id] = []string{label}
	}
	seed(clDecision, model.TypeDecision, model.KindData, map[string]any{"prompt": "p", "options": []map[string]any{{"id": "a", "label": "a"}}})
	seed(clEscalation, model.TypeEscalation, model.KindData, map[string]any{"operation_type": "github_pr_merge", "operation_spec": map[string]any{}, "context": "c"})
	seed(clRelease, model.TypeRelease, model.KindData, map[string]any{"tag": "t"})
	seed(clPlan, model.TypeEpic, model.KindIssue, map[string]any{})
	f.ms.labels[clPlan] = []string{label, "plan"}
	seed(clChannel, typeChannel, model.KindData, map[string]any{"name": "general", "channel_kind": "general", "members": []string{clEmail}})
	seed(clThread, "message", model.KindData, map[string]any{"text": "t", "channel_id": clChannel})
	f.ms.comments[beadClaimlayer] = append(f.ms.comments[beadClaimlayer], &model.Comment{ID: 1, BeadID: beadClaimlayer, Author: clEmail, Text: "c", CreatedAt: time.Now()})
}

// crossLandEdgesToB lists every dependency from a bead outside B onto one of
// B's beads.
func crossLandEdgesToB(f *scopedFixture) []string {
	f.ms.beadsMu.Lock()
	defer f.ms.beadsMu.Unlock()
	var out []string
	for from, deps := range f.ms.deps {
		if strings.HasPrefix(from, "bv-") {
			continue
		}
		for _, d := range deps {
			if strings.HasPrefix(d.DependsOnID, "bv-") || d.DependsOnID == bvAccount || d.DependsOnID == bvProject {
				out = append(out, from+" -> "+d.DependsOnID)
			}
		}
	}
	slices.Sort(out)
	return out
}

// crossLandTimeRe and crossLandNewIDRe match the timestamps a write stamps
// and the ids it mints, which differ between the two fixtures by the clock
// and the random source alone.
var (
	crossLandTimeRe  = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})`)
	crossLandNewIDRe = regexp.MustCompile(`\bgd-[A-Za-z0-9]{10}\b`)
)

func crossLandNormalize(s string) string {
	s = crossLandNoiseRe.ReplaceAllString(s, "")
	s = crossLandTimeRe.ReplaceAllString(s, "T")
	return crossLandNewIDRe.ReplaceAllString(s, "gd-new")
}

func seedCrossLandsOwn(t *testing.T) (b, m *scopedFixture) {
	t.Helper()
	b, m = seedCrossLands(t), seedCrossLands(t)
	seedCrossLandOwn(b)
	seedCrossLandOwn(m)
	return b, m
}

func TestCrossLandMatrix_OwnBeadNamesB(t *testing.T) {
	fb, fm := seedCrossLandsOwn(t)
	before := crossLandSnapshot(fb)

	oracles := map[string]bool{}
	for _, c := range crossLandCallers() {
		for _, pattern := range registeredRoutePatterns() {
			method, path := crossLandOwnPath(pattern)
			query, queried := crossLandQueries[pattern]
			var body any
			if method != http.MethodGet && method != http.MethodDelete {
				body = crossLandBody()
			} else if !queried {
				continue
			}
			code, resp := crossLandDo(t, fb, c.token, c.door, method, path+query, body)
			var missingBody any
			if body != nil {
				raw, _ := json.Marshal(body)
				_ = json.Unmarshal([]byte(crossLandToMissing.Replace(string(raw))), &missingBody)
			}
			mCode, mResp := crossLandDo(t, fm, c.token, c.door, method, path+crossLandToMissing.Replace(query), missingBody)
			where := c.name + " " + method + " " + path + query

			reseed := false
			if strings.Contains(resp, crossLandMarker) {
				t.Errorf("%s: %d, the body carries B's data: %.300s", where, code, resp)
			}
			if after := crossLandSnapshot(fb); after != before {
				t.Errorf("%s: %d, B's beads changed:\nbefore %s\nafter  %s", where, code, before, after)
				reseed = true
			}
			if edges := crossLandEdgesToB(fb); len(edges) > 0 {
				t.Errorf("%s: %d, A's bead now depends on B's: %v", where, code, edges)
				reseed = true
			}
			got := crossLandNormalize(resp)
			missing := crossLandNormalize(crossLandFromMissing.Replace(mResp))
			if code != mCode || got != missing {
				oracles[pattern] = true
				reseed = true
				if !crossLandBodyOracles[pattern] {
					t.Errorf("%s: naming B's bead answers %d %.200s\n  naming a missing one answers %d %.200s\n  so A can tell B's bead exists (or pin %q in crossLandBodyOracles)",
						where, code, got, mCode, missing, pattern)
				}
			}
			if reseed {
				fb, fm = seedCrossLandsOwn(t)
				before = crossLandSnapshot(fb)
			}
		}
	}
	for pattern := range crossLandBodyOracles {
		if !oracles[pattern] {
			t.Errorf("crossLandBodyOracles pins %q, which now answers B's bead as a missing one: delete its line", pattern)
		}
	}
}

// A tenant caller (a scoped member or a tenant agent) links its own bead only
// to beads it can see: B's bead and a missing one both answer 404 with the
// same body, and unlinking either answers {"removed":false} and leaves an
// edge an admin made in place. An unscoped admin still links across lands.
func TestTenantLinksOnlyWhatItSees(t *testing.T) {
	f := seedCrossLands(t)
	seedCrossLandOwn(f)
	link := func(tok, to string) (int, string) {
		return f.do(t, tok, http.MethodPost, "/v1/beads/"+beadClaimlayer+"/dependencies", map[string]any{"depends_on_id": to, "type": "blocks"})
	}
	for _, tok := range []string{tokScopedWriter, tokClaimAgent} {
		code, body := link(tok, bvDecision)
		mCode, mBody := link(tok, "bv-gone-decision")
		if code != http.StatusNotFound || code != mCode || crossLandNormalize(body) != crossLandNormalize(mBody) {
			t.Errorf("%s links cl-1 -> B's bead: %d %s; -> a missing one: %d %s; want both 404 alike", tok, code, body, mCode, mBody)
		}
		if edges := crossLandEdgesToB(f); len(edges) > 0 {
			t.Fatalf("%s: A's bead now depends on B's: %v", tok, edges)
		}
		if code, body := link(tok, clDecision); code != http.StatusCreated {
			t.Errorf("%s links cl-1 -> its own decision: %d %s, want 201", tok, code, body)
		}
		f.ms.deps[beadClaimlayer] = []*model.Dependency{{BeadID: beadClaimlayer, DependsOnID: bvDecision, Type: "blocks"}}
		for _, to := range []string{bvDecision, "bv-gone-decision"} {
			path := "/v1/beads/" + beadClaimlayer + "/dependencies?type=blocks&depends_on_id=" + to
			if code, body := f.do(t, tok, http.MethodDelete, path, nil); code != http.StatusOK || !strings.Contains(body, `"removed":false`) {
				t.Errorf("%s unlinks cl-1 -> %s: %d %s, want 200 removed:false", tok, to, code, body)
			}
		}
		if len(f.ms.deps[beadClaimlayer]) != 1 {
			t.Errorf("%s: a hidden unlink removed the edge: %v", tok, f.ms.deps[beadClaimlayer])
		}
		f.ms.deps[beadClaimlayer] = nil
	}
	if code, body := link(tokOwnerAdmin, bvDecision); code != http.StatusCreated {
		t.Errorf("admin links cl-1 -> B's bead: %d %s, want 201", code, body)
	}
}

// The gRPC twins answer the same: NotFound for a link to B's bead or a
// missing one, removed=false for an unlink of either.
func TestTenantLinksOnlyWhatItSees_GRPC(t *testing.T) {
	f := seedCrossLands(t)
	client := beadsv1.NewBeadsServiceClient(crossLandGRPC(t, f))
	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+tokClaimAgent)
	for _, to := range []string{bvDecision, "bv-gone-decision"} {
		_, err := client.AddDependency(ctx, &beadsv1.AddDependencyRequest{BeadId: beadClaimlayer, DependsOnId: to, Type: "blocks"})
		if status.Code(err) != codes.NotFound || status.Convert(err).Message() != "depends_on bead not found" {
			t.Errorf("A agent AddDependency cl-1 -> %s: %v, want NotFound", to, err)
		}
		resp, err := client.RemoveDependency(ctx, &beadsv1.RemoveDependencyRequest{BeadId: beadClaimlayer, DependsOnId: to, Type: "blocks"})
		if err != nil || resp.GetRemoved() {
			t.Errorf("A agent RemoveDependency cl-1 -> %s: %v %v, want removed=false", to, resp, err)
		}
	}
	if edges := crossLandEdgesToB(f); len(edges) > 0 {
		t.Errorf("A's bead now depends on B's: %v", edges)
	}
}

// A tenant agent cannot move its own bead into another land: a project:
// label naming any project but its token's is refused, set by PATCH or added
// by the label routes, and the labels are unchanged; nor may it create a
// bead carrying one beside its own. Its own land's label and a plain label
// still apply.
func TestTenantAgentLabelsStayInLand(t *testing.T) {
	f := seedCrossLands(t)
	client := beadsv1.NewBeadsServiceClient(crossLandGRPC(t, f))
	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+tokClaimAgent)
	own := []string{"project:" + crossLandA}
	for _, land := range []string{crossLandB, "zulu"} {
		label := "project:" + land
		if code, body := f.do(t, tokClaimAgent, http.MethodPatch, "/v1/beads/"+beadClaimlayer, map[string]any{"labels": []string{label}}); code != http.StatusForbidden {
			t.Errorf("A agent PATCH labels [%s]: %d %s, want 403", label, code, body)
		}
		if code, body := f.do(t, tokClaimAgent, http.MethodPost, "/v1/beads/"+beadClaimlayer+"/labels", map[string]any{"label": label}); code != http.StatusForbidden {
			t.Errorf("A agent adds %s: %d %s, want 403", label, code, body)
		}
		if _, err := client.AddLabel(ctx, &beadsv1.AddLabelRequest{BeadId: beadClaimlayer, Label: label}); status.Code(err) != codes.PermissionDenied {
			t.Errorf("A agent AddLabel %s over gRPC: %v, want PermissionDenied", label, err)
		}
		if got := f.ms.labels[beadClaimlayer]; !slices.Equal(got, own) {
			t.Fatalf("labels after refused %s: %v, want %v", label, got, own)
		}
	}
	// Nor file a bead that carries another project's label beside its own
	// (gd-kwJ0jPMdNY): either order, so intake's first-label pick cannot be
	// steered either.
	for _, land := range []string{crossLandB, "gardenio"} {
		for _, labels := range [][]string{{own[0], "project:" + land}, {"project:" + land, own[0]}} {
			before := map[string]bool{}
			for id := range f.ms.beads {
				before[id] = true
			}
			if code, body := f.do(t, tokClaimAgent, http.MethodPost, "/v1/beads", map[string]any{"title": "x", "type": "task", "labels": labels}); code != http.StatusForbidden {
				t.Errorf("A agent creates with %v: %d %s, want 403", labels, code, body)
			}
			if _, err := client.CreateBead(ctx, &beadsv1.CreateBeadRequest{Title: "x", Type: "task", Labels: labels}); status.Code(err) != codes.PermissionDenied {
				t.Errorf("A agent CreateBead %v over gRPC: %v, want PermissionDenied", labels, err)
			}
			for id, b := range f.ms.beads {
				if !before[id] && b.Type != "audit" { // the denial's own record
					t.Fatalf("a refused create with %v stored %s: type=%s labels=%v title=%q", labels, id, b.Type, b.Labels, b.Title)
				}
			}
		}
	}
	if code, body := f.do(t, tokClaimAgent, http.MethodPost, "/v1/beads", map[string]any{"title": "x", "type": "task", "labels": append(own, "area:x")}); code != http.StatusCreated {
		t.Errorf("A agent creates in its land: %d %s, want 201", code, body)
	}
	if code, body := f.do(t, tokClaimAgent, http.MethodPatch, "/v1/beads/"+beadClaimlayer, map[string]any{"labels": append(own, "area:x")}); code != http.StatusOK {
		t.Errorf("A agent PATCH labels in its land: %d %s, want 200", code, body)
	}
	if code, body := f.do(t, tokClaimAgent, http.MethodPost, "/v1/beads/"+beadClaimlayer+"/labels", map[string]any{"label": "area:y"}); code/100 != 2 {
		t.Errorf("A agent adds a plain label: %d %s, want 2xx", code, body)
	}
}

// A tenant agent cannot strip its bead's project label (gd-U9AV7jG8hF): not
// by a PATCH label set without it, nor by the label-remove routes, so the
// bead neither leaves its land's view nor reads as the fleet's. A plain label
// still comes off.
func TestTenantAgentKeepsItsProjectLabel(t *testing.T) {
	f := seedCrossLands(t)
	client := beadsv1.NewBeadsServiceClient(crossLandGRPC(t, f))
	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+tokClaimAgent)
	own := "project:" + crossLandA
	for _, labels := range [][]string{{}, {"area:x"}} {
		if code, body := f.do(t, tokClaimAgent, http.MethodPatch, "/v1/beads/"+beadClaimlayer, map[string]any{"labels": labels}); code != http.StatusForbidden {
			t.Errorf("A agent PATCH labels %v: %d %s, want 403", labels, code, body)
		}
	}
	if code, body := f.do(t, tokClaimAgent, http.MethodDelete, "/v1/beads/"+beadClaimlayer+"/labels/"+own, nil); code != http.StatusForbidden {
		t.Errorf("A agent removes %s: %d %s, want 403", own, code, body)
	}
	if _, err := client.RemoveLabel(ctx, &beadsv1.RemoveLabelRequest{BeadId: beadClaimlayer, Label: own}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("A agent RemoveLabel %s over gRPC: %v, want PermissionDenied", own, err)
	}
	if got := f.ms.labels[beadClaimlayer]; !slices.Equal(got, []string{own}) {
		t.Fatalf("labels after refused removals: %v, want [%s]", got, own)
	}
	if code, body := f.do(t, tokClaimAgent, http.MethodPost, "/v1/beads/"+beadClaimlayer+"/labels", map[string]any{"label": "area:y"}); code/100 != 2 {
		t.Fatalf("A agent adds a plain label: %d %s, want 2xx", code, body)
	}
	if code, body := f.do(t, tokClaimAgent, http.MethodDelete, "/v1/beads/"+beadClaimlayer+"/labels/area:y", nil); code/100 != 2 {
		t.Errorf("A agent removes a plain label: %d %s, want 2xx", code, body)
	}
}

// Edges from A's own bead whose far end is named in a body field or header
// rather than the path (gd-fgMx8iN5Uh): parent_bead_id on create,
// depends_on_id on a link, and X-Trigger-Bead on a claim, which writes a
// triggered-by edge. Each far end is one of B's beads or a fleet bead.
// X-Trigger-Bead takes only gd-/ka- ids, so those far ends have gd- ids.
const (
	edgeFleetEpic    = "fl-epic"
	edgeBTrigger     = "gd-bvtrigger"
	edgeFleetTrigger = "gd-fltrigger"
	edgeOwnTrigger   = "gd-cltrigger"
)

// crossLandEdgeMissing maps each foreign far end to a missing id.
var crossLandEdgeMissing = map[string]string{
	bvPlan:           "bv-gone-plan",
	bvTask:           "bv-gone-task",
	edgeFleetEpic:    "fl-gone-epic",
	edgeBTrigger:     "gd-bvgonetrg",
	edgeFleetTrigger: "gd-flgonetrg",
}

// crossLandEdgeCell is one edge-writing call from A's bead to far end to.
type crossLandEdgeCell struct {
	name, method, path string
	body               func(to string) map[string]any
	headers            func(to string) map[string]string
	edge               model.DependencyType
	// source names the bead the edge hangs from, "" for the bead the call
	// creates.
	source string
}

var crossLandEdgeCells = []crossLandEdgeCell{
	{
		name: "create parent_bead_id", method: http.MethodPost, path: "/v1/beads",
		body: func(to string) map[string]any {
			return map[string]any{"title": "child", "type": "task", "labels": []string{"project:" + crossLandA}, "parent_bead_id": to}
		},
		edge: model.DepParentChild,
	},
	{
		name: "link blocks", method: http.MethodPost, path: "/v1/beads/" + beadClaimlayer + "/dependencies",
		body: func(to string) map[string]any { return map[string]any{"depends_on_id": to, "type": "blocks"} },
		edge: model.DepBlocks, source: beadClaimlayer,
	},
	{
		name: "link parent-child", method: http.MethodPost, path: "/v1/beads/" + beadClaimlayer + "/dependencies",
		body: func(to string) map[string]any { return map[string]any{"depends_on_id": to, "type": "parent-child"} },
		edge: model.DepParentChild, source: beadClaimlayer,
	},
	{
		name: "claim X-Trigger-Bead", method: http.MethodPatch, path: "/v1/beads/" + beadClaimlayer,
		body:    func(string) map[string]any { return map[string]any{"status": "in_progress", "assignee": "cl-agent-1"} },
		headers: func(to string) map[string]string { return map[string]string{"X-Trigger-Bead": to} },
		edge:    model.DepTriggeredBy, source: beadClaimlayer,
	},
}

// foreignTargets are the far ends a cell is walked with: B's and the fleet's.
func (c crossLandEdgeCell) foreignTargets() []string {
	if c.headers != nil {
		return []string{edgeBTrigger, edgeFleetTrigger}
	}
	if c.source == "" {
		return []string{bvPlan, edgeFleetEpic}
	}
	return []string{bvTask, edgeFleetEpic}
}

func (c crossLandEdgeCell) do(t *testing.T, f *scopedFixture, caller crossLandCaller, to string) (int, string) {
	t.Helper()
	var hdr map[string]string
	if c.headers != nil {
		hdr = c.headers(to)
	}
	return crossLandDoHeaders(t, f, caller.token, caller.door, c.method, c.path, c.body(to), hdr)
}

// seedCrossLandEdges seeds A and B with the far ends the edge cells name: B's
// trigger, a fleet epic and trigger, and A's own trigger.
func seedCrossLandEdges(t *testing.T) *scopedFixture {
	t.Helper()
	f := seedCrossLands(t)
	seedCrossLandOwn(f)
	f.seedTyped(edgeBTrigger, model.TypeTask, "project:"+crossLandB)
	f.ms.beads[edgeBTrigger].Title = crossLandMarker
	f.seedTyped(edgeFleetEpic, model.TypeEpic, "project:gardenio")
	f.seedTyped(edgeFleetTrigger, model.TypeTask, "project:gardenio")
	f.seedTyped(edgeOwnTrigger, model.TypeTask, "project:"+crossLandA)
	return f
}

// crossLandEdgesOnto lists every edge onto one of targets.
func crossLandEdgesOnto(f *scopedFixture, targets ...string) []string {
	f.ms.beadsMu.Lock()
	defer f.ms.beadsMu.Unlock()
	var out []string
	for from, deps := range f.ms.deps {
		for _, d := range deps {
			if slices.Contains(targets, d.DependsOnID) {
				out = append(out, from+" -"+string(d.Type)+"-> "+d.DependsOnID)
			}
		}
	}
	slices.Sort(out)
	return out
}

// Every edge cell, as each of A's identities, pointed at B's bead or a fleet
// bead: no edge lands on it, B is unchanged, and the answer is the answer for
// a missing far end, so A can neither attach its bead to another land's or the
// fleet's nor tell that the bead exists. The fixtures for the foreign and the
// missing call are seeded alike and reseeded per cell.
func TestCrossLandMatrix_EdgeBodyFields(t *testing.T) {
	for _, caller := range crossLandCallers() {
		for _, cell := range crossLandEdgeCells {
			for _, to := range cell.foreignTargets() {
				fb, fm := seedCrossLandEdges(t), seedCrossLandEdges(t)
				before := crossLandSnapshot(fb)
				where := caller.name + " " + cell.name + " -> " + to
				missing := crossLandEdgeMissing[to]

				code, resp := cell.do(t, fb, caller, to)
				mCode, mResp := cell.do(t, fm, caller, missing)
				if edges := crossLandEdgesOnto(fb, to); len(edges) > 0 {
					t.Errorf("%s: %d, an edge now points at the foreign bead: %v", where, code, edges)
				}
				if strings.Contains(resp, crossLandMarker) {
					t.Errorf("%s: %d, the body carries B's data: %.300s", where, code, resp)
				}
				if after := crossLandSnapshot(fb); after != before {
					t.Errorf("%s: %d, B's beads changed:\nbefore %s\nafter  %s", where, code, before, after)
				}
				got := crossLandNormalize(resp)
				want := crossLandNormalize(strings.ReplaceAll(mResp, missing, to))
				if code != mCode || got != want {
					t.Errorf("%s: answers %d %.200s\n  a missing far end answers %d %.200s\n  so A can tell the bead exists", where, code, got, mCode, want)
				}
			}
		}
	}
}

// The positive control: the same cells pointed at A's own bead write the
// edge, so the walk above reaches the code that writes it. X-Trigger-Bead is
// honoured only on a per-agent token, so its cell runs as A's agent alone.
func TestCrossLandMatrix_EdgeBodyFieldsControl(t *testing.T) {
	for _, tok := range []string{tokScopedWriter, tokClaimAgent} {
		for _, cell := range crossLandEdgeCells {
			if cell.headers != nil && tok != tokClaimAgent {
				continue
			}
			to := clDecision
			switch {
			case cell.headers != nil:
				to = edgeOwnTrigger
			case cell.source == "":
				to = clPlan
			}
			f := seedCrossLandEdges(t)
			code, resp := cell.do(t, f, crossLandCaller{token: tok}, to)
			if code/100 != 2 {
				t.Errorf("%s %s -> %s: %d %.300s, want 2xx", tok, cell.name, to, code, resp)
				continue
			}
			edges := crossLandEdgesOnto(f, to)
			if len(edges) != 1 || !strings.Contains(edges[0], "-"+string(cell.edge)+"->") {
				t.Errorf("%s %s -> %s: edges %v, want one %s edge", tok, cell.name, to, edges, cell.edge)
			}
		}
	}
}
