package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// newPingCmd builds a minimal cobra command carrying the flags runPing reads.
func newPingCmd() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Flags().DurationVar(&pingTimeout, "timeout", 10_000_000_000, "")
	cmd.SetContext(context.Background())
	return cmd
}

func TestRunPing_HealthyDaemon(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	cmd := newPingCmd()
	var out strings.Builder
	cmd.SetOut(&out)
	if err := runPing(cmd, nil); err != nil {
		t.Fatalf("expected nil err on healthy daemon, got %v", err)
	}
	if gotPath != "/v1/health" {
		t.Errorf("ping should probe /v1/health, got %q", gotPath)
	}
	if !strings.Contains(out.String(), "ok") {
		t.Errorf("expected 'ok' output, got %q", out.String())
	}
}

func TestRunPing_DownDaemonReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	cmd := newPingCmd()
	// A daemon that errors MUST yield a nonzero exit — runPing returns a
	// non-nil error, which main() maps to os.Exit(1). This is the whole
	// point of the probe: the empty-output false-positive `gb news` PASS
	// (session d31q) cannot happen here.
	if err := runPing(cmd, nil); err == nil {
		t.Error("expected error (nonzero exit) when daemon is down")
	}
}

func TestRunPing_JSONVerdictOnFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	oldJSON := jsonOutput
	jsonOutput = true
	t.Cleanup(func() { jsonOutput = oldJSON })

	cmd := newPingCmd()
	// --json still exits nonzero on failure while emitting the verdict.
	if err := runPing(cmd, nil); err == nil {
		t.Error("expected nonzero exit even with --json on a down daemon")
	}
}

func TestRunPing_NilDaemon(t *testing.T) {
	old := daemon
	daemon = nil
	t.Cleanup(func() { daemon = old })

	cmd := newPingCmd()
	if err := runPing(cmd, nil); err == nil {
		t.Error("expected error when daemon client is not initialized")
	}
}

// Through a land door gb ping asks /v1/me, the one door route that needs no
// bead to exist, since the door lists no credential-free route (gd-c6LzZkk0qy).
func TestRunPing_LandDoorAsksMe(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if r.URL.Path != "/land/demo/v1/me" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"operator_id":"oidc:guest@example.com","source":"auth"}`))
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL+"/land/demo")

	cmd := newPingCmd()
	var out strings.Builder
	cmd.SetOut(&out)
	if err := runPing(cmd, nil); err != nil {
		t.Fatalf("ping through the land door: %v (asked %q)", err, gotPath)
	}
	if gotPath != "/land/demo/v1/me" || !strings.Contains(out.String(), "ok") {
		t.Errorf("asked %q, printed %q; want /land/demo/v1/me and ok", gotPath, out.String())
	}
}

// A door that refuses the caller (not a guest of the land) fails the ping.
func TestRunPing_LandDoorRefusedFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL+"/land/demo")

	if err := runPing(newPingCmd(), nil); err == nil {
		t.Error("ping succeeded through a door that refused the caller")
	}
}
