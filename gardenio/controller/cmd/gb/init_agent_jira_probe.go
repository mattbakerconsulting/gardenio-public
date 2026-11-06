package main

// init_agent_jira_probe.go — startup smoke test for JIRA service-account access.
//
// ka-0000000556 / ka-0000003632: when the JIRA service-account loses project
// visibility, agents whose triage depends on `gb jira show` quietly produce
// blind output. This probe runs once at agent startup and squawks loudly if
// JIRA creds are set but cannot reach a known canary ticket.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// jiraSmokeTicketDefault is the canary ticket fetched when JIRA_SMOKE_TICKET
// is unset. PE-1 is the oldest, most stable ticket in the PE project; if the
// service account loses PE visibility, every triage agent goes blind.
const jiraSmokeTicketDefault = "PE-1"

// jiraProbeHTTPClient is overridable for tests.
var jiraProbeHTTPClient = &http.Client{Timeout: 15 * time.Second}

// probeJIRA sanity-checks JIRA service-account access at agent startup.
//
//   - Creds unset → no-op silently (agent simply isn't a JIRA-aware role).
//   - 401/403/404 from the canary fetch → squawk loudly to the operator
//     channel. These are the signatures of permission breakage seen in
//     ka-0000003632; they mean the service account is configured but cannot
//     read the canary, so any downstream `gb jira show` will return garbage.
//   - Network/DNS/timeout → log a warning but do not squawk. Transient
//     failures shouldn't flap the operator; only durable misconfiguration
//     should.
func probeJIRA(ctx context.Context) {
	baseURL := strings.TrimRight(os.Getenv("JIRA_BASE_URL"), "/")
	email := os.Getenv("JIRA_EMAIL")
	apiToken := os.Getenv("JIRA_API_TOKEN")

	if baseURL == "" || email == "" || apiToken == "" {
		return
	}

	ticket := strings.TrimSpace(os.Getenv("JIRA_SMOKE_TICKET"))
	if ticket == "" {
		ticket = jiraSmokeTicketDefault
	}

	probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	status, body, err := jiraSmokeFetch(probeCtx, baseURL, email, apiToken, ticket)
	if err != nil {
		logInit("WARNING: JIRA smoke probe transient failure (%s @ %s): %v", ticket, baseURL, err)
		return
	}
	if status >= 200 && status < 300 {
		logInit("JIRA smoke probe OK (%s @ %s)", ticket, baseURL)
		return
	}
	if !isJiraPermissionStatus(status) {
		logInit("WARNING: JIRA smoke probe got HTTP %d for %s — not a permission code, skipping squawk", status, ticket)
		return
	}

	snippet := strings.TrimSpace(body)
	if len(snippet) > 200 {
		snippet = snippet[:200] + "..."
	}
	msg := fmt.Sprintf(
		"JIRA smoke probe FAILED: GET %s/rest/api/3/issue/%s -> HTTP %d. "+
			"Service-account creds are set but cannot reach the canary ticket. "+
			"Triage agents that depend on JIRA descriptions will produce blind output until this is fixed (ka-0000003632).",
		baseURL, ticket, status)
	if snippet != "" {
		msg += " Body: " + snippet
	}
	logInit("ALARM: %s", msg)
	if err := jiraSmokeSquawk(ctx, msg); err != nil {
		logInit("warning: failed to squawk JIRA probe alarm: %v", err)
	}
}

// isJiraPermissionStatus reports whether the HTTP status indicates a
// permission/visibility problem (vs a transient failure or server error).
func isJiraPermissionStatus(status int) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return true
	}
	return false
}

// jiraSmokeFetch GETs the canary issue and returns (status, body, err). A
// network error returns err non-nil; non-2xx still returns the status.
func jiraSmokeFetch(ctx context.Context, baseURL, email, token, ticket string) (int, string, error) {
	url := baseURL + "/rest/api/3/issue/" + ticket + "?fields=summary"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(email+":"+token)))
	req.Header.Set("Accept", "application/json")

	resp, err := jiraProbeHTTPClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	return resp.StatusCode, string(body), nil
}

// jiraSmokeSquawk emits an operator-visible squawk via the daemon API.
// Mirrors runSquawk in squawk.go (create message bead, then close).
func jiraSmokeSquawk(ctx context.Context, text string) error {
	if daemon == nil {
		return fmt.Errorf("beads daemon not connected")
	}
	title := text
	if len(title) > 500 {
		title = title[:497] + "..."
	}
	fields, err := json.Marshal(map[string]string{
		"source_agent": actor,
		"text":         text,
	})
	if err != nil {
		return err
	}
	id, err := daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:     title,
		Type:      "message",
		Kind:      "data",
		Labels:    []string{"squawk", "jira-smoke", fmt.Sprintf("from:%s", actor)},
		Fields:    json.RawMessage(fields),
		CreatedBy: actor,
	})
	if err != nil {
		return err
	}
	return daemon.CloseBead(ctx, id, nil)
}
