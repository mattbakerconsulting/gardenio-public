package tests

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Keycloak role-drift probe (gd-L2MyfEe9lL, a 10.7 Lands security-review
// finding). A fresh realm user holding gardenio-viewer and no
// gardenio-project-member is an unrestricted reader in kbeads and the viewer;
// before this probe only the 10.10 check-in (lands-fence-matrix.sh K1/K2)
// looked for it. The contract enforced here:
//   - the script fails on a gardenio-* role on default-roles-<realm> (R1) and
//     on any error, signs in as its own client, and makes no write call,
//   - the CronJob, its client Secret and its KeycloakClient (realm-management
//     view-realm only, operator decision gd-TgsaQWJPQ3) render only while the
//     probe and the in-box Keycloak are on, and never mount the master admin,
//   - the Keycloak ingress and the probe's egress NetworkPolicies admit it,
//   - KeycloakRoleDriftProbeFailing / -Unmonitored select its CronJob, and
//     pod-state-metrics watches cronjobs for it.

// fakeKcadm stands in for kcadm.sh: it logs each call to $FAKE_LOG and answers
// from FAKE_* env vars (printf %b, so "\n" separates lines). Any call it does
// not know fails.
const fakeKcadm = `#!/usr/bin/env bash
echo "$*" >> "$FAKE_LOG"
case "$1 $2" in
  "config credentials") [ -n "${FAKE_LOGIN_FAIL:-}" ] && { echo "HTTP 401 Unauthorized" >&2; exit 1; }; exit 0 ;;
  "config truststore") exit 0 ;;
esac
case "$1 $2" in
  get\ roles/default-roles-*/composites) [ -n "${FAKE_READ_FAIL:-}" ] && { echo "HTTP 403 Forbidden" >&2; exit 1; }; printf '%b' "${FAKE_DEFAULT_ROLES:-}" ;;
  *) exit 1 ;;
esac
`

// runRoleDriftProbe runs files/keycloak/role-drift-probe.sh against fakeKcadm
// with the given FAKE_* env and returns its output, exit code and kcadm log.
func runRoleDriftProbe(t *testing.T, env ...string) (string, int, string) {
	t.Helper()
	dir := t.TempDir()
	fake := filepath.Join(dir, "kcadm.sh")
	if err := os.WriteFile(fake, []byte(fakeKcadm), 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "kcadm.log")
	cmd := exec.Command("bash", filepath.Join(chartDir(t), "files", "keycloak", "role-drift-probe.sh"))
	cmd.Env = append([]string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + dir,
		"KCADM=" + fake,
		"FAKE_LOG=" + logPath,
		"KC_SERVER=http://keycloak:8080/auth",
		"KC_REALM=gardenio",
		"KC_CLIENT_ID=gardenio-role-drift-probe",
		"KC_CLIENT_SECRET=s3cret",
	}, env...)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run probe: %v\n%s", err, out)
		}
		code = ee.ExitCode()
	}
	log, _ := os.ReadFile(logPath)
	return string(out), code, string(log)
}

func TestKeycloakRoleDriftProbe_Script(t *testing.T) {
	t.Parallel()
	const clean = "offline_access\numa_authorization\nmanage-account\n"
	cases := []struct {
		name     string
		env      []string
		wantCode int
		want     []string
	}{
		{"clean realm passes", []string{"FAKE_DEFAULT_ROLES=" + clean},
			0, []string{"PASS R1 default-roles-gardenio composes no gardenio-* role"}},
		{"viewer on default-roles fails R1 (the 10-25 drift)", []string{"FAKE_DEFAULT_ROLES=" + clean + "gardenio-viewer\n"},
			1, []string{"FAIL R1 default-roles-gardenio composes gardenio-viewer"}},
		{"any gardenio-* composite fails R1", []string{"FAKE_DEFAULT_ROLES=" + clean + "gardenio-operator\ngardenio-project-member\n"},
			1, []string{"FAIL R1 default-roles-gardenio composes gardenio-operator gardenio-project-member"}},
		{"login failure fails", []string{"FAKE_LOGIN_FAIL=1"},
			1, []string{"FAIL setup: client gardenio-role-drift-probe could not sign in", "kcadm> HTTP 401"}},
		{"a refused read fails, never passes", []string{"FAKE_READ_FAIL=1"},
			1, []string{"FAIL R1 could not read default-roles-gardenio", "kcadm> HTTP 403"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, code, log := runRoleDriftProbe(t, tc.env...)
			if code != tc.wantCode {
				t.Errorf("exit %d, want %d; output:\n%s", code, tc.wantCode, out)
			}
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("output missing %q:\n%s", w, out)
				}
			}
			if !strings.Contains(log, "config credentials --server http://keycloak:8080/auth --realm gardenio --client gardenio-role-drift-probe") {
				t.Errorf("the probe must sign in to the realm as its own client; kcadm calls:\n%s", log)
			}
			for _, verb := range []string{"\ncreate ", "\nupdate ", "\ndelete ", "--realm master"} {
				if strings.Contains("\n"+log, verb) {
					t.Errorf("the probe must only read (found %q); kcadm calls:\n%s", strings.TrimSpace(verb), log)
				}
			}
		})
	}
}

// noViewerDefault clears keycloak.realm.defaultGroups, whose chart default
// (gardenio-viewers) makes every new user a viewer on purpose and so turns the
// probe off. The k3s overlay sets it to [].
const noViewerDefault = "keycloak.realm.defaultGroups=null"

// TestKeycloakRoleDriftProbe_OffWhenDefaultGroupGrantsViewer: with the chart's
// default viewer group declared, the probe would fail every run, so neither it,
// its NetworkPolicies nor its alerts render.
func TestKeycloakRoleDriftProbe_OffWhenDefaultGroupGrantsViewer(t *testing.T) {
	t.Parallel()
	on := []string{"keycloak.runtime.enabled=true", "networkPolicies.enabled=true", "networkPolicies.defaultDenyEgress=true", "monitoring.podStateMetrics.enabled=true"}
	on = append(on, gapAlertsNotifierArgs...)
	for _, tmpl := range []string{
		"templates/networkpolicies/networkpolicies.yaml",
		"templates/networkpolicies/default-deny-egress.yaml",
		"templates/vmalert/configmap.yaml",
		"templates/monitoring/pod-state-metrics.yaml",
	} {
		if out := helmTemplate(t, tmpl, on...); strings.Contains(out, "component: keycloak-runtime-role-drift") || strings.Contains(out, "alert: KeycloakRoleDrift") || strings.Contains(out, ",cronjobs") {
			t.Errorf("%s must not render probe parts while defaultGroups grants gardenio-viewer", tmpl)
		}
	}
	if out, err := helmTemplateAllowFail(t, "templates/keycloak-runtime/role-drift-cronjob.yaml", on...); err == nil && strings.Contains(out, "kind: CronJob") {
		t.Errorf("the probe must not render while defaultGroups grants gardenio-viewer, got:\n%s", out)
	}
	// A default group without a gardenio-* role keeps the probe on.
	if out := helmTemplate(t, "templates/keycloak-runtime/role-drift-cronjob.yaml", "keycloak.runtime.enabled=true", "keycloak.realm.defaultGroups[0]=invited"); !strings.Contains(out, "kind: CronJob") {
		t.Errorf("a default group with no gardenio-* role must keep the probe on")
	}
}

func TestKeycloakRoleDriftProbe_CronJob(t *testing.T) {
	t.Parallel()
	const tmpl = "templates/keycloak-runtime/role-drift-cronjob.yaml"

	if out, err := helmTemplateAllowFail(t, tmpl); err == nil && strings.Contains(out, "kind: CronJob") {
		t.Errorf("the probe must not render without the in-box Keycloak, got:\n%s", out)
	}
	if out, err := helmTemplateAllowFail(t, tmpl, "keycloak.runtime.enabled=true", noViewerDefault, "keycloak.runtime.roleDriftProbe.enabled=false"); err == nil && strings.Contains(out, "kind: CronJob") {
		t.Errorf("roleDriftProbe.enabled=false must suppress the CronJob, got:\n%s", out)
	}

	out := helmTemplateNS(t, "gardenio", tmpl, "keycloak.runtime.enabled=true", noViewerDefault)
	script, err := os.ReadFile(filepath.Join(chartDir(t), "files", "keycloak", "role-drift-probe.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"name: test-keycloak-runtime-role-drift\n",
		`schedule: "23 * * * *"`,
		"backoffLimit: 0",
		"app.kubernetes.io/component: keycloak-runtime-role-drift",
		"automountServiceAccountToken: false",
		"readOnlyRootFilesystem: true",
		`value: "http://test-keycloak-runtime.gardenio.svc.cluster.local:8080/auth"`,
		"name: KC_CLIENT_SECRET\n                  valueFrom:\n                    secretKeyRef:\n                      name: \"test-keycloak-runtime-role-drift-client\"",
		"kind: Secret",
		"name: test-keycloak-runtime-role-drift-client\n",
		"\"helm.sh/resource-policy\": keep",
		// The whole script, line for line.
		"PASS R1 default-roles-$KC_REALM",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("CronJob missing %q, got:\n%s", want, out)
		}
	}
	for _, line := range strings.Split(strings.TrimSpace(string(script)), "\n") {
		if strings.TrimSpace(line) != "" && !strings.Contains(out, line) {
			t.Errorf("CronJob does not embed script line %q", line)
			break
		}
	}
	client := renderedDoc(t, helmTemplate(t, "templates/keycloak/clients.yaml", "keycloak.runtime.enabled=true", noViewerDefault), "KeycloakClient", "test-client-gardenio-role-drift-probe")
	for _, want := range []string{
		`clientId: "gardenio-role-drift-probe"`,
		"public: false",
		"standardFlowEnabled: false",
		"directAccess: false",
		`secret: "$test-keycloak-runtime-role-drift-client:clientSecret"`,
		"clientRoles:\n    - clientId: realm-management\n      roles:\n      - view-realm\n    enabled: true\n    realmRoles: []",
	} {
		if !strings.Contains(client, want) {
			t.Errorf("probe KeycloakClient missing %q, got:\n%s", want, client)
		}
	}
	if out := helmTemplate(t, "templates/keycloak/clients.yaml", "keycloak.runtime.enabled=true"); strings.Contains(out, "gardenio-role-drift-probe") {
		t.Errorf("no probe client while the probe does not render")
	}
	if strings.Contains(out, "KC_ADMIN") || strings.Contains(out, "keycloak-runtime-admin") {
		t.Errorf("the probe must not mount the master-realm admin credential, got:\n%s", out)
	}
	// component=keycloak-runtime would put the probe pod behind the Keycloak
	// Service and under its ingress policy.
	if strings.Contains(out, "app.kubernetes.io/component: keycloak-runtime\n") {
		t.Errorf("the probe pod must not carry the Keycloak server's component label, got:\n%s", out)
	}

	tls := helmTemplateNS(t, "gardenio", tmpl, "keycloak.runtime.enabled=true", noViewerDefault, "keycloak.runtime.tls.enabled=true")
	for _, want := range []string{
		`value: "https://test-keycloak-runtime.gardenio.svc.cluster.local:8443/auth"`,
		"name: TRUSTSTORE_FILE",
		"path: truststore.p12",
	} {
		if !strings.Contains(tls, want) {
			t.Errorf("TLS CronJob missing %q, got:\n%s", want, tls)
		}
	}
}

func TestKeycloakRoleDriftProbe_NetworkPolicies(t *testing.T) {
	t.Parallel()
	base := []string{"keycloak.runtime.enabled=true", noViewerDefault, "networkPolicies.enabled=true", "networkPolicies.defaultDenyEgress=true"}
	const sel = "app.kubernetes.io/component: keycloak-runtime-role-drift"
	for _, tc := range []struct {
		name string
		args []string
		port string
	}{
		{"http", base, "port: 8080"},
		{"https", append(append([]string{}, base...), "keycloak.runtime.tls.enabled=true"), "port: 8443"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ingress := renderedDoc(t, helmTemplate(t, "templates/networkpolicies/networkpolicies.yaml", tc.args...), "NetworkPolicy", "test-keycloak-runtime-ingress")
			i := strings.Index(ingress, sel)
			if i < 0 {
				t.Fatalf("Keycloak ingress must admit the probe, got:\n%s", ingress)
			}
			rest := ingress[i:]
			if j := strings.Index(rest, "ports:"); j < 0 || !strings.Contains(rest[j:min(len(rest), j+60)], tc.port) {
				t.Errorf("the probe must be admitted on %s, got:\n%s", tc.port, ingress)
			}
			egress := renderedDoc(t, helmTemplate(t, "templates/networkpolicies/default-deny-egress.yaml", tc.args...), "NetworkPolicy", "test-keycloak-role-drift-egress")
			for _, want := range []string{sel, "app.kubernetes.io/component: keycloak-runtime\n", tc.port, "port: 53"} {
				if !strings.Contains(egress, want) {
					t.Errorf("probe egress missing %q, got:\n%s", want, egress)
				}
			}
		})
	}

	off := append(append([]string{}, base...), "keycloak.runtime.roleDriftProbe.enabled=false")
	if out := helmTemplate(t, "templates/networkpolicies/networkpolicies.yaml", off...); strings.Contains(out, sel) {
		t.Errorf("no ingress for a probe that does not render")
	}
	if out := helmTemplate(t, "templates/networkpolicies/default-deny-egress.yaml", off...); strings.Contains(out, "role-drift-egress") {
		t.Errorf("no egress policy for a probe that does not render")
	}
}

func TestKeycloakRoleDriftProbe_Alerts(t *testing.T) {
	t.Parallel()
	args := append(append([]string{}, gapAlertsNotifierArgs...), "keycloak.runtime.enabled=true", noViewerDefault)

	if out := helmTemplate(t, "templates/vmalert/configmap.yaml", gapAlertsNotifierArgs...); strings.Contains(out, "KeycloakRoleDrift") {
		t.Errorf("role-drift alerts must not render without the probe, got:\n%s", out)
	}

	out := helmTemplateNS(t, "gardenio", "templates/vmalert/configmap.yaml", args...)
	sel := `kube_cronjob_status_last_successful_time{namespace="gardenio", cronjob="test-keycloak-runtime-role-drift"}`
	failing := alertBlock(t, out, "KeycloakRoleDriftProbeFailing")
	if !strings.Contains(failing, "time() - "+sel+"\n                > 5400") {
		t.Errorf("KeycloakRoleDriftProbeFailing must compare the probe's last success to 5400 s, got:\n%s", failing)
	}
	absent := alertBlock(t, out, "KeycloakRoleDriftProbeUnmonitored")
	if !strings.Contains(absent, "absent("+sel+")") || !strings.Contains(absent, `for: "3h"`) {
		t.Errorf("KeycloakRoleDriftProbeUnmonitored must be absent() of the same selector for 3h, got:\n%s", absent)
	}
	for _, block := range []string{failing, absent} {
		for _, want := range []string{`severity: "warn"`, "subsystem: role-drift", "#keycloakroledriftprobefailing"} {
			if !strings.Contains(block, want) {
				t.Errorf("role-drift alert missing %q, got:\n%s", want, block)
			}
		}
	}

	if off := helmTemplate(t, "templates/vmalert/configmap.yaml", append(args, "vmalert.gapAlerts.keycloakRoleDrift.enabled=false")...); strings.Contains(off, "KeycloakRoleDrift") {
		t.Errorf("keycloakRoleDrift.enabled=false must suppress both alerts")
	}

	psm := helmTemplate(t, "templates/monitoring/pod-state-metrics.yaml", "monitoring.podStateMetrics.enabled=true", "keycloak.runtime.enabled=true", noViewerDefault)
	if !strings.Contains(psm, `"--resources=pods,cronjobs"`) {
		t.Errorf("pod-state-metrics must watch cronjobs while the probe renders, got:\n%s", psm)
	}
}
