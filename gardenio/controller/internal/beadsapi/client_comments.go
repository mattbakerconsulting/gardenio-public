package beadsapi

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// Comment represents a bead comment. ID matches the int64 column in
// kbeads (see kbeads/internal/model/comment.go) — using a string here
// makes json.Unmarshal fail on every response.
type Comment struct {
	ID                int64     `json:"id"`
	Author            string    `json:"author"`
	Text              string    `json:"text"`
	CreatedAt         time.Time `json:"created_at"`
	OperatedBy        string    `json:"operated_by,omitempty"`
	OperatedByDisplay string    `json:"operated_by_display,omitempty"`
	OperatedByEmail   string    `json:"operated_by_email,omitempty"`
	// OperatedBySlug is the viewer-resolved canonical per-user-homepage
	// slug for the comment's operator (per-user-homepage M2 §6 /
	// ka-0000007787). The daemon does NOT populate this — beads-viewer
	// fills it in at handler time via a per-server slug cache backed by
	// the GetOperatorSlug daemon endpoint.
	OperatedBySlug string `json:"-"`
}

// AddComment adds a comment to a bead.
func (c *Client) AddComment(ctx context.Context, beadID, author, text string) error {
	body := map[string]string{
		"author": author,
		"text":   text,
	}
	path := "/v1/beads/" + url.PathEscape(beadID) + "/comments"
	if err := c.doJSON(ctx, http.MethodPost, path, body, nil); err != nil {
		return fmt.Errorf("adding comment to %s: %w", beadID, err)
	}
	return nil
}

// GetComments returns all comments on a bead.
func (c *Client) GetComments(ctx context.Context, beadID string) ([]Comment, error) {
	var resp struct {
		Comments []Comment `json:"comments"`
	}
	path := "/v1/beads/" + url.PathEscape(beadID) + "/comments"
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, fmt.Errorf("getting comments for %s: %w", beadID, err)
	}
	return resp.Comments, nil
}
