package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"unicode/utf8"

	"gardenio/kbeads/internal/model"
)

// Formula-pour short_title (gd-4Nrk1IDadk, plan gd-YZATAIOhfW Task 10b). A
// pour creates task and decision step beads, both gated types, so under
// BEADS_SHORT_TITLE_GATE=enforce a step without a short_title made the first
// gated create fail and took the whole pour down with it.

// shortTitlePourFixture seeds a formula whose steps cover every way a step
// gets its short_title: declared with a {{var}}, declared but expanded past the
// limit, absent with a short title, absent with a long title, a decision gate,
// and a nested child.
func shortTitlePourFixture(t *testing.T, ms *mockStore) {
	t.Helper()
	fields, err := json.Marshal(map[string]any{
		"vars": []map[string]any{
			{"name": "component", "required": true},
		},
		"steps": []map[string]any{
			{"id": "design", "title": "Design the {{component}} change", "short_title": "design {{component}}"},
			{"id": "overflow", "title": "Overflow", "short_title": "{{component}} {{component}} {{component}} {{component}} {{component}} {{component}} {{component}} {{component}} {{component}}"},
			{"id": "build", "title": "Build {{component}}", "depends_on": []string{"design"}},
			{"id": "review", "title": "Review the {{component}} change against the rollout checklist and sign it off", "depends_on": []string{"build"}},
			{"id": "approve", "title": "Approve {{component}} rollout?", "depends_on": []string{"review"},
				"gate": map[string]any{"type": "decision", "prompt": "Ship it?", "options": []map[string]any{{"id": "y", "label": "Yes"}}}},
			{"id": "fanout", "title": "Fan out", "children": []map[string]any{
				{"id": "child", "title": "Child step for {{component}}"},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ms.beads["gd-formula-st"] = &model.Bead{
		ID: "gd-formula-st", Kind: model.KindData, Type: "formula",
		Title: "Ship {{component}}", Status: model.StatusOpen, Fields: fields,
	}
}

// pourAs drives handlePourFormula directly with the caller identity id, so
// the test can pour as an agent (fleet/machine) without a matching
// token verifier.
func pourAs(t *testing.T, s *BeadsServer, id AuthIdentity, formulaID string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/formulas/"+formulaID+"/pour", bytes.NewReader(b)).
		WithContext(shortTitleGateCtx(id))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", formulaID)
	rec := httptest.NewRecorder()
	s.handlePourFormula(rec, req)
	return rec
}

func TestPourFormula_ShortTitleGateEnforce_EveryStepNamed(t *testing.T) {
	t.Setenv("BEADS_SHORT_TITLE_GATE", "enforce")
	for _, name := range []string{"fleet", "machine"} {
		t.Run(name, func(t *testing.T) {
			s, ms, _ := newTestServer()
			shortTitlePourFixture(t, ms)

			rec := pourAs(t, s, shortTitleGateCallers[name].id, "gd-formula-st", map[string]any{
				"vars": map[string]string{"component": "auth"},
			})
			requireStatus(t, rec, http.StatusCreated)
			var resp pourFormulaResponse
			decodeJSON(t, rec, &resp)

			if len(resp.Steps) != 7 {
				t.Fatalf("steps = %v; want 7 (every step created)", resp.Steps)
			}
			want := map[string]string{
				"design":   "design auth",                              // declared, var substituted
				"overflow": "auth auth auth auth auth auth auth auth…", // declared, clipped after substitution
				"build":    "Build auth",                               // derived, fits as is
				"review":   "Review the auth change against the…",      // derived, cut at a word
				"approve":  "Approve auth rollout?",                    // decision gate step
				"fanout":   "Fan out",
				"child":    "Child step for auth", // nested child
			}
			for stepID, beadID := range resp.Steps {
				b := ms.beads[beadID]
				if b == nil {
					t.Fatalf("step %q (%s) not persisted", stepID, beadID)
				}
				if b.ShortTitle == "" {
					t.Errorf("step %q (type %s) has no short_title", stepID, b.Type)
				}
				if n := utf8.RuneCountInString(b.ShortTitle); n > model.ShortTitleMax {
					t.Errorf("step %q short_title %q is %d runes; want <= %d", stepID, b.ShortTitle, n, model.ShortTitleMax)
				}
				if w, ok := want[stepID]; ok && b.ShortTitle != w {
					t.Errorf("step %q short_title = %q; want %q", stepID, b.ShortTitle, w)
				}
			}
			if typ := ms.beads[resp.Steps["approve"]].Type; typ != model.TypeDecision {
				t.Errorf("approve step type = %q; want decision", typ)
			}

			// The molecule is not gated, but renders as a plan phase.
			if got := ms.beads[resp.Molecule.ID].ShortTitle; got != "Ship auth" {
				t.Errorf("molecule short_title = %q; want %q", got, "Ship auth")
			}
		})
	}
}
