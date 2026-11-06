package server

import (
	"context"
	"encoding/json"
	"testing"

	"gardenio/kbeads/internal/model"
)

// seedEscalation stores an open escalation in project gardenio. operatedBy is
// the requester's human (four-eyes compares it byte-for-byte first).
func seedEscalation(t *testing.T, ms *mockStore, id, operatedBy string, fields map[string]any) {
	t.Helper()
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	b := &model.Bead{
		ID: id, Title: "esc " + id, Type: model.TypeEscalation, Kind: model.KindData,
		Status: model.StatusOpen, Labels: []string{"project:gardenio"}, Fields: raw, OperatedBy: operatedBy,
	}
	if err := ms.CreateBead(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	// The project check reads labels from the mock's label index, which
	// CreateBead does not fill.
	ms.labels[id] = b.Labels
}

func escFields(t *testing.T, ms *mockStore, id string) map[string]any {
	t.Helper()
	b, err := ms.GetBead(context.Background(), id)
	if err != nil || b == nil {
		t.Fatalf("get %s: %v", id, err)
	}
	m := map[string]any{}
	_ = json.Unmarshal(b.Fields, &m)
	return m
}
