package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// GitLabTriggerSource implements TriggerSource for GitLab MR/pipeline triggers.
type GitLabTriggerSource struct {
	gitlab *GitLabClient
	logger *slog.Logger
}

// NewGitLabTriggerSourceWithLogger creates a GitLab trigger source with an
// explicit logger. Callers that configure a non-default slog logger (e.g. the
// gitlab-bridge JSON handler) use this so the pipeline_ref_pattern
// matched/filtered counts land on their structured sink rather than the
// process default.
func NewGitLabTriggerSourceWithLogger(gitlab *GitLabClient, logger *slog.Logger) *GitLabTriggerSource {
	if logger == nil {
		logger = slog.Default()
	}
	return &GitLabTriggerSource{gitlab: gitlab, logger: logger}
}

func (s *GitLabTriggerSource) Name() string { return "gitlab" }

// defaultPipelineMaxAge bounds how far back a pipeline trigger looks when its
// bead sets no explicit pipeline_max_age. GitLab's pipelines endpoint returns
// up to 100 historical pipelines with no implicit recency window, so a
// freshly-enabled (or engine-restarted) pipeline trigger whose seen-map is
// empty would otherwise treat every pre-existing failure as new and fire
// against all of them. A source-level recency window makes re-enable and
// restart safe regardless of seen-map state — the durable guard the engine's
// LastChecked.IsZero()-gated bootstrap seeding cannot provide once a stale
// last_checked is present. See ka-0000004176 / decision ka-0000000538.
const defaultPipelineMaxAge = 2 * time.Hour

// gitlabTriggerData holds the GitLab-specific data for a trigger.
type gitlabTriggerData struct {
	ProjectPath  string // GitLab project path (e.g. "example-org/monorepo")
	State        string // MR state filter: opened, merged, closed, all
	Labels       string // comma-separated label filter
	AuthorUser   string // filter by author username
	TargetBranch string // filter by target branch
	EventType    string // "merge_request" or "pipeline" (default: merge_request)
	PipelineRef  string // branch/tag ref for pipeline triggers (exact match, GitLab ?ref=)
	// PipelineRefPattern is an optional Go regexp applied to each pipeline's Ref
	// AFTER the GitLab API fetch and BEFORE matches are returned. It complements
	// (does not replace) the server-side exact PipelineRef: train refs are per-MR
	// (refs/merge-requests/<N>/train), so no single exact ?ref= can select "all
	// trains plus main", but a regexp can. A non-matching ref never becomes a
	// TriggerMatch — so it never creates a task bead and never spawns an agent.
	// An invalid regexp fails CLOSED (evaluatePipelines returns an error and zero
	// matches), matching the parsePipelineMaxAge posture: a typo must not silently
	// reopen the spawn firehose.
	PipelineRefPattern string
	PipelineStatus     string        // pipeline status filter (success, failed, etc.)
	PipelineMaxAge     time.Duration // recency window for pipeline triggers (0 ⇒ defaultPipelineMaxAge)
	// MinDuration is an optional "slow pipeline" threshold (raw Go duration
	// string, e.g. "45m"). When set, a pipeline becomes a match ONLY if its run
	// duration is >= MinDuration — so the fleet can wake on a train that merely
	// got SLOW, not only one that BROKE (operator ask, thread ka-0000004950).
	//
	// It is stored raw and parsed in evaluatePipelines (same shape as
	// PipelineRefPattern) so an invalid value fails CLOSED: evaluate returns an
	// error and zero matches, logged loudly, rather than silently disabling the
	// threshold and reopening the spawn firehose.
	//
	// CRITICAL: GitLab's LIST pipelines endpoint omits `duration` entirely (only
	// the single-pipeline DETAIL endpoint returns it — verified against the live
	// API 2026-08-05). So when MinDuration is set, evaluatePipelines fetches
	// pipeline detail for each candidate that already passed the cheap
	// status+ref filters, and applies the threshold to the authoritative
	// duration. A null/unknown duration fails closed (never fires). Empty
	// MinDuration ⇒ no threshold and NO detail fetch, i.e. exactly the
	// pre-feature behaviour at zero extra API cost.
	MinDuration string
	// IgnoreFailedStatuses is an optional comma-separated list of external commit
	// status NAMES (e.g. "Meticulous Tests"). When set, a FAILED pipeline is
	// SUPPRESSED — never becomes a match, never creates a task bead, never spawns
	// an agent — if and ONLY if every required (allow_failure=false) failed commit
	// status on the pipeline's SHA is named in this list. This exists because a
	// monorepo pipeline can read "failed" with EVERY GitLab job green when the sole
	// red gate is an external required commit status (see advice ka-0000006053);
	// when that gate is a visual-diff tool like Meticulous, the failure is expected
	// review noise the tool already comments on, and spawning a failure analyst is
	// pure waste (operator ruling, advice ka-0000007792).
	//
	// It fails SAFE, not closed: an empty value ⇒ no suppression and NO extra API
	// call (pre-feature behaviour). When set, evaluatePipelines fetches the SHA's
	// commit statuses only for candidates whose status is "failed"; a fetch error,
	// or a failure that originates OUTSIDE the named statuses (e.g. a real CI job
	// failure, or no required commit status failed at all), never suppresses — the
	// pipeline still matches and an analyst still spawns. Suppression happens only
	// when the ignorable set fully explains the failure.
	IgnoreFailedStatuses string

	// AllowHumanMRSpawns is an optional boolean that explicitly allows spawning
	// agents in response to pipeline failures on merge requests authored by humans.
	// When false or empty (default), pipeline-triggered spawn_agent actions will
	// not fire for MR pipelines without explicit permission. This guard prevents
	// unintended job execution on human-authored work (operator ruling, 2026-09-02).
	AllowHumanMRSpawns string
}

func (s *GitLabTriggerSource) LoadTriggers(ctx context.Context, daemon TriggerBeadClient) ([]TriggerDef, error) {
	result, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"trigger"},
		Statuses: []string{"open"},
		Labels:   []string{"trigger:gitlab"},
		Limit:    100,
	})
	if err != nil {
		return nil, err
	}

	var triggers []TriggerDef
	for _, bead := range result.Beads {
		if bead.Fields["enabled"] == "false" {
			continue
		}
		projectPath := bead.Fields["project_path"]
		if projectPath == "" {
			continue
		}

		t := ParseTriggerDef(bead, "gitlab")
		t.SourceData = parseGitLabTriggerData(bead)
		triggers = append(triggers, t)
	}
	return triggers, nil
}

func parseGitLabTriggerData(bead *beadsapi.BeadDetail) gitlabTriggerData {
	eventType := bead.Fields["event_type"]
	if eventType == "" {
		eventType = "merge_request"
	}
	return gitlabTriggerData{
		ProjectPath:          bead.Fields["project_path"],
		State:                bead.Fields["state"],
		Labels:               bead.Fields["gitlab_labels"],
		AuthorUser:           bead.Fields["author"],
		TargetBranch:         bead.Fields["target_branch"],
		EventType:            eventType,
		PipelineRef:          bead.Fields["pipeline_ref"],
		PipelineRefPattern:   strings.TrimSpace(bead.Fields["pipeline_ref_pattern"]),
		PipelineStatus:       bead.Fields["pipeline_status"],
		PipelineMaxAge:       parsePipelineMaxAge(bead.Fields["pipeline_max_age"]),
		MinDuration:          strings.TrimSpace(bead.Fields["min_duration"]),
		IgnoreFailedStatuses: strings.TrimSpace(bead.Fields["ignore_failed_statuses"]),
		AllowHumanMRSpawns:   strings.TrimSpace(bead.Fields["allow_human_mr_spawns"]),
	}
}

// parseIgnoreStatuses splits the comma-separated ignore_failed_statuses field
// into a set of lower-cased, trimmed status names. Matching is case-insensitive
// and exact (not substring): over-broad matching here would SUPPRESS a real
// failure, so precision is the fail-safe choice. An empty/blank value yields an
// empty set ⇒ the filter is inert.
func parseIgnoreStatuses(raw string) map[string]struct{} {
	set := make(map[string]struct{})
	for _, part := range strings.Split(raw, ",") {
		name := strings.ToLower(strings.TrimSpace(part))
		if name != "" {
			set[name] = struct{}{}
		}
	}
	return set
}

// parseMinDuration reads the optional min_duration ("slow pipeline") threshold
// as a Go duration. It returns (0, false) for an empty value — no threshold,
// the pre-feature behaviour — and (d, true) for a valid positive duration. A
// non-empty but unparseable or non-positive value is a hard error so the caller
// can fail CLOSED (return zero matches) rather than silently disabling the
// threshold: a typo must not turn a slow-pipeline trigger into one that fires on
// every pipeline. Mirrors the fail-closed posture of pipeline_ref_pattern.
func parseMinDuration(raw string) (time.Duration, bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, false, fmt.Errorf("invalid min_duration %q: %w", raw, err)
	}
	if d <= 0 {
		return 0, false, fmt.Errorf("min_duration must be positive, got %q", raw)
	}
	return d, true, nil
}

// parsePipelineMaxAge reads the optional pipeline_max_age field as a Go duration.
// An empty or unparseable value yields 0, which evaluatePipelines treats as
// "apply defaultPipelineMaxAge". A negative value is clamped to 0 (default)
// rather than disabling the window, so a typo can't silently reopen the
// spawn-storm hole. To intentionally widen the window, set a large duration
// (e.g. "720h"); there is deliberately no "unbounded" escape hatch.
func parsePipelineMaxAge(raw string) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		return 0
	}
	return d
}

func (s *GitLabTriggerSource) Evaluate(ctx context.Context, trig TriggerDef) ([]TriggerMatch, error) {
	data, ok := trig.SourceData.(gitlabTriggerData)
	if !ok {
		return nil, nil
	}

	switch data.EventType {
	case "pipeline":
		return s.evaluatePipelines(ctx, data)
	default:
		return s.evaluateMergeRequests(ctx, data)
	}
}

func (s *GitLabTriggerSource) evaluateMergeRequests(ctx context.Context, data gitlabTriggerData) ([]TriggerMatch, error) {
	opts := MRListOptions{
		State:   data.State,
		PerPage: 50,
	}
	if opts.State == "" {
		opts.State = "opened"
	}
	if data.Labels != "" {
		opts.Labels = strings.Split(data.Labels, ",")
		for i, l := range opts.Labels {
			opts.Labels[i] = strings.TrimSpace(l)
		}
	}
	if data.AuthorUser != "" {
		opts.AuthorUser = data.AuthorUser
	}
	if data.TargetBranch != "" {
		opts.TargetBranch = data.TargetBranch
	}

	mrs, err := s.gitlab.ListMergeRequests(ctx, data.ProjectPath, opts)
	if err != nil {
		return nil, err
	}

	var matches []TriggerMatch
	for _, mr := range mrs {
		matches = append(matches, gitlabMRToMatch(mr, data.ProjectPath))
	}
	return matches, nil
}

func (s *GitLabTriggerSource) evaluatePipelines(ctx context.Context, data gitlabTriggerData) ([]TriggerMatch, error) {
	// Bound the lookback with a recency window (updated_after) so a re-enabled
	// or engine-restarted trigger can never fire against pre-existing/stale
	// failures — the seen-map alone is not restart-safe (see
	// defaultPipelineMaxAge). Unset ⇒ defaultPipelineMaxAge.
	maxAge := data.PipelineMaxAge
	if maxAge <= 0 {
		maxAge = defaultPipelineMaxAge
	}
	updatedAfter := time.Now().Add(-maxAge)

	// Compile the optional client-side ref filter BEFORE the API fetch so an
	// invalid regexp fails CLOSED — return an error and zero matches, logged
	// loudly — rather than silently reopening the spawn firehose. Same posture
	// as parsePipelineMaxAge clamping a bad value to the default.
	var refPattern *regexp.Regexp
	if data.PipelineRefPattern != "" {
		var perr error
		refPattern, perr = regexp.Compile(data.PipelineRefPattern)
		if perr != nil {
			s.logger.Error("gitlab pipeline trigger: invalid pipeline_ref_pattern — failing closed, zero matches",
				"project", data.ProjectPath, "pipeline_ref_pattern", data.PipelineRefPattern, "error", perr)
			return nil, fmt.Errorf("invalid pipeline_ref_pattern %q: %w", data.PipelineRefPattern, perr)
		}
	}

	// Parse the optional min_duration ("slow pipeline") threshold BEFORE the API
	// fetch so a bad value fails CLOSED (error + zero matches) rather than
	// silently disabling the threshold — the same posture as pipeline_ref_pattern.
	minDuration, hasMinDuration, mderr := parseMinDuration(data.MinDuration)
	if mderr != nil {
		s.logger.Error("gitlab pipeline trigger: invalid min_duration — failing closed, zero matches",
			"project", data.ProjectPath, "min_duration", data.MinDuration, "error", mderr)
		return nil, mderr
	}

	// The ignore_failed_statuses ("suppress an external-gate-only failure") set.
	// Empty ⇒ the filter is inert and no commit-status detail fetch happens. This
	// filter fails SAFE (still spawns on uncertainty), unlike the fail-closed
	// filters above — see gitlabTriggerData.IgnoreFailedStatuses.
	ignoreStatuses := parseIgnoreStatuses(data.IgnoreFailedStatuses)

	pips, err := s.gitlab.ListPipelines(ctx, data.ProjectPath, data.PipelineRef, data.PipelineStatus, updatedAfter)
	if err != nil {
		return nil, err
	}

	var matches []TriggerMatch
	refFiltered := 0
	durationFiltered := 0
	statusFiltered := 0
	for _, pip := range pips {
		// Apply the ref pattern AFTER the fetch and BEFORE constructing a
		// TriggerMatch, so a non-matching ref never becomes a match, never
		// creates a task bead, and never spawns an agent.
		if refPattern != nil && !refPattern.MatchString(pip.Ref) {
			refFiltered++
			continue
		}

		// Apply the min_duration threshold AFTER the cheap status+ref filters so
		// we only pay the per-pipeline detail fetch for candidates that already
		// matched everything else. GitLab's LIST endpoint omits `duration`, so
		// the list-shaped pip.Duration is always nil here — we MUST fetch the
		// single-pipeline DETAIL to get the authoritative run duration. A
		// null/unknown duration (still running, skipped, or a detail-fetch
		// failure) fails CLOSED: it never becomes a match. This guarantees a
		// slow-pipeline trigger fires ONLY on a pipeline confirmed at/over the
		// threshold, never on an unknown one.
		if hasMinDuration {
			dur, ok := s.resolvePipelineDuration(ctx, data.ProjectPath, pip)
			if !ok || dur < minDuration {
				durationFiltered++
				continue
			}
		}

		// Suppress a failure explained entirely by ignorable external gates (e.g.
		// a Meticulous-only visual-diff failure). Only failed pipelines can be
		// suppressed, and only when the filter is configured — so a non-failed
		// pipeline or an unconfigured trigger pays zero extra API cost. Fails SAFE:
		// see pipelineFailureIsIgnorable.
		if len(ignoreStatuses) > 0 && pip.Status == "failed" &&
			s.pipelineFailureIsIgnorable(ctx, data.ProjectPath, pip, ignoreStatuses) {
			statusFiltered++
			continue
		}

		matches = append(matches, gitlabPipelineToMatch(pip, data.ProjectPath))
	}

	// Log NUMBERS (fetched / matched / filtered) so we can prove the filter fires
	// rather than succeeding silently — the whole point of these fields is to
	// suppress no-op spawns, and a silent success is indistinguishable from a
	// filter that never ran.
	if refPattern != nil || hasMinDuration || len(ignoreStatuses) > 0 {
		s.logger.Info("gitlab pipeline trigger: applied client-side filters",
			"project", data.ProjectPath,
			"pipeline_ref_pattern", data.PipelineRefPattern, "min_duration", data.MinDuration,
			"ignore_failed_statuses", data.IgnoreFailedStatuses,
			"fetched", len(pips), "matched", len(matches),
			"ref_filtered", refFiltered, "duration_filtered", durationFiltered,
			"status_filtered", statusFiltered)
	}
	return matches, nil
}

// pipelineFailureIsIgnorable reports whether a FAILED pipeline should be
// suppressed because every required (allow_failure=false) failed commit status
// on its SHA is named in `ignore` (lower-cased) — e.g. an external "Meticulous
// Tests" visual-diff gate that is the pipeline's only red gate.
//
// It fails SAFE, never closed. It returns false (⇒ still match, still spawn) when:
//   - the commit-status fetch errors (transient API failure), or
//   - any required failed status is NOT in the ignore set (a real failure the
//     analyst should look at), or
//   - NO required commit status is failed at all (the failure originates outside
//     commit statuses — GitLab's LIST commit statuses returns the latest status
//     per name, so a since-retried job reads green here; treat as uncertain).
//
// It returns true (⇒ suppress) only when it saw at least one required failed
// status AND every one of them is named in `ignore`.
func (s *GitLabTriggerSource) pipelineFailureIsIgnorable(ctx context.Context, projectPath string, pip GitLabPipeline, ignore map[string]struct{}) bool {
	statuses, err := s.gitlab.ListCommitStatusesByPath(ctx, projectPath, pip.SHA)
	if err != nil {
		s.logger.Warn("gitlab pipeline trigger: ignore_failed_statuses commit-status fetch failed — not suppressing (fail safe)",
			"project", projectPath, "pipeline_id", pip.ID, "sha", pip.SHA, "error", err)
		return false
	}

	sawRequiredFailure := false
	for _, st := range statuses {
		if st.Status != "failed" || st.AllowFailure {
			continue
		}
		sawRequiredFailure = true
		if _, ok := ignore[strings.ToLower(strings.TrimSpace(st.Name))]; !ok {
			return false
		}
	}
	return sawRequiredFailure
}

// resolvePipelineDuration returns the authoritative run duration for a pipeline
// and whether it is known. GitLab's LIST pipelines endpoint omits `duration`, so
// the list-shaped pipeline almost never carries it; when absent we fetch the
// single-pipeline DETAIL (which does populate `duration`). Returns ok=false when
// the duration is genuinely unknown (still running/skipped) OR the detail fetch
// fails — the caller treats not-ok as "does not match the threshold" (fail
// closed), so a transient API error can never manufacture a spurious match.
func (s *GitLabTriggerSource) resolvePipelineDuration(ctx context.Context, projectPath string, pip GitLabPipeline) (time.Duration, bool) {
	// Fast path: if the list result unexpectedly already carries a duration
	// (e.g. a future GitLab version, or a caller that pre-populated it), use it
	// and skip the extra round trip.
	if pip.Duration != nil {
		return time.Duration(*pip.Duration * float64(time.Second)), true
	}
	detail, err := s.gitlab.GetPipelineByPath(ctx, projectPath, pip.ID)
	if err != nil {
		s.logger.Warn("gitlab pipeline trigger: min_duration detail fetch failed — treating duration as unknown (no match)",
			"project", projectPath, "pipeline_id", pip.ID, "error", err)
		return 0, false
	}
	if detail == nil || detail.Duration == nil {
		return 0, false
	}
	return time.Duration(*detail.Duration * float64(time.Second)), true
}

func (s *GitLabTriggerSource) CatchUpKeys(ctx context.Context, daemon TriggerBeadClient, triggerID string) ([]string, error) {
	keySet := make(map[string]struct{})

	// Path 1 — file_bead action. The created issue bead carries the
	// `trigger:<id>` label and the `gitlab_key` field (createTaskBead copies
	// both), so the durable fire record is a kind=issue bead.
	issueResult, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Kinds:    []string{"issue"},
		Statuses: []string{"open", "in_progress", "closed"},
		Labels:   []string{"trigger:" + triggerID},
		Limit:    500,
	})
	if err != nil {
		return nil, err
	}
	for _, bead := range issueResult.Beads {
		if key := bead.Fields["gitlab_key"]; key != "" {
			keySet[key] = struct{}{}
		}
	}

	// Path 2 — apply_formula / spawn_agent action. These actions file NO issue
	// bead: the durable fire record is the driver AGENT bead (kind=config),
	// which carries the `gitlab_key` field (triggerSourceFieldKey) and a
	// `trigger_id` field pointing back at this trigger. Because that agent bead
	// is kind=config it is invisible to the kind=issue query above, so without
	// this second pass an apply_formula gitlab trigger has NO restart-durable
	// dedup at all — the in-memory seen-set is its only guard, and every engine
	// restart (or the 24h seen-key TTL eviction) re-opens the fire for an
	// already-processed MR. That is the FSM-475 !1996 verify-fix re-fire storm
	// (ka-0000000963): one merge fanned out 26+ full Opus verify pods on an
	// already-verified, already-shipped MR. No Statuses filter — a completed
	// driver (closed/any status) is still proof the key was already fired.
	agentResult, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:  []string{"agent"},
		Fields: map[string]string{"trigger_id": triggerID},
		Limit:  500,
	})
	if err != nil {
		return nil, err
	}
	for _, bead := range agentResult.Beads {
		if key := bead.Fields["gitlab_key"]; key != "" {
			keySet[key] = struct{}{}
		}
	}

	keys := make([]string, 0, len(keySet))
	for k := range keySet {
		keys = append(keys, k)
	}
	return keys, nil
}

// gitlabMRToMatch converts a GitLabMR to a TriggerMatch with template vars.
func gitlabMRToMatch(mr GitLabMR, projectPath string) TriggerMatch {
	key := fmt.Sprintf("%s!%d", projectPath, mr.IID)

	vars := map[string]string{
		"{{gitlab_key}}":           key,
		"{{gitlab_mr_iid}}":        strconv.Itoa(mr.IID),
		"{{gitlab_mr_title}}":      mr.Title,
		"{{gitlab_mr_url}}":        mr.WebURL,
		"{{gitlab_mr_state}}":      mr.State,
		"{{gitlab_project_path}}":  projectPath,
		"{{gitlab_source_branch}}": mr.SourceBranch,
		"{{gitlab_target_branch}}": mr.TargetBranch,
		"{{gitlab_labels}}":        strings.Join(mr.Labels, ", "),
	}

	if mr.Author != nil {
		vars["{{gitlab_author}}"] = mr.Author.Username
		vars["{{gitlab_author_name}}"] = mr.Author.Name
	}
	if mr.HeadPipeline != nil {
		vars["{{gitlab_pipeline_status}}"] = mr.HeadPipeline.Status
		vars["{{gitlab_pipeline_url}}"] = mr.HeadPipeline.WebURL
	}
	if mr.Description != "" {
		vars["{{gitlab_description}}"] = mr.Description
	} else {
		vars["{{gitlab_description}}"] = ""
	}

	fields := map[string]string{
		"gitlab_key":     key,
		"gitlab_mr_url":  mr.WebURL,
		"gitlab_mr_iid":  strconv.Itoa(mr.IID),
		"gitlab_project": projectPath,
	}
	if mr.Author != nil {
		fields["gitlab_author"] = mr.Author.Username
	}
	if mr.HeadPipeline != nil {
		fields["gitlab_pipeline_status"] = mr.HeadPipeline.Status
	}

	return TriggerMatch{
		Key:    key,
		Title:  mr.Title,
		Fields: fields,
		Labels: []string{
			"gitlab:" + key,
		},
		TemplateVars: vars,
	}
}

// gitlabPipelineToMatch converts a GitLabPipeline to a TriggerMatch with template vars.
func gitlabPipelineToMatch(pip GitLabPipeline, projectPath string) TriggerMatch {
	key := fmt.Sprintf("%s/pipeline/%d", projectPath, pip.ID)

	vars := map[string]string{
		"{{gitlab_key}}":             key,
		"{{gitlab_pipeline_id}}":     strconv.Itoa(pip.ID),
		"{{gitlab_pipeline_status}}": pip.Status,
		"{{gitlab_pipeline_ref}}":    pip.Ref,
		"{{gitlab_pipeline_sha}}":    pip.SHA,
		"{{gitlab_pipeline_url}}":    pip.WebURL,
		"{{gitlab_project_path}}":    projectPath,
	}

	fields := map[string]string{
		"gitlab_key":             key,
		"gitlab_pipeline_id":     strconv.Itoa(pip.ID),
		"gitlab_pipeline_url":    pip.WebURL,
		"gitlab_pipeline_status": pip.Status,
		"gitlab_project":         projectPath,
	}

	return TriggerMatch{
		Key:    key,
		Title:  fmt.Sprintf("Pipeline #%d (%s) on %s — %s", pip.ID, pip.Status, pip.Ref, projectPath),
		Fields: fields,
		Labels: []string{
			"gitlab:" + key,
		},
		TemplateVars: vars,
	}
}
