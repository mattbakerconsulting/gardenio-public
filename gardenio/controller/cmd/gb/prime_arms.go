package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/plans"
)

// prime_arms.go — the arms duty in the preamble (badges-from-arms plan
// gd-pQZO9ie11D, step 3 gd-YvWv4mR4fu). Arms are chosen, not generated, so an
// epic only bears them when the agent that holds it picks them. Two holders get
// told so here:
//
//   - a plan-gardener whose plan epic, or any phase epic in its phase_order,
//     bears no arms gets the "## Arms" duty naming each one: choose, check,
//     write, and note the choice in the run comment;
//   - any other agent whose task is an armless type=epic gets the same duty as
//     one line.
//
// The playbook (internal/plangardener PlaybookPrompt) carries the duty too, but
// that prompt is frozen on the trigger at bind time, so only this live section
// reaches gardeners bound before the duty existed. It renders nothing when
// every epic in question already bears arms, so it costs an armed plan no bytes.

// armsDutyHowTo is the one sentence both forms share.
const armsDutyHowTo = "read it, choose three to five charges with `kd heraldry library --search <word>`, " +
	"check them with `kd heraldry check \"<run>\" --project <project>`, and write them with " +
	"`kd update <id> --heraldry \"<run>\"`"

// outputArmsDuty writes the arms duty for the epic the agent holds, if it (or,
// for a plan-gardener, any of its phases) bears no arms. Non-fatal: any lookup
// failure renders nothing.
func outputArmsDuty(w io.Writer, agentID, role string) {
	if agentID == "" {
		return
	}
	ctx := context.Background()
	agentBead, err := daemon.GetBead(ctx, agentID)
	if err != nil || agentBead == nil {
		return
	}
	taskID := strings.TrimSpace(agentBead.Fields["task_id"])
	if taskID == "" {
		return
	}
	held, err := daemon.GetBead(ctx, taskID)
	if err != nil || held == nil || held.Type != "epic" || held.Status == "closed" {
		return
	}
	if role == "" {
		role = agentBead.Fields["role"]
	}
	if role != planGardenerSpawnRole {
		if held.Heraldry == "" {
			fmt.Fprintf(w, "\nYour task %s is an epic with no arms yet: %s.\n", held.ID, armsDutyHowTo)
		}
		return
	}
	armless := armlessPlanEpics(ctx, held)
	if len(armless) == 0 {
		return
	}
	fmt.Fprintln(w, "\n## Arms")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "These epics of your plan bear no arms yet: %s.\n", strings.Join(armless, ", "))
	fmt.Fprintf(w, "At the start of this run, for each one, %s.\n", armsDutyHowTo)
	fmt.Fprintf(w, "Name the arms you chose for each in this run's comment on %s.\n", held.ID)
}

// armlessPlanEpics returns the plan epic and the open phase epics in its
// phase_order that bear no arms, labelled "plan <id>" / "phase <id>", plan
// first then phases in phase order. A phase that is not an epic (a poured
// molecule) cannot bear arms and is skipped.
func armlessPlanEpics(ctx context.Context, plan *beadsapi.BeadDetail) []string {
	var out []string
	if plan.Heraldry == "" {
		out = append(out, "plan "+plan.ID)
	}
	for _, id := range plans.PhaseOrder(plan) {
		phase, err := daemon.GetBead(ctx, id)
		if err != nil || phase == nil || phase.Type != "epic" || phase.Status == "closed" {
			continue
		}
		if phase.Heraldry == "" {
			out = append(out, "phase "+phase.ID)
		}
	}
	return out
}
