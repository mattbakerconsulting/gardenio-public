package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/landmap"
	"gardenio/kbeads/internal/model"
)

// An update to a land's own lands-<land> account bead is a purse change;
// any other account, a bead of another type with that title, an account
// labelled with another land and a create stay what their topic says.
func TestChangesForMap_PurseWrite(t *testing.T) {
	hub := newSSEHub()
	var got []landmap.ChangeKind
	changesForMap{hub}.Tap(func(c landmap.Change) { got = append(got, c.Kind) })
	acme := []string{"project:acme"}
	payload := func(typ, title string) []byte {
		b, _ := json.Marshal(events.BeadUpdated{Bead: &model.Bead{ID: "gd-a", Type: model.BeadType(typ), Title: title}})
		return b
	}
	hub.broadcast(events.TopicBeadUpdated, payload("account", "lands-acme"), "gd-a", "", "", acme, nil)
	hub.broadcast(events.TopicBeadUpdated, payload("account", "lands-zeta"), "gd-a", "", "", acme, nil)
	hub.broadcast(events.TopicBeadUpdated, payload("account", "acme"), "gd-a", "", "", acme, nil)
	hub.broadcast(events.TopicBeadUpdated, payload("task", "lands-acme"), "gd-a", "", "", acme, nil)
	hub.broadcast(events.TopicBeadUpdated, payload("account", "lands-acme"), "gd-a", "", "", []string{"project:zeta"}, nil)
	hub.broadcast(events.TopicBeadCreated, payload("account", "lands-acme"), "gd-a", "", "", acme, nil)
	hub.broadcast(events.TopicBeadUpdated, []byte(`{"bead":{"type":"account"`), "gd-a", "", "", acme, nil)
	want := []landmap.ChangeKind{landmap.ChangePurse, landmap.ChangeUpdated, landmap.ChangeUpdated, landmap.ChangeUpdated,
		landmap.ChangeUpdated, landmap.ChangeCreated, landmap.ChangeUpdated}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("kinds = %v, want %v", got, want)
	}
}

func fig(v float64) *float64 { return &v }

// The ledger: each day closes on its last snapshot, and spends the rise in
// used from the day before's close (its own first snapshot, for the first
// day); a fall spends nothing; a snapshot with no used figure, or read out
// of order, is skipped; days before first are dropped.
func TestLedgerDays(t *testing.T) {
	at := func(day, hour int) time.Time { return time.Date(2026, 10, day, hour, 0, 0, 0, time.UTC) }
	snap := func(t time.Time, granted float64, used *float64) landmap.BudgetRecord {
		return landmap.BudgetRecord{Granted: fig(granted), Used: used, State: "ok", ReadAt: t}
	}
	got := ledgerDays([]landmap.BudgetRecord{
		snap(at(20, 9), 100, fig(1)),
		snap(at(22, 9), 100, fig(10)),
		snap(at(22, 18), 100, fig(15)),
		snap(at(22, 19), 100, nil),
		snap(at(23, 1), 150, fig(20)),
		snap(at(22, 23), 150, fig(99)),
		snap(at(23, 8), 150, fig(18)),
		{Granted: fig(150), Used: fig(30)},
		snap(at(24, 3), 150, fig(18)),
	}, at(21, 0))
	want := []landmap.BudgetDayRecord{
		{Day: at(22, 0), Spent: 14, Remaining: 85},
		{Day: at(23, 0), Spent: 3, Remaining: 132},
		{Day: at(24, 0), Spent: 0, Remaining: 132},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("ledgerDays =\n %v\nwant\n %v", got, want)
	}
	if got := ledgerDays([]landmap.BudgetRecord{snap(at(20, 9), 100, fig(1)), snap(at(20, 12), 100, fig(4))}, at(20, 0)); len(got) != 1 || got[0].Spent != 3 {
		t.Errorf("one day, no day before: %v", got)
	}
	if got := ledgerDays(nil, at(20, 0)); len(got) != 0 {
		t.Errorf("no snapshots: %v", got)
	}
}

// creditsVM answers the credit series queries for one land: a range query with
// one point per 00:00Z it is asked for, and an instant query with today's.
type creditsVM struct {
	mu      sync.Mutex
	queries []string
	fail    bool
	empty   bool
}

func (v *creditsVM) serve(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	q := r.Form.Get("query")
	v.mu.Lock()
	v.queries = append(v.queries, q)
	fail, empty := v.fail, v.empty
	v.mu.Unlock()
	if fail {
		http.Error(w, "vmselect down", http.StatusBadGateway)
		return
	}
	value := 7.0
	if strings.Contains(q, creditsRemainingSeries) {
		value = 400
	}
	result := "[]"
	if !empty {
		if strings.HasSuffix(r.URL.Path, "/query_range") {
			start, _ := strconv.ParseInt(r.Form.Get("start"), 10, 64)
			end, _ := strconv.ParseInt(r.Form.Get("end"), 10, 64)
			var pts []string
			for ts := start; ts <= end; ts += 86400 {
				pts = append(pts, fmt.Sprintf(`[%d,"%g"]`, ts, value+float64((end-ts)/86400)))
			}
			result = `[{"metric":{"land":"claimlayer"},"values":[` + strings.Join(pts, ",") + `]}]`
		} else {
			result = fmt.Sprintf(`[{"metric":{"land":"claimlayer"},"value":[%d,"%g"]}]`, time.Now().Unix(), value/2)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"status":"success","data":{"resultType":"x","result":%s}}`, result)
}

// TestLandsAPI_BudgetHistory: the history is the credit series when
// VictoriaMetrics answers, read with the land as an exact matcher; the purse
// snapshots recorded on the account bead, stale, when it is not wired, fails
// or has nothing for the land; an operator land and another land have none.
func TestLandsAPI_BudgetHistory(t *testing.T) {
	f := newScopedFixture(t)
	f.seedLandProject(t, "false")
	get := func(q string) (int, string) {
		code, _, body := f.landsDo(t, tokScopedWriter, http.MethodGet, "/claimlayer/budget/history"+q, nil)
		return code, body
	}

	// No account bead yet, no series: nothing recorded.
	if code, body := get(""); code != http.StatusOK || strings.TrimSpace(body) != `{"points":[],"stale":true,"unit":"credits"}` {
		t.Errorf("no account: %d %s", code, body)
	}

	f.seedTyped("acct-cl", "account", "")
	acct := f.ms.beads["acct-cl"]
	acct.Title, acct.Kind = "lands-claimlayer", "config"
	now := time.Now().UTC()
	today := now.Truncate(24 * time.Hour)
	for i, s := range []struct {
		at   time.Time
		used string
	}{
		{today.Add(-47 * time.Hour), "10"},
		{today.Add(-30 * time.Hour), "12.5"},
		{today.Add(-2 * time.Hour), "20"},
		{today.Add(time.Minute), "21"},
	} {
		fields, _ := json.Marshal(map[string]string{
			"purse_granted_credits": "100", "purse_used_credits": s.used, "purse_state": "ok",
			"purse_read_at": s.at.Format(time.RFC3339), "credential": "sk-live-not-for-tenants",
		})
		payload, _ := json.Marshal(events.BeadUpdated{Bead: &model.Bead{ID: "acct-cl", Type: model.TypeAccount, Title: acct.Title, Fields: fields}})
		f.ms.events = append(f.ms.events, &model.Event{ID: int64(100 + i), Topic: events.TopicBeadUpdated, BeadID: "acct-cl", Payload: payload, CreatedAt: s.at})
	}
	day := func(n int) string { return today.AddDate(0, 0, -n).Format("2006-01-02") }
	ledger := fmt.Sprintf(`{"points":[{"day":%q,"remaining":87.5,"spent":2.5},{"day":%q,"remaining":80,"spent":7.5},{"day":%q,"remaining":79,"spent":1}],"stale":true,"unit":"credits"}`,
		day(2), day(1), day(0))
	if code, body := get(""); code != http.StatusOK || strings.TrimSpace(body) != ledger {
		t.Errorf("ledger: %d %s\nwant %s", code, body, ledger)
	}
	if code, body := get("?days=1"); code != http.StatusOK || !strings.Contains(body, `"points":[{"day":"`+day(0)+`"`) || strings.Contains(body, day(1)) {
		t.Errorf("ledger, days=1: %d %s", code, body)
	}

	vm := &creditsVM{}
	ts := httptest.NewServer(http.HandlerFunc(vm.serve))
	t.Cleanup(ts.Close)
	f.srv.SetVMClient(NewVMClient(ts.URL, ""))
	code, body := get("?days=3")
	series := fmt.Sprintf(`{"points":[{"day":%q,"remaining":401,"spent":8},{"day":%q,"remaining":400,"spent":7},{"day":%q,"remaining":200,"spent":3.5}],"unit":"credits"}`,
		day(2), day(1), day(0))
	if code != http.StatusOK || strings.TrimSpace(body) != series {
		t.Errorf("series: %d %s\nwant %s", code, body, series)
	}
	vm.mu.Lock()
	for _, q := range vm.queries {
		if !strings.Contains(q, `{land="claimlayer"}`) || strings.Contains(q, "=~") || strings.Contains(q, "usd") {
			t.Errorf("query %q: want the credit series with an exact land matcher", q)
		}
	}
	if len(vm.queries) != 4 {
		t.Errorf("queries = %q, want two range and two instant", vm.queries)
	}
	vm.queries, vm.fail = nil, true
	vm.mu.Unlock()
	if code, body := get(""); code != http.StatusOK || strings.TrimSpace(body) != ledger {
		t.Errorf("vm down: %d %s, want the ledger", code, body)
	}
	vm.mu.Lock()
	vm.fail, vm.empty = false, true
	vm.mu.Unlock()
	if code, body := get(""); code != http.StatusOK || strings.TrimSpace(body) != ledger {
		t.Errorf("no series for the land: %d %s, want the ledger", code, body)
	}

	code, _, body = f.landsDo(t, tokScopedWriter, http.MethodGet, "/other/budget/history", nil)
	assertLandsProblem(t, "other land", code, body, http.StatusNotFound, "not_found")
	f.ms.beads["proj-claimlayer"].Fields = []byte(`{"land_kind":"operator"}`)
	code, body = get("")
	assertLandsProblem(t, "operator land", code, body, http.StatusNotFound, "not_found")
}

// The series names are two constants here and two record: lines in the chart's
// 3.2 rules; a name only one side knows makes every land's history read stale.
func TestBudgetHistorySeries_RecordedByChart(t *testing.T) {
	tpl, err := os.ReadFile("../../../gardenio/helm/gardenio/templates/_lands-credits.tpl")
	if err != nil {
		t.Fatalf("read chart rules: %v", err)
	}
	for _, series := range []string{creditsTodaySeries, creditsRemainingSeries} {
		if !strings.Contains(string(tpl), "- record: "+series+"\n") {
			t.Errorf("%s is not recorded by templates/_lands-credits.tpl", series)
		}
	}
}
