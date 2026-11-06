package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

// consent_to_push.go implements `gb consent-to-push` — the FRONT half of the
// "prepared fix → consent → push" flow the operator opened the thread with
// (Matt Baker, 2026-08-27, thread ka-0000006045, design ka-0000005342 S2/M4):
//
//	"for stuff like the monorepo cicd pipeline failure thing, it'd be great if
//	 we could create commits for users to use, but we dont want to interfere …
//	 DM them a report/message and a decision that asks whether they want their
//	 MR pushed to."
//
// A CI-failure triage agent that has prepared a fix and PUSHED IT TO A SIDE
// BRANCH (never the author's MR branch — that is the "don't interfere" part)
// runs this to hand the MR author a decision, addressed to THEM, with the three
// canonical options push-to-my-mr / open-separate-mr / no-thanks. The DM +
// addressee resolution + resolve-permission widening are all C1–C3, already
// merged: this command is the thin composition layer that (a) fixes the option
// ids/labels/artifact_types into a stable contract, (b) carries the side-branch
// + MR pointers as machine-readable fields, and (c) renders the diff-summary
// report and per-option execution instructions into the decision body.
//
// The BACK half is free: DecisionRespawner already respawns an agent to act on
// the resolved decision (decisions.go respawnAgentForDecision), handing it the
// chosen option and the full decision context. The executor reads the
// consent_push_* fields below to know exactly which branch to push where.

// Canonical consent-to-push option ids. These are a CONTRACT the executor (the
// DecisionRespawner-spawned agent) keys on — do not rename without updating the
// execution instructions rendered into the decision body.
const (
	consentPushToMyMR    = "push-to-my-mr"
	consentOpenSeparate  = "open-separate-mr"
	consentNoThanks      = "no-thanks"
	consentPushFieldFlag = "consent_push"
)

// consentToPushOptions returns the three canonical options for a consent-to-push
// decision, in presentation order. The artifact_type on each option is what the
// system expects the respawned executor to PRODUCE for that choice:
//   - push-to-my-mr    → a diff-summary of what landed on the author's MR.
//   - open-separate-mr → a report linking the new MR.
//   - no-thanks        → a report noting the side branch was discarded.
func consentToPushOptions() []map[string]any {
	return []map[string]any{
		{
			"id":            consentPushToMyMR,
			"short":         "Push to my MR",
			"label":         "Push the fix to my MR branch",
			"artifact_type": "diff-summary",
		},
		{
			"id":            consentOpenSeparate,
			"short":         "Open a separate MR",
			"label":         "Open a separate MR with the fix",
			"artifact_type": "report",
		},
		{
			"id":            consentNoThanks,
			"short":         "No thanks",
			"label":         "No thanks — discard the prepared fix",
			"artifact_type": "report",
		},
	}
}

// consentPushAutoField is the type:user field recording a person's standing
// consent to have prepared CI fixes pushed to their MRs (gd-QV2WuUjely). Only an
// OIDC gardenio-admin or the person themself (POST /v1/users/me/consent-push)
// may set it, and the daemon, not this CLI, answers a consent-to-push decision
// for someone who gave it: the decision comes back already closed with
// push-to-my-mr chosen and responded_by system:standing-consent.
const consentPushAutoField = "consent_push_to_mr_auto"

// hasConsent reports whether a user bead carries standing consent. Only the
// top-level field counts; a consent_push_to_mr_auto key inside preferences (the
// old location, which any agent could write) is ignored, as the daemon ignores it.
func hasConsent(bead *beadsapi.BeadDetail) bool {
	return bead != nil && strings.TrimSpace(bead.Fields[consentPushAutoField]) == "true"
}

// answeredByStandingConsent reports whether the daemon answered a just-filed
// consent-to-push decision from the addressee's standing consent.
func answeredByStandingConsent(bead *beadsapi.BeadDetail) bool {
	return bead != nil && bead.Status == "closed" && bead.Fields["chosen"] == consentPushToMyMR &&
		bead.Fields["responded_by"] == standingConsentActor
}

// standingConsentActor is the responder kbeads records on a decision answered
// by standing consent (kbeads user_standing_consent.go).
const standingConsentActor = "system:standing-consent"

// renderConsentContext builds the decision body: the human-facing diff-summary
// report followed by the machine-facing execution contract, so BOTH the MR
// author (reading the DM) and the respawned executor (reading the bead) have
// everything they need in one place.
func renderConsentContext(mrURL, branch, targetBranch, report string) string {
	var b strings.Builder
	b.WriteString("A CI-triage agent prepared a fix for the failing pipeline on your MR and pushed it to a side branch — it did NOT touch your branch. Choose what happens next.\n\n")
	b.WriteString("**MR:** ")
	b.WriteString(mrURL)
	b.WriteString("\n**Prepared fix (side branch):** `")
	b.WriteString(branch)
	b.WriteString("`\n")
	if strings.TrimSpace(targetBranch) != "" {
		b.WriteString("**Would land on:** `")
		b.WriteString(targetBranch)
		b.WriteString("`\n")
	}
	b.WriteString("\n## Diff summary\n\n")
	b.WriteString(strings.TrimSpace(report))
	b.WriteString("\n\n## What each option does\n\n")
	b.WriteString(fmt.Sprintf("- **%s** — cherry-pick/merge `%s` onto ", consentPushToMyMR, branch))
	if strings.TrimSpace(targetBranch) != "" {
		b.WriteString(fmt.Sprintf("`%s` and push to your MR.\n", targetBranch))
	} else {
		b.WriteString("your MR's source branch and push.\n")
	}
	b.WriteString(fmt.Sprintf("- **%s** — open a new MR from `%s` so you can review it independently.\n", consentOpenSeparate, branch))
	b.WriteString(fmt.Sprintf("- **%s** — discard the prepared fix; delete `%s`.\n", consentNoThanks, branch))
	b.WriteString("\n## Opt-in to automatic pushes\n\n")
	b.WriteString("💡 **Tip:** If you'd prefer to skip this step in the future, you can opt in to having CI fixes pushed automatically to your MRs: run `gb consent-to-push-manage opt-in` signed in as yourself. These fixes will then be applied without a decision prompt.\n")
	return b.String()
}

// validateConsentInputs enforces the minimum a consent-to-push decision needs to
// be actionable by the executor: a real MR URL, a side branch, and an addressee.
func validateConsentInputs(mrURL, branch, forHuman string) error {
	if strings.TrimSpace(mrURL) == "" {
		return fmt.Errorf("--mr is required (the merge request whose pipeline failed)")
	}
	if !strings.Contains(mrURL, "/merge_requests/") {
		return fmt.Errorf("--mr %q does not look like a GitLab merge-request URL (expected …/-/merge_requests/<n>)", mrURL)
	}
	if strings.TrimSpace(branch) == "" {
		return fmt.Errorf("--branch is required (the side branch you pushed the prepared fix to)")
	}
	if strings.TrimSpace(forHuman) == "" {
		return fmt.Errorf("--for is required (the MR author to address the decision to: an email, a Slack id/mention, or oidc:/slack: identity)")
	}
	return nil
}

var consentToPushCmd = &cobra.Command{
	Use:     "consent-to-push",
	Short:   "Ask an MR author to consent to a prepared CI fix (front half of consent-to-push)",
	GroupID: "orchestration",
	Long: `Ask an MR author to consent to a prepared CI fix.

For a CI-failure triage agent that has prepared a fix and PUSHED IT TO A SIDE
BRANCH (never the author's MR branch — don't interfere). This files a decision
addressed to the MR author, DM'd to them, with the three canonical options:

  push-to-my-mr     cherry-pick/merge the fix onto their MR and push
  open-separate-mr  open a new MR from the side branch
  no-thanks         discard the prepared fix

DecisionRespawner respawns an agent to ACT on whatever they choose, reading the
consent_push_mr / consent_push_branch fields this command stamps — so the whole
"prepared fix → consent → push" loop is closed once you run this.

Example:
  gb consent-to-push \
    --mr=https://gitlab.com/example-org/app/-/merge_requests/123 \
    --branch=ci-fix/ka-ABC-pipeline \
    --target-branch=feature/authors-branch \
    --for=author@example.com \
    --report=@diff-summary.md \
    --requesting-task=ka-ABC \
    --no-wait`,
	RunE: func(cmd *cobra.Command, args []string) error {
		mrURL, _ := cmd.Flags().GetString("mr")
		branch, _ := cmd.Flags().GetString("branch")
		targetBranch, _ := cmd.Flags().GetString("target-branch")
		forHuman, _ := cmd.Flags().GetString("for")
		reportRaw, _ := cmd.Flags().GetString("report")
		requestingTask, _ := cmd.Flags().GetString("requesting-task")
		priority, _ := cmd.Flags().GetInt("priority")
		noWait, _ := cmd.Flags().GetBool("no-wait")

		if err := validateConsentInputs(mrURL, branch, forHuman); err != nil {
			return err
		}

		// Expand curl-style @file / @- / @@ on --report, matching --context on
		// `gb decision create` (atfile.go), so a bare "@report.md" reads the file
		// rather than persisting a dangling pointer (ka-0000003861).
		report, err := resolveTextValue(reportRaw)
		if err != nil {
			return err
		}
		if strings.TrimSpace(report) == "" {
			return fmt.Errorf("--report is required (the diff-summary of the prepared fix; @file / @- accepted)")
		}

		fields := map[string]any{
			"options":             consentToPushOptions(),
			"context":             renderConsentContext(mrURL, branch, targetBranch, report),
			consentPushFieldFlag:  "true",
			"consent_push_mr":     mrURL,
			"consent_push_branch": branch,
			"mr_url":              mrURL,
			"requested_by":        actor,
		}
		if strings.TrimSpace(targetBranch) != "" {
			fields["consent_push_target_branch"] = targetBranch
		}

		agentID, _ := resolveAgentID("")
		if agentID != "" {
			fields["requesting_agent_bead_id"] = agentID
			if seat, gardener := resolveDecisionOwner(cmd.Context(), agentID); seat != "" || gardener != "" {
				if seat != "" {
					fields["owner_seat"] = seat
				}
				if gardener != "" {
					fields["owner_gardener"] = gardener
				}
			}
		}

		// Address the decision to the MR author (C3, ka-0000000024): --for is
		// REQUIRED here (validated above) — the whole point is asking someone who
		// is NOT the filing agent's own operator. decisionAddresseeFields stamps
		// operated_by (who the bridge DMs) and gifted_by (the filing agent).
		addressee, err := decisionAddresseeFields(cmd.Context(), forHuman, agentID)
		if err != nil {
			return err
		}
		for k, v := range addressee {
			fields[k] = v
		}
		if requestingTask != "" {
			fields["requesting_task"] = requestingTask
		}

		project := resolveDecisionProject(cmd.Context(), requestingTask)
		var labels []string
		if project != "" {
			labels = []string{"project:" + project}
		}

		prompt := fmt.Sprintf("CI fix ready for %s — push to your MR, open a separate MR, or discard?", mrURL)
		if len(prompt) > 500 {
			prompt = prompt[:500]
		}

		fieldsJSON, err := json.Marshal(fields)
		if err != nil {
			return fmt.Errorf("encoding fields: %w", err)
		}

		id, err := daemon.CreateBead(cmd.Context(), beadsapi.CreateBeadRequest{
			Title:     prompt,
			Type:      "decision",
			Kind:      "data",
			Priority:  priority,
			Assignee:  actor,
			CreatedBy: actor,
			Labels:    labels,
			Fields:    fieldsJSON,
		})
		if err != nil {
			return fmt.Errorf("creating consent-to-push decision: %w", err)
		}

		// Materialize the decision→work blocks-edge for the requesting task
		// (ka-0000003927), mirroring `gb decision create` — best-effort, the
		// decision must not be lost to a link failure.
		if requestingTask != "" {
			if err := daemon.AddDependency(cmd.Context(), requestingTask, id, "blocks", actor); err != nil {
				fmt.Fprintf(os.Stderr, "WARNING: decision created but could not link blocks-edge from %s: %v\n", requestingTask, err)
			}
			if planID := resolveOwningPlan(cmd.Context(), requestingTask); planID != "" {
				if err := daemon.UpdateBeadFields(cmd.Context(), id, map[string]string{
					"requesting_plan": planID,
				}); err != nil {
					fmt.Fprintf(os.Stderr, "WARNING: could not record requesting_plan %s: %v\n", planID, err)
				}
			}
		}

		// The daemon answers the decision at create time when the addressee
		// gave standing consent; there is then nothing to wait for.
		var standing bool
		if created, err := daemon.GetBead(cmd.Context(), id); err == nil {
			standing = answeredByStandingConsent(created)
		}

		if jsonOutput {
			out := map[string]string{"id": id}
			if standing {
				out["chosen"] = consentPushToMyMR
				out["responded_by"] = standingConsentActor
			}
			printJSON(out)
		} else if standing {
			fmt.Printf("Created consent-to-push decision: %s (addressed to %s); answered %s by their standing consent\n", id, forHuman, consentPushToMyMR)
		} else {
			fmt.Printf("Created consent-to-push decision: %s (addressed to %s)\n", id, forHuman)
		}

		if noWait || standing {
			return nil
		}
		fmt.Fprintf(os.Stderr, "Waiting for response...\n")
		return waitForDecision(cmd, id)
	},
}

func init() {
	consentToPushCmd.Flags().String("mr", "", "the merge-request URL whose pipeline failed (required)")
	consentToPushCmd.Flags().String("branch", "", "the side branch you pushed the prepared fix to (required) — NOT the author's MR branch")
	consentToPushCmd.Flags().String("target-branch", "", "the MR's source branch the fix would land on (optional; renders into the execution instructions)")
	consentToPushCmd.Flags().String("for", "", "the MR author to address the decision to (required): an email, a Slack id/mention (U0AB1CD2EF, <@U0AB1CD2EF>), or an explicit identity (oidc:…, slack:…). Stamps operated_by (who the bridge DMs) and gifted_by (you).")
	consentToPushCmd.Flags().String("report", "", "the diff-summary report body (required; @file reads a file, @- reads stdin, @@ escapes a literal leading '@')")
	consentToPushCmd.Flags().String("requesting-task", "", "your current task bead id; writes a blocks-edge from that task to the decision so the blocked work reads as blocked (ka-0000003927)")
	consentToPushCmd.Flags().Int("priority", 2, "decision priority: 0=critical, 1=high, 2=normal, 3=low, 4=backlog")
	consentToPushCmd.Flags().Bool("no-wait", false, "return immediately without waiting for response")

	rootCmd.AddCommand(consentToPushCmd)
}
