package main

import (
	"encoding/json"
	"testing"

	"gardenio/controller/internal/claudeconfig"
)

func TestConfigCategoryNames_ReturnsAll(t *testing.T) {
	names := ConfigCategoryNames()
	if len(names) != len(configCategories) {
		t.Fatalf("expected %d names, got %d", len(configCategories), len(names))
	}

	expected := map[string]bool{
		"claude-settings":     true,
		"claude-hooks":        true,
		"claude-mcp":          true,
		"type":                true,
		"context":             true,
		"view":                true,
		"claude-instructions": true,
		"wrapup-config":        true,
		"human-survey-config":  true,
		"nudge-prompts":        true,
		"bridge-notifications": true,
		"idle-ttl":             true,
		"prerun":               true,
		"codex-config":         true,
		"codex-instructions":   true,
		"gemini-config":        true,
		"gemini-instructions":  true,
	}
	for _, name := range names {
		if !expected[name] {
			t.Errorf("unexpected category name: %s", name)
		}
	}
}

func TestLookupCategory(t *testing.T) {
	cat := claudeconfig.LookupCategory("claude-settings")
	if cat == nil {
		t.Fatal("expected claude-settings category")
	}
	if cat.Strategy != MergeOverride {
		t.Errorf("expected MergeOverride for claude-settings, got %d", cat.Strategy)
	}

	cat = claudeconfig.LookupCategory("claude-hooks")
	if cat == nil {
		t.Fatal("expected claude-hooks category")
	}
	if cat.Strategy != MergeConcat {
		t.Errorf("expected MergeConcat for claude-hooks, got %d", cat.Strategy)
	}

	cat = claudeconfig.LookupCategory("nonexistent")
	if cat != nil {
		t.Errorf("expected nil for nonexistent category, got %v", cat)
	}
}

func TestMergeLayers_Override(t *testing.T) {
	layers := []json.RawMessage{
		json.RawMessage(`{"model":"sonnet","permissions":{"allow":["Bash(*)"]}}`),
		json.RawMessage(`{"model":"opus"}`),
	}

	result := claudeconfig.MergeLayers(MergeOverride, layers)

	if result["model"] != "opus" {
		t.Errorf("expected model=opus (override), got %v", result["model"])
	}
	if result["permissions"] == nil {
		t.Error("expected permissions from first layer to survive")
	}
}

func TestMergeLayers_Concat(t *testing.T) {
	layers := []json.RawMessage{
		json.RawMessage(`{"hooks":{"SessionStart":[{"matcher":"","hooks":[{"type":"command","command":"gb hook prime"}]}]}}`),
		json.RawMessage(`{"hooks":{"SessionStart":[{"matcher":"","hooks":[{"type":"command","command":"gb hook check-mail"}]}]}}`),
	}

	result := claudeconfig.MergeLayers(MergeConcat, layers)

	hooks, ok := result["hooks"].(map[string]any)
	if !ok {
		t.Fatal("expected hooks key")
	}
	sessionStart, ok := hooks["SessionStart"].([]any)
	if !ok {
		t.Fatal("expected SessionStart array")
	}
	if len(sessionStart) != 2 {
		t.Errorf("expected 2 SessionStart hooks (concatenated), got %d", len(sessionStart))
	}
}

func TestMergeLayers_ConcatNewHookType(t *testing.T) {
	layers := []json.RawMessage{
		json.RawMessage(`{"hooks":{"SessionStart":[{"matcher":"","hooks":[{"type":"command","command":"a"}]}]}}`),
		json.RawMessage(`{"hooks":{"Stop":[{"matcher":"","hooks":[{"type":"command","command":"b"}]}]}}`),
	}

	result := claudeconfig.MergeLayers(MergeConcat, layers)

	hooks := result["hooks"].(map[string]any)
	if hooks["SessionStart"] == nil {
		t.Error("expected SessionStart from first layer")
	}
	if hooks["Stop"] == nil {
		t.Error("expected Stop from second layer")
	}
}

func TestMergeLayers_ConcatList(t *testing.T) {
	// Array-valued keys concatenate across layers (global then role); a scalar
	// key uses override semantics.
	layers := []json.RawMessage{
		json.RawMessage(`{"prerun":[{"id":"a","cmd":"gb ready"}],"scalar":"g"}`),
		json.RawMessage(`{"prerun":[{"id":"b","cmd":"gb news"}],"scalar":"r"}`),
	}

	result := claudeconfig.MergeLayers(MergeConcatList, layers)

	list, ok := result["prerun"].([]any)
	if !ok {
		t.Fatalf("expected prerun array, got %T", result["prerun"])
	}
	if len(list) != 2 {
		t.Errorf("expected 2 concatenated prerun entries, got %d", len(list))
	}
	// Global layer appended first, role layer second.
	first := list[0].(map[string]any)
	if first["id"] != "a" {
		t.Errorf("expected global entry (id=a) first, got %v", first["id"])
	}
	if result["scalar"] != "r" {
		t.Errorf("expected scalar override to role layer (r), got %v", result["scalar"])
	}
}

func TestMergeLayers_EmptyLayers(t *testing.T) {
	result := claudeconfig.MergeLayers(MergeOverride, nil)
	if len(result) != 0 {
		t.Errorf("expected empty result, got %v", result)
	}

	result = claudeconfig.MergeLayers(MergeConcat, nil)
	if len(result) != 0 {
		t.Errorf("expected empty result, got %v", result)
	}
}

func TestMergeLayers_InvalidJSON(t *testing.T) {
	layers := []json.RawMessage{
		json.RawMessage(`not valid json`),
		json.RawMessage(`{"model":"opus"}`),
	}

	result := claudeconfig.MergeLayers(MergeOverride, layers)
	if result["model"] != "opus" {
		t.Errorf("expected valid layer to be applied, got %v", result["model"])
	}
}

func TestLookupCategory_ClaudeInstructions(t *testing.T) {
	cat := claudeconfig.LookupCategory("claude-instructions")
	if cat == nil {
		t.Fatal("expected claude-instructions category")
	}
	if cat.Strategy != MergeOverride {
		t.Errorf("expected MergeOverride for claude-instructions, got %d", cat.Strategy)
	}
	if cat.Description == "" {
		t.Error("expected non-empty description for claude-instructions")
	}
}

func TestAllCategoriesHaveDescriptions(t *testing.T) {
	for _, cat := range configCategories {
		if cat.Description == "" {
			t.Errorf("category %q has no description", cat.Name)
		}
	}
}
