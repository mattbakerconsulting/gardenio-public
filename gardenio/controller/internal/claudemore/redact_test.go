package claudemore

import (
	"strings"
	"testing"
)

// TestRedactBytes_BareCredentialShapes is the A4 / R4-SEC-3 core: a reasoning
// agent can echo a BARE provider credential (no key= prefix) into the wire, and
// it must not survive into a cassette. Mirrors coop's
// redacts_bare_credential_shapes test so the two redactors cannot drift.
func TestRedactBytes_BareCredentialShapes(t *testing.T) {
	secrets := []struct {
		name string
		in   string
		leak string // substring that must NOT appear post-redaction
	}{
		{"anthropic oauth", "using key sk-ant-oat01-AbCdEf012345_gh-ijkl for auth", "oat01-AbCdEf012345"},
		{"anthropic api", "ANTHROPIC_API_KEY=sk-ant-api03-ZZZZ1111bbbbCCCC", "api03-ZZZZ1111"},
		{"litellm sk", "master sk-1234567890abcdefABCDEF was minted", "1234567890abcdefABCDEF"},
		{"litellm vk", "virtual key vk-abcdEFGH01234567 minted", "abcdEFGH01234567"},
		{"github pat", "token ghp_aaaaBBBBccccDDDDeeee here", "aaaaBBBBccccDDDD"},
		{"gitlab pat", "glpat-xxxxYYYYzzzz1111 leaked", "glpat-xxxxYYYYzzzz1111"},
		{"github_pat", "github_pat_11ABCDEFG0abcdefghij leaked", "github_pat_11ABCDEFG0"},
	}
	for _, s := range secrets {
		t.Run(s.name, func(t *testing.T) {
			out := RedactString(s.in)
			if strings.Contains(out, s.leak) {
				t.Fatalf("secret leaked: %q still contains %q", out, s.leak)
			}
			if !strings.Contains(out, RedactedValue) {
				t.Fatalf("expected redaction marker in %q", out)
			}
			if ContainsSecretShape([]byte(out)) {
				t.Fatalf("ContainsSecretShape still true after redaction: %q", out)
			}
		})
	}
}

// Fake JWT (header {"alg":"RS256","typ":"JWT"}, payload {"sub":"user-123"}); not a real credential.
const testJWT = "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJ1c2VyLTEyMyJ9.c2lnbmF0dXJlLXZhbHVlLTEyMw" // gitleaks:allow: fake JWT for the redaction test

// TestRedactBytes_JWTAndBearer is the OpenAI/ChatGPT-cassette prerequisite: an
// `Authorization: Bearer eyJ…` header, a bare JWT in a JSON string, and a
// non-JWT opaque bearer token must all be redacted with no keyword prefix.
func TestRedactBytes_JWTAndBearer(t *testing.T) {
	cases := []struct {
		name string
		in   string
		leak string
	}{
		{"authorization header jwt", "Authorization: Bearer " + testJWT, "eyJhbGci"},
		{"bearer jwt lowercase", "authorization: bearer " + testJWT, "eyJzdWIi"},
		{"bearer opaque", "Authorization: Bearer abcDEF0123456789xyzXYZ==", "abcDEF0123456789"},
		{"bearer tab-delimited", "Bearer\tabcDEF0123456789xyzXYZ", "abcDEF0123456789"},
		{"bare jwt in json", `{"id_token":"` + testJWT + `","x":1}`, "c2lnbmF0dXJl"},
		{"bare jwt in prose", "the agent printed " + testJWT + " to the log", "eyJhbGci"},
		{"jwt empty signature", "eyJhbGciOiJub25lIn0.eyJzdWIiOiJ4eHh4In0.", "eyJzdWIi"}, // gitleaks:allow: fake JWT for the redaction test
		{"json escaped header", `{"headers":{"Authorization":"Bearer ` + testJWT + `"}}`, "eyJhbGci"},
		{"keyword then bearer", "token: Bearer abcDEF0123456789xyzXYZ", "abcDEF0123456789"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := RedactString(c.in)
			if strings.Contains(out, c.leak) {
				t.Fatalf("secret leaked: %q still contains %q", out, c.leak)
			}
			if !strings.Contains(out, RedactedValue) {
				t.Fatalf("expected redaction marker in %q", out)
			}
			if !ContainsSecretShape([]byte(c.in)) {
				t.Fatalf("ContainsSecretShape false on unredacted input %q", c.in)
			}
			if ContainsSecretShape([]byte(out)) {
				t.Fatalf("ContainsSecretShape still true after redaction: %q", out)
			}
		})
	}
}

func TestRedactBytes_BearerKeepsScheme(t *testing.T) {
	got := RedactString("Authorization: Bearer " + testJWT)
	if want := "Authorization: Bearer " + RedactedValue; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestRedactBytes_KeywordShapes covers the key=value / token: value pass.
func TestRedactBytes_KeywordShapes(t *testing.T) {
	cases := []struct {
		in   string
		leak string
	}{
		{"$ export API_KEY=sk-live-supersecret123", "supersecret123"},
		{"password: hunter2", "hunter2"},
		{"bearer: eyJhbGciOiJIUzI1NiJ9.payload.sig", "eyJhbGciOiJIUzI1NiJ9"},
		{"secret = topsecret", "topsecret"},
	}
	for _, c := range cases {
		out := RedactString(c.in)
		if strings.Contains(out, c.leak) {
			t.Errorf("keyword secret leaked: %q still contains %q", out, c.leak)
		}
		if !strings.Contains(out, RedactedValue) {
			t.Errorf("expected redaction marker in %q", out)
		}
	}
}

// TestRedactBytes_PreservesProse asserts the redactor does not eat non-secret
// text — a cassette must stay useful. Also guards the false-positive floor: a
// short hyphenated identifier that merely starts with sk-/vk- must survive.
func TestRedactBytes_PreservesProse(t *testing.T) {
	clean := []string{
		"plain narrative with no secret at all",
		"the model chose the Read tool then Edit",
		"task-sk-workaround is a bead slug, not a key", // sk- but <16 body
		"vk-short", // vk- but far below floor
		`{"model":"claude-sonnet-5","messages":[]}`,
		"bearer authentication is required",          // Bearer + short word
		"the bearer of bad news",                     // prose
		"Bearer",                                     // scheme word alone
		"eyJ is the base64 prefix of a JSON object.", // eyJ without JWT shape
		"see eyJhbGci.docs for details",              // two segments only
	}
	for _, s := range clean {
		out := RedactString(s)
		if out != s {
			t.Errorf("clean text mutated:\n in: %q\nout: %q", s, out)
		}
		if ContainsSecretShape([]byte(s)) {
			t.Errorf("ContainsSecretShape false positive on %q", s)
		}
	}
}

// TestRedactBytes_KeywordThenBareCollapses verifies token=sk-… collapses to a
// single marker (keyword pass first), not a doubled/partial redaction.
func TestRedactBytes_KeywordThenBareCollapses(t *testing.T) {
	out := RedactString("token=sk-ant-oat01-AbCdEf012345secret")
	if strings.Contains(out, "AbCdEf012345") {
		t.Fatalf("secret leaked: %q", out)
	}
	if n := strings.Count(out, RedactedValue); n != 1 {
		t.Fatalf("expected exactly one redaction marker, got %d: %q", n, out)
	}
}

func TestRedactBytes_Empty(t *testing.T) {
	if got := RedactBytes(nil); got != nil {
		t.Errorf("RedactBytes(nil) = %v, want nil", got)
	}
	if got := RedactString(""); got != "" {
		t.Errorf("RedactString(\"\") = %q, want empty", got)
	}
}
