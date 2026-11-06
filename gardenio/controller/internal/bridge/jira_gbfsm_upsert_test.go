package bridge

// Security + behavioral tests for the GBFSM upsert — the read (SearchGBFSM) + edit
// (EditGBFSMTicket) halves of the ONE-ticket-per-FSM-ticket writer (1:1 reshape
// ka-0000002637, Phase 2 ka-0000001491).
//
// The load-bearing invariant this file pins: a crafted or fuzzy fsm_target_key can
// NEVER redirect the upsert-EDIT onto an FSM (or any non-GBFSM) key. Two independent
// guards enforce it, proven both at the SearchGBFSM unit level AND behaviorally
// through the wired handleUpdated path:
//
//  1. SearchGBFSM scopes its JQL to `project = <GBFSM id>` and REJECTS any returned
//     candidate whose project id ≠ outboxProjectID (a compromised/buggy Jira handing
//     back an FSM-project issue is dropped, never surfaced as the edit target).
//  2. The JQL uses the EXACT `=` operator (not the fuzzy `~`, which could tokenizer-miss
//     and silently return empty → a duplicate create), and every candidate's
//     fsm_target_key must still EQUAL the target verbatim (FSM-40 never matches FSM-408).
//
// Duplicate observability: when >1 GBFSM ticket matches one FSM-N, SearchGBFSM returns
// the OLDEST (JQL is ORDER BY created ASC) so re-triage converges, and warns — the
// upsert cannot self-heal accrued duplicates. Pinned by TestSearchGBFSM_Duplicates*.
//
// The edit body carries ONLY the mutable content (Description + labels + version) —
// never project / issuetype / fsm_target_key — so re-triage cannot re-point the
// 1:1 link, and the PUT path is built solely from the search-verified GBFSM key.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// gbfsmSearchServer is a recording Jira server whose /search/jql GET returns a
// configurable issues payload (so the upsert can be steered into the create branch,
// the edit branch, or the poisoned-result reject branch), records the JQL of the
// search, echoes createKey on create POSTs, and records edit PUTs.
type gbfsmSearchServer struct {
	*jiraRecordingServer
	mu       sync.Mutex
	lastJQL  string
	searches int
}

// newGBFSMSearchServer returns a server that answers /search/jql with searchJSON
// (a full `{"issues":[...]}` body) and every create with {"key": createKey}.
func newGBFSMSearchServer(searchJSON, createKey string) (*httptest.Server, *gbfsmSearchServer) {
	g := &gbfsmSearchServer{jiraRecordingServer: &jiraRecordingServer{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		g.jiraRecordingServer.mu.Lock()
		g.jiraRecordingServer.requests = append(g.jiraRecordingServer.requests,
			recordedJiraRequest{Method: r.Method, Path: r.URL.Path, Body: body})
		g.jiraRecordingServer.mu.Unlock()

		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/search/jql"):
			g.mu.Lock()
			g.lastJQL = r.URL.Query().Get("jql")
			g.searches++
			g.mu.Unlock()
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, searchJSON)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/transitions"):
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"transitions":[]}`)
		default:
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"id":"1","key":%q}`, createKey)
		}
	}))
	return srv, g
}

// gbfsmIssueJSON builds a one-issue search payload with the given key, project id,
// and fsm_target_key custom-field value — the shapes SearchGBFSM must accept/reject.
func gbfsmIssueJSON(key, projectID, targetKey string) string {
	return fmt.Sprintf(`{"issues":[{"key":%q,"fields":{"project":{"id":%q},%q:%q}}]}`,
		key, projectID, outboxFieldTargetKey, targetKey)
}

// editPuts returns every PUT recorded against a per-issue path containing pathFrag.
func editPuts(rec *jiraRecordingServer, pathFrag string) []recordedJiraRequest {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	var out []recordedJiraRequest
	for _, r := range rec.requests {
		if r.Method == http.MethodPut && strings.Contains(r.Path, pathFrag) {
			out = append(out, r)
		}
	}
	return out
}

// ── SearchGBFSM: accept / reject matrix ──

// TestSearchGBFSM_ReturnsGBFSMTicket proves an exact fsm_target_key match in the
// GBFSM project is returned (the edit target). The sandbox project is keyed "GFO"
// (id 10037), so the KEY prefix is GFO — the numeric project id is what qualifies it.
func TestSearchGBFSM_ReturnsGBFSMTicket(t *testing.T) {
	srv, _ := newGBFSMSearchServer(gbfsmIssueJSON("GFO-501", outboxProjectID, "FSM-408"), "")
	defer srv.Close()
	c := newTestJiraClient(srv.URL)

	issue, err := c.SearchGBFSM(context.Background(), "FSM-408")
	if err != nil {
		t.Fatalf("SearchGBFSM: %v", err)
	}
	if issue == nil {
		t.Fatal("expected the GBFSM ticket to be returned")
	}
	if issue.Key != "GFO-501" {
		t.Errorf("returned key = %q, want the GBFSM-project GFO-501", issue.Key)
	}
}

// TestSearchGBFSM_RejectsNonGBFSMProject is the CORE security guard: a search result
// whose project id ≠ GBFSM — even one whose fsm_target_key matches AND whose key is
// an FSM key — is REJECTED (returns nil), so it can NEVER become the edit target.
func TestSearchGBFSM_RejectsNonGBFSMProject(t *testing.T) {
	// A poisoned result: an FSM-project issue (id "10099") whose key IS an FSM key.
	srv, _ := newGBFSMSearchServer(gbfsmIssueJSON("FSM-408", "10099", "FSM-408"), "")
	defer srv.Close()
	c := newTestJiraClient(srv.URL)

	issue, err := c.SearchGBFSM(context.Background(), "FSM-408")
	if err != nil {
		t.Fatalf("SearchGBFSM: %v", err)
	}
	if issue != nil {
		t.Fatalf("a non-GBFSM search result MUST be rejected (returns nil), got key %q project-mismatch — "+
			"a crafted fsm_target_key could otherwise redirect the edit onto FSM", issue.Key)
	}
}

// TestSearchGBFSM_RejectsMissingOrUnparseableProject proves the fail-closed default:
// a result with NO project field (or an unparseable one) has no provable project id,
// so issueProjectID returns "" ≠ GBFSM and the candidate is rejected — it can never
// become the edit target.
func TestSearchGBFSM_RejectsMissingOrUnparseableProject(t *testing.T) {
	cases := []struct {
		name, searchJSON string
	}{
		{
			"no project field",
			fmt.Sprintf(`{"issues":[{"key":"GFO-1","fields":{%q:"FSM-408"}}]}`, outboxFieldTargetKey),
		},
		{
			"unparseable project (array, not object)",
			fmt.Sprintf(`{"issues":[{"key":"GFO-1","fields":{"project":[1,2],%q:"FSM-408"}}]}`, outboxFieldTargetKey),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newGBFSMSearchServer(tc.searchJSON, "")
			defer srv.Close()
			c := newTestJiraClient(srv.URL)

			issue, err := c.SearchGBFSM(context.Background(), "FSM-408")
			if err != nil {
				t.Fatalf("SearchGBFSM: %v", err)
			}
			if issue != nil {
				t.Errorf("a result with no provable GBFSM project must be rejected, got key %q", issue.Key)
			}
		})
	}
}

// TestSearchGBFSM_RejectsFuzzyNonExactMatch proves the fuzzy `~` operator cannot
// smuggle a near-match: a GBFSM ticket whose fsm_target_key is FSM-4080 is rejected
// when we searched for FSM-408.
func TestSearchGBFSM_RejectsFuzzyNonExactMatch(t *testing.T) {
	srv, _ := newGBFSMSearchServer(gbfsmIssueJSON("GFO-9", outboxProjectID, "FSM-4080"), "")
	defer srv.Close()
	c := newTestJiraClient(srv.URL)

	issue, err := c.SearchGBFSM(context.Background(), "FSM-408")
	if err != nil {
		t.Fatalf("SearchGBFSM: %v", err)
	}
	if issue != nil {
		t.Errorf("fuzzy near-match (FSM-4080 vs FSM-408) must be rejected, got key %q", issue.Key)
	}
}

// TestSearchGBFSM_NoResults returns (nil,nil) — the caller then creates a fresh
// GBFSM ticket (always safe: the create is GBFSM-hardcoded).
func TestSearchGBFSM_NoResults(t *testing.T) {
	srv, _ := newGBFSMSearchServer(`{"issues":[]}`, "")
	defer srv.Close()
	c := newTestJiraClient(srv.URL)

	issue, err := c.SearchGBFSM(context.Background(), "FSM-408")
	if err != nil {
		t.Fatalf("SearchGBFSM: %v", err)
	}
	if issue != nil {
		t.Errorf("no results must yield a nil issue, got %q", issue.Key)
	}
}

// TestSearchGBFSM_JQLScopedToGBFSM proves the JQL is scoped to the GBFSM project id,
// uses the EXACT `=` operator (never the fuzzy `~`, which could tokenizer-miss and fall
// through to a duplicate create — ka-0000003401 item 1a), and embeds the target as an
// escaped literal (defense-in-depth guard 1).
func TestSearchGBFSM_JQLScopedToGBFSM(t *testing.T) {
	srv, g := newGBFSMSearchServer(`{"issues":[]}`, "")
	defer srv.Close()
	c := newTestJiraClient(srv.URL)

	if _, err := c.SearchGBFSM(context.Background(), "FSM-408"); err != nil {
		t.Fatalf("SearchGBFSM: %v", err)
	}
	g.mu.Lock()
	jql := g.lastJQL
	g.mu.Unlock()
	if !strings.Contains(jql, "project = "+outboxProjectID) {
		t.Errorf("JQL must scope to the GBFSM project id %q, got: %q", outboxProjectID, jql)
	}
	cfNum := strings.TrimPrefix(outboxFieldTargetKey, "customfield_")
	if !strings.Contains(jql, "cf["+cfNum+"]") {
		t.Errorf("JQL must reference the fsm_target_key custom field cf[%s], got: %q", cfNum, jql)
	}
	// Exact-match operator: cf[...] = "FSM-408", NOT cf[...] ~ "FSM-408".
	if !strings.Contains(jql, "cf["+cfNum+"] = "+`"FSM-408"`) {
		t.Errorf("JQL must use the EXACT `=` operator on the fsm_target_key field, got: %q", jql)
	}
	if strings.Contains(jql, "cf["+cfNum+"] ~") {
		t.Errorf("JQL must NOT use the fuzzy `~` operator (tokenizer-miss → duplicate create risk), got: %q", jql)
	}
	if !strings.Contains(jql, `"FSM-408"`) {
		t.Errorf("JQL must embed the target as a quoted literal, got: %q", jql)
	}
}

// gbfsmMultiIssueJSON builds a multi-issue search payload — all in the GBFSM project,
// all with the given fsm_target_key — to exercise the duplicate (>1 match) path.
func gbfsmMultiIssueJSON(targetKey string, keys ...string) string {
	var issues []string
	for _, k := range keys {
		issues = append(issues, fmt.Sprintf(`{"key":%q,"fields":{"project":{"id":%q},%q:%q}}`,
			k, outboxProjectID, outboxFieldTargetKey, targetKey))
	}
	return `{"issues":[` + strings.Join(issues, ",") + `]}`
}

// TestSearchGBFSM_DuplicatesReturnOldest proves that when duplicate GBFSM tickets exist
// for one FSM-N (an anomaly the upsert cannot self-heal), SearchGBFSM returns the OLDEST
// — the first result, since the JQL is ORDER BY created ASC — so re-triage keeps editing
// one canonical ticket instead of oscillating (ka-0000003401 item 2, observability).
func TestSearchGBFSM_DuplicatesReturnOldest(t *testing.T) {
	// Jira returns them created-ASC; GFO-101 is the oldest.
	srv, _ := newGBFSMSearchServer(gbfsmMultiIssueJSON("FSM-408", "GFO-101", "GFO-102", "GFO-103"), "")
	defer srv.Close()
	c := newTestJiraClient(srv.URL)

	issue, err := c.SearchGBFSM(context.Background(), "FSM-408")
	if err != nil {
		t.Fatalf("SearchGBFSM: %v", err)
	}
	if issue == nil {
		t.Fatal("expected a GBFSM ticket to be returned despite duplicates")
	}
	if issue.Key != "GFO-101" {
		t.Errorf("with duplicates SearchGBFSM must return the OLDEST (created-ASC first) GFO-101, got %q", issue.Key)
	}
}

// TestSearchGBFSM_DuplicatesWarn proves SearchGBFSM emits a WARN when >1 GBFSM ticket
// matches one FSM-N so the at-most-one-per-FSM-N arming-checklist invariant is observable
// in the bridge logs (ka-0000003401 item 2). The single-match and no-match paths must NOT
// warn. GBFSM-side quality only — no isolation invariant is affected.
func TestSearchGBFSM_DuplicatesWarn(t *testing.T) {
	cases := []struct {
		name       string
		searchJSON string
		wantWarn   bool
	}{
		{"two duplicates warn", gbfsmMultiIssueJSON("FSM-408", "GFO-101", "GFO-102"), true},
		{"single match no warn", gbfsmMultiIssueJSON("FSM-408", "GFO-101"), false},
		{"no match no warn", `{"issues":[]}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newGBFSMSearchServer(tc.searchJSON, "")
			defer srv.Close()

			var buf bytes.Buffer
			c := NewJiraClient(JiraClientConfig{
				BaseURL: srv.URL, Email: "t@e", APIToken: "tok",
				Logger: slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
			})

			if _, err := c.SearchGBFSM(context.Background(), "FSM-408"); err != nil {
				t.Fatalf("SearchGBFSM: %v", err)
			}
			logged := buf.String()
			gotWarn := strings.Contains(logged, "multiple GBFSM tickets match")
			if gotWarn != tc.wantWarn {
				t.Errorf("duplicate warn = %v, want %v (logs: %q)", gotWarn, tc.wantWarn, logged)
			}
			if tc.wantWarn {
				if !strings.Contains(logged, `"match_count":2`) {
					t.Errorf("warn must record match_count=2, logs: %q", logged)
				}
				if !strings.Contains(logged, "GFO-101") {
					t.Errorf("warn must name the chosen (oldest) key GFO-101, logs: %q", logged)
				}
			}
		})
	}
}

// TestSearchGBFSM_NilLoggerNoPanicOnDuplicates ensures the duplicate-warn path is nil-
// logger safe (newTestJiraClient wires a logger, but a caller may not) — a warn guarded
// on c.logger != nil must never panic when duplicates are present.
func TestSearchGBFSM_NilLoggerNoPanicOnDuplicates(t *testing.T) {
	srv, _ := newGBFSMSearchServer(gbfsmMultiIssueJSON("FSM-408", "GFO-101", "GFO-102"), "")
	defer srv.Close()
	c := NewJiraClient(JiraClientConfig{BaseURL: srv.URL, Email: "t@e", APIToken: "tok"}) // no Logger

	issue, err := c.SearchGBFSM(context.Background(), "FSM-408")
	if err != nil {
		t.Fatalf("SearchGBFSM: %v", err)
	}
	if issue == nil || issue.Key != "GFO-101" {
		t.Errorf("nil-logger duplicate path must still return the oldest GFO-101, got %v", issue)
	}
}

// TestSearchGBFSM_FailsClosedOnEmptyTarget sends NO request on an empty target.
func TestSearchGBFSM_FailsClosedOnEmptyTarget(t *testing.T) {
	srv, g := newGBFSMSearchServer(`{"issues":[]}`, "")
	defer srv.Close()
	c := newTestJiraClient(srv.URL)

	if _, err := c.SearchGBFSM(context.Background(), "  "); err == nil {
		t.Error("expected an error for an empty fsm_target_key")
	}
	g.mu.Lock()
	n := g.searches
	g.mu.Unlock()
	if n != 0 {
		t.Errorf("empty target must send NO search request, got %d", n)
	}
}

// ── EditGBFSMTicket: wire format + guards ──

// TestEditGBFSMTicket_WireFormat proves the edit is a PUT to /rest/api/3/issue/<key>
// carrying ONLY the mutable content (Description + labels + version) — never
// project / issuetype / fsm_target_key / idempotency_key (immutable link + key).
func TestEditGBFSMTicket_WireFormat(t *testing.T) {
	srv, rec := newJiraRecordingServer()
	defer srv.Close()
	c := newTestJiraClient(srv.URL)

	err := c.EditGBFSMTicket(context.Background(), "GFO-501", GBFSMTicket{
		FSMTargetKey:   "FSM-408", // present on the value but MUST NOT reach the edit body
		Body:           "updated analysis",
		Labels:         []string{"gardenio-triaged", "gardenio-triaged-sev-1"},
		LabelsFieldID:  labelsTestField,
		Version:        "2026.8.0",
		VersionFieldID: versionTestField,
	})
	if err != nil {
		t.Fatalf("EditGBFSMTicket: %v", err)
	}
	puts := editPuts(rec, "/issue/GFO-501")
	if len(puts) != 1 {
		t.Fatalf("expected exactly 1 PUT to /issue/GFO-501, got %d", len(puts))
	}
	fields, ok := puts[0].Body["fields"].(map[string]any)
	if !ok {
		t.Fatalf("edit body has no fields object: %+v", puts[0].Body)
	}
	if _, hasDesc := fields["description"]; !hasDesc {
		t.Error("edit must set the Description")
	}
	if got := fieldStrings(t, fields, labelsTestField); len(got) != 2 {
		t.Errorf("edit labels field = %v, want the 2 triage labels", got)
	}
	if got, _ := fields[versionTestField].(string); got != "2026.8.0" {
		t.Errorf("edit version field = %q, want 2026.8.0", got)
	}
	// Immutable fields must be ABSENT from the edit body.
	for _, forbidden := range []string{"project", "issuetype", outboxFieldTargetKey, outboxFieldIdempotencyKey} {
		if _, has := fields[forbidden]; has {
			t.Errorf("edit body must NOT carry the immutable field %q", forbidden)
		}
	}
}

// TestEditGBFSMTicket_FailsClosed sends NO request on an empty key or empty body.
func TestEditGBFSMTicket_FailsClosed(t *testing.T) {
	cases := []struct {
		name, key, body string
	}{
		{"empty key", "  ", "x"},
		{"empty body", "GFO-1", "  "},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, rec := newJiraRecordingServer()
			defer srv.Close()
			client := newTestJiraClient(srv.URL)
			if err := client.EditGBFSMTicket(context.Background(), c.key, GBFSMTicket{Body: c.body}); err == nil {
				t.Errorf("%s: expected an error", c.name)
			}
			rec.mu.Lock()
			n := len(rec.requests)
			rec.mu.Unlock()
			if n != 0 {
				t.Errorf("%s: must send NO request, got %d", c.name, n)
			}
		})
	}
}

// ── Source guard: SearchGBFSM verifies the project id (the reject guard has teeth) ──

// TestSearchGBFSM_SourceVerifiesProject parses jira_outbox.go and asserts SearchGBFSM
// references BOTH issueProjectID and outboxProjectID in its body — i.e. it actually
// performs the project-id verification. A future edit that drops the check (making
// the reject guard vacuous) trips this test. Fails closed on a parse error.
func TestSearchGBFSM_SourceVerifiesProject(t *testing.T) {
	const src = "jira_outbox.go"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, src, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", src, err)
	}
	var found, refsIssueProjectID, refsProjectID bool
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Name.Name != "SearchGBFSM" || fd.Body == nil {
			continue
		}
		found = true
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			switch id := n.(type) {
			case *ast.Ident:
				if id.Name == "issueProjectID" {
					refsIssueProjectID = true
				}
				if id.Name == "outboxProjectID" {
					refsProjectID = true
				}
			}
			return true
		})
	}
	if !found {
		t.Fatal("SearchGBFSM not found in jira_outbox.go — the guard has gone blind")
	}
	if !refsIssueProjectID || !refsProjectID {
		t.Errorf("SearchGBFSM must verify the result's project id (refs issueProjectID=%v, outboxProjectID=%v) — "+
			"without it a non-GBFSM result could become the edit target (ka-0000001491)",
			refsIssueProjectID, refsProjectID)
	}
}
