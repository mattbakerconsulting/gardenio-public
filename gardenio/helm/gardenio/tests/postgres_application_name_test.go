package tests

import (
	"strings"
	"testing"
)

// TestPostgresClients_ApplicationName pins the application_name every chart
// postgres client connects with on the k3s overlay, so pg_stat_activity and the
// postgres exporter attribute load to the client that sent it (gd-rXqkv4rSmG).
// libpq and lib/pq clients read PGAPPNAME; Prisma (litellm) and pgjdbc
// (keycloak) take it in the connection URL.
func TestPostgresClients_ApplicationName(t *testing.T) {
	t.Parallel()
	const pgAppName = "- name: PGAPPNAME\n"
	for _, tc := range []struct {
		template string
		set      []string
		want     []string
	}{
		{"templates/beads/deployment.yaml", nil,
			[]string{pgAppName, `value: "kbeads-server"`}},
		{"templates/beads/migrate-job.yaml", nil,
			[]string{pgAppName, `value: "kbeads-migrate"`}},
		{"templates/litellm/deployment.yaml", nil,
			[]string{"/litellm?application_name=litellm\n"}},
		{"templates/keycloak-runtime/statefulset.yaml", nil,
			[]string{`/keycloak?ApplicationName=keycloak"`}},
		{"templates/postgres/s3-backup-cronjob.yaml", []string{"postgres.s3Backup.enabled=true"},
			[]string{pgAppName, `value: "gardenio-backup"`}},
		{"templates/postgres/backup-cronjob.yaml", []string{"postgres.backup.enabled=true"},
			[]string{pgAppName, `value: "gardenio-backup"`}},
		{"templates/backup/postgres-cronjob.yaml", []string{"backup.postgres.enabled=true"},
			[]string{pgAppName, `value: "gardenio-backup"`}},
	} {
		t.Run(tc.template, func(t *testing.T) {
			out := helmTemplateShowOnlyWithValues(t, tc.template, []string{k3sOverlay}, tc.set...)
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("%s: want %q in the render, not found", tc.template, w)
				}
			}
		})
	}
}
