// Package bridge provides the JIRA polling loop.
//
// JiraPoller periodically queries JIRA for new issues matching configured
// JQL criteria and creates task beads in the beads daemon. It deduplicates
// by tracking JIRA key → bead ID mappings, and on startup runs a CatchUp
// pass to populate the tracked map from existing beads.
package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// JiraBeadClient is the subset of beadsapi.Client used by the JIRA poller.
type JiraBeadClient interface {
	CreateBead(ctx context.Context, req beadsapi.CreateBeadRequest) (string, error)
	ListTaskBeads(ctx context.Context) ([]*beadsapi.BeadDetail, error)
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
	AddDependency(ctx context.Context, beadID, dependsOnID, depType, createdBy string) error
	UpdateBeadFields(ctx context.Context, beadID string, fields map[string]string) error
	CloseBead(ctx context.Context, beadID string, fields map[string]string) error
}

// JiraPollerConfig holds configuration for the JIRA poller.
type JiraPollerConfig struct {
	Projects       []string          // JIRA project keys (e.g., ["PE", "DEVOPS"])
	Statuses       []string          // JIRA statuses to ingest
	IssueTypes     []string          // JIRA issue types to ingest
	FixVersions    []string          // JIRA fixVersion filter (e.g., ["2.5.0"])
	RequiredLabels []string          // JIRA labels that must be present (e.g., ["approved-to-work"])
	ExcludedLabels []string          // JIRA labels that must NOT be present
	FilterProjects []string          // Projects that FixVersions/RequiredLabels apply to (empty = all)
	CustomJQL      string            // Arbitrary JQL clause appended to the filter
	PollInterval   time.Duration     // Polling interval (default 60s)
	ProjectMap     map[string]string // JIRA prefix (upper) → boat project name (e.g., "PE" → "monorepo")
	SkipAssigned   bool              // Skip JIRA issues that have a human assignee
	BotAccountID   string            // JIRA account ID of the bot (excluded from assignee checks)

	// CustomFields maps a JIRA custom-field id to the bead field name it should
	// be promoted to (e.g. {"customfield_11976": "jira_issue_url"}). These are
	// always requested from JIRA and written as scalar bead fields.
	CustomFields map[string]string
	// CaptureAllCustom, when true, captures every populated customfield_* value
	// into a single JSON bead field (jira_custom_fields) keyed by human-readable
	// field name, so agents can inspect the full custom-field map without
	// modeling each id. Requires fetching all fields ("*all") from JIRA.
	CaptureAllCustom bool

	// WritePolicy is the config-driven, project-agnostic write policy
	// (ka-0000007252). When non-nil it is the SOLE project-verdict path for the
	// back-off comment — FSM is just a configured toggle, not a compiled-in
	// special case. The production jira-bridge wires this. When nil (tests) the
	// legacy ProjectGate below is used; DefaultWritePolicy reproduces its project
	// verdict byte-for-byte so the swap is behavior-safe. Like ProjectGate it is
	// a process-level control preserved across hot-reload (see UpdateConfig), NOT
	// sourced from the jira-poller config bead.
	WritePolicy *WritePolicyStore

	// ProjectGate is the coordinated deny-by-default project gate shared with
	// the other three outbound paths. LEGACY fallback — used only when
	// WritePolicy is nil. FSM is governed by the single JIRA_FSM_SYNC_ENABLED
	// toggle (ka-0000001679). nil ⇒ the poller falls back to the package default
	// gate (FSM off), so behavior is unchanged. It is NOT sourced from the
	// jira-poller config bead — UpdateConfig preserves the process-level gate
	// across hot-reload so a config-bead push cannot clobber the cutover toggle.
	ProjectGate *ProjectSyncGate

	Logger *slog.Logger
}

// JiraPoller polls JIRA for new issues and creates task beads.
type JiraPoller struct {
	jira   *JiraClient
	daemon JiraBeadClient
	cfg    JiraPollerConfig

	mu        sync.Mutex
	tracked   map[string]string // JIRA key → bead ID
	wiredDeps map[string]bool   // "src:dst:type" dedup
	// caughtUp flips true once a CatchUp pass has fully succeeded. Bead
	// creation (fullScan/poll) is blocked until then — creating with an
	// unseeded tracked map duplicates every JQL-matching issue when the
	// beads daemon restarts alongside the bridge in a deploy window
	// (ka-0000001931).
	caughtUp bool
	// pendingFullScan defers the startup full scan when the initial
	// catch-up retry budget elapsed without success; the first caught-up
	// poll cycle runs it instead.
	pendingFullScan bool
	// pendingTriggerCatchUp defers the trigger evaluator's seen-set
	// catch-up the same way — evaluating triggers with empty seen sets
	// re-fires every already-handled issue (duplicate agent spawns).
	pendingTriggerCatchUp bool

	configCh chan JiraPollerConfig // receives hot-reloaded configs
	triggers *JiraTriggerEvaluator // optional: JQL trigger evaluator

	fieldNames   map[string]string // customfield id → human name (lazy, for CaptureAllCustom)
	fieldNamesAt time.Time         // last refresh of fieldNames

	// catchUpRetryInterval / catchUpRetryBudget pace the fail-closed startup
	// catch-up loop in Run. Overridable in tests.
	catchUpRetryInterval time.Duration
	catchUpRetryBudget   time.Duration
}

// NewJiraPoller creates a new JIRA polling loop.
func NewJiraPoller(jira *JiraClient, daemon JiraBeadClient, cfg JiraPollerConfig) *JiraPoller {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 60 * time.Second
	}
	return &JiraPoller{
		jira:                 jira,
		daemon:               daemon,
		cfg:                  cfg,
		tracked:              make(map[string]string),
		wiredDeps:            make(map[string]bool),
		configCh:             make(chan JiraPollerConfig, 1),
		catchUpRetryInterval: 5 * time.Second,
		catchUpRetryBudget:   5 * time.Minute,
	}
}

// SetTriggerEvaluator attaches a JQL trigger evaluator to the poller.
// When set, trigger beads are evaluated after each poll cycle.
func (p *JiraPoller) SetTriggerEvaluator(evaluator *JiraTriggerEvaluator) {
	p.triggers = evaluator
}

// UpdateConfig sends a new configuration to the poller for hot-reload.
// Non-blocking: if the channel is full the update is dropped (the next one
// will carry the latest state).
func (p *JiraPoller) UpdateConfig(cfg JiraPollerConfig) {
	// Preserve logger and project map from original config — these are not
	// set from config beads.
	cfg.Logger = p.cfg.Logger
	if cfg.ProjectMap == nil {
		cfg.ProjectMap = p.cfg.ProjectMap
	}
	// Preserve the coordinated project gate + config-driven write policy across
	// hot-reload: both are process-level cutover controls, never sourced from the
	// config bead, so a config-bead push must not reset them to nil
	// (ka-0000001679, ka-0000007252).
	cfg.ProjectGate = p.cfg.ProjectGate
	cfg.WritePolicy = p.cfg.WritePolicy
	select {
	case p.configCh <- cfg:
	default:
	}
}

// projectGate returns the poller's coordinated project gate, falling back to
// the package default (FSM off) when unwired so the verdict is always
// deny-by-default with FSM blocked.
func (p *JiraPoller) projectGate() *ProjectSyncGate {
	if p.cfg.ProjectGate != nil {
		return p.cfg.ProjectGate
	}
	return defaultProjectSyncGate
}

// allowProject reports whether the bridge may post a back-off comment to the
// project owning jiraKey. Prefers the config-driven write policy; falls back to
// the legacy coordinated project gate (which itself defaults to FSM-off) when
// unwired.
func (p *JiraPoller) allowProject(jiraKey string) bool {
	if p.cfg.WritePolicy != nil {
		return p.cfg.WritePolicy.AllowProject(jiraKey)
	}
	return p.projectGate().AllowProject(jiraKey)
}

// projectGateDiag returns a human-readable description of the project verdict
// source for the "back-off comment suppressed" log.
func (p *JiraPoller) projectGateDiag() any {
	if p.cfg.WritePolicy != nil {
		return p.cfg.WritePolicy.Projects()
	}
	return p.projectGate().FSMSyncEnabled()
}

// Run starts the polling loop. It runs CatchUp (with retry) until the tracked
// map is seeded, then polls at the configured interval until ctx is canceled.
// It also listens on configCh for hot-reloaded configurations, resetting the
// ticker on interval changes.
//
// Fail-closed startup (ka-0000001931): bead creation is gated on a successful
// catch-up. During a release deploy the beads daemon and this bridge restart
// together; if catch-up ran once, failed with connection-refused, and fullScan
// then proceeded with an empty tracked map, every JQL-matching issue got a
// duplicate bead. Now catch-up retries until it succeeds (bounded by
// catchUpRetryBudget) and fullScan/poll refuse to create beads until it has.
func (p *JiraPoller) Run(ctx context.Context) error {
	// Touch the poller-metric series to their zero baseline before the loop so
	// the stall/deadman alert has samples the instant the pod is first scraped
	// (jira_poller_stall.go).
	initPollerMetrics()

	// Populate tracked map from existing beads, retrying while the beads
	// daemon comes up.
	p.catchUpWithRetry(ctx)
	if ctx.Err() != nil {
		return ctx.Err()
	}

	// Populate trigger seen sets from existing beads. Runs after the
	// catch-up retry loop so the daemon is known-reachable by now (the seen
	// sets have the same empty-on-failure re-fire hazard as the tracked
	// map). If the retry budget elapsed with the daemon still down, this is
	// deferred to the first caught-up poll cycle (see poll).
	if p.triggers != nil {
		if p.isCaughtUp() {
			p.triggers.CatchUp(ctx)
		} else {
			p.mu.Lock()
			p.pendingTriggerCatchUp = true
			p.mu.Unlock()
		}
	}

	p.cfg.Logger.Info("JIRA poller started",
		"projects", p.cfg.Projects,
		"statuses", p.cfg.Statuses,
		"issue_types", p.cfg.IssueTypes,
		"fix_versions", p.cfg.FixVersions,
		"required_labels", p.cfg.RequiredLabels,
		"excluded_labels", p.cfg.ExcludedLabels,
		"custom_jql", p.cfg.CustomJQL,
		"interval", p.cfg.PollInterval)

	ticker := time.NewTicker(p.cfg.PollInterval)
	defer ticker.Stop()

	// Full scan on start to catch all matching issues across all pages.
	// fullScan refuses to run before a successful catch-up (fail-closed)
	// and defers itself to the first caught-up poll cycle instead.
	p.fullScan(ctx)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case newCfg := <-p.configCh:
			oldInterval := p.cfg.PollInterval
			p.mu.Lock()
			p.cfg.Projects = newCfg.Projects
			p.cfg.Statuses = newCfg.Statuses
			p.cfg.IssueTypes = newCfg.IssueTypes
			p.cfg.FixVersions = newCfg.FixVersions
			p.cfg.RequiredLabels = newCfg.RequiredLabels
			p.cfg.ExcludedLabels = newCfg.ExcludedLabels
			p.cfg.FilterProjects = newCfg.FilterProjects
			p.cfg.CustomJQL = newCfg.CustomJQL
			p.cfg.SkipAssigned = newCfg.SkipAssigned
			if newCfg.PollInterval > 0 {
				p.cfg.PollInterval = newCfg.PollInterval
			}
			p.mu.Unlock()
			if p.cfg.PollInterval != oldInterval {
				ticker.Reset(p.cfg.PollInterval)
			}
			p.cfg.Logger.Info("JIRA poller config reloaded",
				"projects", p.cfg.Projects,
				"fix_versions", p.cfg.FixVersions,
				"required_labels", p.cfg.RequiredLabels,
				"excluded_labels", p.cfg.ExcludedLabels,
				"filter_projects", p.cfg.FilterProjects,
				"custom_jql", p.cfg.CustomJQL,
				"skip_assigned", p.cfg.SkipAssigned,
				"interval", p.cfg.PollInterval)
			// Run a poll immediately with the new config.
			p.poll(ctx)
		case <-ticker.C:
			p.poll(ctx)
		}
	}
}

// CatchUp queries the daemon for existing JIRA-sourced beads and populates the
// tracked map to prevent duplicate creation across restarts. Uses paginated
// ListBeadsFiltered to handle large bead counts reliably.
//
// Returns true only when every page listed successfully; a partial pass (e.g.
// the daemon dropped mid-pagination) returns false and does NOT mark the
// poller caught-up, so bead creation stays blocked (ka-0000001931).
func (p *JiraPoller) CatchUp(ctx context.Context) bool {
	count := 0
	offset := 0
	const pageSize = 500

	for {
		result, err := p.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
			Kinds: []string{"issue"},
			// fsmSourcedBeadStatuses (jira_backfill.go) is the shared population
			// oracle — the full status set an FSM-sourced bead can hold. The GBFSM
			// backfill reuses this exact set so its preview covers the same backlog.
			Statuses: fsmSourcedBeadStatuses,
			Labels:   []string{"source:jira"},
			Limit:    pageSize,
			Offset:   offset,
		})
		if err != nil {
			p.cfg.Logger.Warn("JIRA poller catch-up: failed to list beads",
				"offset", offset, "error", err)
			return false
		}
		if len(result.Beads) == 0 {
			break
		}

		p.mu.Lock()
		for _, b := range result.Beads {
			jiraKey := b.Fields["jira_key"]
			if jiraKey != "" {
				p.tracked[jiraKey] = b.ID
				count++
			}
		}
		p.mu.Unlock()

		offset += len(result.Beads)
		if len(result.Beads) < pageSize {
			break
		}
	}

	p.mu.Lock()
	p.caughtUp = true
	p.mu.Unlock()

	p.cfg.Logger.Info("JIRA poller catch-up complete", "tracked", count)
	return true
}

// catchUpWithRetry runs CatchUp until it succeeds or the retry budget
// elapses. During a deploy window the beads daemon typically becomes
// reachable within seconds of the bridge starting, so the common case is one
// or two retries. If the budget elapses the poller stays NOT caught-up: bead
// creation remains blocked and each poll cycle retries via ensureCaughtUp.
func (p *JiraPoller) catchUpWithRetry(ctx context.Context) {
	deadline := time.Now().Add(p.catchUpRetryBudget)
	for {
		if p.CatchUp(ctx) {
			return
		}
		if time.Now().After(deadline) {
			p.cfg.Logger.Error("JIRA poller catch-up: retry budget exhausted — bead creation stays blocked until a poll-cycle catch-up succeeds",
				"budget", p.catchUpRetryBudget)
			return
		}
		p.cfg.Logger.Warn("JIRA poller catch-up: beads daemon unavailable, retrying",
			"interval", p.catchUpRetryInterval)
		select {
		case <-ctx.Done():
			return
		case <-time.After(p.catchUpRetryInterval):
		}
	}
}

// isCaughtUp reports whether a CatchUp pass has fully succeeded since startup.
func (p *JiraPoller) isCaughtUp() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.caughtUp
}

// ensureCaughtUp retries catch-up once if it has not yet succeeded. Called at
// the top of every poll cycle so a poller that started while the beads daemon
// was down converges as soon as the daemon is reachable. Returns whether the
// poller is caught up (and may therefore create beads).
func (p *JiraPoller) ensureCaughtUp(ctx context.Context) bool {
	if p.isCaughtUp() {
		return true
	}
	return p.CatchUp(ctx)
}

// fullScan paginates through all JIRA results using nextPageToken cursor
// to import issues missed by the single-page poll.
//
// Fail-closed: refuses to run before a successful catch-up — scanning with an
// unseeded tracked map would create a duplicate bead for every JQL-matching
// issue (ka-0000001931). The scan is deferred instead; the first caught-up
// poll cycle runs it.
func (p *JiraPoller) fullScan(ctx context.Context) {
	if !p.isCaughtUp() {
		p.mu.Lock()
		p.pendingFullScan = true
		p.mu.Unlock()
		p.cfg.Logger.Warn("JIRA full scan deferred: catch-up has not succeeded yet")
		return
	}

	jql := p.buildJQL()
	fields := p.requestFields()

	totalCreated := 0
	totalSkipped := 0
	pageNum := 0
	var allIssues []JiraIssue
	pageToken := ""

	for {
		result, err := p.jira.SearchIssuesPage(ctx, jql, fields, 50, pageToken)
		if err != nil {
			// Retry once on rate limit.
			if strings.Contains(err.Error(), "429") {
				p.cfg.Logger.Warn("JIRA full scan rate limited, backing off", "page", pageNum)
				select {
				case <-ctx.Done():
					break
				case <-time.After(5 * time.Second):
				}
				result, err = p.jira.SearchIssuesPage(ctx, jql, fields, 50, pageToken)
			}
			if err != nil {
				p.cfg.Logger.Error("JIRA full scan page failed", "page", pageNum, "error", err)
				break
			}
		}

		if len(result.Issues) == 0 {
			break
		}

		for _, issue := range result.Issues {
			p.mu.Lock()
			_, exists := p.tracked[issue.Key]
			p.mu.Unlock()

			if exists {
				totalSkipped++
				continue
			}

			beadID, err := p.createBeadFromIssue(ctx, issue)
			if err != nil {
				p.cfg.Logger.Error("failed to create bead for JIRA issue",
					"key", issue.Key, "error", err)
				continue
			}

			p.mu.Lock()
			p.tracked[issue.Key] = beadID
			p.mu.Unlock()
			totalCreated++

			p.cfg.Logger.Info("created bead for JIRA issue",
				"key", issue.Key, "bead_id", beadID,
				"summary", issue.Fields.Summary)
		}

		allIssues = append(allIssues, result.Issues...)
		pageNum++

		// No more pages.
		if result.NextPageToken == "" {
			break
		}
		pageToken = result.NextPageToken

		// Pace to avoid rate limiting.
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}

	p.wireDependencies(ctx, allIssues)

	// Count startup-scan materialization so a fresh pod that catches up via the
	// full scan and then goes quiet does not read as "0 beads ever created"
	// (jira_poller_stall.go).
	if totalCreated > 0 {
		jiraPollerBeadsCreated.Add(float64(totalCreated))
	}

	p.cfg.Logger.Info("JIRA full scan complete",
		"created", totalCreated, "skipped", totalSkipped, "pages", pageNum)
}

// poll executes a single JIRA poll cycle.
//
// Fail-closed: skipped entirely until a catch-up pass has succeeded — creating
// beads with an unseeded tracked map duplicates every matching issue
// (ka-0000001931). ensureCaughtUp retries the catch-up each cycle, so the
// poller converges as soon as the beads daemon is reachable; a deferred
// startup full scan then runs before the regular poll.
func (p *JiraPoller) poll(ctx context.Context) {
	if !p.ensureCaughtUp(ctx) {
		// Fail-closed catch-up gate not yet passed. This is a liveness concern
		// (the deadman leg watches for the loop ticking at all), not a
		// materialization stall — clear the stall verdict so a legitimately
		// blocked startup (ka-0000001931) cannot false-page.
		jiraPollerPolls.WithLabelValues(pollResultNotCaughtUp).Inc()
		jiraPollerStalled.Set(0)
		p.cfg.Logger.Warn("JIRA poll skipped: catch-up has not succeeded yet")
		return
	}

	p.mu.Lock()
	runFullScan := p.pendingFullScan
	runTriggerCatchUp := p.pendingTriggerCatchUp
	p.pendingFullScan = false
	p.pendingTriggerCatchUp = false
	p.mu.Unlock()
	if runTriggerCatchUp && p.triggers != nil {
		p.triggers.CatchUp(ctx)
	}
	if runFullScan {
		p.fullScan(ctx)
	}

	// Refresh tracked from the daemon at the start of each poll. This makes
	// the poller idempotent across restarts — even after a successful
	// catch-up, poll self-heals drift by checking live data. Note
	// ListTaskBeads only returns non-closed beads, so this is a belt on top
	// of the full catch-up, not a replacement.
	if beads, err := p.daemon.ListTaskBeads(ctx); err == nil {
		p.mu.Lock()
		for _, b := range beads {
			// Only self-heal from genuine Jira mirrors — beads carrying the
			// source:jira label. A bare jira_key FIELD is not sufficient: this
			// admits the bead into the assignee-back-off close machinery
			// (checkAssigneeBatch), so an unrelated task bead that merely
			// happens to hold a jira_key field must never be enrolled, or the
			// poller could later close it. Mirrors CatchUp's source:jira
			// scoping. A correctly PROMOTED dark bead is stamped source:jira +
			// jira_key + jira:<KEY> together (`gb jira-dark stamp`), so it is
			// still adopted here — not duplicated (ka-0000005212).
			if !hasLabel(b.Labels, "source:jira") {
				continue
			}
			if key := b.Fields["jira_key"]; key != "" {
				if _, exists := p.tracked[key]; !exists {
					p.tracked[key] = b.ID
				}
			}
		}
		p.mu.Unlock()
	}

	jql := p.buildJQL()
	fields := p.requestFields()

	issues, err := p.jira.SearchIssues(ctx, jql, fields, 50)
	if err != nil {
		// A search error is a Jira-side/transient failure, not the silent
		// materialization stall this gauge tracks — clear the verdict and let
		// the loud error log (and, if persistent, the loop's own liveness)
		// carry it. The poll is still counted so the deadman sees the loop
		// ticking.
		jiraPollerPolls.WithLabelValues(pollResultSearchError).Inc()
		jiraPollerStalled.Set(0)
		p.cfg.Logger.Error("JIRA poll failed", "error", err)
		return
	}

	created := 0
	skipped := 0
	for _, issue := range issues {
		p.mu.Lock()
		_, exists := p.tracked[issue.Key]
		p.mu.Unlock()

		if exists {
			skipped++
			continue
		}

		beadID, err := p.createBeadFromIssue(ctx, issue)
		if err != nil {
			p.cfg.Logger.Error("failed to create bead for JIRA issue",
				"key", issue.Key, "error", err)
			continue
		}

		p.mu.Lock()
		p.tracked[issue.Key] = beadID
		p.mu.Unlock()
		created++

		p.cfg.Logger.Info("created bead for JIRA issue",
			"key", issue.Key, "bead_id", beadID,
			"summary", issue.Fields.Summary)

	}

	// Account materialization + the stall verdict for this cycle
	// (jira_poller_stall.go). recordPollOutcome counts the ok poll and, on a
	// zero-row cycle, probes approximate-count to decide whether matter is
	// sitting upstream unmaterialized.
	if created > 0 {
		jiraPollerBeadsCreated.Add(float64(created))
	}
	p.recordPollOutcome(ctx, jql, len(issues))

	// Second pass: wire dependencies now that all beads from this batch exist.
	p.wireDependencies(ctx, issues)

	if created > 0 || p.cfg.Logger.Enabled(ctx, slog.LevelDebug) {
		p.cfg.Logger.Info("JIRA poll complete",
			"found", len(issues), "created", created, "skipped", skipped)
	}

	// Third pass: check for assignee changes on tracked tickets.
	if p.cfg.SkipAssigned {
		p.checkAssigneeChanges(ctx)
	}

	// Fourth pass: evaluate JQL trigger beads.
	if p.triggers != nil {
		p.triggers.Evaluate(ctx)
	}
}

// checkAssigneeChanges queries JIRA for tracked issues that now have a human
// assignee and closes the corresponding beads. This implements the "back off
// when a human takes over" behavior.
func (p *JiraPoller) checkAssigneeChanges(ctx context.Context) {
	p.mu.Lock()
	trackedKeys := make([]string, 0, len(p.tracked))
	for key := range p.tracked {
		trackedKeys = append(trackedKeys, key)
	}
	p.mu.Unlock()

	if len(trackedKeys) == 0 {
		return
	}

	// Batch keys into groups of 50 to avoid oversized JQL queries.
	const batchSize = 50
	for i := 0; i < len(trackedKeys); i += batchSize {
		end := i + batchSize
		if end > len(trackedKeys) {
			end = len(trackedKeys)
		}
		batch := trackedKeys[i:end]
		p.checkAssigneeBatch(ctx, batch)
	}
}

// checkAssigneeBatch checks a batch of JIRA keys for human assignee changes.
func (p *JiraPoller) checkAssigneeBatch(ctx context.Context, keys []string) {
	// Build JQL: key IN (...) AND assignee IS NOT EMPTY
	jql := "key IN (" + quoteJQL(keys) + ") AND assignee IS NOT EMPTY"

	issues, err := p.jira.SearchIssues(ctx, jql, []string{"assignee"}, len(keys))
	if err != nil {
		p.cfg.Logger.Debug("assignee check query failed", "error", err)
		return
	}

	for _, issue := range issues {
		if issue.Fields.Assignee == nil {
			continue
		}

		// Skip if the assignee is the bot itself.
		if p.cfg.BotAccountID != "" && issue.Fields.Assignee.AccountID == p.cfg.BotAccountID {
			continue
		}

		p.mu.Lock()
		beadID, exists := p.tracked[issue.Key]
		p.mu.Unlock()

		if !exists {
			continue
		}

		// Check if the bead is still open/in_progress before closing.
		result, err := p.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
			Labels:   []string{"jira:" + issue.Key},
			Statuses: []string{"open", "in_progress"},
			Limit:    1,
		})
		if err != nil || len(result.Beads) == 0 {
			continue
		}

		// Close the bead that actually OWNS the jira:<KEY> label, and only if
		// it is the same bead the tracked map points at. For every natively-
		// ingested mirror AND for a correctly-promoted dark bead
		// (`gb jira-dark stamp` writes jira_key + jira:<KEY> onto the same
		// bead), tracked[key] and the jira:<KEY> label-owner ARE the same
		// bead. If they DIVERGE — a stray jira_key field, a mis-stamp, a
		// re-parented key — fail closed: skip the close so the poller can never
		// close a bead that does not own this issue's key (ka-0000005212,
		// refuted self-heal/close-path finding jira_poller.go:302-311,421,443).
		labelOwnerID := result.Beads[0].ID
		if labelOwnerID != beadID {
			p.cfg.Logger.Warn("assignee back-off close skipped: tracked bead != jira-label owner",
				"key", issue.Key, "tracked_bead", beadID, "label_owner", labelOwnerID)
			continue
		}

		assigneeName := issue.Fields.Assignee.DisplayName
		p.cfg.Logger.Info("human assigned to JIRA issue, backing off",
			"key", issue.Key, "bead_id", beadID, "assignee", assigneeName)

		// Close the bead with a reason.
		if err := p.daemon.CloseBead(ctx, beadID, map[string]string{
			"close_reason":     "human_assigned",
			"jira_assignee":    assigneeName,
			"jira_assignee_id": issue.Fields.Assignee.AccountID,
		}); err != nil {
			p.cfg.Logger.Error("failed to close bead for human-assigned issue",
				"key", issue.Key, "bead_id", beadID, "error", err)
			continue
		}

		// Post a comment to JIRA so the human knows gardenio backed off —
		// but only for projects agents are permitted to write back to.
		// Without this guard a permissive JIRA_PROJECTS / JIRA_CUSTOM_JQL
		// scope (e.g. FSM) would leak agent comments onto a customer-facing
		// service desk that the write policy marks non-writable. The bead
		// close and tracked-delete below still run so we don't re-import the
		// issue — only the outbound comment is suppressed (ka-0000008042).
		//
		// The project verdict comes from the config-driven write policy (with the
		// legacy coordinated gate as the unwired fallback): FSM is a toggle-gated
		// project in that policy, so the poller stops/starts FSM write-back in
		// lockstep with the other three paths (ka-0000001679, ka-0000007252).
		// Bead→ticket scoping: the comment targets issue.Key, which must equal
		// the bound jira_key of the tracked bead we just closed — fail-closed if
		// the tracked bead's binding has drifted from the polled issue, so the
		// poller can only comment on the ticket the bead actually owns.
		//
		// NOT field-allowlist-gated, and deliberately so (ka-0000004831, gap 2).
		// This is a roleless/system back-off note: it is triggered by a JIRA-side
		// event (a human took the ticket), its body is a fixed bridge-internal
		// string with NO acting-agent role, and there is no per-role policy to
		// resolve. Routing it through the per-role AllowlistGate would mean
		// inventing a synthetic role, which is the wrong shape. Instead it is
		// pinned to an INTERNAL note via AddCommentWithVisibility(CommentInternal)
		// so that — like every other governed comment write — it can never land
		// as a customer-facing reply on a JSM project, even though the project
		// gate already restricts it to PE/DEVOPS. No agent-attributable governed
		// write is reachable through this path; the field gate covers the three
		// agent-driven paths (jira_sync, jira_squawk_forward, jira_mentions).
		// Asserted by TestCheckAssigneeBatch_BackoffComment_IsInternalNote.
		boundKey := boundJiraKeyFromFields(result.Beads[0].Fields, result.Beads[0].Labels)
		switch {
		case !p.allowProject(issue.Key):
			p.cfg.Logger.Info("back-off comment suppressed: project blocked",
				"key", issue.Key, "writable_projects", p.projectGateDiag())
		case !jiraWriteInScope(boundKey, issue.Key):
			p.cfg.Logger.Warn("back-off comment suppressed: out of scope (bound key != polled issue)",
				"key", issue.Key, "bead_id", beadID, "bound_jira_key", boundKey)
		default:
			comment := "Gardenio is backing off — " + assigneeName + " has been assigned to this issue."
			// Stamp operator attribution ("Acted by <operator>") so the Jira
			// history records which human drove the tracked bead despite the
			// shared service account (epic ka-0000004904 decision #4 /
			// ka-0000006003). result.Beads[0] is a daemon-resolved BeadDetail, so
			// prefer the typed OperatedBy/OperatedByDisplay and fall back to the
			// operated_by field when the typed value is empty.
			trackedBead := result.Beads[0]
			operatedBy := trackedBead.OperatedBy
			if operatedBy == "" {
				operatedBy = trackedBead.Fields["operated_by"]
			}
			comment = withAttribution(comment, operatorAttributionLine(operatedBy, trackedBead.OperatedByDisplay))
			if err := p.jira.AddCommentWithVisibility(ctx, issue.Key, comment, CommentInternal); err != nil {
				p.cfg.Logger.Warn("failed to post back-off comment to JIRA",
					"key", issue.Key, "error", err)
			}
		}

		// Remove from tracked so we don't re-import it.
		p.mu.Lock()
		delete(p.tracked, issue.Key)
		p.mu.Unlock()
	}
}

// createBeadFromIssue creates a task bead from a JIRA issue.
// It performs a server-side duplicate check before creating to guard against
// races and CatchUp gaps that previously caused ~9x duplication.
func (p *JiraPoller) createBeadFromIssue(ctx context.Context, issue JiraIssue) (string, error) {
	// Server-side duplicate check: query for existing bead with same jira: label.
	// FAIL-CLOSED on error (ka-0000001931): during a deploy window the beads
	// daemon can become reachable between this query and the CreateBead call
	// below — proceeding on a failed check turned exactly that race into a
	// duplicate bead per issue. Skipping this cycle is safe: the issue is
	// retried on the next poll.
	existing, err := p.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Labels: []string{"jira:" + issue.Key},
		Limit:  1,
	})
	if err != nil {
		return "", fmt.Errorf("duplicate check for %s: %w", issue.Key, err)
	}
	if len(existing.Beads) > 0 {
		// Already exists — update tracked map and return existing ID.
		beadID := existing.Beads[0].ID
		p.mu.Lock()
		p.tracked[issue.Key] = beadID
		p.mu.Unlock()
		return beadID, nil
	}

	// Build labels.
	labels := []string{
		"source:jira",
		"jira:" + issue.Key,
	}

	// Extract project key from issue key (e.g., "PE" from "PE-7001") and map
	// to the boat project name via ProjectMap. Falls back to the lowercased
	// JIRA prefix when no mapping is configured.
	project := ""
	if parts := strings.SplitN(issue.Key, "-", 2); len(parts) == 2 {
		jiraPrefix := strings.ToUpper(parts[0])
		boatProject, ok := p.cfg.ProjectMap[jiraPrefix]
		if !ok {
			boatProject = strings.ToLower(jiraPrefix)
		}
		project = jiraPrefix
		labels = append(labels, "project:"+boatProject)
	}

	isEpic := issue.Fields.IssueType != nil && issue.Fields.IssueType.Name == "Epic"

	// Add JIRA labels with prefix.
	for _, l := range issue.Fields.Labels {
		labels = append(labels, "jira-label:"+l)
	}

	// Tag epics for graph visibility.
	if issue.Fields.IssueType != nil && issue.Fields.IssueType.Name == "Epic" {
		labels = append(labels, "jira-epic")
	}

	// Build fields.
	fields := map[string]string{
		"jira_key":     issue.Key,
		"jira_project": strings.ToUpper(project),
		"jira_url":     p.jira.baseURL + "/browse/" + issue.Key,
	}
	if issue.Fields.IssueType != nil {
		fields["jira_type"] = issue.Fields.IssueType.Name
	}
	if issue.Fields.Status != nil {
		fields["jira_status"] = issue.Fields.Status.Name
	}
	if issue.Fields.Parent != nil {
		fields["jira_epic"] = issue.Fields.Parent.Key
	}
	if issue.Fields.Reporter != nil {
		fields["jira_reporter"] = issue.Fields.Reporter.DisplayName
		// The accountId/email (not the display name) are the resolvable keys
		// mapping a reporter to a fleet human — carried on mirror beads so the
		// reporter attribution is available downstream (ka-0000001781).
		if issue.Fields.Reporter.AccountID != "" {
			fields["jira_reporter_id"] = issue.Fields.Reporter.AccountID
		}
		if issue.Fields.Reporter.EmailAddress != "" {
			fields["jira_reporter_email"] = issue.Fields.Reporter.EmailAddress
		}
	}
	if issue.Fields.Assignee != nil {
		fields["jira_assignee"] = issue.Fields.Assignee.DisplayName
		fields["jira_assignee_id"] = issue.Fields.Assignee.AccountID
	}

	// Issue link count for graph visibility.
	if len(issue.Fields.IssueLinks) > 0 {
		fields["jira_link_count"] = fmt.Sprintf("%d", len(issue.Fields.IssueLinks))
	}

	// Attachment metadata for media indicators.
	if len(issue.Fields.Attachments) > 0 {
		fields["jira_attachment_count"] = fmt.Sprintf("%d", len(issue.Fields.Attachments))
		var hasImages, hasVideo bool
		for _, att := range issue.Fields.Attachments {
			mime := strings.ToLower(att.MimeType)
			if strings.HasPrefix(mime, "image/") {
				hasImages = true
			}
			if strings.HasPrefix(mime, "video/") {
				hasVideo = true
			}
		}
		if hasImages {
			fields["jira_has_images"] = "true"
		}
		if hasVideo {
			fields["jira_has_video"] = "true"
		}
		// Always tag as media when attachments exist — the search/jql
		// endpoint often omits mimeType from attachment stubs, so the
		// image/video checks above may not fire even when screenshots
		// are present.
		labels = append(labels, "jira-has-media")
	}

	// Promote configured custom fields (e.g. the FSM "URL" field) onto the bead.
	p.applyCustomFields(ctx, issue, fields)

	// Map priority. Epics get priority=1 (high) since they're higher-level.
	priority := 2 // default medium
	if isEpic {
		priority = 1
	} else if issue.Fields.Priority != nil {
		priority = MapJiraPriority(issue.Fields.Priority.Name)
	}

	// Convert description from ADF to markdown.
	description := adfToMarkdown(issue.Fields.Description)

	// Fetch remote links (GitLab MRs, etc.) and append to description.
	mrLinks := p.fetchMRLinks(ctx, issue.Key)
	if len(mrLinks) > 0 {
		fields["mr_url"] = mrLinks[0].url
		var sb strings.Builder
		sb.WriteString("\n\n## Linked MRs\n\n")
		for _, mr := range mrLinks {
			fmt.Fprintf(&sb, "- [%s](%s) — %s\n", mr.title, mr.url, mr.status)
		}
		description += sb.String()
	}

	fieldsJSON, err := json.Marshal(fields)
	if err != nil {
		return "", fmt.Errorf("marshal fields: %w", err)
	}

	title := fmt.Sprintf("[%s] %s", issue.Key, issue.Fields.Summary)

	beadID, err := p.daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:       title,
		Type:        "task",
		Kind:        "issue",
		Description: description,
		Labels:      labels,
		Priority:    priority,
		CreatedBy:   "jira-bridge",
		Fields:      fieldsJSON,
	})
	if err != nil {
		return "", fmt.Errorf("create bead: %w", err)
	}

	return beadID, nil
}

// applyCustomFields promotes JIRA custom fields onto the bead's field map.
//
// Configured CustomFields are written as scalar bead fields under their mapped
// names (e.g. customfield_11976 -> jira_issue_url). When CaptureAllCustom is
// set, every populated customfield_* value is additionally collected into a
// single jira_custom_fields JSON blob keyed by human-readable field name.
func (p *JiraPoller) applyCustomFields(ctx context.Context, issue JiraIssue, fields map[string]string) {
	if len(issue.RawFields) == 0 {
		return
	}

	for id, beadField := range p.cfg.CustomFields {
		raw, ok := issue.RawFields[id]
		if !ok {
			continue
		}
		if v, ok := CustomFieldString(raw); ok {
			fields[beadField] = v
		}
	}

	if !p.cfg.CaptureAllCustom {
		return
	}

	names := p.customFieldNames(ctx)
	custom := make(map[string]string)
	for id, raw := range issue.RawFields {
		if !strings.HasPrefix(id, "customfield_") {
			continue
		}
		v, ok := CustomFieldString(raw)
		if !ok {
			continue
		}
		name := names[id]
		if name == "" {
			name = id
		}
		custom[name] = v
	}
	if len(custom) > 0 {
		if blob, err := json.Marshal(custom); err == nil {
			fields["jira_custom_fields"] = string(blob)
		}
	}
}

// customFieldNames returns the cached customfield-id → human-name map, lazily
// fetching and refreshing it from JIRA at most once per hour.
func (p *JiraPoller) customFieldNames(ctx context.Context) map[string]string {
	p.mu.Lock()
	cached := p.fieldNames
	freshUntil := p.fieldNamesAt.Add(time.Hour)
	p.mu.Unlock()

	if cached != nil && time.Now().Before(freshUntil) {
		return cached
	}

	names, err := p.jira.ListFields(ctx)
	if err != nil {
		p.cfg.Logger.Warn("failed to fetch JIRA field names for custom-field capture", "error", err)
		if cached != nil {
			return cached
		}
		return map[string]string{}
	}

	p.mu.Lock()
	p.fieldNames = names
	p.fieldNamesAt = time.Now()
	p.mu.Unlock()
	return names
}

// mrLinkInfo holds a parsed MR link from JIRA remote links.
type mrLinkInfo struct {
	url    string
	title  string
	status string
}

// fetchMRLinks fetches remote links from JIRA and returns those that look
// like merge request URLs (GitLab or GitHub).
func (p *JiraPoller) fetchMRLinks(ctx context.Context, issueKey string) []mrLinkInfo {
	links, err := p.jira.GetRemoteLinks(ctx, issueKey)
	if err != nil {
		p.cfg.Logger.Debug("failed to fetch remote links", "key", issueKey, "error", err)
		return nil
	}

	var mrs []mrLinkInfo
	for _, link := range links {
		u := link.Object.URL
		if u == "" {
			continue
		}
		if !isMRURL(u) {
			continue
		}
		title := link.Object.Title
		if title == "" {
			title = u
		}
		status := "unknown"
		if link.Object.Status != nil {
			if link.Object.Status.Icon != nil && link.Object.Status.Icon.Title != "" {
				status = normalizeMRState(link.Object.Status.Icon.Title)
			} else if link.Object.Status.Resolved {
				status = "merged"
			} else {
				status = "opened"
			}
		}
		mrs = append(mrs, mrLinkInfo{url: u, title: title, status: status})
	}
	return mrs
}

// ghPRPattern matches GitHub pull request URLs.
var ghPRPattern = regexp.MustCompile(`^https?://[^/]+/[^/]+/[^/]+/pull/\d+`)

// isMRURL returns true if the URL is a valid merge request or pull request URL.
// Uses ParseMRURL (gitlab.go) for GitLab MRs and a regex for GitHub PRs.
func isMRURL(u string) bool {
	return ParseMRURL(u) != nil || ghPRPattern.MatchString(u)
}

// normalizeMRState maps JIRA remote link status labels to canonical mr_state
// values used by the gitlab-bridge (opened, closed, merged, locked).
func normalizeMRState(title string) string {
	switch strings.ToLower(title) {
	case "merged":
		return "merged"
	case "open", "opened":
		return "opened"
	case "closed":
		return "closed"
	case "locked":
		return "locked"
	default:
		return strings.ToLower(title)
	}
}

// wireDependencies wires child-of and issue-link edges for a batch of issues.
func (p *JiraPoller) wireDependencies(ctx context.Context, issues []JiraIssue) {
	p.mu.Lock()
	snapshot := make(map[string]string, len(p.tracked))
	for k, v := range p.tracked {
		snapshot[k] = v
	}
	p.mu.Unlock()

	wired := 0

	// Pass A: child-of edges (issue with Parent → parent bead).
	for _, issue := range issues {
		if issue.Fields.Parent == nil {
			continue
		}
		childID, childOK := snapshot[issue.Key]
		parentID, parentOK := snapshot[issue.Fields.Parent.Key]
		if !childOK || !parentOK {
			continue
		}
		dedupKey := childID + ":child-of:" + parentID
		if p.wiredDeps[dedupKey] {
			continue
		}
		if err := p.daemon.AddDependency(ctx, childID, parentID, "child-of", "jira-bridge"); err != nil {
			p.cfg.Logger.Warn("failed to wire child-of dependency",
				"child", issue.Key, "parent", issue.Fields.Parent.Key, "error", err)
			// Mark as wired to stop retrying — the target bead may have been
			// deleted, and the error will recur every sync cycle otherwise.
			p.wiredDeps[dedupKey] = true
			continue
		}
		p.wiredDeps[dedupKey] = true
		wired++
	}

	// Pass B: issue link edges.
	// Collect cross-project / unresolvable links per source bead.
	xlinks := make(map[string][]string) // bead ID → []"type:KEY"
	for _, issue := range issues {
		srcID, srcOK := snapshot[issue.Key]
		if !srcOK {
			continue
		}
		for _, link := range issue.Fields.IssueLinks {
			depType := MapJiraLinkType(link.Type.Name)

			// Determine the target issue key.
			var targetKey string
			if link.OutwardIssue != nil {
				targetKey = link.OutwardIssue.Key
			} else if link.InwardIssue != nil {
				targetKey = link.InwardIssue.Key
			}
			if targetKey == "" {
				continue
			}

			targetID, targetOK := snapshot[targetKey]
			if !targetOK {
				// Target not imported — store as cross-project link field.
				xlinks[srcID] = append(xlinks[srcID], depType+":"+targetKey)
				continue
			}

			dedupKey := srcID + ":" + depType + ":" + targetID
			if p.wiredDeps[dedupKey] {
				continue
			}
			if err := p.daemon.AddDependency(ctx, srcID, targetID, depType, "jira-bridge"); err != nil {
				p.cfg.Logger.Warn("failed to wire issue link dependency",
					"source", issue.Key, "target", targetKey, "type", depType, "error", err)
				// Mark as wired to stop retrying on every sync cycle.
				p.wiredDeps[dedupKey] = true
				continue
			}
			p.wiredDeps[dedupKey] = true
			wired++
		}
	}

	// Store cross-project links as a field on each source bead.
	// Dedup: skip if the same value was already written (avoids no-op bead
	// updates that pollute the activity feed on every poll cycle).
	for beadID, links := range xlinks {
		value := strings.Join(links, ",")
		dedupKey := beadID + ":xlinks:" + value
		if p.wiredDeps[dedupKey] {
			continue
		}
		if err := p.daemon.UpdateBeadFields(ctx, beadID, map[string]string{
			"jira_xlinks": value,
		}); err != nil {
			p.cfg.Logger.Warn("failed to store cross-project links",
				"bead", beadID, "links", links, "error", err)
		}
		p.wiredDeps[dedupKey] = true
	}

	if wired > 0 {
		p.cfg.Logger.Info("wired JIRA dependencies", "count", wired)
	}
}

// baseRequestFields is the set of system fields the poller always requests.
var baseRequestFields = []string{
	"summary", "description", "status", "issuetype", "priority",
	"reporter", "assignee", "labels", "parent", "created", "updated",
	"issuelinks", "attachment",
}

// requestFields returns the JIRA field list to request for each issue. When
// CaptureAllCustom is set it requests "*all" so every custom field is returned;
// otherwise it requests the base system fields plus any explicitly-configured
// custom-field ids.
func (p *JiraPoller) requestFields() []string {
	if p.cfg.CaptureAllCustom {
		return []string{"*all"}
	}
	if len(p.cfg.CustomFields) == 0 {
		return baseRequestFields
	}
	fields := make([]string, len(baseRequestFields), len(baseRequestFields)+len(p.cfg.CustomFields))
	copy(fields, baseRequestFields)
	for id := range p.cfg.CustomFields {
		fields = append(fields, id)
	}
	return fields
}

// buildJQL constructs the JQL query for polling.
//
// When FilterProjects is set, fixVersion and requiredLabels filters are scoped
// to only those projects. Other projects in the Projects list pass through
// without those filters. This allows project-specific triage labels (e.g.,
// PE requires "approved-to-work" + fixVersion "2.5.0") without blocking
// other projects (e.g., DEVOPS) that don't use those fields.
func (p *JiraPoller) buildJQL() string {
	var parts []string

	hasPerProjectFilters := len(p.cfg.FilterProjects) > 0 &&
		(len(p.cfg.FixVersions) > 0 || len(p.cfg.RequiredLabels) > 0)

	if hasPerProjectFilters {
		// Build a compound project clause:
		//   (project IN ("PE") AND fixVersion IN (...) AND labels = "...")
		//   OR project IN ("DEVOPS", ...)
		var filteredClause strings.Builder
		filteredClause.WriteString("(project IN (" + quoteJQL(p.cfg.FilterProjects) + ")")
		if len(p.cfg.FixVersions) > 0 {
			filteredClause.WriteString(" AND fixVersion IN (" + quoteJQL(p.cfg.FixVersions) + ")")
		}
		for _, label := range p.cfg.RequiredLabels {
			filteredClause.WriteString(` AND labels = "` + label + `"`)
		}
		filteredClause.WriteString(")")

		// Find unfiltered projects (in Projects but not in FilterProjects).
		filterSet := make(map[string]bool, len(p.cfg.FilterProjects))
		for _, fp := range p.cfg.FilterProjects {
			filterSet[strings.ToUpper(fp)] = true
		}
		var unfiltered []string
		for _, proj := range p.cfg.Projects {
			if !filterSet[strings.ToUpper(proj)] {
				unfiltered = append(unfiltered, proj)
			}
		}

		if len(unfiltered) > 0 {
			parts = append(parts, "("+filteredClause.String()+" OR project IN ("+quoteJQL(unfiltered)+"))")
		} else {
			parts = append(parts, filteredClause.String())
		}
	} else {
		// No per-project scoping — apply filters globally (original behavior).
		if len(p.cfg.Projects) > 0 {
			parts = append(parts, "project IN ("+quoteJQL(p.cfg.Projects)+")")
		}
		if len(p.cfg.FixVersions) > 0 {
			parts = append(parts, "fixVersion IN ("+quoteJQL(p.cfg.FixVersions)+")")
		}
		for _, label := range p.cfg.RequiredLabels {
			parts = append(parts, "labels = "+`"`+label+`"`)
		}
	}

	if len(p.cfg.Statuses) > 0 {
		parts = append(parts, "status IN ("+quoteJQL(p.cfg.Statuses)+")")
	}
	if len(p.cfg.IssueTypes) > 0 {
		parts = append(parts, "issuetype IN ("+quoteJQL(p.cfg.IssueTypes)+")")
	}
	for _, label := range p.cfg.ExcludedLabels {
		parts = append(parts, "labels != "+`"`+label+`"`)
	}
	if p.cfg.SkipAssigned {
		parts = append(parts, "assignee is EMPTY")
	}
	if p.cfg.CustomJQL != "" {
		parts = append(parts, "("+p.cfg.CustomJQL+")")
	}

	jql := strings.Join(parts, " AND ")
	jql += " ORDER BY created DESC"
	return jql
}

// quoteJQL wraps each value in double quotes for JQL IN clauses.
func quoteJQL(values []string) string {
	quoted := make([]string, len(values))
	for i, v := range values {
		quoted[i] = `"` + v + `"`
	}
	return strings.Join(quoted, ",")
}

// hasLabel is defined in chat.go — reused here for label filtering.
