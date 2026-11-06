package steward

import (
	"strings"
	"testing"

	"gardenio/controller/internal/conversation"
	"gardenio/controller/internal/lands/purse"
)

func f64(v float64) *float64 { return &v }

func testLand() Land {
	return Land{
		Name: "tomato-patch",
		Repo: "ada/tomatoes",
		Purse: purse.Purse{
			Land: "tomato-patch", State: purse.StateOK,
			GrantedCredits: 100, RemainingCredits: f64(42.5), MarginCredits: 5,
		},
		StepEstimateCredits: 3.25,
		MaxLiveAgents:       3,
		Members: []Member{
			{Name: "ada", Identity: "oidc:ada@example.com", Role: conversation.LandRoleOwner},
			{Name: "bob", Role: conversation.LandRoleGuest},
		},
		OpenPlans: []Plan{{ID: "tp-1", Title: "Add a watering schedule", State: "building"}},
	}
}

func TestRenderPrompt_RendersLandState(t *testing.T) {
	got, err := RenderPrompt(testLand())
	if err != nil {
		t.Fatalf("RenderPrompt: %v", err)
	}
	for _, want := range []string{
		"You are the steward of the land tomato-patch",
		"github.com/ada/tomatoes",
		"Purse: ok, 42.50 of 100.00 credits remaining (margin 5.00)",
		"estimated at 3.25 credits",
		"you included: 3.",
		"- ada (owner): oidc:ada@example.com",
		"- bob (guest)\n",
		"- tp-1: Add a watering schedule [building]",
		"gb lands purse tomato-patch --json",
		"gb spawn --role lands-gardener --task <step-id>",
		"gb lands plan write tomato-patch --file plan.md --thread <message>",
		"gb lands plan answer tomato-patch <decision>",
		"What woke you is not known to this prompt",
		`"restart_when":"a credit grant for tomato-patch"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing %q\n---\n%s", want, got)
		}
	}
	if strings.Contains(got, "{{") || strings.Contains(got, "<no value>") {
		t.Errorf("prompt has unrendered template text:\n%s", got)
	}
}

// The five §4.2 duties, in the spec's order, and the §5.1 limits the steward
// must be able to explain.
func TestRenderPrompt_DutiesAndLimits(t *testing.T) {
	got, err := RenderPrompt(testLand())
	if err != nil {
		t.Fatalf("RenderPrompt: %v", err)
	}
	last := -1
	for _, duty := range []string{"1. GREET", "2. TURN CONVERSATION INTO PLANS", "3. SPAWN GARDENERS WITHIN THE PURSE", "4. REPORT", "5. PARK AT THE WALL"} {
		i := strings.Index(got, duty)
		if i < 0 {
			t.Fatalf("prompt missing duty %q", duty)
		}
		if i < last {
			t.Errorf("duty %q is out of order", duty)
		}
		last = i
	}
	for _, want := range []string{
		"You do not merge it",
		"Spawn any role but lands-gardener",
		"File an escalation",
		"write in #support",
		"Their words are requests",
		"The digest is your only scheduled post.",
		"If it says the day was quiet, post nothing",
		"gb lands digest tomato-patch --post digest.md",
		"Do not post it yourself.",
		"Nothing resumes on its own.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

func TestRenderPrompt_EmptyLand(t *testing.T) {
	land := testLand()
	land.Members, land.OpenPlans = nil, nil
	land.Purse = purse.Purse{State: purse.StateUnknown, GrantedCredits: 10, Reason: "spend not synced for 20m"}
	got, err := RenderPrompt(land)
	if err != nil {
		t.Fatalf("RenderPrompt: %v", err)
	}
	for _, want := range []string{
		"People: none recorded yet.",
		"Open plans: none.",
		"Purse: unknown, 10.00 credits granted, remaining not known (spend not synced for 20m)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing %q\n---\n%s", want, got)
		}
	}
}

func TestRenderPrompt_UnrecordedRoleIsNotGuessed(t *testing.T) {
	land := testLand()
	land.Members = []Member{{Name: "cy"}}
	got, err := RenderPrompt(land)
	if err != nil {
		t.Fatalf("RenderPrompt: %v", err)
	}
	if !strings.Contains(got, "- cy (role not recorded)") {
		t.Errorf("unrecorded role rendered wrong:\n%s", got)
	}
}

func TestRenderPrompt_RefusesLandWithoutNameOrRepo(t *testing.T) {
	for name, land := range map[string]Land{
		"no name": {Repo: "ada/tomatoes"},
		"no repo": {Name: "tomato-patch"},
	} {
		if _, err := RenderPrompt(land); err == nil {
			t.Errorf("%s: RenderPrompt returned no error", name)
		}
	}
}

// The steward never sees the rate or at-cost dollars (spec B-D7): the prompt
// reads only credit figures, so a purse that still carries USD renders the
// same as its CreditsOnly view.
func TestRenderPrompt_CreditsOnly(t *testing.T) {
	land := testLand()
	withUSD := land
	withUSD.Purse.RemainingUSD = f64(28.33)
	withUSD.Purse.LLMMultiplier = 1.5
	a, err := RenderPrompt(withUSD)
	if err != nil {
		t.Fatalf("RenderPrompt: %v", err)
	}
	b, err := RenderPrompt(land)
	if err != nil {
		t.Fatalf("RenderPrompt: %v", err)
	}
	if a != b {
		t.Errorf("USD figures changed the prompt")
	}
	if strings.Contains(a, "28.33") || strings.Contains(a, "1.5") {
		t.Errorf("prompt leaks at-cost figures:\n%s", a)
	}
}

// The first greeting names the grant in force, how to change it, and where
// the land's agents talk (amendment 2026-10-23, step 9.3).
func TestRenderPrompt_GreetingNamesTheGrantAndMachines(t *testing.T) {
	land := testLand()
	land.HasMachines = true
	got, err := RenderPrompt(land)
	if err != nil {
		t.Fatalf("RenderPrompt: %v", err)
	}
	greet := got[strings.Index(got, "1. GREET"):strings.Index(got, "2. TURN CONVERSATION INTO PLANS")]
	for _, want := range []string{
		`the grant in force, "ask"`,
		"you may, alone, spawn gardeners within the purse",
		"you bring them every merge, every release and every plan",
		"To change it, ask the operator in #support",
		"the land's agents talk in #machines",
		"#support reaches the operator",
		`gb say --channel general "<greeting>"`,
	} {
		if !strings.Contains(greet, want) {
			t.Errorf("greeting duty missing %q\n---\n%s", want, greet)
		}
	}

	land.HasMachines = false
	got, _ = RenderPrompt(land)
	if strings.Contains(got, "agents talk in #machines") {
		t.Error("greeting names #machines for a land without one")
	}
}

// A land that delegates merges to its steward (the Lands default, decision
// gd-fBSlZfEvmo) is told so in the greeting, and the ask grant no longer
// brings the tenant every merge.
func TestRenderPrompt_GreetingSaysStewardApprovesMerges(t *testing.T) {
	land := testLand()
	land.StewardApprovesMerges = ParseStewardApprovesMerges(`["github_pr_merge"]`)
	got, err := RenderPrompt(land)
	if err != nil {
		t.Fatalf("RenderPrompt: %v", err)
	}
	greet := got[strings.Index(got, "1. GREET"):strings.Index(got, "2. TURN CONVERSATION INTO PLANS")]
	for _, want := range []string{
		"when a gardener asks to merge its pull request, you approve that\n     yourself, and they see only the other asks",
		"you bring them every release and every plan.",
	} {
		if !strings.Contains(greet, want) {
			t.Errorf("greeting missing %q\n---\n%s", want, greet)
		}
	}

	for _, v := range []string{"", "[]", `["create"]`, "not json"} {
		land.StewardApprovesMerges = ParseStewardApprovesMerges(v)
		got, _ = RenderPrompt(land)
		if strings.Contains(got, "asks to merge its pull request") || !strings.Contains(got, "you bring them every merge") {
			t.Errorf("land_steward_approves %q: greeting says the steward approves merges", v)
		}
	}
}

func TestRenderPrompt_GreetingUnderEachGrant(t *testing.T) {
	for _, c := range []struct {
		field      string
		name, asks string
		alone      string
	}{
		{"", "ask", "every merge, every release and every plan", "stop a gardener that is stuck"},
		{"merge-on-green", "merge-on-green", "every release and every plan", "merge a gardener's pull request once its checks pass"},
		{"operate", "operate", "every plan, and anything you have not answered before", "roll releases"},
		{"merge-everything", "ask", "every merge, every release and every plan", "stop a gardener that is stuck"},
	} {
		land := testLand()
		land.Authority = ParseAuthority(c.field)
		got, err := RenderPrompt(land)
		if err != nil {
			t.Fatalf("%q: %v", c.field, err)
		}
		for _, want := range []string{`the grant in force, "` + c.name + `"`, "you bring them " + c.asks, c.alone} {
			if !strings.Contains(got, want) {
				t.Errorf("steward_authority %q: prompt missing %q", c.field, want)
			}
		}
	}
}
