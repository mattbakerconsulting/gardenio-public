package main

// Gardener Page operability layer (task ka-0000003713, epic ka-0000001553,
// CDQR Operability dimension ka-0000005307).
//
// The /gardener/{id} page used to be read-only: it rendered the goal, counters,
// stop/progress JSON and the life-thread, but exposed ZERO operate-the-gardener
// controls. This file closes the read → write → config loop with operator-gated
// routes mounted under /gardener/{id}/...:
//
//   Lifecycle (write):
//     POST /gardener/{id}/enable      — flip enabled=true
//     POST /gardener/{id}/disable     — flip enabled=false
//     POST /gardener/{id}/run-now     — spawn one iteration immediately
//     POST /gardener/{id}/cancel-run  — close the in-flight iteration
//     POST /gardener/{id}/retire      — close the gardener trigger bead
//
//   Config (edit):
//     GET  /gardener/{id}/edit        — inline config form (operator-gated)
//     POST /gardener/{id}/config      — persist edited GardenerSpec knobs
//
// Every mutation is gated behind operatorOnly at the route layer (see
// server.go's registration block) — read-scoped viewers see state but no levers.
// Config persists back to the gardener trigger bead's `gardener` JSON field via
// the SAME bridge.GardenerSpec encode path the engine + create-gardener use, so
// the page never drifts from the spec the engine reads.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
	"gardenio/controller/internal/gardenermode"
	"gardenio/controller/internal/oidcauth"
	"gardenio/controller/internal/scheduler"
	"gardenio/controller/internal/scheduler/conditions"
	"gardenio/controller/internal/scheduler/progress"
)

// gardenerControlsView is the data passed to the operator controls partial —
// the lifecycle lever bar (enable/disable · run-now · cancel-run · retire) that
// renders at the top of /gardener/{id} for operators. It is rebuilt and swapped
// in place after every lifecycle mutation so the bar reflects current state.
type gardenerControlsView struct {
	ID       string
	Enabled  bool
	IsClosed bool
	// HasInflightRun is true when the gardener's LastAgentID points at an
	// open/in_progress iteration — gates the cancel-run lever.
	HasInflightRun bool
	// LastAgentID is the in-flight iteration the cancel-run lever targets
	// (empty when there is nothing to cancel).
	LastAgentID string
	// Mode is the gardener's firing mode for the timed⇄event toggle: "timed"
	// (trigger:schedule + cron) or "event" (trigger:multi + subtree events).
	// Empty when the bead is in an ambiguous label state — the toggle renders
	// disabled with a tooltip rather than guessing (task ka-0000006866).
	Mode string
	// ModeAmbiguous is true when Detect could not resolve a single mode (both
	// or neither trigger label present). The toggle renders inert.
	ModeAmbiguous bool
}

// gardenerControlsViewFromBead builds the lifecycle control-bar view from a
// gardener bead. The in-flight check is a best-effort surface hint: the
// authoritative one_spawn / cancel guards live in the handlers, which re-read
// the agent's live status before acting.
func gardenerControlsViewFromBead(b *beadsapi.BeadDetail) gardenerControlsView {
	v := gardenerControlsView{
		ID:       b.ID,
		Enabled:  b.Fields["enabled"] != "false",
		IsClosed: statusOpenSortKey(b.Status) != 0,
	}
	def := bridge.ParseTriggerDef(b, "schedule")
	if def.Gardener != nil && def.Gardener.LastAgentID != "" {
		v.LastAgentID = def.Gardener.LastAgentID
		v.HasInflightRun = true
	}
	// Firing mode for the timed⇄event toggle. An ambiguous label state (both or
	// neither trigger label) leaves Mode empty + ModeAmbiguous set so the bar
	// renders an inert control instead of a lever that would 409 on POST.
	if mode, err := gardenermode.Detect(b); err != nil {
		v.ModeAmbiguous = true
	} else {
		v.Mode = string(mode)
	}
	return v
}

// gardenerConfigView is the data passed to the inline config-edit form. It
// carries the editable GardenerSpec knobs plus the gardener identity so the form
// can post back and re-render in place. Draft + Error are populated when a
// POST /config round-trips back to the form on a validation failure, so the
// operator does not lose their input.
type gardenerConfigView struct {
	ID    string
	Title string
	Name  string

	// Prompt is the gardener's per-run `prompt` field — the operative instruction
	// sent verbatim to every run the gardener spawns, and the single most
	// behavior-defining knob (bug ka-0000003596). It is a TOP-LEVEL trigger-bead
	// field (like cron/conditions below), NOT part of the gardener blob, so the
	// save path writes it via setSchedField. It was the one knob the config
	// editor could not view or edit; the form now exposes it as a textarea.
	Prompt string

	// Editable governing knobs (GardenerSpec). Stored as strings so the form
	// re-renders the operator's raw input verbatim on a validation error.
	TerminalGoal    string
	ExitCondition   string
	OnExit          string
	RunBudget       string
	MaxRuns         string
	RetryOnFailure  bool
	MaxRetries      string
	RunTimeout      string
	Timeout         string
	OneSpawnAtATime bool
	StopJSON        string
	// ProgressJSON is the gardener-spec `progress` block (events/windows/surface)
	// rendered as a raw-JSON textarea, mirroring StopJSON. It lives INSIDE the
	// `gardener` blob (GardenerSpec.Progress), unlike the four scheduling knobs
	// below, which are top-level trigger-bead fields.
	ProgressJSON string

	// --- Top-level trigger-bead scheduling knobs (NOT part of the gardener
	// blob). These govern WHEN/HOW OFTEN the gardener fires and how many
	// concurrent evaluations/pre-flight gates apply — the create form (I2.1)
	// exposes them, so the edit form must reach parity or a save silently
	// flattens them. Persisted as top-level bead fields, not inside `gardener`. ---
	Conditions    string // conditions[] pre-flight gate (JSON array)
	Cron          string // 5-field cron cadence (empty = default */30)
	Cooldown      string // cooldown_minutes between fires
	MaxConcurrent string // max_concurrent per-cycle fire cap

	// StopBuilder is the schema-driven guided-row view of the stop spec
	// (task ka-0000000897 / I2.3). It is the primary authoring surface; the
	// raw StopJSON textarea remains as an advanced escape hatch. Populated
	// from the current spec on load and re-derived from the submitted rows on
	// save so a validation error re-renders the operator's exact input.
	StopBuilder stopSpecBuilderView

	// Error, when non-empty, is rendered as an inline alert above the form.
	Error string
	// Saved flips true after a successful POST /config so the partial can
	// render a "saved" confirmation in place of the form.
	Saved bool
}

// loadGardenerTrigger loads a gardener trigger bead and validates it is a real
// gardener target (a type=trigger bead carrying role:gardener OR a legacy
// type=schedule bead with a gardener spec). Returns an HTTP-mapped error via the
// supplied writer and (nil, false) when the bead is missing / not a gardener;
// callers should return immediately when ok is false.
func (s *Server) loadGardenerTrigger(w http.ResponseWriter, r *http.Request, id string) (*beadsapi.BeadDetail, bool) {
	bead, err := s.daemon.GetBead(r.Context(), id)
	if err != nil {
		var apiErr *beadsapi.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			http.Error(w, "Gardener not found", http.StatusNotFound)
			return nil, false
		}
		s.logger.Error("gardener ops: get bead", "id", id, "error", err)
		http.Error(w, "Failed to load gardener", http.StatusInternalServerError)
		return nil, false
	}
	if bead == nil {
		http.Error(w, "Gardener not found", http.StatusNotFound)
		return nil, false
	}
	if !isGardenerBead(bead) {
		http.Error(w, "Not a gardener bead", http.StatusBadRequest)
		return nil, false
	}
	return bead, true
}

// auditProbeHeader is the request header an operability audit sets to declare
// "this is a route-existence probe, not an operate-the-gardener action". When a
// mutation handler sees it AFTER its auth gates have passed (operatorOnly route
// + email != ""), the handler returns the would-succeed status (200) WITHOUT
// performing any state change. Any non-empty value opts in.
//
// This is defense-in-depth for bug ka-0000007079: the CDQR operability probe
// curls every gardener lifecycle write route (enable/disable/run-now/cancel-run/
// retire/config) to record which are wired. A 401 already proves a route is
// implemented+gated, so the probe is meant to be VIEWER-scoped — but if operator
// identity ever leaks into the probe (as it did in the 2026-08-25T07:47:00Z
// iteration, which DISABLED + RETIRED a live prod gardener), the bare POST would
// otherwise EXECUTE against a real gardener. Honouring this header lets an audit
// confirm "operator would be accepted" (200) on a real gardener without mutating
// it — a single curl can no longer close a prod gardener.
const auditProbeHeader = "X-Gardenio-Audit-Probe"

// auditProbeDryRun reports whether this request is an operability audit probe
// that must NOT mutate. It is checked by every gardener mutation handler AFTER
// the auth gates (so a viewer-scoped probe still gets its authoritative 401 and
// an unauthorized caller cannot use the header to learn anything a 401 wouldn't
// already tell it) and BEFORE any daemon write.
func auditProbeDryRun(r *http.Request) bool {
	return strings.TrimSpace(r.Header.Get(auditProbeHeader)) != ""
}

// writeAuditProbeAck responds to an audit probe that reached a mutation handler
// with authenticated-operator identity but must not mutate. It returns 200 with
// a plain-text acknowledgement so the probe records "route exists + operator
// would be accepted" without any state change. The X-Gardenio-Audit-Probe echo
// lets a caller assert the dry-run path was taken (vs a real 200 mutation).
func writeAuditProbeAck(w http.ResponseWriter, action string) {
	w.Header().Set(auditProbeHeader, "acknowledged")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "audit-probe: %s route reachable by operator; no state change performed\n", action)
}

// gardenerOpError carries an HTTP status alongside a message so a shared-core
// mutation failure maps to the right response on WHICHEVER surface invoked it
// — the /gardener/{id} control bar (gardener_ops.go) and the /plans/{id} gardener
// bar (plans_gardener.go) both call the cores below and translate this into
// their own error rendering (an http.Error vs an inline bar re-render). Keeping
// the status on the error is what lets the two surfaces reuse ONE mutation body
// without re-deriving the status/message per call site.
type gardenerOpError struct {
	Status  int
	Message string
}

func (e *gardenerOpError) Error() string { return e.Message }

// isGardenerBead reports whether a bead is a valid gardener target: a
// type=trigger bead labelled role:gardener (the canonical shape), or any bead
// carrying a non-empty `gardener` JSON field (covers legacy type=schedule
// gardeners and trigger:schedule beads the loader treats as gardeners).
func isGardenerBead(b *beadsapi.BeadDetail) bool {
	if b == nil {
		return false
	}
	if b.Type == "trigger" && (hasLabel(b, "role:gardener") || hasLabel(b, "trigger:schedule")) {
		return true
	}
	if b.Type == "schedule" {
		return true
	}
	return strings.TrimSpace(b.Fields["gardener"]) != ""
}

// handleGardenerEnable / handleGardenerDisable flip the gardener trigger bead's
// `enabled` field. We write an explicit value in both directions to match the
// asymmetric loader defaults (type=schedule is enabled iff enabled=="true";
// trigger:schedule is enabled unless enabled=="false") — mirroring
// handleToggleTriggerEnabled exactly.
func (s *Server) handleGardenerEnable(w http.ResponseWriter, r *http.Request) {
	s.setGardenerEnabled(w, r, true)
}

func (s *Server) handleGardenerDisable(w http.ResponseWriter, r *http.Request) {
	s.setGardenerEnabled(w, r, false)
}

func (s *Server) setGardenerEnabled(w http.ResponseWriter, r *http.Request, enable bool) {
	id := strings.TrimSpace(r.PathValue("id"))
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	bead, ok := s.loadGardenerTrigger(w, r, id)
	if !ok {
		return
	}
	if auditProbeDryRun(r) {
		writeAuditProbeAck(w, map[bool]string{true: "enable", false: "disable"}[enable])
		return
	}
	if opErr := s.setGardenerEnabledCore(r, bead, enable, email); opErr != nil {
		http.Error(w, opErr.Message, opErr.Status)
		return
	}
	s.rerenderGardenerControls(w, r, id)
}

// setGardenerEnabledCore flips the gardener trigger bead's `enabled` field and
// writes the best-effort audit comment, returning a gardenerOpError (with the
// HTTP status the caller should surface) on a hard failure. It is the shared
// mutation body behind BOTH the /gardener/{id} control bar (setGardenerEnabled)
// AND the /plans/{id} gardener bar (handlePlanGardenerToggle) — extracted so the
// plan surface reuses the exact enable/disable + loader-default semantics
// instead of forking them. Callers pass an already-loaded, already-validated
// gardener bead and own the re-render; the core owns only the state change +
// audit. `enabled` is declared boolean in the type:trigger/type:schedule
// schema, so the write MUST be a JSON bool — the string "true"/"false" is
// 400-rejected by the daemon's field validation (ka-0000003627).
func (s *Server) setGardenerEnabledCore(r *http.Request, bead *beadsapi.BeadDetail, enable bool, email string) *gardenerOpError {
	if statusOpenSortKey(bead.Status) != 0 {
		return &gardenerOpError{Status: http.StatusConflict, Message: "Gardener is closed"}
	}
	next := strconv.FormatBool(enable)
	if err := s.daemon.UpdateBeadFieldsTyped(r.Context(), bead.ID, map[string]any{"enabled": enable}); err != nil {
		s.logger.Error("gardener enable toggle", "id", bead.ID, "actor", email, "next", next, "error", err)
		return &gardenerOpError{Status: http.StatusBadGateway, Message: "Failed to update gardener."}
	}
	// Best-effort audit comment — log on failure but still report success so the
	// caller re-renders with the new state (mutation_history is the canonical
	// record either way).
	if err := s.daemon.AddComment(r.Context(), bead.ID, email, fmt.Sprintf("enabled → %s by %s", next, email)); err != nil {
		s.logger.Warn("gardener enable audit comment", "id", bead.ID, "actor", email, "error", err)
	}
	return nil
}

// handleGardenerRunNow spawns one gardener iteration immediately, mirroring the
// engine's spawn path (bridge.actionSpawnAgent → stampGardenerLoopContext): the
// new agent bead carries schedule_id == the gardener bead ID, the gardener's
// terminal goal as prompt, and the loop-context fields gb prime reads. It then
// bumps RunsSpawned + stamps LastAgentID on the gardener spec exactly like
// gardenerPostFire, so the engine and page stay in sync.
//
// A run-now respects one_spawn_at_a_time: if the prior iteration is still
// running, we reject rather than spawn a second concurrent run.
func (s *Server) handleGardenerRunNow(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	bead, ok := s.loadGardenerTrigger(w, r, id)
	if !ok {
		return
	}
	if auditProbeDryRun(r) {
		// An operability probe must never spawn a real iteration against a live
		// gardener — acknowledge the route without firing (bug ka-0000007079).
		writeAuditProbeAck(w, "run-now")
		return
	}
	// Fleet-view (/gardeners list) clicks carry row=1 (task ka-0000006826): they
	// re-render the compact inline Run-now action with a confirmation/error note
	// in place, since the list row has no #gardener-controls bar to swap. The
	// cockpit control bar (no row flag) still swaps #gardener-controls. Same
	// spawnGardenerRunNowCore verb behind both — only the re-render target differs.
	fromRow := strings.TrimSpace(r.FormValue("row")) != ""
	if _, opErr := s.spawnGardenerRunNowCore(r, bead, email); opErr != nil {
		if fromRow {
			// Inline the failure note (HTTP 200 so HTMX 2 swaps it — a 4xx/5xx
			// response is NOT swapped by default) rather than a bare http.Error, so
			// the operator learns WHY nothing spawned without losing the button.
			s.renderGardenerRunNow(w, r, id, opErr.Message, true)
			return
		}
		http.Error(w, opErr.Message, opErr.Status)
		return
	}
	if fromRow {
		s.renderGardenerRunNow(w, r, id, "Spawned one iteration.", false)
		return
	}
	s.rerenderGardenerControls(w, r, id)
}

// gardenerRunNowView is the data the compact inline /gardeners-list Run-now action
// needs (task ka-0000006826). Notice, when non-empty, is the post-click
// confirmation ("Spawned one iteration.") or error ("A run is already in
// flight…") rendered beside the button; Error tones it red. Both are empty on
// the page render — the button then renders bare.
type gardenerRunNowView struct {
	ID     string
	Notice string
	Error  bool
}

// renderGardenerRunNow renders the compact inline Run-now action partial for one
// /gardeners-list row, threading the post-click notice (if any) beside the
// button so the fleet-view click reports its outcome in place.
func (s *Server) renderGardenerRunNow(w http.ResponseWriter, r *http.Request, id, notice string, isErr bool) {
	if isErr && respondAction(w, r, id, actionFailure(notice, http.StatusOK)) {
		return
	}
	s.renderPartial(w, r, "templates/partials/gardener_run_now.html", "gardener_run_now", map[string]any{
		"RunNow": gardenerRunNowView{ID: id, Notice: notice, Error: isErr},
	})
}

// spawnGardenerRunNowCore spawns one gardener iteration immediately and does the
// post-fire bookkeeping, returning the new agent bead id (or a gardenerOpError
// with the HTTP status the caller should surface). It is the shared spawn body
// behind BOTH the /gardener/{id} control bar (handleGardenerRunNow) AND the
// /plans/{id} Wake button (handlePlanGardenerWake) — extracted so the plan Wake
// verb reuses the EXACT engine-mirroring spawn path (role, loop-context fields,
// assigned edge, spec bump) rather than forking it. That reuse is what makes a
// plan's gardener and its interactive agent one lineage: because run-now honours
// the trigger's spawn role (plan-gardener) and writes the assigned edge, the
// Wake-spawned iteration resolves as the plan's bound agent (planAssistant),
// exactly as a scheduled fire would. Callers pass an already-loaded,
// already-validated gardener bead and own the re-render.
func (s *Server) spawnGardenerRunNowCore(r *http.Request, bead *beadsapi.BeadDetail, email string) (string, *gardenerOpError) {
	id := bead.ID
	// The run counter (runs_spawned / lineage_root) is written back to the
	// trigger bead below, and kbeads accepts that write only from the trigger
	// engine or an OIDC gardenio-admin session (autonomousCounterPatchDenial,
	// gd-is1H5NmI05). An operator-only session would spawn an iteration whose
	// counter write 403s, so the run would escape run_budget (gd-uzCT6oXur9).
	// Refuse up front; viewerCanAdmin fails closed for a roleless session, which
	// reaches the daemon with the static token and would be refused the same way.
	if !viewerCanAdmin(r) {
		return "", &gardenerOpError{Status: http.StatusForbidden, Message: "Run now / Wake needs a gardenio-admin session: the run must be counted against the gardener's run budget."}
	}
	if statusOpenSortKey(bead.Status) != 0 {
		return "", &gardenerOpError{Status: http.StatusConflict, Message: "Gardener is closed"}
	}

	def := bridge.ParseTriggerDef(bead, "schedule")
	spec := def.Gardener
	if spec == nil {
		// A bead with no gardener JSON can still be run, but without a spec we
		// have no goal/budget to honour — treat as a misconfigured gardener.
		return "", &gardenerOpError{Status: http.StatusBadRequest, Message: "Gardener has no spec to run"}
	}

	// one_spawn_at_a_time guard — refuse a concurrent run while the prior
	// iteration is still live (matches gardenerPreFireCheck gate 2).
	if spec.OneSpawnAtATime && spec.LastAgentID != "" {
		if prior, err := s.daemon.GetBead(r.Context(), spec.LastAgentID); err == nil && prior != nil {
			switch prior.Status {
			case "open", "in_progress":
				return "", &gardenerOpError{Status: http.StatusConflict, Message: "A run is already in flight (one spawn at a time)."}
			}
		}
	}

	project := def.Project
	if project == "" {
		project = beadProjectLabel(bead)
	}
	prompt := strings.TrimSpace(def.Prompt)
	if prompt == "" {
		prompt = strings.TrimSpace(spec.TerminalGoal)
	}
	if prompt == "" {
		prompt = "Drive gardener " + id
	}

	agentName, err := generateAgentName(project, prompt)
	if err != nil {
		s.logger.Error("gardener run-now: name", "id", id, "error", err)
		return "", &gardenerOpError{Status: http.StatusInternalServerError, Message: "Failed to spawn iteration."}
	}

	// Task target: pin to the gardener's assign bead when set (mirrors
	// actionSpawnAgent's AssignBead branch); otherwise the gardener's task_id;
	// otherwise leave empty (the iteration is its own home).
	taskID := strings.TrimSpace(spec.AssignBead)
	if taskID == "" {
		taskID = strings.TrimSpace(bead.Fields["task_id"])
	}

	// Spawn role: honour the trigger's role field like the engine's scheduled
	// fire does (actionSpawnAgent spawns trig.Role). A gardener trigger may set
	// role=<other> so its iterations take a specific role (ka-0000005048) —
	// e.g. a plan gardener spawns role=plan-gardener (or the legacy
	// plan-assistant alias) so its iterations resolve as the plan's bound
	// assistant. Hard-coding "gardener" here made run-now iterations diverge
	// from every scheduled fire of the same trigger.
	spawnRole := strings.TrimSpace(def.Role)
	if spawnRole == "" {
		spawnRole = "gardener"
	}

	fields := map[string]string{
		"agent":       agentName,
		"mode":        "crew",
		"role":        spawnRole,
		"project":     project,
		"prompt":      prompt,
		"spawned_by":  "run-now:" + id,
		"operated_by": canonicalOIDC(email),
	}
	if taskID != "" {
		fields["task_id"] = taskID
	}
	// Loop-context fields — identical keys to stampGardenerLoopContext so gb
	// prime renders the "## Scheduled Loop Context" section and the iteration
	// joins this gardener's life-thread (schedule_id is the FK the page filters
	// on).
	fields["schedule_id"] = id
	fields["schedule_title"] = def.Title
	fields["schedule_runs_spawned"] = strconv.Itoa(spec.RunsSpawned + 1)
	fields["schedule_runs_completed"] = strconv.Itoa(spec.RunsCompleted)
	fields["schedule_max_runs"] = strconv.Itoa(spec.MaxRuns)
	fields["spawn_source"] = "schedule"
	if spec.TerminalGoal != "" {
		fields["schedule_terminal_goal"] = spec.TerminalGoal
	}
	if spec.Name != "" {
		fields["schedule_name"] = spec.Name
	}
	if spec.SlackChannel != "" {
		fields["schedule_slack_channel"] = spec.SlackChannel
	}
	if spec.SlackUserID != "" {
		fields["slack_user_id"] = spec.SlackUserID
	}
	if spec.UserBeadID != "" {
		fields["user_bead_id"] = spec.UserBeadID
	}
	if spec.LastAgentID != "" {
		fields["schedule_prior_agent_id"] = spec.LastAgentID
		// Neutral succession back-pointer, written additively alongside the
		// untouched schedule_prior_agent_id (persistent-agents Phase 1, task
		// ka-0000006931). Mirrors stampGardenerLoopContext on the engine's
		// scheduled-fire path so a run-now iteration joins the same unified
		// lineage chain the viewer walks (prior_agent_id OR schedule_prior_agent_id).
		fields["prior_agent_id"] = spec.LastAgentID
	}
	// lineage_root_id — the gardener's first iteration agent, carried on the spec
	// and stamped on every iteration so the whole life-thread groups by indexed
	// lookup. Empty on the very first run-now (the post-fire block below seeds it).
	if spec.LineageRoot != "" {
		fields["lineage_root_id"] = spec.LineageRoot
	}
	// Runtime backend opt-in — honour the trigger's execution_target exactly
	// like the engine's scheduled fire (actionSpawnAgent) does, so a Wake/
	// run-now iteration of a MicroVM-targeted gardener lands on the same
	// backend as its scheduled siblings (ka-0000003026).
	if def.ExecutionTarget != "" {
		fields["execution_target"] = def.ExecutionTarget
	}

	fieldsJSON, err := json.Marshal(fields)
	if err != nil {
		s.logger.Error("gardener run-now: marshal fields", "id", id, "error", err)
		return "", &gardenerOpError{Status: http.StatusInternalServerError, Message: "Failed to spawn iteration."}
	}
	desc := "Gardener run-now iteration of " + id
	if taskID != "" {
		desc = "Assigned to task: " + taskID + "\n\n" + desc
	}
	agentID, err := s.daemon.CreateBead(r.Context(), beadsapi.CreateBeadRequest{
		Title:       agentName,
		Type:        "agent",
		Description: desc,
		CreatedBy:   canonicalOIDC(email),
		Fields:      fieldsJSON,
	})
	if err != nil {
		s.logger.Error("gardener run-now: create agent", "id", id, "error", err)
		return "", &gardenerOpError{Status: http.StatusBadGateway, Message: "Failed to spawn iteration."}
	}
	// Best-effort labels + lineage so the iteration appears in project-scoped
	// listings and links back to the gardener.
	if project != "" {
		if err := s.daemon.AddLabel(r.Context(), agentID, "project:"+project); err != nil {
			s.logger.Warn("gardener run-now: project label", "agent", agentID, "error", err)
		}
	}
	if err := s.daemon.AddLabel(r.Context(), agentID, "role:"+spawnRole); err != nil {
		s.logger.Warn("gardener run-now: role label", "agent", agentID, "error", err)
	}
	// AssignBead-pinned iterations also carry the `assigned` edge SpawnAgent
	// writes on the scheduled path — the load-bearing edge the constellation,
	// rollups, and the plan-assistant resolver (planAssistant) read.
	if taskID != "" {
		if err := s.daemon.AddDependency(r.Context(), agentID, taskID, "assigned", agentName); err != nil {
			s.logger.Warn("gardener run-now: assigned edge", "agent", agentID, "task", taskID, "error", err)
		}
	}

	// Post-fire bookkeeping: bump RunsSpawned + stamp LastAgentID, then persist
	// the updated spec back through the SHARED encode path so the engine's next
	// LoadTriggers reads consistent counters (mirrors gardenerPostFire).
	updated := *spec
	updated.RunsSpawned++
	updated.LastAgentID = agentID
	// Seed the lineage root once, mirroring gardenerPostFire: the first iteration
	// agent this gardener records is the chain root, held constant so later
	// iterations stamp lineage_root_id = this value (task ka-0000006931).
	if updated.LineageRoot == "" {
		updated.LineageRoot = agentID
	}
	// The counter write is load-bearing: an iteration the budget never sees is an
	// uncounted run. If it cannot be persisted, retract the iteration (close the
	// agent bead, which despawns its pod) and fail the action instead of logging.
	raw, mErr := json.Marshal(updated)
	if mErr == nil {
		mErr = s.daemon.UpdateBeadFieldsTyped(r.Context(), id, map[string]any{"gardener": string(raw)})
	}
	if mErr != nil {
		s.logger.Error("gardener run-now: persist spec; retracting iteration", "id", id, "agent", agentID, "error", mErr)
		if cErr := s.daemon.CloseBeadAs(r.Context(), agentID, canonicalOIDC(email), map[string]string{
			"close_reason": "run-now: run counter write failed; iteration retracted",
		}); cErr != nil {
			s.logger.Error("gardener run-now: retract iteration", "id", id, "agent", agentID, "error", cErr)
		}
		return "", &gardenerOpError{Status: http.StatusBadGateway, Message: "Failed to record the run against the gardener's budget; the iteration was cancelled."}
	}

	if err := s.daemon.AddComment(r.Context(), id, email, fmt.Sprintf("run-now: spawned iteration %s by %s", agentID, email)); err != nil {
		s.logger.Warn("gardener run-now audit comment", "id", id, "actor", email, "error", err)
	}
	return agentID, nil
}

// handleGardenerCancelRun closes the in-flight iteration (the gardener's
// LastAgentID, if still running). Closing the agent bead signals the
// controller to despawn the pod (the same path `gb stop` / a terminal status
// drives). The gardener trigger bead itself is left running so the loop can
// spawn the next iteration.
func (s *Server) handleGardenerCancelRun(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	bead, ok := s.loadGardenerTrigger(w, r, id)
	if !ok {
		return
	}
	if auditProbeDryRun(r) {
		writeAuditProbeAck(w, "cancel-run")
		return
	}

	def := bridge.ParseTriggerDef(bead, "schedule")
	var target string
	if def.Gardener != nil {
		target = strings.TrimSpace(def.Gardener.LastAgentID)
	}
	if target == "" {
		http.Error(w, "No in-flight run to cancel.", http.StatusConflict)
		return
	}
	prior, err := s.daemon.GetBead(r.Context(), target)
	if err != nil || prior == nil {
		s.logger.Warn("gardener cancel-run: load target", "id", id, "agent", target, "error", err)
		http.Error(w, "No in-flight run to cancel.", http.StatusConflict)
		return
	}
	if prior.Status != "open" && prior.Status != "in_progress" {
		http.Error(w, "Latest run is not in flight.", http.StatusConflict)
		return
	}
	if err := s.daemon.CloseBeadAs(r.Context(), target, canonicalOIDC(email), map[string]string{
		"cancelled_by":     canonicalOIDC(email),
		"cancelled_reason": "operator cancel-run from gardener page",
	}); err != nil {
		s.logger.Error("gardener cancel-run: close agent", "id", id, "agent", target, "error", err)
		http.Error(w, "Failed to cancel run.", http.StatusBadGateway)
		return
	}
	if err := s.daemon.AddComment(r.Context(), id, email, fmt.Sprintf("cancel-run: closed in-flight iteration %s by %s", target, email)); err != nil {
		s.logger.Warn("gardener cancel-run audit comment", "id", id, "actor", email, "error", err)
	}
	s.rerenderGardenerControls(w, r, id)
}

// handleGardenerRetire closes the gardener trigger bead — terminating the loop so
// it stops showing up in future LoadTriggers passes. Mirrors gardenerTerminate's
// effect (comment + close) but attributed to the operator. The life-thread of
// past iterations is preserved (closing the trigger does not touch the agent
// beads), so the page still renders as a historical record.
func (s *Server) handleGardenerRetire(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	bead, ok := s.loadGardenerTrigger(w, r, id)
	if !ok {
		return
	}
	if auditProbeDryRun(r) {
		// The most destructive lever: retire CLOSES the gardener trigger bead.
		// An operability probe must confirm the route without closing a live
		// prod gardener (bug ka-0000007079 — this exact path retired ka-0000003719).
		writeAuditProbeAck(w, "retire")
		return
	}
	if statusOpenSortKey(bead.Status) != 0 {
		// Already closed — re-render controls (idempotent) rather than erroring.
		s.rerenderGardenerControls(w, r, id)
		return
	}
	if err := s.daemon.AddComment(r.Context(), id, email, fmt.Sprintf("retired by %s", email)); err != nil {
		s.logger.Warn("gardener retire audit comment", "id", id, "actor", email, "error", err)
	}
	if err := s.daemon.CloseBeadAs(r.Context(), id, canonicalOIDC(email), map[string]string{
		"gardener_terminated_slot": "operator",
		"gardener_terminated_kind": "retired",
	}); err != nil {
		s.logger.Error("gardener retire: close", "id", id, "error", err)
		http.Error(w, "Failed to retire gardener.", http.StatusBadGateway)
		return
	}
	s.rerenderGardenerControls(w, r, id)
}

// handleGardenerEditForm renders the inline config-edit form (GET
// /gardener/{id}/edit). Operator-gated at the route layer (operatorOnly), but
// that RBAC gate default-allows a roleless caller, so the form BODY is ALSO
// gated here on viewerCanOperateAuthed — the SAME parity POST /config enforces
// (email!="" → 401). Without this an unauthenticated in-cluster request could
// pull up the editable terminal_goal / exit_condition / on_exit form even
// though the save would 401 (bug ka-0000005377: render-gate ≠ route-gate).
func (s *Server) handleGardenerEditForm(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if !viewerCanOperateAuthed(r) {
		s.renderPartial(w, r, "templates/partials/gardener_config.html", "gardener_operator_note", nil)
		return
	}
	bead, ok := s.loadGardenerTrigger(w, r, id)
	if !ok {
		return
	}
	s.renderGardenerConfigForm(w, r, gardenerConfigViewFromBead(bead))
}

// handleGardenerConfigSave persists edited GardenerSpec knobs back to the
// gardener trigger bead's `gardener` JSON field (POST /gardener/{id}/config).
// Operator-gated. The engine-owned runtime counters (RunsSpawned, LastAgentID,
// ProgressState, …) are PRESERVED — we decode the existing spec, overlay only
// the operator-editable knobs, and re-encode through the shared
// bridge.GardenerSpec path so the page never clobbers engine state.
func (s *Server) handleGardenerConfigSave(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	bead, ok := s.loadGardenerTrigger(w, r, id)
	if !ok {
		return
	}
	if auditProbeDryRun(r) {
		// Config-save persists edited GardenerSpec knobs — an operability probe
		// must confirm the route is operator-reachable without overwriting the
		// live gardener's spec (bug ka-0000007079).
		writeAuditProbeAck(w, "config")
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form", http.StatusBadRequest)
		return
	}

	// Decode the existing spec so engine-owned runtime fields survive the edit.
	def := bridge.ParseTriggerDef(bead, "schedule")
	spec := bridge.GardenerSpec{}
	if def.Gardener != nil {
		spec = *def.Gardener
	}

	// Build a draft view from the submitted form so we can re-render with the
	// operator's raw input on a validation error.
	draft := gardenerConfigView{
		ID:              id,
		Title:           bead.Title,
		Name:            spec.Name,
		Prompt:          strings.TrimSpace(r.PostForm.Get("prompt")),
		TerminalGoal:    strings.TrimSpace(r.PostForm.Get("terminal_goal")),
		ExitCondition:   strings.TrimSpace(r.PostForm.Get("exit_condition")),
		OnExit:          strings.TrimSpace(r.PostForm.Get("on_exit")),
		RunBudget:       strings.TrimSpace(r.PostForm.Get("run_budget")),
		MaxRuns:         strings.TrimSpace(r.PostForm.Get("max_runs")),
		RetryOnFailure:  r.PostForm.Get("retry_on_failure") != "",
		MaxRetries:      strings.TrimSpace(r.PostForm.Get("max_retries")),
		RunTimeout:      strings.TrimSpace(r.PostForm.Get("run_timeout")),
		Timeout:         strings.TrimSpace(r.PostForm.Get("timeout")),
		OneSpawnAtATime: r.PostForm.Get("one_spawn_at_a_time") != "",
		StopJSON:        strings.TrimSpace(r.PostForm.Get("stop_json")),
		ProgressJSON:    strings.TrimSpace(r.PostForm.Get("progress_json")),
		Conditions:      strings.TrimSpace(r.PostForm.Get("conditions")),
		Cron:            strings.TrimSpace(r.PostForm.Get("cron")),
		Cooldown:        strings.TrimSpace(r.PostForm.Get("cooldown_minutes")),
		MaxConcurrent:   strings.TrimSpace(r.PostForm.Get("max_concurrent")),
	}

	// Populate the guided-row builder view up front so EVERY error path below
	// (including a numeric-knob rejection) re-renders the operator's exact stop
	// rows rather than dropping them. Uses a best-effort run budget (a real
	// negative/garbage budget is rejected just below); the stop-handling block
	// re-derives the view with the validated budget.
	if r.PostForm.Get(stopBuilderMarker) != "" {
		bestBudget, _ := parseNonNegInt(strings.TrimSpace(r.PostForm.Get("run_budget")))
		draft.StopBuilder = buildStopSpecBuilderView(parseStopSpecFromForm(r.PostForm), bestBudget)
	}

	// Parse + validate the numeric knobs. Empty → 0 (omitted). Negative or
	// non-numeric → inline error with the draft preserved.
	runBudget, err := parseNonNegInt(draft.RunBudget)
	if err != nil {
		s.renderGardenerConfigError(w, r, draft, "Run budget must be a non-negative whole number.")
		return
	}
	maxRuns, err := parseNonNegInt(draft.MaxRuns)
	if err != nil {
		s.renderGardenerConfigError(w, r, draft, "Max runs must be a non-negative whole number.")
		return
	}
	maxRetries, err := parseNonNegInt(draft.MaxRetries)
	if err != nil {
		s.renderGardenerConfigError(w, r, draft, "Max retries must be a non-negative whole number.")
		return
	}
	// Resolve the stop spec from whichever authoring surface the form used.
	//
	//   - Guided builder (stop_builder marker present, task ka-0000000897):
	//     reconstruct the spec from the schema-driven rows and run the
	//     no-silent-no-op validator. A blocking error re-renders the builder
	//     with the operator's exact rows + the per-row error list, so the
	//     engine never persists a spec that would silently never fire
	//     (max_runs with a 0 budget, wall_clock with a garbage duration, a
	//     bead_query with an unknown op, ...).
	//   - Legacy raw JSON textarea (advanced escape hatch): validated as JSON
	//     only AND semantically via scheduler.ValidateStopSpec, which mirrors
	//     the engine's per-kind requirements so we never persist dead config
	//     (task ka-0000002260, I2.5).
	var stopRaw json.RawMessage
	if r.PostForm.Get(stopBuilderMarker) != "" {
		stopSpec := parseStopSpecFromForm(r.PostForm)
		draft.StopBuilder = buildStopSpecBuilderView(stopSpec, runBudget)
		if vres := validateStopSpec(stopSpec, runBudget); !vres.OK() {
			draft.StopBuilder.Errors = vres.Errors
			draft.StopBuilder.Advisories = vres.Advisories
			s.renderGardenerConfigError(w, r, draft,
				"Fix the stop conditions below — the highlighted rows would never fire.")
			return
		}
		encoded, err := stopSpecToJSON(stopSpec)
		if err != nil {
			s.renderGardenerConfigError(w, r, draft, "Failed to encode the stop spec.")
			return
		}
		stopRaw = encoded
		// Keep the raw textarea mirror in sync for the advanced view.
		draft.StopJSON = prettyRawJSON(stopRaw)
	} else if draft.StopJSON != "" {
		if !json.Valid([]byte(draft.StopJSON)) {
			s.renderGardenerConfigError(w, r, draft, "Stop spec must be valid JSON.")
			return
		}
		if err := scheduler.ValidateStopSpec(draft.StopJSON); err != nil {
			s.renderGardenerConfigError(w, r, draft, "Stop spec is invalid: "+err.Error())
			return
		}
		stopRaw = json.RawMessage(draft.StopJSON)
	}

	// Progress spec (events/windows/surface) lives INSIDE the gardener blob and,
	// like the stop spec, is validated through the engine's own parser so we
	// never persist a spec the engine silently drops — an unknown window scope,
	// a missing window name, a last_n_runs window with n<=0, or an event with no
	// kind all cause progress.ParseSpec to error, which is exactly what the
	// scheduler's gardener_hooks path would hit at eval time (ka-0000006347, I2.2).
	var progressRaw json.RawMessage
	if draft.ProgressJSON != "" {
		if !json.Valid([]byte(draft.ProgressJSON)) {
			s.renderGardenerConfigError(w, r, draft, "Progress spec must be valid JSON.")
			return
		}
		if _, err := progress.ParseSpec(draft.ProgressJSON); err != nil {
			s.renderGardenerConfigError(w, r, draft, "Progress spec is invalid: "+err.Error())
			return
		}
		progressRaw = json.RawMessage(draft.ProgressJSON)
	}

	// Conditions[] is the pre-flight bead-query gate — a top-level trigger-bead
	// field (NOT part of the gardener blob). Validate it against the SAME
	// conditions parser the engine uses (ParseExpressions preserves field leaves
	// + boolean composition, unlike the lossy legacy ParseConditions), so a
	// typo'd gate reds the save instead of silently gating the gardener off.
	if draft.Conditions != "" {
		if !json.Valid([]byte(draft.Conditions)) {
			s.renderGardenerConfigError(w, r, draft, "Conditions must be a valid JSON array.")
			return
		}
		if _, err := conditions.ParseExpressions(draft.Conditions); err != nil {
			s.renderGardenerConfigError(w, r, draft, "Conditions are invalid: "+err.Error())
			return
		}
	}

	// Cron cadence — validate through the engine's own 5-field parser (supports
	// the @hourly/@daily aliases) so an unparseable expression cannot silently
	// wedge the gardener's fire schedule. Empty leaves the field untouched
	// (default */30 staggered cron applies).
	if draft.Cron != "" {
		if _, err := scheduler.ParseCron(draft.Cron); err != nil {
			s.renderGardenerConfigError(w, r, draft, "Cron expression is invalid: "+err.Error())
			return
		}
	}

	// Cooldown + max_concurrent are non-negative integer trigger-bead fields
	// (stored as strings in the schema, like max_runs). Empty → omitted so the
	// engine's defaults (5-min cooldown, 3 concurrent) apply.
	if _, err := parseNonNegInt(draft.Cooldown); err != nil {
		s.renderGardenerConfigError(w, r, draft, "Cooldown must be a non-negative whole number of minutes.")
		return
	}
	if _, err := parseNonNegInt(draft.MaxConcurrent); err != nil {
		s.renderGardenerConfigError(w, r, draft, "Max concurrent must be a non-negative whole number.")
		return
	}

	// Overlay ONLY the operator-editable knobs. Runtime/engine-owned fields
	// (RunsSpawned, RunsCompleted, ConsecutiveFailures, LastAgentID,
	// LastCheckedAgent, RunLog, ProgressState, Slack/User binding, AssignBead)
	// are carried through from the decoded spec untouched.
	spec.TerminalGoal = draft.TerminalGoal
	spec.ExitCondition = draft.ExitCondition
	spec.OnExit = draft.OnExit
	spec.RunBudget = runBudget
	spec.MaxRuns = maxRuns
	spec.RetryOnFailure = draft.RetryOnFailure
	spec.MaxRetries = maxRetries
	spec.RunTimeout = draft.RunTimeout
	spec.Timeout = draft.Timeout
	spec.OneSpawnAtATime = draft.OneSpawnAtATime
	spec.Stop = stopRaw
	spec.Progress = progressRaw

	raw, err := json.Marshal(spec)
	if err != nil {
		s.logger.Error("gardener config save: marshal", "id", id, "error", err)
		s.renderGardenerConfigError(w, r, draft, "Failed to encode the gardener spec.")
		return
	}
	// Persist the gardener blob PLUS the top-level scheduling knobs in one typed
	// update. A non-empty knob is written verbatim; an emptied knob is written
	// (as "") to CLEAR it back to the engine default — but ONLY when the field
	// already exists on the bead, so we never introduce a brand-new empty field
	// onto a bead whose type-schema does not declare it (a legacy type=schedule
	// gardener's schema lacks conditions/cooldown_minutes; a PATCH that ADDS an
	// unknown field is 400-rejected by the daemon, whereas one that clears an
	// EXISTING field is accepted — kbeads unknown-field guard, beads.go). The
	// canonical live gardener is type=trigger, whose schema declares all four, so
	// this only guards the legacy edge.
	fieldUpdates := map[string]any{"gardener": string(raw)}
	setSchedField := func(key, val string) {
		if val != "" || bead.Fields[key] != "" {
			fieldUpdates[key] = val
		}
	}
	// Prompt is the operative per-run instruction — a top-level field, not part
	// of the gardener blob (bug ka-0000003596). Persist it alongside the other
	// scheduling knobs. The textarea is pre-populated with the current prompt so
	// a plain save round-trips it unchanged; an operator who clears it explicitly
	// clears it (the run-now path then falls back to terminal_goal).
	setSchedField("prompt", draft.Prompt)
	setSchedField("conditions", draft.Conditions)
	setSchedField("cron", draft.Cron)
	setSchedField("cooldown_minutes", draft.Cooldown)
	setSchedField("max_concurrent", draft.MaxConcurrent)
	if err := s.daemon.UpdateBeadFieldsTyped(r.Context(), id, fieldUpdates); err != nil {
		s.logger.Error("gardener config save: persist", "id", id, "actor", email, "error", err)
		s.renderGardenerConfigError(w, r, draft, "Failed to save the gardener config.")
		return
	}
	if err := s.daemon.AddComment(r.Context(), id, email, fmt.Sprintf("gardener config edited by %s", email)); err != nil {
		s.logger.Warn("gardener config save audit comment", "id", id, "actor", email, "error", err)
	}

	draft.Saved = true
	s.renderGardenerConfigForm(w, r, draft)
}

// rerenderGardenerControls re-fetches the gardener and renders the operator
// controls partial in place (the htmx swap target for lifecycle actions).
func (s *Server) rerenderGardenerControls(w http.ResponseWriter, r *http.Request, id string) {
	bead, err := s.daemon.GetBead(r.Context(), id)
	if err != nil || bead == nil {
		s.logger.Warn("gardener controls re-render: get bead", "id", id, "error", err)
		http.Error(w, "Updated, but failed to re-render controls.", http.StatusOK)
		return
	}
	s.renderPartial(w, r, "templates/partials/gardener_controls.html", "gardener_controls",
		map[string]any{"Controls": gardenerControlsViewFromBead(bead)})
}

// renderGardenerConfigForm renders the config-edit partial as a standalone htmx
// fragment.
func (s *Server) renderGardenerConfigForm(w http.ResponseWriter, r *http.Request, v gardenerConfigView) {
	s.renderPartial(w, r, "templates/partials/gardener_config.html", "gardener_config", map[string]any{"Config": v})
}

// renderGardenerConfigError re-renders the form with an inline error and the
// operator's draft preserved. Rendered at HTTP 200 (not 4xx) because htmx 2's
// default responseHandling does not swap on 4xx/5xx — same rationale as
// renderCreateGardenerError (ka-0000005181).
func (s *Server) renderGardenerConfigError(w http.ResponseWriter, r *http.Request, draft gardenerConfigView, msg string) {
	if respondAction(w, r, draft.ID, actionFailure(msg, http.StatusOK)) {
		return
	}
	draft.Error = msg
	s.renderGardenerConfigForm(w, r, draft)
}

// gardenerConfigViewFromBead builds the edit-form view from a gardener bead's
// current spec.
func gardenerConfigViewFromBead(b *beadsapi.BeadDetail) gardenerConfigView {
	v := gardenerConfigView{ID: b.ID, Title: b.Title}
	// The four scheduling knobs are top-level bead fields, present whether or
	// not the bead carries a gardener blob — read them straight off the bead so
	// the form renders the operator's current cadence/gate even for a bare
	// schedule. Only the numeric defaults (max_concurrent=3, cooldown=5) are
	// left blank when unset so the placeholder text shows the default.
	v.Conditions = prettyJSON(strings.TrimSpace(b.Fields["conditions"]))
	v.Cron = strings.TrimSpace(b.Fields["cron"])
	v.Cooldown = strings.TrimSpace(b.Fields["cooldown_minutes"])
	v.MaxConcurrent = strings.TrimSpace(b.Fields["max_concurrent"])
	// Per-run prompt (bug ka-0000003596): a top-level trigger-bead field present
	// with or without a gardener blob — read it straight off the bead so the form
	// renders even a bare-schedule gardener's operative instruction.
	v.Prompt = strings.TrimSpace(b.Fields["prompt"])

	def := bridge.ParseTriggerDef(b, "schedule")
	spec := def.Gardener
	if spec == nil {
		return v
	}
	v.Name = spec.Name
	v.TerminalGoal = spec.TerminalGoal
	v.ExitCondition = spec.ExitCondition
	v.OnExit = spec.OnExit
	v.RunBudget = intToFormStr(spec.RunBudget)
	v.MaxRuns = intToFormStr(spec.MaxRuns)
	v.RetryOnFailure = spec.RetryOnFailure
	v.MaxRetries = intToFormStr(spec.MaxRetries)
	v.RunTimeout = spec.RunTimeout
	v.Timeout = spec.Timeout
	v.OneSpawnAtATime = spec.OneSpawnAtATime
	v.StopJSON = prettyRawJSON(spec.Stop)
	v.ProgressJSON = prettyRawJSON(spec.Progress)
	// Guided stop builder (task ka-0000000897 / I2.3): decode the raw stop JSON
	// into schema-driven rows. A parse miss yields a zero spec (empty builder) —
	// the raw StopJSON textarea above still carries the operator's data as the
	// advanced escape hatch, so nothing is lost.
	stopSpec, _ := scheduler.ParseStopSpec(string(spec.Stop))
	v.StopBuilder = buildStopSpecBuilderView(stopSpec, spec.RunBudget)
	return v
}

// parseNonNegInt parses an optional non-negative integer form value. Empty
// returns 0 (the omitempty zero). Negative or non-numeric returns an error.
func parseNonNegInt(s string) (int, error) {
	if s == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid non-negative int: %q", s)
	}
	return n, nil
}

// intToFormStr renders an int for a form value, mapping 0 to "" so an unset
// (omitempty) knob renders as an empty field rather than a literal 0.
func intToFormStr(n int) string {
	if n == 0 {
		return ""
	}
	return strconv.Itoa(n)
}

// beadProjectLabel extracts the project from a bead's project:<name> label.
func beadProjectLabel(b *beadsapi.BeadDetail) string {
	for _, l := range b.Labels {
		if p, ok := strings.CutPrefix(l, "project:"); ok {
			return p
		}
	}
	return ""
}

// viewerCanOperate reports whether the request's viewer may see operate-the-
// gardener levers. It mirrors the route-layer operatorOnly (oidcauth.RequireRole)
// semantics EXACTLY so the page and the POST routes agree on who can operate:
// when the request carries OIDC realm roles, the viewer must meet the operator
// minimum; when no roles are present (dev bypass / non-OIDC internal caller),
// RBAC is not enforced and the levers render (the routes would accept the POST
// too). Without this parity the page could hide a lever the route accepts, or
// show one the route rejects with a 403.
func viewerCanOperate(r *http.Request) bool {
	roles := oidcauth.RolesFromContext(r.Context())
	if len(roles) == 0 {
		return true
	}
	return oidcauth.HasMinimumRole(roles, oidcauth.RoleOperator)
}

// viewerCanOperateAuthed reports whether the request's viewer may see AND
// actually succeed at using the operate-the-gardener levers. It is
// viewerCanOperate (the RBAC-role parity) AND an authenticated identity,
// because EVERY gardener mutation POST additionally rejects a request carrying
// no email (oidcauth.EmailFromContext == "" → 401 Unauthenticated) on top of
// the operatorOnly route gate.
//
// Gating the RENDER on the role alone (viewerCanOperate) over-advertises: an
// unauthenticated in-cluster caller carries no OIDC roles, so viewerCanOperate
// returns true (RBAC is not enforced when no roles are present) yet every
// mutation 401s on the missing email — the render-gate ≠ route-gate dead-end
// this bead fixes (bug ka-0000005377). Requiring the email here makes the page
// emit levers (and the GET /edit form body) ONLY when the matching POST would
// actually be accepted. This is the operator-tier sibling of the thread
// composer's `viewerCanCompose(r) && email != ""` parity (ka-0000003462).
func viewerCanOperateAuthed(r *http.Request) bool {
	return viewerCanOperate(r) && oidcauth.EmailFromContext(r.Context()) != ""
}

// viewerCanCompose reports whether the request's viewer may see the thread
// composer (the /threads + /threads/{id} message box). It mirrors the
// viewerOnly route gate on POST /b/{id}/thread-reply and POST /threads/compose
// (RequireRole(RoleViewer)) EXACTLY, so the page and the POST routes agree on
// who can compose: when the request carries OIDC realm roles the viewer must
// meet the viewer minimum; when no roles are present (dev bypass / non-OIDC
// internal caller) RBAC is not enforced and the box renders.
//
// This is the VIEWER-tier sibling of viewerCanOperate (ka-0000003462). Posting
// a message into a thread is the same trust level as adding a bead comment, so
// the composer is gated at viewer, not operator — otherwise it stays invisible
// to every live AVA user (whose forwarded group is the full subgroup PATH, so
// it never matches the bare RoleOperator name and they fall back to the
// gardenio-viewer default role). Compose handlers still require an authenticated
// identity (email) on top of this, since the send is attributed to the person.
func viewerCanCompose(r *http.Request) bool {
	roles := oidcauth.RolesFromContext(r.Context())
	if len(roles) == 0 {
		return true
	}
	return oidcauth.HasMinimumRole(roles, oidcauth.RoleViewer)
}

// canonicalOIDC normalises a bare email into the canonical "oidc:<email>"
// operator id the daemon and the agent fleet (BEADS_OPERATED_BY) already use.
//
// The OIDC email is a case-INSENSITIVE identifier, but the X-Auth-Request-Email
// header Keycloak sets carries the human's mixed-case address
// (e.g. "Matthew.Baker@acme.example"). Stamping that verbatim forks a SECOND
// operator row for the same human — the exact identity-fragmentation bug behind
// "my plans shows nothing" (ka-0000002505): the agent-created plan epics landed
// under lowercase oidc:matthew.baker@… while the viewer's default filter (and
// viewer-stamped writes) used the mixed-case form, and the daemon alias match
// was case-sensitive. So we fold the oidc form to lowercase here — for both the
// read filter and the write stamp — so there is one canonical operator id.
//
// Slack ids (slack:U<ALNUM>) are case-SENSITIVE and are left untouched.
func canonicalOIDC(email string) string {
	email = strings.TrimSpace(email)
	if email == "" {
		return ""
	}
	low := strings.ToLower(email)
	if strings.HasPrefix(low, "slack:") {
		return email
	}
	if strings.HasPrefix(low, "oidc:") {
		return low
	}
	return "oidc:" + low
}
