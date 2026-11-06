package tests

import (
	"strings"
	"testing"
)

// Custom gardenio-assistant-app chat-box WRITE gate (C8, ka-0000005589, epic
// ka-0000002011 — OSS Grafana chat frontend, Path C).
//
// C7 (ka-0000001262) gave the embedded mcp-grafana server dashboard/folder write
// power at the server layer. C8 extends the BRANDED plugin with a config-gated
// write surface: the plugin reads jsonData.enableDashboardWrites and, when true,
// offers the model the vetted dashboard/folder write tools (update_dashboard,
// create_folder) — and ONLY those. This chart layer provisions that flag onto
// the gardenio-assistant-app app, default FALSE.
//
// The assistant app hard-depends on grafana-llm-app, so its provisioning entry
// renders only when BOTH grafana.llm.enabled AND grafana.assistant.enabled.
//
// These tests pin: (1) default-off/additive (no assistant entry, no jsonData),
// (2) enabled + writes-on renders the entry with enableDashboardWrites: true,
// (3) enabled + writes-off (the assistant.enabled default) renders the entry
// with enableDashboardWrites: false — the read-only posture stays the default.

// assistantWriteFlags is the realistic enabled config: the chat plugin
// hard-depends on grafana-llm-app, so grafana.assistant.enabled is only
// meaningful with grafana.llm.enabled (+ litellm for the in-cluster transport).
var assistantWriteFlags = []string{
	"grafana.enabled=true",
	"grafana.llm.enabled=true",
	"litellm.enabled=true",
	"grafana.assistant.enabled=true",
}

// TestGrafanaAssistantWrites_DisabledByDefault — with grafana + llm on but the
// assistant untouched, the provisioning configmap renders NO assistant app entry
// and NO write flag. Guards the additive contract: an env that does not opt into
// the assistant renders byte-identically to the C7 grafana-llm-app-only state.
func TestGrafanaAssistantWrites_DisabledByDefault(t *testing.T) {
	t.Parallel()
	cm := helmTemplate(t, "templates/grafana/configmap.yaml",
		"grafana.enabled=true",
		"grafana.llm.enabled=true",
		"litellm.enabled=true",
	)
	for _, needle := range []string{
		"gardenio-assistant-app",
		"enableDashboardWrites",
	} {
		if strings.Contains(cm, needle) {
			t.Errorf("grafana provisioning configmap must not contain %q when grafana.assistant is off (additive contract, ka-0000005589); got:\n%s", needle, cm)
		}
	}
}

// TestGrafanaAssistantWrites_OffWhenAssistantOff_GrafanaOnly — even with the
// whole grafana.llm block off, the assistant entry never renders. Belt-and-
// suspenders for the default render.
func TestGrafanaAssistantWrites_OffWhenAssistantOff_GrafanaOnly(t *testing.T) {
	t.Parallel()
	cm := helmTemplate(t, "templates/grafana/configmap.yaml",
		"grafana.enabled=true",
	)
	if strings.Contains(cm, "gardenio-assistant-app") {
		t.Errorf("assistant provisioning entry must not render with only grafana.enabled (ka-0000005589); got:\n%s", cm)
	}
}

// TestGrafanaAssistantWrites_EntryRendersWhenEnabled — enabling the assistant
// provisions the gardenio-assistant-app app entry (enabled, org 1) so the chat
// nav page + config page appear, with the write flag in its jsonData.
func TestGrafanaAssistantWrites_EntryRendersWhenEnabled(t *testing.T) {
	t.Parallel()
	cm := helmTemplate(t, "templates/grafana/configmap.yaml", assistantWriteFlags...)
	if !strings.Contains(cm, "- type: gardenio-assistant-app") {
		t.Errorf("assistant provisioning entry must render when grafana.assistant.enabled (ka-0000005589); got:\n%s", cm)
	}
	if !strings.Contains(cm, "enableDashboardWrites:") {
		t.Errorf("assistant jsonData.enableDashboardWrites must render when grafana.assistant.enabled (ka-0000005589); got:\n%s", cm)
	}
	// The grafana-llm-app foundation entry must still be present alongside it.
	if !strings.Contains(cm, "- type: grafana-llm-app") {
		t.Errorf("grafana-llm-app entry must still render alongside the assistant entry (ka-0000005589); got:\n%s", cm)
	}
}

// TestGrafanaAssistantWrites_WritesOffByDefault — enabling the assistant WITHOUT
// flipping enableDashboardWrites renders `enableDashboardWrites: false`. The
// read-only posture is the default even once the chat box is provisioned.
func TestGrafanaAssistantWrites_WritesOffByDefault(t *testing.T) {
	t.Parallel()
	cm := helmTemplate(t, "templates/grafana/configmap.yaml", assistantWriteFlags...)
	if !strings.Contains(cm, "enableDashboardWrites: false") {
		t.Errorf("enableDashboardWrites must default to false when the assistant is enabled but writes are not opted in (ka-0000005589); got:\n%s", cm)
	}
	if strings.Contains(cm, "enableDashboardWrites: true") {
		t.Errorf("enableDashboardWrites must NOT be true by default (read-only posture is the default, ka-0000005589); got:\n%s", cm)
	}
}

// TestGrafanaAssistantWrites_WritesOnWhenOptedIn — setting
// grafana.assistant.enableDashboardWrites=true renders the flag true, opting the
// env into the bounded dashboard/folder write surface.
func TestGrafanaAssistantWrites_WritesOnWhenOptedIn(t *testing.T) {
	t.Parallel()
	flags := append([]string{"grafana.assistant.enableDashboardWrites=true"}, assistantWriteFlags...)
	cm := helmTemplate(t, "templates/grafana/configmap.yaml", flags...)
	if !strings.Contains(cm, "enableDashboardWrites: true") {
		t.Errorf("enableDashboardWrites must render true when opted in via grafana.assistant.enableDashboardWrites (ka-0000005589); got:\n%s", cm)
	}
}
