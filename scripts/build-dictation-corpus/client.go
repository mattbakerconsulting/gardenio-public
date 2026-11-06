package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// beadsClient is a tiny read-only HTTP client for the kbeads REST API.
// We use a purpose-built client here (rather than internal/client in the
// kbeads module) so the corpus tool can live in its own module under
// scripts/ with no internal-package dependencies.
type beadsClient struct {
	baseURL string
	token   string
	fleet   string
	http    *http.Client
}

func newBeadsClient(baseURL, token, fleet string) *beadsClient {
	return &beadsClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		fleet:   fleet,
		http:    &http.Client{Timeout: 60 * time.Second},
	}
}

// bead mirrors only the fields we care about for corpus building.
type bead struct {
	ID          string          `json:"id"`
	Kind        string          `json:"kind"`
	Type        string          `json:"type"`
	Title       string          `json:"title"`
	Description string          `json:"description,omitempty"`
	Notes       string          `json:"notes,omitempty"`
	Status      string          `json:"status"`
	Labels      []string        `json:"labels,omitempty"`
	Fields      json.RawMessage `json:"fields,omitempty"`
}

type comment struct {
	ID     int64  `json:"id"`
	BeadID string `json:"bead_id"`
	Author string `json:"author"`
	Text   string `json:"text"`
}

type listBeadsResponse struct {
	Beads []*bead `json:"beads"`
	Total int     `json:"total"`
}

func (c *beadsClient) do(ctx context.Context, path string, q url.Values, out any) error {
	u := c.baseURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if c.fleet != "" {
		req.Header.Set("X-Beads-Fleet", c.fleet)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s %s: HTTP %d: %s", http.MethodGet, u, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// listBeadsPage fetches a single page of beads matching the given statuses,
// optionally narrowed by kind and label.
func (c *beadsClient) listBeadsPage(ctx context.Context, statuses []string, kind, label string, offset, limit int) (*listBeadsResponse, error) {
	q := url.Values{}
	if len(statuses) > 0 {
		q.Set("status", strings.Join(statuses, ","))
	}
	if kind != "" {
		q.Set("kind", kind)
	}
	if label != "" {
		q.Set("label", label)
	}
	q.Set("limit", fmt.Sprintf("%d", limit))
	q.Set("offset", fmt.Sprintf("%d", offset))
	var out listBeadsResponse
	if err := c.do(ctx, "/v1/beads", q, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// listAllBeads paginates through beads across open/in_progress/deferred/closed,
// optionally scoped to a single kind and/or a single label (e.g. "project:gardenio").
func (c *beadsClient) listAllBeads(ctx context.Context, pageSize int, kind, label string) ([]*bead, error) {
	if pageSize <= 0 {
		pageSize = 500
	}
	// Ask explicitly for every status; the default HTTP filter on some
	// deployments is open+in_progress only.
	statuses := []string{"open", "in_progress", "deferred", "closed"}
	var all []*bead
	offset := 0
	for {
		page, err := c.listBeadsPage(ctx, statuses, kind, label, offset, pageSize)
		if err != nil {
			return nil, fmt.Errorf("list beads offset=%d: %w", offset, err)
		}
		if len(page.Beads) == 0 {
			break
		}
		all = append(all, page.Beads...)
		offset += len(page.Beads)
		if offset >= page.Total {
			break
		}
	}
	return all, nil
}

func (c *beadsClient) getComments(ctx context.Context, beadID string) ([]*comment, error) {
	var out struct {
		Comments []*comment `json:"comments"`
	}
	if err := c.do(ctx, "/v1/beads/"+url.PathEscape(beadID)+"/comments", nil, &out); err != nil {
		return nil, err
	}
	return out.Comments, nil
}
