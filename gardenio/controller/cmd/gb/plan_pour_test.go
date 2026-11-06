package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func TestParsePlanPourVars(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		got, err := parsePlanPourVars(nil)
		if err != nil || got != nil {
			t.Fatalf("parsePlanPourVars(nil) = %v, %v; want nil, nil", got, err)
		}
	})
	t.Run("parses k=v", func(t *testing.T) {
		got, err := parsePlanPourVars([]string{"component=auth", "version=1.2=3"})
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if got["component"] != "auth" {
			t.Errorf("component = %q, want auth", got["component"])
		}
		// Only the FIRST '=' splits, so a value may itself contain '='.
		if got["version"] != "1.2=3" {
			t.Errorf("version = %q, want 1.2=3", got["version"])
		}
	})
	for _, bad := range []string{"noequals", "=novalue"} {
		t.Run("rejects "+bad, func(t *testing.T) {
			if _, err := parsePlanPourVars([]string{bad}); err == nil {
				t.Errorf("parsePlanPourVars(%q) = nil err, want error", bad)
			}
		})
	}
}

// planPourStub captures the pour request the CLI sends and returns a canned
// molecule response.
type planPourStub struct {
	gotFormulaID  string
	gotTargetPlan string
	gotVars       map[string]string
	gotForce      bool
	gotPosition   *int
}

func (s *planPourStub) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/formulas/{id}/pour", func(w http.ResponseWriter, r *http.Request) {
		s.gotFormulaID = r.PathValue("id")
		body, _ := io.ReadAll(r.Body)
		var req beadsapi.PourFormulaRequest
		_ = json.Unmarshal(body, &req)
		s.gotTargetPlan = req.TargetPlan
		s.gotVars = req.Vars
		s.gotForce = req.Force
		s.gotPosition = req.TargetPosition

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"molecule":    map[string]any{"id": "gd-mol-new", "title": "Poured phase"},
			"total_steps": 2,
			"plan":        []any{},
		})
	})
	return mux
}

func setupPlanPourDaemon(t *testing.T, stub *planPourStub) {
	t.Helper()
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	old := daemon
	daemon = c
	t.Cleanup(func() { daemon = old })
}

func TestRunPlanPour_EmplacesFormulaIntoPlan(t *testing.T) {
	stub := &planPourStub{}
	setupPlanPourDaemon(t, stub)

	// The verb runs on behalf of an oidc:<email> operator (operatorEmailForViewer).
	oldOp := operatedBy
	operatedBy = "oidc:matt@acme.example"
	t.Cleanup(func() { operatedBy = oldOp })

	planPourFormula = "gd-formula-1"
	planPourVars = []string{"component=auth"}
	planPourForce = true
	planPourEphemeral = false
	t.Cleanup(func() {
		planPourFormula, planPourVars, planPourForce, planPourEphemeral = "", nil, false, false
	})

	planPourCmd.SetContext(context.Background())
	if err := runPlanPour(planPourCmd, []string{"gd-plan-1"}); err != nil {
		t.Fatalf("runPlanPour: %v", err)
	}

	if stub.gotFormulaID != "gd-formula-1" {
		t.Errorf("formula id = %q, want gd-formula-1", stub.gotFormulaID)
	}
	if stub.gotTargetPlan != "gd-plan-1" {
		t.Errorf("target_plan = %q, want gd-plan-1 (the plan arg must be threaded as the emplacement target)", stub.gotTargetPlan)
	}
	if stub.gotVars["component"] != "auth" {
		t.Errorf("vars = %v, want component=auth", stub.gotVars)
	}
	if !stub.gotForce {
		t.Error("force flag not threaded to the pour request")
	}
}

// TestRunPlanPour_ThreadsPosition — `--position N` threads through as
// TargetPosition so the CLI can emplace a phase at a chosen slot (ka-0000001001).
// The flag is opt-in: it is only sent when explicitly set (cmd.Flags().Changed).
func TestRunPlanPour_ThreadsPosition(t *testing.T) {
	stub := &planPourStub{}
	setupPlanPourDaemon(t, stub)

	oldOp := operatedBy
	operatedBy = "oidc:matt@acme.example"
	t.Cleanup(func() { operatedBy = oldOp })

	planPourFormula = "gd-formula-1"
	t.Cleanup(func() {
		planPourFormula, planPourVars, planPourForce, planPourEphemeral = "", nil, false, false
	})

	// Drive the flag through cobra so cmd.Flags().Changed("position") is true.
	planPourCmd.SetContext(context.Background())
	if err := planPourCmd.Flags().Set("position", "1"); err != nil {
		t.Fatalf("set position flag: %v", err)
	}
	// planPourCmd is a package singleton, so the Changed bit persists across
	// tests — reset both the value AND the Changed flag so a later test that
	// omits --position sees a clean slate.
	t.Cleanup(func() {
		_ = planPourCmd.Flags().Set("position", "-1")
		if f := planPourCmd.Flags().Lookup("position"); f != nil {
			f.Changed = false
		}
		planPourPosition = -1
	})

	if err := runPlanPour(planPourCmd, []string{"gd-plan-1"}); err != nil {
		t.Fatalf("runPlanPour: %v", err)
	}
	if stub.gotPosition == nil {
		t.Fatal("--position 1 must thread through as a non-nil TargetPosition")
	}
	if *stub.gotPosition != 1 {
		t.Errorf("TargetPosition = %d, want 1", *stub.gotPosition)
	}
}

// TestRunPlanPour_OmittedPositionIsNil — without --position the request carries a
// nil TargetPosition (the daemon tail-appends, unchanged default).
func TestRunPlanPour_OmittedPositionIsNil(t *testing.T) {
	stub := &planPourStub{}
	setupPlanPourDaemon(t, stub)

	oldOp := operatedBy
	operatedBy = "oidc:matt@acme.example"
	t.Cleanup(func() { operatedBy = oldOp })

	planPourFormula = "gd-formula-1"
	t.Cleanup(func() { planPourFormula = "" })

	planPourCmd.SetContext(context.Background())
	if err := runPlanPour(planPourCmd, []string{"gd-plan-1"}); err != nil {
		t.Fatalf("runPlanPour: %v", err)
	}
	if stub.gotPosition != nil {
		t.Errorf("omitted --position must leave TargetPosition nil, got %d", *stub.gotPosition)
	}
}

func TestRunPlanPour_RequiresFormula(t *testing.T) {
	oldOp := operatedBy
	operatedBy = "oidc:matt@acme.example"
	t.Cleanup(func() { operatedBy = oldOp })
	planPourFormula = ""
	t.Cleanup(func() { planPourFormula = "" })

	if err := runPlanPour(planPourCmd, []string{"gd-plan-1"}); err == nil {
		t.Error("expected an error when --formula is missing")
	}
}
