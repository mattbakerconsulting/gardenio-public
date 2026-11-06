package main

import "testing"

// TestResolveSpendReadBaseURL asserts the spend READ path resolves litellm
// directly at base_url (or the global fallback) and — by construction — has no
// way to route through a fronting proxy: there is no proxyURL parameter. This is
// the load-bearing half of the claudemore transparency property A1
// (ka-0000007533): claudemore meters no spend, so the controller reads spend
// from litellm directly whether or not a proxy fronts the write path.
func TestResolveSpendReadBaseURL(t *testing.T) {
	tests := []struct {
		name            string
		baseURL, defURL string
		want            string
	}{
		{"base_url wins", "http://litellm:4000", "http://global:4000", "http://litellm:4000"},
		{"global fallback when no base_url", "", "http://global:4000", "http://global:4000"},
		{"trailing slash trimmed on base_url", "http://litellm:4000/", "", "http://litellm:4000"},
		{"trailing slash trimmed on default", "", "http://global:4000/", "http://global:4000"},
		{"whitespace trimmed", "  http://litellm:4000  ", "", "http://litellm:4000"},
		{"empty when neither", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveSpendReadBaseURL(tt.baseURL, tt.defURL); got != tt.want {
				t.Errorf("resolveSpendReadBaseURL(%q, %q) = %q, want %q",
					tt.baseURL, tt.defURL, got, tt.want)
			}
		})
	}
}

// TestSpendTransparency_ReadPathIgnoresProxyURL is the A1 crux stated as a
// contract between the WRITE path and the READ path. For the SAME account, the
// pod-spawn write path (resolveAccountBaseURL) routes agent traffic THROUGH
// claudemore (honoring proxy_url), while the controller's spend read path
// (resolveSpendReadBaseURL) resolves litellm DIRECTLY (ignoring proxy_url). The
// two must diverge exactly on proxy_url — that divergence is what keeps spend
// metering byte-identical to a litellm-direct baseline while claudemore fronts
// the write path (R5 §5.2 cost fence).
//
// A regression that "unified" the read path onto resolveAccountBaseURL — a
// tempting cleanup — would make write==read here and reds this test, catching
// the silent metering break before it ships.
func TestSpendTransparency_ReadPathIgnoresProxyURL(t *testing.T) {
	const (
		proxyURL = "http://gardenio-claudemore:8100" // claudemore fronts the write path
		baseURL  = "http://gardenio-litellm:4000"    // litellm — the metering source of truth
		global   = "http://litellm-fallback:4000"
	)

	// WRITE path: agents point ANTHROPIC_BASE_URL at claudemore when proxy_url set.
	write := resolveAccountBaseURL(proxyURL, baseURL, global)
	if write != proxyURL {
		t.Fatalf("write path (resolveAccountBaseURL) = %q, want claudemore proxy %q — the write path must honor proxy_url", write, proxyURL)
	}

	// READ path: the controller reads spend from litellm directly, bypassing
	// claudemore, even though proxy_url is set on the account.
	read := resolveSpendReadBaseURL(baseURL, global)
	if read != baseURL {
		t.Fatalf("read path (resolveSpendReadBaseURL) = %q, want litellm base_url %q — the spend read path must bypass claudemore", read, baseURL)
	}
	if read == proxyURL {
		t.Fatalf("read path resolved to the claudemore proxy %q — spend must never be read through claudemore (A1 transparency)", proxyURL)
	}
	if read == write {
		t.Fatalf("read and write paths both resolved to %q — they MUST diverge on proxy_url so metering stays litellm-direct (A1)", read)
	}

	// A2 rollback corollary: with proxy_url cleared, the write path falls back to
	// the same litellm base_url the read path already used — so metering is
	// unchanged across a claudemore rollback (identical read endpoint before and
	// after), which is what makes the baseline "identical" claim hold.
	writeRolledBack := resolveAccountBaseURL("", baseURL, global)
	if writeRolledBack != read {
		t.Errorf("after proxy_url rollback, write path = %q but spend read path = %q — they must coincide on litellm-direct", writeRolledBack, read)
	}
}
