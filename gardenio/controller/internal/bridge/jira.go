// Package bridge provides the JIRA REST API v3 HTTP client.
//
// JiraClient wraps JIRA Cloud REST API methods needed by the jira-bridge:
// search, get issue, transition, comment, and remote link operations.
// It uses basic auth (email:apiToken) and returns typed Go structs.
package bridge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/httpjson"
)

// JiraClient is an HTTP client for the JIRA REST API v3.
type JiraClient struct {
	baseURL    string
	authHeader string
	httpClient *http.Client
	logger     *slog.Logger

	// readOnly, when true, makes every write method (TransitionIssue,
	// AddComment*, AddRemoteLink, AddLabels, SetAffectsVersions, AssignIssue)
	// fail closed with ErrJiraReadOnly WITHOUT issuing a request. It is the
	// client-layer leg of the dark-Jira write-token fence (ka-0000005242):
	// belt-and-suspenders over the fenced JIRA_API_TOKEN (pod layer) and the
	// denied Atlassian egress (network layer). Read methods are unaffected.
	readOnly bool
}

// ErrJiraReadOnly is returned by every JiraClient write method when the client
// was constructed read-only (JiraClientConfig.ReadOnly). It exists so a dark
// agent's attempt to mutate real upstream Jira fails loudly and locally, rather
// than being silently attempted and blocked only downstream by the fenced token
// or the egress deny (ka-0000005242).
var ErrJiraReadOnly = errors.New("jira client is read-only: writes to upstream Jira are fenced in dark mode (ka-0000005242)")

// JiraClientConfig holds configuration for creating a JiraClient.
type JiraClientConfig struct {
	BaseURL  string // e.g., "https://acme.atlassian.net"
	Email    string
	APIToken string
	Logger   *slog.Logger

	// ReadOnly makes every write method fail closed with ErrJiraReadOnly. Set
	// by callers that must never mutate upstream Jira — the dark-agent gb path
	// (ka-0000005242). Read methods (search, get, thread) still work.
	ReadOnly bool
}

// NewJiraClient creates a new JIRA REST API client.
func NewJiraClient(cfg JiraClientConfig) *JiraClient {
	auth := base64.StdEncoding.EncodeToString([]byte(cfg.Email + ":" + cfg.APIToken))
	return &JiraClient{
		baseURL:    strings.TrimRight(cfg.BaseURL, "/"),
		authHeader: "Basic " + auth,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		logger:     cfg.Logger,
		readOnly:   cfg.ReadOnly,
	}
}

// ReadOnly reports whether this client fails writes closed (see readOnly).
func (c *JiraClient) ReadOnly() bool {
	return c != nil && c.readOnly
}

// guardWrite returns ErrJiraReadOnly (logging the refused op) when the client is
// read-only, and nil otherwise. Every write method calls it FIRST, before any
// request is built, so a fenced write never touches the network.
func (c *JiraClient) guardWrite(op, key string) error {
	if c == nil || !c.readOnly {
		return nil
	}
	if c.logger != nil {
		c.logger.Warn("refusing Jira write in read-only (dark) mode",
			"op", op, "key", key)
	}
	return fmt.Errorf("%s %s: %w", op, key, ErrJiraReadOnly)
}

// JiraIssue represents a JIRA issue from the REST API.
type JiraIssue struct {
	Key    string          `json:"key"`
	ID     string          `json:"id"`
	Fields JiraIssueFields `json:"fields"`

	// RawFields preserves the unparsed "fields" object so callers can read
	// custom fields (customfield_*) that are not modeled by JiraIssueFields.
	RawFields map[string]json.RawMessage `json:"-"`
}

// UnmarshalJSON decodes a JIRA issue into both the typed Fields struct and the
// untyped RawFields map. The map keeps every field returned by the API
// (including customfield_* entries) so the poller can promote configured
// custom fields onto beads without modeling each one explicitly.
func (i *JiraIssue) UnmarshalJSON(data []byte) error {
	var raw struct {
		Key    string                     `json:"key"`
		ID     string                     `json:"id"`
		Fields map[string]json.RawMessage `json:"fields"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	i.Key = raw.Key
	i.ID = raw.ID
	i.RawFields = raw.Fields

	if len(raw.Fields) > 0 {
		fieldsJSON, err := json.Marshal(raw.Fields)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(fieldsJSON, &i.Fields); err != nil {
			return err
		}
	}
	return nil
}

// JiraIssueFields contains the fields of a JIRA issue.
type JiraIssueFields struct {
	Summary     string           `json:"summary"`
	Description json.RawMessage  `json:"description"` // ADF format
	Status      *JiraNamedRef    `json:"status"`
	IssueType   *JiraNamedRef    `json:"issuetype"`
	Priority    *JiraNamedRef    `json:"priority"`
	Reporter    *JiraUser        `json:"reporter"`
	Assignee    *JiraUser        `json:"assignee"`
	Labels      []string         `json:"labels"`
	Parent      *JiraParentRef   `json:"parent"` // epic link
	Created     string           `json:"created"`
	Updated     string           `json:"updated"`
	IssueLinks  []JiraIssueLink  `json:"issuelinks"`
	Attachments []JiraAttachment `json:"attachment"`
}

// JiraNamedRef is a JIRA object with a name field (status, priority, issuetype).
type JiraNamedRef struct {
	Name string `json:"name"`
}

// JiraUser represents a JIRA user.
type JiraUser struct {
	DisplayName string `json:"displayName"`
	AccountID   string `json:"accountId"`
	// EmailAddress is present when the JIRA instance/app has permission to
	// expose it (Jira Cloud omits it for privacy-restricted profiles). It is
	// the reliable key for mapping a reporter to a fleet user bead
	// (Keycloak-provisioned beads carry email, almost never the opaque
	// accountId) — see lookupJiraUserIdentity (ka-0000001781).
	EmailAddress string `json:"emailAddress"`
}

// JiraParentRef is a reference to a parent issue (epic).
type JiraParentRef struct {
	Key    string `json:"key"`
	Fields struct {
		Summary string `json:"summary"`
	} `json:"fields"`
}

// JiraIssueLink represents a link between two JIRA issues.
type JiraIssueLink struct {
	Type         JiraLinkType   `json:"type"`
	InwardIssue  *JiraLinkedRef `json:"inwardIssue,omitempty"`
	OutwardIssue *JiraLinkedRef `json:"outwardIssue,omitempty"`
}

// JiraLinkType describes the semantics of an issue link.
type JiraLinkType struct {
	Name    string `json:"name"`
	Inward  string `json:"inward"`
	Outward string `json:"outward"`
}

// JiraLinkedRef is a minimal reference to a linked issue.
type JiraLinkedRef struct {
	Key string `json:"key"`
}

// JiraAttachment represents a JIRA issue attachment.
type JiraAttachment struct {
	ID        string `json:"id"`
	Filename  string `json:"filename"`
	MimeType  string `json:"mimeType"`
	Size      int64  `json:"size"`
	Content   string `json:"content"`
	Thumbnail string `json:"thumbnail"`
	Created   string `json:"created"`
}

// jiraTransition represents a JIRA issue transition.
type jiraTransition struct {
	ID   string        `json:"id"`
	Name string        `json:"name"`
	To   *JiraNamedRef `json:"to"`
}

// SearchIssues searches JIRA issues using JQL (single page).
func (c *JiraClient) SearchIssues(ctx context.Context, jql string, fields []string, maxResults int) ([]JiraIssue, error) {
	page, err := c.SearchIssuesPage(ctx, jql, fields, maxResults, "")
	if err != nil {
		return nil, err
	}
	return page.Issues, nil
}

// JiraSearchPage holds a page of JIRA search results with cursor token.
type JiraSearchPage struct {
	Issues        []JiraIssue
	NextPageToken string // empty when no more pages
}

// SearchIssuesPage fetches a single page of JIRA search results.
// Pass an empty pageToken for the first page.
func (c *JiraClient) SearchIssuesPage(ctx context.Context, jql string, fields []string, maxResults int, pageToken string) (*JiraSearchPage, error) {
	q := url.Values{}
	q.Set("jql", jql)
	if len(fields) > 0 {
		q.Set("fields", strings.Join(fields, ","))
	}
	if maxResults > 0 {
		q.Set("maxResults", fmt.Sprintf("%d", maxResults))
	}
	if pageToken != "" {
		q.Set("nextPageToken", pageToken)
	}

	var result struct {
		Issues        []JiraIssue `json:"issues"`
		NextPageToken string      `json:"nextPageToken"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/rest/api/3/search/jql?"+q.Encode(), nil, &result); err != nil {
		return nil, fmt.Errorf("JIRA search: %w", err)
	}
	return &JiraSearchPage{
		Issues:        result.Issues,
		NextPageToken: result.NextPageToken,
	}, nil
}

// ApproximateCount returns the estimated number of issues matching jql via
// POST /rest/api/3/search/approximate-count. This is the cheap, single-call
// way to get a count: the post-410 /search/jql endpoint no longer returns a
// "total" field, and paging the full result set to count is far more
// rate-limit-expensive (and exposes the pagination path). The endpoint returns
// an estimate ({"count": <int>}), which is fine for the trend gauges the
// metrics collector publishes. A JQL *syntax* error returns HTTP 400 (surfaced
// as a *JiraAPIError); note that unknown field/enum names silently return
// count 0 / HTTP 200, so a non-error 0 does not prove the JQL is meaningful.
func (c *JiraClient) ApproximateCount(ctx context.Context, jql string) (int64, error) {
	var result struct {
		Count int64 `json:"count"`
	}
	body := map[string]any{"jql": jql}
	if err := c.doJSON(ctx, http.MethodPost, "/rest/api/3/search/approximate-count", body, &result); err != nil {
		return 0, fmt.Errorf("JIRA approximate-count: %w", err)
	}
	return result.Count, nil
}

// GetIssue fetches a single JIRA issue by key.
func (c *JiraClient) GetIssue(ctx context.Context, key string) (*JiraIssue, error) {
	var issue JiraIssue
	if err := c.doJSON(ctx, http.MethodGet, "/rest/api/3/issue/"+url.PathEscape(key), nil, &issue); err != nil {
		return nil, fmt.Errorf("JIRA get issue %s: %w", key, err)
	}
	return &issue, nil
}

// TransitionIssue transitions a JIRA issue to the named status.
// It fetches available transitions and finds one matching transitionName.
func (c *JiraClient) TransitionIssue(ctx context.Context, key, transitionName string) error {
	if err := c.guardWrite("transition", key); err != nil {
		return err
	}
	// Get available transitions.
	var result struct {
		Transitions []jiraTransition `json:"transitions"`
	}
	path := "/rest/api/3/issue/" + url.PathEscape(key) + "/transitions"
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &result); err != nil {
		return fmt.Errorf("JIRA get transitions for %s: %w", key, err)
	}

	// Find matching transition.
	var transitionID string
	for _, t := range result.Transitions {
		if strings.EqualFold(t.Name, transitionName) || strings.EqualFold(t.To.Name, transitionName) {
			transitionID = t.ID
			break
		}
	}
	if transitionID == "" {
		return fmt.Errorf("JIRA transition %q not available for %s", transitionName, key)
	}

	// Execute transition.
	body := map[string]any{
		"transition": map[string]string{"id": transitionID},
	}
	if err := c.doJSON(ctx, http.MethodPost, path, body, nil); err != nil {
		return fmt.Errorf("JIRA transition %s to %s: %w", key, transitionName, err)
	}
	return nil
}

// AddComment adds a comment to a JIRA issue in Atlassian Document Format.
//
// This posts a plain comment with no JSM visibility property set. On a Jira
// Service Management project the resulting visibility is the project's default,
// which can be customer-facing — prefer AddCommentWithVisibility on any path
// where the internal/external distinction matters (the field allowlist gates
// public comments per role; see jira_allowlist.go).
func (c *JiraClient) AddComment(ctx context.Context, key, text string) error {
	if err := c.guardWrite("add comment", key); err != nil {
		return err
	}
	body := map[string]any{
		"body": adfParagraph(text),
	}
	path := "/rest/api/3/issue/" + url.PathEscape(key) + "/comment"
	if err := c.doJSON(ctx, http.MethodPost, path, body, nil); err != nil {
		return fmt.Errorf("JIRA add comment to %s: %w", key, err)
	}
	return nil
}

// JiraCommentEntry is one issue comment returned by GetComments. Body is
// json.RawMessage because Jira Server returns a plain string while Jira Cloud
// returns an ADF object — use jiraCommentBodyToText to flatten either.
type JiraCommentEntry struct {
	ID      string          `json:"id"`
	Author  JiraUser        `json:"author"`
	Body    json.RawMessage `json:"body"`
	Created string          `json:"created"`
}

// GetComments fetches up to maxResults of the most recent comments on an
// issue, newest first. Used to hand spawned/nudged agents the ticket's
// conversation history (F4, plan ka-0000007401).
func (c *JiraClient) GetComments(ctx context.Context, key string, maxResults int) ([]JiraCommentEntry, error) {
	if maxResults <= 0 {
		maxResults = 20
	}
	path := fmt.Sprintf("/rest/api/3/issue/%s/comment?maxResults=%d&orderBy=-created",
		url.PathEscape(key), maxResults)
	var result struct {
		Comments []JiraCommentEntry `json:"comments"`
	}
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &result); err != nil {
		return nil, fmt.Errorf("JIRA get comments for %s: %w", key, err)
	}
	return result.Comments, nil
}

// AddCommentWithVisibility adds a comment and explicitly stamps the JSM
// internal-vs-public distinction via the sd.public.comment comment property.
// On a JSM project, {"internal": true} keeps the note staff-only while
// {"internal": false} makes it a customer-facing reply. On a non-JSM project the
// property is simply ignored by Jira, so passing CommentInternal is always safe.
//
// The bridge routes governed comment writes through here so the field allowlist
// can gate public replies per role: autonomous agents post internal notes only
// (epic ka-0000004904; read-only-mirror advice ka-0000003234).
func (c *JiraClient) AddCommentWithVisibility(ctx context.Context, key, text string, vis CommentVisibility) error {
	return c.postCommentDoc(ctx, key, adfParagraph(text), vis)
}

// jiraReplySourceLinkText is the display text of the anchor paragraph a
// source-linked reply prepends before its body (see
// AddCommentWithVisibilityAndSourceLink).
const jiraReplySourceLinkText = "↩ In reply to this comment"

// AddCommentWithVisibilityAndSourceLink posts a visibility-stamped comment
// like AddCommentWithVisibility, prepending a paragraph that links back to the
// source comment (the @gardenio mention that spawned the acting agent) via the
// browse-page ?focusedCommentId=<id> anchor.
//
// Jira Cloud REST exposes NO comment threading for any project type (no
// parentId on comment create — G4 finding, ka-0000001458), so this visual
// anchor is the closest available substitute (G4b, ka-0000002864). An empty
// sourceCommentID — or a client with no base URL — degrades to the exact flat
// single-paragraph comment AddCommentWithVisibility posts.
func (c *JiraClient) AddCommentWithVisibilityAndSourceLink(ctx context.Context, key, text string, vis CommentVisibility, sourceCommentID string) error {
	doc := adfParagraph(text)
	if href := c.commentAnchorURL(key, sourceCommentID); href != "" {
		doc = adfDocParagraphs(
			[]any{adfLinkText(jiraReplySourceLinkText, href)},
			adfInlineText(text),
		)
	}
	return c.postCommentDoc(ctx, key, doc, vis)
}

// AddMarkdownCommentWithVisibilityAndSourceLink posts a visibility-stamped,
// optionally source-linked comment like AddCommentWithVisibilityAndSourceLink,
// but first renders the body's markdown to rich ADF (bold, lists, code blocks,
// links — H1, epic ka-0000007401) so an agent's `gb jira reply` reads like a
// Slack reply instead of literal asterisks.
//
// Rendering is best-effort: any markdown construct without an ADF mapping (or
// an empty render) degrades to the EXACT plain-text comment the non-markdown
// variant posts, so this path can never be less readable than before. The
// write-policy gate, visibility property, and G4b anchor semantics are
// identical to the plain variant — this only changes how the body is typeset.
func (c *JiraClient) AddMarkdownCommentWithVisibilityAndSourceLink(ctx context.Context, key, text string, vis CommentVisibility, sourceCommentID string) error {
	blocks, ok := adfBlocksFromMarkdown(text)
	if !ok {
		return c.AddCommentWithVisibilityAndSourceLink(ctx, key, text, vis, sourceCommentID)
	}
	if href := c.commentAnchorURL(key, sourceCommentID); href != "" {
		linkPara := map[string]any{
			"type":    "paragraph",
			"content": []any{adfLinkText(jiraReplySourceLinkText, href)},
		}
		blocks = append([]any{linkPara}, blocks...)
	}
	doc := map[string]any{"version": 1, "type": "doc", "content": blocks}
	return c.postCommentDoc(ctx, key, doc, vis)
}

// postCommentDoc posts an ADF comment document with the JSM
// internal-vs-public property stamped (see AddCommentWithVisibility).
func (c *JiraClient) postCommentDoc(ctx context.Context, key string, doc map[string]any, vis CommentVisibility) error {
	if err := c.guardWrite("add comment", key); err != nil {
		return err
	}
	body := map[string]any{
		"body": doc,
		"properties": []any{
			map[string]any{
				"key":   "sd.public.comment",
				"value": map[string]any{"internal": vis != CommentPublic},
			},
		},
	}
	path := "/rest/api/3/issue/" + url.PathEscape(key) + "/comment"
	if err := c.doJSON(ctx, http.MethodPost, path, body, nil); err != nil {
		return fmt.Errorf("JIRA add %s comment to %s: %w", vis, key, err)
	}
	return nil
}

// commentAnchorURL returns the browse URL for key focused on the given
// comment (<baseURL>/browse/<KEY>?focusedCommentId=<id>), or "" when the
// client has no base URL or either part is empty. Like the issue key in
// BrowseURL, the comment id is external DATA and is query-escaped defensively.
func (c *JiraClient) commentAnchorURL(key, commentID string) string {
	base := c.BrowseURL(key)
	commentID = strings.TrimSpace(commentID)
	if base == "" || commentID == "" {
		return ""
	}
	return base + "?focusedCommentId=" + url.QueryEscape(commentID)
}

// BrowseURL returns the human-facing Jira browse URL for an issue key
// (baseURL + "/browse/" + key), or "" when the client has no base URL configured or
// the key is empty. The key is path-escaped defensively so an untrusted key cannot
// break out of the path — the outbox treats the FSM target key as untrusted DATA
// (epic ka-0000002067).
//
// This is DISPLAY metadata only: it builds a URL string, it is NOT a write and never
// issues a request against the target issue. It is used to render a clickable link to
// the FSM ticket INSIDE a GBFSM ticket's Description (the GBFSM→FSM half of the 1:1
// bidirectional link, Phase 3 ka-0000001070). Rendering a link *to* FSM in a GBFSM
// ticket is a pure GBFSM write — it does not touch FSM, so the ZERO-FSM-write
// invariant is untouched.
func (c *JiraClient) BrowseURL(key string) string {
	key = strings.TrimSpace(key)
	if c == nil || c.baseURL == "" || key == "" {
		return ""
	}
	return c.baseURL + "/browse/" + url.PathEscape(key)
}

// AddRemoteLink adds a remote link to a JIRA issue.
func (c *JiraClient) AddRemoteLink(ctx context.Context, key, linkURL, title string) error {
	if err := c.guardWrite("add remote link", key); err != nil {
		return err
	}
	body := map[string]any{
		"object": map[string]any{
			"url":   linkURL,
			"title": title,
		},
	}
	path := "/rest/api/3/issue/" + url.PathEscape(key) + "/remotelink"
	if err := c.doJSON(ctx, http.MethodPost, path, body, nil); err != nil {
		return fmt.Errorf("JIRA add remote link to %s: %w", key, err)
	}
	return nil
}

// JiraRemoteLink represents a remote link on a JIRA issue (e.g., GitLab MR).
type JiraRemoteLink struct {
	ID     int `json:"id"`
	Object struct {
		URL    string `json:"url"`
		Title  string `json:"title"`
		Status *struct {
			Resolved bool `json:"resolved"`
			Icon     *struct {
				Title string `json:"title"`
			} `json:"icon"`
		} `json:"status"`
	} `json:"object"`
}

// GetRemoteLinks fetches all remote links for a JIRA issue.
func (c *JiraClient) GetRemoteLinks(ctx context.Context, key string) ([]JiraRemoteLink, error) {
	var links []JiraRemoteLink
	path := "/rest/api/3/issue/" + url.PathEscape(key) + "/remotelink"
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &links); err != nil {
		return nil, fmt.Errorf("JIRA get remote links for %s: %w", key, err)
	}
	return links, nil
}

// AddLabels adds labels to a JIRA issue, preserving existing labels.
func (c *JiraClient) AddLabels(ctx context.Context, key string, labels []string) error {
	if err := c.guardWrite("add labels", key); err != nil {
		return err
	}
	updates := make([]map[string]string, len(labels))
	for i, l := range labels {
		updates[i] = map[string]string{"add": l}
	}
	body := map[string]any{
		"update": map[string]any{
			"labels": updates,
		},
	}
	path := "/rest/api/3/issue/" + url.PathEscape(key)
	if err := c.doJSON(ctx, http.MethodPut, path, body, nil); err != nil {
		return fmt.Errorf("JIRA add labels to %s: %w", key, err)
	}
	return nil
}

// SetAffectsVersions additively adds the named Affects-Version(s) to a JIRA
// issue, preserving any versions already set. It mirrors AddLabels' additive PUT
// shape ({"update":{"versions":[{"add":{"name":v}}, ...]}}) so a triage write is
// idempotent: re-adding a version already present is a no-op on Jira's side.
//
// The version name must ALREADY exist in the project (as a released or
// unreleased version) — Jira rejects an unknown version name rather than
// creating it, and that error is surfaced here; the bridge never auto-creates a
// version. This is the Affects-Version leg of the fsm-triage bridge path
// (ka-0000003103 / operator Option A): the ONLY versions write surface, gated
// behind the field allowlist (FieldVersions) like every other governed write.
// Empty / all-blank names is a no-op (no request sent).
func (c *JiraClient) SetAffectsVersions(ctx context.Context, key string, names []string) error {
	if err := c.guardWrite("set affects-versions", key); err != nil {
		return err
	}
	adds := make([]map[string]any, 0, len(names))
	for _, n := range names {
		if n = strings.TrimSpace(n); n != "" {
			adds = append(adds, map[string]any{"add": map[string]any{"name": n}})
		}
	}
	if len(adds) == 0 {
		return nil
	}
	body := map[string]any{
		"update": map[string]any{
			"versions": adds,
		},
	}
	path := "/rest/api/3/issue/" + url.PathEscape(key)
	if err := c.doJSON(ctx, http.MethodPut, path, body, nil); err != nil {
		return fmt.Errorf("JIRA set affects-versions on %s: %w", key, err)
	}
	return nil
}

// AssignIssue sets the assignee of a JIRA issue by account ID.
func (c *JiraClient) AssignIssue(ctx context.Context, key, accountID string) error {
	if err := c.guardWrite("assign issue", key); err != nil {
		return err
	}
	body := map[string]any{
		"accountId": accountID,
	}
	path := "/rest/api/3/issue/" + url.PathEscape(key) + "/assignee"
	if err := c.doJSON(ctx, http.MethodPut, path, body, nil); err != nil {
		return fmt.Errorf("JIRA assign issue %s: %w", key, err)
	}
	return nil
}

// DownloadAttachment fetches a JIRA attachment by URL and writes it to w.
// The contentURL is the full URL from JiraAttachment.Content.
func (c *JiraClient) DownloadAttachment(ctx context.Context, contentURL string, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, contentURL, nil)
	if err != nil {
		return fmt.Errorf("create download request: %w", err)
	}
	req.Header.Set("Authorization", c.authHeader)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("download request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("download returned status %d", resp.StatusCode)
	}

	if _, err := io.Copy(w, resp.Body); err != nil {
		return fmt.Errorf("writing attachment: %w", err)
	}
	return nil
}

// JiraFieldMeta describes a JIRA field returned by the /field endpoint.
type JiraFieldMeta struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ListFields fetches all field definitions (system + custom) so the poller can
// resolve customfield_* ids to human-readable names. Returns a map keyed by
// field id (e.g. "customfield_11976" -> "URL").
func (c *JiraClient) ListFields(ctx context.Context) (map[string]string, error) {
	var fields []JiraFieldMeta
	if err := c.doJSON(ctx, http.MethodGet, "/rest/api/3/field", nil, &fields); err != nil {
		return nil, fmt.Errorf("JIRA list fields: %w", err)
	}
	out := make(map[string]string, len(fields))
	for _, f := range fields {
		if f.ID != "" {
			out[f.ID] = f.Name
		}
	}
	return out, nil
}

// CustomFieldString extracts a human-readable string value from a raw JIRA
// custom-field value. JIRA custom fields come in several shapes:
//   - url / text fields: a bare JSON string
//   - select / option fields: an object with a "value" key
//   - cascading or user fields: an object with "name"/"displayName"
//
// It returns the best available scalar representation and a bool indicating
// whether a non-empty value was found. Array and deeply-nested structured
// values are not flattened (returns false) to keep bead fields scalar.
func CustomFieldString(raw json.RawMessage) (string, bool) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "", false
	}

	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		s = strings.TrimSpace(s)
		return s, s != ""
	}

	var obj struct {
		Value       string `json:"value"`
		Name        string `json:"name"`
		DisplayName string `json:"displayName"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		for _, v := range []string{obj.Value, obj.DisplayName, obj.Name} {
			if v = strings.TrimSpace(v); v != "" {
				return v, true
			}
		}
	}

	return "", false
}

// JiraAPIError is returned by doJSON for any HTTP >= 400 response. It preserves
// the legacy error string verbatim via Error() so existing callers and tests
// that match on the message keep working, while exposing the status code and a
// parsed Retry-After so the metrics collector can implement 429 backoff without
// re-parsing the message.
type JiraAPIError struct {
	Method     string
	Path       string
	StatusCode int
	Body       string
	// RetryAfter is the parsed Retry-After header (0 if absent/unparseable).
	RetryAfter time.Duration
}

func (e *JiraAPIError) Error() string {
	return fmt.Sprintf("JIRA API %s %s returned %d: %s", e.Method, e.Path, e.StatusCode, e.Body)
}

// asJiraAPIError unwraps err to a *JiraAPIError, or returns (nil, false) if the
// chain holds no such error.
func asJiraAPIError(err error) (*JiraAPIError, bool) {
	var apiErr *JiraAPIError
	if errors.As(err, &apiErr) {
		return apiErr, true
	}
	return nil, false
}

// parseRetryAfter parses the Retry-After header value, which JIRA Cloud sends
// as an integer number of seconds. The HTTP-date form is not used by Atlassian
// for rate limiting, so only the delta-seconds form is honored; anything else
// yields 0 (no explicit backoff hint).
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	return 0
}

// doJSON performs an HTTP request against the JIRA API with JSON body/response.
func (c *JiraClient) doJSON(ctx context.Context, method, path string, body any, result any) error {
	err := httpjson.Do(ctx, c.httpClient, httpjson.Request{
		Method: method,
		URL:    c.baseURL + path,
		Header: http.Header{"Authorization": {c.authHeader}},
		Body:   body,
	}, result)
	var se *httpjson.StatusError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &se):
		return &JiraAPIError{
			Method:     method,
			Path:       path,
			StatusCode: se.StatusCode,
			Body:       truncate(se.Body, 512),
			RetryAfter: parseRetryAfter(se.Header.Get("Retry-After")),
		}
	default:
		return fmt.Errorf("JIRA %w", err)
	}
}

// adfParagraph creates a minimal ADF document with a single paragraph.
//
// Newlines in text are promoted to ADF hardBreak nodes so multi-line comment
// bodies render as distinct lines in Jira instead of collapsing to a single run
// (a literal "\n" inside one ADF text node renders as a space). This keeps an
// appended "Acted by <operator>" attribution line (jira_attribution.go) visibly
// on its own line in the issue history.
func adfParagraph(text string) map[string]any {
	return adfDocParagraphs(adfInlineText(text))
}

// adfInlineText renders plain text into a slice of ADF inline nodes, promoting
// newlines to hardBreak nodes (see adfParagraph). Factored out so callers that
// need to append additional inline nodes (e.g. a clickable link mark) after the
// text can reuse the exact same line-splitting behavior.
func adfInlineText(text string) []any {
	lines := strings.Split(text, "\n")
	content := make([]any, 0, len(lines)*2-1)
	for i, line := range lines {
		if i > 0 {
			content = append(content, map[string]any{"type": "hardBreak"})
		}
		// An empty text node is invalid ADF; a blank line is represented by the
		// surrounding hardBreaks alone, so only emit a text node for non-empty
		// segments.
		if line != "" {
			content = append(content, map[string]any{
				"type": "text",
				"text": line,
			})
		}
	}
	// Guard against a fully-empty body producing an empty paragraph content
	// array (also invalid ADF): fall back to a single empty-safe text node.
	if len(content) == 0 {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	return content
}

// adfDocParagraphs wraps one or more slices of inline ADF nodes as sibling paragraphs
// in a minimal ADF document. Each argument becomes its own <paragraph> block (so
// callers can prepend a link paragraph before the body paragraph); a nil/empty inline
// slice is skipped. Called with a single argument it produces the same single-paragraph
// document adfParagraph always has.
func adfDocParagraphs(paragraphs ...[]any) map[string]any {
	blocks := make([]any, 0, len(paragraphs))
	for _, content := range paragraphs {
		if len(content) == 0 {
			continue
		}
		blocks = append(blocks, map[string]any{
			"type":    "paragraph",
			"content": content,
		})
	}
	return map[string]any{
		"version": 1,
		"type":    "doc",
		"content": blocks,
	}
}

// adfLinkText returns an ADF text node whose text carries a "link" mark pointing at
// href, so it renders as a clickable hyperlink in Jira. Used to make the FSM target
// key a one-click link inside a GBFSM ticket's Description (Phase 3 ka-0000001070).
func adfLinkText(text, href string) map[string]any {
	return map[string]any{
		"type": "text",
		"text": text,
		"marks": []any{
			map[string]any{
				"type":  "link",
				"attrs": map[string]any{"href": href},
			},
		},
	}
}

// adfMediaSingle returns an ADF mediaSingle block that renders an attached
// file inline in a Description or comment (ka-0000001730).
//
// attachmentID is the id returned by AddAttachment. ADF media keys on the
// attachment id, not a URL — which is exactly why an auth-gated
// `<viewer>/beads/image/<key>` link can never render here and an attachment
// can: the bytes live on the issue, so Jira serves them to whoever can see it.
//
// layout "center" is the full-width single-image presentation; Jira also
// accepts wide/full-width/align-start/align-end, but center is the right
// default for before/after evidence a reviewer is meant to actually look at.
func adfMediaSingle(attachmentID string) map[string]any {
	return map[string]any{
		"type":  "mediaSingle",
		"attrs": map[string]any{"layout": "center"},
		"content": []any{
			map[string]any{
				"type": "media",
				"attrs": map[string]any{
					"id":   attachmentID,
					"type": "file",
					// Jira Cloud resolves the collection from the issue when it
					// is omitted; sending an empty string instead makes it 400.
				},
			},
		},
	}
}

// adfToMarkdown converts an Atlassian Document Format JSON blob to markdown.
// Handles paragraph, text, heading, bulletList, orderedList, codeBlock,
// and hardBreak nodes. Unknown nodes are silently skipped.
func adfToMarkdown(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	var doc struct {
		Content []adfNode `json:"content"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return ""
	}

	var sb strings.Builder
	for _, node := range doc.Content {
		renderADFNode(&sb, node, 0)
	}
	return strings.TrimSpace(sb.String())
}

// adfNode is a recursive ADF content node.
type adfNode struct {
	Type    string          `json:"type"`
	Text    string          `json:"text,omitempty"`
	Content []adfNode       `json:"content,omitempty"`
	Attrs   json.RawMessage `json:"attrs,omitempty"`
	Marks   []adfMark       `json:"marks,omitempty"`
}

// adfMark represents an inline mark (bold, italic, code, link).
type adfMark struct {
	Type  string          `json:"type"`
	Attrs json.RawMessage `json:"attrs,omitempty"`
}

func renderADFNode(sb *strings.Builder, node adfNode, depth int) {
	switch node.Type {
	case "doc":
		for _, child := range node.Content {
			renderADFNode(sb, child, depth)
		}

	case "paragraph":
		renderADFInline(sb, node.Content)
		sb.WriteString("\n\n")

	case "heading":
		level := 1
		if len(node.Attrs) > 0 {
			var attrs struct {
				Level int `json:"level"`
			}
			if json.Unmarshal(node.Attrs, &attrs) == nil && attrs.Level > 0 {
				level = attrs.Level
			}
		}
		sb.WriteString(strings.Repeat("#", level) + " ")
		renderADFInline(sb, node.Content)
		sb.WriteString("\n\n")

	case "bulletList":
		for _, item := range node.Content {
			if item.Type == "listItem" {
				sb.WriteString("- ")
				for i, child := range item.Content {
					if i > 0 {
						sb.WriteString("  ")
					}
					renderADFInline(sb, child.Content)
				}
				sb.WriteString("\n")
			}
		}
		sb.WriteString("\n")

	case "orderedList":
		for i, item := range node.Content {
			if item.Type == "listItem" {
				sb.WriteString(fmt.Sprintf("%d. ", i+1))
				for j, child := range item.Content {
					if j > 0 {
						sb.WriteString("   ")
					}
					renderADFInline(sb, child.Content)
				}
				sb.WriteString("\n")
			}
		}
		sb.WriteString("\n")

	case "codeBlock":
		lang := ""
		if len(node.Attrs) > 0 {
			var attrs struct {
				Language string `json:"language"`
			}
			if json.Unmarshal(node.Attrs, &attrs) == nil {
				lang = attrs.Language
			}
		}
		sb.WriteString("```" + lang + "\n")
		renderADFInline(sb, node.Content)
		sb.WriteString("\n```\n\n")

	case "blockquote":
		for _, child := range node.Content {
			sb.WriteString("> ")
			renderADFInline(sb, child.Content)
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
	}
}

func renderADFInline(sb *strings.Builder, nodes []adfNode) {
	for _, n := range nodes {
		switch n.Type {
		case "text":
			text := n.Text
			// Apply marks (bold, italic, code).
			for _, mark := range n.Marks {
				switch mark.Type {
				case "strong":
					text = "**" + text + "**"
				case "em":
					text = "_" + text + "_"
				case "code":
					text = "`" + text + "`"
				case "link":
					var attrs struct {
						Href string `json:"href"`
					}
					if json.Unmarshal(mark.Attrs, &attrs) == nil && attrs.Href != "" {
						text = "[" + text + "](" + attrs.Href + ")"
					}
				}
			}
			sb.WriteString(text)
		case "hardBreak":
			sb.WriteString("\n")
		case "mention":
			// ADF renders an @mention as a node whose display text rides in
			// attrs.text (older payloads carry it on the node's text field).
			// Jira does NOT reliably include the leading "@" — a native user
			// @-mention often arrives as the bare display name ("Gardenio Agent"
			// rather than "@gardenio", confirmed on the real prod GBFSM-85
			// payload) — so normalize to a guaranteed single leading "@" via
			// mentionNodeText. This keeps the trigger token (e.g. "@gardenio")
			// present in the markdown so the inbound mention path can extract
			// the instruction after it (H3, epic ka-0000007401). Without this
			// the node was either silently dropped or emitted without its "@".
			sb.WriteString(mentionNodeText(n))
		case "inlineCard":
			if len(n.Attrs) > 0 {
				var attrs struct {
					URL string `json:"url"`
				}
				if json.Unmarshal(n.Attrs, &attrs) == nil && attrs.URL != "" {
					sb.WriteString(attrs.URL)
				}
			}
		}
	}
}

// MapJiraPriority maps JIRA priority names to bead priority integers.
// Highest/Critical → 0, High → 1, Medium → 2, Low/Lowest → 3.
func MapJiraPriority(name string) int {
	switch strings.ToLower(name) {
	case "highest", "critical", "blocker":
		return 0
	case "high":
		return 1
	case "medium":
		return 2
	case "low", "lowest", "trivial":
		return 3
	default:
		return 2
	}
}

// MapJiraLinkType maps JIRA issue link type names to canonical dependency types.
func MapJiraLinkType(name string) string {
	switch strings.ToLower(name) {
	case "blocks":
		return "blocks"
	case "relates":
		return "relates"
	case "duplicate":
		return "duplicate"
	case "action item":
		return "action-item"
	case "escalate":
		return "escalate"
	case "cloners":
		return "clones"
	default:
		return "jira-link"
	}
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
