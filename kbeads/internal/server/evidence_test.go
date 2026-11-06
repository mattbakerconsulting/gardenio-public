package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

// type=evidence is a builtin data-kind type with no write_policy, so any
// authenticated caller files one (spec 2026-10-22-lands-harvest-icons-design.md
// §2.1). No land tier writes one: a land has no reason to grade a step.
func TestEvidence_TypeRegistered(t *testing.T) {
	if !slices.Contains(BuiltinBeadTypeNames(), string(model.TypeEvidence)) {
		t.Fatal("evidence is not a builtin type")
	}
	s, _, _ := newTestServer()
	tc, err := s.resolveTypeConfig(context.Background(), model.TypeEvidence)
	if err != nil || tc == nil {
		t.Fatalf("resolveTypeConfig: %v %v", tc, err)
	}
	if tc.Kind != model.KindData || tc.WritePolicy != nil {
		t.Errorf("evidence must be data-kind with no write_policy: %+v", tc)
	}
	declared := map[string]model.FieldDef{}
	for _, f := range tc.Fields {
		declared[f.Name] = f
	}
	for name, typ := range map[string]model.FieldType{
		"kind": model.FieldTypeString, "step": model.FieldTypeString, "subject": model.FieldTypeString,
		"score": model.FieldTypeInteger, "of": model.FieldTypeInteger, "percent": model.FieldTypeInteger,
		"urls": model.FieldTypeString, "pr": model.FieldTypeString,
	} {
		if f, ok := declared[name]; !ok || f.Type != typ {
			t.Errorf("evidence.%s = %+v, want declared %s", name, f, typ)
		}
	}
	for _, name := range []string{"kind", "step"} {
		if !declared[name].Required {
			t.Errorf("evidence.%s must be required", name)
		}
	}
	for _, act := range []string{"create", "update", "close", "comment"} {
		if anyTierAllows(model.TypeEvidence, act) {
			t.Errorf("actionTable allows %s on evidence", act)
		}
	}
}

// A crew pod's token (a fleet token scoped to its project, no type scope) files
// a grade the way the commons-ux-acceptance skill does: create it with the
// skill's fields, hang it off the step, close it.
func TestEvidence_CrewTokenCreatesAndCloses(t *testing.T) {
	const (
		adminToken = "admin-evidence-secret"
		crewToken  = "crew-evidence-tok"
	)
	fc := NewFleetConfig("gardenio", nil)
	fc.RegisterTokenWithSubject(crewToken, "gardenio", "gardenio", nil, nil, time.Time{}, "gardenio-crew-grader")
	ts, _, ms := newE2EServer(t, adminToken, fc)

	step := createTaskBeadOwnedBy(t, ts, crewToken, "4b.6.1 harvest writers", "")
	resp := e2eRequest(t, ts, http.MethodPost, "/v1/beads", crewToken, createBeadInput{
		Title:  "UX grade 5/5: cursorphase",
		Type:   string(model.TypeEvidence),
		Labels: []string{"project:gardenio"},
		Fields: json.RawMessage(`{"kind":"ux","step":"` + step + `","subject":"cursorphase","score":5,"of":5,"percent":100,` +
			`"urls":"https://gardenio.clonk.tv/image/a.png\nhttps://gardenio.clonk.tv/image/b.png","pr":"https://github.com/o/r/pull/825"}`),
	})
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create evidence: %d %s", resp.StatusCode, body)
	}
	var ev model.Bead
	if err := json.Unmarshal(body, &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Kind != model.KindData {
		t.Errorf("evidence kind = %q, want data", ev.Kind)
	}

	resp = e2eRequest(t, ts, http.MethodPost, "/v1/beads/"+ev.ID+"/dependencies", crewToken,
		addDependencyRequest{DependsOnID: step, Type: string(model.DepParentChild)})
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		t.Fatalf("parent evidence to step: %d %s", resp.StatusCode, body)
	}

	resp = e2eRequest(t, ts, http.MethodPost, "/v1/beads/"+ev.ID+"/close", crewToken, map[string]any{})
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("close evidence: %d %s", resp.StatusCode, body)
	}
	if got := ms.beads[ev.ID].Status; got != model.StatusClosed {
		t.Errorf("evidence status = %q, want closed", got)
	}
	// Closing the grade leaves the step alone.
	if got := ms.beads[step].Status; got == model.StatusClosed {
		t.Errorf("closing evidence closed its step")
	}
}

// A grade missing the join keys is refused, so the farm never reads an
// evidence row it cannot place.
func TestEvidence_RequiresKindAndStep(t *testing.T) {
	_, _, h := newTestServer()
	for _, fields := range []string{`{"kind":"ux"}`, `{"step":"gd-x"}`} {
		rec := doJSON(t, h, http.MethodPost, "/v1/beads", createBeadInput{
			Title: "UX grade", Type: string(model.TypeEvidence), Labels: []string{"project:gardenio"},
			Fields: json.RawMessage(fields),
		})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("fields %s: status %d, want 400: %s", fields, rec.Code, rec.Body.String())
		}
	}
}

// The step-level harvest fields (github-bridge's ci_state, tests_touched and
// goldens_touched, the merge desk's review_rounds) are declared on every
// work type a PR can come from: those schemas reject unknown fields, so a
// missing one would 400 the bridge's write at merge.
func TestBuiltinHarvestFieldsDeclared(t *testing.T) {
	for _, typ := range []string{"task", "bug", "feature", "chore", "epic"} {
		tc := parseBuiltinSchema(t, typ)
		var names []string
		for _, f := range tc.Fields {
			names = append(names, f.Name)
		}
		for _, want := range []string{"ci_state", "tests_touched", "goldens_touched", "review_rounds"} {
			if !slices.Contains(names, want) {
				t.Errorf("type:%s does not declare %s (has %s)", typ, want, strings.Join(names, ","))
			}
		}
	}
}
