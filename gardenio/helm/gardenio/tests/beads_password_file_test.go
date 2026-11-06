package tests

import (
	"strings"
	"testing"
)

// Rotation-tolerant DB password via a mounted Secret FILE (bug ka-0000004150,
// fix #2 of P0 postmortem ka-0000006811). A secretKeyRef env var is frozen at
// container start and never picks up an RDS master-password rotation, so the
// daemon CrashLooped fleet-wide on pq 28P01. beads.databasePasswordFile.enabled
// switches the beads Deployment to mount the password Secret as a live-updating
// volume and hand the daemon BEADS_DATABASE_PASSWORD_FILE, which it re-reads per
// connection. These tests pin that wiring.

// envValue returns the plain `value:` rendered for the named env var (used for
// env vars whose value is a literal, not a secretKeyRef).
func envValue(t *testing.T, manifest, envName string) string {
	t.Helper()
	lines := strings.Split(manifest, "\n")
	for i, ln := range lines {
		if strings.TrimSpace(ln) == "- name: "+envName {
			for _, follow := range lines[i+1:] {
				ts := strings.TrimSpace(follow)
				if strings.HasPrefix(ts, "value: ") {
					return strings.Trim(strings.TrimSpace(strings.TrimPrefix(ts, "value:")), `"`)
				}
				if strings.HasPrefix(ts, "- name: ") {
					break
				}
			}
		}
	}
	t.Fatalf("no value found for env %q in:\n%s", envName, manifest)
	return ""
}

// TestBeadsPasswordFile_DefaultOff: with the flag off (the default), the daemon
// keeps the legacy env-var password path — NO behavior change.
func TestBeadsPasswordFile_DefaultOff(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads/deployment.yaml",
		"postgres.enabled=false",
		"beads.externalRDS.enabled=true",
		"beads.externalRDS.host=rds.example.com",
	)
	if strings.Contains(out, "BEADS_DATABASE_PASSWORD_FILE") {
		t.Error("BEADS_DATABASE_PASSWORD_FILE must NOT render when databasePasswordFile.enabled is false (default)")
	}
	if !strings.Contains(out, "- name: BEADS_DATABASE_PASSWORD\n") {
		t.Error("legacy BEADS_DATABASE_PASSWORD env var must still render when the file flag is off")
	}
	if !strings.Contains(out, "$(BEADS_DATABASE_PASSWORD)") {
		t.Error("BEADS_DATABASE_URL must still carry the $(BEADS_DATABASE_PASSWORD) substitution when the file flag is off")
	}
	if strings.Contains(out, "name: db-secret") {
		t.Error("db-secret volume must NOT render when databasePasswordFile.enabled is false")
	}
}

// TestBeadsPasswordFile_EnabledExternalRDS: with the flag on, the daemon gets
// BEADS_DATABASE_PASSWORD_FILE, a password-LESS BEADS_DATABASE_URL, and a
// live-updating Secret volume mounted read-only — the rotation-tolerant path.
func TestBeadsPasswordFile_EnabledExternalRDS(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads/deployment.yaml",
		"beads.databasePasswordFile.enabled=true",
		"postgres.enabled=false",
		"beads.externalRDS.enabled=true",
		"beads.externalRDS.host=rds.example.com",
	)

	// No secretKeyRef password env var — the file is authoritative.
	if strings.Contains(out, "- name: BEADS_DATABASE_PASSWORD\n") {
		t.Error("BEADS_DATABASE_PASSWORD env var must NOT render in file mode (ka-0000004150)")
	}
	// The daemon reads the password from the mounted file.
	if got := envValue(t, out, "BEADS_DATABASE_PASSWORD_FILE"); got != "/var/run/db-secret/password" {
		t.Errorf("BEADS_DATABASE_PASSWORD_FILE = %q, want /var/run/db-secret/password", got)
	}
	// The connection string must NOT bake in any password (no substitution).
	url := envValue(t, out, "BEADS_DATABASE_URL")
	if strings.Contains(url, "BEADS_DATABASE_PASSWORD") || strings.Contains(url, ":$(") {
		t.Errorf("BEADS_DATABASE_URL must be password-less in file mode, got %q", url)
	}
	if !strings.HasPrefix(url, "postgres://gardenio_test@rds.example.com") {
		t.Errorf("BEADS_DATABASE_URL must carry user@host with no password, got %q", url)
	}

	// The password Secret must be mounted as a volume (which live-updates),
	// read-only, and optional (so the pod schedules before ESO syncs it).
	if !strings.Contains(out, "- name: db-secret") {
		t.Error("db-secret volume mount must render in file mode")
	}
	if !strings.Contains(out, "mountPath: /var/run/db-secret") {
		t.Error("db-secret must mount at /var/run/db-secret")
	}
	vol := afterMarker(out, "- name: db-secret\n          secret:")
	if !strings.Contains(vol, "secretName: test-rds-instance-password") {
		t.Errorf("db-secret volume must reference the raw-password Secret, got:\n%s", vol)
	}
	if !strings.Contains(vol, "optional: true") {
		t.Errorf("db-secret volume must be optional so a not-yet-synced Secret does not wedge the pod, got:\n%s", vol)
	}
}

// TestBeadsPasswordFile_MountsRawKeyForPostgresExternal: postgresExternal splits
// the raw `key` (password) from the url-encoded `urlKey` (password_url). The
// FILE mount must use the RAW key — the daemon URL-encodes the password itself,
// so mounting the pre-encoded copy would double-encode and fail auth.
func TestBeadsPasswordFile_MountsRawKeyForPostgresExternal(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads/deployment.yaml",
		"beads.databasePasswordFile.enabled=true",
		"postgres.enabled=false",
		"postgresExternal.enabled=true",
		"postgresExternal.host=rds.example.com",
		"postgresExternal.username=gardenioadmin",
		"postgresExternal.passwordSecret.key=password",
		"postgresExternal.passwordSecret.urlKey=password_url",
	)
	if got := envValue(t, out, "BEADS_DATABASE_PASSWORD_FILE"); got != "/var/run/db-secret/password" {
		t.Errorf("file mode must mount the RAW key 'password' (not the url-encoded 'password_url'); BEADS_DATABASE_PASSWORD_FILE = %q", got)
	}
	if strings.Contains(out, "/var/run/db-secret/password_url") {
		t.Error("file mode must NOT mount the url-encoded key — the daemon URL-encodes the raw password itself (ka-0000004150)")
	}
	if !strings.Contains(out, "secretName: test-postgres-external-password") {
		t.Error("db-secret volume must reference the postgresExternal password Secret")
	}
}

// TestBeadsPasswordFile_CustomMountPath: the mount path is overridable and the
// *_FILE env tracks it.
func TestBeadsPasswordFile_CustomMountPath(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads/deployment.yaml",
		"beads.databasePasswordFile.enabled=true",
		"beads.databasePasswordFile.mountPath=/etc/db",
		"postgres.enabled=false",
		"beads.externalRDS.enabled=true",
		"beads.externalRDS.host=rds.example.com",
	)
	if got := envValue(t, out, "BEADS_DATABASE_PASSWORD_FILE"); got != "/etc/db/password" {
		t.Errorf("BEADS_DATABASE_PASSWORD_FILE must track the custom mountPath, got %q", got)
	}
	if !strings.Contains(out, "mountPath: /etc/db") {
		t.Error("db-secret volumeMount must honor the custom mountPath")
	}
}

// afterMarker returns the slice of out starting at marker (empty if absent),
// scoped to the next ~8 lines so a Contains check stays within one block.
func afterMarker(out, marker string) string {
	i := strings.Index(out, marker)
	if i < 0 {
		return ""
	}
	rest := out[i:]
	lines := strings.SplitN(rest, "\n", 8)
	return strings.Join(lines, "\n")
}
