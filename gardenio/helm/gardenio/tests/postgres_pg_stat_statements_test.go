package tests

import (
	"strings"
	"testing"
)

// gd-g25jihMHFs: postgres preloads pg_stat_statements so saturation can be
// attributed per query. args replace the image CMD, so `postgres` must lead.
func TestPostgres_PgStatStatementsPreload(t *testing.T) {
	t.Parallel()
	on := helmTemplate(t, "templates/postgres/statefulset.yaml", "postgres.enabled=true")
	for _, want := range []string{
		"args:\n            - postgres\n",
		"shared_preload_libraries=pg_stat_statements",
	} {
		if !strings.Contains(on, want) {
			t.Errorf("default render missing %q:\n%s", want, on)
		}
	}

	off := helmTemplate(t, "templates/postgres/statefulset.yaml",
		"postgres.enabled=true", "postgres.pgStatStatements.enabled=false")
	if strings.Contains(off, "shared_preload_libraries") {
		t.Errorf("pgStatStatements.enabled=false must not set shared_preload_libraries:\n%s", off)
	}
}
