package glass

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"gardenio/controller/internal/oidcauth"
)

// sharedFixturePath is the tier fixture coopmux's Rust test also asserts
// (coop/crates/mux/src/transport/auth_tests.rs). It lives on the Rust side
// because auth.rs is the source of truth; this package is the port.
var sharedFixturePath = filepath.Join("..", "..", "..", "..", "coop", "crates", "mux", "tests", "fixtures", "edge_tiers.json")

type tierFixture struct {
	Routes []struct {
		Method string `json:"method"`
		Path   string `json:"path"`
		Role   string `json:"role"`
	} `json:"routes"`
	SessionFrames []struct {
		Body string `json:"body"`
		Role string `json:"role"`
	} `json:"session_frames"`
}

func loadSharedFixture(t *testing.T) tierFixture {
	t.Helper()
	raw, err := os.ReadFile(sharedFixturePath)
	if err != nil {
		t.Fatalf("shared tier fixture missing: %v (the Go and Rust tier tables are pinned by the same file; do not skip)", err)
	}
	var fx tierFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("parse %s: %v", sharedFixturePath, err)
	}
	if len(fx.Routes) == 0 || len(fx.SessionFrames) == 0 {
		t.Fatalf("fixture has %d routes and %d frames; both must be non-empty", len(fx.Routes), len(fx.SessionFrames))
	}
	return fx
}

// TestTiersMatchSharedFixture pins RouteTier and SessionFrameTier to the
// fixture coopmux asserts too, so the two tables cannot drift.
func TestTiersMatchSharedFixture(t *testing.T) {
	fx := loadSharedFixture(t)
	for _, c := range fx.Routes {
		if got := RouteTier(c.Method, c.Path).Role(); got != c.Role {
			t.Errorf("RouteTier(%q, %q) = %s, fixture says %s", c.Method, c.Path, got, c.Role)
		}
	}
	for _, c := range fx.SessionFrames {
		if got := SessionFrameTier([]byte(c.Body)).Role(); got != c.Role {
			t.Errorf("SessionFrameTier(%q) = %s, fixture says %s", c.Body, got, c.Role)
		}
	}
}

// TestSharedFixtureCoversEveryTier guards the fixture itself: a table edit
// that drops a whole tier from either side would otherwise pass trivially.
func TestSharedFixtureCoversEveryTier(t *testing.T) {
	fx := loadSharedFixture(t)
	for _, table := range []struct {
		name string
		seen map[string]bool
	}{
		{"routes", map[string]bool{}},
		{"session_frames", map[string]bool{}},
	} {
		if table.name == "routes" {
			for _, c := range fx.Routes {
				table.seen[c.Role] = true
			}
		} else {
			for _, c := range fx.SessionFrames {
				table.seen[c.Role] = true
			}
		}
		for _, tier := range []Tier{TierViewer, TierOperator, TierAdmin} {
			if !table.seen[tier.Role()] {
				t.Errorf("fixture %s has no %s case", table.name, tier.Role())
			}
		}
		for role := range table.seen {
			if _, ok := TierForRole(role); !ok {
				t.Errorf("fixture %s names unknown role %q", table.name, role)
			}
		}
	}
}

func TestTierRolesAndRank(t *testing.T) {
	cases := []struct {
		tier Tier
		role string
	}{
		{TierViewer, oidcauth.RoleViewer},
		{TierOperator, oidcauth.RoleOperator},
		{TierAdmin, oidcauth.RoleAdmin},
	}
	for _, c := range cases {
		if c.tier.Role() != c.role || c.tier.String() != c.role {
			t.Errorf("%d.Role() = %s, want %s", int(c.tier), c.tier.Role(), c.role)
		}
		back, ok := TierForRole(c.role)
		if !ok || back != c.tier {
			t.Errorf("TierForRole(%s) = %v, %v; want %v, true", c.role, back, ok, c.tier)
		}
	}
	if _, ok := TierForRole(oidcauth.RoleProjectMember); ok {
		t.Error("project member is not a tier")
	}
	if _, ok := TierForRole(""); ok {
		t.Error("empty role is not a tier")
	}
	if Tier(0).Role() != oidcauth.RoleAdmin || Tier(99).Role() != oidcauth.RoleAdmin {
		t.Error("out-of-range tiers must fail closed to admin")
	}
	if !(TierViewer < TierOperator && TierOperator < TierAdmin) {
		t.Error("tiers must order viewer < operator < admin")
	}
}

func TestTierAllowsUsesTheRoleLadder(t *testing.T) {
	admin := []string{oidcauth.RoleAdmin}
	operator := []string{oidcauth.RoleOperator}
	viewer := []string{oidcauth.RoleViewer}
	member := []string{oidcauth.RoleProjectMember}
	none := []string(nil)

	type want struct {
		roles []string
		ok    bool
	}
	for _, c := range []struct {
		tier  Tier
		wants []want
	}{
		{TierViewer, []want{{admin, true}, {operator, true}, {viewer, true}, {member, false}, {none, false}}},
		{TierOperator, []want{{admin, true}, {operator, true}, {viewer, false}, {member, false}, {none, false}}},
		{TierAdmin, []want{{admin, true}, {operator, false}, {viewer, false}, {member, false}, {none, false}}},
	} {
		for _, w := range c.wants {
			if got := c.tier.Allows(w.roles); got != w.ok {
				t.Errorf("%s.Allows(%v) = %v, want %v", c.tier, w.roles, got, w.ok)
			}
		}
	}
}
