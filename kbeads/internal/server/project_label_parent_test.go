package server

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

// seedPlan puts a plan epic labelled project:gardenio into ms.
func seedPlan(ms *mockStore, id string) {
	ms.beads[id] = &model.Bead{ID: id, Type: "epic", Kind: model.KindIssue, Title: "plan", Status: model.StatusOpen}
	ms.labels[id] = []string{"plan", "project:gardenio"}
}

// An operator's `kd create --parent <plan>` from a laptop carries no project
// label (no KD_PROJECT there); the step is born with the plan's, so the plan's
// gardener and crew can read it (gd-BYCG4IGJB2).
func TestCreateBead_ParentedStepInheritsPlanProject(t *testing.T) {
	s, ms, _ := newTestServer()
	seedPlan(ms, "gd-plan1")

	step, err := s.createBead(operatorCtx("op@example.com"), createBeadInput{
		Title: "new step", Type: "task", Kind: "issue", ParentBeadID: "gd-plan1",
	})
	if err != nil {
		t.Fatalf("createBead: %v", err)
	}
	if got := ms.labels[step.ID]; !slices.Contains(got, "project:gardenio") {
		t.Fatalf("step labels = %v, want project:gardenio inherited from the plan", got)
	}
	if !beadMatchesTokenProject(agentCtx("gd-agent1", "gardener-a", "gardenio"), ms.labels[step.ID]) {
		t.Fatalf("a gardenio-scoped token cannot see the step (labels %v)", ms.labels[step.ID])
	}
}

// The 11-04 shape: `kd create` with no parent, then `kd dep add <step> <plan>
// --type parent-child`, both on the operator's OIDC session. The edge gives the
// label-less step the plan's project; a step with its own project, a non
// parent-child edge and a global advice bead are left as they are.
func TestAddDependency_ParentChildGivesLabelLessChildParentProject(t *testing.T) {
	cases := []struct {
		name    string
		typ     model.BeadType
		labels  []string
		depType string
		want    []string
	}{
		{"label-less step", "task", nil, "parent-child", []string{"project:gardenio"}},
		{"step of another project", "task", []string{"project:other"}, "parent-child", []string{"project:other"}},
		{"blocks edge", "task", nil, "blocks", nil},
		{"global advice", "advice", nil, "parent-child", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ms, h := newTestServerAuth()
			seedPlan(ms, "gd-plan1")
			ms.beads["gd-step1"] = &model.Bead{ID: "gd-step1", Type: tc.typ, Kind: model.KindIssue, Title: "s", Status: model.StatusOpen}
			ms.labels["gd-step1"] = slices.Clone(tc.labels)

			rec := doJSONAs(t, h, http.MethodPost, "/v1/beads/gd-step1/dependencies",
				map[string]string{"depends_on_id": "gd-plan1", "type": tc.depType}, "op@example.com")
			if rec.Code != http.StatusCreated {
				t.Fatalf("add dependency: %d %s", rec.Code, rec.Body.String())
			}
			var projects []string
			for _, l := range ms.labels["gd-step1"] {
				if strings.HasPrefix(l, "project:") {
					projects = append(projects, l)
				}
			}
			if !slices.Equal(projects, tc.want) {
				t.Fatalf("step project labels = %v, want %v", projects, tc.want)
			}
		})
	}
}

// Mail from an unscoped sender (here an operator, as the e2e's gb mail send;
// the bridge's nudges are the same shape) takes its recipient's project, so the
// recipient's project-scoped token can see it on its stream and in its drain
// (gd-D8jkm7eyEO). An agent is found by fields.agent, a runner:<id> recipient
// by its runner bead; a recipient with no single project, or none at all,
// leaves the mail as written, and mail with its own project keeps it.
func TestCreateBead_LabelLessMailTakesRecipientProject(t *testing.T) {
	cases := []struct {
		name     string
		assignee string
		labels   []string
		want     string // the one project label wanted, or "" for none
	}{
		{"agent recipient", "crew-mail", nil, "project:gardenio"},
		{"runner recipient", "runner:gd-runner1", nil, "project:gardenio"},
		{"multi-project runner", "runner:gd-runner2", nil, ""},
		{"unknown recipient", "nobody", nil, ""},
		{"own project kept", "crew-mail", []string{"project:other"}, "project:other"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, ms, _ := newTestServer()
			ms.beads["gd-agent1"] = &model.Bead{ID: "gd-agent1", Type: "agent", Kind: model.KindConfig, Title: "crew-mail", Status: model.StatusOpen,
				Fields: mustJSON(map[string]string{"agent": "crew-mail", "project": "gardenio"})}
			ms.labels["gd-agent1"] = []string{"project:gardenio"}
			ms.beads["gd-runner1"] = &model.Bead{ID: "gd-runner1", Type: "runner", Kind: model.KindConfig, Title: "laptop", Status: model.StatusOpen}
			ms.labels["gd-runner1"] = []string{"project:gardenio"}
			ms.beads["gd-runner2"] = &model.Bead{ID: "gd-runner2", Type: "runner", Kind: model.KindConfig, Title: "shared", Status: model.StatusOpen}
			ms.labels["gd-runner2"] = []string{"project:gardenio", "project:other"}

			mail, err := s.createBead(operatorCtx("op@example.com"), createBeadInput{
				Title: "hello", Type: "mail", Kind: "data", Assignee: tc.assignee,
				Labels: append([]string{"from:op", "delivery:interrupt"}, tc.labels...),
			})
			if err != nil {
				t.Fatalf("createBead: %v", err)
			}
			got := ms.labels[mail.ID]
			var projects []string
			for _, l := range got {
				if strings.HasPrefix(l, "project:") {
					projects = append(projects, l)
				}
			}
			switch {
			case tc.want == "" && len(projects) != 0:
				t.Fatalf("labels = %v, want no project label", got)
			case tc.want != "" && !slices.Equal(projects, []string{tc.want}):
				t.Fatalf("labels = %v, want only %s", got, tc.want)
			}
			if tc.want == "project:gardenio" && !beadMatchesTokenProject(agentCtx("gd-agent1", "crew-mail", "gardenio"), got) {
				t.Fatalf("the recipient's gardenio-scoped token cannot see the mail (labels %v)", got)
			}
		})
	}
}
