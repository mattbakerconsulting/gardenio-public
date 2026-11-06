package beadsapi

import (
	"context"
	"fmt"
	"net/http"
)

// LinkOperatorAliasRequest is the body for POST /v1/operators/me/aliases.
// The daemon resolves the caller's OIDC operator from the auth context; the
// (Kind, Subject) pair identifies the alias being attached. Source records
// why the alias was written (e.g. "identity-link").
type LinkOperatorAliasRequest struct {
	Kind    string `json:"kind"`
	Subject string `json:"subject"`
	Source  string `json:"source"`
}

// LinkOperatorAliasResponse describes the outcome of a link attempt. When the
// (kind, subject) alias previously pointed at a different operator row, the
// daemon repoints the alias AND rewrites every operated_by_id reference to
// the surviving OIDC operator; MergedFromID and RowsRepointed surface that
// work to the caller.
type LinkOperatorAliasResponse struct {
	OperatorID    int64  `json:"operator_id"`
	Kind          string `json:"kind"`
	Subject       string `json:"subject"`
	Merged        bool   `json:"merged"`
	MergedFromID  int64  `json:"merged_from_id,omitempty"`
	RowsRepointed int64  `json:"rows_repointed,omitempty"`
	AlreadyLinked bool   `json:"already_linked,omitempty"`
}

// LinkOperatorAlias attaches (kind, subject) to the caller's OIDC operator
// row. Idempotent; when the alias already points at a different operator,
// the daemon merges that operator's audit references into the caller's
// OIDC operator in a single transaction.
func (c *Client) LinkOperatorAlias(ctx context.Context, req LinkOperatorAliasRequest) (*LinkOperatorAliasResponse, error) {
	var resp LinkOperatorAliasResponse
	if err := c.doJSON(ctx, http.MethodPost, "/v1/operators/me/aliases", req, &resp); err != nil {
		return nil, fmt.Errorf("link operator alias %s/%s: %w", req.Kind, req.Subject, err)
	}
	return &resp, nil
}
