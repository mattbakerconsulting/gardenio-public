// Package bridge — agent-initiated Jira ticket-body read endpoint.
//
// GET /api/jira/issue is the ticket-BODY analog of GET /api/jira/thread
// (feature G6, epic ka-0000007401). G1 (/api/jira/thread) lets a Jira-bound
// agent re-read the ticket's COMMENTS mid-session without raw Jira credentials
// in the pod; this endpoint does the same for the ticket's current
// description / status / labels / fields. A live conversational agent needs it
// because the F4 spawn-prompt snapshot of the ticket body goes stale the moment
// an operator edits the description or transitions the status — and H5
// (ka-0000004269) even spawns agents ON description edits, yet the agent could
// not read the current body back.
//
// `gb jira show` uses raw JIRA_BASE_URL/EMAIL/API_TOKEN when they are present
// (operator laptop), but agent pods carry none of those — so inside a pod the
// direct path fails. Routing `gb jira show` through this bridge endpoint gives
// the same credential-free, bound-ticket-scoped read the thread endpoint gives.
//
// Security invariants (mirrors /api/jira/thread — do not weaken):
//
//   - Bound-ticket scoping: the read target is resolved from the acting
//     agent's OWN jira_key binding (agent bead field, else notes). There is
//     no way to name an arbitrary ticket — an agent can only ever read the
//     ticket it is bound to, so pods never need raw JIRA_* credentials.
//   - Bearer auth: the same JIRA_BRIDGE_AGENT_TOKEN gate as the reply and
//     thread endpoints applies (empty token = local dev, unauthenticated).
//   - This is a READ of a ticket whose content the agent already received at
//     spawn time (F4 snapshot), so the outbound WritePolicyStore (which gates
//     writes) is not consulted; nothing here can post to Jira.
package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// JiraIssueReader is the subset of JiraClient used to serve GET /api/jira/issue:
// a single bound-ticket fetch. The production jira-bridge threads the same
// *JiraClient it already wires as the reply/thread History client. A nil reader
// disables issue reads (the endpoint answers 501) without affecting replies or
// thread reads.
type JiraIssueReader interface {
	GetIssue(ctx context.Context, key string) (*JiraIssue, error)
}

// jiraIssueResponse is the flattened response body for GET /api/jira/issue.
// It mirrors the fields `gb jira show` renders, with the ADF description
// normalized to markdown so the agent can inject it as context directly.
type jiraIssueResponse struct {
	OK          bool               `json:"ok"`
	JiraKey     string             `json:"jira_key,omitempty"`
	Summary     string             `json:"summary,omitempty"`
	Description string             `json:"description,omitempty"`
	Status      string             `json:"status,omitempty"`
	IssueType   string             `json:"issue_type,omitempty"`
	Priority    string             `json:"priority,omitempty"`
	Assignee    string             `json:"assignee,omitempty"`
	Reporter    string             `json:"reporter,omitempty"`
	Labels      []string           `json:"labels,omitempty"`
	Parent      *jiraIssueParent   `json:"parent,omitempty"`
	Created     string             `json:"created,omitempty"`
	Updated     string             `json:"updated,omitempty"`
	BrowseURL   string             `json:"browse_url,omitempty"`
	Links       []jiraIssueLinkOut `json:"links,omitempty"`
	Error       string             `json:"error,omitempty"`
}

// jiraIssueParent is the epic/parent link on an issue-read response.
type jiraIssueParent struct {
	Key     string `json:"key"`
	Summary string `json:"summary,omitempty"`
}

// jiraIssueLinkOut is one linked issue on an issue-read response, flattened to
// the human-readable relationship phrase plus the linked key. The modeled
// JiraLinkedRef carries only the key (the poller's issue-link fetch does not
// expand summary/status), so that is all we surface.
type jiraIssueLinkOut struct {
	Relationship string `json:"relationship"`
	Key          string `json:"key"`
}

// handleGetIssue handles GET /api/jira/issue?agent=<name>.
func (a *JiraReplyAPI) handleGetIssue(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJiraIssueError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	if !a.authorized(r) {
		writeJiraIssueError(w, http.StatusUnauthorized, "missing or invalid bearer token")
		return
	}

	if a.issues == nil {
		writeJiraIssueError(w, http.StatusNotImplemented, "issue reads are not configured on this bridge")
		return
	}

	agent := strings.TrimSpace(r.URL.Query().Get("agent"))
	if agent == "" {
		writeJiraIssueError(w, http.StatusBadRequest, "agent is required")
		return
	}

	agentBead, err := a.daemon.FindAgentBead(r.Context(), agent)
	if err != nil {
		a.logger.Warn("jira-issue: agent bead not found", "agent", agent, "error", err)
		writeJiraIssueError(w, http.StatusNotFound, "agent not found")
		return
	}

	// Bound-ticket scoping: same binding resolution as the reply/thread endpoints.
	jiraKey := agentBead.Fields["jira_key"]
	if jiraKey == "" {
		if notes := beadsapi.ParseNotes(agentBead.Notes); notes != nil {
			jiraKey = notes["jira_key"]
		}
	}
	if jiraKey == "" {
		writeJiraIssueError(w, http.StatusForbidden, "agent has no jira_key binding")
		return
	}

	issue, err := a.issues.GetIssue(r.Context(), jiraKey)
	if err != nil {
		a.logger.Error("jira-issue: failed to fetch issue",
			"agent", agent, "jira_key", jiraKey, "error", err)
		writeJiraIssueError(w, http.StatusBadGateway, "failed to fetch issue")
		return
	}

	resp := flattenJiraIssue(jiraKey, issue, a.browseURL(jiraKey))
	a.logger.Info("jira-issue: served issue read", "agent", agent, "jira_key", jiraKey)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// browseURL returns the ticket's browse URL when the issue reader can build one
// (the production *JiraClient does), else "".
func (a *JiraReplyAPI) browseURL(key string) string {
	if b, ok := a.issues.(interface{ BrowseURL(string) string }); ok {
		return b.BrowseURL(key)
	}
	return ""
}

// flattenJiraIssue projects the modeled JiraIssue onto the wire response,
// normalizing the ADF description to markdown.
func flattenJiraIssue(key string, issue *JiraIssue, browseURL string) jiraIssueResponse {
	resp := jiraIssueResponse{OK: true, JiraKey: key, BrowseURL: browseURL}
	if issue == nil {
		return resp
	}
	f := issue.Fields
	resp.Summary = f.Summary
	resp.Description = strings.TrimSpace(adfToMarkdown(f.Description))
	resp.Status = namedRefName(f.Status)
	resp.IssueType = namedRefName(f.IssueType)
	resp.Priority = namedRefName(f.Priority)
	resp.Assignee = jiraUserName(f.Assignee)
	resp.Reporter = jiraUserName(f.Reporter)
	resp.Labels = f.Labels
	resp.Created = f.Created
	resp.Updated = f.Updated
	if f.Parent != nil && f.Parent.Key != "" {
		resp.Parent = &jiraIssueParent{Key: f.Parent.Key, Summary: f.Parent.Fields.Summary}
	}
	for _, link := range f.IssueLinks {
		if link.OutwardIssue != nil && link.OutwardIssue.Key != "" {
			resp.Links = append(resp.Links, jiraIssueLinkOut{
				Relationship: link.Type.Outward,
				Key:          link.OutwardIssue.Key,
			})
		}
		if link.InwardIssue != nil && link.InwardIssue.Key != "" {
			resp.Links = append(resp.Links, jiraIssueLinkOut{
				Relationship: link.Type.Inward,
				Key:          link.InwardIssue.Key,
			})
		}
	}
	return resp
}

// namedRefName returns ref.Name or "" for a nil ref.
func namedRefName(ref *JiraNamedRef) string {
	if ref == nil {
		return ""
	}
	return ref.Name
}

// jiraUserName returns u.DisplayName or "" for a nil user.
func jiraUserName(u *JiraUser) string {
	if u == nil {
		return ""
	}
	return u.DisplayName
}

// writeJiraIssueError writes a JSON error response with the given status.
func writeJiraIssueError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(jiraIssueResponse{OK: false, Error: msg})
}
