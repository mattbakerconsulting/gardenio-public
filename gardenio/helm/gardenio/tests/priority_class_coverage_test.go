package tests

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Regression guards for gd-2qjxTVMBkC: on 2026-10-20 a crew burst filled the
// agent nodes and left sign-in, grafana, the backups and the release deployer
// Pending, because they ran at priority 0 alongside the crews. Every non-agent
// pod the chart renders must carry gardenio-system-critical or gardenio-infra.

const (
	pcCritical = "gardenio-system-critical"
	pcInfra    = "gardenio-infra"
	pcAgent    = "gardenio-agent"
)

// workloadKinds are the kinds that carry a pod template.
var workloadKinds = map[string]bool{
	"Deployment": true, "StatefulSet": true, "DaemonSet": true, "CronJob": true, "Job": true,
}

// signInAndBackups must sit on the top tier, not infra (by name suffix after
// the release prefix).
var signInAndBackups = []string{
	"-keycloak-runtime", "-forwardauth", "-forwardauth-redis", "-edge-auth",
	"-postgres-s3-backup", "-postgres-backup", "-backup-postgres", "-backup-nats",
}

// agentTierWorkloads may sit on the agent tier: they only warm the agent
// image on agent nodes and must never displace a crew.
var agentTierWorkloads = []string{"-agent-image-prewarm"}

type renderedWorkload struct {
	kind, name, priorityClass, hook string
}

var (
	podPriorityClassRE = regexp.MustCompile(`(?m)^\s+priorityClassName:\s*"?([^"\s]+)"?\s*$`)
	helmHookRE         = regexp.MustCompile(`(?m)^\s+"?helm\.sh/hook"?:\s*"?([^"\s]+)"?\s*$`)
)

// renderedWorkloads extracts each pod-bearing object by regex, like the
// sibling chart tests: the forwardauth Deployment carries a duplicate label
// key, which the API server tolerates but yaml.v3 rejects. Each workload has
// exactly one pod template, so the first priorityClassName is its own.
func renderedWorkloads(t *testing.T, out string) []renderedWorkload {
	t.Helper()
	var ws []renderedWorkload
	for raw := range strings.SplitSeq(out, "\n---") {
		m := kindLineRE.FindStringSubmatch(raw)
		if m == nil || !workloadKinds[m[1]] {
			continue
		}
		w := renderedWorkload{kind: m[1]}
		if nm := nameMetadataRE.FindStringSubmatch(raw); nm != nil {
			w.name = nm[1]
		}
		if pc := podPriorityClassRE.FindStringSubmatch(raw); pc != nil {
			w.priorityClass = pc[1]
		}
		if h := helmHookRE.FindStringSubmatch(raw); h != nil {
			w.hook = h[1]
		}
		ws = append(ws, w)
	}
	return ws
}

func hasAnySuffix(name string, suffixes []string) bool {
	for _, s := range suffixes {
		if strings.HasSuffix(name, s) {
			return true
		}
	}
	return false
}

// The k3s deployment, rendered as release-deploy.sh renders it: every
// workload has a non-agent tier, and the sign-in path and backups are on top.
func TestPriorityClassCoverage_K3sRender(t *testing.T) {
	t.Parallel()
	out := helmTemplateFull(t, []string{"../../../deploy-values/gardenio-k3s.yaml"})
	ws := renderedWorkloads(t, out)
	if len(ws) < 15 {
		t.Fatalf("expected the k3s render to carry at least 15 workloads, got %d", len(ws))
	}
	seenCritical := map[string]bool{}
	for _, w := range ws {
		if strings.HasPrefix(w.hook, "pre-") || strings.Contains(w.hook, ",pre-") {
			// gardenio.hookPriorityClassName: lookup-gated, so empty offline.
			continue
		}
		switch {
		case hasAnySuffix(w.name, signInAndBackups):
			seenCritical[w.name] = true
			if w.priorityClass != pcCritical {
				t.Errorf("%s %s: priorityClassName %q, want %q (sign-in path / backups)", w.kind, w.name, w.priorityClass, pcCritical)
			}
		case w.priorityClass != pcCritical && w.priorityClass != pcInfra:
			t.Errorf("%s %s: priorityClassName %q, want %q or %q", w.kind, w.name, w.priorityClass, pcCritical, pcInfra)
		}
	}
	for _, want := range []string{"test-keycloak-runtime", "test-forwardauth", "test-postgres-s3-backup"} {
		if !seenCritical[want] {
			t.Errorf("expected %s in the k3s render (positive control)", want)
		}
	}
}

// templateWorkloadKindRE matches a top-level pod-bearing kind in a template.
var templateWorkloadKindRE = regexp.MustCompile(`(?m)^kind: (Deployment|StatefulSet|DaemonSet|CronJob|Job)\s*$`)

// Components off in the k3s overlay never reach the render test above, so
// every template that defines a workload must name a tier statically too.
func TestPriorityClassCoverage_EveryTemplate(t *testing.T) {
	t.Parallel()
	root := filepath.Join(chartDir(t), "templates")
	checked := 0
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".yaml") {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		n := len(templateWorkloadKindRE.FindAll(b, -1))
		if n == 0 {
			return nil
		}
		checked++
		rel, _ := filepath.Rel(chartDir(t), path)
		tiers := bytes.Count(b, []byte(".Values.global.systemPriorityClass.name")) +
			bytes.Count(b, []byte(".Values.global.systemPriorityClass.infra.name")) +
			bytes.Count(b, []byte(".Values.global.systemPriorityClass.agent.name")) +
			bytes.Count(b, []byte(`include "gardenio.hookPriorityClassName"`))
		if tiers < n {
			t.Errorf("%s: %d workload(s) but %d priorityClassName tier reference(s); every non-agent pod needs gardenio-system-critical or gardenio-infra (gd-2qjxTVMBkC)", rel, n, tiers)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked < 50 {
		t.Fatalf("expected at least 50 workload templates, checked %d", checked)
	}
}

// Only the image prewarm DaemonSet may use the agent tier.
func TestPriorityClassCoverage_AgentTierOnlyForPrewarm(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/agent-image-prewarm.yaml",
		"agents.prewarmImage.enabled=true", "global.systemPriorityClass.create=true")
	for _, w := range renderedWorkloads(t, out) {
		if !hasAnySuffix(w.name, agentTierWorkloads) {
			continue
		}
		if w.priorityClass != pcAgent {
			t.Errorf("%s %s: priorityClassName %q, want %q", w.kind, w.name, w.priorityClass, pcAgent)
		}
	}
	k3s := renderedWorkloads(t, helmTemplateFull(t, []string{"../../../deploy-values/gardenio-k3s.yaml"}))
	for _, w := range k3s {
		if w.priorityClass == pcAgent && !hasAnySuffix(w.name, agentTierWorkloads) {
			t.Errorf("%s %s: on the agent tier; only agent pods belong there", w.kind, w.name)
		}
	}
}

// The five classes render with the documented order; the agent tier and the
// scheduled-gardener tier (the interactive value) are barred from preempting.
func TestPriorityClassTiers_Rendered(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/system-priorityclass.yaml", "global.systemPriorityClass.create=true")
	type pc struct {
		Metadata struct {
			Name string `yaml:"name"`
		} `yaml:"metadata"`
		Value            int    `yaml:"value"`
		PreemptionPolicy string `yaml:"preemptionPolicy"`
	}
	got := map[string]pc{}
	dec := yaml.NewDecoder(strings.NewReader(out))
	for {
		var c pc
		if err := dec.Decode(&c); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatal(err)
		}
		got[c.Metadata.Name] = c
	}
	crit, infra, inter, sched, agent := got[pcCritical], got[pcInfra], got["gardenio-interactive"], got["gardenio-scheduled"], got[pcAgent]
	if len(got) != 5 {
		t.Fatalf("expected 5 PriorityClasses, got %v", got)
	}
	if !(crit.Value > infra.Value && infra.Value > inter.Value && inter.Value > agent.Value) {
		t.Errorf("tiers out of order: critical %d, infra %d, interactive %d, agent %d", crit.Value, infra.Value, inter.Value, agent.Value)
	}
	if agent.PreemptionPolicy != "Never" {
		t.Errorf("agent tier preemptionPolicy %q, want Never", agent.PreemptionPolicy)
	}
	if sched.Value != inter.Value || sched.PreemptionPolicy != "Never" {
		t.Errorf("scheduled tier value %d preemptionPolicy %q, want %d and Never", sched.Value, sched.PreemptionPolicy, inter.Value)
	}
	for _, c := range []pc{crit, infra, inter} {
		if c.PreemptionPolicy != "" {
			t.Errorf("%s: preemptionPolicy %q, want the default", c.Metadata.Name, c.PreemptionPolicy)
		}
	}

	ctl := helmTemplate(t, "templates/controller/deployment.yaml", "agents.enabled=true", "global.systemPriorityClass.create=true")
	for _, want := range []string{
		"- name: INTERACTIVE_PRIORITY_CLASS\n              value: \"gardenio-interactive\"",
		"- name: SCHEDULED_PRIORITY_CLASS\n              value: \"gardenio-scheduled\"",
		"- name: AGENT_PRIORITY_CLASS\n              value: \"gardenio-agent\"",
	} {
		if !strings.Contains(ctl, want) {
			t.Errorf("controller deployment missing %q", want)
		}
	}
}

func TestPriorityClassTiers_AbsentByDefault(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "templates/system-priorityclass.yaml")
	if err == nil && strings.Contains(out, "kind: PriorityClass") {
		t.Errorf("expected no PriorityClass with create=false; got:\n%s", out)
	}
	ctl := helmTemplate(t, "templates/controller/deployment.yaml", "agents.enabled=true")
	if strings.Contains(ctl, "AGENT_PRIORITY_CLASS") || strings.Contains(ctl, "INTERACTIVE_PRIORITY_CLASS") || strings.Contains(ctl, "SCHEDULED_PRIORITY_CLASS") {
		t.Error("expected no agent priority env with create=false")
	}
}
