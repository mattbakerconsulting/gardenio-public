package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// fakeLandsStore is an in-memory landsStore: projects, accounts and the
// credit_rate / credit_grant beads gb lands writes.
type fakeLandsStore struct {
	projects map[string]beadsapi.ProjectInfo
	accounts map[string]beadsapi.AccountInfo
	beads    []*beadsapi.BeadDetail
	nextID   int
	deleted  []string
	created  []beadsapi.CreateBeadRequest
	// createErr / deleteErr, when set, fail the matching call.
	createErr error
	deleteErr error
}

func newFakeLandsStore() *fakeLandsStore {
	return &fakeLandsStore{
		projects: map[string]beadsapi.ProjectInfo{
			"tomato-patch": {Name: "tomato-patch", LandKind: "tenant", Account: "lands-tomato-patch"},
			"bean-row":     {Name: "bean-row", LandKind: "tenant", Account: "lands-bean-row"},
			"seedling":     {Name: "seedling", LandKind: "tenant"},
			"gardenio":     {Name: "gardenio", Account: "fleet"},
		},
		accounts: map[string]beadsapi.AccountInfo{
			"lands-tomato-patch": {Name: "lands-tomato-patch", LiteLLMKey: "sk-tomato"},
			"lands-bean-row":     {Name: "lands-bean-row", LiteLLMKey: "sk-bean"},
			"fleet":              {Name: "fleet", LiteLLMKey: "sk-fleet"},
		},
	}
}

func (f *fakeLandsStore) ListProjectBeads(context.Context) (map[string]beadsapi.ProjectInfo, error) {
	return f.projects, nil
}

func (f *fakeLandsStore) ListAccountBeads(context.Context) (map[string]beadsapi.AccountInfo, error) {
	return f.accounts, nil
}

func (f *fakeLandsStore) ListAllBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	var out []*beadsapi.BeadDetail
	for _, b := range f.beads {
		if len(q.Types) > 0 && b.Type != q.Types[0] {
			continue
		}
		ok := true
		for _, l := range q.Labels {
			found := false
			for _, have := range b.Labels {
				found = found || have == l
			}
			ok = ok && found
		}
		if ok {
			out = append(out, b)
		}
	}
	return &beadsapi.ListBeadsResult{Beads: out, Total: len(out)}, nil
}

func (f *fakeLandsStore) CreateBeadDetail(_ context.Context, req beadsapi.CreateBeadRequest) (*beadsapi.BeadDetail, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	f.created = append(f.created, req)
	if req.IdempotencyKey != "" {
		for _, b := range f.beads {
			if b.Fields["_idem"] == req.IdempotencyKey {
				return b, nil
			}
		}
	}
	f.nextID++
	fields := beadsapi.ParseFieldsJSON(req.Fields)
	if req.IdempotencyKey != "" {
		fields["_idem"] = req.IdempotencyKey
	}
	b := &beadsapi.BeadDetail{
		ID: fmt.Sprintf("kd-%d", f.nextID), Title: req.Title, Type: req.Type, Status: req.Status,
		Labels: req.Labels, Fields: fields, CreatedAt: time.Date(2026, 10, 19, 15, f.nextID, 0, 0, time.UTC),
	}
	f.beads = append(f.beads, b)
	return b, nil
}

func (f *fakeLandsStore) DeleteBead(_ context.Context, id string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleted = append(f.deleted, id)
	for i, b := range f.beads {
		if b.ID == id {
			f.beads = append(f.beads[:i], f.beads[i+1:]...)
			break
		}
	}
	return nil
}

// budgetRecorder is a keyBudgetSetter that records every call.
type budgetRecorder struct {
	set map[string]float64
	err map[string]error
}

func (r *budgetRecorder) setter() keyBudgetSetter {
	if r.set == nil {
		r.set = map[string]float64{}
	}
	return func(_ context.Context, key string, usd float64) error {
		if err := r.err[key]; err != nil {
			return err
		}
		r.set[key] = usd
		return nil
	}
}

func seedRate(t *testing.T, st *fakeLandsStore, mult float64) creditRate {
	t.Helper()
	r, err := setCreditRate(context.Background(), st, creditRate{LLMMultiplier: mult, SetBy: "oidc:op"}, time.Unix(0, 0))
	if err != nil {
		t.Fatalf("setCreditRate: %v", err)
	}
	return r
}

func TestLandKeyBudgetUSD_RoundsDown(t *testing.T) {
	rate := creditRate{LLMMultiplier: 1.5}
	for _, tc := range []struct{ credits, want float64 }{
		{15, 10}, {10, 6.66}, {1, 0.66}, {0.01, 0}, {0, 0}, {-5, 0}, {100, 66.66}, {150, 100},
	} {
		if got := landKeyBudgetUSD(tc.credits, rate); got != tc.want {
			t.Errorf("landKeyBudgetUSD(%g, x1.5) = %v, want %v", tc.credits, got, tc.want)
		}
	}
	if got := landKeyBudgetUSD(50, creditRate{}); got != 0 {
		t.Errorf("no multiplier must give a zero budget, got %v", got)
	}
}

// Fail closed: with no rate version nothing is written and LiteLLM is never
// called.
func TestAddCredit_RefusedWithoutRate(t *testing.T) {
	st := newFakeLandsStore()
	rec := &budgetRecorder{}
	_, err := addCredit(context.Background(), st, rec.setter(), creditAddInput{Land: "tomato-patch", Credits: 50, GrantedBy: "oidc:op"})
	if !errors.Is(err, errNoCreditRate) {
		t.Fatalf("err = %v, want errNoCreditRate", err)
	}
	if len(st.created) != 0 || len(rec.set) != 0 {
		t.Fatalf("wrote %d beads and %d key budgets without a rate", len(st.created), len(rec.set))
	}
}

func TestAddCredit_WritesGrantAndDerivesBudget(t *testing.T) {
	st := newFakeLandsStore()
	rate := seedRate(t, st, 1.5)
	rec := &budgetRecorder{}
	ctx := context.Background()

	res, err := addCredit(ctx, st, rec.setter(), creditAddInput{Land: "tomato-patch", Credits: 50, Reason: "welcome", DecisionID: "gd-dec", GrantedBy: "oidc:op"})
	if err != nil {
		t.Fatalf("addCredit: %v", err)
	}
	if res.TotalCredits != 50 || res.MaxBudgetUSD != 33.33 || rec.set["sk-tomato"] != 33.33 {
		t.Fatalf("res = %+v, key budget %v; want 50 credits, $33.33", res, rec.set["sk-tomato"])
	}
	req := st.created[len(st.created)-1]
	if req.Type != "credit_grant" || req.Status != "closed" || len(req.Labels) != 1 || req.Labels[0] != "project:tomato-patch" {
		t.Fatalf("grant request = %+v", req)
	}
	var f map[string]any
	_ = json.Unmarshal(req.Fields, &f)
	if f["amount_credits"] != 50.0 || f["rate_version"] != float64(rate.Version) || f["granted_by"] != "oidc:op" || f["decision_id"] != "gd-dec" || f["land"] != "tomato-patch" {
		t.Fatalf("grant fields = %v", f)
	}

	// A second grant and a refund: the budget follows the whole ledger.
	if _, err := addCredit(ctx, st, rec.setter(), creditAddInput{Land: "tomato-patch", Credits: 25, GrantedBy: "oidc:op"}); err != nil {
		t.Fatal(err)
	}
	if rec.set["sk-tomato"] != 50 {
		t.Fatalf("75 credits at x1.5 = $%v, want $50", rec.set["sk-tomato"])
	}
	if _, err := addCredit(ctx, st, rec.setter(), creditAddInput{Land: "tomato-patch", Credits: -15, Reason: "duplicate", GrantedBy: "oidc:op"}); err != nil {
		t.Fatal(err)
	}
	if rec.set["sk-tomato"] != 40 {
		t.Fatalf("60 credits at x1.5 = $%v, want $40", rec.set["sk-tomato"])
	}
	if _, ok := rec.set["sk-bean"]; ok {
		t.Fatal("another land's key was touched")
	}
}

func TestAddCredit_RollsBackOnLiteLLMError(t *testing.T) {
	st := newFakeLandsStore()
	seedRate(t, st, 1.5)
	rec := &budgetRecorder{err: map[string]error{"sk-tomato": errors.New("HTTP 500: boom")}}
	_, err := addCredit(context.Background(), st, rec.setter(), creditAddInput{Land: "tomato-patch", Credits: 50, GrantedBy: "oidc:op"})
	if err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("err = %v, want a rollback", err)
	}
	if len(st.deleted) != 1 {
		t.Fatalf("deleted = %v, want the grant", st.deleted)
	}
	grants, _ := loadCreditGrants(context.Background(), st, "tomato-patch")
	if len(grants) != 0 {
		t.Fatalf("ledger after rollback = %+v", grants)
	}

	// A rollback that fails says so and names the bead.
	st.deleteErr = errors.New("daemon down")
	_, err = addCredit(context.Background(), st, rec.setter(), creditAddInput{Land: "tomato-patch", Credits: 50, GrantedBy: "oidc:op"})
	if err == nil || !strings.Contains(err.Error(), "delete it by hand") {
		t.Fatalf("err = %v, want a manual-cleanup error", err)
	}
}

func TestAddCredit_Refusals(t *testing.T) {
	st := newFakeLandsStore()
	seedRate(t, st, 1.5)
	rec := &budgetRecorder{}
	for _, tc := range []struct {
		name string
		in   creditAddInput
		want string
	}{
		{"zero", creditAddInput{Land: "tomato-patch", Credits: 0, GrantedBy: "oidc:op"}, "nonzero"},
		{"negative without reason", creditAddInput{Land: "tomato-patch", Credits: -5, GrantedBy: "oidc:op"}, "needs --reason"},
		{"no operator", creditAddInput{Land: "tomato-patch", Credits: 5}, "granted_by"},
		{"unknown land", creditAddInput{Land: "nowhere", Credits: 5, GrantedBy: "oidc:op"}, "not a project"},
		{"operator land", creditAddInput{Land: "gardenio", Credits: 5, GrantedBy: "oidc:op"}, "operator land"},
		{"no key", creditAddInput{Land: "seedling", Credits: 5, GrantedBy: "oidc:op"}, "no LiteLLM key"},
	} {
		before := len(st.created)
		_, err := addCredit(context.Background(), st, rec.setter(), tc.in)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
		if len(st.created) != before {
			t.Errorf("%s: a bead was written", tc.name)
		}
	}
	if len(rec.set) != 0 {
		t.Fatalf("key budgets set on refusal: %v", rec.set)
	}
}

func TestSetCreditRate_Versions(t *testing.T) {
	st := newFakeLandsStore()
	if _, err := currentCreditRate(context.Background(), st); !errors.Is(err, errNoCreditRate) {
		t.Fatalf("empty table: err = %v", err)
	}
	v1 := seedRate(t, st, 1.5)
	v2 := seedRate(t, st, 2)
	if v1.Version != 1 || v2.Version != 2 {
		t.Fatalf("versions = %d, %d", v1.Version, v2.Version)
	}
	cur, err := currentCreditRate(context.Background(), st)
	if err != nil || cur.Version != 2 || cur.LLMMultiplier != 2 || cur.ComputeCreditsPerAgentHour != 0 {
		t.Fatalf("current = %+v, %v", cur, err)
	}
	if req := st.created[len(st.created)-1]; req.Type != "credit_rate" || req.IdempotencyKey != "credit_rate:v2" {
		t.Fatalf("rate request = %+v", req)
	}
	for _, bad := range []creditRate{{LLMMultiplier: 0}, {LLMMultiplier: -1}, {LLMMultiplier: 1.5, ComputeCreditsPerAgentHour: -0.25}} {
		if _, err := setCreditRate(context.Background(), st, bad, time.Now()); err == nil {
			t.Errorf("setCreditRate(%+v) accepted", bad)
		}
	}
}

// A version written concurrently with another multiplier is reported, not
// silently taken as ours.
func TestSetCreditRate_ConcurrentVersion(t *testing.T) {
	st := newFakeLandsStore()
	seedRate(t, st, 1.5)
	// Someone else wrote v2 between our read and our write.
	st.beads = append(st.beads, &beadsapi.BeadDetail{ID: "kd-other", Type: "credit_rate",
		Fields: map[string]string{"version": "2", "llm_multiplier": "3", "_idem": "credit_rate:v2"}})
	stale := &staleRatesStore{fakeLandsStore: st}
	_, err := setCreditRate(context.Background(), stale, creditRate{LLMMultiplier: 2}, time.Now())
	if err == nil || !strings.Contains(err.Error(), "concurrently") {
		t.Fatalf("err = %v, want a concurrency error", err)
	}
}

// staleRatesStore hides credit_rate beads past v1 from reads, as if they had
// landed after this caller's read.
type staleRatesStore struct{ *fakeLandsStore }

func (s *staleRatesStore) ListAllBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	res, err := s.fakeLandsStore.ListAllBeadsFiltered(ctx, q)
	if err != nil || len(q.Types) == 0 || q.Types[0] != "credit_rate" {
		return res, err
	}
	var keep []*beadsapi.BeadDetail
	for _, b := range res.Beads {
		if b.Fields["version"] == "1" {
			keep = append(keep, b)
		}
	}
	return &beadsapi.ListBeadsResult{Beads: keep, Total: len(keep)}, nil
}

func TestReapplyCreditRate_EveryTenantKey(t *testing.T) {
	st := newFakeLandsStore()
	seedRate(t, st, 1.5)
	rec := &budgetRecorder{}
	ctx := context.Background()
	if _, err := addCredit(ctx, st, rec.setter(), creditAddInput{Land: "tomato-patch", Credits: 60, GrantedBy: "oidc:op"}); err != nil {
		t.Fatal(err)
	}
	if _, err := addCredit(ctx, st, rec.setter(), creditAddInput{Land: "bean-row", Credits: 10, GrantedBy: "oidc:op"}); err != nil {
		t.Fatal(err)
	}

	v2 := seedRate(t, st, 2)
	rec.err = map[string]error{"sk-bean": errors.New("HTTP 502")}
	results, err := reapplyCreditRate(ctx, st, rec.setter(), v2)
	if err != nil {
		t.Fatal(err)
	}
	byLand := map[string]landReapply{}
	for _, r := range results {
		byLand[r.Land] = r
	}
	if len(results) != 3 {
		t.Fatalf("results = %+v, want the three tenant lands only", results)
	}
	if r := byLand["tomato-patch"]; r.Action != "set" || r.MaxBudgetUSD != 30 || rec.set["sk-tomato"] != 30 {
		t.Errorf("tomato-patch = %+v (key $%v), want set $30", r, rec.set["sk-tomato"])
	}
	if r := byLand["bean-row"]; r.Action != "failed" {
		t.Errorf("bean-row = %+v, want failed", r)
	}
	if r := byLand["seedling"]; r.Action != "skipped" {
		t.Errorf("seedling = %+v, want skipped", r)
	}
	if _, ok := rec.set["sk-fleet"]; ok {
		t.Error("an operator land's key was touched")
	}
	var buf bytes.Buffer
	if err := printReapply(&buf, v2, results); err == nil || !strings.Contains(err.Error(), "gb lands rates reapply") {
		t.Errorf("printReapply err = %v, want a reapply hint", err)
	}
}

func TestCreditLedger_TwoFigures(t *testing.T) {
	st := newFakeLandsStore()
	ctx := context.Background()

	// Before any rate: credits, and no dollar figure.
	l, err := buildCreditLedger(ctx, st, "tomato-patch")
	if err != nil {
		t.Fatal(err)
	}
	if l.AtCostUSD != nil || l.AtCostUSDNote == "" {
		t.Fatalf("ledger without a rate = %+v", l)
	}

	seedRate(t, st, 1.5)
	rec := &budgetRecorder{}
	for _, c := range []float64{30, 15} {
		if _, err := addCredit(ctx, st, rec.setter(), creditAddInput{Land: "tomato-patch", Credits: c, Reason: "r", GrantedBy: "oidc:op"}); err != nil {
			t.Fatal(err)
		}
	}
	l, err = buildCreditLedger(ctx, st, "tomato-patch")
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Grants) != 2 || l.TotalCredits != 45 || l.AtCostUSD == nil || *l.AtCostUSD != 30 {
		t.Fatalf("ledger = %+v", l)
	}
	var buf bytes.Buffer
	printCreditLedger(&buf, l)
	out := buf.String()
	for _, want := range []string{"CREDITS", "AT COST (v1)", "+30", "$20.00", "45 credits granted; $30.00 at cost"} {
		if !strings.Contains(out, want) {
			t.Errorf("ledger output missing %q:\n%s", want, out)
		}
	}
	if _, err := buildCreditLedger(ctx, st, "gardenio"); err == nil {
		t.Error("an operator land has no ledger")
	}
}

// Merge-desk send-back on PR #627: land_kind and account are project fields a
// static token can write, so a misconfigured project must never steer a
// budget write onto another account's key (the fleet's above all).
func TestLandKey_RefusesAnotherAccountsKey(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(st *fakeLandsStore)
		land  string
		want  string
	}{
		{"fleet project flipped to tenant", func(st *fakeLandsStore) {
			p := st.projects["gardenio"]
			p.LandKind = "tenant"
			st.projects["gardenio"] = p
		}, "gardenio", "not its own lands-gardenio"},
		{"tenant pointed at the fleet account", func(st *fakeLandsStore) {
			p := st.projects["tomato-patch"]
			p.Account = "fleet"
			st.projects["tomato-patch"] = p
		}, "tomato-patch", "not its own lands-tomato-patch"},
		{"tenant account shared with another project", func(st *fakeLandsStore) {
			st.projects["sneaky"] = beadsapi.ProjectInfo{Name: "sneaky", Account: "lands-tomato-patch"}
		}, "tomato-patch", "shared with sneaky"},
		{"tenant account holding the fleet key", func(st *fakeLandsStore) {
			st.accounts["lands-tomato-patch"] = beadsapi.AccountInfo{Name: "lands-tomato-patch", LiteLLMKey: "sk-fleet"}
		}, "tomato-patch", "also account fleet's"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newFakeLandsStore()
			tc.setup(st)
			rate := seedRate(t, st, 1.5)
			rec := &budgetRecorder{}
			ctx := context.Background()

			_, err := addCredit(ctx, st, rec.setter(), creditAddInput{Land: tc.land, Credits: 50, GrantedBy: "oidc:op"})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("credit add err = %v, want %q", err, tc.want)
			}
			for _, b := range st.beads {
				if b.Type == "credit_grant" {
					t.Fatalf("a grant was written for a refused land: %+v", b)
				}
			}

			results, err := reapplyCreditRate(ctx, st, rec.setter(), rate)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range results {
				if r.Land == tc.land && (r.Action != "failed" || !strings.Contains(r.Note, tc.want)) {
					t.Errorf("reapply %s = %+v, want failed with %q", tc.land, r, tc.want)
				}
			}
			if _, ok := rec.set["sk-fleet"]; ok {
				t.Errorf("the fleet key's max_budget was written: $%v", rec.set["sk-fleet"])
			}
		})
	}
}

// A bead squatting on the rate's idempotency key (any type, any version) is
// never taken as the rate just set.
func TestSetCreditRate_IdempotencySquatFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		squat *beadsapi.BeadDetail
		want  string
	}{
		{"task on the key", &beadsapi.BeadDetail{ID: "kd-squat", Type: "task",
			Fields: map[string]string{"_idem": "credit_rate:v1"}}, "not a credit_rate"},
		{"credit_rate with another version", &beadsapi.BeadDetail{ID: "kd-squat", Type: "credit_rate",
			Fields: map[string]string{"_idem": "credit_rate:v1", "version": "7", "llm_multiplier": "1.5"}}, "version 7"},
		{"credit_rate with no multiplier", &beadsapi.BeadDetail{ID: "kd-squat", Type: "credit_rate",
			Fields: map[string]string{"_idem": "credit_rate:v1", "version": "1"}}, "concurrently"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newFakeLandsStore()
			st.beads = append(st.beads, tc.squat)
			_, err := setCreditRate(context.Background(), squatHidingStore{st, tc.squat.ID}, creditRate{LLMMultiplier: 1.5}, time.Now())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

// squatHidingStore hides one bead from list reads, so the caller computes v1
// and its create lands on the squatted key.
type squatHidingStore struct {
	*fakeLandsStore
	hide string
}

func (s squatHidingStore) ListAllBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	res, err := s.fakeLandsStore.ListAllBeadsFiltered(ctx, q)
	if err != nil {
		return res, err
	}
	var keep []*beadsapi.BeadDetail
	for _, b := range res.Beads {
		if b.ID != s.hide {
			keep = append(keep, b)
		}
	}
	return &beadsapi.ListBeadsResult{Beads: keep, Total: len(keep)}, nil
}

// An empty rate table read on a per-agent token says the rate is not visible,
// not that none is set: kbeads hides the label-less credit_rate bead from a
// per-agent token's list (gd-nVhTwz4aBC).
func TestPrintCreditRates_EmptyTable(t *testing.T) {
	for _, tc := range []struct {
		name, agent, want, notWant string
	}{
		{"operator", "", "no credit rate is set", "visible to this token"},
		{"per-agent token", "gardenio-crew-x", "no rate is visible to this token", "no credit rate is set"},
	} {
		var buf bytes.Buffer
		if err := printCreditRates(&buf, nil, tc.agent, false); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		got := buf.String()
		if !strings.Contains(got, tc.want) || strings.Contains(got, tc.notWant) {
			t.Errorf("%s: output = %q, want %q and not %q", tc.name, got, tc.want, tc.notWant)
		}
		if tc.agent != "" && (!strings.Contains(got, tc.agent) || !strings.Contains(got, "an admin")) {
			t.Errorf("%s: output = %q, want the agent and the admin route", tc.name, got)
		}
	}
}

func TestPrintCreditRates_JSONVisibleToCaller(t *testing.T) {
	rates := []creditRate{{Version: 1, LLMMultiplier: 1.5}}
	for _, tc := range []struct {
		name     string
		rates    []creditRate
		agent    string
		visible  bool
		wantNote bool
	}{
		{"operator, empty", nil, "", true, false},
		{"per-agent token, empty", nil, "gardenio-crew-x", false, true},
		{"per-agent token, rates", rates, "gardenio-crew-x", true, false},
	} {
		var buf bytes.Buffer
		if err := printCreditRates(&buf, tc.rates, tc.agent, true); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		var got struct {
			Rates           []creditRate `json:"rates"`
			VisibleToCaller *bool        `json:"visible_to_caller"`
			Note            string       `json:"note"`
		}
		if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
			t.Fatalf("%s: %v in %s", tc.name, err, buf.String())
		}
		if got.VisibleToCaller == nil || *got.VisibleToCaller != tc.visible {
			t.Errorf("%s: visible_to_caller = %v, want %v", tc.name, got.VisibleToCaller, tc.visible)
		}
		if (got.Note != "") != tc.wantNote {
			t.Errorf("%s: note = %q, want note %v", tc.name, got.Note, tc.wantNote)
		}
		if len(got.Rates) != len(tc.rates) {
			t.Errorf("%s: rates = %+v", tc.name, got.Rates)
		}
	}
}

func TestPrintCreditRates_Table(t *testing.T) {
	var buf bytes.Buffer
	rates := []creditRate{{Version: 1, LLMMultiplier: 1.5}, {Version: 2, LLMMultiplier: 2}}
	if err := printCreditRates(&buf, rates, "", false); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[1], "v2") || !strings.Contains(lines[1], "in force") || !strings.HasPrefix(lines[2], "v1") {
		t.Fatalf("table = %q", buf.String())
	}
}
