package postgres

import (
	"context"
	"database/sql/driver"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/lib/pq"
)

// passwordFileConnector is a database/sql/driver.Connector that composes its
// DSN from a base connection string plus a password re-read from a file on
// EVERY physical connection attempt. It is the mechanism that makes the beads
// daemon rotation-tolerant: because a Kubernetes Secret volume live-updates on
// the kubelet sync loop (unlike a secretKeyRef env var, which is frozen at
// container start), each new pooled connection authenticates with the CURRENT
// password. When an RDS master-password rotation skews the credential, the
// pool's ConnMaxLifetime churn (or a fresh dial after a drop) picks up the new
// password WITHOUT a pod restart — no CrashLoop, no fleet-wide kd/gb outage.
//
// See bug ka-0000004150 (fix #2 of P0 postmortem ka-0000006811). lib/pq has no
// dynamic-password hook, so we implement driver.Connector and inject the
// password into the DSN per Connect() call rather than migrating to pgx (a far
// larger change touching every query, pq.Array, and the sqlmock tests).
type passwordFileConnector struct {
	// baseDSN is a URL-form connection string with NO password (the
	// userinfo carries only the username). The password is spliced in per
	// connection from passwordFile.
	baseDSN string
	// passwordFile is the path to the mounted Secret key holding the raw
	// (NOT url-encoded) password.
	passwordFile string
}

// newPasswordFileConnector builds a connector that reads its password from
// passwordFile on each connection. databaseURL is the full BEADS_DATABASE_URL;
// any password already present in it is stripped (the file is authoritative)
// so a stale $(BEADS_DATABASE_PASSWORD) substitution baked into the URL can
// never shadow the freshly-rotated file value.
//
// It requires a URL-form DSN (postgres:// or postgresql://) — the only form the
// chart renders. A keyword/DSN-form connection string is rejected with a clear
// error rather than silently ignoring the password file.
func newPasswordFileConnector(databaseURL, passwordFile string) (*passwordFileConnector, error) {
	if passwordFile == "" {
		return nil, fmt.Errorf("password file path is empty")
	}
	if !strings.HasPrefix(databaseURL, "postgres://") && !strings.HasPrefix(databaseURL, "postgresql://") {
		return nil, fmt.Errorf("BEADS_DATABASE_PASSWORD_FILE requires a URL-form BEADS_DATABASE_URL (postgres:// or postgresql://); got a keyword-form DSN")
	}
	u, err := url.Parse(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database URL: %w", err)
	}
	// Strip any embedded password; keep the username. The file is the sole
	// source of truth for the password from here on.
	if u.User != nil {
		u.User = url.User(u.User.Username())
	}
	// Verify the file is readable up front so a misconfigured mount fails
	// fast with a clear error instead of surfacing as an auth failure later.
	if _, err := readPasswordFile(passwordFile); err != nil {
		return nil, err
	}
	return &passwordFileConnector{
		baseDSN:      u.String(),
		passwordFile: passwordFile,
	}, nil
}

// Connect re-reads the password file, splices the current password into the
// base DSN, and opens a new connection via lib/pq. Called by database/sql for
// every new physical connection the pool creates, so a rotated password takes
// effect as the pool cycles connections.
func (c *passwordFileConnector) Connect(ctx context.Context) (driver.Conn, error) {
	password, err := readPasswordFile(c.passwordFile)
	if err != nil {
		return nil, err
	}
	dsn, err := dsnWithPassword(c.baseDSN, password)
	if err != nil {
		return nil, err
	}
	connector, err := pq.NewConnector(dsn)
	if err != nil {
		return nil, fmt.Errorf("build pq connector: %w", err)
	}
	return connector.Connect(ctx)
}

// Driver returns the underlying lib/pq driver, satisfying driver.Connector.
func (c *passwordFileConnector) Driver() driver.Driver { return &pq.Driver{} }

// readPasswordFile reads and trims the password from the mounted Secret file.
// A trailing newline is common in Secret files / here-docs, and Postgres
// passwords never legitimately carry surrounding whitespace, so trimming avoids
// a spurious auth failure. An empty file is treated as an error so a not-yet-
// synced mount surfaces clearly rather than attempting an empty-password auth.
func readPasswordFile(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read database password file %q: %w", path, err)
	}
	password := strings.TrimRight(string(raw), "\r\n")
	if password == "" {
		return "", fmt.Errorf("database password file %q is empty (Secret not synced yet?)", path)
	}
	return password, nil
}

// dsnWithPassword returns baseDSN (a URL-form DSN with a password-less
// userinfo) with password injected into the userinfo. url.UserPassword
// URL-encodes the password, so a raw password with special characters is
// escaped correctly for the postgres:// URL form.
func dsnWithPassword(baseDSN, password string) (string, error) {
	u, err := url.Parse(baseDSN)
	if err != nil {
		return "", fmt.Errorf("parse base database URL: %w", err)
	}
	username := ""
	if u.User != nil {
		username = u.User.Username()
	}
	u.User = url.UserPassword(username, password)
	return u.String(), nil
}
