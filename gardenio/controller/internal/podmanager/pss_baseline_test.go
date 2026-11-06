package podmanager

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// Pod Security Standards "baseline" over the pod the controller builds. The
// live-namespace e2e lane runs the controller in namespaces labelled
// pod-security.kubernetes.io/enforce=baseline (gardenio/tests/e2e/lane/rbac/
// namespace-pool.yaml; operator review gd-LXD71HHC8n), so an agent pod that
// trips baseline is an agent that never starts there. The plain pod (no DinD,
// no Ziti, no gVisor runtime class) must pass; the DinD sidecar must be seen
// by the checker, or the first assertion is vacuous.

var (
	pssBaselineCaps = map[corev1.Capability]bool{
		"AUDIT_WRITE": true, "CHOWN": true, "DAC_OVERRIDE": true, "FOWNER": true, "FSETID": true,
		"KILL": true, "MKNOD": true, "NET_BIND_SERVICE": true, "SETFCAP": true, "SETGID": true,
		"SETPCAP": true, "SETUID": true, "SYS_CHROOT": true,
	}
	pssBaselineSysctls = map[string]bool{
		"kernel.shm_rmid_forced": true, "net.ipv4.ip_local_port_range": true, "net.ipv4.ip_unprivileged_port_start": true,
		"net.ipv4.tcp_syncookies": true, "net.ipv4.ping_group_range": true, "net.ipv4.ip_local_reserved_ports": true,
		"net.ipv4.tcp_keepalive_time": true, "net.ipv4.tcp_fin_timeout": true, "net.ipv4.tcp_keepalive_intvl": true,
		"net.ipv4.tcp_keepalive_probes": true, "net.ipv4.tcp_rmem": true, "net.ipv4.tcp_wmem": true,
	}
)

// pssBaselineViolations lists every field of spec the baseline profile
// rejects (upstream pod-security-standards table).
func pssBaselineViolations(spec corev1.PodSpec) []string {
	var out []string
	if spec.HostNetwork {
		out = append(out, "hostNetwork")
	}
	if spec.HostPID {
		out = append(out, "hostPID")
	}
	if spec.HostIPC {
		out = append(out, "hostIPC")
	}
	for _, v := range spec.Volumes {
		if v.HostPath != nil {
			out = append(out, "hostPath volume "+v.Name)
		}
	}
	if psc := spec.SecurityContext; psc != nil {
		for _, s := range psc.Sysctls {
			if !pssBaselineSysctls[s.Name] {
				out = append(out, "unsafe sysctl "+s.Name)
			}
		}
		if psc.SeccompProfile != nil && psc.SeccompProfile.Type == corev1.SeccompProfileTypeUnconfined {
			out = append(out, "pod seccomp Unconfined")
		}
		if psc.SELinuxOptions != nil && (psc.SELinuxOptions.User != "" || psc.SELinuxOptions.Role != "") {
			out = append(out, "pod seLinuxOptions user/role")
		}
		if psc.WindowsOptions != nil && psc.WindowsOptions.HostProcess != nil && *psc.WindowsOptions.HostProcess {
			out = append(out, "pod windows hostProcess")
		}
	}
	check := func(kind string, c corev1.Container) {
		name := kind + "/" + c.Name
		for _, p := range c.Ports {
			if p.HostPort != 0 {
				out = append(out, name+": hostPort")
			}
		}
		sc := c.SecurityContext
		if sc == nil {
			return
		}
		if sc.Privileged != nil && *sc.Privileged {
			out = append(out, name+": privileged")
		}
		if sc.Capabilities != nil {
			for _, cap := range sc.Capabilities.Add {
				if !pssBaselineCaps[cap] {
					out = append(out, name+": adds "+string(cap))
				}
			}
		}
		if sc.ProcMount != nil && *sc.ProcMount != corev1.DefaultProcMount {
			out = append(out, name+": procMount "+string(*sc.ProcMount))
		}
		if sc.SeccompProfile != nil && sc.SeccompProfile.Type == corev1.SeccompProfileTypeUnconfined {
			out = append(out, name+": seccomp Unconfined")
		}
		if sc.AppArmorProfile != nil && sc.AppArmorProfile.Type == corev1.AppArmorProfileTypeUnconfined {
			out = append(out, name+": appArmor Unconfined")
		}
		if sc.SELinuxOptions != nil && (sc.SELinuxOptions.User != "" || sc.SELinuxOptions.Role != "") {
			out = append(out, name+": seLinuxOptions user/role")
		}
	}
	for _, c := range spec.InitContainers {
		check("init", c)
	}
	for _, c := range spec.Containers {
		check("container", c)
	}
	return out
}

func TestBuildPod_MinimalSpec_PassesPSSBaseline(t *testing.T) {
	m := newTestManager()
	pod := m.buildPod(minimalSpec())
	if v := pssBaselineViolations(pod.Spec); len(v) != 0 {
		t.Errorf("the plain agent pod would be rejected by pod-security enforce=baseline:\n  %s", strings.Join(v, "\n  "))
	}
	if len(pod.Spec.Containers) == 0 {
		t.Fatal("no containers built")
	}
}

func TestBuildPod_DockerSpec_FailsPSSBaseline(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec()
	spec.Docker = true
	pod := m.buildPod(spec)
	v := pssBaselineViolations(pod.Spec)
	if len(v) == 0 {
		t.Fatal("the DinD sidecar is privileged; the checker must report it or the baseline assertion above is vacuous")
	}
	for _, s := range v {
		if strings.HasSuffix(s, DindContainerName+": privileged") {
			return
		}
	}
	t.Errorf("violations %q do not name the privileged %s sidecar", v, DindContainerName)
}
