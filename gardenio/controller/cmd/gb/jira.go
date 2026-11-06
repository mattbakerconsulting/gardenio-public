package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

var jiraCmd = &cobra.Command{
	Use:     "jira",
	Short:   "JIRA ticket commands",
	Long:    `Fetch and display JIRA tickets. Requires JIRA_BASE_URL, JIRA_EMAIL, and JIRA_API_TOKEN env vars.`,
	GroupID: "orchestration",
}

var jiraShowCmd = &cobra.Command{
	Use:   "show <ticket-id>",
	Short: "Fetch and display a JIRA ticket",
	Long: `Fetches a JIRA ticket and outputs a structured summary including title,
description, status, assignee, labels, and linked issues.

Two access paths, tried in order:

  1. Direct JIRA REST: when JIRA_BASE_URL, JIRA_EMAIL, and JIRA_API_TOKEN are
     all set, reads ANY ticket via REST API v3 and includes its comments. These
     creds are NOT universal in agent pods — they come from per-PROJECT secrets
     declared on the project bead, so an agent only has them when its project
     wires them (e.g. monorepo, devsecops). On a project without them, these
     vars are legitimately UNSET and only path 2 is available.
  2. Bridge-proxied (agent pod): when JIRA_BRIDGE_URL is set, reads the agent's
     OWN bound ticket through the jira-bridge — no raw JIRA_* creds needed. Only
     pods spawned FROM a JIRA ticket (jira-mention) carry a binding; a generic
     agent has none, so this path returns a 403 "no jira_key binding". Reflects
     operator edits made after spawn; the ticket-body analog of
     "gb jira thread". Comments are not included — use "gb jira thread".

If NEITHER path can serve the ticket, you cannot read it from this pod — state a
verification gap rather than assuming its contents.

Environment variables:
  JIRA_BASE_URL   - e.g. https://acme.atlassian.net (direct path; per-project)
  JIRA_EMAIL      - JIRA account email (direct path; per-project)
  JIRA_API_TOKEN  - JIRA API token (direct path; per-project)
  JIRA_BRIDGE_URL - jira-bridge base URL (agent-pod path, injected by the controller)`,
	Args: cobra.ExactArgs(1),
	RunE: runJiraShow,
}

func runJiraShow(cmd *cobra.Command, args []string) error {
	ticketID := strings.ToUpper(strings.TrimSpace(args[0]))

	// Prefer direct JIRA REST when the full operator credentials are present
	// (operator laptop): this path can read ANY ticket and includes comments.
	baseURL := os.Getenv("JIRA_BASE_URL")
	email := os.Getenv("JIRA_EMAIL")
	apiToken := os.Getenv("JIRA_API_TOKEN")
	if baseURL != "" && email != "" && apiToken != "" {
		return runJiraShowDirect(cmd, ticketID, baseURL, email, apiToken)
	}

	// Fallback: an agent pod carries no raw JIRA_* creds, but the jira-bridge
	// can serve the agent's OWN bound ticket credential-free (feature G6, the
	// ticket-body analog of `gb jira thread`). Only the bound ticket is
	// readable this way; reflects operator edits made after spawn.
	if os.Getenv("JIRA_BRIDGE_URL") != "" {
		return runJiraShowViaBridge(ticketID)
	}

	return fmt.Errorf("no JIRA access: set JIRA_BASE_URL, JIRA_EMAIL, and JIRA_API_TOKEN (operator), or JIRA_BRIDGE_URL (agent pod)")
}

// runJiraShowDirect reads a ticket straight from the JIRA REST API using the
// operator's Basic-auth credentials. It can read any ticket and fetches
// comments alongside the issue.
func runJiraShowDirect(cmd *cobra.Command, ticketID, baseURL, email, apiToken string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client := &jiraHTTPClient{
		baseURL:    strings.TrimRight(baseURL, "/"),
		authHeader: "Basic " + base64.StdEncoding.EncodeToString([]byte(email+":"+apiToken)),
		http:       &http.Client{Timeout: 30 * time.Second},
	}

	// Fetch the issue.
	issue, err := client.getIssue(ctx, ticketID)
	if err != nil {
		return fmt.Errorf("fetching %s: %w", ticketID, err)
	}

	// Fetch comments.
	maxComments, _ := cmd.Flags().GetInt("max-comments")
	comments, err := client.getComments(ctx, ticketID, maxComments)
	if err != nil {
		// Non-fatal — show issue without comments.
		comments = nil
	}

	if jsonOutput {
		out := map[string]any{
			"key":    issue.Key,
			"fields": issue.Fields,
		}
		if comments != nil {
			out["comments"] = comments
		}
		printJSON(out)
		return nil
	}

	printJiraTicket(issue, comments)
	return nil
}

// jiraBridgeIssueResult mirrors the bridge's GET /api/jira/issue response body
// (internal/bridge/jira_issue_read.go jiraIssueResponse). The description
// arrives pre-rendered as markdown.
type jiraBridgeIssueResult struct {
	OK          bool                `json:"ok"`
	JiraKey     string              `json:"jira_key,omitempty"`
	Summary     string              `json:"summary,omitempty"`
	Description string              `json:"description,omitempty"`
	Status      string              `json:"status,omitempty"`
	IssueType   string              `json:"issue_type,omitempty"`
	Priority    string              `json:"priority,omitempty"`
	Assignee    string              `json:"assignee,omitempty"`
	Reporter    string              `json:"reporter,omitempty"`
	Labels      []string            `json:"labels,omitempty"`
	Parent      *jiraBridgeIssueRef `json:"parent,omitempty"`
	Created     string              `json:"created,omitempty"`
	Updated     string              `json:"updated,omitempty"`
	BrowseURL   string              `json:"browse_url,omitempty"`
	Links       []jiraBridgeLink    `json:"links,omitempty"`
	Error       string              `json:"error,omitempty"`
}

type jiraBridgeIssueRef struct {
	Key     string `json:"key"`
	Summary string `json:"summary,omitempty"`
}

type jiraBridgeLink struct {
	Relationship string `json:"relationship"`
	Key          string `json:"key"`
}

// runJiraShowViaBridge reads the acting agent's bound ticket through the
// jira-bridge (no raw JIRA_* creds required). The endpoint is bound-ticket
// scoped, so it can only return the agent's own ticket — if the caller asked
// for a different key, that is an explicit error rather than a silent swap.
func runJiraShowViaBridge(ticketID string) error {
	bridgeURL := os.Getenv("JIRA_BRIDGE_URL")

	u := strings.TrimRight(bridgeURL, "/") + "/api/jira/issue?agent=" + url.QueryEscape(actor)
	resp, err := jiraBridgeHTTPClient().Get(u)
	if err != nil {
		return fmt.Errorf("fetching issue: %w", err)
	}
	defer resp.Body.Close()

	var result jiraBridgeIssueResult
	if decErr := json.NewDecoder(resp.Body).Decode(&result); decErr != nil && resp.StatusCode == http.StatusOK {
		return fmt.Errorf("decoding response: %w", decErr)
	}
	if resp.StatusCode != http.StatusOK {
		// The bridge's 403 "no jira_key binding" is honest but cryptic: it means
		// this pod was NOT spawned from a JIRA ticket, so the bridge has no
		// ticket bound to it to read. Without raw JIRA_* creds (path 1), there
		// is no way to read an arbitrary ticket from here. Expand the message so
		// the agent states a verification gap instead of retrying a dead route
		// or silently assuming the ticket's contents (bug ka-0000000090).
		if resp.StatusCode == http.StatusForbidden && strings.Contains(result.Error, "jira_key binding") {
			return fmt.Errorf("cannot read %s from this pod: it was not spawned from a JIRA ticket, "+
				"so the jira-bridge has no bound ticket to serve, and this project injects no direct "+
				"JIRA_BASE_URL/JIRA_EMAIL/JIRA_API_TOKEN creds (those are per-project — only some projects "+
				"wire them). Report a verification gap for %s rather than guessing its contents", ticketID, ticketID)
		}
		if result.Error != "" {
			return fmt.Errorf("bridge returned HTTP %d: %s", resp.StatusCode, result.Error)
		}
		return fmt.Errorf("bridge returned HTTP %d", resp.StatusCode)
	}

	// Bound-ticket scoping: the bridge only ever returns the agent's own
	// ticket. Refuse to masquerade another key's output as the requested one.
	if ticketID != "" && result.JiraKey != "" && !strings.EqualFold(ticketID, result.JiraKey) {
		return fmt.Errorf("in an agent pod, gb jira show can only read your own bound ticket %s (requested %s); use gb jira thread for its conversation", result.JiraKey, ticketID)
	}

	if jsonOutput {
		printJSON(result)
		return nil
	}

	printJiraBridgeIssue(&result)
	return nil
}

// printJiraBridgeIssue renders a bridge-proxied issue read. Comments are not
// included (the bridge issue endpoint returns the ticket body only); point the
// reader at gb jira thread for the live conversation.
func printJiraBridgeIssue(r *jiraBridgeIssueResult) {
	fmt.Printf("# %s: %s\n\n", r.JiraKey, r.Summary)
	fmt.Printf("%-12s %s\n", "Type:", orNone(r.IssueType))
	fmt.Printf("%-12s %s\n", "Status:", orNone(r.Status))
	fmt.Printf("%-12s %s\n", "Priority:", orNone(r.Priority))
	fmt.Printf("%-12s %s\n", "Assignee:", orUnassigned(r.Assignee))
	fmt.Printf("%-12s %s\n", "Reporter:", orUnassigned(r.Reporter))
	if len(r.Labels) > 0 {
		fmt.Printf("%-12s %s\n", "Labels:", strings.Join(r.Labels, ", "))
	}
	if r.Parent != nil {
		fmt.Printf("%-12s %s — %s\n", "Parent:", r.Parent.Key, r.Parent.Summary)
	}
	if r.Created != "" {
		fmt.Printf("%-12s %s\n", "Created:", formatJiraTime(r.Created))
	}
	if r.Updated != "" {
		fmt.Printf("%-12s %s\n", "Updated:", formatJiraTime(r.Updated))
	}
	if r.BrowseURL != "" {
		fmt.Printf("%-12s %s\n", "URL:", r.BrowseURL)
	}

	if len(r.Links) > 0 {
		fmt.Printf("\n## Linked Issues\n\n")
		for _, link := range r.Links {
			fmt.Printf("  %s %s\n", link.Relationship, link.Key)
		}
	}

	if desc := strings.TrimSpace(r.Description); desc != "" {
		fmt.Printf("\n## Description\n\n%s\n", desc)
	} else {
		fmt.Printf("\n## Description\n\n(no description)\n")
	}

	fmt.Printf("\n(comments not shown — run `gb jira thread` for the ticket conversation)\n")
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func orUnassigned(s string) string {
	if s == "" {
		return "(unassigned)"
	}
	return s
}

// ── jira reply ─────────────────────────────────────────────────────

var jiraReplyCmd = &cobra.Command{
	Use:   "reply <message>",
	Short: "Post a comment on the JIRA ticket this agent was spawned from",
	Long: `Posts a comment on the originating JIRA ticket via the jira-bridge.
Reads JIRA_KEY and JIRA_BRIDGE_URL from the environment (injected by the
controller into JIRA-spawned agent pods).

Markdown in the message (bold, lists, code blocks, links, headings) renders
as rich text in Jira; anything the renderer cannot map posts as plain text.

Comments are INTERNAL notes by default (staff-only on JSM projects). Pass
--public to request a customer-facing reply — the bridge still gates public
comments per role, and no role is granted them by default.

After posting, records a best-effort type=message bead (label jira-reply) and
stamps triggered-by lineage to the agent bead so the reply is discoverable
from beads-viewer. Bead failures never fail the JIRA post.`,
	Args: cobra.ExactArgs(1),
	RunE: runJiraReply,
}

// jiraReplyRequest is the wire request for POST /api/jira/reply on the bridge.
type jiraReplyRequest struct {
	Agent      string `json:"agent"`
	JiraKey    string `json:"jira_key"`
	Text       string `json:"text"`
	Visibility string `json:"visibility"`
}

// jiraReplyResult mirrors the bridge's response body.
type jiraReplyResult struct {
	OK      bool   `json:"ok"`
	JiraKey string `json:"jira_key,omitempty"`
	Error   string `json:"error,omitempty"`
}

func runJiraReply(cmd *cobra.Command, args []string) error {
	bridgeURL, jiraKey, err := resolveJiraReplyEnv()
	if err != nil {
		return err
	}

	visibility := "internal"
	if public, _ := cmd.Flags().GetBool("public"); public {
		visibility = "public"
	}

	payload := jiraReplyRequest{
		Agent:      actor,
		JiraKey:    jiraKey,
		Text:       args[0],
		Visibility: visibility,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshaling request: %w", err)
	}

	u := strings.TrimRight(bridgeURL, "/") + "/api/jira/reply"
	resp, err := jiraBridgeHTTPClient().Post(u, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("posting reply: %w", err)
	}
	defer resp.Body.Close()

	var result jiraReplyResult
	if decErr := json.NewDecoder(resp.Body).Decode(&result); decErr != nil && resp.StatusCode == http.StatusOK {
		return fmt.Errorf("decoding response: %w", decErr)
	}
	if resp.StatusCode != http.StatusOK {
		if result.Error != "" {
			return fmt.Errorf("bridge returned HTTP %d: %s", resp.StatusCode, result.Error)
		}
		return fmt.Errorf("bridge returned HTTP %d", resp.StatusCode)
	}

	// Best-effort: record the reply as a type=message bead so it is reachable
	// from the agent graph in beads-viewer, mirroring recordSlackReplyBead.
	// Bead failures never fail the reply itself — the JIRA comment has already
	// been posted.
	recordJiraReplyBead(cmd.Context(), args[0], jiraKey, visibility)

	if jsonOutput {
		printJSON(result)
	} else {
		fmt.Println("Reply posted.")
	}
	return nil
}

// ── jira thread ────────────────────────────────────────────────────

var jiraThreadCmd = &cobra.Command{
	Use:   "thread",
	Short: "Fetch the conversation on the JIRA ticket this agent was spawned from",
	Long: `Fetches recent comments on the originating JIRA ticket via the jira-bridge
and prints them oldest-first as markdown for context injection (--json for raw).

Reads JIRA_BRIDGE_URL from the environment (injected by the controller into
JIRA-spawned agent pods); the ticket is the agent's own binding — the bridge
resolves it from the agent bead, so no JIRA credentials are needed in the pod.
Use this to re-read the ticket conversation mid-session; the history in your
spawn prompt is a snapshot and goes stale as people keep commenting.`,
	RunE: runJiraThread,
}

// jiraThreadComment mirrors the bridge's thread-read comment shape.
type jiraThreadComment struct {
	ID      string `json:"id"`
	Author  string `json:"author"`
	Created string `json:"created"`
	Text    string `json:"text"`
}

// jiraThreadResult mirrors the bridge's response body for GET /api/jira/thread.
type jiraThreadResult struct {
	OK       bool                `json:"ok"`
	JiraKey  string              `json:"jira_key,omitempty"`
	Comments []jiraThreadComment `json:"comments,omitempty"`
	Error    string              `json:"error,omitempty"`
}

func runJiraThread(cmd *cobra.Command, args []string) error {
	bridgeURL := os.Getenv("JIRA_BRIDGE_URL")
	if bridgeURL == "" {
		return fmt.Errorf("JIRA_BRIDGE_URL is not set — cannot reach the JIRA bridge")
	}

	u := strings.TrimRight(bridgeURL, "/") + "/api/jira/thread?agent=" + url.QueryEscape(actor)
	if limit, _ := cmd.Flags().GetInt("limit"); limit > 0 {
		u += fmt.Sprintf("&limit=%d", limit)
	}

	resp, err := jiraBridgeHTTPClient().Get(u)
	if err != nil {
		return fmt.Errorf("fetching thread: %w", err)
	}
	defer resp.Body.Close()

	var result jiraThreadResult
	if decErr := json.NewDecoder(resp.Body).Decode(&result); decErr != nil && resp.StatusCode == http.StatusOK {
		return fmt.Errorf("decoding response: %w", decErr)
	}
	if resp.StatusCode != http.StatusOK {
		if result.Error != "" {
			return fmt.Errorf("bridge returned HTTP %d: %s", resp.StatusCode, result.Error)
		}
		return fmt.Errorf("bridge returned HTTP %d", resp.StatusCode)
	}

	if jsonOutput {
		printJSON(result)
		return nil
	}

	if len(result.Comments) == 0 {
		fmt.Printf("No comments on %s yet.\n", result.JiraKey)
		return nil
	}
	fmt.Printf("# %s conversation (%d comments, oldest first)\n\n", result.JiraKey, len(result.Comments))
	for _, c := range result.Comments {
		fmt.Printf("**%s**", c.Author)
		if c.Created != "" {
			fmt.Printf(" (%s)", formatJiraTime(c.Created))
		}
		fmt.Printf(":\n%s\n\n", c.Text)
	}
	return nil
}

// resolveJiraReplyEnv reads and validates the env vars gb jira reply needs.
// Falls back to the agent bead's jira_key field when JIRA_KEY is not set
// (mirrors resolveSlackEnv's bead fallback for non-thread-bound agents).
func resolveJiraReplyEnv() (bridgeURL, jiraKey string, err error) {
	bridgeURL = os.Getenv("JIRA_BRIDGE_URL")
	if bridgeURL == "" {
		return "", "", fmt.Errorf("JIRA_BRIDGE_URL is not set — cannot reach the JIRA bridge")
	}
	jiraKey = os.Getenv("JIRA_KEY")
	if jiraKey != "" {
		return bridgeURL, jiraKey, nil
	}

	// Fallback: look up the binding from the agent bead. The mention handler
	// stamps jira_key on the agent bead at spawn, so even agents started
	// before the env injection shipped can discover their ticket.
	beadID := os.Getenv("BOAT_AGENT_BEAD_ID")
	if beadID == "" || daemon == nil {
		return "", "", fmt.Errorf("no JIRA context: JIRA_KEY is not set and BOAT_AGENT_BEAD_ID is unavailable")
	}
	bead, err := daemon.GetBead(context.Background(), beadID)
	if err != nil {
		return "", "", fmt.Errorf("no JIRA context: failed to look up agent bead %s: %w", beadID, err)
	}
	jiraKey = bead.Fields["jira_key"]
	if jiraKey == "" {
		return "", "", fmt.Errorf("no JIRA context — agent bead %s has no jira_key binding", beadID)
	}
	os.Setenv("JIRA_KEY", jiraKey) // cache for subsequent calls in this process
	return bridgeURL, jiraKey, nil
}

// jiraBridgeHTTPClient returns an HTTP client for jira-bridge calls. When
// JIRA_BRIDGE_TOKEN is set, requests carry `Authorization: Bearer <token>`
// (same pattern as slackHTTPClient); empty token preserves local-dev behavior.
func jiraBridgeHTTPClient() *http.Client {
	c := &http.Client{Timeout: 15 * time.Second}
	if tok := os.Getenv("JIRA_BRIDGE_TOKEN"); tok != "" {
		c.Transport = &bearerTokenTransport{token: tok, base: http.DefaultTransport}
	}
	return c
}

// recordJiraReplyBead creates a type=message bead recording an outbound
// `gb jira reply` post and stamps the same triggered-by lineage that
// gb squawk / gb slack reply use (agent bead, trigger bead, human fallback).
//
// The bead carries label `jira-reply` plus `from:<actor>`. Failures are
// reported on stderr but never fail the caller — the JIRA comment has already
// been posted. Skips silently when the beads daemon is unreachable.
func recordJiraReplyBead(ctx context.Context, text, jiraKey, visibility string) {
	if daemon == nil {
		return
	}

	title := text
	if len(title) > 500 {
		title = title[:497] + "..."
	}

	fields := map[string]string{
		"source_agent": actor,
		"text":         text,
		"jira_key":     jiraKey,
		"visibility":   visibility,
	}
	fieldsJSON, err := json.Marshal(fields)
	if err != nil {
		fmt.Fprintf(os.Stderr, "jira reply: warn: failed to marshal bead fields: %v\n", err)
		return
	}

	beadID, err := daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:     title,
		Type:      "message",
		Kind:      "data",
		Labels:    []string{"jira-reply", fmt.Sprintf("from:%s", actor)},
		Fields:    json.RawMessage(fieldsJSON),
		CreatedBy: actor,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "jira reply: warn: failed to create message bead: %v\n", err)
		return
	}

	writeSlackReplyLineage(ctx, beadID)

	// Link the reply into the ticket graph: stamp a triggered-by edge from the
	// reply message bead to the ticket's Jira-mirror bead (label jira:<KEY>).
	// The INBOUND mention task already gets this edge (bridge
	// writeMirrorTriggeredBy); without the OUTBOUND analog, beads-viewer
	// traversal from a ticket node reaches the spawned agent but not the
	// replies it posted back — the outbound half of the conversation floats
	// free of the ticket graph. The Slack twin binds replies to the thread head
	// via thread_id (recordSlackReplyBead); this is the Jira equivalent. Part
	// of the auto-link substrate (epic ka-0000000933). Best-effort: degrades to
	// no edge when no mirror exists yet (poller not synced / manual binding).
	linkJiraReplyToMirror(ctx, beadID, jiraKey)

	if err := daemon.CloseBead(ctx, beadID, nil); err != nil {
		fmt.Fprintf(os.Stderr, "jira reply: warn: failed to close message bead %s: %v\n", beadID, err)
	}
}

// linkJiraReplyToMirror adds a `triggered-by` dependency from the reply message
// bead to the ticket's Jira-mirror bead, resolved by the canonical
// `jira:<KEY>` label (the poller / gitlab-bridge stamp it on the mirror). This
// mirrors the bridge's inbound writeMirrorTriggeredBy so the outbound reply is
// discoverable from the same ticket node.
//
// Best-effort: a missing daemon, an empty key, a lookup failure, a no-match
// (mirror not yet synced), or a write failure are all warned on stderr and
// never fail the caller — the Jira comment and message bead already exist.
func linkJiraReplyToMirror(ctx context.Context, replyBeadID, jiraKey string) {
	if daemon == nil || replyBeadID == "" || jiraKey == "" {
		return
	}
	result, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Labels: []string{"jira:" + jiraKey},
		Limit:  10,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "jira reply: warn: mirror lookup failed for %s: %v\n", jiraKey, err)
		return
	}
	if result == nil || len(result.Beads) == 0 {
		// No mirror yet — the reply keeps its agent/trigger lineage; nothing
		// to link to. This is expected for a ticket the poller has not synced.
		return
	}
	for _, mirror := range result.Beads {
		if mirror == nil || mirror.ID == "" || mirror.ID == replyBeadID {
			continue
		}
		if err := daemon.AddDependency(ctx, replyBeadID, mirror.ID, "triggered-by", actor); err != nil {
			fmt.Fprintf(os.Stderr, "jira reply: warn: failed to link reply %s to mirror %s: %v\n", replyBeadID, mirror.ID, err)
		}
	}
}

// ── JIRA HTTP client ───────────────────────────────────────────────

type jiraHTTPClient struct {
	baseURL    string
	authHeader string
	http       *http.Client
}

type jiraIssueResponse struct {
	Key    string              `json:"key"`
	ID     string              `json:"id"`
	Fields jiraIssueFieldsResp `json:"fields"`
}

type jiraIssueFieldsResp struct {
	Summary     string          `json:"summary"`
	Description json.RawMessage `json:"description"`
	Status      *jiraNamedRef   `json:"status"`
	IssueType   *jiraNamedRef   `json:"issuetype"`
	Priority    *jiraNamedRef   `json:"priority"`
	Reporter    *jiraUserRef    `json:"reporter"`
	Assignee    *jiraUserRef    `json:"assignee"`
	Labels      []string        `json:"labels"`
	Parent      *jiraParentRef  `json:"parent"`
	Created     string          `json:"created"`
	Updated     string          `json:"updated"`
	IssueLinks  []jiraLinkRef   `json:"issuelinks"`
}

type jiraNamedRef struct {
	Name string `json:"name"`
}

type jiraUserRef struct {
	DisplayName string `json:"displayName"`
}

type jiraParentRef struct {
	Key    string `json:"key"`
	Fields struct {
		Summary string `json:"summary"`
	} `json:"fields"`
}

type jiraLinkRef struct {
	Type         jiraLinkTypeRef  `json:"type"`
	InwardIssue  *jiraLinkedIssue `json:"inwardIssue,omitempty"`
	OutwardIssue *jiraLinkedIssue `json:"outwardIssue,omitempty"`
}

type jiraLinkTypeRef struct {
	Inward  string `json:"inward"`
	Outward string `json:"outward"`
}

type jiraLinkedIssue struct {
	Key    string `json:"key"`
	Fields struct {
		Summary string        `json:"summary"`
		Status  *jiraNamedRef `json:"status"`
	} `json:"fields"`
}

type jiraCommentResponse struct {
	Body    json.RawMessage `json:"body"`
	Author  *jiraUserRef    `json:"author"`
	Created string          `json:"created"`
}

func (c *jiraHTTPClient) getIssue(ctx context.Context, key string) (*jiraIssueResponse, error) {
	path := "/rest/api/3/issue/" + url.PathEscape(key)
	var issue jiraIssueResponse
	if err := c.doGet(ctx, path, &issue); err != nil {
		return nil, err
	}
	return &issue, nil
}

func (c *jiraHTTPClient) getComments(ctx context.Context, key string, maxResults int) ([]jiraCommentResponse, error) {
	if maxResults <= 0 {
		maxResults = 20
	}
	path := fmt.Sprintf("/rest/api/3/issue/%s/comment?maxResults=%d&orderBy=-created",
		url.PathEscape(key), maxResults)

	var result struct {
		Comments []jiraCommentResponse `json:"comments"`
		Total    int                   `json:"total"`
	}
	if err := c.doGet(ctx, path, &result); err != nil {
		return nil, err
	}
	return result.Comments, nil
}

func (c *jiraHTTPClient) doGet(ctx context.Context, path string, result any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", c.authHeader)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		msg := string(body)
		if len(msg) > 256 {
			msg = msg[:256] + "..."
		}
		return fmt.Errorf("JIRA API returned %d: %s", resp.StatusCode, msg)
	}

	if err := json.Unmarshal(body, result); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// ── ADF to text ────────────────────────────────────────────────────

func jiraADFToText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	var doc struct {
		Content []jiraADFNode `json:"content"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return ""
	}

	var sb strings.Builder
	for _, node := range doc.Content {
		jiraRenderNode(&sb, node, 0)
	}
	return strings.TrimSpace(sb.String())
}

type jiraADFNode struct {
	Type    string          `json:"type"`
	Text    string          `json:"text,omitempty"`
	Content []jiraADFNode   `json:"content,omitempty"`
	Attrs   json.RawMessage `json:"attrs,omitempty"`
	Marks   []jiraADFMark   `json:"marks,omitempty"`
}

type jiraADFMark struct {
	Type  string          `json:"type"`
	Attrs json.RawMessage `json:"attrs,omitempty"`
}

func jiraRenderNode(sb *strings.Builder, node jiraADFNode, depth int) {
	switch node.Type {
	case "doc":
		for _, child := range node.Content {
			jiraRenderNode(sb, child, depth)
		}
	case "paragraph":
		jiraRenderInline(sb, node.Content)
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
		jiraRenderInline(sb, node.Content)
		sb.WriteString("\n\n")
	case "bulletList":
		for _, item := range node.Content {
			if item.Type == "listItem" {
				sb.WriteString("- ")
				for i, child := range item.Content {
					if i > 0 {
						sb.WriteString("  ")
					}
					jiraRenderInline(sb, child.Content)
				}
				sb.WriteString("\n")
			}
		}
		sb.WriteString("\n")
	case "orderedList":
		for i, item := range node.Content {
			if item.Type == "listItem" {
				fmt.Fprintf(sb, "%d. ", i+1)
				for j, child := range item.Content {
					if j > 0 {
						sb.WriteString("   ")
					}
					jiraRenderInline(sb, child.Content)
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
		jiraRenderInline(sb, node.Content)
		sb.WriteString("\n```\n\n")
	case "blockquote":
		for _, child := range node.Content {
			sb.WriteString("> ")
			jiraRenderInline(sb, child.Content)
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
	}
}

func jiraRenderInline(sb *strings.Builder, nodes []jiraADFNode) {
	for _, n := range nodes {
		switch n.Type {
		case "text":
			text := n.Text
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

// ── Output formatting ──────────────────────────────────────────────

func printJiraTicket(issue *jiraIssueResponse, comments []jiraCommentResponse) {
	f := issue.Fields

	fmt.Printf("# %s: %s\n\n", issue.Key, f.Summary)

	fmt.Printf("%-12s %s\n", "Type:", nameOrEmpty(f.IssueType))
	fmt.Printf("%-12s %s\n", "Status:", nameOrEmpty(f.Status))
	fmt.Printf("%-12s %s\n", "Priority:", nameOrEmpty(f.Priority))
	fmt.Printf("%-12s %s\n", "Assignee:", userOrEmpty(f.Assignee))
	fmt.Printf("%-12s %s\n", "Reporter:", userOrEmpty(f.Reporter))
	if len(f.Labels) > 0 {
		fmt.Printf("%-12s %s\n", "Labels:", strings.Join(f.Labels, ", "))
	}
	if f.Parent != nil {
		fmt.Printf("%-12s %s — %s\n", "Parent:", f.Parent.Key, f.Parent.Fields.Summary)
	}
	fmt.Printf("%-12s %s\n", "Created:", formatJiraTime(f.Created))
	fmt.Printf("%-12s %s\n", "Updated:", formatJiraTime(f.Updated))

	// Linked issues.
	if len(f.IssueLinks) > 0 {
		fmt.Printf("\n## Linked Issues\n\n")
		for _, link := range f.IssueLinks {
			if link.OutwardIssue != nil {
				status := ""
				if link.OutwardIssue.Fields.Status != nil {
					status = " [" + link.OutwardIssue.Fields.Status.Name + "]"
				}
				fmt.Printf("  %s %s — %s%s\n", link.Type.Outward, link.OutwardIssue.Key,
					link.OutwardIssue.Fields.Summary, status)
			}
			if link.InwardIssue != nil {
				status := ""
				if link.InwardIssue.Fields.Status != nil {
					status = " [" + link.InwardIssue.Fields.Status.Name + "]"
				}
				fmt.Printf("  %s %s — %s%s\n", link.Type.Inward, link.InwardIssue.Key,
					link.InwardIssue.Fields.Summary, status)
			}
		}
	}

	// Description.
	desc := jiraADFToText(f.Description)
	if desc != "" {
		fmt.Printf("\n## Description\n\n%s\n", desc)
	} else {
		fmt.Printf("\n## Description\n\n(no description)\n")
	}

	// Comments.
	if len(comments) > 0 {
		fmt.Printf("\n## Comments (%d)\n\n", len(comments))
		for _, c := range comments {
			author := "unknown"
			if c.Author != nil {
				author = c.Author.DisplayName
			}
			body := jiraADFToText(c.Body)
			if len(body) > 500 {
				body = body[:500] + "..."
			}
			fmt.Printf("### %s (%s)\n%s\n\n", author, formatJiraTime(c.Created), body)
		}
	}
}

func nameOrEmpty(ref *jiraNamedRef) string {
	if ref != nil {
		return ref.Name
	}
	return "(none)"
}

func userOrEmpty(ref *jiraUserRef) string {
	if ref != nil {
		return ref.DisplayName
	}
	return "(unassigned)"
}

func formatJiraTime(s string) string {
	t, err := time.Parse("2006-01-02T15:04:05.000-0700", s)
	if err != nil {
		return s
	}
	return t.Format("2006-01-02 15:04")
}

func init() {
	jiraShowCmd.Flags().Int("max-comments", 20, "maximum number of comments to fetch")

	jiraReplyCmd.Flags().Bool("public", false, "post a public (customer-facing) reply instead of an internal note; still gated per role by the bridge write policy")

	jiraThreadCmd.Flags().Int("limit", 0, "maximum number of comments to fetch (bridge default 20, cap 100)")

	jiraCmd.AddCommand(jiraShowCmd)
	jiraCmd.AddCommand(jiraReplyCmd)
	jiraCmd.AddCommand(jiraThreadCmd)
}
