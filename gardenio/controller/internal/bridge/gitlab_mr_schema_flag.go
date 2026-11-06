package bridge

// gitlab_mr_schema_flag.go — review-time MR-comment bot (G5, ka-0000007779).
//
// A cheap, early "yellow flag" that complements the authoritative CI drift
// guard (G1). Four times in three months a prod outage came from the same
// class of mistake: a field was added to a `type:*` config in the gardenio
// bridge's init.go `configs()` map WITHOUT the matching registration in the
// daemon's builtinConfigs (kbeads/internal/server/config.go). Every typed write
// carrying the new field then 400s in prod, because the daemon rejects the
// unknown field (incidents ka-0000001549, ka-0000004509, ka-0000000811,
// ka-0000003673 — the exact shape of MR !7647 / f8dab085f, which added
// `resolved_by` and `owner` to type:agent and touched no config.go).
//
// The CI guard (G1) is authoritative but it is a red X only AFTER a full test
// run — a reviewer has to open the pipeline and dig through subtasks to learn
// WHY. This bot is a yellow flag BEFORE that: on an interval it scans the
// group's open MRs and, for any MR that adds a `type:*` field to init.go's
// `configs()` while leaving config.go untouched, posts one comment naming the
// incident IDs and the exact fix. It is belt-and-suspenders, never a
// replacement for the CI guard.
//
// Safety / idempotency:
//   - It only ever POSTS a comment and ADDS a label. It never merges, closes,
//     or touches main — a reviewer still decides.
//   - It fires exactly once per MR: the first fire stamps the MR with the
//     `bridge-schema-flag` label, and any MR already carrying that label is
//     skipped. A fresh per-MR re-fetch immediately before writing re-checks the
//     label and open state, so two overlapping scans (or a race with a human
//     adding the label) cannot double-comment.
//   - The trigger is narrow: init_configs.yaml (the data configs() loads)
//     touched AND a `- {name: …}` field line added in its diff AND config.go
//     NOT touched. An MR that correctly updates
//     both files never fires; a config.go-only change never fires.

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"
)

// Paths whose cross-module relationship this bot guards. Repo-relative, matching
// the NewPath/OldPath GitLab reports for a change.
const (
	// bridgeInitConfigsPath is the bridge config data (embedded by configs() in
	// init.go) that declares every type:* field the daemon must also know about.
	bridgeInitConfigsPath = "gardenio/controller/internal/bridge/init_configs.yaml"
	// daemonConfigGoPath is the daemon's builtinConfigs registry — the file that
	// MUST change in lockstep when init_configs.yaml adds a type:* field.
	daemonConfigGoPath = "kbeads/internal/server/config.go"
	// schemaFlagLabel is the once-only idempotency marker stamped on an MR after
	// the bot comments. An MR carrying it is never re-flagged.
	schemaFlagLabel = "bridge-schema-flag"
)

// schemaFieldAddPattern matches an added field entry in an init_configs.yaml
// unified diff — a line like `+    - {name: resolved_by, type: string}`. Only
// type:* entries list `{name: …}` items (views and contexts have none), so an
// added one is precisely a type:* field addition. The `^\+` anchors to an
// added line; `+++ b/…` file headers never match because the character after
// the first `+` is another `+`, not whitespace-then-`-`.
var schemaFieldAddPattern = regexp.MustCompile(`^\+\s*-\s*\{\s*name:\s*"?([^",}\s]+)`)

// schemaFlagGitLab is the subset of the GitLab client the bot needs. Kept small
// so the trigger logic is unit-testable against a fake.
type schemaFlagGitLab interface {
	// ListGroupMergeRequests enumerates the group's MRs in a state/window.
	ListGroupMergeRequests(ctx context.Context, groupID int, state string, updatedAfter time.Time) ([]GitLabMR, error)
	// ListMergeRequestChanges returns per-file diffs (with hunk text).
	ListMergeRequestChanges(ctx context.Context, projectPath string, mrIID int) ([]GitLabDiff, error)
	// GetMergeRequestByPath re-fetches one MR to re-verify state + labels.
	GetMergeRequestByPath(ctx context.Context, projectPath string, mrIID int) (*GitLabMR, error)
	// PostMRNote posts the advisory comment.
	PostMRNote(ctx context.Context, projectPath string, mrIID int, body string) (*GitLabNote, error)
	// AddMergeRequestLabels stamps the once-only idempotency label.
	AddMergeRequestLabels(ctx context.Context, projectPath string, mrIID int, labels []string) error
}

// MRSchemaFlagConfig configures the schema-flag MR-comment bot.
type MRSchemaFlagConfig struct {
	GitLab   schemaFlagGitLab
	Logger   *slog.Logger
	GroupID  int
	Interval time.Duration // how often to scan; default 15m
	Window   time.Duration // only consider MRs updated within this window; default 14d
	DryRun   bool          // when true, log intended comments but do not write
}

// MRSchemaFlagReconciler periodically flags open MRs that add a type:* field to
// the bridge's init.go without the matching daemon config.go change.
type MRSchemaFlagReconciler struct {
	gitlab   schemaFlagGitLab
	logger   *slog.Logger
	groupID  int
	interval time.Duration
	window   time.Duration
	dryRun   bool
}

// NewMRSchemaFlagReconciler builds the bot from config, applying defaults.
func NewMRSchemaFlagReconciler(cfg MRSchemaFlagConfig) *MRSchemaFlagReconciler {
	interval := cfg.Interval
	if interval <= 0 {
		interval = 15 * time.Minute
	}
	window := cfg.Window
	if window <= 0 {
		window = 14 * 24 * time.Hour
	}
	return &MRSchemaFlagReconciler{
		gitlab:   cfg.GitLab,
		logger:   cfg.Logger,
		groupID:  cfg.GroupID,
		interval: interval,
		window:   window,
		dryRun:   cfg.DryRun,
	}
}

// schemaFlagStats is the per-scan outcome tally, returned for tests.
type schemaFlagStats struct {
	openMRs       int // open MRs examined
	alreadyFlag   int // skipped: MR already carries the schema-flag label
	noInitChange  int // skipped: MR does not touch init.go
	configTouched int // skipped: MR also touches config.go (correctly paired)
	noFieldAdd    int // skipped: init.go changed but no type:* field added
	flagged       int // MRs commented + labeled this scan
	errors        int // transport / write errors
}

// Run starts the scan loop, scanning immediately then every interval. It blocks
// until ctx is canceled. A zero group ID disables the bot.
func (r *MRSchemaFlagReconciler) Run(ctx context.Context) error {
	if r.groupID == 0 {
		r.logger.Info("MR schema-flag bot disabled (no group id)")
		return nil
	}
	r.logger.Info("starting MR schema-flag bot",
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

// scan performs one pass over the group's recently-updated open MRs.
func (r *MRSchemaFlagReconciler) scan(ctx context.Context) schemaFlagStats {
	since := time.Now().Add(-r.window)
	var s schemaFlagStats

	mrs, err := r.gitlab.ListGroupMergeRequests(ctx, r.groupID, "opened", since)
	if err != nil {
		s.errors++
		r.logger.Error("MR schema-flag scan: list open MRs failed", "error", err)
		return s
	}

	for i := range mrs {
		r.processMR(ctx, &mrs[i], &s)
	}

	r.logger.Info("MR schema-flag scan done",
		"open_mrs", s.openMRs,
		"already_flagged", s.alreadyFlag,
		"no_init_change", s.noInitChange,
		"config_touched", s.configTouched,
		"no_field_add", s.noFieldAdd,
		"flagged", s.flagged,
		"errors", s.errors,
		"dry_run", r.dryRun)
	return s
}

// processMR evaluates one open MR and flags it if it drifts the schema.
func (r *MRSchemaFlagReconciler) processMR(ctx context.Context, mr *GitLabMR, s *schemaFlagStats) {
	s.openMRs++
	if mr.WebURL == "" {
		return
	}
	// Cheap skip: an MR already carrying the label was flagged before.
	if hasLabel(mr.Labels, schemaFlagLabel) {
		s.alreadyFlag++
		return
	}
	ref := ParseMRURL(mr.WebURL)
	if ref == nil {
		s.errors++
		r.logger.Warn("MR schema-flag scan: unparseable MR url", "mr_url", mr.WebURL)
		return
	}

	changes, err := r.gitlab.ListMergeRequestChanges(ctx, ref.ProjectPath, ref.IID)
	if err != nil {
		s.errors++
		r.logger.Warn("MR schema-flag scan: fetch changes failed", "mr_url", mr.WebURL, "error", err)
		return
	}

	added, touchesInit, touchesConfig := evalSchemaDrift(changes)
	if !touchesInit {
		s.noInitChange++
		return
	}
	if touchesConfig {
		// init.go AND config.go both changed — the field was (presumably)
		// registered. This is the correct shape; nothing to warn about.
		s.configTouched++
		return
	}
	if len(added) == 0 {
		// init.go changed but not in a way that adds a type:* field (e.g. a
		// comment or a view-config tweak). No drift risk.
		s.noFieldAdd++
		return
	}

	if r.dryRun {
		s.flagged++
		r.logger.Info("MR schema-flag scan: would flag MR (dry-run)",
			"mr_url", mr.WebURL, "added_fields", added)
		return
	}

	// Re-verify against GitLab immediately before writing: the group listing is a
	// snapshot, so re-fetch to confirm the MR is still open and still unlabeled.
	// This closes the window where a concurrent scan (or a human) labeled it
	// between the listing and now, so the bot can never double-comment.
	fresh, err := r.gitlab.GetMergeRequestByPath(ctx, ref.ProjectPath, ref.IID)
	if err != nil {
		s.errors++
		r.logger.Warn("MR schema-flag scan: re-verify fetch failed — not commenting",
			"mr_url", mr.WebURL, "error", err)
		return
	}
	if fresh.State != "opened" || hasLabel(fresh.Labels, schemaFlagLabel) {
		s.alreadyFlag++
		return
	}

	// Comment first, then label. If the comment fails we do not stamp the label,
	// so a transient failure retries on the next scan instead of silently
	// suppressing the warning forever. If the label write fails after a
	// successful comment, the next scan re-fetches, sees no label, and would
	// re-comment — acceptable and rare (a stale duplicate beats a missed warning).
	body := formatSchemaFlagComment(added)
	if _, err := r.gitlab.PostMRNote(ctx, ref.ProjectPath, ref.IID, body); err != nil {
		s.errors++
		r.logger.Error("MR schema-flag scan: post comment failed", "mr_url", mr.WebURL, "error", err)
		return
	}
	if err := r.gitlab.AddMergeRequestLabels(ctx, ref.ProjectPath, ref.IID, []string{schemaFlagLabel}); err != nil {
		// Comment already landed; log but count as flagged.
		r.logger.Warn("MR schema-flag scan: label write failed (comment posted)",
			"mr_url", mr.WebURL, "error", err)
	}
	s.flagged++
	r.logger.Info("MR schema-flag scan: flagged MR",
		"mr_url", mr.WebURL, "added_fields", added)
}

// evalSchemaDrift inspects an MR's per-file changes and reports (added type:*
// field names in init_configs.yaml, whether it was touched, whether config.go was
// touched). It is pure so the trigger conditions are unit-testable in isolation.
func evalSchemaDrift(changes []GitLabDiff) (added []string, touchesInit, touchesConfig bool) {
	for _, ch := range changes {
		if pathMatches(ch, daemonConfigGoPath) {
			touchesConfig = true
		}
		if pathMatches(ch, bridgeInitConfigsPath) {
			touchesInit = true
			added = append(added, addedSchemaFields(ch.Diff)...)
		}
	}
	return added, touchesInit, touchesConfig
}

// pathMatches reports whether a change touches the given repo-relative path,
// checking both the new and old path so a rename/delete of the file still counts.
func pathMatches(ch GitLabDiff, path string) bool {
	return ch.NewPath == path || ch.OldPath == path
}

// addedSchemaFields returns the names of every type:* FieldDef literal added in
// a unified diff, in order and de-duplicated.
func addedSchemaFields(diff string) []string {
	var out []string
	seen := map[string]bool{}
	for _, line := range strings.Split(diff, "\n") {
		m := schemaFieldAddPattern.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		name := m[1]
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// formatSchemaFlagComment renders the advisory MR comment, naming the added
// field(s) and the four prior incidents so the reviewer knows exactly why and
// how to fix it.
func formatSchemaFlagComment(added []string) string {
	var fieldList string
	switch len(added) {
	case 0:
		fieldList = "a `type:*` field"
	case 1:
		fieldList = fmt.Sprintf("the `%s` field", added[0])
	default:
		quoted := make([]string, len(added))
		for i, f := range added {
			quoted[i] = "`" + f + "`"
		}
		fieldList = "the " + strings.Join(quoted, ", ") + " fields"
	}
	return fmt.Sprintf(
		"⚠ **Schema-drift risk (belt-and-suspenders check, ka-0000007779).**\n\n"+
			"This MR modifies `%s` in a way that adds %s to a `type:*` config, but does "+
			"not touch `%s`.\n\n"+
			"Every field the bridge adds to a `type:*` config in `init_configs.yaml` must also be "+
			"registered in the daemon's `builtinConfigs` — otherwise all typed writes "+
			"carrying that field will **400 in prod** (see ka-0000001549, ka-0000004509, "+
			"ka-0000000811, ka-0000003673). Add the field to `builtinConfigs[\"type:<T>\"]` "+
			"in `config.go`.\n\n"+
			"_If the field is intentionally bridge-only (never written through the daemon), "+
			"disregard — this is an early advisory, not the authoritative CI guard._",
		bridgeInitConfigsPath, fieldList, daemonConfigGoPath)
}
