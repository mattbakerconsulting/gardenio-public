package postgres

// cascadeContentTables will be the single list of tables ScrubTaint's
// cascadeContent helper (ADR docs/adr/taints.md §6.5, gd-fVAy32nWDf) rewrites
// or deletes rows from, once that helper lands — it MUST range over this
// slice rather than inlining its own, so the list stays shared code, not
// duplicated, between that helper and TestScrubCascadeCoversContentTables
// (scrub_cascade_tables_test.go), which diffs it against the live
// migration-tracked schema — ADR §11 invariant 7: "the list is shared code
// between the two, not duplicated." Until cascadeContent exists, this list
// is a preparatory invariant only: this test cannot yet detect drift between
// the two, because there is nothing on the other side to drift from.
//
// purge and detaint/strip run the exact same content cascade (§6.5) and so
// share this one list. detaint/fork never touches the original's content —
// it closes the bead and forks a successor instead — so it is exempt by
// design (§11 invariant 7) and does not consult this list. detaint/lift
// (§15.4, gd-XAXEBMrKOD) is exempt for a different reason: it deletes the
// bead_taints row with no content cascade at all (integrity taints have no
// pre-image to protect), so it touches only bead_taints and taint_audit —
// both already classified below as non-content tables — and likewise never
// consults this list.
//
// Every table here corresponds to a row in ADR §1.3 ("Where tainted content
// actually lives"): beads (title/description/notes/fields — prose columns,
// not id/timestamp columns), comments (text), events (payload — the whole
// bead/comment body, not topic/ids/actor), mutation_history (old_value/
// new_value), bead_embeddings (the embedding vector, a derived copy of the
// text), deps (metadata — free text, not bead_id/depends_on_id/type),
// session_gates (metadata — JSON, not the gate's id/status columns).
//
// When a new migration adds a table that stores bead-derived prose content,
// that table must be added here in the same MR (ADR §11 invariant 7) — see
// scrub_cascade_tables_test.go for the classification a reviewer needs to
// extend this correctly.
var cascadeContentTables = []string{
	"beads",
	"comments",
	"events",
	"mutation_history",
	"bead_embeddings",
	"deps",
	"session_gates",
}
