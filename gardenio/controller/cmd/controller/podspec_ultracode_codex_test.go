package main

import (
	"strings"
	"testing"

	"gardenio/controller/internal/subscriber"
)

// ADR 0004 section 13: ultracode has no Codex equivalent. An explicit
// per-spawn --ultracode=on is refused on a codex spec (HarnessError); every
// other input is a quiet no-op that never bakes BOAT_ULTRACODE.
func TestUltracode_CodexHarness(t *testing.T) {
	tests := []struct {
		name      string
		agentMeta string
		roleUC    *bool
		wantErr   bool
	}{
		{"agent on is refused", "on", nil, true},
		{"agent on beats role off, still refused", "on", boolPtr(false), true},
		{"agent off is a no-op", "off", nil, false},
		{"absent is a no-op", "", nil, false},
		{"unrecognized is a no-op", "maybe", nil, false},
		{"role-level on is ignored, not refused", "", boolPtr(true), false},
		{"role-level on with agent off is ignored", "off", boolPtr(true), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := pinRoleAccount(onlyAccount(newHarnessCfg("", "", "codex"), "codex-acct"), "codex-acct")
			e := cfg.RoleCache["crew"]
			e.Ultracode = tc.roleUC
			cfg.RoleCache["crew"] = e
			meta := map[string]string{}
			if tc.agentMeta != "" {
				meta["ultracode"] = tc.agentMeta
			}
			ev := subscriber.Event{Project: "gardenio", Role: "crew", AgentName: "agent1", Metadata: meta}

			for path, spec := range map[string]func() (string, string, bool){
				"events": func() (string, string, bool) {
					s := buildAgentPodSpec(cfg, ev)
					v, ok := s.Env[EnvUltracode]
					return s.HarnessError, v, ok
				},
				"reconcile": func() (string, string, bool) {
					s := BuildSpecFromBeadInfo(cfg, "gardenio", "crew", "crew", "agent1", meta)
					v, ok := s.Env[EnvUltracode]
					return s.HarnessError, v, ok
				},
			} {
				herr, val, present := spec()
				if present {
					t.Errorf("%s: BOAT_ULTRACODE baked on a codex pod: %q", path, val)
				}
				if tc.wantErr {
					if !strings.Contains(herr, "--ultracode=on is not supported on harness \"codex\"") ||
						!strings.Contains(herr, "--harness claude") {
						t.Errorf("%s: HarnessError = %q, want ultracode refusal naming the fix", path, herr)
					}
				} else if strings.Contains(herr, "ultracode") {
					t.Errorf("%s: unexpected ultracode refusal: %q", path, herr)
				}
			}
		})
	}
}

// A claude spec is untouched by the codex branch: --ultracode=on still bakes.
func TestUltracode_ClaudeHarnessUnchanged(t *testing.T) {
	cfg := newHarnessCfg("", "", "claude")
	spec := buildAgentPodSpec(cfg, subscriber.Event{
		Project: "gardenio", Role: "crew", AgentName: "agent1",
		Metadata: map[string]string{"ultracode": "on"},
	})
	if spec.HarnessError != "" || spec.Env[EnvUltracode] != "on" {
		t.Errorf("claude: HarnessError=%q BOAT_ULTRACODE=%q, want no error and on", spec.HarnessError, spec.Env[EnvUltracode])
	}
}

// Gemini shares the codex refusal: an explicit --ultracode=on cannot be
// delivered, and BOAT_ULTRACODE is never baked into a gemini pod.
func TestUltracode_GeminiHarness(t *testing.T) {
	for _, on := range []bool{true, false} {
		cfg := pinRoleAccount(onlyAccount(newHarnessCfg("", "", "gemini"), "gemini-acct"), "gemini-acct")
		cfg.GeminiHarnessEnabled = true
		meta := map[string]string{}
		if on {
			meta["ultracode"] = "on"
		}
		spec := BuildSpecFromBeadInfo(cfg, "gardenio", "crew", "crew", "agent1", meta)
		if v, ok := spec.Env[EnvUltracode]; ok {
			t.Errorf("BOAT_ULTRACODE baked on a gemini pod: %q", v)
		}
		if on {
			if !strings.Contains(spec.HarnessError, "--ultracode=on is not supported on harness \"gemini\"") ||
				!strings.Contains(spec.HarnessError, "Gemini has no equivalent") {
				t.Errorf("HarnessError = %q, want gemini ultracode refusal", spec.HarnessError)
			}
		} else if spec.HarnessError != "" {
			t.Errorf("unexpected HarnessError %q", spec.HarnessError)
		}
	}
}
