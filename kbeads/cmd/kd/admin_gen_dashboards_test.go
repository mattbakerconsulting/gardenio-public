package main

import (
	"encoding/json"
	"strings"
	"testing"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/server"
)

// expectedCostsModel assembles the live model bytes the command should write
// for the costs-candidate dashboard, straight from the catalog assembler.
func expectedCostsModel(t *testing.T) string {
	t.Helper()
	d, err := server.AssembleDashboardByUID("gardenio-costs-candidate")
	if err != nil {
		t.Fatalf("AssembleDashboardByUID: %v", err)
	}
	b, err := server.MarshalDashboard(d)
	if err != nil {
		t.Fatalf("MarshalDashboard: %v", err)
	}
	return string(b)
}

// dashboardBead builds a type=dashboard bead carrying the given uid.
func dashboardBead(id, uid string) *model.Bead {
	raw, _ := json.Marshal(map[string]any{"dashboard_uid": uid})
	return &model.Bead{ID: id, Type: "dashboard", Status: model.StatusOpen, Fields: raw}
}

// modelDocBead builds a grafana:dashboard-model type=doc bead with content.
func modelDocBead(id, content string) *model.Bead {
	raw, _ := json.Marshal(map[string]any{"format": "code", "content": content})
	return &model.Bead{
		ID:     id,
		Type:   "doc",
		Status: model.StatusOpen,
		Labels: []string{dashboardModelDocLabel, "project:gardenio"},
		Fields: raw,
	}
}

func resetGenDashboardsFlags(t *testing.T) {
	t.Helper()
	if err := adminGenDashboardsCmd.Flags().Set("dry-run", "false"); err != nil {
		t.Fatalf("reset dry-run: %v", err)
	}
	if err := adminGenDashboardsCmd.Flags().Set("check", "false"); err != nil {
		t.Fatalf("reset check: %v", err)
	}
}

// TestGenDashboards_UpdatesStaleModelDoc is the core acceptance path: a
// dashboard bead whose model doc carries stale content gets its content field
// replaced with the freshly assembled catalog model.
func TestGenDashboards_UpdatesStaleModelDoc(t *testing.T) {
	mc := newMockClient()
	dash := dashboardBead("gd-dash", "gardenio-costs-candidate")
	doc := modelDocBead("gd-doc", `{"stale":true}`)
	mc.Beads["gd-dash"] = dash
	mc.Beads["gd-doc"] = doc
	mc.ListBeadsResult = []*model.Bead{dash}
	// Doc is a reverse dependency of the dashboard (doc → parent-child → dash).
	mc.RevDeps["gd-dash"] = []*model.Dependency{
		{BeadID: "gd-doc", DependsOnID: "gd-dash", Type: model.DepParentChild},
	}

	withMockClient(t, mc)
	defer resetGenDashboardsFlags(t)
	_ = adminGenDashboardsCmd.Flags().Set("dry-run", "false")

	captureStdout(t, func() {
		if err := adminGenDashboardsCmd.RunE(adminGenDashboardsCmd, []string{"gardenio-costs-candidate"}); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	// Two UpdateBead calls: the model doc content write + the dashboard bead
	// resync nudge.
	if len(mc.UpdateBeadCalls) != 2 {
		t.Fatalf("UpdateBead calls = %d, want 2 (doc content + resync nudge)", len(mc.UpdateBeadCalls))
	}
	docUpd := findUpdateForBead(t, mc, "gd-doc")
	var fields map[string]string
	if err := json.Unmarshal(docUpd.Req.Fields, &fields); err != nil {
		t.Fatalf("unmarshal update fields: %v", err)
	}
	want := expectedCostsModel(t)
	if fields["content"] != want {
		t.Errorf("written content does not match assembled model\n got %d bytes\nwant %d bytes", len(fields["content"]), len(want))
	}
	// The written model must be valid JSON carrying the right uid and 3 targets.
	assertCostsModel(t, fields["content"])

	// The dashboard bead must be nudged with an empty dashboard_hash to force a
	// grafana-bridge re-sync.
	dashUpd := findUpdateForBead(t, mc, "gd-dash")
	var dashFields map[string]string
	if err := json.Unmarshal(dashUpd.Req.Fields, &dashFields); err != nil {
		t.Fatalf("unmarshal dashboard update fields: %v", err)
	}
	if v, ok := dashFields["dashboard_hash"]; !ok || v != "" {
		t.Errorf("resync nudge dashboard_hash = %q (present=%v), want \"\"", v, ok)
	}

	// No doc creation on the update path.
	if len(mc.CreateBeadCalls) != 0 {
		t.Errorf("CreateBead calls = %d, want 0", len(mc.CreateBeadCalls))
	}
}

// findUpdateForBead returns the (single) UpdateBead call targeting id, failing
// the test if there is not exactly one.
func findUpdateForBead(t *testing.T, mc *mockBeadsClient, id string) struct {
	ID  string
	Req *client.UpdateBeadRequest
} {
	t.Helper()
	var matches []struct {
		ID  string
		Req *client.UpdateBeadRequest
	}
	for _, c := range mc.UpdateBeadCalls {
		if c.ID == id {
			matches = append(matches, c)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("UpdateBead calls for %s = %d, want 1", id, len(matches))
	}
	return matches[0]
}

// TestGenDashboards_UnchangedIsNoOp verifies an already-current model doc is
// not rewritten (byte-stable → no diff → no write).
func TestGenDashboards_UnchangedIsNoOp(t *testing.T) {
	current := expectedCostsModel(t)
	mc := newMockClient()
	dash := dashboardBead("gd-dash", "gardenio-costs-candidate")
	doc := modelDocBead("gd-doc", current)
	mc.Beads["gd-dash"] = dash
	mc.Beads["gd-doc"] = doc
	mc.ListBeadsResult = []*model.Bead{dash}
	mc.RevDeps["gd-dash"] = []*model.Dependency{
		{BeadID: "gd-doc", DependsOnID: "gd-dash", Type: model.DepParentChild},
	}

	withMockClient(t, mc)
	defer resetGenDashboardsFlags(t)

	captureStdout(t, func() {
		if err := adminGenDashboardsCmd.RunE(adminGenDashboardsCmd, []string{"gardenio-costs-candidate"}); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if len(mc.UpdateBeadCalls) != 0 {
		t.Errorf("UpdateBead calls = %d, want 0 (unchanged)", len(mc.UpdateBeadCalls))
	}
}

// TestGenDashboards_NoResyncSuppressesNudge verifies --no-resync writes the
// model doc but does NOT nudge the dashboard bead.
func TestGenDashboards_NoResyncSuppressesNudge(t *testing.T) {
	mc := newMockClient()
	dash := dashboardBead("gd-dash", "gardenio-costs-candidate")
	doc := modelDocBead("gd-doc", `{"stale":true}`)
	mc.Beads["gd-dash"] = dash
	mc.Beads["gd-doc"] = doc
	mc.ListBeadsResult = []*model.Bead{dash}
	mc.RevDeps["gd-dash"] = []*model.Dependency{
		{BeadID: "gd-doc", DependsOnID: "gd-dash", Type: model.DepParentChild},
	}

	withMockClient(t, mc)
	defer resetGenDashboardsFlags(t)
	_ = adminGenDashboardsCmd.Flags().Set("no-resync", "true")
	defer func() { _ = adminGenDashboardsCmd.Flags().Set("no-resync", "false") }()

	captureStdout(t, func() {
		if err := adminGenDashboardsCmd.RunE(adminGenDashboardsCmd, []string{"gardenio-costs-candidate"}); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	// Only the doc content write — no dashboard nudge.
	if len(mc.UpdateBeadCalls) != 1 {
		t.Fatalf("UpdateBead calls = %d, want 1 (doc only, no resync)", len(mc.UpdateBeadCalls))
	}
	if mc.UpdateBeadCalls[0].ID != "gd-doc" {
		t.Errorf("updated bead = %q, want gd-doc", mc.UpdateBeadCalls[0].ID)
	}
}

// TestGenDashboards_DryRunDoesNotWrite verifies --dry-run reports the change
// but performs no writes.
func TestGenDashboards_DryRunDoesNotWrite(t *testing.T) {
	mc := newMockClient()
	dash := dashboardBead("gd-dash", "gardenio-costs-candidate")
	doc := modelDocBead("gd-doc", `{"stale":true}`)
	mc.Beads["gd-dash"] = dash
	mc.Beads["gd-doc"] = doc
	mc.ListBeadsResult = []*model.Bead{dash}
	mc.RevDeps["gd-dash"] = []*model.Dependency{
		{BeadID: "gd-doc", DependsOnID: "gd-dash", Type: model.DepParentChild},
	}

	withMockClient(t, mc)
	defer resetGenDashboardsFlags(t)
	_ = adminGenDashboardsCmd.Flags().Set("dry-run", "true")

	out := captureStdout(t, func() {
		if err := adminGenDashboardsCmd.RunE(adminGenDashboardsCmd, []string{"gardenio-costs-candidate"}); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})
	if len(mc.UpdateBeadCalls) != 0 {
		t.Errorf("UpdateBead calls = %d, want 0 (dry-run)", len(mc.UpdateBeadCalls))
	}
	if !strings.Contains(out, "updated") {
		t.Errorf("dry-run output should report 'updated', got: %s", out)
	}
}

// TestGenDashboards_CreatesMissingModelDoc verifies a dashboard with no model
// doc gets one created, linked, and labeled.
func TestGenDashboards_CreatesMissingModelDoc(t *testing.T) {
	mc := newMockClient()
	dash := dashboardBead("gd-dash", "gardenio-costs-candidate")
	mc.Beads["gd-dash"] = dash
	mc.ListBeadsResult = []*model.Bead{dash}
	mc.RevDeps["gd-dash"] = nil // no model doc yet
	mc.CreateIDs = []string{"gd-newdoc"}

	withMockClient(t, mc)
	defer resetGenDashboardsFlags(t)

	captureStdout(t, func() {
		if err := adminGenDashboardsCmd.RunE(adminGenDashboardsCmd, []string{"gardenio-costs-candidate"}); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if len(mc.CreateBeadCalls) != 1 {
		t.Fatalf("CreateBead calls = %d, want 1", len(mc.CreateBeadCalls))
	}
	created := mc.CreateBeadCalls[0]
	if created.Type != "doc" {
		t.Errorf("created type = %q, want doc", created.Type)
	}
	var fields map[string]string
	if err := json.Unmarshal(created.Fields, &fields); err != nil {
		t.Fatalf("unmarshal created fields: %v", err)
	}
	assertCostsModel(t, fields["content"])
	// Linked parent-child under the dashboard.
	var linked bool
	for _, d := range mc.AddDepCalls {
		if d.BeadID == "gd-newdoc" && d.DependsOnID == "gd-dash" && d.Type == string(model.DepParentChild) {
			linked = true
		}
	}
	if !linked {
		t.Errorf("model doc not linked parent-child under dashboard; AddDep calls: %+v", mc.AddDepCalls)
	}
}

// TestGenDashboards_MissingDashboardBeadErrors verifies a uid with no
// dashboard bead surfaces a non-zero failure.
func TestGenDashboards_MissingDashboardBeadErrors(t *testing.T) {
	mc := newMockClient()
	mc.ListBeadsResult = nil // no dashboard beads

	withMockClient(t, mc)
	defer resetGenDashboardsFlags(t)

	captureStdout(t, func() {
		err := adminGenDashboardsCmd.RunE(adminGenDashboardsCmd, []string{"gardenio-costs-candidate"})
		if err == nil {
			t.Fatal("RunE = nil error, want failure for missing dashboard bead")
		}
	})
	if len(mc.UpdateBeadCalls) != 0 || len(mc.CreateBeadCalls) != 0 {
		t.Error("no writes expected when the dashboard bead is missing")
	}
}

// TestGenDashboards_UnknownUIDErrors verifies an unknown uid is a per-dashboard
// failure (assembler returns an error), not a panic.
func TestGenDashboards_UnknownUIDErrors(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)
	defer resetGenDashboardsFlags(t)

	captureStdout(t, func() {
		if err := adminGenDashboardsCmd.RunE(adminGenDashboardsCmd, []string{"does-not-exist"}); err == nil {
			t.Fatal("RunE = nil error, want failure for unknown uid")
		}
	})
}

// TestGenDashboards_CheckFailsOnDrift is the drift-guard acceptance path
// (ka-0000003225): --check never writes and returns a non-zero (error) result
// when the live model doc has drifted from the catalog — the exact
// gardenio-costs-candidate condition (stale live doc). The error message must
// name the drifted uid and point at the remediation command.
func TestGenDashboards_CheckFailsOnDrift(t *testing.T) {
	mc := newMockClient()
	dash := dashboardBead("gd-dash", "gardenio-costs-candidate")
	doc := modelDocBead("gd-doc", `{"stale":true}`) // drifted from the catalog
	mc.Beads["gd-dash"] = dash
	mc.Beads["gd-doc"] = doc
	mc.ListBeadsResult = []*model.Bead{dash}
	mc.RevDeps["gd-dash"] = []*model.Dependency{
		{BeadID: "gd-doc", DependsOnID: "gd-dash", Type: model.DepParentChild},
	}

	withMockClient(t, mc)
	defer resetGenDashboardsFlags(t)
	_ = adminGenDashboardsCmd.Flags().Set("check", "true")

	var runErr error
	out := captureStdout(t, func() {
		runErr = adminGenDashboardsCmd.RunE(adminGenDashboardsCmd, []string{"gardenio-costs-candidate"})
	})
	if runErr == nil {
		t.Fatal("--check RunE = nil error, want drift failure")
	}
	if !strings.Contains(runErr.Error(), "drift") ||
		!strings.Contains(runErr.Error(), "gardenio-costs-candidate") ||
		!strings.Contains(runErr.Error(), "gen-dashboards") {
		t.Errorf("drift error = %q, want mention of drift + the uid + the remediation command", runErr.Error())
	}
	// --check must never write.
	if len(mc.UpdateBeadCalls) != 0 || len(mc.CreateBeadCalls) != 0 {
		t.Errorf("--check wrote beads (updates=%d creates=%d), want 0", len(mc.UpdateBeadCalls), len(mc.CreateBeadCalls))
	}
	// It still reports the drifted uid as "updated" in the human output.
	if !strings.Contains(out, "updated") {
		t.Errorf("--check output should report the drifted dashboard as 'updated', got: %s", out)
	}
}

// TestGenDashboards_CheckPassesWhenInSync verifies --check is a clean no-op (nil
// error, no writes) when the live model doc already matches the catalog.
func TestGenDashboards_CheckPassesWhenInSync(t *testing.T) {
	current := expectedCostsModel(t)
	mc := newMockClient()
	dash := dashboardBead("gd-dash", "gardenio-costs-candidate")
	doc := modelDocBead("gd-doc", current) // byte-identical to the catalog
	mc.Beads["gd-dash"] = dash
	mc.Beads["gd-doc"] = doc
	mc.ListBeadsResult = []*model.Bead{dash}
	mc.RevDeps["gd-dash"] = []*model.Dependency{
		{BeadID: "gd-doc", DependsOnID: "gd-dash", Type: model.DepParentChild},
	}

	withMockClient(t, mc)
	defer resetGenDashboardsFlags(t)
	_ = adminGenDashboardsCmd.Flags().Set("check", "true")

	var runErr error
	captureStdout(t, func() {
		runErr = adminGenDashboardsCmd.RunE(adminGenDashboardsCmd, []string{"gardenio-costs-candidate"})
	})
	if runErr != nil {
		t.Fatalf("--check RunE = %v, want nil (in sync)", runErr)
	}
	if len(mc.UpdateBeadCalls) != 0 || len(mc.CreateBeadCalls) != 0 {
		t.Errorf("--check wrote beads (updates=%d creates=%d), want 0", len(mc.UpdateBeadCalls), len(mc.CreateBeadCalls))
	}
}

// assertCostsModel checks the written JSON is the costs-candidate dashboard
// with the three expected series (spend 24h primary, spend 1h, active agents).
func assertCostsModel(t *testing.T, content string) {
	t.Helper()
	if !json.Valid([]byte(content)) {
		t.Fatalf("written model is not valid JSON: %.80s", content)
	}
	var d struct {
		UID    string `json:"uid"`
		Panels []struct {
			Targets []struct {
				Expr         string `json:"expr"`
				LegendFormat string `json:"legendFormat"`
			} `json:"targets"`
		} `json:"panels"`
	}
	if err := json.Unmarshal([]byte(content), &d); err != nil {
		t.Fatalf("unmarshal written model: %v", err)
	}
	if d.UID != "gardenio-costs-candidate" {
		t.Errorf("uid = %q, want gardenio-costs-candidate", d.UID)
	}
	if len(d.Panels) == 0 {
		t.Fatal("written model has no panels")
	}
	// Panel 0 is cost-per-agent-hour, which leads the dashboard (ka-0000006587):
	// 3 targets — the 1h ratio, the 24h ratio, and the agent-count denominator.
	if got := len(d.Panels[0].Targets); got != 3 {
		t.Fatalf("panel 0 (cost-per-agent-hour) target count = %d, want 3 ($/agent-hour 1h, 24h, agents 1h avg)", got)
	}
	// Panel 1 is fleet-spend-rate, carrying 7 targets: primary spend (24h) + the
	// spend (1h) overlay + cost per agent (24h) + cost per agent (1h)
	// (ka-0000003828) + the active-agents overlay + the 1h/24h rolling
	// agent-count averages (ka-0000006587).
	targets := d.Panels[1].Targets
	if len(targets) != 7 {
		t.Fatalf("panel 1 target count = %d, want 7 (spend 24h, spend 1h, cost/agent 24h, cost/agent 1h, active agents, agents 1h, agents 24h)", len(targets))
	}
	joined := content
	for _, want := range []string{"gardenio_account_cost_usd", "gardenio_agents_active"} {
		if !strings.Contains(joined, want) {
			t.Errorf("written model missing expected PromQL metric %q", want)
		}
	}
}
