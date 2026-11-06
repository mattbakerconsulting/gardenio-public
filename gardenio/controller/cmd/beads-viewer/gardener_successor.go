package main

// gardener_successor.go — card-graph AUTHORING for gardener chaining (task
// ka-0000005273 / "C3"; epic ka-0000005510; gardener-chaining design §4).
//
// C1 landed the `chain.to` schema + the `chains_to`/`succeeds` typed edges; C2
// renders the workflow rails on /maps; C4 landed the engine-side heir spawn
// that materializes the heir when the predecessor reaches its terminal goal.
// C3 is the MISSING organ the design calls the "biggest idea": AUTHORING. From
// a gardener card an operator draws a successor off the card's right edge → we
// serialize it to a `chain.to` on the predecessor bead. The chain is *drawn*,
// then the SAME engine (C4) hands off the baton for real on goal_satisfied, and
// the SAME rail (C2) renders it. Nothing new fires here — this just writes the
// declaration.
//
// Three shapes the design asks for (§3.3), all expressed by writing chain.to:
//
//   - drag-to-create-successor — the linear pipeline: one `chain.to` heir.
//   - fan-out — one predecessor arming N heirs: `chain.to` becomes an array
//     (bridge.ChainSpec.Extra / Heirs()). Authoring appends; the array grows.
//   - join (fan-in) — an heir that waits for a whole umbrella subtree to close:
//     `chain.to.join_parent`, which flips the heir's preflight bead_query to
//     `==0 open under <umbrella>` (bridge.buildHeirBeadSpec).
//   - branch — a conditional heir woken by a decision: `chain.to.branch_decision_key`,
//     which authors the heir with a `decision_resolved` trigger.
//
// Every mutation is operator-gated at the route layer (see server.go). The
// predecessor's engine-owned runtime fields are never touched: we decode the
// existing `chain` block, append/replace ONE heir, and re-encode through the
// shared bridge.ChainSpec codec so the engine reads exactly what we wrote.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
	"gardenio/controller/internal/oidcauth"
)

// gardenerSuccessorSheetView is the data passed to the authoring sheet partial —
// the "create successor gardener" form opened off a gardener card's right edge.
type gardenerSuccessorSheetView struct {
	// PredID / PredName identify the predecessor the successor chains off.
	PredID   string
	PredName string
	// PredEpic is the predecessor's own epic (gardener-epic:<id> label), shown as
	// the default the heir inherits ({{same}}).
	PredEpic string
	// ExistingHeirs is how many heirs the predecessor already declares — the
	// form makes clear an additional successor FANS OUT rather than replaces.
	ExistingHeirs int
	// PhaseDefault seeds the phase field (e.g. "impl" → "verify"); best-effort.
	PhaseDefault string

	// Error / Draft are populated when a POST round-trips back on a validation
	// failure so the operator does not lose their input.
	Draft gardenerSuccessorDraft
	Error string
	// Saved flips true after a successful append so the partial confirms in place.
	Saved   bool
	HeirNum int // 1-based position of the just-added heir (for the success note)
}

// gardenerSuccessorDraft is the operator-editable heir declaration. It mirrors
// bridge.ChainTo's authorable subset (the engine-owned inherit/role defaults
// are filled by bridge.ChainSpec.Normalize at parse time).
type gardenerSuccessorDraft struct {
	Phase             string
	TerminalGoal      string
	PromptAdvice      string
	Epic              string
	Role              string
	JoinParent        string
	BranchDecisionKey string
}

// handleGardenerSuccessorForm renders the authoring sheet (GET
// /gardener/{id}/successor). Operator-gated at the route layer, but the RBAC
// gate default-allows a roleless caller, so the form BODY is ALSO gated here on
// viewerCanOperateAuthed — the SAME parity POST enforces (email == "" → 401) —
// so an unauthenticated in-cluster request can't pull up the authoring form
// whose save would 401 (mirrors handleGardenerEditForm, bug ka-0000005377).
func (s *Server) handleGardenerSuccessorForm(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if !viewerCanOperateAuthed(r) {
		// Reuse the shared operator-note block (defined in gardener_config.html)
		// so the authoring gate reads identically to the config-edit gate.
		s.renderPartial(w, r, "templates/partials/gardener_config.html", "gardener_operator_note", nil)
		return
	}
	bead, ok := s.loadGardenerTrigger(w, r, id)
	if !ok {
		return
	}
	s.renderGardenerSuccessorForm(w, r, gardenerSuccessorSheetFromBead(bead))
}

// handleGardenerSuccessor appends a `chain.to` heir onto the predecessor's
// `chain` field (POST /gardener/{id}/successor). This is the "serialize the
// drawn chain to chain.to" write. It is additive: an existing single heir
// becomes a fan-out array; the engine-owned `on`/`mode` and the other heirs are
// preserved. Operator-gated.
func (s *Server) handleGardenerSuccessor(w http.ResponseWriter, r *http.Request) {
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
		// Drawing a successor rewrites the gardener's chain.to — an operability
		// probe must confirm the route without mutating it (bug ka-0000007079).
		writeAuditProbeAck(w, "successor")
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form", http.StatusBadRequest)
		return
	}

	base := gardenerSuccessorSheetFromBead(bead)
	draft := gardenerSuccessorDraft{
		Phase:             strings.TrimSpace(r.PostForm.Get("phase")),
		TerminalGoal:      strings.TrimSpace(r.PostForm.Get("terminal_goal")),
		PromptAdvice:      strings.TrimSpace(r.PostForm.Get("prompt_advice")),
		Epic:              strings.TrimSpace(r.PostForm.Get("epic")),
		Role:              strings.TrimSpace(r.PostForm.Get("role")),
		JoinParent:        strings.TrimSpace(r.PostForm.Get("join_parent")),
		BranchDecisionKey: strings.TrimSpace(r.PostForm.Get("branch_decision_key")),
	}
	base.Draft = draft

	// An heir must name SOMETHING identity-bearing, else bridge.hasHeir drops it
	// and the write is a silent no-op. Require at least a phase or a terminal
	// goal so the drawn card is meaningful and the cycle guard has a handle.
	if draft.Phase == "" && draft.TerminalGoal == "" && draft.PromptAdvice == "" {
		s.renderGardenerSuccessorError(w, r, base,
			"Give the successor at least a phase, a terminal goal, or a prompt-advice id.")
		return
	}

	heir := &bridge.ChainTo{
		Phase:             draft.Phase,
		TerminalGoal:      draft.TerminalGoal,
		PromptAdvice:      draft.PromptAdvice,
		Epic:              draft.Epic,
		Role:              draft.Role,
		JoinParent:        draft.JoinParent,
		BranchDecisionKey: draft.BranchDecisionKey,
	}

	// Decode the predecessor's existing chain (if any), append the heir, and
	// re-encode. bridge.ChainSpec's MarshalJSON emits a bare object for one heir
	// and an array only when it fans out, so a single-heir append stays
	// byte-compatible with C1's shape.
	spec := parseChainSpec(bead)
	if spec == nil {
		spec = &bridge.ChainSpec{}
	}
	if spec.To == nil {
		spec.To = heir
	} else {
		spec.Extra = append(spec.Extra, heir)
	}

	raw, err := json.Marshal(spec)
	if err != nil {
		s.logger.Error("gardener successor: marshal chain", "id", id, "error", err)
		s.renderGardenerSuccessorError(w, r, base, "Failed to encode the chain declaration.")
		return
	}
	if err := s.daemon.UpdateBeadFieldsTyped(r.Context(), id, map[string]any{"chain": string(raw)}); err != nil {
		s.logger.Error("gardener successor: persist chain", "id", id, "actor", email, "error", err)
		s.renderGardenerSuccessorError(w, r, base, "Failed to save the successor.")
		return
	}
	if err := s.daemon.AddComment(r.Context(), id, email,
		fmt.Sprintf("gardener successor authored by %s: %s", email, successorAuditLabel(heir))); err != nil {
		s.logger.Warn("gardener successor: audit comment", "id", id, "actor", email, "error", err)
	}

	// Re-read the count so the confirmation reflects the persisted fan-out.
	base.ExistingHeirs = len(spec.Heirs())
	base.HeirNum = base.ExistingHeirs
	base.Draft = draft
	base.Saved = true
	s.renderGardenerSuccessorForm(w, r, base)
}

// parseChainSpec decodes a gardener bead's `chain` field into a bridge.ChainSpec
// without the hasHeir/Normalize filtering ParseTriggerDef applies (we want the
// raw declaration so an append preserves an already-authored — but as-yet
// terse — heir). Returns nil when the field is absent or unparseable.
func parseChainSpec(b *beadsapi.BeadDetail) *bridge.ChainSpec {
	raw := strings.TrimSpace(b.Fields["chain"])
	if raw == "" {
		return nil
	}
	var spec bridge.ChainSpec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		return nil
	}
	return &spec
}

// gardenerSuccessorSheetFromBead builds the authoring-sheet view from a gardener
// bead: its identity, current heir count, and a phase-default seed.
func gardenerSuccessorSheetFromBead(b *beadsapi.BeadDetail) gardenerSuccessorSheetView {
	def := bridge.ParseTriggerDef(b, "schedule")
	v := gardenerSuccessorSheetView{
		PredID:   b.ID,
		PredName: gardenerDisplayName(b, def),
		PredEpic: beadGardenerEpicLabel(b),
	}
	if def.Chain != nil {
		v.ExistingHeirs = len(def.Chain.Heirs())
	}
	v.PhaseDefault = successorPhaseDefault(def)
	return v
}

// gardenerDisplayName resolves the gardener's human name — the GardenerSpec.Name
// when set, else the bead title.
func gardenerDisplayName(b *beadsapi.BeadDetail, def bridge.TriggerDef) string {
	if def.Gardener != nil {
		if n := strings.TrimSpace(def.Gardener.Name); n != "" {
			return n
		}
	}
	return b.Title
}

// beadGardenerEpicLabel extracts the predecessor's own epic from its
// gardener-epic:<id> label — the epic an heir inherits by default.
func beadGardenerEpicLabel(b *beadsapi.BeadDetail) string {
	for _, l := range b.Labels {
		if rest, ok := strings.CutPrefix(l, "gardener-epic:"); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

// successorPhaseDefault seeds the phase field with the "next" phase after the
// predecessor's declared phase, using the dogfood pipeline's canonical order
// (research → impl → ux → verify, design §6). Falls back to "" when the
// predecessor's phase is unknown or terminal.
func successorPhaseDefault(def bridge.TriggerDef) string {
	// The predecessor's phase lives on its own chain.to (what IT hands to) only
	// for a mid-chain gardener; for the head we read a `phase` hint off the last
	// heir if present. Best-effort — an empty default just leaves the field blank.
	order := []string{"research", "impl", "ux", "verify"}
	var cur string
	if def.Chain != nil {
		for _, h := range def.Chain.Heirs() {
			if p := strings.TrimSpace(h.Phase); p != "" {
				cur = p
			}
		}
	}
	if cur == "" {
		return ""
	}
	for i, p := range order {
		if strings.EqualFold(p, cur) && i+1 < len(order) {
			return order[i+1]
		}
	}
	return ""
}

// successorAuditLabel summarizes an authored heir for the predecessor's audit
// comment, naming the fan-out/join/branch shape so the write is legible in the
// bead's history without decoding the JSON.
func successorAuditLabel(to *bridge.ChainTo) string {
	parts := []string{}
	if to.Phase != "" {
		parts = append(parts, "phase="+to.Phase)
	}
	if to.TerminalGoal != "" {
		parts = append(parts, "goal="+truncateForAudit(to.TerminalGoal, 60))
	}
	if to.JoinParent != "" {
		parts = append(parts, "join="+to.JoinParent)
	}
	if to.BranchDecisionKey != "" {
		parts = append(parts, "branch="+to.BranchDecisionKey)
	}
	if len(parts) == 0 {
		return "(unspecified heir)"
	}
	return strings.Join(parts, " · ")
}

// truncateForAudit clamps a string to n runes with an ellipsis so an audit
// comment stays a one-liner.
func truncateForAudit(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// renderGardenerSuccessorForm renders the authoring sheet as a standalone htmx
// fragment (the swap target the card's "+ successor" affordance posts into).
func (s *Server) renderGardenerSuccessorForm(w http.ResponseWriter, r *http.Request, v gardenerSuccessorSheetView) {
	s.renderPartial(w, r, "templates/partials/gardener_successor.html", "gardener_successor", map[string]any{"Sheet": v})
}

// renderGardenerSuccessorError re-renders the sheet with an inline error and the
// operator's draft preserved. Rendered at HTTP 200 (not 4xx) because htmx 2's
// default responseHandling does not swap on 4xx/5xx — same rationale as
// renderCreateGardenerError (ka-0000005181).
func (s *Server) renderGardenerSuccessorError(w http.ResponseWriter, r *http.Request, v gardenerSuccessorSheetView, msg string) {
	if respondAction(w, r, v.PredID, actionFailure(msg, http.StatusOK)) {
		return
	}
	v.Error = msg
	s.renderGardenerSuccessorForm(w, r, v)
}
