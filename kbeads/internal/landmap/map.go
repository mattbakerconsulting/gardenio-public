package landmap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"gardenio/kbeads/internal/model"
	"gardenio/lands/contract/landsv1"
)

// BaseURL is where the daemon mounts the contract's /lands/v1/ paths. Under
// /v1, the land door's /land/<name>/v1/ prefix (which the edge rewrites to
// /v1/) reaches them, so gl's base URL is https://<edge>/land/<name>/v1.
const BaseURL = "/v1"

// Lands is what the map asks the server about a land itself, which no daemon
// route a tenant can reach answers.
type Lands interface {
	// Applicant reports whether the caller is a signed-in stranger: no
	// gardenio role and no land. The Gate refuses such a caller every route
	// (Lands Track B 7.5), so the map answers forbidden before anything else.
	Applicant(ctx context.Context) bool
	// Admits reports whether the caller may read the land's beads.
	Admits(ctx context.Context, land string) bool
	// Land returns the land's record; ok is false when there is no such land.
	Land(ctx context.Context, land string) (rec LandRecord, ok bool, err error)
	// Plot returns the repository the land works in; ok is false when there
	// is no such land or it names none.
	Plot(ctx context.Context, land string) (rec PlotRecord, ok bool, err error)
	// Caller reports the caller's class, for Requests' caller label.
	Caller(ctx context.Context) Caller
	// Budget returns the purse snapshot the controller's purse watcher stamps
	// on the land's account bead; ok is false when the land has no purse (no
	// such land, or an operator land). A tenant land not stamped yet is ok
	// with an empty record.
	Budget(ctx context.Context, land string) (rec BudgetRecord, ok bool, err error)
	// BudgetHistory returns the land's credits per UTC day for the last days
	// days, today included, oldest first; ok is false as for Budget. err is
	// set only when no source could answer.
	BudgetHistory(ctx context.Context, land string, days int) (rec BudgetHistoryRecord, ok bool, err error)

	// The agents members run on their own machines (agents.go). Each judges
	// the caller itself and returns ErrAgentForbidden, ErrAgentNotFound or
	// ErrAgentConflict for a refusal.
	//
	// Agents lists the land's, newest first.
	Agents(ctx context.Context, land string) ([]AgentRecord, error)
	// Agent returns one; ok is false when id is not one of the land's.
	Agent(ctx context.Context, land, id string) (rec AgentRecord, ok bool, err error)
	// MintAgent starts an agent for the caller and mints its credential.
	MintAgent(ctx context.Context, land string, req AgentMintRequest) (AgentGrantRecord, error)
	// RefreshAgent spends the agent's refresh for a new credential.
	RefreshAgent(ctx context.Context, land, id, refresh string) (AgentGrantRecord, error)
	// RevokeAgent stops the agent and kills its credential, answering it as
	// it now is.
	RevokeAgent(ctx context.Context, land, id string) (AgentRecord, error)
}

// LandRecord is the part of a land the contract shows. A land's id is its
// name: its backing record's id is in the operator's namespace, not the
// land's.
type LandRecord struct {
	CreatedAt time.Time
	Paused    bool
}

// PlotRecord is a land's repository. The map shows its name and branch, never
// its address.
type PlotRecord struct {
	GitURL        string
	DefaultBranch string
}

// BudgetRecord is a tenant land's purse snapshot (D.6c gd-uAiEaCwsJJ), as the
// purse watcher (gardenio/controller/internal/lands/watcher) stamped it: the
// granted and used credits (nil when not written, or written empty because
// spend could not be computed), the purse state (ok, wall or unknown) and
// when the purse was read.
type BudgetRecord struct {
	Granted, Used *float64
	State         string
	ReadAt        time.Time
}

// BudgetHistoryRecord is a tenant land's daily credit history: one entry per
// UTC day something was recorded, oldest first. Stale is set when the
// credit series (vmalert's gardenio:land_spend_credits:*) could not be read
// and the days come from the purse snapshots recorded on the account bead.
type BudgetHistoryRecord struct {
	Days  []BudgetDayRecord
	Stale bool
}

// BudgetDayRecord is one UTC day: the credits spent that day and the credits
// remaining at its end (now, for today). Day is midnight UTC.
type BudgetDayRecord struct {
	Day              time.Time
	Spent, Remaining float64
}

// maxHistoryDays is the contract's cap on getBudgetHistory's days, and its
// default.
const maxHistoryDays = 30

// purseOK is the purse state of a land that may start new work; any other
// state (wall, unknown) refuses new spawns.
const purseOK = "ok"

// purseUnknownAfter is how old a purse snapshot may be before it reads as
// exhausted. It is purse.UnknownAfter (gardenio/controller/internal/lands/purse),
// copied because this module cannot import it: a snapshot that old means
// nobody has read the purse, and an unknown purse is the wall for new spawns
// (Lands Track B §3.4: fail closed is the rule for a tenant's money).
const purseUnknownAfter = 15 * time.Minute

// Map implements landsv1.ServerInterface over the daemon's own routes.
type Map struct {
	d     daemon
	lands Lands
	feed  *feed
	// pictures is where decision evidence pictures are read (evidence.go);
	// nil serves none.
	pictures *Pictures
}

var _ landsv1.ServerInterface = (*Map)(nil)

// New returns the map. daemonMux is the daemon's route mux below the auth
// chain; the map calls its routes in-process with the caller's context.
// changes is the daemon's event stream the land feed is cut from (nil: the
// feed streams keepalives only).
func New(daemonMux http.Handler, lands Lands, changes Changes) *Map {
	return &Map{d: daemon{h: daemonMux}, lands: lands, feed: newFeed(changes)}
}

// Registrar is the slice of a mux Register needs.
type Registrar interface {
	HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request))
}

// Register registers every contract route, under BaseURL, on mux.
func Register(mux Registrar, m *Map) {
	landsv1.HandlerWithOptions(m, landsv1.StdHTTPServerOptions{
		BaseURL:    BaseURL,
		BaseRouter: registrarMux{mux, m.lands},
		ErrorHandlerFunc: func(w http.ResponseWriter, _ *http.Request, err error) {
			// The generated parameter binder's own text: the request validator.
			problem(w, landsv1.InvalidRequest, err.Error())
		},
	})
}

// registrarMux lets the generated registration run on a Registrar, and puts
// the applicant refusal in front of every route, ahead of the generated
// parameter binding. The daemon's mux serves the routes; this one never does.
// It also names the request's op and caller on its tally (Guard's, or a fresh
// one when nothing counts the request) and hands the tally on with the
// response writer.
type registrarMux struct {
	r     Registrar
	lands Lands
}

func (g registrarMux) HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	op := operationID(handler)
	g.r.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		t := tallyOf(r.Context())
		if t == nil {
			t = &tally{}
		}
		t.op = op
		w = &tallyWriter{ResponseWriter: w, t: t}
		if g.lands.Applicant(r.Context()) {
			problem(w, landsv1.Forbidden, "")
			return
		}
		t.caller = g.lands.Caller(r.Context())
		handler(w, r)
	})
}

func (registrarMux) ServeHTTP(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }

// landName is the contract's LandName.
var landName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

const (
	defaultLimit = 50
	maxLimit     = 200
	maxBodyBytes = 1 << 20
	// maxPlanSteps is the contract's Plan.steps maxItems.
	maxPlanSteps = 500
)

// admit answers not_found, the same for a land that does not exist and one
// the caller is not in, unless the caller may read the land. An admitted land
// is the request's land in Requests.
func (m *Map) admit(w http.ResponseWriter, r *http.Request, land string) bool {
	if !landName.MatchString(land) || !m.lands.Admits(r.Context(), land) {
		problem(w, landsv1.NotFound, "")
		return false
	}
	tallyFromWriter(w).admitted(land)
	return true
}

// writable is admit plus the land's kill switch: a paused land takes no
// writes (the daemon refuses them too; this gives the tenant the right code).
func (m *Map) writable(w http.ResponseWriter, r *http.Request, op, land string) bool {
	if !m.admit(w, r, land) {
		return false
	}
	rec, ok, err := m.lands.Land(r.Context(), land)
	if err != nil {
		fail(w, r, op, err)
		return false
	}
	if ok && rec.Paused {
		problem(w, landsv1.LandPaused, "")
		return false
	}
	return true
}

// decodeBody decodes a closed request body: unknown fields are refused.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		problem(w, landsv1.InvalidRequest, "request body: "+err.Error())
		return false
	}
	if dec.More() {
		problem(w, landsv1.InvalidRequest, "request body: more than one JSON value")
		return false
	}
	return true
}

// textOK checks a string's length against the contract's bounds.
func textOK(w http.ResponseWriter, name, s string, minLen, maxLen int) bool {
	n := utf8.RuneCountInString(s)
	if n < minLen || n > maxLen {
		problem(w, landsv1.InvalidRequest, name+" must be "+strconv.Itoa(minLen)+" to "+strconv.Itoa(maxLen)+" characters")
		return false
	}
	return true
}

func priorityOK(w http.ResponseWriter, p *landsv1.Priority) bool {
	if p != nil && (*p < 1 || *p > 4) {
		problem(w, landsv1.InvalidRequest, "priority must be 1 to 4")
		return false
	}
	return true
}

// getBead reads one bead and answers not_found unless it is in the land and
// one of the given types. The daemon's own read gate runs first.
func (m *Map) getBead(w http.ResponseWriter, r *http.Request, op, land, id string, types ...model.BeadType) (*beadRecord, bool) {
	var b beadRecord
	if err := m.d.call(r, rGetBead, []string{id}, nil, nil, &b); err != nil {
		var ue *upstreamError
		if errors.As(err, &ue) && ue.status == http.StatusForbidden {
			// Out of scope reads the same as absent: no oracle for other lands.
			problem(w, landsv1.NotFound, "")
			return nil, false
		}
		fail(w, r, op, err)
		return nil, false
	}
	if !b.inLand(land) || !slices.Contains(types, b.Type) {
		problem(w, landsv1.NotFound, "")
		return nil, false
	}
	return &b, true
}

// list reads one page of land beads. keep drops records after the read (a
// status the daemon cannot filter on); the page may then be short, and the
// cursor still moves past everything read.
func (m *Map) list(w http.ResponseWriter, r *http.Request, op, land string, q url.Values, cursor *landsv1.Cursor, limit *int, types []model.BeadType, keep func(*beadRecord) bool) ([]beadRecord, *landsv1.Cursor, bool) {
	off, n, ok := pageParams(w, cursor, limit)
	if !ok {
		return nil, nil, false
	}
	if !q.Has("labels") {
		q.Set("labels", "project:"+land)
	}
	q.Set("type", joinTypes(types))
	q.Set("limit", strconv.Itoa(n))
	q.Set("offset", strconv.Itoa(off))
	var pg beadPage
	if err := m.d.call(r, rListBeads, nil, q, nil, &pg); err != nil {
		fail(w, r, op, err)
		return nil, nil, false
	}
	out := make([]beadRecord, 0, len(pg.Beads))
	for i := range pg.Beads {
		b := &pg.Beads[i]
		if !b.inLand(land) || !slices.Contains(types, b.Type) || (keep != nil && !keep(b)) {
			continue
		}
		out = append(out, *b)
	}
	var next *landsv1.Cursor
	if read := off + len(pg.Beads); len(pg.Beads) > 0 && read < pg.Total {
		c := encodeCursor(read)
		next = &c
	}
	return out, next, true
}

// pageParams reads a list's cursor and limit: the offset to start at and the
// page size.
func pageParams(w http.ResponseWriter, cursor *landsv1.Cursor, limit *int) (off, n int, ok bool) {
	off, ok = decodeCursor(cursor)
	if !ok {
		problem(w, landsv1.InvalidRequest, "cursor is not one this server issued")
		return 0, 0, false
	}
	n = defaultLimit
	if limit != nil {
		if *limit < 1 || *limit > maxLimit {
			problem(w, landsv1.InvalidRequest, "limit must be 1 to 200")
			return 0, 0, false
		}
		n = *limit
	}
	return off, n, true
}

// pageOf cuts one page of n items at off out of all, and the cursor past it.
func pageOf[T any](all []T, off, n int) ([]T, *landsv1.Cursor) {
	items := all[min(off, len(all)):]
	var next *landsv1.Cursor
	if len(items) > n {
		items = items[:n]
		c := encodeCursor(off + n)
		next = &c
	}
	return append([]T{}, items...), next
}

func joinTypes(types []model.BeadType) string {
	s := make([]string, len(types))
	for i, t := range types {
		s[i] = string(t)
	}
	return strings.Join(s, ",")
}

func joinStatuses(st []model.Status) string {
	s := make([]string, len(st))
	for i, x := range st {
		s[i] = string(x)
	}
	return strings.Join(s, ",")
}

// deferred answers a verb the map does not serve yet (Verb.Deferred).
func deferred(w http.ResponseWriter) { problem(w, landsv1.NotFound, "") }

// ── land ─────────────────────────────────────────────────────────────────

func (m *Map) GetLand(w http.ResponseWriter, r *http.Request, land landsv1.LandPath) {
	if !m.admit(w, r, land) {
		return
	}
	rec, ok, err := m.lands.Land(r.Context(), land)
	if err != nil {
		fail(w, r, "getLand", err)
		return
	}
	if !ok {
		problem(w, landsv1.NotFound, "")
		return
	}
	out := landsv1.Land{Id: land, Name: land, Status: landsv1.LandStatusActive, CreatedAt: stamp(rec.CreatedAt)}
	if rec.Paused {
		out.Status = landsv1.LandStatusPaused
	}
	writeJSON(w, http.StatusOK, out)
}

// plots is the land's plots: none, or the one repository it works in.
func (m *Map) plots(w http.ResponseWriter, r *http.Request, op, land string) ([]landsv1.Plot, bool) {
	if !m.admit(w, r, land) {
		return nil, false
	}
	rec, ok, err := m.lands.Plot(r.Context(), land)
	if err != nil {
		fail(w, r, op, err)
		return nil, false
	}
	if !ok {
		return []landsv1.Plot{}, true
	}
	return []landsv1.Plot{plotFrom(land, rec)}, true
}

func (m *Map) ListPlots(w http.ResponseWriter, r *http.Request, land landsv1.LandPath, params landsv1.ListPlotsParams) {
	off, n, ok := pageParams(w, params.Cursor, params.Limit)
	if !ok {
		return
	}
	all, ok := m.plots(w, r, "listPlots", land)
	if !ok {
		return
	}
	items, next := pageOf(all, off, n)
	writeJSON(w, http.StatusOK, landsv1.PlotPage{Items: items, NextCursor: next})
}

func (m *Map) GetPlot(w http.ResponseWriter, r *http.Request, land landsv1.LandPath, plot landsv1.Id) {
	all, ok := m.plots(w, r, "getPlot", land)
	if !ok {
		return
	}
	for _, p := range all {
		if p.Id == plot {
			writeJSON(w, http.StatusOK, p)
			return
		}
	}
	problem(w, landsv1.NotFound, "")
}

// GetBudget answers with the land's purse snapshot. The status is the
// tenant's answer to "may this land start new work": exhausted at the wall
// (which the margin puts a little before every credit is spent), when the
// purse is unknown, and when the snapshot is stale. No snapshot, or one with
// no used figure, is upstream_unavailable, never invented zeros.
func (m *Map) GetBudget(w http.ResponseWriter, r *http.Request, land landsv1.LandPath) {
	if !m.admit(w, r, land) {
		return
	}
	rec, ok, err := m.lands.Budget(r.Context(), land)
	if err != nil {
		fail(w, r, "getBudget", err)
		return
	}
	if !ok {
		problem(w, landsv1.NotFound, "")
		return
	}
	out, ok := toBudget(rec)
	if !ok {
		problem(w, landsv1.UpstreamUnavailable, "")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// toBudget projects a purse snapshot; ok is false for no snapshot, or one
// with no used figure.
func toBudget(rec BudgetRecord) (landsv1.Budget, bool) {
	if rec.Granted == nil || rec.Used == nil || rec.ReadAt.IsZero() {
		return landsv1.Budget{}, false
	}
	out := landsv1.Budget{
		Limit: float32(max(0, *rec.Granted)), Spent: float32(max(0, *rec.Used)),
		Unit: landsv1.Credits, Status: landsv1.Ok,
	}
	if rec.State != purseOK || time.Since(rec.ReadAt) > purseUnknownAfter {
		out.Status = landsv1.Exhausted
	}
	return out, true
}

// GetBudgetHistory answers with the land's daily credits: from the credit
// series, or, marked stale, from the purse's recorded snapshots when the
// series cannot be read. Neither is upstream_unavailable, never invented
// zeros; a land with nothing recorded yet has no points.
func (m *Map) GetBudgetHistory(w http.ResponseWriter, r *http.Request, land landsv1.LandPath, params landsv1.GetBudgetHistoryParams) {
	if !m.admit(w, r, land) {
		return
	}
	days := maxHistoryDays
	if params.Days != nil {
		if *params.Days < 1 || *params.Days > maxHistoryDays {
			problem(w, landsv1.InvalidRequest, "days must be 1 to 30")
			return
		}
		days = *params.Days
	}
	rec, ok, err := m.lands.BudgetHistory(r.Context(), land, days)
	if err != nil {
		fail(w, r, "getBudgetHistory", err)
		return
	}
	if !ok {
		problem(w, landsv1.NotFound, "")
		return
	}
	out := landsv1.BudgetHistory{Unit: landsv1.Credits, Points: make([]landsv1.BudgetDay, 0, len(rec.Days))}
	if rec.Stale {
		out.Stale = &rec.Stale
	}
	// The last days days, today included; the source decides which have data.
	first := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, 1-days)
	slices.SortFunc(rec.Days, func(a, b BudgetDayRecord) int { return a.Day.Compare(b.Day) })
	for _, d := range rec.Days {
		day := d.Day.UTC().Truncate(24 * time.Hour)
		if day.Before(first) || len(out.Points) == maxHistoryDays {
			continue
		}
		p := landsv1.BudgetDay{Spent: float32(max(0, d.Spent)), Remaining: float32(max(0, d.Remaining))}
		p.Day.Time = day
		out.Points = append(out.Points, p)
	}
	writeJSON(w, http.StatusOK, out)
}

// ── crew ─────────────────────────────────────────────────────────────────

// crew is the land's people, from the daemon's members route (one answer,
// capped there), in its order.
func (m *Map) crew(w http.ResponseWriter, r *http.Request, op, land string) ([]landsv1.Member, bool) {
	if !m.admit(w, r, land) {
		return nil, false
	}
	var rec membersRecord
	if err := m.d.call(r, rLandMembers, []string{land}, nil, nil, &rec); err != nil {
		var ue *upstreamError
		if errors.As(err, &ue) && ue.status == http.StatusNotFound {
			problem(w, landsv1.NotFound, "")
			return nil, false
		}
		fail(w, r, op, err)
		return nil, false
	}
	out := make([]landsv1.Member, 0, len(rec.Members))
	for _, p := range rec.Members {
		name := strings.TrimSpace(p.Name)
		if name == "" {
			name = displayName(p.Principal)
		}
		if name == "" {
			name = "unknown"
		}
		// No presence is recorded for a person yet: every member reads
		// active, as whoami does.
		out = append(out, landsv1.Member{Id: memberID(p.Principal), Name: name, Role: memberRole(p.Role), Status: landsv1.MemberStatusActive})
	}
	return out, true
}

func (m *Map) ListCrew(w http.ResponseWriter, r *http.Request, land landsv1.LandPath, params landsv1.ListCrewParams) {
	off, n, ok := pageParams(w, params.Cursor, params.Limit)
	if !ok {
		return
	}
	all, ok := m.crew(w, r, "listCrew", land)
	if !ok {
		return
	}
	items, next := pageOf(all, off, n)
	writeJSON(w, http.StatusOK, landsv1.MemberPage{Items: items, NextCursor: next})
}

func (m *Map) GetMember(w http.ResponseWriter, r *http.Request, land landsv1.LandPath, member landsv1.Id) {
	all, ok := m.crew(w, r, "getMember", land)
	if !ok {
		return
	}
	for _, p := range all {
		if p.Id == member {
			writeJSON(w, http.StatusOK, p)
			return
		}
	}
	problem(w, landsv1.NotFound, "")
}

// memberID is a member's opaque id: stable for a person, and not their
// address. The members route names a person oidc:<email> and /v1/me by the
// bare email, so both read as the lowercased email first.
func memberID(principal string) string {
	principal = strings.ToLower(strings.TrimSpace(principal))
	principal = strings.TrimPrefix(principal, "oidc:")
	sum := sha256.Sum256([]byte(principal))
	return "m" + hex.EncodeToString(sum[:10])
}

func (m *Map) Whoami(w http.ResponseWriter, r *http.Request, land landsv1.LandPath) {
	if !m.admit(w, r, land) {
		return
	}
	var me meRecord
	if err := m.d.call(r, rMe, nil, nil, nil, &me); err != nil {
		fail(w, r, "whoami", err)
		return
	}
	out := landsv1.Member{
		Id:     memberID(me.OperatorID),
		Name:   displayName(me.OperatorID),
		Role:   landsv1.MemberRoleOther,
		Status: landsv1.MemberStatusActive,
	}
	if out.Name == "" {
		out.Name = "unknown"
	}
	for _, l := range me.Lands {
		if l.Name == land {
			out.Role = memberRole(l.Tier)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// ── thread / message (D.9) ───────────────────────────────────────────────

func (m *Map) ListThreads(w http.ResponseWriter, _ *http.Request, _ landsv1.LandPath, _ landsv1.ListThreadsParams) {
	deferred(w)
}

func (m *Map) GetThread(w http.ResponseWriter, _ *http.Request, _ landsv1.LandPath, _ landsv1.ThreadPath) {
	deferred(w)
}

func (m *Map) ListMessages(w http.ResponseWriter, _ *http.Request, _ landsv1.LandPath, _ landsv1.ThreadPath, _ landsv1.ListMessagesParams) {
	deferred(w)
}

func (m *Map) PostMessage(w http.ResponseWriter, _ *http.Request, _ landsv1.LandPath, _ landsv1.ThreadPath, _ landsv1.PostMessageParams) {
	deferred(w)
}
