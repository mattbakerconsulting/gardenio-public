package gardenhub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A tenant land's credit charts (GET /garden/charts/costs-land, plan gd-gh5J0vVbJP step 3.3): the scope is checked
// before any read, a land outside it answers exactly as one that does not exist, and no answer carries a dollar figure
// or a URL.

type fakeCredits struct {
	calls []string
	h     CreditHistory
	err   error
}

func (f *fakeCredits) LandCredits(_ context.Context, land string, w CreditWindow) (CreditHistory, error) {
	f.calls = append(f.calls, land+" "+w.Name)
	return f.h, f.err
}

// landCreditsFarm has acme, a tenant land with its purse at cost too (as the farm holds it), gardenio, an operator
// land with no purse, and home, an operator land told as an unmetered purse.
func landCreditsFarm() *Farm {
	return &Farm{cost: []byte(`{"projects":[{"project":"gardenio","today_usd":3},` +
		`{"project":"acme","today_usd":7,"purse":{"land":"acme","state":"ok","granted_credits":100,"spent_credits":60,` +
		`"in_flight_credits":2,"remaining_credits":38,"margin_credits":5,"used_pct":62,"llm_multiplier":1.5,` +
		`"granted_usd":66.67,"spent_usd":40,"remaining_usd":25.33,"read_at":"2026-11-01T07:00:00Z"}},` +
		`{"project":"home","purse":{"land":"home","state":"unmetered"}}]}`)}
}

func landCreditsHandler(src LandCreditSource, allow ...string) *Handler {
	return &Handler{Farm: landCreditsFarm(), Credits: src, InLand: func(_ *http.Request, land string) bool {
		for _, a := range allow {
			if a == land {
				return true
			}
		}
		return false
	}}
}

func getRaw(t *testing.T, h *Handler, path string) (int, string) {
	t.Helper()
	mux := http.NewServeMux()
	h.Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	return rec.Code, rec.Body.String()
}

// Outside the scope, not a land, an operator land, a name of the wrong shape: each a 404 with the same body, and the
// history is never read. With no InLand at all every land is refused.
func TestLandCostsRefusesBeforeAnyRead(t *testing.T) {
	src := &fakeCredits{h: CreditHistory{Recorded: true}}
	h := landCreditsHandler(src, "gardenio", "home", "nope")
	_, want := getRaw(t, h, "/garden/charts/costs-land?land=nope")
	for _, land := range []string{"acme", "nope", "gardenio", "home", "", "ACME", "acme%22%7D", "a..b"} {
		code, body := getRaw(t, h, "/garden/charts/costs-land?land="+land+"&range=7d")
		if code != http.StatusNotFound || body != want {
			t.Errorf("land %q: %d %q, want 404 %q", land, code, body, want)
		}
	}
	if code, _ := getRaw(t, &Handler{Farm: landCreditsFarm(), Credits: src}, "/garden/charts/costs-land?land=acme"); code != http.StatusNotFound {
		t.Errorf("no InLand: %d, want 404", code)
	}
	if len(src.calls) != 0 {
		t.Errorf("the history was read for a refused land: %v", src.calls)
	}
	// the admin-only route by its set name is untouched: costs-land is its own route, never a registry set
	if code, _ := getRaw(t, h, "/garden/charts/costs-land?land=acme"); code != http.StatusNotFound {
		t.Errorf("acme outside the scope: %d", code)
	}
}

func TestLandCostsRange(t *testing.T) {
	src := &fakeCredits{h: CreditHistory{Recorded: true}}
	h := landCreditsHandler(src, "acme")
	if code, _ := getRaw(t, h, "/garden/charts/costs-land?land=acme&range=1y"); code != http.StatusBadRequest {
		t.Errorf("range 1y: %d, want 400", code)
	}
	for _, r := range []string{"today", "7d", "30d", ""} {
		code, doc := getQueue(t, h, "/garden/charts/costs-land?land=acme&range="+r)
		want := r
		if r == "" {
			want = "7d"
		}
		if code != http.StatusOK || doc["range"] != want {
			t.Errorf("range %q: %d %v", r, code, doc["range"])
		}
	}
	if strings.Join(src.calls, ",") != "acme today,acme 7d,acme 30d,acme 7d" {
		t.Errorf("history reads: %v", src.calls)
	}
}

// keysOf walks a decoded JSON value and calls fn for every object key and every string.
func keysOf(v any, fn func(key, str string)) {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			fn(k, "")
			keysOf(e, fn)
		}
	case []any:
		for _, e := range x {
			keysOf(e, fn)
		}
	case string:
		fn("", x)
	}
}

// The land's own figures: the purse in credits from the farm, the tiles, the burn line and its run-out, the agents
// ranked with the rest summed; and nothing at cost, no rate and no URL anywhere in it.
func TestLandCostsAnswer(t *testing.T) {
	agents := []AgentCredits{{"z-idle", 0}}
	for i := 1; i <= 12; i++ {
		agents = append(agents, AgentCredits{fmt.Sprintf("a%02d", i), float64(i)})
	}
	src := &fakeCredits{h: CreditHistory{
		Recorded: true, Today: f64(1.5), Week: f64(20), Month: f64(60), RatePerHour: f64(0.5),
		Remaining: []CreditPoint{{1, 50}, {2, 40}}, Agents: agents,
	}}
	h := landCreditsHandler(src, "acme")
	code, body := getRaw(t, h, "/garden/charts/costs-land?land=acme&range=30d")
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, body)
	}
	var d landCosts
	if err := json.Unmarshal([]byte(body), &d); err != nil {
		t.Fatal(err)
	}
	if d.Land != "acme" || d.History != creditHistoryOK || d.Purse.Granted != 100 || !eqF(d.Purse.Remaining, f64(38)) || d.Purse.Margin != 5 {
		t.Errorf("doc: %+v", d)
	}
	if d.Spent == nil || !eqF(d.Spent.Today, f64(1.5)) || !eqF(d.Spent.Week, f64(20)) || !eqF(d.Spent.Month, f64(60)) {
		t.Errorf("spent: %+v", d.Spent)
	}
	// 38 remaining, the wall at 5, 0.5 an hour: 66 hours after the window's end
	if d.Burn.RunOut == nil || *d.Burn.RunOut != d.Burn.End+66*3600 || d.Burn.Wall != 5 || len(d.Burn.Points) != 2 || d.Burn.End-d.Burn.Start != 30*86400 {
		t.Errorf("burn: %+v", d.Burn)
	}
	if len(d.Agents) != landAgentsTop || d.Agents[0].Agent != "a12" || d.Agents[9].Agent != "a03" || d.Rest == nil || d.Rest.Count != 2 || d.Rest.Credits != 3 {
		t.Errorf("agents: %+v rest %+v", d.Agents, d.Rest)
	}
	var generic any
	_ = json.Unmarshal([]byte(body), &generic)
	keysOf(generic, func(k, s string) {
		if strings.HasSuffix(k, "_usd") || k == "llm_multiplier" || k == "src" || k == "open_url" || strings.Contains(k, "dashboard") {
			t.Errorf("tenant answer carries key %q", k)
		}
		if strings.Contains(s, "://") {
			t.Errorf("tenant answer carries a URL %q", s)
		}
	})
}

// The degraded states: no history source or a failed read is unavailable, a land with no series yet is none; both
// still carry the purse, with no tiles, no line and no agents.
func TestLandCostsDegraded(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  LandCreditSource
		want string
	}{
		{"no source", nil, creditHistoryUnavailable},
		{"read failed", &fakeCredits{err: ErrCreditsUnavailable}, creditHistoryUnavailable},
		{"no series yet", &fakeCredits{}, creditHistoryNone},
	} {
		code, body := getRaw(t, landCreditsHandler(tc.src, "acme"), "/garden/charts/costs-land?land=acme")
		var d landCosts
		_ = json.Unmarshal([]byte(body), &d)
		if code != http.StatusOK || d.History != tc.want || d.Spent != nil || d.Burn.RunOut != nil || len(d.Burn.Points) != 0 || len(d.Agents) != 0 || d.Purse.State != "ok" || d.Purse.Granted != 100 {
			t.Errorf("%s: %d %s", tc.name, code, body)
		}
		if !strings.Contains(body, `"spent":null`) || !strings.Contains(body, `"points":[]`) || !strings.Contains(body, `"agents":[]`) {
			t.Errorf("%s: want null spent and empty lists, got %s", tc.name, body)
		}
	}
}

func TestRunOut(t *testing.T) {
	now := time.Unix(1000, 0)
	at := func(p *int64) string {
		if p == nil {
			return "nil"
		}
		return fmt.Sprint(*p - 1000)
	}
	for _, tc := range []struct {
		rem, rate *float64
		want      string
	}{
		{nil, f64(1), "nil"},
		{f64(10), nil, "nil"},
		{f64(10), f64(0), "nil"},
		{f64(10), f64(-1), "nil"},
		{f64(4), f64(1), "0"},       // past the wall already
		{f64(7), f64(2), "3600"},    // 2 above the wall at 2 an hour
		{f64(5e6), f64(0.1), "nil"}, // decades away
	} {
		if got := at(runOut(tc.rem, 5, tc.rate, now)); got != tc.want {
			t.Errorf("runOut(%v, %v) = %s, want %s", tc.rem, tc.rate, got, tc.want)
		}
	}
}

func TestCreditWindow(t *testing.T) {
	now := time.Date(2026, 11, 1, 7, 30, 0, 0, time.UTC)
	if w := creditWindow("today", now); !w.Start.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) || w.Step != 5*time.Minute {
		t.Errorf("today: %+v", w)
	}
	if w := creditWindow("today", now.Truncate(24*time.Hour)); !w.Start.Before(w.End) {
		t.Errorf("today at midnight: an empty window %+v", w)
	}
	for name, n := range map[string]int{"today": 90, "7d": 168, "30d": 120} {
		w := creditWindow(name, now)
		if got := int(w.End.Sub(w.Start) / w.Step); got != n || got > 288 {
			t.Errorf("%s: %d steps, want %d", name, got, n)
		}
	}
}
