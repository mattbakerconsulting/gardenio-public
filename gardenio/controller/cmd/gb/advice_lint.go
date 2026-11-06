package main

// advice_lint.go implements `gb advice lint` — per-bead targeting-hygiene
// checks for the advice corpus (Phase 3 of the context-crisis plan,
// ka-0000003733 / ka-0000005093). Where `gb advice audit` totals matched
// cost per archetype, lint flags individual beads whose targeting broadcasts
// unexpectedly, matches nothing, or is otherwise suspect.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"gardenio/controller/internal/advice"
	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

var adviceLintCmd = &cobra.Command{
	Use:   "lint",
	Short: "Lint advice-bead targeting (broadcasts, dead targeting, junk labels)",
	Long: `Lint every open advice bead for targeting-hygiene problems:

  denied-broadcast    (error)  bare-project bead without ` + advice.BroadcastOptInLabel + ` —
                               dropped by the delivery path, reaches nobody
  no-targeting        (error)  no global/project:/role:/agent: label — matches no agent
  undeliverable-capability (error)  title asserts a capability / "no escalation
                               needed" yet the bead is undeliverable (on-demand or
                               denied broadcast) — the correction reaches nobody
  broken-glab-mr-flag (error)  description teaches ` + "`glab mr list --state=…`" + ` (or
                               bare ` + "`--opened`" + `) — glab has no --state flag; it errors
  opted-in-broadcast  (warn)   intentional project-wide broadcast — costs every agent
  useless-opt-in      (warn)   stray ` + advice.BroadcastOptInLabel + ` on a non-broadcast bead
  junk-project-label  (warn)   project:gd-… bead-ID-shaped target
  hippo-mirror        (warn)   legacy hippo mirror — never delivered, close it
  dead-role-target    (warn)   role: target references a retired/unknown role

Exits non-zero when any error-severity finding exists, so the check can gate
advice-hygiene workflows. Use --errors-only to hide warnings.

With --over-constraint, run the QUALITATIVE over-constraint checks instead —
rules-over-judgment framing, directive-density spikes, and system-prompt /
tool-description redundancy (the P4 context-audit guard, epic ka-0000006604).
Those findings are all advisory (WARN); add --strict to exit non-zero when any
is present.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		overConstraint, _ := cmd.Flags().GetBool("over-constraint")
		if overConstraint {
			strict, _ := cmd.Flags().GetBool("strict")
			return runAdviceLintOverConstraint(cmd, strict)
		}

		errorsOnly, _ := cmd.Flags().GetBool("errors-only")

		beads, err := advice.ListOpenAdvice(cmd.Context(), daemon)
		if err != nil {
			return fmt.Errorf("listing advice: %w", err)
		}

		findings := advice.LintAdvice(beads, liveRoleSet(cmd.Context()), prerunReplacedAdvice(cmd.Context()))
		if errorsOnly {
			kept := findings[:0]
			for _, f := range findings {
				if f.Severity == advice.LintError {
					kept = append(kept, f)
				}
			}
			findings = kept
		}

		errors := 0
		for _, f := range findings {
			if f.Severity == advice.LintError {
				errors++
			}
		}

		if jsonOutput {
			printJSON(findings)
		} else {
			printAdviceLint(findings, len(beads), errors)
		}

		if errors > 0 {
			// Findings are the diagnostic; the error just sets the exit code.
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			return fmt.Errorf("%d targeting error(s)", errors)
		}
		return nil
	},
}

// liveRoleSet builds the union-registry of roles that can currently spawn an
// agent, for the dead-role-target lint check: the builtin role set, every
// type:role bead name, the role field of every OPEN type=trigger bead
// (gardeners and other trigger beads that spawn a role without a config bead),
// and every role wired into a project bead's channel_roles map (a Slack
// mention in such a channel spawns that role — see the ka-0000002346 note on
// projectChannelRoles below).
// It is fail-open by design — mirroring roleWarning's degrade-to-no-warning:
// any fetch error returns nil, which LintAdvice treats as "registry unknown,
// skip the check" rather than "no roles are live" (which would flag every
// role: bead). Only when ALL lookups succeed do we return a populated set the
// check can trust.
func liveRoleSet(ctx context.Context) map[string]struct{} {
	// Fetch type:role beads via ListBeadsFiltered rather than ListRoleBeads so
	// we can see Total and apply the same truncation guard as the trigger fetch
	// below. ListRoleBeads discards Total, so a >1000-role catalog (the daemon's
	// default page cap) would silently drop beyond-cap roles from a NON-nil set
	// and manufacture spurious dead-role warnings for advice targeting them.
	// Limit mirrors the daemon default; statuses mirror ListRoleBeads' active
	// set. We only need the role NAMES (bead titles) for the union — duplicate
	// titles collapse into the same set key, so ListRoleBeads' duplicate-winner
	// resolution is irrelevant here.
	roleBeads, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"role"},
		Statuses: []string{"open", "in_progress", "blocked", "deferred"},
		Limit:    1000,
	})
	if err != nil {
		return nil // fail-open: unknown registry, skip the check
	}
	if roleBeads.Total > len(roleBeads.Beads) {
		return nil // fail-open: truncated role page, don't trust a partial set
	}
	trig, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"trigger"},
		Statuses: []string{"open"},
		Limit:    500,
	})
	if err != nil {
		return nil // fail-open: unknown registry, skip the check
	}
	// A truncated trigger page would drop roles that only a beyond-limit
	// trigger carries, manufacturing spurious dead-role warnings. Treat that
	// like a fetch error and fail open rather than trust a partial set.
	if trig.Total > len(trig.Beads) {
		return nil
	}
	chanRoles, ok := projectChannelRoles(ctx)
	if !ok {
		return nil // fail-open: unknown registry, skip the check
	}

	live := make(map[string]struct{}, len(builtinRoles)+len(roleBeads.Beads)+len(chanRoles))
	for name := range builtinRoles {
		live[name] = struct{}{}
	}
	for _, b := range roleBeads.Beads {
		if b == nil || b.Title == "" {
			continue
		}
		live[b.Title] = struct{}{}
	}
	for _, b := range trig.Beads {
		if b == nil {
			continue
		}
		if role := b.Fields["role"]; role != "" {
			live[role] = struct{}{}
		}
	}
	for role := range chanRoles {
		live[role] = struct{}{}
	}
	return live
}

// projectChannelRoles returns every role name wired into some project bead's
// channel_roles map — the fourth live spawn path, alongside builtins, type:role
// beads, and open triggers. A Slack mention in a wired channel spawns that role
// via roleFromChannel (bridge/bot_mentions.go), with NO type:role bead required.
//
// This path was missing from the union until ka-0000002346, and the omission was
// expensive: the per-user assistant recipe wired channel_roles but never created
// a type:role bead, so all 11 personal assistants (mbaker/pt/legend/joon/akim/
// ak/dc/dj/jp/hw/cg) kept spawning under the right role name while lint saw the
// role as retired. The 2026-07-25 hygiene sweep closed every one of their
// personality advice beads as dead-role-target, and because the channels still
// worked, nothing looked broken for ~3 weeks — the assistants had simply, and
// silently, lost their cross-session memory.
//
// Returns (set, true) on success and (nil, false) on any fetch failure or a
// truncated page, so the caller fails open rather than trusting a partial set
// and manufacturing the very warning this exists to prevent.
func projectChannelRoles(ctx context.Context) (map[string]struct{}, bool) {
	projects, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"project"},
		Statuses: []string{"open", "in_progress", "blocked", "deferred"},
		Limit:    500,
	})
	if err != nil {
		return nil, false
	}
	if projects.Total > len(projects.Beads) {
		return nil, false
	}

	roles := make(map[string]struct{})
	for _, b := range projects.Beads {
		if b == nil {
			continue
		}
		raw := b.Fields["channel_roles"]
		if raw == "" {
			continue
		}
		// Tolerate a malformed/type-flipped channel_roles blob on ONE project
		// rather than failing the whole check open: a bad field is a data bug
		// on that project, and skipping it still lets every other project's
		// wiring count. (The field is object-typed but a plain `-f
		// channel_roles=@file` write flips it to a string — ka-0000000372.)
		var m map[string]string
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			continue
		}
		for _, list := range beadsapi.ParseChannelRolesMap(m) {
			for _, r := range list {
				if r != "" {
					roles[r] = struct{}{}
				}
			}
		}
	}
	return roles, true
}

// prerunReplacedAdvice returns the set of advice bead ids that some open prerun
// config bead already delivers live per-pod (the union of every prerun entry's
// `replaces_advice`). A bead in this set is intentionally scope:on-demand — its
// capability correction is injected at boot by the probe, not left undeliverable
// — so the undeliverable-capability lint check exempts it (see LintAdvice).
//
// Fail-open by design: any fetch error or decode failure returns nil, which
// LintAdvice treats as "exempt nothing" — the check then behaves exactly as it
// did before this exemption existed. A nil return can only ADD spurious findings
// the operator can eyeball, never silently drop a real one.
func prerunReplacedAdvice(ctx context.Context) map[string]struct{} {
	// Config beads carry the prerun list in Description as JSON, keyed by the
	// `prerun` category. List them paged across ALL layers/labels — the exemption
	// is about whether ANY prerun replaces a bead, not about which role a given
	// agent matches. Paging (not a single Limit:500) so the exemption set stays
	// complete as the config-bead population grows: a truncated fetch would drop
	// prerun-replaced ids past the cap and manufacture spurious undeliverable
	// findings (the config half of the F10 silent-truncation class, ka-0000005760).
	result, err := daemon.ListAllBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"config"},
		Statuses: []string{"open"},
	})
	if err != nil {
		return nil // fail-open: exempt nothing, check behaves as before
	}
	replaced := make(map[string]struct{})
	for _, b := range result.Beads {
		if b == nil || b.Title != prerunCategory {
			continue
		}
		var payload struct {
			Prerun []struct {
				ReplacesAdvice []string `json:"replaces_advice"`
			} `json:"prerun"`
		}
		if err := json.Unmarshal([]byte(b.Description), &payload); err != nil {
			continue // skip a malformed prerun bead rather than abort the whole set
		}
		for _, e := range payload.Prerun {
			for _, id := range e.ReplacesAdvice {
				if id != "" {
					replaced[id] = struct{}{}
				}
			}
		}
	}
	return replaced
}

func printAdviceLint(findings []advice.LintFinding, corpus, errors int) {
	if len(findings) == 0 {
		fmt.Printf("No targeting problems in %d open advice beads.\n", corpus)
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSEVERITY\tCHECK\tTITLE\tMESSAGE")
	for _, f := range findings {
		title := f.Title
		if len(title) > 40 {
			title = title[:37] + "..."
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", f.ID, f.Severity, f.Check, title, f.Message)
	}
	w.Flush()
	fmt.Printf("\n%d finding(s) in %d open advice beads (%d error(s))\n", len(findings), corpus, errors)
}

func init() {
	adviceCmd.AddCommand(adviceLintCmd)
	adviceLintCmd.Flags().Bool("errors-only", false, "show only error-severity findings")
	adviceLintCmd.Flags().Bool("over-constraint", false, "run the qualitative over-constraint checks (rules-over-judgment framing, directive-density spikes, system-prompt redundancy) instead of targeting-hygiene checks")
	adviceLintCmd.Flags().Bool("strict", false, "with --over-constraint, exit non-zero when any (WARN-severity) over-constraint finding is present")
}
