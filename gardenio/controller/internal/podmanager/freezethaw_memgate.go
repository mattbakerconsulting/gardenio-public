package podmanager

// freezethaw_memgate.go — the pre-thaw memory gate and the restore-need and restore-peak models it uses.
// Split out of freezethaw.go (gd-Wl6LuCnvlH); see freezethaw.go for the design overview.

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ThawInsufficientMemoryError is the typed pre-thaw memory-gate failure
// (ka-0000000088). The gate probes the pinned node's MemAvailable BEFORE
// creating Pod B and compares it against Snapshot.PagesBytes + a safety
// margin; on shortfall it returns this error so the caller's cold-start
// fallback fires in milliseconds instead of paying ~7s + a create+delete +
// possible kernel OOM. Retryable() is intentionally FALSE — the placeholder
// is node-pinned to the snapshot's own node and MemAvailable will not
// meaningfully improve on the retry timescale, so a retry is deterministic
// waste. The caller's error branch (freezeThawLifecycle) falls through to
// main.go's normal cold-start path, which is always safe: the snapshot is
// left INTACT so a later reconciler pass (once the node has headroom, or
// after node roll) can still serve the thaw hot path.
type ThawInsufficientMemoryError struct {
	Node       string
	PagesBytes int64
	MemAvail   int64
	Margin     int64
	// ModeledNeed is the fattened restore-peak estimate the gate compared
	// against MemAvail (ka-0000007579): PagesBytes×restore-multiplier +
	// sentry/gofer overhead×max(ContainerCount, baseline) + Margin. The gate
	// refuses when MemAvail < ModeledNeed. Carried on the error so controller
	// logs show the whole model, not just the raw pages.img size the OLD
	// (pages+margin) gate reported — the entire point of the fatten is that
	// pages.img+margin UNDER-modeled the real restore peak RSS (page-image
	// reconstruction into the restored sentry + sentry/gofer process overhead +
	// the sub-container working-set + RAM held by co-scheduled warm-b siblings),
	// so canary #14 (2026-07-04) passed the OLD gate with 2.7G modeled headroom
	// and then OOM-SIGKILLed the agent-sub restore anyway (rc=137).
	ModeledNeed int64
	// ContainerCount is the barrier target (sandbox root + subs) the overhead
	// term was multiplied by, after clamping to the bare-agent baseline.
	// Diagnostic only.
	ContainerCount int
	// PodCgroupLimit is the Pod B CONTAINER-scope cgroup memory LIMIT the gate
	// modeled as the SECOND binding constraint (ka-0000003398 / bug ka-0000000413):
	// the gate now refuses when min(MemAvail, PodCgroupLimit) < ModeledNeed. On the
	// create path this is the limit shapePlaceholderSpec WILL mint (belt-and-braces,
	// always >= ModeledNeed by construction); on the warm-B graft path it is the
	// tightest bounded memory limit read from the ACTUAL supplied placeholder pod,
	// so a stale pre-fix 512Mi pool pod is caught here and cold-started rather than
	// driven into a doomed cgroup-OOM restore. Zero means "unbounded / unknown" (no
	// cgroup ceiling — MemAvail alone bound the decision). LimitBound reports which.
	PodCgroupLimit int64
	// LimitBound is true when PodCgroupLimit (not MemAvail) was the binding
	// constraint that triggered the refusal — i.e. the node had headroom but the
	// Pod B cgroup limit did not. Diagnostic only; lets a controller log
	// distinguish the canary #15 shape (fat node, tight cgroup) from the canary
	// #12/#14 shape (tight node).
	LimitBound bool
	// Fallback is true when PagesBytes was resolved from the node-side stat
	// (legacy snapshot without persisted PagesBytes). Diagnostic only —
	// preserved in the message so a stat-vs-snapshot mismatch shows up in
	// controller logs without needing a rerun.
	Fallback bool
}

func (e *ThawInsufficientMemoryError) Error() string {
	src := "snapshot"
	if e.Fallback {
		src = "node-stat"
	}
	limitDesc := "unbounded"
	if e.PodCgroupLimit > 0 {
		limitDesc = fmt.Sprintf("%d bytes", e.PodCgroupLimit)
	}
	// Attribute the binding constraint so a controller log can tell the canary #15
	// shape (fat node, tight Pod B cgroup) apart from canary #12/#14 (tight node).
	bind, oomKind := "node MemAvailable", "kernel"
	if e.LimitBound {
		bind, oomKind = "Pod B cgroup memory limit", "cgroup"
	}
	return fmt.Sprintf(
		"thaw: pre-thaw memory gate failed on node %s: modeled restore peak=%d bytes (pages.img=%d (%s) × restore-multiplier + sentry/gofer overhead × %d containers + margin=%d) exceeds min(MemAvailable=%d, PodCgroupLimit=%s) bound by %s — an eager runsc restore would %s-OOM-SIGKILL (rc=137); refusing to create Pod B, cold-starting",
		e.Node, e.ModeledNeed, e.PagesBytes, src, e.ContainerCount, e.Margin, e.MemAvail, limitDesc, bind, oomKind)
}

// Retryable is intentionally false — a same-node retry cannot fix a structural
// memory shortfall (placeholder is node-pinned; MemAvailable will not
// meaningfully improve on retry). The caller falls through to cold-start.
func (e *ThawInsufficientMemoryError) Retryable() bool { return false }

// preThawMemoryGate is the cheap read-only pre-thaw check that refuses a
// restore that would OOM-SIGKILL when runsc eagerly mmaps pages.img on a node
// with insufficient MemAvailable (ka-0000000088). Returns nil to proceed, or
// *ThawInsufficientMemoryError to fail-close into cold start.
//
// Contract:
//   - Node stat / MemAvailable comes from the mem-gate probe script (host
//     namespace via HMNT so a probe-pod cgroup limit is not confused for the
//     node's real free memory).
//   - PagesBytes prefers snap.PagesBytes (freeze-side authoritative capture);
//     falls back to the probe's FT_PAGES_STAT_BYTES for legacy snapshots.
//   - Both PagesBytes AND MemAvailable must be resolvable for the gate to fire.
//   - Unknown PagesBytes ⇒ INDETERMINATE ⇒ proceed (let the barrier restore
//     surface the real error rather than fail-close on a probe artifact).
//   - Unknown MemAvailable ⇒ FAIL-CLOSE (a broken /proc/meminfo must not
//     degrade into "gate-off" and produce the very OOM we're preventing).
//   - Transport failures on the probe script itself are non-fatal (the
//     downstream thaw script will observe the same issues) — logged and
//     proceed.
//
// The refusal threshold is the FATTENED modeled restore-peak
// (modeledRestoreNeed, ka-0000007579), NOT the raw pages.img+margin the gate
// originally used. Canary #14 (2026-07-04) proved pages.img+margin under-models
// real peak: a 3.19G pages.img on a 6.43G-avail node passed the old gate with
// 2.7G modeled headroom and then OOM-SIGKILLed (rc=137) mid-restore, because
// the actual restore RSS is pages.img reconstruction into the restored sentry +
// sentry/gofer process overhead + the sub-container's own restored working-set +
// RAM held by co-scheduled warm-b siblings on the same node — a total that
// empirically exceeded MemAvailable (the observed peak/pages ratio floor was
// MemAvail/pages = 2.01x). The fattened model errs HIGH on purpose: a
// false-negative that cold-starts a thaw the node could actually have served is
// FREE (cold start is exactly today's behaviour), whereas a false-positive that
// admits a restore the node cannot hold OOM-kills mid-flight. So the gate models
// the peak generously and fails-closed to cold start on a tight node.
func (f *FreezeThawManager) preThawMemoryGate(ctx context.Context, snap Snapshot, req ThawRequest) error {
	if f.MemGateDisabled {
		// Test seam: the legacy thaw tests index exec.scripts[0] as the
		// barrier-restore script. Bypass the pre-flight exec so those tests
		// see the same script indices they did pre-ka-0000000088. Production
		// path leaves this false.
		return nil
	}
	if snap.Node == "" {
		// A snapshot with no node cannot be thawed; the barrier restore's
		// existing checks will surface a clearer error. Do not gate here.
		return nil
	}
	script := f.buildMemGateScript(memGateScriptParams{ImagePath: snap.ImagePath})
	res, err := f.exec.Exec(ctx, snap.Node, script)
	if err != nil {
		// Transport failure — the downstream thaw script would fail the same
		// way. Log and proceed so the operator sees the actual node-exec
		// error there, not a paradoxical "memory gate transport failure"
		// masking it.
		f.logger.Warn("thaw: pre-thaw memory gate probe transport failure — proceeding without gate",
			"agent", snap.AgentName, "node", snap.Node, "error", err)
		return nil
	}
	fields := parseSentinels(res.Stdout)
	if fields[keyMemGateOK] != "yes" || res.ExitCode != 0 {
		// The probe script ran but did not emit the OK sentinel — treat as
		// a truncated stdout / partial exec. Proceed rather than fail-close
		// on missing telemetry: the downstream thaw script will fail-close
		// with a real diagnostic if the node is actually broken.
		f.logger.Warn("thaw: pre-thaw memory gate probe did not complete cleanly — proceeding without gate",
			"agent", snap.AgentName, "node", snap.Node, "exitCode", res.ExitCode,
			"stderr", tail(res.Stderr, 200))
		return nil
	}

	memAvail, memOK := int64Field(fields, keyNodeMemAvailBytes)
	pages := snap.PagesBytes
	fallback := false
	if pages <= 0 {
		if statBytes, ok := int64Field(fields, keyPagesStatBytes); ok && statBytes > 0 {
			pages = statBytes
			fallback = true
		}
	}
	if pages <= 0 {
		// INDETERMINATE — no captured PagesBytes and the node-side stat also
		// returned nothing readable. Better to let the barrier restore report
		// the real problem than fail-close on a probe blind spot.
		f.logger.Info("thaw: pre-thaw memory gate skipped — no PagesBytes on snapshot and node-side stat unavailable",
			"agent", snap.AgentName, "node", snap.Node, "memAvail", memAvail)
		return nil
	}
	margin := f.memGateMargin()
	cc := f.memGateContainerCount(snap.ContainerCount)
	need := f.modeledRestoreNeed(pages, cc)

	// SECOND binding constraint (ka-0000003398 / bug ka-0000000413): the Pod B
	// CONTAINER-scope cgroup memory LIMIT. Canary #15 proved node MemAvailable is
	// the WRONG denominator on its own — the node had 14.4Gi free but the runsc
	// restore was cgroup-OOM-SIGKILLed against Pod B's 8Gi container limit. Gate
	// against min(MemAvailable, PodCgroupLimit) so a doomed restore never runs.
	//   - create path (req.PlaceholderPod == ""): model the limit
	//     shapePlaceholderSpec WILL mint. By construction it is >= need
	//     (modeledRestorePeakLimit's 3.0× >= modeledRestoreNeed's 1.5×), so this is
	//     belt-and-braces — it only ever binds if fix A regresses below the model.
	//   - warm-B graft (req.PlaceholderPod set): read the tightest bounded memory
	//     limit off the ACTUAL supplied pod. A stale pre-fix 512Mi pool pod is
	//     caught HERE and cold-started, not driven into a cgroup-OOM restore.
	// cgroupLimit == 0 means "unbounded / unknown" — no cgroup ceiling, MemAvail
	// alone binds (the pre-ka-0000003398 behavior).
	cgroupLimit := f.modeledPodCgroupLimit(ctx, snap, req)

	// binding is the effective ceiling the restore peak must fit under. On an
	// unknown MemAvail (memOK == false) we still fail closed below; use 0 so the
	// min is 0 and the refusal fires. When cgroupLimit is 0 (unbounded) the node
	// avail alone binds.
	binding := memAvail
	if cgroupLimit > 0 && cgroupLimit < binding {
		binding = cgroupLimit
	}
	limitBound := cgroupLimit > 0 && cgroupLimit < memAvail

	if !memOK {
		// Unknown headroom on a known-fat restore — fail closed. A broken
		// /proc/meminfo read must not silently degrade the gate into off.
		return &ThawInsufficientMemoryError{
			Node:           snap.Node,
			PagesBytes:     pages,
			MemAvail:       0,
			Margin:         margin,
			ModeledNeed:    need,
			ContainerCount: cc,
			PodCgroupLimit: cgroupLimit,
			Fallback:       fallback,
		}
	}
	if binding < need {
		return &ThawInsufficientMemoryError{
			Node:           snap.Node,
			PagesBytes:     pages,
			MemAvail:       memAvail,
			Margin:         margin,
			ModeledNeed:    need,
			ContainerCount: cc,
			PodCgroupLimit: cgroupLimit,
			LimitBound:     limitBound,
			Fallback:       fallback,
		}
	}
	// WATCH (ka-0000002024): the modeled need/pages ratio decays toward the bare
	// restore multiplier (1.5×) as pages.img grows and crosses BELOW the ≈2.01×
	// canary #14 empirical OOM floor once the image exceeds ~4GiB. Any restore we
	// ADMIT in that regime is running on less modeled headroom-per-byte than the
	// only OOM we have ever measured required, and the model is calibrated to
	// canary #14's ~3.2GiB image — not to a measured peak at this scale. Surface it
	// so the next >4GiB canary is watched for an rc=137 restore OOM; if one still
	// SIGKILLs, the fix is to raise MemGateRestoreMultiplierPermille. LOG-ONLY: the
	// gate decision is unchanged here (a false cold-start is free; an OOM
	// mid-restore is the bug).
	if ratio := modeledNeedOverPagesPermille(need, pages); ratio > 0 && ratio < empiricalPeakOverPagesFloorPermille {
		f.logger.Warn("thaw: pre-thaw mem-gate ADMITTED a restore whose modeled need/pages ratio decayed below the ≈2.01x canary #14 empirical OOM floor — WATCH this >4GiB pages.img canary for an rc=137 restore OOM (ka-0000002024)",
			"agent", snap.AgentName, "node", snap.Node,
			"pagesBytes", pages, "modeledNeed", need,
			"modeledRatioPermille", ratio,
			"empiricalFloorPermille", empiricalPeakOverPagesFloorPermille,
			"restoreMultiplierPermille", f.memGateRestoreMultiplierPermille(),
			"containerCount", cc, "memAvail", memAvail, "binding", binding)
	}
	f.logger.Info("thaw: pre-thaw memory gate passed",
		"agent", snap.AgentName, "node", snap.Node,
		"pagesBytes", pages, "memAvail", memAvail, "margin", margin,
		"restoreMultiplierPermille", f.memGateRestoreMultiplierPermille(),
		"sentryGoferOverhead", f.memGateSentryOverhead(), "containerCount", cc,
		"podCgroupLimit", cgroupLimit, "binding", binding,
		"modeledNeed", need, "headroom", binding-need, "fallback", fallback)
	return nil
}

// modeledPodCgroupLimit returns the Pod B CONTAINER-scope cgroup memory LIMIT the
// pre-thaw gate treats as the second binding constraint (ka-0000003398 / bug
// ka-0000000413). Returns 0 for "unbounded / unknown" — MemAvailable alone binds.
//
//   - Warm-B graft (req.PlaceholderPod set): read the ACTUAL supplied placeholder
//     pod's tightest bounded app-container memory limit. This is the live cgroup
//     memory.max the restore will run against — the exact ceiling a stale pre-fix
//     512Mi pool pod carries, so it fail-closes to cold start. A Get failure (pod
//     absent / API hiccup) returns 0: the subsequent Step-1 Get surfaces the real
//     error; the gate must not fail-close on a transient probe artifact.
//   - Create path (req.PlaceholderPod == ""): Pod B does not exist yet, so model
//     it EXACTLY as buildPlaceholderPodB will — run the SAME shapePlaceholderSpec
//     over the snapshot template and read the binding limit off the shaped spec.
//     Modeling the real minted spec (rather than an idealized
//     modeledRestorePeakLimit) is strictly safer: it honors an unbounded template
//     (→ 0, no ceiling) and a template whose inherited limit already exceeds the
//     model, so the gate never OVER-states the ceiling and admits a restore the
//     actual pod could not hold. Because the shaper raises every bounded app
//     container to >= modeledRestorePeakLimit >= modeledRestoreNeed, the binding
//     limit here is still >= need — the create-path term stays belt-and-braces.
func (f *FreezeThawManager) modeledPodCgroupLimit(ctx context.Context, snap Snapshot, req ThawRequest) int64 {
	if req.PlaceholderPod == "" {
		// Create path: replay the EXACT shapePlaceholderSpec call buildPlaceholderPodB
		// makes (same snap.PodTemplate / snap.PagesBytes / snap.ContainerCount) and
		// read the binding limit off the shaped spec, so the modeled ceiling is
		// byte-identical to the limit the pod will actually carry. Keying off
		// snap.PagesBytes (NOT the gate's node-stat fallback) is load-bearing: the
		// shaper raises the limit only when snap.PagesBytes>0, so a legacy PagesBytes=0
		// snapshot keeps its inherited template limit — modeling the fatter
		// fallback-derived ceiling here would OVER-state it and admit a restore the
		// real (un-raised) pod could not hold. An unbounded template → 0 (no ceiling).
		shaped := shapePlaceholderSpec(snap.PodTemplate, snap.Node, snap.PagesBytes, snap.ContainerCount)
		limit, bounded := bindingContainerMemLimit(shaped)
		if !bounded {
			return 0 // unbounded template → no cgroup ceiling; MemAvail alone binds.
		}
		return limit
	}
	pod, gerr := f.client.CoreV1().Pods(snap.Namespace).Get(ctx, req.PlaceholderPod, metav1.GetOptions{})
	if gerr != nil {
		// Transient / absent — do not fail-close here; Step 1's own Get reports it.
		f.logger.Warn("thaw: pre-thaw memory gate could not read supplied placeholder for cgroup-limit check — proceeding on node MemAvailable alone",
			"agent", snap.AgentName, "placeholder", req.PlaceholderPod, "error", gerr)
		return 0
	}
	limit, bounded := bindingContainerMemLimit(pod.Spec)
	if !bounded {
		return 0 // no cgroup ceiling on any app container — MemAvail alone binds.
	}
	return limit
}

// bindingContainerMemLimit returns the TIGHTEST bounded memory limit among a
// pod's app containers (spec.Containers) and whether any app container carries a
// bounded limit at all (ka-0000003398). The runsc restore runs inside a
// container-scope cgroup and canary #15's OOM fired against the app (agent)
// container's OWN scope cgroup memory.max — NOT the pod-slice sum — so the min of
// the per-container limits is the ceiling that OOMs first. Taking the MIN (rather
// than the sum) means a limitless init/sidecar cannot mask a tight app-container
// limit. An unbounded container contributes no ceiling (skipped). Returns
// (0, false) when no app container is bounded.
//
// SCOPE (deliberate): only spec.Containers is scanned, NOT the always-on native
// sidecars (DinD/Postgres/ziti-tunnel, which live in spec.InitContainers with
// RestartPolicy=Always). The OOM this fix targets is the ROOT/app restore that
// eagerly maps the single sandbox-wide pages.img — the dominant RSS — and that
// runs in an app container's cgroup. Sub-container restores satisfy the barrier
// but do not re-map the full pages.img, so their per-cgroup peak is far smaller;
// the current always-on sidecars are moreover either unbounded (DinD/Postgres —
// no cgroup ceiling to OOM) or carry a modest fixed limit (ziti-tunnel 256Mi).
// Extending the limit-raise + this gate term to sidecars would also have to avoid
// re-imposing the pages fence the multi-sidecar right-size (ka-0000005097)
// deliberately carries only ONCE — so it is left as a belt-and-suspenders
// follow-up rather than widened here. The canary #15 shape is fully covered by
// the app containers.
func bindingContainerMemLimit(spec corev1.PodSpec) (int64, bool) {
	var tightest int64
	found := false
	for i := range spec.Containers {
		q, ok := spec.Containers[i].Resources.Limits[corev1.ResourceMemory]
		if !ok {
			continue
		}
		v := q.Value()
		if !found || v < tightest {
			tightest, found = v, true
		}
	}
	return tightest, found
}

// memGateMargin returns the effective safety margin the pre-thaw memory gate
// uses. Overridden via MemGateMargin (test seam); a zero or negative value
// falls back to defaultMemGateMargin.
func (f *FreezeThawManager) memGateMargin() int64 {
	if f.MemGateMargin > 0 {
		return f.MemGateMargin
	}
	return defaultMemGateMargin
}

// defaultMemGateMargin is the KERNEL-headroom term of the fattened pre-thaw
// memory model (ka-0000000088 gardener scope: "suggest 512Mi"; one term of the
// ka-0000007579 model). Room for the runsc control-plane process, small
// allocations during the restore, and headroom against the OOM-killer that
// ships kernel-side well before the strict OOM condition. It is ADDED on top of
// the image-scaled restore-multiplier term and the per-container sentry/gofer
// term (see modeledRestoreNeed) — it is no longer the ONLY slack over pages.img,
// which is precisely the under-model that let canary #14 OOM through.
const defaultMemGateMargin int64 = 512 * 1024 * 1024

// memGateRestoreMultiplierPermille returns the effective restore-peak multiplier
// (in PER-MILLE, so 1500 == 1.5×) the gate applies to pages.img. Overridden via
// MemGateRestoreMultiplierPermille (test seam); a value < 1000 (which would model
// the restore as needing LESS than the image it must hold resident — never
// fail-safe) falls back to defaultMemGateRestoreMultiplierPermille.
func (f *FreezeThawManager) memGateRestoreMultiplierPermille() int64 {
	if f.MemGateRestoreMultiplierPermille >= 1000 {
		return f.MemGateRestoreMultiplierPermille
	}
	return defaultMemGateRestoreMultiplierPermille
}

// memGateSentryOverhead returns the effective PER-CONTAINER sentry+gofer overhead
// term. Overridden via MemGateSentryOverhead (test seam); a zero or negative
// value falls back to defaultMemGateSentryOverhead.
func (f *FreezeThawManager) memGateSentryOverhead() int64 {
	if f.MemGateSentryOverhead > 0 {
		return f.MemGateSentryOverhead
	}
	return defaultMemGateSentryOverhead
}

// memGateContainerCount clamps a snapshot's ContainerCount to the bare-agent
// baseline (freezeBaselineContainerCount = 2: pause + agent) so the per-container
// overhead term never UNDER-counts. A legacy snapshot may carry ContainerCount==0
// (the field pre-dated its capture) and the node-stat fallback path has no roster
// at all — treating either as "zero containers" would zero out the overhead term
// and re-open the under-model. The floor keeps the model fail-safe for every
// snapshot shape.
func (f *FreezeThawManager) memGateContainerCount(containerCount int) int {
	if containerCount < freezeBaselineContainerCount {
		return freezeBaselineContainerCount
	}
	return containerCount
}

// modeledRestoreNeed is the FATTENED estimate of a runsc restore's peak resident
// memory (ka-0000007579), the threshold preThawMemoryGate compares against the
// node's MemAvailable. It replaces the original under-model (pages.img + a flat
// 512Mi margin) that passed canary #14 with 2.7G of phantom headroom and then
// OOM-SIGKILLed the agent-sub restore (rc=137).
//
//	need = pages × restoreMultiplier            // image resident + reconstruction/working-set
//	     + sentryGoferOverhead × containerCount // per-restored-container process overhead
//	     + margin                               // kernel / OOM-killer standoff
//
// Why each term:
//   - restoreMultiplier (default 1.5×): `runsc restore` mmaps pages.img EAGERLY —
//     it is resident within seconds — AND rebuilding the sentry MemoryFile from
//     pages_meta.img + pages.img holds reconstruction/working-set transients on
//     top of the bare image. Empirically runsc restore commonly needs ~1.3–1.5×
//     the image; we take the top of that range.
//   - sentryGoferOverhead × containerCount (default 768Mi × containers): each
//     restored container (sandbox root + every sub) brings a runsc sentry (Go
//     runtime + netstack) and a gofer (host-FS proxy) process, plus the
//     sub-container's own restored working-set — costs that scale with the
//     barrier's container count, not with the image size.
//   - margin: unchanged kernel-headroom term (defaultMemGateMargin).
//
// Co-scheduled warm-b siblings (a fourth restore-peak pressure) are absorbed
// INDIRECTLY: MemAvailable already nets out siblings' CURRENT RSS, and the
// generous multiplier + overhead build in headroom for sibling GROWTH between the
// probe and the restore. The gate deliberately does NOT enumerate siblings — that
// would need a live pod-list per thaw, breaking the "one cheap read-only probe"
// contract — and instead over-models the peak so a tight node fails-closed to
// cold start (free) rather than OOM-killing mid-restore (the bug).
//
// For the canary #14 shape (pages≈3.19e9, 2 containers) this yields
// 3.19e9×1.5 + 768Mi×2 + 512Mi ≈ 6.94e9 bytes, which exceeds the observed
// MemAvailable of 6.43e9 → REFUSE (the old pages+512M model computed only
// 3.73e9 and PASSED). Effective peak/pages ratio at the baseline is ≈2.17×,
// above the ≈2.01× floor the OOM itself established.
func (f *FreezeThawManager) modeledRestoreNeed(pages int64, containerCount int) int64 {
	mult := f.memGateRestoreMultiplierPermille()
	overhead := f.memGateSentryOverhead()
	cc := f.memGateContainerCount(containerCount)
	return pages*mult/1000 + overhead*int64(cc) + f.memGateMargin()
}

// defaultMemGateRestoreMultiplierPermille models the restore-peak resident memory
// as 1.5× the pages.img size (see modeledRestoreNeed). Per-mille integer math
// keeps the whole gate in int64 bytes with no float rounding.
const defaultMemGateRestoreMultiplierPermille int64 = 1500

// empiricalPeakOverPagesFloorPermille is the restore peak/pages ratio floor that
// canary #14's OOM-SIGKILL (rc=137, 2026-07-04) established empirically:
// MemAvailable/pages = 6.43e9 / 3.19e9 ≈ 2.01× (ka-0000002024, ka-0000007579).
// That single data point is the only measured lower bound on how much RAM a runsc
// restore of a given pages.img actually needs.
//
// The fattened modeledRestoreNeed's need/pages RATIO decays toward the bare
// multiplier (1.5×) as pages.img grows, because the fixed additive overhead
// (sentryGofer×cc + margin, ≈2Gi at the bare-agent baseline) shrinks relative to a
// larger image. At the bare-agent baseline (cc=2) the modeled ratio crosses BELOW
// this empirical floor once pages.img exceeds ~4GiB — past that point the model
// admits a restore on LESS headroom-per-byte than the only OOM we have ever
// observed required. This is NOT a defect (the model is still strictly fatter than
// the pre-ka-0000007579 one on every input, and a false cold-start is free), but it
// is a WATCH ITEM: the model is calibrated to canary #14's ~3.2GiB image, not to
// any measured peak at >4GiB. See modeledNeedOverPagesPermille + the watch WARN in
// preThawMemoryGate. If the next >4GiB canary still OOM-SIGKILLs at restore, the
// fix is to raise MemGateRestoreMultiplierPermille (the multiplier), not just the
// additive terms.
const empiricalPeakOverPagesFloorPermille int64 = 2010

// defaultMemGateSentryOverhead is the per-restored-container sentry+gofer +
// restored-working-set overhead term (768Mi). At the bare-agent baseline of 2
// containers this contributes 1.5Gi — the slack the old flat-margin model lacked.
const defaultMemGateSentryOverhead int64 = 768 * 1024 * 1024

// modeledRestorePeakLimit is the FATTENED estimate of a runsc restore's peak
// resident memory used to size Pod B's CONTAINER-scope cgroup memory LIMIT
// (ka-0000003398 / bug ka-0000000413). It is the SIBLING of modeledRestoreNeed —
// same pages/overhead/margin shape — but with a FATTER image multiplier
// (defaultRestorePeakLimitMultiplierPermille = 3.0×, vs the node gate's 1.5×):
//
//	limit = pages × restorePeakLimitMultiplier    // image resident + reconstruction transients
//	      + sentryGoferOverhead × containerCount   // per-restored-container process overhead
//	      + margin                                 // kernel / OOM-killer standoff
//
// WHY a SEPARATE, FATTER multiplier than the node gate. The two models size two
// DIFFERENT denominators, and canary #15 (ka-0000000413) proved they diverge:
//   - modeledRestoreNeed (1.5×) sizes the NODE MemAvailable check. The node had
//     14.4Gi free and the gate passed with 7.5Gi modeled headroom.
//   - THIS model sizes the POD-CGROUP memory.max the runsc restore process runs
//     inside. The ROOT-sandbox restore of a 2.96Gi pages.img peaked >8Gi = >2.71×
//     the image and was cgroup-OOM-SIGKILLed against Pod B's 8Gi container LIMIT —
//     even though node RAM was abundant. So the 1.5× node model UNDER-models the
//     per-container restore peak; the limit must be sized off the observed >2.71×
//     ceiling, hence 3.0× (which yields ~2.71×+headroom once the additive terms
//     are included).
//
// INVARIANT (load-bearing for fix B): because the multiplier here (3.0×) is
// STRICTLY GREATER than modeledRestoreNeed's (1.5×) and the overhead+margin terms
// are IDENTICAL, modeledRestorePeakLimit(pages, cc) >= modeledRestoreNeed(pages, cc)
// for every pages >= 0. So a Pod B minted with THIS limit always clears the
// pre-thaw mem-gate's own modeled need — fix A (raise the limit) structurally
// satisfies fix B (gate against the limit). TestRestorePeakLimit_ExceedsModeledNeed
// pins this ordering.
//
// TODO(ka-0000003398 fix C): the 3.0× multiplier is derived from a SINGLE
// data point (canary #15's >2.71× root-sandbox peak) plus headroom. The node
// thaw script now captures the ACTUAL cgroup memory.peak post-restore
// (FT_RESTORE_PEAK_BYTES → ThawResult.RestorePeakBytes); once enough peaks are
// collected across mediums (self vs dir) and sandbox roles (root vs sub), replace
// this flat multiplier with a per-medium / per-role calibrated term.
//
// It is a PURE package function (not a manager method) so both the placeholder
// spec-shaper (shapePlaceholderSpec, which has no manager receiver) and the
// legacy warm-pool default builder can call it. cc is floored to the bare-agent
// baseline exactly like memGateContainerCount so a legacy zero-count snapshot
// never zeroes the overhead term. pages <= 0 returns 0 (INDETERMINATE): the
// caller must NOT impose a limit off an unknown pages size.
func modeledRestorePeakLimit(pages int64, containerCount int) int64 {
	if pages <= 0 {
		return 0
	}
	cc := containerCount
	if cc < freezeBaselineContainerCount {
		cc = freezeBaselineContainerCount
	}
	return pages*defaultRestorePeakLimitMultiplierPermille/1000 +
		defaultMemGateSentryOverhead*int64(cc) + defaultMemGateMargin
}

// defaultRestorePeakLimitMultiplierPermille sizes the pod-cgroup memory LIMIT at
// 3.0× the pages.img (see modeledRestorePeakLimit). It is DELIBERATELY fatter
// than defaultMemGateRestoreMultiplierPermille (1.5×, the node-availability
// model): canary #15's root-sandbox restore peaked >2.71× the image against the
// container cgroup, so the limit must clear that ceiling with headroom while the
// node model stays tight (a false cold-start on node RAM is free; a cgroup-OOM
// mid-restore is the bug). Per-mille integer math keeps sizing in int64 bytes.
const defaultRestorePeakLimitMultiplierPermille int64 = 3000

// peakOverPagesPermille is the observed restore-peak-to-pages ratio in per-mille
// (2710 == 2.71×), for the fix-C calibration log line. Returns 0 when either term
// is non-positive (nothing to ratio). This is the number the multiplier constants
// are meant to bound: canary #15's root sandbox measured >2710.
func peakOverPagesPermille(peak, pages int64) int64 {
	if peak <= 0 || pages <= 0 {
		return 0
	}
	return peak * 1000 / pages
}

// modeledNeedOverPagesPermille is the modeledRestoreNeed/pages ratio in per-mille
// (2010 == 2.01×) — the WATCH signal for ka-0000002024. It decays from the fixed
// additive overhead dominating (small images, ratio ≫ multiplier) toward the bare
// restore multiplier (large images, ratio → 1.5×), so comparing it against
// empiricalPeakOverPagesFloorPermille tells the gate when it is admitting a restore
// on less modeled headroom-per-byte than the canary #14 OOM floor. Returns 0 when
// either term is non-positive (nothing to ratio).
func modeledNeedOverPagesPermille(need, pages int64) int64 {
	if need <= 0 || pages <= 0 {
		return 0
	}
	return need * 1000 / pages
}
