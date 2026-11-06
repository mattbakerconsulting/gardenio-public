package tests

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// The mailer's own SES keys (gd-kSLbp23ekV, Lands gate 5).
//
// Invariants locked here:
//   - agents.mailer.credentialsSecret renders nothing unless both the mailer
//     is enabled and credentialsSecret.name is set: no
//     MAILER_SES_CREDENTIALS_DIR, no mailer-ses-credentials mount or volume.
//   - Set, the two keys are a read-only file mount (projected to accessKeyId
//     and secretAccessKey); the Secret is never an env var.
//   - The in-box MinIO AWS_ACCESS_KEY_ID/SECRET env the rest of the
//     controller's AWS chain uses renders exactly as it does without it.

const (
	mailerOn = "agents.enabled=true,objectStore.enabled=true,agents.s3.endpoint=http://minio:9000," +
		"agents.mailer.enabled=true,agents.mailer.from=lands@x.example,agents.mailer.baseUrl=https://x.example"
	mailerSESSecret = "gardenio-mailer-ses"
	mailerSESDir    = "/var/run/secrets/gardenio-mailer-ses"
)

func TestMailerCredentials_UnsetRendersNothing(t *testing.T) {
	t.Parallel()
	for name, set := range map[string]string{
		"mailer on, no secret":  mailerOn,
		"secret, mailer off":    "agents.enabled=true,agents.mailer.credentialsSecret.name=" + mailerSESSecret,
		"mailer and secret off": "agents.enabled=true",
	} {
		t.Run(name, func(t *testing.T) {
			out, _ := renderControllerPodSpec(t, set)
			for _, forbidden := range []string{"MAILER_SES_CREDENTIALS_DIR", "mailer-ses-credentials", mailerSESDir} {
				if strings.Contains(out, forbidden) {
					t.Errorf("controller Deployment must not reference %q", forbidden)
				}
			}
		})
	}
}

func TestMailerCredentials_SetMountsReadOnlyFiles(t *testing.T) {
	t.Parallel()
	out, d := renderControllerPodSpec(t, mailerOn+
		",agents.mailer.credentialsSecret.name="+mailerSESSecret+
		",agents.mailer.credentialsSecret.accessKeyIdKey=AWS_ACCESS_KEY_ID"+
		",agents.mailer.credentialsSecret.secretAccessKeyKey=AWS_SECRET_ACCESS_KEY")
	c := d.Spec.Template.Spec.Containers[0]

	if got := envValue(t, out, "MAILER_SES_CREDENTIALS_DIR"); got != mailerSESDir {
		t.Errorf("MAILER_SES_CREDENTIALS_DIR = %q, want %q", got, mailerSESDir)
	}
	for _, e := range c.Env {
		if e.ValueFrom != nil && strings.Contains(fmt.Sprint(e.ValueFrom), mailerSESSecret) {
			t.Errorf("env %s reads the mailer SES Secret; it must only be a file mount", e.Name)
		}
	}

	mounted := false
	for _, m := range c.VolumeMounts {
		if m.Name == "mailer-ses-credentials" {
			mounted = true
			if m.MountPath != mailerSESDir || !m.ReadOnly {
				t.Errorf("mailer-ses-credentials mount = %+v, want read-only at %s", m, mailerSESDir)
			}
		}
	}
	if !mounted {
		t.Error("mailer-ses-credentials is not mounted")
	}

	var found bool
	for _, v := range d.Spec.Template.Spec.Volumes {
		if v.Name != "mailer-ses-credentials" {
			continue
		}
		found = true
		if v.Secret == nil || v.Secret.SecretName != mailerSESSecret || !v.Secret.Optional {
			t.Fatalf("mailer-ses-credentials volume = %+v, want optional Secret %s", v.Secret, mailerSESSecret)
		}
		got := map[string]string{}
		for _, it := range v.Secret.Items {
			got[it.Key] = it.Path
		}
		want := map[string]string{"AWS_ACCESS_KEY_ID": "accessKeyId", "AWS_SECRET_ACCESS_KEY": "secretAccessKey"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("mailer-ses-credentials items = %v, want %v", got, want)
		}
	}
	if !found {
		t.Error("mailer-ses-credentials volume is missing")
	}
}

func TestMailerCredentials_LeavesMinIOEnvUntouched(t *testing.T) {
	t.Parallel()
	awsEnv := func(set string) map[string]string {
		_, d := renderControllerPodSpec(t, set)
		env := map[string]string{}
		for _, e := range d.Spec.Template.Spec.Containers[0].Env {
			if strings.HasPrefix(e.Name, "AWS_") {
				env[e.Name] = e.Value + fmt.Sprint(e.ValueFrom)
			}
		}
		return env
	}
	without := awsEnv(mailerOn)
	with := awsEnv(mailerOn + ",agents.mailer.credentialsSecret.name=" + mailerSESSecret)
	if _, ok := without["AWS_ACCESS_KEY_ID"]; !ok {
		t.Fatalf("baseline render has no MinIO AWS_ACCESS_KEY_ID: %v", without)
	}
	if !reflect.DeepEqual(with, without) {
		t.Errorf("AWS_* env changed with credentialsSecret set:\n got  %v\n want %v", with, without)
	}
}
