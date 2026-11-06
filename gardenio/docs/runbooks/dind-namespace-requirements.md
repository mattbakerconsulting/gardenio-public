# DinD-enabled projects: namespace requirements

Project beads with `docker: true` cause the controller to attach a
Docker-in-Docker (DinD) sidecar to every agent pod
(`gardenio/controller/internal/podmanager/dind.go`). The sidecar runs
`docker:dind` with `securityContext.privileged=true`, `runAsUser=0`, and
`runAsNonRoot=false`. Pod Security Admission (PSA) profiles `baseline`
and `restricted` reject this securityContext at admission time.

The default fleet PSA profile is `restricted`
(`gardenio/controller/cmd/gb/fleet.go::psaProfile`), and the gardenio
prod / gardenio-rwx namespaces default to `enforce=baseline` since
[59165e9a]. **A `docker: true` project will not admit pods in those
namespaces without an explicit opt-in.**

[59165e9a]: ../../../gardenio/CHANGELOG-related — bead ka-0000003329

## Symptom

Without the controller-side preflight, you would see a Pod create call
that succeeds against the apiserver but is then rejected by the PSA
admission webhook with a message like:

```
violates PodSecurity "baseline:latest": privileged (container "dind"
must not set securityContext.privileged=true)
```

With the preflight in place
(`podmanager.K8sManager.checkDindAdmittable`), the controller instead
fails fast on `CreateAgentPod` with:

```
dind preflight: namespace "gardenio" has pod-security.kubernetes.io/enforce="baseline"
which rejects the privileged DinD sidecar; set GARDENIO_PSA_PROFILE=privileged
on the controller before creating the fleet namespace, or relabel the
namespace with pod-security.kubernetes.io/enforce=privileged.
See docs/runbooks/dind-namespace-requirements.md for the opt-in path
```

## Opt-in path (option C)

Two ways to relax PSA so a fleet namespace can host DinD agents.

### A. New fleet — set `GARDENIO_PSA_PROFILE=privileged` before `gb fleet create`

`gb fleet create` stamps every namespace with the value of
`GARDENIO_PSA_PROFILE` on `pod-security.kubernetes.io/enforce|audit|warn`.
For a fleet that needs `docker: true` projects, set the env on the
controller (or on the `gb fleet create` invocation) BEFORE the
namespace is created:

```bash
GARDENIO_PSA_PROFILE=privileged gb fleet create my-dind-fleet
```

This is the cleanest path: the namespace is born with the right labels
and the audit/warn surfaces match.

### B. Existing namespace — relabel it

If the fleet namespace already exists and you need to admit a DinD pod
in it, relabel the namespace:

```bash
kubectl label namespace <ns> \
  pod-security.kubernetes.io/enforce=privileged \
  pod-security.kubernetes.io/audit=privileged \
  pod-security.kubernetes.io/warn=privileged \
  --overwrite
```

Note that this widens the security posture of the WHOLE namespace.
Every workload in `<ns>` will admit privileged pods after this change,
not just gardenio agents. Prefer option A (a dedicated DinD fleet) when
the trust boundary matters.

## Why we did not pick option A or B from the bug

The bug (ka-0000003990) listed four options:

- **(a)** Per-project namespace with PSA=privileged for `docker=true`
  projects — controller emits namespace + PSA labels at create time.
  Rejected for now: no live `docker=true` projects on gardenio-rwx or
  gardenio (`gb` projects audited 2026-04-29). Adding a per-project
  namespace lifecycle for a non-existent consumer is premature.
- **(b)** Make DinD use a less-privileged shim (rootless docker, sysbox,
  kaniko). Rejected for now: significant compatibility risk for any
  future `docker: true` consumer; revisit if option C proves
  insufficient.
- **(c)** ✅ Document `docker: true` as requiring
  `GARDENIO_PSA_PROFILE=privileged` and add a controller preflight.
  Picked because it surfaces the failure with an actionable message and
  costs nothing to keep until a real DinD consumer appears.
- **(d)** Drop `docker: true` entirely. Rejected: the pod plumbing,
  helm config plumbing, and tests are already in place; the cost of
  removing them and adding them back later exceeds the cost of leaving
  the codepath behind a documented opt-in.

If a real DinD consumer materializes and option C's "widen the whole
namespace" tradeoff becomes painful, file a follow-up to revisit
option (a) or (b).

## Controller preflight

The preflight lives in
`gardenio/controller/internal/podmanager/dind.go::K8sManager.checkDindAdmittable`
and runs at the top of `CreateAgentPod` whenever `spec.Docker == true`.
It looks up the target namespace via the K8s API and:

- returns `nil` when `enforce=privileged` or the label is absent;
- returns an actionable error when `enforce` is `baseline` or
  `restricted`;
- logs a warning and allows pod creation when `enforce` has an unknown
  value (so a typo does not block deploys);
- logs a warning and allows pod creation when the namespace lookup
  itself fails (so a transient apiserver hiccup does not break
  non-DinD agents in the same controller run — the cluster's own
  admission denial will still surface from the Pod create call).

Tests: `gardenio/controller/internal/podmanager/dind_test.go::TestCheckDindAdmittable`
and `TestCreateAgentPod_DockerInBaselineNamespace`.
