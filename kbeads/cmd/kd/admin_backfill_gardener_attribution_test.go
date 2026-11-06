package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

// gardenerTriggerBead builds a type=trigger + role:gardener bead whose `gardener`
// JSON blob is the marshalled capFields map (nil = no gardener field at all).
func gardenerTriggerBead(id, operatedBy string, capFields map[string]any) *model.Bead {
	fields := map[string]any{"role": "gardener"}
	if capFields != nil {
		raw, _ := json.Marshal(capFields)
		fields["gardener"] = string(raw)
	}
	rawFields, _ := json.Marshal(fields)
	return &model.Bead{
		ID:         id,
		Type:       "trigger",
		Kind:       model.KindIssue,
		Status:     model.StatusOpen,
		OperatedBy: operatedBy,
		Labels:     []string{"role:gardener"},
		Fields:     rawFields,
	}
}

// agentIterationBead builds a type=agent iteration carrying schedule_id plus
// the supplied flat owner fields.
func agentIterationBead(id, scheduleID, slackUserID, userBeadID string) *model.Bead {
	fields := map[string]any{"schedule_id": scheduleID}
	if slackUserID != "" {
		fields["slack_user_id"] = slackUserID
	}
	if userBeadID != "" {
		fields["user_bead_id"] = userBeadID
	}
	raw, _ := json.Marshal(fields)
	return &model.Bead{
		ID:     id,
		Type:   "agent",
		Kind:   model.KindIssue,
		Status: model.StatusClosed,
		Fields: raw,
	}
}

func TestDecodeGardenerBlob(t *testing.T) {
	t.Run("nil → empty map", func(t *testing.T) {
		m, err := decodeGardenerBlob(nil)
		if err != nil || len(m) != 0 {
			t.Fatalf("got %v, %v", m, err)
		}
	})
	t.Run("empty string → empty map", func(t *testing.T) {
		m, err := decodeGardenerBlob("  ")
		if err != nil || len(m) != 0 {
			t.Fatalf("got %v, %v", m, err)
		}
	})
	t.Run("json string decodes", func(t *testing.T) {
		m, err := decodeGardenerBlob(`{"slack_user_id":"U1","name":"cap"}`)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if m["slack_user_id"] != "U1" {
			t.Errorf("slack_user_id = %v", m["slack_user_id"])
		}
	})
	t.Run("already-decoded map passes through", func(t *testing.T) {
		in := map[string]any{"user_bead_id": "gd-u1"}
		m, err := decodeGardenerBlob(in)
		if err != nil || m["user_bead_id"] != "gd-u1" {
			t.Fatalf("got %v, %v", m, err)
		}
	})
	t.Run("malformed → error", func(t *testing.T) {
		if _, err := decodeGardenerBlob(`{not json`); err == nil {
			t.Fatal("expected error for malformed JSON")
		}
	})
}

func TestPlanGardenerAttribution_Idempotent(t *testing.T) {
	// A gardener already carrying slack_user_id is skipped without resolving.
	b := gardenerTriggerBead("gd-cap1", "slack:U0OPERATOR1", map[string]any{
		"name": "delivery", "slack_user_id": "U0OPERATOR1",
	})
	owner, rows, patch := planGardenerAttribution(context.Background(), b, nil, nil)
	if patch != nil {
		t.Fatalf("expected nil patch, got %s", patch)
	}
	if len(rows) != 1 || rows[0].Status != statusSkipIdempotent {
		t.Fatalf("rows = %+v", rows)
	}
	if owner.SlackUserID != "U0OPERATOR1" {
		t.Errorf("owner.SlackUserID = %q", owner.SlackUserID)
	}
}

func TestPlanGardenerAttribution_Malformed(t *testing.T) {
	// A trigger bead whose gardener field is malformed JSON is an ERROR row.
	rawFields, _ := json.Marshal(map[string]any{"role": "gardener", "gardener": "{bad"})
	b := &model.Bead{ID: "gd-capBad", Type: "trigger", Fields: rawFields}
	_, rows, patch := planGardenerAttribution(context.Background(), b, nil, nil)
	if patch != nil {
		t.Fatalf("expected nil patch, got %s", patch)
	}
	if len(rows) != 1 || rows[0].Status != statusError {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestPlanGardenerAttribution_WouldUpdate(t *testing.T) {
	// A gardener missing both fields resolves its oidc operator to a user bead
	// and proposes slack_user_id (from the user bead's slack_id) + user_bead_id.
	mc := newMockClient()
	userBead := &model.Bead{
		ID: "gd-userMatt", Type: "user", Kind: model.KindConfig,
		Fields: mustFields(map[string]any{"email": "matt@acme.example", "slack_id": "U0OPERATOR1"}),
	}
	mc.ListBeadsResult = []*model.Bead{userBead} // findUserBead returns this
	mc.Beads["gd-userMatt"] = userBead           // GetBead for slack_id derivation
	withMockClient(t, mc)

	b := gardenerTriggerBead("gd-cap2", "oidc:matt@acme.example", map[string]any{"name": "release"})
	owner, rows, patch := planGardenerAttribution(context.Background(), b,
		map[string]string{}, map[string]string{})

	if len(patch) == 0 {
		t.Fatal("expected a non-empty gardener patch")
	}
	if owner.UserBeadID != "gd-userMatt" || owner.SlackUserID != "U0OPERATOR1" {
		t.Fatalf("owner = %+v", owner)
	}
	// Both fields planned as WOULD_UPDATE.
	var sawSlack, sawUserBead bool
	for _, r := range rows {
		if r.Status != statusWouldUpdate {
			t.Errorf("row %+v not WOULD_UPDATE", r)
		}
		switch r.Field {
		case "slack_user_id":
			sawSlack = true
			if r.New != "U0OPERATOR1" || r.Old != "(unset)" {
				t.Errorf("slack row = %+v", r)
			}
		case "user_bead_id":
			sawUserBead = true
			if r.New != "gd-userMatt" {
				t.Errorf("user_bead row = %+v", r)
			}
		}
	}
	if !sawSlack || !sawUserBead {
		t.Errorf("missing rows: slack=%v userBead=%v", sawSlack, sawUserBead)
	}
	// The patch carries the merged gardener blob with both fields stamped, and
	// preserves the pre-existing `name` key.
	var p map[string]json.RawMessage
	if err := json.Unmarshal(patch, &p); err != nil {
		t.Fatalf("patch unmarshal: %v", err)
	}
	var cap map[string]any
	if err := json.Unmarshal(p["gardener"], &cap); err != nil {
		t.Fatalf("gardener unmarshal: %v", err)
	}
	if cap["slack_user_id"] != "U0OPERATOR1" || cap["user_bead_id"] != "gd-userMatt" || cap["name"] != "release" {
		t.Errorf("merged gardener = %+v", cap)
	}
}

func TestPlanGardenerAttribution_NoOwner(t *testing.T) {
	// A sentinel operator resolves to nothing → SKIP_NO_OWNER, no patch.
	b := gardenerTriggerBead("gd-cap3", "system:scheduler", map[string]any{"name": "x"})
	owner, rows, patch := planGardenerAttribution(context.Background(), b,
		map[string]string{}, map[string]string{})
	if patch != nil {
		t.Fatalf("expected nil patch, got %s", patch)
	}
	if owner.SlackUserID != "" || owner.UserBeadID != "" {
		t.Errorf("owner = %+v", owner)
	}
	if len(rows) != 1 || rows[0].Status != statusSkipNoOwner {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestPlanIterationOwner_Idempotent(t *testing.T) {
	b := agentIterationBead("gd-iter1", "gd-cap1", "U0OPERATOR1", "")
	rows, patch := planIterationOwner(context.Background(), b, map[string]gardenerAttribution{}, nil, nil)
	if patch != nil {
		t.Fatalf("expected nil patch, got %s", patch)
	}
	if len(rows) != 1 || rows[0].Status != statusSkipIdempotent {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestPlanIterationOwner_InheritFromCache(t *testing.T) {
	// A cache-hit on ownerByGardenerID inherits the owner with no client calls.
	b := agentIterationBead("gd-iter2", "gd-cap2", "", "")
	cache := map[string]gardenerAttribution{
		"gd-cap2": {SlackUserID: "U0OPERATOR1", UserBeadID: "gd-userMatt"},
	}
	rows, patch := planIterationOwner(context.Background(), b, cache, nil, nil)
	if len(patch) == 0 {
		t.Fatal("expected a patch")
	}
	var p map[string]string
	if err := json.Unmarshal(patch, &p); err != nil {
		t.Fatalf("patch unmarshal: %v", err)
	}
	if p["slack_user_id"] != "U0OPERATOR1" || p["user_bead_id"] != "gd-userMatt" {
		t.Errorf("patch = %+v", p)
	}
	for _, r := range rows {
		if r.Status != statusWouldUpdate {
			t.Errorf("row %+v not WOULD_UPDATE", r)
		}
	}
}

func TestPlanIterationOwner_NoGardener(t *testing.T) {
	// schedule_id points at a missing gardener → SKIP_NO_GARDENER, no patch.
	mc := newMockClient()
	mc.GetErr = nil // GetBead returns "not found" for an unknown id
	withMockClient(t, mc)

	b := agentIterationBead("gd-iter3", "gd-gone", "", "")
	rows, patch := planIterationOwner(context.Background(), b,
		map[string]gardenerAttribution{}, map[string]string{}, map[string]string{})
	if patch != nil {
		t.Fatalf("expected nil patch, got %s", patch)
	}
	if len(rows) != 1 || rows[0].Status != statusSkipNoGardener {
		t.Fatalf("rows = %+v", rows)
	}
}

// TestAdminBackfillGardenerAttribution_DryRunIdempotent exercises the full RunE in
// --dry-run against already-attributed data: every row must be SKIP_IDEMPOTENT,
// no UpdateBead is issued, and the PHASE/BEAD_ID/FIELD/OLD/NEW/STATUS header is
// printed. This is the idempotency acceptance check from design doc §7.
func TestAdminBackfillGardenerAttribution_DryRunIdempotent(t *testing.T) {
	mc := newMockClient()
	// Page 0: gardeners (already attributed). Page 1: agent iterations
	// (already attributed). Each page is < page-size so each phase terminates
	// after one ListBeads call without an intermixed user-bead lookup.
	mc.ListBeadsPages = [][]*model.Bead{
		{gardenerTriggerBead("gd-cap1", "slack:U0OPERATOR1", map[string]any{
			"name": "delivery", "slack_user_id": "U0OPERATOR1", "user_bead_id": "gd-userMatt",
		})},
		{agentIterationBead("gd-iter1", "gd-cap1", "U0OPERATOR1", "gd-userMatt")},
	}
	withMockClient(t, mc)
	t.Cleanup(func() { _ = adminBackfillGardenerAttributionCmd.Flags().Set("dry-run", "false") })
	_ = adminBackfillGardenerAttributionCmd.Flags().Set("dry-run", "true")

	out := captureStdout(t, func() {
		if err := adminBackfillGardenerAttributionCmd.RunE(adminBackfillGardenerAttributionCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if len(mc.UpdateBeadCalls) != 0 {
		t.Errorf("dry-run issued %d UpdateBead calls", len(mc.UpdateBeadCalls))
	}
	if !strings.Contains(out, "PHASE\tBEAD_ID\tFIELD\tOLD\tNEW\tSTATUS") &&
		!strings.Contains(out, "PHASE") {
		t.Errorf("missing tab header in output:\n%s", out)
	}
	if !strings.Contains(out, string(statusSkipIdempotent)) {
		t.Errorf("expected SKIP_IDEMPOTENT rows:\n%s", out)
	}
	if strings.Contains(out, string(statusWouldUpdate)) {
		t.Errorf("unexpected WOULD_UPDATE in all-idempotent run:\n%s", out)
	}
}

func mustFields(m map[string]any) json.RawMessage {
	raw, _ := json.Marshal(m)
	return raw
}
