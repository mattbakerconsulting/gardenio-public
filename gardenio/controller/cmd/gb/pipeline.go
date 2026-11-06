package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"gardenio/controller/internal/bridge"

	"github.com/spf13/cobra"
)

var pipelineCmd = &cobra.Command{
	Use:     "pipeline",
	Short:   "GitLab CI pipeline commands",
	GroupID: "orchestration",
}

var pipelineArtifactsCmd = &cobra.Command{
	Use:   "artifacts <bead-id-or-mr-url>",
	Short: "List or download pipeline artifacts from the MR's latest pipeline",
	Args:  cobra.ExactArgs(1),
	RunE:  runPipelineArtifacts,
}

// pipelineRetriggerCmd is kept only to refuse: PR CI is never retriggered
// (owner rule 2026-10-31, gd-lNeFr44JkM). It stays registered so an agent
// following old advice gets the reason instead of "unknown command".
var pipelineRetriggerCmd = &cobra.Command{
	Use:    "retrigger <bead-id-or-mr-url>",
	Short:  "Refuses: PR CI is never retriggered",
	Hidden: true,
	Args:   cobra.ArbitraryArgs,
	RunE: func(*cobra.Command, []string) error {
		return errPipelineRetriggerRefused
	},
}

var errPipelineRetriggerRefused = errors.New("gb pipeline retrigger is retired: PR CI is never retriggered, rerun or refreshed " +
	"(owner rule 2026-10-31, root CLAUDE.md \"CI on pull requests\"). Lost runners are re-run by retry-lost-runners.yml; " +
	"a red from your own diff is fixed by a push; a red that is only main's red is fixed on main")

var pipelineLogCmd = &cobra.Command{
	Use:   "log <bead-id-or-mr-url> [job-name]",
	Short: "Fetch job log output from the MR's pipeline",
	Long: `Fetch the log output for a CI job in the MR's latest pipeline.

If job-name is provided, show that specific job's log.
If omitted and the pipeline has a failed job, show the first failed job's log.
If no jobs have failed, list available jobs.`,
	Args: cobra.RangeArgs(1, 2),
	RunE: runPipelineLog,
}

var pipelineWhyFailedCmd = &cobra.Command{
	Use:   "why-failed <bead-id-or-mr-url-or-pipeline-url>",
	Short: "Surface the real failing entries for a pipeline (GitLab jobs + external commit statuses)",
	Long: `Investigates why a pipeline is failing by querying BOTH:

  1. GitLab CI jobs (/projects/:id/pipelines/:pid/jobs) — what glab ci get shows
  2. External commit statuses (/projects/:id/repository/commits/:sha/statuses)
     — pushed by external systems (other CI runners) that DO contribute to the aggregated
     pipeline status but are invisible to glab ci get.

This is the right tool when a pipeline is marked failed but all GitLab jobs look green.`,
	Args: cobra.ExactArgs(1),
	RunE: runPipelineWhyFailed,
}

func init() {
	pipelineArtifactsCmd.Flags().Bool("download", false, "Download artifacts instead of listing")
	pipelineArtifactsCmd.Flags().StringP("output-dir", "o", ".", "Directory to save downloaded artifacts")

	pipelineCmd.AddCommand(pipelineArtifactsCmd)
	pipelineCmd.AddCommand(pipelineLogCmd)
	pipelineCmd.AddCommand(pipelineRetriggerCmd)
	pipelineCmd.AddCommand(pipelineWhyFailedCmd)
}

// pipelineURLPattern matches GitLab pipeline URLs:
//
//	https://gitlab.com/org/repo/-/pipelines/12345
var pipelineURLPattern = regexp.MustCompile(`^https?://[^/]+/(.+?)/-/pipelines/(\d+)`)

// resolvePipelineInfo resolves a bead ID or MR URL to (projectPath, pipelineID,
// baseURL). baseURL is the GitLab base URL derived from the resolved MR/pipeline
// URL host (empty when arg is a pipeline URL, since no GitLab call is needed to
// resolve it); callers pass it back into newMRGitLabClient so a full URL arg
// works without --gitlab-url/GITLAB_BASE_URL set (bug ka-0000008229).
func resolvePipelineInfo(cmd *cobra.Command, arg string) (projectPath string, pipelineID int, baseURL string, err error) {
	// If it's a pipeline URL, parse directly — no GitLab call needed.
	if m := pipelineURLPattern.FindStringSubmatch(arg); m != nil {
		pid, _ := strconv.Atoi(m[2])
		return m[1], pid, baseURLFromGitLabURL(arg), nil
	}

	// Resolve to MR URL (bead ID or MR URL).
	mrURL, rErr := resolveMRURL(cmd, arg)
	if rErr != nil {
		return "", 0, "", rErr
	}

	ref := bridge.ParseMRURL(mrURL)
	if ref == nil {
		return "", 0, "", fmt.Errorf("could not parse MR URL: %s", mrURL)
	}

	// Build the client only once the URL is known, so the base URL can be
	// derived from the MR URL host when --gitlab-url/GITLAB_BASE_URL is unset.
	baseURL = baseURLFromGitLabURL(mrURL)
	client, cErr := newMRGitLabClient(baseURL)
	if cErr != nil {
		return "", 0, "", cErr
	}

	// Query GitLab for the MR to get pipeline info.
	mr, mErr := client.GetMergeRequestByPath(cmd.Context(), ref.ProjectPath, ref.IID)
	if mErr != nil {
		return "", 0, "", fmt.Errorf("fetching MR !%d: %w", ref.IID, mErr)
	}

	if mr.HeadPipeline == nil || mr.HeadPipeline.ID == 0 {
		return "", 0, "", fmt.Errorf("MR !%d has no pipeline", ref.IID)
	}

	return ref.ProjectPath, mr.HeadPipeline.ID, baseURL, nil
}

func runPipelineArtifacts(cmd *cobra.Command, args []string) error {
	projectPath, pipelineID, baseURL, err := resolvePipelineInfo(cmd, args[0])
	if err != nil {
		return err
	}

	client, err := newMRGitLabClient(baseURL)
	if err != nil {
		return err
	}

	jobs, err := client.ListPipelineJobsByPath(cmd.Context(), projectPath, pipelineID)
	if err != nil {
		return fmt.Errorf("listing jobs: %w", err)
	}

	// Filter jobs that have artifacts.
	type artifactEntry struct {
		JobID    int    `json:"job_id"`
		JobName  string `json:"job_name"`
		FileType string `json:"file_type"`
		Filename string `json:"filename"`
		Size     int64  `json:"size"`
	}

	var entries []artifactEntry
	for _, job := range jobs {
		for _, a := range job.Artifacts {
			if a.Size > 0 {
				entries = append(entries, artifactEntry{
					JobID:    job.ID,
					JobName:  job.Name,
					FileType: a.FileType,
					Filename: a.Filename,
					Size:     a.Size,
				})
			}
		}
	}

	download, _ := cmd.Flags().GetBool("download")

	if !download {
		// List mode.
		if len(entries) == 0 {
			if jsonOutput {
				printJSON([]any{})
			} else {
				cmd.Printf("No artifacts for pipeline #%d\n", pipelineID)
			}
			return nil
		}

		if jsonOutput {
			printJSON(entries)
			return nil
		}

		cmd.Printf("%-30s %-15s %-12s %10s\n", "JOB", "FILE TYPE", "FILENAME", "SIZE")
		cmd.Printf("%-30s %-15s %-12s %10s\n",
			strings.Repeat("-", 30), strings.Repeat("-", 15), strings.Repeat("-", 12), strings.Repeat("-", 10))
		for _, e := range entries {
			cmd.Printf("%-30s %-15s %-12s %10s\n",
				truncate(e.JobName, 30), e.FileType, truncate(e.Filename, 12), formatSize(e.Size))
		}
		return nil
	}

	// Download mode.
	if len(entries) == 0 {
		cmd.Printf("No artifacts to download for pipeline #%d\n", pipelineID)
		return nil
	}

	outputDir, _ := cmd.Flags().GetString("output-dir")
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return fmt.Errorf("creating output directory: %w", err)
	}

	// Download artifacts per job (one archive per job).
	downloaded := make(map[int]bool)
	for _, e := range entries {
		if downloaded[e.JobID] {
			continue
		}
		downloaded[e.JobID] = true

		filename := fmt.Sprintf("%s-artifacts.zip", e.JobName)
		destPath := filepath.Join(outputDir, filename)

		f, fErr := os.Create(destPath)
		if fErr != nil {
			return fmt.Errorf("creating %s: %w", filename, fErr)
		}

		if dErr := client.DownloadJobArtifacts(cmd.Context(), projectPath, e.JobID, f); dErr != nil {
			f.Close()
			os.Remove(destPath)
			return fmt.Errorf("downloading artifacts for job %s: %w", e.JobName, dErr)
		}
		f.Close()

		cmd.Printf("Downloaded %s\n", destPath)
	}

	return nil
}

func runPipelineLog(cmd *cobra.Command, args []string) error {
	projectPath, pipelineID, baseURL, err := resolvePipelineInfo(cmd, args[0])
	if err != nil {
		return err
	}

	client, err := newMRGitLabClient(baseURL)
	if err != nil {
		return err
	}

	jobs, err := client.ListPipelineJobsByPath(cmd.Context(), projectPath, pipelineID)
	if err != nil {
		return fmt.Errorf("listing jobs: %w", err)
	}

	if len(jobs) == 0 {
		cmd.Printf("No jobs found for pipeline #%d\n", pipelineID)
		return nil
	}

	var targetJob *bridge.GitLabJob

	if len(args) > 1 {
		// Find the named job.
		jobName := args[1]
		for i := range jobs {
			if jobs[i].Name == jobName {
				targetJob = &jobs[i]
				break
			}
		}
		if targetJob == nil {
			cmd.Printf("Job %q not found in pipeline #%d. Available jobs:\n", jobName, pipelineID)
			for _, j := range jobs {
				cmd.Printf("  %s (%s) — %s\n", j.Name, j.Stage, j.Status)
			}
			return nil
		}
	} else {
		// Auto-select: first failed job, or list all.
		for i := range jobs {
			if jobs[i].Status == "failed" {
				targetJob = &jobs[i]
				break
			}
		}
		if targetJob == nil {
			if jsonOutput {
				type jobSummary struct {
					ID     int    `json:"id"`
					Name   string `json:"name"`
					Stage  string `json:"stage"`
					Status string `json:"status"`
				}
				var summaries []jobSummary
				for _, j := range jobs {
					summaries = append(summaries, jobSummary{j.ID, j.Name, j.Stage, j.Status})
				}
				printJSON(summaries)
				return nil
			}
			cmd.Printf("No failed jobs in pipeline #%d. Available jobs:\n", pipelineID)
			for _, j := range jobs {
				cmd.Printf("  %-30s %-15s %s\n", j.Name, j.Stage, j.Status)
			}
			return nil
		}
	}

	cmd.Printf("Fetching log for job %q (%s, %s)...\n", targetJob.Name, targetJob.Stage, targetJob.Status)

	log, err := client.GetJobLog(cmd.Context(), projectPath, targetJob.ID)
	if err != nil {
		return fmt.Errorf("fetching job log: %w", err)
	}

	fmt.Println(log)
	return nil
}

// whyFailedEntry is the unified view of a failing (or present) step —
// either a GitLab CI job or an external commit status.
type whyFailedEntry struct {
	Source       string `json:"source"` // "gitlab-job" or "commit-status"
	Name         string `json:"name"`
	Stage        string `json:"stage,omitempty"`
	Status       string `json:"status"`
	AllowFailure bool   `json:"allow_failure"`
	ID           int    `json:"id,omitempty"`
	WebURL       string `json:"web_url,omitempty"`
	TargetURL    string `json:"target_url,omitempty"`
	Description  string `json:"description,omitempty"`
}

type whyFailedReport struct {
	ProjectPath    string           `json:"project_path"`
	PipelineID     int              `json:"pipeline_id"`
	PipelineStatus string           `json:"pipeline_status"`
	PipelineURL    string           `json:"pipeline_web_url,omitempty"`
	SHA            string           `json:"sha"`
	Failures       []whyFailedEntry `json:"failures"`
	AllEntries     []whyFailedEntry `json:"all_entries"`

	// jobCount is the number of GitLab CI jobs in AllEntries (the rest are
	// external commit statuses). Used only for human-readable rendering.
	jobCount int
}

func runPipelineWhyFailed(cmd *cobra.Command, args []string) error {
	projectPath, pipelineID, baseURL, err := resolvePipelineInfo(cmd, args[0])
	if err != nil {
		return err
	}

	client, err := newMRGitLabClient(baseURL)
	if err != nil {
		return err
	}

	pip, err := client.GetPipelineByPath(cmd.Context(), projectPath, pipelineID)
	if err != nil {
		return fmt.Errorf("fetching pipeline: %w", err)
	}

	jobs, err := client.ListPipelineJobsByPath(cmd.Context(), projectPath, pipelineID)
	if err != nil {
		return fmt.Errorf("listing jobs: %w", err)
	}

	statuses, err := client.ListCommitStatusesByPath(cmd.Context(), projectPath, pip.SHA)
	if err != nil {
		return fmt.Errorf("listing commit statuses: %w", err)
	}

	report := buildWhyFailedReport(projectPath, pipelineID, pip, jobs, statuses)

	renderWhyFailedReport(cmd, report)
	return nil
}

// buildWhyFailedReport merges GitLab pipeline jobs with external commit
// statuses into a single report. Commit statuses whose name is already covered
// by a GitLab job are dropped — the job is canonical for that name. Statuses
// from external runners survive and are grouped separately.
//
// Hard failures are entries with Status=="failed" and AllowFailure==false; the
// pipeline-level status is ultimately determined by whether any such entry
// exists, so these are what the CLI surfaces by default.
func buildWhyFailedReport(projectPath string, pipelineID int, pip *bridge.GitLabPipeline, jobs []bridge.GitLabJob, statuses []bridge.GitLabCommitStatus) whyFailedReport {
	jobNames := make(map[string]bool, len(jobs))
	var all []whyFailedEntry
	for _, j := range jobs {
		jobNames[j.Name] = true
		all = append(all, whyFailedEntry{
			Source: "gitlab-job",
			Name:   j.Name,
			Stage:  j.Stage,
			Status: j.Status,
			ID:     j.ID,
			WebURL: j.WebURL,
		})
	}
	for _, s := range statuses {
		if jobNames[s.Name] {
			continue
		}
		all = append(all, whyFailedEntry{
			Source:       "commit-status",
			Name:         s.Name,
			Status:       s.Status,
			AllowFailure: s.AllowFailure,
			ID:           s.ID,
			TargetURL:    s.TargetURL,
			Description:  s.Description,
		})
	}

	var failures []whyFailedEntry
	for _, e := range all {
		if e.Status == "failed" && !e.AllowFailure {
			failures = append(failures, e)
		}
	}

	report := whyFailedReport{
		ProjectPath: projectPath,
		PipelineID:  pipelineID,
		Failures:    failures,
		AllEntries:  all,
		jobCount:    len(jobs),
	}
	if pip != nil {
		report.PipelineStatus = pip.Status
		report.PipelineURL = pip.WebURL
		report.SHA = pip.SHA
	}
	return report
}

func renderWhyFailedReport(cmd *cobra.Command, report whyFailedReport) {
	if jsonOutput {
		printJSON(report)
		return
	}

	cmd.Printf("Pipeline #%d (%s) on %s @ %s\n", report.PipelineID, report.PipelineStatus, report.ProjectPath, report.SHA)
	if report.PipelineURL != "" {
		cmd.Printf("  %s\n", report.PipelineURL)
	}
	cmd.Printf("\n%d GitLab job(s), %d commit status(es) not covered by jobs, %d hard failure(s)\n",
		report.jobCount, len(report.AllEntries)-report.jobCount, len(report.Failures))

	if len(report.Failures) == 0 {
		cmd.Printf("\nNo hard failures found. Pipeline status may reflect a transient/canceled state.\n")
		if len(report.AllEntries) > 0 {
			cmd.Printf("\nAll entries:\n")
			renderWhyFailedGrouped(cmd, report.AllEntries)
		}
		return
	}

	cmd.Printf("\nFailing entries (allow_failure=false):\n")
	renderWhyFailedGrouped(cmd, report.Failures)
}

// renderWhyFailedGrouped prints entries grouped by source: GitLab jobs first,
// then external commit statuses. This makes it obvious at a glance which
// failures came from GitLab CI vs an external CI runner.
func renderWhyFailedGrouped(cmd *cobra.Command, entries []whyFailedEntry) {
	var gitlab, external []whyFailedEntry
	for _, e := range entries {
		if e.Source == "gitlab-job" {
			gitlab = append(gitlab, e)
		} else {
			external = append(external, e)
		}
	}
	if len(gitlab) > 0 {
		cmd.Printf("  GitLab jobs:\n")
		for _, e := range gitlab {
			printWhyFailedEntry(cmd, e)
		}
	}
	if len(external) > 0 {
		cmd.Printf("  External commit statuses:\n")
		for _, e := range external {
			printWhyFailedEntry(cmd, e)
		}
	}
}

func printWhyFailedEntry(cmd *cobra.Command, e whyFailedEntry) {
	stage := e.Stage
	if stage == "" {
		stage = "-"
	}
	cmd.Printf("  [%s] %-40s %-10s %-10s", e.Source, truncate(e.Name, 40), stage, e.Status)
	if e.AllowFailure {
		cmd.Printf(" (allow_failure)")
	}
	cmd.Printf("\n")
	if url := e.WebURL; url != "" {
		cmd.Printf("      %s\n", url)
	} else if url := e.TargetURL; url != "" {
		cmd.Printf("      %s\n", url)
	}
	if e.Description != "" {
		cmd.Printf("      %s\n", e.Description)
	}
}
