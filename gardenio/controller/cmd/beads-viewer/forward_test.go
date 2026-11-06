package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// forwardFake builds a minimal fakeDaemon with one bead and one preset
// alias mapping (alice@example.com → operator id 42).
func forwardFake() *fakeDaemon {
	return &fakeDaemon{
		beads: map[string]*beadsapi.BeadDetail{
			"gd-fwd1": {
				ID:       "gd-fwd1",
				Title:    "Forward target",
				Type:     "task",
				Status:   "open",
				Priority: 2,
				Labels:   []string{"project:gardenio"},
			},
		},
		comments: map[string][]beadsapi.Comment{"gd-fwd1": {}},
		aliasResp: map[string]*beadsapi.LookupOperatorByAliasResponse{
			"oidc|alice@example.com": {Found: true, OperatorID: 42},
		},
	}
}

// TestBeadShow_RendersForwardPanelSeed verifies that the bead-show page
// pre-renders the forward panel with the operator's email so the form is
// usable without an extra round trip.
func TestBeadShow_RendersForwardPanelSeed(t *testing.T) {
	mux := mkServer(t, forwardFake())
	req := httptest.NewRequest("GET", "/gd-fwd1", nil)
	req.Header.Set("X-Auth-Request-Email", authorEmail)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`id="forward-panel"`,
		`Forward to operator`,
		`hx-post="/b/gd-fwd1/forward"`,
		`name="email"`,
		`name="note"`,
		`Followers (0)`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("forward panel missing %q\nbody snippet=%s", want, snippetForward(body))
		}
	}
}

// TestBeadShow_AnonymousForwardPanelHidesForm verifies anonymous viewers see
// the followers section (read-only) but not the form.
func TestBeadShow_AnonymousForwardPanelHidesForm(t *testing.T) {
	mux := mkServer(t, forwardFake())
	req := httptest.NewRequest("GET", "/gd-fwd1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, `hx-post="/b/gd-fwd1/forward"`) {
		t.Errorf("anonymous viewer should not see forward form")
	}
	if !strings.Contains(body, "Sign in to forward this bead") {
		t.Errorf("expected anonymous prompt, body snippet=%s", snippetForward(body))
	}
}

// TestAddForward_HappyPath posts a forward, then re-renders the panel with
// the new follower visible. The alias is resolved via the fake's preset map.
func TestAddForward_HappyPath(t *testing.T) {
	f := forwardFake()
	mux := mkServer(t, f)
	form := url.Values{
		"email": []string{"alice@example.com"},
		"note":  []string{"please look"},
	}
	req := authedPost("/b/gd-fwd1/forward", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.followersAddCalls != 1 {
		t.Fatalf("expected 1 AddBeadFollower call, got %d", f.followersAddCalls)
	}
	if f.followersAddLastBead != "gd-fwd1" || f.followersAddLastOp != 42 || f.followersAddLastNote != "please look" {
		t.Errorf("unexpected add args: bead=%s op=%d note=%q", f.followersAddLastBead, f.followersAddLastOp, f.followersAddLastNote)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Followers (1)") {
		t.Errorf("expected Followers (1) in re-rendered panel, body=%s", snippetForward(body))
	}
	if !strings.Contains(body, "please look") {
		t.Errorf("expected note in re-rendered panel, body=%s", snippetForward(body))
	}
}

// TestAddForward_UnknownEmailRendersInlineError verifies an alias miss surfaces
// an inline error and preserves the operator's draft so a typo is one keystroke
// away from being fixed.
func TestAddForward_UnknownEmailRendersInlineError(t *testing.T) {
	f := forwardFake()
	mux := mkServer(t, f)
	form := url.Values{
		"email": []string{"nobody@example.com"},
		"note":  []string{"oops"},
	}
	req := authedPost("/b/gd-fwd1/forward", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	if f.followersAddCalls != 0 {
		t.Errorf("AddBeadFollower must not be called on resolve failure, got %d", f.followersAddCalls)
	}
	body := w.Body.String()
	for _, want := range []string{
		"No operator found for that email.",
		`value="nobody@example.com"`, // prefilled email
		`value="oops"`,               // prefilled note
	} {
		if !strings.Contains(body, want) {
			t.Errorf("error render missing %q\nbody=%s", want, snippetForward(body))
		}
	}
}

// TestAddForward_OperatorIDPrefixSkipsAlias verifies "operator-id:N" bypasses
// the alias lookup and forwards directly.
func TestAddForward_OperatorIDPrefixSkipsAlias(t *testing.T) {
	f := forwardFake()
	mux := mkServer(t, f)
	form := url.Values{"email": []string{"operator-id:99"}}
	req := authedPost("/b/gd-fwd1/forward", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.aliasCalls != 0 {
		t.Errorf("operator-id input should not hit alias lookup, got %d alias calls", f.aliasCalls)
	}
	if f.followersAddCalls != 1 || f.followersAddLastOp != 99 {
		t.Errorf("expected forward to op=99, got calls=%d op=%d", f.followersAddCalls, f.followersAddLastOp)
	}
}

// TestRemoveForward_HappyPath unforwards an existing follower and re-renders
// the now-shorter list.
func TestRemoveForward_HappyPath(t *testing.T) {
	f := forwardFake()
	f.followersByBead = map[string][]*beadsapi.BeadFollower{
		"gd-fwd1": {{BeadID: "gd-fwd1", OperatorID: 42, OperatorDisplay: "alice"}},
	}
	mux := mkServer(t, f)
	form := url.Values{"operator_id": []string{"42"}}
	req := authedPost("/b/gd-fwd1/forward/remove", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.followersRemoveCalls != 1 || f.followersRemoveLastBead != "gd-fwd1" || f.followersRemoveLastOp != 42 {
		t.Errorf("unexpected remove args: calls=%d bead=%s op=%d", f.followersRemoveCalls, f.followersRemoveLastBead, f.followersRemoveLastOp)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Followers (0)") {
		t.Errorf("expected Followers (0) after remove, body=%s", snippetForward(body))
	}
}

// TestRemoveForward_InvalidOperatorID rejects non-numeric input with an inline
// error, without calling the daemon.
func TestRemoveForward_InvalidOperatorID(t *testing.T) {
	f := forwardFake()
	mux := mkServer(t, f)
	form := url.Values{"operator_id": []string{"not-a-number"}}
	req := authedPost("/b/gd-fwd1/forward/remove", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	if f.followersRemoveCalls != 0 {
		t.Errorf("RemoveBeadFollower must not be called on parse failure, got %d", f.followersRemoveCalls)
	}
	if !strings.Contains(w.Body.String(), "Invalid operator id.") {
		t.Errorf("expected inline error, body=%s", snippetForward(w.Body.String()))
	}
}

// snippetForward extracts the forward-panel section from a full bead-show
// render to keep test failure output readable.
func snippetForward(body string) string {
	const start = `id="forward-panel"`
	const end = `</section>`
	i := strings.Index(body, start)
	if i < 0 {
		if len(body) > 1500 {
			return body[:1500]
		}
		return body
	}
	rest := body[i:]
	j := strings.Index(rest, end)
	if j < 0 {
		return rest
	}
	return rest[:j+len(end)]
}
