package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"gardenio/kbeads/internal/model"
)

// The grant table (ownership_grants.go, plan gd-fRmqrpHIwk) decides every
// ownership check: one case per inventory row of kbeads/docs/ownership-grants.md
// pins the row that admits it.

func TestOwnershipGrants_Golden(t *testing.T) {
	got, err := marshalOwnershipGrantsGolden()
	if err != nil {
		t.Fatal(err)
	}
	const path = "testdata/ownership_grants.golden.json"
	if *updateMachineGolden {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (regenerate with `go test -run TestOwnershipGrants_Golden -update`): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("the ownership grant table differs from %s (regenerate with -update and review the diff):\n got: %s\nwant: %s", path, got, want)
	}
}

func TestOwnershipGrantTable_Valid(t *testing.T) {
	if err := validateOwnershipGrantTable(ownershipGrantTable()); err != nil {
		t.Fatal(err)
	}
	roles, err := roleOwnershipGrants(roleBeadWith("gd-r", "herder", `{"ownership_grants":[`+strings.Trim(bypassRows, "[]")+`,{"types":["task"],"label":"thread","actions":["update","close"]}]}`, model.StatusOpen))
	if err != nil {
		t.Fatal(err)
	}
	if err := validateOwnershipGrantTable(roles); err != nil {
		t.Fatal(err)
	}
	ids := []string{roles[0].ID, roles[1].ID}
	if !slices.Equal(ids, []string{"role:herder/0", "role:herder/1"}) {
		t.Fatalf("role row ids = %v", ids)
	}
	if !ownershipGrantPrivileged(&roles[0]) || ownershipGrantPrivileged(&roles[1]) {
		t.Fatalf("the all-types row is privileged and the labelled row is not: %v %v", ownershipGrantPrivileged(&roles[0]), ownershipGrantPrivileged(&roles[1]))
	}
	if got, err := roleOwnershipGrants(roleBeadWith("gd-r", "herder", `{"ownership_grants":[{"types":["decision"],"label":"x","actions":["update"]}]}`, model.StatusOpen)); len(got) != 0 || err == nil {
		t.Fatalf("a stored invalid ownership_grants grants nothing, got %v %v", got, err)
	}
	for _, bad := range []ownershipGrant{
		{ID: "a", Principal: grantPrincipal{Kind: grantKindAny}, Types: grantTypes{All: true}},
		{ID: "b", Principal: grantPrincipal{Kind: "root"}, Actions: []string{"update"}, Types: grantTypes{All: true}},
		{ID: "c", Principal: grantPrincipal{Kind: grantKindAny}, Actions: []string{"update"}, Types: grantTypes{All: true}, Condition: "always"},
		{ID: "d", Principal: grantPrincipal{Kind: grantKindAny}, Actions: []string{"close"}, Types: grantTypes{Only: []model.BeadType{"epic"}}, Fields: []string{"x"}},
		{ID: "e", Principal: grantPrincipal{Kind: grantKindAny}, Actions: []string{"close"}},
	} {
		if err := validateOwnershipGrantTable([]ownershipGrant{bad}); err == nil {
			t.Errorf("row %s must be refused", bad.ID)
		}
	}
}

// Every machine-derived row traces to its machineCapabilityGrants entry, and
// every ownership-relaxing entry has its row, so the two goldens cannot
// disagree.
func TestOwnershipGrants_MachineRowsTraceToCapabilityGrants(t *testing.T) {
	want := 0
	for _, g := range machineCapabilityGrants {
		if len(g.Ownership) > 0 {
			want++
		}
		if len(g.ReclaimOwnership) > 0 {
			want += 2
		}
		if g.PlanScope {
			want += 2
		}
		if g.EscalationExecutor {
			want += 2
		}
		if len(g.DispatchClaim) > 0 {
			want++
		}
		want += len(g.LabeledOwnership) + len(g.FieldOwnership)
	}
	rows := machineGrantRows()
	if len(rows) != want {
		t.Fatalf("machine rows = %d, want %d", len(rows), want)
	}
	for _, r := range rows {
		g, ok := machineCapabilityGrants[r.Principal.Name]
		if !ok || r.Principal.Kind != grantKindMachine {
			t.Fatalf("row %s names no machine role in machineCapabilityGrants", r.ID)
		}
		suffix := r.ID[strings.Index(r.ID, "/")+1:]
		var covered bool
		switch {
		case suffix == "ownership":
			covered = slices.Equal(r.Types.Only, sortedTypes(g.Ownership))
		case suffix == "reclaim":
			covered = slices.Equal(r.Types.Only, sortedTypes(g.ReclaimOwnership)) && r.Condition == "assignee_gone_agent"
		case suffix == "reclaim-reaped":
			covered = slices.Equal(r.Types.Only, sortedTypes(g.ReclaimOwnership)) && r.Condition == "assignee_reaped_agent"
		case suffix == "plan-scope", suffix == "plan-gardener":
			covered = g.PlanScope
		case suffix == "approved-targets", suffix == "approved-close":
			covered = g.EscalationExecutor && r.Condition == "listed_by_claimed_approved_escalation"
		case suffix == "dispatch-claim":
			covered = slices.Equal(r.Types.Only, sortedTypes(g.DispatchClaim)) && r.Condition == "dispatched_agent_claims_unassigned"
		case strings.HasPrefix(suffix, "labeled/"):
			covered = slices.ContainsFunc(g.LabeledOwnership, func(l machineLabeledOwnership) bool {
				return l.Label == r.Label && slices.Equal(l.Actions, r.Actions) && r.Types.Only[0] == l.BeadType
			})
		case strings.HasPrefix(suffix, "fields/"):
			covered = slices.ContainsFunc(g.FieldOwnership, func(f machineFieldOwnership) bool {
				return slices.Equal(f.Fields, r.Fields) && r.Types.Only[0] == f.BeadType
			})
		}
		if !covered {
			t.Errorf("row %s does not match %s's capability", r.ID, r.Principal.Name)
		}
	}
}

func grantEdgeCtx(subject string, v2 bool, roles []string) context.Context {
	ctx := withAuthMethod(context.Background(), authMethodEdge)
	if v2 {
		ctx = withAuthMethod(context.Background(), authMethodOIDC)
	}
	ctx = WithRoles(ctx, roles)
	return WithAuthIdentity(ctx, AuthIdentity{Type: "edge", Subject: subject})
}

func grantStaticCtx(actor string) context.Context {
	return WithAuthIdentity(withAuthMethod(context.Background(), authMethodStatic), AuthIdentity{Type: "static", Subject: actor})
}

func grantRunnerCtx(owner string) context.Context {
	return WithAuthIdentity(withAuthMethod(context.Background(), authMethodRunner), AuthIdentity{Type: runnerIdentityType, Subject: owner})
}

func grantLandCtx(land string) context.Context {
	ctx := WithRoles(withAuthMethod(context.Background(), authMethodLand), landPrincipalRoles)
	return WithAuthIdentity(ctx, AuthIdentity{Type: "land", Subject: landSubjectPrefix + land})
}

// checkOwnershipRow asserts which grant row decides one write and that
// authorizeOwnership enforces it. wantGrant "" means the write is denied.
func checkOwnershipRow(t *testing.T, srv *BeadsServer, ctx context.Context, action string, bead *model.Bead, wantGrant string) {
	t.Helper()
	adm, ok := srv.evaluateOwnershipGrants(ctx, action, bead)
	if adm.grant != wantGrant || ok != (wantGrant != "") {
		t.Errorf("grant = %q/%v, want %q", adm.grant, ok, wantGrant)
	}
	if d := srv.authorizeOwnership(ctx, action, bead); (d == nil) != ok {
		t.Errorf("authorizeOwnership admit=%v, table admit=%v (%v)", d == nil, ok, d)
	}
}

func foreignBead(id string, typ model.BeadType, labels ...string) *model.Bead {
	return &model.Bead{ID: id, Type: typ, Kind: model.KindIssue, Status: model.StatusOpen,
		CreatedBy: "someone-else@example.com", Labels: labels}
}

// One case per admitting branch of authorizeOwnership, plus the denial.
func TestOwnershipGrants_InventoryRows(t *testing.T) {
	srv, _, _ := testCtx(t)
	task := foreignBead("gd-t", model.TypeTask)
	esc := foreignBead("gd-e", model.TypeEscalation)
	dec := foreignBead("gd-d", model.TypeDecision)

	cases := []struct {
		name   string
		ctx    context.Context
		action string
		bead   *model.Bead
		wantID string
	}{
		{"2 admin oidc", oidcCtx("admin@example.com", []string{RoleAdmin}), "delete", task, "admin-oidc"},
		{"2 admin edge v2", grantEdgeCtx("admin@example.com", true, []string{RoleAdmin}), "delete", task, "admin-edge"},
		// 3: the shared static token is retired (gd-ke6PC7bRto): its identity,
		// were one to reach the gate, takes no grant.
		{"3 retired static token", grantStaticCtx("static"), "delete", task, ""},
		{"4a machine typed", machineCtx("gardenio-controller", "gardenio-machine-trigger-ops"), "close", task, "machine:trigger-ops/ownership"},
		{"4b machine labelled", machineCtx("gardenio-dashboard-sync", "gardenio-machine-dashboard-sync"), "update", foreignBead("gd-doc", typeDoc, "grafana:dashboard-model"), "machine:dashboard-sync/labeled/0"},
		{"4b machine labelled, not close", machineCtx("gardenio-dashboard-sync", "gardenio-machine-dashboard-sync"), "close", foreignBead("gd-doc", typeDoc, "grafana:dashboard-model"), ""},
		{"6 privileged fleet", WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "bridge:gitlab", Privileged: true}), "delete", task, "fleet-privileged"},
		{"7 dm carve-out", fleetCtx("bridge:slack"), "delete", dmBead("gd-dm", "crew-a"), "fleet-dm"},
		{"8 escalation, agent token", agentFleetCtx("gd-a1", "crew-a"), "close", esc, "queue-escalation-fleet"},
		{"8 escalation, operator", oidcCtx("op@example.com", []string{RoleOperator}), "close", esc, "queue-escalation-oidc"},
		{"8 escalation, viewer", oidcCtx("viewer@example.com", []string{RoleViewer}), "close", esc, ""},
		{"8 escalation, edge v1", grantEdgeCtx("op@example.com", false, nil), "update", esc, "queue-escalation-edge-v1"},
		{"8 escalation, edge v2 operator", grantEdgeCtx("op@example.com", true, []string{RoleOperator}), "update", esc, "queue-escalation-edge-v2"},
		{"8 escalation, machine", machineCtx("gardenio-authz-canary"), "update", esc, "queue-escalation-machine"},
		{"8 escalation, runner", grantRunnerCtx("op@example.com"), "update", esc, "queue-escalation-runner"},
		{"8 escalation, land", grantLandCtx("plot-a"), "update", esc, ""},
		{"8 decision, operator", oidcCtx("op@example.com", []string{RoleOperator}), "close", dec, "queue-decision-oidc"},
		{"8 decision, bridge", fleetCtx("bridge:slack"), "close", dec, "queue-decision-fleet-bridge"},
		{"8 decision, machine", machineCtx("gardenio-authz-canary"), "close", dec, "queue-decision-machine"},
		{"8 decision, edge v1", grantEdgeCtx("op@example.com", false, nil), "close", dec, "queue-decision-edge-v1"},
		{"8 decision, agent token", agentFleetCtx("gd-a1", "crew-a"), "close", dec, ""},
		{"9 no identity", WithAuthIdentity(withAuthMethod(context.Background(), authMethodNone), AuthIdentity{Type: "none"}), "delete", task, "anonymous"},
		{"10 legacy unowned", oidcCtx("bob@example.com", nil), "update", &model.Bead{ID: "gd-l", Type: model.TypeTask, Kind: model.KindIssue}, "legacy-unowned"},
		{"11 owner", oidcCtx("someone-else@example.com", nil), "delete", task, "owner"},
		{"11 assignee", oidcCtx("bob@example.com", nil), "close", &model.Bead{ID: "gd-a", Type: model.TypeTask, Kind: model.KindIssue, CreatedBy: "x", Assignee: "bob@example.com"}, "owner"},
		{"12 bridge rename", fleetCtx("bridge:jira"), "update", &model.Bead{ID: "gd-j", Type: model.TypeTask, Kind: model.KindIssue, CreatedBy: "jira-bridge"}, "bridge-legacy-creator"},
		{"13 operated_by", oidcCtx("op@example.com", []string{RoleOperator}), "delete", &model.Bead{ID: "gd-o", Type: model.TypeTask, Kind: model.KindIssue, CreatedBy: "crew-a", OperatedBy: "op@example.com"}, "operator-of-bead"},
		{"19 denied", oidcCtx("bob@example.com", []string{RoleOperator}), "update", task, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			checkOwnershipRow(t, srv, c.ctx, c.action, c.bead, c.wantID)
		})
	}

	t.Run("0 and 1 stay outside the table", func(t *testing.T) {
		cfg := &model.Bead{ID: "gd-c", Type: "agent", Kind: model.KindConfig, CreatedBy: "x"}
		if d := srv.authorizeOwnership(oidcCtx("bob@example.com", nil), "update", cfg); d != nil {
			t.Fatalf("config kind: %v", d)
		}
		if _, ok := srv.evaluateOwnershipGrants(oidcCtx("bob@example.com", nil), "update", cfg); ok {
			t.Fatal("the table never sees a config-kind bead")
		}
	})

	t.Run("5 machine reclaim", func(t *testing.T) {
		srv, _ := reclaimServer(t)
		ctrl := machineCtx("gardenio-controller", "gardenio-machine-controller-ownership")
		checkOwnershipRow(t, srv, ctrl, "update", reclaimBead("gd-b", model.TypeBug, "dead-crew"), "machine:controller-ownership/reclaim")
		checkOwnershipRow(t, srv, ctrl, "update", reclaimBead("gd-b", model.TypeBug, "live-crew"), "")
		checkOwnershipRow(t, srv, ctrl, "close", reclaimBead("gd-b", model.TypeBug, "dead-crew"), "")
		checkOwnershipRow(t, srv, ctrl, "update", reapedBead("gd-b", model.TypeBug, "dead-crew"), "machine:controller-ownership/reclaim-reaped")
		checkOwnershipRow(t, srv, ctrl, "update", reapedBead("gd-b", model.TypeBug, "live-crew"), "")
		checkOwnershipRow(t, srv, ctrl, "close", reapedBead("gd-b", model.TypeBug, "dead-crew"), "")
	})

	t.Run("14 role bypass", func(t *testing.T) {
		srv := newChainWalkServer(
			&model.Bead{ID: "gd-rv", Type: "agent", Fields: agentFields("gardenio", "reviewer")},
			roleBeadWith("gd-r1", "reviewer", bypassRoleFields, model.StatusOpen),
		)
		ctx := agentFleetCtx("gd-rv", "merge-desk")
		checkOwnershipRow(t, srv, ctx, "close", task, "role:reviewer/0")
		checkOwnershipRow(t, srv, ctx, "delete", task, "")
		checkOwnershipRow(t, srv, ctx, "update", dec, "")
	})

	t.Run("15 role labelled", func(t *testing.T) {
		srv, ctx := herderGrantServer(herderRowsGrant)
		thread := foreignBead("gd-w", model.TypeTask, "project:gardenio", "thread")
		checkOwnershipRow(t, srv, ctx, "close", thread, "role:herder/0")
		checkOwnershipRow(t, srv, ctx, "update", task, "")
		checkOwnershipRow(t, srv, ctx, "delete", thread, "")
	})

	t.Run("16 steward notes", func(t *testing.T) {
		lf := newLandFixture(t)
		srv := &BeadsServer{store: lf.ms}
		notes := &model.Bead{ID: "gd-n", Type: typeDoc, Kind: "data", Title: "steward-notes:plot-a", Labels: []string{"project:plot-a"}, CreatedBy: "controller"}
		checkOwnershipRow(t, srv, lf.steward, "update", notes, "steward-notes")
		checkOwnershipRow(t, srv, lf.steward, "close", notes, "")
		checkOwnershipRow(t, srv, lf.crew, "update", notes, "")
	})

	t.Run("17 plan scope", func(t *testing.T) {
		srv, ms, _ := testCtx(t)
		planScopeFixture(ms)
		step, _ := ms.GetBead(context.Background(), "gd-step")
		ctx := planAgent(ms, "gd-ag1", "crew-plan-owner", "crew", nil)
		checkOwnershipRow(t, srv, ctx, "close", step, "plan-scope")
		checkOwnershipRow(t, srv, ctx, "delete", step, "")
		other, _ := ms.GetBead(context.Background(), "gd-step2")
		checkOwnershipRow(t, srv, ctx, "update", other, "")

		reactor := machineCtx("gardenio-beads-viewer", "gardenio-machine-plan-reactor")
		checkOwnershipRow(t, srv, reactor, "update", step, "machine:plan-reactor/plan-scope")
		trig, _ := ms.GetBead(context.Background(), "gd-trig")
		checkOwnershipRow(t, srv, reactor, "update", trig, "machine:plan-reactor/plan-gardener")
		checkOwnershipRow(t, srv, reactor, "close", trig, "")
		plan, _ := ms.GetBead(context.Background(), "gd-plan")
		// Finding 9: the reactor's typed epic ownership admits first.
		checkOwnershipRow(t, srv, reactor, "delete", plan, "machine:plan-reactor/ownership")
	})

	t.Run("18 escalation close", func(t *testing.T) {
		srv := newChainWalkServer(closeGrantAgent("gd-a1", closeGrantEsc), closeGrantEscalation(nil))
		ctx := agentFleetCtx("gd-a1", closeGrantExecutor)
		checkOwnershipRow(t, srv, ctx, "close", orphanBead("gd-t1", model.TypeTask), "escalation-close")
		checkOwnershipRow(t, srv, ctx, "update", orphanBead("gd-t1", model.TypeTask), "")
		checkOwnershipRow(t, srv, ctx, "close", orphanBead("gd-t9", model.TypeTask), "")
	})
}

// An admit counts the admitting row.
func TestOwnershipGrants_CountsAdmits(t *testing.T) {
	srv, _, _ := testCtx(t)
	ctx := oidcCtx("alice@example.com", nil)
	bead := &model.Bead{ID: "gd-a", Type: model.TypeTask, Kind: model.KindIssue, CreatedBy: "alice@example.com", UpdatedAt: time.Now()}
	counter := authzOwnershipAdmitsTotal.WithLabelValues("owner", "human", "close", "task")
	before := testutil.ToFloat64(counter)
	if d := srv.authorizeOwnership(ctx, "close", bead); d != nil {
		t.Fatal(d)
	}
	if got := testutil.ToFloat64(counter) - before; got != 1 {
		t.Fatalf("admits{owner,human,close,task} += %v, want 1", got)
	}
}

// ownershipGrantGolden is one row of testdata/ownership_grants.golden.json.
type ownershipGrantGolden struct {
	ID         string   `json:"id"`
	Principal  string   `json:"principal"`
	MinRole    string   `json:"min_role,omitempty"`
	Actions    []string `json:"actions"`
	Types      string   `json:"types"`
	Label      string   `json:"label,omitempty"`
	Fields     []string `json:"fields,omitempty"`
	Condition  string   `json:"condition,omitempty"`
	Privileged bool     `json:"privileged"`
	Origin     string   `json:"origin"`
}

// describeGrantTypes renders a type set: "all", "all except decision", or
// "epic,task".
func describeGrantTypes(t grantTypes) string {
	if t.All {
		if len(t.Except) == 0 {
			return "all"
		}
		ex := make([]string, 0, len(t.Except))
		for _, e := range t.Except {
			ex = append(ex, string(e))
		}
		return "all except " + strings.Join(ex, ",")
	}
	only := make([]string, 0, len(t.Only))
	for _, o := range t.Only {
		only = append(only, string(o))
	}
	return strings.Join(only, ",")
}

// marshalOwnershipGrantsGolden renders the effective table (code rows and
// machine-derived rows, sorted by id, with the computed privilege). Role rows
// are data on role beads and are not in it.
func marshalOwnershipGrantsGolden() ([]byte, error) {
	rows := ownershipGrantTable()
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	out := make([]ownershipGrantGolden, 0, len(rows))
	for i := range rows {
		g := &rows[i]
		principal := g.Principal.Kind
		if g.Principal.Name != "" {
			principal += ":" + g.Principal.Name
		}
		out = append(out, ownershipGrantGolden{ID: g.ID, Principal: principal, MinRole: g.Principal.MinRole, Actions: g.Actions,
			Types: describeGrantTypes(g.Types), Label: g.Label, Fields: g.Fields, Condition: g.Condition,
			Privileged: ownershipGrantPrivileged(g), Origin: g.Origin})
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// validateOwnershipGrantTable checks the invariants every row must hold: a
// unique id, at least one action, a known principal kind and condition, a
// Fields list only on an update-only row, Except only with All.
func validateOwnershipGrantTable(rows []ownershipGrant) error {
	known := map[string]bool{grantKindAny: true, grantKindNone: true, grantKindOIDC: true, grantKindEdgeV2: true,
		grantKindEdgeV1: true, grantKindFleet: true, grantKindFleetBridge: true,
		grantKindFleetPrivileged: true, grantKindAgent: true, grantKindMachine: true, grantKindRunner: true}
	seen := map[string]bool{}
	for i := range rows {
		g := &rows[i]
		switch {
		case g.ID == "" || seen[g.ID]:
			return fmt.Errorf("row %d: id %q is empty or repeated", i, g.ID)
		case len(g.Actions) == 0:
			return fmt.Errorf("row %s: no actions", g.ID)
		case !known[g.Principal.Kind]:
			return fmt.Errorf("row %s: unknown principal kind %q", g.ID, g.Principal.Kind)
		case len(g.Fields) > 0 && !slices.Equal(g.Actions, []string{"update"}):
			return fmt.Errorf("row %s: a field list needs actions [update]", g.ID)
		case !g.Types.All && len(g.Types.Except) > 0:
			return fmt.Errorf("row %s: Except without All", g.ID)
		case !g.Types.All && len(g.Types.Only) == 0:
			return fmt.Errorf("row %s: no types", g.ID)
		}
		if g.Condition != "" {
			if _, ok := ownershipConditions[g.Condition]; !ok {
				return fmt.Errorf("row %s: unknown condition %q", g.ID, g.Condition)
			}
		}
		seen[g.ID] = true
	}
	return nil
}
