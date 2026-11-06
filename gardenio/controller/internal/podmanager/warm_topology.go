package podmanager

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// placeholderSleepCommand is the passive sleep shell every gVisor placeholder
// runs — both the create-path Pod B (buildPlaceholderPodB) and the warm-B pool
// placeholder. It ensures /work/repo exists (the restore repoints into /work)
// then sleeps, so the container holds the kubelet-owned bindings without racing
// a future graft. Kept as a single constant so the two builders never drift.
var placeholderSleepCommand = []string{"/bin/sh", "-c", "mkdir -p /work/repo 2>/dev/null; exec sleep 86400"}

// placeholderCPURequestFloor / placeholderMemRequestFloor are the tiny compute
// REQUESTS a sleep-shelled placeholder container reserves at admission time
// (ka-0000008044). They match the legacy bare-placeholder floor
// (defaultWarmPlaceholderResources) and the cheap admission floor MR !4383
// (ka-0000000239) used to keep a node warm.
//
// Why we floor REQUESTS but keep LIMITS (the structural fix for the R4-graft gap):
// the warm-B bucket is node-pinned (the checkpoint image is node-local), so a
// refreeze/thaw snapshot's placeholder lands on the SAME node already running
// that agent's live 2-CPU pod. If the placeholder inherited the template's full
// 2-CPU REQUEST it could never be admitted there (kubelet OutOfcpu) — the exact
// storm MR !4383 had to suppress, which left the ~321ms R4 warm-graft path
// structurally unavailable for the case it exists to serve. Flooring the request
// makes the sleeping placeholder admissible alongside the live agent; the agent
// is being refrozen, so its 2-CPU slot returns to the node, and the grafted
// agent bursts into it. runsc restore grafts the checkpointed process tree onto
// the kubelet-owned sandbox cgroup — RestoreValidateSpec keys mounts by
// Destination only and does NOT require request-equality (freezethaw_node.go
// [3]), so the sandbox's request need not equal the agent's steady-state use.
//
// LIMITS are deliberately preserved from the template. Pod resources are
// immutable and ApplyWarmPlaceholderPromotion only rewrites labels/annotations —
// so whatever ceiling the placeholder is minted with is the ceiling the grafted
// agent runs under forever. Keeping the template's limit (4 CPU / 16Gi for a
// default agent) means the woken agent retains its real burst envelope; only the
// guaranteed reservation is relaxed, yielding a Burstable pod that fits at mint
// time and bursts after the live slot frees.
//
// MEMORY-FENCE OVERRIDE (ka-0000000088): CPU stays at the 100m floor forever —
// the WHY above is untouched. Memory, however, needs to hold a scheduler fence
// over the RAM the freeze just released, or a backfill pod consumes it, the
// pinned same-node thaw arrives to no headroom, and the eager runsc restore
// OOM-SIGKILLs (canary #12: c6a.large, 3.05Gi allocatable, ~660Mi backfill,
// 3.0G pages.img). The refreeze ordering makes the fence safely admittable
// exactly when it matters: freeze COMPLETES → subject's ~2Gi returns to the
// node → placeholder mint fires with mem-request set to max(256Mi, PagesBytes)
// → placeholder holds the freed RAM until the thawing subject is grafted onto
// it. On a first-freeze snapshot with PagesBytes=0 (legacy / capture-failure)
// the request falls back to the 256Mi floor and nothing regresses.
var (
	placeholderCPURequestFloor = resource.MustParse("100m")
	placeholderMemRequestFloor = resource.MustParse("256Mi")
)

// placeholderMemRequestFor returns the memory REQUEST a warm-B placeholder's
// sleep-shelled container should reserve (ka-0000000088). It is
// max(placeholderMemRequestFloor, pagesBytes) — pagesBytes is the size of the
// runsc pages.img the eager restore will consume, captured at freeze
// (Snapshot.PagesBytes). Zero (legacy snapshot / capture failure) collapses to
// the floor so nothing regresses; a real value fences the freed RAM against
// backfill until the graft lands.
func placeholderMemRequestFor(pagesBytes int64) resource.Quantity {
	if pagesBytes <= 0 {
		return placeholderMemRequestFloor.DeepCopy()
	}
	pages := resource.NewQuantity(pagesBytes, resource.BinarySI)
	if pages.Cmp(placeholderMemRequestFloor) < 0 {
		return placeholderMemRequestFloor.DeepCopy()
	}
	return *pages
}

// floorPlaceholderRequests relaxes a sleep-shelled placeholder container's CPU
// down to the small admission floor while setting its memory REQUEST to the
// pages-bytes-aware quantity computed by placeholderMemRequestFor. LIMITS are
// left untouched so the woken agent retains its burst ceiling. Only call this
// for containers that run the passive sleep shell; containers doing real work
// (init-clone) keep their real requests.
//
// CPU: request is only lowered when the template's request exceeds the floor —
// a template that already requests less keeps its smaller value.
//
// Memory (ka-0000000088): request is set to max(256Mi, pagesBytes) SO THAT it
// fences the RAM the freeze released, preventing a backfill pod from consuming
// it before the same-node thaw graft lands. This is a REGRESSION of the
// ka-0000008044 floor for containers whose template requested more than
// pagesBytes — the pre-fix behavior floored to 256Mi regardless. That regression
// is intentional and load-bearing: without the fence, canary #12's structural
// OOM re-occurs. The refreeze ordering (freeze completes → subject RAM
// released → placeholder mint) guarantees the fence is admittable at mint time.
func floorPlaceholderRequests(c *corev1.Container, pagesBytes int64) {
	if c.Resources.Requests == nil {
		// No template requests to floor. Pre-ka-0000000088 behavior was a no-op
		// here (a nil Requests kept the pod at BestEffort). Preserve that
		// exactly for containers that had no explicit request — a placeholder
		// minted from a BestEffort template stays BestEffort so the pre-fix
		// no-op path (and warm_topology_test's Nil-Requests case) is unchanged.
		return
	}
	if cpu, ok := c.Resources.Requests[corev1.ResourceCPU]; ok && cpu.Cmp(placeholderCPURequestFloor) > 0 {
		c.Resources.Requests[corev1.ResourceCPU] = placeholderCPURequestFloor.DeepCopy()
	}
	// Memory: always overwrite to the pages-bytes-aware request so the
	// placeholder fences the freed RAM even when the template requested less
	// than pages.img size. The floor path (pagesBytes <= 0) collapses to 256Mi,
	// matching the pre-fix behavior exactly.
	if _, hasMem := c.Resources.Requests[corev1.ResourceMemory]; hasMem {
		c.Resources.Requests[corev1.ResourceMemory] = placeholderMemRequestFor(pagesBytes)
	} else if pagesBytes > 0 {
		// Template omitted a memory request but this snapshot HAS a PagesBytes:
		// set one so the scheduler fence engages. Skip for pagesBytes<=0 to
		// preserve the BestEffort-shape (a template with cpu-req + no mem-req)
		// as-is when we have no useful number to write.
		c.Resources.Requests[corev1.ResourceMemory] = placeholderMemRequestFor(pagesBytes)
	}
}

// raisePlaceholderMemLimit RAISES a sleep-shelled app container's memory LIMIT to
// cover the runsc restore peak (ka-0000003398 / bug ka-0000000413). The
// create-then-restore path DeepCopies the frozen Pod A template and floors only
// REQUESTS (floorPlaceholderRequests, above) — it NEVER touched LIMITS — so Pod B
// inherited the agent's role memory LIMIT (e.g. gvisor-poc's 8Gi). Canary #15
// proved that limit is the binding cgroup: `runsc restore` runs inside Pod B's
// CONTAINER-scope cgroup (cgroup-v2 memory.max), and the ROOT-sandbox restore of a
// 2.96Gi pages.img peaked >8Gi = >2.71× the image and was cgroup-OOM-SIGKILLed
// against that 8Gi ceiling — even though the node had 14.4Gi free. Raising the
// LIMIT to modeledRestorePeakLimit(pages, cc) gives the restore process cgroup
// headroom to reconstruct the pages.img without tripping memory.max.
//
// Rules (all load-bearing):
//   - RAISE-ONLY: newLimit = max(existingLimit, modeled). A template that already
//     carries a limit fatter than the model (e.g. a 16Gi burst envelope) keeps it
//     — we never SHRINK the woken agent's ceiling.
//   - NEVER IMPOSE on an unbounded container: a container with no memory limit has
//     NO cgroup ceiling to OOM against (only the node can OOM it, which the
//     pre-thaw node mem-gate covers), so leave it unbounded. Imposing a limit
//     where none existed would only ADD a constraint the restore could trip.
//   - ADMISSION-SAFE: whenever a limit is set/raised, ensure a memory REQUEST
//     exists. K8s defaults request=limit when a container has a limit but no
//     request; without a fence request the raised ~10Gi limit would become a
//     ~10Gi RESERVATION and make the node-pinned placeholder un-admittable
//     alongside the live agent (the exact storm floorPlaceholderRequests exists
//     to avoid). Setting the pages fence (max(256Mi, pages)) keeps the pod
//     Burstable: request fences the freed RAM, the fatter limit is only the
//     restore-window ceiling. floorPlaceholderRequests normally sets this already;
//     this is the belt-and-braces for a BestEffort template (nil Requests).
//   - pages <= 0 (legacy / capture failure) is a NO-OP: modeledRestorePeakLimit
//     returns 0, so max(existing, 0) == existing — nothing regresses.
func raisePlaceholderMemLimit(c *corev1.Container, pagesBytes int64, containerCount int) {
	existing, bounded := c.Resources.Limits[corev1.ResourceMemory]
	if !bounded {
		// Unbounded container: no cgroup memory.max ceiling to OOM against. Leave
		// it unbounded — the node mem-gate is the backstop for node-level OOM.
		return
	}
	modeled := modeledRestorePeakLimit(pagesBytes, containerCount)
	if modeled <= 0 {
		return // INDETERMINATE pages — nothing to raise to.
	}
	want := resource.NewQuantity(modeled, resource.BinarySI)
	if existing.Cmp(*want) >= 0 {
		return // template limit already >= modeled peak — never shrink it.
	}
	c.Resources.Limits[corev1.ResourceMemory] = *want
	// Ensure a memory request exists so K8s does not default request=limit and
	// turn the raised ceiling into a huge reservation. Only fill a GAP — never
	// overwrite a request floorPlaceholderRequests already set to the fence.
	if c.Resources.Requests == nil {
		c.Resources.Requests = corev1.ResourceList{}
	}
	if _, has := c.Resources.Requests[corev1.ResourceMemory]; !has {
		c.Resources.Requests[corev1.ResourceMemory] = placeholderMemRequestFor(pagesBytes)
	}
}

// shapePlaceholderSpec turns a snapshot's preserved Pod A template into a
// placeholder PodSpec the kubelet can admit to Ready and a checkpoint can be
// grafted onto. It is the single source of truth for the SPEC shape both
// placeholder builders share (create-path buildPlaceholderPodB and the warm-B
// pool's BuildWarmPlaceholderFromSnapshot), so the warm placeholder is
// guaranteed mount- and topology-identical to what the create-path would have
// minted for the same snapshot (ka-0000002219):
//
//   - pin to the snapshot node (the checkpoint image is node-local);
//   - runsc RuntimeClass, RestartPolicy=Never;
//   - override every app container command with a sleep shell so the live body
//     never races the restore;
//   - keep run-to-completion init containers (init-clone) UNCHANGED so the
//     workspace is re-hydrated with the repo before the graft (C6 strategy b),
//     closing the "empty repo" gap for the warm path too;
//   - keep always-on native sidecars (DinD/Postgres) so the kubelet provisions
//     their container slots for the N-container barrier restore (ka-0000003561),
//     but override their command with the same sleep shell.
//
// pagesBytes is the size of the runsc pages.img the eager restore will
// consume (Snapshot.PagesBytes, ka-0000000088). It is threaded through so the
// sleeping placeholder's memory REQUEST fences the freed subject RAM against
// backfill until the same-node graft lands. Because pages.img is a SINGLE
// sandbox-wide checkpoint (one restore mapping for the whole sentry, not a
// per-container quantity) and the k8s scheduler sums every container's request
// into one pod-level reservation, the fence is carried ONCE on the app
// container; the always-on sidecars floor to 256Mi so a multi-sidecar
// (DinD/Postgres) pod fences pagesBytes total, not (1+N)×pagesBytes — which
// would over-reserve past what the freeze released and miss same-node-thaw
// admission (ka-0000005097). Zero collapses to the 256Mi admission floor —
// nothing regresses on legacy snapshots.
//
// containerCount is the snapshot's barrier target (Snapshot.ContainerCount:
// sandbox root + every sub-container). It sizes the per-container memory LIMIT
// raise (raisePlaceholderMemLimit) — the restore peak scales with how many
// containers the sentry reconstructs — floored to the bare-agent baseline inside
// modeledRestorePeakLimit so a legacy zero-count snapshot never under-sizes the
// ceiling.
//
// It mutates nothing the caller owns: the template is deep-copied first.
func shapePlaceholderSpec(template corev1.PodSpec, node string, pagesBytes int64, containerCount int) corev1.PodSpec {
	spec := *template.DeepCopy()
	spec.NodeName = node
	rc := RuntimeClassRunsc
	spec.RuntimeClassName = &rc
	spec.RestartPolicy = corev1.RestartPolicyNever

	for i := range spec.Containers {
		spec.Containers[i].Command = placeholderSleepCommand
		spec.Containers[i].Args = nil
		spec.Containers[i].LivenessProbe = nil
		spec.Containers[i].ReadinessProbe = nil
		spec.Containers[i].StartupProbe = nil
		// Relax the sleeping app container's CPU REQUEST to the admission
		// floor so the node-pinned placeholder fits alongside the live agent it
		// will be grafted for (ka-0000008044); set MEMORY REQUEST to
		// max(256Mi, pagesBytes) so the placeholder fences the freed subject
		// RAM (ka-0000000088).
		floorPlaceholderRequests(&spec.Containers[i], pagesBytes)
		// RAISE the app container's memory LIMIT to cover the runsc restore peak
		// so the eager pages.img reconstruction does not cgroup-OOM-SIGKILL against
		// an inherited role limit (ka-0000003398 / bug ka-0000000413). Raise-only,
		// never imposed on an unbounded container; a no-op when pages is unknown.
		raisePlaceholderMemLimit(&spec.Containers[i], pagesBytes, containerCount)
	}

	keep, sidecars := partitionInitContainers(spec.InitContainers)
	for i := range sidecars {
		sidecars[i].Command = placeholderSleepCommand
		sidecars[i].Args = nil
		sidecars[i].LivenessProbe = nil
		sidecars[i].ReadinessProbe = nil
		sidecars[i].StartupProbe = nil
		// Always-on sidecars (DinD/Postgres/ziti-tunnel) floor CPU AND memory to
		// the small admission floor — they do NOT each carry the pages fence
		// (ka-0000005097). pages.img is a SINGLE sandbox-wide checkpoint that the
		// eager restore maps ONCE for the whole sentry, so pagesBytes is the
		// restore's total RAM need for the pod, not a per-container quantity. The
		// k8s scheduler sums every container's memory request into one pod-level
		// reservation, so carrying the full pagesBytes on the app container above
		// already fences the whole freed envelope; adding it to each sidecar too
		// would fence (1+N)×pagesBytes and, on a DinD/Postgres-tier pod, push the
		// placeholder's total request above what the subject freeze actually
		// released — a same-node-thaw admission MISS that forces the cold start
		// the warm path exists to avoid. Passing 0 collapses placeholderMemRequestFor
		// to the 256Mi floor (the pagesBytes<=0 path), keeping each sleeping
		// sidecar admissible without double-counting the sandbox's pages.
		floorPlaceholderRequests(&sidecars[i], 0)
	}
	// Run-to-completion init containers (init-clone) are NOT floored: init-clone
	// runs its real clone/fetch before the graft and keeps its own modest request.
	spec.InitContainers = append(keep, sidecars...)
	return spec
}

// SnapshotTopologyKey returns the topology bucket key for a snapshot — a short,
// stable hash of everything that makes a warm placeholder a VALID graft target
// for the snapshot's checkpoint:
//
//   - the full mount-destination set of every app container and native sidecar
//     (runsc RestoreValidateSpec keys mounts by Destination and requires the
//     checkpoint's set to match Pod B exactly — POC fix #2 / ka-0000002219
//     gap 1); two snapshots with the same destination set are graft-compatible;
//   - the init-container / sidecar partition (init-clone presence + which
//     sidecars are always-on), so a placeholder presents the same kubelet
//     container slots the N-container barrier restore expects (ka-0000003561);
//   - the repo identity baked into the init-clone command (project / git URL /
//     branch / reference repos), so a placeholder pre-clones the RIGHT repo
//     before the graft (ka-0000002219 gap 2). Agent-specific git-identity lines
//     are normalized out so same-project agents share a bucket.
//
// Both the pool fill path (which hashes an existing snapshot's template) and the
// wake path (which hashes the thawing agent's snapshot) compute this key, so a
// placeholder is only ever handed to an agent whose checkpoint it can actually
// accept.
func SnapshotTopologyKey(snap Snapshot) string {
	return PodTemplateTopologyKey(snap.PodTemplate)
}

// PodTemplateTopologyKey computes the topology key from a pod template directly.
// Exported so tests and callers without a full Snapshot can bucket by shape.
func PodTemplateTopologyKey(spec corev1.PodSpec) string {
	var sb strings.Builder

	// App containers: name + sorted mount destinations.
	appShape := containerShapes(spec.Containers)
	sort.Strings(appShape)
	sb.WriteString("app=")
	sb.WriteString(strings.Join(appShape, ";"))

	// Init containers, partitioned: run-to-completion (init-clone) vs always-on
	// sidecars. The partition is part of the shape — a placeholder must present
	// the same kubelet container slots.
	keep, sidecars := partitionInitContainers(spec.InitContainers)
	keepShape := containerShapes(keep)
	sort.Strings(keepShape)
	sb.WriteString("|init=")
	sb.WriteString(strings.Join(keepShape, ";"))
	sideShape := containerShapes(sidecars)
	sort.Strings(sideShape)
	sb.WriteString("|sidecar=")
	sb.WriteString(strings.Join(sideShape, ";"))

	// Repo identity: the init-clone command (project / git URL / branch /
	// reference repos) with agent-specific identity lines normalized out, so two
	// agents in the same project+role share a bucket but a different repo does
	// not. Empty when no init-clone is present (no repo to warm).
	sb.WriteString("|repo=")
	sb.WriteString(repoSignature(spec.InitContainers))

	// DNS shape (ka-0000004024): the restore repoints the restored sandbox's
	// /etc/resolv.conf onto the CLAIMED placeholder's OWN kubelet-generated file,
	// which the kubelet synthesizes from the placeholder pod's DNSPolicy +
	// DNSConfig. A placeholder minted from a snapshot whose template carries a
	// DIFFERENT DNS search domain (DNSConfig.Searches — e.g. the cross-namespace
	// "<release-ns>.svc.cluster.local" appended for a gVisor-routed agent, bug
	// ka-0000001058) therefore hands the graft the WRONG resolv.conf. Fold the DNS
	// shape into the bucket key so a checkpoint is only ever grafted onto a member
	// whose resolv.conf search domains + policy match what the frozen agent ran
	// with — DNS is resolved correctly FROM the claimed member, not silently
	// inherited from a differently-shaped placeholder.
	sb.WriteString("|dns=")
	sb.WriteString(dnsSignature(spec))

	sum := sha256.Sum256([]byte(sb.String()))
	return hex.EncodeToString(sum[:])[:16]
}

// dnsSignature renders a pod's DNS resolution shape — everything the kubelet
// bakes into the pod's /etc/resolv.conf — as a deterministic fingerprint:
// DNSPolicy, plus the DNSConfig nameservers, search domains, and options when a
// DNSConfig is present. Search domains and nameservers are order-significant in
// resolv.conf, so they are NOT sorted; options are sorted (kube emits them
// order-independently as name[:value] pairs). Empty DNSConfig collapses to just
// the policy, so a legacy/default-DNS snapshot keeps a stable, shared key.
func dnsSignature(spec corev1.PodSpec) string {
	var sb strings.Builder
	sb.WriteString("policy=")
	sb.WriteString(string(spec.DNSPolicy))
	if c := spec.DNSConfig; c != nil {
		sb.WriteString("|ns=")
		sb.WriteString(strings.Join(c.Nameservers, ","))
		sb.WriteString("|search=")
		sb.WriteString(strings.Join(c.Searches, ","))
		opts := make([]string, 0, len(c.Options))
		for _, o := range c.Options {
			if o.Value != nil {
				opts = append(opts, o.Name+":"+*o.Value)
			} else {
				opts = append(opts, o.Name)
			}
		}
		sort.Strings(opts)
		sb.WriteString("|opt=")
		sb.WriteString(strings.Join(opts, ","))
	}
	return sb.String()
}

// containerShapes renders each container as "name:dst1,dst2,..." with mount
// destinations sorted, for a deterministic, order-independent shape fingerprint.
func containerShapes(cs []corev1.Container) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		dsts := make([]string, 0, len(c.VolumeMounts))
		for _, m := range c.VolumeMounts {
			dsts = append(dsts, m.MountPath)
		}
		sort.Strings(dsts)
		out = append(out, c.Name+":"+strings.Join(dsts, ","))
	}
	return out
}

// repoSignature extracts a repo-identity fingerprint from the init-clone
// container's command. The init-clone script embeds the project, git URL,
// branch, and reference repos verbatim, so hashing it (minus the per-agent git
// identity lines) yields a key that is identical for same-project agents and
// distinct across repos. Returns "" when there is no init-clone container.
func repoSignature(inits []corev1.Container) string {
	for _, c := range inits {
		if c.Name != InitCloneName {
			continue
		}
		// Command is ["/bin/sh","-c",<script>]; hash the normalized script.
		script := ""
		if len(c.Command) > 0 {
			script = c.Command[len(c.Command)-1]
		}
		sum := sha256.Sum256([]byte(normalizeInitCloneScript(script)))
		return hex.EncodeToString(sum[:])[:12]
	}
	return ""
}

// normalizeInitCloneScript strips the per-agent git-identity lines from an
// init-clone script so two agents that differ ONLY by agent name (same project,
// git URL, branch, reference repos) produce the same repo signature and share a
// warm-pool bucket. Everything that determines repo CONTENT is preserved.
func normalizeInitCloneScript(script string) string {
	lines := strings.Split(script, "\n")
	kept := lines[:0]
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "git config user.name ") ||
			strings.HasPrefix(t, "git config user.email ") {
			continue
		}
		kept = append(kept, ln)
	}
	return strings.Join(kept, "\n")
}
