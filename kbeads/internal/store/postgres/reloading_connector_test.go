package postgres

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writePasswordFile writes password to a temp file and returns its path.
func writePasswordFile(t *testing.T, password string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "password")
	if err := os.WriteFile(path, []byte(password), 0o600); err != nil {
		t.Fatalf("write password file: %v", err)
	}
	return path
}

func TestNewPasswordFileConnector_RejectsKeywordDSN(t *testing.T) {
	path := writePasswordFile(t, "secret")
	_, err := newPasswordFileConnector("host=localhost user=gardenio dbname=beads", path)
	if err == nil {
		t.Fatal("expected an error for a keyword-form DSN, got nil")
	}
	if !strings.Contains(err.Error(), "URL-form") {
		t.Errorf("expected URL-form rejection error, got: %v", err)
	}
}

func TestNewPasswordFileConnector_EmptyPath(t *testing.T) {
	_, err := newPasswordFileConnector("postgres://gardenio@localhost/beads", "")
	if err == nil {
		t.Fatal("expected an error for an empty password-file path, got nil")
	}
}

func TestNewPasswordFileConnector_MissingFile(t *testing.T) {
	_, err := newPasswordFileConnector("postgres://gardenio@localhost/beads",
		filepath.Join(t.TempDir(), "does-not-exist"))
	if err == nil {
		t.Fatal("expected an error for a missing password file, got nil")
	}
}

func TestNewPasswordFileConnector_EmptyFile(t *testing.T) {
	path := writePasswordFile(t, "")
	_, err := newPasswordFileConnector("postgres://gardenio@localhost/beads", path)
	if err == nil {
		t.Fatal("expected an error for an empty password file, got nil")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("expected empty-file error, got: %v", err)
	}
}

// TestPasswordFileConnector_StripsEmbeddedPassword verifies that any password
// baked into the base DSN (the stale $(BEADS_DATABASE_PASSWORD) substitution)
// is discarded — the file value is authoritative — while the username, host,
// database, and query params are preserved.
func TestPasswordFileConnector_StripsEmbeddedPassword(t *testing.T) {
	path := writePasswordFile(t, "irrelevant")
	c, err := newPasswordFileConnector(
		"postgres://gardenio:STALE_ENV_PASSWORD@rds.example.com:5432/beads?sslmode=require", path)
	if err != nil {
		t.Fatalf("newPasswordFileConnector: %v", err)
	}
	if strings.Contains(c.baseDSN, "STALE_ENV_PASSWORD") {
		t.Errorf("base DSN must not retain the embedded password; got %q", c.baseDSN)
	}
	for _, want := range []string{"gardenio", "rds.example.com:5432", "/beads", "sslmode=require"} {
		if !strings.Contains(c.baseDSN, want) {
			t.Errorf("base DSN must preserve %q; got %q", want, c.baseDSN)
		}
	}
}

// TestDSNWithPassword_RotationReflectsCurrentFileValue is the core
// rotation-tolerance assertion: composing the DSN after the file is rewritten
// yields the NEW password, so a fresh connection authenticates with the
// rotated credential without a process restart. Special characters in the
// password are URL-encoded so the postgres:// URL stays valid.
func TestDSNWithPassword_RotationReflectsCurrentFileValue(t *testing.T) {
	path := writePasswordFile(t, "old-password")
	c, err := newPasswordFileConnector("postgres://gardenio@rds.example.com:5432/beads?sslmode=require", path)
	if err != nil {
		t.Fatalf("newPasswordFileConnector: %v", err)
	}

	// Simulate what Connect() does: read the file, then compose the DSN.
	pw1, err := readPasswordFile(c.passwordFile)
	if err != nil {
		t.Fatalf("read password file: %v", err)
	}
	dsn1, err := dsnWithPassword(c.baseDSN, pw1)
	if err != nil {
		t.Fatalf("dsnWithPassword: %v", err)
	}
	if !strings.Contains(dsn1, "old-password") {
		t.Errorf("DSN must carry the initial password; got %q", dsn1)
	}

	// Rotate: overwrite the mounted file with a password containing chars
	// that MUST be URL-encoded (@ / : would otherwise corrupt the URL).
	const rotated = "n3w:p@ss/word"
	if err := os.WriteFile(path, []byte(rotated+"\n"), 0o600); err != nil {
		t.Fatalf("rotate password file: %v", err)
	}

	pw2, err := readPasswordFile(c.passwordFile)
	if err != nil {
		t.Fatalf("read rotated password file: %v", err)
	}
	if pw2 != rotated {
		t.Errorf("readPasswordFile must trim the trailing newline; got %q want %q", pw2, rotated)
	}
	dsn2, err := dsnWithPassword(c.baseDSN, pw2)
	if err != nil {
		t.Fatalf("dsnWithPassword (rotated): %v", err)
	}
	if strings.Contains(dsn2, "old-password") {
		t.Errorf("rotated DSN must not retain the old password; got %q", dsn2)
	}
	// The special characters must be percent-encoded in the URL userinfo.
	if !strings.Contains(dsn2, "n3w%3Ap%40ss%2Fword") {
		t.Errorf("rotated DSN must URL-encode the special-char password; got %q", dsn2)
	}
	// And it must still round-trip back to the raw password via the pq parser
	// path (sanity: the username/host survive too).
	for _, want := range []string{"gardenio", "rds.example.com:5432", "/beads", "sslmode=require"} {
		if !strings.Contains(dsn2, want) {
			t.Errorf("rotated DSN must preserve %q; got %q", want, dsn2)
		}
	}
}

// TestPasswordFileConnector_Driver verifies the connector reports the lib/pq
// driver (satisfies driver.Connector).
func TestPasswordFileConnector_Driver(t *testing.T) {
	path := writePasswordFile(t, "secret")
	c, err := newPasswordFileConnector("postgres://gardenio@localhost/beads", path)
	if err != nil {
		t.Fatalf("newPasswordFileConnector: %v", err)
	}
	if c.Driver() == nil {
		t.Error("Driver() must return a non-nil driver")
	}
}

// TestPasswordFileConnector_ConnectReadsFileEachCall confirms Connect re-reads
// the file (so a rotation is picked up per new connection). We can't reach a
// real Postgres here, so we assert on the failure surface: with a valid file
// the error is a dial/auth failure (file read succeeded), and after deleting
// the file the error is specifically the file-read error — proving Connect
// consults the file on every call rather than caching it.
func TestPasswordFileConnector_ConnectReadsFileEachCall(t *testing.T) {
	path := writePasswordFile(t, "secret")
	// Point at a port nothing is listening on so Connect fails fast on dial.
	c, err := newPasswordFileConnector("postgres://gardenio@127.0.0.1:1/beads?sslmode=disable&connect_timeout=1", path)
	if err != nil {
		t.Fatalf("newPasswordFileConnector: %v", err)
	}

	_, err = c.Connect(context.Background())
	if err == nil {
		t.Fatal("expected a connection error against a dead port, got nil")
	}
	if strings.Contains(err.Error(), "password file") {
		t.Errorf("with a readable file, the error should be a dial/auth failure, not a file-read error; got: %v", err)
	}

	// Remove the mounted file and reconnect: Connect must now fail on the
	// file read, proving it re-reads per call.
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove password file: %v", err)
	}
	_, err = c.Connect(context.Background())
	if err == nil {
		t.Fatal("expected an error after removing the password file, got nil")
	}
	if !strings.Contains(err.Error(), "password file") {
		t.Errorf("after removing the file, Connect must fail on the file read; got: %v", err)
	}
}
