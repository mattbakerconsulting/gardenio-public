// freezethaw_token_refresh.go implements the POST-THAW credential refresher
// (bug ka-0000000518). It closes a gap the canary-#8 GO/NO-GO audit flagged as a
// non-canary-gating operational follow-up:
//
// At thaw the SA/IRSA token mounts are bound to controller-owned WARM COPIES
// under $BUNDLE/.gofer/<n>, and Pod-B's live projected token is overlaid ONCE
// (freezethaw_node.go PYAGENT _SA_TOKEN_DIRS branch). But that warm copy is a
// STATIC snapshot OUTSIDE the kubelet atomic-writer rotation loop — and thaw
// step [4] `runsc delete --force`s Pod B's containers, so the kubelet stops
// rotating (and reclaims) Pod B's projected token entirely. rehydration-design.md
// assumed the fix for the ~1h-TTL projected SA token was "re-read the file path",
// but post-thaw there is no live rotating source left on the node to re-read. So
// ~1h after thaw the overlaid token EXPIRES and the thawed agent's k8s API + AWS
// STS calls start 401-ing.
//
// This refresher is the missing rotation loop, done controller-side (the most
// self-contained of the bead's options a/b/c — no coop/agent change, no thaw or
// freeze node-script change): periodically the controller re-mints the SA + IRSA
// tokens itself via the k8s TokenRequest API and re-overlays the fresh bytes into
// the warm copy over the isolated buildTokenRefreshScript node op. coop's
// freshness_gate runs ONCE per thaw epoch (coop/thaw.rs), so re-writing the warm
// copy AFTER rehydrate is transparent — coop never re-gates the token, so a
// byte-swap cannot re-trip PodAIdentity.
//
// The whole path is DEFAULT-OFF (cfg.PostThawTokenRefreshEnabled) with zero blast
// radius when disabled, and best-effort throughout: a mint or node-exec miss for
// one agent is logged and skipped, never propagated — the agent keeps its prior
// token until the next cycle.
package podmanager

import (
	"context"
	"encoding/base64"
	"strings"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SA/IRSA projected-token mount destinations in their NORMALIZED (/run/...) form
// — the same _SA_TOKEN_DIRS set the thaw cred-overlay binds to warm copies
// (freezethaw_node.go). Kept here as the Go-side single source of truth threaded
// into the refresher node script so the script has no hard-coded paths.
const (
	// saTokenMountDest is the standard projected k8s API ServiceAccount token
	// (default API-server audience).
	saTokenMountDest = "/run/secrets/kubernetes.io/serviceaccount"
	// irsaTokenMountDest is the EKS IRSA web-identity token (sts.amazonaws.com
	// audience).
	irsaTokenMountDest = "/run/secrets/eks.amazonaws.com/serviceaccount"
	// awsSTSAudience is the only audience AWS STS accepts on an IRSA
	// web-identity token; it must match coop's AWS_STS_AUDIENCE (rehydrate.rs).
	awsSTSAudience = "sts.amazonaws.com"
)

// refreshTokenTTLSeconds is the requested lifetime of the freshly-minted tokens.
// The apiserver clamps it to [min, --service-account-max-token-expiration], and
// returns the granted exp. It is deliberately well ABOVE the refresh interval
// (default 30m) so each cycle re-mints long before the prior token expires; the
// clamp guarantees a lower bound of the cluster's minimum (>= 10m). One hour
// matches the projected-token default the warm copy replaces.
const refreshTokenTTLSeconds int64 = 3600

// RefreshThawedTokens re-mints and re-overlays the SA/IRSA tokens for every live
// thawed agent whose warm-copy token would otherwise expire ~1h post-thaw (bug
// ka-0000000518). It is the periodic body the controller's token-refresh loop
// invokes on a timer. namespaces is the set to enumerate live placeholder pods in
// (the controller namespace plus, when configured, the gVisor namespace).
//
// Best-effort per agent: a snapshot-list, mint, or node-exec failure for one
// agent is logged and skipped, never propagated. Returns the count of agents
// whose tokens were successfully refreshed (>=1 mount landed) for logging.
func (f *FreezeThawManager) RefreshThawedTokens(ctx context.Context, namespaces []string) (refreshed int) {
	// Index CONSUMED (i.e. already-thawed) snapshots by canonical pod name. A
	// consumed snapshot survives post-thaw (its bundle backs the live Pod B) and
	// carries the authoritative BundlePath + Node the warm copy lives under — the
	// join key back from a live placeholder pod. A non-consumed snapshot is an
	// agent awaiting thaw (no live warm-copy token to rotate yet), skipped.
	snaps, err := f.snaps.List(ctx)
	if err != nil {
		f.logger.Warn("post-thaw token refresh: listing snapshots failed; skipping this cycle",
			"error", err)
		return 0
	}
	byPodName := make(map[string]Snapshot, len(snaps))
	for _, s := range snaps {
		if s.Consumed && s.PodName != "" {
			byPodName[s.PodName] = s
		}
	}
	if len(byPodName) == 0 {
		return 0
	}

	// Enumerate live thaw-placeholder pods and join each back to its consumed
	// snapshot via the canonical pod name (the exact remap the reconciler uses:
	// trim "-thaw-b" for a create-path Pod B, or read AnnotationCanonicalPod for a
	// warm-B graft whose pool name the suffix-trim cannot recover).
	seen := make(map[string]struct{}, len(byPodName))
	for _, ns := range namespaces {
		if ns == "" {
			continue
		}
		pods, err := f.client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
			LabelSelector: LabelThawPlaceholder + "=true",
		})
		if err != nil {
			f.logger.Warn("post-thaw token refresh: listing thaw-placeholder pods failed; skipping namespace",
				"namespace", ns, "error", err)
			continue
		}
		for i := range pods.Items {
			pod := &pods.Items[i]
			// A pod being torn down has no future — its warm copy is about to go
			// with the bundle, so a refresh would be wasted (and could race the
			// delete). Skip it.
			if pod.DeletionTimestamp != nil {
				continue
			}
			canonical := canonicalThawPodName(pod)
			snap, ok := byPodName[canonical]
			if !ok {
				continue
			}
			// De-dup: the same canonical agent can only be refreshed once per
			// cycle even if two placeholder pods somehow map to it.
			if _, dup := seen[canonical]; dup {
				continue
			}
			seen[canonical] = struct{}{}
			if f.refreshOneThawedAgent(ctx, pod, snap) {
				refreshed++
			}
		}
	}
	return refreshed
}

// canonicalThawPodName maps a live thaw-placeholder pod back to the canonical
// agent pod name a snapshot is keyed by (Snapshot.PodName). Mirrors the
// reconciler's remap (reconciler.go): prefer AnnotationCanonicalPod (set at
// warm-B promotion, since the pool name "warm-b-<node>-…" cannot be suffix-
// trimmed), else trim the create-path "-thaw-b" suffix.
func canonicalThawPodName(pod *corev1.Pod) string {
	if c := pod.Annotations[AnnotationCanonicalPod]; c != "" {
		return c
	}
	return strings.TrimSuffix(pod.Name, ThawPlaceholderSuffix)
}

// refreshOneThawedAgent mints fresh SA + IRSA tokens for one live thawed agent
// and re-overlays them into its warm copy over the isolated token-refresh node
// op. Returns true iff the node script confirmed at least one mount refreshed.
// Best-effort: every failure mode logs and returns false without touching the
// live agent.
func (f *FreezeThawManager) refreshOneThawedAgent(ctx context.Context, pod *corev1.Pod, snap Snapshot) bool {
	// The warm copy lives under the snapshot's bundle on the node the pod runs
	// on. Prefer the pod's actual NodeName (source of truth for where the warm
	// copy physically is); fall back to the snapshot's recorded node.
	node := pod.Spec.NodeName
	if node == "" {
		node = snap.Node
	}
	if node == "" || snap.BundlePath == "" {
		f.logger.Warn("post-thaw token refresh: missing node or bundle path; skipping agent",
			"agent", snap.AgentName, "podB", pod.Name, "node", node, "bundlePath", snap.BundlePath)
		return false
	}
	saName := pod.Spec.ServiceAccountName
	if saName == "" {
		saName = "default"
	}

	// Mint UNBOUND SA-scoped tokens (no BoundObjectRef). Post-thaw Pod B is
	// phase=Failed by design (its placeholder containers were runsc-deleted at
	// thaw step [4]), so binding a token to that pod object is unreliable; an
	// unbound token authenticates as the IDENTICAL SA identity, which is exactly
	// what the thawed agent needs. The mounted projected token's pod_uid claim is
	// moot here because coop's freshness_gate runs only once per thaw epoch and
	// never re-reads the rotated warm copy (coop/thaw.rs).
	//
	// SA token: nil audiences => the apiserver's default API audiences, matching
	// the projected SA token the warm copy replaces. IRSA token: the STS audience.
	saTok := f.mintServiceAccountToken(ctx, pod.Namespace, saName, nil)
	irsaTok := f.mintServiceAccountToken(ctx, pod.Namespace, saName, []string{awsSTSAudience})
	if saTok == "" && irsaTok == "" {
		// Both mints failed — nothing to write. mintServiceAccountToken already
		// logged the cause (RBAC, SA missing, transport).
		return false
	}

	script := f.buildTokenRefreshScript(tokenRefreshScriptParams{
		BundlePath:   snap.BundlePath,
		SADest:       saTokenMountDest,
		IRSADest:     irsaTokenMountDest,
		SATokenB64:   base64.StdEncoding.EncodeToString([]byte(saTok)),
		IRSATokenB64: base64.StdEncoding.EncodeToString([]byte(irsaTok)),
	})
	res, err := f.exec.Exec(ctx, node, script)
	if err != nil {
		f.logger.Warn("post-thaw token refresh: node exec failed; agent keeps prior token until next cycle",
			"agent", snap.AgentName, "podB", pod.Name, "node", node, "error", err)
		return false
	}
	sent := parseSentinels(res.Stdout)
	if sent[keyTokenRefreshDone] != "yes" {
		f.logger.Warn("post-thaw token refresh: node script did not run to completion; agent keeps prior token",
			"agent", snap.AgentName, "podB", pod.Name, "node", node, "stderr", tail(res.Stderr, 300))
		return false
	}
	saRes, irsaRes := sent[keyTokenRefreshSA], sent[keyTokenRefreshIRSA]
	ok := saRes == "ok" || irsaRes == "ok"
	f.logger.Info("post-thaw token refresh: re-overlaid warm-copy token(s) for thawed agent",
		"agent", snap.AgentName, "podB", pod.Name, "node", node,
		"sa", saRes, "irsa", irsaRes, "refreshed", ok)
	return ok
}

// mintServiceAccountToken requests a fresh token for the named ServiceAccount via
// the k8s TokenRequest API. audiences nil => the apiserver's default API
// audiences (the standard projected-SA-token audience). Returns "" and logs on
// any error (RBAC denial, missing SA, transport) so the caller degrades to
// best-effort without wedging the live agent.
func (f *FreezeThawManager) mintServiceAccountToken(ctx context.Context, namespace, saName string, audiences []string) string {
	ttl := refreshTokenTTLSeconds
	tr := &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{
			Audiences:         audiences,
			ExpirationSeconds: &ttl,
		},
	}
	out, err := f.client.CoreV1().ServiceAccounts(namespace).CreateToken(ctx, saName, tr, metav1.CreateOptions{})
	if err != nil {
		f.logger.Warn("post-thaw token refresh: minting SA token failed",
			"namespace", namespace, "serviceAccount", saName, "audiences", audiences, "error", err)
		return ""
	}
	return out.Status.Token
}
