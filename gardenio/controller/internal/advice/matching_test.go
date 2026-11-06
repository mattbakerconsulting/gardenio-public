package advice

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func TestMatchesSubscriptions_Global(t *testing.T) {
	labels := []string{"global"}
	subs := []string{"global", "agent:test/crew/bot"}
	if !MatchesSubscriptions(labels, subs) {
		t.Error("global advice should match any agent with global subscription")
	}
}

func TestMatchesSubscriptions_RoleMatch(t *testing.T) {
	labels := []string{"role:crew"}
	subs := []string{"global", "role:crew", "agent:gardenio/crews/bot"}
	if !MatchesSubscriptions(labels, subs) {
		t.Error("role:crew advice should match agent with role:crew subscription")
	}
}

func TestMatchesSubscriptions_RoleMismatch(t *testing.T) {
	labels := []string{"role:lead"}
	subs := []string{"global", "role:crew", "agent:gardenio/crews/bot"}
	if MatchesSubscriptions(labels, subs) {
		t.Error("role:lead advice should not match agent with only role:crew subscription")
	}
}

func TestMatchesSubscriptions_ProjectRequired(t *testing.T) {
	labels := []string{"project:gardenio", "role:crew"}
	subs := []string{"global", "project:other", "role:crew"}
	if MatchesSubscriptions(labels, subs) {
		t.Error("advice with project:gardenio should not match agent with project:other")
	}
}

func TestMatchesSubscriptions_AgentSpecific(t *testing.T) {
	labels := []string{"agent:gardenio/crews/bot"}
	subs := []string{"global", "agent:gardenio/crews/bot"}
	if !MatchesSubscriptions(labels, subs) {
		t.Error("agent-specific advice should match the exact agent")
	}
}

func TestMatchesSubscriptions_AgentMismatch(t *testing.T) {
	labels := []string{"agent:gardenio/crews/other"}
	subs := []string{"global", "agent:gardenio/crews/bot"}
	if MatchesSubscriptions(labels, subs) {
		t.Error("agent-specific advice should not match a different agent")
	}
}

func TestMatchesSubscriptions_ANDGroup(t *testing.T) {
	// g0:role:crew AND g0:project:gardenio -- both must match
	labels := []string{"g0:role:crew", "g0:project:gardenio"}
	subs := []string{"global", "role:crew", "project:gardenio"}
	if !MatchesSubscriptions(labels, subs) {
		t.Error("AND group should match when all labels in group match")
	}
}

func TestMatchesSubscriptions_ANDGroupPartial(t *testing.T) {
	labels := []string{"g0:role:crew", "g0:project:gardenio"}
	subs := []string{"global", "role:crew", "project:other"}
	if MatchesSubscriptions(labels, subs) {
		t.Error("AND group should not match when only some labels match")
	}
}

func TestMatchesSubscriptions_ORGroups(t *testing.T) {
	// Two separate groups: g0:role:crew OR g1:role:lead
	labels := []string{"g0:role:crew", "g1:role:lead"}
	subs := []string{"global", "role:lead"}
	if !MatchesSubscriptions(labels, subs) {
		t.Error("OR across groups should match when any group matches")
	}
}

func TestParseGroups_Grouped(t *testing.T) {
	labels := []string{"g0:role:crew", "g0:project:gardenio", "g1:role:lead"}
	groups := ParseGroups(labels)

	if len(groups[0]) != 2 {
		t.Errorf("group 0 should have 2 labels, got %d", len(groups[0]))
	}
	if len(groups[1]) != 1 {
		t.Errorf("group 1 should have 1 label, got %d", len(groups[1]))
	}
}

func TestParseGroups_Ungrouped(t *testing.T) {
	labels := []string{"global", "role:crew"}
	groups := ParseGroups(labels)

	// Each ungrouped label gets its own group (starting at 1000)
	count := 0
	for _, v := range groups {
		count += len(v)
	}
	if count != 2 {
		t.Errorf("expected 2 total labels across groups, got %d", count)
	}
}

func TestCategorizeScope_Global(t *testing.T) {
	scope, target := CategorizeScope([]string{"global"})
	if scope != "global" || target != "" {
		t.Errorf("expected global/'', got %s/%s", scope, target)
	}
}

func TestCategorizeScope_Role(t *testing.T) {
	scope, target := CategorizeScope([]string{"global", "role:crew"})
	if scope != "role" || target != "crew" {
		t.Errorf("expected role/crew, got %s/%s", scope, target)
	}
}

func TestCategorizeScope_Agent(t *testing.T) {
	scope, target := CategorizeScope([]string{"global", "role:crew", "agent:gardenio/crews/bot"})
	if scope != "agent" || target != "gardenio/crews/bot" {
		t.Errorf("expected agent/gardenio/crews/bot, got %s/%s", scope, target)
	}
}

func TestCategorizeScope_Project(t *testing.T) {
	scope, target := CategorizeScope([]string{"project:gardenio"})
	if scope != "project" || target != "gardenio" {
		t.Errorf("expected project/gardenio, got %s/%s", scope, target)
	}
}

func TestCategorizeScope_GroupPrefix(t *testing.T) {
	scope, target := CategorizeScope([]string{"g0:role:crew", "g0:project:gardenio"})
	if scope != "role" || target != "crew" {
		t.Errorf("expected role/crew, got %s/%s", scope, target)
	}
}

// TestCategorizeScopeForMatch_PrefersMatchedRole is the regression test for
// ka-0000001489: a gardener-e2e agent loading multi-role advice tagged at both
// gardener-e2e and crew-e2e MUST render under the gardener-e2e header, not under
// crew-e2e (the sibling role).
func TestCategorizeScopeForMatch_PrefersMatchedRole(t *testing.T) {
	labels := []string{
		"g0:project:gardenio", "g0:role:gardener-e2e",
		"g1:project:gardenio", "g1:role:crew-e2e",
	}
	// Gardener-e2e agent: only role:gardener-e2e is matched.
	matched := []string{"project:gardenio", "role:gardener-e2e"}
	scope, target := CategorizeScopeForMatch(labels, matched)
	if scope != "role" || target != "gardener-e2e" {
		t.Errorf("expected role/gardener-e2e, got %s/%s", scope, target)
	}

	// Crew-e2e agent: only role:crew-e2e is matched. Same labels, opposite role.
	matchedCrew := []string{"project:gardenio", "role:crew-e2e"}
	scope, target = CategorizeScopeForMatch(labels, matchedCrew)
	if scope != "role" || target != "crew-e2e" {
		t.Errorf("expected role/crew-e2e, got %s/%s", scope, target)
	}
}

// TestCategorizeScopeForMatch_AgentWins ensures an explicit agent: label still
// short-circuits to agent scope, regardless of matched role labels.
func TestCategorizeScopeForMatch_AgentWins(t *testing.T) {
	labels := []string{"role:gardener-e2e", "role:crew-e2e", "agent:gardenio/gardeners/bot"}
	matched := []string{"role:gardener-e2e", "agent:gardenio/gardeners/bot"}
	scope, target := CategorizeScopeForMatch(labels, matched)
	if scope != "agent" || target != "gardenio/gardeners/bot" {
		t.Errorf("expected agent/gardenio/gardeners/bot, got %s/%s", scope, target)
	}
}

// TestCategorizeScopeForMatch_FallbackNoMatchedRole ensures the function
// degrades to the legacy CategorizeScope when matchedLabels carries no
// role: entry (e.g. global/project-only advice).
func TestCategorizeScopeForMatch_FallbackNoMatchedRole(t *testing.T) {
	labels := []string{"global"}
	scope, target := CategorizeScopeForMatch(labels, []string{"global"})
	if scope != "global" || target != "" {
		t.Errorf("expected global/'', got %s/%s", scope, target)
	}

	labels = []string{"project:gardenio"}
	scope, target = CategorizeScopeForMatch(labels, []string{"project:gardenio"})
	if scope != "project" || target != "gardenio" {
		t.Errorf("expected project/gardenio, got %s/%s", scope, target)
	}
}

// TestCategorizeScopeForMatch_StripGroupPrefix ensures matched labels carrying
// a gN: prefix still resolve to a role match.
func TestCategorizeScopeForMatch_StripGroupPrefix(t *testing.T) {
	labels := []string{"g0:role:gardener-e2e", "g1:role:crew-e2e"}
	matched := []string{"g0:role:gardener-e2e"}
	scope, target := CategorizeScopeForMatch(labels, matched)
	if scope != "role" || target != "gardener-e2e" {
		t.Errorf("expected role/gardener-e2e, got %s/%s", scope, target)
	}
}

func TestBuildAgentSubscriptions(t *testing.T) {
	subs := BuildAgentSubscriptions("gardenio/crews/bot", nil)

	expected := map[string]bool{
		"global":                   true,
		"agent:gardenio/crews/bot": true,
		"project:gardenio":         true,
		"role:crews":               true,
		"role:crew":                true,
	}
	for _, s := range subs {
		delete(expected, s)
	}
	if len(expected) > 0 {
		t.Errorf("missing subscriptions: %v", expected)
	}
}

func TestBuildAgentSubscriptions_SimpleID(t *testing.T) {
	subs := BuildAgentSubscriptions("myproject", nil)

	has := make(map[string]bool)
	for _, s := range subs {
		has[s] = true
	}
	if !has["global"] {
		t.Error("should include global")
	}
	if !has["project:myproject"] {
		t.Error("should include project:myproject")
	}
}

func TestBuildAgentSubscriptions_WithExtra(t *testing.T) {
	subs := BuildAgentSubscriptions("gardenio/crews/bot", []string{"custom:label"})
	has := make(map[string]bool)
	for _, s := range subs {
		has[s] = true
	}
	if !has["custom:label"] {
		t.Error("should include extra labels")
	}
}

func TestStripGroupPrefix(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"g0:role:crew", "role:crew"},
		{"g12:project:gardenio", "project:gardenio"},
		{"global", "global"},
		{"role:crew", "role:crew"},
		{"g:bad", "g:bad"}, // g without number
	}
	for _, tt := range tests {
		got := StripGroupPrefix(tt.input)
		if got != tt.want {
			t.Errorf("StripGroupPrefix(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestSingularize(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"crews", "crew"},
		{"leads", "lead"},
		{"crew", "crew"},
		{"s", ""},
	}
	for _, tt := range tests {
		got := Singularize(tt.input)
		if got != tt.want {
			t.Errorf("Singularize(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestHasTargetingLabel(t *testing.T) {
	tests := []struct {
		labels []string
		want   bool
	}{
		{[]string{"global"}, true},
		{[]string{"project:gardenio"}, true},
		{[]string{"role:crew"}, true},
		{[]string{"agent:bot"}, true},
		{[]string{"g0:role:crew"}, true},
		{[]string{"random"}, false},
		{nil, false},
	}
	for _, tt := range tests {
		got := HasTargetingLabel(tt.labels)
		if got != tt.want {
			t.Errorf("HasTargetingLabel(%v) = %v, want %v", tt.labels, got, tt.want)
		}
	}
}

func TestFindMatchedLabels(t *testing.T) {
	labels := []string{"g0:role:crew", "g0:project:gardenio", "global"}
	subs := []string{"global", "role:crew", "project:gardenio"}

	matched := FindMatchedLabels(labels, subs)
	if len(matched) != 3 {
		t.Errorf("expected 3 matched labels, got %d: %v", len(matched), matched)
	}
}

// TestMatchesSubscriptions_RoleRequiredAcrossUngroupedLabels covers ka-0000001253:
// advice tagged `project:gardenio, role:gardener` (no g-prefix) must NOT match a
// crew agent. Previously the bare project label formed its own OR group that
// satisfied any gardenio member, regardless of role.
func TestMatchesSubscriptions_RoleRequiredAcrossUngroupedLabels(t *testing.T) {
	subs := []string{"global", "role:crew", "project:gardenio", "agent:gardenio-x"}

	if MatchesSubscriptions([]string{"project:gardenio", "role:gardener"}, subs) {
		t.Error("crew agent must not match role:gardener advice via project label")
	}
	if MatchesSubscriptions([]string{"project:gardenio", "role:gardener-delivery"}, subs) {
		t.Error("crew agent must not match role:gardener-delivery advice")
	}
	if MatchesSubscriptions([]string{"project:gardenio", "role:gardener-e2e"}, subs) {
		t.Error("crew agent must not match role:gardener-e2e advice")
	}
	if MatchesSubscriptions([]string{"project:gardenio", "role:crew-e2e"}, subs) {
		t.Error("crew agent must not match role:crew-e2e advice")
	}
	if !MatchesSubscriptions([]string{"project:gardenio", "role:crew"}, subs) {
		t.Error("crew agent must match role:crew advice")
	}
	if !MatchesSubscriptions([]string{"project:gardenio"}, subs) {
		t.Error("project-only advice must match any agent in the project")
	}
	// Multi-role advice: at least one role must match (OR).
	if !MatchesSubscriptions([]string{"project:gardenio", "role:gardener", "role:crew"}, subs) {
		t.Error("multi-role advice must match when one role matches the agent")
	}
	// Group prefix should not bypass the role requirement.
	if MatchesSubscriptions([]string{"g0:role:gardener-e2e", "g0:role:crew-e2e", "project:gardenio"}, subs) {
		t.Error("crew agent must not match gardener-e2e/crew-e2e grouped advice")
	}
}

func TestMatchesSubscriptions_MultiRole(t *testing.T) {
	// Agent with multiple role subscriptions should match advice for either role
	subs := []string{"global", "role:thread", "role:crew", "project:gardenio"}

	if !MatchesSubscriptions([]string{"role:crew"}, subs) {
		t.Error("multi-role agent should match role:crew advice")
	}
	if !MatchesSubscriptions([]string{"role:thread"}, subs) {
		t.Error("multi-role agent should match role:thread advice")
	}
	if MatchesSubscriptions([]string{"role:gardener"}, subs) {
		t.Error("multi-role agent should not match role:gardener advice")
	}
}

func TestBuildScopeHeader(t *testing.T) {
	tests := []struct {
		scope, target, want string
	}{
		{"global", "", "Global"},
		{"project", "gardenio", "Project: gardenio"},
		{"role", "crew", "Role: crew"},
		{"agent", "gardenio/crews/bot", "Agent: gardenio/crews/bot"},
	}
	for _, tt := range tests {
		got := BuildScopeHeader(tt.scope, tt.target)
		if got != tt.want {
			t.Errorf("BuildScopeHeader(%q, %q) = %q, want %q", tt.scope, tt.target, got, tt.want)
		}
	}
}

func TestGroupSortKey(t *testing.T) {
	// Global should sort before project, project before role, role before agent
	keys := []string{
		GroupSortKey("agent", "bot"),
		GroupSortKey("global", ""),
		GroupSortKey("role", "crew"),
		GroupSortKey("project", "gardenio"),
	}
	if keys[1] >= keys[3] || keys[3] >= keys[2] || keys[2] >= keys[0] {
		t.Errorf("sort order wrong: global=%s project=%s role=%s agent=%s",
			keys[1], keys[3], keys[2], keys[0])
	}
}

// TestEnrichAgentSubscriptions_FallsBackToClosedBead covers the regression
// fixed in ka-0000007427: a thread agent's previous session bead has closed
// before the next session bead opens, leaving FindAgentBead with no active
// match. Without the fallback, EnrichAgentSubscriptions would silently drop
// the project/role labels and the agent would prime with zero project-scoped
// advice. With the fallback, the closed bead's labels survive and project:X
// is added to the subscriptions.
func TestEnrichAgentSubscriptions_FallsBackToClosedBead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Active query (status=open,...) → 404 (the resume window).
		// Any-status query (no status filter) → one closed match. The new
		// /v1/agents-by-name/{name} endpoint returns a single bead JSON object,
		// so the test handler now mirrors that single-object contract.
		statuses := r.URL.Query().Get("status")
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(statuses, "open") {
			http.Error(w, `{"error":"agent not found"}`, http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{
			"id":"gd-closed",
			"type":"agent",
			"status":"closed",
			"updated_at":"2026-05-01T01:34:06Z",
			"labels":["project:monorepo","role:k6-perf"],
			"fields":{"agent":"thread-x","role":"k6-perf","project":"monorepo"}
		}`))
	}))
	defer srv.Close()

	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("New client: %v", err)
	}

	subs := BuildAgentSubscriptions("thread-x", nil)
	subs = EnrichAgentSubscriptions(context.Background(), c, "thread-x", subs)

	want := map[string]bool{
		"project:monorepo": false,
		"role:k6-perf":     false,
	}
	for _, s := range subs {
		if _, ok := want[s]; ok {
			want[s] = true
		}
	}
	for label, present := range want {
		if !present {
			t.Errorf("expected %q in enriched subs, got: %v", label, subs)
		}
	}

	// Required label of project:monorepo advice: subs must satisfy it.
	if !MatchesSubscriptions([]string{"project:monorepo", "reference:true"}, subs) {
		t.Errorf("project:monorepo advice should match subs after fallback enrichment, got subs=%v", subs)
	}
}

// TestAppendRoleFamilySubs verifies the gardener role-family anchor rule
// (ka-0000000880 / decision ka-0000003366): a differentiated gardener role
// inherits role:gardener so the shared gardener doctrine pool reaches the ~90%
// of real gardenio gardeners that run a differentiated role. It must be a token
// match (covering SUFFIX forms plan-gardener/cost-gardener/infra-gardener), not a
// gardener-* prefix glob, and must NOT drag non-gardener roles into the family.
func TestAppendRoleFamilySubs(t *testing.T) {
	cases := []struct {
		name       string
		in         []string
		wantAnchor bool // expect role:gardener + role:gardeners appended
	}{
		{"bare gardener already anchored", []string{"role:gardener", "role:gardeners"}, false},
		{"prefix-token differentiated gardener", []string{"role:gardener-delivery"}, true},
		{"prefix-token gardener-release-owner", []string{"role:gardener-release-owner"}, true},
		{"suffix-token plan-gardener", []string{"role:plan-gardener"}, true},
		{"suffix-token cost-gardener", []string{"role:cost-gardener"}, true},
		{"suffix-token infra-gardener", []string{"role:infra-gardener"}, true},
		{"group-prefixed differentiated gardener", []string{"g0:role:gardener-bugfix"}, true},
		{"non-gardener role not in family", []string{"role:crew-fable"}, false},
		{"plain crew not in family", []string{"role:crew"}, false},
		{"no role labels", []string{"global", "project:gardenio"}, false},
		// A family role that ALSO already carries the anchor: idempotent, no dup.
		{"family role with anchor present", []string{"role:gardener-delivery", "role:gardener", "role:gardeners"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := AppendRoleFamilySubs(append([]string(nil), c.in...))
			hasCap, hasCaps := false, false
			nCap, nCaps := 0, 0
			for _, s := range got {
				switch s {
				case "role:gardener":
					hasCap = true
					nCap++
				case "role:gardeners":
					hasCaps = true
					nCaps++
				}
			}
			if c.wantAnchor {
				if !hasCap || !hasCaps {
					t.Errorf("AppendRoleFamilySubs(%v) = %v; want role:gardener + role:gardeners anchor", c.in, got)
				}
			} else {
				// Either the anchor was already present (kept, not duplicated)
				// or the role is not in the family (never added).
				if nCap > 1 || nCaps > 1 {
					t.Errorf("AppendRoleFamilySubs(%v) = %v; duplicated anchor label", c.in, got)
				}
			}
		})
	}
}

// TestBuildAgentSubscriptions_GardenerFamily locks the end-to-end path: a
// differentiated gardener agent ID yields the role:gardener anchor, and a bare
// project/role agent that is NOT in the family does not.
func TestBuildAgentSubscriptions_GardenerFamily(t *testing.T) {
	subs := BuildAgentSubscriptions("gardenio/gardener-delivery/bot-x", nil)
	set := map[string]bool{}
	for _, s := range subs {
		set[s] = true
	}
	if !set["role:gardener"] || !set["role:gardeners"] {
		t.Errorf("gardener-delivery agent should inherit role:gardener anchor, got %v", subs)
	}
	if !set["role:gardener-delivery"] {
		t.Errorf("gardener-delivery agent should keep its own role, got %v", subs)
	}

	subs = BuildAgentSubscriptions("gardenio/crew-fables/bot-y", nil)
	for _, s := range subs {
		if s == "role:gardener" {
			t.Errorf("crew-fable agent must NOT inherit role:gardener, got %v", subs)
		}
	}
}
