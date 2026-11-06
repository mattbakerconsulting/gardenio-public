package tests

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// gd-cawNAc0iU7: every custom-query series the Postgres efficiency board reads
// must be one the exporter's queries.yaml (as the k3s overlay renders it)
// actually emits, or the panel renders dark with nothing saying why. A custom
// series is named <query namespace>_<column> for each non-LABEL column.
var pgCustomSeries = regexp.MustCompile(`\bpg_(?:top_statements|top_tables|activity|active_statements|cpu)_[a-z_]+`)

func TestPostgresEfficiencyDashboard_ReadsOnlyEmittedSeries(t *testing.T) {
	raw := readDashboardJSON(t, "postgres-efficiency.json")
	var d struct {
		UID string `json:"uid"`
	}
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		t.Fatalf("postgres-efficiency.json is not valid JSON: %v", err)
	}
	if d.UID != "gardenio-postgres-efficiency" {
		t.Errorf("uid = %q, want gardenio-postgres-efficiency (links and bookmarks key on it)", d.UID)
	}

	emitted := pgExporterEmittedSeries(t)

	read := map[string]bool{}
	for _, name := range pgCustomSeries.FindAllString(raw, -1) {
		read[name] = true
	}
	if len(read) < 10 {
		t.Fatalf("board reads only %d custom series — did the queries get renamed?", len(read))
	}
	var missing []string
	for name := range read {
		if !emitted[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("board reads series queries.yaml does not emit: %s", strings.Join(missing, ", "))
	}
}

// pgExporterEmittedSeries is every custom series the exporter's queries.yaml
// emits as the k3s overlay renders it: <query namespace>_<column> for each
// non-LABEL column.
func pgExporterEmittedSeries(t *testing.T) map[string]bool {
	t.Helper()
	out := helmTemplateShowOnlyWithValues(t, pgExporterConfigMap, []string{k3sOverlay})
	var cm struct {
		Data map[string]string `yaml:"data"`
	}
	if err := yaml.Unmarshal([]byte(out), &cm); err != nil {
		t.Fatalf("ConfigMap is not YAML: %v", err)
	}
	var queries map[string]struct {
		Metrics []map[string]struct {
			Usage string `yaml:"usage"`
		} `yaml:"metrics"`
	}
	if err := yaml.Unmarshal([]byte(cm.Data["queries.yaml"]), &queries); err != nil {
		t.Fatalf("queries.yaml is not YAML: %v", err)
	}
	emitted := map[string]bool{}
	for ns, q := range queries {
		for _, m := range q.Metrics {
			for col, spec := range m {
				if spec.Usage != "LABEL" {
					emitted[ns+"_"+col] = true
				}
			}
		}
	}
	return emitted
}
