package main

import (
	"testing"

	"gardenio/controller/internal/microvm"
	"gardenio/controller/internal/podmanager"
)

// TestMicrovmAgentSpecFromPodSpec verifies the runtime-neutral translation:
// correlation identifiers carry over, credential/model/account env is forwarded
// verbatim, the cluster-internal service endpoints the MicroVM image owns via its
// local Ziti proxy listeners are stripped (they would otherwise clobber the
// image's 127.0.0.1 config with unresolvable cluster DNS), and the outbound-only
// registration mode is stamped (C6, ka-0000005563).
func TestMicrovmAgentSpecFromPodSpec(t *testing.T) {
	pod := podmanager.AgentPodSpec{
		Project:         "gardenio",
		Role:            "crew",
		AgentName:       "w5p",
		TaskID:          "gd-task",
		OperatedBy:      "oidc:sky.bennett@acme.example",
		ExecutionTarget: podmanager.ExecutionTargetLambdaMicroVM,
		Env: map[string]string{
			// Forwarded (account/model/git/correlation):
			"CLAUDE_MODEL":      "opus",
			"BOAT_PROJECTS":     "gardenio",
			"ANTHROPIC_MODEL":   "claude-opus-4-8",
			"COOP_BROKER_TOKEN": "tok",
			// Stripped (MicroVM image supplies its own local-proxy endpoints):
			"BEADS_HTTP_ADDR":           "http://gardenio-beads:8080",
			"BEADS_GRPC_ADDR":           "gardenio-beads:9090",
			"BEADS_NATS_URL":            "nats://gardenio-nats:4222",
			"COOP_BROKER_URL":           "http://gardenio-coopmux:9800",
			"COOP_MUX_URL":              "http://gardenio-coopmux:9800",
			"COOP_NATS_URL":             "nats://gardenio-nats:4222",
			"COOP_NATS_FORWARD":         "1",
			"COOP_NATS_TOKEN_FILE":      "/var/run/nats/token",
			"COOP_HOST":                 "0.0.0.0",
			"ANTHROPIC_BASE_URL":        "http://gardenio-litellm:4000",
			"BEADS_E2E_HTTP_ADDR":       "http://x",
			"SLACK_BRIDGE_URL":          "http://gardenio-slack-bridge:8090",
			"BEADS_VIEWER_INTERNAL_URL": "http://gardenio-beads-viewer.gardenio-system.svc.cluster.local:8095",
		},
	}

	spec := microvmAgentSpecFromPodSpec("gd-BEAD1", pod)

	if spec.BeadID != "gd-BEAD1" || spec.Project != "gardenio" || spec.Role != "crew" ||
		spec.AgentName != "w5p" || spec.TaskID != "gd-task" {
		t.Errorf("correlation fields wrong: %+v", spec)
	}

	// Forwarded keys present.
	for k, want := range map[string]string{
		"CLAUDE_MODEL": "opus", "BOAT_PROJECTS": "gardenio",
		"ANTHROPIC_MODEL": "claude-opus-4-8", "COOP_BROKER_TOKEN": "tok",
	} {
		if got := spec.Environment[k]; got != want {
			t.Errorf("env[%q] = %q, want %q (should be forwarded)", k, got, want)
		}
	}

	// Stripped keys absent.
	for k := range microvmClusterInternalEnvKeys {
		if _, present := spec.Environment[k]; present {
			t.Errorf("env[%q] present; want stripped (MicroVM image owns it locally)", k)
		}
	}

	// Outbound-only registration mode stamped (C6): coop must register OUTBOUND
	// over the NATS relay and never advertise an inbound POD_IP/127.0.0.1
	// callback coopmux cannot reach.
	for k, want := range microvmOutboundOnlyEnv {
		if got := spec.Environment[k]; got != want {
			t.Errorf("env[%q] = %q, want %q (outbound-only registration mode)", k, got, want)
		}
	}
	// The relay subject prefix (session discovery) must match coopmux's
	// --nats-relay-prefix; the general event prefix must stay on coop.events so
	// the two subject spaces never collide (ka-0000005381).
	if spec.Environment["COOP_NATS_RELAY_PREFIX"] != coopmuxNatsRelayPrefix {
		t.Errorf("COOP_NATS_RELAY_PREFIX = %q, want %q (must match coopmux --nats-relay-prefix)",
			spec.Environment["COOP_NATS_RELAY_PREFIX"], coopmuxNatsRelayPrefix)
	}
	if spec.Environment["COOP_NATS_PREFIX"] != coopEventsPrefix {
		t.Errorf("COOP_NATS_PREFIX = %q, want %q (general event space, must NOT be the relay prefix)",
			spec.Environment["COOP_NATS_PREFIX"], coopEventsPrefix)
	}

	// Agent identity / correlation / task / attribution env is present — the
	// runtime-neutral set the pod path also emits (ka-0000000401). Without
	// BOAT_AGENT_BEAD_ID the in-VM `gb init` cannot identify its own bead, set
	// agent_state, or claim BOAT_TASK_ID.
	for k, want := range map[string]string{
		"BOAT_AGENT_BEAD_ID": "gd-BEAD1",
		"KD_AGENT_ID":        "gd-BEAD1",
		"BOAT_TASK_ID":       "gd-task",
		"BOAT_ROLE":          "crew",
		"BOAT_PROJECT":       "gardenio",
		"BOAT_AGENT":         "w5p",
		"BEADS_ACTOR":        "w5p",
		"KD_ACTOR":           "w5p",
		"BEADS_AGENT_NAME":   "gardenio/w5p",
		"BEADS_OPERATED_BY":  "oidc:sky.bennett@acme.example",
	} {
		if got := spec.Environment[k]; got != want {
			t.Errorf("identity env[%q] = %q, want %q (must match the pod-path contract)", k, got, want)
		}
	}

	// The identity builder is the single source of truth: everything it emits
	// for this spec must appear verbatim in the MicroVM runtime document (the
	// outbound-only overrides do not collide with any identity key). The helper
	// pins spec.BeadID to the passed agentBeadID, so mirror that here.
	pod.BeadID = "gd-BEAD1"
	for k, want := range podmanager.IdentityEnvVars(pod) {
		if got := spec.Environment[k]; got != want {
			t.Errorf("IdentityEnvVars[%q]=%q not propagated to runtime doc (got %q)", k, want, got)
		}
	}
}

// TestMicrovmAgentSpecOutboundOnlyIsAuthoritative guards that a stray relay/
// callback env on the pod spec can never flip a MicroVM back onto the inbound
// path — the outbound-only mode is applied last and wins.
func TestMicrovmAgentSpecOutboundOnlyIsAuthoritative(t *testing.T) {
	pod := podmanager.AgentPodSpec{
		Env: map[string]string{
			"COOP_OUTBOUND_ONLY":     "",      // someone tries to disable it
			"COOP_NATS_RELAY":        "",      // ...and the outbound relay
			"COOP_NATS_PREFIX":       "wrong", // ...and misalign the event subject
			"COOP_NATS_RELAY_PREFIX": "wrong", // ...and the relay subject
		},
	}
	spec := microvmAgentSpecFromPodSpec("gd-Y", pod)
	for k, want := range microvmOutboundOnlyEnv {
		if got := spec.Environment[k]; got != want {
			t.Errorf("env[%q] = %q, want %q (outbound-only must be authoritative)", k, got, want)
		}
	}
}

// TestMicrovmAgentSpecEmptyEnv guards the nil-env path: even with no forwarded
// pod env, the outbound-only registration mode AND the always-present agent
// identity vars (keyed on the bead ID) are stamped so the in-VM gb init can
// still identify itself and check in.
func TestMicrovmAgentSpecEmptyEnv(t *testing.T) {
	spec := microvmAgentSpecFromPodSpec("gd-X", podmanager.AgentPodSpec{})
	if spec.BeadID != "gd-X" {
		t.Errorf("BeadID = %q, want gd-X", spec.BeadID)
	}
	if spec.Environment == nil {
		t.Error("Environment = nil, want non-nil map")
	}
	// Outbound-only registration keys present.
	for k, want := range microvmOutboundOnlyEnv {
		if got := spec.Environment[k]; got != want {
			t.Errorf("env[%q] = %q, want %q", k, got, want)
		}
	}
	// The agent's own bead id is stamped even with no forwarded env — this is
	// the fix for ka-0000000401 (an in-VM agent with no BOAT_AGENT_BEAD_ID can
	// never set agent_state or claim work).
	if got := spec.Environment["BOAT_AGENT_BEAD_ID"]; got != "gd-X" {
		t.Errorf("BOAT_AGENT_BEAD_ID = %q, want gd-X", got)
	}
	if got := spec.Environment["KD_AGENT_ID"]; got != "gd-X" {
		t.Errorf("KD_AGENT_ID = %q, want gd-X", got)
	}
	// No BOAT_TASK_ID when unassigned (omitted, not empty).
	if _, present := spec.Environment["BOAT_TASK_ID"]; present {
		t.Error("BOAT_TASK_ID present with no TaskID; want omitted")
	}
}

// compile-time check that the concrete backend satisfies the runtime seam the
// spawn door depends on (documents the intended coupling).
var _ microvm.AgentRuntime = (*microvm.Backend)(nil)
