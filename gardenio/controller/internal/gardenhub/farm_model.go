package gardenhub

// farm_model.go — the wire schema of the Homestead farm read model
// (gd-L9fjdMGV7P). Every type here is part of the contract the art track builds
// against; the prose version, with a worked example, is
// gardenio/docs/homestead-farm-read-model.md. Change the two together, and bump
// FarmSchema on any breaking change.

import "gardenio/controller/internal/lands/purse"

// FarmSchema is the version of the JSON shape below.
const FarmSchema = 1

// Task states, in the order the farm draws them (in-flight first).
const (
	TaskInProgress = "in_progress"
	TaskReview     = "review" // in flight AND a merge request is linked and not yet merged/closed
	TaskBlocked    = "blocked"
	TaskOpen       = "open"
	TaskDeferred   = "deferred"
	TaskClosed     = "closed" // only in FarmPhase.Closed: a closed step the bed shows as a cursor stop
)

// Seed states. A seed is a plan that has not been approved to build yet.
const (
	SeedFrosted = "frosted" // plan_state=draft: still being authored
	SeedReady   = "ready"   // plan_state=ready: waiting for "Approve & build"
	SeedAsked   = "asked"   // an open decision blocks the plan: waiting on its owner's answer (FarmSeed.Decision)
)

// FarmMR is the merge request linked to a task by `gb mr link` (the task bead
// carries mr_url / mr_iid / mr_state fields). No daemon call is needed to read it.
type FarmMR struct {
	URL   string `json:"url"`
	IID   string `json:"iid,omitempty"`
	State string `json:"state,omitempty"` // raw mr_state: opened, draft, merged, closed, ...
}

// FarmTask is one plant: a non-closed work bead. Closed beads are never tasks;
// they are counted (FarmCounts.Closed) and the newest listed as FarmClosedStep
// (FarmPhase.Closed).
type FarmTask struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// ShortTitle is the bead's short title (at most 40 runes), trimmed; omitted
	// when it has none. A label where space is tight reads it before Title
	// (web/titles.js TITLES.name).
	ShortTitle string `json:"short_title,omitempty"`
	Type       string `json:"type"`  // task, feature, bug, chore, decision, escalation, epic, molecule
	State      string `json:"state"` // Task* constants
	Priority   int    `json:"priority"`
	Project    string `json:"project,omitempty"`
	Assignee   string `json:"assignee,omitempty"`
	Updated    int64  `json:"updated"` // unix seconds
	// Created is created_at (unix seconds). The bed plants its tasks and listed
	// closed steps by it, then id, so a step keeps its cell through every
	// status, priority and updated_at change (gd-wDdun2AVwq).
	Created int64   `json:"created"`
	MR      *FarmMR `json:"mr,omitempty"`
	// Heraldry is the bead's arms in the canonical stored form ("blue 🌱 . 🔥 .
	// 📦", heraldry spec §4.1), omitted when it bears none. Opaque to the farm:
	// the client fetches the picture from the renderer by it, never prints it.
	Heraldry string `json:"heraldry,omitempty"`
	// BlockedBy lists open beads this task has a `blocks` dependency on (the
	// "rocks"/"ditches" edges). It only names beads that are still open, so an
	// edge disappears the moment its blocker closes. Capped at maxBlockedBy.
	BlockedBy []string `json:"blocked_by,omitempty"`
	// BlockedByProject maps each id in BlockedBy whose project label differs
	// from this task's to that label ("" for a blocker with none). A blocker it
	// does not name shares the task's project. It lets a client draw a
	// cross-project edge to a blocker the document does not carry (gd-CNhEh30Ykf).
	BlockedByProject map[string]string `json:"blocked_by_project,omitempty"`
	// BlockedByForeign is set only in a project-scoped viewer's cut (farm_cut.go, gd-U2pbNv3lLC): the projects of the
	// blockers the cut removed from BlockedBy because the viewer cannot read them, distinct and sorted, "" for a blocker
	// with no project: label ("another project"). It names the project only, never a bead, so the client can draw a
	// bridge off the viewer's region labelled with whom to ask.
	BlockedByForeign []string `json:"blocked_by_foreign,omitempty"`
	// Parent is set on meadow beads only: the bead's parent when it has one that
	// is not part of any plan (a loose bead with no parent leaves it empty).
	Parent string `json:"parent,omitempty"`
	// Op and Family are an escalation's, as on its FarmWait, so its plot draws
	// the same family sprite as the Escalations cell; Wait is its Wait* state (pending,
	// approved, rejected or failed) while it is open. All three are omitted on
	// any other type, and Wait on a closed escalation.
	Op     string `json:"op,omitempty"`
	Family string `json:"family,omitempty"`
	Wait   string `json:"wait,omitempty"`
}

// FarmClosedStep is one listed closed step (FarmPhase.Closed): a cursor stop on
// the bed, so it carries only what the cell, its tooltip and its status line
// show. Priority, project and the MR link are left out (gd-8fjCFsoAKI); the bead
// view reads them from the bead itself.
type FarmClosedStep struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	ShortTitle string `json:"short_title,omitempty"` // FarmTask.ShortTitle
	Type       string `json:"type"`
	State      string `json:"state"` // always TaskClosed
	Assignee   string `json:"assignee,omitempty"`
	Updated    int64  `json:"updated"` // unix seconds
	Created    int64  `json:"created"` // created_at, unix seconds: its place in the bed (FarmTask.Created)
	// Harvest is set only on the plan's maxHarvestPerPlan newest closed steps:
	// what the step yielded (farm_harvest.go, harvest spec §3).
	Harvest *FarmHarvest `json:"harvest,omitempty"`
}

// FarmHarvest is what a closed step yielded: the facts its cell's base sprite
// and badges are drawn from (harvest spec §3.1, §4). Zero and empty facts are
// left out, so a bare step with nothing recorded is {"closed":...}.
type FarmHarvest struct {
	Closed int64  `json:"closed"`           // closed_at, unix seconds
	Base   string `json:"base,omitempty"`   // HarvestSheaf or HarvestTrampled; absent for bare stubble
	MR     string `json:"mr,omitempty"`     // the merged MR's number: #825 on GitHub, !825 on GitLab
	UX     []int  `json:"ux,omitempty"`     // the newest UX grade: [score, of] (gates passed, gates graded)
	Pics   int    `json:"pics,omitempty"`   // that grade's picture links plus goldens_touched
	Tests  int    `json:"tests,omitempty"`  // tests_touched
	CI     string `json:"ci,omitempty"`     // ci_state at merge; only "success" lights the lantern
	Rounds int    `json:"rounds,omitempty"` // review_rounds: 1 is merged first time
}

// FarmCounts is an exact tally. It counts every non-closed row the hub read,
// including tasks omitted from FarmPhase.Tasks by the per-phase cap.
type FarmCounts struct {
	InProgress int `json:"in_progress"`
	Review     int `json:"review"`
	Blocked    int `json:"blocked"`
	Open       int `json:"open"`
	Deferred   int `json:"deferred"`
	// Closed counts DIRECT closed children only (a count-only pile; the beads
	// themselves are never sent).
	Closed int `json:"closed"`
}

// FarmPhase is one row of a plan's plot. A phase is a container listed in the
// plan's phase_order, so a closed phase still appears (as a count-only pile).
type FarmPhase struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// ShortTitle is the phase container's short title (FarmTask.ShortTitle);
	// omitted when it has none, and always on Unphased.
	ShortTitle string     `json:"short_title,omitempty"`
	Status     string     `json:"status"` // bead status of the phase container
	Counts     FarmCounts `json:"counts"`
	// Tasks lists at most maxPhaseTasks non-closed tasks. The cap keeps the
	// in-flight ones (sortTasks); the listed ones are then ordered by created,
	// then id (sortPlanted), the order the bed plants them in, so a status
	// change never moves a cell (gd-wDdun2AVwq).
	Tasks []FarmTask `json:"tasks"`
	More  int        `json:"more,omitempty"` // non-closed tasks omitted by the cap
	// Heraldry is the phase container's arms in the canonical form, omitted
	// when it bears none (and always on Unphased, which has no bead).
	Heraldry string `json:"heraldry,omitempty"`
	// HeraldryName is those arms in plain words (heraldry.Arms.PlainName,
	// spec §8), the name the phase's stake says aloud; omitted with Heraldry
	// and when the form does not parse.
	HeraldryName string `json:"heraldry_name,omitempty"`
	// Closed lists the phase's closed steps, newest first, only as many as the
	// bed has cells left after Tasks and the "+N" crate (bedCells), so the
	// cursor can land on a closed step and open it (gd-0Fj6E2vPql). A plan lists
	// at most maxClosedPerPlan and the document at most maxClosedListed, newest
	// first (gd-8fjCFsoAKI). Counts.Closed is still the whole number; the rest
	// are count-only stubble. Omitted when the slow tier has not read them
	// (farmClosedRowCap).
	Closed []FarmClosedStep `json:"closed,omitempty"`
}

// FarmPlan is a plan epic that is being built (plan_state building, complete
// but still open, abandoned but still open, or unset).
type FarmPlan struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// ShortTitle is the plan epic's short title (FarmTask.ShortTitle); omitted
	// when it has none.
	ShortTitle string `json:"short_title,omitempty"`
	State      string `json:"state"`  // raw plan_state, "" when unset
	Status     string `json:"status"` // bead status of the plan epic
	Project    string `json:"project,omitempty"`
	Operator   string `json:"operator,omitempty"` // lowercased operated_by alias
	Owner      string `json:"owner,omitempty"`
	Priority   int    `json:"priority"`
	Created    int64  `json:"created"`
	Updated    int64  `json:"updated"`  // the epic's own updated_at
	Activity   int64  `json:"activity"` // newest updated_at across the epic and its open tasks
	// Heraldry is the plan epic's arms in the canonical form, omitted when it
	// bears none (FarmTask.Heraldry).
	Heraldry string `json:"heraldry,omitempty"`
	// HeraldryName is those arms in plain words (FarmPhase.HeraldryName), the
	// plot's accessible name for the picture at its gate.
	HeraldryName string `json:"heraldry_name,omitempty"`
	// Phases are in phase_order. Unphased holds tasks that reach the plan
	// without passing through a phase (the "Other" row).
	Phases   []FarmPhase `json:"phases"`
	Unphased FarmPhase   `json:"unphased"`
	Counts   FarmCounts  `json:"counts"` // sum over Phases and Unphased
	// OpenDecisions / OpenEscalations are open type=decision / type=escalation
	// tasks in the plan (also present in Tasks as signposts / chests).
	OpenDecisions   int `json:"open_decisions"`
	OpenEscalations int `json:"open_escalations"`
	// NeedsHuman counts the open decisions and escalations that wait on a human
	// (FarmWait.Human): only these are "needs you". An escalation the owner
	// already approved is open but waits on its executor, so it is not counted,
	// unless that wait falls to the operator (a local or manual run, or no live
	// party left): then it is a human wait again (gd-g7jfH27efD).
	NeedsHuman int `json:"needs_human"`
	// Waiting is every open decision and escalation in the plan, saying what it
	// waits on: human waits first, then oldest first. Capped at maxWaiting; the
	// exact totals are OpenDecisions + OpenEscalations and NeedsHuman.
	Waiting []FarmWait `json:"waiting,omitempty"`
	// Produce is what the plan produced, or is about to (the produce bin,
	// gd-HqcG2zcwnJ): merged and open merge requests on its steps, closed ones
	// included, and the bundle and report beads under it. Not-ready items
	// first, then newest first; at most maxProduce listed, ProduceMore counts
	// the rest. Read in the slow tier (farm_produce.go). Omitted when empty.
	Produce     []FarmProduce `json:"produce,omitempty"`
	ProduceMore int           `json:"produce_more,omitempty"`
	// Cost is what the plan's crew have spent, from the controller's rollup on
	// the plan epic (internal/planspend, gd-kcy4IFNzv5): the same crew and the
	// same per-agent figure as the /plans crew card.
	Cost FarmPlanCost `json:"cost"`
	// Shipped is the tag of the release that carries the plan's newest merged
	// PR, among each project's newest maxReleasesPerProject releases and
	// whatever their status (farm_releases.go); omitted when no release
	// carries it yet. The surfaces read that release's status and targets
	// from releases.
	Shipped string `json:"shipped,omitempty"`
	// Boarded is true when the plan's newest merged PR is on its project's
	// open release instead: merged, not yet cut (Shipped is then empty).
	Boarded bool `json:"boarded,omitempty"`
}

// FarmPlanCost is a plan's crew spend as the controller last rolled it up.
// Like FarmCostProject, a figure that is not known is null, never 0.
type FarmPlanCost struct {
	// Has is false until the controller has written a figure on the plan
	// epic; USD is then null and the rest is zero.
	Has bool     `json:"has"`
	USD *float64 `json:"usd"` // US dollars, rounded to the cent; null when !has
	// SyncedAt is when the controller wrote the figure (unix seconds). A plan
	// whose figure has not changed keeps its old stamp: see Stale.
	SyncedAt int64 `json:"synced_at,omitempty"`
	Crew     int   `json:"crew"`           // crew agent beads summed
	Live     int   `json:"live,omitempty"` // of which were live at SyncedAt
	// Stale is true when the plan had live crew at SyncedAt (so its figure was
	// still moving) and the controller has written no plan in this build for
	// three rollup intervals (planspend.ReadWithPass): the rollup has stopped,
	// and the figure is a lower bound. A figure with no
	// live crew is settled and never stale.
	Stale bool `json:"stale,omitempty"`
}

// FarmSlimCost is the cost a seed or a closed plan carries: the figure and its
// gardener count, so the cockpit says what the /plans crew card says.
type FarmSlimCost struct {
	Has  bool     `json:"has"`
	USD  *float64 `json:"usd"`            // null when !has
	Crew int      `json:"crew,omitempty"` // crew agent beads summed
}

// FarmProduce is one item in a plan's produce bin.
type FarmProduce struct {
	// ID is the bead the item stands for: the step for a merge request (its
	// bead view carries the MR link), the bundle or report bead itself.
	ID    string `json:"id"`
	Kind  string `json:"kind"` // Produce* constants: mr, bundle, report
	Title string `json:"title"`
	URL   string `json:"url,omitempty"` // mr: the merge request's URL
	IID   string `json:"iid,omitempty"` // mr: "!412" / "#412" when known
	// State is the raw mr_state for an MR (merged, opened, draft; an MR closed
	// unmerged is never produce) and the bead status for a bundle or report.
	State string `json:"state"`
	// Ready says the item is in the bin: a merged MR, an existing document. An
	// unmerged MR is expected produce and is shown grayed, ahead of the merged
	// ones (the bin's order is by kind first: reports, bundles, then MRs).
	Ready bool `json:"ready"`
	// Task is the step the item hangs off, the one to zoom the plot to: the MR's
	// own step; a document's nearest step ancestor, "" when it sits directly
	// under the plan or a phase. Phase is that step's phase ("" for unphased).
	Task    string `json:"task,omitempty"`
	Phase   string `json:"phase,omitempty"`
	Updated int64  `json:"updated"` // unix seconds
}

// Wait states of an open decision or escalation (FarmWait.State). They are read
// from the fields kbeads writes (kbeads/internal/server/http_decisions.go,
// http_escalations.go), never guessed from the status alone.
const (
	WaitUnanswered = "unanswered" // decision with no answer: waits on a human
	WaitAnswered   = "answered"   // decision answered but not yet closed: waits on the agent that asked
	WaitPending    = "pending"    // escalation not yet approved: waits on a human
	WaitApproved   = "approved"   // escalation approved, not yet closed: waits on its executor or the agent that asked
	WaitRejected   = "rejected"   // escalation rejected but still open
	WaitFailed     = "failed"     // escalation ran and failed but is still open
)

// What FarmWait.WaitsOn says when it does not name an agent (gd-g7jfH27efD).
const (
	WaitsOnOperator     = "the operator"
	WaitsOnDesk         = "the merge desk"          // a desk-audience escalation (gd-bmOiteQi36)
	WaitsOnOperators    = "the operators"           // an operator-audience one
	WaitsOnExecutor     = "its executor"            // a live synthetic "escalation-<id>" executor
	WaitsOnExecutorDone = "its executor (finished)" // a finished one, when nothing better is known
)

// FarmWait is one open decision or escalation and who it waits on.
type FarmWait struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Kind  string `json:"kind"`  // decision | escalation
	State string `json:"state"` // Wait* constants
	// Held names the agent lane holding a non-floor escalation (AgentLaneHolder): its cell's answer box offers
	// only "Approve anyway" (gd-Mq0uDXHswK).
	Held string `json:"held,omitempty"`
	// Human is true when the item waits on a human: WaitUnanswered, WaitPending,
	// or any other state whose WaitsOn is WaitsOnOperator.
	Human bool `json:"human"`
	// WaitsOn names the live party a wait is on: an agent name (resolved from an
	// agent bead id where the bead carries one), WaitsOnExecutor or
	// WaitsOnExecutorDone for a synthetic "escalation-<id>" executor, or
	// WaitsOnOperator. Empty for an unanswered decision or a pending escalation
	// (the human is implied), or when the bead names nobody.
	WaitsOn string `json:"waits_on,omitempty"`
	// Due is when the operator is expected to act (unix seconds, from
	// fields.due_at), set only on a wait that falls to the operator.
	Due int64 `json:"due,omitempty"`
	// Since is when the current wait began (unix seconds): approved_at for an
	// approved escalation, responded_at for an answered decision, else created_at.
	Since int64 `json:"since"`
	// Project is the bead's project: label, "" when it has none (an
	// unprojected escalation: the Commons strip's). Set on an escalation only.
	Project string `json:"project,omitempty"`
	// Op is an escalation's fields.operation_type verbatim, "" when unset;
	// Family is its normalised kind (Esc* constants, EscalationFamily), never
	// empty on an escalation. Both are omitted on a decision.
	Op     string `json:"op,omitempty"`
	Family string `json:"family,omitempty"`
	// Executor names the gardener working an escalation: the live agent whose
	// fields.escalation_id is this escalation (the join that stands it on the
	// plot, FarmGardener.Escalation). Omitted when none is.
	Executor string `json:"executor,omitempty"`
}

// Escalation families (FarmWait.Family): what kind of thing an escalation
// asks for, one sprite and one noun each (farm_escalations.go).
const (
	EscMerge  = "merge"
	EscDeploy = "deploy"
	EscConfig = "config"
	EscPrompt = "prompt"
	EscBeads  = "beads"
	EscAccess = "access"
	EscInfra  = "infra"
	EscRead   = "read"
	EscOther  = "other"
)

// FarmEscalations is every open escalation the farm read, fleet-wide
// (pending, approved, rejected or failed but still open; closed never), each a
// FarmWait carrying its project, op, family and executor: the strips'
// escalations. Open is ordered as FarmPlan.Waiting (human waits first, then
// the longest wait first) and capped at maxEscalations; Total is exact for the
// rows read, and ByProject is it per project ("" the unprojected ones), so a
// home strip's "+N" crate counts the ones Open's cap left out (gd-4FIF5pzC6O).
type FarmEscalations struct {
	Open      []FarmWait     `json:"open"`
	Total     int            `json:"total"`
	ByProject map[string]int `json:"by_project"`
}

// FarmSeed is a draft or ready plan: slim on purpose (counts, no task bodies) so
// 20+ drafts stay cheap. A ready or asked seed also carries its steps and
// summary, so its pot can be read before it is approved.
type FarmSeed struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	State    string `json:"state"` // Seed* constants
	Status   string `json:"status"`
	Project  string `json:"project,omitempty"`
	Operator string `json:"operator,omitempty"`
	Priority int    `json:"priority"`
	Created  int64  `json:"created"`
	Updated  int64  `json:"updated"`
	Phases   int    `json:"phases"`
	Tasks    int    `json:"tasks"` // non-closed tasks authored so far
	// Decision is the open decision that names this plan (requesting_plan, else
	// requesting_task): the steward's "Plan <id>: <title> - run it?"
	// (steward.WritePlan) the plan is blocked on. The pot's card answers it
	// through the Mailbox's own POST (web/answer.js). Nil when none is open;
	// State is SeedAsked while one is.
	Decision *FarmSeedAsk `json:"decision,omitempty"`
	// Steps are a ready or asked seed's open steps in step_order (else by
	// created), at most maxSeedSteps; none on a frosted seed.
	Steps []FarmSeedStep `json:"steps,omitempty"`
	// Summary is a ready or asked seed's description, cut at seedSummaryMax
	// runes; empty on a frosted seed, or while it could not be read.
	Summary string `json:"summary,omitempty"`
	// ShortTitle is a ready or asked seed's short title (at most 40 runes),
	// which its staked plot's sign reads; empty on a frosted seed, or when the
	// plan has none.
	ShortTitle string `json:"short_title,omitempty"`
	// Cost is the crew spend so far (FarmPlanCost.USD); a seed may have had a
	// planning crew.
	Cost FarmSlimCost `json:"cost"`
}

// FarmSeedStep is one step of a ready or asked seed: what a stake names.
type FarmSeedStep struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	N        int    `json:"n"`      // 1-based, its place in the seed's steps
	Status   string `json:"status"` // the bead's status
	Heraldry string `json:"heraldry,omitempty"`
}

// FarmSeedAsk is a seed's open decision, with what its card needs: the
// question and the options to answer with.
type FarmSeedAsk struct {
	ID string `json:"id"`
	// Prompt is the question without the "Plan <id>: <title> - " the pot
	// already says ("run it?"); a prompt of another shape is kept whole.
	Prompt  string        `json:"prompt"`
	Options []QueueOption `json:"options,omitempty"` // empty when they could not be read: the card offers a written answer
	For     string        `json:"for,omitempty"`     // whom it asks: the decision's operated_by, lower-cased
	Since   int64         `json:"since"`             // created_at, unix seconds
}

// FarmClosedPlan is a finished plan, shown as a slim tile.
type FarmClosedPlan struct {
	ID      string       `json:"id"`
	Title   string       `json:"title"`
	State   string       `json:"state"`
	Project string       `json:"project,omitempty"`
	Updated int64        `json:"updated"`
	Cost    FarmSlimCost `json:"cost"` // what its crew spent, rolled up once more at close
}

// FarmClosedPlans is the newest finished plans plus the exact total.
type FarmClosedPlans struct {
	Total int              `json:"total"`
	Shown []FarmClosedPlan `json:"shown"`
}

// FarmMeadow is the loose beads: work beads that sit under no plan. Total is
// exact for the rows the hub read (see FarmMeta.Truncated); Shown is the newest
// maxMeadowShown by update time.
type FarmMeadow struct {
	Total int        `json:"total"`
	Shown []FarmTask `json:"shown"`
	// WithMR counts meadow beads that carry a linked MR ("orphaned with a PR").
	WithMR int `json:"with_mr"`
}

// Wish outcomes (FarmClosedWish.Outcome): how a closed wish ended, read from
// the fields the bridge writes when it closes a thread head
// (internal/bridge/thread_task_autoclose.go, bot_reactions_wish_close.go).
const (
	WishDone      = "done"      // closed by a person (a reaction, kd close), by the herder (herder-fulfilled, herder-owner-close) or by its agent finishing
	WishAbandoned = "abandoned" // its agent failed and nobody took it up (triage_reason set), or the close reason says so
	WishExpired   = "expired"   // autoclosed idle (close_reason slack-thread-idle*)
)

// The herder's verdicts on an open wish (FarmWish.HerderVerdict), as the
// scheduled herder (role herder, trigger gd-AHklx9rH70) stamps them in the
// head's herder_verdict field next to its "[herder] verdict:" comment.
const (
	HerderFulfilled   = "fulfilled"   // what it asked for was done: the herder closes it
	HerderInProgress  = "in-progress" // an agent, an open kin task or PR carries it
	HerderUnfulfilled = "unfulfilled" // its agents stopped without doing it: the herder wakes the thread's agent
	HerderUnclear     = "unclear"     // the evidence conflicts: the herder asks the asker
)

// Wish sources (FarmWish.Source), from the head's source:<name> label.
const (
	WishFromSlack   = "slack"
	WishFromCommons = "commons"
)

// FarmWish is one open wish: a Slack or Commons thread head (type=task, label
// thread). It is never a plant: the farm lists it here, not in the meadow or
// on a plot, and the weeds rules do not read it as loose work. Its kin are the
// open plans, phases and steps that carry fields.thread_id = this wish, so a
// client knows which plots it belongs by; the head itself carries no thread_id.
type FarmWish struct {
	ID      string `json:"id"`
	Title   string `json:"title"` // the short title when the head has one, else its title
	Project string `json:"project,omitempty"`
	Source  string `json:"source,omitempty"` // WishFrom* constants; "" when the head names no source
	Asker   string `json:"asker,omitempty"`  // lowercased operated_by: who asked
	Opened  int64  `json:"opened"`           // created_at, unix seconds
	Updated int64  `json:"updated"`          // the head's own updated_at, unix seconds
	// Kin are the open rows whose thread_id is this wish, newest change first,
	// at most maxWishKin; KinTotal is the exact number among the rows read and
	// KinClosed the closed ones among the closed rows the slow tier keeps.
	Kin       []string `json:"kin,omitempty"`
	KinTotal  int      `json:"kin_total"`
	KinClosed int      `json:"kin_closed"`
	// Plots are the distinct plots the kin are drawn on, in kin order, so the
	// goat knows where to graze without resolving the ids itself. A kin inside
	// a seed, or past a phase's listed cap, has no plot here (the same rule as
	// a gardener's plot).
	Plots []FarmPlot `json:"plots,omitempty"`
	// Tending is true while some kin is in progress or in review: a tended wish
	// is solid, an untended one a ghost.
	Tending bool `json:"tending"`
	// Arms are its kin's line arms (line_arms.go, heraldry 5.10): the first
	// armed kin's run and, when kin sit in two or more armed lines, a second
	// distinct run, in kin order (at most maxWishArms). A wish never bears its
	// own (R6); omitted when no kin's line bears any.
	Arms []beadArms `json:"arms,omitempty"`
	// Href is the wish's thread in the Commons, below the viewer root (the
	// route a gardener's Chat uses).
	Href string `json:"href"`
	// HerderVerdict is the herder's last verdict on it (Herder* constants,
	// from herder_verdict), "" before its first pass; HerderCheckedAt is when
	// (herder_checked_at, unix seconds; 0 when unstamped). HerderActed is set
	// on an unfulfilled or unclear verdict when the herder has woken the
	// thread's agent or asked the asker (herder_woke_at) since the day before
	// that verdict, so the goat says "agent woken" or "asked" only when it was.
	HerderVerdict   string `json:"herder_verdict,omitempty"`
	HerderCheckedAt int64  `json:"herder_checked_at,omitempty"`
	HerderActed     bool   `json:"herder_acted,omitempty"`
}

// FarmClosedWish is one closed wish: a headstone. It carries what the stone
// and its hover show; the bead view has the rest.
type FarmClosedWish struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Project string `json:"project,omitempty"`
	Source  string `json:"source,omitempty"`
	Asker   string `json:"asker,omitempty"`
	Closed  int64  `json:"closed"`  // closed_at, else updated_at; unix seconds
	Kin     int    `json:"kin"`     // rows that carry its thread_id, open and closed, among the rows read
	Outcome string `json:"outcome"` // Wish* constants
	// Reason is why it closed, as the bridge wrote it (close_reason, else
	// triage_reason); the hover card says it, the stone does not.
	Reason string `json:"reason,omitempty"`
	// Produce is what its kin merged: their merged MRs from the produce bin's
	// read, newest first, at most maxWishProduce; ProduceMore counts the rest.
	Produce     []FarmProduce `json:"produce,omitempty"`
	ProduceMore int           `json:"produce_more,omitempty"`
	// Href is the wish's thread in the Commons, below the viewer root.
	Href string `json:"href"`
}

// FarmWishes is every open wish the farm read and the newest closed ones.
// Open is newest first, capped per project at maxWishesPerProject; OpenTotal
// is exact for the rows read and OpenMore counts, per project, the ones the cap
// left out. Closed is newest first, capped per project at
// maxClosedWishesPerProject out of the closed rows the slow tier keeps;
// ClosedTotal is the daemon's count of closed heads. Fulfilled counts the
// open wishes, the capped ones too, the herder judged fulfilled: what it is
// about to close, for the pen's sign.
type FarmWishes struct {
	Open        []FarmWish       `json:"open"`
	OpenTotal   int              `json:"open_total"`
	OpenMore    map[string]int   `json:"open_more,omitempty"`
	Fulfilled   int              `json:"fulfilled,omitempty"`
	Closed      []FarmClosedWish `json:"closed"`
	ClosedTotal int              `json:"closed_total"`
}

// FarmReleases is every release-enabled project's releases (farm_releases.go,
// releases-design.md §3.5), keyed by project. A project with no
// release_tag_pattern has no key; the blob is {} before the first read.
type FarmReleases map[string]FarmProjectReleases

// FarmProjectReleases is one project's releases as the truck reads them.
type FarmProjectReleases struct {
	// Latest is the newest release by cut_at; null when the project has none.
	Latest *FarmRelease `json:"latest"`
	// Deployments is what runs on each target now: one row per target some
	// release of the newest maxReleasesPerProject holds, by target name. Empty
	// is "no deployments yet", never an error.
	Deployments []FarmReleaseDeployment `json:"deployments"`
	// Recent is the newest maxReleasesPerProject releases, newest cut first.
	Recent []FarmReleaseRow `json:"recent"`
	// Shipping is true while Latest is cut or built: loaded, not yet live.
	Shipping bool `json:"shipping"`
}

// FarmRelease is the newest release of a project: what is on the truck.
type FarmRelease struct {
	ID     string `json:"id"` // the release bead
	Tag    string `json:"tag"`
	Status string `json:"status"` // beadsapi.ReleaseStatus*: cut, built, live, superseded
	CutAt  int64  `json:"cut_at"` // unix seconds; 0 when the record has none
	// BuiltAt is when every artifact existed, 0 when not built (or no build step).
	BuiltAt int64 `json:"built_at,omitempty"`
	// LiveSince is the earliest open deployment's since, 0 unless live.
	LiveSince int64 `json:"live_since,omitempty"`
	// Count is the PRs it carries (its contents rows).
	Count int `json:"count"`
	// Plans are the plans those PRs came from with the PRs from each, most
	// first; Unplanned counts the rows with no plan.
	Plans     []FarmReleasePlan `json:"plans"`
	Unplanned int               `json:"unplanned,omitempty"`
}

// FarmReleasePlan is one plan's share of a release: a crate on the truck.
type FarmReleasePlan struct {
	ID    string `json:"id"`
	Count int    `json:"count"`
}

// FarmReleaseDeployment is the release that runs on one target now.
type FarmReleaseDeployment struct {
	Target   string `json:"target"`
	Kind     string `json:"kind,omitempty"`
	Tag      string `json:"tag"`
	Release  string `json:"release"` // its bead
	Since    int64  `json:"since"`   // unix seconds
	Revision string `json:"revision,omitempty"`
}

// FarmReleaseRow is one of a project's recent releases.
type FarmReleaseRow struct {
	ID     string `json:"id"`
	Tag    string `json:"tag"`
	Status string `json:"status"`
	CutAt  int64  `json:"cut_at"`
	Count  int    `json:"count"` // PRs carried
	Plans  int    `json:"plans"` // distinct plans they came from
}

// FarmPiles are the count-only archives: closed things are never sent as rows.
// OrphanMRs is the one bounded exception, because each row is something to tend.
type FarmPiles struct {
	ClosedDecisions   int `json:"closed_decisions"`
	ClosedEscalations int `json:"closed_escalations"`
	ClosedWork        int `json:"closed_work"` // closed task/feature/bug/chore
	ClosedPlans       int `json:"closed_plans"`
	// OrphanMRs are open pull requests that no open work bead carries: the bead
	// that linked them is closed, or no work bead links them at all (the
	// "orphaned PR" weed, gd-ytTNc3Fu9M). Newest first, capped at maxOrphanMRs;
	// OrphanMRTotal is the count found among the open PRs the hub read.
	OrphanMRs     []FarmOrphanMR `json:"orphan_mrs,omitempty"`
	OrphanMRTotal int            `json:"orphan_mr_total,omitempty"`
}

// Why a pull request is an orphan (FarmOrphanMR.Why).
const (
	OrphanBeadClosed = "bead_closed" // the work bead that links it is closed
	OrphanNoBead     = "no_bead"     // no work bead links it
)

// FarmOrphanMR is an open pull request whose bead is closed or missing. Its
// state comes from the type=merge-request bead the forge bridge keeps current,
// never from a task bead's mr_state (that field goes stale).
type FarmOrphanMR struct {
	URL   string `json:"url"`
	IID   string `json:"iid,omitempty"`
	Title string `json:"title,omitempty"`
	Repo  string `json:"repo,omitempty"` // owner/name
	Why   string `json:"why"`            // Orphan* constants
	Bead  string `json:"bead,omitempty"` // the closed work bead, when Why is OrphanBeadClosed
	// Assignee is the closed bead's last assignee: whom the chore tour pings about the open PR (gd-S2Cn2dTsP0).
	Assignee string `json:"assignee,omitempty"`
	Project  string `json:"project,omitempty"`
	// Since is when it became an orphan (unix seconds): when the bead closed
	// (its updated_at), else when the PR bead was created.
	Since int64 `json:"since"`
}

// FarmMeta carries the bounds the payload was built under.
type FarmMeta struct {
	Schema int `json:"schema"`
	// Truncated is true when the work-bead read hit the row cap: counts and
	// plans may then be missing the newest rows. It is shown, never silent.
	Truncated bool `json:"truncated"`
	RowCap    int  `json:"row_cap"`
	Rows      int  `json:"rows"` // non-closed work rows actually read
	// GardenersTruncated is true when more than maxGardeners agents were live:
	// gardeners then holds the newest maxGardeners by updated_at.
	GardenersTruncated bool `json:"gardeners_truncated,omitempty"`
	// GardenersAsOf is when gardeners was last read well (unix seconds), set
	// only once that read is older than farmRosterStale. Omitted while fresh,
	// so meta does not change on every refresh.
	GardenersAsOf int64 `json:"gardeners_as_of,omitempty"`
}

// Gardener buckets (FarmGardener.Bucket). They are beads-viewer's
// agentStateBucket buckets; TestAgentBucketMatchesGardenhub keeps the two in
// agreement.
const (
	GardenerAlive   = "alive"
	GardenerStuck   = "stuck"
	GardenerIdle    = "idle"
	GardenerDormant = "dormant"
)

// FarmPlot is where a gardener works: the plan its task is drawn in, and the
// phase ("" when the task reaches the plan without one, or is the plan epic).
type FarmPlot struct {
	Plan  string `json:"plan"`
	Phase string `json:"phase"`
}

// FarmReason says why a gardener is not simply working (farm_reason.go):
// Kind is a Reason* constant, Text the words every client shows, Href (below
// the viewer root) the handle a parked gardener waits on. A working gardener
// carries "reason": null.
type FarmReason struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
	Href string `json:"href,omitempty"`
}

// FarmGardener is one live agent, drawn on the plot of its task (design:
// gardenio/docs/homestead-agents-as-people.md section 2). Nothing in it ticks
// with each turn, so a working gardener upserts about once a minute at most.
type FarmGardener struct {
	ID       string `json:"id"`                 // the agent bead
	Agent    string `json:"agent"`              // fields.agent, else the title
	Role     string `json:"role"`               // raw fields.role
	Project  string `json:"project,omitempty"`  // the agent's own project, not the plan's
	Operator string `json:"operator,omitempty"` // lowercased operated_by alias
	State    string `json:"state"`              // raw agent_state, "" when unset
	Bucket   string `json:"bucket"`             // Gardener* constants
	Task     string `json:"task"`               // raw task_id, "" when unset
	// TaskTitle is the task's title, from the work rows read, else from the
	// farm's own read of it (readRefTitles: a closed task, one past the cap).
	TaskTitle string `json:"task_title,omitempty"`
	// Plot is null when the gardener is at home: no task, or a task that is
	// not drawn on a plot (a seed, the meadow, closed, or not read).
	Plot  *FarmPlot `json:"plot"`
	Since int64     `json:"since"` // last_progress_at, else updated_at; unix seconds floored to the minute
	Href  string    `json:"href"`  // the in-garden bead view, below the viewer root
	// Chat is the agent's Commons place route, set only when the bead names a
	// thread. Below the viewer root, like Href.
	Chat string `json:"chat,omitempty"`
	// ThreadTitle is the title of the thread head the bead names (thread_id),
	// without the "Commons thread: " lead, cut at 120 runes: set when the head
	// is among the open work rows read, or, for a thread agent (role thread or
	// dm), from the farm's own read of it (readRefTitles). A thread agent at
	// home reads "In the Commons: <thread_title>".
	ThreadTitle string `json:"thread_title,omitempty"`
	// Tends is the bead the garden's "Send a gardener" sent this gardener to
	// (tend.go), "" for any other gardener. With no task it stands on that
	// bead's plot. TendsTitle is set as TaskTitle is, when the bead is among
	// the open work rows read.
	Tends      string `json:"tends,omitempty"`
	TendsTitle string `json:"tends_title,omitempty"`
	// Escalation is the escalation this gardener was spawned to run (the
	// bridge's "escalation-<id>" executor; fields.escalation_id), "" for any
	// other gardener. With no task and no send it stands on that escalation's
	// plot (an escalation is parented to the task that filed it), so it is on
	// the bead it works and not at the barn. EscalationTitle is set as
	// TaskTitle is, when the escalation is among the open work rows read.
	Escalation      string `json:"escalation,omitempty"`
	EscalationTitle string `json:"escalation_title,omitempty"`
	// TaskArms are the line arms of the bead that places this gardener (its
	// task, else the bead it was sent to, else the escalation it runs): that
	// bead's nearest armed ancestor's, by badge.ResolveArms over the rows the
	// farm already holds (farm_gardeners.go gardenerArms; heraldry step 5.9,
	// gd-QboM9sPYCg). A client draws the run before the task's title and as
	// the figure's badge. Absent when the line bears none the farm can see.
	TaskArms *beadArms   `json:"task_arms,omitempty"`
	Reason   *FarmReason `json:"reason"`
	// Cost is what this gardener has spent (farm_gardener_cost.go), null when
	// no figure is known: never a made-up $0.
	Cost *FarmGardenerCost `json:"cost"`
}

// FarmGardenerCost is one gardener's spend. Every amount is US dollars,
// rounded to the cent. Project, for the per-viewer cut, is the gardener's own
// FarmGardener.Project.
type FarmGardenerCost struct {
	// SpentUSD is beads-viewer's agentSpendUSD: total_cost_usd, the sessions
	// the reconciler has closed, plus, while the gardener is alive, the
	// running session's live_cost_usd floored to gardenerLiveStepUSD. Null
	// when neither is known.
	SpentUSD *float64 `json:"spent_usd"`
	// LiveUSD is the running session's part of SpentUSD, null when it is not
	// counted: the gardener is not alive, or the estimate reads under one
	// step (live_cost_usd stays 0 on the LiteLLM path, gd-UIlh0DR2VG, and 0
	// there means not known, not free).
	LiveUSD *float64 `json:"live_usd"`
	// AtLeast is true when SpentUSD is a lower bound: a session may still be
	// open whose cost is counted in full only when it closes.
	AtLeast bool `json:"at_least"`
	// GardenerUSD is set on a gardener's run (trigger_id, else schedule_id):
	// the trigger's gardener_spend_usd, every run of that gardener and the
	// crew they sent, as the controller rolls it up. Null otherwise.
	GardenerUSD *float64 `json:"gardener_usd"`
	// Text is the words every client shows on hover and reads aloud.
	Text string `json:"text"`
}

// Exit kinds (FarmExit.Kind): why a gardener left the set.
const (
	ExitFinished    = "finished"    // gb stop, with its task done (or no task to finish)
	ExitParked      = "parked"      // gb stop with the task still an open step (behind a handle)
	ExitInterrupted = "interrupted" // no gb stop: killed, pod lost, reaped, expired
)

// FarmExit is sent on a patch, never in a snapshot or a reset, for a gardener
// that just left the set. A client uses it for the leaving animation and never
// stores it.
type FarmExit struct {
	ID    string    `json:"id"`
	Agent string    `json:"agent"`
	Kind  string    `json:"kind"`           // Exit* constants
	TLDR  string    `json:"tldr,omitempty"` // the wrap-up's tldr; never on an interrupted exit
	Plot  *FarmPlot `json:"plot"`           // the last plot, null if it was at home
	At    int64     `json:"at"`             // when the farm saw it leave (unix seconds)
	// Tends is the bead it was sent to by "Send a gardener" (FarmGardener.Tends), so the bead view can say what the
	// gardener said when it left. Escalation is the escalation it was spawned to run (FarmGardener.Escalation), for
	// the same reason.
	Tends      string `json:"tends,omitempty"`
	Escalation string `json:"escalation,omitempty"`
}

// Budget periods (FarmCostProject.Period): the window a project budget covers.
const (
	BudgetMonth = "month" // UTC calendar month to date; resets at 00:00 UTC on the 1st
	Budget7d    = "7d"    // rolling 7 days, the same window as week_usd
)

// Cost scopes (FarmCost.Scope): whose spend the blob's own figures sum.
const (
	CostScopeFleet   = "fleet"   // every project the rollup reports, and the account budgets: an operator's view
	CostScopeProject = "project" // only the viewer's projects (FarmCost.ForProjects); no account budgets
)

// Budget levels (FarmCostProject.Level): how much of the budget the period has
// spent, bucketed like the gardener run-budget meter (beads-viewer
// gardenerBudgetBurn) so a client maps it straight to a CSS class. Display
// only: nothing gates a spawn on it.
const (
	BudgetLevelOK   = "ok"   // under 75% spent
	BudgetLevelWarn = "warn" // 75% to 99%
	BudgetLevelOver = "over" // 100% or more
)

// FarmCost is the purse: what the fleet spends, read from the costs rollup
// (the real-bill per-project tag gauge /costs rolls up by project). It is in
// the document only when the viewer wires a spend source. It carries the
// purse's own figures (gd-594kYy1IYo) and one row per project (gd-T6zwDjWLBI).
//
// As on the rows, a figure that is not known is never 0; here it is omitted,
// as is a false flag, so a cut with nothing in it is just {"projects":[]}.
type FarmCost struct {
	// Scope says what the figures below sum: CostScopeFleet (the document as
	// the farm builds it) or CostScopeProject (a viewer's cut).
	Scope string `json:"scope,omitempty"`
	// Has is false when the costs rollup could not be read: today_usd and
	// week_usd are then absent and AsOf is the stale marker, as on a row. A
	// scoped cut with no row in dollars (only tenant lands) has neither.
	Has bool `json:"has,omitempty"`
	// TodayUSD and WeekUSD are spend since 00:00 UTC and over the rolling 7
	// days. For the fleet they sum every project the rollup reports, a project
	// with no type:project bead included; for a scoped cut, the scope's rows
	// in dollars (a tenant land's row is in credits and is not summed).
	TodayUSD *float64 `json:"today_usd,omitempty"`
	WeekUSD  *float64 `json:"week_usd,omitempty"`
	AsOf     int64    `json:"as_of,omitempty"`
	// HasBudget is false when no account has a budget (spend_limit_usd, else
	// max_budget, above 0), the account budgets have not been read, or the
	// scope is a project's: the header then shows a dash for "$ remaining".
	// BudgetUSD, SpentUSD and RemainingUSD sum the budgeted accounts (a shared
	// LiteLLM key counted once, as /costs does): spent is total_cost_usd -
	// spend_at_last_reset (0 when there is none), and remaining is budget -
	// spent clamped at 0 per account, so one overspent account does not eat
	// another's money. They do not depend on the rollup, so has=false leaves
	// them known. All three are absent when !has_budget.
	HasBudget    bool     `json:"has_budget,omitempty"`
	BudgetUSD    *float64 `json:"budget_usd,omitempty"`
	SpentUSD     *float64 `json:"spent_usd,omitempty"`
	RemainingUSD *float64 `json:"remaining_usd,omitempty"`
	// Level is BudgetLevel of budget_usd less remaining_usd against
	// budget_usd, for the header purse's $ remaining (gd-H9LerJ0v3g), so it
	// agrees with remaining_usd. Absent when !has_budget.
	Level    string            `json:"level,omitempty"`
	Projects []FarmCostProject `json:"projects"` // one per type:project bead, by name
}

// FarmCostProject is one project's spend and budget. Budgets are per project,
// so a viewer of several projects gets one row each, never a sum.
//
// Every amount is US dollars and says so in its name. A figure that is not
// known is null, never 0: a client shows a dash, so an unreachable rollup can
// never read as "$0 spent" or "full budget left". A later unit (Lands 8.2 adds
// credits for tenant lands) arrives as new sibling fields; the *_usd fields
// keep their meaning.
type FarmCostProject struct {
	Project string `json:"project"`
	// Has is false when the costs rollup could not be read: every spend figure
	// (and remaining_usd) is then null and AsOf is the stale marker.
	Has bool `json:"has"`
	// HasBudget is false when the project bead has no usable budget_usd
	// (blank, or not a number above 0): the scoped header hides "$ remaining".
	HasBudget bool     `json:"has_budget"`
	BudgetUSD *float64 `json:"budget_usd"`       // null when !has_budget
	Period    string   `json:"period,omitempty"` // Budget* constants; "" when !has_budget
	TodayUSD  *float64 `json:"today_usd"`        // spend since 00:00 UTC
	WeekUSD   *float64 `json:"week_usd"`         // spend over the rolling 7 days
	// SpentUSD is spend over the budget period, and RemainingUSD is
	// budget_usd - spent_usd clamped at 0 (an overspend reads $0, never a
	// negative amount). Both are null unless has and has_budget.
	SpentUSD     *float64 `json:"spent_usd"`
	RemainingUSD *float64 `json:"remaining_usd"`
	// Level is BudgetLevel of spent_usd against budget_usd. It is "" (omitted)
	// unless has and has_budget: an unknown spend has no level.
	Level string `json:"level,omitempty"`
	// AsOf is when the figures were last read well (unix seconds). It is set
	// only while has is false, as the stale marker, and is 0 (omitted) when the
	// rollup has not been read well since the process started.
	AsOf int64 `json:"as_of,omitempty"`
	// Purse is a tenant land's purse (Lands 8.2, internal/lands/purse): credits
	// granted, spent, in flight and remaining, the margin, and state ok, wall
	// or unknown. It also carries the same amounts at cost (*_usd), because
	// /garden is operator-only today; a tenant's view takes CreditsOnly
	// (step 8.8). Absent on operator lands.
	Purse *purse.Purse `json:"purse,omitempty"`
	// Steward is the land's steward (Track B §4.3): its daily run cap and
	// whether it is hit, for the land header. Absent on a land with no steward.
	Steward *FarmSteward `json:"steward,omitempty"`
}
