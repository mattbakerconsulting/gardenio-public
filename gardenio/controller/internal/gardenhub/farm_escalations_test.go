package gardenhub

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

// TestFarmWaitEscalationFamily pins the family table: each listed operation
// type, the prefixes, the exact names a prefix must not swallow, and other for
// anything else, never "".
func TestFarmWaitEscalationFamily(t *testing.T) {
	for op, want := range map[string]string{
		"gitlab_mr_merge": EscMerge, "github_pr_merge": EscMerge, "gitlab_merge": EscMerge,
		"helm_deploy": EscDeploy, "deploy": EscDeploy,
		"helm_chart_edit": EscConfig, "helm_chart_change": EscConfig,
		"trigger_prompt_install": EscPrompt, "trigger_prompt_reseal": EscPrompt, "trigger_prompt_amend": EscPrompt,
		"trigger_reseal": EscPrompt, "trigger_prompt_install_reseal": EscPrompt,
		"kbeads_admin_write": EscBeads, "kbeads_label_fix": EscBeads, "beads_admin_write": EscBeads,
		"aws_iam": EscAccess, "iam_grant": EscAccess, "k8s_rbac": EscAccess, "github_token_scope": EscAccess,
		"kbeads_project_access": EscAccess, "kbeads_authz_change": EscAccess,
		"infrastructure": EscInfra, "terraform_apply": EscInfra, "github_workflow_dispatch": EscInfra,
		"db_readonly_query": EscRead, "cluster_read": EscRead,
		" Helm_Deploy ": EscDeploy, // read trimmed and case-blind
		"":              EscOther, "manual_operator_action": EscOther, "capability_grant": EscOther,
	} {
		if got := EscalationFamily(op); got != want {
			t.Errorf("EscalationFamily(%q) = %q, want %q", op, got, want)
		}
	}
}

// TestFarmTaskEscalationSprite: an escalation's own FarmTask carries the op,
// family and wait state its FarmWait does, so its plot draws the Escalations cell's sprite
// (web/art.js, gd-UDx80F47JN); a closed one keeps its family but no wait, and
// any other type carries none of the three.
func TestFarmTaskEscalationSprite(t *testing.T) {
	f := &Farm{}
	esc := func(status string, fields map[string]string) FarmTask {
		b := fb("gd-E", "escalation", status, "", 1)
		b.Fields = fields
		return f.task(b, nil)
	}
	for _, c := range []struct {
		name, status string
		fields       map[string]string
		fam, wait    string
	}{
		{"pending", "open", map[string]string{"operation_type": "github_pr_merge"}, EscMerge, WaitPending},
		{"approved", "open", map[string]string{"operation_type": "helm_deploy", "approved_at": "2026-11-02T07:00:00Z"}, EscDeploy, WaitApproved},
		{"claimed", "in_progress", map[string]string{"operation_type": "k8s_rbac"}, EscAccess, WaitApproved},
		{"rejected", "open", map[string]string{"operation_type": "cluster_read", "result_status": "failure", "result": `{"error":"rejected"}`}, EscRead, WaitRejected},
		{"failed", "open", map[string]string{"operation_type": "terraform_apply", "result_status": "failure"}, EscInfra, WaitFailed},
		{"no op", "open", map[string]string{}, EscOther, WaitPending},
		{"closed", "closed", map[string]string{"operation_type": "github_pr_merge"}, EscMerge, ""},
	} {
		ft := esc(c.status, c.fields)
		if ft.Family != c.fam || ft.Wait != c.wait || ft.Op != c.fields["operation_type"] {
			t.Errorf("%s: op %q family %q wait %q, want %q %q %q", c.name, ft.Op, ft.Family, ft.Wait, c.fields["operation_type"], c.fam, c.wait)
		}
	}
	task := fb("gd-T", "task", "open", "", 1)
	task.Fields["operation_type"] = "github_pr_merge"
	if ft := f.task(task, nil); ft.Op != "" || ft.Family != "" || ft.Wait != "" {
		t.Errorf("a task carries an escalation's fields: %+v", ft)
	}
}

// TestFarmWaitEscalations is the read model's escalations (gd-VyP9DLgvih):
// every open escalation, in a plan or not, with its op verbatim, its family
// and the gardener running it, in the plan's waiting[] and in the fleet-wide
// escalations blob; a closed one in neither, a decision in neither's fields.
func TestFarmWaitEscalations(t *testing.T) {
	f := newTestFarm(escalationWorld())
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := decodeSnap(t, f)
	p := findPlan(s, "gd-planY")
	if p == nil || p.OpenEscalations != len(escalationOps)+1 {
		t.Fatalf("plan Y = %+v; want %d open escalations", p, len(escalationOps)+1)
	}
	if w := findWait(p.Waiting, "gd-Ycross"); w == nil || w.Executor != "zeta-runner" {
		t.Errorf("cross in plan Y's waiting = %+v; want its zeta executor named on the whole farm", w)
	}
	checkEscalationFamilies(t, p.Waiting, s.Escalations.Open)
	if w := findWait(p.Waiting, "gd-Ye0"); w.State != WaitApproved || w.Human {
		t.Errorf("the run merge = %+v; want approved, not a human wait", w)
	}
	checkFleetEscalations(t, s.Escalations)
	checkEscalationWire(t, f)
	// A decision carries none of the escalation fields.
	if dw := findWait(findPlan(decodeSnap(t, newWaitFarm(t)), "gd-planW").Waiting, "gd-Wdu"); dw == nil || dw.Op != "" || dw.Family != "" || dw.Project != "" {
		t.Errorf("decision wait = %+v; want no op, family or project", dw)
	}
}

// checkEscalationFamilies: each of the plan's escalations, in its waiting[] and in the fleet-wide list, says its op,
// family and project, and only the run merge names an executor.
func checkEscalationFamilies(t *testing.T, waiting, fleet []FarmWait) {
	t.Helper()
	for i, o := range escalationOps {
		id := "gd-Ye" + string(rune('0'+i))
		for where, ws := range map[string][]FarmWait{"waiting": waiting, "escalations": fleet} {
			w := findWait(ws, id)
			if w == nil || w.Op != o.op || w.Family != o.family || w.Project != "gardenio" || w.Kind != "escalation" {
				t.Errorf("%s %s = %+v; want op %s, family %s, project gardenio", where, id, w, o.op, o.family)
				continue
			}
			if wantExec := map[bool]string{true: "escalation-gd-ye0"}[i == 0]; w.Executor != wantExec {
				t.Errorf("%s %s: executor = %q, want %q", where, id, w.Executor, wantExec)
			}
		}
	}
}

// checkFleetEscalations: the fleet-wide list holds every open escalation, planned or not, projected or not, none
// closed, in the waits' order.
func checkFleetEscalations(t *testing.T, es FarmEscalations) {
	t.Helper()
	if es.Total != len(escalationOps)+4 || len(es.Open) != es.Total {
		t.Fatalf("escalations: total %d, %d listed; want %d", es.Total, len(es.Open), len(escalationOps)+4)
	}
	if bp := es.ByProject; len(bp) != 3 || bp["gardenio"] != len(escalationOps)+2 || bp["zeta"] != 1 || bp[""] != 1 {
		t.Errorf("escalations by project = %v; want gardenio %d, zeta 1, unprojected 1", bp, len(escalationOps)+2)
	}
	if findWait(es.Open, "gd-Yclosed") != nil {
		t.Error("a closed escalation is in the open list")
	}
	if w := findWait(es.Open, "gd-Yloose"); w == nil || w.Family != EscMerge || w.Project != "gardenio" || w.State != WaitPending || w.WaitsOn != WaitsOnDesk {
		t.Errorf("an escalation with no plan = %+v; want listed, merge, pending with the desk", w)
	}
	if w := findWait(es.Open, "gd-Ybare"); w == nil || w.Project != "" || w.Family != EscRead {
		t.Errorf("an unprojected escalation = %+v; want listed with no project", w)
	}
	if w := findWait(es.Open, "gd-Ycross"); w == nil || w.Executor != "zeta-runner" {
		t.Errorf("cross = %+v; want its zeta executor named on the whole farm", w)
	}
	for i := 1; i < len(es.Open); i++ {
		if waitLess(es.Open[i], es.Open[i-1]) {
			t.Fatalf("escalations not in the waits' order at %d: %+v", i, es.Open)
		}
	}
}

// checkEscalationWire: on the wire, family is always there on an escalation, executor only when one runs it.
func checkEscalationWire(t *testing.T, f *Farm) {
	t.Helper()
	_, raw, _ := f.Snapshot()
	var doc struct {
		Escalations struct {
			Open []map[string]json.RawMessage `json:"open"`
		} `json:"escalations"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, w := range doc.Escalations.Open {
		_, hasExec := w["executor"]
		id := string(w["id"])
		if _, ok := w["family"]; !ok {
			t.Errorf("%s: no family on the wire", id)
		}
		if hasExec != (id == `"gd-Ye0"` || id == `"gd-Ycross"`) {
			t.Errorf("%s: executor on the wire = %v", id, hasExec)
		}
	}
}

func newWaitFarm(t *testing.T) *Farm {
	t.Helper()
	f := newTestFarm(waitWorld())
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	return f
}

// TestFarmWaitEscalationsCut: a project-scoped viewer gets its projects'
// escalations only, never an unprojected one, and an executor only when the
// executor's own project is in the set too.
func TestFarmWaitEscalationsCut(t *testing.T) {
	f := newCutFarm(t, escalationWorld())
	s, _, _, _ := cutDoc(t, f, "gardenio")
	if findWait(s.Escalations.Open, "gd-Yzeta") != nil || findWait(s.Escalations.Open, "gd-Ybare") != nil {
		t.Errorf("gardenio's cut lists another project's or an unprojected escalation: %+v", s.Escalations.Open)
	}
	if s.Escalations.Total != len(escalationOps)+2 {
		t.Errorf("gardenio's cut: total %d, want %d", s.Escalations.Total, len(escalationOps)+2)
	}
	if w := findWait(s.Escalations.Open, "gd-Ycross"); w == nil || w.Executor != "" {
		t.Errorf("cross in gardenio's cut = %+v; want listed, its zeta executor not named", w)
	}
	if w := findWait(s.Escalations.Open, "gd-Ye0"); w == nil || w.Executor != "escalation-gd-ye0" {
		t.Errorf("merge in gardenio's cut = %+v; want its gardenio executor named", w)
	}
	// The same rule holds in its plan's waiting[]: the plan is gardenio's, its executor is not.
	if p := findPlan(s, "gd-planY"); p == nil {
		t.Fatal("plan Y is not in gardenio's cut")
	} else if w := findWait(p.Waiting, "gd-Ycross"); w == nil || w.Executor != "" {
		t.Errorf("cross in plan Y's waiting in gardenio's cut = %+v; want listed, its zeta executor not named", w)
	} else if w := findWait(p.Waiting, "gd-Ye0"); w == nil || w.Executor != "escalation-gd-ye0" {
		t.Errorf("merge in plan Y's waiting in gardenio's cut = %+v; want its gardenio executor named", w)
	}
	z, _, _, _ := cutDoc(t, f, "zeta")
	if len(z.Escalations.Open) != 1 || z.Escalations.Open[0].ID != "gd-Yzeta" || z.Escalations.Total != 1 || len(z.Escalations.ByProject) != 1 || z.Escalations.ByProject["zeta"] != 1 {
		t.Errorf("zeta's cut = %+v; want gd-Yzeta alone", z.Escalations)
	}
	both, _, _, _ := cutDoc(t, f, "gardenio", "zeta")
	if w := findWait(both.Escalations.Open, "gd-Ycross"); w == nil || w.Executor != "zeta-runner" {
		t.Errorf("cross in both projects' cut = %+v; want its executor named", w)
	}
	if w := findWait(findPlan(both, "gd-planY").Waiting, "gd-Ycross"); w == nil || w.Executor != "zeta-runner" {
		t.Errorf("cross in plan Y's waiting in both projects' cut = %+v; want its executor named", w)
	}
}

// escalationOps is one operation_type per escalation family (FarmWait.Family), and an unknown one for other.
var escalationOps = []struct{ op, family string }{
	{"github_pr_merge", EscMerge}, {"helm_deploy", EscDeploy}, {"helm_chart_edit", EscConfig},
	{"trigger_prompt_install_reseal", EscPrompt}, {"kbeads_admin_write", EscBeads}, {"github_token_scope", EscAccess},
	{"terraform_apply", EscInfra}, {"db_readonly_query", EscRead}, {"capability_grant", EscOther},
}

// escalationWorld is the Escalations cell's world (gd-VyP9DLgvih): a plan holding one open escalation of each family, the
// merge one approved and run by an executor; an escalation with no plan and one with no project either; one in
// project zeta run by a zeta executor and one in gardenio run by a zeta executor; and a closed one that must not appear.
func escalationWorld() *fakeFarmSrc {
	seq = 0
	f := newFakeFarm()
	f.add(planBead("gd-planY", "building", "in_progress", "gd-Y1"), fb("gd-Y1", "epic", "in_progress", "gd-planY", 40))
	for i, o := range escalationOps {
		e := fb(fmt.Sprintf("gd-Ye%d", i), "escalation", "open", "gd-Y1", int64(100+i))
		e.Fields["operation_type"], e.Fields["requesting_agent"] = o.op, "crew-asker"
		f.add(e)
	}
	merge := f.beads[2] // gd-Ye0, the merge: approved, its executor live
	merge.Status, merge.Fields["approved_at"], merge.Fields["executor"] = "in_progress", "2026-10-19T01:56:09Z", "escalation-gd-ye0"
	exec := agentBead("gd-gYexec", "escalation-gd-ye0", "crew", "working", "", 7900)
	exec.Fields["escalation_id"] = "gd-Ye0"

	loose := fb("gd-Yloose", "escalation", "open", "", 200)
	loose.Fields["operation_type"] = "gitlab_mr_merge"
	bare := fb("gd-Ybare", "escalation", "open", "", 210) // no project: the Commons strip's
	bare.Labels, bare.Fields["operation_type"] = nil, "cluster_read"
	zeta := fb("gd-Yzeta", "escalation", "open", "", 220)
	zeta.Labels, zeta.Fields["operation_type"] = []string{"project:zeta"}, "deploy"
	cross := fb("gd-Ycross", "escalation", "open", "gd-Y1", 230) // gardenio's, in plan Y, run by a zeta gardener
	cross.Fields["operation_type"] = "helm_deploy"
	zexec := agentBead("gd-gZexec", "zeta-runner", "crew", "working", "", 7910)
	zexec.Labels, zexec.Fields["escalation_id"] = []string{"project:zeta"}, "gd-Ycross"
	closed := fb("gd-Yclosed", "escalation", "closed", "gd-Y1", 240)
	closed.Fields["operation_type"] = "github_pr_merge"
	f.add(loose, bare, zeta, cross, closed, exec, zexec)
	return f
}
