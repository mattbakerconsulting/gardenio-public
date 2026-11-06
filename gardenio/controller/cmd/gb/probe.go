package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/podmanager"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// probe.go — `gb probe run <script-bead>` (probe-pods plan ka-0000000544).
//
// A probe is the cheap "did anything change?" pre-check that replaces a full
// no-op gardener inference: a type=script bead whose `script` field is run in a
// one-off K8s pod (execution_target=probe) with NO coop lifecycle and NO Claude
// OAuth. The pod runs to a terminal phase; its container exit code is the whole
// signal — 0 closes the bead done, non-zero marks it failed. Ship UNWIRED (no
// trigger): this is the primitive, not the release-owner gate.

var (
	probeImage        string
	probeNamespace    string
	probePullSecrets  []string
	probeTimeout      time.Duration
	probePollInterval time.Duration
	probeKeepPod      bool
)

var probeCmd = &cobra.Command{
	Use:     "probe",
	Short:   "Run scripted one-off probe pods (execution_target=probe)",
	GroupID: "orchestration",
}

var probeRunCmd = &cobra.Command{
	Use:   "run <script-bead-id>",
	Short: "Run a type=script bead as a one-off probe pod; exit 0 closes it done, non-zero marks it failed",
	Long: `Run a type=script bead as a one-off probe pod.

The bead's ` + "`script`" + ` field is executed as ` + "`/bin/sh -lc \"$script\"`" + ` in a
bare pod (execution_target=probe): a slim (50m/128Mi) container with NO coop
lifecycle, NO Claude OAuth, and RestartPolicy Never. gb waits for the pod to
reach a terminal phase and reads the container exit code:

  - exit 0        → the bead is closed done (agent_state=done)
  - exit non-zero → the bead is marked failed (agent_state=failed) and closed
  - deadline hit  → the kubelet fails the pod → treated as non-zero

The observed exit code is stamped on the bead's exit_code field either way.
gb's own process exit code mirrors the probe (0 on success, 1 on probe
failure) so a caller can branch on it.

This is the probe PRIMITIVE, shipped unwired — no trigger fires it. A gardener
lane opts in explicitly by running it.`,
	Args: cobra.ExactArgs(1),
	RunE: runProbeCmd,
}

func init() {
	probeCmd.AddCommand(probeRunCmd)
	probeRunCmd.Flags().StringVar(&probeImage, "image", "",
		"container image for the probe pod (default: $COOP_IMAGE / $GARDENIO_AGENT_IMAGE — the fleet agent image, for git/jq/kd/gb/glab on PATH)")
	probeRunCmd.Flags().StringVar(&probeNamespace, "namespace", "",
		"namespace to run the probe pod in (default: $POD_NAMESPACE / $NAMESPACE / \"gardenio\")")
	probeRunCmd.Flags().StringSliceVar(&probePullSecrets, "pull-secret", nil,
		"K8s Secret name(s) to pull the private agent image (repeatable/comma-separated; default: $IMAGE_PULL_SECRETS — the same secret the controller stamps on agent pods)")
	probeRunCmd.Flags().DurationVar(&probeTimeout, "timeout", 5*time.Minute,
		"max wall-clock to wait for the probe pod to reach a terminal phase (a hung create/schedule)")
	probeRunCmd.Flags().DurationVar(&probePollInterval, "poll-interval", 2*time.Second,
		"how often to poll the probe pod's phase")
	probeRunCmd.Flags().BoolVar(&probeKeepPod, "keep-pod", false,
		"do not delete the probe pod after it terminates (default: delete for cleanup)")
}

// probeOutcome is the resolved result of a probe run: the observed exit code
// and whether it counts as success (exit 0). It is the seam the exit-code →
// bead-state decision is tested through, independent of any live K8s cluster.
type probeOutcome struct {
	// ExitCode is the probe container's terminated exit code. -1 when no
	// terminated status was observed (e.g. the pod failed to schedule / was
	// deadline-killed before the container ran); such a pod is a failure.
	ExitCode int
	// Reason is a short human-readable cause, empty on success.
	Reason string
}

// succeeded reports whether the probe passed — exit code exactly 0.
func (o probeOutcome) succeeded() bool { return o.ExitCode == 0 }

// probeAgentState maps a probe outcome to the beads agent_state the bead is
// closed with. It mirrors statusreporter.PhaseToAgentState's terminal mapping
// (PodSucceeded→done, PodFailed→failed) so a probe bead reads the same as an
// agent bead: exit 0 → "done", anything else → "failed".
func probeAgentState(o probeOutcome) string {
	if o.succeeded() {
		return "done"
	}
	return "failed"
}

func runProbeCmd(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	beadID := args[0]

	bead, err := daemon.GetBead(ctx, beadID)
	if err != nil {
		return fmt.Errorf("fetching script bead %s: %w", beadID, err)
	}
	if bead.Type != "script" {
		return fmt.Errorf("bead %s is type=%q, not type=script — `gb probe run` requires a script bead", beadID, bead.Type)
	}
	script := bead.Fields["script"]
	if script == "" {
		return fmt.Errorf("script bead %s has no `script` field to run", beadID)
	}

	image := probeImage
	if image == "" {
		image = envOr("COOP_IMAGE", os.Getenv("GARDENIO_AGENT_IMAGE"))
	}
	if image == "" {
		return fmt.Errorf("no probe image: pass --image or set COOP_IMAGE / GARDENIO_AGENT_IMAGE")
	}

	namespace := probeNamespace
	if namespace == "" {
		namespace = envOr("POD_NAMESPACE", envOr("NAMESPACE", "gardenio"))
	}

	// The probe reuses the PRIVATE fleet agent image, so it needs the same
	// registry pull secret the controller stamps on every agent pod — otherwise
	// the pod ImagePullBackOffs (403) and never runs. Default to the exact env
	// the controller reads (IMAGE_PULL_SECRETS) so an in-cluster `gb probe run`
	// inherits the fleet's secret with no flag; --pull-secret overrides.
	pullSecrets := probePullSecrets
	if len(pullSecrets) == 0 {
		pullSecrets = splitCommaTrim(os.Getenv("IMAGE_PULL_SECRETS"))
	}

	project := bead.Fields["project"]

	deadline := int64(0)
	if v := bead.Fields["activity_deadline_seconds"]; v != "" {
		if n, perr := strconv.ParseInt(v, 10, 64); perr == nil && n > 0 {
			deadline = n
		}
	}

	k8s, err := buildK8sClient()
	if err != nil {
		return fmt.Errorf("probe needs a K8s client: %w", err)
	}

	spec := podmanager.ProbePodSpec{
		Name:                  probePodName(beadID),
		Namespace:             namespace,
		Image:                 image,
		Script:                script,
		BeadID:                beadID,
		Project:               project,
		ActiveDeadlineSeconds: deadline,
		ImagePullSecrets:      pullSecrets,
	}

	fmt.Printf("probe: launching pod %s/%s for script bead %s\n", namespace, spec.Name, beadID)
	outcome, err := runProbePod(ctx, k8s, spec, probeTimeout, probePollInterval, probeKeepPod)
	if err != nil {
		// A harness error (couldn't create/observe the pod) is distinct from a
		// probe that ran and returned non-zero: don't silently close the bead
		// on a harness failure — leave it open for a retry and surface the error.
		return fmt.Errorf("probe run failed for %s: %w", beadID, err)
	}

	state := probeAgentState(outcome)
	closeFields := map[string]string{
		"exit_code":   strconv.Itoa(outcome.ExitCode),
		"agent_state": state,
	}
	if !outcome.succeeded() {
		closeFields["close_reason"] = probeCloseReason(outcome)
	}
	if cerr := daemon.CloseBead(ctx, beadID, closeFields); cerr != nil {
		return fmt.Errorf("probe finished (exit=%d, state=%s) but closing bead %s failed: %w",
			outcome.ExitCode, state, beadID, cerr)
	}

	if outcome.succeeded() {
		fmt.Printf("probe: bead %s DONE (exit 0)\n", beadID)
		return nil
	}
	fmt.Printf("probe: bead %s FAILED (exit %d: %s)\n", beadID, outcome.ExitCode, outcome.Reason)
	// Mirror the probe's failure in gb's own exit code so a caller can branch.
	return &probeFailedError{outcome: outcome}
}

// probeCloseReason builds the close_reason string stamped on a failed probe.
func probeCloseReason(o probeOutcome) string {
	if o.Reason != "" {
		return fmt.Sprintf("probe exit %d: %s", o.ExitCode, o.Reason)
	}
	return fmt.Sprintf("probe exit %d", o.ExitCode)
}

// probeFailedError signals a probe that ran and returned non-zero. It is a
// distinct type so main() can map it to process exit code 1 without conflating
// it with a usage/harness error.
type probeFailedError struct{ outcome probeOutcome }

func (e *probeFailedError) Error() string {
	return fmt.Sprintf("probe failed with exit code %d", e.outcome.ExitCode)
}

// probePodName derives a stable, DNS-1123-safe probe pod name from the bead id.
// kd ids are mixed-case (ka-0000002769), but a K8s pod name MUST be a lowercase
// DNS-1123 label, so the id is lowercased. The bead id is still recoverable from
// the pod's gardenio.io/bead-id annotation (BuildProbePod stamps the ORIGINAL id).
func probePodName(beadID string) string {
	name := "probe-" + strings.ToLower(beadID)
	return truncateDNS1123(name)
}

// truncateDNS1123 caps a name at the 63-char DNS-1123 label limit.
func truncateDNS1123(name string) string {
	const maxLen = 63
	if len(name) <= maxLen {
		return name
	}
	return name[:maxLen]
}

// splitCommaTrim splits a comma-separated env value (e.g. IMAGE_PULL_SECRETS)
// into trimmed, non-empty entries. Mirrors config.parseCommaSeparated so an
// in-cluster `gb probe run` reads the fleet's pull-secret env the same way the
// controller does. Returns nil for an empty/whitespace-only value.
func splitCommaTrim(s string) []string {
	var out []string
	for v := range strings.SplitSeq(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// runProbePod creates the probe pod, waits for it to reach a terminal phase
// (bounded by timeout), reads the container exit code, and (unless keepPod)
// deletes the pod. It returns the resolved probeOutcome. A non-zero exit is NOT
// an error — it is a valid outcome the caller maps to a failed bead; only a
// harness failure (create/observe/timeout) returns a non-nil error.
func runProbePod(ctx context.Context, k8s kubernetes.Interface, spec podmanager.ProbePodSpec, timeout, poll time.Duration, keepPod bool) (probeOutcome, error) {
	pod := podmanager.BuildProbePod(spec)

	if _, err := k8s.CoreV1().Pods(spec.Namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// A stale probe pod from a prior run is in the way. Delete it and
			// retry once so a re-run of the same bead is not permanently stuck.
			_ = k8s.CoreV1().Pods(spec.Namespace).Delete(ctx, spec.Name, metav1.DeleteOptions{})
			if _, rerr := k8s.CoreV1().Pods(spec.Namespace).Create(ctx, pod, metav1.CreateOptions{}); rerr != nil {
				return probeOutcome{ExitCode: -1}, fmt.Errorf("creating probe pod (after clearing stale): %w", rerr)
			}
		} else {
			return probeOutcome{ExitCode: -1}, fmt.Errorf("creating probe pod: %w", err)
		}
	}

	if !keepPod {
		// Best-effort cleanup on every exit path. Detach from ctx so a
		// cancelled parent still reaps the pod.
		defer func() {
			delCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			_ = k8s.CoreV1().Pods(spec.Namespace).Delete(delCtx, spec.Name, metav1.DeleteOptions{})
		}()
	}

	outcome, err := waitProbeTerminal(ctx, k8s, spec.Namespace, spec.Name, timeout, poll)
	if err != nil {
		return probeOutcome{ExitCode: -1}, err
	}
	return outcome, nil
}

// waitProbeTerminal polls the probe pod until it reaches a terminal phase
// (Succeeded/Failed), then extracts the probe container's exit code. It is
// bounded by timeout — a pod that never schedules or hangs surfaces as a
// harness error rather than blocking forever.
func waitProbeTerminal(ctx context.Context, k8s kubernetes.Interface, namespace, name string, timeout, poll time.Duration) (probeOutcome, error) {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	for {
		pod, err := k8s.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			if o, done := probeOutcomeFromPod(pod); done {
				return o, nil
			}
		}
		if time.Now().After(deadline) {
			return probeOutcome{ExitCode: -1}, fmt.Errorf("timed out after %s waiting for probe pod %s/%s to terminate", timeout, namespace, name)
		}
		select {
		case <-ctx.Done():
			return probeOutcome{ExitCode: -1}, ctx.Err()
		case <-ticker.C:
		}
	}
}

// probeOutcomeFromPod extracts the probe outcome from a pod's status. The bool
// return is true only once the pod is in a terminal phase — the caller keeps
// polling while it is false. Terminal-phase decoding:
//   - PodSucceeded → exit 0.
//   - PodFailed    → the probe container's terminated exit code (if the
//     container ran), else -1 with the pod-level reason (deadline / eviction /
//     schedule failure that killed the pod before the container terminated).
func probeOutcomeFromPod(pod *corev1.Pod) (probeOutcome, bool) {
	switch pod.Status.Phase {
	case corev1.PodSucceeded:
		return probeOutcome{ExitCode: 0}, true
	case corev1.PodFailed:
		if code, reason, ok := terminatedProbeContainer(pod); ok {
			return probeOutcome{ExitCode: int(code), Reason: reason}, true
		}
		// No container terminated status — the pod itself failed (deadline,
		// eviction, unschedulable). Report a synthetic non-zero.
		reason := pod.Status.Reason
		if reason == "" {
			reason = "pod failed before the probe container reported an exit code"
		}
		return probeOutcome{ExitCode: -1, Reason: reason}, true
	default:
		return probeOutcome{}, false
	}
}

// terminatedProbeContainer returns the exit code + reason of the probe
// container's terminated state, if present.
func terminatedProbeContainer(pod *corev1.Pod) (int32, string, bool) {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != podmanager.ProbeContainerName {
			continue
		}
		if t := cs.State.Terminated; t != nil {
			reason := t.Reason
			if t.Message != "" {
				reason = t.Reason + ": " + t.Message
			}
			return t.ExitCode, reason, true
		}
	}
	return 0, "", false
}
