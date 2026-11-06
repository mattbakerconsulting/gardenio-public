package bridge

import (
	"context"
	"encoding/json"

	"gardenio/controller/internal/scheduler"
)

// Edge-event SSE topics that affect a bead but do not carry a full bead
// payload. Subscribing to these lets trigger:bead and trigger:multi wake
// gardeners on graph mutations beyond bead create/update/close.
//
// Audit and rationale: ka-0000001427 (tree-hugger v2 platform: harden
// bead-graph edge-change triggers). Before this file, the canonical
// "wake when ANYTHING in the connected subgraph of my demo bead mutates"
// fired only on the bead itself being created/updated/closed —
// adding a parent edge, comment, label, or follower did nothing.
//
// These payloads don't include the affected bead's fields/labels, so the
// trigger sources hydrate each affected bead via the daemon (GetBead) before
// firing — see RegisterSSE in trigger_source_multi.go / trigger_source_bead.go.
// Hydration fails open so a missing daemon or GetBead error skips the event
// rather than firing an unmatchable id-only one.
var edgeEventTopics = []string{
	"beads.dependency.added",
	"beads.dependency.removed",
	"beads.label.added",
	"beads.label.removed",
	"beads.comment.added",
	"beads.comment.deleted",
	"beads.follower.added",
	"beads.follower.removed",
	"beads.bead.deleted",
	"beads.bead.transferred",
}

// EdgeEventTopics returns a copy of the edge-event SSE topic list so callers
// in other packages (e.g. the trigger-engine main) can subscribe the SSE
// stream to these topics. A copy is returned so callers cannot mutate the
// package-level slice.
func EdgeEventTopics() []string {
	return append([]string(nil), edgeEventTopics...)
}

// ParentInvalidator drops cached parent-child parents; *beadsapi.Client
// implements it once EnableParentCache is on.
type ParentInvalidator interface {
	InvalidateParents(ids ...string)
}

// InvalidateParentsOnEdgeEvents subscribes inv to the edge events that can
// change a bead's parent (a dependency added or removed, a bead deleted) and
// drops the cached parent of every bead the event names. The drop happens
// during SSE dispatch, before the engine tick that matches the buffered event
// (gd-ZaztjcPrDc).
func InvalidateParentsOnEdgeEvents(stream *SSEStream, inv ParentInvalidator) {
	for _, topic := range []string{"beads.dependency.added", "beads.dependency.removed", "beads.bead.deleted"} {
		t := topic
		stream.On(t, func(_ context.Context, data []byte) {
			if ids := ParseEdgeEventBeadIDs(t, data); len(ids) > 0 {
				inv.InvalidateParents(ids...)
			}
		})
	}
}

// ParseEdgeEventBeadIDs extracts the set of bead IDs whose graph state was
// affected by an edge-shaped event. A dependency add/remove touches both
// endpoints (head and tail) — the function returns both. Other events
// affect a single bead.
//
// Returns nil if the payload cannot be parsed.
func ParseEdgeEventBeadIDs(topic string, data []byte) []string {
	switch topic {
	case "beads.dependency.added", "beads.dependency.removed":
		return parseDependencyBeadIDs(data)
	case "beads.label.added", "beads.label.removed":
		return parseSimpleBeadID(data, "bead_id")
	case "beads.comment.added":
		return parseCommentAddedBeadID(data)
	case "beads.comment.deleted":
		return parseSimpleBeadID(data, "bead_id")
	case "beads.follower.added", "beads.follower.removed":
		return parseSimpleBeadID(data, "bead_id")
	case "beads.bead.deleted":
		return parseSimpleBeadID(data, "bead_id")
	case "beads.bead.transferred":
		return parseTransferredBeadID(data)
	}
	return nil
}

func parseDependencyBeadIDs(data []byte) []string {
	var p struct {
		Dependency *struct {
			BeadID      string `json:"bead_id"`
			DependsOnID string `json:"depends_on_id"`
		} `json:"dependency"`
		// Removed events use a flat shape.
		BeadID      string `json:"bead_id"`
		DependsOnID string `json:"depends_on_id"`
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return nil
	}
	var ids []string
	if p.Dependency != nil {
		if p.Dependency.BeadID != "" {
			ids = append(ids, p.Dependency.BeadID)
		}
		if p.Dependency.DependsOnID != "" && p.Dependency.DependsOnID != p.Dependency.BeadID {
			ids = append(ids, p.Dependency.DependsOnID)
		}
	}
	if p.BeadID != "" {
		ids = appendUniqueID(ids, p.BeadID)
	}
	if p.DependsOnID != "" {
		ids = appendUniqueID(ids, p.DependsOnID)
	}
	return ids
}

func parseCommentAddedBeadID(data []byte) []string {
	var p struct {
		Comment *struct {
			BeadID string `json:"bead_id"`
		} `json:"comment"`
		BeadID string `json:"bead_id"`
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return nil
	}
	if p.Comment != nil && p.Comment.BeadID != "" {
		return []string{p.Comment.BeadID}
	}
	if p.BeadID != "" {
		return []string{p.BeadID}
	}
	return nil
}

// CommentAddedFields returns the payload keys a beads.comment.added event adds
// to the hydrated bead: the comment's author (the daemon's authenticated
// subject, never a body claim) and its text, under scheduler.PayloadCommentAuthor
// and scheduler.PayloadCommentText. Hydration replaces the frame with the
// commented bead, so without these a trigger cannot tell whose comment woke it.
// Returns nil when the frame does not parse or carries neither.
func CommentAddedFields(data []byte) map[string]any {
	var p struct {
		Comment *struct {
			Author string `json:"author"`
			Text   string `json:"text"`
		} `json:"comment"`
	}
	if err := json.Unmarshal(data, &p); err != nil || p.Comment == nil {
		return nil
	}
	if p.Comment.Author == "" && p.Comment.Text == "" {
		return nil
	}
	return map[string]any{
		scheduler.PayloadCommentAuthor: p.Comment.Author,
		scheduler.PayloadCommentText:   p.Comment.Text,
	}
}

func parseSimpleBeadID(data []byte, field string) []string {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil
	}
	if v, ok := raw[field]; ok {
		var s string
		if err := json.Unmarshal(v, &s); err == nil && s != "" {
			return []string{s}
		}
	}
	return nil
}

func parseTransferredBeadID(data []byte) []string {
	// Transferred events publish a full bead under "bead" — reuse the
	// existing parser so we get labels/fields for downstream conditions.
	if bead := ParseBeadEvent(data); bead != nil && bead.ID != "" {
		return []string{bead.ID}
	}
	return parseSimpleBeadID(data, "bead_id")
}

func appendUniqueID(s []string, v string) []string {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}
