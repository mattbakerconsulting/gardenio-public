// freezethaw_node.go holds the node-side bash scripts that drive `runsc
// checkpoint|restore` for the path-b freeze/thaw verbs. These are faithful,
// parameterized ports of the PROVEN PoC harnesses:
//
//   - freeze  ⇐ multi-container-resume-node.sh steps [2/8]–[4/8] +
//     restore-through-lifecycle.sh step [2/6] (checkpoint + preserve
//     bundles + capture overlay opts)
//   - thaw    ⇐ multi-container-resume-node.sh steps [3/8]–[8/8]
//     (overlay reconstruction, mount-dest normalization, count-gated
//     barrier restore) composed with restore-into-b.py
//     (A→B sandbox-cid / pod-uid / netns repoint)
//
// They run on the node via NodeExecutor (the privileged hostPath probe / chroot
// /host), the same access pattern the PoC harnesses and Phase-2 use. Each runsc
// invocation is wrapped in `timeout ${RUNSC_TIMEOUT}` because runsc C/R can hang
// on process exit (epic constraint). The scripts emit machine-readable FT_*=…
// sentinel lines on stdout that the Go orchestration parses for its post-state
// assertions; everything else on stdout is human-readable progress.
//
// The scripts are kept as Go text/template so the controller controls the cids,
// paths, container count, and timeout — but the mechanics (overlay collapse to
// a fresh RO lowerdir, /var/run→/run normalization, background-root +
// foreground-sub barrier ordering, nsenter -t 1 -m -p -n) are byte-for-byte the
// proven sequence. Comments cross-reference the harness line they mirror.
//
// The template bodies live in freezethaw_scripts/*.sh.tmpl and are embedded
// below (moved out of Go raw-string literals byte-for-byte, gd-Wl6LuCnvlH).
package podmanager

import (
	"bytes"
	_ "embed"
	"net"
	"strings"
	"text/template"
)

// splitEgressTarget parses a rehydrate-consumer endpoint into (host, port) for
// the thaw script's [6c] in-sentry egress dial (fix B, ka-0000008207). It accepts
// the two endpoint shapes the controller config carries:
//
//   - a URL with a scheme — coop's NATS URL "nats://<release>-nats:4222" — parsed
//     via net/url so the scheme is stripped and the port read from the authority;
//   - a bare host[:port] — the beads daemon "BEADS_HTTP_ADDR" (e.g.
//     "localhost:8080" or "<release>-beads:8080").
//
// A missing port falls back to def (NATS 4222 / beads 8080). An empty or
// unparseable raw value returns ("",""), which the thaw template renders as empty
// EGRESS_* vars — the script then SKIPS the end-to-end dial for that target and
// relies on the [6c-1] in-sentry default-route check (which needs no target). So a
// malformed endpoint degrades gracefully to the link-only route detector rather
// than breaking the thaw.
func splitEgressTarget(raw, def string) (host, port string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ""
	}
	// URL form (scheme://host[:port][/...]) — e.g. nats://host:4222.
	if strings.Contains(raw, "://") {
		i := strings.Index(raw, "://")
		raw = raw[i+3:]
	}
	// Strip any path/query the beads addr could (rarely) carry.
	if i := strings.IndexAny(raw, "/?"); i >= 0 {
		raw = raw[:i]
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ""
	}
	if h, p, err := net.SplitHostPort(raw); err == nil {
		if h == "" {
			return "", ""
		}
		if p == "" {
			p = def
		}
		return h, p
	}
	// No colon (or an IPv6 literal without a port): treat the whole thing as host.
	return raw, def
}

// freezeScriptParams parameterize the node-side freeze script.
type freezeScriptParams struct {
	PodName      string // CRI sandbox-name to resolve the live cids for
	RunscTimeout int    // seconds; wraps each runsc call (epic hang guard)
	ImagePath    string // node-local checkpoint image dir
	BundlePath   string // node-local preserved-bundle dir
}

// reapImageScriptParams parameterize the post-thaw checkpoint-image reaper
// (ka-0000006968).
type reapImageScriptParams struct {
	ImagePath string // node-local checkpoint image dir to delete (snap.ImagePath)
}

// memGateScriptParams parameterize the pre-thaw node memory-capacity probe
// (ka-0000000088). The probe reads MemAvailable from the pinned node's
// /proc/meminfo and (for legacy snapshots that have no persisted PagesBytes)
// also stat -c %s on pages.img so the Go side gates on the freshest possible
// numbers.
type memGateScriptParams struct {
	// ImagePath is the snapshot's node-local checkpoint dir. The probe stat's
	// $ImagePath/pages.img to fill the FT_PAGES_BYTES fallback for legacy
	// snapshots (freeze pre-ka-0000000088).
	ImagePath string
}

// tokenRefreshScriptParams parameterize the POST-THAW token refresher node
// script (bug ka-0000000518). Unlike freeze/thaw, this runs on a LIVE thawed
// agent (Pod B is the running pod) periodically for the agent's lifetime, so it
// touches ONLY the SA/IRSA token warm copies under $BUNDLE/.gofer/<n> — never
// the sandbox, the rootfs, or any other mount. It re-overlays freshly-minted
// (controller-side, via the k8s TokenRequest API) token BYTES onto the same
// warm-copy backing files the thaw cred-overlay wrote, and re-chowns them
// 1000:1000 so the restored UID/GID-1000 agent keeps its read grant — exactly
// mirroring the thaw overlay+chown block (freezethaw_node.go PYAGENT
// _SA_TOKEN_DIRS branch), but on a running pod rather than at restore time.
type tokenRefreshScriptParams struct {
	// BundlePath is the agent's controller-owned bundle dir on the node
	// (BundleHostBase/<agent> = /var/lib/gardenio/bundle/<agent>). The
	// .gofer/manifest.json under it maps each SA/IRSA token mount destination to
	// its warm-copy subdir ($BUNDLE/.gofer/<n>).
	BundlePath string
	// SAToken is the freshly-minted projected-SA (kubernetes API audience) JWT
	// the controller obtained from the TokenRequest API, base64-std encoded so
	// the multi-KB token rides safely inside the node script body (no shell
	// quoting hazard). Empty means "no SA token to refresh" (the script skips
	// that mount).
	SATokenB64 string
	// IRSAToken is the freshly-minted IRSA web-identity (sts.amazonaws.com
	// audience) JWT, base64-std encoded. Empty skips the IRSA mount.
	IRSATokenB64 string
	// SADest / IRSADest are the normalized (/run/...) mount destinations whose
	// warm copies the manifest keys. They are constants today (the
	// _SA_TOKEN_DIRS set) but threaded as params so the script has no hard-coded
	// paths and the Go side owns the single source of truth.
	SADest   string
	IRSADest string
}

// thawScriptParams parameterize the node-side thaw (barrier restore) script.
type thawScriptParams struct {
	PlaceholderPod string // CRI sandbox-name of the kubelet-owned placeholder Pod B
	BPodUID        string // Pod B's kubelet pod uid (for the A→B uid repoint)
	ASandboxCID    string // A sandbox cid to rewrite out of all bind sources
	AAgentCID      string // A agent sub-container cid
	APodUID        string // A pod uid to rewrite out of all bind sources
	ImagePath      string // node-local checkpoint image dir (from the snapshot)
	BundlePath     string // node-local preserved-bundle dir (from the snapshot)
	ContainerCount int    // barrier target (statefile ContainerCount)
	RunscTimeout   int    // seconds; wraps each runsc call

	// RestoreOverlayFlag is the fully-rendered "--overlay2=<medium>" the checkpoint
	// was taken under (overlay-dir-medium, ka-0000007239), or "" for the default
	// self medium. Non-empty is appended to every `runsc restore` so the restored
	// sentry rebuilds its rootfs-overlay upper from the SAME medium the checkpoint
	// expects (a root:dir checkpoint can only restore under root:dir). EMPTY leaves
	// the restore lines byte-identical to the pre-fix script — zero blast radius on
	// self-medium nodes. Rendered by overlayRestoreFlag(snap.OverlayMedium).
	RestoreOverlayFlag string

	// SpecFiles re-materializes the OCI bundle at thaw start (fix B,
	// ka-0000000301): the host bundle dir may have been GC'd by Pod A teardown,
	// so we rewrite the captured config.json/.overlay-opts before any runsc
	// restore. Each entry is base64 so it survives template + shell intact.
	SpecFiles []thawSpecFile

	// SubContainers is the ordered roster of sub-containers to restore on top of
	// the sandbox root (ka-0000003561): the agent plus any always-on DinD/Postgres
	// sidecars. The thaw script reconstructs each one's rootfs overlay, normalizes
	// + repoints its bundle, and fires one `runsc restore` per entry to satisfy
	// the count-gated barrier. The bundle subdir is keyed by container Name.
	SubContainers []thawSubContainer

	// ThawStateDir is the NEW agent's per-agent thaw-state hostPath on the node
	// (thawStateHostDir(req.AgentName) = ThawStateHostBase/<sanitized agent>), the
	// dir the controller's NodeRehydrationTrigger writes the epoch marker into and
	// polls the consumer acks from. The thaw [3] mount-rewrite FORCES the agent
	// bundle's MountThawState (/run/gardenio-thaw) mount SOURCE to this dir,
	// OVERRIDING the bsrc[d] live-Pod-B branch (bug ka-0000000673 / ka-0000001104).
	//
	// WHY a dedicated param (not the bsrc[d] live-Pod-B source): on the warm-B graft
	// path Pod B is a pool placeholder minted from a DIFFERENT agent's snapshot
	// template (shapePlaceholderSpec over snap.PodTemplate), so its thaw-state
	// hostPath sources from thawStateHostDir(<placeholder-template-agent>), NOT the
	// new agent the controller addresses. The checkpointed agent bundle likewise
	// carries the FROZEN agent's dir. Both resolve to the WRONG dir, so the restored
	// coop/gb watchers read+ack into a stale thaw-state while the controller polls
	// thawStateHostDir(<new agent>) — guaranteed both-consumer fail-close (canary #7,
	// prod 2026.180.27). Only the controller knows the new agent's identity, so it
	// threads the resolved dir in here. EMPTY (legacy snapshot / older controller)
	// SKIPS the force entirely — the rewrite falls through to the pre-fix bsrc[d]
	// behavior, so an unset value carries zero blast radius. The node pre-creates +
	// chowns this dir (AgentUID:AgentGID) before the [3a] dangling-source guard so
	// the forced source resolves and is agent-writable for the ack.
	ThawStateDir string

	// ThawStateDest is the canonical in-pod mount destination of the thaw-state
	// volume (MountThawState = /run/gardenio-thaw), threaded in so the [3] PYAGENT
	// rewrite matches the same destination the Go constant defines (single source
	// of truth) rather than hardcoding the path string inside the embedded python.
	ThawStateDest string

	// NatsHost/NatsPort and BeadsHost/BeadsPort are the in-sentry egress targets
	// for the [6c] sentry-side egress assertion (fix B, ka-0000008207 / bug
	// ka-0000000514). They are the SAME endpoints the two rehydrate consumers dial
	// from inside the restored sentry — coop's NATS (COOP_NATS_URL) and gb's beads
	// daemon (BEADS_HTTP_ADDR) — so the probe exercises the exact egress paths that
	// fail-closed the rehydrate-ack gate on prod. Empty (older controller / unset)
	// SKIPS the end-to-end dial; the in-sentry default-route check still runs (it
	// needs no target). Parsed from FreezeThawManager.NatsURL / .BeadsHTTPAddr by
	// the buildThawScript caller.
	NatsHost  string
	NatsPort  string
	BeadsHost string
	BeadsPort string

	// NatsDialAddr/BeadsDialAddr are the resolved ClusterIP LITERALS the [6c-2]
	// in-sentry dial actually connects to (bug ka-0000002907). The pre-fix dial used
	// the bare service DNS name ($EGRESS_NATS_HOST) — but a LINK-ONLY restored sentry
	// cannot reach kube-dns, so its lookup failed gaierror -> classified "other" ->
	// FT_SENTRY_EGRESS=inconclusive -> flake-OPEN "restore verified" (the exact canary
	// #11 miss). A ClusterIP is a cluster-global VIP the netstack routes purely at L3
	// (no DNS), so a link-only sentry no-routes it with EHOSTUNREACH -> classified
	// link-only -> fail-CLOSED. The controller resolves NatsURL/BeadsHTTPAddr to their
	// ClusterIPs (resolveEgressDialAddr, best-effort DNS from the controller pod) and
	// threads the literals in here; EMPTY (unresolvable / no resolver / already an IP
	// that failed) SKIPS the end-to-end dial for that target — the [6c-1] in-sentry
	// default-route check (which needs no target) remains the load-bearing floor. The
	// NatsHost/BeadsHost names are still rendered for the human-readable log line.
	NatsDialAddr  string
	BeadsDialAddr string
}

// thawSpecFile is one bundle-relative spec file re-materialized at thaw start.
type thawSpecFile struct {
	Path   string // bundle-relative, e.g. "sandbox/config.json"
	Base64 string // base64-encoded file bytes
}

// thawSubContainer is one sub-container the barrier restore must reconstruct.
type thawSubContainer struct {
	Name string // CRI container-name == bundle subdir (e.g. "agent", "dind")
	ACID string // A-side cid (diagnostic; restore pairs by Name on the node)
}

func (f *FreezeThawManager) buildFreezeScript(p freezeScriptParams) string {
	if p.ImagePath == "" {
		p.ImagePath = "$IMG"
	}
	if p.BundlePath == "" {
		p.BundlePath = "$BUNDLE"
	}
	var buf bytes.Buffer
	if err := freezeTmpl.Execute(&buf, p); err != nil {
		// PANIC: the template is a compile-time constant parsed via
		// template.Must and freezeScriptParams has only fixed string/int
		// fields, so Execute cannot fail at runtime — an error here is a
		// programming bug (bad template edit), not a recoverable condition.
		panic("podmanager: freeze template execute: " + err.Error())
	}
	return buf.String()
}

// buildReapImageScript renders the post-thaw image reaper (ka-0000006968). It is
// a separate one-line node op rather than appended to the thaw script so it runs
// only AFTER the Go side has positively verified the restore (and rehydration) —
// the thaw script itself must never delete the image it might still need if a
// later step bails.
func (f *FreezeThawManager) buildReapImageScript(p reapImageScriptParams) string {
	var buf bytes.Buffer
	if err := reapImageTmpl.Execute(&buf, p); err != nil {
		// PANIC: same invariant as buildFreezeScript/buildThawScript — a
		// template.Must-parsed constant template over fixed-field params cannot
		// fail Execute at runtime; an error signals a bad template edit.
		panic("podmanager: reap-image template execute: " + err.Error())
	}
	return buf.String()
}

// buildMemGateScript renders the pre-thaw memory-capacity probe (ka-0000000088).
// It reads MemAvailable_kB from the pinned node's /proc/meminfo AND stat's
// pages.img at snap.ImagePath — so a legacy snapshot that pre-dates the freeze
// script's FT_PAGES_BYTES capture can still be gated on the node-side stat.
// Emits three sentinels the Go side parses: FT_NODE_MEM_AVAIL_BYTES,
// FT_PAGES_STAT_BYTES, FT_MEM_GATE_OK. Non-zero script exit on catastrophic
// probe failure (no /proc/meminfo readable) so the thaw fails-closed on
// unknown headroom rather than proceeding to a probable OOM-SIGKILL. HMNT to
// read the host meminfo (probe-ns /proc/meminfo would report the probe pod's
// cgroup limits, not the node's real free memory).
func (f *FreezeThawManager) buildMemGateScript(p memGateScriptParams) string {
	var buf bytes.Buffer
	if err := memGateTmpl.Execute(&buf, p); err != nil {
		// PANIC: same invariant as buildFreezeScript/buildThawScript — a
		// template.Must-parsed constant template over fixed-field params cannot
		// fail Execute at runtime; an error signals a bad template edit.
		panic("podmanager: mem-gate template execute: " + err.Error())
	}
	return buf.String()
}

func (f *FreezeThawManager) buildThawScript(p thawScriptParams) string {
	var buf bytes.Buffer
	if err := thawTmpl.Execute(&buf, p); err != nil {
		// PANIC: same invariant as buildFreezeScript — a template.Must-parsed
		// constant template over fixed-field params cannot fail Execute at
		// runtime; an error signals a bad template edit, a programming bug.
		panic("podmanager: thaw template execute: " + err.Error())
	}
	return buf.String()
}

// buildTokenRefreshScript renders the POST-THAW token refresher (bug
// ka-0000000518). It is a separate, isolated node op — NOT appended to the thaw
// script — because it runs periodically on a LIVE thawed pod (long after the
// restore), and its blast radius is deliberately confined to the two SA/IRSA
// token warm copies. The controller mints the fresh tokens Go-side (TokenRequest
// API) and passes the bytes in base64; the script only re-overlays + re-chowns
// the warm-copy backing files.
func (f *FreezeThawManager) buildTokenRefreshScript(p tokenRefreshScriptParams) string {
	var buf bytes.Buffer
	if err := tokenRefreshTmpl.Execute(&buf, p); err != nil {
		// PANIC: same invariant as buildFreezeScript — a template.Must-parsed
		// constant template over fixed-field params cannot fail Execute at
		// runtime; an error signals a bad template edit, a programming bug.
		panic("podmanager: token-refresh template execute: " + err.Error())
	}
	return buf.String()
}

// freezeTmpl mirrors multi-container-resume-node.sh [1/8]–[4/8]: resolve the
// live sandbox+agent cids for the pod, `runsc checkpoint --leave-running`
// (one image for the whole pod, container_count=N baked in), preserve both OCI
// bundles + the captured overlay mount-opt strings + the warm /work and SA
// token, all keyed by agent identity for a later same-node thaw.
var freezeTmpl = template.Must(template.New("freeze").Parse(freezeScriptSrc))

//go:embed freezethaw_scripts/freeze.sh.tmpl
var freezeScriptSrc string

// reapImageTmpl deletes the 3.0G checkpoint image dir host-namespaced after a
// verified thaw (ka-0000006968). The image lives at the controller-owned safe
// path /var/lib/gardenio/ckpt/<agent> (ka-0000003702), DELIBERATELY outside any
// pod-lifecycle GC, so nothing else ever reclaims it; a single freeze→thaw cycle
// would leak one 3.0G image per agent on the node. `runsc restore` has already
// loaded the image into the live sentry by the time this runs — the restored
// container runs off the bundle overlay, not the image — so the image is dead
// weight and safe to delete. HMNT (nsenter -t 1 -m) so the rm targets the same
// host mount ns runsc wrote/read the image in (the ka-0000002536 invariant); a
// probe-ns rm would miss it. The BUNDLE is NOT touched: $BUNDLE/<c>/rootfs is the
// live overlay mountpoint and $BUNDLE/.work the live /work source — both back the
// running Pod B and must outlive the thaw (multi-cycle re-freeze cleanup is the
// snapshot-transport follow-up's concern). Emit FT_IMAGE_REAPED so the Go side
// can log whether the reclaim landed; a failed rm is best-effort (the restore
// already succeeded — a leaked image is a disk-space nuisance, never a
// correctness problem — so the reaper never fails the thaw).
var reapImageTmpl = template.Must(template.New("reapImage").Parse(reapImageScriptSrc))

//go:embed freezethaw_scripts/reap_image.sh.tmpl
var reapImageScriptSrc string

// memGateTmpl probes the pinned node's free memory + the pages.img size BEFORE
// Pod B is created, so the Go side (thawOnce) can fail-closed IN MILLISECONDS
// rather than committing to a create+restore that will OOM-SIGKILL 7s in
// (ka-0000000088 / canary #12 rc=137). The failure signature this closes is
// structurally deterministic: a c6a.large node (3.05Gi allocatable) had a
// prewarmed pod backfill it after the subject freeze released ~660Mi, so best
// free is ~2.1Gi — the eager runsc restore of a 3.0G pages.img (loaded RESIDENT
// within seconds; freezethaw_node.go [4/8] invariant :2195/:2203) cannot fit,
// external SIGKILL from the kernel OOM-killer, thaw dies with SubRC=137.
//
// Sentinels (all HMNT-scoped so they see the HOST view, not the probe pod's cgroup):
//
//	FT_NODE_MEM_AVAIL_BYTES=<bytes> — MemAvailable_kB * 1024 from host /proc/meminfo,
//	or empty on parse failure (Go side then falls back to fail-closed).
//	FT_PAGES_STAT_BYTES=<bytes> — stat -c %s of $IMG/pages.img on the host, or empty
//	if the image path is absent/unreadable.
//	FT_MEM_GATE_OK=yes — sentinel that the script ran to completion (belt-and-braces;
//	the Go side keys the gate on the two numeric fields, but a missing OK
//	signals a truncated stdout / transport miss and is treated as fail-closed).
//
// The probe is read-only: it stats a path and reads /proc/meminfo, so it CANNOT
// perturb the frozen sandbox or the target node. Exec-timeout is bounded by
// RUNSC_TIMEOUT applied by the caller (thawOnce), same as the other node ops.
var memGateTmpl = template.Must(template.New("memGate").Parse(memGateScriptSrc))

//go:embed freezethaw_scripts/mem_gate.sh.tmpl
var memGateScriptSrc string

// tokenRefreshTmpl re-overlays freshly-minted SA/IRSA token bytes into a thawed
// agent's warm-copy token files (bug ka-0000000518). BACKGROUND: at thaw the
// SA/IRSA token mounts are bound to controller-owned warm copies under
// $BUNDLE/.gofer/<n> and Pod-B's live token is overlaid ONCE (freezethaw_node.go
// PYAGENT _SA_TOKEN_DIRS branch). But that warm copy is a STATIC snapshot
// OUTSIDE the kubelet atomic-writer rotation loop, and thaw step [4] runsc-deletes
// Pod B's containers so the kubelet stops rotating its projected token entirely —
// so ~1h post-thaw the overlaid token EXPIRES and the agent's k8s/AWS calls 401.
// rehydration-design.md K2 assumed the fix was "re-read the file path", but there
// is no live rotating source left on the node to re-read. This periodic refresher
// is the missing rotation loop: the controller re-mints the tokens itself (k8s
// TokenRequest API, bound to the live Pod B) and this script writes the fresh
// bytes back into the warm copy.
//
// It is a DELIBERATELY MINIMAL mirror of the thaw overlay+chown block, scoped to
// ONLY the two SA/IRSA token warm copies:
//
//   - Resolve each destination's warm-copy subdir from $BUNDLE/.gofer/manifest.json
//     (the same normalized-destination lookup thaw uses). A destination absent
//     from the manifest is skipped (best-effort — the agent simply keeps the old
//     token until the next mount that IS keyed refreshes).
//   - ATOMIC-overlay the fresh bytes onto BOTH `token` and `..data/token` following
//     the atomic-writer symlink chain to the real backing file (temp file on the
//     same dir + os.rename), so a mid-write interrupt never leaves a truncated JWT
//     (matches _atomic_overlay in the thaw PYAGENT).
//   - chown the REAL backing file 1000:1000 + re-assert 0640, so the restored
//     UID/GID-1000 agent keeps its read grant (mirrors the thaw TOKEN-OWNERSHIP
//     RESTORE, bug ka-0000000189 — copytree/root-write resets group agent->root).
//
// Coop's freshness_gate runs ONCE per thaw epoch (coop/thaw.rs), so re-writing the
// warm copy AFTER rehydrate is transparent to coop — it never re-gates the token,
// so a byte-swap cannot re-trip PodAIdentity. The freshly-minted token is bound to
// the LIVE Pod B (its pod_uid) and carries a fresh exp, exactly what a kubelet
// rotation would have produced.
//
// SCOPE GUARD (fiq4 rubric ka-0000005354): touches ONLY $BUNDLE/.gofer/<n> for the
// two _SA_TOKEN_DIRS destinations — never the sandbox, the rootfs, the [6c]
// sentry-egress block, the self-IPAM route, or the ~/.claude warm trees.
//
// Sentinels: FT_TOKREFRESH_<slug>=ok|skip|fail per mount, and FT_TOKREFRESH_DONE=yes
// on completion. Best-effort throughout: a miss on one mount must never wedge the
// live agent (it keeps the prior token until the next cycle).
var tokenRefreshTmpl = template.Must(template.New("tokenRefresh").Parse(tokenRefreshScriptSrc))

//go:embed freezethaw_scripts/token_refresh.sh.tmpl
var tokenRefreshScriptSrc string

// thawTmpl mirrors multi-container-resume-node.sh [3/8]–[8/8] composed with
// restore-into-b.py: reconstruct each container's rootfs overlay in host-init's
// ns from the captured opts (collapse {lower+upper} into a fresh RO lowerdir +
// fresh upper/work, whiteout leftover filestore), normalize mount destinations
// /var/run→/run, repoint A-sandbox-cid→B / A-pod-uid→B and the netns path to B's
// CNI netns, then drive the COUNT-GATED BARRIER restore (background root parks
// at restoringUnstarted; foreground sub satisfies the count and unblocks both),
// all under nsenter -t 1 -m -p -n. Emits the netns proof.
var thawTmpl = template.Must(template.New("thaw").Parse(thawScriptSrc))

//go:embed freezethaw_scripts/thaw.sh.tmpl
var thawScriptSrc string
