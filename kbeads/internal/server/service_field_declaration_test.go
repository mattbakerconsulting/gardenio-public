package server

// Service-owned field DECLARATION, derived from the builtin bead-type schemas
// (epic ka-0000005214).
//
// THE RECURRING DEFECT THIS CLOSES. Service identities legitimately own
// specific fields on beads they did not create — a bridge stamping a trigger's
// fire-state, the scheduler recording run counts, the acme-scif portal
// recording a Gate-1 approval. The ownership guard (authorizeOwnership) has no
// field vocabulary at all, so this is expressed as a PRE-GATE:
// patchOnlyChangesServiceManagedFields (beads.go) decides whether
// authorizeOwnership runs, from a hand-maintained map of type → field keys.
//
// That map is a SECOND copy of a fact whose first copy is the stamp helper that
// writes those fields, with nothing binding them. They drifted, in prod, twice
// in two days:
//
//   - ka-0000004245: commit 6a8511bce folded last_outcome / last_outcome_at
//     into the trigger `last_checked` patch. The bypass is ALL-OR-NOTHING, so
//     one unlisted key sent the whole write to authorizeOwnership, which denied
//     it. Three prod Jira intake lanes ran with frozen liveness signals for 11
//     days while enabled=true read healthy, emitting ~180 denial audit
//     beads/hour (~51k total).
//   - ka-0000001628: the acme-scif portal is structurally a THIRD identity
//     (four-eyes requires approver != requester, so the portal owns neither
//     side) and every Gate-1 approval 403'd.
//
// THE KEY EVIDENCE, and why the declaration belongs on the schema. Commit
// 6a8511bce DID update the type:trigger schema in config.go — exactly the two
// lines declaring last_outcome and last_outcome_at — and did NOT update the
// allowlist. The author reliably updated the schema because there is a forcing
// function: an undeclared field 400s "unknown field" on the very first write,
// loudly and immediately (and TestBuiltin*FieldsDeclared pins it). The
// allowlist has no forcing function, so it is the copy that rots. Hanging the
// PERMISSION off the declaration the writer is already compelled to maintain
// collapses two places into one, and makes 6a8511bce correct by construction.
//
// WHY NOT THE STORE-RESOLVED TypeConfig. resolveTypeConfig prefers a store
// override over the builtin. Store configs are written through PUT
// /v1/configs/{key...}, which is requireRole(RoleAdmin) — and requireRole
// short-circuits every NON-OIDC caller (interceptors.go: if authMethod !=
// authMethodOIDC { handler(); return }), so any fleet token can rewrite any
// type config today (the separate open hole ka-0000004303). Deriving
// permission from the store copy would therefore let any bridge declare itself
// the owner of any field — including fields.requested_by, the input to the
// acme-scif four-eyes check. So this file reads builtinConfigs ONLY. A store
// override can still add fields for VALIDATION; it can never grant a write
// permission. Pinned by TestServiceOwnedDeclarationIgnoresStoreOverride.
//
// WHERE IT IS ENFORCED. The daemon still gates on the hand-maintained
// serviceManagedFieldsByType (beads.go); the derivation below lives with the
// tests, and TestServiceOwnedDeclarationMatchesAllowlist fails the build when
// the two disagree, so the allowlist cannot rot silently.

import (
	"encoding/json"
	"sync"
	"testing"

	"gardenio/kbeads/internal/model"
)

// serviceOwnedAnySubject is the ServiceOwnedBy entry meaning "any
// authenticated caller", i.e. the field is unscoped observability state. It
// reproduces the pre-existing semantics of serviceManagedFieldsByType, which
// takes no identity into account whatsoever
// (patchOnlyChangesServiceManagedFields has no ctx parameter).
//
// It is deliberately spelled as an explicit marker rather than an empty list,
// so that "unscoped" is a decision someone wrote down rather than an omission.
// New declarations should prefer naming a concrete subject — the acme-scif
// approval fields are the model (ka-0000001628).
const serviceOwnedAnySubject = "*"

var (
	serviceOwnedOnce   sync.Once
	serviceOwnedByType map[model.BeadType]map[string][]string
)

// serviceOwnedFieldsFromBuiltins returns, for each bead type, the set of
// service-owned field names declared in that type's BUILTIN schema, mapped to
// the subjects allowed to stamp them.
//
// Read from builtinConfigs (in-code) and never from the store — see the file
// header for why that distinction is load-bearing for security.
//
// The result is computed once and must be treated as read-only.
func serviceOwnedFieldsFromBuiltins() map[model.BeadType]map[string][]string {
	serviceOwnedOnce.Do(func() {
		serviceOwnedByType = buildServiceOwnedFields(builtinConfigs)
	})
	return serviceOwnedByType
}

// buildServiceOwnedFields is the pure, testable core of
// serviceOwnedFieldsFromBuiltins. Exposed separately so a test can feed it a
// synthetic config map (including a would-be store override) without mutating
// package state.
func buildServiceOwnedFields(configs map[string]*model.Config) map[model.BeadType]map[string][]string {
	out := map[model.BeadType]map[string][]string{}
	for key, cfg := range configs {
		if cfg == nil {
			continue
		}
		typeName, ok := typeNameFromConfigKey(key)
		if !ok {
			continue
		}
		var tc model.TypeConfig
		if err := json.Unmarshal(cfg.Value, &tc); err != nil {
			// A malformed builtin is a programming error caught by other
			// tests; fail CLOSED here by contributing no permissions.
			continue
		}
		for _, fd := range tc.Fields {
			if len(fd.ServiceOwnedBy) == 0 || fd.Name == "" {
				continue
			}
			subjects := make([]string, 0, len(fd.ServiceOwnedBy))
			for _, s := range fd.ServiceOwnedBy {
				if s != "" {
					subjects = append(subjects, s)
				}
			}
			if len(subjects) == 0 {
				continue
			}
			if out[model.BeadType(typeName)] == nil {
				out[model.BeadType(typeName)] = map[string][]string{}
			}
			out[model.BeadType(typeName)][fd.Name] = subjects
		}
	}
	return out
}

// typeNameFromConfigKey extracts "trigger" from "type:trigger". Returns false
// for any key that is not a bead-type config (e.g. "authz-denied-text").
func typeNameFromConfigKey(key string) (string, bool) {
	const prefix = "type:"
	if len(key) <= len(prefix) || key[:len(prefix)] != prefix {
		return "", false
	}
	return key[len(prefix):], true
}

// declaredFieldNames returns every field name declared on a builtin bead type.
func declaredFieldNames(t *testing.T, beadType model.BeadType) map[string]bool {
	t.Helper()
	cfg := builtinConfigs["type:"+string(beadType)]
	if cfg == nil {
		return nil
	}
	var tc model.TypeConfig
	if err := json.Unmarshal(cfg.Value, &tc); err != nil {
		t.Fatalf("builtin type:%s is not valid JSON: %v", beadType, err)
	}
	out := map[string]bool{}
	for _, fd := range tc.Fields {
		out[fd.Name] = true
	}
	return out
}

// TestServiceOwnedDeclarationMatchesAllowlist is the GENERIC drift guard for
// epic ka-0000005214 — the whole point of the schema declaration.
//
// It replaces the per-instance, opt-in shape of the !7708 guard
// (TestTriggerFireStateAllowlistMatchesEngineStamps, which pins a THIRD
// hand-written copy of the trigger field set in its own `want` map). This test
// needs no per-type maintenance: it asserts that the two places that must
// agree — the `service_owned_by` declarations on the builtin bead-type schemas,
// and the serviceManagedFieldsByType allowlist that actually grants the
// ownership bypass — agree for EVERY bead type, in both directions.
//
// Instance 3 (ka-0000001628, the acme-scif approval portal) shipped with no
// drift guard at all because nobody knew to write one. Under this test, a new
// service-owned field is covered the moment it is declared.
//
// IF THIS TEST FAILS, the two sides have drifted. Fix by making them agree:
//   - declared but not allowlisted → the service CANNOT write it; the bypass
//     is all-or-nothing so the whole stamp patch will be ownership-denied
//     (that is verbatim the ka-0000004245 outage). Add the key to
//     serviceManagedFieldsByType in beads.go.
//   - allowlisted but not declared → the permission is unreachable or
//     undocumented. Add `"service_owned_by": [...]` to the field in
//     config.go, or drop the dead allowlist entry.
func TestServiceOwnedDeclarationMatchesAllowlist(t *testing.T) {
	declared := serviceOwnedFieldsFromBuiltins()

	types := map[model.BeadType]bool{}
	for bt := range declared {
		types[bt] = true
	}
	for bt := range serviceManagedFieldsByType {
		types[bt] = true
	}

	for beadType := range types {
		beadType := beadType
		t.Run(string(beadType), func(t *testing.T) {
			declaredSet := map[string]bool{}
			for name := range declared[beadType] {
				declaredSet[name] = true
			}
			allowedSet := map[string]bool{}
			for _, name := range serviceManagedFieldsByType[beadType] {
				allowedSet[name] = true
			}

			for name := range declaredSet {
				if !allowedSet[name] {
					t.Errorf("type:%s declares field %q as service_owned_by but "+
						"serviceManagedFieldsByType[%q] omits it — the ownership bypass is "+
						"ALL-OR-NOTHING, so every stamp patch containing this key will be "+
						"ownership-denied in full (the ka-0000004245 outage shape). "+
						"Add it to serviceManagedFieldsByType in beads.go.",
						beadType, name, beadType)
				}
			}
			for name := range allowedSet {
				if !declaredSet[name] {
					t.Errorf("serviceManagedFieldsByType[%q] allows %q but type:%s does not "+
						"declare it with \"service_owned_by\" — the permission and the schema "+
						"have drifted. Either annotate the field in config.go, or remove the "+
						"dead allowlist entry.",
						beadType, name, beadType)
				}
			}
		})
	}
}

// TestServiceOwnedFieldsAreRealDeclaredFields catches an allowlist or
// declaration entry that names a field the bead type does not have at all — a
// typo, or a field renamed on one side only. Such an entry is dead weight at
// best: an undeclared key 400s at field validation ("unknown field") long
// before authorization is consulted, so the permission can never be exercised
// and its presence misleads the next reader into thinking a write is
// permitted.
//
// This is how the dead serviceManagedFieldsByType["schedule"]["last_checked"]
// entry was found (ka-0000005214): type:schedule never declared a
// `last_checked` field, and the only producer of `last_checked` anywhere is
// TriggerEngine.stampCheckedOutcome, which writes it to TRIGGER beads.
func TestServiceOwnedFieldsAreRealDeclaredFields(t *testing.T) {
	for beadType, fields := range serviceManagedFieldsByType {
		declared := declaredFieldNames(t, beadType)
		if declared == nil {
			t.Errorf("serviceManagedFieldsByType has an entry for %q but there is no "+
				"builtin type:%s schema", beadType, beadType)
			continue
		}
		for _, name := range fields {
			if !declared[name] {
				t.Errorf("serviceManagedFieldsByType[%q] names %q, which type:%s does not "+
					"declare as a field — a write of that key 400s \"unknown field\" before "+
					"authz runs, so this permission is unreachable. Fix the name or drop it.",
					beadType, name, beadType)
			}
		}
	}
}

// TestRequestedByIsNeverServiceOwned is the non-negotiable four-eyes guard for
// epic ka-0000005214, and it is designed to FAIL if a future change makes
// `requested_by` writable by a service under ANY of the mechanisms in play.
//
// WHY. fields.requested_by on a type=investigation-request bead is the INPUT to
// the acme-scif Gate-1 four-eyes check: decideRequestCore reads
// bead.Fields["requested_by"] and the control is "approver != requester". It is
// server-stamped from the authenticated identity at create time and never
// honored from client input. A service identity able to rewrite it could set
// requester to anyone, make approver != requester true by fiat, and
// self-approve a run against real PHI-bearing clinical data — defeating the
// exact control it exists to enforce. The approval portal (bridge:acme-scif)
// must be able to stamp the DECISION (approval_status, approved_by, ...) and
// must never be able to restate the QUESTION.
//
// The assertions are deliberately redundant across every surface a grant could
// appear on, because the epic's whole lesson is that one mechanism's guard does
// not cover another mechanism.
func TestRequestedByIsNeverServiceOwned(t *testing.T) {
	const guarded = "requested_by"

	t.Run("not declared service_owned_by on any bead type", func(t *testing.T) {
		for beadType, fields := range serviceOwnedFieldsFromBuiltins() {
			if subjects, ok := fields[guarded]; ok {
				t.Errorf("type:%s declares %q as service_owned_by %v — FORBIDDEN. "+
					"requested_by is the input to the acme-scif four-eyes check "+
					"(approver != requester); a service able to write it can forge the "+
					"requester and self-approve a PHI-bearing run. Remove the annotation.",
					beadType, guarded, subjects)
			}
		}
	})

	t.Run("not in serviceManagedFieldsByType for any bead type", func(t *testing.T) {
		for beadType, fields := range serviceManagedFieldsByType {
			for _, name := range fields {
				if name == guarded {
					t.Errorf("serviceManagedFieldsByType[%q] contains %q — FORBIDDEN, see "+
						"this test's doc comment. The ownership bypass would let a service "+
						"rewrite the four-eyes input on a bead it does not own.",
						beadType, guarded)
				}
			}
		}
	})

	// The field must still EXIST on the request schema (it is read by the
	// four-eyes check) — so this test cannot be trivially satisfied by
	// deleting it.
	t.Run("still declared as a plain field on investigation-request", func(t *testing.T) {
		declared := declaredFieldNames(t, "investigation-request")
		if !declared[guarded] {
			t.Fatalf("type:investigation-request no longer declares %q at all — the "+
				"four-eyes check reads this field; it must exist, just never be "+
				"service-owned", guarded)
		}
	})
}

// TestServiceOwnedDeclarationIgnoresStoreOverride pins the security property
// that makes it safe to derive a WRITE PERMISSION from a bead-type schema.
//
// resolveTypeConfig prefers a store-resident "type:<name>" config over the
// builtin. Store configs are written through PUT /v1/configs/{key...}, gated by
// requireRole(RoleAdmin) — and requireRole short-circuits every non-OIDC
// caller (interceptors.go), so any fleet token can rewrite any type config
// (ka-0000004303, a separate open hole this design must not lean on). If the
// declaration were read through resolveTypeConfig, a bridge token could PUT
// itself a grant on any field — requested_by included — and quietly defeat
// four-eyes.
//
// So the derivation reads builtinConfigs only. This test proves a
// store-shaped config carrying a hostile grant contributes nothing.
func TestServiceOwnedDeclarationIgnoresStoreOverride(t *testing.T) {
	hostile := map[string]*model.Config{
		"type:investigation-request": {
			Key: "type:investigation-request",
			Value: json.RawMessage(`{
				"kind": "data",
				"fields": [
					{"name": "requested_by", "type": "string", "service_owned_by": ["bridge:evil"]},
					{"name": "approval_status", "type": "string", "service_owned_by": ["*"]}
				]
			}`),
		},
	}

	// The hostile map, parsed on its own, WOULD yield grants — proving the
	// test fixture is meaningful rather than vacuous.
	if got := buildServiceOwnedFields(hostile); len(got["investigation-request"]) != 2 {
		t.Fatalf("fixture is vacuous: expected the hostile config to parse into 2 grants, got %v",
			got["investigation-request"])
	}

	// The real derivation must not contain them, because it reads builtins.
	real := serviceOwnedFieldsFromBuiltins()["investigation-request"]
	if _, ok := real["requested_by"]; ok {
		t.Errorf("serviceOwnedFieldsFromBuiltins() granted requested_by — the derivation "+
			"is reading store/attacker-influenced config instead of builtinConfigs; got %v", real)
	}
}

// TestServiceOwnedFieldsAreNeverConfigOrContent guards the privilege boundary
// the serviceManagedFieldsByType comment states but nothing enforced: a
// service-owned field must be a record of what the service DID, never trigger
// CONFIGURATION or user-authored content. A service actor that could patch
// config on a bead it does not own is a privilege escalation, not an
// observability bypass — e.g. rewriting a trigger's `prompt`, `role` or
// `formula_id` turns a liveness stamp into arbitrary agent spawning, and
// repointing `jql` or `enabled` silently reshapes an intake lane.
func TestServiceOwnedFieldsAreNeverConfigOrContent(t *testing.T) {
	forbidden := map[model.BeadType][]string{
		"trigger": {
			"enabled", "jql", "cooldown_minutes", "prompt", "formula_id", "role",
			"gardener", "action", "project", "max_concurrent", "allowed_projects",
			"allowed_roles", "webhook_secret", "webhook_token", "conditions", "chain",
		},
		"schedule": {
			"cron", "prompt", "project", "role", "enabled", "max_concurrent",
			"run_budget", "max_runs", "timezone", "terminal_goal",
		},
		"agent": {
			"agent", "role", "project", "model", "image", "prompt", "coop_token",
		},
		"investigation-request": {
			// The whole request payload: what an approved run may READ.
			"prompt", "data_scope", "requested_by", "origin_bead",
		},
	}

	declared := serviceOwnedFieldsFromBuiltins()
	for beadType, keys := range forbidden {
		for _, key := range keys {
			if subjects, ok := declared[beadType][key]; ok {
				t.Errorf("type:%s declares CONFIG/CONTENT field %q as service_owned_by %v — "+
					"service-owned fields must record what a service DID, never what a bead "+
					"IS. This grant would let a service reshape a bead it does not own.",
					beadType, key, subjects)
			}
		}
	}
}

// TestServiceOwnedSubjectsAreWellFormed keeps the declaration honest: every
// grant must name at least one subject, and a subject must be either the
// explicit "*" marker or a concrete server-derived auth subject. An empty
// subject list is an omission masquerading as a grant.
func TestServiceOwnedSubjectsAreWellFormed(t *testing.T) {
	for beadType, fields := range serviceOwnedFieldsFromBuiltins() {
		for name, subjects := range fields {
			if len(subjects) == 0 {
				t.Errorf("type:%s field %q has an empty service_owned_by list", beadType, name)
				continue
			}
			for _, s := range subjects {
				if s == serviceOwnedAnySubject {
					continue
				}
				// A concrete subject must look like a server-derived one
				// ("bridge:<name>"), never a bare fleet name a caller could
				// spoof and never a wildcard pattern.
				if len(s) < 2 || s[len(s)-1] == '*' {
					t.Errorf("type:%s field %q declares suspicious subject %q — use the "+
						"explicit %q marker for unscoped, or a concrete server-derived "+
						"AuthIdentity.Subject such as \"bridge:acme-scif\"",
						beadType, name, s, serviceOwnedAnySubject)
				}
			}
		}
	}
}
