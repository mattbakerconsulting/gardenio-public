package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// The resolver mirrors gitlab-bridge's agent resolver (internal/bridge/
// gitlab_agent_resolver.go): when review feedback is waiting on a PR and the
// agent that opened it is gone, spawn a worker on the same task bead to address
// it. GitLab's version is driven by the comment event; GitHub's is a sweep, so
// it also catches threads whose webhook arrived while the bridge was down, and
// threads a reviewer left open after the first round was "addressed".
//
// The guards, cheapest first, are the point: a resolver that respawns on every
// tick is a respawn storm.
//
//	task finished or PR not ours → live agent on the task or PR → persisted
//	cooldown → GraphQL (PR open? unresolved threads?) → attempt cap per thread
//	set → per-sweep spawn cap → spawn.

const (
	// resolverSpawnSource tags the agent bead so it is traceable and so the
	// prompt, viewer and metrics can tell a resolver respawn from a fresh task.
	resolverSpawnSource = "github-pr-review"

	// Task-bead fields the resolver keeps its memory in. They live on the bead,
	// not in the process, so a bridge restart or a second replica cannot
	// forget a spawn and do it again.
	fieldSpawnedAt   = "resolver_spawned_at"   // RFC3339 time of the last respawn
	fieldSpawnCount  = "resolver_spawn_count"  // respawns for the current thread set
	fieldThreadsHash = "resolver_threads_hash" // fingerprint of that thread set
	fieldGaveUp      = "resolver_gave_up_hash" // thread set the cap was announced for
	resolverRole     = "crew"
	resolverMaxShown = 3 // threads quoted into the Slack line and the mail
)

// resolverDefaults are the knobs main wires from the environment.
const (
	defaultResolverInterval    = 5 * time.Minute
	defaultResolverCooldown    = 30 * time.Minute
	defaultResolverMaxRespawns = 3
	// maxCheckedPerSweep bounds GraphQL calls (and so rate-limit spend) per
	// tick; the rest wait for the next one.
	maxCheckedPerSweep = 50
	// maxSpawnsPerSweep is the storm guard of last resort: even if every other
	// guard misjudged, one tick starts at most this many agents.
	maxSpawnsPerSweep = 3
)

// reviewThread is one unresolved review thread, reduced to what an agent and a
// Slack line need.
type reviewThread struct {
	ID       string
	Path     string
	Line     int
	Outdated bool
	Author   string
	Body     string // first comment
	URL      string
}

// prThreads is the live state of one PR as the GraphQL query sees it.
type prThreads struct {
	State      string // OPEN | CLOSED | MERGED
	Title      string
	Branch     string
	Unresolved []reviewThread
}

// threadSource reads a PR's review threads (a seam for tests).
type threadSource interface {
	Threads(ctx context.Context, pr prRef) (*prThreads, error)
}

// resolverDaemon is the daemon surface the resolver uses: the shared bead
// client plus agent spawning.
type resolverDaemon interface {
	beadClient
	SpawnAgent(ctx context.Context, agentName, project, taskID, role, customPrompt string, extraFields ...map[string]string) (string, error)
}

var _ resolverDaemon = (*beadsapi.Client)(nil)

type resolverConfig struct {
	Interval    time.Duration
	Cooldown    time.Duration
	MaxRespawns int
	DryRun      bool
}

func (c resolverConfig) withDefaults() resolverConfig {
	if c.Interval <= 0 {
		c.Interval = defaultResolverInterval
	}
	if c.Cooldown <= 0 {
		c.Cooldown = defaultResolverCooldown
	}
	if c.MaxRespawns <= 0 {
		c.MaxRespawns = defaultResolverMaxRespawns
	}
	return c
}

// resolver is the sweep loop.
type resolver struct {
	daemon   resolverDaemon
	threads  threadSource
	notifier *beadNotifier
	cfg      resolverConfig
	logger   *slog.Logger
	now      func() time.Time
}

func newResolver(daemon resolverDaemon, threads threadSource, cfg resolverConfig, logger *slog.Logger) *resolver {
	return &resolver{
		daemon:   daemon,
		threads:  threads,
		notifier: &beadNotifier{daemon: daemon, logger: logger},
		cfg:      cfg.withDefaults(),
		logger:   logger,
		now:      time.Now,
	}
}

// Run sweeps every Interval until ctx is done. The first sweep waits one
// interval: a restart loop must not turn into a respawn loop.
func (r *resolver) Run(ctx context.Context) {
	t := time.NewTicker(r.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := r.Sweep(ctx); err != nil && ctx.Err() == nil {
				r.logger.Warn("resolver: sweep failed", "error", err)
			}
		}
	}
}

// resolverStatuses are the task statuses a respawn may target. Closed work is
// finished by definition; deferred was parked on purpose.
var resolverStatuses = []string{"open", "in_progress", "blocked"}

// candidates lists the unfinished GitHub-linked work beads. Two queries,
// because a PR link made with `gb mr link` before the bridge saw the PR carries
// no forge field, but does carry the review flag once feedback has arrived.
func (r *resolver) candidates(ctx context.Context) ([]*beadsapi.BeadDetail, error) {
	var out []*beadsapi.BeadDetail
	seen := map[string]bool{}
	for _, f := range []map[string]string{{"forge": forgeGitHub}, {reviewFlagField: "true"}} {
		res, err := r.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
			Types:    []string{"task", "bug", "feature", "chore"},
			Statuses: resolverStatuses,
			Fields:   f,
			Sort:     "-updated_at",
			Limit:    200,
		})
		if err != nil {
			return nil, fmt.Errorf("list candidate beads: %w", err)
		}
		if res == nil {
			continue
		}
		for _, b := range res.Beads {
			if seen[b.ID] {
				continue
			}
			seen[b.ID] = true
			if _, ok := parsePRURL(b.Fields["mr_url"]); ok && workBeadTypes[b.Type] {
				out = append(out, b)
			}
		}
	}
	return out, nil
}

// Sweep runs one pass. Per-task errors are logged and skipped so one bad PR
// cannot starve the rest; only a failure to list candidates is returned.
func (r *resolver) Sweep(ctx context.Context) error {
	tasks, err := r.candidates(ctx)
	if err != nil {
		return err
	}
	checked, spawned := 0, 0
	for _, task := range tasks {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if checked >= maxCheckedPerSweep || spawned >= maxSpawnsPerSweep {
			r.logger.Info("resolver: sweep cap reached, rest wait for next tick",
				"checked", checked, "spawned", spawned, "candidates", len(tasks))
			break
		}
		did, askedGitHub, err := r.consider(ctx, task)
		if askedGitHub {
			checked++
		}
		if did {
			spawned++
		}
		if err != nil {
			r.logger.Warn("resolver: task skipped", "task", task.ID, "error", err)
		}
	}
	return nil
}

// consider applies the guards to one task. spawned reports an agent started
// (or, in dry-run, one that would have been); askedGitHub reports a GraphQL
// call was spent, for the per-sweep budget.
func (r *resolver) consider(ctx context.Context, task *beadsapi.BeadDetail) (spawned, askedGitHub bool, err error) {
	prURL := task.Fields["mr_url"]
	pr, _ := parsePRURL(prURL)

	live, err := r.liveAgent(ctx, task, prURL)
	if err != nil {
		return false, false, err
	}
	if live != "" {
		r.logger.Debug("resolver: agent still live, leaving feedback to it", "task", task.ID, "agent", live)
		return false, false, nil
	}

	now := r.now()
	if at, perr := time.Parse(time.RFC3339, task.Fields[fieldSpawnedAt]); perr == nil && now.Sub(at) < r.cfg.Cooldown {
		return false, false, nil
	}

	ag, err := findAgentForTask(ctx, r.daemon, task)
	if err != nil {
		return false, false, err
	}
	// Nobody ever worked this task: it is a PR a human linked, with no session
	// to continue and no one to hand the feedback to.
	if ag == nil || ag.Name == "" {
		return false, false, nil
	}

	state, err := r.threads.Threads(ctx, pr)
	if err != nil {
		return false, true, fmt.Errorf("review threads for %s: %w", prURL, err)
	}
	if state.State != "OPEN" || len(state.Unresolved) == 0 {
		return false, true, nil
	}

	hash := threadsHash(state.Unresolved)
	count := 0
	if task.Fields[fieldThreadsHash] == hash {
		count, _ = strconv.Atoi(task.Fields[fieldSpawnCount])
	}
	if count >= r.cfg.MaxRespawns {
		r.giveUp(ctx, ag, task, state, hash, prURL)
		return false, true, nil
	}

	if r.cfg.DryRun {
		r.logger.Info("resolver: DRY RUN, would respawn agent for unresolved review threads",
			"task", task.ID, "agent", ag.Name, "pr", prURL, "threads", len(state.Unresolved))
		return true, true, nil
	}

	// Stamp before spawning: if the spawn succeeds but the write after it
	// failed, the next sweep would spawn again. A failed spawn gives its
	// attempt back below, so a daemon hiccup does not burn the cap.
	stamp := map[string]string{
		fieldSpawnedAt:   now.UTC().Format(time.RFC3339),
		fieldSpawnCount:  strconv.Itoa(count + 1),
		fieldThreadsHash: hash,
	}
	if err := r.daemon.UpdateBeadFields(ctx, task.ID, stamp); err != nil {
		return false, true, fmt.Errorf("stamp %s: %w", task.ID, err)
	}

	extra := map[string]string{
		"spawn_source": resolverSpawnSource,
		"mr_url":       prURL,
		"forge":        forgeGitHub,
		// Refuse a second crew if one slipped in since the liveness check.
		"single_claim_task": "true",
	}
	if state.Branch != "" {
		extra["mr_source_branch"] = state.Branch
	}
	if op := task.Fields["operated_by"]; humanOperator(op) {
		extra["operated_by"] = op
	}
	project := projectLabel(task.Labels)
	agentBead, err := r.daemon.SpawnAgent(ctx, ag.Name, project, task.ID, resolverRole,
		resolverPrompt(task, prURL, state), extra)
	if err != nil {
		_ = r.daemon.UpdateBeadFields(ctx, task.ID, map[string]string{fieldSpawnCount: strconv.Itoa(count)})
		return false, true, fmt.Errorf("spawn %s for %s: %w", ag.Name, task.ID, err)
	}
	r.logger.Info("resolver: respawned agent for unresolved review threads",
		"task", task.ID, "agent", ag.Name, "agent_bead", agentBead, "pr", prURL,
		"threads", len(state.Unresolved), "attempt", count+1)

	key := "github-resolver:" + task.ID + ":" + hash + ":" + strconv.Itoa(count+1)
	r.notifier.postThread(ctx, ag, key, resolverThreadLine(prURL, pr, state, ag.Name, count+1, r.cfg.MaxRespawns))
	return true, true, nil
}

// giveUp tells the thread, once per thread set, that the resolver has stopped
// trying. Without the line the operator sees only silence.
func (r *resolver) giveUp(ctx context.Context, ag *agentTarget, task *beadsapi.BeadDetail, state *prThreads, hash, prURL string) {
	if task.Fields[fieldGaveUp] == hash {
		return
	}
	pr, _ := parsePRURL(prURL)
	line := fmt.Sprintf(":warning: %d review thread%s still unresolved on %s after %d respawns. Not respawning again; this needs a human.\n%s",
		len(state.Unresolved), plural(len(state.Unresolved)), prLink(prURL, pr, state.Title), r.cfg.MaxRespawns,
		threadQuotes(state.Unresolved))
	if !r.cfg.DryRun {
		if err := r.daemon.UpdateBeadFields(ctx, task.ID, map[string]string{fieldGaveUp: hash}); err != nil {
			r.logger.Warn("resolver: record give-up failed", "task", task.ID, "error", err)
		}
		r.notifier.postThread(ctx, ag, "github-resolver-giveup:"+task.ID+":"+hash, line)
	}
	r.logger.Warn("resolver: respawn cap reached, leaving threads to a human",
		"task", task.ID, "pr", prURL, "threads", len(state.Unresolved), "cap", r.cfg.MaxRespawns)
}

// liveAgent returns the name of an agent still working the task or the PR, or
// "". An agent counts as live the way the daemon's single-claim guard counts
// it: not closed, not in a terminal agent_state, and not stalled past
// ProgressStaleThreshold. The PR is checked as well as the task because a
// different task bead pointing at the same PR may have a worker on it.
func (r *resolver) liveAgent(ctx context.Context, task *beadsapi.BeadDetail, prURL string) (string, error) {
	for _, f := range []map[string]string{{"task_id": task.ID}, {"mr_url": prURL}} {
		res, err := r.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
			Types:    []string{"agent"},
			Statuses: resolverStatuses,
			Fields:   f,
			Limit:    20,
		})
		if err != nil {
			return "", fmt.Errorf("list live agents: %w", err)
		}
		if res == nil {
			continue
		}
		for _, a := range res.Beads {
			switch a.Fields["agent_state"] {
			case "done", "failed":
				continue
			}
			if beadsapi.AgentProgressStalled(a.Fields, r.now(), beadsapi.ProgressStaleThreshold) {
				continue
			}
			return firstNonEmpty(a.Fields["agent"], a.Title), nil
		}
	}
	return "", nil
}

// threadsHash fingerprints a set of unresolved threads by id, so the attempt
// cap resets when a reviewer opens a new thread but not when the same ones sit.
func threadsHash(ts []reviewThread) string {
	ids := make([]string, len(ts))
	for i, t := range ts {
		ids[i] = t.ID
	}
	sort.Strings(ids)
	sum := sha256.Sum256([]byte(strings.Join(ids, "\x00")))
	return hex.EncodeToString(sum[:])[:12]
}

func projectLabel(labels []string) string {
	for _, l := range labels {
		if v, ok := strings.CutPrefix(l, "project:"); ok {
			return v
		}
	}
	return ""
}

// humanOperator reports whether v is an operator id the daemon accepts on an
// audit-bearing write: slack:U<alnum> or oidc:<local>@<dotted.domain>. A copy
// of internal/bridge.IsHumanOperator's shape check, which this binary does not
// import (it would pull the whole Slack bridge in); keep them in step.
func humanOperator(v string) bool {
	if rest, ok := strings.CutPrefix(v, "slack:U"); ok {
		return rest != ""
	}
	if rest, ok := strings.CutPrefix(v, "oidc:"); ok {
		at := strings.IndexByte(rest, '@')
		if at <= 0 || at == len(rest)-1 {
			return false
		}
		d := rest[at+1:]
		return strings.Contains(d, ".") && !strings.HasPrefix(d, ".") && !strings.HasSuffix(d, ".")
	}
	return false
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func prLink(prURL string, pr prRef, title string) string {
	return fmt.Sprintf("<%s|#%d %s>", prURL, pr.Number, slackText(title))
}

// threadQuotes renders up to resolverMaxShown threads as Slack blockquotes,
// each headed by file, line and reviewer, with a note for the rest.
func threadQuotes(ts []reviewThread) string {
	var entries []string
	for i, t := range ts {
		if i == resolverMaxShown {
			entries = append(entries, fmt.Sprintf("_…and %d more_", len(ts)-i))
			break
		}
		loc := t.Path
		if loc != "" && t.Line > 0 {
			loc = fmt.Sprintf("%s:%d", t.Path, t.Line)
		}
		head := fmt.Sprintf("*%s*", slackText(t.Author))
		if loc != "" {
			head = fmt.Sprintf("`%s` · %s", slackText(loc), head)
		}
		if t.URL != "" {
			head = fmt.Sprintf("<%s|%s>", t.URL, head)
		}
		if q := quoteBlock(firstLine(t.Body)); q != "" {
			head += "\n" + q
		}
		entries = append(entries, head)
	}
	return strings.Join(entries, "\n")
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i]) + " …"
	}
	return s
}

// resolverThreadLine is what the agent's Slack thread shows when the resolver
// brings it back: why, how many, who said what, and which attempt this is.
func resolverThreadLine(prURL string, pr prRef, st *prThreads, agent string, attempt, max int) string {
	n := len(st.Unresolved)
	return fmt.Sprintf(":recycle: %d unresolved review thread%s on %s and `%s` is no longer running. Respawned it to address them (attempt %d of %d).\n%s",
		n, plural(n), prLink(prURL, pr, st.Title), slackText(agent), attempt, max, threadQuotes(st.Unresolved))
}

// resolverPrompt is the respawned agent's first instruction. It names the
// threads so the agent starts on the work rather than on discovery.
func resolverPrompt(task *beadsapi.BeadDetail, prURL string, st *prThreads) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You have been respawned to address %d unresolved review thread%s on PR %s (task %s: %s).",
		len(st.Unresolved), plural(len(st.Unresolved)), prURL, task.ID, task.Title)
	if st.Branch != "" {
		fmt.Fprintf(&b, " Check out the PR branch `%s`.", st.Branch)
	}
	b.WriteString(" Read each thread, make the change or reply with why not, push, and resolve the thread. Use `kd show " + task.ID + "` for the full task context.\n\nUnresolved threads:")
	for i, t := range st.Unresolved {
		if i == 10 {
			fmt.Fprintf(&b, "\n- …and %d more; list them with the GitHub API (reviewThreads).", len(st.Unresolved)-i)
			break
		}
		loc := t.Path
		if t.Line > 0 {
			loc = fmt.Sprintf("%s:%d", t.Path, t.Line)
		}
		fmt.Fprintf(&b, "\n- %s @%s", loc, t.Author)
		if t.Outdated {
			b.WriteString(" (outdated: the code moved)")
		}
		if t.URL != "" {
			fmt.Fprintf(&b, " %s", t.URL)
		}
		fmt.Fprintf(&b, "\n  %s", truncateRunes(firstLine(t.Body), 400))
	}
	return b.String()
}
