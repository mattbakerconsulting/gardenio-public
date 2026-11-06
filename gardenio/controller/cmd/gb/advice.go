package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"gardenio/controller/internal/advice"
	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

var adviceCmd = &cobra.Command{
	Use:   "advice",
	Short: "Manage advice beads",
	Long: `Advice beads are persistent guidance delivered to agents based on label matching.

LABEL MATCHING:

  Labels control which agents receive advice. Targeting labels are:
  global, project:<name>, role:<name>, agent:<id>

  Ungrouped labels (no prefix) use simple rules:
    - project: and agent: labels are hard requirements (all must match)
    - role: labels participate in OR matching
    - global matches every agent

  When combining --project and --role, they are AND-grouped by default.
  This is correct for project-specific roles (e.g., mr-tester in monorepo).
  For global roles (e.g., thread agents across all projects), use the role
  label alone — do NOT add a project label, or the advice won't reach
  agents in other projects.

LABELING GUIDE:

  Global role (applies to all projects):
    gb advice add "tip" --role thread            → role:thread

  Project-specific role:
    gb advice add "tip" --project monorepo --role mr-tester
                                                 → g0:project:monorepo, g0:role:mr-tester

  Multiple projects (OR):
    gb advice add "tip" -l project:gardenio -l project:monorepo
    gb advice add "tip" --project gardenio --role thread --or-group

GROUP MATCHING (gN: prefix):

  For compound targeting, use the gN: prefix to create label groups:
    - Labels in the SAME group (same N) are AND'd — all must match
    - DIFFERENT groups are OR'd — any group matching is sufficient

  Examples:
    g0:project:gardenio, g0:role:crew      →  (project:gardenio AND role:crew)
    g0:project:gardenio, g1:role:thread    →  (project:gardenio) OR (role:thread)
    g0:project:A, g0:role:X, g1:role:Y   →  (project:A AND role:X) OR (role:Y)

  The --project/--role flags AND-group their labels by default (g0:).
  Use --or-group to create separate OR groups instead.

RE-TARGETING EXISTING ADVICE:

  Use "gb advice relabel" — it adds and removes labels in ONE call, so a
  scope/role move can never leave the bead carrying both the old and the new
  label. Prefer it over a "kd label add" + "kd label remove" pair.

    gb advice relabel <id> --scope on-demand`,
	GroupID: "orchestration",
	// adviceCmd is a parent group, but it MUST be runnable (have a RunE) for
	// cobra to reach the Args validator: a non-runnable parent short-circuits to
	// flag.ErrHelp BEFORE ValidateArgs runs (cobra command.go: `if
	// !c.Runnable() { return flag.ErrHelp }`), printing help to STDOUT and
	// exiting 0. That is the second half of ka-0000003820 — `gb advice relabel`
	// did not exist AND its absence was silent, so a caller scripting
	// `gb advice relabel ... || <fallback>` never saw the fallback fire. With a
	// RunE present, cobra.NoArgs rejects any UNKNOWN subcommand with a non-zero
	// exit + stderr message, matching how the ROOT gb command already rejects
	// unknown commands. KNOWN subcommands match first in cobra's Find and are
	// validated by their own Args, so they never reach this NoArgs. A bare
	// `gb advice` passes NoArgs and falls through to RunE → help, exit 0.
	// Same fix shape as depCmd in kbeads/cmd/kd/dep.go (ka-0000005587).
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return cmd.Help()
	},
}

// ── advice add ────────────────────────────────────────────────────────

var adviceAddCmd = &cobra.Command{
	Use:   "add <text>",
	Short: "Create an advice bead",
	Long: `Create an advice bead with targeting labels.

By default, --project and --role are AND-grouped (g0:), meaning agents must
match ALL of them. Use --or-group to create separate OR groups instead.

Examples:
  # Match crew agents in gardenio (AND)
  gb advice add "tip" --project gardenio --role crew

  # Match gardenio agents OR thread agents (OR)
  gb advice add "tip" --project gardenio --role thread --or-group

  # Custom group labels for complex targeting
  gb advice add "tip" -l g0:project:gardenio,g0:role:crew -l g1:role:thread

  # Time-bounded advice: expire after 7 days (cycle summaries, temporary workarounds)
  gb advice add "freeze non-critical merges this week" --project gardenio --ttl 7d

  # Expire at a specific absolute time (e.g. until a known release lands)
  gb advice add "use --flag X until MR 123 lands" --project gardenio \
      --expires-at 2026-05-01T00:00:00Z`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		text := args[0]

		title, _ := cmd.Flags().GetString("title")
		description, _ := cmd.Flags().GetString("description")
		priority, _ := cmd.Flags().GetInt("priority")

		// Expand curl-style @file / @- / @@ on --description (ka-0000003818).
		// This is the direct analog of `kd`'s documented `-d/--description
		// (@file reads the value from a file …)` — advice bodies are exactly the
		// long-form prose agents draft in a file, so parity here is the strongest
		// muscle-memory case in the audit. Without it a bare "@notes.md" was
		// stored as the literal pointer, publishing advice whose body is a
		// dangling path. A missing/typo'd path fails LOUD. Done before the
		// `description == "" → text` fallback so an unset flag still mirrors the
		// positional advice text. The one-line positional <text> title is left
		// literal (a leading '@' in a title is legitimate).
		if description != "" {
			var err error
			description, err = resolveTextValue(description)
			if err != nil {
				return fmt.Errorf("--description: %w", err)
			}
		}
		labels, _ := cmd.Flags().GetStringSlice("label")
		force, _ := cmd.Flags().GetBool("force")

		project, _ := cmd.Flags().GetString("project")
		rig, _ := cmd.Flags().GetString("rig")
		role, _ := cmd.Flags().GetString("role")
		agent, _ := cmd.Flags().GetString("agent")
		orGroup, _ := cmd.Flags().GetBool("or-group")

		if rig != "" {
			return fmt.Errorf("--rig has been removed; use --project=%s instead", rig)
		}

		// Collect shorthand targeting labels.
		var targeting []string
		if project != "" {
			targeting = append(targeting, "project:"+project)
		}
		if role != "" {
			targeting = append(targeting, "role:"+role)
		}
		if agent != "" {
			targeting = append(targeting, "agent:"+agent)
		}
		if len(targeting) > 1 {
			if orGroup {
				// OR mode: each targeting label gets its own group number.
				for i, l := range targeting {
					labels = append(labels, fmt.Sprintf("g%d:%s", i, l))
				}
			} else {
				// AND mode (default): all targeting labels share g0.
				for _, l := range targeting {
					labels = append(labels, "g0:"+l)
				}
			}
		} else {
			labels = append(labels, targeting...)
		}

		if !advice.HasTargetingLabel(labels) {
			labels = append(labels, "global")
		}

		// Bare-project broadcast guard (ka-0000003733 Phase 3): a project:
		// label with no role:/agent: narrowing reaches every agent in the
		// project, and the delivery path drops such beads unless they carry
		// the explicit opt-in — so creating one without it would produce
		// advice nobody ever receives.
		if advice.IsDeniedBroadcastLabels(labels) {
			return fmt.Errorf("bare-project advice broadcasts to every agent in the project and will NOT be delivered.\n"+
				"Either narrow the target (add --role <role> or --agent <id>), or opt in to intentional\n"+
				"project-wide broadcast with: -l %s", advice.BroadcastOptInLabel)
		}

		if title == "" {
			title = text
		}
		if description == "" {
			description = text
		}

		// Check for similar existing advice unless --force is set.
		if !force {
			similar, err := advice.FindSimilar(cmd.Context(), daemon, title)
			if err == nil && len(similar) > 0 {
				fmt.Fprintf(os.Stderr, "Warning: found %d similar existing advice bead(s):\n", len(similar))
				for _, s := range similar {
					fmt.Fprintf(os.Stderr, "  %s (%.0f%% similar): %s\n", s.Bead.ID, s.Score*100, s.Bead.Title)
				}
				fmt.Fprintln(os.Stderr, "Use --force to create anyway.")
				return fmt.Errorf("potential duplicate advice found (use --force to override)")
			}
		}

		// Only send fields that were explicitly provided.
		fields := map[string]any{}
		hookCmd, _ := cmd.Flags().GetString("hook-command")
		hookTrigger, _ := cmd.Flags().GetString("hook-trigger")
		if hookCmd != "" {
			fields["hook_command"] = hookCmd
		}
		if hookTrigger != "" {
			fields["hook_trigger"] = hookTrigger
		}

		ttlStr, _ := cmd.Flags().GetString("ttl")
		expiresAtStr, _ := cmd.Flags().GetString("expires-at")
		if ttlStr != "" && expiresAtStr != "" {
			return fmt.Errorf("--ttl and --expires-at are mutually exclusive")
		}
		if ttlStr != "" {
			d, err := advice.ParseDuration(ttlStr)
			if err != nil {
				return fmt.Errorf("--ttl: %w", err)
			}
			if d <= 0 {
				return fmt.Errorf("--ttl: ttl must be > 0")
			}
			fields["expires_at"] = time.Now().UTC().Add(d).Format(time.RFC3339)
		}
		if expiresAtStr != "" {
			t, err := time.Parse(time.RFC3339, expiresAtStr)
			if err != nil {
				return fmt.Errorf("--expires-at: must be RFC3339 (e.g. 2026-05-01T00:00:00Z): %w", err)
			}
			if !t.After(time.Now()) {
				return fmt.Errorf("--expires-at: expiration already in the past")
			}
			fields["expires_at"] = t.UTC().Format(time.RFC3339)
		}

		var fieldsJSON json.RawMessage
		if len(fields) > 0 {
			var err error
			fieldsJSON, err = json.Marshal(fields)
			if err != nil {
				return fmt.Errorf("encoding fields: %w", err)
			}
		}

		id, err := daemon.CreateBead(cmd.Context(), beadsapi.CreateBeadRequest{
			Title:       title,
			Description: description,
			Type:        "advice",
			Priority:    priority,
			Labels:      labels,
			CreatedBy:   actor,
			Fields:      fieldsJSON,
		})
		if err != nil {
			return fmt.Errorf("creating advice: %w", err)
		}

		if jsonOutput {
			printJSON(map[string]string{"id": id})
		} else {
			fmt.Printf("Created advice %s: %s\n", id, title)
		}
		return nil
	},
}

// ── advice list ────────────────────────────────────────────────────────

var adviceListCmd = &cobra.Command{
	Use:   "list",
	Short: "List advice beads",
	Long: `List advice beads.

By default this lists only beads that actually reach an agent's session via
gb prime — i.e. it applies the same legacy hippo→advice mirror filter
(isHippoMirror / filterOutHippoMirrors in prime_advice.go) that gb prime
applies on the delivery path. This keeps "what gb advice list shows" in sync
with "what gb prime delivers"; before this, list surfaced
source:hippo-memory / source:hippo-global mirror beads that gb prime silently
drops, misleading anyone reasoning about which advice is live.

Use --show-undelivered to also list the filtered-out (undelivered) mirror
beads, each annotated NOT-DELIVERED. That's the path to enumerate stale hippo
mirrors for cleanup without being misled into thinking they're live.

--for <agent-context> simulates delivery for an agent context and lists the
advice that context actually receives, through the SAME matcher gb prime uses
(label group AND/OR semantics, role-required gate, role-family anchors). The
argument is an agent context, NOT a raw label:

  gb advice list --for gardenio/crews/foo      # a full agent ID (the form the
                                              #   prime truncation footer emits)
  gb advice list --for monorepo/mbaker-assistants  # project/role → sees
                                              #   project-AND-role-scoped advice
  gb advice list --for crew                   # a bare role → global + role advice

A bare role has no project, so it will not match advice scoped to a specific
project (that advice's project: label has no match). Give the project/role form
to include project-scoped advice. --for and --label are mutually exclusive.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		labels, _ := cmd.Flags().GetStringSlice("label")
		forAgent, _ := cmd.Flags().GetString("for")
		showUndelivered, _ := cmd.Flags().GetBool("show-undelivered")

		var beads []*beadsapi.BeadDetail
		var total int
		if forAgent != "" {
			// --for is delivery SIMULATION for an agent context, run through the
			// real matcher — NOT a raw label filter. Previously it appended the
			// argument verbatim as a label; an agent ID ("gardenio/crews/foo") or
			// a role ("crew") is never a literal advice label, so it silently
			// matched nothing and always printed "No advice beads found" — the
			// exact defect in ka-0000002346, and the reason the prime truncation
			// footer's advertised `gb advice list --for <agentID>` was dead.
			subs := adviceContextSubscriptions(cmd.Context(), forAgent)
			all, err := advice.ListOpenAdvice(cmd.Context(), daemon)
			if err != nil {
				return fmt.Errorf("listing advice: %w", err)
			}
			for _, b := range all {
				if advice.IsFixture(b) || advice.IsDeniedBroadcast(b) {
					continue
				}
				if advice.MatchesSubscriptions(b.Labels, subs) {
					beads = append(beads, b)
				}
			}
			total = len(beads)
		} else {
			result, err := daemon.ListBeadsFiltered(cmd.Context(), beadsapi.ListBeadsQuery{
				Types:    []string{"advice"},
				Labels:   labels,
				Statuses: []string{"open", "in_progress"},
				Limit:    100,
			})
			if err != nil {
				return fmt.Errorf("listing advice: %w", err)
			}
			beads = result.Beads
			total = result.Total
		}

		// Partition by delivery truth using the SAME predicate gb prime uses
		// (isHippoMirror). delivered == what an agent's prime would receive.
		delivered, undelivered := partitionDeliverableAdvice(beads)

		if jsonOutput {
			if showUndelivered {
				printJSON(annotateAdviceDelivery(beads))
			} else {
				printJSON(delivered)
			}
			return nil
		}

		if showUndelivered {
			printAdviceListAnnotated(beads, total)
		} else {
			printAdviceList(delivered, total)
			if n := len(undelivered); n > 0 {
				fmt.Printf("(%d undelivered hippo-mirror advice bead(s) hidden — not reachable via gb prime; use --show-undelivered to view)\n", n)
			}
		}
		return nil
	},
}

// adviceContextSubscriptions turns the --for argument into the subscription set
// the real delivery matcher (advice.MatchesSubscriptions) evaluates against, so
// `gb advice list --for X` shows exactly what gb prime would deliver to X.
//
// The argument is an agent CONTEXT, matched to how it is spelled:
//   - a full agent ID ("project/roles/name", the form the prime truncation
//     footer emits) → BuildAgentSubscriptions + EnrichAgentSubscriptions, i.e.
//     the identical subscription path gb prime runs for a live agent.
//   - a bare role token ("crew", "mbaker-assistant") → global + role subs
//     (+ role-family anchors) via ArchetypeSubscriptions with no project. This
//     matches global and role-scoped advice but NOT project-scoped advice; use
//     the project/role form to include the latter.
func adviceContextSubscriptions(ctx context.Context, forArg string) []string {
	if strings.Contains(forArg, "/") {
		subs := advice.BuildAgentSubscriptions(forArg, nil)
		return advice.EnrichAgentSubscriptions(ctx, daemon, forArg, subs)
	}
	return advice.ArchetypeSubscriptions("", forArg)
}

// ── advice show ────────────────────────────────────────────────────────

var adviceShowCmd = &cobra.Command{
	Use:   "show <id>",
	Short: "Show details of an advice bead",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		bead, err := daemon.GetBead(cmd.Context(), args[0])
		if err != nil {
			return fmt.Errorf("getting advice %s: %w", args[0], err)
		}

		if jsonOutput {
			printJSON(bead)
			return nil
		}

		fmt.Printf("ID:          %s\n", bead.ID)
		fmt.Printf("Title:       %s\n", bead.Title)
		fmt.Printf("Status:      %s\n", bead.Status)
		// Provenance — who wrote this practice and when (task ka-0000007527).
		// The catalog is overwhelmingly agent-authored: 200 of 253 open advice
		// beads, from 172 distinct agents. Until now `gb advice show` printed
		// none of that, so every item read as house doctrine that had always
		// been there — a practice one agent inferred once and a rule the fleet
		// converged on were typographically identical.
		//
		// This is NOT a credit line, and deliberately not a ranking. An earlier
		// draft of this change added a per-author reach leaderboard; it was cut
		// because ranking agents by how widely their advice is delivered
		// manufactures exactly the prestige gradient that turns a fleet's
		// self-reports into load-bearing premises (operator direction,
		// 2026-08-16). Origin and age are facts the reader can act on. Standing
		// is not.
		if v := strings.TrimSpace(bead.CreatedBy); v != "" {
			fmt.Printf("Authored By: %s\n", v)
		}
		if !bead.CreatedAt.IsZero() {
			fmt.Printf("Written:     %s  (%s)\n",
				bead.CreatedAt.UTC().Format(time.RFC3339), relativeTime(bead.CreatedAt, time.Now()))
		}
		if len(bead.Labels) > 0 {
			fmt.Printf("Labels:      %s\n", strings.Join(bead.Labels, ", "))
		}
		if v := bead.Fields["hook_command"]; v != "" {
			fmt.Printf("Hook Cmd:    %s\n", v)
		}
		if v := bead.Fields["hook_trigger"]; v != "" {
			fmt.Printf("Hook When:   %s\n", v)
		}
		if v := bead.Fields["expires_at"]; v != "" {
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				now := time.Now()
				expired := !t.After(now)
				rel := relativeTime(t, now)
				fmt.Printf("Expires:     %s  (%s)\n", t.UTC().Format(time.RFC3339), rel)
				fmt.Printf("Expired:     %t\n", expired)
			} else {
				fmt.Printf("Expires:     %s\n", v)
			}
		}
		if body := strings.TrimSpace(bead.Description); body != "" {
			fmt.Printf("\n%s\n", body)
		}
		return nil
	},
}

// relativeTime renders the gap between t and now as a human-readable string
// like "in 4 days" or "3 hours ago".
func relativeTime(t, now time.Time) string {
	d := t.Sub(now)
	suffix := ""
	if d < 0 {
		d = -d
		suffix = " ago"
	} else {
		return "in " + humanDuration(d)
	}
	return humanDuration(d) + suffix
}

func humanDuration(d time.Duration) string {
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	case d >= time.Hour:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	case d >= time.Minute:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	default:
		return fmt.Sprintf("%d seconds", int(d.Seconds()))
	}
}

// ── advice remove ────────────────────────────────────────────────────────

var adviceRemoveCmd = &cobra.Command{
	Use:   "remove <id>",
	Short: "Remove an advice bead",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id := args[0]

		hard, _ := cmd.Flags().GetBool("hard")
		if hard {
			if err := daemon.DeleteBead(cmd.Context(), id); err != nil {
				return fmt.Errorf("deleting advice %s: %w", id, err)
			}
			fmt.Printf("Deleted advice %s\n", id)
		} else {
			if err := daemon.CloseBead(cmd.Context(), id, nil); err != nil {
				return fmt.Errorf("closing advice %s: %w", id, err)
			}
			fmt.Printf("Closed advice %s\n", id)
		}
		return nil
	},
}

// ── helpers ────────────────────────────────────────────────────────────

func printAdviceList(beads []*beadsapi.BeadDetail, total int) {
	if len(beads) == 0 {
		fmt.Println("No advice beads found.")
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tTITLE\tLABELS")
	for _, b := range beads {
		title := b.Title
		if len(title) > 50 {
			title = title[:47] + "..."
		}
		lbls := strings.Join(b.Labels, ", ")
		fmt.Fprintf(w, "%s\t%s\t%s\n", b.ID, title, lbls)
	}
	w.Flush()
	fmt.Printf("\n%d advice beads (%d total)\n", len(beads), total)
}

// partitionDeliverableAdvice splits advice beads into those gb prime would
// actually deliver to an agent (delivered) and those it silently drops on the
// delivery path (undelivered). It uses the SAME predicates as gb prime —
// isHippoMirror (filterOutHippoMirrors, prime_advice.go) and the bare-project
// broadcast deny (advice.IsDeniedBroadcast) — so that `gb advice list`
// delivery-truth matches `gb prime` — the divergence fixed in ka-0000002735.
// Input order is preserved within each bucket. Nil-safe.
func partitionDeliverableAdvice(beads []*beadsapi.BeadDetail) (delivered, undelivered []*beadsapi.BeadDetail) {
	for _, b := range beads {
		if isUndeliveredAdvice(b) {
			undelivered = append(undelivered, b)
		} else {
			delivered = append(delivered, b)
		}
	}
	return delivered, undelivered
}

// isUndeliveredAdvice is the single list-side delivery-truth predicate: true
// when gb prime would silently drop the bead (legacy hippo mirror, or a
// bare-project broadcast without the scope:project-broadcast opt-in). All
// gb advice list render paths must use this so they cannot drift from each
// other or from the delivery path.
func isUndeliveredAdvice(b *beadsapi.BeadDetail) bool {
	return isHippoMirror(b) || advice.IsDeniedBroadcast(b)
}

// adviceDeliveryItem wraps an advice bead with a delivered flag for the
// --show-undelivered JSON output. The embedded *BeadDetail flattens all its
// JSON fields, with an added "delivered" boolean mirroring gb prime delivery
// truth (false == filtered out as a legacy hippo mirror).
type adviceDeliveryItem struct {
	*beadsapi.BeadDetail
	Delivered bool `json:"delivered"`
}

// annotateAdviceDelivery tags each bead with whether gb prime would deliver it,
// preserving input order. Used by `gb advice list --show-undelivered --json`.
func annotateAdviceDelivery(beads []*beadsapi.BeadDetail) []adviceDeliveryItem {
	items := make([]adviceDeliveryItem, len(beads))
	for i, b := range beads {
		items[i] = adviceDeliveryItem{BeadDetail: b, Delivered: !isUndeliveredAdvice(b)}
	}
	return items
}

// printAdviceListAnnotated renders every advice bead with a DELIVERED column,
// marking legacy hippo mirrors NOT-DELIVERED. Used by --show-undelivered so an
// auditor can enumerate the undelivered mirrors without being misled into
// thinking they're live (the failure mode behind ka-0000002735).
func printAdviceListAnnotated(beads []*beadsapi.BeadDetail, total int) {
	if len(beads) == 0 {
		fmt.Println("No advice beads found.")
		return
	}

	undelivered := 0
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tDELIVERED\tTITLE\tLABELS")
	for _, b := range beads {
		title := b.Title
		if len(title) > 50 {
			title = title[:47] + "..."
		}
		lbls := strings.Join(b.Labels, ", ")
		status := "yes"
		if isUndeliveredAdvice(b) {
			status = "NOT-DELIVERED"
			undelivered++
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", b.ID, status, title, lbls)
	}
	w.Flush()
	fmt.Printf("\n%d advice beads (%d total); %d delivered, %d NOT-DELIVERED (hippo mirrors + denied bare-project broadcasts gb prime drops)\n",
		len(beads), total, len(beads)-undelivered, undelivered)
}

func init() {
	adviceCmd.AddCommand(adviceAddCmd)
	adviceCmd.AddCommand(adviceListCmd)
	adviceCmd.AddCommand(adviceShowCmd)
	adviceCmd.AddCommand(adviceRelabelCmd)
	adviceCmd.AddCommand(adviceRemoveCmd)

	adviceAddCmd.Flags().StringP("title", "t", "", "override title")
	adviceAddCmd.Flags().StringP("description", "d", "", "detailed description (@file reads the value from a file, @- reads stdin; @@ escapes a literal leading '@')")
	adviceAddCmd.Flags().IntP("priority", "p", 2, "priority (0-4)")
	adviceAddCmd.Flags().StringSliceP("label", "l", nil, "labels (repeatable)")
	adviceAddCmd.Flags().String("project", "", "shorthand for --label project:<value>")
	adviceAddCmd.Flags().String("rig", "", "deprecated: use --project instead")
	adviceAddCmd.Flags().String("role", "", "shorthand for --label role:<value>")
	adviceAddCmd.Flags().String("agent", "", "shorthand for --label agent:<value>")
	adviceAddCmd.Flags().Bool("or-group", false, "OR-group --project/--role/--agent labels (default is AND)")
	adviceAddCmd.Flags().Bool("force", false, "skip duplicate check and create anyway")
	adviceAddCmd.Flags().Lookup("rig").Hidden = true
	adviceAddCmd.Flags().String("hook-command", "", "shell command to execute")
	adviceAddCmd.Flags().String("hook-trigger", "", "when to run")
	adviceAddCmd.Flags().String("ttl", "", "expire after duration (e.g. 4h, 7d, '4 hours', '1d12h')")
	adviceAddCmd.Flags().String("expires-at", "", "expire at absolute RFC3339 timestamp (e.g. 2026-05-01T00:00:00Z)")
	adviceAddCmd.MarkFlagsMutuallyExclusive("ttl", "expires-at")

	adviceListCmd.Flags().StringSliceP("label", "l", nil, "filter by label (raw label match; mutually exclusive with --for)")
	adviceListCmd.Flags().String("for", "", "simulate delivery for an agent context (agent ID 'proj/roles/name' or a bare role) and list what it receives")
	adviceListCmd.Flags().Bool("show-undelivered", false, "also show beads gb prime drops (legacy hippo mirrors), annotated NOT-DELIVERED")
	adviceListCmd.MarkFlagsMutuallyExclusive("label", "for")

	adviceRemoveCmd.Flags().Bool("hard", false, "permanently delete instead of closing")
}
