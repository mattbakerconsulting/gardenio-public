package thawepoch

import (
	"context"
	"log/slog"
	"time"
)

// Handler runs a consumer's rehydration sequence for a single thaw epoch. It is
// supplied by the downstream build beads (the gb supervisor wires its cred
// re-resolution + heartbeat; coop wires reconnect_all). The Handler MUST be
// idempotent: the watcher may invoke it more than once for the same logical
// thaw (an inotify+poll double-fire, or the drift backstop firing after the
// primary marker), and re-running must be a cheap no-op when nothing changed
// (rehydration-design §4 Stage 0).
//
// The returned Ack's Epoch and Consumer are overwritten by the watcher; the
// Handler only needs to populate OK, Error, and Stages. A returned error is
// treated as a fail-closed (OK=false) ack with the error string attached.
type Handler func(ctx context.Context, m Marker) (Ack, error)

// DriftDetector is the Stage-0 backstop (rehydration-design §3.1 option C). It
// reports the pod identity the process currently observes from outside the
// freeze line — e.g. /etc/hostname or the SA-token namespace. When this differs
// from the identity the watcher last rehydrated under AND no fresher marker has
// arrived, the watcher self-triggers so an un-marked thaw still eventually
// rehydrates rather than silently rotting. Returning an empty string means
// "unknown / do not self-trigger".
type DriftDetector func() string

// Watcher is the agent-side consumer of the thaw-epoch signal. One Watcher runs
// per consumer (gb, coop). It detects epoch increments via a poll loop (the
// safe floor on gVisor; an inotify front-end may nudge it via Notify), runs the
// Handler once per epoch, and writes the consumer's ack.
type Watcher struct {
	StateDir string
	Consumer string
	Handler  Handler

	// PollInterval defaults to DefaultPollInterval when zero.
	PollInterval time.Duration
	// Logger defaults to slog.Default() when nil.
	Logger *slog.Logger
	// Drift, when set, enables the backstop self-trigger (option C).
	Drift DriftDetector

	lastEpoch    int64  // highest epoch we have run the Handler for
	lastIdentity string // pod identity we last rehydrated under (for drift)
	notify       chan struct{}
}

func (w *Watcher) logger() *slog.Logger {
	if w.Logger != nil {
		return w.Logger
	}
	return slog.Default()
}

func (w *Watcher) interval() time.Duration {
	if w.PollInterval > 0 {
		return w.PollInterval
	}
	return DefaultPollInterval
}

// Run polls for epoch increments until ctx is cancelled. It is the long-lived
// agent-side loop. Run returns ctx.Err() on cancellation. Run does not return
// on Handler failure — a fail-closed ack is written and the loop continues so a
// subsequent (corrected) marker can still rehydrate.
func (w *Watcher) Run(ctx context.Context) error {
	if w.notify == nil {
		w.notify = make(chan struct{}, 1)
	}
	// Latch the current epoch as already-seen on startup *only* if our ack
	// for it already exists — otherwise a watcher that starts after the
	// marker was written (the normal thaw case) must still run the Handler.
	w.recoverLastEpoch()

	tick := time.NewTicker(w.interval())
	defer tick.Stop()

	// Immediate first check so we don't wait a full interval on a marker that
	// is already present at startup.
	w.checkOnce(ctx)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			w.checkOnce(ctx)
		case <-w.notify:
			w.checkOnce(ctx)
		}
	}
}

// recoverLastEpoch sets lastEpoch from an existing ack so a restarted watcher
// (not a thaw — e.g. the supervisor crashed and re-execed) does not re-run an
// already-acked epoch.
func (w *Watcher) recoverLastEpoch() {
	if ack, ok, err := ReadAck(w.StateDir, w.Consumer); err == nil && ok && ack.OK {
		w.lastEpoch = ack.Epoch
	}
}

// checkOnce reads the marker (and consults the drift backstop) and runs the
// Handler if a new epoch is present.
func (w *Watcher) checkOnce(ctx context.Context) {
	m, ok, err := Read(w.StateDir)
	if err != nil {
		w.logger().Warn("thawepoch: read marker failed", "consumer", w.Consumer, "err", err)
		return
	}

	// Drift backstop: if no marker (or a stale one) but the observed pod
	// identity changed since our last rehydration, synthesize a marker so the
	// agent still rehydrates. This guards an un-marked thaw (controller bug /
	// manual restore).
	if w.Drift != nil {
		if id := sanitize(w.Drift()); id != "" {
			markerIdentity := ""
			if ok {
				markerIdentity = m.PodUID
			}
			drifted := id != w.lastIdentity && w.lastIdentity != "" && id != markerIdentity
			if drifted {
				w.logger().Warn("thawepoch: identity drift detected with no fresh marker; self-triggering backstop",
					"consumer", w.Consumer, "observed", id, "last", w.lastIdentity)
				synthetic := Marker{Epoch: w.lastEpoch + 1, PodUID: id, TS: time.Now().UTC()}
				w.runHandler(ctx, synthetic, true)
				return
			}
			if w.lastIdentity == "" {
				w.lastIdentity = id // seed on first observation
			}
		}
	}

	if !ok || m.Epoch <= w.lastEpoch {
		return // no marker yet, or already handled this epoch
	}
	w.runHandler(ctx, m, false)
}

// runHandler invokes the Handler under the marker's deadline, writes the ack,
// and latches the epoch on success.
func (w *Watcher) runHandler(ctx context.Context, m Marker, synthetic bool) {
	log := w.logger().With("consumer", w.Consumer, "epoch", m.Epoch, "pod_uid", m.PodUID)
	log.Info("thawepoch: rehydration triggered", "synthetic", synthetic)

	hctx, cancel := context.WithTimeout(ctx, m.Deadline())
	defer cancel()

	var ack Ack
	if w.Handler != nil {
		a, err := w.Handler(hctx, m)
		ack = a
		if err != nil {
			ack.OK = false
			if ack.Error == "" {
				ack.Error = err.Error()
			}
		}
	} else {
		// No handler wired yet (Stage 0 signal-only): a present marker is
		// acked OK so the gate releases. Downstream beads replace this.
		ack.OK = true
	}
	ack.Epoch = m.Epoch
	ack.Consumer = w.Consumer

	if err := WriteAck(w.StateDir, ack); err != nil {
		log.Error("thawepoch: write ack failed", "err", err)
		return
	}

	if ack.OK {
		w.lastEpoch = m.Epoch
		w.lastIdentity = m.PodUID
		log.Info("thawepoch: rehydration acked OK")
	} else {
		// Fail-closed: do not latch the epoch, so a corrected marker (or a
		// retry by the controller) can re-trigger.
		log.Warn("thawepoch: rehydration fail-closed", "err", ack.Error)
	}
}
