package main

// gb release record | built | backfill — write a project's release record
// (gardenio/docs/releases-design.md §3-4, plan gd-UZKZ9H25UD step 3). The
// record itself is built by internal/releaserecord; these verbs pick its
// source and print what it did.
//
//   record <tag>   after a cut: the release bead with its contents (the PRs
//                  between the previous release tag and this one, joined to
//                  their MR beads, steps and plans) and a "shipped in" comment
//                  on each plan. The tag is cut onto the project's open
//                  release when it has one, and the next release is opened
//                  (plan gd-lgN4U7X5Tb). release-cut.sh calls it after the push.
//   open [project] the project's next release, opened ahead of its cut.
//   next [project] the open release and what it carries so far, by plan.
//   built <tag>    every image exists: built_at (and images) on the release,
//                  status cut -> built. Probes the tag's images.yml run unless
//                  --at is given.
//   backfill       the last 30 release tags (or --since <tag>), oldest first,
//                  so the surfaces have history on day one.
//   backfill-deployments
//                  where each release ran (step 4): the release-deployer's
//                  "deployed and verified" event beads replayed as deployments
//                  on the target, oldest first.
//
// Every write is idempotent on (project, tag): a second run writes nothing.
// The daemon lets only the release captain, the operator, gardenio-admin and
// a machine principal with a release grant write; --dry-run reads only and is
// allowed for anyone.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"gardenio/controller/internal/releasegardener"
	"gardenio/controller/internal/releaserecord"
)

var (
	releaseRecordProject   string
	releaseRecordRepoDir   string
	releaseRecordSource    string
	releaseRecordDryRun    bool
	releaseRecordNoComment bool
	releaseRecordNoLookup  bool
	releaseBuiltAt         string
	releaseBuiltWorkflow   string
	releaseBuiltImagesFile string
	releaseBuiltRegistry   string
	releaseBuiltFromTag    bool
	releaseBackfillSince   string
	releaseBackfillLast    int
	releaseBackfillBuilt   bool
)

var releaseRecordCmd = &cobra.Command{
	Use:   "record <tag>",
	Short: "Write the release record of a tag: contents joined to MR beads, steps and plans",
	Long: `Write the type=release bead for <tag> of a project: its commit, the
previous release tag, the cut time and tagger, and the contents (each merged
PR between the previous release tag and this one on the first-parent line,
joined to its merge-request bead, task, plan step and plan). Each plan whose
steps shipped gets a "Shipped in <project> <tag>" comment.

History is read from the local checkout (--source git, the default; fetch the
tags first) or the GitHub API (--source github, through gh). A commit whose
subject names no PR or bead is looked up with GitHub's commits/{sha}/pulls.

When the project has an open release (its next release, opened before the
cut) and the tag is above every recorded release, the tag is cut onto that
bead in one write instead of minting a new one. Its contents are reconciled
against git: the git rows win, and a row a merge added that git does not show
between the two tags is dropped and listed. Either way the project's next
release is then opened.

Idempotent: a release already recorded only gets the fields it lacks
(contents, previous_tag, sha), a plan already commented is skipped, and a
project has one open release. --dry-run prints the record and writes nothing;
any role may run it.`,
	Args: cobra.ExactArgs(1),
	RunE: runReleaseRecord,
}

var releaseBuiltCmd = &cobra.Command{
	Use:   "built <tag>",
	Short: "Mark a release built (every image pushed): built_at and images",
	Long: `Set built_at on the release of <tag>, moving it from cut to built. Without
--at the time is the finish of the tag's successful images.yml push run (the
same test release-deploy.sh's images-ready gate makes); no such run yet exits
3 and writes nothing. --images-file .github/images.json with --registry
records one image ref per entry; with --images-from-tag the file is read at
<tag> in --repo-dir, so the refs are the images that tag built. A release
already built is left alone.`,
	Args: cobra.ExactArgs(1),
	RunE: runReleaseBuilt,
}

var releaseBackfillCmd = &cobra.Command{
	Use:   "backfill",
	Short: "Write the release records of a project's recent tags (history for the surfaces)",
	Long: `Record each release tag of the project, oldest first: the newest --last
(default 30), or every tag from --since <tag> on. Each is written as
'gb release record' writes it, with source=backfill and no plan comments. A
tag already recorded with its contents is skipped, so a second run writes
nothing. --built also marks each release built from its images.yml run; a
tag with no successful run yet (the one just cut, or a red tag) is left cut
and the next backfill marks it.`,
	Args: cobra.NoArgs,
	RunE: runReleaseBackfill,
}

var releaseBackfillDeploymentsCmd = &cobra.Command{
	Use:   "backfill-deployments",
	Short: "Record where past releases ran, from the release-deployer's event beads",
	Long: `Replay each "release-deploy <tag>: deployed and verified" event bead of the
project (closed, labelled release-deploy) as a deployment of that release on
--target, oldest first, at the event's time and the helm revision its
description names ("helm revision after: N", or an operator's "helm rev N").
A deployment older than the target's newest goes into the history closed, so
the release running there now keeps its open row. A roll reported twice is
written once, and the daemon records a helm revision once per target, so a
late report of a recorded roll changes nothing and a revision another release
holds is skipped; a tag with no recorded release is skipped (run gb release
backfill first). A second run writes nothing. Failed rolls and rollbacks left
no tag in their event, so the controller's release reconciler records what
runs now. --dry-run lists the events and writes nothing.`,
	Args: cobra.NoArgs,
	RunE: runReleaseBackfillDeployments,
}

var releaseOpenCmd = &cobra.Command{
	Use:   "open [project]",
	Short: "Open a project's next release ahead of its cut (a no-op when it has one)",
	Long: `Open the project's next release: a release bead with no tag, which merged
PRs board and the next cut writes its tag onto. A project has at most one; when
it already has one, that release is printed and nothing is written. gb release
record opens the next one after every cut, so this is for a project's first.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runReleaseOpen,
}

var releaseNextCmd = &cobra.Command{
	Use:   "next [project]",
	Short: "Print a project's open release and what it carries so far, by plan",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runReleaseNext,
}

var (
	releaseDeployTarget string
	releaseDeploySince  string
)

func init() {
	releaseBackfillDeploymentsCmd.Flags().StringVar(&releaseRecordProject, "project", "", "project whose deployments these are (default: $KD_PROJECT, then $BOAT_PROJECT)")
	releaseBackfillDeploymentsCmd.Flags().BoolVar(&releaseRecordDryRun, "dry-run", false, "print what would be written; write nothing")
	releaseBackfillDeploymentsCmd.Flags().StringVar(&releaseDeployTarget, "target", "prod", "the release target the rolls went to")
	releaseBackfillDeploymentsCmd.Flags().StringVar(&releaseDeploySince, "since", "", "skip events before this time (RFC 3339)")
	for _, c := range []*cobra.Command{releaseRecordCmd, releaseBuiltCmd, releaseBackfillCmd} {
		c.Flags().StringVar(&releaseRecordProject, "project", "", "project whose release this is (default: $KD_PROJECT, then $BOAT_PROJECT)")
		c.Flags().BoolVar(&releaseRecordDryRun, "dry-run", false, "print what would be written; write nothing")
	}
	for _, c := range []*cobra.Command{releaseRecordCmd, releaseBuiltCmd, releaseBackfillCmd} {
		c.Flags().StringVar(&releaseRecordRepoDir, "repo-dir", "", "checkout to read tags and history from (default: the working directory)")
	}
	for _, c := range []*cobra.Command{releaseRecordCmd, releaseBackfillCmd} {
		c.Flags().StringVar(&releaseRecordSource, "source", "git", "where history is read: git (the checkout) or github (the API, through gh)")
		c.Flags().BoolVar(&releaseRecordNoLookup, "no-pr-lookup", false, "do not ask GitHub for the PR of a commit whose subject names none")
	}
	releaseRecordCmd.Flags().BoolVar(&releaseRecordNoComment, "no-comment", false, "do not comment \"shipped in\" on the plans")
	for _, c := range []*cobra.Command{releaseBuiltCmd, releaseBackfillCmd} {
		c.Flags().StringVar(&releaseBuiltWorkflow, "workflow", "images.yml", "the workflow whose successful tag run means built")
		c.Flags().StringVar(&releaseBuiltImagesFile, "images-file", "", "JSON list of {\"image\": name} (.github/images.json); records <registry>/<name>:<tag> per entry")
		c.Flags().StringVar(&releaseBuiltRegistry, "registry", "", "image registry prefix for --images-file")
		c.Flags().BoolVar(&releaseBuiltFromTag, "images-from-tag", false, "read --images-file (a repo path) at each tag in --repo-dir, not from the working tree")
	}
	releaseBuiltCmd.Flags().StringVar(&releaseBuiltAt, "at", "", "built time (RFC 3339); default: the images run's finish")
	releaseBackfillCmd.Flags().StringVar(&releaseBackfillSince, "since", "", "first tag to record; every later release tag is recorded too")
	releaseBackfillCmd.Flags().IntVar(&releaseBackfillLast, "last", releaserecord.DefaultBackfill, "how many of the newest release tags to record when --since is not given")
	releaseBackfillCmd.Flags().BoolVar(&releaseBackfillBuilt, "built", false, "also mark each release built from its images.yml run")
	for _, c := range []*cobra.Command{releaseOpenCmd, releaseNextCmd} {
		c.Flags().StringVar(&releaseRecordProject, "project", "", "project (default: the argument, then $KD_PROJECT, then $BOAT_PROJECT)")
	}
	releaseCmd.AddCommand(releaseRecordCmd, releaseBuiltCmd, releaseBackfillCmd, releaseBackfillDeploymentsCmd, releaseOpenCmd, releaseNextCmd)
}

// releaseProjectName is --project, else $KD_PROJECT, else $BOAT_PROJECT.
func releaseProjectName() (string, error) {
	for _, p := range []string{releaseRecordProject, os.Getenv("KD_PROJECT"), os.Getenv("BOAT_PROJECT")} {
		if p = strings.TrimSpace(p); p != "" {
			return p, nil
		}
	}
	return "", errors.New("no project: pass --project <name> (or set KD_PROJECT)")
}

// newReleaseRecorder builds the recorder for project: the history source
// from --source, and GitHub's PR lookup when gh is on PATH.
func newReleaseRecorder(ctx context.Context, project string) (*releaserecord.Recorder, error) {
	r := &releaserecord.Recorder{Daemon: daemon, Author: releaseActor()}
	if err := r.Load(ctx, project); err != nil {
		return nil, err
	}
	gh, err := releaseGitHub(ctx, project)
	if err != nil && releaseRecordSource == "github" {
		return nil, err
	}
	switch releaseRecordSource {
	case "git":
		r.Source = releaserecord.GitSource{Dir: releaseRecordRepoDir}
	case "github":
		r.Source = gh
	default:
		return nil, fmt.Errorf("--source %q: want git or github", releaseRecordSource)
	}
	if !releaseRecordNoLookup && gh.Repo != "" {
		if _, lookErr := exec.LookPath("gh"); lookErr == nil {
			r.PRs = gh
		}
	}
	return r, nil
}

// releaseGitHub is the GitHub reader for project's repo, from its git_url.
func releaseGitHub(ctx context.Context, project string) (releaserecord.GitHub, error) {
	info, err := daemon.GetProjectBead(ctx, project)
	if err != nil {
		return releaserecord.GitHub{}, fmt.Errorf("reading project %s: %w", project, err)
	}
	repo := releaserecord.RepoFullName(releaserecord.RepoWebURL(info.GitURL))
	if repo == "" {
		return releaserecord.GitHub{}, fmt.Errorf("project %s has no GitHub git_url (%q)", project, info.GitURL)
	}
	return releaserecord.GitHub{API: releaserecord.GhCLI{}, Repo: repo}, nil
}

// releaseActor names the writer for the plan comments: the agent, else the
// user.
func releaseActor() string {
	for _, v := range []string{os.Getenv("BOAT_AGENT"), os.Getenv("USER")} {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return "gb release record"
}

func runReleaseRecord(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	project, err := releaseProjectName()
	if err != nil {
		return err
	}
	r, err := newReleaseRecorder(ctx, project)
	if err != nil {
		return err
	}
	r.CommentPlans = !releaseRecordNoComment
	r.CutOpen = true
	rec, err := r.Plan(ctx, project, args[0], releaserecord.SourceScript)
	if err != nil {
		return err
	}
	if releaseRecordDryRun {
		printReleaseRecord(cmd, rec, nil)
		return nil
	}
	res, err := r.Write(ctx, rec)
	if err != nil {
		return err
	}
	printReleaseRecord(cmd, rec, res)
	if res.Next == nil || releaseNoGardener {
		return nil
	}
	// The record is written; a gardener that would not bind is a warning, so
	// release-cut.sh does not read the cut's record as failed.
	if _, err := bindReleaseGardener(cmd, res.Next); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: %v; run gb release gardener %s\n", err, rec.Project)
	}
	return nil
}

func runReleaseBackfill(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	project, err := releaseProjectName()
	if err != nil {
		return err
	}
	r, err := newReleaseRecorder(ctx, project)
	if err != nil {
		return err
	}
	tags, err := r.BackfillTags(ctx, project, releaseBackfillSince, releaseBackfillLast)
	if err != nil {
		return err
	}
	items, err := r.Backfill(ctx, project, tags, !releaseRecordDryRun)
	if !jsonOutput {
		for _, it := range items {
			printReleaseRecord(cmd, it.Record, it.Result)
		}
	}
	if err != nil {
		return err
	}
	var built []string
	if releaseBackfillBuilt {
		for _, it := range items {
			ok, bErr := markReleaseBuilt(cmd, project, it.Record.Tag.Name, it.Record.Tag.SHA, "")
			if errors.Is(bErr, errNotBuilt) {
				continue
			}
			if bErr != nil {
				return bErr
			}
			if ok {
				built = append(built, it.Record.Tag.Name)
			}
		}
	}
	if jsonOutput {
		printJSON(map[string]any{"project": project, "dry_run": releaseRecordDryRun, "items": items, "built": built})
		return nil
	}
	written := 0
	for _, it := range items {
		if it.Result != nil && (it.Result.Created || len(it.Result.Filled) > 0) {
			written++
		}
	}
	verb := "wrote"
	if releaseRecordDryRun {
		verb, written = "would write (dry run, nothing written)", len(items)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "backfill %s: %d of %d release tags %s; %d marked built\n", project, written, len(items), verb, len(built))
	return nil
}

func runReleaseBackfillDeployments(cmd *cobra.Command, _ []string) error {
	project, err := releaseProjectName()
	if err != nil {
		return err
	}
	opts := releaserecord.DeployBackfillOptions{Project: project, Target: releaseDeployTarget, Write: !releaseRecordDryRun}
	if releaseDeploySince != "" {
		if opts.Since, err = time.Parse(time.RFC3339, releaseDeploySince); err != nil {
			return fmt.Errorf("--since %q is not an RFC 3339 time", releaseDeploySince)
		}
	}
	items, err := releaserecord.BackfillDeployments(cmd.Context(), daemon, opts)
	if jsonOutput {
		printJSON(map[string]any{"project": project, "target": opts.Target, "dry_run": releaseRecordDryRun, "items": items})
		return err
	}
	out := cmd.OutOrStdout()
	recorded := 0
	for _, it := range items {
		state := "would record"
		switch {
		case it.Recorded:
			state, recorded = "recorded", recorded+1
		case it.Skip != "":
			state = "skipped: " + it.Skip
		}
		fmt.Fprintf(out, "%s  %-12s rev %-5s %s  (%s)\n", it.At, it.Tag, dashIfBlank(it.Revision), state, it.Bead)
	}
	if err != nil {
		return err
	}
	if releaseRecordDryRun {
		fmt.Fprintf(out, "backfill-deployments %s: %d deploy events on %s (dry run, nothing written)\n", project, len(items), opts.Target)
		return nil
	}
	fmt.Fprintf(out, "backfill-deployments %s: recorded %d of %d deploy events on %s\n", project, recorded, len(items), opts.Target)
	return nil
}

// errNotBuilt is runReleaseBuilt's exit-3 case: no successful images run yet.
var errNotBuilt = errors.New("not built yet")

func runReleaseBuilt(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	project, err := releaseProjectName()
	if err != nil {
		return err
	}
	rel, err := daemon.GetRelease(ctx, args[0], project)
	if err != nil {
		return err
	}
	ok, err := markReleaseBuilt(cmd, project, rel.Tag, rel.SHA, releaseBuiltAt)
	if errors.Is(err, errNotBuilt) {
		fmt.Fprintf(cmd.ErrOrStderr(), "%s %s: no successful %s push run at %s yet; nothing written\n", project, rel.Tag, releaseBuiltWorkflow, rel.SHA)
		os.Exit(3)
	}
	if err != nil {
		return err
	}
	if !ok {
		fmt.Fprintf(cmd.OutOrStdout(), "%s %s: already built; nothing written\n", project, rel.Tag)
	}
	return nil
}

// markReleaseBuilt sets built_at on the release of tag: at, else the finish
// of the tag's successful workflow run (errNotBuilt when there is none). It
// reports whether it wrote (or, on a dry run, would write).
func markReleaseBuilt(cmd *cobra.Command, project, tag, sha, at string) (bool, error) {
	ctx := cmd.Context()
	rel, err := daemon.GetRelease(ctx, tag, project)
	if err != nil {
		return false, err
	}
	if rel.BuiltAt != "" {
		return false, nil
	}
	if at == "" {
		gh, err := releaseGitHub(ctx, project)
		if err != nil {
			return false, err
		}
		if sha == "" {
			sha = rel.SHA
		}
		if at, err = gh.BuiltAt(ctx, releaseBuiltWorkflow, tag, sha); err != nil {
			return false, err
		}
		if at == "" {
			return false, errNotBuilt
		}
	}
	images, err := releaseImages(tag)
	if err != nil {
		return false, err
	}
	if releaseRecordDryRun {
		fmt.Fprintf(cmd.OutOrStdout(), "%s %s: would mark built at %s with %d images (dry run)\n", project, tag, at, len(images))
		return true, nil
	}
	_, wrote, err := releaserecord.MarkBuilt(ctx, daemon, project, tag, at, images)
	if err == nil && wrote && !jsonOutput {
		fmt.Fprintf(cmd.OutOrStdout(), "%s %s: built at %s (%d images)\n", project, tag, at, len(images))
	}
	return wrote, err
}

// releaseImages reads --images-file into one ref per image.
func releaseImages(tag string) ([]releaserecord.Image, error) {
	if releaseBuiltImagesFile == "" {
		return nil, nil
	}
	if releaseBuiltRegistry == "" {
		return nil, errors.New("--images-file needs --registry")
	}
	raw, err := readImagesFile(tag)
	if err != nil {
		return nil, err
	}
	var entries []struct {
		Image string `json:"image"`
	}
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("%s: %w", releaseBuiltImagesFile, err)
	}
	var out []releaserecord.Image
	for _, e := range entries {
		if e.Image != "" {
			out = append(out, releaserecord.Image{Name: e.Image, Ref: strings.TrimSuffix(releaseBuiltRegistry, "/") + "/" + e.Image + ":" + tag})
		}
	}
	return out, nil
}

// readImagesFile is --images-file: the file on disk, or with
// --images-from-tag the file as committed at tag in --repo-dir.
func readImagesFile(tag string) ([]byte, error) {
	if !releaseBuiltFromTag {
		return os.ReadFile(releaseBuiltImagesFile)
	}
	args := []string{"show", tag + ":" + strings.TrimPrefix(releaseBuiltImagesFile, "./")}
	if releaseRecordRepoDir != "" {
		args = append([]string{"-C", releaseRecordRepoDir}, args...)
	}
	raw, err := exec.Command("git", args...).Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return raw, nil
}

// printReleaseRecord prints one record: what it carries, grouped by plan, and
// what the write did (res nil on a dry run).
func printReleaseRecord(cmd *cobra.Command, rec *releaserecord.Record, res *releaserecord.Result) {
	if jsonOutput {
		printJSON(map[string]any{"record": rec, "result": res})
		return
	}
	w := cmd.OutOrStdout()
	prev := rec.Previous
	if prev == "" {
		prev = "(first release)"
	}
	complete := ""
	if !rec.Complete {
		complete = ", truncated"
	}
	fmt.Fprintf(w, "%s %s at %.12s, previous %s: %d commits%s\n", rec.Project, rec.Tag.Name, rec.Tag.SHA, prev, len(rec.Rows), complete)
	printReleaseRows(w, rec.Rows)
	if rec.Open != nil {
		fmt.Fprintf(w, "  cut onto the open release %s\n", rec.Open.ID)
		if len(rec.Dropped) > 0 {
			fmt.Fprintf(w, "  dropped %d row(s) git does not show: %s\n", len(rec.Dropped), releaserecord.DroppedSummary(rec.Dropped))
		}
	}
	switch {
	case res == nil:
		fmt.Fprintln(w, "  (dry run: nothing written)")
	case res.CutOpen:
		fmt.Fprintf(w, "  recorded as %s (status %s)\n", res.Release.ID, res.Release.Status)
	case res.Created:
		fmt.Fprintf(w, "  recorded as %s (status %s)\n", res.Release.ID, res.Release.Status)
	case len(res.Filled) > 0:
		fmt.Fprintf(w, "  %s already recorded; filled %s\n", res.Release.ID, strings.Join(res.Filled, ", "))
	default:
		fmt.Fprintf(w, "  %s already recorded; nothing written\n", res.Release.ID)
	}
	if res != nil && len(res.Commented) > 0 {
		fmt.Fprintf(w, "  commented on plans %s\n", strings.Join(res.Commented, ", "))
	}
	if res != nil && res.Next != nil {
		verb := "is"
		if res.NextOpened {
			verb = "opened as"
		}
		fmt.Fprintf(w, "  next release %s %s\n", verb, res.Next.ID)
	}
}

// printReleaseRows prints contents rows grouped by plan, unplanned last.
func printReleaseRows(w io.Writer, rows []releaserecord.Row) {
	for _, g := range releaserecord.GroupByPlan(rows) {
		plan := g.Plan
		if plan == "" {
			plan = "unplanned"
		}
		fmt.Fprintf(w, "  %s\n", plan)
		for _, row := range g.Rows {
			ref := fmt.Sprintf("%.8s", row.MergeSHA)
			if row.PR != 0 {
				ref = fmt.Sprintf("#%d", row.PR)
			}
			via := row.Step
			if via == "" {
				via = row.Task
			}
			if via != "" {
				via = " [" + via + "]"
			}
			fmt.Fprintf(w, "    %-8s %s%s\n", ref, row.Title, via)
		}
	}
}

// releaseOpenProject is the project of gb release open / next: the argument,
// else releaseProjectName.
func releaseOpenProject(args []string) (string, error) {
	if len(args) == 1 && strings.TrimSpace(args[0]) != "" {
		return strings.TrimSpace(args[0]), nil
	}
	return releaseProjectName()
}

func runReleaseOpen(cmd *cobra.Command, args []string) error {
	project, err := releaseOpenProject(args)
	if err != nil {
		return err
	}
	rel, created, err := releaserecord.OpenNext(cmd.Context(), daemon, project)
	if err != nil {
		return err
	}
	if !jsonOutput {
		verb := "already open"
		if created {
			verb = "opened"
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s next release %s: %s\n", project, verb, rel.ID)
	}
	var gardener *releasegardener.Result
	if !releaseNoGardener {
		gardener, err = bindReleaseGardener(cmd, rel)
	}
	if jsonOutput {
		printJSON(map[string]any{"release": rel, "opened": created, "gardener": gardener})
	}
	return err
}

func runReleaseNext(cmd *cobra.Command, args []string) error {
	project, err := releaseOpenProject(args)
	if err != nil {
		return err
	}
	rel, err := releaserecord.FindOpen(cmd.Context(), daemon, project)
	if err != nil {
		return err
	}
	if jsonOutput {
		printJSON(map[string]any{"release": rel})
		return nil
	}
	w := cmd.OutOrStdout()
	if rel == nil {
		fmt.Fprintf(w, "%s has no open release; gb release open %s opens one\n", project, project)
		return nil
	}
	rows, bad := releaserecord.ParseRows(rel.Contents)
	fmt.Fprintf(w, "%s next release %s, opened %s by %s: %d PRs so far\n",
		project, rel.ID, rel.OpenedAt, rel.OpenedBy, len(rows))
	printReleaseRows(w, rows)
	if bad > 0 {
		fmt.Fprintf(w, "  (%d row(s) unreadable)\n", bad)
	}
	return nil
}
