package tests

import (
	"strings"
	"testing"
)

// External postgres (RDS master credentials) — ka-0000008013.
//
// The chart previously knew only `postgres.enabled` (in-cluster) and
// `beads.externalRDS.enabled` (per-instance provisioning). The
// customer-poc POC overlay set `postgresExternal.*` keys the chart did
// not implement, so BEADS_DATABASE_URL rendered empty and the beads
// daemon crashlooped. These tests pin the implementation.

func TestPostgresExternal_DefaultDisabled(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads/deployment.yaml")
	if strings.Contains(out, "postgres-external-password") {
		t.Errorf("postgres-external-password Secret ref must NOT appear when postgresExternal.enabled is false (the default), got:\n%s", out)
	}
}

func TestPostgresExternal_EnabledWiresDatabaseURL(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads/deployment.yaml",
		"postgres.enabled=false",
		"postgresExternal.enabled=true",
		"postgresExternal.host=rds.example.com",
		"postgresExternal.username=gardenioadmin",
	)
	want := "postgres://gardenioadmin:$(BEADS_DATABASE_PASSWORD)@rds.example.com:5432/gardenio?sslmode=require"
	if !strings.Contains(out, want) {
		t.Errorf("BEADS_DATABASE_URL not wired correctly\nwant: %s\ngot:\n%s", want, out)
	}
}

func TestPostgresExternal_EnabledWiresPasswordSecretRef(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads/deployment.yaml",
		"postgres.enabled=false",
		"postgresExternal.enabled=true",
		"postgresExternal.host=rds.example.com",
		"postgresExternal.username=gardenioadmin",
	)
	if !strings.Contains(out, "name: test-postgres-external-password") {
		t.Errorf("default password Secret name (<release>-postgres-external-password) not wired, got:\n%s", out)
	}
	if !strings.Contains(out, "key: password") {
		t.Errorf("default password Secret key not wired, got:\n%s", out)
	}
}

func TestPostgresExternal_PasswordSecretNameOverride(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads/deployment.yaml",
		"postgres.enabled=false",
		"postgresExternal.enabled=true",
		"postgresExternal.host=rds.example.com",
		"postgresExternal.username=gardenioadmin",
		"postgresExternal.passwordSecret.name=my-rds-creds",
		"postgresExternal.passwordSecret.key=db_password",
	)
	if !strings.Contains(out, "name: my-rds-creds") {
		t.Errorf("password Secret name override not applied, got:\n%s", out)
	}
	if !strings.Contains(out, "key: db_password") {
		t.Errorf("password Secret key override not applied, got:\n%s", out)
	}
}

func TestPostgresExternal_PortAndSslmodeOverride(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads/deployment.yaml",
		"postgres.enabled=false",
		"postgresExternal.enabled=true",
		"postgresExternal.host=rds.example.com",
		"postgresExternal.port=6432",
		"postgresExternal.username=gardenioadmin",
		"postgresExternal.database=beads_prod",
		"postgresExternal.sslmode=verify-full",
	)
	want := "postgres://gardenioadmin:$(BEADS_DATABASE_PASSWORD)@rds.example.com:6432/beads_prod?sslmode=verify-full"
	if !strings.Contains(out, want) {
		t.Errorf("BEADS_DATABASE_URL custom port/db/sslmode not wired\nwant: %s\ngot:\n%s", want, out)
	}
}

func TestPostgresExternal_MigrateJobRegistersPreInstallHook(t *testing.T) {
	t.Parallel()
	// With postgresExternal.enabled=true the migrate-job MUST register
	// both pre-install AND pre-upgrade hooks so the schema is created on
	// first install. Without pre-install, helm installs the beads
	// Deployment and the daemon's m.Up() races against a non-existent
	// schema on a fresh database.
	out := helmTemplate(t, "templates/beads/migrate-job.yaml",
		"postgres.enabled=false",
		"postgresExternal.enabled=true",
		"postgresExternal.host=rds.example.com",
		"postgresExternal.username=gardenioadmin",
	)
	if !strings.Contains(out, `"helm.sh/hook": "pre-install,pre-upgrade"`) {
		t.Errorf("migrate-job must register pre-install,pre-upgrade hooks when postgresExternal.enabled, got:\n%s", out)
	}
}

func TestPostgresExternal_MigrateJobWiresDatabaseEnv(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads/migrate-job.yaml",
		"postgres.enabled=false",
		"postgresExternal.enabled=true",
		"postgresExternal.host=rds.example.com",
		"postgresExternal.username=gardenioadmin",
	)
	wantURL := "postgres://gardenioadmin:$(BEADS_DATABASE_PASSWORD)@rds.example.com:5432/gardenio?sslmode=require"
	if !strings.Contains(out, wantURL) {
		t.Errorf("migrate-job BEADS_DATABASE_URL not wired, got:\n%s", out)
	}
	if !strings.Contains(out, "name: test-postgres-external-password") {
		t.Errorf("migrate-job password Secret ref not wired, got:\n%s", out)
	}
}

// secretKeyForEnv extracts the secretKeyRef `key:` rendered for the named env
// var in a deployment/job manifest. It scans for `- name: <envName>` and
// returns the first `key:` value that follows, so tests can assert which
// Secret key a specific env var pulls from.
func secretKeyForEnv(t *testing.T, manifest, envName string) string {
	t.Helper()
	lines := strings.Split(manifest, "\n")
	for i, ln := range lines {
		if strings.TrimSpace(ln) == "- name: "+envName {
			for _, follow := range lines[i+1:] {
				ts := strings.TrimSpace(follow)
				if strings.HasPrefix(ts, "key: ") {
					return strings.TrimSpace(strings.TrimPrefix(ts, "key:"))
				}
				// Stop at the next env entry to avoid bleeding into a sibling.
				if strings.HasPrefix(ts, "- name: ") {
					break
				}
			}
		}
	}
	t.Fatalf("no secretKeyRef key found for env %q in:\n%s", envName, manifest)
	return ""
}

// ka-0000005248: the litellm db-init initContainer's PGPASSWORD needs the
// LITERAL password (psql does not URL-decode PGPASSWORD), while the
// connection-string env vars (BEADS_DATABASE_URL / litellm DATABASE_URL) need
// the URL-encoded copy. `urlKey` routes the URL consumers; `key` stays literal.

func TestPostgresExternal_URLKey_SplitsLiteralVsEncoded(t *testing.T) {
	t.Parallel()
	// litellm db-init PGPASSWORD must read the literal `key`, while
	// LITELLM_DATABASE_PASSWORD (embedded in DATABASE_URL) must read `urlKey`.
	out := helmTemplate(t, "templates/litellm/deployment.yaml",
		"postgres.enabled=false",
		"postgresExternal.enabled=true",
		"postgresExternal.host=rds.example.com",
		"postgresExternal.username=gardenioadmin",
		"litellm.enabled=true",
		"postgresExternal.passwordSecret.key=password",
		"postgresExternal.passwordSecret.urlKey=password_url",
	)
	if got := secretKeyForEnv(t, out, "PGPASSWORD"); got != "password" {
		t.Errorf("PGPASSWORD (db-init) must read literal key 'password', got %q", got)
	}
	if got := secretKeyForEnv(t, out, "LITELLM_DATABASE_PASSWORD"); got != "password_url" {
		t.Errorf("LITELLM_DATABASE_PASSWORD (DATABASE_URL) must read urlKey 'password_url', got %q", got)
	}
}

func TestPostgresExternal_URLKey_BeadsURLConsumerUsesURLKey(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads/deployment.yaml",
		"postgres.enabled=false",
		"postgresExternal.enabled=true",
		"postgresExternal.host=rds.example.com",
		"postgresExternal.username=gardenioadmin",
		"postgresExternal.passwordSecret.urlKey=password_url",
	)
	if got := secretKeyForEnv(t, out, "BEADS_DATABASE_PASSWORD"); got != "password_url" {
		t.Errorf("BEADS_DATABASE_PASSWORD (embedded in BEADS_DATABASE_URL) must read urlKey 'password_url', got %q", got)
	}
}

func TestPostgresExternal_URLKey_MigrateJobUsesURLKey(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads/migrate-job.yaml",
		"postgres.enabled=false",
		"postgresExternal.enabled=true",
		"postgresExternal.host=rds.example.com",
		"postgresExternal.username=gardenioadmin",
		"postgresExternal.passwordSecret.urlKey=password_url",
	)
	if got := secretKeyForEnv(t, out, "BEADS_DATABASE_PASSWORD"); got != "password_url" {
		t.Errorf("migrate-job BEADS_DATABASE_PASSWORD must read urlKey 'password_url', got %q", got)
	}
	// The wait-for-db-secret initContainer must gate on the same key.
	if !strings.Contains(out, "/var/run/db-secret/password_url") {
		t.Errorf("migrate-job wait-for-db-secret initContainer must gate on key 'password_url', got:\n%s", out)
	}
}

func TestPostgresExternal_URLKey_DefaultsToLiteralKey(t *testing.T) {
	t.Parallel()
	// Backward-compat: with urlKey unset, every consumer falls back to `key`
	// so single-key Secrets keep working exactly as before.
	out := helmTemplate(t, "templates/beads/deployment.yaml",
		"postgres.enabled=false",
		"postgresExternal.enabled=true",
		"postgresExternal.host=rds.example.com",
		"postgresExternal.username=gardenioadmin",
	)
	if got := secretKeyForEnv(t, out, "BEADS_DATABASE_PASSWORD"); got != "password" {
		t.Errorf("with urlKey unset, BEADS_DATABASE_PASSWORD must fall back to key 'password', got %q", got)
	}
}

// P0 postmortem ka-0000006811: the chart's postgresPassword ExternalSecret syncs
// the RDS-MANAGED master password, which AWS RDS/Secrets Manager rotates
// AUTOMATICALLY on its own schedule. It was categorized `static` (6h refresh),
// whose rationale — "rotates only as a manual operator step" — is FALSE for the
// RDS master. When RDS rotated, the in-cluster Secret lagged up to 6h, the beads
// daemon authenticated with the stale password (baked into BEADS_DATABASE_URL at
// container start, an env var that never live-updates), and CrashLooped on pq
// 28P01 fleet-wide (~30min on 2026-07-25). The fix reclassifies it to the new
// `rotated-db` category (5m). These tests pin the short interval so a future
// edit cannot silently drop it back into a slow bucket and reintroduce the
// outage.

// refreshIntervalForES extracts the `refreshInterval:` value from the first
// ExternalSecret doc whose metadata name matches wantName in a full-chart render.
func refreshIntervalForES(t *testing.T, rendered, wantName string) string {
	t.Helper()
	for doc := range strings.SplitSeq(rendered, "\n---") {
		if !strings.Contains(doc, "kind: ExternalSecret") {
			continue
		}
		if !strings.Contains(doc, "name: "+wantName+"\n") {
			continue
		}
		for ln := range strings.SplitSeq(doc, "\n") {
			ts := strings.TrimSpace(ln)
			if v, ok := strings.CutPrefix(ts, "refreshInterval:"); ok {
				return strings.TrimSpace(v)
			}
		}
	}
	t.Fatalf("no ExternalSecret named %q with a refreshInterval found in:\n%s", wantName, rendered)
	return ""
}

func TestPostgresPasswordES_ShortRefreshForRDSRotation(t *testing.T) {
	t.Parallel()
	// Render the chart with the postgres-password ExternalSecret active (ESO
	// backing store + a remoteRef, the prod posture). Full render, not
	// --show-only, so the doc-name match sees the real metadata block.
	out := helmTemplate(t, "",
		"externalSecrets.enabled=true",
		"externalSecrets.secretStoreName=aws-secretsmanager",
		"externalSecrets.postgresPassword.remoteRef=rds!cluster-abc",
		"externalSecrets.postgresPassword.property=password",
	)
	got := refreshIntervalForES(t, out, "test-postgres-password")
	if got != "5m" {
		t.Errorf("postgres-password ExternalSecret must refresh every 5m (category rotated-db) so an RDS auto-rotation propagates in minutes, not the old 6h static window that caused ka-0000006811; got %q", got)
	}
	// Guard against a silent regression back to the slow buckets that caused
	// the outage (6h static, or the 1h flat fallback).
	if got == "6h" || got == "1h" {
		t.Errorf("postgres-password ExternalSecret refreshInterval %q is a slow bucket — the RDS-managed master auto-rotates and needs the rotated-db (5m) cadence (ka-0000006811)", got)
	}
}

func TestPostgresPasswordES_RotatedDBCategoryOverridable(t *testing.T) {
	t.Parallel()
	// The interval must resolve through the rotated-db category knob, so an
	// operator can retune the whole bucket from a per-env values file.
	out := helmTemplate(t, "",
		"externalSecrets.enabled=true",
		"externalSecrets.secretStoreName=aws-secretsmanager",
		"externalSecrets.postgresPassword.remoteRef=rds!cluster-abc",
		"externalSecrets.postgresPassword.property=password",
		"externalSecrets.refreshIntervals.rotated-db=90s",
	)
	if got := refreshIntervalForES(t, out, "test-postgres-password"); got != "90s" {
		t.Errorf("postgres-password ExternalSecret must honor externalSecrets.refreshIntervals.rotated-db override, got %q", got)
	}
}
