package purse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

var now = time.Date(2026, 10, 19, 20, 0, 0, 0, time.UTC)

func rate(m float64) *Rate { return &Rate{Version: 3, LLMMultiplier: m} }

func val(t *testing.T, p *float64) float64 {
	t.Helper()
	if p == nil {
		t.Fatal("figure is null")
	}
	return *p
}

// baseInputs is a land with 100 credits granted at x1.5, synced a minute ago.
func baseInputs() Inputs {
	return Inputs{
		Land: "tomato-patch", Rate: rate(1.5), GrantedCredits: 100, Account: true,
		SpentUSD: 10, SpendSyncedAt: now.Add(-time.Minute),
	}
}

func TestCompute_Spent(t *testing.T) {
	p := Compute(baseInputs(), now)
	if got := val(t, p.SpentCredits); got != 15 {
		t.Errorf("spent = %g credits, want 15 ($10 x 1.5)", got)
	}
	if got := val(t, p.RemainingCredits); got != 85 {
		t.Errorf("remaining = %g, want 85", got)
	}
	if p.State != StateOK || p.Blocked {
		t.Errorf("state = %s blocked=%v, want ok", p.State, p.Blocked)
	}
	if got := val(t, p.UsedPct); got != 15 {
		t.Errorf("used_pct = %g, want 15", got)
	}
}

func TestCompute_MultiplierIsData(t *testing.T) {
	in := baseInputs()
	in.Rate = rate(2)
	p := Compute(in, now)
	if got := val(t, p.SpentCredits); got != 20 {
		t.Errorf("spent at x2 = %g, want 20", got)
	}
	if p.LLMMultiplier != 2 || p.RateVersion != 3 {
		t.Errorf("rate = v%d x%g, want v3 x2", p.RateVersion, p.LLMMultiplier)
	}
}

func TestCompute_InFlightCountsOnlyPastBaseline(t *testing.T) {
	in := baseInputs()
	in.Agents = []AgentCost{{ID: "a1", LiveCostUSD: 3}, {ID: "a2", LiveCostUSD: 2}, {ID: "a3", LiveCostUSD: 1}}
	// a1 had $1 of its $3 in the synced account figure; a2 started after the
	// sync; a3's live cost went backwards (a new session), which is not negative.
	in.Baseline = map[string]float64{"a1": 1, "a3": 4}
	p := Compute(in, now)
	if got := val(t, p.InFlightCredits); got != 6 {
		t.Errorf("in_flight = %g credits, want 6 (($2 + $2 + 0) x 1.5)", got)
	}
	if got := val(t, p.InFlightUSD); got != 4 {
		t.Errorf("in_flight_usd = %g, want 4", got)
	}
	if got := val(t, p.RemainingCredits); got != 100-15-6 {
		t.Errorf("remaining = %g, want 79", got)
	}
	if p.RunningAgents != 3 {
		t.Errorf("running_agents = %d, want 3", p.RunningAgents)
	}
}

func TestCompute_Granted(t *testing.T) {
	in := baseInputs()
	in.GrantedCredits = 15
	in.SpentUSD = 0
	p := Compute(in, now)
	if p.GrantedCredits != 15 || val(t, p.GrantedUSD) != 10 {
		t.Errorf("granted = %g credits / $%g, want 15 / $10 (the key's max_budget)", p.GrantedCredits, val(t, p.GrantedUSD))
	}
}

func TestMargin(t *testing.T) {
	for _, tc := range []struct{ granted, want float64 }{
		{0, 2}, {10, 2}, {40, 2}, {100, 5}, {1000, 50},
	} {
		if got := Margin(tc.granted); got != tc.want {
			t.Errorf("Margin(%g) = %g, want %g", tc.granted, got, tc.want)
		}
	}
}

func TestCompute_WallAtMargin(t *testing.T) {
	for _, tc := range []struct {
		name     string
		granted  float64
		spentUSD float64
		want     string
	}{
		{"above the 5% margin", 100, 63.33, StateOK},      // remaining 5.005
		{"at the 5% margin", 100, 63.3334, StateWall},     // remaining 4.9999
		{"floor margin of 2 credits", 10, 5.4, StateWall}, // remaining 1.9
		{"just above the floor", 10, 5.3, StateOK},        // remaining 2.05
		{"overspent", 10, 20, StateWall},
		{"nothing granted", 0, 0, StateWall},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := baseInputs()
			in.GrantedCredits, in.SpentUSD = tc.granted, tc.spentUSD
			p := Compute(in, now)
			if p.State != tc.want || p.Blocked != (tc.want != StateOK) {
				t.Errorf("state = %s blocked=%v (remaining %v), want %s", p.State, p.Blocked, *p.RemainingCredits, tc.want)
			}
		})
	}
}

func TestCompute_InFlightReachesTheWall(t *testing.T) {
	in := baseInputs()
	in.GrantedCredits, in.SpentUSD = 10, 2 // 3 spent, 7 left
	in.Agents = []AgentCost{{ID: "a1", LiveCostUSD: 3.5}}
	if p := Compute(in, now); p.State != StateWall {
		t.Errorf("state = %s, want wall: 3 spent + 5.25 in flight leaves 1.75 < 2", p.State)
	}
}

func TestCompute_UnknownIsTheWall(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Inputs)
		want string
	}{
		{"stale spend", func(in *Inputs) { in.SpendSyncedAt = now.Add(-16 * time.Minute) }, "more than 15m0s"},
		{"never synced", func(in *Inputs) { in.SpendSyncedAt = time.Time{} }, "never stamped"},
		{"no rate", func(in *Inputs) { in.Rate = nil }, "no credit rate"},
		{"no account", func(in *Inputs) { in.Account, in.AccountProblem = false, "no account bead lands-tomato-patch" }, "no account bead"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := baseInputs()
			tc.edit(&in)
			p := Compute(in, now)
			if p.State != StateUnknown || !p.Blocked {
				t.Errorf("state = %s blocked=%v, want unknown and blocked", p.State, p.Blocked)
			}
			if !strings.Contains(p.Reason, tc.want) {
				t.Errorf("reason = %q, want it to mention %q", p.Reason, tc.want)
			}
		})
	}
	// Fifteen minutes exactly is still known.
	in := baseInputs()
	in.SpendSyncedAt = now.Add(-UnknownAfter)
	if p := Compute(in, now); p.State != StateOK {
		t.Errorf("synced exactly %s ago: state = %s, want ok", UnknownAfter, p.State)
	}
}

func TestCompute_StaleKeepsFiguresAndSyncTime(t *testing.T) {
	in := baseInputs()
	in.SpendSyncedAt = now.Add(-time.Hour)
	p := Compute(in, now)
	if p.SpentCredits == nil || p.SpendSyncedAt != "2026-10-19T19:00:00Z" {
		t.Errorf("a stale purse keeps its last figures and says when: spent=%v synced=%q", p.SpentCredits, p.SpendSyncedAt)
	}
}

func TestCreditsOnly(t *testing.T) {
	p := Compute(baseInputs(), now).CreditsOnly()
	raw, _ := json.Marshal(p)
	for _, k := range []string{"_usd", "llm_multiplier"} {
		if strings.Contains(string(raw), k) {
			t.Errorf("tenant view %s carries %q", raw, k)
		}
	}
	if p.RemainingCredits == nil {
		t.Error("tenant view lost the credits")
	}
}

// fakeSource serves ListBeadsFiltered from a bead list, honouring type,
// labels, status, limit and offset.
type fakeSource struct {
	beads []*beadsapi.BeadDetail
	err   error
	calls int
}

func (f *fakeSource) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	var match []*beadsapi.BeadDetail
	for _, b := range f.beads {
		if len(q.Types) > 0 && !slices.Contains(q.Types, b.Type) {
			continue
		}
		if len(q.Statuses) > 0 && !slices.Contains(q.Statuses, b.Status) {
			continue
		}
		ok := true
		for _, l := range q.Labels {
			ok = ok && slices.Contains(b.Labels, l)
		}
		if ok {
			match = append(match, b)
		}
	}
	total := len(match)
	if q.Offset < len(match) {
		match = match[q.Offset:]
	} else {
		match = nil
	}
	if q.Limit > 0 && len(match) > q.Limit {
		match = match[:q.Limit]
	}
	return &beadsapi.ListBeadsResult{Beads: match, Total: total}, nil
}

func (f *fakeSource) add(typ, title, status string, labels []string, fields map[string]string) {
	f.beads = append(f.beads, &beadsapi.BeadDetail{
		ID: fmt.Sprintf("kd-%03d", len(f.beads)+1), Type: typ, Title: title, Status: status, Labels: labels, Fields: fields,
	})
}

// newFarm is two tenant lands and an operator land, rate v1 x1.5.
func newFarm() *fakeSource {
	f := &fakeSource{}
	f.add("project", "tomato-patch", "open", nil, map[string]string{"land_kind": "tenant", "account": "lands-tomato-patch"})
	f.add("project", "bean-row", "open", nil, map[string]string{"land_kind": "tenant", "account": "lands-bean-row"})
	f.add("project", "gardenio", "open", nil, map[string]string{"account": "fleet"})
	f.add("credit_rate", "v1", "open", nil, map[string]string{"version": "1", "llm_multiplier": "1.2"})
	f.add("credit_rate", "v2", "open", nil, map[string]string{"version": "2", "llm_multiplier": "1.5"})
	f.add("credit_grant", "g", "closed", []string{"project:tomato-patch"}, map[string]string{"land": "tomato-patch", "amount_credits": "50"})
	f.add("credit_grant", "g", "closed", []string{"project:tomato-patch"}, map[string]string{"land": "tomato-patch", "amount_credits": "-5"})
	// A grant labelled for the land but naming another is not counted.
	f.add("credit_grant", "g", "closed", []string{"project:tomato-patch"}, map[string]string{"land": "bean-row", "amount_credits": "1000"})
	f.add("account", "lands-tomato-patch", "open", nil, map[string]string{
		"total_cost_usd": "4", FieldSpendSyncedAt: now.Add(-2 * time.Minute).Format(time.RFC3339),
		FieldLiveCostBaseline: `{"kd-ag1":1.5}`,
	})
	f.add("agent", "steward", "open", []string{"project:tomato-patch"}, map[string]string{"project": "tomato-patch", "agent_state": "working", "live_cost_usd": "2"})
	f.add("agent", "crew", "open", []string{"project:tomato-patch"}, map[string]string{"project": "tomato-patch", "agent_state": "done", "live_cost_usd": "9"})
	f.add("agent", "old", "closed", []string{"project:tomato-patch"}, map[string]string{"project": "tomato-patch", "live_cost_usd": "9"})
	f.beads[len(f.beads)-3].ID = "kd-ag1"
	return f
}

func TestRead_FromBeads(t *testing.T) {
	p, err := Read(context.Background(), newFarm(), "tomato-patch", now)
	if err != nil {
		t.Fatal(err)
	}
	if p.GrantedCredits != 45 {
		t.Errorf("granted = %g, want 45 (50 - 5)", p.GrantedCredits)
	}
	if p.RateVersion != 2 || p.LLMMultiplier != 1.5 {
		t.Errorf("rate = v%d x%g, want the highest version, v2 x1.5", p.RateVersion, p.LLMMultiplier)
	}
	if got := val(t, p.SpentCredits); got != 6 {
		t.Errorf("spent = %g, want 6", got)
	}
	if got := val(t, p.InFlightCredits); got != 0.75 {
		t.Errorf("in_flight = %g, want 0.75 (steward $2 - $1.5 baseline; done and closed agents skipped)", got)
	}
	if p.RunningAgents != 1 || p.State != StateOK {
		t.Errorf("running=%d state=%s, want 1 ok", p.RunningAgents, p.State)
	}
}

func TestRead_NoAccountIsUnknown(t *testing.T) {
	p, err := Read(context.Background(), newFarm(), "bean-row", now)
	if err != nil {
		t.Fatal(err)
	}
	if p.State != StateUnknown || !strings.Contains(p.Reason, "no account bead lands-bean-row") {
		t.Errorf("state=%s reason=%q, want unknown for a missing account", p.State, p.Reason)
	}
}

func TestRead_ForeignAccountIsUnknown(t *testing.T) {
	f := newFarm()
	f.beads[0].Fields["account"] = "fleet"
	p, err := Read(context.Background(), f, "tomato-patch", now)
	if err != nil {
		t.Fatal(err)
	}
	if p.State != StateUnknown || !p.Blocked {
		t.Errorf("a land billing to the fleet account must not read as an empty purse: state=%s", p.State)
	}
}

func TestRead_OperatorLandsUnchanged(t *testing.T) {
	f := newFarm()
	if _, err := Read(context.Background(), f, "gardenio", now); !errors.Is(err, ErrOperatorLand) {
		t.Errorf("operator land: err = %v, want ErrOperatorLand", err)
	}
	f.beads[2].Fields["land_kind"] = "operator"
	if _, err := Read(context.Background(), f, "gardenio", now); !errors.Is(err, ErrOperatorLand) {
		t.Errorf("land_kind=operator: err = %v, want ErrOperatorLand", err)
	}
	if _, err := Read(context.Background(), f, "nowhere", now); !errors.Is(err, ErrNotLand) {
		t.Errorf("unknown land: err = %v, want ErrNotLand", err)
	}
	lands, err := TenantLands(context.Background(), f)
	if err != nil || !slices.Equal(lands, []string{"bean-row", "tomato-patch"}) {
		t.Errorf("TenantLands = %v, %v; want the two tenant lands only", lands, err)
	}
}

func TestRead_DaemonErrorIsAnError(t *testing.T) {
	f := newFarm()
	f.err = errors.New("daemon down")
	if _, err := Read(context.Background(), f, "tomato-patch", now); err == nil {
		t.Error("a failed read must be an error the caller treats as unknown")
	}
}

func TestListAll_Pages(t *testing.T) {
	f := &fakeSource{}
	for range pageSize + 7 {
		f.add("credit_grant", "g", "closed", []string{"project:x"}, map[string]string{"land": "x", "amount_credits": "1"})
	}
	grants, err := LoadGrants(context.Background(), f, "x")
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != pageSize+7 || SumCredits(grants) != pageSize+7 {
		t.Errorf("read %d grants, want %d", len(grants), pageSize+7)
	}
	if f.calls != 2 {
		t.Errorf("calls = %d, want 2 pages", f.calls)
	}
}

func TestBaselineRoundTrip(t *testing.T) {
	got := parseBaseline(BaselineJSON([]AgentCost{{ID: "a", LiveCostUSD: 1.25}}))
	if got["a"] != 1.25 {
		t.Errorf("baseline round trip = %v", got)
	}
	if len(parseBaseline("not json")) != 0 {
		t.Error("a bad baseline must read as empty (every live cost in flight)")
	}
	if LandOfAccount("lands-tomato") != "tomato" || LandOfAccount("fleet") != "" {
		t.Error("LandOfAccount")
	}
}

// A second project bead with a land's name and no land_kind (a non-admin
// could create one before the daemon refused it) does not shadow the land,
// even when its id sorts first: the land stays a frozen tenant land. With
// two land_kind beads, a frozen one wins; with no land_kind, the lowest id.
func TestReadLands_DuplicateFailsClosed(t *testing.T) {
	f := newFarm()
	f.beads[0].Fields["land_frozen"] = "true"
	f.add("project", "tomato-patch", "open", nil, map[string]string{"account": "fleet"})
	f.beads[len(f.beads)-1].ID = "kd-000"
	f.add("project", "Project: tomato-patch ", "open", nil, map[string]string{})
	f.beads[len(f.beads)-1].ID = "kd-0000"
	tenant, frozen, err := LandOf(context.Background(), f, "tomato-patch")
	if err != nil || !tenant || !frozen {
		t.Fatalf("LandOf(tomato-patch) with a duplicate = tenant %v frozen %v err %v, want a frozen tenant land", tenant, frozen, err)
	}
	if b, _, _ := Project(context.Background(), f, "tomato-patch"); b == nil || b.ID != f.beads[0].ID {
		t.Errorf("Project(tomato-patch) = %+v, want the land bead %s", b, f.beads[0].ID)
	}
	if got, _ := TenantProjects(context.Background(), f); got["tomato-patch"] == nil || got["tomato-patch"].ID != f.beads[0].ID {
		t.Errorf("TenantProjects[tomato-patch] = %+v, want the land bead", got["tomato-patch"])
	}

	// Two tenant beads: the frozen one wins over a lower id.
	f.add("project", "bean-row", "open", nil, map[string]string{"land_kind": "tenant", "land_frozen": "true"})
	if _, frozen, _ := LandOf(context.Background(), f, "bean-row"); !frozen {
		t.Error("bean-row: a frozen land_kind duplicate must win over an unfrozen lower id")
	}
	// Neither has a land_kind: the lowest id, as before.
	f.add("project", "gardenio", "open", nil, map[string]string{"account": "other"})
	if b, _, _ := Project(context.Background(), f, "gardenio"); b == nil || b.Fields["account"] != "fleet" {
		t.Errorf("gardenio = %+v, want the lowest id", b)
	}
}

func TestLandOf(t *testing.T) {
	f := newFarm()
	f.beads[0].Fields["land_frozen"] = "true"
	for _, tc := range []struct {
		name           string
		tenant, frozen bool
	}{{"tomato-patch", true, true}, {"bean-row", true, false}, {"gardenio", false, false}} {
		tenant, frozen, err := LandOf(context.Background(), f, tc.name)
		if err != nil || tenant != tc.tenant || frozen != tc.frozen {
			t.Errorf("LandOf(%s) = tenant %v frozen %v err %v, want %v %v", tc.name, tenant, frozen, err, tc.tenant, tc.frozen)
		}
	}
	if _, _, err := LandOf(context.Background(), f, "nowhere"); !errors.Is(err, ErrNotLand) {
		t.Errorf("unknown land: err = %v, want ErrNotLand", err)
	}
	f.err = errors.New("daemon down")
	if _, _, err := LandOf(context.Background(), f, "tomato-patch"); err == nil || errors.Is(err, ErrNotLand) {
		t.Errorf("a failed read must be an error, not ErrNotLand: %v", err)
	}
}

func TestUnmetered(t *testing.T) {
	now := time.Date(2026, 10, 31, 5, 0, 0, 0, time.UTC)
	p := Unmetered("gardenio", now)
	if !p.IsUnmetered() || p.Blocked || p.State != StateUnmetered || p.Reason != UnmeteredReason || p.ReadAt != "2026-10-31T05:00:00Z" {
		t.Fatalf("Unmetered = %+v", p)
	}
	if p.RemainingCredits != nil || p.SpentCredits != nil || p.UsedPct != nil || p.MarginCredits != 0 {
		t.Fatalf("an unmetered purse has no credit figures: %+v", p)
	}
	if c := p.CreditsOnly(); !c.IsUnmetered() || c.Blocked {
		t.Fatalf("CreditsOnly lost the unmetered state: %+v", c)
	}
	if Unreadable("x", nil, now).IsUnmetered() {
		t.Fatal("an unreadable purse reads as unmetered")
	}
}
