# infra-admin role: design

Task gd-sAMar9tn5B (P6), plan gd-xiV5hyhPrG, decision D2.

## Goal

A role an agent can be granted for full cluster control. A human approves
every spawn. It is never auto-approved. D2: "full cluster control for the
gardenio admin infra role". The per-project namespace-scoped tier (P8, P9)
escalates to this tier for shared platform pieces (P10).

## What the grant includes

The grant is the built-in `cluster-admin` ClusterRole, bound cluster-wide. The
owner should object now if any of this is more than intended:

- **Read of every Secret in every namespace.** That covers the shared beads
  token, the Claude credentials, Keycloak admin and client secrets, GitHub
  credentials and anything else stored as a Secret.
- **RBAC `escalate`, `bind` and `impersonate`.** The holder can create any
  role, bind anyone to it, and act as any user, group or ServiceAccount,
  including the controller's and the release deployer's.
- **Cluster-scoped objects:** nodes (cordon, taint, delete), CRDs,
  validating and mutating webhooks, ValidatingAdmissionPolicies, StorageClasses,
  PersistentVolumes, namespace deletion.
- **Workload control everywhere:** `pods/exec`, `pods/attach`, `pods/portforward`,
  create and delete of every workload kind in every namespace.
- **Disabling the controls below it.** The holder can delete the
  admission policies and NetworkPolicies that bound other identities, for
  example the ha-drill and pv-gc policies.

A compromised infra-admin pod is a compromised cluster. The controls below
reduce the chance and the dwell time; they do not bound the blast radius.
This is why a ValidatingAdmissionPolicy per grant (the original approach in
the plan) was dropped for this role: it cannot bound cluster-admin.

## Design

### 1. Identity

A dedicated ServiceAccount `gardenio-infra-admin` in namespace `gardenio`,
`automountServiceAccountToken: false`, label
`gardenio.io/access-boundary: infra-admin`.

The token reaches a pod only through the role bead (`needs_k8s_api=true`,
`service_account=gardenio-infra-admin`). It is the kubelet-projected bound
token: about one hour, rotated, and invalid once the pod is deleted. There is
no long-lived token Secret.

### 2. Grant: a new template, not `agents.clusterAdmin`

`agents.clusterAdmin` (`values.yaml`, `templates/controller/agent-cluster-rbac.yaml`)
also renders `templates/controller/cluster-rbac.yaml`, which grants the
deployer SA `clusterrolebindings` create and documents per-project opt-in
through project beads. Using it would widen the deployer. It stays off.

`templates/controller/infra-admin-rbac.yaml` is gated on
`agents.enabled && agents.infraAdmin.enabled` (default `false`) and renders
exactly a ServiceAccount and a ClusterRoleBinding to `cluster-admin`, mirroring
`infra-reader-rbac.yaml`. There is no chart-authored ClusterRole, so the grant
cannot drift narrower or wider than the built-in.

### 3. Never auto-approved, structurally

The set `neverAutoApproveRoles = {"infra-admin"}` lives in code
(`kbeads/internal/server/spawn_policy.go`), not in bead data, so no bead write
can add an auto path:

1. `validateSpawnPolicyFields` rejects any `auto` rule on a policy whose `role`
   is in the set (write time, create and update).
2. `evaluateSpawnEscalation` returns without acting for those roles, even if an
   auto rule reached the store some other way, for example a direct SQL write
   or a rule that predates the guard. It also skips the "no rule matched, deny
   and close" branch: the evaluator touches nothing.
3. Approval needs an OIDC human through an approval-capable client
   (`escalationApprover`). `handleEscalation` (`write_authz.go`) does **not**
   stamp `requires_four_eyes` on these escalations: the org has one human
   approver, and agent-filed escalations resolve to that same operator, so a
   stamp could never be satisfied and returned 403 to the owner's own approval
   (gd-TXbrvA9E6F, gd-vjjLRTIy68). The `requires_four_eyes` field and its check
   remain for explicit, policy-driven use: a rule opts in with `four_eyes` (below).

The spawn_policy bead for the role is
`{"role":"infra-admin","rules":[{"when":{},"mode":"human","approvers":["oidc:matthewbaker1110@gmail.com"]}]}`.
`approvers` is documentation; the actual gate is an OIDC human calling approve.

#### Optional four-eyes (`four_eyes`, gd-kvrZdjItZV)

Four-eyes is off by default and configurable per rule. A `mode: "human"` rule may
set `"four_eyes": true`; when it is the first rule matching a spawn request,
`handleEscalation` stamps `requires_four_eyes` on the escalation, so the
requester's own operator cannot approve it and a different approver must.

Because a stamp nobody else can satisfy would just block the escalation, the
rule is validated at **write** time (create and update), never at approval time.
`four_eyes: true` is refused unless the rule's `approvers` list at least two
distinct human operators: entries are deduplicated, `team:` entries do not
count, and aliases that `LookupOperatorAlias` links to one operator row (a Slack
id and an OIDC email of the same person) count once. `four_eyes` on an `auto`
rule is refused too.

This is independent of never-auto: `neverAutoApproveRoles` is code-level and
holds with `four_eyes` unset, false or true. The live `infra-admin` policy keeps
the default (off). The rule shape (a per-rule opt-in flag, validated at write)
is deliberately generic, as a seed for configurable-autonomy policy elsewhere
(merge, release, escalation-type, spend).

Example: `{"when":{},"mode":"human","four_eyes":true,"approvers":["oidc:owner@example.com","oidc:other@example.com"]}`.

### 4. Compensating controls

- Human OIDC approval on every spawn, through an approval-capable client (a browser or Slack session, not a `kd login` device grant). No four-eyes: there is one approver.
- The role bead's resource caps: 500m/1Gi request, 1500m/2Gi limit.
- Spawn provenance annotations on the pod.
- The role bead's description tells the agent to comment every mutating
  command on its task bead.
- Short-lived bound token (see Identity).

**Gap: k3s audit logging is off today.** Without it there is no server-side
record of what the admin identity did; the task-bead comments are
self-reported. This design does not enable audit logging. It is filed as a
follow-up bug (see Follow-ups).

### 5. Narrow roles stay

`infra` (read-only) and `ha-drill-operator` stay, and are preferred when they
are enough. `infra-admin` does not subsume them and nothing should be moved
onto it for convenience.

### 6. Out of scope

- AWS. P11 may add `gardenio-infra-admin` as a second trusted subject for
  read-only AWS.
- Enabling k3s audit logging.

## Refinement found while building: the release deployer

The release deployer (`templates/release-deployer/rbac.yaml`) re-applies the
chart on autonomous rolls. Helm reads every object it owns on each upgrade, so
the deployer needs `get` on the new ClusterRoleBinding, and the chart adds the
binding's name to the deployer's `resourceNames` rule next to the ha-drill
entry.

The deployer does **not** get `bind` or `escalate`. Kubernetes escalation
prevention requires `bind` on `cluster-admin` (or holding every permission) to
create or update a binding that references it. Consequences:

- The deployer cannot use the new name to bind anything, so the entry adds no
  privilege. `delete` of the binding is allowed and only removes access.
- A change to this binding (subjects, rename) is an **operator roll**, never an
  autonomous one. Unchanged, it passes through autonomous rolls untouched.
- `release_deployer_test.go` normally flags any binding to a role the chart
  does not render. `gardenio-infra-admin` is exempted by exact name
  (`operatorOnlyClusterAdminBindings`); a planted cluster-admin binding under
  any other name is still reported. `TestInfraAdmin_ReleaseDeployerCannotBindClusterAdmin`
  pins that the deployer ClusterRole carries no `bind`, `escalate`,
  `impersonate` or wildcard.

## Rollout

Binding order matters. Nothing below runs from a crew pod; the operator
(laptop-claude) opens a window and rolls.

1. Merge and roll the **kbeads guard**. It is inert until the role and policy
   beads exist.
2. Confirm **P12** (gd-Gpvi7wld87, agent-chosen ServiceAccount) is merged
   **and rolled**. Until it is, any agent bead can name any ServiceAccount, so
   binding cluster-admin to one would be reachable without the role bead.
3. Merge and roll the **chart PR** (`agents.infraAdmin.enabled: true` in
   `deploy-values/gardenio-k3s.yaml`). Check:
   `kubectl auth can-i '*' '*' --as=system:serviceaccount:gardenio:gardenio-infra-admin`
   prints `yes`, and the same for `gardenio-agent` prints `no`.
4. Write the **role bead** `infra-admin` by guarded SQL, with an AUDIT RECORD
   comment on this task: `privileged=true`, `needs_k8s_api=true`,
   `service_account=gardenio-infra-admin`, `cpu_request=500m`,
   `cpu_limit=1500m`, `memory_request=1Gi`, `memory_limit=2Gi`, labels
   `privileged-role`, `project:gardenio`.
5. Write the **spawn_policy bead** above.
6. P13 proves the path live.

**Rollback:** set `agents.infraAdmin.enabled: false` and roll (the binding is
deleted), then close the role bead. Reverting the kbeads guard is not needed.

## Follow-ups

- Enable k3s API server audit logging (bug, filed against this task).
- P13: live end-to-end test of the spawn, approval and revoke path.
