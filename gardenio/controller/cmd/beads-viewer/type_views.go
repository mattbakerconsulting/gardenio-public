package main

import (
	"encoding/json"
	"html/template"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
	"gardenio/controller/internal/decisionview"
)

// decisionOption is one option on a decision bead.
type decisionOption struct {
	ID           string
	Short        string
	Label        string
	Description  string
	ArtifactType string
}

// decisionResolution captures the chosen-option branch of a resolved decision.
// Canceled is true when the decision was dismissed (POST .../cancel) rather
// than resolved by selecting an option; CancelReason carries the optional
// dismiss reason.
type decisionResolution struct {
	OptionID     string
	ResponseText string
	ResolvedBy   string
	ResolvedAt   string
	Canceled     bool
	CancelReason string
}

// decisionView is the per-type view model for `decision` beads.
type decisionView struct {
	BeadID     string
	PromptHTML template.HTML
	// Context is the decision's background (decisionview.Context: the
	// context field, else the description) as the shared block. Every
	// surface with a resolve control renders it above the options
	// (gd-hmwSI9lwKe); the block says so when the agent gave none.
	Context decisionContextView
	// Evidence is the strip of screenshots from the beads the decision is
	// about, under the context (decisionEvidenceOf, gd-Pi7C83262N); set by
	// the callers that hold a request context, "" when there are none.
	Evidence    template.HTML
	RequestedBy string
	Options     []decisionOption
	Resolved    bool
	Resolution  decisionResolution
	// CanResolve is true when the viewer is authenticated and the decision is
	// open — gates rendering of the resolve forms in type_decision.html.
	CanResolve bool
	// ResponseDraft preserves the operator's free-text response across
	// re-renders (e.g. after a transient resolve failure).
	ResponseDraft string
	// Error renders inline at the top of the card (auth failure, already-resolved
	// race, daemon error).
	Error string
	// Origin is the agent / task / plan strip (request_origin.go). Populated by
	// the callers that have a daemon handle (injectTypeView,
	// renderDecisionPartial); buildDecisionView itself is pure.
	Origin requestOrigin
	// DetailURL links the card to the /decisions/{id} interaction page (context
	// sidebar + reflag). Empty when the card is already rendered on that page.
	DetailURL string
}

// scheduleView is the per-type view model for `schedule` beads.
// Next-fire is computed server-side via robfig/cron when possible; on parse
// error NextFireError carries a one-line reason and NextFire is empty.
type scheduleView struct {
	Cron          string
	Enabled       bool
	Timezone      string
	Project       string
	Role          string
	Prompt        string
	LastRun       string
	RunsSpawned   string
	RunsCompleted string
	LastAgentID   string
	NextFire      string
	NextFireError string
}

// formulaVar is one variable binding declared on a formula bead.
type formulaVar struct {
	Name        string
	Description string
	Value       string
}

// formulaStep is one step in a formula's pipeline. Status / BeadID are
// derived from sibling fields when present; both may be empty. The DSL-shape
// markers (gate/branch/loop/children/expand/on_complete) drive the structure
// DAG's glyphs (formula_dag.go); depends_on (merged with its `needs` alias)
// drives the layout.
type formulaStep struct {
	ID     string
	Type   string
	Title  string
	Status string
	BeadID string

	// DependsOn is depends_on ∪ needs, deduplicated in first-seen order —
	// the same merge kd formula apply performs.
	DependsOn []string
	// GateType is "decision" / "sync" / "manual" when the step is a gate.
	GateType string
	// IsBranch / BranchDesc describe a conditional-expansion step
	// (BranchDesc names the branching variable, e.g. "on env").
	IsBranch   bool
	BranchDesc string
	// LoopDesc is a compact loop descriptor ("region ∈ [us-east-1, …]",
	// "i × 3", "n ∈ 1..5"); empty for non-loop steps.
	LoopDesc string
	// ChildCount is the number of nested children steps.
	ChildCount int
	// Expand is the referenced expansion formula id, when the step inlines one.
	Expand string
	// OnComplete is true when the step declares a deferred on_complete expansion.
	OnComplete bool
	// WaitsFor carries the fan-in marker (only "all-children" today).
	WaitsFor string
}

// formulaView is the per-type view model for `formula` beads.
type formulaView struct {
	Vars        []formulaVar
	Steps       []formulaStep
	CurrentStep *formulaStep
	// HasRunState is true when at least one step carries a real run status
	// (a populated step_status_<id> field). A pristine recipe — a template
	// with no run — has all-empty step statuses, so this is false and the
	// view suppresses the execution-shaped chrome: a recipe has no completion
	// count or "current" step, and rendering "0 / N completed" reads as a
	// stalled molecule run rather than a legible template (ka-0000003537).
	HasRunState    bool
	CompletedSteps int
	PourStatus     string
	// DAG is the layered structure layout (nil when the formula has no steps
	// or the graph is too large to draw — template falls back to the list).
	DAG *formulaDAG
	// Extends lists parent formula bead ids this recipe inherits steps from.
	Extends []string
	// StepsJSON is the raw steps field, pretty-printed, for the
	// "View raw JSON" reveal.
	StepsJSON string
	// PourCommand / CookCommand are copy-pasteable kd invocations for this
	// recipe (pour has no daemon HTTP endpoint yet — ka-0000007376 deferred
	// a live Pour action — so the viewer hands the operator the CLI command
	// instead). Vars without a bound value render as <name> placeholders.
	PourCommand string
	CookCommand string
}

// adviceView is the per-type view model for `advice` beads. Scope is derived
// from labels (any label that isn't a `g{N}:`-prefixed grouping marker is
// treated as a scope token; `global` shows when no scope tokens are present).
type adviceView struct {
	Scopes               []string
	HookTrigger          string
	HookCommand          string
	HookTimeout          string
	HookOnFailure        string
	Subscriptions        string
	SubscriptionsExclude string
	Inline               bool
}

// buildDecisionView parses decision-specific fields. Returns nil when the
// bead carries no decision-shaped data so the template can show a placeholder.
// renderMarkdown is called with the prompt text and the configured basePath
// so bead-id linkification matches the rest of the page.
func buildDecisionView(bead *beadsapi.BeadDetail, renderMarkdown func(text string) template.HTML) *decisionView {
	if bead == nil {
		return nil
	}
	d := decisionview.From(bead)
	v := &decisionView{
		BeadID:      bead.ID,
		Context:     newDecisionContextView(bead, renderMarkdown),
		RequestedBy: bead.Fields["requested_by"],
	}
	if prompt := bead.Fields["prompt"]; prompt != "" && renderMarkdown != nil {
		v.PromptHTML = renderMarkdown(prompt)
	}
	v.Options = decisionOptionsOf(d.Options)
	if res, ok := parseDecisionResolution(bead.Fields["resolution"]); ok {
		v.Resolution = res
		v.Resolved = true
	} else if res, ok := flatDecisionResolution(bead.Fields); ok {
		// Production resolutions land as flat fields on the bead (`chosen`,
		// `response_text`, `responded_by`, `responded_at`) rather than a
		// JSON-encoded `resolution` blob. Surface them so the resolved-state
		// banner has data to render right after a viewer-driven resolve.
		v.Resolution = res
		v.Resolved = true
	} else if bead.Status == "closed" || bead.Status == "resolved" {
		v.Resolved = true
	}
	if v.RequestedBy == "" && bead.CreatedBy != "" {
		v.RequestedBy = bead.CreatedBy
	}
	return v
}

// parseDecisionOptions reads a decision's options through the shared read
// model (internal/decisionview), which decodes each element on its own so a
// malformed string-array (["a","b"]) never collapses the list — the symptom
// of ka-0000006724: a bare string renders as a plain label.
func parseDecisionOptions(raw string) []decisionOption {
	return decisionOptionsOf(decisionview.ParseOptions(raw))
}

func decisionOptionsOf(opts []decisionview.Option) []decisionOption {
	if len(opts) == 0 {
		return nil
	}
	out := make([]decisionOption, 0, len(opts))
	for _, o := range opts {
		out = append(out, decisionOption{ID: o.ID, Short: o.Short, Label: o.Label, Description: o.Description, ArtifactType: o.ArtifactType})
	}
	return out
}

// decisionResolutionSentinels are `chosen` values the resolution machinery
// writes to mark a resolution SHAPE rather than an option the operator picked.
// They must never be treated as (or normalized against) an option id.
var decisionResolutionSentinels = map[string]bool{
	"dismissed":                        true,
	"_expired":                         true,
	"promoted_to_escalation":           true,
	"returned_to_agent_for_escalation": true,
}

// normalizeChosen reconciles a historical/foreign `chosen` value against the
// decision's declared options so the viewer badges the right option regardless
// of which writer produced it (ka-0000000003). Three Slack write paths used to
// store an option LABEL/SHORT or a raw sentence in `chosen` instead of the id.
//
// Given the raw `chosen` and the options JSON it returns (optionID, responseText):
//   - chosen already equals an option id  -> (id, "")                 [badge fires]
//   - chosen equals a label/short         -> (that option's id, "")   [badge fires]
//   - chosen matches no option, options exist -> ("", chosen)         [render as text]
//   - no options declared, or chosen empty/sentinel -> (chosen, "")   [unchanged]
//
// The read-side backfill means the ~91 already-broken rows badge correctly with
// no data migration; new writes store the id directly and hit the first case.
func normalizeChosen(chosen, optionsRaw string) (optionID, responseText string) {
	if chosen == "" || decisionResolutionSentinels[chosen] {
		return chosen, ""
	}
	opts := parseDecisionOptions(optionsRaw)
	if len(opts) == 0 {
		// No structured options to reconcile against — leave as-is.
		return chosen, ""
	}
	needle := strings.ToLower(strings.TrimSpace(chosen))
	// Prefer an exact id match, then short, then label (case-insensitive).
	for _, o := range opts {
		if o.ID != "" && strings.ToLower(strings.TrimSpace(o.ID)) == needle {
			return o.ID, ""
		}
	}
	for _, o := range opts {
		if o.Short != "" && strings.ToLower(strings.TrimSpace(o.Short)) == needle {
			return o.ID, ""
		}
	}
	for _, o := range opts {
		if o.Label != "" && strings.ToLower(strings.TrimSpace(o.Label)) == needle {
			return o.ID, ""
		}
	}
	// Matches no option — a free-text answer misfiled into `chosen`. Surface it
	// as response text so it renders as a sentence, not a code-span option id.
	return "", chosen
}

// flatDecisionResolution reads the resolution attributes the kbeads HTTP API
// stores directly as bead fields after POST /v1/decisions/{id}/resolve or
// POST /v1/decisions/{id}/cancel. The API uses `chosen` for the option id
// (or the literal `"dismissed"` for a cancel); the other names mirror the
// request bodies (`response_text`, `responded_by`, `responded_at`,
// `canceled`, `cancel_reason`, `canceled_by`).
func flatDecisionResolution(fields map[string]string) (decisionResolution, bool) {
	canceled := parseBool(fields["canceled"])
	r := decisionResolution{
		OptionID:     fields["chosen"],
		ResponseText: fields["response_text"],
		ResolvedBy:   fields["responded_by"],
		ResolvedAt:   fields["responded_at"],
		Canceled:     canceled,
		CancelReason: fields["cancel_reason"],
	}
	if canceled {
		// On cancel, kbeads sets `chosen=dismissed` to mark the resolution
		// shape — that's a bookkeeping marker, not an option the user picked.
		// Map attribution to the cancel-specific fields so the banner reads
		// "Dismissed by <canceledBy>" instead of "chose dismissed".
		if r.ResolvedBy == "" {
			r.ResolvedBy = fields["canceled_by"]
		}
	} else {
		// Reconcile a historical/foreign `chosen` (label/short/raw text) to the
		// option id so the chosen option badges. An unmatched free-text value
		// moves to ResponseText so it renders as a sentence, not a <code> id.
		optionID, respText := normalizeChosen(r.OptionID, fields["options"])
		r.OptionID = optionID
		if respText != "" && r.ResponseText == "" {
			r.ResponseText = respText
		}
	}
	if r.OptionID == "" && r.ResponseText == "" && r.ResolvedBy == "" && r.ResolvedAt == "" && !r.Canceled {
		return r, false
	}
	return r, true
}

func parseDecisionResolution(raw string) (decisionResolution, bool) {
	if raw == "" {
		return decisionResolution{}, false
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return decisionResolution{}, false
	}
	r := decisionResolution{
		OptionID:     stringField(m, "option_id"),
		ResponseText: stringField(m, "response_text"),
		ResolvedBy:   stringField(m, "resolved_by"),
		ResolvedAt:   stringField(m, "resolved_at"),
	}
	if r.OptionID == "" && r.ResponseText == "" && r.ResolvedBy == "" && r.ResolvedAt == "" {
		return r, false
	}
	return r, true
}

// buildScheduleView parses schedule-specific fields and computes next-fire.
//
// Run bookkeeping authority (bug ka-0000002119): a gardener's engine-owned run
// counters live in TWO places that can drift apart — the bead's top-level
// `last_agent_id` / `runs_spawned` / `runs_completed` fields AND the embedded
// `gardener` spec (GardenerSpec.LastAgentID / RunsSpawned / RunsCompleted). The
// gardener evaluator stamps the latest run onto the SPEC each fire
// (gardener_evaluator.go gardenerPostFire), but the top-level fields are a stale
// legacy-scheduler surface that no longer tracks the live run. Reading the
// top-level `last_agent_id` therefore shows an OLD iteration as the "last agent"
// while the spec carries the true latest run. Prefer the gardener spec whenever
// the bead carries one; fall back to the top-level fields for plain
// legacy-scheduler `type=schedule` beads that have no embedded spec.
func buildScheduleView(bead *beadsapi.BeadDetail, now time.Time) *scheduleView {
	if bead == nil {
		return nil
	}
	f := bead.Fields
	// Cadence fallback (ka-0000007640): a type=schedule gardener migrated onto a
	// `triggers:` array carries its cadence in the first kind:cron entry. Prefer
	// the legacy top-level fields when present (no change for un-migrated beads);
	// fall back to the triggers array once the deprecation pass clears them, so
	// the schedule detail page still renders cron + next-fire.
	cronVal, tzVal := scheduleCronTimezone(bead)
	v := &scheduleView{
		Cron:          cronVal,
		Timezone:      tzVal,
		Project:       f["project"],
		Role:          f["role"],
		Prompt:        f["prompt"],
		LastRun:       f["last_run"],
		RunsSpawned:   f["runs_spawned"],
		RunsCompleted: f["runs_completed"],
		LastAgentID:   f["last_agent_id"],
		Enabled:       parseBool(f["enabled"]),
	}
	// Prefer the gardener spec's engine-owned run bookkeeping over the stale
	// top-level fields. The spec is the live surface the gardener evaluator
	// updates; the top-level fields are a legacy snapshot that lags behind. A
	// non-gardener or malformed `gardener` JSON yields a nil spec, leaving the
	// top-level fallbacks above intact.
	if spec := bridge.ParseTriggerDef(bead, "schedule").Gardener; spec != nil {
		if spec.LastAgentID != "" {
			v.LastAgentID = spec.LastAgentID
		}
		if spec.RunsSpawned > 0 {
			v.RunsSpawned = strconv.Itoa(spec.RunsSpawned)
		}
		if spec.RunsCompleted > 0 {
			v.RunsCompleted = strconv.Itoa(spec.RunsCompleted)
		}
	}
	if v.Cron != "" {
		nf, err := computeNextFire(v.Cron, v.Timezone, now)
		if err != nil {
			v.NextFireError = err.Error()
		} else {
			v.NextFire = nf.Format("2006-01-02 15:04:05 MST")
		}
	}
	return v
}

// nextFireParser accepts the same shapes the kbeads scheduler does: 5-field
// cron AND descriptor forms (@hourly, @daily, @weekly, @monthly, @yearly,
// @every <duration>). Without cron.Descriptor, beads with a "@hourly" cron
// field fail to parse here, get a NextFireError row, and sink to the bottom
// of the next-fire sort — which reads to operators as "sort is broken".
var nextFireParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// computeNextFire parses the schedule's cron field and returns the next firing
// time after `now` in the requested timezone. Empty timezone defaults to UTC.
func computeNextFire(spec, tz string, now time.Time) (time.Time, error) {
	next, _, err := computeNextFire2(spec, tz, now)
	return next, err
}

// computeNextFire2 returns BOTH the next firing time after `now` and the fire
// immediately after that ("next + 1"). The second value is a deterministic
// tiebreaker for the next-fire sort: when two schedules share an identical
// next-fire instant (e.g. an @hourly UTC row and a 0 21 PDT row both landing
// on 04:00 UTC during the 03:00–04:00 UTC window), the one whose *subsequent*
// fire is sooner — i.e. the more frequent schedule — should rank first so the
// hourly row never sinks below a coincident daily row (ka-0000003725 /
// ka-0000007664). Empty timezone defaults to UTC.
func computeNextFire2(spec, tz string, now time.Time) (next, after time.Time, err error) {
	loc := time.UTC
	if tz != "" {
		l, lerr := time.LoadLocation(tz)
		if lerr != nil {
			return time.Time{}, time.Time{}, lerr
		}
		loc = l
	}
	sched, perr := nextFireParser.Parse(spec)
	if perr != nil {
		return time.Time{}, time.Time{}, perr
	}
	next = sched.Next(now.In(loc))
	after = sched.Next(next)
	return next, after, nil
}

// buildFormulaView parses formula-specific fields. Step status is best-effort:
// the daemon stores per-step status under the ad-hoc field `step_status_<id>`
// when populated, otherwise the step is shown as pending.
func buildFormulaView(bead *beadsapi.BeadDetail) *formulaView {
	if bead == nil {
		return nil
	}
	f := bead.Fields
	v := &formulaView{
		PourStatus: f["pour_status"],
	}
	v.Vars = parseFormulaVars(f["vars"], f)
	v.Steps = parseFormulaSteps(f["steps"], f)
	for i := range v.Steps {
		if v.Steps[i].Status != "" {
			v.HasRunState = true
		}
		if v.Steps[i].Status == "completed" || v.Steps[i].Status == "done" {
			v.CompletedSteps++
		}
	}
	// Only surface a "current step" when the formula is actually executing.
	// currentFormulaStep returns nil for a pristine recipe (all-empty
	// statuses), so the recipe page never claims step[0] is a running step.
	if v.HasRunState {
		v.CurrentStep = currentFormulaStep(v.Steps)
	}
	v.DAG = buildFormulaDAG(v.Steps, v.CurrentStep)
	v.Extends = parseStringArray(f["extends"])
	v.StepsJSON = prettyJSON(f["steps"])
	v.PourCommand = formulaCLICommand("pour", bead.ID, v.Vars)
	v.CookCommand = formulaCLICommand("cook", bead.ID, v.Vars)
	return v
}

// formulaCLICommand renders a copy-pasteable `kd formula <verb>` invocation
// with one --var flag per declared variable. Bound values are inlined;
// unbound vars become <name> placeholders for the operator to fill in.
func formulaCLICommand(verb, beadID string, vars []formulaVar) string {
	var b strings.Builder
	b.WriteString("kd formula ")
	b.WriteString(verb)
	b.WriteString(" ")
	b.WriteString(beadID)
	for _, v := range vars {
		if v.Name == "" {
			continue
		}
		b.WriteString(" --var ")
		b.WriteString(v.Name)
		b.WriteString("=")
		if v.Value != "" {
			b.WriteString(v.Value)
		} else {
			b.WriteString("<")
			b.WriteString(v.Name)
			b.WriteString(">")
		}
	}
	return b.String()
}

// parseStringArray decodes a JSON string-array field ([]string), returning
// nil on empty or malformed input.
func parseStringArray(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

func parseFormulaVars(raw string, fields map[string]string) []formulaVar {
	if raw == "" {
		return nil
	}
	var arr []map[string]any
	if err := json.Unmarshal([]byte(raw), &arr); err != nil {
		return nil
	}
	out := make([]formulaVar, 0, len(arr))
	for _, m := range arr {
		name := stringField(m, "name")
		out = append(out, formulaVar{
			Name:        name,
			Description: stringField(m, "description"),
			Value:       fields["var_"+name],
		})
	}
	return out
}

func parseFormulaSteps(raw string, fields map[string]string) []formulaStep {
	if raw == "" {
		return nil
	}
	var arr []map[string]any
	if err := json.Unmarshal([]byte(raw), &arr); err != nil {
		return nil
	}
	out := make([]formulaStep, 0, len(arr))
	for _, m := range arr {
		id := stringField(m, "id")
		s := formulaStep{
			ID:        id,
			Type:      stringField(m, "type"),
			Title:     stringField(m, "title"),
			Status:    fields["step_status_"+id],
			BeadID:    fields["step_bead_"+id],
			DependsOn: mergeDeps(m),
			Expand:    stringField(m, "expand"),
			WaitsFor:  stringField(m, "waits_for"),
		}
		if gate, ok := m["gate"].(map[string]any); ok {
			// kd rejects untyped gates at creation, but hand-written or
			// legacy beads may still carry one — default the label to
			// "decision" so the gate glyph never silently drops.
			s.GateType = pickNonEmpty(stringField(gate, "type"), "decision")
		}
		if branch, ok := m["branch"].(map[string]any); ok {
			s.IsBranch = true
			if on := stringField(branch, "on"); on != "" {
				s.BranchDesc = "on " + on
			}
		}
		if loop, ok := m["loop"].(map[string]any); ok {
			s.LoopDesc = loopDesc(loop)
		}
		if children, ok := m["children"].([]any); ok {
			s.ChildCount = len(children)
		}
		if _, ok := m["on_complete"]; ok {
			s.OnComplete = true
		}
		out = append(out, s)
	}
	return out
}

// mergeDeps merges a step's depends_on and needs arrays (needs is a readable
// alias), deduplicated in first-seen order — the same merge kd formula apply
// performs before wiring dependencies.
func mergeDeps(m map[string]any) []string {
	var out []string
	seen := map[string]bool{}
	for _, key := range []string{"depends_on", "needs"} {
		arr, ok := m[key].([]any)
		if !ok {
			continue
		}
		for _, el := range arr {
			dep, ok := el.(string)
			if !ok || dep == "" || seen[dep] {
				continue
			}
			seen[dep] = true
			out = append(out, dep)
		}
	}
	return out
}

// loopDesc summarizes a loop spec for the node's kind label. Exactly one of
// values / count / range is set per the DSL; a spec with none reads as bare
// "loop".
func loopDesc(loop map[string]any) string {
	v := stringField(loop, "var")
	if vals, ok := loop["values"].([]any); ok {
		parts := make([]string, 0, len(vals))
		for _, el := range vals {
			if s, ok := el.(string); ok {
				parts = append(parts, s)
			}
		}
		return v + " ∈ [" + strings.Join(parts, ", ") + "]"
	}
	if count, ok := loop["count"].(float64); ok {
		return v + " × " + strconv.Itoa(int(count))
	}
	if rng := stringField(loop, "range"); rng != "" {
		return v + " ∈ " + rng
	}
	return "loop"
}

// formulaVarPlaceholder matches an unbound recipe variable placeholder in a
// step title, e.g. `{{version}}` or `{{ TICKET }}`. It deliberately does NOT
// match Go-template actions ({{.Field}}, {{if}}, {{range}}, …): a formula step
// title is plain recipe text, and the only `{{…}}` it legitimately carries is a
// bare variable name (optionally spaced). Matching only `{{ name }}` keeps the
// substitution scoped to genuine recipe placeholders.
var formulaVarPlaceholder = regexp.MustCompile(`\{\{\s*[A-Za-z_][A-Za-z0-9_]*\s*\}\}`)

// normalizeFormulaPlaceholders rewrites unbound recipe-variable placeholders
// ({{version}}) to the readable angle form (<version>) the Copy-pour command
// already uses. It is applied to the DAG's plain-text node label + tooltip so a
// recipe's structure graph reads with clean `<version>` tokens instead of raw
// `{{version}}` (or an ugly mid-placeholder truncation). A poured molecule's
// titles carry substituted values, so this is a no-op there (ka-0000003537).
func normalizeFormulaPlaceholders(s string) string {
	return formulaVarPlaceholder.ReplaceAllStringFunc(s, func(m string) string {
		return "<" + strings.TrimSpace(strings.Trim(m, "{}")) + ">"
	})
}

// TitleHTML renders the step title with any unbound recipe-variable placeholder
// ({{version}}) wrapped in a styled var-token span, so a recipe's own template
// variables read as fill-in-at-pour tokens rather than leaking as literal prose
// in what would otherwise look like a running step (ka-0000003537). All other
// text is HTML-escaped; only the injected span markup is trusted.
func (s formulaStep) TitleHTML() template.HTML {
	var b strings.Builder
	last := 0
	for _, m := range formulaVarPlaceholder.FindAllStringIndex(s.Title, -1) {
		b.WriteString(template.HTMLEscapeString(s.Title[last:m[0]]))
		token := strings.TrimSpace(strings.Trim(s.Title[m[0]:m[1]], "{}"))
		b.WriteString(`<span class="formula-var-token">`)
		b.WriteString(template.HTMLEscapeString(token))
		b.WriteString(`</span>`)
		last = m[1]
	}
	b.WriteString(template.HTMLEscapeString(s.Title[last:]))
	return template.HTML(b.String())
}

// currentFormulaStep returns the frontier step of an EXECUTING formula — the
// first step still pending/in-progress/running once the run has begun. It is
// only meaningful when at least one step carries a real status; on a pristine
// recipe (every status empty) there is no "current" step — step[0] is just the
// first step of a template, not a running one — so the caller (buildFormulaView)
// gates this on formulaView.HasRunState and this function bails when NO step has
// a real run-status (ka-0000003537).
func currentFormulaStep(steps []formulaStep) *formulaStep {
	anyRunState := false
	for i := range steps {
		if steps[i].Status != "" {
			anyRunState = true
			break
		}
	}
	if !anyRunState {
		return nil
	}
	for i := range steps {
		s := steps[i].Status
		if s == "" || s == "pending" || s == "in_progress" || s == "running" {
			return &steps[i]
		}
	}
	return nil
}

// buildAdviceView extracts advice scope tokens from labels and surfaces hook
// configuration. Scope grouping prefixes (`g0:`, `g1:`, …) are stripped.
func buildAdviceView(bead *beadsapi.BeadDetail) *adviceView {
	if bead == nil {
		return nil
	}
	v := &adviceView{
		HookTrigger:          bead.Fields["hook_trigger"],
		HookCommand:          bead.Fields["hook_command"],
		HookTimeout:          bead.Fields["hook_timeout"],
		HookOnFailure:        bead.Fields["hook_on_failure"],
		Subscriptions:        bead.Fields["subscriptions"],
		SubscriptionsExclude: bead.Fields["subscriptions_exclude"],
	}
	for _, l := range bead.Labels {
		if l == "inline:true" {
			v.Inline = true
			continue
		}
		v.Scopes = append(v.Scopes, stripGroupingPrefix(l))
	}
	return v
}

// stripGroupingPrefix turns "g0:role:gardener" into "role:gardener". Labels
// without a grouping prefix pass through unchanged.
func stripGroupingPrefix(label string) string {
	rest, ok := strings.CutPrefix(label, "g")
	if !ok {
		return label
	}
	colon := strings.IndexByte(rest, ':')
	if colon < 1 {
		return label
	}
	if _, err := strconv.Atoi(rest[:colon]); err != nil {
		return label
	}
	return rest[colon+1:]
}

func stringField(m map[string]any, k string) string {
	v, ok := m[k]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	bs, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(bs)
}

func parseBool(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "1", "yes", "on":
		return true
	}
	return false
}
