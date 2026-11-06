package bridge

// Unit + guard tests for the GBFSM outbox intent writer (epic ka-0000002067).
//
// Two things are proven here:
//
//  1. WIRE FORMAT — CreateOutboxIntent POSTs to the issue-collection endpoint
//     with the GBFSM project hardcoded, the correct intent work-type id, the FSM
//     target key in customfield_10148, the op value in customfield_10149, and the
//     idempotency key in customfield_10150 (Add-Label / Set-Version); an
//     Internal-Comment carries its body as the Description and no op_value.
//
//  2. GBFSM-ONLY / NO-FSM-TARGET — the create project is hardcoded to GBFSM and a
//     caller-supplied FSMTargetKey (even a customer-project or FSM key) can NEVER
//     redirect the create off GBFSM: the target rides only in the custom field.
//     This is the API-level guarantee that the bridge cannot create a customer
//     ticket. Proven both behaviorally (a malicious target still lands in GBFSM)
//     and by a source scan of jira_outbox.go.

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// outboxCreatePath is the issue-collection endpoint every intent create hits.
const outboxCreatePath = "/rest/api/3/issue"

// outboxCreates returns the fields object of every intent-create POST recorded
// against the outbox endpoint (in order). Shared by the triage sync-back and
// wiring suites to assert which GBFSM intents actually flowed.
func outboxCreates(rec *jiraRecordingServer) []map[string]any {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	var out []map[string]any
	for _, r := range rec.requests {
		if r.Method == http.MethodPost && r.Path == outboxCreatePath {
			if f, ok := r.Body["fields"].(map[string]any); ok {
				out = append(out, f)
			}
		}
	}
	return out
}

// outboxCreatesByType counts intent creates of a given work-type id (Add-Label
// 10040 / Set-Version 10041 / Internal-Comment 10042).
func outboxCreatesByType(rec *jiraRecordingServer, typeID string) int {
	n := 0
	for _, f := range outboxCreates(rec) {
		if it, ok := f["issuetype"].(map[string]any); ok {
			if id, _ := it["id"].(string); id == typeID {
				n++
			}
		}
	}
	return n
}

// outboxTargetKeys returns the fsm_target_key of every intent create (to assert
// intents reference the bound ticket as DATA and nothing leaks off it).
func outboxTargetKeys(rec *jiraRecordingServer) []string {
	var out []string
	for _, f := range outboxCreates(rec) {
		if k, ok := f[outboxFieldTargetKey].(string); ok {
			out = append(out, k)
		}
	}
	return out
}

// createdFields returns the fields object of the (single) create POST recorded
// against the outbox endpoint, failing the test if there is not exactly one.
func createdFields(t *testing.T, rec *jiraRecordingServer) map[string]any {
	t.Helper()
	var creates []recordedJiraRequest
	rec.mu.Lock()
	for _, r := range rec.requests {
		if r.Method == http.MethodPost && r.Path == outboxCreatePath {
			creates = append(creates, r)
		}
	}
	rec.mu.Unlock()
	if len(creates) != 1 {
		t.Fatalf("expected exactly 1 create POST to %s, got %d", outboxCreatePath, len(creates))
	}
	fields, ok := creates[0].Body["fields"].(map[string]any)
	if !ok {
		t.Fatalf("create body has no fields object: %+v", creates[0].Body)
	}
	return fields
}

// projectID digs fields.project.id out of a create body.
func projectID(t *testing.T, fields map[string]any) string {
	t.Helper()
	proj, ok := fields["project"].(map[string]any)
	if !ok {
		t.Fatalf("create fields has no project object: %+v", fields)
	}
	id, _ := proj["id"].(string)
	return id
}

func issueTypeID(t *testing.T, fields map[string]any) string {
	t.Helper()
	it, ok := fields["issuetype"].(map[string]any)
	if !ok {
		t.Fatalf("create fields has no issuetype object: %+v", fields)
	}
	id, _ := it["id"].(string)
	return id
}

// TestCreateOutboxIntent_AddLabel_WireFormat pins the Add-Label intent shape:
// GBFSM project (10037), work type 10040, target/op/idempotency in the three
// custom fields, no Description.
func TestCreateOutboxIntent_AddLabel_WireFormat(t *testing.T) {
	srv, rec := newJiraRecordingServer()
	defer srv.Close()
	c := newTestJiraClient(srv.URL)

	err := c.CreateOutboxIntent(context.Background(), OutboxIntent{
		Type:           IntentAddLabel,
		FSMTargetKey:   "FSM-408",
		OpValue:        "gardenio-triaged-sev-3",
		IdempotencyKey: outboxIdempotencyKey("bd-x", string(IntentAddLabel), "gardenio-triaged-sev-3"),
	})
	if err != nil {
		t.Fatalf("CreateOutboxIntent: %v", err)
	}

	fields := createdFields(t, rec)
	if got := projectID(t, fields); got != outboxProjectID {
		t.Errorf("create project id = %q, want GBFSM %q", got, outboxProjectID)
	}
	if got := issueTypeID(t, fields); got != outboxTypeAddLabel {
		t.Errorf("issuetype id = %q, want Add-Label %q", got, outboxTypeAddLabel)
	}
	if got, _ := fields[outboxFieldTargetKey].(string); got != "FSM-408" {
		t.Errorf("fsm_target_key (%s) = %q, want FSM-408", outboxFieldTargetKey, got)
	}
	if got, _ := fields[outboxFieldOpValue].(string); got != "gardenio-triaged-sev-3" {
		t.Errorf("op_value (%s) = %q, want the label", outboxFieldOpValue, got)
	}
	if got, _ := fields[outboxFieldIdempotencyKey].(string); got != "bd-x:add-label:gardenio-triaged-sev-3" {
		t.Errorf("idempotency_key (%s) = %q", outboxFieldIdempotencyKey, got)
	}
	if _, hasDesc := fields["description"]; hasDesc {
		t.Error("Add-Label intent must NOT carry a Description")
	}
}

// TestCreateOutboxIntent_SetVersion_WireFormat pins the Set-Version intent shape.
func TestCreateOutboxIntent_SetVersion_WireFormat(t *testing.T) {
	srv, rec := newJiraRecordingServer()
	defer srv.Close()
	c := newTestJiraClient(srv.URL)

	err := c.CreateOutboxIntent(context.Background(), OutboxIntent{
		Type:           IntentSetVersion,
		FSMTargetKey:   "FSM-700",
		OpValue:        "2026.7.0",
		IdempotencyKey: outboxIdempotencyKey("bd-y", string(IntentSetVersion), "2026.7.0"),
	})
	if err != nil {
		t.Fatalf("CreateOutboxIntent: %v", err)
	}
	fields := createdFields(t, rec)
	if got := projectID(t, fields); got != outboxProjectID {
		t.Errorf("create project id = %q, want GBFSM %q", got, outboxProjectID)
	}
	if got := issueTypeID(t, fields); got != outboxTypeSetVersion {
		t.Errorf("issuetype id = %q, want Set-Version %q", got, outboxTypeSetVersion)
	}
	if got, _ := fields[outboxFieldOpValue].(string); got != "2026.7.0" {
		t.Errorf("op_value = %q, want 2026.7.0", got)
	}
}

// TestCreateOutboxIntent_InternalComment_WireFormat pins the Internal-Comment
// intent shape: work type 10042, body in the Description (ADF), NO op_value.
func TestCreateOutboxIntent_InternalComment_WireFormat(t *testing.T) {
	srv, rec := newJiraRecordingServer()
	defer srv.Close()
	c := newTestJiraClient(srv.URL)

	err := c.CreateOutboxIntent(context.Background(), OutboxIntent{
		Type:           IntentInternalComment,
		FSMTargetKey:   "FSM-900",
		Body:           "internal status note",
		IdempotencyKey: outboxIdempotencyKey("bd-z", string(IntentInternalComment), ""),
	})
	if err != nil {
		t.Fatalf("CreateOutboxIntent: %v", err)
	}
	fields := createdFields(t, rec)
	if got := issueTypeID(t, fields); got != outboxTypeInternalComment {
		t.Errorf("issuetype id = %q, want Internal-Comment %q", got, outboxTypeInternalComment)
	}
	if _, hasDesc := fields["description"]; !hasDesc {
		t.Error("Internal-Comment intent must carry the body as a Description")
	}
	if _, hasOp := fields[outboxFieldOpValue]; hasOp {
		t.Error("Internal-Comment intent must NOT carry an op_value")
	}
	// idempotency_key for a comment intent is exactly bead-id + op (no value).
	if got, _ := fields[outboxFieldIdempotencyKey].(string); got != "bd-z:internal-comment" {
		t.Errorf("comment idempotency_key = %q, want bd-z:internal-comment", got)
	}
}

// TestCreateOutboxIntent_GBFSMHardcoded_MaliciousTargetCannotRedirect is the
// API-level security assertion: no matter what FSMTargetKey the caller supplies
// — a customer project key, an FSM key, or even a GBFSM-shaped key — the create
// project stays hardcoded GBFSM and the supplied key lands ONLY in the
// fsm_target_key custom field. A compromised or buggy caller therefore cannot
// make the bridge create (or write) a ticket in FSM or any customer project.
func TestCreateOutboxIntent_GBFSMHardcoded_MaliciousTargetCannotRedirect(t *testing.T) {
	for _, target := range []string{"FSM-1", "PE-1", "DEVOPS-9", "GBFSM-5", "'; DROP", "10037"} {
		t.Run(target, func(t *testing.T) {
			srv, rec := newJiraRecordingServer()
			defer srv.Close()
			c := newTestJiraClient(srv.URL)

			if err := c.CreateOutboxIntent(context.Background(), OutboxIntent{
				Type: IntentAddLabel, FSMTargetKey: target, OpValue: "gardenio-triaged",
			}); err != nil {
				t.Fatalf("CreateOutboxIntent: %v", err)
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

// TestCreateOutboxIntent_FailsClosed proves malformed intents send NO request:
// an unknown type, an empty target, and an empty op_value on Add-Label/Set-Version.
func TestCreateOutboxIntent_FailsClosed(t *testing.T) {
	cases := []struct {
		name   string
		intent OutboxIntent
	}{
		{"unknown type", OutboxIntent{Type: "bogus", FSMTargetKey: "FSM-1", OpValue: "x"}},
		{"empty target", OutboxIntent{Type: IntentAddLabel, FSMTargetKey: "  ", OpValue: "x"}},
		{"add-label empty op", OutboxIntent{Type: IntentAddLabel, FSMTargetKey: "FSM-1", OpValue: ""}},
		{"set-version empty op", OutboxIntent{Type: IntentSetVersion, FSMTargetKey: "FSM-1", OpValue: ""}},
		{"comment empty body", OutboxIntent{Type: IntentInternalComment, FSMTargetKey: "FSM-1", Body: ""}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, rec := newJiraRecordingServer()
			defer srv.Close()
			client := newTestJiraClient(srv.URL)
			if err := client.CreateOutboxIntent(context.Background(), c.intent); err == nil {
				t.Errorf("%s: expected an error, got nil", c.name)
			}
			rec.mu.Lock()
			n := len(rec.requests)
			rec.mu.Unlock()
			if n != 0 {
				t.Errorf("%s: malformed intent must send NO request, got %d", c.name, n)
			}
		})
	}
}

// TestOutboxIdempotencyKey pins the key shape: bead-id + op, plus the value when
// present (so distinct labels don't collide but an identical retry maps equal).
func TestOutboxIdempotencyKey(t *testing.T) {
	cases := []struct {
		bead, op, value, want string
	}{
		{"bd-1", "add-label", "sev-3", "bd-1:add-label:sev-3"},
		{"bd-1", "set-version", "2026.7.0", "bd-1:set-version:2026.7.0"},
		{"bd-2", "internal-comment", "", "bd-2:internal-comment"},
		{" bd-3 ", "add-label", " lbl ", "bd-3:add-label:lbl"},
	}
	for _, c := range cases {
		if got := outboxIdempotencyKey(c.bead, c.op, c.value); got != c.want {
			t.Errorf("outboxIdempotencyKey(%q,%q,%q) = %q, want %q", c.bead, c.op, c.value, got, c.want)
		}
	}
	// Two distinct labels from one bead+op must NOT collide.
	a := outboxIdempotencyKey("bd", "add-label", "sev-1")
	b := outboxIdempotencyKey("bd", "add-label", "gxp-med")
	if a == b {
		t.Error("distinct labels must produce distinct idempotency keys")
	}
}

// TestJqlQuote_EscapesAdversarialPayloads is the adversarial regression guard on the
// jqlQuote helper (jira_outbox.go). fsm_target_key is untrusted DATA (epic
// ka-0000002067): even though it is a bead-derived FSM key in practice, the search JQL
// treats it as attacker-controlled and MUST escape backslash and double-quote so a
// crafted value cannot break out of the quoted literal (JQL injection). This pins the
// `"` → `\"` and `\` → `\\` escapes and that the result is a single well-formed JQL
// string literal. Escaping is already load-bearing-independently guarded (the project-id
// scope + exact-match re-verification), so this is cheap defense-in-depth on the helper.
func TestJqlQuote_EscapesAdversarialPayloads(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"plain fsm key", "FSM-408", `"FSM-408"`},
		{"double quote", `a"b`, `"a\"b"`},
		{"backslash", `a\b`, `"a\\b"`},
		{"quote injection break-out attempt", `x" OR project = "10099`, `"x\" OR project = \"10099"`},
		{"trailing backslash (no escape-the-close-quote)", `abc\`, `"abc\\"`},
		{"backslash-quote pair", `\"`, `"\\\""`},
		{"empty", "", `""`},
		{"jql-y payload", `" ORDER BY created DESC--`, `"\" ORDER BY created DESC--"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := jqlQuote(c.in); got != c.want {
				t.Errorf("jqlQuote(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}

	// Structural invariant: for ANY input, the result opens and closes with an
	// UNescaped double-quote and every interior quote/backslash is escaped, so the
	// value can never terminate the literal early. We verify by asserting the interior
	// (result minus the outer quotes) contains no unescaped `"`.
	for _, payload := range []string{`"`, `\`, `""`, `\\`, `a"b\c"`, "FSM-\"408"} {
		got := jqlQuote(payload)
		if len(got) < 2 || got[0] != '"' || got[len(got)-1] != '"' {
			t.Fatalf("jqlQuote(%q) = %q is not wrapped in double quotes", payload, got)
		}
		interior := got[1 : len(got)-1]
		// Walk the interior; a `"` must always be immediately preceded by an escaping `\`.
		for i := 0; i < len(interior); i++ {
			switch interior[i] {
			case '\\':
				i++ // skip the escaped char (\\ or \")
			case '"':
				t.Errorf("jqlQuote(%q) = %q has an UNESCAPED interior quote at %d — JQL break-out risk", payload, got, i)
			}
		}
	}
}

// ── Source scan: the create is GBFSM-hardcoded and never targets FSM ──

// TestOutbox_CreateIsGBFSMHardcoded parses jira_outbox.go and asserts that the one
// create-collection POST hardcodes the GBFSM project id and that no FSM project
// literal appears anywhere in the file as a write/create target. This catches a
// future edit that would interpolate a caller key into the project or hardcode
// an FSM target, complementing the behavioral guarantee above. Fails closed on a
// parse error.
func TestOutbox_CreateIsGBFSMHardcoded(t *testing.T) {
	const src = "jira_outbox.go"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, src, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", src, err)
	}

	var (
		sawCreateEndpoint bool
		sawGBFSMProjectID bool
		fsmLiterals       []string
	)
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		val, uerr := strconv.Unquote(lit.Value)
		if uerr != nil {
			return true
		}
		if val == outboxCreatePath {
			sawCreateEndpoint = true
		}
		if val == outboxProjectID {
			sawGBFSMProjectID = true
		}
		// No hardcoded FSM issue/project key may appear as a literal — FSM keys
		// are always runtime DATA (from the bead binding), never source constants.
		if strings.HasPrefix(strings.ToUpper(val), "FSM-") || strings.EqualFold(val, "FSM") {
			pos := fset.Position(lit.Pos())
			fsmLiterals = append(fsmLiterals, val+" @ "+pos.String())
		}
		return true
	})

	if !sawCreateEndpoint {
		t.Errorf("%s: expected the create-collection endpoint %q — the scan matched nothing, "+
			"the create path may have moved and this guard gone blind", src, outboxCreatePath)
	}
	if !sawGBFSMProjectID {
		t.Errorf("%s: the GBFSM project id %q must appear as a literal in the create body — "+
			"the create project must be hardcoded to GBFSM, not derived from a caller key", src, outboxProjectID)
	}
	if len(fsmLiterals) != 0 {
		t.Errorf("%s: contains hardcoded FSM key literal(s) %v — an FSM key must never be a "+
			"source constant in the outbox writer; it is always runtime data (epic ka-0000002067)", src, fsmLiterals)
	}
}

// TestOutbox_FSMNeverWritable is the policy-level companion to the no-bypass
// scan: the production write policy never makes FSM writable, in ANY outbox
// toggle state, and FSM does not appear among the writable projects. Combined
// with the GBFSM-hardcoded create, this proves no code path can construct an
// FSM-project write (epic ka-0000002067, task gd-8Iy8Ynw0Ko requirement 5).
func TestOutbox_FSMNeverWritable(t *testing.T) {
	for _, outboxOn := range []bool{false, true} {
		store := NewWritePolicyStore(DefaultWritePolicy(outboxOn), nil)
		for _, key := range []string{"FSM-1", "FSM-408", "fsm", "FSM", "FSM-999"} {
			if store.AllowProject(key) {
				t.Errorf("outbox=%v: FSM key %q must NEVER be writable", outboxOn, key)
			}
			if store.AllowField(key, "fsm-triage", FieldLabels) {
				t.Errorf("outbox=%v: no field write may be permitted on FSM key %q", outboxOn, key)
			}
		}
		for _, p := range store.Projects() {
			if strings.EqualFold(p, "FSM") {
				t.Errorf("outbox=%v: FSM must not appear among writable projects %v", outboxOn, store.Projects())
			}
		}
	}
}
