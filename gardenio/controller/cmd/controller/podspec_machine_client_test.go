package main

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"

	"gardenio/controller/internal/config"
	"gardenio/controller/internal/podmanager"
	"gardenio/controller/internal/subscriber"
)

const coordClient = "gardenio-cluster-coordinator"

// machineClientCfg has a role "coord" naming coordClient, a role "rogue"
// naming a client off the allowlist, and a role "bad" with an invalid id.
func machineClientCfg(mounts ...string) *config.Config {
	return &config.Config{
		Namespace:                "test",
		CoopImage:                "agent:latest",
		ProjectCache:             map[string]config.ProjectCacheEntry{},
		AgentMachineClientMounts: mounts,
		RoleCache: map[string]config.RoleCacheEntry{
			"coord": {MachineClient: coordClient},
			"rogue": {MachineClient: "gardenio-laptop-operator"},
			"bad":   {MachineClient: "../etc"},
			"crew":  {},
		},
	}
}

// buildBoth runs the reconciler and events spec builders.
func buildBoth(cfg *config.Config, role string, metadata map[string]string) map[string]podmanager.AgentPodSpec {
	return map[string]podmanager.AgentPodSpec{
		"reconciler": BuildSpecFromBeadInfo(cfg, "gardenio", role, role, "agent1", metadata),
		"events":     buildAgentPodSpec(cfg, subscriber.Event{Project: "gardenio", Role: role, AgentName: "agent1", Metadata: metadata}),
	}
}

func assertNoMachineClient(t *testing.T, what string, spec podmanager.AgentPodSpec) {
	t.Helper()
	if spec.MachineClientSecret != "" || spec.Env[podmanager.EnvMachineClientID] != "" || spec.Env[podmanager.EnvMachineClientSecretFile] != "" {
		t.Errorf("%s: got machine client secret %q env %q/%q, want none", what,
			spec.MachineClientSecret, spec.Env[podmanager.EnvMachineClientID], spec.Env[podmanager.EnvMachineClientSecretFile])
	}
}

// Positive: an allowlisted role client is mounted by both builders.
func TestRoleMachineClientMountedWhenAllowlisted(t *testing.T) {
	for path, spec := range buildBoth(machineClientCfg(coordClient), "coord", nil) {
		if spec.MachineClientSecret != "gardenio-machine-"+coordClient {
			t.Errorf("%s: MachineClientSecret = %q, want gardenio-machine-%s", path, spec.MachineClientSecret, coordClient)
		}
		if got := spec.Env[podmanager.EnvMachineClientID]; got != coordClient {
			t.Errorf("%s: %s = %q, want %q", path, podmanager.EnvMachineClientID, got, coordClient)
		}
		if got := spec.Env[podmanager.EnvMachineClientSecretFile]; got != "/run/gardenio/machine-client/secret" {
			t.Errorf("%s: %s = %q", path, podmanager.EnvMachineClientSecretFile, got)
		}
	}
}

// Negative, same role: the chart allowlist is empty or names another client.
func TestRoleMachineClientNotMountedUnlessAllowlisted(t *testing.T) {
	c := agentMachineClientIgnoredTotal.WithLabelValues("not_allowlisted")
	before := testutil.ToFloat64(c)
	for _, mounts := range [][]string{nil, {"gardenio-laptop-operator"}} {
		for path, spec := range buildBoth(machineClientCfg(mounts...), "coord", nil) {
			assertNoMachineClient(t, path, spec)
		}
	}
	for path, spec := range buildBoth(machineClientCfg(coordClient), "rogue", nil) {
		assertNoMachineClient(t, "rogue "+path, spec)
	}
	if got := testutil.ToFloat64(c) - before; got != 6 { // 3 cases x 2 builders
		t.Errorf("not_allowlisted counter moved by %v, want 6", got)
	}
}

func TestRoleMachineClientInvalidIDIgnored(t *testing.T) {
	c := agentMachineClientIgnoredTotal.WithLabelValues("invalid_id")
	before := testutil.ToFloat64(c)
	for path, spec := range buildBoth(machineClientCfg("../etc"), "bad", nil) {
		assertNoMachineClient(t, path, spec)
	}
	if got := testutil.ToFloat64(c) - before; got != 2 {
		t.Errorf("invalid_id counter moved by %v, want 2", got)
	}
}

// An agent bead cannot choose a machine client, even an allowlisted one; the
// same bead under the coord role still gets the role's.
func TestAgentMetadataCannotChooseMachineClient(t *testing.T) {
	c := agentMachineClientIgnoredTotal.WithLabelValues("agent_metadata")
	before := testutil.ToFloat64(c)
	meta := map[string]string{"machine_client": coordClient}
	for path, spec := range buildBoth(machineClientCfg(coordClient), "crew", meta) {
		assertNoMachineClient(t, path, spec)
	}
	if got := testutil.ToFloat64(c) - before; got != 2 {
		t.Errorf("agent_metadata counter moved by %v, want 2", got)
	}
	for path, spec := range buildBoth(machineClientCfg(coordClient), "coord", meta) {
		if spec.MachineClientSecret == "" {
			t.Errorf("coord %s: role machine client dropped", path)
		}
	}
}

// The rendered pod carries the secret as one 0400 file, read-only, and never
// as env.
func TestMachineClientRenderedAsFileNotEnv(t *testing.T) {
	spec := BuildSpecFromBeadInfo(machineClientCfg(coordClient), "gardenio", "coord", "coord", "agent1", nil)
	pod := podmanager.New(fake.NewClientset(), discardLogger()).BuildPod(spec)
	var vol *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == podmanager.VolumeMachineClient {
			vol = &pod.Spec.Volumes[i]
		}
	}
	if vol == nil || vol.Secret == nil || vol.Secret.SecretName != "gardenio-machine-"+coordClient {
		t.Fatalf("machine client volume = %+v", vol)
	}
	if len(vol.Secret.Items) != 1 || vol.Secret.Items[0].Key != "clientSecret" || vol.Secret.Items[0].Path != "secret" ||
		vol.Secret.Items[0].Mode == nil || *vol.Secret.Items[0].Mode != 0o400 {
		t.Errorf("machine client items = %+v, want clientSecret -> secret 0400", vol.Secret.Items)
	}
	for _, c := range pod.Spec.Containers {
		for _, e := range c.Env {
			if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil && e.ValueFrom.SecretKeyRef.Name == vol.Secret.SecretName {
				t.Errorf("container %s env %s reads the machine secret", c.Name, e.Name)
			}
		}
	}
	found := false
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		if m.Name == podmanager.VolumeMachineClient {
			found = m.MountPath == "/run/gardenio/machine-client" && m.ReadOnly
		}
	}
	if !found {
		t.Errorf("agent container has no read-only machine client mount at /run/gardenio/machine-client")
	}
}

// Agent pods cannot dial the public edge serving the issuer, so a
// machine-client pod gets the in-cluster discovery URL and the Keycloak CA
// (only its ca.crt, optional) when the controller is configured with them.
func TestMachineClientGetsInClusterIssuer(t *testing.T) {
	cfg := machineClientCfg(coordClient)
	cfg.AgentOIDCDiscoveryURL = "https://kc.test.svc:8443/auth/realms/gardenio"
	cfg.AgentOIDCCASecret = "kc-tls"
	spec := buildBoth(cfg, "coord", nil)["events"]
	if got := spec.Env[podmanager.EnvOIDCDiscoveryURL]; got != cfg.AgentOIDCDiscoveryURL {
		t.Errorf("%s = %q, want %q", podmanager.EnvOIDCDiscoveryURL, got, cfg.AgentOIDCDiscoveryURL)
	}
	if got := spec.Env[podmanager.EnvOIDCCAFile]; got != "/run/gardenio/machine-client-ca/ca.crt" {
		t.Errorf("%s = %q", podmanager.EnvOIDCCAFile, got)
	}
	pod := podmanager.New(fake.NewClientset(), discardLogger()).BuildPod(spec)
	var vol *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == podmanager.VolumeMachineClientCA {
			vol = &pod.Spec.Volumes[i]
		}
	}
	if vol == nil || vol.Secret == nil || vol.Secret.SecretName != "kc-tls" {
		t.Fatalf("CA volume = %+v, want Secret kc-tls", vol)
	}
	if len(vol.Secret.Items) != 1 || vol.Secret.Items[0].Key != "ca.crt" || vol.Secret.Optional == nil || !*vol.Secret.Optional {
		t.Errorf("CA volume must project only ca.crt, optional: %+v", vol.Secret)
	}

	plain := buildBoth(machineClientCfg(coordClient), "coord", nil)["events"]
	if plain.Env[podmanager.EnvOIDCDiscoveryURL] != "" || plain.MachineClientCASecret != "" {
		t.Errorf("unconfigured controller set discovery %q / CA %q", plain.Env[podmanager.EnvOIDCDiscoveryURL], plain.MachineClientCASecret)
	}
	crew := buildBoth(cfg, "crew", nil)["events"]
	if crew.Env[podmanager.EnvOIDCDiscoveryURL] != "" || crew.MachineClientCASecret != "" {
		t.Error("a role without a machine client got the in-cluster issuer")
	}
}
