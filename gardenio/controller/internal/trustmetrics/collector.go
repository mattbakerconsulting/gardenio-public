package trustmetrics

import (
	"context"
	"log/slog"
	"strings"
	"time"
)

// MRMeta is the raw per-MR metadata the GitLab source yields before
// enrichment. It is the source-neutral shape the adapter (package bridge)
// converts a *bridge.GitLabMR into, so the join logic below is testable with
// fakes and never imports bridge.
type MRMeta struct {
	ProjectPath string
	IID         int
	State       string // opened, closed, merged, locked
	WebURL      string
	OpenedAt    time.Time
	MergedAt    time.Time
	Description string
}

// DiffStats mirrors bridge.GitLabMRDiffStats in source-neutral form.
type DiffStats struct {
	Additions     int
	Deletions     int
	FilesChanged  int
	CodeDiffChars int
}

// Approvals mirrors bridge.GitLabMRApprovals in source-neutral form.
type Approvals struct {
	Approved   bool
	ApprovedBy []string
}

// Note is one MR note (comment), source-neutral.
type Note struct {
	Author string
	System bool
	Body   string
}

// PeelInfo carries the beads-side facts joined onto an MR: the peel-request
// timestamp (#10), provenance completeness + bead link (#8), the merge-time
// trust survey score (#3), and the escaped-defect marker (#1/#4 balancing).
type PeelInfo struct {
	PeelRequestedAt       time.Time
	HasProvenanceBeadLink bool
	TrustScore            int
	EscapedDefect         bool
}

// GitLabSource is the read-only dark GitLab MR API surface the collector needs.
// *bridge.GitLabClient (via the adapter in package bridge) satisfies it; tests
// inject a fake. ListTrickleMRs applies the "which MRs deliver dark work
// upstream" policy (group + target-branch / label filter) in the adapter so the
// collector stays policy-free.
type GitLabSource interface {
	ListTrickleMRs(ctx context.Context, updatedAfter time.Time) ([]MRMeta, error)
	DiffStats(ctx context.Context, projectPath string, iid int) (DiffStats, error)
	Approvals(ctx context.Context, projectPath string, iid int) (Approvals, error)
	Notes(ctx context.Context, projectPath string, iid int) ([]Note, error)
}

// BeadSource is the beads surface the collector needs: it resolves an MR (by
// web URL) to its peel-request timestamp, provenance bead link, trust survey
// score, and escaped-defect marker. The adapter walks the type=merge-request
// bead and its producing peel-request bead(s). A miss returns a zero PeelInfo
// with found=false; the MR still contributes its GitLab-side metrics.
type BeadSource interface {
	PeelInfoForMR(ctx context.Context, mrWebURL string) (info PeelInfo, found bool, err error)
}

// Clock is injectable for deterministic tests.
type Clock func() time.Time

// CollectorConfig configures the periodic R5 metric collector.
type CollectorConfig struct {
	GitLab   GitLabSource
	Beads    BeadSource
	Metrics  *Collector
	Logger   *slog.Logger
	Interval time.Duration // collection cadence; default 10m
	Window   time.Duration // look-back window for "recent" trickle MRs; default 28d
	BotUser  string        // GitLab username of the bot; its notes are excluded from review counts
	Clock    Clock
}

// PeriodicCollector runs the R5 collection loop: every Interval it pulls the
// trickle MRs updated within Window, enriches each with diff stats / approvals
// / notes (GitLab) and peel-request / provenance / trust facts (beads), reduces
// them to the R5 metric table via the pure Compute, and publishes the gauges.
type PeriodicCollector struct {
	gitlab   GitLabSource
	beads    BeadSource
	metrics  *Collector
	logger   *slog.Logger
	interval time.Duration
	window   time.Duration
	botUser  string
	clock    Clock
}

// NewPeriodicCollector builds the loop. Defaults: Interval 10m, Window 28d.
func NewPeriodicCollector(cfg CollectorConfig) *PeriodicCollector {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	window := cfg.Window
	if window <= 0 {
		window = 28 * 24 * time.Hour
	}
	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}
	return &PeriodicCollector{
		gitlab:   cfg.GitLab,
		beads:    cfg.Beads,
		metrics:  cfg.Metrics,
		logger:   logger,
		interval: interval,
		window:   window,
		botUser:  cfg.BotUser,
		clock:    clock,
	}
}

// Run executes one collection immediately, then on every interval tick until
// ctx is cancelled.
func (p *PeriodicCollector) Run(ctx context.Context) error {
	if err := p.collectOnce(ctx); err != nil && ctx.Err() == nil {
		p.logger.Warn("R5 trust-metrics: initial collection failed", "error", err)
	}
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := p.collectOnce(ctx); err != nil && ctx.Err() == nil {
				p.logger.Warn("R5 trust-metrics: collection failed", "error", err)
			}
		}
	}
}

// collectOnce performs one full collect → compute → publish cycle.
func (p *PeriodicCollector) collectOnce(ctx context.Context) error {
	now := p.clock()
	since := now.Add(-p.window)

	mrs, err := p.gitlab.ListTrickleMRs(ctx, since)
	if err != nil {
		return err
	}

	records := make([]MRRecord, 0, len(mrs))
	for _, mr := range mrs {
		rec := p.buildRecord(ctx, mr)
		records = append(records, rec)
	}

	m := Compute(records, WindowDaysFor(p.window))
	p.metrics.Publish(m, now.Unix())
	p.logger.Debug("R5 trust-metrics: published",
		"trickle_mrs", m.TotalMRCount, "delivered", m.DeliveredMRCount,
		"trusted_throughput_per_week", m.TrustedThroughputPerWeek,
		"diff_size_p50", m.DiffSizeP50, "provenance_completeness", m.ProvenanceCompleteness)
	return nil
}

// buildRecord enriches one MR with all per-source facts and joins beads data.
// Per-MR enrichment failures are logged and tolerated — a partial record still
// contributes the facts that did resolve, which is better than dropping the MR
// entirely from the window.
func (p *PeriodicCollector) buildRecord(ctx context.Context, mr MRMeta) MRRecord {
	rec := MRRecord{
		ProjectPath: mr.ProjectPath,
		IID:         mr.IID,
		State:       mr.State,
		OpenedAt:    mr.OpenedAt,
		MergedAt:    mr.MergedAt,
	}

	if ds, err := p.gitlab.DiffStats(ctx, mr.ProjectPath, mr.IID); err != nil {
		p.logger.Debug("R5 trust-metrics: diff stats failed", "mr", mr.WebURL, "error", err)
	} else {
		rec.LinesAdded = ds.Additions
		rec.LinesRemoved = ds.Deletions
		rec.FilesChanged = ds.FilesChanged
		rec.CodeDiffChars = ds.CodeDiffChars
	}

	// Verbosity ratio (#7): description prose with the structured provenance
	// block excluded (§3.4). ProvenanceComplete (#8) is also determined here
	// from whether the description carries the structured provenance block.
	prose, hasProvBlock := splitProvenance(mr.Description)
	rec.DescriptionChars = len(prose)

	if ap, err := p.gitlab.Approvals(ctx, mr.ProjectPath, mr.IID); err != nil {
		p.logger.Debug("R5 trust-metrics: approvals failed", "mr", mr.WebURL, "error", err)
	} else {
		rec.Approved = ap.Approved
		rec.ApprovedBy = ap.ApprovedBy
	}

	if notes, err := p.gitlab.Notes(ctx, mr.ProjectPath, mr.IID); err != nil {
		p.logger.Debug("R5 trust-metrics: notes failed", "mr", mr.WebURL, "error", err)
	} else {
		rounds, total, reviewers := p.summarizeNotes(notes)
		rec.ReviewRounds = rounds
		rec.HumanReviewNotes = total
		rec.Reviewers = reviewers
	}

	// Join beads: peel-request timestamp (#10), provenance bead link (#8),
	// trust survey score (#3), escaped-defect marker (#1/#4 balancing).
	hasBeadLink := false
	if p.beads != nil {
		if info, found, err := p.beads.PeelInfoForMR(ctx, mr.WebURL); err != nil {
			p.logger.Debug("R5 trust-metrics: bead join failed", "mr", mr.WebURL, "error", err)
		} else if found {
			rec.PeelRequestedAt = info.PeelRequestedAt
			rec.TrustScore = info.TrustScore
			rec.EscapedDefect = info.EscapedDefect
			hasBeadLink = info.HasProvenanceBeadLink
		}
	}
	rec.HasProvenanceBeadLink = hasBeadLink

	// #8: a trickle MR's provenance is complete when it carries the structured
	// provenance block AND is linked to its producing bead(s).
	rec.ProvenanceComplete = hasProvBlock && hasBeadLink

	return rec
}

// summarizeNotes reduces MR notes to (review rounds, total human notes, distinct
// reviewers). System notes and the bot's own notes are excluded. A "review
// round" is a human note that is not a pure approval acknowledgement — every
// substantive human note counts toward rounds, capturing the back-and-forth
// the rework-cycles (#4) and review-effort (#2) metrics measure.
func (p *PeriodicCollector) summarizeNotes(notes []Note) (rounds, totalHuman int, reviewers []string) {
	seen := map[string]struct{}{}
	for _, n := range notes {
		if n.System {
			continue
		}
		if p.botUser != "" && strings.EqualFold(n.Author, p.botUser) {
			continue
		}
		totalHuman++
		rounds++
		if n.Author != "" {
			if _, ok := seen[n.Author]; !ok {
				seen[n.Author] = struct{}{}
				reviewers = append(reviewers, n.Author)
			}
		}
	}
	return rounds, totalHuman, reviewers
}
