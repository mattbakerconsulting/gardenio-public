package main

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"
)

// ── escalation amend (gd-3VaIxgZfDp) ────────────────────────────────────────
//
// operation_spec is what the approver reviews, so kbeads keeps it immutable.
// The one exception is the filer correcting its own merge escalation while it
// is open, unclaimed, not handed up and unapproved (filerMayAmendMergeSpec in
// kbeads): a wrong mr_url used to mean withdraw and refile (gd-fmQTQDXHTt), or
// a comment the desk had to notice (gd-leerYGCjIS still named closed PR #369).

var escalationAmendCmd = &cobra.Command{
	Use:   "amend <escalation-id>",
	Short: "Correct the PR on your own open merge escalation",
	Long: `Correct operation_spec on a merge escalation (gitlab_mr_merge,
github_pr_merge) you filed, while it is still open, unclaimed and unapproved.

  --mr-url URL           point the escalation at this PR: replaces every key
                         that names a PR and keeps the rest of the spec.
  --operation-spec JSON  replace the whole spec.

Only the filer may amend, on its own per-agent token or signed-in identity
(never the shared static token), and only before the desk claims, hands up or
anyone approves it; kbeads refuses everything else. The change is recorded as a comment on
the escalation. If the title names the old PR, fix it too:
kd update <id> --title "...".`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id := args[0]
		mrURL, _ := cmd.Flags().GetString("mr-url")
		specFlag, _ := cmd.Flags().GetString("operation-spec")
		if (mrURL == "") == (specFlag == "") {
			return fmt.Errorf("give exactly one of --mr-url or --operation-spec")
		}

		b, err := daemon.GetBead(cmd.Context(), id)
		if err != nil {
			return fmt.Errorf("reading %s: %w", id, err)
		}
		if b.Type != "escalation" {
			return fmt.Errorf("%s is a %s, not an escalation", id, b.Type)
		}
		opType := b.Fields["operation_type"]
		if !mergeOperationTypes[opType] {
			return fmt.Errorf("%s is a %s escalation; only a merge escalation's spec can be amended (withdraw and refile anything else)", id, opType)
		}
		if b.Status != "open" {
			return fmt.Errorf("%s is %s; its spec can be amended only while it is open", id, b.Status)
		}
		if b.Fields["claimed_by"] != "" || b.Fields["approved_at"] != "" || b.Fields["handed_up_by"] != "" {
			return fmt.Errorf("%s is already claimed, handed up or approved; its spec is fixed. Comment the right PR on it instead: kd comment add %s \"...\"", id, id)
		}

		oldRaw := b.Fields["operation_spec"]
		var spec map[string]any
		if specFlag != "" {
			if err := json.Unmarshal([]byte(specFlag), &spec); err != nil || spec == nil {
				return fmt.Errorf("--operation-spec must be a JSON object")
			}
		} else {
			u := firstPRURL(mrURL)
			if u == "" {
				return fmt.Errorf("--mr-url %q is not a GitHub PR or GitLab MR URL", mrURL)
			}
			if oldRaw != "" {
				_ = json.Unmarshal([]byte(oldRaw), &spec)
			}
			if spec == nil {
				spec = map[string]any{}
			}
			setSpecMRURL(spec, u)
		}

		// Same one-open-escalation-per-PR rule as create: repointing this one
		// at a PR another open merge escalation already names would leave the
		// desk two handles for one merge.
		if key := prKey(specPRURL(spec)); key != "" {
			if other := findOpenMergeEscalation(cmd.Context(), daemon, key); other != nil && other.ID != id {
				return fmt.Errorf("merge escalation %s is already open for %s; withdraw %s instead of pointing it at the same PR", other.ID, key, id)
			}
		}

		if err := daemon.UpdateBeadFieldsTyped(cmd.Context(), id, map[string]any{"operation_spec": spec}); err != nil {
			if strings.Contains(err.Error(), "immutable") {
				return fmt.Errorf("kbeads refused the amend (only the filer, on its own per-agent token or signed-in identity, may amend, and only while the escalation is open, unclaimed, not handed up and unapproved; an older daemon refuses every amend): %w", err)
			}
			return fmt.Errorf("amending %s: %w", id, err)
		}
		newRaw, _ := json.Marshal(spec)
		note := fmt.Sprintf("operation_spec amended by the filer: %s -> %s", oldRaw, newRaw)
		if err := daemon.AddComment(cmd.Context(), id, actor, note); err != nil {
			fmt.Fprintf(os.Stderr, "WARNING: amended, but could not record it as a comment on %s: %v\n", id, err)
		}
		if n := prNumber(specPRURL(spec)); n != 0 {
			if nums := prNumbersIn(b.Title); len(nums) > 0 && !slices.Contains(nums, n) {
				fmt.Fprintf(os.Stderr, "WARNING: the title still names %s: kd update %s --title \"...\"\n", joinPRNumbers(nums), id)
			}
		}
		fmt.Fprintf(cmd.OutOrStdout(), "escalation_id=%s\noperation_spec=%s\n", id, newRaw)
		return nil
	},
}

func init() {
	escalationAmendCmd.Flags().String("mr-url", "", "the PR/MR the escalation should merge")
	escalationAmendCmd.Flags().String("operation-spec", "", "replacement operation_spec, as a JSON object")
}
