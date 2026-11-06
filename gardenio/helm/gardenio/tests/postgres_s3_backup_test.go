package tests

import (
	"reflect"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// Regression guards for gd-vekJWxnNGG — off-node S3 backup for the in-box
// postgres StatefulSet (templates/postgres/s3-backup-cronjob.yaml).
//
// Distinct from postgres.backup and backup.postgres (backup_test.go above):
// both of those write to a second PVC, which on a single-node cluster is
// node-affine local storage bound to the same node as the live data — it
// protects against a corrupt data volume, not node loss. This CronJob writes
// no PVC at all: pg_dumpall -> emptyDir -> aws-cli sidecar -> S3.

func TestPostgresS3Backup_DefaultDisabled(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "templates/postgres/s3-backup-cronjob.yaml",
		"postgres.enabled=true",
	)
	rendersNothing(t, out, err)
}

func TestPostgresS3Backup_RequiresPostgresEnabled(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "templates/postgres/s3-backup-cronjob.yaml",
		"postgres.s3Backup.enabled=true",
		"postgres.s3Backup.bucket=test-bucket",
		"postgres.enabled=false",
	)
	rendersNothing(t, out, err)
}

func TestPostgresS3Backup_RendersCronJobNoPVC(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/postgres/s3-backup-cronjob.yaml",
		"postgres.enabled=true",
		"postgres.s3Backup.enabled=true",
		"postgres.s3Backup.bucket=test-bucket",
	)
	if !strings.Contains(out, "kind: CronJob") {
		t.Fatalf("expected a CronJob; got:\n%s", out)
	}
	if !strings.Contains(out, "name: test-postgres-s3-backup") {
		t.Errorf("expected the -s3-backup name; got:\n%s", out)
	}
	// The whole point of this template: no PVC, unlike its sibling backup
	// CronJobs — a PVC here would be node-affine and die with the node.
	if strings.Contains(out, "kind: PersistentVolumeClaim") {
		t.Errorf("expected NO PVC (node-affine local storage defeats the purpose of an off-node backup); got:\n%s", out)
	}
	if !strings.Contains(out, "emptyDir: {}") {
		t.Errorf("expected the dump volume to be an emptyDir; got:\n%s", out)
	}
}

func TestPostgresS3Backup_UsesPgDumpallAndAwsCliSidecar(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/postgres/s3-backup-cronjob.yaml",
		"postgres.enabled=true",
		"postgres.s3Backup.enabled=true",
		"postgres.s3Backup.bucket=test-bucket",
		"postgres.s3Backup.prefix=k3s-postgres",
	)
	if !strings.Contains(out, "pg_dumpall | zstd -T2 -3 -q -c") {
		t.Errorf("expected `pg_dumpall | zstd -T2 -3 -q -c` (whole-cluster dump, fast compressor: gd-w24jkWkith); got:\n%s", out)
	}
	// The dump's extension and the sidecar's upload glob must agree, or the
	// sidecar uploads nothing and still exits 0.
	if !strings.Contains(out, `pgdumpall-${STAMP}.sql.zst"`) {
		t.Errorf("expected the dump to be named .sql.zst; got:\n%s", out)
	}
	if !strings.Contains(out, `--include "pgdumpall-*.sql.zst"`) {
		t.Errorf("expected the upload glob to match .sql.zst; got:\n%s", out)
	}
	if strings.Contains(out, ".sql.gz") {
		t.Errorf("expected no .sql.gz left in the dump or upload glob; got:\n%s", out)
	}
	if !strings.Contains(out, ".partial") {
		t.Errorf("expected atomic .partial rename so a killed run leaves no truncated dump; got:\n%s", out)
	}
	if !strings.Contains(out, "amazon/aws-cli") {
		t.Errorf("expected the aws-cli sidecar image; got:\n%s", out)
	}
	if !strings.Contains(out, "s3://test-bucket/k3s-postgres/") {
		t.Errorf("expected the configured bucket/prefix in the upload command; got:\n%s", out)
	}
	// cp, not sync --delete: each run is a new timestamped object, retention
	// is an S3 lifecycle rule, not job-side pruning.
	if !strings.Contains(out, "aws s3 cp") {
		t.Errorf("expected `aws s3 cp` (not sync --delete); got:\n%s", out)
	}
	if strings.Contains(out, "aws s3 sync") {
		t.Errorf("expected no `aws s3 sync --delete` — each run uploads a new object, it does not mirror; got:\n%s", out)
	}
}

func TestPostgresS3Backup_HardenedPodSpec(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/postgres/s3-backup-cronjob.yaml",
		"postgres.enabled=true",
		"postgres.s3Backup.enabled=true",
		"postgres.s3Backup.bucket=test-bucket",
	)
	for _, want := range []string{
		"concurrencyPolicy: Forbid",
		"runAsNonRoot: true",
		"readOnlyRootFilesystem: true",
		"automountServiceAccountToken: false",
		"AWS_EC2_METADATA_DISABLED",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected hardened pod spec to contain %q; got:\n%s", want, out)
		}
	}
}

// Cost plan 2026-10-21-aws-cost-cuts Task B1 (gd-diP4TgbQv4): the uncompressed
// pg_dumpall must not stream across an AZ. The k3s deployment pins postgres to
// one node by its local PV, so the backup pod prefers that node by pod
// affinity. B1b (gd-RbR8vI9K5e): the preference did not hold, so the k3s
// values also REQUIRE postgres's zone and give the backup its own node
// selector (platform + amd64, no lifecycle=on-demand), so the zone's spot
// platform nodes can take a run when the postgres node is full.
func TestPostgresS3Backup_PostgresBackupAffinity(t *testing.T) {
	t.Parallel()
	out := helmTemplateShowOnlyWithValues(t,
		"templates/postgres/s3-backup-cronjob.yaml",
		[]string{"../../../deploy-values/gardenio-k3s.yaml"},
	)
	var cj struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			JobTemplate struct {
				Spec struct {
					Template struct {
						Spec struct {
							Affinity     map[string]any    `json:"affinity"`
							NodeSelector map[string]string `json:"nodeSelector"`
						} `json:"spec"`
					} `json:"template"`
				} `json:"spec"`
			} `json:"jobTemplate"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal([]byte(out), &cj); err != nil {
		t.Fatalf("unmarshal CronJob: %v\n%s", err, out)
	}
	if cj.Metadata.Name != "test-postgres-s3-backup" {
		t.Fatalf("expected the test-postgres-s3-backup CronJob; got %q:\n%s", cj.Metadata.Name, out)
	}
	var want map[string]any
	if err := yaml.Unmarshal([]byte(`
podAffinity:
  requiredDuringSchedulingIgnoredDuringExecution:
    - topologyKey: topology.kubernetes.io/zone
      labelSelector:
        matchLabels:
          app.kubernetes.io/component: postgres
          app.kubernetes.io/instance: test
  preferredDuringSchedulingIgnoredDuringExecution:
    - weight: 100
      podAffinityTerm:
        topologyKey: kubernetes.io/hostname
        labelSelector:
          matchLabels:
            app.kubernetes.io/component: postgres
            app.kubernetes.io/instance: test
`), &want); err != nil {
		t.Fatal(err)
	}
	spec := cj.Spec.JobTemplate.Spec.Template.Spec
	if got := spec.Affinity; !reflect.DeepEqual(got, want) {
		t.Errorf("backup pod affinity:\n got: %#v\nwant: %#v\nrender:\n%s", got, want, out)
	}
	// Its own selector, not postgres's: postgres needs lifecycle=on-demand
	// (its local PV must not live on a spot node); the backup must not have
	// it, or a full postgres node plus the required zone term strands it.
	wantSel := map[string]string{
		"gardenio.io/role":   "platform",
		"kubernetes.io/arch": "amd64",
	}
	if got := spec.NodeSelector; !reflect.DeepEqual(got, wantSel) {
		t.Errorf("backup pod nodeSelector:\n got: %v\nwant: %v", got, wantSel)
	}
}

// Chart defaults: no zone requirement (a cluster whose nodes carry no zone
// label would leave every run Pending), and with no s3Backup.nodeSelector the
// backup inherits postgres.nodeSelector.
func TestPostgresS3Backup_DefaultsPreferOnlyAndInheritNodeSelector(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/postgres/s3-backup-cronjob.yaml",
		"postgres.enabled=true",
		"postgres.s3Backup.enabled=true",
		"postgres.s3Backup.bucket=test-bucket",
		"postgres.nodeSelector.pg-node=yes",
	)
	if strings.Contains(out, "requiredDuringSchedulingIgnoredDuringExecution") {
		t.Errorf("expected no required affinity term by default; got:\n%s", out)
	}
	if !strings.Contains(out, "preferredDuringSchedulingIgnoredDuringExecution") {
		t.Errorf("expected the preferred same-node term by default; got:\n%s", out)
	}
	if !strings.Contains(out, "pg-node: \"yes\"") && !strings.Contains(out, "pg-node: yes") {
		t.Errorf("expected postgres.nodeSelector to be inherited; got:\n%s", out)
	}

	out = helmTemplate(t, "templates/postgres/s3-backup-cronjob.yaml",
		"postgres.enabled=true",
		"postgres.s3Backup.enabled=true",
		"postgres.s3Backup.bucket=test-bucket",
		"postgres.nodeSelector.pg-node=yes",
		"postgres.s3Backup.nodeSelector.backup-node=yes",
	)
	if strings.Contains(out, "pg-node") {
		t.Errorf("expected s3Backup.nodeSelector to replace postgres.nodeSelector; got:\n%s", out)
	}
	if !strings.Contains(out, "backup-node") {
		t.Errorf("expected s3Backup.nodeSelector in the render; got:\n%s", out)
	}
}
