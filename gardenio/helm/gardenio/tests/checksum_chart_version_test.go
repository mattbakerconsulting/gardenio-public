package tests

import (
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A calver release that changes no config must not roll a pod through a
// checksum/* annotation. Those annotations hashed the whole rendered
// ConfigMap/Secret, labels included, and helm.sh/chart carries the chart
// version, so every release rolled every hashed workload. litellm is a single
// Recreate replica: each roll cut LiteLLM for minutes and failed the lands
// provisioner's purse step mid-flight (gd-AQ9iCiKxML). The forwardauth fix
// (forwardauth_restart_test.go) dropped the label from one ConfigMap; this
// test covers every checksum the production overlay renders.

var (
	checksumAnnotationRE = regexp.MustCompile(`(?m)^\s+(checksum/[a-z0-9-]+): (\S+)$`)
	workloadNameRE       = regexp.MustCompile(`(?m)^metadata:\n  name: (\S+)$`)
)

// renderPackagedK3s packages the chart at version and renders it with the
// production overlay plus any extra --set flags.
func renderPackagedK3s(t *testing.T, version string, setFlags ...string) string {
	t.Helper()
	dest := t.TempDir()
	if out, err := exec.Command("helm", "package", chartDir(t), "--version", version, "--app-version", version, "--destination", dest).CombinedOutput(); err != nil {
		t.Fatalf("helm package %s failed: %v\n%s", version, err, out)
	}
	args := []string{"template", "test", filepath.Join(dest, "gardenio-"+version+".tgz"), "-f", filepath.Join(chartDir(t), k3sOverlay)}
	for _, s := range setFlags {
		args = append(args, "--set", s)
	}
	out, err := exec.Command("helm", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template %s failed: %v\n%s", version, err, out)
	}
	return string(out)
}

// podChecksums maps "<workload>/<annotation>" to its value for every
// checksum/* annotation in the render.
func podChecksums(rendered string) map[string]string {
	sums := map[string]string{}
	for doc := range strings.SplitSeq(rendered, "\n---") {
		name := workloadNameRE.FindStringSubmatch(doc)
		if name == nil {
			continue
		}
		for _, m := range checksumAnnotationRE.FindAllStringSubmatch(doc, -1) {
			sums[name[1]+"/"+m[1]] = m[2]
		}
	}
	return sums
}

func TestPodChecksums_IgnoreChartVersion(t *testing.T) {
	t.Parallel()
	v1 := renderPackagedK3s(t, "2026.100.1")
	v2 := renderPackagedK3s(t, "2026.200.9")
	if !strings.Contains(v1, "gardenio-2026.100.1") || !strings.Contains(v2, "gardenio-2026.200.9") {
		t.Fatal("control: helm.sh/chart does not carry the packaged version; the version bump is not reaching the render")
	}
	s1, s2 := podChecksums(v1), podChecksums(v2)
	// The overlay renders litellm, vmagent, vmalert, alertmanager, the
	// controller and coopmux at least; a near-empty map means the regexes
	// stopped matching, not that the chart is clean.
	if len(s1) < 6 || len(s1) != len(s2) {
		t.Fatalf("expected the same >=6 checksum annotations in both renders, got %d and %d: %v", len(s1), len(s2), s1)
	}
	for _, want := range []string{"test-litellm/checksum/config"} {
		if _, ok := s1[want]; !ok {
			t.Errorf("%s missing from the render: %v", want, s1)
		}
	}
	for k, a := range s1 {
		if b := s2[k]; a != b {
			t.Errorf("%s changed with the chart version (%s -> %s): every calver release would roll this pod", k, a, b)
		}
	}
}

// The checksum must still move when the hashed content does, or it would
// never roll a pod at all.
func TestPodChecksums_FollowConfigContent(t *testing.T) {
	t.Parallel()
	base := podChecksums(renderPackagedK3s(t, "2026.100.1"))
	edited := podChecksums(renderPackagedK3s(t, "2026.100.1", "litellm.models[0].model_name=checksum-probe"))
	k := "test-litellm/checksum/config"
	if base[k] == "" || base[k] == edited[k] {
		t.Errorf("%s did not change with the LiteLLM config (%q -> %q)", k, base[k], edited[k])
	}
}
