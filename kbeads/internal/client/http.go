package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"gardenio/clicreds/avareauth"
	"gardenio/clicreds/beadswire"
	"gardenio/kbeads/internal/model"
)

// RefreshCallback is invoked after a successful refresh-token exchange so
// callers (e.g. the kd CLI) can persist the new access token (and rotated
// refresh token, if any) outside the client. Either argument may be empty —
// newRefresh is empty when the server keeps the refresh stable across
// exchanges (rotation disabled).
type RefreshCallback func(newAccess, newRefresh string)

// HTTPClient implements BeadsClient using the kbeads HTTP/JSON REST API.
type HTTPClient struct {
	baseURL    string
	fleet      string
	actor      string
	operatedBy string
	httpClient *http.Client

	// tokenMu guards token + refreshToken. Held briefly to read/copy values
	// for outgoing requests; refresh is serialised by refreshMu instead so
	// other requests can keep flowing during the round-trip.
	tokenMu      sync.RWMutex
	token        string
	refreshToken string

	// refreshMu serialises calls to /v1/auth/refresh. Concurrent 401s share
	// one exchange — once it returns, the second caller observes the
	// updated token via tokenMu and retries with the new credential.
	refreshMu sync.Mutex

	onRefresh RefreshCallback

	// bearerRefresher, when set, is asked for a replacement bearer on a 401
	// that the daemon's own refresh-token path does not handle (see
	// SetBearerRefresher). Guarded by tokenMu; calls are serialised by
	// refreshMu.
	bearerRefresher func(ctx context.Context) (string, error)

	// onDaemonVersion, when set, is invoked at most once with the daemon's
	// X-Kbeads-Version response header value the first time it is observed.
	// The kd/gb CLI uses it to emit a version-skew warning to stderr without
	// coupling the transport to stdio (ka-0000002965). Guarded by
	// versionOnce so a burst of requests fires it a single time.
	onDaemonVersion func(daemonVersion string)
	versionOnce     sync.Once

	// dbAuthRetries bounds how many EXTRA times doJSON re-sends a request that
	// the daemon answered with its transient DB-auth 503 (SQLSTATE 28P01;
	// ka-0000006554), for every method — see beadswire.DBAuthRetries for why
	// that is safe. Zero disables the retry (the original single-shot behavior).
	dbAuthRetries int
	// dbAuthRetryBackoff is the base wait between DB-auth retries when the
	// server sends no parseable Retry-After (beadswire.DBAuthRetryDelay).
	dbAuthRetryBackoff time.Duration
	// sleep is the sleep primitive doJSON uses between retries; overridable in
	// tests so they do not wall-clock wait. Defaults to a context-aware sleep.
	sleep func(ctx context.Context, d time.Duration) error

	// zitiContext, when set, is the OpenZiti context backing the transport.
	// It is released by Close(). Nil for plain-HTTP clients.
	zitiContext zitiDialer
}

// sendOnceRetryConn wraps sendOnce with a bounded retry on a transport-level
// connection failure, scoped to idempotent methods only (see
// beadswire.ConnRetries). Non-idempotent methods are deliberately excluded:
// unlike the dbAuthRetries case above, a connection reset gives no guarantee
// the daemon never applied the mutation.
func (c *HTTPClient) sendOnceRetryConn(ctx context.Context, method, path string, bodyBytes []byte) (status int, respBody []byte, wwwAuth, retryAfter, usedToken string, err error) {
	err = beadswire.RetryConn(ctx, method, c.sleep, func() error {
		var serr error
		status, respBody, wwwAuth, retryAfter, usedToken, serr = c.sendOnce(ctx, method, path, bodyBytes)
		return serr
	})
	return status, respBody, wwwAuth, retryAfter, usedToken, err
}

// NewHTTPClient creates a new HTTP client targeting the given base URL
// (e.g. "http://localhost:8080"). When token is non-empty, an Authorization
// header is set on every request.
func NewHTTPClient(baseURL, token string) *HTTPClient {
	return newHTTPClient(baseURL, token, http.DefaultTransport, nil)
}

func newHTTPClient(baseURL, token string, inner http.RoundTripper, ztx zitiDialer) *HTTPClient {
	return &HTTPClient{
		baseURL:            strings.TrimRight(baseURL, "/"),
		token:              token,
		zitiContext:        ztx,
		httpClient:         &http.Client{Transport: inner},
		dbAuthRetries:      beadswire.DBAuthRetries,
		dbAuthRetryBackoff: beadswire.DBAuthRetryBackoff,
		sleep:              beadswire.SleepWithContext,
	}
}

// SetRefreshToken enables transparent refresh-on-401 by giving the client a
// refresh-token plaintext. When set, doJSON intercepts 401 responses whose
// WWW-Authenticate header carries error="invalid_token" + token-expired,
// exchanges the refresh for a new access token, swaps it onto the client,
// and retries the request once. Pass "" to disable.
func (c *HTTPClient) SetRefreshToken(refreshToken string) {
	c.tokenMu.Lock()
	c.refreshToken = refreshToken
	c.tokenMu.Unlock()
}

// SetOnRefresh installs a callback invoked after a successful refresh-token
// exchange. The callback receives the new access token and (when the server
// rotated it) the new refresh token. It runs synchronously inside the
// refresh path — keep it short. Pass nil to clear.
func (c *HTTPClient) SetOnRefresh(cb RefreshCallback) {
	c.tokenMu.Lock()
	c.onRefresh = cb
	c.tokenMu.Unlock()
}

// SetBearerRefresher installs a hook for 401s that the daemon's refresh-token
// path (SetRefreshToken) does not cover: any 401 without the expired-token
// challenge, including one from an edge proxy in front of the daemon. doJSON
// calls fn once, swaps the returned token onto the client and retries the
// request once; fn returning an error, "" or the same token leaves the
// original 401 to surface. The kd CLI uses it to refresh a `kd login` session
// through Keycloak when the edge or daemon rejected a token kd believed fresh
// (gd-NrzwHXUxV0). Concurrent 401s share one call (refreshMu). Pass nil to clear.
func (c *HTTPClient) SetBearerRefresher(fn func(ctx context.Context) (string, error)) {
	c.tokenMu.Lock()
	c.bearerRefresher = fn
	c.tokenMu.Unlock()
}

// SetOnDaemonVersion installs a callback invoked at most once with the
// daemon's X-Kbeads-Version response header. The kd/gb CLI uses it to warn
// about version skew when the local binary is materially behind the daemon
// (ka-0000002965). Pass nil to clear.
func (c *HTTPClient) SetOnDaemonVersion(cb func(daemonVersion string)) {
	c.onDaemonVersion = cb
}

// noteDaemonVersion fires the onDaemonVersion callback the first time a
// non-empty X-Kbeads-Version header is seen. Safe to call on every response.
func (c *HTTPClient) noteDaemonVersion(daemonVersion string) {
	if daemonVersion == "" || c.onDaemonVersion == nil {
		return
	}
	c.versionOnce.Do(func() { c.onDaemonVersion(daemonVersion) })
}

// SetFleet configures the fleet override header sent with every request.
func (c *HTTPClient) SetFleet(fleet string) {
	c.fleet = fleet
}

// SetActor configures the X-Actor header sent with every request. The server
// honors this only for primary-static-token callers — it is ignored for
// OIDC and fleet-scoped tokens, so sending it from all clients is safe.
func (c *HTTPClient) SetActor(actor string) {
	c.actor = actor
}

// SetOperatedBy configures the X-Operated-By header sent with every request.
// The server honors this only for primary-static-token callers (per R3 §4.2);
// OIDC callers have operated_by collapsed from their JWT subject and the
// header is ignored. Sending it from all clients is therefore safe.
func (c *HTTPClient) SetOperatedBy(v string) {
	c.operatedBy = v
}

// Close releases any transport resources owned by the HTTP client.
func (c *HTTPClient) Close() error {
	if c.zitiContext != nil {
		c.zitiContext.Close()
	}
	return nil
}

// --- Bead CRUD ---

func (c *HTTPClient) CreateBead(ctx context.Context, req *CreateBeadRequest) (*model.Bead, error) {
	var bead model.Bead
	if err := c.doJSON(ctx, http.MethodPost, "/v1/beads", req, &bead); err != nil {
		return nil, err
	}
	return &bead, nil
}

func (c *HTTPClient) GetBead(ctx context.Context, id string) (*model.Bead, error) {
	var bead model.Bead
	if err := c.doJSON(ctx, http.MethodGet, "/v1/beads/"+url.PathEscape(id), nil, &bead); err != nil {
		return nil, err
	}
	return &bead, nil
}

func (c *HTTPClient) ListBeads(ctx context.Context, req *ListBeadsRequest) (*ListBeadsResponse, error) {
	q := url.Values{}
	if len(req.Status) > 0 {
		q.Set("status", strings.Join(req.Status, ","))
	}
	if len(req.Type) > 0 {
		q.Set("type", strings.Join(req.Type, ","))
	}
	if len(req.ExcludeTypes) > 0 {
		q.Set("exclude_type", strings.Join(req.ExcludeTypes, ","))
	}
	if len(req.Kind) > 0 {
		q.Set("kind", strings.Join(req.Kind, ","))
	}
	if len(req.Labels) > 0 {
		q.Set("labels", strings.Join(req.Labels, ","))
	}
	if len(req.LabelsAny) > 0 {
		q.Set("labels_any", strings.Join(req.LabelsAny, ","))
	}
	if len(req.ExcludeLabels) > 0 {
		q.Set("exclude_labels", strings.Join(req.ExcludeLabels, ","))
	}
	if req.Assignee != "" {
		q.Set("assignee", req.Assignee)
	}
	if req.Unassigned {
		q.Set("unassigned", "true")
	}
	if req.CreatedBy != "" {
		q.Set("created_by", req.CreatedBy)
	}
	if req.Search != "" {
		q.Set("search", req.Search)
	}
	if req.Sort != "" {
		q.Set("sort", req.Sort)
	}
	if req.Priority != nil {
		q.Set("priority", fmt.Sprintf("%d", *req.Priority))
	}
	if req.NoOpenDeps {
		q.Set("no_open_deps", "true")
	}
	if req.NoParent {
		q.Set("no_parent", "true")
	}
	if req.ShortTitleMissing {
		q.Set("short_title_missing", "true")
	}
	if req.Heraldry != "" {
		q.Set("heraldry", req.Heraldry)
	}
	if req.HeraldryProject != "" {
		q.Set("heraldry_project", req.HeraldryProject)
	}
	if req.ExcludeScheduled {
		q.Set("exclude_scheduled", "true")
	}
	if req.Limit > 0 {
		q.Set("limit", fmt.Sprintf("%d", req.Limit))
	}
	if req.Offset > 0 {
		q.Set("offset", fmt.Sprintf("%d", req.Offset))
	}
	if req.Fleet != "" {
		q.Set("fleet", req.Fleet)
	}
	for k, v := range req.FieldFilters {
		q.Add("field", k+"="+v)
	}
	if req.Fleet != "" {
		q.Set("fleet", req.Fleet)
	}
	if req.IncludeDeleted {
		q.Set("include_deleted", "true")
	}
	if req.OnlyDeleted {
		q.Set("only_deleted", "true")
	}
	if req.Mentions != "" {
		q.Set("mentions", req.Mentions)
		if req.MentionsType != nil {
			// url.Values.Set always emits the key, even with an empty
			// value — the server distinguishes "?mentions_type=" (untyped
			// only) from missing.
			q.Set("mentions_type", *req.MentionsType)
		}
	}
	if len(req.OperatedByAliases) > 0 {
		q.Set("operated_by_alias_in", strings.Join(req.OperatedByAliases, ","))
	}
	// parent=<bead-id> restricts to the transitive parent-child subtree under
	// the bead (the REVERSE child set). The server maps this to
	// filter.ParentSubtreeID — the same recursive CTE the scheduler's
	// bead_query {parent} gate uses. See ListBeadsRequest.Parent.
	if req.Parent != "" {
		q.Set("parent", req.Parent)
	}
	// has_report (R1, ka-0000007923) and blocked_on_decision (R2,
	// ka-0000001065) are three-state pointers: nil = no filter, &true /
	// &false emit "true" / "false" respectively.
	if req.HasReport != nil {
		if *req.HasReport {
			q.Set("has_report", "true")
		} else {
			q.Set("has_report", "false")
		}
	}
	if req.BlockedOnDecision != nil {
		if *req.BlockedOnDecision {
			q.Set("blocked_on_decision", "true")
		} else {
			q.Set("blocked_on_decision", "false")
		}
	}
	// parked_on_decision (ka-0000003797): text-mention sibling, same
	// three-state pointer shape as blocked_on_decision above.
	if req.ParkedOnDecision != nil {
		if *req.ParkedOnDecision {
			q.Set("parked_on_decision", "true")
		} else {
			q.Set("parked_on_decision", "false")
		}
	}
	// with_open_blockers=true denormalizes each row's OPEN blocking edges onto
	// the response (ka-0000000945). Enrichment, not a filter.
	if req.WithOpenBlockers {
		q.Set("with_open_blockers", "true")
	}
	// count_only=true short-circuits the daemon to a bare SELECT COUNT(*),
	// returning only the total (empty Beads). The `kd list` operational nudge
	// uses this to size the hidden escalation/decision count cheaply.
	if req.CountOnly {
		q.Set("count_only", "true")
	}
	// Time-window params (ka-0000008195). RFC3339 on the wire; the server
	// silently ignores unparseable values, but the client always emits a
	// well-formed timestamp.
	setTimeParam := func(key string, t *time.Time) {
		if t != nil {
			q.Set(key, t.Format(time.RFC3339))
		}
	}
	setTimeParam("created_after", req.CreatedAfter)
	setTimeParam("created_before", req.CreatedBefore)
	setTimeParam("updated_after", req.UpdatedAfter)
	setTimeParam("updated_before", req.UpdatedBefore)
	setTimeParam("closed_after", req.ClosedAfter)
	setTimeParam("closed_before", req.ClosedBefore)

	path := "/v1/beads"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}

	var resp ListBeadsResponse
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *HTTPClient) UpdateBead(ctx context.Context, id string, req *UpdateBeadRequest) (*model.Bead, error) {
	var bead model.Bead
	if err := c.doJSON(ctx, http.MethodPatch, "/v1/beads/"+url.PathEscape(id), req, &bead); err != nil {
		return nil, err
	}
	return &bead, nil
}

func (c *HTTPClient) EditBeadSubField(ctx context.Context, id, field string, req *EditSubFieldRequest) (*model.Bead, error) {
	var bead model.Bead
	path := "/v1/beads/" + url.PathEscape(id) + "/fields/" + url.PathEscape(field) + "/edit"
	if err := c.doJSON(ctx, http.MethodPost, path, req, &bead); err != nil {
		return nil, err
	}
	return &bead, nil
}

func (c *HTTPClient) CloseBead(ctx context.Context, id string, closedBy string) (*model.Bead, error) {
	body := map[string]string{}
	if closedBy != "" {
		body["closed_by"] = closedBy
	}
	var bead model.Bead
	if err := c.doJSON(ctx, http.MethodPost, "/v1/beads/"+url.PathEscape(id)+"/close", body, &bead); err != nil {
		return nil, err
	}
	return &bead, nil
}

func (c *HTTPClient) DeleteBead(ctx context.Context, id string) error {
	return c.doJSON(ctx, http.MethodDelete, "/v1/beads/"+url.PathEscape(id), nil, nil)
}

// RestoreBead clears deleted_at on a soft-deleted bead, returning the
// restored bead. Admin-only on the server.
func (c *HTTPClient) RestoreBead(ctx context.Context, id string) (*model.Bead, error) {
	var bead model.Bead
	if err := c.doJSON(ctx, http.MethodPost, "/v1/beads/"+url.PathEscape(id)+"/restore", nil, &bead); err != nil {
		return nil, err
	}
	return &bead, nil
}

// --- Fleet ---

func (c *HTTPClient) TransferBead(ctx context.Context, beadID, toFleet string) (*model.Bead, error) {
	body := map[string]string{"to_fleet": toFleet}
	var bead model.Bead
	if err := c.doJSON(ctx, http.MethodPost, "/v1/beads/"+url.PathEscape(beadID)+"/transfer", body, &bead); err != nil {
		return nil, err
	}
	return &bead, nil
}

// --- Dependencies ---

func (c *HTTPClient) AddDependency(ctx context.Context, req *AddDependencyRequest) (*model.Dependency, error) {
	body := map[string]string{
		"depends_on_id": req.DependsOnID,
		"type":          req.Type,
		"created_by":    req.CreatedBy,
	}
	var dep model.Dependency
	if err := c.doJSON(ctx, http.MethodPost, "/v1/beads/"+url.PathEscape(req.BeadID)+"/dependencies", body, &dep); err != nil {
		return nil, err
	}
	return &dep, nil
}

func (c *HTTPClient) RemoveDependency(ctx context.Context, beadID, dependsOnID, depType string) (bool, error) {
	q := url.Values{}
	q.Set("depends_on_id", dependsOnID)
	if depType != "" {
		q.Set("type", depType)
	}
	path := "/v1/beads/" + url.PathEscape(beadID) + "/dependencies?" + q.Encode()
	// A *bool distinguishes "daemon said no row matched" (present, false) from
	// "daemon predates the signal" (absent / bodiless 204). Absent MUST read as
	// removed=true: assuming a no-op there would make a NEW kd report "no such
	// dependency" for a removal an OLD daemon really performed (ka-0000001966).
	var resp struct {
		Removed *bool `json:"removed"`
	}
	if err := c.doJSON(ctx, http.MethodDelete, path, nil, &resp); err != nil {
		return false, err
	}
	if resp.Removed == nil {
		return true, nil
	}
	return *resp.Removed, nil
}

func (c *HTTPClient) GetDependencies(ctx context.Context, beadID string) ([]*model.Dependency, error) {
	var resp struct {
		Dependencies []*model.Dependency `json:"dependencies"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/v1/beads/"+url.PathEscape(beadID)+"/dependencies", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Dependencies, nil
}

func (c *HTTPClient) GetReverseDependencies(ctx context.Context, beadID string) ([]*model.Dependency, error) {
	var resp struct {
		Dependencies []*model.Dependency `json:"dependencies"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/v1/beads/"+url.PathEscape(beadID)+"/dependents", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Dependencies, nil
}

// --- References ---

func (c *HTTPClient) ListBeadsMentioning(ctx context.Context, beadID string, limit, offset int, relationshipType *string) (*ListReferencesResponse, error) {
	return c.listReferences(ctx, beadID, "/mentioned-by", limit, offset, relationshipType)
}

func (c *HTTPClient) ListBeadReferencesFrom(ctx context.Context, beadID string, limit, offset int, relationshipType *string) (*ListReferencesResponse, error) {
	return c.listReferences(ctx, beadID, "/mentions", limit, offset, relationshipType)
}

func (c *HTTPClient) listReferences(ctx context.Context, beadID, suffix string, limit, offset int, relationshipType *string) (*ListReferencesResponse, error) {
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if offset > 0 {
		q.Set("offset", strconv.Itoa(offset))
	}
	if relationshipType != nil {
		// url.Values.Set always emits the key, even with an empty value —
		// the server distinguishes "?type=" (untyped filter) from missing.
		q.Set("type", *relationshipType)
	}
	path := "/v1/beads/" + url.PathEscape(beadID) + suffix
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var resp ListReferencesResponse
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, err
	}
	if resp.References == nil {
		resp.References = []*model.BeadReferenceSummary{}
	}
	return &resp, nil
}

// --- Labels ---

func (c *HTTPClient) AddLabel(ctx context.Context, beadID, label string) (*model.Bead, error) {
	body := map[string]string{"label": label}
	var bead model.Bead
	if err := c.doJSON(ctx, http.MethodPost, "/v1/beads/"+url.PathEscape(beadID)+"/labels", body, &bead); err != nil {
		return nil, err
	}
	return &bead, nil
}

func (c *HTTPClient) RemoveLabel(ctx context.Context, beadID, label string) (bool, error) {
	// A *bool distinguishes "daemon said no row matched" (present, false) from
	// "daemon predates the signal" (absent / bodiless 204). Absent MUST read as
	// removed=true: assuming a no-op there would make a NEW kd report "no such
	// label" for a removal an OLD daemon really performed (ka-0000000424).
	var resp struct {
		Removed *bool `json:"removed"`
	}
	path := "/v1/beads/" + url.PathEscape(beadID) + "/labels/" + url.PathEscape(label)
	if err := c.doJSON(ctx, http.MethodDelete, path, nil, &resp); err != nil {
		return false, err
	}
	if resp.Removed == nil {
		return true, nil
	}
	return *resp.Removed, nil
}

func (c *HTTPClient) GetLabels(ctx context.Context, beadID string) ([]string, error) {
	var resp struct {
		Labels []string `json:"labels"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/v1/beads/"+url.PathEscape(beadID)+"/labels", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Labels, nil
}

// --- Comments ---

func (c *HTTPClient) AddComment(ctx context.Context, beadID, author, text string) (*model.Comment, error) {
	body := map[string]string{"author": author, "text": text}
	var comment model.Comment
	if err := c.doJSON(ctx, http.MethodPost, "/v1/beads/"+url.PathEscape(beadID)+"/comments", body, &comment); err != nil {
		return nil, err
	}
	return &comment, nil
}

func (c *HTTPClient) GetComments(ctx context.Context, beadID string) ([]*model.Comment, error) {
	var resp struct {
		Comments []*model.Comment `json:"comments"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/v1/beads/"+url.PathEscape(beadID)+"/comments", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Comments, nil
}

func (c *HTTPClient) DeleteComment(ctx context.Context, beadID string, commentID int64) (bool, error) {
	var resp struct {
		Deleted bool `json:"deleted"`
	}
	path := "/v1/beads/" + url.PathEscape(beadID) + "/comments/" + strconv.FormatInt(commentID, 10)
	if err := c.doJSON(ctx, http.MethodDelete, path, nil, &resp); err != nil {
		return false, err
	}
	return resp.Deleted, nil
}

func (c *HTTPClient) TrimComments(ctx context.Context, beadID string, keepLast int, authorFilter string) ([]int64, error) {
	body := map[string]any{"keep_last": keepLast}
	if authorFilter != "" {
		body["author"] = authorFilter
	}
	var resp struct {
		DeletedIDs []int64 `json:"deleted_ids"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/v1/beads/"+url.PathEscape(beadID)+"/comments/trim", body, &resp); err != nil {
		return nil, err
	}
	return resp.DeletedIDs, nil
}

// --- Events ---

func (c *HTTPClient) GetEvents(ctx context.Context, beadID string) ([]*model.Event, error) {
	var resp struct {
		Events []*model.Event `json:"events"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/v1/beads/"+url.PathEscape(beadID)+"/events", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Events, nil
}

func (c *HTTPClient) ListGlobalEvents(ctx context.Context, req *ListGlobalEventsRequest) (*ListGlobalEventsResponse, error) {
	q := url.Values{}
	if !req.Since.IsZero() {
		q.Set("since", req.Since.UTC().Format(time.RFC3339))
	}
	if !req.Until.IsZero() {
		q.Set("until", req.Until.UTC().Format(time.RFC3339))
	}
	if req.Cursor > 0 {
		q.Set("cursor", strconv.FormatInt(req.Cursor, 10))
	}
	if req.Limit > 0 {
		q.Set("limit", strconv.Itoa(req.Limit))
	}
	if req.Desc {
		q.Set("order", "desc")
	}
	path := "/v1/events"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var resp ListGlobalEventsResponse
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// --- Mutation history ---

func (c *HTTPClient) GetMutations(ctx context.Context, beadID string) ([]*model.MutationRow, error) {
	var resp struct {
		Mutations []*model.MutationRow `json:"mutations"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/v1/beads/"+url.PathEscape(beadID)+"/mutations", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Mutations, nil
}

func (c *HTTPClient) UndoMutation(ctx context.Context, mutationID int64) (*UndoMutationResponse, error) {
	var resp UndoMutationResponse
	path := "/v1/mutations/" + strconv.FormatInt(mutationID, 10) + "/undo"
	if err := c.doJSON(ctx, http.MethodPost, path, nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// --- Taints (docs/adr/taints.md §7) ---

func (c *HTTPClient) GrantTaint(ctx context.Context, name string, req *GrantTaintRequest) (*model.TaintGrant, error) {
	var grant model.TaintGrant
	path := "/v1/taints/" + url.PathEscape(name) + "/grants"
	if err := c.doJSON(ctx, http.MethodPost, path, req, &grant); err != nil {
		return nil, err
	}
	return &grant, nil
}

func (c *HTTPClient) RevokeTaint(ctx context.Context, name, principal string) (bool, error) {
	var resp struct {
		Removed bool `json:"removed"`
	}
	path := "/v1/taints/" + url.PathEscape(name) + "/grants/" + url.PathEscape(principal)
	if err := c.doJSON(ctx, http.MethodDelete, path, nil, &resp); err != nil {
		return false, err
	}
	return resp.Removed, nil
}

func (c *HTTPClient) ApplyTaint(ctx context.Context, beadID, taintName string) (*model.BeadTaint, error) {
	body := map[string]string{"name": taintName}
	var bt model.BeadTaint
	if err := c.doJSON(ctx, http.MethodPost, "/v1/beads/"+url.PathEscape(beadID)+"/taints", body, &bt); err != nil {
		return nil, err
	}
	return &bt, nil
}

func (c *HTTPClient) GetBeadTaints(ctx context.Context, beadID string) ([]*model.BeadTaint, error) {
	var resp struct {
		Taints []*model.BeadTaint `json:"taints"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/v1/beads/"+url.PathEscape(beadID)+"/taints", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Taints, nil
}

// RequestTaintGrant posts POST /v1/taints/{name}/grant-requests (ADR
// docs/adr/taints.md §9.2), returning the created escalation. The 202 body
// decodes via decodeResponse's EscalationCreated branch, which surfaces as an
// error value carrying the escalation details — unwrap it here into a plain
// return so `kd taint grant-request` doesn't have to know that convention.
func (c *HTTPClient) RequestTaintGrant(ctx context.Context, name string, req *RequestTaintGrantRequest) (*EscalationCreated, error) {
	if req == nil {
		req = &RequestTaintGrantRequest{}
	}
	path := "/v1/taints/" + url.PathEscape(name) + "/grant-requests"
	err := c.doJSON(ctx, http.MethodPost, path, req, nil)
	var esc *EscalationCreated
	if errors.As(err, &esc) {
		return esc, nil
	}
	return nil, err
}

func (c *HTTPClient) CreateScrubRequest(ctx context.Context, req *CreateScrubRequestRequest) (*ScrubRequestView, error) {
	resp := ScrubRequestView{Bead: &model.Bead{}}
	if err := c.doJSON(ctx, http.MethodPost, "/v1/scrub-requests", req, &resp); err != nil {
		return nil, fmt.Errorf("creating scrub-request for taint %s: %w", req.Taint, err)
	}
	return &resp, nil
}

// --- Config ---

func (c *HTTPClient) SetConfig(ctx context.Context, key string, value json.RawMessage) (*model.Config, error) {
	body := map[string]json.RawMessage{"value": value}
	var config model.Config
	if err := c.doJSON(ctx, http.MethodPut, "/v1/configs/"+key, body, &config); err != nil {
		return nil, err
	}
	return &config, nil
}

func (c *HTTPClient) GetConfig(ctx context.Context, key string) (*model.Config, error) {
	var config model.Config
	if err := c.doJSON(ctx, http.MethodGet, "/v1/configs/"+key, nil, &config); err != nil {
		return nil, err
	}
	return &config, nil
}

func (c *HTTPClient) ListConfigs(ctx context.Context, namespace string) ([]*model.Config, error) {
	var resp struct {
		Configs []*model.Config `json:"configs"`
	}
	path := "/v1/configs?namespace=" + url.QueryEscape(namespace)
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Configs, nil
}

func (c *HTTPClient) DeleteConfig(ctx context.Context, key string) error {
	return c.doJSON(ctx, http.MethodDelete, "/v1/configs/"+key, nil, nil)
}

// --- Hooks ---

func (c *HTTPClient) EmitHook(ctx context.Context, req *EmitHookRequest) (*EmitHookResponse, error) {
	var resp EmitHookResponse
	if err := c.doJSON(ctx, http.MethodPost, "/v1/hooks/emit", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// --- Gates ---

func (c *HTTPClient) ListGates(ctx context.Context, agentBeadID string) ([]model.GateRow, error) {
	var gates []model.GateRow
	if err := c.doJSON(ctx, http.MethodGet, "/v1/agents/"+url.PathEscape(agentBeadID)+"/gates", nil, &gates); err != nil {
		return nil, err
	}
	return gates, nil
}

func (c *HTTPClient) SatisfyGate(ctx context.Context, agentBeadID, gateID string) error {
	path := "/v1/agents/" + url.PathEscape(agentBeadID) + "/gates/" + url.PathEscape(gateID) + "/satisfy"
	return c.doJSON(ctx, http.MethodPost, path, nil, nil)
}

func (c *HTTPClient) ClearGate(ctx context.Context, agentBeadID, gateID string) error {
	path := "/v1/agents/" + url.PathEscape(agentBeadID) + "/gates/" + url.PathEscape(gateID)
	return c.doJSON(ctx, http.MethodDelete, path, nil, nil)
}

// --- Roster ---

func (c *HTTPClient) GetAgentRoster(ctx context.Context, staleThresholdSecs int) (*AgentRosterResponse, error) {
	path := fmt.Sprintf("/v1/agents/roster?stale_threshold_secs=%d", staleThresholdSecs)
	var resp AgentRosterResponse
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// --- Operators ---

func (c *HTTPClient) LookupOperatorByAlias(ctx context.Context, kind, subject string) (*LookupOperatorByAliasResponse, error) {
	path := "/v1/operators/by-alias?kind=" + url.QueryEscape(kind) + "&subject=" + url.QueryEscape(subject)
	var resp LookupOperatorByAliasResponse
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// --- Batch ---

func (c *HTTPClient) BatchGetBeads(ctx context.Context, ids []string) ([]BatchGetResult, error) {
	// Escape each ID individually so commas remain literal delimiters.
	escaped := make([]string, len(ids))
	for i, id := range ids {
		escaped[i] = url.QueryEscape(id)
	}
	path := "/v1/batch/beads?ids=" + strings.Join(escaped, ",")
	var resp struct {
		Results []BatchGetResult `json:"results"`
	}
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Results, nil
}

// --- Fleet Tokens ---

func (c *HTTPClient) RegisterFleetToken(ctx context.Context, token, fleet string) error {
	body := map[string]string{"token": token, "fleet": fleet}
	var resp struct {
		Fleet string `json:"fleet"`
	}
	return c.doJSON(ctx, http.MethodPost, "/v1/fleet-tokens", body, &resp)
}

// RegisterFleetTokenRequest mirrors the daemon's POST /v1/fleet-tokens body
// (`internal/server/http_fleet_tokens.go:registerBridgeTokenRequest`). Used by
// the `kd bridge token register` CLI (formerly `kd fleet token`, ka-0000005788)
// for the full C1.x option surface (TTL, subject, refresh, project/types/ops
// scopes). Type/field names stay `fleet`-shaped to match the on-wire JSON.
type RegisterFleetTokenRequest struct {
	Token        string   `json:"token"`
	Fleet        string   `json:"fleet"`
	Project      string   `json:"project,omitempty"`
	AllowedTypes []string `json:"allowed_types,omitempty"`
	AllowedOps   []string `json:"allowed_ops,omitempty"`
	TTL          string   `json:"ttl,omitempty"`
	ExpiresAt    string   `json:"expires_at,omitempty"`
	WithRefresh  bool     `json:"with_refresh,omitempty"`
	RefreshTTL   string   `json:"refresh_ttl,omitempty"`
	Subject      string   `json:"subject,omitempty"`
	// Privileged marks the token as a privileged service-fleet identity that
	// may bypass per-bead ownership for system-managed graph writes
	// (produces/triggered-by deps, mr_url back-fill). See ka-0000001946.
	Privileged bool `json:"privileged,omitempty"`
}

// RegisterFleetTokenResponse is the decoded JSON body returned by the daemon
// from POST /v1/fleet-tokens. RefreshToken / RefreshTokenExpiresAt are only
// populated when the request set WithRefresh=true.
type RegisterFleetTokenResponse struct {
	Fleet                 string   `json:"fleet"`
	Project               string   `json:"project,omitempty"`
	AllowedTypes          []string `json:"allowed_types,omitempty"`
	AllowedOps            []string `json:"allowed_ops,omitempty"`
	ExpiresAt             string   `json:"expires_at,omitempty"`
	Subject               string   `json:"subject,omitempty"`
	Privileged            bool     `json:"privileged,omitempty"`
	RefreshToken          string   `json:"refresh_token,omitempty"`
	RefreshTokenExpiresAt string   `json:"refresh_token_expires_at,omitempty"`
}

// RegisterFleetTokenFull POSTs the full RegisterFleetTokenRequest shape to
// /v1/fleet-tokens and returns the decoded response. Admin-only on the
// server side.
func (c *HTTPClient) RegisterFleetTokenFull(ctx context.Context, req *RegisterFleetTokenRequest) (*RegisterFleetTokenResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("RegisterFleetTokenFull: nil request")
	}
	var resp RegisterFleetTokenResponse
	if err := c.doJSON(ctx, http.MethodPost, "/v1/fleet-tokens", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *HTTPClient) ListFleetTokens(ctx context.Context) ([]string, error) {
	var resp struct {
		Fleets []string `json:"fleets"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/v1/fleet-tokens", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Fleets, nil
}

func (c *HTTPClient) RevokeFleetToken(ctx context.Context, fleet string) error {
	return c.doJSON(ctx, http.MethodDelete, "/v1/fleet-tokens/"+url.PathEscape(fleet), nil, nil)
}

// --- Semantic search ---

// FindSimilar queries GET /v1/beads/{id}/similar and returns the
// decoded response. limit defaults to 10 when <= 0; minScore <= 0 is sent
// as-is (server treats 0 as "no filter").
func (c *HTTPClient) FindSimilar(ctx context.Context, beadID string, limit int, minScore float64) (*FindSimilarResponse, error) {
	if limit <= 0 {
		limit = 10
	}
	q := url.Values{}
	q.Set("limit", strconv.Itoa(limit))
	if minScore > 0 {
		q.Set("min_score", strconv.FormatFloat(minScore, 'f', -1, 64))
	}
	path := "/v1/beads/" + url.PathEscape(beadID) + "/similar?" + q.Encode()
	var resp FindSimilarResponse
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// UpsertEmbedding posts a single bead's embedding to POST /v1/embeddings/upsert.
// Admin-only on the server side; used by the `kd embed sync` backfill flow.
func (c *HTTPClient) UpsertEmbedding(ctx context.Context, req *UpsertEmbeddingRequest) error {
	if req == nil {
		return fmt.Errorf("UpsertEmbedding: nil request")
	}
	return c.doJSON(ctx, http.MethodPost, "/v1/embeddings/upsert", req, nil)
}

// GetEmbeddingHashes posts a bead-ID list to POST /v1/embeddings/hashes and
// returns the stored text_hash per bead with an existing embedding row.
// Missing beads are omitted. Admin-only; used by embed-sync to skip beads
// whose text content has not changed since the last backfill.
func (c *HTTPClient) GetEmbeddingHashes(ctx context.Context, beadIDs []string) (map[string]string, error) {
	body := map[string][]string{"bead_ids": beadIDs}
	var resp struct {
		Hashes map[string]string `json:"hashes"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/v1/embeddings/hashes", body, &resp); err != nil {
		return nil, err
	}
	if resp.Hashes == nil {
		resp.Hashes = map[string]string{}
	}
	return resp.Hashes, nil
}

// SyncBatchEmbeddings posts a sync-batch request to POST /v1/embeddings/sync-batch.
// The server embeds one page of beads using its configured embedder and
// returns per-page stats. Admin-only; used by the `kd embed sync` backfill
// loop so the embedding API is only called from inside the cluster.
func (c *HTTPClient) SyncBatchEmbeddings(ctx context.Context, req *SyncBatchEmbeddingsRequest) (*SyncBatchEmbeddingsResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("SyncBatchEmbeddings: nil request")
	}
	var resp SyncBatchEmbeddingsResponse
	if err := c.doJSON(ctx, http.MethodPost, "/v1/embeddings/sync-batch", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// MigrateEmbeddings posts a destructive schema migration to
// POST /v1/embeddings/migrate. The server drops the ANN index, truncates
// bead_embeddings, and alters the embedding column to vector(req.Dims).
// Admin-only; used by `kd embed migrate`.
func (c *HTTPClient) MigrateEmbeddings(ctx context.Context, req *MigrateEmbeddingsRequest) error {
	if req == nil {
		return fmt.Errorf("MigrateEmbeddings: nil request")
	}
	return c.doJSON(ctx, http.MethodPost, "/v1/embeddings/migrate", req, nil)
}

// ReindexEmbeddings calls POST /v1/embeddings/reindex to re-create the ANN
// index after a migration+backfill. Admin-only.
func (c *HTTPClient) ReindexEmbeddings(ctx context.Context) error {
	return c.doJSON(ctx, http.MethodPost, "/v1/embeddings/reindex", struct{}{}, nil)
}

// ReextractReferences posts one page of the bead-references backfill to
// POST /v1/admin/reextract-references. The server walks beads with id >
// req.AfterID (LIMIT req.BatchSize), re-runs reference extraction on each
// bead's text fields and every comment, and replaces the matching rows in
// bead_references. Admin-only; used by `kd admin reextract-references`.
func (c *HTTPClient) ReextractReferences(ctx context.Context, req *ReextractReferencesRequest) (*ReextractReferencesResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("ReextractReferences: nil request")
	}
	var resp ReextractReferencesResponse
	if err := c.doJSON(ctx, http.MethodPost, "/v1/admin/reextract-references", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// BackfillSearchTSV posts one page of the search_tsv FTS backfill to
// POST /v1/admin/backfill-search-tsv. The server fills search_tsv on the next
// page of legacy beads whose search_tsv IS NULL (id > req.AfterID, LIMIT
// req.BatchSize). Admin-only; used by `kd admin backfill-search-tsv`
// (ka-0000003832 / ka-search relevance Option 2).
func (c *HTTPClient) BackfillSearchTSV(ctx context.Context, req *BackfillSearchTSVRequest) (*BackfillSearchTSVResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("BackfillSearchTSV: nil request")
	}
	var resp BackfillSearchTSVResponse
	if err := c.doJSON(ctx, http.MethodPost, "/v1/admin/backfill-search-tsv", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// SetStewardDelegation posts a tenant land's steward delegation to
// POST /v1/lands/{land}/steward-delegation. An empty ops revokes.
func (c *HTTPClient) SetStewardDelegation(ctx context.Context, land string, ops []string) error {
	if ops == nil {
		ops = []string{}
	}
	return c.doJSON(ctx, http.MethodPost, "/v1/lands/"+url.PathEscape(land)+"/steward-delegation",
		map[string]any{"operation_types": ops}, nil)
}

// BackfillCategory posts one page of the category materialization backfill to
// POST /v1/admin/backfill-category. The server re-stamps beads.category on the
// next page of legacy beads whose stored category disagrees with the current
// derive_bead_category rule table (id > req.AfterID, LIMIT req.BatchSize).
// Admin-only; used by `kd admin backfill-category` (ka-0000007274).
func (c *HTTPClient) BackfillCategory(ctx context.Context, req *BackfillCategoryRequest) (*BackfillCategoryResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("BackfillCategory: nil request")
	}
	var resp BackfillCategoryResponse
	if err := c.doJSON(ctx, http.MethodPost, "/v1/admin/backfill-category", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// BackfillProjectLabels posts one page of the project-label backfill to
// POST /v1/admin/backfill-project-labels. Admin-only; used by
// `kd admin backfill-project-labels`.
func (c *HTTPClient) BackfillProjectLabels(ctx context.Context, req *BackfillProjectLabelsRequest) (*BackfillProjectLabelsResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("BackfillProjectLabels: nil request")
	}
	var resp BackfillProjectLabelsResponse
	if err := c.doJSON(ctx, http.MethodPost, "/v1/admin/backfill-project-labels", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// BackfillOperators posts one run of the operators-FK backfill to
// POST /v1/admin/backfill-operators. Admin-only; used by
// `kd admin backfill-operators`. Implements P3 of
// docs/adr/human-attribution-identity-v2.md §4.3 — Pass 1 upserts
// operators + operator_aliases from DISTINCT operated_by; Pass 2
// rebinds each audit row's operated_by_id FK; verifier confirms no
// straggler rows remain.
func (c *HTTPClient) BackfillOperators(ctx context.Context, req *BackfillOperatorsRequest) (*BackfillOperatorsResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("BackfillOperators: nil request")
	}
	var resp BackfillOperatorsResponse
	if err := c.doJSON(ctx, http.MethodPost, "/v1/admin/backfill-operators", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// BackfillOperatedByLegacy posts one run of the legacy-operated-by cleanup
// to POST /v1/admin/backfill-operated-by-legacy. Admin-only; used by
// `kd admin backfill-operated-by-legacy` (ka-0000001880).
func (c *HTTPClient) BackfillOperatedByLegacy(ctx context.Context, req *BackfillOperatedByLegacyRequest) (*BackfillOperatedByLegacyResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("BackfillOperatedByLegacy: nil request")
	}
	var resp BackfillOperatedByLegacyResponse
	if err := c.doJSON(ctx, http.MethodPost, "/v1/admin/backfill-operated-by-legacy", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// BackfillSlackUserID posts one run of the thread-derived slack_user_id
// backfill to POST /v1/admin/backfill-slack-user-id. Admin-only; used by
// `kd admin backfill-slack-user-id` (ka-0000001582).
func (c *HTTPClient) BackfillSlackUserID(ctx context.Context, req *BackfillSlackUserIDRequest) (*BackfillSlackUserIDResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("BackfillSlackUserID: nil request")
	}
	var resp BackfillSlackUserIDResponse
	if err := c.doJSON(ctx, http.MethodPost, "/v1/admin/backfill-slack-user-id", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// BackfillOperatorSlugs posts one run of the op-<id> → readable slug backfill
// to POST /v1/admin/backfill-operator-slugs. Admin-only; used by
// `kd admin backfill-operator-slugs` (ka-0000001149).
func (c *HTTPClient) BackfillOperatorSlugs(ctx context.Context, req *BackfillOperatorSlugsRequest) (*BackfillOperatorSlugsResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("BackfillOperatorSlugs: nil request")
	}
	var resp BackfillOperatorSlugsResponse
	if err := c.doJSON(ctx, http.MethodPost, "/v1/admin/backfill-operator-slugs", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ForgetOperator posts one operator soft-delete to
// POST /v1/admin/operators/{id}/forget. Admin-only; used by
// `kd admin forget-operator` (ka-0000005150). A guard refusal surfaces as an
// *APIError (409) whose message names the audit-row count and threshold.
func (c *HTTPClient) ForgetOperator(ctx context.Context, id int64, req *ForgetOperatorRequest) (*ForgetOperatorResponse, error) {
	if id <= 0 {
		return nil, fmt.Errorf("ForgetOperator: invalid operator id %d", id)
	}
	if req == nil {
		req = &ForgetOperatorRequest{}
	}
	var resp ForgetOperatorResponse
	path := "/v1/admin/operators/" + strconv.FormatInt(id, 10) + "/forget"
	if err := c.doJSON(ctx, http.MethodPost, path, req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// BackfillOwner posts one run of the Matt-default owner rebind to
// POST /v1/admin/backfill-owner. Admin-only; used by
// `kd admin backfill-owner --rebind-matt-default` (ka-0000006961).
func (c *HTTPClient) BackfillOwner(ctx context.Context, req *BackfillOwnerRequest) (*BackfillOwnerResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("BackfillOwner: nil request")
	}
	var resp BackfillOwnerResponse
	if err := c.doJSON(ctx, http.MethodPost, "/v1/admin/backfill-owner", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// BackfillEmptyOwner posts one run of the three-phase empty-owner backfill
// to POST /v1/admin/backfill-empty-owner. Admin-only; used by
// `kd admin backfill-empty-owner` (ka-0000007029).
func (c *HTTPClient) BackfillEmptyOwner(ctx context.Context, req *BackfillEmptyOwnerRequest) (*BackfillEmptyOwnerResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("BackfillEmptyOwner: nil request")
	}
	var resp BackfillEmptyOwnerResponse
	if err := c.doJSON(ctx, http.MethodPost, "/v1/admin/backfill-empty-owner", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// BackfillOperatedByAgents posts one run of the per-user-homepage M2
// agent operated_by_id backfill to
// POST /v1/admin/backfill-operated-by-agents. Admin-only; used by
// `kd admin backfill-operated-by-agents` (sub-item #1 of ka-0000006817).
func (c *HTTPClient) BackfillOperatedByAgents(ctx context.Context, req *BackfillOperatedByAgentsRequest) (*BackfillOperatedByAgentsResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("BackfillOperatedByAgents: nil request")
	}
	var resp BackfillOperatedByAgentsResponse
	if err := c.doJSON(ctx, http.MethodPost, "/v1/admin/backfill-operated-by-agents", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// BackfillParentChildDeps posts one run of the agent-orphan parent-child
// reconstruction to POST /v1/admin/backfill-parent-child-deps. Admin-only;
// used by `kd admin backfill-parent-child-deps` (ka-0000006775).
func (c *HTTPClient) BackfillParentChildDeps(ctx context.Context, req *BackfillParentChildDepsRequest) (*BackfillParentChildDepsResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("BackfillParentChildDeps: nil request")
	}
	var resp BackfillParentChildDepsResponse
	if err := c.doJSON(ctx, http.MethodPost, "/v1/admin/backfill-parent-child-deps", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// RepairReversedParentChildDeps posts one run of the reversed
// epic->child parent-child edge audit/repair to
// POST /v1/admin/repair-reversed-parent-child-deps. Admin-only; used by
// `kd admin repair-reversed-parent-child-deps` (ka-0000006880).
func (c *HTTPClient) RepairReversedParentChildDeps(ctx context.Context, req *RepairReversedParentChildDepsRequest) (*RepairReversedParentChildDepsResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("RepairReversedParentChildDeps: nil request")
	}
	var resp RepairReversedParentChildDepsResponse
	if err := c.doJSON(ctx, http.MethodPost, "/v1/admin/repair-reversed-parent-child-deps", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// AuditBlocksCycles fetches the blocks-cycle audit from
// GET /v1/admin/audit-blocks-cycles. Admin-only; used by
// `kd admin audit-blocks-cycles` (ka-0000007984).
func (c *HTTPClient) AuditBlocksCycles(ctx context.Context) (*AuditBlocksCyclesResponse, error) {
	var resp AuditBlocksCyclesResponse
	if err := c.doJSON(ctx, http.MethodGet, "/v1/admin/audit-blocks-cycles", nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// BackfillTriggeredByDeps posts one run of the M4 auto-link backfill
// scanner to POST /v1/admin/backfill-triggered-by-deps. Admin-only;
// used by `kd admin backfill-triggered-by-deps` (ka-0000000004).
func (c *HTTPClient) BackfillTriggeredByDeps(ctx context.Context, req *BackfillTriggeredByDepsRequest) (*BackfillTriggeredByDepsResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("BackfillTriggeredByDeps: nil request")
	}
	var resp BackfillTriggeredByDepsResponse
	if err := c.doJSON(ctx, http.MethodPost, "/v1/admin/backfill-triggered-by-deps", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// CloseTriggerAuditBeads posts one run of the legacy trigger-audit close
// sweep to POST /v1/admin/close-trigger-audit-beads. Admin-only; used by
// `kd admin close-trigger-audit-beads` (ka-0000006341).
func (c *HTTPClient) CloseTriggerAuditBeads(ctx context.Context, req *CloseTriggerAuditBeadsRequest) (*CloseTriggerAuditBeadsResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("CloseTriggerAuditBeads: nil request")
	}
	var resp CloseTriggerAuditBeadsResponse
	if err := c.doJSON(ctx, http.MethodPost, "/v1/admin/close-trigger-audit-beads", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// SemanticSearch posts a semantic-search query to POST /v1/search/semantic and
// returns the scored results. Requires an embedder configured server-side;
// the server responds 503 when BEADS_EMBED_BACKEND=noop.
func (c *HTTPClient) SemanticSearch(ctx context.Context, req *SemanticSearchRequest) (*SemanticSearchResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("SemanticSearch: nil request")
	}
	var resp SemanticSearchResponse
	if err := c.doJSON(ctx, http.MethodPost, "/v1/search/semantic", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// --- Health ---

func (c *HTTPClient) Health(ctx context.Context) (string, error) {
	var resp struct {
		Status string `json:"status"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/v1/health", nil, &resp); err != nil {
		return "", err
	}
	return resp.Status, nil
}

// --- internal helpers ---

// APIError represents an error response from the server.
type APIError = beadswire.APIError

// OperatorRefusalErrorCode is the structured `code` value embedded in HTTP
// 400 bodies and gRPC InvalidArgument messages when the server's resolver
// fails closed because no human operator could be attributed to the
// request. The kd CLI keys on this code (not the human message) to exit
// with the EX_USAGE-family sentinel exit code 64 and emit its structured
// stderr payload. See ka-0000006731.
const OperatorRefusalErrorCode = "operator_refusal"

// OperatorRefusalError is returned by the client when the server's
// resolver fails closed on a write path. ResolverSource is the
// classifyOperatorRefusal label (header_missing, header_non_human,
// fallback_agent_only, chain_root_emailless) and identifies WHY
// attribution failed, so the agent harness can post a structured bug
// bead without leaking headers or tokens. See ka-0000006731.
type OperatorRefusalError struct {
	StatusCode     int
	Message        string
	ResolverSource string
}

func (e *OperatorRefusalError) Error() string {
	if e == nil {
		return "operator could not be resolved"
	}
	if e.ResolverSource == "" {
		return fmt.Sprintf("HTTP %d: %s", e.StatusCode, e.Message)
	}
	return fmt.Sprintf("HTTP %d: %s (resolver_source=%s)", e.StatusCode, e.Message, e.ResolverSource)
}

// DedupAckErrorCode is the structured `code` value embedded in the HTTP 409
// body when the dedup-ack gate (epic ka-0000004599) rejects a decision or
// escalation create because open beads of that type already exist in the
// project and the caller did not acknowledge the current open list. The kd
// CLI keys on this code to render the open list and the retry hint. See
// kbeads/docs/research-dedup-ack-gate.md.
const DedupAckErrorCode = "dedup_ack_required"

// DedupAckOpenBead is one entry in a DedupAckRequiredError's open list.
type DedupAckOpenBead struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	AgeSeconds int64  `json:"age_seconds"`
}

// DedupAckRequiredError is returned by the client when the server's dedup-ack
// gate rejects a decision/escalation create. It carries the readable open
// list, the opaque acknowledgement hash to echo on retry, and a
// copy-pasteable retry hint. The CLI renders these so an agent (or human) can
// look at the existing list and re-submit with --saw-open-<type>=<hash>.
type DedupAckRequiredError struct {
	StatusCode int
	Message    string
	BeadType   string             `json:"bead_type"`
	Project    string             `json:"project"`
	Hash       string             `json:"hash"`
	Open       []DedupAckOpenBead `json:"open"`
	Truncated  bool               `json:"truncated"`
	TotalOpen  int                `json:"total_open"`
	RetryWith  string             `json:"retry_with"`
}

func (e *DedupAckRequiredError) Error() string {
	if e == nil {
		return "dedup-ack required"
	}
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("HTTP %d: %d open %s(s) require acknowledgement in project %q",
		e.StatusCode, e.TotalOpen, e.BeadType, e.Project)
}

// TaintDenialError is returned by the client when a write is rejected by the
// server's taint write gate (ADR docs/adr/taints.md §9.4). AdvisoryText
// carries the server's exact templated wording (naming the bead, the missing
// taint(s), and the owner team(s)) — Error() returns it verbatim so a kd
// command that wraps this error with fmt.Errorf("...: %w", err) reproduces
// that text rather than a generic "not authorized" message. Discriminated in
// decodeResponse by a non-empty taint_names on the error body, mirroring how
// DedupAckRequiredError is discriminated by its `code` field.
type TaintDenialError struct {
	StatusCode      int
	Message         string   `json:"error"`
	AdvisoryText    string   `json:"advisory_text"`
	TaintNames      []string `json:"taint_names"`
	TaintOwnerTeams []string `json:"taint_owner_teams"`
	DeniedAction    string   `json:"denied_action"`
	BeadType        string   `json:"bead_type"`
}

func (e *TaintDenialError) Error() string {
	if e == nil {
		return "write denied: taint"
	}
	if e.AdvisoryText != "" {
		return e.AdvisoryText
	}
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("HTTP %d: write denied by taint gate", e.StatusCode)
}

// EscalationCreated is returned when the server responds with 202 Accepted
// after creating an escalation bead in response to an X-Escalate request.
type EscalationCreated = beadswire.EscalationCreated

// doJSON performs an HTTP request with optional JSON body and decodes the JSON response.
// If result is nil, the response body is discarded (for DELETE/204 responses).
//
// On a 401 carrying WWW-Authenticate: Bearer error="invalid_token",
// error_description="token expired" — and only when a refresh token is
// configured — doJSON transparently exchanges the refresh for a fresh
// access token and retries the request once. Concurrent expired-token
// responses funnel through a single exchange (refreshMu) so the daemon
// never sees a thundering herd of refresh requests after a stale-token
// outage.
func (c *HTTPClient) doJSON(ctx context.Context, method, path string, body any, result any) error {
	// Marshal the body once — we may need to send it twice (initial + retry).
	var bodyBytes []byte
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshaling request body: %w", err)
		}
		bodyBytes = data
	}

	respStatus, respBody, refreshHint, retryAfter, usedToken, err := c.sendOnceRetryConn(ctx, method, path, bodyBytes)
	if err != nil {
		return err
	}

	if c.shouldRefresh(respStatus, refreshHint) {
		if err := c.refreshAccessToken(ctx, usedToken); err != nil {
			// Fall through to surface the original 401 to the caller — a
			// failed refresh is no better than an expired access token, and
			// returning the refresh error directly would mask the auth
			// failure the operator likely cares about.
			return decodeResponse(respStatus, respBody, result)
		}
		respStatus, respBody, _, retryAfter, _, err = c.sendOnceRetryConn(ctx, method, path, bodyBytes)
		if err != nil {
			return err
		}
	} else if respStatus == http.StatusUnauthorized && c.rotateBearer(ctx, usedToken) {
		// A 401 the daemon's refresh path does not cover, and a bearer
		// refresher produced a new token: retry once. On a second 401 the
		// retry's response is what surfaces.
		respStatus, respBody, _, retryAfter, _, err = c.sendOnceRetryConn(ctx, method, path, bodyBytes)
		if err != nil {
			return err
		}
	}

	// Transient DB-auth retry (ka-0000006554): a 503 + Retry-After means the
	// daemon could not authenticate to Postgres, so the request changed
	// nothing and a bounded re-send recovers a write that would otherwise be
	// dropped. beadswire.IsTransientDBAuth leaves out the 503s without a
	// Retry-After and the statement-cancellation 503 (ka-0000000979).
	respStatus, respBody, err = beadswire.RetryDBAuth(ctx, c.dbAuthRetries, c.dbAuthRetryBackoff, c.sleep,
		respStatus, respBody, retryAfter, func() (int, []byte, string, error) {
			s, b, _, ra, _, err := c.sendOnce(ctx, method, path, bodyBytes)
			return s, b, ra, err
		})
	if err != nil {
		return err
	}

	return decodeResponse(respStatus, respBody, result)
}

// sendOnce performs a single HTTP round-trip and returns the status, body,
// the WWW-Authenticate header (when present), the Retry-After header (when
// present — the daemon sets it on a transient DB-auth 503, ka-0000006554), and
// the access token actually used on the wire. The used-token is what
// refreshAccessToken needs to make its single-flight bail decision: a peer
// goroutine has already refreshed iff c.token has moved past this value.
func (c *HTTPClient) sendOnce(ctx context.Context, method, path string, bodyBytes []byte) (int, []byte, string, string, string, error) {
	var bodyReader io.Reader
	if bodyBytes != nil {
		bodyReader = bytes.NewReader(bodyBytes)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return 0, nil, "", "", "", fmt.Errorf("creating request: %w", err)
	}
	if bodyBytes != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c.tokenMu.RLock()
	tok := c.token
	c.tokenMu.RUnlock()
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	if c.fleet != "" {
		req.Header.Set("X-Fleet", c.fleet)
	}
	if c.actor != "" {
		req.Header.Set("X-Actor", c.actor)
	}
	if c.operatedBy != "" {
		req.Header.Set("X-Operated-By", c.operatedBy)
	}
	if EscalateFromContext(ctx) {
		req.Header.Set("X-Escalate", "true")
	}
	if ForceCloseFromContext(ctx) {
		req.Header.Set("X-Force-Close", "true")
	}
	if NoAutoParentFromContext(ctx) {
		req.Header.Set("X-No-Auto-Parent", "1")
	}
	if ForceParentEdgeFromContext(ctx) {
		req.Header.Set("X-Force-Parent-Edge", "1")
	}
	if tb := TriggerBeadFromContext(ctx); tb != "" {
		req.Header.Set("X-Trigger-Bead", tb)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, nil, "", "", "", fmt.Errorf("performing request: %w", err)
	}
	defer resp.Body.Close()

	// Observe the daemon's build version (ka-0000002965) before branching on
	// status — the header rides every response, including 204s and errors.
	c.noteDaemonVersion(resp.Header.Get("X-Kbeads-Version"))

	wwwAuth := resp.Header.Get("WWW-Authenticate")
	retryAfter := resp.Header.Get("Retry-After")

	// Advisory warnings (X-Beads-Warning, e.g. "short_title missing: …") ride
	// a successful write; hand them to the caller's collector, if any.
	if resp.StatusCode < 400 {
		noteWarnings(ctx, resp.Header.Values(WarningHeader))
	}

	if resp.StatusCode == http.StatusNoContent {
		return resp.StatusCode, nil, wwwAuth, retryAfter, tok, nil
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, "", "", "", fmt.Errorf("reading response: %w", err)
	}
	// When the edge bounced the request to an interactive sign-in page,
	// surface the "run `gb login`" sentinel instead of letting the caller
	// try to JSON-decode a login HTML page.
	if rerr := avareauth.DetectFromResponse(resp, respBody); rerr != nil {
		return 0, nil, "", "", "", rerr
	}
	return resp.StatusCode, respBody, wwwAuth, retryAfter, tok, nil
}

// decodeResponse maps a status+body pair to either one of kd's typed
// refusals (OperatorRefusalError, DedupAckRequiredError, TaintDenialError) or
// the shared decoding in beadswire.DecodeResponse (APIError,
// EscalationCreated, or a successful unmarshal into result).
func decodeResponse(status int, respBody []byte, result any) error {
	if status >= 400 {
		var errResp struct {
			Error          string   `json:"error"`
			Code           string   `json:"code"`
			ResolverSource string   `json:"resolver_source"`
			TaintNames     []string `json:"taint_names"`
		}
		if json.Unmarshal(respBody, &errResp) == nil && errResp.Error != "" {
			if errResp.Code == OperatorRefusalErrorCode {
				return &OperatorRefusalError{
					StatusCode:     status,
					Message:        errResp.Error,
					ResolverSource: errResp.ResolverSource,
				}
			}
			if errResp.Code == DedupAckErrorCode {
				// Re-decode the full structured 409 body into the typed error
				// so the CLI can render the open list + retry hint.
				dedup := DedupAckRequiredError{StatusCode: status, Message: errResp.Error}
				_ = json.Unmarshal(respBody, &dedup)
				return &dedup
			}
			if len(errResp.TaintNames) > 0 {
				// Taint write-gate denial (ADR docs/adr/taints.md §9.4) — carries
				// advisory_text/taint_names/taint_owner_teams, not just `code`.
				taintDenial := TaintDenialError{StatusCode: status}
				_ = json.Unmarshal(respBody, &taintDenial)
				return &taintDenial
			}
		}
	}
	return beadswire.DecodeResponse(status, respBody, result)
}

// shouldRefresh returns true when the response is the daemon's "your
// access token is expired, refresh it" signal AND we have a refresh token
// to exchange.
func (c *HTTPClient) shouldRefresh(status int, wwwAuth string) bool {
	if status != http.StatusUnauthorized {
		return false
	}
	if !beadswire.IsExpiredAccessTokenChallenge(wwwAuth) {
		return false
	}
	c.tokenMu.RLock()
	defer c.tokenMu.RUnlock()
	return c.refreshToken != ""
}

// rotateBearer asks the bearer refresher (SetBearerRefresher) for a
// replacement token after staleToken was answered 401 and installs it. It
// reports whether a retry with a different token is worth making. Serialised
// by refreshMu with a single-flight check, so concurrent 401s share one call.
func (c *HTTPClient) rotateBearer(ctx context.Context, staleToken string) bool {
	c.tokenMu.RLock()
	fn := c.bearerRefresher
	c.tokenMu.RUnlock()
	if fn == nil {
		return false
	}
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	c.tokenMu.RLock()
	current := c.token
	c.tokenMu.RUnlock()
	if current != staleToken && current != "" {
		return true // a peer already rotated it; retry with that
	}
	tok, err := fn(ctx)
	if err != nil || tok == "" || tok == staleToken {
		return false
	}
	c.tokenMu.Lock()
	c.token = tok
	c.tokenMu.Unlock()
	return true
}

// refreshAccessToken exchanges the current refresh token for a new access
// token (and, when the server rotates refreshes, a new refresh token too)
// and swaps the result onto the client.
//
// The exchange runs under refreshMu so concurrent 401s share one network
// round-trip. staleToken is the access token that was on the wire for the
// failing request; once we acquire refreshMu we compare against the current
// token — if a peer already swapped it past staleToken, we skip the
// exchange and let the retry use the freshly-installed token.
func (c *HTTPClient) refreshAccessToken(ctx context.Context, staleToken string) error {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()

	// Single-flight: bail if a peer goroutine refreshed while we waited.
	c.tokenMu.RLock()
	current := c.token
	refresh := c.refreshToken
	c.tokenMu.RUnlock()
	if current != staleToken && current != "" {
		return nil
	}
	if refresh == "" {
		return fmt.Errorf("no refresh token configured")
	}

	rr, err := beadswire.ExchangeRefreshToken(ctx, c.httpClient, c.baseURL, refresh)
	if err != nil {
		return err
	}

	c.tokenMu.Lock()
	c.token = rr.AccessToken
	if rr.Rotated && rr.RefreshToken != "" {
		c.refreshToken = rr.RefreshToken
	}
	cb := c.onRefresh
	c.tokenMu.Unlock()
	if cb != nil {
		cb(rr.AccessToken, rr.RefreshToken)
	}
	return nil
}
