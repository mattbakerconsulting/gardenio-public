package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// captureLimitsOutput runs runAccountLimits against a stub daemon serving the
// given account beads, returning combined stdout+stderr. The table goes to
// stdout and the explanatory footer to stderr, so a caller asserting on the
// whole rendering needs both.
func captureLimitsOutput(t *testing.T, beads []map[string]any) string {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": beads, "total": len(beads)})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	origOut, origErr := os.Stdout, os.Stderr
	rOut, wOut, _ := os.Pipe()
	rErr, wErr, _ := os.Pipe()
	os.Stdout, os.Stderr = wOut, wErr
	t.Cleanup(func() { os.Stdout, os.Stderr = origOut, origErr })

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	runErr := runAccountLimits(cmd, nil)

	_ = wOut.Close()
	_ = wErr.Close()
	os.Stdout, os.Stderr = origOut, origErr

	var buf bytes.Buffer
	_, _ = io.Copy(&buf, rOut)
	_, _ = io.Copy(&buf, rErr)

	if runErr != nil {
		t.Fatalf("runAccountLimits: %v", runErr)
	}
	return buf.String()
}

// TestAccountLimits_AuthDeadAccountNotShownAsHealthy reproduces the live
// matthewbaker1110 state observed on 2026-08-02: the controller probe took a
// hard 403 and set probe_auth_status=rejected, but that path early-returns
// WITHOUT refreshing throttle_status / utilization / throttle_reset_at. Those
// fields stayed frozen at their last good values, so the operator table
// rendered the account as the fleet's HEALTHIEST reserve — "allowed", 73% of
// the 5h window free, reset "past" — at a moment when every other account was
// exhausted and the fleet was riding a single one. Acting on that row (a
// gb account reset, or just believing the headroom existed) would have pointed
// traffic at a credential that 403s on every request.
func TestAccountLimits_AuthDeadAccountNotShownAsHealthy(t *testing.T) {
	staleProbe := time.Now().Add(-7 * 24 * time.Hour).UTC().Format(time.RFC3339)
	pastReset := time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339)

	fields := fmt.Sprintf(`{
		"provider":"anthropic",
		"status":"exhausted",
		"utilization_5h":"0.27",
		"utilization_7d":"0.63",
		"throttle_status":"allowed",
		"probe_auth_status":"rejected",
		"overage_status":"rejected",
		"overage_disabled_reason":"out_of_credits",
		"throttle_reset_at":"%s",
		"probe_checked_at":"%s"
	}`, pastReset, staleProbe)

	out := captureLimitsOutput(t, []map[string]any{{
		"id": "ka-0000006759", "title": "matthewbaker1110", "type": "account",
		"status": "open", "labels": []string{"project:gardenio"},
		"fields": json.RawMessage(fields),
	}})

	// The throttle column must not advertise an auth-dead credential as
	// serviceable. Before the fix this rendered the raw frozen "allowed".
	if !strings.Contains(out, "auth-dead") {
		t.Errorf("expected THROTTLE column to read auth-dead, got:\n%s", out)
	}

	// The stale-probe marker must be present: the numbers are a week old and
	// the controller's own switchback path already refuses to trust them.
	if !strings.Contains(out, "!") {
		t.Errorf("expected stale-probe marker on PROBE AGE, got:\n%s", out)
	}

	// The footer previously explained only throttle_status=="rejected"
	// accounts, so this one — the row most needing explanation — got none.
	if !strings.Contains(out, "AUTH-DEAD") {
		t.Errorf("expected an AUTH-DEAD footer explanation, got:\n%s", out)
	}
	if !strings.Contains(out, "out_of_credits") {
		t.Errorf("expected last-known overage reason in footer, got:\n%s", out)
	}
}

// TestAccountLimits_HealthyAccountUnchanged guards the fix against
// over-reach: an account with a good probe must still render its real
// throttle status, with no auth-dead label and no staleness marker.
func TestAccountLimits_HealthyAccountUnchanged(t *testing.T) {
	freshProbe := time.Now().Add(-3 * time.Minute).UTC().Format(time.RFC3339)
	futureReset := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)

	fields := fmt.Sprintf(`{
		"provider":"anthropic",
		"status":"active",
		"utilization_5h":"0.34",
		"utilization_7d":"0.04",
		"throttle_status":"allowed",
		"probe_auth_status":"ok",
		"throttle_reset_at":"%s",
		"probe_checked_at":"%s"
	}`, futureReset, freshProbe)

	out := captureLimitsOutput(t, []map[string]any{{
		"id": "ka-0000006686", "title": "gardenio", "type": "account",
		"status": "open", "labels": []string{"project:gardenio"},
		"fields": json.RawMessage(fields),
	}})

	if strings.Contains(out, "auth-dead") || strings.Contains(out, "AUTH-DEAD") {
		t.Errorf("healthy account must not be flagged auth-dead, got:\n%s", out)
	}
	if !strings.Contains(out, "allowed") {
		t.Errorf("healthy account should render throttle allowed, got:\n%s", out)
	}
	// A 3-minute-old probe is well inside probeStaleThreshold.
	if strings.Contains(out, "!") {
		t.Errorf("fresh probe must not carry the stale marker, got:\n%s", out)
	}
}

// TestAccountLimits_StaleProbeWithoutAuthRejection covers the third state:
// the credential is fine but the probe has simply gone quiet (prober wedged,
// controller restarted). The numbers are still not current, so the row must
// carry the staleness marker — without being condemned as auth-dead.
func TestAccountLimits_StaleProbeWithoutAuthRejection(t *testing.T) {
	staleProbe := time.Now().Add(-90 * time.Minute).UTC().Format(time.RFC3339)

	fields := fmt.Sprintf(`{
		"provider":"anthropic",
		"status":"active",
		"utilization_5h":"0.10",
		"utilization_7d":"0.20",
		"throttle_status":"allowed",
		"probe_auth_status":"ok",
		"probe_checked_at":"%s"
	}`, staleProbe)

	out := captureLimitsOutput(t, []map[string]any{{
		"id": "gd-Stale1", "title": "quiet-acct", "type": "account",
		"status": "open", "labels": []string{"project:gardenio"},
		"fields": json.RawMessage(fields),
	}})

	if strings.Contains(out, "auth-dead") {
		t.Errorf("stale-but-authorized account must not read auth-dead, got:\n%s", out)
	}
	if !strings.Contains(out, "!") {
		t.Errorf("expected stale-probe marker for a 90m-old probe, got:\n%s", out)
	}
}
