package beadsapi

import (
	"context"
	"fmt"
)

// LandPurgeRequest is the body of POST /v1/lands/purge (kbeads
// http_lands_purge.go, Lands step 8.11). Exactly one of Land and
// ApplicationID is set.
type LandPurgeRequest struct {
	Land           string `json:"land,omitempty"`
	ApplicationID  string `json:"application_id,omitempty"`
	IncludeRecords bool   `json:"include_records,omitempty"`
	DryRun         bool   `json:"dry_run,omitempty"`
}

// LandPurgeResult lists the beads the purge deleted (or, on a dry run, would
// delete) and each one's type.
type LandPurgeResult struct {
	Purged []string          `json:"purged"`
	Types  map[string]string `json:"types"`
}

// PurgeLand hard-deletes what req selects: a closed tenant land's beads
// (keeping its project bead and, unless IncludeRecords, its credit ledger),
// or one denied/withdrawn plot application. The daemon admits only an OIDC
// admin or the lands provisioner's machine principal.
func (c *Client) PurgeLand(ctx context.Context, req LandPurgeRequest) (*LandPurgeResult, error) {
	var out LandPurgeResult
	if err := c.doJSON(ctx, "POST", "/v1/lands/purge", req, &out); err != nil {
		target := req.Land
		if target == "" {
			target = req.ApplicationID
		}
		return nil, fmt.Errorf("purging %s: %w", target, err)
	}
	return &out, nil
}
