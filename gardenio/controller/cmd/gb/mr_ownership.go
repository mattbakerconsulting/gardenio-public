package main

import (
	"fmt"
	"os"
	"strings"

	"gardenio/controller/internal/bridge"

	"github.com/spf13/cobra"
)

// `gb mr ownership` — the single place an agent asks "may I push to this MR's
// branch?" (bug ka-0000004809, decision ka-0000007743 → fix-now).
//
// Before this, the rule lived as PROSE duplicated across two merge-queue sweep
// trigger prompts ("rebase ... ONLY where the branch is gardenio-owned"), keyed
// on the MR author username alone. monorepo !2038 was opened by gardeniobot and
// then adopted by a human — 3 of 5 commits his, assignee him, approved by a
// reviewer — so the author-only test said "agent-owned" and the sweep
// force-pushed it. Prose cannot be unit-tested and drifted between the two
// copies; this command is the tested predicate both prompts now call.

var mrOwnershipCmd = &cobra.Command{
	Use:   "ownership <bead-id-or-mr-url>",
	Short: "Decide whether an agent may push/force-push to an MR's source branch",
	Long: `Answers ONE question: may an agent write to this MR's source branch?

Replaces the author-only prose test that let the monorepo merge-queue sweep
force-push !2038 — an MR opened by gardeniobot but ADOPTED by a human (bug
ka-0000004809). GitLab never updates ` + "`author`" + ` after the fact, so an
author-only check misclassifies every agent-opened/human-adopted MR.

Two rules, in order:

 1. WIDENED OWNERSHIP. Agent-owned requires that the MR author, EVERY branch
    commit's author AND committer, and every assignee are agent identities. Any
    human fingerprint anywhere makes it human-owned. A commit list that cannot
    be read fails CLOSED (unknown never authorizes a destructive push).
 2. GUARD, applied even when ownership says agent: never force-push an MR that
    is APPROVED or ASSIGNED. Those are exactly the states where a force-push
    resets an approval or invalidates a reviewer's checkout — comment with the
    diagnosis and let the owner press the button.

Exit codes let a sweep branch on the verdict in one line:

  0 = MAY PUSH — agent-owned, unapproved, unassigned.
  3 = DO NOT PUSH — human-owned, approved, assigned, or unreadable. Comment
      instead; ask the owner before touching the branch.
  1 = could not evaluate (bad target, API/auth/network error).

Example:

  if gb mr ownership https://gitlab.com/g/p/-/merge_requests/2038; then
    git push --force-with-lease
  else
    glab mr note 2038 -m "stale base; rebase needed — your call"
  fi

Use --json for the full signal breakdown (owner, guard, push_allowed,
human_signals, agent_signals).`,
	Args: cobra.ExactArgs(1),
	RunE: runMROwnership,
}

// mrOwnershipAgentUsers / mrOwnershipAgentEmails let a caller on a different
// GitLab instance or bot identity override the built-in agent identity set
// rather than forking the predicate.
var (
	mrOwnershipAgentUsers  []string
	mrOwnershipAgentEmails []string
)

func init() {
	mrOwnershipCmd.Flags().StringSliceVar(&mrOwnershipAgentUsers, "agent-user", nil,
		"GitLab username considered an agent identity (repeatable; replaces the built-in list)")
	mrOwnershipCmd.Flags().StringSliceVar(&mrOwnershipAgentEmails, "agent-email", nil,
		"Git commit email considered an agent identity (repeatable; replaces the built-in list)")
	mrCmd.AddCommand(mrOwnershipCmd)
}

func runMROwnership(cmd *cobra.Command, args []string) error {
	mrURL, err := resolveMRURL(cmd, args[0])
	if err != nil {
		return err
	}
	ref := bridge.ParseMRURL(mrURL)
	if ref == nil {
		return fmt.Errorf("could not parse MR URL: %s", mrURL)
	}
	client, err := newMRGitLabClient(baseURLFromGitLabURL(mrURL))
	if err != nil {
		return err
	}

	identity := bridge.AgentIdentity{
		Usernames: mrOwnershipAgentUsers,
		Emails:    mrOwnershipAgentEmails,
	}
	verdict, in, err := bridge.FetchBranchOwnership(cmd.Context(), client, ref.ProjectPath, ref.IID, identity)
	if err != nil {
		return err
	}

	if jsonOutput {
		printJSON(map[string]any{
			"mr_url":        mrURL,
			"project":       ref.ProjectPath,
			"mr_iid":        ref.IID,
			"owner":         string(verdict.Owner),
			"push_allowed":  verdict.PushAllowed,
			"guard":         verdict.Guard,
			"reason":        verdict.Reason,
			"human_signals": verdict.HumanSignals,
			"agent_signals": verdict.AgentSignals,
			"author":        in.AuthorUsername,
			"assignees":     in.AssigneeUsernames,
			"approved":      in.Approved,
			"approved_by":   in.ApprovedBy,
			"commits_read":  in.CommitsRead,
			"commit_count":  len(in.Commits),
		})
	} else {
		cmd.Println(verdict.String())
		cmd.Printf("  owner: %s", verdict.Owner)
		if verdict.Guard != "" {
			cmd.Printf("  guard: %s", verdict.Guard)
		}
		cmd.Println()
		printOwnershipSignals(cmd, "human", verdict.HumanSignals)
		printOwnershipSignals(cmd, "agent", verdict.AgentSignals)
		if !verdict.PushAllowed {
			cmd.Println("  Do NOT push or force-push. Post a comment with the diagnosis and let the owner act.")
		}
	}

	if !verdict.PushAllowed {
		// Distinct non-zero exit (3), matching `gb mr verify-cleanup`/
		// `dispatch-gate`, so a sweep's shell separates "refused, comment
		// instead" from a real error (1). Message already printed.
		cmd.SilenceUsage = true
		cmd.SilenceErrors = true
		os.Exit(3)
	}
	return nil
}

func printOwnershipSignals(cmd *cobra.Command, kind string, signals []string) {
	for _, s := range signals {
		cmd.Printf("  [%s] %s\n", kind, strings.TrimSpace(s))
	}
}
