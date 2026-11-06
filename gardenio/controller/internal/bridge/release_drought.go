// Release-drought deadman watchdog (ka-0000002594).
//
// The 2026-07-01/02 P0 (ka-0000007176): 16 consecutive tag releases failed
// silently over ~15h because the release pipeline's OWN failure notifiers were
// broken (missing SLACK_RELEASE_WEBHOOK CI vault secret; 403 on the
// release-gate bug-bead auto-file). A broken pipeline hid behind a broken
// notifier; the operator noticed only by the ABSENCE of releases.
//
// This watchdog is the deadman switch for that failure class: it alerts on the
// absence of success, not the presence of failure, and shares NO code path
// with the release pipeline's notify legs. It polls the GitLab API directly
// (tags + Releases) from the gitlab-bridge process and alerts via:
//
//  1. the TriggerNotifier (in prod the trigger-notice bead relay: a closed
//     message bead that slack-bridge posts to Slack on its own credentials),
//  2. a P0 bug bead filed through the beads daemon (BEADS_DAEMON_TOKEN — not
//     the CI write token that 403'd in the P0), which enters the ready queue
//     for gardener-delivery triage,
//  3. Prometheus gauges on the host binary's /metrics endpoint, so an
//     optional PrometheusRule adds an alerting leg that survives even a
//     beads-daemon outage (and an absent() rule can watch the watchdog).
//
// DROUGHT SEMANTICS: a drought is "the oldest unreleased calver tag has been
// waiting longer than Threshold". A GitLab Release object is the canonical
// full-success artifact — `create-release` is the LAST step of the release
// pipeline, so a tag without a Release (and no newer released tag) did not
// fully ship. Anchoring at the oldest waiting tag's push time (rather than a
// wall-clock window) means an in-flight pipeline for a minutes-old tag never
// false-positives, while a tag stuck for Threshold hours always alerts — the
// exact "new tags pushed but no success landed for N hours" acceptance shape.
package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// Release-drought defaults. Threshold matches the post-mortem suggestion
// (N=6h); the check interval keeps detection latency well under one threshold
// while staying trivially cheap (two GET requests per tick).
const (
	releaseDroughtDefaultThreshold = 6 * time.Hour
	releaseDroughtDefaultInterval  = 15 * time.Minute
	releaseDroughtDefaultProject   = "example-org/gardenio"
	releaseDroughtBeadLabel        = "release-drought"
)

// releaseDroughtGitLab is the read-only slice of GitLabClient the watchdog
// uses. Both calls are plain GETs; the watchdog never mutates GitLab state.
type releaseDroughtGitLab interface {
	ListTags(ctx context.Context, projectPath string) ([]GitLabTag, error)
	ListReleases(ctx context.Context, projectPath string) ([]GitLabRelease, error)
}

// releaseDroughtDaemon is the slice of the beads daemon client used to dedup
// and file the drought bug bead. Matched by *beadsapi.Client and by the
// package test mock.
type releaseDroughtDaemon interface {
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
	CreateBead(ctx context.Context, req beadsapi.CreateBeadRequest) (string, error)
}

// ReleaseDroughtConfig configures the watchdog.
type ReleaseDroughtConfig struct {
	GitLab   releaseDroughtGitLab
	Daemon   releaseDroughtDaemon
	Notifier TriggerNotifier
	Logger   *slog.Logger

	// Project is the GitLab project path whose release lane is watched.
	Project string
	// Threshold is how long the oldest unreleased calver tag may wait before
	// the drought alarm fires. Default 6h.
	Threshold time.Duration
	// Interval is the poll cadence. Default 15m.
	Interval time.Duration
	// Repeat re-alerts every Repeat while a drought episode persists, so a
	// multi-hour drought pages more than once. 0 disables re-alerts (one
	// alert per episode). Default = Threshold.
	Repeat time.Duration
	// CreateBead files a P0 bug bead (deduped on an open release-drought
	// label) on the first alert of an episode.
	CreateBead bool
	// BeadProject labels the filed bug bead (project:<BeadProject>) so it
	// enters that project's ready queue. Default "gardenio".
	BeadProject string

	// Now overrides the clock for tests. Default time.Now.
	Now func() time.Time
}

// ReleaseDroughtWatchdog polls GitLab and raises the drought alarm. One
// instance watches one project.
type ReleaseDroughtWatchdog struct {
	gitlab      releaseDroughtGitLab
	daemon      releaseDroughtDaemon
	notifier    TriggerNotifier
	logger      *slog.Logger
	project     string
	threshold   time.Duration
	interval    time.Duration
	repeat      time.Duration
	createBead  bool
	beadProject string
	now         func() time.Time

	mu           sync.Mutex
	droughtSince time.Time // first observation of the current episode; zero = healthy
	lastAlertAt  time.Time
}

// NewReleaseDroughtWatchdog builds a watchdog with defaults applied. Returns
// nil if the GitLab client is missing — the watchdog is a no-op without one.
func NewReleaseDroughtWatchdog(cfg ReleaseDroughtConfig) *ReleaseDroughtWatchdog {
	if cfg.GitLab == nil {
		return nil
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.Project == "" {
		cfg.Project = releaseDroughtDefaultProject
	}
	if cfg.Threshold <= 0 {
		cfg.Threshold = releaseDroughtDefaultThreshold
	}
	if cfg.Interval <= 0 {
		cfg.Interval = releaseDroughtDefaultInterval
	}
	if cfg.Repeat < 0 {
		cfg.Repeat = 0
	} else if cfg.Repeat == 0 {
		cfg.Repeat = cfg.Threshold
	}
	if cfg.BeadProject == "" {
		cfg.BeadProject = "gardenio"
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &ReleaseDroughtWatchdog{
		gitlab:      cfg.GitLab,
		daemon:      cfg.Daemon,
		notifier:    cfg.Notifier,
		logger:      logger,
		project:     cfg.Project,
		threshold:   cfg.Threshold,
		interval:    cfg.Interval,
		repeat:      cfg.Repeat,
		createBead:  cfg.CreateBead,
		beadProject: cfg.BeadProject,
		now:         cfg.Now,
	}
}

// Run checks immediately, then on every tick until ctx is cancelled.
func (w *ReleaseDroughtWatchdog) Run(ctx context.Context) error {
	w.logger.Info("release-drought watchdog started",
		"project", w.project, "threshold", w.threshold.String(),
		"interval", w.interval.String(), "repeat", w.repeat.String(),
		"create_bead", w.createBead)

	w.Check(ctx)

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			w.logger.Info("release-drought watchdog stopping")
			return ctx.Err()
		case <-ticker.C:
			w.Check(ctx)
		}
	}
}

// releaseDroughtVerdict is the outcome of one drought assessment.
type releaseDroughtVerdict struct {
	Drought          bool
	AnchorTag        string    // oldest unreleased calver tag
	AnchorTime       time.Time // its commit timestamp
	AnchorAge        time.Duration
	UnreleasedCount  int
	LatestReleaseTag string    // newest released calver tag; "" if none
	LatestReleaseAt  time.Time // zero if none
}

// assessReleaseDrought derives the drought verdict from raw tag/release
// listings. Pure function — all I/O stays in Check.
//
// A calver tag counts as unreleased when its calver key is strictly newer
// than the newest released calver (Releases with upcoming_release are
// ignored: they are pre-created placeholders, not shipped releases). Keys are
// compared, not tag-name set membership, so a release cut for a newer tag
// retires every older waiting tag with it.
func assessReleaseDrought(tags []GitLabTag, releases []GitLabRelease, now time.Time, threshold time.Duration) releaseDroughtVerdict {
	var v releaseDroughtVerdict

	// Newest released calver.
	var relYear, relDoy, relBuild int
	haveRelease := false
	for _, r := range releases {
		if r.UpcomingRel {
			continue
		}
		y, d, b, ok := calverKey(r.TagName)
		if !ok {
			continue
		}
		if !haveRelease || calverNewer(y, d, b, relYear, relDoy, relBuild) {
			relYear, relDoy, relBuild = y, d, b
			haveRelease = true
			v.LatestReleaseTag = r.TagName
			v.LatestReleaseAt = parseGitLabTime(r.ReleasedAt)
		}
	}

	// Oldest unreleased calver tag by commit time. Tags without a parseable
	// timestamp are skipped: with no age there is no deadman anchor, and
	// counting them could only manufacture an unageable alert.
	for _, t := range tags {
		y, d, b, ok := calverKey(t.Name)
		if !ok {
			continue
		}
		if haveRelease && !calverNewer(y, d, b, relYear, relDoy, relBuild) {
			continue
		}
		ts := parseGitLabTime(t.Commit.CreatedAt)
		if ts.IsZero() {
			continue
		}
		v.UnreleasedCount++
		if v.AnchorTime.IsZero() || ts.Before(v.AnchorTime) {
			v.AnchorTag = t.Name
			v.AnchorTime = ts
		}
	}

	if v.AnchorTag != "" {
		v.AnchorAge = now.Sub(v.AnchorTime)
		v.Drought = v.AnchorAge > threshold
	}
	return v
}

// calverNewer reports whether calver key a (year/doy/build) is strictly newer
// than key b.
func calverNewer(aYear, aDoy, aBuild, bYear, bDoy, bBuild int) bool {
	if aYear != bYear {
		return aYear > bYear
	}
	if aDoy != bDoy {
		return aDoy > bDoy
	}
	return aBuild > bBuild
}

// Check performs one fetch + assess + alert pass. Exported so tests (and a
// future manual probe) can drive single passes without the ticker.
func (w *ReleaseDroughtWatchdog) Check(ctx context.Context) {
	now := w.now()

	tags, err := w.gitlab.ListTags(ctx, w.project)
	if err != nil {
		w.checkFailed("list tags", err)
		return
	}
	releases, err := w.gitlab.ListReleases(ctx, w.project)
	if err != nil {
		w.checkFailed("list releases", err)
		return
	}

	v := assessReleaseDrought(tags, releases, now, w.threshold)
	w.recordMetrics(now, v)

	if !v.Drought {
		w.mu.Lock()
		recovered := !w.droughtSince.IsZero()
		w.droughtSince = time.Time{}
		w.lastAlertAt = time.Time{}
		w.mu.Unlock()
		if recovered {
			w.logger.Info("release-drought watchdog: drought RECOVERED",
				"project", w.project,
				"latest_release", v.LatestReleaseTag)
			w.notify(ctx, fmt.Sprintf(
				"✅ release-drought recovered: %s released (%d calver tags still queued)",
				v.LatestReleaseTag, v.UnreleasedCount))
		}
		return
	}

	w.mu.Lock()
	firstOfEpisode := w.droughtSince.IsZero()
	if firstOfEpisode {
		w.droughtSince = now
	}
	shouldAlert := firstOfEpisode ||
		(w.repeat > 0 && now.Sub(w.lastAlertAt) >= w.repeat)
	if shouldAlert {
		w.lastAlertAt = now
	}
	w.mu.Unlock()

	w.logger.Warn("release-drought watchdog: DROUGHT",
		"project", w.project,
		"anchor_tag", v.AnchorTag,
		"anchor_age", v.AnchorAge.Round(time.Minute).String(),
		"threshold", w.threshold.String(),
		"unreleased_tags", v.UnreleasedCount,
		"latest_release", v.LatestReleaseTag,
		"alerting", shouldAlert)

	if !shouldAlert {
		return
	}

	msg := w.droughtMessage(v, now)
	w.notify(ctx, msg)
	if w.createBead && firstOfEpisode {
		w.fileDroughtBead(ctx, v, msg)
	}
}

// checkFailed records a failed poll. Episode state is deliberately NOT reset:
// a transient GitLab hiccup mid-drought must not silence the next re-alert.
func (w *ReleaseDroughtWatchdog) checkFailed(op string, err error) {
	releaseDroughtCheckErrors.WithLabelValues(w.project).Inc()
	w.logger.Warn("release-drought watchdog: check failed", "op", op, "project", w.project, "error", err)
}

// droughtMessage renders the loud human-facing alert.
func (w *ReleaseDroughtWatchdog) droughtMessage(v releaseDroughtVerdict, now time.Time) string {
	lastRelease := "NONE FOUND"
	if v.LatestReleaseTag != "" {
		lastRelease = fmt.Sprintf("%s (%s ago)",
			v.LatestReleaseTag, now.Sub(v.LatestReleaseAt).Round(time.Minute))
	}
	return fmt.Sprintf(
		"🚨🚨 RELEASE DROUGHT (%s): tag %s has waited %s for a release (threshold %s) — "+
			"%d unreleased calver tag(s) queued; last successful release: %s. "+
			"Tags are being cut but nothing is shipping — the release pipeline is failing, "+
			"possibly SILENTLY (its own failure notifier has broken before: P0 ka-0000007176). "+
			"Check the tag pipelines: https://gitlab.com/%s/-/pipelines · "+
			"Runbook: gardenio/docs/release-drought-watchdog.md",
		w.project, v.AnchorTag, v.AnchorAge.Round(time.Minute), w.threshold,
		v.UnreleasedCount, lastRelease, w.project)
}

// notify sends msg through the configured notifier. Nil notifier is a logged
// no-op — the metrics + bead legs still fire.
func (w *ReleaseDroughtWatchdog) notify(ctx context.Context, msg string) {
	if w.notifier == nil {
		w.logger.Warn("release-drought watchdog: no notifier configured; skipping notify", "message", msg)
		return
	}
	w.notifier(ctx, "release-drought-watchdog", msg, "")
}

// fileDroughtBead files the P0 bug bead for a new drought episode, deduped
// against any open/in-progress bug already carrying the release-drought
// label — a drought that outlives watchdog restarts must not pile up beads.
func (w *ReleaseDroughtWatchdog) fileDroughtBead(ctx context.Context, v releaseDroughtVerdict, msg string) {
	if w.daemon == nil {
		return
	}

	existing, err := w.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"bug"},
		Statuses: []string{"open", "in_progress"},
		Labels:   []string{releaseDroughtBeadLabel},
		Limit:    1,
	})
	if err != nil {
		w.logger.Warn("release-drought watchdog: dedup query failed; filing anyway", "error", err)
	} else if len(existing.Beads) > 0 {
		w.logger.Info("release-drought watchdog: open drought bead already exists; not filing another",
			"bead", existing.Beads[0].ID)
		return
	}

	title := fmt.Sprintf("Release drought: %s unreleased for %s (%d tags queued)",
		v.AnchorTag, v.AnchorAge.Round(time.Minute), v.UnreleasedCount)
	desc := fmt.Sprintf(
		"related: ka-0000007176\n\n%s\n\n"+
			"Filed by the release-drought deadman watchdog (ka-0000002594). "+
			"This alarm means calver tags are being pushed but NO GitLab Release has been "+
			"created for %s+ — the release pipeline is failing end-to-end, and its own "+
			"failure notifiers cannot be trusted (that is why this watchdog exists).\n\n"+
			"Triage: read the newest tag pipelines and the images.yml run for that tag; "+
			"see gardenio/docs/release-drought-watchdog.md.",
		msg, w.threshold)

	id, err := w.daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:       title,
		Type:        "bug",
		Description: desc,
		Priority:    0,
		Labels: []string{
			releaseDroughtBeadLabel,
			"source:release-drought-watchdog",
			"project:" + w.beadProject,
		},
		CreatedBy: "release-drought-watchdog",
	})
	if err != nil {
		w.logger.Warn("release-drought watchdog: failed to file drought bug bead", "error", err)
		return
	}
	w.logger.Info("release-drought watchdog: filed drought bug bead", "bead", id)
}

// recordMetrics publishes the verdict to the Prometheus gauges so a
// PrometheusRule can alert on drought (and on the watchdog going silent)
// without any bead/Slack dependency.
func (w *ReleaseDroughtWatchdog) recordMetrics(now time.Time, v releaseDroughtVerdict) {
	active := 0.0
	if v.Drought {
		active = 1.0
	}
	releaseDroughtActive.WithLabelValues(w.project).Set(active)
	releaseDroughtUnreleasedTags.WithLabelValues(w.project).Set(float64(v.UnreleasedCount))
	if v.AnchorTag != "" {
		releaseDroughtOldestUnreleasedAge.WithLabelValues(w.project).Set(now.Sub(v.AnchorTime).Seconds())
	} else {
		releaseDroughtOldestUnreleasedAge.WithLabelValues(w.project).Set(0)
	}
	if !v.LatestReleaseAt.IsZero() {
		releaseDroughtLastReleaseTS.WithLabelValues(w.project).Set(float64(v.LatestReleaseAt.Unix()))
	}
	releaseDroughtLastCheckTS.WithLabelValues(w.project).Set(float64(now.Unix()))
}
