package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"gardenio/clicreds/oidclogin"
	"gardenio/lands/contract/landsv1"
)

const host = "127.0.0.1"

func TestTicketShow_SendsTheLandsBearerThroughItsDoor(t *testing.T) {
	useTempState(t)
	f := newFakeLand(t, "acme")
	f.answer("GET /tickets/or-Tkt4nB8vCx", 200, "Ticket/open.json")
	signIn(t, "acme", host, oidclogin.Session{AccessToken: "acme-tok"})

	r := run(t, runOpts{}, "--edge", f.srv.URL, "--land", "acme", "ticket", "show", "or-Tkt4nB8vCx")
	if r.e != nil {
		t.Fatalf("gl: %v\n%s", r.e, r.err)
	}
	reqs := f.requests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want 1", len(reqs))
	}
	if got := reqs[0].header.Get("Authorization"); got != "Bearer acme-tok" {
		t.Errorf("Authorization = %q", got)
	}
	var want landsv1.Ticket
	_ = json.Unmarshal(fixture(t, "Ticket/open.json"), &want)
	if !strings.Contains(r.out, want.Title) || !strings.Contains(r.out, string(want.Status)) {
		t.Errorf("output does not show the ticket:\n%s", r.out)
	}
}

func TestTicketOpen_PostsTheContractBodyWithAnIdempotencyKey(t *testing.T) {
	useTempState(t)
	f := newFakeLand(t, "acme")
	f.answer("POST /tickets", 201, "Ticket/open.json")
	signIn(t, "acme", host, oidclogin.Session{})

	r := run(t, runOpts{}, "--edge", f.srv.URL, "--land", "acme", "ticket", "open", "--kind", "bug", "--priority", "2", "Fix", "the", "gate")
	if r.e != nil {
		t.Fatalf("gl: %v\n%s", r.e, r.err)
	}
	req := f.requests()[0]
	var body map[string]any
	if err := json.Unmarshal([]byte(req.body), &body); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"kind": "bug", "title": "Fix the gate", "priority": float64(2)}
	if len(body) != len(want) {
		t.Errorf("body = %v, want %v", body, want)
	}
	for k, v := range want {
		if body[k] != v {
			t.Errorf("body[%s] = %v, want %v", k, body[k], v)
		}
	}
	if !regexp.MustCompile(`^[A-Z2-7]{26}$`).MatchString(req.header.Get("Idempotency-Key")) {
		t.Errorf("Idempotency-Key = %q", req.header.Get("Idempotency-Key"))
	}
}

func TestTicketList_JSONIsTheContractPage(t *testing.T) {
	useTempState(t)
	f := newFakeLand(t, "acme")
	f.answer("GET /tickets", 200, "TicketPage/first_page.json")
	signIn(t, "acme", host, oidclogin.Session{})

	r := run(t, runOpts{}, "--edge", f.srv.URL, "--land", "acme", "--json", "ticket", "list", "--status", "open", "--limit", "5")
	if r.e != nil {
		t.Fatalf("gl: %v\n%s", r.e, r.err)
	}
	if q := f.requests()[0].query; !strings.Contains(q, "status=open") || !strings.Contains(q, "limit=5") {
		t.Errorf("query = %q", q)
	}
	var got, want landsv1.TicketPage
	if err := json.Unmarshal([]byte(r.out), &got); err != nil {
		t.Fatalf("--json output is not a TicketPage: %v\n%s", err, r.out)
	}
	_ = json.Unmarshal(fixture(t, "TicketPage/first_page.json"), &want)
	if len(got.Items) != len(want.Items) || got.Items[0].Id != want.Items[0].Id {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// Arms print as their emoji run before the title, never as the form they
// came in: no colour word, no empty places.
func TestArmsPrintAsTheEmojiRun(t *testing.T) {
	useTempState(t)
	f := newFakeLand(t, "acme")
	page := func(names ...string) http.HandlerFunc {
		items := make([]json.RawMessage, 0, len(names))
		for _, n := range names {
			items = append(items, fixture(t, n))
		}
		body, _ := json.Marshal(map[string]any{"items": items})
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(body)
		}
	}
	f.on("GET /tickets", page("Ticket/with_arms.json", "Ticket/open.json"))
	f.on("GET /plans", page("Plan/with_arms.json"))
	f.answer("GET /plans/p1", 200, "Plan/with_arms.json")
	f.answer("GET /tickets/t1", 200, "Ticket/with_arms.json")
	signIn(t, "acme", host, oidclogin.Session{})

	for _, tc := range []struct {
		args       []string
		want, none string
	}{
		{[]string{"ticket", "list"}, "  🍎🏷️🪙 Price the apples\n", "red"},
		{[]string{"ticket", "show", "t1"}, "or-Tkt2pL7mQs  🍎🏷️🪙 Price the apples\n", "red"},
		{[]string{"plan", "list"}, "  🔥🧭📦 Autumn shop\n", "blue"},
		{[]string{"plan", "show", "p1"}, "or-Pln5kR2tHa  🔥🧭📦 Autumn shop\n", "blue"},
	} {
		r := run(t, runOpts{}, append([]string{"--edge", f.srv.URL, "--land", "acme"}, tc.args...)...)
		if r.e != nil || !strings.Contains(r.out, tc.want) || strings.Contains(r.out, tc.none) || strings.Contains(r.out, " . ") {
			t.Errorf("gl %v: %v\n%s", tc.args, r.e, r.out)
		}
	}
	// A ticket bearing none prints its title alone.
	if r := run(t, runOpts{}, "--edge", f.srv.URL, "--land", "acme", "ticket", "list"); !strings.Contains(r.out, "  Add a seasonal banner\n") {
		t.Errorf("ticket list:\n%s", r.out)
	}
}

func TestTicketSearch_SendsTheText(t *testing.T) {
	useTempState(t)
	f := newFakeLand(t, "acme")
	f.answer("GET /search/tickets", 200, "TicketPage/last_page.json")
	signIn(t, "acme", host, oidclogin.Session{})

	r := run(t, runOpts{}, "--edge", f.srv.URL, "--land", "acme", "ticket", "search", "autumn", "banner")
	if r.e != nil {
		t.Fatalf("gl: %v\n%s", r.e, r.err)
	}
	if q := f.requests()[0].query; q != "q=autumn+banner" {
		t.Errorf("query = %q", q)
	}
}

func TestA401RenewsTheSignInOnceAndResendsTheSameRequest(t *testing.T) {
	useTempState(t)
	k := newFakeKeycloak(t)
	f := newFakeLand(t, "acme")
	created := fixture(t, "Ticket/open.json")
	f.on("POST /tickets", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer renewed" {
			problem(w, t, 401, "Problem/unauthenticated.json")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		_, _ = w.Write(created)
	})
	signIn(t, "acme", host, oidclogin.Session{Issuer: k.srv.URL, ClientID: "gardenio-cli", AccessToken: "stale", RefreshToken: "refresh-1"})

	r := run(t, runOpts{}, "--edge", f.srv.URL, "--land", "acme", "ticket", "open", "Fix the gate")
	if r.e != nil {
		t.Fatalf("gl: %v\n%s", r.e, r.err)
	}
	reqs := f.requests()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want 2", len(reqs))
	}
	if reqs[0].header.Get("Idempotency-Key") != reqs[1].header.Get("Idempotency-Key") || reqs[0].body != reqs[1].body {
		t.Errorf("the resend is not the same request:\n%+v\n%+v", reqs[0], reqs[1])
	}
	s, _ := oidclogin.LoadFor("acme")
	if s == nil || s.AccessToken != "renewed" || s.Land != "acme" || s.Daemon != host {
		t.Errorf("land session after renewal = %+v", s)
	}
	if plain, _ := oidclogin.LoadFor(""); plain != nil {
		t.Errorf("renewal wrote the ordinary session: %+v", plain)
	}
}

func TestAnEndedSignInSaysHowToSignInAgain(t *testing.T) {
	useTempState(t)
	f := newFakeLand(t, "acme")
	f.on("GET /budget", func(w http.ResponseWriter, _ *http.Request) { problem(w, t, 401, "Problem/unauthenticated.json") })
	signIn(t, "acme", host, oidclogin.Session{})

	r := run(t, runOpts{}, "--edge", f.srv.URL, "--land", "acme", "budget")
	var se *signInError
	if !errors.As(r.e, &se) || !strings.Contains(r.e.Error(), "gl login --land acme") {
		t.Fatalf("error = %v, want a sign-in error naming 'gl login --land acme'", r.e)
	}
}

func TestASignInForAnotherEdgeIsNeverSent(t *testing.T) {
	useTempState(t)
	f := newFakeLand(t, "acme")
	f.answer("GET /budget", 200, "Budget/ok.json")
	signIn(t, "acme", "api.example.com", oidclogin.Session{})

	r := run(t, runOpts{}, "--edge", f.srv.URL, "--land", "acme", "budget")
	if r.e == nil || !strings.Contains(r.e.Error(), "gl login --land acme") {
		t.Fatalf("error = %v", r.e)
	}
	if n := len(f.requests()); n != 0 {
		t.Errorf("the land got %d requests; a session for another edge must not be sent", n)
	}
}

func TestASignInForAnotherLandIsNeverSent(t *testing.T) {
	useTempState(t)
	f := newFakeLand(t, "acme")
	signIn(t, "beta", host, oidclogin.Session{})

	r := run(t, runOpts{}, "--edge", f.srv.URL, "--land", "acme", "budget")
	if r.e == nil || !strings.Contains(r.e.Error(), "not signed in to land acme") {
		t.Fatalf("error = %v", r.e)
	}
	if n := len(f.requests()); n != 0 {
		t.Errorf("the land got %d requests", n)
	}
}

func TestAProblemIsShownWithItsCorrelationId(t *testing.T) {
	useTempState(t)
	f := newFakeLand(t, "acme")
	f.on("GET /tickets/x", func(w http.ResponseWriter, _ *http.Request) { problem(w, t, 403, "Problem/forbidden.json") })
	signIn(t, "acme", host, oidclogin.Session{})

	r := run(t, runOpts{}, "--edge", f.srv.URL, "--land", "acme", "ticket", "show", "x")
	var p landsv1.Problem
	_ = json.Unmarshal(fixture(t, "Problem/forbidden.json"), &p)
	if r.e == nil || !strings.Contains(r.e.Error(), p.Title) || !strings.Contains(r.e.Error(), p.CorrelationId) {
		t.Fatalf("error = %v, want title %q and correlation id %q", r.e, p.Title, p.CorrelationId)
	}
}

func TestContractSkewWarnsOnceAndCarriesOn(t *testing.T) {
	useTempState(t)
	f := newFakeLand(t, "acme")
	f.contract = "1.9.0"
	f.answer("GET /decisions/or-Dcs4nB8vCx", 200, "Decision/open.json")
	f.answer("POST /decisions/or-Dcs4nB8vCx/resolve", 200, "Decision/resolved.json")
	signIn(t, "acme", host, oidclogin.Session{})

	r := run(t, runOpts{}, "--edge", f.srv.URL, "--land", "acme", "decision", "resolve", "or-Dcs4nB8vCx", "--option", "1")
	if r.e != nil {
		t.Fatalf("gl: %v\n%s", r.e, r.err)
	}
	if n := strings.Count(r.err, "1.9.0"); n != 1 {
		t.Errorf("skew warnings = %d, want 1:\n%s", n, r.err)
	}
}

func TestDecisionResolve_TakesAnOptionsIdNumberOrLabel(t *testing.T) {
	for _, opt := range []string{"or-Opt0000002", "2", "wait for the NEW logo"} {
		t.Run(opt, func(t *testing.T) {
			useTempState(t)
			f := newFakeLand(t, "acme")
			f.answer("GET /decisions/or-Dcs4nB8vCx", 200, "Decision/open.json")
			f.answer("POST /decisions/or-Dcs4nB8vCx/resolve", 200, "Decision/resolved.json")
			signIn(t, "acme", host, oidclogin.Session{})

			r := run(t, runOpts{}, "--edge", f.srv.URL, "--land", "acme", "decision", "resolve", "or-Dcs4nB8vCx", "--option", opt, "--note", "logo first")
			if r.e != nil {
				t.Fatalf("gl: %v\n%s", r.e, r.err)
			}
			post := f.requests()[1]
			if post.body != `{"note":"logo first","option_id":"or-Opt0000002"}` {
				t.Errorf("resolve body = %s", post.body)
			}
		})
	}
}

func TestDecisionResolve_AnUnknownOptionListsTheChoicesAndSendsNothing(t *testing.T) {
	useTempState(t)
	f := newFakeLand(t, "acme")
	f.answer("GET /decisions/or-Dcs4nB8vCx", 200, "Decision/open.json")
	signIn(t, "acme", host, oidclogin.Session{})

	r := run(t, runOpts{}, "--edge", f.srv.URL, "--land", "acme", "decision", "resolve", "or-Dcs4nB8vCx", "--option", "9")
	if r.e == nil || !strings.Contains(r.e.Error(), "Wait for the new logo") {
		t.Fatalf("error = %v", r.e)
	}
	if n := len(f.requests()); n != 1 {
		t.Errorf("requests = %d, want only the GET", n)
	}
}

func TestEscalationCreate_PrintsWhereItIsApproved(t *testing.T) {
	useTempState(t)
	f := newFakeLand(t, "acme")
	f.answer("POST /escalations", 201, "Escalation/open.json")
	signIn(t, "acme", host, oidclogin.Session{})
	var e landsv1.Escalation
	_ = json.Unmarshal(fixture(t, "Escalation/open.json"), &e)
	link := "https://web.example.com/garden?bead=" + e.Id

	r := run(t, runOpts{}, "--edge", f.srv.URL, "--land", "acme", "--web", "https://web.example.com", "escalation", "create", "--title", "More credits", "--reason", "the build ran out")
	if r.e != nil {
		t.Fatalf("gl: %v\n%s", r.e, r.err)
	}
	if !strings.Contains(r.out, link) {
		t.Errorf("output has no approval link %s:\n%s", link, r.out)
	}
	if body := f.requests()[0].body; body != `{"reason":"the build ran out","title":"More credits"}` {
		t.Errorf("body = %s", body)
	}

	r = run(t, runOpts{}, "--edge", f.srv.URL, "--land", "acme", "--web", "https://web.example.com", "--json", "escalation", "create", "--title", "t", "--reason", "r")
	var v map[string]any
	if err := json.Unmarshal([]byte(r.out), &v); err != nil || v["approval_url"] != link || v["id"] != e.Id {
		t.Errorf("--json = %s (%v)", r.out, err)
	}
}

// gl approve and reject never approve or reject (P10.B gd-4mEAhIE01U): they
// read the escalation and print its page, sending nothing else.
func TestEscalationApproveReject_HandThePageLink(t *testing.T) {
	for _, verb := range []string{"approve", "reject"} {
		t.Run(verb, func(t *testing.T) {
			useTempState(t)
			f := newFakeLand(t, "acme")
			var e landsv1.Escalation
			_ = json.Unmarshal(fixture(t, "Escalation/open.json"), &e)
			f.answer("GET /escalations/"+e.Id, 200, "Escalation/open.json")
			signIn(t, "acme", host, oidclogin.Session{})

			r := run(t, runOpts{}, "--edge", f.srv.URL, "--land", "acme", "--web", "https://web.example.com", "escalation", verb, e.Id)
			if r.e != nil {
				t.Fatalf("gl: %v\n%s", r.e, r.err)
			}
			if link := "https://web.example.com/garden?bead=" + e.Id; !strings.Contains(r.out, link) {
				t.Errorf("output has no page link %s:\n%s", link, r.out)
			}
			for _, req := range f.requests() {
				if req.method != "GET" {
					t.Errorf("gl %s sent %s %s; it must only read", verb, req.method, req.path)
				}
			}
		})
	}
}

func TestEscalationApprove_AlreadyDecidedSaysSo(t *testing.T) {
	useTempState(t)
	f := newFakeLand(t, "acme")
	f.answer("GET /escalations/x", 200, "Escalation/approved.json")
	signIn(t, "acme", host, oidclogin.Session{})

	r := run(t, runOpts{}, "--edge", f.srv.URL, "--land", "acme", "escalation", "approve", "x")
	if r.e == nil || !strings.Contains(r.e.Error(), "already") {
		t.Errorf("gl: %v\n%s", r.e, r.out)
	}
}

func TestEscalationShow_NoApprovalLinkOnceDecided(t *testing.T) {
	useTempState(t)
	f := newFakeLand(t, "acme")
	f.answer("GET /escalations/x", 200, "Escalation/approved.json")
	signIn(t, "acme", host, oidclogin.Session{})

	r := run(t, runOpts{}, "--edge", f.srv.URL, "--land", "acme", "escalation", "show", "x")
	if r.e != nil || strings.Contains(r.out, "/garden") {
		t.Errorf("gl: %v\n%s", r.e, r.out)
	}
}

func TestThreadShowAndSay(t *testing.T) {
	useTempState(t)
	f := newFakeLand(t, "acme")
	f.answer("GET /threads/th1", 200, "Thread/thread.json")
	f.answer("GET /threads/th1/messages", 200, "MessagePage/oldest_first.json")
	f.answer("POST /threads/th1/messages", 201, "Message/"+firstFile(t, "Message"))
	signIn(t, "acme", host, oidclogin.Session{})

	r := run(t, runOpts{}, "--edge", f.srv.URL, "--land", "acme", "thread", "show", "th1")
	if r.e != nil {
		t.Fatalf("gl: %v\n%s", r.e, r.err)
	}
	var page landsv1.MessagePage
	_ = json.Unmarshal(fixture(t, "MessagePage/oldest_first.json"), &page)
	if !strings.Contains(r.out, page.Items[0].Author) {
		t.Errorf("thread show:\n%s", r.out)
	}

	r = run(t, runOpts{}, "--edge", f.srv.URL, "--land", "acme", "say", "th1", "hello", "there")
	if r.e != nil {
		t.Fatalf("gl say: %v\n%s", r.e, r.err)
	}
	if body := f.requests()[2].body; body != `{"text":"hello there"}` {
		t.Errorf("say body = %s", body)
	}
}

func TestBudget(t *testing.T) {
	useTempState(t)
	f := newFakeLand(t, "acme")
	f.answer("GET /budget", 200, "Budget/ok.json")
	signIn(t, "acme", host, oidclogin.Session{})

	r := run(t, runOpts{}, "--edge", f.srv.URL, "--land", "acme", "budget")
	var b landsv1.Budget
	_ = json.Unmarshal(fixture(t, "Budget/ok.json"), &b)
	want := "Spent " + amount(b.Spent) + " of " + amount(b.Limit) + " credits (ok)."
	if r.e != nil || strings.TrimSpace(r.out) != want {
		t.Errorf("gl budget = %q (%v), want %q", r.out, r.e, want)
	}
}

// gl budget --history asks for the days and prints one line a day, oldest
// first; a stale history says so; --json is the contract object; --days is
// checked before any call.
func TestBudgetHistory(t *testing.T) {
	useTempState(t)
	f := newFakeLand(t, "acme")
	f.answer("GET /budget/history", 200, "BudgetHistory/series.json")
	signIn(t, "acme", host, oidclogin.Session{})

	r := run(t, runOpts{}, "--edge", f.srv.URL, "--land", "acme", "budget", "--history", "--days", "3")
	want := "DAY         SPENT (credits)  REMAINING (credits)\n" +
		"2026-10-25  12.5             387.5\n" +
		"2026-10-26  40.25            347.25\n" +
		"2026-10-27  3                344.25\n"
	if r.e != nil || r.out != want {
		t.Errorf("gl budget --history = %q (%v), want %q", r.out, r.e, want)
	}
	reqs := f.requests()
	if last := reqs[len(reqs)-1]; last.path != "/land/acme/v1/lands/v1/lands/acme/budget/history" || last.query != "days=3" {
		t.Errorf("request = %+v", last)
	}

	r = run(t, runOpts{}, "--edge", f.srv.URL, "--land", "acme", "--json", "budget", "--history")
	var h landsv1.BudgetHistory
	if err := json.Unmarshal([]byte(r.out), &h); r.e != nil || err != nil || len(h.Points) != 3 || h.Stale != nil {
		t.Errorf("gl --json budget --history = %q (%v)", r.out, r.e)
	}
	if last := f.requests()[len(f.requests())-1]; last.query != "days=30" {
		t.Errorf("default days: %+v", last)
	}

	f.answer("GET /budget/history", 200, "BudgetHistory/stale.json")
	r = run(t, runOpts{}, "--edge", f.srv.URL, "--land", "acme", "budget", "--history")
	if r.e != nil || !strings.Contains(r.out, "2026-10-27") || !strings.Contains(r.out, "Stale: the credit series could not be read") {
		t.Errorf("stale history = %q (%v)", r.out, r.e)
	}
	f.answer("GET /budget/history", 200, "BudgetHistory/new_land.json")
	r = run(t, runOpts{}, "--edge", f.srv.URL, "--land", "acme", "budget", "--history")
	if r.e != nil || strings.TrimSpace(r.out) != "No credit history yet." {
		t.Errorf("new land = %q (%v)", r.out, r.e)
	}

	n := len(f.requests())
	for _, args := range [][]string{{"budget", "--history", "--days", "0"}, {"budget", "--history", "--days", "31"}, {"budget", "--days", "7"}} {
		if r := run(t, runOpts{}, append([]string{"--edge", f.srv.URL, "--land", "acme"}, args...)...); r.e == nil {
			t.Errorf("gl %v: no error", args)
		}
	}
	if len(f.requests()) != n {
		t.Errorf("a refused --days still called the land")
	}
}

func TestPlanShowAndAnswer(t *testing.T) {
	useTempState(t)
	f := newFakeLand(t, "acme")
	f.answer("GET /plans/p1", 200, "Plan/building_phased.json")
	f.answer("GET /decisions/or-Dcs4nB8vCx", 200, "Decision/open.json")
	f.answer("POST /plans/p1/answer", 200, "Decision/resolved.json")
	signIn(t, "acme", host, oidclogin.Session{})

	r := run(t, runOpts{}, "--edge", f.srv.URL, "--land", "acme", "plan", "show", "p1")
	if r.e != nil || !strings.Contains(r.out, "Phase:") {
		t.Errorf("plan show: %v\n%s", r.e, r.out)
	}
	r = run(t, runOpts{}, "--edge", f.srv.URL, "--land", "acme", "plan", "answer", "p1", "--decision", "or-Dcs4nB8vCx", "--option", "1")
	if r.e != nil {
		t.Fatalf("plan answer: %v\n%s", r.e, r.err)
	}
	if body := f.requests()[2].body; body != `{"decision_id":"or-Dcs4nB8vCx","option_id":"or-Opt0000001"}` {
		t.Errorf("answer body = %s", body)
	}
}

func TestWhoami(t *testing.T) {
	useTempState(t)
	f := newFakeLand(t, "acme")
	f.answer("GET /whoami", 200, "Member/owner.json")
	signIn(t, "acme", host, oidclogin.Session{AccessToken: fakeJWT(map[string]any{"email": "ada@example.com"})})

	r := run(t, runOpts{}, "--edge", f.srv.URL, "--land", "acme", "--json", "whoami")
	if r.e != nil {
		t.Fatalf("gl: %v\n%s", r.e, r.err)
	}
	var v whoamiView
	if err := json.Unmarshal([]byte(r.out), &v); err != nil {
		t.Fatal(err)
	}
	if v.Land != "acme" || v.Member == nil || v.Member.Role != landsv1.MemberRoleOwner || v.ContractLand != landsv1.ContractVersion || v.SupportAsTenant {
		t.Errorf("whoami = %+v", v)
	}
	r = run(t, runOpts{}, "--edge", f.srv.URL, "--land", "acme", "whoami")
	if !strings.Contains(r.out, "Signed in: ada@example.com") {
		t.Errorf("whoami:\n%s", r.out)
	}
}

func TestAsTenant_ShowsTheSupportBannerOnceAndUsesThatLand(t *testing.T) {
	useTempState(t)
	f := newFakeLand(t, "acme")
	f.answer("GET /decisions/or-Dcs4nB8vCx", 200, "Decision/open.json")
	f.answer("POST /decisions/or-Dcs4nB8vCx/resolve", 200, "Decision/resolved.json")
	signIn(t, "acme", host, oidclogin.Session{AccessToken: fakeJWT(map[string]any{"email": "op@example.com"})})

	r := run(t, runOpts{env: map[string]string{"GL_LAND": "beta"}}, "--edge", f.srv.URL, "--as-tenant", "acme", "decision", "resolve", "or-Dcs4nB8vCx", "--option", "1")
	if r.e != nil {
		t.Fatalf("gl: %v\n%s", r.e, r.err)
	}
	if n := strings.Count(r.err, "support view of land acme as op@example.com"); n != 1 {
		t.Errorf("banners = %d:\n%s", n, r.err)
	}
	if len(f.requests()) != 2 {
		t.Errorf("requests = %d", len(f.requests()))
	}

	r = run(t, runOpts{}, "--edge", f.srv.URL, "--as-tenant", "acme", "--land", "beta", "budget")
	if r.e == nil || !strings.Contains(r.e.Error(), "different lands") {
		t.Errorf("--as-tenant with another --land: %v", r.e)
	}

	r = run(t, runOpts{}, "--edge", f.srv.URL, "--as-tenant", "zed", "budget")
	if r.e == nil || !strings.Contains(r.e.Error(), "gl --as-tenant zed login") {
		t.Errorf("not signed in: %v", r.e)
	}
}

func TestLandIsChosenByFlagThenEnvThenUseThenTheOnlySignIn(t *testing.T) {
	useTempState(t)
	a := newApp(&strings.Builder{}, &strings.Builder{})
	env := map[string]string{}
	a.getenv = func(k string) string { return env[k] }
	land := func() string {
		t.Helper()
		a.cfg = nil
		l, err := a.land()
		if err != nil {
			return "error: " + err.Error()
		}
		return l
	}

	if got := land(); !strings.Contains(got, "gl login --land") {
		t.Errorf("nothing anywhere: %q", got)
	}
	signIn(t, "solo", host, oidclogin.Session{})
	if got := land(); got != "solo" {
		t.Errorf("one sign-in: %q", got)
	}
	signIn(t, "duo", host, oidclogin.Session{})
	if got := land(); !strings.Contains(got, "duo, solo") {
		t.Errorf("two sign-ins: %q", got)
	}
	if err := a.saveConfig(&config{Land: "used"}); err != nil {
		t.Fatal(err)
	}
	if got := land(); got != "used" {
		t.Errorf("gl use: %q", got)
	}
	env["GL_LAND"] = "fromenv"
	if got := land(); got != "fromenv" {
		t.Errorf("GL_LAND: %q", got)
	}
	a.landFlag = "flagged"
	if got := land(); got != "flagged" {
		t.Errorf("--land: %q", got)
	}
	a.landFlag = "Not_A_Land"
	if got := land(); !strings.Contains(got, "not a land name") {
		t.Errorf("bad name: %q", got)
	}
}

func TestUse_RemembersTheLandAndAddresses(t *testing.T) {
	useTempState(t)
	r := run(t, runOpts{}, "--edge", "https://edge.example.com/", "--web", "https://web.example.com", "use", "acme")
	if r.e != nil {
		t.Fatalf("gl use: %v", r.e)
	}
	if !strings.Contains(r.out, "not signed in to it yet") {
		t.Errorf("gl use:\n%s", r.out)
	}
	path, _ := configPath()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var c config
	_ = json.Unmarshal(data, &c)
	if c != (config{Edge: "https://edge.example.com", Land: "acme", Web: "https://web.example.com"}) {
		t.Errorf("config = %+v", c)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("config mode = %v", fi.Mode().Perm())
	}
	if r := run(t, runOpts{}, "use", "Bad"); r.e == nil {
		t.Error("gl use accepted a bad land name")
	}
}

func TestCheckEdge(t *testing.T) {
	for in, want := range map[string]string{
		"https://api.clonk.tv":   "https://api.clonk.tv",
		"https://api.clonk.tv/":  "https://api.clonk.tv",
		"http://127.0.0.1:8080":  "http://127.0.0.1:8080",
		"http://localhost:1":     "http://localhost:1",
		"http://api.clonk.tv":    "",
		"https://api.clonk.tv/x": "",
		"ftp://api.clonk.tv":     "",
		"api.clonk.tv":           "",
		"https://u:p@h.example":  "",
	} {
		got, err := checkEdge(in)
		if want == "" {
			if err == nil {
				t.Errorf("checkEdge(%q) = %q, want an error", in, got)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("checkEdge(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestLogin_SignsInForTheOneLandAndMakesItTheDefault(t *testing.T) {
	useTempState(t)
	k := newFakeKeycloak(t)

	r := run(t, runOpts{}, "--edge", "https://api.example.com", "--issuer", k.srv.URL, "login", "--land", "acme", "--no-browser")
	if r.e != nil {
		t.Fatalf("gl login: %v\n%s", r.e, r.err)
	}
	if k.deviceScope != oidclogin.ScopeFor("acme") {
		t.Errorf("scope = %q, want %q", k.deviceScope, oidclogin.ScopeFor("acme"))
	}
	s, _ := oidclogin.LoadFor("acme")
	if s == nil || s.Land != "acme" || s.Daemon != "api.example.com" || s.Issuer != k.srv.URL {
		t.Fatalf("session = %+v", s)
	}
	if !strings.Contains(r.out, "Signed in as ada@example.com to land acme.") || !strings.Contains(r.err, "ABCD-EFGH") {
		t.Errorf("out:\n%s\nerr:\n%s", r.out, r.err)
	}
	a := newApp(nil, nil)
	if c, _ := a.config(); c.Land != "acme" {
		t.Errorf("default land = %q", c.Land)
	}

	// A second land does not move the default, and a support sign-in never does.
	if r := run(t, runOpts{}, "--edge", "https://api.example.com", "--issuer", k.srv.URL, "login", "--land", "beta", "--no-browser"); r.e != nil {
		t.Fatal(r.e)
	}
	if r := run(t, runOpts{}, "--edge", "https://api.example.com", "--issuer", k.srv.URL, "--as-tenant", "zed", "login", "--no-browser"); r.e != nil {
		t.Fatal(r.e)
	}
	a = newApp(nil, nil)
	if c, _ := a.config(); c.Land != "acme" {
		t.Errorf("default land moved to %q", c.Land)
	}
	if lands, _ := signedInLands(); strings.Join(lands, ",") != "acme,beta,zed" {
		t.Errorf("signed in to %v", lands)
	}

	r = run(t, runOpts{}, "--land", "beta", "logout")
	if r.e != nil || !strings.Contains(r.out, "Signed out of land beta.") {
		t.Errorf("logout: %v %s", r.e, r.out)
	}
	if s, _ := oidclogin.LoadFor("beta"); s != nil {
		t.Errorf("beta still signed in")
	}
}

func TestBearer_UsesTheCachedTokenWhenTheSignInServiceIsDown(t *testing.T) {
	useTempState(t)
	k := newFakeKeycloak(t)
	k.refreshReply = func(w http.ResponseWriter) { w.WriteHeader(http.StatusBadGateway) }
	// Inside the renewal skew but not yet lapsed.
	signIn(t, "acme", host, oidclogin.Session{Issuer: k.srv.URL, AccessToken: "still-good", RefreshToken: "refresh-1", Expiry: time.Now().Add(10 * time.Second)})
	a := newApp(&strings.Builder{}, &strings.Builder{})
	tok, err := a.bearer(t.Context(), "http://"+host, "acme", false)
	if err != nil || tok != "still-good" {
		t.Errorf("bearer = %q, %v", tok, err)
	}
	if _, err := a.bearer(t.Context(), "http://"+host, "acme", true); err == nil || !strings.Contains(err.Error(), "try again") {
		t.Errorf("forced renewal with the service down: %v", err)
	}
}

func firstFile(t *testing.T, dir string) string {
	t.Helper()
	ents, err := os.ReadDir("../contract/fixtures/" + dir)
	if err != nil || len(ents) == 0 {
		t.Fatalf("fixtures/%s: %v", dir, err)
	}
	return ents[0].Name()
}
