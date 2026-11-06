package server

import "testing"

// TestRedactCredShapes covers the F17 (R4-SEC-4) bare-credential-shape scrubber
// applied to agent-authored bead prose (comment text, description, notes).
func TestRedactCredShapes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "empty is unchanged",
			in:   "",
			want: "",
		},
		{
			name: "clean prose is unchanged",
			in:   "the release is green and merged_today went 0->1",
			want: "the release is green and merged_today went 0->1",
		},
		{
			name: "anthropic sk-ant key with no keyword prefix",
			in:   "auth failed with sk-ant-oat01-AbCdEf012345_gh-ijkl retrying",
			want: "auth failed with <redacted> retrying",
		},
		{
			name: "litellm sk- virtual key",
			in:   "x-litellm-api-key was sk-1234567890abcdefABCDEF",
			want: "x-litellm-api-key was <redacted>",
		},
		{
			name: "litellm vk- key",
			in:   "minted vk-abcdEFGH01234567 for the run",
			want: "minted <redacted> for the run",
		},
		{
			name: "github ghp_ token",
			in:   "cloned using ghp_aaaaBBBBccccDDDDeeeeFFFF1234 ok",
			want: "cloned using <redacted> ok",
		},
		{
			name: "gitlab glpat token",
			in:   "GITLAB_TOKEN=glpat-ABCdef0123456789xyzQ", // gitleaks:allow: fake token for the redaction test
			want: "GITLAB_TOKEN=<redacted>",
		},
		{
			name: "github_pat_ fine-grained token",
			in:   "token github_pat_11ABCDEFG0abcdefghij present",
			want: "token <redacted> present",
		},
		{
			name: "multiple secrets in one string",
			in:   "sk-ant-AAAAAAAA and vk-BBBBBBBBCCCCDDDD both leaked",
			want: "<redacted> and <redacted> both leaked",
		},
		{
			name: "short sk- like word is not a credential (below body length)",
			in:   "we use sk-1 as a shorthand",
			want: "we use sk-1 as a shorthand",
		},
		{
			// Regression: a bare 2-char litellm prefix inside an ordinary
			// hyphenated identifier must NOT be redacted (body below the 16-char
			// floor) — the redactor mutates durable prose, so a false positive
			// corrupts legitimate content.
			name: "hyphenated word with sk- fragment is preserved",
			in:   "the task-sk-workaround shipped and disk-space is low",
			want: "the task-sk-workaround shipped and disk-space is low",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := redactCredShapes(tc.in); got != tc.want {
				t.Errorf("redactCredShapes(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestRedactCredShapes_PreservesShortHyphenatedWords guards against the scrubber
// eating ordinary hyphenated identifiers that merely start with a matching
// prefix — the body must be at least 8 chars of base64url/hex before it fires.
func TestRedactCredShapes_PreservesShortHyphenatedWords(t *testing.T) {
	for _, s := range []string{"sk-", "vk-", "sk-abc", "ghp_x", "the sky-blue color"} {
		if got := redactCredShapes(s); got != s {
			t.Errorf("redactCredShapes(%q) unexpectedly changed to %q", s, got)
		}
	}
}
