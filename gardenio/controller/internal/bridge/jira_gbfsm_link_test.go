package bridge

// Tests for the GBFSM→FSM clickable-link half of the 1:1 bidirectional link
// (Phase 3 ka-0000001070). The GBFSM ticket carries a one-click hyperlink to its FSM
// ticket in its Description (an ADF "link" mark on the FSM key text), built from
// JiraClient.BrowseURL(fsm_target_key).
//
// The load-bearing invariant this file pins: the link is DISPLAY metadata inside the
// GBFSM ticket's OWN Description — rendering a hyperlink that POINTS at FSM is a pure
// GBFSM write and NEVER a request against the FSM ticket, so the ZERO-FSM-write
// isolation (epic ka-0000002067) is untouched. The wired tests assert not one request
// ever targets the FSM key even though its browse URL rides in the Description.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// adfLink is a decoded (text, href) pair from a "link"-marked ADF text node.
type adfLink struct{ Text, Href string }

// adfLinks walks an ADF document (as produced by mutationFields / adfParagraph) and
// returns every text node carrying a "link" mark, in order. It re-marshals the doc so
// it works on both the raw map[string]any built in-process and the map decoded back by
// the recording Jira server.
func adfLinks(doc any) []adfLink {
	raw, _ := json.Marshal(doc)
	var parsed struct {
		Content []struct {
			Content []struct {
				Type  string `json:"type"`
				Text  string `json:"text"`
				Marks []struct {
					Type  string `json:"type"`
					Attrs struct {
						Href string `json:"href"`
					} `json:"attrs"`
				} `json:"marks"`
			} `json:"content"`
		} `json:"content"`
	}
	_ = json.Unmarshal(raw, &parsed)
	var out []adfLink
	for _, para := range parsed.Content {
		for _, node := range para.Content {
			for _, m := range node.Marks {
				if m.Type == "link" {
					out = append(out, adfLink{Text: node.Text, Href: m.Attrs.Href})
				}
			}
		}
	}
	return out
}

// ── BrowseURL unit ──

// TestBrowseURL builds the human-facing browse URL and fails safe on empties.
func TestBrowseURL(t *testing.T) {
	c := newTestJiraClient("https://acme.atlassian.net/")
	if got := c.BrowseURL("FSM-408"); got != "https://acme.atlassian.net/browse/FSM-408" {
		t.Errorf("BrowseURL = %q, want the /browse/ URL (base trailing slash trimmed)", got)
	}
	// Empty key ⇒ empty (no link rendered).
	if got := c.BrowseURL("  "); got != "" {
		t.Errorf("BrowseURL(empty key) = %q, want \"\"", got)
	}
	// A client with no base URL configured ⇒ empty (writer still works, no link).
	if got := newTestJiraClient("").BrowseURL("FSM-1"); got != "" {
		t.Errorf("BrowseURL(no base URL) = %q, want \"\"", got)
	}
	// A nil client is safe (defensive).
	var nilC *JiraClient
	if got := nilC.BrowseURL("FSM-1"); got != "" {
		t.Errorf("nil client BrowseURL = %q, want \"\"", got)
	}
}

// TestBrowseURL_EscapesUntrustedKey proves the FSM target key — treated as untrusted
// DATA (epic ka-0000002067) — is path-escaped so it cannot break out of the URL path.
func TestBrowseURL_EscapesUntrustedKey(t *testing.T) {
	c := newTestJiraClient("https://acme.atlassian.net")
	got := c.BrowseURL("FSM-1/../../secret?x=1")
	if strings.Contains(got, "?") || strings.Contains(got, "/../") {
		t.Errorf("BrowseURL did not escape the untrusted key: %q", got)
	}
	if !strings.HasPrefix(got, "https://acme.atlassian.net/browse/") {
		t.Errorf("BrowseURL lost its /browse/ prefix: %q", got)
	}
}

// ── descriptionADF: link present / absent ──

// TestDescriptionADF_RendersClickableFSMLink proves that when FSMTargetURL is set the
// Description carries a clickable "link"-marked node whose text is the FSM key and
// whose href is the FSM browse URL — AND the analysis body still renders.
func TestDescriptionADF_RendersClickableFSMLink(t *testing.T) {
	ticket := GBFSMTicket{
		FSMTargetKey: "FSM-408",
		FSMTargetURL: "https://acme.atlassian.net/browse/FSM-408",
		Body:         "Severity: 2\nGxP risk: med",
	}
	doc := ticket.descriptionADF()

	links := adfLinks(doc)
	if len(links) != 1 {
		t.Fatalf("expected exactly 1 clickable link in the Description, got %d (%+v)", len(links), links)
	}
	if links[0].Text != "FSM-408" {
		t.Errorf("link text = %q, want the FSM key FSM-408", links[0].Text)
	}
	if links[0].Href != "https://acme.atlassian.net/browse/FSM-408" {
		t.Errorf("link href = %q, want the FSM browse URL", links[0].Href)
	}
	// The analysis body must still be present alongside the link.
	if full := adfText(doc); !strings.Contains(full, "Severity: 2") || !strings.Contains(full, "GxP risk: med") {
		t.Errorf("Description dropped the analysis body: %q", full)
	}
}

// TestDescriptionADF_NoURL_PlainBody proves the graceful fallback: with FSMTargetURL
// empty (a client with no base URL configured), the Description carries NO link mark
// and renders the plain body — the writer still works, unlinked.
func TestDescriptionADF_NoURL_PlainBody(t *testing.T) {
	ticket := GBFSMTicket{FSMTargetKey: "FSM-408", Body: "analysis text"}
	doc := ticket.descriptionADF()

	if links := adfLinks(doc); len(links) != 0 {
		t.Errorf("no FSMTargetURL ⇒ expected 0 links, got %d (%+v)", len(links), links)
	}
	if full := adfText(doc); !strings.Contains(full, "analysis text") {
		t.Errorf("plain Description dropped the body: %q", full)
	}
}

// TestDescriptionADF_NoKey_PlainBody proves the link is dropped when the target key is
// empty even if a URL somehow slipped in — both must be present to render a link.
func TestDescriptionADF_NoKey_PlainBody(t *testing.T) {
	ticket := GBFSMTicket{FSMTargetKey: "  ", FSMTargetURL: "https://x/browse/Y", Body: "b"}
	if links := adfLinks(ticket.descriptionADF()); len(links) != 0 {
		t.Errorf("empty target key ⇒ expected 0 links, got %d", len(links))
	}
}

// ── create / edit wire format carries the link ──

// TestCreateGBFSMTicket_DescriptionCarriesLink proves the CREATE wire body's
// Description carries the clickable FSM link (the create is still GBFSM-hardcoded).
func TestCreateGBFSMTicket_DescriptionCarriesLink(t *testing.T) {
	srv, rec := newKeyedJiraRecordingServer("GBFSM-501")
	defer srv.Close()
	c := newTestJiraClient(srv.URL)

	if _, err := c.CreateGBFSMTicket(context.Background(), GBFSMTicket{
		IssueTypeID:  analysisTestType,
		FSMTargetKey: "FSM-408",
		FSMTargetURL: c.BrowseURL("FSM-408"),
		Body:         "analysis",
	}); err != nil {
		t.Fatalf("CreateGBFSMTicket: %v", err)
	}
	fields := createdFields(t, rec)
	links := adfLinks(fields["description"])
	if len(links) != 1 || links[0].Text != "FSM-408" {
		t.Fatalf("create Description must carry the FSM-408 link, got %+v", links)
	}
	if !strings.HasSuffix(links[0].Href, "/browse/FSM-408") {
		t.Errorf("create link href = %q, want the FSM browse URL", links[0].Href)
	}
	// The create is STILL GBFSM-hardcoded — the link in the Description does not move it.
	if got := projectID(t, fields); got != outboxProjectID {
		t.Errorf("create project = %q, want GBFSM — the Description link must not redirect it", got)
	}
}

// TestEditGBFSMTicket_DescriptionCarriesLink proves the upsert-EDIT body also renders
// the clickable link (a re-triage keeps the GBFSM→FSM link), and the edit still
// targets the GBFSM key only.
func TestEditGBFSMTicket_DescriptionCarriesLink(t *testing.T) {
	srv, rec := newJiraRecordingServer()
	defer srv.Close()
	c := newTestJiraClient(srv.URL)

	if err := c.EditGBFSMTicket(context.Background(), "GFO-501", GBFSMTicket{
		FSMTargetKey: "FSM-408",
		FSMTargetURL: c.BrowseURL("FSM-408"),
		Body:         "updated analysis",
	}); err != nil {
		t.Fatalf("EditGBFSMTicket: %v", err)
	}
	puts := editPuts(rec, "/issue/GFO-501")
	if len(puts) != 1 {
		t.Fatalf("expected 1 PUT to GFO-501, got %d", len(puts))
	}
	fields, _ := puts[0].Body["fields"].(map[string]any)
	links := adfLinks(fields["description"])
	if len(links) != 1 || links[0].Text != "FSM-408" {
		t.Fatalf("edit Description must carry the FSM-408 link, got %+v", links)
	}
	// The edit must NEVER target FSM (the link in the body does not re-point the PUT).
	if n := len(editPuts(rec, "/issue/FSM-")); n != 0 {
		t.Errorf("SECURITY: the Description link redirected the edit PUT onto an FSM key (%d)", n)
	}
}

// ── wired: the link flows through handleUpdated, still ZERO FSM write ──

// TestGBFSMUpsert_WiredLinkNeverWritesFSM is the behavioral guarantee: driving a real
// triage event through handleUpdated produces a GBFSM create whose Description carries
// the clickable FSM link, while NOT A SINGLE request ever targets the FSM ticket.
func TestGBFSMUpsert_WiredLinkNeverWritesFSM(t *testing.T) {
	srv, g := newGBFSMSearchServer(`{"issues":[]}`, "GFO-900")
	defer srv.Close()

	s := newGBFSMSync(t, srv.URL, true)
	s.handleUpdated(context.Background(), triageClaimEvent("bd-link", "FSM-700"))

	creates := outboxCreates(g.jiraRecordingServer)
	if len(creates) != 1 {
		t.Fatalf("expected exactly 1 GBFSM create, got %d", len(creates))
	}
	links := adfLinks(creates[0]["description"])
	if len(links) != 1 || links[0].Text != "FSM-700" {
		t.Fatalf("wired create Description must carry the FSM-700 link, got %+v", links)
	}
	if !strings.HasSuffix(links[0].Href, "/browse/FSM-700") {
		t.Errorf("wired link href = %q, want the FSM-700 browse URL", links[0].Href)
	}
	// The load-bearing isolation assertion: the FSM browse URL rides in the GBFSM
	// Description, but NOT ONE request ever targets the FSM ticket.
	if n := len(g.jiraRecordingServer.writesTo("/issue/FSM-700")); n != 0 {
		t.Fatalf("SECURITY: rendering the FSM link produced %d direct write(s) to the FSM ticket — "+
			"the GBFSM→FSM link must be display-only (ka-0000001070)", n)
	}
}
