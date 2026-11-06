package gardenhub

import (
	"context"
	"encoding/json"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
	"gardenio/controller/internal/decisionview"
	"gardenio/controller/internal/kin"
	"gardenio/controller/internal/plangardener"
)

// Typed faces (Lands workbench-glass spec MUST row WB-1, step 5.3, gd-vFUZG6FnTT): GET /garden/bead/{id} carries a face
// block keyed by the bead's type, and web/workbench.js paints one renderer per kind above the description. The kinds are
// the spec's: work (task, bug, feature, chore), epic, plan (an epic with the plan label), decision, escalation, agent,
// message, bundle, report, gardener (a gardener loop: a role:gardener trigger or a bead carrying a gardener spec) and mr
// (merge-request), release (a type=release bead, bead_face_release.go), and wish (a thread head, gd-zn6iUnPd9Q). Any
// other type has no face and keeps the generic fields table. The beads viewer's type_* partials
// (cmd/beads-viewer/templates/partials) are the reference rendering; the field keys are theirs.
//
// Every value is read from the bead the panel already fetched, plus the beads a face names (who asked, the task, the last
// run), which are hydrated in the same bounded pass as the related beads. Nothing here reads a list but the live park: a
// gardener loop's probe, and for a plan the one label-filtered list that finds its gardener first (planGardener).

const (
	faceTextMax   = 4000 // runes of a prompt, a context or a result
	faceOptsMax   = 30   // decision options sent (the rubric's F3 has twelve and more)
	faceTargetMax = 6    // an escalation's links to act on
	faceDocsMax   = 40   // a bundle's documents
)

type beadFace struct {
	Kind       string          `json:"kind"`
	Work       *faceWork       `json:"work,omitempty"`
	Epic       *faceEpic       `json:"epic,omitempty"`
	Plan       *facePlan       `json:"plan,omitempty"`
	Decision   *faceDecision   `json:"decision,omitempty"`
	Escalation *faceEscalation `json:"escalation,omitempty"`
	Agent      *faceAgent      `json:"agent,omitempty"`
	Message    *faceMessage    `json:"message,omitempty"`
	Bundle     *faceBundle     `json:"bundle,omitempty"`
	Report     *faceReport     `json:"report,omitempty"`
	Gardener   *faceGardener   `json:"gardener,omitempty"`
	MR         *faceMR         `json:"mr,omitempty"`
	Release    *faceRelease    `json:"release,omitempty"`
	Wish       *faceWish       `json:"wish,omitempty"`
}

// faceWish is a thread head: who asked, where and when, the ask itself, and the counts of the thread's kin (kin.Doc),
// which the panel says as one line over the sections that list them.
type faceWish struct {
	Asker     string `json:"asker,omitempty"`     // oidc:<email>, slack:<user id>, or the writer the daemon stamped
	Where     string `json:"where,omitempty"`     // slack or commons (kin.Where)
	Channel   string `json:"channel,omitempty"`   // the Slack channel id
	Permalink string `json:"permalink,omitempty"` // the Slack thread, when the bridge recorded its link
	At        int64  `json:"at,omitempty"`
	Ask       string `json:"ask,omitempty"` // slack_root_text, or the Commons wish's text
	// Counted says the kin were read, so the counts below are the thread's; without it the panel draws none.
	Counted    bool `json:"counted,omitempty"`
	Replies    int  `json:"replies,omitempty"` // the thread's messages, cards, deleted ones and mentions aside
	AgentsLive int  `json:"agents_live,omitempty"`
	AgentsLeft int  `json:"agents_left,omitempty"`
	WorkOpen   int  `json:"work_open,omitempty"` // the roots of its work (kin.Work rows with no parent) by state
	WorkClosed int  `json:"work_closed,omitempty"`
	PRsMerged  int  `json:"prs_merged,omitempty"`
}

// wishFields are a wish head's fields its face shows, so the generic fields table does not repeat them.
var wishFields = map[string]bool{"slack_root_text": true, "text": true}

// faceMR is a pull request as a work bead or a merge-request bead names it.
type faceMR struct {
	URL       string `json:"url,omitempty"`
	State     string `json:"state,omitempty"`
	Number    string `json:"number,omitempty"`
	Title     string `json:"title,omitempty"`
	Pipeline  string `json:"pipeline,omitempty"`
	Source    string `json:"source,omitempty"`
	Target    string `json:"target,omitempty"`
	Author    string `json:"author,omitempty"`
	Approvals string `json:"approvals,omitempty"` // "given/required"
	Threads   string `json:"threads,omitempty"`   // unresolved review threads
	Commit    string `json:"commit,omitempty"`    // the last commit, short sha and subject
}

type faceWork struct {
	Assignee string  `json:"assignee,omitempty"`
	Owner    string  `json:"owner,omitempty"`
	MR       *faceMR `json:"mr,omitempty"`
	Due      int64   `json:"due,omitempty"`
	Deferred int64   `json:"deferred,omitempty"` // defer_until
	ClosedBy string  `json:"closed_by,omitempty"`
}

// faceEpic counts the epic's children by status (of the ones the panel lists; ChildrenMore says how many it did not).
type faceEpic struct {
	Total  int            `json:"total"`
	Counts map[string]int `json:"counts,omitempty"`
	More   int            `json:"more,omitempty"`
}

type facePlan struct {
	State  string `json:"state,omitempty"` // plan_state: draft, ready, building, complete, abandoned
	Phases int    `json:"phases"`
	Steps  int    `json:"steps,omitempty"` // step_order, when the plan keeps one
	// Gardener is the plan's bound gardener loop (planGardener) and GardenerState running, paused or retired, as its own
	// face says it. Dormant and DormantKind are its live park (gardenerPark), as on the gardener face.
	Gardener      *beadRel `json:"gardener,omitempty"`
	GardenerState string   `json:"gardener_state,omitempty"`
	Dormant       *beadRel `json:"dormant,omitempty"`
	DormantKind   string   `json:"dormant_kind,omitempty"`
}

type faceOption struct {
	ID       string `json:"id"`
	Short    string `json:"short,omitempty"`
	Label    string `json:"label,omitempty"`
	Artifact string `json:"artifact,omitempty"`
}

// faceAsker is who asked for a decision or an escalation, and for what: the agent (a bead when it is one, else its name),
// the task and the plan.
type faceAsker struct {
	Agent     *beadRel `json:"agent,omitempty"`
	AgentName string   `json:"agent_name,omitempty"`
	Task      *beadRel `json:"task,omitempty"`
	Plan      *beadRel `json:"plan,omitempty"`
}

type faceDecision struct {
	Prompt string `json:"prompt,omitempty"`
	// Context is the decision's background (the context field, else the
	// description), what the panel shows above the options so the question
	// can be answered where it is asked (gd-hmwSI9lwKe): the decision's body,
	// whole up to the description's budget (beadDescMax), as Markdown.
	Context string       `json:"context,omitempty"`
	Options []faceOption `json:"options,omitempty"`
	// State is open, answered, dismissed or expired; Open says the panel may answer it (status open, no answer yet).
	State     string     `json:"state"`
	Open      bool       `json:"open,omitempty"`
	Chosen    string     `json:"chosen,omitempty"` // the option id answered
	Answer    string     `json:"answer,omitempty"` // a free-text answer, or a dismissal's reason
	By        string     `json:"by,omitempty"`
	At        int64      `json:"at,omitempty"`
	AskedBy   *faceAsker `json:"asked_by,omitempty"`
	MoreOpts  int        `json:"more_options,omitempty"`
	Requested string     `json:"requested,omitempty"` // requested_by, when no agent is named
	// Evidence is the screenshots on the beads the decision is about (its requesting task, its --evidence beads), newest
	// first: the panel draws them under the context as the Mailbox does (web/workbench.js evidenceStrip, gd-Pi7C83262N).
	Evidence []faceEvidence `json:"evidence,omitempty"`
}

// faceEvidence is one evidence picture: its link as the bead holds it (the page loads it through the image proxy,
// web/workbench.js picSrc, or not at all), its alt text, and the bead it is on.
type faceEvidence struct {
	URL       string `json:"url"`
	Alt       string `json:"alt,omitempty"`
	From      string `json:"from,omitempty"`
	FromTitle string `json:"from_title,omitempty"`
}

func faceEvidenceOf(imgs []decisionview.Image) []faceEvidence {
	var out []faceEvidence
	for _, im := range imgs {
		out = append(out, faceEvidence{URL: im.URL, Alt: clip(im.Alt, 200), From: im.Source, FromTitle: clip(im.SourceTitle, 200)})
	}
	return out
}

type faceEscalation struct {
	Ask         string     `json:"ask,omitempty"` // context: why this needs a human
	Op          string     `json:"op,omitempty"`  // operation_type
	Sensitivity string     `json:"sensitivity,omitempty"`
	Audience    string     `json:"audience,omitempty"`
	Held        string     `json:"held,omitempty"` // AgentLaneHolder: its answer box offers only "Approve anyway" (gd-Mq0uDXHswK)
	State       string     `json:"state"`          // pending, approved, done, failed, rejected
	Open        bool       `json:"open,omitempty"`
	By          string     `json:"by,omitempty"` // who approved or rejected it
	At          int64      `json:"at,omitempty"`
	Method      string     `json:"method,omitempty"` // approval_method
	Reason      string     `json:"reason,omitempty"` // a rejection's
	Result      string     `json:"result,omitempty"`
	Executor    string     `json:"executor,omitempty"`
	Targets     []string   `json:"targets,omitempty"` // the http(s) links of its operation spec, an MR first
	AskedBy     *faceAsker `json:"asked_by,omitempty"`
}

type faceAgent struct {
	Name    string   `json:"name"`
	State   string   `json:"state,omitempty"` // agent_state
	Live    bool     `json:"live,omitempty"`  // still on the farm: the bead open and the state not a finished one
	Role    string   `json:"role,omitempty"`
	Project string   `json:"project,omitempty"`
	Mode    string   `json:"mode,omitempty"`
	Harness string   `json:"harness,omitempty"`
	Task    *beadRel `json:"task,omitempty"`
	Thread  *beadRel `json:"thread,omitempty"` // the thread head it answers (thread_id), when that is not its task (5b)
	By      string   `json:"spawned_by,omitempty"`
	CostUSD float64  `json:"cost_usd,omitempty"`
	Turns   int      `json:"turns,omitempty"`
	TLDR    string   `json:"tldr,omitempty"`
}

type faceMessage struct {
	From   string   `json:"from,omitempty"`
	Via    string   `json:"via"` // squawk, slack or message
	At     int64    `json:"at,omitempty"`
	Text   string   `json:"text,omitempty"` // fields.text, when it is not the description already shown
	Thread *beadRel `json:"thread,omitempty"`
}

type faceDoc struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Format string `json:"format,omitempty"`
}

type faceBundle struct {
	Docs []faceDoc `json:"docs,omitempty"`
	More int       `json:"more,omitempty"`
}

type faceReport struct {
	Type    string `json:"type,omitempty"` // report_type
	Format  string `json:"format,omitempty"`
	Content string `json:"content,omitempty"`
}

type faceGardener struct {
	Name          string `json:"name,omitempty"`
	Enabled       bool   `json:"enabled"`
	Retired       bool   `json:"retired,omitempty"` // the trigger bead is closed
	Steward       bool   `json:"steward,omitempty"` // a land's steward (farm_steward.go)
	Standing      bool   `json:"standing,omitempty"`
	Goal          string `json:"goal,omitempty"` // terminal_goal
	Cron          string `json:"cron,omitempty"`
	Timezone      string `json:"timezone,omitempty"`
	RunsSpawned   int    `json:"runs_spawned,omitempty"`
	RunsCompleted int    `json:"runs_completed,omitempty"`
	MaxRuns       int    `json:"max_runs,omitempty"`
	Failures      int    `json:"failures,omitempty"` // consecutive
	RunsDay       string `json:"runs_day,omitempty"` // YYYY-MM-DD, UTC: the day RunsToday counts (the panel compares it with today)
	RunsToday     int    `json:"runs_today,omitempty"`
	MaxPerDay     int    `json:"max_runs_per_day,omitempty"`
	// Dormant is the bead a parked loop waits on and DormantKind which park it is (bridge.ReconcileDormancy.Kind:
	// operator_wedge, mr_merged). Both are the live probe's (gardenerPark), never the stamped dormant_blocker.
	Dormant     *beadRel `json:"dormant,omitempty"`
	DormantKind string   `json:"dormant_kind,omitempty"`
	LastRun     *beadRel `json:"last_run,omitempty"`
}

// faceKindOf names the face a bead gets, "" for the generic fields table. A gardener loop is decided first, as the bead
// page decides it (isGardenerBeadView), but an agent bead is always an agent.
func faceKindOf(b *beadsapi.BeadDetail) string {
	if b.Type != "agent" && ((b.Type == "trigger" && hasLabel(b, "role:gardener")) || strings.TrimSpace(b.Fields["gardener"]) != "") {
		return "gardener"
	}
	switch b.Type {
	case "task", "bug", "feature", "chore":
		if hasLabel(b, "thread") {
			return "wish"
		}
		return "work"
	case "epic":
		if hasLabel(b, "plan") {
			return "plan"
		}
		return "epic"
	case "merge-request":
		return "mr"
	case "decision", "escalation", "agent", "message", "bundle", "report", "release":
		return b.Type
	}
	return ""
}

// buildFace reads the face from the bead itself. It returns the beads the face names, for hydrate to title; finishFace
// then fills what needs the related beads read (an epic's counts, a bundle's documents).
func buildFace(b *beadsapi.BeadDetail) (*beadFace, []*beadRel) {
	kind := faceKindOf(b)
	if kind == "" {
		return nil, nil
	}
	f, fl := &beadFace{Kind: kind}, b.Fields
	var refs []*beadRel
	ref := func(id string) *beadRel {
		id = strings.TrimSpace(id)
		if !beadRefRe.MatchString(id) || id == b.ID {
			return nil
		}
		r := &beadRel{ID: id}
		refs = append(refs, r)
		return r
	}
	asker := func(agentRefs ...string) *faceAsker {
		a := &faceAsker{Task: ref(fl["requesting_task"]), Plan: ref(fl["requesting_plan"])}
		for _, s := range agentRefs {
			if s = strings.TrimSpace(s); s == "" {
				continue
			}
			if a.Agent = ref(s); a.Agent == nil {
				a.AgentName = s
			}
			break
		}
		if a.Agent == nil && a.AgentName == "" && a.Task == nil && a.Plan == nil {
			return nil
		}
		return a
	}
	switch kind {
	case "work":
		f.Work = &faceWork{Assignee: b.Assignee, Owner: b.Owner, MR: faceMROf(fl, false, ""), Due: fieldUnix(firstNonEmpty(b.DueAt, fl["due_at"])),
			Deferred: fieldUnix(firstNonEmpty(b.DeferUntil, fl["defer_until"])), ClosedBy: b.ClosedBy}
	case "epic":
		f.Epic = &faceEpic{}
	case "plan":
		f.Plan = &facePlan{State: fl["plan_state"], Phases: len(idList(fl["phase_order"])), Steps: len(idList(fl["step_order"]))}
	case "decision":
		f.Decision = decisionFace(b)
		f.Decision.AskedBy = asker(fl["requesting_agent_bead_id"], fl["requesting_agent"], fl["requested_by"])
		if f.Decision.AskedBy == nil {
			f.Decision.Requested = b.CreatedBy
		}
	case "escalation":
		f.Escalation = escalationFace(b)
		f.Escalation.AskedBy = asker(fl["requesting_agent"])
	case "agent":
		state := fl["agent_state"]
		f.Agent = &faceAgent{Name: firstNonEmpty(fl["agent"], b.Title), State: state, Role: fl["role"], Project: firstNonEmpty(fl["project"], projectOf(b)),
			Mode: fl["mode"], Harness: firstNonEmpty(fl["resolved_harness"], fl["harness"]), Task: ref(fl["task_id"]), By: fl["spawned_by"],
			CostUSD: agentCost(fl), Turns: firstInt(fl["total_request_count"], fl["live_turns"]), TLDR: wrapTLDR(fl["wrapup"]),
			Live: b.Status != "closed" && !leavingAgentStates[state] && !deadAgentStates[state]}
		if head := threadHead(b); head != strings.TrimSpace(fl["task_id"]) {
			f.Agent.Thread = ref(head)
		}
	case "message":
		via := "message"
		switch {
		case hasLabel(b, "squawk"):
			via = "squawk"
		case strings.Contains(fl["source"], "slack") || hasLabel(b, "slack-thread-reply") || hasLabel(b, "slack-mention") || labelPrefixed(b, "source:slack"):
			via = "slack"
		}
		m := &faceMessage{Via: via, From: firstNonEmpty(labelValue(b, "from:"), fl["source_agent"], b.CreatedBy, b.OperatedBy), Thread: ref(fl["thread_id"])}
		if !b.CreatedAt.IsZero() {
			m.At = b.CreatedAt.Unix()
		}
		if t := strings.TrimSpace(fl["text"]); t != "" && t != strings.TrimSpace(b.Description) {
			m.Text = clipKeep(t, faceTextMax)
		}
		f.Message = m
	case "bundle":
		f.Bundle = &faceBundle{}
	case "report":
		f.Report = &faceReport{Type: fl["report_type"], Format: fl["format"], Content: clipKeep(fl["content"], beadDescMax)}
	case "gardener":
		var last string
		f.Gardener, last = gardenerFace(b)
		f.Gardener.LastRun = ref(last)
	case "mr":
		f.MR = faceMROf(fl, true, b.CreatedBy)
	case "release":
		f.Release = releaseFace(fl, ref)
	case "wish":
		f.Wish = wishFace(b)
	}
	return f, refs
}

// wishFace reads a thread head as the bridge or the Commons wrote it. The asker of a Slack thread is its opener as the
// bridge resolved it into operated_by (an oidc: person, else the slack: sentinel), else slack:<slack_opener_user_id>; a
// Commons wish's is its author (kin.Author). The ask is the text the Commons shows for it (fields.text, else
// slack_root_text).
func wishFace(b *beadsapi.BeadDetail) *faceWish {
	fl := b.Fields
	w := &faceWish{Ask: clipKeep(firstNonEmpty(fl["text"], fl["slack_root_text"]), faceTextMax), Permalink: httpURL(fl["slack_permalink"])}
	w.Where, w.Channel = kin.Where(b)
	if uid := strings.TrimSpace(fl["slack_opener_user_id"]); uid != "" {
		w.Asker = "slack:" + uid
		if op := strings.TrimSpace(fl["operated_by"]); strings.HasPrefix(op, "oidc:") || strings.HasPrefix(op, "slack:") {
			w.Asker = op
		}
	} else {
		w.Asker = kin.Author(b)
	}
	if !b.CreatedAt.IsZero() {
		w.At = b.CreatedAt.Unix()
	} else if sec, _, _ := strings.Cut(fl["slack_root_at"], "."); sec != "" {
		w.At, _ = strconv.ParseInt(sec, 10, 64)
	}
	return w
}

// countWish fills a wish's counts from its thread's kin; a thread whose kin were not read keeps none.
func countWish(w *faceWish, k *kin.Doc) {
	if w == nil || k == nil {
		return
	}
	w.Counted = true
	for _, l := range k.Conversation {
		if l.Via != kin.ViaCard && l.Via != kin.ViaMention && !l.Deleted {
			w.Replies++
		}
	}
	for _, a := range k.Agents {
		if a.Live {
			w.AgentsLive++
		} else {
			w.AgentsLeft++
		}
	}
	for _, x := range k.Work {
		if x.PRState == "merged" {
			w.PRsMerged++
		}
		if x.Parent != "" {
			continue
		}
		switch x.State {
		case "closed", "complete", "abandoned":
			w.WorkClosed++
		default:
			w.WorkOpen++
		}
	}
}

// finishFace fills the parts of the face that come from the related beads the panel read: an epic's children by status,
// a bundle's documents in their position order (the bundle page's table of contents), and a wish's counts of its kin.
func finishFace(f *beadFace, d *beadDoc, got map[string]*beadsapi.BeadDetail) {
	if f == nil {
		return
	}
	switch {
	case f.Wish != nil:
		countWish(f.Wish, d.Kin)
	case f.Epic != nil:
		f.Epic.Counts = map[string]int{}
		for _, c := range d.Children {
			if c.Status != "" {
				f.Epic.Counts[c.Status]++
			}
			f.Epic.Total++
		}
		f.Epic.More = d.RelMore
	case f.Bundle != nil:
		type doc struct {
			faceDoc
			pos int
		}
		var docs []doc
		for _, c := range d.Children {
			b := got[c.ID]
			if b == nil || b.Type != "doc" {
				continue
			}
			pos, err := strconv.Atoi(strings.TrimSpace(b.Fields["position"]))
			if err != nil {
				pos = 1 << 30
			}
			docs = append(docs, doc{faceDoc{ID: b.ID, Title: firstNonEmpty(b.Fields["headline"], b.Title), Format: b.Fields["format"]}, pos})
		}
		sort.SliceStable(docs, func(i, j int) bool { return docs[i].pos < docs[j].pos })
		for i, x := range docs {
			if i >= faceDocsMax {
				f.Bundle.More = len(docs) - faceDocsMax
				break
			}
			f.Bundle.Docs = append(f.Bundle.Docs, x.faceDoc)
		}
	}
}

// faceMROf is the pull request a bead's fields name, nil when they name none. A merge-request bead (own) also answers to
// its bare url and state keys (type_mr.html), and its creator is the fallback author.
func faceMROf(fl map[string]string, own bool, author string) *faceMR {
	var url, state string
	if own {
		url, state = firstNonEmpty(fl["url"], fl["gitlab_mr_url"]), fl["state"]
	}
	m := &faceMR{
		URL:      httpURL(firstNonEmpty(fl["mr_url"], fl["pr_url"], url)),
		State:    firstNonEmpty(fl["mr_state"], state),
		Number:   firstNonEmpty(fl["mr_iid"], fl["pr_number"]),
		Title:    fl["mr_title"],
		Pipeline: firstNonEmpty(fl["mr_pipeline_status"], fl["pipeline_status"], fl["ci_status"], fl["ci_state"]),
		Source:   firstNonEmpty(fl["source_branch"], fl["branch"]),
		Target:   fl["target_branch"],
		Threads:  fl["unresolved_threads"],
	}
	if !own && m.URL == "" && m.State == "" && m.Number == "" {
		return nil
	}
	if own {
		m.Author = firstNonEmpty(fl["author"], author)
	}
	if g, r := strings.TrimSpace(fl["approvals_given"]), strings.TrimSpace(fl["approvals_required"]); g != "" || r != "" {
		m.Approvals = firstNonEmpty(g, "0") + "/" + firstNonEmpty(r, "?")
	}
	if sha := strings.TrimSpace(fl["last_commit_sha"]); sha != "" {
		if len(sha) > 8 {
			sha = sha[:8]
		}
		m.Commit = strings.TrimSpace(sha + " " + clip(fl["last_commit_message"], 120))
	}
	return m
}

// The sentinels the resolution machinery writes into chosen (cmd/beads-viewer decisionResolutionSentinels).
var decisionSentinels = map[string]bool{"dismissed": true, "_expired": true, "promoted_to_escalation": true, "returned_to_agent_for_escalation": true}

// decisionFace reads a decision as the bead page's decision card does (buildDecisionView): the prompt, the options, and
// the resolution from its flat fields (chosen, response_text, responded_by/at, canceled and its reason). A chosen that
// names an option by its short or label is matched back to its id (normalizeChosen); one that matches none is a
// free-text answer.
func decisionFace(b *beadsapi.BeadDetail) *faceDecision {
	fl := b.Fields
	dv := decisionview.From(b)
	d := &faceDecision{Prompt: clipKeep(dv.Prompt, faceTextMax), Context: clipKeep(dv.Context, beadDescMax), State: "open"}
	for _, v := range dv.Options {
		o := faceOption{ID: v.ID, Short: v.Short, Label: clip(v.Label, 400), Artifact: v.ArtifactType}
		if o.ID == "" && o.Label == "" {
			continue
		}
		if len(d.Options) >= faceOptsMax {
			d.MoreOpts++
			continue
		}
		d.Options = append(d.Options, o)
	}
	chosen, text := strings.TrimSpace(fl["chosen"]), strings.TrimSpace(fl["response_text"])
	d.By, d.At = fl["responded_by"], fieldUnix(fl["responded_at"])
	switch {
	case fl["canceled"] == "true" || chosen == "dismissed":
		d.State, d.Answer, d.By = "dismissed", clipKeep(firstNonEmpty(fl["cancel_reason"], text), faceTextMax), firstNonEmpty(fl["canceled_by"], d.By)
	case chosen == "_expired":
		d.State = "expired"
	case chosen != "" || text != "":
		d.State, d.Answer = "answered", clipKeep(text, faceTextMax)
		if chosen != "" && !decisionSentinels[chosen] {
			if id := matchOption(d.Options, chosen); id != "" {
				d.Chosen = id
			} else if len(d.Options) > 0 && d.Answer == "" {
				d.Answer = clipKeep(chosen, faceTextMax)
			} else if len(d.Options) == 0 {
				d.Chosen = chosen
			}
		}
	case b.Status == "closed":
		d.State = "answered"
	}
	d.Open = d.State == "open" && b.Status == "open"
	return d
}

// matchOption is the id of the option chosen names: by id, then short, then label, ignoring case.
func matchOption(opts []faceOption, chosen string) string {
	needle := strings.ToLower(chosen)
	for _, pick := range []func(faceOption) string{
		func(o faceOption) string { return o.ID }, func(o faceOption) string { return o.Short }, func(o faceOption) string { return o.Label },
	} {
		for _, o := range opts {
			if v := pick(o); v != "" && o.ID != "" && strings.ToLower(v) == needle {
				return o.ID
			}
		}
	}
	return ""
}

// faceExecutor is an escalation's executor in its Run by row when it is the synthetic agent "escalation-<id>" spawned to run
// it: no name to show (farm_waits.go syntheticExecRe; a wait words it "its executor").
const faceExecutor = "the escalation executor"

// EscalationState is where an escalation stands, as its face reads it (escalationFace): pending, approved, done, failed or
// rejected. Only a pending one may be approved or rejected; an approved one is in_progress with approved_by set, so a
// list of open + in_progress escalations (the Queues panel's tab) cannot tell them apart by status (gd-yuOG6cX6VI).
func EscalationState(b *beadsapi.BeadDetail) string {
	if b == nil {
		return ""
	}
	return escalationFace(b).State
}

// escalationFace reads an escalation as the bead page's escalation card does (buildEscalationView): rejected when someone
// rejected it (or it closed with a reason), approved once approved_by is set, and done or failed once its run wrote
// result_status; pending until then. Open says the panel may approve or reject it.
func escalationFace(b *beadsapi.BeadDetail) *faceEscalation {
	fl := b.Fields
	e := &faceEscalation{Ask: clipKeep(fl["context"], faceTextMax), Op: fl["operation_type"], Sensitivity: fl["sensitivity"],
		Audience: string(beadsapi.EscalationAudience(fl)), Held: AgentLaneHolder(fl), Executor: fl["executor"], State: "pending"}
	if syntheticExecRe.MatchString(strings.TrimSpace(e.Executor)) {
		e.Executor = faceExecutor
	}
	reason := firstNonEmpty(fl["rejection_reason"], fl["reject_reason"])
	approvedBy := firstNonEmpty(fl["approved_by_email"], fl["approved_by"])
	switch {
	case fl["rejected_by"] != "" || (b.Status == "closed" && reason != "") || (fl["result_status"] == "failure" && strings.Contains(fl["result"], "rejected")):
		e.State, e.By, e.Reason, e.At = "rejected", fl["rejected_by"], clipKeep(reason, faceTextMax), fieldUnix(fl["rejected_at"])
	case approvedBy != "" || strings.TrimSpace(fl["approved_at"]) != "":
		e.State, e.By, e.At, e.Method = "approved", approvedBy, fieldUnix(fl["approved_at"]), fl["approval_method"]
		switch fl["result_status"] {
		case "success":
			e.State = "done"
		case "failure":
			e.State = "failed"
		}
	}
	if fl["result_status"] != "" && e.State != "rejected" {
		e.Result = clipKeep(firstNonEmpty(resultText(fl["result"]), fl["result_status"]), faceTextMax)
	}
	e.Open = e.State == "pending" && (b.Status == "open" || b.Status == "in_progress")
	// the links the operation acts on: an MR first, then any other http(s) value of its spec
	var spec map[string]any
	if json.Unmarshal([]byte(fl["operation_spec"]), &spec) == nil {
		if u := httpURL(str(spec["mr_url"])); u != "" {
			e.Targets = append(e.Targets, u)
		}
		keys := make([]string, 0, len(spec))
		for k := range spec {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if u := httpURL(str(spec[k])); u != "" && k != "mr_url" && len(e.Targets) < faceTargetMax {
				e.Targets = append(e.Targets, u)
			}
		}
	}
	return e
}

// resultText is a run's result as a sentence: a JSON object's summary, message or error, else the text as written.
func resultText(raw string) string {
	var m map[string]any
	if json.Unmarshal([]byte(raw), &m) == nil {
		for _, k := range []string{"summary", "message", "error", "detail"} {
			if s := str(m[k]); s != "" {
				return s
			}
		}
		return ""
	}
	return strings.TrimSpace(raw)
}

// gardenerFace reads a gardener loop: its trigger bead's enabled and schedule fields, and its gardener blob's goal and
// run counters (bridge.GardenerSpec's keys), and the agent bead of its last run.
func gardenerFace(b *beadsapi.BeadDetail) (*faceGardener, string) {
	fl := b.Fields
	var spec struct {
		Name          string `json:"name"`
		TerminalGoal  string `json:"terminal_goal"`
		Standing      bool   `json:"standing"`
		MaxRuns       int    `json:"max_runs"`
		MaxRunsPerDay int    `json:"max_runs_per_day"`
		RunsDay       string `json:"runs_day"`
		RunsToday     int    `json:"runs_today"`
		RunsSpawned   int    `json:"runs_spawned"`
		RunsCompleted int    `json:"runs_completed"`
		Failures      int    `json:"consecutive_failures"`
		LastAgentID   string `json:"last_agent_id"`
	}
	_ = json.Unmarshal([]byte(fl["gardener"]), &spec)
	g := &faceGardener{Name: firstNonEmpty(spec.Name, b.Title), Enabled: fl["enabled"] != "false" && b.Status != "closed", Retired: b.Status == "closed",
		Steward: hasLabel(b, StewardMarkerLabel), Standing: spec.Standing || hasLabel(b, "perpetual:true"), Goal: clipKeep(spec.TerminalGoal, faceTextMax),
		Cron: fl["cron"], Timezone: fl["timezone"], RunsSpawned: spec.RunsSpawned, RunsCompleted: spec.RunsCompleted, MaxRuns: spec.MaxRuns,
		Failures: spec.Failures, MaxPerDay: spec.MaxRunsPerDay, RunsDay: spec.RunsDay, RunsToday: spec.RunsToday}
	return g, spec.LastAgentID
}

// gardenerPark is the live park of a running gardener loop, or nil: bridge.ReconcileDormancyProbe, the probe `gb
// trigger reconcile` and the /plans/{id} cockpit use, over the panel's reader with every write refused. The gardener
// blob's dormant_on_escalation and dormant_blocker are never read for it: the blocker is write-once and goes stale when
// the wedge moves to another bead, and the flag stays set after the loop wakes until its next fire (gd-yK7bWh9s3W). A
// paused or retired loop is not probed (it says why it fires nothing), nor is one on a reader that cannot list. Fail-safe
// toward nil, as the probe is: only a proven park with a blocker renders, and a panic reads as not parked.
func gardenerPark(ctx context.Context, src BeadReader, b *beadsapi.BeadDetail) (d *bridge.ReconcileDormancy) {
	r, ok := src.(bridge.DormancyReader)
	if !ok || faceKindOf(b) != "gardener" || b.Status == "closed" || b.Fields["enabled"] == "false" {
		return nil
	}
	defer func() {
		if recover() != nil {
			d = nil
		}
	}()
	d = gardenerDormancyProbe(ctx, r, quietLog, b)
	if d == nil || !d.Parked || strings.TrimSpace(d.LiveBlocker) == "" {
		return nil
	}
	return d
}

// planGardenerMax caps the list that finds a plan's gardener: a plan binds one, so more is a stale or duplicate bind.
const planGardenerMax = 20

// planGardener is the gardener loop bound to a plan, or nil: a trigger or schedule bead pinned to it by the
// gardener-epic:<plan> label (plangardener.GardenerEpicLabel), picked as the /plans cockpit picks it (beads-viewer
// plans_gardener.go planGardener): open over closed, then the plan-gardener marker, then the freshest. It costs one
// daemon-side label-filtered list, made only for a plan's face on a reader that can list; a failed list is no gardener.
func planGardener(ctx context.Context, src BeadReader, b *beadsapi.BeadDetail) *beadsapi.BeadDetail {
	r, ok := src.(bridge.DormancyReader)
	if !ok || faceKindOf(b) != "plan" {
		return nil
	}
	label := plangardener.GardenerEpicLabel(b.ID)
	res, err := r.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{Types: []string{"trigger", "schedule"}, Labels: []string{label}, Limit: planGardenerMax})
	if err != nil || res == nil {
		return nil
	}
	var best *beadsapi.BeadDetail
	for _, g := range res.Beads {
		// the daemon filtered on the label already; a reader that did not cannot name another loop as this plan's
		if g == nil || !hasLabel(g, label) || faceKindOf(g) != "gardener" {
			continue
		}
		if best == nil || planGardenerBetter(g, best) {
			best = g
		}
	}
	return best
}

// planGardenerBetter says a is the plan's gardener before b: open beats closed, then the plan-gardener marker, then the
// one updated last.
func planGardenerBetter(a, b *beadsapi.BeadDetail) bool {
	if ao, bo := a.Status != "closed", b.Status != "closed"; ao != bo {
		return ao
	}
	if am, bm := hasLabel(a, plangardener.MarkerLabel), hasLabel(b, plangardener.MarkerLabel); am != bm {
		return am
	}
	return a.UpdatedAt.After(b.UpdatedAt)
}

// loopState is a gardener loop's state as its face says it: retired (closed), paused (enabled false) or running.
func loopState(g *beadsapi.BeadDetail) string {
	switch {
	case g.Status == "closed":
		return "retired"
	case g.Fields["enabled"] == "false":
		return "paused"
	}
	return "running"
}

// facePark is the live park a bead's face shows, and the plan's gardener it is of: a plan's bound gardener and its park,
// or a gardener loop's own park (with no gardener returned). Both nil for anything else.
func facePark(ctx context.Context, src BeadReader, b *beadsapi.BeadDetail) (*beadsapi.BeadDetail, *bridge.ReconcileDormancy) {
	if g := planGardener(ctx, src, b); g != nil {
		return g, gardenerPark(ctx, src, g)
	}
	return nil, gardenerPark(ctx, src, b)
}

// gardenerDormancyProbe is the live probe gardenerPark calls; tests replace it.
var gardenerDormancyProbe = bridge.ReconcileDormancyProbeReadOnly

// quietLog drops the probe's fail-safe warnings: on a panel read they would log every click on a gardener.
var quietLog = slog.New(slog.DiscardHandler)

// idList is a JSON array of ids (phase_order, step_order), empty when it is not one.
func idList(raw string) []string {
	var ids []string
	if json.Unmarshal([]byte(raw), &ids) != nil {
		return nil
	}
	return ids
}

func firstInt(ss ...string) int {
	for _, s := range ss {
		if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

func str(v any) string { s, _ := v.(string); return strings.TrimSpace(s) }

var httpRe = regexp.MustCompile(`^https?://\S+$`)

// httpURL is s when it is an http(s) URL, else "": a face links nothing else.
func httpURL(s string) string {
	s = strings.TrimSpace(s)
	if !httpRe.MatchString(s) {
		return ""
	}
	return s
}

func labelValue(b *beadsapi.BeadDetail, prefix string) string {
	for _, l := range b.Labels {
		if strings.HasPrefix(l, prefix) && len(l) > len(prefix) {
			return l[len(prefix):]
		}
	}
	return ""
}

func labelPrefixed(b *beadsapi.BeadDetail, prefix string) bool { return labelValue(b, prefix) != "" }
