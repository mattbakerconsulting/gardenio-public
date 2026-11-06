package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/yuin/goldmark"
	highlighting "github.com/yuin/goldmark-highlighting/v2"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/renderer/html"

	"gardenio/controller/cmd/beads-viewer/viewer"
	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
	"gardenio/controller/internal/commons"
	"gardenio/controller/internal/coopmuxapi"
	"gardenio/controller/internal/decisionview"
	"gardenio/controller/internal/freehtmx"
	"gardenio/controller/internal/gardenhub"
	"gardenio/controller/internal/oidcauth"
	"gardenio/controller/internal/tunes"
)

// screenClient is the subset of coopmuxapi.Client that the server needs; an
// interface so tests can fake coopmux. Write verbs: SendInput (PTY keystrokes),
// SendKeys (named keys), Nudge (re-prompt a parked agent), Respond (answer
// ask_user prompts).
type screenClient interface {
	Disabled() bool
	GetScreen(ctx context.Context, sessionID string) (*coopmuxapi.Screen, error)
	// GetAgentState fetches the full untruncated last assistant reply. Unused by
	// the viewer since its minter moved to the controller (ka-0000006891); kept
	// for parity with the concrete client.
	GetAgentState(ctx context.Context, sessionID string) (*coopmuxapi.AgentState, error)
	SendInput(ctx context.Context, sessionID, text string) error
	SendKeys(ctx context.Context, sessionID string, keys []string) error
	Nudge(ctx context.Context, sessionID, message string) error
	Respond(ctx context.Context, sessionID string, req coopmuxapi.RespondRequest) error
}

// maxCommentLen caps comment text length. Matches the design doc §5.2.
const maxCommentLen = 10_000

// projectListLimit caps the number of open beads rendered on /p/<project>.
const projectListLimit = 100

// commentTimeFormat is the wire format for `?since=` and the
// `data-comments-latest` data attribute. RFC3339Nano keeps comments distinct
// even when posted within the same second.
const commentTimeFormat = time.RFC3339Nano

// commentView is the template view model for a comment with linkified HTML text.
type commentView struct {
	ID               int64 // for #comment-<id> anchor + permalink
	Author           string
	TextHTML         template.HTML
	CreatedAt        time.Time
	CreatedAtRFC     string // machine-readable timestamp for data-created-at
	IsNew            bool   // CreatedAt > since (animate on swap)
	OperatorChipHTML template.HTML
}

func renderCommentMarkdown(md goldmark.Markdown, text, basePath string) template.HTML {
	return renderCommentMarkdownWithOpts(md, text, basePath, LinkifyOpts{})
}

func renderCommentMarkdownWithOpts(md goldmark.Markdown, text, basePath string, opts LinkifyOpts) template.HTML {
	// md is configured with Unsafe:false (goldmark default), so raw HTML in
	// text is emitted as escaped text — no XSS path through comment bodies.
	var buf bytes.Buffer
	if err := md.Convert([]byte(text), &buf); err != nil {
		// Fallback: escaped + linkified plain text preserves safety if the
		// markdown parser ever errors on pathological input.
		return LinkifyTextWithOpts(text, basePath, opts)
	}
	return LinkifyHTMLWithOpts(buf.String(), basePath, opts)
}

// linkifyComments renders comments and flags any with CreatedAt > since as
// new (so the partial can animate them in on htmx swap). When since is the
// zero time (initial page render or no client cursor), no comments are flagged.
func linkifyComments(md goldmark.Markdown, comments []beadsapi.Comment, basePath string, since time.Time, viewerEmail string) []commentView {
	return linkifyCommentsWithOpts(md, comments, basePath, since, viewerEmail, LinkifyOpts{})
}

// linkifyCommentsWithOpts is linkifyComments with caller-supplied linkifier
// options (e.g. SkipMRRefs for multi-repo projects).
func linkifyCommentsWithOpts(md goldmark.Markdown, comments []beadsapi.Comment, basePath string, since time.Time, viewerEmail string, opts LinkifyOpts) []commentView {
	if len(comments) == 0 {
		return nil
	}
	out := make([]commentView, len(comments))
	for i, c := range comments {
		// The comment's Author serves as the createdBy hint — for agents
		// it carries the `sched-…` discriminator that mid-thread scheduler
		// detection needs (R8 §6.1).
		chip := resolveCommentOperatorChip(c.OperatedBy, c.OperatedByDisplay, c.OperatedByEmail, c.OperatedBySlug, c.Author, c.Author, basePath, viewerEmail)
		out[i] = commentView{
			ID:               c.ID,
			Author:           c.Author,
			TextHTML:         renderCommentMarkdownWithOpts(md, c.Text, basePath, opts),
			CreatedAt:        c.CreatedAt,
			CreatedAtRFC:     c.CreatedAt.UTC().Format(commentTimeFormat),
			IsNew:            !since.IsZero() && c.CreatedAt.After(since),
			OperatorChipHTML: renderOperatorChip(chip),
		}
	}
	return out
}

// linkifyOptsForBead resolves linkifier options from the bead's OWN project,
// so a bare `!N` MR ref links to that project's repo (ka-0000005612).
// Multi-repo projects get SkipMRRefs, since `!N` is ambiguous (ka-0000004231).
// No project label, unknown project, no repo, or a load error yields the zero
// LinkifyOpts (process-wide default).
func (s *Server) linkifyOptsForBead(ctx context.Context, bead *beadsapi.BeadDetail) LinkifyOpts {
	if bead == nil {
		return LinkifyOpts{}
	}
	project := projectFromLabels(bead.Labels)
	if project == "" {
		return LinkifyOpts{}
	}
	// Cached: the partial handlers (description/title/comments) each resolve
	// opts too, so an uncached fetch would add a daemon RTT per htmx swap.
	projects, err := s.listProjectBeadsCached(ctx)
	if err != nil {
		s.logger.Debug("linkify opts: listing projects", "bead", bead.ID, "error", err)
		return LinkifyOpts{}
	}
	info, ok := projects[project]
	if !ok {
		return LinkifyOpts{}
	}
	if len(info.Repos) > 1 {
		return LinkifyOpts{SkipMRRefs: true}
	}
	return LinkifyOpts{ProjectURL: mrRefProjectURL(info)}
}

// mrRefProjectURL resolves the GitLab project web URL that bare `!N` MR refs
// on a single-repo project's beads should point at. Callers must have already
// excluded the ambiguous multi-repo case. Precedence: the sole repos[] entry,
// then git_url (older projects that predate repos[]). Returns "" when neither
// is set. The result is normalized via normalizeRepoWebURL so callers can
// append `/-/merge_requests/N`.
func mrRefProjectURL(info beadsapi.ProjectInfo) string {
	if len(info.Repos) == 1 {
		if u := normalizeRepoWebURL(info.Repos[0].URL); u != "" {
			return u
		}
	}
	return normalizeRepoWebURL(info.GitURL)
}

// normalizeRepoWebURL turns a git remote URL into the browsable project URL:
// whitespace-trimmed, `.git` suffix removed, trailing slash removed. Returns
// "" for an empty input so callers can treat it as "unresolved".
func normalizeRepoWebURL(raw string) string {
	u := strings.TrimSpace(raw)
	if u == "" {
		return ""
	}
	u = strings.TrimSuffix(strings.TrimRight(u, "/"), ".git")
	return strings.TrimRight(u, "/")
}

// humanizeTime renders t as a coarse relative phrase ("3 minutes ago"), "just
// now" under a minute or in the future (clock skew), and "" for the zero time.
func humanizeTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	if d < time.Minute {
		return "just now"
	}
	plural := func(n int, unit string) string {
		if n == 1 {
			return "1 " + unit + " ago"
		}
		return strconv.Itoa(n) + " " + unit + "s ago"
	}
	switch {
	case d < time.Hour:
		return plural(int(d/time.Minute), "minute")
	case d < 24*time.Hour:
		return plural(int(d/time.Hour), "hour")
	case d < 7*24*time.Hour:
		return plural(int(d/(24*time.Hour)), "day")
	case d < 30*24*time.Hour:
		return plural(int(d/(7*24*time.Hour)), "week")
	case d < 365*24*time.Hour:
		return plural(int(d/(30*24*time.Hour)), "month")
	default:
		return plural(int(d/(365*24*time.Hour)), "year")
	}
}

// latestCommentTime returns the most recent CreatedAt across comments, or
// the zero time when comments is empty.
func latestCommentTime(comments []beadsapi.Comment) time.Time {
	var latest time.Time
	for _, c := range comments {
		if c.CreatedAt.After(latest) {
			latest = c.CreatedAt
		}
	}
	return latest
}

// beadAPI is the subset of beadsapi.Client methods used by the server.
// Kept as an interface so tests can inject fakes without spinning up a daemon.
type beadAPI interface {
	GetBead(ctx context.Context, id string) (*beadsapi.BeadDetail, error)
	BatchGetBeads(ctx context.Context, ids []string) (map[string]*beadsapi.BeadDetail, error)
	FindAgentBead(ctx context.Context, agentName string) (*beadsapi.BeadDetail, error)
	FindMostRecentAgentBead(ctx context.Context, agentName string) (*beadsapi.BeadDetail, error)
	FindAgentBeadsByNames(ctx context.Context, agentNames []string, statuses []string) (map[string]*beadsapi.BeadDetail, error)
	GetDependencies(ctx context.Context, id string) ([]beadsapi.Dependency, error)
	GetReverseDependencies(ctx context.Context, id string) ([]beadsapi.Dependency, error)
	BatchGetDependencies(ctx context.Context, ids []string, direction string) (map[string][]beadsapi.Dependency, error)
	RollupMoleculeUsage(ctx context.Context, molID string) (*beadsapi.MoleculeUsage, error)
	GetMentionedBy(ctx context.Context, id string, limit int) ([]beadsapi.BeadReferenceSummary, error)
	GetMentions(ctx context.Context, id string, limit int) ([]beadsapi.BeadReferenceSummary, error)
	GetComments(ctx context.Context, id string) ([]beadsapi.Comment, error)
	GetEvents(ctx context.Context, id string) ([]beadsapi.Event, error)
	GetRecentEvents(ctx context.Context, id string, limit int) ([]beadsapi.Event, error)
	GetOperatorHistory(ctx context.Context, id string) ([]beadsapi.OperatorHistoryRow, error)
	AddComment(ctx context.Context, beadID, author, text string) error
	SemanticSearch(ctx context.Context, req beadsapi.SemanticSearchRequest) (*beadsapi.SemanticSearchResponse, error)
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
	MRBeadByURL(ctx context.Context, mrURL string) (*beadsapi.BeadDetail, error)
	MRBeadsForAgent(ctx context.Context, agentBeadID string) ([]*beadsapi.BeadDetail, error)
	ListEscalations(ctx context.Context, status string, limit int) ([]beadsapi.EscalationDetail, error)
	GetEscalationPermissionDiff(ctx context.Context, escalationID string) (*beadsapi.SpawnPermissionDiff, bool, error)
	// ApproveEscalation / RejectEscalation are the bead-page approval verbs
	// (ka-0000006884). Approve uses the daemon's native endpoint, which records
	// the trail AND replays the denied operation (e.g. the MR merge).
	ApproveEscalation(ctx context.Context, escalationID string, req beadsapi.ApproveEscalationRequest) error
	RejectEscalation(ctx context.Context, escalationID string, req beadsapi.RejectEscalationRequest) error
	// CanApproveEscalation is the daemon's dry run of its approve checks for the
	// caller (POST /v1/escalations/{id}/can-approve). It alone decides whether
	// Approve/Reject render (escalationApproveGate).
	CanApproveEscalation(ctx context.Context, escalationID string) (beadsapi.CanApproveResult, error)
	// RouteEscalation re-routes an escalation (gd-bmOiteQi36); the daemon
	// enforces who may and records the comment.
	RouteEscalation(ctx context.Context, escalationID string, req beadsapi.RouteEscalationRequest) (string, error)
	ListFollowedBeadIDs(ctx context.Context, operatorID int64, limit int) ([]string, error)
	ListBeadFollowers(ctx context.Context, beadID string) ([]*beadsapi.BeadFollower, error)
	AddBeadFollower(ctx context.Context, beadID string, operatorID int64, note string) error
	RemoveBeadFollower(ctx context.Context, beadID string, operatorID int64) error
	LookupOperatorByAlias(ctx context.Context, kind, subject string) (*beadsapi.LookupOperatorByAliasResponse, error)
	LookupOperatorBySlug(ctx context.Context, slug string) (*beadsapi.LookupOperatorBySlugResponse, error)
	GetOperatorSlug(ctx context.Context, operatorID int64) (*beadsapi.GetOperatorSlugResponse, error)
	ListOperators(ctx context.Context, limit int) (*beadsapi.ListOperatorsResponse, error)
	ListSlackOperatorAliases(ctx context.Context) (*beadsapi.ListSlackOperatorAliasesResponse, error)
	OperatorStats(ctx context.Context, recentPerOperator, recentLimit, recentWindowHours int) (*beadsapi.OperatorStatsResponse, error)
	FindSimilar(ctx context.Context, id string, limit int) (*beadsapi.FindSimilarResponse, error)
	ListProjectBeads(ctx context.Context) (map[string]beadsapi.ProjectInfo, error)
	ListRoleBeads(ctx context.Context) (map[string]beadsapi.RoleInfo, error)
	ListAccountBeadsByProject(ctx context.Context, project string) ([]beadsapi.AccountInfo, error)
	ResolveDecision(ctx context.Context, decisionID string, req beadsapi.ResolveDecisionRequest) error
	CancelDecision(ctx context.Context, decisionID, reason, canceledBy string) error
	UpdateBead(ctx context.Context, beadID string, req beadsapi.UpdateBeadRequest) error
	UpdateBeadFieldsTyped(ctx context.Context, beadID string, fields map[string]any) error
	// AppendBeadArrayField / ReorderBeadArrayField are the atomic plan-ordering
	// primitives (ka-0000000988): append-if-absent and compare-and-swap reorder
	// on phase_order/step_order, so concurrent browser and CLI edits cannot
	// clobber each other. Plan write verbs (plan_edit.go, plan_phases.go) use
	// these, not GetBead → UpdateBeadFieldsTyped.
	AppendBeadArrayField(ctx context.Context, beadID, field, element string) error
	ReorderBeadArrayField(ctx context.Context, beadID, field string, expected, next []string) error
	CloseBeadAs(ctx context.Context, beadID, actor string, fields map[string]string) error
	GetStats(ctx context.Context) (*beadsapi.GraphStats, error)
	ListConfigs(ctx context.Context, namespace string) ([]beadsapi.ConfigEntry, error)
	CreateBead(ctx context.Context, req beadsapi.CreateBeadRequest) (string, error)
	CreateBeadDetail(ctx context.Context, req beadsapi.CreateBeadRequest) (*beadsapi.BeadDetail, error)
	PourFormula(ctx context.Context, formulaID string, req beadsapi.PourFormulaRequest) (*beadsapi.PourFormulaResponse, error)
	DeleteBead(ctx context.Context, beadID string) error
	AddDependency(ctx context.Context, beadID, dependsOnID, depType, createdBy string) error
	RemoveDependency(ctx context.Context, beadID, dependsOnID, depType string) error
	AddLabel(ctx context.Context, beadID, label string) error
	RemoveLabel(ctx context.Context, beadID, label string) error
	EventStream(ctx context.Context, topics string) (<-chan beadsapi.SSEEvent, error)
	ListGlobalEvents(ctx context.Context, req beadsapi.ListGlobalEventsRequest) (*beadsapi.ListGlobalEventsResponse, error)
	RecordUITelemetry(ctx context.Context, row *beadsapi.UITelemetryRow) error
	GetAcceptanceMetrics(ctx context.Context) (*beadsapi.AcceptanceMetrics, error)
	// GetBeadTaints / ListTaints back the bead-page taint badges (ADR
	// docs/adr/taints.md §9.1). ListTaints supplies the registry because
	// GetBeadTaints carries no owner_team (needed for the accent color).
	GetBeadTaints(ctx context.Context, id string) ([]beadsapi.BeadTaint, error)
	ListTaints(ctx context.Context) ([]beadsapi.TaintRegistryEntry, error)
	// ListSlowToolCalls backs /tool-latency: the P2 slow-call ledger, slowest-first.
	ListSlowToolCalls(ctx context.Context, filter beadsapi.SlowToolCallFilter) ([]*beadsapi.SlowToolCallRow, error)
	// CreateTaint backs the admin-only "New taint" action (docs/adr/taints.md
	// §9.1, gd-QXSHroFXwB). The daemon requires direct OIDC RoleAdmin, so it
	// always uses the caller's per-request user bearer (set by
	// operatorMiddleware), never the static token + X-Operated-By relay.
	CreateTaint(ctx context.Context, req beadsapi.CreateTaintRequest) (*beadsapi.CreatedTaint, error)
	// FindTeamBySlug / GetTeamTaintSummary back /teams/{team} (ADR
	// docs/adr/taints.md §9.1 "Team page"): the type:team bead by `team` slug,
	// then its granted taints and open scrub requests. Owned taints come from
	// ListTaints filtered by OwnerTeam.
	FindTeamBySlug(ctx context.Context, slug string) (beadsapi.TeamInfo, error)
	GetTeamTaintSummary(ctx context.Context, teamID string) (*beadsapi.TeamTaintSummary, error)
	// ListReleases / GetRelease back the Release pages' frame, the bead
	// view's "Shipped in" line, the front page's Latest release tiles and the
	// /health build tile's link (release_page.go, plan gd-UZKZ9H25UD step 8).
	ListReleases(ctx context.Context, project, status string, limit int) ([]beadsapi.Release, error)
	GetRelease(ctx context.Context, ref, project string) (*beadsapi.Release, error)
	// CreateScrubRequest files a scrub request (docs/adr/taints.md §6.2/§7)
	// for an explicit bead-id set (/me/taints and bead-page "Request scrub",
	// gd-nOnc1Q8Zcv), via the static-token + X-Operated-By relay; the daemon's
	// checks are authoritative.
	CreateScrubRequest(ctx context.Context, in beadsapi.CreateScrubRequestInput) (*beadsapi.CreateScrubRequestResult, error)
	// The remaining taint methods back /taints/{name} and the bead-page "Apply
	// taint" action (gd-nOnc1Q8Zcv), via the relay. Exception:
	// ApproveScrubRequest / RejectScrubRequest always use the caller's user
	// bearer, because the daemon's scrubApprover check requires OIDC (four-eyes
	// integrity, as for CreateTaint).
	GetTaint(ctx context.Context, name string) (*beadsapi.TaintDetail, error)
	UpdateTaintPolicy(ctx context.Context, name string, scrubPolicy json.RawMessage) (*beadsapi.TaintDetail, error)
	ListTaintGrants(ctx context.Context, name string) ([]beadsapi.TaintGrantEntry, error)
	GrantTaint(ctx context.Context, name string, req beadsapi.GrantTaintRequest) (*beadsapi.TaintGrantEntry, error)
	RevokeTaint(ctx context.Context, name, principal string) (bool, error)
	ApplyTaint(ctx context.Context, beadID, taintName string) (*beadsapi.BeadTaint, error)
	ListTaintBeads(ctx context.Context, name string, limit, offset int) ([]beadsapi.TaintBeadEntry, int, error)
	ListTaintScrubRequests(ctx context.Context, name string, limit, offset int) ([]beadsapi.TaintScrubRequestEntry, int, error)
	ListTaintAudit(ctx context.Context, name string, filter beadsapi.TaintAuditFilter) ([]beadsapi.TaintAuditEntry, int, error)
	ApproveScrubRequest(ctx context.Context, beadID, approvalReason string) (*beadsapi.BeadDetail, error)
	RejectScrubRequest(ctx context.Context, beadID, reason string) (*beadsapi.BeadDetail, error)
}

// similarBeadView is the template view model for one similar-bead row.
type similarBeadView struct {
	ID    string
	Title template.HTML
	Score string
}

// Server handles HTTP requests for the beads viewer.
type Server struct {
	daemon beadAPI
	logger *slog.Logger

	// evidence caches each decision's evidence images briefly, per caller
	// (decision_context.go decisionEvidenceOf, gd-Pi7C83262N). Nil reads on
	// every render.
	evidence *decisionview.EvidenceCache

	// machinePrincipal is the viewer's Keycloak service-account credential
	// (machine:<clientId>), attached via machineCtx ONLY to writes no human
	// triggers (plan-label and plan-step reactors); human writes carry the
	// user's bearer. nil = reactors use the client's default credential
	// (gd-ZsfxvXRiyv, gd-Hc34EENbO9).
	machinePrincipal beadsapi.TokenSource

	// landsTerms reads and records the caller's own terms acceptance for the
	// Lands front of house (lands.go), always on the caller's bearer. NewServer
	// takes it from the daemon client when that client implements it.
	landsTerms landsTermsAPI
	// landsGitHub is the Lands GitHub App's setup leg (lands_github.go); nil
	// when the App is not configured, and then the setup routes answer 503.
	landsGitHub *landsGitHub
	// landsGate is the plot application flow and its approval reactor
	// (lands_apply.go); nil until main.go wires it, and then /lands/apply
	// answers 503.
	landsGate *landsGate
	// landsDemo is the demo land's door (lands_demo.go); nil unless
	// LANDS_DEMO_LAND is set, and then /lands/demo says there is no demo.
	landsDemo *landsDemo
	// landsLivePlan is the "See a live plan" link on the Lands home
	// (landsLivePlanURL); empty unless LANDS_LIVE_PLAN_URL is set, and then
	// the home page has no link.
	landsLivePlan string
	// landsLiveFarm is the "See the whole farm" link beside it, a land
	// window (landsLiveFarmURL); empty unless LANDS_LIVE_FARM_URL is set.
	landsLiveFarm string
	// landsGatehouse is the gatehouse land (lands_gatehouse.go); "" unless
	// LANDS_GATEHOUSE_LAND is set.
	landsGatehouse string
	// landsGatehouseTools answers the gatehouse steward's apply and demo
	// requests (lands_gatehouse_reactor.go); nil without a gatehouse or a
	// machine principal. GET /lands/apply prefills from its drafts.
	landsGatehouseTools *gatehouseReactor
	// window is the Window's per-window cache of plan views (window.go); nil
	// when the daemon client cannot read a window, and then /w/ is all 404.
	window *windowCache

	pages    map[string]*template.Template
	partials map[string]*template.Template
	basePath string
	// theme is the viewer look: "meadow" (default, Dawn Meadow, S1j
	// gd-cxU8eyiGyR) or "plain" (kill switch BEADS_VIEWER_THEME=plain). See
	// theme.go.
	theme    string
	md       goldmark.Markdown
	mdInline goldmark.Markdown
	// widgetTemplates is the parsed widget partial set; per-widget renderers
	// execute their widget_<slug> sub-templates from it.
	widgetTemplates *template.Template

	// codeHighlighter renders chroma-highlighted snippets for widgets that
	// show code outside a markdown fence (#4, #13). Cached on (language,
	// code-hash).
	codeHighlighter *codeHighlighter
	static          http.Handler

	// beadsHTTPAddr/beadsDaemonToken are passed to the gb subprocess that
	// renders the prime view (prime.go).
	beadsHTTPAddr    string
	beadsDaemonToken string

	// coopmux is the read-only client for the agent-detail PTY snapshot. Nil or
	// disabled → the fallback <pre> is empty; the live xterm WebSocket works.
	coopmux screenClient

	// coopmuxPublicURL is the public coopmux dashboard base URL; the agent
	// bead page links "Open terminal" to <coopmuxPublicURL>#<pod_name>.
	// Empty → link hidden.
	coopmuxPublicURL string

	// slackWorkspaceDomain is the workspace subdomain (e.g., "acme") for
	// Slack thread links on agent beads. Empty → link hidden.
	slackWorkspaceDomain string

	// grafanaBaseURL is the public Grafana base URL (under /grafana) for the
	// /charts "Open in Grafana ↗" link, from GRAFANA_BASE_URL. Empty → link
	// hidden (ka-0000006706, epic ka-0000007895).
	grafanaBaseURL string

	// escalationReviewURL is the public base URL of the bridge's escalation
	// review SPA; /escalations rows deep-link to it with ?bead_id=<id>.
	// Empty → link hidden.
	escalationReviewURL string

	// tunables exposes runtime-tunable config beads (component:beads-viewer)
	// with bead > env > compiled precedence (e.g. migrationBannerFlagKey, P7).
	// Nil-safe: reads return the compiled default.
	tunables *beadsapi.ConfigWatcher

	// actions is the Incident Intelligence action registry
	// (incident_intel_actions.go). Nil → handleAction returns 503.
	actions *ActionRegistry

	// mezmo is the live-data client for widget-instance beads with a
	// `mezmo_source` field (ka-0000002519). Nil → no-op; widgets use baked
	// fields. mezmoCache is process-wide so render storms don't hammer Mezmo.
	mezmo      MezmoClient
	mezmoCache *mezmoCache

	// resolver maps OIDC subject → operators(id) for the inbox (G2 /
	// ka-0000003651). Nil → /inbox renders the Mode A blocking banner and
	// skips the §1.2.1 query.
	resolver *viewer.Resolver

	// thinking fans out coop.events.messages.*.thinking from NATS to the
	// telepathy SSE page (epic ka-0000007154, ka-0000007528, ka-0000002159).
	// Nil → empty live feed. Set via SetThinkingHub.
	thinking *ThinkingHub

	// feedSeedCache memoizes the /feed first-paint seed for a short TTL. The
	// seed is viewer-independent (filtered client-side), so one build serves a
	// render burst; live SSE hides the staleness (ka-0000002634). Always
	// non-nil after NewServer.
	feedSeedCache *feedSeedCache

	// beadFeed fans one upstream EventStream("beads.bead.>") out to all
	// /feed/events SSE connections, with a bounded warm-paint ring
	// (ka-0000000643). Nil → each connection opens its own live-only stream.
	// Set via SetBeadFeedHub.
	beadFeed *BeadFeedHub

	// garden serves the /garden live page (gd-BKx5ddIilZ). Nil → routes not
	// registered.
	garden *gardenhub.Handler
	// commons serves the Commons read routes under /garden/c (Lands Commons I
	// step 8). Nil → routes not registered.
	commons *commons.Handler

	// widgetDiagnostics is the in-memory ring buffer of widget render/parse
	// errors shown on the /widget-types/{slug} Diagnostics tab (P5,
	// ka-0000002154).
	widgetDiagnostics *widgetDiagnostics

	// htmx is the signer + rate-limiter backing
	// /htmx/widget/{widget}/{instance}/{action} (P3, ka-0000003324).
	htmx *htmxState

	// now is the clock (time.Now by default); tests pin it via SetNow.
	now func() time.Time

	// gardenerDormancyProbe, when set, replaces bridge.ReconcileDormancyProbe for
	// the plan cockpit's "parked" line (plans_gardener_park.go). Tests only; nil
	// in production.
	gardenerDormancyProbe func(ctx context.Context, gardener *beadsapi.BeadDetail) *bridge.ReconcileDormancy

	// userCaps memoizes the email → capabilities index from type=user beads,
	// gating the escalation approve/reject chips on approve:escalation (as the
	// bridge SPA does).
	userCaps userCapsCache

	// planNudgeMu serialises the read-modify-write of a plan's durable
	// fields.last_nudge_at (ka-0000002855) in shouldNudgePlanAssistant, so a
	// burst of "add step" clicks coalesces into one assistant nudge. The
	// explicit prompt verb (handlePlanAssistantPrompt) is never debounced.
	planNudgeMu sync.Mutex

	// mrLister is the GitLab REST client behind the /index "Merge requests"
	// rail panel. Nil → explanatory empty state. Wired from GITLAB_API_TOKEN +
	// GITLAB_BASE_URL + GARDENIO_GITLAB_PROJECT.
	mrLister mrLister
	// mrProjectPath is the project path the rail panel queries. Empty → panel
	// reports Available=false even when mrLister is set.
	mrProjectPath string
	// mrProjectURL is the public GitLab MR list URL for the panel header.
	mrProjectURL string

	// viewerProject is the deployment's project (BOAT_PROJECT, SetViewerProject).
	// /claude-config uses it as the project layer so `project:<p>, role:<r>`
	// claude-settings beads and project-scoped effort pins match. Empty →
	// global + role only.
	viewerProject string
	// mrCache is the TTL cache fronting mrLister. Always non-nil after NewServer.
	mrCache *mrStatusCache

	// imageS3 + imageBucket + imagePrefix back the /image/{key...} proxy
	// (ka-0000006131). Unset → the route answers 503. Set via SetImageProxy.
	imageS3     s3ImageGetter
	imageBucket string
	imagePrefix string
	// imageFallbackBucket is read when imageBucket misses (gd-CmjVAU3vgd).
	imageFallbackBucket string

	// cassetteStore backs /cassettes (Phase-7 S6, ka-0000006302). Nil → an
	// explicit empty state, never a 5xx. Set via SetCassetteStore.
	cassetteStore cassetteReader

	// metricsUpstream + metricsHTTP back the read-only GET /api/v1/query_range
	// proxy to vmsingle (ka-0000002747). Empty upstream → 503. Set via
	// SetMetricsProxy.
	metricsUpstream string
	metricsHTTP     *http.Client

	// grafanaProxy forwards /grafana to the in-cluster Grafana (grafanaUpstream),
	// stamping the verified viewer email into grafanaAuthHeader so Grafana rides
	// the AVA session (Option C, decision ka-0000004242; epic ka-0000000746).
	// Nil → 503. grafanaAuthHeader must equal Grafana's
	// GF_AUTH_PROXY_HEADER_NAME; empty means defaultGrafanaAuthProxyHeader.
	// Set via SetGrafanaProxy / SetGrafanaAuthProxyHeader. See grafana_proxy.go.
	grafanaProxy      *httputil.ReverseProxy
	grafanaUpstream   string
	grafanaAuthHeader string

	// planCritiqueFindingsEnabled dark-launches the computed inline
	// plan-critique panel on /plans/{id} (ka-0000002614), from
	// BEADS_VIEWER_PLAN_CRITIQUE_FINDINGS=1. Distinct from the manual
	// line-comment critique (epic ka-0000002316), which is always on.
	planCritiqueFindingsEnabled bool

	// operatorSlugCache memoizes operators(id) → canonical slug lookups so
	// that operator-chip rendering can emit /u/{slug} links (per-user-
	// homepage M2 §6 / ka-0000007787) without hitting the daemon on every
	// row. Backed by the GetOperatorSlug HTTP endpoint; populated lazily
	// by resolveOperatorSlug + populateOperatedBySlugs. nil-safe.
	operatorSlugCache *operatorSlugCache

	// gardenerOwnerCache memoizes the per-row gardener owner-resolution daemon
	// calls (user-bead lookup, Slack-alias lookup, operator-directory scan) so
	// the /gardeners index and /u/{slug} their-gardeners section don't fan one
	// uncached RTT — plus a full ListOperators directory scan — per gardener
	// row (perf regression fix ka-0000003665; the operatorSlugCache covered
	// only the slug leg). TTL-bounded; populated lazily by resolveGardenerOwner.
	// nil-safe.
	gardenerOwnerCache *gardenerOwnerCache

	// eventProbeCache memoizes the health gardeners tile's per-gardener probes
	// (in-flight run, ready-work count) for 60s (health.go). nil-safe.
	eventProbeCache *eventGardenerProbeCache

	// dashboardHubCache memoizes the enumerated Observability Hub (every
	// type=dashboard bead → grouped cards) for a short TTL, so a /charts render
	// burst folds into one enumeration pass instead of re-fanning
	// buildDashboardView across all dashboards per request (W2, ka-0000000773).
	// nil-safe; populated lazily by Server.dashboardHub.
	dashboardHubCache *dashboardHubCache

	// planProjectCache memoizes the ListProjectBeads round-trip behind
	// planProjectOptions for a short TTL (ka-0000006264). The per-viewer
	// project-filter chip row on /plans + /deck made that call universal (was
	// operator-gated), so a busy index burst now folds into one upstream fetch of
	// a list that changes only on project create/rename. TTL-bounded; caches only
	// successful fetches so the soft-degrade path is unchanged. nil-safe.
	planProjectCache *planProjectCache

	// planEnrichListCache memoizes the two viewer-invariant bulk list queries of
	// the /plans enrich pass (planAssistantBulk, planGardenersByEpic), the page's
	// dominant cost (ka-0000003113). TTL-bounded; caches only successful
	// fetches. nil-safe.
	planEnrichListCache *planEnrichListCache

	// inboxLegacyRoute maps `/inbox` to the legacy handleInbox renderer instead
	// of the production 308 → /u/me?view=dashboard. Tests only.
	inboxLegacyRoute bool
}

// SetResolver installs the OIDC-subject → operators(id) resolver.
func (s *Server) SetResolver(r *viewer.Resolver) {
	s.resolver = r
}

// SetMezmoClient installs the Mezmo live-data client.
func (s *Server) SetMezmoClient(c MezmoClient) {
	s.mezmo = c
}

// SetThinkingHub installs the telepathy NATS fan-out hub (ka-0000007528);
// main wires it when NATS_URL is set.
func (s *Server) SetThinkingHub(h *ThinkingHub) {
	s.thinking = h
}

// SetBeadFeedHub installs the activity-feed fan-out hub (ka-0000000643).
func (s *Server) SetBeadFeedHub(h *BeadFeedHub) {
	s.beadFeed = h
}

// handleHome serves "/" with a 302 (a product choice browsers must not cache):
// the gatehouse's steward for a person whose only land is the gatehouse
// (lands_gatehouse.go); a scoped principal's land, /garden?land=<land> or
// /lands/mine (lands_home.go, step 8.8); else /garden, the front door
// (decision gd-e17Jl61EcP). Without /garden, or for a scoped principal with no
// land, it serves the /feed Activity feed directly.
func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	if href := s.gatehouseHome(r); href != "" {
		http.Redirect(w, r, href, http.StatusFound)
		return
	}
	if href := s.landHome(r); href != "" {
		http.Redirect(w, r, href, http.StatusFound)
		return
	}
	if s.garden == nil || isScopedCtx(r.Context()) {
		s.handleFeed(w, r)
		return
	}
	http.Redirect(w, r, s.basePath+"/garden", http.StatusFound)
}

// SetCommonsHandler installs the Commons read routes (gd-LCtVzVbuWM).
func (s *Server) SetCommonsHandler(h *commons.Handler) {
	s.commons = h
}

// SetGardenHandler installs the /garden page handler (gd-BKx5ddIilZ).
func (s *Server) SetGardenHandler(h *gardenhub.Handler) {
	s.garden = h
}

// SetTunables installs the runtime config-bead watcher.
func (s *Server) SetTunables(w *beadsapi.ConfigWatcher) {
	s.tunables = w
}

// SetMRLister wires the GitLab client behind the /index "Merge requests"
// panel. projectURL is the panel header's deep link ("" if unknown).
func (s *Server) SetMRLister(lister mrLister, projectPath, projectURL string) {
	s.mrLister = lister
	s.mrProjectPath = projectPath
	s.mrProjectURL = projectURL
}

// SetViewerProject sets the deployment's project (BOAT_PROJECT), the
// /claude-config project layer.
func (s *Server) SetViewerProject(project string) {
	s.viewerProject = strings.TrimSpace(project)
}

// SetMachinePrincipal installs the viewer's own machine credential for its
// background reactors (see Server.machinePrincipal). nil disables it.
func (s *Server) SetMachinePrincipal(ts beadsapi.TokenSource) {
	s.machinePrincipal = ts
}

// daemonBearer is the bearer for raw daemon HTTP calls that bypass
// beadsapi.Client (health, charts proxy, gb prime): the machine principal's
// token (a fetch failure is an error, never a static-token fallback), else
// BEADS_DAEMON_TOKEN ("" when unset).
func (s *Server) daemonBearer(ctx context.Context) (string, error) {
	if s.machinePrincipal == nil {
		return s.beadsDaemonToken, nil
	}
	tok, err := s.machinePrincipal.Token(ctx)
	if err != nil {
		return "", err
	}
	if tok == "" {
		return "", beadsapi.ErrMachineAuthUnavailable
	}
	return tok, nil
}

// machineCtx returns ctx carrying the machine principal as the per-call daemon
// credential (beadsapi.WithMachinePrincipal), or ctx unchanged when none is
// configured. Reactors call it at entry so all their daemon calls authenticate
// as machine:<clientId>, never the static token or an unvouched human.
func (s *Server) machineCtx(ctx context.Context) context.Context {
	if s.machinePrincipal == nil {
		return ctx
	}
	return beadsapi.WithMachinePrincipal(ctx, s.machinePrincipal)
}

// templateAssets bundles every parse-once rendering asset NewServer derives
// purely from basePath: the page/partial/widget template sets and the two
// goldmark renderers. loadTemplateAssets memoizes it per basePath because the
// ~0.7s parse, repeated per test Server, busted the package timeout
// (ka-0000005687).
//
// Invariant: shared templates are read-only after parse. No handler may
// Execute a pages/partials entry directly; Clone first (see bindAgentLink),
// since html/template forbids Clone-after-Execute. widgetTemplates is the
// exception: executed directly (widgets_phase4.go), never cloned.
type templateAssets struct {
	pages           map[string]*template.Template
	partials        map[string]*template.Template
	widgetTemplates *template.Template
	md              goldmark.Markdown
	mdInline        goldmark.Markdown
}

var (
	templateAssetsMu    sync.Mutex
	templateAssetsCache = map[string]*templateAssets{}
)

// loadTemplateAssets returns the memoized parse for basePath, building it on
// first use under templateAssetsMu.
func loadTemplateAssets(basePath string) *templateAssets {
	templateAssetsMu.Lock()
	defer templateAssetsMu.Unlock()
	if a, ok := templateAssetsCache[basePath]; ok {
		return a
	}
	a := newTemplateAssets(basePath)
	templateAssetsCache[basePath] = a
	return a
}

// newTemplateAssets parses every page, partial, and widget template and wires
// the template func map. basePath is the only input.
func newTemplateAssets(basePath string) *templateAssets {
	// Fragment partials are parsed into every page AND may be served
	// standalone via htmx routes (e.g. /b/{id}/comments).
	fragmentPartials := []string{
		"templates/partials/comments.html",
		"templates/partials/comment_preview.html",
		// plan_critique_thread: one plan step's inline-critique thread (epic
		// ka-0000002316), standalone via /plans/{id}/comments (plans_critique.go).
		"templates/partials/plan_critique_thread.html",
		// plan_critique (T4, ka-0000000167): the line-critique overlay shell
		// embedded by plan_detail.html; plan_gutter.js fills it with threads.
		"templates/partials/plan_critique.html",
		// plan_critique_findings (ka-0000002614): the computed critique panel,
		// page-embedded only, behind BEADS_VIEWER_PLAN_CRITIQUE_FINDINGS=1.
		"templates/partials/plan_critique_findings.html",
		"templates/partials/bead_chat.html",
		"templates/partials/deps.html",
		"templates/partials/type_decision.html",
		// type_escalation (ka-0000006884): embedded by bead_show.html and the
		// approve/reject swap payload (escalation_actions.go).
		"templates/partials/type_escalation.html",
		// decision_context: the /decisions/{id} sidebar, embedded by
		// decision_detail.html and the reflag swap payload (decision_reflag.go).
		"templates/partials/decision_context.html",
		// request_origin: the agent / task / plan strip on decision and
		// escalation rows and cards (gd-AREKDbHx70); type_decision and
		// type_escalation render standalone, so it is a fragment (see partialDeps).
		"templates/partials/request_origin.html",
		"templates/partials/bead_actions.html",
		"templates/partials/priority_block.html",
		"templates/partials/priority_edit.html",
		"templates/partials/assignee_block.html",
		"templates/partials/assignee_edit.html",
		"templates/partials/description_block.html",
		"templates/partials/description_edit.html",
		"templates/partials/description_preview.html",
		"templates/partials/labels_block.html",
		"templates/partials/labels_edit.html",
		"templates/partials/owner_block.html",
		"templates/partials/owner_edit.html",
		"templates/partials/status_edit.html",
		"templates/partials/title_block.html",
		"templates/partials/title_edit.html",
		"templates/partials/bead_show_inline_header.html",
		"templates/partials/active_row.html",
		"templates/partials/project_row.html",
		"templates/partials/row_badges.html",
		"templates/partials/constellation.html",
		"templates/partials/rating_widget.html",
		"templates/partials/quick_spawn.html",
		// peel_request (ka-0000008213): the "request peel" affordance, embedded
		// per row by peel_catalog.html and the POST /peel-catalog/{id}/request
		// swap payload.
		"templates/partials/peel_request.html",
		"templates/partials/create_advice.html",
		"templates/partials/create_taint.html",
		"templates/partials/create_gardener.html",
		"templates/partials/settings_block.html",
		"templates/partials/settings_edit.html",
		"templates/partials/health_tile_open_beads.html",
		"templates/partials/health_tile_fleet.html",
		"templates/partials/health_tile_events.html",
		"templates/partials/health_tile_incidents.html",
		"templates/partials/health_tile_bridges.html",
		"templates/partials/health_tile_decisions.html",
		"templates/partials/health_tile_gardeners.html",
		"templates/partials/health_tile_costs.html",
		"templates/partials/health_tile_build.html",
		"templates/partials/health_tile_escalations.html",
		"templates/partials/health_tile_operator_signals.html",
		"templates/partials/bead_activity.html",
		"templates/partials/agent_permission_diff.html",
		// jira_timeline: read-only Jira timeline for source:jira beads (F-C,
		// epic ka-0000007401); page-embedded only.
		"templates/partials/jira_timeline.html",
		"templates/partials/migration_banner.html",
		"templates/partials/inbox_homepage_banner.html",
		// garden_banner is the "Open it in the garden" notice on a page that is
		// retiring into /garden (garden_retire.go, Lands workbench-glass §2.6).
		"templates/partials/garden_banner.html",
		"templates/partials/inbox_decisions.html",
		"templates/partials/inbox_row.html",
		"templates/partials/inbox_adjacent.html",
		"templates/partials/forward_panel.html",
		"templates/partials/trigger_toggle.html",
		"templates/partials/gardener_pause_toggle.html",
		// gardener_run_now (ka-0000006826): the /gardeners row Run-now action,
		// embedded by gardeners.html and rendered standalone by
		// handleGardenerRunNow's row=1 branch.
		"templates/partials/gardener_run_now.html",
		"templates/partials/trigger_row.html",
		"templates/partials/trigger_bulk_toolbar.html",
		"templates/partials/user_home_section.html",
		"templates/partials/gardener_iteration_card.html",
		// agents_row: the /agents row markup shared by agents.html and the SSE
		// writer (renderAgentsIndexRow), so live swaps match the static render
		// (ka-0000007652).
		"templates/partials/agents_row.html",
		// agents_gardener: the /agents?group=gardener view (ka-0000006067),
		// page-embedded by agents.html.
		"templates/partials/agents_gardener.html",
		"templates/partials/gardener_runs_section.html",
		"templates/partials/gardener_invocations.html",
		"templates/partials/gardener_beads.html",
		"templates/partials/gardener_controls.html",
		"templates/partials/gardener_config.html",
		"templates/partials/gardener_successor.html",
		"templates/partials/project_config.html",
		"templates/partials/operator_hover_card.html",
		"templates/partials/user_status_message_block.html",
		"templates/partials/user_status_message_edit.html",
		"templates/partials/user_home_inbox.html",
		// user_home_activity: user_home.html's activity spine (ka-0000002041).
		"templates/partials/user_home_activity.html",
		// costs_rollup + costs_rollup_table (decision ka-0000000742): embedded by
		// costs.html; the table is the htmx swap target on control change.
		"templates/partials/costs_rollup.html",
		// thread_timeline (ka-0000006034): embedded by thread_detail.html and the
		// ?since= poll target (handleThreadTimeline); thread_composer rides along.
		"templates/partials/thread_timeline.html",
		"templates/partials/thread_composer.html",
		// plan_steps / plan_narrative (epic ka-0000001409, S3 ka-0000002473):
		// embedded by plan_detail.html; plan_steps is the add/close/reopen swap
		// target, plan_narrative the /plans/{id}/narrative fragment and SSE
		// plan-narrative-updated payload.
		"templates/partials/plan_steps.html",
		// plan_pour: the "⚗ Pour a formula" disclosure (epic ka-0000000759) in
		// the plan_steps footer, standalone via GET /plans/{id}/pours/new and
		// POST /plans/{id}/pours; also a plan_steps partialDep below.
		"templates/partials/plan_pour.html",
		// The Share disclosure (PW.4, plan_share.go): embedded by plan_detail.html
		// and served standalone by GET/POST /plans/{id}/share.
		"templates/partials/plan_share.html",
		"templates/partials/plan_narrative.html",
		// plans_master_switch (ka-0000004048): pause/resume-all control + pause
		// banner, embedded by plans.html and the POST
		// /plans/master-switch/toggle swap payload.
		"templates/partials/plans_master_switch.html",
		// plan_decisions (ka-0000003157): clarifying questions on /plans/{id},
		// re-rendered by /plans/{id}/decisions/{d}/{resolve,dismiss} and the SSE
		// plan-decisions-updated frame.
		"templates/partials/plan_decisions.html",
		// plan_agent (ka-0000000116): the #plan-agent card, swapped by the
		// /plans/{id}/assistant/prompt, /plans/{id}/gardener/{toggle,wake} and
		// /plans/{id}/{state,build} POSTs and the SSE plan-agent-updated event.
		"templates/partials/plan_agent.html",
		// plan_escalations (epic ka-0000005480, WS4): the blocked-by-escalation
		// banner, re-rendered by the SSE plan-escalations-updated event.
		"templates/partials/plan_escalations.html",
		// plan_crew (ka-0000000150): the crew roster, re-rendered by the SSE
		// plan-crew-updated event.
		"templates/partials/plan_crew.html",
		// plan_card (ka-0000003881): one /plans card, rendered by plans.html and
		// standalone by handlePlanGardenerRowToggle.
		"templates/partials/plan_card.html",
		// plan_tree: one inline-SVG tree in /plans?view=tree (plans_tree.go,
		// ka-0000005850).
		"templates/partials/plan_tree.html",
		// plans_bulk_toolbar (ka-0000004771): rendered by plans.html and by the
		// bulk handlers (plans_bulk.go) as the swap target.
		"templates/partials/plans_bulk_toolbar.html",
		// plan_peek: the card-peek drawer body, served only by GET
		// /plans/{id}/peek (plans_board.go).
		"templates/partials/plan_peek.html",
	}
	// Page-only partials — included by bead_show.html's per-type dispatch but
	// never rendered as standalone fragments.
	pagePartials := []string{
		// release_shipped is the "Shipped in <tag>" line on the bead view and
		// the plan page (release_page.go).
		"templates/partials/release_shipped.html",
		// releases_words: the words releases.js puts on /releases and
		// /release/{id}, shared by both pages.
		"templates/partials/releases_words.html",
		// filter_strip is the one page-level filter strip (F6, ka-0000005357);
		// pages range a []filterChip through it rather than hand-rolling markup.
		"templates/partials/filter_strip.html",
		// filter_row (multi-row control panel) and section_tabs (page-section
		// tab bar) are the shared sub-nav components (F7, ka-0000006647).
		"templates/partials/filter_row.html",
		"templates/partials/section_tabs.html",
		"templates/partials/type_generic.html",
		"templates/partials/type_schedule.html",
		// type_gardener (ka-0000006542) and gardener_detail.html share
		// gardener_summary; type_gardener falls back to type_schedule without a
		// gardener spec.
		"templates/partials/type_gardener.html",
		"templates/partials/gardener_summary.html",
		"templates/partials/type_formula.html",
		// formula_dag_svg: the step DAG shared by type_formula and type_molecule
		// (ka-0000007739); also in the standalone type_molecule set below.
		"templates/partials/formula_dag_svg.html",
		// type_molecule (ka-0000007739): its molecule_exec fragment is also
		// rendered by the /molecule/{id}/events SSE writer (registered below).
		"templates/partials/type_molecule.html",
		"templates/partials/type_mr.html",
		// trickle_checklist: the R3 trickle-MR eligibility grid on a
		// type=merge-request bead (ka-0000005053 §5/§7, epic ka-0000005560); a
		// no-op elsewhere.
		"templates/partials/trickle_checklist.html",
		"templates/partials/mr_pill.html",
		"templates/partials/type_agent.html",
		"templates/partials/agent_usage_chips.html",
		// lineage_chip: type_agent's lineage-strip chip (ka-0000007852).
		"templates/partials/lineage_chip.html",
		"templates/partials/type_advice.html",
		"templates/partials/type_dashboard.html",
		"templates/partials/type_rating.html",
		"templates/partials/type_rich_report.html",
		"templates/partials/type_report.html",
		"templates/partials/type_bundle.html",
		"templates/partials/type_widget_type.html",
		"templates/partials/widget_type_preview.html",
		"templates/partials/widget_type_diagnostics.html",
		"templates/partials/widgets_phase4.html",
		"templates/partials/ratings_summary.html",
		"templates/partials/agent_screen.html",
		// taint_badges renders the bead-page taint badges (ADR
		// docs/adr/taints.md §9.1, task gd-jz56jXsHrK) in the bead-show header.
		"templates/partials/taint_badges.html",
	}
	// Chroma highlighting with inline styles (no stylesheet needed for htmx
	// swaps). Built before the func map so helpers can close over mdInline.
	// blockIDHighlightingOptions carries data-block-id onto fenced code blocks
	// (T1, epic ka-0000002316), which the highlighter would otherwise drop.
	hlOpts := append([]highlighting.Option{
		highlighting.WithStyle("github-dark"),
		highlighting.WithFormatOptions(chromahtml.WithClasses(false)),
	}, blockIDHighlightingOptions()...)
	hl := highlighting.NewHighlighting(hlOpts...)
	// withHeadingAnchors gives headings label-derived ids so
	// `ka-0000001713#C1.1` deep-links (epic ka-0000006913). withBlockIDs stamps
	// data-block-id on blocks for critique anchors (T1). withMermaid makes a
	// ```mermaid fence a <pre class="mermaid-diagram"> for mermaid_render.js
	// (ka-0000005977); at priority 90 it runs before block-IDs (110) and
	// highlighting (200). See mermaid.go for the XSS defense.
	md := goldmark.New(
		goldmark.WithExtensions(extension.GFM, hl),
		goldmark.WithParserOptions(withHeadingAnchors(), withBlockIDs(), withMermaid()),
		goldmark.WithRendererOptions(withBlockIDRenderers(), withMermaidRenderer()),
	)
	// mdInline uses hard wraps: in short comments a newline means a line break.
	mdInline := goldmark.New(
		goldmark.WithExtensions(extension.GFM, hl),
		goldmark.WithParserOptions(withHeadingAnchors(), withBlockIDs(), withMermaid()),
		goldmark.WithRendererOptions(html.WithHardWraps(), withBlockIDRenderers(), withMermaidRenderer()),
	)
	funcs := template.FuncMap{
		"humanizeTime": humanizeTime,
		// emptyPrompt is the placeholder of every empty box where a human types
		// to the fleet (gardenhub.EmptyPrompt, gd-5e6xgIOH6B); the box carries
		// class="empty-prompt" for the muted placeholder colour in layout.html.
		"emptyPrompt": func() string { return gardenhub.EmptyPrompt },
		// landsShortTitle drops the product prefix from a legal document title
		// ("Gardenio Lands Terms of Service" -> "Terms of Service"), lands.html.
		"landsShortTitle": landsShortTitle,
		// displayTitle shows a bead's short_title (full title on hover) in list
		// rows, cards and plan steps, falling back to the title (gd-V5nGseRS5f).
		"displayTitle": displayTitle,
		// permissionRuleLabel names a ServiceAccount permission-diff row the
		// way `kubectl auth can-i --list` does (agent_permission_diff.html).
		"permissionRuleLabel": permissionDiffRuleLabel,
		// The Lands music player (internal/tunes, gd-z8nt2TpS6o), inlined by
		// lands.html's "lands_tunes".
		"tunesCSS":    func() template.CSS { return template.CSS(tunes.CSS) },
		"tunesJS":     func() template.JS { return template.JS(tunes.JS) },               //nolint:gosec // embedded at build time, not user input
		"tunesMarkup": func() template.HTML { return template.HTML(tunes.Markup(true)) }, //nolint:gosec // fixed markup from the package
		// The time-of-day skin (skin_clock.go, gd-yV1HX7zGNE), inlined in <head>
		// so a viewer with no stored skin gets one before first paint.
		"skinClockJS":     func() template.JS { return template.JS(skinClockJS) }, //nolint:gosec // a package constant, not user input
		"landsStateWords": landsStateWords,
		"landsCredits":    formatCredits,
		"landsDate":       func(t time.Time) string { return t.UTC().Format("2 January 2006") },
		"landsCapital": func(s string) string {
			if s == "" {
				return s
			}
			return strings.ToUpper(s[:1]) + s[1:]
		},
		// threadMessageMarkdown renders a /threads message through the bead
		// comment pipeline (renderCommentMarkdown) for parity (ka-0000001087).
		"threadMessageMarkdown": func(text string) template.HTML {
			return renderCommentMarkdown(mdInline, text, basePath)
		},
		// threadParticipantAvatar renders a /threads participant's initials
		// avatar, linked to /u/{slug} when resolvable (ka-0000000064);
		// threadParticipantOverflow renders the "+N" beyond the per-card cap.
		"threadParticipantAvatar": func(p threadParticipant) template.HTML {
			return renderThreadParticipantAvatar(p, basePath)
		},
		"threadParticipantOverflow": renderThreadParticipantOverflow,
		"operatorChip":              renderOperatorChip,
		// fieldLabel / fieldValue render one bead field in the Fields dump and the
		// escalation card (ka-0000006884): markdown + Linkify, humanized
		// timestamps, and nested expansion of JSON blobs (e.g. operation_spec).
		"fieldLabel": humanizeFieldKey,
		"fieldValue": func(v string) template.HTML {
			return renderFieldValue(mdInline, v, basePath, time.Now())
		},
		"beadOperatorChip":   beadOperatorChip,
		"operatedByBanner":   operatedByBanner,
		"operatorHeaderChip": operatorHeaderChipHelper,
		// planHeaderChip is the plan-count-aware /plans variant of
		// operatorHeaderChip (ka-0000005414).
		"planHeaderChip": renderPlanHeaderChip,
		// planCreatorChip: the /plans card's creator chip (S2, ka-0000002728).
		"planCreatorChip": planCreatorChip,
		// planCreatorChipScoped suppresses the chip when the card's operator is
		// the board's ?operated_by= filter (ka-0000005352 iter-61).
		"planCreatorChipScoped": planCreatorChipScoped,
		// planLaneCrew sums the rows' .Crew rollup (working + open-stuck) for a
		// /plans lane or ?group= section header (ka-0000005352); same source as
		// the card facets, no extra daemon call (advice ka-0000005924).
		"planLaneCrew": planLaneCrew,
		// badge is a bead's badge picture (icon_svg_url / icon_png_url) as
		// /image/ proxy paths, nil when none (badge_image.go).
		"badge": beadBadge,
		// heraldry renders a bead's arms in a slot (24 rows, 40 cards, 64 the
		// bead page H1) as the emoji run, nothing when it bears none (arms.go).
		"heraldry": heraldrySpan,
		// bearerArms is a bead's arms only when its type may bear them (arms.go).
		"bearerArms": bearerArms,
		// actorInitials condenses a display name to 1–4 uppercased initials for
		// narrow /inbox rows (ka-0000005380).
		"actorInitials": actorInitials,
		// agentLink links an agent-shaped name to its agent bead, else plain
		// text. This parse-time stub has a nil resolver (always plain text);
		// render()/renderPartial() rebind it per request. See agent_link.go.
		"agentLink": func(name string) template.HTML {
			return renderAgentLink(basePath, name, nil)
		},
		// agentLiveIcon renders a green dot linking to the agent bead when the
		// name resolves to a live agent, else "". Parse-time stub with a nil
		// resolver; bindAgentLink rebinds it per request.
		"agentLiveIcon": func(name string) template.HTML {
			return renderAgentLiveIcon(basePath, name, nil)
		},
		// deref renders a *string, or "" when nil (deps.html relationship_type).
		// dict (below) builds a map[string]any from alternating key/value pairs,
		// for passing several values to a sub-template.
		"deref": func(p *string) string {
			if p == nil {
				return ""
			}
			return *p
		},
		// upMnemonic renders an /up footer label with the 0-based rune idx
		// underlined (<0 = plain; PRD §4.3). Segments are escaped.
		"upMnemonic": upMnemonicHTML,
		"dict": func(pairs ...any) (map[string]any, error) {
			if len(pairs)%2 != 0 {
				return nil, fmt.Errorf("dict: odd number of arguments")
			}
			out := make(map[string]any, len(pairs)/2)
			for i := 0; i < len(pairs); i += 2 {
				k, ok := pairs[i].(string)
				if !ok {
					return nil, fmt.Errorf("dict: key %d is not a string", i)
				}
				out[k] = pairs[i+1]
			}
			return out, nil
		},
		// userLink links a display name to /u/{slug} (ka-0000007502); escaped
		// plain text when slug is empty, empty HTML when display is empty.
		"userLink": func(display, slug string) template.HTML {
			return renderUserLink(display, slug, basePath)
		},
		// ownerInitials renders an owner's initials avatar for agent-list rows,
		// linked to /u/{slug} when resolvable (ka-0000001514, ka-0000007078).
		"ownerInitials": func(display, slug string) template.HTML {
			return renderOwnerInitialsBadge(display, slug, basePath)
		},
		// Phase 7 — uPlot config blobs serialized into data-uplot-config.
		"sparkConfig":    MarshalSparkConfig,
		"timelineConfig": MarshalTimelineConfig,
		// liveAttrs: htmx polling attrs for allowlisted live widgets, else "".
		"liveAttrs": func(slug, beadID string) template.HTMLAttr {
			return template.HTMLAttr(liveAttrs(slug, beadID, basePath))
		},
		// liveAttrsCard is liveAttrs with an explicit interval (Mezmo-sourced
		// widgets pick their own cadence, ka-0000002519).
		"liveAttrsCard": func(slug, beadID string, intervalSecs int) template.HTMLAttr {
			return template.HTMLAttr(liveAttrsFor(slug, beadID, basePath, intervalSecs))
		},
		// sub is integer subtraction (e.g. "Show {{sub total head}} more").
		"sub": func(a, b int) int { return a - b },
		// add is used for 1-based step numbering (epic ka-0000001409).
		"add": func(a, b int) int { return a + b },
		// highlightQuery bolds case-insensitive matches of query in text, as
		// escaped HTML (/search, ka-0000005947).
		"highlightQuery": highlightQuery,
		// barHeight is a sparkline bar's height as a percentage of max, floored
		// to 4% for non-zero counts so small bars stay visible (ka-0000003534).
		"barHeight": func(count, max int) int {
			if max <= 0 || count <= 0 {
				return 0
			}
			h := count * 100 / max
			if h < 4 {
				return 4
			}
			return h
		},
	}
	pageNames := []string{"index.html", "feed.html", "bead_show.html", "bead_ownership.html", "search.html", "project.html", "active.html", "formulas.html", "formula_pour.html", "decisions.html", "decision_detail.html", "escalations.html", "me_taints.html", "peel_catalog.html", "extern.html", "agents.html", "telepathy.html", "cassettes.html", "advice.html", "triggers.html", "costs.html", "charts.html", "dashboards.html", "molecule_cost.html", "views_index.html", "view_show.html", "prime_show.html", "ratings.html", "settings.html", "health.html", "trust.html", "incident_intel_gallery.html", "inbox.html", "bundle.html", "widget_types_index.html", "widget_type_show.html", "up.html", "user_home.html", "user_inbox.html", "people.html", "fleet_index.html", "fleet_bridge.html", "gardener_detail.html", "gardeners.html", "maps.html", "role_hub.html", "project_hub.html", "roles.html", "pool.html", "old_viewer.html", "claude_config.html", "claude_config_detail.html", "threads.html", "thread_detail.html", "plans.html", "plan_detail.html", "deck.html", "taints.html", "tool_latency.html", "taint_detail.html", "team.html", "releases.html", "release.html"}
	pages := make(map[string]*template.Template, len(pageNames)+2)
	for _, name := range pageNames {
		files := []string{"templates/layout.html", "templates/" + name}
		files = append(files, fragmentPartials...)
		files = append(files, pagePartials...)
		pages[name] = template.Must(template.New("").Funcs(funcs).ParseFS(templateFS, files...))
	}
	// login.html is standalone (no layout): served unauthenticated, it must not
	// depend on templates that reference oidcauth state.
	pages["login.html"] = template.Must(template.New("").Funcs(funcs).ParseFS(templateFS, "templates/login.html"))
	// lands.html is standalone too: the Lands front of house (lands.go) serves
	// visitors with no session or role.
	pages["lands.html"] = template.Must(template.New("").Funcs(funcs).ParseFS(templateFS, "templates/lands.html"))
	// window.html is the Window's page (window.go): standalone, served with no
	// session, in the Lands skin (lands.html's lands_style).
	pages["window.html"] = template.Must(template.New("").Funcs(funcs).ParseFS(templateFS, "templates/lands.html", "templates/window.html"))
	// partialDeps lists the companion templates a partial's {{template}} refs
	// need when it is rendered standalone (pages already parse every fragment).
	partialDeps := map[string][]string{
		"templates/partials/bead_actions.html": {
			"templates/partials/status_edit.html",
		},
		// type_decision / type_escalation embed request_origin.
		"templates/partials/type_decision.html": {
			"templates/partials/request_origin.html",
		},
		"templates/partials/type_escalation.html": {
			"templates/partials/request_origin.html",
		},
		// plan_steps embeds plan_pour in its footer (epic ka-0000000759).
		"templates/partials/plan_steps.html": {
			"templates/partials/plan_pour.html",
		},
		// description_edit embeds description_preview.
		"templates/partials/description_edit.html": {
			"templates/partials/description_preview.html",
		},
		// comments embeds comment_preview.
		"templates/partials/comments.html": {
			"templates/partials/comment_preview.html",
		},
		// project_row is re-rendered standalone by OOB swaps.
		"templates/partials/project_row.html": {
			"templates/partials/row_badges.html",
		},
		// trigger_row is re-rendered standalone by bulk OOB swaps.
		"templates/partials/trigger_row.html": {
			"templates/partials/trigger_toggle.html",
			"templates/partials/row_badges.html",
		},
		// The per-run cost/tokens strip is agent_usage_chips (ka-0000001009 / C2).
		"templates/partials/gardener_iteration_card.html": {
			"templates/partials/mr_pill.html",
			"templates/partials/agent_usage_chips.html",
		},
		// gardener_invocations shows per-run usage chips (ka-0000001009 / C2).
		"templates/partials/gardener_invocations.html": {
			"templates/partials/agent_usage_chips.html",
		},
		// agents_row is rendered standalone by the SSE writer (ka-0000007652).
		"templates/partials/agents_row.html": {
			"templates/partials/agent_usage_chips.html",
			"templates/partials/mr_pill.html",
		},
		// gardener_runs_section embeds gardener_iteration_card and its deps.
		"templates/partials/gardener_runs_section.html": {
			"templates/partials/gardener_iteration_card.html",
			"templates/partials/mr_pill.html",
			// agent_usage_chips: via gardener_iteration_card (C2).
			"templates/partials/agent_usage_chips.html",
		},
		// gardener_beads → gardener_constellation → constellation_node
		// (constellation.html); re-requested by the degree pills (ka-0000007657).
		"templates/partials/gardener_beads.html": {
			"templates/partials/gardener_constellation.html",
			"templates/partials/constellation.html",
		},
		// plan_agent embeds the bead_chat transcript (ka-0000004607).
		"templates/partials/plan_agent.html": {
			"templates/partials/bead_chat.html",
		},
		// plan_card uses plan_agent_chip (plan_agent.html, which needs bead_chat)
		// and gardener_pause_toggle.
		"templates/partials/plan_card.html": {
			"templates/partials/plan_agent.html",
			"templates/partials/gardener_pause_toggle.html",
			"templates/partials/bead_chat.html",
		},
		// plan_peek embeds plan_steps (and its plan_pour, epic ka-0000000759),
		// plan_agent_chip, plan_crew and bead_chat (ka-0000002125).
		"templates/partials/plan_peek.html": {
			"templates/partials/plan_steps.html",
			// plan_steps embeds plan_pour (epic ka-0000000759).
			"templates/partials/plan_pour.html",
			"templates/partials/plan_agent.html",
			"templates/partials/plan_crew.html",
			"templates/partials/bead_chat.html",
		},
	}
	partials := make(map[string]*template.Template, len(fragmentPartials)+1)
	for _, p := range fragmentPartials {
		files := append([]string{p}, partialDeps[p]...)
		partials[p] = template.Must(template.New("").Funcs(funcs).ParseFS(templateFS, files...))
	}
	// Standalone-only fragments are registered directly so pages don't parse
	// them. bead_preview is served by /b/{id}/preview.
	partials["templates/partials/bead_preview.html"] = template.Must(template.New("").Funcs(funcs).ParseFS(templateFS,
		"templates/partials/bead_preview.html"))
	// bead_preview_mr is handleBeadPreview's type=merge-request variant.
	partials["templates/partials/bead_preview_mr.html"] = template.Must(template.New("").Funcs(funcs).ParseFS(templateFS,
		"templates/partials/bead_preview_mr.html"))
	// cmdk_results is the command palette's swap target (cmdk.js).
	partials["templates/partials/cmdk_results.html"] = template.Must(template.New("").Funcs(funcs).ParseFS(templateFS,
		"templates/partials/cmdk_results.html"))
	// deck_card is GET /plans/{id}/card (deck_card.go); it has no
	// {{template}} includes by design.
	partials["templates/partials/deck_card.html"] = template.Must(template.New("").Funcs(funcs).ParseFS(templateFS,
		"templates/partials/deck_card.html"))
	// agent_screen is GET /agents/{id}/screen, the terminal fallback when the
	// live WebSocket drops (agent_terminal.js).
	partials["templates/partials/agent_screen.html"] = template.Must(template.New("").Funcs(funcs).ParseFS(templateFS,
		"templates/partials/agent_screen.html"))
	// widget_type_preview / widget_type_diagnostics are /widget-types/{slug}
	// tab swap targets (P5, ka-0000002154); also in pagePartials.
	partials["templates/partials/widget_type_preview.html"] = template.Must(template.New("").Funcs(funcs).ParseFS(templateFS,
		"templates/partials/widget_type_preview.html"))
	partials["templates/partials/widget_type_diagnostics.html"] = template.Must(template.New("").Funcs(funcs).ParseFS(templateFS,
		"templates/partials/widget_type_diagnostics.html"))
	// type_molecule's molecule_exec is rendered by the /molecule/{id}/events
	// SSE writer (renderMoleculeExecFragment, ka-0000007739); it needs
	// formula_dag_svg.
	partials["templates/partials/type_molecule.html"] = template.Must(template.New("").Funcs(funcs).ParseFS(templateFS,
		"templates/partials/type_molecule.html",
		"templates/partials/formula_dag_svg.html"))
	// project_hub_section is the GET /b/project/{name}/<section> lazy
	// fragment (ka-0000004817).
	partials["templates/partials/project_hub_section.html"] = template.Must(template.New("").Funcs(funcs).ParseFS(templateFS,
		"templates/partials/project_hub_section.html"))
	// role_hub_agents is the lazy GET /b/role/{role}/agents fragment
	// (ka-0000004054).
	partials["templates/partials/role_hub_agents.html"] = template.Must(template.New("").Funcs(funcs).ParseFS(templateFS,
		"templates/partials/role_hub_agents.html"))

	widgetTemplates := template.Must(template.New("").Funcs(funcs).ParseFS(templateFS,
		"templates/partials/widgets_phase4.html",
		"templates/partials/widgets_phase5.html",
		"templates/partials/widgets_phase6.html",
		"templates/partials/widgets_phase7.html"))

	return &templateAssets{
		pages:           pages,
		partials:        partials,
		widgetTemplates: widgetTemplates,
		md:              md,
		mdInline:        mdInline,
	}
}

// NewServer creates a beads viewer server. beadsHTTPAddr / beadsDaemonToken
// are forwarded to the gb prime subprocess (prime.go). coopmux may be nil.
func NewServer(daemon beadAPI, logger *slog.Logger, basePath, beadsHTTPAddr, beadsDaemonToken string, coopmux screenClient) *Server {
	assets := loadTemplateAssets(basePath)
	// Shallow-copy the maps so tests injecting probe pages stay local to this
	// Server; the *Template values stay shared.
	pages := make(map[string]*template.Template, len(assets.pages))
	maps.Copy(pages, assets.pages)
	partials := make(map[string]*template.Template, len(assets.partials))
	maps.Copy(partials, assets.partials)

	staticSub, err := fs.Sub(staticFS, "templates/static")
	if err != nil {
		// PANIC: embed.FS with a compile-time path can't fail at runtime;
		// if fs.Sub errors here the binary is fundamentally broken.
		panic(err)
	}

	actions := NewActionRegistry()
	registerStandardActions(actions)
	s := &Server{
		daemon:              daemon,
		logger:              logger,
		evidence:            decisionview.NewEvidenceCache(0),
		pages:               pages,
		partials:            partials,
		basePath:            basePath,
		theme:               themeMeadow,
		md:                  assets.md,
		mdInline:            assets.mdInline,
		static:              staticCached(overlayFS{staticSub, gardenhub.XtermFS}),
		beadsHTTPAddr:       beadsHTTPAddr,
		beadsDaemonToken:    beadsDaemonToken,
		coopmux:             coopmux,
		actions:             actions,
		widgetTemplates:     assets.widgetTemplates,
		codeHighlighter:     newCodeHighlighter(),
		mezmo:               noopMezmoClient{},
		mezmoCache:          newMezmoCache(128),
		widgetDiagnostics:   newWidgetDiagnostics(),
		htmx:                newHtmxState(),
		now:                 time.Now,
		mrCache:             &mrStatusCache{},
		feedSeedCache:       &feedSeedCache{},
		gardenerOwnerCache:  newGardenerOwnerCache(),
		eventProbeCache:     newEventGardenerProbeCache(),
		dashboardHubCache:   newDashboardHubCache(),
		planProjectCache:    &planProjectCache{},
		planEnrichListCache: newPlanEnrichListCache(),
	}
	// Phase 6: server-bound action handlers (need access to s.daemon for the
	// bypass-record audit comment). Registered after the Server is built.
	registerServerBoundActions(s)
	if wa, ok := daemon.(windowAPI); ok {
		if c, isClient := wa.(*beadsapi.Client); !isClient || c != nil {
			s.window = newWindowCache(wa)
		}
	}
	if lt, ok := daemon.(landsTermsAPI); ok {
		// A typed-nil *beadsapi.Client (no daemon configured) is not a client.
		if c, isClient := lt.(*beadsapi.Client); !isClient || c != nil {
			s.landsTerms = lt
		}
	}
	return s
}

// newBundleRenderCtx builds the render context for /bundle/<id> per-format
// renderers (one per handleBundleShow), so they need no *Server.
func (s *Server) newBundleRenderCtx() *bundleRenderCtx {
	return &bundleRenderCtx{
		md:          s.md,
		hl:          s.codeHighlighter,
		basePath:    s.basePath,
		imageHosts:  bundleImageHostWhitelist(),
		imageBucket: s.imageBucket,
		imagePrefix: s.imagePrefix,

		imageFallbackBucket: s.imageFallbackBucket,
	}
}

// newWidgetRenderCtx builds the widget render context, once per rich-report
// render (buildRichReportView).
func (s *Server) newWidgetRenderCtx() *widgetRenderCtx {
	return &widgetRenderCtx{
		md:         s.md,
		templates:  s.widgetTemplates,
		hl:         s.codeHighlighter,
		mezmo:      s.mezmo,
		mezmoCache: s.mezmoCache,
	}
}

// RegisterRoutes adds all application routes to the mux.
func (s *Server) RegisterRoutes(mux routeRegistrar) {
	// "/" lands on /garden when wired, else the feed (decisions ka-0000004036,
	// gd-e17Jl61EcP).
	mux.HandleFunc("GET /{$}", s.handleHome)
	mux.HandleFunc("GET /index", s.handleIndex)
	// /feed and /active retire into the garden's activity tab behind a flag
	// (R5, garden_retire.go).
	mux.HandleFunc("GET /feed", s.retireToGarden(retireActivity, gardenActivityTarget, s.handleFeed))
	mux.HandleFunc("GET /feed/events", s.handleFeedEvents)
	// /feed/backfill: reconnect gap-fill by event time (?since=<RFC3339>,
	// ka-0000000643).
	mux.HandleFunc("GET /feed/backfill", s.handleFeedBackfill)
	// /search retires into the garden's search panel behind a flag (R4,
	// garden_retire.go).
	mux.HandleFunc("GET /search", s.retireToGarden(retireSearch, gardenSearchTarget, s.handleSearch))
	// /garden: snapshot-rendered live page + SSE patch stream (gd-BKx5ddIilZ).
	// More specific than the "GET /{id}" bead catch-all, so it wins.
	if s.garden != nil {
		s.garden.Register(mux)
	}
	// /garden/c: the Commons read model (channels, timeline, thread, message
	// fragment, per-principal event stream), read on the viewer's own bearer.
	if s.commons != nil {
		s.commons.Register(mux)
	}
	mux.HandleFunc("GET /active", s.retireToGarden(retireActivity, gardenActivityTarget, s.handleActive))
	// /formulas: formula catalog (ka-0000007376, S1 ka-0000000872). The Pour
	// POST (ka-0000000251) is operator-gated below.
	mux.HandleFunc("GET /formulas", s.handleFormulas)
	mux.HandleFunc("GET /formulas/{id}/pour", s.handleFormulaPourForm)
	mux.HandleFunc("GET /me", s.handleMe)
	// /me/taints (docs/adr/taints.md §9.1 "My taints"): taints on the viewer's
	// beads. Needs its own pattern ("/me" is exact-match); its POST is
	// operator-gated below.
	mux.HandleFunc("GET /me/taints", s.handleMeTaints)
	mux.HandleFunc("GET /people", s.handlePeople)
	mux.HandleFunc("GET /htmx/operator-hover/{slug}", s.handleOperatorHoverCard)
	mux.HandleFunc("GET /u/{slug}", s.handleUserHome)
	mux.HandleFunc("GET /u/{slug}/section/forwarded", s.handleUserHomeSectionForwarded)
	mux.HandleFunc("GET /u/{slug}/section/recent-comments", s.handleUserHomeSectionRecentComments)
	mux.HandleFunc("GET /u/{slug}/section/inbox", s.handleUserHomeSectionInbox)
	mux.HandleFunc("GET /u/{slug}/inbox", s.handleUserInbox)
	mux.HandleFunc("GET /u/{slug}/status-message", s.handleUserStatusMessageView)
	mux.HandleFunc("GET /u/{slug}/status-message/edit", s.handleUserStatusMessageEdit)
	mux.HandleFunc("POST /u/{slug}/status-message", s.handleUpdateUserStatusMessage)
	// Gardener page + lazy older-runs section (epic ka-0000001579,
	// ka-0000001207).
	mux.HandleFunc("GET /gardener/{id}", s.retireToGarden(retireGardener, gardenGardenerTarget, s.handleGardenerDetail))
	mux.HandleFunc("GET /gardener/{id}/runs", s.handleGardenerRunsSection)
	// Invocations drawer + connected-beads graph (ka-0000007414).
	mux.HandleFunc("GET /gardener/{id}/invocations", s.handleGardenerInvocations)
	mux.HandleFunc("GET /gardener/{id}/beads", s.handleGardenerBeads)
	// Gardeners index (epic ka-0000004574).
	mux.HandleFunc("GET /gardeners", s.retireToGarden(retireGardener, gardenGardenersTarget, s.handleGardeners))
	// Threads (epic ka-0000000679, ka-0000003348): /threads lists wish heads,
	// /threads/{id} renders the fused timeline (threads.go). Precedence vs the
	// "/{id}" catch-all is locked by TestThreadsRoutesDoNotShadow.
	mux.HandleFunc("GET /threads", s.handleThreads)
	mux.HandleFunc("GET /threads/{id}", s.handleThreadDetail)
	// /threads/{id}/audit: JSON PASS/FAIL that the fused timeline is complete
	// and in order (ka-0000005790).
	mux.HandleFunc("GET /threads/{id}/audit", s.handleThreadAudit)
	// The fused timeline's ?since= polling fragment (ka-0000006034); the
	// composer writes via operatorOnly POST /b/{id}/thread-reply below.
	mux.HandleFunc("GET /threads/{id}/timeline", s.handleThreadTimeline)
	// Maps: gardeners as connected cards (epic ka-0000001644, ka-0000002415);
	// live fleet by default, ?all=1 adds disabled/closed/legacy.
	mux.HandleFunc("GET /maps", s.handleMaps)
	// Sentinel byline indexes: system/fleet/bridge writes link to
	// /fleet/<bridge> (M3 ka-0000003587, R7 §7.6).
	mux.HandleFunc("GET /fleet", s.handleFleetIndex)
	mux.HandleFunc("GET /fleet/{bridge}", s.handleFleetBridge)
	mux.HandleFunc("GET /up", s.handleUp)
	mux.HandleFunc("GET /up/{id}", s.handleUpBead)
	// Plans cockpit (epic ka-0000001409): /plans and /plans/{id}, retiring into
	// the garden's plan cockpit behind a flag (R7, garden_retire.go).
	mux.HandleFunc("GET /plans", s.retireToGarden(retirePlans, gardenPlansTarget, s.handlePlans))
	mux.HandleFunc("GET /plans/{id}", s.retireToGarden(retirePlans, gardenPlanTarget, s.handlePlanDetail))
	// Retired agent chat: /agents/chat* redirects to the Commons thread
	// (agent_chat_redirect.go).
	mux.HandleFunc("GET /agents/chat", s.handleAgentChatRedirect)
	mux.HandleFunc("GET /agents/chat/rail", s.handleAgentChatRedirect)
	mux.HandleFunc("GET /agents/chat/card/{id}", s.handleAgentChatRedirect)
	mux.HandleFunc("GET /agents/chat/conversation/{id}", s.handleAgentChatRedirect)
	mux.HandleFunc("GET /agents/chat/message/{id}", s.handleAgentChatRedirect)
	// Card-peek drawer fragment (plans_board.go).
	mux.HandleFunc("GET /plans/{id}/peek", s.handlePlanPeek)
	// Lean deck card, cheap enough for the deck to prefetch (ka-0000000492,
	// deck_card.go).
	mux.HandleFunc("GET /plans/{id}/card", s.handlePlanCard)
	// The mobile card deck (ka-0000000492, deck.go): one plan at a time,
	// most-urgent first. The webmanifest's start_url points here.
	mux.HandleFunc("GET /deck", s.handleDeck)
	// Live plan-progress SSE (ka-0000001793).
	mux.HandleFunc("GET /plans/{id}/events", s.handlePlanEvents)
	// Plan narrative fragment, read-only (S3 ka-0000002473).
	mux.HandleFunc("GET /plans/{id}/narrative", s.handlePlanNarrativeView)
	// The Share panel (PW.4 gd-tJe0GrtFDw, plan_share.go): a plan's public
	// windows. No operator wrapper: kbeads authorizes on the caller's bearer.
	mux.HandleFunc("GET /plans/{id}/share", s.handlePlanShare)
	mux.HandleFunc("GET /plans/{id}/share/preview", s.handlePlanSharePreview)
	mux.HandleFunc("POST /plans/{id}/share", s.handlePlanShareMint)
	mux.HandleFunc("POST /plans/{id}/share/{window}/revoke", s.handlePlanShareRevoke)
	// The land's Share tab (LW.5, land_share.go): JSON for the /garden project panel, and the preview page.
	mux.HandleFunc("GET /p/{project}/share", s.handleLandShare)
	mux.HandleFunc("GET /p/{project}/share/preview", s.handleLandSharePreview)
	mux.HandleFunc("POST /p/{project}/share", s.handleLandShareMint)
	mux.HandleFunc("POST /p/{project}/share/{window}/revoke", s.handleLandShareRevoke)
	// The plan-agent card (ka-0000000116): gardener levers + assistant
	// conversation. Its POST writes are operator-gated below.
	mux.HandleFunc("GET /plans/{id}/agent", s.handlePlanAgentView)
	// Taints registry (docs/adr/taints.md §9.1, gd-QXSHroFXwB), visible to
	// every viewer; the New-taint routes are adminOnly below.
	mux.HandleFunc("GET /taints", s.handleTaintsRegistry)
	// Slowest tool calls from the P2 slow_tool_calls ledger (plan
	// agent-tool-latency-attribution P5, gd-GdEpjNcXXh).
	mux.HandleFunc("GET /tool-latency", s.handleToolLatency)
	// Team page (docs/adr/taints.md §9.1 "Team page", gd-NeEllAWKFw), visible
	// to every viewer. Its membership-edit POST is below.
	mux.HandleFunc("GET /teams/{team}", s.handleTeamPage)
	mux.HandleFunc("GET /releases", s.handleReleases)
	mux.HandleFunc("GET /release/{id}", s.handleRelease)
	// Taint detail page (gd-nOnc1Q8Zcv, docs/adr/taints.md §9.1): ungated
	// here; the daemon's visibility checks are the real gates. Its writes are
	// operatorOnly below.
	mux.HandleFunc("GET /taints/{name}", s.handleTaintDetail)
	// /decisions, /decisions/{id} and /escalations retire into the garden's
	// Queues panel behind a flag (R1, garden_retire.go).
	mux.HandleFunc("GET /decisions", s.retiredToGarden(gardenQueueTarget(gardenhub.QueueDecisions), s.handleDecisions))
	// /decisions/{id}: the per-decision interaction page (ka-0000000855); a
	// bare "/ka-abc" stays on handleBeadShow. Its writes are the operator-gated
	// /b/{id}/decision/* routes below.
	mux.HandleFunc("GET /decisions/{id}", s.retiredToGarden(gardenQueueTarget(gardenhub.QueueDecisions), s.handleDecisionDetail))
	mux.HandleFunc("GET /escalations", s.retiredToGarden(gardenQueueTarget(gardenhub.QueueEscalations), s.handleEscalations))
	// Peel catalog (R4, ka-0000006592 §4.2): ranked peel-eligible dark MRs;
	// the "request peel" POST is operatorOnly below.
	mux.HandleFunc("GET /peel-catalog", s.handlePeelCatalog)
	mux.HandleFunc("GET /extern", s.handleExtern)
	mux.HandleFunc("GET /ratings", s.handleRatings)
	mux.HandleFunc("GET /telepathy", s.handleTelepathy)
	mux.HandleFunc("GET /telepathy/events", s.handleTelepathyEvents)
	mux.HandleFunc("GET /cassettes", s.handleCassettes)
	mux.HandleFunc("GET /agents", s.retireToGarden(retireAgents, gardenAgentsTarget, s.handleAgents))
	mux.HandleFunc("GET /agents/events", s.handleAgentsEvents)
	mux.HandleFunc("GET /agents/events-status", s.handleAgentsEventsStatus)
	mux.HandleFunc("GET /agents/{id}/prime", s.handleAgentPrime)
	mux.HandleFunc("GET /agents/{id}/screen", s.handleAgentScreen)
	mux.HandleFunc("GET /agents/{id}/events", s.handleAgentEvents)
	// RequireRole passes requests with no roles in context (dev bypass /
	// non-OIDC callers); OIDC users are checked by realm role.
	operatorOnly := oidcauth.RequireRole(oidcauth.RoleOperator, s.logger)
	viewerOnly := oidcauth.RequireRole(oidcauth.RoleViewer, s.logger)
	// adminOnly gates the project config editor: type:project writes need
	// global gardenio-admin (§6, ka-0000007456); the daemon enforces it too.
	adminOnly := oidcauth.RequireRole(oidcauth.RoleAdmin, s.logger)
	mux.Handle("GET /p/{project}/edit", adminOnly(http.HandlerFunc(s.handleProjectConfigForm)))
	mux.Handle("POST /p/{project}/config", jsonAction(adminOnly(http.HandlerFunc(s.handleProjectConfigSave))))
	// New-taint (docs/adr/taints.md §9.1, gd-QXSHroFXwB); the daemon also
	// requires direct OIDC RoleAdmin.
	mux.Handle("GET /taints/new", adminOnly(http.HandlerFunc(s.handleCreateTaintSheet)))
	mux.Handle("POST /taints", adminOnly(http.HandlerFunc(s.handleCreateTaintSubmit)))
	// Team membership edit (docs/adr/taints.md §9.1): viewerOnly here because
	// owner-or-admin can't be a static role; handleTeamMembershipEdit enforces it.
	mux.Handle("POST /teams/{team}/members", viewerOnly(http.HandlerFunc(s.handleTeamMembershipEdit)))
	// /me/taints/scrub (docs/adr/taints.md §9.1, PR #141) files scrub-request
	// beads, so it is operator-gated like other writes.
	mux.Handle("POST /me/taints/scrub", operatorOnly(http.HandlerFunc(s.handleMeTaintsScrubAll)))
	// Taint detail writes (gd-nOnc1Q8Zcv); the daemon's per-action checks are
	// the true gates.
	mux.Handle("POST /taints/{name}/policy", operatorOnly(http.HandlerFunc(s.handleTaintPolicyUpdate)))
	mux.Handle("POST /taints/{name}/grants", operatorOnly(http.HandlerFunc(s.handleTaintGrantCreate)))
	mux.Handle("POST /taints/{name}/grants/revoke", operatorOnly(http.HandlerFunc(s.handleTaintGrantRevoke)))
	mux.Handle("POST /taints/{name}/scrub-requests/{id}/approve", operatorOnly(http.HandlerFunc(s.handleTaintScrubApprove)))
	mux.Handle("POST /taints/{name}/scrub-requests/{id}/reject", operatorOnly(http.HandlerFunc(s.handleTaintScrubReject)))
	mux.Handle("POST /agents/{id}/input", operatorOnly(http.HandlerFunc(s.handleAgentInput)))
	mux.Handle("POST /agents/{id}/control/claim", operatorOnly(http.HandlerFunc(s.handleAgentControlClaim)))
	// "request peel" (ka-0000008213) files a peel-request bead that wakes the
	// peel gardener; it never opens an MR itself.
	mux.Handle("POST /peel-catalog/{id}/request", operatorOnly(http.HandlerFunc(s.handleRequestPeel)))
	// /image/{key...} streams S3 bytes for embedded images (ka-0000006131,
	// ka-0000007953). Not viewerOnly: same authz as the bundle pages that embed
	// them. The handler enforces the key prefix.
	mux.HandleFunc("GET /image/{key...}", s.handleImage)
	// Read-only vmsingle range-query proxy for /charts (ka-0000002747, epic
	// ka-0000003954): fixed upstream path, only the query string forwarded.
	mux.HandleFunc("GET /api/v1/query_range", s.handleMetricsQueryRange)
	// /api/v1/charts[/{id}]: read-only proxy to the kbeads chart catalog, so
	// the browser never sees the daemon bearer (E6.1 ka-0000005063, epic
	// ka-0000000596; E5 ka-0000001540).
	mux.HandleFunc("GET /api/v1/charts", s.handleChartsCatalog)
	// Fixed /dashboards chart endpoints (ka-0000004892), more specific than
	// {chart_id}. The daemon resolves identities; the viewer forwards the JSON
	// verbatim and never resolves an operator_id (plan §4 privacy allow-list).
	mux.HandleFunc("GET /api/v1/charts/uniqueness", s.handleChartsUniqueness)
	mux.HandleFunc("GET /api/v1/charts/engagement/{panel}", s.handleChartsEngagement)
	mux.HandleFunc("GET /api/v1/charts/security/{panel}", s.handleChartsSecurity)
	mux.HandleFunc("GET /api/v1/charts/{chart_id}", s.handleChartsRender)
	// /charts: the Observability Hub (type=dashboard beads as cards).
	mux.HandleFunc("GET /charts", s.handleCharts)
	// /dashboards: engagement + security panels drawn with ECharts from the
	// endpoints above (ka-0000004892, epic ka-0000001609).
	mux.HandleFunc("GET /dashboards", s.handleDashboards)
	// /grafana[/{path...}]: AVA-authenticated Grafana proxy (Option C,
	// decision ka-0000004242; epic ka-0000000746). The subtree is methodless so
	// every method and Live WebSockets pass; Grafana's org roles govern access.
	// Bare /grafana MUST be GET-only: a methodless "/grafana" conflicts with
	// "GET /{id}" in ServeMux. See grafana_proxy.go.
	mux.HandleFunc("GET /grafana", s.handleGrafanaProxy)
	mux.HandleFunc("/grafana/{path...}", s.handleGrafanaProxy)
	mux.Handle("POST /agents/{id}/keys", operatorOnly(http.HandlerFunc(s.handleAgentKeys)))
	mux.Handle("POST /agents/{id}/keys/cancel", operatorOnly(http.HandlerFunc(s.handleAgentKeysCancel)))
	mux.Handle("POST /agents/{id}/nudge", operatorOnly(http.HandlerFunc(s.handleAgentNudge)))
	mux.Handle("POST /agents/{id}/respond", operatorOnly(http.HandlerFunc(s.handleAgentRespond)))
	mux.Handle("POST /agents/{id}/compact", operatorOnly(http.HandlerFunc(s.handleAgentCompact)))
	mux.Handle("POST /agents/{id}/compact/cancel", operatorOnly(http.HandlerFunc(s.handleAgentCompactCancel)))
	mux.HandleFunc("GET /advice", s.handleAdvice)
	mux.HandleFunc("GET /advice/new", s.handleCreateAdviceSheet)
	mux.HandleFunc("POST /advice", s.handleCreateAdvice)
	mux.HandleFunc("GET /triggers", s.handleTriggers)
	// Pour a formula (ka-0000000251) via the daemon's POST
	// /v1/formulas/{id}/pour. Then the /triggers row toggle (ka-0000005886),
	// which writes `enabled`; LoadTriggers re-reads it on the next tick.
	mux.Handle("POST /formulas/{id}/pour", operatorOnly(http.HandlerFunc(s.handleFormulaPour)))
	mux.Handle("POST /triggers/{id}/toggle", operatorOnly(http.HandlerFunc(s.handleToggleTriggerEnabled)))
	// Bulk enable/disable on /triggers (ka-0000003995).
	mux.Handle("POST /triggers/bulk/enable", operatorOnly(http.HandlerFunc(s.handleTriggersBulkEnable)))
	mux.Handle("POST /triggers/bulk/disable", operatorOnly(http.HandlerFunc(s.handleTriggersBulkDisable)))
	// /schedules → /triggers (301) after the rename (ka-0000005521).
	mux.HandleFunc("GET /schedules", func(w http.ResponseWriter, r *http.Request) {
		target := s.basePath + "/triggers"
		if q := r.URL.RawQuery; q != "" {
			target += "?" + q
		}
		http.Redirect(w, r, target, http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET /costs", s.handleCosts)
	// Cost rollup fragment for /costs (decision ka-0000000742,
	// costs_rollup.go); ?part=table re-renders just the table.
	mux.HandleFunc("GET /costs/rollup", s.handleCostsRollup)
	// Molecule subtree cost rollup (ka-0000007305 §3.3); ?format=json or
	// Accept: application/json returns the raw MoleculeUsage.
	mux.HandleFunc("GET /molecule/{id}/cost", s.handleMoleculeCost)
	// Molecule execution live stream (ka-0000007739): re-renders the
	// #molecule-exec fragment on any subtree bead change.
	mux.HandleFunc("GET /molecule/{id}/events", s.handleMoleculeEvents)
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /trust", s.handleTrust)
	mux.HandleFunc("GET /dev/incident-intel/gallery", s.handleIncidentIntelGallery)
	// Live-widget fragments (allowlist in widgets_live.go).
	mux.HandleFunc("GET /widgets/{slug}/{bead}", s.handleLiveWidget)
	// Widget CSP-violation report receiver; logs only (ka-0000002043, plan
	// ka-0000002149 §3, follow-up ka-0000007136).
	mux.HandleFunc("POST /htmx/widget/csp-report", freehtmx.CSPReportHandler(s.logger, nil))
	// Free-htmx widget actions (P3, ka-0000003324): GET viewerOnly, POST
	// operatorOnly; the handler enforces the signed token + rate limit.
	mux.Handle("GET /htmx/widget/{widget}/{instance}/{action}", viewerOnly(http.HandlerFunc(s.handleHtmxWidget)))
	mux.Handle("POST /htmx/widget/{widget}/{instance}/{action}", operatorOnly(http.HandlerFunc(s.handleHtmxWidget)))
	// Incident Intelligence click-through telemetry.
	mux.HandleFunc("POST /dev/incident-intel/telemetry", s.handleIncidentIntelTelemetry)
	// Incident Intelligence action dispatcher (a write surface).
	mux.Handle("POST /actions/{id}", operatorOnly(http.HandlerFunc(s.handleAction)))
	mux.HandleFunc("GET /health/tile/open-beads", s.handleHealthTileOpenBeads)
	mux.HandleFunc("GET /health/tile/fleet", s.handleHealthTileFleet)
	mux.HandleFunc("GET /health/tile/events", s.handleHealthTileEvents)
	mux.HandleFunc("GET /health/tile/incidents", s.handleHealthTileIncidents)
	mux.HandleFunc("GET /health/tile/bridges", s.handleHealthTileBridges)
	mux.HandleFunc("GET /health/tile/decisions", s.handleHealthTileDecisions)
	mux.HandleFunc("GET /health/tile/gardeners", s.handleHealthTileGardeners)
	mux.HandleFunc("GET /health/tile/costs", s.handleHealthTileCosts)
	mux.HandleFunc("GET /health/tile/build", s.handleHealthTileBuild)
	mux.HandleFunc("GET /health/tile/escalations", s.handleHealthTileEscalations)
	mux.HandleFunc("GET /health/tile/operator-signals", s.handleHealthTileOperatorSignals)
	mux.HandleFunc("GET /views", s.handleViewsList)
	mux.HandleFunc("POST /views", s.handleViewCreate)
	mux.HandleFunc("GET /views/{id}", s.handleViewShow)
	mux.HandleFunc("POST /views/{id}/delete", s.handleViewDelete)
	mux.HandleFunc("GET /quick-spawn", s.retireToGarden(retireGardener, gardenQuickSpawnTarget, s.handleQuickSpawnSheet))
	mux.Handle("POST /quick-spawn", jsonAction(http.HandlerFunc(s.handleQuickSpawn)))
	mux.HandleFunc("GET /quick-spawn-inline", s.handleQuickSpawnInline)
	mux.HandleFunc("GET /create-gardener", s.retireToGarden(retireGardener, gardenCreateGardenerTarget, s.handleCreateGardenerSheet))
	mux.Handle("POST /create-gardener", jsonAction(operatorOnly(http.HandlerFunc(s.handleCreateGardener))))
	// Gardener lifecycle + config writes (ka-0000003713); config is saved to
	// the trigger bead's `gardener` JSON via bridge.GardenerSpec.
	mux.Handle("POST /gardener/{id}/enable", jsonAction(operatorOnly(http.HandlerFunc(s.handleGardenerEnable))))
	mux.Handle("POST /gardener/{id}/disable", jsonAction(operatorOnly(http.HandlerFunc(s.handleGardenerDisable))))
	// /gardeners row pause toggle (ka-0000007934): flips `enabled`, renders the
	// row toggle partial.
	mux.Handle("POST /gardener/{id}/pause-toggle", jsonAction(operatorOnly(http.HandlerFunc(s.handleGardenerPauseToggle))))
	// run-now and /plans/{id}/gardener/wake are operator-routed but additionally
	// need an OIDC admin (spawnGardenerRunNowCore): the run counter write-back is
	// admin/engine-only at the daemon (gd-uzCT6oXur9).
	mux.Handle("POST /gardener/{id}/run-now", jsonAction(operatorOnly(http.HandlerFunc(s.handleGardenerRunNow))))
	mux.Handle("POST /gardener/{id}/cancel-run", jsonAction(operatorOnly(http.HandlerFunc(s.handleGardenerCancelRun))))
	mux.Handle("POST /gardener/{id}/retire", jsonAction(operatorOnly(http.HandlerFunc(s.handleGardenerRetire))))
	// Firing-mode timed⇄event flip via gardenermode.Set (ka-0000006866, epic
	// ka-0000006020).
	mux.Handle("POST /gardener/{id}/mode", jsonAction(operatorOnly(http.HandlerFunc(s.handleGardenerModeToggle))))
	mux.Handle("GET /gardener/{id}/edit", operatorOnly(http.HandlerFunc(s.handleGardenerEditForm)))
	mux.Handle("POST /gardener/{id}/config", jsonAction(operatorOnly(http.HandlerFunc(s.handleGardenerConfigSave))))
	// Successor authoring (ka-0000005273 / "C3", chaining design §4): the POST
	// appends the heir to the predecessor's `chain.to`.
	mux.Handle("GET /gardener/{id}/successor", operatorOnly(http.HandlerFunc(s.handleGardenerSuccessorForm)))
	mux.Handle("POST /gardener/{id}/successor", jsonAction(operatorOnly(http.HandlerFunc(s.handleGardenerSuccessor))))
	// Create a plan epic from a one-line goal, labels set in one atomic write
	// and verified before redirect (ka-0000006081, ka-0000005773 §4, guard R2).
	mux.Handle("POST /plans", operatorOnly(http.HandlerFunc(s.handleCreatePlan)))
	// Plans master switch (ka-0000004048): pauses/resumes all plan gardeners
	// via the shared gardeners-pause gate bead (pausegate).
	mux.Handle("POST /plans/master-switch/toggle", operatorOnly(http.HandlerFunc(s.handlePlansMasterSwitchToggle)))
	// Bulk disable/enable of selected plan gardeners (ka-0000004771), via
	// setGardenerEnabledCore.
	mux.Handle("POST /plans/bulk/disable", operatorOnly(http.HandlerFunc(s.handlePlansBulkDisable)))
	mux.Handle("POST /plans/bulk/enable", operatorOnly(http.HandlerFunc(s.handlePlansBulkEnable)))
	// Bulk Mark complete via completePlanCore (ka-0000005361, ka-0000001785,
	// decision ka-0000001273).
	mux.Handle("POST /plans/bulk/complete", operatorOnly(http.HandlerFunc(s.handlePlansBulkComplete)))
	// Bulk Abandon via abandonPlanCore (ka-0000004351, epic ka-0000007470): the
	// terminal for stale draft/ready plans, which cannot reach complete.
	mux.Handle("POST /plans/bulk/abandon", operatorOnly(http.HandlerFunc(s.handlePlansBulkAbandon)))
	// Plan cockpit writes (epic ka-0000001409, S3 ka-0000002473). jsonAction
	// writes answer JSON to the garden cockpit (Accept: application/json,
	// action_json.go, respondPlanAction) and their HTML fragment otherwise
	// (Lands WB-7, step 5.9).
	mux.Handle("POST /plans/{id}/steps", jsonAction(operatorOnly(http.HandlerFunc(s.handlePlanAddStep))))
	mux.Handle("POST /plans/{id}/steps/{step}/close", jsonAction(operatorOnly(http.HandlerFunc(s.handlePlanCloseStep))))
	mux.Handle("POST /plans/{id}/steps/{step}/reopen", jsonAction(operatorOnly(http.HandlerFunc(s.handlePlanReopenStep))))
	// Dispatch a step to the plan's gardener via promptPlanAssistant, spawning
	// one if none is bound (ka-0000002484). → #plan-steps
	mux.Handle("POST /plans/{id}/steps/{step}/dispatch", jsonAction(operatorOnly(http.HandlerFunc(s.handlePlanDispatchStep))))
	// Answer/dismiss a plan's clarifying questions in place (ka-0000003157).
	// → #plan-decisions
	mux.Handle("POST /plans/{id}/decisions/{decision}/resolve", operatorOnly(http.HandlerFunc(s.handlePlanResolveDecision)))
	mux.Handle("POST /plans/{id}/decisions/{decision}/dismiss", operatorOnly(http.HandlerFunc(s.handlePlanDismissDecision)))
	mux.Handle("POST /plans/{id}/mark", jsonAction(operatorOnly(http.HandlerFunc(s.handlePlanMark))))
	// Reorder a phase in fields.phase_order (?dir=up|down, ka-0000005926).
	// → #plan-steps
	mux.Handle("POST /plans/{id}/phases/{phase}/reorder", jsonAction(operatorOnly(http.HandlerFunc(s.handlePlanPhaseReorder))))
	// In-cockpit write-through editing (ka-0000007445, D5 ka-0000003301); SSE
	// updates other viewers. Contracts in plan_edit.go.
	//   narrative summary edit (epic.description) → #plan-narrative
	mux.Handle("POST /plans/{id}/narrative", jsonAction(operatorOnly(http.HandlerFunc(s.handlePlanNarrativeEdit))))
	//   bundle-doc body edit (doc fields.content) → #plan-narrative
	mux.Handle("POST /plans/{id}/docs/{doc}", jsonAction(operatorOnly(http.HandlerFunc(s.handlePlanDocEdit))))
	//   step reword (step.title) → #plan-steps
	mux.Handle("POST /plans/{id}/steps/{step}/title", jsonAction(operatorOnly(http.HandlerFunc(s.handlePlanStepTitleEdit))))
	//   step reorder within its group (parent fields.step_order, ?dir=up|down) → #plan-steps
	mux.Handle("POST /plans/{id}/steps/{step}/reorder", jsonAction(operatorOnly(http.HandlerFunc(s.handlePlanStepReorder))))
	//   create-phase (mint epic + GROW fields.phase_order) → #plan-steps
	mux.Handle("POST /plans/{id}/phases", jsonAction(operatorOnly(http.HandlerFunc(s.handlePlanCreatePhase))))
	// Pour-a-formula form fragments (ka-0000001848): the picker, or with
	// ?formula=<id> its inputs. Operator-gated like the POST.
	mux.Handle("GET /plans/{id}/pours/new", operatorOnly(http.HandlerFunc(s.handlePlanPourForm)))
	// Pour a formula into the plan as a phase (ka-0000003984) via the daemon's
	// target_plan pour; dry_run previews (ka-0000001848). See plan_pour.go.
	mux.Handle("POST /plans/{id}/pours", jsonAction(operatorOnly(http.HandlerFunc(s.handlePlanPour))))
	// Inline critique comments on a step bead, ?step={beadID} (epic
	// ka-0000002316, decision ka-0000004596).
	mux.HandleFunc("GET /plans/{id}/comments", s.handlePlanCritiqueComments)
	mux.Handle("POST /plans/{id}/comments", operatorOnly(http.HandlerFunc(s.handleAddPlanCritiqueComment)))
	// Message the plan assistant: nudge, wake, or spawn (S2 ka-0000007556).
	mux.Handle("POST /plans/{id}/assistant/prompt", operatorOnly(http.HandlerFunc(s.handlePlanAssistantPrompt)))
	// Pause/resume + Wake the plan's gardener (ka-0000002775), via the
	// gardener_ops.go cores.
	mux.Handle("POST /plans/{id}/gardener/toggle", operatorOnly(http.HandlerFunc(s.handlePlanGardenerToggle)))
	mux.Handle("POST /plans/{id}/gardener/wake", operatorOnly(http.HandlerFunc(s.handlePlanGardenerWake)))
	// Gardener cadence via gardenermode.SetCadence (ka-0000008237): the cron in
	// timed mode, the watchdog cron in event mode (empty ⇒ event-only).
	mux.Handle("POST /plans/{id}/gardener/cadence", operatorOnly(http.HandlerFunc(s.handlePlanGardenerCadence)))
	// Bind a gardener to the plan via plangardener.Bind (ka-0000004696).
	// → #plan-agent
	mux.Handle("POST /plans/{id}/gardener/enable", operatorOnly(http.HandlerFunc(s.handlePlanGardenerEnable)))
	// /gardener/toggle for a /plans index row, re-rendering plan_card
	// (ka-0000003881, epic ka-0000004792; R6 build gate applies).
	mux.Handle("POST /plans/{id}/gardener/pause-toggle", operatorOnly(http.HandlerFunc(s.handlePlanGardenerRowToggle)))
	// Index-card Dispatch of the next open step via promptPlanAssistant
	// (ka-0000001290); re-renders the row.
	mux.Handle("POST /plans/{id}/dispatch-next", operatorOnly(http.HandlerFunc(s.handlePlanRowDispatch)))
	// fields.plan_state lifecycle (ka-0000005350, D5 ka-0000003301, R6): /state
	// flips draft↔ready; /build flips ready→building and enables the gardener.
	mux.Handle("POST /plans/{id}/state", jsonAction(operatorOnly(http.HandlerFunc(s.handlePlanState))))
	mux.Handle("POST /plans/{id}/build", jsonAction(operatorOnly(http.HandlerFunc(s.handlePlanBuild))))
	// /complete (ka-0000000616) and /abandon (ka-0000001377) close the epic,
	// disable the gardener and stamp plan_state; /reopen inverts either.
	mux.Handle("POST /plans/{id}/complete", jsonAction(operatorOnly(http.HandlerFunc(s.handlePlanComplete))))
	mux.Handle("POST /plans/{id}/abandon", jsonAction(operatorOnly(http.HandlerFunc(s.handlePlanAbandon))))
	mux.Handle("POST /plans/{id}/reopen", jsonAction(operatorOnly(http.HandlerFunc(s.handlePlanReopen))))
	mux.HandleFunc("GET /settings", s.retireToGarden(retireSettings, gardenSettingsTarget, s.handleSettings))
	mux.HandleFunc("GET /settings/edit", s.handleSettingsEdit)
	mux.HandleFunc("GET /settings/view", s.handleSettingsView)
	mux.Handle("POST /settings", jsonAction(http.HandlerFunc(s.handleUpdateSettings)))
	mux.HandleFunc("POST /settings/skin", s.handleUpdateSkin)
	mux.HandleFunc("POST /banner/migration/dismiss", s.handleMigrationBannerDismiss)
	mux.HandleFunc("POST /banner/inbox-homepage/dismiss", s.handleInboxHomepageBannerDismiss)
	mux.HandleFunc("GET /cmdk/suggest", s.handleCmdkSuggest)
	// The project hub retires into the garden's project panel behind a flag
	// (R8, garden_retire.go); its Work list stays.
	mux.HandleFunc("GET /p/{project}", s.retireToGarden(retireProject, gardenProjectTarget, s.handleProject))
	mux.HandleFunc("POST /p/{project}/bulk/status", s.handleProjectBulkStatus)
	mux.HandleFunc("POST /p/{project}/bulk/labels", s.handleProjectBulkLabels)
	mux.HandleFunc("POST /p/{project}/bulk/assignee", s.handleProjectBulkAssignee)
	// Project-hub lazy fragments (ka-0000004817, §3.2), under /b/ to avoid
	// ServeMux overlap with /p/{project}.
	mux.HandleFunc("GET /b/project/{name}/agents", s.handleProjectHubAgents)
	mux.HandleFunc("GET /b/project/{name}/gardeners", s.handleProjectHubGardeners)
	mux.HandleFunc("GET /b/project/{name}/accounts", s.handleProjectHubAccounts)
	mux.HandleFunc("GET /b/project/{name}/costs", s.handleProjectHubCosts)
	mux.HandleFunc("GET /b/project/{name}/activity", s.handleProjectHubActivity)
	mux.HandleFunc("GET /b/project/{name}/decisions", s.handleProjectHubDecisions)

	// Role hub (epic ka-0000001026, ka-0000004054, §8.5); its agents panel
	// lazy-loads from /b/role/. /roles indexes every role (ka-0000007118).
	mux.HandleFunc("GET /roles", s.handleRolesIndex)
	// /pool: per-(project,role) prewarmed-pool stats (epic ka-0000007327,
	// ka-0000001128).
	mux.HandleFunc("GET /pool", s.handlePool)
	// /old-viewer lists the LATER pages the garden has no panel for yet; the
	// garden's Menu links it (old_viewer.go, Lands step 5.13).
	mux.HandleFunc("GET /old-viewer", s.handleOldViewer)
	mux.HandleFunc("GET /role/{role}", s.handleRoleHub)
	mux.HandleFunc("GET /b/role/{role}/agents", s.handleRoleHubAgents)
	// /claude-config: read-only role×knob matrix of effective Claude settings
	// via internal/claudeconfig, the same merge that writes settings.json
	// (ka-0000006351, plan ka-0000005440).
	mux.HandleFunc("GET /claude-config", s.handleClaudeConfig)
	mux.HandleFunc("GET /claude-config/{role}", s.handleClaudeConfigDetail)
	mux.Handle("GET /assets/static/", http.StripPrefix("/assets/static/", s.static))
	// The Lands front of house: landing, legal documents, terms acceptance.
	s.registerLandsRoutes(mux)
	s.registerWindowRoutes(mux)
	s.registerLandsPurseRoute(mux)
	// The releases JSON the Release page reads (plan gd-UZKZ9H25UD step 6).
	s.registerReleasesAPIRoutes(mux)
	// BASE_PATH-aware PWA web manifest (ka-0000008014).
	mux.HandleFunc("GET /manifest.webmanifest", s.handleWebManifest)
	// Fragment routes live under /b/: ServeMux rejects /{id}/comments as
	// overlapping /p/{project}.
	mux.HandleFunc("GET /b/{id}/activity", s.handleBeadActivity)
	mux.HandleFunc("GET /b/{id}/permission-diff", s.handleAgentPermissionDiff)
	mux.HandleFunc("GET /b/{id}/ownership", s.handleBeadOwnership)
	mux.HandleFunc("GET /b/{id}/comments", s.handleBeadComments)
	mux.Handle("POST /b/{id}/comments", jsonAction(http.HandlerFunc(s.handleAddComment)))
	mux.HandleFunc("POST /b/{id}/comments/preview", s.handleCommentPreview)
	// Thread composer writes (ka-0000005643): a thread-say bead that
	// slack-bridge relays to Slack. viewerOnly, not operatorOnly
	// (ka-0000003462): same trust as a bead comment, and AVA's full-path group
	// names never match RoleOperator. Handlers 401 without an identity (A.6).
	mux.Handle("POST /b/{id}/thread-reply", jsonAction(viewerOnly(http.HandlerFunc(s.handleThreadReply))))
	mux.Handle("POST /threads/compose", viewerOnly(http.HandlerFunc(s.handleThreadCompose)))
	mux.HandleFunc("GET /b/{id}/deps", s.handleBeadDeps)
	mux.HandleFunc("GET /b/{id}/preview", s.handleBeadPreview)
	// /a/{name}/preview: the bead_preview hover card for /a/{name} mention
	// links from linkify.
	mux.HandleFunc("GET /a/{name}/preview", s.handleAgentByNamePreview)
	mux.Handle("POST /b/{id}/decision/resolve", jsonAction(http.HandlerFunc(s.handleResolveDecision)))
	mux.Handle("POST /b/{id}/escalation/approve", jsonAction(http.HandlerFunc(s.handleApproveEscalation)))
	mux.Handle("POST /b/{id}/escalation/reject", jsonAction(http.HandlerFunc(s.handleRejectEscalation)))
	mux.Handle("POST /b/{id}/escalation/route", jsonAction(http.HandlerFunc(s.handleRouteEscalation)))
	mux.Handle("POST /b/{id}/decision/dismiss", jsonAction(http.HandlerFunc(s.handleDismissDecision)))
	// Reflag: bump toward P0 + re-surface to Slack, like `gb decision reflag`
	// (ka-0000000855). 401 without an identity.
	mux.Handle("POST /b/{id}/decision/reflag", jsonAction(http.HandlerFunc(s.handleReflagDecision)))
	mux.HandleFunc("GET /b/{id}/rating", s.handleRatingWidget)
	mux.Handle("POST /b/{id}/rating", jsonAction(http.HandlerFunc(s.handleSubmitRating)))
	// Bead-page taint actions (docs/adr/taints.md §9.1, gd-nOnc1Q8Zcv),
	// route-open; the daemon's gates are authoritative.
	mux.Handle("POST /b/{id}/taints/{taint}/request-scrub", jsonAction(http.HandlerFunc(s.handleRequestScrubForBead)))
	mux.Handle("POST /b/{id}/apply-taint", jsonAction(http.HandlerFunc(s.handleApplyTaintToBead)))
	mux.Handle("POST /b/{id}/close", jsonAction(http.HandlerFunc(s.handleCloseBeadAction)))
	mux.Handle("POST /b/{id}/reopen", jsonAction(http.HandlerFunc(s.handleReopenBead)))
	mux.Handle("POST /b/{id}/claim", jsonAction(http.HandlerFunc(s.handleClaimBead)))
	mux.Handle("POST /b/{id}/release", jsonAction(http.HandlerFunc(s.handleReleaseBead)))
	mux.Handle("POST /b/{id}/defer", jsonAction(http.HandlerFunc(s.handleDeferBead)))
	mux.Handle("POST /b/{id}/inbox-worthy/toggle", jsonAction(http.HandlerFunc(s.handleToggleInboxWorthy)))
	mux.Handle("POST /b/{id}/inbox-pin/toggle", jsonAction(http.HandlerFunc(s.handleToggleInboxPinned)))
	mux.HandleFunc("GET /b/{id}/priority/edit", s.handlePriorityEdit)
	mux.HandleFunc("GET /b/{id}/priority", s.handlePriorityView)
	mux.Handle("POST /b/{id}/priority", jsonAction(http.HandlerFunc(s.handleUpdatePriority)))
	mux.HandleFunc("GET /b/{id}/assignee/edit", s.handleAssigneeEdit)
	mux.HandleFunc("GET /b/{id}/assignee", s.handleAssigneeView)
	mux.Handle("POST /b/{id}/assignee", jsonAction(http.HandlerFunc(s.handleUpdateAssignee)))
	mux.HandleFunc("GET /b/{id}/description/edit", s.handleDescriptionEdit)
	mux.HandleFunc("GET /b/{id}/description", s.handleDescriptionView)
	mux.Handle("POST /b/{id}/description", jsonAction(http.HandlerFunc(s.handleUpdateDescription)))
	mux.HandleFunc("POST /b/{id}/description/preview", s.handleDescriptionPreview)
	mux.HandleFunc("GET /b/{id}/labels/edit", s.handleLabelsEdit)
	mux.HandleFunc("GET /b/{id}/labels", s.handleLabelsView)
	mux.Handle("POST /b/{id}/labels", jsonAction(http.HandlerFunc(s.handleUpdateLabels)))
	mux.HandleFunc("GET /b/{id}/owner/edit", s.handleOwnerEdit)
	mux.HandleFunc("GET /b/{id}/owner", s.handleOwnerView)
	mux.Handle("POST /b/{id}/owner", jsonAction(http.HandlerFunc(s.handleUpdateOwner)))
	mux.Handle("POST /b/{id}/status", jsonAction(http.HandlerFunc(s.handleUpdateStatus)))
	mux.HandleFunc("GET /b/{id}/title/edit", s.handleTitleEdit)
	mux.HandleFunc("GET /b/{id}/title", s.handleTitleView)
	mux.Handle("POST /b/{id}/title", jsonAction(http.HandlerFunc(s.handleUpdateTitle)))
	mux.Handle("POST /b/{id}/heraldry", jsonAction(http.HandlerFunc(s.handleUpdateHeraldry))) // the Workbench's Change arms (heraldry 3.2)
	// Forward / unforward follower edits (ka-0000003867).
	mux.HandleFunc("GET /b/{id}/forward", s.handleForwardPanel)
	mux.Handle("POST /b/{id}/forward", jsonAction(operatorOnly(http.HandlerFunc(s.handleAddForward))))
	mux.Handle("POST /b/{id}/forward/remove", jsonAction(operatorOnly(http.HandlerFunc(s.handleRemoveForward))))
	// Inbox: bare /inbox 308s to /u/me?view=dashboard (M3 ka-0000003587); the
	// legacy view is /u/me?view=constellation. /inbox and /inbox/{id} retire
	// into the garden's Queues panel behind a flag (R2, garden_retire.go).
	toInbox := gardenQueueTarget(gardenhub.QueueInbox)
	if s.inboxLegacyRoute {
		mux.HandleFunc("GET /inbox", s.retiredToGarden(toInbox, s.handleInbox))
	} else {
		mux.HandleFunc("GET /inbox", s.retiredToGarden(toInbox, s.handleInboxFolded308))
	}
	mux.HandleFunc("GET /inbox/{id}", s.retiredToGarden(toInbox, s.handleInboxBead))
	mux.HandleFunc("GET /inbox/orphans/{scope}", s.handleInboxOrphans)
	mux.HandleFunc("GET /api/v1/inbox/{id}/subgraph", s.handleInboxSubgraphJSON)
	mux.HandleFunc("GET /operator-inbox", s.handleOperatorInboxRedirect)
	mux.HandleFunc("GET /operator-inbox/{rest...}", s.handleOperatorInboxRedirect)
	mux.Handle("POST /b/{id}/parent", jsonAction(operatorOnly(http.HandlerFunc(s.handleSetParent))))
	mux.Handle("POST /b/{id}/deps", jsonAction(operatorOnly(http.HandlerFunc(s.handleAddDep))))
	mux.Handle("POST /b/{id}/deps/remove", jsonAction(operatorOnly(http.HandlerFunc(s.handleRemoveDep))))
	// Meadow adoption (gd-6HQyB0jgmO): link a loose bead into a plan or phase
	// (gardenhub adopt.go), operator-gated here.
	if s.garden != nil {
		mux.Handle("POST /garden/bead/{id}/adopt", operatorOnly(http.HandlerFunc(s.garden.Adopt)))
		// Wake / Restart a gardener (gd-3xrmzPMA1Y): wake.go sends the `gb wake`
		// revive on the operator's bearer and re-checks the admin role.
		mux.Handle("POST /garden/gardener/{id}/wake", operatorOnly(http.HandlerFunc(s.garden.Wake)))
	}
	// type=bundle reading page (ka-0000008153), retiring into the garden's
	// bundle panel behind a flag (R6, garden_retire.go).
	mux.HandleFunc("GET /bundle/{id}", s.retireToGarden(retireBundle, gardenBundleTarget, s.handleBundleShow))
	// P5 free-htmx widget preview surface (ka-0000002154).
	mux.HandleFunc("GET /widget-types/{$}", s.handleWidgetTypesIndex)
	mux.HandleFunc("GET /widget-types/{slug}", s.handleWidgetTypeShow)
	// The preview fragment is template_source output on its own; StrictCSP
	// keeps a script in it inert if the fragment is opened top-level.
	mux.Handle("GET /htmx/widget-types/{slug}/preview", freehtmx.CSPMiddleware(http.HandlerFunc(s.handleHTMXWidgetPreview)))
	mux.Handle("POST /htmx/widget-types/{slug}/preview", operatorOnly(freehtmx.CSPMiddleware(http.HandlerFunc(s.handleHTMXWidgetPreview))))
	mux.HandleFunc("GET /htmx/widget-types/{slug}/diagnostics", s.handleHTMXWidgetDiagnostics)
	// Legacy /ui/<file>.html redirect for already-sent escalation links
	// (ka-0000005003, epic ka-0000000887); allowlist in compat_redirect.go.
	mux.HandleFunc("GET /ui/{page}", s.handleLegacyUIRedirect)
	// Pre-rename captain UI routes (gd-5O9QUlA8zr phase 4): 301 to the gardener
	// routes so already-sent links keep resolving rather than hitting GET /{id}.
	mux.HandleFunc("GET /captains", s.legacyGardenerRedirect("/gardeners"))
	mux.HandleFunc("GET /captains/{rest...}", s.legacyGardenerRedirect("/gardener"))
	mux.HandleFunc("GET /captain/{rest...}", s.legacyGardenerRedirect("/gardener"))
	mux.HandleFunc("GET /b/project/{name}/captains", s.legacyGardenerRedirect(""))
	mux.HandleFunc("GET /{id}", s.handleBeadShow)
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, data map[string]any) {
	tmpl, ok := s.pages[name]
	if !ok {
		s.logger.Error("template not found", "template", name)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	if data == nil {
		data = make(map[string]any)
	}
	data["BasePath"] = s.basePath
	if _, ok := data["ViewerEmail"]; !ok {
		data["ViewerEmail"] = ""
	}
	// VibeMode for every page (gb-ux #2, ka-0000007201); false when there is
	// no user bead. The lookup is memoized per request (userBeadCache).
	prefs, hasBead := s.viewerPrefs(r)
	data["VibeMode"] = bool(prefs.VibeMode)
	// The stored skin (skin_pref.go), painted from the first frame. SkinSave
	// needs a user bead and an unscoped viewer (a scoped viewer can't write its
	// user bead, so its pick stays in localStorage).
	data["SkinPref"] = ""
	if validSkinPref(prefs.Skin) {
		data["SkinPref"] = prefs.Skin
	}
	data["SkinSave"] = hasBead && !isScopedCtx(r.Context())
	data["Version"] = version
	data["Theme"] = s.theme
	data["AdminSessionBanner"] = adminSessionBanner(r)
	bound, err := s.bindAgentLink(tmpl, r)
	if err != nil {
		s.logger.Error("cloning template", "template", name, "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := bound.ExecuteTemplate(w, "layout", data); err != nil {
		if isClientDisconnect(err) {
			s.logger.Debug("template render aborted: client disconnect", "template", name, "error", err)
		} else {
			s.logger.Error("template render error", "template", name, "error", err)
		}
	}
}

// viewerPrefs returns the viewer's stored preferences and whether they have a
// user bead. Fails soft to zero Preferences (nil request, anonymous, no bead,
// daemon error) so a render never breaks. Memoized per request (userBeadCache).
func (s *Server) viewerPrefs(r *http.Request) (Preferences, bool) {
	if r == nil {
		return Preferences{}, false
	}
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		return Preferences{}, false
	}
	bead, err := s.findUserBeadByEmail(r.Context(), email)
	if err != nil {
		s.logger.Debug("render: resolving viewer preferences", "email", email, "error", err)
		return Preferences{}, false
	}
	if bead == nil {
		return Preferences{}, false
	}
	return parsePreferences(bead.Fields), true
}

// bindAgentLink clones tmpl and rebinds agentLink / agentLiveIcon to a
// per-request agentResolver (emitting href="/{ka-id}" links). The clone is
// required: template.Funcs must not be re-bound on a shared template. Reuses
// a resolver prewarmed via withAgentResolver, else creates one.
func (s *Server) bindAgentLink(tmpl *template.Template, r *http.Request) (*template.Template, error) {
	cloned, err := tmpl.Clone()
	if err != nil {
		return nil, err
	}
	var ctx context.Context
	if r != nil {
		ctx = r.Context()
	} else {
		// No request means no human: the nil-request callers are the agents-page
		// SSE row renderers, which run off the shared event stream.
		ctx = beadsapi.WithSystemRead(context.Background())
	}
	resolver := agentResolverFromContext(ctx)
	if resolver == nil {
		resolver = newAgentResolver(ctx, s.daemon)
	}
	cloned.Funcs(template.FuncMap{
		"agentLink": func(name string) template.HTML {
			return renderAgentLink(s.basePath, name, resolver)
		},
		"agentLiveIcon": func(name string) template.HTML {
			return renderAgentLiveIcon(s.basePath, name, resolver)
		},
	})
	return cloned, nil
}

// projectBeadView is the template view model for one open bead on the
// per-project page.
type projectBeadView struct {
	ID            string
	Title         string
	ShortTitle    string // shown in place of Title when set (gd-V5nGseRS5f)
	Heraldry      string // the bead's arms, canonical; "" when none (arms.go)
	Type          string
	Status        string
	Priority      int
	ShowPriority  bool
	Assignee      string
	DescPreview   string
	Ago           string
	UpdatedAtRFC  string
	OperatorEmail string
	CanClose      bool
	Project       string
	// IsRichReport marks `type=rich-report` rows for the "View report →" CTA.
	IsRichReport bool
	// HasReport drives the 📄 badge: daemon has_report (R1, ka-0000007923; R5
	// spec gardenio/docs/research/sequencer-report-decision-visibility.md).
	HasReport bool
	// BlockedOnDecision drives the ⏸ badge (R2, ka-0000001065).
	BlockedOnDecision bool
	// BlockedOnDecisionBeadID is the open decision's ka-id; the ⏸ badge
	// links to /<id> when set, else falls back to /decisions.
	BlockedOnDecisionBeadID string
	// OperatorChipCompactHTML is the initials-only operator chip
	// (ka-0000000305); empty for scheduler / empty / self / suppressed kinds.
	OperatorChipCompactHTML template.HTML
}

// selectOption is one <option> of the /p/<project> sort and status dropdowns.
type selectOption struct {
	Label    string
	Value    string
	Selected bool
}

// projectTypeChips defines the canonical chip strip on /p/<project>, in
// rendering order. Mirrors iOS BeadsTab with Agents+Advice in place of Epics.
var projectTypeChips = []struct {
	Label string
	Value string
}{
	{"Issues", "issues"},
	{"All", "all"},
	{"Tasks", "task"},
	{"Features", "feature"},
	{"Bugs", "bug"},
	{"Triggers", "schedule"},
	{"Trigger beads", "trigger"},
	{"Decisions", "decision"},
	{"Agents", "agent"},
	{"Advice", "advice"},
}

// projectSortOptions is the /p/<project> sort dropdown. Values are
// ListBeadsQuery.Sort ("-column" = descending); "priority" is the default.
var projectSortOptions = []struct {
	Label string
	Value string
}{
	{"Priority", "priority"},
	{"Recently updated", "-updated_at"},
	{"Recently created", "-created_at"},
	{"Oldest first", "created_at"},
	{"Title (A→Z)", "title"},
}

// projectStatusOptions defines the user-visible status dropdown on
// /p/<project>. Values are passed verbatim to ListBeadsQuery.Statuses
// (comma-separated for multi-select); "all" expands to no status filter.
var projectStatusOptions = []struct {
	Label string
	Value string
}{
	{"Open", "open"},
	{"In progress", "in_progress"},
	{"Open + in progress", "open,in_progress"},
	{"Closed", "closed"},
	{"All", "all"},
}

// resolveProjectSort normalizes the ?sort= query into one of the sort option
// values. Empty or unrecognized input falls back to "priority" (the default).
func resolveProjectSort(q string) string {
	q = strings.TrimSpace(q)
	if q == "" {
		return "priority"
	}
	for _, o := range projectSortOptions {
		if o.Value == q {
			return q
		}
	}
	return "priority"
}

// projectStatusSingles are the single statuses the dropdown offers; a
// comma-separated ?status= may combine any of them (open,closed), which is how
// a filter deep link carries a multi-select the dropdown has no preset for.
var projectStatusSingles = map[string]bool{"open": true, "in_progress": true, "closed": true}

// resolveProjectStatus normalizes the ?status= query into one of the status
// option values, or a comma-separated combination of the single statuses
// (trimmed, deduplicated, in the order given). Empty or unrecognized input,
// including a list with any unknown member, falls back to "open".
func resolveProjectStatus(q string) string {
	q = strings.TrimSpace(q)
	if q == "" {
		return "open"
	}
	for _, o := range projectStatusOptions {
		if o.Value == q {
			return q
		}
	}
	parts := parseCommaSeparated(q)
	seen := make(map[string]bool, len(parts))
	statuses := make([]string, 0, len(parts))
	for _, p := range parts {
		if !projectStatusSingles[p] {
			return "open"
		}
		if !seen[p] {
			seen[p] = true
			statuses = append(statuses, p)
		}
	}
	if len(statuses) == 0 {
		return "open"
	}
	return strings.Join(statuses, ",")
}

// projectStatusLabel names a resolved status for the dropdown: the preset's
// label, or the singles' labels joined with " + " for a combination.
func projectStatusLabel(status string) string {
	labels := make(map[string]string, len(projectStatusOptions))
	for _, o := range projectStatusOptions {
		if o.Value == status {
			return o.Label
		}
		labels[o.Value] = o.Label
	}
	parts := parseCommaSeparated(status)
	for i, p := range parts {
		parts[i] = labels[p]
	}
	return strings.Join(parts, " + ")
}

// projectFilterStatuses maps a status option value to the daemon Statuses[]
// filter: "all" → nil (no status filter); anything else splits on comma.
func projectFilterStatuses(status string) []string {
	if status == "all" {
		return nil
	}
	return parseCommaSeparated(status)
}

// issueMacroTypes composes the "Issues" filter (as iOS BeadsTab.issueTypes),
// plus rich-report so its CTA shows by default (ka-0000005611).
var issueMacroTypes = []string{"task", "feature", "bug", "epic", "chore", "rich-report"}

// resolveProjectTypeFilter normalizes the ?type= query into one of the chip
// values. Empty or unrecognized input falls back to "issues" (the default).
func resolveProjectTypeFilter(q string) string {
	q = strings.TrimSpace(q)
	if q == "" {
		return "issues"
	}
	for _, c := range projectTypeChips {
		if c.Value == q {
			return q
		}
	}
	return "issues"
}

// projectFilterTypes maps a chip value to the daemon Types[] filter:
//   - "all"    → nil (no type filter)
//   - "issues" → the issueMacroTypes slice
//   - any other valid chip value → that single type
func projectFilterTypes(filter string) []string {
	switch filter {
	case "all":
		return nil
	case "issues":
		out := make([]string, len(issueMacroTypes))
		copy(out, issueMacroTypes)
		return out
	default:
		return []string{filter}
	}
}

// projectListParamsPresent reports whether any list-shaping param
// (type/status/sort/label/q) is non-empty, so old filter deep links still land
// on the Work list rather than the hub Overview (ka-0000001620).
func projectListParamsPresent(q url.Values) bool {
	for _, k := range []string{"type", "status", "sort", "label", "q"} {
		if strings.TrimSpace(q.Get(k)) != "" {
			return true
		}
	}
	return false
}

// projectHubRequested reports whether a /p/{project} query asks for the hub
// Overview rather than the Work list: no ?view=list and either ?view=overview
// or none of the list's filters (design doc §2, ka-0000001620).
func projectHubRequested(q url.Values) bool {
	return q.Get("view") != "list" && (q.Get("view") == "overview" || !projectListParamsPresent(q))
}

// buildProjectChipHref returns a /p/<project> type-chip href that keeps the
// current non-empty status/label/q/sort filters.
func buildProjectChipHref(basePath, project, typeValue string, current url.Values) string {
	out := url.Values{}
	out.Set("type", typeValue)
	if v := strings.TrimSpace(current.Get("status")); v != "" {
		out.Set("status", v)
	}
	if v := strings.TrimSpace(current.Get("label")); v != "" {
		out.Set("label", v)
	}
	if v := strings.TrimSpace(current.Get("q")); v != "" {
		out.Set("q", v)
	}
	if v := strings.TrimSpace(current.Get("sort")); v != "" {
		out.Set("sort", v)
	}
	return basePath + "/p/" + url.PathEscape(project) + "?" + out.Encode()
}

// rfcTime renders t as RFC3339Nano in UTC, or "" for the zero value. It is
// the rows' `data-last-activity-ts`, compared lexicographically by
// unread_listing.js against `bv:lastSeen:v1`; "" never shows an unread dot.
func rfcTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(commentTimeFormat)
}

// formatAgo renders a relative-time label like "3m", "5h", "2d", "1w" for the
// project bead row's "updated" column. Mirrors iOS BeadsTab compact format.
func formatAgo(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := max(time.Since(t), 0)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + "m"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + "h"
	case d < 7*24*time.Hour:
		return strconv.Itoa(int(d/(24*time.Hour))) + "d"
	default:
		return strconv.Itoa(int(d/(7*24*time.Hour))) + "w"
	}
}

// indexStatsView is the /index banner view model, one packed grid
// (index.html, ka-0000002715). A tile whose *OK flag is false renders an
// em-dash.
type indexStatsView struct {
	TotalBeads      int
	TotalOpen       int
	TotalInProgress int
	TotalBlocked    int
	TotalClosed     int
	TotalDeferred   int
	TotalTypes      int
	UniqueFields    int
	Decisions       int // open type=decision beads
	Agents          int // in_progress type=agent beads
	// Ready is the count of open beads with no open dependencies, the depth of
	// the "Hot ready" queue (ka-0000004033).
	Ready int
	// EscalationsPending is the count of the viewer's pending escalations
	// (ka-0000006022).
	EscalationsPending int
	// P0Open is the count of open priority-0 issues, capped at
	// indexBannerP0ScanLimit; same derivation as /health (ka-0000006022).
	P0Open int
	// Per-sub-call OK flags: one failing daemon call blanks only its own
	// tiles (ka-0000002676).
	StatsOK       bool // GetStats: TotalBeads/Open/InProgress/Blocked/Closed/Deferred
	ConfigsOK     bool // ListConfigs: TotalTypes/UniqueFields
	DecisionsOK   bool // Decisions
	ReadyOK       bool // Ready
	AgentsOK      bool // Agents
	EscalationsOK bool // EscalationsPending
	P0OK          bool // P0Open
}

// computeIndexStats fetches the /index banner numbers, one daemon call after
// another. Each call is best-effort and sets only its own OK flag on success
// (ka-0000002676).
func (s *Server) computeIndexStats(ctx context.Context) indexStatsView {
	view := indexStatsView{}
	stats, err := s.daemon.GetStats(ctx)
	if err != nil {
		s.logger.Warn("getting stats for index banner", "error", err)
	} else if stats != nil {
		view.TotalBeads = stats.Total()
		view.TotalOpen = stats.TotalOpen
		view.TotalInProgress = stats.TotalInProgress
		view.TotalBlocked = stats.TotalBlocked
		view.TotalClosed = stats.TotalClosed
		view.TotalDeferred = stats.TotalDeferred
		view.StatsOK = true
	}
	configs, err := s.daemon.ListConfigs(ctx, "type")
	if err != nil {
		s.logger.Warn("listing type configs for index banner", "error", err)
	} else {
		view.TotalTypes = len(configs)
		view.UniqueFields = countUniqueFields(configs)
		view.ConfigsOK = true
	}
	// Errors below leave the count zero and its OK flag false.
	if decisions, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"decision"},
		Statuses: []string{"open"},
		Limit:    1,
	}); err != nil {
		s.logger.Warn("listing decisions for index banner", "error", err)
	} else if decisions != nil {
		view.Decisions = decisions.Total
		view.DecisionsOK = true
	}
	// Ready: the same filter as the "Hot ready" rail; Limit=1, only Total is used.
	if ready, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Statuses:         []string{"open"},
		NoOpenDeps:       true,
		ExcludeScheduled: true,
		Limit:            1,
	}); err != nil {
		s.logger.Warn("listing ready beads for index banner", "error", err)
	} else if ready != nil {
		view.Ready = ready.Total
		view.ReadyOK = true
	}
	// "agents live" matches /agents: status open|in_progress AND an alive
	// agent_state; status alone undercounts (ka-0000000513).
	if agents, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"agent"},
		Statuses: []string{"open", "in_progress"},
		Limit:    indexBannerAgentScanLimit,
	}); err != nil {
		s.logger.Warn("listing agents for index banner", "error", err)
	} else if agents != nil {
		alive := 0
		for _, b := range agents.Beads {
			if b == nil || b.Fields == nil {
				continue
			}
			if isAgentStateIn(b.Fields["agent_state"], agentStateAlive) {
				alive++
			}
		}
		view.Agents = alive
		view.AgentsOK = true
	}
	// Escalations: empty status = the daemon's default open+in_progress set,
	// as in buildEscalationsTileView.
	if escalations, err := s.daemon.ListEscalations(ctx, "", indexBannerEscalationsScanLimit); err != nil {
		s.logger.Warn("listing escalations for index banner", "error", err)
	} else {
		owner, _ := ownerEscalations(escalations, s.viewerLaneRefs(ctx, oidcauth.EmailFromContext(ctx))) // the viewer's (gd-bmOiteQi36, escalation-routing D9)
		view.EscalationsPending = len(owner)
		view.EscalationsOK = true
	}
	// P0: Sort=priority puts P0s first; count until the first non-zero
	// priority. Capped at indexBannerP0ScanLimit.
	if p0res, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Kinds:    []string{"issue"},
		Statuses: []string{"open"},
		Sort:     "priority",
		Limit:    indexBannerP0ScanLimit,
	}); err != nil {
		s.logger.Warn("listing P0 open beads for index banner", "error", err)
	} else if p0res != nil {
		for _, b := range p0res.Beads {
			if b == nil {
				continue
			}
			if b.Priority != 0 {
				break
			}
			view.P0Open++
		}
		view.P0OK = true
	}
	return view
}

// indexBannerAgentScanLimit caps the active-agent fetch behind "agents live",
// which is classified client-side (the daemon has no agent_state filter).
const indexBannerAgentScanLimit = 500

// indexBannerEscalationsScanLimit caps the escalations fetch for the banner;
// matches escalationsListLimit on /escalations.
const indexBannerEscalationsScanLimit = 200

// indexBannerP0ScanLimit caps the P0 scan; mirrors
// healthOpenBeadsPriorityScanLimit so /index and /health agree.
const indexBannerP0ScanLimit = 200

// indexProjectView is one /index project card with status counts
// (ka-0000002715, ka-0000007960).
type indexProjectView struct {
	Name       string
	Open       int
	InProgress int
	Blocked    int
	Deferred   int
	Decisions  int
	// Stale is the count of in_progress beads untouched for 24h, as in
	// projectSummary / gb news (§8, ka-0000002729).
	Stale int
	// HasConfig is true when the project has a type:project config bead. It
	// gates the validity badge, since a label-only project's zero report is OK().
	HasConfig bool
	// Validity drives the card's green/yellow/red config badge (§5.3); only
	// meaningful when HasConfig. Unlike the hub, includes the account check.
	Validity projectValidityReport
}

// indexRecentItem is one /index "Recent activity" row (see computeIndexRecent).
type indexRecentItem struct {
	ID      string
	Title   string
	Type    string
	Status  string
	Project string
	Ago     string
	// Operator identity for /feed "mine" scoping of backfill cards
	// (ka-0000003403).
	OperatedBy      string
	OperatedByEmail string
}

// indexStaleThreshold is how long an in_progress bead can go untouched before
// the index card flags it as stale. Matches projectSummary / gb news (24h).
const indexStaleThreshold = 24 * time.Hour

// computeIndexProjects returns the /index project rows sorted by name, each
// count and the validity report fetched in parallel. projects maps name →
// config (BeadID drives validity); knownAccounts are live type:account names
// for the account check (nil skips it); now anchors the staleness cutoff.
// Each fetch is best-effort: an error leaves its field zero.
func (s *Server) computeIndexProjects(ctx context.Context, projects map[string]beadsapi.ProjectInfo, knownAccounts map[string]struct{}, now time.Time) []indexProjectView {
	if len(projects) == 0 {
		return nil
	}
	names := make([]string, 0, len(projects))
	for name := range projects {
		if name != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	staleCutoff := now.Add(-indexStaleThreshold)
	out := make([]indexProjectView, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		out[i] = indexProjectView{Name: name}
		wg.Add(1)
		go func(i int, name string, info beadsapi.ProjectInfo) {
			defer wg.Done()
			projectLabel := []string{"project:" + name}
			count := func(target *int, q beadsapi.ListBeadsQuery, kind string) {
				q.Labels = projectLabel
				q.Limit = 1
				q.ExcludeScheduled = true
				// Kinds:["issue"], as `kd list`: otherwise sessions, events,
				// agents etc. inflate Total (ka-0000002522, ka-0000006881).
				q.Kinds = []string{"issue"}
				res, err := s.daemon.ListBeadsFiltered(ctx, q)
				if err != nil {
					s.logger.Warn("counting beads for project", "project", name, "kind", kind, "error", err)
					return
				}
				if res != nil {
					*target = res.Total
				}
			}
			var inner sync.WaitGroup
			inner.Add(7)
			go func() {
				defer inner.Done()
				count(&out[i].Open, beadsapi.ListBeadsQuery{Statuses: []string{"open"}}, "open")
			}()
			go func() {
				defer inner.Done()
				count(&out[i].InProgress, beadsapi.ListBeadsQuery{Statuses: []string{"in_progress"}}, "in_progress")
			}()
			go func() {
				defer inner.Done()
				count(&out[i].Blocked, beadsapi.ListBeadsQuery{Statuses: []string{"blocked"}}, "blocked")
			}()
			go func() {
				defer inner.Done()
				count(&out[i].Deferred, beadsapi.ListBeadsQuery{Statuses: []string{"deferred"}}, "deferred")
			}()
			go func() {
				defer inner.Done()
				count(&out[i].Decisions, beadsapi.ListBeadsQuery{Types: []string{"decision"}, Statuses: []string{"open"}}, "decisions")
			}()
			go func() {
				defer inner.Done()
				// Stale in_progress: untouched for >24h. UpdatedBefore is the
				// cheap server-side cutoff (ka-0000002729) — no row scan.
				count(&out[i].Stale, beadsapi.ListBeadsQuery{Statuses: []string{"in_progress"}, UpdatedBefore: staleCutoff}, "stale")
			}()
			out[i].HasConfig = info.BeadID != ""
			go func() {
				defer inner.Done()
				out[i].Validity = s.indexProjectValidity(ctx, name, info.BeadID, knownAccounts)
			}()
			inner.Wait()
		}(i, name, projects[name])
	}
	wg.Wait()
	return out
}

// indexProjectValidity runs buildProjectValidityReport over the config bead's
// RAW fields (a parsed ProjectInfo has already dropped the malformed fields we
// want to flag), with knownAccounts. An empty beadID or daemon error yields
// the zero report.
func (s *Server) indexProjectValidity(ctx context.Context, name, beadID string, knownAccounts map[string]struct{}) projectValidityReport {
	if beadID == "" {
		return projectValidityReport{}
	}
	bead, err := s.daemon.GetBead(ctx, beadID)
	if err != nil || bead == nil {
		s.logger.Warn("index project validity: fetching config bead", "project", name, "bead", beadID, "error", err)
		return projectValidityReport{}
	}
	return buildProjectValidityReport(bead.Fields, knownAccounts)
}

// indexAccountSet loads the live type:account bead names for the validity
// account check. A daemon error returns nil, which skips that check rather
// than flagging every account as dangling.
func (s *Server) indexAccountSet(ctx context.Context) map[string]struct{} {
	res, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"account"},
		Statuses: []string{"open", "in_progress"},
		Limit:    indexAccountSetLimit,
	})
	if err != nil {
		s.logger.Warn("index: listing account beads for validity check", "error", err)
		return nil
	}
	if res == nil {
		return nil
	}
	set := make(map[string]struct{}, len(res.Beads))
	for _, b := range res.Beads {
		if b != nil && b.Title != "" {
			set[b.Title] = struct{}{}
		}
	}
	return set
}

// indexAccountSetLimit caps the account-bead scan backing the index validity
// check. Generous: account beads number in the low tens.
const indexAccountSetLimit = 500

// recentActivityNonActionableTypes are infrastructure bead types hidden from
// the /index "Recent activity" feed.
var recentActivityNonActionableTypes = map[string]struct{}{
	"agent":    {},
	"decision": {},
	"schedule": {},
	"advice":   {},
	"rating":   {},
	"generic":  {},
	"mail":     {},
	"config":   {},
}

// computeIndexRecent returns up to limit (default 20) in_progress beads by
// -updated_at, minus recentActivityNonActionableTypes. nil on daemon error.
func (s *Server) computeIndexRecent(ctx context.Context, limit int) []indexRecentItem {
	if limit <= 0 {
		limit = 20
	}
	// Over-fetch so type filtering doesn't starve the panel when meta-type
	// beads dominate recent updates.
	res, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Statuses:         []string{"in_progress"},
		Sort:             "-updated_at",
		Limit:            limit * 3,
		ExcludeScheduled: true,
	})
	if err != nil {
		s.logger.Warn("listing recent beads for index", "error", err)
		return nil
	}
	if res == nil {
		return nil
	}
	out := make([]indexRecentItem, 0, limit)
	for _, b := range res.Beads {
		if b == nil {
			continue
		}
		if _, skip := recentActivityNonActionableTypes[b.Type]; skip {
			continue
		}
		out = append(out, indexRecentItem{
			ID:              b.ID,
			Title:           b.Title,
			Type:            b.Type,
			Status:          b.Status,
			Project:         projectFromLabels(b.Labels),
			Ago:             formatAgo(b.UpdatedAt),
			OperatedBy:      b.OperatedBy,
			OperatedByEmail: b.OperatedByEmail,
		})
		if len(out) >= limit {
			break
		}
	}
	return out
}

// countUniqueFields returns the number of distinct field names across type
// config entries, silently skipping malformed ones.
func countUniqueFields(configs []beadsapi.ConfigEntry) int {
	seen := make(map[string]struct{})
	for _, c := range configs {
		if len(c.Value) == 0 {
			continue
		}
		var tc struct {
			Fields []struct {
				Name string `json:"name"`
			} `json:"fields"`
		}
		if err := json.Unmarshal(c.Value, &tc); err != nil {
			continue
		}
		for _, f := range tc.Fields {
			if f.Name != "" {
				seen[f.Name] = struct{}{}
			}
		}
	}
	return len(seen)
}

// handleIndex renders /index: banner stats, project cards, the 20 most
// recently updated in_progress beads, and the rail (ka-0000002715). The
// fetches run in parallel and degrade per panel; only a project-list error
// is shown on the page.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	email := oidcauth.EmailFromContext(r.Context())
	data := map[string]any{
		"Projects":    []indexProjectView{},
		"Recent":      []indexRecentItem{},
		"ViewerEmail": email,
		"InboxCount":  s.inboxCountForViewer(r.Context(), email),
	}

	ctx := r.Context()

	var (
		wg          sync.WaitGroup
		stats       indexStatsView
		recent      []indexRecentItem
		rail        indexRail
		projectsMap map[string]beadsapi.ProjectInfo
		projectsErr error
	)
	wg.Add(4)
	go func() {
		defer wg.Done()
		stats = s.computeIndexStats(ctx)
	}()
	go func() {
		defer wg.Done()
		recent = s.computeIndexRecent(ctx, 20)
	}()
	go func() {
		defer wg.Done()
		rail = s.computeIndexRail(ctx, email)
	}()
	go func() {
		defer wg.Done()
		projectsMap, projectsErr = s.daemon.ListProjectBeads(ctx)
	}()
	wg.Wait()

	data["Stats"] = stats
	data["Recent"] = recent
	data["Rail"] = rail

	if projectsErr != nil {
		s.logger.Error("listing projects", "error", projectsErr)
		data["Error"] = "Failed to load project list."
		s.render(w, r, "index.html", data)
		return
	}

	// One account set for every card's validity check (nil on error).
	knownAccounts := s.indexAccountSet(ctx)
	data["Projects"] = s.computeIndexProjects(ctx, projectsMap, knownAccounts, time.Now())
	data["LatestReleases"] = s.latestReleaseTiles(ctx, projectsMap, time.Now())
	s.render(w, r, "index.html", data)
}

// handleProject renders the /p/<name> Work list of beads labeled
// project:<name>, capped at projectListLimit, or the hub Overview (see
// projectHubRequested). ?type= drives the chip strip (projectFilterTypes).
func (s *Server) handleProject(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	if project == "" {
		http.Error(w, "Missing project", http.StatusBadRequest)
		return
	}

	q := r.URL.Query()

	// The hub Overview is the default; the list is the Work tab (§2,
	// ka-0000001620).
	if projectHubRequested(q) {
		s.handleProjectHub(w, r)
		return
	}

	filter := resolveProjectTypeFilter(q.Get("type"))
	status := resolveProjectStatus(q.Get("status"))
	sortOpt := resolveProjectSort(q.Get("sort"))
	extraLabels := parseCommaSeparated(q.Get("label"))
	search := strings.TrimSpace(q.Get("q"))

	// Summary rail + chip counts, best-effort. Computed before the main query
	// so tests' listLastQ sees the primary list call.
	summary, sumErr := buildProjectSummary(r.Context(), s.daemon, project, time.Now())
	if sumErr != nil {
		s.logger.Warn("building project summary", "project", project, "error", sumErr)
		summary = nil
	}

	chips := make([]filterChip, len(projectTypeChips))
	for i, c := range projectTypeChips {
		count := 0
		if summary != nil {
			count = summary.TypeCounts[c.Value]
		}
		chips[i] = filterChip{
			Label:     c.Label,
			Value:     c.Value,
			Href:      buildProjectChipHref(s.basePath, project, c.Value, q),
			Active:    c.Value == filter,
			Count:     count,
			ShowCount: summary != nil,
		}
	}

	sortOptions := make([]selectOption, len(projectSortOptions))
	for i, o := range projectSortOptions {
		sortOptions[i] = selectOption{Label: o.Label, Value: o.Value, Selected: o.Value == sortOpt}
	}
	statusOptions := make([]selectOption, 0, len(projectStatusOptions)+1)
	preset := false
	for _, o := range projectStatusOptions {
		statusOptions = append(statusOptions, selectOption{Label: o.Label, Value: o.Value, Selected: o.Value == status})
		preset = preset || o.Value == status
	}
	if !preset {
		// A deep-linked combination with no preset: show it as the selected
		// option rather than letting the browser fall back to the first one.
		statusOptions = append(statusOptions, selectOption{Label: projectStatusLabel(status), Value: status, Selected: true})
	}

	// The hub tab bar, with Work ("list") active (§2).
	tabs := buildProjectHubTabs(s.basePath, project, "list", viewerCanEditProjectConfig(r), false)

	data := map[string]any{
		"Project":       project,
		"Limit":         projectListLimit,
		"Beads":         []projectBeadView{},
		"TypeFilter":    filter,
		"StatusFilter":  status,
		"SortFilter":    sortOpt,
		"LabelFilter":   strings.Join(extraLabels, ","),
		"Search":        search,
		"Chips":         chips,
		"SortOptions":   sortOptions,
		"StatusOptions": statusOptions,
		"Summary":       summary,
		"Tabs":          tabs,
	}

	labels := append([]string{"project:" + project}, extraLabels...)
	// Opt in to has_report / blocked_on_decision for the 📄 / ⏸ badges;
	// otherwise the daemon returns placeholders.
	res, err := s.daemon.ListBeadsFiltered(r.Context(), beadsapi.ListBeadsQuery{
		Statuses:              projectFilterStatuses(status),
		Types:                 projectFilterTypes(filter),
		Labels:                labels,
		Search:                search,
		Sort:                  sortOpt,
		Limit:                 projectListLimit,
		WithHasReport:         true,
		WithBlockedOnDecision: true,
	})
	if err != nil {
		s.logger.Error("listing project beads", "project", project, "error", err)
		data["Error"] = "Failed to load beads for project."
		s.render(w, r, "project.html", data)
		return
	}

	email := oidcauth.EmailFromContext(r.Context())
	// Slugs before chip render so rows link to /u/{slug} (M2 §6, ka-0000007787).
	s.populateOperatedBySlugs(r.Context(), res.Beads)
	views := make([]projectBeadView, 0, len(res.Beads))
	for _, b := range res.Beads {
		if b == nil {
			continue
		}
		views = append(views, projectViewFromBead(b, email, project, s.basePath))
	}
	data["Beads"] = views
	data["Truncated"] = res.Total > len(views)
	r = s.prewarmAgentLinksFromBeads(r, res.Beads)
	s.render(w, r, "project.html", data)
}

// handleBeadShow renders a single bead at /{id}.
func (s *Server) handleBeadShow(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "Missing bead id", http.StatusBadRequest)
		return
	}

	bead, err := s.daemon.GetBead(r.Context(), id)
	if err != nil {
		if beadMissing(err) {
			http.Error(w, "Bead not found", http.StatusNotFound)
			return
		}
		s.logger.Error("getting bead", "id", id, "error", err)
		http.Error(w, "Failed to get bead", http.StatusInternalServerError)
		return
	}

	// Bundle beads redirect to /bundle/<id>; `?bead=1` shows the raw bead page.
	if bead.Type == "bundle" && r.URL.Query().Get("bead") != "1" {
		http.Redirect(w, r, s.basePath+"/bundle/"+id, http.StatusFound)
		return
	}

	// Plan epics redirect to /plans/<id> (ka-0000001056); `?bead=1` shows the
	// raw bead page, so the cockpit's back-link doesn't loop.
	if bead.Type == "epic" && hasLabel(bead, planLabel) && r.URL.Query().Get("bead") != "1" {
		http.Redirect(w, r, s.basePath+"/plans/"+id, http.StatusFound)
		return
	}

	// The bead page retires into the garden's bead panel behind a flag (R3,
	// garden_retire.go), checked after the read so unknown ids still 404. An
	// agent's page retires into the Glass instead (G-R1).
	gardenHref := ""
	step, target := retireBead, gardenBeadTarget
	if bead.Type == "agent" {
		step, target = retireGlass, gardenGlassTarget
	}
	if s.gardenRedirects(r, step) {
		http.Redirect(w, r, target(s, r), http.StatusFound)
		return
	}
	gardenHref = s.gardenBanner(r, step, target)

	// Operator slug before chip render, for /u/{slug} links (M2 §6,
	// ka-0000007787). Best-effort.
	s.populateOperatedBySlug(r.Context(), bead)

	// Best-effort: dependencies and comments. Daemon errors degrade gracefully.
	deps, err := s.daemon.GetDependencies(r.Context(), id)
	if err != nil {
		s.logger.Warn("getting dependencies", "id", id, "error", err)
		deps = nil
	}
	comments, err := s.daemon.GetComments(r.Context(), id)
	if err != nil {
		s.logger.Warn("getting comments", "id", id, "error", err)
		comments = nil
	}
	// Activity feed, bounded by activityFetchLimit: trigger beads can carry
	// >30 MB of events and OOMKill the pod (ka-0000002256).
	events, err := s.daemon.GetRecentEvents(r.Context(), id, activityFetchLimit)
	if err != nil {
		s.logger.Warn("getting events", "id", id, "error", err)
		events = nil
	}
	activity := buildActivityRows(events)

	// Text-mention edges (epic ka-0000002334), best-effort (older daemons lack
	// the endpoints).
	mentionedBy, err := s.daemon.GetMentionedBy(r.Context(), id, 50)
	if err != nil {
		s.logger.Debug("getting mentioned-by", "id", id, "error", err)
		mentionedBy = nil
	}
	mentions, err := s.daemon.GetMentions(r.Context(), id, 50)
	if err != nil {
		s.logger.Debug("getting mentions", "id", id, "error", err)
		mentions = nil
	}

	// Per-project linkifier opts (see linkifyOptsForBead, ka-0000004231).
	linkOpts := s.linkifyOptsForBead(r.Context(), bead)

	var descHTML template.HTML
	if bead.Description != "" {
		var buf bytes.Buffer
		if err := s.md.Convert([]byte(bead.Description), &buf); err != nil {
			s.logger.Warn("rendering markdown", "id", id, "error", err)
		} else {
			descHTML = LinkifyDescriptionHTMLWithOpts(buf.String(), s.basePath, typedMentionsByID(mentions, "description"), linkOpts)
		}
	}

	// Similar beads; any error hides the panel.
	similarResp, err := s.daemon.FindSimilar(r.Context(), id, 8)
	if err != nil {
		s.logger.Debug("finding similar beads", "id", id, "error", err)
		similarResp = nil
	}

	// Reverse dependencies for the constellation, best-effort.
	revDeps, err := s.daemon.GetReverseDependencies(r.Context(), id)
	if err != nil {
		s.logger.Debug("getting reverse dependencies", "id", id, "error", err)
		revDeps = nil
	}

	// Message beads among the mention refs move from the constellation into a
	// chat panel (ka-0000007791). Best-effort.
	chatMsgIDs := collectMessageRefIDs(bead.ID, mentions, mentionedBy)

	// An agent's own messages are linked only by a `from:<agent title>` label,
	// not by mentions, so fetch them too (ka-0000002848).
	if bead.Type == "agent" && strings.TrimSpace(bead.Title) != "" {
		res, lerr := s.daemon.ListBeadsFiltered(r.Context(), agentAuthoredMessagesQuery(bead.Title))
		if lerr != nil {
			s.logger.Debug("listing agent-authored messages", "id", id, "error", lerr)
		} else if res != nil {
			seen := make(map[string]struct{}, len(chatMsgIDs))
			for _, mid := range chatMsgIDs {
				seen[mid] = struct{}{}
			}
			for _, b := range res.Beads {
				if b == nil || b.ID == "" || b.ID == bead.ID {
					continue
				}
				if _, dupe := seen[b.ID]; dupe {
					continue
				}
				seen[b.ID] = struct{}{}
				chatMsgIDs = append(chatMsgIDs, b.ID)
			}
		}
	}

	var chatView mapChatView
	if len(chatMsgIDs) > 0 {
		msgBeads, herr := s.daemon.BatchGetBeads(r.Context(), chatMsgIDs)
		if herr != nil {
			s.logger.Debug("hydrating chat message beads", "id", id, "error", herr)
		} else {
			chatView = buildBeadChat(chatMsgIDs, msgBeads, s.basePath, s.now(), linkOpts)
		}
		// Strip message refs even if hydration failed.
		mentions = stripMessageRefs(mentions)
		mentionedBy = stripMessageRefs(mentionedBy)
	}

	treeLevel := parseTreeLevelParam(r.URL.Query().Get("tree"))
	showClosedAgents := parseShowClosedAgentsParam(r.URL.Query().Get("show_closed_agents"))
	constellation := s.buildConstellation(r.Context(), bead, deps, revDeps, similarResp, mentions, mentionedBy, treeLevel, showClosedAgents)

	email := oidcauth.EmailFromContext(r.Context())
	latest := latestCommentTime(comments)
	// Forward-panel followers, best-effort.
	followers, ferr := s.daemon.ListBeadFollowers(r.Context(), id)
	if ferr != nil {
		s.logger.Debug("listing followers", "id", id, "error", ferr)
		followers = nil
	}
	forwardView := buildForwardPanelView(id, email, followers)
	data := map[string]any{
		"Bead":                      bead,
		"OGImage":                   badgeOGImage(r, s.basePath, bead),
		"Forward":                   forwardView,
		"TitleHTML":                 LinkifyTextWithOpts(bead.Title, s.basePath, linkOpts),
		"DescriptionHTML":           descHTML,
		"Dependencies":              deps,
		"Mentions":                  mentions,
		"MentionedBy":               mentionedBy,
		"ReverseDeps":               revDeps,
		"Comments":                  linkifyCommentsWithOpts(s.mdInline, comments, s.basePath, time.Time{}, email, linkOpts),
		"CommentsLatestRFC":         commentsLatestAttr(latest),
		"Activity":                  activity,
		"Project":                   projectFromLabels(bead.Labels),
		"Similar":                   similarViews(similarResp, s.basePath),
		"Constellation":             constellation,
		"ConstellationDefaultState": s.constellationDefaultState(w, r),
		// Trickle-MR checklist (ka-0000005053 §5/§7, epic ka-0000005560); nil
		// for non-MR beads (AC-3). deps let ancestry fall back to the parent.
		"TrickleChecklist": buildTrickleChecklist(bead, deps, s.basePath),
		"Chat":             chatView,
		"AuthorEmail":      email,
		"ViewerEmail":      email,
		"CanComment":       email != "",
		"Actions":          buildBeadActions(bead, email, s.viewerPinnedSet(r.Context(), email)),
		"CurrentBeadID":    bead.ID,
		"GardenHref":       gardenHref,
	}
	// "Part of wish" chip linking to the thread_id's /threads page
	// (ka-0000008112); nil without one.
	data["ThreadChip"] = s.buildThreadChip(r.Context(), bead)
	data["ShippedIn"] = s.releaseShippedIn(r.Context(), bead)
	// Taint badges + actions (ADR docs/adr/taints.md §9.1, gd-jz56jXsHrK /
	// gd-nOnc1Q8Zcv).
	data["TaintActions"] = s.buildTaintActions(r.Context(), bead, email)
	// Inline spawn form, pre-filled with this bead and its project.
	if email != "" {
		q := r.URL.Query()
		if q.Get("task") == "" {
			q.Set("task", bead.ID)
		}
		if q.Get("project") == "" {
			if p := projectFromLabels(bead.Labels); p != "" {
				q.Set("project", p)
			}
		}
		r2 := r.Clone(r.Context())
		r2.URL.RawQuery = q.Encode()
		view := s.buildQuickSpawnSheetView(r2, email)
		data["SpawnSheet"] = view
	}
	// Jira timeline for source:jira mirror beads (F-C, epic ka-0000007401),
	// from their `triggered-by` reverse edges in revDeps. Best-effort.
	if isJiraMirrorBead(bead) {
		timelineIDs := collectJiraTimelineIDs(bead.ID, revDeps)
		if len(timelineIDs) > 0 {
			linked, terr := s.daemon.BatchGetBeads(r.Context(), timelineIDs)
			if terr != nil {
				s.logger.Debug("hydrating jira timeline beads", "id", id, "error", terr)
			} else {
				data["JiraTimeline"] = buildJiraTimeline(timelineIDs, linked, s.basePath, s.now())
			}
		}
	}
	s.injectTypeView(r.Context(), data, bead)
	// Gardener beads render the /gardener/<id> summary inline (ka-0000006542);
	// IsGardener stays unset on failure.
	s.injectGardenerBeadView(r.Context(), data, bead)
	// Next: the escalation gates, then IsGenericType, which marks a bead with
	// only the Fields dump (rendered below the narrative, ka-0000006872); it
	// must follow injectGardenerBeadView so IsGardener is settled.
	if ev, _ := data["Escalation"].(*escalationView); ev != nil {
		// Approve/Reject render only if the daemon's can-approve allows this
		// viewer (D9); needs the viewer's bearer, hence here.
		if ev.Open {
			ev.CanApprove, ev.ApproveBlockedReason = s.escalationApproveGate(r.Context(), ev.BeadID, email)
		}
		ev.CanRoute = viewerMayRouteEscalation(r) && ev.Open && !ev.AudienceForced
	}
	isGardener, _ := data["IsGardener"].(bool)
	data["IsGenericType"] = !isGardener && isGenericBeadType(bead.Type)
	if bead.Type == "rating" {
		if rv, _ := data["Rating"].(*ratingView); rv != nil {
			s.enrichRatingDetailView(r.Context(), rv, bead, mentionedBy, revDeps)
		}
	}
	if bead.Type == "agent" {
		// First-paint PTY snapshot ("" is fine; xterm.js fills in). Not for a
		// scoped principal or a verifier: coopmux is not project-scoped and
		// answers on the viewer's token, so this is admin-only like
		// GET /agents/{id}/screen.
		if !isScopedCtx(r.Context()) && !isVerifier(r.Context()) {
			data["ScreenAnsiText"] = s.getScreenSnapshot(r.Context(), bead)
			data["TerminalAvailable"] = s.terminalAvailable(bead)
		}
		// Cookie-persisted UI prefs (query may override), avoiding a flash of
		// defaults.
		data["FeedMode"] = s.feedMode(w, r)
		data["FeedDensity"] = s.feedDensity(w, r)
		data["TermFontSize"] = s.termFontSize(w, r)
		data["CoarsePointer"] = isCoarsePointer(r)
		// Public terminal link from COOPMUX_PUBLIC_URL + pod_name; coop_url is
		// a pod IP (ka-0000002453, decision ka-0000004056).
		data["CoopmuxTerminalURL"] = s.coopmuxTerminalURLFor(r.Context(), bead)
		// Slack thread link; empty without slack_thread_* fields or domain.
		data["SlackThreadURL"] = s.slackThreadURL(bead)
		// Context + cost summary; cost-only once the agent is not alive.
		agentLive := bead.Status != "closed" && isAgentStateIn(bead.Fields["agent_state"], agentStateAlive)
		data["Usage"] = buildAgentUsageView(bead.Fields, agentLive)
		// MRs via agent --produces--> task --produces--> MR, as mr_pills.
		data["ProducedMRs"] = s.buildAgentProducedMRs(r.Context(), bead.ID)
		// Lineage strip via prior_agent_id ∨ schedule_prior_agent_id
		// (ka-0000007852); nil hides it.
		data["Lineage"] = s.buildAgentLineageStrip(r.Context(), bead)
		// Wrap-up TLDR banner (ka-0000006621); empty Text hides it.
		data["WrapUpTLDR"] = buildAgentCardTLDR(bead)
		// The Slack card's "↻ Restart:" line (ka-0000008218); empty Text hides it.
		data["WrapUpRestart"] = buildAgentCardRestart(bead)
		// Spawn-provenance badge (gd-jbY5UiWlN4): policy-/human-approved or a
		// red "unreviewed" for a privileged role. Empty Label hides it.
		data["SpawnBadge"] = s.buildAgentSpawnBadge(r.Context(), bead)
		// Live-vs-approved SA permissions (gd-SyEYkqe601), lazy-loaded so the
		// RBAC probe never blocks the page.
		data["PermissionDiffEscalation"] = agentSpawnEscalationID(bead)
		data["Harness"] = agentHarness(bead.Fields)
		// The assigned task's line arms before its link (gd-ASLjQPkHJz); nil draws none.
		data["TaskArms"] = agentTaskArms(r.Context(), s.daemon, bead, deps)
	}
	if bead.Type != "rating" {
		// Rating widget from reverse `rates` edges, best-effort.
		data["Widget"] = s.buildRatingWidget(r.Context(), bead, email, 0, "", "")
	}
	s.render(w, r, "bead_show.html", data)
}

// handleBundleShow renders /bundle/{id}, the multi-doc reading page for
// type=bundle beads; per-format renderers come from registerBundleRenderer.
func (s *Server) handleBundleShow(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "Missing bundle id", http.StatusBadRequest)
		return
	}

	bundle, err := s.daemon.GetBead(r.Context(), id)
	if err != nil {
		if beadMissing(err) {
			http.Error(w, "Bundle not found", http.StatusNotFound)
			return
		}
		s.logger.Error("getting bundle bead", "id", id, "error", err)
		http.Error(w, "Failed to get bundle", http.StatusInternalServerError)
		return
	}
	if bundle.Type != "bundle" {
		http.Error(w, "Not a bundle bead", http.StatusNotFound)
		return
	}

	// Reverse deps (written by `gb bundle add`, ka-0000003477) list the docs;
	// mentions give the back-link. Best-effort.
	reverseDeps, err := s.daemon.GetReverseDependencies(r.Context(), id)
	if err != nil {
		s.logger.Debug("getting reverse deps for bundle", "id", id, "error", err)
		reverseDeps = nil
	}
	mentions, err := s.daemon.GetMentions(r.Context(), id, 50)
	if err != nil {
		s.logger.Debug("getting mentions for bundle", "id", id, "error", err)
		mentions = nil
	}

	view := buildBundleView(r.Context(), bundle, mentions, reverseDeps, s.daemon, s.newBundleRenderCtx())
	s.render(w, r, "bundle.html", map[string]any{"Bundle": view, "GardenHref": s.gardenBanner(r, retireBundle, gardenBundleTarget)})
}

// handleBeadComments renders the comments fragment for htmx lazy-load / poll.
// When `?since=<RFC3339Nano>` equals the latest comment's CreatedAt it returns
// 204, else the partial with comments newer than since flagged new. Either
// way X-Comments-Latest carries the cursor.
//
// Why 204, not 304: htmx 2 swaps a 304's empty body into the outerHTML
// target, erasing #comments-section and its poll trigger (ka-0000006459).
func (s *Server) handleBeadComments(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "Missing bead id", http.StatusBadRequest)
		return
	}
	bead, err := s.daemon.GetBead(r.Context(), id)
	if err != nil {
		if beadMissing(err) {
			http.Error(w, "Bead not found", http.StatusNotFound)
			return
		}
		s.logger.Error("getting bead", "id", id, "error", err)
		http.Error(w, "Failed to get bead", http.StatusInternalServerError)
		return
	}
	comments, err := s.daemon.GetComments(r.Context(), id)
	if err != nil {
		s.logger.Warn("getting comments", "id", id, "error", err)
		comments = nil
	}
	latest := latestCommentTime(comments)
	latestAttr := commentsLatestAttr(latest)

	since := parseSinceParam(r.URL.Query().Get("since"))
	// Unchanged: 204, not 304 (see the function doc).
	if !latest.IsZero() && !since.IsZero() && latest.Equal(since) {
		w.Header().Set("X-Comments-Latest", latestAttr)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	email := oidcauth.EmailFromContext(r.Context())
	w.Header().Set("X-Comments-Latest", latestAttr)
	// Same linkifier opts as the SSR render, so `!N` matches on poll
	// (ka-0000005612).
	linkOpts := s.linkifyOptsForBead(r.Context(), bead)
	s.renderPartial(w, r, "templates/partials/comments.html", "comments", map[string]any{
		"Bead":              bead,
		"Comments":          linkifyCommentsWithOpts(s.mdInline, comments, s.basePath, since, email, linkOpts),
		"CommentsLatestRFC": latestAttr,
		"AuthorEmail":       email,
		"CanComment":        email != "",
	})
}

// parseSinceParam parses the `?since=` query value, returning the zero time
// for empty or malformed input (so a bad cursor degrades to a full render).
func parseSinceParam(raw string) time.Time {
	if raw == "" {
		return time.Time{}
	}
	t, err := time.Parse(commentTimeFormat, raw)
	if err != nil {
		return time.Time{}
	}
	return t
}

// commentsLatestAttr renders the `data-comments-latest` value, or "" when
// there are no comments (so the next poll gets a full render).
func commentsLatestAttr(latest time.Time) string {
	if latest.IsZero() {
		return ""
	}
	return latest.UTC().Format(commentTimeFormat)
}

// handleAddComment posts a new comment as the authenticated user and
// re-renders the comments fragment so htmx can swap it into place.
func (s *Server) handleAddComment(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "Missing bead id", http.StatusBadRequest)
		return
	}
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		// Defense in depth: the OIDC middleware should already block this.
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form", http.StatusBadRequest)
		return
	}
	text := strings.TrimSpace(r.PostForm.Get("text"))
	if text == "" {
		s.renderCommentError(w, r, id, email, "", "Comment cannot be empty.", http.StatusBadRequest)
		return
	}
	if len(text) > maxCommentLen {
		s.renderCommentError(w, r, id, email, text, "Comment is too long (max 10000 characters).", http.StatusRequestEntityTooLarge)
		return
	}
	// X-Actor stamp (ka-0000006262). The daemon derives comment.Author from
	// the auth identity (AUDIT §C3), so with a user bearer this is ignored; on
	// a static-token viewer it is dropped too, since X-Actor must name an agent
	// (gd-qZER8OOiIb), and the comment stores Author="static".
	ctx := beadsapi.WithActor(r.Context(), email)
	if err := s.daemon.AddComment(ctx, id, email, text); err != nil {
		s.logger.Error("adding comment", "id", id, "author", email, "error", err)
		s.renderCommentError(w, r, id, email, text, "Failed to post comment.", http.StatusBadGateway)
		return
	}
	// Re-render the comments fragment so htmx swaps in the freshly-posted comment.
	s.handleBeadComments(w, r)
}

// renderCommentError re-renders the comments partial with an inline error,
// keeping the user's draft.
func (s *Server) renderCommentError(w http.ResponseWriter, r *http.Request, id, email, draft, msg string, status int) {
	if respondAction(w, r, id, actionFailure(msg, status)) {
		return
	}
	bead, err := s.daemon.GetBead(r.Context(), id)
	if err != nil {
		// Plain error so htmx leaves the existing form alone.
		http.Error(w, msg, status)
		return
	}
	comments, err := s.daemon.GetComments(r.Context(), id)
	if err != nil {
		s.logger.Warn("getting comments after add error", "id", id, "error", err)
		comments = nil
	}
	w.WriteHeader(status)
	// Same linkifier opts as the SSR render (ka-0000005612).
	linkOpts := s.linkifyOptsForBead(r.Context(), bead)
	s.renderPartial(w, r, "templates/partials/comments.html", "comments", map[string]any{
		"Bead":              bead,
		"Comments":          linkifyCommentsWithOpts(s.mdInline, comments, s.basePath, time.Time{}, email, linkOpts),
		"CommentsLatestRFC": commentsLatestAttr(latestCommentTime(comments)),
		"AuthorEmail":       email,
		"CanComment":        true,
		"CommentError":      msg,
		"CommentDraft":      draft,
		// CommentDraftHTML: the draft rendered as it will be saved.
		"CommentDraftHTML": s.renderMarkdownCommentDraftForBead(r.Context(), bead, draft),
	})
}

// handleBeadDeps renders the dependencies fragment for htmx refresh.
func (s *Server) handleBeadDeps(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "Missing bead id", http.StatusBadRequest)
		return
	}
	if _, err := s.daemon.GetBead(r.Context(), id); err != nil {
		if beadMissing(err) {
			http.Error(w, "Bead not found", http.StatusNotFound)
			return
		}
		s.logger.Error("getting bead", "id", id, "error", err)
		http.Error(w, "Failed to get bead", http.StatusInternalServerError)
		return
	}
	deps, err := s.daemon.GetDependencies(r.Context(), id)
	if err != nil {
		s.logger.Warn("getting dependencies", "id", id, "error", err)
		deps = nil
	}
	mentionedBy, err := s.daemon.GetMentionedBy(r.Context(), id, 50)
	if err != nil {
		s.logger.Debug("getting mentioned-by", "id", id, "error", err)
		mentionedBy = nil
	}
	mentions, err := s.daemon.GetMentions(r.Context(), id, 50)
	if err != nil {
		s.logger.Debug("getting mentions", "id", id, "error", err)
		mentions = nil
	}
	s.renderPartial(w, r, "templates/partials/deps.html", "deps", map[string]any{
		"Dependencies": deps,
		"Mentions":     mentions,
		"MentionedBy":  mentionedBy,
		"BasePath":     s.basePath,
	})
}

func (s *Server) renderPartial(w http.ResponseWriter, r *http.Request, file, name string, data map[string]any) {
	tmpl, ok := s.partials[file]
	if !ok {
		s.logger.Error("partial template not found", "file", file)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	if data == nil {
		data = make(map[string]any)
	}
	data["BasePath"] = s.basePath
	bound, err := s.bindAgentLink(tmpl, r)
	if err != nil {
		s.logger.Error("cloning partial template", "file", file, "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := bound.ExecuteTemplate(w, name, data); err != nil {
		if isClientDisconnect(err) {
			s.logger.Debug("partial render aborted: client disconnect", "file", file, "error", err)
		} else {
			s.logger.Error("partial render error", "file", file, "error", err)
		}
	}
}

// similarViews flattens a FindSimilarResponse into view rows; nil (nothing
// valid) means hide the panel.
func similarViews(resp *beadsapi.FindSimilarResponse, basePath string) []similarBeadView {
	if resp == nil || len(resp.Results) == 0 {
		return nil
	}
	views := make([]similarBeadView, 0, len(resp.Results))
	for _, r := range resp.Results {
		if r == nil || r.Bead == nil {
			continue
		}
		views = append(views, similarBeadView{
			ID:    r.Bead.ID,
			Title: LinkifyText(r.Bead.Title, basePath),
			Score: strconv.FormatFloat(r.Score, 'f', 3, 64),
		})
	}
	if len(views) == 0 {
		return nil
	}
	return views
}

// injectTypeView populates per-type view-model keys on the template data
// for the dispatcher in bead_show.html. Unknown types fall through to the
// generic field-list dump and need no entry here.
func (s *Server) injectTypeView(ctx context.Context, data map[string]any, bead *beadsapi.BeadDetail) {
	switch bead.Type {
	case "decision":
		dv := buildDecisionView(bead, func(text string) template.HTML {
			var buf bytes.Buffer
			if err := s.md.Convert([]byte(text), &buf); err != nil {
				return LinkifyText(text, s.basePath)
			}
			return LinkifyHTML(buf.String(), s.basePath)
		})
		if dv != nil {
			authorEmail, _ := data["AuthorEmail"].(string)
			dv.CanResolve = !dv.Resolved && authorEmail != ""
			dv.Origin = s.resolveRequestOrigin(ctx, bead)
			dv.DetailURL = s.basePath + "/decisions/" + url.PathEscape(bead.ID)
			dv.Evidence = s.decisionEvidenceOf(ctx, bead)
		}
		data["Decision"] = dv
	case "escalation":
		// Rich escalation card (ka-0000006884); the caller fills CanApprove.
		ev := buildEscalationView(bead, s.md, s.basePath, s.now())
		if ev != nil {
			ev.Origin = s.resolveRequestOrigin(ctx, bead)
			s.fillLaneOffline(ctx, ev)
		}
		data["Escalation"] = ev
	case "schedule":
		data["Schedule"] = buildScheduleView(bead, time.Now())
	case "formula":
		data["Formula"] = buildFormulaView(bead)
	case "molecule":
		// Formula DAG with live step status and cost (ka-0000007739).
		data["Molecule"] = s.buildMoleculeExecView(ctx, bead)
	case "advice":
		data["Advice"] = buildAdviceView(bead)
	case "dashboard":
		// Grafana dashboard bead: iframe when GRAFANA_BASE_URL is set, plus a
		// best-effort panel list (W2c, ka-0000001078).
		data["Dashboard"] = buildDashboardView(ctx, bead, s.daemon, s.grafanaBaseURL)
	case "rating":
		now := time.Now()
		rv := buildRatingView(bead, "", s.renderBeadMarkdown, now)
		if rv != nil && rv.RatedBeadID != "" {
			if rb, err := s.daemon.GetBead(ctx, rv.RatedBeadID); err == nil && rb != nil {
				rv.RatedTitle = rb.Title
			}
		}
		data["Rating"] = rv
	case "report":
		data["Report"] = buildReportView(bead, s.md, s.basePath)
	case "session":
		// Per-session slowest tool calls written by `gb session close` (P4,
		// gd-YcmtujOTrC). nil suppresses the section in type_generic.
		data["SlowCalls"] = buildSessionSlowCalls(bead)
	case "rich-report":
		// Widget-instance children come from the caller's mention sets; a
		// failed child degrades to a placeholder card.
		mentionedBy, _ := data["MentionedBy"].([]beadsapi.BeadReferenceSummary)
		mentions, _ := data["Mentions"].([]beadsapi.BeadReferenceSummary)
		data["RichReport"] = buildRichReportView(ctx, bead, mentions, mentionedBy, s.daemon, s.newWidgetRenderCtx())
	case "bundle":
		// Bundle TOC from reverse deps (ka-0000003477); CTA to /bundle/<id>.
		reverseDeps, _ := data["ReverseDeps"].([]beadsapi.Dependency)
		data["Bundle"] = buildBundleSummaryView(ctx, bead, reverseDeps, s.daemon, s.basePath)
	case "widget-type":
		// Inline preview against a sample payload (P5, ka-0000002154).
		view := buildWidgetTypeView(s.basePath, bead, nil, "overview")
		view.Preview = s.buildPreview(ctx, bead, "")
		data["WidgetType"] = view
	}
}

// isGenericBeadType reports whether a bead type renders bead_show.html's
// generic type_generic Fields dump, i.e. is not one of its rich type branches
// (session, though in injectTypeView, is generic). An explicit deny-list, so a
// new rich type must be added here and in the template.
func isGenericBeadType(t string) bool {
	switch t {
	case "decision", "escalation", "schedule", "formula", "molecule", "merge-request",
		"agent", "advice", "dashboard", "rating", "rich-report", "report",
		"bundle", "widget-type":
		return false
	}
	return true
}

// projectFromLabels returns the value of the first "project:<name>" label,
// or empty if none is present.
func projectFromLabels(labels []string) string {
	for _, l := range labels {
		if v, ok := strings.CutPrefix(l, "project:"); ok {
			return v
		}
	}
	return ""
}

// routeRegistrar is the slice of *http.ServeMux that RegisterRoutes uses.
type routeRegistrar interface {
	Handle(pattern string, handler http.Handler)
	HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request))
}
