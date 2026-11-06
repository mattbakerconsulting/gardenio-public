package beadsapi

import (
	"context"
	"net/http"
)

// Read cursors (Lands Commons III step 5): the signed-in person's read
// position in each Commons channel, channel bead id -> last seen message id,
// kept on their own user bead by kbeads (/v1/users/me/read-cursors, debounced
// there to one write per 10 s per person). Both calls need the person's own
// bearer on ctx; kbeads answers 403 for any other caller and 404 when the
// person has no user bead.

type readCursorsBody struct {
	ReadCursors map[string]string `json:"read_cursors"`
}

// ReadCursors returns the caller's read positions, including any still
// waiting for kbeads' write interval.
func (c *Client) ReadCursors(ctx context.Context) (map[string]string, error) {
	var resp readCursorsBody
	if err := c.doJSON(ctx, http.MethodGet, "/v1/users/me/read-cursors", nil, &resp); err != nil {
		return nil, err
	}
	return resp.ReadCursors, nil
}

// SetReadCursors records positions for the caller and returns all of them.
func (c *Client) SetReadCursors(ctx context.Context, cursors map[string]string) (map[string]string, error) {
	var resp readCursorsBody
	if err := c.doJSON(ctx, http.MethodPost, "/v1/users/me/read-cursors", readCursorsBody{ReadCursors: cursors}, &resp); err != nil {
		return nil, err
	}
	return resp.ReadCursors, nil
}
