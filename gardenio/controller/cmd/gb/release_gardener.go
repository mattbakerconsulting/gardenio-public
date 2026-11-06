package main

// gb release gardener: an open release's own gardener (plan gd-lgN4U7X5Tb,
// step gd-BS9fpWa1bx; internal/releasegardener). gb release open and
// gb release record bind it too, on the release they open.

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/releasegardener"
	"gardenio/controller/internal/releaserecord"
)

var (
	releaseGardenerRelease string
	releaseGardenerPaused  bool
	releaseNoGardener      bool
)

var releaseGardenerCmd = &cobra.Command{
	Use:   "gardener [project]",
	Short: "Give a project's open release its own gardener (a no-op when it has one)",
	Long: `Bind the release's gardener: a trigger that spawns the release-captain role
in the cut windows and on the release's own changes, to cut it, mark it built,
watch its deployment and retire once it is live or superseded. Its playbook is
the advice bead labelled ` + releasegardener.PlaybookLabel + `. A release has at
most one gardener; when it has one, that one is printed and nothing is written
but the release's gardener_id. gb release open and gb release record bind it on
the release they open, so this is for a release opened without one.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runReleaseGardener,
}

func init() {
	releaseGardenerCmd.Flags().StringVar(&releaseRecordProject, "project", "", "project (default: the argument, then $KD_PROJECT, then $BOAT_PROJECT)")
	releaseGardenerCmd.Flags().StringVar(&releaseGardenerRelease, "release", "", "the release's bead id (default: the project's open release)")
	releaseGardenerCmd.Flags().BoolVar(&releaseGardenerPaused, "paused", false, "bind it disabled; an admin's enable starts it")
	releaseOpenCmd.Flags().BoolVar(&releaseNoGardener, "no-gardener", false, "open the release without binding its gardener")
	releaseRecordCmd.Flags().BoolVar(&releaseNoGardener, "no-gardener", false, "do not bind the gardener of the release the cut opens")
	releaseCmd.AddCommand(releaseGardenerCmd)
}

func runReleaseGardener(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	var rel *beadsapi.Release
	if id := strings.TrimSpace(releaseGardenerRelease); id != "" {
		var err error
		if rel, err = daemon.GetRelease(ctx, id, ""); err != nil {
			return fmt.Errorf("reading release %s: %w", id, err)
		}
	} else {
		project, err := releaseOpenProject(args)
		if err != nil {
			return err
		}
		if rel, err = releaserecord.FindOpen(ctx, daemon, project); err != nil {
			return err
		}
		if rel == nil {
			return fmt.Errorf("%s has no open release; gb release open %s opens one and binds its gardener", project, project)
		}
	}
	res, err := bindReleaseGardener(cmd, rel)
	if jsonOutput && err == nil {
		printJSON(map[string]any{"release": rel.ID, "gardener": res})
	}
	return err
}

// bindReleaseGardener ensures rel's gardener and, outside --json, says so.
func bindReleaseGardener(cmd *cobra.Command, rel *beadsapi.Release) (*releasegardener.Result, error) {
	res, err := releasegardener.Ensure(cmd.Context(), daemon, rel, releasegardener.Spec{
		CreatedBy: releaseActor(),
		Paused:    releaseGardenerPaused,
	})
	if err != nil {
		return res, fmt.Errorf("binding release %s's gardener: %w", rel.ID, err)
	}
	if !jsonOutput {
		verb := "bound"
		if res.Adopted {
			verb = "already has"
		}
		fmt.Fprintf(cmd.OutOrStdout(), "release %s %s gardener %s\n", rel.ID, verb, res.ID)
	}
	return res, nil
}
