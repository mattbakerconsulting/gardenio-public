package tests

import (
	"strings"
	"testing"
)

// Regression guards for ka-0000008198 — Gardenio-in-a-Box backup/DR templates:
//   - templates/backup/postgres-cronjob.yaml: pg_dumpall whole-cluster dump
//   - templates/backup/nats-cronjob.yaml: optional `nats account backup`
//   - templates/backup/storageclass-retain.yaml: reclaimPolicy: Retain SC
//
// All three are off by default and gated behind values flags (+ the
// corresponding store being enabled). The in-box overlay turns them on.

// --- postgres pg_dumpall CronJob ---

// rendersNothing asserts a --show-only render produced no objects. helm exits
// non-zero with "could not find template" when a gated template renders empty.
func rendersNothing(t *testing.T, out string, err error) {
	t.Helper()
	trimmed := strings.TrimSpace(out)
	if err == nil && trimmed != "" {
		t.Errorf("expected the template to render nothing; got:\n%s", out)
		return
	}
	if trimmed != "" && !strings.Contains(trimmed, "could not find template") {
		t.Errorf("expected no rendered objects; got:\n%s", out)
	}
}

func TestBackup_PostgresDumpall_DefaultDisabled(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "templates/backup/postgres-cronjob.yaml",
		"postgres.enabled=true",
	)
	rendersNothing(t, out, err)
}

func TestBackup_PostgresDumpall_RequiresPostgresEnabled(t *testing.T) {
	t.Parallel()
	// Flag on but the in-cluster StatefulSet off -> nothing renders (the job
	// targets the in-box Service, not external RDS).
	out, err := helmTemplateAllowFail(t, "templates/backup/postgres-cronjob.yaml",
		"backup.postgres.enabled=true",
		"postgres.enabled=false",
	)
	rendersNothing(t, out, err)
}

func TestBackup_PostgresDumpall_RendersCronJobAndPVC(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/backup/postgres-cronjob.yaml",
		"backup.postgres.enabled=true",
		"postgres.enabled=true",
	)
	if !strings.Contains(out, "kind: CronJob") {
		t.Errorf("expected a CronJob; got:\n%s", out)
	}
	if !strings.Contains(out, "kind: PersistentVolumeClaim") {
		t.Errorf("expected a dedicated backup PVC; got:\n%s", out)
	}
	if !strings.Contains(out, "name: test-postgres-dumpall") {
		t.Errorf("expected the -dumpall name; got:\n%s", out)
	}
}

func TestBackup_PostgresDumpall_UsesPgDumpallNotPgDump(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/backup/postgres-cronjob.yaml",
		"backup.postgres.enabled=true",
		"postgres.enabled=true",
	)
	// The whole point: pg_dumpall (all DBs + roles), gzipped, atomic rename.
	if !strings.Contains(out, "pg_dumpall | gzip -c") {
		t.Errorf("expected `pg_dumpall | gzip -c` (whole-cluster dump); got:\n%s", out)
	}
	if !strings.Contains(out, ".partial") {
		t.Errorf("expected atomic .partial rename so a killed run leaves no truncated dump; got:\n%s", out)
	}
	// Day-based retention prune via find -mtime.
	if !strings.Contains(out, "-mtime +") {
		t.Errorf("expected find -mtime retention prune; got:\n%s", out)
	}
}

func TestBackup_PostgresDumpall_HardenedPodSpec(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/backup/postgres-cronjob.yaml",
		"backup.postgres.enabled=true",
		"postgres.enabled=true",
	)
	for _, want := range []string{
		"concurrencyPolicy: Forbid",
		"runAsNonRoot: true",
		"readOnlyRootFilesystem: true",
		"automountServiceAccountToken: false",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected hardened pod spec to contain %q; got:\n%s", want, out)
		}
	}
}

// --- NATS account backup CronJob (optional) ---

func TestBackup_Nats_DefaultDisabled(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "templates/backup/nats-cronjob.yaml",
		"nats.enabled=true",
	)
	rendersNothing(t, out, err)
}

func TestBackup_Nats_RequiresNatsEnabled(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "templates/backup/nats-cronjob.yaml",
		"backup.nats.enabled=true",
		"nats.enabled=false",
	)
	rendersNothing(t, out, err)
}

func TestBackup_Nats_UsesAccountBackup(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/backup/nats-cronjob.yaml",
		"backup.nats.enabled=true",
		"nats.enabled=true",
	)
	if !strings.Contains(out, "kind: CronJob") {
		t.Fatalf("expected a CronJob; got:\n%s", out)
	}
	// Must use `nats account backup`, NOT a cp -r of the live JetStream dir.
	if !strings.Contains(out, "nats account backup") {
		t.Errorf("expected `nats account backup`; got:\n%s", out)
	}
	if !strings.Contains(out, "nats://test-nats:4222") {
		t.Errorf("expected NATS_URL to derive the in-cluster service; got:\n%s", out)
	}
}

// --- reclaimPolicy: Retain StorageClass ---

func TestBackup_StorageClassRetain_DefaultDisabled(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "templates/backup/storageclass-retain.yaml")
	rendersNothing(t, out, err)
}

func TestBackup_StorageClassRetain_RendersRetain(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/backup/storageclass-retain.yaml",
		"storageClassRetain.create=true",
	)
	if !strings.Contains(out, "kind: StorageClass") {
		t.Fatalf("expected a StorageClass; got:\n%s", out)
	}
	if !strings.Contains(out, "reclaimPolicy: Retain") {
		t.Errorf("expected reclaimPolicy: Retain so data survives uninstall; got:\n%s", out)
	}
	if !strings.Contains(out, "name: local-path-retain") {
		t.Errorf("expected the local-path-retain name; got:\n%s", out)
	}
	// keep annotation so a release that stops templating it orphans the SC.
	if !strings.Contains(out, `"helm.sh/resource-policy": keep`) {
		t.Errorf("expected helm.sh/resource-policy: keep on the cluster-scoped SC; got:\n%s", out)
	}
}
