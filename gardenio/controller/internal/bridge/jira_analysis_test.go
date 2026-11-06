package bridge

// Client-level unit + guard tests for the ONE upserted GBFSM ticket per FSM ticket
// (1:1 reshape ka-0000002637, Phase 2 ka-0000001491). The collapsed writer replaces
// the fan-out (1 analysis + N Add-Label + 1 Set-Version + 1 link-back intent) with a
// SINGLE GBFSM ticket carrying the analysis in its Description PLUS the N triage
// labels and recommended Affects-Version as FIELDS.
//
// This file pins the CREATE half + the body/summary helpers; the SearchGBFSM /
// EditGBFSMTicket read+upsert half and the security guards live in
// jira_gbfsm_upsert_test.go, and the wired handleUpdated path in
// jira_triage_bridge_test.go.
//
// Three things are proven here:
//
//  1. WIRE FORMAT — CreateGBFSMTicket POSTs to the issue-collection endpoint with
//     the GBFSM project HARDCODED, the configured "Analysis" issue-type id, the FSM
//     target key in customfield_10148 (DATA), the full analysis in the Description,
//     the idempotency key in customfield_10150, and — when their (operator-
//     configured) field ids are set — the N labels + version as FIELDS. It returns
//     the created issue key so a subsequent re-triage can upsert it.
//
//  2. GBFSM-ONLY / NO-FSM-TARGET — exactly like CreateOutboxIntent, a caller-
//     supplied FSMTargetKey (even a customer-project or FSM key) can NEVER redirect
//     the create off GBFSM; the target rides only in the custom field.
//
//  3. INERT-BY-DEFAULT — an empty issue-type id (the scaffold-pending default) sends
//     NO request, and unconfigured label/version field ids OMIT those fields (the
//     analysis Description still carries the classification in prose), so merging
//     changes nothing until the operator scaffolds the type + fields.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// analysisTestType is a TEST-ONLY placeholder for the GBFSM "Analysis" issue-type
// id. The real id does not exist yet (operator scaffold pending, see the const
// outboxTypeAnalysis and the scaffold-dependency note on ka-0000001294); the tests
// supply their own so they can exercise the armed path without inventing a
// production id.
const analysisTestType = "10099"

// labelsTestField / versionTestField are TEST-ONLY placeholder custom-field ids for
// the GBFSM labels + version fields (the real ids are scaffold-pending — the consts
// outboxFieldLabels / outboxFieldVersion default empty). Tests set them so they can
// assert the collapsed ticket carries the classification as FIELDS.
const (
	labelsTestField  = "customfield_10160"
	versionTestField = "customfield_10161"
)

// newKeyedJiraRecordingServer is newJiraRecordingServer's sibling that also returns
// a created issue "key" on every create POST (so CreateGBFSMTicket resolves a real
// key) and, on the SearchGBFSM GET (/search/jql), an EMPTY issues array — so the
// upsert takes the CREATE branch (no existing ticket). Every request is recorded.
func newKeyedJiraRecordingServer(key string) (*httptest.Server, *jiraRecordingServer) {
	rec := &jiraRecordingServer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		rec.mu.Lock()
		rec.requests = append(rec.requests, recordedJiraRequest{Method: r.Method, Path: r.URL.Path, Body: body})
		rec.mu.Unlock()

		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/transitions"):
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"transitions":[{"id":"31","name":"In Progress","to":{"name":"In Progress"}},{"id":"41","name":"Review","to":{"name":"Review"}}]}`)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/search/jql"):
			// No existing GBFSM ticket ⇒ the upsert takes the CREATE branch.
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"issues":[]}`)
		default:
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"id":"1","key":%q}`, key)
		}
	}))
	return srv, rec
}

// ── CreateGBFSMTicket wire format ──

// TestCreateGBFSMTicket_WireFormat pins the GBFSM-ticket create shape: GBFSM project
// (10037), the configured issue type, target in customfield_10148, the analysis in
// the Description, the idempotency key in customfield_10150, the N labels + version
// as FIELDS (under the configured field ids), and the created key from the response.
func TestCreateGBFSMTicket_WireFormat(t *testing.T) {
	srv, rec := newKeyedJiraRecordingServer("GBFSM-501")
	defer srv.Close()
	c := newTestJiraClient(srv.URL)

	key, err := c.CreateGBFSMTicket(context.Background(), GBFSMTicket{
		IssueTypeID:    analysisTestType,
		FSMTargetKey:   "FSM-408",
		Summary:        "Gardenio analysis: FSM-408",
		Body:           "Severity: 2\nGxP risk: med\n\nLog findings:\ncrash loop",
		IdempotencyKey: outboxIdempotencyKey("FSM-408", "gbfsm", ""),
		Labels:         []string{"gardenio-triaged", "gardenio-triaged-sev-2"},
		LabelsFieldID:  labelsTestField,
		Version:        "2026.7.0",
		VersionFieldID: versionTestField,
	})
	if err != nil {
		t.Fatalf("CreateGBFSMTicket: %v", err)
	}
	if key != "GBFSM-501" {
		t.Errorf("returned key = %q, want the created GBFSM-501", key)
	}

	fields := createdFields(t, rec)
	if got := projectID(t, fields); got != outboxProjectID {
		t.Errorf("create project id = %q, want GBFSM %q", got, outboxProjectID)
	}
	if got := issueTypeID(t, fields); got != analysisTestType {
		t.Errorf("issuetype id = %q, want the analysis type %q", got, analysisTestType)
	}
	if got, _ := fields[outboxFieldTargetKey].(string); got != "FSM-408" {
		t.Errorf("fsm_target_key (%s) = %q, want FSM-408", outboxFieldTargetKey, got)
	}
	if _, hasDesc := fields["description"]; !hasDesc {
		t.Error("GBFSM ticket must carry the analysis body as a Description")
	}
	// idempotency keyed on FSM-N (not the bead) so a re-triage upserts.
	if got, _ := fields[outboxFieldIdempotencyKey].(string); got != "FSM-408:gbfsm" {
		t.Errorf("idempotency_key (%s) = %q, want FSM-408:gbfsm", outboxFieldIdempotencyKey, got)
	}
	// The N labels + version ride as FIELDS on the ONE ticket (the collapse).
	gotLabels := fieldStrings(t, fields, labelsTestField)
	if len(gotLabels) != 2 || gotLabels[0] != "gardenio-triaged" || gotLabels[1] != "gardenio-triaged-sev-2" {
		t.Errorf("labels field (%s) = %v, want the 2 triage labels", labelsTestField, gotLabels)
	}
	if got, _ := fields[versionTestField].(string); got != "2026.7.0" {
		t.Errorf("version field (%s) = %q, want 2026.7.0", versionTestField, got)
	}
	// The single ticket must NOT carry op_value (it is not an Add-Label/Set-Version intent).
	if _, hasOp := fields[outboxFieldOpValue]; hasOp {
		t.Error("GBFSM ticket must NOT carry an op_value")
	}
}

// TestCreateGBFSMTicket_OmitsUnscaffoldedFields proves the field-inertness contract:
// with the label/version field ids UNSET (the scaffold-pending default), the ticket
// is still created (Description + fsm_target_key) but carries NO labels/version
// fields — so the writer works before the fields are scaffolded.
func TestCreateGBFSMTicket_OmitsUnscaffoldedFields(t *testing.T) {
	srv, rec := newKeyedJiraRecordingServer("GBFSM-1")
	defer srv.Close()
	c := newTestJiraClient(srv.URL)

	if _, err := c.CreateGBFSMTicket(context.Background(), GBFSMTicket{
		IssueTypeID:  analysisTestType,
		FSMTargetKey: "FSM-9",
		Body:         "analysis",
		Labels:       []string{"gardenio-triaged"},
		Version:      "2026.7.0",
		// LabelsFieldID / VersionFieldID deliberately empty (unscaffolded).
	}); err != nil {
		t.Fatalf("CreateGBFSMTicket: %v", err)
	}
	fields := createdFields(t, rec)
	if _, hasDesc := fields["description"]; !hasDesc {
		t.Error("ticket must still carry the Description even with fields unscaffolded")
	}
	if _, has := fields[labelsTestField]; has {
		t.Error("labels field must be OMITTED when its field id is unset")
	}
	if _, has := fields[versionTestField]; has {
		t.Error("version field must be OMITTED when its field id is unset")
	}
}

// TestCreateGBFSMTicket_DefaultSummary proves an empty Summary generates a sensible
// default ("Gardenio analysis: <target>") rather than an empty summary.
func TestCreateGBFSMTicket_DefaultSummary(t *testing.T) {
	srv, rec := newKeyedJiraRecordingServer("GBFSM-777")
	defer srv.Close()
	c := newTestJiraClient(srv.URL)

	if _, err := c.CreateGBFSMTicket(context.Background(), GBFSMTicket{
		IssueTypeID: analysisTestType, FSMTargetKey: "FSM-9", Body: "x",
	}); err != nil {
		t.Fatalf("CreateGBFSMTicket: %v", err)
	}
	fields := createdFields(t, rec)
	if got, _ := fields["summary"].(string); got != "Gardenio analysis: FSM-9" {
		t.Errorf("default summary = %q, want %q", got, "Gardenio analysis: FSM-9")
	}
}

// TestCreateGBFSMTicket_GBFSMHardcoded_MaliciousTargetCannotRedirect is the
// API-level security assertion for the create: no caller-supplied FSMTargetKey — a
// customer-project key, an FSM key, or a GBFSM-shaped key — can redirect the create
// off GBFSM. The target lands ONLY in the fsm_target_key custom field.
func TestCreateGBFSMTicket_GBFSMHardcoded_MaliciousTargetCannotRedirect(t *testing.T) {
	for _, target := range []string{"FSM-1", "PE-1", "DEVOPS-9", "GBFSM-5", "'; DROP", "10037"} {
		t.Run(target, func(t *testing.T) {
			srv, rec := newKeyedJiraRecordingServer("GBFSM-1")
			defer srv.Close()
			c := newTestJiraClient(srv.URL)

			if _, err := c.CreateGBFSMTicket(context.Background(), GBFSMTicket{
				IssueTypeID: analysisTestType, FSMTargetKey: target, Body: "analysis",
			}); err != nil {
				t.Fatalf("CreateGBFSMTicket: %v", err)
			}
			fields := createdFields(t, rec)
			if got := projectID(t, fields); got != outboxProjectID {
				t.Errorf("malicious target %q redirected the create project to %q — must stay GBFSM %q",
					target, got, outboxProjectID)
			}
			if got, _ := fields[outboxFieldTargetKey].(string); got != target {
				t.Errorf("target %q must land verbatim in %s (as data), got %q", target, outboxFieldTargetKey, got)
			}
		})
	}
}

// TestCreateGBFSMTicket_FailsClosed proves malformed GBFSM tickets send NO request:
// an empty issue-type id (the scaffold-pending default — the surface stays inert),
// an empty target, and an empty body.
func TestCreateGBFSMTicket_FailsClosed(t *testing.T) {
	cases := []struct {
		name   string
		ticket GBFSMTicket
	}{
		{"empty issue type (scaffold pending)", GBFSMTicket{IssueTypeID: "", FSMTargetKey: "FSM-1", Body: "x"}},
		{"empty issue type (const default)", GBFSMTicket{IssueTypeID: outboxTypeAnalysis, FSMTargetKey: "FSM-1", Body: "x"}},
		{"empty target", GBFSMTicket{IssueTypeID: analysisTestType, FSMTargetKey: "  ", Body: "x"}},
		{"empty body", GBFSMTicket{IssueTypeID: analysisTestType, FSMTargetKey: "FSM-1", Body: "  "}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, rec := newKeyedJiraRecordingServer("GBFSM-1")
			defer srv.Close()
			client := newTestJiraClient(srv.URL)
			key, err := client.CreateGBFSMTicket(context.Background(), c.ticket)
			if err == nil {
				t.Errorf("%s: expected an error, got nil", c.name)
			}
			if key != "" {
				t.Errorf("%s: expected empty key on failure, got %q", c.name, key)
			}
			rec.mu.Lock()
			n := len(rec.requests)
			rec.mu.Unlock()
			if n != 0 {
				t.Errorf("%s: malformed GBFSM ticket must send NO request, got %d", c.name, n)
			}
		})
	}
}

// fieldStrings pulls a []string out of a create/edit field that the recording server
// decoded as []any (JSON array of strings). Fails the test on a wrong shape.
func fieldStrings(t *testing.T, fields map[string]any, key string) []string {
	t.Helper()
	raw, ok := fields[key]
	if !ok {
		return nil
	}
	arr, ok := raw.([]any)
	if !ok {
		t.Fatalf("field %q is not a JSON array: %T (%v)", key, raw, raw)
	}
	out := make([]string, 0, len(arr))
	for _, v := range arr {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("field %q element is not a string: %T", key, v)
		}
		out = append(out, s)
	}
	return out
}

// ── analysisBody / helper unit tests ──

// TestAnalysisBody_AssemblesSections proves the body is composed from the signals on
// the event: sev/gxp from the triage labels, the recommended version, the free-form
// triage fields when present, plus the target + source-bead footer.
func TestAnalysisBody_AssemblesSections(t *testing.T) {
	bead := &BeadEvent{
		ID:    "bd-77",
		Title: "[FSM-408] Customer escalation",
		Labels: []string{
			"source:jira", "jira:FSM-408",
			"gardenio-triaged", "gardenio-triaged-sev-2", "gardenio-triaged-gxp-med",
		},
		Fields: map[string]string{
			"recommended_affects_version": "2026.7.0",
			"triage_reasoning":            "repro on prod build only",
			"log_findings":                "NPE in FooService",
		},
	}
	body := analysisBody(bead, "FSM-408")
	for _, want := range []string{
		"FSM-408", "[FSM-408] Customer escalation",
		"Severity: 2", "GxP risk: med", "Recommended Affects-Version: 2026.7.0",
		"Triage reasoning:", "repro on prod build only",
		"Log findings:", "NPE in FooService",
		"(Source bead: bd-77)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("analysis body missing %q\n---\n%s", want, body)
		}
	}
	// Absent sections are omitted (no empty "Code findings" heading).
	if strings.Contains(body, "Code findings") {
		t.Error("analysis body must omit sections whose field is absent")
	}
}

// TestAnalysisBody_MinimalBead proves a triage bead with no classification and no
// analysis fields still yields a non-empty body carrying the target (so the create
// never fails closed on an empty body just because the agent stamped nothing).
func TestAnalysisBody_MinimalBead(t *testing.T) {
	body := analysisBody(&BeadEvent{ID: "bd-1", Fields: map[string]string{}}, "FSM-500")
	if strings.TrimSpace(body) == "" {
		t.Fatal("analysis body must never be empty")
	}
	if !strings.Contains(body, "FSM-500") {
		t.Errorf("minimal analysis body must still name the target, got: %q", body)
	}
}

// TestTriageLabelValue pins sev/gxp extraction across the canonical and abbreviated
// label forms, and returns "" when absent.
func TestTriageLabelValue(t *testing.T) {
	labels := []string{"gardenio-triaged", "gardenio-triaged-sev-3", "gxp-high"}
	if got := triageLabelValue(labels, "sev"); got != "3" {
		t.Errorf("sev = %q, want 3 (canonical form)", got)
	}
	if got := triageLabelValue(labels, "gxp"); got != "high" {
		t.Errorf("gxp = %q, want high (abbreviated form)", got)
	}
	if got := triageLabelValue([]string{"gardenio-triaged"}, "sev"); got != "" {
		t.Errorf("absent sev must yield \"\", got %q", got)
	}
}
