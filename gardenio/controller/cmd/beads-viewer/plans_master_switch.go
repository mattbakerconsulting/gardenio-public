package main

// plans_master_switch.go — the page-level master switch + global-pause banner
// at the top of the /plans index (step 4, ka-0000004048, epic ka-0000004792).
//
// The master switch is a SINGLE control that pauses/resumes every plan gardener
// at once. Plan gardeners ARE gardeners (trigger:schedule beads, or
// trigger:multi beads in event mode), and the fleet already halts all gardener
// firing through the well-known `gardeners-pause` gate bead read by
// bridge.gardenersPauseEngaged in gardenerPreFireCheck. So the master
// switch does NOT add a new mechanism — it opens/closes that SAME gate via the
// SAME shared core the Slack `/schedule` command uses (internal/pausegate,
// extracted from bot_schedule_cmd.go so neither surface reimplements it).
//
// CRITICAL invariant (task): this file only OPENS/CLOSES the gate and READS its
// state. It never touches gardenerPreFireCheck or gardenersPauseEngaged — the
// firing-time semantics are unchanged; the gate a gardener checks is byte-
// identical to the one this switch toggles.
//
// The banner is the always-visible companion: whenever gardeners-pause OR
// scheduler-pause is engaged, a persistent banner surfaces at the top of the
// index so the operator can never miss that the global storm-guard is on — and
// that it SUPERSEDES the per-row toggles (step 3) and the bulk bar (step 6). A
// per-row "running" chip means nothing while the master switch is paused; the
// banner says so explicitly.

import (
	"net/http"

	"gardenio/controller/internal/oidcauth"
	"gardenio/controller/internal/pausegate"
)

// plansMasterSwitchView is the data the master switch + banner partial renders.
// It is rebuilt and swapped in place after every toggle so the control and the
// banner always reflect the live gate state.
type plansMasterSwitchView struct {
	// GardenersPaused is true when the gardeners-pause gate is open — every plan
	// gardener (indeed every gardener fleet-wide) is halted. This is the state the
	// master switch toggles.
	GardenersPaused bool
	// SchedulerPaused is true when the scheduler-pause gate is open — the cron
	// firing of schedule-flavored triggers is silenced, and so is every gardener
	// trigger whatever its source (bridge.scheduleGatedGardener), so neither a
	// scheduled nor an event-mode plan gardener fires while it is on. The master
	// switch does NOT operate this gate (that is `/schedule`'s broader scope), but
	// the banner surfaces it for that reason.
	SchedulerPaused bool
	// IsOperator gates the switch lever (read-only viewers see the banner + a
	// static state chip, but no toggle) — mirrors the operatorOnly POST route.
	IsOperator bool
	// Error, when non-empty, renders as an inline alert in the switch region
	// after a failed toggle (HTTP 200 so htmx still swaps it in).
	Error string
}

// anyPauseEngaged reports whether either global gate is open — the banner's
// render condition.
func (v plansMasterSwitchView) AnyPauseEngaged() bool {
	return v.GardenersPaused || v.SchedulerPaused
}

// plansMasterSwitchData reads both pause gates and projects them into the view.
// Best-effort: a daemon error on either lookup degrades that gate to "not
// engaged" (fail-open, matching bridge.gardenersPauseEngaged) — the banner is a
// visibility aid, and a false "paused" banner would be worse than a transient
// missed one. The switch lever still works regardless (the toggle re-reads the
// gate authoritatively before acting).
func (s *Server) plansMasterSwitchData(r *http.Request, isOperator bool, errMsg string) map[string]any {
	ctx := r.Context()
	view := plansMasterSwitchView{IsOperator: isOperator, Error: errMsg}

	capGate, err := pausegate.Find(ctx, s.daemon, pausegate.GardenersPauseTitle)
	if err != nil {
		s.logger.Warn("plans master switch: gardeners-pause lookup", "error", err)
	} else {
		view.GardenersPaused = capGate != nil
	}

	schedGate, err := pausegate.Find(ctx, s.daemon, pausegate.SchedulerPauseTitle)
	if err != nil {
		s.logger.Warn("plans master switch: scheduler-pause lookup", "error", err)
	} else {
		view.SchedulerPaused = schedGate != nil
	}

	return map[string]any{"MasterSwitch": view}
}

// handlePlansMasterSwitchToggle flips the gardeners-pause gate from the /plans
// index master switch: an engaged (open) gate is closed (resume all gardeners),
// a disengaged (nil) gate is opened (pause all gardeners). Operator-gated at the
// route layer. It re-reads the live gate state and flips to its inverse so a
// rapid double-click can't desync the switch from the write. Delegates to the
// shared pausegate core so the gate is opened/closed EXACTLY as the /schedule
// command does — same title, same type=gate bead — never a parallel mechanism.
func (s *Server) handlePlansMasterSwitchToggle(w http.ResponseWriter, r *http.Request) {
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	ctx := r.Context()

	// Authoritative current state: re-read the gate rather than trusting a
	// hidden form value, so the flip is always against what is live NOW.
	capGate, err := pausegate.Find(ctx, s.daemon, pausegate.GardenersPauseTitle)
	if err != nil {
		s.logger.Error("plans master switch toggle: gate lookup", "actor", email, "error", err)
		s.renderPlansMasterSwitch(w, r, "Failed to read the gardeners-pause state.")
		return
	}

	if capGate != nil {
		// Engaged → resume: close the gate, attributed to the operator so the
		// audit trail names who lifted the fleet-wide pause (CloseBeadAs, the
		// viewer's operator-attributed close idiom — the bridge's `/schedule on`
		// closes unattributed, which is why pausegate has no shared Close).
		if err := s.daemon.CloseBeadAs(ctx, capGate.ID, canonicalOIDC(email), map[string]string{
			"resumed_by":     canonicalOIDC(email),
			"resumed_reason": "operator resumed all gardeners from /plans master switch",
		}); err != nil {
			s.logger.Error("plans master switch toggle: close gate", "id", capGate.ID, "actor", email, "error", err)
			s.renderPlansMasterSwitch(w, r, "Failed to resume gardeners.")
			return
		}
		s.logger.Info("plans master switch: gardeners resumed", "actor", email, "gate", capGate.ID)
	} else {
		// Disengaged → pause: open the gate. createdBy is the operator id so the
		// gate bead's author records who engaged the storm-guard.
		if _, err := pausegate.Open(ctx, s.daemon, pausegate.GardenersPauseTitle, canonicalOIDC(email)); err != nil {
			s.logger.Error("plans master switch toggle: open gate", "actor", email, "error", err)
			s.renderPlansMasterSwitch(w, r, "Failed to pause gardeners.")
			return
		}
		s.logger.Info("plans master switch: gardeners paused", "actor", email)
	}

	s.renderPlansMasterSwitch(w, r, "")
}

// renderPlansMasterSwitch re-reads the gate state and renders the master switch
// + banner partial standalone (the htmx swap target). errMsg, when non-empty,
// renders as an inline alert. Always HTTP 200 so htmx 2 swaps the response
// (its default responseHandling does not swap on 4xx/5xx) — mirrors the plan
// gardener bar's renderPlanGardenerErr rationale.
func (s *Server) renderPlansMasterSwitch(w http.ResponseWriter, r *http.Request, errMsg string) {
	data := s.plansMasterSwitchData(r, viewerCanOperate(r), errMsg)
	s.renderPartial(w, r, "templates/partials/plans_master_switch.html", "plans_master_switch", data)
}
