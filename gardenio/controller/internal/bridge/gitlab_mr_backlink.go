package bridge

// gitlab_mr_backlink.go — reverse MR→bead reconciler (bug ka-0000004261).
//
// The gitlab-bridge is otherwise forward-only: it watches beads that already
// carry an mr_url (set by `gb mr link` or the `gb stop --wrapup` guard) and
// dual-writes the type=merge-request bead + `produces` edge. If an agent
// forgets `gb mr link` AND omits the MR from its wrap-up pull_requests, the MR
// is invisible in beads-viewer forever (evidence: the whole PE-10133 chained-MR
// tree had zero mr_url).
//
// This reconciler closes that gap. On an interval it scans recently-updated
// open/merged MRs in the tracked GitLab group, and for each MR that has no
// type=merge-request bead yet, it tries to recover the owning bead from a
// PE-/FSM-/gd- key in the MR's source branch / title / description and sets
// mr_url on it — exactly what `gb mr link` does. The existing forward poller /
// SSE sync then mints the MR bead + produces edge on its next pass.
//
// Safety is the whole point: a wrong attachment is worse than no attachment.
// The matcher resolves a key to a single actionable bead (task/feature/bug)
// and refuses to guess. On zero or multiple candidates it emits a structured
// log and — when configured — files one low-priority "unlinked MR" bead so a
// gardener can resolve it by hand. It never attaches an MR to an ambiguous or
// epic-only match.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// mrBacklinkDaemon is the subset of the beads client the reverse reconciler
// needs. Kept small so the matching logic is unit-testable with a fake.
type mrBacklinkDaemon interface {
	// MRBeadByURL returns the canonical type=merge-request bead for a URL, or
	// (nil, nil) when none exists yet.
	MRBeadByURL(ctx context.Context, mrURL string) (*beadsapi.BeadDetail, error)
	// ListBeadsFiltered is used to resolve a JIRA/bead key to candidate beads.
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
	// GetBead resolves an explicit ka-XXXX reference directly.
	GetBead(ctx context.Context, beadID string) (*beadsapi.BeadDetail, error)
	// UpdateBeadFields performs the same mr_url write `gb mr link` does.
	UpdateBeadFields(ctx context.Context, beadID string, fields map[string]string) error
	// CreateBead is used (optionally) to file an "unlinked MR" bead when a key
	// is present but cannot be resolved to a single actionable bead.
	CreateBead(ctx context.Context, req beadsapi.CreateBeadRequest) (string, error)
}

// mrLister is the subset of the GitLab client the reconciler needs. Defined as
// an interface so tests can supply a canned MR list without HTTP.
type mrLister interface {
	ListGroupMergeRequests(ctx context.Context, groupID int, state string, updatedAfter time.Time) ([]GitLabMR, error)
}

// actionableBeadTypes are the issue types an MR may legitimately attach to.
// The epic is deliberately excluded — a chained-MR tree must link each slice
// MR to its slice bead, never to the umbrella epic.
var actionableBeadTypes = map[string]bool{
	"task":    true,
	"feature": true,
	"bug":     true,
	"chore":   true,
}

// MRBacklinkConfig configures the reverse MR→bead reconciler.
type MRBacklinkConfig struct {
	GitLab   mrLister
	Daemon   mrBacklinkDaemon
	Logger   *slog.Logger
	GroupID  int
	Interval time.Duration // how often to scan; default 15m
	Window   time.Duration // only consider MRs updated within this window; default 14d
	DryRun   bool          // when true, log intended links but do not write
	// FileUnlinkedBeads, when true, files a low-priority "unlinked MR" bead for
	// each MR that carries a key but cannot be resolved to one actionable bead,
	// so a gardener can resolve it. Off by default to avoid bead spam until the
	// matcher is trusted in the field.
	FileUnlinkedBeads bool
	// UnlinkedBeadProject is the project label applied to filed "unlinked MR"
	// beads (e.g. "gardenio"). Empty disables the project label.
	UnlinkedBeadProject string
}

// MRBacklinkReconciler periodically back-links GitLab MRs to their owning bead.
type MRBacklinkReconciler struct {
	gitlab              mrLister
	daemon              mrBacklinkDaemon
	logger              *slog.Logger
	groupID             int
	interval            time.Duration
	window              time.Duration
	dryRun              bool
	fileUnlinkedBeads   bool
	unlinkedBeadProject string

	// filed tracks MR URLs we have already filed an "unlinked MR" bead for, so
	// repeated scans of the same still-unresolvable MR don't spam new beads.
	filed map[string]bool
}

// NewMRBacklinkReconciler builds a reconciler from config, applying defaults.
func NewMRBacklinkReconciler(cfg MRBacklinkConfig) *MRBacklinkReconciler {
	interval := cfg.Interval
	if interval <= 0 {
		interval = 15 * time.Minute
	}
	window := cfg.Window
	if window <= 0 {
		window = 14 * 24 * time.Hour
	}
	return &MRBacklinkReconciler{
		gitlab:              cfg.GitLab,
		daemon:              cfg.Daemon,
		logger:              cfg.Logger,
		groupID:             cfg.GroupID,
		interval:            interval,
		window:              window,
		dryRun:              cfg.DryRun,
		fileUnlinkedBeads:   cfg.FileUnlinkedBeads,
		unlinkedBeadProject: cfg.UnlinkedBeadProject,
		filed:               map[string]bool{},
	}
}

// backlinkStats is the per-scan outcome tally, also returned for tests.
type backlinkStats struct {
	scanned        int // MRs examined
	alreadyLinked  int // skipped: a type=merge-request bead already exists
	noKey          int // skipped: no PE-/FSM-/gd- key in branch/title/description
	linked         int // mr_url set on the resolved bead
	ambiguous      int // key present but zero or multiple candidate beads
	clobberBlocked int // resolved bead already points at a different mr_url — refused
	filedUnlinked  int // "unlinked MR" beads created this scan
	errors         int // transport / write errors
}

// Run starts the reconciler loop, scanning immediately then every interval.
// It blocks until ctx is canceled. A zero group ID disables the reconciler.
func (r *MRBacklinkReconciler) Run(ctx context.Context) error {
	if r.groupID == 0 {
		r.logger.Info("reverse MR→bead reconciler disabled (no group id)")
		return nil
	}
	r.logger.Info("starting reverse MR→bead reconciler",
		"group_id", r.groupID, "interval", r.interval, "window", r.window, "dry_run", r.dryRun)

	r.scan(ctx)

	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			r.scan(ctx)
		}
	}
}

// scan performs one reconciliation pass over recently-updated group MRs.
func (r *MRBacklinkReconciler) scan(ctx context.Context) backlinkStats {
	since := time.Now().Add(-r.window)
	var s backlinkStats

	// Scan opened and merged MRs. Closed (abandoned) MRs are intentionally
	// skipped — there is no value in minting an MR bead for work that was
	// thrown away, and it would surface dead nodes in the constellation.
	var mrs []GitLabMR
	for _, state := range []string{"opened", "merged"} {
		batch, err := r.gitlab.ListGroupMergeRequests(ctx, r.groupID, state, since)
		if err != nil {
			s.errors++
			r.logger.Error("reverse reconciler: list MRs failed", "state", state, "error", err)
			continue
		}
		mrs = append(mrs, batch...)
	}

	for i := range mrs {
		r.processMR(ctx, &mrs[i], &s)
	}

	r.logger.Info("reverse MR→bead reconciler scan done",
		"scanned", s.scanned,
		"already_linked", s.alreadyLinked,
		"no_key", s.noKey,
		"linked", s.linked,
		"ambiguous", s.ambiguous,
		"clobber_blocked", s.clobberBlocked,
		"filed_unlinked", s.filedUnlinked,
		"errors", s.errors,
		"dry_run", r.dryRun)
	return s
}

// processMR handles a single MR: skip if already linked, extract a key, resolve
// it to exactly one actionable bead, and set mr_url. Ambiguity never guesses.
func (r *MRBacklinkReconciler) processMR(ctx context.Context, mr *GitLabMR, s *backlinkStats) {
	s.scanned++

	if mr.WebURL == "" {
		return
	}

	// Already linked? The canonical signal is an existing type=merge-request
	// bead keyed on the URL. Cheap and idempotent — the common case once the
	// forward poller has caught up.
	if bead, err := r.daemon.MRBeadByURL(ctx, mr.WebURL); err != nil {
		s.errors++
		r.logger.Warn("reverse reconciler: MRBeadByURL failed", "mr_url", mr.WebURL, "error", err)
		return
	} else if bead != nil {
		s.alreadyLinked++
		return
	}

	// Strip the lab-internal dark-provenance trailers (Dark-Bead:/Dark-MR:/
	// Provenance-Hash:) from the description before key extraction. A peel-on-
	// demand MR lives on the UPSTREAM host but its description anchors back to
	// DARK beads on the dark host; without this, the upstream bridge's
	// reconciler would extract that dark ka-XXXX and set mr_url on the dark bead
	// — the cross-host backlink false-attach E-PROV #21 (FW-6) forbids. The
	// legitimate (upstream) peel-request key still arrives via branch, title,
	// and the plain `Bead:` trailer.
	keys := extractMRBeadKeys(mr.SourceBranch, mr.Title, stripDarkProvenanceAnchors(mr.Description))
	if keys.empty() {
		s.noKey++
		r.logger.Debug("reverse reconciler: no recoverable key", "mr_url", mr.WebURL, "branch", mr.SourceBranch)
		return
	}

	target, reason := r.resolveTarget(ctx, keys)
	if target == nil {
		s.ambiguous++
		r.logger.Info("reverse reconciler: MR carries a key but no single actionable bead matched — NOT guessing",
			"mr_url", mr.WebURL, "branch", mr.SourceBranch, "keys", strings.Join(keys.all(), ","), "reason", reason)
		r.maybeFileUnlinked(ctx, mr, keys.all(), reason, s)
		return
	}

	// Clobber guard (bug ka-0000002276): the reconciler only BACKFILLS a missing
	// mr_url. If the resolved bead already points at a DIFFERENT MR — very often
	// its own, correct, already-merged fix that the forward path linked — never
	// silently repoint it at this MR. Refuse and log; a wrong overwrite is worse
	// than a stale link, and it corrupts the fleet-wide fix-state signal.
	if existing := strings.TrimSpace(target.Fields["mr_url"]); existing != "" && existing != mr.WebURL {
		s.clobberBlocked++
		r.logger.Warn("reverse reconciler: refusing to overwrite existing mr_url on bead",
			"mr_url", mr.WebURL, "bead", target.ID, "existing_mr_url", existing, "branch", mr.SourceBranch)
		return
	}

	if r.dryRun {
		r.logger.Info("reverse reconciler: would link MR (dry-run)",
			"mr_url", mr.WebURL, "bead", target.ID, "branch", mr.SourceBranch)
		s.linked++
		return
	}

	if err := r.daemon.UpdateBeadFields(ctx, target.ID, map[string]string{"mr_url": mr.WebURL}); err != nil {
		s.errors++
		r.logger.Error("reverse reconciler: set mr_url failed", "mr_url", mr.WebURL, "bead", target.ID, "error", err)
		return
	}
	s.linked++
	r.logger.Info("reverse reconciler: back-linked MR to bead (no `gb mr link` was run)",
		"mr_url", mr.WebURL, "bead", target.ID, "branch", mr.SourceBranch)
}

// resolveTarget maps the recovered keys to exactly one actionable bead. It
// returns (nil, reason) when the result is ambiguous (zero or multiple
// candidates), so the caller can log + optionally file an unlinked-MR bead.
//
// Resolution rules, in order:
//   - kd refs are resolved by SOURCE TIER, not alphabetically (bug
//     ka-0000002276). The SUBJECT tier (branch + title) is tried first: those
//     refs name the bead the MR fixes. Only if the subject tier yields no
//     actionable bead do we fall back to the BODY tier (description-only refs),
//     which are usually cross-references to OTHER beads. Within a tier, if more
//     than one DISTINCT kd ref resolves to an actionable bead, that's genuinely
//     ambiguous — refuse rather than pick one. (A kd ref to an epic is rejected
//     — never link an MR to an epic.)
//   - Otherwise resolve JIRA keys (PE-/FSM-/…) via the `jira:<key>` label,
//     preferring the most-recently-updated actionable child over the epic. If
//     more than one DISTINCT key maps to beads, that's ambiguous — refuse.
func (r *MRBacklinkReconciler) resolveTarget(ctx context.Context, keys mrBeadKeys) (*beadsapi.BeadDetail, string) {
	// 1. Explicit ka-XXXX references win, honoring source precedence. The
	// subject tier (branch/title) names the fix target; the body tier
	// (description-only) is consulted ONLY when the subject tier is empty, so a
	// description cross-ref can never outrank the real subject.
	if bead, reason, decided := r.resolveKDTier(ctx, "subject", keys.subjectKD); decided {
		return bead, reason
	}
	if bead, reason, decided := r.resolveKDTier(ctx, "description", keys.bodyKD); decided {
		return bead, reason
	}

	// 2. JIRA keys via the jira:<key> label. Collect the best actionable bead
	// per distinct key; if more than one distinct key yields a bead, refuse.
	// keys.jira is already ka-free, de-duplicated, and sorted.
	matchedKeys := map[string]*beadsapi.BeadDetail{}
	for _, k := range keys.jira {
		if _, done := matchedKeys[k]; done {
			continue
		}
		if best := r.bestActionableForJiraKey(ctx, k); best != nil {
			matchedKeys[k] = best
		}
	}

	switch len(matchedKeys) {
	case 0:
		return nil, "no actionable bead found for any key"
	case 1:
		for _, b := range matchedKeys {
			return b, ""
		}
	}
	// Multiple distinct keys each resolved to a bead — genuinely ambiguous.
	var ks []string
	for k := range matchedKeys {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return nil, fmt.Sprintf("multiple distinct keys matched beads: %s", strings.Join(ks, ","))
}

// resolveKDTier resolves one source tier of kd refs to a single actionable
// bead. It returns (bead, reason, decided):
//   - decided=true, bead!=nil  → exactly one ref in this tier resolved to an
//     actionable bead; use it and stop.
//   - decided=true, bead==nil  → this tier is CONCLUSIVE but not a clean match:
//     a ref resolved to a non-actionable bead (e.g. an epic), or two-or-more
//     distinct refs each resolved to an actionable bead (genuinely ambiguous).
//     Refuse — never fall through to a lower tier or guess.
//   - decided=false            → no ref in this tier resolved to any bead; the
//     caller may consult the next tier.
//
// The tier boundary is what fixes bug ka-0000002276: a description-only
// cross-ref (body tier) is consulted only when the branch/title (subject tier)
// names no resolvable bead, so it can never outrank the MR's real subject.
func (r *MRBacklinkReconciler) resolveKDTier(ctx context.Context, tier string, refs []string) (*beadsapi.BeadDetail, string, bool) {
	var matched []*beadsapi.BeadDetail
	for _, k := range refs {
		bead, err := r.daemon.GetBead(ctx, k)
		if err != nil || bead == nil || bead.ID == "" {
			continue
		}
		if !isActionableBead(bead) {
			return nil, fmt.Sprintf("%s kd ref %s is type=%s, not an actionable bead", tier, k, bead.Type), true
		}
		matched = append(matched, bead)
	}

	switch len(matched) {
	case 0:
		return nil, "", false
	case 1:
		return matched[0], "", true
	default:
		var ids []string
		for _, b := range matched {
			ids = append(ids, b.ID)
		}
		sort.Strings(ids)
		return nil, fmt.Sprintf("multiple %s kd refs matched actionable beads: %s", tier, strings.Join(ids, ",")), true
	}
}

// bestActionableForJiraKey resolves a JIRA key to its best actionable bead via
// the `jira:<key>` label, preferring the most-recently-updated task/feature/bug
// over the epic. Returns nil if no actionable bead carries the label.
func (r *MRBacklinkReconciler) bestActionableForJiraKey(ctx context.Context, key string) *beadsapi.BeadDetail {
	res, err := r.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Labels: []string{"jira:" + key},
		Limit:  100,
	})
	if err != nil || res == nil {
		if err != nil {
			r.logger.Warn("reverse reconciler: jira label lookup failed", "key", key, "error", err)
		}
		return nil
	}
	var actionable []*beadsapi.BeadDetail
	for _, b := range res.Beads {
		if isActionableBead(b) {
			actionable = append(actionable, b)
		}
	}
	if len(actionable) == 0 {
		return nil
	}
	sort.SliceStable(actionable, func(i, j int) bool {
		return actionable[i].UpdatedAt.After(actionable[j].UpdatedAt)
	})
	return actionable[0]
}

// maybeFileUnlinked files one low-priority "unlinked MR" bead per still-
// unresolvable MR (once), when enabled, so a gardener can resolve it by hand.
func (r *MRBacklinkReconciler) maybeFileUnlinked(ctx context.Context, mr *GitLabMR, keys []string, reason string, s *backlinkStats) {
	if !r.fileUnlinkedBeads || r.dryRun {
		return
	}
	if r.filed[mr.WebURL] {
		return
	}

	labels := []string{"unlinked-mr"}
	if r.unlinkedBeadProject != "" {
		labels = append(labels, "project:"+r.unlinkedBeadProject)
	}
	fields, _ := json.Marshal(map[string]string{
		"mr_url":           mr.WebURL,
		"mr_source_branch": mr.SourceBranch,
	})
	desc := fmt.Sprintf(
		"The reverse MR→bead reconciler found a recognizable key but could not safely link this MR to a single actionable bead.\n\n"+
			"- MR: %s\n- Branch: %s\n- Title: %s\n- Keys seen: %s\n- Reason: %s\n\n"+
			"Resolve by running `gb mr link <correct-bead-id> %s`, then close this bead.",
		mr.WebURL, mr.SourceBranch, mr.Title, strings.Join(keys, ","), reason, mr.WebURL)

	id, err := r.daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:       fmt.Sprintf("Unlinked MR needs manual `gb mr link`: %s", mr.Title),
		Type:        "task",
		Kind:        "issue",
		Description: desc,
		Labels:      labels,
		Priority:    3,
		Fields:      fields,
	})
	if err != nil {
		s.errors++
		r.logger.Error("reverse reconciler: file unlinked-MR bead failed", "mr_url", mr.WebURL, "error", err)
		return
	}
	r.filed[mr.WebURL] = true
	s.filedUnlinked++
	r.logger.Info("reverse reconciler: filed unlinked-MR bead", "mr_url", mr.WebURL, "bead", id)
}

// isActionableBead reports whether a bead is a valid MR attachment target: an
// issue-kind task/feature/bug/chore (never an epic, agent, decision, etc.).
func isActionableBead(b *beadsapi.BeadDetail) bool {
	if b == nil {
		return false
	}
	if b.Kind != "" && b.Kind != "issue" {
		return false
	}
	return actionableBeadTypes[b.Type]
}
