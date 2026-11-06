// Package purge is the Lands purge sweep (Lands Track B spec §3.3, step
// 8.11): it deletes what the privacy policy (gardenio/docs/lands/legal/
// privacy.md §5) says goes once a land has been closed for the retention
// period, and turned-down applications after theirs.
//
// For each tenant land closed (land_closed_at) longer than Retention, in
// order:
//
//  1. sessions: every object under <prefix>/<agent bead id>/ in the coop
//     session bucket, for each agent bead the daemon's purge will delete
//     (transcripts, terminal recordings, event logs), every version
//     included. Only those ids: an agent bead's coop_session_id and
//     s3_session_key are written by the agent's own pod, so a tenant could
//     point them at an operator agent's session. A field that names a key
//     outside the land is logged and left alone.
//  2. beads: POST /v1/lands/purge hard-deletes every bead labelled
//     project:<land> except the project bead (the tombstone that keeps the
//     name and bead prefix taken) and the credit ledger. The lands-<land>
//     account goes with them; its LiteLLM key was revoked and cleared when
//     the land closed (close.go closeKey), so no per-land secret is left.
//  3. land_purged_at on the project bead.
//
// Once land_closed_at is older than RecordsRetention, the credit ledger goes
// too and land_records_purged_at is stamped. A denied or withdrawn
// plot_application is deleted ApplicationRetention after its decision (or
// its withdrawal). The approved application of a land is its agreement
// record and stays.
//
// LiteLLM's per-request spend rows are not deleted here: LiteLLM has no API
// that deletes a key's rows, so they age out under its global
// maximum_spend_logs_retention_period, which must be no longer than
// Retention (gardenio/docs/lands/legal/README.md "Before publishing").
//
// Every step is idempotent and a failed land is retried on the next pass:
// land_purged_at is stamped only after the sessions and beads are gone. The
// daemon holds every purge to its own retention floors
// (BEADS_LANDS_*_RETENTION_DAYS), so a sweep configured shorter is refused
// before it deletes a session.
//
// With DryRun the sweep deletes and stamps nothing: it logs, for each land,
// records set and application that is due, what would go (beads, agents,
// session objects).
package purge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// Interval is how often the sweep runs. Retention is counted in days, so an
// hour is plenty.
const Interval = time.Hour

// Defaults, in days, from the privacy policy's recommended values
// (gardenio/docs/lands/legal/README.md: RETENTION_DAYS, RECORDS_RETENTION,
// APPLICATION_RETENTION).
const (
	DefaultRetentionDays            = 30
	DefaultRecordsRetentionDays     = 730
	DefaultApplicationRetentionDays = 90
)

// DefaultSessionPrefix is the key prefix coop writes sessions under when
// COOP_S3_PREFIX is unset (deathtranscript.DefaultPrefix).
const DefaultSessionPrefix = "coop/sessions"

// Daemon is the slice of the daemon client the sweep uses, as the
// controller's machine principal holding gardenio-machine-lands-provisioner.
// *beadsapi.Client satisfies it.
type Daemon interface {
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
	UpdateBeadFieldsTyped(ctx context.Context, beadID string, fields map[string]any) error
	PurgeLand(ctx context.Context, req beadsapi.LandPurgeRequest) (*beadsapi.LandPurgeResult, error)
}

// Sessions deletes coop session objects (S3Sessions).
type Sessions interface {
	// DeletePrefix deletes every object, and every version of one, whose
	// key starts with prefix, and returns how many it deleted.
	DeletePrefix(ctx context.Context, prefix string) (int, error)
	// CountPrefix counts what DeletePrefix would delete.
	CountPrefix(ctx context.Context, prefix string) (int, error)
}

// Sweeper is the purge sweep. Daemon is required. A nil Sessions means the
// deployment stores no coop sessions (COOP_S3_BUCKET unset), so there are
// none to delete.
type Sweeper struct {
	Daemon        Daemon
	Sessions      Sessions
	SessionPrefix string

	// Retention is how long after land_closed_at a land's data is kept;
	// RecordsRetention how long its credit ledger is; ApplicationRetention
	// how long a denied or withdrawn application is. Zero means the default.
	Retention            time.Duration
	RecordsRetention     time.Duration
	ApplicationRetention time.Duration

	// DryRun logs what is due and deletes nothing.
	DryRun bool

	Logger *slog.Logger
	// Now defaults to time.Now.
	Now func() time.Time
}

// Days is n days as a duration.
func Days(n int) time.Duration { return time.Duration(n) * 24 * time.Hour }

// Run sweeps every interval until ctx is cancelled, with one pass on start.
func (s *Sweeper) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = Interval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := s.Pass(ctx); err != nil {
			s.logger().Warn("lands purge: pass failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Pass purges every land and application that is due. One land's failure
// never stops the others; it is logged and retried on the next pass. The
// error is the first one met, or a read that stopped the pass.
func (s *Sweeper) Pass(ctx context.Context) error {
	if s.Retention > 0 && s.RecordsRetention > 0 && s.RecordsRetention < s.Retention {
		return fmt.Errorf("records retention (%s) is shorter than retention (%s)", s.RecordsRetention, s.Retention)
	}
	var errs []error
	lands, err := listAll(ctx, s.Daemon, beadsapi.ListBeadsQuery{
		Types: []string{"project"}, Statuses: []string{"closed"}, Fields: map[string]string{"land_kind": "tenant"},
	})
	if err != nil {
		return fmt.Errorf("reading closed lands: %w", err)
	}
	now := s.now()
	for _, p := range lands {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := s.land(ctx, p, now); err != nil {
			s.logger().Error("lands purge: land failed; retried next pass", "land", landName(p), "error", err)
			errs = append(errs, err)
		}
	}
	if err := s.applications(ctx, now); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// land purges p's data or its records when they are due.
func (s *Sweeper) land(ctx context.Context, p *beadsapi.BeadDetail, now time.Time) error {
	if p.Type != "project" || p.Status != "closed" || p.Fields["land_kind"] != "tenant" {
		return nil
	}
	name := landName(p)
	closedAt, err := time.Parse(time.RFC3339, strings.TrimSpace(p.Fields["land_closed_at"]))
	if err != nil {
		// A land closed by hand rather than by gb lands close: the daemon
		// refuses it too, so say so once a pass rather than fail.
		s.logger().Warn("lands purge: closed tenant land has no land_closed_at; skipped", "land", name, "project", p.ID)
		return nil
	}
	switch {
	case p.Fields["land_purged_at"] == "":
		if now.Before(closedAt.Add(s.retention())) {
			return nil
		}
		return s.purgeLand(ctx, p, name, now)
	case p.Fields["land_records_purged_at"] == "":
		if now.Before(closedAt.Add(s.recordsRetention())) {
			return nil
		}
		return s.purgeRecords(ctx, p, name, now)
	}
	return nil
}

func (s *Sweeper) purgeLand(ctx context.Context, p *beadsapi.BeadDetail, land string, now time.Time) error {
	log := s.logger().With("land", land, "project", p.ID)
	plan, err := s.Daemon.PurgeLand(ctx, beadsapi.LandPurgeRequest{Land: land, DryRun: true})
	if err != nil {
		return err
	}
	keys, err := s.sessionKeys(ctx, land, plan)
	if err != nil {
		return err
	}
	objects := 0
	if s.Sessions != nil {
		for _, key := range keys {
			prefix := s.sessionPrefix() + "/" + key + "/"
			var n int
			if s.DryRun {
				n, err = s.Sessions.CountPrefix(ctx, prefix)
			} else {
				n, err = s.Sessions.DeletePrefix(ctx, prefix)
			}
			if err != nil {
				return fmt.Errorf("the sessions of %s: %w", key, err)
			}
			objects += n
		}
	}
	if s.DryRun {
		log.Info("lands purge: dry run: land due", "beads", len(plan.Purged), "agents", len(keys), "session_objects", objects)
		return nil
	}
	res, err := s.Daemon.PurgeLand(ctx, beadsapi.LandPurgeRequest{Land: land})
	if err != nil {
		return err
	}
	if err := s.Daemon.UpdateBeadFieldsTyped(ctx, p.ID, map[string]any{"land_purged_at": now.UTC().Format(time.RFC3339)}); err != nil {
		return fmt.Errorf("stamping land_purged_at: %w", err)
	}
	log.Info("lands purge: land purged", "beads", len(res.Purged), "agents", len(keys), "session_objects", objects)
	return nil
}

func (s *Sweeper) purgeRecords(ctx context.Context, p *beadsapi.BeadDetail, land string, now time.Time) error {
	res, err := s.Daemon.PurgeLand(ctx, beadsapi.LandPurgeRequest{Land: land, IncludeRecords: true, DryRun: s.DryRun})
	if err != nil {
		return err
	}
	if s.DryRun {
		s.logger().Info("lands purge: dry run: land records due", "land", land, "project", p.ID, "beads", len(res.Purged))
		return nil
	}
	if err := s.Daemon.UpdateBeadFieldsTyped(ctx, p.ID, map[string]any{"land_records_purged_at": now.UTC().Format(time.RFC3339)}); err != nil {
		return fmt.Errorf("stamping land_records_purged_at: %w", err)
	}
	s.logger().Info("lands purge: land records purged", "land", land, "project", p.ID, "beads", len(res.Purged))
	return nil
}

// sessionKeyPattern is a key the sweep will build a delete prefix from: one
// path segment, never empty, never a wildcard.
var sessionKeyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// sessionKeyFields are the agent bead fields that repoint its session
// (deathtranscript.sessionKeyFields).
var sessionKeyFields = []string{"coop_session_id", "s3_session_key"}

// sessionKeys are the session keys of the land's agents: the id of each agent
// bead in the purge plan, soft-deleted ones included. The daemon chose those
// beads by the land's label and will delete them, so their sessions are the
// land's. Nothing a bead says about itself adds a key: coop_session_id and
// s3_session_key are written by the agent's pod (gb init adopting a resumed
// chain), so a tenant could name another land's or an operator's agent
// there. A respawn chain's key is an earlier agent of the same land, which is
// already in the plan; a field naming any other key is logged for an
// operator and not deleted. An id that is not one plain path segment is
// skipped and logged, never deleted by: the sweep never deletes a prefix it
// did not mean, and one odd id must not hold back the rest of the land.
func (s *Sweeper) sessionKeys(ctx context.Context, land string, plan *beadsapi.LandPurgeResult) ([]string, error) {
	var keys []string
	own := map[string]bool{}
	for _, id := range plan.Purged {
		if plan.Types[id] != "agent" {
			continue
		}
		if !sessionKeyPattern.MatchString(id) {
			s.logger().Error("lands purge: agent bead id is not a single path segment; its sessions are not deleted",
				"land", land, "agent", id)
			continue
		}
		own[id] = true
		keys = append(keys, id)
	}
	sort.Strings(keys)
	agents, err := listAll(ctx, s.Daemon, beadsapi.ListBeadsQuery{
		Types: []string{"agent"}, Labels: []string{"project:" + land}, Statuses: allStatuses,
	})
	if err != nil {
		return nil, fmt.Errorf("reading %s's agents: %w", land, err)
	}
	for _, a := range agents {
		for _, f := range sessionKeyFields {
			if v := strings.TrimSpace(a.Fields[f]); v != "" && !own[v] {
				s.logger().Warn("lands purge: agent names a session key outside the land; not deleted",
					"land", land, "agent", a.ID, "field", f, "session_key", v)
			}
		}
	}
	return keys, nil
}

// Application states the sweep deletes (kbeads plot_application.go).
var purgeableApplicationStates = []string{"denied", "withdrawn"}

// applications deletes every denied or withdrawn application older than the
// application retention period.
func (s *Sweeper) applications(ctx context.Context, now time.Time) error {
	var errs []error
	for _, state := range purgeableApplicationStates {
		apps, err := listAll(ctx, s.Daemon, beadsapi.ListBeadsQuery{
			Types: []string{"plot_application"}, Labels: []string{"project:lands-intake"},
			Fields: map[string]string{"state": state}, Statuses: allStatuses,
		})
		if err != nil {
			return fmt.Errorf("reading %s applications: %w", state, err)
		}
		for _, a := range apps {
			if a.Type != "plot_application" || a.Fields["state"] != state {
				continue
			}
			at, ok := decidedAt(a)
			if !ok || now.Before(at.Add(s.applicationRetention())) {
				continue
			}
			if _, err := s.Daemon.PurgeLand(ctx, beadsapi.LandPurgeRequest{ApplicationID: a.ID, DryRun: s.DryRun}); err != nil {
				s.logger().Error("lands purge: application failed; retried next pass", "application", a.ID, "error", err)
				errs = append(errs, err)
				continue
			}
			if s.DryRun {
				s.logger().Info("lands purge: dry run: application due", "application", a.ID, "state", state)
				continue
			}
			s.logger().Info("lands purge: application purged", "application", a.ID, "state", state)
		}
	}
	return errors.Join(errs...)
}

// decidedAt is when an application was turned down or withdrawn: decided_at,
// else when it was closed, else its last update.
func decidedAt(b *beadsapi.BeadDetail) (time.Time, bool) {
	for _, v := range []string{b.Fields["decided_at"], b.ClosedAt} {
		if t, err := time.Parse(time.RFC3339, strings.TrimSpace(v)); err == nil {
			return t, true
		}
	}
	return b.UpdatedAt, !b.UpdatedAt.IsZero()
}

// landName is a project bead's name: its title without "Project: ".
func landName(b *beadsapi.BeadDetail) string {
	return strings.TrimSpace(strings.TrimPrefix(b.Title, "Project: "))
}

var allStatuses = []string{"open", "in_progress", "blocked", "deferred", "closed"}

const (
	pageSize = 200
	maxPages = 50
)

// listAll pages a query to the end; past maxPages it is an error, never a
// silent cut.
func listAll(ctx context.Context, d Daemon, q beadsapi.ListBeadsQuery) ([]*beadsapi.BeadDetail, error) {
	q.Limit, q.Sort = pageSize, "created_at"
	var out []*beadsapi.BeadDetail
	seen := map[string]bool{}
	for page := range maxPages {
		q.Offset = page * pageSize
		res, err := d.ListBeadsFiltered(ctx, q)
		if err != nil {
			return nil, err
		}
		if res == nil {
			return out, nil
		}
		for _, b := range res.Beads {
			if b != nil && !seen[b.ID] {
				seen[b.ID] = true
				out = append(out, b)
			}
		}
		if len(res.Beads) < pageSize {
			return out, nil
		}
	}
	return nil, fmt.Errorf("more than %d %v beads; refusing a partial read", maxPages*pageSize, q.Types)
}

func (s *Sweeper) retention() time.Duration {
	if s.Retention > 0 {
		return s.Retention
	}
	return Days(DefaultRetentionDays)
}

func (s *Sweeper) recordsRetention() time.Duration {
	if s.RecordsRetention > 0 {
		return s.RecordsRetention
	}
	return Days(DefaultRecordsRetentionDays)
}

func (s *Sweeper) applicationRetention() time.Duration {
	if s.ApplicationRetention > 0 {
		return s.ApplicationRetention
	}
	return Days(DefaultApplicationRetentionDays)
}

func (s *Sweeper) sessionPrefix() string {
	if p := strings.Trim(strings.TrimSpace(s.SessionPrefix), "/"); p != "" {
		return p
	}
	return DefaultSessionPrefix
}

func (s *Sweeper) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

func (s *Sweeper) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}
