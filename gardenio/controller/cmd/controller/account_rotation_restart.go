package main

import (
	"context"
	"log/slog"
	"time"

	corev1 "k8s.io/api/core/v1"

	"gardenio/controller/internal/config"
	"gardenio/controller/internal/podmanager"
)

// accountRotationRestarter restarts running agent pods that are pinned to an
// account at the moment that account transitions to status="exhausted".
//
// Why this exists (ka-0000007997): agent pods bake their LiteLLM virtual key
// into the ANTHROPIC_CUSTOM_HEADERS env var at spawn time (see applyAccountConfig).
// The reconciler never restarts a running pod over its spec — it never
// looks at env — so when the rate-limit probe flips an account to "exhausted",
// running pods keep their now-exhausted key and loop on 429s indefinitely.
//
// The spawn-time guard (resolveNonExhaustedAccount) already redirects *new*
// pods to a healthy fallback. So the fix is narrow: when an account transitions
// to exhausted, delete the running pods that still carry that account's key.
// The reconciler recreates them, and the spawn-time guard re-credentials them
// against a healthy account.
//
// Churn guard: pods are restarted exactly once per transition. We track each
// account's previously observed status and only act on the active→exhausted
// edge, not on every reconcile while the account stays exhausted. As a second
// layer of safety, pods are matched by the *exact* baked key value, so a pod
// that has already been respawned onto a healthy fallback carries a different
// key and will not be matched again.
type accountRotationRestarter struct {
	pods      podmanager.Manager
	namespace string
	logger    *slog.Logger

	// sessions reports each live session's CURRENT account (session id ==
	// pod name) from coopmux, which reconciles it from the account coop
	// itself reports. nil when no coopmux client is configured.
	//
	// Why this matters (ka-0000005463): all three reconcile paths match pods
	// by the LiteLLM key baked into pod env at spawn. Coop's in-process
	// credential switch changes the live credentials WITHOUT changing pod
	// env, so a pod that already failed over still carries the dead
	// account's key and looks restart-worthy — deleting it then destroys a
	// healthy session's context (observed 2026-07-08: 4 already-switched
	// pods killed by the stranded sweep). The baked key only matters if it
	// is what the session is actually running on.
	sessions liveSessionAccounts

	// drain gates a delete on the agent finishing its turn, so a
	// controller-initiated restart stops killing pods mid-response
	// (ka-0000001482). nil disables the gate and every delete is immediate —
	// the pre-fix behavior, kept as the fail-open for tests and for a
	// controller wired without a reconciler.
	//
	// It is the RECONCILER's tracker, shared deliberately: the drain map is
	// keyed by pod name, so a second tracker would double-nudge a pod the
	// node-drain path is already draining.
	drain drainGate

	// beads stamps the real restart reason on the agent bead before deleting, so
	// the death report stops attributing controller deletes to Spot/node-drain
	// (ka-0000001482 D3). nil disables the stamp.
	beads beadFieldStamper

	// drainTimeout bounds the wait. A wedged pod that never reports idle is
	// still recycled once this expires, so the gate can never permanently
	// strand a pod on a dead account.
	drainTimeout time.Duration

	// blindSweeps counts CONSECUTIVE stranded sweeps that ran without live
	// session-account data, so the sweep can defer a bounded number of times and
	// then fail open rather than trusting baked keys immediately (ka-0000001482 D2)
	// or deferring forever (which would make coopmux a SPOF).
	blindSweeps int

	// prevStatus records the last-observed status for each account name so we
	// can detect the active→exhausted transition. nil until the first call.
	prevStatus map[string]string

	// pendingTerminal holds accounts whose terminal-exhaustion restart was
	// DEFERRED because a matching pod was still mid-turn (ka-0000001482).
	//
	// Reconcile is EDGE-triggered: it acts only on the active→exhausted edge and
	// then forgets. Without this, a deferred delete would never be retried and the
	// pod would sit on a dead account forever — the drain gate would have
	// converted a context loss into a permanent strand. Entries are re-tried every
	// cycle and dropped once no pod carries the account's key any more.
	// account name → baked LiteLLM key at deferral time.
	pendingTerminal map[string]string
}

// strandedRecoveryBlindSweeps is how many consecutive stranded-recovery sweeps
// may be deferred while coopmux gives no live-account data before the sweep
// proceeds on baked-key matching alone. At the 5-minute sweep cadence this
// tolerates a ~10-minute coopmux outage before accepting the
// already-recovered-pod risk.
const strandedRecoveryBlindSweeps = 2

// drainGate is the slice of reconciler.UpgradeTracker the restarter needs;
// narrowed to an interface so tests can fake it.
type drainGate interface {
	IsDraining(podName string) bool
	StartDrainWithMsg(ctx context.Context, podName, agentName, msg string) bool
	DrainReady(ctx context.Context, podName string, timeout time.Duration) bool
}

// beadFieldStamper writes fields on an agent bead; narrowed so tests can fake it.
type beadFieldStamper interface {
	UpdateBeadFields(ctx context.Context, beadID string, fields map[string]string) error
}

// controllerRestartReasonField records, on the agent bead, that the CONTROLLER
// deleted this pod and why.
//
// Why (ka-0000001482 D3): the pod's own SIGTERM handler closes the bead
// `context_cancelled`, whose Slack summary hardcodes "node drain, rollout, or
// Spot reclaim" — all three false for a rotation restart, and actively
// misleading (the 2026-07-27 report sent triage hunting Karpenter and Spot for a
// delete our own control loop initiated on an all-on-demand cluster). The
// restarter knows the real reason, so it stamps it BEFORE deleting and the death
// report prefers it over the generic guess.
const controllerRestartReasonField = "controller_restart_reason"

// podAgentBeadID returns the agent bead id baked into the pod's env at spawn
// (BOAT_AGENT_BEAD_ID), or "" when absent.
func podAgentBeadID(pod *corev1.Pod) string {
	for _, c := range pod.Spec.Containers {
		if c.Name != "agent" {
			continue
		}
		for _, e := range c.Env {
			if e.Name == "BOAT_AGENT_BEAD_ID" {
				return e.Value
			}
		}
	}
	return ""
}

// podDrainAgent returns the agent to mail before deleting pod: the BOAT_AGENT
// baked into its env at spawn, falling back to the gardenio.io/agent label.
// It is "" when there is no live session to drain, because the pod is not
// running or carries no agent name.
func podDrainAgent(pod *corev1.Pod) string {
	if pod.Status.Phase != corev1.PodRunning {
		return ""
	}
	for _, c := range pod.Spec.Containers {
		if c.Name != "agent" {
			continue
		}
		for _, e := range c.Env {
			if e.Name == "BOAT_AGENT" && e.Value != "" {
				return e.Value
			}
		}
	}
	return pod.Labels[podmanager.LabelAgent]
}

// stampControllerRestartReason records why the controller is about to delete
// this pod, so the death report can name the real cause instead of blaming AWS.
// Best-effort: a failed stamp must never block the restart it annotates.
func (a *accountRotationRestarter) stampControllerRestartReason(ctx context.Context, beadID, reason string) {
	if a.beads == nil || beadID == "" {
		return
	}
	if err := a.beads.UpdateBeadFields(ctx, beadID, map[string]string{
		controllerRestartReasonField: reason,
	}); err != nil {
		a.logger.Warn("could not stamp controller restart reason on agent bead",
			"bead", beadID, "reason", reason, "error", err)
	}
}

// accountRotationDrainNudgeMsg is the nudge sent to an agent whose pod is about
// to be recycled onto a healthy account. Mirrors the upgrade/node-drain
// land-your-WIP shape so the agent's response path is identical no matter which
// kill trigger fired.
const accountRotationDrainNudgeMsg = "Your pod is being moved to a healthy " +
	"account — it will be replaced shortly. Commit and push your WIP now " +
	"(git add -A && git commit && git push), then gb stop. Uncommitted work is " +
	"auto-salvaged to S3 but pushing is cleaner."

// drainBeforeDelete reports whether pod may be deleted NOW. It opens a graceful
// drain on first call and returns false while the agent is still mid-turn, so
// the caller must `continue` and re-check on the next sweep.
//
// The nudge is interrupt mail to agentName (see podDrainAgent) and the drain
// waits on that agent's bead, so nothing dials the pod.
//
// Returns true (delete immediately) when the gate is disabled, when there is
// no agent to mail, or when the gate cannot open a drain — there is no way to
// ask an unreachable agent to wrap up, and blocking forever on it would strand
// the pod on a dead account.
func (a *accountRotationRestarter) drainBeforeDelete(ctx context.Context, podName, agentName string) bool {
	if a.drain == nil {
		return true
	}
	if agentName == "" {
		return true
	}
	// One nudge per drain episode: the sweep re-enters every tick, and the
	// tracker keeps the entry until the pod goes away, so IsDraining dedups the
	// re-nudge (the reconciler.go:882 wasDraining pattern, ka-0000000550).
	if !a.drain.IsDraining(podName) &&
		!a.drain.StartDrainWithMsg(ctx, podName, agentName, accountRotationDrainNudgeMsg) {
		return true
	}
	if a.drain.DrainReady(ctx, podName, a.drainTimeout) {
		return true
	}
	a.logger.Info("deferring account-rotation restart: agent still mid-turn, draining",
		"pod", podName)
	return false
}

// liveSessionAccounts is the slice of the coopmux client the restarter needs;
// narrowed to an interface so tests can fake it.
type liveSessionAccounts interface {
	ListSessionAccounts(ctx context.Context) (map[string]string, error)
}

func newAccountRotationRestarter(pods podmanager.Manager, namespace string, sessions liveSessionAccounts, logger *slog.Logger) *accountRotationRestarter {
	return &accountRotationRestarter{
		pods:      pods,
		namespace: namespace,
		sessions:  sessions,
		logger:    logger.With("component", "account-rotation-restart"),
	}
}

// withDrainGate routes the restarter's live-session deletes through gate,
// bounded by timeout, so an agent mid-turn is nudged to land its WIP instead of
// being killed outright (ka-0000001482). A nil gate leaves every delete
// immediate.
func (a *accountRotationRestarter) withDrainGate(gate drainGate, timeout time.Duration) *accountRotationRestarter {
	a.drain = gate
	a.drainTimeout = timeout
	return a
}

// withBeadStamper lets the restarter record the real restart reason on the agent
// bead before deleting its pod (ka-0000001482 D3).
func (a *accountRotationRestarter) withBeadStamper(beads beadFieldStamper) *accountRotationRestarter {
	a.beads = beads
	return a
}

// liveAccounts returns the session-id → live-account map from coopmux, or an
// empty map when coopmux is unconfigured/unreachable. Fail-open: a missing
// map means "no live-account information", and the reconcile paths fall back
// to their pre-existing baked-key behavior — coopmux availability must not
// become a new SPOF for legitimate crashloop recovery.
func (a *accountRotationRestarter) liveAccounts(ctx context.Context) map[string]string {
	if a.sessions == nil {
		return nil
	}
	accounts, err := a.sessions.ListSessionAccounts(ctx)
	if err != nil {
		a.logger.Warn("could not fetch live session accounts from coopmux; falling back to baked-key matching",
			"error", err)
		return nil
	}
	return accounts
}

// podSwitchedOffAccount reports whether coopmux says the pod's session is
// currently running on a DIFFERENT account than the one being acted on —
// i.e. an in-process credential switch already recovered it and its baked
// key is stale-but-irrelevant. Unknown sessions return false (fail-open).
func podSwitchedOffAccount(live map[string]string, podName, account string) bool {
	cur, ok := live[podName]
	return ok && cur != "" && cur != account
}

// podBoatAccount returns the account name the pod was spawned against
// (BOAT_ACCOUNT env, pinned by applyAccountConfig), or "" when unset.
func podBoatAccount(pod *corev1.Pod) string {
	for _, c := range pod.Spec.Containers {
		if c.Name != "agent" {
			continue
		}
		for _, e := range c.Env {
			if e.Name == "BOAT_ACCOUNT" {
				return e.Value
			}
		}
	}
	return ""
}

// litellmHeaderFor returns the ANTHROPIC_CUSTOM_HEADERS value that
// applyAccountConfig bakes for an account with the given LiteLLM key. Kept in
// lockstep with applyAccountConfig — if that format changes, change it here too.
func litellmHeaderFor(litellmKey string) string {
	return "x-litellm-api-key: Bearer " + litellmKey
}

// accountExhaustionTerminal reports whether an account's exhaustion is a
// TERMINAL condition — one a running pod pinned to the account can never
// recover from in place — as opposed to a transient 5h/7d rolling rate-limit
// window that self-heals when it resets (or that the in-session coop
// credential switch recovers WITHOUT a pod restart).
//
// Why this gate exists (ka-0000002085): the active→exhausted edge that drives
// Reconcile is set by the rate-limit probe — reconcileLegacyStatusFields flips
// Status="exhausted" on throttle_status="rejected" OR utilization_7d>=1.0.
// BOTH are rolling-window conditions that lift on their own when the window
// resets (observed <~1h for the 5h window during the 2026-06-22 incident).
// Deleting a pod on that edge destroys all in-pod work — one stuck pod carried
// 272k tokens of context — for a condition that would have cleared by itself.
// restartCount stays 0 because it is a Pod delete+recreate (not a container
// restart), so it looks free but is not: the work is gone. The one stuck pod
// that survived the incident recovered on its own when its 5h throttle lifted,
// NOT via rotation — proving aggressive deletion on the transient edge is pure
// loss.
//
// Terminal cases DO warrant a restart, because the pod's baked credential is
// permanently dead and no window reset will revive it:
//   - ProbeAuthStatus == "rejected": the read-only probe got a 401/403 — the
//     OAuth token is revoked or the org has disabled Claude Code access (the
//     matthewbaker1110 hard error). The pod can never authenticate again on
//     this account, so leaving it in place just 401-loops forever.
//   - overageReasonBlocks(OverageDisabledReason): a genuinely-blocking billing
//     reason (NOT the benign subscription-overage reasons — see
//     overageReasonBlocks). Billing is dead; requests fail regardless of the
//     rate-limit window.
//
// Stale virtual-key rotation (a healthy account whose litellm_key was rotated
// out from under a long-lived pod) is a separate terminal case handled by
// ReconcileStaleVKeys and is intentionally not covered here.
func accountExhaustionTerminal(acct config.AccountCacheEntry) bool {
	if acct.ProbeAuthStatus == "rejected" {
		return true
	}
	if overageReasonBlocks(acctBillingType(acct), acct.OverageDisabledReason) {
		return true
	}
	return false
}

// Reconcile detects accounts that have just transitioned to exhausted and
// restarts the agent pods still carrying their baked LiteLLM key — but ONLY
// when the exhaustion is TERMINAL (accountExhaustionTerminal). A pod whose
// account is exhausted on a transient 5h/7d rate-limit window is left running:
// the window reset or the in-session coop switch recovers it in place without
// losing its context (ka-0000002085). It is meant to be called once per
// periodic sync cycle with the freshly refreshed account cache. Returns the
// names of pods that were deleted (for observability/tests).
//
// The first call only seeds the previous-status snapshot and never restarts
// anything — without a prior observation there is no transition to act on, and
// restarting on first observation would churn every pod on controller startup.
func (a *accountRotationRestarter) Reconcile(ctx context.Context, accounts map[string]config.AccountCacheEntry) []string {
	// Compute newly-exhausted accounts before swapping in the new snapshot.
	type exhausted struct {
		name     string
		key      string
		terminal bool
	}
	var transitioned []exhausted
	seeding := a.prevStatus == nil

	next := make(map[string]string, len(accounts))
	for name, acct := range accounts {
		next[name] = acct.Status
		if seeding {
			continue
		}
		prev := a.prevStatus[name]
		// Only the active→exhausted edge triggers a restart. If the account
		// was already exhausted (or unknown last cycle but exhausted now and
		// we have prior state), we only act on the edge where it was NOT
		// exhausted before and IS now.
		if prev != "exhausted" && acct.Status == "exhausted" && acct.LiteLLMKey != "" {
			transitioned = append(transitioned, exhausted{
				name: name, key: acct.LiteLLMKey,
				terminal: accountExhaustionTerminal(acct),
			})
		}
	}
	a.prevStatus = next

	if seeding {
		a.logger.Debug("seeded account status snapshot, no restarts on first pass",
			"accounts", len(accounts))
		return nil
	}

	// Re-arm any terminal restart the drain gate deferred on an earlier cycle, as
	// long as the account is still exhausted. Reconcile is edge-triggered, so
	// without this replay a drained pod is never revisited (see pendingTerminal).
	for name, key := range a.pendingTerminal {
		acct, ok := accounts[name]
		if !ok || acct.Status != "exhausted" {
			// Account recovered or vanished — nothing to move the pod off of.
			delete(a.pendingTerminal, name)
			continue
		}
		alreadyQueued := false
		for _, ex := range transitioned {
			if ex.name == name {
				alreadyQueued = true
				break
			}
		}
		if !alreadyQueued {
			transitioned = append(transitioned, exhausted{
				name: name, key: key, terminal: accountExhaustionTerminal(acct),
			})
		}
	}

	if len(transitioned) == 0 {
		return nil
	}

	agentPods, err := a.pods.ListAgentPods(ctx, a.namespace, map[string]string{
		podmanager.LabelApp: podmanager.LabelAppValue,
	})
	if err != nil {
		a.logger.Error("failed to list agent pods for exhausted-account restart", "error", err)
		return nil
	}

	live := a.liveAccounts(ctx)

	var deleted []string
	for _, ex := range transitioned {
		// Transient exhaustion (a 5h/7d rolling rate-limit window) is NOT a
		// reason to delete a running pod: the window resets on its own — often
		// within the hour — and the in-session coop credential switch can
		// recover the pod in place without losing its context. Deleting here
		// destroys recoverable work (ka-0000002085). Only TERMINAL exhaustion
		// (dead auth / dead billing) warrants the delete+recreate, since no
		// window reset will revive the pod's baked credential.
		if !ex.terminal {
			a.logger.Info("account exhausted on a transient rate-limit window; leaving pods running to recover in place",
				"account", ex.name)
			continue
		}
		wantHeader := litellmHeaderFor(ex.key)
		restarted := 0
		deferredPods := 0
		for i := range agentPods {
			pod := &agentPods[i]
			if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
				continue
			}
			if !podHasCustomHeader(pod, wantHeader) {
				continue
			}
			if podSwitchedOffAccount(live, pod.Name, ex.name) {
				a.logger.Info("skipping pod: session already switched off exhausted account in-process",
					"pod", pod.Name, "account", ex.name, "live_account", live[pod.Name])
				continue
			}
			// Let the agent finish its turn first (ka-0000001482). The account is
			// terminally dead so the pod must eventually move, but a mid-response
			// kill costs a full context re-drive; the drain timeout bounds the wait.
			if !a.drainBeforeDelete(ctx, pod.Name, podDrainAgent(pod)) {
				// Remember to come back: this path is edge-triggered and the edge is
				// already spent, so without re-arming the pod would stay on the dead
				// account forever.
				if a.pendingTerminal == nil {
					a.pendingTerminal = map[string]string{}
				}
				a.pendingTerminal[ex.name] = ex.key
				deferredPods++
				continue
			}
			a.logger.Info("restarting agent pod pinned to terminally-exhausted account",
				"pod", pod.Name, "account", ex.name)
			a.stampControllerRestartReason(ctx, podAgentBeadID(pod),
				"controller restarted the pod off terminally-exhausted account "+ex.name)
			if err := a.pods.DeleteAgentPod(ctx, pod.Name, a.namespace); err != nil {
				a.logger.Warn("failed to delete pod for exhausted-account restart",
					"pod", pod.Name, "account", ex.name, "error", err)
				continue
			}
			deleted = append(deleted, pod.Name)
			restarted++
		}
		// Nothing left mid-turn for this account: drop the retry so the replay
		// above stops re-arming it every cycle.
		if deferredPods == 0 {
			delete(a.pendingTerminal, ex.name)
		}
		a.logger.Info("terminal-exhaustion transition restart complete",
			"account", ex.name, "pods_restarted", restarted,
			"pods_draining", deferredPods)
	}
	return deleted
}

// ReconcileStaleVKeys restarts running agent pods whose baked LiteLLM virtual
// key no longer matches any current account key — i.e. the key was rotated out
// from under a long-lived pod.
//
// Why this exists (ka-0000003704): the exhaustion path above only fires on the
// active→exhausted edge. A plain vkey rotation (e.g. the LiteLLM stage2/stage3
// vkey migration) replaces an account's litellm_key while the account stays
// healthy, so no status transition occurs and the exhaustion path never acts.
// The pod keeps its now-deleted key baked into ANTHROPIC_CUSTOM_HEADERS and
// every coop resume gets a 401 "Unable to find token in LiteLLM_Verification
// TokenTable" → Claude enters an error state → nudge undeliverable → gb init
// FATAL → restart → repeat, forever (observed: 200+ restarts on the monorepo
// crew pods, 14h crashloop).
//
// Detection is set-membership, not per-account pinning: a pod is stale iff its
// baked key is non-empty and is NOT the current litellm_key of ANY account in
// the cache. After a rotation the old key is gone from every account, so the
// pod is flagged; the reconciler recreates it and the spawn path bakes the
// fresh key from the account bead. New pods carry a valid key, so they are
// never matched again — the same self-idempotency the exhaustion path relies
// on.
//
// Safety guards (a bad/empty account refresh must never nuke the fleet):
//   - Returns early unless the cache carries at least one non-empty LiteLLMKey.
//     An empty or partial snapshot would otherwise make every key look stale.
//   - Skips pods with no baked key (they don't authenticate via LiteLLM).
//   - Skips terminal (Failed/Succeeded) and already-Terminating pods.
//
// Returns the names of pods deleted (for observability/tests).
func (a *accountRotationRestarter) ReconcileStaleVKeys(ctx context.Context, accounts map[string]config.AccountCacheEntry) []string {
	// Build the set of currently-valid LiteLLM keys across all accounts.
	validKeys := make(map[string]struct{}, len(accounts))
	for _, acct := range accounts {
		if acct.LiteLLMKey != "" {
			validKeys[acct.LiteLLMKey] = struct{}{}
		}
	}
	// Guard: never act on an empty/partial snapshot — that would flag every
	// pod as stale and restart the whole fleet.
	if len(validKeys) == 0 {
		a.logger.Debug("no valid litellm keys in account cache, skipping stale-vkey sweep",
			"accounts", len(accounts))
		return nil
	}

	agentPods, err := a.pods.ListAgentPods(ctx, a.namespace, map[string]string{
		podmanager.LabelApp: podmanager.LabelAppValue,
	})
	if err != nil {
		a.logger.Error("failed to list agent pods for stale-vkey restart", "error", err)
		return nil
	}

	// Map each account's CURRENT key so a pod's baked key can be attributed
	// to the account it was spawned against (validKeys only answers set
	// membership). Needed for the in-process-switch skip below.
	live := a.liveAccounts(ctx)

	var deleted []string
	for i := range agentPods {
		pod := &agentPods[i]
		if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
			continue
		}
		if pod.DeletionTimestamp != nil {
			continue // already terminating — recreate is in flight
		}
		key := podLiteLLMKey(pod)
		if key == "" {
			continue // not LiteLLM-authenticated
		}
		if _, ok := validKeys[key]; ok {
			continue // key still valid
		}
		// A rotated-out baked key only 401-loops the pod if the session is
		// still USING it. If coop switched the session to a DIFFERENT
		// account in-process, its live credentials came from the broker —
		// the stale baked env is inert and a restart would only destroy
		// context. The rotated key no longer maps to any account, so
		// attribute it via the pod's birth account (BOAT_ACCOUNT) instead:
		// live != birth means an in-process switch happened. live == birth
		// is the true ka-0000003704 crashloop and must still restart.
		if birth := podBoatAccount(pod); birth != "" && podSwitchedOffAccount(live, pod.Name, birth) {
			a.logger.Info("skipping stale-vkey restart: session already switched to another account in-process",
				"pod", pod.Name, "birth_account", birth, "live_account", live[pod.Name])
			continue
		}
		// NO drain gate here, deliberately (ka-0000001482). Unlike the
		// exhaustion/stranded paths, this pod's baked key 401s on every request,
		// so Claude is in an error state and the drain nudge is UNDELIVERABLE by
		// construction (see the crashloop chain in this function's doc comment).
		// Draining would add the full timeout to a pod that is guaranteed dead and
		// cannot land WIP anyway. Do not "consistency-fix" this into a drain.
		a.logger.Info("restarting agent pod carrying a rotated-out LiteLLM key",
			"pod", pod.Name)
		if err := a.pods.DeleteAgentPod(ctx, pod.Name, a.namespace); err != nil {
			a.logger.Warn("failed to delete pod for stale-vkey restart",
				"pod", pod.Name, "error", err)
			continue
		}
		deleted = append(deleted, pod.Name)
	}
	if len(deleted) > 0 {
		a.logger.Info("stale-vkey sweep complete", "pods_restarted", len(deleted))
	}
	return deleted
}

// ReconcileKeylessGainedKey restarts running agent pods that were spawned
// KEYLESS — no LiteLLM virtual key baked into ANTHROPIC_CUSTOM_HEADERS — against
// an account that has SINCE gained a litellm_key.
//
// Why this exists (ka-0000004556): ReconcileStaleVKeys above only fires when a
// pod carries a baked key that is no longer valid; it explicitly SKIPS keyless
// pods (`key == "" → continue`). But the mirror-image drift is just as fatal.
// When an account bead is edited from keyless to keyed (e.g. gardenio3 bumped
// from opus-4-6/no-key to opus-4-7 WITH a litellm_key), the account starts
// routing through LiteLLM and LiteLLM begins REQUIRING the key — but the pods
// spawned before the edit carry no ANTHROPIC_CUSTOM_HEADERS at all. Every
// request they send is rejected `token_not_found_in_db` (401) and the pod burns
// compute looping retries until an operator kills it by hand (the 2026-05-15
// incident: 3 pods >2h old on the just-keyed account, no headers, 401-looping).
//
// The pod env is baked at spawn and only a delete+recreate re-bakes it: the
// controller's coopmux sync (syncAccountsToCoopmux / SyncSingleAccount) updates
// coopmux's credential STORE but never Distributes to running sessions — only
// auth-failover/switchback Distributes — so a same-account key addition never
// reaches a running keyless session in-process. So the fix is a restart: the
// reconciler recreates the pod and applyAccountConfig bakes the account's now-
// present key into ANTHROPIC_CUSTOM_HEADERS. A re-baked pod carries the key, so
// it is never matched again — the same self-idempotency the other paths rely on.
//
// Detection is per-account, not set-membership (the inverse of
// ReconcileStaleVKeys): a pod is stale iff it carries NO baked key AND its birth
// account (BOAT_ACCOUNT) currently has a non-empty litellm_key. Keying on the
// pod's OWN account avoids flagging pods that are legitimately keyless because
// THEIR account is still keyless (direct-provider auth) — a genuinely keyless
// account never triggers a restart.
//
// Safety guards (a bad/empty account refresh must never nuke the fleet):
//   - Skips pods that already carry a baked key (ReconcileStaleVKeys' job).
//   - Skips pods with no BOAT_ACCOUNT — the birth account is unknown, so there
//     is nothing to compare against.
//   - Acts only when the pod's birth account is present in the cache AND carries
//     a non-empty key. A missing account or an empty/partial refresh (the account
//     temporarily absent or its key blank) leaves the pod alone.
//   - Skips terminal (Failed/Succeeded) and already-Terminating pods.
//   - Skips a pod whose session already switched to a DIFFERENT account
//     in-process (coopmux live account != BOAT_ACCOUNT): its live credentials
//     came from the broker, the baked keyless env is inert, and a restart would
//     only destroy context (the ka-0000005463 guard the other paths share).
//
// NO drain gate here, deliberately — mirroring ReconcileStaleVKeys. The keyless
// pod 401s on every request, so Claude is in an error state and the drain nudge
// is undeliverable by construction; draining would only add the full timeout to
// a pod that cannot land WIP anyway.
//
// Returns the names of pods deleted (for observability/tests).
func (a *accountRotationRestarter) ReconcileKeylessGainedKey(ctx context.Context, accounts map[string]config.AccountCacheEntry) []string {
	// Guard: never act on an empty snapshot. A wiped/failed account refresh has
	// no keyed accounts to compare against, so there is nothing to detect and
	// acting would only risk churn on a bad read.
	anyKeyed := false
	for _, acct := range accounts {
		if acct.LiteLLMKey != "" {
			anyKeyed = true
			break
		}
	}
	if !anyKeyed {
		a.logger.Debug("no keyed accounts in cache, skipping keyless-gained-key sweep",
			"accounts", len(accounts))
		return nil
	}

	agentPods, err := a.pods.ListAgentPods(ctx, a.namespace, map[string]string{
		podmanager.LabelApp: podmanager.LabelAppValue,
	})
	if err != nil {
		a.logger.Error("failed to list agent pods for keyless-gained-key restart", "error", err)
		return nil
	}

	live := a.liveAccounts(ctx)

	var deleted []string
	for i := range agentPods {
		pod := &agentPods[i]
		if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
			continue
		}
		if pod.DeletionTimestamp != nil {
			continue // already terminating — recreate is in flight
		}
		if podLiteLLMKey(pod) != "" {
			continue // already carries a key — ReconcileStaleVKeys' concern, not ours
		}
		account := podBoatAccount(pod)
		if account == "" {
			continue // birth account unknown — nothing to compare against
		}
		acct, ok := accounts[account]
		if !ok || acct.LiteLLMKey == "" {
			continue // account absent or still keyless — no drift, no restart
		}
		// The pod's birth account gained a key it does not carry. If coop already
		// switched this session to a DIFFERENT account in-process, its live
		// credentials came from the broker and the baked keyless env is inert —
		// restarting would only destroy a healthy session's context.
		if podSwitchedOffAccount(live, pod.Name, account) {
			a.logger.Info("skipping keyless-gained-key restart: session already switched to another account in-process",
				"pod", pod.Name, "birth_account", account, "live_account", live[pod.Name])
			continue
		}
		a.logger.Info("restarting keyless agent pod whose account gained a LiteLLM key",
			"pod", pod.Name, "account", account)
		a.stampControllerRestartReason(ctx, podAgentBeadID(pod),
			"controller restarted the keyless pod: account "+account+" gained a LiteLLM key")
		if err := a.pods.DeleteAgentPod(ctx, pod.Name, a.namespace); err != nil {
			a.logger.Warn("failed to delete pod for keyless-gained-key restart",
				"pod", pod.Name, "account", account, "error", err)
			continue
		}
		deleted = append(deleted, pod.Name)
	}
	if len(deleted) > 0 {
		a.logger.Info("keyless-gained-key sweep complete", "pods_restarted", len(deleted))
	}
	return deleted
}

// ReconcileStrandedRecovered restarts running agent pods that were baked onto an
// account which is STILL exhausted, but whose project pool has SINCE gained a
// healthy alternative. It is the level-triggered counterpart to Reconcile's
// edge trigger.
//
// Why this exists (ka-0000005398): Reconcile only fires on the active→exhausted
// edge. During the 2026-06-29 monorepo exhaustion incident the ENTIRE project
// pool was exhausted simultaneously with no healthy fallback, so the spawn path
// (resolveNonExhaustedAccount) returned the original exhausted account and baked
// pods onto its now-dead key. No active→exhausted edge ever followed — the
// account was exhausted before the pods spawned — so Reconcile never acted.
// When capacity LATER returned (a sibling's 5h/7d window resetting so the probe
// flips it to allowed/allowed_warning, OR an operator adding project:<name> to a
// previously-healthy account), nothing restarted the stranded pods. They
// 429-looped forever until manually killed.
//
// The sweep is a pure recovery probe: for each running pod whose baked key maps
// to an account that is currently status="exhausted" AND throttle_status=
// "rejected" (i.e. genuinely dead, not merely budget-flagged), it asks the SAME
// spawn-time selector the reconciler would use on recreate — selectAccount
// over the current AccountCache + live-pod counts — whether the pod's project
// now has a healthy account to land on. Only if that selector returns a
// DIFFERENT, healthy account carrying a DIFFERENT LiteLLM key does it delete the
// pod; the reconciler recreates it and the spawn path re-bakes the healthy key.
//
// Safety guards (mirror Reconcile / ReconcileStaleVKeys):
//   - Skips terminal (Failed/Succeeded) and already-Terminating pods.
//   - Acts ONLY when a NEW healthy candidate exists — a different account whose
//     key differs from the pod's current key. If the pool is still fully
//     exhausted (selector returns "" or re-selects the stranded account), it is
//     a no-op: never strand a pod into an equal-or-worse situation.
//   - Idempotent: a re-baked pod carries the healthy account's key, which no
//     longer maps to an exhausted account, so it is never matched again — the
//     same self-idempotency the other two paths rely on. This is why the sweep
//     can run every cycle without churning.
//   - Restricted to the stranded account being BOTH exhausted AND rejected, so a
//     merely budget-gated account (Status="", unfit via !isHealthyAuthorized)
//     does not trigger fleet restarts; that is the spawn path's job, not a
//     running-pod recovery concern.
//
// Returns the names of pods deleted (for observability/tests).
//
// Concurrency: selectAccount reads cfg.AccountCache / cfg.RoleCache live,
// and refreshProjectCache reassigns those maps under ProjectCacheMu. The spawn
// path avoids that data race by holding ProjectCacheMu.RLock around its
// selectAccount call; this sweep does the same — but only around the
// pure-CPU decision phase, never across the K8s list/delete I/O. So the shape
// is: list pods (no lock) → decide under RLock → delete the chosen pods (no
// lock).
func (a *accountRotationRestarter) ReconcileStrandedRecovered(ctx context.Context, cfg *config.Config) []string {
	agentPods, err := a.pods.ListAgentPods(ctx, a.namespace, map[string]string{
		podmanager.LabelApp: podmanager.LabelAppValue,
	})
	if err != nil {
		a.logger.Error("failed to list agent pods for stranded-account recovery sweep", "error", err)
		return nil
	}

	// Live-pod counts feed the spread selector exactly as the spawn path does.
	// SnapshotAccountLiveCounts self-locks (AccountLiveCountsMu), so it is safe
	// to call before taking the ProjectCacheMu read lock below.
	//
	// Working COPY: the per-pod decision loop optimistically bumps the count of
	// each chosen candidate (mirroring the spawn path's BumpAccountLiveCount —
	// see main.go's spawn caller after applyAccountConfig). Without that bump,
	// selectAccount is a pure function and re-picks the alphabetically-
	// first least-loaded healthy account on every iteration, so a batch of N
	// pods stranded on the same project+role all redirect to the SAME healthy
	// sibling and re-saturate its per-account RPM/5h limit — the exact pile-up
	// the spread path was written to prevent (account_spread.go's header).
	// Threading the bump keeps the rebake batch spread across the available
	// healthy candidates, just like a burst of fresh spawns would.
	liveCounts := cfg.SnapshotAccountLiveCounts()

	// Live per-session accounts from coopmux — fetched BEFORE taking
	// ProjectCacheMu (network I/O must not run under the lock). A pod whose
	// session already switched off the stranded account in-process is not
	// stranded at all; deleting it would destroy a healthy session's context
	// (ka-0000005463, 2026-07-08: 4 already-switched pods swept).
	liveSessions := a.liveAccounts(ctx)
	// With no live-account data the ka-0000005463 guard below silently evaporates
	// (podSwitchedOffAccount returns false for EVERY pod), so the sweep treats
	// already-recovered pods as stranded — invisibly, since the fail-open in
	// liveAccounts is intentional and quiet (ka-0000001482 D2: it logged 0 skips
	// while 5 working pods were deleted).
	//
	// BOUNDED deferral, not a hard skip: sit out up to strandedRecoveryBlindSweeps
	// consecutive blind sweeps (this sweep is periodic, so a coopmux blip costs
	// only a tick), then fail open and restart anyway. A permanent skip would make
	// coopmux a SPOF for crashloop recovery and leave a genuinely stranded pod
	// 429-looping forever — the exact failure
	// TestReconcileStrandedRecovered_FailsOpenWhenCoopmuxUnavailable guards.
	// Only defer when coopmux is CONFIGURED but gave nothing — that is a
	// transient outage worth waiting out. With no coopmux wired at all the data is
	// never coming, so deferring would just delay every recovery forever.
	if len(liveSessions) == 0 && a.sessions != nil {
		a.blindSweeps++
		if a.blindSweeps <= strandedRecoveryBlindSweeps {
			a.logger.Warn("deferring stranded-account recovery sweep: no live session-account data from coopmux, cannot tell already-recovered pods from stranded ones",
				"agent_pods", len(agentPods), "blind_sweeps", a.blindSweeps,
				"defer_limit", strandedRecoveryBlindSweeps)
			return nil
		}
		a.logger.Warn("proceeding with stranded-account recovery on baked-key matching alone: coopmux still unavailable after repeated sweeps, accepting the risk of restarting an already-recovered pod",
			"agent_pods", len(agentPods), "blind_sweeps", a.blindSweeps)
	} else {
		a.blindSweeps = 0
	}

	// strandedPod records a pod that should be restarted, decided under the read
	// lock and acted on after it is released.
	type strandedPod struct {
		name      string
		agent     string // for the drain mail (podDrainAgent); captured here since the action phase has no pod ref
		beadID    string // for the restart-reason stamp, same reason
		stranded  string
		candidate string
		project   string
		role      string
	}
	var toRestart []strandedPod

	// Decision phase — pure CPU under ProjectCacheMu.RLock (no K8s I/O), so it
	// races neither refreshProjectCache (which reassigns cfg.AccountCache under
	// the write lock) nor the spawn path.
	cfg.ProjectCacheMu.RLock()
	accounts := cfg.AccountCache
	// Build LiteLLM-key → account-name reverse index so we can map a pod's baked
	// key back to the account it was credentialed against.
	keyToAccount := make(map[string]string, len(accounts))
	for name, acct := range accounts {
		if acct.LiteLLMKey != "" {
			keyToAccount[acct.LiteLLMKey] = name
		}
	}
	// Guard: an empty/partial snapshot has nothing to map against — bail rather
	// than risk acting on a bad refresh (mirrors ReconcileStaleVKeys).
	if len(keyToAccount) > 0 {
		for i := range agentPods {
			pod := &agentPods[i]
			if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
				continue
			}
			if pod.DeletionTimestamp != nil {
				continue // already terminating — recreate is in flight
			}
			key := podLiteLLMKey(pod)
			if key == "" {
				continue // not LiteLLM-authenticated
			}
			acctName, ok := keyToAccount[key]
			if !ok {
				continue // key not on any current account — ReconcileStaleVKeys' job
			}
			acct := accounts[acctName]
			// Only pods stranded on a GENUINELY dead account: exhausted lifecycle
			// AND a live rejected throttle signal. A merely budget-redirected
			// account is left to the spawn path.
			if acct.Status != "exhausted" || acct.ThrottleStatus != "rejected" {
				continue
			}
			// …and only when that exhaustion is TERMINAL. A 5h/7d rolling
			// rate-limit window resets on its own and coop's in-session
			// credential switch recovers the pod in place with its context
			// intact, so killing here destroys recoverable work for nothing
			// (ka-0000002085). reconcileExhaustedAccounts already declines the
			// delete on a transient window; without the same gate here, this
			// periodic sweep re-killed the pods that path had just spared —
			// two mid-turn agents on 2026-07-31 (ka-0000002039), stranded on an
			// account whose status was exhausted/rejected but whose
			// probe_auth was ok.
			if !accountExhaustionTerminal(acct) {
				a.logger.Info("skipping stranded-recovery restart: account exhausted on a transient rate-limit window, pod can recover in place",
					"pod", pod.Name, "stranded_account", acctName)
				continue
			}
			// Not stranded if the session already switched off the account
			// in-process: the baked key is inert and the pod is healthy.
			if podSwitchedOffAccount(liveSessions, pod.Name, acctName) {
				a.logger.Info("skipping stranded-recovery restart: session already switched off account in-process",
					"pod", pod.Name, "stranded_account", acctName, "live_account", liveSessions[pod.Name])
				continue
			}

			// Probe: would a freshly-spawned pod for this project+role now land on
			// a healthy account? Reconstruct the minimal spec the selector reads.
			project, role := podProjectRole(pod)
			// Harness is left unset (claude): pods are matched here by their LiteLLM
			// key, which a codex (subscription-authed) pod never carries.
			spec := &podmanager.AgentPodSpec{Project: project, Role: role}
			candidate := selectAccount(cfg, project, spec, liveCounts)
			if candidate == "" || candidate == acctName {
				continue // no healthy alternative — never strand a worse situation
			}
			candKey := accounts[candidate].LiteLLMKey
			if candKey == "" || candKey == key {
				// Candidate carries no usable key, or the same key the pod
				// already has — a restart would not improve anything. No-op
				// (idempotent).
				continue
			}
			toRestart = append(toRestart, strandedPod{
				name: pod.Name, agent: podDrainAgent(pod), beadID: podAgentBeadID(pod),
				stranded: acctName, candidate: candidate,
				project: project, role: role,
			})
			// Bump the working count so the NEXT stranded pod in this batch
			// spreads onto a different healthy candidate (see liveCounts comment
			// above). liveCounts is a snapshot-local map; we never write back to
			// cfg.AccountLiveCounts — refreshAccountLiveCounts reconciles against
			// ground truth (the actual pod list) on the next sync cycle, and a
			// transient over-count is harmless: at worst it slightly skews ONE
			// subsequent spawn-time spread pick before the refresh, and only
			// up-counted accounts (the healthy ones we just chose) can be
			// affected.
			liveCounts[candidate]++
		}
	}
	cfg.ProjectCacheMu.RUnlock()

	// Action phase — delete the chosen pods outside the lock.
	var deleted []string
	for _, p := range toRestart {
		// Let the agent finish its turn first (ka-0000001482 — this is the site
		// that killed 5 working pods on 2026-07-27). This sweep is PERIODIC (every
		// 5 min), so deferring costs nothing: an undrained pod is re-evaluated on
		// the next tick and the drain timeout still bounds the total wait.
		if !a.drainBeforeDelete(ctx, p.name, p.agent) {
			continue
		}
		a.logger.Info("restarting agent pod stranded on exhausted account; project pool has recovered",
			"pod", p.name, "stranded_account", p.stranded,
			"healthy_candidate", p.candidate, "project", p.project, "role", p.role)
		a.stampControllerRestartReason(ctx, p.beadID,
			"controller restarted the pod off exhausted account "+p.stranded+
				" onto "+p.candidate)
		if err := a.pods.DeleteAgentPod(ctx, p.name, a.namespace); err != nil {
			a.logger.Warn("failed to delete pod for stranded-account recovery",
				"pod", p.name, "stranded_account", p.stranded, "error", err)
			continue
		}
		deleted = append(deleted, p.name)
	}
	if len(deleted) > 0 {
		a.logger.Info("stranded-account recovery sweep complete", "pods_restarted", len(deleted))
	}
	return deleted
}

// podProjectRole extracts the project and role a pod was spawned for. It prefers
// the BOAT_PROJECT / BOAT_ROLE env vars on the agent container (which carry the
// RAW, un-sanitized values — multi-role agents keep their comma-separated role
// so the spread selector's role-pin / whitelist / exclusivity checks resolve
// correctly), falling back to the gardenio.io/project and gardenio.io/role labels.
// The label role is SanitizeRole'd (commas→dashes), so it is only a last resort.
func podProjectRole(pod *corev1.Pod) (project, role string) {
	for _, c := range pod.Spec.Containers {
		if c.Name != "agent" {
			continue
		}
		for _, e := range c.Env {
			switch e.Name {
			case "BOAT_PROJECT":
				project = e.Value
			case "BOAT_ROLE":
				role = e.Value
			}
		}
	}
	if project == "" {
		project = pod.Labels[podmanager.LabelProject]
	}
	if role == "" {
		role = pod.Labels[podmanager.LabelRole]
	}
	return project, role
}

// podHasCustomHeader reports whether the pod's agent container carries an
// ANTHROPIC_CUSTOM_HEADERS env var equal to want.
func podHasCustomHeader(pod *corev1.Pod, want string) bool {
	for _, c := range pod.Spec.Containers {
		if c.Name != "agent" {
			continue
		}
		for _, e := range c.Env {
			if e.Name == "ANTHROPIC_CUSTOM_HEADERS" {
				return e.Value == want
			}
		}
	}
	return false
}
