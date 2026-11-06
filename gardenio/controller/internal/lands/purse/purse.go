// Package purse computes a tenant land's purse (Lands Track B spec
// gardenio/docs/superpowers/specs/2026-10-19-lands-track-b-design.md §3.4,
// step 8.2).
//
// Every figure is in credits; m is the llm_multiplier of the credit rate in
// force (1 credit = US$1 billed to the tenant, gd-XQmEoHAqmp):
//
//	granted   = sum(credit_grant.amount_credits)
//	spent     = m × account(lands-<land>).total_cost_usd      (exact, ≤ 5 min old)
//	in_flight = m × sum(live_cost_usd − baseline) over the land's running agents
//	remaining = granted − spent − in_flight
//	wall      = remaining ≤ margin,  margin = max(2 credits, 5% of granted)
//
// spent comes from the spend sync (cmd/controller/spend_sync.go), which stamps
// spend_synced_at on a land's account bead and, with it, live_cost_baseline:
// each running agent's live_cost_usd at that sync, so in_flight counts only
// what the account figure does not hold yet.
//
// When the spend source has not synced for more than UnknownAfter, or the
// purse cannot be read at all (no rate in force, no land account), the purse
// is unknown, and unknown counts as the wall for new spawns: fail closed is
// the rule for a tenant's money. Operator lands have no purse
// (ErrOperatorLand), so their behaviour is unchanged. Where an operator land
// must still be told as a purse (the home garden's steward, step 9.13), it is
// Unmetered: operator-paid, never the wall.
package purse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

const (
	// UnknownAfter is how stale the synced spend may be before the purse is
	// unknown. The spend sync runs every 5 minutes, so this is three misses.
	UnknownAfter = 15 * time.Minute
	// MinMarginCredits and MarginFraction set the margin: max(2 credits, 5% of
	// granted). It covers the 5-minute sync lag plus a gardener's next few
	// calls; the land key's LiteLLM max_budget stops the rest.
	MinMarginCredits = 2.0
	MarginFraction   = 0.05

	// FieldSpendSyncedAt (RFC3339) and FieldLiveCostBaseline (a JSON object of
	// agent bead id → live_cost_usd) are written on a land's account bead by
	// the spend sync.
	FieldSpendSyncedAt    = "spend_synced_at"
	FieldLiveCostBaseline = "live_cost_baseline"

	// States.
	StateOK      = "ok"
	StateWall    = "wall"
	StateUnknown = "unknown"
	// StateUnmetered is an operator land told as a purse: no credits, no
	// wall (Unmetered).
	StateUnmetered = "unmetered"

	pageSize = 1000
	maxPages = 100
)

var (
	// ErrOperatorLand: the project is one of the operator's own lands, which
	// have no purse.
	ErrOperatorLand = errors.New("operator lands have no purse")
	// ErrNotLand: no live type:project bead carries the name.
	ErrNotLand = errors.New("not a project")
)

// liveStatuses are the bead statuses of an agent that may still be running.
var liveStatuses = []string{"open", "in_progress", "blocked", "deferred"}

// allStatuses lets a ledger read see the grants and rates, which are born closed.
var allStatuses = []string{"open", "in_progress", "blocked", "deferred", "closed"}

// Source is the slice of the daemon client the purse reads through. The
// daemon client, the viewer's beadAPI and gardenhub's FarmSource all have it.
type Source interface {
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
}

// Rate is one version of the credit rate table (type=credit_rate, step 8.1).
type Rate struct {
	ID                         string  `json:"id"`
	Version                    int     `json:"version"`
	EffectiveAt                string  `json:"effective_at"`
	LLMMultiplier              float64 `json:"llm_multiplier"`
	ComputeCreditsPerAgentHour float64 `json:"compute_credits_per_agent_hour"`
	SetBy                      string  `json:"set_by,omitempty"`
	DecisionID                 string  `json:"decision_id,omitempty"`
	Reason                     string  `json:"reason,omitempty"`
}

// Grant is one credit ledger entry (type=credit_grant, step 8.1).
type Grant struct {
	ID            string  `json:"id"`
	Land          string  `json:"land"`
	AmountCredits float64 `json:"amount_credits"`
	Reason        string  `json:"reason,omitempty"`
	GrantedBy     string  `json:"granted_by"`
	DecisionID    string  `json:"decision_id,omitempty"`
	RateVersion   int     `json:"rate_version"`
	CreatedAt     string  `json:"created_at,omitempty"`
}

// FieldFloat parses a numeric bead field; anything unparseable is 0.
func FieldFloat(fields map[string]string, k string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(fields[k]), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return v
}

// FieldInt parses an integer bead field, accepting a float-shaped one.
func FieldInt(fields map[string]string, k string) int {
	v, err := strconv.Atoi(strings.TrimSpace(fields[k]))
	if err != nil {
		return int(FieldFloat(fields, k))
	}
	return v
}

// RateFromBead parses a credit_rate bead.
func RateFromBead(b *beadsapi.BeadDetail) Rate {
	f := b.Fields
	return Rate{
		ID: b.ID, Version: FieldInt(f, "version"), EffectiveAt: f["effective_at"],
		LLMMultiplier: FieldFloat(f, "llm_multiplier"), ComputeCreditsPerAgentHour: FieldFloat(f, "compute_credits_per_agent_hour"),
		SetBy: f["set_by"], DecisionID: f["decision_id"], Reason: f["reason"],
	}
}

// GrantFromBead parses a credit_grant bead.
func GrantFromBead(b *beadsapi.BeadDetail) Grant {
	f := b.Fields
	g := Grant{
		ID: b.ID, Land: f["land"], AmountCredits: FieldFloat(f, "amount_credits"), Reason: f["reason"],
		GrantedBy: f["granted_by"], DecisionID: f["decision_id"], RateVersion: FieldInt(f, "rate_version"),
	}
	if !b.CreatedAt.IsZero() {
		g.CreatedAt = b.CreatedAt.UTC().Format(time.RFC3339)
	}
	return g
}

// InForce is the highest rate version, or nil when none is set or it charges
// nothing (which no grant is priced on either).
func InForce(rates []Rate) *Rate {
	var top *Rate
	for i := range rates {
		if top == nil || rates[i].Version > top.Version {
			top = &rates[i]
		}
	}
	if top == nil || !(top.LLMMultiplier > 0) {
		return nil
	}
	r := *top
	return &r
}

// keyBudgetEpsilon absorbs float noise before rounding down to the cent, so
// 15 credits at 1.5 is $10.00, not $9.99.
const keyBudgetEpsilon = 1e-9

// KeyBudgetUSD is a land key's LiteLLM max_budget, the second wall behind the
// purse (§3.4): credits / llm_multiplier, rounded down to the cent, never
// below zero.
func KeyBudgetUSD(credits float64, rate Rate) float64 {
	if credits <= 0 || rate.LLMMultiplier <= 0 {
		return 0
	}
	return math.Floor(credits/rate.LLMMultiplier*100+keyBudgetEpsilon) / 100
}

// AccountName is the LiteLLM account a tenant land is billed to.
func AccountName(land string) string { return "lands-" + land }

// LandOfAccount is the land an account bills, or "" when it is not a land's.
func LandOfAccount(account string) string {
	land, ok := strings.CutPrefix(account, "lands-")
	if !ok {
		return ""
	}
	return land
}

// AgentCost is one running agent's in-pod cost estimate.
type AgentCost struct {
	ID          string
	LiveCostUSD float64
}

// Inputs is everything Compute needs, already read.
type Inputs struct {
	Land string
	// Rate is the rate in force; nil = none set.
	Rate *Rate
	// GrantedCredits is the sum of the land's ledger.
	GrantedCredits float64
	// Account is false when the land has no lands-<land> account to read
	// spend from; AccountProblem says why.
	Account        bool
	AccountProblem string
	// SpentUSD is the account's total_cost_usd as of SpendSyncedAt (zero =
	// the spend sync never stamped it).
	SpentUSD      float64
	SpendSyncedAt time.Time
	// Baseline is each agent's live_cost_usd at SpendSyncedAt.
	Baseline map[string]float64
	// Agents are the land's running agents now.
	Agents []AgentCost
}

// Purse is one land's purse. Credit figures are what a tenant sees; the *_usd
// figures are the same amounts at cost (credits ÷ m), the operator's second
// figure. A figure that cannot be computed (no rate, no account) is null.
type Purse struct {
	Land string `json:"land"`
	// State is ok, wall, unknown, or unmetered for an operator land told as
	// a purse. Blocked is true at the wall and when unknown: new spawns and
	// gardener fires are refused (step 8.3).
	State   string `json:"state"`
	Blocked bool   `json:"blocked"`
	// Reason says why the state is unknown or unmetered.
	Reason string `json:"reason,omitempty"`

	GrantedCredits   float64  `json:"granted_credits"`
	SpentCredits     *float64 `json:"spent_credits"`
	InFlightCredits  *float64 `json:"in_flight_credits"`
	RemainingCredits *float64 `json:"remaining_credits"`
	MarginCredits    float64  `json:"margin_credits"`
	// UsedPct is (spent + in_flight) ÷ granted × 100, the level the threshold
	// notices read (step 8.4); null when nothing is granted or spent is null.
	UsedPct *float64 `json:"used_pct"`

	RateVersion   int     `json:"rate_version,omitempty"`
	LLMMultiplier float64 `json:"llm_multiplier,omitempty"`

	GrantedUSD   *float64 `json:"granted_usd,omitempty"`
	SpentUSD     *float64 `json:"spent_usd,omitempty"`
	InFlightUSD  *float64 `json:"in_flight_usd,omitempty"`
	RemainingUSD *float64 `json:"remaining_usd,omitempty"`

	RunningAgents int    `json:"running_agents"`
	SpendSyncedAt string `json:"spend_synced_at,omitempty"`
	ReadAt        string `json:"read_at"`
}

// CreditsOnly is the purse as a tenant sees it: credits, no at-cost dollars
// and no rate.
func (p Purse) CreditsOnly() Purse {
	p.GrantedUSD, p.SpentUSD, p.InFlightUSD, p.RemainingUSD = nil, nil, nil, nil
	p.LLMMultiplier = 0
	return p
}

// Unreadable is the purse of a land whose read failed: unknown, so blocked.
func Unreadable(land string, err error, now time.Time) Purse {
	reason := "the purse could not be read"
	if err != nil {
		reason += ": " + err.Error()
	}
	return Purse{
		Land: land, State: StateUnknown, Blocked: true, Reason: reason,
		MarginCredits: MinMarginCredits, ReadAt: now.UTC().Format(time.RFC3339),
	}
}

// UnmeteredReason is an unmetered purse's Reason.
const UnmeteredReason = "operator-paid: the land bills to the operator, so it has no credits and no wall"

// Unmetered is the purse of an operator land that is told as one (the home
// garden, step 9.13): operator-paid, so never blocked, with no credit figures.
func Unmetered(land string, now time.Time) Purse {
	return Purse{Land: land, State: StateUnmetered, Reason: UnmeteredReason, ReadAt: now.UTC().Format(time.RFC3339)}
}

// IsUnmetered reports an operator land's purse (Unmetered).
func (p Purse) IsUnmetered() bool { return p.State == StateUnmetered }

// Margin is max(MinMarginCredits, MarginFraction × granted).
func Margin(granted float64) float64 {
	return math.Max(MinMarginCredits, MarginFraction*granted)
}

func round2(v float64) *float64 {
	r := math.Round(v*100) / 100
	if r == 0 {
		r = 0 // no -0 in the JSON
	}
	return &r
}

// Compute is the §3.4 arithmetic. It never fails: what it cannot know makes
// the purse unknown, which blocks like the wall.
func Compute(in Inputs, now time.Time) Purse {
	p := Purse{
		Land: in.Land, GrantedCredits: *round2(in.GrantedCredits), MarginCredits: *round2(Margin(in.GrantedCredits)),
		RunningAgents: len(in.Agents), ReadAt: now.UTC().Format(time.RFC3339),
	}
	if !in.SpendSyncedAt.IsZero() {
		p.SpendSyncedAt = in.SpendSyncedAt.UTC().Format(time.RFC3339)
	}
	unknown := func(reason string) Purse {
		p.State, p.Blocked, p.Reason = StateUnknown, true, reason
		return p
	}
	if in.Rate == nil {
		return unknown("no credit rate is in force, so spend cannot be priced in credits")
	}
	m := in.Rate.LLMMultiplier
	p.RateVersion, p.LLMMultiplier = in.Rate.Version, m
	p.GrantedUSD = round2(math.Floor(in.GrantedCredits/m*100+1e-9) / 100)
	if !in.Account {
		return unknown(in.AccountProblem)
	}

	var inFlightUSD float64
	for _, a := range in.Agents {
		inFlightUSD += math.Max(0, a.LiveCostUSD-in.Baseline[a.ID])
	}
	spent, inFlight := m*in.SpentUSD, m*inFlightUSD
	remaining := in.GrantedCredits - spent - inFlight
	p.SpentCredits, p.InFlightCredits, p.RemainingCredits = round2(spent), round2(inFlight), round2(remaining)
	p.SpentUSD, p.InFlightUSD, p.RemainingUSD = round2(in.SpentUSD), round2(inFlightUSD), round2(remaining/m)
	if in.GrantedCredits > 0 {
		p.UsedPct = round2((spent + inFlight) / in.GrantedCredits * 100)
	}

	switch {
	case in.SpendSyncedAt.IsZero():
		return unknown(fmt.Sprintf("the spend sync has never stamped account %s", AccountName(in.Land)))
	case now.Sub(in.SpendSyncedAt) > UnknownAfter:
		return unknown(fmt.Sprintf("spend last synced %s ago (more than %s)", now.Sub(in.SpendSyncedAt).Truncate(time.Second), UnknownAfter))
	case remaining <= Margin(in.GrantedCredits):
		p.State, p.Blocked = StateWall, true
	default:
		p.State = StateOK
	}
	return p
}

// listAll pages a query to the end. A population past maxPages pages is an
// error, never a silent cut: a missing grant or agent would misstate money.
func listAll(ctx context.Context, src Source, q beadsapi.ListBeadsQuery) ([]*beadsapi.BeadDetail, error) {
	q.Limit, q.Sort = pageSize, "created_at"
	var out []*beadsapi.BeadDetail
	seen := map[string]bool{}
	for page := range maxPages {
		q.Offset = page * pageSize
		res, err := src.ListBeadsFiltered(ctx, q)
		if err != nil {
			return nil, err
		}
		if res == nil {
			return out, nil
		}
		for _, b := range res.Beads {
			if b != nil && !seen[b.ID] {
				seen[b.ID] = true
				out = append(out, b)
			}
		}
		if len(res.Beads) < pageSize {
			return out, nil
		}
	}
	return nil, fmt.Errorf("more than %d %v beads; refusing a partial read", maxPages*pageSize, q.Types)
}

// LoadRates reads every credit_rate version.
func LoadRates(ctx context.Context, src Source) ([]Rate, error) {
	beads, err := listAll(ctx, src, beadsapi.ListBeadsQuery{Types: []string{"credit_rate"}, Statuses: allStatuses})
	if err != nil {
		return nil, fmt.Errorf("reading the credit rate table: %w", err)
	}
	out := make([]Rate, 0, len(beads))
	for _, b := range beads {
		out = append(out, RateFromBead(b))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// LoadGrants reads a land's credit ledger, oldest first.
func LoadGrants(ctx context.Context, src Source, land string) ([]Grant, error) {
	beads, err := listAll(ctx, src, beadsapi.ListBeadsQuery{
		Types: []string{"credit_grant"}, Labels: []string{"project:" + land}, Statuses: allStatuses,
	})
	if err != nil {
		return nil, fmt.Errorf("reading %s's credit ledger: %w", land, err)
	}
	var out []Grant
	for _, b := range beads {
		if g := GrantFromBead(b); g.Land == land {
			out = append(out, g)
		}
	}
	return out, nil
}

// SumCredits totals a ledger.
func SumCredits(grants []Grant) float64 {
	var total float64
	for _, g := range grants {
		total += g.AmountCredits
	}
	return total
}

// deadAgentStates are the agent states whose live_cost_usd no longer moves.
var deadAgentStates = map[string]bool{"done": true, "failed": true}

// LiveCosts reads the live_cost_usd of every running agent of a land: agent
// beads labelled project:<land> that are not closed and not done or failed.
func LiveCosts(ctx context.Context, src Source, land string) ([]AgentCost, error) {
	beads, err := listAll(ctx, src, beadsapi.ListBeadsQuery{
		Types: []string{"agent"}, Labels: []string{"project:" + land}, Statuses: liveStatuses,
	})
	if err != nil {
		return nil, fmt.Errorf("reading %s's agents: %w", land, err)
	}
	var out []AgentCost
	for _, b := range beads {
		if b.Type != "agent" || (b.Fields["project"] != land && !slices.Contains(b.Labels, "project:"+land)) {
			continue
		}
		if p := strings.TrimSpace(b.Fields["project"]); p != "" && p != land {
			continue
		}
		if deadAgentStates[b.Fields["agent_state"]] {
			continue
		}
		out = append(out, AgentCost{ID: b.ID, LiveCostUSD: math.Max(0, FieldFloat(b.Fields, "live_cost_usd"))})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// BaselineJSON encodes a live-cost snapshot for FieldLiveCostBaseline.
func BaselineJSON(agents []AgentCost) string {
	m := make(map[string]float64, len(agents))
	for _, a := range agents {
		m[a.ID] = a.LiveCostUSD
	}
	raw, _ := json.Marshal(m)
	return string(raw)
}

// parseBaseline decodes FieldLiveCostBaseline; a missing or bad one is empty,
// which counts every agent's whole live cost as in flight (the safe side).
func parseBaseline(raw string) map[string]float64 {
	m := map[string]float64{}
	if strings.TrimSpace(raw) != "" {
		_ = json.Unmarshal([]byte(raw), &m)
	}
	return m
}

// land is one type:project bead as the purse needs it.
type land struct {
	// kind says the bead has a land_kind at all (operator or tenant).
	kind    bool
	tenant  bool
	frozen  bool
	account string
	bead    *beadsapi.BeadDetail
}

// readLands lists the live type:project beads by name. Two beads with one
// name fail closed for the kill switch: a bead with a land_kind wins, then a
// frozen one, then the lowest bead id (beadsapi.ListProjectBeads' rule), so a
// second bead with neither cannot make a frozen tenant land read as an
// unfrozen operator land. The daemon's FindProjectByName ranks the same way,
// and only an admin may create the duplicate at all.
func readLands(ctx context.Context, src Source) (map[string]land, error) {
	beads, err := listAll(ctx, src, beadsapi.ListBeadsQuery{Types: []string{"project"}, Statuses: liveStatuses})
	if err != nil {
		return nil, fmt.Errorf("reading projects: %w", err)
	}
	out := map[string]land{}
	for _, b := range beads {
		name := strings.TrimSpace(strings.TrimPrefix(b.Title, "Project: "))
		if name == "" {
			continue
		}
		kind := strings.TrimSpace(b.Fields["land_kind"])
		l := land{
			kind:    kind != "",
			tenant:  kind != "" && kind != beadsapi.LandKindOperator,
			frozen:  strings.TrimSpace(b.Fields["land_frozen"]) == "true",
			account: strings.TrimSpace(b.Fields["account"]),
			bead:    b,
		}
		if prev, ok := out[name]; ok && !l.outranks(prev) {
			continue
		}
		out[name] = l
	}
	return out, nil
}

// outranks says which of two project beads with one name is the land.
func (l land) outranks(o land) bool {
	if l.kind != o.kind {
		return l.kind
	}
	if l.frozen != o.frozen {
		return l.frozen
	}
	return l.bead.ID < o.bead.ID
}

// readAccounts lists the live account beads by name.
func readAccounts(ctx context.Context, src Source) (map[string]map[string]string, error) {
	beads, err := listAll(ctx, src, beadsapi.ListBeadsQuery{Types: []string{"account"}, Statuses: liveStatuses})
	if err != nil {
		return nil, fmt.Errorf("reading accounts: %w", err)
	}
	ids := map[string]string{}
	out := map[string]map[string]string{}
	for _, b := range beads {
		if ids[b.Title] != "" && ids[b.Title] < b.ID {
			continue
		}
		ids[b.Title] = b.ID
		out[b.Title] = b.Fields
	}
	return out, nil
}

// LandOf reads one project's land kind and kill switch (land_frozen) from its
// live project bead. ErrNotLand when no live project has the name.
func LandOf(ctx context.Context, src Source, name string) (tenant, frozen bool, err error) {
	lands, err := readLands(ctx, src)
	if err != nil {
		return false, false, err
	}
	l, ok := lands[name]
	if !ok {
		return false, false, fmt.Errorf("land %q: %w", name, ErrNotLand)
	}
	return l.tenant, l.frozen, nil
}

// Project is the live project bead a land is named by, and whether it is a
// tenant land. ErrNotLand when no live project has the name.
func Project(ctx context.Context, src Source, name string) (bead *beadsapi.BeadDetail, tenant bool, err error) {
	lands, err := readLands(ctx, src)
	if err != nil {
		return nil, false, err
	}
	l, ok := lands[name]
	if !ok {
		return nil, false, fmt.Errorf("land %q: %w", name, ErrNotLand)
	}
	return l.bead, l.tenant, nil
}

// KnownTenant says why name, which no live project bead carries, is still a
// tenant land: a closed project bead of that name with a tenant land_kind, or
// a lands-<name> account bead (live or closed). "" when there is neither, so
// the name is no land at all; an error when the beads cannot be read. The
// spawn wall asks it before letting ErrNotLand through, so closing a tenant
// land's project bead cannot lift its wall.
func KnownTenant(ctx context.Context, src Source, name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", nil
	}
	projects, err := listAll(ctx, src, beadsapi.ListBeadsQuery{Types: []string{"project"}, Statuses: allStatuses, Search: name})
	if err != nil {
		return "", fmt.Errorf("reading projects: %w", err)
	}
	for _, b := range projects {
		kind := strings.TrimSpace(b.Fields["land_kind"])
		if strings.TrimSpace(strings.TrimPrefix(b.Title, "Project: ")) == name && kind != "" && kind != beadsapi.LandKindOperator {
			return fmt.Sprintf("its project bead %s is %s", b.ID, b.Status), nil
		}
	}
	account := AccountName(name)
	accounts, err := listAll(ctx, src, beadsapi.ListBeadsQuery{Types: []string{"account"}, Statuses: allStatuses, Search: account})
	if err != nil {
		return "", fmt.Errorf("reading accounts: %w", err)
	}
	for _, b := range accounts {
		if b.Title == account {
			return fmt.Sprintf("it has no live project bead but has the account %s (%s)", b.ID, account), nil
		}
	}
	return "", nil
}

// TenantLands names every tenant land, sorted.
func TenantLands(ctx context.Context, src Source) ([]string, error) {
	projects, err := TenantProjects(ctx, src)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(projects))
	for name := range projects {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// TenantProjects is every tenant land's live project bead, by land name.
func TenantProjects(ctx context.Context, src Source) (map[string]*beadsapi.BeadDetail, error) {
	lands, err := readLands(ctx, src)
	if err != nil {
		return nil, err
	}
	out := map[string]*beadsapi.BeadDetail{}
	for name, l := range lands {
		if l.tenant {
			out[name] = l.bead
		}
	}
	return out, nil
}

// Read reads one land's purse. ErrNotLand and ErrOperatorLand say it has
// none; any other error is a failed read, which a caller deciding a spawn
// must treat as unknown.
func Read(ctx context.Context, src Source, name string, now time.Time) (Purse, error) {
	got, err := ReadMany(ctx, src, []string{name}, now)
	if err != nil {
		return Purse{}, err
	}
	return got[name], nil
}

// ReadMany reads several lands' purses, sharing the project, account and rate
// reads. Every name must be a tenant land.
func ReadMany(ctx context.Context, src Source, names []string, now time.Time) (map[string]Purse, error) {
	lands, err := readLands(ctx, src)
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		l, ok := lands[name]
		if !ok {
			return nil, fmt.Errorf("land %q: %w", name, ErrNotLand)
		}
		if !l.tenant {
			return nil, fmt.Errorf("land %q: %w", name, ErrOperatorLand)
		}
	}
	rates, err := LoadRates(ctx, src)
	if err != nil {
		return nil, err
	}
	accounts, err := readAccounts(ctx, src)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Purse, len(names))
	for _, name := range names {
		in := Inputs{Land: name, Rate: InForce(rates)}
		grants, err := LoadGrants(ctx, src, name)
		if err != nil {
			return nil, err
		}
		in.GrantedCredits = SumCredits(grants)
		if in.Agents, err = LiveCosts(ctx, src, name); err != nil {
			return nil, err
		}
		acct := AccountName(name)
		fields, has := accounts[acct]
		switch {
		case lands[name].account != acct:
			// Spend billed to any other account is not this purse's to count,
			// so a land that does not bill to its own is unknown, not empty.
			in.AccountProblem = fmt.Sprintf("land bills to account %q, not its own %s", lands[name].account, acct)
		case !has:
			in.AccountProblem = fmt.Sprintf("no account bead %s", acct)
		default:
			in.Account = true
			in.SpentUSD = math.Max(0, FieldFloat(fields, "total_cost_usd"))
			if t, err := time.Parse(time.RFC3339, strings.TrimSpace(fields[FieldSpendSyncedAt])); err == nil {
				in.SpendSyncedAt = t
			}
			in.Baseline = parseBaseline(fields[FieldLiveCostBaseline])
		}
		out[name] = Compute(in, now)
	}
	return out, nil
}
