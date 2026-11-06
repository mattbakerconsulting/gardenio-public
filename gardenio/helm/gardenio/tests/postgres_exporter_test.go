package tests

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// gd-cawNAc0iU7: postgres_exporter sidecar behind postgres.exporter.enabled,
// off by default and on in the k3s overlay.

const (
	pgStatefulSet       = "templates/postgres/statefulset.yaml"
	pgExporterConfigMap = "templates/postgres/exporter-configmap.yaml"
	pgExporterSecret    = "templates/postgres/exporter-secret.yaml"
)

func TestPostgresExporter_OffByDefault(t *testing.T) {
	out := helmTemplate(t, pgStatefulSet, "postgres.enabled=true")
	for _, banned := range []string{"name: exporter", "prometheus.io/scrape", "exporter-config"} {
		if strings.Contains(out, banned) {
			t.Errorf("default postgres render must not contain %q:\n%s", banned, out)
		}
	}
	// postgres.enabled=true so the exporter gate, not the postgres one, is what
	// keeps them out.
	for _, f := range []string{pgExporterConfigMap, pgExporterSecret} {
		got, err := helmTemplateAllowFail(t, f, "postgres.enabled=true")
		if err != nil && !strings.Contains(got, "could not find template") {
			t.Fatalf("helm template %s: %v\n%s", f, err, got)
		}
		if strings.Contains(got, "kind:") {
			t.Errorf("%s must render nothing by default, got:\n%s", f, got)
		}
	}
}

func TestPostgresExporter_K3sOverlayEnablesSidecar(t *testing.T) {
	out := helmTemplateShowOnlyWithValues(t, pgStatefulSet, []string{k3sOverlay})
	for _, want := range []string{
		"- name: exporter-setup\n",
		"- name: exporter\n",
		`prometheus.io/scrape: "true"`,
		`prometheus.io/port: "9187"`,
		"containerPort: 9187",
		"quay.io/prometheuscommunity/postgres-exporter:v0.20.1@sha256:",
		"--auto-discover-databases",
		"--extend.query-path=/etc/postgres-exporter/queries.yaml",
		"--no-collector.settings",
		"--collector.postmaster", // the board's restart annotation
	} {
		if !strings.Contains(out, want) {
			t.Errorf("k3s postgres StatefulSet missing %q:\n%s", want, out)
		}
	}
}

// The exporter reads its password from the chart-made Secret over localhost,
// carries small requests and no CPU limit, and has no readinessProbe: a pod is
// Ready only when every container is, so a slow exporter probe would drop the
// postgres Service's only endpoint.
func TestPostgresExporter_ContainerSpec(t *testing.T) {
	out := helmTemplate(t, pgStatefulSet, "postgres.enabled=true", "postgres.exporter.enabled=true")
	exporter := containerBlock(t, out, "exporter")
	for _, want := range []string{
		`value: "127.0.0.1:5432/beads?sslmode=disable&application_name=postgres_exporter"`,
		"name: DATA_SOURCE_PASS\n              valueFrom:\n                secretKeyRef:\n                  name: test-postgres-exporter",
		"cpu: 25m",
		"memory: 64Mi",
	} {
		if !strings.Contains(exporter, want) {
			t.Errorf("exporter container missing %q:\n%s", want, exporter)
		}
	}
	if strings.Contains(exporter, "readinessProbe") {
		t.Errorf("exporter must have no readinessProbe:\n%s", exporter)
	}
	limits := exporter[strings.Index(exporter, "limits:"):strings.Index(exporter, "requests:")]
	if strings.Contains(limits, "cpu:") {
		t.Errorf("exporter must have no CPU limit, got limits:\n%s", limits)
	}

	setup := containerBlock(t, out, "exporter-setup")
	for _, want := range []string{"name: EXPORTER_PASSWORD", "exec sleep infinity", "name: test-postgres-exporter"} {
		if !strings.Contains(setup, want) {
			t.Errorf("exporter-setup container missing %q:\n%s", want, setup)
		}
	}
	// The password reaches psql through its environment, never argv (ps).
	if strings.Contains(setup, "-v password=") || strings.Contains(setup, "readinessProbe") {
		t.Errorf("exporter-setup must not pass the password on argv or carry a readinessProbe:\n%s", setup)
	}
}

func TestPostgresExporter_SecretIsKept(t *testing.T) {
	out := helmTemplate(t, pgExporterSecret, "postgres.enabled=true", "postgres.exporter.enabled=true")
	for _, want := range []string{"name: test-postgres-exporter", `"helm.sh/resource-policy": keep`, "password: "} {
		if !strings.Contains(out, want) {
			t.Errorf("exporter Secret missing %q:\n%s", want, out)
		}
	}
}

// setup.sql makes a read-only pg_monitor role, and must never let its own
// ALTER ROLE ... PASSWORD land in pg_stat_statements (utility statements are
// stored verbatim) or the server log; queries.yaml must parse and stay bounded.
func TestPostgresExporter_ConfigMap(t *testing.T) {
	out := helmTemplate(t, pgExporterConfigMap, "postgres.enabled=true", "postgres.exporter.enabled=true")
	var cm struct {
		Data map[string]string `yaml:"data"`
	}
	if err := yaml.Unmarshal([]byte(out), &cm); err != nil {
		t.Fatalf("ConfigMap is not YAML: %v\n%s", err, out)
	}

	setup := cm.Data["setup.sql"]
	for _, want := range []string{
		"GRANT pg_monitor TO",
		"default_transaction_read_only = on",
		"statement_timeout = '10s'",
		"NOSUPERUSER",
		"CREATE EXTENSION IF NOT EXISTS pg_stat_statements",
		"SECURITY DEFINER",
	} {
		if !strings.Contains(setup, want) {
			t.Errorf("setup.sql missing %q:\n%s", want, setup)
		}
	}
	alter := strings.Index(setup, "PASSWORD :'password'")
	if alter < 0 {
		t.Fatalf("setup.sql has no ALTER ROLE ... PASSWORD:\n%s", setup)
	}
	for _, before := range []string{
		`\getenv password EXPORTER_PASSWORD`,
		"SET pg_stat_statements.track_utility = off",
		"SET log_statement = 'none'",
		"SET log_min_duration_statement = -1",
		"SET log_min_error_statement = panic", // a failed ALTER logs its STATEMENT text
	} {
		if i := strings.Index(setup, before); i < 0 || i > alter {
			t.Errorf("setup.sql must run %q before ALTER ROLE ... PASSWORD:\n%s", before, setup)
		}
	}

	var queries map[string]struct {
		Master  bool             `yaml:"master"`
		Query   string           `yaml:"query"`
		Metrics []map[string]any `yaml:"metrics"`
	}
	if err := yaml.Unmarshal([]byte(cm.Data["queries.yaml"]), &queries); err != nil {
		t.Fatalf("queries.yaml is not YAML: %v\n%s", err, cm.Data["queries.yaml"])
	}
	for name, master := range map[string]bool{
		"pg_top_statements": true, "pg_activity": true, "pg_cpu": true, "pg_top_tables": false,
		"pg_active_statements": true,
	} {
		q, ok := queries[name]
		if !ok {
			t.Errorf("queries.yaml has no %s", name)
			continue
		}
		if q.Master != master {
			t.Errorf("%s master = %v, want %v (cluster-wide views run once, per-table stats per database)", name, q.Master, master)
		}
		if len(q.Metrics) == 0 {
			t.Errorf("%s declares no metrics", name)
		}
	}
	if q := queries["pg_top_statements"].Query; !strings.Contains(q, "LIMIT 50") || !strings.Contains(q, `'''[^'']*'''`) {
		t.Errorf("pg_top_statements must be bounded to 50 and strip string literals from the label:\n%s", q)
	}
	// The pile-up budget (gd-ylgk1jBhLi) counts active copies per queryid.
	if q := queries["pg_active_statements"].Query; !strings.Contains(q, "LIMIT 10") || !strings.Contains(q, "query_id::text AS queryid") {
		t.Errorf("pg_active_statements must label by queryid and be bounded to 10:\n%s", q)
	}
	if q := queries["pg_top_tables"].Query; !strings.Contains(q, "LIMIT 20") || !strings.Contains(q, "LIMIT 10") {
		t.Errorf("pg_top_tables must be bounded (20 by size, 10 by writes):\n%s", q)
	}

	// With pg_stat_statements off the view does not exist, so the query must go.
	noPSS := helmTemplate(t, pgExporterConfigMap, "postgres.enabled=true", "postgres.exporter.enabled=true",
		"postgres.pgStatStatements.enabled=false")
	if strings.Contains(noPSS, "pg_top_statements") || strings.Contains(noPSS, "pg_active_statements") || strings.Contains(noPSS, "CREATE EXTENSION") {
		t.Errorf("pgStatStatements.enabled=false must drop pg_top_statements, pg_active_statements and the extension:\n%s", noPSS)
	}
}

func TestPostgresExporter_NetworkPolicyAdmitsVMAgent(t *testing.T) {
	out := helmTemplate(t, "templates/networkpolicies/networkpolicies.yaml",
		"networkPolicies.enabled=true", "postgres.enabled=true", "postgres.exporter.enabled=true", "vmagent.enabled=true")
	policy := out[strings.Index(out, "name: test-postgres-ingress"):]
	if end := strings.Index(policy, "\n---"); end > 0 {
		policy = policy[:end]
	}
	rule := policy[strings.Index(policy, "# vmagent scrapes the postgres_exporter"):]
	for _, want := range []string{"app.kubernetes.io/component: vmagent", "port: 9187"} {
		if !strings.Contains(rule, want) {
			t.Errorf("postgres ingress policy must admit vmagent on 9187, missing %q:\n%s", want, policy)
		}
	}
}

// containerBlock returns the rendered text of one container, up to the next
// container or the pod's volumes.
func containerBlock(t *testing.T, out, name string) string {
	t.Helper()
	start := strings.Index(out, "        - name: "+name+"\n")
	if start < 0 {
		t.Fatalf("no container %q in:\n%s", name, out)
	}
	rest := out[start+1:]
	end := len(rest)
	for _, stop := range []string{"\n        - name: ", "\n      volumes:"} {
		if i := strings.Index(rest, stop); i >= 0 && i < end {
			end = i
		}
	}
	return rest[:end]
}
