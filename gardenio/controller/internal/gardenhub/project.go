package gardenhub

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/lands/purse"
)

// The project panel (Lands workbench-glass spec WB-10, step 5.11, gd-fT15vJniWQ): one project as a Workbench kind
// (web/project.js, /garden?project=<name>), opened from the project's sign on the map and every retired /p/{project},
// /active and /feed link (R5, R8). Its tabs are the project hub's sections: agents, gardeners, LLM accounts, cost and
// purse, activity (what is growing, the project's slice of /active, and what moved last, its slice of /feed), the open
// decisions and escalations; the project's config, edited in place; and, for the land's owner or an operator, Share: windows
// on the whole land (LW.5). The reader is the viewer's own (cmd/beads-viewer
// project_garden.go: the hub's section builders, the Queues panel's decision and escalation rows, the Lands purse) on
// the request's bearer, so the daemon cuts the answer as it cuts the hub. The plot's spend and budget come from the
// farm's cost blob, the one the map's header reads. Every write is the viewer's own POST route asked for JSON: the
// config's is POST /p/{project}/config; a decision is answered and an escalation approved through the Mailbox's answer
// box (web/answer.js).

// ProjectReader reads one project's panel for the request's viewer. A name that is no project the viewer may see answers
// a *beadsapi.APIError with StatusNotFound.
type ProjectReader interface {
	Project(ctx context.Context, name string) (*ProjectDoc, error)
}

const (
	projectTimeout = 15 * time.Second
	projectRowsMax = 60  // rows of one list the panel is sent; the rest are counted
	projectTextMax = 400 // runes of a decision's prompt in a row
)

// ProjectSecret is one secret the project's agents mount: an env var from a key of a Kubernetes Secret. Names only,
// never a value.
type ProjectSecret struct {
	Env    string `json:"env"`
	Secret string `json:"secret"`
	Key    string `json:"key"`
}

// ProjectFinding is one config-validity finding (the hub's badge): severity error or warning, the field it is on.
type ProjectFinding struct {
	Field    string `json:"field,omitempty"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
}

// ProjectConfig is the project's type:project bead as the config tab shows and edits it. Every value is the field as
// stored, so the form posts back what it was given.
type ProjectConfig struct {
	BeadID           string           `json:"bead_id"`
	Status           string           `json:"status,omitempty"`
	GitURL           string           `json:"git_url,omitempty"`
	DefaultBranch    string           `json:"default_branch,omitempty"`
	Image            string           `json:"image,omitempty"`
	Account          string           `json:"account,omitempty"`
	Model            string           `json:"model,omitempty"`
	CPURequest       string           `json:"cpu_request,omitempty"`
	CPULimit         string           `json:"cpu_limit,omitempty"`
	MemoryRequest    string           `json:"memory_request,omitempty"`
	MemoryLimit      string           `json:"memory_limit,omitempty"`
	StorageClass     string           `json:"storage_class,omitempty"`
	CardTheme        string           `json:"card_theme,omitempty"`
	BudgetUSD        string           `json:"budget_usd,omitempty"`
	BudgetPeriod     string           `json:"budget_period,omitempty"`
	Docker           bool             `json:"docker"`
	Postgres         bool             `json:"postgres"`
	NeedsK8sAPI      bool             `json:"needs_k8s_api"`
	SchedulesEnabled bool             `json:"schedules_enabled"`
	TriggersEnabled  bool             `json:"triggers_enabled"`
	Secrets          []ProjectSecret  `json:"secrets,omitempty"` // sent only to a viewer who may edit the config
	Findings         []ProjectFinding `json:"findings,omitempty"`
}

// GardenerPreset is one way to start a gardener (the create form's presets: an epic driver, a ready-queue drainer, ...);
// each seeds the knobs the form leaves blank.
type GardenerPreset struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// ProjectAgent is one of the project's active agents.
type ProjectAgent struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Role      string `json:"role,omitempty"`
	State     string `json:"state,omitempty"` // agent_state: working, idle, done, ...
	Status    string `json:"status,omitempty"`
	TaskID    string `json:"task_id,omitempty"`
	TaskTitle string `json:"task_title,omitempty"`
	Ago       string `json:"ago,omitempty"` // last update, in words ("3m")
}

// ProjectGardener is one of the project's live gardeners (a trigger or a schedule).
type ProjectGardener struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Kind    string `json:"kind,omitempty"`   // trigger, schedule
	Status  string `json:"status,omitempty"` // active, disabled
	Enabled bool   `json:"enabled"`
	Goal    string `json:"goal,omitempty"`
	Runs    int    `json:"runs"`
}

// ProjectAccount is one LLM account the project may spend through, with its LiteLLM spend.
type ProjectAccount struct {
	Name     string   `json:"name"`
	Provider string   `json:"provider,omitempty"`
	Model    string   `json:"model,omitempty"`
	Status   string   `json:"status,omitempty"`
	TodayUSD *float64 `json:"today_usd"`
	WeekUSD  *float64 `json:"week_usd"`
	TotalUSD *float64 `json:"total_usd"`
	// Shared: another account here bills the same key, so its dollars are counted there, not twice.
	Shared bool `json:"shared,omitempty"`
}

// ProjectSpend is the project's spend summed over its accounts (shared keys once).
type ProjectSpend struct {
	TodayUSD float64 `json:"today_usd"`
	WeekUSD  float64 `json:"week_usd"`
	TotalUSD float64 `json:"total_usd"`
	Has      bool    `json:"has"`
}

// ProjectBead is one bead in an activity or a blocked list.
type ProjectBead struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Type   string `json:"type,omitempty"`
	Status string `json:"status,omitempty"`
	Ago    string `json:"ago,omitempty"`
}

// ProjectPerson is one person of a land: oidc:<email>, as conversation beads name a person, and their user bead's title.
type ProjectPerson struct {
	Principal string `json:"principal"`
	Name      string `json:"name,omitempty"`
}

// ProjectDoc is one project as the panel needs it. A list the daemon did not answer is named in Partial, so the panel
// says so instead of showing it empty.
type ProjectDoc struct {
	Name string `json:"name"`
	// Config is nil for a label-only project (no type:project bead): there is nothing to show or edit.
	Config *ProjectConfig `json:"config,omitempty"`
	// CanEdit: the viewer may save the config (POST /p/{project}/config).
	CanEdit   bool              `json:"can_edit,omitempty"`
	Agents    []ProjectAgent    `json:"agents"`
	Gardeners []ProjectGardener `json:"gardeners"`
	// CanCreateGardener: the viewer may start a gardener here (POST /create-gardener), from GardenerPresets.
	CanCreateGardener bool             `json:"can_create_gardener,omitempty"`
	GardenerPresets   []GardenerPreset `json:"gardener_presets,omitempty"`
	SchedulesPaused   bool             `json:"schedules_paused,omitempty"`
	TriggersPaused    bool             `json:"triggers_paused,omitempty"`
	Accounts          []ProjectAccount `json:"accounts"`
	Spend             *ProjectSpend    `json:"spend,omitempty"`
	// Plot is the project's row of the farm's cost blob: the spend since midnight and over the week from the costs
	// rollup, and the budget and what is left of it, as the map's header shows them. Nil without a spend source.
	Plot *FarmCostProject `json:"plot,omitempty"`
	// Purse is a tenant land's purse read now (Lands 8.2): credits granted, spent, in flight and left, and its state.
	// Absent on an operator land.
	Purse *purse.Purse `json:"purse,omitempty"`
	// Owner is a tenant land's owner (the land's members with role owner, as of their last sign-in), named on the
	// header's owner row, where an admin is offered Become (web/become.js; plan gd-0SUIdKXWV3 step 3). Nil on an operator
	// land, and when the daemon did not say.
	Owner *ProjectPerson `json:"owner,omitempty"`
	// Growing is what is in progress (the project's /active); Recent what moved last, any state (its /feed).
	Growing []ProjectBead `json:"growing"`
	Recent  []ProjectBead `json:"recent"`
	// Decisions and Escalations are the open ones of the project, as the Queues panel's rows (with a decision's options,
	// for the answer box); Blocked the beads waiting on one.
	Decisions   []QueueRow    `json:"decisions"`
	Escalations []QueueRow    `json:"escalations"`
	Blocked     []ProjectBead `json:"blocked"`
	// Approve: the viewer may approve an escalation (the answer box offers it).
	Approve bool `json:"approve,omitempty"`
	// CanShare: the viewer may mint, preview and revoke windows on the whole land (Lands: the land window, LW.5): the
	// panel's Share tab, which asks the viewer's GET /p/{project}/share for its windows (beads-viewer land_share.go). The
	// land's owner or an operator; kbeads checks again on every call.
	CanShare bool `json:"can_share,omitempty"`
	// CredentialPool: the header offers the credential pool (GL-15, glass_pool.go). The pool is fleet-wide, so it opens
	// from an operator land only (not a tenant's), and only for a viewer who may read it (an admin).
	CredentialPool bool `json:"credential_pool,omitempty"`
	// More counts the rows past projectRowsMax, per list.
	More map[string]int `json:"more,omitempty"`
	// Partial names the lists the daemon did not answer: agents, gardeners, accounts, purse, activity, decisions.
	Partial []string `json:"partial,omitempty"`
}

// projectView serves GET /garden/project/{name}.
func (h *Handler) projectView(w http.ResponseWriter, r *http.Request) {
	if h.Projects == nil {
		http.Error(w, "projects not enabled", http.StatusServiceUnavailable)
		return
	}
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" || len(name) > 200 {
		http.Error(w, "bad project name", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), projectTimeout)
	defer cancel()
	doc, err := h.Projects.Project(ctx, name)
	if err == nil && doc == nil {
		err = &beadsapi.APIError{StatusCode: http.StatusNotFound, Message: "no such project"}
	}
	if err != nil {
		var ae *beadsapi.APIError
		switch {
		case errors.As(err, &ae) && (ae.StatusCode == http.StatusNotFound || ae.StatusCode == http.StatusForbidden):
			http.Error(w, "no such project", http.StatusNotFound) // one the viewer may not see is not there
		default:
			http.Error(w, "the beads daemon did not answer", http.StatusBadGateway)
		}
		return
	}
	if doc.Plot == nil && h.Farm != nil {
		doc.Plot = h.Farm.ProjectCost(doc.Name)
	}
	doc.Approve = h.can(r, h.CanApproveEscalation)
	doc.CredentialPool = doc.Purse == nil && (h.Farm == nil || !h.Farm.IsTenantLand(doc.Name)) && h.mayReadPool(r)
	doc.CanCreateGardener = h.can(r, h.CanOperateGardeners) && len(doc.GardenerPresets) > 0
	if h.readOnly(r) {
		doc.CanEdit = false // the config editor too; finishProject then keeps the secrets back
	}
	if !doc.CanCreateGardener {
		doc.GardenerPresets = nil
	}
	finishProject(doc)
	writeTagged(w, r, "kbeads", doc)
}

// finishProject sends every list (empty, never null), clips a decision's prompt and caps each list, counting the rest,
// so every reader sends the same shape. The open asks go in the Queues panel's attention order.
func finishProject(d *ProjectDoc) {
	more := map[string]int{}
	d.Agents = capList(d.Agents, "agents", more)
	d.Gardeners = capList(d.Gardeners, "gardeners", more)
	d.Accounts = capList(d.Accounts, "accounts", more)
	d.Growing = capList(d.Growing, "growing", more)
	d.Recent = capList(d.Recent, "recent", more)
	d.Blocked = capList(d.Blocked, "blocked", more)
	for _, rows := range []*[]QueueRow{&d.Decisions, &d.Escalations} {
		for i := range *rows {
			(*rows)[i].Text = clip((*rows)[i].Text, projectTextMax)
		}
		sortQueue(*rows)
	}
	d.Decisions = capList(d.Decisions, "decisions", more)
	d.Escalations = capList(d.Escalations, "escalations", more)
	if d.Config != nil && !d.CanEdit {
		d.Config.Secrets = nil
	}
	if len(more) > 0 {
		d.More = more
	} else {
		d.More = nil
	}
}

// capList is rows, never nil, cut to projectRowsMax with the rest counted in more[key].
func capList[T any](rows []T, key string, more map[string]int) []T {
	if rows == nil {
		return []T{}
	}
	if len(rows) > projectRowsMax {
		more[key] += len(rows) - projectRowsMax
		return rows[:projectRowsMax]
	}
	return rows
}
