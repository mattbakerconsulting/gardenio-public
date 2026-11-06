package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/lands/purse"
)

// purseDaemon is the package fake with ListBeadsFiltered served from a plain
// bead list, filtered by type and labels.
type purseDaemon struct {
	*fakeDaemon
	list []*beadsapi.BeadDetail
	err  error
}

func (d purseDaemon) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	if d.err != nil {
		return nil, d.err
	}
	var out []*beadsapi.BeadDetail
	for _, b := range d.list {
		if !slices.Contains(q.Types, b.Type) || q.Offset > 0 {
			continue
		}
		ok := true
		for _, l := range q.Labels {
			ok = ok && slices.Contains(b.Labels, l)
		}
		if ok {
			out = append(out, b)
		}
	}
	return &beadsapi.ListBeadsResult{Beads: out, Total: len(out)}, nil
}

func getLandPurse(t *testing.T, d purseDaemon, land string, now time.Time) (int, map[string]any) {
	t.Helper()
	s := NewServer(d, slog.Default(), "", "", "", nil)
	s.SetNow(func() time.Time { return now })
	mux := http.NewServeMux()
	s.registerLandsPurseRoute(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/lands/"+land+"/purse", nil))
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	return rec.Code, body
}

func TestLandPurseAPI(t *testing.T) {
	now := time.Date(2026, 10, 19, 20, 0, 0, 0, time.UTC)
	d := purseDaemon{fakeDaemon: newFake(), list: []*beadsapi.BeadDetail{
		{ID: "p1", Type: "project", Title: "tomato", Fields: map[string]string{"land_kind": "tenant", "account": "lands-tomato"}},
		{ID: "p2", Type: "project", Title: "gardenio", Fields: map[string]string{"account": "fleet"}},
		{ID: "r1", Type: "credit_rate", Fields: map[string]string{"version": "1", "llm_multiplier": "1.5"}},
		{ID: "g1", Type: "credit_grant", Labels: []string{"project:tomato"}, Fields: map[string]string{"land": "tomato", "amount_credits": "20"}},
		{ID: "a1", Type: "account", Title: "lands-tomato", Fields: map[string]string{
			"total_cost_usd": "10", purse.FieldSpendSyncedAt: now.Add(-time.Minute).Format(time.RFC3339)}},
	}}

	code, body := getLandPurse(t, d, "tomato", now)
	if code != http.StatusOK {
		t.Fatalf("status %d: %v", code, body)
	}
	// 20 granted - 15 spent ($10 x 1.5) leaves 5, above the 2-credit margin.
	if body["state"] != "ok" || body["remaining_credits"] != 5.0 || body["spent_usd"] != 10.0 || body["llm_multiplier"] != 1.5 {
		t.Errorf("purse = %v; want ok, 5 credits left, $10 at cost, x1.5", body)
	}

	if code, body := getLandPurse(t, d, "gardenio", now); code != http.StatusNotFound {
		t.Errorf("operator land: %d %v, want 404", code, body)
	}
	if code, _ := getLandPurse(t, d, "nowhere", now); code != http.StatusNotFound {
		t.Errorf("unknown land: %d, want 404", code)
	}

	d.err = errors.New("daemon down")
	code, body = getLandPurse(t, d, "tomato", now)
	if code != http.StatusBadGateway || body["state"] != "unknown" {
		t.Errorf("daemon down: %d %v, want 502 with state unknown", code, body)
	}
}

// creditsCrossCheck is internal/lands/purse/testdata/credits_crosscheck.json,
// the farm the purse API, the controller's credit gauges and the vmalert
// credit rules (helm/gardenio/tests) must all read the same (plan step 3.2).
type creditsCrossCheck struct {
	LLMMultiplier float64 `json:"llm_multiplier"`
	Lands         []struct {
		Land         string    `json:"land"`
		Grants       []float64 `json:"grants"`
		TotalCostUSD float64   `json:"total_cost_usd"`
		Want         struct {
			GrantedCredits   float64 `json:"granted_credits"`
			SpentCredits     float64 `json:"spent_credits"`
			RemainingCredits float64 `json:"remaining_credits"`
		} `json:"want"`
	} `json:"lands"`
}

// The purse API's credits for the cross-check farm are the fixture's wants,
// the figures the gardenio:land_* recording rules are held to.
func TestLandPurseAPI_CreditsCrossCheck(t *testing.T) {
	raw, err := os.ReadFile("../../internal/lands/purse/testdata/credits_crosscheck.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx creditsCrossCheck
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	list := []*beadsapi.BeadDetail{
		{ID: "r1", Type: "credit_rate", Fields: map[string]string{"version": "1", "llm_multiplier": strconv.FormatFloat(fx.LLMMultiplier, 'f', -1, 64)}},
	}
	for i, l := range fx.Lands {
		list = append(list,
			&beadsapi.BeadDetail{ID: fmt.Sprintf("p%d", i), Type: "project", Title: l.Land, Fields: map[string]string{"land_kind": "tenant", "account": purse.AccountName(l.Land)}},
			&beadsapi.BeadDetail{ID: fmt.Sprintf("a%d", i), Type: "account", Title: purse.AccountName(l.Land), Fields: map[string]string{
				"total_cost_usd": strconv.FormatFloat(l.TotalCostUSD, 'f', -1, 64), purse.FieldSpendSyncedAt: now.Add(-time.Minute).Format(time.RFC3339)}},
		)
		for j, g := range l.Grants {
			list = append(list, &beadsapi.BeadDetail{ID: fmt.Sprintf("g%d-%d", i, j), Type: "credit_grant", Labels: []string{"project:" + l.Land},
				Fields: map[string]string{"land": l.Land, "amount_credits": strconv.FormatFloat(g, 'f', -1, 64)}})
		}
	}
	d := purseDaemon{fakeDaemon: newFake(), list: list}
	for _, l := range fx.Lands {
		code, body := getLandPurse(t, d, l.Land, now)
		if code != http.StatusOK {
			t.Fatalf("%s: status %d: %v", l.Land, code, body)
		}
		for k, want := range map[string]float64{
			"granted_credits": l.Want.GrantedCredits, "spent_credits": l.Want.SpentCredits, "remaining_credits": l.Want.RemainingCredits,
			"in_flight_credits": 0,
		} {
			if body[k] != want {
				t.Errorf("%s: %s = %v, want %v", l.Land, k, body[k], want)
			}
		}
	}
}
