package gardenhub

// farm_escalations.go — the escalations (gd-VyP9DLgvih): every open
// escalation the farm read, fleet-wide, as a FarmWait that also says what kind
// of thing it asks for (its family) and which gardener is working it.

import (
	"sort"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// maxEscalations caps FarmEscalations.Open. The fleet has a few dozen open at
// a time; the cap only bounds a runaway.
const maxEscalations = 120

// escalationFamilies maps an operation_type (lowercased) to its family. It is
// the one table: the client's words and sprites follow it. An operation type
// it does not name falls to escalationPrefixes, then to EscOther.
var escalationFamilies = map[string]string{
	"gitlab_mr_merge": EscMerge,
	"github_pr_merge": EscMerge,
	"gitlab_merge":    EscMerge,

	"helm_deploy": EscDeploy,
	"deploy":      EscDeploy,

	"helm_chart_edit":   EscConfig,
	"helm_chart_change": EscConfig,

	"trigger_prompt_install": EscPrompt,
	"trigger_prompt_reseal":  EscPrompt,
	"trigger_prompt_amend":   EscPrompt,
	"trigger_reseal":         EscPrompt,

	"beads_admin_write": EscBeads,

	"aws_iam":               EscAccess,
	"iam_grant":             EscAccess,
	"k8s_rbac":              EscAccess,
	"github_token_scope":    EscAccess,
	"kbeads_project_access": EscAccess,
	"kbeads_authz_change":   EscAccess,

	"infrastructure":           EscInfra,
	"terraform_apply":          EscInfra,
	"github_workflow_dispatch": EscInfra,

	"db_readonly_query": EscRead,
	"cluster_read":      EscRead,
}

// escalationPrefixes are the families named by an operation type's prefix,
// read after escalationFamilies, so kbeads_project_access stays access.
var escalationPrefixes = []struct{ prefix, family string }{
	{"trigger_prompt_", EscPrompt}, // trigger_prompt_install_reseal and the like
	{"kbeads_", EscBeads},
}

// EscalationFamily is the family of an operation_type: one of the Esc*
// constants, EscOther for an unknown or empty one, never "".
func EscalationFamily(op string) string {
	op = strings.ToLower(strings.TrimSpace(op))
	if fam, ok := escalationFamilies[op]; ok {
		return fam
	}
	for _, p := range escalationPrefixes {
		if strings.HasPrefix(op, p.prefix) {
			return p.family
		}
	}
	return EscOther
}

// execRef is the gardener working an escalation, and its own project (a cut
// names it only when that project is in the set).
type execRef struct {
	name, project string
	since         int64
	id            string
}

// escalationExecutors is the gardener working each escalation, by escalation
// id: a gardener whose fields.escalation_id names it (buildGardeners' rule,
// gardenerOf). From the agent read when it was complete, else from the last
// good gardener set. Two on one escalation name the newest, then the lower id.
func escalationExecutors(agents []*beadsapi.BeadDetail, fresh bool, last map[string]FarmGardener) map[string]execRef {
	out := map[string]execRef{}
	put := func(esc string, r execRef) {
		if esc == "" || r.name == "" {
			return
		}
		if o, ok := out[esc]; ok && (o.since > r.since || (o.since == r.since && o.id < r.id)) {
			return
		}
		out[esc] = r
	}
	if fresh {
		for _, b := range agents {
			if !isGardener(b) {
				continue
			}
			project := projectOf(b)
			if project == "" {
				project = strings.TrimSpace(b.Fields["project"])
			}
			put(strings.TrimSpace(b.Fields["escalation_id"]), execRef{name: agentFromBead(b).name, project: project, since: b.UpdatedAt.Unix(), id: b.ID})
		}
		return out
	}
	for id, g := range last {
		put(g.Escalation, execRef{name: g.Agent, project: g.Project, since: g.Since, id: id})
	}
	return out
}

// escalationWait is waitOf for an escalation, with its project, op, family and
// executor.
func escalationWait(b *beadsapi.BeadDetail, party func(string) (string, int), execs map[string]execRef) (FarmWait, execRef) {
	w := waitOf(b, party)
	ex := execs[b.ID]
	w.Executor = ex.name
	return w, ex
}

// escalationSet is the escalations before the cap, with each one's
// plan and executor project, so a cut can be built without a read.
type escalationSet struct {
	waits []FarmWait // sorted (waitLess)
	plans []string   // the plan or seed it is drawn in, "" for none, by index into waits
	execs []string   // the executor's project, by index into waits
}

// readEscalationSet projects every escalation row read; planOf is the plan or
// seed a row is drawn in ("" for none).
func readEscalationSet(rows []*beadsapi.BeadDetail, planOf func(*beadsapi.BeadDetail) string, party func(string) (string, int), execs map[string]execRef) *escalationSet {
	type one struct {
		w          FarmWait
		plan, proj string
	}
	var all []one
	for _, b := range rows {
		if b == nil || b.Type != "escalation" || b.Status == "closed" {
			continue
		}
		w, ex := escalationWait(b, party, execs)
		all = append(all, one{w, planOf(b), ex.project})
	}
	sort.Slice(all, func(i, j int) bool { return waitLess(all[i].w, all[j].w) })
	es := &escalationSet{waits: make([]FarmWait, len(all)), plans: make([]string, len(all)), execs: make([]string, len(all))}
	for i, o := range all {
		es.waits[i], es.plans[i], es.execs[i] = o.w, o.plan, o.proj
	}
	return es
}

// build is the blob: every escalation whose project allow admits (nil: all)
// and whose plan, when it has one, planOK admits (a step of the set inside a
// foreign plan is not the set's to see), its executor kept only when the
// executor's own project is admitted too. An unprojected escalation is
// admitted only by a nil allow, the whole farm.
func (es *escalationSet) build(allow, planOK func(string) bool) FarmEscalations {
	out := FarmEscalations{Open: []FarmWait{}, ByProject: map[string]int{}}
	if es == nil {
		return out
	}
	for i, w := range es.waits {
		if allow != nil {
			if w.Project == "" || !allow(w.Project) || (es.plans[i] != "" && !planOK(es.plans[i])) {
				continue
			}
			if w.Executor != "" && (es.execs[i] == "" || !allow(es.execs[i])) {
				w.Executor = ""
			}
		}
		out.Total++
		out.ByProject[w.Project]++
		if len(out.Open) < maxEscalations {
			out.Open = append(out.Open, w)
		}
	}
	return out
}

// emptyEscalations is the blob before the first refresh.
const emptyEscalations = `{"open":[],"total":0,"by_project":{}}`
