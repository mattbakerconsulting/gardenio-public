package tests

import (
	"strings"
	"testing"
)

// Guard rails for ephemeral-namespace EBS attachment pressure (bug ka-0000000711).
//
// Throwaway ephemeral namespaces (the RWX-era gardenio-rwx-<id>, reaped within
// TTL) deployed postgres, nats, vmsingle, and coopmux — each backed by its own
// EBS PVC. With maxConcurrent=10 ephemerals that is ~40 EBS volumes that can concentrate on a
// single POC node and trip the EC2 per-instance attach limit
// ("AttachmentLimitExceeded: You may attach up to 27 devices for this instance
// type"): postgres-0 sticks in ContainerCreating (FailedAttachVolume) -> beads
// cannot reach postgres -> beads-migrate Job BackoffLimitExceeded -> helm
// pre-upgrade hooks fail -> the deploy reds and golden promotion is blocked.
//
// The fix: ephemeral overlays set persistence.enabled=false for these
// components, and the StatefulSets back the `data` volume with an emptyDir when
// persistence is disabled. emptyDir sidesteps EBS entirely. These tests pin the
// chart-side fallback so a future edit cannot silently reintroduce the dangling
// `data` mount (which would crash the pod with "volume not found") or
// re-introduce a PVC for the throwaway path.

// stateful is a component whose StatefulSet declares its data volume via
// volumeClaimTemplates when persistence is on, and must fall back to emptyDir
// when persistence is off.
type statefulComponent struct {
	name     string // values key (postgres/nats/vmsingle)
	template string // chart template path
	mount    string // expected data mountPath
}

var statefulComponents = []statefulComponent{
	{"postgres", "templates/postgres/statefulset.yaml", "/var/lib/postgresql/data"},
	{"nats", "templates/nats/statefulset.yaml", "/data"},
	{"vmsingle", "templates/vmsingle/statefulset.yaml", "/victoria-metrics-data"},
}

// When persistence is disabled, the StatefulSet must render an emptyDir-backed
// `data` volume and NO volumeClaimTemplates — so no EBS volume is attached
// (ka-0000000711).
func TestEphemeral_PersistenceDisabled_UsesEmptyDir(t *testing.T) {
	t.Parallel()
	for _, c := range statefulComponents {
		t.Run(c.name, func(t *testing.T) {
			out := helmTemplate(t, c.template,
				c.name+".enabled=true",
				c.name+".persistence.enabled=false",
			)

			if strings.Contains(out, "volumeClaimTemplates") {
				t.Errorf("%s: must NOT render volumeClaimTemplates when persistence is disabled — that re-introduces an EBS PVC (ka-0000000711)", c.name)
			}

			// The data volume must still exist (backed by emptyDir) so the
			// `data` volumeMount does not dangle and crash the pod.
			dataVol := dataVolumeBlock(out)
			if dataVol == "" {
				t.Fatalf("%s: no `- name: data` volume found in rendered StatefulSet — the data mount would dangle:\n%s", c.name, out)
			}
			if !strings.Contains(dataVol, "emptyDir") {
				t.Errorf("%s: data volume must be backed by emptyDir when persistence is disabled, got:\n%s", c.name, dataVol)
			}

			// Sanity: the data mountPath must still be present so the workload
			// writes to the emptyDir.
			if !strings.Contains(out, "mountPath: "+c.mount) {
				t.Errorf("%s: expected data mountPath %q to remain mounted, not found in:\n%s", c.name, c.mount, out)
			}
		})
	}
}

// When persistence is enabled (the long-lived staging singleton path), the
// StatefulSet must still render a PVC via volumeClaimTemplates — the emptyDir
// fallback must not leak into the persistent path.
func TestEphemeral_PersistenceEnabled_UsesPVC(t *testing.T) {
	t.Parallel()
	for _, c := range statefulComponents {
		t.Run(c.name, func(t *testing.T) {
			out := helmTemplate(t, c.template,
				c.name+".enabled=true",
				c.name+".persistence.enabled=true",
				c.name+".persistence.size=5Gi",
			)
			if !strings.Contains(out, "volumeClaimTemplates") {
				t.Errorf("%s: must render volumeClaimTemplates (PVC) when persistence is enabled (ka-0000000711 regression guard)", c.name)
			}
			// The data volume must NOT be an emptyDir in the persistent path —
			// the claim template provides it.
			if dataVol := dataVolumeBlock(out); strings.Contains(dataVol, "emptyDir") {
				t.Errorf("%s: data volume must come from the PVC claim template, not emptyDir, when persistence is enabled; got:\n%s", c.name, dataVol)
			}
		})
	}
}

// dataVolumeBlock extracts the `- name: data` VOLUME DEFINITION from the
// StatefulSet's `volumes:` list (spec.template.spec.volumes). The rendered
// output contains two `- name: data` list items: the volumeMounts entry (which
// carries a `mountPath:`) and the volumes entry (which carries the backing
// source, e.g. `emptyDir:` or `persistentVolumeClaim:`). We want the latter, so
// we skip any block whose body is a mount (has `mountPath:`). volumeClaimTemplates
// uses the bare `name: data` form (no leading `- `) and is not matched here.
func dataVolumeBlock(rendered string) string {
	lines := strings.Split(rendered, "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) != "- name: data" {
			continue
		}
		// Capture this line plus the following more-indented lines.
		block := []string{l}
		baseIndent := len(l) - len(strings.TrimLeft(l, " "))
		for j := i + 1; j < len(lines); j++ {
			nl := lines[j]
			if strings.TrimSpace(nl) == "" {
				continue
			}
			indent := len(nl) - len(strings.TrimLeft(nl, " "))
			if indent <= baseIndent {
				break
			}
			block = append(block, nl)
		}
		joined := strings.Join(block, "\n")
		// Skip the volumeMounts entry — we want the volume definition.
		if strings.Contains(joined, "mountPath:") {
			continue
		}
		return joined
	}
	return ""
}
