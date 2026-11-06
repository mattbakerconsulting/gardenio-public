package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// fakeDaemon is a hand-rolled beadAPI for unit tests.
type fakeDaemon struct {
	// ListSlowToolCalls hooks (/tool-latency).
	slowToolCallsResp       []*beadsapi.SlowToolCallRow
	slowToolCallsErr        error
	slowToolCallsLastFilter beadsapi.SlowToolCallFilter

	// listMu guards concurrent writes to listAllQ / listLastQ from handlers
	// that fan out ListBeadsFiltered across goroutines (e.g. handleIndex).
	listMu sync.Mutex
	// mintMu guards the CreateBead / GetBead bookkeeping (createCalls,
	// createReqs, createByIdemKey, getBeadCalls) so a test that drives the
	// AssistantTurnMinter's live and background-replay paths on two goroutines
	// (the concurrent live+backfill dedup case) is race-clean under `go test
	// -race`. Existing sequential tests are unaffected.
	mintMu      sync.Mutex
	beads       map[string]*beadsapi.BeadDetail
	deps        map[string][]beadsapi.Dependency
	revDeps     map[string][]beadsapi.Dependency
	comments    map[string][]beadsapi.Comment
	mentions    map[string][]beadsapi.BeadReferenceSummary
	mentionedBy map[string][]beadsapi.BeadReferenceSummary
	// mentionedByErr / mentionsErr force errors from the corresponding fetcher.
	mentionedByErr error
	mentionsErr    error

	// AddComment hooks.
	addCommentErr      error
	addCommentLastID   string
	addCommentLastAuth string
	addCommentLastText string
	addCommentLastCtx  context.Context
	addCommentCalls    int

	// Search-related hooks.
	semanticResp    *beadsapi.SemanticSearchResponse
	semanticErr     error
	semanticLastReq *beadsapi.SemanticSearchRequest
	listResp        *beadsapi.ListBeadsResult
	listErr         error
	listLastQ       *beadsapi.ListBeadsQuery
	listAllQ        []beadsapi.ListBeadsQuery
	// decisionResp, when non-nil, is returned for queries with Types=[decision]
	// instead of listResp. Lets a single fake satisfy /active's two-call shape.
	decisionResp *beadsapi.ListBeadsResult
	// formulaResp, when non-nil, is returned for queries with Types=[formula] —
	// the /formulas catalog and the plan-cockpit pour picker (ka-0000003236).
	// Defaults to an empty result so listResp fixtures don't leak into it.
	formulaResp *beadsapi.ListBeadsResult
	// agentRecentResp, when non-nil, is returned for queries with
	// Types=[agent] AND Statuses containing "closed" — i.e. /agents' second
	// call for recently-terminated agents. Active-agent queries still fall
	// through to listResp.
	agentRecentResp *beadsapi.ListBeadsResult
	// agentDormantResp, when non-nil, is returned for the /agents dormant-bucket
	// query — Types=[agent], Statuses containing "closed", and a field-equality
	// filter on agent_state=dormant (the engine-set parked-identity bucket,
	// ka-0000004675). Routed ahead of agentRecentResp so a test can seed the
	// parked population disjointly from recently-terminated agents.
	agentDormantResp *beadsapi.ListBeadsResult
	// liveAgentsResp, when set, is returned for queries with Types=[agent]
	// AND Statuses=[in_progress] — i.e. /index's right-rail "Live agents"
	// panel. Defaults to an empty result so legacy tests don't accidentally
	// leak listResp into the panel (ka-0000006596).
	liveAgentsResp *beadsapi.ListBeadsResult
	// mrResp / jiraResp, when set, route the /extern page's two queries:
	// Types=[merge-request] → mrResp, Labels containing "source:jira" →
	// jiraResp. Default to empty results so a single fake can seed each
	// source independently of listResp.
	mrResp   *beadsapi.ListBeadsResult
	jiraResp *beadsapi.ListBeadsResult
	// mergedMrResp, when set, routes the /u/{slug} "Merged MRs (14d)" query
	// (Types=[merge-request], Statuses=[closed]) SEPARATELY from the "Open MRs"
	// query (Statuses=[open,in_progress] → mrResp). Lets a test seed the two MR
	// populations disjointly so the merged-vs-open facet split (bug
	// ka-0000008262) is exercisable end-to-end. Nil ⇒ closed-MR queries fall
	// through to mrResp, preserving legacy single-population behavior.
	mergedMrResp *beadsapi.ListBeadsResult
	// squawkResp, when set, is returned for the /index Live-agents rail's
	// batched squawk-preview query (Types=[message], Labels=[squawk],
	// Sort=-created_at). Defaults to an empty result so legacy tests don't
	// accidentally leak listResp into the squawk-by-agent join (ka-0000003997).
	squawkResp *beadsapi.ListBeadsResult
	// agentMsgsResp, when set, is returned for the bead-show agent-authored
	// chat fetch — Types=[message,mail] filtered by a `from:<title>` label
	// (ka-0000002848). Defaults to an empty result so non-agent show tests
	// don't accidentally leak listResp into the chat panel.
	agentMsgsResp *beadsapi.ListBeadsResult
	// userResp, when set, is returned for the user-bead lookup
	// (Types=[user], Kinds=[config]) used by settings + /feed default-project
	// resolution (findUserBeadByEmail). Defaults to an empty result so legacy
	// tests don't accidentally leak listResp into the user-bead scan.
	userResp *beadsapi.ListBeadsResult
	// hotReadyResp, when set, is returned for the /index right-rail "Hot
	// ready" query (Statuses=[open], Sort=priority,-updated_at,
	// NoOpenDeps=true, Types=issueMacroTypes). Defaults to an empty result
	// so legacy tests don't accidentally leak listResp into the panel
	// (ka-0000006596, ka-0000008099).
	hotReadyResp *beadsapi.ListBeadsResult
	// hotEpicsResp, when set, is returned for queries with Types=[epic] —
	// i.e. /index's right-rail "Hot epics" panel. Defaults to an empty
	// result so legacy tests don't accidentally leak listResp into the
	// panel (ka-0000006022).
	hotEpicsResp *beadsapi.ListBeadsResult
	// p0ScanResp, when set, is returned for queries with Sort="priority"
	// and Kinds=[issue] — i.e. /index banner's P0-open scan. Defaults to
	// an empty result so legacy tests don't accidentally leak listResp
	// into the P0 count (ka-0000006022).
	p0ScanResp *beadsapi.ListBeadsResult
	// triggerScheduleResp, when set, is returned for queries with
	// Types=[trigger] AND Labels containing "trigger:schedule" — i.e. the
	// second list call in /triggers. Defaults to an empty result so legacy
	// tests don't accidentally double-count rows.
	triggerScheduleResp *beadsapi.ListBeadsResult
	// gardenerFieldOnlyResp, when set, is returned for gardener-list queries that
	// filter on the role=gardener FIELD (Fields["role"]=="gardener") rather than
	// the role:gardener label — the resilience path in loadGardenerRows that
	// surfaces gardeners created the "wrong way" (ka-0000001950). Defaults empty.
	gardenerFieldOnlyResp *beadsapi.ListBeadsResult
	// involveStepScanResp, when set, is returned for the plans-involvement
	// step-owner facet scan (Step 1, plans_involvement.go plansWithStepOwnedBy):
	// Kinds=[issue] + OperatedByAliases set + no Types. The returned beads' ids
	// are then mapped to parent plans via the forward-dep walk (f.deps). Defaults
	// empty so unrelated Kinds=[issue] queries are unaffected.
	involveStepScanResp *beadsapi.ListBeadsResult
	// involveDecisionScanResp, when set, is returned for the plans-involvement
	// decision-resolver facet scan (plans_involvement.go
	// plansWithDecisionResolvedBy): Types=[decision] + a responded_by field
	// filter. Routed ahead of the generic decision branch so the resolver scan is
	// seedable independently of the per-plan planDecisions response (decisionResp).
	// Defaults empty.
	involveDecisionScanResp *beadsapi.ListBeadsResult
	// queuedResp, when non-nil, is returned for queries with Statuses=[open]
	// (without Types=[decision]) — i.e. /active's third call counting queued
	// (non-agent open) work. Otherwise such queries fall through to listResp.
	queuedResp *beadsapi.ListBeadsResult
	// accountResp, when non-nil, is returned for queries with Types=[account] —
	// i.e. the /costs page and the /health costs tile. Defaults to an empty
	// result so legacy tests don't accidentally inherit cost rows.
	accountResp *beadsapi.ListBeadsResult
	// inProgressResp, when non-nil, is returned for queries with
	// Statuses=[in_progress] and no Types filter — i.e. the /health open-beads
	// tile's stale-in-progress scan.
	inProgressResp *beadsapi.ListBeadsResult
	// staleResp, when non-nil, is returned for the /index per-project stale
	// count: Statuses=[in_progress] + a non-zero UpdatedBefore cutoff
	// (ka-0000002729). Routed ahead of the plain in_progress branches so the
	// stale tally is seedable independently of the InProgress count.
	staleResp *beadsapi.ListBeadsResult

	// agentCountByField serves the CountOnly iteration-count queries
	// enrichGardenerRunCounts issues for legacy zero-tally gardeners
	// (ka-0000005661). Keyed "<fieldKey>=<value>" for total spawned and
	// "<fieldKey>=<value>|closed" for the closed/completed subset; the value is
	// returned as ListBeadsResult.Total. A missing key returns Total 0. Routed
	// ahead of the generic agent branches so the CountOnly shape is not hijacked
	// by liveAgentsResp / agentRecentResp.
	agentCountByField map[string]int

	// epicSubtreeCount serves the CountOnly subtree queries enrichGardenerOwnedEpics
	// issues for the per-gardener owned-epic rollup (ka-0000001266). Keyed
	// "<epicID>" for the total issue-subtree count and "<epicID>|closed" for the
	// closed subset; the value is returned as ListBeadsResult.Total. A missing key
	// returns Total 0. Routed by the Parent+CountOnly+Kinds=[issue] shape.
	epicSubtreeCount map[string]int

	// readyWorkCount serves the health tile's event-gardener ready-work probe
	// (eventGardenerRows, gd-fL4PM5z8kc): Parent=<epic>, NoOpenDeps=true,
	// CountOnly=true. Keyed on the epic id; the value is returned as
	// ListBeadsResult.Total (missing key → 0). readyWorkErr fails every probe.
	readyWorkCount map[string]int
	readyWorkErr   error

	// iterationGlyphBeads serves the recent-run glyph fallback fetch the /maps
	// board issues for gardeners with an empty ProgressState (bug ka-0000001069).
	// Keyed by gardener id; the matching iteration agent beads are returned as
	// ListBeadsResult.Beads. Routed by the Types=[agent] + non-CountOnly +
	// single trigger_id/schedule_id field + Sort=-created_at shape so it is not
	// hijacked by liveAgentsResp. A missing key returns an empty result.
	iterationGlyphBeads map[string][]*beadsapi.BeadDetail

	// MRBeadByURL hooks. The fake scans f.beads for a type=merge-request bead
	// whose fields.mr_url matches; tests can preempt that scan by setting
	// mrByURLErr (forces a hard error) or by leaving f.beads empty (the absent
	// path returns nil, nil).
	mrByURLErr   error
	mrByURLLast  string
	mrByURLCalls int

	// MRBeadsForAgent hooks. mrsForAgentErr forces a hard first-hop error so
	// tests can exercise the degraded path; mrsForAgentLast/Calls record the
	// last invocation so tests can assert call shape.
	mrsForAgentErr   error
	mrsForAgentLast  string
	mrsForAgentCalls int

	// escalationsResp is the canned response for ListEscalations. Empty slice
	// is a valid value — distinct from nil (interpreted as "no canned data").
	escalationsResp []beadsapi.EscalationDetail
	escalationsErr  error

	// Escalation approve/reject recording (ka-0000006884).
	approveEscCalls    []beadsapi.ApproveEscalationRequest
	approveEscIDs      []string
	approveEscErr      error
	rejectEscCalls     []beadsapi.RejectEscalationRequest
	routeEscIDs        []string
	routeEscCalls      []beadsapi.RouteEscalationRequest
	routeEscErr        error
	rejectEscIDs       []string
	rejectEscErr       error
	escalationsLastSt  string
	escalationsLastLim int
	// canApproveResp answers CanApproveEscalation by escalation id; a missing id
	// is {Allowed:false}. canApproveErr fails every call; canApproveIDs records
	// which escalations were asked about.
	canApproveResp map[string]beadsapi.CanApproveResult
	canApproveErr  error
	canApproveIDs  []string

	// GetEscalationPermissionDiff canned response (gd-SyEYkqe601).
	// permissionDiffOK false (the default) models the daemon's
	// `available: false`.
	permissionDiffResp   *beadsapi.SpawnPermissionDiff
	permissionDiffOK     bool
	permissionDiffErr    error
	permissionDiffCalls  int
	permissionDiffLastID string

	// Similar-related hooks.
	similarResp      *beadsapi.FindSimilarResponse
	similarErr       error
	similarLastID    string
	similarLastLimit int

	// Project-related hooks.
	projectsResp map[string]beadsapi.ProjectInfo
	projectsErr  error
	// projectsCalls counts ListProjectBeads invocations (guarded by listMu), so
	// a test can assert the planProjectCache memo collapses repeated /plans +
	// /deck round-trips into one upstream fetch (ka-0000006264).
	projectsCalls int

	// Role-registry hooks (role hub, role_hub.go). rolesResp is returned by
	// ListRoleBeads; rolesErr forces an error so the role-hub assembler's
	// best-effort degradation can be exercised.
	rolesResp map[string]beadsapi.RoleInfo
	rolesErr  error

	// ListAccountBeadsByProject hooks (project-hub accounts/costs fragments,
	// ka-0000004817). accountsByProjectResp is the canned per-project account
	// list; accountsByProjectErr forces the degraded path; LastArg records the
	// project the handler asked for.
	accountsByProjectResp    []beadsapi.AccountInfo
	accountsByProjectErr     error
	accountsByProjectLastArg string

	// ResolveDecision hooks.
	resolveErr     error
	resolveLastID  string
	resolveLastReq beadsapi.ResolveDecisionRequest
	resolveCalls   int
	// resolveOnSuccess, when non-nil, is invoked after a successful resolve to
	// mutate the fake's bead state (e.g. flip status to "closed" and set
	// resolved fields) so the post-resolve re-render reflects daemon state.
	resolveOnSuccess func(*fakeDaemon, string, beadsapi.ResolveDecisionRequest)

	// CancelDecision hooks (used by /b/{id}/decision/dismiss).
	cancelErr        error
	cancelLastID     string
	cancelLastReason string
	cancelLastBy     string
	cancelCalls      int
	// cancelOnSuccess mirrors resolveOnSuccess: invoked after a successful
	// cancel so tests can flip the fake bead's status to "closed" and set
	// the canceled fields the post-cancel re-render reads.
	cancelOnSuccess func(*fakeDaemon, string, string, string)

	// UpdateBead hooks (used by /b/{id}/reopen, claim, release).
	updateErr     error
	updateLastID  string
	updateLastReq beadsapi.UpdateBeadRequest
	updateCalls   int

	// UpdateBeadFieldsTyped hooks (used by ratings POST to update typed
	// fields on re-rate).
	updateFieldsErr     error
	updateFieldsLastID  string
	updateFieldsLastVal map[string]any
	updateFieldsCalls   int
	// updateFieldsHistory records every (id, fields) write in order so a test
	// that exercises a multi-write flow (e.g. the approve→build handler writes
	// the epic's plan_state THEN the gardener's enabled) can assert on each.
	updateFieldsHistory []updateFieldsCall

	// AppendBeadArrayField hooks (plan phase/step-order GROW primitive,
	// ka-0000000988). The fake reflects the append-if-absent onto the seeded
	// bead's Fields as a JSON array so a subsequent GetBead/re-render reads it.
	appendArrayErr   error
	appendArrayCalls int
	appendArrayLast  arrayFieldAppend

	// ReorderBeadArrayField hooks (plan phase/step-order CAS reorder primitive,
	// ka-0000000988). reorderArrayConflicts, when > 0, makes that many leading
	// calls return a 409 CAS conflict (decrementing) so a test can exercise the
	// retry loop; reorderArrayErr forces a non-CAS hard error. The fake also runs
	// a REAL compare-and-swap against the seeded field so a genuinely stale
	// `expected` conflicts on its own.
	reorderArrayErr       error
	reorderArrayConflicts int
	reorderArrayCalls     int
	reorderArrayLast      arrayFieldReorder

	// CloseBeadAs hooks (used by /b/{id}/close).
	closeErr    error
	closeLastID string
	closeLastBy string
	closeCalls  int

	// GetStats / ListConfigs hooks (used by /index banner).
	statsResp   *beadsapi.GraphStats
	statsErr    error
	configsByNS map[string][]beadsapi.ConfigEntry
	configsErr  error

	// PourFormula hooks (used by POST /formulas/{id}/pour).
	pourErr     error
	pourResp    *beadsapi.PourFormulaResponse
	pourLastID  string
	pourLastReq beadsapi.PourFormulaRequest
	pourCalls   int

	// CreateBead hooks (used by /views POST).
	createErr       error
	createResp      string // ID returned on success; defaults to "gd-new" when empty
	createLastReq   beadsapi.CreateBeadRequest
	createLastOpBy  string // beadsapi.OperatorFromContext at call time — guards X-Operated-By stamping
	createLastSysOp string // beadsapi.SystemOperatorFromContext at call time — guards WithSystemOperator stamping (plan-label reactor)
	createCalls     int

	// createLastMachine / updateFieldsLastMachine record whether the call's ctx
	// carried a per-call machine principal (beadsapi.WithMachinePrincipal) —
	// guards the reactors' own credential (gd-Hc34EENbO9).
	createLastMachine       bool
	updateFieldsLastMachine bool
	// updateLastMachine records whether the last UpdateBead (the parked-gardener
	// wake) carried a machine principal on its ctx.
	updateLastMachine bool

	// createReqs records every CreateBead request in order (createLastReq only
	// keeps the most recent). Lets a test inspect a multi-create flow — e.g.
	// the composer's head mint followed by its message bead.
	createReqs []beadsapi.CreateBeadRequest
	// createByIdemKey models the daemon's resolve-or-create dedup guard
	// (kbeads migration 029): a CreateBead with a non-empty IdempotencyKey
	// already seen returns the same id WITHOUT minting a second bead or
	// invoking createOnSuccess. This is what makes the composer double-submit
	// test prove "exactly one wish minted".
	createByIdemKey map[string]string
	// createIDForReq, when non-nil, computes the id a successful create
	// returns from the request (e.g. distinct ids for a head vs a message
	// bead). Nil falls back to createResp / "gd-new".
	createIDForReq func(beadsapi.CreateBeadRequest) string
	// createErrForReq, when non-nil and it returns a non-nil error, fails THAT
	// CreateBead (per-request). Symmetric with createIDForReq: it lets a test
	// fail one create in a multi-create flow while the others succeed — e.g. let
	// the plan epic mint but fail the chained author-assistant create to prove
	// the create→spawn chain is best-effort (the plan still exists).
	createErrForReq func(beadsapi.CreateBeadRequest) error
	// createOnSuccess, when non-nil, is invoked after a successful create so
	// tests can populate the fake's bead store with the resulting BeadDetail.
	createOnSuccess func(*fakeDaemon, string, beadsapi.CreateBeadRequest)

	// DeleteBead hooks (used by /views POST .../delete).
	deleteErr    error
	deleteLastID string
	deleteCalls  int

	// RecordUITelemetry hooks (Phase 9 sink for incident-intel clicks).
	uiTelemetryErr   error
	uiTelemetryLast  beadsapi.UITelemetryRow
	uiTelemetryCalls int

	// GetAcceptanceMetrics hooks (C5 operator-signals tile, ka-0000006219).
	// acceptanceResp/Err drive the canned response; Calls is the invocation
	// counter so tests can assert the tile reaches the daemon.
	acceptanceResp  *beadsapi.AcceptanceMetrics
	acceptanceErr   error
	acceptanceCalls int

	// AddDependency hooks (used by ratings POST to wire `rates` edges).
	addDepErr      error
	addDepCalls    int
	addDepLastBead string
	addDepLastDep  string
	addDepLastType string
	addDepLastBy   string

	removeDepErr   error
	removeDepCalls int
	removeDepLast  [3]string // bead, depends-on, type

	// AddLabel hooks (used by /quick-spawn to apply project: and role: labels).
	addLabelErr      error
	addLabelCalls    int
	addLabelLastBead string
	addLabelLastVal  string
	addLabelAll      []string

	// RemoveLabel hooks (used by bulk multi-select on /p/<project>).
	removeLabelErr      error
	removeLabelCalls    int
	removeLabelLastBead string
	removeLabelLastVal  string
	removeLabelAll      []string

	// EventStream hooks (used by /agents/{id}/events SSE proxy).
	eventStreamCh         chan beadsapi.SSEEvent
	eventStreamErr        error
	eventStreamCalls      int
	eventStreamLastTopics string

	// ListGlobalEvents hooks (durable /v1/events seed + reconnect backfill).
	listGlobalEventsResp    *beadsapi.ListGlobalEventsResponse
	listGlobalEventsErr     error
	listGlobalEventsCalls   int
	listGlobalEventsLastReq beadsapi.ListGlobalEventsRequest
	// listGlobalEventsFn, when set, is consulted instead of listGlobalEventsResp
	// so a test can hand back a different page per call (e.g. to simulate
	// cursor-based paging over more events than fit in one static fixture).
	listGlobalEventsFn func(req beadsapi.ListGlobalEventsRequest) (*beadsapi.ListGlobalEventsResponse, error)

	// GetEvents hooks (used by /b/{id}/activity).
	events      map[string][]beadsapi.Event
	eventsErr   error
	eventsCalls int
	getBeadErr  error // when non-nil, GetBead returns this regardless of map.
	// getBeadDelay, when non-zero, sleeps inside GetBead before returning.
	// Used by parallel-fan-out tests to prove concurrent execution
	// (peak-in-flight > 1, elapsed << serial floor). getBeadMu guards the
	// live/peak counters that those tests read.
	getBeadDelay time.Duration
	getBeadMu    sync.Mutex
	getBeadLive  int
	getBeadPeak  int

	// getBeadCalls / batchGetBeadsCalls count fetches so bundle tests can
	// assert the N+1 refactor: one BatchGetBeads call, zero per-child GetBead.
	getBeadCalls       int
	batchGetBeadsCalls int
	batchGetBeadsLastN int
	// batchGetBeadsAllIDs accumulates every id passed to BatchGetBeads across all
	// calls (deduped), so a test can assert WHICH children were hydrated — used by
	// the plans-index step-data type-filter test (ka-0000003113).
	batchGetBeadsAllIDs map[string]struct{}
	batchGetBeadsErr    error
	// batchGetBeadsHonorCtx makes BatchGetBeads return ctx.Err() when the passed
	// context is already done, so tests can exercise the hydration-budget detach
	// (ka-0000006335). Off by default — the live client surfaces a cancelled ctx
	// as a request error, which this mirrors.
	batchGetBeadsHonorCtx bool
	// moleculeUsageResp / moleculeUsageErr are the canned response and error
	// for RollupMoleculeUsage; moleculeUsageLastID records the requested id.
	moleculeUsageResp   *beadsapi.MoleculeUsage
	moleculeUsageErr    error
	moleculeUsageLastID string
	// batchGetDepsCalls counts BatchGetDependencies invocations; tests use
	// this to assert one batched fetch per outer level rather than N per
	// parent (ka-0000008009).
	batchGetDepsCalls         int
	batchGetDepsLastN         int
	batchGetDepsLastDirection string
	// depDelay sleeps inside GetDependencies / GetReverseDependencies, with
	// shared depCalls/Live/Peak counters for the constellation parallel
	// fan-out test. Guarded by getBeadMu (one mutex covers both maps).
	depDelay time.Duration
	depCalls int
	depLive  int
	depPeak  int

	// GetOperatorHistory hooks (used by /b/{id}/ownership).
	operatorHistory      map[string][]beadsapi.OperatorHistoryRow
	operatorHistoryErr   error
	operatorHistoryCalls int

	// ListFollowedBeadIDs hooks (drives the inbox UNION right side, ka-0000007204).
	// followedBeadIDsByOp returns IDs in the order test set them — already in
	// the daemon's "follow created_at DESC" order.
	followedBeadIDsByOp    map[int64][]string
	followedBeadIDsErr     error
	followedBeadIDsCalls   int
	followedBeadIDsLastOp  int64
	followedBeadIDsLastLim int

	// Bead-follower hooks (ka-0000003867 forward UI). followersByBead is the
	// initial state; AddBeadFollower/RemoveBeadFollower mutate it so tests can
	// drive the panel through add → list → remove without re-priming.
	followersByBead         map[string][]*beadsapi.BeadFollower
	followersErr            error
	followersAddErr         error
	followersRemoveErr      error
	followersAddCalls       int
	followersAddLastBead    string
	followersAddLastOp      int64
	followersAddLastNote    string
	followersRemoveCalls    int
	followersRemoveLastBead string
	followersRemoveLastOp   int64

	// LookupOperatorByAlias hooks. aliasResp is keyed by "kind|subject".
	aliasResp     map[string]*beadsapi.LookupOperatorByAliasResponse
	aliasErr      error
	aliasCalls    int
	aliasLastKind string
	aliasLastSubj string

	// LookupOperatorBySlug / GetOperatorSlug hooks (per-user homepage M1,
	// ka-0000000022). slugResp keyed by slug → response; opSlugResp keyed by
	// operator id → response. Defaults to Found=false when unset.
	slugResp   map[string]*beadsapi.LookupOperatorBySlugResponse
	opSlugResp map[int64]*beadsapi.GetOperatorSlugResponse

	// ListOperators hook (per-user homepage M2 §4 /people, ka-0000000340).
	// listOperatorsResp returns the canned directory; listOperatorsErr lets
	// tests force an error. listOperatorsCalls counts invocations so the
	// gardener-owner N+1 regression guard (ka-0000003665) can assert the full
	// operator-directory scan happens at most once per render rather than once
	// per Slack-owned gardener row. Guarded by getBeadMu (shared call-count mutex).
	listOperatorsResp  *beadsapi.ListOperatorsResponse
	listOperatorsErr   error
	listOperatorsCalls int

	// ListSlackOperatorAliases hook (ka-0000003119) — the id→@name resolver
	// feed backing the /people "recent requests" snippet Slack-mention
	// resolution. slackAliasesResp returns the canned alias feed;
	// slackAliasesErr forces an error; slackAliasesCalls counts invocations so
	// the single-round-trip posture is assertable. Guarded by getBeadMu.
	slackAliasesResp  *beadsapi.ListSlackOperatorAliasesResponse
	slackAliasesErr   error
	slackAliasesCalls int

	// operatorStatsResp returns the canned per-operator aggregate;
	// operatorStatsErr lets tests force an error. operatorStatsCalls counts
	// invocations. operatorStatsWindowHours records the recent-window argument
	// the caller passed, so a test can assert /people asks for an age-bounded
	// chip list rather than an unbounded one (ka-0000001991). Guarded by
	// getBeadMu (shared call-count mutex).
	operatorStatsResp        *beadsapi.OperatorStatsResponse
	operatorStatsErr         error
	operatorStatsCalls       int
	operatorStatsWindowHours int

	// GetBeadTaints / ListTaints hooks (bead-page taint badges, task
	// gd-jz56jXsHrK). beadTaints is keyed by bead ID; taintRegistryResp is the
	// canned GET /v1/taints response shared across every bead. Either *Err
	// forces the corresponding call to fail so a test can assert the
	// best-effort degrade path.
	beadTaints        map[string][]beadsapi.BeadTaint
	beadTaintsErr     error
	taintRegistryResp []beadsapi.TaintRegistryEntry
	taintRegistryErr  error

	// CreateTaint hooks (taints registry "New taint" action, task
	// gd-QXSHroFXwB). taintCreateReq captures the last request passed
	// through, for tests to assert the form → client wiring.
	taintCreateResp *beadsapi.CreatedTaint
	taintCreateErr  error
	taintCreateReq  *beadsapi.CreateTaintRequest

	// FindTeamBySlug / GetTeamTaintSummary hooks (team page, task
	// gd-NeEllAWKFw). teamsBySlug is keyed by the team's `team` slug field;
	// teamTaintSummaryResp is keyed by team bead ID.
	teamsBySlug          map[string]beadsapi.TeamInfo
	teamFindErr          error
	teamTaintSummaryResp map[string]*beadsapi.TeamTaintSummary
	teamTaintSummaryErr  error

	// ListReleases / GetRelease hooks (Release pages, release_page_test.go).
	// releases is every project's, newest cut first, as the daemon answers.
	releases    []beadsapi.Release
	releasesErr error

	// CreateScrubRequest hook (/me/taints "request detaint for all of mine",
	// task gd-k6NZx0hDpw; also the bead-page and taint-detail-page "Request
	// scrub" action, task gd-nOnc1Q8Zcv). scrubRequests records every call in
	// order; scrubRequestErr forces every call to fail (a test can key it by
	// taint via scrubRequestErrByTaint for a partial-failure scenario).
	scrubRequests          []beadsapi.CreateScrubRequestInput
	scrubRequestErr        error
	scrubRequestErrByTaint map[string]error
	scrubRequestResult     *beadsapi.CreateScrubRequestResult

	// /me/taints owns/created facet responses (myTaintedBeads, task
	// gd-k6NZx0hDpw). Routed in ListBeadsFiltered ahead of the generic
	// listResp fallthrough by query shape: a bare Owner filter is the owns
	// facet, a bare OperatedByAliases filter (no Types/Kinds/Statuses — the
	// shape no other production caller uses, see plans_involvement.go's
	// baseQuery-scoped variant and up.go's Statuses-scoped variant) is the
	// created facet. Kept distinct from listResp so a test can seed the two
	// facets with disjoint or overlapping bead sets independently.
	meTaintsOwnResp     *beadsapi.ListBeadsResult
	meTaintsOwnErr      error
	meTaintsCreatedResp *beadsapi.ListBeadsResult
	meTaintsCreatedErr  error

	// Taint detail page hooks (task gd-nOnc1Q8Zcv): GetTaint/UpdateTaintPolicy
	// back the Policy tab, ListTaintGrants/GrantTaint/RevokeTaint back the
	// Grants tab, ListTaintBeads backs the Beads tab, ListTaintScrubRequests/
	// ApproveScrubRequest/RejectScrubRequest back the Scrubs tab, and
	// ListTaintAudit backs the Audit tab. ApplyTaint also backs the bead-page
	// "Apply taint" action.
	getTaintResp            *beadsapi.TaintDetail
	getTaintErr             error
	updateTaintPolicyResp   *beadsapi.TaintDetail
	updateTaintPolicyErr    error
	updateTaintPolicyReq    json.RawMessage
	taintGrantsResp         []beadsapi.TaintGrantEntry
	taintGrantsErr          error
	grantTaintResp          *beadsapi.TaintGrantEntry
	grantTaintErr           error
	grantTaintReq           *beadsapi.GrantTaintRequest
	revokeTaintResp         bool
	revokeTaintErr          error
	revokeTaintPrincipal    string
	applyTaintResp          *beadsapi.BeadTaint
	applyTaintErr           error
	applyTaintCalls         []string
	taintBeadsResp          []beadsapi.TaintBeadEntry
	taintBeadsTotal         int
	taintBeadsErr           error
	taintScrubRequestsResp  []beadsapi.TaintScrubRequestEntry
	taintScrubRequestsTotal int
	taintScrubRequestsErr   error
	taintAuditResp          []beadsapi.TaintAuditEntry
	taintAuditTotal         int
	taintAuditErr           error
	approveScrubRequestResp *beadsapi.BeadDetail
	approveScrubRequestErr  error
	rejectScrubRequestResp  *beadsapi.BeadDetail
	rejectScrubRequestErr   error
}

func (f *fakeDaemon) ListBeadFollowers(_ context.Context, beadID string) ([]*beadsapi.BeadFollower, error) {
	if f.followersErr != nil {
		return nil, f.followersErr
	}
	return f.followersByBead[beadID], nil
}

func (f *fakeDaemon) AddBeadFollower(_ context.Context, beadID string, operatorID int64, note string) error {
	f.followersAddCalls++
	f.followersAddLastBead = beadID
	f.followersAddLastOp = operatorID
	f.followersAddLastNote = note
	if f.followersAddErr != nil {
		return f.followersAddErr
	}
	if f.followersByBead == nil {
		f.followersByBead = map[string][]*beadsapi.BeadFollower{}
	}
	for _, fol := range f.followersByBead[beadID] {
		if fol.OperatorID == operatorID {
			fol.Note = note
			return nil
		}
	}
	f.followersByBead[beadID] = append(f.followersByBead[beadID], &beadsapi.BeadFollower{
		BeadID:     beadID,
		OperatorID: operatorID,
		Note:       note,
	})
	return nil
}

func (f *fakeDaemon) RemoveBeadFollower(_ context.Context, beadID string, operatorID int64) error {
	f.followersRemoveCalls++
	f.followersRemoveLastBead = beadID
	f.followersRemoveLastOp = operatorID
	if f.followersRemoveErr != nil {
		return f.followersRemoveErr
	}
	if f.followersByBead == nil {
		return nil
	}
	rows := f.followersByBead[beadID]
	out := rows[:0]
	for _, fol := range rows {
		if fol.OperatorID == operatorID {
			continue
		}
		out = append(out, fol)
	}
	f.followersByBead[beadID] = out
	return nil
}

func (f *fakeDaemon) LookupOperatorByAlias(_ context.Context, kind, subject string) (*beadsapi.LookupOperatorByAliasResponse, error) {
	f.aliasCalls++
	f.aliasLastKind = kind
	f.aliasLastSubj = subject
	if f.aliasErr != nil {
		return nil, f.aliasErr
	}
	if r, ok := f.aliasResp[kind+"|"+subject]; ok {
		return r, nil
	}
	return &beadsapi.LookupOperatorByAliasResponse{Found: false}, nil
}

func (f *fakeDaemon) LookupOperatorBySlug(_ context.Context, slug string) (*beadsapi.LookupOperatorBySlugResponse, error) {
	if f.slugResp != nil {
		if r, ok := f.slugResp[slug]; ok {
			return r, nil
		}
	}
	return &beadsapi.LookupOperatorBySlugResponse{Found: false}, nil
}

func (f *fakeDaemon) GetOperatorSlug(_ context.Context, operatorID int64) (*beadsapi.GetOperatorSlugResponse, error) {
	if f.opSlugResp != nil {
		if r, ok := f.opSlugResp[operatorID]; ok {
			return r, nil
		}
	}
	return &beadsapi.GetOperatorSlugResponse{Found: false}, nil
}

func (f *fakeDaemon) ListOperators(_ context.Context, _ int) (*beadsapi.ListOperatorsResponse, error) {
	f.getBeadMu.Lock()
	f.listOperatorsCalls++
	f.getBeadMu.Unlock()
	if f.listOperatorsErr != nil {
		return nil, f.listOperatorsErr
	}
	if f.listOperatorsResp != nil {
		return f.listOperatorsResp, nil
	}
	return &beadsapi.ListOperatorsResponse{}, nil
}

func (f *fakeDaemon) ListSlackOperatorAliases(_ context.Context) (*beadsapi.ListSlackOperatorAliasesResponse, error) {
	f.getBeadMu.Lock()
	f.slackAliasesCalls++
	f.getBeadMu.Unlock()
	if f.slackAliasesErr != nil {
		return nil, f.slackAliasesErr
	}
	if f.slackAliasesResp != nil {
		return f.slackAliasesResp, nil
	}
	return &beadsapi.ListSlackOperatorAliasesResponse{}, nil
}

func (f *fakeDaemon) OperatorStats(_ context.Context, _, _, recentWindowHours int) (*beadsapi.OperatorStatsResponse, error) {
	f.getBeadMu.Lock()
	f.operatorStatsCalls++
	f.operatorStatsWindowHours = recentWindowHours
	f.getBeadMu.Unlock()
	if f.operatorStatsErr != nil {
		return nil, f.operatorStatsErr
	}
	if f.operatorStatsResp != nil {
		return f.operatorStatsResp, nil
	}
	return &beadsapi.OperatorStatsResponse{}, nil
}

func (f *fakeDaemon) GetBead(_ context.Context, id string) (*beadsapi.BeadDetail, error) {
	f.getBeadMu.Lock()
	f.getBeadCalls++
	f.getBeadLive++
	if f.getBeadLive > f.getBeadPeak {
		f.getBeadPeak = f.getBeadLive
	}
	delay := f.getBeadDelay
	f.getBeadMu.Unlock()

	if delay > 0 {
		time.Sleep(delay)
	}

	defer func() {
		f.getBeadMu.Lock()
		f.getBeadLive--
		f.getBeadMu.Unlock()
	}()

	if f.getBeadErr != nil {
		return nil, f.getBeadErr
	}
	b, ok := f.beads[id]
	if !ok {
		return nil, &beadsapi.APIError{StatusCode: http.StatusNotFound, Message: "not found"}
	}
	return b, nil
}

// BatchGetBeads mirrors the daemon's batch endpoint: present beads land in
// the result map; absent IDs are simply omitted (no error). Tests can set
// batchGetBeadsErr to force a hard failure.
func (f *fakeDaemon) BatchGetBeads(ctx context.Context, ids []string) (map[string]*beadsapi.BeadDetail, error) {
	// Guard the call-count bookkeeping — same rationale as BatchGetDependencies:
	// the two-wave /plans enrich (ka-0000004074) calls this fake concurrently, so
	// the shared counters need the lock (the real HTTP client is stateless).
	f.getBeadMu.Lock()
	f.batchGetBeadsCalls++
	f.batchGetBeadsLastN = len(ids)
	if f.batchGetBeadsAllIDs == nil {
		f.batchGetBeadsAllIDs = make(map[string]struct{}, len(ids))
	}
	for _, id := range ids {
		f.batchGetBeadsAllIDs[id] = struct{}{}
	}
	f.getBeadMu.Unlock()
	if f.batchGetBeadsErr != nil {
		return nil, f.batchGetBeadsErr
	}
	if f.batchGetBeadsHonorCtx {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	out := make(map[string]*beadsapi.BeadDetail, len(ids))
	for _, id := range ids {
		if b, ok := f.beads[id]; ok {
			out[id] = b
		}
	}
	return out, nil
}

// RollupMoleculeUsage returns the canned moleculeUsageResp/moleculeUsageErr.
// It records the requested id so tests can assert the handler forwards the
// path value unchanged.
func (f *fakeDaemon) RollupMoleculeUsage(_ context.Context, molID string) (*beadsapi.MoleculeUsage, error) {
	f.moleculeUsageLastID = molID
	if f.moleculeUsageErr != nil {
		return nil, f.moleculeUsageErr
	}
	return f.moleculeUsageResp, nil
}

// FindAgentBead scans the in-memory bead map for a type=agent bead whose
// fields.agent matches agentName. Closed/inactive beads are skipped to mirror
// the live daemon's active-only semantics.
func (f *fakeDaemon) FindAgentBead(_ context.Context, agentName string) (*beadsapi.BeadDetail, error) {
	for _, b := range f.beads {
		if b.Type != "agent" {
			continue
		}
		if b.Status == "closed" {
			continue
		}
		if b.Fields["agent"] == agentName {
			return b, nil
		}
	}
	return nil, beadsapi.ErrAgentNotFound
}

// FindMostRecentAgentBead mirrors the active-preferred + closed-fallback
// semantics of the live daemon client. The fake walks the in-memory bead
// map twice: first preferring active beads, then falling back to the most
// recent closed match.
func (f *fakeDaemon) FindMostRecentAgentBead(_ context.Context, agentName string) (*beadsapi.BeadDetail, error) {
	var active, closed *beadsapi.BeadDetail
	for _, b := range f.beads {
		if b.Type != "agent" {
			continue
		}
		if b.Fields["agent"] != agentName {
			continue
		}
		if b.Status == "closed" {
			if closed == nil || b.UpdatedAt.After(closed.UpdatedAt) {
				closed = b
			}
			continue
		}
		if active == nil || b.UpdatedAt.After(active.UpdatedAt) {
			active = b
		}
	}
	if active != nil {
		return active, nil
	}
	if closed != nil {
		return closed, nil
	}
	return nil, beadsapi.ErrAgentNotFound
}

// FindAgentBeadsByNames is the batched form: loops over names and reuses
// the per-name lookup. The handler test asserts call count = 1, so it
// counts invocations of *this* method, not the per-name fallback.
func (f *fakeDaemon) FindAgentBeadsByNames(ctx context.Context, names []string, _ []string) (map[string]*beadsapi.BeadDetail, error) {
	out := make(map[string]*beadsapi.BeadDetail, len(names))
	for _, n := range names {
		b, err := f.FindMostRecentAgentBead(ctx, n)
		if err != nil {
			continue
		}
		out[n] = b
	}
	return out, nil
}

func (f *fakeDaemon) GetDependencies(_ context.Context, id string) ([]beadsapi.Dependency, error) {
	f.depCallStart()
	defer f.depCallEnd()
	return f.deps[id], nil
}

func (f *fakeDaemon) GetBeadTaints(_ context.Context, id string) ([]beadsapi.BeadTaint, error) {
	if f.beadTaintsErr != nil {
		return nil, f.beadTaintsErr
	}
	return f.beadTaints[id], nil
}

func (f *fakeDaemon) ListTaints(_ context.Context) ([]beadsapi.TaintRegistryEntry, error) {
	if f.taintRegistryErr != nil {
		return nil, f.taintRegistryErr
	}
	return f.taintRegistryResp, nil
}

func (f *fakeDaemon) ListSlowToolCalls(_ context.Context, filter beadsapi.SlowToolCallFilter) ([]*beadsapi.SlowToolCallRow, error) {
	f.slowToolCallsLastFilter = filter
	if f.slowToolCallsErr != nil {
		return nil, f.slowToolCallsErr
	}
	return f.slowToolCallsResp, nil
}

func (f *fakeDaemon) CreateTaint(_ context.Context, req beadsapi.CreateTaintRequest) (*beadsapi.CreatedTaint, error) {
	f.taintCreateReq = &req
	if f.taintCreateErr != nil {
		return nil, f.taintCreateErr
	}
	if f.taintCreateResp != nil {
		return f.taintCreateResp, nil
	}
	return &beadsapi.CreatedTaint{ID: 1, Name: req.Name, OwnerTeam: req.OwnerTeam}, nil
}

func (f *fakeDaemon) FindTeamBySlug(_ context.Context, slug string) (beadsapi.TeamInfo, error) {
	if f.teamFindErr != nil {
		return beadsapi.TeamInfo{}, f.teamFindErr
	}
	team, ok := f.teamsBySlug[slug]
	if !ok {
		return beadsapi.TeamInfo{}, fmt.Errorf("team %q not found", slug)
	}
	return team, nil
}

func (f *fakeDaemon) GetTeamTaintSummary(_ context.Context, teamID string) (*beadsapi.TeamTaintSummary, error) {
	if f.teamTaintSummaryErr != nil {
		return nil, f.teamTaintSummaryErr
	}
	if s, ok := f.teamTaintSummaryResp[teamID]; ok {
		return s, nil
	}
	return &beadsapi.TeamTaintSummary{}, nil
}

func (f *fakeDaemon) CreateScrubRequest(_ context.Context, in beadsapi.CreateScrubRequestInput) (*beadsapi.CreateScrubRequestResult, error) {
	f.scrubRequests = append(f.scrubRequests, in)
	if f.scrubRequestErrByTaint != nil {
		if err, ok := f.scrubRequestErrByTaint[in.Taint]; ok && err != nil {
			return nil, err
		}
	}
	if f.scrubRequestErr != nil {
		return nil, f.scrubRequestErr
	}
	if f.scrubRequestResult != nil {
		return f.scrubRequestResult, nil
	}
	return &beadsapi.CreateScrubRequestResult{
		Bead:        &beadsapi.BeadDetail{ID: "kd-scrub-" + in.Taint, Type: "scrub-request"},
		ResolvedIDs: in.BeadIDs,
		DetaintMode: "fork",
	}, nil
}

func (f *fakeDaemon) GetTaint(_ context.Context, name string) (*beadsapi.TaintDetail, error) {
	if f.getTaintErr != nil {
		return nil, f.getTaintErr
	}
	if f.getTaintResp != nil {
		return f.getTaintResp, nil
	}
	return &beadsapi.TaintDetail{Name: name}, nil
}

func (f *fakeDaemon) UpdateTaintPolicy(_ context.Context, name string, scrubPolicy json.RawMessage) (*beadsapi.TaintDetail, error) {
	f.updateTaintPolicyReq = scrubPolicy
	if f.updateTaintPolicyErr != nil {
		return nil, f.updateTaintPolicyErr
	}
	if f.updateTaintPolicyResp != nil {
		return f.updateTaintPolicyResp, nil
	}
	return &beadsapi.TaintDetail{Name: name, ScrubPolicy: scrubPolicy}, nil
}

func (f *fakeDaemon) ListTaintGrants(_ context.Context, _ string) ([]beadsapi.TaintGrantEntry, error) {
	if f.taintGrantsErr != nil {
		return nil, f.taintGrantsErr
	}
	return f.taintGrantsResp, nil
}

func (f *fakeDaemon) GrantTaint(_ context.Context, _ string, req beadsapi.GrantTaintRequest) (*beadsapi.TaintGrantEntry, error) {
	f.grantTaintReq = &req
	if f.grantTaintErr != nil {
		return nil, f.grantTaintErr
	}
	if f.grantTaintResp != nil {
		return f.grantTaintResp, nil
	}
	return &beadsapi.TaintGrantEntry{Principal: req.Principal, ExpiresAt: req.Until}, nil
}

func (f *fakeDaemon) RevokeTaint(_ context.Context, _, principal string) (bool, error) {
	f.revokeTaintPrincipal = principal
	if f.revokeTaintErr != nil {
		return false, f.revokeTaintErr
	}
	return f.revokeTaintResp, nil
}

func (f *fakeDaemon) ApplyTaint(_ context.Context, beadID, taintName string) (*beadsapi.BeadTaint, error) {
	f.applyTaintCalls = append(f.applyTaintCalls, beadID+":"+taintName)
	if f.applyTaintErr != nil {
		return nil, f.applyTaintErr
	}
	if f.applyTaintResp != nil {
		return f.applyTaintResp, nil
	}
	return &beadsapi.BeadTaint{TaintName: taintName, Source: "explicit"}, nil
}

func (f *fakeDaemon) ListTaintBeads(_ context.Context, _ string, _, _ int) ([]beadsapi.TaintBeadEntry, int, error) {
	if f.taintBeadsErr != nil {
		return nil, 0, f.taintBeadsErr
	}
	return f.taintBeadsResp, f.taintBeadsTotal, nil
}

func (f *fakeDaemon) ListTaintScrubRequests(_ context.Context, _ string, _, _ int) ([]beadsapi.TaintScrubRequestEntry, int, error) {
	if f.taintScrubRequestsErr != nil {
		return nil, 0, f.taintScrubRequestsErr
	}
	return f.taintScrubRequestsResp, f.taintScrubRequestsTotal, nil
}

func (f *fakeDaemon) ListTaintAudit(_ context.Context, _ string, _ beadsapi.TaintAuditFilter) ([]beadsapi.TaintAuditEntry, int, error) {
	if f.taintAuditErr != nil {
		return nil, 0, f.taintAuditErr
	}
	return f.taintAuditResp, f.taintAuditTotal, nil
}

func (f *fakeDaemon) ApproveScrubRequest(_ context.Context, beadID, _ string) (*beadsapi.BeadDetail, error) {
	if f.approveScrubRequestErr != nil {
		return nil, f.approveScrubRequestErr
	}
	if f.approveScrubRequestResp != nil {
		return f.approveScrubRequestResp, nil
	}
	return &beadsapi.BeadDetail{ID: beadID}, nil
}

func (f *fakeDaemon) RejectScrubRequest(_ context.Context, beadID, _ string) (*beadsapi.BeadDetail, error) {
	if f.rejectScrubRequestErr != nil {
		return nil, f.rejectScrubRequestErr
	}
	if f.rejectScrubRequestResp != nil {
		return f.rejectScrubRequestResp, nil
	}
	return &beadsapi.BeadDetail{ID: beadID}, nil
}

func (f *fakeDaemon) GetReverseDependencies(_ context.Context, id string) ([]beadsapi.Dependency, error) {
	f.depCallStart()
	defer f.depCallEnd()
	return f.revDeps[id], nil
}

// BatchGetDependencies mirrors the daemon's batch endpoint: every requested
// ID lands in the returned map (nil slice when the fake has no edges for it).
// Tests that want to assert "exactly one batched call per outer level" inspect
// batchGetDepsCalls / batchGetDepsLastDirection / batchGetDepsLastN.
func (f *fakeDaemon) BatchGetDependencies(_ context.Context, ids []string, direction string) (map[string][]beadsapi.Dependency, error) {
	// Guard the call-count bookkeeping: the /plans enrich pass now fans its bulk
	// joins out across two concurrent waves (ka-0000004074), so several goroutines
	// may hit this fake at once. The real beadsapi HTTP client is stateless per
	// request; only the fake's shared counters need the lock. The Last* fields
	// become nondeterministic under that concurrency, but no concurrent-path test
	// reads them (only single-threaded bundle/constellation/ratings tests do).
	f.getBeadMu.Lock()
	f.batchGetDepsCalls++
	f.batchGetDepsLastDirection = direction
	f.batchGetDepsLastN = len(ids)
	f.getBeadMu.Unlock()
	out := make(map[string][]beadsapi.Dependency, len(ids))
	src := f.deps
	if direction == "reverse" {
		src = f.revDeps
	}
	for _, id := range ids {
		out[id] = src[id]
	}
	return out, nil
}

// depCallStart / depCallEnd track concurrency of GetDependencies + GetReverseDependencies
// for parallel-fan-out tests (constellation, etc.) and honor the optional
// depDelay knob so tests can prove fan-out actually overlaps RTTs.
func (f *fakeDaemon) depCallStart() {
	f.getBeadMu.Lock()
	delay := f.depDelay
	f.depCalls++
	f.depLive++
	if f.depLive > f.depPeak {
		f.depPeak = f.depLive
	}
	f.getBeadMu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
}

func (f *fakeDaemon) depCallEnd() {
	f.getBeadMu.Lock()
	f.depLive--
	f.getBeadMu.Unlock()
}

func (f *fakeDaemon) GetMentionedBy(_ context.Context, id string, _ int) ([]beadsapi.BeadReferenceSummary, error) {
	if f.mentionedByErr != nil {
		return nil, f.mentionedByErr
	}
	return f.mentionedBy[id], nil
}

func (f *fakeDaemon) GetMentions(_ context.Context, id string, _ int) ([]beadsapi.BeadReferenceSummary, error) {
	if f.mentionsErr != nil {
		return nil, f.mentionsErr
	}
	return f.mentions[id], nil
}

func (f *fakeDaemon) GetComments(_ context.Context, id string) ([]beadsapi.Comment, error) {
	return f.comments[id], nil
}

func (f *fakeDaemon) GetEvents(_ context.Context, id string) ([]beadsapi.Event, error) {
	f.eventsCalls++
	if f.eventsErr != nil {
		return nil, f.eventsErr
	}
	return f.events[id], nil
}

func (f *fakeDaemon) GetRecentEvents(_ context.Context, id string, limit int) ([]beadsapi.Event, error) {
	f.eventsCalls++
	if f.eventsErr != nil {
		return nil, f.eventsErr
	}
	all := f.events[id]
	if limit > 0 && len(all) > limit {
		return all[len(all)-limit:], nil
	}
	return all, nil
}

func (f *fakeDaemon) GetOperatorHistory(_ context.Context, id string) ([]beadsapi.OperatorHistoryRow, error) {
	f.operatorHistoryCalls++
	if f.operatorHistoryErr != nil {
		return nil, f.operatorHistoryErr
	}
	return f.operatorHistory[id], nil
}

func (f *fakeDaemon) AddComment(ctx context.Context, id, author, text string) error {
	f.addCommentCalls++
	f.addCommentLastID = id
	f.addCommentLastAuth = author
	f.addCommentLastText = text
	f.addCommentLastCtx = ctx
	if f.addCommentErr != nil {
		return f.addCommentErr
	}
	f.comments[id] = append(f.comments[id], beadsapi.Comment{
		Author:    author,
		Text:      text,
		CreatedAt: time.Date(2026, 4, 24, 17, 0, 0, 0, time.UTC),
	})
	return nil
}

func (f *fakeDaemon) SemanticSearch(_ context.Context, req beadsapi.SemanticSearchRequest) (*beadsapi.SemanticSearchResponse, error) {
	r := req
	f.semanticLastReq = &r
	if f.semanticErr != nil {
		return nil, f.semanticErr
	}
	return f.semanticResp, nil
}

func (f *fakeDaemon) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	qq := q
	f.listMu.Lock()
	f.listLastQ = &qq
	f.listAllQ = append(f.listAllQ, qq)
	f.listMu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	// Thread-identity label lookup (composer mint fast-path,
	// thread_compose.go): a single-label query on slack-thread:{c}:{ts}
	// resolves to the matching seeded head in f.beads. Routed first so the
	// generic switch below doesn't hijack it.
	if len(q.Labels) == 1 && strings.HasPrefix(q.Labels[0], "slack-thread:") {
		var out []*beadsapi.BeadDetail
		for _, b := range f.beads {
			if b != nil && slices.Contains(b.Labels, q.Labels[0]) {
				out = append(out, b)
			}
		}
		return &beadsapi.ListBeadsResult{Beads: out, Total: len(out)}, nil
	}
	// Thread aggregator queries (Phase 2, threads.go) — routed ahead of every
	// generic branch and driven straight off f.beads so the /threads handlers can
	// be exercised with the package-wide fake instead of the narrow threadFake.
	// Both branches key on thread-only shapes (the "thread" head label and the
	// "thread_id" field) that no other query uses, so they never perturb an
	// existing fixture.
	//
	// thread_id field-equality membership query (threadList per-head tally +
	// threadMembers field half): return every seeded bead whose fields.thread_id
	// matches, regardless of status (membership is lifecycle-agnostic).
	if want, ok := q.Fields[threadIDField]; ok && len(q.Fields) == 1 {
		var out []*beadsapi.BeadDetail
		for _, b := range f.beads {
			if b != nil && b.Fields[threadIDField] == want {
				out = append(out, b)
			}
		}
		return &beadsapi.ListBeadsResult{Beads: out, Total: len(out)}, nil
	}
	// Thread-head enumeration (threadList): every bead carrying ALL requested
	// labels (the "thread" head label, plus an optional project:<name> scope).
	// threadList re-sorts its summaries by LastActivity, so the fake leaves the
	// result order to map iteration — the handler's final sort is what tests
	// assert against.
	if slices.Contains(q.Labels, threadHeadLabel) {
		var out []*beadsapi.BeadDetail
		for _, b := range f.beads {
			if b == nil {
				continue
			}
			all := true
			for _, want := range q.Labels {
				if !slices.Contains(b.Labels, want) {
					all = false
					break
				}
			}
			if all {
				out = append(out, b)
			}
		}
		return &beadsapi.ListBeadsResult{Beads: out, Total: len(out)}, nil
	}
	// plans-involvement decision-resolver facet scan (plans_involvement.go
	// plansWithDecisionResolvedBy): Types=[decision] filtered by a responded_by
	// field. Routed ahead of the generic decision branch (which serves the
	// per-plan planDecisions list) so the resolver scan is seedable independently.
	if slices.Contains(q.Types, "decision") && q.Fields["responded_by"] != "" {
		if f.involveDecisionScanResp != nil {
			return f.involveDecisionScanResp, nil
		}
		return &beadsapi.ListBeadsResult{}, nil
	}
	// requesting_plan pin scan (Layer 2, ka-0000007801) — Types=[decision]
	// filtered by the requesting_plan field. Serves OPEN decisions whose
	// fields.requesting_plan matches, straight off f.beads, so a decision pinned
	// to a plan but NOT a parent-child child is seedable distinctly from the
	// parent-subtree query (decisionResp). Routed ahead of the generic decision
	// branch so the pin scan isn't hijacked by the per-plan list response.
	if slices.Contains(q.Types, "decision") {
		if want, ok := q.Fields["requesting_plan"]; ok {
			var out []*beadsapi.BeadDetail
			for _, b := range f.beads {
				if b == nil || b.Type != "decision" || b.Fields["requesting_plan"] != want {
					continue
				}
				if len(q.Statuses) > 0 && statusOpenSortKey(b.Status) != 0 {
					continue // pin scan asks for open forks only
				}
				out = append(out, b)
			}
			return &beadsapi.ListBeadsResult{Beads: out, Total: len(out)}, nil
		}
	}
	// /agents/chat decision family (agent_chat.go, Task 1 gd-jqdZXW0ZdQ): Types=
	// [decision], Statuses includes "closed" — the first caller wanting both
	// open and resolved decisions in one shot (every other decision-list
	// caller above asks Statuses=[open] only). Routed ahead of the generic
	// decisionResp branch so the chat membership enrich is seedable off
	// f.beads directly.
	if slices.Contains(q.Types, "decision") && slices.Contains(q.Statuses, "closed") {
		var out []*beadsapi.BeadDetail
		for _, b := range f.beads {
			if b != nil && b.Type == "decision" {
				out = append(out, b)
			}
		}
		return &beadsapi.ListBeadsResult{Beads: out, Total: len(out)}, nil
	}
	if slices.Contains(q.Types, "decision") {
		if f.decisionResp != nil {
			return f.decisionResp, nil
		}
		return &beadsapi.ListBeadsResult{}, nil
	}
	// requesting_plan escalation pin (Layer 2, ka-0000007801): the page-wide open
	// escalation query planEscalationsBulk buckets by fields.requesting_plan, and
	// the single-epic pin query resolvePlanEscalationInterest issues. Both are
	// Types=[escalation]; serve open escalations straight off f.beads (honouring
	// a requesting_plan field filter when present) so both the render bucket and
	// the SSE interest set are seedable. No other production query lists
	// Types=[escalation] via ListBeadsFiltered, so this never hijacks a fixture.
	if slices.Contains(q.Types, "escalation") {
		want, byPin := q.Fields["requesting_plan"]
		var out []*beadsapi.BeadDetail
		for _, b := range f.beads {
			if b == nil || b.Type != "escalation" {
				continue
			}
			if len(q.Statuses) > 0 && statusOpenSortKey(b.Status) != 0 {
				continue
			}
			if byPin && b.Fields["requesting_plan"] != want {
				continue
			}
			out = append(out, b)
		}
		return &beadsapi.ListBeadsResult{Beads: out, Total: len(out)}, nil
	}
	// /agents/chat report family (agent_chat.go): Types=[report],
	// Statuses=[open,closed]. Serves every type=report bead straight off
	// f.beads so the decision_id nesting (a report's only membership path,
	// spec §5.1) is seedable. No other production query lists
	// Types=[report] via ListBeadsFiltered.
	if slices.Contains(q.Types, "report") {
		var out []*beadsapi.BeadDetail
		for _, b := range f.beads {
			if b != nil && b.Type == "report" {
				out = append(out, b)
			}
		}
		return &beadsapi.ListBeadsResult{Beads: out, Total: len(out)}, nil
	}
	// Formula catalog / plan-pour picker (ka-0000003236): Types=[formula].
	// Routes to formulaResp when set so the plan-cockpit pour picker can be
	// seeded independently of listResp; otherwise empty.
	if slices.Contains(q.Types, "formula") {
		if f.formulaResp != nil {
			return f.formulaResp, nil
		}
		return &beadsapi.ListBeadsResult{}, nil
	}
	// /extern page: merge-request beads vs source:jira beads. Routed ahead of
	// the generic branches so the two-source page seeds each independently.
	if slices.Contains(q.Types, "merge-request") {
		// /u/{slug} splits MRs by status: closed → "Merged MRs (14d)",
		// open/in_progress → "Open MRs". When mergedMrResp is set, route the
		// closed query to it so a test can seed the two populations disjointly
		// (merged-vs-open facet split, bug ka-0000008262). Otherwise both fall
		// through to mrResp (legacy single-population behavior).
		if f.mergedMrResp != nil && slices.Contains(q.Statuses, "closed") {
			return f.mergedMrResp, nil
		}
		if f.mrResp != nil {
			return f.mrResp, nil
		}
		return &beadsapi.ListBeadsResult{}, nil
	}
	if slices.Contains(q.Labels, "source:jira") {
		if f.jiraResp != nil {
			return f.jiraResp, nil
		}
		return &beadsapi.ListBeadsResult{}, nil
	}
	// User-bead lookup (settings + /feed default-project resolution):
	// Types=[user], Kinds=[config]. Routes to userResp when set so the viewer's
	// stored preferences can be seeded independently; otherwise falls through to
	// listResp, which the settings tests use to seed the user bead.
	if f.userResp != nil && slices.Contains(q.Types, "user") && slices.Contains(q.Kinds, "config") {
		return f.userResp, nil
	}
	// bead-show agent-authored chat fetch — Types=[message,mail] filtered by a
	// `from:<title>` label (agentAuthoredMessagesQuery, ka-0000002848). Routed
	// ahead of the squawk-preview branch so it isn't hijacked by the generic
	// message route.
	for _, l := range q.Labels {
		if strings.HasPrefix(l, "from:") {
			if f.agentMsgsResp != nil {
				return f.agentMsgsResp, nil
			}
			return &beadsapi.ListBeadsResult{}, nil
		}
	}
	// /people "recent requests" strip (people.go lookupRecentThreadMessages,
	// ka-0000005614): Types=[message], Labels=[slack-thread-message],
	// Sort=-created_at, no fields. Returns every seeded type=message bead
	// carrying the label, sorted newest-first, so the per-operator grouping and
	// cap the handler applies are testable off the same f.beads fixtures.
	// Routed ahead of the plan-transcript / squawk message branches so it isn't
	// hijacked (its label is neither from:/plan_id nor squawk).
	if slices.Contains(q.Types, "message") && slices.Contains(q.Labels, "slack-thread-message") {
		var out []*beadsapi.BeadDetail
		for _, b := range f.beads {
			if b != nil && b.Type == "message" && slices.Contains(b.Labels, "slack-thread-message") {
				out = append(out, b)
			}
		}
		sort.SliceStable(out, func(i, j int) bool {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		})
		return &beadsapi.ListBeadsResult{Beads: out, Total: len(out)}, nil
	}
	// plan-transcript reverse lookup (plans_assistant_prompt.go
	// planTranscriptView, task 2b ka-0000004607): Types=[message],
	// Fields={plan_id:<epic>}, NO from:/LabelsAny — the durable transcript-turn
	// beads task 2a minted, bound to the plan by fields.plan_id. Routed ahead of
	// the generic message branches (which key on Labels=[squawk]) so the
	// field-scoped chat fetch is testable off the same seeded f.beads fixtures.
	if slices.Contains(q.Types, "message") && len(q.LabelsAny) == 0 {
		if want, ok := q.Fields[planTranscriptPlanIDField]; ok && len(q.Fields) == 1 {
			var out []*beadsapi.BeadDetail
			for _, b := range f.beads {
				if b != nil && b.Type == "message" && b.Fields[planTranscriptPlanIDField] == want {
					out = append(out, b)
				}
			}
			return &beadsapi.ListBeadsResult{Beads: out, Total: len(out)}, nil
		}
	}
	// /agents/chat message+mail family (agent_chat.go, Task 1 gd-jqdZXW0ZdQ):
	// Types=[message, mail], no Labels — the membership enrich's one combined
	// family list covering squawks, mail, and generic chat lines. None of the
	// label/field-scoped message branches above match an unlabeled query, so
	// this is safe to serve straight off f.beads.
	if slices.Contains(q.Types, "message") && slices.Contains(q.Types, "mail") && len(q.Labels) == 0 {
		var out []*beadsapi.BeadDetail
		for _, b := range f.beads {
			if b != nil && (b.Type == "message" || b.Type == "mail") {
				out = append(out, b)
			}
		}
		return &beadsapi.ListBeadsResult{Beads: out, Total: len(out)}, nil
	}
	// plan-gardener fields.plan_id reverse lookup (plans_assistant.go
	// planAssistant path b): Types=[agent], Labels=[role:plan-gardener],
	// Fields={plan_id:<epic>}. The resolver keys on the single role:plan-gardener
	// label (the plan-assistant dual-read was removed, decision ka-0000007411),
	// so this fake matches on the requested Labels (AND). Scan f.beads for agent
	// beads carrying the role label + plan_id so the field-binding discovery path
	// is testable off the same seeded fixtures as the assigned-edge path.
	beadMatchesLabels := func(b *beadsapi.BeadDetail) bool {
		for _, want := range q.Labels {
			if !slices.Contains(b.Labels, want) {
				return false
			}
		}
		return true
	}
	if slices.Contains(q.Types, "agent") && slices.Contains(q.Labels, "role:plan-gardener") {
		if want, ok := q.Fields["plan_id"]; ok {
			var out []*beadsapi.BeadDetail
			for _, b := range f.beads {
				if b != nil && beadMatchesLabels(b) && b.Fields["plan_id"] == want {
					out = append(out, b)
				}
			}
			return &beadsapi.ListBeadsResult{Beads: out, Total: len(out)}, nil
		}
		// Bulk /plans-index join (planAssistantBulk, S4 ka-0000007888): a
		// role-scoped agent list with NO plan_id field. Return every seeded
		// agent bead carrying the role label so the handler joins them to their
		// epics in Go, exactly as production does. Sorted newest-updated-first to
		// mirror the daemon's Sort=-updated_at.
		var out []*beadsapi.BeadDetail
		for _, b := range f.beads {
			if b != nil && b.Type == "agent" && beadMatchesLabels(b) {
				out = append(out, b)
			}
		}
		sort.SliceStable(out, func(i, j int) bool {
			return out[i].UpdatedAt.After(out[j].UpdatedAt)
		})
		return &beadsapi.ListBeadsResult{Beads: out, Total: len(out)}, nil
	}
	// /index Live-agents rail squawk-preview batched fetch — Types=[message],
	// Labels=[squawk]. Routes to squawkResp so the squawk-by-agent join is
	// seedable independently of the agent list (ka-0000003997).
	if slices.Contains(q.Types, "message") && slices.Contains(q.Labels, "squawk") {
		if f.squawkResp != nil {
			return f.squawkResp, nil
		}
		return &beadsapi.ListBeadsResult{}, nil
	}
	// CountOnly owned-epic subtree queries (enrichGardenerOwnedEpics,
	// ka-0000001266): Parent=<epic>, Kinds=[issue], CountOnly=true. Total keyed on
	// the epic id; the closed subset adds Statuses=[closed] and a "|closed"
	// suffix. Routed ahead of the generic CountOnly branches since it carries no
	// Types.
	if q.CountOnly && q.Parent != "" && q.NoOpenDeps {
		if f.readyWorkErr != nil {
			return nil, f.readyWorkErr
		}
		return &beadsapi.ListBeadsResult{Total: f.readyWorkCount[q.Parent]}, nil
	}
	if q.CountOnly && q.Parent != "" && slices.Contains(q.Kinds, "issue") {
		key := q.Parent
		if slices.Contains(q.Statuses, "closed") {
			key += "|closed"
		}
		return &beadsapi.ListBeadsResult{Total: f.epicSubtreeCount[key]}, nil
	}
	// CountOnly iteration-count queries for legacy zero-tally gardeners
	// (enrichGardenerRunCounts, ka-0000005661): Types=[agent], CountOnly=true,
	// keyed on a single trigger_id/schedule_id field. Routed ahead of the
	// generic agent branches so the count shape isn't hijacked. The closed
	// subset adds Statuses=[closed]; key it with a "|closed" suffix.
	if slices.Contains(q.Types, "agent") && q.CountOnly && len(q.Fields) == 1 {
		var key string
		for k, v := range q.Fields {
			key = k + "=" + v
		}
		if slices.Contains(q.Statuses, "closed") {
			key += "|closed"
		}
		return &beadsapi.ListBeadsResult{Total: f.agentCountByField[key]}, nil
	}
	// CountOnly never-ran-shell query (enrichGardenerRunCounts, bug
	// ka-0000006805): Types=[agent], CountOnly=true, two fields — the owner key
	// AND agent_state=expired. Keyed with a "|expired" suffix on the owner-field
	// entry so the shell count is seedable independently of the total/closed
	// tallies.
	if slices.Contains(q.Types, "agent") && q.CountOnly && len(q.Fields) == 2 {
		if _, ok := q.Fields[fieldAgentState]; ok {
			var key string
			for k, v := range q.Fields {
				if k == fieldAgentState {
					continue
				}
				key = k + "=" + v
			}
			key += "|expired"
			return &beadsapi.ListBeadsResult{Total: f.agentCountByField[key]}, nil
		}
	}
	// Recent-run glyph fallback fetch (enrichGardenerRunGlyphs, bug ka-0000001069):
	// Types=[agent], NOT CountOnly, a single trigger_id/schedule_id owner field,
	// Sort=-created_at, Limit=gardenerRunGlyphFetchLimit (the detail page's
	// gardenerIterations shares the rest of the shape at a larger limit). Routed
	// ahead of the generic non-closed-agent branch so the
	// per-gardener iteration fetch isn't hijacked by liveAgentsResp.
	if slices.Contains(q.Types, "agent") && !q.CountOnly && q.Sort == "-created_at" && q.Limit == gardenerRunGlyphFetchLimit && len(q.Fields) == 1 {
		var gardenerID string
		for k, v := range q.Fields {
			if k == fieldTriggerID || k == fieldScheduleID {
				gardenerID = v
			}
		}
		if gardenerID != "" {
			return &beadsapi.ListBeadsResult{Beads: f.iterationGlyphBeads[gardenerID]}, nil
		}
	}
	// /agents/chat full-roster membership query (agent_chat.go, Task 1
	// gd-jqdZXW0ZdQ): Types=[agent], Statuses=[open, in_progress, closed] — the
	// first caller wanting every agent regardless of lifecycle in one call
	// (every other agent-list caller above asks either just-closed or
	// just-open/in_progress). Routed ahead of the closed-only / live-only
	// branches so the full roster is seedable off f.beads directly, honoring
	// an optional project-label filter via the beadMatchesLabels closure above.
	if slices.Contains(q.Types, "agent") && slices.Contains(q.Statuses, "open") && slices.Contains(q.Statuses, "closed") {
		var out []*beadsapi.BeadDetail
		for _, b := range f.beads {
			if b != nil && b.Type == "agent" && beadMatchesLabels(b) {
				out = append(out, b)
			}
		}
		return &beadsapi.ListBeadsResult{Beads: out, Total: len(out)}, nil
	}
	// Dormant-bucket query (/agents parked-identity bucket, ka-0000004675):
	// Types=[agent], closed, field-equality on agent_state=dormant, not
	// CountOnly. Routed AHEAD of the generic recent branch so the parked
	// population is seedable disjointly from recently-terminated agents.
	if slices.Contains(q.Types, "agent") && slices.Contains(q.Statuses, "closed") &&
		!q.CountOnly && q.Fields["agent_state"] == "dormant" {
		if f.agentDormantResp != nil {
			return f.agentDormantResp, nil
		}
		return &beadsapi.ListBeadsResult{}, nil
	}
	if slices.Contains(q.Types, "agent") && slices.Contains(q.Statuses, "closed") {
		if f.agentRecentResp != nil {
			return f.agentRecentResp, nil
		}
		return &beadsapi.ListBeadsResult{}, nil
	}
	// /index "Live agents" rail panel + banner "agents live" stat both query
	// active agent beads (open / in_progress). The rail panel sorts
	// descending (`-updated_at`); the banner doesn't sort. /agents.handleAgents
	// uses ascending `updated_at`, so we route by sort field to avoid
	// hijacking the /agents query. Both /index callers flow through
	// liveAgentsResp so tests can seed a single fixture.
	if slices.Contains(q.Types, "agent") && !slices.Contains(q.Statuses, "closed") &&
		q.Sort != "updated_at" {
		if f.liveAgentsResp != nil {
			return f.liveAgentsResp, nil
		}
		return &beadsapi.ListBeadsResult{}, nil
	}
	// /index right-rail "Hot ready" panel. The production query restricts
	// to issueMacroTypes (task/bug/epic/chore/feature/rich-report) so meta
	// types like session/trigger don't dominate the panel; the older shape
	// with empty Types still routes here so the banner-ready count query
	// and legacy fixtures keep working (ka-0000008099).
	if q.NoOpenDeps && slices.Contains(q.Statuses, "open") &&
		(len(q.Types) == 0 || slices.Equal(q.Types, issueMacroTypes)) {
		if f.hotReadyResp != nil {
			return f.hotReadyResp, nil
		}
		return &beadsapi.ListBeadsResult{}, nil
	}
	// /index "Hot epics" rail panel — exactly Types=[epic]. Must not match the
	// project page's "issues" macro (which includes "epic" alongside task/bug/etc).
	if len(q.Types) == 1 && q.Types[0] == "epic" {
		if f.hotEpicsResp != nil {
			return f.hotEpicsResp, nil
		}
		return &beadsapi.ListBeadsResult{}, nil
	}
	// /index banner P0-open scan — Kinds=[issue], Statuses=[open], Sort=priority.
	// The shape collides with /health's open-beads tile (same filters), so when
	// p0ScanResp is not set fall through to listResp to preserve the /health
	// fixture (ka-0000006022).
	if q.Sort == "priority" && slices.Contains(q.Kinds, "issue") && slices.Contains(q.Statuses, "open") {
		if f.p0ScanResp != nil {
			return f.p0ScanResp, nil
		}
		// Fall through to listResp.
	}
	// Plan-gardener lookup (plans_gardener.go planGardener, Step 2 ka-0000002775):
	// a single-type query (trigger OR schedule) scoped to a gardener-epic:<id>
	// label. Scan f.beads for beads of that type carrying the label so the plan
	// gardener bar can be exercised off the same seeded fixtures as every other
	// surface. Routed ahead of the generic trigger/schedule + role=gardener
	// branches since this query is also Types=[trigger]/[schedule].
	if len(q.Types) == 1 && (q.Types[0] == "trigger" || q.Types[0] == "schedule") &&
		len(q.Labels) == 1 && strings.HasPrefix(q.Labels[0], "gardener-epic:") {
		var out []*beadsapi.BeadDetail
		for _, b := range f.beads {
			if b != nil && b.Type == q.Types[0] && slices.Contains(b.Labels, q.Labels[0]) {
				out = append(out, b)
			}
		}
		return &beadsapi.ListBeadsResult{Beads: out, Total: len(out)}, nil
	}
	// Gardener-list resilience path: queries filtering on the role=gardener FIELD
	// (loadGardenerRows ka-0000001950). Must precede the generic trigger/schedule
	// routes since this query is also Types=[trigger]/[schedule].
	if q.Fields["role"] == "gardener" {
		if f.gardenerFieldOnlyResp != nil {
			return f.gardenerFieldOnlyResp, nil
		}
		return &beadsapi.ListBeadsResult{}, nil
	}
	// Pause-gate lookup (pausegate.Find, used by the /plans master switch step 4
	// ka-0000004048 and the /schedule command): Types=[gate], Statuses=[open,
	// in_progress], Search=<title>. Serve open gate beads straight off f.beads so
	// a test can seed a gardeners-pause / scheduler-pause gate and drive the
	// banner + switch state. The daemon's Search is fuzzy, so return every open
	// gate and let pausegate.Find's exact-title match narrow it (that is the
	// behavior under test). Routed ahead of the generic fallthrough so a seeded
	// gate isn't swallowed by listResp.
	if len(q.Types) == 1 && q.Types[0] == "gate" {
		var out []*beadsapi.BeadDetail
		for _, b := range f.beads {
			if b != nil && b.Type == "gate" && statusOpenSortKey(b.Status) == 0 {
				out = append(out, b)
			}
		}
		return &beadsapi.ListBeadsResult{Beads: out, Total: len(out)}, nil
	}
	// Plan-gardener idempotent-adopt finder (plangardener.FindAnyGardenerForEpic,
	// the shared core behind gb plan gardener + the viewer auto-bind,
	// ka-0000002863): exactly Types=[trigger, schedule] with NO label filter —
	// a shape no other production query uses (the involvement/stall gardener
	// scans add Labels=[role:gardener]). Serve live trigger/schedule beads
	// straight off f.beads so a bind → re-bind flow adopts what the first bind
	// stored (via createOnSuccess) instead of minting a duplicate.
	if len(q.Types) == 2 && slices.Contains(q.Types, "trigger") &&
		slices.Contains(q.Types, "schedule") && len(q.Labels) == 0 {
		var out []*beadsapi.BeadDetail
		for _, b := range f.beads {
			if b != nil && (b.Type == "trigger" || b.Type == "schedule") &&
				statusOpenSortKey(b.Status) == 0 {
				out = append(out, b)
			}
		}
		return &beadsapi.ListBeadsResult{Beads: out, Total: len(out)}, nil
	}
	if slices.Contains(q.Types, "trigger") {
		if f.triggerScheduleResp != nil {
			return f.triggerScheduleResp, nil
		}
		return &beadsapi.ListBeadsResult{}, nil
	}
	if f.queuedResp != nil && len(q.Types) == 0 && slices.Contains(q.Statuses, "open") {
		return f.queuedResp, nil
	}
	if f.accountResp != nil && slices.Contains(q.Types, "account") {
		return f.accountResp, nil
	}
	// /index per-project stale count: in_progress + a non-zero UpdatedBefore
	// cutoff (ka-0000002729). Routed ahead of the plain in_progress branches.
	if f.staleResp != nil && !q.UpdatedBefore.IsZero() && slices.Contains(q.Statuses, "in_progress") {
		return f.staleResp, nil
	}
	if f.inProgressResp != nil && len(q.Types) == 0 && slices.Contains(q.Statuses, "in_progress") {
		return f.inProgressResp, nil
	}
	// plans-involvement step-owner facet scan (plans_involvement.go
	// plansWithStepOwnedBy): Kinds=[issue] + an operator-alias filter, no Types.
	// Routed here (ahead of the generic listResp fallthrough) so the issue-owned-
	// by-X scan is seedable independently. Defaults empty so unrelated
	// Kinds=[issue] fixtures are unaffected.
	if len(q.Types) == 0 && slices.Contains(q.Kinds, "issue") && len(q.OperatedByAliases) > 0 {
		if f.involveStepScanResp != nil {
			return f.involveStepScanResp, nil
		}
		return &beadsapi.ListBeadsResult{}, nil
	}
	// /me/taints owns facet (myTaintedBeads): a bare Owner filter, no Types.
	if q.Owner != "" && len(q.Types) == 0 {
		if f.meTaintsOwnErr != nil {
			return nil, f.meTaintsOwnErr
		}
		if f.meTaintsOwnResp != nil {
			return f.meTaintsOwnResp, nil
		}
		return &beadsapi.ListBeadsResult{}, nil
	}
	// /me/taints created facet (myTaintedBeads): a bare OperatedByAliases
	// filter with no Types/Kinds/Statuses — distinct from the shapes above
	// (plans_involvement.go's baseQuery-scoped Types=[epic] variant, up.go's
	// Statuses=[in_progress] variant).
	if len(q.OperatedByAliases) > 0 && len(q.Types) == 0 && len(q.Kinds) == 0 && len(q.Statuses) == 0 {
		if f.meTaintsCreatedErr != nil {
			return nil, f.meTaintsCreatedErr
		}
		if f.meTaintsCreatedResp != nil {
			return f.meTaintsCreatedResp, nil
		}
		return &beadsapi.ListBeadsResult{}, nil
	}
	return f.listResp, nil
}

func (f *fakeDaemon) GetAcceptanceMetrics(_ context.Context) (*beadsapi.AcceptanceMetrics, error) {
	f.acceptanceCalls++
	if f.acceptanceErr != nil {
		return nil, f.acceptanceErr
	}
	return f.acceptanceResp, nil
}

func (f *fakeDaemon) ListEscalations(_ context.Context, status string, limit int) ([]beadsapi.EscalationDetail, error) {
	f.escalationsLastSt = status
	f.escalationsLastLim = limit
	if f.escalationsErr != nil {
		return nil, f.escalationsErr
	}
	return f.escalationsResp, nil
}

func (f *fakeDaemon) ApproveEscalation(_ context.Context, id string, req beadsapi.ApproveEscalationRequest) error {
	f.approveEscIDs = append(f.approveEscIDs, id)
	f.approveEscCalls = append(f.approveEscCalls, req)
	return f.approveEscErr
}

func (f *fakeDaemon) GetEscalationPermissionDiff(_ context.Context, escalationID string) (*beadsapi.SpawnPermissionDiff, bool, error) {
	f.permissionDiffCalls++
	f.permissionDiffLastID = escalationID
	if f.permissionDiffErr != nil {
		return nil, false, f.permissionDiffErr
	}
	return f.permissionDiffResp, f.permissionDiffOK, nil
}

func (f *fakeDaemon) RejectEscalation(_ context.Context, id string, req beadsapi.RejectEscalationRequest) error {
	f.rejectEscIDs = append(f.rejectEscIDs, id)
	f.rejectEscCalls = append(f.rejectEscCalls, req)
	return f.rejectEscErr
}

func (f *fakeDaemon) CanApproveEscalation(_ context.Context, id string) (beadsapi.CanApproveResult, error) {
	f.canApproveIDs = append(f.canApproveIDs, id)
	if f.canApproveErr != nil {
		return beadsapi.CanApproveResult{}, f.canApproveErr
	}
	return f.canApproveResp[id], nil
}

func (f *fakeDaemon) RouteEscalation(_ context.Context, id string, req beadsapi.RouteEscalationRequest) (string, error) {
	f.routeEscIDs = append(f.routeEscIDs, id)
	f.routeEscCalls = append(f.routeEscCalls, req)
	if f.routeEscErr != nil {
		return "", f.routeEscErr
	}
	if b := f.beads[id]; b != nil && req.Audience != "" {
		b.Fields["audience"] = req.Audience
	}
	return req.Audience, nil
}

func (f *fakeDaemon) ListFollowedBeadIDs(_ context.Context, operatorID int64, limit int) ([]string, error) {
	f.followedBeadIDsCalls++
	f.followedBeadIDsLastOp = operatorID
	f.followedBeadIDsLastLim = limit
	if f.followedBeadIDsErr != nil {
		return nil, f.followedBeadIDsErr
	}
	ids := f.followedBeadIDsByOp[operatorID]
	if limit > 0 && len(ids) > limit {
		ids = ids[:limit]
	}
	return ids, nil
}

func (f *fakeDaemon) ListProjectBeads(_ context.Context) (map[string]beadsapi.ProjectInfo, error) {
	f.listMu.Lock()
	f.projectsCalls++
	f.listMu.Unlock()
	if f.projectsErr != nil {
		return nil, f.projectsErr
	}
	return f.projectsResp, nil
}

func (f *fakeDaemon) ListRoleBeads(_ context.Context) (map[string]beadsapi.RoleInfo, error) {
	if f.rolesErr != nil {
		return nil, f.rolesErr
	}
	return f.rolesResp, nil
}

func (f *fakeDaemon) ListAccountBeadsByProject(_ context.Context, project string) ([]beadsapi.AccountInfo, error) {
	f.accountsByProjectLastArg = project
	if f.accountsByProjectErr != nil {
		return nil, f.accountsByProjectErr
	}
	return f.accountsByProjectResp, nil
}

func (f *fakeDaemon) ResolveDecision(_ context.Context, id string, req beadsapi.ResolveDecisionRequest) error {
	f.resolveCalls++
	f.resolveLastID = id
	f.resolveLastReq = req
	if f.resolveErr != nil {
		return f.resolveErr
	}
	if f.resolveOnSuccess != nil {
		f.resolveOnSuccess(f, id, req)
	}
	return nil
}

func (f *fakeDaemon) CancelDecision(_ context.Context, id, reason, canceledBy string) error {
	f.cancelCalls++
	f.cancelLastID = id
	f.cancelLastReason = reason
	f.cancelLastBy = canceledBy
	if f.cancelErr != nil {
		return f.cancelErr
	}
	if f.cancelOnSuccess != nil {
		f.cancelOnSuccess(f, id, reason, canceledBy)
	}
	return nil
}

func (f *fakeDaemon) UpdateBead(ctx context.Context, id string, req beadsapi.UpdateBeadRequest) error {
	f.updateCalls++
	f.updateLastMachine = beadsapi.MachinePrincipalFromContext(ctx) != nil
	f.updateLastID = id
	f.updateLastReq = req
	if f.updateErr != nil {
		return f.updateErr
	}
	if b, ok := f.beads[id]; ok {
		if req.Status != nil {
			b.Status = *req.Status
		}
		if req.Assignee != nil {
			b.Assignee = *req.Assignee
		}
		if req.Priority != nil {
			b.Priority = *req.Priority
		}
		if req.Description != nil {
			b.Description = *req.Description
		}
		if req.Title != nil {
			b.Title = *req.Title
		}
		if req.Labels != nil {
			b.Labels = *req.Labels
		}
		if req.Owner != nil {
			b.Owner = *req.Owner
		}
		// Reflect an atomic fields patch (ka-0000001583: the wake sends status +
		// fields in ONE UpdateBead) onto the seeded bead so a read-after-write reads
		// the new values. nil values DELETE the key (RFC-7386), mirroring the daemon.
		if len(req.Fields) > 0 {
			if b.Fields == nil {
				b.Fields = map[string]string{}
			}
			for k, v := range req.Fields {
				if v == nil {
					delete(b.Fields, k)
					continue
				}
				if s, ok := v.(string); ok {
					b.Fields[k] = s
				} else {
					b.Fields[k] = fmt.Sprintf("%v", v)
				}
			}
		}
	}
	return nil
}

func (f *fakeDaemon) UpdateBeadFieldsTyped(ctx context.Context, id string, fields map[string]any) error {
	f.updateFieldsCalls++
	f.updateFieldsLastID = id
	f.updateFieldsLastMachine = beadsapi.MachinePrincipalFromContext(ctx) != nil
	cp := make(map[string]any, len(fields))
	maps.Copy(cp, fields)
	f.updateFieldsLastVal = cp
	f.updateFieldsHistory = append(f.updateFieldsHistory, updateFieldsCall{ID: id, Fields: cp})
	if f.updateFieldsErr != nil {
		return f.updateFieldsErr
	}
	// Reflect the write onto the seeded bead (as a string field) so a subsequent
	// GetBead in the same flow — e.g. a re-render after a state write — reads the
	// new value, matching the daemon's read-after-write behaviour.
	if b, ok := f.beads[id]; ok {
		if b.Fields == nil {
			b.Fields = map[string]string{}
		}
		for k, v := range cp {
			switch val := v.(type) {
			case string:
				b.Fields[k] = val
			case bool:
				b.Fields[k] = strconv.FormatBool(val)
			}
		}
	}
	return nil
}

// updateFieldsCall is one recorded UpdateBeadFieldsTyped write.
type updateFieldsCall struct {
	ID     string
	Fields map[string]any
}

// arrayFieldAppend / arrayFieldReorder record the args of the atomic
// plan-ordering primitive calls (ka-0000000988) so a test can assert on the exact
// GROW element / CAS (expected, next) the handler sent.
type arrayFieldAppend struct {
	ID, Field, Element string
}
type arrayFieldReorder struct {
	ID, Field      string
	Expected, Next []string
}

// decodeArrayField reads a bead's string-array JSONB field the way the daemon
// stores it (a JSON array text under Fields[field]); absent/blank/unparseable ⇒
// nil, matching planPhaseOrder/planStepOrder's decode so the fake's CAS compares
// like-for-like.
func decodeArrayField(b *beadsapi.BeadDetail, field string) []string {
	if b == nil || b.Fields == nil {
		return nil
	}
	raw, ok := b.Fields[field]
	if !ok || raw == "" {
		return nil
	}
	var ids []string
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		return nil
	}
	return ids
}

// AppendBeadArrayField models the daemon's append-if-absent GROW primitive: it
// appends element to the seeded bead's JSON-array field only when absent, so a
// double-submit is idempotent and a subsequent GetBead/re-render reads the grown
// array.
func (f *fakeDaemon) AppendBeadArrayField(_ context.Context, id, field, element string) error {
	f.appendArrayCalls++
	f.appendArrayLast = arrayFieldAppend{ID: id, Field: field, Element: element}
	if f.appendArrayErr != nil {
		return f.appendArrayErr
	}
	b, ok := f.beads[id]
	if !ok {
		return &beadsapi.APIError{StatusCode: http.StatusNotFound, Message: "bead not found"}
	}
	cur := decodeArrayField(b, field)
	if slices.Contains(cur, element) {
		return nil // append-if-absent: already present is a no-op
	}
	if b.Fields == nil {
		b.Fields = map[string]string{}
	}
	raw, _ := json.Marshal(append(append([]string(nil), cur...), element))
	b.Fields[field] = string(raw)
	return nil
}

// ReorderBeadArrayField models the daemon's compare-and-swap REORDER primitive:
// it writes next only if the seeded field still equals expected, returning a 409
// CAS conflict otherwise. reorderArrayConflicts forces leading transient
// conflicts (to drive the retry loop); reorderArrayErr forces a non-CAS error.
func (f *fakeDaemon) ReorderBeadArrayField(_ context.Context, id, field string, expected, next []string) error {
	f.reorderArrayCalls++
	f.reorderArrayLast = arrayFieldReorder{ID: id, Field: field, Expected: expected, Next: next}
	if f.reorderArrayConflicts > 0 {
		f.reorderArrayConflicts--
		return &beadsapi.APIError{StatusCode: http.StatusConflict, Message: "compare-and-swap conflict"}
	}
	if f.reorderArrayErr != nil {
		return f.reorderArrayErr
	}
	b, ok := f.beads[id]
	if !ok {
		return &beadsapi.APIError{StatusCode: http.StatusNotFound, Message: "bead not found"}
	}
	// Real CAS: the stored value must still equal the caller's expected snapshot.
	if !slices.Equal(decodeArrayField(b, field), expected) {
		return &beadsapi.APIError{StatusCode: http.StatusConflict, Message: "compare-and-swap conflict"}
	}
	if b.Fields == nil {
		b.Fields = map[string]string{}
	}
	raw, _ := json.Marshal(next)
	b.Fields[field] = string(raw)
	return nil
}

func (f *fakeDaemon) CloseBeadAs(_ context.Context, id, actor string, _ map[string]string) error {
	f.closeCalls++
	f.closeLastID = id
	f.closeLastBy = actor
	if f.closeErr != nil {
		return f.closeErr
	}
	if b, ok := f.beads[id]; ok {
		b.Status = "closed"
		b.ClosedBy = actor
	}
	return nil
}

func (f *fakeDaemon) FindSimilar(_ context.Context, id string, limit int) (*beadsapi.FindSimilarResponse, error) {
	f.similarLastID = id
	f.similarLastLimit = limit
	if f.similarErr != nil {
		return nil, f.similarErr
	}
	return f.similarResp, nil
}

func (f *fakeDaemon) GetStats(_ context.Context) (*beadsapi.GraphStats, error) {
	if f.statsErr != nil {
		return nil, f.statsErr
	}
	return f.statsResp, nil
}

func (f *fakeDaemon) ListConfigs(_ context.Context, namespace string) ([]beadsapi.ConfigEntry, error) {
	if f.configsErr != nil {
		return nil, f.configsErr
	}
	return f.configsByNS[namespace], nil
}

func (f *fakeDaemon) PourFormula(_ context.Context, formulaID string, req beadsapi.PourFormulaRequest) (*beadsapi.PourFormulaResponse, error) {
	f.pourCalls++
	f.pourLastID = formulaID
	f.pourLastReq = req
	if f.pourErr != nil {
		return nil, f.pourErr
	}
	if f.pourResp != nil {
		return f.pourResp, nil
	}
	return &beadsapi.PourFormulaResponse{}, nil
}

func (f *fakeDaemon) CreateBead(ctx context.Context, req beadsapi.CreateBeadRequest) (string, error) {
	bead, err := f.CreateBeadDetail(ctx, req)
	if err != nil {
		return "", err
	}
	return bead.ID, nil
}

// CreateBeadDetail mirrors the real client: the daemon's create response carries
// the full PERSISTED bead (labels read back inside the create transaction,
// ka-0000002898), so the fake returns the stored bead when a test wired one via
// createOnSuccess (e.g. storeCreatedBeads / a deliberate mislabel), and
// otherwise synthesizes the committed shape from the request the same way the
// daemon would echo it back. CreateBead delegates here and keeps only the id.
func (f *fakeDaemon) CreateBeadDetail(ctx context.Context, req beadsapi.CreateBeadRequest) (*beadsapi.BeadDetail, error) {
	f.mintMu.Lock()
	defer f.mintMu.Unlock()
	f.createCalls++
	f.createLastReq = req
	f.createReqs = append(f.createReqs, req)
	f.createLastOpBy = beadsapi.OperatorFromContext(ctx)
	f.createLastSysOp = beadsapi.SystemOperatorFromContext(ctx)
	f.createLastMachine = beadsapi.MachinePrincipalFromContext(ctx) != nil
	if f.createErr != nil {
		return nil, f.createErr
	}
	if f.createErrForReq != nil {
		if err := f.createErrForReq(req); err != nil {
			return nil, err
		}
	}
	// Daemon resolve-or-create dedup guard: a repeat IdempotencyKey returns the
	// existing bead and mints nothing new (no createOnSuccess) — mirroring kbeads
	// migration 029 so the composer double-submit test sees exactly one head.
	if req.IdempotencyKey != "" {
		if existing, ok := f.createByIdemKey[req.IdempotencyKey]; ok {
			return f.createdDetail(existing, req), nil
		}
	}
	id := f.createResp
	if f.createIDForReq != nil {
		if v := f.createIDForReq(req); v != "" {
			id = v
		}
	}
	if id == "" {
		id = "gd-new"
	}
	if req.IdempotencyKey != "" {
		if f.createByIdemKey == nil {
			f.createByIdemKey = map[string]string{}
		}
		f.createByIdemKey[req.IdempotencyKey] = id
	}
	if f.createOnSuccess != nil {
		f.createOnSuccess(f, id, req)
	}
	return f.createdDetail(id, req), nil
}

// createdDetail returns the bead a create response would carry for id: the
// stored bead when a test persisted one (createOnSuccess), else the committed
// shape synthesized from the request — exactly what the daemon echoes back after
// reading labels back within the create transaction. Caller holds mintMu.
func (f *fakeDaemon) createdDetail(id string, req beadsapi.CreateBeadRequest) *beadsapi.BeadDetail {
	if b, ok := f.beads[id]; ok {
		return b
	}
	return &beadsapi.BeadDetail{
		ID:          id,
		Title:       req.Title,
		Type:        req.Type,
		Kind:        req.Kind,
		Status:      "open",
		Labels:      append([]string(nil), req.Labels...),
		Description: req.Description,
		CreatedBy:   req.CreatedBy,
		Fields:      beadsapi.ParseFieldsJSON(req.Fields),
	}
}

// createCallsOfType counts recorded CreateBead requests of a given bead type.
// Handlers that mint side beads (e.g. the plan-transcript operator/assistant
// turn beads, type=message) alongside their primary agent-bead work make the
// bare createCalls counter ambiguous; a type-scoped count lets a test assert on
// exactly the create it cares about (e.g. "no NEW agent bead on wake-in-place").
func (f *fakeDaemon) createCallsOfType(beadType string) int {
	n := 0
	for _, req := range f.createReqs {
		if req.Type == beadType {
			n++
		}
	}
	return n
}

// createReqsSnapshot returns a copy of the recorded CreateBead requests taken
// under mintMu, so a test that observes creates from a BACKGROUND goroutine
// (e.g. a reactor pump appending via CreateBeadDetail) can poll them race-free
// under `go test -race`. Sequential tests keep reading f.createReqs directly.
func (f *fakeDaemon) createReqsSnapshot() []beadsapi.CreateBeadRequest {
	f.mintMu.Lock()
	defer f.mintMu.Unlock()
	return append([]beadsapi.CreateBeadRequest(nil), f.createReqs...)
}

func (f *fakeDaemon) AddDependency(_ context.Context, beadID, dependsOnID, depType, createdBy string) error {
	f.addDepCalls++
	f.addDepLastBead = beadID
	f.addDepLastDep = dependsOnID
	f.addDepLastType = depType
	f.addDepLastBy = createdBy
	if f.addDepErr != nil {
		return f.addDepErr
	}
	f.deps[beadID] = append(f.deps[beadID], beadsapi.Dependency{
		BeadID:      beadID,
		DependsOnID: dependsOnID,
		Type:        depType,
	})
	if f.revDeps == nil {
		f.revDeps = map[string][]beadsapi.Dependency{}
	}
	f.revDeps[dependsOnID] = append(f.revDeps[dependsOnID], beadsapi.Dependency{
		BeadID:      beadID,
		DependsOnID: dependsOnID,
		Type:        depType,
	})
	return nil
}

func (f *fakeDaemon) RemoveDependency(_ context.Context, beadID, dependsOnID, depType string) error {
	f.removeDepCalls++
	f.removeDepLast = [3]string{beadID, dependsOnID, depType}
	return f.removeDepErr
}

func (f *fakeDaemon) AddLabel(_ context.Context, beadID, label string) error {
	f.addLabelCalls++
	f.addLabelLastBead = beadID
	f.addLabelLastVal = label
	f.addLabelAll = append(f.addLabelAll, label)
	if f.addLabelErr != nil {
		return f.addLabelErr
	}
	if b, ok := f.beads[beadID]; ok {
		b.Labels = append(b.Labels, label)
	}
	return nil
}

func (f *fakeDaemon) RemoveLabel(_ context.Context, beadID, label string) error {
	f.removeLabelCalls++
	f.removeLabelLastBead = beadID
	f.removeLabelLastVal = label
	f.removeLabelAll = append(f.removeLabelAll, label)
	if f.removeLabelErr != nil {
		return f.removeLabelErr
	}
	if b, ok := f.beads[beadID]; ok {
		out := b.Labels[:0]
		for _, l := range b.Labels {
			if l != label {
				out = append(out, l)
			}
		}
		b.Labels = out
	}
	return nil
}

func (f *fakeDaemon) EventStream(_ context.Context, topics string) (<-chan beadsapi.SSEEvent, error) {
	f.eventStreamCalls++
	f.eventStreamLastTopics = topics
	if f.eventStreamErr != nil {
		return nil, f.eventStreamErr
	}
	if f.eventStreamCh != nil {
		return f.eventStreamCh, nil
	}
	ch := make(chan beadsapi.SSEEvent)
	close(ch)
	return ch, nil
}

func (f *fakeDaemon) ListGlobalEvents(_ context.Context, req beadsapi.ListGlobalEventsRequest) (*beadsapi.ListGlobalEventsResponse, error) {
	f.listGlobalEventsCalls++
	f.listGlobalEventsLastReq = req
	if f.listGlobalEventsErr != nil {
		return nil, f.listGlobalEventsErr
	}
	if f.listGlobalEventsFn != nil {
		return f.listGlobalEventsFn(req)
	}
	if f.listGlobalEventsResp != nil {
		// Faithfully mirror the real daemon: a desc request returns events
		// newest-first. Test fixtures author events oldest-first (the natural
		// chronological order), so reverse a copy when req.Desc is set rather
		// than forcing every fixture to be pre-reversed.
		if req.Desc {
			src := f.listGlobalEventsResp.Events
			rev := make([]beadsapi.Event, len(src))
			for i, e := range src {
				rev[len(src)-1-i] = e
			}
			return &beadsapi.ListGlobalEventsResponse{
				Events:     rev,
				HasMore:    f.listGlobalEventsResp.HasMore,
				NextCursor: f.listGlobalEventsResp.NextCursor,
			}, nil
		}
		return f.listGlobalEventsResp, nil
	}
	return &beadsapi.ListGlobalEventsResponse{}, nil
}

func (f *fakeDaemon) RecordUITelemetry(_ context.Context, row *beadsapi.UITelemetryRow) error {
	f.uiTelemetryCalls++
	if row != nil {
		f.uiTelemetryLast = *row
	}
	return f.uiTelemetryErr
}

// MRBeadByURL scans the fake's in-memory bead map for a type=merge-request
// bead whose fields.mr_url matches mrURL. Returns (nil, nil) on miss to mirror
// the live client's "not found" contract. mrByURLErr forces a hard error so
// tests can exercise the resolver's miss path independently from the absent
// path. mrByURLCalls records invocations so tests can assert call count.
func (f *fakeDaemon) MRBeadByURL(_ context.Context, mrURL string) (*beadsapi.BeadDetail, error) {
	f.mrByURLCalls++
	f.mrByURLLast = mrURL
	if f.mrByURLErr != nil {
		return nil, f.mrByURLErr
	}
	if mrURL == "" {
		return nil, nil
	}
	for _, b := range f.beads {
		if b == nil || b.Type != "merge-request" {
			continue
		}
		if b.Fields["mr_url"] == mrURL {
			return b, nil
		}
	}
	return nil, nil
}

// MRBeadsForAgent mirrors the live client's 2-hop walk:
// agent --produces--> task --produces--> merge-request. Per-bead lookup
// failures are silently skipped and duplicates (same MR via multiple tasks)
// are deduped, matching the production behavior. mrsForAgentErr forces a hard
// error on the first hop so tests can exercise the degraded path.
func (f *fakeDaemon) MRBeadsForAgent(_ context.Context, agentBeadID string) ([]*beadsapi.BeadDetail, error) {
	f.mrsForAgentCalls++
	f.mrsForAgentLast = agentBeadID
	if f.mrsForAgentErr != nil {
		return nil, f.mrsForAgentErr
	}
	if agentBeadID == "" {
		return nil, nil
	}
	var out []*beadsapi.BeadDetail
	seen := map[string]bool{}
	for _, td := range f.deps[agentBeadID] {
		if td.Type != "produces" || td.DependsOnID == "" {
			continue
		}
		task, ok := f.beads[td.DependsOnID]
		if !ok || task == nil {
			continue
		}
		for _, md := range f.deps[task.ID] {
			if md.Type != "produces" || md.DependsOnID == "" {
				continue
			}
			if seen[md.DependsOnID] {
				continue
			}
			mr, ok := f.beads[md.DependsOnID]
			if !ok || mr == nil || mr.Type != "merge-request" {
				continue
			}
			seen[md.DependsOnID] = true
			out = append(out, mr)
		}
	}
	return out, nil
}

func (f *fakeDaemon) DeleteBead(_ context.Context, id string) error {
	f.deleteCalls++
	f.deleteLastID = id
	if f.deleteErr != nil {
		return f.deleteErr
	}
	delete(f.beads, id)
	return nil
}

func newFake() *fakeDaemon {
	return &fakeDaemon{
		beads: map[string]*beadsapi.BeadDetail{
			"gd-aaa": {
				ID:          "gd-aaa",
				Slug:        "example-slug",
				Title:       "Example bead",
				Kind:        "issue",
				Type:        "task",
				Status:      "open",
				Priority:    2,
				Assignee:    "bot-1",
				Owner:       "owner-1",
				Labels:      []string{"project:gardenio", "scope:test"},
				Description: "First paragraph.\n\nSecond paragraph with `code`.",
				Notes:       "scratchpad notes here",
				Fields: map[string]string{
					"design":              "use a graph",
					"acceptance_criteria": "all green",
				},
				CreatedBy: "static",
				ClosedBy:  "closer-1",
				Fleet:     "default",
				CreatedAt: time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC),
				UpdatedAt: time.Date(2026, 4, 2, 11, 0, 0, 0, time.UTC),
				DueAt:     "2026-05-01",
			},
		},
		deps: map[string][]beadsapi.Dependency{
			"gd-aaa": {{BeadID: "gd-aaa", DependsOnID: "gd-parent", Type: "parent-child"}},
		},
		comments: map[string][]beadsapi.Comment{
			"gd-aaa": {{
				ID:        101,
				Author:    "reviewer",
				Text:      "Looks good to me.",
				CreatedAt: time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC),
			}},
		},
	}
}

func mkServer(t *testing.T, daemon beadAPI) http.Handler {
	t.Helper()
	srv := NewServer(daemon, slog.Default(), "", "", "", nil)
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	return edgeAuth(mux)
}

// mkServerWithNow is mkServer with the wall-clock source pinned. Use when a
// handler asserts ordering against time.Now-derived data (e.g. /triggers
// next-fire sort) so the test result is independent of the host's current
// time-of-day.
func mkServerWithNow(t *testing.T, daemon beadAPI, now func() time.Time) http.Handler {
	t.Helper()
	srv := NewServer(daemon, slog.Default(), "", "", "", nil)
	srv.SetNow(now)
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	return edgeAuth(mux)
}

func TestBeadShow_RendersBead(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/gd-aaa", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"Example bead",
		"gd-aaa",
		"example-slug",
		"issue",
		"project:gardenio",
		"scope:test",
		"Priority",
		"bot-1",
		"owner-1",
		"closer-1",
		"default",
		"gardenio", // Project field rendered from project: label
		"scratchpad notes here",
		"design",
		"use a graph",
		"acceptance_criteria",
		"all green",
		"2026-04-01",
		"2026-04-02",
		"2026-05-01",
		">First paragraph.</p>", // <p> now carries a data-block-id (crit-critique T1)
		"Second paragraph with <code>code</code>",
		`href="/gd-parent"`,
		"parent-child",
		"reviewer",
		"Looks good to me.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered bead missing %q\nbody: %s", want, body)
		}
	}
}

// TestBeadShow_InlineHeader verifies that the compact chip strip rendered
// directly under the title surfaces the same signal-dense fields the metadata
// sidebar carries — status, type, priority, assignee, owner, age — so the top
// of /bead-show is informative without scrolling. Locks in the info-density
// fix for gd-p1FHUUBqfB.
func TestBeadShow_InlineHeader(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/gd-aaa", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// The strip itself must be present, and each chip must carry the bead's
	// canonical value so a reader can pick up status / type / priority /
	// assignee / owner / age without consulting the field-list.
	for _, want := range []string{
		`class="bead-show-inline-header"`,
		`bsh-chip-status bsh-chip-status-open`,
		`bsh-chip-type`,
		`>task<`,
		`bsh-chip-priority bsh-chip-priority-2`,
		`>P2<`,
		`bsh-chip-assignee`,
		`>@bot-1<`,
		`bsh-chip-owner`,
		`>owner: owner-1<`,
		`bsh-chip-age`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("inline header missing %q", want)
		}
	}
}

// TestBeadShow_EmptyFields verifies that empty fields render with an em-dash
// rather than disappearing — the structure should be predictable across beads.
func TestBeadShow_EmptyFields(t *testing.T) {
	f := &fakeDaemon{
		beads: map[string]*beadsapi.BeadDetail{
			"gd-bare": {
				ID:     "gd-bare",
				Title:  "Bare bead",
				Type:   "task",
				Status: "open",
			},
		},
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-bare", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// Each empty optional field should still produce a row with em-dash.
	for _, label := range []string{"Slug", "Owner", "Closed By", "Project", "Fleet", "Closed At", "Due At", "Defer Until"} {
		if !strings.Contains(body, label) {
			t.Errorf("missing field label %q in empty-bead render\nbody: %s", label, body)
		}
	}
	if !strings.Contains(body, "&mdash;") {
		t.Errorf("expected em-dash placeholders for empty fields\nbody: %s", body)
	}
}

func TestBeadShow_NotFound(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/gd-missing", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestBeadShow_EscapesUserContent(t *testing.T) {
	f := newFake()
	f.beads["gd-xss"] = &beadsapi.BeadDetail{
		ID:     "gd-xss",
		Title:  "<script>alert(1)</script>",
		Type:   "task",
		Status: "open",
		Labels: []string{`"><img src=x>`},
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-xss", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Error("title rendered without escaping")
	}
	if !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Error("expected title to be HTML-escaped")
	}
	if strings.Contains(body, `"><img src=x>`) {
		t.Error("label rendered without escaping")
	}
}

func TestBeadShow_LinkifiesBeadIDs(t *testing.T) {
	f := newFake()
	f.beads["gd-link"] = &beadsapi.BeadDetail{
		ID:     "gd-link",
		Title:  "Rebase after gd-aaa landed",
		Type:   "task",
		Status: "open",
		Description: "Blocked by gd-bbb. Also see [gd-existing](https://other/gd-existing) " +
			"and multi refs gd-ccc and gd-ddd in one paragraph.\n\n" +
			"```\ncode block with gd-eee\n```",
	}
	f.comments["gd-link"] = []beadsapi.Comment{{
		Author:    "reviewer",
		Text:      "LGTM, thanks. Rollup in gd-fff.",
		CreatedAt: time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC),
	}}

	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-link", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	// Description IDs go through LinkifyDescriptionHTML and emit rich anchors
	// (id="mention-…", class="mention", data-bead-id=…) — assert via href
	// substrings so the test tolerates either anchor form. Title and comment
	// IDs still use the plain LinkifyText form.
	for _, want := range []string{
		`<a href="/gd-aaa">gd-aaa</a>`,     // ID in title (plain anchor)
		`href="/gd-bbb"`,                   // inline ID in description (rich anchor)
		`>gd-bbb</a>`,                      //   visible text
		`href="/gd-ccc"`,                   // multiple IDs in one paragraph
		`>gd-ccc</a>`,                      //
		`href="/gd-ddd"`,                   // (second of the pair)
		`>gd-ddd</a>`,                      //
		`href="/gd-eee"`,                   // inside a code block (Matt wants nav everywhere)
		`>gd-eee</a>`,                      //
		`<a href="/gd-fff">gd-fff</a>`,     // inside a comment body (plain anchor)
		`href="https://other/gd-existing"`, // pre-existing markdown link href preserved
		`>gd-existing</a>`,                 // pre-existing markdown link text preserved
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected body to contain %q\nbody: %s", want, body)
		}
	}

	// Must not double-wrap the pre-existing markdown link to gd-existing.
	if strings.Contains(body, `<a href="/gd-existing">gd-existing</a>`) {
		t.Errorf("gd-existing was double-wrapped with a local link\nbody: %s", body)
	}
	// "gd-existing" appears exactly twice: once in the external href, once as visible text.
	if got := strings.Count(body, "gd-existing"); got != 2 {
		t.Errorf("expected gd-existing to appear exactly 2x (href + text), got %d\nbody: %s", got, body)
	}
}

// TestBeadShow_MRRef_MultiRepoProject_SkipsLinkification verifies the
// ka-0000004231 fix: when the bead's project has more than one repo
// (e.g. project:monorepo with repos:[monorepo, gardenio, site-deployment]),
// the description's bare `!N` references render as plain text rather than
// pointing at an arbitrary repo.
//
// Single-repo projects DO linkify — and per ka-0000005612 they resolve to that
// project's OWN repo, not to the process-wide gardenio default. A project that
// declares no repo at all is the only case still left on the default.
// Existing fully-qualified URLs are preserved in every branch.
func TestBeadShow_MRRef_MultiRepoProject_SkipsLinkification(t *testing.T) {
	makeBead := func(id, project string) *beadsapi.BeadDetail {
		return &beadsapi.BeadDetail{
			ID:     id,
			Title:  "Observability work",
			Type:   "epic",
			Status: "open",
			Labels: []string{"project:" + project},
			// One bare !N (linkifier candidate) and one fully-qualified MR
			// URL (markdown link — must round-trip untouched in both branches).
			Description: "See !897 and [!901](https://gitlab.com/Acme/Platform/site-deployment/-/merge_requests/901).",
		}
	}

	cases := []struct {
		name        string
		project     string
		projectInfo beadsapi.ProjectInfo
		// wantBareHref is the href bare !897 must resolve to, or "" when it
		// must not be linkified at all.
		wantBareHref string
	}{
		{
			name:    "multi-repo project skips linkification",
			project: "monorepo",
			projectInfo: beadsapi.ProjectInfo{
				Name: "monorepo",
				Repos: []beadsapi.RepoEntry{
					{URL: "https://github.com/org/monorepo.git", Role: "primary"},
					{URL: "https://github.com/org/gardenio.git", Role: "reference"},
					{URL: "https://github.com/org/site-deployment.git", Role: "reference"},
				},
			},
			wantBareHref: "",
		},
		{
			// The ka-0000005612 regression: this used to link at gardenio.
			name:    "single-repo project resolves to its OWN repo",
			project: "kbeads",
			projectInfo: beadsapi.ProjectInfo{
				Name: "kbeads",
				Repos: []beadsapi.RepoEntry{
					{URL: "https://github.com/org/kbeads.git", Role: "primary"},
				},
			},
			wantBareHref: "https://github.com/org/kbeads/-/merge_requests/897",
		},
		{
			// devsecops' real shape: one repo, role=reference, no git_url.
			// The role must not matter — one repo is one repo.
			name:    "single reference repo with no git_url resolves to that repo",
			project: "devsecops",
			projectInfo: beadsapi.ProjectInfo{
				Name:  "devsecops",
				Repos: []beadsapi.RepoEntry{{URL: "https://github.com/org/monorepo.git", Role: "reference"}},
			},
			wantBareHref: "https://github.com/org/monorepo/-/merge_requests/897",
		},
		{
			name:         "project with only git_url resolves to it",
			project:      "legacy",
			projectInfo:  beadsapi.ProjectInfo{Name: "legacy", GitURL: "https://github.com/org/legacy.git"},
			wantBareHref: "https://github.com/org/legacy/-/merge_requests/897",
		},
		{
			name:         "project declaring no repo at all keeps the default",
			project:      "gardenio",
			projectInfo:  beadsapi.ProjectInfo{Name: "gardenio"},
			wantBareHref: defaultGitLabProjectURL + "/-/merge_requests/897",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			id := "gd-multirepo-" + tc.project
			f.beads[id] = makeBead(id, tc.project)
			f.projectsResp = map[string]beadsapi.ProjectInfo{tc.project: tc.projectInfo}

			mux := mkServer(t, f)
			req := httptest.NewRequest("GET", "/"+id, nil)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d", w.Code)
			}
			body := w.Body.String()

			// Fully-qualified URL must always survive.
			if !strings.Contains(body, "site-deployment/-/merge_requests/901") {
				t.Errorf("expected fully-qualified !901 URL to survive, got body:\n%s", body)
			}

			// Bare !897 resolves to the bead's own project repo, or is left
			// as plain text when the project is ambiguously multi-repo.
			if tc.wantBareHref == "" {
				if strings.Contains(body, "/-/merge_requests/897") {
					t.Errorf("bare !897 should NOT be linkified for an ambiguous multi-repo project\nbody: %s", body)
				}
			} else {
				if !strings.Contains(body, `href="`+tc.wantBareHref+`"`) {
					t.Errorf("bare !897 should link to %q\nbody: %s", tc.wantBareHref, body)
				}
				// Guard the actual reported symptom: never silently fall back
				// to gardenio when the project named a different repo.
				if !strings.Contains(tc.wantBareHref, "gardenio") &&
					strings.Contains(body, "gardenio/-/merge_requests/897") {
					t.Errorf("bare !897 leaked to the gardenio default\nbody: %s", body)
				}
			}

			// Either way, the literal "!897" text must appear in the rendered
			// description (as plain text or as anchor body).
			if !strings.Contains(body, "!897") {
				t.Errorf("expected !897 to appear in body, got:\n%s", body)
			}
		})
	}
}

func TestBeadShow_LinkifyRespectsBasePath(t *testing.T) {
	f := newFake()
	f.beads["gd-bp"] = &beadsapi.BeadDetail{
		ID:          "gd-bp",
		Title:       "title",
		Type:        "task",
		Status:      "open",
		Description: "See gd-xxx for context.",
	}
	srv := NewServer(f, slog.Default(), "/beads", "", "", nil)
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/gd-bp", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	// Description goes through the rich linkifier; assert href substring +
	// visible text rather than the exact plain-anchor form.
	if !strings.Contains(body, `href="/beads/gd-xxx"`) || !strings.Contains(body, `>gd-xxx</a>`) {
		t.Errorf("expected linkified id to include basePath /beads, got:\n%s", body)
	}
}

func TestBeadShow_SimilarPanel_Renders(t *testing.T) {
	f := newFake()
	f.similarResp = &beadsapi.FindSimilarResponse{
		SourceID: "gd-aaa",
		Model:    "nomic-embed-text",
		Results: []*beadsapi.SemanticSearchResult{
			{
				Bead:  &beadsapi.BeadDetail{ID: "gd-sim1", Title: "Related work item"},
				Score: 0.912,
			},
			{
				Bead:  &beadsapi.BeadDetail{ID: "gd-sim2", Title: "Another related"},
				Score: 0.8317,
			},
		},
	}

	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-aaa", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if f.similarLastID != "gd-aaa" {
		t.Errorf("FindSimilar called with id=%q, want gd-aaa", f.similarLastID)
	}
	if f.similarLastLimit != 8 {
		t.Errorf("FindSimilar called with limit=%d, want 8", f.similarLastLimit)
	}

	body := w.Body.String()
	for _, want := range []string{
		"Similar beads",
		"gd-sim1",
		"Related work item",
		"0.912",
		"gd-sim2",
		"Another related",
		"0.832",
		`href="/gd-sim1"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("similar panel missing %q\nbody=%s", want, body)
		}
	}
}

// TestBeadShow_SimilarPanel_HiddenOnError: if FindSimilar returns an error
// (embedder disabled, bead has no embedding, daemon down, …) the panel must
// disappear and the page must still render as 200.
func TestBeadShow_SimilarPanel_HiddenOnError(t *testing.T) {
	f := newFake()
	f.similarErr = &beadsapi.APIError{StatusCode: http.StatusServiceUnavailable, Message: "embedder disabled"}

	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-aaa", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "Similar beads") {
		t.Errorf("expected panel hidden on error, body=%s", body)
	}
}

// TestBeadShow_SimilarPanel_HiddenWhenEmpty: no matches → no header.
func TestBeadShow_SimilarPanel_HiddenWhenEmpty(t *testing.T) {
	f := newFake()
	f.similarResp = &beadsapi.FindSimilarResponse{SourceID: "gd-aaa", Results: nil}

	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-aaa", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "Similar beads") {
		t.Errorf("expected panel hidden on empty results")
	}
}

// TestBeadShow_SimilarPanel_LinkifiesTitles: bead IDs embedded in titles
// should be linkified like any other user-visible bead reference.
func TestBeadShow_SimilarPanel_LinkifiesTitles(t *testing.T) {
	f := newFake()
	f.similarResp = &beadsapi.FindSimilarResponse{
		Results: []*beadsapi.SemanticSearchResult{
			{
				Bead:  &beadsapi.BeadDetail{ID: "gd-sim1", Title: "Follow-up to gd-zzz"},
				Score: 0.75,
			},
		},
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-aaa", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, `<a href="/gd-zzz">gd-zzz</a>`) {
		t.Errorf("expected linkified gd-zzz in similar title, body=%s", body)
	}
}

// TestBeadShow_TaintBadges_Renders verifies the ADR docs/adr/taints.md §9.1
// acceptance criterion directly: a bead with two taints shows two badges,
// each carrying its owner team's accent color and the propagation chain
// (name, source, and a working link to source_bead_id when present) in the
// hover/tap detail.
func TestBeadShow_TaintBadges_Renders(t *testing.T) {
	f := newFake()
	f.beadTaints = map[string][]beadsapi.BeadTaint{
		"gd-aaa": {
			{BeadID: "gd-aaa", TaintName: "pii", Source: "explicit"},
			{BeadID: "gd-aaa", TaintName: "security-sensitive", Source: "inherit:parent", SourceBeadID: "gd-parent"},
		},
	}
	f.taintRegistryResp = []beadsapi.TaintRegistryEntry{
		{Name: "pii", OwnerTeam: "privacy"},
		{Name: "security-sensitive", OwnerTeam: "appsec"},
	}

	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-aaa", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	if got := strings.Count(body, `class="taint-badge-chip"`); got != 2 {
		t.Errorf("expected 2 taint badge chips, got %d\nbody=%s", got, body)
	}

	piiAccent := ownerInitialsHue("privacy")
	appsecAccent := ownerInitialsHue("appsec")
	for _, want := range []string{
		"pii",
		"security-sensitive",
		"owner: privacy",
		"owner: appsec",
		"Applied explicitly",
		"Inherited from parent",
		`--taint-accent: ` + piiAccent,
		`--taint-accent: ` + appsecAccent,
		`href="/gd-parent"`,
		"source: gd-parent",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("taint badges missing %q\nbody=%s", want, body)
		}
	}

	// No action buttons — "Request scrub" / "Apply taint" land with
	// p2-viewer-taint-detail, not this task.
	for _, unwanted := range []string{"Request scrub", "Apply taint"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("did not expect action button %q on this task's badges, body=%s", unwanted, body)
		}
	}
}

// TestBeadShow_TaintBadges_HiddenWhenEmpty: a bead with no taints renders no
// badge markup at all.
func TestBeadShow_TaintBadges_HiddenWhenEmpty(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/gd-aaa", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if body := w.Body.String(); strings.Contains(body, `class="taint-badges"`) {
		t.Errorf("expected no taint badge markup for an untainted bead, body=%s", body)
	}
}

// TestBeadShow_TaintBadges_HiddenOnError: a GetBeadTaints failure (daemon
// down, etc.) degrades to no badges rather than failing the page render —
// matching every other best-effort panel on this handler.
func TestBeadShow_TaintBadges_HiddenOnError(t *testing.T) {
	f := newFake()
	f.beadTaintsErr = &beadsapi.APIError{StatusCode: http.StatusServiceUnavailable, Message: "daemon down"}

	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-aaa", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if body := w.Body.String(); strings.Contains(body, `class="taint-badges"`) {
		t.Errorf("expected no taint badge markup on GetBeadTaints error, body=%s", body)
	}
}

// TestBeadShow_TaintBadges_RegistryErrorStillRendersBadge: a ListTaints
// registry failure loses the owner accent color but must not hide the
// badge itself — the bead is still tainted and the viewer should still see
// that.
func TestBeadShow_TaintBadges_RegistryErrorStillRendersBadge(t *testing.T) {
	f := newFake()
	f.beadTaints = map[string][]beadsapi.BeadTaint{
		"gd-aaa": {{BeadID: "gd-aaa", TaintName: "pii", Source: "explicit"}},
	}
	f.taintRegistryErr = &beadsapi.APIError{StatusCode: http.StatusServiceUnavailable, Message: "registry down"}

	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-aaa", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `class="taint-badge-chip"`) {
		t.Errorf("expected badge to still render without owner team, body=%s", body)
	}
	if !strings.Contains(body, `--taint-accent: `+ownerInitialsHue("")) {
		t.Errorf("expected neutral fallback accent when registry lookup fails, body=%s", body)
	}
}

// TestBeadShow_CommentRendersMarkdown verifies that comment bodies go through
// the markdown renderer — agents and humans routinely post **bold**, `code`,
// and multi-line content, and seeing raw asterisks/backticks (the pre-fix
// behavior reported on this bead) reads as broken.
func TestBeadShow_CommentRendersMarkdown(t *testing.T) {
	f := newFake()
	f.beads["gd-mdc"] = &beadsapi.BeadDetail{
		ID:     "gd-mdc",
		Title:  "md comment target",
		Type:   "task",
		Status: "open",
	}
	f.comments["gd-mdc"] = []beadsapi.Comment{{
		Author:    "reviewer",
		Text:      "**Done.** Rolled up in `foo.go`.\nSecond line here.\n\n- bullet one\n- bullet two",
		CreatedAt: time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC),
	}}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-mdc", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	for _, want := range []string{
		"<strong>Done.</strong>", // bold
		"<code>foo.go</code>",    // inline code
		"<br",                    // hard wrap converts single \n to <br>
		">bullet one</li>",       // list item (<li> now carries a data-block-id, T1)
		">bullet two</li>",       // list item
	} {
		if !strings.Contains(body, want) {
			t.Errorf("comment render missing %q\nbody: %s", want, body)
		}
	}
	// Raw markdown must not leak through as literal text.
	if strings.Contains(body, "**Done.**") {
		t.Errorf("comment body still contains raw markdown ** asterisks\nbody: %s", body)
	}
}

// TestBeadShow_CommentEscapesRawHTML verifies that raw HTML in a comment
// stays escaped (goldmark Unsafe defaults to false). Otherwise comments
// would become an XSS vector.
func TestBeadShow_CommentEscapesRawHTML(t *testing.T) {
	f := newFake()
	f.beads["gd-xss-c"] = &beadsapi.BeadDetail{
		ID:     "gd-xss-c",
		Title:  "xss comment target",
		Type:   "task",
		Status: "open",
	}
	f.comments["gd-xss-c"] = []beadsapi.Comment{{
		Author:    "attacker",
		Text:      `<script>alert(1)</script> and <img src=x onerror=alert(2)>`,
		CreatedAt: time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC),
	}}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-xss-c", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Errorf("raw <script> leaked into comment render\nbody: %s", body)
	}
	// Raw HTML renders as its escaped source (gd-oNoa0zf55W), so the text
	// "onerror=" is on the page; what must never appear is a tag carrying it.
	if regexp.MustCompile(`<[^>]*onerror=`).MatchString(body) {
		t.Errorf("raw onerror attribute leaked into comment render\nbody: %s", body)
	}
	if !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Errorf("raw <script> was not rendered as its escaped source\nbody: %s", body)
	}
}

// TestBeadShow_CopyButtons asserts the per-bead copy-to-clipboard buttons
// (Copy ID + Copy permalink) render next to the bead ID. The actual click
// behavior is JS — we just verify the markup is in place so the static JS
// has something to bind to. See ka-0000003083.
func TestBeadShow_CopyButtons(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/gd-aaa", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()

	for _, want := range []string{
		`class="copy-btn" data-copy="id" data-value="gd-aaa"`,
		`class="copy-btn" data-copy="permalink"`,
		`>Copy ID<`,
		`>Copy permalink<`,
		`/assets/static/copy_buttons.js`,
		`/assets/static/preview_popover.js`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("bead_show missing %q\nbody: %s", want, body)
		}
	}
}

// TestBeadShow_ConstellationExpandedByDefault verifies the constellation
// graph is wrapped in a <details> element that defaults to OPEN (ka-0000004042:
// "make display on by default"), and that the reveal-strip exposes a one-click
// link to flip the default back to collapsed.
func TestBeadShow_ConstellationExpandedByDefault(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/gd-aaa", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	if !strings.Contains(body, `class="constellation-reveal"`) {
		t.Errorf("expected constellation-reveal wrapper, got:\n%s", body)
	}
	// Without the cookie / query param, the <details> must be open.
	if !strings.Contains(body, `class="constellation-reveal" open`) {
		t.Errorf("expected constellation-reveal to default to open, got closed")
	}
	if !strings.Contains(body, "Constellation graph") {
		t.Errorf("expected obvious 'Constellation graph' reveal label, got:\n%s", body)
	}
	if !strings.Contains(body, "click to hide by default") {
		t.Errorf("expected pref toggle copy in reveal body, got:\n%s", body)
	}
}

// TestBeadShow_ConstellationCollapsedViaQuery verifies that
// `?constellation=collapsed` closes the <details> on first render AND sets the
// cookie so the preference persists across reloads.
func TestBeadShow_ConstellationCollapsedViaQuery(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/gd-aaa?constellation=collapsed", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, `class="constellation-reveal" open`) {
		t.Errorf("expected constellation-reveal to be closed with ?constellation=collapsed, got open")
	}
	if !strings.Contains(body, "click to show by default") {
		t.Errorf("expected 'click to show by default' toggle copy when collapsed, got:\n%s", body)
	}
	gotCookie := false
	for _, c := range w.Result().Cookies() {
		if c.Name == cookieConstellationOpen && c.Value == "collapsed" {
			gotCookie = true
		}
	}
	if !gotCookie {
		t.Errorf("expected %s=collapsed cookie to be set on query override", cookieConstellationOpen)
	}
}

// TestBeadShow_CommentPermalinks asserts each comment carries an
// `id="comment-<id>"` anchor and a clickable timestamp linking to the
// in-page fragment. See ka-0000001537.
func TestBeadShow_CommentPermalinks(t *testing.T) {
	f := newFake()
	f.beads["gd-perm"] = &beadsapi.BeadDetail{
		ID:     "gd-perm",
		Title:  "permalink target",
		Type:   "task",
		Status: "open",
	}
	f.comments["gd-perm"] = []beadsapi.Comment{
		{ID: 7, Author: "alice", Text: "first", CreatedAt: time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)},
		{ID: 42, Author: "bob", Text: "second", CreatedAt: time.Date(2026, 4, 20, 13, 0, 0, 0, time.UTC)},
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-perm", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`id="comment-7"`,
		`id="comment-42"`,
		`href="#comment-7"`,
		`href="#comment-42"`,
		`class="comment-permalink"`,
		`data-comment-id="7"`,
		`data-comment-id="42"`,
		`/assets/static/comment_links.js`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("comment permalink markup missing %q\nbody: %s", want, body)
		}
	}
}

// TestBeadShow_Snapshot is a minimal snapshot-style check: the rendered
// HTML must contain the stable structural markers (title, id, description,
// dependency link, comment) in roughly the expected order.
func TestBeadShow_Snapshot(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/gd-aaa", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	markers := []string{
		`<h1 class="bead-title">Example bead</h1>`,
		`<code>gd-aaa</code>`,
		`<h3 aria-level="2">Description</h3>`,
		"<h2>Comments (1)</h2>",
		"<h2>Metadata</h2>",
		"<h2>Dependencies</h2>",
		`<a href="/gd-parent"`,
	}
	last := 0
	for _, m := range markers {
		idx := strings.Index(body[last:], m)
		if idx < 0 {
			t.Errorf("snapshot missing marker %q after position %d\nbody: %s", m, last, body)
			return
		}
		last += idx + len(m)
	}
}

// TestBeadShow_NarrativeAboveFieldsAndConstellation locks in ka-0000006872:
// on a generic bead the description/comments narrative must render ABOVE both
// the generic Fields dump AND the constellation graph. Historically the Fields
// list was hoisted right under the title and the constellation reveal sat atop
// the main column, pushing the human-readable content down — Matt asked for the
// inverse default. gd-aaa is a task (generic type) carrying Fields, so the
// generic dump renders in the main column beneath comments.
func TestBeadShow_NarrativeAboveFieldsAndConstellation(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/gd-aaa", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	idxDesc := strings.Index(body, `<h3 aria-level="2">Description</h3>`)
	idxComments := strings.Index(body, "<h2>Comments (")
	idxFields := strings.Index(body, `<h3 aria-level="2">Fields</h3>`)
	idxConstellation := strings.Index(body, `class="constellation-reveal"`)

	if idxDesc < 0 || idxComments < 0 || idxFields < 0 || idxConstellation < 0 {
		t.Fatalf("missing a required section: desc=%d comments=%d fields=%d constellation=%d\nbody: %s",
			idxDesc, idxComments, idxFields, idxConstellation, body)
	}
	// Narrative first: description then comments.
	if !(idxDesc < idxComments) {
		t.Errorf("expected Description (%d) before Comments (%d)", idxDesc, idxComments)
	}
	// Both narrative blocks must precede the generic Fields dump...
	if !(idxComments < idxFields) {
		t.Errorf("expected Comments (%d) above the Fields dump (%d) — narrative should be on top", idxComments, idxFields)
	}
	// ...and the whole narrative + fields must precede the constellation graph.
	if !(idxFields < idxConstellation) {
		t.Errorf("expected Fields (%d) above the constellation graph (%d)", idxFields, idxConstellation)
	}
	if !(idxDesc < idxConstellation) {
		t.Errorf("expected Description (%d) above the constellation graph (%d)", idxDesc, idxConstellation)
	}
	// Regression guard: the Fields dump must NOT be hoisted above the
	// description anymore (its old position under the title).
	if idxFields < idxDesc {
		t.Errorf("Fields dump (%d) is still hoisted above Description (%d) — the reorder regressed", idxFields, idxDesc)
	}
}

// TestBeadShow_OperatedByBanner_ResolvedHuman locks in the happy path for
// ka-0000003352: when the daemon attaches a real human display name to
// operated_by, the royal-purple banner renders that name and the raw
// `slack:U…` alias never escapes as the headline text.
func TestBeadShow_OperatedByBanner_ResolvedHuman(t *testing.T) {
	f := newFake()
	f.beads["gd-opb-ok"] = &beadsapi.BeadDetail{
		ID:                "gd-opb-ok",
		Title:             "resolved operator",
		Type:              "task",
		Status:            "open",
		OperatedBy:        "slack:U0OPERATOR1",
		OperatedByDisplay: "Matt Wood",
		OperatedByEmail:   "matt@example.com",
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-opb-ok", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	if !strings.Contains(body, `class="bead-operated-by-banner"`) {
		t.Errorf("expected royal-purple banner wrapper, body=%s", body)
	}
	if !strings.Contains(body, `class="bead-operated-by-name">Matt Wood</strong>`) {
		t.Errorf("expected human display name in banner, body=%s", body)
	}
	// The raw `slack:U…` alias must NEVER appear as visible text in the banner.
	// The data-operated-by attribute is fine (machine-readable; not rendered),
	// so we scope the prohibition to the rendered name slot.
	if strings.Contains(body, `class="bead-operated-by-name">slack:U0OPERATOR1`) {
		t.Errorf("raw slack alias leaked into banner name slot, body=%s", body)
	}
	if strings.Contains(body, `class="bead-operated-by-banner bead-operated-by-banner-unresolved"`) {
		t.Errorf("resolved banner must not carry the unresolved variant class, body=%s", body)
	}
}

// TestBeadShow_OperatedByBanner_UnresolvedSlackAlias verifies the
// regression-worthy red treatment when the daemon could not attach a
// human display name to a slack: alias. The banner must NOT fall back to
// rendering the raw `slack:U…` alias as the headline (regression in
// ka-0000003352 acceptance criteria).
func TestBeadShow_OperatedByBanner_UnresolvedSlackAlias(t *testing.T) {
	f := newFake()
	f.beads["gd-opb-bad"] = &beadsapi.BeadDetail{
		ID:         "gd-opb-bad",
		Title:      "unresolved operator",
		Type:       "task",
		Status:     "open",
		OperatedBy: "slack:U0OPERATOR1",
		// OperatedByDisplay deliberately empty — simulates failed lookup.
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-opb-bad", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	if !strings.Contains(body, `class="bead-operated-by-banner bead-operated-by-banner-unresolved"`) {
		t.Errorf("expected unresolved-variant class on banner, body=%s", body)
	}
	if !strings.Contains(body, `(unresolved operator)`) {
		t.Errorf("expected '(unresolved operator)' label, body=%s", body)
	}
	// Hard guard: the raw `slack:U…` alias must never appear inside a
	// `<strong …>` slot — that's the headline render slot, where leaking
	// the opaque id is the regression.
	if strings.Contains(body, `<strong class="bead-operated-by-name">slack:`) {
		t.Errorf("raw slack alias leaked into name slot on unresolved render, body=%s", body)
	}
	if strings.Contains(body, `<strong class="bead-operated-by-name">slack:U0OPERATOR1`) {
		t.Errorf("raw slack id rendered as resolved name on unresolved bead, body=%s", body)
	}
}

// TestBeadShow_OperatedByBanner_AbsentForNonHuman verifies the banner is
// suppressed for beads with no human operator — scheduler / agent / empty
// states are already disclosed by the metadata sidebar, and a banner there
// would be noise.
func TestBeadShow_OperatedByBanner_AbsentForNonHuman(t *testing.T) {
	f := newFake()
	f.beads["gd-opb-none"] = &beadsapi.BeadDetail{
		ID:        "gd-opb-none",
		Title:     "no operator",
		Type:      "task",
		Status:    "open",
		CreatedBy: "sched-some-gardener-abc",
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-opb-none", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, `class="bead-operated-by-banner"`) ||
		strings.Contains(body, `class="bead-operated-by-banner bead-operated-by-banner-unresolved"`) {
		t.Errorf("banner must be suppressed when there's no human operator, body=%s", body)
	}
}

// TestBeadShow_AgentAuthoredChat verifies that opening a type=agent bead
// renders the agent's OWN squawks/mail in the chat panel (ka-0000002848).
// Agent squawks/mail are linked by a `from:<agent-title>` label, not typed
// mentions, so the mention-scoped path is empty; the handler must additionally
// fetch by that label and feed them into the existing chat panel.
func TestBeadShow_AgentAuthoredChat(t *testing.T) {
	f := newFake()
	f.beads["gd-agent1"] = &beadsapi.BeadDetail{
		ID:     "gd-agent1",
		Title:  "my-worker-9xb",
		Type:   "agent",
		Status: "open",
	}
	// The agent's authored messages — returned by the `from:` label fetch and
	// hydrated via BatchGetBeads (so they live in f.beads too).
	older := &beadsapi.BeadDetail{
		ID: "gd-sq1", Type: "message", Status: "closed",
		Labels:    []string{"from:my-worker-9xb", "squawk"},
		Fields:    map[string]string{"text": "first squawk line", "source_agent": "my-worker-9xb"},
		CreatedAt: time.Date(2026, 6, 11, 9, 0, 0, 0, time.UTC),
	}
	newer := &beadsapi.BeadDetail{
		ID: "gd-ml1", Type: "mail", Status: "open",
		Labels:    []string{"from:my-worker-9xb"},
		Fields:    map[string]string{"text": "a later mail line"},
		CreatedAt: time.Date(2026, 6, 11, 9, 30, 0, 0, time.UTC),
	}
	f.beads["gd-sq1"] = older
	f.beads["gd-ml1"] = newer
	// Newest-first, mirroring the daemon's -created_at sort.
	f.agentMsgsResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{newer, older}}

	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-agent1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	// The chat panel must render with both authored messages.
	if !strings.Contains(body, "Chat thread") {
		t.Errorf("agent chat panel missing — expected 'Chat thread' header\nbody: %s", body)
	}
	for _, want := range []string{"first squawk line", "a later mail line"} {
		if !strings.Contains(body, want) {
			t.Errorf("agent chat panel missing message %q", want)
		}
	}

	// The handler must have queried by the from:<title> label across
	// message+mail types, newest-first.
	var got *beadsapi.ListBeadsQuery
	for i := range f.listAllQ {
		q := f.listAllQ[i]
		if slices.Contains(q.Labels, "from:my-worker-9xb") {
			got = &f.listAllQ[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("handler never queried agent-authored messages by from: label; queries=%+v", f.listAllQ)
	}
	if !slices.Contains(got.Types, "message") || !slices.Contains(got.Types, "mail") {
		t.Errorf("agent-authored query Types=%v, want both message and mail", got.Types)
	}
	if got.Sort != "-created_at" {
		t.Errorf("agent-authored query Sort=%q, want -created_at", got.Sort)
	}
}

// TestBeadShow_ChatPanelDirectionAttribution verifies that on a work (task)
// bead, the Chat thread panel visually distinguishes a human Slack reply from
// an agent squawk: the human reply renders with dir-from-user + the slack kind
// badge, the agent squawk with dir-from-agent + the squawk badge
// (ka-0000000676). Both are pulled into the panel via the mentioned-by path.
func TestBeadShow_ChatPanelDirectionAttribution(t *testing.T) {
	f := newFake()
	f.beads["gd-work"] = &beadsapi.BeadDetail{
		ID: "gd-work", Title: "some work bead", Type: "task", Status: "in_progress",
	}
	// A human Slack reply forwarded by the bridge: slack-bridge author, human
	// operated_by, conversation label, NO source_agent.
	humanReply := &beadsapi.BeadDetail{
		ID: "gd-mprkiihw1m", Type: "message", Status: "closed",
		Labels:            []string{"slack-thread-reply", "slack-thread-message"},
		Fields:            map[string]string{"text": "user reply body text"},
		CreatedBy:         "slack-bridge",
		OperatedBy:        "oidc:matt@x",
		OperatedByDisplay: "Matt",
		CreatedAt:         time.Date(2026, 7, 3, 9, 0, 0, 0, time.UTC),
	}
	// An agent squawk: source_agent set, from: label.
	agentSquawk := &beadsapi.BeadDetail{
		ID: "gd-agentsq", Type: "message", Status: "closed",
		Labels:    []string{"from:worker-x", "squawk"},
		Fields:    map[string]string{"text": "agent squawk body text", "source_agent": "worker-x"},
		CreatedBy: "worker-x", OperatedBy: "slack:U07",
		CreatedAt: time.Date(2026, 7, 3, 9, 5, 0, 0, time.UTC),
	}
	f.beads["gd-mprkiihw1m"] = humanReply
	f.beads["gd-agentsq"] = agentSquawk
	// Both reference the work bead in prose → surfaced via mentioned-by.
	f.mentionedBy = map[string][]beadsapi.BeadReferenceSummary{
		"gd-work": {
			{ID: "gd-mprkiihw1m", Type: "message"},
			{ID: "gd-agentsq", Type: "message"},
		},
	}

	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-work", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	if !strings.Contains(body, "Chat thread") {
		t.Fatalf("chat panel missing 'Chat thread' header\nbody: %s", body)
	}
	// Both bodies present.
	for _, want := range []string{"user reply body text", "agent squawk body text"} {
		if !strings.Contains(body, want) {
			t.Errorf("chat panel missing body %q", want)
		}
	}
	// Direction classes present (attribution rendered).
	if !strings.Contains(body, "bead-chat-msg dir-from-user") {
		t.Errorf("human reply not rendered with dir-from-user\nbody: %s", body)
	}
	if !strings.Contains(body, "bead-chat-msg dir-from-agent") {
		t.Errorf("agent squawk not rendered with dir-from-agent\nbody: %s", body)
	}
	// Kind badges present.
	if !strings.Contains(body, "kind-slack") {
		t.Errorf("human Slack reply missing kind-slack badge")
	}
	if !strings.Contains(body, "kind-squawk") {
		t.Errorf("agent squawk missing kind-squawk badge")
	}
}

// TestBeadShow_NonAgentSkipsAuthoredFetch verifies the agent-authored fetch is
// scoped to type=agent beads only — a plain task bead must NOT issue a
// from:<title> label query (ka-0000002848).
func TestBeadShow_NonAgentSkipsAuthoredFetch(t *testing.T) {
	f := newFake()
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-aaa", nil) // default task bead
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	for _, q := range f.listAllQ {
		for _, l := range q.Labels {
			if strings.HasPrefix(l, "from:") {
				t.Errorf("non-agent bead issued a from: authored-message query: %+v", q)
			}
		}
	}
}

// TestIsUnresolvedOperatorDisplay covers the boundary helper that gates the
// red regression treatment: empty / equal-to-raw / slack: / oidc: prefixes
// all count as unresolved; a real human name passes through.
func TestIsUnresolvedOperatorDisplay(t *testing.T) {
	cases := []struct {
		name, display, raw string
		want               bool
	}{
		{"empty display", "", "slack:U1", true},
		{"display equals raw slack alias", "slack:U1", "slack:U1", true},
		{"display equals raw oidc alias", "oidc:a@b", "oidc:a@b", true},
		{"display still slack-prefixed", "slack:U1", "", true},
		{"display still oidc-prefixed", "oidc:a@b", "", true},
		{"resolved human name", "Matt Wood", "slack:U1", false},
		{"resolved email-shaped display", "matt@example.com", "slack:U1", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isUnresolvedOperatorDisplay(c.display, c.raw); got != c.want {
				t.Errorf("isUnresolvedOperatorDisplay(%q,%q)=%v, want %v", c.display, c.raw, got, c.want)
			}
		})
	}
}
