// Package postgres implements the store.Store interface backed by PostgreSQL.
package postgres

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/lib/pq"

	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/store"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// PostgresStore implements store.Store backed by a PostgreSQL database.
type PostgresStore struct {
	db *sql.DB
	// extractCommentRefs gates ka-id reference extraction from comment
	// bodies. Default false so deployments opt in once the comment-text
	// inclusion decision (ka-0000000868) lands. When false, AddComment
	// skips replaceCommentReferences and the reextract-references backfill
	// skips the per-bead comment iteration. When toggled on, the operator
	// should run `kd admin reextract-references` to backfill existing
	// comments (otherwise comment refs only appear for newly-added comments).
	extractCommentRefs bool
	// reconcileTypedMentionDeps gates auto-deletion of stale parser-created
	// parent-child dep rows on the write path. Default false (opt-in via
	// BEADS_REFERENCES_RECONCILE_DEPS). When true, an edit that removes a
	// `parent:`/`child-of:` typed mention also removes the parent-child dep
	// row the parser had materialized. Manual `kd dep add` rows are never
	// touched. See reconcileTypedMentionDeps() and
	// kbeads/docs/design-reconcile-typed-mention-deps.md (Proposal B).
	reconcileTypedMentionDeps bool
}

// Compile-time check that PostgresStore implements store.Store.
var _ store.Store = (*PostgresStore)(nil)

// New opens a connection to the PostgreSQL database at the given URL,
// configures the connection pool, and runs any pending migrations.
// Used by the //go:build integration tests in this package.
func New(databaseURL string) (*PostgresStore, error) {
	return NewWithPasswordFile(databaseURL, "")
}

// NewWithPasswordFile is New with rotation-tolerant credentials: when
// passwordFile is non-empty the password is read from that file per DB
// connection and injected into databaseURL in-process, so a rotated
// (e.g. RDS master-password) credential mounted as a live-updating K8s
// Secret volume is picked up WITHOUT a pod restart. Empty passwordFile
// falls back to the legacy env-var-substituted DSN (no behavior change).
// See bug ka-0000004150 (fix #2 of P0 postmortem ka-0000006811).
func NewWithPasswordFile(databaseURL, passwordFile string) (*PostgresStore, error) {
	db, err := openDB(databaseURL, passwordFile)
	if err != nil {
		return nil, err
	}

	// Keep the pool WARM. The daemon serves the whole fleet, so request
	// volume is bursty: a spike opens up to MaxOpenConns physical connections,
	// and every connection returned to the pool ABOVE MaxIdleConns is closed
	// immediately, not parked. With MaxIdleConns=5 (the previous value) any
	// burst wider than 5 destroyed the excess connections, so the NEXT burst
	// had to re-dial them cold. A cold dial to RDS (TCP + TLS + the per-Connect
	// password-file read in passwordFileConnector) is orders of magnitude
	// slower than reusing a warm connection — measured as a bimodal ~4.08s tail
	// on ~13% of otherwise-trivial requests, with some queuing to 503
	// (bug ka-0000004428). Setting MaxIdleConns == MaxOpenConns means a
	// returned connection is never destroyed for being "excess idle", so a
	// steady-state working set of warm connections survives between bursts and
	// the cold-dial tail disappears. This does NOT raise the connection ceiling
	// — MaxOpenConns=25 still caps concurrent physical connections — it only
	// stops the pool from throwing warm connections away.
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(25)
	// ConnMaxLifetime is the rotation-tolerance knob: every physical connection
	// is recycled within 5m regardless of use, so a rotated RDS password (read
	// per-Connect from the mounted Secret file) is picked up within one lifetime
	// window WITHOUT a pod restart (see passwordFileConnector). Leave it at 5m.
	db.SetConnMaxLifetime(5 * time.Minute)
	// Do NOT idle-reap on a short (30s) timer — that reintroduced the same cold-
	// dial churn the warm pool above is meant to eliminate, because a brief lull
	// between bursts would reap the working set and force the next burst cold.
	// ConnMaxLifetime (5m) is the sole reaper, so an idle connection still dies
	// within 5m and the pool naturally drains to its working set during a
	// genuinely quiet period; rotation tolerance is unchanged.
	db.SetConnMaxIdleTime(0)

	// In password-file (rotation-tolerant) mode, be more patient at
	// startup: each ping re-dials through the connector, which re-reads
	// the file, so a boot-time skew self-heals once the mounted Secret
	// syncs the rotated password (~one kubelet sync period) instead of
	// exiting after 5. In env mode, preserve the original retry budget.
	maxRetries := 5
	if passwordFile != "" {
		maxRetries = 12
	}
	if err := pingWithRetry(db, maxRetries, time.Second, 30*time.Second); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	if err := runMigrations(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("run migrations: %w", err)
	}

	return &PostgresStore{db: db}, nil
}

// openDB opens the *sql.DB backing a PostgresStore. When passwordFile is set
// it returns a DB backed by a passwordFileConnector that re-reads the password
// on every physical connection (rotation-tolerant); otherwise it falls back to
// the plain env-var-substituted DSN via lib/pq's own connector. Either way the
// connector is wrapped in instrumentedConnector, which records per-store-method
// query metrics (gd-rXqkv4rSmG).
func openDB(databaseURL, passwordFile string) (*sql.DB, error) {
	if passwordFile != "" {
		connector, err := newPasswordFileConnector(databaseURL, passwordFile)
		if err != nil {
			return nil, fmt.Errorf("build password-file connector: %w", err)
		}
		return sql.OpenDB(instrumentedConnector{connector}), nil
	}
	connector, err := pq.NewConnector(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	return sql.OpenDB(instrumentedConnector{connector}), nil
}

// SetExtractCommentReferences toggles ka-id reference extraction from
// comment bodies (off by default). See PostgresStore.extractCommentRefs.
func (s *PostgresStore) SetExtractCommentReferences(enabled bool) {
	s.extractCommentRefs = enabled
}

// SetReconcileTypedMentionDeps toggles auto-deletion of stale parser-created
// parent-child dep rows on the write path (off by default). See
// PostgresStore.reconcileTypedMentionDeps and Proposal B
// (kbeads/docs/design-reconcile-typed-mention-deps.md).
func (s *PostgresStore) SetReconcileTypedMentionDeps(enabled bool) {
	s.reconcileTypedMentionDeps = enabled
}

// MigrateWithPasswordFile opens a connection to databaseURL, applies any
// pending schema migrations to completion, and closes the connection.
// Intended for use by `kd migrate` (the standalone migration runner) and by
// tests that need to materialize the schema without standing up a full
// PostgresStore.
//
// Honors BEADS_MIGRATE_AUTO_RECOVER=last-clean for dirty-state recovery,
// matching the daemon-startup migration path. The intent is that a
// pre-upgrade Helm Job runs this once per release transition (no startup
// probe pressure), and the daemon then verifies version on boot rather
// than re-running migrations under a 60s probe deadline. See ka-0000005716
// for the architectural plan.
//
// Credentials are as rotation-tolerant as NewWithPasswordFile's: when
// passwordFile is non-empty the password is read from that file per
// connection so the migrate Job survives a mid-rotation credential skew
// instead of failing on pq 28P01. Empty passwordFile preserves the legacy
// env-var-substituted DSN path.
func MigrateWithPasswordFile(databaseURL, passwordFile string) error {
	db, err := openDB(databaseURL, passwordFile)
	if err != nil {
		return err
	}
	defer db.Close()

	maxRetries := 5
	if passwordFile != "" {
		maxRetries = 12
	}
	if err := pingWithRetry(db, maxRetries, time.Second, 30*time.Second); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}

	return runMigrations(db)
}

// pinger abstracts a database ping for testability.
type pinger interface {
	Ping() error
}

// pingWithRetry pings the database with exponential backoff. It retries up to
// maxRetries times, starting with initialDelay and doubling each attempt up to
// maxDelay. This prevents fatal crashes on transient connection resets during
// startup (e.g. "connection reset by peer").
func pingWithRetry(p pinger, maxRetries int, initialDelay, maxDelay time.Duration) error {
	var err error
	delay := initialDelay
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if err = p.Ping(); err == nil {
			return nil
		}
		if attempt < maxRetries {
			slog.Warn("database ping failed, retrying",
				"attempt", attempt+1,
				"max_retries", maxRetries,
				"delay", delay,
				"err", err,
			)
			time.Sleep(delay)
			delay *= 2
			if delay > maxDelay {
				delay = maxDelay
			}
		}
	}
	return err
}

func runMigrations(db *sql.DB) error {
	sourceDriver, err := iofs.New(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("create migration source: %w", err)
	}

	dbDriver, err := postgres.WithInstance(db, &postgres.Config{})
	if err != nil {
		return fmt.Errorf("create migration db driver: %w", err)
	}

	m, err := migrate.NewWithInstance("iofs", sourceDriver, "postgres", dbDriver)
	if err != nil {
		return fmt.Errorf("create migrator: %w", err)
	}

	// Preflight pending migrations for CREATE EXTENSION directives — fail
	// early with a specific error instead of letting golang-migrate leave
	// schema_migrations dirty (ka-0000005888).
	currentVersion := 0
	if v, _, vErr := m.Version(); vErr == nil {
		currentVersion = int(v)
	} else if !errors.Is(vErr, migrate.ErrNilVersion) {
		return fmt.Errorf("get current migration version: %w", vErr)
	}
	if err := preflightExtensions(db, migrationsFS, currentVersion); err != nil {
		return err
	}

	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return handleMigrateError(m, err)
	}

	return nil
}

// handleMigrateError enriches a failing m.Up() call. For the specific case of
// ErrDirty (schema_migrations.dirty=true, which golang-migrate leaves behind
// after any interrupted migration) it logs an actionable message with the
// exact recovery command. If BEADS_MIGRATE_AUTO_RECOVER=last-clean is set,
// it force-clears the dirty flag to version-1 and retries the migration.
//
// Default behavior stays fail-fast — a dirty DB is a real signal and silent
// recovery can mask genuine bugs. The auto-recovery is opt-in so the operator
// explicitly acknowledges the trade-off. See ka-0000003541 for the incident
// motivation (2026-04-22: 21h silent CrashLoopBackOff on dirty flag).
func handleMigrateError(m *migrate.Migrate, err error) error {
	var dirty migrate.ErrDirty
	if !errors.As(err, &dirty) {
		return fmt.Errorf("apply migrations: %w", err)
	}

	lastClean := dirty.Version - 1
	slog.Error(
		"schema_migrations is in a dirty state — a previous migration was interrupted mid-apply",
		"dirty_version", dirty.Version,
		"last_clean_version", lastClean,
		"recovery_manual", fmt.Sprintf(
			"psql -c 'UPDATE schema_migrations SET dirty = false, version = %d'",
			lastClean,
		),
		"recovery_auto_env", "BEADS_MIGRATE_AUTO_RECOVER=last-clean",
	)

	if os.Getenv("BEADS_MIGRATE_AUTO_RECOVER") != "last-clean" {
		return fmt.Errorf(
			"apply migrations: %w — re-run with BEADS_MIGRATE_AUTO_RECOVER=last-clean to auto-clean to version %d, or fix manually",
			err, lastClean,
		)
	}

	slog.Warn(
		"auto-recover enabled — forcing schema_migrations to last-clean version",
		"from_version", dirty.Version,
		"to_version", lastClean,
	)
	if fErr := m.Force(lastClean); fErr != nil {
		return fmt.Errorf("auto-recover: force version %d: %w (original error: %v)", lastClean, fErr, err)
	}
	if upErr := m.Up(); upErr != nil && upErr != migrate.ErrNoChange {
		return fmt.Errorf("auto-recover: retry migrations after force: %w", upErr)
	}
	slog.Info("auto-recover succeeded — migrations applied after force-clean")
	return nil
}

// BackfillFleet updates any rows still using the 'default' fleet value
// to the given fleet name. This is called at startup when BEADS_DEFAULT_FLEET
// is set to a value other than 'default', ensuring existing data is assigned
// to the correct fleet.
func (s *PostgresStore) BackfillFleet(ctx context.Context, fleet string) (int64, error) {
	if fleet == "" || fleet == "default" {
		return 0, nil
	}
	var total int64
	for _, table := range []string{"beads", "labels", "deps", "comments", "events", "configs"} {
		res, err := s.db.ExecContext(ctx, "UPDATE "+table+" SET fleet = $1 WHERE fleet = 'default'", fleet)
		if err != nil {
			return total, fmt.Errorf("backfill %s: %w", table, err)
		}
		n, _ := res.RowsAffected()
		total += n
	}
	return total, nil
}

// Ping verifies the database connection is alive.
func (s *PostgresStore) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

// Close closes the underlying database connection.
func (s *PostgresStore) Close() error {
	return s.db.Close()
}

// DBStats returns the live connection-pool statistics of the underlying
// *sql.DB (sql.DB.Stats). It is the observability hook behind the
// kbeads_db_pool_* metrics: MaxIdleClosed / WaitCount in particular are the
// direct signals for the cold-dial churn that drove bug ka-0000004428, so a
// flat MaxIdleClosed after the warm-pool fix is how that fix is verified in
// prod. Reading Stats() is a cheap in-memory snapshot (no DB round-trip), so
// it is safe to call on a metrics-scrape path.
func (s *PostgresStore) DBStats() sql.DBStats {
	return s.db.Stats()
}

func (s *PostgresStore) CreateBead(ctx context.Context, bead *model.Bead) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit
	if err := queryCreateBead(ctx, tx, bead); err != nil {
		return err
	}
	if err := replaceBeadReferences(ctx, tx, bead); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *PostgresStore) GetBead(ctx context.Context, id string) (*model.Bead, error) {
	return queryGetBead(ctx, s.db, id)
}

func (s *PostgresStore) GetBeadIncludingDeleted(ctx context.Context, id string) (*model.Bead, error) {
	return queryGetBeadIncludingDeleted(ctx, s.db, id)
}

func (s *PostgresStore) FindAgentByName(ctx context.Context, name string, statuses []model.Status) (*model.Bead, error) {
	return queryFindAgentByName(ctx, s.db, name, statuses)
}

func (s *PostgresStore) FindAgentsByNames(ctx context.Context, names []string, statuses []model.Status) ([]*model.Bead, error) {
	return queryFindAgentsByNames(ctx, s.db, names, statuses)
}

func (s *PostgresStore) FindProjectByName(ctx context.Context, name string) (*model.Bead, error) {
	return queryFindProjectByName(ctx, s.db, name)
}

func (s *PostgresStore) ListBeads(ctx context.Context, filter model.BeadFilter) ([]*model.Bead, int, error) {
	return queryListBeads(ctx, s.db, filter)
}

func (s *PostgresStore) FillShortTitle(ctx context.Context, bead *model.Bead) (bool, error) {
	return queryFillShortTitle(ctx, s.db, bead)
}

func (s *PostgresStore) LockBeadAssignee(ctx context.Context, id string) (string, error) {
	return queryLockBeadAssignee(ctx, s.db, id)
}

func (s *PostgresStore) UpdateBead(ctx context.Context, bead *model.Bead) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit
	if err := queryUpdateBead(ctx, tx, bead); err != nil {
		return err
	}
	if err := replaceBeadReferences(ctx, tx, bead); err != nil {
		return err
	}
	if s.reconcileTypedMentionDeps {
		if err := reconcileTypedMentionDeps(ctx, tx, bead, s.extractCommentRefs); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *PostgresStore) MergeBeadFields(ctx context.Context, id string, extra map[string]any) (*model.Bead, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit
	bead, err := queryMergeBeadFields(ctx, tx, id, extra)
	if err != nil {
		return nil, err
	}
	if err := replaceBeadReferences(ctx, tx, bead); err != nil {
		return nil, err
	}
	if s.reconcileTypedMentionDeps {
		if err := reconcileTypedMentionDeps(ctx, tx, bead, s.extractCommentRefs); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit tx: %w", err)
	}
	return bead, nil
}

func (s *PostgresStore) IncrementBeadFields(ctx context.Context, id string, deltas map[string]float64) (*model.Bead, error) {
	return queryIncrementBeadFields(ctx, s.db, id, deltas)
}

// AllocateAgentHandleSeq atomically returns the next per-gardener sequence number
// for monotonic agent_handle naming (ka-0000002870). See
// queryAllocateAgentHandleSeq for the race-free upsert and the never-resets
// rationale.
func (s *PostgresStore) AllocateAgentHandleSeq(ctx context.Context, gardenerID string) (int64, error) {
	return queryAllocateAgentHandleSeq(ctx, s.db, gardenerID)
}

func (s *PostgresStore) AppendBeadArrayField(ctx context.Context, id, field, element string) (*model.Bead, bool, error) {
	return queryAppendBeadArrayField(ctx, s.db, id, field, element)
}

// InsertBeadArrayField opens an explicit transaction so the positional insert's
// read-modify-write (queryInsertBeadArrayField) holds its `FOR UPDATE` row lock
// across both statements — mirroring MergeBeadMapField's entry point.
func (s *PostgresStore) InsertBeadArrayField(ctx context.Context, id, field, element string, index int) (*model.Bead, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after successful commit
	bead, changed, err := queryInsertBeadArrayField(ctx, tx, id, field, element, index)
	if err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("commit: %w", err)
	}
	return bead, changed, nil
}

func (s *PostgresStore) CASBeadArrayField(ctx context.Context, id, field string, expected, next []string) (*model.Bead, error) {
	return queryCASBeadArrayField(ctx, s.db, id, field, expected, next)
}

func (s *PostgresStore) CloseBead(ctx context.Context, id string, closedBy string) (*model.Bead, error) {
	return queryCloseBead(ctx, s.db, id, closedBy)
}

func (s *PostgresStore) DeleteBead(ctx context.Context, id string) error {
	return queryDeleteBead(ctx, s.db, id)
}

func (s *PostgresStore) RestoreBead(ctx context.Context, id string) (*model.Bead, error) {
	return queryRestoreBead(ctx, s.db, id)
}

func (s *PostgresStore) TransferBead(ctx context.Context, id string, toFleet string) (*model.Bead, string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, "", fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after successful commit
	bead, fromFleet, err := queryTransferBead(ctx, tx, id, toFleet)
	if err != nil {
		return nil, "", err
	}
	if err := tx.Commit(); err != nil {
		return nil, "", fmt.Errorf("commit: %w", err)
	}
	return bead, fromFleet, nil
}

func (s *PostgresStore) GetStats(ctx context.Context) (*model.GraphStats, error) {
	return queryGetStats(ctx, s.db)
}

func (s *PostgresStore) FleetTypeStatusCounts(ctx context.Context) (map[store.FleetTypeStatusKey]int, error) {
	return queryFleetTypeStatusCounts(ctx, s.db)
}

func (s *PostgresStore) FleetActiveAgentsByProject(ctx context.Context) (map[string]int, error) {
	return queryFleetActiveAgentsByProject(ctx, s.db)
}

func (s *PostgresStore) FleetAgentLifetimeRollup(ctx context.Context) ([]store.FleetAgentProjectRollup, error) {
	return queryFleetAgentLifetimeRollup(ctx, s.db)
}

func (s *PostgresStore) FleetQueueDepthByProject(ctx context.Context) (map[store.FleetQueueDepthKey]int, error) {
	return queryFleetQueueDepthByProject(ctx, s.db, store.FleetQueueTypes)
}

func (s *PostgresStore) FleetQueueOldestAgeByProject(ctx context.Context) (map[store.FleetQueueDepthKey]float64, error) {
	return queryFleetQueueOldestAgeByProject(ctx, s.db, store.FleetQueueTypes)
}

func (s *PostgresStore) ProjectLabelMissingCounts(ctx context.Context, projectTypes []string) (map[string]int, error) {
	return queryProjectLabelMissingCounts(ctx, s.db, projectTypes)
}

func (s *PostgresStore) AcceptanceMetrics(ctx context.Context) (*store.AcceptanceMetrics, error) {
	return queryAcceptanceMetrics(ctx, s.db)
}

func (s *PostgresStore) GetDependenciesForBeads(ctx context.Context, beadIDs []string) (map[string][]*model.Dependency, error) {
	return queryGetDependenciesForBeads(ctx, s.db, beadIDs)
}

func (s *PostgresStore) GetReverseDependenciesForBeads(ctx context.Context, beadIDs []string) (map[string][]*model.Dependency, error) {
	return queryGetReverseDependenciesForBeads(ctx, s.db, beadIDs)
}

func (s *PostgresStore) GetDependencyCounts(ctx context.Context, beadIDs []string) (map[string]*model.DependencyCounts, error) {
	return queryGetDependencyCounts(ctx, s.db, beadIDs)
}

func (s *PostgresStore) GetChildrenStatusHistogram(ctx context.Context, parentIDs []string) (map[string]map[string]int, error) {
	return queryGetChildrenStatusHistogram(ctx, s.db, parentIDs)
}

func (s *PostgresStore) GetLabelsForBeads(ctx context.Context, beadIDs []string) (map[string][]string, error) {
	return queryGetLabelsForBeads(ctx, s.db, beadIDs)
}

func (s *PostgresStore) GetBlockedByForBeads(ctx context.Context, beadIDs []string) (map[string][]string, error) {
	return queryGetBlockedByForBeads(ctx, s.db, beadIDs)
}

func (s *PostgresStore) GetBeadsByIDs(ctx context.Context, ids []string) ([]*model.Bead, error) {
	return queryGetBeadsByIDs(ctx, s.db, ids)
}

func (s *PostgresStore) GetTextReferencesForBeads(ctx context.Context, beadIDs []string) ([]*model.BeadReference, error) {
	return queryGetTextReferencesForBeads(ctx, s.db, beadIDs)
}

func (s *PostgresStore) AddDependency(ctx context.Context, dep *model.Dependency) error {
	return queryAddDependency(ctx, s.db, dep)
}

func (s *PostgresStore) RepairReversedParentChildDeps(ctx context.Context, opts store.RepairReversedParentChildDepsOptions) (*store.RepairReversedParentChildDepsStats, error) {
	return queryRepairReversedParentChildDeps(ctx, s.db, opts)
}

func (s *PostgresStore) RemoveDependency(ctx context.Context, beadID, dependsOnID string, depType model.DependencyType) (bool, error) {
	n, err := queryRemoveDependency(ctx, s.db, beadID, dependsOnID, depType)
	return n > 0, err
}

func (s *PostgresStore) GetDependencies(ctx context.Context, beadID string) ([]*model.Dependency, error) {
	return queryGetDependencies(ctx, s.db, beadID)
}

func (s *PostgresStore) GetReverseDependencies(ctx context.Context, beadID string) ([]*model.Dependency, error) {
	return queryGetReverseDependencies(ctx, s.db, beadID)
}

func (s *PostgresStore) GetDependencyEdgesByType(ctx context.Context, depType model.DependencyType) ([]model.DependencyEdge, error) {
	return queryGetDependencyEdgesByType(ctx, s.db, depType)
}

func (s *PostgresStore) ListBeadsMentioning(ctx context.Context, targetID string, limit, offset int, relationshipType *string) ([]*model.BeadReferenceSummary, int, error) {
	return queryListBeadsMentioning(ctx, s.db, targetID, limit, offset, relationshipType)
}

func (s *PostgresStore) HasBundleChild(ctx context.Context, parentID string) (bool, error) {
	return queryHasBundleChild(ctx, s.db, parentID)
}

func (s *PostgresStore) ListBeadReferencesFrom(ctx context.Context, sourceID string, limit, offset int, relationshipType *string) ([]*model.BeadReferenceSummary, int, error) {
	return queryListBeadReferencesFrom(ctx, s.db, sourceID, limit, offset, relationshipType)
}

// AddLabel adds one label. A project: label also rewrites beads.project
// (migration 057), so the label and the column move in one transaction.
func (s *PostgresStore) AddLabel(ctx context.Context, beadID string, label string) error {
	if !isProjectLabel(label) {
		return queryAddLabel(ctx, s.db, beadID, label)
	}
	return s.RunInTransaction(ctx, func(tx store.Store) error {
		return tx.AddLabel(ctx, beadID, label)
	})
}

// RemoveLabel removes one label; a project: label moves beads.project with
// it, in one transaction, as AddLabel does.
func (s *PostgresStore) RemoveLabel(ctx context.Context, beadID string, label string) (bool, error) {
	if !isProjectLabel(label) {
		n, err := queryRemoveLabel(ctx, s.db, beadID, label)
		return n > 0, err
	}
	var removed bool
	err := s.RunInTransaction(ctx, func(tx store.Store) error {
		var err error
		removed, err = tx.RemoveLabel(ctx, beadID, label)
		return err
	})
	return removed, err
}

func (s *PostgresStore) GetLabels(ctx context.Context, beadID string) ([]string, error) {
	return queryGetLabels(ctx, s.db, beadID)
}

func (s *PostgresStore) GetTaintsForBead(ctx context.Context, beadID string) ([]int64, error) {
	return queryGetTaintsForBead(ctx, s.db, beadID)
}

func (s *PostgresStore) GetTaintsForBeads(ctx context.Context, beadIDs []string) (map[string][]int64, error) {
	return queryGetTaintsForBeads(ctx, s.db, beadIDs)
}

func (s *PostgresStore) GetAllTaintsForBead(ctx context.Context, beadID string) ([]int64, error) {
	return queryGetAllTaintsForBead(ctx, s.db, beadID)
}

func (s *PostgresStore) GetAllTaintsForBeads(ctx context.Context, beadIDs []string) (map[string][]int64, error) {
	return queryGetAllTaintsForBeads(ctx, s.db, beadIDs)
}

func (s *PostgresStore) GetGrantedTaintIDs(ctx context.Context, principalKind string, principals []string) ([]int64, error) {
	return queryGetGrantedTaintIDs(ctx, s.db, principalKind, principals)
}

func (s *PostgresStore) GetTaintIDsOwnedByTeams(ctx context.Context, teams []string) ([]int64, error) {
	return queryGetTaintIDsOwnedByTeams(ctx, s.db, teams)
}

func (s *PostgresStore) HasAnyTaints(ctx context.Context) (bool, error) {
	return queryHasAnyTaints(ctx, s.db)
}

func (s *PostgresStore) SetTaint(ctx context.Context, t *model.Taint) error {
	return querySetTaint(ctx, s.db, t)
}

func (s *PostgresStore) GetTaint(ctx context.Context, name string) (*model.Taint, error) {
	return queryGetTaint(ctx, s.db, name)
}

func (s *PostgresStore) ListTaints(ctx context.Context) ([]*model.Taint, error) {
	return queryListTaints(ctx, s.db)
}

func (s *PostgresStore) GrantTaint(ctx context.Context, g *model.TaintGrant) error {
	return queryGrantTaint(ctx, s.db, g)
}

func (s *PostgresStore) RevokeTaint(ctx context.Context, taintID int64, principalKind, principal string) (bool, error) {
	return queryRevokeTaint(ctx, s.db, taintID, principalKind, principal)
}

func (s *PostgresStore) GetTaintGrant(ctx context.Context, taintID int64, principalKind, principal string) (*model.TaintGrant, error) {
	return queryGetTaintGrant(ctx, s.db, taintID, principalKind, principal)
}

func (s *PostgresStore) ApplyTaint(ctx context.Context, bt *model.BeadTaint) (bool, error) {
	return queryApplyTaint(ctx, s.db, bt)
}

func (s *PostgresStore) GetBeadTaints(ctx context.Context, beadID string) ([]*model.BeadTaint, error) {
	return queryGetBeadTaints(ctx, s.db, beadID)
}

func (s *PostgresStore) GetGateBeadTaints(ctx context.Context, beadID string) ([]*model.BeadTaint, error) {
	return queryGetGateBeadTaints(ctx, s.db, beadID)
}

func (s *PostgresStore) RecordTaintAudit(ctx context.Context, a *model.TaintAudit) error {
	return queryRecordTaintAudit(ctx, s.db, a)
}

func (s *PostgresStore) ListTaintGrants(ctx context.Context, taintID int64) ([]*model.TaintGrant, error) {
	return queryListTaintGrants(ctx, s.db, taintID)
}

func (s *PostgresStore) ListTaintAudit(ctx context.Context, taintID int64, filter model.TaintAuditFilter) ([]*model.TaintAudit, int, error) {
	return queryListTaintAudit(ctx, s.db, taintID, filter)
}

func (s *PostgresStore) GetBeadTaintsForTaint(ctx context.Context, taintID int64, beadIDs []string) (map[string]*model.BeadTaint, error) {
	return queryGetBeadTaintsForTaint(ctx, s.db, taintID, beadIDs)
}

func (s *PostgresStore) AddComment(ctx context.Context, comment *model.Comment) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit
	if err := queryAddComment(ctx, tx, comment); err != nil {
		return err
	}
	if s.extractCommentRefs {
		if err := replaceCommentReferences(ctx, tx, comment); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *PostgresStore) GetComments(ctx context.Context, beadID string) ([]*model.Comment, error) {
	return queryGetComments(ctx, s.db, beadID)
}

func (s *PostgresStore) DeleteComment(ctx context.Context, beadID string, commentID int64) (*model.Comment, error) {
	return queryDeleteComment(ctx, s.db, beadID, commentID)
}

func (s *PostgresStore) TrimComments(ctx context.Context, beadID string, keepLast int, authorFilter string) ([]*model.Comment, error) {
	return queryTrimComments(ctx, s.db, beadID, keepLast, authorFilter)
}

func (s *PostgresStore) ReextractReferencesPage(ctx context.Context, afterID string, batchSize int, beadType string) (*store.ReextractReferencesStats, error) {
	return queryReextractReferencesPage(ctx, s.db, afterID, batchSize, s.extractCommentRefs, s.reconcileTypedMentionDeps, beadType)
}

func (s *PostgresStore) BackfillSearchTSVPage(ctx context.Context, afterID string, batchSize int, reindex bool) (*store.BackfillSearchTSVStats, error) {
	return queryBackfillSearchTSVPage(ctx, s.db, afterID, batchSize, reindex)
}

func (s *PostgresStore) BackfillCategoryPage(ctx context.Context, afterID string, batchSize int) (*store.BackfillCategoryStats, error) {
	return queryBackfillCategoryPage(ctx, s.db, afterID, batchSize)
}

func (s *PostgresStore) BackfillProjectLabelsPage(ctx context.Context, projectTypes []string, batchSize int) (*store.BackfillProjectLabelsStats, error) {
	return queryBackfillProjectLabelsPage(ctx, s.db, projectTypes, batchSize)
}

func (s *PostgresStore) BackfillOperators(ctx context.Context, opts store.BackfillOperatorsOptions) (*store.BackfillOperatorsStats, error) {
	return queryBackfillOperators(ctx, s.db, opts)
}

func (s *PostgresStore) BackfillOperatedByLegacy(ctx context.Context, opts store.BackfillOperatedByLegacyOptions) (*store.BackfillOperatedByLegacyStats, error) {
	return queryBackfillOperatedByLegacy(ctx, s.db, opts)
}

func (s *PostgresStore) BackfillSlackUserID(ctx context.Context, opts store.BackfillSlackUserIDOptions) (*store.BackfillSlackUserIDStats, error) {
	return queryBackfillSlackUserID(ctx, s.db, opts)
}

func (s *PostgresStore) BackfillOperatorSlugs(ctx context.Context, opts store.BackfillOperatorSlugsOptions) (*store.BackfillOperatorSlugsStats, error) {
	return queryBackfillOperatorSlugs(ctx, s.db, opts)
}

func (s *PostgresStore) BackfillOwner(ctx context.Context, opts store.BackfillOwnerOptions) (*store.BackfillOwnerStats, error) {
	return queryBackfillOwner(ctx, s.db, opts)
}

func (s *PostgresStore) BackfillEmptyOwner(ctx context.Context, opts store.BackfillEmptyOwnerOptions) (*store.BackfillEmptyOwnerStats, error) {
	return queryBackfillEmptyOwner(ctx, s.db, opts)
}

func (s *PostgresStore) BackfillOperatedByAgents(ctx context.Context, opts store.BackfillOperatedByAgentsOptions) (*store.BackfillOperatedByAgentsStats, error) {
	return queryBackfillOperatedByAgents(ctx, s.db, opts)
}

func (s *PostgresStore) BackfillParentChildDeps(ctx context.Context, opts store.BackfillParentChildDepsOptions) (*store.BackfillParentChildDepsStats, error) {
	return queryBackfillParentChildDeps(ctx, s.db, opts)
}

func (s *PostgresStore) BackfillTriggeredByDeps(ctx context.Context, opts store.BackfillTriggeredByDepsOptions) (*store.BackfillTriggeredByDepsStats, error) {
	return queryBackfillTriggeredByDeps(ctx, s.db, opts)
}

func (s *PostgresStore) CloseTriggerAuditBeads(ctx context.Context, opts store.CloseTriggerAuditBeadsOptions) (*store.CloseTriggerAuditBeadsStats, error) {
	return queryCloseTriggerAuditBeads(ctx, s.db, opts)
}

func (s *PostgresStore) RecordEvent(ctx context.Context, event *model.Event) error {
	return queryRecordEvent(ctx, s.db, event)
}

func (s *PostgresStore) GetEvents(ctx context.Context, beadID string) ([]*model.Event, error) {
	return queryGetEvents(ctx, s.db, beadID)
}

func (s *PostgresStore) GetRecentEvents(ctx context.Context, beadID string, limit int) ([]*model.Event, error) {
	return queryGetRecentEvents(ctx, s.db, beadID, limit)
}

func (s *PostgresStore) GetEventsByTopic(ctx context.Context, beadID, topic string, limit int) ([]*model.Event, error) {
	return queryGetEventsByTopic(ctx, s.db, beadID, topic, limit)
}

func (s *PostgresStore) EventHistogram(ctx context.Context, since, until time.Time, bucket string, scope model.EventReadScope) ([]model.EventHistogramBucket, error) {
	return queryEventHistogram(ctx, s.db, since, until, bucket, scope)
}

func (s *PostgresStore) GetEventsByTimeRange(ctx context.Context, since, until time.Time, cursor int64, limit int, scope model.EventReadScope) ([]*model.Event, error) {
	return queryGetEventsByTimeRange(ctx, s.db, since, until, cursor, limit, scope)
}

func (s *PostgresStore) GetEventsByTimeRangeDesc(ctx context.Context, since, until time.Time, cursor int64, limit int, scope model.EventReadScope) ([]*model.Event, error) {
	return queryGetEventsByTimeRangeDesc(ctx, s.db, since, until, cursor, limit, scope)
}

func (s *PostgresStore) LastEventAtByOperatorAliases(ctx context.Context, aliases []string) (map[string]time.Time, error) {
	return queryLastEventAtByOperatorAliases(ctx, s.db, aliases)
}

func (s *PostgresStore) SetConfig(ctx context.Context, config *model.Config) error {
	return querySetConfig(ctx, s.db, config)
}

func (s *PostgresStore) GetConfig(ctx context.Context, key string) (*model.Config, error) {
	return queryGetConfig(ctx, s.db, key)
}

func (s *PostgresStore) ListConfigs(ctx context.Context, namespace string) ([]*model.Config, error) {
	return queryListConfigs(ctx, s.db, namespace)
}

func (s *PostgresStore) ListAllConfigs(ctx context.Context) ([]*model.Config, error) {
	return queryListAllConfigs(ctx, s.db)
}

func (s *PostgresStore) DeleteConfig(ctx context.Context, key string) error {
	return queryDeleteConfig(ctx, s.db, key)
}

func (s *PostgresStore) UpsertEmbedding(ctx context.Context, beadID string, embedding []float32, modelName, textHash string) error {
	return queryUpsertEmbedding(ctx, s.db, beadID, embedding, modelName, textHash)
}

func (s *PostgresStore) SemanticSearch(ctx context.Context, queryVec []float32, filter model.BeadFilter, limit int) ([]*model.SemanticResult, error) {
	// Acquire a single backend session so the `SET ivfflat.probes` issued by
	// querySemanticSearch lands on the same connection as the SELECT. Without
	// this, the connection pool could route the SET and the SELECT to different
	// backends and the probes bump would silently no-op (ka-0000005881).
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire connection for semantic search: %w", err)
	}
	defer conn.Close()
	return querySemanticSearch(ctx, conn, queryVec, filter, limit)
}

func (s *PostgresStore) GetEmbedding(ctx context.Context, beadID string) ([]float32, string, error) {
	return queryGetEmbedding(ctx, s.db, beadID)
}

func (s *PostgresStore) GetEmbeddingHashes(ctx context.Context, beadIDs []string) (map[string]string, error) {
	return queryGetEmbeddingHashes(ctx, s.db, beadIDs)
}

func (s *PostgresStore) ListBeadsNeedingEmbedding(ctx context.Context, fleet string, limit, offset int) ([]*model.Bead, int, error) {
	return queryListBeadsNeedingEmbedding(ctx, s.db, fleet, limit, offset)
}

func (s *PostgresStore) RefreshEmbeddingTimestamps(ctx context.Context, beadIDs []string) (int64, error) {
	return queryRefreshEmbeddingTimestamps(ctx, s.db, beadIDs)
}

func (s *PostgresStore) DeleteEmbedding(ctx context.Context, beadID string) error {
	return queryDeleteEmbedding(ctx, s.db, beadID)
}

func (s *PostgresStore) GetEmbeddingStats(ctx context.Context) (*model.EmbeddingStats, error) {
	return queryGetEmbeddingStats(ctx, s.db)
}

func (s *PostgresStore) MigrateEmbeddingsSchema(ctx context.Context, modelName string, dims int, actor, note string) error {
	return queryMigrateEmbeddingsSchema(ctx, s.db, modelName, dims, actor, note)
}

func (s *PostgresStore) RecreateEmbeddingIndex(ctx context.Context) error {
	return queryRecreateANNIndex(ctx, s.db)
}

// RunInTransaction begins a database transaction, creates a txStore that
// delegates to it, calls fn, and commits on success or rolls back on error.
func (s *PostgresStore) RunInTransaction(ctx context.Context, fn func(tx store.Store) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}

	txS := &txStore{tx: tx, extractCommentRefs: s.extractCommentRefs, reconcileTypedMentionDeps: s.reconcileTypedMentionDeps}
	if err := fn(txS); err != nil {
		_ = tx.Rollback()
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// txStore implements store.Store using a *sql.Tx.
type txStore struct {
	tx                        *sql.Tx
	extractCommentRefs        bool
	reconcileTypedMentionDeps bool
}

// Compile-time check that txStore implements store.Store.
var _ store.Store = (*txStore)(nil)

func (s *txStore) CreateBead(ctx context.Context, bead *model.Bead) error {
	if err := queryCreateBead(ctx, s.tx, bead); err != nil {
		return err
	}
	return replaceBeadReferences(ctx, s.tx, bead)
}

func (s *txStore) GetBead(ctx context.Context, id string) (*model.Bead, error) {
	return queryGetBead(ctx, s.tx, id)
}

func (s *txStore) GetBeadIncludingDeleted(ctx context.Context, id string) (*model.Bead, error) {
	return queryGetBeadIncludingDeleted(ctx, s.tx, id)
}

func (s *txStore) FindAgentByName(ctx context.Context, name string, statuses []model.Status) (*model.Bead, error) {
	return queryFindAgentByName(ctx, s.tx, name, statuses)
}

func (s *txStore) FindAgentsByNames(ctx context.Context, names []string, statuses []model.Status) ([]*model.Bead, error) {
	return queryFindAgentsByNames(ctx, s.tx, names, statuses)
}

func (s *txStore) FindProjectByName(ctx context.Context, name string) (*model.Bead, error) {
	return queryFindProjectByName(ctx, s.tx, name)
}

func (s *txStore) ListBeads(ctx context.Context, filter model.BeadFilter) ([]*model.Bead, int, error) {
	return queryListBeads(ctx, s.tx, filter)
}

func (s *txStore) FillShortTitle(ctx context.Context, bead *model.Bead) (bool, error) {
	return queryFillShortTitle(ctx, s.tx, bead)
}

func (s *txStore) LockBeadAssignee(ctx context.Context, id string) (string, error) {
	return queryLockBeadAssignee(ctx, s.tx, id)
}

func (s *txStore) UpdateBead(ctx context.Context, bead *model.Bead) error {
	if err := queryUpdateBead(ctx, s.tx, bead); err != nil {
		return err
	}
	if err := replaceBeadReferences(ctx, s.tx, bead); err != nil {
		return err
	}
	if s.reconcileTypedMentionDeps {
		return reconcileTypedMentionDeps(ctx, s.tx, bead, s.extractCommentRefs)
	}
	return nil
}

func (s *txStore) MergeBeadFields(ctx context.Context, id string, extra map[string]any) (*model.Bead, error) {
	bead, err := queryMergeBeadFields(ctx, s.tx, id, extra)
	if err != nil {
		return nil, err
	}
	if err := replaceBeadReferences(ctx, s.tx, bead); err != nil {
		return nil, err
	}
	if s.reconcileTypedMentionDeps {
		if err := reconcileTypedMentionDeps(ctx, s.tx, bead, s.extractCommentRefs); err != nil {
			return nil, err
		}
	}
	return bead, nil
}

func (s *txStore) IncrementBeadFields(ctx context.Context, id string, deltas map[string]float64) (*model.Bead, error) {
	return queryIncrementBeadFields(ctx, s.tx, id, deltas)
}

func (s *txStore) AllocateAgentHandleSeq(ctx context.Context, gardenerID string) (int64, error) {
	return queryAllocateAgentHandleSeq(ctx, s.tx, gardenerID)
}

func (s *txStore) AppendBeadArrayField(ctx context.Context, id, field, element string) (*model.Bead, bool, error) {
	return queryAppendBeadArrayField(ctx, s.tx, id, field, element)
}

func (s *txStore) InsertBeadArrayField(ctx context.Context, id, field, element string, index int) (*model.Bead, bool, error) {
	return queryInsertBeadArrayField(ctx, s.tx, id, field, element, index)
}

func (s *txStore) CASBeadArrayField(ctx context.Context, id, field string, expected, next []string) (*model.Bead, error) {
	return queryCASBeadArrayField(ctx, s.tx, id, field, expected, next)
}

func (s *txStore) CloseBead(ctx context.Context, id string, closedBy string) (*model.Bead, error) {
	return queryCloseBead(ctx, s.tx, id, closedBy)
}

func (s *txStore) DeleteBead(ctx context.Context, id string) error {
	return queryDeleteBead(ctx, s.tx, id)
}

func (s *txStore) RestoreBead(ctx context.Context, id string) (*model.Bead, error) {
	return queryRestoreBead(ctx, s.tx, id)
}

func (s *txStore) TransferBead(ctx context.Context, id string, toFleet string) (*model.Bead, string, error) {
	return queryTransferBead(ctx, s.tx, id, toFleet)
}

func (s *txStore) GetStats(ctx context.Context) (*model.GraphStats, error) {
	return queryGetStats(ctx, s.tx)
}

func (s *txStore) FleetTypeStatusCounts(ctx context.Context) (map[store.FleetTypeStatusKey]int, error) {
	return queryFleetTypeStatusCounts(ctx, s.tx)
}

func (s *txStore) FleetActiveAgentsByProject(ctx context.Context) (map[string]int, error) {
	return queryFleetActiveAgentsByProject(ctx, s.tx)
}

func (s *txStore) FleetAgentLifetimeRollup(ctx context.Context) ([]store.FleetAgentProjectRollup, error) {
	return queryFleetAgentLifetimeRollup(ctx, s.tx)
}

func (s *txStore) FleetQueueDepthByProject(ctx context.Context) (map[store.FleetQueueDepthKey]int, error) {
	return queryFleetQueueDepthByProject(ctx, s.tx, store.FleetQueueTypes)
}

func (s *txStore) FleetQueueOldestAgeByProject(ctx context.Context) (map[store.FleetQueueDepthKey]float64, error) {
	return queryFleetQueueOldestAgeByProject(ctx, s.tx, store.FleetQueueTypes)
}

func (s *txStore) ProjectLabelMissingCounts(ctx context.Context, projectTypes []string) (map[string]int, error) {
	return queryProjectLabelMissingCounts(ctx, s.tx, projectTypes)
}

func (s *txStore) AcceptanceMetrics(ctx context.Context) (*store.AcceptanceMetrics, error) {
	return queryAcceptanceMetrics(ctx, s.tx)
}

func (s *txStore) GetDependenciesForBeads(ctx context.Context, beadIDs []string) (map[string][]*model.Dependency, error) {
	return queryGetDependenciesForBeads(ctx, s.tx, beadIDs)
}

func (s *txStore) GetReverseDependenciesForBeads(ctx context.Context, beadIDs []string) (map[string][]*model.Dependency, error) {
	return queryGetReverseDependenciesForBeads(ctx, s.tx, beadIDs)
}

func (s *txStore) GetDependencyCounts(ctx context.Context, beadIDs []string) (map[string]*model.DependencyCounts, error) {
	return queryGetDependencyCounts(ctx, s.tx, beadIDs)
}

func (s *txStore) GetChildrenStatusHistogram(ctx context.Context, parentIDs []string) (map[string]map[string]int, error) {
	return queryGetChildrenStatusHistogram(ctx, s.tx, parentIDs)
}

func (s *txStore) GetLabelsForBeads(ctx context.Context, beadIDs []string) (map[string][]string, error) {
	return queryGetLabelsForBeads(ctx, s.tx, beadIDs)
}

func (s *txStore) GetBlockedByForBeads(ctx context.Context, beadIDs []string) (map[string][]string, error) {
	return queryGetBlockedByForBeads(ctx, s.tx, beadIDs)
}

func (s *txStore) GetBeadsByIDs(ctx context.Context, ids []string) ([]*model.Bead, error) {
	return queryGetBeadsByIDs(ctx, s.tx, ids)
}

func (s *txStore) GetTextReferencesForBeads(ctx context.Context, beadIDs []string) ([]*model.BeadReference, error) {
	return queryGetTextReferencesForBeads(ctx, s.tx, beadIDs)
}

func (s *txStore) AddDependency(ctx context.Context, dep *model.Dependency) error {
	return queryAddDependency(ctx, s.tx, dep)
}

func (s *txStore) RemoveDependency(ctx context.Context, beadID, dependsOnID string, depType model.DependencyType) (bool, error) {
	n, err := queryRemoveDependency(ctx, s.tx, beadID, dependsOnID, depType)
	return n > 0, err
}

func (s *txStore) GetDependencies(ctx context.Context, beadID string) ([]*model.Dependency, error) {
	return queryGetDependencies(ctx, s.tx, beadID)
}

func (s *txStore) GetReverseDependencies(ctx context.Context, beadID string) ([]*model.Dependency, error) {
	return queryGetReverseDependencies(ctx, s.tx, beadID)
}

func (s *txStore) GetDependencyEdgesByType(ctx context.Context, depType model.DependencyType) ([]model.DependencyEdge, error) {
	return queryGetDependencyEdgesByType(ctx, s.tx, depType)
}

func (s *txStore) ListBeadsMentioning(ctx context.Context, targetID string, limit, offset int, relationshipType *string) ([]*model.BeadReferenceSummary, int, error) {
	return queryListBeadsMentioning(ctx, s.tx, targetID, limit, offset, relationshipType)
}

func (s *txStore) HasBundleChild(ctx context.Context, parentID string) (bool, error) {
	return queryHasBundleChild(ctx, s.tx, parentID)
}

func (s *txStore) ListBeadReferencesFrom(ctx context.Context, sourceID string, limit, offset int, relationshipType *string) ([]*model.BeadReferenceSummary, int, error) {
	return queryListBeadReferencesFrom(ctx, s.tx, sourceID, limit, offset, relationshipType)
}

func (s *txStore) AddLabel(ctx context.Context, beadID string, label string) error {
	return queryAddLabel(ctx, s.tx, beadID, label)
}

func (s *txStore) RemoveLabel(ctx context.Context, beadID string, label string) (bool, error) {
	n, err := queryRemoveLabel(ctx, s.tx, beadID, label)
	return n > 0, err
}

func (s *txStore) GetLabels(ctx context.Context, beadID string) ([]string, error) {
	return queryGetLabels(ctx, s.tx, beadID)
}

func (s *txStore) GetTaintsForBead(ctx context.Context, beadID string) ([]int64, error) {
	return queryGetTaintsForBead(ctx, s.tx, beadID)
}

func (s *txStore) GetTaintsForBeads(ctx context.Context, beadIDs []string) (map[string][]int64, error) {
	return queryGetTaintsForBeads(ctx, s.tx, beadIDs)
}

func (s *txStore) GetAllTaintsForBead(ctx context.Context, beadID string) ([]int64, error) {
	return queryGetAllTaintsForBead(ctx, s.tx, beadID)
}

func (s *txStore) GetAllTaintsForBeads(ctx context.Context, beadIDs []string) (map[string][]int64, error) {
	return queryGetAllTaintsForBeads(ctx, s.tx, beadIDs)
}

func (s *txStore) GetGrantedTaintIDs(ctx context.Context, principalKind string, principals []string) ([]int64, error) {
	return queryGetGrantedTaintIDs(ctx, s.tx, principalKind, principals)
}

func (s *txStore) GetTaintIDsOwnedByTeams(ctx context.Context, teams []string) ([]int64, error) {
	return queryGetTaintIDsOwnedByTeams(ctx, s.tx, teams)
}

func (s *txStore) HasAnyTaints(ctx context.Context) (bool, error) {
	return queryHasAnyTaints(ctx, s.tx)
}

func (s *txStore) SetTaint(ctx context.Context, t *model.Taint) error {
	return querySetTaint(ctx, s.tx, t)
}

func (s *txStore) GetTaint(ctx context.Context, name string) (*model.Taint, error) {
	return queryGetTaint(ctx, s.tx, name)
}

func (s *txStore) ListTaints(ctx context.Context) ([]*model.Taint, error) {
	return queryListTaints(ctx, s.tx)
}

func (s *txStore) GrantTaint(ctx context.Context, g *model.TaintGrant) error {
	return queryGrantTaint(ctx, s.tx, g)
}

func (s *txStore) RevokeTaint(ctx context.Context, taintID int64, principalKind, principal string) (bool, error) {
	return queryRevokeTaint(ctx, s.tx, taintID, principalKind, principal)
}

func (s *txStore) GetTaintGrant(ctx context.Context, taintID int64, principalKind, principal string) (*model.TaintGrant, error) {
	return queryGetTaintGrant(ctx, s.tx, taintID, principalKind, principal)
}

func (s *txStore) ApplyTaint(ctx context.Context, bt *model.BeadTaint) (bool, error) {
	return queryApplyTaint(ctx, s.tx, bt)
}

func (s *txStore) GetBeadTaints(ctx context.Context, beadID string) ([]*model.BeadTaint, error) {
	return queryGetBeadTaints(ctx, s.tx, beadID)
}

func (s *txStore) GetGateBeadTaints(ctx context.Context, beadID string) ([]*model.BeadTaint, error) {
	return queryGetGateBeadTaints(ctx, s.tx, beadID)
}

func (s *txStore) RecordTaintAudit(ctx context.Context, a *model.TaintAudit) error {
	return queryRecordTaintAudit(ctx, s.tx, a)
}

func (s *txStore) ListTaintGrants(ctx context.Context, taintID int64) ([]*model.TaintGrant, error) {
	return queryListTaintGrants(ctx, s.tx, taintID)
}

func (s *txStore) ListTaintAudit(ctx context.Context, taintID int64, filter model.TaintAuditFilter) ([]*model.TaintAudit, int, error) {
	return queryListTaintAudit(ctx, s.tx, taintID, filter)
}

func (s *txStore) GetBeadTaintsForTaint(ctx context.Context, taintID int64, beadIDs []string) (map[string]*model.BeadTaint, error) {
	return queryGetBeadTaintsForTaint(ctx, s.tx, taintID, beadIDs)
}

func (s *txStore) AddComment(ctx context.Context, comment *model.Comment) error {
	if err := queryAddComment(ctx, s.tx, comment); err != nil {
		return err
	}
	if !s.extractCommentRefs {
		return nil
	}
	return replaceCommentReferences(ctx, s.tx, comment)
}

func (s *txStore) GetComments(ctx context.Context, beadID string) ([]*model.Comment, error) {
	return queryGetComments(ctx, s.tx, beadID)
}

func (s *txStore) DeleteComment(ctx context.Context, beadID string, commentID int64) (*model.Comment, error) {
	return queryDeleteComment(ctx, s.tx, beadID, commentID)
}

func (s *txStore) TrimComments(ctx context.Context, beadID string, keepLast int, authorFilter string) ([]*model.Comment, error) {
	return queryTrimComments(ctx, s.tx, beadID, keepLast, authorFilter)
}

// ReextractReferencesPage on txStore is intentionally unsupported: the
// backfill manages its own per-bead transactions and must not be nested
// inside another caller-supplied tx. Returning an error keeps the
// store.Store interface satisfied while making misuse loud.
func (s *txStore) ReextractReferencesPage(_ context.Context, _ string, _ int, _ string) (*store.ReextractReferencesStats, error) {
	return nil, fmt.Errorf("ReextractReferencesPage: not supported inside a transaction")
}

// BackfillSearchTSVPage on txStore is intentionally unsupported: the backfill
// runs one autocommit UPDATE per page so each batch is its own transaction
// (no-eager-backfill policy). Nesting inside a caller tx would defeat that.
func (s *txStore) BackfillSearchTSVPage(_ context.Context, _ string, _ int, _ bool) (*store.BackfillSearchTSVStats, error) {
	return nil, fmt.Errorf("BackfillSearchTSVPage: not supported inside a transaction")
}

// BackfillCategoryPage on txStore is intentionally unsupported for the same
// reason as BackfillSearchTSVPage: the backfill runs one autocommit UPDATE per
// page so each batch is its own transaction (no-eager-backfill policy). Nesting
// inside a caller tx would defeat that.
func (s *txStore) BackfillCategoryPage(_ context.Context, _ string, _ int) (*store.BackfillCategoryStats, error) {
	return nil, fmt.Errorf("BackfillCategoryPage: not supported inside a transaction")
}

func (s *txStore) BackfillProjectLabelsPage(_ context.Context, _ []string, _ int) (*store.BackfillProjectLabelsStats, error) {
	return nil, fmt.Errorf("BackfillProjectLabelsPage: not supported inside a transaction")
}

// BackfillOperators on txStore is intentionally unsupported: the backfill
// needs its own *sql.Conn and per-table autocommit batches; nesting inside
// a caller tx would deadlock or run forever. Loud error keeps misuse out.
func (s *txStore) BackfillOperators(_ context.Context, _ store.BackfillOperatorsOptions) (*store.BackfillOperatorsStats, error) {
	return nil, fmt.Errorf("BackfillOperators: not supported inside a transaction")
}

// BackfillOperatedByLegacy on txStore is intentionally unsupported: the
// backfill manages its own per-batch transactions and parent-walk
// iterations, so nesting inside a caller tx would either deadlock on the
// candidate scan or violate the per-batch-commit invariant.
func (s *txStore) BackfillOperatedByLegacy(_ context.Context, _ store.BackfillOperatedByLegacyOptions) (*store.BackfillOperatedByLegacyStats, error) {
	return nil, fmt.Errorf("BackfillOperatedByLegacy: not supported inside a transaction")
}

// BackfillSlackUserID on txStore is intentionally unsupported for the same
// reason as BackfillOperatedByLegacy: it manages its own per-batch
// transactions.
func (s *txStore) BackfillSlackUserID(_ context.Context, _ store.BackfillSlackUserIDOptions) (*store.BackfillSlackUserIDStats, error) {
	return nil, fmt.Errorf("BackfillSlackUserID: not supported inside a transaction")
}

// BackfillOperatorSlugs on txStore is intentionally unsupported for the same
// reason as BackfillSlackUserID: it manages its own per-candidate
// transactions and must not nest inside an outer tx.
func (s *txStore) BackfillOperatorSlugs(_ context.Context, _ store.BackfillOperatorSlugsOptions) (*store.BackfillOperatorSlugsStats, error) {
	return nil, fmt.Errorf("BackfillOperatorSlugs: not supported inside a transaction")
}

// BackfillOwner on txStore is intentionally unsupported for the same
// reason as BackfillOperatedByLegacy: it manages its own per-batch
// transactions.
func (s *txStore) BackfillOwner(_ context.Context, _ store.BackfillOwnerOptions) (*store.BackfillOwnerStats, error) {
	return nil, fmt.Errorf("BackfillOwner: not supported inside a transaction")
}

// BackfillEmptyOwner on txStore is intentionally unsupported for the same
// reason as BackfillOwner: it manages its own per-batch transactions
// across three phases and must not nest inside an outer tx.
func (s *txStore) BackfillEmptyOwner(_ context.Context, _ store.BackfillEmptyOwnerOptions) (*store.BackfillEmptyOwnerStats, error) {
	return nil, fmt.Errorf("BackfillEmptyOwner: not supported inside a transaction")
}

// BackfillOperatedByAgents on txStore is intentionally unsupported for
// the same per-batch-transaction reason as BackfillOwner — the
// per-user-homepage M2 backfill (sub-item #1 of ka-0000006817) manages
// its own per-page commits and must not nest inside an outer tx.
func (s *txStore) BackfillOperatedByAgents(_ context.Context, _ store.BackfillOperatedByAgentsOptions) (*store.BackfillOperatedByAgentsStats, error) {
	return nil, fmt.Errorf("BackfillOperatedByAgents: not supported inside a transaction")
}

// BackfillParentChildDeps on txStore is intentionally unsupported: the
// backfill manages its own per-batch transactions, so nesting inside a
// caller tx would either deadlock on the candidate scan or violate the
// per-batch-commit invariant.
func (s *txStore) BackfillParentChildDeps(_ context.Context, _ store.BackfillParentChildDepsOptions) (*store.BackfillParentChildDepsStats, error) {
	return nil, fmt.Errorf("BackfillParentChildDeps: not supported inside a transaction")
}

// RepairReversedParentChildDeps on txStore is intentionally unsupported
// for the same per-batch-transaction reason as BackfillParentChildDeps:
// the repair manages its own per-page commits and must not nest inside
// an outer tx.
func (s *txStore) RepairReversedParentChildDeps(_ context.Context, _ store.RepairReversedParentChildDepsOptions) (*store.RepairReversedParentChildDepsStats, error) {
	return nil, fmt.Errorf("RepairReversedParentChildDeps: not supported inside a transaction")
}

// BackfillTriggeredByDeps on txStore is intentionally unsupported for
// the same per-batch-transaction reason as BackfillParentChildDeps —
// the M4 auto-link scanner (ka-0000000004) manages its own per-page
// commits and must not nest inside an outer tx.
func (s *txStore) BackfillTriggeredByDeps(_ context.Context, _ store.BackfillTriggeredByDepsOptions) (*store.BackfillTriggeredByDepsStats, error) {
	return nil, fmt.Errorf("BackfillTriggeredByDeps: not supported inside a transaction")
}

// CloseTriggerAuditBeads on txStore is intentionally unsupported: the
// sweep drives its own per-batch transactions and must not run inside
// an outer tx.
func (s *txStore) CloseTriggerAuditBeads(_ context.Context, _ store.CloseTriggerAuditBeadsOptions) (*store.CloseTriggerAuditBeadsStats, error) {
	return nil, fmt.Errorf("CloseTriggerAuditBeads: not supported inside a transaction")
}

func (s *txStore) RecordEvent(ctx context.Context, event *model.Event) error {
	return queryRecordEvent(ctx, s.tx, event)
}

func (s *txStore) GetEvents(ctx context.Context, beadID string) ([]*model.Event, error) {
	return queryGetEvents(ctx, s.tx, beadID)
}

func (s *txStore) GetRecentEvents(ctx context.Context, beadID string, limit int) ([]*model.Event, error) {
	return queryGetRecentEvents(ctx, s.tx, beadID, limit)
}

func (s *txStore) GetEventsByTopic(ctx context.Context, beadID, topic string, limit int) ([]*model.Event, error) {
	return queryGetEventsByTopic(ctx, s.tx, beadID, topic, limit)
}

func (s *txStore) EventHistogram(ctx context.Context, since, until time.Time, bucket string, scope model.EventReadScope) ([]model.EventHistogramBucket, error) {
	return queryEventHistogram(ctx, s.tx, since, until, bucket, scope)
}

func (s *txStore) GetEventsByTimeRange(ctx context.Context, since, until time.Time, cursor int64, limit int, scope model.EventReadScope) ([]*model.Event, error) {
	return queryGetEventsByTimeRange(ctx, s.tx, since, until, cursor, limit, scope)
}

func (s *txStore) GetEventsByTimeRangeDesc(ctx context.Context, since, until time.Time, cursor int64, limit int, scope model.EventReadScope) ([]*model.Event, error) {
	return queryGetEventsByTimeRangeDesc(ctx, s.tx, since, until, cursor, limit, scope)
}

func (s *txStore) LastEventAtByOperatorAliases(ctx context.Context, aliases []string) (map[string]time.Time, error) {
	return queryLastEventAtByOperatorAliases(ctx, s.tx, aliases)
}

func (s *txStore) SetConfig(ctx context.Context, config *model.Config) error {
	return querySetConfig(ctx, s.tx, config)
}

func (s *txStore) GetConfig(ctx context.Context, key string) (*model.Config, error) {
	return queryGetConfig(ctx, s.tx, key)
}

func (s *txStore) ListConfigs(ctx context.Context, namespace string) ([]*model.Config, error) {
	return queryListConfigs(ctx, s.tx, namespace)
}

func (s *txStore) ListAllConfigs(ctx context.Context) ([]*model.Config, error) {
	return queryListAllConfigs(ctx, s.tx)
}

func (s *txStore) DeleteConfig(ctx context.Context, key string) error {
	return queryDeleteConfig(ctx, s.tx, key)
}

func (s *txStore) UpsertEmbedding(ctx context.Context, beadID string, embedding []float32, modelName, textHash string) error {
	return queryUpsertEmbedding(ctx, s.tx, beadID, embedding, modelName, textHash)
}

func (s *txStore) SemanticSearch(ctx context.Context, queryVec []float32, filter model.BeadFilter, limit int) ([]*model.SemanticResult, error) {
	return querySemanticSearch(ctx, s.tx, queryVec, filter, limit)
}

func (s *txStore) GetEmbedding(ctx context.Context, beadID string) ([]float32, string, error) {
	return queryGetEmbedding(ctx, s.tx, beadID)
}

func (s *txStore) GetEmbeddingHashes(ctx context.Context, beadIDs []string) (map[string]string, error) {
	return queryGetEmbeddingHashes(ctx, s.tx, beadIDs)
}

func (s *txStore) ListBeadsNeedingEmbedding(ctx context.Context, fleet string, limit, offset int) ([]*model.Bead, int, error) {
	return queryListBeadsNeedingEmbedding(ctx, s.tx, fleet, limit, offset)
}

func (s *txStore) RefreshEmbeddingTimestamps(ctx context.Context, beadIDs []string) (int64, error) {
	return queryRefreshEmbeddingTimestamps(ctx, s.tx, beadIDs)
}

func (s *txStore) DeleteEmbedding(ctx context.Context, beadID string) error {
	return queryDeleteEmbedding(ctx, s.tx, beadID)
}

func (s *txStore) GetEmbeddingStats(ctx context.Context) (*model.EmbeddingStats, error) {
	return queryGetEmbeddingStats(ctx, s.tx)
}

func (s *txStore) MigrateEmbeddingsSchema(ctx context.Context, modelName string, dims int, actor, note string) error {
	return queryMigrateEmbeddingsSchema(ctx, s.tx, modelName, dims, actor, note)
}

func (s *txStore) RecreateEmbeddingIndex(ctx context.Context) error {
	return queryRecreateANNIndex(ctx, s.tx)
}

// RunInTransaction on a txStore reuses the existing transaction (no nesting).
func (s *txStore) RunInTransaction(ctx context.Context, fn func(tx store.Store) error) error {
	return fn(s)
}

// Ping is a no-op for a transaction store; the parent store owns the connection.
func (s *txStore) Ping(_ context.Context) error {
	return nil
}

// Close is a no-op for a transaction store; the parent store owns the connection.
func (s *txStore) Close() error {
	return nil
}
