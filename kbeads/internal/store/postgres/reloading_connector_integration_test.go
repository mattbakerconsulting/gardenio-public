//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lib/pq"
)

// TestPasswordFileConnector_LiveRotation is the end-to-end proof for the P0
// rotation-tolerant reconnect fix (ka-0000004150 / postmortem ka-0000006811).
// It connects to a real Postgres via the passwordFileConnector, ROTATES the
// role's password (ALTER ROLE ... PASSWORD) AND the mounted password file, then
// verifies that a brand-new pool connection authenticates with the ROTATED
// password — no process restart. This is exactly what a secretKeyRef env var
// CANNOT do (it is frozen at container start) and is why the daemon CrashLooped
// fleet-wide on the RDS master-password rotation.
//
// Gated by `//go:build integration` and BEADS_TEST_DATABASE_URL so the default
// sqlmock-only `go test ./...` workflow is unaffected. The DSN's role MUST have
// permission to ALTER its own password (a superuser like the docker-postgres
// default `postgres` user does).
//
// Run with:
//
//	BEADS_TEST_DATABASE_URL=postgres://postgres:pw@localhost:5432/beads?sslmode=disable \
//	  go test -tags=integration ./internal/store/postgres/ \
//	  -run TestPasswordFileConnector_LiveRotation
func TestPasswordFileConnector_LiveRotation(t *testing.T) {
	dsn := os.Getenv("BEADS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("BEADS_TEST_DATABASE_URL not set; skipping real-Postgres integration test")
	}

	cfg, err := pq.NewConfig(dsn)
	if err != nil {
		t.Fatalf("parse test DSN: %v", err)
	}
	// The current password is our starting point; we rotate to a new one and
	// then restore it in cleanup so a shared test DB is left as we found it.
	origPassword := cfg.Password
	if origPassword == "" {
		t.Skip("BEADS_TEST_DATABASE_URL has no password; live-rotation test needs a password-auth role")
	}
	role := cfg.User

	// Materialize the mounted-Secret file with the CURRENT password.
	dir := t.TempDir()
	pwFile := filepath.Join(dir, "password")
	if err := os.WriteFile(pwFile, []byte(origPassword), 0o600); err != nil {
		t.Fatalf("write password file: %v", err)
	}

	connector, err := newPasswordFileConnector(dsn, pwFile)
	if err != nil {
		t.Fatalf("newPasswordFileConnector: %v", err)
	}
	db := sql.OpenDB(connector)
	// Force the pool to churn quickly so a rotated password is picked up
	// promptly by fresh connections (prod uses 5m; we want the test fast).
	db.SetConnMaxLifetime(100 * time.Millisecond)
	db.SetMaxIdleConns(0)
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("initial ping with current password failed: %v", err)
	}

	// Rotate: change the role password on the server, then update the mounted
	// file (mirroring an ExternalSecret sync of a rotated RDS master password).
	const rotated = "rotated-p@ss:w0rd/xyz"
	if _, err := db.ExecContext(ctx, "ALTER ROLE "+quoteIdent(role)+" WITH PASSWORD "+quoteLiteral(rotated)); err != nil {
		t.Fatalf("rotate role password: %v", err)
	}
	t.Cleanup(func() {
		// Restore the original password. `connector` reads the password from
		// pwFile on every dial, and at this point pwFile still holds the
		// ROTATED value (set below, at "update the mounted file to the
		// rotated password") -- matching what the server currently expects.
		// The restore connection MUST authenticate with that CURRENT value
		// to be able to run the ALTER ROLE that changes it back; writing
		// origPassword into pwFile first (as this used to do) makes the
		// very next dial present the OLD password to a role whose real
		// password is still the rotated one, so the dial 28P01s, the
		// ALTER ROLE never runs, and the error was discarded (`_, _ =`) --
		// silently leaving the real Postgres role's password rotated for
		// the rest of the CI job. Every later integration test's first
		// fresh connection (anything not already pooled pre-rotation) then
		// fails 28P01 too, which is exactly what surfaced as
		// TestSubFieldEditPrimitives_RealPostgres failing downstream
		// (gd-Ai6ekNov28 triage). Fix: ALTER ROLE back to origPassword
		// FIRST (dialing with the still-current rotated password), THEN
		// overwrite the file to match.
		restore := sql.OpenDB(connector)
		defer restore.Close()
		if _, err := restore.ExecContext(context.Background(),
			"ALTER ROLE "+quoteIdent(role)+" WITH PASSWORD "+quoteLiteral(origPassword)); err != nil {
			t.Logf("restore role password: %v", err)
		}
		if err := os.WriteFile(pwFile, []byte(origPassword), 0o600); err != nil {
			t.Logf("restore password file: %v", err)
		}
	})

	// Update the mounted file to the rotated password.
	if err := os.WriteFile(pwFile, []byte(rotated), 0o600); err != nil {
		t.Fatalf("update password file to rotated value: %v", err)
	}

	// Drain existing connections so the next query MUST open a fresh one that
	// authenticates with the rotated password read from the file.
	time.Sleep(200 * time.Millisecond)

	// The daemon self-heals: a fresh connection authenticates with the rotated
	// password with NO restart. Retry briefly to ride out pool churn timing.
	var pingErr error
	for i := 0; i < 20; i++ {
		if pingErr = db.PingContext(ctx); pingErr == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if pingErr != nil {
		t.Fatalf("after rotation, a fresh connection should authenticate with the ROTATED password from the file, but ping failed: %v", pingErr)
	}

	// Sanity: prove the connection is genuinely usable post-rotation.
	var one int
	if err := db.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil || one != 1 {
		t.Fatalf("post-rotation query failed: one=%d err=%v", one, err)
	}

	// Negative control: a connector whose FILE still holds the OLD password
	// must now FAIL — proving the auth actually switched (not a caching fluke).
	stalePwFile := filepath.Join(dir, "stale")
	if err := os.WriteFile(stalePwFile, []byte(origPassword), 0o600); err != nil {
		t.Fatalf("write stale password file: %v", err)
	}
	staleConnector, err := newPasswordFileConnector(dsn, stalePwFile)
	if err != nil {
		t.Fatalf("newPasswordFileConnector (stale): %v", err)
	}
	staleDB := sql.OpenDB(staleConnector)
	defer staleDB.Close()
	if err := staleDB.PingContext(ctx); err == nil {
		t.Fatal("a connection presenting the pre-rotation password should have been rejected (28P01), but ping succeeded")
	} else if !strings.Contains(err.Error(), "password authentication failed") && !strings.Contains(err.Error(), "28P01") {
		// Not fatal — some servers phrase it differently — but log for signal.
		t.Logf("stale-password rejection surfaced as: %v", err)
	}
}

// quoteIdent wraps a SQL identifier in double quotes, escaping embedded quotes.
func quoteIdent(ident string) string {
	return `"` + strings.ReplaceAll(ident, `"`, `""`) + `"`
}

// quoteLiteral wraps a SQL string literal in single quotes, escaping embedded
// quotes. Used only for the test's ALTER ROLE ... PASSWORD, whose value is a
// constant defined in this file (not user input).
func quoteLiteral(s string) string {
	return `'` + strings.ReplaceAll(s, `'`, `''`) + `'`
}
