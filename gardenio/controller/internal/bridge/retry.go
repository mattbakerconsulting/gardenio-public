package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// BackoffConfig configures RetryWithBackoff: a capped exponential backoff used
// by the startup catch-up load and by the self-heal sweeps' list queries
// (gd-niQPoeR7ja).
//
// The zero value is usable — every field falls back to a sane default in
// RetryWithBackoff — so callers only set what they care about.
type BackoffConfig struct {
	// Initial is the delay before the second attempt. Default 1s.
	Initial time.Duration
	// Max caps the delay. Default 30s. "Capped" is the whole point: a beads
	// outage must not push the retry interval out to hours, because the engine
	// is unavailable (not ready) for exactly as long as it takes to land.
	Max time.Duration
	// Factor multiplies the delay after each failed attempt. Default 2.
	Factor float64
	// MaxAttempts caps the total number of attempts. Zero means "retry until
	// ctx is done" — the right setting for a startup gate that must NEVER give
	// up and exit. Bounded callers (the per-cycle heal sweeps, which get a free
	// retry on the next poll anyway) set a small number.
	MaxAttempts int
}

// retry backoff defaults. Deliberately modest: the beads daemon is in-cluster,
// so the common failure is a seconds-long DNS/rollout blip, not a multi-hour
// outage — and a 30s ceiling still reconnects promptly once it clears.
const (
	defaultBackoffInitial = 1 * time.Second
	defaultBackoffMax     = 30 * time.Second
	defaultBackoffFactor  = 2.0
)

// RetryWithBackoff calls fn until it succeeds, its attempt budget is spent, or
// ctx is done. Each failure is logged at WARN with the attempt number and the
// delay before the next try, so a pod stuck retrying says so in its logs every
// interval rather than going quiet.
//
// It returns nil on success, the last error when MaxAttempts is exhausted, and
// a ctx error (wrapping the last failure for context) when the caller is shut
// down mid-retry. Callers distinguish the two with errors.Is(err,
// context.Canceled) — a canceled retry means "shutting down", NOT "give up and
// exit non-zero".
//
// No jitter: each binary runs one instance of each retry loop and there are a
// handful of daemon clients fleet-wide, so there is no herd to spread out, and
// deterministic delays keep the tests honest.
func RetryWithBackoff(ctx context.Context, logger *slog.Logger, op string, cfg BackoffConfig, fn func(context.Context) error) error {
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.Initial <= 0 {
		cfg.Initial = defaultBackoffInitial
	}
	if cfg.Max <= 0 {
		cfg.Max = defaultBackoffMax
	}
	if cfg.Factor < 1 {
		cfg.Factor = defaultBackoffFactor
	}
	if cfg.Initial > cfg.Max {
		cfg.Initial = cfg.Max
	}

	delay := cfg.Initial
	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		err := fn(ctx)
		if err == nil {
			if attempt > 1 {
				logger.Info("retry succeeded", "op", op, "attempt", attempt)
			}
			return nil
		}

		if cfg.MaxAttempts > 0 && attempt >= cfg.MaxAttempts {
			logger.Warn("retry budget exhausted", "op", op,
				"attempts", attempt, "error", err)
			return err
		}

		logger.Warn("retry scheduled after failure", "op", op,
			"attempt", attempt, "retry_in", delay, "error", err)

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			// Wrap so errors.Is(context.Canceled) still identifies the
			// shutdown while the log line keeps the reason it was retrying.
			return fmt.Errorf("%w (last error: %v)", ctx.Err(), err)
		case <-timer.C:
		}

		delay = time.Duration(float64(delay) * cfg.Factor)
		if delay > cfg.Max {
			delay = cfg.Max
		}
	}
}
