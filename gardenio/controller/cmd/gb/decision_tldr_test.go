package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// TestTLDRLimits exercises the helper that extracts length params from the
// chosen option's artifact_params.
func TestTLDRLimits(t *testing.T) {
	tests := []struct {
		name                       string
		optionsJSON                string
		chosenID                   string
		wantMax, wantMin, wantWarn int
	}{
		{
			name:     "defaults when options empty",
			wantMax:  500,
			wantMin:  0,
			wantWarn: 350,
		},
		{
			name:        "defaults when chosen does not match",
			optionsJSON: `[{"id":"a","artifact_type":"tldr","artifact_params":{"max_chars":400}}]`,
			chosenID:    "b",
			wantMax:     500,
			wantMin:     0,
			wantWarn:    350,
		},
		{
			name:        "defaults when option has no artifact_params",
			optionsJSON: `[{"id":"a","artifact_type":"tldr"}]`,
			chosenID:    "a",
			wantMax:     500,
			wantMin:     0,
			wantWarn:    350,
		},
		{
			name:        "max only overrides max",
			optionsJSON: `[{"id":"a","artifact_type":"tldr","artifact_params":{"max_chars":400}}]`,
			chosenID:    "a",
			wantMax:     400,
			wantMin:     0,
			wantWarn:    350,
		},
		{
			name:        "all three params override",
			optionsJSON: `[{"id":"a","artifact_type":"tldr","artifact_params":{"max_chars":600,"min_chars":40,"warn_at":450}}]`,
			chosenID:    "a",
			wantMax:     600,
			wantMin:     40,
			wantWarn:    450,
		},
		{
			name:        "malformed JSON returns defaults",
			optionsJSON: `not json`,
			chosenID:    "a",
			wantMax:     500,
			wantMin:     0,
			wantWarn:    350,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotMax, gotMin, gotWarn := tldrLimits(tc.optionsJSON, tc.chosenID)
			if gotMax != tc.wantMax || gotMin != tc.wantMin || gotWarn != tc.wantWarn {
				t.Errorf("tldrLimits = (%d,%d,%d); want (%d,%d,%d)",
					gotMax, gotMin, gotWarn, tc.wantMax, tc.wantMin, tc.wantWarn)
			}
		})
	}
}

// reportDaemonStub handles all endpoints the report handler calls so the test
// can focus on validation behavior rather than plumbing.
type reportDaemonStub struct {
	decisionFields map[string]string
}

func (s *reportDaemonStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/beads/"):
			id := strings.TrimPrefix(r.URL.Path, "/v1/beads/")
			bead := map[string]any{
				"id":     id,
				"type":   "decision",
				"status": "open",
				"fields": s.decisionFields,
			}
			_ = json.NewEncoder(w).Encode(bead)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/beads":
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "gd-report-1"})
		default:
			// Accept everything else (deps add, close, gate satisfy, field updates).
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	})
}

// runReportCmd invokes the report subcommand with fresh flags and returns
// (err, stderr). KD_AGENT_ID is unset so satisfy-gate paths no-op silently.
func runReportCmd(t *testing.T, stub *reportDaemonStub, content, reportType string) (error, string) {
	t.Helper()
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)

	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	oldDaemon := daemon
	daemon = c
	t.Cleanup(func() { daemon = oldDaemon })

	oldAgentID := os.Getenv("KD_AGENT_ID")
	_ = os.Unsetenv("KD_AGENT_ID")
	t.Cleanup(func() {
		if oldAgentID != "" {
			_ = os.Setenv("KD_AGENT_ID", oldAgentID)
		}
	})

	// Capture stderr.
	oldStderr := os.Stderr
	rPipe, wPipe, _ := os.Pipe()
	os.Stderr = wPipe
	t.Cleanup(func() { os.Stderr = oldStderr })

	cmd := decisionReportCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("content", content)
	_ = cmd.Flags().Set("type", reportType)
	_ = cmd.Flags().Set("format", "markdown")
	t.Cleanup(func() {
		cmd.Flags().Set("content", "")
		cmd.Flags().Set("type", "")
		cmd.Flags().Set("format", "markdown")
	})

	runErr := cmd.RunE(cmd, []string{"gd-decision-1"})
	_ = wPipe.Close()
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, rPipe)
	return runErr, buf.String()
}

func TestDecisionReport_TLDRLengthValidation(t *testing.T) {
	// Default options have max_chars=400, min_chars=40, warn_at=350 so we can
	// exercise all boundaries deterministically.
	optsJSON := `[{"id":"done","short":"Summary","label":"Summary","artifact_type":"tldr","artifact_params":{"max_chars":400,"min_chars":40,"warn_at":350}}]`

	mkDecisionFields := func(options, chosen string) map[string]string {
		return map[string]string{
			"options":           options,
			"chosen":            chosen,
			"required_artifact": "tldr",
		}
	}

	tests := []struct {
		name        string
		content     string
		reportType  string
		options     string
		chosen      string
		wantErr     bool
		errContains string
		wantWarnOn  bool
	}{
		{
			name:       "at max_chars passes",
			content:    strings.Repeat("a", 400),
			reportType: "tldr",
			options:    optsJSON,
			chosen:     "done",
			wantErr:    false,
			wantWarnOn: true,
		},
		{
			name:        "over max_chars fails with specific text",
			content:     strings.Repeat("a", 401),
			reportType:  "tldr",
			options:     optsJSON,
			chosen:      "done",
			wantErr:     true,
			errContains: "tldr over limit: 401 / 400 chars. Tighten to the shape \"Root cause: X. Fix: Y. Verified: Z.\" and retry.",
		},
		{
			name:        "under min_chars fails",
			content:     strings.Repeat("a", 39),
			reportType:  "tldr",
			options:     optsJSON,
			chosen:      "done",
			wantErr:     true,
			errContains: "tldr under limit: 39 / 40 chars",
		},
		{
			name:        "empty content fails",
			content:     "",
			reportType:  "tldr",
			options:     optsJSON,
			chosen:      "done",
			wantErr:     true,
			errContains: "required", // hits the pre-existing "--content or stdin is required" guard
		},
		{
			name:        "whitespace-only content fails with tldr-specific error",
			content:     "    \n\t  ",
			reportType:  "tldr",
			options:     optsJSON,
			chosen:      "done",
			wantErr:     true,
			errContains: "tldr content is empty",
		},
		{
			name:       "just over warn_at emits warning but succeeds",
			content:    strings.Repeat("a", 351),
			reportType: "tldr",
			options:    optsJSON,
			chosen:     "done",
			wantErr:    false,
			wantWarnOn: true,
		},
		{
			name:       "at warn_at emits no warning",
			content:    strings.Repeat("a", 350),
			reportType: "tldr",
			options:    optsJSON,
			chosen:     "done",
			wantErr:    false,
			wantWarnOn: false,
		},
		{
			name:       "defaults when no options match: max=500 allows 600 only if params are set; here 450 passes",
			content:    strings.Repeat("a", 450),
			reportType: "tldr",
			options:    "", // no options → defaults (max=500, min=0, warn=350)
			chosen:     "",
			wantErr:    false,
			wantWarnOn: true,
		},
		{
			name:        "defaults: 501 over limit triggers default max=500 error",
			content:     strings.Repeat("a", 501),
			reportType:  "tldr",
			options:     "",
			chosen:      "",
			wantErr:     true,
			errContains: "tldr over limit: 501 / 500 chars",
		},
		{
			name:       "non-tldr report type: oversize content is not rejected by validator",
			content:    strings.Repeat("a", 10000),
			reportType: "plan",
			options:    optsJSON,
			chosen:     "done",
			wantErr:    false,
			wantWarnOn: false,
		},
		{
			name:       "non-tldr report type: empty whitespace still rejected by pre-existing guard",
			content:    "",
			reportType: "checklist",
			options:    optsJSON,
			chosen:     "done",
			wantErr:    true,
			// hits the "--content or stdin is required" guard
			errContains: "required",
			wantWarnOn:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stub := &reportDaemonStub{
				decisionFields: mkDecisionFields(tc.options, tc.chosen),
			}
			// If the test specifies a non-tldr reportType, override required_artifact
			// so the handler picks up reportType from the flag rather than the field.
			if tc.reportType != "tldr" {
				stub.decisionFields["required_artifact"] = tc.reportType
			}
			err, stderr := runReportCmd(t, stub, tc.content, tc.reportType)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (stderr=%q)", stderr)
				}
				if tc.errContains != "" && !strings.Contains(err.Error(), tc.errContains) {
					t.Errorf("error %q does not contain %q", err.Error(), tc.errContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v (stderr=%q)", err, stderr)
			}
			hasWarn := strings.Contains(stderr, "warning: tldr is")
			if hasWarn != tc.wantWarnOn {
				t.Errorf("warning-on-stderr = %v, want %v (stderr=%q)", hasWarn, tc.wantWarnOn, stderr)
			}
		})
	}
}
