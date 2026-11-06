package server

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

// Reopen-attempt accounting (ka-0000002985 DEFECT 3) stamps three fields on the
// ISSUE bead an involuntarily-killed agent left in_progress. Two writers send
// them, and both put the status change, the assignee clear and the fields in ONE
// PATCH /v1/beads/{id} (beadsapi.Client.UpdateBead):
//
//   - the controller SIGKILL backstop, statusreporter/stalereaper.go
//     reopenStrandedIssueTasks (reopen) and parkCappedStrandedTask (cap park);
//   - the in-pod stop-gate, cmd/gb/hook_beadclose.go reopenOrphanedClaimedTask
//     (reopen, plus resume_s3_key when S3 is configured) and parkCappedTask.
//
// None of the three was declared on any issue-kind builtin, and issue types do
// not allow additional fields, so the daemon rejected every one of those
// PATCHes whole: "HTTP 400: invalid fields: validation failed: reopen_attempts:
// unknown field". The status and assignee changes rolled back with it, so the
// stale-reaper retried the same bead every pass and StaleReaperStats.Reopened
// could never be non-zero (gd-aJze4IIIEC).
//
// It shipped green because the controller-side tests (cmd/gb/hook_reopen_test.go,
// statusreporter's reaper tests) assert the patch against mock daemons that
// accept any field map. These tests run the SAME patch bodies through the real
// HTTP PATCH handler, so resolveTypeConfig and model.ValidateTypeFieldsForUpdate
// check them against the real builtin schemas.

// reopenAccountingSrcRelPath declares the wire names the writers above use, as
// Go constants that every call site references (so a grep for the literal
// string misses them). Reached across the module boundary by relative path, the
// same way config_driftguard_test.go reaches beadsapi/{projects,roles}.go.
const reopenAccountingSrcRelPath = "../../../gardenio/controller/internal/beadsapi/reopen.go"

// Wire names of the reopen-accounting fields, pinned here so the PATCH tests
// below run even where the controller source is not checked out.
// TestReopenAccountingFields_MatchControllerConstants keeps them in step with
// the controller's constants.
const (
	wireReopenAttempts  = "reopen_attempts"
	wireReopenCapped    = "reopen_capped"
	wireReopenCapReason = "reopen_cap_reason"
	wireReapedAssignee  = "reaped_assignee"
)

// reopenScannedIssueTypes are the builtin issue types the two writers can hit.
// Both select by kind=issue with no type filter (stalereaper
// reopenStrandedIssueTasks, beadsapi ListAssignedTasks), and every one of these
// builds its schema from issueFields, which is where the fields are declared.
var reopenScannedIssueTypes = []model.BeadType{
	model.TypeTask,
	model.TypeBug,
	model.TypeFeature,
	model.TypeChore,
	model.TypeEpic,
}

// extractReopenFieldConsts parses reopen.go and returns every
// `<Name>Field = "<value>"` string constant, keyed by constant name. It returns
// nil (after skipping the test) when the sibling module is not checked out.
func extractReopenFieldConsts(t *testing.T) map[string]string {
	t.Helper()
	abs, err := filepath.Abs(reopenAccountingSrcRelPath)
	if err != nil {
		t.Fatalf("resolve %s: %v", reopenAccountingSrcRelPath, err)
	}
	if _, statErr := os.Stat(abs); os.IsNotExist(statErr) {
		t.Skipf("gardenio sibling source %s not checked out; reopen-field guard runs where the controller module is present", reopenAccountingSrcRelPath)
	}
	file, err := parser.ParseFile(token.NewFileSet(), abs, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", reopenAccountingSrcRelPath, err)
	}
	out := map[string]string{}
	for _, decl := range file.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, name := range vs.Names {
				if !strings.HasSuffix(name.Name, "Field") || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				v, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("unquote %s: %v", name.Name, err)
				}
				out[name.Name] = v
			}
		}
	}
	return out
}

// TestReopenAccountingFields_MatchControllerConstants guards both directions of
// drift between the controller constants and the daemon schema: a renamed
// constant value fails the pin, and a NEW *Field constant added to reopen.go
// fails until it is declared on every scanned issue type.
func TestReopenAccountingFields_MatchControllerConstants(t *testing.T) {
	consts := extractReopenFieldConsts(t)
	if len(consts) == 0 {
		t.Fatalf("extracted zero *Field string constants from %s; the declaration idiom changed, update extractReopenFieldConsts", reopenAccountingSrcRelPath)
	}

	pinned := map[string]string{
		"ReopenAttemptsField":  wireReopenAttempts,
		"ReopenCappedField":    wireReopenCapped,
		"ReopenCapReasonField": wireReopenCapReason,
		"ReapedAssigneeField":  wireReapedAssignee,
	}
	for name, want := range pinned {
		if got, ok := consts[name]; !ok {
			t.Errorf("%s no longer declares %s; update this guard and the schema together", reopenAccountingSrcRelPath, name)
		} else if got != want {
			t.Errorf("beadsapi.%s = %q, want %q: the wire name changed, so the daemon schema (reopenFields in config.go) no longer declares it", name, got, want)
		}
	}

	names := make([]string, 0, len(consts))
	for name := range consts {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, bt := range reopenScannedIssueTypes {
		tc := parseBuiltinSchema(t, string(bt))
		declared := make(map[string]model.FieldType, len(tc.Fields))
		for _, f := range tc.Fields {
			declared[f.Name] = f.Type
		}
		for _, name := range names {
			wire := consts[name]
			typ, ok := declared[wire]
			if !ok {
				t.Errorf("type:%s does not declare %q (beadsapi.%s): every reopen/park PATCH carrying it 400s \"unknown field\" and the whole reopen rolls back (gd-aJze4IIIEC)", bt, wire, name)
				continue
			}
			// The writers send strconv.Itoa / "true" / free text: all JSON strings.
			if typ != model.FieldTypeString {
				t.Errorf("type:%s field %q type=%q, want %q (the writers send a JSON string)", bt, wire, typ, model.FieldTypeString)
			}
		}
	}
}

// TestReopenStrandedIssue_PatchValidatesAgainstBuiltinSchema sends the exact
// PATCH bodies the two writers produce through the real HTTP handler for every
// issue type they can hit, and checks that each one lands.
//
// Every sub-case seeds a FRESH bead that carries none of the reopen fields. The
// update path grandfathers keys already stored on the bead, so a bead that
// already had reopen_attempts would accept the patch whether or not the schema
// declares it, and the test would pass without proving anything.
func TestReopenStrandedIssue_PatchValidatesAgainstBuiltinSchema(t *testing.T) {
	const deadAgent = "gardenio-crew-deadbeef"
	capReason := "assignee agent gone (crash/OOM or hard SIGKILL, not reaped)"

	cases := []struct {
		name       string
		wantStatus model.Status
		fields     map[string]any
	}{
		{
			// stalereaper.go reopenStrandedIssueTasks: Status "open", Assignee "",
			// Fields {ReopenAttemptsField: strconv.Itoa(next), ReapedAssigneeField}.
			name:       "stalereaper reopen",
			wantStatus: model.StatusOpen,
			fields:     map[string]any{wireReopenAttempts: strconv.Itoa(1), wireReapedAssignee: deadAgent},
		},
		{
			// hook_beadclose.go reopenOrphanedClaimedTask with COOP_S3_BUCKET set:
			// the same reopen plus the resume_s3_key handoff in the same patch.
			name:       "stop-gate reopen with resume handoff",
			wantStatus: model.StatusOpen,
			fields: map[string]any{
				wireReopenAttempts: strconv.Itoa(3),
				wireReapedAssignee: deadAgent,
				"resume_s3_key":    "sessions/gd-abc123/transcript",
			},
		},
		{
			// stalereaper.go parkCappedStrandedTask and hook_beadclose.go
			// parkCappedTask: Status "blocked", Assignee "", all four fields.
			name:       "cap park",
			wantStatus: model.StatusBlocked,
			fields: map[string]any{
				wireReopenAttempts:  strconv.Itoa(11),
				wireReopenCapped:    "true",
				wireReopenCapReason: capReason,
				wireReapedAssignee:  deadAgent,
			},
		},
	}

	for _, bt := range reopenScannedIssueTypes {
		for _, tc := range cases {
			t.Run(string(bt)+"/"+tc.name, func(t *testing.T) {
				_, ms, h := newTestServer()
				const id = "gd-stranded"
				ms.beads[id] = &model.Bead{
					ID:        id,
					Title:     "stranded claimed work",
					Kind:      model.KindIssue,
					Type:      bt,
					Status:    model.StatusInProgress,
					Assignee:  deadAgent,
					Fields:    json.RawMessage(`{"operated_by":"slack:U0TEST"}`),
					CreatedBy: defaultTestSubject,
				}

				// Same shape as beadsapi.Client.UpdateBead's body.
				rec := doJSON(t, h, "PATCH", "/v1/beads/"+id, map[string]any{
					"status":   string(tc.wantStatus),
					"assignee": "",
					"fields":   tc.fields,
				})
				if rec.Code != 200 {
					t.Fatalf("PATCH rejected: status %d, body %s", rec.Code, rec.Body.String())
				}

				got := ms.beads[id]
				if got.Status != tc.wantStatus {
					t.Errorf("status = %q, want %q", got.Status, tc.wantStatus)
				}
				if got.Assignee != "" {
					t.Errorf("assignee = %q, want cleared", got.Assignee)
				}
				var stored map[string]any
				if err := json.Unmarshal(got.Fields, &stored); err != nil {
					t.Fatalf("unmarshal stored fields: %v", err)
				}
				for k, want := range tc.fields {
					if stored[k] != want {
						t.Errorf("fields[%q] = %#v, want %#v", k, stored[k], want)
					}
				}
				if stored["operated_by"] != "slack:U0TEST" {
					t.Errorf("pre-existing field operated_by lost by the merge: %#v", stored["operated_by"])
				}
			})
		}
	}
}
