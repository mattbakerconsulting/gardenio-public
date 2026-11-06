# Admission policy starter set (Kyverno)

**Task:** ka-0000002606 · **Audit finding:** F5 (Medium), audit ka-0000003522 ·
**Epic:** ka-0000003872 (Gardenio security audit)

## What this closes

Audit finding **F5**: `prod-gardenio-eks` runs **no admission policy engine**
(no Kyverno/Gatekeeper/OPA, only infra webhooks). For a platform that schedules
**untrusted, LLM-generated pod specs**, nothing enforced — at admission time — a
registry allow-list, a restricted securityContext, required resource limits, or
a sandboxed runtime. A crafted or compromised agent pod spec (attacker paths
**A3** compromised agent, **A5** malicious teammate/bead) could pull an arbitrary
image, run privileged/as-root, omit limits to starve co-tenants, or skip the
gVisor sandbox.

This change adds a **Kyverno starter policy set** — four `ClusterPolicy`
resources, each **scoped to agent pods only** and shipped **default OFF** with an
**Audit → Enforce** rollout posture.

Two Kyverno policies already existed before this set and follow the same pattern:

- `verify-image-signatures` — cosign keyless signature verification
  (`global.imageSigning.verify.enabled`, see [`image-signing.md`](image-signing.md)).
- `disallow-cover-images` — deny coverage-instrumented `-cover` images outside
  E2E (`global.coverage.disallowCoverImages.enabled`).

The starter set below rounds out the F5 remediation.

## The four policies

Every policy matches **only** pods carrying the `gardenio.io/agent` label
(`podmanager.LabelAgent`), which every spawned agent pod carries and **no spine
workload** does. This is load-bearing: the `gardenio-system` namespace runs at PSA
`baseline` and hosts a **privileged DinD sidecar** and **root-running litellm**;
a cluster-wide policy would deny those and take the platform down. Scoping to the
agent label constrains exactly the untrusted workloads and nothing else.

| Policy | Values key (`global.admissionPolicies.*`) | Canonical source | Requires |
|---|---|---|---|
| Registry allow-list | `restrictRegistries` | `policy/kyverno/restrict-agent-registries.yaml` | agent images match `<registry>/*` |
| Require resource limits | `requireResourceLimits` | `policy/kyverno/require-agent-resource-limits.yaml` | every container sets `resources.limits.{cpu,memory}` |
| Restricted securityContext | `restrictSecurityContext` | `policy/kyverno/restrict-agent-securitycontext.yaml` | `allowPrivilegeEscalation=false`, not privileged, `runAsNonRoot=true` |
| Require runsc RuntimeClass | `requireRunsc` | `policy/kyverno/require-agent-runtimeclass.yaml` | `spec.runtimeClassName=runsc` |

Each has a values-parameterized chart copy under
`gardenio/helm/gardenio/templates/kyverno/` and a `validationFailureAction` that
defaults to **`Audit`** (report-only).

### Why these are also mirrored as canonical `policy/kyverno/` sources

Like the two pre-existing policies, each ships as a **cluster-agnostic canonical
YAML** under `policy/kyverno/` (applies with a plain `kubectl apply` on any
cluster) **and** a values-parameterized Helm copy under `templates/kyverno/`. The
canonical source is the readable reference and the non-Helm deploy path; the
chart copy is what a gardenio release renders when the operator flips the flag.

## Default OFF — why

These are Kyverno CRDs (`kyverno.io/v1 ClusterPolicy`). `prod-gardenio-eks` does
**not** run Kyverno today, so an unconditional render would break `helm upgrade`
on a missing CRD. Nothing renders unless the operator both installs Kyverno
**and** flips the per-policy `enabled` flag — identical to
`global.imageSigning.verify.enabled` and the fleet-manager VAP gate.

## Rollout: Audit → Enforce

1. **Install Kyverno** on the cluster (admission controller + policy-reporter).
2. **Enable in Audit** — set the per-policy `enabled: true` (they default to
   `validationFailureAction: Audit`). Pods are still admitted; violations surface
   as `PolicyReport` / `ClusterPolicyReport` objects:

   ```sh
   kubectl get clusterpolicyreport -o wide
   kubectl get policyreport -A -o wide
   ```

3. **Review the reports.** Agent pods are already restricted-conformant and
   podmanager-built pods already carry limits + gardenio-registry images, so
   `restrictSecurityContext`, `requireResourceLimits`, and `restrictRegistries`
   should report **zero** violations on the happy path. Any violation is a real
   gap to fix **before** enforcing.

4. **Flip to Enforce per policy** once its report is clean:

   ```yaml
   global:
     admissionPolicies:
       restrictRegistries:
         enabled: true
         validationFailureAction: Enforce
   ```

### ⚠️ `requireRunsc` stays Audit until gVisor is universal

`requireRunsc` is a special case: it must remain **Audit** until gVisor is fully
rolled out — `gvisor.enabled=true`, the gVisor node pool exists, **and** every
agent role/project sets `runtime_class: runsc` (see
[`gvisor-runsc-prod.md`](gvisor-runsc-prod.md)). Flipping it to Enforce before
that would **deny every agent pod not yet migrated onto runsc**. Keep it as a
report-only backstop that measures migration progress until the substrate is
universal.

## Configuration reference

All keys live under `global.admissionPolicies` in
`gardenio/helm/gardenio/values.yaml` (fully commented there):

```yaml
global:
  admissionPolicies:
    restrictRegistries:
      enabled: false
      validationFailureAction: Audit
      allowedImageRegexes:            # image must match at least one; [.] = literal dot
        - "^registry[.]gitlab[.]com/acme/.*"
    requireResourceLimits:
      enabled: false
      validationFailureAction: Audit
    restrictSecurityContext:
      enabled: false
      validationFailureAction: Audit
    requireRunsc:
      enabled: false
      validationFailureAction: Audit  # keep Audit until gVisor is universal
      runtimeClassName: runsc
```

## Tests

`gardenio/helm/gardenio/tests/admission_starter_policies_test.go` asserts, per
policy: default-OFF renders nothing; enabled renders a `kyverno.io/v1
ClusterPolicy` scoped to the `gardenio.io/agent` label with `Audit` default; the
control-specific fields render; and the `validationFailureAction`, allow-list
regex, and RuntimeClass name are overridable.
