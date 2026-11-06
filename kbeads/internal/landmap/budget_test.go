package landmap

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"gardenio/lands/contract/landsv1"
)

// The history is the source's days in credits, oldest first, within the
// days asked for; stale only when the source says so; never below zero.
// days outside 1-30 is invalid_request; no source is upstream_unavailable;
// an operator land and another land have none.
func TestBudgetHistory(t *testing.T) {
	today := time.Now().UTC().Truncate(24 * time.Hour)
	ago := func(n int) time.Time { return today.AddDate(0, 0, -n) }
	rec := BudgetHistoryRecord{Days: []BudgetDayRecord{
		{Day: ago(0), Spent: 3, Remaining: 344.25},
		{Day: ago(40), Spent: 1, Remaining: 500},
		{Day: ago(2), Spent: 12.5, Remaining: 387.5},
		{Day: ago(1), Spent: 40.25, Remaining: -2},
	}}
	_, h := newMapFixture(t, fakeLands{history: rec})
	r := do(t, h, "GET", "/acme/budget/history", "")
	got := decodeAs[landsv1.BudgetHistory](t, r, http.StatusOK)
	day := func(t time.Time) string { return t.Format("2006-01-02") }
	want := fmt.Sprintf(`{"points":[{"day":%q,"remaining":387.5,"spent":12.5},{"day":%q,"remaining":0,"spent":40.25},{"day":%q,"remaining":344.25,"spent":3}],"unit":"credits"}`,
		day(ago(2)), day(ago(1)), day(ago(0)))
	if strings.TrimSpace(r.body) != want || got.Stale != nil {
		t.Errorf("history = %s\nwant      %s", r.body, want)
	}
	r = do(t, h, "GET", "/acme/budget/history?days=2", "")
	if got := decodeAs[landsv1.BudgetHistory](t, r, http.StatusOK); len(got.Points) != 2 || got.Points[0].Day.Time != ago(1) {
		t.Errorf("days=2: %s", r.body)
	}

	_, h = newMapFixture(t, fakeLands{history: BudgetHistoryRecord{Stale: true}})
	r = do(t, h, "GET", "/acme/budget/history", "")
	if strings.TrimSpace(r.body) != `{"points":[],"stale":true,"unit":"credits"}` {
		t.Errorf("stale, no days: %s", r.body)
	}

	for _, q := range []string{"days=0", "days=31", "days=-1"} {
		if p := problemOf(t, do(t, h, "GET", "/acme/budget/history?"+q, ""), landsv1.InvalidRequest); p.Detail == nil {
			t.Errorf("%s: no detail", q)
		}
	}
	problemOf(t, do(t, h, "GET", "/acme/budget/history?days=many", ""), landsv1.InvalidRequest)

	_, h = newMapFixture(t, fakeLands{historyErr: fmt.Errorf("vm: connection refused; ledger: dial tcp 10.0.0.1")})
	if p := problemOf(t, do(t, h, "GET", "/acme/budget/history", ""), landsv1.UpstreamUnavailable); p.Detail != nil {
		t.Errorf("upstream detail %q", *p.Detail)
	}
	_, h = newMapFixture(t, fakeLands{noPurse: true})
	problemOf(t, do(t, h, "GET", "/acme/budget/history", ""), landsv1.NotFound)
	problemOf(t, do(t, h, "GET", "/zeta/budget/history", ""), landsv1.NotFound)
}
