package server

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// gd-Bcqj2r4m7E: capability_tiers on type:role, with privileged derived from
// it. Every refusal below sits next to a write that succeeds, so a refusal
// cannot be a test that always passes.

// roleTierPair reads a stored role bead's capability_tiers and privileged.
func roleTierPair(t *testing.T, raw json.RawMessage) ([]string, bool) {
	t.Helper()
	var f map[string]any
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("decode fields %s: %v", raw, err)
	}
	tiers, ok := model.ParseCapabilityTiers(f[model.RoleCapabilityTiersField])
	if _, has := f[model.RoleCapabilityTiersField]; !has || !ok {
		t.Fatalf("capability_tiers missing or malformed in %s", raw)
	}
	priv, isBool := f[model.RolePrivilegedField].(bool)
	if !isBool {
		t.Fatalf("privileged missing or not a boolean in %s", raw)
	}
	return tiers, priv
}

func TestRoleSchemaCapabilityTiersMatchRegistry(t *testing.T) {
	var tc model.TypeConfig
	if err := json.Unmarshal(builtinConfigs["type:role"].Value, &tc); err != nil {
		t.Fatalf("decode type:role: %v", err)
	}
	for _, f := range tc.Fields {
		if f.Name != model.RoleCapabilityTiersField {
			continue
		}
		if f.Type != model.FieldTypeEnums {
			t.Fatalf("capability_tiers type = %q, want enum[]", f.Type)
		}
		if !reflect.DeepEqual(f.Values, model.CapabilityTiers) {
			t.Fatalf("schema values %v, registry %v", f.Values, model.CapabilityTiers)
		}
		return
	}
	t.Fatal("type:role has no capability_tiers field")
}

func TestCreateRoleBead_CapabilityTiersDerivePrivileged(t *testing.T) {
	cases := []struct {
		name     string
		fields   string
		wantTier []string
		wantPriv bool
	}{
		{"legacy privileged flag", `{"privileged":true}`, []string{"privileged"}, true},
		{"legacy unprivileged", `{"model":"opus"}`, []string{}, false},
		{"tiers decide", `{"capability_tiers":["privileged"]}`, []string{"privileged"}, true},
		{"restricted tier only", `{"capability_tiers":["lands-tenant"]}`, []string{"lands-tenant"}, false},
		{"both, agreeing", `{"capability_tiers":["lands-tenant","privileged"],"privileged":true}`, []string{"lands-tenant", "privileged"}, true},
		{"duplicates folded", `{"capability_tiers":["lands-tenant","lands-tenant"]}`, []string{"lands-tenant"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _ := newTestServer()
			b, err := s.createBead(adminOIDCCtx(), createBeadInput{Title: "r", Type: "role", Fields: json.RawMessage(tc.fields)})
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			tiers, priv := roleTierPair(t, b.Fields)
			if !reflect.DeepEqual(tiers, tc.wantTier) || priv != tc.wantPriv {
				t.Fatalf("got tiers=%v privileged=%t, want %v %t", tiers, priv, tc.wantTier, tc.wantPriv)
			}
		})
	}
}

func TestCreateRoleBead_CapabilityTiersRefused(t *testing.T) {
	for name, fields := range map[string]string{
		"privileged contradicts tiers": `{"capability_tiers":[],"privileged":true}`,
		"tiers contradict privileged":  `{"capability_tiers":["privileged"],"privileged":false}`,
		"unregistered tier":            `{"capability_tiers":["previleged"]}`,
		"not an array":                 `{"capability_tiers":"privileged"}`,
		// The privilege gate reads the derived flag: a restricted tier does
		// not license an elevated runtime field.
		"elevated field without the privileged tier": `{"capability_tiers":["lands-tenant"],"needs_k8s_api":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			s, _, _ := newTestServer()
			if _, err := s.createBead(adminOIDCCtx(), createBeadInput{Title: "r", Type: "role", Fields: json.RawMessage(fields)}); err == nil {
				t.Fatalf("create of %s accepted", fields)
			}
		})
	}
	// Control: the privileged tier alone licenses the elevated field.
	s, _, _ := newTestServer()
	if _, err := s.createBead(adminOIDCCtx(), createBeadInput{Title: "r", Type: "role", Fields: json.RawMessage(`{"capability_tiers":["privileged"],"needs_k8s_api":true}`)}); err != nil {
		t.Fatalf("privileged tier with needs_k8s_api refused: %v", err)
	}
}

func TestUpdateRoleBead_CapabilityTiersStayInStep(t *testing.T) {
	cases := []struct {
		name     string
		stored   string
		patch    string
		wantTier []string
		wantPriv bool
	}{
		// An unrelated write backfills a legacy bead from its flag.
		{"unrelated write backfills privileged", `{"privileged":true}`, `{"model":"opus"}`, []string{"privileged"}, true},
		{"unrelated write backfills unprivileged", `{"image":"x"}`, `{"model":"opus"}`, []string{}, false},
		// A caller that has not moved to tiers toggles only the privileged tier.
		{"privileged on keeps other tiers", `{"capability_tiers":["lands-tenant"],"privileged":false}`, `{"privileged":true}`, []string{"lands-tenant", "privileged"}, true},
		{"privileged off keeps other tiers", `{"capability_tiers":["lands-tenant","privileged"],"privileged":true}`, `{"privileged":false}`, []string{"lands-tenant"}, false},
		{"privileged null is off", `{"capability_tiers":["privileged"],"privileged":true}`, `{"privileged":null}`, []string{}, false},
		// The set decides when the patch names it.
		{"tiers add privileged", `{"capability_tiers":[],"privileged":false}`, `{"capability_tiers":["privileged"]}`, []string{"privileged"}, true},
		{"tiers drop privileged", `{"capability_tiers":["privileged"],"privileged":true}`, `{"capability_tiers":["lands-tenant"]}`, []string{"lands-tenant"}, false},
		{"tiers null empties the set", `{"capability_tiers":["privileged"],"privileged":true}`, `{"capability_tiers":null}`, []string{}, false},
		// A stored pair that drifted (written before this daemon) is repaired
		// from the set.
		{"drifted flag repaired from the set", `{"capability_tiers":["privileged"],"privileged":false}`, `{"model":"opus"}`, []string{"privileged"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, ms, _ := newTestServer()
			seedConfigBead(t, ms, "gd-role1", "role", tc.stored)
			b, err := s.updateBead(adminOIDCCtx(), "gd-role1", updateBeadInput{Fields: json.RawMessage(tc.patch)})
			if err != nil {
				t.Fatalf("update: %v", err)
			}
			tiers, priv := roleTierPair(t, b.Fields)
			if !reflect.DeepEqual(tiers, tc.wantTier) || priv != tc.wantPriv {
				t.Fatalf("got tiers=%v privileged=%t, want %v %t", tiers, priv, tc.wantTier, tc.wantPriv)
			}
		})
	}
}

func TestUpdateRoleBead_CapabilityTiersConflictRefused(t *testing.T) {
	const stored = `{"capability_tiers":["privileged"],"privileged":true}`
	s, ms, _ := newTestServer()
	seedConfigBead(t, ms, "gd-role1", "role", stored)
	_, err := s.updateBead(adminOIDCCtx(), "gd-role1", updateBeadInput{Fields: json.RawMessage(`{"capability_tiers":[],"privileged":true}`)})
	if err == nil || !strings.Contains(err.Error(), "contradicts") {
		t.Fatalf("contradicting patch: err = %v, want a contradiction refusal", err)
	}
	if got := string(ms.beads["gd-role1"].Fields); got != stored {
		t.Fatalf("fields mutated despite refusal: %s", got)
	}
	// Control: the same patch with the flag agreeing goes through.
	if _, err := s.updateBead(adminOIDCCtx(), "gd-role1", updateBeadInput{Fields: json.RawMessage(`{"capability_tiers":[],"privileged":false}`)}); err != nil {
		t.Fatalf("agreeing patch refused: %v", err)
	}
}

// capability_tiers is an authority field like privileged: only an admin (or an
// approved replay) changes it, and on a role every change counts.
func TestRoleBead_CapabilityTiers_MachineDenied(t *testing.T) {
	s, _, _ := newTestServer()
	for _, fields := range []string{`{"capability_tiers":["lands-tenant"]}`, `{"capability_tiers":["privileged"]}`} {
		_, err := s.createBead(machineUserCtx(), createBeadInput{Title: "steward", Type: "role", Fields: json.RawMessage(fields)})
		requireDenied(t, err)
	}
	// Control: the machine token's ordinary role create (no tier) still works,
	// though the daemon stamps an empty set on it.
	b, err := s.createBead(machineUserCtx(), createBeadInput{Title: "steward", Type: "role", Fields: json.RawMessage(`{"model":"opus"}`)})
	if err != nil {
		t.Fatalf("plain machine role create denied: %v", err)
	}
	if tiers, priv := roleTierPair(t, b.Fields); len(tiers) != 0 || priv {
		t.Fatalf("plain create stamped tiers=%v privileged=%t", tiers, priv)
	}
	if _, err := s.createBead(adminOIDCCtx(), createBeadInput{Title: "steward", Type: "role", Fields: json.RawMessage(`{"capability_tiers":["lands-tenant"]}`)}); err != nil {
		t.Fatalf("admin create denied: %v", err)
	}

	stored := json.RawMessage(`{"capability_tiers":["lands-tenant"],"privileged":false}`)
	for name, patch := range map[string]string{
		"add a tier":         `{"capability_tiers":["lands-tenant","privileged"]}`,
		"drop the only tier": `{"capability_tiers":[]}`,
		"delete the set":     `{"capability_tiers":null}`,
		"malformed":          `{"capability_tiers":"lands-tenant"}`,
	} {
		if d := s.authorizeAgentIdentityFields(machineUserCtx(), "update", "role", json.RawMessage(patch), stored); d == nil {
			t.Errorf("%s: machine update must be denied", name)
		}
	}
	// An unchanged set echoed back (any order) is not a change.
	if d := s.authorizeAgentIdentityFields(machineUserCtx(), "update", "role", json.RawMessage(`{"capability_tiers":["lands-tenant","lands-tenant"]}`), stored); d != nil {
		t.Fatalf("unchanged echo denied: %v", d)
	}
	if d := s.authorizeAgentIdentityFields(adminOIDCCtx(), "update", "role", json.RawMessage(`{"capability_tiers":[]}`), stored); d != nil {
		t.Fatalf("admin update denied: %v", d)
	}
}

// The single-field routes write one key straight to the store, so they refuse
// either half of the pair on a role, even for an admin.
func TestRoleBead_CapabilityTiers_FieldEndpointsRefused(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	seedConfigBead(t, ms, "gd-role1", "role", `{"capability_tiers":[],"privileged":false}`)
	seedConfigBead(t, ms, "gd-proj1", "project", projectBeadFieldsJSON)
	for _, k := range []string{model.RoleCapabilityTiersField, model.RolePrivilegedField} {
		if err := srv.mergeBeadFields(adminOIDCCtx(), "gd-role1", map[string]any{k: true}); err == nil {
			t.Errorf("admin single-field write of %s on a role accepted", k)
		}
	}
	if got := string(ms.beads["gd-role1"].Fields); got != `{"capability_tiers":[],"privileged":false}` {
		t.Fatalf("role fields mutated: %s", got)
	}
	// Controls: an unrelated key on the role, and the same key on another type.
	if err := srv.mergeBeadFields(adminOIDCCtx(), "gd-role1", map[string]any{"model": "opus"}); err != nil {
		t.Errorf("unrelated role field: %v", err)
	}
	if err := roleTierFieldEndpointError(ms.beads["gd-proj1"], model.RolePrivilegedField); err != nil {
		t.Errorf("project bead refused: %v", err)
	}
}
