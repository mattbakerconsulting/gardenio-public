package server

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"gardenio/kbeads/internal/model"
)

// TestValidateReproFields covers the repro-harness cross-field constraint:
// gate_passed=true requires repro_evidence to be a valid http/https URL. When
// gate_passed is false, null, or absent the rule is inert so legacy beads and
// in-progress repros are never wedged (ka-0000002579).
func TestValidateReproFields(t *testing.T) {
	cases := []struct {
		name    string
		fields  string
		wantErr bool
	}{
		{"empty fields", ``, false},
		{"no repro fields", `{"repro_ticket":"FSM-468"}`, false},
		{"gate absent, evidence present", `{"repro_evidence":"https://x.io/v"}`, false},
		{"gate false ignores evidence", `{"gate_passed":false}`, false},
		{"gate null ignores evidence", `{"gate_passed":null}`, false},
		{"gate true with valid https URL", `{"gate_passed":true,"repro_evidence":"https://s3.example.com/repro/video.mp4"}`, false},
		{"gate true with valid http URL", `{"gate_passed":true,"repro_evidence":"http://viewer.local/beads/image/abc"}`, false},
		{"gate true with ticket too", `{"gate_passed":true,"repro_evidence":"https://x.io/v","repro_ticket":"FSM-468"}`, false},
		{"gate true, evidence missing", `{"gate_passed":true}`, true},
		{"gate true, evidence empty", `{"gate_passed":true,"repro_evidence":""}`, true},
		{"gate true, evidence not a URL", `{"gate_passed":true,"repro_evidence":"see the video"}`, true},
		{"gate true, evidence bare host", `{"gate_passed":true,"repro_evidence":"example.com/v"}`, true},
		{"gate true, evidence ticket id", `{"gate_passed":true,"repro_evidence":"gd-abc12345"}`, true},
		{"gate true, evidence non-http scheme", `{"gate_passed":true,"repro_evidence":"ftp://host/v"}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var raw json.RawMessage
			if tc.fields != "" {
				raw = json.RawMessage(tc.fields)
			}
			err := validateReproFields(raw)
			if tc.wantErr && err == nil {
				t.Errorf("validateReproFields(%s) = nil, want error", tc.fields)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("validateReproFields(%s) = %v, want nil", tc.fields, err)
			}
		})
	}
}

// TestIsValidEvidenceURL pins the URL predicate: http/https + non-empty host.
func TestIsValidEvidenceURL(t *testing.T) {
	valid := []string{
		"https://s3.amazonaws.com/bucket/key.png",
		"http://localhost:8095/beads/image/x",
		"https://gitlab.com/foo/bar/-/merge_requests/1",
	}
	for _, s := range valid {
		if !isValidEvidenceURL(s) {
			t.Errorf("isValidEvidenceURL(%q) = false, want true", s)
		}
	}
	invalid := []string{
		"",
		"example.com",
		"FSM-468",
		"gd-abc12345",
		"ftp://host/x",
		"/relative/path",
		"just some text",
	}
	for _, s := range invalid {
		if isValidEvidenceURL(s) {
			t.Errorf("isValidEvidenceURL(%q) = true, want false", s)
		}
	}
}

// TestCreateBead_ReproFields_Task drives the acceptance criteria through the
// real create choke point on a type=task bead: the three fields are accepted
// (no unknown-field 400), gate_passed=true with a valid evidence URL is
// accepted, and gate_passed=true with a non-URL evidence is rejected as an
// inputError (→400). Covers acceptance #2 and #3 (ka-0000002579).
func TestCreateBead_ReproFields_Task(t *testing.T) {
	// gate_passed=true + valid URL + ticket: accepted, persisted verbatim.
	t.Run("accepts gate_passed + evidence URL + ticket", func(t *testing.T) {
		s, ms, _ := newTestServer()
		bead, err := s.createBead(ctxAsTestSubject(), createBeadInput{
			Title: "emit step",
			Type:  "task",
			Fields: json.RawMessage(`{
				"gate_passed": true,
				"repro_evidence": "https://s3.example.com/repro/FSM-468/video.mp4",
				"repro_ticket": "FSM-468"
			}`),
		})
		if err != nil {
			t.Fatalf("createBead with repro fields should be accepted, got: %v", err)
		}
		var f map[string]any
		if uerr := json.Unmarshal(ms.beads[bead.ID].Fields, &f); uerr != nil {
			t.Fatalf("unmarshal stored fields: %v", uerr)
		}
		if f["gate_passed"] != true {
			t.Errorf("gate_passed not persisted: %v", f["gate_passed"])
		}
		if f["repro_ticket"] != "FSM-468" {
			t.Errorf("repro_ticket not persisted: %v", f["repro_ticket"])
		}
	})

	// gate_passed=true with non-URL evidence: rejected as inputError (→400).
	t.Run("rejects gate_passed=true with non-URL evidence", func(t *testing.T) {
		s, _, _ := newTestServer()
		_, err := s.createBead(ctxAsTestSubject(), createBeadInput{
			Title:  "bad evidence",
			Type:   "task",
			Fields: json.RawMessage(`{"gate_passed":true,"repro_evidence":"see the recording"}`),
		})
		if err == nil {
			t.Fatal("createBead with gate_passed=true and non-URL evidence should be rejected, got nil")
		}
		if _, ok := err.(inputError); !ok {
			t.Fatalf("expected inputError (→400 / InvalidArgument), got %T: %v", err, err)
		}
	})

	// gate_passed=true with no evidence: rejected — a passed gate must cite it.
	t.Run("rejects gate_passed=true with missing evidence", func(t *testing.T) {
		s, _, _ := newTestServer()
		_, err := s.createBead(ctxAsTestSubject(), createBeadInput{
			Title:  "no evidence",
			Type:   "task",
			Fields: json.RawMessage(`{"gate_passed":true}`),
		})
		if err == nil {
			t.Fatal("createBead with gate_passed=true and no evidence should be rejected, got nil")
		}
		if _, ok := err.(inputError); !ok {
			t.Fatalf("expected inputError, got %T: %v", err, err)
		}
	})

	// gate_passed=false: no evidence required — an in-progress/failed repro is
	// never wedged by the cross-field rule.
	t.Run("accepts gate_passed=false with no evidence", func(t *testing.T) {
		s, _, _ := newTestServer()
		if _, err := s.createBead(ctxAsTestSubject(), createBeadInput{
			Title:  "gate not passed",
			Type:   "task",
			Fields: json.RawMessage(`{"gate_passed":false}`),
		}); err != nil {
			t.Fatalf("gate_passed=false with no evidence should be accepted, got: %v", err)
		}
	})
}

// TestCreateBead_ReproFields_Molecule pins that the pour-level molecule epic
// accepts the same fields and the same cross-field gate (ka-0000002579).
func TestCreateBead_ReproFields_Molecule(t *testing.T) {
	s, ms, _ := newTestServer()
	bead, err := s.createBead(ctxAsTestSubject(), createBeadInput{
		Title:  "repro molecule",
		Type:   "molecule",
		Fields: json.RawMessage(`{"gate_passed":true,"repro_evidence":"https://viewer.local/beads/image/x"}`),
	})
	if err != nil {
		t.Fatalf("createBead(molecule) with repro fields should be accepted, got: %v", err)
	}
	if ms.beads[bead.ID] == nil {
		t.Fatalf("molecule %s not persisted", bead.ID)
	}

	// And the gate still fires on a molecule with bad evidence.
	if _, err := s.createBead(ctxAsTestSubject(), createBeadInput{
		Title:  "bad molecule",
		Type:   "molecule",
		Fields: json.RawMessage(`{"gate_passed":true,"repro_evidence":"nope"}`),
	}); err == nil {
		t.Fatal("createBead(molecule) with non-URL evidence should be rejected, got nil")
	}
}

// TestUpdateBead_ReproFields_PatchOrder covers the two-write orderings the
// repro-harness emit step and its triage mirror actually perform: setting
// gate_passed=true in a PATCH is validated against the repro_evidence already
// stored on the bead (and vice-versa), because bead.Fields is the post-merge
// effective state at validation time (ka-0000002579, acceptance #3/#4).
func TestUpdateBead_ReproFields_PatchOrder(t *testing.T) {
	seedTask := func(t *testing.T, ms *mockStore, id string, fields string) {
		t.Helper()
		b := &model.Bead{
			ID: id, Title: "triage bead", Type: "task", Kind: model.KindIssue,
			Status: model.StatusOpen, Labels: []string{"project:gardenio"},
		}
		if fields != "" {
			b.Fields = json.RawMessage(fields)
		}
		if err := ms.CreateBead(context.Background(), b); err != nil {
			t.Fatalf("seed task: %v", err)
		}
	}

	// Evidence stored first, then a later PATCH flips gate_passed=true: accepted
	// because the merged state carries a valid URL.
	t.Run("evidence first, gate flipped later", func(t *testing.T) {
		s, ms, _ := newTestServer()
		seedTask(t, ms, "gd-repro-a", `{"repro_evidence":"https://x.io/v.mp4"}`)
		if _, err := s.updateBead(ctxAsTestSubject(), "gd-repro-a", updateBeadInput{
			Fields: json.RawMessage(`{"gate_passed":true}`),
		}); err != nil {
			t.Fatalf("flipping gate_passed=true when a valid evidence URL is already stored should be accepted, got: %v", err)
		}
	})

	// gate_passed=true but the bead has no evidence and the PATCH doesn't add
	// one: rejected on the merged state.
	t.Run("gate flipped without any evidence", func(t *testing.T) {
		s, ms, _ := newTestServer()
		seedTask(t, ms, "gd-repro-b", "")
		_, err := s.updateBead(ctxAsTestSubject(), "gd-repro-b", updateBeadInput{
			Fields: json.RawMessage(`{"gate_passed":true}`),
		})
		if err == nil {
			t.Fatal("flipping gate_passed=true with no stored/added evidence should be rejected, got nil")
		}
		if _, ok := err.(inputError); !ok {
			t.Fatalf("expected inputError, got %T: %v", err, err)
		}
	})

	// gate already true (stored), a later PATCH clobbers evidence with a non-URL:
	// rejected — the merged state now has a passed gate with junk evidence.
	t.Run("gate stored true, evidence overwritten with junk", func(t *testing.T) {
		s, ms, _ := newTestServer()
		seedTask(t, ms, "gd-repro-c", `{"gate_passed":true,"repro_evidence":"https://x.io/v.mp4"}`)
		_, err := s.updateBead(ctxAsTestSubject(), "gd-repro-c", updateBeadInput{
			Fields: json.RawMessage(`{"repro_evidence":"broken"}`),
		})
		if err == nil {
			t.Fatal("overwriting evidence with a non-URL while gate_passed=true is stored should be rejected, got nil")
		}
		if _, ok := err.(inputError); !ok {
			t.Fatalf("expected inputError, got %T: %v", err, err)
		}
	})
}

// TestCreateBead_ReproScorecard_Task covers the newly-provisioned repro_scorecard
// field on a task bead: it was never registered on any schema, so the emit step's
// `kd update <emit-step> --field repro_scorecard=...` 400'd ("repro_scorecard:
// unknown field") and the per-axis critic scores (ka-0000008255) had to be posted
// as a non-queryable comment instead (ka-0000003501). Assert the field is accepted
// and persisted verbatim as a typed string.
func TestCreateBead_ReproScorecard_Task(t *testing.T) {
	s, ms, _ := newTestServer()
	scorecard := `{"repro":5,"clarity":4,"scope":3}`
	bead, err := s.createBead(ctxAsTestSubject(), createBeadInput{
		Title: "emit step",
		Type:  "task",
		Fields: json.RawMessage(fmt.Sprintf(`{
			"gate_passed": true,
			"repro_evidence": "https://s3.example.com/repro/FSM-472/video.mp4",
			"repro_ticket": "FSM-472",
			"repro_scorecard": %q
		}`, scorecard)),
	})
	if err != nil {
		t.Fatalf("createBead with repro_scorecard should be accepted, got: %v", err)
	}
	var f map[string]any
	if uerr := json.Unmarshal(ms.beads[bead.ID].Fields, &f); uerr != nil {
		t.Fatalf("unmarshal stored fields: %v", uerr)
	}
	if f["repro_scorecard"] != scorecard {
		t.Errorf("repro_scorecard not persisted verbatim: got %v want %q", f["repro_scorecard"], scorecard)
	}
}

// TestCreateBead_ReproFields_RichReport pins that the repro-harness gate fields
// are accepted on a rich-report bead — the type fsm-triage actually creates its
// triage beads as. Before the fix NONE of gate_passed / repro_evidence /
// repro_scorecard were registered on the rich-report schema, so the formula's
// Phase-4 step-3 mirror onto {{TICKET}} 400'd ("gate_passed: unknown field")
// for the exact bead type triage produces (ka-0000003501). The gate_passed →
// valid-evidence-URL cross-field rule is enforced here too so a mirrored pass
// can't be recorded without citing evidence.
func TestCreateBead_ReproFields_RichReport(t *testing.T) {
	// All four gate fields, gate_passed=true + valid URL: accepted and persisted.
	t.Run("accepts all repro fields with valid evidence", func(t *testing.T) {
		s, ms, _ := newTestServer()
		bead, err := s.createBead(ctxAsTestSubject(), createBeadInput{
			Title: "FSM-472 Triage",
			Type:  "rich-report",
			Fields: json.RawMessage(`{
				"gate_passed": true,
				"repro_evidence": "https://viewer.local/beads/image/abc",
				"repro_ticket": "FSM-472",
				"repro_scorecard": "{\"repro\":5}"
			}`),
		})
		if err != nil {
			t.Fatalf("createBead(rich-report) with repro fields should be accepted, got: %v", err)
		}
		var f map[string]any
		if uerr := json.Unmarshal(ms.beads[bead.ID].Fields, &f); uerr != nil {
			t.Fatalf("unmarshal stored fields: %v", uerr)
		}
		if f["gate_passed"] != true {
			t.Errorf("gate_passed not persisted: %v", f["gate_passed"])
		}
	})

	// gate_passed=true with a non-URL evidence: rejected, same as task/molecule —
	// the mirror target carries the same "no unbacked pass" invariant.
	t.Run("rejects gate_passed=true with non-URL evidence", func(t *testing.T) {
		s, _, _ := newTestServer()
		_, err := s.createBead(ctxAsTestSubject(), createBeadInput{
			Title:  "bad triage",
			Type:   "rich-report",
			Fields: json.RawMessage(`{"gate_passed":true,"repro_evidence":"see the recording"}`),
		})
		if err == nil {
			t.Fatal("createBead(rich-report) with gate_passed=true and non-URL evidence should be rejected, got nil")
		}
		if _, ok := err.(inputError); !ok {
			t.Fatalf("expected inputError (→400), got %T: %v", err, err)
		}
	})
}

// TestUpdateBead_ReproFields_RichReportMirror drives the exact two-PATCH mirror
// the repro formula performs against a rich-report triage bead: seed the triage
// bead with the evidence URL, then a later single-field PATCH flips
// gate_passed=true. Both writes must be accepted (no unknown-field 400) and the
// gate must be validated against the merged state, matching the task path
// (ka-0000003501).
func TestUpdateBead_ReproFields_RichReportMirror(t *testing.T) {
	seedReport := func(t *testing.T, ms *mockStore, id, fields string) {
		t.Helper()
		b := &model.Bead{
			ID: id, Title: "FSM-472 Triage", Type: "rich-report", Kind: model.KindIssue,
			Status: model.StatusOpen, Labels: []string{"project:gardenio"},
		}
		if fields != "" {
			b.Fields = json.RawMessage(fields)
		}
		if err := ms.CreateBead(context.Background(), b); err != nil {
			t.Fatalf("seed rich-report: %v", err)
		}
	}

	// Evidence + scorecard stored first, then a later PATCH flips gate_passed=true:
	// accepted because the merged state carries a valid URL.
	t.Run("evidence first, gate flipped later", func(t *testing.T) {
		s, ms, _ := newTestServer()
		seedReport(t, ms, "gd-rr-a", `{"repro_evidence":"https://x.io/v.mp4","repro_scorecard":"{\"repro\":5}"}`)
		if _, err := s.updateBead(ctxAsTestSubject(), "gd-rr-a", updateBeadInput{
			Fields: json.RawMessage(`{"gate_passed":true}`),
		}); err != nil {
			t.Fatalf("mirror PATCH flipping gate_passed=true on rich-report with stored evidence should be accepted, got: %v", err)
		}
	})

	// gate_passed=true flipped with no evidence anywhere: rejected on the merged
	// state, same invariant as the task emit step.
	t.Run("gate flipped without any evidence", func(t *testing.T) {
		s, ms, _ := newTestServer()
		seedReport(t, ms, "gd-rr-b", "")
		_, err := s.updateBead(ctxAsTestSubject(), "gd-rr-b", updateBeadInput{
			Fields: json.RawMessage(`{"gate_passed":true}`),
		})
		if err == nil {
			t.Fatal("flipping gate_passed=true on rich-report with no stored/added evidence should be rejected, got nil")
		}
		if _, ok := err.(inputError); !ok {
			t.Fatalf("expected inputError, got %T: %v", err, err)
		}
	})
}

// TestUpdateBead_UnknownFieldRejectedAtomically closes bug concern #3
// (ka-0000003501): the bug reported that "an earlier multi-field kd update
// returned success-looking output while persisting nothing, so this can fail
// silently." validateFields accumulates ALL field errors and the update path
// returns them BEFORE store.UpdateBead, so a multi-field PATCH that mixes a
// known field with an unknown one must reject the WHOLE write (no partial
// persistence) rather than silently succeed. This pins that contract so a future
// refactor can't reintroduce a silent-drop path.
func TestUpdateBead_UnknownFieldRejectedAtomically(t *testing.T) {
	s, ms, _ := newTestServer()
	b := &model.Bead{
		ID: "gd-atomic-1", Title: "triage bead", Type: "rich-report", Kind: model.KindIssue,
		Status: model.StatusOpen, Labels: []string{"project:gardenio"},
	}
	if err := ms.CreateBead(context.Background(), b); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// One known field (repro_ticket, now registered on rich-report) mixed with a
	// genuinely-unknown key: the whole PATCH must error, and nothing must persist.
	_, err := s.updateBead(ctxAsTestSubject(), "gd-atomic-1", updateBeadInput{
		Fields: json.RawMessage(`{"repro_ticket":"FSM-472","totally_bogus_field":"x"}`),
	})
	if err == nil {
		t.Fatal("multi-field update with an unknown field should be rejected, got nil (silent success is the bug)")
	}
	if _, ok := err.(inputError); !ok {
		t.Fatalf("expected inputError (→400), got %T: %v", err, err)
	}
	// The known field must NOT have leaked through — the write is all-or-nothing.
	if fields := ms.beads["gd-atomic-1"].Fields; len(fields) > 0 {
		var f map[string]any
		_ = json.Unmarshal(fields, &f)
		if _, present := f["repro_ticket"]; present {
			t.Errorf("rejected update partially persisted repro_ticket: %s", fields)
		}
	}
}
