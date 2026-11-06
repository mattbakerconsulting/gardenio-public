package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"text/tabwriter"
	"time"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

// probeStaleThreshold is the maximum age of probe_checked_at before this view
// stops presenting the probe's throttle/utilization numbers as current. Kept in
// sync with the controller's constant of the same name (auth_failover.go); gb is
// a separate package and deliberately carries no dependency on the controller.
const probeStaleThreshold = 20 * time.Minute

var accountLimitsCmd = &cobra.Command{
	Use:   "limits",
	Short: "Show Anthropic rate-limit utilization for each account",
	Long: `Displays the rate-limit utilization data collected by the controller's
ratelimit probe. Each account's 5-hour and 7-day utilization windows are
shown along with throttle status and reset times.

This data comes from Anthropic's anthropic-ratelimit-unified-* response
headers, probed periodically by the controller. The "remaining" percentage
is calculated as (1 - utilization) * 100.`,
	RunE: runAccountLimits,
}

type accountLimits struct {
	Name                   string  `json:"name"`
	Provider               string  `json:"provider"`
	Status                 string  `json:"status"`
	ThrottleStatus         string  `json:"throttle_status"`
	Utilization5h          float64 `json:"utilization_5h"`
	Utilization7d          float64 `json:"utilization_7d"`
	Remaining5hPct         float64 `json:"remaining_5h_pct"`
	Remaining7dPct         float64 `json:"remaining_7d_pct"`
	BindingWindow          string  `json:"binding_window"`
	OverageStatus          string  `json:"overage_status,omitempty"`
	OverageReason          string  `json:"overage_reason,omitempty"`
	ThrottleResetAt        string  `json:"throttle_reset_at,omitempty"`
	ResetIn                string  `json:"reset_in,omitempty"`
	ProbeCheckedAt         string  `json:"probe_checked_at,omitempty"`
	ProbeAge               string  `json:"probe_age,omitempty"`
	// ProbeAuthStatus mirrors the account bead's probe_auth_status, set to
	// "rejected" by the controller probe on a hard 401/403 (ratelimit_probe.go).
	// A hard auth rejection makes the probe early-return WITHOUT refreshing
	// throttle_status/utilization, so those fields freeze at their last-good
	// values and keep rendering "allowed" with plenty of headroom forever. That
	// made an auth-dead account read as the fleet's healthiest reserve
	// (matthewbaker1110: allowed / 73% 5h free / reset "past" / probe 7d stale).
	ProbeAuthStatus string `json:"probe_auth_status,omitempty"`
	// ProbeStale is true when the probe data is too old to act on. Rendered as a
	// "!" suffix on PROBE AGE so a frozen "allowed" is never read as current.
	ProbeStale bool `json:"probe_stale,omitempty"`
	SoftFailoverState      string  `json:"soft_failover_state,omitempty"`
	SoftFailoverThreshold  float64 `json:"soft_failover_threshold,omitempty"`
	// ModelGroups is the per-model-group rate-limit split the probe now records
	// (ka-0000003806): Fable capacity tracked separately from Opus/Sonnet. Decoded
	// from the account bead's model_group_limits JSON field. Empty for accounts
	// whose probe response carried no group-suffixed headers (older accounts, or a
	// group Anthropic has not split). Sorted by key for stable output.
	ModelGroups []modelGroupLimitView `json:"model_groups,omitempty"`
}

// modelGroupLimitView is the CLI-facing shape of one per-model-group slice, decoded
// from the controller-written model_group_limits field. Mirrors the controller's
// modelGroupLimit (ratelimit_probe.go) — kept as a separate type so gb has no
// dependency on the controller package. RemainingPct is derived for display.
type modelGroupLimitView struct {
	Key          string  `json:"key"` // "<window>_<group>", e.g. "7d_fable"
	Window       string  `json:"window"`
	Group        string  `json:"group"`
	Utilization  float64 `json:"utilization"`
	RemainingPct float64 `json:"remaining_pct"`
	Reset        int64   `json:"reset,omitempty"`
	ResetIn      string  `json:"reset_in,omitempty"`
	Status       string  `json:"status,omitempty"`
}

func runAccountLimits(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()

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

	beads := result.Beads
	sort.Slice(beads, func(i, j int) bool { return beads[i].Title < beads[j].Title })

	now := time.Now().UTC()
	var results []accountLimits

	for _, b := range beads {
		// Only show accounts that have probe data (utilization fields set by the ratelimit probe).
		util5h := b.Fields["utilization_5h"]
		util7d := b.Fields["utilization_7d"]
		if util5h == "" && util7d == "" {
			continue
		}

		acctStatus := b.Fields["status"]
		if acctStatus == "" {
			acctStatus = "active"
		}

		u5h := parseFieldFloat(util5h)
		u7d := parseFieldFloat(util7d)

		al := accountLimits{
			Name:                  b.Title,
			Provider:              b.Fields["provider"],
			Status:                acctStatus,
			ThrottleStatus:        b.Fields["throttle_status"],
			Utilization5h:         u5h,
			Utilization7d:         u7d,
			Remaining5hPct:        math.Max(0, (1-u5h)*100),
			Remaining7dPct:        math.Max(0, (1-u7d)*100),
			BindingWindow:         b.Fields["representative_claim"],
			OverageStatus:         b.Fields["overage_status"],
			OverageReason:         b.Fields["overage_disabled_reason"],
			ThrottleResetAt:       b.Fields["throttle_reset_at"],
			ProbeCheckedAt:        b.Fields["probe_checked_at"],
			ProbeAuthStatus:       b.Fields["probe_auth_status"],
			SoftFailoverState:     b.Fields["soft_failover_state"],
			SoftFailoverThreshold: parseFieldFloat(b.Fields["soft_failover_threshold"]),
		}

		// Compute human-friendly reset countdown.
		if al.ThrottleResetAt != "" {
			if resetTime, err := time.Parse(time.RFC3339, al.ThrottleResetAt); err == nil {
				if resetTime.After(now) {
					al.ResetIn = fmtDuration(resetTime.Sub(now))
				} else {
					al.ResetIn = "past"
				}
			}
		}

		// Compute probe age, and flag data too old to act on. Mirrors the
		// controller's probeStaleThreshold (auth_failover.go) — beyond it the
		// switchback path already refuses to trust throttle_status, so the
		// operator view must not present it as current either.
		if al.ProbeCheckedAt != "" {
			if checkedAt, err := time.Parse(time.RFC3339, al.ProbeCheckedAt); err == nil {
				age := now.Sub(checkedAt)
				al.ProbeAge = fmtDuration(age)
				al.ProbeStale = age > probeStaleThreshold
			}
		}

		// Decode the per-model-group split (ka-0000003806) so Fable capacity is
		// visible separately from Opus/Sonnet. Absent/malformed field → no groups
		// (older accounts render exactly as before).
		al.ModelGroups = decodeModelGroups(b.Fields["model_group_limits"], now)

		results = append(results, al)
	}

	if len(results) == 0 {
		fmt.Fprintln(os.Stdout, "No accounts with rate-limit probe data found.")
		return nil
	}

	if jsonOutput {
		printJSON(results)
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ACCOUNT\tSTATUS\t5H USED\t5H LEFT\t7D USED\t7D LEFT\tTHROTTLE\tFAILOVER\tRESET IN\tPROBE AGE")
	for _, r := range results {
		// A hard auth rejection freezes throttle_status/utilization at their
		// last-good values, so rendering the raw "allowed" advertises an
		// auth-dead account as usable capacity. Show the auth verdict instead —
		// it is the operative one for "can this account serve traffic?".
		throttle := r.ThrottleStatus
		if r.ProbeAuthStatus == "rejected" {
			throttle = "auth-dead"
		} else if throttle == "" {
			throttle = "-"
		}
		resetIn := r.ResetIn
		if resetIn == "" {
			resetIn = "-"
		}
		probeAge := r.ProbeAge
		if probeAge == "" {
			probeAge = "-"
		} else if r.ProbeStale {
			probeAge += "!" // data older than probeStaleThreshold — do not trust
		}
		failover := fmtFailoverState(r.SoftFailoverState, r.SoftFailoverThreshold)

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			r.Name,
			r.Status,
			fmtPct(r.Utilization5h),
			fmtPct(r.Remaining5hPct/100),
			fmtPct(r.Utilization7d),
			fmtPct(r.Remaining7dPct/100),
			throttle,
			failover,
			resetIn,
			probeAge,
		)
	}
	w.Flush()

	// Per-model-group breakdown (ka-0000003806). Anthropic splits subscription
	// limits per model family — a Fable-only window separate from Opus/Sonnet — so
	// an account can be Fable-exhausted while its aggregate 5h/7d looks healthy.
	// Print a compact per-group section for every account that carries the split,
	// so an operator can see WHICH model group is tapped. Accounts without the
	// split (older probe data) contribute nothing here.
	printModelGroupBreakdown(results)

	// Print binding window and overage info if any account is throttled.
	// An auth-dead account gets its own line: it is never throttle_status
	// "rejected" (the probe early-returns before refreshing that field), so
	// without this branch the one account an operator most needs explained is
	// the only one with no explanation at all.
	for _, r := range results {
		switch {
		case r.ProbeAuthStatus == "rejected":
			fmt.Fprintf(os.Stderr, "\n%s: AUTH-DEAD — probe got a hard 401/403; credential is invalid/revoked.", r.Name)
			fmt.Fprintf(os.Stderr, " Its %s/%s columns are FROZEN at the last good probe (%s ago) and do not reflect real capacity.",
				"5H", "7D", r.ProbeAge)
			if r.OverageReason != "" {
				fmt.Fprintf(os.Stderr, " Last known overage: %s.", r.OverageReason)
			}
			fmt.Fprintln(os.Stderr, " Rotation already excludes it; re-mint the credential to return it to the pool.")
		case r.ThrottleStatus == "rejected":
			fmt.Fprintf(os.Stderr, "\n%s: throttled (binding window: %s", r.Name, r.BindingWindow)
			if r.OverageReason != "" {
				fmt.Fprintf(os.Stderr, ", overage: %s", r.OverageReason)
			}
			fmt.Fprintln(os.Stderr, ")")
		}
	}

	return nil
}

// decodeModelGroups decodes the account bead's model_group_limits JSON field
// (written by the controller probe, ka-0000003806) into a display-sorted slice of
// per-model-group views. The field encodes a map keyed "<window>_<group>"; this
// flattens it, derives remaining-% and a human reset countdown, and sorts by key
// for stable output. An empty or malformed field yields nil so accounts without
// the split render unchanged — the CLI must never error on an account that simply
// predates the per-group probe.
func decodeModelGroups(raw string, now time.Time) []modelGroupLimitView {
	if raw == "" {
		return nil
	}
	var m map[string]struct {
		Window      string  `json:"window"`
		Group       string  `json:"group"`
		Utilization float64 `json:"utilization"`
		Reset       int64   `json:"reset"`
		Status      string  `json:"status"`
	}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil
	}
	views := make([]modelGroupLimitView, 0, len(m))
	for key, g := range m {
		v := modelGroupLimitView{
			Key:          key,
			Window:       g.Window,
			Group:        g.Group,
			Utilization:  g.Utilization,
			RemainingPct: math.Max(0, (1-g.Utilization)*100),
			Reset:        g.Reset,
			Status:       g.Status,
		}
		if g.Reset > 0 {
			resetTime := time.Unix(g.Reset, 0).UTC()
			if resetTime.After(now) {
				v.ResetIn = fmtDuration(resetTime.Sub(now))
			} else {
				v.ResetIn = "past"
			}
		}
		views = append(views, v)
	}
	sort.Slice(views, func(i, j int) bool { return views[i].Key < views[j].Key })
	return views
}

// printModelGroupBreakdown renders the per-model-group section beneath the main
// limits table for every account carrying the split. It writes to stdout via a
// fresh tabwriter so its columns align independently of the main table. A no-op
// when no account has group data (older-probe fleets see zero change).
func printModelGroupBreakdown(results []accountLimits) {
	any := false
	for _, r := range results {
		if len(r.ModelGroups) > 0 {
			any = true
			break
		}
	}
	if !any {
		return
	}
	fmt.Fprintln(os.Stdout, "\nPer-model-group windows (Fable/Opus/Sonnet split):")
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ACCOUNT\tWINDOW\tGROUP\tUSED\tLEFT\tSTATUS\tRESET IN")
	for _, r := range results {
		for _, g := range r.ModelGroups {
			status := g.Status
			if status == "" {
				status = "-"
			}
			resetIn := g.ResetIn
			if resetIn == "" {
				resetIn = "-"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				r.Name, g.Window, g.Group,
				fmtPct(g.Utilization), fmtPct(g.RemainingPct/100),
				status, resetIn)
		}
	}
	w.Flush()
}

// parseFieldFloat parses a bead field string as float64; returns 0 on empty or error.
func parseFieldFloat(s string) float64 {
	if s == "" {
		return 0
	}
	f, _ := strconv.ParseFloat(s, 64)
	return f
}

// fmtPct formats a 0.0–1.0 fraction as a percentage string like "42.3%".
func fmtPct(v float64) string {
	pct := v * 100
	if pct == 0 {
		return "0%"
	}
	if pct >= 99.95 {
		return "100%"
	}
	return fmt.Sprintf("%.1f%%", pct)
}

// fmtFailoverState formats the soft failover column for the limits table.
// Returns "WARMING @80%" when warming, or the threshold if configured, or "-".
func fmtFailoverState(state string, threshold float64) string {
	if state == "warming" {
		if threshold > 0 {
			return fmt.Sprintf("WARMING @%s", fmtPct(threshold))
		}
		return "WARMING"
	}
	if threshold > 0 {
		return fmt.Sprintf("@%s", fmtPct(threshold))
	}
	return "-"
}

// fmtDuration formats a duration in a concise human-readable form.
func fmtDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	hours := int(d.Hours())
	mins := int(d.Minutes()) % 60
	if hours < 24 {
		if mins > 0 {
			return fmt.Sprintf("%dh%dm", hours, mins)
		}
		return fmt.Sprintf("%dh", hours)
	}
	days := hours / 24
	remainHours := hours % 24
	if remainHours > 0 {
		return fmt.Sprintf("%dd%dh", days, remainHours)
	}
	return fmt.Sprintf("%dd", days)
}

func init() {
	accountCmd.AddCommand(accountLimitsCmd)
}
