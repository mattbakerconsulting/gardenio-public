package k3sauditshipper

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, b)
	}
	return m
}

const adminConfigMapWrite = `{"kind":"Event","apiVersion":"audit.k8s.io/v1","level":"RequestResponse","auditID":"a1","stage":"ResponseComplete","requestURI":"/api/v1/namespaces/gardenio/configmaps/cfg","verb":"update","user":{"username":"system:serviceaccount:gardenio:gardenio-infra-admin","groups":["system:serviceaccounts"],"extra":{"authentication.kubernetes.io/pod-name":["x"]}},"sourceIPs":["10.0.0.5"],"userAgent":"kubectl/v1","objectRef":{"resource":"configmaps","namespace":"gardenio","name":"cfg","apiVersion":"v1"},"responseStatus":{"code":200},"requestObject":{"kind":"ConfigMap","data":{"DB_PASSWORD":"hunter2"}},"responseObject":{"kind":"ConfigMap","data":{"DB_PASSWORD":"hunter2"}},"requestReceivedTimestamp":"2026-10-15T01:00:00.000000Z","stageTimestamp":"2026-10-15T01:00:00.100000Z"}`

func TestTransform_SensitiveKindBodiesNeverShip(t *testing.T) {
	for _, res := range []string{"configmaps", "secrets", "helmcharts", "helmchartconfigs", "serviceaccounts/token", "tokenreviews"} {
		base, sub, _ := strings.Cut(res, "/")
		ev := map[string]any{
			"kind": "Event", "verb": "update", "stageTimestamp": "2026-10-15T01:00:00Z",
			"user":          map[string]any{"username": "u"},
			"objectRef":     map[string]any{"resource": base, "subresource": sub, "name": "n", "namespace": "ns"},
			"requestObject": map[string]any{"spec": map[string]any{"valuesContent": "hunter2-ZZZ"}},
		}
		b, _ := json.Marshal(ev)
		out, keep := Transform(b, Options{})
		if !keep {
			t.Fatalf("%s: dropped", res)
		}
		if strings.Contains(string(out), "hunter2-ZZZ") {
			t.Errorf("%s: body leaked: %s", res, out)
		}
		rec := decode(t, out)
		if rec["requestObjectOmitted"] != "sensitive-kind" {
			t.Errorf("%s: want sensitive-kind marker, got %v", res, rec["requestObjectOmitted"])
		}
	}
}

func TestTransform_AdminConfigMapWrite(t *testing.T) {
	out, keep := Transform([]byte(adminConfigMapWrite), Options{Node: "ip-10-0-0-1"})
	if !keep {
		t.Fatal("dropped")
	}
	if strings.Contains(string(out), "hunter2") {
		t.Fatalf("ConfigMap data shipped: %s", out)
	}
	rec := decode(t, out)
	if rec["source"] != Source || rec["node"] != "ip-10-0-0-1" {
		t.Errorf("source/node = %v/%v", rec["source"], rec["node"])
	}
	if rec["time"] != "2026-10-15T01:00:00.100000Z" {
		t.Errorf("time = %v, want stageTimestamp", rec["time"])
	}
	want := "update configmaps gardenio/cfg by system:serviceaccount:gardenio:gardenio-infra-admin -> 200"
	if rec["msg"] != want {
		t.Errorf("msg = %q, want %q", rec["msg"], want)
	}
	user := rec["user"].(map[string]any)
	if _, ok := user["extra"]; ok {
		t.Error("user.extra should not be shipped")
	}
}

func TestTransform_NonSensitiveBodyKeysMasked(t *testing.T) {
	ev := `{"kind":"Event","verb":"create","stageTimestamp":"2026-10-15T01:00:00Z","user":{"username":"admin"},
	"objectRef":{"resource":"deployments","namespace":"ns","name":"d"},
	"requestObject":{"metadata":{"name":"d","managedFields":[{"x":1}],"annotations":{"kubectl.kubernetes.io/last-applied-configuration":"{\"a\":1}","keep":"me"}},
	 "spec":{"template":{"spec":{"containers":[{"name":"c","image":"i","env":[{"name":"API_TOKEN","value":"tok-123456"},{"name":"MODE","value":"fast"},{"name":"P","valueFrom":{"secretKeyRef":{"name":"s","key":"k"}}}]}]}}}}}`
	out, keep := Transform([]byte(ev), Options{})
	if !keep {
		t.Fatal("dropped")
	}
	s := string(out)
	for _, bad := range []string{"tok-123456", "managedFields", "last-applied-configuration"} {
		if strings.Contains(s, bad) {
			t.Errorf("output still contains %q: %s", bad, s)
		}
	}
	for _, good := range []string{"fast", `"keep":"me"`, `"image":"i"`, redacted} {
		if !strings.Contains(s, good) {
			t.Errorf("output lost %q: %s", good, s)
		}
	}
}

func TestTransform_SecretShapeScrubbedFromURI(t *testing.T) {
	ev := `{"kind":"Event","verb":"create","stageTimestamp":"2026-10-15T01:00:00Z","user":{"username":"admin"},
	"requestURI":"/api/v1/namespaces/ns/pods/p/exec?command=curl&command=-H&command=token%3Dsk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAAAAAA",
	"objectRef":{"resource":"pods","subresource":"exec","namespace":"ns","name":"p"}}`
	out, _ := Transform([]byte(ev), Options{})
	if strings.Contains(string(out), "AAAAAAAAAAAA") {
		t.Errorf("credential shipped: %s", out)
	}
}

func TestTransform_BodyModesAndCap(t *testing.T) {
	ev := `{"kind":"Event","verb":"create","stageTimestamp":"2026-10-15T01:00:00Z","user":{"username":"admin"},
	"objectRef":{"resource":"deployments","namespace":"ns","name":"d"},"requestObject":{"spec":{"x":"` + strings.Repeat("y", 200) + `"}}}`
	rec := decode(t, mustTransform(t, ev, Options{Bodies: BodiesNone}))
	if rec["requestObjectOmitted"] != "bodies-disabled" || rec["requestObject"] != nil {
		t.Errorf("none mode: %v", rec)
	}
	rec = decode(t, mustTransform(t, ev, Options{MaxBodyBytes: 100}))
	if s, _ := rec["requestObjectOmitted"].(string); !strings.HasPrefix(s, "oversize:") || rec["requestObject"] != nil {
		t.Errorf("cap: %v", rec)
	}
	rec = decode(t, mustTransform(t, ev, Options{}))
	if rec["requestObject"] == nil {
		t.Errorf("default mode dropped a small body: %v", rec)
	}
}

func mustTransform(t *testing.T, ev string, o Options) []byte {
	t.Helper()
	out, keep := Transform([]byte(ev), o)
	if !keep {
		t.Fatal("dropped")
	}
	return out
}

func TestTransform_DropsNonEvents(t *testing.T) {
	for _, in := range []string{"", "not json", `[1]`, `{"kind":"Pod"}`, `{"a":`} {
		if _, keep := Transform([]byte(in), Options{}); keep {
			t.Errorf("kept %q", in)
		}
	}
}

func TestTransform_DropReads(t *testing.T) {
	read := `{"kind":"Event","verb":"get","user":{"username":"someone"},"objectRef":{"resource":"pods"}}`
	adminRead := `{"kind":"Event","verb":"get","user":{"username":"admin"},"objectRef":{"resource":"pods"}}`
	write := `{"kind":"Event","verb":"delete","user":{"username":"someone"},"objectRef":{"resource":"pods"}}`
	o := Options{DropReads: true, KeepReadsUsers: []string{"admin"}}
	if _, keep := Transform([]byte(read), o); keep {
		t.Error("read by other user kept")
	}
	if _, keep := Transform([]byte(adminRead), o); !keep {
		t.Error("read by kept user dropped")
	}
	if _, keep := Transform([]byte(write), o); !keep {
		t.Error("write dropped")
	}
	if _, keep := Transform([]byte(read), Options{}); !keep {
		t.Error("reads should ship by default")
	}
}

func TestTransform_NumbersSurviveVerbatim(t *testing.T) {
	ev := `{"kind":"Event","verb":"create","user":{"username":"a"},"objectRef":{"resource":"deployments","name":"d"},"requestObject":{"spec":{"replicas":3,"big":12345678901234567890}}}`
	out := string(mustTransform(t, ev, Options{}))
	if !strings.Contains(out, `"replicas":3`) || !strings.Contains(out, "12345678901234567890") {
		t.Errorf("numbers mangled: %s", out)
	}
}

// A: RedactBytes over the serialized record let `token=` swallow a closing quote
// and produced invalid JSON; redaction is now per string value, and the output
// must always parse.
func TestTransform_TokenInValueKeepsOutputValidJSON(t *testing.T) {
	for _, uri := range []string{
		`/api/v1/namespaces/ns/pods/p/exec?command=curl&command=token%3Dabc123&command=http://x`,
		`/x?token=abc","evil":"1`,
		`/x?password=a"b\c`,
		"/x?secret= line",
	} {
		ev, _ := json.Marshal(map[string]any{
			"kind": "Event", "verb": "create", "requestURI": uri,
			"user":      map[string]any{"username": "admin"},
			"objectRef": map[string]any{"resource": "pods", "subresource": "exec", "namespace": "ns", "name": "p"},
		})
		out, st := TransformStatus(ev, Options{})
		if st != Kept {
			t.Fatalf("%q: status %v", uri, st)
		}
		rec := decode(t, out) // fails the test if the output does not parse
		if strings.Contains(string(out), "abc123") {
			t.Errorf("%q: credential shipped: %s", uri, out)
		}
		if rec["evil"] != nil {
			t.Errorf("%q: an attacker-controlled value injected a top-level field: %v", uri, rec)
		}
		if bytes.IndexByte(out, '\n') >= 0 {
			t.Errorf("%q: output spans lines", uri)
		}
	}
}

// A redaction that damages one value must not take down the neighbours.
func TestTransform_RedactionIsPerValue(t *testing.T) {
	ev := `{"kind":"Event","verb":"create","user":{"username":"admin"},"userAgent":"token=abc ua-keep",
	"requestURI":"/api/v1/namespaces/ns/pods","objectRef":{"resource":"pods","namespace":"ns","name":"p"}}`
	out := mustTransform(t, ev, Options{})
	rec := decode(t, out)
	if rec["requestURI"] != "/api/v1/namespaces/ns/pods" {
		t.Errorf("unrelated field altered: %v", rec["requestURI"])
	}
	if ua, _ := rec["userAgent"].(string); strings.Contains(ua, "abc") {
		t.Errorf("userAgent not redacted: %q", ua)
	}
}

func TestTransform_ResidualCredentialInKeyIsDropped(t *testing.T) {
	// redactTree rewrites values, not keys; the output gate catches the rest.
	ev := `{"kind":"Event","verb":"create","user":{"username":"admin"},
	"objectRef":{"resource":"deployments","namespace":"ns","name":"d"},
	"annotations":{"glpat-AAAAAAAAAAAAAAAAAAAA":"x"}}`
	out, st := TransformStatus([]byte(ev), Options{})
	if st != Unshippable || out != nil {
		t.Fatalf("status %v out %s", st, out)
	}
	if _, keep := Transform([]byte(ev), Options{}); keep {
		t.Error("Transform kept an unshippable event")
	}
}

func TestShippable(t *testing.T) {
	for in, want := range map[string]bool{
		`{"a":"b"}`:                         true,
		`{"a":"b"`:                          false, // truncated
		`[1]`:                               false,
		"{\"a\":\n\"b\"}":                   false, // spans lines
		``:                                  false,
		`{"a":"sk-ant-api03-AAAAAAAAAAAA"}`: false,
	} {
		if got := shippable([]byte(in)); got != want {
			t.Errorf("shippable(%q) = %v, want %v", in, got, want)
		}
	}
}

// B: credential shapes the claudemore patterns miss.
func TestRedactString_ExtraShapes(t *testing.T) {
	jwt := "eyJhbGciOiJSUzI1NiIsImtpZCI6IjEifQ.eyJzdWIiOiJhZG1pbiIsImV4cCI6MX0.c2lnbmF0dXJlLWJ5dGVz" // gitleaks:allow: fake credential for the redaction test
	for name, in := range map[string]string{
		"bearer header":   "curl -H Authorization: Bearer " + jwt + " https://k",
		"bare jwt":        "command=" + jwt,
		"bearer opaque":   "Bearer abcdEFGH0123456789xyz",
		"basic":           "authorization=Basic dXNlcjpwYXNzd29yZA==",
		"db dsn":          "command=DB_DSN=postgres://u:hunter2@db:5432/x",
		"github pat":      "GITHUB_PAT=aaaa1111bbbb",
		"api key":         "STRIPE_KEY=rk_live_abc123", // gitleaks:allow: fake credential for the redaction test
		"vendor secret":   "APP_SECRET=s3cr3t-value",
		"vendor token":    "SLACK_TOKEN=xoxb-1-2-3",
		"vendor password": "DB_PASSWORD=hunter2",
		"url userinfo":    "https://user:hunter2@example.com/x",
		"pem":             "-----BEGIN RSA PRIVATE KEY-----\nMIIBOgIBAAJBAKj34GkxFhD90vcNLYLInFEX6Ppy1tPf9Cnzj4p4WGeKLs1Pt8Qu\n-----END RSA PRIVATE KEY-----", // gitleaks:allow: fake credential for the redaction test
	} {
		out := redactString(in)
		for _, leak := range []string{"eyJhbGci", "eyJzdWIi", "abcdEFGH0123", "dXNlcjpw", "hunter2", "aaaa1111", "rk_live", "s3cr3t", "xoxb-", "MIIBOgIB"} {
			if strings.Contains(out, leak) {
				t.Errorf("%s: %q leaked in %q", name, leak, out)
			}
		}
		if out == in {
			t.Errorf("%s: not redacted", name)
		}
	}
	// Prose and ordinary identifiers survive.
	for _, keep := range []string{"the bearer of bad news", "basic configuration of the cluster", "path=/tmp/x", "monkey patch notes", "kubectl get pods -n kube-system"} {
		if got := redactString(keep); got != keep {
			t.Errorf("false positive: %q -> %q", keep, got)
		}
	}
}

func TestTransform_BearerJWTInExecArgs(t *testing.T) {
	jwt := "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJhZG1pbiJ9.c2lnbmF0dXJl" // gitleaks:allow: fake credential for the redaction test
	ev := `{"kind":"Event","verb":"create","user":{"username":"admin"},
	"requestURI":"/api/v1/namespaces/ns/pods/p/exec?command=curl&command=-H&command=Authorization%3A+Bearer+` + jwt + `&command=https://x",
	"objectRef":{"resource":"pods","subresource":"exec","namespace":"ns","name":"p"}}`
	out := mustTransform(t, ev, Options{})
	decode(t, out)
	if strings.Contains(string(out), "eyJ") {
		t.Errorf("JWT shipped: %s", out)
	}
}

func TestSensitiveKey_Conventions(t *testing.T) {
	for _, k := range []string{"STRIPE_KEY", "db_key", "DB_DSN", "SENTRY_DSN", "GITHUB_PAT", "GH_PAT", "APP_SECRET", "API_TOKEN", "DB_PASSWORD", "dbDsn", "signingKey", "dsn", "pat"} {
		if !sensitiveKey.MatchString(k) {
			t.Errorf("%q should be sensitive", k)
		}
	}
	for _, k := range []string{"key", "name", "image", "path", "patch", "monkey", "replicas", "labels"} {
		if sensitiveKey.MatchString(k) {
			t.Errorf("%q should not be sensitive", k)
		}
	}
}

func TestTransform_EnvNameConventionsMasked(t *testing.T) {
	ev := `{"kind":"Event","verb":"create","user":{"username":"admin"},
	"objectRef":{"resource":"deployments","namespace":"ns","name":"d"},
	"requestObject":{"spec":{"containers":[{"name":"c","env":[
	  {"name":"STRIPE_KEY","value":"zz-key-1"},{"name":"DB_DSN","value":"zz-dsn-2"},{"name":"GITHUB_PAT","value":"zz-pat-3"},{"name":"MODE","value":"fast"}]}]}}}`
	out := string(mustTransform(t, ev, Options{}))
	for _, bad := range []string{"zz-key-1", "zz-dsn-2", "zz-pat-3"} {
		if strings.Contains(out, bad) {
			t.Errorf("%s shipped: %s", bad, out)
		}
	}
	if !strings.Contains(out, "fast") {
		t.Errorf("non-sensitive env lost: %s", out)
	}
}

func TestTransform_ImpersonatedUserTrimmed(t *testing.T) {
	ev := `{"kind":"Event","verb":"get","user":{"username":"admin"},
	"impersonatedUser":{"username":"bob","uid":"u1","groups":["g"],"extra":{"authentication.kubernetes.io/credential-id":["JTI=abc"]}},
	"objectRef":{"resource":"pods"}}`
	rec := decode(t, mustTransform(t, ev, Options{}))
	iu := rec["impersonatedUser"].(map[string]any)
	if iu["username"] != "bob" || iu["uid"] != "u1" || iu["groups"] == nil {
		t.Errorf("impersonatedUser = %v", iu)
	}
	if _, ok := iu["extra"]; ok {
		t.Error("impersonatedUser.extra should not be shipped")
	}
}
