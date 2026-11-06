package bridge

import "testing"

// TestCarriedBackend: a bridge re-mint is a fleet spawn, so a laptop agent's
// execution_target=laptop is not carried forward (gd-SzPJDNTBXS); every other
// backend is.
func TestCarriedBackend(t *testing.T) {
	for prior, want := range map[string]string{
		"":                 "",
		" lambda_microvm ": "lambda_microvm",
		"kubernetes":       "kubernetes",
		"laptop":           "",
	} {
		if got := carriedBackend(prior); got != want {
			t.Errorf("carriedBackend(%q) = %q, want %q", prior, got, want)
		}
	}
}
