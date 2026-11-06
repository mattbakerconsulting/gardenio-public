package server

import (
	"encoding/json"
	"strings"
	"testing"
)

// Step 9.6 (gd-soDpoj3JQb): `gb lands digest --post` records where the digest
// stopped on the land's steward-notes bead (a type:doc). type:doc is a closed
// schema, so an undeclared key 400s on update; the digest would then be said
// but never recorded, and every later digest would window a day back.
func TestUpdateBead_StewardNotesRecordsDigestMark(t *testing.T) {
	s, ms, _ := newTestServer()
	notes, err := s.createBead(ctxAsTestSubject(), createBeadInput{
		Title: "steward-notes:tomato-patch", Type: "doc",
		Fields: json.RawMessage(`{"format":"md","content":"Steward notes.\n","machines_high_water":"2026-10-23T05:00:00Z"}`),
	})
	if err != nil {
		t.Fatalf("seed notes: %v", err)
	}

	mark := json.RawMessage(`{"digest_at":"2026-10-23T07:00:00Z","digest_used_credits":"1.25"}`)
	if _, err := s.updateBead(ctxAsTestSubject(), notes.ID, updateBeadInput{Fields: mark}); err != nil {
		t.Fatalf("recording the digest mark on the notes bead must validate, got %v", err)
	}
	got := parseFieldsToStringMap(ms.beads[notes.ID].Fields)
	if got["digest_at"] != "2026-10-23T07:00:00Z" || got["digest_used_credits"] != "1.25" {
		t.Errorf("fields after = %v, want the digest mark stored", got)
	}

	// The doc schema stays closed: a key nobody declared still 400s.
	_, err = s.updateBead(ctxAsTestSubject(), notes.ID, updateBeadInput{Fields: json.RawMessage(`{"digest_typo_at":"x"}`)})
	if err == nil || !strings.Contains(err.Error(), "digest_typo_at") {
		t.Errorf("an undeclared doc field must still be refused, got %v", err)
	}
}
