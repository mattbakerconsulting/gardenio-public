package main

import (
	"bytes"
	"strings"
	"testing"
)

// The TestLintTriggerFields_* tests guard ka-0000000736: a type=trigger bead
// carrying both the triggers: array and legacy single-source fields warns,
// since the engine silently ignores the legacy ones.
func TestLintTriggerFields_NotTrigger(t *testing.T) {
	var buf bytes.Buffer
	n := lintTriggerFields(&buf, "schedule", []string{
		"triggers=[{\"kind\":\"cron\",\"cron\":\"*/5 * * * *\"}]",
		"cron=*/5 * * * *",
	})
	if n != 0 || buf.Len() != 0 {
		t.Fatalf("non-trigger should not warn, got n=%d output=%q", n, buf.String())
	}
}

func TestLintTriggerFields_TriggersOnly_NoWarning(t *testing.T) {
	var buf bytes.Buffer
	n := lintTriggerFields(&buf, "trigger", []string{
		"triggers=[{\"kind\":\"cron\",\"cron\":\"*/5 * * * *\"}]",
		"action=spawn_agent",
		"role=crew",
		"project=gardenio",
		"prompt=do stuff",
	})
	if n != 0 {
		t.Fatalf("triggers-only should not warn, got n=%d output:\n%s", n, buf.String())
	}
}

func TestLintTriggerFields_LegacyOnly_NoWarning(t *testing.T) {
	var buf bytes.Buffer
	n := lintTriggerFields(&buf, "trigger", []string{
		"source=jira",
		"jql=project = FOO",
		"action=spawn_agent",
	})
	if n != 0 {
		t.Fatalf("legacy-only should not warn (only the dual shape is advisory), got n=%d output:\n%s",
			n, buf.String())
	}
}

func TestLintTriggerFields_BothShapes_Warns(t *testing.T) {
	var buf bytes.Buffer
	n := lintTriggerFields(&buf, "trigger", []string{
		"triggers=[{\"kind\":\"cron\",\"cron\":\"*/5 * * * *\"}]",
		"cron=*/5 * * * *",
		"source=schedule",
		"action=spawn_agent",
	})
	if n != 1 {
		t.Fatalf("dual shape should emit exactly 1 warning, got %d:\n%s", n, buf.String())
	}
	out := buf.String()
	if !strings.HasPrefix(out, "warning:") {
		t.Errorf("warning must be prefixed 'warning:', got:\n%s", out)
	}
	if !strings.Contains(out, "cron") || !strings.Contains(out, "source") {
		t.Errorf("warning should list the offending legacy fields, got:\n%s", out)
	}
	if !strings.Contains(out, "triggers:") {
		t.Errorf("warning should reference the triggers: array, got:\n%s", out)
	}
}

func TestLintTriggerFields_EmptyLegacyValuesIgnored(t *testing.T) {
	var buf bytes.Buffer
	n := lintTriggerFields(&buf, "trigger", []string{
		"triggers=[{\"kind\":\"cron\",\"cron\":\"*/5 * * * *\"}]",
		"cron=",
		"source=",
		"jql=",
	})
	if n != 0 {
		t.Fatalf("empty legacy values should not count as set, got %d:\n%s", n, buf.String())
	}
}

func TestLintTriggerFields_EmptyTriggersValueIgnored(t *testing.T) {
	var buf bytes.Buffer
	n := lintTriggerFields(&buf, "trigger", []string{
		"triggers=",
		"cron=*/5 * * * *",
		"source=schedule",
	})
	if n != 0 {
		t.Fatalf("empty triggers value means no dual shape, got %d:\n%s", n, buf.String())
	}
}

func TestLintTriggerFields_AllLegacyFieldsDetected(t *testing.T) {
	// Sanity: every documented legacy single-source field should trigger
	// the dual-shape warning when paired with a non-empty triggers[].
	for _, field := range legacyTriggerSourceFields {
		t.Run(field, func(t *testing.T) {
			var buf bytes.Buffer
			n := lintTriggerFields(&buf, "trigger", []string{
				"triggers=[{\"kind\":\"cron\",\"cron\":\"*/5 * * * *\"}]",
				field + "=value",
			})
			if n != 1 {
				t.Fatalf("legacy field %q should warn alongside triggers[], got %d:\n%s",
					field, n, buf.String())
			}
			if !strings.Contains(buf.String(), field) {
				t.Errorf("warning should name the legacy field %q, got:\n%s",
					field, buf.String())
			}
		})
	}
}
