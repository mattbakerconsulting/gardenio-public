package tests

import (
	"strings"
	"testing"
)

// Admission policy starter set (audit finding F5, ka-0000002606): four Kyverno
// ClusterPolicies that harden admission of AGENT pods (the untrusted,
// LLM-generated workloads this platform schedules). Each is scoped to pods
// carrying the gardenio.io/agent label so the gardenio-system spine (postgres,
// nats, kyverno, the privileged DinD sidecar, root-running litellm) is NOT
// constrained.
//
// DEFAULT OFF, everywhere: these are Kyverno CRDs (kyverno.io/v1 ClusterPolicy)
// and prod-gardenio-eks does not run Kyverno, so an unconditional render would
// break `helm upgrade` on a missing CRD. Nothing renders unless the per-policy
// global.admissionPolicies.<name>.enabled=true. Rollout posture is Audit
// (report-only) by default; operators flip to Enforce per policy.

const (
	restrictRegistriesTemplate = "templates/kyverno/restrict-agent-registries.yaml"
	requireLimitsTemplate      = "templates/kyverno/require-agent-resource-limits.yaml"
	restrictSecCtxTemplate     = "templates/kyverno/restrict-agent-securitycontext.yaml"
	requireRunscTemplate       = "templates/kyverno/require-agent-runtimeclass.yaml"
)

// assertGatedOff renders a template with no --set and asserts it produces no
// ClusterPolicy — helm exits non-zero with "could not find template" when a
// --show-only target renders to nothing (the gated-off case).
func assertGatedOff(t *testing.T, template string) {
	t.Helper()
	out, err := helmTemplateAllowFail(t, template)
	if err == nil {
		if strings.Contains(out, "kind: ClusterPolicy") {
			t.Errorf("%s must NOT render a ClusterPolicy when disabled, got:\n%s", template, out)
		}
		return
	}
	if !strings.Contains(err.Error(), "could not find template") &&
		!strings.Contains(out, "could not find template") {
		t.Errorf("unexpected helm failure for %s: %v\n%s", template, err, out)
	}
}

// Every policy in the starter set must be default-OFF so a Kyverno-less cluster
// (prod-gardenio-eks today) is not broken on `helm upgrade`.
func TestAdmissionStarter_AllDefaultDisabled(t *testing.T) {
	t.Parallel()
	for _, tmpl := range []string{
		restrictRegistriesTemplate,
		requireLimitsTemplate,
		restrictSecCtxTemplate,
		requireRunscTemplate,
	} {
		assertGatedOff(t, tmpl)
	}
}

// The shared shape every rendered starter policy must satisfy: a kyverno.io/v1
// ClusterPolicy, scoped to agent pods via the gardenio.io/agent label selector,
// defaulting to the report-only Audit posture.
func assertStarterShape(t *testing.T, out string) {
	t.Helper()
	if !strings.Contains(out, "kind: ClusterPolicy") {
		t.Fatalf("ClusterPolicy must render when enabled=true, got:\n%s", out)
	}
	if !strings.Contains(out, "apiVersion: kyverno.io/v1") {
		t.Errorf("policy must be a kyverno.io/v1 resource, got:\n%s", out)
	}
	// Load-bearing scope: agent pods ONLY, or the policy would constrain the
	// gardenio-system spine and take the platform down.
	if !strings.Contains(out, "key: gardenio.io/agent") {
		t.Errorf("policy must scope to the gardenio.io/agent label, got:\n%s", out)
	}
	// Starter posture is report-only until an operator reviews the reports.
	if !strings.Contains(out, "validationFailureAction: Audit") {
		t.Errorf("policy must default to validationFailureAction: Audit, got:\n%s", out)
	}
}

func TestAdmissionStarter_RestrictRegistries(t *testing.T) {
	t.Parallel()
	out := helmTemplateNS(t, "gardenio", restrictRegistriesTemplate,
		"global.admissionPolicies.restrictRegistries.enabled=true",
	)
	assertStarterShape(t, out)
	// The default allow-list regex, written with [.] literal dots so the
	// rendered policy is a clean double-quoted scalar (a \. escape would fail
	// YAML parse).
	if !strings.Contains(out, "ghcr[.]io/mattbakerconsulting/gardenio/") {
		t.Errorf("policy must carry the gardenio registry allow-list regex, got:\n%s", out)
	}
	// Both container and initContainer image lists are checked.
	if !strings.Contains(out, "request.object.spec.containers") ||
		!strings.Contains(out, "request.object.spec.initContainers") {
		t.Errorf("policy must check both containers and initContainers, got:\n%s", out)
	}
}

// The regex must be overridable so a different registry mirror can be
// allow-listed without editing the chart.
func TestAdmissionStarter_RestrictRegistries_CustomRegex(t *testing.T) {
	t.Parallel()
	out := helmTemplateNS(t, "gardenio", restrictRegistriesTemplate,
		"global.admissionPolicies.restrictRegistries.enabled=true",
		"global.admissionPolicies.restrictRegistries.allowedImageRegexes={^example[.]com/.*}",
	)
	if !strings.Contains(out, "example[.]com/") {
		t.Errorf("custom allowedImageRegexes must render, got:\n%s", out)
	}
	if strings.Contains(out, "gitlab[.]com/acme") {
		t.Errorf("custom regex must REPLACE the default, got:\n%s", out)
	}
}

func TestAdmissionStarter_RequireResourceLimits(t *testing.T) {
	t.Parallel()
	out := helmTemplateNS(t, "gardenio", requireLimitsTemplate,
		"global.admissionPolicies.requireResourceLimits.enabled=true",
	)
	assertStarterShape(t, out)
	// Requires BOTH cpu and memory limits (?* = any non-empty value).
	if !strings.Contains(out, "cpu: \"?*\"") || !strings.Contains(out, "memory: \"?*\"") {
		t.Errorf("policy must require non-empty cpu and memory limits, got:\n%s", out)
	}
}

func TestAdmissionStarter_RestrictSecurityContext(t *testing.T) {
	t.Parallel()
	out := helmTemplateNS(t, "gardenio", restrictSecCtxTemplate,
		"global.admissionPolicies.restrictSecurityContext.enabled=true",
	)
	assertStarterShape(t, out)
	if !strings.Contains(out, "allowPrivilegeEscalation: false") {
		t.Errorf("policy must require allowPrivilegeEscalation=false, got:\n%s", out)
	}
	if !strings.Contains(out, "runAsNonRoot: true") {
		t.Errorf("policy must require runAsNonRoot=true, got:\n%s", out)
	}
}

func TestAdmissionStarter_RequireRunsc(t *testing.T) {
	t.Parallel()
	out := helmTemplateNS(t, "gardenio", requireRunscTemplate,
		"global.admissionPolicies.requireRunsc.enabled=true",
	)
	assertStarterShape(t, out)
	if !strings.Contains(out, "runtimeClassName: runsc") {
		t.Errorf("policy must require runtimeClassName: runsc, got:\n%s", out)
	}
}

// The RuntimeClass name must be overridable so a differently-named sandbox
// runtime can be required without editing the chart.
func TestAdmissionStarter_RequireRunsc_CustomRuntimeClass(t *testing.T) {
	t.Parallel()
	out := helmTemplateNS(t, "gardenio", requireRunscTemplate,
		"global.admissionPolicies.requireRunsc.enabled=true",
		"global.admissionPolicies.requireRunsc.runtimeClassName=gvisor",
	)
	if !strings.Contains(out, "runtimeClassName: gvisor") {
		t.Errorf("custom runtimeClassName must render, got:\n%s", out)
	}
}

// validationFailureAction must be overridable per policy so an operator can
// flip an individual policy to Enforce after reviewing its Audit reports.
func TestAdmissionStarter_EnforceOverride(t *testing.T) {
	t.Parallel()
	out := helmTemplateNS(t, "gardenio", restrictRegistriesTemplate,
		"global.admissionPolicies.restrictRegistries.enabled=true",
		"global.admissionPolicies.restrictRegistries.validationFailureAction=Enforce",
	)
	if !strings.Contains(out, "validationFailureAction: Enforce") {
		t.Errorf("validationFailureAction override to Enforce must render, got:\n%s", out)
	}
}
