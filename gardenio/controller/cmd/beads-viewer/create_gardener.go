package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
	"gardenio/controller/internal/gardenermode"
	"gardenio/controller/internal/oidcauth"
	"gardenio/controller/internal/plangardener"
)

// createGardenerSheetView is the data passed to the create-gardener modal partial.
//
// The form is the rich create surface for a gardener (task I2.1, epic
// ka-0000001611): it authors a real `type=trigger` / `role:gardener` loop with
// full knob coverage — schedule (cron) + preset, terminal goal, exit spec, run
// budget / retry, concurrency, and a wake condition on the epic's open
// children — rather than the old 3-input one-shot `type=agent` spawn. Role is
// pinned to `gardener`; operators pick a mentor from the existing-gardeners
// roster and a schedule preset rather than hand-authoring engine JSON.
type createGardenerSheetView struct {
	OperatorEmail string
	Project       string
	EpicID        string
	EpicTitle     string
	PromptDefault string
	Mentors       []gardenerMentorOption
	Presets       []gardenerPresetOption
	Schedules     []schedulePresetOption

	// Defaulted knob values (from the selected preset / sensible fallbacks).
	TerminalGoalDefault  string
	CronDefault          string
	MaxRunsDefault       string
	CooldownDefault      string
	LoopWallClockDefault string
	RunTimeoutDefault    string
	TimezoneDefault      string
}

// gardenerMentorOption is one row in the mentor picker — an existing
// gardener agent the new gardener should learn from.
type gardenerMentorOption struct {
	BeadID    string
	AgentName string
	TaskID    string // epic the mentor was driving (best-effort)
	TaskTitle string
	Status    string
}

// gardenerPresetOption is one selectable behavior preset. Presets seed the
// knob defaults (goal / cron / budget / wake shape) from the patterns the R0.3
// live-gardener survey clustered (epic-driver, ready-queue drainer, MR-green
// watcher, cron roll-up, standing quality gardener). The operator can still
// override every field the preset seeds.
type gardenerPresetOption struct {
	Key         string
	Label       string
	Description string
	// Defaults the preset applies (data-* attributes the form JS reads to
	// re-seed the knob inputs when the preset is chosen).
	Cron          string
	MaxRuns       string
	Cooldown      string
	LoopWallClock string
	Standing      bool
	// GoalTemplate is a human-readable terminal-goal seed; "{epic}" is
	// substituted with the epic id at render time.
	GoalTemplate string
}

// schedulePresetOption is one common cron cadence offered in the schedule
// picker (hourly / every 20m / daily / …). "custom" lets the operator type a
// raw cron. The staggered default is computed from the epic id so two gardeners
// authored the same minute do not stampede.
type schedulePresetOption struct {
	Key   string
	Label string
	Cron  string // empty for "custom" and "staggered" (computed / free-form)
}

// createGardenerResultView is the data passed to the result partial.
type createGardenerResultView struct {
	BeadID    string
	AgentName string
	Project   string
	EpicID    string
	MentorID  string
	Cron      string
	Preset    string
	Error     string
}

// gardenerPresets is the canonical preset catalogue (R0.3, ka-0000001939). Each
// preset seeds a cluster of knobs an operator would otherwise hand-tune. Order
// is display order; the first entry is the default selection.
func gardenerPresets() []gardenerPresetOption {
	return []gardenerPresetOption{
		{
			Key:           "epic-driver",
			Label:         "Epic driver (drive children to closure)",
			Description:   "Wakes every 30m while the epic still has open children; spawns crew to land them, then goes dormant. The default for most epics.",
			Cron:          "", // staggered from epic id
			MaxRuns:       "20",
			Cooldown:      "5",
			LoopWallClock: "168h",
			GoalTemplate:  "All children of {epic} are closed.",
		},
		{
			Key:           "ready-queue-drainer",
			Label:         "Ready-queue drainer (opportunistic)",
			Description:   "Wakes frequently (every 20m) to pull ready work into MRs. Good for a lane that must stay drained.",
			Cron:          "*/20 * * * *",
			MaxRuns:       "30",
			Cooldown:      "5",
			LoopWallClock: "168h",
			GoalTemplate:  "The ready queue under {epic} stays drained: every open child lands a merged MR.",
		},
		{
			Key:           "mr-green-watcher",
			Label:         "MR-green watcher",
			Description:   "Wakes every 15m to shepherd open MRs to green + merge. Tighter cadence, higher run budget.",
			Cron:          "*/15 * * * *",
			MaxRuns:       "40",
			Cooldown:      "3",
			LoopWallClock: "168h",
			GoalTemplate:  "Every MR produced under {epic} is green and merged.",
		},
		{
			Key:           "cron-rollup",
			Label:         "Daily roll-up (scheduled report)",
			Description:   "Fires once a day to produce a roll-up / audit. Low run budget; the run itself does the work.",
			Cron:          "0 9 * * *",
			MaxRuns:       "3",
			Cooldown:      "60",
			LoopWallClock: "168h",
			GoalTemplate:  "Publish the daily roll-up for {epic}.",
		},
		{
			Key:           "standing-quality",
			Label:         "Standing quality gardener (perpetual)",
			Description:   "A perpetual gardener pinned to no terminal epic — audits + drives fixes forever. Marks itself standing so a quiet period is not mistaken for a wedge.",
			Cron:          "23,53 * * * *",
			MaxRuns:       "20",
			Cooldown:      "5",
			LoopWallClock: "168h",
			Standing:      true,
			GoalTemplate:  "STANDING: keep {epic} healthy — this gardener does NOT self-complete; it re-arms every quiet period.",
		},
	}
}

// schedulePresets is the common-cadence catalogue for the schedule picker.
func schedulePresets() []schedulePresetOption {
	return []schedulePresetOption{
		{Key: "staggered", Label: "Every 30m (staggered — recommended)", Cron: ""},
		{Key: "every-15m", Label: "Every 15 minutes", Cron: "*/15 * * * *"},
		{Key: "every-20m", Label: "Every 20 minutes", Cron: "*/20 * * * *"},
		{Key: "hourly", Label: "Hourly", Cron: "0 * * * *"},
		{Key: "every-6h", Label: "Every 6 hours", Cron: "0 */6 * * *"},
		{Key: "daily-9am", Label: "Daily at 09:00", Cron: "0 9 * * *"},
		{Key: "weekdays-9am", Label: "Weekdays at 09:00", Cron: "0 9 * * 1-5"},
		{Key: "custom", Label: "Custom cron…", Cron: ""},
	}
}

// findPreset returns the preset with the given key, or the default (first)
// preset when the key is empty/unknown.
func findPreset(key string) gardenerPresetOption {
	presets := gardenerPresets()
	for _, p := range presets {
		if p.Key == key {
			return p
		}
	}
	return presets[0]
}

// handleCreateGardenerSheet renders the modal that the "Create gardener"
// button opens. The epic id is required (`?epic=gd-…`) — the affordance
// only makes sense when the operator has picked an epic for the new
// gardener to drive.
func (s *Server) handleCreateGardenerSheet(w http.ResponseWriter, r *http.Request) {
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}

	epicID := strings.TrimSpace(r.URL.Query().Get("epic"))
	if epicID == "" {
		http.Error(w, "epic id is required", http.StatusBadRequest)
		return
	}

	view := createGardenerSheetView{
		OperatorEmail: email,
		EpicID:        epicID,
	}

	// Look up the epic so we can show its title, infer the project, and
	// (when the operator hasn't supplied a prompt yet) hint at a sensible
	// default.
	if epic, err := s.daemon.GetBead(r.Context(), epicID); err == nil && epic != nil {
		view.EpicTitle = epic.Title
		view.Project = beadProjectLabel(epic)
		view.PromptDefault = "Drive epic " + epicID
		if epic.Title != "" {
			view.PromptDefault += " (" + epic.Title + ")"
		}
		view.PromptDefault += " to land. Use mentor history for context."
	} else if err != nil {
		s.logger.Warn("loading epic for create-gardener sheet", "epic", epicID, "error", err)
	}

	view.Mentors = s.listGardenerMentors(r.Context())
	view.Presets = gardenerPresets()
	view.Schedules = schedulePresets()

	// Seed the knob defaults from the default preset (epic-driver). The
	// staggered cron is computed from the epic id so co-authored gardeners do
	// not stampede — this is the same seed gb gardener start uses.
	def := findPreset("")
	view.TerminalGoalDefault = strings.ReplaceAll(def.GoalTemplate, "{epic}", epicID)
	view.CronDefault = gardenermode.StaggeredCron(epicID, 30*time.Minute)
	view.MaxRunsDefault = def.MaxRuns
	view.CooldownDefault = def.Cooldown
	view.LoopWallClockDefault = def.LoopWallClock
	view.RunTimeoutDefault = "30m"
	view.TimezoneDefault = plangardener.DefaultTimezone

	s.renderPartial(w, r, "templates/partials/create_gardener.html", "create_gardener_sheet", map[string]any{
		"Sheet": view,
	})
}

// gardenerPreflightCondition is the JSON shape of one cron pre-flight wake gate
// on a gardener trigger — the open-children bead_query the engine ANDs into the
// cron. It mirrors scheduleConditionJSON in cmd/gb/trigger_schedule.go (the CLI
// authoring path) so a viewer-authored gardener and a `gb gardener start` gardener
// carry byte-identical conditions.
type gardenerPreflightCondition struct {
	Type     string   `json:"type"`
	Op       string   `json:"op,omitempty"`
	Value    string   `json:"value,omitempty"`
	Parent   string   `json:"parent,omitempty"`
	Types    []string `json:"types,omitempty"`
	Statuses []string `json:"statuses,omitempty"`
}

// gardenerCreateForm holds the parsed + validated form input for a new gardener.
type gardenerCreateForm struct {
	Project         string
	EpicID          string
	MentorID        string
	Prompt          string
	Preset          string
	TerminalGoal    string
	ExitCondition   string
	OnExit          string
	Cron            string
	Timezone        string
	MaxRuns         int
	Cooldown        int
	MaxConcurrent   int
	LoopWallClock   string
	RunTimeout      string
	RetryOnFailure  bool
	MaxRetries      int
	OneSpawnAtATime bool
	Standing        bool
}

// handleCreateGardener validates the rich form, authors a real `type=trigger` /
// `role:gardener` loop (cron + wake conditions + a bounded stop/progress blob),
// applies project: + role:gardener + gardener-epic: labels, adds an `assigned`
// dependency on the chosen epic, and (when a mentor is chosen) records a
// `mentor:<id>` typed mention so beads-viewer renders a constellation edge.
func (s *Server) handleCreateGardener(w http.ResponseWriter, r *http.Request) {
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form", http.StatusBadRequest)
		return
	}

	form, errMsg := s.parseGardenerCreateForm(r, email)
	if errMsg != "" {
		s.renderCreateGardenerError(w, r, errMsg, http.StatusBadRequest)
		return
	}

	id, name, err := s.spawnGardener(r.Context(), email, form)
	if err != nil {
		s.logger.Error("creating gardener trigger", "error", err)
		s.renderCreateGardenerError(w, r, "Failed to author gardener: "+err.Error(), http.StatusBadGateway)
		return
	}

	// The project panel's create form (Lands WB-11) asks for JSON: the new gardener, to open its panel.
	if wantsActionJSON(r) {
		writeActionJSON(w, actionResponse{OK: true, Bead: id, Agent: name})
		return
	}
	result := createGardenerResultView{
		BeadID:    id,
		AgentName: name,
		Project:   form.Project,
		EpicID:    form.EpicID,
		MentorID:  form.MentorID,
		Cron:      form.Cron,
		Preset:    form.Preset,
	}
	s.renderPartial(w, r, "templates/partials/create_gardener.html", "create_gardener_result", map[string]any{
		"Result": result,
	})
}

// parseGardenerCreateForm reads + validates the rich create form. Returns a
// populated form and "" on success, or a zero form and an inline error message.
func (s *Server) parseGardenerCreateForm(r *http.Request, email string) (gardenerCreateForm, string) {
	get := func(k string) string { return strings.TrimSpace(r.PostForm.Get(k)) }

	f := gardenerCreateForm{
		Project:         get("project"),
		EpicID:          get("epic_id"),
		MentorID:        get("mentor_id"),
		Prompt:          get("prompt"),
		Preset:          get("preset"),
		TerminalGoal:    get("terminal_goal"),
		ExitCondition:   get("exit_condition"),
		OnExit:          get("on_exit"),
		Cron:            get("cron"),
		Timezone:        get("timezone"),
		LoopWallClock:   get("loop_wall_clock"),
		RunTimeout:      get("run_timeout"),
		RetryOnFailure:  r.PostForm.Get("retry_on_failure") != "",
		OneSpawnAtATime: r.PostForm.Get("one_spawn_at_a_time") != "",
		Standing:        r.PostForm.Get("standing") != "",
	}

	if f.EpicID == "" {
		return f, "Epic id is required."
	}
	if f.Prompt == "" {
		return f, "Prompt is required — describe what the gardener should drive."
	}

	// Derive project from the epic when omitted.
	if f.Project == "" {
		if epic, err := s.daemon.GetBead(r.Context(), f.EpicID); err == nil && epic != nil {
			f.Project = beadProjectLabel(epic)
		}
	}
	if f.Project == "" {
		return f, "Project is required."
	}

	preset := findPreset(f.Preset)

	// Numeric knobs — empty falls back to the preset default, then a hard
	// floor. Negative / non-numeric is an inline error.
	var errMsg string
	f.MaxRuns, errMsg = parsePositiveIntDefault(get("max_runs"), preset.MaxRuns, "Max runs")
	if errMsg != "" {
		return f, errMsg
	}
	f.Cooldown, errMsg = parseNonNegIntDefault(get("cooldown_minutes"), preset.Cooldown, "Cooldown")
	if errMsg != "" {
		return f, errMsg
	}
	f.MaxConcurrent, errMsg = parseNonNegIntDefault(get("max_concurrent"), "1", "Max concurrent")
	if errMsg != "" {
		return f, errMsg
	}
	f.MaxRetries, errMsg = parseNonNegIntDefault(get("max_retries"), "0", "Max retries")
	if errMsg != "" {
		return f, errMsg
	}

	// Cron / wall-clock / goal fall back to the preset (or the staggered
	// default) when the operator left them blank.
	if f.Cron == "" {
		if preset.Cron != "" {
			f.Cron = preset.Cron
		} else {
			f.Cron = gardenermode.StaggeredCron(f.EpicID, 30*time.Minute)
		}
	}
	if f.Timezone == "" {
		f.Timezone = plangardener.DefaultTimezone
	}
	if f.LoopWallClock == "" {
		f.LoopWallClock = firstNonEmptyStr(preset.LoopWallClock, plangardener.DefaultLoopWallClock)
	}
	if f.RunTimeout == "" {
		f.RunTimeout = "30m"
	}
	if f.TerminalGoal == "" {
		f.TerminalGoal = strings.ReplaceAll(preset.GoalTemplate, "{epic}", f.EpicID)
	}
	if preset.Standing {
		f.Standing = true
	}

	// Validate durations so an engine-invalid value never persists.
	if _, err := time.ParseDuration(f.LoopWallClock); err != nil {
		return f, fmt.Sprintf("Loop wall-clock must be a Go duration (e.g. 168h); got %q.", f.LoopWallClock)
	}
	if _, err := time.ParseDuration(f.RunTimeout); err != nil {
		return f, fmt.Sprintf("Run timeout must be a Go duration (e.g. 30m); got %q.", f.RunTimeout)
	}
	return f, ""
}

// spawnGardener authors the gardener trigger bead: a `type=trigger` /
// `trigger:schedule` / `action=spawn_agent` / `role=gardener` bead carrying a
// bounded `gardener` loop blob (built via the shared plangardener builder so it
// is byte-identical to `gb gardener start`), a cron pre-flight on the epic's
// open children, and the operator's authored knobs. Labels, the epic
// `assigned` dependency, and the mentor mention/dep are applied best-effort.
func (s *Server) spawnGardener(ctx context.Context, email string, f gardenerCreateForm) (string, string, error) {
	// Stamp the operator so the kbeads audit resolver does not fail-close the
	// create, and so plangardener owner-resolution attributes the gardener.
	operator := canonicalOIDC(email)
	ctx = beadsapi.WithOperator(ctx, operator)

	agentName, err := generateAgentName(f.Project, f.Prompt)
	if err != nil {
		return "", "", fmt.Errorf("generating gardener name: %w", err)
	}

	// The gardener runs its iterations off this prompt. Embed the mentor
	// reference so each iteration reads its mentor's history at startup — the
	// literal `mentor(ka-X)` surface form is what the typed-mention parser
	// keys off for the constellation edge.
	spawnPrompt := f.Prompt
	if f.MentorID != "" {
		spawnPrompt = "Mentor: mentor(" + f.MentorID + "). Read the mentor's bead and recent " +
			"activity for context before starting.\n\n" + f.Prompt
	}

	// Build the bounded loop blob via the shared builder (same one gb gardener
	// start / plan-bind use): run_budget + a bead_query/max_runs/no_progress
	// loop_end + a consecutive_failures/wall_clock hard_stop + a progress spec.
	capBlob := plangardener.BuildGardenerLoopBlob(ctx, s.daemon, operator, plangardener.LoopBlobParams{
		EpicID:          f.EpicID,
		Project:         f.Project,
		Terminal:        f.TerminalGoal,
		MaxRuns:         f.MaxRuns,
		LoopWallClock:   f.LoopWallClock,
		OneSpawnAtATime: f.OneSpawnAtATime,
	})
	if capBlob == "" {
		return "", "", fmt.Errorf("building gardener loop blob")
	}

	// Overlay the extra operator knobs the shared builder does not carry
	// (exit spec, retry, run_timeout, standing marker) onto the blob.
	capBlob, err = overlayGardenerKnobs(capBlob, f)
	if err != nil {
		return "", "", fmt.Errorf("overlaying gardener knobs: %w", err)
	}

	// Fail-closed bounded-stop guard (advice ka-0000000974) — mirror gb
	// gardener start so the viewer can never persist an unbounded gardener.
	if v := bridge.ValidateGardenerBlob(capBlob); v != nil {
		return "", "", fmt.Errorf("gardener is not bounded: %s", v.Error())
	}

	// The cron pre-flight: fire only while the epic still has open children.
	// Once the subtree drains the trigger goes dormant (the bare-shape
	// analogue of the loop_end bead_query stop). Mirrors the exact shape gb
	// gardener start writes (scheduleConditionJSON in cmd/gb/trigger_schedule.go).
	conds, err := json.Marshal([]gardenerPreflightCondition{{
		Type:     "bead_query",
		Op:       ">",
		Value:    "0",
		Parent:   f.EpicID,
		Types:    []string{"task", "bug", "epic"},
		Statuses: []string{"open", "in_progress", "blocked"},
	}})
	if err != nil {
		return "", "", fmt.Errorf("marshalling pre-flight conditions: %w", err)
	}

	fields := map[string]any{
		"enabled":          true,
		"action":           "spawn_agent",
		"role":             "gardener",
		"project":          f.Project,
		"prompt":           spawnPrompt,
		"cron":             f.Cron,
		"timezone":         f.Timezone,
		"conditions":       string(conds),
		"cooldown_minutes": f.Cooldown,
		"max_concurrent":   f.MaxConcurrent,
		"gardener":         capBlob,
	}
	if f.MentorID != "" {
		fields["mentor_bead_id"] = f.MentorID
	}
	fieldsJSON, err := json.Marshal(fields)
	if err != nil {
		return "", "", fmt.Errorf("marshalling trigger fields: %w", err)
	}

	// Description: the typed mention `mentor(ka-X)` is what the bead_references
	// extractor (and the beads-viewer constellation) picks up as a typed edge.
	var desc strings.Builder
	desc.WriteString("Gardener for epic: ")
	desc.WriteString(f.EpicID)
	desc.WriteString("\n")
	if f.MentorID != "" {
		desc.WriteString("\nmentor: ")
		desc.WriteString(f.MentorID)
		desc.WriteString("\n")
	}
	desc.WriteString("\n")
	desc.WriteString(spawnPrompt)

	labels := []string{
		"trigger:schedule",
		"role:gardener",
		"project:" + f.Project,
		plangardener.GardenerEpicLabel(f.EpicID),
	}
	if f.Standing {
		labels = append(labels, "perpetual:true")
	}

	// Standing-marker guard (bug ka-0000004311): a goal that declares itself
	// perpetual MUST carry a machine-readable marker. We add perpetual:true
	// above when Standing, so the guard sees the same marker set that persists.
	if v := bridge.ValidateGardenerStandingBlob(capBlob, labels); v != nil {
		return "", "", fmt.Errorf("standing marker: %s", v.Error())
	}

	req := beadsapi.CreateBeadRequest{
		Title:       agentName,
		Type:        "trigger",
		Kind:        "data",
		Description: desc.String(),
		CreatedBy:   email,
		Labels:      labels,
		Fields:      fieldsJSON,
	}
	id, err := s.daemon.CreateBead(ctx, req)
	if err != nil {
		return "", "", fmt.Errorf("creating gardener trigger: %w", err)
	}

	// Best-effort edges — log and continue.
	if err := s.daemon.AddDependency(ctx, id, f.EpicID, "assigned", agentName); err != nil {
		s.logger.Warn("adding epic dependency", "gardener", agentName, "bead", id, "epic", f.EpicID, "error", err)
	}
	if f.MentorID != "" {
		if err := s.daemon.AddDependency(ctx, id, f.MentorID, "related", agentName); err != nil {
			s.logger.Warn("adding mentor dependency", "gardener", agentName, "bead", id, "mentor", f.MentorID, "error", err)
		}
	}
	return id, agentName, nil
}

// overlayGardenerKnobs decodes the shared-builder blob, applies the extra
// operator knobs the builder does not carry (name, exit spec, retry,
// run_timeout, standing), and re-encodes. Engine-owned runtime counters in the
// freshly-built blob are all zero, so a plain overlay is safe.
func overlayGardenerKnobs(capBlob string, f gardenerCreateForm) (string, error) {
	var spec bridge.GardenerSpec
	if err := json.Unmarshal([]byte(capBlob), &spec); err != nil {
		return "", err
	}
	spec.ExitCondition = f.ExitCondition
	spec.OnExit = f.OnExit
	spec.RetryOnFailure = f.RetryOnFailure
	spec.MaxRetries = f.MaxRetries
	spec.RunTimeout = f.RunTimeout
	spec.Standing = f.Standing
	raw, err := json.Marshal(spec)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// listGardenerMentors returns recent gardener agent beads for the mentor
// picker. Closed gardeners are included — gardener agent beads close themselves
// when their session ends (gb stop), so excluding closed would leave the
// dropdown almost always empty. The picker surfaces the status in the option
// label so operators can tell which mentors are dormant; in_progress and open
// gardeners are listed first so live mentors are easy to find.
func (s *Server) listGardenerMentors(ctx context.Context) []gardenerMentorOption {
	res, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:  []string{"agent"},
		Kinds:  []string{"config"},
		Labels: []string{"role:gardener"},
		Sort:   "updated_at",
		Limit:  100,
	})
	if err != nil {
		s.logger.Warn("listing gardener mentors", "error", err)
		return nil
	}
	if res == nil {
		return nil
	}
	// Phase 1: build the option list with TaskIDs but no titles. Cheap.
	out := make([]gardenerMentorOption, 0, len(res.Beads))
	taskIDs := make([]string, 0, len(res.Beads))
	for _, b := range res.Beads {
		if b == nil {
			continue
		}
		opt := gardenerMentorOption{
			BeadID:    b.ID,
			AgentName: b.Title,
			Status:    b.Status,
		}
		if tid := b.Fields["task_id"]; tid != "" {
			opt.TaskID = tid
			taskIDs = append(taskIDs, tid)
		}
		out = append(out, opt)
	}
	// Batch-fetch all mentor task titles in one round-trip (beadsapi.BatchGetBeads
	// shards internally at BatchGetBeadsMaxIDs, well above the mentor ceiling).
	// Missing tasks are simply absent from the map → TaskTitle stays empty,
	// preserving the previous per-mentor GetBead failure semantics. This
	// batch supersedes the bounded per-row GetBead fan-out from the original
	// perf branch — one (or two) round trips beats N parallel ones.
	if len(taskIDs) > 0 {
		tasksByID, err := s.daemon.BatchGetBeads(ctx, taskIDs)
		if err != nil {
			s.logger.Warn("batch get mentor task titles", "error", err)
		} else {
			for i := range out {
				if out[i].TaskID == "" {
					continue
				}
				if t, ok := tasksByID[out[i].TaskID]; ok && t != nil {
					out[i].TaskTitle = t.Title
				}
			}
		}
	}
	// Live mentors first, then dormant. Within each status group, preserve
	// the daemon's updated_at order (most-recently-active first).
	sort.SliceStable(out, func(i, j int) bool {
		return mentorStatusOrder(out[i].Status) < mentorStatusOrder(out[j].Status)
	})
	return out
}

func mentorStatusOrder(s string) int {
	switch s {
	case "in_progress":
		return 0
	case "open":
		return 1
	case "deferred":
		return 2
	case "closed":
		return 3
	}
	return 4
}

// renderCreateGardenerError re-renders the result partial with an inline
// error so the operator sees the failure and can correct + resubmit.
//
// We deliberately render at HTTP 200 instead of the underlying validation /
// upstream status. HTMX 2's default `responseHandling` does not swap on
// 4xx/5xx (`{code:"[45]..",swap:false,error:true}`), so writing the error
// at 4xx caused the form to silently appear to "do nothing" — the symptom
// reported in ka-0000005181 ("button doesn't seem to do anything"). The
// status arg is NOT written to the HTML response; a JSON caller (the project
// panel's create form) gets it with the message.
func (s *Server) renderCreateGardenerError(w http.ResponseWriter, r *http.Request, msg string, status int) {
	if respondAction(w, r, "", actionFailure(msg, status)) {
		return
	}
	s.renderPartial(w, r, "templates/partials/create_gardener.html", "create_gardener_result", map[string]any{
		"Result": createGardenerResultView{Error: msg},
	})
}

// parsePositiveIntDefault parses an optional positive int, falling back to
// def (a string) when empty. Returns (value, "") on success or (0, errMsg).
func parsePositiveIntDefault(s, def, label string) (int, string) {
	if s == "" {
		s = def
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0, label + " must be a positive whole number."
	}
	return n, ""
}

// parseNonNegIntDefault parses an optional non-negative int, falling back to
// def when empty. Returns (value, "") on success or (0, errMsg).
func parseNonNegIntDefault(s, def, label string) (int, string) {
	if s == "" {
		s = def
	}
	if s == "" {
		return 0, ""
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, label + " must be a non-negative whole number."
	}
	return n, ""
}

// firstNonEmptyStr returns the first non-empty string argument.
func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
