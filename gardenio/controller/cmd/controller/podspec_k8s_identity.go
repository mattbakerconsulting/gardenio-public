package main

import (
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"gardenio/controller/internal/podmanager"
)

// agentK8sIdentityIgnoredTotal counts agent beads whose own metadata asked for a
// Kubernetes identity (a ServiceAccount, or the API token) that the controller
// refused to honour. The reason label is a closed set, so cardinality is fixed.
var agentK8sIdentityIgnoredTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "agent_k8s_identity_override_ignored_total",
		Help: "Agent-bead metadata asks (service_account, needs_k8s_api=true) to choose the pod's Kubernetes identity that the controller ignored; the identity comes only from the role bead, project bead and chart default.",
	},
	[]string{"reason"},
)

// applyAgentK8sIdentity applies the agent-level slice of the pod's Kubernetes
// identity. It runs after the role, project and chart-default layers have set
// spec.ServiceAccountName and spec.NeedsK8sAPI, and it can only narrow them:
//
//   - service_account is never read into the spec. A pod's ServiceAccount comes
//     only from config an admin wrote (role bead, project bead, chart default),
//     so an agent bead cannot select, say, gardenio-infra-reader for itself. A
//     metadata value that differs from the resolved ServiceAccount is logged and
//     counted. The restart pin still holds: a reconciler-driven recreation
//     re-derives the same role-derived value.
//   - machine_client is never read from agent metadata either (Task 15 C5);
//     see ignoreAgentMachineClient.
//   - needs_k8s_api=false turns the API token off. needs_k8s_api=true never
//     turns it on; if the role and project did not, it is logged and counted.
//
// Both spec builders (reconciler and events) call this so they agree.
func applyAgentK8sIdentity(spec *podmanager.AgentPodSpec, metadata map[string]string, agentName string) {
	if sa := metadata["service_account"]; sa != "" && sa != spec.ServiceAccountName {
		slog.Warn("ignoring agent-level service_account: a pod's ServiceAccount comes only from the role bead, project bead or chart default",
			"agent", agentName,
			"role", spec.Role,
			"requested_service_account", sa,
			"service_account", spec.ServiceAccountName,
		)
		agentK8sIdentityIgnoredTotal.WithLabelValues("service_account").Inc()
	}

	ignoreAgentMachineClient(spec, metadata, agentName)

	switch metadata["needs_k8s_api"] {
	case "false":
		spec.NeedsK8sAPI = false
	case "true":
		if !spec.NeedsK8sAPI {
			slog.Warn("ignoring agent-level needs_k8s_api=true: the API token is enabled only by the role bead or project bead",
				"agent", agentName,
				"role", spec.Role,
			)
			agentK8sIdentityIgnoredTotal.WithLabelValues("needs_k8s_api").Inc()
		}
	}
}
