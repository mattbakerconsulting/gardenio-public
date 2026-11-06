package main

// gb release smoke — the read-only prod UAT smoke analogue of `gb release
// status` (release.go). It collapses the gardener-release-owner Step-7 probe
// suite (doc ka-0000000327 Step 7) — six read-only liveness/parity checks the
// gardener runs EVERY cycle — into ONE pass/fail JSON verdict so a single
// (potentially small) model can replace the Step-7 kubectl/gb/glab stitching
// with one call.
//
// THE SIX STEP-7 PROBES (all read-only):
//
//  1. pods-present       pods in the prod namespace are present and Running
//  2. controller-phase   the controller (component=agents) pod is Running
//                        (the pod-phase liveness that replaced the dropped
//                        Lease check — bug ka-0000007021 / G8)
//  3. slack-bridge       the slack-bridge (component=slack-bridge) pod is Running
//  4. beads-roundtrip    the beads daemon answers a health round-trip
//                        (the `kd list --limit 1` HTTP/gRPC round-trip)
//  5. gb-news-roundtrip  a `gb news`-shaped in-progress query round-trips
//  6. prod-drift         the prod controller image tag matches the latest
//                        GOLDEN (released) calver tag — no drift
//
// SIGNALS: a failure of probes 1–5 surfaces the `uat-regression` trigger
// signal; a failure of probe 6 surfaces `prod-drift`. These are exactly the
// Step-6 escalation events the gardener fires (one-shot smoke failure → file
// bug; TWO consecutive cycles → escalation). This command reports the
// one-shot failure + signal; the caller owns the consecutive-cycle bookkeeping.
//
// EXIT CODE: the JSON verdict is always emitted on stdout; the process exits
// NON-ZERO when any probe fails, so a gardener can branch on `$?` alone.
//
// READ-ONLY CONTRACT: every code path here issues GET/List requests only
// (CoreV1().Pods().List, daemon.Health, daemon.ListBeadsFiltered,
// ListTags, ListReleases). It NEVER deploys, rolls back, scales, retriggers a
// pipeline, or mutates any bead. Keep it that way — Step-7 runs it on EVERY
// cycle, so it must be side-effect free.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/podmanager"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// defaultProdNamespace is the namespace the Step-7 probes target
// (`kubectl get pods -n gardenio`). Overridable with --namespace.
const defaultProdNamespace = "gardenio"

// Kubernetes label/value constants for the component probes.
const (
	componentLabel       = "app.kubernetes.io/component"
	componentAgents      = "agents"       // the controller deployment
	componentSlackBridge = "slack-bridge" // the slack-bridge deployment
)

// The trigger signals a failing probe surfaces. These mirror the Step-6
// escalation event names in doc ka-0000000327.
const (
	signalUATRegression = "uat-regression"
	signalProdDrift     = "prod-drift"
)

// errSmokeFailed is the sentinel returned (after the JSON is printed) so the
// process exits non-zero on any probe failure. cmd.SilenceErrors is set first
// so the JSON on stdout is the only output — the exit code carries the verdict.
var errSmokeFailed = errors.New("release smoke: one or more probes failed")

var (
	releaseSmokeNamespaceFlag string
	releaseSmokeProjectFlag   string
)

var releaseSmokeCmd = &cobra.Command{
	Use:   "smoke",
	Short: "Run the read-only prod UAT smoke probes (Step-7) and emit one pass/fail JSON",
	Long: `Collapse the gardener-release-owner Step-7 prod UAT smoke suite (doc
ka-0000000327 Step 7) into one deterministic pass/fail JSON object so a single
(potentially small) model can replace the Step-7 kubectl/gb/glab stitching with
one call.

Read-only: issues GET/List requests only. Never deploys, rolls back, scales,
retriggers a pipeline, or mutates a bead.

Six probes (all read-only):

  pods-present       pods in the namespace are present and Running
  controller-phase   the controller (component=agents) pod is Running
  slack-bridge       the slack-bridge (component=slack-bridge) pod is Running
  beads-roundtrip    the beads daemon answers a health round-trip (kd list)
  gb-news-roundtrip  a gb news-shaped in-progress query round-trips
  prod-drift         the prod controller image tag == the latest golden tag

Output (one JSON object):

  pass            true only when every probe passed
  probes          per-probe {name, pass, detail, signal}
  failed_probes   names of the probes that failed
  signals         distinct trigger signals to fire (uat-regression / prod-drift)

A failure of probes 1–5 surfaces the uat-regression signal; a failure of the
drift probe surfaces prod-drift. The JSON is always printed; the process exits
NON-ZERO when any probe fails, so a caller can branch on the exit code alone.

The prod-drift probe compares the running controller image tag to the latest
GOLDEN (released) calver tag; it requires GITLAB_BASE_URL + a GitLab token
(--gitlab-token, else $GITLAB_API_TOKEN, else $GITLAB_TOKEN — the token glab
uses). The pod/component probes require in-cluster (or kubeconfig) access to
the target namespace.`,
	Args: cobra.NoArgs,
	RunE: runReleaseSmoke,
}

func init() {
	releaseSmokeCmd.Flags().StringVar(&releaseSmokeNamespaceFlag, "namespace", defaultProdNamespace,
		"Kubernetes namespace whose pods the smoke probes inspect")
	releaseSmokeCmd.Flags().StringVar(&releaseSmokeProjectFlag, "project", defaultReleaseProject,
		"GitLab project path of the release lane (golden-tag source for the drift probe)")
	releaseCmd.AddCommand(releaseSmokeCmd)
}

// smokeProbe is one read-only Step-7 probe result. Signal is the trigger the
// probe failure surfaces (omitted when the probe passes).
type smokeProbe struct {
	Name   string `json:"name"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail"`
	Signal string `json:"signal,omitempty"`
}

// smokeResult is the single JSON object emitted by `gb release smoke`. The
// field set and JSON tags are a STABLE CONTRACT — gardeners (and the haiku
// precursor work, epic ka-0000000264) parse this shape. Add fields additively;
// do not rename or repurpose existing ones.
type smokeResult struct {
	Pass    bool         `json:"pass"`
	Probes  []smokeProbe `json:"probes"`
	Failed  []string     `json:"failed_probes"`
	Signals []string     `json:"signals"`
}

// smokePod is the network-free projection of a Kubernetes pod the pure probe
// classifiers operate on. Ephemeral marks a spawned agent worker pod (carries
// the gardenio.io/agent label), which legitimately churns through
// Pending/Running/Succeeded and is therefore EXCLUDED from the namespace
// health probe.
type smokePod struct {
	Name      string
	Phase     string
	Component string
	Ephemeral bool
}

// passProbe / failProbe construct the two probe outcomes. failProbe stamps the
// trigger signal; passProbe leaves it empty.
func passProbe(name, detail string) smokeProbe {
	return smokeProbe{Name: name, Pass: true, Detail: detail}
}

func failProbe(name, signal, detail string) smokeProbe {
	return smokeProbe{Name: name, Pass: false, Detail: detail, Signal: signal}
}

// isHealthyPodPhase reports whether a long-lived pod phase is acceptable.
// Running is the steady state; Succeeded covers completed one-shot pods (e.g.
// migration jobs) that legitimately linger in the namespace.
func isHealthyPodPhase(phase string) bool {
	return phase == string(corev1.PodRunning) || phase == string(corev1.PodSucceeded)
}

// classifyPodsProbe is the PURE namespace-health probe: given the pod
// projection (and any list error) it passes only when at least one non-ephemeral
// pod is present and every non-ephemeral pod is healthy. Spawned agent worker
// pods are excluded — they cycle through Pending/Succeeded as a matter of
// course and would otherwise flap the probe.
func classifyPodsProbe(pods []smokePod, fetchErr error) smokeProbe {
	const name = "pods-present"
	if fetchErr != nil {
		return failProbe(name, signalUATRegression, fmt.Sprintf("listing pods: %v", fetchErr))
	}
	system := make([]smokePod, 0, len(pods))
	for _, p := range pods {
		if !p.Ephemeral {
			system = append(system, p)
		}
	}
	if len(system) == 0 {
		return failProbe(name, signalUATRegression, "no system pods found in namespace")
	}
	var bad []string
	for _, p := range system {
		if !isHealthyPodPhase(p.Phase) {
			bad = append(bad, fmt.Sprintf("%s=%s", p.Name, p.Phase))
		}
	}
	if len(bad) > 0 {
		return failProbe(name, signalUATRegression,
			fmt.Sprintf("%d/%d pods not Running: %s", len(bad), len(system), strings.Join(bad, ", ")))
	}
	return passProbe(name, fmt.Sprintf("%d system pods healthy", len(system)))
}

// classifyComponentProbe is the PURE single-component liveness probe (probes 2
// and 3): given the pod projection it passes only when at least one pod carries
// the component label and every matching pod is Running. A standby /
// leader-election replica is still phase Running, so "Running" is the correct
// "not Lease/standby" signal (bug ka-0000007021 / G8).
func classifyComponentProbe(name, component string, pods []smokePod, fetchErr error) smokeProbe {
	if fetchErr != nil {
		return failProbe(name, signalUATRegression, fmt.Sprintf("listing pods: %v", fetchErr))
	}
	match := make([]smokePod, 0, len(pods))
	for _, p := range pods {
		if p.Component == component {
			match = append(match, p)
		}
	}
	if len(match) == 0 {
		return failProbe(name, signalUATRegression, fmt.Sprintf("no pod with %s=%s", componentLabel, component))
	}
	var bad []string
	for _, p := range match {
		if p.Phase != string(corev1.PodRunning) {
			bad = append(bad, fmt.Sprintf("%s=%s", p.Name, p.Phase))
		}
	}
	if len(bad) > 0 {
		return failProbe(name, signalUATRegression,
			fmt.Sprintf("%d/%d %s pods not Running: %s", len(bad), len(match), component, strings.Join(bad, ", ")))
	}
	return passProbe(name, fmt.Sprintf("%d %s pod(s) Running", len(match), component))
}

// classifyRoundtripProbe is the PURE daemon round-trip probe (probes 4 and 5):
// it passes only when the round-trip returned no error.
func classifyRoundtripProbe(name string, rtErr error) smokeProbe {
	if rtErr != nil {
		return failProbe(name, signalUATRegression, fmt.Sprintf("round-trip failed: %v", rtErr))
	}
	return passProbe(name, "round-trip ok")
}

// classifyDriftProbe is the PURE prod-drift probe (probe 6): it passes only
// when the running prod controller image tag matches the latest golden tag.
// Either tag missing, or a mismatch, surfaces the prod-drift signal. A single
// failure here is the one-shot drift signal; the caller decides escalation on
// consecutive cycles (Step-7 "drift ≥ 2 cycles").
func classifyDriftProbe(prodTag, goldenTag string, fetchErr error) smokeProbe {
	const name = "prod-drift"
	if fetchErr != nil {
		return failProbe(name, signalProdDrift, fmt.Sprintf("reading golden tag: %v", fetchErr))
	}
	pt := normalizeTag(prodTag)
	gt := normalizeTag(goldenTag)
	if pt == "" {
		return failProbe(name, signalProdDrift, "could not determine prod controller image tag")
	}
	if gt == "" {
		return failProbe(name, signalProdDrift, "no golden release found to compare against")
	}
	if pt != gt {
		return failProbe(name, signalProdDrift, fmt.Sprintf("prod on %s, latest golden %s", prodTag, goldenTag))
	}
	return passProbe(name, fmt.Sprintf("prod and latest golden both %s", goldenTag))
}

// combineSmokeResult is the PURE aggregation core: pass is true only when every
// probe passed; failed_probes and signals are derived from the failing probes.
// Signals are de-duplicated in first-seen (probe) order so the output is
// deterministic. Nil probes normalize to an empty slice for stable JSON.
func combineSmokeResult(probes []smokeProbe) smokeResult {
	if probes == nil {
		probes = []smokeProbe{}
	}
	res := smokeResult{Pass: true, Probes: probes, Failed: []string{}, Signals: []string{}}
	seen := map[string]bool{}
	for _, p := range probes {
		if p.Pass {
			continue
		}
		res.Pass = false
		res.Failed = append(res.Failed, p.Name)
		if p.Signal != "" && !seen[p.Signal] {
			seen[p.Signal] = true
			res.Signals = append(res.Signals, p.Signal)
		}
	}
	return res
}

// normalizeTag strips a leading "v" and surrounding whitespace so a "v2026.x.y"
// release tag and a "2026.x.y" image tag compare equal.
func normalizeTag(tag string) string {
	return strings.TrimPrefix(strings.TrimSpace(tag), "v")
}

// parseImageTag extracts the tag from a container image reference, handling a
// trailing "@sha256:…" digest and a registry-host port colon. Returns "" when
// the reference carries no tag.
func parseImageTag(image string) string {
	image = strings.TrimSpace(image)
	if at := strings.Index(image, "@"); at >= 0 {
		image = image[:at]
	}
	// The tag is the segment after the last ':' that follows the last '/'.
	// A ':' before the last '/' is a registry-host port, not a tag.
	slash := strings.LastIndex(image, "/")
	colon := strings.LastIndex(image, ":")
	if colon <= slash {
		return ""
	}
	return image[colon+1:]
}

// latestGoldenTagName returns the newest calver tag that has a GitLab Release
// (i.e. was promoted to golden), or "" when no golden tag exists. Pure: reuses
// release.go's sortCalverDesc so the newest-first ordering matches the status
// command's golden math exactly.
func latestGoldenTagName(tagNames []string, golden map[string]struct{}) string {
	for _, t := range sortCalverDesc(tagNames) {
		if _, ok := golden[t.name]; ok {
			return t.name
		}
	}
	return ""
}

func runReleaseSmoke(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	ns := strings.TrimSpace(releaseSmokeNamespaceFlag)
	if ns == "" {
		ns = defaultProdNamespace
	}
	project := strings.TrimSpace(releaseSmokeProjectFlag)
	if project == "" {
		project = defaultReleaseProject
	}

	// --- read-only Kubernetes read: one pod list backs probes 1–3 ----------
	pods, controllerImage, podsErr := listSmokePods(ctx, ns)

	// --- read-only GitLab read: latest golden tag for the drift probe -------
	goldenTag, goldenErr := latestGoldenTagFromGitLab(ctx, project)

	probes := []smokeProbe{
		classifyPodsProbe(pods, podsErr),
		classifyComponentProbe("controller-phase", componentAgents, pods, podsErr),
		classifyComponentProbe("slack-bridge", componentSlackBridge, pods, podsErr),
		classifyRoundtripProbe("beads-roundtrip", beadsHealthRoundtrip(ctx)),
		classifyRoundtripProbe("gb-news-roundtrip", gbNewsRoundtrip(ctx)),
		classifyDriftProbe(parseImageTag(controllerImage), goldenTag, goldenErr),
	}

	result := combineSmokeResult(probes)

	if jsonOutput {
		printJSON(result)
	} else {
		printSmokeHuman(cmd, result)
	}

	if !result.Pass {
		// JSON (the contract) is already on stdout; carry the verdict in the
		// exit code without a stderr "Error:" line (mirrors mr.go).
		cmd.SilenceErrors = true
		return errSmokeFailed
	}
	return nil
}

// listSmokePods performs the single read-only pod list backing probes 1–3 and
// extracts the running controller image (for the drift probe). Read-only:
// CoreV1().Pods().List only.
func listSmokePods(ctx context.Context, namespace string) ([]smokePod, string, error) {
	k8s, err := buildK8sClient()
	if err != nil {
		return nil, "", fmt.Errorf("building K8s client: %w", err)
	}
	list, err := k8s.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, "", fmt.Errorf("listing pods in %s: %w", namespace, err)
	}
	pods := make([]smokePod, 0, len(list.Items))
	controllerImage := ""
	for i := range list.Items {
		p := &list.Items[i]
		_, ephemeral := p.Labels[podmanager.LabelAgent]
		component := p.Labels[componentLabel]
		pods = append(pods, smokePod{
			Name:      p.Name,
			Phase:     string(p.Status.Phase),
			Component: component,
			Ephemeral: ephemeral,
		})
		if controllerImage == "" && component == componentAgents {
			controllerImage = controllerContainerImage(p)
		}
	}
	return pods, controllerImage, nil
}

// controllerContainerImage returns the controller container's image from a
// component=agents pod, preferring the container literally named "agents" and
// falling back to the first container.
func controllerContainerImage(p *corev1.Pod) string {
	for _, c := range p.Spec.Containers {
		if c.Name == componentAgents {
			return c.Image
		}
	}
	if len(p.Spec.Containers) > 0 {
		return p.Spec.Containers[0].Image
	}
	return ""
}

// beadsHealthRoundtrip is the `kd list`-shaped beads HTTP/gRPC round-trip
// (probe 4). Read-only: GET /v1/health.
func beadsHealthRoundtrip(ctx context.Context) error {
	if daemon == nil {
		return fmt.Errorf("beads daemon client not initialized")
	}
	return daemon.Health(ctx)
}

// gbNewsRoundtrip is the `gb news`-shaped in-progress query round-trip (probe
// 5): it issues the same in-progress-issue list `gb news` runs, capped at one
// row. Read-only: list query only.
func gbNewsRoundtrip(ctx context.Context) error {
	if daemon == nil {
		return fmt.Errorf("beads daemon client not initialized")
	}
	_, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Statuses: []string{"in_progress"},
		Kinds:    []string{"issue"},
		Limit:    1,
	})
	return err
}

// latestGoldenTagFromGitLab reads the release lane's tags + releases and returns
// the newest golden (released) calver tag. Read-only: ListTags + ListReleases.
func latestGoldenTagFromGitLab(ctx context.Context, project string) (string, error) {
	client, err := newMRGitLabClient()
	if err != nil {
		return "", err
	}
	tags, err := client.ListTags(ctx, project)
	if err != nil {
		return "", err
	}
	tagNames := make([]string, 0, len(tags))
	for _, t := range tags {
		tagNames = append(tagNames, t.Name)
	}
	releases, err := client.ListReleases(ctx, project)
	if err != nil {
		return "", err
	}
	golden := make(map[string]struct{}, len(releases))
	for _, r := range releases {
		if r.UpcomingRel {
			// An upcoming (unpublished) release is not a promotion.
			continue
		}
		golden[r.TagName] = struct{}{}
	}
	return latestGoldenTagName(tagNames, golden), nil
}

// printSmokeHuman renders the verdict for human eyes. The JSON form (--json) is
// the contract; this is a convenience for interactive use.
func printSmokeHuman(cmd *cobra.Command, res smokeResult) {
	verdict := "PASS"
	if !res.Pass {
		verdict = "FAIL"
	}
	cmd.Printf("smoke: %s\n", verdict)
	for _, p := range res.Probes {
		mark := "ok  "
		if !p.Pass {
			mark = "FAIL"
		}
		cmd.Printf("  [%s] %-18s %s\n", mark, p.Name, p.Detail)
	}
	if len(res.Signals) > 0 {
		cmd.Printf("signals: %s\n", strings.Join(res.Signals, ", "))
	}
}
