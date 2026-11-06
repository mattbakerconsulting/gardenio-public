package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"gardenio/controller/internal/beadsapi"
)

// landBeads serves ListBeadsFiltered from a bead list by type and labels, the
// slice of the daemon the purse reads.
type landBeads struct {
	mockQuerier
	list  []*beadsapi.BeadDetail
	err   error
	calls int
}

func (l *landBeads) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	l.calls++
	if l.err != nil {
		return nil, l.err
	}
	var out []*beadsapi.BeadDetail
	for _, b := range l.list {
		if !slices.Contains(q.Types, b.Type) || q.Offset > 0 {
			continue
		}
		ok := true
		for _, lb := range q.Labels {
			ok = ok && slices.Contains(b.Labels, lb)
		}
		if ok {
			out = append(out, b)
		}
	}
	return &beadsapi.ListBeadsResult{Beads: out, Total: len(out)}, nil
}

func (l *landBeads) add(typ, title string, labels []string, fields map[string]string) {
	l.list = append(l.list, &beadsapi.BeadDetail{
		ID: fmt.Sprintf("kd-%03d", len(l.list)+1), Type: typ, Title: title, Status: "open", Labels: labels, Fields: fields,
	})
}

func (l *landBeads) grant(land string, credits float64) {
	l.add("credit_grant", "g", []string{"project:" + land}, map[string]string{"land": land, "amount_credits": strconv.FormatFloat(credits, 'f', -1, 64)})
}

func newLandCreditsCollector(q BeadQuerier) *Collector {
	return New(q, slog.New(slog.NewTextHandler(os.Stderr, nil)), 30*time.Second)
}

// The gauges carry the purse's figures for internal/lands/purse/testdata/
// credits_crosscheck.json, the farm the purse API and the vmalert credit rules
// are held to as well (plan step 3.2).
func TestCollectLandCredits_CrossCheck(t *testing.T) {
	raw, err := os.ReadFile("../lands/purse/testdata/credits_crosscheck.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		LLMMultiplier float64 `json:"llm_multiplier"`
		Lands         []struct {
			Land   string    `json:"land"`
			Grants []float64 `json:"grants"`
			Want   struct {
				GrantedCredits float64 `json:"granted_credits"`
			} `json:"want"`
		} `json:"lands"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	q := &landBeads{}
	q.add("credit_rate", "v1", nil, map[string]string{"version": "1", "llm_multiplier": strconv.FormatFloat(fx.LLMMultiplier, 'f', -1, 64)})
	for _, l := range fx.Lands {
		q.add("project", l.Land, nil, map[string]string{"land_kind": "tenant", "account": "lands-" + l.Land})
		for _, g := range l.Grants {
			q.grant(l.Land, g)
		}
	}
	c := newLandCreditsCollector(q)
	c.collectLandCredits(context.Background())

	for _, l := range fx.Lands {
		if got := testutil.ToFloat64(c.landCreditRate.WithLabelValues(l.Land)); got != fx.LLMMultiplier {
			t.Errorf("%s: credit rate = %v, want %v", l.Land, got, fx.LLMMultiplier)
		}
		if got := testutil.ToFloat64(c.landCreditsGranted.WithLabelValues(l.Land)); got != l.Want.GrantedCredits {
			t.Errorf("%s: granted = %v, want %v", l.Land, got, l.Want.GrantedCredits)
		}
	}
}

// Only tenant lands get a series; the highest rate version is the one in force.
func TestCollectLandCredits_TenantLandsAndRateInForce(t *testing.T) {
	q := &landBeads{}
	q.add("project", "tomato", nil, map[string]string{"land_kind": "tenant", "account": "lands-tomato"})
	q.add("project", "gardenio", nil, map[string]string{"land_kind": "operator", "account": "fleet"})
	q.add("project", "legacy", nil, map[string]string{"account": "fleet"})
	q.add("credit_rate", "v1", nil, map[string]string{"version": "1", "llm_multiplier": "1.2"})
	q.add("credit_rate", "v2", nil, map[string]string{"version": "2", "llm_multiplier": "1.5"})
	q.grant("tomato", 30)
	q.grant("gardenio", 99)
	c := newLandCreditsCollector(q)
	c.collectLandCredits(context.Background())

	if n := testutil.CollectAndCount(c.landCreditRate); n != 1 {
		t.Errorf("credit rate series = %d, want 1 (tomato only)", n)
	}
	if n := testutil.CollectAndCount(c.landCreditsGranted); n != 1 {
		t.Errorf("granted series = %d, want 1 (tomato only)", n)
	}
	if got := testutil.ToFloat64(c.landCreditRate.WithLabelValues("tomato")); got != 1.5 {
		t.Errorf("credit rate = %v, want 1.5 (v2)", got)
	}
}

// With no rate in force there is no rate series, so the credit rules record
// nothing; granted credits are still credits.
func TestCollectLandCredits_NoRateNoRateSeries(t *testing.T) {
	q := &landBeads{}
	q.add("project", "tomato", nil, map[string]string{"land_kind": "tenant"})
	q.grant("tomato", 30)
	c := newLandCreditsCollector(q)
	c.collectLandCredits(context.Background())

	if n := testutil.CollectAndCount(c.landCreditRate); n != 0 {
		t.Errorf("credit rate series = %d, want none without a rate", n)
	}
	if got := testutil.ToFloat64(c.landCreditsGranted.WithLabelValues("tomato")); got != 30 {
		t.Errorf("granted = %v, want 30", got)
	}
}

// The ledger is re-read every landCreditsInterval, not every scrape, and a
// failed read keeps the last good figures.
func TestCollectLandCredits_IntervalAndFailedReadKeepsFigures(t *testing.T) {
	q := &landBeads{}
	q.add("project", "tomato", nil, map[string]string{"land_kind": "tenant"})
	q.add("credit_rate", "v1", nil, map[string]string{"version": "1", "llm_multiplier": "1.5"})
	q.grant("tomato", 30)
	c := newLandCreditsCollector(q)
	ctx := context.Background()
	c.collectLandCredits(ctx)

	calls := q.calls
	c.collectLandCredits(ctx)
	if q.calls != calls {
		t.Errorf("a second read inside the interval made %d daemon calls, want 0", q.calls-calls)
	}

	q.err = errors.New("daemon down")
	c.landCreditsAt = time.Now().Add(-landCreditsInterval)
	c.collectLandCredits(ctx)
	if got := testutil.ToFloat64(c.landCreditsGranted.WithLabelValues("tomato")); got != 30 {
		t.Errorf("granted after a failed read = %v, want the last good 30", got)
	}
	if got := testutil.ToFloat64(c.landCreditRate.WithLabelValues("tomato")); got != 1.5 {
		t.Errorf("rate after a failed read = %v, want the last good 1.5", got)
	}
	if got := testutil.ToFloat64(c.scrapeErrors); got != 1 {
		t.Errorf("scrape errors = %v, want 1", got)
	}
}
