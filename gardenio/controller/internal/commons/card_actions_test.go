package commons

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/commons/commonstest"
)

// fakeInbound records the card actions the Commons sends the bridge.
type fakeInbound struct {
	mu     sync.Mutex
	got    []CardRequest
	bearer []string
	answer CardAnswer
	err    error
}

func (f *fakeInbound) Card(ctx context.Context, req CardRequest) (CardAnswer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, req)
	f.bearer = append(f.bearer, beadsapi.UserBearerFromContext(ctx))
	return f.answer, f.err
}

// fakeEscalations records Approve and Reject.
type fakeEscalations struct{ got []string }

func (f *fakeEscalations) Approve(_ *http.Request, id string) error {
	f.got = append(f.got, "approve "+id)
	return nil
}

func (f *fakeEscalations) Reject(_ *http.Request, id, reason string) error {
	f.got = append(f.got, "reject "+id+": "+reason)
	return nil
}

func postForm(t *testing.T, h http.Handler, path, form string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", path, strings.NewReader(form))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("X-Test-As", "claimlayer")
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// TestCardActions drives the card action and submit routes: the land cut, the
// action-table gate, what goes to the bridge, the escalation path, and the
// answer (the card as it now renders, with the bridge's words).
func TestCardActions(t *testing.T) {
	in := &fakeInbound{answer: CardAnswer{Notes: []string{"Recorded your choice: *Ship it*."}}}
	esc := &fakeEscalations{}
	h := commonsServer(t, commonstest.TwoLands(t0), func(h *Handler) { h.Inbound, h.Escalations = in, esc })

	for _, c := range []struct {
		name, path, form string
		hdr              []string
		code             int
		want             string
	}{
		{"anonymous", "/garden/c/action/cl-dcard1", "action=resolve&value=1", []string{"X-Test-Anon", "1"}, http.StatusUnauthorized, ""},
		{"guest", "/garden/c/action/cl-dcard1", "action=resolve&value=1", []string{"X-Test-Guest", "1"}, http.StatusForbidden, "does not let you"},
		{"another land's card", "/garden/c/action/gd-msg1", "action=resolve&value=1", nil, http.StatusNotFound, ""},
		{"not a card", "/garden/c/action/cl-msg1", "action=resolve", nil, http.StatusNotFound, ""},
		{"no such button", "/garden/c/action/cl-dcard1", "action=approve", nil, http.StatusBadRequest, ""},
		{"empty own words", "/garden/c/submit/cl-dcard1", "kind=other&text=+", nil, http.StatusBadRequest, ""},
		{"resolve", "/garden/c/action/cl-dcard1", "action=resolve&value=1", nil, http.StatusOK, `id="m-cl-dcard1"`},
		{"own words", "/garden/c/submit/cl-dcard1", "kind=other&text=Ship+it+Monday", nil, http.StatusOK, `id="m-cl-dcard1"`},
		{"escalate", "/garden/c/submit/cl-dcard1", "kind=promote", nil, http.StatusOK, ""},
		{"allow", "/garden/c/action/cl-pcard1", "action=allow", nil, http.StatusOK, `id="m-cl-pcard1"`},
		{"dismiss", "/garden/c/action/cl-dcard1", "action=dismiss&reason=+Asked+twice+", nil, http.StatusOK, `id="m-cl-dcard1"`},
		{"dismiss, no reason", "/garden/c/action/cl-dcard1", "action=dismiss&reason=+", nil, http.StatusOK, `id="m-cl-dcard1"`},
		{"kill", "/garden/c/action/cl-card1", "action=kill", nil, http.StatusOK, `id="m-cl-card1"`},
		{"approve", "/garden/c/action/cl-ecard1", "action=approve", nil, http.StatusOK, `id="m-cl-ecard1"`},
		{"reject", "/garden/c/action/cl-ecard1", "action=reject", nil, http.StatusOK, ""},
	} {
		rec := postForm(t, h, c.path, c.form, c.hdr...)
		if rec.Code != c.code || !strings.Contains(rec.Body.String(), c.want) {
			t.Errorf("%s: %d %q, want %d containing %q", c.name, rec.Code, rec.Body.String(), c.code, c.want)
		}
		if c.name == "resolve" {
			note, _ := url.PathUnescape(rec.Header().Get("X-Commons-Note"))
			if note != "Recorded your choice: Ship it." {
				t.Errorf("resolve note %q", note)
			}
		}
	}

	want := []CardRequest{
		{Kind: "action", Card: "cl-dcard1", Action: "resolve", Value: "1"},
		{Kind: "submit", Card: "cl-dcard1", Action: "other", Values: map[string]string{"text": "Ship it Monday"}},
		{Kind: "submit", Card: "cl-dcard1", Action: "promote"},
		{Kind: "action", Card: "cl-pcard1", Action: "allow"},
		{Kind: "action", Card: "cl-dcard1", Action: "dismiss", Values: map[string]string{"reason": "Asked twice"}},
		{Kind: "action", Card: "cl-dcard1", Action: "dismiss"},
		{Kind: "action", Card: "cl-card1", Action: "kill"},
	}
	gotJSON, _ := json.Marshal(in.got)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("the bridge got %s\nwant %s", gotJSON, wantJSON)
	}
	for _, b := range in.bearer {
		if b != commonstest.ClaimlayerBearer {
			t.Errorf("the bridge call carried bearer %q, want the viewer's", b)
		}
	}
	if strings.Join(esc.got, "; ") != "approve cl-esc1; reject cl-esc1: Rejected in the Commons" {
		t.Errorf("escalations got %v", esc.got)
	}
}

// TestCardActionsRefuseCrossOrigin: a card action and a lightbox's form are
// taken only from the Commons' own page. Another page, even a same-site one
// carrying the viewer's cookie, gets 403, and neither the bridge nor the
// daemon's escalation routes are called.
func TestCardActionsRefuseCrossOrigin(t *testing.T) {
	in := &fakeInbound{}
	esc := &fakeEscalations{}
	h := commonsServer(t, commonstest.TwoLands(t0), func(h *Handler) { h.Inbound, h.Escalations = in, esc })
	routes := []struct{ path, form string }{
		{"/garden/c/action/cl-card1", "action=kill"},
		{"/garden/c/action/cl-pcard1", "action=allow"},
		{"/garden/c/action/cl-ecard1", "action=approve"},
		{"/garden/c/submit/cl-dcard1", "kind=other&text=Ship+it"},
	}
	for name, hdr := range map[string][]string{
		"cross-site":            {"Sec-Fetch-Site", "cross-site", "Origin", "https://evil.example"},
		"same-site":             {"Sec-Fetch-Site", "same-site", "Origin", "https://other.example.com"},
		"foreign origin":        {"Origin", "https://evil.example"},
		"opaque origin":         {"Origin", "null"},
		"origin on another fwd": {"Origin", "https://evil.example", "X-Forwarded-Host", "example.com"},
	} {
		for _, rt := range routes {
			if rec := postForm(t, h, rt.path, rt.form, hdr...); rec.Code != http.StatusForbidden {
				t.Errorf("%s %s = %d %q, want 403", name, rt.path, rec.Code, rec.Body.String())
			}
		}
	}
	if len(in.got) != 0 || len(esc.got) != 0 {
		t.Fatalf("a cross-origin card action went through: bridge %v, escalations %v", in.got, esc.got)
	}
	// Control: the page's own fetch goes through.
	for _, rt := range routes {
		if rec := postForm(t, h, rt.path, rt.form, "Sec-Fetch-Site", "same-origin"); rec.Code != http.StatusOK {
			t.Errorf("same-origin %s = %d %q, want 200", rt.path, rec.Code, rec.Body.String())
		}
	}
}

// TestCardActions_BridgeRefusal passes the bridge's refusal through with its
// words, and hides anything else behind a 502.
func TestCardActions_BridgeRefusal(t *testing.T) {
	in := &fakeInbound{err: &CardRefusal{Status: http.StatusConflict, Message: "This decision is answered already."}}
	h := commonsServer(t, commonstest.TwoLands(t0), func(h *Handler) { h.Inbound = in })
	if rec := postForm(t, h, "/garden/c/action/cl-dcard1", "action=dismiss"); rec.Code != http.StatusConflict ||
		!strings.Contains(rec.Body.String(), "answered already") {
		t.Errorf("refusal: %d %q", rec.Code, rec.Body.String())
	}
	in.err = &CardRefusal{Status: http.StatusInternalServerError, Message: "stack trace"}
	if rec := postForm(t, h, "/garden/c/action/cl-dcard1", "action=dismiss"); rec.Code != http.StatusBadGateway ||
		strings.Contains(rec.Body.String(), "stack trace") {
		t.Errorf("failure: %d %q", rec.Code, rec.Body.String())
	}
}

// TestBridgeInbound posts the card request to the bridge on the viewer's
// bearer and reads its answer and its refusals.
func TestBridgeInbound(t *testing.T) {
	var gotAuth string
	var gotReq CardRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		if gotReq.Action == "deny" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"Your role in claimlayer does not let you do that."}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"handled","ephemeral":["Allowed."]}`))
	}))
	defer srv.Close()
	in := NewBridgeInbound(srv.URL+"/", nil)
	ctx := beadsapi.WithUserBearer(context.Background(), "tok")
	a, err := in.Card(ctx, CardRequest{Kind: "action", Card: "cl-pcard1", Action: "allow"})
	if err != nil || len(a.Notes) != 1 || a.Notes[0] != "Allowed." || gotAuth != "Bearer tok" || gotReq.Card != "cl-pcard1" {
		t.Fatalf("allow: %+v %v auth=%q req=%+v", a, err, gotAuth, gotReq)
	}
	_, err = in.Card(ctx, CardRequest{Kind: "action", Card: "cl-pcard1", Action: "deny"})
	if r, ok := err.(*CardRefusal); !ok || r.Status != http.StatusForbidden || !strings.Contains(r.Message, "does not let you") {
		t.Fatalf("deny: %v", err)
	}
	if _, err := in.Card(context.Background(), CardRequest{Kind: "action"}); err == nil {
		t.Fatal("a call with no bearer went out")
	}
	if NewBridgeInbound(" ", nil) != nil {
		t.Fatal("an empty bridge URL built an Inbound")
	}
}

// TestAgentCardButtons: a live agent card offers Kill, and Restart only when
// the agent is bound to a Slack thread to restart in; an ended one neither.
func TestAgentCardButtons(t *testing.T) {
	d := commonstest.TwoLands(t0)
	h := commonsServer(t, d)
	_, body := get(t, h, "/test/m/cl-card1", "claimlayer")
	mustContain(t, body, `<summary class="btn small">Kill…</summary>`, `value="kill">Kill agent claimlayer-crew-1</button>`)
	mustNotContain(t, body, `value="restart"`)

	a, err := d.GetBead(context.Background(), "cl-agent1")
	if err != nil {
		t.Fatal(err)
	}
	bound := *a
	bound.Fields = map[string]string{}
	for k, v := range a.Fields {
		bound.Fields[k] = v
	}
	bound.Fields["slack_thread_channel"], bound.Fields["slack_thread_ts"] = "C0CREW", "1690000000.000100"
	d.Put(&bound)
	_, body = get(t, h, "/test/m/cl-card1", "claimlayer")
	mustContain(t, body, `value="restart">Restart</button>`, `value="kill">Kill agent claimlayer-crew-1</button>`)

	bound.Status = "closed"
	bound.Fields["agent_state"] = "done"
	d.Put(&bound)
	_, body = get(t, h, "/test/m/cl-card1", "claimlayer")
	mustNotContain(t, body, `value="restart"`, `value="kill"`)
}
