package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/lands/purse"

	"github.com/spf13/cobra"
)

// gb lands is the operator's purse ledger for tenant lands (Lands Track B
// spec gardenio/docs/superpowers/specs/2026-10-19-lands-track-b-design.md
// §3.4, B-D7; step 8.1).
//
// One credit is US$1 billed to the tenant. A land is charged its LiteLLM
// spend times the llm_multiplier of the credit rate in force (OQ1
// gd-7sTt20714N: markup; the values are owner decision gd-XQmEoHAqmp). Two
// kinds of bead hold it, both admin-gated in kbeads (lands_ledger.go):
//
//   - credit_rate: one version of the rate table. gb lands rates set writes
//     the next version; the highest is in force. None ships, and until the
//     operator sets one every grant is refused, so no grant is priced on a
//     guessed number.
//   - credit_grant: one ledger entry, labelled project:<land>, born closed.
//
// The land key's LiteLLM max_budget is US dollars at cost, the second wall
// behind the purse: granted credits / llm_multiplier, rounded down to the
// cent. It is always derived from the whole ledger, never incremented, so a
// re-run or a rate change converges on the same figure.

// allBeadStatuses lists every status, so a ledger read sees closed grants.
var allBeadStatuses = []string{"open", "in_progress", "blocked", "deferred", "closed"}

// creditRate is one version of the rate table, and creditGrant one ledger
// entry; internal/lands/purse owns both shapes, so the purse and the ledger
// read the beads the same way.
type (
	creditRate  = purse.Rate
	creditGrant = purse.Grant
)

// landsStore is the slice of the daemon client gb lands uses.
type landsStore interface {
	ListProjectBeads(ctx context.Context) (map[string]beadsapi.ProjectInfo, error)
	ListAccountBeads(ctx context.Context) (map[string]beadsapi.AccountInfo, error)
	ListAllBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
	CreateBeadDetail(ctx context.Context, req beadsapi.CreateBeadRequest) (*beadsapi.BeadDetail, error)
	DeleteBead(ctx context.Context, beadID string) error
}

// keyBudgetSetter sets a LiteLLM virtual key's max_budget (US dollars).
type keyBudgetSetter func(ctx context.Context, key string, maxBudgetUSD float64) error

// errNoCreditRate is the fail-closed refusal: no rate version exists.
var errNoCreditRate = errors.New("no credit rate is set, so no grant can be priced: the operator sets one with `gb lands rates set --llm-multiplier <x>` (values: owner decision gd-XQmEoHAqmp)")

// landKeyBudgetUSD is a land key's max_budget: credits / llm_multiplier,
// rounded down to the cent, never below zero (purse.KeyBudgetUSD, which the
// provisioner uses too).
func landKeyBudgetUSD(credits float64, rate creditRate) float64 {
	return purse.KeyBudgetUSD(credits, rate)
}

// loadCreditRates returns every rate version, oldest first.
func loadCreditRates(ctx context.Context, st landsStore) ([]creditRate, error) {
	res, err := st.ListAllBeadsFiltered(ctx, beadsapi.ListBeadsQuery{Types: []string{"credit_rate"}, Statuses: allBeadStatuses})
	if err != nil {
		return nil, fmt.Errorf("reading the credit rate table: %w", err)
	}
	var out []creditRate
	for _, b := range res.Beads {
		out = append(out, purse.RateFromBead(b))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// currentCreditRate is the highest rate version, or errNoCreditRate.
func currentCreditRate(ctx context.Context, st landsStore) (creditRate, error) {
	rates, err := loadCreditRates(ctx, st)
	if err != nil {
		return creditRate{}, err
	}
	r := purse.InForce(rates)
	if r == nil {
		return creditRate{}, errNoCreditRate
	}
	return *r, nil
}

// loadCreditGrants returns a land's ledger, oldest first.
func loadCreditGrants(ctx context.Context, st landsStore, land string) ([]creditGrant, error) {
	res, err := st.ListAllBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types: []string{"credit_grant"}, Labels: []string{"project:" + land}, Statuses: allBeadStatuses,
	})
	if err != nil {
		return nil, fmt.Errorf("reading %s's credit ledger: %w", land, err)
	}
	var out []creditGrant
	for _, b := range res.Beads {
		if g := purse.GrantFromBead(b); g.Land == land {
			out = append(out, g)
		}
	}
	return out, nil
}

func sumCredits(grants []creditGrant) float64 { return purse.SumCredits(grants) }

// tenantLand resolves a land that has a purse: a tenant project. It also
// returns every project, which landKey needs.
func tenantLand(ctx context.Context, st landsStore, land string) (beadsapi.ProjectInfo, map[string]beadsapi.ProjectInfo, error) {
	projects, err := st.ListProjectBeads(ctx)
	if err != nil {
		return beadsapi.ProjectInfo{}, nil, err
	}
	p, ok := projects[land]
	if !ok {
		return beadsapi.ProjectInfo{}, nil, fmt.Errorf("land %q is not a project", land)
	}
	if p.IsOperatorLand() {
		return beadsapi.ProjectInfo{}, nil, fmt.Errorf("land %q is an operator land (land_kind=%q); only tenant lands have a purse", land, p.LandKind)
	}
	return p, projects, nil
}

// landAccount is the per-land LiteLLM account a tenant land is billed to
// (spec §3.4).
func landAccount(land string) string { return "lands-" + land }

// landKey is the LiteLLM virtual key of a tenant land's own account, or ""
// when the land has none yet (not provisioned). land_kind and account are
// project fields a static token can write, so the land's word is not enough
// to aim a max_budget write: the account must be lands-<land>, no other
// project may bill to it, and its key must not be another account's key.
// Anything else is an error, so a misconfigured project can never zero the
// fleet's key.
func landKey(accounts map[string]beadsapi.AccountInfo, projects map[string]beadsapi.ProjectInfo, land string) (string, error) {
	acct := projects[land].Account
	if acct == "" {
		return "", nil
	}
	if acct != landAccount(land) {
		return "", fmt.Errorf("land %q bills to account %q, not its own %s; refusing to write a budget to another account's key", land, acct, landAccount(land))
	}
	var sharing []string
	for name, p := range projects {
		if name != land && p.Account == acct {
			sharing = append(sharing, name)
		}
	}
	if len(sharing) > 0 {
		sort.Strings(sharing)
		return "", fmt.Errorf("account %s is shared with %s; a tenant land's key is its own, so no budget is written", acct, strings.Join(sharing, ", "))
	}
	key := strings.TrimSpace(accounts[acct].LiteLLMKey)
	if key == "" {
		return "", nil
	}
	var same []string
	for name, a := range accounts {
		if name != acct && strings.TrimSpace(a.LiteLLMKey) == key {
			same = append(same, name)
		}
	}
	if len(same) > 0 {
		sort.Strings(same)
		return "", fmt.Errorf("account %s's LiteLLM key is also account %s's; refusing to write a budget to a shared key", acct, strings.Join(same, ", "))
	}
	return key, nil
}

// creditAddInput is one gb lands credit add.
type creditAddInput struct {
	Land       string
	Credits    float64
	Reason     string
	DecisionID string
	GrantedBy  string
}

// creditAddResult is what a grant did.
type creditAddResult struct {
	Grant        creditGrant `json:"grant"`
	TotalCredits float64     `json:"total_credits"`
	MaxBudgetUSD float64     `json:"max_budget_usd"`
	Rate         creditRate  `json:"rate"`
}

// addCredit writes a grant and sets the land key's max_budget from the whole
// ledger; when LiteLLM refuses, the grant is deleted again so the ledger
// never shows credits the second wall does not.
func addCredit(ctx context.Context, st landsStore, setBudget keyBudgetSetter, in creditAddInput) (*creditAddResult, error) {
	if in.Credits == 0 || math.IsNaN(in.Credits) || math.IsInf(in.Credits, 0) {
		return nil, fmt.Errorf("credits must be a nonzero number")
	}
	if in.Credits < 0 && strings.TrimSpace(in.Reason) == "" {
		return nil, fmt.Errorf("a negative grant (a refund or a correction) needs --reason")
	}
	if strings.TrimSpace(in.GrantedBy) == "" {
		return nil, fmt.Errorf("no operator identity to record as granted_by (set --operated-by or BEADS_OPERATED_BY)")
	}
	p, projects, err := tenantLand(ctx, st, in.Land)
	if err != nil {
		return nil, err
	}
	rate, err := currentCreditRate(ctx, st)
	if err != nil {
		return nil, err
	}
	accounts, err := st.ListAccountBeads(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading account beads: %w", err)
	}
	key, err := landKey(accounts, projects, in.Land)
	if err != nil {
		return nil, err
	}
	if key == "" {
		return nil, fmt.Errorf("land %q has no LiteLLM key (project account %q has no litellm_key); a grant without its second wall is refused", in.Land, p.Account)
	}

	fields := map[string]any{
		"land": in.Land, "amount_credits": in.Credits, "granted_by": in.GrantedBy, "rate_version": rate.Version,
	}
	if r := strings.TrimSpace(in.Reason); r != "" {
		fields["reason"] = r
	}
	if d := strings.TrimSpace(in.DecisionID); d != "" {
		fields["decision_id"] = d
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	created, err := st.CreateBeadDetail(ctx, beadsapi.CreateBeadRequest{
		Title:  fmt.Sprintf("Credit grant: %s %+g credits", in.Land, in.Credits),
		Type:   "credit_grant",
		Kind:   "data",
		Status: "closed",
		Labels: []string{"project:" + in.Land},
		Fields: raw,
	})
	if err != nil {
		return nil, fmt.Errorf("writing the credit_grant: %w", err)
	}
	grant := creditGrant{ID: created.ID, Land: in.Land, AmountCredits: in.Credits, Reason: strings.TrimSpace(in.Reason),
		GrantedBy: in.GrantedBy, DecisionID: strings.TrimSpace(in.DecisionID), RateVersion: rate.Version}

	// Derive from the ledger as it now stands, this grant included.
	grants, err := loadCreditGrants(ctx, st, in.Land)
	if err == nil {
		seen := false
		for _, g := range grants {
			seen = seen || g.ID == created.ID
		}
		if !seen {
			grants = append(grants, grant)
		}
	}
	var budget, total float64
	if err == nil {
		total = sumCredits(grants)
		budget = landKeyBudgetUSD(total, rate)
		err = setBudget(ctx, key, budget)
	}
	if err != nil {
		if delErr := st.DeleteBead(ctx, created.ID); delErr != nil {
			return nil, fmt.Errorf("raising %s's key max_budget failed (%v), and rolling back credit_grant %s failed too (%v): delete it by hand so the ledger matches LiteLLM", in.Land, err, created.ID, delErr)
		}
		return nil, fmt.Errorf("raising %s's key max_budget failed, credit_grant %s rolled back: %w", in.Land, created.ID, err)
	}
	return &creditAddResult{Grant: grant, TotalCredits: total, MaxBudgetUSD: budget, Rate: rate}, nil
}

// landReapply is the outcome of re-deriving one land's key budget.
type landReapply struct {
	Land         string  `json:"land"`
	Action       string  `json:"action"` // set | skipped | failed
	TotalCredits float64 `json:"total_credits"`
	MaxBudgetUSD float64 `json:"max_budget_usd"`
	Note         string  `json:"note,omitempty"`
}

// reapplyCreditRate sets every tenant land key's max_budget from its ledger at
// rate. A land with no key yet (not provisioned) is skipped; a LiteLLM error
// is reported and the rest carry on.
func reapplyCreditRate(ctx context.Context, st landsStore, setBudget keyBudgetSetter, rate creditRate) ([]landReapply, error) {
	projects, err := st.ListProjectBeads(ctx)
	if err != nil {
		return nil, err
	}
	accounts, err := st.ListAccountBeads(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading account beads: %w", err)
	}
	var names []string
	for name, p := range projects {
		if !p.IsOperatorLand() {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	var out []landReapply
	for _, name := range names {
		r := landReapply{Land: name}
		key, keyErr := landKey(accounts, projects, name)
		grants, err := loadCreditGrants(ctx, st, name)
		switch {
		case keyErr != nil:
			r.Action, r.Note = "failed", keyErr.Error()
		case err != nil:
			r.Action, r.Note = "failed", err.Error()
		case key == "":
			r.Action, r.Note = "skipped", "no LiteLLM key yet"
			r.TotalCredits = sumCredits(grants)
		default:
			r.TotalCredits = sumCredits(grants)
			r.MaxBudgetUSD = landKeyBudgetUSD(r.TotalCredits, rate)
			if err := setBudget(ctx, key, r.MaxBudgetUSD); err != nil {
				r.Action, r.Note = "failed", err.Error()
			} else {
				r.Action = "set"
			}
		}
		out = append(out, r)
	}
	return out, nil
}

// setCreditRate writes the next rate version. The idempotency key makes two
// concurrent sets of the same version collapse into one bead, which the
// caller then sees as a mismatch instead of two rows claiming one version.
func setCreditRate(ctx context.Context, st landsStore, rate creditRate, now time.Time) (creditRate, error) {
	if !(rate.LLMMultiplier > 0) || math.IsInf(rate.LLMMultiplier, 0) {
		return creditRate{}, fmt.Errorf("--llm-multiplier must be greater than 0 (credits charged per US$1 of LiteLLM spend)")
	}
	if rate.ComputeCreditsPerAgentHour < 0 {
		return creditRate{}, fmt.Errorf("--compute-credits-per-agent-hour must not be negative")
	}
	rates, err := loadCreditRates(ctx, st)
	if err != nil {
		return creditRate{}, err
	}
	rate.Version = 1
	if len(rates) > 0 {
		rate.Version = rates[len(rates)-1].Version + 1
	}
	rate.EffectiveAt = now.UTC().Format(time.RFC3339)
	fields := map[string]any{
		"version": rate.Version, "effective_at": rate.EffectiveAt,
		"llm_multiplier": rate.LLMMultiplier, "compute_credits_per_agent_hour": rate.ComputeCreditsPerAgentHour,
	}
	for k, v := range map[string]string{"set_by": rate.SetBy, "decision_id": rate.DecisionID, "reason": rate.Reason} {
		if strings.TrimSpace(v) != "" {
			fields[k] = strings.TrimSpace(v)
		}
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		return creditRate{}, err
	}
	created, err := st.CreateBeadDetail(ctx, beadsapi.CreateBeadRequest{
		Title:          fmt.Sprintf("Credit rate v%d: x%g", rate.Version, rate.LLMMultiplier),
		Type:           "credit_rate",
		Kind:           "config",
		Fields:         raw,
		IdempotencyKey: fmt.Sprintf("credit_rate:v%d", rate.Version),
	})
	if err != nil {
		return creditRate{}, fmt.Errorf("writing credit rate v%d: %w", rate.Version, err)
	}
	// The idempotency key is not bound to a type, so the bead that comes
	// back must be the rate just asked for; anything else (another writer's
	// v<N>, or a bead squatting on the key) fails closed.
	if created == nil || created.Type != "credit_rate" {
		id, typ := "", ""
		if created != nil {
			id, typ = created.ID, created.Type
		}
		return creditRate{}, fmt.Errorf("idempotency key credit_rate:v%d returned bead %s of type %q, not a credit_rate; no rate was set (delete or re-key that bead, then re-run)", rate.Version, id, typ)
	}
	if v := purse.FieldInt(created.Fields, "version"); v != rate.Version {
		return creditRate{}, fmt.Errorf("idempotency key credit_rate:v%d returned bead %s with version %d; no rate was set", rate.Version, created.ID, v)
	}
	if got := purse.FieldFloat(created.Fields, "llm_multiplier"); got != rate.LLMMultiplier ||
		purse.FieldFloat(created.Fields, "compute_credits_per_agent_hour") != rate.ComputeCreditsPerAgentHour {
		return creditRate{}, fmt.Errorf("credit rate v%d was written concurrently with llm_multiplier %g (bead %s); check gb lands rates show and re-run", rate.Version, got, created.ID)
	}
	rate.ID = created.ID
	return rate, nil
}

// landsKeyBudgetSetter resolves the LiteLLM endpoint from cmd's flags and
// returns a setter that POSTs /key/update.
func landsKeyBudgetSetter(cmd *cobra.Command) (keyBudgetSetter, error) {
	baseURL, masterKey, err := litellmKeyEndpoint(cmd)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 15 * time.Second}
	return func(ctx context.Context, key string, maxBudgetUSD float64) error {
		v := maxBudgetUSD
		_, err := litellmKeyUpdate(ctx, client, baseURL, masterKey, litellmKeyUpdateRequest{Key: key, MaxBudget: &v})
		return err
	}, nil
}

// landsOperator is who the ledger records as granting or setting.
func landsOperator() string {
	if s := strings.TrimSpace(operatedBy); s != "" {
		return s
	}
	return strings.TrimSpace(actor)
}

var landsCmd = &cobra.Command{
	Use:   "lands",
	Short: "Tenant lands: purses, credit grants, the rate table, provisioning, invites, close and the kill switches (admin only)",
	Long: `Operator tools for tenant lands (Lands Track B spec §3.3, §3.4).

One credit is US$1 billed to the tenant; a land is charged its LiteLLM spend
times the llm_multiplier of the credit rate in force. Every write is a bead
type only a gardenio-admin may write (credit_grant, credit_rate), and every key
change needs the LiteLLM master key (--litellm-key / LITELLM_MASTER_KEY).

  gb lands rates show|set|reapply   the versioned rate table
  gb lands credit add|list          a land's credit ledger
  gb lands purse [land]             what is left: granted, spent, in flight, the wall
  gb lands notes <land>             the steward's notes: read, or write back at the end of a run
  gb lands provision <app> [--retry] where provisioning is; retry a failed one
  gb lands close <land>             take a land apart (the controller does it)
  gb lands invite <email>           a sign-in invite by email (GitHub sign-in is invite-only)
  gb lands invites | uninvite <id>  the pending invites; end one
  gb lands freeze|thaw <land>       one land's kill switch (land_frozen, spec §5.5)
  gb lands pause|resume             every tenant land's kill switch (lands-pause gate)`,
	GroupID: "orchestration",
}

var landsCreditCmd = &cobra.Command{
	Use:   "credit",
	Short: "Grant credits to a tenant land and list its ledger",
}

var landsCreditAddCmd = &cobra.Command{
	Use:   "add <land> <credits>",
	Short: "Grant credits and raise the land key's LiteLLM max_budget in one step",
	Long: `Writes a credit_grant bead for the land and sets its LiteLLM key's max_budget
to the whole ledger at cost: granted credits / llm_multiplier, rounded down to
the cent. If LiteLLM refuses, the grant is deleted again.

Refused until a credit rate exists (gb lands rates set): no grant is priced on
a guessed number. A negative amount (a refund or a correction) needs --reason.
SLA credits are ordinary grants whose reason names the incident.`,
	Example: `  gb lands credit add tomato-patch 50 --reason "welcome grant" --decision gd-abc
  gb lands credit add tomato-patch -5 --reason "duplicate grant gd-xyz"`,
	Args: cobra.ExactArgs(2),
	RunE: runLandsCreditAdd,
}

var landsCreditListCmd = &cobra.Command{
	Use:   "list <land>",
	Short: "Print a land's credit ledger in credits and at-cost US dollars",
	Args:  cobra.ExactArgs(1),
	RunE:  runLandsCreditList,
}

var landsRatesCmd = &cobra.Command{
	Use:   "rates",
	Short: "Show or set the versioned credit rate table",
}

var landsRatesShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Print the rate in force and every earlier version",
	Args:  cobra.NoArgs,
	RunE:  runLandsRatesShow,
}

var landsRatesSetCmd = &cobra.Command{
	Use:   "set",
	Short: "Write the next rate version and re-derive every tenant land key's max_budget",
	Long: `Writes credit_rate version N+1, in force from now, then sets every tenant land
key's max_budget to its ledger at the new rate (granted / llm_multiplier,
rounded down). A land whose key update fails is reported; fix the cause and run
gb lands rates reapply.`,
	Example: `  gb lands rates set --llm-multiplier 1.5 --decision gd-XQmEoHAqmp`,
	Args:    cobra.NoArgs,
	RunE:    runLandsRatesSet,
}

var landsRatesReapplyCmd = &cobra.Command{
	Use:   "reapply",
	Short: "Re-derive every tenant land key's max_budget at the rate in force",
	Args:  cobra.NoArgs,
	RunE:  runLandsRatesReapply,
}

var landsPurseCmd = &cobra.Command{
	Use:   "purse [land]",
	Short: "Print a tenant land's purse: granted, spent, in flight, remaining and the wall",
	Long: `Reads a tenant land's purse (internal/lands/purse, spec §3.4), the figure the
spawn wall and /garden read, in credits and at cost:

  remaining = granted - spent - in_flight
  wall      = remaining <= max(2 credits, 5% of granted)

spent is the land account's synced LiteLLM spend x llm_multiplier; in_flight
is what its running agents spent since that sync. When the spend has not
synced for more than 15 minutes, or the purse cannot be read, it is unknown,
and unknown blocks new spawns like the wall. With no land, every tenant land.`,
	Example: `  gb lands purse tomato-patch
  gb lands purse --json`,
	Args: cobra.MaximumNArgs(1),
	RunE: runLandsPurse,
}

// readLandPurses reads the named land's purse, or every tenant land's.
func readLandPurses(ctx context.Context, src purse.Source, args []string, now time.Time) ([]purse.Purse, error) {
	names := args
	if len(names) == 0 {
		var err error
		if names, err = purse.TenantLands(ctx, src); err != nil {
			return nil, err
		}
	}
	got, err := purse.ReadMany(ctx, src, names, now)
	if err != nil {
		return nil, err
	}
	out := make([]purse.Purse, 0, len(names))
	for _, n := range names {
		out = append(out, got[n])
	}
	return out, nil
}

// creditFig formats a purse figure that may be unknown.
func creditFig(v *float64, format string) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprintf(format, *v)
}

// printPurse is the operator's view: credits and the at-cost dollars.
func printPurse(w io.Writer, p purse.Purse) {
	head := fmt.Sprintf("%s: %s", p.Land, p.State)
	if p.Blocked {
		head += " (new spawns refused)"
	}
	fmt.Fprintln(w, head)
	if p.Reason != "" {
		fmt.Fprintf(w, "  why        %s\n", p.Reason)
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintf(tw, "  remaining\t%s\tcredits\t%s\t\n", creditFig(p.RemainingCredits, "%.2f"), creditFig(p.RemainingUSD, "$%.2f"))
	fmt.Fprintf(tw, "  granted\t%.2f\tcredits\t%s\t\n", p.GrantedCredits, creditFig(p.GrantedUSD, "$%.2f"))
	fmt.Fprintf(tw, "  spent\t%s\tcredits\t%s\t\n", creditFig(p.SpentCredits, "%.2f"), creditFig(p.SpentUSD, "$%.2f"))
	fmt.Fprintf(tw, "  in flight\t%s\tcredits\t%s\t\n", creditFig(p.InFlightCredits, "%.2f"), creditFig(p.InFlightUSD, "$%.2f"))
	fmt.Fprintf(tw, "  margin\t%.2f\tcredits\t\t\n", p.MarginCredits)
	_ = tw.Flush()
	rate := "no rate in force"
	if p.RateVersion > 0 {
		rate = fmt.Sprintf("rate v%d x%g", p.RateVersion, p.LLMMultiplier)
	}
	synced := "never synced"
	if p.SpendSyncedAt != "" {
		synced = "spend synced " + p.SpendSyncedAt
	}
	fmt.Fprintf(w, "  %s, %d running agent(s), %s, %s used\n", rate, p.RunningAgents, synced, creditFig(p.UsedPct, "%.0f%%"))
}

func runLandsPurse(cmd *cobra.Command, args []string) error {
	purses, err := readLandPurses(cmd.Context(), daemon, args, time.Now())
	if err != nil {
		return err
	}
	if jsonOutput {
		if len(args) == 1 {
			return json.NewEncoder(os.Stdout).Encode(purses[0])
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"lands": purses})
	}
	if len(purses) == 0 {
		fmt.Println("no tenant lands")
	}
	for i, p := range purses {
		if i > 0 {
			fmt.Println()
		}
		printPurse(os.Stdout, p)
	}
	return nil
}

func runLandsCreditAdd(cmd *cobra.Command, args []string) error {
	credits, err := strconv.ParseFloat(args[1], 64)
	if err != nil {
		return fmt.Errorf("credits %q is not a number", args[1])
	}
	setBudget, err := landsKeyBudgetSetter(cmd)
	if err != nil {
		return err
	}
	reason, _ := cmd.Flags().GetString("reason")
	decision, _ := cmd.Flags().GetString("decision")
	res, err := addCredit(cmd.Context(), daemon, setBudget, creditAddInput{
		Land: args[0], Credits: credits, Reason: reason, DecisionID: decision, GrantedBy: landsOperator(),
	})
	if err != nil {
		return err
	}
	if jsonOutput {
		return json.NewEncoder(os.Stdout).Encode(res)
	}
	fmt.Printf("granted %s %+g credits (%s); ledger %g credits; key max_budget $%.2f at rate v%d (x%g)\n",
		res.Grant.Land, res.Grant.AmountCredits, res.Grant.ID, res.TotalCredits, res.MaxBudgetUSD, res.Rate.Version, res.Rate.LLMMultiplier)
	return nil
}

// creditLedger is gb lands credit list's --json output.
type creditLedger struct {
	Land          string        `json:"land"`
	Grants        []creditGrant `json:"grants"`
	TotalCredits  float64       `json:"total_credits"`
	Rate          *creditRate   `json:"rate"`
	AtCostUSD     *float64      `json:"at_cost_usd"`
	AtCostUSDNote string        `json:"at_cost_usd_note,omitempty"`
}

func buildCreditLedger(ctx context.Context, st landsStore, land string) (*creditLedger, error) {
	if _, _, err := tenantLand(ctx, st, land); err != nil {
		return nil, err
	}
	grants, err := loadCreditGrants(ctx, st, land)
	if err != nil {
		return nil, err
	}
	l := &creditLedger{Land: land, Grants: grants, TotalCredits: sumCredits(grants)}
	rate, err := currentCreditRate(ctx, st)
	switch {
	case errors.Is(err, errNoCreditRate):
		l.AtCostUSDNote = "no credit rate set"
	case err != nil:
		return nil, err
	default:
		l.Rate = &rate
		usd := landKeyBudgetUSD(l.TotalCredits, rate)
		l.AtCostUSD = &usd
	}
	return l, nil
}

func printCreditLedger(w io.Writer, l *creditLedger) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	usdHead := "AT COST"
	if l.Rate != nil {
		usdHead = fmt.Sprintf("AT COST (v%d)", l.Rate.Version)
	}
	fmt.Fprintf(tw, "GRANT\tWHEN\tCREDITS\t%s\tRATE\tBY\tDECISION\tREASON\n", usdHead)
	for _, g := range l.Grants {
		usd := "-"
		if l.Rate != nil {
			usd = fmt.Sprintf("$%.2f", g.AmountCredits/l.Rate.LLMMultiplier)
		}
		fmt.Fprintf(tw, "%s\t%s\t%+g\t%s\tv%d\t%s\t%s\t%s\n", g.ID, orNone(g.CreatedAt), g.AmountCredits, usd,
			g.RateVersion, g.GrantedBy, orNone(g.DecisionID), g.Reason)
	}
	_ = tw.Flush()
	if l.AtCostUSD != nil {
		fmt.Fprintf(w, "\n%s: %g credits granted; $%.2f at cost (the key's max_budget) at rate v%d, x%g\n",
			l.Land, l.TotalCredits, *l.AtCostUSD, l.Rate.Version, l.Rate.LLMMultiplier)
		return
	}
	fmt.Fprintf(w, "\n%s: %g credits granted; at cost unknown: %s\n", l.Land, l.TotalCredits, l.AtCostUSDNote)
}

func runLandsCreditList(cmd *cobra.Command, args []string) error {
	l, err := buildCreditLedger(cmd.Context(), daemon, args[0])
	if err != nil {
		return err
	}
	if jsonOutput {
		return json.NewEncoder(os.Stdout).Encode(l)
	}
	printCreditLedger(os.Stdout, l)
	return nil
}

func runLandsRatesShow(cmd *cobra.Command, _ []string) error {
	rates, err := loadCreditRates(cmd.Context(), daemon)
	if err != nil {
		return err
	}
	agent := ""
	if len(rates) == 0 {
		// Only an empty table needs to know who is asking (GET /v1/me).
		agent, _ = bridgeAgentIdentity(cmd.Context())
	}
	return printCreditRates(os.Stdout, rates, agent, jsonOutput)
}

// printCreditRates writes the rate table, newest first. agent is the caller's
// per-agent token's agent when the table came back empty: kbeads scopes a
// per-agent token's list to its own project, and the credit_rate bead carries
// no project label, so that empty table says nothing about whether a rate is
// set (gd-nVhTwz4aBC), and gb says so instead of claiming none is.
func printCreditRates(w io.Writer, rates []creditRate, agent string, asJSON bool) error {
	hidden := len(rates) == 0 && agent != ""
	note := ""
	if hidden {
		note = fmt.Sprintf("no rate is visible to this token: per-agent tokens (this one is %s's) cannot read the fleet credit_rate; an admin runs gb lands rates show", agent)
	}
	if asJSON {
		out := map[string]any{"rates": rates, "visible_to_caller": !hidden}
		if note != "" {
			out["note"] = note
		}
		return json.NewEncoder(w).Encode(out)
	}
	if hidden {
		fmt.Fprintln(w, note)
		return nil
	}
	if len(rates) == 0 {
		fmt.Fprintln(w, "no credit rate is set: credit grants are refused until `gb lands rates set` writes one")
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "VERSION\tEFFECTIVE\tLLM MULTIPLIER\tCOMPUTE/AGENT-HOUR\tSET BY\tDECISION\t")
	for i := len(rates) - 1; i >= 0; i-- {
		r := rates[i]
		mark := ""
		if i == len(rates)-1 {
			mark = "in force"
		}
		fmt.Fprintf(tw, "v%d\t%s\t%g\t%g\t%s\t%s\t%s\n", r.Version, r.EffectiveAt, r.LLMMultiplier,
			r.ComputeCreditsPerAgentHour, r.SetBy, orNone(r.DecisionID), mark)
	}
	return tw.Flush()
}

func printReapply(w io.Writer, rate creditRate, results []landReapply) error {
	failed := 0
	for _, r := range results {
		switch r.Action {
		case "set":
			fmt.Fprintf(w, "  %s: %g credits -> max_budget $%.2f\n", r.Land, r.TotalCredits, r.MaxBudgetUSD)
		default:
			fmt.Fprintf(w, "  %s: %s (%s)\n", r.Land, r.Action, r.Note)
		}
		if r.Action == "failed" {
			failed++
		}
	}
	if len(results) == 0 {
		fmt.Fprintln(w, "  no tenant lands")
	}
	if failed > 0 {
		return fmt.Errorf("%d tenant land key(s) not updated to rate v%d; fix the cause and run gb lands rates reapply", failed, rate.Version)
	}
	return nil
}

func runLandsRatesSet(cmd *cobra.Command, _ []string) error {
	if !cmd.Flags().Changed("llm-multiplier") {
		return fmt.Errorf("--llm-multiplier is required (credits charged per US$1 of LiteLLM spend)")
	}
	ctx := cmd.Context()
	setBudget, err := landsKeyBudgetSetter(cmd)
	if err != nil {
		return err
	}
	mult, _ := cmd.Flags().GetFloat64("llm-multiplier")
	compute, _ := cmd.Flags().GetFloat64("compute-credits-per-agent-hour")
	decision, _ := cmd.Flags().GetString("decision")
	reason, _ := cmd.Flags().GetString("reason")
	rate, err := setCreditRate(ctx, daemon, creditRate{
		LLMMultiplier: mult, ComputeCreditsPerAgentHour: compute, SetBy: landsOperator(), DecisionID: decision, Reason: reason,
	}, time.Now())
	if err != nil {
		return err
	}
	results, err := reapplyCreditRate(ctx, daemon, setBudget, rate)
	if jsonOutput {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"rate": rate, "lands": results})
		if err != nil {
			return err
		}
		return printReapply(io.Discard, rate, results)
	}
	fmt.Printf("credit rate v%d in force: x%g LLM spend, %g credits per agent-hour (%s)\n",
		rate.Version, rate.LLMMultiplier, rate.ComputeCreditsPerAgentHour, rate.ID)
	if err != nil {
		return fmt.Errorf("rate v%d written, but re-deriving land keys failed: %w; run gb lands rates reapply", rate.Version, err)
	}
	return printReapply(os.Stdout, rate, results)
}

func runLandsRatesReapply(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	rate, err := currentCreditRate(ctx, daemon)
	if err != nil {
		return err
	}
	setBudget, err := landsKeyBudgetSetter(cmd)
	if err != nil {
		return err
	}
	results, err := reapplyCreditRate(ctx, daemon, setBudget, rate)
	if err != nil {
		return err
	}
	if jsonOutput {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"rate": rate, "lands": results})
		return printReapply(io.Discard, rate, results)
	}
	fmt.Printf("re-deriving tenant land keys at rate v%d (x%g):\n", rate.Version, rate.LLMMultiplier)
	return printReapply(os.Stdout, rate, results)
}

func init() {
	addEndpointFlags(landsCreditAddCmd)
	landsCreditAddCmd.Flags().String("reason", "", "why the grant is made (required for a negative amount)")
	landsCreditAddCmd.Flags().String("decision", "", "decision bead id behind the grant")
	landsCreditCmd.AddCommand(landsCreditAddCmd, landsCreditListCmd)

	addEndpointFlags(landsRatesSetCmd)
	landsRatesSetCmd.Flags().Float64("llm-multiplier", 0, "credits charged per US$1 of LiteLLM spend (required, > 0)")
	landsRatesSetCmd.Flags().Float64("compute-credits-per-agent-hour", 0, "credits charged per agent-hour of compute")
	landsRatesSetCmd.Flags().String("decision", "", "decision bead id that chose the rate")
	landsRatesSetCmd.Flags().String("reason", "", "why the rate changes")
	addEndpointFlags(landsRatesReapplyCmd)
	landsRatesCmd.AddCommand(landsRatesShowCmd, landsRatesSetCmd, landsRatesReapplyCmd)

	landsCmd.AddCommand(landsCreditCmd, landsRatesCmd, landsPurseCmd)
}
