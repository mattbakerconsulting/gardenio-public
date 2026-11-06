package server

import (
	"context"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

// short_title (plan 2026-10-14-short-titles, gd-V5nGseRS5f): the <=40-rune
// human display name. These tests pin the HTTP shape: create, get, batch get,
// the 40-rune gate, the groomer's only_if_short_title_empty guard and the
// short_title_missing list filter.

// shortTitle40 is exactly 40 runes, mostly multibyte, so a byte-length check
// would reject it.
var shortTitle40 = strings.Repeat("é", 20) + strings.Repeat("🌱", 20)

func TestShortTitle_CreateRoundTripsThroughGetAndBatch(t *testing.T) {
	_, _, h := newTestServer()
	rec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title":       "Forward duration_ms through the gb hook relay so the trace panel sees it",
		"short_title": shortTitle40,
		"type":        "task",
	})
	requireStatus(t, rec, 201)
	var created model.Bead
	decodeJSON(t, rec, &created)
	if created.ShortTitle != shortTitle40 {
		t.Fatalf("create short_title = %q, want %q", created.ShortTitle, shortTitle40)
	}

	rec = doJSON(t, h, "GET", "/v1/beads/"+created.ID, nil)
	requireStatus(t, rec, 200)
	var got model.Bead
	decodeJSON(t, rec, &got)
	if got.ShortTitle != shortTitle40 {
		t.Fatalf("GET short_title = %q, want %q", got.ShortTitle, shortTitle40)
	}

	rec = doJSON(t, h, "GET", "/v1/batch/beads?ids="+created.ID, nil)
	requireStatus(t, rec, 200)
	var batch struct {
		Results []struct {
			ID   string      `json:"id"`
			Bead *model.Bead `json:"bead"`
		} `json:"results"`
	}
	decodeJSON(t, rec, &batch)
	if len(batch.Results) != 1 || batch.Results[0].Bead == nil {
		t.Fatalf("batch results = %+v, want one bead", batch.Results)
	}
	if st := batch.Results[0].Bead.ShortTitle; st != shortTitle40 {
		t.Fatalf("batch short_title = %q, want %q", st, shortTitle40)
	}
}

func TestShortTitle_CreateNormalizesWhitespace(t *testing.T) {
	_, _, h := newTestServer()
	rec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title":       "Fix the bridge",
		"short_title": "  fix   the\tbridge  ",
		"type":        "task",
	})
	requireStatus(t, rec, 201)
	var created model.Bead
	decodeJSON(t, rec, &created)
	if created.ShortTitle != "fix the bridge" {
		t.Fatalf("short_title = %q, want %q", created.ShortTitle, "fix the bridge")
	}
}

func TestShortTitle_CreateTooLongRejected(t *testing.T) {
	_, _, h := newTestServer()
	rec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title":       "Too long",
		"short_title": shortTitle40 + "x",
		"type":        "task",
	})
	requireStatus(t, rec, 400)
	const want = "invalid bead: validation failed: short_title: must be 40 characters or fewer"
	if body := rec.Body.String(); !strings.Contains(body, want) {
		t.Fatalf("body = %s, want it to contain %q", body, want)
	}
}

func TestShortTitle_UpdateTooLongRejected(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-st1"] = &model.Bead{ID: "gd-st1", Kind: model.KindIssue, Type: model.TypeTask, Title: "Bead", Status: model.StatusOpen}

	rec := doJSON(t, h, "PATCH", "/v1/beads/gd-st1", map[string]any{"short_title": shortTitle40 + "x"})
	requireStatus(t, rec, 400)
	if body := rec.Body.String(); !strings.Contains(body, "short_title: must be 40 characters or fewer") {
		t.Fatalf("body = %s, want the short_title length error", body)
	}
	if st := ms.beads["gd-st1"].ShortTitle; st != "" {
		t.Fatalf("stored short_title = %q after a rejected update, want empty", st)
	}
}

func TestShortTitle_UpdateSetsAndClears(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-st2"] = &model.Bead{ID: "gd-st2", Kind: model.KindIssue, Type: model.TypeTask, Title: "Bead", Status: model.StatusOpen}

	rec := doJSON(t, h, "PATCH", "/v1/beads/gd-st2", map[string]any{"short_title": "fix the bridge"})
	requireStatus(t, rec, 200)
	if st := ms.beads["gd-st2"].ShortTitle; st != "fix the bridge" {
		t.Fatalf("short_title = %q after set, want %q", st, "fix the bridge")
	}

	// An explicit "" clears it; an absent key leaves it alone.
	rec = doJSON(t, h, "PATCH", "/v1/beads/gd-st2", map[string]any{"title": "Bead renamed"})
	requireStatus(t, rec, 200)
	if st := ms.beads["gd-st2"].ShortTitle; st != "fix the bridge" {
		t.Fatalf("short_title = %q after an unrelated patch, want it unchanged", st)
	}
	rec = doJSON(t, h, "PATCH", "/v1/beads/gd-st2", map[string]any{"short_title": ""})
	requireStatus(t, rec, 200)
	if st := ms.beads["gd-st2"].ShortTitle; st != "" {
		t.Fatalf("short_title = %q after clearing, want empty", st)
	}
}

func TestShortTitle_OnlyIfEmptyGuard(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-authored"] = &model.Bead{ID: "gd-authored", Kind: model.KindIssue, Type: model.TypeTask, Title: "Authored", ShortTitle: "authored name", Status: model.StatusOpen}
	ms.beads["gd-empty"] = &model.Bead{ID: "gd-empty", Kind: model.KindIssue, Type: model.TypeTask, Title: "Empty", Status: model.StatusOpen}

	groom := map[string]any{"short_title": "groomer name", "only_if_short_title_empty": true}

	// An authored value always wins over the groomer (plan D5).
	rec := doJSON(t, h, "PATCH", "/v1/beads/gd-authored", groom)
	requireStatus(t, rec, 200)
	var got model.Bead
	decodeJSON(t, rec, &got)
	if got.ShortTitle != "authored name" {
		t.Fatalf("response short_title = %q, want the authored value kept", got.ShortTitle)
	}
	if st := ms.beads["gd-authored"].ShortTitle; st != "authored name" {
		t.Fatalf("stored short_title = %q, want the authored value kept", st)
	}

	// An unset one is filled.
	rec = doJSON(t, h, "PATCH", "/v1/beads/gd-empty", groom)
	requireStatus(t, rec, 200)
	if st := ms.beads["gd-empty"].ShortTitle; st != "groomer name" {
		t.Fatalf("stored short_title = %q, want %q", st, "groomer name")
	}

	// Without the guard an explicit update still replaces an authored value.
	rec = doJSON(t, h, "PATCH", "/v1/beads/gd-authored", map[string]any{"short_title": "renamed"})
	requireStatus(t, rec, 200)
	if st := ms.beads["gd-authored"].ShortTitle; st != "renamed" {
		t.Fatalf("stored short_title = %q, want %q", st, "renamed")
	}
}

func TestListBeads_ShortTitleMissing(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-named"] = &model.Bead{ID: "gd-named", Title: "Named", ShortTitle: "named bead", Status: model.StatusOpen}
	ms.beads["gd-unnamed"] = &model.Bead{ID: "gd-unnamed", Title: "Unnamed", Status: model.StatusOpen}

	rec := doJSON(t, h, "GET", "/v1/beads?short_title_missing=true", nil)
	requireStatus(t, rec, 200)
	var result struct {
		Beads []model.Bead `json:"beads"`
		Total int          `json:"total"`
	}
	decodeJSON(t, rec, &result)
	if len(result.Beads) != 1 || result.Beads[0].ID != "gd-unnamed" {
		ids := make([]string, 0, len(result.Beads))
		for _, b := range result.Beads {
			ids = append(ids, b.ID)
		}
		t.Fatalf("short_title_missing=true returned %v, want [gd-unnamed]", ids)
	}

	rec = doJSON(t, h, "GET", "/v1/beads", nil)
	requireStatus(t, rec, 200)
	decodeJSON(t, rec, &result)
	if len(result.Beads) != 2 {
		t.Fatalf("unfiltered list returned %d beads, want 2", len(result.Beads))
	}
}

// The groomer fills closed beads too: short_title is not a transient field,
// so a closed bead keeps the write.
func TestShortTitle_FillsClosedBead(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-done"] = &model.Bead{ID: "gd-done", Kind: model.KindIssue, Type: model.TypeTask, Title: "Done", Status: model.StatusClosed}

	rec := doJSON(t, h, "PATCH", "/v1/beads/gd-done", map[string]any{"short_title": "finished work", "only_if_short_title_empty": true})
	requireStatus(t, rec, 200)
	if st := ms.beads["gd-done"].ShortTitle; st != "finished work" {
		t.Fatalf("stored short_title = %q, want %q", st, "finished work")
	}
	if ms.beads["gd-done"].Status != model.StatusClosed {
		t.Fatalf("status = %q, want the bead to stay closed", ms.beads["gd-done"].Status)
	}
}

func TestUndoMutation_HappyPath_ShortTitle(t *testing.T) {
	bead := &model.Bead{ID: "gd-undo-st", Title: "x", ShortTitle: "new name", Type: "task", Kind: model.KindIssue, Status: model.StatusOpen}
	s, _, mutID := newUndoFixture(t, "gd-undo-st", bead, "short_title", "old name", "new name")

	updated, _, err := s.undoMutation(context.Background(), mutID, DefaultUndoRetentionDays)
	if err != nil {
		t.Fatalf("undoMutation: %v", err)
	}
	if updated.ShortTitle != "old name" {
		t.Errorf("short_title=%q, want %q", updated.ShortTitle, "old name")
	}
}
