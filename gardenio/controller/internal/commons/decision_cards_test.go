package commons

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/commons/commonstest"
	"gardenio/controller/internal/decisionview"
)

func TestDecisionCardFaces(t *testing.T) {
	d := commonstest.TwoLands(t0)
	h := commonsServer(t, d)

	// Open: priority, prompt and context, the options as buttons on the
	// card's action route, Other and Escalate lightboxes, Dismiss.
	_, body := get(t, h, "/test/m/cl-dcard1", "claimlayer")
	mustContain(t, body, `class="cm-dcard cm-dcard--open"`, `data-urgency="high"`, ">P1<", "Decision needed",
		`action="/base/garden/c/action/cl-dcard1"`, `name="value" value="1"`, `<span class="cm-opt-title">Ship it</span>`,
		"Merge PR #42 and cut a release", "Needs a report",
		`href="/base/garden/c/m/cl-dcard1/lightbox/other"`, `href="/base/garden/c/m/cl-dcard1/lightbox/promote"`,
		`name="action" value="dismiss"`, `<summary class="btn small ghost">Dismiss…</summary>`, `name="reason" maxlength="500"`,
		`data-arm="Tap again to dismiss">Dismiss decision</button>`)
	mustNotContain(t, body, `<p class="cm-text">Decision needed</p>`) // the card's fallback text gives way to the face

	// One primary per card (rubric C8): with none recommended, the first
	// option; the rest are ghost.
	mustContain(t, body, `<button class="btn cm-opt-btn" type="submit" name="value" value="1">`, `<button class="btn cm-opt-btn ghost" type="submit" name="value" value="2">`)
	mustNotContain(t, body, "Recommended")

	// Answered with a written answer and no option: the answer is what was chosen.
	d.Put(&beadsapi.BeadDetail{ID: "cl-dec1", Type: "decision", Status: "closed", Priority: 1, Labels: []string{"project:claimlayer"},
		Fields: map[string]string{"prompt": "Ship the chart fix?", "options": `[{"id":"ship","short":"Ship it"}]`,
			"response_text": "Ship it after the demo", "responded_by": "oidc:ann@claimlayer.example"}})
	_, body = get(t, h, "/test/m/cl-dcard1", "claimlayer")
	mustContain(t, body, `cm-dcard--answered`, "Answered: <strong>Ship it after the demo</strong> · by ann")
	mustNotContain(t, body, "cm-dcard-opts", "lightbox", `value="dismiss"`)

	// An option chosen by id reads as its title; handed back for an escalation.
	for chosen, want := range map[string]string{"ship": "Answered: <strong>Ship it</strong>", chosenReturned: "Handed back for an escalation"} {
		d.Beads["cl-dec1"].Fields["chosen"] = chosen
		_, body = get(t, h, "/test/m/cl-dcard1", "claimlayer")
		mustContain(t, body, want)
	}

	// Permission: waiting, then answered.
	_, body = get(t, h, "/test/m/cl-pcard1", "claimlayer")
	mustContain(t, body, `cm-pcard--open`, `<code class="cm-pcard-tool">Bash</code>`, "Run the chart tests",
		"make test-helm &amp;&amp; echo &#39;&lt;done&gt;&#39;", `name="action" value="allow"`, `name="action" value="deny"`)
	d.Beads["cl-perm1"].Status = "closed"
	d.Beads["cl-perm1"].Fields["behavior"], d.Beads["cl-perm1"].Fields["answered_by"], d.Beads["cl-perm1"].Fields["verdict_error"] = "allow", "@matt", "dial tcp: refused"
	_, body = get(t, h, "/test/m/cl-pcard1", "claimlayer")
	mustContain(t, body, `cm-pcard--allow`, "Allowed by matt · the answer did not reach the agent")
	mustNotContain(t, body, `value="allow"`)
}

// TestCardsSayWhomTheyWaitOn: an open decision or permission card says whom
// it waits on (rubric C9): the viewer as "you", anyone else by name, from the
// decision's operated_by, else its agent's; the owner when neither says. A
// closed card waits on nobody.
func TestCardsSayWhomTheyWaitOn(t *testing.T) {
	d := commonstest.TwoLands(t0)
	h := commonsServer(t, d)
	you := `<p class="cm-waiting cm-waiting--you">Waiting on you</p>`

	op := d.Beads["cl-agent1"].Fields["operated_by"] // the asking agent works for ann
	delete(d.Beads["cl-agent1"].Fields, "operated_by")
	_, body := get(t, h, "/test/m/cl-dcard1", "claimlayer")
	mustContain(t, body, `<p class="cm-waiting">Waiting on the owner</p>`)
	d.Beads["cl-agent1"].Fields["operated_by"] = op
	_, body = get(t, h, "/test/m/cl-dcard1", "claimlayer")
	mustContain(t, body, `<p class="cm-waiting">Waiting on ann</p>`)
	d.Beads["cl-dec1"].Fields["operated_by"] = "oidc:Bo@claimlayer.example" // the decision's own wins, matched without case
	_, body = get(t, h, "/test/m/cl-dcard1", "claimlayer")
	mustContain(t, body, you)

	_, body = get(t, h, "/test/m/cl-pcard1", "claimlayer")
	mustContain(t, body, `<p class="cm-waiting">Waiting on ann</p>`)
	d.Beads["cl-agent1"].OperatedBy = "oidc:bo@claimlayer.example"
	_, body = get(t, h, "/test/m/cl-pcard1", "claimlayer")
	mustContain(t, body, you)

	for _, card := range []string{"cl-dcard2", "cl-pcard2"} {
		_, body = get(t, h, "/test/m/"+card, "claimlayer")
		mustNotContain(t, body, "Waiting on")
	}
}

func TestDecisionOptionsPrimary(t *testing.T) {
	prim := func(raw string) (got []bool) {
		for _, o := range decisionOptions(decisionview.ParseOptions(raw)) {
			got = append(got, o.Primary)
		}
		return got
	}
	for raw, want := range map[string]string{
		`["a","b","c"]`: "100",
		`[{"id":"a"},{"id":"b","recommended":true},{"id":"c","recommended":true}]`: "010", // the first recommended
		`[{"id":"a","recommended":"yes"},{"id":"b"}]`:                              "10",  // not a bool: not recommended
	} {
		var b strings.Builder
		for _, p := range prim(raw) {
			if p {
				b.WriteByte('1')
			} else {
				b.WriteByte('0')
			}
		}
		if b.String() != want {
			t.Errorf("%s: primary %s, want %s", raw, b.String(), want)
		}
	}
	if decisionOptions(nil) != nil {
		t.Error("no options, no primary")
	}
}

func TestDecisionCardSubjectStaysInItsLand(t *testing.T) {
	d := commonstest.TwoLands(t0)
	h := commonsServer(t, d)
	// The card names a decision of another land: no face, no lightbox.
	d.Put(&beadsapi.BeadDetail{ID: "cl-dec1", Type: "decision", Status: "open", Labels: []string{"project:gardenio"},
		Fields: map[string]string{"prompt": "Gardenio secret decision"}})
	_, body := get(t, h, "/test/m/cl-dcard1", "claimlayer")
	mustContain(t, body, `href="/base/garden?bead=cl-dec1" data-bead="cl-dec1" title="cl-dec1: not found" data-titles="off">Bead not found <span class="cm-bead-id">cl-dec1</span></a>`)
	mustNotContain(t, body, "cm-dcard", "Gardenio secret decision")
	if code, body := get(t, h, "/garden/c/m/cl-dcard1/lightbox/other", "claimlayer"); code != http.StatusNotFound || strings.Contains(body, "secret") {
		t.Fatalf("lightbox of a foreign decision: %d %s", code, body)
	}
}

func TestDecisionLightbox(t *testing.T) {
	h := commonsServer(t, commonstest.TwoLands(t0))
	for path, want := range map[string]int{
		"/garden/c/m/cl-dcard1/lightbox/other":   http.StatusOK,
		"/garden/c/m/cl-dcard1/lightbox/promote": http.StatusOK,
		"/garden/c/m/cl-dcard1/lightbox/delete":  http.StatusBadRequest, // not a lightbox
		"/garden/c/m/cl-pcard1/lightbox/other":   http.StatusNotFound,   // a permission card has none
		"/garden/c/m/cl-msg1/lightbox/other":     http.StatusNotFound,   // not a card
		"/garden/c/m/gd-msg1/lightbox/other":     http.StatusNotFound,   // another land
	} {
		if code, body := get(t, h, path, "claimlayer"); code != want {
			t.Errorf("GET %s = %d, want %d: %s", path, code, want, body)
		}
	}
}

// TestCardActionsAreOffWithoutABridge: with no way to the bridge, the
// buttons' routes answer 501 with words the pane shows.
func TestCardActionsAreOffWithoutABridge(t *testing.T) {
	h := commonsServer(t, commonstest.TwoLands(t0))
	for _, c := range [][2]string{{"/garden/c/action/cl-dcard1", "action=dismiss"}, {"/garden/c/submit/cl-dcard1", "kind=promote"}} {
		r := httptest.NewRequest("POST", c[0], strings.NewReader(c[1]))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("X-Test-As", "claimlayer")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != http.StatusNotImplemented || !strings.Contains(rec.Body.String(), "Answer this one in Slack") {
			t.Errorf("POST %s = %d %q", c[0], rec.Code, rec.Body.String())
		}
	}
}
