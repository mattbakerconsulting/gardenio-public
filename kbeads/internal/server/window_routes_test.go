package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"gardenio/kbeads/internal/model"
)

// Fixture plan in claimlayer: one phase (two steps, one closed), one flat
// step, the narrative bundle and its doc, a resolved decision, an open
// escalation, a window that must stay out of the closure, and an agent on a
// step. Every value that must never leave carries the "SECRET" marker.
const (
	winPlan      = "cl-plan"
	winPhase     = "cl-ph1"
	winStepDone  = "cl-s1"
	winStepOpen  = "cl-s2"
	winStepFlat  = "cl-s3"
	winBundle    = "cl-bundle"
	winDoc       = "cl-doc"
	winDecision  = "cl-dec"
	winEsc       = "cl-esc"
	winAgent     = "cl-agent"
	winOtherPlan = "ot-plan"
)

func seedWindowPlan(t *testing.T, f *scopedFixture) {
	t.Helper()
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	closedAt := base.Add(48 * time.Hour)
	add := func(id string, typ model.BeadType, kind model.Kind, title string, status model.Status, fields string, parent string, offset time.Duration) {
		b := &model.Bead{
			ID: id, Type: typ, Kind: kind, Title: title, Status: status, Fleet: "default",
			Description: "SECRET-DESC " + id, CreatedBy: "SECRET-creator@example.com", Assignee: "SECRET-assignee",
			OperatedBy: "oidc:SECRET-op@example.com", CreatedAt: base.Add(offset), UpdatedAt: base.Add(offset),
		}
		if fields != "" {
			b.Fields = json.RawMessage(fields)
		}
		if status == model.StatusClosed {
			at := closedAt.Add(offset)
			b.ClosedAt = &at
		}
		f.ms.beads[id] = b
		f.ms.labels[id] = []string{"project:claimlayer"}
		if parent != "" {
			f.ms.deps[id] = append(f.ms.deps[id], &model.Dependency{BeadID: id, DependsOnID: parent, Type: model.DepParentChild})
		}
	}
	add(winPlan, model.TypeEpic, model.KindIssue, "The plan", model.StatusOpen,
		`{"plan_state":"building","phase_order":["`+winPhase+`"],"session_id":"SECRET-session"}`, "", 0)
	f.ms.labels[winPlan] = []string{"project:claimlayer", "plan"}
	add(winPhase, model.TypeEpic, model.KindIssue, "Phase one", model.StatusOpen, `{"step_order":["`+winStepOpen+`","`+winStepDone+`"]}`, winPlan, time.Minute)
	add(winStepDone, model.TypeTask, model.KindIssue, "Done step", model.StatusClosed, `{"mr_url":"https://SECRET.example/mr/1"}`, winPhase, 2*time.Minute)
	add(winStepOpen, model.TypeTask, model.KindIssue, "Open step", model.StatusInProgress, "", winPhase, 3*time.Minute)
	add(winStepFlat, model.TypeBug, model.KindIssue, "Flat step", model.StatusOpen, "", winPlan, 4*time.Minute)
	add(winBundle, model.TypeBundle, model.KindIssue, "Narrative", model.StatusOpen, `{"kind":"plan","headline":"Building now"}`, winPlan, 5*time.Minute)
	add(winDoc, typeDoc, model.KindData, "The narrative doc", model.StatusOpen, `{"format":"md","content":"# Hello","content_url":"https://SECRET.example/doc"}`, winBundle, 6*time.Minute)
	add("cl-doc-link", typeDoc, model.KindData, "A link doc", model.StatusOpen, `{"format":"link","content":"https://SECRET.example/link"}`, winBundle, 7*time.Minute)
	add(winDecision, model.TypeDecision, model.KindData, "Which way?", model.StatusClosed,
		`{"prompt":"SECRET-prompt","context":"SECRET-context","options":[{"id":"a","label":"Go left"},{"id":"b","label":"Go right"}],"chosen":"b","responded_by":"SECRET-responder@example.com"}`, winStepOpen, 8*time.Minute)
	add(winEsc, model.TypeEscalation, model.KindData, "SECRET-ESC-TITLE merge it", model.StatusOpen,
		`{"operation_type":"gitlab_mr_merge","operation_spec":{"cmd":"SECRET-cmd"},"sensitivity":"internal","context":"SECRET-esc-context"}`, winPlan, 9*time.Minute)
	add("cl-old-window", typeWindow, model.KindConfig, "SECRET-window", model.StatusOpen,
		`{"target":"`+winPlan+`","land":"claimlayer","token_hash":"SECRET-hash","expires_at":"2099-01-01T00:00:00Z","created_by":"x"}`, winPlan, 10*time.Minute)
	add(winAgent, "agent", model.KindConfig, "crew-a", model.StatusOpen, `{"agent":"crew-a","agent_state":"working","last_session_id":"SECRET-sess"}`, "", 11*time.Minute)
	f.ms.deps[winAgent] = append(f.ms.deps[winAgent], &model.Dependency{BeadID: winAgent, DependsOnID: winStepOpen, Type: windowAssignedDep})

	f.ms.beads[winOtherPlan] = &model.Bead{ID: winOtherPlan, Type: model.TypeEpic, Kind: model.KindIssue, Title: "Other plan", Status: model.StatusOpen, Fleet: "default"}
	f.ms.labels[winOtherPlan] = []string{"project:other", "plan"}
}

func windowDo(t *testing.T, f *scopedFixture, method, path, token string, body any) (int, string) {
	t.Helper()
	resp := oidcE2ERequest(t, f.ts, method, path, token, body)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// mintWindow mints a window on plan as token and returns the response.
func mintWindow(t *testing.T, f *scopedFixture, token, plan string, days int) (int, createWindowResponse) {
	t.Helper()
	code, body := windowDo(t, f, http.MethodPost, "/v1/windows", token, createWindowRequest{PlanID: plan, ExpiresInDays: days})
	var out createWindowResponse
	if code == http.StatusCreated {
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("decode mint: %v (%s)", err, body)
		}
	}
	return code, out
}

func TestWindowToken_Shape(t *testing.T) {
	a, ha, err := newWindowToken()
	if err != nil {
		t.Fatal(err)
	}
	b, _, _ := newWindowToken()
	if a == b {
		t.Fatal("two tokens are equal")
	}
	if !strings.HasPrefix(a, "gw_") || len(a) != windowTokenLen || !wellFormedWindowToken(a) {
		t.Errorf("token %q: want gw_ and %d characters", a, windowTokenLen)
	}
	if ha != windowTokenHash(a) || len(ha) != 64 {
		t.Errorf("hash %q is not the 64-hex sha256 of the token", ha)
	}
	for _, bad := range []string{"gw_", "gw_short", a + "x", "gw_" + strings.Repeat("!", 22), "xx_" + a[3:]} {
		if wellFormedWindowToken(bad) {
			t.Errorf("wellFormedWindowToken(%q) = true", bad)
		}
	}
}

func TestWindow_MintViewRevoke(t *testing.T) {
	f := newScopedFixture(t)
	seedWindowPlan(t, f)

	code, minted := mintWindow(t, f, tokScopedOwner, winPlan, 0)
	if code != http.StatusCreated {
		t.Fatalf("mint as owner: %d", code)
	}
	tok := minted.Token
	if !wellFormedWindowToken(tok) || minted.Path != "/w/"+tok {
		t.Fatalf("mint returned token %q path %q", tok, minted.Path)
	}
	if got := time.Until(minted.Window.ExpiresAt); got < 29*24*time.Hour || got > 31*24*time.Hour {
		t.Errorf("default expiry in %v, want 30 days", got)
	}
	win := f.ms.beads[minted.Window.ID]
	if win == nil || win.Type != typeWindow {
		t.Fatalf("no window bead %s", minted.Window.ID)
	}
	if f := windowFields(win); f.TokenHash != windowTokenHash(tok) || f.Target != winPlan || f.Land != "claimlayer" {
		t.Errorf("window fields = %+v", f)
	}
	if !f.ms.hasParentChildEdge(win.ID, winPlan) {
		t.Error("the window is not a child of the plan")
	}
	for id, b := range f.ms.beads {
		if strings.Contains(string(b.Fields), tok) || strings.Contains(b.Title, tok) || strings.Contains(b.Description, tok) {
			t.Errorf("bead %s stores the raw token", id)
		}
	}

	code, body := windowDo(t, f, http.MethodGet, windowViewPath, tok, nil)
	if code != http.StatusOK {
		t.Fatalf("view: %d %s", code, body)
	}
	for _, leak := range []string{"SECRET", "@example.com", winPlan, winPhase, winStepDone, winStepOpen, winStepFlat, winBundle, winDoc, winDecision, winEsc, winAgent, minted.Window.ID, "token_hash"} {
		if strings.Contains(body, leak) {
			t.Errorf("view leaks %q: %s", leak, body)
		}
	}
	var v windowView
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatal(err)
	}
	if v.Plan.Title != "The plan" || v.Plan.State != "building" || v.Plan.CurrentPhase != 1 || v.Plan.StepsDone != 1 || v.Plan.StepsTotal != 3 {
		t.Errorf("plan = %+v", v.Plan)
	}
	if len(v.Phases) != 1 || len(v.Phases[0].Steps) != 2 || v.Phases[0].Steps[0].Title != "Open step" || v.Phases[0].Steps[1].DoneAt == nil {
		t.Errorf("phases = %+v", v.Phases)
	}
	if len(v.Steps) != 1 || v.Steps[0].Title != "Flat step" || v.Steps[0].Kind != "bug" {
		t.Errorf("flat steps = %+v", v.Steps)
	}
	if len(v.Decisions) != 1 || v.Decisions[0].State != "resolved" || v.Decisions[0].Chosen != "Go right" {
		t.Errorf("decisions = %+v", v.Decisions)
	}
	if v.Escalations.Open != 1 || v.Escalations.Total != 1 || v.Escalations.Items[0].OperationType != "gitlab_mr_merge" || v.Escalations.Items[0].Sensitivity != "internal" {
		t.Errorf("escalations = %+v", v.Escalations)
	}
	if len(v.Crew) != 1 || v.Crew[0].Name != "crew-a" || !v.Crew[0].Live {
		t.Errorf("crew = %+v", v.Crew)
	}
	if v.Narrative.Headline != "Building now" || len(v.Narrative.Docs) != 1 || v.Narrative.Docs[0].Content != "# Hello" {
		t.Errorf("narrative = %+v", v.Narrative)
	}
	if len(v.Recent) == 0 || len(v.Recent) > windowRecentMax {
		t.Fatalf("recent = %+v", v.Recent)
	}
	for i := 1; i < len(v.Recent); i++ {
		if v.Recent[i].At.After(v.Recent[i-1].At) {
			t.Errorf("recent not newest first: %+v", v.Recent)
		}
	}

	// The owner's list carries neither the token nor its hash.
	code, body = windowDo(t, f, http.MethodGet, "/v1/windows?land=claimlayer", tokScopedOwner, nil)
	if code != http.StatusOK || !strings.Contains(body, minted.Window.ID) || strings.Contains(body, tok) || strings.Contains(body, windowTokenHash(tok)) {
		t.Errorf("list as owner: %d %s", code, body)
	}

	// Revoke: the next request is refused like an unknown token.
	code, body = windowDo(t, f, http.MethodDelete, "/v1/windows/"+minted.Window.ID, tokScopedOwner, nil)
	if code != http.StatusOK || !strings.Contains(body, `"state":"revoked"`) {
		t.Fatalf("revoke: %d %s", code, body)
	}
	if code, body := windowDo(t, f, http.MethodGet, windowViewPath, tok, nil); code != http.StatusUnauthorized || !strings.Contains(body, "invalid token") {
		t.Errorf("view after revoke: %d %s, want 401 invalid token", code, body)
	}
	if code, _ := windowDo(t, f, http.MethodDelete, "/v1/windows/"+minted.Window.ID, tokScopedOwner, nil); code != http.StatusOK {
		t.Errorf("second revoke: %d, want 200", code)
	}
}

func TestWindow_ExpiredIsUnknown(t *testing.T) {
	f := newScopedFixture(t)
	seedWindowPlan(t, f)
	_, minted := mintWindow(t, f, tokOperator, winPlan, 1)
	win := f.ms.beads[minted.Window.ID]
	var fm map[string]any
	_ = json.Unmarshal(win.Fields, &fm)
	fm[windowFieldExpiresAt] = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	win.Fields, _ = json.Marshal(fm)
	if code, body := windowDo(t, f, http.MethodGet, windowViewPath, minted.Token, nil); code != http.StatusUnauthorized || !strings.Contains(body, "invalid token") {
		t.Errorf("expired window: %d %s, want 401 invalid token", code, body)
	}
}

func TestWindow_MintAuthorization(t *testing.T) {
	f := newScopedFixture(t)
	seedWindowPlan(t, f)
	for name, c := range map[string]struct {
		token, plan string
		want        int
	}{
		"owner":                       {tokScopedOwner, winPlan, http.StatusCreated},
		"operator":                    {tokOperator, winPlan, http.StatusCreated},
		"admin":                       {tokOwnerAdmin, winPlan, http.StatusCreated},
		"member":                      {tokScopedWriter, winPlan, http.StatusForbidden},
		"guest":                       {tokScopedGuest, winPlan, http.StatusForbidden},
		"static token":                {scopedAdminToken, winPlan, http.StatusForbidden},
		"fleet token":                 {tokFleetBridge, winPlan, http.StatusForbidden},
		"land principal":              {tokLandClaimlayer, winPlan, http.StatusForbidden},
		"owner, plan in another land": {tokScopedOwner, winOtherPlan, http.StatusNotFound},
		"owner, no such plan":         {tokScopedOwner, "cl-nope", http.StatusNotFound},
		"owner, not an epic":          {tokScopedOwner, winStepOpen, http.StatusBadRequest},
	} {
		t.Run(name, func(t *testing.T) {
			if code, _ := mintWindow(t, f, c.token, c.plan, 0); code != c.want {
				t.Errorf("mint: %d, want %d", code, c.want)
			}
		})
	}
	for _, days := range []int{-1, 366} {
		if code, _ := mintWindow(t, f, tokScopedOwner, winPlan, days); code != http.StatusBadRequest {
			t.Errorf("expires_in_days %d: %d, want 400", days, code)
		}
	}
	// List and revoke follow the same gate.
	_, minted := mintWindow(t, f, tokScopedOwner, winPlan, 0)
	for _, tok := range []string{tokScopedWriter, scopedAdminToken, tokFleetBridge, tokLandClaimlayer, minted.Token} {
		if code, _ := windowDo(t, f, http.MethodGet, "/v1/windows?land=claimlayer", tok, nil); code != http.StatusForbidden {
			t.Errorf("list as %s: %d, want 403", tok, code)
		}
		if code, _ := windowDo(t, f, http.MethodDelete, "/v1/windows/"+minted.Window.ID, tok, nil); code != http.StatusForbidden {
			t.Errorf("revoke as %s: %d, want 403", tok, code)
		}
	}
	if code, _ := windowDo(t, f, http.MethodGet, "/v1/windows?land=other", tokScopedOwner, nil); code != http.StatusForbidden {
		t.Errorf("owner listing another land: %d, want 403", code)
	}
	// No generic write forges a window: type:window is admin-gated.
	forged := map[string]any{"title": "forged", "type": "window", "kind": "config", "labels": []string{"project:claimlayer"},
		"fields": map[string]any{"target": winPlan, "land": "claimlayer", "token_hash": windowTokenHash("gw_AAAAAAAAAAAAAAAAAAAAAA"), "expires_at": "2099-01-01T00:00:00Z", "created_by": "x"}}
	for _, tok := range []string{scopedAdminToken, tokFleetBridge, tokScopedOwner, tokLandClaimlayer} {
		if code, body := windowDo(t, f, http.MethodPost, "/v1/beads", tok, forged); code/100 == 2 {
			t.Errorf("generic create of a window as %s: %d %s", tok, code, body)
		}
	}
}

// TestWindow_CallerDenialMessage: a caller who owns no land is refused before
// the land is resolved, and the refusal names the land the request names, or
// no land at all, never a blank one (gd-sNA5UihnIv).
func TestWindow_CallerDenialMessage(t *testing.T) {
	f := newScopedFixture(t)
	seedWindowPlan(t, f)
	for name, c := range map[string]struct {
		method, path string
		body         any
		want         string
	}{
		"preview of a land": {http.MethodGet, "/v1/windows/preview?land=claimlayer", nil, "only the owner of land claimlayer or an operator"},
		"list of a land":    {http.MethodGet, "/v1/windows?land=claimlayer", nil, "only the owner of land claimlayer or an operator"},
		"preview of a plan": {http.MethodGet, "/v1/windows/preview?plan=" + winPlan, nil, "only a land's owner or an operator"},
		"mint":              {http.MethodPost, "/v1/windows", createWindowRequest{Land: "claimlayer"}, "only a land's owner or an operator"},
	} {
		t.Run(name, func(t *testing.T) {
			code, body := windowDo(t, f, c.method, c.path, tokScopedWriter, c.body)
			if code != http.StatusForbidden {
				t.Fatalf("%d %s, want 403", code, body)
			}
			if !strings.Contains(body, c.want) || strings.Contains(body, "land  ") {
				t.Errorf("refusal %s, want it to say %q", body, c.want)
			}
		})
	}
}

// TestWindow_Preview: the owner's preview is the projection a window would
// serve, admitted exactly as a mint is, and it mints, writes and counts
// nothing.
func TestWindow_Preview(t *testing.T) {
	f := newScopedFixture(t)
	seedWindowPlan(t, f)
	windows := func() int {
		n := 0
		for _, b := range f.ms.beads {
			if b.Type == typeWindow {
				n++
			}
		}
		return n
	}
	before := windows()
	code, body := windowDo(t, f, http.MethodGet, "/v1/windows/preview?plan="+winPlan, tokScopedOwner, nil)
	if code != http.StatusOK {
		t.Fatalf("preview as owner: %d %s", code, body)
	}
	for _, leak := range []string{"SECRET", "@example.com", winPlan, winStepOpen, winDecision, winEsc, winAgent, "token"} {
		if strings.Contains(body, leak) {
			t.Errorf("preview leaks %q: %s", leak, body)
		}
	}
	var pv windowView
	if err := json.Unmarshal([]byte(body), &pv); err != nil {
		t.Fatal(err)
	}
	if got := time.Until(pv.ExpiresAt); got < 29*24*time.Hour || got > 31*24*time.Hour {
		t.Errorf("preview expiry in %v, want 30 days", got)
	}
	if windows() != before {
		t.Errorf("preview minted a window: %d windows, was %d", windows(), before)
	}
	// It is the view a minted window serves, but for the expiry.
	_, minted := mintWindow(t, f, tokScopedOwner, winPlan, 0)
	_, viewBody := windowDo(t, f, http.MethodGet, windowViewPath, minted.Token, nil)
	var wv windowView
	if err := json.Unmarshal([]byte(viewBody), &wv); err != nil {
		t.Fatal(err)
	}
	pv.ExpiresAt, wv.ExpiresAt = time.Time{}, time.Time{}
	a, _ := json.Marshal(pv)
	b, _ := json.Marshal(wv)
	if !bytes.Equal(a, b) {
		t.Errorf("preview differs from the window's view:\npreview %s\nwindow  %s", a, b)
	}

	_, body = windowDo(t, f, http.MethodGet, "/v1/windows/preview?plan="+winPlan+"&expires_in_days=7", tokOperator, nil)
	_ = json.Unmarshal([]byte(body), &pv)
	if got := time.Until(pv.ExpiresAt); got < 6*24*time.Hour || got > 8*24*time.Hour {
		t.Errorf("preview with expires_in_days=7 expires in %v", got)
	}
	for name, c := range map[string]struct {
		token, query string
		want         int
	}{
		"operator":                    {tokOperator, "plan=" + winPlan, http.StatusOK},
		"member":                      {tokScopedWriter, "plan=" + winPlan, http.StatusForbidden},
		"guest":                       {tokScopedGuest, "plan=" + winPlan, http.StatusForbidden},
		"static token":                {scopedAdminToken, "plan=" + winPlan, http.StatusForbidden},
		"fleet token":                 {tokFleetBridge, "plan=" + winPlan, http.StatusForbidden},
		"land principal":              {tokLandClaimlayer, "plan=" + winPlan, http.StatusForbidden},
		"window":                      {minted.Token, "plan=" + winPlan, http.StatusForbidden},
		"owner, plan in another land": {tokScopedOwner, "plan=" + winOtherPlan, http.StatusNotFound},
		"owner, no plan":              {tokScopedOwner, "", http.StatusBadRequest},
		"owner, not an epic":          {tokScopedOwner, "plan=" + winStepOpen, http.StatusBadRequest},
		"owner, too long":             {tokScopedOwner, "plan=" + winPlan + "&expires_in_days=366", http.StatusBadRequest},
		"owner, not a number":         {tokScopedOwner, "plan=" + winPlan + "&expires_in_days=soon", http.StatusBadRequest},
	} {
		t.Run(name, func(t *testing.T) {
			if code, body := windowDo(t, f, http.MethodGet, "/v1/windows/preview?"+c.query, c.token, nil); code != c.want {
				t.Errorf("preview: %d %s, want %d", code, body, c.want)
			}
		})
	}
}

func TestWindow_LivePerLandCap(t *testing.T) {
	f := newScopedFixture(t)
	seedWindowPlan(t, f)
	// The seeded plan already holds one live window (cl-old-window).
	for i := 1; i < windowMaxLivePerLand; i++ {
		if code, _ := mintWindow(t, f, tokScopedOwner, winPlan, 0); code != http.StatusCreated {
			t.Fatalf("mint %d: %d", i+1, code)
		}
	}
	if code, _ := mintWindow(t, f, tokScopedOwner, winPlan, 0); code != http.StatusConflict {
		t.Errorf("mint past the cap: %d, want 409", code)
	}
}

// windowAuthExempt are the routes AuthMiddleware serves before it reads any
// bearer (health probes, the refresh-token exchange, runner registration).
var windowAuthExempt = map[string]bool{
	"GET /v1/health":            true,
	"GET /v1/health/live":       true,
	"POST /v1/auth/refresh":     true,
	"POST /v1/runners/register": true,
}

// TestWindow_BearerOpensOneRoute is the negative matrix: a live window bearer,
// on a plan or on a land (LW.1), is refused on every registered pattern but
// the view route, and the view route serves no other caller.
func TestWindow_BearerOpensOneRoute(t *testing.T) {
	f := newScopedFixture(t)
	seedWindowLand(t, f)
	_, onPlan := mintWindow(t, f, tokScopedOwner, winPlan, 0)
	_, onLand := mintLandWindow(t, f, tokScopedOwner, "claimlayer", 0)
	if onPlan.Token == "" || onLand.Token == "" {
		t.Fatal("mint failed")
	}
	for kind, tok := range map[string]string{windowTargetPlan: onPlan.Token, windowTargetLand: onLand.Token} {
		t.Run(kind, func(t *testing.T) { windowBearerOpensOneRoute(t, f, tok) })
	}
}

func windowBearerOpensOneRoute(t *testing.T, f *scopedFixture, tok string) {
	t.Helper()
	n := 0
	for _, pattern := range registeredRoutePatterns() {
		if pattern == windowViewPattern || pattern == windowLivePattern {
			// The window's own routes; the stream is exercised in
			// window_live_test.go.
			continue
		}
		n++
		method, path := routeRequest(pattern)
		code, body := doRoute(t, f, tok, method, path)
		switch {
		case windowAuthExempt[pattern]:
			// AuthMiddleware never reads a bearer here: the window answers
			// exactly as no credential does.
			if anon, _ := doRoute(t, f, "", method, path); code != anon {
				t.Errorf("%s as a window: %d, anonymous %d", pattern, code, anon)
			}
		case strings.HasPrefix(path, "/v1/lands/v1/"):
			// landmap.Guard answers a tenant problem+json, whoever refused.
			if code != http.StatusForbidden {
				t.Errorf("%s as a window: %d %q, want 403", pattern, code, body)
			}
		case code != http.StatusForbidden || !strings.Contains(body, "a window opens only"):
			t.Errorf("%s as a window: %d %q, want 403", pattern, code, body)
		}
	}
	if n < 100 {
		t.Fatalf("only %d routes exercised", n)
	}
	for _, path := range []string{windowViewPath, windowLivePath} {
		for _, method := range []string{http.MethodPost, http.MethodHead, http.MethodPut, http.MethodDelete} {
			if code, _ := doRoute(t, f, tok, method, path); code != http.StatusForbidden {
				t.Errorf("%s %s as a window: %d, want 403", method, path, code)
			}
		}
		for _, other := range []string{tokOwnerAdmin, tokOperator, tokScopedOwner, scopedAdminToken, tokFleetBridge, tokLandClaimlayer} {
			if code, _ := doRoute(t, f, other, http.MethodGet, path); code != http.StatusForbidden {
				t.Errorf("GET %s as %s: %d, want 403", path, other, code)
			}
		}
	}
	if code, _ := doRoute(t, f, tok, http.MethodGet, windowViewPath); code != http.StatusOK {
		t.Errorf("positive control: view as the window: %d", code)
	}
}

// TestWindow_UnclassifiedBearerIs401: any bearer the middleware cannot
// classify, gw_ or not, is 401 on HTTP and Unauthenticated on gRPC; a live
// window is PermissionDenied on gRPC.
func TestWindow_UnclassifiedBearerIs401(t *testing.T) {
	f := newScopedFixture(t)
	seedWindowPlan(t, f)
	_, minted := mintWindow(t, f, tokScopedOwner, winPlan, 0)
	unknown, _, _ := newWindowToken()
	bogus := []string{unknown, "gw_", "gw_not-a-token", minted.Token + "x", "not-a-token-at-all", "eyJhbGciOiJub25lIn0.e30."}
	for _, tok := range bogus {
		for _, path := range []string{windowViewPath, "/v1/beads", "/v1/whoami"} {
			if code, _ := doRoute(t, f, tok, http.MethodGet, path); code != http.StatusUnauthorized {
				t.Errorf("GET %s as %q: %d, want 401", path, tok, code)
			}
		}
	}

	interceptor := windowAuthInterceptor(f.srv, f.fc, nil)
	call := func(tok string) error {
		ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+tok))
		_, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/beads.v1.BeadsService/GetBead"}, func(context.Context, any) (any, error) { return "served", nil })
		return err
	}
	for _, tok := range bogus {
		if err := call(tok); status.Code(err) != codes.Unauthenticated {
			t.Errorf("gRPC as %q: %v, want Unauthenticated", tok, err)
		}
	}
	if err := call(minted.Token); status.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), "HTTP-only") {
		t.Errorf("gRPC as a live window: %v, want PermissionDenied HTTP-only", err)
	}
	if err := call(scopedAdminToken); err != nil {
		t.Errorf("positive control: gRPC as the static token: %v", err)
	}
	// The exported interceptor resolves no window: a live one is unknown.
	if _, err := AuthInterceptor(f.fc, nil)(metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+minted.Token)),
		nil, &grpc.UnaryServerInfo{FullMethod: "/beads.v1.BeadsService/GetBead"}, func(context.Context, any) (any, error) { return nil, nil }); status.Code(err) != codes.Unauthenticated {
		t.Errorf("AuthInterceptor without a resolver: %v, want Unauthenticated", err)
	}
}

// TestWindowPrincipal_Gates pins the gates a window context meets if it were
// ever to reach a handler: it holds no role, is roleBearing so every role gate
// enforces, and has an empty scope.
func TestWindowPrincipal_Gates(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, nil)
	win := &model.Bead{ID: "w-1", Type: typeWindow, Fields: json.RawMessage(`{"target":"p-1","land":"claimlayer"}`)}
	ctx := windowPrincipalContext(context.Background(), NewFleetConfig("gardenio", nil), win)

	if !roleBearing(authMethodFromContext(ctx)) {
		t.Error("a window is not roleBearing: role gates would wave it through as a service token")
	}
	if roles := RolesFromContext(ctx); len(roles) != 0 {
		t.Errorf("a window carries roles %v", roles)
	}
	if isScopedPrincipal(ctx) {
		t.Error("a window is a scoped principal")
	}
	if d := scopeFromContext(ctx); d.Unrestricted || len(d.Projects) != 0 {
		t.Errorf("scopeFromContext = %+v, want empty", d)
	}
	if callerMayApproveAsAdmin(ctx) || isAdminCaller(ctx) {
		t.Error("a window is an admin")
	}
	for _, role := range []string{RoleViewer, RoleOperator, RoleAdmin} {
		for wrap, mw := range map[string]func(string, http.HandlerFunc) http.HandlerFunc{
			"requireRole": requireRole, "requireRoleMachineAllowed": requireRoleMachineAllowed,
		} {
			rec := httptest.NewRecorder()
			mw(role, func(http.ResponseWriter, *http.Request) {})(rec, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx))
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s(%s) = %d, want 403", wrap, role, rec.Code)
			}
		}
	}
	for k := range machineAllowedWrites {
		if d := s.authorizeWrite(ctx, k.Action, k.BeadType, []string{"project:claimlayer"}); d == nil {
			t.Errorf("authorizeWrite(%s %s) admitted a window", k.Action, k.BeadType)
		}
	}
	if d := s.authorizeWrite(ctx, "create", typeWindow, []string{"project:claimlayer"}); d == nil {
		t.Error("authorizeWrite(create window) admitted a window")
	}
	if d := windowManageDenial(ctx, "claimlayer"); d == nil {
		t.Error("a window may manage windows")
	}
	_, err := RBACInterceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/beads.v1.BeadsService/GetBead"}, func(context.Context, any) (any, error) { return nil, nil })
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("RBACInterceptor = %v, want PermissionDenied", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/beads", nil).WithContext(ctx)
	req.Pattern = "GET /v1/beads"
	scopedRouteGate(nil, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("scopedRouteGate(GET /v1/beads) = %d, want 403", rec.Code)
	}
}

func TestWindow_ViewCounts(t *testing.T) {
	f := newScopedFixture(t)
	seedWindowPlan(t, f)
	_, minted := mintWindow(t, f, tokScopedOwner, winPlan, 0)
	req := func(views string) {
		r, _ := http.NewRequest(http.MethodGet, f.ts.URL+windowViewPath, nil)
		r.Header.Set("Authorization", "Bearer "+minted.Token)
		if views != "" {
			r.Header.Set(windowViewsHeader, views)
		}
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("view: %d", resp.StatusCode)
		}
	}
	stored := func() windowFieldSet {
		b, err := f.ms.GetBead(context.Background(), minted.Window.ID)
		if err != nil || b == nil {
			t.Fatalf("read window: %v", err)
		}
		return windowFields(b)
	}
	req("") // the first view flushes at once
	deadline := time.Now().Add(5 * time.Second)
	for stored().ViewCount != 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := stored(); got.ViewCount != 1 || got.LastViewedAt == "" {
		t.Fatalf("after one view: %+v", got)
	}
	req("0") // a refresh, not a view
	req("7")
	req("100000") // capped
	if n, _ := f.srv.windowViewCounter().pending(minted.Window.ID); n != 7+windowViewsMax {
		t.Errorf("pending = %d, want %d (held until the interval ends)", n, 7+windowViewsMax)
	}
	if got := stored().ViewCount; got != 1 {
		t.Errorf("view_count written on the request path: %v", got)
	}
	code, body := windowDo(t, f, http.MethodGet, "/v1/windows?land=claimlayer", tokScopedOwner, nil)
	if code != http.StatusOK || !strings.Contains(body, `"view_count":1008`) {
		t.Errorf("list counts pending views: %d %s", code, body)
	}
}

// windowViewGolden is the frozen GET /v1/windows/self/view body for the
// seedWindowPlan closure (ruling gd-AnnNVNRtDn: windowView is the PW payload
// contract; PW.2's planwindow fixtures copy this file). A field change here is
// a contract change: comment it on PW.1 gd-sCLo2acEFb and PW.2 gd-jX6ogU9l5T.
const windowViewGolden = "testdata/window_view.golden.json"

func TestWindowView_Golden(t *testing.T) {
	f := newScopedFixture(t)
	seedWindowPlan(t, f)
	win := &model.Bead{ID: "cl-golden-window", Type: typeWindow, Status: model.StatusOpen,
		Fields: json.RawMessage(`{"target":"` + winPlan + `","land":"claimlayer","expires_at":"2026-10-31T12:00:00Z"}`)}
	ctx := windowPrincipalContext(context.Background(), f.fc, win)
	rec := httptest.NewRecorder()
	f.srv.handleWindowView(rec, httptest.NewRequest(http.MethodGet, windowViewPath, nil).WithContext(ctx))
	if rec.Code != http.StatusOK {
		t.Fatalf("view: %d %s", rec.Code, rec.Body)
	}
	got := rec.Body.Bytes()
	// -update is the package's golden flag (machine_grants_test.go).
	if *updateMachineGolden {
		if err := os.WriteFile(windowViewGolden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(windowViewGolden)
	if err != nil {
		t.Fatalf("read golden (regenerate with `go test -run TestWindowView_Golden -update`): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("the window view differs from %s, the PW payload contract (regenerate with -update and tell PW.2):\n got: %s\nwant: %s", windowViewGolden, got, want)
	}
}
