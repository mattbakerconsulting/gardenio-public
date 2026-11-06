// Package bridge provides the GitLab MR merge watcher.
//
// GitLabSync subscribes to kbeads SSE bead events, watches for beads with
// mr_url fields pointing to GitLab MRs, and queries GitLab to detect merges.
// When an MR is merged, it sets mr_merged=true on the bead.
package bridge

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/forgebeads"
)

// gitlabMaxBodyBytes bounds GitLab webhook request bodies. GitLab payloads
// (merge_request, note, pipeline, emoji) can include large issue descriptions
// and diff notes; 2 MiB is a generous cap that still prevents memory
// exhaustion from an attacker-controlled body. See AUDIT-2026-04-19 §J2.
const gitlabMaxBodyBytes = 2 << 20 // 2 MiB

// GitLabBeadClient is the subset of beadsapi.Client used by GitLabSync.
type GitLabBeadClient interface {
	ListTaskBeads(ctx context.Context) ([]*beadsapi.BeadDetail, error)
	ListTaskBeadsAll(ctx context.Context) ([]*beadsapi.BeadDetail, error)
	// ListTaskBeadsForMR returns task beads (all statuses, incl. closed) bound
	// to one MR via fields.mr_url. The note webhook path uses this instead of
	// ListTaskBeadsAll so a single MR comment does not drag the entire task
	// corpus into memory (ka-0000007917).
	ListTaskBeadsForMR(ctx context.Context, mrURL string) ([]*beadsapi.BeadDetail, error)
	ListActiveAgentBeadDetails(ctx context.Context) ([]*beadsapi.BeadDetail, error)
	UpdateBeadFields(ctx context.Context, beadID string, fields map[string]string) error
	AddComment(ctx context.Context, beadID, author, text string) error
	// UpsertMergeRequestBead creates or updates a type=merge-request bead keyed
	// by canonical MR URL. Returns the bead ID. See epic ka-0000001613 and
	// task ka-0000007548.
	UpsertMergeRequestBead(ctx context.Context, f beadsapi.MergeRequestBeadFields, closed bool) (string, error)
	// AddDependency links two beads with a typed dependency edge. Used by the
	// bridge to attach a `produces` edge from the producing task bead to the
	// MR bead.
	AddDependency(ctx context.Context, beadID, dependsOnID, depType, createdBy string) error
	// MRBeadForTask walks task → merge-request via the `produces` dep edge.
	// Used to read MR state from the canonical MR bead instead of the legacy
	// task-bead mr_* fields. See epic ka-0000001613.
	MRBeadForTask(ctx context.Context, taskBeadID string) (*beadsapi.BeadDetail, error)
}

// GitLabSync watches for MR merges and updates bead fields.
type GitLabSync struct {
	gitlab   *GitLabClient
	daemon   GitLabBeadClient
	logger   *slog.Logger
	nudge    NudgeFunc
	resolver *AgentResolver   // optional; when set, handles agent lookup/reuse for review nudges
	users    GitLabUserLookup // optional; resolves MR author → human operator for operated_by attribution

	// viewerBaseURL / jiraBaseURL deep-link the Bead / JIRA lines in the managed
	// MR-description block. Empty falls back to plain text. See
	// docs/design/bead-linking-in-gitlab.md (Gap 1a).
	viewerBaseURL string
	jiraBaseURL   string

	// host scopes SSE-driven MR handling to MR URLs on this exact GitLab host.
	// In the dual-GitLab world (one bridge per host sharing ONE beads daemon +
	// SSE stream — R2 artifact §7), an unscoped bridge would react to the OTHER
	// host's mr_urls: query its own (wrong) GitLab for a foreign IID and PUT a
	// bogus description, or mint a produces-edge whose MR bead it can never
	// track. host is derived from the GitLab client's base URL at construction.
	// Empty = match every host (preserves single-bridge prod behavior; this is
	// the FW-1 foundation that makes #17-#22 safe). See hostMatches.
	host string

	mu   sync.Mutex
	seen map[string]time.Time // dedup key → last check time
}

// NudgeFunc is an optional callback for sending nudges to agents.
type NudgeFunc func(ctx context.Context, agentName, message string) error

// GitLabSyncConfig holds configuration for the GitLab sync watcher.
type GitLabSyncConfig struct {
	GitLab   *GitLabClient
	Daemon   GitLabBeadClient
	Logger   *slog.Logger
	Nudge    NudgeFunc        // optional; if nil, nudges are skipped
	Resolver *AgentResolver   // optional; when set, handles agent lookup/reuse for review nudges
	Users    GitLabUserLookup // optional; resolves MR author → human operator for operated_by attribution

	// ViewerBaseURL / JIRABaseURL deep-link the Bead / JIRA lines in the managed
	// MR-description block. Both optional; empty falls back to plain text.
	ViewerBaseURL string
	JIRABaseURL   string

	// Host scopes SSE-driven MR handling to MR URLs on this exact GitLab host.
	// Empty = handle every host (single-bridge deployments; the default). Set
	// it (typically to the GitLab client's own host) when running one bridge
	// per GitLab instance against a SHARED beads daemon + SSE stream so this
	// bridge ignores the other host's mr_urls. See GitLabSync.host / the R2
	// artifact §7 "Dual-GitLab structural note" (FW-1 / ka-0000006221).
	Host string
}

// NewGitLabSync creates a new GitLab MR sync watcher.
func NewGitLabSync(cfg GitLabSyncConfig) *GitLabSync {
	return &GitLabSync{
		gitlab:   cfg.GitLab,
		daemon:   cfg.Daemon,
		logger:   cfg.Logger,
		nudge:    cfg.Nudge,
		resolver: cfg.Resolver,
		users:    cfg.Users,

		viewerBaseURL: cfg.ViewerBaseURL,
		jiraBaseURL:   cfg.JIRABaseURL,

		// Scope to the configured GitLab host so this bridge ignores the other
		// host's MR URLs on the shared SSE stream. Empty (the default) = match
		// all hosts, preserving single-bridge behavior. main.go sets this to
		// the GitLab client's own host for the dark deployment.
		host: cfg.Host,

		seen: make(map[string]time.Time),
	}
}

// hostMatches reports whether an MR URL on the given host should be handled by
// this bridge. An empty configured host matches everything (single-bridge
// deployments, and tests). Otherwise the URL's host must equal the configured
// host exactly — this is the guard that keeps the dark bridge from acting on
// upstream MR URLs and vice versa when both share one beads daemon + SSE
// stream (R2 artifact §7; FW-1 / ka-0000006221).
func (s *GitLabSync) hostMatches(ref *MRRef) bool {
	if s.host == "" {
		return true
	}
	return ref != nil && ref.Host == s.host
}

// RegisterHandlers registers SSE event handlers on the given stream.
// Watches for bead updates where mr_url is set — triggers MR status check,
// MR description sync, and review comment nudges.
func (s *GitLabSync) RegisterHandlers(stream *SSEStream) {
	stream.On("beads.bead.updated", s.handleUpdated)
	stream.On("beads.bead.updated", s.handleDescriptionSync)
	stream.On("beads.bead.updated", s.handleReviewNudge)
	s.logger.Info("GitLab sync watcher registered SSE handlers",
		"topics", []string{"beads.bead.updated"})
}

// handleUpdated checks if a bead's mr_url points to a GitLab MR and queries
// its merge status. If merged, sets mr_merged=true on the bead.
func (s *GitLabSync) handleUpdated(ctx context.Context, data []byte) {
	// Service-identity writes — see ka-0000005187 / decision ka-0000001054.
	// Bridge runs under a fleet token; kbeads audit attributes writes as
	// fleet:gitlab-bridge automatically.
	bead := ParseBeadEvent(data)
	if bead == nil {
		return
	}

	// Skip type=merge-request beads. An MR bead carries its own mr_url field,
	// so without this guard handleUpdated re-enters on every MR-bead update and
	// calls upsertMRBeadAndLink(taskBeadID=the-MR-bead) — writing a bogus
	// produces(MR→MR) self-loop and never the genuine produces(task→MR) edge.
	// Only task/bug/feature/epic beads legitimately drive the MR-upsert path.
	// See root-cause ka-0000003609 / fix ka-0000004161.
	if bead.Type == "merge-request" {
		return
	}

	mrURL := bead.Fields["mr_url"]
	if mrURL == "" {
		return
	}

	// Only process GitLab MR URLs.
	ref := ParseMRURL(mrURL)
	if ref == nil {
		return
	}

	// Host scope: ignore MR URLs that belong to a different GitLab host. With
	// one bridge per host on a shared SSE stream, this prevents the dark bridge
	// from querying its own GitLab for an upstream IID (and vice versa). See
	// hostMatches / R2 artifact §7 (FW-1 / ka-0000006221).
	if !s.hostMatches(ref) {
		s.logger.Debug("skipping MR on foreign GitLab host",
			"bead", bead.ID, "mr_host", ref.Host, "bridge_host", s.host)
		return
	}

	// Skip if the linked MR bead already records the MR as merged. Reading
	// from the MR bead is the canonical source-of-truth after the
	// dual-write removal (epic ka-0000001613).
	if taskMRMerged(ctx, s.daemon, bead.ID, s.logger) {
		return
	}

	// Dedup: don't re-check the same MR too frequently.
	dedupKey := "gitlab-mr:" + bead.ID + ":" + mrURL
	if s.isDuplicate(dedupKey) {
		return
	}

	s.logger.Info("checking GitLab MR status",
		"bead", bead.ID, "mr_url", mrURL, "project", ref.ProjectPath, "iid", ref.IID)

	mr, err := s.gitlab.GetMergeRequestByPath(ctx, ref.ProjectPath, ref.IID)
	if err != nil {
		s.logger.Error("failed to get GitLab MR",
			"bead", bead.ID, "mr_url", mrURL, "error", err)
		return
	}

	if mr.State == "merged" {
		s.logger.Info("MR merged — upserting MR bead",
			"bead", bead.ID, "mr_url", mrURL)
	}

	// Upsert the type=merge-request bead and link it to the producing task
	// bead via a `produces` dep. The MR bead is the sole source of truth for
	// mr_pipeline_status, mr_pipeline_url, and gitlab_project_id; the
	// residual task-bead writes were dropped per task ka-0000000586
	// (following ka-0000008134 which dropped mr_state/mr_merged/gitlab_mr_iid).
	mrFields := beadsapi.MergeRequestBeadFields{
		MRURL:           mrURL,
		MRIID:           strconv.Itoa(mr.IID),
		ProjectPath:     ref.ProjectPath,
		SourceBranch:    mr.SourceBranch,
		TargetBranch:    mr.TargetBranch,
		MRState:         mr.State,
		Title:           mr.Title,
		GitLabProjectID: strconv.Itoa(mr.ProjectID),
	}
	if mr.HeadPipeline != nil {
		mrFields.PipelineStatus = mr.HeadPipeline.Status
		mrFields.PipelineURL = mr.HeadPipeline.WebURL
	}
	if mr.Author != nil {
		mrFields.Author = mr.Author.Username
		// Default the MR bead's operator to its GitLab creator, resolved
		// through the user directory to the human (slack:/oidc:) when known
		// (ka-0000004473).
		mrFields.OperatedBy = operatorFromGitLabUsername(ctx, s.users, mr.Author.Username, mr.Author.ID)
	}
	closed := mr.State == "merged" || mr.State == "closed"
	s.upsertMRBeadAndLink(ctx, bead.ID, mrFields, closed)
}

// upsertMRBeadAndLink upserts the MR bead and adds a `produces` dep from
// taskBeadID → MR bead. Best-effort — errors are logged. taskBeadID may be
// empty when there is no producing task bead (e.g. ad-hoc MRs seen by the
// poller or webhook), in which case the dep step is skipped.
func (s *GitLabSync) upsertMRBeadAndLink(ctx context.Context, taskBeadID string, f beadsapi.MergeRequestBeadFields, closed bool) {
	mrBeadID, err := s.daemon.UpsertMergeRequestBead(ctx, f, closed)
	if err != nil {
		s.logger.Error("failed to upsert MR bead",
			"task_bead", taskBeadID, "mr_url", f.MRURL, "error", err)
		return
	}
	if taskBeadID == "" {
		return
	}
	// produces edge plus the auto-link M2b (epic ka-0000000933) reverse
	// triggered-by edge, so reverse-graph queries can walk from any MR bead
	// back to every work bead that produced it.
	if err := forgebeads.LinkTaskToMR(ctx, s.daemon, taskBeadID, mrBeadID, "system:gitlab-sync"); err != nil {
		s.logger.Warn("failed to link task to MR bead",
			"task_bead", taskBeadID, "mr_bead", mrBeadID, "error", err)
	}
}

// mrDescFields are the bead fields that feed into the MR description section.
var mrDescFields = map[string]bool{
	"jira_key":           true,
	"jira_status":        true,
	"mr_state":           true,
	"mr_pipeline_status": true,
	"mr_pipeline_url":    true,
	"mr_approved":        true,
	"mr_approvers":       true,
}

// mrDescTriggerFields is the set of field-changes that trigger a live
// description re-sync. It is mrDescFields plus mr_url: linking an MR (setting
// mr_url) is itself a reason to write the managed "Agent Context" section.
// Without mr_url here, a bead whose mr_url is set but whose display fields never
// subsequently change (common for bead-linked MRs in sibling group repos, where
// the bridge sees no pipeline/jira events) would never get the section on the
// live SSE path — only via the boot-time catch-up. That was Gap A of
// ka-0000001471 (task ka-0000004869).
var mrDescTriggerFields = func() map[string]bool {
	m := map[string]bool{"mr_url": true}
	for k := range mrDescFields {
		m[k] = true
	}
	return m
}()

// beadLinkTypes are the bead types whose mr_url gets the gardenio-managed
// "Agent Context" section. The section is repo-agnostic and host-scoped, so any
// work bead that can carry an mr_url must be covered — not just type=task. A
// type=bug or type=feature bead whose MR lives in a sibling group repo
// (e.g. the type=bug bead carrying site-deployment!1234, which had no marker)
// was silently skipped before. Closes Gap A of ka-0000001471 (task
// ka-0000004869).
var beadLinkTypes = map[string]bool{
	"task":    true,
	"bug":     true,
	"feature": true,
}

// beadLinkTypeList is beadLinkTypes as a slice for server-side type filters.
var beadLinkTypeList = []string{"task", "bug", "feature"}

// handleDescriptionSync updates the gardenio-managed section of the GitLab MR
// description when relevant bead fields change.
func (s *GitLabSync) handleDescriptionSync(ctx context.Context, data []byte) {
	if s.gitlab == nil {
		return
	}

	bead := ParseBeadEvent(data)
	if bead == nil || !beadLinkTypes[bead.Type] {
		return
	}

	mrURL := bead.Fields["mr_url"]
	if mrURL == "" {
		return
	}

	ref := ParseMRURL(mrURL)
	if ref == nil {
		return
	}

	// Host scope: never PUT a description to a foreign GitLab host (FW-1 /
	// ka-0000006221; see hostMatches).
	if !s.hostMatches(ref) {
		return
	}

	// Only sync when a relevant field changed. mr_url counts (see
	// mrDescTriggerFields) so freshly-linked MRs get the managed section on the
	// live path instead of waiting for the next boot catch-up.
	if bead.Changes != nil {
		relevant := false
		for k := range bead.Changes {
			if mrDescTriggerFields[k] {
				relevant = true
				break
			}
		}
		if !relevant {
			return
		}
	}

	_ = s.syncDescriptionForBead(ctx, bead.ID, mrURL, ref, bead.Fields)
}

// syncDescriptionForBead PUTs the gardenio-managed section into the MR
// description for one bead. It is shared by the SSE handler and the startup
// catch-up. The write is idempotent — syncMRDescription replaces exactly the
// managed block between its markers, so re-running it on every boot with
// unchanged fields produces byte-identical output and no external corruption
// (bug ka-0000002703, Finding 2). Dedup coalesces a catch-up with any
// concurrent SSE replay within the TTL window.
func (s *GitLabSync) syncDescriptionForBead(ctx context.Context, beadID, mrURL string, ref *MRRef, fields map[string]string) error {
	// Dedup: don't re-sync too frequently.
	dedupKey := "gitlab-mrdesc:" + beadID
	if s.isDuplicate(dedupKey) {
		return nil
	}

	// Read mr_state from the canonical MR bead (epic ka-0000001613). When no
	// MR bead is linked yet, the section omits the MR state line.
	mrState := ""
	if mrBead, err := s.daemon.MRBeadForTask(ctx, beadID); err == nil && mrBead != nil {
		mrState = mrBead.Fields["mr_state"]
	} else if err != nil {
		s.logger.Debug("MR bead lookup failed for description sync",
			"bead", beadID, "error", err)
	}

	section := MRDescriptionSection{
		BeadID:         beadID,
		JIRAKey:        fields["jira_key"],
		JIRAStatus:     fields["jira_status"],
		PipelineStatus: fields["mr_pipeline_status"],
		PipelineURL:    fields["mr_pipeline_url"],
		Approved:       fields["mr_approved"],
		Approvers:      fields["mr_approvers"],
		MRState:        mrState,
		ViewerBaseURL:  s.viewerBaseURL,
		JIRABaseURL:    s.jiraBaseURL,
	}

	if err := syncMRDescription(ctx, s.gitlab, ref.ProjectPath, ref.IID, section, s.logger); err != nil {
		// WARN+ so a silent miss (a bead-linked MR that never got the managed
		// section because its GitLab description-PUT kept failing) surfaces in
		// logs instead of being swallowed. task ka-0000004869.
		s.logger.Error("failed to sync MR description",
			"bead", beadID, "mr_url", mrURL, "project", ref.ProjectPath, "mr_iid", ref.IID, "error", err)
		return err
	}
	return nil
}

// GitLabSyncCatchUpClient is the subset of beadsapi.Client needed by the
// startup description catch-up: listing active task beads to re-sync.
type GitLabSyncCatchUpClient interface {
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
}

// activeBeadStatuses is the set of non-closed statuses the description catch-up
// enumerates. An MR whose work bead has closed no longer needs its managed
// section re-synced on boot, so closed beads are excluded to bound the fan-out.
var activeBeadStatuses = []string{"open", "in_progress", "blocked", "deferred"}

// CatchUpDescriptions re-runs the MR-description sync for every active
// bead-linked bead (task/bug/feature, per beadLinkTypeList) that already carries
// an mr_url, closing the gap where the SSE stream (which starts from the
// persisted Last-Event-ID, or "now" on a fresh-state boot) never delivers a
// bead.updated event for MRs that existed before this pod started. Such beads
// otherwise miss the managed "Agent Context" section until the next unrelated
// bead.updated tick touches them.
//
// This doubles as the idempotent back-fill reconcile for Gap A (task
// ka-0000004869): any active bead-linked MR with no gardenio marker gets the
// managed section spliced in. syncMRDescription is a no-op when the section is
// already present and unchanged, so re-running it over bug/feature/task beads is
// safe. The enumeration was previously type=task ONLY, which is why a bead-linked
// bug/feature MR in a sibling group repo (e.g. site-deployment!1234, carried by
// a type=bug bead) never received the section.
//
// This is SAFE to run on every boot: syncMRDescription is an idempotent PUT of
// the managed block, with no external-corruption risk (unlike the wl-bridge
// completion write) — which is why THIS gap can take a jira-style catch-up
// directly, per bug ka-0000002703 (Finding 2). Host scope and per-bead dedup are
// honored via syncDescriptionForBead. Errors are logged per-bead at WARN+ and
// counted, never aborting the pass; limit bounds the fan-out so a restart never
// issues an unbounded burst of GitLab PUTs.
func (s *GitLabSync) CatchUpDescriptions(ctx context.Context, client GitLabSyncCatchUpClient, limit int) {
	if s.gitlab == nil || client == nil {
		return
	}
	if limit <= 0 {
		limit = 512
	}

	result, err := client.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    beadLinkTypeList,
		Statuses: activeBeadStatuses,
		Limit:    limit,
	})
	if err != nil {
		s.logger.Warn("gitlab description catch-up: failed to list beads", "error", err)
		return
	}

	synced := 0
	failed := 0
	for _, bead := range result.Beads {
		if ctx.Err() != nil {
			s.logger.Warn("gitlab description catch-up: context canceled mid-pass", "error", ctx.Err())
			return
		}
		mrURL := bead.Fields["mr_url"]
		if mrURL == "" {
			continue
		}
		ref := ParseMRURL(mrURL)
		if ref == nil {
			continue
		}
		// Host scope: never PUT a description to a foreign GitLab host (FW-1 /
		// ka-0000006221; see hostMatches).
		if !s.hostMatches(ref) {
			continue
		}
		if err := s.syncDescriptionForBead(ctx, bead.ID, mrURL, ref, bead.Fields); err != nil {
			failed++
			continue
		}
		synced++
	}

	s.logger.Info("gitlab description catch-up complete",
		"beads", len(result.Beads), "synced", synced, "failed", failed)
}

// handleReviewNudge nudges the assigned agent when mr_has_review_comments
// changes to true on a bead. When an AgentResolver is configured, it handles
// agent lookup/reuse: if the original agent is alive it gets nudged; if dead,
// a new agent is spawned with the MR context.
func (s *GitLabSync) handleReviewNudge(ctx context.Context, data []byte) {
	if s.nudge == nil && s.resolver == nil {
		return
	}
	// Service-identity writes — see ka-0000005187. Bridge runs under a fleet
	// token; audit attributes writes as fleet:gitlab-bridge automatically.
	bead := ParseBeadEvent(data)
	if bead == nil || bead.Type != "task" {
		return
	}

	// Only nudge when mr_has_review_comments just changed.
	if bead.Changes == nil {
		return
	}
	if _, ok := bead.Changes["mr_has_review_comments"]; !ok {
		return
	}
	if bead.Fields["mr_has_review_comments"] != "true" {
		return
	}

	// Forge scope: github-bridge sets mr_has_review_comments on beads whose PR
	// lives on GitHub and mails the agent itself. Without this the flag write
	// would also reach here, where a non-GitLab mr_url falls through the host
	// scope below as match-all, and the agent would be nudged twice.
	if bead.Fields["forge"] == "github" {
		return
	}

	// Host scope: only the bridge owning this MR's GitLab host nudges, so a
	// review on a dark MR doesn't fire a duplicate nudge from the prod bridge
	// and vice versa (FW-1 / ka-0000006221; see hostMatches). An mr_url that
	// doesn't parse falls through to match-all so legacy/non-URL cases behave
	// as before.
	if ref := ParseMRURL(bead.Fields["mr_url"]); ref != nil && !s.hostMatches(ref) {
		return
	}

	assignee := bead.Assignee
	if assignee == "" {
		return
	}

	// Guard: don't nudge/spawn for merged MRs. Source of truth is the linked
	// MR bead (epic ka-0000001613).
	if taskMRMerged(ctx, s.daemon, bead.ID, s.logger) {
		return
	}

	// Guard: don't nudge/spawn for closed task beads.
	if bead.Status == "closed" || bead.Status == "done" {
		return
	}

	// Dedup: don't nudge the same agent too frequently for the same bead.
	dedupKey := "gitlab-review-nudge:" + bead.ID
	if s.isDuplicate(dedupKey) {
		return
	}

	message := fmt.Sprintf("MR has new review comments — address them: %s", bead.Fields["mr_url"])

	// When the resolver is available, use it for agent lookup/reuse.
	// This handles dead agents by spawning new ones with MR context.
	if s.resolver != nil {
		if err := s.resolver.ResolveAndNudge(ctx, *bead, message); err != nil {
			s.logger.Error("failed to resolve/nudge agent for review comments",
				"bead", bead.ID, "agent", assignee, "error", err)
		} else {
			s.logger.Info("resolved and nudged agent for MR review comments",
				"bead", bead.ID, "agent", assignee)
		}
		return
	}

	// Fallback: simple nudge by agent name (no agent reuse).
	if err := s.nudge(ctx, assignee, message); err != nil {
		s.logger.Error("failed to nudge agent for review comments",
			"bead", bead.ID, "agent", assignee, "error", err)
	} else {
		s.logger.Info("nudged agent for MR review comments",
			"bead", bead.ID, "agent", assignee)
	}
}

// taskMRMerged reports whether the task bead's linked MR bead records the MR
// as merged. Returns false when no MR bead is linked yet (e.g. a freshly
// mentioned task) or the lookup errors. This replaces reads of the legacy
// task-bead mr_merged field, which is no longer written. Epic ka-0000001613 /
// task ka-0000008134.
func taskMRMerged(ctx context.Context, c GitLabBeadClient, taskBeadID string, logger *slog.Logger) bool {
	if c == nil || taskBeadID == "" {
		return false
	}
	mrBead, err := c.MRBeadForTask(ctx, taskBeadID)
	if err != nil {
		if logger != nil {
			logger.Debug("MR bead lookup for merge check failed",
				"task", taskBeadID, "error", err)
		}
		return false
	}
	if mrBead == nil {
		return false
	}
	return mrBead.Fields["mr_outcome"] == "merged" || mrBead.Fields["mr_state"] == "merged"
}

// isDuplicate returns true if the key was seen within the last 5 minutes.
func (s *GitLabSync) isDuplicate(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	// Periodic cleanup.
	for k, t := range s.seen {
		if now.Sub(t) > 5*time.Minute {
			delete(s.seen, k)
		}
	}

	if t, ok := s.seen[key]; ok && now.Sub(t) < 5*time.Minute {
		return true
	}
	s.seen[key] = now
	return false
}

// GitLabPoller periodically polls GitLab for recently merged MRs and updates
// matching beads. This is the fallback for when webhooks don't fire.
type GitLabPoller struct {
	gitlab       *GitLabClient
	daemon       GitLabBeadClient
	logger       *slog.Logger
	groupID      int
	pollInterval time.Duration
	lastPoll     time.Time
	users        GitLabUserLookup // optional; resolves MR author → human operator for operated_by attribution
}

// GitLabPollerConfig holds configuration for the GitLab poller.
type GitLabPollerConfig struct {
	GitLab       *GitLabClient
	Daemon       GitLabBeadClient
	Logger       *slog.Logger
	GroupID      int
	PollInterval time.Duration
	Users        GitLabUserLookup // optional; resolves MR author → human operator for operated_by attribution
}

// NewGitLabPoller creates a new GitLab MR polling fallback.
func NewGitLabPoller(cfg GitLabPollerConfig) *GitLabPoller {
	return &GitLabPoller{
		gitlab:       cfg.GitLab,
		daemon:       cfg.Daemon,
		logger:       cfg.Logger,
		groupID:      cfg.GroupID,
		pollInterval: cfg.PollInterval,
		lastPoll:     time.Now().Add(-cfg.PollInterval), // poll immediately on first run
		users:        cfg.Users,
	}
}

// Run starts the polling loop. It blocks until ctx is canceled.
func (p *GitLabPoller) Run(ctx context.Context) error {
	p.logger.Info("starting GitLab MR poller",
		"group_id", p.groupID, "poll_interval", p.pollInterval)

	// Initial poll.
	p.poll(ctx)

	ticker := time.NewTicker(p.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			p.poll(ctx)
		}
	}
}

// poll checks GitLab for recently merged MRs and updates matching beads.
func (p *GitLabPoller) poll(ctx context.Context) {
	// Service-identity writes — see ka-0000005187. Bridge runs under a fleet
	// token; audit attributes writes as fleet:gitlab-bridge automatically.
	since := p.lastPoll
	now := time.Now()

	mrs, err := p.gitlab.ListMergedMRs(ctx, p.groupID, since)
	if err != nil {
		// Leave p.lastPoll unchanged so the failed window [since, now] is
		// re-examined on the next tick. Advancing the cursor here would
		// permanently skip any MR merged during a failed poll — and a
		// transient GitLab API error is correlated with the same instability
		// that drops merge webhooks, i.e. exactly when this fallback is
		// needed most (ka-0000007753). Upserts are idempotent and
		// taskMRMerged skips already-merged beads, so the re-scan overlap on
		// the next successful tick is harmless.
		p.logger.Error("GitLab poll failed", "error", err)
		return
	}
	// Commit the cursor only after a successful fetch.
	p.lastPoll = now

	if len(mrs) == 0 {
		p.logger.Debug("no recently merged MRs", "since", since)
		return
	}

	p.logger.Info("found recently merged MRs", "count", len(mrs), "since", since)

	// Load beads with mr_url to match against.
	beads, err := p.daemon.ListTaskBeads(ctx)
	if err != nil {
		p.logger.Error("failed to list task beads", "error", err)
		return
	}

	// Build mr_url → bead index.
	urlIndex := make(map[string]*beadsapi.BeadDetail)
	for _, b := range beads {
		if u := b.Fields["mr_url"]; u != "" {
			urlIndex[u] = b
		}
	}

	// Match merged MRs to beads.
	for _, mr := range mrs {
		bead, ok := urlIndex[mr.WebURL]
		if !ok {
			continue
		}

		// Skip already-merged beads. Source of truth is the linked MR bead
		// (epic ka-0000001613) — the legacy task-bead mr_merged field is no
		// longer written.
		if taskMRMerged(ctx, p.daemon, bead.ID, p.logger) {
			continue
		}

		p.logger.Info("poll: MR merged — upserting MR bead",
			"bead", bead.ID, "mr_url", mr.WebURL, "mr_iid", mr.IID)

		// Upsert the MR bead and link it to the producing task bead. The MR
		// bead is the sole source of truth for mr_pipeline_status,
		// mr_pipeline_url, and gitlab_project_id; the residual task-bead
		// writes were dropped per task ka-0000000586.
		mrFields := beadsapi.MergeRequestBeadFields{
			MRURL:           mr.WebURL,
			MRIID:           strconv.Itoa(mr.IID),
			SourceBranch:    mr.SourceBranch,
			TargetBranch:    mr.TargetBranch,
			MRState:         "merged",
			MROutcome:       "merged",
			Title:           mr.Title,
			GitLabProjectID: strconv.Itoa(mr.ProjectID),
		}
		if mr.HeadPipeline != nil {
			mrFields.PipelineStatus = mr.HeadPipeline.Status
			mrFields.PipelineURL = mr.HeadPipeline.WebURL
		}
		if mr.Author != nil {
			mrFields.Author = mr.Author.Username
			// Default the MR bead's operator to its GitLab creator
			// (ka-0000004473). On the poll path the MR bead usually already
			// exists, so operated_by was stamped at create time; setting it
			// here keeps poll-first MRs attributed too.
			mrFields.OperatedBy = operatorFromGitLabUsername(ctx, p.users, mr.Author.Username, mr.Author.ID)
		}
		mrBeadID, uerr := p.daemon.UpsertMergeRequestBead(ctx, mrFields, true)
		if uerr != nil {
			p.logger.Error("failed to upsert MR bead from poll",
				"bead", bead.ID, "mr_url", mr.WebURL, "error", uerr)
			continue
		}
		if derr := forgebeads.LinkTaskToMR(ctx, p.daemon, bead.ID, mrBeadID, "system:gitlab-poller"); derr != nil {
			p.logger.Warn("failed to link task to MR bead from poll",
				"task_bead", bead.ID, "mr_bead", mrBeadID, "error", derr)
		}
	}
}

// GitLabWebhookConfig holds configuration for the GitLab webhook handler.
type GitLabWebhookConfig struct {
	GitLab         *GitLabClient
	Daemon         GitLabBeadClient
	WebhookSecret  string
	BotUsername    string           // GitLab username of the bot; notes from this user are ignored to prevent loops
	Nudge          NudgeFunc        // optional; if set, review comments nudge the assigned agent with rich context
	AgentResolver  *AgentResolver   // optional; if set, spawns agents for review comments when no running agent found
	MentionHandler *MentionHandler  // optional; if set, @gardenio mentions spawn a new agent
	Users          GitLabUserLookup // optional; resolves MR author → human operator for operated_by attribution
	// AcceptanceReconciler, when set, emits the R5 reverse-provenance acceptance
	// signal after an upstream peel MR bead is closed: it stamps the producing
	// peel-request bead(s) with upstream_accepted_at / upstream_mr_url /
	// upstream_merged_by and never closes any dark work bead (E-PROV FW-3,
	// ka-0000002343). nil on the dark bridge / single-host deployments.
	AcceptanceReconciler *UpstreamAcceptanceReconciler
	// AIApproval, when set, applies a required human-approval rule to every
	// AI-authored MR at open/reopen time (DEVOPS-730). nil when the feature is
	// disabled — the enforcer is opt-in and a nil enforcer is a no-op.
	AIApproval *AIApprovalEnforcer
	Logger     *slog.Logger
}

// GitLabWebhookHandlerWithConfig returns an http.Handler using the full config.
func GitLabWebhookHandlerWithConfig(cfg GitLabWebhookConfig) http.Handler {
	daemon := cfg.Daemon
	logger := cfg.Logger
	webhookSecret := cfg.WebhookSecret
	botUsername := cfg.BotUsername
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// started times this delivery for the handling-latency histogram
		// (webhook_latency.go). Taken as the very first statement so the
		// measurement covers the whole handler — including the secret compare
		// and the body read, which is where a reject-path regression would show.
		started := time.Now()
		// Every terminal path records exactly one outcome on
		// gardenio_gitlab_webhook_deliveries_total (gitlab_webhook_metrics.go) so
		// a still-arriving-but-rejected stream (token_failed/bad_request) is
		// distinguishable from silence — the receive-path liveness signal for
		// the GitLab door of the non-AVA webhook carve-out (ka-0000005632, see
		// gardenio/docs/security/non-ava-webhook-carveout.md). Mirrors the Jira
		// bridge's recordWebhookOutcome (jira_webhook_deadman.go, bug
		// ka-0000000426).
		//
		// Verify webhook secret — fail-closed when no secret is configured.
		if webhookSecret == "" {
			recordGitLabWebhookOutcome(gitlabWebhookOutcomeTokenFailed, started)
			logger.Error("webhook: GITLAB_WEBHOOK_SECRET not configured, rejecting request")
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		gotToken := r.Header.Get("X-Gitlab-Token")
		if len(gotToken) != len(webhookSecret) || subtle.ConstantTimeCompare([]byte(gotToken), []byte(webhookSecret)) != 1 {
			recordGitLabWebhookOutcome(gitlabWebhookOutcomeTokenFailed, started)
			logger.Warn("webhook: invalid X-Gitlab-Token")
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}

		if r.Method != http.MethodPost {
			recordGitLabWebhookOutcome(gitlabWebhookOutcomeBadRequest, started)
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}

		// Read body upfront so we can peek at object_kind before full decode.
		// Bound the read with MaxBytesReader so an unbounded body cannot exhaust
		// memory. See AUDIT-2026-04-19 §J2 (ka-0000005263).
		r.Body = http.MaxBytesReader(w, r.Body, gitlabMaxBodyBytes)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			recordGitLabWebhookOutcome(gitlabWebhookOutcomeBadRequest, started)
			logger.Error("webhook: failed to read body", "error", err)
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}

		var peek struct {
			ObjectKind string `json:"object_kind"`
		}
		if err := json.Unmarshal(body, &peek); err != nil {
			recordGitLabWebhookOutcome(gitlabWebhookOutcomeBadRequest, started)
			logger.Error("webhook: failed to peek at object_kind", "error", err)
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}

		// Emoji events have a different payload structure — handle separately.
		if peek.ObjectKind == "emoji" {
			handleEmojiWebhook(r.Context(), body, cfg.GitLab, logger)
			recordGitLabWebhookOutcome(gitlabWebhookOutcomeProcessed, started)
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{"status":"processed","kind":"emoji"}`)
			return
		}

		var event struct {
			ObjectKind string `json:"object_kind"`
			User       struct {
				ID       int    `json:"id"`
				Username string `json:"username"`
			} `json:"user"`
			ObjectAttr struct {
				IID       int    `json:"iid"`
				State     string `json:"state"`
				Action    string `json:"action"`
				URL       string `json:"url"`
				ProjectID int    `json:"target_project_id"`
				// merge_request-specific fields.
				Title        string `json:"title"`
				SourceBranch string `json:"source_branch"`
				TargetBranch string `json:"target_branch"`
				// Pipeline-specific fields (when object_kind=pipeline).
				ID     int    `json:"id"`
				Status string `json:"status"`
				// Note-specific fields (when object_kind=note).
				Note         string `json:"note"`
				NoteableType string `json:"noteable_type"`
				System       bool   `json:"system"`
				DiscussionID string `json:"discussion_id"`
				Position     *struct {
					NewPath string `json:"new_path"`
					NewLine int    `json:"new_line"`
					OldPath string `json:"old_path"`
					OldLine int    `json:"old_line"`
				} `json:"position"`
			} `json:"object_attributes"`
			Project *struct {
				PathWithNamespace string `json:"path_with_namespace"`
			} `json:"project"`
			MergeRequest *webhookMR `json:"merge_request"`
		}

		if err := json.Unmarshal(body, &event); err != nil {
			recordGitLabWebhookOutcome(gitlabWebhookOutcomeBadRequest, started)
			logger.Error("webhook: failed to decode body", "error", err)
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}

		switch event.ObjectKind {
		case "push":
			handlePushWebhook(r.Context(), body, cfg.Nudge, daemon, botUsername, logger)
			recordGitLabWebhookOutcome(gitlabWebhookOutcomeProcessed, started)
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{"status":"processed","kind":"push"}`)
			return
		case "note":
			var pos *notePosition
			if event.ObjectAttr.Position != nil {
				pos = &notePosition{
					NewPath: event.ObjectAttr.Position.NewPath,
					NewLine: event.ObjectAttr.Position.NewLine,
					OldPath: event.ObjectAttr.Position.OldPath,
					OldLine: event.ObjectAttr.Position.OldLine,
				}
			}
			nc := noteContext{
				NoteableType: event.ObjectAttr.NoteableType,
				Note:         event.ObjectAttr.Note,
				System:       event.ObjectAttr.System,
				Author:       event.User.Username,
				AuthorID:     event.User.ID,
				BotUsername:  botUsername,
				Position:     pos,
				DiscussionID: event.ObjectAttr.DiscussionID,
				MR:           event.MergeRequest,
			}
			handleNoteWebhook(r.Context(), nc, cfg.Nudge, cfg.AgentResolver, cfg.MentionHandler, daemon, logger)
			recordGitLabWebhookOutcome(gitlabWebhookOutcomeProcessed, started)
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{"status":"processed","kind":"note"}`)
			return
		case "pipeline":
			handlePipelineWebhook(r.Context(), event.ObjectAttr.ID, event.ObjectAttr.Status, event.ObjectAttr.URL, event.MergeRequest, daemon, logger)
			recordGitLabWebhookOutcome(gitlabWebhookOutcomeProcessed, started)
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{"status":"processed","kind":"pipeline"}`)
			return
		case "merge_request":
			switch event.ObjectAttr.Action {
			case "merge":
				// Fall through to merge handling below (which also upserts).
			case "approved", "unapproved":
				handleApprovalWebhook(r.Context(), event.ObjectAttr.URL, event.ObjectAttr.Action, event.User.Username, daemon, logger)
				recordGitLabWebhookOutcome(gitlabWebhookOutcomeProcessed, started)
				w.WriteHeader(http.StatusOK)
				fmt.Fprintf(w, `{"status":"processed","action":"%s"}`, event.ObjectAttr.Action)
				return
			case "open", "reopen", "update", "close":
				projectPath := ""
				if event.Project != nil {
					projectPath = event.Project.PathWithNamespace
				}
				if cfg.AIApproval != nil && (event.ObjectAttr.Action == "open" || event.ObjectAttr.Action == "reopen") {
					if err := cfg.AIApproval.EnsureRule(r.Context(), projectPath, event.ObjectAttr.IID, event.User.Username); err != nil {
						logger.Warn("webhook: failed to ensure AI-approval rule",
							"mr_url", event.ObjectAttr.URL, "error", err)
					}
				}
				handleMergeRequestUpsertWebhook(r.Context(), mrUpsertContext{
					MRURL:        event.ObjectAttr.URL,
					IID:          event.ObjectAttr.IID,
					ProjectID:    event.ObjectAttr.ProjectID,
					ProjectPath:  projectPath,
					State:        event.ObjectAttr.State,
					Action:       event.ObjectAttr.Action,
					Title:        event.ObjectAttr.Title,
					SourceBranch: event.ObjectAttr.SourceBranch,
					TargetBranch: event.ObjectAttr.TargetBranch,
					Author:       event.User.Username,
					OperatedBy:   operatorFromGitLabUsername(r.Context(), cfg.Users, event.User.Username, event.User.ID),
				}, daemon, logger)
				recordGitLabWebhookOutcome(gitlabWebhookOutcomeProcessed, started)
				w.WriteHeader(http.StatusOK)
				fmt.Fprintf(w, `{"status":"processed","action":"%s"}`, event.ObjectAttr.Action)
				return
			default:
				recordGitLabWebhookOutcome(gitlabWebhookOutcomeIgnored, started)
				w.WriteHeader(http.StatusOK)
				fmt.Fprintf(w, `{"status":"ignored","reason":"action=%s"}`, event.ObjectAttr.Action)
				return
			}
		default:
			recordGitLabWebhookOutcome(gitlabWebhookOutcomeIgnored, started)
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{"status":"ignored","reason":"kind=%s"}`, event.ObjectKind)
			return
		}

		logger.Info("webhook: MR merged",
			"mr_url", event.ObjectAttr.URL,
			"iid", event.ObjectAttr.IID,
			"project_id", event.ObjectAttr.ProjectID)

		// Find matching bead by mr_url. Service-identity writes — see
		// ka-0000005187; bridge runs under fleet:gitlab-bridge.
		ctx := r.Context()
		beads, err := daemon.ListTaskBeads(ctx)
		if err != nil {
			recordGitLabWebhookOutcome(gitlabWebhookOutcomeError, started)
			logger.Error("webhook: failed to list beads", "error", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}

		matched := false
		var producingBeadID string
		for _, bead := range beads {
			if bead.Fields["mr_url"] == event.ObjectAttr.URL {
				if producingBeadID == "" {
					producingBeadID = bead.ID
				}
				// Skip already-merged beads. Source of truth is the linked
				// MR bead (epic ka-0000001613) — the legacy task-bead
				// mr_merged field is no longer written.
				if taskMRMerged(ctx, daemon, bead.ID, logger) {
					matched = true
					continue // already processed
				}
				logger.Info("webhook: matched task bead for merged MR",
					"bead", bead.ID, "mr_url", event.ObjectAttr.URL)
				// The MR bead is the sole source of truth for gitlab_project_id
				// (upserted below); the residual task-bead write was dropped
				// per task ka-0000000586.
				matched = true
			}
		}

		// Dual-write phase: also upsert the type=merge-request bead and link
		// the producing task bead via a `produces` dep. Best-effort.
		projectPath := ""
		if event.Project != nil {
			projectPath = event.Project.PathWithNamespace
		}
		mrFields := beadsapi.MergeRequestBeadFields{
			MRURL:           event.ObjectAttr.URL,
			MRIID:           strconv.Itoa(event.ObjectAttr.IID),
			ProjectPath:     projectPath,
			SourceBranch:    event.ObjectAttr.SourceBranch,
			TargetBranch:    event.ObjectAttr.TargetBranch,
			MRState:         "merged",
			MROutcome:       "merged",
			Title:           event.ObjectAttr.Title,
			Author:          event.User.Username,
			OperatedBy:      operatorFromGitLabUsername(ctx, cfg.Users, event.User.Username, event.User.ID),
			GitLabProjectID: strconv.Itoa(event.ObjectAttr.ProjectID),
		}
		mrBeadID, uerr := daemon.UpsertMergeRequestBead(ctx, mrFields, true)
		if uerr != nil {
			logger.Error("webhook: failed to upsert MR bead",
				"mr_url", event.ObjectAttr.URL, "error", uerr)
		} else if producingBeadID != "" {
			if derr := forgebeads.LinkTaskToMR(ctx, daemon, producingBeadID, mrBeadID, "system:gitlab-webhook"); derr != nil {
				logger.Warn("webhook: failed to link task to MR bead",
					"task_bead", producingBeadID, "mr_bead", mrBeadID, "error", derr)
			}
		}

		// Reverse-provenance acceptance signal (E-PROV FW-3, ka-0000002343). The
		// MR bead is now closed; if it is an upstream-mirror bead (scope-guarded
		// inside the reconciler), stamp the producing peel-request bead(s) with
		// the acceptance outcome. The merger is the webhook actor. Best-effort;
		// it NEVER closes a dark work bead.
		if cfg.AcceptanceReconciler != nil && mrBeadID != "" {
			mergedBy := operatorFromGitLabUsername(ctx, cfg.Users, event.User.Username, event.User.ID)
			cfg.AcceptanceReconciler.Reconcile(ctx, mrBeadID, mergedBy)
		}

		// A merge with no matching bead is still an authenticated, well-formed
		// delivery we received and acted on (upserted the MR bead) — it counts as
		// receive-path liveness, so record `processed` (not `ignored`) even in
		// the no_match arm. The response body still distinguishes the two.
		recordGitLabWebhookOutcome(gitlabWebhookOutcomeProcessed, started)
		w.WriteHeader(http.StatusOK)
		if matched {
			fmt.Fprintf(w, `{"status":"processed"}`)
		} else {
			fmt.Fprintf(w, `{"status":"no_match","mr_url":"%s"}`, event.ObjectAttr.URL)
		}
	})
}

// mrUpsertContext carries the fields needed by handleMergeRequestUpsertWebhook.
type mrUpsertContext struct {
	MRURL        string
	IID          int
	ProjectID    int
	ProjectPath  string
	State        string // GitLab state: opened, closed, merged, locked
	Action       string // webhook action: open, reopen, update, close
	Title        string
	SourceBranch string
	TargetBranch string
	Author       string
	OperatedBy   string // resolved human-operator id for the MR creator; empty to skip
}

// handleMergeRequestUpsertWebhook upserts a type=merge-request bead in
// response to non-merge MR webhook actions (open/update/reopen/close) and
// links it back to the producing task bead via a `produces` dep when one
// exists. See epic ka-0000001613 / task ka-0000007548.
func handleMergeRequestUpsertWebhook(ctx context.Context, mc mrUpsertContext, daemon GitLabBeadClient, logger *slog.Logger) {
	if mc.MRURL == "" {
		return
	}
	// Service-identity writes — see ka-0000005187; bridge runs under
	// fleet:gitlab-bridge. MR author is preserved in the bead's Author field.

	logger.Info("webhook: MR upsert",
		"mr_url", mc.MRURL, "action", mc.Action, "state", mc.State)

	mrFields := beadsapi.MergeRequestBeadFields{
		MRURL:           mc.MRURL,
		MRIID:           strconv.Itoa(mc.IID),
		ProjectPath:     mc.ProjectPath,
		SourceBranch:    mc.SourceBranch,
		TargetBranch:    mc.TargetBranch,
		MRState:         mc.State,
		Title:           mc.Title,
		Author:          mc.Author,
		OperatedBy:      mc.OperatedBy,
		GitLabProjectID: strconv.Itoa(mc.ProjectID),
	}
	// "close" with state="closed" (no merge) → set mr_outcome=closed.
	closed := mc.Action == "close" && (mc.State == "closed" || mc.State == "merged")
	if closed {
		if mc.State == "merged" {
			mrFields.MROutcome = "merged"
		} else {
			mrFields.MROutcome = "closed"
		}
	}

	mrBeadID, err := daemon.UpsertMergeRequestBead(ctx, mrFields, closed)
	if err != nil {
		logger.Error("webhook: failed to upsert MR bead",
			"mr_url", mc.MRURL, "action", mc.Action, "error", err)
		return
	}

	// Find every producing task bead and link both directions. Multi-task
	// MRs (e.g. a release MR that produces several outputs) need each
	// producer linked, so we no longer break after the first match —
	// every task bead carrying this MR URL gets a `produces` edge plus the
	// reverse `triggered-by` edge. See M2b on epic ka-0000000933 (test 5.9
	// in unit plan ka-0000004136).
	beads, err := daemon.ListTaskBeads(ctx)
	if err != nil {
		logger.Warn("webhook: failed to list beads for produces dep",
			"mr_url", mc.MRURL, "error", err)
		return
	}
	for _, bead := range beads {
		// Only task beads can be producers of an MR — skip the MR bead itself
		// and any other non-task beads that may carry an mr_url field.
		if bead.Type != "task" {
			continue
		}
		if bead.Fields["mr_url"] != mc.MRURL {
			continue
		}
		if derr := forgebeads.LinkTaskToMR(ctx, daemon, bead.ID, mrBeadID, "system:gitlab-webhook"); derr != nil {
			logger.Warn("webhook: failed to link task to MR bead",
				"task_bead", bead.ID, "mr_bead", mrBeadID, "error", derr)
		}
	}
}

// handlePipelineWebhook processes a GitLab pipeline webhook event. It matches
// the pipeline's MR URL to a bead and updates the pipeline status fields.
//
// The pipeline payload's `merge_request` object carries the MR's current
// `state` plus source/target branches. The bridge forwards these to the MR
// bead so a pipeline event that beats the merge_request webhook (or is the
// only event we see for an MR — e.g. when GitLab replays only pipeline
// events) still leaves the MR bead with a populated mr_state. Failing to
// forward `state` was the root cause of the Class A `mr_state` drift
// documented in ka-0000002952 (W-A / ka-0000005367).
func handlePipelineWebhook(ctx context.Context, pipelineID int, status, pipelineURL string, mr *webhookMR, daemon GitLabBeadClient, logger *slog.Logger) {
	if mr == nil || mr.URL == "" {
		logger.Debug("webhook: pipeline event has no merge_request, skipping")
		return
	}
	// Service-identity writes — see ka-0000005187; bridge runs under
	// fleet:gitlab-bridge.

	logger.Info("webhook: pipeline status update",
		"pipeline_id", pipelineID, "status", status, "mr_url", mr.URL, "mr_state", mr.State)

	// Stamp pipeline status onto the type=merge-request bead. The MR bead is
	// the sole source of truth for mr_pipeline_status / mr_pipeline_url; the
	// residual task-bead pipeline writes were dropped per task ka-0000000586.
	//
	// Forward the MR state from the pipeline payload — pipeline events arrive
	// alongside merge_request events (and sometimes ahead of them on cold
	// caches), so the pipeline path must also keep mr_state current. Treat
	// the MR as closed when GitLab reports a terminal state so the bead
	// transitions to status=closed even if the merge_request webhook is
	// dropped or reordered.
	closed := mr.State == "merged" || mr.State == "closed"
	mrFields := beadsapi.MergeRequestBeadFields{
		MRURL:          mr.URL,
		MRIID:          strconv.Itoa(mr.IID),
		Title:          mr.Title,
		MRState:        mr.State,
		SourceBranch:   mr.SourceBranch,
		TargetBranch:   mr.TargetBranch,
		PipelineStatus: status,
		PipelineURL:    pipelineURL,
	}
	if closed {
		if mr.State == "merged" {
			mrFields.MROutcome = "merged"
		} else {
			mrFields.MROutcome = "closed"
		}
	}
	if _, err := daemon.UpsertMergeRequestBead(ctx, mrFields, closed); err != nil {
		logger.Warn("webhook: failed to upsert MR bead pipeline status",
			"mr_url", mr.URL, "error", err)
	}
}

// handleApprovalWebhook processes MR approved/unapproved webhook events.
func handleApprovalWebhook(ctx context.Context, mrURL, action, username string, daemon GitLabBeadClient, logger *slog.Logger) {
	if mrURL == "" {
		return
	}
	// Service-identity writes — see ka-0000005187; bridge runs under
	// fleet:gitlab-bridge. Approver username is logged below.

	logger.Info("webhook: MR approval event",
		"mr_url", mrURL, "action", action, "user", username)

	beads, err := daemon.ListTaskBeads(ctx)
	if err != nil {
		logger.Error("webhook: failed to list beads for approval event", "error", err)
		return
	}

	for _, bead := range beads {
		if bead.Fields["mr_url"] != mrURL {
			continue
		}

		fields := map[string]string{}

		switch action {
		case "approved":
			fields["mr_approved"] = "true"
			// Append approver to comma-separated list.
			existing := bead.Fields["mr_approvers"]
			if existing == "" {
				fields["mr_approvers"] = username
			} else if !containsApprover(existing, username) {
				fields["mr_approvers"] = existing + "," + username
			}
		case "unapproved":
			fields["mr_approved"] = "false"
			// Remove approver from list.
			fields["mr_approvers"] = removeApprover(bead.Fields["mr_approvers"], username)
		}

		if err := daemon.UpdateBeadFields(ctx, bead.ID, fields); err != nil {
			logger.Error("webhook: failed to update approval status on bead",
				"bead", bead.ID, "error", err)
		} else {
			logger.Info("webhook: updated approval status on bead",
				"bead", bead.ID, "action", action, "user", username)
		}
	}
}

// webhookMR is the merge_request subset embedded in GitLab pipeline, note,
// and emoji webhook payloads. Includes State plus source/target branches so
// the bridge can keep mr_state current on the MR bead even when pipeline or
// note events arrive without (or before) the corresponding merge_request
// event. See ka-0000005367 (W-A from ka-0000002952).
type webhookMR struct {
	IID          int    `json:"iid"`
	URL          string `json:"url"`
	Title        string `json:"title"`
	State        string `json:"state"` // opened, closed, merged, locked
	SourceBranch string `json:"source_branch"`
	TargetBranch string `json:"target_branch"`
}

// notePosition mirrors the GitLab note position object for inline review comments.
type notePosition struct {
	NewPath string `json:"new_path"`
	NewLine int    `json:"new_line"`
	OldPath string `json:"old_path"`
	OldLine int    `json:"old_line"`
}

// noteContext holds parsed data from a GitLab note webhook event.
type noteContext struct {
	NoteableType string
	Note         string
	System       bool
	Author       string
	AuthorID     int // GitLab numeric user id of Author; 0 when unknown
	BotUsername  string
	Position     *notePosition
	DiscussionID string
	MR           *webhookMR
}

// handleNoteWebhook processes a GitLab note webhook event. It matches
// MR review comments to beads, creates bead comments with the review feedback,
// and nudges the assigned agent with a rich message containing file path, line,
// diff context, and discussion ID. If no running agent is found and an
// AgentResolver is configured, it spawns a new agent to handle the review.
// Notes from the bot user are skipped to prevent feedback loops.
func handleNoteWebhook(ctx context.Context, nc noteContext, nudge NudgeFunc, resolver *AgentResolver, mentions *MentionHandler, daemon GitLabBeadClient, logger *slog.Logger) {
	// Only process MR discussion notes.
	if nc.NoteableType != "MergeRequest" {
		logger.Debug("webhook: note event is not for MergeRequest, skipping",
			"noteable_type", nc.NoteableType)
		return
	}
	// Service-identity writes — see ka-0000005187; bridge runs under
	// fleet:gitlab-bridge. Comment author is preserved in the GitLab note
	// body (the bridge wraps it as "**@<author>**: <body>").

	// Skip system-generated notes (merge status changes, etc.).
	if nc.System {
		logger.Debug("webhook: skipping system note")
		return
	}

	// If the note is from the bot user, check whether it was posted by a
	// gardenio agent (squawk forwarder prefixes "**gardenio agent** `<name>`:")
	// or by CI/automation. Agent-to-agent comments are allowed but
	// own-comments (where the authoring agent == the bead's assignee) are
	// filtered per-bead in the loop below.
	var botAgentAuthor string
	if nc.BotUsername != "" && nc.Author == nc.BotUsername {
		botAgentAuthor = extractBotAgentName(nc.Note)
		if botAgentAuthor == "" {
			// Bot comment without agent prefix — CI/automation, skip.
			logger.Debug("webhook: skipping non-agent bot note",
				"author", nc.Author, "bot_username", nc.BotUsername)
			return
		}
		logger.Debug("webhook: bot note from agent, allowing inter-agent delivery",
			"bot_agent", botAgentAuthor)
	}

	if nc.MR == nil || nc.MR.URL == "" {
		logger.Debug("webhook: note event has no merge_request, skipping")
		return
	}

	logger.Info("webhook: MR review comment",
		"mr_url", nc.MR.URL, "author", nc.Author)

	// Check for @gardenio mention — if present, spawn a new agent and skip
	// normal review-comment processing.
	if mentions != nil && botAgentAuthor == "" {
		if mentions.HandleMention(ctx, nc) {
			return
		}
	}

	// Fetch only the task beads bound to THIS MR. Closed beads are included —
	// review comments can arrive after the agent's task is complete.
	//
	// This must stay a targeted, server-side-filtered lookup. It used to call
	// ListTaskBeadsAll and filter in Go, which pulled the whole task corpus
	// (10k full BeadDetail rows out of 52k+ matches) on EVERY MR comment and
	// OOMKilled the 128Mi bridge — 19 restarts in 19h — while also silently
	// dropping matches past the 10k cap. See ka-0000007917, advice ka-0000003555.
	beads, err := daemon.ListTaskBeadsForMR(ctx, nc.MR.URL)
	if err != nil {
		logger.Error("webhook: failed to list beads for note event",
			"mr_url", nc.MR.URL, "error", err)
		return
	}

	// Format the comment text for the bead.
	var commentText strings.Builder
	commentText.WriteString(fmt.Sprintf("**GitLab review comment** by @%s:\n\n", nc.Author))
	if nc.Position != nil && nc.Position.NewPath != "" {
		commentText.WriteString(fmt.Sprintf("`%s:%d`\n\n", nc.Position.NewPath, nc.Position.NewLine))
	}
	commentText.WriteString(nc.Note)

	for _, bead := range beads {
		// Redundant with the server-side mr_url predicate above, but kept as a
		// cheap correctness backstop: a future refactor that widens the query
		// must not silently start commenting on unrelated beads.
		if bead.Fields["mr_url"] != nc.MR.URL {
			continue
		}

		// Add comment to the bead.
		if err := daemon.AddComment(ctx, bead.ID, "gitlab-bridge", commentText.String()); err != nil {
			logger.Error("webhook: failed to add review comment to bead",
				"bead", bead.ID, "error", err)
			continue
		}

		logger.Info("webhook: added review comment to bead",
			"bead", bead.ID, "author", nc.Author, "mr_url", nc.MR.URL)

		// Only set the review flag and nudge/spawn when the comment
		// @-mentions the bot user or is from another gardenio agent.
		// Random human-to-human comments on the MR should not wake up agents.
		if botAgentAuthor == "" && !noteAddressesBot(nc) {
			continue
		}

		// Set mr_has_review_comments flag.
		fields := map[string]string{
			"mr_has_review_comments": "true",
		}
		if err := daemon.UpdateBeadFields(ctx, bead.ID, fields); err != nil {
			logger.Error("webhook: failed to set review comment flag on bead",
				"bead", bead.ID, "error", err)
		}

		// Skip nudging if this is the agent's own comment (squawk echoed back).
		if botAgentAuthor != "" && botAgentAuthor == bead.Assignee {
			logger.Debug("webhook: skipping nudge for agent's own comment",
				"bead", bead.ID, "agent", botAgentAuthor)
			continue
		}

		// Nudge the assigned agent with rich context.
		nudged := false
		if nudge != nil && bead.Assignee != "" {
			msg := buildReviewNudgeMessage(nc, bead.ID)
			if err := nudge(ctx, bead.Assignee, msg); err != nil {
				logger.Error("webhook: failed to nudge agent for review comment",
					"bead", bead.ID, "agent", bead.Assignee, "error", err)
			} else {
				logger.Info("webhook: nudged agent for review comment",
					"bead", bead.ID, "agent", bead.Assignee, "author", nc.Author)
				nudged = true
			}
		}

		// Fallback: if nudge didn't work and we have an agent resolver,
		// try to find or spawn an agent for this MR review comment.
		// Skip if the MR is already merged or the task bead is closed —
		// there's no running agent to address review feedback. Source of
		// truth for merge status is the linked MR bead (epic
		// ka-0000001613) — the legacy task-bead mr_merged field is no
		// longer written.
		if !nudged && resolver != nil &&
			!taskMRMerged(ctx, daemon, bead.ID, logger) &&
			bead.Status != "closed" && bead.Status != "done" {
			taskEvent := BeadEvent{
				ID:       bead.ID,
				Type:     bead.Type,
				Title:    bead.Title,
				Status:   bead.Status,
				Assignee: bead.Assignee,
				Labels:   bead.Labels,
				Fields:   bead.Fields,
			}
			msg := buildReviewNudgeMessage(nc, bead.ID)
			if err := resolver.ResolveAndNudge(ctx, taskEvent, msg); err != nil {
				logger.Warn("webhook: agent resolver failed for review comment",
					"bead", bead.ID, "mr_url", nc.MR.URL, "error", err)
			} else {
				logger.Info("webhook: dispatched review via agent resolver",
					"bead", bead.ID, "mr_url", nc.MR.URL, "author", nc.Author)
			}
		}
	}
}

// extractBotAgentName extracts the agent name from a squawk-forwarded GitLab
// note. The squawk forwarder formats notes as: **gardenio agent** `<name>`:
// Returns empty string if the note doesn't match the agent prefix pattern.
func extractBotAgentName(noteBody string) string {
	const prefix = "**gardenio agent** `"
	if !strings.HasPrefix(noteBody, prefix) {
		return ""
	}
	rest := noteBody[len(prefix):]
	idx := strings.Index(rest, "`")
	if idx <= 0 {
		return ""
	}
	return rest[:idx]
}

// buildReviewNudgeMessage constructs a rich nudge message for an agent from a
// GitLab review comment, including file path, line number, diff context,
// discussion ID, and MR reference.
func buildReviewNudgeMessage(nc noteContext, beadID string) string {
	var b strings.Builder
	b.WriteString("GitLab review comment on MR")
	if nc.MR.Title != "" {
		b.WriteString(fmt.Sprintf(" \"%s\"", nc.MR.Title))
	}
	b.WriteString(fmt.Sprintf(" by @%s", nc.Author))
	if nc.Position != nil && nc.Position.NewPath != "" {
		b.WriteString(fmt.Sprintf("\nFile: %s", nc.Position.NewPath))
		if nc.Position.NewLine > 0 {
			b.WriteString(fmt.Sprintf(":%d", nc.Position.NewLine))
		}
		if nc.Position.OldPath != "" && nc.Position.OldPath != nc.Position.NewPath {
			b.WriteString(fmt.Sprintf(" (was %s)", nc.Position.OldPath))
		}
		if nc.Position.OldLine > 0 {
			b.WriteString(fmt.Sprintf(" [old line %d]", nc.Position.OldLine))
		}
	}
	b.WriteString(fmt.Sprintf("\nComment: %s", nc.Note))
	if nc.DiscussionID != "" {
		b.WriteString(fmt.Sprintf("\nDiscussion ID: %s", nc.DiscussionID))
	}
	if nc.MR.URL != "" {
		b.WriteString(fmt.Sprintf("\nMR: %s", nc.MR.URL))
	}
	b.WriteString(fmt.Sprintf("\nBead: %s", beadID))
	return b.String()
}

// noteAddressesBot returns true if the note @-mentions the GitLab bot user.
// When no bot username is configured, returns true to preserve existing behavior.
func noteAddressesBot(nc noteContext) bool {
	if nc.BotUsername == "" {
		return true
	}
	return strings.Contains(nc.Note, "@"+nc.BotUsername)
}

// containsApprover checks if username is in the comma-separated approvers list.
func containsApprover(approvers, username string) bool {
	for _, a := range strings.Split(approvers, ",") {
		if strings.TrimSpace(a) == username {
			return true
		}
	}
	return false
}

// removeApprover removes username from the comma-separated approvers list.
func removeApprover(approvers, username string) string {
	var result []string
	for _, a := range strings.Split(approvers, ",") {
		a = strings.TrimSpace(a)
		if a != "" && a != username {
			result = append(result, a)
		}
	}
	return strings.Join(result, ",")
}

// e2eTestSummaryMarker is the HTML comment marker used by the CI e2e summary
// job to identify its MR comments. When an emoji reaction is added to a note
// containing this marker, the bridge triggers a pipeline re-run.
const e2eTestSummaryMarker = "<!-- e2e-test-summary -->"

// e2eRerunEmoji is the GitLab award emoji name that triggers an e2e re-run.
// Users add a :runner: (🏃) reaction to the e2e test summary comment.
const e2eRerunEmoji = "runner"

// emojiWebhookEvent represents the GitLab emoji webhook payload.
// See: https://docs.gitlab.com/user/project/integrations/webhook_events/
type emojiWebhookEvent struct {
	ObjectKind string `json:"object_kind"` // "emoji"
	EventType  string `json:"event_type"`  // "award" or "revoke"
	User       struct {
		Username string `json:"username"`
		Name     string `json:"name"`
	} `json:"user"`
	ProjectID  int `json:"project_id"`
	ObjectAttr struct {
		AwardableID   int    `json:"awardable_id"`
		AwardableType string `json:"awardable_type"` // "Note", "MergeRequest", "Issue"
		Name          string `json:"name"`           // emoji name, e.g. "runner"
		Action        string `json:"action"`         // "award" or "revoke"
	} `json:"object_attributes"`
	MergeRequest *webhookMR `json:"merge_request"`
	// Note is present when AwardableType == "Note" (emoji on a comment).
	Note *struct {
		ID   int    `json:"id"`
		Note string `json:"note"`
	} `json:"note"`
}

// handleEmojiWebhook processes a GitLab emoji webhook event. It detects
// :runner: emoji reactions on e2e test summary MR comments and triggers a
// new pipeline run for the MR's source branch.
func handleEmojiWebhook(ctx context.Context, body []byte, gitlab *GitLabClient, logger *slog.Logger) {
	var event emojiWebhookEvent
	if err := json.Unmarshal(body, &event); err != nil {
		logger.Error("webhook: failed to decode emoji event", "error", err)
		return
	}

	// Only process award events (not revoke).
	if event.EventType != "award" && event.ObjectAttr.Action != "award" {
		logger.Debug("webhook: emoji event is not an award, skipping",
			"event_type", event.EventType, "action", event.ObjectAttr.Action)
		return
	}

	// Only process the designated re-run emoji.
	if event.ObjectAttr.Name != e2eRerunEmoji {
		logger.Debug("webhook: emoji is not the e2e rerun trigger, skipping",
			"emoji", event.ObjectAttr.Name, "expected", e2eRerunEmoji)
		return
	}

	// Must have an MR context.
	if event.MergeRequest == nil || event.MergeRequest.URL == "" {
		logger.Debug("webhook: emoji event has no merge_request context, skipping")
		return
	}

	// The emoji must be on a Note (comment), not on the MR itself.
	if event.ObjectAttr.AwardableType != "Note" {
		logger.Debug("webhook: emoji is not on a Note, skipping",
			"awardable_type", event.ObjectAttr.AwardableType)
		return
	}

	// Check if the note contains the e2e test summary marker.
	// The note body may be included in the webhook payload, or we may need
	// to fetch it via the API.
	noteBody := ""
	if event.Note != nil && event.Note.Note != "" {
		noteBody = event.Note.Note
	}

	if noteBody == "" && gitlab != nil {
		// Fetch the note via API to check its content.
		ref := ParseMRURL(event.MergeRequest.URL)
		if ref != nil {
			note, err := gitlab.GetMRNote(ctx, ref.ProjectPath, ref.IID, event.ObjectAttr.AwardableID)
			if err != nil {
				logger.Error("webhook: failed to fetch note for emoji check",
					"mr_url", event.MergeRequest.URL,
					"note_id", event.ObjectAttr.AwardableID,
					"error", err)
				return
			}
			noteBody = note.Body
		}
	}

	if !strings.Contains(noteBody, e2eTestSummaryMarker) {
		logger.Debug("webhook: emoji note does not contain e2e summary marker, skipping",
			"note_id", event.ObjectAttr.AwardableID,
			"mr_url", event.MergeRequest.URL)
		return
	}

	// All checks passed — trigger a new pipeline for the MR's source branch.
	logger.Info("webhook: e2e rerun triggered via emoji",
		"emoji", event.ObjectAttr.Name,
		"user", event.User.Username,
		"mr_url", event.MergeRequest.URL,
		"mr_iid", event.MergeRequest.IID,
		"source_branch", event.MergeRequest.SourceBranch)

	if gitlab == nil {
		logger.Warn("webhook: GitLab client not configured, cannot trigger pipeline")
		return
	}

	ref := ParseMRURL(event.MergeRequest.URL)
	if ref == nil {
		logger.Error("webhook: failed to parse MR URL for pipeline trigger",
			"mr_url", event.MergeRequest.URL)
		return
	}

	// Trigger a new MR pipeline (source=merge_request_event). A branch POST is
	// rejected outright by MR-pipeline-only projects like the monorepo
	// (ka-0000003062), and this path is always MR-scoped anyway.
	pip, err := gitlab.CreateMergeRequestPipeline(ctx, ref.ProjectPath, ref.IID)
	if err != nil {
		logger.Error("webhook: failed to trigger e2e rerun pipeline",
			"project", ref.ProjectPath,
			"ref", event.MergeRequest.SourceBranch,
			"error", err)

		// Post a failure comment on the MR so the user knows.
		failMsg := fmt.Sprintf("🏃 **E2E re-run failed** — could not trigger pipeline for `%s`.\n\nError: %s\n\n_Triggered by @%s via :runner: emoji_",
			event.MergeRequest.SourceBranch, err.Error(), event.User.Username)
		if _, err := gitlab.PostMRNote(ctx, ref.ProjectPath, ref.IID, failMsg); err != nil {
			logger.Error("webhook: failed to post failure comment",
				"mr_iid", ref.IID, "error", err)
		}
		return
	}

	// Post an acknowledgment comment on the MR.
	ackMsg := fmt.Sprintf("🏃 **E2E tests re-triggered** by @%s — [Pipeline #%d](%s)\n\n_New results will appear as a comment when the pipeline completes._",
		event.User.Username, pip.ID, pip.WebURL)
	if _, err := gitlab.PostMRNote(ctx, ref.ProjectPath, ref.IID, ackMsg); err != nil {
		logger.Error("webhook: failed to post rerun acknowledgment comment",
			"mr_iid", ref.IID, "pipeline_id", pip.ID, "error", err)
	}

	logger.Info("webhook: e2e rerun pipeline triggered successfully",
		"pipeline_id", pip.ID,
		"pipeline_url", pip.WebURL,
		"mr_iid", ref.IID,
		"triggered_by", event.User.Username)
}

// pushWebhookEvent represents the GitLab push webhook payload.
type pushWebhookEvent struct {
	ObjectKind string `json:"object_kind"` // "push"
	Ref        string `json:"ref"`         // "refs/heads/branch-name"
	Before     string `json:"before"`      // previous HEAD SHA
	After      string `json:"after"`       // new HEAD SHA
	UserName   string `json:"user_name"`
	UserEmail  string `json:"user_email"`
	User       struct {
		Username string `json:"username"`
	} `json:"user"`
	Project struct {
		ID       int    `json:"id"`
		Name     string `json:"name"`
		URL      string `json:"web_url"`
		PathNS   string `json:"path_with_namespace"`
		Homepage string `json:"homepage"`
	} `json:"project"`
	Commits []pushCommit `json:"commits"`
}

// pushCommit represents a single commit in a push event.
type pushCommit struct {
	ID      string `json:"id"`
	Message string `json:"message"`
	Title   string `json:"title"`
	Author  struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	} `json:"author"`
	URL string `json:"url"`
}

// handlePushWebhook processes a GitLab push webhook event. It finds agents
// working on the pushed branch (via task bead workspace metadata or agent bead
// gitlab_mr_source_branch field) and nudges them about the new commits.
func handlePushWebhook(ctx context.Context, body []byte, nudge NudgeFunc, daemon GitLabBeadClient, botUsername string, logger *slog.Logger) {
	var event pushWebhookEvent
	if err := json.Unmarshal(body, &event); err != nil {
		logger.Error("webhook: failed to decode push event", "error", err)
		return
	}

	// Extract branch name from ref (strip "refs/heads/" prefix).
	branch := strings.TrimPrefix(event.Ref, "refs/heads/")
	if branch == event.Ref {
		// Not a branch push (could be a tag) — skip.
		logger.Debug("webhook: push event is not a branch push, skipping", "ref", event.Ref)
		return
	}

	// Skip pushes to main/master — too noisy and not agent-owned.
	if branch == "main" || branch == "master" {
		logger.Debug("webhook: skipping push to default branch", "branch", branch)
		return
	}

	// Skip if the push deletes the branch (after == zero SHA).
	if event.After == "0000000000000000000000000000000000000000" {
		logger.Debug("webhook: skipping branch deletion", "branch", branch)
		return
	}

	// Skip pushes from the bot user to avoid self-nudge loops.
	pusher := event.User.Username
	if botUsername != "" && pusher == botUsername {
		logger.Debug("webhook: skipping push from bot user", "user", pusher, "branch", branch)
		return
	}

	logger.Info("webhook: push event",
		"branch", branch, "user", pusher,
		"commits", len(event.Commits), "project", event.Project.PathNS)

	if nudge == nil {
		logger.Debug("webhook: no nudge function configured, skipping push notification")
		return
	}

	// Track which agents we've already nudged to avoid duplicate nudges
	// when multiple beads point to the same branch.
	nudged := make(map[string]bool)

	// Search task beads for ones with workspace metadata matching this branch.
	taskBeads, err := daemon.ListTaskBeads(ctx)
	if err != nil {
		logger.Error("webhook: failed to list task beads for push event", "error", err)
	} else {
		for _, bead := range taskBeads {
			wsJSON := bead.Fields["workspace"]
			if wsJSON == "" {
				continue
			}
			var ws struct {
				Branch string `json:"branch"`
			}
			if err := json.Unmarshal([]byte(wsJSON), &ws); err != nil || ws.Branch == "" {
				continue
			}
			if ws.Branch != branch {
				continue
			}
			agent := bead.Assignee
			if agent == "" || nudged[agent] {
				continue
			}
			// Don't nudge an agent about its own push.
			if agent == pusher {
				continue
			}
			msg := buildPushNudgeMessage(event, branch, bead.ID)
			if err := nudge(ctx, agent, msg); err != nil {
				logger.Error("webhook: failed to nudge agent for push event",
					"bead", bead.ID, "agent", agent, "branch", branch, "error", err)
			} else {
				logger.Info("webhook: nudged agent for push event (via task bead)",
					"bead", bead.ID, "agent", agent, "branch", branch)
				nudged[agent] = true
			}
		}
	}

	// Search agent beads for ones bound to this branch via gitlab_mr_source_branch.
	agentBeads, err := daemon.ListActiveAgentBeadDetails(ctx)
	if err != nil {
		logger.Error("webhook: failed to list agent beads for push event", "error", err)
	} else {
		for _, ab := range agentBeads {
			if ab.Fields["gitlab_mr_source_branch"] != branch {
				continue
			}
			agent := ab.Fields["agent"]
			if agent == "" || nudged[agent] {
				continue
			}
			if agent == pusher {
				continue
			}
			msg := buildPushNudgeMessage(event, branch, "")
			if err := nudge(ctx, agent, msg); err != nil {
				logger.Error("webhook: failed to nudge agent for push event",
					"agent_bead", ab.ID, "agent", agent, "branch", branch, "error", err)
			} else {
				logger.Info("webhook: nudged agent for push event (via agent bead)",
					"agent_bead", ab.ID, "agent", agent, "branch", branch)
				nudged[agent] = true
			}
		}
	}

	if len(nudged) == 0 {
		logger.Debug("webhook: no agents found for pushed branch", "branch", branch)
	}
}

// buildPushNudgeMessage constructs a nudge message for an agent when new
// commits are pushed to the branch they're working on.
func buildPushNudgeMessage(event pushWebhookEvent, branch, beadID string) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("New commits pushed to branch `%s`", branch))

	pusher := event.User.Username
	if pusher == "" {
		pusher = event.UserName
	}
	if pusher != "" {
		b.WriteString(fmt.Sprintf(" by @%s", pusher))
	}

	b.WriteString(fmt.Sprintf(" (%d commit(s)):\n", len(event.Commits)))

	// Show up to 5 commit summaries.
	limit := len(event.Commits)
	if limit > 5 {
		limit = 5
	}
	for i := 0; i < limit; i++ {
		c := event.Commits[i]
		title := c.Title
		if title == "" {
			title = strings.SplitN(c.Message, "\n", 2)[0]
		}
		// Truncate long titles.
		if len(title) > 80 {
			title = title[:77] + "..."
		}
		b.WriteString(fmt.Sprintf("  - %s: %s\n", c.ID[:8], title))
	}
	if len(event.Commits) > 5 {
		b.WriteString(fmt.Sprintf("  ... and %d more\n", len(event.Commits)-5))
	}

	b.WriteString("\nPlease pull the latest changes with `git pull` to incorporate these commits into your work.")
	if beadID != "" {
		b.WriteString(fmt.Sprintf("\nBead: %s", beadID))
	}
	return b.String()
}
