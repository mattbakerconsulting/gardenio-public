package main

import (
	"context"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// withFakePlanWriter substitutes an in-memory fakePlanWriter for the daemon the
// `gb plan` WRITE verbs compose against (the planCmdWriter seam), restoring the
// real resolver on cleanup. This is what lets a RunE end-to-end test exercise the
// full command wiring — operator validation, flag reads, helper dispatch, output
// — with no live daemon and no HTTP round-trip.
func withFakePlanWriter(t *testing.T, f planDaemonWriter) {
	t.Helper()
	prev := planCmdWriter
	planCmdWriter = func() planDaemonWriter { return f }
	t.Cleanup(func() { planCmdWriter = prev })
}

// restorePlanGlobals snapshots the package-level add-step command globals the
// plan tests mutate and restores them on cleanup, so a test's overrides never
// leak into another test in the package.
func restorePlanGlobals(t *testing.T) {
	t.Helper()
	prevTitle, prevPhase, prevOp, prevJSON := planAddStepTitle, planAddStepPhase, operatedBy, jsonOutput
	t.Cleanup(func() {
		planAddStepTitle, planAddStepPhase, operatedBy, jsonOutput = prevTitle, prevPhase, prevOp, prevJSON
	})
}

// TestRunPlanAddStep_EndToEnd drives runPlanAddStep through its full RunE path:
// operator validation + the daemon-direct addPlanStep helper — asserting the fake
// writer sees a step minted, linked parent-child under the plan, and stamped with
// the operator. Guards the command wiring, not just the helper.
func TestRunPlanAddStep_EndToEnd(t *testing.T) {
	f := newFakePlanWriter()
	f.add(&beadsapi.BeadDetail{ID: "gd-EpicXYZ", Type: "epic", Status: "open", Labels: []string{"plan", "project:gardenio"}})
	withFakePlanWriter(t, f)

	// runPlanAddStep reads package-level flag vars + the operatedBy global; set
	// them the way the persistent pre-run + flag parsing would.
	restorePlanGlobals(t)
	planAddStepTitle = "Wire the mirror"
	planAddStepPhase = ""
	operatedBy = "oidc:matt@acme.example"
	jsonOutput = false

	// planAddStepCmd is a package-level *cobra.Command; the tests that touch it
	// (via SetContext) must not run in parallel.
	cmd := planAddStepCmd
	cmd.SetContext(context.Background())
	if err := runPlanAddStep(cmd, []string{"gd-EpicXYZ"}); err != nil {
		t.Fatalf("runPlanAddStep: %v", err)
	}

	create := f.opsOfKind("create")
	if len(create) != 1 || create[0].req.Title != "Wire the mirror" {
		t.Fatalf("create = %+v, want a single step titled 'Wire the mirror'", create)
	}
	dep := f.opsOfKind("dep")
	if len(dep) != 1 || dep[0].depTo != "gd-EpicXYZ" || dep[0].depType != "parent-child" {
		t.Errorf("dep = %+v, want a parent-child edge to gd-EpicXYZ", dep)
	}
	if f.lastOperator != "oidc:matt@acme.example" {
		t.Errorf("operator = %q, want oidc:matt@acme.example", f.lastOperator)
	}
}

// TestRunPlanAddStep_UnderPhase covers the --phase branch: the step is parented
// to the named phase sub-epic, not the plan directly.
func TestRunPlanAddStep_UnderPhase(t *testing.T) {
	f := newFakePlanWriter()
	f.add(&beadsapi.BeadDetail{ID: "gd-EpicXYZ", Type: "epic", Status: "open",
		Labels: []string{"plan", "project:gardenio"},
		Fields: map[string]string{"phase_order": `["gd-PH1"]`}})
	f.add(&beadsapi.BeadDetail{ID: "gd-PH1", Type: "epic", Status: "open", Labels: []string{"project:gardenio"}})
	withFakePlanWriter(t, f)

	restorePlanGlobals(t)
	planAddStepTitle = "Design the schema"
	planAddStepPhase = "gd-PH1"
	operatedBy = "oidc:matt@acme.example"
	jsonOutput = false

	cmd := planAddStepCmd
	cmd.SetContext(context.Background())
	if err := runPlanAddStep(cmd, []string{"gd-EpicXYZ"}); err != nil {
		t.Fatalf("runPlanAddStep(phase): %v", err)
	}
	dep := f.opsOfKind("dep")
	if len(dep) != 1 || dep[0].depTo != "gd-PH1" {
		t.Errorf("dep target = %+v, want gd-PH1", dep)
	}
}

// TestRunPlanAddStep_JSONOutput covers the jsonOutput=true branch of
// runPlanAddStep (the printJSON path), which the plain end-to-end test skips.
func TestRunPlanAddStep_JSONOutput(t *testing.T) {
	f := newFakePlanWriter()
	f.add(&beadsapi.BeadDetail{ID: "gd-EpicXYZ", Type: "epic", Status: "open", Labels: []string{"plan", "project:gardenio"}})
	withFakePlanWriter(t, f)

	restorePlanGlobals(t)
	planAddStepTitle = "Wire the mirror"
	planAddStepPhase = ""
	operatedBy = "oidc:matt@acme.example"
	jsonOutput = true

	cmd := planAddStepCmd
	cmd.SetContext(context.Background())
	if err := runPlanAddStep(cmd, []string{"gd-EpicXYZ"}); err != nil {
		t.Fatalf("runPlanAddStep: %v", err)
	}
	if len(f.opsOfKind("create")) != 1 {
		t.Errorf("want a single step create in the JSON-output path, got %d", len(f.opsOfKind("create")))
	}
}

func TestRunPlanAddStep_MissingTitle(t *testing.T) {
	restorePlanGlobals(t)
	planAddStepTitle = ""
	operatedBy = "oidc:matt@acme.example"

	cmd := planAddStepCmd
	cmd.SetContext(context.Background())
	if err := runPlanAddStep(cmd, []string{"gd-P"}); err == nil {
		t.Fatal("want error when --title is empty")
	}
}

func TestRunPlanAddStep_NonHumanOperatorRejected(t *testing.T) {
	restorePlanGlobals(t)
	planAddStepTitle = "t"
	// A svc:/machine operator must be rejected: gb plan writes are stamped on
	// behalf of a HUMAN (ka-0000005347 / ka-0000006809), never a machine sink.
	operatedBy = "svc:steering-gardener"

	cmd := planAddStepCmd
	cmd.SetContext(context.Background())
	if err := runPlanAddStep(cmd, []string{"gd-P"}); err == nil {
		t.Fatal("want error when operated-by is not a human (oidc:<email> or slack:U…) id")
	}
}

// TestRunPlanAddStep_HelperErrorSurfaces proves a daemon-layer failure (here a
// missing plan epic) is surfaced by the command, not swallowed into a bogus
// success.
func TestRunPlanAddStep_HelperErrorSurfaces(t *testing.T) {
	f := newFakePlanWriter() // no plan epic seeded → loadPlanEpic fails
	withFakePlanWriter(t, f)

	restorePlanGlobals(t)
	planAddStepTitle = "t"
	planAddStepPhase = ""
	operatedBy = "oidc:matt@acme.example"

	cmd := planAddStepCmd
	cmd.SetContext(context.Background())
	if err := runPlanAddStep(cmd, []string{"gd-MISSING"}); err == nil {
		t.Fatal("want error when the plan epic does not exist")
	}
}

func TestOperatorEmailForViewer(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"plain oidc", "oidc:matt@acme.example", "matt@acme.example", false},
		{"oidc with subdomain", "oidc:a.b@corp.example.com", "a.b@corp.example.com", false},
		// slack:U… is a first-class HUMAN operator (the human behind the agent):
		// accepted and returned verbatim so an agent gardener can drive the verbs
		// with real human attribution — no svc: identity (ka-0000005347).
		{"slack accepted", "slack:U0OPERATOR1", "slack:U0OPERATOR1", false},
		{"slack lowercase rejected", "slack:u0operator1", "", true},
		{"slack no U rejected", "slack:07K1RLQAKF", "", true},
		{"slack empty rejected", "slack:", "", true},
		{"empty rejected", "", "", true},
		{"bare agent rejected", "gardenio-steering-gardener", "", true},
		{"svc sentinel rejected", "svc:steering-gardener", "", true},
		{"system sentinel rejected", "system:machine", "", true},
		{"oidc no domain dot rejected", "oidc:matt@localhost", "", true},
		{"oidc no at rejected", "oidc:matt", "", true},
		{"oidc empty local rejected", "oidc:@acme.example", "", true},
		// These MUST match kbeads isHumanOperator exactly — a leading/trailing
		// dot domain is rejected there, so it must be rejected here too or the
		// verb stamps an id the daemon then 400s.
		{"oidc leading-dot domain rejected", "oidc:foo@.bar.com", "", true},
		{"oidc trailing-dot domain rejected", "oidc:foo@bar.", "", true},
		{"oidc bare-dot domain rejected", "oidc:foo@.", "", true},
		{"oidc over-128 rejected", "oidc:" + strings.Repeat("a", 120) + "@acme.example", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := operatorEmailForViewer(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("operatorEmailForViewer(%q) = %q, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("operatorEmailForViewer(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("operatorEmailForViewer(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
