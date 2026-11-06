package main

import (
	"context"
	"log/slog"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/plangardener"
	"gardenio/controller/internal/steward"
)

// The plan answer reactor (gd-Dl88Htipuq, owner decision gd-fBSlZfEvmo item
// 2): it applies the owner's answer to a plan's approve / change / drop card
// server-side, so the owner clicks once. Before it, the agent that asked had
// to run gb lands plan answer, and an agent that could not write the plan (a
// plan another agent filed, a step in a project its token cannot read) filed
// a beads_admin_write escalation the owner then approved: a second click per
// plan ruling.
//
// It sweeps, like the Lands apply reactor: every planAnswerReactorEvery it
// lists the plans still ready (waiting on their owner) and, for each,
// steward.FindRuling picks the answer it may apply: one the daemon stamped as
// an operator-lane human's (resolver_admin), on a decision of the plan's land
// that asked about this plan, answered within steward.RulingWindow. A plan
// leaves the set the moment its answer is applied, so a sweep reads little
// and needs no backfill after a restart.
//
// What it does is what gb lands plan answer does: steward.AnswerPlan, then on
// a home-garden approve plangardener.Start, which binds or enables the plan's
// gardener. The enable is sealed server-side from the owner's approval
// (kbeads spawn_lineage_plan_approval.go), so the gardener's triggers spawn the
// first cycle with no escalation. A dismissed card or words with no option
// apply nothing. A re-run, or the asking agent's own gb lands plan answer
// afterwards, changes nothing.
//
// It writes as the viewer's machine principal (gardenio-machine-plan-reactor,
// whose plan_scope capability reaches the plan's own beads and its gardener
// only), with the owner who answered as the operator.

// planAnswerReactorEvery is the sweep period. The asking agent is respawned on
// the answer and takes longer than this to boot, so the server usually applies
// the answer before the agent tries to.
const planAnswerReactorEvery = 30 * time.Second

// planAnswerReactorActor names the reactor in the comments and closes it writes.
const planAnswerReactorActor = "plan-answer-reactor"

// planAnswerStore is the plan store the reactor writes through, plus the read
// of a plan's comments that keeps each of its notes to one per plan.
type planAnswerStore interface {
	steward.PlanStore
	GetComments(ctx context.Context, beadID string) ([]beadsapi.Comment, error)
}

// PlanAnswerReactor runs the sweep. A nil *PlanAnswerReactor is safe.
type PlanAnswerReactor struct {
	st      planAnswerStore
	ctxFor  func(context.Context) context.Context
	every   time.Duration
	now     func() time.Time
	logger  *slog.Logger
	cancel  context.CancelFunc
	done    chan struct{}
	noticed map[string]bool // the refusals and failures already logged
}

// NewPlanAnswerReactor returns nil without a daemon. srv supplies the machine
// principal; a nil srv leaves the client's default credential.
func NewPlanAnswerReactor(st planAnswerStore, srv *Server, logger *slog.Logger) *PlanAnswerReactor {
	if st == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	r := &PlanAnswerReactor{st: st, every: planAnswerReactorEvery, now: time.Now, logger: logger, noticed: map[string]bool{}}
	r.ctxFor = func(ctx context.Context) context.Context { return ctx }
	if srv != nil {
		r.ctxFor = srv.machineCtx
	}
	return r
}

// Start begins sweeping: once now, then every period.
func (r *PlanAnswerReactor) Start() {
	if r == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel, r.done = cancel, make(chan struct{})
	go func() {
		defer close(r.done)
		t := time.NewTicker(r.every)
		defer t.Stop()
		for {
			r.sweep(ctx)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	r.logger.Info("plan answer reactor started", "every", r.every)
}

// Close stops the sweep and waits for the one in flight.
func (r *PlanAnswerReactor) Close() {
	if r == nil || r.cancel == nil {
		return
	}
	r.cancel()
	<-r.done
}

// sweep applies every ready plan's ruling once. It returns how many answers it
// applied, for tests.
func (r *PlanAnswerReactor) sweep(parent context.Context) int {
	ctx, cancel := context.WithTimeout(r.ctxFor(parent), r.every)
	defer cancel()
	res, err := r.st.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"epic"},
		Labels:   []string{plangardener.PlanLabel},
		Statuses: []string{"open", "in_progress"},
		Fields:   map[string]string{"plan_state": "ready"},
		Limit:    200,
	})
	if err != nil {
		r.logger.Warn("plan answer reactor: listing ready plans failed", "error", err)
		return 0
	}
	applied := 0
	for _, plan := range res.Beads {
		if parent.Err() != nil {
			break
		}
		if plan.Fields["plan_state"] != "ready" {
			continue
		}
		if r.react(ctx, plan) {
			applied++
		}
	}
	return applied
}

// react applies plan's ruling, if it has one. It reports whether an answer
// was applied.
func (r *PlanAnswerReactor) react(ctx context.Context, plan *beadsapi.BeadDetail) bool {
	ruling, refused, err := steward.FindRuling(ctx, r.st, plan, r.now())
	if err != nil {
		r.logger.Warn("plan answer reactor: reading the plan's decisions failed", "plan", plan.ID, "error", err)
		return false
	}
	for _, why := range refused {
		r.notice(why, "plan answer reactor: not applying an answer", "plan", plan.ID, "why", why)
	}
	if ruling == nil {
		return false
	}
	opCtx := beadsapi.WithOperator(ctx, ruling.By)
	a, err := steward.AnswerPlan(opCtx, r.st, ruling.Land, ruling.Decision, planAnswerReactorActor)
	if err != nil {
		// Most often a step with no body yet: the plan stays ready and the
		// next sweep applies the answer once the step is filled in. Say so on
		// the plan once, so whoever fills it knows nothing else is needed.
		key := ruling.Decision + ": " + err.Error()
		if r.notice(key, "plan answer reactor: applying the answer failed",
			"plan", plan.ID, "decision", ruling.Decision, "answer", ruling.Answer, "error", err) {
			r.commentOnce(opCtx, plan.ID, key,
				"The owner's "+ruling.Answer+" on "+ruling.Decision+" is not applied yet: "+err.Error()+
					". The server applies it on its own once that is fixed (within a day of the answer); nobody needs to run gb lands plan answer.")
		}
		return false
	}
	r.logger.Info("plan answer reactor: applied the owner's answer",
		"plan", plan.ID, "decision", ruling.Decision, "answer", a.Answer, "state", a.State, "applied", a.Applied, "by", ruling.By)
	if a.Home && a.Answer == steward.PlanApprove && a.State == "building" && a.Next != "" {
		r.startGardener(opCtx, a, ruling)
	}
	return a.Applied
}

// startGardener binds or enables an approved home-garden plan's gardener, as
// gb lands plan answer does. It fires no cycle: the server seals the gardener
// to the owner's approval, and the trigger engine fires its first cycle once it
// loads the enabled gardener (gd-Vk2ADepJZ2): only the engine can spend a seal.
func (r *PlanAnswerReactor) startGardener(ctx context.Context, a *steward.PlanAnswer, ruling *steward.Ruling) {
	plan, err := r.st.GetBead(ctx, a.Plan)
	if err == nil {
		var res *plangardener.BindResult
		var enabled bool
		res, enabled, err = plangardener.Start(ctx, r.st, plan, plangardener.BindSpec{
			Operator:  firstNonEmptyStr(a.Owner, ruling.By),
			CreatedBy: planAnswerReactorActor,
		})
		if err == nil {
			r.logger.Info("plan answer reactor: plan gardener started",
				"plan", a.Plan, "gardener", res.ID, "bound", !res.Adopted, "enabled", enabled)
			return
		}
	}
	r.logger.Warn("plan answer reactor: starting the plan's gardener failed", "plan", a.Plan, "error", err)
	_ = r.st.AddComment(ctx, a.Plan, planAnswerReactorActor,
		"plan_state is building (approved on "+a.Decision+"), but starting its gardener failed: "+err.Error()+
			". Run gb plan gardener "+a.Plan+", then gb gardener fire-now on it.")
}

// commentOnce posts text on the plan unless the plan already carries it.
// noticed alone is not enough: it lives in this process, so every restart of
// the viewer would post the same note again (gd-OxEUOYP5TU: three copies on
// one plan in 25 minutes). The plan's own comments are the record that
// survives. If they cannot be read it posts nothing and forgets key, so the
// next sweep tries again.
func (r *PlanAnswerReactor) commentOnce(ctx context.Context, planID, key, text string) {
	comments, err := r.st.GetComments(ctx, planID)
	if err != nil {
		r.logger.Warn("plan answer reactor: reading the plan's comments failed", "plan", planID, "error", err)
		delete(r.noticed, key)
		return
	}
	for _, c := range comments {
		if c.Text == text {
			return
		}
	}
	_ = r.st.AddComment(ctx, planID, planAnswerReactorActor, text)
}

// notice logs msg once per key; it reports whether it logged.
func (r *PlanAnswerReactor) notice(key, msg string, args ...any) bool {
	if r.noticed[key] {
		return false
	}
	r.noticed[key] = true
	r.logger.Warn(msg, args...)
	return true
}

// Compile-time check: the daemon client main.go hands the reactor is a plan
// store.
var _ planAnswerStore = (*beadsapi.Client)(nil)
