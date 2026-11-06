package gardenhub

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/decisionview"
	"gardenio/controller/internal/servertiming"
	"gardenio/controller/internal/tunes"
)

// heartbeatEvery is the SSE keep-alive cadence. Sent as a named event (not a
// comment) so the page can show "synced Ns ago" and detect a dead stream.
const heartbeatEvery = 15 * time.Second

// Handler serves the /garden page, its SSE stream and a diagnostics endpoint.
type Handler struct {
	Hub *Hub
	// Farm serves the structured Homestead read model (/garden/farm*). Nil
	// answers 503 so a viewer built without it still registers the routes.
	Farm *Farm
	// Beads reads one bead for the in-garden bead view (GET /garden/bead/{id}, bead.go). Nil answers 503; the page
	// then shows what the map already knows about the bead.
	Beads BeadReader
	// Evidence caches a decision's evidence images for the bead view, per caller (decisionview.EvidenceCache,
	// gd-Pi7C83262N). Nil reads them on every view.
	Evidence *decisionview.EvidenceCache
	// Links writes the parent-child edge that adopts a meadow bead into a plan (POST /garden/bead/{id}/adopt, adopt.go).
	// Nil answers 503.
	Links BeadLinker
	// Queues reads the Queues panel's tabs (GET /garden/queue/{kind}, queue.go): the viewer's own inbox, decisions and
	// escalations builders. Nil answers 503.
	Queues QueueReader
	// Search answers the search panel (GET /garden/search, search.go): the viewer's own id probe, semantic and lexical
	// search. Nil answers 503.
	Search SearchReader
	// Bundles reads the bundle panel (GET /garden/bundle/{id}, bundle.go): the viewer's own bundle page's documents, raw.
	// Nil answers 503.
	Bundles BundleReader
	// Plans reads the plan cockpit (GET /garden/plan/{id}, its pour fold and its live stream, plan.go): the viewer's own
	// cockpit builders. Nil answers 503.
	Plans PlanReader
	// Projects reads the project panel (GET /garden/project/{name}, project.go): the viewer's own project hub sections,
	// decisions and escalations, and the land's purse. Nil answers 503.
	Projects ProjectReader
	// Gardeners reads the gardener panel (GET /garden/gardener/{id}, gardener.go): the viewer's own gardener page sections.
	// Nil answers 503.
	Gardeners GardenerReader
	// Glass relays live agent sessions (GET /garden/glass/{agent} and its stream, glass.go) and reads the credential pool
	// (GET /garden/glass/credentials, glass_pool.go). Nil answers 503.
	Glass *Glass
	// Settings reads the settings panel (GET /garden/settings, settings.go): the requester's own user-bead settings. Nil
	// answers 503.
	Settings SettingsReader
	// GrafanaBaseURL is the viewer's Grafana base (beadsViewer.grafanaBaseURL) the chart sets are built against (GET
	// /garden/charts/{set}, charts.go). "" degrades every card to its words.
	GrafanaBaseURL string
	// Credits reads a tenant land's recorded credit series for its charts (GET /garden/charts/costs-land,
	// land_credits.go). Nil says the history is unavailable; the purse figures still answer.
	Credits LandCreditSource
	// InLand reports whether the viewer's land scope admits land (an unrestricted viewer admits every land). It is the
	// tenant charts' cut, checked before any read: a land it refuses is a 404. Nil refuses every land.
	InLand func(r *http.Request, land string) bool
	// Notes reads and writes a land's steward-notes:<land> bead on the viewer's own bearer (GET /garden/notes/{land} and
	// POST /garden/notes/{land}/holds, land_notes.go; gd-q4NgYNGpy8). Nil answers 503. InLand is their cut too.
	Notes NotesStore
	// MayEditHolds reports whether the requester may edit the Holds section of land's steward notes: its owner and
	// members, not guests (owner Q3, conversation.ActionEditHolds). The notes answer carries it as "edit"; the save checks
	// it again, and kbeads checks edit_holds on the viewer's bearer. Nil means no.
	MayEditHolds func(r *http.Request, land string) bool
	// NotesOver is how many bytes notes are past steward.NotesCapBytes with every Answer rolled out
	// (steward.NotesOverBytes): a Holds save past it is a 413, since the steward could not write the notes again.
	// Nil holds the whole notes to notesHoldsMax.
	NotesOver func(notes string) int
	BasePath  string
	// Viewer returns the canonical lowercase operator alias of the requester
	// ("oidc:<email>"), or "" when anonymous. It drives the "mine" view only;
	// visibility is identical to /active.
	Viewer func(*http.Request) string
	// CanApproveEscalation reports whether the requester may approve or reject escalations. It only
	// decides whether the Mailbox offers those buttons; the endpoints enforce it again. Nil means no.
	CanApproveEscalation func(*http.Request) bool
	// CanAdopt reports whether the requester may adopt a meadow bead into a plan. It only decides whether the bead view
	// offers "Adopt into a plan"; the route is operator-gated where it is registered. Nil means no.
	CanAdopt func(*http.Request) bool
	// CanOperateGardeners reports whether the requester may pull a gardener's levers, edit its config, add a successor or
	// create one (the /gardener/{id}/* and /create-gardener POSTs, operator-only where they are registered). It only decides
	// what the gardener and project panels offer; the routes enforce it again. Nil means no.
	CanOperateGardeners func(*http.Request) bool
	// CanTend reports whether the requester may send a gardener to a bead from the bead view (web/tend.js, tend.go). It
	// only decides whether the button is offered; the Commons checks the viewer's land role when the mention is said. Nil
	// means no.
	CanTend func(*http.Request) bool
	// CanEdit reports whether the requester may write to a bead from the bead view (web/beadwrite.js: inline edit,
	// comment, lifecycle, parent and waits-on/holds-up links; Lands WB-2..WB-4). It only decides whether the controls are
	// offered; each write is the viewer's own POST /b/{id}/* route, which enforces its gate again. Nil means no.
	CanEdit func(*http.Request) bool
	// CanSeeCosts reports whether the requester may open the Costs panel (GET /garden/charts/{set}, plan gd-gh5J0vVbJP;
	// admin-only where it is registered). It only decides whether the page offers the openers, the Menu's Costs item and
	// the header purse's link (step 2.3, gd-H9LerJ0v3g); a pasted ?costs= link still opens the panel and says the charts are for
	// operators. Nil means no.
	CanSeeCosts func(*http.Request) bool
	// IsAdmin reports whether the requester is a gardenio admin (plan gd-0SUIdKXWV3 step 3): the page then offers Become
	// beside a tenant in the Commons People lightbox and on a tenant land's owner row (web/become.js), which opens
	// BecomeURL. It is the admin role itself, never "unscoped": an operator or a roleless session is unscoped and is no
	// admin. It only decides whether the button is offered; the Become page sits behind the edge's admin check and
	// Keycloak's impersonation role. Nil means no.
	IsAdmin func(*http.Request) bool
	// BecomeURL is the Become page (auth.clonk.tv/gardenio/become/, gardenio-ziti public-edge.yaml); the button opens it
	// with ?email=<tenant>. Empty offers no button.
	BecomeURL string
	// Impersonator names the admin acting behind the requester's impersonated session (plan gd-0SUIdKXWV3 step 4): the
	// verified token's impersonator claim, never a request header. The page then says data-impersonator on its body and
	// draws the banner, "You are <tenant>, impersonated by <admin>", with Stop (StopURL). Nil or "" draws none.
	Impersonator func(*http.Request) string
	// StopURL is where the banner's Stop goes: the edge's sign-out, then Keycloak's end-session, then back to /garden, which
	// asks the admin to sign in as themselves. Empty draws the banner without Stop.
	StopURL string
	// MaySpawn reports whether the requester may start an agent in project's land from the bead view (quick-spawn, Lands
	// WB-12; owner and members, not guests, Q8c). GET /garden/bead/{id} carries the answer for the bead's project as
	// "spawn"; POST /quick-spawn checks it again. Nil means no.
	MaySpawn func(r *http.Request, project string) bool
	// Agents writes the revive that wakes or restarts a gardener (POST /garden/gardener/{id}/wake, wake.go). Nil answers 503.
	Agents AgentReviver
	// CanWake reports whether the requester may wake or restart a gardener. It decides whether the card and the thread
	// header offer it, and wake.go checks it again; the route is also operator-gated where it is registered. Nil means no.
	CanWake func(*http.Request) bool
	// SeesGardener is the requester's cut of gardeners: the activity events on /garden/farm/events (activity.go) go
	// only for a gardener it keeps. Nil, or a nil func, keeps every gardener (the admin view, the only one today).
	SeesGardener func(*http.Request) func(FarmGardener) bool
	// MayTranscript reports whether the requester may open a transcript on its farm stream (farmEventsView); a
	// read-only verifier may not (beads-viewer verifier.go). Nil means yes.
	MayTranscript func(*http.Request) bool
	// ReadOnly reports whether the requester sees /garden read-only: a project-scoped human, for whom every /garden write
	// route stays admin-only (gd-tUi5IuttJn moves the ones a scoped human may take). It turns every write affordance off,
	// whatever the Can* hooks say (can), and the page carries a read-only marker (gd-GuZ3tbnTFI). Nil means no.
	ReadOnly func(*http.Request) bool
	// Cut is the requester's project set for the farm (farm_cut.go): a scoped cut gets the document (GET /garden/farm)
	// cut to its projects, and on /garden/farm/events that cut's own stream (farm_cut_stream.go). Nil, or an unscoped
	// cut, is the whole farm, byte for byte the admin document and stream.
	Cut func(*http.Request) FarmCut
	// Live serves each stream a page's one connection carries (GET /garden/live/events, livemux.go): the viewer's own
	// route chain under its auth, so a stream is read, cut and refused exactly as the route alone would be. Nil answers
	// 503, and the page opens each stream as its own EventSource (body data-live is off).
	Live http.Handler
	// Welcome is the welcome tour the requester's page is due (welcome.go, step 9.14b): the land it shows them round, and
	// their role there, until they have had it. Nil, or a zero WelcomeTour, runs it for nobody.
	Welcome func(*http.Request) WelcomeTour
	// WelcomeSeen records that the requester has had the welcome tour (POST /garden/welcome, welcome.go). Nil answers 503.
	WelcomeSeen func(*http.Request) error

	live    liveConns
	gzMu    sync.Mutex
	gzPages map[pageKey][]byte // the same snapshot + viewer is a byte-identical page
}

// pageViewer is everything that tells two viewers' pages apart at one snapshot version: the viewer, its capability
// flags, whether it is offered Become (admin), the admin impersonating it (a Become session's banner), for a
// project-scoped viewer its cut's key, and the welcome tour it is due (welcome.go), which ends with no new snapshot when
// the person has had it. The page's ETag is a 32-bit hash of it; the gzip cache is keyed on it whole, so two viewers
// whose hashes collide never share a cached page (a scoped viewer never gets an admin's).
type pageViewer struct {
	me           string
	flags        byte
	admin        bool
	impersonator string // a Become session's admin: the banner names them, so the tenant's own page never matches it
	scoped       bool
	cut          string
	welcome      WelcomeTour
}

// pageKey is a cached gzip page's key: its ETag (snapshot version, viewer hash, assets) and the viewer it was hashed from.
type pageKey struct {
	etag string
	pageViewer
}

// hash is the viewer part of the page's ETag: FNV-32a over the viewer, each set flag's bit, admin, the impersonator and the
// cut's key.
func (v pageViewer) hash() uint32 {
	hs := fnv.New32a()
	hs.Write([]byte(v.me))
	for bit := byte(1); bit != 0; bit <<= 1 {
		if v.flags&bit != 0 {
			hs.Write([]byte{bit})
		}
	}
	if v.admin {
		hs.Write([]byte{3}) // a byte no other flag writes: the flags are written in order, so each set of them hashes apart
	}
	if v.impersonator != "" { // the banner names the admin, so each admin's impersonated page hashes apart
		hs.Write([]byte{5})
		hs.Write([]byte(v.impersonator))
	}
	if v.scoped { // one viewer's project set can change at one hub version
		hs.Write([]byte{0})
		hs.Write([]byte(v.cut))
	}
	if v.welcome.Land != "" { // due until the person has had it, at one hub version
		hs.Write([]byte{1})
		hs.Write([]byte(v.welcome.Land + "\x00" + v.welcome.Role + "\x00" + strconv.FormatBool(v.welcome.Gate)))
	}
	return hs.Sum32()
}

// gzipPageCap bounds the compressed-page cache (one entry per live snapshot version and viewer).
const gzipPageCap = 32

// gzipped returns the gzip of page, cached by key so a burst of loads at one snapshot version compresses once.
func (h *Handler) gzipped(key pageKey, page []byte) []byte {
	h.gzMu.Lock()
	defer h.gzMu.Unlock()
	if gz, ok := h.gzPages[key]; ok {
		return gz
	}
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	_, _ = zw.Write(page)
	_ = zw.Close()
	if h.gzPages == nil || len(h.gzPages) >= gzipPageCap {
		h.gzPages = map[pageKey][]byte{}
	}
	h.gzPages[key] = b.Bytes()
	return b.Bytes()
}

// writeDoc answers a panel's JSON document (the Workbench routes). Everything the route did before it is its read,
// named for the Server-Timing header and the phase histogram ("kbeads", or "glass" and "charts" for the routes that read
// something else); the encode is "encode". The bytes are json.Encoder's, as each route wrote them before.
func writeDoc(w http.ResponseWriter, read string, doc any) {
	writeTagged(w, nil, read, doc)
}

// writeTagged is writeDoc for a document the page fetches again each time its stream says it moved (the bead view, the
// cockpit, the project panel; gd-HprlA40a7z): its ETag hashes the body, so a refetch that finds it unchanged is a
// bodyless 304. Private and revalidated every time: the body is cut to the viewer, and the ETag names that cut's bytes.
// A nil r is writeDoc: no ETag, never stored.
func writeTagged(w http.ResponseWriter, r *http.Request, read string, doc any) {
	servertiming.Lap(w, read)
	var b bytes.Buffer
	_ = json.NewEncoder(&b).Encode(doc)
	servertiming.Lap(w, "encode")
	if r == nil {
		w.Header().Set("Cache-Control", "no-store")
	} else {
		sum := sha256.Sum256(b.Bytes())
		etag := `"d` + hex.EncodeToString(sum[:12]) + `"`
		w.Header().Set("ETag", etag)
		w.Header().Set("Cache-Control", "private, no-cache")
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(b.Bytes())
}

// Registrar is the slice of *http.ServeMux that Register uses, so a caller can
// interpose on route registration.
type Registrar interface {
	HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request))
}

// Register mounts the garden routes on mux, each but the event streams timed (Server-Timing and the route histograms,
// internal/servertiming).
func (h *Handler) Register(mux Registrar) {
	mux = servertiming.Timed(mux)
	mux.HandleFunc("GET /garden", h.page)
	mux.HandleFunc("GET /garden/events", h.events)
	mux.HandleFunc("GET /garden/state", h.state)
	mux.HandleFunc("GET /garden/farm", h.farmPage)
	mux.HandleFunc("GET /garden/farm/events", h.farmEvents)
	mux.HandleFunc("POST /garden/farm/events/view", h.farmEventsView)
	mux.HandleFunc("GET /garden/live/events", h.liveEvents) // one EventSource per page (livemux.go)
	mux.HandleFunc("POST /garden/live/sub", h.liveSubscribe)
	mux.HandleFunc("GET /garden/farm/activity", h.farmTranscriptScope) // the transcripts (transcript.go)
	mux.HandleFunc("GET /garden/farm/activity/{id}", h.farmTranscript)
	mux.HandleFunc("GET /garden/bead/{id}", h.beadView)
	mux.HandleFunc("GET /garden/bead/{id}/events", h.beadEvents)
	mux.HandleFunc("GET /garden/arms.svg", armsSVG) // the bead view's Heraldry section and Change arms (bead_arms.go)
	mux.HandleFunc("GET /garden/arms/charge/{cp}", armsCharge)
	mux.HandleFunc("GET /garden/arms/library", armsLibraryServe)
	mux.HandleFunc("GET /garden/arms/register", h.armsRegister)
	mux.HandleFunc("GET /garden/queue/{kind}", h.queueView)
	mux.HandleFunc("GET /garden/search", h.searchView)
	mux.HandleFunc("GET /garden/names", h.namesView) // the ids a page shows untitled, named (names.go)
	mux.HandleFunc("GET /garden/bundle/{id}", h.bundleView)
	mux.HandleFunc("GET /garden/plan/{id}", h.planView)
	mux.HandleFunc("GET /garden/plan/{id}/events", h.planEvents)
	mux.HandleFunc("GET /garden/plan/{id}/pour", h.planPour)
	mux.HandleFunc("GET /garden/project/{name}", h.projectView)
	mux.HandleFunc("GET /garden/gardener/{id}", h.gardenerPanel)
	mux.HandleFunc("GET /garden/settings", h.settingsPanel)
	mux.HandleFunc("GET /garden/charts/{set}", h.chartsPanel)
	mux.HandleFunc("GET /garden/charts/costs-land", h.landCostsPanel)
	mux.HandleFunc("GET /garden/notes/{land}", h.landNotes) // the land header's steward notes (land_notes.go)
	mux.HandleFunc("POST /garden/notes/{land}/holds", h.saveLandHolds)
	mux.HandleFunc("GET /garden/glass/credentials", h.glassPool) // admin only, before {agent} by its specificity
	mux.HandleFunc("GET /garden/glass/wall", h.glassWall)
	mux.HandleFunc("GET /garden/glass/{agent}", h.glassView)
	mux.HandleFunc("GET /garden/glass/{agent}/stream", h.glassStream)
	mux.HandleFunc("GET /garden/glass/{agent}/prompt", h.glassPrompt)
	mux.HandleFunc("POST /garden/glass/{agent}/send", h.glassSend)
	mux.HandleFunc("POST /garden/glass/{agent}/control", h.glassControl)
	mux.HandleFunc("GET "+assetsPath+"{file}", pageAssets.serve) // the page's scripts and stylesheets, content-hashed (assets.go)
	mux.HandleFunc("POST /garden/welcome", h.welcomeSeen)        // the welcome tour was had (welcome.go)
	mux.HandleFunc("GET /garden/manifest.webmanifest", h.manifest)
	mux.HandleFunc("GET /garden/splash", h.splash) // the splash demo: a WebGL canvas (splash.go, plan gd-EJGA3zDGCk)
	mux.HandleFunc("GET "+splashThreePath, splashFile("text/javascript; charset=utf-8", splashThree))
	mux.HandleFunc("GET "+splashStillPath, splashFile("image/png", splashStill))
	mux.HandleFunc("GET /garden/arms.png", arms)
	for _, n := range iconSizes {
		mux.HandleFunc("GET /garden/icon-"+strconv.Itoa(n)+".png", icon(n))
	}
}

type pageData struct {
	Base   string
	V      uint64
	Me     string
	CanEsc bool
	Adopt  bool
	Tend   bool
	Wake   bool
	Edit   bool
	Costs  bool
	// Admin: the viewer is a gardenio admin, offered Become (Handler.IsAdmin); Become is the page it opens.
	Admin  bool
	Become string
	// Impersonator: the admin behind an impersonated session (Handler.Impersonator), drawn as the banner over every view
	// with You (the tenant) and Stop (Handler.StopURL).
	Impersonator string
	You          string
	Stop         string
	// ReadOnly: the viewer sees the page read-only (Handler.ReadOnly); the header says so and the answer boxes the shared
	// fragments carry are hidden (garden.css body[data-ro]).
	ReadOnly bool
	// Cut: the lists are cut to the viewer's projects (Hub.CutSnapshot), body data-cut; the page paints them once and
	// does not open GET /garden/events, which carries every project's items.
	Cut bool
	// Glass: the viewer may watch an agent's terminal (glass.go, spec 3.4 #5), so the page shows the glass kind and its
	// Watch links.
	Glass bool
	// Live: the page holds one shared connection for its streams (GET /garden/live/events, livemux.go; web/live.js), body
	// data-live. Off, each stream is its own EventSource.
	Live bool
	// Welcome: the welcome tour this page runs (welcome.go), body data-welcome, data-welcome-role and data-welcome-gate;
	// none when Land is "".
	Welcome WelcomeTour
	Lists   map[string]template.HTML
	Counts  map[string]int
	Version string
	// EmptyPrompt is the placeholder every composer on the page shows (voice.go).
	EmptyPrompt string

	// The teensy music player's root (internal/tunes, gd-z8nt2TpS6o), beside the HUD's plaque; its stylesheet and script
	// are page assets.
	TunesMarkup template.HTML
}

// ServePage serves the /garden page at another URL: the Commons (internal/commons) answers a browser's navigation to
// a channel or thread with it, and the page opens its Commons pane there from the address (web/commons.js). A workbench
// node's address (/garden/c/n/{id}, or a steward's, workbenchPath) gets the workbench's own page instead (web/node.html, web/commons2.js).
func (h *Handler) ServePage(w http.ResponseWriter, r *http.Request) { h.page(w, r) }

// workbenchPath reports whether a path is the workbench's: a node, /garden/c/n/{id}, or a land's steward, /garden/c/s and
// /garden/c/s/{land} (steward view S2, gd-LisW87zrnf).
func workbenchPath(p string) bool {
	return strings.Contains(p, "/garden/c/n/") || strings.HasSuffix(p, "/garden/c/s") || strings.Contains(p, "/garden/c/s/")
}

func (h *Handler) page(w http.ResponseWriter, r *http.Request) {
	me := ""
	if h.Viewer != nil {
		me = h.Viewer(r)
	}
	readOnly := h.readOnly(r)
	canEsc := h.can(r, h.CanApproveEscalation)
	canAdopt := h.can(r, h.CanAdopt)
	canTend := h.can(r, h.CanTend)
	canWake := h.can(r, h.CanWake)
	canEdit := h.can(r, h.CanEdit)
	canCosts := h.CanSeeCosts != nil && h.CanSeeCosts(r)
	isAdmin := h.BecomeURL != "" && h.IsAdmin != nil && h.IsAdmin(r)
	impersonator := ""
	if h.Impersonator != nil {
		impersonator = h.Impersonator(r)
	}
	canWatch := h.mayWatch(r)
	// A project-scoped viewer's lists are the hub's cut to its projects (Hub.CutSnapshot); the page is told so (Cut),
	// and does not open the hub's stream, which has no per-viewer cut and stays admin-only.
	cut := h.cutOf(r)
	snap := h.Hub.CutSnapshot(cut)
	servertiming.Lap(w, "snapshot")
	if !snap.BuiltAt.IsZero() {
		servertiming.Note(w, "snapshot-age", time.Since(snap.BuiltAt))
	}

	// Same snapshot version + same viewer => byte-identical page, so a revisit
	// is a bodyless 304.
	pv := pageViewer{me: me, admin: isAdmin, impersonator: impersonator, scoped: cut.Scoped, welcome: h.welcome(r)}
	for bit, on := range []bool{canEsc, canAdopt, canWake, canTend, canEdit, canWatch, canCosts, readOnly} {
		if on {
			pv.flags |= 1 << bit
		}
	}
	if cut.Scoped {
		pv.cut, _ = cut.key()
	}
	tmpl, kind := pageTmpl, "g"
	if workbenchPath(r.URL.Path) {
		tmpl, kind = nodeTmpl, "n"
	}
	etag := fmt.Sprintf(`W/"%s%d-%x-%s"`, kind, snap.V, pv.hash(), assetsTag)
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, no-cache")
	w.Header().Set("Vary", "Cookie, Accept-Encoding")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	d := pageData{
		Base: h.BasePath, V: snap.V, Me: me, CanEsc: canEsc, Adopt: canAdopt, Tend: canTend, Wake: canWake, Edit: canEdit, Costs: canCosts, Admin: isAdmin, Become: h.BecomeURL, Impersonator: impersonator, You: strings.TrimPrefix(me, "oidc:"), Stop: h.StopURL, ReadOnly: readOnly, Cut: cut.Scoped, Glass: canWatch, Live: h.Live != nil, Welcome: pv.welcome, Lists: map[string]template.HTML{}, Counts: snap.Counts, EmptyPrompt: EmptyPrompt,
		TunesMarkup: template.HTML(tunes.Markup(false)), //nolint:gosec // embedded at build time, not user input
	}
	for _, sec := range Sections {
		d.Lists[sec] = template.HTML(strings.Join(snap.Frags[sec], "")) //nolint:gosec // fragments are produced by html/template in renderItem
	}

	var buf strings.Builder
	if err := tmpl.Execute(&buf, d); err != nil {
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	servertiming.Lap(w, "template")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The shell is the lists and the markup (the scripts are assets); text compresses about 5x, which is the difference on a phone.
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		gz := h.gzipped(pageKey{etag, pv}, []byte(buf.String()))
		servertiming.Lap(w, "gzip")
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(gz)
		return
	}
	_, _ = w.Write([]byte(buf.String()))
}

func (h *Handler) state(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(h.Hub.Stats())
}

func (h *Handler) events(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	// Last-Event-ID (sent by EventSource on its own reconnects) beats ?since=
	// (which is only the version the page was rendered at).
	raw := r.Header.Get("Last-Event-ID")
	if raw == "" {
		raw = r.URL.Query().Get("since")
	}
	since, _ := strconv.ParseUint(raw, 10, 64)

	hd := w.Header()
	hd.Set("Content-Type", "text/event-stream")
	hd.Set("Cache-Control", "no-cache")
	hd.Set("Connection", "keep-alive")
	hd.Set("X-Accel-Buffering", "no")

	catchUp, live, cancel := h.Hub.Subscribe(since)
	defer cancel()

	fmt.Fprint(w, "retry: 2000\n: connected\n\n")
	for _, m := range catchUp {
		writeMsg(w, m)
	}
	fl.Flush()

	hb := time.NewTicker(heartbeatEvery)
	defer hb.Stop()
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case m, ok := <-live:
			if !ok {
				return
			}
			if m.Event == "reset" && m.Data == nil {
				m = h.Hub.Reset() // collapsed laggard: answer with fresh full state
			}
			writeMsg(w, m)
			fl.Flush()
		case <-hb.C:
			fmt.Fprint(w, "event: hb\ndata: {}\n\n")
			fl.Flush()
		}
	}
}

func writeMsg(w http.ResponseWriter, m Msg) {
	fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", m.V, m.Event, m.Data)
}
