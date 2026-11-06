package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"gardenio/controller/internal/natsauth"

	"gardenio/controller/internal/beadsapi"
)

// probeEvent is the NATS payload published by rotationNotifier on probe transitions.
// It is a subset of rotationEvent — only the fields relevant to probe-driven failover.
type probeEvent struct {
	Event       string `json:"event"`        // account_throttled | account_recovered
	FromAccount string `json:"from_account"` // the account that changed state
	ResetAt     string `json:"reset_at,omitempty"`
	Timestamp   string `json:"timestamp"`
}

// probeEventListener subscribes to probe-sourced NATS events
// (gardenio.rotation.account_throttled) and drives auth failover for every
// project that is currently using the throttled account.
//
// Project → account mapping is resolved via the controller's in-memory
// failoverRuntime (for projects in failover) combined with the bead-labelled
// primary pool (for projects not yet in failover).
type probeEventListener struct {
	daemon   *beadsapi.Client
	failover *authFailover
	logger   *slog.Logger

	debounce time.Duration
	mu       sync.Mutex
	lastSeen map[string]time.Time // account → last event time
}

func newProbeEventListener(daemon *beadsapi.Client, failover *authFailover, logger *slog.Logger) *probeEventListener {
	return &probeEventListener{
		daemon:   daemon,
		failover: failover,
		logger:   logger.With("component", "probe-event-listener"),
		debounce: 2 * time.Minute,
		lastSeen: make(map[string]time.Time),
	}
}

// Start subscribes to probe events and blocks until ctx is cancelled.
func (l *probeEventListener) Start(ctx context.Context, natsURL, natsToken, natsUser string) error {
	if natsURL == "" {
		l.logger.Info("probe event listener disabled (no NATS URL)")
		return nil
	}

	opts := []nats.Option{
		nats.Name("gardenio-controller-probe-events"),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(5 * time.Second),
	}
	opts = append(opts, natsauth.Options(natsUser, natsToken)...)
	opts = append(opts, natsauth.StartupRetry()...)

	conn, err := nats.Connect(natsURL, opts...)
	if err != nil {
		return err
	}
	defer conn.Close()

	sub, err := conn.Subscribe("gardenio.rotation.account_throttled", func(msg *nats.Msg) {
		l.handleThrottled(ctx, msg)
	})
	if err != nil {
		return err
	}
	defer sub.Unsubscribe() //nolint:errcheck // best-effort cleanup on shutdown

	subRecov, err := conn.Subscribe("gardenio.rotation.account_recovered", func(msg *nats.Msg) {
		l.handleRecovered(ctx, msg)
	})
	if err != nil {
		return err
	}
	defer subRecov.Unsubscribe() //nolint:errcheck // best-effort cleanup on shutdown

	l.logger.Info("probe event listener subscribed",
		"subjects", "gardenio.rotation.account_throttled,gardenio.rotation.account_recovered")
	<-ctx.Done()
	return nil
}

// handleThrottled processes an account_throttled event from the probe.
// It finds all projects currently using the throttled account and triggers
// auth failover for each.
func (l *probeEventListener) handleThrottled(ctx context.Context, msg *nats.Msg) {
	var evt probeEvent
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		l.logger.Debug("probe event: malformed payload", "error", err)
		return
	}
	if evt.FromAccount == "" {
		return
	}

	if !l.tryDebounce(evt.FromAccount) {
		l.logger.Debug("probe event: debounced", "account", evt.FromAccount)
		return
	}

	l.logger.Info("probe event: account throttled, finding affected projects",
		"account", evt.FromAccount, "reset_at", evt.ResetAt)

	projects, err := l.findProjectsForAccount(ctx, evt.FromAccount)
	if err != nil {
		l.logger.Warn("probe event: failed to find projects for account",
			"account", evt.FromAccount, "error", err)
		return
	}

	if len(projects) == 0 {
		l.logger.Info("probe event: no active projects on throttled account",
			"account", evt.FromAccount)
		return
	}

	for _, project := range projects {
		l.logger.Info("probe event: triggering failover",
			"account", evt.FromAccount, "project", project)
		if err := l.failover.Handle(ctx, project, ""); err != nil {
			l.logger.Error("probe event: failover failed",
				"account", evt.FromAccount, "project", project, "error", err)
		}
	}
}

// findProjectsForAccount returns all project names whose currently-serving
// account is the given one. Projects in failover come from the runtime map;
// projects not in failover are resolved via their label-scoped account pool
// (the primary account serves requests when no failover is active) with a
// legacy fallback to config:auth beads for deployments that label projects
// on the config bead rather than on account beads.
func (l *probeEventListener) findProjectsForAccount(ctx context.Context, account string) ([]string, error) {
	seen := map[string]bool{}

	// Projects in active failover — runtime is authoritative.
	for _, p := range l.failover.runtime.ProjectsByCurrent(account) {
		seen[p] = true
	}

	// Projects NOT in failover: the primary of each project's labeled pool
	// is the active account. Iterate account beads grouped by project to
	// find those whose primary matches.
	result, err := l.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"account"},
		Statuses: []string{"open"},
		Limit:    500,
	})
	if err != nil {
		return nil, err
	}
	byProject := map[string][]*beadsapi.BeadDetail{}
	for _, b := range result.Beads {
		for _, lbl := range b.Labels {
			if p, ok := strings.CutPrefix(lbl, "project:"); ok {
				byProject[p] = append(byProject[p], b)
			}
		}
	}
	for project, accts := range byProject {
		if seen[project] {
			continue
		}
		if st, ok := l.failover.runtime.Get(project); ok && st.inFailover() {
			continue // handled by ProjectsByCurrent above
		}
		if projectPrimary(accts) == account {
			seen[project] = true
		}
	}

	// Legacy fallback: resolve projects via config:auth beads whose
	// description.account names the throttled account. Covers fixtures and
	// deployments that label projects on the config bead rather than on
	// account beads.
	legacy, err := listLegacyAuthConfigs(ctx, l.daemon)
	if err != nil {
		l.logger.Warn("probe event: failed to list legacy config:auth beads", "error", err)
	} else {
		for project, cfg := range legacy {
			if seen[project] {
				continue
			}
			if st, ok := l.failover.runtime.Get(project); ok && st.inFailover() {
				continue
			}
			if parseAuthConfigAccount(cfg.Description) == account {
				seen[project] = true
			}
		}
	}

	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

// handleRecovered processes an account_recovered event from the probe. It
// retries any projects stuck without a healthy fallback now that the account
// is back online.
//
// Switchback is abolished (ka-0000008240), so this no longer steers projects
// back onto a recovered "primary". Recovery is (a) SweepStaleExhausted clearing
// the account's exhausted status so it rejoins the healthy spread pool, and
// (b) retryStuckFailovers below kicking the forward failover path for any
// project left with no serving account.
func (l *probeEventListener) handleRecovered(ctx context.Context, msg *nats.Msg) {
	var evt probeEvent
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		l.logger.Debug("probe event: malformed payload", "error", err)
		return
	}
	if evt.FromAccount == "" {
		return
	}

	l.logger.Info("probe event: account recovered, retrying stuck failovers",
		"account", evt.FromAccount)

	l.retryStuckFailovers(ctx, evt.FromAccount)
}

// retryStuckFailovers finds projects whose previous failover attempt yielded
// no healthy fallback (so no runtime entry was recorded) and retries them
// now that `recoveredAccount` is back online. A project counts as stuck when
// its primary is either (a) in active cooldown (status=exhausted with future
// cooldown_until — set when failover succeeded but the chain later broke) or
// (b) currently throttle_status=rejected (the failover attempt found no
// fallback and bailed before marking cooldown, leaving the primary active in
// the bead but unable to serve traffic).
func (l *probeEventListener) retryStuckFailovers(ctx context.Context, recoveredAccount string) {
	result, err := l.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"account"},
		Statuses: []string{"open"},
		Limit:    500,
	})
	if err != nil {
		l.logger.Warn("retry stuck: failed to list account beads", "error", err)
		return
	}

	byProject := map[string][]*beadsapi.BeadDetail{}
	for _, b := range result.Beads {
		for _, lbl := range b.Labels {
			if p, ok := strings.CutPrefix(lbl, "project:"); ok {
				byProject[p] = append(byProject[p], b)
			}
		}
	}

	// Legacy pattern: projects with account beads that lack project: labels
	// and are instead grouped via a config:auth bead. Seed byProject entries
	// from the config:auth.account chain so the stuck-retry logic sees them.
	legacy, err := listLegacyAuthConfigs(ctx, l.daemon)
	if err != nil {
		l.logger.Warn("retry stuck: failed to list config:auth beads", "error", err)
	} else {
		for project, cfg := range legacy {
			if _, ok := byProject[project]; ok {
				continue
			}
			primary := parseAuthConfigAccount(cfg.Description)
			if primary == "" {
				continue
			}
			chain := walkChainFromPrimary(result.Beads, primary)
			if len(chain) > 0 {
				byProject[project] = chain
			}
		}
	}

	now := time.Now()
	for project, accts := range byProject {
		if st, ok := l.failover.runtime.Get(project); ok && st.inFailover() {
			continue
		}
		primaryName := projectPrimary(accts)
		primary := findByTitle(accts, primaryName)
		if primary == nil || !primaryStuck(primary, now) {
			continue
		}
		// Only retry if the recovered account is actually in this project's
		// fallback chain — otherwise the retry is guaranteed to fail again.
		if !chainContains(accts, primary, recoveredAccount) {
			continue
		}

		l.logger.Info("retry stuck: attempting failover for stuck project",
			"project", project, "primary", primaryName,
			"recovered_account", recoveredAccount)
		if err := l.failover.Handle(ctx, project, ""); err != nil {
			l.logger.Error("retry stuck: failover failed",
				"project", project, "error", err)
		}
	}
}

// chainContains returns true when `target` is reachable via the fallback
// chain starting at `start`.
func chainContains(accounts []*beadsapi.BeadDetail, start *beadsapi.BeadDetail, target string) bool {
	if start == nil {
		return false
	}
	visited := map[string]bool{start.Title: true}
	candidate := start.Fields["fallback"]
	var names []string
	for candidate != "" {
		if visited[candidate] {
			break
		}
		visited[candidate] = true
		names = append(names, candidate)
		bead := findByTitle(accounts, candidate)
		if bead == nil {
			break
		}
		candidate = bead.Fields["fallback"]
	}
	return slices.Contains(names, target)
}

func (l *probeEventListener) tryDebounce(account string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if last, ok := l.lastSeen[account]; ok && now.Sub(last) < l.debounce {
		return false
	}
	l.lastSeen[account] = now
	return true
}
