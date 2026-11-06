package main

import "gardenio/kbeads/internal/model"

// ka-0000002877 (discoverability): `kd show <id> --json` emits comment
// sub-objects whose body key is "text", so the conventional guess
// `.comments[] | "\(.author): \(.body)"` rendered "<author>: null" — the
// author half resolved and the body half silently vanished, which is the worst
// failure mode available: partially-correct output that looks like a data
// problem rather than a schema mismatch. An absent jq key is not an error, so
// nothing pointed at the real key. Observed live in session ka-0000008257
// (2 wasted guess->null->inspect-raw->re-project round-trips).
//
// Only "body" is genuinely missing — model.Comment already ships "author"
// (and "operated_by_display" is a distinct, resolver-populated field, NOT the
// author fallback the bug report assumed; a comment authored by an agent has
// author set and operated_by_display empty). So this wrapper adds exactly one
// key rather than the two the report predicted.

// commentView is one comment with the additive "body" alias of its canonical
// "text" key. The comment is EMBEDDED rather than copied field by field, so
// every existing key (id, bead_id, author, text, created_at, operated_by,
// operated_by_display…) promotes unchanged and automatically keeps pace with
// model.Comment. "text" stays canonical — this is a pure superset, so no
// existing consumer changes behavior.
type commentView struct {
	*model.Comment
	// Body mirrors Comment.Text. NOT omitempty: an empty-bodied comment must
	// still expose the key, because the whole defect being fixed is a caller
	// unable to tell "absent key" from "empty value" — omitempty would
	// reintroduce exactly that ambiguity for the empty case.
	Body string `json:"body"`
}

// commentViews wraps each comment with its "body" alias. Returns nil for an
// empty slice so a comment-less bead encodes byte-identically to a bare bead.
// nil entries are skipped rather than dereferenced: the JSON path must never
// panic on a malformed row when the text path merely skips it.
func commentViews(comments []*model.Comment) []*commentView {
	if len(comments) == 0 {
		return nil
	}
	views := make([]*commentView, 0, len(comments))
	for _, c := range comments {
		if c == nil {
			continue
		}
		views = append(views, &commentView{Comment: c, Body: c.Text})
	}
	return views
}
