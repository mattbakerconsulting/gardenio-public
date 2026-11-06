package tests

import (
	"strings"
	"testing"
)

// BEADS_HERALDRY_GATE and BEADS_HERALDRY_GATE_TENANT_LANDS select the kbeads
// create-time heraldry gate's mode for the home garden's epics and for tenant
// lands' (kbeads/internal/server/heraldry_gate.go, gd-fn7uPujOqj). The chart
// owns them through beads.heraldryGate.{home,tenantLands}: warn for the home
// garden and off for tenant lands by default; empty leaves the env unset so
// the daemon default applies.

// heraldryGateEnvValue returns the value line rendered after the env name, or
// "" when the env is not rendered.
func heraldryGateEnvValue(t *testing.T, out, name string) string {
	t.Helper()
	lines := strings.Split(out, "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) != "- name: "+name {
			continue
		}
		if i+1 >= len(lines) {
			t.Fatalf("%s rendered with no value line", name)
		}
		return strings.TrimSpace(lines[i+1])
	}
	return ""
}

func TestBeadsHeraldryGateEnv_Default(t *testing.T) {
	t.Parallel()
	out := helmBeadsDeployment(t)
	if !strings.Contains(out, "BEADS_HTTP_ADDR") {
		t.Fatalf("control: beads Deployment did not render the daemon env:\n%s", out)
	}
	if got, want := heraldryGateEnvValue(t, out, "BEADS_HERALDRY_GATE"), `value: "warn"`; got != want {
		t.Errorf("default BEADS_HERALDRY_GATE = %q; want %q (warn for the home garden)", got, want)
	}
	if got, want := heraldryGateEnvValue(t, out, "BEADS_HERALDRY_GATE_TENANT_LANDS"), `value: "off"`; got != want {
		t.Errorf("default BEADS_HERALDRY_GATE_TENANT_LANDS = %q; want %q (off for tenant lands)", got, want)
	}
}

func TestBeadsHeraldryGateEnv_Set(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"warn", "enforce", "off"} {
		t.Run(mode, func(t *testing.T) {
			out := helmBeadsDeployment(t,
				"--set", "beads.heraldryGate.home="+mode,
				"--set", "beads.heraldryGate.tenantLands="+mode)
			want := `value: "` + mode + `"`
			for _, name := range []string{"BEADS_HERALDRY_GATE", "BEADS_HERALDRY_GATE_TENANT_LANDS"} {
				if got := heraldryGateEnvValue(t, out, name); got != want {
					t.Errorf("%s = %q; want %q", name, got, want)
				}
			}
		})
	}
}

func TestBeadsHeraldryGateEnv_EmptyUnset(t *testing.T) {
	t.Parallel()
	out := helmBeadsDeployment(t,
		"--set", "beads.heraldryGate.home=",
		"--set", "beads.heraldryGate.tenantLands=")
	if strings.Contains(out, "BEADS_HERALDRY_GATE") {
		t.Errorf("empty beads.heraldryGate must leave both envs unset (daemon defaults)")
	}
}
