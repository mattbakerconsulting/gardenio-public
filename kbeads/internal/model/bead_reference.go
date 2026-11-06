package model

// BeadReferenceSummary is the minimal projection returned by the
// reverse-lookup APIs (`GET /v1/beads/{id}/mentioned-by`,
// `GET /v1/beads/{id}/mentions`). One row corresponds to a single
// (source bead, source field) pair from the bead_references table.
//
// For the forward direction (`mentions`) the target bead may be missing
// from the beads table (dangling reference — typo, cross-fleet ID, or
// not-yet-created bead). In that case the bead-side fields (Title,
// Type, Status) are empty strings and only the ID and SourceField are
// populated. See kbeads/docs/design-bead-references.md § 5a.
type BeadReferenceSummary struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Type        string `json:"type"`
	Status      string `json:"status"`
	SourceField string `json:"source_field"`
	// RelationshipType carries the typed-mention verb that the author
	// declared in prose (e.g. `parent(ka-X)`). nil means the row is an
	// untyped mention — the historic default — and JSON-encodes as
	// `null`. See kbeads/docs/research-typed-mention-syntax.md § 4.2.
	RelationshipType *string `json:"relationship_type"`
}
