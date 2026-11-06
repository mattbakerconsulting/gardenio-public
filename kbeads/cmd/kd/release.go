package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"

	"gardenio/kbeads/internal/client"
	"github.com/spf13/cobra"
)

// release.go: a project's releases and where they run
// (gardenio/docs/releases-design.md, plan gd-UZKZ9H25UD step 2). The verbs
// call the daemon's /v1/releases routes. create and deploy are refused (403)
// unless the caller is the release captain, the operator or gardenio-admin;
// anyone who can read the project reads its releases.

var releaseCmd = &cobra.Command{
	Use:     "release",
	Aliases: []string{"releases"},
	Short:   "A project's releases: what each carries and where it runs",
	Long: `A release is one tag of one project: the commit, the merged PRs it carries and
where it runs. Its status is derived by the daemon: cut (recorded), built (every
artifact exists), live (running on at least one target), superseded (replaced on
every target). A project has releases only when its project bead sets
release_tag_pattern; the tag must match it.

  kd release list --project gardenio                  # newest first
  kd release show 2026.300.1 --project gardenio       # by tag, or by release bead id
  kd release create --project gardenio --tag 2026.300.1 --sha <sha>
  kd release open gardenio                            # the next release, before its cut
  kd release deploy 2026.300.1 --project gardenio --target prod --revision 412
  kd release repair-deployments --project gardenio --dry-run

create, open, deploy and repair-deployments are for the release captain, the operator
and gardenio-admin.`,
	GroupID: "workflow",
}

var releaseListCmd = &cobra.Command{
	Use:   "list",
	Short: "List releases, newest cut first",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		project, _ := cmd.Flags().GetString("project")
		status, _ := cmd.Flags().GetString("status")
		limit, _ := cmd.Flags().GetInt("limit")
		rels, err := beadsClient.ListReleases(cmd.Context(), project, status, limit)
		if err != nil {
			return err
		}
		if jsonOutput {
			if rels == nil {
				rels = []client.Release{}
			}
			emitJSON(rels)
			return nil
		}
		if len(rels) == 0 {
			fmt.Println("No releases found.")
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "TAG\tPROJECT\tSTATUS\tCUT_AT\tRUNS_ON\tID")
		for _, r := range rels {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
				r.Tag, r.Project, releaseStatusWord(r), dashIfEmpty(r.CutAt), dashIfEmpty(strings.Join(releaseRunsOn(r), ",")), r.ID)
		}
		return w.Flush()
	},
}

var releaseShowCmd = &cobra.Command{
	Use:   "show <id|tag>",
	Short: "Show one release (a tag needs --project)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		project, _ := cmd.Flags().GetString("project")
		r, err := beadsClient.GetRelease(cmd.Context(), args[0], project)
		if err != nil {
			return err
		}
		if jsonOutput {
			emitJSON(r)
			return nil
		}
		printRelease(r)
		return nil
	},
}

var releaseCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Record a release (release captain, operator, gardenio-admin)",
	Long: `Record a release of --project at --tag. A second create of the same project and
tag returns the existing release. previous defaults to the release named by
--previous-tag, else the newest earlier release of the project; cut_at to now;
cut_by to you. --contents and --images take a JSON list (@file reads a file).`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		req := client.CreateReleaseRequest{}
		req.Project, _ = cmd.Flags().GetString("project")
		req.Tag, _ = cmd.Flags().GetString("tag")
		req.SHA, _ = cmd.Flags().GetString("sha")
		req.Previous, _ = cmd.Flags().GetString("previous")
		req.PreviousTag, _ = cmd.Flags().GetString("previous-tag")
		req.CutAt, _ = cmd.Flags().GetString("cut-at")
		req.CutBy, _ = cmd.Flags().GetString("cut-by")
		req.Source, _ = cmd.Flags().GetString("source")
		req.BuiltAt, _ = cmd.Flags().GetString("built-at")
		if req.Project == "" || req.Tag == "" {
			return fmt.Errorf("--project and --tag are required")
		}
		for flag, dst := range map[string]*json.RawMessage{"contents": &req.Contents, "images": &req.Images} {
			v, _ := cmd.Flags().GetString(flag)
			if v == "" {
				continue
			}
			text, err := resolveTextValue(v)
			if err != nil {
				return fmt.Errorf("--%s: %w", flag, err)
			}
			if !json.Valid([]byte(text)) {
				return fmt.Errorf("--%s is not valid JSON", flag)
			}
			*dst = json.RawMessage(text)
		}
		if cmd.Flags().Changed("contents-complete") {
			v, _ := cmd.Flags().GetBool("contents-complete")
			req.ContentsComplete = &v
		}
		r, err := beadsClient.CreateRelease(cmd.Context(), req)
		if err != nil {
			return err
		}
		if jsonOutput {
			emitJSON(r)
			return nil
		}
		printRelease(r)
		return nil
	},
}

var releaseOpenCmd = &cobra.Command{
	Use:   "open <project>",
	Short: "Open a project's next release ahead of its cut (release captain, operator, gardenio-admin)",
	Long: `Open the project's next release: a release with no tag yet (status open), which
merged PRs board and the cut writes its tag onto. A project has at most one; when it
already has one, that release is printed and nothing is written. The recorder
(gb release record) opens the next one after every cut.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		r, err := beadsClient.CreateRelease(ctx, client.CreateReleaseRequest{Project: args[0]})
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict {
			// The daemon refuses a second open release: show the one there is.
			if rels, lerr := beadsClient.ListReleases(ctx, args[0], "open", 1); lerr == nil && len(rels) == 1 {
				r, err = &rels[0], nil
			}
		}
		if err != nil {
			return err
		}
		if jsonOutput {
			emitJSON(r)
			return nil
		}
		printRelease(r)
		return nil
	},
}

var releaseDeployCmd = &cobra.Command{
	Use:   "deploy <id|tag>",
	Short: "Record that a release runs on a target (release captain, operator, gardenio-admin)",
	Long: `Record that a release now runs on --target, one of its project's release_targets.
The release that held the target is superseded there. A helm revision is one roll:
a report of a revision the release already records on the target changes nothing,
and one another release records is refused (409). Without --revision, a repeat of
the open row changes nothing. Deploying an older release is a rollback. A tag
needs --project.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		project, _ := cmd.Flags().GetString("project")
		req := client.DeployReleaseRequest{}
		req.Target, _ = cmd.Flags().GetString("target")
		req.Kind, _ = cmd.Flags().GetString("kind")
		req.Since, _ = cmd.Flags().GetString("since")
		req.By, _ = cmd.Flags().GetString("by")
		req.Via, _ = cmd.Flags().GetString("via")
		req.Revision, _ = cmd.Flags().GetString("revision")
		req.ImageTag, _ = cmd.Flags().GetString("image-tag")
		if req.Target == "" {
			return fmt.Errorf("--target is required")
		}
		r, err := beadsClient.DeployRelease(cmd.Context(), args[0], project, req)
		if err != nil {
			return err
		}
		if jsonOutput {
			emitJSON(r)
			return nil
		}
		printRelease(r)
		return nil
	},
}

var releaseRepairDeploymentsCmd = &cobra.Command{
	Use:   "repair-deployments",
	Short: "Rebuild a project's deployment history: one row per revision (release captain, operator, gardenio-admin)",
	Long: `Rebuild the deployment rows of every release of --project from the rows they have.
Rows of one release with the same target and revision are one roll: they collapse
into the earliest. Each target's rows are then chained in since order, every row
closed at the next one's since and the newest left running, and each changed
release's status is recomputed. A revision recorded on two releases is listed as a
conflict and left alone. History that is already clean comes out unchanged, so a
second run writes nothing. --dry-run prints the changes and writes nothing.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		project, _ := cmd.Flags().GetString("project")
		dryRun, _ := cmd.Flags().GetBool("dry-run")
		if project == "" {
			return fmt.Errorf("--project is required")
		}
		res, err := beadsClient.RepairReleaseDeployments(cmd.Context(), project, dryRun)
		if err != nil {
			return err
		}
		if jsonOutput {
			emitJSON(res)
			return nil
		}
		printReleaseRepair(res)
		return nil
	},
}

// printReleaseRepair prints each release a repair changed, as its rows after.
func printReleaseRepair(res *client.RepairDeploymentsResult) {
	verb := "repaired"
	if res.DryRun {
		verb = "would repair (dry run, nothing written)"
	}
	fmt.Printf("%s: %d releases %s\n", res.Project, len(res.Releases), verb)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, r := range res.Releases {
		fmt.Fprintf(w, "%s\t%s -> %s\t%d rows -> %d (%d duplicates dropped)\t%s\n",
			r.Tag, r.StatusBefore, r.Status, len(r.Before), len(r.Deployments), len(r.Removed), r.ID)
		for _, d := range r.Deployments {
			until := d.Until
			if until == "" {
				until = "(running)"
			}
			fmt.Fprintf(w, "  %s\trev %s\t%s\t%s\t%s\n", d.Target, dashIfEmpty(d.Revision), d.Since, until, dashIfEmpty(d.By))
		}
	}
	w.Flush()
	for _, c := range res.Conflicts {
		fmt.Printf("conflict (left alone): %s\n", c)
	}
}

// releaseRunsOn is the targets a release runs on now (open rows).
func releaseRunsOn(r client.Release) []string {
	var out []string
	for _, d := range r.Deployments {
		if d.Until == "" {
			out = append(out, d.Target)
		}
	}
	return out
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// releaseStatusWord is a list row's status: the derived status, with
// "(skipped)" when a newer release went out first.
func releaseStatusWord(r client.Release) string {
	if r.SkippedBy != "" {
		return r.Status + " (skipped)"
	}
	return r.Status
}

// printRelease prints one release the way kd show prints a bead.
func printRelease(r *client.Release) {
	fmt.Printf("ID:          %s\n", r.ID)
	tag := r.Tag
	if tag == "" {
		tag = "(next, not cut yet)"
	}
	fmt.Printf("Release:     %s %s\n", r.Project, tag)
	fmt.Printf("Status:      %s\n", r.Status)
	if r.SkippedBy != "" {
		fmt.Printf("Skipped:     never deployed; %s went out first\n", r.SkippedBy)
	}
	if r.OpenedAt != "" {
		fmt.Printf("Opened:      %s by %s\n", r.OpenedAt, dashIfEmpty(r.OpenedBy))
	}
	fmt.Printf("Commit:      %s\n", dashIfEmpty(r.SHA))
	prev := dashIfEmpty(r.PreviousTag)
	if r.Previous != "" {
		prev += " (" + r.Previous + ")"
	}
	fmt.Printf("Previous:    %s\n", prev)
	fmt.Printf("Cut:         %s by %s (%s)\n", dashIfEmpty(r.CutAt), dashIfEmpty(r.CutBy), dashIfEmpty(r.Source))
	fmt.Printf("Built:       %s\n", dashIfEmpty(r.BuiltAt))
	if n := releaseRowCount(r.Contents); n >= 0 {
		complete := ""
		if r.ContentsComplete != nil && !*r.ContentsComplete {
			complete = " (incomplete)"
		}
		fmt.Printf("Contents:    %d PRs%s\n", n, complete)
	}
	if n := releaseRowCount(r.Images); n >= 0 {
		fmt.Printf("Images:      %d\n", n)
	}
	if len(r.Deployments) == 0 {
		fmt.Println("Deployments: none")
		return
	}
	fmt.Println("Deployments:")
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "  TARGET\tKIND\tSINCE\tUNTIL\tBY\tVIA\tREVISION\tIMAGE")
	for _, d := range r.Deployments {
		until := d.Until
		if until == "" {
			until = "(running)"
		}
		fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", d.Target, dashIfEmpty(d.Kind), d.Since, until,
			dashIfEmpty(d.By), dashIfEmpty(d.Via), dashIfEmpty(d.Revision), dashIfEmpty(d.ImageTag))
	}
	w.Flush()
}

// releaseRowCount is the length of a JSON list, -1 when absent or not a list.
func releaseRowCount(raw json.RawMessage) int {
	if len(raw) == 0 {
		return -1
	}
	var rows []json.RawMessage
	if json.Unmarshal(raw, &rows) != nil {
		return -1
	}
	return len(rows)
}

// Flag sets, shared by init and the command tree the tests build.

func addReleaseListFlags(c *cobra.Command) {
	c.Flags().String("project", "", "only this project's releases")
	c.Flags().String("status", "", "comma-separated statuses: open, cut, built, live, superseded")
	c.Flags().Int("limit", 0, "maximum releases (daemon default 50)")
}

func addReleaseShowFlags(c *cobra.Command) {
	c.Flags().String("project", "", "the project, when the argument is a tag")
}

func addReleaseCreateFlags(c *cobra.Command) {
	c.Flags().String("project", "", "project name (required)")
	c.Flags().String("tag", "", "the release tag; must match the project's release_tag_pattern (required)")
	c.Flags().String("sha", "", "the tag's commit")
	c.Flags().String("previous", "", "the previous release's bead id")
	c.Flags().String("previous-tag", "", "the previous release's tag")
	c.Flags().String("cut-at", "", "when the tag was cut, RFC 3339 (default now)")
	c.Flags().String("cut-by", "", "who cut it (default you)")
	c.Flags().String("source", "", "webhook, script, backfill or manual (default manual)")
	c.Flags().String("built-at", "", "when every artifact existed, RFC 3339")
	c.Flags().String("contents", "", "JSON list of PR rows (@file reads a file)")
	c.Flags().String("images", "", "JSON list of {name, ref} (@file reads a file)")
	c.Flags().Bool("contents-complete", true, "false when the contents were truncated")
}

func addReleaseDeployFlags(c *cobra.Command) {
	c.Flags().String("project", "", "the project, when the argument is a tag")
	c.Flags().String("target", "", "one of the project's release_targets (required)")
	c.Flags().String("kind", "", "target kind (default: the target's declared kind)")
	c.Flags().String("since", "", "when it started running, RFC 3339 (default now)")
	c.Flags().String("by", "", "who deployed it (default you)")
	c.Flags().String("via", "", "script, contract, github or reconciler (default script)")
	c.Flags().String("revision", "", "the Helm revision or the forge's deployment id")
	c.Flags().String("image-tag", "", "the image tag rolled out (default the release tag)")
}

func addReleaseRepairFlags(c *cobra.Command) {
	c.Flags().String("project", "", "the project whose deployments to repair (required)")
	c.Flags().Bool("dry-run", false, "print what would change; write nothing")
}

func init() {
	addReleaseListFlags(releaseListCmd)
	addReleaseShowFlags(releaseShowCmd)
	addReleaseCreateFlags(releaseCreateCmd)
	addReleaseDeployFlags(releaseDeployCmd)
	addReleaseRepairFlags(releaseRepairDeploymentsCmd)

	releaseCmd.AddCommand(releaseListCmd)
	releaseCmd.AddCommand(releaseShowCmd)
	releaseCmd.AddCommand(releaseCreateCmd)
	releaseCmd.AddCommand(releaseOpenCmd)
	releaseCmd.AddCommand(releaseDeployCmd)
	releaseCmd.AddCommand(releaseRepairDeploymentsCmd)
}
