package landmap

import (
	"gardenio/kbeads/internal/model"
	"gardenio/lands/contract/landsv1"
)

// The map's closed tables (contract design gd-EWSKPH7pa5 §3, "Closed
// tables"). Each is data the handlers read, not a list kept beside them: a
// route the map calls is a routeID, a field it reads goes through its noun's
// TypeRow, a header it sends must be in ResponseHeaders. Tests here and in the
// server package hold each table to the thing it describes.

// Route is a daemon route: the method and the mux pattern exactly as the
// server's registerRoutes registers it.
type Route struct {
	Method  string
	Pattern string
}

func (r Route) String() string { return r.Method + " " + r.Pattern }

// routeID names a row of routeTable. daemon.call takes one, so a route that is
// not in the table cannot be called.
type routeID int

const (
	rListBeads routeID = iota
	rGetBead
	rCreateBead
	rUpdateBead
	rCloseBead
	rAddComment
	rListComments
	rResolveDecision
	rCancelDecision
	rMe
	rLandMembers
	rListReleases
	rGetRelease
	rDeployRelease
	numRoutes
)

// routeTable is every daemon route the map calls. TestRouteTable_Registered
// (server package) checks each is registered and open to a land member.
var routeTable = [numRoutes]Route{
	rListBeads:       {"GET", "/v1/beads"},
	rGetBead:         {"GET", "/v1/beads/{id}"},
	rCreateBead:      {"POST", "/v1/beads"},
	rUpdateBead:      {"PATCH", "/v1/beads/{id}"},
	rCloseBead:       {"POST", "/v1/beads/{id}/close"},
	rAddComment:      {"POST", "/v1/beads/{id}/comments"},
	rListComments:    {"GET", "/v1/beads/{id}/comments"},
	rResolveDecision: {"POST", "/v1/decisions/{id}/resolve"},
	rCancelDecision:  {"POST", "/v1/decisions/{id}/cancel"},
	rMe:              {"GET", "/v1/me"},
	rLandMembers:     {"GET", "/v1/lands/{land}/members"},
	rListReleases:    {"GET", "/v1/releases"},
	rGetRelease:      {"GET", "/v1/releases/{id}"},
	rDeployRelease:   {"POST", "/v1/releases/{id}/deployments"},
}

// Routes returns the route table.
func Routes() []Route { return append([]Route(nil), routeTable[:]...) }

// ActionRead marks a Verb that only reads. Reads are cut by the daemon's read
// scope (scopeListFilter, authorizeBeadRead), not by actionTable.
const ActionRead = "read"

// ActionLand marks a Verb that writes only through Lands, which judges the
// caller itself: no actionTable action applies, since the daemon routes it
// stands for (the agent bead create, the token mint) refuse every tenant.
const ActionLand = "land"

// Verb is one contract operation: the bead types it touches, the actionTable
// action it needs on them, and the daemon routes it calls.
type Verb struct {
	// Op is the contract's operationId.
	Op string
	// Types and Action: what the verb does, in the daemon's terms. Action is
	// ActionRead, ActionLand or an actionTable action ("create", "update",
	// "close", "comment"); TestVerbTable_ActionTable (server package) checks that a
	// land member's tier allows it on every type, so a verb the daemon would
	// 403 for every tenant never ships.
	Types  []model.BeadType
	Action string
	routes []routeID
	// Land is set when the verb reads the land itself (Lands.Land), which no
	// daemon route a tenant can reach answers.
	Land bool
	// OwnerOnly is set for a write the daemon admits for the land's owner
	// and no other tenant: Action is in the owner tier's row of actionTable
	// and not the member's (TestLandsAPI_VerbsInActionTable checks both).
	OwnerOnly bool
	// Deferred says why a verb is not served yet and where it will be. The map
	// answers it with not_found.
	Deferred string
}

var ticketTypes = []model.BeadType{model.TypeTask, model.TypeBug, model.TypeFeature}

// blockingDeps are the dependency edges that hold a bead back, and
// openStatuses the statuses a blocker holds it back in: the daemon's own sets
// (blockingDepTypes and openBlockerStatusSQL in store/postgres), so a ticket
// reads waiting exactly when `kd list --no-blockers` would hide it.
var (
	blockingDeps = []model.DependencyType{model.DepBlocks, "blocked-by", "depends-on"}
	openStatuses = []model.Status{model.StatusOpen, model.StatusInProgress, model.StatusBlocked, model.StatusDeferred}
)

const (
	deferredThreads = "D.9 gd-Z9VVGjvzBk (and owner question 3, whether the transcript is tenant-visible)"
)

// feedTypes are the bead types the land feed can stream an event for.
var feedTypes = []model.BeadType{model.TypeTask, model.TypeBug, model.TypeFeature, model.TypeDecision, model.TypeEscalation, model.TypeEpic, model.TypeRelease}

// Verbs is the verb table: one row per contract operation.
// TestVerbTable_CoversContract checks it against landsv1.ServerInterface.
var Verbs = []Verb{
	{Op: "getLand", Action: ActionRead, Land: true},
	// A land works in one repository, its project bead's (Lands.Plot).
	{Op: "listPlots", Action: ActionRead, Land: true},
	{Op: "getPlot", Action: ActionRead, Land: true},
	{Op: "listCrew", Action: ActionRead, Land: true, routes: []routeID{rLandMembers}},
	{Op: "getMember", Action: ActionRead, Land: true, routes: []routeID{rLandMembers}},
	{Op: "whoami", Action: ActionRead, Land: true, routes: []routeID{rMe}},
	// The purse snapshot on the land's account bead (Lands.Budget).
	{Op: "getBudget", Action: ActionRead, Land: true},
	// The land's credit series, or the purse snapshots recorded on its
	// account bead (Lands.BudgetHistory).
	{Op: "getBudgetHistory", Action: ActionRead, Land: true},

	// A listed ticket's blockers are read one by one (Map.waiting).
	{Op: "listTickets", Types: ticketTypes, Action: ActionRead, routes: []routeID{rListBeads, rGetBead}},
	{Op: "searchTickets", Types: ticketTypes, Action: ActionRead, routes: []routeID{rListBeads, rGetBead}},
	{Op: "getTicket", Types: ticketTypes, Action: ActionRead, routes: []routeID{rGetBead}},
	{Op: "createTicket", Types: ticketTypes, Action: "create", Land: true, routes: []routeID{rCreateBead}},
	{Op: "updateTicket", Types: ticketTypes, Action: "update", Land: true, routes: []routeID{rGetBead, rUpdateBead}},
	{Op: "commentTicket", Types: ticketTypes, Action: "comment", Land: true, routes: []routeID{rGetBead, rAddComment}},
	{Op: "closeTicket", Types: ticketTypes, Action: "close", Land: true, routes: []routeID{rGetBead, rCloseBead}},
	{Op: "reopenTicket", Types: ticketTypes, Action: "update", Land: true, routes: []routeID{rGetBead, rUpdateBead}},

	{Op: "listDecisions", Types: []model.BeadType{model.TypeDecision}, Action: ActionRead, routes: []routeID{rListBeads}},
	// A decision's evidence is read off the tickets it names, their comments
	// included (evidence.go); the verbs that answer a decision answer it so.
	{Op: "getDecision", Types: []model.BeadType{model.TypeDecision}, Action: ActionRead, routes: []routeID{rGetBead, rListComments}},
	// One evidence picture, served from the picture bucket (Pictures) only
	// when the decision's evidence, worked out again, names it.
	{Op: "getDecisionEvidence", Types: []model.BeadType{model.TypeDecision}, Action: ActionRead, routes: []routeID{rGetBead, rListComments}},
	// Resolve and cancel close the decision (authorizeResolutionTarget).
	{Op: "resolveDecision", Types: []model.BeadType{model.TypeDecision}, Action: "close", Land: true, routes: []routeID{rGetBead, rResolveDecision, rListComments}},
	{Op: "cancelDecision", Types: []model.BeadType{model.TypeDecision}, Action: "close", Land: true, routes: []routeID{rGetBead, rCancelDecision, rListComments}},
	{Op: "commentDecision", Types: []model.BeadType{model.TypeDecision}, Action: "comment", Land: true, routes: []routeID{rGetBead, rAddComment}},

	{Op: "listEscalations", Types: []model.BeadType{model.TypeEscalation}, Action: ActionRead, routes: []routeID{rListBeads}},
	{Op: "getEscalation", Types: []model.BeadType{model.TypeEscalation}, Action: ActionRead, routes: []routeID{rGetBead}},
	{Op: "createEscalation", Types: []model.BeadType{model.TypeEscalation}, Action: "create", Land: true, routes: []routeID{rCreateBead, rGetBead}},

	{Op: "listPlans", Types: []model.BeadType{model.TypeEpic}, Action: ActionRead, routes: []routeID{rListBeads, rGetBead}},
	{Op: "getPlan", Types: []model.BeadType{model.TypeEpic}, Action: ActionRead, routes: []routeID{rGetBead, rListBeads}},
	// Answering a plan's question resolves one of its decisions.
	{Op: "answerPlan", Types: []model.BeadType{model.TypeDecision}, Action: "close", Land: true, routes: []routeID{rGetBead, rResolveDecision, rListComments}},

	// A member's own agent (agents.go): its record and credential are the
	// server's to make and judge (Lands.MintAgent and the rest).
	{Op: "listAgents", Action: ActionRead, Land: true},
	{Op: "getAgent", Action: ActionRead, Land: true},
	{Op: "createAgent", Action: ActionLand, Land: true},
	{Op: "refreshAgent", Action: ActionLand, Land: true},
	{Op: "revokeAgent", Action: ActionLand, Land: true},
	// A land's releases, read through the daemon's release routes
	// (releases.go). Reporting a deployment is the land owner's alone; the
	// release is read first so another land's answers not_found.
	{Op: "listReleases", Types: releaseTypes, Action: ActionRead, routes: []routeID{rListReleases}},
	{Op: "getRelease", Types: releaseTypes, Action: ActionRead, routes: []routeID{rGetRelease}},
	{Op: "reportDeployment", Types: releaseTypes, Action: "release_deploy", OwnerOnly: true, Land: true, routes: []routeID{rGetRelease, rDeployRelease}},

	{Op: "listThreads", Deferred: deferredThreads},
	{Op: "getThread", Deferred: deferredThreads},
	{Op: "listMessages", Deferred: deferredThreads},
	{Op: "postMessage", Deferred: deferredThreads},
	// The feed reads each changed bead (and a plan's steps, or a release
	// through its route) as the reader, at delivery (feed.go), and a purse
	// change's snapshot through Lands.Budget.
	{Op: "streamEvents", Types: feedTypes, Action: ActionRead, Land: true, routes: []routeID{rGetBead, rListBeads, rGetRelease}},
}

// TypeRow is what the map does with one bead type: the contract noun it is
// projected as ("" for a type tenants never see), the bead keys the map reads
// to do it (Fields) and the keys it writes when it creates or edits one
// (Writes). A key is a top-level bead key ("title") or "fields.<key>".
// records.go reads nothing outside Fields, TestWrites_InTypeRows holds every
// write body to Writes, and the server package's map_schema_parity_test.go
// (D.7, gd-WV1InzSogg) holds every key to the live bead schema.
type TypeRow struct {
	Type   model.BeadType
	Noun   string
	Fields []string
	Writes []string
}

// ticketKeys and ticketWrites are every ticket type's row.
var (
	ticketKeys   = []string{"id", "type", "title", "description", "status", "priority", "assignee", "labels", "updated_at", "dependencies", "open_blockers", "heraldry"}
	ticketWrites = []string{"title", "type", "kind", "description", "priority", "labels", "status", "heraldry"}
)

// TypeRows has a row for every bead type the daemon knows, hidden or
// visible. TestTypeRows_EveryType (server package) fails for a type with no
// row, so a new type is a decision here before it can be shown.
var TypeRows = []TypeRow{
	{Type: model.TypeTask, Noun: "ticket", Fields: ticketKeys, Writes: ticketWrites},
	{Type: model.TypeBug, Noun: "ticket", Fields: ticketKeys, Writes: ticketWrites},
	{Type: model.TypeFeature, Noun: "ticket", Fields: ticketKeys, Writes: ticketWrites},
	{Type: model.TypeDecision, Noun: "decision", Fields: []string{
		"id", "type", "title", "status", "labels", "created_at",
		"fields.prompt", "fields.context", "fields.options", "fields.chosen", "fields.canceled", "fields.requesting_plan",
		"fields.evidence", "fields.requesting_task",
	}},
	{Type: model.TypeEscalation, Noun: "escalation", Fields: []string{
		"id", "type", "title", "description", "status", "labels", "created_by",
		"fields.reason", "fields.requesting_agent", "fields.approved_by",
	}, Writes: []string{
		"title", "type", "kind", "description", "labels",
		"fields.operation_type", "fields.operation_spec", "fields.reason",
	}},
	// A plan is an epic labelled "plan"; its steps are its children, read as
	// tickets (their rows above).
	{Type: model.TypeEpic, Noun: "plan", Fields: []string{"id", "type", "title", "status", "labels", "heraldry", "fields.plan_state"}},
	// A release is read as a bead only to know what changed (the feed); its
	// record comes from the release routes, keys in releaseKeys.
	{Type: model.TypeRelease, Noun: "release", Fields: []string{"id", "type", "labels"}},

	// Hidden: never projected. The land itself is read through Lands, not as
	// a bead.
	{Type: model.TypeChore}, {Type: "merge-request"}, {Type: "jira-poller"}, {Type: model.TypeAdvice},
	{Type: model.TypeJack}, {Type: "mail"}, {Type: "agent"}, {Type: "rating"}, {Type: model.TypeReport}, {Type: model.TypeEvidence},
	{Type: model.TypeRichReport}, {Type: "widget-type"}, {Type: "widget-instance"}, {Type: model.TypeProject},
	{Type: model.TypeFleet}, {Type: "user"}, {Type: "plot_application"}, {Type: "gatehouse_request"}, {Type: "land_invite"}, {Type: "window"}, {Type: "invite"},
	{Type: "credit_grant"}, {Type: "credit_rate"}, {Type: "permission"}, {Type: "dm"}, {Type: model.TypeAccount},
	{Type: model.TypeTeam}, {Type: "reaction-rule"}, {Type: "role"}, {Type: "spawn_policy"}, {Type: "escalation_policy"}, {Type: "rig"},
	{Type: "runner"}, {Type: "runner_invite"},
	{Type: "convoy"}, {Type: "config"}, {Type: "event"}, {Type: "gate"}, {Type: "message"}, {Type: "channel"},
	{Type: "notification"}, {Type: model.TypeFormula}, {Type: model.TypeBundle}, {Type: "doc"},
	{Type: model.TypeMolecule}, {Type: "script"}, {Type: "memory"}, {Type: "mention"}, {Type: "artifact"},
	{Type: "runbook"}, {Type: "credential"}, {Type: "audit"}, {Type: model.TypeInvestigationRequest},
	{Type: model.TypeScrubRequest}, {Type: "session"}, {Type: "trigger"}, {Type: "schedule"}, {Type: "dashboard"},
	{Type: "alert_rule"}, {Type: "alert_mute"}, {Type: "operator"}, {Type: "taint"},
}

// RowFor returns the row for a bead type; ok is false for a type with none.
func RowFor(t model.BeadType) (TypeRow, bool) {
	for _, r := range TypeRows {
		if r.Type == t {
			return r, true
		}
	}
	return TypeRow{}, false
}

// FieldKind is the schema type the map reads or writes one fields.<key> of a
// TypeRow as. Values, for an enum, is every value the map maps; the schema
// must have exactly these. Stamped is set for a key the daemon writes itself
// outside the type's schema (mergeBeadFields): the daemon source, from the
// kbeads root, that stamps it.
type FieldKind struct {
	Type    model.BeadType
	Key     string
	Kind    model.FieldType
	Values  []string
	Stamped string
}

// FieldKinds has a row for every fields.<key> in a TypeRow's Fields or
// Writes. TestMapSchemaParity (server package) holds each to the bead type's
// built-in schema, so a kbeads change that renames, retypes or removes a field
// the map projects fails there, not in a tenant's response.
var FieldKinds = []FieldKind{
	{Type: model.TypeDecision, Key: "prompt", Kind: model.FieldTypeString},
	{Type: model.TypeDecision, Key: "context", Kind: model.FieldTypeString},
	{Type: model.TypeDecision, Key: "options", Kind: model.FieldTypeJSON},
	{Type: model.TypeDecision, Key: "chosen", Kind: model.FieldTypeString},
	{Type: model.TypeDecision, Key: "canceled", Kind: model.FieldTypeBoolean, Stamped: "internal/server/http_decisions.go"},
	{Type: model.TypeDecision, Key: "requesting_plan", Kind: model.FieldTypeString},
	{Type: model.TypeDecision, Key: "evidence", Kind: model.FieldTypeString},
	{Type: model.TypeDecision, Key: "requesting_task", Kind: model.FieldTypeString},

	{Type: model.TypeEscalation, Key: "reason", Kind: model.FieldTypeString},
	{Type: model.TypeEscalation, Key: "requesting_agent", Kind: model.FieldTypeString},
	{Type: model.TypeEscalation, Key: "approved_by", Kind: model.FieldTypeString},
	{Type: model.TypeEscalation, Key: "operation_type", Kind: model.FieldTypeString},
	{Type: model.TypeEscalation, Key: "operation_spec", Kind: model.FieldTypeJSON},

	{Type: model.TypeEpic, Key: "plan_state", Kind: model.FieldTypeEnum, Values: []string{
		string(landsv1.Draft), string(landsv1.Ready), string(landsv1.Building), string(landsv1.Complete), string(landsv1.Abandoned),
	}},
}

// commentKeys is what the map reads off a daemon comment.
var commentKeys = []string{"id", "author", "text", "created_at", "operated_by_display"}

// releaseKeys is what the map reads off the release routes (the server's
// releaseView): the record, a contents row and a deployments row.
var releaseKeys = []string{
	"releases", "id", "project", "tag", "sha", "cut_at", "status", "contents", "deployments",
	"contents.pr", "contents.title", "contents.url", "contents.step", "contents.plan",
	"deployments.target", "deployments.kind", "deployments.since", "deployments.until", "deployments.by", "deployments.revision",
}

// meKeys is what the map reads off GET /v1/me.
var meKeys = []string{"operator_id", "lands", "lands.name", "lands.tier"}

// memberKeys is what the map reads off GET /v1/lands/{land}/members.
var memberKeys = []string{"members", "members.principal", "members.name", "members.role"}

// EventRow is one event type the land feed may carry and the noun its
// object is. EventTypes is the whole allowlist: the feed (feed.go) streams
// these and drops everything else. message.posted waits on threads
// (deferredThreads) and is not produced yet. budget.updated is the purse
// snapshot (Lands.Budget), sent when the purse watcher rewrites it on the
// land's account bead (ChangePurse) and it reads differently. release.cut is
// a release recorded, release.deployed an edit that leaves it with
// deployments (releaseEvent).
type EventRow struct {
	Type landsv1.EventType
	Noun string
}

var EventTypes = []EventRow{
	{landsv1.TicketUpdated, "ticket"},
	{landsv1.DecisionOpened, "decision"},
	{landsv1.DecisionResolved, "decision"},
	{landsv1.EscalationOpened, "escalation"},
	{landsv1.MessagePosted, "message"},
	{landsv1.PlanUpdated, "plan"},
	{landsv1.BudgetUpdated, "budget"},
	{landsv1.ReleaseCut, "release"},
	{landsv1.ReleaseDeployed, "release"},
}

// ResponseHeaders is every header a map response may carry. The map deletes
// any other header already set on the response, the daemon's version headers
// included, before it writes. X-Accel-Buffering is the event stream's
// (feedHeaders), X-Content-Type-Options an evidence picture's (evidence.go).
var ResponseHeaders = []string{"Content-Type", landsv1.HeaderContract, "Cache-Control", "X-Accel-Buffering", "X-Content-Type-Options"}
