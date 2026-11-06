package gardenhub

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// The gardener panel (Lands workbench-glass spec WB-11, step 5.12, gd-au7ywvWAG8): one gardener loop (a trigger bead that
// spawns runs on a schedule or an event) as a Workbench kind (web/gardener.js, /garden?gardener=<id>), opened from a
// gardener bead's face, from the project panel's Gardeners tab and from every retired /gardener/{id} and /gardeners link
// (R9). Its tabs are the gardener page's sections: what it is (goal, when it wakes and stops, its counters and spend), its
// runs (the invocations, newest first, each with its wrap-up), the beads its runs touched (by state), its config, edited in
// place, and its successors. The reader is the viewer's own (cmd/beads-viewer gardener_garden.go: the gardener page's
// header, life-thread, invocation wrap-ups, bead walk and config form) on the request's bearer, so the daemon cuts the
// answer as it cuts the page. Every write is the viewer's own POST /gardener/{id}/* route asked for JSON: enable, disable,
// run-now, cancel-run, retire, mode, config and successor; a new gardener is POST /create-gardener, from the project panel.

// GardenerReader reads one gardener loop's panel for the request's viewer. An id that is no gardener the viewer may see
// answers a *beadsapi.APIError with StatusNotFound.
type GardenerReader interface {
	Gardener(ctx context.Context, id string) (*GardenerDoc, error)
}

const (
	gardenerTimeout  = 15 * time.Second
	gardenerRunsMax  = 25 // runs the panel is sent, newest first; the rest are counted
	gardenerLaneMax  = 30 // beads of one state the panel is sent
	gardenerTextMax  = 2000
	gardenerTitleMax = 400
)

// GardenerRun is one run (an invocation: an agent the gardener spawned), newest first.
type GardenerRun struct {
	ID    string `json:"id"`
	Name  string `json:"name,omitempty"`
	Label string `json:"label,omitempty"` // "run 12", or the time it was spawned when the run number is unknown
	// State is the agent's state (working, done, ...); Live: it is still running; DidNotRun: a queued spawn that expired
	// before it claimed a pod, never a run.
	State     string `json:"state,omitempty"`
	Live      bool   `json:"live,omitempty"`
	DidNotRun bool   `json:"did_not_run,omitempty"`
	// Outcome is "ok" or "fail" for a run that ended, "" while live.
	Outcome   string `json:"outcome,omitempty"`
	Headline  string `json:"headline,omitempty"` // its wrap-up's tl;dr, else what it worked on
	Cost      string `json:"cost,omitempty"`     // "$1.23", "" when none is reported
	Turns     string `json:"turns,omitempty"`
	Ago       string `json:"ago,omitempty"`
	TaskID    string `json:"task_id,omitempty"`
	TaskTitle string `json:"task_title,omitempty"`
}

// GardenerLane is the beads the gardener's runs touched in one state: in_progress, blocked, open or done.
type GardenerLane struct {
	Lane  string        `json:"lane"`
	Label string        `json:"label"`
	Count int           `json:"count"`
	Beads []ProjectBead `json:"beads"`
}

// GardenerConfig is the gardener's config as POST /gardener/{id}/config reads it: every value the field as stored (the
// stop, progress and conditions specs as JSON), so the form posts back what it was given. Sent only to an operator.
type GardenerConfig struct {
	Prompt          string `json:"prompt"`
	TerminalGoal    string `json:"terminal_goal"`
	ExitCondition   string `json:"exit_condition"`
	OnExit          string `json:"on_exit"`
	RunBudget       string `json:"run_budget"`
	MaxRuns         string `json:"max_runs"`
	RetryOnFailure  bool   `json:"retry_on_failure"`
	MaxRetries      string `json:"max_retries"`
	RunTimeout      string `json:"run_timeout"`
	Timeout         string `json:"timeout"`
	OneSpawnAtATime bool   `json:"one_spawn_at_a_time"`
	StopJSON        string `json:"stop_json"`
	ProgressJSON    string `json:"progress_json"`
	Conditions      string `json:"conditions"`
	Cron            string `json:"cron"`
	Cooldown        string `json:"cooldown_minutes"`
	MaxConcurrent   string `json:"max_concurrent"`
}

// GardenerDoc is one gardener loop as the panel needs it. A section the daemon did not answer is named in Partial, so the
// panel says so instead of showing it empty.
type GardenerDoc struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Name    string `json:"name,omitempty"`
	Project string `json:"project,omitempty"`
	Status  string `json:"status,omitempty"`
	Enabled bool   `json:"enabled"`
	Closed  bool   `json:"closed,omitempty"`
	// Standing: a perpetual gardener, whose run budget and max runs are accounting, not a ceiling.
	Standing bool `json:"standing,omitempty"`
	// Mode is how it wakes, "timed" (a cron) or "event" (a bead condition); ModeAmbiguous: its fields say both or neither,
	// so the mode cannot be flipped from here.
	Mode          string `json:"mode,omitempty"`
	ModeAmbiguous bool   `json:"mode_ambiguous,omitempty"`
	Goal          string `json:"goal,omitempty"`
	Prompt        string `json:"prompt,omitempty"`
	Chasing       string `json:"chasing,omitempty"`
	// Wakes and Stops are its firing and stop config in words; OnLoopEnd what it does when its loop ends.
	Wakes     []string `json:"wakes,omitempty"`
	Stops     []string `json:"stops,omitempty"`
	OnLoopEnd string   `json:"on_loop_end,omitempty"`
	Cron      string   `json:"cron,omitempty"`
	Timezone  string   `json:"timezone,omitempty"`
	NextFire  string   `json:"next_fire,omitempty"` // "in 12m"
	// GoalState is "achieved" or "unachieved" once its goal is decided, from its target epic now; Ended why it closed.
	GoalState string `json:"goal_state,omitempty"`
	Ended     string `json:"ended,omitempty"`
	// The engine's counters, and the runs the life-thread holds (Runs counts real ones; DidNotRun the expired spawns).
	RunsSpawned   int    `json:"runs_spawned"`
	RunsCompleted int    `json:"runs_completed"`
	MaxRuns       int    `json:"max_runs,omitempty"`
	RunBudget     int    `json:"run_budget,omitempty"`
	Failures      int    `json:"failures,omitempty"` // consecutive failed runs
	RunCount      int    `json:"run_count"`
	LiveCount     int    `json:"live_count,omitempty"`
	DidNotRun     int    `json:"did_not_run,omitempty"`
	EngineCount   string `json:"engine_count,omitempty"` // the engine's lifetime tally in words, beside the run count
	Spend         string `json:"spend,omitempty"`        // "$12.34" over its runs
	SpendStale    bool   `json:"spend_stale,omitempty"`
	// Overdue and Silent name a liveness worry in words: it should have fired and did not; it fires and makes no progress.
	Overdue string `json:"overdue,omitempty"`
	Silent  string `json:"silent,omitempty"`
	Owner   string `json:"owner,omitempty"`
	// InFlight: its last run may still be running (cancel-run has a target); LastRun is that run.
	InFlight bool   `json:"in_flight,omitempty"`
	LastRun  string `json:"last_run,omitempty"`
	// Successors are the gardeners it hands on to when it ends, in words.
	Successors []string       `json:"successors"`
	Runs       []GardenerRun  `json:"runs"`
	Beads      []GardenerLane `json:"beads"`
	// Config is nil for a viewer who may not operate it.
	Config *GardenerConfig `json:"config,omitempty"`
	// CanOperate: the viewer may pull its levers and edit it (the /gardener/{id}/* POSTs are operator-only).
	CanOperate bool `json:"can_operate,omitempty"`
	// More counts what was not sent: runs past gardenerRunsMax, a lane's beads past gardenerLaneMax.
	More map[string]int `json:"more,omitempty"`
	// Partial names the sections the daemon did not answer: runs, beads.
	Partial []string `json:"partial,omitempty"`
}

// gardenerPanel serves GET /garden/gardener/{id}.
func (h *Handler) gardenerPanel(w http.ResponseWriter, r *http.Request) {
	if h.Gardeners == nil {
		http.Error(w, "gardeners not enabled", http.StatusServiceUnavailable)
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" || len(id) > 200 {
		http.Error(w, "bad gardener id", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), gardenerTimeout)
	defer cancel()
	doc, err := h.Gardeners.Gardener(ctx, id)
	if err == nil && doc == nil {
		err = &beadsapi.APIError{StatusCode: http.StatusNotFound, Message: "no such gardener"}
	}
	if err != nil {
		var ae *beadsapi.APIError
		switch {
		case errors.As(err, &ae) && (ae.StatusCode == http.StatusNotFound || ae.StatusCode == http.StatusForbidden):
			http.Error(w, "no such gardener", http.StatusNotFound) // one the viewer may not see is not there
		default:
			http.Error(w, "the beads daemon did not answer", http.StatusBadGateway)
		}
		return
	}
	doc.CanOperate = h.can(r, h.CanOperateGardeners)
	finishGardener(doc)
	writeDoc(w, "kbeads", doc)
}

// finishGardener sends every list (empty, never null), clips the long texts, caps the runs and each lane, counting the
// rest, and keeps the config from a viewer who may not edit it, so every reader sends the same shape.
func finishGardener(d *GardenerDoc) {
	more := map[string]int{}
	d.Goal, d.Prompt = clipKeep(d.Goal, gardenerTextMax), clipKeep(d.Prompt, gardenerTextMax)
	if d.Successors == nil {
		d.Successors = []string{}
	}
	if d.Runs == nil {
		d.Runs = []GardenerRun{}
	}
	if len(d.Runs) > gardenerRunsMax {
		more["runs"] += len(d.Runs) - gardenerRunsMax
		d.Runs = d.Runs[:gardenerRunsMax]
	}
	for i := range d.Runs {
		d.Runs[i].Headline = clip(d.Runs[i].Headline, gardenerTitleMax)
	}
	if d.Beads == nil {
		d.Beads = []GardenerLane{}
	}
	for i := range d.Beads {
		l := &d.Beads[i]
		if l.Beads == nil {
			l.Beads = []ProjectBead{}
		}
		if len(l.Beads) > gardenerLaneMax {
			more[l.Lane] += len(l.Beads) - gardenerLaneMax
			l.Beads = l.Beads[:gardenerLaneMax]
		}
	}
	if !d.CanOperate {
		d.Config = nil
	}
	if len(more) > 0 {
		d.More = more
	} else {
		d.More = nil
	}
}
