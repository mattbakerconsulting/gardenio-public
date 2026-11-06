package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/ciwait"
)

// The CI-wait resolver (plan gardenio/plans/wake-on-ci-completion.md §2.2-2.4,
// task gd-WKrO492yYP). An agent runs `gb wait ci --repo R --sha S`, which
// stamps ci_wait_state=pending and ci_wait_key=R@S on its own bead, then
// `gb stop`s. This handler records every workflow_run delivery into a per-sha
// run ledger and, on a sweep, resolves each pending wait whose ledger shows
// every awaited run completed and quiet for the quiesce window: ONE PATCH sets
// ci_wait_state=resolved (or timed_out) and ci_wait_result, then the agent is
// woken (ciwake.go, P3) and reads what this wrote with `gb wait ci --result`.
//
// The ledger is kept per sha, not per waiter, because the agent pushes BEFORE
// it calls `gb wait ci`: the requested/in_progress deliveries for most runs
// arrive before any waiter exists, and a ledger that started at the wait would
// resolve as soon as the first short workflow finished. Deliveries only touch
// memory; the sweep does all bead I/O, mirrors the ledger onto each waiter's
// ci_wait_ledger (so a restart keeps what was seen) and merges it back.
//
// A delivery GitHub never sent (or sent while the bridge was down, for a sha
// with no waiter yet) is in no ledger: the poll fallback (ciwait_poll.go)
// feeds an Actions API listing into the same ledger and the same Evaluate. The
// resolver never takes an agent name from a payload (§2.6): it
// only writes beads that already carry the matching ci_wait_key.

const (
	defaultCIWaitSweep = 15 * time.Second
	// ciWaitLedgerTTL is how long a sha's ledger is kept after its last
	// delivery: past `gb wait ci`'s 2h default timeout with room to spare.
	ciWaitLedgerTTL = 6 * time.Hour
	// ciWaitMaxKeys bounds memory; past it the stalest sha is dropped.
	ciWaitMaxKeys = 5000
	// ciWaitMaxWaiters is the page one sweep reads. More pending waits than
	// this means something upstream is wrong; the rest wait for a later tick.
	ciWaitMaxWaiters = 200
)

// ciWaitStatuses is every status an agent bead can have: the waiting agent is
// usually CLOSED (it stopped), so the lookup must not default to open beads.
var ciWaitStatuses = []string{"open", "in_progress", "blocked", "deferred", "closed"}

// ciWaitBeads is the slice of the daemon client the resolver uses.
type ciWaitBeads interface {
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
	UpdateBeadFields(ctx context.Context, beadID string, fields map[string]string) error
	ciWakeBeads
}

type ciWaitEntry struct {
	ledger ciwait.Ledger
	seen   time.Time // last delivery, merge or sweep, for TTL eviction
	waited time.Time // first sweep that found a waiter on the sha
	polled time.Time // last Actions poll (ciwait_poll.go)
}

type ciWaitHandler struct {
	beads   ciWaitBeads
	logger  *slog.Logger
	quiesce time.Duration
	sweep   time.Duration
	now     func() time.Time

	// runs lists a sha's workflow runs for the poll fallback; nil (no GitHub
	// App configured) leaves the resolver webhook-only.
	runs         ciRunLister
	pollInterval time.Duration
	pollGrace    time.Duration

	mu      sync.Mutex
	ledgers map[string]*ciWaitEntry // lower-cased ci_wait_key -> ledger
}

func newCIWaitHandler(beads ciWaitBeads, quiesce, sweep time.Duration, logger *slog.Logger) *ciWaitHandler {
	if quiesce <= 0 {
		quiesce = ciwait.DefaultQuiesce
	}
	if sweep <= 0 {
		sweep = defaultCIWaitSweep
	}
	return &ciWaitHandler{
		beads:        beads,
		logger:       logger,
		quiesce:      quiesce,
		sweep:        sweep,
		now:          time.Now,
		pollInterval: defaultCIWaitPollInterval,
		pollGrace:    defaultCIWaitPollGrace,
		ledgers:      map[string]*ciWaitEntry{},
	}
}

// ciWaitLedgerKey folds case: GitHub reports repository.full_name in its
// canonical case, while ci_wait_key keeps whatever the agent typed.
func ciWaitLedgerKey(key string) string { return strings.ToLower(key) }

type ciWorkflowRun struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	HeadSHA    string `json:"head_sha"`
	RunAttempt int    `json:"run_attempt"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

// Handle records a workflow_run delivery (requested, in_progress or completed)
// into its sha's ledger. Memory only, so it never fails a delivery.
func (h *ciWaitHandler) Handle(_ context.Context, ev Event) error {
	if ev.Type != "workflow_run" {
		return nil
	}
	var p struct {
		WorkflowRun ciWorkflowRun `json:"workflow_run"`
	}
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		h.logger.Warn("ci wait: undecodable workflow_run payload", "delivery", ev.DeliveryID, "error", err)
		return nil
	}
	wr := p.WorkflowRun
	key, err := beadsapi.CIWaitKey(ev.Repo, wr.HeadSHA)
	if err != nil {
		h.logger.Debug("ci wait: workflow_run without a usable repo/sha", "delivery", ev.DeliveryID, "error", err)
		return nil
	}
	status := wr.Status
	if status == "" {
		status = ev.Action // the action names the status it reports
	}
	run := ciwait.Run{ID: wr.ID, Name: wr.Name, Attempt: wr.RunAttempt, Status: status, Conclusion: wr.Conclusion}

	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	e := h.entryLocked(ciWaitLedgerKey(key), now)
	if e.ledger.Record(run, now) {
		h.logger.Debug("ci wait: run recorded", "key", key, "workflow", wr.Name,
			"run_id", wr.ID, "attempt", run.Attempt, "status", status, "conclusion", wr.Conclusion)
	}
	return nil
}

// entryLocked returns the ledger for key, creating it (and evicting the
// stalest one past ciWaitMaxKeys). h.mu must be held.
func (h *ciWaitHandler) entryLocked(key string, now time.Time) *ciWaitEntry {
	e, ok := h.ledgers[key]
	if !ok {
		if len(h.ledgers) >= ciWaitMaxKeys {
			var oldest string
			for k, v := range h.ledgers {
				if oldest == "" || v.seen.Before(h.ledgers[oldest].seen) {
					oldest = k
				}
			}
			delete(h.ledgers, oldest)
		}
		e = &ciWaitEntry{ledger: ciwait.Ledger{Key: key}}
		h.ledgers[key] = e
	}
	e.seen = now
	return e
}

// Run sweeps until ctx is done.
func (h *ciWaitHandler) Run(ctx context.Context) {
	t := time.NewTicker(h.sweep)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.sweepOnce(ctx)
		}
	}
}

// sweepOnce evaluates every pending wait against its sha's ledger, writes what
// changed, and expires stale ledgers. One list call per tick; a failed write
// is simply retried on the next tick (the wait is still pending).
func (h *ciWaitHandler) sweepOnce(ctx context.Context) {
	res, err := h.beads.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"agent"},
		Statuses: ciWaitStatuses,
		Fields:   map[string]string{beadsapi.FieldCIWaitState: beadsapi.CIWaitStatePending},
		Limit:    ciWaitMaxWaiters,
	})
	if err != nil {
		h.logger.Warn("ci wait: listing pending waits failed", "error", err)
		return
	}
	if res != nil {
		if res.Total > len(res.Beads) {
			h.logger.Warn("ci wait: more pending waits than one sweep reads", "total", res.Total, "read", len(res.Beads))
		}
		for _, b := range res.Beads {
			h.apply(ctx, b)
		}
	}
	h.expire()
}

// apply settles one waiter. It reports the fields it wrote (nil when nothing
// changed), for tests.
func (h *ciWaitHandler) apply(ctx context.Context, b *beadsapi.BeadDetail) map[string]string {
	w := beadsapi.CIWaitFromFields(b.Fields)
	if !w.Pending() || w.Key == "" {
		return nil
	}
	key := ciWaitLedgerKey(w.Key)
	persistedRaw := b.Fields[beadsapi.FieldCIWaitLedger]

	h.mu.Lock()
	now := h.now()
	e := h.entryLocked(key, now)
	if persisted, err := ciwait.Decode(persistedRaw); err != nil {
		h.logger.Warn("ci wait: ignoring an unreadable persisted ledger", "agent_bead", b.ID, "error", err)
	} else if persisted.Key == key && len(persisted.Runs) > 0 {
		e.ledger.Merge(persisted)
	}
	if e.waited.IsZero() {
		e.waited = now
	}
	pollDue := h.pollDueLocked(e, now)
	h.mu.Unlock()

	if pollDue {
		_ = h.poll(ctx, w.Key, key)
	}
	out, enc, unconfirmed := h.evaluate(key, w)
	if out.State == beadsapi.CIWaitStateResolved && unconfirmed {
		if h.poll(ctx, w.Key, key) == nil {
			out, enc, _ = h.evaluate(key, w)
		}
	}

	fields := map[string]string{}
	if enc != "" && enc != persistedRaw {
		fields[beadsapi.FieldCIWaitLedger] = enc
	}
	if out.State != beadsapi.CIWaitStatePending {
		// One PATCH: state and result land together, so a reader never sees
		// resolved without its result (plan §2.4).
		fields[beadsapi.FieldCIWaitState] = out.State
		fields[beadsapi.FieldCIWaitResult] = out.Result
	}
	if len(fields) == 0 {
		return nil
	}
	if err := h.beads.UpdateBeadFields(ctx, b.ID, fields); err != nil {
		h.logger.Warn("ci wait: agent bead update failed; retried next sweep",
			"agent_bead", b.ID, "key", w.Key, "state", out.State, "error", err)
		return nil
	}
	if out.State != beadsapi.CIWaitStatePending {
		h.logger.Info("ci wait concluded", "agent_bead", b.ID, "agent", b.Fields["agent"],
			"key", w.Key, "state", out.State, "summary", out.Summary)
		h.wakeCIWaiter(ctx, b, w, out)
	}
	return fields
}

// evaluate decides w from the ledger at key. enc is the ledger to mirror onto
// the bead ("" while it has no runs: an empty ledger is not worth a write).
// With no runs seen at all the wait stays pending until its deadline, then
// times out like any other (a lost wait never strands its task).
func (h *ciWaitHandler) evaluate(key string, w beadsapi.CIWait) (out ciwait.Outcome, enc string, unconfirmed bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	l := ciwait.Ledger{Key: key}
	if e := h.ledgers[key]; e != nil {
		l = e.ledger
	}
	out = ciwait.Evaluate(l, w.Workflows, w.Deadline, h.now(), h.quiesce)
	if len(l.Runs) > 0 {
		enc = l.Encode()
	}
	return out, enc, h.unconfirmedLocked(key)
}

// expire drops ledgers with no delivery or merge for ciWaitLedgerTTL.
func (h *ciWaitHandler) expire() {
	h.mu.Lock()
	defer h.mu.Unlock()
	cutoff := h.now().Add(-ciWaitLedgerTTL)
	for k, e := range h.ledgers {
		if e.seen.Before(cutoff) {
			delete(h.ledgers, k)
		}
	}
}
