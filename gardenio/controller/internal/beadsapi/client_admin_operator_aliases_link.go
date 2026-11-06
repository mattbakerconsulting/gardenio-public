package beadsapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

// OperatorAliasRef is one (kind, subject) reference to an operator alias —
// mirrors server.OperatorAliasRef.
type OperatorAliasRef struct {
	Kind    string `json:"kind"`
	Subject string `json:"subject"`
}

// LinkOperatorAliasesRequest is the body for
// POST /v1/admin/operators/link-aliases.
type LinkOperatorAliasesRequest struct {
	// Aliases is an explicit alias set. The daemon accepts it only from an
	// OIDC gardenio-admin; a static or fleet token gets 403 (gd-uAvleAZUvV).
	Aliases []OperatorAliasRef `json:"aliases,omitempty"`
	Source  string             `json:"source,omitempty"`
	// NoMerge asks the daemon to refuse (409) rather than merge operators when
	// the aliases already span more than one.
	NoMerge bool `json:"no_merge,omitempty"`
	// ResyncBeadID asks the daemon to link the identity fields of this
	// type:user bead. It is the only form a static or fleet token may send.
	ResyncBeadID string `json:"resync_bead_id,omitempty"`
}

// LinkOperatorAliasesResponse mirrors server.AdminLinkOperatorAliasesResponse.
// MergedFromIDs lists every operator id whose alias was repointed to the
// surviving operator; RowsRepointed is the total audit-FK rows moved across
// all merges. NothingToLink is set on a resync of a bead with fewer than two
// linkable identities.
type LinkOperatorAliasesResponse struct {
	OperatorID      int64   `json:"operator_id"`
	AliasesInserted int     `json:"aliases_inserted"`
	AliasesUpdated  int     `json:"aliases_updated"`
	AlreadyLinked   bool    `json:"already_linked"`
	NothingToLink   bool    `json:"nothing_to_link,omitempty"`
	MergedFromIDs   []int64 `json:"merged_from_ids,omitempty"`
	RowsRepointed   int64   `json:"rows_repointed"`
}

// ResyncUserBeadAliases asks the daemon to link the identity aliases carried by
// the type:user bead beadID to a single operator. The daemon reads the aliases
// from the bead itself (fields only an OIDC gardenio-admin can write), so a
// static or fleet token cannot choose them. It never merges operators: if the
// bead's aliases already point at more than one operator the daemon refuses
// with 409 (see IsAliasCollision) and writes nothing. This is what the
// Slack↔OIDC alias resync loop calls.
func (c *Client) ResyncUserBeadAliases(ctx context.Context, beadID string) (*LinkOperatorAliasesResponse, error) {
	body := LinkOperatorAliasesRequest{ResyncBeadID: beadID}
	var resp LinkOperatorAliasesResponse
	if err := c.doJSON(ctx, http.MethodPost, "/v1/admin/operators/link-aliases", body, &resp); err != nil {
		return nil, fmt.Errorf("resync user bead aliases: %w", err)
	}
	return &resp, nil
}

// LinkOperatorAliases ensures every supplied (kind, subject) alias resolves to
// a single operator row, creating the operator and missing alias rows as
// needed. Idempotent — replaying the same aliases is a no-op
// (AlreadyLinked=true).
//
// The daemon accepts an explicit alias list only from an OIDC gardenio-admin
// (403 for the static / fleet token every agent pod and bridge holds): a caller
// that could name aliases could attach any identity to any operator
// (gd-uAvleAZUvV). The request is sent with no_merge=true, so it also refuses
// (409, see IsAliasCollision) rather than merge operators that already hold
// the aliases; a merge stays an explicit admin action. Background loops use
// ResyncUserBeadAliases instead.
func (c *Client) LinkOperatorAliases(ctx context.Context, aliases []OperatorAliasRef, source string) (*LinkOperatorAliasesResponse, error) {
	body := LinkOperatorAliasesRequest{Aliases: aliases, Source: source, NoMerge: true}
	var resp LinkOperatorAliasesResponse
	if err := c.doJSON(ctx, http.MethodPost, "/v1/admin/operators/link-aliases", body, &resp); err != nil {
		return nil, fmt.Errorf("link operator aliases: %w", err)
	}
	return &resp, nil
}

// IsAliasCollision reports whether err is the daemon's 409 refusal of a
// no-merge link whose aliases already point at different operators.
func IsAliasCollision(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict
}

// PruneOperatorAliasesRequest is the body for
// POST /v1/admin/operators/prune-aliases. Keep is for an OIDC admin pruning a
// source other than user-bead-bridge; the daemon rejects it from a machine
// token.
type PruneOperatorAliasesRequest struct {
	Source string             `json:"source"`
	Keep   []OperatorAliasRef `json:"keep,omitempty"`
}

// PruneOperatorAliasesResponse mirrors server.AdminPruneOperatorAliasesResponse.
type PruneOperatorAliasesResponse struct {
	Deleted []OperatorAliasRef `json:"deleted"`
	Count   int                `json:"count"`
}

// PruneUserBeadAliases is the revocation half of the user-bead alias bridge: it
// asks the daemon to delete every source=user-bead-bridge alias row that no
// type:user bead carries any more, so an identity removed from a bead stops
// resolving to its operator. The daemon computes the keep-set from the user
// beads; the caller cannot supply one.
func (c *Client) PruneUserBeadAliases(ctx context.Context) (*PruneOperatorAliasesResponse, error) {
	body := PruneOperatorAliasesRequest{Source: "user-bead-bridge"}
	var resp PruneOperatorAliasesResponse
	if err := c.doJSON(ctx, http.MethodPost, "/v1/admin/operators/prune-aliases", body, &resp); err != nil {
		return nil, fmt.Errorf("prune user bead aliases: %w", err)
	}
	return &resp, nil
}
