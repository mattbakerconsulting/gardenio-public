package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// forceCleanTestReset clears the boot-time latch and every env override so each
// test starts from a known state. Restores nothing on cleanup beyond the latch —
// t.Setenv handles env restoration.
func forceCleanTestReset(t *testing.T) {
	t.Helper()
	old := forceCleanLatched.Load()
	forceCleanLatched.Store(nil)
	t.Cleanup(func() { forceCleanLatched.Store(old) })
	for _, k := range []string{
		forceCleanTurnThresholdEnv, forceCleanTokenThresholdEnv,
		forceCleanWakeThresholdEnv, forceCleanDisableEnv,
		"COOP_S3_BUCKET", "KD_AGENT_ID", "BOAT_AGENT_BEAD_ID",
	} {
		t.Setenv(k, "")
	}
}

// -- Pure policy core (decideForceClean) ------------------------------------

func TestDecideForceClean_Thresholds(t *testing.T) {
	def := forceCleanThresholds{
		turns:  defaultForceCleanTurnThreshold,
		tokens: defaultForceCleanTokenThreshold,
		wakes:  defaultForceCleanWakeThreshold,
	}
	cases := []struct {
		name       string
		sig        forceCleanSignals
		thr        forceCleanThresholds
		wantFire   bool
		wantReason string // substring the reason must contain when firing
	}{
		{
			name: "healthy short session does not fire",
			sig:  forceCleanSignals{turns: 40, tokens: 50_000, wakes: 1},
			thr:  def,
		},
		{
			name:       "turns over threshold fires",
			sig:        forceCleanSignals{turns: 900, tokens: 10_000, wakes: 1},
			thr:        def,
			wantFire:   true,
			wantReason: "turns=900",
		},
		{
			name:       "tokens over threshold fires (near context window)",
			sig:        forceCleanSignals{turns: 10, tokens: 400_000, wakes: 1},
			thr:        def,
			wantFire:   true,
			wantReason: "context_tokens=400000",
		},
		{
			name:       "wakes over threshold fires (re-primed too many times)",
			sig:        forceCleanSignals{turns: 10, tokens: 1_000, wakes: 12},
			thr:        def,
			wantFire:   true,
			wantReason: "wakes=12",
		},
		{
			name: "exactly at threshold minus one does not fire",
			sig:  forceCleanSignals{turns: 799, tokens: 349_999, wakes: 11},
			thr:  def,
		},
		{
			name: "disabled trigger (threshold 0) never fires on that signal",
			sig:  forceCleanSignals{turns: 100_000},
			thr:  forceCleanThresholds{turns: 0, tokens: 0, wakes: 0},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fire, reason := decideForceClean(tc.sig, tc.thr)
			if fire != tc.wantFire {
				t.Fatalf("decideForceClean fire=%v, want %v (reason=%q)", fire, tc.wantFire, reason)
			}
			if tc.wantFire && !strings.Contains(reason, tc.wantReason) {
				t.Fatalf("reason %q missing %q", reason, tc.wantReason)
			}
		})
	}
}

// -- Threshold env overrides -------------------------------------------------

func TestResolveForceCleanThresholds_EnvOverrides(t *testing.T) {
	forceCleanTestReset(t)
	t.Setenv(forceCleanTurnThresholdEnv, "50")
	t.Setenv(forceCleanTokenThresholdEnv, "0") // disable the token trigger
	// wake threshold left unset → default

	thr := resolveForceCleanThresholds()
	if thr.turns != 50 {
		t.Errorf("turns threshold = %d, want 50", thr.turns)
	}
	if thr.tokens != 0 {
		t.Errorf("tokens threshold = %d, want 0 (disabled)", thr.tokens)
	}
	if thr.wakes != defaultForceCleanWakeThreshold {
		t.Errorf("wakes threshold = %d, want default %d", thr.wakes, defaultForceCleanWakeThreshold)
	}
}

// -- End-to-end boot decision + coop env wiring ------------------------------

// serveAgentBead returns an httptest server that answers GET /v1/beads/<id>
// with the given fields (numeric fields serialize as JSON numbers, exactly as
// the daemon emits them).
func serveAgentBead(t *testing.T, beadID string, fields map[string]any) *beadsapi.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/beads/") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":     beadID,
				"title":  "agent",
				"type":   "agent",
				"kind":   "config",
				"status": "open",
				"fields": fields,
			})
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{})
	}))
	t.Cleanup(srv.Close)
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	return c
}

// TestForceCleanPolicy_BlowupAgentForcesCleanAndWiresEnv is the golden path: an
// agent bead whose prior life ran far past the turn threshold latches force-clean
// AND buildCoopEnv wires COOP_S3_NO_RESTORE=true while STILL setting the stable
// COOP_S3_SESSION_KEY (so this fresh life keeps uploading under the same prefix).
func TestForceCleanPolicy_BlowupAgentForcesCleanAndWiresEnv(t *testing.T) {
	forceCleanTestReset(t)
	t.Setenv("COOP_S3_BUCKET", "gardenio-sessions")
	t.Setenv("KD_AGENT_ID", "gd-blowup1")

	oldDaemon := daemon
	daemon = serveAgentBead(t, "gd-blowup1", map[string]any{
		"live_turns":     900, // > defaultForceCleanTurnThreshold
		"context_tokens": 20_000,
		"session_count":  2,
	})
	t.Cleanup(func() { daemon = oldDaemon })

	resolveForceCleanPolicy(context.Background())
	if !forceCleanActive() {
		t.Fatal("expected force-clean to latch for a 900-turn agent")
	}

	env := buildCoopEnv(initConfig{role: "crew"})
	if !envContains(env, forceCleanEnvVar+"="+forceCleanEnvValue) {
		t.Errorf("expected %s=%s in coop env; got %v", forceCleanEnvVar, forceCleanEnvValue, redactEnv(env))
	}
	// The session key must STILL be set: force-clean suppresses the RESTORE, not
	// the upload — this fresh life's transcript belongs under the same prefix.
	if !envHasPrefix(env, "COOP_S3_SESSION_KEY=gd-blowup1") {
		t.Errorf("expected COOP_S3_SESSION_KEY=gd-blowup1 to remain set under force-clean; got %v", redactEnv(env))
	}
}

// TestForceCleanPolicy_HealthyAgentResumesNormally is the negative control: a
// short prior life does NOT force-clean, and COOP_S3_NO_RESTORE is absent — the
// pod resumes exactly as it does today.
func TestForceCleanPolicy_HealthyAgentResumesNormally(t *testing.T) {
	forceCleanTestReset(t)
	t.Setenv("COOP_S3_BUCKET", "gardenio-sessions")
	t.Setenv("KD_AGENT_ID", "gd-healthy1")

	oldDaemon := daemon
	daemon = serveAgentBead(t, "gd-healthy1", map[string]any{
		"live_turns":     30,
		"context_tokens": 40_000,
		"session_count":  1,
	})
	t.Cleanup(func() { daemon = oldDaemon })

	resolveForceCleanPolicy(context.Background())
	if forceCleanActive() {
		t.Fatal("did not expect force-clean for a healthy 30-turn agent")
	}
	env := buildCoopEnv(initConfig{role: "crew"})
	if envHasKey(env, forceCleanEnvVar) {
		t.Errorf("did not expect %s in coop env for a healthy agent; got %v", forceCleanEnvVar, redactEnv(env))
	}
}

// TestForceCleanPolicy_DisableKillSwitch verifies the hard kill-switch: even a
// wildly-over-threshold agent never force-cleans when GARDENIO_FORCE_CLEAN_DISABLE
// is set.
func TestForceCleanPolicy_DisableKillSwitch(t *testing.T) {
	forceCleanTestReset(t)
	t.Setenv("COOP_S3_BUCKET", "gardenio-sessions")
	t.Setenv("KD_AGENT_ID", "gd-blowup2")
	t.Setenv(forceCleanDisableEnv, "1")

	oldDaemon := daemon
	daemon = serveAgentBead(t, "gd-blowup2", map[string]any{
		"live_turns":     5_000,
		"context_tokens": 900_000,
		"session_count":  50,
	})
	t.Cleanup(func() { daemon = oldDaemon })

	resolveForceCleanPolicy(context.Background())
	if forceCleanActive() {
		t.Fatal("kill-switch must prevent force-clean regardless of thresholds")
	}
}

// TestForceCleanPolicy_NoS3IsNoop verifies that without an S3 bucket (a --local
// dev run / mock mode) the policy is a quiet no-op — there is no transcript to
// force-clean away, and the read never happens.
func TestForceCleanPolicy_NoS3IsNoop(t *testing.T) {
	forceCleanTestReset(t)
	t.Setenv("KD_AGENT_ID", "gd-blowup3")
	// COOP_S3_BUCKET intentionally unset.

	oldDaemon := daemon
	daemon = serveAgentBead(t, "gd-blowup3", map[string]any{"live_turns": 9_000})
	t.Cleanup(func() { daemon = oldDaemon })

	resolveForceCleanPolicy(context.Background())
	if forceCleanActive() {
		t.Fatal("no S3 configured — force-clean must be a no-op")
	}
}

// TestForceCleanPolicy_TurnsFallBackToTotalRequestCount verifies the turn signal
// falls back to the authoritative total_request_count when the live_turns field
// was never written (a prior life that closed before its first live flush).
func TestForceCleanPolicy_TurnsFallBackToTotalRequestCount(t *testing.T) {
	forceCleanTestReset(t)
	t.Setenv("COOP_S3_BUCKET", "gardenio-sessions")
	t.Setenv("KD_AGENT_ID", "gd-blowup4")

	oldDaemon := daemon
	daemon = serveAgentBead(t, "gd-blowup4", map[string]any{
		// live_turns absent → fall back to total_request_count.
		"total_request_count": 1_200,
		"context_tokens":      10_000,
		"session_count":       1,
	})
	t.Cleanup(func() { daemon = oldDaemon })

	resolveForceCleanPolicy(context.Background())
	if !forceCleanActive() {
		t.Fatal("expected force-clean via total_request_count fallback")
	}
}

// -- small env assertion helpers --------------------------------------------
// (envContains / envHasKey live in init_agent_loop_test.go)

func envHasPrefix(env []string, prefix string) bool {
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}

// redactEnv returns only the COOP_S3_* / force-clean keys from env for readable
// failure messages (the full pod env is large and noisy).
func redactEnv(env []string) []string {
	var out []string
	for _, e := range env {
		if strings.HasPrefix(e, "COOP_S3_") || strings.HasPrefix(e, forceCleanEnvVar) {
			out = append(out, e)
		}
	}
	return out
}

// TestForceCleanEnvValueIsClapBoolLiteral pins the WIRE FORMAT of the value
// buildCoopEnv writes for COOP_S3_NO_RESTORE (ka-0000001975).
//
// coop reads that env var into a clap-derive `bool`. A derive `bool` is an
// ArgAction::SetTrue flag, and clap parses an env-supplied value for it with the
// STRICT bool value parser — only the literal "true" / "false" are accepted.
// "1" is a usage error, and a usage error is FATAL: coop exits 2 during arg
// parsing, before it starts anything at all. Because buildCoopEnv re-wires the
// same value on every relaunch, a wrong spelling is a deterministic crash loop
// that burns every restart in ~70s and closes the agent bead
// max_restarts_reached — turning the force-clean RECOVERY path into the thing
// that kills the agent (observed on ka-0000003182, 12 wakes).
//
// coop now also accepts boolish spellings, so a skewed pair works either way.
// This test guards the half that stays correct against an OLDER coop, and it is
// the tripwire if anyone "simplifies" the value back to "1".
func TestForceCleanEnvValueIsClapBoolLiteral(t *testing.T) {
	if forceCleanEnvValue != "true" && forceCleanEnvValue != "false" {
		t.Fatalf("forceCleanEnvValue = %q; coop's clap bool parser accepts only "+
			"the literals \"true\"/\"false\" and exits 2 on anything else", forceCleanEnvValue)
	}
}
