package tests

import (
	"strings"
	"testing"
)

// The beads-migrate Job is a Helm pre-install/pre-upgrade hook, but the
// postgres-password Secret it consumes via env secretKeyRef is a normal
// release resource (an ExternalSecret target on ESO-backed envs). On a fresh /
// capacity-contended ephemeral namespace ESO can lose the race against the
// hook, and the migrate pod hard-fails with `secret "<ns>-postgres-password"
// not found` — reding the whole deploy (bug ka-0000003749).
//
// The fix: a wait-for-db-secret initContainer that mounts the Secret optional
// and blocks until the exact key file the migrate container needs appears.
// These tests pin that the gate is present and that it waits on the SAME
// Secret name + key the env secretKeyRef resolves to, across all postgres
// modes — a future edit that drifts the two apart would silently reintroduce
// the race.

func TestMigrateJob_WaitForSecret_InboxESO(t *testing.T) {
	t.Parallel()
	// In-box postgres + externalSecrets: the failing staging/poc case. The env
	// secretKeyRef reads key `password_url`, so the gate must wait on the
	// same key.
	out := helmTemplate(t, "templates/beads/migrate-job.yaml",
		"postgres.enabled=true",
		"externalSecrets.enabled=true",
		"externalSecrets.postgresPassword.remoteRef=gardenio/postgres",
	)
	if !strings.Contains(out, "name: wait-for-db-secret") {
		t.Fatalf("migrate-job must render the wait-for-db-secret initContainer when a DB Secret is consumed, got:\n%s", out)
	}
	if !strings.Contains(out, `secretName: test-postgres-password`) {
		t.Errorf("wait volume must mount the postgres-password Secret, got:\n%s", out)
	}
	if !strings.Contains(out, "optional: true") {
		t.Errorf("wait volume must mount the Secret optional so the pod schedules before ESO syncs it, got:\n%s", out)
	}
	if !strings.Contains(out, "/var/run/db-secret/password_url") {
		t.Errorf("wait gate must poll the SAME key (password_url) the env secretKeyRef consumes, got:\n%s", out)
	}
}

func TestMigrateJob_WaitForSecret_InboxNoESO(t *testing.T) {
	t.Parallel()
	// In-box postgres, no externalSecrets: chart-owned Secret, key `password`.
	out := helmTemplate(t, "templates/beads/migrate-job.yaml",
		"postgres.enabled=true",
		"externalSecrets.enabled=false",
	)
	if !strings.Contains(out, "name: wait-for-db-secret") {
		t.Fatalf("wait-for-db-secret initContainer missing, got:\n%s", out)
	}
	if !strings.Contains(out, "/var/run/db-secret/password") {
		t.Errorf("wait gate must poll key `password` for the chart-owned Secret, got:\n%s", out)
	}
	if !strings.Contains(out, "secretName: test-postgres-password") {
		t.Errorf("wait volume must mount test-postgres-password, got:\n%s", out)
	}
}

func TestMigrateJob_WaitForSecret_PostgresExternalCustomKey(t *testing.T) {
	t.Parallel()
	// postgresExternal with a custom secret name + key: the gate must track
	// the override, not the default.
	out := helmTemplate(t, "templates/beads/migrate-job.yaml",
		"postgres.enabled=false",
		"postgresExternal.enabled=true",
		"postgresExternal.host=rds.example.com",
		"postgresExternal.username=admin",
		"postgresExternal.passwordSecret.name=my-rds-creds",
		"postgresExternal.passwordSecret.key=db_password",
	)
	if !strings.Contains(out, "secretName: my-rds-creds") {
		t.Errorf("wait volume must mount the overridden Secret name, got:\n%s", out)
	}
	if !strings.Contains(out, "/var/run/db-secret/db_password") {
		t.Errorf("wait gate must poll the overridden key db_password, got:\n%s", out)
	}
}

func TestMigrateJob_WaitForSecret_ExternalRDS(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads/migrate-job.yaml",
		"postgres.enabled=false",
		"beads.externalRDS.enabled=true",
	)
	if !strings.Contains(out, "name: wait-for-db-secret") {
		t.Fatalf("wait-for-db-secret initContainer missing for externalRDS, got:\n%s", out)
	}
	if !strings.Contains(out, "secretName: test-rds-instance-password") {
		t.Errorf("wait volume must mount the externalRDS password Secret, got:\n%s", out)
	}
	if !strings.Contains(out, "/var/run/db-secret/password") {
		t.Errorf("wait gate must poll key `password` for externalRDS, got:\n%s", out)
	}
}

func TestMigrateJob_WaitForSecret_TimeoutConfigurable(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads/migrate-job.yaml",
		"postgres.enabled=true",
		"externalSecrets.enabled=false",
		"beads.migrationJob.waitForSecretTimeoutSeconds=42",
	)
	if !strings.Contains(out, "TIMEOUT=42") {
		t.Errorf("waitForSecretTimeoutSeconds must flow into the gate timeout, got:\n%s", out)
	}
}
