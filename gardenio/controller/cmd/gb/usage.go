package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

var usageCmd = &cobra.Command{
	Use:   "usage",
	Short: "Show LLM spend per account from LiteLLM (API-equivalent $)",
	Long: `Displays cumulative spend per account in API-equivalent dollars across
multiple time windows.

By default (no LiteLLM master key), figures are read directly off each account
bead's cost fields, which the controller's spend_sync daemon keeps fresh from
LiteLLM — so this works in pods that have no master key. Supply a master key
(--litellm-key / LITELLM_MASTER_KEY), or pass --live with one, to instead query
LiteLLM per-account for the freshest read.

All dollar figures are API-equivalent pricing calculated by LiteLLM — they
do not represent actual subscription cost. Anthropic subscriptions have their
own opaque limits; use these numbers as an empirical signal for when a sub
is likely to throttle. The "since reset" column is the most useful: it tracks
spend since the last time an exhausted account was brought back to active.`,
	GroupID: "orchestration",
	RunE:    runUsage,
}

// liteLLMKeyInfo is the relevant subset of LiteLLM's /key/info response.
type liteLLMKeyInfo struct {
	Key            string   `json:"key"`
	KeyAlias       string   `json:"key_alias"`
	Spend          float64  `json:"spend"`
	MaxBudget      *float64 `json:"max_budget"`
	BudgetDuration string   `json:"budget_duration"`
	BudgetResetAt  string   `json:"budget_reset_at"`
	Models         []string `json:"models"`
}

// liteLLMKeyInfoResponse wraps the /key/info endpoint response.
type liteLLMKeyInfoResponse struct {
	Info liteLLMKeyInfo `json:"info"`
}

// liteLLMSpendLog is a single entry from /spend/logs.
type liteLLMSpendLog struct {
	APIKey    string  `json:"api_key"`
	Spend     float64 `json:"spend"`
	StartTime string  `json:"startTime"`
}

type accountUsage struct {
	Name       string  `json:"name"`
	Provider   string  `json:"provider"`
	Status     string  `json:"status"`
	Total      float64 `json:"total"`
	SinceReset float64 `json:"since_reset"`
	Daily      float64 `json:"daily"`
	Weekly     float64 `json:"weekly"`
	ResetAt    string  `json:"reset_at,omitempty"`
	KeySuffix  string  `json:"key_suffix"`
	Error      string  `json:"error,omitempty"`
}

func runUsage(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	litellmURL, _ := cmd.Flags().GetString("litellm-url")
	litellmMasterKey, _ := cmd.Flags().GetString("litellm-key")
	live, _ := cmd.Flags().GetBool("live")

	// Live mode reads spend straight from LiteLLM per-account, which needs the
	// master key. It is auto-detected when a key is present (or forced with
	// --live). Without a key we fall through to the keyless path, which reads
	// the cost fields the controller's spend_sync daemon keeps fresh on each
	// account bead — so gb usage works in pods that have no master key (e.g.
	// the auto-inspector pod, ka-0000003260 / ka-0000005931).
	liveMode := live || litellmMasterKey != ""

	if liveMode && litellmMasterKey == "" {
		return fmt.Errorf("--live requires a LiteLLM master key: set --litellm-key or LITELLM_MASTER_KEY")
	}
	if liveMode && litellmURL == "" {
		return fmt.Errorf("LiteLLM URL required for live mode: set --litellm-url or LITELLM_URL")
	}

	// Fetch raw account beads so we can read spend_at_last_reset and
	// last_reset_at directly from fields (these aren't on AccountInfo).
	result, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"account"},
		Statuses: []string{"open", "in_progress", "blocked", "deferred"},
		Limit:    50,
	})
	if err != nil {
		return fmt.Errorf("listing accounts: %w", err)
	}

	if len(result.Beads) == 0 {
		fmt.Fprintln(os.Stdout, "No account beads found.")
		return nil
	}

	// Sort by title for stable output.
	beads := result.Beads
	sort.Slice(beads, func(i, j int) bool { return beads[i].Title < beads[j].Title })

	if !liveMode {
		return runUsageKeyless(beads)
	}

	now := time.Now().UTC()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	weekStart := dayStart.AddDate(0, 0, -int(dayStart.Weekday()))

	var results []accountUsage
	client := &http.Client{Timeout: 10 * time.Second}

	for _, b := range beads {
		litellmKey := b.Fields["litellm_key"]
		if litellmKey == "" {
			continue
		}

		status := b.Fields["status"]
		if status == "" {
			status = "active"
		}

		usage := accountUsage{
			Name:      b.Title,
			Provider:  b.Fields["provider"],
			Status:    status,
			KeySuffix: keySuffix(litellmKey),
			ResetAt:   b.Fields["last_reset_at"],
		}

		// Per-account base_url override: a vkey minted on an in-cluster proxy is
		// only resolvable there, so an account carrying a non-empty base_url is
		// queried against that URL rather than the global --litellm-url. Empty
		// base_url falls back to the global URL (current behavior). Additive and
		// reversible (ka-0000003221).
		acctURL := accountLiteLLMURL(litellmURL, b.Fields["base_url"])

		// Total spend from /key/info.
		info, err := queryLiteLLMKeyInfo(ctx, client, acctURL, litellmMasterKey, litellmKey)
		if err != nil {
			usage.Error = err.Error()
			results = append(results, usage)
			continue
		}
		usage.Total = info.Spend

		// Since-reset: total minus snapshot taken at last account reset.
		if snap := b.Fields["spend_at_last_reset"]; snap != "" {
			if baseline, err := strconv.ParseFloat(snap, 64); err == nil {
				usage.SinceReset = info.Spend - baseline
			}
		}

		// Daily and weekly spend from /spend/logs.
		daily, weekly := queryPeriodSpend(ctx, client, acctURL, litellmMasterKey, litellmKey, dayStart, weekStart)
		usage.Daily = daily
		usage.Weekly = weekly

		results = append(results, usage)
	}

	if len(results) == 0 {
		fmt.Fprintln(os.Stdout, "No accounts with LiteLLM keys found.")
		return nil
	}

	if jsonOutput {
		printJSON(results)
		return nil
	}

	printUsageTable(results)
	fmt.Fprintln(os.Stderr, "\nAll figures are API-equivalent $ (LiteLLM pricing, not actual subscription cost).")

	return nil
}

// runUsageKeyless renders spend from the cost fields the controller's
// spend_sync daemon (cmd/controller/spend_sync.go) keeps fresh on each account
// bead: total/daily/weekly_cost_usd, plus spend_at_last_reset / last_reset_at
// from the account-reset path. This is the same read-only contract
// beads-viewer's /costs page uses (cmd/beads-viewer/costs.go) — no LiteLLM
// master key required, so gb usage works in pods that lack it. since-reset is
// total minus the snapshot taken at the last reset, matching live mode.
func runUsageKeyless(beads []*beadsapi.BeadDetail) error {
	results, latestSync := buildKeylessUsage(beads)

	if len(results) == 0 {
		fmt.Fprintln(os.Stdout, "No accounts with LiteLLM keys found.")
		return nil
	}

	if jsonOutput {
		printJSON(results)
		return nil
	}

	printUsageTable(results)

	syncNote := ""
	if !latestSync.IsZero() {
		syncNote = " (last bead sync " + latestSync.UTC().Format("2006-01-02 15:04 MST") + ")"
	}
	fmt.Fprintf(os.Stderr, "\nFigures come from controller spend-sync, read off account beads%s — no LiteLLM key in use.\n", syncNote)
	fmt.Fprintln(os.Stderr, "All figures are API-equivalent $ (LiteLLM pricing, not actual subscription cost).")

	return nil
}

// buildKeylessUsage maps the controller-synced cost fields on account beads to
// accountUsage rows, and returns the most recent bead UpdatedAt as the sync
// timestamp. Pure (no I/O) so the keyless mapping can be unit-tested. Accounts
// without a litellm_key are skipped, matching live mode's filter.
func buildKeylessUsage(beads []*beadsapi.BeadDetail) ([]accountUsage, time.Time) {
	var results []accountUsage
	var latestSync time.Time

	for _, b := range beads {
		if b == nil {
			continue
		}
		f := b.Fields
		if f["litellm_key"] == "" {
			continue
		}

		status := f["status"]
		if status == "" {
			status = "active"
		}

		usage := accountUsage{
			Name:      b.Title,
			Provider:  f["provider"],
			Status:    status,
			KeySuffix: keySuffix(f["litellm_key"]),
			ResetAt:   f["last_reset_at"],
		}

		total, _ := strconv.ParseFloat(f["total_cost_usd"], 64)
		usage.Total = total
		usage.Daily, _ = strconv.ParseFloat(f["daily_cost_usd"], 64)
		usage.Weekly, _ = strconv.ParseFloat(f["weekly_cost_usd"], 64)

		// Since-reset: total minus snapshot taken at last account reset
		// (same logic as live mode).
		if snap := f["spend_at_last_reset"]; snap != "" {
			if baseline, err := strconv.ParseFloat(snap, 64); err == nil {
				usage.SinceReset = total - baseline
			}
		}

		if b.UpdatedAt.After(latestSync) {
			latestSync = b.UpdatedAt
		}

		results = append(results, usage)
	}

	return results, latestSync
}

// printUsageTable renders the shared spend table to stdout. Both the live and
// keyless paths feed it the same accountUsage rows so the output is identical.
func printUsageTable(results []accountUsage) {
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ACCOUNT\tSTATUS\tSINCE RESET\tDAILY\tWEEKLY\tTOTAL\tKEY")
	for _, r := range results {
		sinceReset := fmtSpend(r.SinceReset)
		daily := fmtSpend(r.Daily)
		weekly := fmtSpend(r.Weekly)
		total := fmtSpend(r.Total)

		if r.Error != "" {
			sinceReset = "-"
			daily = "-"
			weekly = "-"
			total = "err"
		}
		if r.ResetAt == "" {
			sinceReset = total // no reset recorded yet — since-reset = total
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			r.Name, r.Status, sinceReset, daily, weekly, total, r.KeySuffix)
	}
	w.Flush()
}

// queryPeriodSpend fetches spend logs from LiteLLM for daily and weekly windows.
// Returns (daily, weekly) spend. Falls back to (0, 0) on error.
func queryPeriodSpend(ctx context.Context, client *http.Client, baseURL, masterKey, virtualKey string, dayStart, weekStart time.Time) (float64, float64) {
	logs := querySpendLogs(ctx, client, baseURL, masterKey, virtualKey, weekStart)
	if logs == nil {
		return 0, 0
	}

	var daily, weekly float64
	for _, entry := range logs {
		weekly += entry.Spend
		if t, err := time.Parse("2006-01-02T15:04:05", entry.StartTime); err == nil {
			if !t.Before(dayStart) {
				daily += entry.Spend
			}
		} else if t, err := time.Parse(time.RFC3339, entry.StartTime); err == nil {
			if !t.Before(dayStart) {
				daily += entry.Spend
			}
		}
	}
	return daily, weekly
}

// querySpendLogs fetches LiteLLM /spend/logs for a key since startDate.
func querySpendLogs(ctx context.Context, client *http.Client, baseURL, masterKey, virtualKey string, startDate time.Time) []liteLLMSpendLog {
	u := fmt.Sprintf("%s/spend/logs?api_key=%s&start_date=%s&end_date=%s",
		strings.TrimRight(baseURL, "/"),
		virtualKey,
		startDate.Format("2006-01-02"),
		time.Now().UTC().AddDate(0, 0, 1).Format("2006-01-02"),
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+masterKey)

	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil
	}

	var logs []liteLLMSpendLog
	if err := json.Unmarshal(body, &logs); err != nil {
		return nil
	}
	return logs
}

// queryLiteLLMKeyInfo calls LiteLLM GET /key/info?key=<virtualKey>.
func queryLiteLLMKeyInfo(ctx context.Context, client *http.Client, baseURL, masterKey, virtualKey string) (*liteLLMKeyInfo, error) {
	u := strings.TrimRight(baseURL, "/") + "/key/info?key=" + virtualKey

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+masterKey)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(body), 120))
	}

	var result liteLLMKeyInfoResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parsing response: %w", err)
	}
	return &result.Info, nil
}

// accountLiteLLMURL resolves the LiteLLM base URL for an account: the
// per-account base_url when non-empty, else the global URL. The chosen URL is
// trimmed by the downstream query helpers, so it is returned verbatim here.
func accountLiteLLMURL(globalURL, accountBaseURL string) string {
	if trimmed := strings.TrimSpace(accountBaseURL); trimmed != "" {
		return trimmed
	}
	return globalURL
}

// fmtSpend formats a dollar amount for display.
func fmtSpend(v float64) string {
	if v == 0 {
		return "-"
	}
	return fmt.Sprintf("$%.2f", v)
}

// keySuffix returns the last 6 characters of a key for display.
func keySuffix(key string) string {
	if len(key) <= 6 {
		return key
	}
	return "..." + key[len(key)-6:]
}

// truncate shortens a string to max length with ellipsis.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}

func init() {
	usageCmd.Flags().String("litellm-url", os.Getenv("LITELLM_URL"), "LiteLLM proxy URL (env: LITELLM_URL)")
	usageCmd.Flags().String("litellm-key", os.Getenv("LITELLM_MASTER_KEY"), "LiteLLM master/admin key (env: LITELLM_MASTER_KEY)")
	usageCmd.Flags().Bool("live", false, "query LiteLLM directly for freshest spend (requires a master key); default reads controller-synced account-bead fields")
}
