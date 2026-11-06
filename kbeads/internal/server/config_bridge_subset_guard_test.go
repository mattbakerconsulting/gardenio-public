package server

import "testing"

// This file closes the LAST leg of the schema-drift triangle.
//
// gardenio registers bead-type schemas in TWO places that look interchangeable
// and are not:
//
//	(a) kbeads/internal/server/config.go  builtinConfigs["type:<t>"]
//	    — the ONLY source resolveTypeConfig consults (builtins + the `configs`
//	      KV table). This is what field validation actually enforces.
//	(b) gardenio/controller/internal/bridge  configs()["type:<t>"], loaded from
//	    init_configs.yaml plus a few init_*.go entries
//	    — fed to EnsureConfigs, which writes a type=config BEAD. The daemon's
//	      type resolver NEVER reads a config bead (ka-0000001549).
//
// So adding a field to (b) alone is a NO-OP for validation, while LOOKING like
// a completed registration — a reviewer sees `{Name: "x", Type: "string"}` land
// in a file literally named init.go and reasonably concludes the field is
// registered. This has now produced three separate incidents:
//
//	ka-0000001549  type=dashboard creates 400'd "unknown bead type" — grafana-bridge
//	               registered the whole TYPE in (b) only.
//	ka-0000004509  successor / next_respawn_at / respawn_count / max_respawns —
//	               idle-TTL park CloseBead 400'd, agents terminally despawned
//	               instead of parking.
//	ka-0000000811  prior_wrapup / error_category / error_detail — MR !7200 added
//	               prior_wrapup to (b) and the bead closed "verified on main", but
//	               every revive-in-place PATCH kept 400ing for days afterward,
//	               cold-minting thread agents and losing their conversations.
//
// The existing {Project,Role}Schema_ParserDrift guards in config_driftguard_test.go
// cover controller-side field READERS. This one covers controller-side field
// DECLARATIONS: every field the bridge declares must also exist in the daemon
// builtin. The invariant is deliberately one-directional (subset, not equality):
// the daemon legitimately declares fields the bridge never mentions (jira/gitlab
// bridge fields, controller-only telemetry), and an extra builtin field is
// harmless — it widens the accept-set. It is the bridge-only field that is the
// bug, every time.

// bridgeGuardedTypes are the type:<name> keys whose bridge declaration must be a
// subset of the daemon builtin. Scoped to the types whose fields the controller
// actively writes at runtime (where a missing registration 400s a live write)
// rather than every type the bridge happens to seed — those are the ones with an
// incident history. trigger and message joined from their own one-type parity
// tests (gd-cTYdroErdb). Extend this list when a new type joins that set.
var bridgeGuardedTypes = []string{"agent", "project", "role", "session", "trigger", "message"}

// TestBridgeTypeConfig_SubsetOfBuiltin asserts that every field the gardenio
// bridge declares for a guarded type is also declared in the daemon's builtin
// schema for that type.
//
// The bridge fields come from parseBridgeInitGoFields, which reads
// bridge/init_configs.yaml and every bridge/init*.go file, so a type split out
// of configs() (type:message lives in init_message.go) is still checked.
//
// A failure here means someone registered a field in the bridge only. That
// registration writes a config BEAD, which resolveTypeConfig never reads, so the
// field is NOT registered as far as validation is concerned and every typed write
// carrying it 400s "unknown field" — taking down the whole PATCH, not just that
// key. Fix by adding the field to builtinConfigs in
// kbeads/internal/server/config.go; keeping it in the bridge too is harmless.
func TestBridgeTypeConfig_SubsetOfBuiltin(t *testing.T) {
	for _, typeName := range bridgeGuardedTypes {
		t.Run(typeName, func(t *testing.T) {
			bridgeFields := parseBridgeInitGoFields(t, "type:"+typeName)
			if len(bridgeFields) == 0 {
				t.Fatalf("extracted zero FieldDef names for type:%s from bridge/init_configs.yaml or init*.go — "+
					"the bridge's configs() idiom changed (or the type was removed); update "+
					"parseBridgeInitGoFields or bridgeGuardedTypes", typeName)
			}

			tc := parseBuiltinSchema(t, typeName)
			declared := make(map[string]struct{}, len(tc.Fields))
			for _, f := range tc.Fields {
				declared[f.Name] = struct{}{}
			}

			var missing []string
			for _, name := range bridgeFields {
				if _, ok := declared[name]; !ok {
					missing = append(missing, name)
				}
			}
			if len(missing) > 0 {
				t.Errorf("the bridge (init_configs.yaml / init*.go) declares type:%s field(s) %v that the daemon "+
					"builtin omits — registering a field there writes a config BEAD, "+
					"which resolveTypeConfig NEVER reads, so every typed write carrying "+
					"one of these 400s \"unknown field\" and fails the WHOLE PATCH "+
					"(ka-0000000811, ka-0000001549). Add them to builtinConfigs[\"type:%s\"] "+
					"in kbeads/internal/server/config.go.", typeName, missing, typeName)
			}
		})
	}
}

// ciWaitAgentFields are the type:agent keys `gb wait ci` stamps on the calling
// agent's own bead (gardenio plan wake-on-ci-completion.md §2.1, task
// gd-AwuRDezONo; beadsapi.CIWaitFields on the gardenio side). Hand-listed because
// kbeads does not import the gardenio module.
var ciWaitAgentFields = []string{
	"ci_wait_state", "ci_wait_key", "ci_wait_workflows",
	"ci_wait_task", "ci_wait_deadline", "ci_wait_result",
	"ci_wait_ledger", // the bridge resolver's run ledger (gd-WKrO492yYP)
}

// TestBuiltinAgent_DeclaresCIWaitFields pins the ci_wait_* keys on the builtin
// type:agent schema, and on the bridge copy via the same AST extraction the
// subset guard uses, so the CI wait registration cannot regress in either file.
func TestBuiltinAgent_DeclaresCIWaitFields(t *testing.T) {
	tc := parseBuiltinSchema(t, "agent")
	builtin := make(map[string]string, len(tc.Fields))
	for _, f := range tc.Fields {
		builtin[f.Name] = string(f.Type)
	}
	bridge := make(map[string]struct{})
	for _, name := range parseBridgeInitGoFields(t, "type:agent") {
		bridge[name] = struct{}{}
	}
	for _, name := range ciWaitAgentFields {
		if typ, ok := builtin[name]; !ok {
			t.Errorf("builtin type:agent omits %q", name)
		} else if typ != "string" {
			t.Errorf("builtin type:agent %q has type %q, want string", name, typ)
		}
		if _, ok := bridge[name]; !ok {
			t.Errorf("bridge init_configs.yaml type:agent omits %q", name)
		}
	}
}

// TestBuiltinSession_DeclaresInterruptFields pins the interrupt keys that
// CloseSessionWithShutdown stamps on a session bead (gd-3HAERLWMLu) on the
// builtin type:session schema and on the bridge seed. type:session is strict,
// so an undeclared key 400s the whole close and loses ended_at/exit_code/usage.
func TestBuiltinSession_DeclaresInterruptFields(t *testing.T) {
	want := map[string]string{"interrupt_count": "integer", "last_interrupt_at": "string"}
	builtin := make(map[string]string)
	for _, f := range parseBuiltinSchema(t, "session").Fields {
		builtin[f.Name] = string(f.Type)
	}
	bridge := make(map[string]struct{})
	for _, name := range parseBridgeInitGoFields(t, "type:session") {
		bridge[name] = struct{}{}
	}
	for name, typ := range want {
		if got, ok := builtin[name]; !ok {
			t.Errorf("builtin type:session omits %q", name)
		} else if got != typ {
			t.Errorf("builtin type:session %q has type %q, want %s", name, got, typ)
		}
		if _, ok := bridge[name]; !ok {
			t.Errorf("bridge init_configs.yaml type:session omits %q", name)
		}
	}
}
