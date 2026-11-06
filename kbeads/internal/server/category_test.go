package server

import (
	"testing"

	"gardenio/kbeads/internal/model"
)

// TestIsDeliverableType pins the deliverable set the category trigger
// (migration 043) classifies `artifact` — the durable fix for the ready-queue
// flood (ka-0000007274) — and that claimable work stays out of it.
func TestIsDeliverableType(t *testing.T) {
	for _, ty := range []model.BeadType{"bundle", "dashboard", "rich-report", "mr", "merge-request"} {
		if !isDeliverableType(ty) {
			t.Errorf("isDeliverableType(%q) = false, want true", ty)
		}
	}
	for _, ty := range []model.BeadType{model.TypeTask, model.TypeBug, model.TypeEpic, model.TypeDecision, "config"} {
		if isDeliverableType(ty) {
			t.Errorf("isDeliverableType(%q) = true, want false", ty)
		}
	}
}
