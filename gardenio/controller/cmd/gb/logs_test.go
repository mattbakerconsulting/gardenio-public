package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"gardenio/controller/internal/vlogs"
)

func TestResolveLogsEndpoint(t *testing.T) {
	// Flag wins over everything.
	t.Setenv("GARDENIO_LOGS_URL", "http://from-env:9428")
	if got := resolveLogsEndpoint("http://from-flag:9428"); got != "http://from-flag:9428" {
		t.Errorf("flag should win, got %q", got)
	}
	// Env next.
	if got := resolveLogsEndpoint(""); got != "http://from-env:9428" {
		t.Errorf("env should win over default, got %q", got)
	}
	// Fallback to the secondary env var.
	os.Unsetenv("GARDENIO_LOGS_URL")
	t.Setenv("GARDENIO_VICTORIALOGS_URL", "http://secondary:9428")
	if got := resolveLogsEndpoint(""); got != "http://secondary:9428" {
		t.Errorf("secondary env should be used, got %q", got)
	}
	// Default when nothing is set.
	os.Unsetenv("GARDENIO_VICTORIALOGS_URL")
	if got := resolveLogsEndpoint(""); got != defaultLogsEndpoint {
		t.Errorf("default should apply, got %q", got)
	}
}

func TestRunLogsDisallowedComponent(t *testing.T) {
	logsCmd.SetArgs([]string{"etcd"})
	err := runLogs(logsCmd, []string{"etcd"})
	if err == nil || !strings.Contains(err.Error(), "not queryable") {
		t.Fatalf("expected not-queryable error, got %v", err)
	}
}

func TestRunLogsHumanOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"_time":"2026-08-29T00:00:00Z","_msg":"spawn ok","source":"controller","level":"INFO"}`+"\n")
	}))
	defer srv.Close()
	t.Setenv("GARDENIO_LOGS_URL", srv.URL)

	// Reset flags to defaults (cobra flag state persists across tests).
	logsCmd.Flags().Set("since", "15m")
	logsCmd.Flags().Set("grep", "")
	logsCmd.Flags().Set("limit", "200")
	logsCmd.Flags().Set("url", "")
	jsonOutput = false

	out := string(captureStdout(t, func() error {
		return runLogs(logsCmd, []string{"controller"})
	}))
	if !strings.Contains(out, "spawn ok") || !strings.Contains(out, "[controller]") || !strings.Contains(out, "INFO") {
		t.Errorf("human output missing fields: %q", out)
	}
}

func TestFormatLineFallback(t *testing.T) {
	// A record with none of the stamped fields falls back to raw JSON.
	l := vlogs.Line{Raw: map[string]any{"weird": "value"}}
	got := formatLine(l)
	if !strings.Contains(got, "weird") {
		t.Errorf("fallback did not include raw record: %q", got)
	}
}
