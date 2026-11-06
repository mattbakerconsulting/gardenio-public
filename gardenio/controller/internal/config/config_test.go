package config

import (
	"os"
	"testing"
	"time"
)

// ── envOr ──────────────────────────────────────────────────────────────────

func TestEnvOr_Fallback(t *testing.T) {
	if got := envOr("CONFIG_TEST_UNSET_VAR_XYZ", "default"); got != "default" {
		t.Errorf("envOr = %q, want default", got)
	}
}

func TestEnvOr_Set(t *testing.T) {
	t.Setenv("CONFIG_TEST_SET_VAR", "custom")
	if got := envOr("CONFIG_TEST_SET_VAR", "default"); got != "custom" {
		t.Errorf("envOr = %q, want custom", got)
	}
}

// ── envLookupOr ──────────────────────────────────────────────────────────────

// An unset key returns the fallback — same as envOr.
func TestEnvLookupOr_Unset(t *testing.T) {
	if got := envLookupOr("CONFIG_TEST_LOOKUP_UNSET_XYZ", "default"); got != "default" {
		t.Errorf("envLookupOr = %q, want default", got)
	}
}

// A set non-empty key returns its value.
func TestEnvLookupOr_Set(t *testing.T) {
	t.Setenv("CONFIG_TEST_LOOKUP_SET", "custom")
	if got := envLookupOr("CONFIG_TEST_LOOKUP_SET", "default"); got != "custom" {
		t.Errorf("envLookupOr = %q, want custom", got)
	}
}

// The distinguishing case from envOr: an explicitly-SET but EMPTY key returns
// "", not the fallback. This is what lets AGENT_DEFAULT_FABLE_MODEL="" disable
// the fable-alias pin instead of silently re-inheriting the baked default
// (ka-0000003175).
func TestEnvLookupOr_SetEmpty(t *testing.T) {
	t.Setenv("CONFIG_TEST_LOOKUP_EMPTY", "")
	if got := envLookupOr("CONFIG_TEST_LOOKUP_EMPTY", "default"); got != "" {
		t.Errorf("envLookupOr = %q, want empty string (set-but-empty must not fall back)", got)
	}
}

// ── AgentDefaultFableModel (Parse) ────────────────────────────────────────────

// Parse bakes the ACL-allowed "-oauth" fable model group as the default when
// AGENT_DEFAULT_FABLE_MODEL is entirely unset (ka-0000003175). The baked value
// tracks the CURRENT fable tier — claude-fable-5-1-oauth since ka-0000002744.
func TestParse_AgentDefaultFableModel_Default(t *testing.T) {
	os.Unsetenv("AGENT_DEFAULT_FABLE_MODEL")
	cfg := Parse()
	if got := cfg.AgentDefaultFableModel; got != "claude-fable-5-1-oauth" {
		t.Errorf("AgentDefaultFableModel = %q, want claude-fable-5-1-oauth", got)
	}
}

// An explicit empty value disables the pin (Parse must preserve "", not fall
// back to the baked default) — the operator escape hatch.
func TestParse_AgentDefaultFableModel_EmptyDisables(t *testing.T) {
	t.Setenv("AGENT_DEFAULT_FABLE_MODEL", "")
	cfg := Parse()
	if got := cfg.AgentDefaultFableModel; got != "" {
		t.Errorf("AgentDefaultFableModel = %q, want empty (explicit empty disables the pin)", got)
	}
}

// A non-empty override is honored verbatim.
func TestParse_AgentDefaultFableModel_Override(t *testing.T) {
	t.Setenv("AGENT_DEFAULT_FABLE_MODEL", "claude-fable-5-custom")
	cfg := Parse()
	if got := cfg.AgentDefaultFableModel; got != "claude-fable-5-custom" {
		t.Errorf("AgentDefaultFableModel = %q, want claude-fable-5-custom", got)
	}
}

// ── AgentDefaultHaikuModel (Parse) ────────────────────────────────────────────

// Parse bakes the ACL-allowed rolling pass-through haiku group as the default
// when AGENT_DEFAULT_HAIKU_MODEL is entirely unset (ka-0000005599). The value is
// asserted verbatim — the whole point of the fix is that it must be the rolling
// "anthropic/claude-haiku-4-5" group the key ACL allows, NOT the dated
// "…-20251001" snapshot the harness resolves on its own.
func TestParse_AgentDefaultHaikuModel_Default(t *testing.T) {
	os.Unsetenv("AGENT_DEFAULT_HAIKU_MODEL")
	cfg := Parse()
	if got := cfg.AgentDefaultHaikuModel; got != "anthropic/claude-haiku-4-5" {
		t.Errorf("AgentDefaultHaikuModel = %q, want anthropic/claude-haiku-4-5", got)
	}
}

// An explicit empty value disables the pin (Parse must preserve "", not fall
// back to the baked default) — the operator escape hatch.
func TestParse_AgentDefaultHaikuModel_EmptyDisables(t *testing.T) {
	t.Setenv("AGENT_DEFAULT_HAIKU_MODEL", "")
	cfg := Parse()
	if got := cfg.AgentDefaultHaikuModel; got != "" {
		t.Errorf("AgentDefaultHaikuModel = %q, want empty (explicit empty disables the pin)", got)
	}
}

// A non-empty override is honored verbatim.
func TestParse_AgentDefaultHaikuModel_Override(t *testing.T) {
	t.Setenv("AGENT_DEFAULT_HAIKU_MODEL", "anthropic/claude-haiku-4-5-custom")
	cfg := Parse()
	if got := cfg.AgentDefaultHaikuModel; got != "anthropic/claude-haiku-4-5-custom" {
		t.Errorf("AgentDefaultHaikuModel = %q, want anthropic/claude-haiku-4-5-custom", got)
	}
}

// ── envIntOr ───────────────────────────────────────────────────────────────

func TestEnvIntOr_Fallback(t *testing.T) {
	if got := envIntOr("CONFIG_TEST_UNSET_INT", 42); got != 42 {
		t.Errorf("envIntOr = %d, want 42", got)
	}
}

func TestEnvIntOr_Valid(t *testing.T) {
	t.Setenv("CONFIG_TEST_INT", "10")
	if got := envIntOr("CONFIG_TEST_INT", 42); got != 10 {
		t.Errorf("envIntOr = %d, want 10", got)
	}
}

func TestEnvIntOr_Invalid(t *testing.T) {
	t.Setenv("CONFIG_TEST_INT_BAD", "not-a-number")
	if got := envIntOr("CONFIG_TEST_INT_BAD", 42); got != 42 {
		t.Errorf("envIntOr = %d, want 42 (fallback on invalid)", got)
	}
}

// ── envBoolOr ──────────────────────────────────────────────────────────────

func TestEnvBoolOr_Fallback(t *testing.T) {
	if got := envBoolOr("CONFIG_TEST_UNSET_BOOL", true); !got {
		t.Error("envBoolOr = false, want true (fallback)")
	}
}

func TestEnvBoolOr_True(t *testing.T) {
	t.Setenv("CONFIG_TEST_BOOL", "true")
	if got := envBoolOr("CONFIG_TEST_BOOL", false); !got {
		t.Error("envBoolOr = false, want true")
	}
}

func TestEnvBoolOr_False(t *testing.T) {
	t.Setenv("CONFIG_TEST_BOOL_F", "false")
	if got := envBoolOr("CONFIG_TEST_BOOL_F", true); got {
		t.Error("envBoolOr = true, want false")
	}
}

func TestEnvBoolOr_Invalid(t *testing.T) {
	t.Setenv("CONFIG_TEST_BOOL_BAD", "not-bool")
	if got := envBoolOr("CONFIG_TEST_BOOL_BAD", true); !got {
		t.Error("envBoolOr = false, want true (fallback on invalid)")
	}
}

// ── envDurationOr ──────────────────────────────────────────────────────────

func TestEnvDurationOr_Fallback(t *testing.T) {
	if got := envDurationOr("CONFIG_TEST_UNSET_DUR", 30*time.Second); got != 30*time.Second {
		t.Errorf("envDurationOr = %v, want 30s", got)
	}
}

func TestEnvDurationOr_Valid(t *testing.T) {
	t.Setenv("CONFIG_TEST_DUR", "5m")
	if got := envDurationOr("CONFIG_TEST_DUR", time.Minute); got != 5*time.Minute {
		t.Errorf("envDurationOr = %v, want 5m", got)
	}
}

func TestEnvDurationOr_Invalid(t *testing.T) {
	t.Setenv("CONFIG_TEST_DUR_BAD", "not-a-duration")
	if got := envDurationOr("CONFIG_TEST_DUR_BAD", time.Minute); got != time.Minute {
		t.Errorf("envDurationOr = %v, want 1m (fallback on invalid)", got)
	}
}

// TestEnvDurationSetButIneffective covers the FREEZE_HARD_DEADLINE silent-drop
// detector (task ka-0000006783): unset/empty is the documented default (not
// "ineffective"); a valid positive value takes effect (not "ineffective"); an
// unparseable value or a non-positive one is ineffective (envDurationOr drops it
// to the 0 default and the manager's `> 0` guard treats it as unset).
func TestEnvDurationSetButIneffective(t *testing.T) {
	cases := []struct {
		name string
		set  bool
		val  string
		want bool
	}{
		{name: "unset", set: false, want: false},
		{name: "empty", set: true, val: "", want: false},
		{name: "valid positive", set: true, val: "5m", want: false},
		{name: "unparseable missing unit", set: true, val: "5", want: true},
		{name: "unparseable garbage", set: true, val: "not-a-duration", want: true},
		{name: "negative", set: true, val: "-4m", want: true},
		{name: "zero", set: true, val: "0s", want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key := "CONFIG_TEST_INEFFECTIVE_DUR"
			if tc.set {
				t.Setenv(key, tc.val)
			} else {
				// Ensure a stray value from the surrounding env cannot leak in.
				t.Setenv(key, "")
				os.Unsetenv(key)
			}
			if got := envDurationSetButIneffective(key); got != tc.want {
				t.Errorf("envDurationSetButIneffective(%q=%q) = %v, want %v", key, tc.val, got, tc.want)
			}
		})
	}
}

// TestParse_FreezeHardDeadlineInvalid asserts Parse surfaces a fat-fingered
// FREEZE_HARD_DEADLINE via the FreezeHardDeadlineInvalid flag while the effective
// FreezeHardDeadline still carries whatever the value parses to (0 on a drop),
// so the flag is purely diagnostic (task ka-0000006783).
func TestParse_FreezeHardDeadlineInvalid(t *testing.T) {
	t.Run("valid override not flagged", func(t *testing.T) {
		t.Setenv("FREEZE_HARD_DEADLINE", "6m")
		cfg := Parse()
		if cfg.FreezeHardDeadlineInvalid {
			t.Error("valid FREEZE_HARD_DEADLINE must not set FreezeHardDeadlineInvalid")
		}
		if cfg.FreezeHardDeadline != 6*time.Minute {
			t.Errorf("FreezeHardDeadline = %v, want 6m", cfg.FreezeHardDeadline)
		}
	})
	t.Run("unparseable flagged and dropped to zero", func(t *testing.T) {
		t.Setenv("FREEZE_HARD_DEADLINE", "5") // missing unit
		cfg := Parse()
		if !cfg.FreezeHardDeadlineInvalid {
			t.Error("unparseable FREEZE_HARD_DEADLINE must set FreezeHardDeadlineInvalid")
		}
		if cfg.FreezeHardDeadline != 0 {
			t.Errorf("FreezeHardDeadline = %v, want 0 (dropped to compute-default sentinel)", cfg.FreezeHardDeadline)
		}
	})
	t.Run("unset not flagged", func(t *testing.T) {
		t.Setenv("FREEZE_HARD_DEADLINE", "")
		os.Unsetenv("FREEZE_HARD_DEADLINE")
		cfg := Parse()
		if cfg.FreezeHardDeadlineInvalid {
			t.Error("unset FREEZE_HARD_DEADLINE must not set FreezeHardDeadlineInvalid")
		}
	})
}

func TestParse_SnapshotS3Transport(t *testing.T) {
	t.Run("unset leaves node-local (empty bucket)", func(t *testing.T) {
		os.Unsetenv("FREEZE_SNAPSHOT_S3_BUCKET")
		os.Unsetenv("FREEZE_SNAPSHOT_S3_PREFIX")
		cfg := Parse()
		if cfg.SnapshotS3Bucket != "" {
			t.Errorf("SnapshotS3Bucket should default empty, got %q", cfg.SnapshotS3Bucket)
		}
		if cfg.SnapshotS3Prefix != "" {
			t.Errorf("SnapshotS3Prefix should default empty (transport applies ftsnap), got %q", cfg.SnapshotS3Prefix)
		}
	})
	t.Run("set is trimmed and read", func(t *testing.T) {
		t.Setenv("FREEZE_SNAPSHOT_S3_BUCKET", "  gardenio-ftsnap  ")
		t.Setenv("FREEZE_SNAPSHOT_S3_PREFIX", " ftsnap ")
		cfg := Parse()
		if cfg.SnapshotS3Bucket != "gardenio-ftsnap" {
			t.Errorf("SnapshotS3Bucket = %q, want gardenio-ftsnap", cfg.SnapshotS3Bucket)
		}
		if cfg.SnapshotS3Prefix != "ftsnap" {
			t.Errorf("SnapshotS3Prefix = %q, want ftsnap", cfg.SnapshotS3Prefix)
		}
	})
}

// ── hostname ───────────────────────────────────────────────────────────────

func TestHostname(t *testing.T) {
	h := hostname()
	if h == "" {
		t.Error("hostname() returned empty string")
	}
}

// ── Parse ──────────────────────────────────────────────────────────────────

func TestParse_Defaults(t *testing.T) {
	// Clear env vars that might be set in the pod environment
	// so we test actual defaults.
	t.Setenv("NAMESPACE", "")
	t.Setenv("BEADS_GRPC_ADDR", "")
	t.Setenv("BEADS_HTTP_ADDR", "")
	t.Setenv("COOP_BURST_LIMIT", "")
	t.Setenv("COOP_SYNC_INTERVAL", "")
	t.Setenv("ENABLE_LEADER_ELECTION", "")
	t.Setenv("LEADER_ELECTION_ID", "")
	t.Setenv("EXTERNAL_SECRET_STORE_NAME", "")
	t.Setenv("AUTH_EXHAUSTED_SWEEP_INTERVAL", "")
	t.Setenv("AUTH_FAILOVER_COOLDOWN", "")
	t.Setenv("AUTH_MAX_FAILOVER_DURATION", "")
	t.Setenv("LOG_LEVEL", "")
	t.Setenv("FREEZE_THAW_ENABLED", "")
	t.Setenv("WARM_PLACEHOLDER_POOL_ENABLED", "")
	t.Setenv("WARM_PLACEHOLDER_POOL_MIN_SIZE", "")
	t.Setenv("THAW_HARD_DEADLINE", "")
	t.Setenv("THAW_DRAIN_TIMEOUT", "")

	cfg := Parse()

	if cfg.Namespace != "gardenio" {
		t.Errorf("Namespace = %q, want gardenio", cfg.Namespace)
	}
	if cfg.FreezeThawEnabled {
		t.Error("FreezeThawEnabled should default to false")
	}
	if cfg.WarmPlaceholderPoolEnabled {
		t.Error("WarmPlaceholderPoolEnabled should default to false")
	}
	if cfg.WarmPlaceholderPoolMinSize != 0 {
		t.Errorf("WarmPlaceholderPoolMinSize = %d, want 0", cfg.WarmPlaceholderPoolMinSize)
	}
	if cfg.WarmPlaceholderStaleDrainRate != 0 {
		t.Errorf("WarmPlaceholderStaleDrainRate = %d, want 0 (poolmanager applies its default)", cfg.WarmPlaceholderStaleDrainRate)
	}
	// harden-thaw (ka-0000001355): hard deadline defers to the manager's computed
	// cap (0); drain timeout defaults to 90s so shutdown waits for in-flight thaws.
	if cfg.ThawHardDeadline != 0 {
		t.Errorf("ThawHardDeadline = %v, want 0 (manager computes the cap)", cfg.ThawHardDeadline)
	}
	if cfg.ThawDrainTimeout != 90*time.Second {
		t.Errorf("ThawDrainTimeout = %v, want 90s", cfg.ThawDrainTimeout)
	}
	if cfg.BeadsGRPCAddr != "localhost:9090" {
		t.Errorf("BeadsGRPCAddr = %q, want localhost:9090", cfg.BeadsGRPCAddr)
	}
	if cfg.BeadsHTTPAddr != "localhost:8080" {
		t.Errorf("BeadsHTTPAddr = %q, want localhost:8080", cfg.BeadsHTTPAddr)
	}
	if cfg.CoopMaxPods != 10 {
		t.Errorf("CoopMaxPods = %d, want 10", cfg.CoopMaxPods)
	}
	if cfg.CoopMaxPodsPerProject != 6 {
		t.Errorf("CoopMaxPodsPerProject = %d, want 6", cfg.CoopMaxPodsPerProject)
	}
	if cfg.CoopMaxPodsGVisor != 0 {
		t.Errorf("CoopMaxPodsGVisor = %d, want 0 (default: no separate gVisor ceiling)", cfg.CoopMaxPodsGVisor)
	}
	if cfg.CoopBurstLimit != 3 {
		t.Errorf("CoopBurstLimit = %d, want 3", cfg.CoopBurstLimit)
	}
	if cfg.CoopSyncInterval != 60*time.Second {
		t.Errorf("CoopSyncInterval = %v, want 60s", cfg.CoopSyncInterval)
	}
	if cfg.LeaderElection {
		t.Error("LeaderElection should default to false")
	}
	if cfg.LeaderElectionID != "agents-leader" {
		t.Errorf("LeaderElectionID = %q, want agents-leader", cfg.LeaderElectionID)
	}
	if cfg.ExternalSecretStoreName != "secretstore" {
		t.Errorf("ExternalSecretStoreName = %q, want secretstore", cfg.ExternalSecretStoreName)
	}
	if cfg.AuthExhaustedSweepInterval != 5*time.Minute {
		t.Errorf("AuthExhaustedSweepInterval = %v, want 5m", cfg.AuthExhaustedSweepInterval)
	}
	if cfg.AuthFailoverCooldown != 5*time.Minute {
		t.Errorf("AuthFailoverCooldown = %v, want 5m", cfg.AuthFailoverCooldown)
	}
	if cfg.AuthMaxFailoverDuration != 24*time.Hour {
		t.Errorf("AuthMaxFailoverDuration = %v, want 24h", cfg.AuthMaxFailoverDuration)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want info", cfg.LogLevel)
	}
}

func TestParse_EnvOverrides(t *testing.T) {
	t.Setenv("NAMESPACE", "custom-ns")
	t.Setenv("COOP_IMAGE", "ghcr.io/agent:v2")
	t.Setenv("COOP_MAX_PODS", "5")
	t.Setenv("COOP_MAX_PODS_PER_PROJECT", "3")
	t.Setenv("COOP_MAX_PODS_GVISOR", "4")
	t.Setenv("COOP_BURST_LIMIT", "10")
	t.Setenv("COOP_SYNC_INTERVAL", "30s")
	t.Setenv("ENABLE_LEADER_ELECTION", "true")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("FREEZE_THAW_ENABLED", "true")
	t.Setenv("WARM_PLACEHOLDER_POOL_ENABLED", "true")
	t.Setenv("WARM_PLACEHOLDER_POOL_MIN_SIZE", "2")
	t.Setenv("WARM_PLACEHOLDER_STALE_DRAIN_RATE", "4")

	cfg := Parse()

	if cfg.Namespace != "custom-ns" {
		t.Errorf("Namespace = %q, want custom-ns", cfg.Namespace)
	}
	if !cfg.FreezeThawEnabled {
		t.Error("FreezeThawEnabled should be true when FREEZE_THAW_ENABLED=true")
	}
	if !cfg.WarmPlaceholderPoolEnabled {
		t.Error("WarmPlaceholderPoolEnabled should be true when WARM_PLACEHOLDER_POOL_ENABLED=true")
	}
	if cfg.WarmPlaceholderPoolMinSize != 2 {
		t.Errorf("WarmPlaceholderPoolMinSize = %d, want 2", cfg.WarmPlaceholderPoolMinSize)
	}
	if cfg.WarmPlaceholderStaleDrainRate != 4 {
		t.Errorf("WarmPlaceholderStaleDrainRate = %d, want 4", cfg.WarmPlaceholderStaleDrainRate)
	}
	if cfg.CoopImage != "ghcr.io/agent:v2" {
		t.Errorf("CoopImage = %q, want ghcr.io/agent:v2", cfg.CoopImage)
	}
	if cfg.CoopMaxPods != 5 {
		t.Errorf("CoopMaxPods = %d, want 5", cfg.CoopMaxPods)
	}
	if cfg.CoopMaxPodsPerProject != 3 {
		t.Errorf("CoopMaxPodsPerProject = %d, want 3", cfg.CoopMaxPodsPerProject)
	}
	if cfg.CoopMaxPodsGVisor != 4 {
		t.Errorf("CoopMaxPodsGVisor = %d, want 4", cfg.CoopMaxPodsGVisor)
	}
	if cfg.CoopBurstLimit != 10 {
		t.Errorf("CoopBurstLimit = %d, want 10", cfg.CoopBurstLimit)
	}
	if cfg.CoopSyncInterval != 30*time.Second {
		t.Errorf("CoopSyncInterval = %v, want 30s", cfg.CoopSyncInterval)
	}
	if !cfg.LeaderElection {
		t.Error("LeaderElection should be true")
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want debug", cfg.LogLevel)
	}
}

func TestParse_S3Config(t *testing.T) {
	t.Setenv("COOP_S3_BUCKET", "gardenio-sessions")
	t.Setenv("COOP_S3_PREFIX", "prod/sessions")
	t.Setenv("COOP_S3_REGION", "us-west-2")

	cfg := Parse()

	if cfg.CoopS3Bucket != "gardenio-sessions" {
		t.Errorf("CoopS3Bucket = %q, want gardenio-sessions", cfg.CoopS3Bucket)
	}
	if cfg.CoopS3Prefix != "prod/sessions" {
		t.Errorf("CoopS3Prefix = %q, want prod/sessions", cfg.CoopS3Prefix)
	}
	if cfg.CoopS3Region != "us-west-2" {
		t.Errorf("CoopS3Region = %q, want us-west-2", cfg.CoopS3Region)
	}
}

func TestParse_S3Config_Empty(t *testing.T) {
	t.Setenv("COOP_S3_BUCKET", "")
	t.Setenv("COOP_S3_PREFIX", "")
	t.Setenv("COOP_S3_REGION", "")

	cfg := Parse()

	if cfg.CoopS3Bucket != "" {
		t.Errorf("CoopS3Bucket = %q, want empty", cfg.CoopS3Bucket)
	}
	if cfg.CoopS3Prefix != "" {
		t.Errorf("CoopS3Prefix = %q, want empty", cfg.CoopS3Prefix)
	}
	if cfg.CoopS3Region != "" {
		t.Errorf("CoopS3Region = %q, want empty", cfg.CoopS3Region)
	}
}

func TestParse_AuthExhaustedSweepInterval_Default(t *testing.T) {
	t.Setenv("AUTH_EXHAUSTED_SWEEP_INTERVAL", "")

	cfg := Parse()

	if cfg.AuthExhaustedSweepInterval != 5*time.Minute {
		t.Errorf("AuthExhaustedSweepInterval = %v, want 5m", cfg.AuthExhaustedSweepInterval)
	}
}

func TestParse_AuthExhaustedSweepInterval_Custom(t *testing.T) {
	t.Setenv("AUTH_EXHAUSTED_SWEEP_INTERVAL", "10m")

	cfg := Parse()

	if cfg.AuthExhaustedSweepInterval != 10*time.Minute {
		t.Errorf("AuthExhaustedSweepInterval = %v, want 10m", cfg.AuthExhaustedSweepInterval)
	}
}

func TestParse_AuthExhaustedSweepInterval_Zero(t *testing.T) {
	t.Setenv("AUTH_EXHAUSTED_SWEEP_INTERVAL", "0s")

	cfg := Parse()

	if cfg.AuthExhaustedSweepInterval != 0 {
		t.Errorf("AuthExhaustedSweepInterval = %v, want 0 (disabled)", cfg.AuthExhaustedSweepInterval)
	}
}

func TestParse_AuthFailoverCooldown_Custom(t *testing.T) {
	t.Setenv("AUTH_FAILOVER_COOLDOWN", "10m")

	cfg := Parse()

	if cfg.AuthFailoverCooldown != 10*time.Minute {
		t.Errorf("AuthFailoverCooldown = %v, want 10m", cfg.AuthFailoverCooldown)
	}
}

func TestParse_AuthFailoverCooldown_Zero(t *testing.T) {
	t.Setenv("AUTH_FAILOVER_COOLDOWN", "0s")

	cfg := Parse()

	if cfg.AuthFailoverCooldown != 0 {
		t.Errorf("AuthFailoverCooldown = %v, want 0 (disabled)", cfg.AuthFailoverCooldown)
	}
}

func TestParse_AuthMaxFailoverDuration_Custom(t *testing.T) {
	t.Setenv("AUTH_MAX_FAILOVER_DURATION", "12h")

	cfg := Parse()

	if cfg.AuthMaxFailoverDuration != 12*time.Hour {
		t.Errorf("AuthMaxFailoverDuration = %v, want 12h", cfg.AuthMaxFailoverDuration)
	}
}

func TestParse_AuthMaxFailoverDuration_Zero(t *testing.T) {
	t.Setenv("AUTH_MAX_FAILOVER_DURATION", "0s")

	cfg := Parse()

	if cfg.AuthMaxFailoverDuration != 0 {
		t.Errorf("AuthMaxFailoverDuration = %v, want 0 (disabled)", cfg.AuthMaxFailoverDuration)
	}
}

func TestParse_SecretEnvVars(t *testing.T) {
	// Controller-level secrets (not per-project — those come from config beads).
	t.Setenv("BEADS_TOKEN_SECRET", "beads-token")
	t.Setenv("CLAUDE_OAUTH_SECRET", "claude-oauth")
	t.Setenv("NATS_TOKEN_SECRET", "nats-token")
	t.Setenv("COOPMUX_TOKEN_SECRET", "coopmux-token")

	cfg := Parse()

	checks := map[string]string{
		"BeadsTokenSecret":   cfg.BeadsTokenSecret,
		"ClaudeOAuthSecret":  cfg.ClaudeOAuthSecret,
		"NatsTokenSecret":    cfg.NatsTokenSecret,
		"CoopmuxTokenSecret": cfg.CoopmuxTokenSecret,
	}
	expected := map[string]string{
		"BeadsTokenSecret":   "beads-token",
		"ClaudeOAuthSecret":  "claude-oauth",
		"NatsTokenSecret":    "nats-token",
		"CoopmuxTokenSecret": "coopmux-token",
	}
	for name, got := range checks {
		if got != expected[name] {
			t.Errorf("%s = %q, want %q", name, got, expected[name])
		}
	}
}

// Env-side Grafana Editor token bundle (task ka-0000002337): Parse must read the
// secret name, env list, and per-env base URLs from env vars.
func TestParse_GrafanaEnvBundle(t *testing.T) {
	t.Setenv("GRAFANA_ENV_API_KEY_SECRET", "grafana-api-key")
	t.Setenv("GRAFANA_ENV_LIST", "devel, perf ,prod")
	t.Setenv("GRAFANA_BASE_URL_DEVEL", "https://lens.devel.acme.net/grafana")
	t.Setenv("GRAFANA_BASE_URL_PROD", "https://lens.acme.net/grafana")

	cfg := Parse()

	if cfg.GrafanaEnvAPIKeySecret != "grafana-api-key" {
		t.Errorf("GrafanaEnvAPIKeySecret = %q", cfg.GrafanaEnvAPIKeySecret)
	}
	wantEnvs := []string{"devel", "perf", "prod"}
	if len(cfg.GrafanaEnvList) != len(wantEnvs) {
		t.Fatalf("GrafanaEnvList = %v, want %v", cfg.GrafanaEnvList, wantEnvs)
	}
	for i, e := range wantEnvs {
		if cfg.GrafanaEnvList[i] != e {
			t.Errorf("GrafanaEnvList[%d] = %q, want %q", i, cfg.GrafanaEnvList[i], e)
		}
	}
	if cfg.GrafanaEnvBaseURLs["devel"] != "https://lens.devel.acme.net/grafana" {
		t.Errorf("base URL devel = %q", cfg.GrafanaEnvBaseURLs["devel"])
	}
	if cfg.GrafanaEnvBaseURLs["prod"] != "https://lens.acme.net/grafana" {
		t.Errorf("base URL prod = %q", cfg.GrafanaEnvBaseURLs["prod"])
	}
	if _, ok := cfg.GrafanaEnvBaseURLs["perf"]; ok {
		t.Error("perf base URL should be absent when unset")
	}
}

func TestParse_GrafanaEnvBundle_DefaultEmpty(t *testing.T) {
	t.Setenv("GRAFANA_ENV_API_KEY_SECRET", "")
	t.Setenv("GRAFANA_ENV_LIST", "")

	cfg := Parse()

	if cfg.GrafanaEnvAPIKeySecret != "" {
		t.Errorf("GrafanaEnvAPIKeySecret = %q, want empty", cfg.GrafanaEnvAPIKeySecret)
	}
	if len(cfg.GrafanaEnvList) != 0 {
		t.Errorf("GrafanaEnvList = %v, want empty", cfg.GrafanaEnvList)
	}
	if len(cfg.GrafanaEnvBaseURLs) != 0 {
		t.Errorf("GrafanaEnvBaseURLs = %v, want empty", cfg.GrafanaEnvBaseURLs)
	}
}

// OpenZiti access broker (epic ka-0000000990): Parse reads broker URL,
// audience, tunneler image, default TTL, and revoke settings from env.
func TestParse_ZitiAccessBroker(t *testing.T) {
	t.Setenv("ZITI_ACCESS_BROKER_URL", "http://ziti-access-broker.ziti-access-broker.svc.cluster.local")
	t.Setenv("ZITI_ACCESS_TOKEN_AUDIENCE", "gardenio-ziti-broker")
	t.Setenv("ZITI_TUNNELER_IMAGE", "openziti/ziti-edge-tunnel:1.0.0")
	t.Setenv("ZITI_ACCESS_DEFAULT_TTL", "15m")
	t.Setenv("ZITI_ACCESS_REVOKE_ON_EXIT", "true")
	t.Setenv("ZITI_ACCESS_CONTROLLER_TOKEN_FILE", "/var/run/secrets/ziti-broker/token")

	cfg := Parse()

	if cfg.ZitiAccessBrokerURL == "" {
		t.Error("ZitiAccessBrokerURL should be set")
	}
	if cfg.ZitiAccessTokenAudience != "gardenio-ziti-broker" {
		t.Errorf("ZitiAccessTokenAudience = %q", cfg.ZitiAccessTokenAudience)
	}
	if cfg.ZitiTunnelerImage != "openziti/ziti-edge-tunnel:1.0.0" {
		t.Errorf("ZitiTunnelerImage = %q", cfg.ZitiTunnelerImage)
	}
	if cfg.ZitiAccessDefaultTTL != "15m" {
		t.Errorf("ZitiAccessDefaultTTL = %q", cfg.ZitiAccessDefaultTTL)
	}
	if !cfg.ZitiAccessRevokeOnExit {
		t.Error("ZitiAccessRevokeOnExit should be true")
	}
	if cfg.ZitiAccessControllerTokenFile != "/var/run/secrets/ziti-broker/token" {
		t.Errorf("ZitiAccessControllerTokenFile = %q", cfg.ZitiAccessControllerTokenFile)
	}
}

// Default: no Ziti env => broker URL empty (feature off) but revoke-on-exit
// still defaults true (harmless when broker URL is empty).
func TestParse_ZitiAccessBroker_DefaultOff(t *testing.T) {
	t.Setenv("ZITI_ACCESS_BROKER_URL", "")
	t.Setenv("ZITI_ACCESS_REVOKE_ON_EXIT", "")

	cfg := Parse()

	if cfg.ZitiAccessBrokerURL != "" {
		t.Errorf("ZitiAccessBrokerURL = %q, want empty (feature off)", cfg.ZitiAccessBrokerURL)
	}
	if !cfg.ZitiAccessRevokeOnExit {
		t.Error("ZitiAccessRevokeOnExit should default true")
	}
}

// MicroVM CloudWatch runtime logging env (ka-0000002120): the log group is a
// pure opt-in (empty default → RunMicrovm Logging stays unset), while the
// stream prefix always carries the "agent/" default so a chart that sets only
// the group still gets per-bead streams.
func TestParse_MicrovmLogging(t *testing.T) {
	t.Setenv("MICROVM_LOG_GROUP", "")
	t.Setenv("MICROVM_LOG_STREAM_PREFIX", "")

	cfg := Parse()
	if cfg.MicrovmLogGroup != "" {
		t.Errorf("MicrovmLogGroup = %q, want empty (logging off by default)", cfg.MicrovmLogGroup)
	}
	if cfg.MicrovmLogStreamPrefix != "agent/" {
		t.Errorf("MicrovmLogStreamPrefix = %q, want agent/ default", cfg.MicrovmLogStreamPrefix)
	}

	t.Setenv("MICROVM_LOG_GROUP", "/aws/lambda/microvms/ziti-microvm-poc")
	t.Setenv("MICROVM_LOG_STREAM_PREFIX", "microvm-agent/")
	cfg = Parse()
	if cfg.MicrovmLogGroup != "/aws/lambda/microvms/ziti-microvm-poc" {
		t.Errorf("MicrovmLogGroup = %q", cfg.MicrovmLogGroup)
	}
	if cfg.MicrovmLogStreamPrefix != "microvm-agent/" {
		t.Errorf("MicrovmLogStreamPrefix = %q", cfg.MicrovmLogStreamPrefix)
	}
}

// TestParse_StuckAgentTimeoutDisabledByDefault locks in that the stuck-agent
// watchdog ships OFF (ka-0000000084).
//
// It previously defaulted to 15m and hard-killed a live monorepo-repro agent
// that was mid-Playwright-run: the pod was healthy (602Mi of an 8Gi request,
// no OOM, no eviction) and had simply not printed to its terminal for 16m. The
// kill destroyed an uncommitted e2e repro spec, because crew pods use
// emptyDir. Until the heuristic has a false-positive-free record it must be
// opt-in, so a default regression here is a real regression.
func TestParse_StuckAgentTimeoutDisabledByDefault(t *testing.T) {
	t.Setenv("STUCK_AGENT_TIMEOUT", "")

	cfg := Parse()

	if cfg.StuckAgentTimeout != 0 {
		t.Errorf("StuckAgentTimeout = %v, want 0 (watchdog disabled by default)", cfg.StuckAgentTimeout)
	}
}

// TestParse_StuckAgentTimeoutOptIn verifies the watchdog can still be turned
// back on deliberately once the liveness signals are trustworthy.
func TestParse_StuckAgentTimeoutOptIn(t *testing.T) {
	t.Setenv("STUCK_AGENT_TIMEOUT", "15m")

	cfg := Parse()

	if cfg.StuckAgentTimeout != 15*time.Minute {
		t.Errorf("StuckAgentTimeout = %v, want 15m", cfg.StuckAgentTimeout)
	}
}

// TestParse_StuckRestartInPlaceEnabledByDefault locks in that the stuck remedy
// is NON-DESTRUCTIVE by default (ka-0000001648): the controller restarts the
// wedged coop session in place (preserving the emptyDir workspace) before ever
// deleting+recreating the pod. A default regression here re-arms the data-loss
// path the watchdog was disabled over.
func TestParse_StuckRestartInPlaceEnabledByDefault(t *testing.T) {
	t.Setenv("STUCK_RESTART_IN_PLACE", "")

	cfg := Parse()

	if !cfg.StuckRestartInPlace {
		t.Error("StuckRestartInPlace = false, want true (non-destructive restart is the default)")
	}
}

// TestParse_StuckRestartInPlaceOptOut verifies an operator can force the legacy
// destructive delete+recreate remedy.
func TestParse_StuckRestartInPlaceOptOut(t *testing.T) {
	t.Setenv("STUCK_RESTART_IN_PLACE", "false")

	cfg := Parse()

	if cfg.StuckRestartInPlace {
		t.Error("StuckRestartInPlace = true, want false when STUCK_RESTART_IN_PLACE=false")
	}
}
