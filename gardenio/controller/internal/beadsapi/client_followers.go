package beadsapi

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// listFollowedBeadIDsResponse mirrors the daemon JSON for
// GET /v1/operators/{id}/followed-beads.
type listFollowedBeadIDsResponse struct {
	BeadIDs []string `json:"bead_ids"`
}

// BeadFollower mirrors model.BeadFollower for the daemon's
// GET /v1/beads/{id}/followers response. Only the fields the UI
// actually renders are surfaced; the daemon may include more.
type BeadFollower struct {
	BeadID             string    `json:"BeadID"`
	OperatorID         int64     `json:"OperatorID"`
	OperatorKind       string    `json:"OperatorKind"`
	OperatorSubject    string    `json:"OperatorSubject"`
	OperatorDisplay    string    `json:"OperatorDisplay"`
	ForwardedBy        *int64    `json:"ForwardedBy"`
	ForwardedByKind    string    `json:"ForwardedByKind"`
	ForwardedBySubject string    `json:"ForwardedBySubject"`
	ForwardedByDisplay string    `json:"ForwardedByDisplay"`
	Note               string    `json:"Note"`
	CreatedAt          time.Time `json:"CreatedAt"`
}

// listFollowersResponse mirrors the daemon JSON for
// GET /v1/beads/{id}/followers.
type listFollowersResponse struct {
	Followers []*BeadFollower `json:"followers"`
}

// addFollowerRequest is the body for POST /v1/beads/{id}/followers.
type addFollowerRequest struct {
	OperatorID int64  `json:"operator_id"`
	Note       string `json:"note,omitempty"`
}

// ListBeadFollowers returns the followers for a bead, joined with the
// operators table for display. Viewer-role.
func (c *Client) ListBeadFollowers(ctx context.Context, beadID string) ([]*BeadFollower, error) {
	if beadID == "" {
		return nil, fmt.Errorf("beadID is required")
	}
	path := "/v1/beads/" + url.PathEscape(beadID) + "/followers"
	var resp listFollowersResponse
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, fmt.Errorf("list followers for bead %s: %w", beadID, err)
	}
	return resp.Followers, nil
}

// AddBeadFollower adds operatorID as a follower of beadID. The daemon
// derives forwarded_by from the request's auth identity. Idempotent —
// re-forwarding the same (bead, operator) pair refreshes the note.
func (c *Client) AddBeadFollower(ctx context.Context, beadID string, operatorID int64, note string) error {
	if beadID == "" {
		return fmt.Errorf("beadID is required")
	}
	if operatorID <= 0 {
		return fmt.Errorf("operatorID must be positive, got %d", operatorID)
	}
	path := "/v1/beads/" + url.PathEscape(beadID) + "/followers"
	body := addFollowerRequest{OperatorID: operatorID, Note: note}
	if err := c.doJSON(ctx, http.MethodPost, path, body, nil); err != nil {
		return fmt.Errorf("add follower for bead %s: %w", beadID, err)
	}
	return nil
}

// RemoveBeadFollower deletes the (beadID, operatorID) follower row.
// Idempotent — a miss returns success.
func (c *Client) RemoveBeadFollower(ctx context.Context, beadID string, operatorID int64) error {
	if beadID == "" {
		return fmt.Errorf("beadID is required")
	}
	if operatorID <= 0 {
		return fmt.Errorf("operatorID must be positive, got %d", operatorID)
	}
	path := "/v1/beads/" + url.PathEscape(beadID) + "/followers/" + strconv.FormatInt(operatorID, 10)
	if err := c.doJSON(ctx, http.MethodDelete, path, nil, nil); err != nil {
		return fmt.Errorf("remove follower for bead %s: %w", beadID, err)
	}
	return nil
}

// ListFollowedBeadIDs returns the bead IDs the operator follows, ordered by
// follow created_at DESC (most-recently-forwarded first). Drives the inbox
// UNION right side (ka-0000007204): the viewer feeds these IDs into the
// existing list path so forwarded beads appear at the top of /inbox.
//
// limit ≤ 0 sends no caller-side cap; the daemon ceiling
// (listFollowedBeadIDsMaxLimit, 1000) still applies.
func (c *Client) ListFollowedBeadIDs(ctx context.Context, operatorID int64, limit int) ([]string, error) {
	if operatorID <= 0 {
		return nil, fmt.Errorf("operatorID must be positive, got %d", operatorID)
	}
	path := "/v1/operators/" + strconv.FormatInt(operatorID, 10) + "/followed-beads"
	if limit > 0 {
		q := url.Values{}
		q.Set("limit", strconv.Itoa(limit))
		path += "?" + q.Encode()
	}
	var resp listFollowedBeadIDsResponse
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, fmt.Errorf("list followed bead ids for operator %d: %w", operatorID, err)
	}
	return resp.BeadIDs, nil
}
