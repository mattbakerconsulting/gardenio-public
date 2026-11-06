package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/store"
)

// Windows (Lands: the Window, PW.1 gd-sCLo2acEFb; narrative PW-D3, PW-D7).
//
//	POST   /v1/windows            mint a window on a plan or a land; returns
//	                              the token once
//	GET    /v1/windows?land=<l>   the land's windows, never a token or its hash
//	DELETE /v1/windows/{id}       revoke, effective on the next request
//	GET    /v1/windows/self/view  the window principal's one route (window_view.go)
//	GET    /v1/windows/preview?plan=<id>|land=<l>&expires_in_days=<n>
//	                              the view such a window would show, for the
//	                              owner's preview before a mint (PW.4)
//
// A window targets a plan or, since LW.1 (gd-BXNhQf4ZqV, plan gd-4iVYF9tGsn),
// a whole land: its target_kind says which, and a window without one is on a
// plan. A land window's target is the land's type:project bead, and its view
// is the land projection (buildLandWindowView), never the farm document.
//
// Mint, list and revoke admit an unrestricted operator (an OIDC caller holding
// gardenio-operator who is not project-scoped) or a scoped human at the owner
// tier of the target's land, on their own bearer (windowManageDenial). The
// shared static token, fleet and per-agent tokens, machine, land and window
// principals and a v1 edge envelope are refused. The window bead is written
// server-side (withMachineWrite) because type:window is admin-gated, so no
// generic write can forge or retarget one.

// typeWindow is the bead type of a window.
const typeWindow model.BeadType = "window"

// Window bead fields.
const (
	windowFieldTarget       = "target"
	windowFieldTargetKind   = "target_kind"
	windowFieldLand         = "land"
	windowFieldTokenHash    = "token_hash"
	windowFieldExpiresAt    = "expires_at"
	windowFieldCreatedBy    = "created_by"
	windowFieldRevokedAt    = "revoked_at"
	windowFieldRevokedBy    = "revoked_by"
	windowFieldViewCount    = "view_count"
	windowFieldLastViewedAt = "last_viewed_at"
)

const (
	// windowDefaultDays is a window's lifetime when the mint names none.
	windowDefaultDays = 30
	// windowMaxDays is the longest a window may live. There is no "never".
	windowMaxDays = 365
	// windowMaxLivePerLand caps a land's live (unrevoked, unexpired) windows.
	windowMaxLivePerLand = 20
	// windowViewFlushInterval is how often a window's view count is written.
	windowViewFlushInterval = 5 * time.Minute
	// windowViewsHeader lets the viewer say how many page views one daemon
	// read stands for (its cache serves the rest); 1 when absent. 0 is a
	// read no visitor opened the page for (the page's 20 s refresh, an
	// unfurl bot), which counts nothing and leaves last_viewed_at alone.
	windowViewsHeader = "X-Window-Views"
	// windowViewsMax caps windowViewsHeader per read.
	windowViewsMax = 1000
)

// Window target kinds (target_kind). Absent means a plan.
const (
	windowTargetPlan = "plan"
	windowTargetLand = "land"
)

// actionWindowManage names the mint, list and revoke actions in denials.
const actionWindowManage = "window_manage"

// windowFieldSet is the decoded fields of a window bead.
type windowFieldSet struct {
	Target       string  `json:"target"`
	TargetKind   string  `json:"target_kind"`
	Land         string  `json:"land"`
	TokenHash    string  `json:"token_hash"`
	ExpiresAt    string  `json:"expires_at"`
	CreatedBy    string  `json:"created_by"`
	RevokedAt    string  `json:"revoked_at"`
	RevokedBy    string  `json:"revoked_by"`
	ViewCount    float64 `json:"view_count"`
	LastViewedAt string  `json:"last_viewed_at"`
}

func windowFields(b *model.Bead) windowFieldSet {
	var f windowFieldSet
	_ = json.Unmarshal(b.Fields, &f)
	return f
}

// kind is the window's target kind: windowTargetLand, else windowTargetPlan.
func (f windowFieldSet) kind() string {
	if f.TargetKind == windowTargetLand {
		return windowTargetLand
	}
	return windowTargetPlan
}

func (f windowFieldSet) expiresAt() time.Time {
	t, _ := time.Parse(time.RFC3339Nano, f.ExpiresAt)
	return t
}

// windowLive reports whether a window bead admits its bearer at now: a window
// bead, open, unrevoked, with a target and an expiry still ahead.
func windowLive(b *model.Bead, now time.Time) bool {
	if b == nil || b.Type != typeWindow || b.Status == model.StatusClosed {
		return false
	}
	f := windowFields(b)
	exp := f.expiresAt()
	return f.Target != "" && f.RevokedAt == "" && !exp.IsZero() && now.Before(exp)
}

// windowRecord is a window as its owner sees it. It never carries the token or
// its hash. PlanID is empty on a land window.
type windowRecord struct {
	ID           string     `json:"id"`
	TargetKind   string     `json:"target_kind"`
	PlanID       string     `json:"plan_id"`
	Land         string     `json:"land"`
	State        string     `json:"state"` // live, expired or revoked
	CreatedAt    time.Time  `json:"created_at"`
	CreatedBy    string     `json:"created_by"`
	ExpiresAt    time.Time  `json:"expires_at"`
	RevokedAt    *time.Time `json:"revoked_at,omitempty"`
	RevokedBy    string     `json:"revoked_by,omitempty"`
	ViewCount    int64      `json:"view_count"`
	LastViewedAt *time.Time `json:"last_viewed_at,omitempty"`
}

func (s *BeadsServer) windowRecordOf(b *model.Bead, now time.Time) windowRecord {
	f := windowFields(b)
	rec := windowRecord{
		ID: b.ID, TargetKind: f.kind(), Land: f.Land, State: "live",
		CreatedAt: b.CreatedAt, CreatedBy: f.CreatedBy, ExpiresAt: f.expiresAt(),
		RevokedBy: f.RevokedBy, ViewCount: int64(f.ViewCount),
	}
	if t, err := time.Parse(time.RFC3339Nano, f.RevokedAt); err == nil {
		rec.RevokedAt = &t
	}
	if t, err := time.Parse(time.RFC3339Nano, f.LastViewedAt); err == nil {
		rec.LastViewedAt = &t
	}
	if rec.TargetKind == windowTargetPlan {
		rec.PlanID = f.Target
	}
	pending, last := s.windowViewCounter().pending(b.ID)
	rec.ViewCount += pending
	if !last.IsZero() && (rec.LastViewedAt == nil || last.After(*rec.LastViewedAt)) {
		rec.LastViewedAt = &last
	}
	switch {
	case f.RevokedAt != "" || b.Status == model.StatusClosed:
		rec.State = "revoked"
	case !windowLive(b, now):
		rec.State = "expired"
	}
	return rec
}

// windowManageDenial authorizes a mint, list or revoke in land: auth-disabled
// mode, an unrestricted operator, or a scoped human at the owner tier of land.
// An empty land (not yet known) refuses everyone but an operator, and its
// message names no land.
func windowManageDenial(ctx context.Context, land string) *AuthzDenial {
	msg := "only the owner of land " + land + " or an operator, on their own sign-in, may manage its windows"
	if land == "" {
		msg = "only a land's owner or an operator, on their own sign-in, may manage its windows"
	}
	refused := &AuthzDenial{
		Action:        actionWindowManage,
		BeadType:      string(typeWindow),
		RequiredScope: land,
		Message:       msg,
	}
	switch authMethodFromContext(ctx) {
	case authMethodNone:
		return nil
	case authMethodOIDC:
	default:
		return refused
	}
	if !isScopedPrincipal(ctx) {
		if hasMinimumRole(RolesFromContext(ctx), RoleOperator) {
			return nil
		}
		return refused
	}
	if land != "" && scopeFromContext(ctx).Tiers[land] == tierOwner {
		return nil
	}
	return refused
}

// windowCallerDenial is windowManageDenial before the land is resolved: it
// refuses a caller who could manage no land's windows, so the routes answer
// them without a lookup. land is the land the request names, if it names one
// before its body is read ("" otherwise), so the refusal can name it.
func windowCallerDenial(ctx context.Context, land string) *AuthzDenial {
	if isScopedPrincipal(ctx) && authMethodFromContext(ctx) == authMethodOIDC {
		for _, t := range scopeFromContext(ctx).Tiers {
			if t == tierOwner {
				return nil
			}
		}
	}
	return windowManageDenial(ctx, land)
}

// createWindowRequest is the body of POST /v1/windows: exactly one of
// plan_id and land.
type createWindowRequest struct {
	PlanID        string `json:"plan_id,omitempty"`
	Land          string `json:"land,omitempty"`
	ExpiresInDays int    `json:"expires_in_days,omitempty"`
}

// createWindowResponse returns the token once, with the URL path the viewer
// serves it on.
type createWindowResponse struct {
	Window windowRecord `json:"window"`
	Token  string       `json:"token"`
	Path   string       `json:"path"`
}

// handleCreateWindow handles POST /v1/windows.
func (s *BeadsServer) handleCreateWindow(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if d := windowCallerDenial(ctx, ""); d != nil {
		s.auditDenial(r, d, "windows")
		writeAuthzDenial(w, d)
		return
	}
	var req createWindowRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	days, ok := windowDays(w, req.ExpiresInDays)
	if !ok {
		return
	}
	target, ok := s.windowManagedTarget(w, r, "handleCreateWindow", req.PlanID, req.Land)
	if !ok {
		return
	}
	land := target.land
	labels := []string{"project:" + land}
	if d := frozenLandDenial(ctx, s.store, "create", typeWindow, labels); d != nil {
		writeAuthzDenial(w, d)
		return
	}
	now := time.Now().UTC()
	live, err := s.landWindows(ctx, land, true)
	if err != nil {
		writeServerError(ctx, w, "handleCreateWindow", err, "failed to count the land's windows")
		return
	}
	if n := countLiveWindows(live, now); n >= windowMaxLivePerLand {
		writeError(w, http.StatusConflict, "land "+land+" already has "+strconv.Itoa(n)+" live windows (the most is 20): revoke one first")
		return
	}
	token, hash, err := newWindowToken()
	if err != nil {
		writeServerError(ctx, w, "handleCreateWindow", err, "failed to mint the window")
		return
	}
	actor := actorFromContext(ctx)
	fields, _ := json.Marshal(map[string]any{
		windowFieldTarget:     target.bead.ID,
		windowFieldTargetKind: target.kind,
		windowFieldLand:       land,
		windowFieldTokenHash:  hash,
		windowFieldExpiresAt:  now.AddDate(0, 0, days).Format(time.RFC3339),
		windowFieldCreatedBy:  actor,
		windowFieldViewCount:  0,
	})
	title, short := "Window on "+target.bead.ID, "window on a plan"
	if target.kind == windowTargetLand {
		title, short = "Window on land "+land, "window on a land"
	}
	// type:window is admin-gated; windowManageDenial above is this write's
	// authorization (see authorizeWriteTitled's call-site list).
	bead, err := s.createBead(withMachineWrite(ctx), createBeadInput{
		Title:        title,
		ShortTitle:   short,
		Kind:         string(model.KindConfig),
		Type:         string(typeWindow),
		Labels:       labels,
		CreatedBy:    actor,
		Fields:       fields,
		ParentBeadID: target.bead.ID,
	})
	if err != nil {
		writeServerError(ctx, w, "handleCreateWindow", err, "failed to mint the window")
		return
	}
	writeJSON(w, http.StatusCreated, createWindowResponse{
		Window: s.windowRecordOf(bead, now), Token: token, Path: "/w/" + token,
	})
}

// windowDays resolves a requested lifetime in days (0 is the default),
// answering 400 itself when it is out of range.
func windowDays(w http.ResponseWriter, days int) (int, bool) {
	if days == 0 {
		days = windowDefaultDays
	}
	if days < 1 || days > windowMaxDays {
		writeError(w, http.StatusBadRequest, "expires_in_days must be between 1 and 365")
		return 0, false
	}
	return days, true
}

// windowTarget is what a mint or a preview names: a plan epic or a land's
// project bead, with its kind and its land.
type windowTarget struct {
	bead *model.Bead
	kind string
	land string
}

// windowManagedTarget resolves the target a mint or a preview names, a plan
// (planID) or a land (land) but not both, and authorizes the caller to manage
// its land's windows. It answers the request itself when it returns false.
func (s *BeadsServer) windowManagedTarget(w http.ResponseWriter, r *http.Request, op, planID, land string) (windowTarget, bool) {
	planID, land = strings.TrimSpace(planID), strings.TrimSpace(land)
	switch {
	case planID != "" && land != "":
		writeError(w, http.StatusBadRequest, "name a plan_id or a land, not both")
		return windowTarget{}, false
	case land != "":
		proj, ok := s.windowManagedLand(w, r, op, land)
		return windowTarget{bead: proj, kind: windowTargetLand, land: land}, ok
	}
	plan, land, ok := s.windowManagedPlan(w, r, op, planID)
	return windowTarget{bead: plan, kind: windowTargetPlan, land: land}, ok
}

// windowManagedLand resolves a land's type:project bead for a land window and
// authorizes the caller to manage the land's windows. A scoped caller learns
// nothing about a land outside its scope. It answers the request itself when
// it returns false.
func (s *BeadsServer) windowManagedLand(w http.ResponseWriter, r *http.Request, op, land string) (*model.Bead, bool) {
	ctx := r.Context()
	if !scopeFromContext(ctx).allows(land) {
		writeError(w, http.StatusNotFound, "no such land")
		return nil, false
	}
	proj, err := s.store.FindProjectByName(ctx, land)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (proj == nil || proj.Type != model.TypeProject)) {
		writeError(w, http.StatusNotFound, "no such land")
		return nil, false
	}
	if err != nil {
		writeServerError(ctx, w, op, err, "failed to read the land")
		return nil, false
	}
	if d := windowManageDenial(ctx, land); d != nil {
		s.auditDenial(r, d, proj.ID)
		writeAuthzDenial(w, d)
		return nil, false
	}
	return proj, true
}

// windowManagedPlan resolves the plan a mint or a preview names and its land,
// and authorizes the caller to manage that land's windows. It answers the
// request itself when it returns false.
func (s *BeadsServer) windowManagedPlan(w http.ResponseWriter, r *http.Request, op, planID string) (*model.Bead, string, bool) {
	ctx := r.Context()
	planID = strings.TrimSpace(planID)
	if planID == "" {
		writeError(w, http.StatusBadRequest, "plan_id or land is required")
		return nil, "", false
	}
	plan, err := s.store.GetBead(ctx, planID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && plan == nil) {
		writeError(w, http.StatusNotFound, "no such plan")
		return nil, "", false
	}
	if err != nil {
		writeServerError(ctx, w, op, err, "failed to read the plan")
		return nil, "", false
	}
	lands := projectLabelValues(plan.Labels)
	// A scoped caller learns nothing about a plan outside its lands.
	if len(lands) == 1 && !scopeFromContext(ctx).allows(lands[0]) {
		writeError(w, http.StatusNotFound, "no such plan")
		return nil, "", false
	}
	if plan.Type != model.TypeEpic {
		writeError(w, http.StatusBadRequest, "a window is opened on a plan epic")
		return nil, "", false
	}
	if len(lands) != 1 {
		writeError(w, http.StatusBadRequest, "the plan needs exactly one project label to say whose land it is in")
		return nil, "", false
	}
	land := lands[0]
	if d := windowManageDenial(ctx, land); d != nil {
		s.auditDenial(r, d, plan.ID)
		writeAuthzDenial(w, d)
		return nil, "", false
	}
	return plan, land, true
}

// handlePreviewWindow handles GET /v1/windows/preview?plan=<id> (or
// ?land=<land>): the projection a window minted now on the plan (or the land)
// would serve, built by the same buildWindowView (or buildLandWindowView), for
// the owner's preview (PW.4). It mints nothing, writes nothing and counts no
// view, and admits exactly the callers a mint admits.
func (s *BeadsServer) handlePreviewWindow(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	if d := windowCallerDenial(ctx, strings.TrimSpace(q.Get("land"))); d != nil {
		s.auditDenial(r, d, "windows")
		writeAuthzDenial(w, d)
		return
	}
	n := 0
	if raw := strings.TrimSpace(q.Get("expires_in_days")); raw != "" {
		var err error
		if n, err = strconv.Atoi(raw); err != nil {
			writeError(w, http.StatusBadRequest, "expires_in_days must be a whole number of days")
			return
		}
	}
	days, ok := windowDays(w, n)
	if !ok {
		return
	}
	target, ok := s.windowManagedTarget(w, r, "handlePreviewWindow", q.Get("plan"), q.Get("land"))
	if !ok {
		return
	}
	expires := time.Now().UTC().AddDate(0, 0, days).Format(time.RFC3339)
	view, err := s.buildTargetWindowView(ctx, windowFieldSet{
		Target: target.bead.ID, TargetKind: target.kind, Land: target.land, ExpiresAt: expires,
	})
	switch {
	case errors.Is(err, errWindowClosureTooLarge):
		writeError(w, http.StatusUnprocessableEntity, "this plan is too large to show through a window")
		return
	case err != nil:
		writeServerError(ctx, w, "handlePreviewWindow", err, "failed to read the window's target")
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, http.StatusOK, view)
}

// buildTargetWindowView builds the view for a window's target kind: the land
// projection for a land window, the plan projection otherwise.
func (s *BeadsServer) buildTargetWindowView(ctx context.Context, wf windowFieldSet) (any, error) {
	if wf.kind() == windowTargetLand {
		return s.buildLandWindowView(ctx, wf)
	}
	return s.buildWindowView(ctx, wf)
}

// handleListWindows handles GET /v1/windows?land=<land>.
func (s *BeadsServer) handleListWindows(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	land := strings.TrimSpace(r.URL.Query().Get("land"))
	if d := windowCallerDenial(ctx, land); d != nil {
		s.auditDenial(r, d, "windows")
		writeAuthzDenial(w, d)
		return
	}
	if land == "" {
		writeError(w, http.StatusBadRequest, "land is required")
		return
	}
	if d := windowManageDenial(ctx, land); d != nil {
		s.auditDenial(r, d, land)
		writeAuthzDenial(w, d)
		return
	}
	beads, err := s.landWindows(ctx, land, false)
	if err != nil {
		writeServerError(ctx, w, "handleListWindows", err, "failed to list windows")
		return
	}
	now := time.Now()
	out := make([]windowRecord, 0, len(beads))
	for _, b := range beads {
		out = append(out, s.windowRecordOf(b, now))
	}
	writeJSON(w, http.StatusOK, map[string]any{"windows": out})
}

// handleRevokeWindow handles DELETE /v1/windows/{id}. Revoking a revoked
// window answers its record again.
func (s *BeadsServer) handleRevokeWindow(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if d := windowCallerDenial(ctx, ""); d != nil {
		s.auditDenial(r, d, "windows")
		writeAuthzDenial(w, d)
		return
	}
	b, err := s.store.GetBead(ctx, r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (b == nil || b.Type != typeWindow)) {
		writeError(w, http.StatusNotFound, "no such window")
		return
	}
	if err != nil {
		writeServerError(ctx, w, "handleRevokeWindow", err, "failed to read the window")
		return
	}
	f := windowFields(b)
	if !scopeFromContext(ctx).allows(f.Land) {
		writeError(w, http.StatusNotFound, "no such window")
		return
	}
	if d := windowManageDenial(ctx, f.Land); d != nil {
		s.auditDenial(r, d, b.ID)
		writeAuthzDenial(w, d)
		return
	}
	now := time.Now().UTC()
	actor := actorFromContext(ctx)
	if f.RevokedAt == "" {
		set := map[string]any{windowFieldRevokedAt: now.Format(time.RFC3339), windowFieldRevokedBy: actor}
		updated, err := s.store.MergeBeadFields(ctx, b.ID, set)
		if err != nil || updated == nil {
			writeServerError(ctx, w, "handleRevokeWindow", err, "failed to revoke the window")
			return
		}
		s.recordAndPublish(ctx, events.TopicBeadUpdated, b.ID, actor, events.BeadUpdated{Bead: updated, Changes: map[string]any{"fields": set}, Actor: actor})
		b = updated
	}
	if b.Status != model.StatusClosed {
		prev := b.Status
		closed, err := s.store.CloseBead(ctx, b.ID, actor)
		if err != nil || closed == nil {
			writeServerError(ctx, w, "handleRevokeWindow", err, "failed to close the revoked window")
			return
		}
		s.recordAndPublish(ctx, events.TopicBeadClosed, closed.ID, actor, events.BeadClosed{Bead: closed, ClosedBy: actor})
		s.recordMutation(ctx, closed.ID, actor, "status", string(prev), string(closed.Status), model.MutationTypeUpdate)
		b = closed
	}
	writeJSON(w, http.StatusOK, map[string]any{"window": s.windowRecordOf(b, now)})
}

// handleWindowView handles GET /v1/windows/self/view: the window principal's
// projection of its plan, or of its land (dispatched on target_kind). Every
// other caller is refused; a window whose target is gone answers 401 like an
// unknown one.
func (s *BeadsServer) handleWindowView(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	win, ok := windowPrincipalFromContext(ctx)
	if !ok {
		writeError(w, http.StatusForbidden, "only a window bearer reads this route")
		return
	}
	view, err := s.buildTargetWindowView(ctx, windowFields(win))
	switch {
	case errors.Is(err, errWindowTargetGone):
		writeError(w, http.StatusUnauthorized, "invalid token")
		return
	case errors.Is(err, errWindowClosureTooLarge):
		writeError(w, http.StatusUnprocessableEntity, "this plan is too large to show through a window")
		return
	case err != nil:
		writeServerError(ctx, w, "handleWindowView", err, "failed to read the window's target")
		return
	}
	views := int64(1)
	if n, err := strconv.ParseInt(r.Header.Get(windowViewsHeader), 10, 64); err == nil && n >= 0 {
		views = min(n, windowViewsMax)
	}
	if views > 0 {
		s.windowViewCounter().note(win.ID, views, time.Now().UTC(), s.flushWindowViews)
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, http.StatusOK, view)
}

// landWindows lists a land's window beads, newest first (open ones only when
// liveOnly).
func (s *BeadsServer) landWindows(ctx context.Context, land string, liveOnly bool) ([]*model.Bead, error) {
	f := model.BeadFilter{
		Type:   []model.BeadType{typeWindow},
		Fields: map[string]string{windowFieldLand: land},
		Sort:   "-created_at",
		Limit:  500,
	}
	if liveOnly {
		f.Status = []model.Status{model.StatusOpen, model.StatusInProgress}
	}
	beads, _, err := s.store.ListBeads(ctx, f)
	return beads, err
}

func countLiveWindows(beads []*model.Bead, now time.Time) int {
	n := 0
	for _, b := range beads {
		if windowLive(b, now) {
			n++
		}
	}
	return n
}

// windowViewCounter returns the server's view counter, made on first use.
func (s *BeadsServer) windowViewCounter() *windowViewTally {
	windowViewsInitMu.Lock()
	defer windowViewsInitMu.Unlock()
	if s.windowViews == nil {
		s.windowViews = &windowViewTally{windows: map[string]*windowViews{}}
	}
	return s.windowViews
}

// windowViewsInitMu guards BeadsServer.windowViews' first use.
var windowViewsInitMu sync.Mutex

// windowViewTally keeps view counts in memory and writes each window's at
// most every windowViewFlushInterval, so no bead is written on the request
// path (a bead write fans out to every subscriber of the plan).
type windowViewTally struct {
	mu      sync.Mutex
	windows map[string]*windowViews
}

type windowViews struct {
	pending         int64
	last, lastFlush time.Time
	timer           *time.Timer
}

// note adds views to a window's tally and arms a flush for its interval's end.
func (t *windowViewTally) note(id string, views int64, at time.Time, flush func(id string, views int64, last time.Time)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	v := t.windows[id]
	if v == nil {
		v = &windowViews{}
		t.windows[id] = v
	}
	v.pending += views
	v.last = at
	if v.timer != nil {
		return
	}
	wait := max(time.Until(v.lastFlush.Add(windowViewFlushInterval)), 0)
	v.timer = time.AfterFunc(wait, func() {
		t.mu.Lock()
		n, last := v.pending, v.last
		v.pending, v.timer, v.lastFlush = 0, nil, time.Now()
		t.mu.Unlock()
		if n > 0 {
			flush(id, n, last)
		}
	})
}

// pending returns the views not yet written for id and the newest view time.
func (t *windowViewTally) pending(id string) (int64, time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if v := t.windows[id]; v != nil {
		return v.pending, v.last
	}
	return 0, time.Time{}
}

// flushWindowViews writes a window's tally in one transaction, last_viewed_at
// before the view_count increment, so a new count has its time (gd-Wyk2a285jn).
func (s *BeadsServer) flushWindowViews(id string, views int64, last time.Time) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const actor = "system:window-views"
	set := map[string]any{windowFieldLastViewedAt: last.UTC().Format(time.RFC3339)}
	var updated *model.Bead
	err := s.store.RunInTransaction(ctx, func(tx store.Store) error {
		b, err := tx.MergeBeadFields(ctx, id, set)
		if err == nil && b != nil {
			updated, err = tx.IncrementBeadFields(ctx, id, map[string]float64{windowFieldViewCount: float64(views)})
		}
		return err
	})
	if err != nil || updated == nil {
		slog.Warn("window view flush failed", "window", id, "error", err)
		return
	}
	s.recordAndPublish(ctx, events.TopicBeadUpdated, id, actor, events.BeadUpdated{
		Bead: updated, Changes: map[string]any{windowFieldViewCount: views, windowFieldLastViewedAt: set[windowFieldLastViewedAt]}, Actor: actor,
	})
}
