package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/gardenhub"
	"gardenio/controller/internal/planwindow"
)

// The Window's page (Lands: the Window, plan gd-pz87vf3xsB, step PW.3
// gd-IZ9mjbOxD0; narrative §3 PW-D6, PW-D8, PW-D9 and §4). A land owner mints
// a window on a plan and shares https://<viewer>/w/gw_<token>; anyone holding
// the link reads the plan, with no account and no cookie.
//
//   - GET /w/{token} is the page, server-rendered and complete with JS off.
//     /w/static/window.js (the only script, no inline script) refetches
//     plan.json every 20 s and, when it changed, swaps the page body for a
//     fresh render and marks the new Recent rows.
//   - GET /w/{token}/plan.json is planwindow.View as JSON, with an ETag.
//   - GET /w/{token}/live (PW.7, window_live.go) is an event stream that says
//     when plan.json changed, so the page updates in about a second; while it
//     is open window.js does not poll, and it polls again whenever the
//     stream is refused, fails or is unsupported.
//
// The token is the only authority. The handlers never consult the request's
// scope (an edge-proven request with no role gets Admin scope from
// scopeFromRequest) and are served outside the auth chain, by mountWindow on
// the outer mux, like noSessionPatterns: the edge's ^~ /w/ location and
// viewer-authproxy's let them through with no identity (PW.5). The daemon
// read goes out on the window token alone (beadsapi.Client.WindowView, the
// per-call machine principal), and kbeads opens exactly one route for it.
//
// Unknown is 404 (PW-D8): a token that is not gw_ plus 22 base64url
// characters never reaches the daemon, and every daemon refusal (401 for
// unknown, expired or revoked, 403, 422) is the same 404 page. A failure (a
// 5xx, a timeout, a payload that does not decode) is not a refusal: the page
// keeps the window's last good view, stale, and is the 404 only for a window
// that has not opened yet. Every response under /w/ carries the PW-D9
// headers and sets no cookie. Nothing here logs the path or the token, or
// echoes either back; view telemetry keys on the route pattern.
//
// Load (PW-D6): a cache entry per window, keyed by the token's sha256, holds
// the built View for windowViewTTL and is shared by every visitor of that window,
// so daemon reads are at most one per window per 20 s whatever the visitors
// do, and a plan.json poll whose ETag still matches is a 304 with no read.
// The narrative's "per plan" key is not available: the payload carries no
// bead ids (PW-D4), so the viewer cannot tell two windows on one plan apart
// from two plans; the bound is per live window instead. Views are counted
// here and handed to kbeads on the next read (X-Window-Views), which flushes
// them to the window bead at most every five minutes; no bead is written on
// the request path. A page opened by an unfurl bot or crawler is counted in
// gardenio_window_views_total{visitor="crawler"} and not sent as a view.
//
// A land window (Lands: the land window, plan gd-4iVYF9tGsn, step LW.4
// gd-OJOsIH52NE) is the same token on the same route; the daemon answers it
// with a land view instead (kbeads dispatches on the window's target_kind,
// and the view says so by naming its land). Then:
//
//   - GET /w/{token} is the land's page: a canvas with the World drawn by
//     gardenhub's SCENE (LW.3), and, complete with JS off, a plain list of the
//     land's plans. /w/static/land.js (the skin clock, the page-scene.js
//     bundle and templates/static/window-world.js) fetches land.json every
//     20 s and pans, zooms and taps; a tapped plan opens a read-only card.
//   - GET /w/{token}/land.json is the cleaned land view shaped for the scene
//     (planwindow.ToScene), with an ETag, through the same cache.
//
// plan.json on a land window and land.json on a plan window are the uniform
// 404, so neither document is ever served for the other kind.

// windowPublicPatterns are the Window's routes, served without a session.
// Each is routePublic in viewerRouteClasses (TestWindowPatternsArePublic).
// "/w/" catches every other path under /w/ (a trailing slash, a POST) so it
// gets the uniform 404 rather than the auth chain.
var windowPublicPatterns = []string{
	"GET /w/{token}",
	"GET /w/{token}/plan.json",
	"GET /w/{token}/live",
	"GET /w/{token}/land.json",
	"GET /w/static/window.js",
	"GET /w/static/land.js",
	"/w/",
}

// mountWindow serves windowPublicPatterns on outer through public (the route
// mux without the auth chain).
func mountWindow(outer *http.ServeMux, public http.Handler) {
	for _, p := range windowPublicPatterns {
		outer.Handle(p, public)
	}
}

func (s *Server) registerWindowRoutes(mux routeRegistrar) {
	mux.HandleFunc("GET /w/{token}", s.handleWindowPage)
	mux.HandleFunc("GET /w/{token}/plan.json", s.handleWindowPlanJSON)
	mux.HandleFunc("GET /w/{token}/live", s.handleWindowLive)
	mux.HandleFunc("GET /w/{token}/land.json", s.handleWindowLandJSON)
	mux.HandleFunc("GET /w/static/window.js", s.handleWindowJS)
	mux.HandleFunc("GET /w/static/land.js", s.handleWindowLandJS)
	mux.HandleFunc("/w/", s.handleWindowNotFound)
}

const (
	// windowTokenPrefix and windowTokenLen are kbeads' (window_principal.go):
	// gw_ and 16 random bytes in unpadded base64url.
	windowTokenPrefix = "gw_"
	windowTokenBytes  = 16
	// windowViewTTL is how long one daemon read serves a window (PW-D6), and the
	// page's refetch interval.
	windowViewTTL = 20 * time.Second
	// windowReadTimeout is how long a visitor waits on a daemon read. A read
	// still running then goes on, up to windowLateReadTimeout, and fills the
	// cache when it ends, so the next visitor sees what it read (gd-FC7tIACC5f).
	windowReadTimeout     = 10 * time.Second
	windowLateReadTimeout = 60 * time.Second
	// windowMaxEntries bounds the cache. Entries idle for windowIdle go first;
	// past the bound a read is served uncached.
	windowMaxEntries = 4096
	windowIdle       = 10 * time.Minute
)

var windowTokenLen = len(windowTokenPrefix) + base64.RawURLEncoding.EncodedLen(windowTokenBytes)

// wellFormedWindowToken is the local shape check (PW-D8): only a token this
// passes is ever sent to the daemon.
func wellFormedWindowToken(token string) bool {
	if len(token) != windowTokenLen || !strings.HasPrefix(token, windowTokenPrefix) {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(token[len(windowTokenPrefix):])
	return err == nil && len(raw) == windowTokenBytes
}

// windowContentSecurityPolicy has no inline script: the page's one script is
// /w/static/window.js (a land's, /w/static/land.js). Inline style is allowed (the progress bar's width, the
// Lands skin's <style> blocks); fonts may be data: URIs.
const windowContentSecurityPolicy = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self' data:; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// setWindowHeaders sets PW-D9's headers on every /w/ response.
func setWindowHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Robots-Tag", "noindex, nofollow")
	h.Set("Cache-Control", "private, no-store")
	h.Set("Content-Security-Policy", windowContentSecurityPolicy)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Del("Set-Cookie")
}

var (
	windowViewsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gardenio_window_views_total",
		Help: "Window pages opened, by visitor: human, or crawler (an unfurl bot or search crawler, not counted on the window).",
	}, []string{"visitor"})
	windowReadsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gardenio_window_daemon_reads_total",
		Help: "Window reads of GET /v1/windows/self/view, by outcome: ok, refused (the daemon said no; the visitor got the 404) or error; timeout counts a visitor served before the read ended, and that read's own outcome is counted when it ends.",
	}, []string{"outcome"})
)

// windowMetrics are the Window's collectors, for main.go's registry.
func windowMetrics() []prometheus.Collector {
	return []prometheus.Collector{windowViewsTotal, windowReadsTotal, windowLiveStreams, planwindow.WithheldTotal}
}

// windowAPI is the daemon surface the page reads. *beadsapi.Client
// implements it; NewServer picks it up from the daemon client.
type windowAPI interface {
	WindowView(ctx context.Context, token string, views int64) (json.RawMessage, error)
}

// errWindowNotFound is every reason a window does not open. It never says
// which.
var errWindowNotFound = errors.New("window not found")

// windowSnapshot is one daemon read, built: a plan's View, or a land's
// cleaned view when land is set. doc is what plan.json or land.json serves.
type windowSnapshot struct {
	view      planwindow.View
	land      *planwindow.LandView
	doc       []byte
	etag      string
	fetchedAt time.Time
}

// windowEntry is one window's cache slot. Concurrent visitors of one window
// share its one daemon read in flight.
type windowEntry struct {
	mu        sync.Mutex
	snap      *windowSnapshot // nil after a refusal
	fetchedAt time.Time
	// read is the daemon read in flight, nil when there is none.
	read *windowRead
	// streamedAt is when the live stream (window_live.go) last put a view
	// here; within windowViewTTL of it a read with no views to report is
	// served from the cache.
	streamedAt time.Time
	// pending is the human page views served since the last read.
	pending  int64
	lastUsed time.Time
}

// windowRead is one daemon read. done is closed once the read's outcome is
// in its entry and in snap and err.
type windowRead struct {
	done chan struct{}
	snap *windowSnapshot
	err  error
}

// windowCache is the per-window cache of built views (PW-D6).
type windowCache struct {
	api windowAPI
	now func() time.Time
	// readTimeout and lateReadTimeout are windowReadTimeout and
	// windowLateReadTimeout.
	readTimeout, lateReadTimeout time.Duration

	mu      sync.Mutex
	entries map[string]*windowEntry

	// live caps the live streams (window_live.go).
	live windowLiveLimits
}

func newWindowCache(api windowAPI) *windowCache {
	return &windowCache{
		api: api, now: time.Now, entries: map[string]*windowEntry{},
		readTimeout: windowReadTimeout, lateReadTimeout: windowLateReadTimeout,
	}
}

// entry returns key's slot, making it if there is room.
func (c *windowCache) entry(key string, now time.Time) *windowEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.entries[key]; e != nil {
		e.lastUsed = now
		return e
	}
	if len(c.entries) >= windowMaxEntries {
		for k, e := range c.entries {
			if now.Sub(e.lastUsed) > windowIdle && e.mu.TryLock() {
				delete(c.entries, k)
				e.mu.Unlock()
			}
		}
	}
	e := &windowEntry{lastUsed: now}
	if len(c.entries) < windowMaxEntries {
		c.entries[key] = e
	}
	return e
}

// get returns the window's view, reading the daemon when the cached one is
// older than windowViewTTL. pageView counts one human view of the page. A
// read the visitor stops waiting on (readTimeout, or the visitor leaving)
// still ends and fills the entry.
func (c *windowCache) get(ctx context.Context, token string, pageView bool) (*windowSnapshot, error) {
	now := c.now()
	e := c.entry(windowKey(token), now)
	e.mu.Lock()
	if pageView {
		e.pending++
	}
	fresh := !e.fetchedAt.IsZero() && now.Sub(e.fetchedAt) < windowViewTTL
	streamed := e.snap != nil && e.pending == 0 && now.Sub(e.streamedAt) < windowViewTTL
	if fresh || streamed {
		defer e.mu.Unlock()
		if e.snap == nil {
			return nil, errWindowNotFound
		}
		return e.snap, nil
	}
	r := e.read
	if r == nil {
		r = c.startRead(e, token)
	}
	e.mu.Unlock()

	wait := time.NewTimer(c.readTimeout)
	defer wait.Stop()
	select {
	case <-r.done:
		return r.snap, r.err
	case <-ctx.Done():
		// The visitor left; that says nothing about the window.
		return nil, errWindowNotFound
	case <-wait.C:
		windowReadsTotal.WithLabelValues("timeout").Inc()
		e.mu.Lock()
		defer e.mu.Unlock()
		return e.failed(now)
	}
}

// startRead starts e's daemon read, reporting the page views pending now. It
// runs on its own context, bounded by lateReadTimeout, and settles e when it
// ends. e.mu is held.
func (c *windowCache) startRead(e *windowEntry, token string) *windowRead {
	r := &windowRead{done: make(chan struct{})}
	e.read = r
	views := e.pending
	go func() {
		defer close(r.done)
		rctx, cancel := context.WithTimeout(context.Background(), c.lateReadTimeout)
		body, err := c.api.WindowView(rctx, token, views)
		cancel()
		e.mu.Lock()
		defer e.mu.Unlock()
		e.read = nil
		r.snap, r.err = e.settle(body, err, views, c.now())
	}()
	return r
}

// settle keeps a read's outcome in e and answers it: a refusal closes the
// window for windowViewTTL, a failure keeps the last view (failed), and a
// view replaces it and clears the views it reported. e.mu is held.
func (e *windowEntry) settle(body []byte, err error, views int64, now time.Time) (*windowSnapshot, error) {
	if err != nil {
		var apiErr *beadsapi.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode < 500 {
			windowReadsTotal.WithLabelValues("refused").Inc()
			e.snap, e.fetchedAt = nil, now
			return nil, errWindowNotFound
		}
		windowReadsTotal.WithLabelValues("error").Inc()
		return e.failed(now)
	}
	snap, err := buildWindowSnapshot(body, now)
	if err != nil {
		windowReadsTotal.WithLabelValues("error").Inc()
		return e.failed(now)
	}
	windowReadsTotal.WithLabelValues("ok").Inc()
	e.snap, e.fetchedAt = snap, now
	e.pending = max(e.pending-views, 0)
	return snap, nil
}

// put keeps a view the live stream delivered (window_live.go). It is the
// daemon's answer to the same route, so get serves it as it would a read's,
// except that page views still waiting to be reported send the next get to
// the daemon, which counts them.
func (c *windowCache) put(token string, snap *windowSnapshot) {
	now := c.now()
	e := c.entry(windowKey(token), now)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.snap, e.streamedAt = snap, now
}

// windowKey is a token's cache key, its sha256: the cache never holds a token.
func windowKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// failed answers a read that failed without a refusal (a 5xx, a timeout, a
// payload that does not decode): the last good view, stale, for another
// windowViewTTL, so a slow daemon never turns a window that opened into "No
// window here" (gd-rXMNcFrzDp). Only a window that never opened has nothing
// to show, and gets the uniform 404.
func (e *windowEntry) failed(now time.Time) (*windowSnapshot, error) {
	e.fetchedAt = now
	if e.snap == nil {
		return nil, errWindowNotFound
	}
	return e.snap, nil
}

func buildWindowSnapshot(body []byte, now time.Time) (*windowSnapshot, error) {
	snap := &windowSnapshot{fetchedAt: now}
	var doc any
	if windowViewIsLand(body) {
		p, err := planwindow.DecodeLand(bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		lv := planwindow.CleanLand(p)
		snap.land, doc = &lv, planwindow.ToScene(lv)
	} else {
		p, err := planwindow.Decode(bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		snap.view = planwindow.Build(p)
		doc = snap.view
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(b)
	snap.doc, snap.etag = b, `"`+hex.EncodeToString(sum[:16])+`"`
	return snap, nil
}

// windowViewIsLand reports whether a view body is a land window's: kbeads'
// land view names its land, and a plan view has no land key.
func windowViewIsLand(body []byte) bool {
	var probe struct {
		Land json.RawMessage `json:"land"`
	}
	return json.Unmarshal(body, &probe) == nil && len(probe.Land) > 0 && string(probe.Land) != "null"
}

// windowCrawlerMarks are user-agent substrings of unfurl bots and crawlers
// (PW-D9): Slack, Discord, iMessage (which sends facebookexternalhit),
// Microsoft SafeLinks and Teams, and the search engines.
var windowCrawlerMarks = []string{
	"bot", "crawl", "spider", "slurp", "facebookexternalhit", "embedly", "preview",
	"whatsapp", "skypeuripreview", "msoffice", "microsoft office", "teams", "outlook",
	"vkshare", "w3c_validator", "curl", "wget", "python-requests", "go-http-client",
}

// windowCrawler reports whether a user agent is a bot rather than a person.
// An empty one is a bot.
func windowCrawler(ua string) bool {
	ua = strings.ToLower(strings.TrimSpace(ua))
	if ua == "" {
		return true
	}
	for _, m := range windowCrawlerMarks {
		if strings.Contains(ua, m) {
			return true
		}
	}
	return false
}

// windowSnapshotFor is the handlers' common front: the shape check, then the
// cache. pageView is a visitor opening the page, not its refresh.
func (s *Server) windowSnapshotFor(r *http.Request, pageView bool) (*windowSnapshot, bool) {
	token := r.PathValue("token")
	if s.window == nil || !wellFormedWindowToken(token) {
		return nil, false
	}
	human := false
	if pageView {
		human = !windowCrawler(r.Header.Get("User-Agent"))
		if human {
			windowViewsTotal.WithLabelValues("human").Inc()
		} else {
			windowViewsTotal.WithLabelValues("crawler").Inc()
		}
	}
	snap, err := s.window.get(r.Context(), token, human)
	return snap, err == nil
}

// windowRefreshParam marks the page fetch window.js makes after plan.json
// changed: a refresh, not a view.
const windowRefreshParam = "refresh"

func (s *Server) handleWindowPage(w http.ResponseWriter, r *http.Request) {
	setWindowHeaders(w)
	refresh := r.URL.Query().Get(windowRefreshParam) == "1"
	snap, ok := s.windowSnapshotFor(r, r.Method == http.MethodGet && !refresh)
	if !ok {
		s.renderWindowNotFound(w)
		return
	}
	if snap.land != nil {
		s.renderWindow(w, http.StatusOK, "window_land", s.windowLandPage(snap))
		return
	}
	s.renderWindow(w, http.StatusOK, "window_page", s.windowPage(snap))
}

func (s *Server) handleWindowPlanJSON(w http.ResponseWriter, r *http.Request) {
	s.serveWindowDoc(w, r, false)
}

func (s *Server) handleWindowLandJSON(w http.ResponseWriter, r *http.Request) {
	s.serveWindowDoc(w, r, true)
}

// serveWindowDoc answers plan.json (land false) or land.json (land true): the
// snapshot's document with its ETag, or the uniform 404 when the window is
// the other kind.
func (s *Server) serveWindowDoc(w http.ResponseWriter, r *http.Request, land bool) {
	setWindowHeaders(w)
	snap, ok := s.windowSnapshotFor(r, false)
	if !ok || (snap.land != nil) != land {
		s.renderWindowNotFound(w)
		return
	}
	w.Header().Set("ETag", snap.etag)
	if inm := r.Header.Get("If-None-Match"); inm != "" && windowETagMatch(inm, snap.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(snap.doc)
}

// windowETagMatch reports whether an If-None-Match list names etag.
func windowETagMatch(inm, etag string) bool {
	for _, t := range strings.Split(inm, ",") {
		t = strings.TrimPrefix(strings.TrimSpace(t), "W/")
		if t == etag || t == "*" {
			return true
		}
	}
	return false
}

func (s *Server) handleWindowNotFound(w http.ResponseWriter, _ *http.Request) {
	setWindowHeaders(w)
	s.renderWindowNotFound(w)
}

// windowJSOnce is window.js with the skin clock in front of it, built once.
var windowJSOnce = sync.OnceValues(func() ([]byte, error) {
	src, err := fs.ReadFile(staticFS, "templates/static/window.js")
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	b.WriteString(skinClockJS)
	b.WriteString("\nSKIN_CLOCK.start('homestead', 'dark-homestead');\n")
	b.Write(src)
	return b.Bytes(), nil
})

// windowLandJSOnce is a land window's one script: the skin clock, gardenhub's
// page-scene.js bundle (the World's drawing, LW.3) and window-world.js, the
// page's glue, built once.
var windowLandJSOnce = sync.OnceValues(func() ([]byte, error) {
	scene, ok := gardenhub.SceneScript()
	if !ok {
		return nil, errors.New("gardenhub's page-scene.js bundle did not build")
	}
	glue, err := fs.ReadFile(staticFS, "templates/static/window-world.js")
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	b.WriteString(skinClockJS)
	b.WriteString("\nSKIN_CLOCK.start('homestead', 'dark-homestead');\n")
	b.WriteString(scene)
	b.WriteString("\n;\n")
	b.Write(glue)
	return b.Bytes(), nil
})

func (s *Server) handleWindowJS(w http.ResponseWriter, _ *http.Request) {
	s.serveWindowScript(w, "window.js", windowJSOnce)
}

func (s *Server) handleWindowLandJS(w http.ResponseWriter, _ *http.Request) {
	s.serveWindowScript(w, "land.js", windowLandJSOnce)
}

func (s *Server) serveWindowScript(w http.ResponseWriter, name string, build func() ([]byte, error)) {
	setWindowHeaders(w)
	js, err := build()
	if err != nil {
		s.logger.Error("window script missing from the build", "script", name, "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	_, _ = w.Write(js)
}

// windowPageView is what templates/window.html renders. Every string in it
// is the read model's, already through hygiene, or the page's own words.
type windowPageView struct {
	BasePath string
	Theme    string
	Title    string
	// Description is og:description and the meta description: state, phase
	// and progress, text only.
	Description string
	View        planwindow.View
	State       string
	Phases      []windowPhase
	Decisions   []windowDecision
	// Waiting is false when nothing waits on the operator.
	Waiting   bool
	Crew      []planwindow.Agent
	Recent    []windowChange
	Narrative []windowDoc
	// Fetched is when the daemon was read; Updated says it ("just now").
	Fetched time.Time
	Updated string
	// Closes is the window's expiry as a date.
	Closes string
	// Land is set on a land window's page (window_land), which shows it in
	// place of the plan fields.
	Land *windowLand
	// Seed is land.json's document inline, set only on the owner's preview
	// (land_share.go), which has no land.json for land.js to fetch.
	Seed template.JS
}

// windowLand is a land window's page: the land's plans as the JS-off list.
type windowLand struct {
	Plans []windowLandPlan
	// Open counts the land's open plans; More, those the view does not show.
	Open, More int
}

// windowLandPlan is one plan in the list. ID is its scene id (plan-<n>,
// planwindow.ToScene), which window-world.js opens the card by.
type windowLandPlan struct {
	ID         string
	Title      string
	State      string
	StateLabel string
	Closed     bool
	// Phase is the current phase's title.
	Phase       string
	Done, Total int
	// Waiting counts open decisions and escalations; Live, crew at work.
	Waiting, Live int
}

type windowPhase struct {
	Title  string
	Status string
	Done   int
	Total  int
	Steps  []windowStep
}

type windowStep struct {
	Title  string
	Status string
	Label  string
	DoneAt time.Time
	Done   string
}

type windowDecision struct {
	Title string
	State string
	Words string
}

type windowChange struct {
	At    time.Time
	Ago   string
	Words string
	Key   string
}

type windowDoc struct {
	Blocks []template.HTML
}

var windowStepLabels = map[string]string{
	planwindow.StatusTodo:     "Not started",
	planwindow.StatusWorking:  "In progress",
	planwindow.StatusBlocked:  "Blocked",
	planwindow.StatusDeferred: "Deferred",
	planwindow.StatusDone:     "Done",
}

func (s *Server) windowPage(snap *windowSnapshot) windowPageView {
	v, now := snap.view, snap.fetchedAt
	title := v.Title
	if title == "" {
		title = "A plan"
	}
	p := windowPageView{
		BasePath: s.basePath, Theme: s.theme, Title: title, View: v, State: v.State,
		Crew: v.Crew, Fetched: now, Updated: windowAgo(now, now),
	}
	if !v.ExpiresAt.IsZero() {
		p.Closes = v.ExpiresAt.Format("2 January 2006")
	}
	p.Description = windowDescription(v)
	for _, ph := range v.Phases {
		wp := windowPhase{Title: ph.Title, Status: ph.Status, Done: ph.Done, Total: ph.Total}
		for _, st := range ph.Steps {
			step := windowStep{Title: st.Title, Status: st.Status, Label: windowStepLabels[st.Status]}
			if step.Title == "" {
				step.Title = st.ShortTitle
			}
			if st.DoneAt != nil {
				step.DoneAt, step.Done = *st.DoneAt, windowAgo(*st.DoneAt, now)
			}
			wp.Steps = append(wp.Steps, step)
		}
		p.Phases = append(p.Phases, wp)
	}
	for _, d := range v.Waiting.Decisions {
		wd := windowDecision{Title: d.Title, State: d.State}
		switch d.State {
		case planwindow.DecisionOpen:
			wd.Words = "Waiting for an answer"
			p.Waiting = true
		case planwindow.DecisionResolved:
			wd.Words = "Answered"
			if d.Chosen != "" {
				wd.Words = "Answered: " + d.Chosen
			}
		default:
			wd.Words = "Withdrawn"
		}
		p.Decisions = append(p.Decisions, wd)
	}
	if v.Waiting.Escalations.Open > 0 {
		p.Waiting = true
	}
	for _, c := range v.Recent {
		p.Recent = append(p.Recent, windowChange{At: c.At, Ago: windowAgo(c.At, now), Words: windowChangeWords(c), Key: c.At.Format(time.RFC3339) + " " + c.Kind})
	}
	for _, d := range v.Narrative {
		var doc windowDoc
		blocks := d.Blocks
		// A narrative opens with "# <plan title>", which the page's h1
		// already says.
		if len(blocks) > 1 && strings.HasPrefix(blocks[0], "# ") && !strings.Contains(blocks[0], "\n") {
			blocks = blocks[1:]
		}
		for _, b := range blocks {
			doc.Blocks = append(doc.Blocks, windowBlock(b))
		}
		p.Narrative = append(p.Narrative, doc)
	}
	return p
}

func (s *Server) windowLandPage(snap *windowSnapshot) windowPageView {
	v, now := snap.land, snap.fetchedAt
	title := v.Land
	if title == "" {
		title = "A land"
	}
	land := &windowLand{Open: v.OpenPlans}
	waiting, open := 0, 0
	live := map[string]bool{} // handles at work on an open plan
	for i, pl := range v.Plans {
		lp := windowLandPlan{
			ID: fmt.Sprintf("plan-%d", i+1), Title: pl.Title, State: pl.State, StateLabel: planwindow.StateLabel(pl.State),
			Closed: pl.Status == "closed", Done: pl.StepsDone, Total: pl.StepsTotal, Waiting: pl.OpenDecisions + pl.OpenEscalations,
		}
		if lp.Title == "" {
			lp.Title = pl.ShortTitle
		}
		for _, ph := range pl.Phases {
			if ph.Number == pl.CurrentPhase && pl.CurrentPhase > 0 {
				lp.Phase = ph.Title
			}
		}
		for i, a := range pl.Crew {
			if !a.Live {
				continue
			}
			lp.Live++
			if !lp.Closed {
				name := a.Name
				if name == planwindow.WithheldText {
					// A withheld handle may be anyone; count each.
					name = fmt.Sprintf("%s#%d#%d", name, len(land.Plans), i)
				}
				live[name] = true
			}
		}
		if !lp.Closed {
			open++
			waiting += lp.Waiting
		}
		land.Plans = append(land.Plans, lp)
	}
	land.Open = max(land.Open, open)
	land.More = land.Open - open
	p := windowPageView{BasePath: s.basePath, Theme: s.theme, Title: title, Land: land, Fetched: now, Updated: windowAgo(now, now)}
	if !v.ExpiresAt.IsZero() {
		p.Closes = v.ExpiresAt.Format("2 January 2006")
	}
	p.Description = windowLandDescription(land.Open, waiting, len(live))
	return p
}

// windowLandDescription is a land's unfurl text: "4 open plans · 2 waiting on
// the operator · 3 crew at work".
func windowLandDescription(open, waiting, live int) string {
	parts := []string{windowCount(open, "open plan", "open plans")}
	if waiting > 0 {
		parts = append(parts, fmt.Sprintf("%d waiting on the operator", waiting))
	}
	if live > 0 {
		parts = append(parts, fmt.Sprintf("%d crew at work", live))
	}
	return strings.Join(parts, " · ")
}

func windowCount(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// windowDescription is the unfurl text: "Building · Now: The swap table · 3
// of 8 steps done".
func windowDescription(v planwindow.View) string {
	parts := []string{v.StateLabel}
	if v.Phase != "" {
		parts = append(parts, "Now: "+v.Phase)
	}
	if v.Progress.Total > 0 {
		parts = append(parts, fmt.Sprintf("%d of %d steps done", v.Progress.Done, v.Progress.Total))
	}
	return strings.Join(parts, " · ")
}

func windowChangeWords(c planwindow.Change) string {
	switch c.Kind {
	case planwindow.KindStepClosed:
		return "Step done: " + c.Title
	case planwindow.KindDecisionOpened:
		return "Question for the operator: " + c.Title
	case planwindow.KindDecisionResolved:
		return "Answered: " + c.Title
	case planwindow.KindDecisionCancelled:
		return "Question withdrawn: " + c.Title
	case planwindow.KindEscalationOpened:
		return "Waiting on the operator for " + c.Title
	case planwindow.KindEscalationClosed:
		return "The operator saw to " + c.Title
	}
	return c.Title
}

// windowAgo says how long before now t was: "just now", "5 min ago", "3 h
// ago", "4 d ago", then the date. window.js says it the same way.
func windowAgo(t, now time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d h ago", int(d/time.Hour))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%d d ago", int(d/(24*time.Hour)))
	}
	return t.UTC().Format("2 Jan 2006")
}

// windowMarkdown renders one narrative block. Raw HTML is dropped
// (goldmark's default), there is no autolinking, every link and image keeps
// only its text (nothing on the page can be clicked into, and no URL on it
// is not under /w/), and headings sit two levels down, under the page's.
var windowMarkdown = goldmark.New(
	goldmark.WithExtensions(extension.Table, extension.Strikethrough, extension.TaskList),
	goldmark.WithParserOptions(parser.WithASTTransformers(util.Prioritized(windowUnlinker{}, 100))),
)

func windowBlock(md string) template.HTML {
	if md == planwindow.WithheldText {
		return template.HTML(`<p class="w-withheld">` + template.HTMLEscapeString(md) + `</p>`) //nolint:gosec // escaped constant
	}
	var buf bytes.Buffer
	if err := windowMarkdown.Convert([]byte(md), &buf); err != nil {
		return template.HTML("<p>" + template.HTMLEscapeString(md) + "</p>") //nolint:gosec // escaped
	}
	return template.HTML(buf.String()) //nolint:gosec // goldmark output, raw HTML dropped, links unwrapped
}

type windowUnlinker struct{}

func (windowUnlinker) Transform(doc *ast.Document, reader text.Reader, _ parser.Context) {
	src := reader.Source()
	var unwrap []ast.Node
	var autos []*ast.AutoLink
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n := n.(type) {
		case *ast.Link, *ast.Image:
			unwrap = append(unwrap, n)
		case *ast.AutoLink:
			autos = append(autos, n)
		case *ast.Heading:
			n.Level = min(n.Level+2, 6)
		}
		return ast.WalkContinue, nil
	})
	for _, n := range unwrap {
		parent := n.Parent()
		for c := n.FirstChild(); c != nil; {
			next := c.NextSibling()
			parent.InsertBefore(parent, n, c)
			c = next
		}
		parent.RemoveChild(parent, n)
	}
	for _, a := range autos {
		a.Parent().ReplaceChild(a.Parent(), a, ast.NewString(a.Label(src)))
	}
}

func (s *Server) renderWindowNotFound(w http.ResponseWriter) {
	s.renderWindow(w, http.StatusNotFound, "window_missing", windowPageView{BasePath: s.basePath, Theme: s.theme, Title: "No window here"})
}

// renderWindow executes templates/window.html, which wears the Lands skin
// (lands.html's lands_style). Like renderLands it never reads who is signed
// in, and it logs neither the path nor the token.
func (s *Server) renderWindow(w http.ResponseWriter, status int, name string, v windowPageView) {
	tmpl, ok := s.pages["window.html"]
	if !ok {
		s.logger.Error("window template not loaded")
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	bound, err := tmpl.Clone()
	if err != nil {
		s.logger.Error("cloning window template", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := bound.ExecuteTemplate(&buf, name, v); err != nil {
		s.logger.Error("window template render error", "page", name, "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if _, err := w.Write(buf.Bytes()); err != nil && !isClientDisconnect(err) {
		s.logger.Debug("window page write failed", "error", err)
	}
}
