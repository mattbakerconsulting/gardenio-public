package beadsapi

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// AddLabel adds a label to a bead.
func (c *Client) AddLabel(ctx context.Context, beadID, label string) error {
	body := map[string]string{"label": label}
	path := "/v1/beads/" + url.PathEscape(beadID) + "/labels"
	if err := c.doJSON(ctx, http.MethodPost, path, body, nil); err != nil {
		return fmt.Errorf("adding label %q to %s: %w", label, beadID, err)
	}
	return nil
}

// RemoveLabel removes a label from a bead.
func (c *Client) RemoveLabel(ctx context.Context, beadID, label string) error {
	path := "/v1/beads/" + url.PathEscape(beadID) + "/labels/" + url.PathEscape(label)
	if err := c.doJSON(ctx, http.MethodDelete, path, nil, nil); err != nil {
		return fmt.Errorf("removing label %q from %s: %w", label, beadID, err)
	}
	return nil
}

// SetLabels replaces a bead's ENTIRE label set in one round-trip, and is the
// atomic primitive behind `gb advice relabel` (ka-0000003820). The daemon's
// PATCH /v1/beads/{id} treats a non-empty `labels` array as a full SET (not a
// merge): handleUpdateBead flips labelsSet, and updateBead then calls
// reconcileLabels inside the same request, computing the add/remove delta
// against the labels it just read. So the add and the remove commit together —
// there is no window where the bead is double-scoped, which is exactly the
// broken intermediate state a caller stringing `AddLabel` then `RemoveLabel`
// can leave behind if the second call fails or is forgotten.
//
// Because this REPLACES rather than merges, callers must pass the complete
// desired set — any label omitted here is removed from the bead. Compute the
// target set from a fresh GetBead read (see relabelledLabels in cmd/gb).
//
// An empty/nil `labels` is refused client-side rather than sent: the daemon
// infers set-ness from a non-empty array (`if in.Labels != nil` on HTTP, and
// `len(req.Labels) > 0` on gRPC), so a wire-empty array is an ambiguous
// no-op-or-wipe. Any caller that genuinely wants to strip every label must use
// RemoveLabel per label and say so explicitly.
func (c *Client) SetLabels(ctx context.Context, beadID string, labels []string) error {
	if len(labels) == 0 {
		return fmt.Errorf("setting labels on %s: refusing to send an empty label set "+
			"(PATCH infers set-vs-noop from a non-empty array, so this is ambiguous); "+
			"remove labels individually with RemoveLabel instead", beadID)
	}
	body := map[string]any{"labels": labels}
	path := "/v1/beads/" + url.PathEscape(beadID)
	if err := c.doJSON(ctx, http.MethodPatch, path, body, nil); err != nil {
		return fmt.Errorf("setting labels on %s to [%s]: %w", beadID, strings.Join(labels, ", "), err)
	}
	return nil
}
