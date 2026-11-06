package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"gardenio/kbeads/internal/buildinfo"
	"gardenio/kbeads/internal/landmap"
)

// beadsMaxBodyBytes bounds JSON request bodies for mutating HTTP handlers. A
// single mutation payload should not exceed a few KB; 1 MiB leaves ample
// headroom for legitimate requests (bead fields, comments, escalation specs)
// while preventing pre-auth memory exhaustion from a streamed or oversized
// body. See AUDIT-2026-04-18 §H1.
const beadsMaxBodyBytes = 1 << 20 // 1 MiB

// beadsMaxConfigBodyBytes is a larger bound for PUT /v1/configs/{key} to
// accommodate formulas, type-config JSON, and similar payloads that can be
// meaningfully larger than a single bead mutation. Still bounded to avoid
// unbounded pre-auth memory growth.
const beadsMaxConfigBodyBytes = 4 << 20 // 4 MiB

// decodeJSONBody wraps r.Body with http.MaxBytesReader(beadsMaxBodyBytes) and
// JSON-decodes it into v. On failure it writes an error response (413 for
// oversize bodies, 400 otherwise) and returns false. Callers that need a
// larger bound should use decodeJSONBodyLimit.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, v any) bool {
	return decodeJSONBodyLimit(w, r, v, beadsMaxBodyBytes)
}

// decodeJSONBodyLimit is decodeJSONBody with a caller-chosen size limit.
func decodeJSONBodyLimit(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return false
		}
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

// decodeJSONBodyOptional decodes r.Body into v if present but swallows any
// decode errors (matching the pre-existing best-effort pattern for endpoints
// that accept optional bodies). The body is still bounded by MaxBytesReader.
// Returns false only when the body exceeded the size cap (in which case an
// error response has been written); returns true otherwise.
func decodeJSONBodyOptional(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, beadsMaxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return false
		}
	}
	return true
}

// readBoundedBody wraps r.Body with http.MaxBytesReader(beadsMaxBodyBytes) and
// reads all bytes. On failure it writes an error response and returns
// (nil, false). Use for handlers that need the raw bytes for re-decoding /
// escalation replay.
func readBoundedBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, beadsMaxBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return nil, false
		}
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return nil, false
	}
	return body, true
}

// NewHTTPHandler returns an http.Handler with all routes registered.
// When fleet tokens are configured or verifier is non-nil, requests (except
// GET /v1/health) must include a valid Authorization: Bearer <token> header:
// a fleet token or an OIDC/machine JWT. There is no shared static token
// (gd-ke6PC7bRto).
func (s *BeadsServer) NewHTTPHandler(verifier TokenVerifier, fleetCfg ...*FleetConfig) http.Handler {
	mux := http.NewServeMux()
	s.registerRoutes(scopeGatedMux{mux: mux, st: s.store, tenant: s.callerIsTenant})
	return s.wrapHTTPChain(mux, verifier, fleetCfg)
}

// registerRoutes registers every REST route on mux. It is the single
// registration site: the scoped-route guard test (scoped_route_classes_test.go)
// walks it, so a route registered anywhere else escapes the classification.
func (s *BeadsServer) registerRoutes(mux routeRegistrar) {
	// --- viewer: read-only access to beads, graphs, dashboards, reports ---
	mux.HandleFunc("GET /v1/beads", requireRole(RoleViewer, s.handleListBeads))
	mux.HandleFunc("GET /v1/beads/{id}", requireRole(RoleViewer, s.handleGetBead))
	mux.HandleFunc("GET /v1/beads/{id}/dependencies", requireRole(RoleViewer, s.handleGetDependencies))
	mux.HandleFunc("GET /v1/beads/{id}/dependents", requireRole(RoleViewer, s.handleGetReverseDependencies))
	mux.HandleFunc("GET /v1/beads/{id}/mentioned-by", requireRole(RoleViewer, s.handleListMentionedBy))
	mux.HandleFunc("GET /v1/beads/{id}/mentions", requireRole(RoleViewer, s.handleListMentions))
	mux.HandleFunc("GET /v1/beads/{id}/labels", requireRole(RoleViewer, s.handleGetLabels))
	mux.HandleFunc("GET /v1/beads/{id}/comments", requireRole(RoleViewer, s.handleGetComments))
	mux.HandleFunc("GET /v1/beads/{id}/followers", requireRole(RoleViewer, s.handleListFollowers))
	mux.HandleFunc("GET /v1/beads/{id}/taints", requireRole(RoleViewer, s.handleGetBeadTaints))
	mux.HandleFunc("GET /v1/beads/{id}/events", requireRole(RoleViewer, s.handleGetEvents))
	mux.HandleFunc("GET /v1/beads/{id}/mutations", requireRole(RoleViewer, s.handleListMutations))
	mux.HandleFunc("GET /v1/beads/{id}/operator-history", requireRole(RoleViewer, s.handleGetOperatorHistory))
	mux.HandleFunc("GET /v1/events", requireRole(RoleViewer, s.handleListGlobalEvents))
	mux.HandleFunc("GET /v1/beads/{id}/similar", requireRole(RoleViewer, s.handleFindSimilar))
	mux.HandleFunc("POST /v1/search/semantic", requireRole(RoleViewer, s.handleSemanticSearch))
	mux.HandleFunc("GET /v1/graph", requireRole(RoleViewer, s.handleGraph))
	mux.HandleFunc("POST /v1/graph", requireRole(RoleViewer, s.handleGraph))
	mux.HandleFunc("GET /v1/stats", requireRole(RoleViewer, s.handleGetStats))
	mux.HandleFunc("GET /v1/ready", requireRole(RoleViewer, s.handleGetReady))
	mux.HandleFunc("GET /v1/decisions/{id}", requireRole(RoleViewer, s.handleGetDecision))
	mux.HandleFunc("GET /v1/decisions", requireRole(RoleViewer, s.handleListDecisions))
	mux.HandleFunc("GET /v1/escalations/{id}", requireRole(RoleViewer, s.handleGetEscalation))
	mux.HandleFunc("GET /v1/escalations/{id}/permission-diff", requireRole(RoleViewer, s.handleGetEscalationPermissionDiff))
	mux.HandleFunc("GET /v1/escalations", requireRole(RoleViewer, s.handleListEscalations))
	// Runners (local-agents Task 7, runners.go). register is pre-auth: the
	// invite code in the body is the credential (AuthMiddleware exception).
	mux.HandleFunc("POST /v1/runners/register", s.handleRegisterRunner)
	mux.HandleFunc("GET /v1/runners/{id}", requireRole(RoleViewer, s.handleGetRunner))
	mux.HandleFunc("POST /v1/runners/{id}/heartbeat", requireRole(RoleOperator, s.handleRunnerHeartbeat))
	mux.HandleFunc("POST /v1/runners/{id}/revoke", requireRole(RoleOperator, s.handleRevokeRunner))
	mux.HandleFunc("GET /v1/batch/beads", requireRole(RoleViewer, s.handleBatchGetBeads))
	mux.HandleFunc("GET /v1/batch/children-by-status", requireRole(RoleViewer, s.handleBatchChildrenByStatus))
	mux.HandleFunc("GET /v1/batch/dependencies", requireRole(RoleViewer, s.handleBatchGetDependencies))
	mux.HandleFunc("GET /v1/configs/{key...}", requireRole(RoleViewer, s.handleGetConfig))
	mux.HandleFunc("GET /v1/configs", requireRole(RoleViewer, s.handleListConfigs))
	mux.HandleFunc("GET /v1/agents/{id}/gates", requireRole(RoleViewer, s.handleListGates))
	mux.HandleFunc("GET /v1/agents/roster", requireRole(RoleViewer, s.handleAgentRoster))
	mux.HandleFunc("GET /v1/agents-by-name/{name}", requireRole(RoleViewer, s.handleFindAgentByName))
	mux.HandleFunc("GET /v1/agents-by-name:batch", requireRole(RoleViewer, s.handleFindAgentsByNames))
	mux.HandleFunc("GET /v1/health/snapshot", requireRole(RoleViewer, s.handleHealthSnapshot))
	mux.HandleFunc("GET /v1/bus/status", requireRole(RoleViewer, s.handleBusStatus))
	mux.HandleFunc("GET /v1/whoami", requireRole(RoleViewer, s.handleWhoami))
	mux.HandleFunc("GET /v1/ui/telemetry", requireRole(RoleViewer, s.handleListUITelemetry))
	mux.HandleFunc("GET /v1/metrics/acceptance", requireRole(RoleViewer, s.handleAcceptanceMetrics))
	mux.HandleFunc("GET /v1/metrics/slow-tool-calls", requireRole(RoleViewer, s.handleListSlowToolCalls))
	mux.HandleFunc("GET /v1/heraldry/library", requireRole(RoleViewer, s.handleHeraldryLibrary))
	mux.HandleFunc("GET /v1/charts", requireRole(RoleViewer, s.handleChartCatalogList))
	mux.HandleFunc("GET /v1/charts/{chart_id}", requireRole(RoleViewer, s.handleChartRender))
	mux.HandleFunc("GET /v1/charts/uniqueness", requireRole(RoleViewer, s.handleChartsUniqueness))
	mux.HandleFunc("GET /v1/charts/engagement/activity", requireRole(RoleViewer, s.handleChartsEngagementActivity))
	mux.HandleFunc("GET /v1/charts/engagement/spend", requireRole(RoleViewer, s.handleChartsEngagementSpend))
	mux.HandleFunc("GET /v1/charts/security/authz", requireRole(RoleViewer, s.handleChartsSecurityAuthz))
	mux.HandleFunc("GET /v1/charts/security/decisions", requireRole(RoleViewer, s.handleChartsSecurityDecisions))
	mux.HandleFunc("GET /v1/charts/security/token-slo", requireRole(RoleViewer, s.handleChartsSecurityTokenSLO))
	mux.HandleFunc("GET /v1/charts/engagement/sessions", requireRole(RoleViewer, s.handleChartsEngagementSessions))
	mux.HandleFunc("GET /v1/taints", requireRole(RoleViewer, s.handleListTaints))
	mux.HandleFunc("GET /v1/taints/{name}", requireRole(RoleViewer, s.handleGetTaint))
	mux.HandleFunc("GET /v1/teams/{id}/taint-summary", requireRole(RoleViewer, s.handleTeamTaintSummary))
	mux.HandleFunc("GET /v1/releases", requireRole(RoleViewer, s.handleListReleases))
	mux.HandleFunc("GET /v1/releases/{id}", requireRole(RoleViewer, s.handleGetRelease))
	// Taint detail page (docs/adr/taints.md §9.1, task gd-nOnc1Q8Zcv): Grants
	// and Audit are owner-team-or-admin gated inside the handler
	// (canSeeTaintDetail), same as scrub_policy visibility on GET
	// /v1/taints/{name}. Beads and Scrub-requests are open to any viewer at
	// the route level — scopeListFilter inside the handler is itself the
	// authorization, same as GET /v1/beads.
	mux.HandleFunc("GET /v1/taints/{name}/grants", requireRole(RoleViewer, s.handleListTaintGrants))
	mux.HandleFunc("GET /v1/taints/{name}/audit", requireRole(RoleViewer, s.handleListTaintAudit))
	mux.HandleFunc("GET /v1/taints/{name}/beads", requireRole(RoleViewer, s.handleListTaintBeads))
	mux.HandleFunc("GET /v1/taints/{name}/scrub-requests", requireRole(RoleViewer, s.handleListTaintScrubRequests))

	// --- operator: beads CRUD, decisions, agent management, SSE streams ---
	mux.HandleFunc("POST /v1/beads", requireRole(RoleOperator, s.handleCreateBead))
	// Releases (http_releases.go): the handlers run the release writer check.
	mux.HandleFunc("POST /v1/releases", requireRole(RoleOperator, s.handleCreateRelease))
	mux.HandleFunc("POST /v1/releases/{id}/deployments", requireRole(RoleOperator, s.handleDeployRelease))
	mux.HandleFunc("POST /v1/releases/{id}/contents", requireRole(RoleOperator, s.handleAppendReleaseContents))
	mux.HandleFunc("POST /v1/releases/repair-deployments", requireRole(RoleOperator, s.handleRepairReleaseDeployments))
	mux.HandleFunc("PATCH /v1/beads/{id}", requireRole(RoleOperator, s.handleUpdateBead))
	mux.HandleFunc("POST /v1/beads/{id}/close", requireRole(RoleOperator, s.handleCloseBead))
	mux.HandleFunc("POST /v1/beads/{id}/increment", requireRole(RoleOperator, s.handleIncrementBeadFields))
	mux.HandleFunc("POST /v1/beads/{id}/fields/{field}/append", requireRole(RoleOperator, s.handleAppendBeadArrayField))
	mux.HandleFunc("POST /v1/beads/{id}/fields/{field}/reorder", requireRole(RoleOperator, s.handleReorderBeadArrayField))
	mux.HandleFunc("POST /v1/beads/{id}/fields/{field}/edit", requireRole(RoleOperator, s.handleEditBeadSubField))
	mux.HandleFunc("POST /v1/beads/{id}/transfer", requireRole(RoleAdmin, s.handleTransferBead))
	mux.HandleFunc("POST /v1/beads/{id}/dependencies", requireRole(RoleOperator, s.handleAddDependency))
	mux.HandleFunc("DELETE /v1/beads/{id}/dependencies", requireRole(RoleOperator, s.handleRemoveDependency))
	mux.HandleFunc("POST /v1/beads/{id}/labels", requireRole(RoleOperator, s.handleAddLabel))
	mux.HandleFunc("DELETE /v1/beads/{id}/labels/{label}", requireRole(RoleOperator, s.handleRemoveLabel))
	mux.HandleFunc("POST /v1/beads/{id}/comments", requireRole(RoleOperator, s.handleAddComment))
	mux.HandleFunc("POST /v1/beads/{id}/comments/trim", requireRole(RoleOperator, s.handleTrimComments))
	mux.HandleFunc("DELETE /v1/beads/{id}/comments/{comment_id}", requireRole(RoleOperator, s.handleDeleteComment))
	mux.HandleFunc("POST /v1/beads/{id}/followers", requireRole(RoleOperator, s.handleAddFollower))
	mux.HandleFunc("DELETE /v1/beads/{id}/followers/{operator_id}", requireRole(RoleOperator, s.handleRemoveFollower))
	// Taint grants/apply (docs/adr/taints.md §7): grant/revoke restrict to the
	// taint's owner team or an admin, apply to a grant holder — both enforced
	// inside the handler (bespoke, not the bead-ownership gate), so these stay
	// on requireRole(RoleOperator) only as the same OIDC-only baseline every
	// other mutation route carries.
	// Policy tab save (docs/adr/taints.md §9.1, task gd-nOnc1Q8Zcv): same
	// owner-team-or-admin gate as grant/revoke, enforced inside the handler
	// (callerMayManageTaint).
	mux.HandleFunc("PATCH /v1/taints/{name}", requireRole(RoleOperator, s.handleUpdateTaintPolicy))
	mux.HandleFunc("POST /v1/taints/{name}/grants", requireRole(RoleOperator, s.handleGrantTaint))
	mux.HandleFunc("DELETE /v1/taints/{name}/grants/{principal}", requireRole(RoleOperator, s.handleRevokeTaint))
	mux.HandleFunc("POST /v1/beads/{id}/taints", requireRole(RoleOperator, s.handleApplyTaint))
	// Grant requests (ADR docs/adr/taints.md §9.2/§9.4): files a `grant`
	// escalation addressed to the taint's owner team. Deliberately NOT
	// restricted to the owner team or an admin (unlike the three routes
	// above) — the whole point is a caller who does NOT hold the taint asking
	// for it. RoleOperator here is just the standard write-route baseline,
	// same as POST /v1/beads.
	mux.HandleFunc("POST /v1/taints/{name}/grant-requests", requireRole(RoleOperator, s.handleGrantTaintRequest))
	// Scrub-request dedicated create surface (docs/adr/taints.md §6.2/§7):
	// grant-holder-authorized (enforced inside applyScrubRequestGuard via
	// createBead), same OIDC-only RoleOperator baseline as the taint routes
	// above.
	mux.HandleFunc("POST /v1/scrub-requests", requireRole(RoleOperator, s.handleCreateScrubRequest))
	// Formula instantiation (ka-0000000251): server-side pour running the
	// same resolve→expand→create pipeline as `kd formula pour` in one
	// request. Backs the beads-viewer operator-gated Pour button.
	mux.HandleFunc("POST /v1/formulas/{id}/pour", requireRole(RoleOperator, s.handlePourFormula))
	mux.HandleFunc("POST /v1/decisions/{id}/resolve", requireRole(RoleOperator, s.handleResolveDecision))
	mux.HandleFunc("POST /v1/decisions/{id}/cancel", requireRole(RoleOperator, s.handleCancelDecision))
	mux.HandleFunc("POST /v1/escalations/{id}/can-approve", requireRole(RoleViewer, s.handleCanApproveEscalation))
	mux.HandleFunc("POST /v1/escalations/{id}/approve", requireRole(RoleOperator, s.handleApproveEscalation))
	// P7 Task 9 (spec D7): caller classes, not role rank. requireRole still
	// runs first so an OIDC caller below operator is refused the usual way;
	// the D6 approval gate (escalationResultDenial) then runs in the handler.
	mux.HandleFunc("POST /v1/escalations/{id}/reject", requireRole(RoleOperator, s.requireEscalationResolver("reject", s.handleRejectEscalation)))
	mux.HandleFunc("POST /v1/escalations/{id}/result", requireRole(RoleOperator, s.requireEscalationResolver("result", s.handleCaptureEscalationResult)))
	// Claim and hand-up are gated on held_by lane membership in the handler;
	// RoleViewer so a land-owner (project member) is not refused before it.
	mux.HandleFunc("POST /v1/escalations/{id}/claim", requireRole(RoleViewer, s.handleClaimEscalation))
	mux.HandleFunc("POST /v1/escalations/{id}/claim-for", requireRole(RoleViewer, s.handleClaimEscalationFor))
	mux.HandleFunc("POST /v1/escalations/{id}/hand-up", requireRole(RoleViewer, s.handleHandUpEscalation))
	mux.HandleFunc("POST /v1/escalations/{id}/audience", requireRole(RoleOperator, s.handleRouteEscalation))
	// P7.1 backfill: re-target approved escalations a hand-up stranded. The
	// handler also requires an OIDC admin, since requireRole admits agent tokens.
	mux.HandleFunc("POST /v1/escalations/reroute", requireRole(RoleAdmin, s.handleRerouteEscalations))
	// Scrub-request approve/reject (docs/adr/taints.md §6.3, gd-DyWyWFxnzC):
	// same requireRole(RoleOperator) baseline as escalation approve/reject,
	// but the OIDC-only correction is enforced with scrubApprover, NOT
	// escalationApprover — it refuses the X-Operated-By relay path that
	// escalation approve honors (a static/fleet token could otherwise name
	// an arbitrary approver). Four-eyes is scrubFourEyesViolation.
	mux.HandleFunc("POST /v1/scrub-requests/{id}/approve", requireRole(RoleOperator, s.handleApproveScrubRequest))
	mux.HandleFunc("POST /v1/scrub-requests/{id}/reject", requireRole(RoleOperator, s.handleRejectScrubRequest))
	// requireRole bypasses role enforcement entirely for static-token/agent
	// callers (interceptors.go) — only an OIDC human would be role-gated
	// here, and handleAttestScrubRequest itself requires an agent identity
	// (resolveTaintAgentBeadID), so this mirrors approve/reject's registration.
	mux.HandleFunc("POST /v1/scrub-requests/{id}/attest", requireRole(RoleOperator, s.handleAttestScrubRequest))
	mux.HandleFunc("POST /v1/hooks/execute", requireRole(RoleOperator, s.handleExecuteHooks))
	mux.HandleFunc("POST /v1/hooks/emit", requireRole(RoleOperator, s.handleHookEmit))
	mux.HandleFunc("POST /v1/hooks/publish", requireRole(RoleOperator, s.handleHookPublish))
	mux.HandleFunc("GET /v1/events/stream", requireRole(RoleOperator, s.handleEventStream))
	mux.HandleFunc("GET /v1/bus/events", requireRole(RoleOperator, s.handleBusEvents))
	mux.HandleFunc("POST /v1/agents/{id}/gates/{gate_id}/satisfy", requireRole(RoleOperator, s.handleSatisfyGate))
	mux.HandleFunc("DELETE /v1/agents/{id}/gates/{gate_id}", requireRole(RoleOperator, s.handleClearGate))
	mux.HandleFunc("POST /v1/ui/telemetry", requireRole(RoleOperator, s.handleRecordUITelemetry))
	mux.HandleFunc("POST /v1/metrics/slow-tool-calls", requireRole(RoleOperator, s.handleInsertSlowToolCall))

	// --- admin: config changes, account management, destructive operations ---
	// POST /v1/taints is admin-only via role check (not a team check) per
	// ADR docs/adr/taints.md §7 — only an admin may register a new taint.
	mux.HandleFunc("POST /v1/taints", requireRole(RoleAdmin, s.handleCreateTaint))
	mux.HandleFunc("DELETE /v1/beads/{id}", requireRole(RoleAdmin, s.handleDeleteBead))
	mux.HandleFunc("POST /v1/beads/{id}/restore", requireRole(RoleAdmin, s.handleRestoreBead))
	// handleUndoMutation does its own RBAC + audit-on-denial (step 8c) so it
	// is not wrapped in requireRole here.
	mux.HandleFunc("POST /v1/mutations/{id}/undo", s.handleUndoMutation)
	// PUT is requireRoleMachineAllowed (see doc comment there): the
	// beads-viewer e2e seed script is a genuine, CI-deployed non-OIDC caller
	// registering non-builtin custom types (type:widget-instance, type:view)
	// on the primary static token, same as any agent pod. DELETE has no
	// confirmed non-OIDC caller and stays admin-only.
	mux.HandleFunc("PUT /v1/configs/{key...}", requireRoleMachineAllowed(RoleAdmin, s.handleSetConfig))
	mux.HandleFunc("DELETE /v1/configs/{key...}", requireRole(RoleAdmin, s.handleDeleteConfig))
	// Bridge-token routes are canonical (ka-0000000310). The fleet-named
	// routes are kept as dual-accept aliases pointing at the SAME handlers
	// during the compat window — no behavior change, only the API noun is
	// renamed. The fleet aliases are removed in the cleanup bead
	// (ka-0000004906).
	mux.HandleFunc("POST /v1/bridge-tokens", requireRoleMachineAllowed(RoleAdmin, s.handleRegisterBridgeToken))
	mux.HandleFunc("GET /v1/bridge-tokens", requireRoleMachineAllowed(RoleAdmin, s.handleListBridgeTokens))
	mux.HandleFunc("DELETE /v1/bridge-tokens/{bridge}", requireRoleMachineAllowed(RoleAdmin, s.handleRevokeBridgeToken))
	mux.HandleFunc("POST /v1/fleet-tokens", requireRoleMachineAllowed(RoleAdmin, s.handleRegisterBridgeToken))
	mux.HandleFunc("GET /v1/fleet-tokens", requireRoleMachineAllowed(RoleAdmin, s.handleListBridgeTokens))
	mux.HandleFunc("DELETE /v1/fleet-tokens/{fleet}", requireRoleMachineAllowed(RoleAdmin, s.handleRevokeBridgeToken))
	mux.HandleFunc("POST /v1/admin/revoke-user", requireRole(RoleAdmin, s.handleRevokeUser))
	mux.HandleFunc("POST /v1/admin/revoke-token", requireRole(RoleAdmin, s.handleRevokeToken))
	mux.HandleFunc("POST /v1/admin/reextract-references", requireRole(RoleAdmin, s.handleReextractReferences))
	mux.HandleFunc("POST /v1/admin/backfill-search-tsv", requireRole(RoleAdmin, s.handleBackfillSearchTSV))
	mux.HandleFunc("POST /v1/admin/backfill-category", requireRole(RoleAdmin, s.handleBackfillCategory))
	mux.HandleFunc("POST /v1/admin/backfill-project-labels", requireRole(RoleAdmin, s.handleBackfillProjectLabels))
	mux.HandleFunc("POST /v1/admin/backfill-operators", requireRole(RoleAdmin, s.handleBackfillOperators))
	mux.HandleFunc("POST /v1/admin/backfill-operated-by-legacy", requireRole(RoleAdmin, s.handleBackfillOperatedByLegacy))
	mux.HandleFunc("POST /v1/admin/backfill-slack-user-id", requireRole(RoleAdmin, s.handleBackfillSlackUserID))
	mux.HandleFunc("POST /v1/admin/backfill-operator-slugs", requireRole(RoleAdmin, s.handleBackfillOperatorSlugs))
	mux.HandleFunc("POST /v1/admin/backfill-owner", requireRole(RoleAdmin, s.handleBackfillOwner))
	mux.HandleFunc("POST /v1/admin/backfill-empty-owner", requireRole(RoleAdmin, s.handleBackfillEmptyOwner))
	mux.HandleFunc("POST /v1/admin/backfill-operated-by-agents", requireRole(RoleAdmin, s.handleBackfillOperatedByAgents))
	mux.HandleFunc("POST /v1/admin/backfill-parent-child-deps", requireRole(RoleAdmin, s.handleBackfillParentChildDeps))
	mux.HandleFunc("POST /v1/admin/repair-reversed-parent-child-deps", requireRole(RoleAdmin, s.handleRepairReversedParentChildDeps))
	mux.HandleFunc("POST /v1/admin/backfill-triggered-by-deps", requireRole(RoleAdmin, s.handleBackfillTriggeredByDeps))
	mux.HandleFunc("GET /v1/admin/audit-blocks-cycles", requireRole(RoleAdmin, s.handleAuditBlocksCycles))
	mux.HandleFunc("POST /v1/admin/close-trigger-audit-beads", requireRole(RoleAdmin, s.handleCloseTriggerAuditBeads))
	mux.HandleFunc("POST /v1/admin/operators/{id}/forget", requireRole(RoleAdmin, s.handleForgetOperator))
	mux.HandleFunc("GET /v1/operators", requireRole(RoleViewer, s.handleListOperators))
	mux.HandleFunc("GET /v1/operators/stats", requireRole(RoleViewer, s.handleOperatorStats))
	mux.HandleFunc("GET /v1/operators/by-alias", requireRole(RoleViewer, s.handleLookupOperatorByAlias))
	mux.HandleFunc("GET /v1/operator-slugs/{slug}", requireRole(RoleViewer, s.handleLookupOperatorBySlug))
	mux.HandleFunc("GET /v1/operator-slugs/by-id/{id}", requireRole(RoleViewer, s.handleGetOperatorSlug))
	mux.HandleFunc("POST /v1/operators/me/aliases", requireRole(RoleOperator, s.handleLinkOperatorAlias))
	// Slack account linking (gd-OZJ6H2S3xx): the person's own token binds
	// their Slack id to their own user bead; the handler refuses every
	// non-OIDC auth method itself (requireRole lets static tokens through
	// below admin).
	mux.HandleFunc("POST /v1/identity-links/slack", requireRole(RoleViewer, s.handleLinkSlackIdentity))
	mux.HandleFunc("DELETE /v1/identity-links/slack", requireRole(RoleViewer, s.handleUnlinkSlackIdentity))
	// Push devices, per person (Lands 4.7b gd-a7U4AgwgCA): registration needs
	// the person's own OIDC token; list/delete are cut to the caller's own
	// rows unless OIDC admin or machine:gardenio-push-bridge, and refused for
	// static/fleet/per-agent tokens (http_devices.go).
	mux.HandleFunc("POST /v1/devices", requireRole(RoleViewer, s.handleRegisterPushDevice))
	mux.HandleFunc("GET /v1/devices", requireRole(RoleViewer, s.handleListPushDevices))
	mux.HandleFunc("DELETE /v1/devices/{token}", requireRole(RoleViewer, s.handleDeletePushDevice))
	// Lands terms acceptance (step 7.4): the person's own token records
	// terms_version/terms_accepted_at on their own user bead. No role wrapper on
	// purpose: an applicant holds no gardenio role (Track B B-D1). The handler
	// refuses every non-OIDC auth method itself.
	mux.HandleFunc("POST /v1/users/me/terms", s.handleAcceptTerms)
	mux.HandleFunc("GET /v1/users/me/terms", s.handleMyTerms)
	// Standing consent to push CI fixes to the person's MRs (gd-QV2WuUjely):
	// same caller rule as terms, the person's own OIDC token and own bead
	// (user_standing_consent.go).
	mux.HandleFunc("POST /v1/users/me/consent-push", s.handleSetMyConsentPush)
	mux.HandleFunc("GET /v1/users/me/consent-push", s.handleMyConsentPush)
	// Commons read cursors (Commons III step 5): the person's own token reads
	// and moves read_cursors on their own user bead, debounced per person
	// (http_read_cursors.go). No role wrapper: a land member may hold only
	// gardenio-project-member, which requireRole does not rank. The handler
	// refuses non-OIDC callers and applicants (no role, no group) itself.
	mux.HandleFunc("GET /v1/users/me/read-cursors", s.handleGetReadCursors)
	mux.HandleFunc("POST /v1/users/me/read-cursors", s.handleSetReadCursors)
	// The welcome tour's seen flag (step 9.14b): the person's own token reads
	// and stamps welcome_tour_seen_at on their own user bead
	// (http_welcome_tour.go). No role wrapper, as read cursors.
	mux.HandleFunc("GET /v1/users/me/welcome-tour", s.handleMyWelcomeTour)
	mux.HandleFunc("POST /v1/users/me/welcome-tour", s.handleSeenMyWelcomeTour)
	// Who am I, which lands, which tier (action_table.go). No role wrapper for
	// the same reason; the body is only the caller's own token.
	mux.HandleFunc("GET /v1/me", s.handleMe)
	// A land's people, for the Commons' @ list (notification.go). No role
	// wrapper, as above; the handler refuses applicants and other lands.
	mux.HandleFunc("GET /v1/lands/{land}/members", s.handleLandMembers)
	// Lands purge sweep (step 8.11, http_lands_purge.go): the handler admits
	// only an OIDC admin or the lands provisioner's machine principal
	// (capLandPurge); requireRole lets the machine through below admin.
	mux.HandleFunc("POST /v1/lands/purge", requireRole(RoleViewer, s.handleLandPurge))
	// Steward delegation (escalation-routing D8, land_steward_delegation.go):
	// the handler admits only a gardenio-admin or the land's owner
	// (actionStewardDelegate); RoleViewer, as for can-approve, so a scoped
	// land owner reaches it.
	mux.HandleFunc("POST /v1/lands/{land}/steward-delegation", requireRole(RoleViewer, s.handleSetStewardDelegation))
	// Windows (window_routes.go): each handler runs windowManageDenial, so the
	// viewer floor only lets a scoped owner reach it. The view route has no
	// role wrapper: a window principal holds no role, and the handler serves
	// no other caller. Nor has the live stream (window_live.go).
	mux.HandleFunc("POST /v1/windows", requireRole(RoleViewer, s.handleCreateWindow))
	mux.HandleFunc("GET /v1/windows", requireRole(RoleViewer, s.handleListWindows))
	mux.HandleFunc("DELETE /v1/windows/{id}", requireRole(RoleViewer, s.handleRevokeWindow))
	mux.HandleFunc("GET /v1/windows/preview", requireRole(RoleViewer, s.handlePreviewWindow))
	mux.HandleFunc(windowViewPattern, s.handleWindowView)
	mux.HandleFunc(windowLivePattern, s.handleWindowLive)
	// The Lands API (Door D.6, lands_api.go): the contract's /lands/v1/ routes
	// under /v1. No role wrapper: each verb calls the daemon's own routes, whose
	// wrappers and scope gates judge the caller.
	s.registerLandsAPI(mux)
	mux.HandleFunc("GET /v1/operators/{id}/followed-beads", requireRole(RoleViewer, s.handleListFollowedBeadIDs))
	mux.HandleFunc("GET /v1/admin/operators/slack-aliases", requireRoleMachineAllowed(RoleAdmin, s.handleListSlackOperatorAliases))
	mux.HandleFunc("POST /v1/admin/operators/{id}/identity", requireRoleMachineAllowed(RoleAdmin, s.handleUpdateOperatorIdentity))
	// link-aliases / prune-aliases: OIDC gardenio-admin may name aliases and
	// merge; a machine token may only send resync_bead_id (link) or the bare
	// user-bead-bridge source (prune) — the daemon derives the aliases and the
	// keep set from the user beads (gd-uAvleAZUvV). The route wrapper alone would
	// let any static token choose aliases.
	mux.HandleFunc("POST /v1/admin/operators/link-aliases", requireRoleMachineAllowed(RoleAdmin, s.handleAdminLinkOperatorAliases))
	mux.HandleFunc("POST /v1/admin/operators/prune-aliases", requireRoleMachineAllowed(RoleAdmin, s.handleAdminPruneOperatorAliases))
	mux.HandleFunc("POST /v1/embeddings/upsert", requireRole(RoleAdmin, s.handleUpsertEmbedding))
	mux.HandleFunc("POST /v1/embeddings/hashes", requireRole(RoleAdmin, s.handleEmbeddingHashes))
	mux.HandleFunc("POST /v1/embeddings/sync-batch", requireRoleMachineAllowed(RoleAdmin, s.handleSyncBatchEmbeddings))
	mux.HandleFunc("POST /v1/embeddings/migrate", requireRole(RoleAdmin, s.handleMigrateEmbeddings))
	mux.HandleFunc("POST /v1/embeddings/reindex", requireRole(RoleAdmin, s.handleReindexEmbeddings))

	// --- exempt: refresh-token exchange (auth is the refresh-token plaintext) ---
	mux.HandleFunc("POST /v1/auth/refresh", s.handleRefreshToken)

	// --- exempt: health (no RBAC) ---
	// GET /v1/health pings the DB (startupProbe / cold-start endpoints gate).
	// GET /v1/health/live is a cheap process-liveness check with NO DB round-
	// trip (liveness + readiness probes) — see handleHealthLive / ka-0000001764.
	mux.HandleFunc("GET /v1/health", s.handleHealth)
	mux.HandleFunc("GET /v1/health/live", s.handleHealthLive)
}

// wrapHTTPChain wraps the route mux in the timeout, provisioning, rate-limit,
// logging, auth, edge-auth, revocation and version-header middleware.
func (s *BeadsServer) wrapHTTPChain(mux http.Handler, verifier TokenVerifier, fleetCfg []*FleetConfig) http.Handler {
	var fc *FleetConfig
	if len(fleetCfg) > 0 {
		fc = fleetCfg[0]
	}
	// downstream is the middleware chain BELOW auth — timeout, user
	// provisioning, rate limit, logging. EdgeAuthMiddleware-trusted
	// requests skip AuthMiddleware and enter here directly.
	var downstream http.Handler = captureRoute(mux)
	downstream = requestTimeoutMiddleware(downstream)
	// An impersonated session's audit bead is written on its first request,
	// after provisioning has put the tenant's user bead on the context
	// (impersonation.go).
	downstream = s.impersonationAuditMiddleware(downstream)
	// User provisioning runs after auth, auto-creating user beads for
	// OIDC-authenticated users on first request.
	if s.userProvisioner != nil {
		downstream = s.userProvisioner.Middleware(downstream)
	}
	// The land door check runs after auth on every path (AuthMiddleware and
	// EdgeAuthMiddleware) and before provisioning: a request the edge or a
	// landgate marked as /land/<name>/ must carry a token for that land
	// (land_audience.go).
	// A land agent's door traffic is its presence (land_door_presence.go):
	// inside the door check, so only an admitted token is recorded.
	downstream = s.landDoorPresenceMiddleware(downstream)
	downstream = s.landDoorMiddleware(downstream)
	// A presence-tracked coordinator client's requests staff its lane
	// (escalation_lane_presence.go).
	downstream = s.coordinatorPresenceMiddleware(downstream)
	// Rate limiting sits INSIDE loggingMiddleware so 429s still land in the
	// access log, and OUTSIDE auth so the identity is already attached to
	// the request context when the limiter keys its buckets.
	rateCfg := DefaultRateLimitConfig()
	if s.rateLimitCfg != nil {
		rateCfg = *s.rateLimitCfg
	}
	downstream = RateLimitMiddleware(rateCfg, s.trustedProxies, downstream)
	downstream = loggingMiddleware(s.trustedProxies, downstream)

	// authChain wraps downstream with AuthMiddleware — the legacy path for
	// requests without an edge-auth envelope (fleet token / OIDC / machine).
	authChain := windowAuthMiddleware(s, s.forwardAuthClients, fc, verifier, downstream)

	// When edge-auth is configured, install EdgeAuthMiddleware at the top.
	// Verified envelopes go straight to downstream (skipping AuthMiddleware);
	// missing envelopes fall through to authChain (legacy path).
	var top http.Handler = authChain
	if s.edgeAuthCfg != nil && (s.edgeAuthCfg.Enabled() || s.edgeAuthCfg.Enforce) {
		top = EdgeAuthMiddleware(*s.edgeAuthCfg, downstream, authChain)
	}

	// Runner bearers ("gr_...") are resolved here, ahead of the edge envelope
	// and AuthMiddleware, and go straight to downstream (runner_auth.go).
	top = s.runnerAuthMiddleware(fc, downstream, top)

	// versionHeaderMiddleware sits outermost so EVERY response — including
	// auth failures, rate-limit 429s, and edge-auth rejects — carries the
	// daemon's build version. That is what lets a stale kd/gb detect version
	// skew from any call it happens to make (ka-0000002965).
	//
	// landmap.Guard sits outside even that, and only on Lands API paths: a
	// tenant is answered problem+json with the contract's headers whoever
	// refused the request, so the version headers, a 401's or 429's text and
	// a mux 404 never reach one (D.8 gd-enMSApFne1).
	return landmap.Guard(versionHeaderMiddleware(TokenRevocationMiddleware(s.revocation, top)))
}

// versionHeaderMiddleware stamps the daemon's build version + commit on every
// response so clients can detect version skew (ka-0000002965). The values come
// from the buildinfo package, which is the same source `kd version` reads —
// and since the daemon IS `kd serve`, they are always in sync. Setting the
// header before ServeHTTP guarantees it survives even handlers that call
// WriteHeader early.
func versionHeaderMiddleware(next http.Handler) http.Handler {
	info := buildinfo.Get()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Kbeads-Version", info.Version)
		w.Header().Set("X-Kbeads-Commit", info.Commit)
		next.ServeHTTP(w, r)
	})
}

// handleHealth handles GET /v1/health. It pings the database to verify
// connectivity and returns 503 if the DB is unreachable.
//
// This DB-backed check backs the beads pod's startupProbe (which gates the pod
// into the Service EndpointSlice only once the DB is reachable at startup — the
// cold-start endpoints gate, ka-0000005545). It is deliberately NOT wired to
// readiness/liveness — see handleHealthLive for why.
func (s *BeadsServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	if err := s.store.Ping(ctx); err != nil {
		slog.Warn("health check failed", "error", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status": "error",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleHealthLive handles GET /v1/health/live. It is a CHEAP process-liveness
// check that does NO database round-trip: if the HTTP server can accept and
// answer the request, the process is alive. This backs the beads pod's
// readinessProbe AND livenessProbe (ka-0000001764).
//
// WHY it exists, separate from /v1/health: beads runs a SINGLE replica in every
// overlay, and /v1/health pings the DB (5s timeout). Gating readiness on a DB
// ping for a single-replica Service is an anti-pattern — a slow cold-start ping
// trips the probe, K8s pulls the pod's ONLY address out of the Service
// EndpointSlice, and an in-flight kd call mid-request gets HTTP 000 (the
// family-B E2E flake, epic ka-0000007460). With no second replica to route
// around, evicting the sole endpoint converts a slow response into a hard
// failure. Wiring the same DB ping to liveness is worse still: a transient DB
// blip restarts an otherwise-healthy daemon.
//
// The DB-connectivity gate is preserved on the startupProbe (still GET
// /v1/health), so a genuinely wedged daemon is still held out of the
// EndpointSlice at startup. After startup, readiness/liveness track only
// process health — which is exactly what a single replica can act on.
func (s *BeadsServer) handleHealthLive(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// isProbeHealthPath reports whether the request targets one of the
// unauthenticated health endpoints the kubelet probes: GET /v1/health (DB
// ping, used by the startupProbe) and GET /v1/health/live (cheap process
// liveness, used by the liveness + readiness probes, ka-0000001764). Both must
// bypass auth, token-revocation, edge-auth, and rate-limiting so a probe never
// needs to carry a Bearer token. GET /v1/health/snapshot is deliberately NOT
// included — it is an authenticated viewer-role endpoint.
func isProbeHealthPath(method, path string) bool {
	return method == http.MethodGet &&
		(path == "/v1/health" || path == "/v1/health/live")
}

// mergeBeadFields merges extra fields into an existing bead's fields JSON via
// a single atomic UPDATE (store.Store.MergeBeadFields) — not a Get + mutate +
// UpdateBead sequence, which would race a concurrent writer to this same bead
// (gd-P6uCMzQqU9: a stale read-modify-write here could silently clobber a
// claim flag another concurrent request just set atomically elsewhere).
//
// It is also a chokepoint for the type:user identity gate: when extra names an
// identity field (userIdentityFields) the target bead is fetched and, if it is
// a user bead, the write is refused for a non-OIDC caller. The common case
// (no identity key) costs nothing extra. Every current caller writes a fixed
// set of lifecycle keys; this keeps a future one from becoming a bypass.
// A runner caller's runtime_id write is gated here too (runnerRuntimeIDDenial),
// since runnerStreamDenial trusts the stored value.
//
// impersonated_by and created_impersonated_by are refused here for every
// caller, so no extra-field merge (an escalation result's fields, a close body)
// can forge them: the one merge that writes impersonated_by is
// stampImpersonatedByOn's own (impersonation.go).
func (s *BeadsServer) mergeBeadFields(ctx context.Context, id string, extra map[string]any) error {
	if !isImpersonationStamp(ctx) {
		for k := range extra {
			if !isImpersonationField(k) {
				continue
			}
			denial := impersonatedByDeny("update", "")
			if existing, err := s.store.GetBead(ctx, id); err == nil && existing != nil {
				denial.BeadType = string(existing.Type)
			}
			return denial
		}
	}
	var identityKeys []string
	for k := range extra {
		if isGatedUserField(k) || podIdentityKeys[k] || channelBindingFields[k] || landSwitchFields[k] || wishHeadFields[k] || k == idempotencyKeyField || isLandMintKey(k) || k == agentRoleField || k == spawnLineageField || isAgentParkField(k) {
			identityKeys = append(identityKeys, k)
		}
	}
	runnerRuntimeID := authMethodFromContext(ctx) == authMethodRunner && hasRuntimeIDKey(extra)
	if len(identityKeys) > 0 || runnerRuntimeID {
		existing, err := s.store.GetBead(ctx, id)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("merge bead fields: fetch %s: %w", id, err)
		}
		if runnerRuntimeID {
			if existing == nil {
				return fmt.Errorf("merge bead fields %s: not found", id)
			}
			if denial := runnerRuntimeIDFieldsDenial(ctx, "update", extra, existing); denial != nil {
				return denial
			}
		}
		if denial := s.authorizeUserBeadFieldWrite(ctx, "update", existing, identityKeys...); denial != nil {
			return denial
		}
		if denial := s.authorizeAgentBeadFieldWrite(ctx, "update", existing, identityKeys...); denial != nil {
			return denial
		}
		if err := roleTierFieldEndpointError(existing, identityKeys...); err != nil {
			return err
		}
	}
	bead, err := s.store.MergeBeadFields(ctx, id, extra)
	if err != nil {
		return fmt.Errorf("merge bead fields: %w", err)
	}
	if bead == nil {
		return fmt.Errorf("merge bead fields %s: not found", id)
	}
	return nil
}

// writeJSON writes a JSON response with the given status code.
func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

// writeError writes a JSON error response.
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// writeErrorCode writes a JSON error response that also carries a stable,
// machine-readable `code` field alongside the human-readable message — the same
// shape writeOperatorRefusal uses so a client can branch on the condition
// without keying on the (unstable) message string. Used for the 503 responses
// writeServerError distinguishes below.
func writeErrorCode(w http.ResponseWriter, status int, message, code string) {
	writeJSON(w, status, map[string]string{"error": message, "code": code})
}

// queryCanceledErrorCode is the stable `code` value stamped on the 503 body when
// a request failed because its statement was cancelled by the 30s deadline
// (SQLSTATE 57014). The kd client keys on this code to NOT retry the request:
// unlike the 28P01 stale-password 503 (where a re-send lands on a freshly-synced
// connection and recovers a would-be-dropped write), a 57014 means the query is
// deterministically too slow for the deadline, so re-running it just burns
// another full deadline and turns one 30s failure into a multi-retry ~90s hang
// (bug ka-0000000979). The Retry-After hint is retained so the 503 still reads
// as "transient, not a hard fault" to proxies and non-kd clients.
const queryCanceledErrorCode = "query_canceled"

// writeOperatorRefusal writes the structured HTTP 400 body returned when
// resolveAuditOperatorRequired fails closed. The body carries a stable
// machine-readable `code` field (operatorRefusalErrorCode) plus the
// classifyOperatorRefusal source label so the kd CLI can map the response
// to the EX_USAGE-family sentinel exit code 64 without keying on the
// human-readable message string. See ka-0000006731.
func writeOperatorRefusal(w http.ResponseWriter, source string) {
	if source == "" {
		source = refusalSourceUnknown
	}
	writeJSON(w, http.StatusBadRequest, map[string]string{
		"error":           operatorRefusalMessage,
		"code":            operatorRefusalErrorCode,
		"resolver_source": source,
	})
}

// dbAuthErrorRetryAfterSeconds is the Retry-After hint (seconds) sent with a
// 503 when a request failed on a PostgreSQL auth error (28P01). It is sized to
// the ExternalSecret refresh interval that bounds the stale-password window
// (category rotated-db = 5m in prod): a client that waits this long is very
// likely to hit a freshly-synced password. A hint, not a guarantee — clients
// bound their own retry budget.
const dbAuthErrorRetryAfterSeconds = "5"

// queryCanceledRetryAfterSeconds is the Retry-After hint (seconds) sent with a
// 503 when a request failed because a statement was cancelled by the 30s
// request-context deadline (SQLSTATE 57014). Unlike the 28P01 case, waiting
// does not itself clear the condition — the underlying query is slow (bug
// ka-0000003621) — so this is a short backoff that lets a client retry (or a
// human narrow the query) rather than treat a slow read as a hard fault. Kept
// small (2s) because a retryable-but-slow read should not stall an interactive
// caller for long; clients bound their own retry budget.
const queryCanceledRetryAfterSeconds = "2"

// writeServerError records the wrapped error via AttachServerError (so the
// logging middleware can emit the 5xx root-cause WARN with the full chain,
// handler name, and request_id) and writes a sanitized error response to the
// client. message is the operator-facing summary that appears in the JSON body;
// the error itself is only logged, never returned to the client.
//
// A PostgreSQL authentication failure (SQLSTATE 28P01) is handled specially:
//   - it increments gardenio_beads_db_auth_errors_total{handler} so a sustained DB-auth
//     fault PAGES (bug ka-0000006554: the ~4-min fleet-wide blackout that was
//     completely un-alerted because no such metric existed), and
//   - it downgrades the response from 500 to 503 + Retry-After, because a 28P01
//     on this deployment is TRANSIENT (a mounted-secret-file password lagging an
//     RDS rotation by up to one ExternalSecret refresh). A 500 tells a caller
//     "your mutation failed permanently, give up"; a 503 + Retry-After tells it
//     "retry shortly" — which is what stops a create/update/close from being
//     silently dropped during the stale-password window (durability, task 3).
//
// The gardenio_beads_db_auth_errors_total counter is NOT incremented here — it is
// incremented once per 5xx in loggingMiddleware (the single choke point that
// inspects every 5xx's captured error), so the metric stays complete even for
// any future 28P01 path that does not route through this helper.
func writeServerError(ctx context.Context, w http.ResponseWriter, handler string, err error, message string) {
	AttachServerError(ctx, handler, err)
	if isDBAuthError(err) {
		w.Header().Set("Retry-After", dbAuthErrorRetryAfterSeconds)
		writeError(w, http.StatusServiceUnavailable, message)
		return
	}
	// A statement cancelled by the 30s request-context deadline (SQLSTATE
	// 57014) is downgraded from a bare, permanent-looking 500 to a 503 so the
	// caller sees "transient, not a hard fault" — most notably a slow
	// multi-term `kd search` seq scan (bug ka-0000003621 / design ka-0000004060).
	// It carries queryCanceledErrorCode so the kd client does NOT auto-retry it:
	// the query is deterministically too slow for the deadline, so re-running it
	// just burns another full 30s and stacks into a ~90s hang for an
	// ultra-common single search term (bug ka-0000000979). This differs from the
	// 28P01 branch above, where a re-send genuinely recovers a would-be-dropped
	// write. Safety net only — the real fix is making the query fast
	// (ka-0000006300 Phases 1/2).
	if isQueryCanceledError(err) {
		w.Header().Set("Retry-After", queryCanceledRetryAfterSeconds)
		writeErrorCode(w, http.StatusServiceUnavailable, message, queryCanceledErrorCode)
		return
	}
	writeError(w, http.StatusInternalServerError, message)
}

// statusRecorder wraps http.ResponseWriter to capture the status code and the
// response body size. It also implements http.Flusher so SSE streaming
// endpoints continue to work.
type statusRecorder struct {
	http.ResponseWriter
	statusCode int
	bytes      int64
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

func (r *statusRecorder) WriteHeader(code int) {
	r.statusCode = code
	r.ResponseWriter.WriteHeader(code)
}

// Flush implements http.Flusher by delegating to the underlying ResponseWriter.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// errorCapture is carried on the request context so handlers can report the
// underlying cause of a 5xx response to loggingMiddleware. The middleware then
// emits a single WARN-level log alongside the access-log entry with the
// wrapped error chain, handler name, and request/trace IDs. This is what turns
// a bare "GET /v1/beads -> 500" access-log line into an actionable signal.
type errorCapture struct {
	handler string
	err     error
}

type errorCaptureKeyType struct{}

var errorCaptureKey errorCaptureKeyType

// AttachServerError records the wrapped error that caused (or will cause) a
// 5xx response on the current request. The logging middleware reads this via
// context to emit a WARN with the wrapped error chain when the response
// status is >= 500. Safe to call with a context that was not initialised by
// loggingMiddleware — in that case it is a no-op.
func AttachServerError(ctx context.Context, handler string, err error) {
	if err == nil {
		return
	}
	if cap, ok := ctx.Value(errorCaptureKey).(*errorCapture); ok && cap != nil {
		cap.handler = handler
		cap.err = err
	}
}

// authRedactRe matches Authorization header fragments and bare Bearer tokens
// that may appear in wrapped error chains from upstream HTTP clients. The
// Bearer branch is intentionally greedy past optional separators ("=", ":")
// so forms like "Authorization=Bearer xyz" redact the token as well as the
// header name.
var authRedactRe = regexp.MustCompile(`(?i)(authorization\s*[:=]?\s*bearer\s+\S+|authorization\s*[:=]\s*\S+|bearer\s+\S+)`)

// sanitizeErrorString strips values that look like bearer tokens or
// Authorization header contents so they never appear in error logs.
func sanitizeErrorString(s string) string {
	if s == "" {
		return s
	}
	return authRedactRe.ReplaceAllString(s, "[REDACTED]")
}

// requestID returns a short request/trace identifier from common tracing
// headers, or the empty string if none is present. Prefers W3C traceparent,
// then X-Request-Id, then X-Trace-Id.
func requestID(r *http.Request) string {
	if tp := r.Header.Get("Traceparent"); tp != "" {
		// traceparent: 00-<trace-id>-<span-id>-<flags>
		parts := strings.SplitN(tp, "-", 4)
		if len(parts) >= 2 && parts[1] != "" {
			return parts[1]
		}
		return tp
	}
	if id := r.Header.Get("X-Request-Id"); id != "" {
		return id
	}
	if id := r.Header.Get("X-Trace-Id"); id != "" {
		return id
	}
	return ""
}

// Caps for the caller-attribution log attributes.
const (
	attrQueryMax = 300
	attrActorMax = 64
	attrAgentMax = 64
)

func capAttr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// requestAttributionAttrs returns the query (list reads only), actor and
// user_agent log attributes for r; empty values are omitted.
func requestAttributionAttrs(r *http.Request) []slog.Attr {
	var attrs []slog.Attr
	if r.Method == http.MethodGet && r.URL.Path == "/v1/beads" && r.URL.RawQuery != "" {
		attrs = append(attrs, slog.String("query", capAttr(r.URL.RawQuery, attrQueryMax)))
	}
	if a := r.Header.Get("X-Actor"); a != "" {
		attrs = append(attrs, slog.String("actor", capAttr(a, attrActorMax)))
	}
	if ua := r.UserAgent(); ua != "" {
		attrs = append(attrs, slog.String("user_agent", capAttr(ua, attrAgentMax)))
	}
	return attrs
}

// loggingMiddleware logs HTTP requests with method, path, status, duration,
// client IP, authenticated user identity, and fleet. Requests taking > 10s
// are logged at WARN, > 1s at INFO, others at DEBUG.
// The trusted proxy config determines whether X-Forwarded-For / X-Real-Ip
// headers are trusted based on the direct peer address.
func loggingMiddleware(tp *TrustedProxyConfig, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		// Sits below every auth path, so the identity is already attached and
		// each authenticated request is counted exactly once (gd-M4pFzv6bUq).
		recordRequestByAuthType(AuthIdentityFromContext(r.Context()).Type, httpMethodIsWrite(r.Method))
		rec := &statusRecorder{ResponseWriter: w, statusCode: http.StatusOK}
		cap := &errorCapture{}
		ctx := context.WithValue(r.Context(), errorCaptureKey, cap)
		ctx, rc := withRouteCapture(ctx)
		next.ServeHTTP(rec, r.WithContext(ctx))
		duration := time.Since(start)

		route := rc.pattern
		if route == "" {
			route = "unmatched"
		}
		caller := responseCaller(AuthIdentityFromContext(r.Context()))
		responseBytesTotal.WithLabelValues(caller, route).Add(float64(rec.bytes))
		if !isStreamingPath(r.URL.Path) {
			requestDuration.WithLabelValues(caller, route).Observe(duration.Seconds())
		}

		attrs := []slog.Attr{
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", rec.statusCode),
			slog.Duration("duration", duration),
			slog.String("remote_ip", clientIP(r, tp)),
			slog.Int64("bytes", rec.bytes),
		}

		// Append auth identity from context (set by AuthMiddleware).
		id := AuthIdentityFromContext(r.Context())
		attrs = append(attrs, slog.String("auth_type", id.Type))
		if id.Subject != "" {
			attrs = append(attrs, slog.String("auth_subject", id.Subject))
		}

		// Append fleet if non-default.
		if fleet := FleetFromContext(r.Context()); fleet != "default" {
			attrs = append(attrs, slog.String("fleet", fleet))
		}

		// Caller attribution (gd-g25jihMHFs): a shared static token makes
		// auth_subject identical for every in-cluster caller, so the filter
		// shape (query), the X-Actor name and the client's User-Agent are the
		// only way to tell WHICH loop is hammering GET /v1/beads. All capped;
		// query only for list reads, where it carries filters, not secrets.
		attrs = append(attrs, requestAttributionAttrs(r)...)

		switch {
		case duration >= 10*time.Second:
			slog.LogAttrs(r.Context(), slog.LevelWarn, "http request (slow)", attrs...)
		case duration >= 1*time.Second:
			slog.LogAttrs(r.Context(), slog.LevelInfo, "http request", attrs...)
		default:
			slog.LogAttrs(r.Context(), slog.LevelDebug, "http request", attrs...)
		}

		// On 5xx, emit a single WARN with the wrapped root cause (sanitized)
		// alongside the access-log entry so operators can escalate without
		// having to spelunk end-to-end. See ka-0000002244.
		if rec.statusCode >= 500 {
			errAttrs := []slog.Attr{
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", rec.statusCode),
			}
			if cap.handler != "" {
				errAttrs = append(errAttrs, slog.String("handler", cap.handler))
			}
			if reqID := requestID(r); reqID != "" {
				errAttrs = append(errAttrs, slog.String("request_id", reqID))
			}
			if cap.err != nil {
				errAttrs = append(errAttrs, slog.String("error", sanitizeErrorString(cap.err.Error())))
			} else {
				errAttrs = append(errAttrs, slog.String("error", "unknown (handler did not call AttachServerError)"))
			}
			slog.LogAttrs(r.Context(), slog.LevelWarn, "http 5xx root cause", errAttrs...)

			// Count DB-auth failures (28P01) so a sustained rate PAGES. This is
			// the metric that was missing during bug ka-0000006554's ~4-min
			// fleet-wide blackout — the daemon lost RDS auth, 500'd 957 requests,
			// and NOTHING alerted because no request-status or DB-error series
			// existed. Counting in the 5xx choke point (rather than only in
			// writeServerError) keeps the series complete for any 28P01 path,
			// including the gRPC-wrapped ListBeads case. handler is the
			// AttachServerError label, or "unknown" when it was not set.
			if isDBAuthError(cap.err) {
				handler := cap.handler
				if handler == "" {
					handler = "unknown"
				}
				dbAuthErrorsTotal.WithLabelValues(handler).Inc()
			}

			// Count statement cancellations (57014) — the 30s request deadline
			// firing on a slow query (bug ka-0000003621). Same choke point, same
			// "unknown" handler fallback. Note the response for this class is a
			// 503 (see writeServerError), so this branch also catches the
			// reclassified case; rec.statusCode >= 500 gates both 500 and 503.
			if isQueryCanceledError(cap.err) {
				handler := cap.handler
				if handler == "" {
					handler = "unknown"
				}
				queryCanceledErrorsTotal.WithLabelValues(handler).Inc()
			}
		}
	})
}

// clientIP extracts the client IP from the request. When the direct peer
// (RemoteAddr) is a trusted proxy, X-Forwarded-For and X-Real-Ip headers
// are used to determine the real client IP. When the peer is untrusted (or
// no TrustedProxyConfig is provided with configured CIDRs), the function
// falls back to RemoteAddr directly.
//
// A nil TrustedProxyConfig trusts all peers for backwards compatibility.
func clientIP(r *http.Request, tp *TrustedProxyConfig) string {
	if tp.IsTrusted(r.RemoteAddr) {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			// X-Forwarded-For may contain multiple IPs; the first is the client.
			if i := strings.IndexByte(xff, ','); i > 0 {
				return strings.TrimSpace(xff[:i])
			}
			return strings.TrimSpace(xff)
		}
		if xri := r.Header.Get("X-Real-Ip"); xri != "" {
			return xri
		}
	}
	// RemoteAddr is "host:port"; strip the port.
	addr := r.RemoteAddr
	if i := strings.LastIndex(addr, ":"); i > 0 {
		return addr[:i]
	}
	return addr
}

// streamingPaths is the closed allowlist of SSE endpoints that hold open
// long-lived connections and must bypass the 30s request timeout and the
// per-identity rate limiter. Add new SSE handlers here explicitly — the
// previous substring match on "events"/"stream" accidentally exempted
// snapshot endpoints like /v1/events and /v1/beads/{id}/events from both safeguards (ka-0000002324).
var streamingPaths = map[string]struct{}{
	"/v1/events/stream": {}, // bead lifecycle SSE (handleEventStream)
	"/v1/bus/events":    {}, // JetStream bus SSE (handleBusEvents)
	windowLivePath:      {}, // the Window's live view SSE (handleWindowLive); capped per window, ends after maxLifetime
}

// isStreamingPath returns true for SSE/streaming endpoints that need
// long-lived connections and should not have a request context deadline.
func isStreamingPath(path string) bool {
	_, ok := streamingPaths[path]
	return ok
}

// longRunningPaths is the closed allowlist of admin endpoints that drive
// whole-table backfills inline and routinely exceed the default 30s request
// timeout on prod-sized data (ka-0000001168 — the operated_by backfill took
// ~5 minutes; UPDATE deps alone was 3m37s). These paths still go through
// the rate limiter and RBAC; they only bypass the per-request context
// deadline and the per-response write deadline. Add new long-running admin
// handlers here explicitly — keep this list narrow.
var longRunningPaths = map[string]struct{}{
	"/v1/admin/backfill-operators":                {}, // handleBackfillOperators (full backfill + verifier inline)
	"/v1/admin/backfill-operated-by-legacy":       {}, // handleBackfillOperatedByLegacy (multi-pass legacy-operated-by cleanup)
	"/v1/admin/backfill-slack-user-id":            {}, // handleBackfillSlackUserID (ka-0000001582; thread-derived slack_user_id backfill)
	"/v1/admin/backfill-operator-slugs":           {}, // handleBackfillOperatorSlugs (ka-0000001149; op-<id> → readable slug backfill)
	"/v1/admin/backfill-owner":                    {}, // handleBackfillOwner (ka-0000006961; Matt-default owner rebind)
	"/v1/admin/backfill-empty-owner":              {}, // handleBackfillEmptyOwner (ka-0000007029; three-phase empty-owner backfill)
	"/v1/admin/backfill-operated-by-agents":       {}, // handleBackfillOperatedByAgents (ka-0000006817 #1; per-user-homepage M2 agent operated_by_id backfill)
	"/v1/admin/backfill-parent-child-deps":        {}, // handleBackfillParentChildDeps (ka-0000006775; agent-orphan parent-child reconstruction)
	"/v1/admin/repair-reversed-parent-child-deps": {}, // handleRepairReversedParentChildDeps (ka-0000006880; reversed epic->child edge flip)
	"/v1/admin/backfill-triggered-by-deps":        {}, // handleBackfillTriggeredByDeps (ka-0000000004; M4 auto-link backfill scanner)
	"/v1/admin/close-trigger-audit-beads":         {}, // handleCloseTriggerAuditBeads (ka-0000006341; legacy trigger-audit close sweep)
	"/v1/admin/audit-blocks-cycles":               {}, // handleAuditBlocksCycles (ka-0000007984; whole-graph blocks-cycle SCC scan)
}

// isLongRunningPath returns true for admin endpoints whose handlers are
// expected to run for many minutes against prod-sized data and must not
// be capped by the standard 30s request-context deadline.
func isLongRunningPath(path string) bool {
	_, ok := longRunningPaths[path]
	return ok
}

// requestTimeout is the per-request context deadline requestTimeoutMiddleware
// sets; a test shrinks it to prove a streaming path outlives it.
var requestTimeout = 30 * time.Second

// requestTimeoutMiddleware wraps each non-streaming request's context with a
// 30s deadline (requestTimeout) and sets a per-response write deadline to prevent any single
// request from holding resources forever. Streaming paths (SSE) and the
// long-running admin allowlist are excluded so that long-lived connections
// and inline whole-table backfills are not killed by server-level timeouts.
func requestTimeoutMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isStreamingPath(r.URL.Path) || isLongRunningPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
		defer cancel()
		// Set a per-response write deadline so that the global
		// http.Server.WriteTimeout can remain at 0 (required for SSE).
		rc := http.NewResponseController(w)
		_ = rc.SetWriteDeadline(time.Now().Add(60 * time.Second))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
