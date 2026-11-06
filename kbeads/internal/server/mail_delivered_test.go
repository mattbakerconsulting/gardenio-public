package server

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"gardenio/kbeads/internal/model"
)

// The recipient pod's mail subscriber (gardenio controller/internal/mailsub
// stampDelivered) stamps delivered_at on mail once coop takes it, through
// beadsapi.Client.UpdateBeadFields: a PATCH /v1/beads/{id} carrying only
// {"fields":{"delivered_at":"<RFC3339>"}}. The bridge's watchMailDelivery
// respawns the agent when interrupt mail is still open AND unstamped two
// minutes after it was sent. type:mail did not declare the field, so every
// stamp 400'd "delivered_at: unknown field", the subscriber only logged it, and
// the bridge killed and resumed healthy thread agents after every @mention
// (PR #1424 send-back). The controller tests assert the stamp against a fake
// daemon that accepts any field, so these run it through the real update path:
// resolveTypeConfig and model.ValidateTypeFieldsForUpdate.

// mailprotoSrcRelPath declares mailproto.FieldDeliveredAt, reached across the
// module boundary the same way reopen_fields_test.go reaches beadsapi.
const mailprotoSrcRelPath = "../../../gardenio/controller/internal/mailproto/mailproto.go"

const wireMailDeliveredAt = "delivered_at"

// nudgeMailFixture stores interrupt mail the slack bridge sent to crew-recipient
// with no delivered_at, and returns the recipient's own per-agent token: the
// identity mailsub stamps with in a pod.
func nudgeMailFixture(t *testing.T) (*BeadsServer, *mockStore, *model.Bead) {
	t.Helper()
	s, ms, _ := newTestServer()
	m := &model.Bead{
		ID:        "gd-nudge1",
		Type:      "mail",
		Kind:      model.KindData,
		Title:     "@mention in thread",
		CreatedBy: "slack-bridge",
		Assignee:  "crew-recipient",
		Status:    model.StatusOpen,
		Fields:    json.RawMessage(`{"source_agent":"slack-bridge"}`),
	}
	ms.beadsMu.Lock()
	ms.beads[m.ID] = m
	ms.labels[m.ID] = []string{"from:slack-bridge", "delivery:interrupt"}
	ms.beadsMu.Unlock()
	return s, ms, m
}

func stampFields(t *testing.T) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(map[string]any{wireMailDeliveredAt: "2026-10-29T06:30:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestMailDeliveredAt_DeclaredOnBuiltin(t *testing.T) {
	f := fieldByName(parseBuiltinSchema(t, "mail"), wireMailDeliveredAt)
	if f == nil {
		t.Fatalf("type:mail does not declare %q: every mailsub stamp 400s \"unknown field\" and the bridge respawns live agents", wireMailDeliveredAt)
	}
	// stampDelivered sends time.Now().UTC().Format(time.RFC3339), a JSON string.
	if f.Type != model.FieldTypeString {
		t.Errorf("type:mail field %q type=%q, want %q", wireMailDeliveredAt, f.Type, model.FieldTypeString)
	}
}

// The exact stamp, sent with the recipient's own token, must land, and
// ValidateTypeFieldsForUpdate must accept it with no prior delivered_at to
// grandfather.
func TestMailDeliveredAt_RecipientStampPassesUpdateValidation(t *testing.T) {
	s, ms, m := nudgeMailFixture(t)
	ctx := ctxWithIdentity(AuthIdentity{Type: "fleet", Subject: m.Assignee}, "agents")

	tc, err := s.resolveTypeConfig(ctx, "mail")
	if err != nil || tc == nil {
		t.Fatalf("resolveTypeConfig(mail) = %v, %v", tc, err)
	}
	var prior map[string]any
	if err := json.Unmarshal(m.Fields, &prior); err != nil {
		t.Fatal(err)
	}
	if err := model.ValidateTypeFieldsForUpdate(
		json.RawMessage(`{"source_agent":"slack-bridge","delivered_at":"2026-10-29T06:30:00Z"}`), tc, prior,
	); err != nil {
		t.Fatalf("ValidateTypeFieldsForUpdate rejected the delivered_at stamp: %v", err)
	}

	if _, err := s.updateBead(ctx, m.ID, updateBeadInput{Fields: stampFields(t)}); err != nil {
		t.Fatalf("recipient's delivered_at stamp refused: %v", err)
	}
	ms.beadsMu.Lock()
	got := ms.beads[m.ID]
	ms.beadsMu.Unlock()
	var stored map[string]any
	if err := json.Unmarshal(got.Fields, &stored); err != nil {
		t.Fatal(err)
	}
	if stored[wireMailDeliveredAt] != "2026-10-29T06:30:00Z" {
		t.Errorf("delivered_at = %#v, want the stamp", stored[wireMailDeliveredAt])
	}
	if stored["source_agent"] != "slack-bridge" {
		t.Errorf("pre-existing source_agent lost by the merge: %#v", stored["source_agent"])
	}
	if got.Status != model.StatusOpen {
		t.Errorf("stamp changed status to %q; mail stays open until gb mail read", got.Status)
	}
}

// Another crew's token is neither creator nor assignee, so the ownership gate
// still refuses it: declaring the field does not let one agent mark another's
// mail delivered and suppress its respawn.
func TestMailDeliveredAt_OtherAgentStampRefused(t *testing.T) {
	s, ms, m := nudgeMailFixture(t)
	other := ctxWithIdentity(AuthIdentity{Type: "fleet", Subject: "crew-other"}, "agents")
	_, err := s.updateBead(other, m.ID, updateBeadInput{Fields: stampFields(t)})
	var denial *AuthzDenial
	if !errors.As(err, &denial) {
		t.Fatalf("another agent's stamp must be refused with AuthzDenial, got %v", err)
	}
	ms.beadsMu.Lock()
	fields := string(ms.beads[m.ID].Fields)
	ms.beadsMu.Unlock()
	if regexp.MustCompile(wireMailDeliveredAt).MatchString(fields) {
		t.Errorf("refused stamp was stored: %s", fields)
	}
}

// The live daemon persists a type:mail row without delivered_at. The builtin
// merge in resolveTypeConfig must add it, so a kbeads upgrade alone fixes the
// running fleet with no manual kd config set.
func TestMailDeliveredAt_StaleStoreConfigRepairedByBuiltinMerge(t *testing.T) {
	s, ms, m := nudgeMailFixture(t)
	ms.configs["type:mail"] = &model.Config{Key: "type:mail", Value: json.RawMessage(`{
		"kind": "data",
		"fields": [
			{"name": "source_agent",     "type": "string"},
			{"name": "decision_bead_id", "type": "string"}
		]
	}`)}
	ctx := ctxWithIdentity(AuthIdentity{Type: "fleet", Subject: m.Assignee}, "agents")
	if _, err := s.updateBead(ctx, m.ID, updateBeadInput{Fields: stampFields(t)}); err != nil {
		t.Fatalf("stamp refused against a pre-upgrade store config: %v", err)
	}
}

// TestMailDeliveredAt_MatchesMailprotoConstant fails if the controller renames
// mailproto.FieldDeliveredAt without the schema following.
func TestMailDeliveredAt_MatchesMailprotoConstant(t *testing.T) {
	abs, err := filepath.Abs(mailprotoSrcRelPath)
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(abs)
	if os.IsNotExist(err) {
		t.Skipf("gardenio sibling source %s not checked out", mailprotoSrcRelPath)
	}
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`FieldDeliveredAt\s*=\s*"([^"]*)"`).FindSubmatch(src)
	if match == nil {
		t.Fatalf("%s no longer declares FieldDeliveredAt; update this guard and the type:mail schema together", mailprotoSrcRelPath)
	}
	if got := string(match[1]); got != wireMailDeliveredAt {
		t.Errorf("mailproto.FieldDeliveredAt = %q, want %q: the type:mail schema no longer declares the wire name", got, wireMailDeliveredAt)
	}
}

// A laptop agent (gb agent start --local) has no per-agent token: mailsub
// stamps on its runner token or its operator's sign-in, neither of which is
// the mail's assignee. Those principals may set delivered_at, once, on mail to
// the laptop agent they run, and nothing else (mail_delivery_stamp.go,
// gd-GTMwKmHM7o).

const laptopOwner = "oidc:owner@example.com"

// laptopMailFixture stores interrupt mail to laptop-agent and its open agent
// bead, created by laptopOwner on runner gd-runner1.
func laptopMailFixture(t *testing.T, agentFields string) (*BeadsServer, *mockStore, *model.Bead) {
	t.Helper()
	s, ms, _ := newTestServer()
	m := &model.Bead{
		ID:        "gd-lmail1",
		Type:      "mail",
		Kind:      model.KindData,
		Title:     "@mention in thread",
		CreatedBy: "slack-bridge",
		Assignee:  "laptop-agent",
		Status:    model.StatusOpen,
		Fields:    json.RawMessage(`{"source_agent":"slack-bridge"}`),
	}
	agent := &model.Bead{
		ID:         "gd-lagent1",
		Type:       "agent",
		Kind:       model.KindConfig,
		Title:      "laptop-agent",
		CreatedBy:  laptopOwner,
		OperatedBy: laptopOwner,
		Assignee:   "laptop-agent",
		Status:     model.StatusOpen,
		Fields:     json.RawMessage(agentFields),
	}
	ms.beadsMu.Lock()
	ms.beads[m.ID] = m
	ms.labels[m.ID] = []string{"from:slack-bridge", "delivery:interrupt"}
	ms.beads[agent.ID] = agent
	ms.beadsMu.Unlock()
	return s, ms, m
}

const laptopAgentFields = `{"agent":"laptop-agent","execution_target":"laptop","runtime_id":"gd-runner1"}`

func runnerCallerCtx(runnerID, owner string) context.Context {
	ctx := withAuthMethod(context.Background(), authMethodRunner)
	ctx = WithAuthIdentity(ctx, AuthIdentity{Type: runnerIdentityType, Subject: owner})
	return WithRunnerID(ctx, runnerID)
}

func oidcCallerCtx(subject string) context.Context {
	ctx := withAuthMethod(context.Background(), authMethodOIDC)
	return WithAuthIdentity(ctx, AuthIdentity{Type: "oidc", Subject: subject})
}

func storedDeliveredAt(t *testing.T, ms *mockStore, id string) any {
	t.Helper()
	ms.beadsMu.Lock()
	raw := ms.beads[id].Fields
	ms.beadsMu.Unlock()
	var f map[string]any
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	return f[wireMailDeliveredAt]
}

func TestMailDeliveredAt_LaptopPrincipalStampAdmitted(t *testing.T) {
	for name, ctx := range map[string]context.Context{
		"its runner":        runnerCallerCtx("gd-runner1", laptopOwner),
		"its operator OIDC": oidcCallerCtx(laptopOwner),
	} {
		t.Run(name, func(t *testing.T) {
			s, ms, m := laptopMailFixture(t, laptopAgentFields)
			if _, err := s.updateBead(ctx, m.ID, updateBeadInput{Fields: stampFields(t)}); err != nil {
				t.Fatalf("laptop agent's delivered_at stamp refused: %v", err)
			}
			if got := storedDeliveredAt(t, ms, m.ID); got != "2026-10-29T06:30:00Z" {
				t.Errorf("delivered_at = %#v, want the stamp", got)
			}
		})
	}
}

func TestMailDeliveredAt_LaptopStampRefusedOutsideItsScope(t *testing.T) {
	other := func(t *testing.T) json.RawMessage {
		return json.RawMessage(`{"source_agent":"forged"}`)
	}
	cases := []struct {
		name        string
		agentFields string
		ctx         context.Context
		fields      func(*testing.T) json.RawMessage
		prestamp    bool
	}{
		{"another runner of the same owner", laptopAgentFields, runnerCallerCtx("gd-runner2", laptopOwner), stampFields, false},
		{"another operator", laptopAgentFields, oidcCallerCtx("oidc:someone@example.com"), stampFields, false},
		{"a field other than delivered_at", laptopAgentFields, runnerCallerCtx("gd-runner1", laptopOwner), other, false},
		{"re-dating an existing stamp", laptopAgentFields, runnerCallerCtx("gd-runner1", laptopOwner), stampFields, true},
		{"a pod agent's mail", `{"agent":"laptop-agent","runtime_id":"gd-runner1"}`, runnerCallerCtx("gd-runner1", laptopOwner), stampFields, false},
		{"a pod agent's mail, operator", `{"agent":"laptop-agent"}`, oidcCallerCtx(laptopOwner), stampFields, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, ms, m := laptopMailFixture(t, tc.agentFields)
			if tc.prestamp {
				ms.beadsMu.Lock()
				ms.beads[m.ID].Fields = json.RawMessage(`{"source_agent":"slack-bridge","delivered_at":"2026-10-29T06:00:00Z"}`)
				ms.beadsMu.Unlock()
			}
			_, err := s.updateBead(tc.ctx, m.ID, updateBeadInput{Fields: tc.fields(t)})
			var denial *AuthzDenial
			if !errors.As(err, &denial) {
				t.Fatalf("want AuthzDenial, got %v", err)
			}
		})
	}
}
