package main

import (
	"fmt"
	"os"
	"time"

	"gardenio/kbeads/internal/store/postgres"
	"github.com/spf13/cobra"
)

// migrateCmd is the standalone migration runner: opens BEADS_DATABASE_URL,
// applies any pending migrations, exits 0/1. Intended to be invoked by the
// pre-upgrade Helm Job (ka-0000007607) so migrations run to completion in
// a Job — no startup-probe deadline, no kubelet SIGKILL mid-DDL — and the
// daemon Deployment then rolls only after migrations have landed.
//
// Honors BEADS_MIGRATE_AUTO_RECOVER=last-clean for dirty-state recovery,
// same as the daemon path. See ka-0000005716 for the broader plan that
// removes m.Up() from the daemon-startup path entirely.
var migrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Apply pending schema migrations and exit",
	Long: `Connect to BEADS_DATABASE_URL, apply any pending schema migrations,
and exit 0 on success or 1 on failure. Designed to run as a one-shot Job
(no HTTP listener, no startup probe). On long-running migrations the Job
controls the deadline via activeDeadlineSeconds; the daemon is then free
to start only after migrations complete.

Set BEADS_MIGRATE_AUTO_RECOVER=last-clean to auto-clear schema_migrations.dirty
to last-clean version and retry.`,
	// Connects to BEADS_DATABASE_URL only; the pre-upgrade Job sets no daemon URL.
	Annotations: map[string]string{noDaemonAnnotation: "true"},
	RunE:        runMigrate,
}

func init() {
	rootCmd.AddCommand(migrateCmd)
}

func runMigrate(_ *cobra.Command, _ []string) error {
	databaseURL := os.Getenv("BEADS_DATABASE_URL")
	if databaseURL == "" {
		return fmt.Errorf("BEADS_DATABASE_URL is required")
	}

	start := time.Now()
	fmt.Println("applying schema migrations…")
	// BEADS_DATABASE_PASSWORD_FILE (optional): read the DB password from a
	// live-updating mounted Secret file per connection so the migrate Job
	// tolerates a mid-rotation credential skew rather than failing on pq
	// 28P01 (ka-0000004150). Empty = legacy env-var mode.
	if err := postgres.MigrateWithPasswordFile(databaseURL, os.Getenv("BEADS_DATABASE_PASSWORD_FILE")); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	fmt.Printf("migrations applied in %s\n", time.Since(start).Round(time.Millisecond))
	return nil
}
