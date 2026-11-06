package tests

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A gardenio-in-a-box reports its release to the home daemon (plan
// gd-UZKZ9H25UD step 4d, releases-design.md §4.3 item 5). With
// releases.box.name set, the controller Deployment carries the env the
// controller's startBoxReleaseReporter reads (cmd/controller/release_reconciler.go)
// and mounts the home client secret as a file; with it unset, none of it renders.

const (
	boxHomeSecretFile = "/var/run/secrets/gardenio-release-home/clientSecret"
	boxHomeCAFile     = "/var/run/secrets/gardenio-release-home-ca/ca.crt"
)

// boxReleaseEnv is every env var the box reporter reads.
var boxReleaseEnv = []string{
	"GARDENIO_BOX_NAME",
	"GARDENIO_RELEASE_HOME_ADDR",
	"GARDENIO_RELEASE_HOME_PROJECT",
	"GARDENIO_RELEASE_HOME_TOKEN_URL",
	"GARDENIO_RELEASE_HOME_CLIENT_ID",
	"GARDENIO_RELEASE_HOME_CLIENT_SECRET_FILE",
	"GARDENIO_RELEASE_HOME_CA_FILE",
}

var boxReleaseOn = []string{
	"agents.enabled=true",
	"releases.box.name=ec2a",
	"releases.box.homeAddr=https://beads.home.example",
	"releases.box.homeTokenURL=https://auth.home.example/auth/realms/gardenio/protocol/openid-connect/token",
}

func TestReleaseBox_RendersHomeReporter(t *testing.T) {
	t.Parallel()
	d := renderDeployment(t, "templates/controller/deployment.yaml", nil, boxReleaseOn...)
	want := map[string]string{
		"GARDENIO_BOX_NAME":                        "ec2a",
		"GARDENIO_RELEASE_HOME_ADDR":               "https://beads.home.example",
		"GARDENIO_RELEASE_HOME_PROJECT":            "gardenio",
		"GARDENIO_RELEASE_HOME_TOKEN_URL":          "https://auth.home.example/auth/realms/gardenio/protocol/openid-connect/token",
		"GARDENIO_RELEASE_HOME_CLIENT_ID":          "gardenio-box-ec2a",
		"GARDENIO_RELEASE_HOME_CLIENT_SECRET_FILE": boxHomeSecretFile,
	}
	for name, v := range want {
		e, ok := d.env("", name)
		if !ok {
			t.Errorf("%s not rendered", name)
			continue
		}
		if e.Value != v || e.ValueFrom != nil {
			t.Errorf("%s = %q (valueFrom %v), want literal %q", name, e.Value, e.ValueFrom, v)
		}
	}
	if e, ok := d.env("", "GARDENIO_RELEASE_HOME_CA_FILE"); ok {
		t.Errorf("GARDENIO_RELEASE_HOME_CA_FILE = %q rendered without releases.box.homeCA.name", e.Value)
	}

	// The client secret is an optional, read-only file mount at the path the
	// env names, never an env var.
	for _, c := range d.Spec.Template.Spec.Containers {
		for _, e := range c.Env {
			if ref, ok := e.ValueFrom["secretKeyRef"].(map[string]any); ok && ref["name"] == "gardenio-release-home-client" {
				t.Errorf("env %s reads the home client secret via secretKeyRef; it must be a file", e.Name)
			}
		}
	}
	assertSecretMount(t, d, "gardenio-release-home-client", filepath.Dir(boxHomeSecretFile))
}

func TestReleaseBox_OverridesAndCA(t *testing.T) {
	t.Parallel()
	d := renderDeployment(t, "templates/controller/deployment.yaml", nil, append(boxReleaseOn,
		"releases.box.homeProject=fleet",
		"releases.box.homeClientID=box-client",
		"releases.box.homeClientSecret.name=my-home-client",
		"releases.box.homeCA.name=home-ca",
	)...)
	for name, v := range map[string]string{
		"GARDENIO_RELEASE_HOME_PROJECT":   "fleet",
		"GARDENIO_RELEASE_HOME_CLIENT_ID": "box-client",
		"GARDENIO_RELEASE_HOME_CA_FILE":   boxHomeCAFile,
	} {
		if e, ok := d.env("", name); !ok || e.Value != v {
			t.Errorf("%s = %q (rendered %v), want %q", name, e.Value, ok, v)
		}
	}
	assertSecretMount(t, d, "my-home-client", filepath.Dir(boxHomeSecretFile))
	assertSecretMount(t, d, "home-ca", filepath.Dir(boxHomeCAFile))
}

// Off: chart defaults, and the inbox overlay (which leaves name empty), render
// none of the env and no home-secret volume.
func TestReleaseBox_OffRendersNothing(t *testing.T) {
	t.Parallel()
	cases := map[string][]string{
		"chart defaults": nil,
		"inbox overlay":  {filepath.Join(chartDir(t), "values-inbox.yaml")},
	}
	for name, files := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// tls.mode=disabled: the box supplies tls.hosts at bootstrap, out of
			// scope here (as in box_networkpolicy_test.go).
			out := helmRenderFiles(t, "templates/controller/deployment.yaml", files, "agents.enabled=true", "tls.mode=disabled")
			if out == "" {
				t.Fatalf("the controller Deployment renders nothing; the control proves nothing")
			}
			for _, env := range boxReleaseEnv {
				if strings.Contains(out, env) {
					t.Errorf("%s rendered with releases.box.name unset", env)
				}
			}
			if strings.Contains(out, "release-home") {
				t.Errorf("a release-home volume or mount rendered with releases.box.name unset")
			}
		})
	}
}

// A box name without the home address or token URL fails the render rather
// than shipping a controller that silently never reports.
func TestReleaseBox_RequiresHomeAddrAndTokenURL(t *testing.T) {
	t.Parallel()
	for _, missing := range []string{"releases.box.homeAddr", "releases.box.homeTokenURL"} {
		args := []string{"template", "test", chartDir(t), "--namespace", "gardenio",
			"--show-only", "templates/controller/deployment.yaml"}
		for _, s := range boxReleaseOn {
			if !strings.HasPrefix(s, missing+"=") {
				args = append(args, "--set", s)
			}
		}
		out, err := exec.Command("helm", args...).CombinedOutput()
		if err == nil {
			t.Errorf("render without %s succeeded; want a required-value error", missing)
			continue
		}
		if !strings.Contains(string(out), missing+" is required") {
			t.Errorf("render without %s failed with %q; want it to name %s", missing, out, missing)
		}
	}
}

func assertSecretMount(t *testing.T, d k8sDeployment, secret, dir string) {
	t.Helper()
	for _, v := range d.Spec.Template.Spec.Volumes {
		if v.Secret == nil || v.Secret.SecretName != secret {
			continue
		}
		if v.Secret.Optional == nil || !*v.Secret.Optional {
			t.Errorf("volume %s must be optional: true so a missing Secret still starts the pod", v.Name)
		}
		for _, c := range d.Spec.Template.Spec.Containers {
			for _, m := range c.VolumeMounts {
				if m.Name == v.Name {
					if m.MountPath != dir || !m.ReadOnly {
						t.Errorf("mount %s = %q readOnly=%v, want %s read-only", m.Name, m.MountPath, m.ReadOnly, dir)
					}
					return
				}
			}
		}
		t.Errorf("Secret %s is not mounted into a container", secret)
		return
	}
	t.Errorf("no Secret volume for %s", secret)
}
