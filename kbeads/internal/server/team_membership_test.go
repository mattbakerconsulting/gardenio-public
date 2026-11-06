package server

import (
	"context"
	"testing"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

func newTeamMembershipTestServer() (*BeadsServer, *mockStore) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	s.userProvisioner = nil
	return s, ms
}

func seedTeamBead(ms *mockStore, id, fieldsJSON string) {
	ms.beads[id] = &model.Bead{
		ID:     id,
		Kind:   model.KindConfig,
		Type:   model.TypeTeam,
		Title:  id,
		Fleet:  "gardenio",
		Fields: []byte(fieldsJSON),
	}
}

func TestTeamsOf_MemberOnlyMatch(t *testing.T) {
	s, ms := newTeamMembershipTestServer()
	ms.operatorAliases = map[string]int64{"oidc\x00alice@example.com": 42}
	seedTeamBead(ms, "gd-team1", `{"team":"acme-prod","members":["oidc:alice@example.com"]}`)
	seedTeamBead(ms, "gd-team2", `{"team":"other","members":["oidc:bob@example.com"]}`)

	ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: "oidc", Subject: "alice@example.com"})
	got, err := s.TeamsOf(ctx)
	if err != nil {
		t.Fatalf("TeamsOf: %v", err)
	}
	if len(got) != 1 || got[0] != "gd-team1" {
		t.Fatalf("expected [gd-team1], got %v", got)
	}
}

func TestTeamsOf_OIDCGroupsOnlyMatch(t *testing.T) {
	s, ms := newTeamMembershipTestServer()
	ms.operatorAliases = map[string]int64{"oidc\x00alice@example.com": 42}
	seedTeamBead(ms, "gd-team1", `{"team":"acme-prod","oidc_groups":["/platform-team"]}`)
	seedTeamBead(ms, "gd-team2", `{"team":"other","oidc_groups":["/other-team"]}`)

	ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: "oidc", Subject: "alice@example.com"})
	ctx = WithGroups(ctx, []string{"/platform-team"})
	got, err := s.TeamsOf(ctx)
	if err != nil {
		t.Fatalf("TeamsOf: %v", err)
	}
	if len(got) != 1 || got[0] != "gd-team1" {
		t.Fatalf("expected [gd-team1], got %v", got)
	}
}

func TestTeamsOf_BothMembersAndGroupsMatch(t *testing.T) {
	s, ms := newTeamMembershipTestServer()
	ms.operatorAliases = map[string]int64{"oidc\x00alice@example.com": 42}
	seedTeamBead(ms, "gd-team1", `{"team":"acme-prod","members":["oidc:alice@example.com"]}`)
	seedTeamBead(ms, "gd-team2", `{"team":"platform","oidc_groups":["/platform-team"]}`)
	seedTeamBead(ms, "gd-team3", `{"team":"other"}`)

	ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: "oidc", Subject: "alice@example.com"})
	ctx = WithGroups(ctx, []string{"/platform-team"})
	got, err := s.TeamsOf(ctx)
	if err != nil {
		t.Fatalf("TeamsOf: %v", err)
	}
	want := map[string]bool{"gd-team1": true, "gd-team2": true}
	if len(got) != len(want) {
		t.Fatalf("expected 2 teams, got %v", got)
	}
	for _, id := range got {
		if !want[id] {
			t.Fatalf("unexpected team %q in result %v", id, got)
		}
	}
}

func TestTeamsOf_NoMatch(t *testing.T) {
	s, ms := newTeamMembershipTestServer()
	ms.operatorAliases = map[string]int64{"oidc\x00alice@example.com": 42}
	seedTeamBead(ms, "gd-team1", `{"team":"acme-prod","members":["oidc:bob@example.com"],"oidc_groups":["/other-team"]}`)

	ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: "oidc", Subject: "alice@example.com"})
	got, err := s.TeamsOf(ctx)
	if err != nil {
		t.Fatalf("TeamsOf: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no teams, got %v", got)
	}
}

func TestTeamsOf_AgentIdentityReturnsEmpty(t *testing.T) {
	s, ms := newTeamMembershipTestServer()
	ms.operatorAliases = map[string]int64{"oidc\x00alice@example.com": 42}
	seedTeamBead(ms, "gd-team1", `{"team":"acme-prod","members":["oidc:alice@example.com"]}`)

	// A static token whose subject is an agent bead id — the session carrier
	// for an agent, never a team member (ADR docs/adr/taints.md §8.1).
	ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: "static", Subject: "gd-agent1"})
	got, err := s.TeamsOf(ctx)
	if err != nil {
		t.Fatalf("TeamsOf: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty result for an agent identity, got %v", got)
	}
	if len(ms.listFilters) != 0 {
		t.Fatalf("expected no store query for an agent identity, got filters %v", ms.listFilters)
	}
}

func TestTeamsOf_FleetAndPrimaryStaticAreEmpty(t *testing.T) {
	s, ms := newTeamMembershipTestServer()
	seedTeamBead(ms, "gd-team1", `{"team":"acme-prod","members":["oidc:alice@example.com"]}`)

	for _, id := range []AuthIdentity{
		{Type: "fleet", Subject: "gardenio-fleet"},
		{Type: "static", Subject: "static"},
		{Type: "anonymous"},
	} {
		ctx := WithAuthIdentity(context.Background(), id)
		got, err := s.TeamsOf(ctx)
		if err != nil {
			t.Fatalf("TeamsOf(%v): %v", id, err)
		}
		if len(got) != 0 {
			t.Fatalf("expected empty result for identity %v, got %v", id, got)
		}
	}
}

// TestTeamsOf_OperatorAliasEquivalence proves the acceptance criterion: an
// operator alias (email vs Slack id both resolving to the same operators.id)
// is treated as one member for `members` matching. Here the team lists the
// caller's Slack alias while the caller authenticates over OIDC (email) —
// both aliases are pre-linked to operator id 42 in the mock operator_aliases
// table, so the caller must still match.
func TestTeamsOf_OperatorAliasEquivalence(t *testing.T) {
	s, ms := newTeamMembershipTestServer()
	ms.operatorAliases = map[string]int64{
		"oidc\x00alice@example.com": 42,
		"slack\x00U12345":           42,
	}
	seedTeamBead(ms, "gd-team1", `{"team":"acme-prod","members":["slack:U12345"]}`)

	ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: "oidc", Subject: "alice@example.com"})
	got, err := s.TeamsOf(ctx)
	if err != nil {
		t.Fatalf("TeamsOf: %v", err)
	}
	if len(got) != 1 || got[0] != "gd-team1" {
		t.Fatalf("expected [gd-team1] via alias equivalence, got %v", got)
	}
}

func TestTeamsOf_MembersAsResolvedOperatorIDs(t *testing.T) {
	s, ms := newTeamMembershipTestServer()
	ms.operatorAliases = map[string]int64{"oidc\x00alice@example.com": 42}
	seedTeamBead(ms, "gd-team1", `{"team":"acme-prod","members":[42]}`)

	ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: "oidc", Subject: "alice@example.com"})
	got, err := s.TeamsOf(ctx)
	if err != nil {
		t.Fatalf("TeamsOf: %v", err)
	}
	if len(got) != 1 || got[0] != "gd-team1" {
		t.Fatalf("expected [gd-team1] via write-time-resolved id, got %v", got)
	}
}

func TestTeamsOf_UnresolvedOperatorReturnsEmptyWithoutQuery(t *testing.T) {
	s, ms := newTeamMembershipTestServer()
	seedTeamBead(ms, "gd-team1", `{"team":"acme-prod","members":["oidc:alice@example.com"]}`)

	ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: "oidc", Subject: "alice@example.com"})
	got, err := s.TeamsOf(ctx)
	if err != nil {
		t.Fatalf("TeamsOf: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty result for an unresolved operator, got %v", got)
	}
	if len(ms.listFilters) != 0 {
		t.Fatalf("expected no type:team list query when the caller has no operator row, got %v", ms.listFilters)
	}
}

func TestTeamsOf_CacheHitSkipsQuery(t *testing.T) {
	s, ms := newTeamMembershipTestServer()
	ms.operatorAliases = map[string]int64{"oidc\x00alice@example.com": 42}
	seedTeamBead(ms, "gd-team1", `{"team":"acme-prod","members":["oidc:alice@example.com"]}`)

	ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: "oidc", Subject: "alice@example.com"})
	if _, err := s.TeamsOf(ctx); err != nil {
		t.Fatalf("first TeamsOf: %v", err)
	}
	if len(ms.listFilters) != 1 {
		t.Fatalf("expected 1 list query after first resolution, got %d", len(ms.listFilters))
	}

	// Mutate the backing store directly: a cache hit must not observe this,
	// proving the second call is served from cache rather than re-querying.
	seedTeamBead(ms, "gd-team2", `{"team":"newly-added","members":["oidc:alice@example.com"]}`)

	got, err := s.TeamsOf(ctx)
	if err != nil {
		t.Fatalf("second TeamsOf: %v", err)
	}
	if len(ms.listFilters) != 1 {
		t.Fatalf("expected cache hit to perform no query, but store was listed %d times", len(ms.listFilters))
	}
	if len(got) != 1 || got[0] != "gd-team1" {
		t.Fatalf("expected cached result [gd-team1], got %v", got)
	}
}
