package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"gardenio/controller/internal/plangardener"
	"gardenio/controller/internal/steward"

	"github.com/spf13/cobra"
)

// gb lands plan is the steward's conversation-to-plan helper (spec §4.2 item
// 2, step 9.4): write turns a plan the steward wrote as markdown into the
// land's plan epic and ordered steps and asks the owner, in one go; answer
// applies the owner's answer when the decision closes. The home garden (the
// operator land on the Commons, steward.HomeOwner) takes the same two verbs
// from its thread agents (step 9.11); there an approve also starts the plan's
// gardener, since no steward lives there to spawn one. A plan the home
// garden's owner asked for in their own thread skips the card (P8.7): write
// starts it building and starts its gardener itself.

var landsPlanCmd = &cobra.Command{
	Use:   "plan",
	Short: "A steward's plans: write one from a conversation and ask the owner, then apply the answer",
}

var landsPlanWriteCmd = &cobra.Command{
	Use:   "write <land>",
	Short: "Write a plan in the land and file the owner's approve / change / drop decision",
	Long: `Reads a plan written as markdown (--file, or - for stdin):

  # <plan title>
  Arms: <three to five emoji>
  <what the plan does and why, for the owner>
  ## <step 1 title>
  <what the step changes, for its gardener>
  ## <step 2 title>
  ...

and writes it in the land: an epic labelled plan and project:<land>, born
draft, bearing the arms, one task per step (at most 12), each blocked by the
one before so they run in order, then plan_state ready. It files one
decision to the land's owner, "Plan <id>: <title> - run it?", with approve,
change and drop, and the plan reads blocked on it until it is answered. The
card and the reply in the thread show the arms.

The Arms: line gives the plan its arms: the emoji in a row, chosen by the
agent that writes the plan (find them with kd heraldry library --search
<word>). They are checked as kd heraldry check checks them, in the library,
three to five, and borne by no other bead of the land; arms that fail are
refused with its words and nothing is written. A plan without an Arms: line
is written, with a warning to give it arms with kd update <plan> --heraldry
before the owner looks.

<land> is a tenant land, or the home garden: the operator land on the
Commons, whose owner is the person its project bead names as operator. Each
step needs a body of at least 200 characters: what it changes, where, and how
to tell it is done, because the gardener that takes it reads only the bead.

In the home garden, a plan written by the agent of a thread the owner
opened, in that thread, is what the owner asked for: it is written straight
to building with no card, its gardener is bound and started, and the reply
in the thread tells the owner how to change or drop it. --propose files the
card anyway, for a plan that is the agent's own idea rather than the
owner's ask. A tenant land's plans always go to the owner on a card.

--thread is the message the ask came in: the plan records it, and the reply
there names the plan. Without it the thread is the agent's own (the thread
head on its agent bead), else the reply goes to the agent's home thread.
--for names the owner when the land has none recorded or several. --replaces is the plan this one replaces after the owner asked
for a change; a plan that starts building abandons the plan it replaces.

A retried write of the same plan for the same ask finds the plan it wrote and
refuses rather than asking twice.`,
	Example: `  gb lands plan write tomato-patch --file plan.md --thread gd-msg123
  gb lands plan write tomato-patch --file plan.md --thread gd-msg123 --replaces gd-plan1`,
	Args: cobra.ExactArgs(1),
	RunE: runLandsPlanWrite,
}

var landsPlanAnswerCmd = &cobra.Command{
	Use:   "answer <land> <decision-id>",
	Short: "Apply the owner's answer to a plan decision",
	Long: `Reads a closed plan decision and applies the answer:

  approve  plan_state building; prints the first step not done yet, to spawn
           a gardener on. In the home garden it also binds the plan's
           gardener, enables it and fires its first cycle, so the plan
           builds with nobody running another command. A plan with a step
           whose body is under 200 characters is not approved: the error
           names the step to fill in.
  change   the plan is abandoned and closed with its open steps; write the
           changed plan with gb lands plan write --replaces <plan>
  drop     the plan is abandoned and closed with its open steps

A dismissed card, or an answer written instead of chosen, changes nothing:
read the owner's words and ask again. Re-running it changes nothing.

An answer the owner chose in their own session (an operator-lane human)
within the last day is applied by the server too, the same way, so the
owner clicks once. If this command fails on such an answer
(a 403 on a bead the agent cannot write), it says so: do not escalate, the
server applies it.`,
	Example: `  gb lands plan answer tomato-patch gd-dec456`,
	Args:    cobra.ExactArgs(2),
	RunE:    runLandsPlanAnswer,
}

func runLandsPlanWrite(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	land := args[0]
	file, _ := cmd.Flags().GetString("file")
	thread, _ := cmd.Flags().GetString("thread")
	forOwner, _ := cmd.Flags().GetString("for")
	replaces, _ := cmd.Flags().GetString("replaces")
	propose, _ := cmd.Flags().GetBool("propose")
	if file == "" {
		return errors.New("--file is required: the plan as markdown (- for stdin)")
	}
	var raw []byte
	var err error
	if file == "-" {
		raw, err = io.ReadAll(os.Stdin)
	} else {
		raw, err = os.ReadFile(file)
	}
	if err != nil {
		return fmt.Errorf("reading %s: %w", file, err)
	}
	draft, err := steward.ParsePlan(string(raw))
	if err != nil {
		return err
	}

	agentID, _ := resolveAgentID("")
	agentThread := homeThreadHead(ctx)
	if thread = strings.TrimSpace(thread); thread == "" {
		thread = agentThread
	}
	res, err := steward.WritePlan(ctx, daemon, land, draft, steward.WritePlanOptions{
		Thread:              thread,
		Owner:               forOwner,
		Replaces:            strings.TrimSpace(replaces),
		StepEstimateCredits: steward.LandStepEstimate(ctx, daemon, land, time.Now()).Credits,
		Actor:               actor,
		Agent:               agentID,
		GiftedBy:            resolveAuthorOperator(ctx, agentID),
		AgentThread:         agentThread,
		Propose:             propose,
	})
	if err != nil {
		return err
	}
	var started *homePlanStart
	if res.Building {
		if started, err = startHomePlan(ctx, res.Plan, res.Owner); err != nil {
			fmt.Fprintf(os.Stderr, "WARNING: plan %s is building, but starting its gardener failed: %v\nRun gb plan gardener %s, then gb gardener fire-now %s\n", res.Plan, err, res.Plan, res.Plan)
		}
		if old := strings.TrimSpace(replaces); old != "" {
			abandonReplacedPlan(ctx, old, res)
		}
	}

	for _, w := range res.Warnings {
		fmt.Fprintf(os.Stderr, "WARNING: %s\n", w)
	}
	replied, err := replyPlanWritten(cmd, res, draft)
	if err != nil && res.Thread != "" {
		fmt.Fprintf(os.Stderr, "WARNING: the plan is written and the owner asked, but the reply in %s failed: %v\nSay it yourself: gb say --thread %s \"...\"\n", res.Thread, err, res.Thread)
	} else if err != nil {
		fmt.Fprintf(os.Stderr, "note: no thread to reply in (%v); say the plan id where the ask came in\n", err)
	}
	if jsonOutput {
		return json.NewEncoder(os.Stdout).Encode(struct {
			*steward.WrittenPlan
			Reply    string         `json:"reply,omitempty"`
			Gardener *homePlanStart `json:"gardener,omitempty"`
		}{res, replied, started})
	}
	fmt.Printf("plan %s: %s\n", res.Plan, withArms(res.Arms, draft.Title))
	for i, s := range res.Steps {
		fmt.Printf("  step %d %s: %s\n", i+1, s, draft.Steps[i].Title)
	}
	if res.Building {
		fmt.Printf("building: %s asked for it in thread %s, so no card\n", strings.TrimPrefix(res.Owner, "oidc:"), res.Thread)
		if started != nil {
			fmt.Print(describeHomePlanStart(started))
		}
	} else {
		fmt.Printf("asked %s on decision %s\n", res.Owner, res.Decision)
	}
	if replied != "" {
		fmt.Printf("replied in %s (%s)\n", res.Thread, replied)
	}
	return nil
}

// replyPlanWritten tells the thread that asked that the plan is written and
// who is asked to approve it: the plan's thread, else the agent's home thread.
func replyPlanWritten(cmd *cobra.Command, res *steward.WrittenPlan, d steward.PlanDraft) (string, error) {
	bridgeURL := resolveBridgeURL()
	if bridgeURL == "" {
		return "", errors.New("SLACK_BRIDGE_URL is not set")
	}
	p := newSayParams(planWrittenReply(res, d))
	if p.Thread = res.Thread; p.Thread == "" {
		if err := p.toHomeThread(cmd.Context()); err != nil {
			return "", err
		}
	}
	out, err := conversationSay(cmd.Context(), bridgeURL, p, nil)
	if err != nil {
		return "", err
	}
	return out.ID, nil
}

// abandonReplacedPlan abandons the plan a building plan replaces, as the
// owner's change answer would have; a failure is reported, not returned, since
// the new plan is written.
func abandonReplacedPlan(ctx context.Context, old string, res *steward.WrittenPlan) {
	email, err := operatorEmailForViewer(firstNonEmpty(operatedBy, res.Owner))
	if err == nil {
		_, err = abandonPlan(ctx, planCmdWriter(), old, email)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "WARNING: plan %s replaces %s, but abandoning %s failed: %v\nRun gb plan abandon %s\n", res.Plan, old, old, err, old)
	}
}

// planWrittenReply is the steward's reply in the thread that asked. The
// plan's arms go before its title, as on every line that names it. A plan
// the owner asked for here is building already: the reply is the "change or
// drop" notice that stands in for the card.
func planWrittenReply(res *steward.WrittenPlan, d steward.PlanDraft) string {
	steps := "1 step"
	if n := len(res.Steps); n != 1 {
		steps = fmt.Sprintf("%d steps", n)
	}
	title := fmt.Sprintf("%q", d.Title)
	if res.Arms != "" {
		title = res.Arms + " " + title
	}
	if res.Building {
		return fmt.Sprintf("You asked for this here, so I started it: plan %s, %s, in %s, is building and its gardener takes the first step. To change it or drop it, say so in this thread.",
			res.Plan, title, steps)
	}
	return fmt.Sprintf("I wrote this up as plan %s, %s, in %s, and asked %s to answer the card to run it. Nothing runs until they do.",
		res.Plan, title, steps, strings.TrimPrefix(res.Owner, "oidc:"))
}

func runLandsPlanAnswer(cmd *cobra.Command, args []string) error {
	a, err := steward.AnswerPlan(cmd.Context(), daemon, args[0], args[1], actor)
	if err != nil {
		if steward.ServerApplies(cmd.Context(), daemon, args[1], time.Now()) {
			return fmt.Errorf("%w\nThe server applies this answer itself (the plan answer reactor, within a minute of the answer), so do not file an escalation for it: re-run this command in a minute to confirm it is applied", err)
		}
		return err
	}
	var started *homePlanStart
	if a.Home && a.Answer == steward.PlanApprove && a.State == "building" && a.Next != "" {
		if started, err = startHomePlan(cmd.Context(), a.Plan, a.Owner); err != nil {
			return fmt.Errorf("plan %s is building, but starting its gardener failed: %w; run gb plan gardener %s, then gb gardener fire-now %s", a.Plan, err, a.Plan, a.Plan)
		}
	}
	if jsonOutput {
		return json.NewEncoder(os.Stdout).Encode(struct {
			*steward.PlanAnswer
			Gardener *homePlanStart `json:"gardener,omitempty"`
		}{a, started})
	}
	fmt.Print(describePlanAnswer(a))
	if started != nil {
		fmt.Print(describeHomePlanStart(started))
	}
	return nil
}

// homePlanStart is what gb lands plan answer did to start an approved
// home-garden plan: its gardener, and the first cycle it fired.
type homePlanStart struct {
	ID string `json:"id"`
	// Bound is true for a gardener bound now, Enabled for a paused one
	// enabled now; neither means it was already running.
	Bound   bool `json:"bound"`
	Enabled bool `json:"enabled"`
	// Sealed is true when the server sealed the gardener to the owner's
	// approval (kbeads spawn_lineage_plan_approval.go): its triggers fire the
	// first cycle through the spawn gate, so no cycle is fired from this
	// agent's own token, which would only file an escalation.
	Sealed bool `json:"sealed,omitempty"`
	// Cycle is the agent bead of the cycle fired now, Escalation the approval
	// asked instead when the role is privileged, Live the cycle already
	// running; all empty when the cycle could not be fired (FireError).
	Cycle      string `json:"cycle,omitempty"`
	Escalation string `json:"escalation,omitempty"`
	Live       string `json:"live,omitempty"`
	FireError  string `json:"fire_error,omitempty"`
}

// startHomePlan starts a building home-garden plan (approved, or asked for by
// its owner in the thread) the way "Approve & build" does on the cockpit, then fires the gardener's first cycle so a crew is on
// the first step within the run rather than at the next cron tick. A failed
// fire is reported, not returned: the enabled gardener's triggers still fire.
// A gardener the server sealed to the owner's approval is left to its triggers
// (gd-YaoqmhIx8x): a fire from this agent's token would file an escalation.
func startHomePlan(ctx context.Context, planID, owner string) (*homePlanStart, error) {
	plan, err := daemon.GetBead(ctx, planID)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", planID, err)
	}
	res, enabled, err := plangardener.Start(ctx, daemon, plan, plangardener.BindSpec{
		Operator:  firstNonEmpty(owner, operatedBy),
		CreatedBy: actor,
	})
	if err != nil {
		return nil, err
	}
	out := &homePlanStart{ID: res.ID, Bound: !res.Adopted, Enabled: enabled}
	if live, holder := gardenerHasLiveCycle(ctx, daemon, res.ID); live {
		out.Live = holder
		return out, nil
	}
	sched, err := daemon.GetBead(ctx, res.ID)
	if err != nil {
		out.FireError = err.Error()
		return out, nil
	}
	if strings.TrimSpace(sched.Fields["spawn_lineage"]) != "" {
		out.Sealed = true
		return out, nil
	}
	fired, err := fireGardenerCycle(ctx, sched, "", false)
	switch {
	case err != nil:
		out.FireError = err.Error()
	case fired.EscalationID != "":
		out.Escalation = fired.EscalationID
	default:
		out.Cycle = fired.Spawn.BeadID
	}
	return out, nil
}

// describeHomePlanStart is the gardener half of the answer's printout.
func describeHomePlanStart(s *homePlanStart) string {
	var b strings.Builder
	switch {
	case s.Bound:
		fmt.Fprintf(&b, "gardener: %s bound and enabled\n", s.ID)
	case s.Enabled:
		fmt.Fprintf(&b, "gardener: %s was paused; enabled\n", s.ID)
	default:
		fmt.Fprintf(&b, "gardener: %s already enabled\n", s.ID)
	}
	switch {
	case s.Cycle != "":
		fmt.Fprintf(&b, "first cycle: %s, on its way to the first step\n", s.Cycle)
	case s.Escalation != "":
		fmt.Fprintf(&b, "first cycle: needs operator approval, escalation %s; the gardener's triggers fire it otherwise\n", s.Escalation)
	case s.Live != "":
		fmt.Fprintf(&b, "first cycle: %s is already running\n", s.Live)
	case s.Sealed:
		b.WriteString("first cycle: the gardener is sealed to the owner's approval; the trigger engine fires its first cycle once it loads the enabled gardener\n")
	default:
		fmt.Fprintf(&b, "first cycle: not fired (%s); the gardener's triggers fire it\n", s.FireError)
	}
	b.WriteString("next: say in the thread that the plan is building\n")
	return b.String()
}

// describePlanAnswer is the answer as gb lands plan answer prints it, ending
// with what the steward does next.
func describePlanAnswer(a *steward.PlanAnswer) string {
	var b strings.Builder
	fmt.Fprintf(&b, "plan %s: %s\n", a.Plan, a.Title)
	answer := a.Answer
	if answer == "" {
		answer = "written, no option chosen"
	}
	fmt.Fprintf(&b, "answer: %s", answer)
	if a.By != "" {
		fmt.Fprintf(&b, " (by %s)", a.By)
	}
	b.WriteString("\n")
	if a.Words != "" {
		fmt.Fprintf(&b, "their words: %s\n", a.Words)
	}
	state := a.State
	if !a.Applied {
		state += " (unchanged)"
	}
	fmt.Fprintf(&b, "plan_state: %s\n", state)
	if a.Thread != "" {
		fmt.Fprintf(&b, "asked in thread: %s\n", a.Thread)
	}
	switch {
	case a.Answer == steward.PlanApprove && a.Next == "":
		b.WriteString("next: every step is done\n")
	case a.Answer == steward.PlanApprove && a.NextStatus == "open" && a.Home:
		fmt.Fprintf(&b, "first step: %s %s (open: the plan's gardener spawns a crew on it)\n", a.Next, a.NextTitle)
	case a.Answer == steward.PlanApprove && a.NextStatus == "open":
		fmt.Fprintf(&b, "next: %s %s (open: spawn a gardener on it)\n", a.Next, a.NextTitle)
	case a.Answer == steward.PlanApprove:
		fmt.Fprintf(&b, "next: %s %s (%s)\n", a.Next, a.NextTitle, a.NextStatus)
	case a.Answer == steward.PlanChange:
		fmt.Fprintf(&b, "next: write the changed plan with gb lands plan write <land> --file plan.md --replaces %s\n", a.Plan)
	case a.Answer == steward.PlanDrop:
		b.WriteString("next: say in the thread that it is dropped\n")
	default:
		b.WriteString("next: read their words and answer or ask again\n")
	}
	return b.String()
}

func init() {
	landsPlanWriteCmd.Flags().String("file", "", "the plan as markdown (- for stdin)")
	landsPlanWriteCmd.Flags().String("thread", "", "the message the ask came in (its bead id)")
	landsPlanWriteCmd.Flags().String("for", "", "the owner to ask, oidc:<email> or an email (default: the land's recorded owner)")
	landsPlanWriteCmd.Flags().String("replaces", "", "the plan this one replaces after a change")
	landsPlanWriteCmd.Flags().Bool("propose", false, "the plan is your own idea, not the owner's ask: file the approve card even in the owner's thread")
	landsPlanCmd.AddCommand(landsPlanWriteCmd, landsPlanAnswerCmd)
	landsCmd.AddCommand(landsPlanCmd)
}
