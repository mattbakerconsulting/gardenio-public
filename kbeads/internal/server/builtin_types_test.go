package server

import (
	"encoding/json"
	"slices"
	"testing"

	"gardenio/kbeads/internal/model"
)

// TestBuiltinThreadFieldsDeclared guards the class of bug where the slack
// bridge stamps a field that no type schema declares, so the create path
// 400s "<field>: unknown field" and every thread-spawned agent silently
// fails to come up (Slack appears dead). thread_id is the wish-pointer the
// bridge stamps on thread-derivative task and agent beads alongside its
// sibling slack_thread_bead_id; both must be declared on both types.
func TestBuiltinThreadFieldsDeclared(t *testing.T) {
	for _, typeName := range []string{"type:task", "type:agent"} {
		c, ok := builtinConfigs[typeName]
		if !ok {
			t.Fatalf("builtinConfigs missing %q", typeName)
		}
		var tc model.TypeConfig
		if err := json.Unmarshal(c.Value, &tc); err != nil {
			t.Fatalf("%s: unmarshal config value: %v", typeName, err)
		}
		have := make(map[string]struct{}, len(tc.Fields))
		for _, f := range tc.Fields {
			have[f.Name] = struct{}{}
		}
		for _, want := range []string{"thread_id", "slack_thread_bead_id"} {
			if _, ok := have[want]; !ok {
				t.Errorf("%s schema missing field %q (bridge stamps it; absence 400s every thread-spawn)", typeName, want)
			}
		}
		// A wish head a Commons mention minted from a chat root names it
		// (slack-bridge inbound_compose.go); without the field every promotion 400s.
		if _, ok := have["promoted_from"]; typeName == "type:task" && !ok {
			t.Errorf("%s schema missing field %q (a Commons mention's promotion writes it)", typeName, "promoted_from")
		}
	}
}

// TestBuiltinAgentLastProgressAtDeclared guards the unknown-field 400 class for
// the last_progress_at heartbeat the gardenio harness (monitorExit) stamps on the
// agent bead when a genuine turn lands. The server-side single-claim guard and
// gb news consume it to make "live" mean PROGRESSING, not pod-running, so a
// livelocked crew no longer squats its bead indefinitely (ka-0000000286).
//
// It asserts BOTH:
//  1. the builtin type:agent schema declares the field (a strict schema 400s any
//     undeclared field, and the stamp write would silently fail), AND
//  2. resolveTypeConfig MERGES the field into a stored config that shadows the
//     builtin but omits it. Prod carries a stored type:agent row (created
//     2026-02-25), and the READ path returns it verbatim, so ONLY the
//     validation-path builtin-merge makes the new field acceptable without a
//     manual SetConfig roll-forward. This is the exact shadow trap advice
//     ka-0000000061 warned about — closed by the #382 builtin-merge, which this
//     test pins for type:agent.
func TestBuiltinAgentLastProgressAtDeclared(t *testing.T) {
	// (1) Builtin declares it, typed string.
	c, ok := builtinConfigs["type:agent"]
	if !ok {
		t.Fatal("builtinConfigs missing \"type:agent\"")
	}
	var tc model.TypeConfig
	if err := json.Unmarshal(c.Value, &tc); err != nil {
		t.Fatalf("type:agent: unmarshal config value: %v", err)
	}
	var found bool
	for _, f := range tc.Fields {
		if f.Name == "last_progress_at" {
			found = true
			if f.Type != model.FieldTypeString {
				t.Errorf("type:agent last_progress_at type=%q, want string", f.Type)
			}
			if f.Required {
				t.Error("type:agent last_progress_at must be optional (older agent beads lack it)")
			}
		}
	}
	if !found {
		t.Fatal("type:agent builtin schema missing field last_progress_at (harness stamps it; a strict schema 400s the write)")
	}

	// (2) A stored config that OMITS last_progress_at (the prod shadow shape)
	// must still validate a write carrying it, because resolveTypeConfig merges
	// the missing builtin field in on the validation path.
	srv, store, ctx := testCtx(t)
	shadow := `{"kind":"config","fields":[
		{"name":"agent","type":"string","required":true},
		{"name":"role","type":"string","required":true},
		{"name":"project","type":"string","required":true},
		{"name":"agent_state","type":"string"}
	]}`
	store.configs["type:agent"] = &model.Config{Key: "type:agent", Value: json.RawMessage(shadow)}

	resolved, err := srv.resolveTypeConfig(ctx, "agent")
	if err != nil {
		t.Fatalf("resolveTypeConfig(agent): %v", err)
	}
	if err := model.ValidateTypeFields(
		json.RawMessage(`{"agent":"a","role":"crew","project":"p","last_progress_at":"2026-08-07T18:00:00Z"}`),
		resolved,
	); err != nil {
		t.Errorf("ValidateTypeFields rejected last_progress_at against a shadowing stored config — the builtin-merge did not un-shadow the field: %v", err)
	}
}

// TestBuiltinGardenerChainFieldDeclared guards the same unknown-field 400 class
// as TestBuiltinThreadFieldsDeclared, for the `chain` field the gardener
// card-graph authoring path stamps on a predecessor gardener (task
// ka-0000005273 / "C3"). The beads-viewer handler handleGardenerSuccessor
// (gardenio/controller/cmd/beads-viewer/gardener_successor.go) serializes a drawn
// successor to `chain.to` and persists it via UpdateBeadFieldsTyped({"chain":
// ...}) on the gardener's type:trigger bead. If the trigger schema does not
// declare `chain`, that write 400s "chain: unknown field" — so the whole C3
// authoring surface (draw successor / fan-out / join / branch) fails against
// EVERY real daemon (prod and the stock hermetic daemon the beads-viewer-e2e CI
// task boots), even though the C3 unit tests pass (their fakeDaemon does no
// field validation). Sits next to the existing `gardener` json field, which the
// gardener spec is stamped through the same way. Exercised end-to-end by
// gardenio/tests/beads-viewer-e2e/tests/gardener_successor.spec.js.
func TestBuiltinGardenerChainFieldDeclared(t *testing.T) {
	c, ok := builtinConfigs["type:trigger"]
	if !ok {
		t.Fatalf("builtinConfigs missing %q", "type:trigger")
	}
	var tc model.TypeConfig
	if err := json.Unmarshal(c.Value, &tc); err != nil {
		t.Fatalf("type:trigger: unmarshal config value: %v", err)
	}
	var chain *model.FieldDef
	for i := range tc.Fields {
		if tc.Fields[i].Name == "chain" {
			chain = &tc.Fields[i]
			break
		}
	}
	if chain == nil {
		t.Fatalf("type:trigger schema missing field %q (gardener card-graph authoring stamps it via UpdateBeadFieldsTyped; absence 400s every draw-successor/fan-out/join write against a real daemon)", "chain")
	}
	if chain.Type != model.FieldTypeJSON {
		t.Errorf("type:trigger `chain` field type = %q, want %q (the C3 codec marshals a bridge.ChainSpec object/array)", chain.Type, model.FieldTypeJSON)
	}
}

// TestBuiltinTriggerSourceMatchFieldsDeclared guards the same unknown-field 400
// class as TestBuiltinGardenerChainFieldDeclared, for the account-cost and
// slack-reaction source-match fields the trigger SOURCES read flat off the
// trigger bead at load time.
//
// The account source (gardenio/controller/internal/bridge/
// trigger_source_account.go) reads bead.Fields["condition_type"|"field"|"op"|
// "threshold"|"account_filter"]; the slack-reaction source
// (trigger_source_slack_reaction.go) reads bead.Fields["emoji"|"match"|
// "channel_filter"]. Both sources ARE wired into the deployed trigger-engine
// (cmd/trigger-engine/main.go NewAccountTriggerSource), so a trigger authored
// with these fields is live-but-unauthorable if the schema omits them: the
// create/update path 400s "condition_type: unknown field" etc. and nobody can
// author an account-cost or slack-reaction trigger through the normal write
// path (bug ka-0000001277). This asserts the builtin declares each; because
// resolveTypeConfig merges builtin fields missing from a stored config on the
// validation path, declaring them here also un-shadows the prod store row.
func TestBuiltinTriggerSourceMatchFieldsDeclared(t *testing.T) {
	c, ok := builtinConfigs["type:trigger"]
	if !ok {
		t.Fatalf("builtinConfigs missing %q", "type:trigger")
	}
	var tc model.TypeConfig
	if err := json.Unmarshal(c.Value, &tc); err != nil {
		t.Fatalf("type:trigger: unmarshal config value: %v", err)
	}
	byName := make(map[string]model.FieldDef, len(tc.Fields))
	for _, f := range tc.Fields {
		byName[f.Name] = f
	}
	// threshold is strconv.ParseFloat'd by the account source, so it must be a
	// float (matches type:account's total_cost_usd); the rest are read as raw
	// strings.
	want := map[string]model.FieldType{
		"condition_type": model.FieldTypeString,
		"field":          model.FieldTypeString,
		"op":             model.FieldTypeString,
		"threshold":      model.FieldTypeFloat,
		"account_filter": model.FieldTypeString,
		"emoji":          model.FieldTypeString,
		"match":          model.FieldTypeString,
		"channel_filter": model.FieldTypeString,
	}
	for name, wantType := range want {
		f, ok := byName[name]
		if !ok {
			t.Errorf("type:trigger schema missing field %q (a trigger source reads it flat off the bead; absence 400s every account-cost/slack-reaction trigger create through the normal write path)", name)
			continue
		}
		if f.Type != wantType {
			t.Errorf("type:trigger `%s` field type = %q, want %q", name, f.Type, wantType)
		}
	}
}

// TestBuiltinDecisionOwnerFieldsDeclared guards the same unknown-field 400
// class as TestBuiltinThreadFieldsDeclared, for the durable lane-ownership
// fields `gb decision create` stamps off the spawning agent bead
// (owner_seat / owner_gardener, feat ka-0000005031, decision.go
// resolveDecisionOwner).
//
// gb decision create marshals owner_seat/owner_gardener into the SAME fields map
// it hands to CreateBead. If the type:decision schema does not declare them, the
// create 400s "owner_seat: unknown field" and `gb decision create` exits 1 —
// which is exactly what broke the release E2E on tags 2026.238.1/2026.238.2
// (bug ka-0000003466): decisions-yield scenario 1 and gate-system scenarios
// 2/3/7 all call `gb decision create` first, so a rejected create failed every
// one of them and froze the golden release at 2026.237.8. The stamp is
// best-effort in gb (a lookup miss leaves the fields empty), but when the
// spawning agent bead DOES carry a seat_key — the normal case in the E2E and in
// prod — the create sends the fields and the schema must accept them.
func TestBuiltinDecisionOwnerFieldsDeclared(t *testing.T) {
	c, ok := builtinConfigs["type:decision"]
	if !ok {
		t.Fatal("builtinConfigs missing \"type:decision\"")
	}
	var tc model.TypeConfig
	if err := json.Unmarshal(c.Value, &tc); err != nil {
		t.Fatalf("type:decision: unmarshal config value: %v", err)
	}
	have := make(map[string]struct{}, len(tc.Fields))
	for _, f := range tc.Fields {
		have[f.Name] = struct{}{}
	}
	for _, want := range []string{"owner_seat", "owner_gardener"} {
		if _, ok := have[want]; !ok {
			t.Errorf("type:decision schema missing field %q (gb decision create stamps it off the spawning agent bead; absence 400s every decision create with a resolvable owner — froze the release E2E on 2026.238.x, bug ka-0000003466)", want)
		}
	}

	// End-to-end: a decision create payload carrying both owner fields (the
	// shape gb decision create sends when the agent bead has a seat_key) must
	// validate clean against the builtin schema.
	fields := json.RawMessage(`{"prompt":"choose one","options":[{"id":"a","artifact_type":"report"}],"owner_seat":"gardenio/crew/foo","owner_gardener":"synthetic-gardener:foo"}`)
	if err := model.ValidateTypeFields(fields, &tc); err != nil {
		t.Errorf("owner-stamped decision create rejected by builtin type:decision schema: %v", err)
	}
}

// TestBuiltinGardenerDeliveryNudgeFieldDeclared guards the same unknown-field
// 400 class as TestBuiltinThreadFieldsDeclared, for the one-shot marker the
// gardener-delivery dispatch gate stamps on a parking decision/escalation after
// posting an aging nudge (`gb mr dispatch-gate`, bridge.DispatchGateNudgedAtField,
// bug ka-0000007496).
//
// maybePostDispatchGateNudge writes gardener_delivery_gate_nudged_at via
// UpdateBeadFields the moment it posts the nudge. If neither type schema declares
// the field, that write 400s "unknown field" — the marker never persists, so the
// evaluator's one-shot check (which keys on the marker being non-empty) sees an
// empty marker on EVERY subsequent cycle and re-posts the nudge indefinitely. The
// "one-time ping" contract silently becomes a per-cycle spam. Caught end-to-end
// by tests/e2e/scripts/test-mr-dispatch-gate-e2e.sh (leg B2). Both the decision
// (merge/hold fork) and escalation (raw approve+merge ask) handle types can park
// an MR, so BOTH must declare it.
func TestBuiltinGardenerDeliveryNudgeFieldDeclared(t *testing.T) {
	for _, typeName := range []string{"type:decision", "type:escalation"} {
		c, ok := builtinConfigs[typeName]
		if !ok {
			t.Fatalf("builtinConfigs missing %q", typeName)
		}
		var tc model.TypeConfig
		if err := json.Unmarshal(c.Value, &tc); err != nil {
			t.Fatalf("%s: unmarshal config value: %v", typeName, err)
		}
		have := make(map[string]struct{}, len(tc.Fields))
		for _, f := range tc.Fields {
			have[f.Name] = struct{}{}
		}
		if _, ok := have["gardener_delivery_gate_nudged_at"]; !ok {
			t.Errorf("%s schema missing field %q (the dispatch-gate stamps it as the aging-nudge one-shot marker; absence 400s the marker write so the nudge re-fires every cycle)", typeName, "gardener_delivery_gate_nudged_at")
		}
	}
}

// TestBuiltinTaskSupportFieldsDeclared: the slack-bridge #support watcher
// (gardenio/controller/internal/bridge/lands_support.go, Lands Track B §6.1)
// files a type:task support bead with these fields and stamps the notice on it.
// type:task rejects unknown fields, so an undeclared one 400s the create and a
// tenant's support request is never filed.
func TestBuiltinTaskSupportFieldsDeclared(t *testing.T) {
	var tc model.TypeConfig
	if err := json.Unmarshal(builtinConfigs["type:task"].Value, &tc); err != nil {
		t.Fatalf("type:task: %v", err)
	}
	have := map[string]bool{}
	for _, f := range tc.Fields {
		have[f.Name] = true
	}
	for _, want := range []string{"support_message_id", "support_channel_id", "support_notified_at", "support_notified_via", "idempotency_key"} {
		if !have[want] {
			t.Errorf("type:task schema missing field %q (the #support watcher writes it)", want)
		}
	}
}

// TestBuiltinMessageProducerFieldsDeclared guards the same unknown-field 400
// class as TestBuiltinThreadFieldsDeclared, but for type:message — the
// conversation bubble bead. Its producers (gb squawk, gb slack reply, the
// beads-viewer thread composer, and the slack-bridge inbound thread-forward
// recordThreadConversationMessage) each marshal a fixed set of fields; if any
// producer writes a key the schema does not declare, the create 400s
// "<key>: unknown field" and the message is silently dropped — so a human's
// thread reply never reaches the /threads timeline. This bit twice (thread_id:
// ka-0000003279/ka-0000005416; slack_user_id: ka-0000004306). The schema MUST be
// a superset of the union of every producer's field set.
func TestBuiltinMessageProducerFieldsDeclared(t *testing.T) {
	c, ok := builtinConfigs["type:message"]
	if !ok {
		t.Fatal("builtinConfigs missing \"type:message\"")
	}
	var tc model.TypeConfig
	if err := json.Unmarshal(c.Value, &tc); err != nil {
		t.Fatalf("type:message: unmarshal config value: %v", err)
	}
	have := make(map[string]struct{}, len(tc.Fields))
	for _, f := range tc.Fields {
		have[f.Name] = struct{}{}
	}
	// The union of every field the message producers marshal. Adding a field to
	// a producer without adding it here (and to the schema) reintroduces the
	// silent-drop bug; this list is the canonical superset to keep in sync.
	producerFields := []string{
		"source_agent",         // gb squawk / notice.go
		"text",                 // all producers
		"trigger_title",        // notice.go NoticeNotifier: trigger-engine notify / release-drought watchdog / slack reaction notices (ka-0000005920)
		"channel",              // gb slack reply
		"thread_ts",            // gb slack reply
		"slack_thread_channel", // gb slack reply / thread composer / thread-forward
		"slack_thread_ts",      // gb slack reply / thread composer / thread-forward
		"slack_user_id",        // thread-forward inbound (per-reply author id)
		"operated_by",          // unified inbound reply: originating operator for inbox attribution (ka-0000007618)
		"thread_id",            // wish-pointer (composer / thread-forward)
		"jira_key",             // gb jira reply provenance bead (recordJiraReplyBead, ka-0000000661)
		"visibility",           // gb jira reply provenance bead: internal|public (ka-0000000661)
		// /plans in-viewer chat transcript turn producer (planTranscriptTurnBead:
		// mintOperatorTurn + AssistantTurnMinter). Absence 400'd every turn mint
		// ("plan_id: unknown field") and, since the mint is best-effort, silently
		// dropped the whole transcript — surfaced by 4b beads-viewer-e2e.
		"plan_id",         // binds a turn bead to its plan epic (planTranscriptMessagesQuery)
		"turn_role",       // operator|assistant — drives the direction bubble side
		"turn_id",         // per-turn dedup key (idempotent re-mint collapse)
		"text_truncated",  // AssistantTurnMinter: >256KB reply clipped marker
		"full_length",     // AssistantTurnMinter: original length of a truncated reply
		"idempotency_key", // transcript mint sets IdempotencyKey → folded into Fields[idempotency_key]
		"handled_at",      // slack-bridge stamps a dispatched Commons message (inbound_reply.go)
		"promoted_to",     // slack-bridge stamps a chat root a mention promoted (inbound_compose.go)
	}
	for _, want := range producerFields {
		if _, ok := have[want]; !ok {
			t.Errorf("type:message schema missing field %q (a producer marshals it; absence 400s the write and silently drops the message)", want)
		}
	}
}

// TestBuiltinMessageChannelRootPostValidates is the schema-side regression for
// ka-0000002479: agents had NO path to post a top-level (channel-root) message to
// an arbitrary Slack channel. The trigger-notice path is that path — an agent
// creates a closed type:message bead with `text` and an override `channel`, and
// slack-bridge's Notice watcher posts it channel-root on the bot's chat:write.
//
// That only works if the create VALIDATES. The watcher reads Fields["channel"]
// (notice.go NoticeChannelField); this asserts the daemon's type:message schema
// actually ACCEPTS the {text, channel} an agent sets — and, to pin down the exact
// mismatch the bug was, that the schema REJECTS the old `slack_channel` name the
// watcher used to read. If someone re-adds slack_channel to the schema instead of
// keeping the watcher on `channel`, this still passes (channel stays valid); the
// gardenio-side TestNotice_ChannelOverrideUsesSchemaValidFieldName is what pins the
// watcher to `channel`. Together the two lock both halves of the seam.
func TestBuiltinMessageChannelRootPostValidates(t *testing.T) {
	c, ok := builtinConfigs["type:message"]
	if !ok {
		t.Fatal("builtinConfigs missing \"type:message\"")
	}
	var tc model.TypeConfig
	if err := json.Unmarshal(c.Value, &tc); err != nil {
		t.Fatalf("type:message: unmarshal config value: %v", err)
	}
	tc.TypeName = "message"

	// What an agent sets for a channel-root post to an arbitrary channel
	// (#acme-ui = C02P59QCRD5 in the originating repro). Must validate clean.
	valid := json.RawMessage(`{"text":"review MR 1931","channel":"C02P59QCRD5","source_agent":"crew"}`)
	if err := model.ValidateTypeFields(valid, &tc); err != nil {
		t.Errorf("channel-root notice fields must validate on type:message, got: %v", err)
	}

	// The old field name the watcher used to read must not pass as a notice
	// override — that was the bug: the override never reached the watcher.
	// slack_channel is now schema-declared as the Slack ref of a Commons
	// conversation message (Lands spec §2.3), so the daemon's card rule
	// (validateMessageCardFields) is what refuses it on a message with no kind,
	// keeping the two names from silently converging.
	invalid := json.RawMessage(`{"text":"hi","slack_channel":"C02P59QCRD5"}`)
	if err := model.ValidateTypeFields(invalid, &tc); err == nil {
		if err := validateMessageCardFields(invalid); err == nil {
			t.Error("expected slack_channel to be rejected on a type:message with no kind; " +
				"the watcher must key on the declared `channel` field, not `slack_channel` (ka-0000002479)")
		}
	}
}

// TestBuiltinMessageJiraReplyProvenanceValidates is the schema-side regression
// for ka-0000000661 BUG 2: `gb jira reply` records a best-effort type=message
// provenance bead (recordJiraReplyBead, gardenio/controller/cmd/gb/jira.go)
// carrying source_agent + text + jira_key + visibility. Those last two fields
// were undeclared, so EVERY successful reply 400'd the follow-up create
// ("jira_key: unknown field; visibility: unknown field") and — because the
// record is best-effort (logged+swallowed) — the provenance bead was silently
// never created, leaving bridge replies invisible in beads-viewer. Assert the
// exact producer field set validates clean now.
func TestBuiltinMessageJiraReplyProvenanceValidates(t *testing.T) {
	c, ok := builtinConfigs["type:message"]
	if !ok {
		t.Fatal("builtinConfigs missing \"type:message\"")
	}
	var tc model.TypeConfig
	if err := json.Unmarshal(c.Value, &tc); err != nil {
		t.Fatalf("type:message: unmarshal config value: %v", err)
	}
	tc.TypeName = "message"

	// Exactly what recordJiraReplyBead marshals on a successful reply.
	valid := json.RawMessage(`{"source_agent":"crew-1","text":"on it","jira_key":"DEVOPS-738","visibility":"internal"}`)
	if err := model.ValidateTypeFields(valid, &tc); err != nil {
		t.Errorf("gb jira reply provenance fields must validate on type:message, got: %v", err)
	}
}

// TestBuiltinAccountProbeFieldsDeclared guards the same unknown-field 400 class
// as the thread/message guards above, but for type:account — the account bead
// the controller's ratelimit-probe writes back to. The probe's handleResult
// marshals a FIXED field map (throttle_status, probe_auth_status, utilization_*,
// probe_checked_at, throttle_reset_*, ...) in ONE UpdateBeadFields call. If any
// single key is undeclared, the daemon 400s the ENTIRE batch, so NO probe data
// persists — while auth_failover's exhausted+cooldown write (which omits the
// undeclared key) still succeeds. That asymmetry is a one-way ratchet: accounts
// get marked exhausted and the probe can never clear them, freezing rotation
// fleet-wide. This bit on probe_auth_status (gd-tL1xcq8XZL): every Anthropic
// account stuck exhausted, only the two accounts that never 429'd still serving.
// The schema MUST be a superset of the union of every field the probe writes.
func TestBuiltinAccountProbeFieldsDeclared(t *testing.T) {
	c, ok := builtinConfigs["type:account"]
	if !ok {
		t.Fatal("builtinConfigs missing \"type:account\"")
	}
	var tc model.TypeConfig
	if err := json.Unmarshal(c.Value, &tc); err != nil {
		t.Fatalf("type:account: unmarshal config value: %v", err)
	}
	have := make(map[string]struct{}, len(tc.Fields))
	for _, f := range tc.Fields {
		have[f.Name] = struct{}{}
	}
	// The union of every field the ratelimit-probe writes back to an account
	// bead: the unconditional handleResult map, its conditional throttle_reset_*
	// keys, the probe_auth_status="rejected" hard-auth write, and the legacy
	// status/exhausted_at/cooldown_until reconciliation. Adding a field to the
	// probe writeback without adding it here (and to the schema) reintroduces the
	// rotation-freeze; this list is the canonical superset to keep in sync.
	probeFields := []string{
		"throttle_status",      // handleResult: result.Status
		"probe_auth_status",    // handleResult "ok" + hard-auth "rejected" (gd-tL1xcq8XZL)
		"utilization_5h",       // handleResult
		"utilization_7d",       // handleResult
		"representative_claim", // handleResult
		"overage_status",       // handleResult
		"overage_disabled_reason",
		"probe_checked_at",     // handleResult
		"throttle_reset_at",    // conditional (result.Reset > 0)
		"throttle_reset_5h_at", // conditional (result.Reset5h > 0)
		"throttle_reset_7d_at", // conditional (result.Reset7d > 0)
		"status",               // reconcileLegacyStatusFields
		"exhausted_at",         // reconcileLegacyStatusFields
		"cooldown_until",       // reconcileLegacyStatusFields
		"soft_failover_state",  // desiredSoftFailoverState writeback (separate batch)
	}
	for _, want := range probeFields {
		if _, ok := have[want]; !ok {
			t.Errorf("type:account schema missing field %q (the ratelimit-probe writes it; absence 400s the whole batch and freezes rotation)", want)
		}
	}
}

// TestBuiltinAccountBillingTypeDeclared guards the operator-set billing_type
// field on the builtin type:account schema. Unlike the probe-written fields
// above, billing_type is written by an OPERATOR (gb / a typed update setting
// billing_type=metered), not the probe — but the same one-way ratchet applies:
// if the builtin schema omits it, the daemon 400s the operator's typed write,
// billing_type never persists, and the account keeps defaulting to subscription.
// For a genuinely-metered account that means the out_of_credits recovery-hold
// (ka-0000003350) never fires and it re-enters the active↔exhausted oscillation
// MR !5192 fixed. The authoritative controller copy of this schema
// (bridge/init.go, upserted on every startup) has a mirror guard in
// init_test.go; both copies MUST carry billing_type.
func TestBuiltinAccountBillingTypeDeclared(t *testing.T) {
	c, ok := builtinConfigs["type:account"]
	if !ok {
		t.Fatal("builtinConfigs missing \"type:account\"")
	}
	var tc model.TypeConfig
	if err := json.Unmarshal(c.Value, &tc); err != nil {
		t.Fatalf("type:account: unmarshal config value: %v", err)
	}
	for _, f := range tc.Fields {
		if f.Name == "billing_type" {
			return
		}
	}
	t.Error("type:account schema missing field \"billing_type\" (operator-set; absence 400s a billing_type=metered write and re-strands metered accounts — ka-0000003350)")
}

// TestBuiltinBeadTypeNames asserts the exported accessor returns the same
// type:* keys the create path validates against, with the prefix stripped and
// the result sorted. It is the single source of truth for CLI --type filter
// validation (ka-0000006244), so a missing core type would silently let an
// invalid filter through.
func TestBuiltinBeadTypeNames(t *testing.T) {
	got := BuiltinBeadTypeNames()

	if !slices.IsSorted(got) {
		t.Errorf("BuiltinBeadTypeNames() not sorted: %v", got)
	}

	set := make(map[string]struct{}, len(got))
	for _, n := range got {
		if n == "" {
			t.Error("empty type name returned")
		}
		set[n] = struct{}{}
	}

	// Core issue types plus the gardener-carrying infra types that the
	// ka-0000006244 repro exercised must all be present.
	for _, want := range []string{
		"task", "bug", "epic", "feature", "chore",
		"advice", "formula", "molecule", "trigger", "schedule",
		"merge-request", "decision", "escalation",
	} {
		if _, ok := set[want]; !ok {
			t.Errorf("BuiltinBeadTypeNames() missing %q", want)
		}
	}

	// The prefix must be stripped — no key should retain "type:".
	for _, n := range got {
		if len(n) >= 5 && n[:5] == "type:" {
			t.Errorf("name %q still carries the type: prefix", n)
		}
	}

	// Every returned name must resolve as a known create-path type config.
	// builtinConfigs is the same map both this accessor and resolveTypeConfig
	// read, so each name must have a matching "type:<name>" entry.
	for _, n := range got {
		if _, ok := builtinConfigs["type:"+n]; !ok {
			t.Errorf("name %q has no builtinConfigs[type:%s] entry", n, n)
		}
	}
}

// TestBuiltinTriageFieldDeclared guards the fsm-triage bead→bridge→FSM path
// (ka-0000001301): the triage agent stamps recommended_affects_version on its
// own task bead (`kd update -f recommended_affects_version=…`) and the jira
// bridge reads it back to drive the Affects-Version write on the FSM ticket.
// If type:task drops the field, the agent's update 400s
// "recommended_affects_version: unknown field" and the version silently never
// reaches FSM — same unknown-field 400 class as the thread/message guards
// above.
func TestBuiltinTriageFieldDeclared(t *testing.T) {
	c, ok := builtinConfigs["type:task"]
	if !ok {
		t.Fatal("builtinConfigs missing \"type:task\"")
	}
	var tc model.TypeConfig
	if err := json.Unmarshal(c.Value, &tc); err != nil {
		t.Fatalf("type:task: unmarshal config value: %v", err)
	}
	for _, f := range tc.Fields {
		if f.Name == "recommended_affects_version" {
			return
		}
	}
	t.Error("type:task schema missing field \"recommended_affects_version\" (fsm-triage agent stamps it; the jira bridge syncs it to FSM Affects Version/s — absence 400s the agent's write)")
}

// TestBuiltinFormulaStepFieldsDeclared guards the same unknown-field 400 class
// as the thread/message/triage guards above, but for the formula step-machinery
// fields on type:task and type:decision. The pour paths (http_formula_pour.go
// and cmd/kd/formula_apply.go) stamp waits_for and on_complete onto step beads;
// cmd/kd/formula_expand.go stamps expansion_var on each deferred-expansion step;
// and the documented output-threading convention has a host step write
// dwf_result so `kd formula expand --from-field dwf_result.X` / {{step.output.X}}
// can thread structured output onto downstream molecules. A step defaults to
// type=task, and a decision-gate step becomes type=decision — both can carry
// these fields — so BOTH schemas must declare them. Their absence 400'd every
// pour of an on_complete/waits_for formula mid-molecule and every deferred
// expand, breaking the on_complete feature end-to-end on the live daemon
// (ka-0000002559). Adding a field to a pour/expand path without adding it here
// (and to the schema) reintroduces the mid-molecule 400.
func TestBuiltinFormulaStepFieldsDeclared(t *testing.T) {
	// The union of every formula step-machinery field the pour/expand paths
	// marshal onto a step bead.
	stepFields := []string{
		"waits_for",         // pour paths: fanout sync mode ("all-children")
		"on_complete",       // pour paths: deferred expansion spec (for_each/var/expand/expand_vars)
		"expansion_var",     // formula_expand.go: per-iteration value on each expanded step
		"dwf_result",        // host-written output-threading field (--from-field / {{step.output.X}})
		"suggest_new_agent", // formula_apply.go / bridge formula_instantiate.go: step-role dispatcher opt-in (a real bool)
		"step_role",         // formula_apply.go / bridge formula_instantiate.go: role resolved at pour time, so the dispatcher need not re-derive it
		"molecule_id",       // bridge formula_instantiate.go: stamped UNCONDITIONALLY on every bridge-poured step — its absence 400'd step 1 of every bridge pour, leaving a childless molecule (ka-0000001148)
		"step_project",      // bridge formula_instantiate.go: cross-project dispatch target
		"dispatched_agent",  // bridge formula_step_dispatch.go: stamped BEFORE the spawn as the durable dispatch guard — its absence made the dispatcher decline-and-return forever, never spawning
		"dispatched_by",     // bridge formula_step_dispatch.go: dispatch provenance ("formula-step-dispatcher"), stamped alongside dispatched_agent
		"gate_type",         // pour paths: gate kind ("manual"/"sync"/"decision") — stamped on EVERY gate step, but only a decision gate flips typ to "decision", so a manual/sync gate step keeps type=task and needs it here (ka-0000006567)
		"timeout",           // pour paths: gate timeout ("72h") — stamped alongside gate_type on a gate step
	}
	for _, typeName := range []string{"type:task", "type:decision"} {
		c, ok := builtinConfigs[typeName]
		if !ok {
			t.Fatalf("builtinConfigs missing %q", typeName)
		}
		var tc model.TypeConfig
		if err := json.Unmarshal(c.Value, &tc); err != nil {
			t.Fatalf("%s: unmarshal config value: %v", typeName, err)
		}
		have := make(map[string]model.FieldDef, len(tc.Fields))
		for _, f := range tc.Fields {
			have[f.Name] = f
		}
		for _, want := range stepFields {
			fd, ok := have[want]
			if !ok {
				t.Errorf("%s schema missing field %q (a formula pour/expand path stamps it; absence 400s the pour mid-molecule and every deferred expand)", typeName, want)
				continue
			}
			// on_complete and dwf_result carry JSON payloads (nested maps); the
			// scalar sync/iteration markers are strings. A wrong type would let
			// the create through the unknown-field check only to reject the value.
			var wantType model.FieldType
			switch want {
			case "on_complete", "dwf_result":
				wantType = model.FieldTypeJSON
			case "suggest_new_agent":
				// formula_apply.go assigns a real Go bool, so a string-typed
				// declaration would pass the unknown-field check and then fail
				// validation with "must be a string" — the 400 just moves.
				wantType = model.FieldTypeBoolean
			default:
				wantType = model.FieldTypeString
			}
			if fd.Type != wantType {
				t.Errorf("%s field %q type=%q, want %q", typeName, want, fd.Type, wantType)
			}
		}
	}
}

// TestBuiltinEscalationReflagPingbackFieldsDeclared guards the same
// unknown-field 400 class as the thread/message/formula-step guards above, but
// for the re-surface bookkeeping fields on type:escalation. `gb escalation
// reflag` (runReflag) and `gb escalation pingback` PATCH these fields to
// re-surface a stalled escalation to the operator, and the slack-bridge
// auto-pingback loop + notify handshake stamp the rest. type:escalation
// declared only the create-time operation fields, so EVERY reflag/pingback
// 400'd "unknown field" on the live daemon — gardeners could not re-notify the
// operator about a stalled P0 and had to fall back to kd comment + gb squawk
// (ka-0000004605). The schema MUST be a superset of the union of every field
// these re-surface writers marshal; adding a field to a writer without adding
// it here (and to the schema) reintroduces the re-surface 400.
func TestBuiltinEscalationReflagPingbackFieldsDeclared(t *testing.T) {
	c, ok := builtinConfigs["type:escalation"]
	if !ok {
		t.Fatal("builtinConfigs missing \"type:escalation\"")
	}
	var tc model.TypeConfig
	if err := json.Unmarshal(c.Value, &tc); err != nil {
		t.Fatalf("type:escalation: unmarshal config value: %v", err)
	}
	have := make(map[string]struct{}, len(tc.Fields))
	for _, f := range tc.Fields {
		have[f.Name] = struct{}{}
	}
	// The union of every re-surface bookkeeping field written to an escalation
	// bead. Reflag: runReflag (cmd/gb/decision.go) stamps reflag_* + pingback_*
	// (source="reflag"). Manual pingback: cmd/gb/escalation.go stamps pingback_*
	// (source="manual"). Auto-pingback loop (bridge/escalation_pingback_loop.go)
	// stamps auto_pingback_count + pingback_* (source="auto") +
	// queue_alert_requested_at. Bridge notify handshake (bridge/escalations.go,
	// bridge/bot_escalations.go) stamps notified_at / slack_channel / slack_ts /
	// slack_thread_ts / pingback_notified_at / queue_alert_notified_at. Create-time
	// plan pin: cmd/gb/escalation.go stamps requesting_plan. Thread membership:
	// the server transitively copies thread_id onto an escalation raised inside a
	// Slack thread (maybeCopyThreadID, auto_parent.go) and an explicit create can
	// supply it directly (ka-0000000226). Review-flag detector 3
	// (bridge/escalation_review_flag.go, applyReviewFlag) stamps the mr_landed*
	// latch + companions in one atomic write when a "merge THEN apply"
	// escalation's MR lands; the schema gap that omitted these five 400'd every
	// flip and left detector 3 100% dead for 21 days (ka-0000004864).
	// Operator DM (bridge/escalation_operator_dm.go, ka-0000002669) stamps
	// operator_dm_at / operator_dm_channel after DMing the human in operated_by.
	// That was the THIRD instance of this class: the stamp 400'd on the live
	// daemon while the DM itself succeeded, so the failure was invisible except
	// as a logger.Warn — and the `gb escalation show` readback rendered nothing.
	writerFields := []string{
		"reflag_count",             // runReflag
		"reflag_reason",            // runReflag (--reason)
		"reflagged_at",             // runReflag
		"reflagged_by",             // runReflag
		"pingback_count",           // reflag + manual + auto pingback
		"pingback_reason",          // reflag + manual + auto pingback (--reason)
		"pingback_by",              // reflag + manual + auto pingback
		"pingback_source",          // "reflag" | "manual" | "auto"
		"pingback_requested_at",    // reflag + manual + auto pingback
		"pingback_notified_at",     // bridge maybeNotifyPingback handshake
		"auto_pingback_count",      // auto-pingback loop cadence counter
		"queue_alert_requested_at", // auto-pingback loop past 48h threshold
		"queue_alert_notified_at",  // bridge maybeNotifyQueueAlert handshake
		"last_digested_at",         // bridge staleness-digest loop per-bead dedup (ka-0000006001)
		"notified_at",              // bridge NotifyEscalation card-posted stamp
		"operator_dm_at",           // bridge EscalationOperatorDM personal-ping stamp (ka-0000002669)
		"operator_dm_channel",      // bridge EscalationOperatorDM personal-ping stamp (ka-0000002669)
		"mr_landed",                // review-flag detector 3 idempotency latch (escalation_review_flag.go)
		"mr_landed_state",          // review-flag detector 3 (escalation_review_flag.go)
		"mr_landed_iid",            // review-flag detector 3 (escalation_review_flag.go)
		"mr_landed_at",             // review-flag detector 3 (escalation_review_flag.go)
		"mr_landed_url",            // review-flag detector 3 (escalation_review_flag.go)
		"slack_channel",            // bridge operator-visibility stamp
		"slack_ts",                 // bridge operator-visibility stamp
		"slack_thread_ts",          // bridge operator-visibility stamp
		"requesting_plan",          // gb escalation create plan pin (ka-0000006860)
		"thread_id",                // server transitive thread-membership copy (maybeCopyThreadID, ka-0000000226)
		"close_requesting_task",    // gb escalation create --requesting-task (gd-91b7zMOR2z)
	}
	for _, want := range writerFields {
		if _, ok := have[want]; !ok {
			t.Errorf("type:escalation schema missing field %q (a reflag/pingback/notify writer marshals it; absence 400s the re-surface PATCH and gardeners cannot re-notify the operator)", want)
		}
	}
}

// TestBuiltinReproFieldsDeclared guards the same unknown-field 400 class as the
// thread/message/triage/formula-step guards above, but for the repro-harness
// gate fields on type:task, type:molecule, AND type:rich-report. The
// repro-harness formula's Phase-4 emit step (ka-0000002084) writes gate_passed /
// repro_evidence / repro_ticket / repro_scorecard onto the emit-step bead and
// MIRRORS gate_passed + repro_evidence + repro_scorecard onto the triage bead
// (formula step 3). The emit step is type=task (formula step beads default to
// task) and the pour-level epic is a molecule, so both declare the fields — but
// fsm-triage creates its triage beads as type=rich-report, so that schema must
// declare them too or the mirror PATCH 400s on the exact type triage produces
// (ka-0000003501). Their absence 400'd the emit write on the live FSM-468 run
// ("gate_passed: unknown field"), forcing the run to fall back to LABELS which
// the repro cockpit cannot query by ticket × gate_passed (ka-0000002579). And
// repro_scorecard was never provisioned at all, so the per-axis critic scores
// (ka-0000008255) were not queryable anywhere. Adding a repro field to the
// emit/mirror path without adding it here (and to the schema) reintroduces the
// 400.
func TestBuiltinReproFieldsDeclared(t *testing.T) {
	reproFields := map[string]model.FieldType{
		"gate_passed":     model.FieldTypeBoolean,
		"repro_evidence":  model.FieldTypeString,
		"repro_ticket":    model.FieldTypeString,
		"repro_scorecard": model.FieldTypeString,
	}
	for _, typeName := range []string{"type:task", "type:molecule", "type:rich-report", "type:bundle"} {
		c, ok := builtinConfigs[typeName]
		if !ok {
			t.Fatalf("builtinConfigs missing %q", typeName)
		}
		var tc model.TypeConfig
		if err := json.Unmarshal(c.Value, &tc); err != nil {
			t.Fatalf("%s: unmarshal config value: %v", typeName, err)
		}
		have := make(map[string]model.FieldDef, len(tc.Fields))
		for _, f := range tc.Fields {
			have[f.Name] = f
		}
		for name, wantType := range reproFields {
			fd, ok := have[name]
			if !ok {
				t.Errorf("%s schema missing field %q (the repro-harness emit/mirror step stamps it; absence 400s the emit write and forces a non-queryable label fallback)", typeName, name)
				continue
			}
			if fd.Type != wantType {
				t.Errorf("%s field %q type=%q, want %q", typeName, name, fd.Type, wantType)
			}
		}
	}
}

// TestBuiltinCodexKeeperFieldsDeclared guards the Codex subscription refresh
// keeper's account fields (gd-6ggiM3hiV5). type:account is a strict schema, so a
// PATCH carrying an undeclared field 400s whole: if codex_rejected_refresh_fp or
// codex_auth_error is missing, markRejected's write never lands, probe_auth_status
// stays unrejected, and the keeper re-POSTs the same dead refresh token on every
// pass -- inverting the never-retry guarantee.
func TestBuiltinCodexKeeperFieldsDeclared(t *testing.T) {
	c, ok := builtinConfigs["type:account"]
	if !ok {
		t.Fatal("builtinConfigs missing type:account")
	}
	var tc model.TypeConfig
	if err := json.Unmarshal(c.Value, &tc); err != nil {
		t.Fatalf("type:account: unmarshal config value: %v", err)
	}
	have := make(map[string]model.FieldDef, len(tc.Fields))
	for _, f := range tc.Fields {
		have[f.Name] = f
	}
	for _, name := range []string{
		"probe_auth_status", "codex_rejected_refresh_fp", "codex_auth_error",
		"throttle_status", "utilization_5h", "utilization_7d", "representative_claim",
		"probe_checked_at", "throttle_reset_at", "throttle_reset_5h_at", "throttle_reset_7d_at",
	} {
		fd, ok := have[name]
		if !ok {
			t.Errorf("type:account schema missing field %q (the Codex keeper stamps it; absence 400s the whole account PATCH)", name)
			continue
		}
		if fd.Type != model.FieldTypeString {
			t.Errorf("type:account field %q type=%q, want %q", name, fd.Type, model.FieldTypeString)
		}
	}
}

// TestBuiltinBundleSupersetOfRichReport guards the fsm-triage migration off
// type=rich-report onto type=bundle (ka-0000004066, epic ka-0000007561). The
// widget/report deprecation retires rich-report as an authoring surface; the
// only live producer is fsm-triage, which stamps report_kind / headline /
// severity / source_ref (plus the mirrored repro-gate fields) on the bead it
// creates. For the migration to be a no-breaking-change drop-in, type:bundle
// MUST declare every field type:rich-report declares — otherwise the migrated
// emit 400s "severity: unknown field" on the exact bead type triage now
// produces, the same trap the rich-report schema was itself patched for
// (ka-0000003501). Asserting the SUPERSET (not a hand-copied field list) means a
// future field added to rich-report is caught here until it is also added to
// bundle, so the two schemas can never silently drift apart while both types
// exist.
func TestBuiltinBundleSupersetOfRichReport(t *testing.T) {
	fieldsOf := func(typeName string) map[string]model.FieldDef {
		c, ok := builtinConfigs[typeName]
		if !ok {
			t.Fatalf("builtinConfigs missing %q", typeName)
		}
		var tc model.TypeConfig
		if err := json.Unmarshal(c.Value, &tc); err != nil {
			t.Fatalf("%s: unmarshal config value: %v", typeName, err)
		}
		out := make(map[string]model.FieldDef, len(tc.Fields))
		for _, f := range tc.Fields {
			out[f.Name] = f
		}
		return out
	}
	bundle := fieldsOf("type:bundle")
	for name, rr := range fieldsOf("type:rich-report") {
		bf, ok := bundle[name]
		if !ok {
			t.Errorf("type:bundle missing field %q that type:rich-report declares — fsm-triage migration (ka-0000004066) would 400 %q on the migrated triage bead", name, name)
			continue
		}
		if bf.Type != rr.Type {
			t.Errorf("type:bundle field %q type=%q, but type:rich-report declares it as %q — the migrated emit would fail type validation", name, bf.Type, rr.Type)
		}
	}
}

// TestBuiltinWorkspaceFieldDeclared guards the same unknown-field 400 class as
// the thread/message/formula-step/repro guards above, but for the worktree-
// recovery `workspace` field that `gb workspace setup` stores. storeWorkspaceInfo
// (gardenio/controller/cmd/gb/workspace.go) PATCHes Fields["workspace"] on the
// bead it just cut a worktree for, and resolveBaseBranch reads it back off the
// bead AND off parent beads (usually epics) to stack child worktrees. No issue-
// kind schema declared it, so EVERY `gb workspace setup` write 400'd
// ("workspace: unknown field"): the worktree landed on disk but the recovery
// metadata was silently dropped, making worktree recovery-after-restart a no-op
// for the standard MR-isolation workflow (ka-0000006623). Because setup targets
// whatever bead the agent claimed (task dominant, but bug/feature/epic are valid
// targets) and the reader walks parents of any type, the field must be present
// on ALL issue-kind builtin types, so assert the shared issueFields helper
// installed it everywhere. Removing it from issueFields (or a type opting out)
// reintroduces the setup 400.
func TestBuiltinWorkspaceFieldDeclared(t *testing.T) {
	// Every issue-kind builtin type `gb workspace setup` can be pointed at, or
	// whose bead can be a parent walked by resolveBaseBranch.
	issueTypes := []string{
		"type:task",
		"type:bug",
		"type:feature",
		"type:chore",
		"type:epic",
	}
	for _, typeName := range issueTypes {
		c, ok := builtinConfigs[typeName]
		if !ok {
			t.Fatalf("builtinConfigs missing %q", typeName)
		}
		var tc model.TypeConfig
		if err := json.Unmarshal(c.Value, &tc); err != nil {
			t.Fatalf("%s: unmarshal config value: %v", typeName, err)
		}
		var fd *model.FieldDef
		for i := range tc.Fields {
			if tc.Fields[i].Name == "workspace" {
				fd = &tc.Fields[i]
				break
			}
		}
		if fd == nil {
			t.Errorf("%s schema missing field %q (gb workspace setup stamps it; absence 400s the recovery-metadata write and worktree recovery-after-restart is silently a no-op)", typeName, "workspace")
			continue
		}
		if fd.Type != model.FieldTypeString {
			t.Errorf("%s field %q type=%q, want %q", typeName, "workspace", fd.Type, model.FieldTypeString)
		}
	}
}

// TestBuiltinResumeChainFieldDeclared guards the transcript-continuity handoff
// field (ka-0000003160). reopenOrphanedClaimedTask (gardenio controller) stamps
// resume_s3_key on the WORKED TASK bead when a crew agent is involuntarily
// killed, so the gardener-respawned successor (new bead id, same task_id) can
// resume the prior transcript instead of orphaning it. The claimed work can be
// any issue-kind type, so every one must declare the field via issueFields or
// the handoff write 400s "resume_s3_key: unknown field" — same unknown-field
// family as workspace (ka-0000006623).
func TestBuiltinResumeChainFieldDeclared(t *testing.T) {
	issueTypes := []string{
		"type:task",
		"type:bug",
		"type:feature",
		"type:chore",
		"type:epic",
	}
	for _, typeName := range issueTypes {
		c, ok := builtinConfigs[typeName]
		if !ok {
			t.Fatalf("builtinConfigs missing %q", typeName)
		}
		var tc model.TypeConfig
		if err := json.Unmarshal(c.Value, &tc); err != nil {
			t.Fatalf("%s: unmarshal config value: %v", typeName, err)
		}
		var fd *model.FieldDef
		for i := range tc.Fields {
			if tc.Fields[i].Name == "resume_s3_key" {
				fd = &tc.Fields[i]
				break
			}
		}
		if fd == nil {
			t.Errorf("%s schema missing field %q (reopenOrphanedClaimedTask stamps it for transcript continuity; absence 400s the handoff and the respawn cold-starts)", typeName, "resume_s3_key")
			continue
		}
		if fd.Type != model.FieldTypeString {
			t.Errorf("%s field %q type=%q, want %q", typeName, "resume_s3_key", fd.Type, model.FieldTypeString)
		}
	}
}

// TestBuiltinAgentSessionKeyFieldsDeclared guards the type:agent side of the
// transcript-continuity fix (ka-0000003160): resolveCanonicalS3Key stamps
// s3_session_key on the pod's own agent bead so a transcript lookup keyed on
// the NEW bead id (deathtranscript.SessionKeyForBead) resolves to the canonical
// prefix. coop_session_id is the sibling override the same resolver honors.
// Both must be declared or the stamp 400s "s3_session_key: unknown field".
func TestBuiltinAgentSessionKeyFieldsDeclared(t *testing.T) {
	c, ok := builtinConfigs["type:agent"]
	if !ok {
		t.Fatalf("builtinConfigs missing type:agent")
	}
	var tc model.TypeConfig
	if err := json.Unmarshal(c.Value, &tc); err != nil {
		t.Fatalf("type:agent: unmarshal config value: %v", err)
	}
	for _, want := range []string{"s3_session_key", "coop_session_id"} {
		var fd *model.FieldDef
		for i := range tc.Fields {
			if tc.Fields[i].Name == want {
				fd = &tc.Fields[i]
				break
			}
		}
		if fd == nil {
			t.Errorf("type:agent schema missing field %q (resolveCanonicalS3Key stamps it; absence 400s the transcript-lookup redirect)", want)
			continue
		}
		if fd.Type != model.FieldTypeString {
			t.Errorf("type:agent field %q type=%q, want %q", want, fd.Type, model.FieldTypeString)
		}
	}
}

// TestBuiltinAgentGardenerIDDeclared guards the canonical gardener-ownership field
// (persistent-agents Phase 1, task ka-0000006626, design memo §2.3). The gardenio
// controller's beadsapi create-choke-point stamps gardener_id on EVERY agent bead
// at spawn so the viewer's "Unowned" bucket is empty by construction. Declared on
// the builtin (not just the store row) so resolveTypeConfig's builtin-merge
// repairs an already-persisted stale type:agent config on upgrade — otherwise the
// stamp 400s "gardener_id: unknown field" on every spawn against a prod daemon
// whose stored config predates this field.
func TestBuiltinAgentGardenerIDDeclared(t *testing.T) {
	c, ok := builtinConfigs["type:agent"]
	if !ok {
		t.Fatalf("builtinConfigs missing type:agent")
	}
	var tc model.TypeConfig
	if err := json.Unmarshal(c.Value, &tc); err != nil {
		t.Fatalf("type:agent: unmarshal config value: %v", err)
	}
	var fd *model.FieldDef
	for i := range tc.Fields {
		if tc.Fields[i].Name == "gardener_id" {
			fd = &tc.Fields[i]
			break
		}
	}
	if fd == nil {
		t.Fatalf("type:agent schema missing field \"gardener_id\" (beadsapi.stampAgentGardenerID stamps it on every spawn; absence 400s every agent create)")
	}
	if fd.Type != model.FieldTypeString {
		t.Errorf("type:agent field \"gardener_id\" type=%q, want %q", fd.Type, model.FieldTypeString)
	}
}

// TestBuiltinIdempotencyKeyDeclared guards the unknown-field 400 class for
// idempotency_key specifically. createBead folds
// CreateBeadRequest.IdempotencyKey into Fields[idempotency_key] BEFORE field
// validation runs, so a mint that carries an IdempotencyKey against a type
// that does not declare the field 400s "idempotency_key: unknown field" — and
// because IdempotencyKey is set by the *caller*, the failure lands on the
// retry/dedup path that exists precisely to make things more reliable.
//
// issue-kind types get the field for free via the shared auditFields block;
// the types below are config-/data-kind and spell their schemas out by hand,
// so each has to declare it explicitly. type:message already did. type:agent
// did not, and gardenio!5958 added a per-wake-up IdempotencyKey to
// respawnThreadAgent — so every Slack wake-up of a done thread agent 400'd
// until the schema caught up (thread-1784938967-063249). type:doc is the
// remaining hand-written type whose producer (controller/internal/scif
// emitter) plumbs an IdempotencyKey through, so it is pinned here too.
//
// Keep this list in sync with the CreateBeadRequest{IdempotencyKey: ...} mint
// sites in the gardenio controller.
func TestBuiltinIdempotencyKeyDeclared(t *testing.T) {
	// type:molecule and type:decision are the pour step/container types the pour
	// handler stamps an idempotency key on (ka-0000005875) so a double-submit
	// collapses to one molecule + one step tree; both carry hand-rolled field
	// lists (not issueFields/auditFields), so the field must be declared explicitly
	// or every idempotent pour 400s "idempotency_key: unknown field".
	for _, typeName := range []string{"type:agent", "type:message", "type:doc", "type:molecule", "type:decision"} {
		c, ok := builtinConfigs[typeName]
		if !ok {
			t.Fatalf("builtinConfigs missing %q", typeName)
		}
		var tc model.TypeConfig
		if err := json.Unmarshal(c.Value, &tc); err != nil {
			t.Fatalf("%s: unmarshal config value: %v", typeName, err)
		}
		var fd *model.FieldDef
		for i := range tc.Fields {
			if tc.Fields[i].Name == "idempotency_key" {
				fd = &tc.Fields[i]
				break
			}
		}
		if fd == nil {
			t.Errorf("%s schema missing field %q (createBead folds IdempotencyKey into fields before validation; absence 400s every idempotent mint of this type)", typeName, "idempotency_key")
			continue
		}
		if fd.Type != model.FieldTypeString {
			t.Errorf("%s field %q type=%q, want %q", typeName, "idempotency_key", fd.Type, model.FieldTypeString)
		}
	}
}

// TestBuiltinAgentTeardownFieldsDeclared guards the unknown-field 400 class for
// fields that `gb init` writes on the agent bead during POD TEARDOWN. These are
// the easiest instances to ship broken: teardown writes are best-effort, their
// failures are logged rather than surfaced, and the feature they gate degrades
// silently instead of erroring.
//
// interrupted_breadcrumb_posted is the mark-before-post dedup marker for the
// "I was interrupted" Slack breadcrumb (ka-0000002138 / ka-0000002080). It was
// never declared on type:agent, so markInterruptedBreadcrumbPosted 400'd on
// every teardown, `marked` stayed false, and a thread that restart-looped got
// one breadcrumb per cycle — the exact duplicate spam the fix was written to
// prevent, with the failure visible only as a log line in a dying pod:
//
//	[gb init] interrupted-breadcrumb: failed to stamp dedup marker on gd-…:
//	  HTTP 400: invalid fields: validation failed:
//	  interrupted_breadcrumb_posted: unknown field
func TestBuiltinAgentTeardownFieldsDeclared(t *testing.T) {
	c, ok := builtinConfigs["type:agent"]
	if !ok {
		t.Fatal("builtinConfigs missing \"type:agent\"")
	}
	var tc model.TypeConfig
	if err := json.Unmarshal(c.Value, &tc); err != nil {
		t.Fatalf("type:agent: unmarshal config value: %v", err)
	}
	have := make(map[string]model.FieldType, len(tc.Fields))
	for _, f := range tc.Fields {
		have[f.Name] = f.Type
	}
	for _, want := range []string{"interrupted_breadcrumb_posted", "close_reason", "wrapup"} {
		ft, ok := have[want]
		if !ok {
			t.Errorf("type:agent schema missing field %q (gb init writes it on teardown; absence 400s the write and the feature it gates degrades silently)", want)
			continue
		}
		if ft != model.FieldTypeString {
			t.Errorf("type:agent field %q type=%q, want %q", want, ft, model.FieldTypeString)
		}
	}
}

// TestBuiltinAgentFireNowFieldsDeclared guards the same unknown-field 400 class
// as the teardown/session-key tests above, but for the `gb gardener fire-now`
// spawn path (gardenio/controller/cmd/gb/gardener_firenow.go) — the documented,
// first-class way to VERIFY a newly-armed or revived gardener actually cycles.
//
// runGardenerFireNow stamps fire_now="true" (spawn-source discriminator) and
// fire_now_by=<actor> (the human who fired it, kept separate from operated_by)
// into the extraFields it hands daemon.SpawnAgentResult, which persists every
// key onto the type=agent bead. type:agent is a strict/closed schema, so an
// undeclared key 400s the WHOLE create — the fire-now cycle never spawns:
//
//	Error: firing gardener cycle for gd-…: spawning agent "…": creating bead:
//	  HTTP 400: invalid fields: validation failed:
//	  fire_now: unknown field; fire_now_by: unknown field
//
// This broke fire-now for EVERY gardener (ka-0000002620) — the exact analogue of
// gd-CSwwLRMGbz (the formula-step dispatcher 400ing on molecule_id/source_formula/
// source_step_id). The schema MUST declare both fields the fire-now path stamps.
func TestBuiltinAgentFireNowFieldsDeclared(t *testing.T) {
	c, ok := builtinConfigs["type:agent"]
	if !ok {
		t.Fatal("builtinConfigs missing \"type:agent\"")
	}
	var tc model.TypeConfig
	if err := json.Unmarshal(c.Value, &tc); err != nil {
		t.Fatalf("type:agent: unmarshal config value: %v", err)
	}
	have := make(map[string]model.FieldType, len(tc.Fields))
	for _, f := range tc.Fields {
		have[f.Name] = f.Type
	}
	for _, want := range []string{"fire_now", "fire_now_by"} {
		ft, ok := have[want]
		if !ok {
			t.Errorf("type:agent schema missing field %q (gb gardener fire-now stamps it; absence 400s the whole agent create so fire-now cannot verify a gardener cycles — ka-0000002620)", want)
			continue
		}
		if ft != model.FieldTypeString {
			t.Errorf("type:agent field %q type=%q, want %q", want, ft, model.FieldTypeString)
		}
	}
}

// TestBuiltinTriggerFireErrorFieldsDeclared guards the same unknown-field 400
// class as TestBuiltinGardenerChainFieldDeclared, for the fire-error diagnostic
// fields the trigger engine stamps when a fireAction RAISES (bug
// ka-0000002991). The engine's stampFireError
// (gardenio/controller/internal/bridge/triggers.go) persists last_fire_error /
// last_fire_error_at (and, for gardeners, bumps the integer consecutive_failures
// mirror) so a gardener that MATCHES EVERY CYCLE AND FAILS is visible in the data
// plane instead of byte-identical to an idle/gated one. If the schema omits
// these fields the stamp 400s and the diagnostic evaporates — the exact failure
// that had two agents mis-diagnose a hard spawn error as a silent decline
// (ka-0000004529). Because resolveTypeConfig merges builtin fields missing from
// a stored config on the validation path, declaring them here also un-shadows
// the prod store row (advice ka-0000000061).
func TestBuiltinTriggerFireErrorFieldsDeclared(t *testing.T) {
	// (1) Builtin declares both, typed string, optional.
	c, ok := builtinConfigs["type:trigger"]
	if !ok {
		t.Fatalf("builtinConfigs missing %q", "type:trigger")
	}
	var tc model.TypeConfig
	if err := json.Unmarshal(c.Value, &tc); err != nil {
		t.Fatalf("type:trigger: unmarshal config value: %v", err)
	}
	have := make(map[string]model.FieldType, len(tc.Fields))
	required := make(map[string]bool, len(tc.Fields))
	for _, f := range tc.Fields {
		have[f.Name] = f.Type
		required[f.Name] = f.Required
	}
	for _, want := range []string{"last_fire_error", "last_fire_error_at"} {
		ft, ok := have[want]
		if !ok {
			t.Errorf("type:trigger schema missing field %q (stampFireError stamps it on a fireAction error; absence 400s the write so the failure is invisible in the data plane — ka-0000002991)", want)
			continue
		}
		if ft != model.FieldTypeString {
			t.Errorf("type:trigger field %q type=%q, want %q", want, ft, model.FieldTypeString)
		}
		if required[want] {
			t.Errorf("type:trigger field %q must be optional (only present after a fire error)", want)
		}
	}

	// (2) A stored config that OMITS the fields (the prod shadow shape) must
	// still validate a write carrying them, via the resolveTypeConfig merge.
	srv, store, ctx := testCtx(t)
	shadow := `{"kind":"config","fields":[
		{"name":"source","type":"string"},
		{"name":"action","type":"string","required":true},
		{"name":"consecutive_failures","type":"integer"}
	]}`
	store.configs["type:trigger"] = &model.Config{Key: "type:trigger", Value: json.RawMessage(shadow)}

	resolved, err := srv.resolveTypeConfig(ctx, "trigger")
	if err != nil {
		t.Fatalf("resolveTypeConfig(trigger): %v", err)
	}
	if err := model.ValidateTypeFields(
		json.RawMessage(`{"action":"spawn_agent","last_fire_error":"HTTP 400: operator could not be resolved","last_fire_error_at":"2026-08-14T06:00:00Z"}`),
		resolved,
	); err != nil {
		t.Errorf("ValidateTypeFields rejected the fire-error fields against a shadowing stored config — the builtin-merge did not un-shadow them: %v", err)
	}
}

// TestBuiltinCredentialType guards the credential inventory operator decision
// gd-NnaRPh0AY0 chose (`structured-field`). The failure it prevents is the one
// that forced the decision: the shared fine-grained GitHub PAT every agent pod
// mounts expires 2026-10-19, and that date lived only as prose in a bead
// comment, so nothing could query it and nothing could alert on it.
//
// Three things are asserted, because each has its own way of silently undoing
// the decision:
//  1. the type resolves at all — resolveTypeConfig consults builtins and the
//     configs KV table but NEVER config beads, so a type that exists only as a
//     config bead 400s "unknown bead type" on every create (ka-0000001549);
//  2. expires_at is a `timestamp`, not a string — a string expiry is prose with
//     extra steps and cannot be filtered on by a reaper or a dashboard; and
//  3. a realistic row validates, including the multi-valued secret_ref that the
//     shared PAT needs because it lives in two Secrets at once.
func TestBuiltinCredentialType(t *testing.T) {
	c, ok := builtinConfigs["type:credential"]
	if !ok {
		t.Fatalf("builtinConfigs missing type:credential — gd-NnaRPh0AY0 chose structured-field; a config bead alone does not register a type")
	}
	var tc model.TypeConfig
	if err := json.Unmarshal(c.Value, &tc); err != nil {
		t.Fatalf("unmarshal type:credential config: %v", err)
	}
	tc.TypeName = "credential"

	byName := make(map[string]model.FieldDef, len(tc.Fields))
	for _, f := range tc.Fields {
		byName[f.Name] = f
	}
	// The five fields the decision names verbatim.
	for _, want := range []string{"token_name", "scopes", "minted_at", "expires_at", "secret_ref"} {
		if _, ok := byName[want]; !ok {
			t.Errorf("type:credential schema missing %q, named in gd-NnaRPh0AY0", want)
		}
	}
	for _, name := range []string{"minted_at", "expires_at", "revoked_at"} {
		if got := byName[name].Type; got != model.FieldTypeTimestamp {
			t.Errorf("%s is %q, want %q — a non-timestamp expiry is not queryable, which is the whole decision", name, got, model.FieldTypeTimestamp)
		}
	}
	if got := byName["secret_ref"].Type; got != model.FieldTypeStrings {
		t.Errorf("secret_ref is %q, want %q — the shared PAT lives in both gardenio-git-credentials and gascity-git-credentials, and a single ref records half the blast radius", got, model.FieldTypeStrings)
	}
	if tc.AllowAdditionalFields {
		t.Error("type:credential must not allow additional fields; an open schema lets a typo'd expiry key validate and then never alert")
	}
	if tc.WritePolicy == nil || tc.WritePolicy.MinRole != "gardenio-admin" {
		t.Errorf("type:credential write_policy = %+v, want min_role gardenio-admin — the credential inventory is exactly what an attacker would edit", tc.WritePolicy)
	}

	// The real row for the PAT that forced the decision.
	row := []byte(`{
		"token_name": "gardenio-crew-contents",
		"credential_type": "pat-fine-grained",
		"consumer": "agent pods (clone, push, gh pr create)",
		"scopes": ["contents:write", "metadata:read"],
		"secret_ref": ["k8s:gardenio/gardenio-git-credentials", "k8s:gardenio/gascity-git-credentials"],
		"minted_at": "2026-09-19T00:00:00Z",
		"expires_at": "2026-10-19T00:00:00Z"
	}`)
	if err := model.ValidateTypeFields(row, &tc); err != nil {
		t.Errorf("ValidateTypeFields rejected the gardenio-crew-contents row: %v", err)
	}

	// A prose expiry must not sneak through; that is the state being retired.
	prose := []byte(`{"token_name": "x", "credential_type": "pat-classic", "expires_at": "expires 2026-10-19"}`)
	if err := model.ValidateTypeFields(prose, &tc); err == nil {
		t.Error("ValidateTypeFields accepted a prose expires_at; the timestamp type is what makes the field alertable")
	}

	// And an undeclared field is still rejected, so the schema cannot rot.
	if err := model.ValidateTypeFields([]byte(`{"token_name":"x","credential_type":"pat-classic","expiry":"2026-10-19T00:00:00Z"}`), &tc); err == nil {
		t.Error("ValidateTypeFields accepted an undeclared `expiry` field; a near-miss key that validates is worse than a rejection")
	}
}

// TestBuiltinEscalationReplayClaimFieldDeclared pins the field that
// ClaimEscalationReplay / ReleaseEscalationReplayClaim write with raw SQL
// (gd-G64kIuCFlT). The raw UPDATE bypasses schema validation, so an
// undeclared key would only surface later as a grandfathered unknown field,
// and an operator could not reset a stuck claim through a typed PATCH on a
// bead that never carried it.
func TestBuiltinEscalationReplayClaimFieldDeclared(t *testing.T) {
	var tc model.TypeConfig
	if err := json.Unmarshal(builtinConfigs["type:escalation"].Value, &tc); err != nil {
		t.Fatalf("unmarshal type:escalation: %v", err)
	}
	for _, f := range tc.Fields {
		if f.Name == "replay_in_progress" {
			if f.Type != "boolean" {
				t.Errorf("replay_in_progress type = %q, want boolean (the claim SQL writes a JSON bool)", f.Type)
			}
			return
		}
	}
	t.Error(`type:escalation schema missing field "replay_in_progress"`)
}

// TestBuiltinEscalationIdempotencyKeyDeclared: createBead persists an
// idempotency_key into fields (beads_idempotency.go) before validating them,
// so a type that does not declare it 400s every create that carries one. The
// Lands API's createEscalation always passes the tenant's Idempotency-Key
// (the contract: every POST accepts one); the D.11 conformance suite against
// a real daemon found it refused (gd-oQdHmmwZFW).
func TestBuiltinEscalationIdempotencyKeyDeclared(t *testing.T) {
	var tc model.TypeConfig
	if err := json.Unmarshal(builtinConfigs["type:escalation"].Value, &tc); err != nil {
		t.Fatalf("unmarshal type:escalation: %v", err)
	}
	if err := model.ValidateTypeFields([]byte(`{"operation_type":"x","operation_spec":{},"idempotency_key":"k-1"}`), &tc); err != nil {
		t.Errorf("type:escalation refuses idempotency_key: %v", err)
	}
}

// TestBuiltinAuditActionCoversScopedActions guards the class of bug where a
// write check is made with an action the type:audit enum does not list, so
// auditDenial's record of the deny 400s and is only logged (recordAuditEntry
// is best-effort): a land member's denied comment on another land's bead went
// unrecorded this way (gd-9UMen1DS2M), as did a denied hook-emit. Every
// reviewed scoped action must validate as an audit action.
func TestBuiltinAuditActionCoversScopedActions(t *testing.T) {
	c, ok := builtinConfigs["type:audit"]
	if !ok {
		t.Fatal("builtinConfigs missing \"type:audit\"")
	}
	var tc model.TypeConfig
	if err := json.Unmarshal(c.Value, &tc); err != nil {
		t.Fatalf("type:audit: unmarshal config value: %v", err)
	}
	tc.TypeName = "audit"

	for _, action := range reviewedActions {
		fields, err := json.Marshal(map[string]any{"actor": "member@acorn.example", "action": action, "outcome": "deny"})
		if err != nil {
			t.Fatal(err)
		}
		if err := model.ValidateTypeFields(fields, &tc); err != nil {
			t.Errorf("audit of a denied %q must validate on type:audit, got: %v", action, err)
		}
	}
}

// TestBuiltinPlacementFieldsDeclared guards the explicit-placement fields
// (local-agents spec §7): the slack bridge stamps placement/runner_id on a
// /spawn --runner task, and an undeclared field 400s that write. Every type gb
// ready lists as leaf work declares them, and placement accepts only "local".
func TestBuiltinPlacementFieldsDeclared(t *testing.T) {
	want := map[string]model.FieldType{
		"placement":          model.FieldTypeEnum,
		"runner_tag":         model.FieldTypeString,
		"runner_id":          model.FieldTypeString,
		"opened_by_operator": model.FieldTypeString,
	}
	for _, typeName := range []string{"type:task", "type:feature", "type:chore", "type:bug"} {
		var tc model.TypeConfig
		if err := json.Unmarshal(builtinConfigs[typeName].Value, &tc); err != nil {
			t.Fatalf("%s: unmarshal config value: %v", typeName, err)
		}
		have := make(map[string]model.FieldDef, len(tc.Fields))
		for _, f := range tc.Fields {
			have[f.Name] = f
		}
		for name, wantType := range want {
			fd, ok := have[name]
			if !ok {
				t.Errorf("%s schema missing field %q", typeName, name)
				continue
			}
			if fd.Type != wantType {
				t.Errorf("%s field %q type=%q, want %q", typeName, name, fd.Type, wantType)
			}
		}
		if got := have["placement"].Values; !slices.Equal(got, []string{"local"}) {
			t.Errorf("%s placement values = %v, want [local]", typeName, got)
		}
	}
}

// TestBuiltinHerderFieldsDeclared: the scheduled herder stamps its verdict on
// each wish head (type:task) it judges. type:task rejects unknown fields, so
// while these were undeclared every stamp 400'd (gd-K1CMS3JmCi).
func TestBuiltinHerderFieldsDeclared(t *testing.T) {
	var tc model.TypeConfig
	if err := json.Unmarshal(builtinConfigs["type:task"].Value, &tc); err != nil {
		t.Fatalf("type:task: %v", err)
	}
	have := map[string]bool{}
	for _, f := range tc.Fields {
		have[f.Name] = true
	}
	for _, want := range []string{"herder_verdict", "herder_checked_at", "close_reason", "herder_woke_at", "herder_decision"} {
		if !have[want] {
			t.Errorf("type:task schema missing field %q (the herder stamps it on wish heads)", want)
		}
	}
}

// TestBuiltinEvidencePerfFieldsValidate guards gd-t2nAuh7y2X: the
// commons-ux-acceptance skill (step 5) files a ux evidence bead with the perf
// budget result in perf_pass / perf_of / perf_fail / perf_ungraded (rubric §4),
// and an undeclared field 400s the whole create ("perf_pass: unknown field").
// The fields are exactly the skill's example.
func TestBuiltinEvidencePerfFieldsValidate(t *testing.T) {
	var tc model.TypeConfig
	if err := json.Unmarshal(builtinConfigs["type:evidence"].Value, &tc); err != nil {
		t.Fatalf("type:evidence: %v", err)
	}
	tc.TypeName = "evidence"

	valid := json.RawMessage(`{"kind":"ux","step":"gd-step","subject":"cursorphase","score":5,"of":5,"percent":100,` +
		`"perf_pass":7,"perf_of":10,"perf_fail":"B1,B4,B5","perf_ungraded":"B7"}`)
	if err := model.ValidateTypeFields(valid, &tc); err != nil {
		t.Errorf("the skill's ux evidence fields must validate on type:evidence, got: %v", err)
	}

	negative := json.RawMessage(`{"kind":"ux","step":"gd-step","perf_pass":-1}`)
	if err := model.ValidateTypeFields(negative, &tc); err == nil {
		t.Error("perf_pass=-1 validated; want the min 0 bound to refuse it")
	}
}
