package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/landmap"
	"gardenio/kbeads/internal/model"
)

// registerLandsAPI mounts the Lands API (Door D.6, gd-yKOBNCMyNk): the
// contract's /lands/v1/ routes under /v1, served by internal/landmap. The map
// reads and writes only by calling this daemon's own routes in-process, with
// the caller's request context, through mux itself: each call passes the same
// route gate, role wrapper, scope filter and actionTable check as a kd call
// with the same bearer. Owner decision gd-BTHHmA2rYc = edge-route: no land
// pod; the land door (/land/<name>/v1/, D.2) reaches these routes as
// /land/<name>/v1/lands/v1/lands/<name>/...
//
// The guard test's recording registrar serves nothing, so the map is given a
// mux only when mux is one.
//
// Decision evidence pictures (gd-wRXlwYH2GI) are read from the viewer's
// picture bucket, named by the same COOP_S3_* environment; without it, or
// without credentials to read it, the door answers their route
// upstream_unavailable and shows no evidence.
func (s *BeadsServer) registerLandsAPI(mux routeRegistrar) {
	var daemon http.Handler = http.NotFoundHandler()
	if h, ok := mux.(http.Handler); ok {
		daemon = h
	}
	m := landmap.New(daemon, landsForMap{s}, changesForMap{s.sseHub})
	if p, err := landmap.PicturesFromEnv(context.Background()); err != nil {
		slog.Warn("lands: decision evidence pictures off: bucket config or credentials", "error", err)
	} else {
		m.SetPictures(p)
	}
	landmap.Register(mux, m)
}

// changesForMap taps the SSE hub for the land feed (D.9 gd-Z9VVGjvzBk). The
// feed takes only the topic, the bead id and the labels resolved at
// broadcast; it reads the bead again, as each reader, when it delivers.
type changesForMap struct{ h *sseHub }

func (c changesForMap) Tap(fn func(landmap.Change)) {
	if c.h == nil {
		return
	}
	c.h.addTap(func(evt *sseEvent) {
		kind := changeKinds[evt.Topic]
		if evt.Topic == events.TopicBeadUpdated && isPurseWrite(evt) {
			kind = landmap.ChangePurse
		}
		fn(landmap.Change{Kind: kind, BeadID: evt.BeadID, Labels: evt.Labels})
	})
}

// accountTypeJSON is how an account bead's type reads in an event payload;
// most events are not one, and are passed without decoding.
var accountTypeJSON = []byte(`"type":"account"`)

// isPurseWrite reports whether an update event is to a land's lands-<land>
// account bead, labelled with that land: the purse watcher's snapshot write,
// which the land feed sends on as budget.updated.
func isPurseWrite(evt *sseEvent) bool {
	if !bytes.Contains(evt.Data, accountTypeJSON) {
		return false
	}
	var p struct {
		Bead *struct {
			Type  model.BeadType `json:"type"`
			Title string         `json:"title"`
		} `json:"bead"`
	}
	if json.Unmarshal(evt.Data, &p) != nil || p.Bead == nil || p.Bead.Type != model.TypeAccount {
		return false
	}
	land, ok := strings.CutPrefix(p.Bead.Title, "lands-")
	return ok && land != "" && slices.Contains(evt.Labels, "project:"+land)
}

// changeKinds maps the daemon topics a land event can come from; every other
// topic is landmap.ChangeOther, which the feed drops.
var changeKinds = map[string]landmap.ChangeKind{
	events.TopicBeadCreated:  landmap.ChangeCreated,
	events.TopicBeadUpdated:  landmap.ChangeUpdated,
	events.TopicCommentAdded: landmap.ChangeUpdated,
	events.TopicLabelAdded:   landmap.ChangeUpdated,
	events.TopicLabelRemoved: landmap.ChangeUpdated,
	events.TopicBeadClosed:   landmap.ChangeClosed,
}

// landsForMap answers the map's questions about a land itself.
type landsForMap struct{ s *BeadsServer }

// Applicant is an OIDC sign-in with no gardenio role and no group, the
// caller handleLandMembers refuses too (applicant_routes_test.go walks every
// route as one).
func (l landsForMap) Applicant(ctx context.Context) bool {
	return authMethodFromContext(ctx) == authMethodOIDC && len(RolesFromContext(ctx)) == 0 && len(GroupsFromContext(ctx)) == 0
}

// Caller classes the caller for the map's request counter: a land principal
// (a land's own token, which its agents hold) is an agent, a scoped principal
// (a land member's sign-in) a member, and every other caller (an admin or
// unscoped sign-in, a static, fleet, machine or edge credential) an operator,
// whose reads are not a land's use.
func (l landsForMap) Caller(ctx context.Context) landmap.Caller {
	switch {
	case authMethodFromContext(ctx) == authMethodLand:
		return landmap.CallerAgent
	case isScopedPrincipal(ctx):
		return landmap.CallerMember
	}
	return landmap.CallerOperator
}

// Admits is landAdmitsCaller: the same answer GET /v1/lands/{land}/members
// gives, so the map is no oracle for other lands' names.
func (l landsForMap) Admits(ctx context.Context, land string) bool {
	return l.s.landAdmitsCaller(ctx, land)
}

// Land reads the land's project bead. A land_frozen that does not parse reads
// as paused: the switch fails closed (landFrozen).
func (l landsForMap) Land(ctx context.Context, land string) (landmap.LandRecord, bool, error) {
	p, err := l.s.store.FindProjectByName(ctx, land)
	switch {
	case errors.Is(err, sql.ErrNoRows), err == nil && p == nil:
		return landmap.LandRecord{}, false, nil
	case err != nil:
		return landmap.LandRecord{}, false, err
	}
	frozen, err := landFrozen(ctx, l.s.store, land)
	return landmap.LandRecord{CreatedAt: p.CreatedAt, Paused: frozen || err != nil}, true, nil
}

// Plot reads the land's repository off its project bead: a land works in one
// repository (the contract's Plot, "a land may have exactly one"), its
// git_url, which a tenant land's provisioning writes with default_branch. ok
// is false for no such land and for a land with no git_url.
func (l landsForMap) Plot(ctx context.Context, land string) (landmap.PlotRecord, bool, error) {
	p, err := l.s.store.FindProjectByName(ctx, land)
	switch {
	case errors.Is(err, sql.ErrNoRows), err == nil && p == nil:
		return landmap.PlotRecord{}, false, nil
	case err != nil:
		return landmap.PlotRecord{}, false, err
	}
	var f struct {
		GitURL        string `json:"git_url"`
		DefaultBranch string `json:"default_branch"`
	}
	if len(p.Fields) > 0 {
		_ = json.Unmarshal(p.Fields, &f)
	}
	rec := landmap.PlotRecord{GitURL: strings.TrimSpace(f.GitURL), DefaultBranch: strings.TrimSpace(f.DefaultBranch)}
	return rec, rec.GitURL != "", nil
}

// Budget reads the purse snapshot the controller's purse watcher stamps on the
// land's lands-<land> account bead (D.6c, gd-uAiEaCwsJJ). It reads through
// the store, not the caller's scope: a land member may not read account beads,
// and the map shows only the snapshot's figures. ok is false for no such land
// and for an operator land, which has no purse. A tenant land with no account
// bead, or one the watcher has not stamped yet, is ok with an empty record.
func (l landsForMap) Budget(ctx context.Context, land string) (landmap.BudgetRecord, bool, error) {
	acct, ok, err := l.purseAccount(ctx, land)
	if err != nil || !ok || acct == nil {
		return landmap.BudgetRecord{}, ok, err
	}
	var f map[string]any
	if len(acct.Fields) > 0 {
		if err := json.Unmarshal(acct.Fields, &f); err != nil {
			return landmap.BudgetRecord{}, false, fmt.Errorf("account %s: fields: %w", acct.ID, err)
		}
	}
	return purseSnapshot(f), true, nil
}

// purseAccount finds a tenant land's lands-<land> account bead through the
// store. ok is false for no such land and for an operator land; a tenant
// land with no account bead yet is ok with a nil bead.
func (l landsForMap) purseAccount(ctx context.Context, land string) (*model.Bead, bool, error) {
	p, err := l.s.store.FindProjectByName(ctx, land)
	switch {
	case errors.Is(err, sql.ErrNoRows), err == nil && p == nil:
		return nil, false, nil
	case err != nil:
		return nil, false, err
	}
	var pf struct {
		LandKind string `json:"land_kind"`
	}
	if len(p.Fields) > 0 {
		_ = json.Unmarshal(p.Fields, &pf)
	}
	if pf.LandKind != landKindTenant {
		return nil, false, nil
	}
	beads, _, err := l.s.store.ListBeads(ctx, model.BeadFilter{
		Type:   []model.BeadType{model.TypeAccount},
		Titles: []string{purseAccountTitle(land)},
		// The live statuses the watcher reads accounts in.
		Status: []model.Status{model.StatusOpen, model.StatusInProgress, model.StatusBlocked, model.StatusDeferred},
	})
	if err != nil {
		return nil, false, err
	}
	// The lowest id wins a duplicate, as the watcher reads them.
	var acct *model.Bead
	for _, b := range beads {
		if b != nil && (acct == nil || b.ID < acct.ID) {
			acct = b
		}
	}
	return acct, true, nil
}

// purseAccountTitle is the account bead a tenant land's purse is written on
// (purse.AccountName in the controller).
func purseAccountTitle(land string) string { return "lands-" + land }

// purseSnapshot reads the purse snapshot out of an account bead's fields.
func purseSnapshot(f map[string]any) landmap.BudgetRecord {
	state, _ := f["purse_state"].(string)
	rec := landmap.BudgetRecord{
		Granted: snapshotFigure(f["purse_granted_credits"]),
		Used:    snapshotFigure(f["purse_used_credits"]),
		State:   strings.TrimSpace(state),
	}
	if at, ok := f["purse_read_at"].(string); ok {
		rec.ReadAt, _ = time.Parse(time.RFC3339, strings.TrimSpace(at))
	}
	return rec
}

// snapshotFigure reads one credit figure of the purse snapshot, written as a
// string; nil when it is absent, empty or not a finite number.
func snapshotFigure(v any) *float64 {
	var x float64
	switch t := v.(type) {
	case float64:
		x = t
	case string:
		var err error
		if x, err = strconv.ParseFloat(strings.TrimSpace(t), 64); err != nil {
			return nil
		}
	default:
		return nil
	}
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return nil
	}
	return &x
}
