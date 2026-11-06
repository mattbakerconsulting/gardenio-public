package bridge

// Round-trip e2e for the collapsed GBFSM upsert (ka-0000003401 item 1c).
//
// WHY THIS EXISTS — the gap it closes: every OTHER GBFSM test stubs /search/jql
// VERBATIM. newGBFSMSearchServer echoes a FIXED issues payload regardless of the JQL
// it is handed, so it can prove "given a search result, the upsert edits it" and
// "given none, it creates" — but it CANNOT prove the create and the subsequent read
// actually agree on the fsm_target_key field. A create that writes customfield_10148
// and a search that filters on cf[10148] = "FSM-N" could silently disagree (wrong
// field id, wrong value shape, `~` tokenizer miss) and the verbatim stub would never
// notice. This file uses a STATEFUL Jira double that (a) STORES what CreateGBFSMTicket
// POSTs and (b) answers /search/jql by actually EVALUATING the JQL's
// `cf[<num>] = "<value>"` predicate against the stored tickets' real
// customfield_10148 — a genuine round-trip against a REAL search field, not a canned
// echo. If the create's field wiring and the search's field/operator wiring ever drift
// apart, the re-triage create-vs-edit selection breaks here.
//
// It also exercises the item-1a `=` exact-operator + item-2 duplicate-convergence
// behavior end-to-end through handleUpdated: two re-triages of one FSM ticket converge
// on ONE GBFSM ticket (create then edit), and a THIRD distinct FSM ticket gets its own.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// storedGBFSMTicket is one ticket the fake project holds after a create.
type storedGBFSMTicket struct {
	key       string
	projectID string
	targetKey string // customfield_10148 value
}

// fakeGBFSMProject is a STATEFUL Jira double for the GBFSM project. Unlike
// newGBFSMSearchServer (which echoes a fixed /search/jql body), it stores every
// CreateGBFSMTicket POST and answers /search/jql by parsing the JQL's
// `cf[<num>] = "<value>"` predicate and returning the tickets whose stored
// customfield_10148 actually equals that value — a real search field, in created order.
type fakeGBFSMProject struct {
	*jiraRecordingServer
	mu      sync.Mutex
	tickets []storedGBFSMTicket
	nextID  int
	lastJQL string
}

// jqlEqRe extracts the value from a `cf[<num>] = "<value>"` clause (the exact operator
// SearchGBFSM emits after item 1a). A `~` clause deliberately does NOT match, so if the
// writer ever regresses to the fuzzy operator this double returns nothing and the
// round-trip create-vs-edit selection breaks — surfacing the regression.
var jqlEqRe = regexp.MustCompile(`cf\[\d+\]\s*=\s*"((?:[^"\\]|\\.)*)"`)

// newFakeGBFSMProject returns a stateful GBFSM project double. keyPrefix names created
// tickets (e.g. "GFO" → GFO-1000, GFO-1001, …).
func newFakeGBFSMProject(keyPrefix string) (*httptest.Server, *fakeGBFSMProject) {
	f := &fakeGBFSMProject{jiraRecordingServer: &jiraRecordingServer{}, nextID: 1000}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.jiraRecordingServer.mu.Lock()
		f.jiraRecordingServer.requests = append(f.jiraRecordingServer.requests,
			recordedJiraRequest{Method: r.Method, Path: r.URL.Path, Body: body})
		f.jiraRecordingServer.mu.Unlock()

		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/search/jql"):
			f.handleSearch(w, r)
		case r.Method == http.MethodPost && r.URL.Path == "/rest/api/3/issue":
			f.handleCreate(w, body, keyPrefix)
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/rest/api/3/issue/"):
			// Edit — nothing to mutate for the round-trip (we assert on the PUT path);
			// just 204 so EditGBFSMTicket succeeds.
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{}`)
		}
	}))
	return srv, f
}

func (f *fakeGBFSMProject) handleCreate(w http.ResponseWriter, body map[string]any, keyPrefix string) {
	fields, _ := body["fields"].(map[string]any)
	projID := ""
	if p, ok := fields["project"].(map[string]any); ok {
		projID, _ = p["id"].(string)
	}
	target, _ := fields[outboxFieldTargetKey].(string)

	f.mu.Lock()
	key := fmt.Sprintf("%s-%d", keyPrefix, f.nextID)
	f.nextID++
	f.tickets = append(f.tickets, storedGBFSMTicket{key: key, projectID: projID, targetKey: target})
	f.mu.Unlock()

	w.WriteHeader(http.StatusCreated)
	fmt.Fprintf(w, `{"id":"%s","key":%q}`, key, key)
}

func (f *fakeGBFSMProject) handleSearch(w http.ResponseWriter, r *http.Request) {
	jql := r.URL.Query().Get("jql")
	f.mu.Lock()
	f.lastJQL = jql
	// Evaluate the `cf[..] = "<value>"` predicate against the stored tickets. The
	// unescaping mirrors jqlQuote's escapes so a round-tripped quote/backslash matches.
	want := ""
	if m := jqlEqRe.FindStringSubmatch(jql); m != nil {
		want = strings.NewReplacer(`\"`, `"`, `\\`, `\`).Replace(m[1])
	}
	var matched []storedGBFSMTicket
	for _, tk := range f.tickets {
		if tk.targetKey == want {
			matched = append(matched, tk) // created order == ORDER BY created ASC
		}
	}
	f.mu.Unlock()

	var issues []string
	for _, tk := range matched {
		issues = append(issues, fmt.Sprintf(`{"key":%q,"fields":{"project":{"id":%q},%q:%q}}`,
			tk.key, tk.projectID, outboxFieldTargetKey, tk.targetKey))
	}
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"issues":[%s]}`, strings.Join(issues, ","))
}

// ticketCount returns how many tickets the fake project holds for one fsm_target_key.
func (f *fakeGBFSMProject) ticketCount(targetKey string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, tk := range f.tickets {
		if tk.targetKey == targetKey {
			n++
		}
	}
	return n
}

// reTriageEvent is a triage claim on fsmKey with an explicit severity + version so a
// SECOND re-triage of the same FSM ticket can carry a CHANGED classification. The
// syncGBFSMTicket dedup is keyed on (fsm, labels, version), NOT the bead id — so an
// IDENTICAL re-triage is (correctly) a no-op. A changed classification re-fires the
// upsert, and since the first create already stored a ticket for this FSM key, the
// second sync must round-trip the real search field and EDIT it rather than create anew.
func reTriageEvent(beadID, fsmKey, sevLabel, version string) []byte {
	return marshalSSEBeadPayload(BeadEvent{
		ID:       beadID,
		Type:     "task",
		Title:    "[" + fsmKey + "] Customer escalation",
		Status:   "in_progress",
		Assignee: "triage-agent",
		Labels: []string{
			"source:jira", "jira:" + fsmKey,
			"gardenio-triaged", sevLabel, "gxp-med",
		},
		Fields: map[string]string{
			"jira_key":                    fsmKey,
			"recommended_affects_version": version,
		},
	})
}

// TestGBFSMRoundTrip_CreateThenReTriageEdits is the core round-trip: a first triage of
// FSM-700 CREATES one GBFSM ticket (writing customfield_10148="FSM-700"); a second
// triage of the SAME FSM ticket must, via a REAL search-field filter (not a verbatim
// stub), find that ticket and EDIT it — never create a second. Proves the create's
// field wiring and the search's field/operator wiring actually agree.
func TestGBFSMRoundTrip_CreateThenReTriageEdits(t *testing.T) {
	srv, f := newFakeGBFSMProject("GFO")
	defer srv.Close()

	s := newGBFSMSync(t, srv.URL, true)

	// 1st triage: no existing ticket ⇒ exactly one create, no edit.
	s.handleUpdated(context.Background(), reTriageEvent("bd-rt-1", "FSM-700", "gardenio-triaged-sev-2", "2026.7.0"))
	if n := len(outboxCreates(f.jiraRecordingServer)); n != 1 {
		t.Fatalf("first triage must CREATE exactly one GBFSM ticket, got %d", n)
	}
	if n := len(editPuts(f.jiraRecordingServer, "/issue/")); n != 0 {
		t.Fatalf("first triage must make NO edit, got %d PUTs", n)
	}
	if c := f.ticketCount("FSM-700"); c != 1 {
		t.Fatalf("fake project must hold exactly 1 GBFSM ticket for FSM-700 after create, got %d", c)
	}
	firstKey := f.tickets[0].key

	// 2nd triage of the SAME FSM ticket with a CHANGED classification (sev-2 → sev-1),
	// so the (fsm, labels, version) dedup re-fires: the search must round-trip to the
	// stored ticket via the real search field and the upsert must EDIT it, not re-create.
	s.handleUpdated(context.Background(), reTriageEvent("bd-rt-2", "FSM-700", "gardenio-triaged-sev-1", "2026.8.0"))

	if n := len(outboxCreates(f.jiraRecordingServer)); n != 1 {
		t.Errorf("re-triage must NOT create a second GBFSM ticket (upsert), still want 1 create, got %d", n)
	}
	puts := editPuts(f.jiraRecordingServer, "/issue/"+firstKey)
	if len(puts) != 1 {
		t.Errorf("re-triage must EDIT the first-created GBFSM ticket %s exactly once, got %d PUTs", firstKey, len(puts))
	}
	// The edit must never target the FSM ticket.
	if n := len(editPuts(f.jiraRecordingServer, "/issue/FSM-")); n != 0 {
		t.Errorf("re-triage edit must NEVER target an FSM key, got %d", n)
	}
	if c := f.ticketCount("FSM-700"); c != 1 {
		t.Errorf("still exactly 1 GBFSM ticket for FSM-700 after re-triage (converged), got %d", c)
	}
}

// TestGBFSMRoundTrip_DistinctFSMKeysGetDistinctTickets proves the search field actually
// DISCRIMINATES by value: a triage of FSM-700 then FSM-800 yields TWO separate GBFSM
// tickets (each create's search finds no prior match for its own key), and a re-triage
// of FSM-700 edits ONLY the FSM-700 ticket — not FSM-800's. A verbatim stub (which
// echoes the same payload for every JQL) could not tell these apart.
func TestGBFSMRoundTrip_DistinctFSMKeysGetDistinctTickets(t *testing.T) {
	srv, f := newFakeGBFSMProject("GFO")
	defer srv.Close()

	s := newGBFSMSync(t, srv.URL, true)
	s.handleUpdated(context.Background(), reTriageEvent("bd-a", "FSM-700", "gardenio-triaged-sev-2", "2026.7.0"))
	s.handleUpdated(context.Background(), reTriageEvent("bd-b", "FSM-800", "gardenio-triaged-sev-3", "2026.7.0"))

	if n := len(outboxCreates(f.jiraRecordingServer)); n != 2 {
		t.Fatalf("two distinct FSM keys must create two GBFSM tickets, got %d", n)
	}
	if f.ticketCount("FSM-700") != 1 || f.ticketCount("FSM-800") != 1 {
		t.Fatalf("expected 1 ticket per FSM key, got FSM-700=%d FSM-800=%d",
			f.ticketCount("FSM-700"), f.ticketCount("FSM-800"))
	}
	key700 := f.tickets[0].key
	key800 := f.tickets[1].key
	if key700 == key800 {
		t.Fatalf("distinct FSM keys must get distinct GBFSM keys, both %s", key700)
	}

	// Re-triage FSM-700 with a CHANGED classification → edits ONLY key700.
	s.handleUpdated(context.Background(), reTriageEvent("bd-a2", "FSM-700", "gardenio-triaged-sev-1", "2026.8.0"))
	if n := len(editPuts(f.jiraRecordingServer, "/issue/"+key700)); n != 1 {
		t.Errorf("re-triage of FSM-700 must edit %s once, got %d", key700, n)
	}
	if n := len(editPuts(f.jiraRecordingServer, "/issue/"+key800)); n != 0 {
		t.Errorf("re-triage of FSM-700 must NOT touch FSM-800's ticket %s, got %d edits", key800, n)
	}
}

// TestGBFSMRoundTrip_SearchUsesExactOperator asserts, at the round-trip level, that the
// JQL the writer actually sent to the (real-filtering) search used `=` and not `~`
// (item 1a). If it regressed to `~`, jqlEqRe would not match and the second triage
// would create a duplicate instead of editing — but we also pin the operator directly
// so the failure names the cause.
func TestGBFSMRoundTrip_SearchUsesExactOperator(t *testing.T) {
	srv, f := newFakeGBFSMProject("GFO")
	defer srv.Close()

	s := newGBFSMSync(t, srv.URL, true)
	s.handleUpdated(context.Background(), reTriageEvent("bd-op", "FSM-700", "gardenio-triaged-sev-2", "2026.7.0"))

	f.mu.Lock()
	jql := f.lastJQL
	f.mu.Unlock()
	cfNum := strings.TrimPrefix(outboxFieldTargetKey, "customfield_")
	if !strings.Contains(jql, "cf["+cfNum+"] = ") {
		t.Errorf("round-trip search JQL must use exact `=`, got: %q", jql)
	}
	if strings.Contains(jql, "cf["+cfNum+"] ~") {
		t.Errorf("round-trip search JQL must NOT use fuzzy `~` (duplicate-create risk), got: %q", jql)
	}
}
