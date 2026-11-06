package podmanager

import "testing"

// TestIdentityEnvVars verifies the runtime-neutral identity/correlation/
// attribution env the shared builder emits. This is the single source of truth
// consumed by BOTH the Kubernetes pod path (buildEnvVars) and the lambda_microvm
// path (microvmAgentSpecFromPodSpec), so both backends give `gb init` the same
// identity contract (ka-0000000401).
func TestIdentityEnvVars(t *testing.T) {
	spec := AgentPodSpec{
		Project:       "gardenio",
		Mode:          "crew",
		Role:          "gardenio-microvms",
		AgentName:     "sat-1",
		BeadID:        "gd-BEAD1",
		TaskID:        "gd-task",
		TriggerBeadID: "gd-trig",
		OperatedBy:    "oidc:sky.bennett@acme.example",
		UserBeadID:    "gd-user",
	}

	env := IdentityEnvVars(spec)

	want := map[string]string{
		"BOAT_ROLE":            "gardenio-microvms",
		"BOAT_PROJECT":         "gardenio",
		"BOAT_AGENT":           "sat-1",
		"BOAT_MODE":            "crew",
		"BEADS_ACTOR":          "sat-1",
		"KD_ACTOR":             "sat-1",
		"KD_AGENT_ID":          "gd-BEAD1",
		"GIT_AUTHOR_NAME":      "sat-1",
		"BEADS_AGENT_NAME":     "gardenio/sat-1",
		"BOAT_AGENT_BEAD_ID":   "gd-BEAD1",
		"DISABLE_AUTOUPDATER":  "1",
		"BOAT_TASK_ID":         "gd-task",
		"BOAT_TRIGGER_BEAD_ID": "gd-trig",
		"BEADS_OPERATED_BY":    "oidc:sky.bennett@acme.example",
		"BEADS_USER_BEAD_ID":   "gd-user",
	}
	for k, wv := range want {
		if got := env[k]; got != wv {
			t.Errorf("IdentityEnvVars[%q] = %q, want %q", k, got, wv)
		}
	}

	// Pod-specific env must NOT leak into the runtime-neutral builder — these
	// are meaningful only inside a Kubernetes pod and are added by buildEnvVars.
	for _, k := range []string{"HOME", "XDG_STATE_HOME", "POD_IP", "DOCKER_HOST", "PGHOST"} {
		if _, present := env[k]; present {
			t.Errorf("IdentityEnvVars must not contain pod-specific key %q", k)
		}
	}
}

// TestIdentityEnvVars_OptionalsOmitted verifies the optional fields are omitted
// (absent, not empty) when unset — matching the pod path's conditional emission
// so a scheduler-driven agent's env stays clean.
func TestIdentityEnvVars_OptionalsOmitted(t *testing.T) {
	env := IdentityEnvVars(AgentPodSpec{
		Project:   "gardenio",
		Mode:      "crew",
		Role:      "crew",
		AgentName: "a",
		BeadID:    "gd-A",
	})
	for _, k := range []string{"BOAT_TASK_ID", "BOAT_TRIGGER_BEAD_ID", "BEADS_OPERATED_BY", "BEADS_USER_BEAD_ID"} {
		if _, present := env[k]; present {
			t.Errorf("optional key %q should be omitted when unset", k)
		}
	}
	// Bead-id identity is always present.
	if env["BOAT_AGENT_BEAD_ID"] != "gd-A" || env["KD_AGENT_ID"] != "gd-A" {
		t.Errorf("bead-id identity missing: %v", env)
	}
}
