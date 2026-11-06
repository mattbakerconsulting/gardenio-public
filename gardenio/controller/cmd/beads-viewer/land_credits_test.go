package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/gardenhub"
)

// The land's credit queries are fixed: only the land reaches them, as an exact matcher, and they name only the 3.2
// credit series (plan gd-gh5J0vVbJP step 3.3).
func TestLandCreditsQueries(t *testing.T) {
	inst, rem := buildLandCreditsInstantQuery("acme"), buildLandCreditsRemainingQuery("acme")
	if inst != `{__name__=~"gardenio:land_spend_credits:(today|increase7d|increase30d|rate1d|by_agent)",land="acme"}` {
		t.Errorf("instant query: %s", inst)
	}
	if rem != `gardenio:land_credits:remaining{land="acme"}` {
		t.Errorf("remaining query: %s", rem)
	}
	for _, q := range []string{inst, rem} {
		if strings.Contains(q, `land=~`) || strings.Contains(strings.ToLower(q), "usd") {
			t.Errorf("query widens the land or reads dollars: %s", q)
		}
	}
	// gardenhub validates the land as a DNS label; a quote still cannot leave the matcher
	if q := buildLandCreditsRemainingQuery(`x"} or vector(1) or {a="`); !strings.HasPrefix(q, `gardenio:land_credits:remaining{land="x\"}`) {
		t.Errorf("escape: %s", q)
	}
}

func TestLandCreditSource(t *testing.T) {
	ctx := context.Background()
	srv := NewServer(newFake(), slog.Default(), "", "", "", nil)
	w := gardenhub.CreditWindow{Name: "7d", Start: time.Unix(1700000000, 0), End: time.Unix(1700604800, 0), Step: time.Hour}
	if _, err := (landCreditSource{srv}).LandCredits(ctx, "acme", w); !errors.Is(err, gardenhub.ErrCreditsUnavailable) {
		t.Fatalf("no upstream: %v, want unavailable", err)
	}

	var mu sync.Mutex
	var paths []string
	failRange := false
	empty := false
	upstream := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path+" "+r.URL.Query().Get("start")+" "+r.URL.Query().Get("end")+" "+r.URL.Query().Get("step"))
		mu.Unlock()
		switch {
		case empty && r.URL.Path == vmsingleInstantQueryPath:
			_, _ = rw.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
		case empty:
			_, _ = rw.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`))
		case r.URL.Path == vmsingleInstantQueryPath:
			_, _ = rw.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[` +
				`{"metric":{"__name__":"gardenio:land_spend_credits:today","land":"acme"},"value":[1700604800,"1.25"]},` +
				`{"metric":{"__name__":"gardenio:land_spend_credits:increase7d","land":"acme"},"value":[1700604800,"20"]},` +
				`{"metric":{"__name__":"gardenio:land_spend_credits:increase30d","land":"acme"},"value":[1700604800,"61"]},` +
				`{"metric":{"__name__":"gardenio:land_spend_credits:rate1d","land":"acme"},"value":[1700604800,"0.4"]},` +
				`{"metric":{"__name__":"gardenio:land_spend_credits:by_agent","land":"acme","agent":"crew-a"},"value":[1700604800,"3"]},` +
				`{"metric":{"__name__":"gardenio:land_spend_credits:by_agent","land":"other","agent":"crew-z"},"value":[1700604800,"9"]},` +
				`{"metric":{"__name__":"gardenio:land_spend_credits:by_agent","land":"acme"},"value":[1700604800,"9"]}` +
				`]}}`))
		case failRange:
			rw.WriteHeader(http.StatusBadGateway)
		default:
			_, _ = rw.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[` +
				`{"metric":{"__name__":"gardenio:land_credits:remaining","land":"acme"},"values":[[1700000000,"50"],[1700003600,"NaN"],[1700007200,"48.5"]]}` +
				`]}}`))
		}
	}))
	t.Cleanup(upstream.Close)
	srv.SetMetricsProxy(upstream.URL)

	h, err := (landCreditSource{srv}).LandCredits(ctx, "acme", w)
	if err != nil {
		t.Fatal(err)
	}
	if !h.Recorded || *h.Today != 1.25 || *h.Week != 20 || *h.Month != 61 || *h.RatePerHour != 0.4 {
		t.Errorf("history: %+v", h)
	}
	if len(h.Agents) != 1 || h.Agents[0] != (gardenhub.AgentCredits{Agent: "crew-a", Credits: 3}) {
		t.Errorf("agents (another land's and a nameless one left out): %+v", h.Agents)
	}
	if len(h.Remaining) != 2 || h.Remaining[1] != (gardenhub.CreditPoint{1700007200, 48.5}) {
		t.Errorf("remaining: %+v", h.Remaining)
	}
	if strings.Join(paths, ",") != vmsingleInstantQueryPath+"   ,"+vmsingleQueryRangePath+" 1700000000 1700604800 3600s" {
		t.Errorf("upstream reads: %q", paths)
	}

	empty = true
	if h, err := (landCreditSource{srv}).LandCredits(ctx, "acme", w); err != nil || h.Recorded {
		t.Errorf("no series: %+v %v, want not recorded", h, err)
	}
	empty, failRange = false, true
	if _, err := (landCreditSource{srv}).LandCredits(ctx, "acme", w); !errors.Is(err, gardenhub.ErrCreditsUnavailable) {
		t.Errorf("range read failed: %v, want unavailable", err)
	}
}
