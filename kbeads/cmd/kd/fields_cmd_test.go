package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/model"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// triggerLikeSchema returns a small type config resembling type:trigger,
// including the slack_channel field whose absence-of-discoverability motivated
// ka-0000002423, plus an enum and a bounded integer to exercise the VALUES
// column.
func triggerLikeSchema(t *testing.T) *model.Config {
	t.Helper()
	lo, hi := 0, 10
	tc := model.TypeConfig{
		Kind: model.KindData,
		Fields: []model.FieldDef{
			{Name: "prompt", Type: model.FieldTypeString, Required: true},
			{Name: "source", Type: model.FieldTypeEnum, Values: []string{"jira", "gitlab", "cron"}},
			{Name: "max_retries", Type: model.FieldTypeInteger, Min: &lo, Max: &hi},
			{Name: "slack_channel", Type: model.FieldTypeString},
		},
	}
	raw, err := json.Marshal(tc)
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	return &model.Config{Key: "type:trigger", Value: raw}
}

func TestFields_TableOutput(t *testing.T) {
	mc := newMockClient()
	mc.ConfigsByKey = map[string]*model.Config{"type:trigger": triggerLikeSchema(t)}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	origJSON := jsonOutput
	jsonOutput = false
	defer func() { jsonOutput = origJSON }()

	out := captureStdout(t, func() {
		if err := fieldsCmd.RunE(fieldsCmd, []string{"trigger"}); err != nil {
			t.Fatalf("fieldsCmd: %v", err)
		}
	})

	// The core manual-verify from the task: `kd fields trigger | grep slack_channel`.
	if !strings.Contains(out, "slack_channel") {
		t.Errorf("table output must name slack_channel, got:\n%s", out)
	}
	// Header + required marker + enum values + integer range all render.
	for _, want := range []string{
		"NAME", "TYPE", "REQUIRED", "VALUES", "prompt", "yes",
		"jira | gitlab | cron", "0..10",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected table output to contain %q, got:\n%s", want, out)
		}
	}
	// Requested the right key.
	if len(mc.GetConfigCalls) != 1 || mc.GetConfigCalls[0] != "type:trigger" {
		t.Errorf("expected GetConfig(type:trigger), got %v", mc.GetConfigCalls)
	}
}

func TestFields_JSONOutput(t *testing.T) {
	mc := newMockClient()
	mc.ConfigsByKey = map[string]*model.Config{"type:trigger": triggerLikeSchema(t)}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	origJSON := jsonOutput
	jsonOutput = true
	defer func() { jsonOutput = origJSON }()

	out := captureStdout(t, func() {
		if err := fieldsCmd.RunE(fieldsCmd, []string{"trigger"}); err != nil {
			t.Fatalf("fieldsCmd --json: %v", err)
		}
	})

	var got struct {
		Type   string           `json:"type"`
		Kind   string           `json:"kind"`
		Fields []model.FieldDef `json:"fields"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	if got.Type != "trigger" {
		t.Errorf("type = %q, want trigger", got.Type)
	}
	if got.Kind != string(model.KindData) {
		t.Errorf("kind = %q, want %q", got.Kind, model.KindData)
	}
	if len(got.Fields) != 4 {
		t.Fatalf("expected 4 fields, got %d", len(got.Fields))
	}
	// Declaration order preserved (not sorted): prompt is first.
	if got.Fields[0].Name != "prompt" || !got.Fields[0].Required {
		t.Errorf("first field = %+v, want prompt/required", got.Fields[0])
	}
	// The slack_channel field is machine-discoverable in the JSON.
	var haveSlack bool
	for _, f := range got.Fields {
		if f.Name == "slack_channel" {
			haveSlack = true
		}
	}
	if !haveSlack {
		t.Errorf("JSON fields missing slack_channel: %+v", got.Fields)
	}
}

// TestFields_AliasResolves proves `kd fields template` resolves the deprecated
// alias to type:formula rather than 404ing on a key that only exists post-alias.
func TestFields_AliasResolves(t *testing.T) {
	mc := newMockClient()
	raw, _ := json.Marshal(model.TypeConfig{Kind: model.KindData, Fields: []model.FieldDef{{Name: "steps", Type: model.FieldTypeJSON}}})
	mc.ConfigsByKey = map[string]*model.Config{"type:formula": {Key: "type:formula", Value: raw}}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	origJSON := jsonOutput
	jsonOutput = false
	defer func() { jsonOutput = origJSON }()

	out := captureStdout(t, func() {
		if err := fieldsCmd.RunE(fieldsCmd, []string{"template"}); err != nil {
			t.Fatalf("fieldsCmd template: %v", err)
		}
	})
	if !strings.Contains(out, "formula") {
		t.Errorf("expected alias template→formula in output, got:\n%s", out)
	}
	if len(mc.GetConfigCalls) != 1 || mc.GetConfigCalls[0] != "type:formula" {
		t.Errorf("expected GetConfig(type:formula) after alias resolution, got %v", mc.GetConfigCalls)
	}
}

// TestFields_UnknownType_HTTP404 covers the default (HTTP) transport: an unknown
// type surfaces a *client.APIError{404}, and the command must translate it into
// a helpful "available types" error rather than leaking the raw HTTP error.
func TestFields_UnknownType_HTTP404(t *testing.T) {
	mc := newMockClient()
	mc.GetConfigErr = &client.APIError{StatusCode: http.StatusNotFound, Message: "config not found"}
	mc.ListConfigsByNamespace = map[string][]*model.Config{
		"type": {{Key: "type:customthing"}},
	}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	err := fieldsCmd.RunE(fieldsCmd, []string{"nonexistent"})
	if err == nil {
		t.Fatal("expected error for unknown type, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "unknown bead type") || !strings.Contains(msg, "nonexistent") {
		t.Errorf("error should name the unknown type, got %q", msg)
	}
	if !strings.Contains(msg, "available types:") {
		t.Errorf("error should list available types, got %q", msg)
	}
	// The live registry widened the known set with the custom type.
	if !strings.Contains(msg, "customthing") {
		t.Errorf("available types should include the live custom type, got %q", msg)
	}
	// A builtin floor type is present too (proves localKnownTypes was consulted).
	if !strings.Contains(msg, "task") {
		t.Errorf("available types should include builtin 'task', got %q", msg)
	}
}

// TestFields_UnknownType_GRPCNotFound covers the gRPC transport: a
// codes.NotFound status must be recognized as "unknown type" too.
func TestFields_UnknownType_GRPCNotFound(t *testing.T) {
	mc := newMockClient()
	mc.GetConfigErr = status.Error(codes.NotFound, "config not found")

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	err := fieldsCmd.RunE(fieldsCmd, []string{"nope"})
	if err == nil {
		t.Fatal("expected error for unknown type over gRPC, got nil")
	}
	if !strings.Contains(err.Error(), "unknown bead type") {
		t.Errorf("gRPC NotFound should map to unknown-type error, got %q", err.Error())
	}
}

// TestFields_TransportErrorNotSwallowed proves a non-404 transport error is
// surfaced as a fetch error, NOT misreported as an unknown type.
func TestFields_TransportErrorNotSwallowed(t *testing.T) {
	mc := newMockClient()
	mc.GetConfigErr = &client.APIError{StatusCode: http.StatusInternalServerError, Message: "boom"}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	err := fieldsCmd.RunE(fieldsCmd, []string{"trigger"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if strings.Contains(err.Error(), "unknown bead type") {
		t.Errorf("a 500 must not be reported as unknown type, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "fetching schema") {
		t.Errorf("expected a fetch error, got %q", err.Error())
	}
}

func TestFieldValuesHint(t *testing.T) {
	lo, hi := 1, 5
	cases := []struct {
		name string
		f    model.FieldDef
		want string
	}{
		{"enum", model.FieldDef{Type: model.FieldTypeEnum, Values: []string{"a", "b"}}, "a | b"},
		{"range", model.FieldDef{Type: model.FieldTypeInteger, Min: &lo, Max: &hi}, "1..5"},
		{"min only", model.FieldDef{Type: model.FieldTypeInteger, Min: &lo}, ">= 1"},
		{"max only", model.FieldDef{Type: model.FieldTypeInteger, Max: &hi}, "<= 5"},
		{"plain string", model.FieldDef{Type: model.FieldTypeString}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := fieldValuesHint(tc.f); got != tc.want {
				t.Errorf("fieldValuesHint(%+v) = %q, want %q", tc.f, got, tc.want)
			}
		})
	}
}
