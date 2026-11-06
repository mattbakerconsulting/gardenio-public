package tests

import (
	"encoding/base64"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// Realm SMTP (gd-hTlQPf59qf). A GitHub registrant starts with
// email_verified=false, and Keycloak's VERIFY_EMAIL required action needs the
// realm's SMTP server to send the link. keycloak.realm.smtp renders
// KeycloakRealm.spec.smtp in the EDP 1.32.0 shape (api/common.SMTP): the
// password is a SourceRef (Secret/ConfigMap key) only, read from the realm's
// namespace, so these tests also pin that no credential lands in the CR.

const realmTemplate = "templates/keycloak/realm.yaml"

// sesSMTP is the SES SMTP shape the escalation asks the operator to set.
var sesSMTP = []string{
	"keycloak.enabled=true",
	"keycloak.realm.smtp.enabled=true",
	"keycloak.realm.smtp.host=email-smtp.us-east-1.amazonaws.com",
	"keycloak.realm.smtp.from=lands@lands.clonk.tv",
	"keycloak.realm.smtp.fromDisplayName=Gardenio",
	"keycloak.realm.smtp.replyTo=support@lands.clonk.tv",
	"keycloak.realm.smtp.auth.secretName=gardenio-keycloak-smtp",
}

// realmSMTP renders the realm and returns its spec.smtp (nil when absent).
func realmSMTP(t *testing.T, setFlags ...string) map[string]any {
	t.Helper()
	out := helmTemplate(t, realmTemplate, setFlags...)
	var doc struct {
		Spec map[string]any `json:"spec"`
	}
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("unmarshal realm: %v\n%s", err, out)
	}
	smtp, _ := doc.Spec["smtp"].(map[string]any)
	return smtp
}

func dig(t *testing.T, m map[string]any, path ...string) any {
	t.Helper()
	var cur any = m
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("path %v: %q is not under a map (got %T)", path, p, cur)
		}
		cur = mm[p]
	}
	return cur
}

// TestKeycloakRealmSMTP_DefaultOff: with default values the realm carries no
// spec.smtp, so rolling the chart changes nothing until the operator opts in.
func TestKeycloakRealmSMTP_DefaultOff(t *testing.T) {
	t.Parallel()
	if smtp := realmSMTP(t, "keycloak.enabled=true"); smtp != nil {
		t.Fatalf("smtp off by default: realm must not render spec.smtp, got %v", smtp)
	}
}

// TestKeycloakRealmSMTP_SESShape pins the rendered block for SES SMTP: port
// 587, STARTTLS, no SSL, and username and password both as secretKeyRefs.
func TestKeycloakRealmSMTP_SESShape(t *testing.T) {
	t.Parallel()
	smtp := realmSMTP(t, sesSMTP...)
	if smtp == nil {
		t.Fatal("smtp enabled: realm has no spec.smtp")
	}
	for _, c := range []struct {
		path []string
		want any
	}{
		{[]string{"template", "from"}, "lands@lands.clonk.tv"},
		{[]string{"template", "fromDisplayName"}, "Gardenio"},
		{[]string{"template", "replyTo"}, "support@lands.clonk.tv"},
		{[]string{"connection", "host"}, "email-smtp.us-east-1.amazonaws.com"},
		{[]string{"connection", "port"}, float64(587)},
		{[]string{"connection", "enableStartTLS"}, true},
		{[]string{"connection", "enableSSL"}, false},
		{[]string{"connection", "authentication", "username", "secretKeyRef", "name"}, "gardenio-keycloak-smtp"},
		{[]string{"connection", "authentication", "username", "secretKeyRef", "key"}, "username"},
		{[]string{"connection", "authentication", "password", "secretKeyRef", "name"}, "gardenio-keycloak-smtp"},
		{[]string{"connection", "authentication", "password", "secretKeyRef", "key"}, "password"},
	} {
		if got := dig(t, smtp, c.path...); got != c.want {
			t.Errorf("spec.smtp.%s = %v (%T), want %v", strings.Join(c.path, "."), got, got, c.want)
		}
	}
	// Optional template fields left empty do not render.
	for _, k := range []string{"replyToDisplayName", "envelopeFrom"} {
		if _, ok := dig(t, smtp, "template").(map[string]any)[k]; ok {
			t.Errorf("spec.smtp.template.%s rendered though unset", k)
		}
	}
}

// TestKeycloakRealmSMTP_PasswordNeverInline: the password is a secretKeyRef
// only (the CRD has no value form for it), and there is no values knob that
// could put one in the CR.
func TestKeycloakRealmSMTP_PasswordNeverInline(t *testing.T) {
	t.Parallel()
	smtp := realmSMTP(t, append(sesSMTP, "keycloak.realm.smtp.auth.password=hunter2")...)
	pw, _ := dig(t, smtp, "connection", "authentication", "password").(map[string]any)
	if len(pw) != 1 || pw["secretKeyRef"] == nil {
		t.Errorf("password must be exactly a secretKeyRef, got %v", pw)
	}
	if out := helmTemplate(t, realmTemplate, append(sesSMTP, "keycloak.realm.smtp.auth.password=hunter2")...); strings.Contains(out, "hunter2") {
		t.Error("a password in values must never reach the rendered CR")
	}
}

// TestKeycloakRealmSMTP_PlainUsername: auth.username renders as a value and
// the password still comes from the Secret.
func TestKeycloakRealmSMTP_PlainUsername(t *testing.T) {
	t.Parallel()
	smtp := realmSMTP(t, append(sesSMTP, "keycloak.realm.smtp.auth.username=AKIAEXAMPLE")...)
	user, _ := dig(t, smtp, "connection", "authentication", "username").(map[string]any)
	if user["value"] != "AKIAEXAMPLE" || user["secretKeyRef"] != nil {
		t.Errorf("username = %v, want only value AKIAEXAMPLE", user)
	}
	if dig(t, smtp, "connection", "authentication", "password", "secretKeyRef", "name") != "gardenio-keycloak-smtp" {
		t.Error("password must still come from the Secret")
	}
}

// TestKeycloakRealmSMTP_NoAuth: auth.enabled=false drops authentication (the
// operator then sets auth=false on the realm) and needs no Secret.
func TestKeycloakRealmSMTP_NoAuth(t *testing.T) {
	t.Parallel()
	smtp := realmSMTP(t,
		"keycloak.enabled=true",
		"keycloak.realm.smtp.enabled=true",
		"keycloak.realm.smtp.host=mail.internal",
		"keycloak.realm.smtp.port=25",
		"keycloak.realm.smtp.starttls=false",
		"keycloak.realm.smtp.from=noreply@example.com",
		"keycloak.realm.smtp.auth.enabled=false",
	)
	if _, ok := dig(t, smtp, "connection").(map[string]any)["authentication"]; ok {
		t.Error("auth.enabled=false must not render connection.authentication")
	}
	if got := dig(t, smtp, "connection", "port"); got != float64(25) {
		t.Errorf("port = %v, want 25", got)
	}
}

// TestKeycloakRealmSMTP_Guards: enabled without host, from or (with auth) a
// Secret name fails the render, and so do ssl and starttls together.
func TestKeycloakRealmSMTP_Guards(t *testing.T) {
	t.Parallel()
	without := func(drop string) []string {
		var args []string
		for _, s := range sesSMTP {
			if !strings.HasPrefix(s, drop+"=") {
				args = append(args, "--set", s)
			}
		}
		return args
	}
	for _, c := range []struct {
		name string
		args []string
		want string
	}{
		{"no host", without("keycloak.realm.smtp.host"), "keycloak.realm.smtp.host is required"},
		{"no from", without("keycloak.realm.smtp.from"), "keycloak.realm.smtp.from is required"},
		{"no secret", without("keycloak.realm.smtp.auth.secretName"), "keycloak.realm.smtp.auth.secretName is required"},
		{"ssl and starttls", append(without(""), "--set", "keycloak.realm.smtp.ssl=true"), "not both"},
	} {
		t.Run(c.name, func(t *testing.T) {
			out := helmTemplateExpectError(t, realmTemplate, c.args...)
			if !strings.Contains(out, c.want) {
				t.Errorf("want error containing %q, got:\n%s", c.want, out)
			}
		})
	}
}

// ── Username via lookup (gd-M8NN95y1Xh) ──────────────────────────────────────
//
// keycloak-operator 1.32.0 rewrites the realm CR on reconcile with
// username: {} for a secretKeyRef (and configMapKeyRef); only username.value
// survives, so a ref-only username leaves Keycloak with an empty SMTP user and
// every send fails "535 AUTH failed". The realm template therefore reads the
// username from the Secret with lookup and renders it as a value, and keeps the
// secretKeyRef when lookup finds nothing (helm template, first install).
//
// helm template has no API server, so lookup always returns an empty map. To
// drive the found branch through the real template, these tests render a copy
// of the chart whose lookup call is swapped for a stub that answers only for
// the expected namespace and Secret name.

// realmLookupCall is the exact lookup the realm template makes.
const realmLookupCall = `lookup "v1" "Secret" $.Release.Namespace $secret`

// realmSMTPWithSecret renders spec.smtp in namespace ns, with lookup answering
// a Secret named name in namespace secretNS that holds data (plain values,
// base64-encoded by the stub as the API server would).
func realmSMTPWithSecret(t *testing.T, ns, secretNS, name string, data map[string]string, setFlags ...string) map[string]any {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "gardenio")
	src := chartDir(t)
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		if d.IsDir() {
			if rel == "tests" || rel == "dashboards" {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(dir, rel), 0o755)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, rel), b, 0o644)
	})
	if err != nil {
		t.Fatalf("copy chart: %v", err)
	}

	realm := filepath.Join(dir, realmTemplate)
	b, err := os.ReadFile(realm)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(b), realmLookupCall); n != 1 {
		t.Fatalf("%s must make exactly one %q, found %d", realmTemplate, realmLookupCall, n)
	}
	stubbed := strings.Replace(string(b), realmLookupCall,
		`fromYaml (include "test.smtpSecretLookup" (list "v1" "Secret" $.Release.Namespace $secret))`, 1)
	if err := os.WriteFile(realm, []byte(stubbed), 0o644); err != nil {
		t.Fatal(err)
	}

	var stub strings.Builder
	stub.WriteString(`{{- define "test.smtpSecretLookup" -}}` + "\n")
	fmt.Fprintf(&stub, `{{- if and (eq (index . 0) "v1") (eq (index . 1) "Secret") (eq (index . 2) %q) (eq (index . 3) %q) -}}`+"\n", secretNS, name)
	stub.WriteString("data:\n")
	for k, v := range data {
		fmt.Fprintf(&stub, "  %s: %s\n", k, base64.StdEncoding.EncodeToString([]byte(v)))
	}
	stub.WriteString("{{- end -}}\n{{- end -}}\n")
	if err := os.WriteFile(filepath.Join(dir, "templates", "_test_lookup.tpl"), []byte(stub.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	args := []string{"template", "test", dir, "--namespace", ns, "--show-only", realmTemplate}
	for _, s := range setFlags {
		args = append(args, "--set", s)
	}
	out, err := exec.Command("helm", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, out)
	}
	var doc struct {
		Spec map[string]any `json:"spec"`
	}
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("unmarshal realm: %v\n%s", err, out)
	}
	smtp, _ := doc.Spec["smtp"].(map[string]any)
	if smtp == nil {
		t.Fatalf("smtp enabled: realm has no spec.smtp\n%s", out)
	}
	return smtp
}

func smtpUsername(t *testing.T, smtp map[string]any) map[string]any {
	t.Helper()
	user, _ := dig(t, smtp, "connection", "authentication", "username").(map[string]any)
	return user
}

// TestKeycloakRealmSMTP_UsernameFromLookup: with the Secret in the release
// namespace, the username renders as a value (the only form the operator
// keeps) and the password stays a secretKeyRef.
func TestKeycloakRealmSMTP_UsernameFromLookup(t *testing.T) {
	t.Parallel()
	smtp := realmSMTPWithSecret(t, "gardenio", "gardenio", "gardenio-keycloak-smtp",
		map[string]string{"username": "AKIALOOKUPEXAMPLE", "password": "derived-smtp-pw"}, sesSMTP...)
	if user := smtpUsername(t, smtp); len(user) != 1 || user["value"] != "AKIALOOKUPEXAMPLE" {
		t.Errorf("username = %v, want only value AKIALOOKUPEXAMPLE from the Secret", user)
	}
	pw, _ := dig(t, smtp, "connection", "authentication", "password").(map[string]any)
	if len(pw) != 1 || dig(t, smtp, "connection", "authentication", "password", "secretKeyRef", "name") != "gardenio-keycloak-smtp" {
		t.Errorf("password must stay exactly a secretKeyRef, got %v", pw)
	}
	if strings.Contains(fmt.Sprint(smtp), "derived-smtp-pw") {
		t.Error("the Secret's password must never be read into the CR")
	}
}

// TestKeycloakRealmSMTP_UsernameLookupCustomKey: lookup reads usernameKey,
// not a hard-coded "username".
func TestKeycloakRealmSMTP_UsernameLookupCustomKey(t *testing.T) {
	t.Parallel()
	smtp := realmSMTPWithSecret(t, "gardenio", "gardenio", "gardenio-keycloak-smtp",
		map[string]string{"username": "wrong", "smtp-user": "AKIACUSTOMKEY"},
		append(sesSMTP, "keycloak.realm.smtp.auth.usernameKey=smtp-user")...)
	if user := smtpUsername(t, smtp); user["value"] != "AKIACUSTOMKEY" {
		t.Errorf("username = %v, want value AKIACUSTOMKEY from usernameKey smtp-user", user)
	}
}

// TestKeycloakRealmSMTP_UsernameLookupFallsBackToRef: when lookup finds no
// usable username (Secret in another namespace, Secret without the key, or no
// cluster at all), the secretKeyRef renders exactly as before.
func TestKeycloakRealmSMTP_UsernameLookupFallsBackToRef(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name     string
		secretNS string
		data     map[string]string
	}{
		{"secret in another namespace", "elsewhere", map[string]string{"username": "AKIAOTHERNS"}},
		{"secret without username key", "gardenio", map[string]string{"password": "x"}},
		{"secret with empty username", "gardenio", map[string]string{"username": ""}},
	} {
		t.Run(c.name, func(t *testing.T) {
			smtp := realmSMTPWithSecret(t, "gardenio", c.secretNS, "gardenio-keycloak-smtp", c.data, sesSMTP...)
			user := smtpUsername(t, smtp)
			if user["value"] != nil || dig(t, smtp, "connection", "authentication", "username", "secretKeyRef", "key") != "username" {
				t.Errorf("username = %v, want the secretKeyRef fallback", user)
			}
		})
	}
	// No API server: plain helm template, lookup returns an empty map.
	if user := smtpUsername(t, realmSMTP(t, sesSMTP...)); user["value"] != nil || user["secretKeyRef"] == nil {
		t.Errorf("without a cluster username = %v, want the secretKeyRef", user)
	}
}

// TestKeycloakRealmSMTP_PlainUsernameBeatsLookup: an explicit auth.username
// wins over the Secret.
func TestKeycloakRealmSMTP_PlainUsernameBeatsLookup(t *testing.T) {
	t.Parallel()
	smtp := realmSMTPWithSecret(t, "gardenio", "gardenio", "gardenio-keycloak-smtp",
		map[string]string{"username": "AKIAFROMSECRET"},
		append(sesSMTP, "keycloak.realm.smtp.auth.username=AKIAFROMVALUES")...)
	if user := smtpUsername(t, smtp); user["value"] != "AKIAFROMVALUES" {
		t.Errorf("username = %v, want value AKIAFROMVALUES from values", user)
	}
}
