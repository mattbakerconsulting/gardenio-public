package server

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/model"
)

func TestGRPCSetConfig(t *testing.T) {
	srv, _, ctx := testCtx(t)
	resp, err := srv.SetConfig(ctx, &beadsv1.SetConfigRequest{
		Key: "view:inbox", Value: []byte(`{"filter":{"status":["open"]}}`),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Config.Key != "view:inbox" {
		t.Fatalf("got key=%q", resp.Config.Key)
	}
}

func TestGRPCGetConfig(t *testing.T) {
	srv, ms, ctx := testCtx(t)
	ms.configs["view:inbox"] = &model.Config{Key: "view:inbox", Value: json.RawMessage(`{"filter":{"status":["open"]}}`)}

	resp, err := srv.GetConfig(ctx, &beadsv1.GetConfigRequest{Key: "view:inbox"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Config.Key != "view:inbox" {
		t.Fatalf("got key=%q", resp.Config.Key)
	}
}

func TestGRPCGetConfig_BuiltinFallback(t *testing.T) {
	srv, _, ctx := testCtx(t)
	resp, err := srv.GetConfig(ctx, &beadsv1.GetConfigRequest{Key: "view:ready"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Config.Key != "view:ready" {
		t.Fatalf("got key=%q", resp.Config.Key)
	}
}

func TestGRPCListConfigs(t *testing.T) {
	srv, ms, ctx := testCtx(t)
	ms.configs["view:inbox"] = &model.Config{Key: "view:inbox", Value: json.RawMessage(`{}`)}

	resp, err := srv.ListConfigs(ctx, &beadsv1.ListConfigsRequest{Namespace: "view"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 1 stored + 3 builtins (view:ready, view:users, view:audit:recent)
	if len(resp.Configs) != 4 {
		t.Fatalf("expected 4 configs, got %d", len(resp.Configs))
	}
}

func TestGRPCGetConfig_FleetTypeBuiltin(t *testing.T) {
	srv, _, ctx := testCtx(t)
	resp, err := srv.GetConfig(ctx, &beadsv1.GetConfigRequest{Key: "type:fleet"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Config.Key != "type:fleet" {
		t.Fatalf("got key=%q, want type:fleet", resp.Config.Key)
	}
	var tc struct {
		Kind   string `json:"kind"`
		Fields []struct {
			Name     string `json:"name"`
			Required bool   `json:"required"`
		} `json:"fields"`
	}
	if err := json.Unmarshal(resp.Config.Value, &tc); err != nil {
		t.Fatalf("unmarshal fleet type config: %v", err)
	}
	if tc.Kind != "config" {
		t.Fatalf("fleet type kind=%q, want config", tc.Kind)
	}
	if len(tc.Fields) != 6 {
		t.Fatalf("fleet type has %d fields, want 6", len(tc.Fields))
	}
	// namespace and token_secret must be required
	required := map[string]bool{}
	for _, f := range tc.Fields {
		if f.Required {
			required[f.Name] = true
		}
	}
	for _, name := range []string{"namespace", "token_secret"} {
		if !required[name] {
			t.Errorf("field %q should be required", name)
		}
	}
}

// TestResolveTypeConfig_TeamBuiltin asserts type:team is registered per ADR
// docs/adr/taints.md §8.1: config kind, admin-only writes, strict schema
// (unknown keys rejected — AllowAdditionalFields not set), only `team`
// required, accepting a bead that sets `team` alone (ka-ORaCUV7C6g).
func TestResolveTypeConfig_TeamBuiltin(t *testing.T) {
	srv, _, ctx := testCtx(t)
	tc, err := srv.resolveTypeConfig(ctx, "team")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tc == nil {
		t.Fatal("expected team type config from builtins, got nil")
	}
	if tc.Kind != model.KindConfig {
		t.Fatalf("team kind=%q, want config", tc.Kind)
	}
	if tc.WritePolicy == nil || tc.WritePolicy.MinRole != RoleAdmin {
		t.Fatalf("team write_policy.min_role=%v, want %q", tc.WritePolicy, RoleAdmin)
	}
	if tc.AllowAdditionalFields {
		t.Error("team schema should be strict (AllowAdditionalFields=false)")
	}

	fieldNames := map[string]bool{}
	var required []string
	for _, f := range tc.Fields {
		fieldNames[f.Name] = true
		if f.Required {
			required = append(required, f.Name)
		}
	}
	for _, name := range []string{"team", "display", "members", "oidc_groups", "owners", "projects"} {
		if !fieldNames[name] {
			t.Errorf("expected field %q in team type, not found", name)
		}
	}
	if len(required) != 1 || required[0] != "team" {
		t.Errorf("team required fields=%v, want exactly [team]", required)
	}

	// Missing `team` is rejected.
	if err := model.ValidateTypeFields(json.RawMessage(`{"display":"Platform"}`), tc); err == nil {
		t.Error("expected validation error for a team bead missing `team`")
	}
	// `team` alone is sufficient — every other field is optional.
	if err := model.ValidateTypeFields(json.RawMessage(`{"team":"acme-prod"}`), tc); err != nil {
		t.Errorf("expected `{\"team\":...}` alone to validate, got: %v", err)
	}
	// Unknown keys are rejected (strict schema).
	if err := model.ValidateTypeFields(json.RawMessage(`{"team":"acme-prod","bogus":true}`), tc); err == nil {
		t.Error("expected validation error for an unknown field on a strict schema")
	}
}

func TestResolveTypeConfig_AccountBuiltin(t *testing.T) {
	srv, _, ctx := testCtx(t)
	tc, err := srv.resolveTypeConfig(ctx, "account")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tc == nil {
		t.Fatal("expected account type config from builtins, got nil")
	}
	if tc.Kind != "config" {
		t.Fatalf("account kind=%q, want config", tc.Kind)
	}

	// Verify ratelimit-probe fields exist.
	fieldNames := map[string]bool{}
	for _, f := range tc.Fields {
		fieldNames[f.Name] = true
	}
	for _, name := range []string{
		"provider", "credential", "throttle_status", "utilization_5h",
		"utilization_7d", "representative_claim", "overage_status",
		"overage_disabled_reason", "probe_checked_at", "throttle_reset_at",
	} {
		if !fieldNames[name] {
			t.Errorf("expected field %q in account type, not found", name)
		}
	}
}

// TestResolveTypeConfig_AccountCredentialOptional pins that an account may
// be created without a credential. A land's account bills through its own
// LiteLLM key alone (gd-ApgYe740ps), and the provisioner creates it from a
// template whose credential is blank, so a required credential 400'd every
// land's purse step.
func TestResolveTypeConfig_AccountCredentialOptional(t *testing.T) {
	srv, _, ctx := testCtx(t)
	tc, err := srv.resolveTypeConfig(ctx, "account")
	if err != nil || tc == nil {
		t.Fatalf("resolveTypeConfig(account) = %v, %v", tc, err)
	}
	for _, f := range tc.Fields {
		if f.Name == "credential" && f.Required {
			t.Fatal("account credential is required; a LiteLLM-only land account cannot be created")
		}
	}
}

// TestResolveTypeConfig_RatingBuiltin asserts that the rating type from the
// human-ratings plan-of-record (gardenio/plans/beads-viewer-human-ratings.md)
// is registered with the right kind and required fields, so kbeads can store
// rating beads without a runtime SetConfig (gd-XPdJCTFzib).
func TestResolveTypeConfig_RatingBuiltin(t *testing.T) {
	srv, _, ctx := testCtx(t)

	tc, err := srv.resolveTypeConfig(ctx, "rating")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tc == nil {
		t.Fatal("expected rating type config, got nil")
	}
	if tc.Kind != "data" {
		t.Errorf("rating kind=%q, want data", tc.Kind)
	}

	byName := map[string]model.FieldDef{}
	for _, f := range tc.Fields {
		byName[f.Name] = f
	}

	for _, name := range []string{"rated_bead_id", "score", "axes", "tags", "rater"} {
		if _, ok := byName[name]; !ok {
			t.Errorf("rating type missing field %q", name)
		}
	}
	for _, name := range []string{"rated_bead_id", "score", "rater"} {
		if !byName[name].Required {
			t.Errorf("rating field %q should be required", name)
		}
	}
	score := byName["score"]
	if score.Type != model.FieldTypeInteger {
		t.Errorf("score type=%q, want integer", score.Type)
	}
	// T4a-2: relaxed from min=1 to min=0 to admit slack-feedback-only sentinel
	// (gd-8w1e4TfPCV / plans/beads-viewer-human-ratings-slack-fold.md §2, §3.3).
	if score.Min == nil || *score.Min != 0 {
		t.Errorf("score.min=%v, want 0", score.Min)
	}
	if score.Max == nil || *score.Max != 5 {
		t.Errorf("score.max=%v, want 5", score.Max)
	}

	// T4a-2: Slack-shaped optional fields (additive — pre-existing rating
	// rows without these fields stay valid).
	for _, name := range []string{
		"score_native", "source", "slack_channel", "slack_thread_ts",
		"slack_user_id", "feedback",
	} {
		fd, ok := byName[name]
		if !ok {
			t.Errorf("rating type missing slack-fold field %q", name)
			continue
		}
		if fd.Required {
			t.Errorf("rating field %q must be optional", name)
		}
	}
	if got := byName["score_native"].Type; got != model.FieldTypeInteger {
		t.Errorf("score_native type=%q, want integer", got)
	}
	for _, name := range []string{"source", "slack_channel", "slack_thread_ts", "slack_user_id", "feedback"} {
		if got := byName[name].Type; got != model.FieldTypeString {
			t.Errorf("rating.%s type=%q, want string", name, got)
		}
	}
}

// TestResolveTypeConfig_NeedsK8sAPIField asserts that `needs_k8s_api` is
// persisted in the built-in type:project and type:role schemas so a fresh seed
// does not drop the field (gd-ycscFUKJUi). The field is the opt-in for
// mounting a projected k8s ServiceAccount token into agent pods
// (see AUDIT.md §C3 / ka-0000006344).
func TestResolveTypeConfig_NeedsK8sAPIField(t *testing.T) {
	srv, _, ctx := testCtx(t)

	for _, typeName := range []model.BeadType{"project", "role"} {
		tc, err := srv.resolveTypeConfig(ctx, typeName)
		if err != nil {
			t.Fatalf("resolveTypeConfig(%q): %v", typeName, err)
		}
		if tc == nil {
			t.Fatalf("type:%s config missing from builtins", typeName)
		}
		var found bool
		for _, f := range tc.Fields {
			if f.Name == "needs_k8s_api" {
				found = true
				if f.Type != "boolean" {
					t.Errorf("type:%s needs_k8s_api type=%q, want boolean", typeName, f.Type)
				}
				break
			}
		}
		if !found {
			t.Errorf("type:%s missing built-in field needs_k8s_api", typeName)
		}
	}
}

// TestResolveTypeConfig_AgentSourceKeyField asserts that the built-in
// type:agent schema declares the generic `source_key` field alongside the
// per-source key fields. The trigger engine stamps a match key onto every
// spawned agent bead via triggerSourceFieldKey (gardenio controller): registered
// sources (jira/gitlab/schedule/schedule-mirror/multi) get their dedicated
// `<source>_key`, and every OTHER source (bead, account, dynamic webhook names)
// falls back to `source_key`. Before this, unregistered sources stamped fields
// like `bead_key` that this schema never declared, so every bead-source
// apply_formula driver spawn 400'd "bead_key: unknown field" and left the
// poured molecule driver-less (ka-0000001885).
func TestResolveTypeConfig_AgentSourceKeyField(t *testing.T) {
	srv, _, ctx := testCtx(t)

	tc, err := srv.resolveTypeConfig(ctx, "agent")
	if err != nil {
		t.Fatalf("resolveTypeConfig(agent): %v", err)
	}
	if tc == nil {
		t.Fatal("type:agent config missing from builtins")
	}
	fieldType := map[string]string{}
	for _, f := range tc.Fields {
		fieldType[f.Name] = string(f.Type)
	}
	// Every match-key field the trigger engine may stamp must be declared,
	// PLUS the three fields the formula-step dispatcher copies from the step
	// onto the worker agent it spawns (bridge/formula_step_dispatch.go
	// dispatch() extraFields). Their absence 400'd the dispatch spawn and made
	// the FSM §7→§8 fix-proof pod unspawnable (BUG3 gd-CSwwLRMGbz, uncovered by
	// the E2E proof ka-0000007685 once the driver-spawn 400s were fixed).
	for _, name := range []string{
		"source_key", "jira_key", "gitlab_key",
		"schedule_key", "schedule_mirror_key", "multi_key",
		"molecule_id", "source_formula", "source_step_id",
	} {
		ft, ok := fieldType[name]
		if !ok {
			t.Errorf("type:agent missing built-in field %q", name)
			continue
		}
		if ft != "string" {
			t.Errorf("type:agent field %q type=%q, want string", name, ft)
		}
	}

	// A source_key write must pass field validation — this is the exact stamp
	// a bead-source apply_formula spawn now emits.
	if err := model.ValidateTypeFields(
		json.RawMessage(`{"agent":"a","role":"crew","project":"p","source_key":"gd-abc123"}`),
		tc,
	); err != nil {
		t.Errorf("ValidateTypeFields rejected source_key stamp: %v", err)
	}

	// The exact agent-bead field set the formula-step dispatcher stamps on
	// spawn must pass validation — a closed type:agent schema 400s any
	// undeclared field, and this is the write that was failing on prod
	// (gd-CSwwLRMGbz). single_claim_task is stripped by SpawnAgent before the
	// create, so it is intentionally NOT part of this payload.
	if err := model.ValidateTypeFields(
		json.RawMessage(`{"agent":"a","role":"monorepo-fix","project":"monorepo","spawned_by":"formula-step-dispatcher","source":"formula_step","molecule_id":"gd-mol1","source_formula":"gd-form1","source_step_id":"extract-inputs-and-fix"}`),
		tc,
	); err != nil {
		t.Errorf("ValidateTypeFields rejected formula-step-dispatcher spawn stamp: %v", err)
	}
}

// TestResolveTypeConfig_AgentModelField asserts that the built-in type:agent
// schema declares the `model` field. The gardenio bridge stamps fields["model"]
// on a `--model` spawn/resume (bot_mentions.go / bot_agent_kill.go), and BOTH
// agent-card render seams read it back to size the context-window occupancy
// percentage PER-MODEL — contextwindow.OccupancyPct(context_tokens, fields
// ["model"]) in cmd/beads-viewer/agent_usage.go and internal/bridge/
// bot_agents_usage.go (ka-0000005244). type:agent does NOT set
// allow_additional_fields, so before this the model stamp 400'd "model:
// unknown field" and never persisted: OccupancyPct always received "" and fell
// back to the fleet-default 200k window, so a long-context (1M) agent's bar
// silently mis-rendered how close it was to compaction. Registering it in the
// builtin closes the drift and keeps a fresh seed / snapshot restore from
// dropping it again (ka-0000001124) — mirroring type:project, which already
// declares model (TestResolveTypeConfig_ProjectControllerReadFields).
func TestResolveTypeConfig_AgentModelField(t *testing.T) {
	srv, _, ctx := testCtx(t)

	tc, err := srv.resolveTypeConfig(ctx, "agent")
	if err != nil {
		t.Fatalf("resolveTypeConfig(agent): %v", err)
	}
	if tc == nil {
		t.Fatal("type:agent config missing from builtins")
	}
	var found *model.FieldDef
	for i := range tc.Fields {
		if tc.Fields[i].Name == "model" {
			found = &tc.Fields[i]
			break
		}
	}
	if found == nil {
		t.Fatal("type:agent missing built-in field \"model\" (both render seams read it)")
	}
	if found.Type != model.FieldTypeString {
		t.Errorf("type:agent field \"model\" type=%q, want string", found.Type)
	}

	// The exact per-model stamp the controller emits on a `--model fable` spawn
	// must pass field validation — this is the write that was 400'ing, which is
	// what pinned OccupancyPct to the 200k default in prod.
	if err := model.ValidateTypeFields(
		json.RawMessage(`{"agent":"a","role":"crew","project":"p","model":"claude-fable-5","context_tokens":128000}`),
		tc,
	); err != nil {
		t.Errorf("ValidateTypeFields rejected model stamp: %v", err)
	}
}

// TestResolveTypeConfig_AgentRespawnParkFields asserts that the built-in
// type:agent schema declares the four controller-authoritative succession +
// respawn wake fields — successor, next_respawn_at, respawn_count, max_respawns
// (the AgentBeadField* const set in gardenio cmd/gb/wrapup.go). The idle-TTL park
// path (init_agent_idle_ttl.go, epic ka-0000006987) closes a persistent agent's
// bead with flat successor=same + next_respawn_at wake metadata so the future
// wake-scanner (ka-0000004902) recreates the identity. type:agent does NOT set
// allow_additional_fields, so before this the park CloseBead 400'd "successor:
// unknown field" and the park silently failed — the agent terminally despawned
// instead of parking, and next_respawn_at never landed (ka-0000004509).
// Registering them in the builtin closes the drift and — via resolveTypeConfig's
// builtin-merge — repairs the persisted prod type:agent row that would otherwise
// keep shadowing this (ka-0000000061).
func TestResolveTypeConfig_AgentRespawnParkFields(t *testing.T) {
	srv, _, ctx := testCtx(t)

	tc, err := srv.resolveTypeConfig(ctx, "agent")
	if err != nil {
		t.Fatalf("resolveTypeConfig(agent): %v", err)
	}
	if tc == nil {
		t.Fatal("type:agent config missing from builtins")
	}
	byName := make(map[string]model.FieldDef, len(tc.Fields))
	for _, f := range tc.Fields {
		byName[f.Name] = f
	}
	for _, name := range []string{"successor", "next_respawn_at", "respawn_count", "max_respawns"} {
		f, ok := byName[name]
		if !ok {
			t.Errorf("type:agent missing built-in field %q (idle-TTL park write)", name)
			continue
		}
		if f.Type != model.FieldTypeString {
			t.Errorf("type:agent field %q type=%q, want string", name, f.Type)
		}
	}

	// The exact merged field map the idle-TTL park path presents to CloseBead
	// (init_agent_idle_ttl.go stamps agent_state=done, close_reason=idle_ttl_parked,
	// successor=same, next_respawn_at=<RFC3339> over the already-persisted
	// agent/role/project) must pass update validation — UpdateBead validates the
	// MERGED map (beads.go), and it was this write that 400'd "successor: unknown
	// field" / "next_respawn_at: unknown field" and silently defeated the park.
	if err := model.ValidateTypeFieldsForUpdate(
		json.RawMessage(`{"agent":"a","role":"crew","project":"p","agent_state":"done","close_reason":"idle_ttl_parked","successor":"same","next_respawn_at":"2026-08-16T21:45:00Z"}`),
		tc,
		nil,
	); err != nil {
		t.Errorf("ValidateTypeFieldsForUpdate rejected idle-TTL park write: %v", err)
	}
}

// TestResolveTypeConfig_AgentBridgeOnlyFields asserts that the three type:agent
// fields which used to be registered ONLY in the gardenio bridge's config-BEAD
// path are present in the DAEMON builtin — the only place validation actually
// consults (resolveTypeConfig reads builtinConfigs + the configs KV table, never
// a config bead; same gap as grafana-bridge's type=dashboard, ka-0000001549).
//
// Registering a field in controller/internal/bridge/init.go is a NO-OP for
// validation. All three 400'd "unknown field" on prod for months (ka-0000000811):
//
//   - prior_wrapup — the revive-in-place PATCH (bridge/bot_agent_kill.go) archives
//     the outgoing wrapup here in the SAME atomic UpdateBead as the
//     closed->in_progress flip, so the 400 failed the WHOLE revive and dropped the
//     respawn to a cold mint: new bead id, new COOP_S3_SESSION_KEY prefix, empty
//     S3, conversation lost — the exact failure revive-in-place prevents
//     (ka-0000006807, ka-0000003692).
//   - error_category / error_detail — terminal-auth-failure diagnostics persisted
//     by the NATS listener (controller/cmd/controller/terminal_auth_failure.go) so
//     a first-turn Unauthorized/OutOfCredits has a durable home (ka-0000003962).
//   - spawn_wall / spawn_wall_reason — the Lands spawn wall's refusal, stamped by
//     the controller with agent_state=budget_exceeded (Track B step 8.3).
//   - purse_nudged_at / park_reason — the Lands purse watcher's nudge time and
//     the reason it parked the agent (purse_empty, Track B step 8.4).
func TestResolveTypeConfig_AgentBridgeOnlyFields(t *testing.T) {
	srv, _, ctx := testCtx(t)

	tc, err := srv.resolveTypeConfig(ctx, "agent")
	if err != nil {
		t.Fatalf("resolveTypeConfig(agent): %v", err)
	}
	if tc == nil {
		t.Fatal("type:agent config missing from builtins")
	}
	byName := make(map[string]model.FieldDef, len(tc.Fields))
	for _, f := range tc.Fields {
		byName[f.Name] = f
	}
	for _, name := range []string{"prior_wrapup", "error_category", "error_detail", "spawn_wall", "spawn_wall_reason", "purse_nudged_at", "park_reason"} {
		f, ok := byName[name]
		if !ok {
			t.Errorf("type:agent missing built-in field %q — registering it in the "+
				"bridge's init.go does NOT register it with the daemon (ka-0000000811)", name)
			continue
		}
		if f.Type != model.FieldTypeString {
			t.Errorf("type:agent field %q type=%q, want string", name, f.Type)
		}
	}

	// The exact merged field map the revive-in-place PATCH presents to UpdateBead
	// (bot_agent_kill.go stamps agent_state + prior_wrapup over the persisted
	// agent/role/project, clearing stop_requested, and carries the status flip in
	// the same write). UpdateBead validates the MERGED map (beads.go), and it was
	// this write that 400'd "prior_wrapup: unknown field" and cold-minted the
	// thread agent twice on 2026-08-17.
	if err := model.ValidateTypeFieldsForUpdate(
		json.RawMessage(`{"agent":"a","role":"thread","project":"p","agent_state":"spawning","stop_requested":"","prior_wrapup":"{\"tldr\":\"prior turn\"}"}`),
		tc,
		nil,
	); err != nil {
		t.Errorf("ValidateTypeFieldsForUpdate rejected revive-in-place PATCH: %v", err)
	}

	// The terminal-auth-failure listener's merge (error_category + error_detail
	// over the persisted identity fields).
	if err := model.ValidateTypeFieldsForUpdate(
		json.RawMessage(`{"agent":"a","role":"crew","project":"p","error_category":"unauthorized","error_detail":"401 key not allowed to access model"}`),
		tc,
		nil,
	); err != nil {
		t.Errorf("ValidateTypeFieldsForUpdate rejected terminal-auth-failure write: %v", err)
	}
}

// TestResolveTypeConfig_ChannelOverrideFields asserts that `channel_modes` and
// `channel_runtime_class` are present in the built-in type:project schema. The
// bridge already reads both (beadsapi/projects.go parses them; channel_modes
// drives per-channel interaction mode and channel_runtime_class pins a single
// channel's thread agents onto gVisor/runsc for freeze/thaw fast hot resume —
// epic ka-0000003080). channel_runtime_class shipped its parsing in MR !4323
// but was never registered in the daemon schema, so the daemon 400-rejected
// every write (`unknown field`) — leaving the gb-turbo→runsc override
// unsettable until the field was hand-patched onto the live config. Registering
// it in the builtin keeps a fresh seed / snapshot restore from dropping it
// again (gd-XO9hRQjmv9).
func TestResolveTypeConfig_ChannelOverrideFields(t *testing.T) {
	srv, _, ctx := testCtx(t)

	tc, err := srv.resolveTypeConfig(ctx, "project")
	if err != nil {
		t.Fatalf("resolveTypeConfig(project): %v", err)
	}
	if tc == nil {
		t.Fatal("type:project config missing from builtins")
	}
	byName := make(map[string]model.FieldDef, len(tc.Fields))
	for _, f := range tc.Fields {
		byName[f.Name] = f
	}
	for _, name := range []string{"channel_modes", "channel_runtime_class"} {
		f, ok := byName[name]
		if !ok {
			t.Errorf("type:project missing built-in field %q", name)
			continue
		}
		if f.Type != model.FieldTypeJSON {
			t.Errorf("type:project field %q type=%q, want json", name, f.Type)
		}
	}
}

// TestResolveTypeConfig_ProjectControllerReadFields asserts that the seven
// fields the controller's ProjectInfo parser reads
// (gardenio/controller/internal/beadsapi/projects.go) but the type:project
// schema previously omitted are now declared with the correct FieldType.
// type:project does NOT set allow_additional_fields, so model.ValidateTypeFields
// (create) and ValidateTypeFieldsForUpdate (update) 400-rejected any write that
// carried these keys — leaving account / model / runtime_class / dm_default /
// triggers_enabled / resource_mapping / claude_config unsettable on a project
// bead. Registering them in the builtin closes the drift and keeps a fresh seed
// / snapshot restore from dropping them again (gd-u8AbgveHQW, design doc §5).
func TestResolveTypeConfig_ProjectControllerReadFields(t *testing.T) {
	srv, _, ctx := testCtx(t)

	tc, err := srv.resolveTypeConfig(ctx, "project")
	if err != nil {
		t.Fatalf("resolveTypeConfig(project): %v", err)
	}
	if tc == nil {
		t.Fatal("type:project config missing from builtins")
	}
	byName := make(map[string]model.FieldDef, len(tc.Fields))
	for _, f := range tc.Fields {
		byName[f.Name] = f
	}

	want := map[string]model.FieldType{
		"account":          model.FieldTypeString,
		"model":            model.FieldTypeString,
		"runtime_class":    model.FieldTypeString,
		"triggers_enabled": model.FieldTypeBoolean,
		"dm_default":       model.FieldTypeBoolean,
		"resource_mapping": model.FieldTypeJSON,
		"claude_config":    model.FieldTypeJSON,
	}
	for name, typ := range want {
		f, ok := byName[name]
		if !ok {
			t.Errorf("type:project missing built-in field %q (controller reads it)", name)
			continue
		}
		if f.Type != typ {
			t.Errorf("type:project field %q type=%q, want %q", name, f.Type, typ)
		}
	}
}

// TestResolveTypeConfig_ProjectSessionResumeEnum asserts that session_resume on
// the type:project schema is an enum constrained to {"true","false","auto"} —
// the exact set the controller parseTriBool path understands (parseTriBool
// treats "true"/"1" as true and any other non-empty value as false; "auto" is
// the documented sentinel for "let the controller decide"). Promoting it from a
// free "string" to an enum means the daemon rejects typo'd values like "yes" or
// "on" at create/update time instead of silently coercing them to false in the
// controller (design doc project-first-class-hub-design.md §5.2). Legacy beads
// that omit session_resume entirely are unaffected — the field is not required.
func TestResolveTypeConfig_ProjectSessionResumeEnum(t *testing.T) {
	srv, _, ctx := testCtx(t)

	tc, err := srv.resolveTypeConfig(ctx, "project")
	if err != nil {
		t.Fatalf("resolveTypeConfig(project): %v", err)
	}
	if tc == nil {
		t.Fatal("type:project config missing from builtins")
	}

	var sr *model.FieldDef
	for i := range tc.Fields {
		if tc.Fields[i].Name == "session_resume" {
			sr = &tc.Fields[i]
			break
		}
	}
	if sr == nil {
		t.Fatal("type:project missing session_resume field")
	}
	if sr.Type != model.FieldTypeEnum {
		t.Errorf("session_resume type=%q, want enum", sr.Type)
	}
	wantVals := []string{"true", "false", "auto"}
	if !slices.Equal(sr.Values, wantVals) {
		t.Errorf("session_resume values=%v, want %v", sr.Values, wantVals)
	}

	// Every accepted enum value must pass create-time validation.
	for _, v := range wantVals {
		fields := json.RawMessage(`{"prefix":"gd-","session_resume":"` + v + `"}`)
		if err := model.ValidateTypeFields(fields, tc); err != nil {
			t.Errorf("session_resume=%q should validate, got %v", v, err)
		}
	}

	// A typo'd / out-of-set value must be rejected.
	bad := json.RawMessage(`{"prefix":"gd-","session_resume":"yes"}`)
	if err := model.ValidateTypeFields(bad, tc); err == nil {
		t.Error("session_resume=\"yes\" should be rejected by the enum, but validation passed")
	}

	// Omitting session_resume entirely must still pass — legacy project beads
	// predate the field and it is intentionally not required.
	omitted := json.RawMessage(`{"prefix":"gd-"}`)
	if err := model.ValidateTypeFields(omitted, tc); err != nil {
		t.Errorf("project bead omitting session_resume should validate, got %v", err)
	}
}

// TestResolveTypeConfig_HarnessEnum asserts that `harness` (the agent CLI
// selector, gd-6SezxhSEEk) is a claude|codex enum on type:project, type:role
// and type:agent, and that any other value is rejected at write time so a typo
// can never reach the controller's pod-spec resolution.
func TestResolveTypeConfig_HarnessEnum(t *testing.T) {
	srv, _, ctx := testCtx(t)

	base := map[string]string{
		"project": `"prefix":"gd-"`,
		"role":    `"image":"agent"`,
		"agent":   `"agent":"a","role":"crew","project":"p"`,
	}
	for typ, required := range base {
		tc, err := srv.resolveTypeConfig(ctx, model.BeadType(typ))
		if err != nil || tc == nil {
			t.Fatalf("resolveTypeConfig(%s): %v (tc=%v)", typ, err, tc)
		}
		var h *model.FieldDef
		for i := range tc.Fields {
			if tc.Fields[i].Name == "harness" {
				h = &tc.Fields[i]
				break
			}
		}
		if h == nil {
			t.Fatalf("type:%s missing harness field", typ)
		}
		if h.Type != model.FieldTypeEnum {
			t.Errorf("type:%s harness type=%q, want enum", typ, h.Type)
		}
		if want := []string{"claude", "codex", "gemini"}; !slices.Equal(h.Values, want) {
			t.Errorf("type:%s harness values=%v, want %v", typ, h.Values, want)
		}
		for _, v := range []string{"claude", "codex", "gemini"} {
			fields := json.RawMessage(`{` + required + `,"harness":"` + v + `"}`)
			if err := model.ValidateTypeFields(fields, tc); err != nil {
				t.Errorf("type:%s harness=%q should validate, got %v", typ, v, err)
			}
		}
		for _, v := range []string{"gpt", "Gemini", "Codex", "", "claude-code"} {
			fields := json.RawMessage(`{` + required + `,"harness":"` + v + `"}`)
			if err := model.ValidateTypeFields(fields, tc); err == nil {
				t.Errorf("type:%s harness=%q should be rejected", typ, v)
			}
		}
		omitted := json.RawMessage(`{` + required + `}`)
		if err := model.ValidateTypeFields(omitted, tc); err != nil {
			t.Errorf("type:%s omitting harness should validate, got %v", typ, err)
		}
	}
}

// TestResolveTypeConfig_AgentRuntimeClassField asserts that `runtime_class` is
// present in the built-in type:agent schema. The slack-bridge stamps
// runtime_class=runsc on a thread-spawned agent bead when its channel carries a
// channel_runtime_class override (e.g. gb-turbo → runsc, epic ka-0000003080),
// and the controller reads metadata["runtime_class"] (podspec.go) to pin the
// pod's RuntimeClass + route it into the gVisor namespace. type:role already
// declared the field, but type:agent omitted it — so every gb-turbo @mention
// 400-rejected on CreateBead ("runtime_class: unknown field"), no agent bead was
// ever created, and the channel silently never responded. Registering it on the
// agent type keeps the override end-to-end functional and survives a fresh seed
// / snapshot restore.
func TestResolveTypeConfig_AgentRuntimeClassField(t *testing.T) {
	srv, _, ctx := testCtx(t)

	tc, err := srv.resolveTypeConfig(ctx, "agent")
	if err != nil {
		t.Fatalf("resolveTypeConfig(agent): %v", err)
	}
	if tc == nil {
		t.Fatal("type:agent config missing from builtins")
	}
	var found bool
	for _, f := range tc.Fields {
		if f.Name == "runtime_class" {
			found = true
			if f.Type != model.FieldTypeString {
				t.Errorf("type:agent runtime_class type=%q, want string", f.Type)
			}
			break
		}
	}
	if !found {
		t.Error("type:agent missing built-in field runtime_class")
	}
}

// TestResolveTypeConfig_AgentPlanIDField asserts that `plan_id` is present in
// the built-in type:agent schema. beads-viewer's spawnPlanAssistant
// (plans_assistant.go) stamps fields.plan_id on the plan-assistant agent bead —
// it is the reverse-lookup binding (plan epic → assistant) the /plans/{id}
// assistant lane keys off. type:agent previously omitted the field, so every
// "start the plan assistant" CreateBead 400-rejected ("plan_id: unknown field"),
// no assistant bead was created, and the browser saw the generic 502 "Failed to
// start the plan assistant" (ka-0000000017). Registering it keeps the plan
// assistant spawn functional and survives a fresh seed / snapshot restore.
func TestResolveTypeConfig_AgentPlanIDField(t *testing.T) {
	srv, _, ctx := testCtx(t)

	tc, err := srv.resolveTypeConfig(ctx, "agent")
	if err != nil {
		t.Fatalf("resolveTypeConfig(agent): %v", err)
	}
	if tc == nil {
		t.Fatal("type:agent config missing from builtins")
	}
	var found bool
	for _, f := range tc.Fields {
		if f.Name == "plan_id" {
			found = true
			if f.Type != model.FieldTypeString {
				t.Errorf("type:agent plan_id type=%q, want string", f.Type)
			}
			break
		}
	}
	if !found {
		t.Error("type:agent missing built-in field plan_id")
	}
}

// TestResolveTypeConfig_AgentMicrovmGitTokenFields asserts that
// `microvm_git_token_id` and `microvm_git_project` are present in the built-in
// type:agent schema. The gardenio controller persists a minted per-run GitLab
// token's revocation coordinates onto the agent bead at launch
// (persistMicrovmGitToken) and reads them back on every exit path to revoke
// the token (revokeGitToken). type:agent previously omitted both fields, so
// the persist 400-rejected ("unknown field"), the revoker found no
// coordinates, and every minted token silently outlived its VM until the
// +2-day expires_at backstop (gd-JbFtmSLPvI). Registering them keeps
// revoke-on-termination functional and survives a fresh seed / snapshot
// restore.
func TestResolveTypeConfig_AgentMicrovmGitTokenFields(t *testing.T) {
	srv, _, ctx := testCtx(t)

	tc, err := srv.resolveTypeConfig(ctx, "agent")
	if err != nil {
		t.Fatalf("resolveTypeConfig(agent): %v", err)
	}
	if tc == nil {
		t.Fatal("type:agent config missing from builtins")
	}
	for _, name := range []string{"microvm_git_token_id", "microvm_git_project"} {
		var found bool
		for _, f := range tc.Fields {
			if f.Name == name {
				found = true
				if f.Type != model.FieldTypeString {
					t.Errorf("type:agent %s type=%q, want string", name, f.Type)
				}
				break
			}
		}
		if !found {
			t.Errorf("type:agent missing built-in field %s", name)
		}
	}
}

// TestResolveTypeConfig_RoleAccountWhitelistField asserts that
// `account_whitelist` is present in the built-in type:role schema. The
// controller already reads this field (beadsapi/roles.go) to pin a role's
// pod-side BOAT_ACCOUNT round-robin to a specific account set, but the schema
// previously omitted it so the daemon 400-rejected writes — leaving the
// gvisor-poc canary with no deterministic account-pin path (gd-QEnm4gPcNO).
func TestResolveTypeConfig_RoleAccountWhitelistField(t *testing.T) {
	srv, _, ctx := testCtx(t)

	tc, err := srv.resolveTypeConfig(ctx, "role")
	if err != nil {
		t.Fatalf("resolveTypeConfig(role): %v", err)
	}
	if tc == nil {
		t.Fatal("type:role config missing from builtins")
	}
	var found bool
	for _, f := range tc.Fields {
		if f.Name == "account_whitelist" {
			found = true
			if f.Type != model.FieldTypeJSON {
				t.Errorf("type:role account_whitelist type=%q, want json", f.Type)
			}
			break
		}
	}
	if !found {
		t.Error("type:role missing built-in field account_whitelist")
	}
}

// TestResolveTypeConfig_RoleModelField asserts that `model` is present in the
// built-in type:role schema. The controller's RoleInfo parser reads
// fields["model"] (gardenio/controller/internal/beadsapi/roles.go) to override
// the model for agents spawned with a role, independent of which account they
// use (epic ka-0000005488), but the schema previously omitted it — so the
// daemon 400-rejected ("model: unknown field") every role bead that set it,
// leaving a single account unable to back fable/opus/sonnet roles without
// account cloning. Registering it in the builtin closes the drift and keeps a
// fresh seed / snapshot restore from dropping it again (gd-7xcz6dWHaD,
// design doc §8.4).
func TestResolveTypeConfig_RoleModelField(t *testing.T) {
	srv, _, ctx := testCtx(t)

	tc, err := srv.resolveTypeConfig(ctx, "role")
	if err != nil {
		t.Fatalf("resolveTypeConfig(role): %v", err)
	}
	if tc == nil {
		t.Fatal("type:role config missing from builtins")
	}
	var found bool
	for _, f := range tc.Fields {
		if f.Name == "model" {
			found = true
			if f.Type != model.FieldTypeString {
				t.Errorf("type:role model type=%q, want string", f.Type)
			}
			break
		}
	}
	if !found {
		t.Error("type:role missing built-in field model")
	}
}

// TestResolveTypeConfig_FormulaInheritanceFields asserts that the built-in
// type:formula schema permits the inheritance/composition fields the CLI
// already writes into a formula bead's Fields map: formula_type, extends, and
// phase (plus requires_skills/aspects/match/before/after). These are
// first-class, documented formula features (see gardenio/docs/formula-reference.md
// and kbeads/CLAUDE.md), but the schema previously allowed only
// vars/steps/default_roles/assigned_agent — so `kd formula create` of any
// extends-based formula 400-rejected with "unknown field". Registering such a
// formula bead is required for it to be invocable via `/formula pour <id>`.
func TestResolveTypeConfig_FormulaInheritanceFields(t *testing.T) {
	srv, _, ctx := testCtx(t)

	tc, err := srv.resolveTypeConfig(ctx, "formula")
	if err != nil {
		t.Fatalf("resolveTypeConfig(formula): %v", err)
	}
	if tc == nil {
		t.Fatal("type:formula config missing from builtins")
	}
	byName := make(map[string]model.FieldDef, len(tc.Fields))
	for _, f := range tc.Fields {
		byName[f.Name] = f
	}
	want := map[string]model.FieldType{
		"formula_type":    model.FieldTypeEnum,
		"extends":         model.FieldTypeJSON,
		"phase":           model.FieldTypeEnum,
		"requires_skills": model.FieldTypeJSON,
		"aspects":         model.FieldTypeJSON,
		"before":          model.FieldTypeJSON,
		"after":           model.FieldTypeJSON,
	}
	for name, typ := range want {
		f, ok := byName[name]
		if !ok {
			t.Errorf("type:formula missing built-in field %q", name)
			continue
		}
		if f.Type != typ {
			t.Errorf("type:formula field %q type=%q, want %q", name, f.Type, typ)
		}
	}
}

func TestResolveTypeConfig_MergesBuiltinFields(t *testing.T) {
	srv, ms, ctx := testCtx(t)

	// Simulate a stale store entry with only base fields.
	ms.configs["type:account"] = &model.Config{
		Key: "type:account",
		Value: json.RawMessage(`{
			"kind": "config",
			"fields": [
				{"name": "provider", "type": "string", "required": true},
				{"name": "credential", "type": "string", "required": true}
			]
		}`),
	}

	tc, err := srv.resolveTypeConfig(ctx, "account")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tc == nil {
		t.Fatal("expected account type config, got nil")
	}

	fieldNames := map[string]bool{}
	for _, f := range tc.Fields {
		fieldNames[f.Name] = true
	}

	// Store fields should be present.
	if !fieldNames["provider"] {
		t.Error("store field 'provider' missing")
	}

	// Builtin-only fields should be merged in.
	for _, name := range []string{
		"throttle_status", "utilization_5h", "probe_checked_at",
	} {
		if !fieldNames[name] {
			t.Errorf("builtin field %q should be merged into store config", name)
		}
	}
}

func TestResolveTypeConfig_WritePolicyFromBuiltin(t *testing.T) {
	srv, _, ctx := testCtx(t)

	// Agent type has write_policy in builtins.
	tc, err := srv.resolveTypeConfig(ctx, "agent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tc == nil {
		t.Fatal("expected agent type config, got nil")
	}
	if tc.WritePolicy == nil {
		t.Fatal("expected write_policy on agent type, got nil")
	}
	if !tc.WritePolicy.ProjectScoped {
		t.Error("agent write_policy.project_scoped should be true")
	}
	if !tc.WritePolicy.Escalatable {
		t.Error("agent write_policy.escalatable should be true")
	}
}

// TestResolveTypeConfig_AgentSlackInheritFields guards the agent-type field
// allowlist against regressing the `gb spawn --task <slack-thread-bead>` path
// (gd-Jc8B5tkWp2). Gardener/`gb spawn` copies the originating slack-thread
// fields onto the spawned agent bead; every field in that inherited set MUST be
// declared on the agent type or the create 400s with "unknown field". A bead
// validated against the agent config carrying the full inherited set must pass.
func TestResolveTypeConfig_AgentSlackInheritFields(t *testing.T) {
	srv, _, ctx := testCtx(t)

	tc, err := srv.resolveTypeConfig(ctx, "agent")
	if err != nil {
		t.Fatalf("resolveTypeConfig(agent): %v", err)
	}
	if tc == nil {
		t.Fatal("type:agent config missing from builtins")
	}

	fieldNames := map[string]bool{}
	for _, f := range tc.Fields {
		fieldNames[f.Name] = true
	}

	// Mirrors slackFieldsToInherit in gardenio/controller/cmd/gb/spawn.go — the
	// set copied onto the agent bead when spawning against a slack-thread task.
	// slack_card_channel/slack_card_ts are not inherited at spawn, but are
	// persisted onto the agent bead by the slack-bridge agent-card writer
	// (gardenio/controller/internal/bridge/bot_agents.go ~L547); they must be
	// declared too or every card persist logs a DEBUG "unknown field"
	// (gd-P0EVYDR64l).
	for _, name := range []string{
		"slack_thread_channel",
		"slack_thread_ts",
		"slack_spawn_channel",
		"slack_opener_user_id",
		"slack_permalink",
		"slack_card_channel",
		"slack_card_ts",
	} {
		if !fieldNames[name] {
			t.Errorf("agent type must allow slack field %q (gb spawn --task on a slack-thread bead 400s / bridge card persist logs unknown field)", name)
		}
	}

	// The actual repro: an agent bead carrying the inherited slack fields plus
	// the bridge-written card-ref fields must pass field validation against the
	// agent type config.
	fields := json.RawMessage(`{
		"agent": "gardenio-crew-x",
		"role": "crew",
		"project": "gardenio",
		"slack_thread_channel": "C-THREAD",
		"slack_thread_ts": "1700000000.000100",
		"slack_spawn_channel": "C-SPAWN",
		"slack_opener_user_id": "U-OPENER",
		"slack_permalink": "https://slack.example.com/archives/C-THREAD/p1700000000000100",
		"slack_card_channel": "C-CARD",
		"slack_card_ts": "1700000000.000200"
	}`)
	if err := model.ValidateTypeFields(fields, tc); err != nil {
		t.Errorf("agent bead with inherited slack + card-ref fields failed validation: %v", err)
	}
}

// TestResolveTypeConfig_AgentLiteLLMSpendFields guards the per-session LiteLLM
// cost reconciler (gardenio/controller/cmd/controller/reconcile_session_spend.go).
// captureBaseline writes litellm_spend_baseline at spawn, and reconcileAtClose
// writes total_cost_usd *together with* litellm_spend_close in one PATCH. If the
// two debug fields are not declared on type:agent, the daemon 400s the whole
// update ("litellm_spend_close: unknown field") and the cost write is dropped
// with it — leaving total_cost_usd=0 on every agent bead while LiteLLM holds the
// real spend (ka-0000000028).
func TestResolveTypeConfig_AgentLiteLLMSpendFields(t *testing.T) {
	srv, _, ctx := testCtx(t)

	tc, err := srv.resolveTypeConfig(ctx, "agent")
	if err != nil {
		t.Fatalf("resolveTypeConfig(agent): %v", err)
	}
	if tc == nil {
		t.Fatal("type:agent config missing from builtins")
	}

	fieldNames := map[string]bool{}
	for _, f := range tc.Fields {
		fieldNames[f.Name] = true
	}
	// Field names must match the constants in reconcile_session_spend.go
	// (spendBaselineField / spendCloseField) and spendlogs_sweep.go
	// (spendLogsCostField — the C3 stateless attribution figure, written each
	// sweep alongside total_cost_usd; unknown here means every sweep PATCH
	// 400s and the P10 agent-spend-parity cross-check never gets a side B).
	for _, name := range []string{"litellm_spend_baseline", "litellm_spend_close", "spend_logs_cost_usd"} {
		if !fieldNames[name] {
			t.Errorf("agent type must allow %q or the per-session spend reconciler's PATCH 400s and total_cost_usd is never written", name)
		}
	}

	// The actual repro: the exact field set reconcileAtClose writes at close
	// must pass validation, or total_cost_usd is dropped along with the unknown
	// field.
	fields := json.RawMessage(`{
		"agent": "gardenio-crew-x",
		"role": "crew",
		"project": "gardenio",
		"total_cost_usd": 1.2345,
		"litellm_spend_close": 100.6789,
		"litellm_spend_baseline": 99.4444
	}`)
	if err := model.ValidateTypeFields(fields, tc); err != nil {
		t.Errorf("agent bead with LiteLLM spend reconcile fields failed validation: %v", err)
	}
}

// TestResolveTypeConfig_AgentLiveUsageFields guards the controller status
// reporter's live-usage mirror (gardenio/controller/internal/statusreporter/
// reporter.go reportLiveUsage). Every SyncAll cycle it PATCHes context_tokens
// and live_cost_usd onto the running agent's bead so the beads-viewer + Slack
// agent cards can show live "context so far" / "cost so far". If those two
// fields are not declared on type:agent, the daemon 400s the whole PATCH
// ("context_tokens: unknown field") and BOTH fields are dropped — so every
// live agent shows no spend and no context until it closes, even though closed
// agents (total_cost_usd, reconciled at session close) work fine. Same
// writer-vs-schema drift class as the LiteLLM-spend repro above (gd-5KgCydSUnD).
func TestResolveTypeConfig_AgentLiveUsageFields(t *testing.T) {
	srv, _, ctx := testCtx(t)

	tc, err := srv.resolveTypeConfig(ctx, "agent")
	if err != nil {
		t.Fatalf("resolveTypeConfig(agent): %v", err)
	}
	if tc == nil {
		t.Fatal("type:agent config missing from builtins")
	}

	fieldNames := map[string]bool{}
	for _, f := range tc.Fields {
		fieldNames[f.Name] = true
	}
	// Field names must match the keys reportLiveUsage writes. live_turns is the
	// live counterpart of total_request_count — mirrored each cycle alongside
	// context_tokens/live_cost_usd (epic ka-0000006449).
	for _, name := range []string{"context_tokens", "live_cost_usd", "live_turns"} {
		if !fieldNames[name] {
			t.Errorf("agent type must allow %q or reportLiveUsage's PATCH 400s and live spend/context is never written", name)
		}
	}

	// The actual repro: the exact field set reportLiveUsage writes each cycle
	// must pass validation, or live usage is dropped fleet-wide.
	fields := json.RawMessage(`{
		"agent": "gardenio-crew-x",
		"role": "crew",
		"project": "gardenio",
		"context_tokens": 127746,
		"live_cost_usd": 1.2345,
		"live_turns": 42
	}`)
	if err := model.ValidateTypeFields(fields, tc); err != nil {
		t.Errorf("agent bead with live-usage fields failed validation: %v", err)
	}
}

// TestResolveTypeConfig_AccountShadowCostFields guards the controller's
// account-cost spend_sync daemon (gardenio/controller/cmd/controller/spend_sync.go),
// which writes total/daily/weekly_cost_usd PLUS the shadow_* mirror fields in one
// atomic PATCH. type:account declared the canonical cost fields but not the
// shadow_* ones, so every spend_sync cycle 400'd with "shadow_total_cost_usd:
// unknown field" — and because the PATCH is atomic, total_cost_usd was dropped
// along with the unknown field, leaving the account card on /costs at $0
// fleet-wide. Same writer-vs-schema drift class as the agent LiteLLM-spend repro
// above (gd-hqsO82ZdVY).
func TestResolveTypeConfig_AccountShadowCostFields(t *testing.T) {
	srv, _, ctx := testCtx(t)

	tc, err := srv.resolveTypeConfig(ctx, "account")
	if err != nil {
		t.Fatalf("resolveTypeConfig(account): %v", err)
	}
	if tc == nil {
		t.Fatal("type:account config missing from builtins")
	}

	fieldNames := map[string]bool{}
	for _, f := range tc.Fields {
		fieldNames[f.Name] = true
	}
	for _, name := range []string{
		"shadow_total_cost_usd", "shadow_daily_cost_usd", "shadow_weekly_cost_usd",
		// Subscription-window bills (ka-0000003343): spend_sync writes these in the
		// SAME atomic PATCH, so an unregistered field 400s the whole batch and
		// drops total_cost_usd — the exact ka-0000007493 trap repeated.
		"spend_5h_window_usd", "spend_7d_window_usd",
		// A tenant land's purse stamp (Lands Track B §3.4). spend_sync writes it
		// in a second PATCH, so a miss only leaves the purse unknown.
		"spend_synced_at", "live_cost_baseline",
		// The purse watcher's state on a land's account (Track B step 8.4).
		"purse_notice_level", "purse_granted_seen",
	} {
		if !fieldNames[name] {
			t.Errorf("account type must allow %q or the account-cost spend_sync PATCH 400s and total_cost_usd is never written", name)
		}
	}

	// The actual repro: the exact field set spend_sync writes each cycle must
	// pass validation, or the canonical cost fields are dropped along with the
	// unknown shadow_* fields.
	fields := json.RawMessage(`{
		"provider": "anthropic",
		"credential": "secret:gardenio-claude-oauth-x",
		"total_cost_usd": 1.2345,
		"daily_cost_usd": 0.45,
		"weekly_cost_usd": 1.2345,
		"spend_5h_window_usd": 0.30,
		"spend_7d_window_usd": 1.10,
		"shadow_total_cost_usd": 1.2345,
		"shadow_daily_cost_usd": 0.45,
		"shadow_weekly_cost_usd": 1.2345
	}`)
	if err := model.ValidateTypeFields(fields, tc); err != nil {
		t.Errorf("account bead with spend_sync cost+shadow fields failed validation: %v", err)
	}
}

// TestResolveTypeConfig_MessageSlackThreadFields guards `gb slack reply`, which
// creates a type:message bead carrying slack_thread_channel/slack_thread_ts
// (gardenio/controller/cmd/gb/slack.go ~L177). type:message only declared
// channel/thread_ts, so every reply logged "warn: failed to create message
// bead ... slack_thread_channel: unknown field" (the reply still posts to Slack
// but the chat-history bead is dropped). Same writer-vs-schema drift class as
// ka-0000000028.
func TestResolveTypeConfig_MessageSlackThreadFields(t *testing.T) {
	srv, _, ctx := testCtx(t)

	tc, err := srv.resolveTypeConfig(ctx, "message")
	if err != nil {
		t.Fatalf("resolveTypeConfig(message): %v", err)
	}
	if tc == nil {
		t.Fatal("type:message config missing from builtins")
	}

	fieldNames := map[string]bool{}
	for _, f := range tc.Fields {
		fieldNames[f.Name] = true
	}
	// thread_id is the origin-agnostic wish-pointer all three message producers
	// (gb squawk, gb slack reply, and the beads-viewer thread composer's "send")
	// stamp so the /threads timeline renders the message as a conversation
	// member. Omitting it from the schema 400'd every composer "send" with
	// "thread_id: unknown field" — surfaced to the operator as a generic
	// "Failed to post message" (ka-0000005416). Same writer-vs-schema drift
	// class as the slack_thread_* fields above.
	for _, name := range []string{"slack_thread_channel", "slack_thread_ts", "thread_id"} {
		if !fieldNames[name] {
			t.Errorf("message type must allow %q or the thread composer / gb slack reply chat-history bead write 400s", name)
		}
	}

	fields := json.RawMessage(`{
		"source_agent": "gardenio-crew-x",
		"text": "hello thread",
		"slack_thread_channel": "C-THREAD",
		"slack_thread_ts": "1700000000.000100",
		"thread_id": "gd-wish01"
	}`)
	if err := model.ValidateTypeFields(fields, tc); err != nil {
		t.Errorf("message bead with thread fields failed validation: %v", err)
	}
}

// TestResolveTypeConfig_MessagePlanTranscriptFields guards the /plans in-viewer
// chat transcript producer (planTranscriptTurnBead: the operator half
// mintOperatorTurn + the assistant half AssistantTurnMinter, beads-viewer). A
// transcript turn is a type=message bead bound to its plan epic by
// fields.plan_id and rendered on the operator/assistant side from
// fields.turn_role. type:message did NOT declare these, so every turn mint 400'd
// "plan_id: unknown field"; because the mint is best-effort (error swallowed so
// the message still delivers), plan transcript turns silently never persisted —
// the third instance of this writer-vs-schema drift class (thread_id:
// ka-0000003279/ka-0000005416; slack_user_id: ka-0000004306), surfaced by the 4b
// beads-viewer-e2e coverage dogfooding the transcript against a real daemon.
func TestResolveTypeConfig_MessagePlanTranscriptFields(t *testing.T) {
	srv, _, ctx := testCtx(t)

	tc, err := srv.resolveTypeConfig(ctx, "message")
	if err != nil {
		t.Fatalf("resolveTypeConfig(message): %v", err)
	}
	if tc == nil {
		t.Fatal("type:message config missing from builtins")
	}

	fieldNames := map[string]bool{}
	for _, f := range tc.Fields {
		fieldNames[f.Name] = true
	}
	for _, name := range []string{"plan_id", "turn_role", "turn_id", "text_truncated", "full_length", "idempotency_key"} {
		if !fieldNames[name] {
			t.Errorf("message type must allow %q or the plan-transcript turn mint 400s and the whole transcript is silently dropped", name)
		}
	}

	// A full turn bead as planTranscriptTurnBead marshals it (operator side + the
	// truncation markers the AssistantTurnMinter adds) must pass validation —
	// INCLUDING the idempotency_key the mint sets (createBead folds IdempotencyKey
	// into Fields[idempotency_key], so it is validated against this schema).
	fields := json.RawMessage(`{
		"source_agent": "gardenio-plan-asst-1",
		"text": "On it — researching the existing routes now.",
		"plan_id": "gd-plan01",
		"turn_role": "assistant",
		"turn_id": "sess1:1700000000000:abc123",
		"text_truncated": "true",
		"full_length": "512000",
		"idempotency_key": "plan-turn:gd-plan01:sess1:1700000000000:abc123"
	}`)
	if err := model.ValidateTypeFields(fields, tc); err != nil {
		t.Errorf("plan transcript turn bead failed validation: %v", err)
	}
}

func TestResolveTypeConfig_WritePolicyMergedFromBuiltin(t *testing.T) {
	srv, ms, ctx := testCtx(t)

	// Store entry without write_policy — builtin's policy should be merged in.
	ms.configs["type:agent"] = &model.Config{
		Key: "type:agent",
		Value: json.RawMessage(`{
			"kind": "config",
			"fields": [
				{"name": "agent", "type": "string", "required": true}
			]
		}`),
	}

	tc, err := srv.resolveTypeConfig(ctx, "agent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tc.WritePolicy == nil {
		t.Fatal("expected builtin write_policy to be merged, got nil")
	}
	if !tc.WritePolicy.ProjectScoped {
		t.Error("merged write_policy.project_scoped should be true")
	}
}

func TestResolveTypeConfig_WritePolicyNotOverridden(t *testing.T) {
	srv, ms, ctx := testCtx(t)

	// Store entry WITH its own write_policy — should NOT be overridden by builtin.
	ms.configs["type:agent"] = &model.Config{
		Key: "type:agent",
		Value: json.RawMessage(`{
			"kind": "config",
			"write_policy": {"min_role": "gardenio-admin", "project_scoped": false},
			"fields": [
				{"name": "agent", "type": "string", "required": true}
			]
		}`),
	}

	tc, err := srv.resolveTypeConfig(ctx, "agent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tc.WritePolicy == nil {
		t.Fatal("expected write_policy, got nil")
	}
	if tc.WritePolicy.MinRole != "gardenio-admin" {
		t.Errorf("write_policy.min_role=%q, want gardenio-admin", tc.WritePolicy.MinRole)
	}
	if tc.WritePolicy.ProjectScoped {
		t.Error("store write_policy.project_scoped should be false (not overridden by builtin)")
	}
}

// TestResolveTypeConfig_UnionsBuiltinEnumValues locks in the fix for
// ka-0000003824: a stale store config for type:trigger predating the
// schedule-mirror dual-write fix had `source` enum values that did NOT
// include "schedule-mirror". The merge logic previously only added
// builtin fields that were entirely absent from the store config —
// matching fields with extended enums were silently kept stale, so the
// daemon rejected every dual-write attempt with HTTP 400. The fix:
// union builtin enum values into matching store-config enum fields.
func TestResolveTypeConfig_UnionsBuiltinEnumValues(t *testing.T) {
	srv, ms, ctx := testCtx(t)

	// Simulate the actual stale store config that was in prod on 2026-05-23:
	// `source` enum lacks "schedule-mirror" even though the builtin lists it.
	ms.configs["type:trigger"] = &model.Config{
		Key: "type:trigger",
		Value: json.RawMessage(`{
			"kind": "data",
			"fields": [
				{"name": "source", "type": "enum", "values": ["jira", "gitlab", "webhook", "bead-state", "schedule", "slack-reaction", "account-cost"]},
				{"name": "enabled", "type": "boolean"}
			]
		}`),
	}

	tc, err := srv.resolveTypeConfig(ctx, "trigger")
	if err != nil {
		t.Fatalf("resolveTypeConfig: %v", err)
	}
	if tc == nil {
		t.Fatal("expected trigger type config, got nil")
	}

	source := fieldByName(tc, "source")
	if source == nil {
		t.Fatal("source field missing after merge")
	}
	if source.Type != model.FieldTypeEnum {
		t.Fatalf("source field type=%q, want enum", source.Type)
	}
	have := make(map[string]bool, len(source.Values))
	for _, v := range source.Values {
		have[v] = true
	}
	// Store-defined values must survive.
	for _, v := range []string{"jira", "gitlab", "schedule"} {
		if !have[v] {
			t.Errorf("store enum value %q dropped after merge", v)
		}
	}
	// Builtin-only enum value must be added.
	if !have["schedule-mirror"] {
		t.Errorf("builtin enum value 'schedule-mirror' not merged into store config; got %v", source.Values)
	}
}

// TestResolveTypeConfig_PreservesStoreEnumValuesNotInBuiltin verifies the
// enum-merge is a union: store-only enum values are NOT dropped.
func TestResolveTypeConfig_PreservesStoreEnumValuesNotInBuiltin(t *testing.T) {
	srv, ms, ctx := testCtx(t)

	ms.configs["type:trigger"] = &model.Config{
		Key: "type:trigger",
		Value: json.RawMessage(`{
			"kind": "data",
			"fields": [
				{"name": "source", "type": "enum", "values": ["jira", "operator-custom-source"]}
			]
		}`),
	}

	tc, err := srv.resolveTypeConfig(ctx, "trigger")
	if err != nil {
		t.Fatalf("resolveTypeConfig: %v", err)
	}
	source := fieldByName(tc, "source")
	if source == nil {
		t.Fatal("source field missing")
	}
	have := make(map[string]bool, len(source.Values))
	for _, v := range source.Values {
		have[v] = true
	}
	if !have["operator-custom-source"] {
		t.Errorf("store-only enum value 'operator-custom-source' dropped; got %v", source.Values)
	}
	if !have["schedule-mirror"] {
		t.Errorf("builtin enum value 'schedule-mirror' not merged; got %v", source.Values)
	}
}

// TestResolveTypeConfig_MergesFormulaStepFieldsOverStaleStore is the
// config-shadowing regression for ka-0000002559. The live daemon resolves
// type:task from a STORED KV config (written 2026-06-29, 44 fields) that
// SHADOWS the builtin and does NOT declare the formula step-machinery fields
// (waits_for/on_complete/expansion_var/dwf_result). Because those fields were
// only added to the builtin, this test proves the resolveTypeConfig merge
// (which appends builtin-only fields absent from the store config — the same
// mechanism that unions builtin enum values, ka-0000003824) surfaces them even
// when a stale store row shadows the builtin. No manual SetConfig patch of the
// stored KV is required: the merge does it at resolution time, so the create
// path validates against the merged superset.
func TestResolveTypeConfig_MergesFormulaStepFieldsOverStaleStore(t *testing.T) {
	srv, ms, ctx := testCtx(t)

	// Simulate the stale 2026-06-29 store config: a couple of the legacy
	// fields, none of the four formula step-machinery fields.
	ms.configs["type:task"] = &model.Config{
		Key: "type:task",
		Value: json.RawMessage(`{
			"kind": "issue",
			"fields": [
				{"name": "source_formula", "type": "string"},
				{"name": "source_step_id", "type": "string"}
			]
		}`),
	}

	tc, err := srv.resolveTypeConfig(ctx, "task")
	if err != nil {
		t.Fatalf("resolveTypeConfig: %v", err)
	}
	if tc == nil {
		t.Fatal("expected task type config, got nil")
	}

	for _, tt := range []struct {
		name    string
		theType model.FieldType
	}{
		{"waits_for", model.FieldTypeString},
		{"on_complete", model.FieldTypeJSON},
		{"expansion_var", model.FieldTypeString},
		{"dwf_result", model.FieldTypeJSON},
	} {
		fd := fieldByName(tc, tt.name)
		if fd == nil {
			t.Errorf("field %q not merged into resolved config over stale store (create path would still 400)", tt.name)
			continue
		}
		if fd.Type != tt.theType {
			t.Errorf("merged field %q type=%q, want %q", tt.name, fd.Type, tt.theType)
		}
	}

	// The stale store fields must survive the merge (union, not replace).
	if fieldByName(tc, "source_formula") == nil {
		t.Error("store field 'source_formula' dropped after merge")
	}
}

// TestResolveTypeConfig_RestoresAllowAdditionalFieldsOverStaleStore is the
// regression for ka-0000002372. The prod store row for type:config (created
// 2026-03-05) predated the allow_additional_fields flag and so left it at the
// false zero value, SHADOWING the builtin's true. resolveTypeConfig merged
// builtin fields + write_policy but never restored AllowAdditionalFields, so
// the open-schema type:config was pinned to reject-unknown-fields and every
// `kd -f key=val` on a config bead 400'd 'unknown field' — the config-bead
// runtime-tunable path went dead on prod for any NEW key. This test simulates
// that stale row (no allow_additional_fields) and pins that the merge restores
// the builtin's true, so an arbitrary open-schema key validates without a
// manual SetConfig roll-forward of the stored KV.
func TestResolveTypeConfig_RestoresAllowAdditionalFieldsOverStaleStore(t *testing.T) {
	srv, ms, ctx := testCtx(t)

	// Simulate the stale 2026-03-05 store config: the `value` field only, with
	// allow_additional_fields omitted (unmarshals to the false zero value).
	ms.configs["type:config"] = &model.Config{
		Key: "type:config",
		Value: json.RawMessage(`{
			"kind": "config",
			"fields": [
				{"name": "value", "type": "json"}
			]
		}`),
	}

	tc, err := srv.resolveTypeConfig(ctx, "config")
	if err != nil {
		t.Fatalf("resolveTypeConfig: %v", err)
	}
	if tc == nil {
		t.Fatal("expected config type config, got nil")
	}

	if !tc.AllowAdditionalFields {
		t.Fatal("AllowAdditionalFields not restored from builtin over the stale store row — the live daemon would still 400 every config-bead -f write of a new key")
	}

	// An arbitrary open-schema key (not in Fields) must now validate, mirroring
	// a `kd -f show_plan_badges=true` write on a config bead.
	arbitrary := json.RawMessage(`{"show_plan_badges": "true"}`)
	if err := model.ValidateTypeFields(arbitrary, tc); err != nil {
		t.Errorf("open-schema key should validate after AllowAdditionalFields restore, got %v", err)
	}

	// The stale store field must survive the merge (union, not replace).
	if fieldByName(tc, "value") == nil {
		t.Error("store field 'value' dropped after merge")
	}
}

// TestResolveTypeConfig_AgentBuiltinIsOpenSchema pins the G3 flip
// (task gd-IAIkeME5YU, decision ka-0000007573 / HYBRID option c, epic
// ka-0000002054): the type:agent builtin now carries allow_additional_fields:true
// so an undeclared controller/bridge field degrades to a bag-of-attributes write
// instead of 400'ing the WHOLE SpawnAgent create/revive — the class of four
// same-class schema-drift outages this flip targets. Crucially, DECLARED fields
// keep FULL type validation, so this also pins that a bad-typed known field and a
// bad enum value are still rejected under the open schema (the G4 runtime alarm
// ka-0000007249 owns the residual "typo lands silently" risk on unknown keys).
func TestResolveTypeConfig_AgentBuiltinIsOpenSchema(t *testing.T) {
	srv, _, ctx := testCtx(t)

	tc, err := srv.resolveTypeConfig(ctx, "agent")
	if err != nil {
		t.Fatalf("resolveTypeConfig: %v", err)
	}
	if tc == nil {
		t.Fatal("expected agent type config, got nil")
	}
	if !tc.AllowAdditionalFields {
		t.Fatal("type:agent builtin must resolve with AllowAdditionalFields==true after the G3 flip (gd-IAIkeME5YU) — an undeclared controller/bridge field would otherwise 400 the whole spawn/revive")
	}

	// The three required fields, satisfied so each payload below isolates the one
	// behavior under test rather than tripping the required-field check first.
	const req = `"agent":"a","role":"crew","project":"gardenio"`

	// An unknown key must now be accepted (bag-of-attributes), mirroring a future
	// controller/bridge field landing before it is declared on the builtin.
	unknown := json.RawMessage(`{` + req + `,"some_new_controller_field": "v"}`)
	if err := model.ValidateTypeFields(unknown, tc); err != nil {
		t.Errorf("unknown field should validate under open schema, got %v", err)
	}

	// Declared typed fields still get full validation: a non-int total_input_tokens
	// must be rejected even with the open schema.
	badTyped := json.RawMessage(`{` + req + `,"total_input_tokens": "not-an-int"}`)
	if err := model.ValidateTypeFields(badTyped, tc); err == nil {
		t.Error("bad-typed declared field (total_input_tokens=string) must still be rejected under open schema")
	}

	// A declared enum still validates its value set under the open schema.
	badEnum := json.RawMessage(`{` + req + `,"agent_state": "not-a-state"}`)
	if err := model.ValidateTypeFields(badEnum, tc); err == nil {
		t.Error("bad enum value (agent_state) must still be rejected under open schema")
	}
}

// TestResolveTypeConfig_MergesRequestingPlanOverStaleEscalationStore is the
// live-daemon half of the ka-0000000658 fix. Declaring requesting_plan in the
// type:escalation BUILTIN only helps if it reaches the running daemon, whose
// stored type:escalation row (written 2026-04-12, 18 fields) SHADOWS the
// builtin and predates every post-April field — it lacks not just
// requesting_plan but the whole reflag_*/pingback_* re-surface set. This test
// pins the same merge mechanism used for the formula step fields above: the
// builtin-only fields are appended at resolution time, so `gb escalation
// create --requesting-task <plan-anchored task>` validates against the merged
// superset and its plan pin lands with NO manual SetConfig roll-forward of the
// stored KV.
func TestResolveTypeConfig_MergesRequestingPlanOverStaleEscalationStore(t *testing.T) {
	srv, ms, ctx := testCtx(t)

	// The shape of the live 2026-04-12 store row: create-time operation fields
	// only, no requesting_plan and no re-surface bookkeeping.
	ms.configs["type:escalation"] = &model.Config{
		Key: "type:escalation",
		Value: json.RawMessage(`{
			"kind": "data",
			"fields": [
				{"name": "operation_type",  "type": "string", "required": true},
				{"name": "operation_spec",  "type": "json",   "required": true},
				{"name": "requesting_agent","type": "string"},
				{"name": "requesting_task", "type": "string"}
			]
		}`),
	}

	tc, err := srv.resolveTypeConfig(ctx, "escalation")
	if err != nil {
		t.Fatalf("resolveTypeConfig: %v", err)
	}
	if tc == nil {
		t.Fatal("expected escalation type config, got nil")
	}

	fd := fieldByName(tc, "requesting_plan")
	if fd == nil {
		t.Fatal("requesting_plan not merged over the stale store row — the live daemon would still 400 the plan pin")
	}
	if fd.Type != model.FieldTypeString {
		t.Errorf("merged requesting_plan type=%q, want %q", fd.Type, model.FieldTypeString)
	}

	// The stale store fields must survive the merge (union, not replace).
	if fieldByName(tc, "requesting_task") == nil {
		t.Error("store field 'requesting_task' dropped after merge")
	}
}

// TestResolveTypeConfig_MergesThreadIDOverStaleEscalationStore is the
// live-daemon half of the ka-0000000226 fix, the twin of the requesting_plan
// test above. The same stale 2026-04-12 store row that shadows the builtin
// lacks thread_id, so declaring it in the builtin only helps if the merge
// appends it at resolution time — otherwise the running daemon would still
// 400 an explicit escalation create that supplies thread_id, dropping the
// escalation out of its Slack thread.
func TestResolveTypeConfig_MergesThreadIDOverStaleEscalationStore(t *testing.T) {
	srv, ms, ctx := testCtx(t)

	// The shape of the live 2026-04-12 store row: create-time operation fields
	// only, no thread_id and no re-surface bookkeeping.
	ms.configs["type:escalation"] = &model.Config{
		Key: "type:escalation",
		Value: json.RawMessage(`{
			"kind": "data",
			"fields": [
				{"name": "operation_type",  "type": "string", "required": true},
				{"name": "operation_spec",  "type": "json",   "required": true},
				{"name": "requesting_agent","type": "string"},
				{"name": "requesting_task", "type": "string"}
			]
		}`),
	}

	tc, err := srv.resolveTypeConfig(ctx, "escalation")
	if err != nil {
		t.Fatalf("resolveTypeConfig: %v", err)
	}
	if tc == nil {
		t.Fatal("expected escalation type config, got nil")
	}

	fd := fieldByName(tc, "thread_id")
	if fd == nil {
		t.Fatal("thread_id not merged over the stale store row — the live daemon would still 400 an escalation created inside a Slack thread")
	}
	if fd.Type != model.FieldTypeString {
		t.Errorf("merged thread_id type=%q, want %q", fd.Type, model.FieldTypeString)
	}

	// The stale store fields must survive the merge (union, not replace).
	if fieldByName(tc, "requesting_task") == nil {
		t.Error("store field 'requesting_task' dropped after merge")
	}
}

// TestResolveTypeConfig_MergesRequestingTaskPlanOverStaleDecisionStore is the
// live-daemon half of the ka-0000003927 fix: `gb decision create
// --requesting-task` now stamps requesting_task at create and PATCHes
// requesting_plan when the task sits under a plan-labeled epic. type:decision
// does NOT AllowAdditionalFields, so both keys must be declared in the schema
// or the daemon 400s the very command that adds them — exactly the failure the
// escalation side hit (ka-0000000658). A stale stored type:decision row that
// predates these fields shadows the builtin, so declaring them in the builtin
// only helps if the merge appends them at resolution time. This pins that
// merge for decisions (the twin of the escalation requesting_plan test above).
func TestResolveTypeConfig_MergesRequestingTaskPlanOverStaleDecisionStore(t *testing.T) {
	srv, ms, ctx := testCtx(t)

	// A stale stored decision row: the original create-time fields only, with
	// neither requesting_task nor requesting_plan declared.
	ms.configs["type:decision"] = &model.Config{
		Key: "type:decision",
		Value: json.RawMessage(`{
			"kind": "data",
			"fields": [
				{"name": "prompt",                   "type": "string", "required": true},
				{"name": "options",                  "type": "json"},
				{"name": "requested_by",             "type": "string"},
				{"name": "requesting_agent_bead_id", "type": "string"}
			]
		}`),
	}

	tc, err := srv.resolveTypeConfig(ctx, "decision")
	if err != nil {
		t.Fatalf("resolveTypeConfig: %v", err)
	}
	if tc == nil {
		t.Fatal("expected decision type config, got nil")
	}

	for _, name := range []string{"requesting_task", "requesting_plan"} {
		fd := fieldByName(tc, name)
		if fd == nil {
			t.Fatalf("%s not merged over the stale store row — the live daemon would 400 `gb decision create --requesting-task`", name)
		}
		if fd.Type != model.FieldTypeString {
			t.Errorf("merged %s type=%q, want %q", name, fd.Type, model.FieldTypeString)
		}
	}

	// The stale store fields must survive the merge (union, not replace).
	if fieldByName(tc, "requesting_agent_bead_id") == nil {
		t.Error("store field 'requesting_agent_bead_id' dropped after merge")
	}
}

func TestResolveTypeConfig_NoWritePolicy(t *testing.T) {
	srv, _, ctx := testCtx(t)

	// Task type has no write_policy in builtins.
	tc, err := srv.resolveTypeConfig(ctx, "task")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tc == nil {
		t.Fatal("expected task type config, got nil")
	}
	if tc.WritePolicy != nil {
		t.Errorf("expected nil write_policy on task type, got %+v", tc.WritePolicy)
	}
}

// --- Schema validation helpers ---

// parseBuiltinSchema unmarshals the builtin type config for a given type key.
func parseBuiltinSchema(t *testing.T, typeName string) *model.TypeConfig {
	t.Helper()
	cfg, ok := builtinConfigs["type:"+typeName]
	if !ok {
		t.Fatalf("builtinConfigs missing type:%s", typeName)
	}
	var tc model.TypeConfig
	if err := json.Unmarshal(cfg.Value, &tc); err != nil {
		t.Fatalf("failed to parse type:%s config: %v", typeName, err)
	}
	return &tc
}

// fieldByName finds a field definition by name in a type config.
func fieldByName(tc *model.TypeConfig, name string) *model.FieldDef {
	for i := range tc.Fields {
		if tc.Fields[i].Name == name {
			return &tc.Fields[i]
		}
	}
	return nil
}

// --- Builtin schema smoke test ---

func TestBuiltinSchemas_AllParseClean(t *testing.T) {
	for key, cfg := range builtinConfigs {
		if len(key) > 5 && key[:5] == "type:" {
			var tc model.TypeConfig
			if err := json.Unmarshal(cfg.Value, &tc); err != nil {
				t.Errorf("type config %q has invalid JSON: %v", key, err)
			}
		}
	}
}

// TestIssueKindTypes_HaveMRFields verifies every issue-kind builtin type
// declares the MR-tracking fields the gitlab-bridge writes. Without these
// fields, `gb mr link` returns HTTP 400 "mr_url: unknown field" when the
// target bead is anything other than a task.
func TestIssueKindTypes_HaveMRFields(t *testing.T) {
	mrFieldNames := []string{
		"forge",
		"mr_url",
		"mr_iid",
		"mr_title",
		"mr_state",
		"mr_merged",
		"mr_pipeline_status",
		"mr_pipeline_url",
		"mr_approved",
		"mr_approvers",
		"mr_has_review_comments",
		"gitlab_mr_iid",
		"gitlab_project_id",
		"gitlab_discussion_id",
		"gitlab_mr_source_branch",
		"mr_close_override",
		"trickle_checklist",
	}
	issueTypes := []string{"task", "bug", "epic", "feature", "chore"}
	for _, typeName := range issueTypes {
		tc := parseBuiltinSchema(t, typeName)
		for _, name := range mrFieldNames {
			fd := fieldByName(tc, name)
			if fd == nil {
				t.Errorf("type:%s missing MR field %q", typeName, name)
				continue
			}
			if fd.Type != model.FieldTypeString {
				t.Errorf("type:%s field %q has type %q, want string", typeName, name, fd.Type)
			}
		}
	}
}

// TestIssueKindTypes_HaveAuditFields locks in the bridge contract from
// commit b419a322 ("push operated_by at create-time"): the issue-kind
// task/bug/epic/feature/chore configs MUST accept operated_by,
// gitlab_username, and jira_user_id in Fields, otherwise mention-spawned
// task creates from the GitLab/JIRA bridges fail HTTP 400 validation
// and no agent is spawned. See ka-0000006941 for the regression case.
func TestIssueKindTypes_HaveAuditFields(t *testing.T) {
	auditFieldNames := []string{"operated_by", "gitlab_username", "jira_user_id"}
	issueTypes := []string{"task", "bug", "epic", "feature", "chore"}
	for _, typeName := range issueTypes {
		tc := parseBuiltinSchema(t, typeName)
		for _, name := range auditFieldNames {
			fd := fieldByName(tc, name)
			if fd == nil {
				t.Errorf("type:%s missing audit field %q", typeName, name)
				continue
			}
			if fd.Type != model.FieldTypeString {
				t.Errorf("type:%s field %q has type %q, want string", typeName, name, fd.Type)
			}
		}
	}
}

// TestAgentSchema_HasAuditFields locks in the SpawnAgent contract: the
// extraFields map merged into the agent bead must include operated_by
// (read by operatedByFromAgentBead as the static-token-write fallback)
// plus gitlab_username / jira_user_id for cross-bridge identity.
// Regression guard for ka-0000006941.
func TestAgentSchema_HasAuditFields(t *testing.T) {
	tc := parseBuiltinSchema(t, "agent")
	for _, name := range []string{"operated_by", "gitlab_username", "jira_user_id"} {
		fd := fieldByName(tc, name)
		if fd == nil {
			t.Errorf("type:agent missing audit field %q", name)
			continue
		}
		if fd.Type != model.FieldTypeString {
			t.Errorf("type:agent field %q has type %q, want string", name, fd.Type)
		}
	}
}

// TestSlackThreadBeadIDField locks in the slack-bridge thread-spawn
// contract: both task and agent types must accept slack_thread_bead_id
// so the bridge can stamp the resolved thread bead onto task and agent
// beads at create/update time. Without these fields, kbeads returns
// HTTP 400 "slack_thread_bead_id: unknown field" and every @gardenio
// thread mention fails. Regression guard for gd-1X0Xc4RCYr.
func TestSlackThreadBeadIDField(t *testing.T) {
	for _, typeName := range []string{"task", "agent"} {
		tc := parseBuiltinSchema(t, typeName)
		fd := fieldByName(tc, "slack_thread_bead_id")
		if fd == nil {
			t.Errorf("type:%s missing field slack_thread_bead_id", typeName)
			continue
		}
		if fd.Type != model.FieldTypeString {
			t.Errorf("type:%s slack_thread_bead_id type=%q, want string", typeName, fd.Type)
		}
	}
}

// TestUserSchema_HasProfilePrefs pins the type:user builtin to a json-typed
// profile_prefs field. The beads-viewer Pin/Unpin button writes inbox-pin
// state into profile_prefs on the type=user kind=config bead via
// UpdateBeadFieldsTyped; without this field (and with allowAdditionalFields
// unset, i.e. false) the daemon rejects the PATCH with HTTP 400
// "profile_prefs: unknown field" and the viewer surfaces a 502
// "Failed to toggle pin." (bug gd-NlwSPbsrUB).
func TestUserSchema_HasProfilePrefs(t *testing.T) {
	tc := parseBuiltinSchema(t, "user")
	fd := fieldByName(tc, "profile_prefs")
	if fd == nil {
		t.Fatal("type:user missing field profile_prefs")
	}
	if fd.Type != model.FieldTypeJSON {
		t.Errorf("type:user profile_prefs type=%q, want json", fd.Type)
	}
}

// TestUserSchema_HasStatusMessage pins the type:user builtin to a string-typed
// status_message field. The beads-viewer self-only status-message editor on
// /u/{slug} writes the value into status_message on the type=user kind=config
// bead via UpdateBeadFieldsTyped; without this field (and with
// allowAdditionalFields unset, i.e. false) the daemon rejects the PATCH with
// HTTP 400 "status_message: unknown field" and the viewer surfaces a 502
// "Failed to save status message." (bug gd-hfW7c7oWD2).
func TestUserSchema_HasStatusMessage(t *testing.T) {
	tc := parseBuiltinSchema(t, "user")
	fd := fieldByName(tc, "status_message")
	if fd == nil {
		t.Fatal("type:user missing field status_message")
	}
	if fd.Type != model.FieldTypeString {
		t.Errorf("type:user status_message type=%q, want string", fd.Type)
	}
}

// --- Agent type schema tests (commits 89969fa7, d231e37d) ---

func TestAgentSchema_NumericFieldsRejectStrings(t *testing.T) {
	tc := parseBuiltinSchema(t, "agent")

	numericFields := []struct {
		name     string
		wantType model.FieldType
	}{
		{"total_input_tokens", model.FieldTypeInteger},
		{"total_output_tokens", model.FieldTypeInteger},
		{"total_thinking_tokens", model.FieldTypeInteger},
		{"total_cache_read_tokens", model.FieldTypeInteger},
		{"total_cache_write_tokens", model.FieldTypeInteger},
		{"total_request_count", model.FieldTypeInteger},
		{"total_tool_calls", model.FieldTypeInteger},
		{"total_errors", model.FieldTypeInteger},
		{"total_compactions", model.FieldTypeInteger},
		{"session_count", model.FieldTypeInteger},
		{"yield_count", model.FieldTypeInteger},
		{"live_turns", model.FieldTypeInteger},
		{"total_cost_usd", model.FieldTypeFloat},
	}

	for _, nf := range numericFields {
		fd := fieldByName(tc, nf.name)
		if fd == nil {
			t.Errorf("agent schema missing field %q", nf.name)
			continue
		}
		if fd.Type != nf.wantType {
			t.Errorf("agent.%s type=%q, want %q", nf.name, fd.Type, nf.wantType)
		}
	}

	// Validate that string values are rejected by the validator for each field.
	for _, nf := range numericFields {
		fields := json.RawMessage(`{"agent":"a","role":"r","project":"p","` + nf.name + `":"not-a-number"}`)
		err := model.ValidateTypeFields(fields, tc)
		if err == nil {
			t.Errorf("agent.%s should reject string value, got nil error", nf.name)
		}
	}
}

func TestAgentSchema_NumericFieldsAcceptNumbers(t *testing.T) {
	tc := parseBuiltinSchema(t, "agent")

	// Integer fields accept whole numbers.
	fields := json.RawMessage(`{"agent":"a","role":"r","project":"p","total_input_tokens":12345,"yield_count":3}`)
	if err := model.ValidateTypeFields(fields, tc); err != nil {
		t.Fatalf("agent integer fields should accept whole numbers: %v", err)
	}

	// Float field accepts decimals.
	fields = json.RawMessage(`{"agent":"a","role":"r","project":"p","total_cost_usd":1.23}`)
	if err := model.ValidateTypeFields(fields, tc); err != nil {
		t.Fatalf("agent.total_cost_usd should accept float: %v", err)
	}
}

func TestAgentSchema_CloseReasonField(t *testing.T) {
	tc := parseBuiltinSchema(t, "agent")

	fd := fieldByName(tc, "close_reason")
	if fd == nil {
		t.Fatal("agent schema missing close_reason field")
	}
	if fd.Type != model.FieldTypeString {
		t.Errorf("close_reason type=%q, want string", fd.Type)
	}

	// Accepts a string value.
	fields := json.RawMessage(`{"agent":"a","role":"r","project":"p","close_reason":"budget_exceeded"}`)
	if err := model.ValidateTypeFields(fields, tc); err != nil {
		t.Fatalf("close_reason should accept string: %v", err)
	}

	// Rejects non-string value.
	fields = json.RawMessage(`{"agent":"a","role":"r","project":"p","close_reason":42}`)
	if err := model.ValidateTypeFields(fields, tc); err == nil {
		t.Error("close_reason should reject non-string value")
	}
}

func TestAgentSchema_AgentStateEnum(t *testing.T) {
	tc := parseBuiltinSchema(t, "agent")

	fd := fieldByName(tc, "agent_state")
	if fd == nil {
		t.Fatal("agent schema missing agent_state")
	}
	if fd.Type != model.FieldTypeEnum {
		t.Fatalf("agent_state type=%q, want enum", fd.Type)
	}

	// Every value the controller writes through the enum-validated PATCH path
	// (UpdateBeadFields/UpdateAgentState -> ValidateTypeFieldsForUpdate) must be
	// admitted here, or the daemon silently drops the write ("must be one of
	// [...]") on any fresh daemon that lacks a stored type:agent override (a
	// fresh gasbox/GiB instance, a fresh e2e namespace, a restored-from-builtin
	// deployment). "resuming"/"resume_failed" were the microvm suspend/resume
	// states restored by ka-0000000585 (!6614); "queued"/"model_rate_limited"/
	// "stop_requested" are the three follow-up states restored by ka-0000006367:
	//   queued             — reconciler markQueued + spawn-declined/pool-exhausted
	//   model_rate_limited — in-pod coop ErrorCategory=model_rate_limited
	//   stop_requested     — RequestAgentStop (gb agent kill --request-stop)
	// "killed" is the close-time state `gb agent kill` writes (agent_remote.go,
	// gd-CTvPgeXKc5); the bridge wrapup and spawn-continuation authz read it.
	// "paused" is the stale reaper's hold on a laptop agent whose runner is
	// away (local-agents D5, gd-PjlxxDPvaz).
	// Keep this list in lockstep with the config.go builtin enum AND with
	// BUILTIN_AGENT_STATES in gardenio/tests/e2e/scripts/test-microvm-execution-target.sh.
	wantValues := []string{"idle", "spawning", "assigning", "working", "stopping", "done", "failed", "rate_limited", "budget_exceeded", "prewarmed", "resuming", "resume_failed", "queued", "model_rate_limited", "stop_requested", "killed", "paused"}
	for _, v := range wantValues {
		fields := json.RawMessage(`{"agent":"a","role":"r","project":"p","agent_state":"` + v + `"}`)
		if err := model.ValidateTypeFields(fields, tc); err != nil {
			t.Errorf("agent_state should accept %q: %v", v, err)
		}
	}

	// Invalid enum value.
	fields := json.RawMessage(`{"agent":"a","role":"r","project":"p","agent_state":"unknown_state"}`)
	if err := model.ValidateTypeFields(fields, tc); err == nil {
		t.Error("agent_state should reject invalid enum value")
	}
}

// --- Advice type schema tests (commit 3392b408) ---

func TestAdviceSchema_MemoryFields(t *testing.T) {
	tc := parseBuiltinSchema(t, "advice")

	memFields := []struct {
		name     string
		wantType model.FieldType
	}{
		{"content_hash", model.FieldTypeString},
		{"memory_category", model.FieldTypeString},
		{"tags", model.FieldTypeString},
		{"supersedes", model.FieldTypeString},
	}

	for _, mf := range memFields {
		fd := fieldByName(tc, mf.name)
		if fd == nil {
			t.Errorf("advice schema missing field %q", mf.name)
			continue
		}
		if fd.Type != mf.wantType {
			t.Errorf("advice.%s type=%q, want %q", mf.name, fd.Type, mf.wantType)
		}
	}
}

func TestAdviceSchema_ConfidenceAndConfirmations(t *testing.T) {
	tc := parseBuiltinSchema(t, "advice")

	fd := fieldByName(tc, "confidence")
	if fd == nil {
		t.Fatal("advice schema missing confidence")
	}
	if fd.Type != model.FieldTypeFloat {
		t.Errorf("advice.confidence type=%q, want float", fd.Type)
	}

	fd = fieldByName(tc, "confirmations")
	if fd == nil {
		t.Fatal("advice schema missing confirmations")
	}
	if fd.Type != model.FieldTypeInteger {
		t.Errorf("advice.confirmations type=%q, want integer", fd.Type)
	}

	// confidence rejects string, confirmations rejects float.
	fields := json.RawMessage(`{"confidence":"high"}`)
	if err := model.ValidateTypeFields(fields, tc); err == nil {
		t.Error("advice.confidence should reject string")
	}

	fields = json.RawMessage(`{"confirmations":1.5}`)
	if err := model.ValidateTypeFields(fields, tc); err == nil {
		t.Error("advice.confirmations should reject non-integer float")
	}

	// Valid values pass.
	fields = json.RawMessage(`{"confidence":0.95,"confirmations":3}`)
	if err := model.ValidateTypeFields(fields, tc); err != nil {
		t.Fatalf("valid confidence/confirmations should pass: %v", err)
	}
}

// --- Project/Role type schema tests (memory_request, memory_limit) ---

func TestProjectSchema_ResourceFields(t *testing.T) {
	tc := parseBuiltinSchema(t, "project")

	for _, name := range []string{"memory_request", "memory_limit", "cpu_request", "cpu_limit"} {
		fd := fieldByName(tc, name)
		if fd == nil {
			t.Errorf("project schema missing field %q", name)
			continue
		}
		if fd.Type != model.FieldTypeString {
			t.Errorf("project.%s type=%q, want string", name, fd.Type)
		}
	}

	// Accept K8s resource strings.
	fields := json.RawMessage(`{"memory_request":"512Mi","memory_limit":"2Gi","cpu_request":"500m","cpu_limit":"2"}`)
	if err := model.ValidateTypeFields(fields, tc); err != nil {
		t.Fatalf("resource fields should accept strings: %v", err)
	}

	// Reject non-string.
	fields = json.RawMessage(`{"memory_request":512}`)
	if err := model.ValidateTypeFields(fields, tc); err == nil {
		t.Error("memory_request should reject non-string value")
	}
}

func TestRoleSchema_ResourceFields(t *testing.T) {
	tc := parseBuiltinSchema(t, "role")

	for _, name := range []string{"memory_request", "memory_limit"} {
		fd := fieldByName(tc, name)
		if fd == nil {
			t.Errorf("role schema missing field %q", name)
			continue
		}
		if fd.Type != model.FieldTypeString {
			t.Errorf("role.%s type=%q, want string", name, fd.Type)
		}
	}
}

// TestTriggerSchema_HasScheduleFields verifies the type:trigger schema
// declares the cron/timezone/last_run fields the schedule trigger source
// reads and writes (gardenio/controller/internal/bridge/trigger_source_schedule.go).
// Without these fields, `gb trigger create schedule` fails with HTTP 400
// "cron: unknown field; timezone: unknown field" (gd-7N4wGnadC7).
func TestTriggerSchema_HasScheduleFields(t *testing.T) {
	tc := parseBuiltinSchema(t, "trigger")
	for _, name := range []string{"cron", "timezone", "last_run"} {
		fd := fieldByName(tc, name)
		if fd == nil {
			t.Errorf("trigger schema missing field %q", name)
			continue
		}
		if fd.Type != model.FieldTypeString {
			t.Errorf("trigger.%s type=%q, want string", name, fd.Type)
		}
	}

	// Field-level: a payload mirroring `gb trigger create schedule` output
	// must validate cleanly against the builtin trigger schema.
	fields := json.RawMessage(`{
		"action":           "spawn_agent",
		"cron":             "3,23,43 * * * *",
		"timezone":         "America/Los_Angeles",
		"conditions":       "[]",
		"role":             "gardener",
		"project":          "gardenio",
		"prompt":           "test",
		"enabled":          true,
		"max_concurrent":   1,
		"cooldown_minutes": 18
	}`)
	if err := model.ValidateTypeFields(fields, tc); err != nil {
		t.Fatalf("schedule-trigger payload should validate: %v", err)
	}
}

// TestTriggerSchema_HasBlockedByField verifies the type:trigger schema
// declares the blocked_by field that `gb trigger block`/`unblock` writes
// (gardenio/controller/cmd/gb/trigger_block.go). The field is a JSON-encoded
// string (either a marshalled array or empty), so the schema declares it as
// "string". Without this field, `gb trigger block` fails with HTTP 400
// "blocked_by: unknown field" (gd-6DwWbFQB7S).
func TestTriggerSchema_HasBlockedByField(t *testing.T) {
	tc := parseBuiltinSchema(t, "trigger")
	fd := fieldByName(tc, "blocked_by")
	if fd == nil {
		t.Fatalf("trigger schema missing field %q", "blocked_by")
	}
	if fd.Type != model.FieldTypeString {
		t.Errorf("trigger.blocked_by type=%q, want string", fd.Type)
	}

	// Field-level: a payload mirroring `gb trigger block`'s update (JSON-
	// encoded array as string) must validate cleanly against the builtin
	// trigger schema.
	fields := json.RawMessage(`{
		"action":     "spawn_agent",
		"blocked_by": "[\"gd-decFGHIJ\"]"
	}`)
	if err := model.ValidateTypeFields(fields, tc); err != nil {
		t.Fatalf("blocked-trigger payload should validate: %v", err)
	}

	// Empty-string form (set when the last decision is unblocked) must also
	// validate, since `gb trigger unblock` writes "" when the list empties.
	emptyFields := json.RawMessage(`{
		"action":     "spawn_agent",
		"blocked_by": ""
	}`)
	if err := model.ValidateTypeFields(emptyFields, tc); err != nil {
		t.Fatalf("unblocked-trigger payload should validate: %v", err)
	}
}

// TestTriggerSchema_HasGardenerLoopFields verifies the type:trigger schema
// declares the gardener loop-spec fields a gardener bead carries. A gardener is a
// type=trigger bead whose stop/progress/run_budget/etc. the trigger-engine
// decodes via GardenerSpec (gardenio/controller/internal/bridge/triggers.go) and
// `gb gardener` / the gardener-loop runbook author as flat top-level fields.
// Without these the create 400s with "unknown field" (ka-0000002027).
func TestTriggerSchema_HasGardenerLoopFields(t *testing.T) {
	tc := parseBuiltinSchema(t, "trigger")

	// The 10 gardener loop-spec keys called out in the task, plus the field
	// types they must validate as.
	wantTypes := map[string]model.FieldType{
		"stop":                 model.FieldTypeJSON,
		"progress":             model.FieldTypeJSON,
		"run_budget":           model.FieldTypeInteger,
		"max_runs":             model.FieldTypeInteger,
		"terminal_goal":        model.FieldTypeString,
		"assign_bead":          model.FieldTypeString,
		"exit_condition":       model.FieldTypeString,
		"run_timeout":          model.FieldTypeString,
		"consecutive_failures": model.FieldTypeInteger,
		"triggers":             model.FieldTypeJSON,
	}
	for name, want := range wantTypes {
		fd := fieldByName(tc, name)
		if fd == nil {
			t.Errorf("trigger schema missing gardener field %q", name)
			continue
		}
		if fd.Type != want {
			t.Errorf("trigger.%s type=%q, want %q", name, fd.Type, want)
		}
	}

	// Field-level: a gardener bead carrying a structured stop blob plus the
	// flat loop-spec fields (mirroring the gardener-loop runbook shape and
	// `gb gardener` output) must validate cleanly — this is the create that
	// used to 400 with "unknown field" before the schema was widened.
	fields := json.RawMessage(`{
		"action":               "spawn_agent",
		"role":                 "gardener",
		"project":              "gardenio",
		"prompt":               "drive the epic to done",
		"enabled":              true,
		"assign_bead":          "gd-rootEpic1",
		"terminal_goal":        "epic closed",
		"exit_condition":       "all children closed",
		"run_budget":           5,
		"max_runs":             5,
		"consecutive_failures": 0,
		"run_timeout":          "30m",
		"triggers":             [{"kind": "cron", "cron": "*/10 * * * *"}],
		"progress":             {"credit": []},
		"stop": {
			"loop_end": {"any": [
				{"kind": "max_runs"},
				{"kind": "bead_query", "op": "==", "value": "0", "parent": "gd-rootEpic1"}
			]},
			"hard_stop": {"any": [{"kind": "consecutive_failures", "threshold": 3}]}
		}
	}`)
	if err := model.ValidateTypeFields(fields, tc); err != nil {
		t.Fatalf("gardener-trigger payload should validate: %v", err)
	}

	// The stop-firing terminate path stamps gardener_terminated_slot/kind via
	// CloseBead (gardener_evaluator.go gardenerTerminate); those write-back keys
	// must also validate or the terminate 400s and the gardener never closes.
	closeFields := json.RawMessage(`{
		"action":                  "spawn_agent",
		"gardener_terminated_slot": "loop_end",
		"gardener_terminated_kind": "max_runs"
	}`)
	if err := model.ValidateTypeFields(closeFields, tc); err != nil {
		t.Fatalf("gardener terminate write-back should validate: %v", err)
	}
}

// TestTaskSchema_HasJiraAssigneeFields verifies the type:task schema declares
// the jira_assignee/jira_assignee_id fields the jira-bridge writes when a JIRA
// issue has an assignee (gardenio/controller/internal/bridge/jira_poller.go).
// Without these fields, the bridge fails to create or close beads with HTTP 400
// "jira_assignee_id: unknown field; jira_assignee: unknown field" (gd-H9sK7pQqKi).
func TestTaskSchema_HasJiraAssigneeFields(t *testing.T) {
	tc := parseBuiltinSchema(t, "task")
	for _, name := range []string{"jira_assignee", "jira_assignee_id"} {
		fd := fieldByName(tc, name)
		if fd == nil {
			t.Errorf("task schema missing field %q", name)
			continue
		}
		if fd.Type != model.FieldTypeString {
			t.Errorf("task.%s type=%q, want string", name, fd.Type)
		}
	}

	// Field-level: a payload mirroring jira_poller.createBeadFromIssue output
	// for an assigned issue must validate cleanly.
	fields := json.RawMessage(`{
		"jira_key":         "PE-8181",
		"jira_url":         "https://example.atlassian.net/browse/PE-8181",
		"jira_assignee":    "Jane Doe",
		"jira_assignee_id": "557058:abc123"
	}`)
	if err := model.ValidateTypeFields(fields, tc); err != nil {
		t.Fatalf("assigned-issue payload should validate: %v", err)
	}
}

// TestEpicSchema_HasPhaseOrderField verifies the type:epic schema declares the
// phase_order string[] field the plan-viewer writes to record the explicit,
// reorderable ordering (and membership set) of a plan epic's phase sub-epics
// (ka-0000005926, operator decision ka-0000001034). Without this field, the
// viewer's UpdateBeadFieldsTyped reorder write fails with HTTP 400
// "phase_order: unknown field" because epics do not allowAdditionalFields.
func TestEpicSchema_HasPhaseOrderField(t *testing.T) {
	tc := parseBuiltinSchema(t, "epic")
	fd := fieldByName(tc, "phase_order")
	if fd == nil {
		t.Fatal("epic schema missing field \"phase_order\"")
	}
	if fd.Type != model.FieldTypeStrings {
		t.Errorf("epic.phase_order type=%q, want string[]", fd.Type)
	}

	// Field-level: an ordered array of sub-epic ids must validate cleanly, and
	// an empty array (a plan with phases cleared) must too.
	for _, payload := range []string{
		`{"phase_order": ["gd-CrSUS78eix", "gd-VXGLJoS7Uj", "gd-D3GglByecY"]}`,
		`{"phase_order": []}`,
	} {
		if err := model.ValidateTypeFields(json.RawMessage(payload), tc); err != nil {
			t.Errorf("phase_order payload %s should validate: %v", payload, err)
		}
	}

	// A non-array value must be rejected — the field is a strict string[].
	if err := model.ValidateTypeFields(json.RawMessage(`{"phase_order": "gd-CrSUS78eix"}`), tc); err == nil {
		t.Error("phase_order as a bare string should fail validation, got nil")
	}
}

// TestEpicSchema_HasPlanStateField verifies the type:epic schema declares the
// plan_state enum the from-scratch authoring cockpit writes to move a plan
// through draft → ready → building and gate the gardener build (task
// ka-0000005350, D5 ka-0000003301, risk R6). Without it the viewer's
// UpdateBeadFieldsTyped state write would fail with HTTP 400 "plan_state:
// unknown field" because epics do not allowAdditionalFields.
func TestEpicSchema_HasPlanStateField(t *testing.T) {
	tc := parseBuiltinSchema(t, "epic")
	fd := fieldByName(tc, "plan_state")
	if fd == nil {
		t.Fatal("epic schema missing field \"plan_state\"")
	}
	if fd.Type != model.FieldTypeEnum {
		t.Errorf("epic.plan_state type=%q, want enum", fd.Type)
	}

	// Each of the five lifecycle values must validate — including BOTH terminal
	// states: `complete` (task ka-0000000616) and the distinct `abandoned` (task
	// ka-0000001377).
	for _, payload := range []string{
		`{"plan_state": "draft"}`,
		`{"plan_state": "ready"}`,
		`{"plan_state": "building"}`,
		`{"plan_state": "complete"}`,
		`{"plan_state": "abandoned"}`,
	} {
		if err := model.ValidateTypeFields(json.RawMessage(payload), tc); err != nil {
			t.Errorf("plan_state payload %s should validate: %v", payload, err)
		}
	}

	// A value outside the closed set must be rejected.
	if err := model.ValidateTypeFields(json.RawMessage(`{"plan_state": "done"}`), tc); err == nil {
		t.Error("plan_state \"done\" should fail validation, got nil")
	}
}

// TestEpicSchema_HasStepOrderField verifies the type:epic schema declares the
// step_order string[] field the plan-viewer writes to record the explicit,
// reorderable ordering of a plan/phase epic's issue-type step children
// (in-cockpit step reorder, ka-0000007445). It is the symmetric sibling of
// phase_order: without it, the viewer's UpdateBeadFieldsTyped reorder write
// fails with HTTP 400 "step_order: unknown field" because epics do not
// allowAdditionalFields.
func TestEpicSchema_HasStepOrderField(t *testing.T) {
	tc := parseBuiltinSchema(t, "epic")
	fd := fieldByName(tc, "step_order")
	if fd == nil {
		t.Fatal("epic schema missing field \"step_order\"")
	}
	if fd.Type != model.FieldTypeStrings {
		t.Errorf("epic.step_order type=%q, want string[]", fd.Type)
	}

	// An ordered array of step ids validates; an empty array (order cleared)
	// validates too.
	for _, payload := range []string{
		`{"step_order": ["gd-Step0001", "gd-Step0002", "gd-Step0003"]}`,
		`{"step_order": []}`,
	} {
		if err := model.ValidateTypeFields(json.RawMessage(payload), tc); err != nil {
			t.Errorf("step_order payload %s should validate: %v", payload, err)
		}
	}

	// A non-array value must be rejected — the field is a strict string[].
	if err := model.ValidateTypeFields(json.RawMessage(`{"step_order": "gd-Step0001"}`), tc); err == nil {
		t.Error("step_order as a bare string should fail validation, got nil")
	}
}

// TestEpicSchema_HasLastNudgeAtField verifies the type:epic schema declares the
// last_nudge_at string field the plan-viewer writes to make the reactive
// add-step debounce DURABLE (ka-0000002855). The 5s coalescing window used to
// live in a volatile in-memory viewer map, lost on restart and shared by no
// other writer; persisting the last-nudge timestamp on the plan epic makes the
// window survive restarts and coordinate concurrent CLI/browser/controller-reactor
// adds. Without this field the viewer's UpdateBeadFieldsTyped debounce write
// would fail with HTTP 400 "last_nudge_at: unknown field" because epics do not
// allowAdditionalFields.
func TestEpicSchema_HasLastNudgeAtField(t *testing.T) {
	tc := parseBuiltinSchema(t, "epic")
	fd := fieldByName(tc, "last_nudge_at")
	if fd == nil {
		t.Fatal("epic schema missing field \"last_nudge_at\"")
	}
	if fd.Type != model.FieldTypeString {
		t.Errorf("epic.last_nudge_at type=%q, want string", fd.Type)
	}

	// An RFC3339 timestamp is a plain string and must validate.
	if err := model.ValidateTypeFields(json.RawMessage(`{"last_nudge_at": "2026-07-23T12:00:00Z"}`), tc); err != nil {
		t.Errorf("last_nudge_at RFC3339 payload should validate: %v", err)
	}

	// A non-string value must be rejected — the field is a strict string.
	if err := model.ValidateTypeFields(json.RawMessage(`{"last_nudge_at": 123}`), tc); err == nil {
		t.Error("last_nudge_at as a bare number should fail validation, got nil")
	}
}

// TestEpicSchema_HasIconSvgUrlField verifies the type:epic schema declares the
// icon_svg_url string field the Phase 2 badge generator/cache writes back onto
// each epic bead (parent ka-0000002812, thread ka-0000004695): every gardenio
// epic gets one reproducible badge SVG, and its object-store URL is persisted on
// the epic so the viewer can render it. Without this field the generator's
// UpdateBeadFieldsTyped write-back would fail with HTTP 400 "icon_svg_url:
// unknown field" because epics do not allowAdditionalFields.
func TestEpicSchema_HasIconSvgUrlField(t *testing.T) {
	tc := parseBuiltinSchema(t, "epic")
	fd := fieldByName(tc, "icon_svg_url")
	if fd == nil {
		t.Fatal("epic schema missing field \"icon_svg_url\"")
	}
	if fd.Type != model.FieldTypeString {
		t.Errorf("epic.icon_svg_url type=%q, want string", fd.Type)
	}

	// A badge object-store URL is a plain string and must validate.
	if err := model.ValidateTypeFields(json.RawMessage(`{"icon_svg_url": "https://store.example/badges/ka-0000002812.svg"}`), tc); err != nil {
		t.Errorf("icon_svg_url URL payload should validate: %v", err)
	}

	// A non-string value must be rejected — the field is a strict string.
	if err := model.ValidateTypeFields(json.RawMessage(`{"icon_svg_url": 123}`), tc); err == nil {
		t.Error("icon_svg_url as a bare number should fail validation, got nil")
	}
}

// TestEpicSchema_HasPlanSpendFields verifies the type:epic schema declares the
// controller's per-plan crew spend rollup fields (gd-kcy4IFNzv5): epics do not
// allowAdditionalFields, so without them every rollup write 400s "unknown
// field". The figure and stamp are strings (the gardener_spend_usd shape); the
// crew counts are integers.
func TestEpicSchema_HasPlanSpendFields(t *testing.T) {
	tc := parseBuiltinSchema(t, "epic")
	for name, want := range map[string]model.FieldType{
		"plan_spend_usd":       model.FieldTypeString,
		"plan_spend_synced_at": model.FieldTypeString,
		"plan_spend_crew":      model.FieldTypeInteger,
		"plan_spend_live":      model.FieldTypeInteger,
	} {
		fd := fieldByName(tc, name)
		if fd == nil {
			t.Errorf("epic schema missing field %q", name)
			continue
		}
		if fd.Type != want {
			t.Errorf("epic.%s type=%q, want %q", name, fd.Type, want)
		}
	}

	// The controller's write payload must validate.
	payload := `{"plan_spend_usd": "12.3400", "plan_spend_synced_at": "2026-10-27T19:30:00Z", "plan_spend_crew": 4, "plan_spend_live": 1}`
	if err := model.ValidateTypeFields(json.RawMessage(payload), tc); err != nil {
		t.Errorf("plan spend rollup payload should validate: %v", err)
	}
	// A bare float for the figure is rejected, as for gardener_spend_usd.
	if err := model.ValidateTypeFields(json.RawMessage(`{"plan_spend_usd": 12.34}`), tc); err == nil {
		t.Error("plan_spend_usd as a bare number should fail validation, got nil")
	}
}

// TestResolveTypeConfig_UniversalIconFields: gb badge --upload writes
// icon_svg_url and icon_png_url onto a bead of any type (gd-NyYxIxxbuw), so
// every type config declares both as strings — builtin-only types, strict
// non-issue types and types whose config is a stored row written before the
// fields existed — and the write-back validates on a task and a decision.
func TestResolveTypeConfig_UniversalIconFields(t *testing.T) {
	srv, ms, ctx := testCtx(t)
	// A stored row that predates the fields: it must still accept them.
	ms.configs["type:bug"] = &model.Config{Key: "type:bug", Value: json.RawMessage(`{"kind":"issue","fields":[]}`)}

	for key := range builtinConfigs {
		typ, ok := strings.CutPrefix(key, "type:")
		if !ok {
			continue
		}
		tc, err := srv.resolveTypeConfig(ctx, model.BeadType(typ))
		if err != nil || tc == nil {
			t.Fatalf("resolveTypeConfig(%s) = %v, %v", typ, tc, err)
		}
		for _, name := range []string{"icon_svg_url", "icon_png_url"} {
			n := 0
			for _, f := range tc.Fields {
				if f.Name == name {
					n++
					if f.Type != model.FieldTypeString {
						t.Errorf("%s.%s type=%q, want string", typ, name, f.Type)
					}
				}
			}
			if n != 1 {
				t.Errorf("%s declares %s %d times, want once", typ, name, n)
			}
		}
	}

	icons := `"icon_svg_url": "https://viewer.example/image/coop/sessions/images/badges/gd-1/abcd1234-model.svg", "icon_png_url": "https://viewer.example/image/coop/sessions/images/badges/gd-1/abcd1234-model-128.png"`
	for typ, fields := range map[model.BeadType]string{
		"task":     `{` + icons + `}`,
		"bug":      `{` + icons + `}`,
		"decision": `{"prompt": "Which?", ` + icons + `}`,
	} {
		tc, _ := srv.resolveTypeConfig(ctx, typ)
		if err := model.ValidateTypeFields(json.RawMessage(fields), tc); err != nil {
			t.Errorf("%s: icon write-back should validate: %v", typ, err)
		}
	}
	tc, _ := srv.resolveTypeConfig(ctx, "task")
	if err := model.ValidateTypeFields(json.RawMessage(`{"icon_png_url": 123}`), tc); err == nil {
		t.Error("icon_png_url as a bare number should fail validation, got nil")
	}
}

// TestIssueSchemas_HasWorkspaceField verifies every issue-kind builtin type
// declares the `workspace` string field that `gb workspace setup` stores for
// worktree recovery-after-restart, and — critically — that the EXACT write that
// used to 400 now passes the real validation path (model.ValidateTypeFields), not
// just a field-set-membership check. storeWorkspaceInfo
// (gardenio/controller/cmd/gb/workspace.go) PATCHes a JSON string
// ({"branch","worktree_path","base_branch"}) into Fields["workspace"]; before
// this fix no schema declared the field, so validateFields rejected every setup
// with HTTP 400 "workspace: unknown field" and the recovery metadata was
// silently dropped (ka-0000006623). This exercises the validation path itself —
// the unit fake in the gardenio CLI test does not enforce the schema, which is
// why the defect shipped.
func TestIssueSchemas_HasWorkspaceField(t *testing.T) {
	// The exact payload storeWorkspaceInfo marshals: WorkspaceInfo JSON encoded
	// as a string value under the "workspace" field.
	wsJSON := `{"branch":"fix-thing-gd-ABC123","worktree_path":".beads/worktrees/gd-ABC123","base_branch":"origin/main"}`
	wsValue, err := json.Marshal(wsJSON) // string-encode, matching UpdateBeadFields map[string]string
	if err != nil {
		t.Fatalf("marshalling workspace value: %v", err)
	}
	setupPayload := json.RawMessage(`{"workspace": ` + string(wsValue) + `}`)
	// storeWorkspaceInfo(ctx, id, WorkspaceInfo{}) on teardown clears the field
	// by writing the empty string — that must validate too.
	clearPayload := json.RawMessage(`{"workspace": ""}`)

	for _, typeName := range []string{"task", "bug", "feature", "chore", "epic"} {
		tc := parseBuiltinSchema(t, typeName)
		fd := fieldByName(tc, "workspace")
		if fd == nil {
			t.Errorf("%s schema missing field \"workspace\" (gb workspace setup stamps it; absence 400s the recovery write and worktree recovery-after-restart is silently a no-op)", typeName)
			continue
		}
		if fd.Type != model.FieldTypeString {
			t.Errorf("%s.workspace type=%q, want string", typeName, fd.Type)
		}
		if err := model.ValidateTypeFields(setupPayload, tc); err != nil {
			t.Errorf("%s: gb workspace setup write should validate, got: %v", typeName, err)
		}
		if err := model.ValidateTypeFields(clearPayload, tc); err != nil {
			t.Errorf("%s: gb workspace teardown clear should validate, got: %v", typeName, err)
		}
	}
}

// TestTriggerSchema_HasScheduleMirrorFields verifies the type:trigger schema
// supports the dual-write mirror payload from the scheduler
// (gardenio/controller/internal/scheduler/dualwrite.go) — the Phase 3b/A5
// dual-write mirrors every open type=schedule into a type=trigger so the
// engine swap and /triggers UI can begin reading from triggers before the
// in-place schedule retirement lands. Without these fields, the mirror
// write fails with HTTP 400 "schedule_id: unknown field; last_agent_id:
// unknown field; source: must be one of [...]" (ka-0000003824).
func TestTriggerSchema_HasScheduleMirrorFields(t *testing.T) {
	tc := parseBuiltinSchema(t, "trigger")
	for _, name := range []string{"schedule_id", "last_agent_id"} {
		fd := fieldByName(tc, name)
		if fd == nil {
			t.Errorf("trigger schema missing field %q", name)
			continue
		}
		if fd.Type != model.FieldTypeString {
			t.Errorf("trigger.%s type=%q, want string", name, fd.Type)
		}
	}

	sourceFD := fieldByName(tc, "source")
	if sourceFD == nil {
		t.Fatal("trigger schema missing source enum")
	}
	if !slices.Contains(sourceFD.Values, "schedule-mirror") {
		t.Errorf("trigger.source enum missing %q; got %v", "schedule-mirror", sourceFD.Values)
	}

	// Field-level: a payload mirroring dualwrite.buildMirrorDesired output
	// must validate cleanly against the builtin trigger schema. Boolean
	// and integer fields are sent typed (not stringified) per the daemon
	// schema validator.
	fields := json.RawMessage(`{
		"action":         "spawn_agent",
		"source":         "schedule-mirror",
		"schedule_id":    "gd-rGNzZSQyxY",
		"last_agent_id":  "gd-agent-1",
		"role":           "gardener",
		"project":        "gardenio",
		"prompt":         "test",
		"enabled":        true,
		"max_concurrent": 1,
		"triggers":       [{"kind":"cron","cron":"*/5 * * * *"}],
		"gardener":        {"name":"gardener-foo","max_runs":10}
	}`)
	if err := model.ValidateTypeFields(fields, tc); err != nil {
		t.Fatalf("schedule-mirror payload should validate: %v", err)
	}
}

// TestJiraPollerSchema_HasSkipAssignedField verifies the type:jira-poller schema
// declares the skip_assigned field the jira-bridge writes when seeding its
// config bead from env vars (gardenio/controller/internal/bridge/jira_config.go).
// Without this field, the bridge fails to seed its config bead at startup with
// HTTP 400 "skip_assigned: unknown field" (gd-FJYU1zWNWJ).
func TestJiraPollerSchema_HasSkipAssignedField(t *testing.T) {
	tc := parseBuiltinSchema(t, "jira-poller")
	fd := fieldByName(tc, "skip_assigned")
	if fd == nil {
		t.Fatal("jira-poller schema missing field \"skip_assigned\"")
	}
	if fd.Type != model.FieldTypeString {
		t.Errorf("jira-poller.skip_assigned type=%q, want string", fd.Type)
	}

	// Field-level: a payload mirroring JiraConfigWatcher.seedConfigBead output
	// must validate cleanly against the builtin jira-poller schema.
	fields := json.RawMessage(`{
		"projects":        "PE,DEVOPS",
		"fix_versions":    "2.5.0",
		"required_labels": "approved-to-work",
		"skip_assigned":   "false",
		"poll_interval":   "30s"
	}`)
	if err := model.ValidateTypeFields(fields, tc); err != nil {
		t.Fatalf("jira-poller seed payload should validate: %v", err)
	}
}

func TestGRPCDeleteConfig(t *testing.T) {
	srv, ms, ctx := testCtx(t)
	ms.configs["view:inbox"] = &model.Config{Key: "view:inbox", Value: json.RawMessage(`{}`)}

	if _, err := srv.DeleteConfig(ctx, &beadsv1.DeleteConfigRequest{Key: "view:inbox"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := ms.configs["view:inbox"]; ok {
		t.Fatal("expected config to be deleted")
	}
}

// TestResolveTypeConfig_ScheduleBuiltin_HasTriggerFields locks in the
// scheduler-runbook contract from ka-0000003204: the type:schedule builtin
// MUST register triggers/stop/progress (and the watch_* / progress_state /
// trigger_state fields the scheduler reads from b.Fields). Without these
// the validator rejects `kd create -f triggers=...` with HTTP 400 "unknown
// field", and the runbook (gardenio/docs/gardener-loop-runbook.md) shape can
// never reach the scheduler.
func TestResolveTypeConfig_ScheduleBuiltin_HasTriggerFields(t *testing.T) {
	tc := parseBuiltinSchema(t, "schedule")

	// Trigger / progress / watch_* fields the runtime scheduler reads from
	// b.Fields["..."] in gardenio/controller/internal/scheduler/scheduler.go.
	required := map[string]model.FieldType{
		"triggers":       model.FieldTypeJSON,
		"stop":           model.FieldTypeJSON,
		"progress":       model.FieldTypeJSON,
		"progress_state": model.FieldTypeJSON,
		"trigger_state":  model.FieldTypeJSON,
		"trigger_mode":   model.FieldTypeString,
		"watch_bead_id":  model.FieldTypeString,
		"watch_channel":  model.FieldTypeString,
		"watch_from":     model.FieldTypeString,
		"watch_labels":   model.FieldTypeString,
		"watch_project":  model.FieldTypeString,
		"watch_role":     model.FieldTypeString,
		"watch_types":    model.FieldTypeString,
		"agent_state":    model.FieldTypeString,
		"close_reason":   model.FieldTypeString,
		"operated_by":    model.FieldTypeString,
		"slack_user_id":  model.FieldTypeString,
	}
	for name, want := range required {
		fd := fieldByName(tc, name)
		if fd == nil {
			t.Errorf("type:schedule missing field %q", name)
			continue
		}
		if fd.Type != want {
			t.Errorf("type:schedule field %q has type %q, want %q", name, fd.Type, want)
		}
	}

	// cron must be optional now (pure event-driven schedules have only
	// triggers). validateScheduleFields enforces "cron OR triggers".
	cron := fieldByName(tc, "cron")
	if cron == nil {
		t.Fatal("type:schedule missing cron field")
	}
	if cron.Required {
		t.Error("type:schedule cron must be optional (cron-or-triggers enforced by validateScheduleFields)")
	}
}

// TestGRPCCreateBead_ScheduleType_AcceptsTriggerFields exercises the full
// create path: with the validator patched, a schedule create that supplies
// triggers/stop/progress JSON instead of cron must succeed.
func TestGRPCCreateBead_ScheduleType_AcceptsTriggerFields(t *testing.T) {
	srv, _, ctx := testCtx(t)

	resp, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
		Title: "Cycle watchdog",
		Type:  "schedule",
		Fields: []byte(`{
			"project":"gardenio",
			"prompt":"Check the cycle status and squawk anomalies.",
			"triggers":[{"type":"bead","watch_types":["agent"]}],
			"stop":{"on":"no_progress_for","duration":"30m"},
			"progress":{"score_field":"progress_score"},
			"trigger_mode":"any",
			"watch_project":"gardenio",
			"watch_role":"crew"
		}`),
	})
	if err != nil {
		t.Fatalf("schedule create with triggers/stop/progress should succeed: %v", err)
	}
	if resp.Bead.Type != "schedule" {
		t.Fatalf("got type=%q, want schedule", resp.Bead.Type)
	}
}

// TestGRPCCreateBead_ScheduleType_RejectsNeitherCronNorTriggers asserts that
// dropping `cron: required:true` does not let bare schedules through —
// validateScheduleFields requires at least one of cron or triggers.
func TestGRPCCreateBead_ScheduleType_RejectsNeitherCronNorTriggers(t *testing.T) {
	srv, _, ctx := testCtx(t)

	_, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
		Title: "Bare schedule",
		Type:  "schedule",
		Fields: []byte(`{
			"project":"gardenio",
			"prompt":"do nothing"
		}`),
	})
	if err == nil {
		t.Fatal("expected error for schedule with neither cron nor triggers")
	}
}

// TestResolveTypeConfig_EscalationExecutionTargetEnum pins the
// execution_target enum on type:escalation so it cannot silently drift
// away from what the gb CLI advertises and writes. The canonical set is
// `local | service_account | server` (gd-9gIQiClbz4) — any change here
// must be coordinated with gardenio/controller/cmd/gb/escalation.go,
// the e2e runner (gardenio/tests/e2e/scripts/test-local-mode.sh), and
// the agent-side runbook.
func TestResolveTypeConfig_EscalationExecutionTargetEnum(t *testing.T) {
	srv, _, ctx := testCtx(t)

	tc, err := srv.resolveTypeConfig(ctx, "escalation")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tc == nil {
		t.Fatal("expected escalation type config from builtins, got nil")
	}

	var execTarget *model.FieldDef
	for i := range tc.Fields {
		if tc.Fields[i].Name == "execution_target" {
			execTarget = &tc.Fields[i]
			break
		}
	}
	if execTarget == nil {
		t.Fatal("escalation type missing execution_target field")
	}
	if execTarget.Type != "enum" {
		t.Errorf("execution_target type=%q, want enum", execTarget.Type)
	}

	want := []string{"local", "service_account", "server"}
	if len(execTarget.Values) != len(want) {
		t.Fatalf("execution_target values=%v, want %v", execTarget.Values, want)
	}
	got := map[string]bool{}
	for _, v := range execTarget.Values {
		got[v] = true
	}
	for _, v := range want {
		if !got[v] {
			t.Errorf("execution_target missing value %q (got %v)", v, execTarget.Values)
		}
	}
}

// TestResolveTypeConfig_UnionsEnumValuesWithBuiltin pins the fix for
// ka-0000003824: when a stale store config for an authoritative builtin
// type pins an enum field to an older value set, resolveTypeConfig must
// union in the builtin's newer values so new enum members (e.g. a new
// `source` for type:trigger) work without manual SetConfig surgery.
func TestResolveTypeConfig_UnionsEnumValuesWithBuiltin(t *testing.T) {
	srv, ms, ctx := testCtx(t)

	// Persist a stale type:trigger config whose `source` enum lacks the
	// schedule-mirror value that newer builtins ship with.
	ms.configs["type:trigger"] = &model.Config{
		Key: "type:trigger",
		Value: json.RawMessage(`{
			"kind": "config",
			"fields": [
				{"name": "source", "type": "enum", "values": ["gardener", "webhook"]}
			]
		}`),
	}

	tc, err := srv.resolveTypeConfig(ctx, "trigger")
	if err != nil {
		t.Fatalf("resolveTypeConfig: %v", err)
	}
	if tc == nil {
		t.Fatal("expected trigger type config, got nil")
	}

	var src *model.FieldDef
	for i := range tc.Fields {
		if tc.Fields[i].Name == "source" {
			src = &tc.Fields[i]
			break
		}
	}
	if src == nil {
		t.Fatal("source field missing from merged trigger config")
	}
	if src.Type != model.FieldTypeEnum {
		t.Errorf("source type=%q, want enum", src.Type)
	}

	// Pre-existing store values must be preserved.
	for _, v := range []string{"gardener", "webhook"} {
		if !slices.Contains(src.Values, v) {
			t.Errorf("source missing store value %q (got %v)", v, src.Values)
		}
	}

	// And every value from the builtin must also appear post-merge.
	// We compare against the builtin trigger config so this test does
	// not have to enumerate every accepted source name by hand.
	builtin := builtinConfigs["type:trigger"]
	if builtin == nil {
		t.Fatal("builtin type:trigger missing — config.go drift?")
	}
	var btc model.TypeConfig
	if err := json.Unmarshal(builtin.Value, &btc); err != nil {
		t.Fatalf("unmarshal builtin trigger: %v", err)
	}
	var builtinSrcValues []string
	for _, f := range btc.Fields {
		if f.Name == "source" {
			builtinSrcValues = f.Values
			break
		}
	}
	if len(builtinSrcValues) == 0 {
		t.Fatal("builtin trigger config has no source enum to union against")
	}
	for _, v := range builtinSrcValues {
		if !slices.Contains(src.Values, v) {
			t.Errorf("source missing builtin value %q after merge (got %v)", v, src.Values)
		}
	}
}

// TestResolveTypeConfig_BeadTriggerCLIFieldContract is the missing CLI->schema
// contract check for gd-qXZlVdcWko. `gb trigger create bead` (and the
// `gb trigger create assert --on-violation` sibling) stamp fields the builtin
// type:trigger schema must accept, or the daemon 400s every create with
// "validation failed: <field>: unknown field" (the bridge's parallel TypeConfig
// is NOT consulted at create time — resolveTypeConfig merges the builtin + store
// config, and the builtin is authoritative).
//
// This test marshals the EXACT field maps gb writes (see
// gardenio/controller/cmd/gb/trigger_bead.go buildBeadFieldsWithExprs /
// buildAssertViolationFields + trigger.go applyCommonFields) and asserts they
// validate clean against the resolved type:trigger config — the same path the
// daemon runs at beads.go ValidateTypeFields. The pre-existing
// TestResolveTypeConfig_UnionsEnumValuesWithBuiltin only checks the `source`
// enum union and never asserts CLI field coverage, which is why event_types /
// assert / on_violation / action=assert shipped missing from the builtin.
func TestResolveTypeConfig_BeadTriggerCLIFieldContract(t *testing.T) {
	srv, _, ctx := testCtx(t)

	tc, err := srv.resolveTypeConfig(ctx, "trigger")
	if err != nil {
		t.Fatalf("resolveTypeConfig(trigger): %v", err)
	}
	if tc == nil {
		t.Fatal("expected type:trigger config from builtins, got nil")
	}

	cases := []struct {
		name   string
		fields map[string]any
	}{
		{
			// `gb trigger create bead --event-types created,updated
			//    --condition status:==:open --action notify --prompt ...`
			name: "bead notify trigger",
			fields: map[string]any{
				"event_types":      "created,updated",
				"conditions":       `[{"field":"status","op":"==","value":"open"}]`,
				"action":           "notify",
				"enabled":          true,
				"cooldown_minutes": float64(5),
				"max_concurrent":   float64(3),
				"project":          "gardenio",
				"prompt":           "Bead {{bead_id}} changed",
			},
		},
		{
			// `gb trigger create assert --event-types updated
			//    --condition status:==:in_progress --expr '<invariant>'
			//    --on-violation notify --prompt ...`
			name: "assert-violation trigger",
			fields: map[string]any{
				"event_types":      "updated",
				"conditions":       `[{"field":"status","op":"==","value":"in_progress"}]`,
				"assert":           `[{"expr":"has(bead.last_checked)"}]`,
				"on_violation":     "notify",
				"action":           "assert",
				"enabled":          true,
				"cooldown_minutes": float64(5),
				"max_concurrent":   float64(3),
				"project":          "gardenio",
				"prompt":           "Task {{bead_id}} stalled: {{violated}}",
			},
		},
		{
			// `gb trigger create bead --action create_bead --bead-type bug
			//    --bead-priority 1 --parent gd-EPIC --bead-label needs-intake ...`
			// AND the assert create_bead leg (--on-violation create_bead|both).
			// These create_bead-shaping fields are read by ParseTriggerDef /
			// createTaskBead but were missing from the builtin schema, so every
			// create with them 400'd (ka-0000002642, C1 collapse-the-spandrel).
			name: "create_bead with bead_type/priority/parent/extra_labels",
			fields: map[string]any{
				"event_types":      "closed",
				"conditions":       `[{"field":"status","op":"==","value":"closed"}]`,
				"action":           "create_bead",
				"bead_type":        "bug",
				"bead_priority":    float64(1),
				"parent":           "gd-gQVBvySwLS",
				"extra_labels":     "needs-intake,liveness",
				"enabled":          true,
				"cooldown_minutes": float64(0),
				"max_concurrent":   float64(3),
				"project":          "gardenio",
				"prompt":           "Filed for {{bead_id}}",
			},
		},
		{
			// assert trigger whose on_violation files a bug — the exact C1
			// fleet-liveness shape (ka-0000002642 / trigger ka-0000002431):
			// notify + create_bead on a violated gardener-liveness invariant.
			name: "assert-violation files a typed bug",
			fields: map[string]any{
				"event_types":      "updated,closed,reconciled",
				"conditions":       `[{"field":"id","op":"in","value":"ka-0000007481,ka-0000003783"}]`,
				"assert":           `[{"expr":"bead.status == \"open\" && (!has(bead.enabled) || bead.enabled == \"true\")"}]`,
				"on_violation":     "both",
				"action":           "assert",
				"bead_type":        "bug",
				"bead_priority":    float64(1),
				"parent":           "gd-gQVBvySwLS",
				"extra_labels":     "needs-intake,liveness",
				"enabled":          true,
				"cooldown_minutes": float64(0),
				"max_concurrent":   float64(3),
				"project":          "gardenio",
				"prompt":           "Gardener {{bead_id}} dark: {{violated}} ({{values}})",
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw, err := json.Marshal(c.fields)
			if err != nil {
				t.Fatalf("marshal %s fields: %v", c.name, err)
			}
			if err := model.ValidateTypeFields(raw, tc); err != nil {
				t.Fatalf("%s: gb-written fields rejected by builtin type:trigger schema: %v\nfields=%s",
					c.name, err, raw)
			}
		})
	}

	// Negative control: the schema is closed (no allow_additional_fields), so a
	// genuinely unknown field must still be rejected. This proves the fix added
	// the specific fields rather than opening the schema wholesale.
	bogus, err := json.Marshal(map[string]any{
		"action":                              "notify",
		"definitely_not_a_real_trigger_field": "x",
	})
	if err != nil {
		t.Fatalf("marshal bogus fields: %v", err)
	}
	if err := model.ValidateTypeFields(bogus, tc); err == nil {
		t.Fatal("expected unknown-field rejection for closed type:trigger schema, got nil")
	}
}

func TestUnionEnumValues(t *testing.T) {
	cases := []struct {
		name string
		a, b []string
		want []string
	}{
		{"empty b returns a", []string{"x", "y"}, nil, []string{"x", "y"}},
		{"empty a returns b appended", nil, []string{"x"}, []string{"x"}},
		{
			"preserves a order then appends new from b",
			[]string{"a", "b"},
			[]string{"c", "a", "d"},
			[]string{"a", "b", "c", "d"},
		},
		{"dedups within b", []string{"a"}, []string{"b", "b", "c"}, []string{"a", "b", "c"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := unionEnumValues(tc.a, tc.b)
			if !slices.Equal(got, tc.want) {
				t.Errorf("unionEnumValues(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// TestResolveTypeConfig_MultiSourceTriggerFields guards the field registration
// that lets a trigger:multi fire create its task/agent beads. multiDecisionToMatch
// (gardenio controller bridge) stamps multi_fire_key/_time/_idx/_entry_kind/
// _entry_label/_cause onto the spawned task bead, and the spawn path adds
// multi_key onto the agent bead. type:task / type:agent set
// allow_additional_fields=false, so a missing registration 400s EVERY
// multi-source fire at bead creation — which silently broke the entire
// event-driven gardener path fleet-wide (bug gd-ewTtwUgCVV). Keep these in
// lockstep with bridge/trigger_source_multi.go multiDecisionToMatch + the
// triggerSourceFieldKey("multi") agent field.
func TestResolveTypeConfig_MultiSourceTriggerFields(t *testing.T) {
	srv, _, ctx := testCtx(t)

	taskCfg, err := srv.resolveTypeConfig(ctx, "task")
	if err != nil {
		t.Fatalf("resolve type:task: %v", err)
	}
	if taskCfg == nil {
		t.Fatal("expected type:task config from builtins, got nil")
	}
	taskFields := map[string]bool{}
	for _, f := range taskCfg.Fields {
		taskFields[f.Name] = true
	}
	for _, name := range []string{
		"multi_fire_key", "multi_fire_time", "multi_entry_idx",
		"multi_entry_kind", "multi_entry_label", "multi_cause",
	} {
		if !taskFields[name] {
			t.Errorf("type:task missing multi-source field %q — trigger:multi create_bead/apply_formula/spawn_agent fires will 400", name)
		}
	}

	agentCfg, err := srv.resolveTypeConfig(ctx, "agent")
	if err != nil {
		t.Fatalf("resolve type:agent: %v", err)
	}
	if agentCfg == nil {
		t.Fatal("expected type:agent config from builtins, got nil")
	}
	agentFields := map[string]bool{}
	for _, f := range agentCfg.Fields {
		agentFields[f.Name] = true
	}
	if !agentFields["multi_key"] {
		t.Error("type:agent missing multi_key — trigger:multi spawn_agent fires will 400 on the agent bead")
	}
}

// TestResolveTypeConfig_BeadSourceCreateBeadFields guards the field
// registration that lets a BEAD-source trigger's create_bead / assert action
// file its issue bead. beadEventToMatch (gardenio controller bridge) stamps
// bead_event_key / source_bead_id / source_event onto TriggerMatch.Fields;
// beadAssertViolationToMatch adds assert_violated / assert_values; and
// createTaskBead always stamps trigger_id / trigger_name / trigger_created_by.
// createTaskBead then copies all of these onto the bead it files — which may be
// any create_bead-allowlisted type (task, bug, feature, chore), and an assert
// trigger's create_bead commonly files a bug. Those types set
// allow_additional_fields=false, so a missing registration 400'd EVERY
// bead-source create_bead / assert fire at bead creation ("bead_event_key:
// unknown field"), leaving the trigger inert — the create_bead task/bug sibling
// (ka-0000005502) of the apply_formula agent-spawn bead_key 400 (ka-0000001885).
// bead_event_key in particular MUST be a persisted typed field, not filtered,
// because CatchUpKeys reads it back off the filed beads after an engine restart
// to re-seed the dedup set. Keep these in lockstep with bridge/triggers.go
// createTaskBead + trigger_source_bead.go beadEventToMatch /
// beadAssertViolationToMatch.
func TestResolveTypeConfig_BeadSourceCreateBeadFields(t *testing.T) {
	srv, _, ctx := testCtx(t)

	// The exact stamp createTaskBead emits for a bead-source assert create_bead:
	// provenance + bead-event match fields + assert violation fields.
	stamp := json.RawMessage(`{
		"trigger_id":"gd-t1","trigger_name":"Dark gardener alert","trigger_created_by":"agent-x",
		"bead_event_key":"gd-abc:updated:violated:deadbeef",
		"source_bead_id":"gd-abc","source_event":"updated",
		"assert_violated":"bead.status == \"open\"","assert_values":"status=closed"
	}`)

	// Every create_bead-allowlisted issue type must both declare the fields and
	// accept the exact create write that used to 400.
	for _, typeName := range []string{"task", "bug", "feature", "chore"} {
		tc, err := srv.resolveTypeConfig(ctx, model.BeadType(typeName))
		if err != nil {
			t.Fatalf("resolveTypeConfig(%s): %v", typeName, err)
		}
		if tc == nil {
			t.Fatalf("type:%s config missing from builtins", typeName)
		}
		declared := map[string]bool{}
		for _, f := range tc.Fields {
			declared[f.Name] = true
		}
		for _, name := range []string{
			"trigger_id", "trigger_name", "trigger_created_by",
			"bead_event_key", "source_bead_id", "source_event",
			"assert_violated", "assert_values",
		} {
			if !declared[name] {
				t.Errorf("type:%s missing bead-source create_bead field %q — "+
					"bead-source create_bead/assert fires will 400", typeName, name)
			}
		}
		if err := model.ValidateTypeFields(stamp, tc); err != nil {
			t.Errorf("type:%s rejected bead-source create_bead stamp: %v", typeName, err)
		}
	}

	// Negative control: the schema stays closed (allow_additional_fields=false),
	// so a genuinely unknown field is still rejected — proves the fix added the
	// specific fields rather than opening the schema wholesale.
	taskCfg, err := srv.resolveTypeConfig(ctx, "task")
	if err != nil {
		t.Fatalf("resolveTypeConfig(task): %v", err)
	}
	if err := model.ValidateTypeFields(
		json.RawMessage(`{"definitely_not_a_real_field":"x"}`), taskCfg,
	); err == nil {
		t.Fatal("expected unknown-field rejection for closed type:task schema, got nil")
	}
}

// TestResolveTypeConfig_ObservabilityBridgeTypes verifies the observability
// bridge types (dashboard, alert_rule, alert_mute — epic ka-0000006776) are
// authoritative builtins. Regression for ka-0000001549: grafana-bridge
// registers these via a config *bead*, but the type resolver only consults
// builtins and the configs KV table — never config beads — so every
// type=dashboard create 400'd "unknown bead type dashboard".
func TestResolveTypeConfig_ObservabilityBridgeTypes(t *testing.T) {
	srv, _, ctx := testCtx(t)

	cases := []struct {
		typeName string
		field    string
		fieldTyp model.FieldType
	}{
		{"dashboard", "dashboard_uid", model.FieldTypeString},
		{"dashboard", "dashboard_source", model.FieldTypeEnum},
		{"dashboard", "dashboard_conflict_critical", model.FieldTypeBoolean},
		{"alert_rule", "rule_kind", model.FieldTypeEnum},
		{"alert_rule", "rule_expr", model.FieldTypeString},
		{"alert_mute", "mute_until", model.FieldTypeTimestamp},
	}
	for _, c := range cases {
		tc, err := srv.resolveTypeConfig(ctx, model.BeadType(c.typeName))
		if err != nil {
			t.Fatalf("resolveTypeConfig(%q): %v", c.typeName, err)
		}
		if tc == nil {
			t.Fatalf("type:%s config missing from builtins (unknown bead type regression)", c.typeName)
		}
		var fd *model.FieldDef
		for i := range tc.Fields {
			if tc.Fields[i].Name == c.field {
				fd = &tc.Fields[i]
				break
			}
		}
		if fd == nil {
			t.Fatalf("type:%s missing builtin field %q", c.typeName, c.field)
		}
		if fd.Type != c.fieldTyp {
			t.Errorf("type:%s field %q type=%q, want %q", c.typeName, c.field, fd.Type, c.fieldTyp)
		}
	}
}

// TestDashboardCreateNo400 is the end-to-end regression for ka-0000001549:
// creating a type=dashboard bead (as grafana-bridge backfill does) must not
// 400 "unknown bead type dashboard".
func TestDashboardCreateNo400(t *testing.T) {
	_, _, h := newTestServer()
	rec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title": "gardenio-cost-overview",
		"type":  "dashboard",
		"force": true,
		"fields": map[string]any{
			"dashboard_uid":    "gardenio-cost-overview",
			"dashboard_source": "beads",
		},
	})
	if rec.Code == http.StatusBadRequest {
		t.Fatalf("got 400 (unknown bead type dashboard); regression: %s", rec.Body.String())
	}
	requireStatus(t, rec, 201)
}

// TestResolveTypeConfig_TaskThreadHeadChannelID guards the Commons channel_id
// stamp on thread (wish) heads (gd-0moiIbZPom): the slack-bridge writes
// channel_id on the head create beside slack_thread_channel, and type:task
// rejects unknown fields, so an undeclared channel_id would 400 every head
// minted in a seeded land.
func TestResolveTypeConfig_TaskThreadHeadChannelID(t *testing.T) {
	srv, _, ctx := testCtx(t)

	tc, err := srv.resolveTypeConfig(ctx, "task")
	if err != nil {
		t.Fatalf("resolveTypeConfig(task): %v", err)
	}
	if tc == nil {
		t.Fatal("type:task config missing from builtins")
	}
	fields := json.RawMessage(`{
		"slack_thread_channel": "C-THREAD",
		"slack_thread_ts": "1700000000.000100",
		"spawn_source": "slack-thread",
		"slack_root_text": "hello",
		"channel_id": "gd-CH1"
	}`)
	if err := model.ValidateTypeFields(fields, tc); err != nil {
		t.Errorf("thread head with channel_id failed validation: %v", err)
	}
}
