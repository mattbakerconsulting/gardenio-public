package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/forgebeads"
	"gardenio/controller/internal/githubapp"
)

// The poster mirrors gitlab-bridge's squawk forwarder (internal/bridge/
// gitlab_squawk_forward.go): a closed squawk message bead from an agent that
// owns a PR becomes a comment on that PR.

// squawkOptInLabel is the label a squawk must carry to be mirrored to the PR
// (`gb squawk --to-mr`). Plain progress squawks stay Slack-only, as on GitLab
// (ka-0000005748).
const squawkOptInLabel = "to-mr"

// squawkMaxLen caps the mirrored comment. Long reports belong in a bundle.
const squawkMaxLen = 4000

// squawkMarker is the hidden comment appended to every posted squawk. It is
// the de-duplication key (hash of PR, agent and text) and the self-filter the
// review handler matches on.
func squawkMarker(hash string) string { return squawkMarkerPrefix + hash + " -->" }

// squawkHash identifies one squawk for one PR. Keyed on content rather than the
// bead id so a looping agent repeating itself posts once, and a redelivered or
// replayed SSE event after a restart cannot post twice.
func squawkHash(prURL, agent, text string) string {
	sum := sha256.Sum256([]byte(prURL + "\x00" + agent + "\x00" + text))
	return hex.EncodeToString(sum[:])[:16]
}

// prRef addresses one pull request.
type prRef struct {
	Repo   string // owner/name
	Number int
}

var githubPRURLRe = regexp.MustCompile(`^https?://[^/]+/([^/]+/[^/]+)/pull/(\d+)(?:[/?#].*)?$`)

// parsePRURL extracts the repo and number from a GitHub pull request URL. A
// GitLab MR URL (/-/merge_requests/) never matches.
func parsePRURL(u string) (prRef, bool) {
	m := githubPRURLRe.FindStringSubmatch(u)
	if m == nil {
		return prRef{}, false
	}
	n, err := strconv.Atoi(m[2])
	if err != nil || n <= 0 {
		return prRef{}, false
	}
	return prRef{Repo: m[1], Number: n}, true
}

// prCommenter is the GitHub side of the poster (a seam for tests).
type prCommenter interface {
	// HasMarker reports whether any comment on the PR already contains marker.
	HasMarker(ctx context.Context, pr prRef, marker string) (bool, error)
	Post(ctx context.Context, pr prRef, body string) error
}

// squawkBeads is the slice of beadsapi.Client the poster uses.
type squawkBeads interface {
	FindAgentBead(ctx context.Context, agentName string) (*beadsapi.BeadDetail, error)
	GetBead(ctx context.Context, beadID string) (*beadsapi.BeadDetail, error)
}

var _ squawkBeads = (*beadsapi.Client)(nil)

// squawkPoster posts agent squawks as PR comments.
type squawkPoster struct {
	beads  squawkBeads
	github prCommenter
	logger *slog.Logger

	mu   sync.Mutex
	seen map[string]bool // squawk bead ids handled this process (SSE reconnect replays)
}

func newSquawkPoster(beads squawkBeads, gh prCommenter, logger *slog.Logger) *squawkPoster {
	return &squawkPoster{beads: beads, github: gh, logger: logger, seen: map[string]bool{}}
}

// squawkTopic is the event topic closed beads (where squawks land) arrive on.
const squawkTopic = "beads.bead.closed"

// squawkReconnectWait is the pause before reopening a dropped event stream.
const squawkReconnectWait = 5 * time.Second

// eventStreamer is the daemon surface Run needs (*beadsapi.Client satisfies it).
type eventStreamer interface {
	EventStream(ctx context.Context, topics string) (<-chan beadsapi.SSEEvent, error)
}

// squawkBead is the subset of a bead.closed payload the poster reads.
type squawkBead struct {
	ID        string
	Type      string
	Title     string
	CreatedBy string
	Labels    []string
	Fields    map[string]string
}

// parseSquawkBead decodes {"bead": {...}} from a kbeads bead event; nil when
// the payload carries no bead.
func parseSquawkBead(data []byte) *squawkBead {
	var w struct {
		Bead *struct {
			ID        string          `json:"id"`
			Type      string          `json:"type"`
			Title     string          `json:"title"`
			CreatedBy string          `json:"created_by"`
			Labels    []string        `json:"labels"`
			Fields    json.RawMessage `json:"fields"`
		} `json:"bead"`
	}
	if json.Unmarshal(data, &w) != nil || w.Bead == nil || w.Bead.ID == "" {
		return nil
	}
	b := w.Bead
	return &squawkBead{ID: b.ID, Type: b.Type, Title: b.Title, CreatedBy: b.CreatedBy,
		Labels: b.Labels, Fields: beadsapi.ParseFieldsJSON(b.Fields)}
}

// Run consumes closed-bead events until ctx is done, reconnecting when the
// stream drops. The stream starts from "now": a squawk closed while the bridge
// was down is not replayed, which is the safe side for an opt-in mirror.
func (p *squawkPoster) Run(ctx context.Context, daemon eventStreamer) {
	for ctx.Err() == nil {
		events, err := daemon.EventStream(ctx, squawkTopic)
		if err != nil {
			p.logger.Warn("squawk: opening event stream failed, retrying", "error", err)
		} else {
			p.logger.Info("squawk: event stream open", "topic", squawkTopic)
			for evt := range events {
				if evt.Event == squawkTopic {
					p.handleClosed(ctx, evt.Data)
				}
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(squawkReconnectWait):
		}
	}
}

func (p *squawkPoster) handleClosed(ctx context.Context, data []byte) {
	bead := parseSquawkBead(data)
	if bead == nil {
		return
	}
	if err := p.post(ctx, bead); err != nil {
		p.logger.Error("squawk: post to PR failed", "squawk", bead.ID, "error", err)
		// Let a replay of this bead try again.
		p.mu.Lock()
		delete(p.seen, bead.ID)
		p.mu.Unlock()
	}
}

func hasLabel(labels []string, want string) bool {
	for _, l := range labels {
		if l == want {
			return true
		}
	}
	return false
}

// squawkAgent names the agent a squawk bead came from, the way gb squawk and
// the Slack watcher read it.
func squawkAgent(b *squawkBead) string {
	name := b.Fields["source_agent"]
	if name == "" {
		for _, l := range b.Labels {
			if strings.HasPrefix(l, "from:") {
				name = strings.TrimPrefix(l, "from:")
				break
			}
		}
	}
	if name == "" {
		name = b.CreatedBy
	}
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	return name
}

// prURLForAgent finds the GitHub PR the agent is working on: its own mr_url if
// set, else the mr_url of the task bead it is bound to. ok is false for an
// agent with no PR, or one on a different forge.
func (p *squawkPoster) prURLForAgent(ctx context.Context, agent string) (string, bool, error) {
	ab, err := p.beads.FindAgentBead(ctx, agent)
	if err != nil {
		var apiErr *beadsapi.APIError
		if errors.Is(err, beadsapi.ErrAgentNotFound) || (errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound) {
			return "", false, nil // not a gardenio agent: nothing to mirror
		}
		return "", false, err
	}
	mrURL := ab.Fields["mr_url"]
	if mrURL == "" {
		if taskID := ab.Fields["task_id"]; taskID != "" {
			task, err := forgebeads.GetBeadIfExists(ctx, p.beads, taskID)
			if err != nil {
				return "", false, err
			}
			if task != nil {
				mrURL = task.Fields["mr_url"]
			}
		}
	}
	if mrURL == "" {
		return "", false, nil
	}
	return mrURL, true, nil
}

// post mirrors one closed bead if it is an opt-in squawk from an agent with a
// GitHub PR. An error means a retry could succeed; skips return nil.
func (p *squawkPoster) post(ctx context.Context, bead *squawkBead) error {
	if bead.Type != "message" || (!hasLabel(bead.Labels, "squawk") && !hasLabel(bead.Labels, "say")) {
		return nil
	}
	// Not marked seen when unlabelled: the bead is not ours to consume.
	if !hasLabel(bead.Labels, squawkOptInLabel) {
		return nil
	}
	p.mu.Lock()
	if p.seen[bead.ID] {
		p.mu.Unlock()
		return nil
	}
	p.seen[bead.ID] = true
	p.mu.Unlock()

	agent := squawkAgent(bead)
	if agent == "" {
		return nil
	}
	text := strings.TrimSpace(firstNonEmpty(bead.Fields["text"], bead.Title))
	if text == "" {
		return nil
	}
	if len(text) > squawkMaxLen {
		text = truncateRunes(text, squawkMaxLen)
	}

	mrURL, ok, err := p.prURLForAgent(ctx, agent)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	pr, ok := parsePRURL(mrURL)
	if !ok {
		return nil // a GitLab MR, or not a PR link: gitlab-bridge's concern
	}

	marker := squawkMarker(squawkHash(mrURL, agent, text))
	dup, err := p.github.HasMarker(ctx, pr, marker)
	if err != nil {
		return fmt.Errorf("check %s#%d for earlier squawk: %w", pr.Repo, pr.Number, err)
	}
	if dup {
		p.logger.Info("squawk: already on PR, skipped", "agent", agent, "pr", mrURL)
		return nil
	}
	body := fmt.Sprintf("**gardenio agent** `%s`:\n\n%s\n\n%s", agent, text, marker)
	if err := p.github.Post(ctx, pr, body); err != nil {
		return fmt.Errorf("post to %s#%d: %w", pr.Repo, pr.Number, err)
	}
	p.logger.Info("squawk: posted PR comment", "agent", agent, "pr", mrURL, "text_length", len(text))
	return nil
}

// githubCommenter posts and lists PR conversation comments with a per-repo
// installation token.
type githubCommenter struct {
	tokens interface {
		Token(ctx context.Context, repo string) (githubapp.Token, error)
	}
	baseURL string
	hc      *http.Client
}

func newGitHubCommenter(tokens *tokenSource, baseURL string) *githubCommenter {
	if baseURL == "" {
		baseURL = githubapp.DefaultAPIBaseURL
	}
	return &githubCommenter{tokens: tokens, baseURL: strings.TrimRight(baseURL, "/"), hc: &http.Client{Timeout: 30 * time.Second}}
}

const (
	commentsPerPage = 100
	// maxCommentPages bounds the marker scan: 1000 comments covers any PR an
	// agent works; past that a duplicate is preferred over an unbounded loop.
	maxCommentPages = 10
)

func (c *githubCommenter) do(ctx context.Context, repo, method, path string, body any, want int, out any) error {
	tok, err := c.tokens.Token(ctx, repo)
	if err != nil {
		return fmt.Errorf("installation token: %w", err)
	}
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return errors.New("build request")
	}
	req.Header.Set("Authorization", "Bearer "+tok.Reveal())
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) && ue.Err != nil {
			return fmt.Errorf("%s %s: %w", method, req.URL.Path, ue.Err)
		}
		return fmt.Errorf("%s %s: request failed", method, req.URL.Path)
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		var m struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &m)
		return fmt.Errorf("%s %s: HTTP %d: %s", method, req.URL.Path, resp.StatusCode, strings.TrimSpace(m.Message))
	}
	if out != nil {
		return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out)
	}
	return nil
}

func (c *githubCommenter) HasMarker(ctx context.Context, pr prRef, marker string) (bool, error) {
	for page := 1; page <= maxCommentPages; page++ {
		var comments []struct {
			Body string `json:"body"`
		}
		path := fmt.Sprintf("/repos/%s/issues/%d/comments?per_page=%d&page=%d", pr.Repo, pr.Number, commentsPerPage, page)
		if err := c.do(ctx, pr.Repo, http.MethodGet, path, nil, http.StatusOK, &comments); err != nil {
			return false, err
		}
		for _, cm := range comments {
			if strings.Contains(cm.Body, marker) {
				return true, nil
			}
		}
		if len(comments) < commentsPerPage {
			return false, nil
		}
	}
	return false, nil
}

func (c *githubCommenter) Post(ctx context.Context, pr prRef, body string) error {
	path := fmt.Sprintf("/repos/%s/issues/%d/comments", pr.Repo, pr.Number)
	return c.do(ctx, pr.Repo, http.MethodPost, path, map[string]string{"body": body}, http.StatusCreated, nil)
}
