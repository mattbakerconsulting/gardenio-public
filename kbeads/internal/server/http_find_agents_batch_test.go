package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// unlabelledAgentsStore returns agent beads with no labels hydrated, as the
// real queryFindAgentsByNames does, and counts per-bead GetLabels calls so a
// test can tell a batched hydration from the old one-SELECT-per-agent loop.
type unlabelledAgentsStore struct {
	*mockStore
	getLabelsCalls       int
	getLabelsForBeadsIDs [][]string
}

func (s *unlabelledAgentsStore) FindAgentsByNames(ctx context.Context, names []string, statuses []model.Status) ([]*model.Bead, error) {
	beads, err := s.mockStore.FindAgentsByNames(ctx, names, statuses)
	for _, b := range beads {
		b.Labels = nil
	}
	return beads, err
}

func (s *unlabelledAgentsStore) GetLabels(ctx context.Context, beadID string) ([]string, error) {
	s.getLabelsCalls++
	return s.mockStore.GetLabels(ctx, beadID)
}

func (s *unlabelledAgentsStore) GetLabelsForBeads(ctx context.Context, beadIDs []string) (map[string][]string, error) {
	s.getLabelsForBeadsIDs = append(s.getLabelsForBeadsIDs, slices.Clone(beadIDs))
	return s.mockStore.GetLabelsForBeads(ctx, beadIDs)
}

// TestHandleFindAgentsByNames_BatchesLabels pins gd-cP9tODid8j: the batch
// agent lookup hydrates labels for the project guards with ONE
// GetLabelsForBeads call, not one GetLabels per agent, and the response is
// unchanged — same project filtering, same labels on each bead, and an
// unlabelled agent still filtered out of a project-scoped token's view.
func TestHandleFindAgentsByNames_BatchesLabels(t *testing.T) {
	ms := newMockStore()
	st := &unlabelledAgentsStore{mockStore: ms}
	s := NewBeadsServer(st, &events.NoopPublisher{})
	s.userProvisioner = nil

	for _, a := range []struct {
		id, name string
		labels   []string
	}{
		{"gd-a1", "alpha", []string{"project:gardenio", "role:crew"}},
		{"gd-a2", "bravo", []string{"project:other"}},
		{"gd-a3", "charlie", nil},
		{"gd-a4", "delta", []string{"project:gardenio"}},
	} {
		ms.beads[a.id] = &model.Bead{
			ID: a.id, Type: "agent", Status: model.StatusOpen,
			Fields: json.RawMessage(`{"agent":"` + a.name + `"}`),
		}
		if a.labels != nil {
			ms.labels[a.id] = a.labels
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/agents-by-name:batch?names=alpha,bravo,charlie,delta", nil)
	req = req.WithContext(WithTokenProject(req.Context(), "gardenio"))
	rec := httptest.NewRecorder()
	s.handleFindAgentsByNames(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Agents []*model.Bead `json:"agents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (body %s)", err, rec.Body.String())
	}
	gotLabels := map[string][]string{}
	for _, b := range got.Agents {
		gotLabels[b.ID] = b.Labels
	}
	want := map[string][]string{
		"gd-a1": {"project:gardenio", "role:crew"},
		"gd-a4": {"project:gardenio"},
	}
	if len(gotLabels) != len(want) {
		t.Fatalf("returned %v, want only the project:gardenio agents %v", gotLabels, want)
	}
	for id, w := range want {
		if !slices.Equal(gotLabels[id], w) {
			t.Errorf("%s labels = %v, want %v", id, gotLabels[id], w)
		}
	}

	if st.getLabelsCalls != 0 {
		t.Errorf("GetLabels called %d times; want 0 (labels must be batched)", st.getLabelsCalls)
	}
	if len(st.getLabelsForBeadsIDs) != 1 {
		t.Fatalf("GetLabelsForBeads called %d times; want 1", len(st.getLabelsForBeadsIDs))
	}
	if ids := slices.Sorted(slices.Values(st.getLabelsForBeadsIDs[0])); !slices.Equal(ids, []string{"gd-a1", "gd-a2", "gd-a3", "gd-a4"}) {
		t.Errorf("GetLabelsForBeads ids = %v, want every resolved agent", ids)
	}
}
