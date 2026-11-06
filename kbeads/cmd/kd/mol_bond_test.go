package main

import (
	"strings"
	"testing"
)

func TestBondCmd_Sequential(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	cmd := newBondCmd()
	cmd.SetArgs([]string{"gd-mol-a", "gd-mol-b"})
	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("bond: %v", err)
		}
	})

	if len(mc.AddDepCalls) != 1 {
		t.Fatalf("expected 1 AddDependency call, got %d", len(mc.AddDepCalls))
	}
	dep := mc.AddDepCalls[0]
	if dep.BeadID != "gd-mol-b" {
		t.Errorf("dep.BeadID = %q, want gd-mol-b", dep.BeadID)
	}
	if dep.DependsOnID != "gd-mol-a" {
		t.Errorf("dep.DependsOnID = %q, want gd-mol-a", dep.DependsOnID)
	}
	if dep.Type != "blocks" {
		t.Errorf("dep.Type = %q, want blocks (sequential default)", dep.Type)
	}
	if dep.CreatedBy != "test-actor" {
		t.Errorf("dep.CreatedBy = %q, want test-actor", dep.CreatedBy)
	}

	if !strings.Contains(out, "Bonded gd-mol-a") {
		t.Errorf("output missing bond confirmation, got:\n%s", out)
	}
}

func TestBondCmd_SequentialExplicit(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	cmd := newBondCmd()
	cmd.SetArgs([]string{"gd-a", "gd-b", "--type", "sequential"})
	captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("bond sequential: %v", err)
		}
	})

	if mc.AddDepCalls[0].Type != "blocks" {
		t.Errorf("explicit sequential type = %q, want blocks", mc.AddDepCalls[0].Type)
	}
}

func TestBondCmd_Parallel(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	cmd := newBondCmd()
	cmd.SetArgs([]string{"gd-a", "gd-b", "--type", "parallel"})
	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("bond parallel: %v", err)
		}
	})

	dep := mc.AddDepCalls[0]
	if dep.Type != "related" {
		t.Errorf("parallel type = %q, want related", dep.Type)
	}
	if !strings.Contains(out, "related") {
		t.Errorf("output should mention 'related', got:\n%s", out)
	}
}

func TestBondCmd_UnknownType(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	cmd := newBondCmd()
	cmd.SetArgs([]string{"gd-a", "gd-b", "--type", "invalid"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for unknown bond type")
	}
	if !strings.Contains(err.Error(), "unknown bond type") {
		t.Errorf("error = %q, want 'unknown bond type'", err)
	}
}

func TestBondCmd_Conditional(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	cmd := newBondCmd()
	cmd.SetArgs([]string{"gd-a", "gd-b", "--type", "conditional"})
	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("bond conditional: %v", err)
		}
	})

	dep := mc.AddDepCalls[0]
	if dep.Type != "conditional" {
		t.Errorf("conditional type = %q, want conditional", dep.Type)
	}
	if !strings.Contains(out, "conditional") {
		t.Errorf("output should mention 'conditional', got:\n%s", out)
	}
}

func TestBondCmd_Direction(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	// bond gd-first gd-second: gd-second depends on gd-first.
	cmd := newBondCmd()
	cmd.SetArgs([]string{"gd-first", "gd-second"})
	captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("bond: %v", err)
		}
	})

	dep := mc.AddDepCalls[0]
	// B (second arg) is the bead that has the dependency.
	if dep.BeadID != "gd-second" {
		t.Errorf("dep.BeadID = %q, want gd-second", dep.BeadID)
	}
	// A (first arg) is the one it depends on.
	if dep.DependsOnID != "gd-first" {
		t.Errorf("dep.DependsOnID = %q, want gd-first", dep.DependsOnID)
	}
}
