package main

import "github.com/spf13/cobra"

// ciCmd is a namespace for CI/pipeline triage helpers that mirror the
// `glab ci` verbs gardeners reach for (ci get, ci view, ci trace). Subcommands
// here should make the "why is CI red?" workflow discoverable without the
// caller having to know whether a failure came from a GitLab job or an
// external commit status pushed by another CI runner.
var ciCmd = &cobra.Command{
	Use:     "ci",
	Short:   "CI triage (why-failed) and bounded GitHub Actions re-runs and dispatches",
	GroupID: "orchestration",
}

// ciWhyFailedCmd aliases `gb pipeline why-failed` so gardeners coming from
// `glab ci get` find the same logic without learning a new namespace. Both
// share the same implementation; this is a discoverability alias, not a fork.
var ciWhyFailedCmd = &cobra.Command{
	Use:   "why-failed <bead-id-or-mr-url-or-pipeline-url>",
	Short: "Surface the real failing entries for a pipeline (GitLab jobs + external commit statuses)",
	Long: `Investigates why a pipeline is failing by querying BOTH:

  1. GitLab CI jobs (/projects/:id/pipelines/:pid/jobs) — what glab ci get shows
  2. External commit statuses (/projects/:id/repository/commits/:sha/statuses)
     — pushed by external systems (other CI runners) that DO contribute to the aggregated
     pipeline status but are invisible to glab ci get.

Failures are grouped by source (GitLab job vs external commit status) so it is
obvious which runner owns the red.

Alias of 'gb pipeline why-failed'.`,
	Args: cobra.ExactArgs(1),
	RunE: runPipelineWhyFailed,
}

func init() {
	ciCmd.AddCommand(ciWhyFailedCmd)
}
