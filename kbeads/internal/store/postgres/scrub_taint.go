package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"gardenio/kbeads/internal/idgen"
	"gardenio/kbeads/internal/model"
	"github.com/lib/pq"
)

// This file implements Store.ScrubTaint (docs/adr/taints.md §6.5,
// gd-fVAy32nWDf): the execution of an approved scrub request against every
// bead it names, in one transaction. It follows the same PostgresStore/
// txStore delegation pattern as subfield_edits.go: the PostgresStore entry
// point opens its own *sql.Tx (so a `FOR UPDATE` lock spans the whole
// per-bead read-modify-write), while txStore delegates to the same free
// function using the caller's existing transaction.
//
// purge and detaint/strip share the cascadeContent helper below — the exact
// per-table cascade docs/adr/taints.md §6.5 specifies, over the tables named
// by cascadeContentTables (scrub_cascade_tables.go). detaint/fork does not
// call cascadeContent at all: it closes the original bead and forks a
// successor instead of touching the original's content. detaint/lift (§15.4,
// gd-XAXEBMrKOD) doesn't call it either: it deletes the bead_taints row in
// place and touches nothing else — legal only for a taint whose effects
// exclude "gate", checked here as well as by the policy validator
// (defence-in-depth, ValidateScrubPolicy). §15.4's rate rule — a lift for a
// high-rate integrity taint must be fired by the approval that already
// exists for the underlying action (merge / plan approve), never by a
// per-bead scrub request — is a design constraint on CALLERS; ScrubTaint has
// no notion of "the underlying action's approval" and does not enforce it.

func (s *PostgresStore) ScrubTaint(ctx context.Context, req *model.ScrubTaintRequest) (*model.ScrubTaintResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after successful commit
	result, err := queryScrubTaint(ctx, tx, req)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return result, nil
}

func (s *txStore) ScrubTaint(ctx context.Context, req *model.ScrubTaintRequest) (*model.ScrubTaintResult, error) {
	return queryScrubTaint(ctx, s.tx, req)
}

// queryScrubTaint dispatches each bead in req.BeadIDs to the branch its
// req.Kind/req.DetaintMode selects. db MUST be a live transaction so the
// per-bead `FOR UPDATE` lock (lockBeadForScrub) spans each bead's read and
// write.
func queryScrubTaint(ctx context.Context, db executor, req *model.ScrubTaintRequest) (*model.ScrubTaintResult, error) {
	result := &model.ScrubTaintResult{Beads: make([]model.ScrubTaintBeadResult, 0, len(req.BeadIDs))}

	for _, beadID := range req.BeadIDs {
		var (
			br  model.ScrubTaintBeadResult
			err error
		)

		switch req.Kind {
		case "purge":
			br, err = scrubPurgeBead(ctx, db, req, beadID)
		case "detaint":
			// Switched on the open model.DetaintMode string (not a two-value
			// bool) so a third arm — "lift" (gd-XAXEBMrKOD) — could slot in
			// without restructuring this switch, and now does.
			mode := req.DetaintMode
			if mode == "" {
				mode = model.DetaintModeFork // scrub_policy default, §6.1
			}
			switch mode {
			case model.DetaintModeFork:
				br, err = scrubDetaintForkBead(ctx, db, req, beadID)
			case model.DetaintModeStrip:
				br, err = scrubDetaintStripBead(ctx, db, req, beadID)
			case model.DetaintModeLift:
				br, err = scrubDetaintLiftBead(ctx, db, req, beadID)
			default:
				err = fmt.Errorf("scrub: detaint_mode %q is not implemented by ScrubTaint", mode)
			}
		default:
			err = fmt.Errorf("scrub: unknown kind %q (want %q or %q)", req.Kind, "purge", "detaint")
		}

		if err != nil {
			return nil, fmt.Errorf("scrub bead %s: %w", beadID, err)
		}
		result.Beads = append(result.Beads, br)
	}

	return result, nil
}

// resolveScrubOperatorID returns req.ActorOperatorID, falling back to the
// system:machine operator id (migration 036) when the approver identity
// didn't resolve to one (ActorOperatorID == 0). ScrubTaint always runs under
// WithMachineOperator (docs/adr/taints.md §6.5), and beads.operated_by_id /
// deps.operated_by_id are hard NOT NULL FKs while events/mutation_history
// carry a NOT VALID CHECK that Postgres still enforces against the new
// tuple on every UPDATE, not just INSERT (migration 026; gd-DWM1o3VZAC) — a
// scrub must never attempt to write a NULL/zero operator, on any table any
// branch touches: the three tables the fork branch inserts into, and the
// events/mutation_history rows purge and detaint/strip cascade through
// (cascadeContent's COALESCE(operated_by_id, ...)).
func resolveScrubOperatorID(ctx context.Context, db executor, req *model.ScrubTaintRequest) (int64, error) {
	if req.ActorOperatorID != 0 {
		return req.ActorOperatorID, nil
	}
	id, err := lookupOperatorAlias(ctx, db, "system", "machine")
	if err != nil {
		return 0, fmt.Errorf("resolve system:machine operator id: %w", err)
	}
	return id, nil
}

// lockBeadForScrub reads a single bead's full row under `FOR UPDATE` so the
// caller's subsequent write is atomic with this read within the same
// transaction. It deliberately does NOT filter on deleted_at: a scrub
// request's resolved_bead_ids is frozen at request-creation time
// (scrub_request.go) and can go stale by the time execution runs, including
// a bead that gets soft-deleted in between. Soft delete keeps every copy of
// a bead's content and is restorable (ADR taints.md §6.5, §12.E) — it is not
// "no live content to scrub" — so purge and detaint/strip must lock and
// scrub a soft-deleted bead exactly like a live one, or `kd restore` would
// revive the tainted content a scrub was supposed to have destroyed
// (gd-T5P13cSoBw). Returns sql.ErrNoRows only when no row exists for this id
// at all (hard-deleted or never existed) — a real data-integrity problem
// callers still treat as a hard failure. Callers that must instead SKIP a
// soft-deleted bead (detaint/fork, which forks a successor rather than
// scrubbing in place) check bead.DeletedAt themselves.
func lockBeadForScrub(ctx context.Context, db executor, beadID string) (*model.Bead, error) {
	row := db.QueryRowContext(ctx,
		`SELECT `+beadColumns+` FROM beads WHERE id = $1 FOR UPDATE`, beadID)
	return scanBead(row)
}

// hashBeadContent is the docs/adr/taints.md §6.5 content_sha. It delegates
// to model.BeadContentSHA — the same definition the attestation endpoint
// uses to seed content_shas — so taint_audit.content_sha and an
// attestation's content_shas can never silently disagree for identical
// bead content (gd-YQZUsNQRVP: this used to be a second, independently
// NUL-separated hash over the same fields, which agreed with
// BeadContentSHA on the collision-resistance property but not on the
// actual bytes hashed).
func hashBeadContent(title, description, notes string, fields json.RawMessage) string {
	return model.BeadContentSHA(&model.Bead{
		Title:       title,
		Description: description,
		Notes:       notes,
		Fields:      fields,
	})
}

// scrubContentReplacement is what cascadeContent overwrites a bead's own
// prose columns with. purge blanks every field; detaint/strip replaces them
// wholesale with the attested clean_content (never a partial patch).
type scrubContentReplacement struct {
	Title       string
	Description string
	Notes       string
	Fields      json.RawMessage
}

// purgeProseFromFields blanks the prose content out of a bead's fields blob
// while preserving every key and non-string value: strings (including
// strings nested in objects/arrays) become "", numbers/booleans/nulls are
// left untouched. Replacing the whole blob with `{}` (the original purge
// behavior) drops keys a custom type's TypeConfig may declare `required`,
// so a later PATCH to the purged bead 400s on a field that "went missing"
// (gd-a8c9mEme1G). Blanking values in place instead of deleting keys keeps
// every required field present-and-non-nil — the only two properties
// validateFieldsImpl checks — while still destroying the tainted prose a
// purge exists to destroy. The store layer has no access to a bead type's
// TypeConfig (that resolution lives in internal/server), so this can't
// special-case only the fields a schema marks as free text; treating every
// string value as prose is the schema-agnostic equivalent.
func purgeProseFromFields(fields json.RawMessage) (json.RawMessage, error) {
	if len(fields) == 0 {
		return json.RawMessage(`{}`), nil
	}
	var m map[string]any
	if err := json.Unmarshal(fields, &m); err != nil {
		return nil, fmt.Errorf("unmarshal fields: %w", err)
	}
	for k, v := range m {
		m[k] = purgeProseValue(v)
	}
	out, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("marshal purged fields: %w", err)
	}
	return out, nil
}

// purgeProseValue recursively blanks string values within an arbitrary JSON
// value (as decoded by encoding/json into any), leaving the shape and every
// non-string value untouched.
func purgeProseValue(v any) any {
	switch val := v.(type) {
	case string:
		return ""
	case []any:
		out := make([]any, len(val))
		for i, e := range val {
			out[i] = purgeProseValue(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(val))
		for k, e := range val {
			out[k] = purgeProseValue(e)
		}
		return out
	default:
		return val
	}
}

// taintStillOnBead locks the (bead_id, taint_id) row in bead_taints FOR
// UPDATE — atomic, within the caller's transaction, with whatever write
// follows — and reports whether it still exists. A scrub request is built
// from a resolved bead/taint snapshot (approval time) that can go stale
// before execution runs: a prior scrub, an explicit untaint, or a concurrent
// scrub of the same pair can clear the row first. cascadeContent must never
// destroy a bead's content on behalf of a taint it no longer carries
// (gd-0fQDgy18lo) — content that was never actually sensitive at execution
// time would be needlessly destroyed.
func taintStillOnBead(ctx context.Context, db executor, beadID string, taintID int64) (bool, error) {
	var discard int
	err := db.QueryRowContext(ctx,
		`SELECT 1 FROM bead_taints WHERE bead_id = $1 AND taint_id = $2 FOR UPDATE`,
		beadID, taintID,
	).Scan(&discard)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	default:
		return false, fmt.Errorf("lock bead_taints: %w", err)
	}
}

// verifyAttestationNotDrifted re-checks a detaint bead's attested
// content_sha against the CURRENT content read under lockBeadForScrub's FOR
// UPDATE lock — the same row lock the cascade's subsequent write depends on
// — closing the TOCTOU window between handleApproveScrubRequest's
// checkAttestationDrift (an unlocked read, before this transaction even
// begins, possibly much earlier if approval and execution are separated in
// time) and the cascade below (gd-JnACECPcvy). req.ContentSHAs is nil for
// purge (no attestation exists for that kind, §6.4) and for any detaint
// request whose attestation carried no content_shas entries — both
// correctly no-op here, since there is nothing to have drifted from.
//
// Any mismatch aborts with an error, which the caller's transaction (begun
// in PostgresStore.ScrubTaint) rolls back in full — a bead that drifted
// after attestation must not be scrubbed on the strength of a stale
// clean_content, and per queryScrubTaint's contract a single bead's error
// aborts the whole request rather than skipping just that bead.
func verifyAttestationNotDrifted(orig *model.Bead, beadID string, contentSHAs map[string]string) error {
	if len(contentSHAs) == 0 {
		return nil
	}
	want, ok := contentSHAs[beadID]
	if !ok || want == "" {
		return fmt.Errorf("no attested content_sha on record for bead %s", beadID)
	}
	if got := model.BeadContentSHA(orig); got != want {
		return fmt.Errorf("content changed since attestation for bead %s (attested %s, current %s) — re-attest before approving", beadID, want, got)
	}
	return nil
}

// cascadeContent runs the docs/adr/taints.md §6.5 content cascade shared by
// purge and detaint/strip: it overwrites the bead's own prose with
// replacement, then blanks/deletes every OTHER table's copy of that bead's
// content, and finally lifts the taint marker itself. It ranges over
// cascadeContentTables (scrub_cascade_tables.go) rather than an inline list,
// so a table added there without a matching case here fails loudly instead
// of silently going unscrubbed. Returns the list of tables touched (always
// the full cascadeContentTables list, in order, on success) and ok=true.
//
// Before touching anything, it locks and re-checks that bead_taints still
// carries (beadID, taintID) (taintStillOnBead) — ok=false, no error, no
// write, means that row is already gone and the caller must skip this bead
// rather than cascade a destructive rewrite for a taint that no longer
// applies.
//
// operatorID is stamped onto any events/mutation_history row the cascade
// touches whose operated_by_id is still NULL (gd-DWM1o3VZAC): those two
// tables predate migration 026's `CHECK (operated_by_id IS NOT NULL) NOT
// VALID` and were deliberately left with legacy NULL rows (ADR human-
// attribution-identity-v2 §4.5) — but Postgres re-validates a NOT VALID
// CHECK against the NEW tuple on every UPDATE, not only on INSERT, so an
// UPDATE that leaves such a row's operated_by_id NULL still violates the
// constraint. COALESCE only fills the gap on a legacy NULL row; an
// already-attributed row keeps its original operator.
func cascadeContent(ctx context.Context, db executor, beadID string, taintID, operatorID int64, replacement scrubContentReplacement) ([]string, bool, error) {
	stillTainted, err := taintStillOnBead(ctx, db, beadID, taintID)
	if err != nil {
		return nil, false, err
	}
	if !stillTainted {
		return nil, false, nil
	}

	fields := replacement.Fields
	if len(fields) == 0 {
		fields = json.RawMessage(`{}`)
	}

	touched := make([]string, 0, len(cascadeContentTables))
	for _, tbl := range cascadeContentTables {
		switch tbl {
		case "beads":
			// search_tsv recomputes automatically via the existing
			// beads_search_tsv_update trigger (migrations 039/040) — no
			// manual handling needed here.
			_, err := db.ExecContext(ctx, `
				UPDATE beads
				SET title = $2, description = $3, notes = $4, fields = $5::jsonb, updated_at = NOW()
				WHERE id = $1`,
				beadID, replacement.Title, replacement.Description, replacement.Notes, []byte(fields),
			)
			if err != nil {
				return nil, false, fmt.Errorf("cascade beads: %w", err)
			}

		case "comments":
			// Rows survive — only the body is destroyed. author/created_at/
			// id are not content (ADR §1.3).
			if _, err := db.ExecContext(ctx,
				`UPDATE comments SET text = '[purged]' WHERE bead_id = $1`, beadID,
			); err != nil {
				return nil, false, fmt.Errorf("cascade comments: %w", err)
			}

		case "events":
			// topic/bead_id/actor/created_at are separate columns, not part
			// of payload — a plain payload-blanking UPDATE keeps them.
			// operated_by_id = COALESCE(...) satisfies migration 026's
			// NOT VALID CHECK on a legacy NULL row without disturbing one
			// that already carries a real operator.
			if _, err := db.ExecContext(ctx,
				`UPDATE events SET payload = '{}'::jsonb, operated_by_id = COALESCE(operated_by_id, $2) WHERE bead_id = $1`,
				beadID, operatorID,
			); err != nil {
				return nil, false, fmt.Errorf("cascade events: %w", err)
			}

		case "mutation_history":
			// Blank the prose diff itself and re-attribute those rows to the
			// scrub sentinel, so undoMutation refuses to reverse them. The
			// `field LIKE 'fields.%'` arm covers per-key subfield mutations
			// (http_beads.go's subfield-set/array-append/CAS/increment paths,
			// and auto_parent.go's fields.thread_id) recorded as
			// "fields.<key>" rather than the whole-blob "fields" row — those
			// carry the same tainted content and must be blanked too, or the
			// pre-scrub value stays readable via mutation_history.new_value.
			// operated_by_id = COALESCE(...), same rationale as events above.
			if _, err := db.ExecContext(ctx, `
				UPDATE mutation_history SET old_value = '', new_value = '', actor = $2, operated_by_id = COALESCE(operated_by_id, $3)
				WHERE bead_id = $1 AND (field IN ('title', 'description', 'notes', 'fields') OR field LIKE 'fields.%')`,
				beadID, model.SystemActorTaintScrub, operatorID,
			); err != nil {
				return nil, false, fmt.Errorf("cascade mutation_history: %w", err)
			}

		case "bead_embeddings":
			// A derived copy of the now-destroyed text — delete the row
			// rather than blank it; there is no "empty embedding" worth
			// keeping a row for.
			if _, err := db.ExecContext(ctx,
				`DELETE FROM bead_embeddings WHERE bead_id = $1`, beadID,
			); err != nil {
				return nil, false, fmt.Errorf("cascade bead_embeddings: %w", err)
			}

		case "deps":
			// Blank free-text metadata on any edge touching this bead on
			// either side; the edge itself (bead_id/depends_on_id/type)
			// survives.
			if _, err := db.ExecContext(ctx, `
				UPDATE deps SET metadata = '' WHERE (bead_id = $1 OR depends_on_id = $1) AND metadata != ''`,
				beadID,
			); err != nil {
				return nil, false, fmt.Errorf("cascade deps: %w", err)
			}

		case "session_gates":
			// Keyed by agent_bead_id, not bead_id.
			if _, err := db.ExecContext(ctx, `
				UPDATE session_gates SET metadata = NULL WHERE agent_bead_id = $1 AND metadata IS NOT NULL`,
				beadID,
			); err != nil {
				return nil, false, fmt.Errorf("cascade session_gates: %w", err)
			}

		default:
			return nil, false, fmt.Errorf("cascadeContent: no cascade implemented for table %q — cascadeContentTables "+
				"(scrub_cascade_tables.go) lists it but this switch has no case for it", tbl)
		}
		touched = append(touched, tbl)
	}

	// Lift the marker itself. Not part of cascadeContentTables — it is
	// classified as a non-content table in scrub_cascade_tables_test.go
	// (it has no prose to blank, only ids), but ScrubTaint still deletes it
	// as part of the same cascade.
	if _, err := db.ExecContext(ctx,
		`DELETE FROM bead_taints WHERE bead_id = $1 AND taint_id = $2`, beadID, taintID,
	); err != nil {
		return nil, false, fmt.Errorf("cascade bead_taints: %w", err)
	}

	return touched, true, nil
}

// scrubPurgeBead runs the "purge" branch for one bead: cascadeContent with
// every prose column blanked, then a taint_audit row hashing the pre-scrub
// content. If bead_taints no longer carries (beadID, req.TaintID) — the
// taint was already scrubbed or explicitly removed since this scrub request
// was built — it skips the cascade and audit entirely rather than
// destroying content that is no longer tainted (gd-0fQDgy18lo). A
// soft-deleted beadID is NOT skipped: lockBeadForScrub locks and returns it
// like any other bead, and it gets the same cascade + audit as a live one
// (gd-T5P13cSoBw) — soft delete is restorable, so skipping would leave
// tainted content for `kd restore` to revive.
func scrubPurgeBead(ctx context.Context, db executor, req *model.ScrubTaintRequest, beadID string) (model.ScrubTaintBeadResult, error) {
	operatorID, err := resolveScrubOperatorID(ctx, db, req)
	if err != nil {
		return model.ScrubTaintBeadResult{}, err
	}

	orig, err := lockBeadForScrub(ctx, db, beadID)
	if err != nil {
		return model.ScrubTaintBeadResult{}, fmt.Errorf("lock bead: %w", err)
	}
	contentSHA := hashBeadContent(orig.Title, orig.Description, orig.Notes, orig.Fields)

	purgedFields, err := purgeProseFromFields(orig.Fields)
	if err != nil {
		return model.ScrubTaintBeadResult{}, fmt.Errorf("purge fields: %w", err)
	}

	tables, ok, err := cascadeContent(ctx, db, beadID, req.TaintID, operatorID, scrubContentReplacement{
		Title:       "[purged]",
		Description: "",
		Notes:       "",
		Fields:      purgedFields,
	})
	if err != nil {
		return model.ScrubTaintBeadResult{}, err
	}
	if !ok {
		return model.ScrubTaintBeadResult{BeadID: beadID, Skipped: true, SkipReason: model.ScrubSkipTaintGone}, nil
	}

	if err := recordScrubAudit(ctx, db, req, beadID, contentSHA); err != nil {
		return model.ScrubTaintBeadResult{}, err
	}

	return model.ScrubTaintBeadResult{BeadID: beadID, ContentSHA: contentSHA, TablesTouched: tables}, nil
}

// scrubDetaintStripBead runs the "detaint" + strip branch for one bead: the
// same cascadeContent as purge, but the bead's prose is overwritten with the
// attested clean_content instead of being blanked, and status is untouched.
// Same stale-taint guard as scrubPurgeBead: if bead_taints no longer carries
// (beadID, req.TaintID), the cascade is skipped rather than overwriting
// content on behalf of a taint that's already gone (gd-0fQDgy18lo). Same
// soft-delete non-skip too — a soft-deleted bead is locked and stripped like
// any other (gd-T5P13cSoBw).
func scrubDetaintStripBead(ctx context.Context, db executor, req *model.ScrubTaintRequest, beadID string) (model.ScrubTaintBeadResult, error) {
	clean, ok := req.CleanContent[beadID]
	if !ok {
		return model.ScrubTaintBeadResult{}, fmt.Errorf("no attested clean_content for bead %s", beadID)
	}

	operatorID, err := resolveScrubOperatorID(ctx, db, req)
	if err != nil {
		return model.ScrubTaintBeadResult{}, err
	}

	orig, err := lockBeadForScrub(ctx, db, beadID)
	if err != nil {
		return model.ScrubTaintBeadResult{}, fmt.Errorf("lock bead: %w", err)
	}
	if err := verifyAttestationNotDrifted(orig, beadID, req.ContentSHAs); err != nil {
		return model.ScrubTaintBeadResult{}, err
	}
	contentSHA := hashBeadContent(orig.Title, orig.Description, orig.Notes, orig.Fields)

	tables, cascaded, err := cascadeContent(ctx, db, beadID, req.TaintID, operatorID, scrubContentReplacement{
		Title:       clean.Title,
		Description: clean.Description,
		Notes:       clean.Notes,
		Fields:      clean.Fields,
	})
	if err != nil {
		return model.ScrubTaintBeadResult{}, err
	}
	if !cascaded {
		return model.ScrubTaintBeadResult{BeadID: beadID, Skipped: true, SkipReason: model.ScrubSkipTaintGone}, nil
	}

	if err := recordScrubAudit(ctx, db, req, beadID, contentSHA); err != nil {
		return model.ScrubTaintBeadResult{}, err
	}

	return model.ScrubTaintBeadResult{BeadID: beadID, ContentSHA: contentSHA, TablesTouched: tables}, nil
}

// queryGetTaintEffects returns the current effects set for taintID. It is
// the defence-in-depth read scrubDetaintLiftBead consults before deleting a
// bead_taints row (ADR docs/adr/taints.md §15.4): ValidateScrubPolicy
// already refuses to save a scrub_policy.detaint_mode="lift" against a
// gate-carrying taint at write time, but a taint's effects can change (or an
// already-approved scrub-request can still be pending execution) between a
// policy save and this call, so ScrubTaint re-checks at the one place that
// actually removes the marker rather than trusting a policy that was valid
// when it was last written.
func queryGetTaintEffects(ctx context.Context, db executor, taintID int64) ([]string, error) {
	var effects []string
	err := db.QueryRowContext(ctx, `SELECT effects FROM taints WHERE id = $1`, taintID).Scan(pq.Array(&effects))
	if err != nil {
		return nil, fmt.Errorf("get taint effects: %w", err)
	}
	return effects, nil
}

// scrubDetaintLiftBead runs the "detaint" + lift branch for one bead (ADR
// docs/adr/taints.md §15.4, gd-XAXEBMrKOD): it removes the bead_taints row
// in place with NO content cascade — an integrity taint (effects excludes
// "gate") has no pre-image to protect, the content was never secret, only
// untrusted — and writes the usual taint_audit row (action scrub-execute)
// hashing the bead's CURRENT content, since lift never changes it.
//
// Refuses to run at all when the taint's current effects include "gate"
// (see queryGetTaintEffects above for why this re-checks a decision the
// policy validator already made once).
//
// Same stale-taint guard as purge/strip (gd-0fQDgy18lo): if bead_taints no
// longer carries (beadID, req.TaintID) — already scrubbed, or explicitly
// untainted, since this scrub request was built — lift is skipped rather
// than writing an audit row for a taint that's already gone.
//
// §15.4's rate rule ("a lift for a high-rate integrity taint must be fired
// by the approval that already exists for the underlying action — merge,
// plan approve — never by a per-bead scrub request") is a design constraint
// on CALLERS, not enforced here: ScrubTaint has no notion of "the underlying
// action's approval," only of the scrub-request/policy that reached it.
func scrubDetaintLiftBead(ctx context.Context, db executor, req *model.ScrubTaintRequest, beadID string) (model.ScrubTaintBeadResult, error) {
	effects, err := queryGetTaintEffects(ctx, db, req.TaintID)
	if err != nil {
		return model.ScrubTaintBeadResult{}, err
	}
	if model.HasEffect(effects, model.EffectGate) {
		return model.ScrubTaintBeadResult{}, fmt.Errorf(
			"scrub: detaint_mode lift is refused for taint %d: effects include %q (§15.4)",
			req.TaintID, model.EffectGate)
	}

	orig, err := lockBeadForScrub(ctx, db, beadID)
	if err != nil {
		return model.ScrubTaintBeadResult{}, fmt.Errorf("lock bead: %w", err)
	}
	contentSHA := hashBeadContent(orig.Title, orig.Description, orig.Notes, orig.Fields)

	stillTainted, err := taintStillOnBead(ctx, db, beadID, req.TaintID)
	if err != nil {
		return model.ScrubTaintBeadResult{}, err
	}
	if !stillTainted {
		return model.ScrubTaintBeadResult{BeadID: beadID, Skipped: true}, nil
	}

	if _, err := db.ExecContext(ctx,
		`DELETE FROM bead_taints WHERE bead_id = $1 AND taint_id = $2`, beadID, req.TaintID,
	); err != nil {
		return model.ScrubTaintBeadResult{}, fmt.Errorf("delete bead_taints: %w", err)
	}

	if err := recordScrubAudit(ctx, db, req, beadID, contentSHA); err != nil {
		return model.ScrubTaintBeadResult{}, err
	}

	return model.ScrubTaintBeadResult{BeadID: beadID, ContentSHA: contentSHA}, nil
}

// scrubDetaintForkBead runs the "detaint" + fork (default) branch for one
// bead: the original's content and bead_taints row are never touched — it
// is closed with fields.close_reason="scrubbed", and a successor bead is
// created from the attested clean_content, linked back with a "supersedes"
// deps edge. Skips (Skipped=true, SkipReason=ScrubSkipSoftDeleted) when the
// original has since been soft-deleted — there is nothing live to close and
// fork a successor from (unlike purge/strip, fork has no in-place content to
// scrub) — but still records a durable taint_audit row
// (Action=TaintAuditScrubSkip) so the request's audit trail shows this id
// was seen and deliberately not forked, not silently dropped (gd-T5P13cSoBw).
func scrubDetaintForkBead(ctx context.Context, db executor, req *model.ScrubTaintRequest, beadID string) (model.ScrubTaintBeadResult, error) {
	clean, ok := req.CleanContent[beadID]
	if !ok {
		return model.ScrubTaintBeadResult{}, fmt.Errorf("no attested clean_content for bead %s", beadID)
	}

	operatorID, err := resolveScrubOperatorID(ctx, db, req)
	if err != nil {
		return model.ScrubTaintBeadResult{}, err
	}

	orig, err := lockBeadForScrub(ctx, db, beadID)
	if err != nil {
		return model.ScrubTaintBeadResult{}, fmt.Errorf("lock bead: %w", err)
	}
	if orig.DeletedAt != nil {
		if err := recordScrubSkipAudit(ctx, db, req, beadID); err != nil {
			return model.ScrubTaintBeadResult{}, err
		}
		return model.ScrubTaintBeadResult{BeadID: beadID, Skipped: true, SkipReason: model.ScrubSkipSoftDeleted}, nil
	}
	if err := verifyAttestationNotDrifted(orig, beadID, req.ContentSHAs); err != nil {
		return model.ScrubTaintBeadResult{}, err
	}
	// Content is unchanged by fork — this hashes the CURRENT content, per
	// docs/adr/taints.md §6.5.
	contentSHA := hashBeadContent(orig.Title, orig.Description, orig.Notes, orig.Fields)

	newFields, err := mergeCloseReason(orig.Fields)
	if err != nil {
		return model.ScrubTaintBeadResult{}, fmt.Errorf("merge close_reason: %w", err)
	}

	now := time.Now().UTC()
	if _, err := db.ExecContext(ctx, `
		UPDATE beads SET status = 'closed', closed_at = $2, closed_by = $3, fields = $4::jsonb, updated_at = $2
		WHERE id = $1`,
		beadID, now, req.Actor, []byte(newFields),
	); err != nil {
		return model.ScrubTaintBeadResult{}, fmt.Errorf("close original: %w", err)
	}

	if err := insertMutation(ctx, db, &model.MutationRow{
		BeadID:       beadID,
		Actor:        model.SystemActorTaintScrub,
		Field:        "status",
		OldValue:     string(orig.Status),
		NewValue:     string(model.StatusClosed),
		MutationType: model.MutationTypeUpdate,
		OperatedByID: &operatorID,
	}); err != nil {
		return model.ScrubTaintBeadResult{}, fmt.Errorf("record status mutation: %w", err)
	}
	if err := insertMutation(ctx, db, &model.MutationRow{
		BeadID:       beadID,
		Actor:        model.SystemActorTaintScrub,
		Field:        "fields",
		OldValue:     string(orig.Fields),
		NewValue:     string(newFields),
		MutationType: model.MutationTypeUpdate,
		OperatedByID: &operatorID,
	}); err != nil {
		return model.ScrubTaintBeadResult{}, fmt.Errorf("record fields mutation: %w", err)
	}

	successorFields := clean.Fields
	if len(successorFields) == 0 {
		successorFields = json.RawMessage(`{}`)
	}
	successorID, err := idgen.Generate()
	if err != nil {
		return model.ScrubTaintBeadResult{}, fmt.Errorf("generate successor id: %w", err)
	}
	successor := &model.Bead{
		ID:                  successorID,
		Kind:                orig.Kind,
		Type:                orig.Type,
		Title:               clean.Title,
		Description:         clean.Description,
		Notes:               clean.Notes,
		Status:              model.StatusOpen,
		Priority:            orig.Priority,
		Assignee:            orig.Assignee,
		Owner:               orig.Owner,
		CreatedAt:           now,
		CreatedBy:           req.Actor,
		UpdatedAt:           now,
		Fields:              successorFields,
		Fleet:               orig.Fleet,
		OperatedByID:        &operatorID,
		CreatedByOperatorID: &operatorID,
	}
	if err := queryCreateBead(ctx, db, successor); err != nil {
		return model.ScrubTaintBeadResult{}, fmt.Errorf("create successor bead: %w", err)
	}

	dep := &model.Dependency{
		BeadID:       successor.ID,
		DependsOnID:  beadID,
		Type:         model.DepSupersedes,
		CreatedAt:    now,
		CreatedBy:    req.Actor,
		OperatedByID: &operatorID,
	}
	if err := queryAddDependency(ctx, db, dep); err != nil {
		return model.ScrubTaintBeadResult{}, fmt.Errorf("add supersedes dependency: %w", err)
	}

	if err := recordScrubAudit(ctx, db, req, beadID, contentSHA); err != nil {
		return model.ScrubTaintBeadResult{}, err
	}

	return model.ScrubTaintBeadResult{BeadID: beadID, ContentSHA: contentSHA, SuccessorBeadID: successor.ID}, nil
}

// mergeCloseReason sets fields.close_reason="scrubbed" on top of an
// existing fields blob without disturbing any other key, tolerating a nil/
// empty input (treated as `{}`).
func mergeCloseReason(fields json.RawMessage) (json.RawMessage, error) {
	m := map[string]json.RawMessage{}
	if len(fields) > 0 {
		if err := json.Unmarshal(fields, &m); err != nil {
			return nil, fmt.Errorf("decoding bead fields: %w", err)
		}
	}
	m["close_reason"] = json.RawMessage(`"scrubbed"`)
	return json.Marshal(m)
}

// recordScrubAudit writes the one taint_audit row docs/adr/taints.md §6.5
// requires per scrubbed bead.
func recordScrubAudit(ctx context.Context, db executor, req *model.ScrubTaintRequest, beadID, contentSHA string) error {
	if err := queryRecordTaintAudit(ctx, db, &model.TaintAudit{
		TaintID:    req.TaintID,
		Action:     model.TaintAuditScrubExecute,
		BeadID:     beadID,
		RequestID:  req.RequestID,
		Actor:      req.Actor,
		ContentSHA: contentSHA,
	}); err != nil {
		return fmt.Errorf("record taint_audit: %w", err)
	}
	return nil
}

// recordScrubSkipAudit writes a taint_audit row (Action=TaintAuditScrubSkip,
// no ContentSHA — no content was read or touched) for a bead
// scrubDetaintForkBead skipped as soft-deleted, so the request's audit trail
// durably records that this id was seen and deliberately skipped rather than
// leaving no trace at all (gd-T5P13cSoBw).
func recordScrubSkipAudit(ctx context.Context, db executor, req *model.ScrubTaintRequest, beadID string) error {
	if err := queryRecordTaintAudit(ctx, db, &model.TaintAudit{
		TaintID:   req.TaintID,
		Action:    model.TaintAuditScrubSkip,
		BeadID:    beadID,
		RequestID: req.RequestID,
		Actor:     req.Actor,
	}); err != nil {
		return fmt.Errorf("record taint_audit skip: %w", err)
	}
	return nil
}
