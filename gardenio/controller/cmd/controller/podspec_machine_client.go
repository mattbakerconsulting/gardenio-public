package main

import (
	"log/slog"
	"regexp"
	"slices"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"gardenio/controller/internal/config"
	"gardenio/controller/internal/podmanager"
)

// agentMachineClientIgnoredTotal counts machine_client asks the controller did
// not mount. The reason label is a closed set: not_allowlisted, invalid_id,
// agent_metadata.
var agentMachineClientIgnoredTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "agent_machine_client_ignored_total",
		Help: "machine_client asks the controller did not mount: a role bead naming a client off GARDENIO_AGENT_MACHINE_CLIENT_MOUNTS or with an invalid id, or an agent bead naming one at all.",
	},
	[]string{"reason"},
)

// machineClientID is a Keycloak client id usable in the Secret name
// gardenio-machine-<id>: a DNS-1123 label.
var machineClientID = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// machineClientSecretName is the Secret holding clientID's clientSecret, as
// Task 13 Step 3 names it.
func machineClientSecretName(clientID string) string {
	return "gardenio-machine-" + clientID
}

// applyRoleMachineClient mounts the role bead's machine_client secret at
// podmanager.MountMachineClient and sets the env vars kd and gb --as-machine
// read (with the in-cluster issuer discovery URL and CA when configured), only
// when clientID is on cfg.AgentMachineClientMounts
// (escalation routing Task 15, C5). Otherwise the field is logged, counted and
// ignored, as for service_account.
func applyRoleMachineClient(cfg *config.Config, spec *podmanager.AgentPodSpec, clientID string) {
	reason := ""
	switch {
	case !machineClientID.MatchString(clientID):
		reason = "invalid_id"
	case !slices.Contains(cfg.AgentMachineClientMounts, clientID):
		reason = "not_allowlisted"
	}
	if reason != "" {
		slog.Warn("ignoring role machine_client: only a client on GARDENIO_AGENT_MACHINE_CLIENT_MOUNTS is mounted",
			"agent", spec.AgentName,
			"role", spec.Role,
			"machine_client", clientID,
			"reason", reason,
		)
		agentMachineClientIgnoredTotal.WithLabelValues(reason).Inc()
		return
	}
	spec.MachineClientSecret = machineClientSecretName(clientID)
	if spec.Env == nil {
		spec.Env = map[string]string{}
	}
	spec.Env[podmanager.EnvMachineClientID] = clientID
	spec.Env[podmanager.EnvMachineClientSecretFile] = podmanager.MountMachineClient + "/" + podmanager.MachineClientSecretFileName
	if cfg.AgentOIDCDiscoveryURL != "" {
		spec.Env[podmanager.EnvOIDCDiscoveryURL] = cfg.AgentOIDCDiscoveryURL
	}
	if cfg.AgentOIDCCASecret != "" {
		spec.MachineClientCASecret = cfg.AgentOIDCCASecret
		spec.Env[podmanager.EnvOIDCCAFile] = podmanager.MountMachineClientCA + "/" + podmanager.MachineClientCAFileName
	}
}

// ignoreAgentMachineClient logs and counts an agent bead's own machine_client:
// only the role bead an admin wrote can choose a pod's machine credential.
func ignoreAgentMachineClient(spec *podmanager.AgentPodSpec, metadata map[string]string, agentName string) {
	if metadata["machine_client"] == "" {
		return
	}
	slog.Warn("ignoring agent-level machine_client: a pod's machine client comes only from its role bead",
		"agent", agentName,
		"role", spec.Role,
		"requested_machine_client", metadata["machine_client"],
	)
	agentMachineClientIgnoredTotal.WithLabelValues("agent_metadata").Inc()
}
