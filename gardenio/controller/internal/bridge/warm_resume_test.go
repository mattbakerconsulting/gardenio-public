package bridge

import (
	"log/slog"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// TestWarmPoolResumeEnabled_DefaultOff verifies the feature is OFF unless the
// env flag is set to a truthy value — the fail-safe default for the warm-pool
// coop-resume graft (ka-0000000983).
func TestWarmPoolResumeEnabled_DefaultOff(t *testing.T) {
	cases := []struct {
		name string
		set  bool
		val  string
		want bool
	}{
		{"unset", false, "", false},
		{"empty", true, "", false},
		{"false", true, "false", false},
		{"zero", true, "0", false},
		{"garbage", true, "yepnope", false},
		{"true", true, "true", true},
		{"one", true, "1", true},
		{"TRUE", true, "TRUE", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv(warmPoolResumeEnabledEnv, tc.val)
			} else {
				// Ensure any ambient value is cleared for the "unset" case.
				t.Setenv(warmPoolResumeEnabledEnv, "")
			}
			if got := warmPoolResumeEnabled(); got != tc.want {
				t.Errorf("warmPoolResumeEnabled() with %q = %v, want %v", tc.val, got, tc.want)
			}
		})
	}
}

// TestResolveWarmResumeS3Key exercises the prior-transcript key resolution and
// the feature gate.
func TestResolveWarmResumeS3Key(t *testing.T) {
	b := &Bot{logger: slog.Default()}

	// Feature OFF → always "", even with a resolvable prior bead.
	t.Run("feature-off", func(t *testing.T) {
		t.Setenv(warmPoolResumeEnabledEnv, "")
		prior := &beadsapi.BeadDetail{ID: "gd-prior"}
		if got := b.resolveWarmResumeS3Key(prior, "agent-x"); got != "" {
			t.Errorf("feature off must return \"\", got %q", got)
		}
	})

	// Feature ON, no prior bead → "".
	t.Run("on-no-prior", func(t *testing.T) {
		t.Setenv(warmPoolResumeEnabledEnv, "true")
		if got := b.resolveWarmResumeS3Key(nil, "agent-x"); got != "" {
			t.Errorf("nil prior bead must return \"\", got %q", got)
		}
	})

	// Feature ON, prior bead with no canonical field → falls back to bead ID
	// (the prefix coop keyed its transcript uploads on).
	t.Run("on-fallback-to-id", func(t *testing.T) {
		t.Setenv(warmPoolResumeEnabledEnv, "true")
		prior := &beadsapi.BeadDetail{ID: "gd-prior-id"}
		if got := b.resolveWarmResumeS3Key(prior, "agent-x"); got != "gd-prior-id" {
			t.Errorf("expected fallback to bead ID gd-prior-id, got %q", got)
		}
	})

	// Feature ON, prior bead carries an explicit canonical key → prefer it over
	// the bead ID (a respawn chain must follow the ORIGINAL prefix).
	t.Run("on-prefers-canonical-field", func(t *testing.T) {
		t.Setenv(warmPoolResumeEnabledEnv, "1")
		prior := &beadsapi.BeadDetail{
			ID:     "gd-prior-id",
			Fields: map[string]string{"s3_session_key": "gd-canonical-origin"},
		}
		if got := b.resolveWarmResumeS3Key(prior, "agent-x"); got != "gd-canonical-origin" {
			t.Errorf("expected canonical key gd-canonical-origin, got %q", got)
		}
	})
}
