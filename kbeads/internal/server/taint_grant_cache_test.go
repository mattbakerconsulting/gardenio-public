package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// Tests for the per-principal grant-set and spawn-chain-root caches
// (taint_grant_cache.go, gd-eawnzZZEjV): a repeat gated read resolves no
// grants from the store, and every write that changes a grant set on this
// process is seen by the very next read.

// countingGrantStore counts the store calls grant resolution makes.
type countingGrantStore struct {
	*mockStore
	grantIDs atomic.Int64
	aliases  atomic.Int64
	beads    atomic.Int64
}

func (c *countingGrantStore) GetGrantedTaintIDs(ctx context.Context, kind string, principals []string) ([]int64, error) {
	c.grantIDs.Add(1)
	return c.mockStore.GetGrantedTaintIDs(ctx, kind, principals)
}

func (c *countingGrantStore) LookupOperatorAlias(ctx context.Context, kind, subject string) (int64, error) {
	c.aliases.Add(1)
	return c.mockStore.LookupOperatorAlias(ctx, kind, subject)
}

func (c *countingGrantStore) GetBead(ctx context.Context, id string) (*model.Bead, error) {
	c.beads.Add(1)
	return c.mockStore.GetBead(ctx, id)
}

func sortedGrants(t *testing.T, s *BeadsServer, ctx context.Context) []int64 {
	t.Helper()
	grants, err := s.grantsOf(ctx)
	if err != nil {
		t.Fatalf("grantsOf: %v", err)
	}
	grants = slices.Clone(grants)
	slices.Sort(grants)
	return grants
}

// A repeat grantsOf for the same human, agent or fleet returns the same set
// with no taint_grants, operator_aliases or spawned_by query.
func TestGrantsOf_RepeatReadHitsCache(t *testing.T) {
	ms := newMockStore()
	cs := &countingGrantStore{mockStore: ms}
	s := NewBeadsServer(cs, &events.NoopPublisher{})
	seedTaint(ms, 1, "own", "gd-other")
	seedTaint(ms, 2, "team-owned", "gd-team-y")
	seedTeamBead(ms, "gd-team-y", `{"members":["oidc:alice@example.com"]}`)
	seedOwnerAgentBead(t, ms, "root-agent", map[string]any{"operated_by": "oidc:alice@example.com"})
	seedOwnerAgentBead(t, ms, "leaf-agent", map[string]any{"spawned_by": "root-agent"})
	ms.operatorAliases = map[string]int64{"oidc\x00alice@example.com": 7}
	if err := ms.GrantTaint(context.Background(), &model.TaintGrant{TaintID: 1, PrincipalKind: model.PrincipalOperator, Principal: "7", GrantedBy: 1}); err != nil {
		t.Fatalf("seed grant: %v", err)
	}
	fleet := WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "gardenio"})

	for _, tc := range []struct {
		name string
		ctx  context.Context
		want []int64
	}{
		{"operator", asOIDC(context.Background(), "alice@example.com"), []int64{1, 2}},
		{"agent", staticAuthCtx("leaf-agent"), []int64{1, 2}},
		{"fleet", fleet, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sortedGrants(t, s, tc.ctx); !slices.Equal(got, tc.want) {
				t.Fatalf("first grantsOf = %v, want %v", got, tc.want)
			}
			grantIDs, aliases, beads := cs.grantIDs.Load(), cs.aliases.Load(), cs.beads.Load()
			for i := 0; i < 5; i++ {
				if got := sortedGrants(t, s, tc.ctx); !slices.Equal(got, tc.want) {
					t.Fatalf("repeat grantsOf = %v, want %v", got, tc.want)
				}
			}
			if d := cs.grantIDs.Load() - grantIDs; d != 0 {
				t.Errorf("repeat reads ran %d GetGrantedTaintIDs, want 0", d)
			}
			if d := cs.aliases.Load() - aliases; d != 0 {
				t.Errorf("repeat reads ran %d LookupOperatorAlias, want 0", d)
			}
			if d := cs.beads.Load() - beads; d != 0 {
				t.Errorf("repeat reads ran %d GetBead, want 0", d)
			}
		})
	}
}

// The same human with a different group claim is a different cache entry:
// a team matched on oidc_groups grants only the token that carries it.
func TestGrantsOf_OperatorKeyedByGroups(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 3, "group-team-owned", "gd-team-g")
	seedTeamBead(ms, "gd-team-g", `{"oidc_groups":["secret-readers"]}`)
	ms.operatorAliases = map[string]int64{"oidc\x00alice@example.com": 7}

	plain := asOIDC(context.Background(), "alice@example.com")
	if got := sortedGrants(t, s, plain); len(got) != 0 {
		t.Fatalf("without the group: grantsOf = %v, want none", got)
	}
	grouped := WithGroups(plain, []string{"secret-readers"})
	if got := sortedGrants(t, s, grouped); !slices.Equal(got, []int64{3}) {
		t.Fatalf("with the group: grantsOf = %v, want [3]", got)
	}
	if got := sortedGrants(t, s, plain); len(got) != 0 {
		t.Fatalf("without the group again: grantsOf = %v, want none", got)
	}
}

// A grant and a revoke through the handlers are seen by the next read.
func TestGrantsOf_GrantAndRevokeHandlersInvalidate(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 1, "acme-production", "gd-owner-team")
	fleet := WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "gardenio"})
	admin := WithRoles(asOIDC(context.Background(), "admin@example.com"), []string{RoleAdmin})
	ms.operatorAliases = map[string]int64{"oidc\x00admin@example.com": 3}

	if got := sortedGrants(t, s, fleet); len(got) != 0 {
		t.Fatalf("before grant: grantsOf = %v, want none", got)
	}

	body, _ := json.Marshal(grantTaintRequest{Principal: "fleet:gardenio"})
	req := httptest.NewRequest(http.MethodPost, "/v1/taints/acme-production/grants", bytes.NewReader(body))
	req.SetPathValue("name", "acme-production")
	rec := httptest.NewRecorder()
	s.handleGrantTaint(rec, req.WithContext(admin))
	if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
		t.Fatalf("grant: %d %s", rec.Code, rec.Body.String())
	}
	if got := sortedGrants(t, s, fleet); !slices.Equal(got, []int64{1}) {
		t.Fatalf("after grant: grantsOf = %v, want [1]", got)
	}

	req = httptest.NewRequest(http.MethodDelete, "/v1/taints/acme-production/grants/fleet:gardenio", nil)
	req.SetPathValue("name", "acme-production")
	req.SetPathValue("principal", "fleet:gardenio")
	rec = httptest.NewRecorder()
	s.handleRevokeTaint(rec, req.WithContext(admin))
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body.String())
	}
	if got := sortedGrants(t, s, fleet); len(got) != 0 {
		t.Fatalf("after revoke: grantsOf = %v, want none", got)
	}
}

// A team update published through recordAndPublish drops the members'
// cached grant sets, team membership included.
func TestGrantsOf_TeamUpdateInvalidates(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 2, "team-owned", "gd-team-y")
	seedTeamBead(ms, "gd-team-y", `{"members":["oidc:alice@example.com"]}`)
	ms.operatorAliases = map[string]int64{"oidc\x00alice@example.com": 7}
	alice := asOIDC(context.Background(), "alice@example.com")

	if got := sortedGrants(t, s, alice); !slices.Equal(got, []int64{2}) {
		t.Fatalf("member: grantsOf = %v, want [2]", got)
	}

	team := ms.beads["gd-team-y"]
	team.Fields = []byte(`{"members":[]}`)
	s.recordAndPublish(context.Background(), events.TopicBeadUpdated, team.ID, "", events.BeadUpdated{Bead: team})

	if got := sortedGrants(t, s, alice); len(got) != 0 {
		t.Fatalf("removed from team: grantsOf = %v, want none", got)
	}
}

// Assigning a tainted task to an agent whose grant set is cached gives the
// agent the grant on its next read; closing the agent takes it away.
func TestGrantsOf_AssignAndCloseInvalidateAgent(t *testing.T) {
	s, ms, _ := newTestServer()
	seedTaint(ms, 2, "customer-pii", "gd-owner-team")
	seedTaskTaint(ms, "gd-task2", 2, "customer-pii")
	ms.beads["gd-task2"] = &model.Bead{ID: "gd-task2", Type: "task", Kind: model.KindIssue, Title: "task2", Status: model.StatusOpen}
	ms.beads["gd-agent2"] = &model.Bead{ID: "gd-agent2", Type: "agent", Kind: model.KindConfig, Title: "agent2", Status: model.StatusOpen}
	ms.operatorAliases = map[string]int64{"oidc\x00bob@example.com": 9}
	agent := staticAuthCtx("gd-agent2")

	if got := sortedGrants(t, s, agent); len(got) != 0 {
		t.Fatalf("before assign: grantsOf = %v, want none", got)
	}

	bob := WithRoles(asOIDC(ctxAsTestSubject(), "bob@example.com"), []string{RoleAdmin})
	assignee := "gd-agent2"
	if _, err := s.updateBead(bob, "gd-task2", updateBeadInput{Assignee: &assignee}); err != nil {
		t.Fatalf("updateBead: %v", err)
	}
	if got := sortedGrants(t, s, agent); !slices.Equal(got, []int64{2}) {
		t.Fatalf("after assign: grantsOf = %v, want [2]", got)
	}

	closedAt := time.Now()
	s.afterBeadClosed(ctxAsTestSubject(), &model.Bead{ID: "gd-agent2", Type: "agent", Kind: model.KindConfig, Status: model.StatusClosed, ClosedAt: &closedAt})
	if got := sortedGrants(t, s, agent); len(got) != 0 {
		t.Fatalf("after agent close: grantsOf = %v, want none", got)
	}
}

// An invalidation that lands while a miss is resolving keeps that miss's
// (possibly pre-write) result out of the cache.
func TestPrincipalCache_InvalidationFencesInFlightLoad(t *testing.T) {
	c := newPrincipalCache[[]int64](time.Minute)
	stale, err := c.load("k", func() ([]int64, error) {
		c.invalidate("other")
		return []int64{1}, nil
	}, nil)
	if err != nil || !slices.Equal(stale, []int64{1}) {
		t.Fatalf("in-flight load = %v, %v", stale, err)
	}
	fresh, _ := c.load("k", func() ([]int64, error) { return []int64{2}, nil }, nil)
	if !slices.Equal(fresh, []int64{2}) {
		t.Fatalf("load after a fenced miss = %v, want the re-resolved [2]", fresh)
	}
	if got := c.LoaderCalls(); got != 2 {
		t.Fatalf("LoaderCalls = %d, want 2", got)
	}
}

// An entry expires after the TTL, and an uncacheable value is never kept.
func TestPrincipalCache_TTLAndCacheable(t *testing.T) {
	now := time.Unix(1000, 0)
	c := newPrincipalCache[string](time.Minute)
	c.now = func() time.Time { return now }
	calls := 0
	resolve := func() (string, error) { calls++; return "root", nil }

	c.load("a", resolve, nil)
	c.load("a", resolve, nil)
	if calls != 1 {
		t.Fatalf("within TTL: %d resolutions, want 1", calls)
	}
	now = now.Add(time.Minute)
	c.load("a", resolve, nil)
	if calls != 2 {
		t.Fatalf("after TTL: %d resolutions, want 2", calls)
	}

	empty := func() (string, error) { calls++; return "", nil }
	nonEmpty := func(v string) bool { return v != "" }
	c.load("b", empty, nonEmpty)
	c.load("b", empty, nonEmpty)
	if calls != 4 {
		t.Fatalf("uncacheable value: %d resolutions, want 4", calls)
	}
}

// A nil cache resolves every time instead of panicking (servers built as a
// struct literal in tests carry none).
func TestPrincipalCache_NilCachesNothing(t *testing.T) {
	var c *principalCache[[]int64]
	calls := 0
	for i := 0; i < 2; i++ {
		c.load("k", func() ([]int64, error) { calls++; return nil, nil }, nil)
	}
	c.invalidate()
	if calls != 2 {
		t.Fatalf("nil cache: %d resolutions, want 2", calls)
	}
}
