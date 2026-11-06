package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/store"
)

// AdminLinkOperatorAliasesRequest is the body for
// POST /v1/admin/operators/link-aliases. The endpoint ensures every supplied
// (kind, subject) alias points at a single survivor operator row, merging any
// displaced operators in a single transaction.
//
// Used by the Slack↔OIDC alias background resync in the controller bridge
// (ka-0000000483 / ka-0000003795): the bridge walks user beads with both a
// Slack id and an OIDC email and calls this endpoint to ensure both
// identities resolve to the same operator row.
//
// A machine caller (the shared static token every agent pod holds, or a fleet
// token) must NOT choose which aliases to link: any (kind, subject) it named
// would be attached to whichever operator already owns another of them, which
// is how an agent token linked slack:UATTACKER to the owner's operator
// (gd-uAvleAZUvV review). A machine caller may only send ResyncBeadID — "re-
// derive bead X's aliases" — and the daemon reads the identity fields from that
// type:user bead itself. Those fields are writable only by an OIDC
// gardenio-admin (authorizeUserIdentityFields, on every write path), so the
// alias set is exactly what an admin put on the bead. The machine path is
// always no-merge: if the bead's aliases already span more than one operator
// the request is refused with 409 and nothing is written, and MergeOperators
// is never called. A merge, which rewrites operated_by_id across five audit
// tables, and an explicit Aliases list stay an action by an OIDC
// gardenio-admin.
type AdminLinkOperatorAliasesRequest struct {
	// Aliases is the explicit alias set. OIDC gardenio-admin only.
	Aliases []OperatorAliasRef `json:"aliases,omitempty"`
	Source  string             `json:"source"`
	// NoMerge makes an admin caller refuse (409) instead of merging. Implied
	// for machine callers, which cannot merge at all.
	NoMerge bool `json:"no_merge,omitempty"`
	// ResyncBeadID names a type:user bead whose identity fields the daemon
	// links. Mutually exclusive with Aliases; the source is always
	// user-bead-bridge.
	ResyncBeadID string `json:"resync_bead_id,omitempty"`
}

// errAliasCollision is returned from the link transaction in no-merge mode
// when the supplied aliases already point at more than one operator.
type errAliasCollision struct{ operators []int64 }

func (e *errAliasCollision) Error() string {
	return fmt.Sprintf("aliases already resolve to %d different operators", len(e.operators))
}

// canonicalAliasSubject normalises an alias subject the same way the write
// path normalises the operator it resolves (canonicalizeHumanOperator
// lowercases oidc subjects), so a mixed-case email cannot create an alias row
// no write will ever match. gitlab and github logins are case-insensitive too
// (the bridge user directory matches them so); migration 054 case-folded the
// gitlab/github rows written before gitlab was added here (gd-9AXizUgtzG).
func canonicalAliasSubject(kind, subject string) string {
	subject = strings.TrimSpace(subject)
	switch kind {
	case "oidc", "gitlab", "github":
		return strings.ToLower(subject)
	}
	return subject
}

// isAdminCaller reports whether the caller may choose aliases and merge
// operators. An OIDC caller reaching a handler behind requireRoleMachineAllowed
// (RoleAdmin) already holds gardenio-admin; none keeps its legacy authority.
// Every other caller (static or fleet token, and a v1 edge envelope, which the
// route refuses anyway: gd-AvSKOUyqRz) is a machine.
func isAdminCaller(ctx context.Context) bool {
	switch authMethodFromContext(ctx) {
	case authMethodOIDC, authMethodNone:
		return true
	}
	return false
}

// OperatorAliasRef is one (kind, subject) reference to an operator alias.
type OperatorAliasRef struct {
	Kind    string `json:"kind"`
	Subject string `json:"subject"`
}

// AdminLinkOperatorAliasesResponse describes the outcome of one link
// operation. OperatorID is the surviving operator. AliasesInserted counts
// alias rows that were newly created. MergedFromIDs lists every operator id
// whose alias was repointed to the survivor; RowsRepointed is the total
// number of audit-FK rows moved across all merges.
type AdminLinkOperatorAliasesResponse struct {
	OperatorID      int64 `json:"operator_id"`
	AliasesInserted int   `json:"aliases_inserted"`
	AliasesUpdated  int   `json:"aliases_updated"`
	AlreadyLinked   bool  `json:"already_linked"`
	// NothingToLink is set on a resync_bead_id request whose bead carries
	// fewer than two linkable identities: there is nothing to link, which is
	// not an error.
	NothingToLink bool    `json:"nothing_to_link,omitempty"`
	MergedFromIDs []int64 `json:"merged_from_ids,omitempty"`
	RowsRepointed int64   `json:"rows_repointed"`
}

// handleAdminLinkOperatorAliases handles POST /v1/admin/operators/link-aliases.
//
// Behaviour:
//   - Lookup each supplied alias.
//   - Pick a survivor operator: prefer an oidc-kind row already pointed at by
//     one of the aliases; otherwise the first existing operator; otherwise
//     create a new operator from the first alias (preferring oidc kind).
//   - Upsert every alias to point at the survivor. Each alias that previously
//     pointed at a *different* operator id triggers a MergeOperators call to
//     repoint audit-FK rows; that operator id is recorded in MergedFromIDs.
//
// The whole pass runs in one RunInTransaction so partial failures roll back.
// Idempotent — re-running with the same aliases is a no-op (AlreadyLinked=true).
func (s *BeadsServer) handleAdminLinkOperatorAliases(w http.ResponseWriter, r *http.Request) {
	var req AdminLinkOperatorAliasesRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	admin := isAdminCaller(r.Context())
	noMerge := req.NoMerge
	source := strings.TrimSpace(req.Source)
	aliases := req.Aliases
	resync := strings.TrimSpace(req.ResyncBeadID) != ""
	switch {
	case resync:
		if len(req.Aliases) > 0 {
			writeError(w, http.StatusBadRequest, "aliases and resync_bead_id are mutually exclusive")
			return
		}
		bead, err := s.store.GetBead(r.Context(), strings.TrimSpace(req.ResyncBeadID))
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			writeServerError(r.Context(), w, "handleAdminLinkOperatorAliases", err, "failed to fetch bead")
			return
		}
		if bead == nil {
			writeError(w, http.StatusNotFound, "bead not found")
			return
		}
		if bead.Type != "user" {
			writeError(w, http.StatusBadRequest, "resync_bead_id must name a type:user bead")
			return
		}
		aliases = userBeadAliasRefs(bead.Fields)
		source = userBeadBridgeAliasSource
		if !admin {
			noMerge = true
		}
		if len(aliases) < 2 {
			writeJSON(w, http.StatusOK, &AdminLinkOperatorAliasesResponse{NothingToLink: true})
			return
		}
	case !admin:
		writeError(w, http.StatusForbidden, "insufficient permissions: choosing aliases requires "+RoleAdmin+" (OIDC); machine callers may only send resync_bead_id")
		return
	}
	type cleanAlias struct {
		kind, subject string
	}
	clean := make([]cleanAlias, 0, len(aliases))
	seen := make(map[string]bool, len(aliases))
	for _, a := range aliases {
		k := strings.TrimSpace(a.Kind)
		sub := canonicalAliasSubject(k, a.Subject)
		if k == "" || sub == "" {
			writeError(w, http.StatusBadRequest, "every alias requires non-empty kind and subject")
			return
		}
		key := k + "\x00" + sub
		if seen[key] {
			continue
		}
		seen[key] = true
		clean = append(clean, cleanAlias{kind: k, subject: sub})
	}
	if len(clean) < 2 {
		writeError(w, http.StatusBadRequest, "at least two distinct aliases are required")
		return
	}
	if source == "" {
		source = userBeadBridgeAliasSource
	}

	var resp AdminLinkOperatorAliasesResponse

	err := s.store.RunInTransaction(r.Context(), func(tx store.Store) error {
		// Pass 1: resolve each alias.
		type resolved struct {
			cleanAlias
			operatorID int64
			found      bool
		}
		all := make([]resolved, 0, len(clean))
		distinctIDs := make(map[int64]bool)
		for _, a := range clean {
			id, err := tx.LookupOperatorAlias(r.Context(), a.kind, a.subject)
			switch {
			case err == nil:
				all = append(all, resolved{cleanAlias: a, operatorID: id, found: true})
				distinctIDs[id] = true
			case errors.Is(err, sql.ErrNoRows):
				all = append(all, resolved{cleanAlias: a, found: false})
			default:
				return err
			}
		}

		if noMerge && len(distinctIDs) > 1 {
			ids := make([]int64, 0, len(distinctIDs))
			for id := range distinctIDs {
				ids = append(ids, id)
			}
			return &errAliasCollision{operators: ids}
		}

		// Pick a survivor. Prefer an alias-kind=oidc row (canonical for human
		// identity); otherwise the first existing operator; otherwise create
		// a new operator from the first alias (preferring oidc kind).
		var survivor int64
		for _, r := range all {
			if r.found && r.kind == "oidc" {
				survivor = r.operatorID
				break
			}
		}
		if survivor == 0 {
			for _, r := range all {
				if r.found {
					survivor = r.operatorID
					break
				}
			}
		}
		if survivor == 0 {
			// No existing operator for any of the supplied aliases. Pick a
			// seed alias — prefer oidc — and create the operator row.
			seed := all[0]
			for _, r := range all {
				if r.kind == "oidc" {
					seed = r
					break
				}
			}
			newID, err := tx.UpsertOperator(r.Context(), seed.kind, seed.subject)
			if err != nil {
				return err
			}
			survivor = newID
		}
		resp.OperatorID = survivor

		// Pass 2: ensure every alias points at the survivor. Merge displaced
		// operators audit-FK rows into the survivor.
		mergedFrom := map[int64]bool{}
		for _, a := range all {
			switch {
			case !a.found:
				if err := tx.UpsertOperatorAlias(r.Context(), survivor, a.kind, a.subject, source); err != nil {
					return err
				}
				resp.AliasesInserted++
			case a.operatorID == survivor:
				// Already pointed at the survivor.
			default:
				if err := tx.UpsertOperatorAlias(r.Context(), survivor, a.kind, a.subject, source); err != nil {
					return err
				}
				resp.AliasesUpdated++
				if !mergedFrom[a.operatorID] {
					moved, err := tx.MergeOperators(r.Context(), a.operatorID, survivor)
					if err != nil {
						return err
					}
					mergedFrom[a.operatorID] = true
					resp.MergedFromIDs = append(resp.MergedFromIDs, a.operatorID)
					resp.RowsRepointed += moved
				}
			}
		}
		if resp.AliasesInserted == 0 && resp.AliasesUpdated == 0 && len(resp.MergedFromIDs) == 0 {
			resp.AlreadyLinked = true
		}
		return nil
	})
	var collision *errAliasCollision
	if errors.As(err, &collision) {
		kinds := make([]string, 0, len(clean))
		for _, a := range clean {
			kinds = append(kinds, a.kind)
		}
		id := AuthIdentityFromContext(r.Context())
		slog.Warn("link-aliases refused: aliases already point at different operators",
			"kinds", strings.Join(kinds, "+"), "operator_ids", collision.operators,
			"source", source, "auth_type", id.Type, "auth_subject", id.Subject)
		writeErrorCode(w, http.StatusConflict,
			"aliases already resolve to different operators; a merge needs an OIDC "+RoleAdmin,
			"alias_collision")
		return
	}
	if err != nil {
		writeServerError(r.Context(), w, "handleAdminLinkOperatorAliases", err, "failed to link operator aliases")
		return
	}
	writeJSON(w, http.StatusOK, &resp)
}

// AdminPruneOperatorAliasesRequest is the body for
// POST /v1/admin/operators/prune-aliases: delete every alias row written
// under Source that is not in the keep set. For source=user-bead-bridge the
// keep set is computed by the daemon from the identity fields of the type:user
// beads themselves, so an identity removed from a bead stops resolving on the
// next cycle (gd-uAvleAZUvV) and NO caller can supply it: a machine caller
// that sent its own list could keep or drop whatever it liked (a dummy keep
// entry deleted every bridge alias, including the owner's Slack identity).
// Keep is accepted only from an OIDC gardenio-admin, and only for a source
// other than user-bead-bridge.
type AdminPruneOperatorAliasesRequest struct {
	Source string             `json:"source"`
	Keep   []OperatorAliasRef `json:"keep,omitempty"`
}

// AdminPruneOperatorAliasesResponse lists the (kind, subject) rows deleted.
type AdminPruneOperatorAliasesResponse struct {
	Deleted []OperatorAliasRef `json:"deleted"`
	Count   int                `json:"count"`
}

// userBeadBridgeAliasSource is the source the user-bead alias bridge writes
// under, and the only source a machine caller may prune.
const userBeadBridgeAliasSource = "user-bead-bridge"

// listUserBeads pages through every type:user bead the store returns.
func (s *BeadsServer) listUserBeads(ctx context.Context) ([]*model.Bead, error) {
	const pageSize = 500
	var all []*model.Bead
	for offset := 0; ; offset += pageSize {
		page, _, err := s.store.ListBeads(ctx, model.BeadFilter{
			Type:   []model.BeadType{"user"},
			Limit:  pageSize,
			Offset: offset,
		})
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		if len(page) < pageSize {
			return all, nil
		}
	}
}

// handleAdminPruneOperatorAliases handles POST /v1/admin/operators/prune-aliases.
//
// Only rows whose source equals the request's source are eligible, so the
// OIDC login path's canonical aliases ("resolver"), migrations and admin links
// can never be revoked by a bridge reconcile. A machine caller (static/fleet
// token) may prune only source=user-bead-bridge and may not send keep; an OIDC
// gardenio-admin may prune any source, with an explicit non-empty keep for
// every source but user-bead-bridge. If the daemon computes an empty keep set
// (no user bead carries any alias) it prunes nothing: a missing or empty scan
// must never be able to wipe the table.
func (s *BeadsServer) handleAdminPruneOperatorAliases(w http.ResponseWriter, r *http.Request) {
	var req AdminPruneOperatorAliasesRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	source := strings.TrimSpace(req.Source)
	if source == "" {
		writeError(w, http.StatusBadRequest, "source is required")
		return
	}
	admin := isAdminCaller(r.Context())
	if !admin {
		if source != userBeadBridgeAliasSource {
			writeError(w, http.StatusForbidden, "insufficient permissions: machine callers may only prune source "+userBeadBridgeAliasSource)
			return
		}
		if len(req.Keep) > 0 {
			writeError(w, http.StatusForbidden, "insufficient permissions: choosing the keep set requires "+RoleAdmin+" (OIDC); the daemon computes it from the user beads")
			return
		}
	}
	var keep []store.OperatorAliasKey
	if source == userBeadBridgeAliasSource {
		if len(req.Keep) > 0 {
			writeError(w, http.StatusBadRequest, "keep is not accepted for source "+userBeadBridgeAliasSource+": the daemon computes it from the user beads")
			return
		}
		beads, err := s.listUserBeads(r.Context())
		if err != nil {
			writeServerError(r.Context(), w, "handleAdminPruneOperatorAliases", err, "failed to list user beads")
			return
		}
		seen := map[store.OperatorAliasKey]bool{}
		for _, b := range beads {
			for _, a := range userBeadAliasRefs(b.Fields) {
				k := store.OperatorAliasKey{Kind: a.Kind, Subject: a.Subject}
				if !seen[k] {
					seen[k] = true
					keep = append(keep, k)
				}
			}
		}
		if len(keep) == 0 {
			slog.Warn("prune-aliases skipped: no user bead carries any alias", "source", source)
			writeJSON(w, http.StatusOK, &AdminPruneOperatorAliasesResponse{Deleted: []OperatorAliasRef{}})
			return
		}
	} else {
		for _, a := range req.Keep {
			k := strings.TrimSpace(a.Kind)
			sub := canonicalAliasSubject(k, a.Subject)
			if k == "" || sub == "" {
				writeError(w, http.StatusBadRequest, "every keep entry requires non-empty kind and subject")
				return
			}
			keep = append(keep, store.OperatorAliasKey{Kind: k, Subject: sub})
		}
		if len(keep) == 0 {
			writeError(w, http.StatusBadRequest, "keep must list at least one alias")
			return
		}
	}
	deleted, err := s.store.PruneOperatorAliases(r.Context(), source, keep)
	if err != nil {
		writeServerError(r.Context(), w, "handleAdminPruneOperatorAliases", err, "failed to prune operator aliases")
		return
	}
	resp := AdminPruneOperatorAliasesResponse{Deleted: make([]OperatorAliasRef, 0, len(deleted)), Count: len(deleted)}
	for _, d := range deleted {
		resp.Deleted = append(resp.Deleted, OperatorAliasRef{Kind: d.Kind, Subject: d.Subject})
	}
	if len(deleted) > 0 {
		id := AuthIdentityFromContext(r.Context())
		slog.Info("operator aliases pruned", "source", source, "count", len(deleted),
			"auth_type", id.Type, "auth_subject", id.Subject)
	}
	writeJSON(w, http.StatusOK, &resp)
}
