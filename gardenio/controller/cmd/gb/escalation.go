package main

import (
	"github.com/spf13/cobra"
)

var escalationCmd = &cobra.Command{
	Use:     "escalation",
	Short:   "Manage privilege escalations",
	GroupID: "orchestration",
}

func init() {
	// escalation create flags.
	escalationCreateCmd.Flags().String("title", "", "Escalation title (required)")
	escalationCreateCmd.Flags().String("operation-type", "", "Operation type, e.g. aws_iam, k8s_rbac, credential_rotation (required)")
	escalationCreateCmd.Flags().String("operation-spec", "", "Operation specification as JSON. Required (a non-empty JSON object) when the escalation names an execution_target or is a create/update/grant replay; optional for verdict records, manual requests and a close")
	escalationCreateCmd.Flags().String("mr-url", "", "Merge escalations: the PR/MR to merge (sets operation_spec.mr_url). Without it: the current branch's PR, then a PR URL in the title, then --requesting-task's mr_url; a title naming another PR is refused. Filed the wrong PR? gb escalation amend")
	escalationCreateCmd.Flags().String("script", "", "Execution script (inline)")
	escalationCreateCmd.Flags().String("script-file", "", "Execution script (from file)")
	escalationCreateCmd.Flags().String("context", "", "Human-readable context/background (@file reads the value from a file, @- reads stdin; @@ escapes a literal leading '@')")
	escalationCreateCmd.Flags().String("credentials", "", "Credential requirements as JSON array")
	escalationCreateCmd.Flags().String("data-requirements", "", "Data requirements as JSON array")
	escalationCreateCmd.Flags().String("expected-outputs", "", "Expected outputs as JSON array")
	escalationCreateCmd.Flags().String("sensitivity", "", "Sensitivity: public, internal, sensitive, secret")
	escalationCreateCmd.Flags().String("audience", "", audienceFlagHelp)
	escalationCreateCmd.Flags().String("execution-target", "", "Execution target: local, service_account, server. local runs it on one named machine and needs --target-runner or --target-owner. To ask the operator or the laptop coordinator to act, leave this unset; the escalation policy routes it")
	escalationCreateCmd.Flags().String("target-runner", "", "Runner bead id of the machine that executes it (`gb runner status`); with --execution-target local, this or --target-owner is required. The agent joined on that runner (`gb runner join`) executes it")
	escalationCreateCmd.Flags().String("target-owner", "", "Operator id (oidc:<subject>) whose one online runner executes it, resolved at approval; an alternative to --target-runner with --execution-target local")
	escalationCreateCmd.Flags().String("spawn-role", "", "Privileged role the daemon spawns to run --script (required with --execution-target service_account)")
	escalationCreateCmd.Flags().String("requesting-task", "", "Task bead ID that triggered this escalation; writes a blocks-edge from it to the escalation so the blocked work (and its /plans plan) reads as blocked. If omitted, auto-derived from the current agent bead's task_id (ka-0000002745)")
	escalationCreateCmd.Flags().String("implements-decision", "", "Decision bead id of the owner ruling this escalation carries out. Cite the chosen option id in --operation-spec as \"chosen\" (a string or list). The chosen option must name this escalation's operation_type, target_bead_id (if any) and operation_spec (equal to --operation-spec less \"chosen\"), and a ruling approves one escalation; the daemon then files it approved by that ruling, unless it is a floor item (secrets, credentials, who may approve), needs four eyes, or runs locally or as a service account (P7.5)")
	escalationCreateCmd.Flags().Bool("close-on-execute", false, "Auto-close the --requesting-task when this escalation executes (stamps the close-on-escalation-execute contract label; the wrapper task is a pure blocker that has nothing left to do once the escalation runs)")
	escalationCreateCmd.Flags().StringArray("label", nil, "Labels (repeatable)")
	escalationCreateCmd.Flags().Int("priority", 0, "Priority (0=unset, 1=highest)")
	escalationCreateCmd.Flags().BoolVar(&skipMergeCapabilityCheck, "skip-merge-capability-check", false, "Bypass the gitlab_mr_merge guard that refuses an escalation for an MR you can merge yourself (or that already merged). Use only when GitLab misreports your access and you have the real error to show.")

	// escalation list flags.
	escalationListCmd.Flags().String("status", "", "Filter by status (comma-separated)")
	escalationListCmd.Flags().Bool("mine", false, "only escalations MY lane owns (owner_seat = my seat_key, with a created_by = --actor fallback); survives respawn. Errors when neither a seat nor an actor is resolvable")
	escalationListCmd.Flags().String("lane", "", "only escalations owned by another lane, by its seat_key (owner_seat = <seat>); mutually exclusive with --mine")
	escalationListCmd.Flags().String("held-by", "", "only escalations held by these lanes (comma-separated lane refs, e.g. operator,coordinator:laptop; an unstamped escalation counts as operator). Mutually exclusive with --mine/--lane/--needs-me")
	escalationListCmd.Flags().Bool("needs-me", false, "only escalations held by a lane YOU are a member of (held_by=@me, resolved by the daemon)")

	escalationHandUpCmd.Flags().String("reason", "", "why this needs the next level up (required)")
	escalationHandUpCmd.Flags().String("operator-class", "", "the merge desk's OPERATOR-ONLY class (1, 2 or 3): puts a PR merge on the owner floor")
	escalationRerouteCmd.Flags().Bool("approved-unexecuted", false, "re-target approved, unexecuted escalations the operator holds (required)")
	escalationRerouteCmd.Flags().Bool("dry-run", false, "list what would move without writing")
	escalationListCmd.Flags().Int("limit", 0, "Max escalations to show; 0 (default) returns ALL. A positive limit prints a truncation banner with the true count when it hides rows.")

	// escalation show flags.
	escalationShowCmd.Flags().Bool("json", false, "Output as JSON")

	// escalation approve flags.
	escalationApproveCmd.Flags().String("as", "", "Approver identity (default: KD_ACTOR or --actor)")
	escalationApproveCmd.Flags().String("execution-target", "", "Override execution target, recorded with the approval (local, service_account)")
	escalationApproveCmd.Flags().String("comment", "", "Optional note recorded with the approval, as a comment on the escalation by you")

	// escalation reject flags.
	escalationRejectCmd.Flags().String("as", "", "Rejector identity (default: KD_ACTOR or --actor)")
	escalationRejectCmd.Flags().String("reason", "", "Rejection reason")

	// escalation close flags.
	escalationCloseCmd.Flags().String("status", "", "Result status: success, failure, partial (required)")
	escalationCloseCmd.Flags().String("result", "", `Result data as JSON, or prose (stored as {"output":"<text>"})`)
	escalationCloseCmd.Flags().String("executor", "", "Executor identity (default: KD_ACTOR or --actor)")
	escalationCloseCmd.Flags().String("deployed-tag", "", "helm_deploy: the tag the roll put on the target; the daemon records the release's deployment (needs --status success)")
	escalationCloseCmd.Flags().String("revision", "", "helm_deploy: the Helm revision the roll made (with --deployed-tag)")
	escalationCloseCmd.Flags().String("target", "", "helm_deploy: the release target the roll went to (with --deployed-tag; default prod)")
	escalationCloseCmd.Flags().Bool("keep-requesting-task", false, "Leave the requesting task open although the escalation was filed --close-on-execute (e.g. this merge does not carry the task's fix)")
	// P7 Task 10: the daemon ignores the body executor.
	_ = escalationCloseCmd.Flags().MarkDeprecated("executor", "the daemon records the authenticated caller as executor")

	// escalation backfill-links flags.
	escalationBackfillLinksCmd.Flags().Bool("dry-run", false, "Preview links without writing")

	// escalation reflag flags — must mirror decisionReflagCmd's since both feed
	// the shared runReflag body.
	escalationReflagCmd.Flags().String("reason", "", "reason for reflag (recorded as audit comment and included in Slack repost)")
	escalationReflagCmd.Flags().Int("priority", -1, "set priority explicitly (0=critical .. 4=backlog) instead of bumping by one toward P0")

	// escalation pingback flags.
	escalationPingbackCmd.Flags().String("reason", "", "reason for the pingback (recorded on the bead and shown in the Slack re-post)")
	escalationPingbackCmd.Flags().Bool("force", false, "override the 1h pingback rate limit")

	escalationCmd.AddCommand(escalationCreateCmd)
	escalationCmd.AddCommand(escalationListCmd)
	escalationCmd.AddCommand(escalationAmendCmd)
	escalationCmd.AddCommand(escalationShowCmd)
	escalationCmd.AddCommand(escalationApproveCmd)
	escalationCmd.AddCommand(escalationRejectCmd)
	escalationCmd.AddCommand(escalationCloseCmd)
	escalationCmd.AddCommand(escalationReflagCmd)
	escalationCmd.AddCommand(escalationPingbackCmd)
	escalationCmd.AddCommand(escalationClaimCmd)
	escalationCmd.AddCommand(escalationHandUpCmd)
	escalationCmd.AddCommand(escalationRerouteCmd)
	escalationCmd.AddCommand(escalationStatsCmd)
	escalationCmd.AddCommand(escalationBackfillLinksCmd)
	escalationCmd.AddCommand(newSuccessorCmd("escalation"))
}
