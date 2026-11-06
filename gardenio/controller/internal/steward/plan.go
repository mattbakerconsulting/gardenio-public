package steward

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation"
	"gardenio/controller/internal/heraldry"
	"gardenio/controller/internal/lands/purse"
	"gardenio/controller/internal/plangardener"
)

// Conversation to plan (spec §4.2 item 2, step 9.4). When a person asks for
// work, the steward writes the plan as markdown and gb lands plan write turns
// it into the plan /plans shows (an epic labelled plan and project:<land>,
// one task per step, run in order) and files one decision to the land's
// owner: approve, change or drop. When the owner answers on the Commons card
// the decision closes, that wakes the steward, and gb lands plan answer
// applies the answer: approve moves the plan to building and names the first
// step to spawn a gardener on; change and drop abandon it (a change is a new
// plan, written with the owner's words, that replaces it).
//
// The plan lives on the authoring lifecycle (plan_state): it is born draft,
// is ready once every step is written and the owner is asked, and goes to
// building or abandoned on the answer. The decision names the plan in
// requesting_task, and the plan is blocked by the decision until it closes, as
// gb decision create --requesting-task links them.
//
// The home garden plans the same way (step 9.11). It is the operator land on
// the Commons: no steward lives there, so a Commons thread agent writes the
// plan, its operator is the owner asked, and on approve gb lands plan answer
// also starts the plan's gardener (cmd/gb/lands_plan.go) where a tenant land's
// steward would spawn a gardener on the first step itself.
//
// A plan the home garden's owner asked for in their own thread is not asked
// about again (programme step P8.7, owner ruling 3 on gd-7qUpu15BBK): the
// thread's own agent writes it straight to building, bound to the thread, and
// tells the thread how to change or drop it. The kbeads server seals the
// gardener the thread then binds from the same facts
// (spawn_lineage_thread_ask.go). A plan the agent proposes itself still goes
// to the owner on a card.

// The plan decision's options, in the order the card shows them.
const (
	PlanApprove = "approve"
	PlanChange  = "change"
	PlanDrop    = "drop"
)

// planOptions are the owner's three answers. They name no artifact_type: an
// answer is acted on with gb lands plan answer, not owed back as a report.
var planOptions = []map[string]string{
	{"id": PlanApprove, "short": "Run the plan", "label": "Run the plan: the steward starts its first step"},
	{"id": PlanChange, "short": "Change it first", "label": "Change the plan first: say what to change, and the steward writes it again"},
	{"id": PlanDrop, "short": "Drop it", "label": "Drop the plan: nothing runs"},
}

// Bounds on a plan: the /plans caps (cmd/gb plan_write.go), a step count
// one owner can read on a card, and the shortest step body a gardener can
// work from (it reads only the bead).
const (
	maxPlanTitle     = 120
	maxPlanStepTitle = 500
	MaxPlanSteps     = 12
	MinPlanStepBody  = 200
)

// planStateField is the epic field holding the authoring lifecycle state.
const planStateField = "plan_state"

// PlanDraft is a plan as the steward writes it: a title, its arms, what it
// is for, and its steps in the order they run.
type PlanDraft struct {
	Title string
	// Arms is the plan's arms from its Arms: line, the emoji run in the
	// library's spelling ("🔥🧭📦"), or "" when it has no Arms: line.
	Arms  string
	Body  string
	Steps []PlanStep
}

// PlanStep is one step: small enough for one gardener and one pull request.
type PlanStep struct {
	Title string
	Body  string
}

// ParsePlan reads a plan written as markdown:
//
//	# <plan title>
//	Arms: <three to five emoji>
//	<what the plan does and why, for the owner>
//	## <step 1 title>
//	<what the step changes, for its gardener>
//	## <step 2 title>
//	...
//
// Anything before the title is ignored; deeper headings stay in the body they
// are under. The Arms: line may stand anywhere in the owner's paragraph and
// is taken out of it; its run is checked as kd heraldry check checks it
// (heraldry.ParseRun), and a run kd would refuse is refused in kd's words.
// A plan without one parses: WritePlan warns.
func ParsePlan(md string) (PlanDraft, error) {
	var d PlanDraft
	var body []string
	var step *PlanStep
	var armsErr error
	armsLines := 0
	flush := func() {
		text := strings.TrimSpace(strings.Join(body, "\n"))
		if step != nil {
			step.Body = text
			d.Steps = append(d.Steps, *step)
		} else {
			d.Body = text
		}
		body = nil
	}
	for _, line := range strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n") {
		switch {
		case d.Title == "" && strings.HasPrefix(line, "# "):
			d.Title = strings.TrimSpace(strings.TrimPrefix(line, "# "))
		case d.Title == "":
		case strings.HasPrefix(line, "## ") || line == "##":
			flush()
			step = &PlanStep{Title: strings.TrimSpace(strings.TrimPrefix(line, "##"))}
		case step == nil && isArmsLine(line):
			if armsLines++; armsLines == 1 {
				d.Arms, armsErr = heraldry.ParseRun(strings.TrimSpace(line)[len(armsPrefix):])
			}
		default:
			body = append(body, line)
		}
	}
	if d.Title == "" {
		return PlanDraft{}, errors.New(`a plan starts with its title, "# <title>"`)
	}
	switch {
	case armsLines > 1:
		return PlanDraft{}, fmt.Errorf("the plan has %d Arms: lines; give its arms once, under the title", armsLines)
	case armsErr != nil:
		return PlanDraft{}, fmt.Errorf("the plan's Arms: line: %w", armsErr)
	}
	flush()
	return d, d.validate()
}

// armsPrefix opens the owner paragraph's Arms: line.
const armsPrefix = "Arms:"

// isArmsLine reports whether line is an Arms: line, in any case.
func isArmsLine(line string) bool {
	line = strings.TrimSpace(line)
	return len(line) >= len(armsPrefix) && strings.EqualFold(line[:len(armsPrefix)], armsPrefix)
}

func (d PlanDraft) validate() error {
	switch {
	case strings.TrimSpace(d.Title) == "":
		return errors.New("the plan has no title")
	case len([]rune(d.Title)) > maxPlanTitle:
		return fmt.Errorf("the plan title is %d characters; keep it to %d", len([]rune(d.Title)), maxPlanTitle)
	case len(d.Steps) == 0:
		return errors.New(`the plan has no steps: write each as "## <step title>" with what it changes under it`)
	case len(d.Steps) > MaxPlanSteps:
		return fmt.Errorf("the plan has %d steps; keep it to %d and plan the rest later", len(d.Steps), MaxPlanSteps)
	}
	for i, s := range d.Steps {
		switch {
		case s.Title == "":
			return fmt.Errorf("step %d has no title", i+1)
		case len([]rune(s.Title)) > maxPlanStepTitle:
			return fmt.Errorf("step %d's title is %d characters; keep it to %d", i+1, len([]rune(s.Title)), maxPlanStepTitle)
		}
		if err := checkStepBody(fmt.Sprintf("step %d (%s)", i+1, s.Title), s.Body); err != nil {
			return err
		}
	}
	return nil
}

// checkStepBody refuses a step body too short to build from: the gardener
// that takes the step reads only its bead.
func checkStepBody(step, body string) error {
	if n := len([]rune(strings.TrimSpace(body))); n < MinPlanStepBody {
		return fmt.Errorf("%s has a %d-character body; write at least %d: what it changes, where, and how to tell it is done (its gardener reads only the bead)", step, n, MinPlanStepBody)
	}
	return nil
}

// PlanStore is the slice of the daemon client the plan verbs use.
type PlanStore interface {
	purse.Source
	GetBead(ctx context.Context, id string) (*beadsapi.BeadDetail, error)
	CreateBead(ctx context.Context, req beadsapi.CreateBeadRequest) (string, error)
	AddDependency(ctx context.Context, beadID, dependsOnID, depType, createdBy string) error
	GetReverseDependencies(ctx context.Context, beadID string) ([]beadsapi.Dependency, error)
	UpdateBeadFieldsTyped(ctx context.Context, beadID string, fields map[string]any) error
	CloseBeadAs(ctx context.Context, beadID, actor string, fields map[string]string) error
	AddComment(ctx context.Context, beadID, author, text string) error
}

// WritePlanOptions are what gb lands plan write knows beyond the plan.
type WritePlanOptions struct {
	// Thread is the message (or thread head) the ask came in, or "".
	Thread string
	// Owner is who the decision is for: oidc:<email>. "" picks the land's
	// owner from its members.
	Owner string
	// Replaces is the plan this one replaces after the owner asked for a
	// change, or "".
	Replaces string
	// StepEstimateCredits is what one step is expected to cost; 0 leaves the
	// estimate off the card.
	StepEstimateCredits float64
	// Actor writes the beads; Agent is the steward run's agent bead and
	// GiftedBy the run's own operator, stamped as they are on any decision
	// an agent addresses to someone else (gb decision create --for).
	Actor    string
	Agent    string
	GiftedBy string
	// AgentThread is the thread head the writing agent is bound to (its
	// agent bead's thread_id), or "". A home-garden plan written in that
	// thread, which the owner opened, starts building with no card.
	AgentThread string
	// Propose files the owner's card even then: the plan is the agent's own
	// idea, not what the owner asked for.
	Propose bool
}

// WrittenPlan is what WritePlan made.
type WrittenPlan struct {
	Land  string   `json:"land"`
	Plan  string   `json:"plan"`
	Steps []string `json:"steps"`
	// Decision is the owner's approve / change / drop card, "" when the plan
	// started building because the owner asked for it (Building).
	Decision string `json:"decision,omitempty"`
	// Building is true when the owner asked for the plan in their own
	// thread, so it was written straight to building with no card.
	Building bool   `json:"building,omitempty"`
	Owner    string `json:"owner"`
	Thread   string `json:"thread,omitempty"`
	// Home is true when land is the home garden (HomeOwner).
	Home bool `json:"home,omitempty"`
	// Arms is the run the plan epic was given, "" when the plan has none.
	Arms string `json:"arms,omitempty"`
	// Warnings are for the agent that wrote the plan: arms to choose, or
	// arms another bead of the land has in another order.
	Warnings []string `json:"warnings,omitempty"`
}

// ErrPlanWritten: the same plan for the same ask was written before. The
// epic is found by its idempotency key, so a retried write never doubles the
// steps or asks the owner twice.
var ErrPlanWritten = errors.New("this plan was already written")

// WritePlan writes a plan in a tenant land or the home garden and asks the
// owner to run it, or, for a plan the home garden's owner asked for in their
// own thread (ownerAskedInThread), writes it straight to building.
func WritePlan(ctx context.Context, st PlanStore, land string, d PlanDraft, opt WritePlanOptions) (*WrittenPlan, error) {
	if err := d.validate(); err != nil {
		return nil, err
	}
	home, err := planLand(ctx, st, land)
	if err != nil {
		return nil, err
	}
	owner, err := planOwner(ctx, st, land, opt.Owner, home)
	if err != nil {
		return nil, err
	}
	if opt.Replaces != "" {
		if _, err := landPlan(ctx, st, land, opt.Replaces); err != nil {
			return nil, err
		}
	}

	key := planKey(land, opt.Thread, opt.Replaces, d.Title)
	asked := ownerAskedInThread(ctx, st, home, owner, opt)
	var warnings []string
	if d.Arms != "" {
		if warnings, err = checkPlanArms(ctx, st, land, d.Arms, key); err != nil {
			return nil, err
		}
	}

	inLand := []string{"project:" + land}
	// No thread_id here: type:epic does not declare it, and the daemon stamps
	// the calling thread agent's thread_id (asked) after the create; the
	// server reads that binding when it seals the gardener.
	epicFields, err := json.Marshal(map[string]string{planStateField: "draft"})
	if err != nil {
		return nil, err
	}
	// The arms go on at creation, so /plans and the card show them from the
	// first read; the daemon checks the register again as it writes.
	planID, err := st.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:          d.Title,
		Type:           "epic",
		Kind:           "issue",
		Description:    planDescription(d, opt),
		Labels:         append(slices.Clone(inLand), plangardener.PlanLabel),
		Fields:         epicFields,
		Heraldry:       d.Arms,
		CreatedBy:      opt.Actor,
		IdempotencyKey: key,
	})
	if err != nil {
		return nil, fmt.Errorf("writing the plan epic: %w", err)
	}
	if children, err := planSteps(ctx, st, planID); err != nil {
		return nil, err
	} else if len(children) > 0 {
		return nil, fmt.Errorf("%w: %s", ErrPlanWritten, planID)
	}

	out := &WrittenPlan{Land: land, Plan: planID, Owner: owner, Thread: opt.Thread, Home: home != "", Arms: d.Arms, Warnings: warnings}
	if d.Arms == "" {
		out.Warnings = append(out.Warnings, noArmsWarning(planID, land, owner))
	}
	if out.Steps, err = writePlanSteps(ctx, st, planID, d, inLand, opt.Actor); err != nil {
		return nil, err
	}
	if err := st.UpdateBeadFieldsTyped(ctx, planID, map[string]any{
		planStateField: "ready",
		"step_order":   out.Steps,
	}); err != nil {
		return nil, fmt.Errorf("marking %s ready: %w", planID, err)
	}
	if opt.Replaces != "" {
		_ = st.AddDependency(ctx, planID, opt.Replaces, "related", opt.Actor)
	}
	if asked != "" {
		if err := st.UpdateBeadFieldsTyped(ctx, planID, map[string]any{planStateField: "building"}); err != nil {
			return nil, fmt.Errorf("moving %s to building: %w", planID, err)
		}
		_ = st.AddComment(ctx, planID, opt.Actor, fmt.Sprintf("plan_state → building: %s asked for it in thread %s, so no card (owner ruling 3 on gd-7qUpu15BBK)",
			strings.TrimPrefix(owner, "oidc:"), asked))
		out.Building = true
		return out, nil
	}

	if out.Decision, err = askPlanOwner(ctx, st, planID, d, out.Steps, owner, inLand, opt); err != nil {
		return nil, err
	}
	return out, nil
}

// writePlanSteps writes one task per step under planID, each blocked by the
// one before so they run in order, and returns their ids.
func writePlanSteps(ctx context.Context, st PlanStore, planID string, d PlanDraft, inLand []string, actor string) ([]string, error) {
	var ids []string
	for i, s := range d.Steps {
		id, err := st.CreateBead(ctx, beadsapi.CreateBeadRequest{
			Title:       s.Title,
			Type:        "task",
			Kind:        "issue",
			Description: s.Body,
			Labels:      slices.Clone(inLand),
			CreatedBy:   actor,
		})
		if err != nil {
			return nil, fmt.Errorf("writing step %d of %s: %w", i+1, planID, err)
		}
		if err := st.AddDependency(ctx, id, planID, "parent-child", actor); err != nil {
			return nil, fmt.Errorf("linking step %s to %s: %w", id, planID, err)
		}
		// Steps run one at a time, in order: each waits on the one before.
		if i > 0 {
			if err := st.AddDependency(ctx, id, ids[i-1], "blocks", actor); err != nil {
				return nil, fmt.Errorf("ordering step %s after %s: %w", id, ids[i-1], err)
			}
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// askPlanOwner files the owner's approve / change / drop decision on planID
// and blocks the plan on it, returning the decision's id.
func askPlanOwner(ctx context.Context, st PlanStore, planID string, d PlanDraft, steps []string, owner string, inLand []string, opt WritePlanOptions) (string, error) {
	decFields := map[string]any{
		"prompt":          planPrompt(planID, d.Title),
		"options":         planOptions,
		"context":         planContext(d, steps, opt),
		"requested_by":    opt.Actor,
		"operated_by":     owner,
		"requesting_task": planID,
		"requesting_plan": planID,
	}
	if opt.Agent != "" {
		decFields["requesting_agent_bead_id"] = opt.Agent
	}
	if opt.GiftedBy != "" && !strings.EqualFold(opt.GiftedBy, owner) {
		decFields["gifted_by"] = opt.GiftedBy
	}
	raw, err := json.Marshal(decFields)
	if err != nil {
		return "", err
	}
	decID, err := st.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:     planPrompt(planID, d.Title),
		Type:      "decision",
		Kind:      "data",
		Priority:  2,
		Assignee:  opt.Actor,
		Labels:    slices.Clone(inLand),
		Fields:    raw,
		CreatedBy: opt.Actor,
	})
	if err != nil {
		return "", fmt.Errorf("asking the owner about %s: %w", planID, err)
	}
	// The plan reads blocked on the owner until the decision closes.
	if err := st.AddDependency(ctx, planID, decID, "blocks", opt.Actor); err != nil {
		return "", fmt.Errorf("linking %s to its decision %s: %w", planID, decID, err)
	}
	return decID, nil
}

// ownerAskedInThread returns the thread head when the plan starts building with
// no card, else "": the land is the home garden, the owner asked is its owner,
// the agent did not mark the plan its own proposal, and the plan's thread is
// the agent's own thread, opened by the owner. opt.Thread may name a message
// in the thread; its thread_id names the head. A failed read files the card.
func ownerAskedInThread(ctx context.Context, st PlanStore, home, owner string, opt WritePlanOptions) string {
	if home == "" || opt.Propose || opt.AgentThread == "" || opt.Thread == "" || !strings.EqualFold(owner, home) {
		return ""
	}
	head, err := st.GetBead(ctx, opt.Thread)
	if err != nil || head == nil {
		return ""
	}
	if up := strings.TrimSpace(head.Fields["thread_id"]); up != "" && up != head.ID {
		if head, err = st.GetBead(ctx, up); err != nil || head == nil {
			return ""
		}
	}
	if head.ID != opt.AgentThread || !slices.Contains(head.Labels, "thread") || !strings.EqualFold(threadOpener(head.CreatedBy), home) {
		return ""
	}
	return head.ID
}

// threadOpener is the person who opened a thread head, "oidc:<email>", from
// its created_by (the opener's own identity, stamped by the server), or ""
// when an agent or a machine opened it.
func threadOpener(createdBy string) string {
	who := strings.TrimPrefix(strings.TrimSpace(createdBy), "oidc:")
	if !strings.Contains(who, "@") {
		return ""
	}
	return "oidc:" + strings.ToLower(who)
}

// armsRegisterLimit bounds the register read, as kd heraldry check bounds it.
const armsRegisterLimit = 5000

// checkPlanArms is kd heraldry check run against land before the plan is
// written: it refuses arms another bead of the land bears, in the daemon's
// words, and warns of a bead bearing the same emoji in another order. The
// plan a retried write of the same ask already made (its idempotency key is
// key) is not another bead: the write finds it and refuses on its own.
func checkPlanArms(ctx context.Context, st PlanStore, land, run, key string) ([]string, error) {
	// The label filter as well, so a daemon that predates heraldry_project
	// still reads only the land's beads.
	res, err := st.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		HeraldryProject: land, Labels: []string{"project:" + land}, Limit: armsRegisterLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("reading the arms already given in %s: %w", land, err)
	}
	var warnings []string
	set := heraldry.RunSet(run)
	for _, b := range res.Beads {
		other := b.HeraldryRun()
		if other == "" || (b.Fields != nil && b.Fields["idempotency_key"] == key) {
			continue
		}
		switch {
		case other == run:
			return nil, fmt.Errorf("the plan's Arms: line: the arms %s are already borne by %s in project %s; arms are unique in a project, so choose other emoji or another order",
				run, armsBearer(b), land)
		case heraldry.RunSet(other) == set:
			warnings = append(warnings, fmt.Sprintf("the arms %s look like %s's, %s: the same emoji in another order, which the owner may find hard to tell apart",
				run, armsBearer(b), other))
		}
	}
	return warnings, nil
}

// armsBearer names a bead of the register as kd heraldry check does.
func armsBearer(b *beadsapi.BeadDetail) string {
	if b.ShortTitle == "" {
		return b.ID
	}
	return b.ID + " (" + b.ShortTitle + ")"
}

// noArmsWarning tells the agent that wrote a plan with no Arms: line to
// choose its arms before the owner looks at the card.
func noArmsWarning(planID, land, owner string) string {
	return fmt.Sprintf("plan %s has no arms. Choose three to five emoji that say what it is (first) and what it is for, before %s looks at the card: "+
		"kd heraldry library --search <word>, kd heraldry check \"<emoji>\" --project %s, then kd update %s --heraldry \"<emoji>\". "+
		"Next time put \"Arms: <emoji>\" under the plan's title.",
		planID, strings.TrimPrefix(owner, "oidc:"), land, planID)
}

// planKey is the plan epic's idempotency key: one ask, one plan.
func planKey(land, thread, replaces, title string) string {
	h := sha256.Sum256([]byte(strings.Join([]string{"steward-plan", land, thread, replaces, strings.TrimSpace(title)}, "\x00")))
	return "steward-plan:" + hex.EncodeToString(h[:12])
}

// planPrompt is the decision's one-line question.
func planPrompt(planID, title string) string {
	return fmt.Sprintf("Plan %s: %s - run it?", planID, title)
}

// threadMarker opens the line of the plan's description that records the
// thread the ask came in.
const threadMarker = "Asked in thread "

// planDescription is the epic's description: the plan's body, and where it
// came from.
func planDescription(d PlanDraft, opt WritePlanOptions) string {
	var b strings.Builder
	b.WriteString(d.Body)
	if opt.Thread != "" {
		fmt.Fprintf(&b, "\n\n%s%s.", threadMarker, opt.Thread)
	}
	if opt.Replaces != "" {
		fmt.Fprintf(&b, "\n\nReplaces plan %s.", opt.Replaces)
	}
	return strings.TrimSpace(b.String())
}

// planContext is what the owner reads on the card: what the plan does, its
// steps, and about what it costs.
func planContext(d PlanDraft, steps []string, opt WritePlanOptions) string {
	var b strings.Builder
	if d.Arms != "" {
		fmt.Fprintf(&b, "Arms: %s\n\n", d.Arms)
	}
	if d.Body != "" {
		b.WriteString(d.Body)
		b.WriteString("\n\n")
	}
	b.WriteString("Steps, one gardener and one pull request each, run in order:\n")
	for i, s := range d.Steps {
		fmt.Fprintf(&b, "%d. %s (%s)\n", i+1, s.Title, steps[i])
	}
	if opt.StepEstimateCredits > 0 {
		fmt.Fprintf(&b, "\nAbout %s credits: %d steps at about %s each.\n",
			formatCredits(opt.StepEstimateCredits*float64(len(d.Steps))), len(d.Steps), formatCredits(opt.StepEstimateCredits))
	}
	if opt.Replaces != "" {
		fmt.Fprintf(&b, "\nThis replaces plan %s, as you asked.\n", opt.Replaces)
	}
	return strings.TrimSpace(b.String())
}

// HomeOwner is the owner of the home garden, or "" when proj is not it. The
// home garden is an operator land on the Commons (its project bead has a
// land_default_channel) whose project bead names a person as its operator
// (operated_by oidc:<email>); that person is its owner. Any other operator
// land takes no plans: nobody there answers the card.
func HomeOwner(proj *beadsapi.BeadDetail) string {
	if proj == nil || proj.Fields == nil {
		return ""
	}
	if kind := strings.TrimSpace(proj.Fields["land_kind"]); kind != "" && kind != beadsapi.LandKindOperator {
		return ""
	}
	if strings.TrimSpace(proj.Fields["land_default_channel"]) == "" {
		return ""
	}
	if op := strings.TrimSpace(proj.OperatedBy); strings.HasPrefix(op, "oidc:") && strings.Contains(op, "@") {
		return op
	}
	return ""
}

// planLand checks land takes plans: a tenant land, or the home garden. For
// the home garden it returns its owner; "" for a tenant land.
func planLand(ctx context.Context, st PlanStore, land string) (string, error) {
	proj, tenant, err := purse.Project(ctx, st, land)
	if err != nil {
		return "", err
	}
	if tenant {
		return "", nil
	}
	if home := HomeOwner(proj); home != "" {
		return home, nil
	}
	return "", fmt.Errorf("%s is an operator land that is not the home garden (no Commons, or no person as its operator); plans are written in tenant lands and the home garden only", land)
}

// planOwner is who the plan's decision is for: the identity given, else the
// home garden's owner (home), else the land's one recorded owner.
func planOwner(ctx context.Context, st PlanStore, land, given, home string) (string, error) {
	if given = strings.TrimSpace(given); given != "" {
		if !strings.HasPrefix(given, "oidc:") {
			if !strings.Contains(given, "@") {
				return "", fmt.Errorf("--for %q: name the owner as oidc:<email> or an email", given)
			}
			given = "oidc:" + strings.ToLower(given)
		}
		return given, nil
	}
	if home != "" {
		return home, nil
	}
	m, ok := st.(landMembers)
	if !ok {
		return "", errors.New("cannot read the land's members; name the owner with --for")
	}
	people, err := m.LandMembers(ctx, land)
	if err != nil {
		return "", fmt.Errorf("reading %s's members: %w", land, err)
	}
	var owners []string
	for _, p := range people {
		if landRole(p.Role) == conversation.LandRoleOwner && p.Principal != "" {
			owners = append(owners, p.Principal)
		}
	}
	switch len(owners) {
	case 0:
		return "", fmt.Errorf("%s has no owner recorded yet; name them with --for, or ask the operator in #support", land)
	case 1:
		return owners[0], nil
	}
	return "", fmt.Errorf("%s has %d owners (%s); name the one to ask with --for", land, len(owners), strings.Join(owners, ", "))
}

// landPlan reads a plan epic and checks it is a plan of land.
func landPlan(ctx context.Context, st PlanStore, land, id string) (*beadsapi.BeadDetail, error) {
	b, err := st.GetBead(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("reading plan %s: %w", id, err)
	}
	if b == nil || b.Type != "epic" || !slices.Contains(b.Labels, plangardener.PlanLabel) {
		return nil, fmt.Errorf("%s is not a plan", id)
	}
	if !slices.Contains(b.Labels, "project:"+land) {
		return nil, fmt.Errorf("plan %s is not in %s", id, land)
	}
	return b, nil
}

// planSteps are a plan's steps in their run order: the steps of each phase in
// phase_order, then the steps under no phase. Each list runs in its parent's
// step_order first, then any child it does not name.
func planSteps(ctx context.Context, st PlanStore, planID string) ([]*beadsapi.BeadDetail, error) {
	plan, err := st.GetBead(ctx, planID)
	if err != nil {
		return nil, fmt.Errorf("reading plan %s: %w", planID, err)
	}
	loose, phases, err := childSteps(ctx, st, planID, plan)
	if err != nil {
		return nil, err
	}
	var out []*beadsapi.BeadDetail
	if plan != nil {
		for _, id := range stepOrder(plan.Fields["phase_order"]) {
			phase, ok := phases[id]
			if !ok {
				continue
			}
			steps, _, err := childSteps(ctx, st, id, phase)
			if err != nil {
				return nil, err
			}
			out = append(out, steps...)
		}
	}
	return append(out, loose...), nil
}

// childSteps reads parent's direct children: its task steps in step_order,
// and the phase containers among them by id.
func childSteps(ctx context.Context, st PlanStore, parentID string, parent *beadsapi.BeadDetail) ([]*beadsapi.BeadDetail, map[string]*beadsapi.BeadDetail, error) {
	deps, err := st.GetReverseDependencies(ctx, parentID)
	if err != nil {
		return nil, nil, fmt.Errorf("reading %s's steps: %w", parentID, err)
	}
	var steps []*beadsapi.BeadDetail
	phases := map[string]*beadsapi.BeadDetail{}
	for _, d := range deps {
		if d.Type != "parent-child" || d.DependsOnID != parentID {
			continue
		}
		b, err := st.GetBead(ctx, d.BeadID)
		if err != nil {
			return nil, nil, fmt.Errorf("reading step %s: %w", d.BeadID, err)
		}
		switch {
		case b != nil && b.Type == "task":
			steps = append(steps, b)
		case beadsapi.IsPhaseContainer(b):
			phases[b.ID] = b
		}
	}
	if parent != nil {
		order := stepOrder(parent.Fields["step_order"])
		slices.SortStableFunc(steps, func(a, b *beadsapi.BeadDetail) int {
			return orderIndex(order, a.ID) - orderIndex(order, b.ID)
		})
	}
	return steps, phases, nil
}

func stepOrder(raw string) []string {
	var ids []string
	_ = json.Unmarshal([]byte(raw), &ids)
	return ids
}

func orderIndex(order []string, id string) int {
	if i := slices.Index(order, id); i >= 0 {
		return i
	}
	return len(order)
}

// PlanAnswer is what gb lands plan answer did with a resolved plan decision.
type PlanAnswer struct {
	Decision string `json:"decision"`
	Plan     string `json:"plan"`
	Title    string `json:"title"`
	// Answer is approve, change or drop; dismissed when the owner dismissed
	// the card; "" when the answer was written rather than chosen.
	Answer string `json:"answer"`
	// Words are what the owner wrote with the answer, or instead of one.
	Words string `json:"words,omitempty"`
	By    string `json:"by,omitempty"`
	// State is the plan's plan_state after the answer was applied.
	State string `json:"state"`
	// Next is the first step not done yet (for approve), and NextStatus its
	// status: open means spawn a gardener on it, in_progress that one is on it.
	Next       string `json:"next,omitempty"`
	NextTitle  string `json:"next_title,omitempty"`
	NextStatus string `json:"next_status,omitempty"`
	// Thread is the thread the plan was asked in, when the plan says.
	Thread string `json:"thread,omitempty"`
	// Owner is who the decision asked (its operated_by).
	Owner string `json:"owner,omitempty"`
	// Home is true when land is the home garden: there the caller also
	// starts the plan's gardener on approve, as no steward lives there.
	Home bool `json:"home,omitempty"`
	// Applied is false when the plan was already past the answer (a second
	// run on the same wake), so nothing changed.
	Applied bool `json:"applied"`
}

// AnswerPlan applies the owner's answer to a plan decision of land: approve
// moves the plan ready → building, once every step has a body to build from;
// change and drop move it to abandoned and close it and its open steps. A dismissed card, or a written answer that
// chose no option, changes nothing: the steward reads the words and asks
// again. Re-running it on an answer already applied changes nothing.
func AnswerPlan(ctx context.Context, st PlanStore, land, decisionID, actor string) (*PlanAnswer, error) {
	dec, err := st.GetBead(ctx, decisionID)
	if err != nil {
		return nil, fmt.Errorf("reading decision %s: %w", decisionID, err)
	}
	if dec == nil || dec.Type != "decision" || !slices.Contains(dec.Labels, "project:"+land) {
		return nil, fmt.Errorf("%s is not a decision of %s", decisionID, land)
	}
	if dec.Status != "closed" {
		return nil, fmt.Errorf("decision %s is not answered yet", decisionID)
	}
	planID := strings.TrimSpace(dec.Fields["requesting_task"])
	if planID == "" {
		return nil, fmt.Errorf("decision %s names no plan (requesting_task)", decisionID)
	}
	plan, err := landPlan(ctx, st, land, planID)
	if err != nil {
		return nil, err
	}
	a := &PlanAnswer{
		Decision: decisionID, Plan: planID, Title: plan.Title,
		Answer: chosenOption(dec.Fields),
		Words:  strings.TrimSpace(dec.Fields["response_text"]),
		By:     strings.TrimSpace(dec.Fields["responded_by"]),
		State:  plan.Fields[planStateField],
		Thread: threadOf(plan.Description),
		Owner:  strings.TrimSpace(dec.Fields["operated_by"]),
	}
	if proj, tenant, err := purse.Project(ctx, st, land); err == nil && !tenant {
		a.Home = HomeOwner(proj) != ""
	}
	if a.Words == "" && a.Answer != "dismissed" {
		a.Words = userRationale(dec.Fields["rationale"])
	}

	switch a.Answer {
	case PlanApprove:
		steps, err := planSteps(ctx, st, planID)
		if err != nil {
			return nil, err
		}
		if a.State == "ready" {
			for i, s := range steps {
				// A closed step is never built, so its body does not matter.
				if s.Status == "closed" {
					continue
				}
				if err := checkStepBody(fmt.Sprintf("step %d %s (%s)", i+1, s.ID, s.Title), s.Description); err != nil {
					return nil, fmt.Errorf("plan %s cannot be approved yet: %w; fill it in with kd update %s -d '...' and run the answer again", planID, err, s.ID)
				}
			}
			if err := st.UpdateBeadFieldsTyped(ctx, planID, map[string]any{planStateField: "building"}); err != nil {
				return nil, fmt.Errorf("moving %s to building: %w", planID, err)
			}
			a.State, a.Applied = "building", true
			_ = st.AddComment(ctx, planID, actor, fmt.Sprintf("plan_state → building: approved on %s", decisionID))
		}
		for _, s := range steps {
			if s.Status != "closed" {
				a.Next, a.NextTitle, a.NextStatus = s.ID, s.Title, s.Status
				break
			}
		}
	case PlanChange, PlanDrop:
		if a.State == "abandoned" || plan.Status == "closed" {
			break
		}
		why := "dropped by the owner on " + decisionID
		if a.Answer == PlanChange {
			why = "the owner asked for a change on " + decisionID + "; a new plan replaces it"
		}
		if err := abandonPlan(ctx, st, plan, why, actor); err != nil {
			return nil, err
		}
		a.State, a.Applied = "abandoned", true
	}
	return a, nil
}

// abandonPlan closes a plan's open steps and the plan, plan_state abandoned.
func abandonPlan(ctx context.Context, st PlanStore, plan *beadsapi.BeadDetail, why, actor string) error {
	steps, err := planSteps(ctx, st, plan.ID)
	if err != nil {
		return err
	}
	for _, s := range steps {
		if s.Status == "closed" {
			continue
		}
		if err := st.CloseBeadAs(ctx, s.ID, actor, map[string]string{"close_reason": "plan " + plan.ID + " abandoned: " + why}); err != nil {
			return fmt.Errorf("closing step %s: %w", s.ID, err)
		}
	}
	if err := st.UpdateBeadFieldsTyped(ctx, plan.ID, map[string]any{planStateField: "abandoned"}); err != nil {
		return fmt.Errorf("abandoning %s: %w", plan.ID, err)
	}
	_ = st.AddComment(ctx, plan.ID, actor, "plan_state → abandoned: "+why)
	if err := st.CloseBeadAs(ctx, plan.ID, actor, map[string]string{"close_reason": "abandoned: " + why}); err != nil {
		return fmt.Errorf("closing %s: %w", plan.ID, err)
	}
	return nil
}

// chosenOption is the option a closed decision chose, by id: the card stores
// an option's id, or its 1-based number. A dismissed card is "dismissed"; a
// written answer with no option is "".
func chosenOption(f map[string]string) string {
	chosen := strings.TrimSpace(f["chosen"])
	if chosen == "dismissed" || f["canceled"] == "true" {
		return "dismissed"
	}
	if n, err := strconv.Atoi(chosen); err == nil && n >= 1 && n <= len(planOptions) {
		return planOptions[n-1]["id"]
	}
	for _, o := range planOptions {
		if strings.EqualFold(chosen, o["id"]) {
			return o["id"]
		}
	}
	return ""
}

// userRationale is a rationale a person wrote, not a surface's stamp
// ("Chosen by ... via the Commons").
func userRationale(r string) string {
	r = strings.TrimSpace(r)
	if strings.HasPrefix(r, "Chosen by ") {
		return ""
	}
	return r
}

// threadOf reads the thread line planDescription writes, or "".
func threadOf(desc string) string {
	i := strings.LastIndex(desc, threadMarker)
	if i < 0 {
		return ""
	}
	id, _, _ := strings.Cut(desc[i+len(threadMarker):], ".")
	if id == "" || strings.ContainsAny(id, " \n") {
		return ""
	}
	return id
}
