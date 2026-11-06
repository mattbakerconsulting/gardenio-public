package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

func TestBuildOperatorSignalsTileView_HappyPathAllOK(t *testing.T) {
	api := &fakeDaemon{
		acceptanceResp: &beadsapi.AcceptanceMetrics{
			OwnerShareMatt:        beadsapi.AcceptanceOwnerShare{Total: 100, Matt: 4, Fraction: 0.04},
			AgentOperatorLeaks:    beadsapi.AcceptanceOperatorLeaks{Count: 0},
			FleetFallbackCount:    beadsapi.AcceptanceFleetFallback{Count: 0},
			OperatorAliasCoverage: beadsapi.AcceptanceAliasCoverage{Total: 12, Matched: 12, Fraction: 1.0},
		},
	}
	v := buildOperatorSignalsTileView(context.Background(), api, time.Now())

	if v.Error != "" {
		t.Fatalf("unexpected error: %q", v.Error)
	}
	if v.OwnerShareMatt.State != operatorSignalStateOK {
		t.Errorf("owner-share state = %q, want ok", v.OwnerShareMatt.State)
	}
	if !strings.Contains(v.OwnerShareMatt.Display, "4/100") || !strings.Contains(v.OwnerShareMatt.Display, "4%") {
		t.Errorf("owner-share display = %q, want '4/100 (4%%)'", v.OwnerShareMatt.Display)
	}
	if v.AgentOperatorLeaks.State != operatorSignalStateOK {
		t.Errorf("leaks state = %q, want ok", v.AgentOperatorLeaks.State)
	}
	if v.AgentOperatorLeaks.Display != "0" {
		t.Errorf("leaks display = %q, want 0", v.AgentOperatorLeaks.Display)
	}
	if v.FleetFallbackCount.State != operatorSignalStateOK {
		t.Errorf("fleet-fallback state = %q, want ok", v.FleetFallbackCount.State)
	}
	if v.FleetFallbackCount.Display != "0" {
		t.Errorf("fleet-fallback display = %q, want 0", v.FleetFallbackCount.Display)
	}
	if v.OperatorAliasCoverage.State != operatorSignalStateOK {
		t.Errorf("alias state = %q, want ok", v.OperatorAliasCoverage.State)
	}
	if !strings.Contains(v.OperatorAliasCoverage.Display, "12/12") || !strings.Contains(v.OperatorAliasCoverage.Display, "100%") {
		t.Errorf("alias display = %q, want '12/12 (100%%)'", v.OperatorAliasCoverage.Display)
	}
}

func TestBuildOperatorSignalsTileView_WarnStates(t *testing.T) {
	api := &fakeDaemon{
		acceptanceResp: &beadsapi.AcceptanceMetrics{
			// 30% Matt-as-owner — over the 10% warn threshold
			OwnerShareMatt: beadsapi.AcceptanceOwnerShare{Total: 10, Matt: 3, Fraction: 0.30},
			// Any leak count > 0 is warn
			AgentOperatorLeaks: beadsapi.AcceptanceOperatorLeaks{Count: 7},
			// Any fleet fallback > 0 is warn
			FleetFallbackCount: beadsapi.AcceptanceFleetFallback{Count: 17},
			// 5/10 alias coverage — under the 90% warn threshold
			OperatorAliasCoverage: beadsapi.AcceptanceAliasCoverage{Total: 10, Matched: 5, Fraction: 0.5},
		},
	}
	v := buildOperatorSignalsTileView(context.Background(), api, time.Now())

	if v.OwnerShareMatt.State != operatorSignalStateWarn {
		t.Errorf("owner-share state = %q, want warn", v.OwnerShareMatt.State)
	}
	if v.AgentOperatorLeaks.State != operatorSignalStateWarn {
		t.Errorf("leaks state = %q, want warn", v.AgentOperatorLeaks.State)
	}
	if v.FleetFallbackCount.State != operatorSignalStateWarn {
		t.Errorf("fleet-fallback state = %q, want warn", v.FleetFallbackCount.State)
	}
	if v.OperatorAliasCoverage.State != operatorSignalStateWarn {
		t.Errorf("alias state = %q, want warn", v.OperatorAliasCoverage.State)
	}
}

func TestBuildOperatorSignalsTileView_EmptyAliasPopulationStaysOK(t *testing.T) {
	// When no type=user beads have both a slack_id and email, the alias
	// coverage row should NOT render warn — there is nothing to cover yet,
	// not a regression.
	api := &fakeDaemon{
		acceptanceResp: &beadsapi.AcceptanceMetrics{
			OperatorAliasCoverage: beadsapi.AcceptanceAliasCoverage{Total: 0, Matched: 0, Fraction: 0},
		},
	}
	v := buildOperatorSignalsTileView(context.Background(), api, time.Now())
	if v.OperatorAliasCoverage.State != operatorSignalStateOK {
		t.Errorf("alias state = %q with empty population, want ok", v.OperatorAliasCoverage.State)
	}
}

func TestBuildOperatorSignalsTileView_DaemonError(t *testing.T) {
	api := &fakeDaemon{acceptanceErr: errors.New("boom")}
	v := buildOperatorSignalsTileView(context.Background(), api, time.Now())
	if v.Error == "" {
		t.Errorf("expected Error to be populated on daemon failure")
	}
}

func TestBuildOperatorSignalsTileView_NilAPI(t *testing.T) {
	v := buildOperatorSignalsTileView(context.Background(), nil, time.Now())
	if v.Error == "" {
		t.Errorf("expected Error when api is nil")
	}
}

func TestHandleHealthTileOperatorSignals_RendersFragment(t *testing.T) {
	api := &fakeDaemon{
		acceptanceResp: &beadsapi.AcceptanceMetrics{
			OwnerShareMatt:        beadsapi.AcceptanceOwnerShare{Total: 100, Matt: 4, Fraction: 0.04},
			AgentOperatorLeaks:    beadsapi.AcceptanceOperatorLeaks{Count: 0},
			FleetFallbackCount:    beadsapi.AcceptanceFleetFallback{Count: 0},
			OperatorAliasCoverage: beadsapi.AcceptanceAliasCoverage{Total: 12, Matched: 12, Fraction: 1.0},
		},
	}
	srv := NewServer(api, slog.Default(), "", "http://test", "", nil)
	req := httptest.NewRequest(http.MethodGet, "/health/tile/operator-signals", nil)
	rec := httptest.NewRecorder()
	srv.handleHealthTileOperatorSignals(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Operator hygiene",
		"Matt-as-owner",
		"4/100",
		"Agent-operator leaks",
		"Fleet fallbacks",
		"Alias coverage",
		"12/12",
		"ka-0000008007",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n--- body ---\n%s", want, body)
		}
	}
	if api.acceptanceCalls != 1 {
		t.Errorf("expected exactly 1 daemon call, got %d", api.acceptanceCalls)
	}
}

func TestFormatPercent(t *testing.T) {
	tests := []struct {
		in   float64
		want string
	}{
		{0, "0%"},
		{0.04, "4%"},
		{0.5, "50%"},
		{0.995, "100%"},
		{1.0, "100%"},
		{1.5, "100%"}, // clamped
		{-0.1, "0%"},  // clamped
		{0.084, "8%"}, // rounds down at <0.5
		{0.086, "9%"}, // rounds up at >=0.5
	}
	for _, tc := range tests {
		if got := formatPercent(tc.in); got != tc.want {
			t.Errorf("formatPercent(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
