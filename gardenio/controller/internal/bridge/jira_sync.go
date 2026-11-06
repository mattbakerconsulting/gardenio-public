// Package bridge provides the JIRA sync-back watcher.
//
// JiraSync subscribes to kbeads SSE bead updated/closed events, filters for
// beads with the source:jira label, and syncs status changes, MR links, and
// closing comments back to the originating JIRA issue.
package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// syncTTL is the dedup window for sync-back operations.
const syncTTL = 10 * time.Minute

// jiraCatchUpPageSize / jiraCatchUpMaxPages bound the paged walk in CatchUp.
// CatchUp must pre-mark EVERY historical source:jira sync operation (across
// open + in_progress + CLOSED, with no time window) so SSE replay after a
// restart cannot re-post a duplicate Jira comment. That population grows
// monotonically as closed beads accumulate, so the previous single Limit:1000
// fetch would eventually be silently capped by the daemon (HTTP 200, no error)
// and quietly stop pre-marking the overflow — re-introducing the exact
// duplicate this function exists to prevent (ka-0000006235 / M9, class
// ka-0000003555). Paging with created_at ordering (immutable, so the page
// sequence is stable while new beads are created underneath the walk) folds
// each page into the dedup map and drops it, so peak memory is one page. The
// max-pages cap is a runaway backstop far above any realistic Jira-sourced
// population; if it is ever hit the walk fails LOUDLY (see CatchUp) rather than
// silently pre-marking a prefix. Mirrors spend_settlement_sweep.go /
// advice.FleetArchetypesPaged.
const (
	jiraCatchUpPageSize = 500
	jiraCatchUpMaxPages = 200
)

// JiraSyncRoleResolver resolves the beads role acting behind a sync-back write.
// The outbound field allowlist (jira_allowlist.go) keys on this role. The
// assignee/closer of a Jira-sourced bead is an agent name, so the resolver
// looks up that agent bead and reads its role. *beadsapi.Client satisfies this
// (it implements FindAgentBead); a nil resolver leaves role resolution to the
// bead's own Fields["role"] only.
type JiraSyncRoleResolver interface {
	FindAgentBead(ctx context.Context, agentName string) (*beadsapi.BeadDetail, error)
}

// JiraSync watches bead SSE events and syncs MR links and status back to JIRA.
type JiraSync struct {
	jira                  *JiraClient
	logger                *slog.Logger
	disableTransitions    bool
	enableClaimTransition bool   // override: allow "In Progress" on claim even when disableTransitions is true
	botAccountID          string // optional JIRA account ID for self-assignment

	// writePolicy is the config-driven, project-agnostic write policy (epic
	// ka-0000003475, task ka-0000007252). When non-nil it is the SOLE decision
	// path for both the project verdict and the per-role field/comment verdict —
	// FSM is just a configured toggle, not a compiled-in special case. The
	// production jira-bridge wires exactly this. When nil (existing unit tests +
	// the parity oracle in jira_write_policy_test.go) the legacy gate pair below
	// is consulted instead; DefaultWritePolicy reproduces that pair byte-for-byte
	// so the swap changes no behavior.
	writePolicy *WritePolicyStore
	// gate enforces the per-role field-write allowlist (epic ka-0000004904).
	// LEGACY fallback — used only when writePolicy is nil (tests). nil ⇒ unwired:
	// writes proceed ungated (preserves legacy behavior and keeps gate-less
	// construction in tests working).
	gate *AllowlistGate
	// projectGate is the coordinated deny-by-default project gate. LEGACY
	// fallback — used only when writePolicy is nil. FSM is governed by the single
	// JIRA_FSM_SYNC_ENABLED toggle shared by all four outbound paths
	// (ka-0000001679). Never nil after construction — NewJiraSync falls back to
	// the package default gate (FSM off) so behavior is unchanged when unwired.
	projectGate *ProjectSyncGate
	// roles resolves the acting agent's role for a bead. nil ⇒ role comes only
	// from the bead's own Fields["role"].
	roles JiraSyncRoleResolver

	// analysisIssueType is the GBFSM "Analysis" issue-type id (epic ka-0000002067,
	// task ka-0000001294). Empty ⇒ the GBFSM ticket surface is inert (no ticket is
	// created). Set from JIRA_OUTBOX_ANALYSIS_ISSUE_TYPE (else the
	// outboxTypeAnalysis const) once the operator scaffolds the issue type.
	analysisIssueType string
	// labelsFieldID / versionFieldID are the GBFSM custom-field ids that carry the N
	// triage labels and the recommended Affects-Version as FIELDS on the ONE upserted
	// GBFSM ticket (1:1 reshape, Phase 2 ka-0000001491). Empty (the default) ⇒ the
	// label / version fields are omitted from the ticket (the analysis Description
	// still carries the classification in prose); wired from JIRA_OUTBOX_LABELS_FIELD
	// / JIRA_OUTBOX_VERSION_FIELD (else the outboxFieldLabels / outboxFieldVersion
	// consts) once the operator scaffolds the fields.
	labelsFieldID  string
	versionFieldID string

	mu   sync.Mutex
	seen map[string]time.Time // dedup key → last sync time
}

// JiraSyncConfig holds configuration for the JiraSync watcher.
type JiraSyncConfig struct {
	Jira                  *JiraClient
	Logger                *slog.Logger
	DisableTransitions    bool
	EnableClaimTransition bool   // override: allow "In Progress" on claim even when DisableTransitions is true
	BotAccountID          string // optional: JIRA account ID for self-assignment
	// WritePolicy is the config-driven, project-agnostic write policy store
	// (ka-0000007252). When set it is the sole decision path (project + field +
	// comment) and the legacy Gate/ProjectGate below are ignored. The production
	// jira-bridge sets this; nil selects the legacy gate pair (tests).
	WritePolicy *WritePolicyStore
	// Gate is the per-role field-write allowlist. LEGACY fallback (used only when
	// WritePolicy is nil). nil ⇒ writes are ungated.
	Gate *AllowlistGate
	// ProjectGate is the coordinated project gate shared across all four
	// outbound Jira paths (ka-0000001679). LEGACY fallback (used only when
	// WritePolicy is nil). nil ⇒ the package default gate (FSM off, legacy
	// deny-by-default) is used, so behavior is unchanged.
	ProjectGate *ProjectSyncGate
	// Roles resolves an assignee/closer agent name to its role for gating.
	// nil ⇒ role comes only from the bead's Fields["role"].
	Roles JiraSyncRoleResolver
	// AnalysisIssueType is the GBFSM "Analysis" issue-type id used by the GBFSM
	// ticket surface (epic ka-0000002067, task ka-0000001294). Empty (the default,
	// and the default of outboxTypeAnalysis) ⇒ the surface is inert: no GBFSM
	// ticket is ever created. Wired from JIRA_OUTBOX_ANALYSIS_ISSUE_TYPE so the
	// operator can arm it once the "Analysis" issue type is scaffolded in the GBFSM
	// project, without a code change. When empty here NewJiraSync falls back to the
	// outboxTypeAnalysis const.
	AnalysisIssueType string
	// LabelsFieldID / VersionFieldID are the GBFSM custom-field ids that carry the N
	// triage labels and the recommended Affects-Version as FIELDS on the ONE upserted
	// GBFSM ticket (1:1 reshape, Phase 2 ka-0000001491). Empty (the default) ⇒ those
	// fields are omitted from the ticket. Wired from JIRA_OUTBOX_LABELS_FIELD /
	// JIRA_OUTBOX_VERSION_FIELD so the operator can arm them once the fields are
	// scaffolded; when empty here NewJiraSync falls back to the outboxFieldLabels /
	// outboxFieldVersion consts.
	LabelsFieldID  string
	VersionFieldID string
}

// NewJiraSync creates a new JIRA sync-back watcher.
func NewJiraSync(cfg JiraSyncConfig) *JiraSync {
	gate := cfg.ProjectGate
	if gate == nil {
		gate = defaultProjectSyncGate
	}
	// The analysis issue-type id comes from config (JIRA_OUTBOX_ANALYSIS_ISSUE_TYPE)
	// when set, else the compiled-in outboxTypeAnalysis const. Both default empty,
	// so the analysis surface is inert until the operator scaffolds the type.
	analysisIssueType := strings.TrimSpace(cfg.AnalysisIssueType)
	if analysisIssueType == "" {
		analysisIssueType = outboxTypeAnalysis
	}
	// The labels / version field ids come from config when set, else the (empty)
	// compiled-in consts — so the label / version fields stay omitted until the
	// operator scaffolds the fields and arms them.
	labelsFieldID := strings.TrimSpace(cfg.LabelsFieldID)
	if labelsFieldID == "" {
		labelsFieldID = outboxFieldLabels
	}
	versionFieldID := strings.TrimSpace(cfg.VersionFieldID)
	if versionFieldID == "" {
		versionFieldID = outboxFieldVersion
	}
	return &JiraSync{
		jira:                  cfg.Jira,
		logger:                cfg.Logger,
		disableTransitions:    cfg.DisableTransitions,
		enableClaimTransition: cfg.EnableClaimTransition,
		botAccountID:          cfg.BotAccountID,
		writePolicy:           cfg.WritePolicy,
		gate:                  cfg.Gate,
		projectGate:           gate,
		roles:                 cfg.Roles,
		analysisIssueType:     analysisIssueType,
		labelsFieldID:         labelsFieldID,
		versionFieldID:        versionFieldID,
		seen:                  make(map[string]time.Time),
	}
}

// actingRole resolves the beads role behind a sync-back write for bead. It
// prefers the bead's own Fields["role"] (set on agent beads), then falls back to
// looking up the acting agent bead (assignee, else closer) and reading its role.
// Returns "" when no role can be resolved — which the gate treats as fail-closed
// in enforce mode.
func (s *JiraSync) actingRole(ctx context.Context, bead *BeadEvent) string {
	if bead == nil {
		return ""
	}
	if r := strings.TrimSpace(bead.Fields["role"]); r != "" {
		return r
	}
	if s.roles == nil {
		return ""
	}
	agent := bead.Assignee
	if agent == "" {
		agent = bead.ClosedBy
	}
	if agent == "" {
		return ""
	}
	agentBead, err := s.roles.FindAgentBead(ctx, agent)
	if err != nil || agentBead == nil {
		s.logger.Debug("jira-sync: could not resolve acting agent role",
			"bead", bead.ID, "agent", agent, "error", err)
		return ""
	}
	return strings.TrimSpace(agentBead.Fields["role"])
}

// allowProject reports whether the bridge may write to the project owning
// jiraKey at all. Prefers the config-driven write policy (project + toggle);
// falls back to the legacy coordinated project gate when unwired.
func (s *JiraSync) allowProject(jiraKey string) bool {
	if s.writePolicy != nil {
		return s.writePolicy.AllowProject(jiraKey)
	}
	return s.projectGate.AllowProject(jiraKey)
}

// projectGateDiag returns a human-readable description of the project verdict
// source for the "blocked write-back" warning logs. With the config-driven
// store the FSM toggle is no longer a first-class concept, so the field carries
// the store's writable-project set instead.
func (s *JiraSync) projectGateDiag() any {
	if s.writePolicy != nil {
		return s.writePolicy.Projects()
	}
	return s.projectGate.FSMSyncEnabled()
}

// allowField reports whether a non-comment governed write may proceed for role
// on the project owning jiraKey. Prefers the config-driven write policy (which
// folds project + role + field with mode/logging); falls back to the legacy
// per-role allowlist gate. An unwired legacy gate (nil) permits all writes.
func (s *JiraSync) allowField(jiraKey, role string, field JiraWriteField) bool {
	if s.writePolicy != nil {
		return s.writePolicy.AllowField(jiraKey, role, field)
	}
	if s.gate == nil {
		return true
	}
	return s.gate.AllowField(role, field)
}

// allowComment reports whether a comment write of the given visibility may
// proceed for role on the project owning jiraKey. Prefers the config-driven
// write policy; falls back to the legacy per-role allowlist gate. An unwired
// legacy gate (nil) permits all writes.
func (s *JiraSync) allowComment(jiraKey, role string, vis CommentVisibility) bool {
	if s.writePolicy != nil {
		return s.writePolicy.AllowComment(jiraKey, role, vis)
	}
	if s.gate == nil {
		return true
	}
	return s.gate.AllowComment(role, vis)
}

// inScope reports whether a sync-back write targeting targetKey is in scope for
// bead — i.e. targetKey names the same Jira issue the bead is bound to (its
// jira_key / jira:<KEY>). Fail-closed: an unbound bead, an empty target, or a
// mismatch denies the write so the bridge can only ever mutate the bead's OWN
// ticket (ka-0000001679). On the sync-back path the target IS derived from the
// binding, so this is a belt-and-suspenders invariant that also documents and
// tests the guarantee.
func (s *JiraSync) inScope(bead *BeadEvent, targetKey string) bool {
	if bead == nil {
		return false
	}
	bound := boundJiraKeyFromFields(bead.Fields, bead.Labels)
	if !jiraWriteInScope(bound, targetKey) {
		s.logger.Warn("jira-sync: blocked out-of-scope write (target != bead's bound ticket)",
			"bead", bead.ID, "bound_jira_key", bound, "target_jira_key", targetKey)
		return false
	}
	return true
}

// RegisterHandlers registers SSE event handlers on the given stream for
// bead updated and closed events.
func (s *JiraSync) RegisterHandlers(stream *SSEStream) {
	stream.On("beads.bead.updated", s.handleUpdated)
	stream.On("beads.bead.closed", s.handleClosed)
	s.logger.Info("JIRA sync watcher registered SSE handlers",
		"topics", []string{"beads.bead.updated", "beads.bead.closed"})
}

// JiraSyncCatchUpClient is the subset of beadsapi.Client needed for catch-up.
type JiraSyncCatchUpClient interface {
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
}

// CatchUp fetches existing Jira-sourced beads from the daemon and pre-populates
// the dedup map so that SSE replay after a restart does not re-post comments,
// links, or transitions that were already synced.
//
// This includes closed beads because the bridge state volume may be ephemeral
// (emptyDir), causing the SSE last-event-ID to be lost on restart. Without
// covering closed beads, ancient claim/close events would be replayed and
// re-posted to Jira every time the bridge restarts.
func (s *JiraSync) CatchUp(ctx context.Context, client JiraSyncCatchUpClient) {
	if client == nil {
		return
	}

	// Fetch all Jira-sourced beads including closed ones. The bridge state
	// volume may be ephemeral (emptyDir), so SSE may replay from the
	// beginning on restart. We must pre-mark ALL historical sync operations
	// to prevent duplicate Jira comments — so this pages to completion rather
	// than riding a single Limit that the daemon could silently cap. Each page
	// is folded into the dedup map and dropped before the next fetch, so peak
	// memory is one page (ka-0000006235 / M9).
	var (
		scanned int
		marked  int
	)
	for page := 0; page < jiraCatchUpMaxPages; page++ {
		result, err := client.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
			Kinds:    []string{"issue"},
			Statuses: []string{"open", "in_progress", "closed"},
			Labels:   []string{"source:jira"},
			Sort:     "created_at",
			Limit:    jiraCatchUpPageSize,
			Offset:   page * jiraCatchUpPageSize,
		})
		if err != nil {
			// A mid-walk failure would leave the dedup map partially
			// populated — later beads unmarked — which is exactly the
			// condition that re-posts duplicate Jira comments. Fail the whole
			// catch-up loudly rather than proceeding with a partial pre-mark.
			s.logger.Warn("jira catch-up: failed to list beads, catch-up incomplete — SSE replay may re-post to Jira",
				"error", err, "page", page, "scanned_so_far", scanned)
			return
		}

		for _, bead := range result.Beads {
			scanned++
			marked += s.markBeadForCatchUp(bead)
		}

		if len(result.Beads) < jiraCatchUpPageSize {
			// Short page = last page: the walk is complete.
			s.logger.Info("jira catch-up complete",
				"beads", scanned, "marked", marked, "pages", page+1)
			return
		}
	}

	// Ran out of pages with every page full — the population exceeds the
	// runaway backstop. Pre-marking stopped at the cap, so SSE replay could
	// re-post the unmarked tail. Surface this loudly; it means the cap needs
	// raising or the query needs a tighter window, NOT that catch-up is fine.
	s.logger.Error("jira catch-up hit the max-pages backstop without exhausting the source:jira population — the tail is UNMARKED and SSE replay may re-post duplicate Jira comments; raise jiraCatchUpMaxPages or window the query",
		"max_pages", jiraCatchUpMaxPages, "page_size", jiraCatchUpPageSize, "scanned", scanned, "marked", marked)
}

// markBeadForCatchUp pre-marks every dedup key implied by a single Jira-sourced
// bead's current state (claim, MR link, MR merged, close) so a replayed SSE
// event for it after a restart is deduped instead of re-posted to Jira. Returns
// the number of keys marked (0 if the bead carries no resolvable jira_key).
func (s *JiraSync) markBeadForCatchUp(bead *beadsapi.BeadDetail) int {
	if bead == nil {
		return 0
	}
	jiraKey := bead.Fields["jira_key"]
	if jiraKey == "" {
		// Fallback: check labels.
		for _, label := range bead.Labels {
			if strings.HasPrefix(label, "jira:") && !strings.HasPrefix(label, "jira-label:") {
				jiraKey = strings.TrimPrefix(label, "jira:")
				break
			}
		}
	}
	if jiraKey == "" {
		return 0
	}

	marked := 0

	// Pre-mark claimed beads. Any bead with an assignee was claimed
	// at some point — mark regardless of current status so that
	// replayed "updated" events for closed beads don't re-post.
	if bead.Assignee != "" {
		s.mark("claimed:" + bead.ID + ":" + bead.Assignee)
		marked++
	}

	// Pre-mark MR links.
	if mrURL := bead.Fields["mr_url"]; mrURL != "" {
		s.mark("mr:" + bead.ID + ":" + mrURL)
		marked++
	}

	// Pre-mark MR merged.
	if bead.Fields["mr_merged"] == "true" {
		s.mark("merged:" + bead.ID)
		marked++
	}

	// Pre-mark closed beads so replayed close events don't re-post.
	if bead.Status == "closed" {
		s.mark("close:" + bead.ID)
		marked++
	}

	return marked
}

// mark records a dedup key as seen without returning whether it was already present.
func (s *JiraSync) mark(key string) {
	s.mu.Lock()
	s.seen[key] = time.Now()
	s.mu.Unlock()
}

func (s *JiraSync) handleUpdated(ctx context.Context, data []byte) {
	bead := ParseBeadEvent(data)
	if bead == nil {
		return
	}

	// Dark beads never flow upstream. The source:jira-dark marker is set on
	// beads that mirror the in-gardenio ("dark") universe; write-back to the
	// real Jira would leak dark-only work upstream. Explicit early-return
	// before any project gating (R7 ka-0000004670, ka-0000005143).
	if hasLabel(bead.Labels, jiraDarkLabel) {
		s.logger.Info("jira-sync: skipping dark-marked bead (no upstream write-back)",
			"bead", bead.ID)
		return
	}

	// Only sync beads that originated from JIRA.
	jiraKey := jiraKeyFromBead(*bead)
	if jiraKey == "" {
		return
	}

	// Resolve the acting beads role once for every write triggered by this event
	// (the outbox intents below and the general sync-back further down), so the
	// per-role field allowlist (epic ka-0000004904) gates each governed write. An
	// unwired gate leaves every write permitted.
	role := s.actingRole(ctx, bead)

	// fsm-triage → OUTBOX (epic ka-0000002067; 1:1 reshape ka-0000002637). A
	// triage-role bead carrying triage classification drives ONE upserted GBFSM
	// ticket in the internal GBFSM outbox project — the bot never touches FSM. This
	// runs BEFORE the bound-project gate below because the GBFSM ticket targets
	// GBFSM, NOT the bead's bound (FSM) ticket. The binding is validated in-scope
	// and passed only as DATA (fsm_target_key); syncGBFSMTicket independently gates
	// the write on GBFSM writability (the outbox toggle) + the labels/versions
	// field. Because FSM is never in the writable set, the general sync-back below
	// can never write it directly — an FSM-bound bead's classification flows
	// exclusively through this GBFSM ticket.
	if role == triageRole && s.inScope(bead, jiraKey) {
		s.syncGBFSMTicket(ctx, bead, jiraKey, role)
	}

	// General sync-back (claim / MR link / merged / close) targets the bead's OWN
	// bound ticket, so it is deny-by-default gated on that project being writable.
	// jiraKeyFromBead keys off the freely agent-settable jira:<KEY> label /
	// jira_key field, so an allow-list (not a deny-list) is required
	// (ka-0000005143). FSM is permanently absent from the writable set (epic
	// ka-0000002067), so an FSM-bound bead makes ZERO direct writes here. This
	// project verdict is mode-independent (always hard-enforced).
	if !s.allowProject(jiraKey) {
		s.logger.Warn("jira-sync: blocked write-back to non-writable project",
			"bead", bead.ID, "jira_key", jiraKey, "project", jiraProjectKey(jiraKey),
			"writable_projects", s.projectGateDiag())
		return
	}

	// Bead→ticket scoping: every write below targets jiraKey, which must be the
	// ticket this bead is bound to. inScope fails closed on a missing binding or
	// mismatch, so the bridge can only ever mutate the bead's OWN ticket
	// (ka-0000001679).
	if !s.inScope(bead, jiraKey) {
		return
	}

	// Operator attribution ("Acted by <operator>") stamped onto every comment
	// below so the Jira history stays honest about which human triggered the
	// work despite the shared service account (epic ka-0000004904 decision #4 /
	// ka-0000006003). The SSE event carries only Fields — no daemon-resolved
	// display name — so this is the raw operated_by fallback. "" when the bead
	// has no human operator, in which case withAttribution is a no-op.
	attribution := operatorAttributionLine(bead.Fields["operated_by"], "")

	// Sync agent claim — post comment, add label, transition, and optionally assign.
	if bead.Status == "in_progress" && bead.Assignee != "" {
		dedupKey := "claimed:" + bead.ID + ":" + bead.Assignee
		if !s.isDuplicate(dedupKey) {
			s.logger.Info("syncing agent claim to JIRA",
				"bead", bead.ID, "jira_key", jiraKey, "assignee", bead.Assignee, "role", role)

			if s.allowComment(jiraKey, role, CommentInternal) {
				comment := withAttribution("Gardenio agent "+bead.Assignee+" is working on this issue.", attribution)
				if err := s.jira.AddCommentWithVisibility(ctx, jiraKey, comment, CommentInternal); err != nil {
					s.logger.Error("failed to add JIRA claim comment",
						"jira_key", jiraKey, "error", err)
				}
			}

			if (s.enableClaimTransition || !s.disableTransitions) && s.allowField(jiraKey, role, FieldTransition) {
				if err := s.jira.TransitionIssue(ctx, jiraKey, "In Progress"); err != nil {
					s.logger.Warn("failed to transition JIRA issue to In Progress",
						"jira_key", jiraKey, "error", err)
				}
			}

			if s.botAccountID != "" && s.allowField(jiraKey, role, FieldAssignee) {
				if err := s.jira.AssignIssue(ctx, jiraKey, s.botAccountID); err != nil {
					s.logger.Warn("failed to assign JIRA issue to bot",
						"jira_key", jiraKey, "error", err)
				}
			}
		}
	}

	// Check for MR URL field — sync it as a remote link to JIRA.
	if mrURL := bead.Fields["mr_url"]; mrURL != "" {
		dedupKey := "mr:" + bead.ID + ":" + mrURL
		if !s.isDuplicate(dedupKey) && s.allowField(jiraKey, role, FieldRemoteLink) {
			s.logger.Info("syncing MR link to JIRA",
				"bead", bead.ID, "jira_key", jiraKey, "mr_url", mrURL, "role", role)

			title := "Merge Request: " + bead.Title
			if err := s.jira.AddRemoteLink(ctx, jiraKey, mrURL, title); err != nil {
				s.logger.Error("failed to add JIRA remote link",
					"jira_key", jiraKey, "mr_url", mrURL, "error", err)
			} else if s.allowComment(jiraKey, role, CommentInternal) {
				comment := withAttribution("Automated MR created: "+mrURL, attribution)
				if err := s.jira.AddCommentWithVisibility(ctx, jiraKey, comment, CommentInternal); err != nil {
					s.logger.Error("failed to add JIRA comment for MR",
						"jira_key", jiraKey, "error", err)
				}
			}
		}
	}

	// Check for mr_merged=true — transition JIRA issue to Review.
	if bead.Fields["mr_merged"] == "true" {
		dedupKey := "merged:" + bead.ID
		if !s.isDuplicate(dedupKey) {
			s.logger.Info("MR merged, transitioning JIRA to Review",
				"bead", bead.ID, "jira_key", jiraKey, "role", role)

			if s.allowComment(jiraKey, role, CommentInternal) {
				comment := withAttribution("MR merged — transitioning to Review.", attribution)
				if err := s.jira.AddCommentWithVisibility(ctx, jiraKey, comment, CommentInternal); err != nil {
					s.logger.Error("failed to add JIRA MR merged comment",
						"jira_key", jiraKey, "error", err)
				}
			}

			if !s.disableTransitions && s.allowField(jiraKey, role, FieldTransition) {
				if err := s.jira.TransitionIssue(ctx, jiraKey, "Review"); err != nil {
					s.logger.Warn("failed to transition JIRA issue to Review",
						"jira_key", jiraKey, "error", err)
				}
			}
		}
	}

}

// syncGBFSMTicket upserts the ONE GBFSM ticket per triaged FSM ticket (1:1 reshape
// ka-0000002637, Phase 2 ka-0000001491). It replaced the former per-mutation fan-out
// (1 analysis ticket + N Add-Label + 1 Set-Version + 1 link-back intent — those
// separate emitters are now retired, not live code) with a single create-or-edit of
// ONE GBFSM ticket carrying the analysis in its Description PLUS the classification
// labels and recommended Affects-Version as FIELDS. It is invoked only after the
// triage-role + in-scope checks have passed (see handleUpdated) and targets the GBFSM outbox
// project — NEVER the FSM ticket. fsmTargetKey is the bead's in-scope binding,
// carried only as DATA (the fsm_target_key custom field), so the bot never touches
// FSM. The bot holds ZERO FSM write; the operator-owned Automation rules (Phase 4)
// read the labels/version off this single ticket and apply them to FSM.
//
// Two default-closed inertness gates keep the whole path INERT on merge (zero prod
// behavior change), exactly like the surface it replaces:
//
//   - analysisIssueType == "" (the scaffold is not yet provisioned) ⇒ skip; AND
//   - GBFSM not writable (the JIRA_OUTBOX_ENABLED toggle, default off) ⇒ skip.
//
// The labels / version fields are additionally gated per-field (FieldLabels /
// FieldVersions) so an out-of-policy tier (or shadow mode) is honored — a gated-off
// field is simply omitted from the ticket; the analysis Description is still written.
//
// Upsert: SearchGBFSM finds an existing GBFSM ticket for this FSM key (idempotency
// keyed on FSM-N), so a re-triage EDITS the same ticket rather than creating a
// second. The in-memory dedup over (fsm, labels, version) additionally short-
// circuits an identical SSE replay within the TTL window before any Jira call.
func (s *JiraSync) syncGBFSMTicket(ctx context.Context, bead *BeadEvent, fsmTargetKey, role string) {
	// Gate 1: the GBFSM issue type must be scaffolded + configured. Empty is the
	// default (see outboxTypeAnalysis) — the surface stays completely inert, so
	// existing triage tests (which never set it) are unaffected.
	if s.analysisIssueType == "" {
		return
	}

	// Gate 2: GBFSM must be writable (the outbox toggle). Checked BEFORE consuming a
	// dedup slot so a toggle-off run re-fires cleanly once the operator flips it on.
	if !s.allowProject(outboxProjectKey) {
		s.logger.Warn("jira-sync: outbox disabled — skipping GBFSM ticket",
			"bead", bead.ID, "fsm_target_key", fsmTargetKey,
			"writable_projects", s.projectGateDiag())
		return
	}

	// Build the ONE GBFSM ticket this triage bead would upsert, with the
	// classification fields gated exactly as production requires (buildGBFSMTicket
	// is the single source of truth shared with the author-ahead backfill).
	ticket := s.buildGBFSMTicket(bead, fsmTargetKey, role)

	// Dedup over (fsm, labels, version): an identical replay within the TTL window
	// is a no-op (the ticket already reflects it), while a CHANGED classification
	// re-fires and upserts. Keyed on the FSM ticket, not the bead — a re-triage of
	// the same FSM ticket from a different bead maps to the same slot.
	if s.isDuplicate(ticket.DedupKey()) {
		return
	}

	// Read half of the upsert: find the existing GBFSM ticket for this FSM key.
	// SearchGBFSM fail-closes any non-GBFSM result, so the edit target below can
	// only ever be a GBFSM key — a crafted fsm_target_key cannot redirect the edit
	// onto an FSM key. On a search ERROR we abort WITHOUT writing (fail-safe against
	// creating a duplicate when the ticket may already exist).
	existing, err := s.jira.SearchGBFSM(ctx, fsmTargetKey)
	if err != nil {
		s.logger.Error("failed to search for existing GBFSM ticket — skipping (no write)",
			"bead", bead.ID, "fsm_target_key", fsmTargetKey, "error", err)
		return
	}

	if existing == nil {
		s.logger.Info("creating GBFSM ticket",
			"bead", bead.ID, "fsm_target_key", fsmTargetKey, "role", role,
			"issue_type", s.analysisIssueType, "labels", ticket.Labels,
			"affects_version", ticket.Version, "outbox_project", outboxProjectKey)
		if _, err := s.jira.CreateGBFSMTicket(ctx, ticket); err != nil {
			s.logger.Error("failed to create GBFSM ticket",
				"fsm_target_key", fsmTargetKey, "error", err)
		}
		return
	}

	// Upsert: edit the GBFSM key returned by search (never FSM-N).
	s.logger.Info("upserting (editing) GBFSM ticket",
		"bead", bead.ID, "fsm_target_key", fsmTargetKey, "gbfsm_key", existing.Key, "role", role,
		"labels", ticket.Labels, "affects_version", ticket.Version, "outbox_project", outboxProjectKey)
	if err := s.jira.EditGBFSMTicket(ctx, existing.Key, ticket); err != nil {
		s.logger.Error("failed to edit GBFSM ticket",
			"fsm_target_key", fsmTargetKey, "gbfsm_key", existing.Key, "error", err)
	}
}

// buildGBFSMTicket assembles the ONE GBFSM ticket a triage bead would upsert for
// its bound FSM ticket: the analysis Description plus the triage classification
// (labels + recommended Affects-Version) as FIELDS, each field independently
// gated (labels→FieldLabels, version→FieldVersions) so a revoked/shadowed field
// is dropped from the ticket while the analysis Description still lands. This
// preserves the allowlist semantics of the retired per-mutation intents.
//
// It is a PURE builder: it performs NO Jira I/O and NO write of any kind.
// BrowseURL just builds the FSM browse-URL string locally (embedded in the GBFSM
// ticket's OWN Description — the GBFSM→FSM half of the 1:1 link, Phase 3
// ka-0000001070 — never a write to FSM). Extracted from syncGBFSMTicket so the
// live upsert and the author-ahead backfill (jira_backfill.go, Phase 5a) compute
// the identical ticket shape from ONE code path and can never drift.
func (s *JiraSync) buildGBFSMTicket(bead *BeadEvent, fsmTargetKey, role string) GBFSMTicket {
	labels, version := triageClassification(bead)
	var gatedLabels []string
	if len(labels) > 0 && s.allowField(outboxProjectKey, role, FieldLabels) {
		gatedLabels = labels
	}
	var gatedVersion string
	if version != "" && s.allowField(outboxProjectKey, role, FieldVersions) {
		gatedVersion = version
	}
	return GBFSMTicket{
		IssueTypeID:  s.analysisIssueType,
		FSMTargetKey: fsmTargetKey,
		// GBFSM→FSM clickable link rendered in the Description (Phase 3
		// ka-0000001070). Display-only — BrowseURL just builds the FSM browse URL
		// string; it is embedded in the GBFSM ticket's own Description, so the bot
		// never touches FSM (ZERO-FSM-write invariant intact).
		FSMTargetURL:   s.jira.BrowseURL(fsmTargetKey),
		Summary:        analysisSummary(bead, fsmTargetKey),
		Body:           analysisBody(bead, fsmTargetKey),
		IdempotencyKey: outboxIdempotencyKey(fsmTargetKey, "gbfsm", ""),
		Labels:         gatedLabels,
		LabelsFieldID:  s.labelsFieldID,
		Version:        gatedVersion,
		VersionFieldID: s.versionFieldID,
	}
}

// analysisSummary is the one-line summary of the GBFSM analysis ticket: the FSM
// target plus the bead title when present, so the GBFSM board is legible.
func analysisSummary(bead *BeadEvent, fsmTargetKey string) string {
	if bead != nil {
		if t := strings.TrimSpace(bead.Title); t != "" {
			return fmt.Sprintf("Gardenio analysis: %s — %s", fsmTargetKey, t)
		}
	}
	return fmt.Sprintf("Gardenio analysis: %s", fsmTargetKey)
}

// analysisBody assembles the analysis Description from the bead. The SSE event
// carries no bead Description, so the body is composed from the signals actually
// present on the event — the title, the triage classification (severity / GxP
// from the labels), the recommended Affects-Version, and any free-form analysis
// fields the fsm-triage agent stamps (triage_reasoning / analysis_notes). Absent
// sections are simply omitted, so the body grows richer automatically as the
// triage agent's field contract expands, with no code change here. It always
// carries the FSM target + bead id for traceability.
//
// Audience is the FSM team's internal audience (scope item 5); GBFSM writes never
// email a customer, so quoting FSM content here is safe.
func analysisBody(bead *BeadEvent, fsmTargetKey string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Gardenio triage analysis for %s\n", fsmTargetKey)
	if bead != nil {
		if t := strings.TrimSpace(bead.Title); t != "" {
			fmt.Fprintf(&b, "\nTicket: %s\n", t)
		}
	}

	labels, version := triageClassification(bead)
	if sev := triageLabelValue(labels, "sev"); sev != "" {
		fmt.Fprintf(&b, "\nSeverity: %s", sev)
	}
	if gxp := triageLabelValue(labels, "gxp"); gxp != "" {
		fmt.Fprintf(&b, "\nGxP risk: %s", gxp)
	}
	if version != "" {
		fmt.Fprintf(&b, "\nRecommended Affects-Version: %s", version)
	}

	// Free-form analysis sections the triage agent may stamp on the bead. Emitted
	// in a stable order; each omitted when absent.
	if bead != nil {
		for _, sec := range []struct{ field, heading string }{
			{"triage_reasoning", "Triage reasoning"},
			{"sev_gxp_assessment", "Severity / GxP assessment"},
			{"log_findings", "Log findings"},
			{"code_findings", "Code findings"},
			{"repro_notes", "Reproduction notes"},
			{"analysis_notes", "Notes"},
		} {
			if v := strings.TrimSpace(bead.Fields[sec.field]); v != "" {
				fmt.Fprintf(&b, "\n\n%s:\n%s", sec.heading, v)
			}
		}
	}

	if bead != nil && strings.TrimSpace(bead.ID) != "" {
		fmt.Fprintf(&b, "\n\n(Source bead: %s)", strings.TrimSpace(bead.ID))
	}
	return b.String()
}

// triageLabelValue extracts the value of a triage classification label of the
// given kind ("sev" or "gxp") from the collected triage labels, tolerating both
// the canonical "gardenio-triaged-sev-N" and the abbreviated "sev-N" forms.
// Returns "" when no such label is present.
func triageLabelValue(labels []string, kind string) string {
	canonical := "gardenio-triaged-" + kind + "-"
	short := kind + "-"
	for _, l := range labels {
		l = strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(l, canonical):
			return strings.TrimPrefix(l, canonical)
		case strings.HasPrefix(l, short):
			return strings.TrimPrefix(l, short)
		}
	}
	return ""
}

func (s *JiraSync) handleClosed(ctx context.Context, data []byte) {
	bead := ParseBeadEvent(data)
	if bead == nil {
		return
	}

	// Dark beads never flow upstream — see handleUpdated (ka-0000005143).
	if hasLabel(bead.Labels, jiraDarkLabel) {
		s.logger.Info("jira-sync: skipping dark-marked bead on close (no upstream write-back)",
			"bead", bead.ID)
		return
	}

	// A mention-spawned agent bead carries jira_key (stamped at spawn,
	// jira_mentions.go) but is NOT a task whose closure means "the work landed":
	// it is the agent itself reaching terminal. Posting the generic "Task bead
	// closed (bead <agent-id>)" note here mislabels an agent's exit as a task
	// close and — since the JiraCompletionAcknowledger also fires on this exact
	// close (GAP 4 / ka-0000004899) — double-posts. The completion acknowledger
	// owns the conversational DONE/FAILED note for these beads, so skip them
	// here. The mention TASK bead (type=task) still gets its generic close note.
	if bead.Type == "agent" && bead.Fields["spawn_source"] == "jira-mention" {
		return
	}

	jiraKey := jiraKeyFromBead(*bead)
	if jiraKey == "" {
		return
	}

	// Deny-by-default: only write back to explicitly writable projects
	// (ka-0000005143). The config-driven write policy folds the former
	// allow/block sets + FSM toggle into one project-agnostic verdict
	// (ka-0000007252); the legacy coordinated gate is the fallback when unwired.
	if !s.allowProject(jiraKey) {
		s.logger.Warn("jira-sync: blocked close write-back to non-writable project",
			"bead", bead.ID, "jira_key", jiraKey, "project", jiraProjectKey(jiraKey),
			"writable_projects", s.projectGateDiag())
		return
	}

	// Bead→ticket scoping: the closing comment targets jiraKey, which must be
	// the ticket this bead is bound to (ka-0000001679).
	if !s.inScope(bead, jiraKey) {
		return
	}

	// Dedup: don't re-close the same bead.
	dedupKey := "close:" + bead.ID
	if s.isDuplicate(dedupKey) {
		return
	}

	role := s.actingRole(ctx, bead)
	s.logger.Info("syncing bead closure to JIRA",
		"bead", bead.ID, "jira_key", jiraKey, "role", role)

	// Post closing comment (no transition — transitions are triggered by mr_merged).
	if !s.allowComment(jiraKey, role, CommentInternal) {
		return
	}
	comment := "Task bead closed in beads system (bead " + bead.ID + ")."
	if mrURL := bead.Fields["mr_url"]; mrURL != "" {
		comment += " MR: " + mrURL
	}
	// Stamp operator attribution (ka-0000006003). Raw operated_by fallback — the
	// SSE close event carries only Fields, no daemon-resolved display name.
	comment = withAttribution(comment, operatorAttributionLine(bead.Fields["operated_by"], ""))
	if err := s.jira.AddCommentWithVisibility(ctx, jiraKey, comment, CommentInternal); err != nil {
		s.logger.Error("failed to add JIRA closing comment",
			"jira_key", jiraKey, "error", err)
	}
}

// triageRole is the beads role whose FSM classification (labels +
// Affects-Version) is emitted as GBFSM outbox intents instead of touching FSM
// directly (epic ka-0000002067). The outbox trigger in handleUpdated fires ONLY
// for this role, so every non-triage role's behavior is unchanged: a stray
// classification label on a crew bead is inert here.
const triageRole = "fsm-triage"

// triageClassification extracts the fsm-triage sync-back payload from a bead:
// the classification LABELS to add (gardenio-triaged, gardenio-triaged-sev-<N>,
// gardenio-triaged-gxp-<X>) and the
// recommended Affects-Version. It returns only recognized triage labels — never
// bookkeeping labels like source:jira or jira:<KEY> — so the additive label
// write to the customer service desk carries exactly the triage surface and
// nothing incidental. Both results may be empty (nothing to sync).
func triageClassification(bead *BeadEvent) (labels []string, version string) {
	if bead == nil {
		return nil, ""
	}
	for _, l := range bead.Labels {
		if isTriageLabel(l) {
			labels = append(labels, l)
		}
	}
	version = strings.TrimSpace(bead.Fields["recommended_affects_version"])
	return labels, version
}

// isTriageLabel reports whether l is one of the fsm-triage classification labels
// the bridge may sync to Jira. The canonical skill vocabulary (trigger prompt
// ka-0000002740; advice ka-0000003412 / ka-0000004718) is:
//
//   - "gardenio-triaged"                       — the "already triaged" sentinel
//   - "gardenio-triaged-sev-{0..3}"            — the classified severity
//   - "gardenio-triaged-gxp-{none,low,med,high}" — the classified GxP risk
//
// Matching the full "gardenio-triaged-sev-"/"gardenio-triaged-gxp-" prefixes (not
// the bare "sev-"/"gxp-" prefixes) mirrors exactly what fsm_label_write.sh
// stamps and what the trigger prompt directs the agent to write to the bead, so
// the sev/gxp labels are actually collected instead of silently dropped. The
// short "sev-"/"gxp-" prefixes are also accepted for robustness/back-compat with
// any bead that carries the abbreviated form. Prefix matching stays
// forward-compatible with new severity/impact values without a code change,
// while still excluding every non-triage label a bead carries (source:jira,
// jira:<KEY>, project labels, …).
func isTriageLabel(l string) bool {
	l = strings.TrimSpace(l)
	return l == "gardenio-triaged" ||
		strings.HasPrefix(l, "gardenio-triaged-sev-") ||
		strings.HasPrefix(l, "gardenio-triaged-gxp-") ||
		strings.HasPrefix(l, "sev-") ||
		strings.HasPrefix(l, "gxp-")
}

// jiraKeyFromBead extracts the JIRA key from a bead's labels or fields.
func jiraKeyFromBead(bead BeadEvent) string {
	// Check fields first (set by poller).
	if key := bead.Fields["jira_key"]; key != "" {
		return key
	}
	// Fallback: check labels for jira:<key> pattern.
	for _, label := range bead.Labels {
		if strings.HasPrefix(label, "jira:") && !strings.HasPrefix(label, "jira-label:") {
			return strings.TrimPrefix(label, "jira:")
		}
	}
	return ""
}

// isDuplicate returns true if the key was seen within the syncTTL window.
// If not, records the key and returns false.
func (s *JiraSync) isDuplicate(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Periodic cleanup.
	s.cleanupLocked()

	if t, ok := s.seen[key]; ok && time.Since(t) < syncTTL {
		return true
	}
	s.seen[key] = time.Now()
	return false
}

// cleanupLocked removes stale entries from the dedup map.
// Caller must hold s.mu.
func (s *JiraSync) cleanupLocked() {
	now := time.Now()
	for key, t := range s.seen {
		if now.Sub(t) > syncTTL {
			delete(s.seen, key)
		}
	}
}
