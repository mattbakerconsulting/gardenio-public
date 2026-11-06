package main

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"gardenio/lands/contract/landsv1"

	"github.com/spf13/cobra"
)

// A land's releases: one per tag of its repository, recorded when the tag is
// pushed (releases plan gd-UZKZ9H25UD step 6). gl reads them and reports
// where one runs; it never writes a release.

func newReleaseCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "release",
		Aliases: []string{"releases"},
		Short:   "The land's releases: list, show, deployed",
	}
	cmd.AddCommand(releaseList(a), releaseShow(a), releaseDeployed(a))
	return cmd
}

func releaseList(a *app) *cobra.Command {
	var pg paging
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List releases, newest first",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			l, err := a.connect()
			if err != nil {
				return err
			}
			r, err := l.c.ListReleasesWithResponse(cmd.Context(), l.land, &landsv1.ListReleasesParams{Cursor: pg.cursorPtr(), Limit: pg.limitPtr()})
			if err != nil {
				return err
			}
			if r.JSON200 == nil {
				return l.fail(r.StatusCode(), r.ApplicationproblemJSONDefault)
			}
			pg := r.JSON200
			err = a.emit(pg, func(w io.Writer) {
				if len(pg.Items) == 0 {
					fmt.Fprintln(w, "No releases.")
					return
				}
				tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "TAG\tSTATUS\tCUT\tPRS\tRUNS ON")
				for _, rel := range pg.Items {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n", rel.Tag, rel.Status, cutAt(rel.CutAt), len(rel.Contents), runsOn(rel.Deployments))
				}
				tw.Flush()
			})
			a.moreHint(pg.NextCursor)
			return err
		},
	}
	pg.flags(cmd)
	return cmd
}

func releaseShow(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "show <tag>",
		Short: "Show a release: what it carries and where it runs",
		Long:  "Show a release: what it carries and where it runs. Name it by its tag or its id.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			l, err := a.connect()
			if err != nil {
				return err
			}
			r, err := l.c.GetReleaseWithResponse(cmd.Context(), l.land, args[0])
			if err != nil {
				return err
			}
			if r.JSON200 == nil {
				return l.fail(r.StatusCode(), r.ApplicationproblemJSONDefault)
			}
			return a.release(r.JSON200)
		},
	}
}

func releaseDeployed(a *app) *cobra.Command {
	var target, revision, by string
	cmd := &cobra.Command{
		Use:   "deployed <tag> --target <name>",
		Short: "Report that a release now runs on one of the land's targets",
		Long: `Report that a release now runs on one of the land's targets (prod, unless
the land declares others). Only the land's owner may; the release that ran
there before is closed on that target. Reporting the same rollout again
changes nothing.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			l, err := a.connect()
			if err != nil {
				return err
			}
			b := landsv1.DeploymentReport{Target: target}
			if revision != "" {
				b.Revision = &revision
			}
			if by != "" {
				b.By = &by
			}
			r, err := l.c.ReportDeploymentWithResponse(cmd.Context(), l.land, args[0], &landsv1.ReportDeploymentParams{IdempotencyKey: newKey()}, b)
			if err != nil {
				return err
			}
			if r.JSON200 == nil {
				return l.fail(r.StatusCode(), r.ApplicationproblemJSONDefault)
			}
			return a.release(r.JSON200)
		},
	}
	cmd.Flags().StringVar(&target, "target", "prod", "where it runs: one of the land's targets")
	cmd.Flags().StringVar(&revision, "revision", "", "the target's own revision of the rollout (a Helm revision, a deployment id)")
	cmd.Flags().StringVar(&by, "by", "", "who or what rolled it out, such as a CI job (default: you)")
	return cmd
}

func (a *app) release(rel *landsv1.Release) error {
	return a.emit(rel, func(w io.Writer) {
		fmt.Fprintf(w, "%s  %s\n", rel.Tag, rel.Id)
		fmt.Fprintf(w, "Status:  %s\n", rel.Status)
		if rel.Sha != nil {
			fmt.Fprintf(w, "Commit:  %s\n", *rel.Sha)
		}
		if rel.CutAt != nil {
			fmt.Fprintf(w, "Cut:     %s\n", stamp(*rel.CutAt))
		}
		fmt.Fprintf(w, "\nCarries %d pull request%s:\n", len(rel.Contents), plural(len(rel.Contents)))
		if len(rel.Contents) > 0 {
			tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
			for _, it := range rel.Contents {
				fmt.Fprintf(tw, "  #%d\t%s\t%s\n", it.Pr, oneLine(it.Title), planOf(it))
			}
			tw.Flush()
		}
		if len(rel.Deployments) == 0 {
			fmt.Fprintln(w, "\nNo deployments yet.")
			return
		}
		fmt.Fprintln(w, "\nDeployments:")
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, d := range rel.Deployments {
			until := "now"
			if d.Until != nil {
				until = stamp(*d.Until)
			}
			fmt.Fprintf(tw, "  %s\t%s\tto %s\t%s\n", d.Target, stamp(d.Since), until, deployedBy(d))
		}
		tw.Flush()
	})
}

// runsOn is the targets a release runs on now, "-" for none.
func runsOn(ds []landsv1.Deployment) string {
	var on []string
	for _, d := range ds {
		if d.Until == nil {
			on = append(on, d.Target)
		}
	}
	if len(on) == 0 {
		return "-"
	}
	return strings.Join(on, ",")
}

func cutAt(t *landsv1.Timestamp) string {
	if t == nil {
		return "-"
	}
	return stamp(*t)
}

func planOf(it landsv1.ReleaseItem) string {
	switch {
	case it.Plan != nil && it.Step != nil:
		return "plan " + *it.Plan + ", step " + *it.Step
	case it.Plan != nil:
		return "plan " + *it.Plan
	case it.Step != nil:
		return "step " + *it.Step
	}
	return "unplanned"
}

func deployedBy(d landsv1.Deployment) string {
	var parts []string
	if d.Revision != nil {
		parts = append(parts, "revision "+*d.Revision)
	}
	if d.By != nil {
		parts = append(parts, "by "+*d.By)
	}
	return strings.Join(parts, ", ")
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
