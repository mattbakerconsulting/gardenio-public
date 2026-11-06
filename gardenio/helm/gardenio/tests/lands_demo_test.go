package tests

import (
	"strings"
	"testing"
)

// The hosted demo land's chart wiring (gd-0inRhBD3gy; runbook
// gardenio/docs/lands/demo-land.md, code gd-c6LzZkk0qy).
//
// Invariants locked here:
//   - agents.lands.demo.land empty (the default): no LANDS_DEMO_* env on the
//     viewer or the controller.
//   - land set: the viewer gets LANDS_DEMO_LAND, _DAEMON_URL, _MAX_GUESTS and
//     _GUEST_TTL; the controller gets LANDS_DEMO_LAND and _GUEST_TTL with the
//     same TTL the viewer shows.
//   - land set without an https daemonURL fails the render.

const landsDemoOn = "agents.lands.demo.land=demo,agents.lands.demo.daemonURL=https://api.example.com"

func TestLandsDemo_OffByDefault(t *testing.T) {
	t.Parallel()
	viewer := helmTemplate(t, "templates/beads-viewer/deployment.yaml", "beadsViewer.enabled=true")
	controller := helmTemplate(t, "templates/controller/deployment.yaml", "agents.enabled=true")
	for name, out := range map[string]string{"beads-viewer": viewer, "controller": controller} {
		if strings.Contains(out, "LANDS_DEMO_") {
			t.Errorf("%s: demo land off by default, but LANDS_DEMO_* is rendered:\n%s", name, out)
		}
	}
}

func TestLandsDemo_Viewer_RendersDoorEnv(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads-viewer/deployment.yaml", "beadsViewer.enabled=true", landsDemoOn)
	want := map[string]string{
		"LANDS_DEMO_LAND":       "demo",
		"LANDS_DEMO_DAEMON_URL": "https://api.example.com",
		"LANDS_DEMO_MAX_GUESTS": "50",
		"LANDS_DEMO_GUEST_TTL":  "24h",
	}
	for k, wantV := range want {
		if got, ok := envValueAfter(out, k); !ok || got != wantV {
			t.Errorf("viewer %s = %q (present=%v), want %q", k, got, ok, wantV)
		}
	}
}

func TestLandsDemo_Viewer_Overrides(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads-viewer/deployment.yaml", "beadsViewer.enabled=true", landsDemoOn,
		"agents.lands.demo.maxGuests=7", "agents.lands.demo.guestTTL=2h")
	if got, _ := envValueAfter(out, "LANDS_DEMO_MAX_GUESTS"); got != "7" {
		t.Errorf("viewer LANDS_DEMO_MAX_GUESTS = %q, want 7", got)
	}
	if got, _ := envValueAfter(out, "LANDS_DEMO_GUEST_TTL"); got != "2h" {
		t.Errorf("viewer LANDS_DEMO_GUEST_TTL = %q, want 2h", got)
	}
}

// Empty maxGuests and guestTTL leave the env out, so the binary defaults hold.
func TestLandsDemo_Viewer_EmptyOptionalsUseBinaryDefaults(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads-viewer/deployment.yaml", "beadsViewer.enabled=true", landsDemoOn,
		"agents.lands.demo.maxGuests=", "agents.lands.demo.guestTTL=")
	for _, k := range []string{"LANDS_DEMO_MAX_GUESTS", "LANDS_DEMO_GUEST_TTL"} {
		if _, ok := envValueAfter(out, k); ok {
			t.Errorf("viewer %s rendered with an empty value; want it left to the binary default", k)
		}
	}
	if _, ok := envValueAfter(out, "LANDS_DEMO_LAND"); !ok {
		t.Errorf("viewer LANDS_DEMO_LAND missing with land set")
	}
}

func TestLandsDemo_Controller_RendersLandAndTTL(t *testing.T) {
	t.Parallel()
	_, d := renderControllerPodSpec(t, "agents.enabled=true", landsDemoOn, "agents.lands.demo.guestTTL=36h")
	got := map[string]string{}
	for _, e := range d.Spec.Template.Spec.Containers[0].Env {
		if strings.HasPrefix(e.Name, "LANDS_DEMO_") {
			got[e.Name] = e.Value
		}
	}
	want := map[string]string{"LANDS_DEMO_LAND": "demo", "LANDS_DEMO_GUEST_TTL": "36h"}
	if len(got) != len(want) {
		t.Errorf("controller LANDS_DEMO_* = %v, want exactly %v (the daemon URL and guest cap are viewer-only)", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("controller %s = %q, want %q", k, got[k], v)
		}
	}
}

func TestLandsDemo_Viewer_DaemonURLRequired(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"missing": "agents.lands.demo.land=demo",
		"http":    "agents.lands.demo.land=demo,agents.lands.demo.daemonURL=http://api.example.com",
		"no host": "agents.lands.demo.land=demo,agents.lands.demo.daemonURL=https://",
	}
	for name, flags := range cases {
		t.Run(name, func(t *testing.T) {
			out, err := helmTemplateAllowFail(t, "templates/beads-viewer/deployment.yaml", "beadsViewer.enabled=true", flags)
			if err == nil {
				t.Fatalf("render succeeded with %s; want a failure naming agents.lands.demo.daemonURL:\n%s", flags, out)
			}
			if !strings.Contains(out, "agents.lands.demo.daemonURL") {
				t.Errorf("failure does not name agents.lands.demo.daemonURL:\n%s", out)
			}
		})
	}
}
