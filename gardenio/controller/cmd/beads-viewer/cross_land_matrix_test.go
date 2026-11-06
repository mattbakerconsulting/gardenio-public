package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/commons"
	"gardenio/controller/internal/commons/commonstest"
	"gardenio/controller/internal/gardenhub"
	"gardenio/controller/internal/oidcauth"
)

// The cross-land negative matrix, viewer half (Lands Track B step 10.6b,
// gd-cIux5tj6WI; spec §5.6 "Proving the fence"). The daemon half is kbeads
// TestCrossLandMatrix (gd-USqiG2sdPU); this is its twin for beads-viewer. Two
// tenant lands: A is claimlayer, B is bravo. Every route registeredViewerRoutes
// records is called as each of A's people (member, owner, guest), with every
// path parameter filled with one of B's beads, B's land or B's channel, through
// the production chain (scopeMiddleware in front of the routes the Server
// registers, garden and Commons included) over daemons that cut reads by
// bearer the way kbeads does. Every call must hold four things:
//
//   - never data: no response body carries xlMarker, which is in every one of
//     B's beads (title, description, notes, fields and comments) in every store
//     the viewer reads (the bead daemon, the Commons and the farm);
//   - never a write to B on anyone else's behalf: a write naming one of B's
//     beads or B's land that reaches a daemon on any bearer but A's own is a
//     breach (the daemon refuses A's own; the viewer's machine identity must
//     never carry it instead);
//   - never a 2xx on a route that names B's bead or land in its path;
//   - never an existence oracle: on such a route, the answer for B's bead is
//     the answer for one that does not exist (the same call with every one of
//     B's values swapped for a missing one), status and body alike. A route
//     that differs may be pinned in xlViewerExistenceOracles; the list may
//     only shrink (a stale entry fails the test).
//
// TestViewerCrossLandMatrix_ControlReachesB runs a few reads as B's own member
// and requires the marker back, so the fills are known to reach B's data.
//
// Beside the walk: the farm document (GET /garden/farm) as A carries nothing of
// B; the bead feed hub, which the Commons event stream and /feed/events ride,
// sends A none of B's events while B's beads change; the Gate's mail goes to
// the application's own applicant only. Push alerts and the purse watcher's
// mail are proved next to their own fakes: internal/bridge
// TestCrossLandMatrix_PushAlerts and internal/lands/watcher
// TestCrossLandMatrix_PurseMail.

const (
	xlA      = "claimlayer"
	xlB      = "bravo"
	xlMarker = "BRAVO-FENCE-MARKER"

	xlBravoBearer = "tok-bravo"
	xlBravoEmail  = "bv@bravo.example"
)

// B's beads, one per family a path parameter names.
const (
	xlTask       = "bv-task"
	xlDecision   = "bv-decision"
	xlEscalation = "bv-escalation"
	xlRelease    = "bv-release"
	xlPlan       = "bv-plan"
	xlBundle     = "bv-bundle"
	xlChannel    = "bv-channel"
	xlThread     = "bv-thread"
	xlMessage    = "bv-msg"
	xlAgent      = "bv-agent"
	xlAgentName  = "bravo-crew-1"
)

// xlFills maps a path parameter to the value of B's that fills it. {id} is
// filled per route family by xlIDFill; a parameter missing here is filled
// with "x" (it names nothing of B's).
var xlFills = map[string]string{
	"land":    xlB,
	"project": xlB,
	"name":    xlB,
	"plot":    xlB,
	"channel": "general",
	"thread":  xlThread,
	"agent":   xlAgent,
	"token":   "bravo-window",
}

// xlNamesB are the parameters whose fill names one of B's beads or B's land,
// so the route's answer must not tell A it exists.
var xlNamesB = map[string]bool{
	"id": true, "land": true, "project": true, "name": true, "plot": true,
	"thread": true, "agent": true,
}

// xlIDFill picks which of B's beads fills {id} for a route.
func xlIDFill(pattern string) string {
	switch {
	case strings.Contains(pattern, "/decisions/"):
		return xlDecision
	case strings.Contains(pattern, "/escalation"):
		return xlEscalation
	case strings.Contains(pattern, "/release"):
		return xlRelease
	case strings.Contains(pattern, "/plans/"):
		return xlPlan
	case strings.Contains(pattern, "/bundle"):
		return xlBundle
	case strings.Contains(pattern, "/garden/c/"):
		return xlMessage
	case strings.Contains(pattern, "/agents/"), strings.Contains(pattern, "/gardener/"):
		return xlAgent
	}
	return xlTask
}

var xlParamRe = regexp.MustCompile(`\{([A-Za-z_]+)(\.\.\.)?\}`)

// xlPath splits pattern into its method and a concrete path with every
// parameter filled from B, and reports whether the path names B.
func xlPath(pattern string) (method, path string, namesB bool) {
	method, path, found := strings.Cut(pattern, " ")
	if !found {
		method, path = http.MethodGet, pattern // a pattern with no method takes every one
	}
	path = strings.ReplaceAll(path, "{$}", "")
	path = xlParamRe.ReplaceAllStringFunc(path, func(m string) string {
		sub := xlParamRe.FindStringSubmatch(m)
		name := sub[1]
		namesB = namesB || xlNamesB[name]
		switch {
		case name == "id":
			return xlIDFill(pattern)
		case xlFills[name] != "":
			return xlFills[name]
		case sub[2] != "":
			return "x/y"
		}
		return "x"
	})
	if path == "" {
		path = "/"
	}
	return method, path, namesB
}

// xlForm is the body every write carries: each key a handler might read, set
// to something that would show in B's beads if it were applied.
func xlForm() url.Values {
	v := url.Values{}
	for k, val := range map[string]string{
		"title": "written from A", "description": "written from A", "text": "written from A",
		"body": "written from A", "comment": "written from A", "answer": "written from A",
		"reason": "written from A", "value": "written from A", "status": "closed",
		"label": "project:" + xlB, "labels": "project:" + xlB, "project": xlB, "land": xlB,
		"id": xlTask, "bead": xlTask, "bead_id": xlTask, "parent": xlPlan, "parent_id": xlPlan,
		"depends_on": xlDecision, "depends_on_id": xlDecision, "channel_id": xlChannel,
		"thread_id": xlThread, "agent": xlAgent, "option": "a", "chosen": "a", "priority": "0",
		"assignee": "a@claimlayer.example", "owner": "a@claimlayer.example", "rating": "5",
	} {
		v.Set(k, val)
	}
	return v
}

// xlMissing maps each of B's values in a path or body to one nothing carries.
var (
	xlToMissing   = strings.NewReplacer("bv-", "bv-gone-", xlB, "zulu")
	xlFromMissing = strings.NewReplacer("bv-gone-", "bv-", "zulu", xlB)
)

// xlViewerExistenceOracles pins the routes on which A's answer for B's bead
// differs from its answer for a missing one. Each line is a debt, not a rule:
// make the route answer an out-of-scope bead as it answers a missing one
// (beadMissing), then delete the line. A pinned route that no longer differs
// fails the test until its line goes. Drained by gd-ZJOUPZWuvs; keep it empty.
var xlViewerExistenceOracles = map[string]bool{}

// xlViewer2xxNamingB pins the routes that answer A's call naming B with a 2xx.
// Same ratchet as the oracles: it may only shrink, and the walk still holds a
// pinned route to never data and never a write. Drained by gd-ZJOUPZWuvs.
var xlViewer2xxNamingB = map[string]bool{}

// xlCallers are A's people. They all carry A's bearer, which the daemons cut
// to claimlayer; their land role is what the groups say.
func xlCallers() []struct {
	name string
	p    principal
} {
	roles := []string{oidcauth.RoleProjectMember}
	return []struct {
		name string
		p    principal
	}{
		{"A member", principal{email: "a@claimlayer.example", roles: roles, groups: []string{"/project-" + xlA}}},
		{"A owner", principal{email: "ao@claimlayer.example", roles: roles, groups: []string{"/project-" + xlA + "/owners"}}},
		{"A guest", principal{email: "ag@claimlayer.example", roles: roles, groups: []string{"/project-" + xlA + "/guests"}}},
	}
}

func xlBravoPrincipal() principal {
	return principal{email: xlBravoEmail, roles: []string{oidcauth.RoleProjectMember}, groups: []string{"/project-" + xlB}}
}

// xlBeads is each land's beads. B's carry xlMarker everywhere a page could
// print it; A's are there so A's own pages have something to render.
func xlBeads(t0 time.Time) []*beadsapi.BeadDetail {
	bead := func(id, land, typ, status string, fields map[string]string, labels ...string) *beadsapi.BeadDetail {
		title, desc := "Title "+id, "about "+id
		if land == xlB {
			title, desc = xlMarker+" "+id, xlMarker
			fields["notes"] = xlMarker
		}
		return &beadsapi.BeadDetail{
			ID: id, Title: title, Description: desc, Notes: desc, Type: typ, Kind: "issue", Status: status, Priority: 2,
			Labels: append([]string{"project:" + land}, labels...), Fields: fields, Fleet: "default",
			CreatedBy: "oidc:someone@" + land + ".example", CreatedAt: t0, UpdatedAt: t0,
		}
	}
	var out []*beadsapi.BeadDetail
	for _, l := range []struct{ land, pre string }{{xlA, "cl"}, {xlB, "bv"}} {
		mark := "about"
		if l.land == xlB {
			mark = xlMarker
		}
		out = append(out,
			bead(l.pre+"-task", l.land, "task", "open", map[string]string{"channel_id": l.pre + "-channel"}),
			bead(l.pre+"-decision", l.land, "decision", "open", map[string]string{
				"prompt": mark, "options": `[{"id":"a","label":"` + mark + `"}]`, "operated_by": "oidc:someone@" + l.land + ".example"}),
			bead(l.pre+"-escalation", l.land, "escalation", "open", map[string]string{
				"operation_type": "github_pr_merge", "context": mark, "requesting_agent": l.pre + "-agent"}),
			bead(l.pre+"-release", l.land, "release", "open", map[string]string{"tag": mark}),
			bead(l.pre+"-plan", l.land, "epic", "in_progress", map[string]string{"plan_state": "building"}, "plan"),
			bead(l.pre+"-bundle", l.land, "bundle", "open", map[string]string{"documents": `[{"name":"n","format":"markdown","body":"` + mark + `"}]`}),
			bead(l.pre+"-channel", l.land, "channel", "open", map[string]string{"name": "general", "channel_kind": "general", "mirror": "both"}),
			bead(l.pre+"-thread", l.land, "task", "open", map[string]string{"channel_id": l.pre + "-channel"}, "thread"),
			bead(l.pre+"-msg", l.land, "message", "closed", map[string]string{
				"channel_id": l.pre + "-channel", "thread_id": l.pre + "-thread", "author": "oidc:someone@" + l.land + ".example", "text": mark}),
			bead(l.pre+"-agent", l.land, "agent", "in_progress", map[string]string{
				"agent": l.land + "-crew-1", "project": l.land, "role": "crew", "agent_state": "working",
				"task_id": l.pre + "-task", "operated_by": "oidc:someone@" + l.land + ".example"}),
		)
	}
	return out
}

// xlDaemon is scopedDaemon over a fakeDaemon holding both lands, cutting the
// rest of the reads a page makes by bead id the way kbeads cuts them, and
// recording every write that names B on a bearer that is not A's.
type xlDaemon struct {
	*scopedDaemon
	mu       sync.Mutex
	breaches []string
}

func newXLDaemon(t0 time.Time) *xlDaemon {
	f := newFake()
	f.semanticErr = context.Canceled // semantic search off: search takes the scoped list path
	f.beads = map[string]*beadsapi.BeadDetail{}
	f.listResp = &beadsapi.ListBeadsResult{}
	var events []beadsapi.Event
	for i, b := range xlBeads(t0) {
		f.beads[b.ID] = b
		f.listResp.Beads = append(f.listResp.Beads, b)
		payload, _ := json.Marshal(map[string]any{"bead": b})
		events = append(events, beadsapi.Event{ID: int64(i + 1), Topic: "beads.bead.created", CreatedAt: t0, Payload: payload})
	}
	f.listResp.Total = len(f.listResp.Beads)
	f.listGlobalEventsResp = &beadsapi.ListGlobalEventsResponse{Events: events}
	f.comments = map[string][]beadsapi.Comment{
		xlTask: {{ID: 1, Author: xlBravoEmail, Text: xlMarker, CreatedAt: t0}},
	}
	f.deps = map[string][]beadsapi.Dependency{xlTask: {{BeadID: xlTask, DependsOnID: xlDecision, Type: "blocks"}}}
	f.projectsResp = map[string]beadsapi.ProjectInfo{xlA: {Name: xlA}, xlB: {Name: xlB}}
	d := newScopedDaemon(f)
	d.scopes[xlBravoBearer] = []string{xlB}
	return &xlDaemon{scopedDaemon: d}
}

// hidden reports whether the bearer on ctx is scoped and id is outside it.
func (d *xlDaemon) hidden(ctx context.Context, id string) bool {
	b, ok := d.fakeDaemon.beads[id]
	return ok && !d.visible(ctx, b.Labels)
}

var errXLForbidden = &beadsapi.APIError{StatusCode: http.StatusForbidden, Message: "not authorized to read beads in this project"}

var errXLMissing = &beadsapi.APIError{StatusCode: http.StatusNotFound, Message: "bead not found"}

// write is every write's gate: one naming B on A's bearer is refused, as the
// daemon refuses it; one naming B on any bearer but A's or B's own is a breach
// (the viewer carried it on its own identity), refused too so nothing changes.
// One naming a missing bead (the walk's twin call) is the daemon's 404.
func (d *xlDaemon) write(ctx context.Context, op string, args ...any) error {
	raw, _ := json.Marshal(args)
	s := string(raw)
	if real := strings.NewReplacer("bv-gone-", "", "zulu", "").Replace(s); strings.Contains(real, "bv-") || strings.Contains(real, xlB) {
		switch bearer := beadsapi.UserBearerFromContext(ctx); bearer {
		case xlBravoBearer:
			return nil
		case scopedBearer:
		default:
			d.mu.Lock()
			d.breaches = append(d.breaches, fmt.Sprintf("%s on bearer %q: %s", op, bearer, s))
			d.mu.Unlock()
		}
		return errXLForbidden
	}
	if strings.Contains(s, "bv-gone-") || strings.Contains(s, "zulu") {
		return errXLMissing
	}
	return nil
}

func (d *xlDaemon) takeBreaches() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := d.breaches
	d.breaches = nil
	return out
}

func (d *xlDaemon) BatchGetBeads(ctx context.Context, ids []string) (map[string]*beadsapi.BeadDetail, error) {
	all, err := d.fakeDaemon.BatchGetBeads(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := map[string]*beadsapi.BeadDetail{}
	for id, b := range all {
		if d.visible(ctx, b.Labels) {
			out[id] = b
		}
	}
	return out, nil
}

func (d *xlDaemon) GetComments(ctx context.Context, id string) ([]beadsapi.Comment, error) {
	if d.hidden(ctx, id) {
		return nil, errXLForbidden
	}
	return d.fakeDaemon.GetComments(ctx, id)
}

func (d *xlDaemon) GetDependencies(ctx context.Context, id string) ([]beadsapi.Dependency, error) {
	if d.hidden(ctx, id) {
		return nil, errXLForbidden
	}
	return d.fakeDaemon.GetDependencies(ctx, id)
}

func (d *xlDaemon) GetReverseDependencies(ctx context.Context, id string) ([]beadsapi.Dependency, error) {
	if d.hidden(ctx, id) {
		return nil, errXLForbidden
	}
	return d.fakeDaemon.GetReverseDependencies(ctx, id)
}

func (d *xlDaemon) GetEvents(ctx context.Context, id string) ([]beadsapi.Event, error) {
	if d.hidden(ctx, id) {
		return nil, errXLForbidden
	}
	return d.fakeDaemon.GetEvents(ctx, id)
}

func (d *xlDaemon) GetRecentEvents(ctx context.Context, id string, limit int) ([]beadsapi.Event, error) {
	if d.hidden(ctx, id) {
		return nil, errXLForbidden
	}
	return d.fakeDaemon.GetRecentEvents(ctx, id, limit)
}

func (d *xlDaemon) FindAgentBead(ctx context.Context, name string) (*beadsapi.BeadDetail, error) {
	b, err := d.fakeDaemon.FindAgentBead(ctx, name)
	if err == nil && !d.visible(ctx, b.Labels) {
		return nil, beadsapi.ErrAgentNotFound
	}
	return b, err
}

func (d *xlDaemon) FindMostRecentAgentBead(ctx context.Context, name string) (*beadsapi.BeadDetail, error) {
	b, err := d.fakeDaemon.FindMostRecentAgentBead(ctx, name)
	if err == nil && !d.visible(ctx, b.Labels) {
		return nil, beadsapi.ErrAgentNotFound
	}
	return b, err
}

func (d *xlDaemon) FindAgentBeadsByNames(ctx context.Context, names, statuses []string) (map[string]*beadsapi.BeadDetail, error) {
	all, err := d.fakeDaemon.FindAgentBeadsByNames(ctx, names, statuses)
	out := map[string]*beadsapi.BeadDetail{}
	for n, b := range all {
		if d.visible(ctx, b.Labels) {
			out[n] = b
		}
	}
	return out, err
}

func (d *xlDaemon) UpdateBead(ctx context.Context, id string, req beadsapi.UpdateBeadRequest) error {
	if err := d.write(ctx, "UpdateBead", id, req); err != nil {
		return err
	}
	return d.fakeDaemon.UpdateBead(ctx, id, req)
}

func (d *xlDaemon) UpdateBeadFieldsTyped(ctx context.Context, id string, fields map[string]any) error {
	if err := d.write(ctx, "UpdateBeadFieldsTyped", id, fields); err != nil {
		return err
	}
	return d.fakeDaemon.UpdateBeadFieldsTyped(ctx, id, fields)
}

func (d *xlDaemon) AppendBeadArrayField(ctx context.Context, id, field, element string) error {
	if err := d.write(ctx, "AppendBeadArrayField", id, field, element); err != nil {
		return err
	}
	return d.fakeDaemon.AppendBeadArrayField(ctx, id, field, element)
}

func (d *xlDaemon) ReorderBeadArrayField(ctx context.Context, id, field string, expected, next []string) error {
	if err := d.write(ctx, "ReorderBeadArrayField", id, field); err != nil {
		return err
	}
	return d.fakeDaemon.ReorderBeadArrayField(ctx, id, field, expected, next)
}

func (d *xlDaemon) CloseBeadAs(ctx context.Context, id, actor string, fields map[string]string) error {
	if err := d.write(ctx, "CloseBeadAs", id); err != nil {
		return err
	}
	return d.fakeDaemon.CloseBeadAs(ctx, id, actor, fields)
}

func (d *xlDaemon) AddComment(ctx context.Context, id, author, text string) error {
	if err := d.write(ctx, "AddComment", id); err != nil {
		return err
	}
	return d.fakeDaemon.AddComment(ctx, id, author, text)
}

func (d *xlDaemon) AddDependency(ctx context.Context, beadID, dependsOnID, depType, createdBy string) error {
	if err := d.write(ctx, "AddDependency", beadID, dependsOnID); err != nil {
		return err
	}
	return d.fakeDaemon.AddDependency(ctx, beadID, dependsOnID, depType, createdBy)
}

func (d *xlDaemon) RemoveDependency(ctx context.Context, beadID, dependsOnID, depType string) error {
	if err := d.write(ctx, "RemoveDependency", beadID, dependsOnID); err != nil {
		return err
	}
	return d.fakeDaemon.RemoveDependency(ctx, beadID, dependsOnID, depType)
}

func (d *xlDaemon) AddLabel(ctx context.Context, beadID, label string) error {
	if err := d.write(ctx, "AddLabel", beadID, label); err != nil {
		return err
	}
	return d.fakeDaemon.AddLabel(ctx, beadID, label)
}

func (d *xlDaemon) RemoveLabel(ctx context.Context, beadID, label string) error {
	if err := d.write(ctx, "RemoveLabel", beadID, label); err != nil {
		return err
	}
	return d.fakeDaemon.RemoveLabel(ctx, beadID, label)
}

func (d *xlDaemon) ResolveDecision(ctx context.Context, id string, req beadsapi.ResolveDecisionRequest) error {
	if err := d.write(ctx, "ResolveDecision", id); err != nil {
		return err
	}
	return d.fakeDaemon.ResolveDecision(ctx, id, req)
}

func (d *xlDaemon) CancelDecision(ctx context.Context, id, reason, canceledBy string) error {
	if err := d.write(ctx, "CancelDecision", id); err != nil {
		return err
	}
	return d.fakeDaemon.CancelDecision(ctx, id, reason, canceledBy)
}

func (d *xlDaemon) ApproveEscalation(ctx context.Context, id string, req beadsapi.ApproveEscalationRequest) error {
	if err := d.write(ctx, "ApproveEscalation", id); err != nil {
		return err
	}
	return d.fakeDaemon.ApproveEscalation(ctx, id, req)
}

func (d *xlDaemon) RejectEscalation(ctx context.Context, id string, req beadsapi.RejectEscalationRequest) error {
	if err := d.write(ctx, "RejectEscalation", id); err != nil {
		return err
	}
	return d.fakeDaemon.RejectEscalation(ctx, id, req)
}

func (d *xlDaemon) RouteEscalation(ctx context.Context, id string, req beadsapi.RouteEscalationRequest) (string, error) {
	if err := d.write(ctx, "RouteEscalation", id); err != nil {
		return "", err
	}
	return d.fakeDaemon.RouteEscalation(ctx, id, req)
}

func (d *xlDaemon) DeleteBead(ctx context.Context, id string) error {
	if err := d.write(ctx, "DeleteBead", id); err != nil {
		return err
	}
	return d.fakeDaemon.DeleteBead(ctx, id)
}

func (d *xlDaemon) AddBeadFollower(ctx context.Context, beadID string, operatorID int64, note string) error {
	if err := d.write(ctx, "AddBeadFollower", beadID); err != nil {
		return err
	}
	return d.fakeDaemon.AddBeadFollower(ctx, beadID, operatorID, note)
}

func (d *xlDaemon) RemoveBeadFollower(ctx context.Context, beadID string, operatorID int64) error {
	if err := d.write(ctx, "RemoveBeadFollower", beadID); err != nil {
		return err
	}
	return d.fakeDaemon.RemoveBeadFollower(ctx, beadID, operatorID)
}

func (d *xlDaemon) ApplyTaint(ctx context.Context, beadID, taintName string) (*beadsapi.BeadTaint, error) {
	if err := d.write(ctx, "ApplyTaint", beadID); err != nil {
		return nil, err
	}
	return d.fakeDaemon.ApplyTaint(ctx, beadID, taintName)
}

func (d *xlDaemon) PourFormula(ctx context.Context, formulaID string, req beadsapi.PourFormulaRequest) (*beadsapi.PourFormulaResponse, error) {
	if err := d.write(ctx, "PourFormula", formulaID, req); err != nil {
		return nil, err
	}
	return d.fakeDaemon.PourFormula(ctx, formulaID, req)
}

func (d *xlDaemon) CreateBead(ctx context.Context, req beadsapi.CreateBeadRequest) (string, error) {
	if err := d.write(ctx, "CreateBead", req); err != nil {
		return "", err
	}
	return d.fakeDaemon.CreateBead(ctx, req)
}

func (d *xlDaemon) CreateBeadDetail(ctx context.Context, req beadsapi.CreateBeadRequest) (*beadsapi.BeadDetail, error) {
	if err := d.write(ctx, "CreateBeadDetail", req); err != nil {
		return nil, err
	}
	return d.fakeDaemon.CreateBeadDetail(ctx, req)
}

func (d *xlDaemon) ApproveScrubRequest(ctx context.Context, beadID, by string) (*beadsapi.BeadDetail, error) {
	if err := d.write(ctx, "ApproveScrubRequest", beadID); err != nil {
		return nil, err
	}
	return d.fakeDaemon.ApproveScrubRequest(ctx, beadID, by)
}

func (d *xlDaemon) RejectScrubRequest(ctx context.Context, beadID, by string) (*beadsapi.BeadDetail, error) {
	if err := d.write(ctx, "RejectScrubRequest", beadID); err != nil {
		return nil, err
	}
	return d.fakeDaemon.RejectScrubRequest(ctx, beadID, by)
}

// The share windows (planShareAPI): a plan's or land's windows are B's when the
// plan or land is, and are minted, listed, revoked and previewed on the
// caller's bearer, as kbeads does.
func (d *xlDaemon) CreateWindow(ctx context.Context, planID string, days int) (*beadsapi.MintedWindow, error) {
	if err := d.write(ctx, "CreateWindow", planID); err != nil {
		return nil, err
	}
	return &beadsapi.MintedWindow{Window: beadsapi.Window{ID: "win-1", PlanID: planID, Land: xlA, State: "live"}, Token: "t", Path: "/w/t"}, nil
}

func (d *xlDaemon) ListWindows(ctx context.Context, land string) ([]beadsapi.Window, error) {
	if projects, scoped := d.projectsFor(ctx); scoped && land != "" && !slices.Contains(projects, land) {
		return nil, errXLForbidden
	}
	return nil, nil
}

func (d *xlDaemon) RevokeWindow(ctx context.Context, id string) (*beadsapi.Window, error) {
	if err := d.write(ctx, "RevokeWindow", id); err != nil {
		return nil, err
	}
	return nil, &beadsapi.APIError{StatusCode: http.StatusNotFound, Message: "window not found"}
}

func (d *xlDaemon) PreviewWindow(ctx context.Context, planID string, days int) (json.RawMessage, error) {
	if d.hidden(ctx, planID) {
		return nil, errXLForbidden
	}
	b, err := d.GetBead(ctx, planID)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"plan": map[string]any{"id": b.ID, "title": b.Title}})
}

// xlCommons is the Commons fake (commonstest.TwoLands, so A's land has its
// channels) with B's conversation beads put in beside it.
func xlCommons(t0 time.Time) *commonstest.Daemon {
	d := commonstest.TwoLands(t0)
	d.Scopes[xlBravoBearer] = []string{xlB}
	d.Selves = map[string]string{xlBravoBearer: "oidc:" + xlBravoEmail}
	d.Members = map[string][]beadsapi.LandMember{
		xlA: d.Members[xlA],
		xlB: {{Principal: "oidc:" + xlBravoEmail, Name: xlMarker}},
	}
	d.Comments = map[string][]beadsapi.Comment{xlTask: {{ID: 1, Text: xlMarker, CreatedAt: t0}}}
	for _, b := range xlBeads(t0) {
		if strings.HasPrefix(b.ID, "bv-") {
			d.Put(b)
			d.Log("beads.bead.created", b, b.CreatedAt)
		}
	}
	return d
}

// xlCommonsWrites returns what the Commons fake wrote that names B.
func xlCommonsWrites(d *commonstest.Daemon) string {
	raw, _ := json.Marshal(struct {
		C []beadsapi.CreateBeadRequest
		U []map[string]any
	}{d.Created, d.Updated})
	if s := string(raw); strings.Contains(s, "bv-") || strings.Contains(s, xlB) {
		return s
	}
	return ""
}

// xlFarm is the farm read model's source, both lands' beads in it.
func xlFarm(t0 time.Time) *farmFake {
	f := &farmFake{}
	for _, b := range xlBeads(t0) {
		f.add(b)
	}
	return f
}

// xlViewer is the production chain over the three fakes.
type xlViewer struct {
	ts      *httptest.Server
	d       *xlDaemon
	commons *commonstest.Daemon
	hub     *BeadFeedHub
}

// xlWho is every caller the walk names, by the X-Test-Who header that carries
// it into the verified context, as the OIDC middleware would put it there.
func xlWho() map[string]principal {
	who := map[string]principal{"B member": xlBravoPrincipal()}
	for _, c := range xlCallers() {
		who[c.name] = c.p
	}
	return who
}

func newXLViewer(t *testing.T) *xlViewer {
	t.Helper()
	t0 := time.Now().Add(-time.Hour)
	d := newXLDaemon(t0)
	cd := xlCommons(t0)
	src := xlFarm(t0)
	farm := gardenhub.NewFarm(src, testLogger())
	farm.Start()
	t.Cleanup(farm.Close)
	gh := gardenhub.New(src, "", testLogger())
	gh.Start()
	t.Cleanup(gh.Close)

	srv := NewServer(d, testLogger(), "", "", "", &fakeScreenClient{})
	hub := NewBeadFeedHub(d, testLogger())
	for _, b := range xlBeads(t0) {
		data, _ := json.Marshal(map[string]any{"bead": b})
		hub.broadcast(beadsapi.SSEEvent{Event: "beads.bead.created", Data: data})
	}
	srv.SetBeadFeedHub(hub)
	srv.SetGardenHandler(&gardenhub.Handler{
		Hub: gh, Farm: farm, Beads: d, Links: d, Cut: gardenFarmCut, InLand: gardenInLand,
		Queues: gardenQueues{srv}, Search: gardenSearch{srv}, Bundles: gardenBundles{srv}, Plans: gardenPlans{srv},
		Projects: gardenProjects{srv}, Gardeners: gardenGardeners{srv}, Settings: gardenSettings{srv},
		Credits:  landCreditSource{srv},
		ReadOnly: func(r *http.Request) bool { return viewerScopeFromContext(r.Context()).Scoped() },
		SeesGardener: func(r *http.Request) func(gardenhub.FarmGardener) bool {
			sc := viewerScopeFromContext(r.Context())
			if !sc.Scoped() {
				return nil
			}
			return func(g gardenhub.FarmGardener) bool { return g.Project != "" && sc.allows(g.Project) }
		},
	})
	srv.SetCommonsHandler(newCommonsHandler(cd, commons.NewHub(hub.Subscribe, cd, testLogger()), commons.NewPresence(nil, nil, nil), "", nil))
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	chain := scopeMiddleware(mux, func(r *http.Request) string { _, p := mux.Handler(r); return p })
	who := xlWho()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := who[r.Header.Get("X-Test-Who")]
		bearer := scopedBearer
		if p.email == xlBravoEmail {
			bearer = xlBravoBearer
		}
		ctx := beadsapi.WithUserBearer(withPrincipal(r.Context(), p), bearer)
		chain.ServeHTTP(w, r.WithContext(ctx))
	}))
	t.Cleanup(ts.Close)
	return &xlViewer{ts: ts, d: d, commons: cd, hub: hub}
}

// xlStreamWait bounds one call of the walk; only a stream route is still
// talking when it runs out, and the call keeps what arrived by then.
const xlStreamWait = 500 * time.Millisecond

func (v *xlViewer) do(t *testing.T, who, method, path string, form url.Values) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), xlStreamWait)
	defer cancel()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, v.ts.URL+path, body)
	if err != nil {
		t.Fatalf("new request %s %s: %v", method, path, err)
	}
	req.Header.Set("X-Test-Who", who)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, ""
		}
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body) // a deadline mid-stream keeps the bytes read so far
	return resp.StatusCode, string(b)
}

// xlNoiseRe drops what differs between two renders of the same answer.
var xlNoiseRe = regexp.MustCompile(`nonce="[^"]*"|\d{4}-\d\d-\d\dT[0-9:.]+Z|\d+ms`)

func xlNorm(s string) string { return xlNoiseRe.ReplaceAllString(s, "") }

func TestViewerCrossLandMatrix(t *testing.T) {
	patterns := registeredViewerRoutes(t).patterns
	v := newXLViewer(t)

	oracles, twoHundreds := map[string]bool{}, map[string]bool{}
	for _, c := range xlCallers() {
		for _, pattern := range patterns {
			method, path, namesB := xlPath(pattern)
			var form url.Values
			if method != http.MethodGet && method != http.MethodDelete && method != http.MethodHead {
				form = xlForm()
			}
			code, resp := v.do(t, c.name, method, path, form)
			where := c.name + " " + method + " " + path

			if strings.Contains(resp, xlMarker) {
				t.Errorf("%s: %d, the body carries B's data: %.300s", where, code, resp)
			}
			if b := v.d.takeBreaches(); len(b) > 0 {
				t.Errorf("%s: %d, a write naming B reached the daemon on another bearer:\n  %s", where, code, strings.Join(b, "\n  "))
			}
			if w := xlCommonsWrites(v.commons); w != "" {
				t.Errorf("%s: %d, the Commons wrote to B: %.300s", where, code, w)
				v = newXLViewer(t)
				continue
			}
			if !namesB {
				continue
			}
			if code/100 == 2 {
				twoHundreds[pattern] = true
				if !xlViewer2xxNamingB[pattern] {
					t.Errorf("%s: %d on a route naming B, want it answered as a missing bead is (or pin %q in xlViewer2xxNamingB): %.300s", where, code, pattern, resp)
				}
			}
			var mForm url.Values
			if form != nil {
				mForm = url.Values{}
				for k, vals := range form {
					mForm[k] = []string{xlToMissing.Replace(vals[0])}
				}
			}
			mCode, mResp := v.do(t, c.name, method, xlToMissing.Replace(path), mForm)
			got, missing := xlNorm(resp), xlNorm(xlFromMissing.Replace(mResp))
			if code != mCode || got != missing {
				oracles[pattern] = true
				if !xlViewerExistenceOracles[pattern] {
					t.Errorf("%s: B's bead answers %d %.200s\n  a missing one answers %d %.200s\n  so A can tell B's bead exists (or pin %q in xlViewerExistenceOracles)",
						where, code, got, mCode, missing, pattern)
				}
			}
		}
	}
	for pattern := range xlViewerExistenceOracles {
		if !oracles[pattern] {
			t.Errorf("xlViewerExistenceOracles pins %q, which now answers B's bead as a missing one: delete its line", pattern)
		}
	}
	for pattern := range xlViewer2xxNamingB {
		if !twoHundreds[pattern] {
			t.Errorf("xlViewer2xxNamingB pins %q, which no longer answers 2xx on a route naming B: delete its line", pattern)
		}
	}
}

// The positive control: B's own member reads B's marker through the same fills,
// on the bead page, the decision page, B's Commons thread and the farm.
func TestViewerCrossLandMatrix_ControlReachesB(t *testing.T) {
	v := newXLViewer(t)
	for _, pattern := range []string{
		"GET /{id}",
		"GET /decisions/{id}",
		"GET /b/{id}/comments",
		"GET /garden/c/{land}/{channel}/t/{thread}",
	} {
		method, path, _ := xlPath(pattern)
		code, resp := v.do(t, "B member", method, path, nil)
		if code != http.StatusOK || !strings.Contains(resp, xlMarker) {
			t.Errorf("B member %s %s: %d %.300s, want 200 with B's marker", method, path, code, resp)
		}
	}
}

// Every pattern fills to a path with no braces left, so a new parameter cannot
// leave a route walking a literal "{x}".
func TestViewerCrossLandMatrix_EveryParamFilled(t *testing.T) {
	for _, pattern := range registeredViewerRoutes(t).patterns {
		if _, path, _ := xlPath(pattern); strings.ContainsAny(path, "{}") {
			t.Errorf("%s fills to %s", pattern, path)
		}
	}
}

// The farm document as A carries nothing of B: no bead, no gardener, no land.
// The control is B's own member, whose farm carries B's plan.
func TestViewerCrossLandMatrix_FarmDocument(t *testing.T) {
	v := newXLViewer(t)
	for _, c := range xlCallers() {
		code, body := v.do(t, c.name, http.MethodGet, "/garden/farm", nil)
		if code != http.StatusOK {
			t.Fatalf("%s GET /garden/farm: %d %.300s", c.name, code, body)
		}
		for _, leak := range []string{xlMarker, "bv-", `"` + xlB + `"`, xlAgentName} {
			if strings.Contains(body, leak) {
				t.Errorf("%s GET /garden/farm carries %q of B's: %.400s", c.name, leak, body)
			}
		}
		if !strings.Contains(body, "cl-plan") {
			t.Errorf("%s GET /garden/farm lost A's own plan: %.400s", c.name, body)
		}
	}
	if _, body := v.do(t, "B member", http.MethodGet, "/garden/farm", nil); !strings.Contains(body, xlPlan) {
		t.Errorf("control: B member's farm does not carry %s, so the A rows are vacuous: %.400s", xlPlan, body)
	}
}

// The bead feed hub (the one upstream the Commons event stream and /feed/events
// ride) sends A none of B's events while B's beads change, and still sends A
// its own; B's own subscription gets B's, so the cut is not vacuous.
func TestViewerCrossLandMatrix_FeedHub(t *testing.T) {
	t0 := time.Now().Add(-time.Hour)
	d := newXLDaemon(t0)
	hub := NewBeadFeedHub(d, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a, stopA := hub.SubscribeFor(ctx, viewerScope{Projects: []string{xlA}})
	defer stopA()
	b, stopB := hub.SubscribeFor(ctx, viewerScope{Projects: []string{xlB}})
	defer stopB()

	for _, topic := range []string{"beads.bead.created", "beads.bead.updated", "beads.bead.closed", "beads.comment.added"} {
		for _, bead := range xlBeads(t0) {
			data, _ := json.Marshal(map[string]any{"bead": bead})
			hub.broadcast(beadsapi.SSEEvent{Event: topic, Data: data})
		}
	}
	gotA, gotB := xlDrain(a), xlDrain(b)
	if strings.Contains(gotA, xlMarker) || strings.Contains(gotA, `"bv-`) {
		t.Errorf("A's subscription got B's events:\n%.600s", gotA)
	}
	if !strings.Contains(gotA, `"cl-task"`) {
		t.Errorf("A's subscription lost its own events:\n%.600s", gotA)
	}
	if !strings.Contains(gotB, xlMarker) {
		t.Errorf("control: B's subscription got none of B's events, so A's row is vacuous:\n%.600s", gotB)
	}
}

func xlDrain(ch <-chan beadsapi.SSEEvent) string {
	var sb strings.Builder
	deadline := time.After(300 * time.Millisecond)
	for {
		select {
		case e, ok := <-ch:
			if !ok {
				return sb.String()
			}
			sb.WriteString(e.Event + " " + string(e.Data) + "\n")
		case <-deadline:
			return sb.String()
		}
	}
}

// The Gate's mail about a land's application goes to that application's
// applicant only: answering B's application (approve, deny or ask) while A's
// waits mails B's applicant, never A's, and every mail sent is B's.
func TestViewerCrossLandMatrix_GateMail(t *testing.T) {
	for _, answer := range []struct{ chosen, text string }{
		{"approve", ""}, {"deny", "not this time"}, {"ask", "Which branch?"},
	} {
		t.Run(answer.chosen, func(t *testing.T) {
			fx := newApplyFixture(t)
			ctx := context.Background()
			fx.intake.seedApplication("sub-claimlayer", xlA, "cl/app", "submitted")
			b := fx.intake.seedApplication("sub-bravo", xlB, "bv/app", "submitted")
			fx.srv.landsGate.sweep(ctx) // files each application's decision
			app, _ := fx.intake.GetBead(ctx, b)
			fx.intake.resolve(app.Fields["decision_id"], answer.chosen, answer.text)
			fx.srv.landsGate.sweep(ctx)

			sent := fx.mail.Sent()
			if len(sent) == 0 {
				t.Fatalf("control: answering B's application mailed no one, so the row is vacuous")
			}
			for _, m := range sent {
				if !slices.Equal(m.To, []string{"sub-bravo@example.com"}) {
					t.Errorf("%s mail went to %v, want B's applicant only", m.Kind, m.To)
				}
			}
		})
	}
}
