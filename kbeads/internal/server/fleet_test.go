package server

import (
	"context"
	"testing"
	"time"
)

func TestParseFleetMap(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		want  map[string]string
	}{
		{"Empty", "", nil},
		{"Single", "tok1:fleet1", map[string]string{"tok1": "fleet1"}},
		{"Multiple", "tok1:fleet1,tok2:fleet2", map[string]string{"tok1": "fleet1", "tok2": "fleet2"}},
		{"WithSpaces", " tok1 : fleet1 , tok2 : fleet2 ", map[string]string{"tok1": "fleet1", "tok2": "fleet2"}},
		{"SkipInvalid", "tok1:fleet1,invalid,tok2:fleet2", map[string]string{"tok1": "fleet1", "tok2": "fleet2"}},
		{"SkipEmpty", "tok1:fleet1,:,:empty,empty:", map[string]string{"tok1": "fleet1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseFleetMap(tc.input)
			if tc.want == nil {
				if got != nil {
					t.Fatalf("expected nil, got %v", got)
				}
				return
			}
			if len(got) != len(tc.want) {
				t.Fatalf("expected %d entries, got %d: %v", len(tc.want), len(got), got)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Fatalf("expected %q=%q, got %q", k, v, got[k])
				}
			}
		})
	}
}

func TestFleetConfig_ResolveFleet(t *testing.T) {
	fc := NewFleetConfig("gardenio", map[string]string{"tok-qa": "qa-fleet", "tok-staging": "staging"})

	for _, tc := range []struct {
		token string
		want  string
	}{
		{"tok-qa", "qa-fleet"},
		{"tok-staging", "staging"},
		{"tok-unknown", "gardenio"},
		{"", "gardenio"},
	} {
		if got := fc.ResolveFleet(tc.token); got != tc.want {
			t.Errorf("ResolveFleet(%q) = %q, want %q", tc.token, got, tc.want)
		}
	}

	// Nil config.
	var nilCfg *FleetConfig
	if got := nilCfg.ResolveFleet("anything"); got != "default" {
		t.Errorf("nil.ResolveFleet() = %q, want 'default'", got)
	}

	// No default fleet.
	noDefault := NewFleetConfig("", map[string]string{"tok1": "fleet1"})
	if got := noDefault.ResolveFleet("unknown"); got != "default" {
		t.Errorf("noDefault.ResolveFleet('unknown') = %q, want 'default'", got)
	}
}

func TestFleetConfig_RegisterToken(t *testing.T) {
	fc := NewFleetConfig("gardenio", nil)

	// Initially no tokens.
	if fc.HasTokens() {
		t.Fatal("expected no tokens initially")
	}

	// Register a token.
	fc.RegisterToken("tok-new", "new-fleet")
	if !fc.HasToken("tok-new") {
		t.Fatal("expected token to be registered")
	}
	if got := fc.ResolveFleet("tok-new"); got != "new-fleet" {
		t.Errorf("ResolveFleet('tok-new') = %q, want 'new-fleet'", got)
	}
	if !fc.HasTokens() {
		t.Fatal("expected HasTokens to be true after registration")
	}

	// Register another token for the same fleet.
	fc.RegisterToken("tok-new2", "new-fleet")
	fleets := fc.ListBridgeTokens()
	if len(fleets) != 1 || fleets[0] != "new-fleet" {
		t.Fatalf("expected [new-fleet], got %v", fleets)
	}
}

func TestFleetConfig_RevokeBridgeToken(t *testing.T) {
	fc := NewFleetConfig("gardenio", map[string]string{
		"tok-qa":      "qa-fleet",
		"tok-staging": "staging",
	})

	// Revoke qa-fleet.
	removed := fc.RevokeBridgeToken("qa-fleet")
	if len(removed) != 1 || removed[0] != "tok-qa" {
		t.Fatalf("expected revoked tokens [tok-qa], got %v", removed)
	}
	if fc.HasToken("tok-qa") {
		t.Fatal("token should no longer be valid")
	}

	// Revoke nonexistent fleet.
	if removed := fc.RevokeBridgeToken("nonexistent"); len(removed) != 0 {
		t.Fatalf("expected revocation of nonexistent fleet to remove nothing, got %v", removed)
	}

	// staging still works.
	if !fc.HasToken("tok-staging") {
		t.Fatal("tok-staging should still be valid")
	}
}

func TestFleetConfig_ListBridgeTokens(t *testing.T) {
	fc := NewFleetConfig("gardenio", map[string]string{
		"tok1": "beta",
		"tok2": "alpha",
		"tok3": "alpha", // duplicate fleet, two tokens
	})

	fleets := fc.ListBridgeTokens()
	if len(fleets) != 2 {
		t.Fatalf("expected 2 unique fleets, got %d: %v", len(fleets), fleets)
	}
	// Sorted order.
	if fleets[0] != "alpha" || fleets[1] != "beta" {
		t.Fatalf("expected [alpha, beta], got %v", fleets)
	}

	// Nil config.
	var nilCfg *FleetConfig
	if got := nilCfg.ListBridgeTokens(); got != nil {
		t.Fatalf("expected nil for nil config, got %v", got)
	}
}

func TestFleetConfig_HasToken_Nil(t *testing.T) {
	var nilCfg *FleetConfig
	if nilCfg.HasToken("anything") {
		t.Fatal("nil config should not have any tokens")
	}
	if nilCfg.HasTokens() {
		t.Fatal("nil config should report no tokens")
	}
}

func TestFleetConfig_RegisterTokenWithScopes(t *testing.T) {
	fc := NewFleetConfig("gardenio", nil)

	// Register with scopes.
	fc.RegisterTokenWithScopes("tok-scoped", "scoped-fleet", "myproj",
		[]string{"agent", "config"}, []string{"create", "update"})

	if !fc.HasToken("tok-scoped") {
		t.Fatal("expected token to be registered")
	}
	if got := fc.ResolveFleet("tok-scoped"); got != "scoped-fleet" {
		t.Fatalf("ResolveFleet = %q, want 'scoped-fleet'", got)
	}
	if got := fc.ResolveProject("tok-scoped"); got != "myproj" {
		t.Fatalf("ResolveProject = %q, want 'myproj'", got)
	}

	types := fc.ResolveTypeScope("tok-scoped")
	if len(types) != 2 || types[0] != "agent" || types[1] != "config" {
		t.Fatalf("ResolveTypeScope = %v, want [agent config]", types)
	}

	ops := fc.ResolveOpScope("tok-scoped")
	if len(ops) != 2 || ops[0] != "create" || ops[1] != "update" {
		t.Fatalf("ResolveOpScope = %v, want [create update]", ops)
	}

	// Unscoped token returns nil.
	fc.RegisterToken("tok-plain", "plain-fleet")
	if got := fc.ResolveTypeScope("tok-plain"); got != nil {
		t.Fatalf("expected nil type scope for unscoped token, got %v", got)
	}
	if got := fc.ResolveOpScope("tok-plain"); got != nil {
		t.Fatalf("expected nil op scope for unscoped token, got %v", got)
	}

	// Unknown token returns nil.
	if got := fc.ResolveTypeScope("unknown"); got != nil {
		t.Fatalf("expected nil for unknown token, got %v", got)
	}
	if got := fc.ResolveOpScope("unknown"); got != nil {
		t.Fatalf("expected nil for unknown token, got %v", got)
	}

	// Nil config returns nil.
	var nilCfg *FleetConfig
	if got := nilCfg.ResolveTypeScope("anything"); got != nil {
		t.Fatalf("expected nil for nil config, got %v", got)
	}
	if got := nilCfg.ResolveOpScope("anything"); got != nil {
		t.Fatalf("expected nil for nil config, got %v", got)
	}
}

func TestTokenScopeAllows(t *testing.T) {
	for _, tc := range []struct {
		name  string
		scope []string
		value string
		want  bool
	}{
		{"NilScope", nil, "anything", true},
		{"EmptyScope", []string{}, "anything", true},
		{"Exact", []string{"agent", "config"}, "agent", true},
		{"NotInScope", []string{"agent", "config"}, "task", false},
		{"Wildcard", []string{"*"}, "anything", true},
		{"WildcardMixed", []string{"agent", "*"}, "task", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tokenScopeAllows(tc.scope, tc.value); got != tc.want {
				t.Fatalf("tokenScopeAllows(%v, %q) = %v, want %v", tc.scope, tc.value, got, tc.want)
			}
		})
	}
}

func TestTokenScopeContext(t *testing.T) {
	ctx := context.Background()

	// No scopes in context.
	if got := TokenTypeScopeFromContext(ctx); got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
	if got := TokenOpScopeFromContext(ctx); got != nil {
		t.Fatalf("expected nil, got %v", got)
	}

	// With scopes.
	ctx = WithTokenTypeScope(ctx, []string{"agent", "config"})
	ctx = WithTokenOpScope(ctx, []string{"create"})

	types := TokenTypeScopeFromContext(ctx)
	if len(types) != 2 || types[0] != "agent" || types[1] != "config" {
		t.Fatalf("TokenTypeScopeFromContext = %v, want [agent config]", types)
	}
	ops := TokenOpScopeFromContext(ctx)
	if len(ops) != 1 || ops[0] != "create" {
		t.Fatalf("TokenOpScopeFromContext = %v, want [create]", ops)
	}
}

func TestFleetConfig_RegisterTokenWithExpiry(t *testing.T) {
	fc := NewFleetConfig("gardenio", nil)

	now := time.Date(2026, 4, 29, 0, 0, 0, 0, time.UTC)
	fc.SetNowFunc(func() time.Time { return now })

	// Token with future expiry — not expired.
	future := now.Add(1 * time.Hour)
	fc.RegisterTokenWithExpiry("tok-future", "fleet-future", "", nil, nil, future)
	if fc.IsExpired("tok-future") {
		t.Fatal("token with future expiry should not be expired")
	}
	if got := fc.ResolveExpiresAt("tok-future"); !got.Equal(future) {
		t.Fatalf("ResolveExpiresAt = %v, want %v", got, future)
	}

	// Token with past expiry — expired.
	past := now.Add(-1 * time.Minute)
	fc.RegisterTokenWithExpiry("tok-past", "fleet-past", "", nil, nil, past)
	if !fc.IsExpired("tok-past") {
		t.Fatal("token with past expiry should be expired")
	}

	// Token with zero expiry — never expires (back-compat).
	fc.RegisterTokenWithExpiry("tok-zero", "fleet-zero", "", nil, nil, time.Time{})
	if fc.IsExpired("tok-zero") {
		t.Fatal("token with zero expiry should never expire")
	}
	if got := fc.ResolveExpiresAt("tok-zero"); !got.IsZero() {
		t.Fatalf("ResolveExpiresAt for zero token = %v, want zero", got)
	}

	// Tokens registered via the older RegisterToken / RegisterTokenWithScopes
	// must also report not-expired (zero ExpiresAt default).
	fc.RegisterToken("tok-plain", "fleet-plain")
	if fc.IsExpired("tok-plain") {
		t.Fatal("plain registered token should not be expired")
	}
	fc.RegisterTokenWithScopes("tok-scoped", "fleet-scoped", "", nil, nil)
	if fc.IsExpired("tok-scoped") {
		t.Fatal("scoped registered token should not be expired")
	}

	// Unknown tokens are not expired (auth path returns 401-invalid_token by
	// other means, not expiry).
	if fc.IsExpired("nope") {
		t.Fatal("unknown token should not be reported as expired")
	}

	// Nil config is safe.
	var nilCfg *FleetConfig
	if nilCfg.IsExpired("anything") {
		t.Fatal("nil config IsExpired must be false")
	}
	if got := nilCfg.ResolveExpiresAt("anything"); !got.IsZero() {
		t.Fatalf("nil config ResolveExpiresAt = %v, want zero", got)
	}
}

func TestFleetContext(t *testing.T) {
	// No fleet in context.
	if got := FleetFromContext(context.Background()); got != "default" {
		t.Fatalf("expected 'default', got %q", got)
	}

	// With fleet.
	ctx := WithFleet(context.Background(), "qa-fleet")
	if got := FleetFromContext(ctx); got != "qa-fleet" {
		t.Fatalf("expected 'qa-fleet', got %q", got)
	}

	// Empty fleet.
	ctx = WithFleet(context.Background(), "")
	if got := FleetFromContext(ctx); got != "default" {
		t.Fatalf("expected 'default' for empty, got %q", got)
	}
}

// TestFleetConfig_RegisterBridgeToken exercises the bridge token path:
// RegisterBridgeToken stores the fleet and subject for audit attribution.
// The old CrossFleetRead read-isolation gate was removed under epic
// ka-0000000871 / ka-0000003285 — all tokens now read across all fleets,
// so there is no per-token read flag to assert anymore.
func TestFleetConfig_RegisterBridgeToken(t *testing.T) {
	fc := NewFleetConfig("default", nil)

	fc.RegisterBridgeToken("bridge-tok", "bridge-fleet", "bridge:test")
	if got := fc.ResolveSubject("bridge-tok"); got != "bridge:test" {
		t.Errorf("bridge token subject = %q, want bridge:test", got)
	}
	if got := fc.ResolveFleet("bridge-tok"); got != "bridge-fleet" {
		t.Errorf("bridge token fleet = %q, want bridge-fleet", got)
	}
}

// TestFleetConfig_SetPrivilegedFleets is the regression guard for gd-ShWLbKKzGs:
// the live gitlab-bridge token is loaded from the bridge-tokens Secret via
// BEADS_FLEET_MAP ("token:fleet" pairs, no per-token privileged field), so it
// arrives through NewFleetConfig with Privileged=false. SetPrivilegedFleets must
// re-mark every already-registered token of a designated fleet, AND mark tokens
// registered afterwards (config-bead merge / rotator) — so the bridge actually
// gets the ownership bypass (ka-0000001946) instead of 403-ing on every
// produces/triggered-by dep + mr_url back-fill.
func TestFleetConfig_SetPrivilegedFleets(t *testing.T) {
	fc := NewFleetConfig("default", map[string]string{
		"glb-tok":   "gitlab-bridge",
		"slack-tok": "slack-bridge",
	})

	// Before designation, fleet-map tokens are NOT privileged (the bug).
	if fc.ResolvePrivileged("glb-tok") {
		t.Fatal("gitlab-bridge token unexpectedly privileged before SetPrivilegedFleets")
	}

	fc.SetPrivilegedFleets([]string{"gitlab-bridge", "  "}) // blank entries ignored

	// The pre-registered gitlab-bridge fleet-map token is now privileged.
	if !fc.ResolvePrivileged("glb-tok") {
		t.Error("gitlab-bridge fleet-map token should be privileged after SetPrivilegedFleets")
	}
	// A non-listed fleet stays ordinary.
	if fc.ResolvePrivileged("slack-tok") {
		t.Error("slack-bridge token must stay unprivileged")
	}

	// Tokens registered AFTER designation (e.g. config-bead merge) are marked
	// at registration time via put().
	fc.RegisterBridgeToken("glb-tok2", "gitlab-bridge", "bridge:gitlab")
	if !fc.ResolvePrivileged("glb-tok2") {
		t.Error("gitlab-bridge token registered after designation should be privileged")
	}
	fc.RegisterToken("slack-tok2", "slack-bridge")
	if fc.ResolvePrivileged("slack-tok2") {
		t.Error("slack-bridge token registered after designation must stay unprivileged")
	}

	// An explicit config-bead privileged:true still wins for any fleet even
	// when not in the designated set (back-compat with the rotator path).
	fc.RegisterTokenFull("explicit-tok", "jira-bridge", "", nil, nil, time.Time{}, "bridge:jira", true)
	if !fc.ResolvePrivileged("explicit-tok") {
		t.Error("explicit privileged:true registration must remain privileged")
	}
}

// TestFleetConfig_SetPrivilegedFleets_Nil guards the nil-receiver and empty-input
// paths so startup wiring can call it unconditionally.
func TestFleetConfig_SetPrivilegedFleets_Nil(t *testing.T) {
	var fc *FleetConfig
	fc.SetPrivilegedFleets([]string{"gitlab-bridge"}) // must not panic

	fc2 := NewFleetConfig("default", map[string]string{"tok": "gitlab-bridge"})
	fc2.SetPrivilegedFleets(nil) // empty set: nothing privileged
	if fc2.ResolvePrivileged("tok") {
		t.Error("no fleets designated → token must stay unprivileged")
	}
}
