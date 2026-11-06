package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"gardenio/kbeads/internal/model"
)

// A role's write reach is rows in ownership_grants on its role bead (step 5 of
// plan gd-fRmqrpHIwk, gd-8IcxHwgGF5): giving a role reach is config, not code.

// bypassRows is the all-types ownership_grants value the coordinating roles
// (reviewer, gardener, captains) hold: privileged by ownershipGrantPrivileged.
const bypassRows = `[{"types":["*"],"except":["decision"],"actions":["update","close"]}]`

// bypassRoleFields is a role bead's fields holding bypassRows.
const bypassRoleFields = `{"ownership_grants":` + bypassRows + `}`

// herderRowsGrant is the herder's narrow grant: update and close on
// thread-labelled tasks.
const herderRowsGrant = `{"ownership_grants":[{"types":["task"],"label":"thread","actions":["update","close"]}]}`

func herderGrantServer(roleFields string) (*BeadsServer, context.Context) {
	srv := newChainWalkServer(
		&model.Bead{ID: "gd-h1", Type: "agent", Fields: agentFields("gardenio", "herder")},
		roleBeadWith("gd-r1", "herder", roleFields, model.StatusOpen),
	)
	return srv, agentFleetCtx("gd-h1", "herder-a")
}

func TestParseRoleOwnershipGrants(t *testing.T) {
	for _, ok := range []string{
		``, `null`, `[]`, bypassRows,
		`[{"types":["task"],"label":"thread","actions":["update","close"]}]`,
		`[{"types":["epic","task"],"actions":["update"],"condition":"in_callers_plan"}]`,
		`[{"types":["doc"],"label":"x","actions":["close"],"privileged":true}]`,
	} {
		if _, err := parseRoleOwnershipGrants(ok); err != nil {
			t.Errorf("%s: refused: %v", ok, err)
		}
	}
	for name, bad := range map[string]string{
		"not a list":            `{"types":["task"]}`,
		"unknown key":           `[{"types":["task"],"actions":["update"],"fields":["x"]}]`,
		"no types":              `[{"actions":["update"]}]`,
		"no actions":            `[{"types":["task"]}]`,
		"delete":                `[{"types":["task"],"actions":["delete"]}]`,
		"hook-emit":             `[{"types":["task"],"actions":["hook-emit"]}]`,
		"decision":              `[{"types":["decision"],"actions":["update"]}]`,
		"all without except":    `[{"types":["*"],"actions":["update"]}]`,
		"all, except no dec":    `[{"types":["*"],"except":["task"],"actions":["update"]}]`,
		"star with others":      `[{"types":["*","task"],"except":["decision"],"actions":["update"]}]`,
		"except without all":    `[{"types":["task"],"except":["decision"],"actions":["update"]}]`,
		"empty type":            `[{"types":[""],"actions":["update"]}]`,
		"unknown condition":     `[{"types":["task"],"actions":["update"],"condition":"always"}]`,
		"bead-only condition":   `[{"types":["bug"],"actions":["update"],"condition":"assignee_gone_agent"}]`,
		"label with spaces":     `[{"types":["task"],"label":" thread","actions":["update"]}]`,
		"two values":            `[] []`,
		"over maxRoleGrantRows": "[" + strings.Repeat(`{"types":["task"],"label":"l","actions":["update"]},`, maxRoleGrantRows) + `{"types":["task"],"label":"l","actions":["update"]}]`,
	} {
		if _, err := parseRoleOwnershipGrants(bad); err == nil {
			t.Errorf("%s: %s must be refused", name, bad)
		}
	}
}

// ownership_grants is a role's only source of rows. The fields it replaced
// (bypass_ownership, labeled_ownership; gd-OsLTduO6u9) are not read.
func TestRoleOwnershipGrants_Rows(t *testing.T) {
	rows, err := roleOwnershipGrants(roleBeadWith("gd-r", "reviewer", `{"ownership_grants":[{"types":["task"],"label":"merge","actions":["close"]}]}`, model.StatusOpen))
	if err != nil || len(rows) != 1 || rows[0].ID != "role:reviewer/0" || rows[0].Label != "merge" || rows[0].Origin != "gd-r" {
		t.Fatalf("rows = %+v, %v; want the one ownership_grants row", rows, err)
	}
	for name, fields := range map[string]string{
		"unset":      `{}`,
		"null":       `{"ownership_grants":null}`,
		"empty":      `{"ownership_grants":[]}`,
		"old bypass": `{"bypass_ownership":true}`,
		"old labels": `{"labeled_ownership":[{"type":"task","label":"thread","actions":["update","close"]}]}`,
	} {
		if rows, err := roleOwnershipGrants(roleBeadWith("gd-r", "reviewer", fields, model.StatusOpen)); err != nil || len(rows) != 0 {
			t.Errorf("%s: want no row, got %+v %v", name, rows, err)
		}
	}
	if rows, err := roleOwnershipGrants(roleBeadWith("gd-r", "reviewer", `{"ownership_grants":[{"types":["decision"],"actions":["update"]}]}`, model.StatusOpen)); err == nil || len(rows) != 0 {
		t.Fatalf("a stored invalid value grants nothing, got %+v %v", rows, err)
	}
}

// The gate enforces a role's ownership_grants rows.
func TestAuthorizeOwnership_RoleOwnershipGrantsRows(t *testing.T) {
	srv, ctx := herderGrantServer(herderRowsGrant)
	thread := foreignBead("gd-w", model.TypeTask, "project:gardenio", "thread")
	for _, action := range []string{"update", "close"} {
		checkOwnershipRow(t, srv, ctx, action, thread, "role:herder/0")
	}
	checkOwnershipRow(t, srv, ctx, "delete", thread, "")
	checkOwnershipRow(t, srv, ctx, "update", foreignBead("gd-t", model.TypeTask, "project:gardenio"), "")

	srv, ctx = herderGrantServer(bypassRoleFields)
	checkOwnershipRow(t, srv, ctx, "close", foreignBead("gd-t", model.TypeTask), "role:herder/0")
	checkOwnershipRow(t, srv, ctx, "update", foreignBead("gd-d", model.TypeDecision), "")

	// A closed role bead grants nothing.
	srv = newChainWalkServer(
		&model.Bead{ID: "gd-h1", Type: "agent", Fields: agentFields("gardenio", "herder")},
		roleBeadWith("gd-r1", "herder", herderRowsGrant, model.StatusClosed),
	)
	checkOwnershipRow(t, srv, agentFleetCtx("gd-h1", "herder-a"), "close", thread, "")
}

// A role row narrowed by a caller-bound condition admits only when it holds.
func TestAuthorizeOwnership_RoleRowCondition(t *testing.T) {
	srv, ctx := herderGrantServer(`{"ownership_grants":[{"types":["task"],"actions":["update"],"condition":"caller_operates_bead"}]}`)
	mine := foreignBead("gd-o", model.TypeTask)
	mine.OperatedBy = "herder-a"
	checkOwnershipRow(t, srv, ctx, "update", mine, "role:herder/0")
	checkOwnershipRow(t, srv, ctx, "update", foreignBead("gd-t", model.TypeTask), "")
}

func TestRoleIsPrivileged_OwnershipGrantsRows(t *testing.T) {
	ms := newMockStore()
	for name, fields := range map[string]string{
		"narrow":   herderRowsGrant,
		"all":      bypassRoleFields,
		"typewide": `{"ownership_grants":[{"types":["task"],"actions":["update"]}]}`,
		"raised":   `{"ownership_grants":[{"types":["task"],"label":"thread","actions":["close"],"privileged":true}]}`,
		"cond":     `{"ownership_grants":[{"types":["task"],"actions":["update"],"condition":"in_callers_plan"}]}`,
		"invalid":  `{"ownership_grants":[{"types":["decision"],"actions":["update"]}]}`,
		"empty":    `{"ownership_grants":[]}`,
	} {
		ms.beads["gd-role-"+name] = roleBeadWith("gd-role-"+name, name, fields, model.StatusOpen)
	}
	for role, want := range map[string]bool{
		"narrow": false, "all": true, "typewide": true, "raised": true, "cond": false, "invalid": true, "empty": false,
	} {
		got, err := roleIsPrivileged(context.Background(), ms, role)
		if err != nil || got != want {
			t.Errorf("%s: privileged = %v (%v), want %v", role, got, err, want)
		}
	}
}

// The lands-gardener role may hold no row at all, narrow or not.
func TestLandsGardenerRole_RefusesOwnershipGrants(t *testing.T) {
	for name, extra := range map[string]string{
		"narrow row": `,"ownership_grants":[{"types":["task"],"label":"thread","actions":["update"]}]`,
		"invalid":    `,"ownership_grants":"nope"`,
		"all types":  `,"ownership_grants":` + bypassRows,
	} {
		ms := newMockStore()
		ms.beads["gd-lg"] = roleBeadWith("gd-lg", landsGardenerRole, `{"capability_tiers":["lands-tenant"]`+extra+`}`, model.StatusOpen)
		if d := landsGardenerRoleDenial(context.Background(), ms, "create"); d == nil {
			t.Errorf("%s: a lands-gardener role holding a grant must refuse the spawn", name)
		}
	}
	ms := newMockStore()
	ms.beads["gd-lg"] = roleBeadWith("gd-lg", landsGardenerRole, `{"capability_tiers":["lands-tenant"],"ownership_grants":[]}`, model.StatusOpen)
	if d := landsGardenerRoleDenial(context.Background(), ms, "create"); d != nil {
		t.Fatalf("control: an empty list holds no row, got %v", d)
	}
}

// The denial keeps naming the bead's owners and the caller (ka-0000005214),
// and lists the rows that would have admitted the write for someone else.
func TestOwnershipDenial_ListsAllowingGrants(t *testing.T) {
	ms := newMockStore()
	docRows := `{"ownership_grants":[{"types":["doc"],"label":"thread","actions":["update","close"]}]}`
	ms.beads["gd-r1"] = roleBeadWith("gd-r1", "herder", docRows, model.StatusOpen)
	ms.beads["gd-r2"] = roleBeadWith("gd-r2", "closed-role", docRows, model.StatusClosed)
	srv := &BeadsServer{store: ms}
	thread := foreignBead("gd-w", typeDoc, "thread")
	d := srv.authorizeOwnership(oidcCtx("bob@example.com", nil), "close", thread)
	if d == nil {
		t.Fatal("bob does not own the doc")
	}
	if d.CallerSubject != "bob@example.com" || d.ResourceCreatedBy != "someone-else@example.com" {
		t.Fatalf("the denial must still name the caller and the owner: %+v", d)
	}
	want := []string{"plan-scope", "role:herder/0"}
	var got []string
	for _, g := range d.AllowingGrants {
		got = append(got, g.Grant)
	}
	if strings.Join(got, ",") != "machine:plan-reactor/plan-scope,"+strings.Join(want, ",") {
		t.Fatalf("allowing grants = %v", got)
	}
	if d.AllowingGrants[2].Principal != "agent:herder" || d.AllowingGrants[1].Condition != "in_callers_plan" {
		t.Fatalf("allowing grants = %+v", d.AllowingGrants)
	}
	if !strings.Contains(d.Message, "would be allowed for: ") || !strings.Contains(d.Message, "agent role herder (row role:herder/0)") ||
		!strings.Contains(d.Message, "agent when in_callers_plan (row plan-scope)") {
		t.Fatalf("message = %s", d.Message)
	}
	b, _ := json.Marshal(d)
	if !strings.Contains(string(b), `"allowing_grants":[`) {
		t.Fatalf("the 403 body carries allowing_grants: %s", b)
	}

	// Rows that match every bead (admin, the base rule, ...) say nothing about
	// this one, so a bead no narrow row reaches lists none.
	other := foreignBead("gd-x", "mail")
	if d := srv.authorizeOwnership(oidcCtx("bob@example.com", nil), "update", other); d == nil || len(d.AllowingGrants) != 0 || strings.Contains(d.Message, "would be allowed") {
		t.Fatalf("want no allowing grants, got %+v", d)
	}
}

// The records the old branches left are left by the rows that replace them.
func TestAuthorizeOwnership_AdmissionRecords(t *testing.T) {
	srv, _, _ := testCtx(t)
	counter := authzLegacyFallthroughTotal.WithLabelValues("update", "task")
	before := testutil.ToFloat64(counter)
	if d := srv.authorizeOwnership(oidcCtx("bob@example.com", nil), "update", &model.Bead{ID: "gd-l", Type: model.TypeTask, Kind: model.KindIssue}); d != nil {
		t.Fatal(d)
	}
	if got := testutil.ToFloat64(counter) - before; got != 1 {
		t.Fatalf("legacy fall-through += %v, want 1", got)
	}
	// Admitted by another row first, an unowned bead is not counted.
	if d := srv.authorizeOwnership(oidcCtx("admin@example.com", []string{RoleAdmin}), "update", &model.Bead{ID: "gd-l", Type: model.TypeTask, Kind: model.KindIssue}); d != nil {
		t.Fatal(d)
	}
	if got := testutil.ToFloat64(counter) - before; got != 1 {
		t.Fatalf("legacy fall-through += %v after an admin write, want 1", got)
	}
}
