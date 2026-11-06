package main

import (
	"testing"
	"time"

	"gardenio/controller/internal/config"
	"gardenio/controller/internal/podmanager"
	"gardenio/controller/internal/subscriber"
)

// In-VM standby parking over Ziti (plan ka-0000004145, task gd-PBHP0WDLyW).
//
// A prewarmed lambda_microvm agent parks in coop's entrypoint standby loop
// (gb init --standby -> standbyWait) until the pool manager flips its bead
// agent_state prewarmed->assigning. For that to work in a MicroVM — which has
// no cluster DNS and reaches the beads daemon only over its local Ziti proxy
// listener (127.0.0.1:18080) — the four standby control vars the entrypoint and
// standbyWait read MUST survive the runtime-neutral translation
// (microvmAgentSpecFromPodSpec) into the per-run agent-config secret's
// `environment`, while the pod path's cluster-DNS BEADS_HTTP_ADDR must be
// STRIPPED so the image's local-proxy value wins.
//
// The four vars, and who reads them (all in the MicroVM guest):
//   - BOAT_STANDBY=true       entrypoint.sh -> adds `gb init --standby`
//   - BOAT_AGENT_STATE=prewarmed  entrypoint.sh gate + gb prime/detectNudgeType
//   - BOAT_STANDBY_POLL       standbyWait poll interval (warm-spawn latency)
//   - BOAT_AGENT_BEAD_ID      standbyWait's GetBead target (its own bead)
//
// BOAT_AGENT_BEAD_ID is stamped by podmanager.IdentityEnvVars (covered by
// microvm_spec_test.go); this test pins the three standby-specific vars end to
// end from the spawn event through the MicroVM AgentSpec the backend persists.

// prewarmedMicrovmCfg returns a config whose gardenio-microvms role pins the
// lambda_microvm execution target and whose StandbyPoll is the sub-second
// warm-pool default (ka-0000003812), so buildAgentPodSpec stamps
// BOAT_STANDBY_POLL exactly as production does.
func prewarmedMicrovmCfg() *config.Config {
	return &config.Config{
		Namespace:     "gardenio",
		BeadsHTTPAddr: "http://gardenio-beads:8080", // cluster DNS — must be stripped
		BeadsGRPCAddr: "gardenio-beads:9090",
		StandbyPoll:   500 * time.Millisecond,
		ProjectCache:  map[string]config.ProjectCacheEntry{},
		RoleCache: map[string]config.RoleCacheEntry{
			"gardenio-microvms": {ExecutionTarget: podmanager.ExecutionTargetLambdaMicroVM},
		},
	}
}

// prewarmedMicrovmEvent is a prewarmed-pool spawn for a lambda_microvm agent:
// agent_state=prewarmed is what buildAgentPodSpec keys the standby env off of.
func prewarmedMicrovmEvent() subscriber.Event {
	return subscriber.Event{
		Type:      subscriber.AgentSpawn,
		Project:   "gardenio",
		Role:      "gardenio-microvms",
		AgentName: "vm-warm",
		BeadID:    "gd-POOL1",
		Metadata: map[string]string{
			"agent_state": "prewarmed",
		},
	}
}

// TestMicrovmPrewarmedEnv_StandbyVarsSurviveTranslation drives the full
// production spawn path (buildAgentPodSpec -> microvmAgentSpecFromPodSpec) and
// asserts the standby control vars land in the MicroVM AgentSpec.Environment
// (i.e. the agent-config secret `environment`), while the cluster-DNS
// BEADS_HTTP_ADDR is stripped so the image's local Ziti proxy value wins.
func TestMicrovmPrewarmedEnv_StandbyVarsSurviveTranslation(t *testing.T) {
	cfg := prewarmedMicrovmCfg()
	pod := buildAgentPodSpec(cfg, prewarmedMicrovmEvent())

	// Precondition: the event resolved to the MicroVM backend and was marked
	// prewarmed (so the standby env branch ran).
	if pod.ExecutionTarget != podmanager.ExecutionTargetLambdaMicroVM {
		t.Fatalf("execution target = %q, want lambda_microvm", pod.ExecutionTarget)
	}
	if !pod.Prewarmed {
		t.Fatalf("pod.Prewarmed = false, want true (agent_state=prewarmed event)")
	}
	// The pod spec carries the standby vars and the cluster-DNS beads addr...
	for k, want := range map[string]string{
		"BOAT_STANDBY":      "true",
		"BOAT_AGENT_STATE":  "prewarmed",
		"BOAT_STANDBY_POLL": (500 * time.Millisecond).String(),
		"BEADS_HTTP_ADDR":   "http://gardenio-beads:8080",
	} {
		if got := pod.Env[k]; got != want {
			t.Fatalf("pod.Env[%q] = %q, want %q (buildAgentPodSpec precondition)", k, got, want)
		}
	}

	// ...now the runtime-neutral translation the MicroVM spawn door applies.
	spec := microvmAgentSpecFromPodSpec("gd-POOL1", pod)

	// The three standby-specific control vars survive verbatim. Without these
	// the in-VM entrypoint never appends `gb init --standby` (BOAT_STANDBY),
	// gb prime/detectNudgeType never sees a parked agent (BOAT_AGENT_STATE), and
	// standbyWait falls back to its hard-coded 5s poll instead of the warm-pool
	// interval (BOAT_STANDBY_POLL).
	for k, want := range map[string]string{
		"BOAT_STANDBY":      "true",
		"BOAT_AGENT_STATE":  "prewarmed",
		"BOAT_STANDBY_POLL": (500 * time.Millisecond).String(),
	} {
		if got := spec.Environment[k]; got != want {
			t.Errorf("microvm env[%q] = %q, want %q (standby var must survive translation)", k, got, want)
		}
	}

	// BOAT_AGENT_BEAD_ID is standbyWait's GetBead target — the agent's own bead.
	// It is stamped by IdentityEnvVars off the resolved bead ID, so it is present
	// even though the event metadata never carried it.
	if got := spec.Environment["BOAT_AGENT_BEAD_ID"]; got != "gd-POOL1" {
		t.Errorf("microvm env[BOAT_AGENT_BEAD_ID] = %q, want gd-POOL1 (standbyWait GetBead target)", got)
	}

	// The pod path's cluster-DNS beads endpoint MUST be stripped: the MicroVM
	// image owns BEADS_HTTP_ADDR, pointing gb at its local Ziti proxy listener
	// (127.0.0.1:18080). Forwarding gardenio-beads:8080 would leave standbyWait's
	// GetBead dialing a hostname the VM cannot resolve — the poll would fail
	// every tick and the agent would never discover its assignment.
	if got, present := spec.Environment["BEADS_HTTP_ADDR"]; present {
		t.Errorf("microvm env[BEADS_HTTP_ADDR] = %q present; want stripped so the image's local Ziti proxy value wins", got)
	}
}

// TestMicrovmPrewarmedEnv_NonPrewarmedHasNoStandby is the negative control: an
// ordinary (non-prewarmed) MicroVM spawn carries none of the standby control
// vars, so its in-VM entrypoint runs the normal (non-standby) path.
func TestMicrovmPrewarmedEnv_NonPrewarmedHasNoStandby(t *testing.T) {
	cfg := prewarmedMicrovmCfg()
	ev := prewarmedMicrovmEvent()
	delete(ev.Metadata, "agent_state") // not a prewarmed spawn

	pod := buildAgentPodSpec(cfg, ev)
	if pod.Prewarmed {
		t.Fatalf("pod.Prewarmed = true for a non-prewarmed event")
	}
	spec := microvmAgentSpecFromPodSpec("gd-POOL1", pod)

	for _, k := range []string{"BOAT_STANDBY", "BOAT_AGENT_STATE", "BOAT_STANDBY_POLL"} {
		if got, present := spec.Environment[k]; present {
			t.Errorf("microvm env[%q] = %q present on a non-prewarmed spawn; want absent", k, got)
		}
	}
}
