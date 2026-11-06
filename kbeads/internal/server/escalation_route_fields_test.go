package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

func TestParseLaneRef(t *testing.T) {
	good := map[string]laneRef{
		"operator":               {Kind: laneOperator, Raw: "operator"},
		" requester ":            {Kind: laneRequester, Raw: "requester"},
		"server":                 {Kind: laneServer, Raw: "server"},
		"coordinator:merge-desk": {Kind: laneCoordinator, Name: "merge-desk", Raw: "coordinator:merge-desk"},
		"steward:acme":           {Kind: laneSteward, Name: "acme", Raw: "steward:acme"},
		"land-owner:acme":        {Kind: laneLandOwner, Name: "acme", Raw: "land-owner:acme"},
		"oidc:a@example.com":     {Kind: laneLegacy, Raw: "oidc:a@example.com"},
		"team:gd-team1":          {Kind: laneLegacy, Raw: "team:gd-team1"},
		"slack:U0ABC":            {Kind: laneLegacy, Raw: "slack:U0ABC"},
		"a@example.com":          {Kind: laneLegacy, Raw: "a@example.com"},
	}
	for in, want := range good {
		got, err := parseLaneRef(in)
		if err != nil || got != want {
			t.Errorf("parseLaneRef(%q) = %+v, %v; want %+v", in, got, err, want)
		}
		if got.String() != strings.TrimSpace(in) {
			t.Errorf("parseLaneRef(%q).String() = %q, want the trimmed input", in, got.String())
		}
	}
	for _, in := range []string{"", "  ", "coordinator:", "steward:", "land-owner:a b", "coordinator:{land}", "operators", "role:reviewer", "machine:x", "oidc:"} {
		if got, err := parseLaneRef(in); err == nil {
			t.Errorf("parseLaneRef(%q) = %+v, want an error", in, got)
		}
	}
}

// escalationTestAgent is the agent bead the escalation route tests act as.
const escalationTestAgent = "gd-crew-agent"

// escalationAgentHandler serves s for an agent pod on its own per-agent fleet
// token (agentBearer, fleet agent-gd-crew-agent), the way agent pods
// authenticate now that the shared static token with an X-Actor claim is
// retired (gd-ke6PC7bRto). It seeds the agent's bead in ms, which the
// capability-tier check reads. fc may be nil.
func escalationAgentHandler(s *BeadsServer, ms *mockStore, fc *FleetConfig) http.Handler {
	if fc == nil {
		fc = NewFleetConfig("gardenio", nil)
	}
	seedAgent(ms, escalationTestAgent, "crew", "gardenio")
	agentBearer(fc, escalationTestAgent)
	s.SetFleetAuth(fc)
	return s.NewHTTPHandler(nil, fc)
}

// doAgentEscalationReq sends a request as escalationTestAgent on its own
// per-agent token (escalationAgentHandler). The agent is a member of no lane
// and is not an admin, so the route-field guards apply.
func doAgentEscalationReq(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Authorization", "Bearer agent-tok-"+escalationTestAgent)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Operated-By", "slack:U0OPERATOR")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// forgedRouteValues has one forged value per server-only field (Global
// Constraint 5 and Review Focus 1).
var forgedRouteValues = map[string]any{
	"held_by":            "coordinator:laptop",
	"held_at":            "2026-10-20T00:00:00Z",
	"execute_by":         "requester",
	"approvers":          []string{"role:crew"},
	"operator_only":      false,
	"policy_rule":        "escalation#1",
	"approved_by":        "machine:gardenio-laptop-operator",
	"approved_by_email":  "x@example.com",
	"approved_at":        "2026-10-20T00:00:00Z",
	"approval_method":    "coordinator",
	"approved_as":        "coordinator:laptop",
	"claimed_by":         "gd-crew-agent",
	"claimed_at":         "2026-10-20T00:00:00Z",
	"handed_up_by":       "gd-crew-agent",
	"handed_up_at":       "2026-10-20T00:00:00Z",
	approvedTargetsField: []string{"gd-forged"},
	"hand_up_reason":     "forged",
}

func TestEscalationRouteFieldsServerOnly_CreateAndPatch(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := escalationAgentHandler(s, ms, nil)

	baseFields := func() map[string]any {
		return map[string]any{"operation_type": "github_pr_merge", "operation_spec": map[string]any{"pr": 1}}
	}

	// Positive control: the same create without route fields succeeds.
	rec := doAgentEscalationReq(t, h, http.MethodPost, "/v1/beads", map[string]any{
		"title": "merge PR 1", "type": "escalation", "kind": "data", "fields": baseFields(),
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("control create: got %d, want 201: %s", rec.Code, rec.Body.String())
	}

	for field, forged := range forgedRouteValues {
		t.Run("create/"+field, func(t *testing.T) {
			f := baseFields()
			f[field] = forged
			rec := doAgentEscalationReq(t, h, http.MethodPost, "/v1/beads", map[string]any{
				"title": "forged " + field, "type": "escalation", "kind": "data", "fields": f,
			})
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), field) {
				t.Fatalf("create with %s: got %d %s, want 400 naming the field", field, rec.Code, rec.Body.String())
			}
		})
	}

	seed := func(id string) {
		fields, _ := json.Marshal(map[string]any{
			"operation_type": "github_pr_merge", "operation_spec": map[string]any{"pr": 2},
			"held_by": "operator", "execute_by": "coordinator:merge-desk",
			"approvers": []string{"coordinator:merge-desk", "operator"},
			// Seeded values differ from forgedRouteValues, so each forged PATCH is
			// a change: the guard refuses changes, not a no-op rewrite.
			"operator_only": true, "policy_rule": "escalation#0",
		})
		ms.beads[id] = &model.Bead{
			ID: id, Kind: model.KindData, Type: model.TypeEscalation, Title: "merge PR 2",
			Status: model.StatusOpen, Fields: fields, CreatedBy: "gd-crew-agent",
		}
	}

	// Positive control: PATCHing an unguarded field on the same fixture works.
	seed("gd-esc-patch-ok")
	rec = doAgentEscalationReq(t, h, http.MethodPatch, "/v1/beads/gd-esc-patch-ok", map[string]any{
		"fields": map[string]any{"context": "more detail"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("control patch: got %d, want 200: %s", rec.Code, rec.Body.String())
	}

	for field, forged := range forgedRouteValues {
		t.Run("patch/"+field, func(t *testing.T) {
			id := "gd-esc-patch-" + strings.ReplaceAll(field, "_", "-")
			seed(id)
			before := string(ms.beads[id].Fields)
			rec := doAgentEscalationReq(t, h, http.MethodPatch, "/v1/beads/"+id, map[string]any{
				"fields": map[string]any{field: forged},
			})
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "immutable on an escalation") {
				t.Fatalf("patch %s: got %d %s, want 400", field, rec.Code, rec.Body.String())
			}
			if after := string(ms.beads[id].Fields); after != before {
				t.Fatalf("patch %s: bead changed despite the 400:\nbefore %s\nafter  %s", field, before, after)
			}
		})
	}
}

// TestEscalationRouteFields_AdminPatchRefusedRouteRepairs replaces #726's
// AdminRepairAllowed (CONTRACT ISSUE 9, revised): held_by, held_at, execute_by
// and operator_only are #756 frozen fields, so even a gardenio-admin's PATCH
// is refused; otherwise held_by and audience could disagree (D11). Repair is
// the route endpoint, whose escalationRouteKey context passes both guards.
func TestEscalationRouteFields_AdminPatchRefusedRouteRepairs(t *testing.T) {
	s, ms, h := newTestServer() // OIDC gardenio-admin
	fields, _ := json.Marshal(map[string]any{"operation_type": "github_pr_merge", "operation_spec": map[string]any{"pr": 3}, "held_by": "coordinator:merge-desk", "audience": "operator"})
	ms.beads["gd-esc-repair"] = &model.Bead{
		ID: "gd-esc-repair", Kind: model.KindData, Type: model.TypeEscalation, Title: "t",
		Status: model.StatusOpen, Fields: fields,
	}
	for _, field := range []string{"held_by", "held_at", "execute_by", "operator_only"} {
		before := string(ms.beads["gd-esc-repair"].Fields)
		rec := doJSON(t, h, http.MethodPatch, "/v1/beads/gd-esc-repair", map[string]any{
			"fields": map[string]any{field: forgedRouteValues[field]},
		})
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "fixed when an escalation is filed") {
			t.Fatalf("admin patch %s: got %d %s, want 400 (frozen)", field, rec.Code, rec.Body.String())
		}
		if string(ms.beads["gd-esc-repair"].Fields) != before {
			t.Fatalf("admin patch %s changed the bead despite the 400", field)
		}
	}
	// Positive control: an unfrozen field on the same bead, same admin.
	rec := doJSON(t, h, http.MethodPatch, "/v1/beads/gd-esc-repair", map[string]any{
		"fields": map[string]any{"context": "repair note"},
	})
	requireStatus(t, rec, http.StatusOK)
	// The route context writes held_by (Task 5 builds the endpoint on it).
	ctx := context.WithValue(context.Background(), escalationRouteKey{}, true)
	patch, _ := json.Marshal(map[string]any{"held_by": "operator", "audience": "owner"})
	if _, err := s.updateBead(ctx, "gd-esc-repair", updateBeadInput{Fields: patch}); err != nil {
		t.Fatalf("route-context write refused: %v", err)
	}
}

// TestApproversNamePerson_LaneRefsAreNotPeople: #756's forced-owner rule must
// not read Task 2's stamped lane refs as people. Legacy person entries still
// force (the #756 rows in TestEscalationAudienceForcedByApprovers stay).
func TestApproversNamePerson_LaneRefsAreNotPeople(t *testing.T) {
	for _, list := range [][]string{
		{"operator"},
		{"coordinator:merge-desk", "coordinator:laptop", "operator"},
		{"steward:acme", "land-owner:acme"},
		{"requester"},
		{"server"},
		{"team:ops"},
	} {
		if approversNamePerson(list) || approversNamePerson(anySlice(list)) {
			t.Errorf("approversNamePerson(%v) = true, want false (lane refs)", list)
		}
	}
	for _, list := range [][]string{{"oidc:owner@example.com"}, {"operator", "owner@example.com"}, {"slack:U0ABC"}} {
		if !approversNamePerson(anySlice(list)) {
			t.Errorf("approversNamePerson(%v) = false, want true (positive control)", list)
		}
	}
}

func anySlice(in []string) []any {
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}

// P7 Task 10: approved_by is a decision field, so /result refuses it from
// every caller, approved escalation or not. The reaper sends only its render
// and witness keys; on an approved escalation the real approver is kept.
func TestCaptureEscalationResult_ApprovedByRefused(t *testing.T) {
	// The caller models EscalationReaper, which posts /result on the bridge's
	// fleet token; the primary static token is refused there (caller class
	// service, gd-nPfSNHTkQQ).
	_, ms, h := newClassedTestServer(t)
	postResult := func(id string, body map[string]any) *httptest.ResponseRecorder {
		t.Helper()
		return doBearerJSON(t, h, http.MethodPost, "/v1/escalations/"+id+"/result", testBridgeToken, body, nil)
	}
	seed := func(id string, extra map[string]any) {
		f := map[string]any{"operation_type": "gitlab_mr_merge"}
		for k, v := range extra {
			f[k] = v
		}
		raw, _ := json.Marshal(f)
		ms.beads[id] = &model.Bead{ID: id, Kind: model.KindData, Type: model.TypeEscalation, Title: "t", Status: model.StatusOpen, Fields: raw}
	}

	seed("gd-esc-r1", nil)
	rec := postResult("gd-esc-r1", map[string]any{
		"result_status": "success", "fields": map[string]string{"approved_by": "alice"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("approved_by on an unapproved escalation: got %d %s, want 400", rec.Code, rec.Body.String())
	}
	if ms.beads["gd-esc-r1"].Status != model.StatusOpen {
		t.Fatalf("refused /result closed the escalation")
	}

	// Positive control: a reaper-shaped /result on an approved, in_progress
	// escalation closes it with the audit trail merged and approved_by kept.
	seed("gd-esc-r2", map[string]any{"approved_by": "owner@example.com", "approved_at": "2026-10-20T00:00:00Z"})
	ms.beads["gd-esc-r2"].Status = model.StatusInProgress
	rec = postResult("gd-esc-r2", map[string]any{
		"result_status": "success",
		"fields": map[string]string{
			"reaped":           "true",
			"reaped_by":        "escalation-reaper",
			"reaped_at":        "2026-10-26T00:00:00Z",
			"reaped_reason":    "MR merged out-of-band",
			"reaped_satisfier": "mallory",
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("reaper-shaped /result on an approved escalation: got %d %s, want 200", rec.Code, rec.Body.String())
	}
	reaped := ms.beads["gd-esc-r2"]
	if reaped.Status != model.StatusClosed {
		t.Fatalf("reaped approved escalation status = %q, want closed", reaped.Status)
	}
	var got map[string]any
	if err := json.Unmarshal(reaped.Fields, &got); err != nil {
		t.Fatal(err)
	}
	if got["approved_by"] != "owner@example.com" {
		t.Errorf("approved_by = %v, want the real approver owner@example.com kept", got["approved_by"])
	}
	if got["reaped"] != "true" || got["reaped_by"] != "escalation-reaper" || got["reaped_satisfier"] != "mallory" {
		t.Errorf("reaper audit trail not merged: %v", got)
	}

	// Every other route or provenance field is refused on /result.
	for _, field := range []string{"held_by", "approved_at", "approval_method", "approved_as", "execute_by"} {
		seed("gd-esc-r3-"+field, nil)
		rec = postResult("gd-esc-r3-"+field, map[string]any{
			"result_status": "success", "fields": map[string]string{field: "x"},
		})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("/result fields.%s: got %d %s, want 400", field, rec.Code, rec.Body.String())
		}
	}
}

func TestBuiltinEscalationRouteFieldsDeclared(t *testing.T) {
	var tc model.TypeConfig
	if err := json.Unmarshal(builtinConfigs["type:escalation"].Value, &tc); err != nil {
		t.Fatal(err)
	}
	have := map[string]model.FieldDef{}
	for _, f := range tc.Fields {
		have[f.Name] = f
	}
	for _, name := range append([]string{"approvers"}, escalationRouteFields...) {
		if _, ok := have[name]; !ok {
			t.Errorf("type:escalation does not declare %q", name)
		}
	}
	if !strings.Contains(strings.Join(have["approval_method"].Values, ","), "coordinator") {
		t.Errorf("approval_method enum lacks coordinator: %v", have["approval_method"].Values)
	}
}
