package server

import (
	"encoding/json"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// historySlimTypes are the bead types whose persisted history is slimmed and
// pruned (gd-1F8R0SQtiQ). Measured 2026-10-31, their fields updates were
// about 8.2 GB of the 8.7 GB of field='fields' mutation_history, and a
// trigger's beads.bead.updated payload ran 30-190 KB. Other types keep the
// full bead in their persisted events, because readers such as the viewer's
// coverage page and /feed seed read bead.fields from stored events. The feed
// seed already excludes these two types (feedSeedExcludeTypes).
var historySlimTypes = []model.BeadType{"trigger", "agent"}

func isHistorySlimType(t model.BeadType) bool {
	for _, st := range historySlimTypes {
		if t == st {
			return true
		}
	}
	return false
}

// persistedBeadUpdated is the events-table shape of a slimmed
// beads.bead.updated event. It keeps the BeadUpdated keys, so a reader that
// decodes bead, changes and actor still works. bead has no fields and no
// comments, and fields_omitted says so: a reader that needs either re-reads
// the bead. changes.fields, when present, is the forward merge patch of the
// update, and fields_old is the reverse patch (the changed keys' prior
// values, null for a key the update added).
type persistedBeadUpdated struct {
	Bead          *model.Bead    `json:"bead"`
	Changes       map[string]any `json:"changes"`
	Actor         string         `json:"actor,omitempty"`
	FieldsOld     map[string]any `json:"fields_old,omitempty"`
	FieldsOmitted bool           `json:"fields_omitted"`
}

// persistedEventPayload returns the payload recordAndPublish writes to the
// events table. It is the live payload for every event except a
// beads.bead.updated event on a historySlimTypes bead, which is slimmed to
// persistedBeadUpdated. The live publish, SSE broadcast and JetStream mutation
// stream always get the full payload.
func persistedEventPayload(topic string, event any, full []byte) []byte {
	if topic != events.TopicBeadUpdated {
		return full
	}
	ev, ok := event.(events.BeadUpdated)
	if !ok || ev.Bead == nil || !isHistorySlimType(ev.Bead.Type) {
		return full
	}
	bead := *ev.Bead
	bead.Fields = nil
	bead.Comments = nil
	out := persistedBeadUpdated{
		Bead:          &bead,
		Changes:       ev.Changes,
		Actor:         ev.Actor,
		FieldsOmitted: true,
	}
	if newFields, has := ev.Changes["fields"]; has && ev.FieldsBefore != nil {
		if nb, ok := fieldsBytes(newFields); ok {
			if forward, reverse, ok := fieldsDelta(ev.FieldsBefore, nb); ok {
				changes := make(map[string]any, len(ev.Changes))
				for k, v := range ev.Changes {
					changes[k] = v
				}
				changes["fields"] = forward
				out.Changes = changes
				out.FieldsOld = reverse
			}
		}
	}
	slim, err := json.Marshal(out)
	if err != nil {
		return full
	}
	return slim
}
