package main

// molecule_view.go — the molecule execution view ("where in its execution a
// given molecule is" — formula-legibility plan ka-0000007376, step S3
// ka-0000007739). A `type=molecule` bead is a poured formula instance: an
// epic whose parent-child children are the step beads `kd formula pour`
// created. This view renders the SAME structure DAG as the parent formula
// (formula_dag.go — the recipe), overlaid with live per-step execution
// status, the active frontier highlighted ("you are here"), per-step owner +
// rolled-up cost, and a top progress bar. The /molecule/{id}/events SSE
// stream re-renders the #molecule-exec fragment on any subtree change so the
// page tracks execution live, mirroring the /plans/{id}/events idiom.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// moleculeSubtreeMaxDepth bounds the parent-child BFS, mirroring
// beadsapi.maxRollupDepth — molecule trees are 2-4 levels in practice.
const moleculeSubtreeMaxDepth = 64

// moleculeVarPattern matches {{variable}} placeholders, the same shape
// kd formula apply substitutes (kbeads varPattern).
var moleculeVarPattern = regexp.MustCompile(`\{\{(\w+)\}\}`)

// moleculeStepExec is one execution row: a formula step joined with the
// molecule step bead(s) instantiated from it. Loop steps expand to several
// beads per recipe step; Beads then carries every iteration and the row's
// Status aggregates them.
type moleculeStepExec struct {
	// StepID is the formula step id with the molecule's applied vars
	// substituted (loop placeholders may remain — see Pattern).
	StepID string
	Title  string
	// Status is the view vocabulary: done / partial / in_progress / ready /
	// pending / blocked / deferred. Empty when the step never got a bead
	// (condition-filtered or deferred on_complete expansion).
	Status string
	// Frontier marks a "you are here" step: in_progress, or ready when
	// nothing is in progress yet.
	Frontier bool
	Beads    []moleculeStepBead
	// CostUSD is the rolled-up cost of the step's whole subtree (the step
	// bead(s) + descendant agents/workflows), pre-formatted. Empty when zero.
	CostUSD string
}

// moleculeStepBead is one instantiated step bead on an execution row.
type moleculeStepBead struct {
	BeadID   string
	Title    string
	Status   string // view vocabulary, per-bead
	Assignee string
	Initials string
}

// moleculeExecView is the per-type view model for `molecule` beads, rendered
// by templates/partials/type_molecule.html.
type moleculeExecView struct {
	MoleculeID string
	// FormulaID / FormulaTitle link back to the recipe. FormulaTitle is empty
	// when the formula bead could not be fetched.
	FormulaID    string
	FormulaTitle string
	// AppliedVars are the pour-time variable bindings, sorted by name.
	AppliedVars []formulaVar

	// Steps is the execution table: formula steps in recipe order, then any
	// step beads that did not match a recipe step (Extra=true rows appended).
	Steps []moleculeStepExec
	// Extras are molecule step beads with no matching recipe step (runtime
	// on_complete expansions, or a formula edited after the pour).
	Extras []moleculeStepBead
	// Frontier lists the "you are here" rows (subset of Steps, by StepID).
	Frontier []moleculeStepExec

	// DAG is the recipe's layered structure graph with per-node status +
	// bead links overlaid. Nil when the formula is unavailable or too large —
	// the template falls back to the step table alone.
	DAG *formulaDAG

	// Progress counters over the ACTUAL step beads (execution truth), not
	// the recipe steps: loop iterations each count once.
	Total      int
	Done       int
	Failed     int // partial-status beads: some descendant work failed
	InProgress int
	Ready      int
	Pending    int
	PctDone    int

	// TotalCostUSD is the whole-subtree rollup total ("$12.34"); CostPartial
	// mirrors MoleculeUsage.Partial (an in-flight workflow run may still be
	// growing). Both empty/false when the rollup failed.
	TotalCostUSD string
	CostPartial  bool
	// AnyStepCost is true iff at least one step row carries a non-empty
	// CostUSD. The template guards the whole Cost column on it, so a molecule
	// with no per-step cost (the common case) drops the column entirely rather
	// than reserving width for permanently-blank cells (ka-0000005731).
	AnyStepCost bool

	// FormulaError is a one-line note when the parent formula could not be
	// resolved (missing formula_id, deleted recipe) — the view degrades to
	// the bead-derived step table.
	FormulaError string
}

// buildMoleculeExecView assembles the execution view for a molecule bead.
// Every daemon fetch is best-effort: a failed formula fetch degrades to the
// step-bead table, a failed rollup drops the cost column — the page renders.
func (s *Server) buildMoleculeExecView(ctx context.Context, bead *beadsapi.BeadDetail) *moleculeExecView {
	if bead == nil {
		return nil
	}
	v := &moleculeExecView{
		MoleculeID:  bead.ID,
		FormulaID:   bead.Fields["formula_id"],
		AppliedVars: parseAppliedVars(bead.Fields["applied_vars"]),
	}

	// (b) Walk the molecule's parent-child subtree. Level-1 children are the
	// step beads; deeper descendants (nested children steps, spawned agents)
	// map back to their level-1 ancestor for the per-step cost rollup.
	subtree, stepAncestor, err := s.moleculeSubtree(ctx, bead.ID)
	if err != nil {
		s.logger.Debug("molecule view: subtree walk", "molecule", bead.ID, "error", err)
	}
	stepIDs := make([]string, 0, len(subtree))
	for _, id := range subtree {
		if stepAncestor[id] == id { // level-1 child = a step bead
			stepIDs = append(stepIDs, id)
		}
	}
	stepBeadByID, berr := s.daemon.BatchGetBeads(ctx, stepIDs)
	if berr != nil {
		s.logger.Debug("molecule view: batch get steps", "molecule", bead.ID, "error", berr)
		stepBeadByID = nil
	}
	stepBeads := make([]*beadsapi.BeadDetail, 0, len(stepIDs))
	for _, id := range stepIDs {
		if b := stepBeadByID[id]; b != nil {
			stepBeads = append(stepBeads, b)
		}
	}

	// (a) Resolve the parent formula and parse its recipe steps.
	var recipeSteps []formulaStep
	if v.FormulaID == "" {
		v.FormulaError = "This molecule carries no formula_id — showing its step beads without the recipe graph."
	} else if formula, ferr := s.daemon.GetBead(ctx, v.FormulaID); ferr != nil || formula == nil {
		s.logger.Debug("molecule view: get formula", "molecule", bead.ID, "formula", v.FormulaID, "error", ferr)
		v.FormulaError = "The parent formula could not be loaded — showing step beads without the recipe graph."
	} else {
		v.FormulaTitle = formula.Title
		recipeSteps = parseFormulaSteps(formula.Fields["steps"], formula.Fields)
	}

	// Join recipe steps ↔ step beads, substituting the molecule's applied
	// vars into recipe ids/titles first. Primary key is the step bead's
	// source_step_id (stamped by kd formula pour); molecules poured through
	// paths that skip that field fall back to a title match. A recipe id or
	// title that still carries a {{placeholder}} (a loop var, bound per
	// iteration at pour time) matches by wildcard, aggregating every
	// iteration onto one row.
	vars := make(map[string]string, len(v.AppliedVars))
	for _, av := range v.AppliedVars {
		vars[av.Name] = av.Value
	}
	matched := make(map[string]bool, len(stepBeads)) // step bead id → consumed
	for _, rs := range recipeSteps {
		id := substituteAppliedVars(rs.ID, vars)
		title := substituteAppliedVars(rs.Title, vars)
		row := moleculeStepExec{StepID: id, Title: title}
		for _, sb := range stepBeads {
			if matched[sb.ID] {
				continue
			}
			if srcID := sb.Fields["source_step_id"]; srcID != "" {
				if srcID == id || matchesLoopPattern(id, srcID) {
					matched[sb.ID] = true
					row.Beads = append(row.Beads, moleculeStepBeadView(sb))
				}
				continue
			}
			// No source_step_id — title-match fallback.
			if sb.Title == title || matchesLoopPattern(title, sb.Title) {
				matched[sb.ID] = true
				row.Beads = append(row.Beads, moleculeStepBeadView(sb))
			}
		}
		v.Steps = append(v.Steps, row)
	}
	// Extras: only unmatched children that VERIFIABLY came from a formula
	// step (source_step_id present — runtime on_complete expansions, or a
	// recipe edited after the pour). Other unmatched children — session logs,
	// squawk messages, follow-up bugs filed under the molecule — are not
	// execution steps and would pollute the progress denominator; they stay
	// visible in the page's dependency/constellation chrome instead.
	for _, sb := range stepBeads {
		if !matched[sb.ID] && sb.Fields["source_step_id"] != "" {
			v.Extras = append(v.Extras, moleculeStepBeadView(sb))
		}
	}

	// Per-bead status → per-row aggregate, then dependency-aware ready/pending
	// over the recipe's depends_on graph: an open step is ready only once every
	// recipe dependency's row is terminal (done or partial — partial unblocks
	// downstream, model.StatusPartial).
	rowByStepID := make(map[string]*moleculeStepExec, len(v.Steps))
	for i := range v.Steps {
		rowByStepID[v.Steps[i].StepID] = &v.Steps[i]
	}
	for i := range v.Steps {
		v.Steps[i].Status = aggregateStepStatus(v.Steps[i].Beads)
	}
	for i, rs := range recipeSteps {
		if i >= len(v.Steps) {
			break
		}
		row := &v.Steps[i]
		if row.Status != "ready" {
			continue
		}
		for _, dep := range rs.DependsOn {
			depRow := rowByStepID[substituteAppliedVars(dep, vars)]
			if depRow == nil {
				continue // inherited/external dep — can't verify, stay optimistic
			}
			if depRow.Status != "done" && depRow.Status != "partial" {
				row.Status = "pending"
				break
			}
		}
	}

	// Progress counters over actual beads (loop iterations each count).
	countBead := func(status string) {
		v.Total++
		switch status {
		case "done":
			v.Done++
		case "partial":
			v.Failed++
		case "in_progress":
			v.InProgress++
		case "ready":
			v.Ready++
		default:
			v.Pending++
		}
	}
	for _, row := range v.Steps {
		for _, b := range row.Beads {
			status := b.Status
			// A per-bead "ready" defers to the row's dependency-aware verdict.
			if status == "ready" && row.Status == "pending" {
				status = "pending"
			}
			countBead(status)
		}
	}
	for _, b := range v.Extras {
		countBead(b.Status)
	}
	if v.Total > 0 {
		v.PctDone = v.Done * 100 / v.Total
	}

	// Frontier: the in_progress rows; when nothing runs yet, the ready rows.
	frontier := map[string]bool{}
	for _, row := range v.Steps {
		if row.Status == "in_progress" {
			frontier[row.StepID] = true
		}
	}
	if len(frontier) == 0 {
		for _, row := range v.Steps {
			if row.Status == "ready" {
				frontier[row.StepID] = true
			}
		}
	}
	for i := range v.Steps {
		if frontier[v.Steps[i].StepID] {
			v.Steps[i].Frontier = true
			v.Frontier = append(v.Frontier, v.Steps[i])
		}
	}

	// Cost: one whole-subtree rollup; per-row cost sums each descendant onto
	// its level-1 step ancestor. Best-effort — a rollup failure drops costs.
	if usage, uerr := s.daemon.RollupMoleculeUsage(ctx, bead.ID); uerr != nil {
		s.logger.Debug("molecule view: cost rollup", "molecule", bead.ID, "error", uerr)
	} else if usage != nil {
		if usage.TotalCostUSD > 0 {
			v.TotalCostUSD = fmtUSD(usage.TotalCostUSD)
		}
		v.CostPartial = usage.Partial
		costByStep := map[string]float64{}
		for _, su := range usage.Steps {
			if anc := stepAncestor[su.BeadID]; anc != "" && anc != bead.ID {
				costByStep[anc] += su.TotalCostUSD
			}
		}
		for i := range v.Steps {
			var sum float64
			for _, b := range v.Steps[i].Beads {
				sum += costByStep[b.BeadID]
			}
			if sum > 0 {
				v.Steps[i].CostUSD = fmtUSD(sum)
				v.AnyStepCost = true
			}
		}
	}

	// The recipe DAG with execution status overlaid: rebuild the step list
	// with per-row status + a bead link, then lay it out exactly as the
	// formula page does, and mark every frontier node "current".
	if len(recipeSteps) > 0 {
		overlay := make([]formulaStep, len(recipeSteps))
		for i, rs := range recipeSteps {
			overlay[i] = rs
			overlay[i].ID = v.Steps[i].StepID
			overlay[i].Title = v.Steps[i].Title
			overlay[i].Status = v.Steps[i].Status
			if len(v.Steps[i].Beads) == 1 {
				overlay[i].BeadID = v.Steps[i].Beads[0].BeadID
			}
			deps := make([]string, len(rs.DependsOn))
			for j, dep := range rs.DependsOn {
				deps[j] = substituteAppliedVars(dep, vars)
			}
			overlay[i].DependsOn = deps
		}
		v.DAG = buildFormulaDAG(overlay, nil)
		if v.DAG != nil {
			for i := range v.DAG.Nodes {
				if frontier[v.DAG.Nodes[i].Step.ID] {
					v.DAG.Nodes[i].IsCurrent = true
					v.DAG.Nodes[i].TitleAttr += "\n▶ you are here"
				}
			}
		}
	}

	return v
}

// moleculeSubtree BFS-walks the parent-child reverse deps under molID.
// Returns the visited ids (excluding the root) and a map from every subtree
// bead to its level-1 step ancestor (level-1 beads map to themselves).
// Mirrors RollupMoleculeUsage's walk so cost attribution matches the rollup.
func (s *Server) moleculeSubtree(ctx context.Context, molID string) (ids []string, stepAncestor map[string]string, err error) {
	stepAncestor = map[string]string{}
	visited := map[string]bool{molID: true}
	level := []string{molID}
	for depth := 0; len(level) > 0; depth++ {
		if depth > moleculeSubtreeMaxDepth {
			return ids, stepAncestor, fmt.Errorf("molecule subtree walk exceeded depth %d for %s", moleculeSubtreeMaxDepth, molID)
		}
		revs, rerr := s.daemon.BatchGetDependencies(ctx, level, "reverse")
		if rerr != nil {
			return ids, stepAncestor, rerr
		}
		var next []string
		for _, id := range level {
			for _, d := range revs[id] {
				if d.Type != "parent-child" || d.BeadID == "" || visited[d.BeadID] {
					continue
				}
				visited[d.BeadID] = true
				if id == molID {
					stepAncestor[d.BeadID] = d.BeadID
				} else {
					stepAncestor[d.BeadID] = stepAncestor[id]
				}
				ids = append(ids, d.BeadID)
				next = append(next, d.BeadID)
			}
		}
		level = next
	}
	return ids, stepAncestor, nil
}

// moleculeStepBeadView maps one step bead onto its row entry.
func moleculeStepBeadView(b *beadsapi.BeadDetail) moleculeStepBead {
	return moleculeStepBead{
		BeadID:   b.ID,
		Title:    b.Title,
		Status:   moleculeBeadStatus(b),
		Assignee: b.Assignee,
		Initials: actorInitials(b.Assignee),
	}
}

// moleculeBeadStatus maps a step bead's raw status onto the view vocabulary.
// "ready" here is provisional — buildMoleculeExecView demotes it to pending
// when the recipe's dependencies aren't terminal yet.
func moleculeBeadStatus(b *beadsapi.BeadDetail) string {
	switch b.Status {
	case "closed":
		return "done"
	case "partial":
		return "partial"
	case "in_progress":
		return "in_progress"
	case "blocked":
		return "blocked"
	case "deferred":
		return "deferred"
	case "open", "":
		return "ready"
	default:
		return b.Status
	}
}

// aggregateStepStatus folds per-iteration bead statuses onto one row verdict.
// Precedence: anything running beats waiting; a failure taints an otherwise
// finished row; the row is done only when EVERY iteration is done.
func aggregateStepStatus(beads []moleculeStepBead) string {
	if len(beads) == 0 {
		return "" // never instantiated (condition-filtered / deferred expansion)
	}
	counts := map[string]int{}
	for _, b := range beads {
		counts[b.Status]++
	}
	switch {
	case counts["in_progress"] > 0:
		return "in_progress"
	case counts["done"]+counts["partial"] == len(beads) && counts["partial"] > 0:
		return "partial"
	case counts["done"] == len(beads):
		return "done"
	case counts["blocked"] > 0:
		return "blocked"
	case counts["deferred"] > 0 && counts["ready"] == 0:
		return "deferred"
	case counts["ready"] > 0:
		return "ready"
	default:
		return "pending"
	}
}

// parseAppliedVars decodes the molecule's applied_vars JSON object into the
// formulaVar shape the side rail already renders, sorted by name for a
// stable display.
func parseAppliedVars(raw string) []formulaVar {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil
	}
	out := make([]formulaVar, 0, len(m))
	for name, val := range m {
		out = append(out, formulaVar{Name: name, Value: val})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// substituteAppliedVars replaces {{name}} placeholders with bound values,
// leaving unbound placeholders (loop vars) in place — the same semantics as
// kd formula apply's substituteVars.
func substituteAppliedVars(s string, vars map[string]string) string {
	if s == "" || len(vars) == 0 {
		return s
	}
	return moleculeVarPattern.ReplaceAllStringFunc(s, func(match string) string {
		if val, ok := vars[match[2:len(match)-2]]; ok {
			return val
		}
		return match
	})
}

// matchesLoopPattern reports whether srcID is a loop-iteration instance of a
// recipe step id that still carries {{placeholder}}s after applied-var
// substitution (each pour iteration substituted its own loop value). The
// placeholders match any non-empty run, the literal segments must match
// exactly.
func matchesLoopPattern(pattern, srcID string) bool {
	if !strings.Contains(pattern, "{{") {
		return false
	}
	parts := moleculeVarPattern.Split(pattern, -1)
	var b strings.Builder
	b.WriteString("^")
	for i, p := range parts {
		if i > 0 {
			b.WriteString(".+")
		}
		b.WriteString(regexp.QuoteMeta(p))
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return false
	}
	return re.MatchString(srcID)
}

// moleculeEventsTopics is the NATS filter set for the molecule live stream:
// bead lifecycle (step status flips) + dependency edges (a runtime expansion
// adding steps mid-flight rebuilds the interest set).
const moleculeEventsTopics = "beads.bead.>,beads.dependency.added,beads.dependency.removed"

// handleMoleculeEvents serves /molecule/{id}/events — the molecule execution
// view's live-progress SSE stream. On any lifecycle event for the molecule or
// a subtree bead it re-renders the WHOLE #molecule-exec fragment under the
// `molecule-updated` event; molecule_events.js swaps it in place, so a step
// flipping to done (or a fresh expansion step landing) shows immediately.
// Mirrors handlePlanEvents' shape.
func (s *Server) handleMoleculeEvents(w http.ResponseWriter, r *http.Request) {
	r, cancelStream := sseRequestUntilTokenExpiry(r)
	defer cancelStream()
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		http.Error(w, "Missing molecule id", http.StatusBadRequest)
		return
	}
	ctx := r.Context()

	bead, err := s.daemon.GetBead(ctx, id)
	if err != nil {
		var apiErr *beadsapi.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			http.Error(w, "Molecule not found", http.StatusNotFound)
			return
		}
		s.logger.Error("molecule events: get bead", "id", id, "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	if bead == nil || bead.Type != "molecule" {
		http.Error(w, "Molecule not found", http.StatusNotFound)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	interest := s.moleculeInterestSet(ctx, id)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher.Flush()

	events, err := s.daemon.EventStream(ctx, moleculeEventsTopics)
	if err != nil {
		s.logger.Error("molecule events: opening event stream", "id", id, "error", err)
		fmt.Fprintf(w, ": stream open failed\n\n")
		flusher.Flush()
		return
	}

	fmt.Fprintf(w, ": connected\n\n")
	flusher.Flush()

	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-events:
			if !ok {
				return
			}
			s.writeMoleculeEvent(ctx, w, r, flusher, evt, id, &interest)
		}
	}
}

// moleculeInterestSet is the molecule + its whole parent-child subtree.
func (s *Server) moleculeInterestSet(ctx context.Context, molID string) map[string]struct{} {
	set := map[string]struct{}{molID: {}}
	ids, _, err := s.moleculeSubtree(ctx, molID)
	if err != nil {
		s.logger.Debug("molecule events: subtree walk", "id", molID, "error", err)
	}
	for _, id := range ids {
		set[id] = struct{}{}
	}
	return set
}

// writeMoleculeEvent filters one daemon event against the interest set and
// emits a re-rendered #molecule-exec fragment when it is relevant. Dependency
// edges touching a member rebuild the interest set first so steps added
// mid-flight (on_complete expansions) start streaming immediately.
func (s *Server) writeMoleculeEvent(ctx context.Context, w http.ResponseWriter, r *http.Request, flusher http.Flusher, evt beadsapi.SSEEvent, molID string, interest *map[string]struct{}) {
	if strings.HasPrefix(evt.Event, "beads.dependency.") {
		var dwrap struct {
			Dependency *struct {
				BeadID      string `json:"bead_id"`
				DependsOnID string `json:"depends_on_id"`
			} `json:"dependency"`
			BeadID      string `json:"bead_id"`
			DependsOnID string `json:"depends_on_id"`
		}
		if err := json.Unmarshal(evt.Data, &dwrap); err != nil {
			return
		}
		target, source := dwrap.DependsOnID, dwrap.BeadID
		if dwrap.Dependency != nil {
			target, source = dwrap.Dependency.DependsOnID, dwrap.Dependency.BeadID
		}
		_, srcIn := (*interest)[source]
		_, tgtIn := (*interest)[target]
		if !srcIn && !tgtIn {
			return
		}
		*interest = s.moleculeInterestSet(ctx, molID)
		if s.writeMoleculeSSEFragment(w, s.renderMoleculeExecFragment(ctx, r, molID)) {
			flusher.Flush()
		}
		return
	}

	if agentEventKind(evt.Event) == "" {
		return // not a bead create/update/close
	}
	var wrapper struct {
		Bead *struct {
			ID string `json:"id"`
		} `json:"bead"`
	}
	if err := json.Unmarshal(evt.Data, &wrapper); err != nil || wrapper.Bead == nil {
		return
	}
	if _, ok := (*interest)[wrapper.Bead.ID]; !ok {
		return
	}
	if s.writeMoleculeSSEFragment(w, s.renderMoleculeExecFragment(ctx, r, molID)) {
		flusher.Flush()
	}
}

// writeMoleculeSSEFragment writes one rendered fragment as a single-line SSE
// message under the `molecule-updated` event. No-op on an empty fragment.
func (s *Server) writeMoleculeSSEFragment(w http.ResponseWriter, fragment string) bool {
	if fragment == "" {
		return false
	}
	fragment = strings.ReplaceAll(fragment, "\r", "")
	fragment = strings.ReplaceAll(fragment, "\n", " ")
	fmt.Fprintf(w, "event: molecule-updated\ndata: %s\n\n", fragment)
	return true
}

// renderMoleculeExecFragment renders the molecule_exec partial to a string
// for the SSE payload. Returns "" on any error so the caller skips the frame
// rather than emitting a broken one.
func (s *Server) renderMoleculeExecFragment(ctx context.Context, r *http.Request, molID string) string {
	tmpl, ok := s.partials["templates/partials/type_molecule.html"]
	if !ok {
		s.logger.Error("molecule events: type_molecule partial not found")
		return ""
	}
	bead, err := s.daemon.GetBead(ctx, molID)
	if err != nil || bead == nil {
		s.logger.Debug("molecule events: refetch molecule", "id", molID, "error", err)
		return ""
	}
	view := s.buildMoleculeExecView(ctx, bead)
	// Prewarm the agent-link resolver over the step assignees so the
	// fragment's agentLink calls resolve from one batched lookup.
	if r != nil {
		var names []string
		for _, row := range view.Steps {
			for _, b := range row.Beads {
				if b.Assignee != "" {
					names = append(names, b.Assignee)
				}
			}
		}
		r = s.prewarmAgentLinksForNames(r, names)
	}
	bound, err := s.bindAgentLink(tmpl, r)
	if err != nil {
		s.logger.Error("molecule events: bind template", "error", err)
		return ""
	}
	var buf strings.Builder
	if err := bound.ExecuteTemplate(&buf, "molecule_exec", map[string]any{
		"Molecule": view,
		"BasePath": s.basePath,
	}); err != nil {
		s.logger.Error("molecule events: render molecule_exec", "molecule", molID, "error", err)
		return ""
	}
	return buf.String()
}
