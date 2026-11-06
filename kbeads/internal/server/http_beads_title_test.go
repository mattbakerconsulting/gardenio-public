package server

import (
	"slices"
	"testing"

	"gardenio/kbeads/internal/model"
)

// ?title= is an exact, repeatable title filter: the lands-pause kill switch
// finds its gate with it, so gates whose titles only mention the name are not
// returned (gd-i74esUHCTg).
func TestListBeads_TitleExact(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-gate"] = &model.Bead{ID: "gd-gate", Title: "lands-pause", Type: "gate", Status: model.StatusOpen}
	ms.beads["gd-drill"] = &model.Bead{ID: "gd-drill", Title: "lands-pause drill", Type: "gate", Status: model.StatusOpen}
	ms.beads["gd-other"] = &model.Bead{ID: "gd-other", Title: "a, b", Type: "gate", Status: model.StatusOpen}

	for _, tc := range []struct {
		query string
		want  []string
	}{
		{"/v1/beads?title=lands-pause", []string{"gd-gate"}},
		{"/v1/beads?title=lands-pause&title=a%2C+b", []string{"gd-gate", "gd-other"}},
		{"/v1/beads?title=lands", nil},
	} {
		rec := doJSON(t, h, "GET", tc.query, nil)
		requireStatus(t, rec, 200)
		var result struct {
			Beads []model.Bead `json:"beads"`
		}
		decodeJSON(t, rec, &result)
		var ids []string
		for _, b := range result.Beads {
			ids = append(ids, b.ID)
		}
		slices.Sort(ids)
		if !slices.Equal(ids, tc.want) {
			t.Errorf("%s returned %v, want %v", tc.query, ids, tc.want)
		}
	}
}
