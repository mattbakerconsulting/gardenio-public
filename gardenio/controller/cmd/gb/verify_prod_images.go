package main

// gb verify-prod-images — the operator-side belt-and-braces check that no
// coverage-instrumented (`-cover`) image variant has leaked onto the prod
// deploy surface. It is the human/CI-drill analogue of the automated defense
// landed by the e2e-coverage epic (ka-0000006199): the Kyverno
// disallow-cover-images ClusterPolicy, which denies at admission time any Pod
// whose container image repo ends with `-cover` outside the E2E namespaces
// (ka-0000003442). (The RWX promote step that refused to tag `-cover` repos,
// ka-0000001728, was deleted with RWX on 2026-09-16.)
//
// This subcommand is the operator-driven line of defense (ka-0000007820,
// gate C5): run it as a post-deploy verification drill in the release runbook.
// It checks two independent surfaces and fails if either shows a cover image:
//
//   - FILE:    the prod deploy-values YAML — any pinned image reference whose
//              repository ends with `-cover`, or whose tag matches the cover
//              tag scheme (main-<sha7> is fine, but `<bin>-cover:<tag>` and the
//              `:cover-*` / `-cover:*` shapes the epic produces are not).
//   - CLUSTER: the live prod namespace's running pod images (every container +
//              initContainer image), via the default kubeconfig, EXCLUDING
//              ephemeral spawned agent worker pods (they legitimately churn and
//              are not part of the deployed release surface).
//
// The cluster cross-check is best-effort: when no cluster is reachable (e.g.
// running the drill from a laptop without prod kubeconfig) the file scan still
// runs and the cluster probe is reported as skipped — a skipped cluster probe
// does NOT fail the command, but a cover image found on EITHER surface does.
//
// EXIT CODE: the JSON verdict is always printed on stdout; the process exits
// NON-ZERO when any cover image is found on any checked surface, so a release
// runbook step can branch on `$?` alone.
//
// READ-ONLY CONTRACT: every code path issues GET/List requests only
// (os.ReadFile, CoreV1().Pods().List). It NEVER deploys, mutates a manifest,
// or touches a bead. Keep it side-effect free — it is an every-release drill.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gardenio/controller/internal/podmanager"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// coverTagRe matches a cover TAG token even when (defensively) the repository
// name does NOT carry the `-cover` suffix — e.g. a hand-edited manifest that
// reuses a normal repo with a cover tag. It fires when the tag carries `cover`
// as a delimited token, covering the task's stated `*-cover-*` and `:cover-*`
// shapes (`cover-main`, `main-cover`, `v1-cover-rc`, a bare `cover`) without
// tripping on incidental substrings like `discovery` or `recover`.
var coverTagRe = regexp.MustCompile(`(?i)(^|[-._])cover([-._]|$)`)

// verifyImagesNamespaceFlag / verifyImagesValuesFlag / verifyImagesSkipCluster
// back the command flags.
var (
	verifyImagesNamespaceFlag string
	verifyImagesValuesFlag    string
	verifyImagesSkipCluster   bool
)

// errCoverImagesFound is the sentinel returned (after the JSON is printed) so
// the process exits non-zero when a cover image is found. cmd.SilenceErrors is
// set first so the JSON on stdout is the only output.
var errCoverImagesFound = errors.New("verify-prod-images: coverage-instrumented image(s) found on a prod surface")

var verifyProdImagesCmd = &cobra.Command{
	Use:   "verify-prod-images",
	Short: "Assert no coverage-instrumented (-cover) image reached the prod deploy surface",
	Long: `Operator drill (release runbook, epic ka-0000006199 gate C5): assert that no
coverage-instrumented image variant (built for E2E runtime coverage) has leaked onto the production deploy surface.

Coverage images (<binary>-cover:<tag>, carrying the org.gardenio.cover=1 OCI
label) run instrumented binaries that write counters to a host volume — they
MUST NEVER run in prod. The Kyverno disallow-cover-images ClusterPolicy is the automated defense
(the RWX promote-refusal gate went with RWX on 2026-09-16); this
command is the operator-side belt-and-braces check.

Two independent surfaces are checked:

  file     the prod deploy-values YAML (--values, default auto-detected):
           any pinned image reference whose repository ends with -cover, or
           whose tag matches the cover / release-candidate tag scheme.
  cluster  the live prod namespace's running pod images (every container and
           initContainer image), via the default kubeconfig, excluding
           ephemeral spawned agent worker pods. Best-effort: a namespace that
           cannot be reached is reported as skipped and does NOT fail the
           command (pass --skip-cluster to skip it deliberately).

Output (one JSON object; field set is a stable contract):

  pass            true only when NO cover image was found on any checked surface
  offenders       [{surface, source, image, reason}] — every cover image found
  file_checked    the deploy-values file that was scanned (empty if none found)
  cluster_probe   scanned | skipped | error  (the cluster cross-check outcome)
  cluster_detail  human detail for the cluster probe (namespace, pod count, or
                  the skip/error reason)

The JSON is always printed; the process exits NON-ZERO when offenders is
non-empty, so a release runbook step can branch on the exit code alone.`,
	Args: cobra.NoArgs,
	RunE: runVerifyProdImages,
}

func init() {
	verifyProdImagesCmd.Flags().StringVar(&verifyImagesNamespaceFlag, "namespace", defaultProdNamespace,
		"prod Kubernetes namespace whose live pod images are cross-checked")
	verifyProdImagesCmd.Flags().StringVar(&verifyImagesValuesFlag, "values", "",
		"path to the prod deploy-values YAML to scan (default: auto-detect under deploy-values/)")
	verifyProdImagesCmd.Flags().BoolVar(&verifyImagesSkipCluster, "skip-cluster", false,
		"skip the live-cluster cross-check (scan the deploy-values file only)")
	rootCmd.AddCommand(verifyProdImagesCmd)
}

// coverOffender is one coverage image found on a prod surface. Surface is
// "file" or "cluster"; Source names the concrete origin (the values-file path,
// or the pod name); Reason explains which signal matched (repo suffix / tag
// scheme).
type coverOffender struct {
	Surface string `json:"surface"`
	Source  string `json:"source"`
	Image   string `json:"image"`
	Reason  string `json:"reason"`
}

// verifyImagesResult is the single JSON object emitted by `gb
// verify-prod-images`. The field set + JSON tags are a STABLE CONTRACT (release
// runbook + any gardener parsing). Add fields additively; never rename or
// repurpose existing ones.
type verifyImagesResult struct {
	Pass          bool            `json:"pass"`
	Offenders     []coverOffender `json:"offenders"`
	FileChecked   string          `json:"file_checked"`
	ClusterProbe  string          `json:"cluster_probe"`
	ClusterDetail string          `json:"cluster_detail"`
}

// classifyImageRef is the PURE cover-image classifier: given an image
// reference it returns a non-empty reason when the reference is a
// coverage-instrumented variant, or "" when it is clean. Two independent
// signals (matching the promote gate's two-way detection):
//
//	repo-suffix  the repository name ends with -cover
//	tag-scheme   the tag carries a cover / release-candidate marker
//
// A plain `main-<sha7>` or a bare calver/latest tag on a non-cover repo is
// clean.
func classifyImageRef(image string) string {
	image = strings.TrimSpace(image)
	if image == "" {
		return ""
	}
	// Repo-suffix signal: the repository NAME (the path component before the
	// tag/digest, after the final '/') ends with -cover. This matches both a
	// full registry reference (…/gardenio/controller-cover:tag) and a BARE
	// repository name (controller-cover) as pinned in deploy-values
	// image.repository fields — the same suffix the promote-refusal gate and
	// the Kyverno disallow-cover-images ClusterPolicy key on.
	if repo := imageRepoName(image); strings.HasSuffix(repo, "-cover") {
		return "repository name carries the -cover suffix"
	}
	// Tag-scheme signal: inspect ONLY the tag component (never the digest or
	// the registry host, which can legitimately contain arbitrary tokens).
	if tag := imageTag(image); tag != "" && coverTagRe.MatchString(tag) {
		return fmt.Sprintf("image tag %q matches the cover / release-candidate scheme", tag)
	}
	return ""
}

// imageRepoName returns the repository basename of an image reference: the
// path component after the final '/' with any :tag and @digest stripped. For a
// bare repository name (no '/', no tag) it returns the name unchanged.
func imageRepoName(image string) string {
	// Strip digest, then tag.
	if base, _, ok := strings.Cut(image, "@"); ok {
		image = base
	}
	lastSlash := strings.LastIndexByte(image, '/')
	name := image
	if lastSlash >= 0 {
		name = image[lastSlash+1:]
	}
	if repo, _, ok := strings.Cut(name, ":"); ok {
		return repo
	}
	return name
}

// imageTag extracts the tag component of an image reference, or "" when the
// reference is untagged / digest-only. It splits on the last '/' first so a
// registry host port (registry:5000/repo) is never mistaken for a tag.
func imageTag(image string) string {
	// Strip any digest — a digest is not a tag.
	if base, _, ok := strings.Cut(image, "@"); ok {
		image = base
	}
	lastSlash := strings.LastIndexByte(image, '/')
	name := image
	if lastSlash >= 0 {
		name = image[lastSlash+1:]
	}
	if _, tag, ok := strings.Cut(name, ":"); ok {
		return tag
	}
	return ""
}

// scanValuesForCoverImages is the PURE file-surface classifier: given the raw
// bytes of a deploy-values YAML it returns every cover-image offender. It scans
// line-oriented for `image:`/`repository:`/`tag:` shaped references rather than
// fully parsing the YAML, because the deploy-values files pin image references
// in several nested shapes (image: <ref>, image.repository + image.tag, bare
// repository: names whose tag comes from the chart appVersion). Reconstructing
// a repository+tag pair from adjacent lines catches the repository-only
// `-cover` suffix even when the tag is supplied elsewhere.
func scanValuesForCoverImages(source string, data []byte) []coverOffender {
	var offenders []coverOffender
	lines := strings.Split(string(data), "\n")
	for i, raw := range lines {
		line := stripYAMLComment(raw)
		key, val := splitYAMLKeyVal(line)
		if val == "" {
			continue
		}
		val = unquoteYAMLScalar(val)
		var candidate string
		switch key {
		case "image", "repository":
			candidate = val
		case "tag":
			// A tag on its own line: reconstruct with the nearest preceding
			// repository:/image: key at a shallower-or-equal indent so a
			// `-cover` repo split across image.repository + image.tag is
			// caught, and a bare cover tag is caught on its own.
			repo := nearestRepository(lines, i)
			if repo != "" {
				candidate = repo + ":" + val
			} else {
				candidate = "any-repo:" + val
			}
		default:
			continue
		}
		if reason := classifyImageRef(candidate); reason != "" {
			offenders = append(offenders, coverOffender{
				Surface: "file",
				Source:  fmt.Sprintf("%s:%d", source, i+1),
				Image:   candidate,
				Reason:  reason,
			})
		}
	}
	return offenders
}

// nearestRepository walks BACKWARD from the tag line to find the closest
// preceding `repository:`/`image:` value, so `image.repository` + `image.tag`
// pairs are reconstructed. It stops at a blank line or after a small window to
// avoid pairing a tag with an unrelated distant repository.
func nearestRepository(lines []string, tagIdx int) string {
	const window = 8
	for i := tagIdx - 1; i >= 0 && i >= tagIdx-window; i-- {
		line := stripYAMLComment(lines[i])
		if strings.TrimSpace(line) == "" {
			return ""
		}
		key, val := splitYAMLKeyVal(line)
		if key == "repository" || key == "image" {
			return unquoteYAMLScalar(val)
		}
	}
	return ""
}

// stripYAMLComment drops an unquoted trailing `#` comment. It is intentionally
// simple: it does not honor `#` inside quoted scalars, which image references
// never contain.
func stripYAMLComment(line string) string {
	if before, _, ok := strings.Cut(line, "#"); ok {
		return before
	}
	return line
}

// splitYAMLKeyVal splits a `key: value` line into (trimmed key, trimmed value).
// Leading `- ` list markers are stripped. Returns ("","") when the line has no
// colon-separated key.
func splitYAMLKeyVal(line string) (string, string) {
	s := strings.TrimSpace(line)
	s = strings.TrimPrefix(s, "- ")
	key, val, ok := strings.Cut(s, ":")
	if !ok {
		return "", ""
	}
	return strings.TrimSpace(key), strings.TrimSpace(val)
}

// unquoteYAMLScalar removes matching surrounding single/double quotes.
func unquoteYAMLScalar(v string) string {
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}

// runVerifyProdImages is the command entrypoint. It scans the file surface,
// optionally cross-checks the live cluster, prints the JSON verdict, and
// returns errCoverImagesFound (non-zero exit) when any offender was found.
func runVerifyProdImages(cmd *cobra.Command, _ []string) error {
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true

	result := verifyImagesResult{Pass: true, Offenders: []coverOffender{}}

	// --- file surface ---
	valuesPath, err := resolveProdValuesPath(verifyImagesValuesFlag)
	if err != nil {
		return err
	}
	if valuesPath != "" {
		data, readErr := os.ReadFile(valuesPath)
		if readErr != nil {
			return fmt.Errorf("reading deploy-values %s: %w", valuesPath, readErr)
		}
		result.FileChecked = valuesPath
		result.Offenders = append(result.Offenders, scanValuesForCoverImages(valuesPath, data)...)
	}

	// --- cluster surface (best-effort) ---
	if verifyImagesSkipCluster {
		result.ClusterProbe = "skipped"
		result.ClusterDetail = "cluster cross-check skipped (--skip-cluster)"
	} else {
		clusterOffenders, probe, detail := scanClusterForCoverImages(cmd.Context(), verifyImagesNamespaceFlag)
		result.ClusterProbe = probe
		result.ClusterDetail = detail
		result.Offenders = append(result.Offenders, clusterOffenders...)
	}

	sortOffenders(result.Offenders)
	result.Pass = len(result.Offenders) == 0

	printJSON(result)
	if !result.Pass {
		return errCoverImagesFound
	}
	return nil
}

// scanClusterForCoverImages lists the target namespace's pods and returns every
// cover-image offender across their container + initContainer images. Ephemeral
// spawned agent worker pods (they carry the podmanager agent label) are
// excluded — they are not part of the deployed release surface and legitimately
// run cover images during E2E. The probe result is "scanned" on success,
// "error" when the namespace cannot be listed (best-effort, non-fatal).
func scanClusterForCoverImages(ctx context.Context, namespace string) (offenders []coverOffender, probe, detail string) {
	k8s, err := buildK8sClient()
	if err != nil {
		return nil, "error", fmt.Sprintf("no Kubernetes client (%v) — file scan only", err)
	}
	list, err := k8s.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, "error", fmt.Sprintf("listing pods in %s failed (%v) — file scan only", namespace, err)
	}
	scanned := 0
	for i := range list.Items {
		p := &list.Items[i]
		if _, ephemeral := p.Labels[podmanager.LabelAgent]; ephemeral {
			continue // spawned worker pods legitimately run -cover during E2E
		}
		scanned++
		for _, c := range p.Spec.InitContainers {
			if reason := classifyImageRef(c.Image); reason != "" {
				offenders = append(offenders, coverOffender{Surface: "cluster", Source: p.Name, Image: c.Image, Reason: reason})
			}
		}
		for _, c := range p.Spec.Containers {
			if reason := classifyImageRef(c.Image); reason != "" {
				offenders = append(offenders, coverOffender{Surface: "cluster", Source: p.Name, Image: c.Image, Reason: reason})
			}
		}
	}
	return offenders, "scanned", fmt.Sprintf("namespace %s: %d non-ephemeral pod(s) checked", namespace, scanned)
}

// resolveProdValuesPath returns the deploy-values file to scan. An explicit
// --values wins. Otherwise it auto-detects the prod values file under
// deploy-values/ from the repo root, preferring gardenio-k3s.yaml (the
// prod-cluster overlay release-deploy.sh uses) then any other gardenio-*prod*.yaml. Returns
// ("", nil) when no prod values file is found (the file surface is then
// skipped and only the cluster is checked).
func resolveProdValuesPath(explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", fmt.Errorf("--values %s: %w", explicit, err)
		}
		return explicit, nil
	}
	root := findRepoRootForSizing()
	dir := filepath.Join(root, "deploy-values")
	// Preferred canonical prod overlays, in order.
	for _, name := range []string{"gardenio-k3s.yaml", "gardenio-prod.yaml"} {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	// Fallback: first gardenio-*prod*.yaml (never a .tmpl, which is not a
	// deployed manifest).
	matches, _ := filepath.Glob(filepath.Join(dir, "gardenio-*prod*.yaml"))
	sort.Strings(matches)
	for _, m := range matches {
		if strings.HasSuffix(m, ".tmpl") {
			continue
		}
		return m, nil
	}
	return "", nil
}

// sortOffenders orders offenders deterministically (surface, source, image) so
// the JSON output is stable across runs.
func sortOffenders(offenders []coverOffender) {
	sort.Slice(offenders, func(i, j int) bool {
		a, b := offenders[i], offenders[j]
		if a.Surface != b.Surface {
			return a.Surface < b.Surface
		}
		if a.Source != b.Source {
			return a.Source < b.Source
		}
		return a.Image < b.Image
	})
}
