package server

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"gardenio/kbeads/internal/store"
)

// LinkOperatorAliasRequest is the body for POST /v1/operators/me/aliases.
// The caller's OIDC operator (resolved from auth context) is the link target;
// (kind, subject) is the alias being attached. Source records why the alias
// was written (e.g. "identity-link" for the gb identity link-operator flow).
type LinkOperatorAliasRequest struct {
	Kind    string `json:"kind"`
	Subject string `json:"subject"`
	Source  string `json:"source"`
}

// LinkOperatorAliasResponse describes the outcome of a link attempt. Merged,
// MergedFromID and RowsRepointed are kept for wire compatibility with older
// CLIs and are always zero: this endpoint never merges operators (see
// handleLinkOperatorAlias).
type LinkOperatorAliasResponse struct {
	OperatorID    int64  `json:"operator_id"`
	Kind          string `json:"kind"`
	Subject       string `json:"subject"`
	Merged        bool   `json:"merged"`
	MergedFromID  int64  `json:"merged_from_id,omitempty"`
	RowsRepointed int64  `json:"rows_repointed,omitempty"`
	AlreadyLinked bool   `json:"already_linked,omitempty"`
}

// errSelfLinkCollision is returned from the link transaction when the alias
// already belongs to a different operator.
type errSelfLinkCollision struct{ owner int64 }

func (e *errSelfLinkCollision) Error() string { return "alias already belongs to another operator" }

// callerIsOIDCAdmin reports whether the request carries an OIDC
// gardenio-admin. Unlike isAdminCaller it checks the role itself: this route
// is registered at RoleOperator, so reaching the handler proves nothing.
func callerIsOIDCAdmin(ctx context.Context) bool {
	return authMethodFromContext(ctx) == authMethodOIDC && hasMinimumRole(RolesFromContext(ctx), RoleAdmin)
}

// userBeadBacksSelfLink reports whether an admin-written type:user bead proves
// that the OIDC caller (email) owns the alias (kind, subject): the bead must
// carry that alias, carry the caller's email (the bead's `email`), and
// every identity field that supplies either must have been last written by a
// human actor, not a token (see userIdentityWriters).
func (s *BeadsServer) userBeadBacksSelfLink(ctx context.Context, email, kind, subject string) (bool, error) {
	beads, err := s.listUserBeads(ctx)
	if err != nil {
		return false, err
	}
	email = canonicalAliasSubject("oidc", email)
	for _, b := range beads {
		var haveAlias, haveEmail bool
		for _, ref := range userBeadAliasRefs(b.Fields) {
			switch {
			case ref.Kind == kind && ref.Subject == subject:
				haveAlias = true
			case ref.Kind == "oidc" && ref.Subject == email:
				haveEmail = true
			}
		}
		if !haveAlias || !haveEmail {
			continue
		}
		muts, err := s.store.ListMutations(ctx, b.ID)
		if err != nil {
			return false, err
		}
		writers := userIdentityWriters(b.Fields, muts)
		human := true
		for _, k := range append(identityKeysForAlias(kind), identityKeysForAlias("oidc")...) {
			if w, set := writers[k]; set && !isHumanActor(w) {
				human = false
			}
		}
		if human {
			return true, nil
		}
	}
	return false, nil
}

// handleLinkOperatorAlias handles POST /v1/operators/me/aliases.
//
// Self-serve identity link: the OIDC-authenticated caller attaches an alias
// (kind, subject) to their own operator row. Idempotent — re-running the
// same request after the alias is already linked returns AlreadyLinked=true.
//
// Trust model (gd-0DMfxQdIow): the route only requires RoleOperator, so the
// daemon must prove the alias is the caller's. Before this check any operator
// could claim slack:<someone else's id> and have that person's operator merged
// into theirs. Now:
//
//   - A non-admin caller may only link an alias that already sits on an
//     admin-written type:user bead whose email matches the caller's OIDC
//     email (userBeadBacksSelfLink); otherwise 403.
//   - An OIDC gardenio-admin may link any non-oidc alias to their own operator
//     (the explicit admin path; admins can also use
//     POST /v1/admin/operators/link-aliases).
//   - This endpoint NEVER merges. If the alias already belongs to a different
//     operator the answer is 409 (logged), and nothing is written. Merging
//     operators rewrites operated_by_id across five audit tables and stays an
//     explicit gardenio-admin action on /v1/admin/operators/link-aliases.
func (s *BeadsServer) handleLinkOperatorAlias(w http.ResponseWriter, r *http.Request) {
	id := AuthIdentityFromContext(r.Context())
	if id.Type != "oidc" || id.Subject == "" {
		writeError(w, http.StatusForbidden, "identity link requires OIDC authentication")
		return
	}

	var req LinkOperatorAliasRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	kind := strings.TrimSpace(req.Kind)
	subject := canonicalAliasSubject(kind, req.Subject)
	source := strings.TrimSpace(req.Source)
	if kind == "" || subject == "" {
		writeError(w, http.StatusBadRequest, "kind and subject are required")
		return
	}
	if kind == "oidc" {
		writeError(w, http.StatusBadRequest, "kind=oidc is reserved for the caller's own operator")
		return
	}
	if source == "" {
		source = "identity-link"
	}

	admin := callerIsOIDCAdmin(r.Context())
	if !admin {
		backed, err := s.userBeadBacksSelfLink(r.Context(), id.Subject, kind, subject)
		if err != nil {
			writeServerError(r.Context(), w, "handleLinkOperatorAlias", err, "failed to verify identity link")
			return
		}
		if !backed {
			slog.Warn("operator alias self-link refused: no admin-written user bead proves it",
				"kind", kind, "auth_subject", id.Subject, "source", source)
			writeError(w, http.StatusForbidden,
				"insufficient permissions: this alias is not recorded for your email on an admin-written user bead; ask a "+RoleAdmin+" to link it")
			return
		}
	}

	var resp LinkOperatorAliasResponse
	resp.Kind = kind
	resp.Subject = subject

	err := s.store.RunInTransaction(r.Context(), func(tx store.Store) error {
		// Upsert the caller's OIDC operator. Cold path inserts; hot path
		// returns the existing id without touching the row.
		newID, err := tx.UpsertOperator(r.Context(), "oidc", id.Subject)
		if err != nil {
			return err
		}
		// Stamp the canonical OIDC alias so future resolver lookups for
		// oidc:<email> hit the alias index instead of the cold path.
		if err := tx.UpsertOperatorAlias(r.Context(), newID, "oidc", id.Subject, source); err != nil {
			return err
		}
		resp.OperatorID = newID

		oldID, lookupErr := tx.LookupOperatorAlias(r.Context(), kind, subject)
		switch {
		case lookupErr != nil && !errors.Is(lookupErr, sql.ErrNoRows):
			return lookupErr
		case lookupErr == nil && oldID == newID:
			// Already linked to the caller's operator — idempotent no-op.
			resp.AlreadyLinked = true
			return nil
		case lookupErr == nil && oldID != 0:
			// Owned by a different operator: never merge here.
			return &errSelfLinkCollision{owner: oldID}
		}
		return tx.UpsertOperatorAlias(r.Context(), newID, kind, subject, source)
	})
	var collision *errSelfLinkCollision
	if errors.As(err, &collision) {
		slog.Warn("operator alias self-link refused: alias belongs to a different operator",
			"kind", kind, "owner_operator_id", collision.owner, "auth_subject", id.Subject,
			"admin", admin, "source", source)
		writeErrorCode(w, http.StatusConflict,
			"alias already belongs to a different operator; merging operators needs an OIDC "+RoleAdmin+" via POST /v1/admin/operators/link-aliases",
			"alias_collision")
		return
	}
	if err != nil {
		writeServerError(r.Context(), w, "handleLinkOperatorAlias", err, "failed to link operator alias")
		return
	}
	writeJSON(w, http.StatusOK, &resp)
}
