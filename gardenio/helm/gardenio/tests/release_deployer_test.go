package tests

import (
	"fmt"
	"os/exec"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// Task 6: `release-deployer` CronJob + least-privilege RBAC (default OFF).
//
// A CronJob that shallow-clones the newest calver tag of this repo and runs
// gardenio/scripts/release-deploy.sh (Task 5) against the live helm release.
// Five requirements, one test group each:
//
//   1. enabled=false renders nothing.
//   2. CronJob spec: concurrencyPolicy Forbid, suspend from values,
//      startingDeadlineSeconds 600, successful/failedJobsHistoryLimit 3,
//      backoffLimit 0, activeDeadlineSeconds >= 3600 (coordinator override —
//      release-deploy.sh's worst case is a 20m --atomic upgrade + rollback,
//      then verify, then a rollback --wait: a kill must never land mid-rollback).
//      Tolerates the agent-pool taint the same way the controller's own pod
//      does (gardenio.tolerations helper, not a nodeSelector — this is batch
//      work with no on-demand requirement).
//   3. Entrypoint ConfigMap script resolves the newest calver tag via
//      `git ls-remote --tags` + `sort -V | tail -1`, shallow-clones it, and
//      runs release-deploy.sh with KD_URL/KD_TOKEN/KD_OPERATED_BY wired,
//      remapping the script's exit 3 (skip) to 0 (a skip is a successful Job).
//   4. RBAC: namespaced Role is broad (operator-accepted `*`/`*` in the
//      release namespace); the ClusterRole is get/list/watch on `*`
//      cluster-wide plus create/update/patch/delete ONLY on the cluster-scoped
//      kinds this chart actually renders (pinned by a full render of
//      deploy-values/gardenio-k3s.yaml), with resourceNames restricting the
//      three statically-named ones.
//   5. automountServiceAccountToken: true on the Pod only, not the
//      ServiceAccount.

func releaseDeployerRepoRootValues() []string {
	return []string{"../../../deploy-values/gardenio-k3s.yaml"}
}

// --- Requirement 1: default off ---

func TestReleaseDeployer_DisabledByDefault_RendersNothing(t *testing.T) {
	t.Parallel()
	for _, tmpl := range []string{
		"templates/release-deployer/cronjob.yaml",
		"templates/release-deployer/rbac.yaml",
		"templates/release-deployer/entrypoint-configmap.yaml",
	} {
		out, err := helmTemplateAllowFail(t, tmpl)
		rendersNothing(t, out, err)
	}
}

// --- Requirement 2: CronJob spec ---

func TestReleaseDeployer_CronJobSpec_Fields(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/release-deployer/cronjob.yaml",
		"releaseDeployer.enabled=true",
	)
	if !strings.Contains(out, "kind: CronJob") {
		t.Fatalf("expected a CronJob; got:\n%s", out)
	}
	for _, want := range []string{
		"concurrencyPolicy: Forbid",
		"startingDeadlineSeconds: 600",
		"successfulJobsHistoryLimit: 3",
		"failedJobsHistoryLimit: 3",
		"backoffLimit: 0",
		"suspend: false",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in rendered CronJob; got:\n%s", want, out)
		}
	}

	adlRE := regexp.MustCompile(`activeDeadlineSeconds:\s*(\d+)`)
	m := adlRE.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("expected activeDeadlineSeconds in rendered CronJob; got:\n%s", out)
	}
	adl, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("could not parse activeDeadlineSeconds %q: %v", m[1], err)
	}
	if adl < 3600 {
		t.Errorf("activeDeadlineSeconds must be at least 3600 (worst case: 20m atomic upgrade+rollback, verify, then a rollback --wait) — got %d", adl)
	}
}

// TestReleaseDeployer_HOMEIsSetUnderReadOnlyRootFilesystem guards against a
// container with readOnlyRootFilesystem: true whose HOME still points at the
// (unwritable) image default -- git/helm/kubectl all write caches/config
// under HOME, so `git config --global` (and anything else that touches
// HOME) fails every run unless HOME is redirected to a writable volume.
func TestReleaseDeployer_HOMEIsSetUnderReadOnlyRootFilesystem(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/release-deployer/cronjob.yaml",
		"releaseDeployer.enabled=true",
	)
	if !strings.Contains(out, "readOnlyRootFilesystem: true") {
		t.Fatalf("expected readOnlyRootFilesystem: true; got:\n%s", out)
	}
	if !regexp.MustCompile(`(?m)^\s*-\s*name:\s*HOME\s*$`).MatchString(out) {
		t.Errorf("expected a HOME env var (readOnlyRootFilesystem is true, so the image's default HOME isn't writable); got:\n%s", out)
	}
	if !strings.Contains(out, "value: /tmp") {
		t.Errorf("expected HOME to point at the writable /tmp emptyDir; got:\n%s", out)
	}
}

func TestReleaseDeployer_CronJobSpec_ScheduleAndSuspendFromValues(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/release-deployer/cronjob.yaml",
		"releaseDeployer.enabled=true",
		`releaseDeployer.schedule=*/5 * * * *`,
		"releaseDeployer.suspend=true",
	)
	if !strings.Contains(out, `schedule: "*/5 * * * *"`) {
		t.Errorf("expected schedule to come from values; got:\n%s", out)
	}
	if !strings.Contains(out, "suspend: true") {
		t.Errorf("expected suspend: true when releaseDeployer.suspend=true; got:\n%s", out)
	}
}

func TestReleaseDeployer_CronJobSpec_TeratesAgentPoolTaint(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/release-deployer/cronjob.yaml",
		"releaseDeployer.enabled=true",
		"global.tolerations[0].key=gardenio.agent",
		"global.tolerations[0].operator=Equal",
		"global.tolerations[0].value=true",
		"global.tolerations[0].effect=NoSchedule",
	)
	if !strings.Contains(out, "tolerations:") {
		t.Fatalf("expected a tolerations block when global.tolerations is set; got:\n%s", out)
	}
	if !strings.Contains(out, "key: gardenio.agent") {
		t.Errorf("expected the agent-pool taint toleration to be present (same mechanism as the controller's own pod — no on-demand requirement, this is batch work); got:\n%s", out)
	}
}

func TestReleaseDeployer_Image_DefaultsToAgentImage_OverrideHonored(t *testing.T) {
	t.Parallel()
	def := helmTemplate(t, "templates/release-deployer/cronjob.yaml",
		"releaseDeployer.enabled=true",
	)
	if !strings.Contains(def, "/agent:") {
		t.Errorf("with releaseDeployer.image empty, expected the CLI-tooling agent image (agents.agentImage); got:\n%s", def)
	}

	override := helmTemplate(t, "templates/release-deployer/cronjob.yaml",
		"releaseDeployer.enabled=true",
		"releaseDeployer.image=my-registry.example.com/custom-deployer:v1",
	)
	if !strings.Contains(override, "my-registry.example.com/custom-deployer:v1") {
		t.Errorf("expected releaseDeployer.image override to be used verbatim when set; got:\n%s", override)
	}
}

// --- Requirement 3: entrypoint script ---

func TestReleaseDeployer_EntrypointScript_ResolvesNewestCalverTag(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/release-deployer/entrypoint-configmap.yaml",
		"releaseDeployer.enabled=true",
	)
	if !strings.Contains(out, "kind: ConfigMap") {
		t.Fatalf("expected a ConfigMap; got:\n%s", out)
	}
	if !strings.Contains(out, "git ls-remote --tags") {
		t.Errorf("expected the script to discover tags via git ls-remote --tags; got:\n%s", out)
	}
	if !strings.Contains(out, `sort -V | tail -1`) {
		t.Errorf("expected the script to pick the newest calver via sort -V | tail -1; got:\n%s", out)
	}
	if !strings.Contains(out, `20[0-9][0-9]\.[0-9]`) && !strings.Contains(out, `20[0-9]{2}\.[0-9]`) {
		t.Errorf("expected the script to filter tags with a calver regex (^20YY.N.N); got:\n%s", out)
	}
}

func TestReleaseDeployer_EntrypointScript_ShallowClonesAndInvokesReleaseDeploy(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/release-deployer/entrypoint-configmap.yaml",
		"releaseDeployer.enabled=true",
	)
	if !strings.Contains(out, "--depth") {
		t.Errorf("expected a shallow clone (--depth); got:\n%s", out)
	}
	if !strings.Contains(out, "gardenio/scripts/release-deploy.sh") {
		t.Errorf("expected the script to invoke gardenio/scripts/release-deploy.sh; got:\n%s", out)
	}
}

func TestReleaseDeployer_EntrypointScript_EnvWiring(t *testing.T) {
	t.Parallel()
	cronOut := helmTemplate(t, "templates/release-deployer/cronjob.yaml",
		"releaseDeployer.enabled=true",
	)
	entryOut := helmTemplate(t, "templates/release-deployer/entrypoint-configmap.yaml",
		"releaseDeployer.enabled=true",
	)

	if !strings.Contains(cronOut, "- name: KD_URL") {
		t.Errorf("expected KD_URL env var on the release-deployer container; got:\n%s", cronOut)
	}
	if !strings.Contains(cronOut, `http://test-beads:8080`) {
		t.Errorf("expected KD_URL to resolve to the in-cluster beads service on port 8080; got:\n%s", cronOut)
	}
	if !strings.Contains(cronOut, "- name: KD_TOKEN") {
		t.Errorf("expected KD_TOKEN env var sourced from the kdTokenSecret; got:\n%s", cronOut)
	}
	if !strings.Contains(cronOut, "kd-beads-token") {
		t.Errorf("expected the default kdTokenSecret name kd-beads-token to be wired; got:\n%s", cronOut)
	}
	if !strings.Contains(cronOut, "- name: KD_OPERATED_BY") {
		t.Errorf("expected KD_OPERATED_BY env var on the release-deployer container; got:\n%s", cronOut)
	}
	if !strings.Contains(entryOut, "KD_URL") || !strings.Contains(entryOut, "KD_TOKEN") || !strings.Contains(entryOut, "KD_OPERATED_BY") {
		t.Errorf("expected the entrypoint script to pass KD_URL/KD_TOKEN/KD_OPERATED_BY through to release-deploy.sh; got:\n%s", entryOut)
	}
	// release-deploy.sh's rule 4 shells out to `gh api` against the private
	// repo -- gh must be authenticated via GH_TOKEN (a process env var the
	// entrypoint exports from the mounted git-creds Secret file, not a pod
	// env var -- ka-0000002787), or every run 404s.
	if !strings.Contains(entryOut, "export GH_TOKEN=") {
		t.Errorf("expected the entrypoint script to export GH_TOKEN so `gh` is authenticated; got:\n%s", entryOut)
	}
}

// TestReleaseDeployer_EntrypointScript_TagResolutionSurvivesNoMatch guards
// against `set -o pipefail` killing the script before the friendly "no
// calver tags found" message: when `git ls-remote` returns no matching
// tags, the `grep -oE` stage exits 1, and without a guard that trips
// pipefail immediately -- the fallback empty-TAG check must never be
// reachable only when grep happens to match something.
func TestReleaseDeployer_EntrypointScript_TagResolutionSurvivesNoMatch(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/release-deployer/entrypoint-configmap.yaml",
		"releaseDeployer.enabled=true",
	)
	if !regexp.MustCompile(`grep -oE[^\n|]*\|\|\s*true`).MatchString(out) {
		t.Errorf("expected the tag-filtering grep to be guarded with `|| true` so a no-match doesn't trip `set -o pipefail` before the empty-TAG check; got:\n%s", out)
	}
}

func TestReleaseDeployer_EntrypointScript_ExitThreeBecomesExitZero(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/release-deployer/entrypoint-configmap.yaml",
		"releaseDeployer.enabled=true",
	)
	// A skip (exit 3) from release-deploy.sh must be treated as a successful
	// Job -- the entrypoint must inspect the exit code and remap 3 -> 0
	// while every other non-zero status propagates unchanged.
	if !strings.Contains(out, "3") {
		t.Fatalf("expected the script to special-case exit code 3; got:\n%s", out)
	}
	if !regexp.MustCompile(`(?s)==\s*3\).*exit 0`).MatchString(out) &&
		!regexp.MustCompile(`(?s)-eq 3.*exit 0`).MatchString(out) {
		t.Errorf("expected exit code 3 (skip) from release-deploy.sh to be remapped to exit 0; got:\n%s", out)
	}
}

// --- Requirement 4: RBAC ---

// releaseDeployerClusterScopedKinds is the closed set of cluster-scoped
// object kinds this chart renders under deploy-values/gardenio-k3s.yaml,
// keyed by the apiGroup + resource-plural the ClusterRole must grant on.
// Kinds with a static (values-default, release-independent) name are
// checked against that one literal name; kinds with a dynamic
// (release-fullname-prefixed) name are checked against every name actually
// observed in a full-chart render (the positive control: a newly rendered
// object of that kind whose name is missing from resourceNames fails this
// test, not just a kind-level set-equality pin).
var releaseDeployerClusterScopedKinds = map[string]struct {
	apiGroup   string
	resource   string
	staticName string // "" for release-prefixed (dynamic) names
}{
	"ClusterKeycloak":                  {"v1.edp.epam.com", "clusterkeycloaks", "gardenio-keycloak-runtime"},
	"ClusterRole":                      {"rbac.authorization.k8s.io", "clusterroles", ""},
	"ClusterRoleBinding":               {"rbac.authorization.k8s.io", "clusterrolebindings", ""},
	"PriorityClass":                    {"scheduling.k8s.io", "priorityclasses", ""}, // four tiers: every rendered name
	"StorageClass":                     {"storage.k8s.io", "storageclasses", "local-path-retain"},
	"ValidatingAdmissionPolicy":        {"admissionregistration.k8s.io", "validatingadmissionpolicies", ""},
	"ValidatingAdmissionPolicyBinding": {"admissionregistration.k8s.io", "validatingadmissionpolicybindings", ""},
}

// namespaceMetadataRE matches a `namespace:` field at the metadata-block
// indent level (2 spaces) used throughout this chart -- as opposed to, e.g.,
// a `namespace:` nested inside a ClusterRoleBinding's `subjects:` list
// (4+ spaces), which must NOT make the object look namespaced.
var namespaceMetadataRE = regexp.MustCompile(`(?m)^  namespace:\s*\S`)

// nameMetadataRE matches a `name:` field at the metadata-block indent level
// (2 spaces), mirroring namespaceMetadataRE.
var nameMetadataRE = regexp.MustCompile(`(?m)^  name:\s*"?([^"\s]+)"?\s*$`)

// clusterScopedKindCounts returns, for each Kind with no metadata-level
// namespace field, how many times it appears in the rendered multi-doc YAML.
func clusterScopedKindCounts(rendered string) map[string]int {
	counts := make(map[string]int)
	for doc := range strings.SplitSeq(rendered, "\n---") {
		m := kindLineRE.FindStringSubmatch(doc)
		if m == nil {
			continue
		}
		if namespaceMetadataRE.MatchString(doc) {
			continue
		}
		counts[m[1]]++
	}
	return counts
}

// clusterScopedObjectNames returns, for each Kind with no metadata-level
// namespace field, every object name observed in the rendered multi-doc YAML.
func clusterScopedObjectNames(rendered string) map[string][]string {
	names := make(map[string][]string)
	for doc := range strings.SplitSeq(rendered, "\n---") {
		m := kindLineRE.FindStringSubmatch(doc)
		if m == nil {
			continue
		}
		if namespaceMetadataRE.MatchString(doc) {
			continue
		}
		nm := nameMetadataRE.FindStringSubmatch(doc)
		if nm == nil {
			continue
		}
		names[m[1]] = append(names[m[1]], nm[1])
	}
	return names
}

// clusterRoleRuleBlocks splits a rendered ClusterRole document's `rules:`
// list into its individual `- apiGroups: ...` entries.
func clusterRoleRuleBlocks(roleOut string) []string {
	idx := strings.Index(roleOut, "\nrules:")
	if idx < 0 {
		return nil
	}
	parts := strings.Split(roleOut[idx:], "\n  - apiGroups:")
	var blocks []string
	for _, p := range parts[1:] {
		blocks = append(blocks, "apiGroups:"+p)
	}
	return blocks
}

func TestReleaseDeployer_ClusterRole_MatchesRenderedClusterScopedKinds(t *testing.T) {
	t.Parallel()
	// Also rendered with beads.rbacProbe.enabled: its <beads>-rbac-reader
	// ClusterRole + binding render only then, so a deploy-values-only render
	// left their resourceNames entry unchecked (gd-S2DYdvSqdg).
	for _, tc := range []struct {
		name string
		set  []string
	}{
		{"deploy-values/gardenio-k3s.yaml", nil},
		{"deploy-values/gardenio-k3s.yaml+rbacProbe", []string{"beads.rbacProbe.enabled=true"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checkReleaseDeployerClusterRoleMatchesRendered(t, append([]string{"releaseDeployer.enabled=true"}, tc.set...)...)
		})
	}
}

func checkReleaseDeployerClusterRoleMatchesRendered(t *testing.T, set ...string) {
	t.Helper()
	full := helmTemplateFull(t, releaseDeployerRepoRootValues(), set...)

	observedCounts := clusterScopedKindCounts(full)
	var observedKinds []string
	for k, n := range observedCounts {
		if n > 0 {
			observedKinds = append(observedKinds, k)
		}
	}
	sort.Strings(observedKinds)

	var wantKinds []string
	for k := range releaseDeployerClusterScopedKinds {
		wantKinds = append(wantKinds, k)
	}
	sort.Strings(wantKinds)

	if strings.Join(observedKinds, ",") != strings.Join(wantKinds, ",") {
		t.Fatalf("cluster-scoped kinds rendered by the full chart changed -- update releaseDeployerClusterScopedKinds to match.\nobserved: %v\nwant:     %v", observedKinds, wantKinds)
	}
	observedNames := clusterScopedObjectNames(full)

	// Rendered WITH the deploy-values overlay (and the test's release name):
	// the VAP names for agentInfra grants only exist when the grants do.
	rbacOut := full
	// rbac.yaml also renders namespaced Roles with their own apiGroups: ["*"]
	// entry -- isolate just the deployer's ClusterRole document so the checks
	// below aren't polluted by the Role's rules or other charts' ClusterRoles.
	var roleOut string
	for doc := range strings.SplitSeq(rbacOut, "\n---") {
		if strings.Contains(doc, "kind: ClusterRole\n") && strings.Contains(doc, "app.kubernetes.io/component: release-deployer") {
			roleOut = doc
			break
		}
	}
	if roleOut == "" {
		t.Fatalf("expected a ClusterRole; got:\n%s", rbacOut)
	}

	// No blanket read rule: apiGroups/resources "*" matches every instance
	// of every resource cluster-wide, including NAMESPACED kinds (e.g.
	// Secrets) in every namespace -- this SA must only ever be granted on
	// the specific cluster-scoped kinds it manages, below.
	if strings.Contains(roleOut, `apiGroups: ["*"]`) || strings.Contains(roleOut, `resources: ["*"]`) {
		t.Errorf("expected no apiGroups/resources \"*\" rule in the ClusterRole (that also grants namespaced kinds like Secrets cluster-wide); got:\n%s", roleOut)
	}

	blocks := clusterRoleRuleBlocks(roleOut)

	// Exactly two rules per distinct apiGroup: one unrestricted
	// create/list/watch, one resourceNames-restricted get/update/patch/
	// delete (kinds sharing an apiGroup, e.g. ClusterRole/ClusterRoleBinding,
	// are combined into the same pair of rules via a multi-value
	// `resources:` list). Pins the rule count so an accidental extra grant
	// is caught. Plus exactly four non-kind rules, each held only so a chart
	// ClusterRole that grants it can be re-applied
	// (TestReleaseDeployer_HoldsEveryChartRoleGrant): get/patch on the release
	// namespace (psa-labels hook), nodes get/list/watch/patch
	// (gardenio-ha-drill-operator), persistentvolumes list/patch/delete
	// (<agents>-pv-gc), get/list on roles/rolebindings/clusterroles/
	// clusterrolebindings (<beads>-rbac-reader). With land namespaces on, two
	// more: escalate on gardenio-land-agent-manager + <agents>-lands-namespaces,
	// bind on <agents>-lands-namespaces (TestReleaseDeployer_LandNamespaceRoles).
	// With agents.infraReader on, one more: escalate + bind on
	// gardenio-infra-reader (TestReleaseDeployer_InfraReaderRoles). With
	// releaseDeployer.edgeNamespace set, one more: get/patch on that namespace
	// (release-deploy.sh rule 4c, TestReleaseDeployer_EdgeNamespaceRole).
	extraRules := 4
	if strings.Contains(roleOut, `resourceNames: ["public-edge"]`) {
		extraRules++
	}
	if slices.Contains(observedNames["ClusterRole"], "gardenio-infra-reader") {
		extraRules++
	}
	if slices.Contains(observedNames["ClusterRole"], "gardenio-land-agent-manager") {
		extraRules += 2
	}
	wantGroups := map[string]bool{}
	for _, v := range releaseDeployerClusterScopedKinds {
		wantGroups[v.apiGroup] = true
	}
	if len(blocks) != 2*len(wantGroups)+extraRules {
		t.Fatalf("expected exactly %d rules in the ClusterRole (create/list/watch + get/update/patch/delete per %d kind-group, plus %d rules held for chart roles), got %d; got:\n%s",
			2*len(wantGroups)+extraRules, len(wantGroups), extraRules, len(blocks), roleOut)
	}

	findBlock := func(apiGroup, resource string, verbsSubstr string) (string, bool) {
		for _, b := range blocks {
			if strings.Contains(b, `"`+apiGroup+`"`) && strings.Contains(b, `"`+resource+`"`) && strings.Contains(b, verbsSubstr) {
				return b, true
			}
		}
		return "", false
	}

	for kind, spec := range releaseDeployerClusterScopedKinds {
		createBlock, ok := findBlock(spec.apiGroup, spec.resource, `verbs: ["create", "list", "watch"]`)
		if !ok {
			t.Errorf("kind %s: expected an unrestricted create/list/watch rule for apiGroup %q resource %q; got:\n%s", kind, spec.apiGroup, spec.resource, roleOut)
		} else if strings.Contains(createBlock, "resourceNames") {
			t.Errorf("kind %s: create/list/watch rule must NOT carry resourceNames (create can't be name-restricted); got:\n%s", kind, createBlock)
		}

		writeBlock, ok := findBlock(spec.apiGroup, spec.resource, `verbs: ["get", "update", "patch", "delete"]`)
		if !ok {
			t.Errorf("kind %s: expected a resourceNames-restricted get/update/patch/delete rule for apiGroup %q resource %q; got:\n%s", kind, spec.apiGroup, spec.resource, roleOut)
			continue
		}

		var wantNames []string
		if spec.staticName != "" {
			wantNames = []string{spec.staticName}
		} else {
			wantNames = observedNames[kind]
			if len(wantNames) == 0 {
				t.Errorf("kind %s: no rendered object names observed to positive-control against", kind)
				continue
			}
		}
		// Positive control: every actually-rendered name of this kind must
		// appear in resourceNames, so a newly rendered object of this kind
		// whose name isn't listed here fails the test.
		for _, name := range wantNames {
			if !strings.Contains(writeBlock, `"`+name+`"`) {
				t.Errorf("kind %s: rendered object name %q is missing from the ClusterRole's resourceNames; got:\n%s", kind, name, writeBlock)
			}
		}
	}
}

// foreignNamespaceResources maps each namespaced Kind the chart may render
// outside the release namespace to the resource plural a Role must grant on.
// A new foreign-namespace Kind fails the test below until it is added here.
var foreignNamespaceResources = map[string]string{
	"Secret":        "secrets",
	"ConfigMap":     "configmaps",
	"Role":          "roles",
	"RoleBinding":   "rolebindings",
	"NetworkPolicy": "networkpolicies",
	"ResourceQuota": "resourcequotas",
	"LimitRange":    "limitranges",
	"DaemonSet":     "daemonsets",
}

var namespaceValueRE = regexp.MustCompile(`(?m)^  namespace:\s*"?([^"\s]+)"?\s*$`)

// helmTemplateFullNamespaced is like helmTemplateFull but pins the rendered
// release namespace via --namespace instead of leaving it to helm's default
// (the HELM_NAMESPACE env var, or the current kube context). Use this for
// any test that classifies rendered objects as foreign by comparing against
// a fixed release namespace constant.
func helmTemplateFullNamespaced(t *testing.T, namespace string, valuesFiles []string, setFlags ...string) string {
	t.Helper()
	args := []string{"template", "test", chartDir(t), "--namespace", namespace}
	for _, f := range valuesFiles {
		args = append(args, "-f", chartDir(t)+"/"+f)
	}
	for _, s := range setFlags {
		args = append(args, "--set", s)
	}
	cmd := exec.Command("helm", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, out)
	}
	return string(out)
}

// Positive control for the 2026.266.3 failure: helm reads every object it
// owns on every upgrade and rollback, so an object rendered OUTSIDE the release
// namespace (where the */* Role applies) must be covered by a Role in its own
// namespace that names it, or every autonomous release fails.
func TestReleaseDeployer_ForeignNamespaceObjectsAreGranted(t *testing.T) {
	t.Parallel()
	// Pin the release namespace explicitly rather than relying on
	// helmTemplateFull's implicit default: helm falls back to the
	// HELM_NAMESPACE env var (or the current kube context) when no
	// --namespace flag is given, so this classification would otherwise
	// depend on the caller's shell (e.g. "arc-runners" on the CI runner)
	// instead of always matching releaseNs.
	const releaseNs = "default"
	// With and without the ClusterKeycloak CA ConfigMap (gd-PhU6jPtSxa); caCert
	// stands in for the lookup, which helm template cannot do.
	for _, tc := range []struct {
		name string
		set  []string
	}{
		{"default", []string{"releaseDeployer.enabled=true"}},
		{"clusterKeycloakTLS", []string{"releaseDeployer.enabled=true", "keycloak.runtime.clusterKeycloak.tls=true", "keycloak.runtime.clusterKeycloak.caCert=test-ca"}},
		// The k3s gVisor installer's DaemonSet + ConfigMap live in kube-system.
		{"gvisorK3sInstaller", []string{"releaseDeployer.enabled=true", "gvisor.enabled=true", "gvisor.k3sInstaller.enabled=true"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			full := helmTemplateFullNamespaced(t, releaseNs, releaseDeployerRepoRootValues(), tc.set...)
			checkForeignNamespaceObjectsAreGranted(t, releaseNs, full)
		})
	}
}

func checkForeignNamespaceObjectsAreGranted(t *testing.T, releaseNs, full string) {
	t.Helper()

	type obj struct{ kind, ns, name string }
	var foreign []obj
	roles := map[string]string{} // namespace -> the deployer Role doc there
	for doc := range strings.SplitSeq(full, "\n---") {
		k := kindLineRE.FindStringSubmatch(doc)
		ns := namespaceValueRE.FindStringSubmatch(doc)
		nm := nameMetadataRE.FindStringSubmatch(doc)
		if k == nil || ns == nil || nm == nil || ns[1] == releaseNs {
			continue
		}
		foreign = append(foreign, obj{k[1], ns[1], nm[1]})
		if k[1] == "Role" && strings.HasSuffix(nm[1], "-release-deployer") {
			roles[ns[1]] = doc
		}
	}
	if len(foreign) == 0 {
		t.Fatal("expected at least the keycloak-runtime admin Secret copy outside the release namespace; a render with none means this control no longer controls anything")
	}
	for _, o := range foreign {
		resource, ok := foreignNamespaceResources[o.kind]
		if !ok {
			t.Errorf("%s %s/%s is rendered outside the release namespace; add its resource to foreignNamespaceResources and grant it in release-deployer/rbac.yaml", o.kind, o.ns, o.name)
			continue
		}
		role, ok := roles[o.ns]
		if !ok {
			t.Errorf("%s %s/%s: no release-deployer Role in namespace %q", o.kind, o.ns, o.name, o.ns)
			continue
		}
		granted := false
		for _, b := range clusterRoleRuleBlocks(role) {
			if strings.Contains(b, `"`+resource+`"`) && strings.Contains(b, `"`+o.name+`"`) && strings.Contains(b, `"get"`) && strings.Contains(b, `"patch"`) {
				granted = true
			}
		}
		if !granted {
			t.Errorf("%s %s/%s: the release-deployer Role there does not grant get+patch on %s named %q; got:\n%s", o.kind, o.ns, o.name, resource, o.name, role)
		}
	}
	// No create/delete on secrets in any foreign namespace (operator decision
	// 2026-09-23): delete+create+get on one name lets the deployer re-create
	// that Secret as a service-account-token Secret for a foreign SA and read
	// the token back. First creation of the copy is an admin step instead.
	// The ClusterKeycloak CA ConfigMap gets the same shape (gd-PhU6jPtSxa).
	for ns, role := range roles {
		for _, b := range clusterRoleRuleBlocks(role) {
			for _, res := range []string{"secrets", "configmaps"} {
				if !strings.Contains(b, `"`+res+`"`) && !strings.Contains(b, `"*"`) {
					continue
				}
				for _, verb := range []string{`"create"`, `"delete"`, `"deletecollection"`, `"*"`} {
					if strings.Contains(b, verb) {
						t.Errorf("release-deployer Role in namespace %q grants %s on %s; it must hold only get/update/patch on the named object; got:\n%s", ns, verb, res, b)
					}
				}
			}
		}
	}
}

func TestReleaseDeployer_RBAC_NamespacedRoleAndServiceAccount(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/release-deployer/rbac.yaml",
		"releaseDeployer.enabled=true",
	)
	if !strings.Contains(out, "kind: ServiceAccount") {
		t.Errorf("expected a ServiceAccount; got:\n%s", out)
	}
	if !strings.Contains(out, "kind: Role\n") {
		t.Errorf("expected a namespaced Role; got:\n%s", out)
	}
	if !strings.Contains(out, `apiGroups: ["*"]`) || !strings.Contains(out, `resources: ["*"]`) {
		t.Errorf("expected the namespaced Role to grant */* in the release namespace (operator-accepted); got:\n%s", out)
	}
}

// --- Requirement 5: automountServiceAccountToken on the pod only ---

func TestReleaseDeployer_AutomountToken_OnPodOnly(t *testing.T) {
	t.Parallel()
	cronOut := helmTemplate(t, "templates/release-deployer/cronjob.yaml",
		"releaseDeployer.enabled=true",
	)
	if !strings.Contains(cronOut, "automountServiceAccountToken: true") {
		t.Errorf("expected automountServiceAccountToken: true on the pod spec; got:\n%s", cronOut)
	}

	rbacOut := helmTemplate(t, "templates/release-deployer/rbac.yaml",
		"releaseDeployer.enabled=true",
	)
	for doc := range strings.SplitSeq(rbacOut, "\n---") {
		if !strings.Contains(doc, "kind: ServiceAccount") {
			continue
		}
		if strings.Contains(doc, "automountServiceAccountToken: true") {
			t.Errorf("ServiceAccount must NOT set automountServiceAccountToken: true itself (pod-level only); got:\n%s", doc)
		}
	}
}

// The psa-labels pre-upgrade hook re-creates a ClusterRole granting get/patch
// on the release namespace on every upgrade. RBAC escalation prevention means
// the release-deployer SA can only create that role if it already holds the
// same permission, so its ClusterRole must carry the matching rule. 2026.284.2
// failed its autonomous roll exactly here (helm rolled back).
func TestReleaseDeployer_ClusterRole_HoldsPSALabelsNamespaceGrant(t *testing.T) {
	t.Parallel()
	psa := helmTemplate(t, "templates/namespace-psa.yaml")
	if !strings.Contains(psa, `resources: ["namespaces"]`) {
		t.Fatalf("psa-labels ClusterRole no longer grants namespaces; revisit this test.\n%s", psa)
	}
	rbacOut := helmTemplate(t, "templates/release-deployer/rbac.yaml", "releaseDeployer.enabled=true")
	idx := strings.Index(rbacOut, "kind: ClusterRole\n")
	if idx < 0 {
		t.Fatalf("release-deployer ClusterRole not rendered:\n%s", rbacOut)
	}
	role := rbacOut[idx:]
	if end := strings.Index(role, "\n---"); end >= 0 {
		role = role[:end]
	}
	// Take the namespace from the psa-labels grant itself rather than assuming
	// one: helm template renders .Release.Namespace from the environment (CI
	// runners render it as their own namespace, e.g. arc-runners).
	m := regexp.MustCompile(`resources: \["namespaces"\]\s*\n\s*resourceNames: \["([^"]+)"\]`).FindStringSubmatch(psa)
	if m == nil {
		t.Fatalf("could not find the namespaces resourceNames in the psa-labels ClusterRole:\n%s", psa)
	}
	ns := m[1]
	want := "  - apiGroups: [\"\"]\n    resources: [\"namespaces\"]\n    resourceNames: [\"" + ns + "\"]\n    verbs: [\"get\", \"patch\"]"
	if !strings.Contains(role, want) {
		t.Errorf("release-deployer ClusterRole must hold get/patch on namespace %q (the psa-labels hook grants it); got:\n%s", ns, role)
	}
}

// --- Requirement 6: the deployer holds every permission the chart grants ---
//
// Kubernetes RBAC escalation prevention lets a subject create or update a
// (Cluster)Role, or a binding to one, only if it already holds every
// permission that role grants, or holds `escalate` on that role (to write the
// role) and `bind` on it (to write a binding to it). The deployer re-applies
// every chart-owned ClusterRole on every upgrade and rollback, so it must hold
// a superset of all of them, except the few it holds a name-scoped `escalate`
// on (the Lands land-namespace roles, gd-9FH9SNjrGE): for those, every chart
// binding to the role needs a name-scoped `bind` too. Twice an autonomous roll failed and rolled back on
// exactly this, one role at a time: 2026.284.2 on the psa-labels hook role
// (namespaces), then 2026.284.4 on gardenio-ha-drill-operator (nodes) and
// <agents>-pv-gc (persistentvolumes), helm revs 74-101. This test checks the
// whole class at once instead of one role per incident.

// rbacPolicyRule mirrors rbac.authorization.k8s.io/v1 PolicyRule.
type rbacPolicyRule struct {
	APIGroups       []string `json:"apiGroups"`
	Resources       []string `json:"resources"`
	ResourceNames   []string `json:"resourceNames"`
	Verbs           []string `json:"verbs"`
	NonResourceURLs []string `json:"nonResourceURLs"`
}

// operatorOnlyClusterAdminBindings names the chart's ClusterRoleBindings that
// reference the built-in cluster-admin ClusterRole. The release-deployer does
// not and must not hold cluster-admin, so uncoveredChartGrants cannot check
// these and exempts them by exact name; every other cluster-admin binding is
// still reported. Only gardenio-infra-admin (templates/controller/
// infra-admin-rbac.yaml, gd-sAMar9tn5B) is listed.
var operatorOnlyClusterAdminBindings = map[string]bool{
	"gardenio-infra-admin": true,
}

// rbacObject is the subset of Role / ClusterRole / *Binding / ServiceAccount
// the superset check reads.
type rbacObject struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
	Rules           []rbacPolicyRule `json:"rules"`
	AggregationRule any              `json:"aggregationRule"`
	RoleRef         struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	} `json:"roleRef"`
}

// parseRBACObjects decodes every ServiceAccount, Role, ClusterRole,
// RoleBinding and ClusterRoleBinding in a rendered multi-doc stream. Other
// kinds are skipped without parsing.
func parseRBACObjects(t *testing.T, rendered string) []rbacObject {
	t.Helper()
	want := map[string]bool{"ServiceAccount": true, "Role": true, "ClusterRole": true, "RoleBinding": true, "ClusterRoleBinding": true}
	var objs []rbacObject
	for doc := range strings.SplitSeq(rendered, "\n---") {
		k := kindLineRE.FindStringSubmatch(doc)
		if k == nil || !want[k[1]] {
			continue
		}
		var o rbacObject
		if err := yaml.Unmarshal([]byte(doc), &o); err != nil {
			t.Fatalf("decode rendered %s: %v\n%s", k[1], err, doc)
		}
		objs = append(objs, o)
	}
	return objs
}

func containsOrStar(list []string, v string) bool {
	return slices.Contains(list, "*") || slices.Contains(list, v)
}

// resourceCovered follows k8s.io/component-helpers/auth/rbac/validation:
// "*" matches every resource, an exact "res" or "res/sub" matches itself, and
// "*/sub" matches subresource sub of any resource.
func resourceCovered(owner []string, resource string) bool {
	if containsOrStar(owner, resource) {
		return true
	}
	if _, sub, ok := strings.Cut(resource, "/"); ok {
		return slices.Contains(owner, "*/"+sub)
	}
	return false
}

// nonResourceURLCovered: "*" matches all, a trailing "*" is a prefix match.
func nonResourceURLCovered(owner []string, url string) bool {
	for _, o := range owner {
		if o == "*" || o == url || (strings.HasSuffix(o, "*") && strings.HasPrefix(url, strings.TrimSuffix(o, "*"))) {
			return true
		}
	}
	return false
}

// ruleCovers reports whether owner grants verb on (group, resource, name).
// name == "" means the needed rule has no resourceNames, i.e. it applies to
// every object, so only an owner rule without resourceNames covers it.
func ruleCovers(owner rbacPolicyRule, group, resource, name, verb string) bool {
	if !containsOrStar(owner.Verbs, verb) || !containsOrStar(owner.APIGroups, group) || !resourceCovered(owner.Resources, resource) {
		return false
	}
	if len(owner.ResourceNames) == 0 {
		return true
	}
	return name != "" && slices.Contains(owner.ResourceNames, name)
}

// uncoveredChartGrants returns one message per atomic permission that some
// chart-rendered ClusterRole (other than the deployer's own), or Role outside
// the release namespace, grants but the release-deployer does not hold where
// that role applies. A role the deployer holds `escalate` on (by name, as RBAC
// checks it) is exempt from that, but then each chart binding to it needs
// `bind` on it. It also flags any binding whose roleRef the chart does not
// render, because those rules cannot be checked from the render.
func uncoveredChartGrants(t *testing.T, rendered string) []string {
	t.Helper()
	objs := parseRBACObjects(t, rendered)

	releaseNs, deployer := "", ""
	for _, o := range objs {
		if o.Kind == "ServiceAccount" && strings.HasSuffix(o.Metadata.Name, "-release-deployer") {
			releaseNs, deployer = o.Metadata.Namespace, o.Metadata.Name
		}
	}
	if deployer == "" {
		t.Fatalf("no release-deployer ServiceAccount in the render; this check needs releaseDeployer.enabled=true")
	}

	// What the deployer holds: its ClusterRole everywhere, plus its Role in
	// each namespace. The release namespace's Role is */*, which is why Roles
	// there are not checked.
	var clusterHeld []rbacPolicyRule
	nsHeld := map[string][]rbacPolicyRule{}
	clusterRoles := map[string]bool{}
	roles := map[string]bool{}
	for _, o := range objs {
		switch {
		case o.Kind == "ClusterRole" && o.Metadata.Name == deployer:
			clusterHeld = o.Rules
		case o.Kind == "Role" && o.Metadata.Name == deployer:
			nsHeld[o.Metadata.Namespace] = o.Rules
		}
		switch o.Kind {
		case "ClusterRole":
			clusterRoles[o.Metadata.Name] = true
		case "Role":
			roles[o.Metadata.Namespace+"/"+o.Metadata.Name] = true
		}
	}
	if clusterHeld == nil {
		t.Fatalf("release-deployer ClusterRole %q not rendered", deployer)
	}

	// holds reports whether any of held grants verb on (group, resource, name).
	holds := func(held []rbacPolicyRule, group, resource, name, verb string) bool {
		for _, h := range held {
			if ruleCovers(h, group, resource, name, verb) {
				return true
			}
		}
		return false
	}
	const rbacGroup = "rbac.authorization.k8s.io"

	// Roles the deployer may write only through `escalate` (it does not hold
	// their rules), keyed "ClusterRole/<name>" or "Role/<ns>/<name>". A binding
	// to one of these needs `bind` on it. Filled by the roles pass, which runs
	// before the bindings pass.
	needsBind := map[string]bool{}

	var problems []string
	for _, pass := range []string{"roles", "bindings"} {
		for _, o := range objs {
			if isBinding := o.Kind == "ClusterRoleBinding" || o.Kind == "RoleBinding"; (pass == "bindings") != isBinding {
				continue
			}
			var held []rbacPolicyRule
			var where, roleResource, key string
			switch {
			case o.Kind == "ClusterRole" && o.Metadata.Name != deployer:
				held, where = clusterHeld, "ClusterRole "+o.Metadata.Name
				roleResource, key = "clusterroles", "ClusterRole/"+o.Metadata.Name
			case o.Kind == "Role" && o.Metadata.Namespace != releaseNs && o.Metadata.Name != deployer:
				held = append(slices.Clone(clusterHeld), nsHeld[o.Metadata.Namespace]...)
				where = "Role " + o.Metadata.Namespace + "/" + o.Metadata.Name
				roleResource, key = "roles", "Role/"+o.Metadata.Namespace+"/"+o.Metadata.Name
			case o.Kind == "ClusterRoleBinding" && operatorOnlyClusterAdminBindings[o.Metadata.Name] && o.RoleRef.Name == "cluster-admin":
				// Bound to the built-in cluster-admin on purpose: the deployer must
				// NOT hold that (see TestInfraAdmin_ReleaseDeployerCannotBindClusterAdmin),
				// so a change to this binding is an operator roll.
				continue
			case o.Kind == "ClusterRoleBinding" || (o.Kind == "RoleBinding" && o.Metadata.Namespace != releaseNs):
				ok := clusterRoles[o.RoleRef.Name]
				if o.RoleRef.Kind == "Role" {
					ok = roles[o.Metadata.Namespace+"/"+o.RoleRef.Name]
				}
				if !ok {
					problems = append(problems, fmt.Sprintf("%s %s binds %s %q, which the chart does not render, so this test cannot check that the deployer holds its permissions", o.Kind, o.Metadata.Name, o.RoleRef.Kind, o.RoleRef.Name))
					continue
				}
				refKey, refResource, bindHeld := "ClusterRole/"+o.RoleRef.Name, "clusterroles", clusterHeld
				if o.Kind == "RoleBinding" {
					bindHeld = append(slices.Clone(clusterHeld), nsHeld[o.Metadata.Namespace]...)
					if o.RoleRef.Kind == "Role" {
						refKey, refResource = "Role/"+o.Metadata.Namespace+"/"+o.RoleRef.Name, "roles"
					}
				}
				if needsBind[refKey] && !holds(bindHeld, rbacGroup, refResource, o.RoleRef.Name, "bind") {
					problems = append(problems, fmt.Sprintf("%s %s binds %s %q, whose rules release-deployer holds only through escalate, but it holds no bind on that role", o.Kind, o.Metadata.Name, o.RoleRef.Kind, o.RoleRef.Name))
				}
				continue
			default:
				continue
			}
			var roleProblems []string
			if o.AggregationRule != nil {
				roleProblems = append(roleProblems, fmt.Sprintf("%s uses aggregationRule; its effective rules are not in the render, so this test cannot check them", where))
			}
			for _, r := range o.Rules {
				for _, verb := range r.Verbs {
					for _, url := range r.NonResourceURLs {
						covered := false
						for _, h := range clusterHeld {
							covered = covered || (containsOrStar(h.Verbs, verb) && nonResourceURLCovered(h.NonResourceURLs, url))
						}
						if !covered {
							roleProblems = append(roleProblems, fmt.Sprintf("%s grants nonResourceURL %q verb %q, which release-deployer does not hold", where, url, verb))
						}
					}
					names := r.ResourceNames
					if len(names) == 0 {
						names = []string{""}
					}
					for _, group := range r.APIGroups {
						for _, resource := range r.Resources {
							for _, name := range names {
								covered := false
								for _, h := range held {
									covered = covered || ruleCovers(h, group, resource, name, verb)
								}
								if !covered {
									shown := "(any name)"
									if name != "" {
										shown = strconv.Quote(name)
									}
									roleProblems = append(roleProblems, fmt.Sprintf("%s grants {apiGroup %q, resource %q, resourceName %s, verb %q}, which release-deployer does not hold", where, group, resource, shown, verb))
								}
							}
						}
					}
				}
			}
			// escalate on this role, by its name, lets the deployer write it
			// whatever it grants. RBAC checks escalate against the request's name,
			// which a create does not carry, so this covers updates only: the first
			// install of such a role is an operator roll.
			if len(roleProblems) > 0 && holds(held, rbacGroup, roleResource, o.Metadata.Name, "escalate") {
				needsBind[key] = true
				continue
			}
			problems = append(problems, roleProblems...)
		}
	}
	return problems
}

// TestReleaseDeployer_HoldsEveryChartRoleGrant renders the chart the way it is
// deployed (deploy-values/gardenio-k3s.yaml) and with defaults, in the ambient
// namespace (HELM_NAMESPACE; CI renders in arc-runners), and fails naming each
// chart role the release-deployer could not re-apply.
func TestReleaseDeployer_HoldsEveryChartRoleGrant(t *testing.T) {
	t.Parallel()
	// The flag-on cases cover the per-agent GitHub App token grants (secrets
	// update/list, key ExternalSecret, gVisor mirror), which only render with
	// agents.githubApp.enabled (gd-BHLDnjX1Hw); both default renders are flag-off.
	const githubAppOn = "agents.enabled=true,agents.githubApp.enabled=true,agents.githubApp.appId=5029022," +
		"agents.githubApp.externalSecret.enabled=true,agents.githubApp.externalSecret.secretStoreName=aws-ssm"
	for _, tc := range []struct {
		name   string
		values []string
		set    []string
	}{
		{"deploy-values/gardenio-k3s.yaml", releaseDeployerRepoRootValues(), nil},
		{"defaults", nil, nil},
		{"deploy-values/gardenio-k3s.yaml+githubApp", releaseDeployerRepoRootValues(), []string{githubAppOn}},
		{"defaults+githubApp", nil, []string{githubAppOn}},
		{"deploy-values/gardenio-k3s.yaml+rbacProbe", releaseDeployerRepoRootValues(), []string{"beads.rbacProbe.enabled=true"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			full := helmTemplateFull(t, tc.values, append([]string{"releaseDeployer.enabled=true"}, tc.set...)...)
			for _, p := range uncoveredChartGrants(t, full) {
				t.Error(p)
			}
		})
	}
}

// TestReleaseDeployer_HoldsEveryChartRoleGrant_CatchesPlantedRoles is the
// control for the check above: roles planted into a real render must be
// reported, and a planted role the deployer does hold must not be. The render
// has land namespaces on, so it also checks that escalate covers only the
// named ClusterRoles it is scoped to: a Role of the same name, or a binding to
// an escalated role the deployer holds no bind on, is still reported.
func TestReleaseDeployer_HoldsEveryChartRoleGrant_CatchesPlantedRoles(t *testing.T) {
	t.Parallel()
	full := helmTemplateFull(t, releaseDeployerRepoRootValues(), "releaseDeployer.enabled=true")
	if !strings.Contains(full, "\n  name: gardenio-land-agent-manager\n") {
		t.Fatal("control: expected deploy-values/gardenio-k3s.yaml to render the gardenio-land-agent-manager ClusterRole (landNamespaces.enabled)")
	}
	planted := full + `
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: planted-secrets-reader
rules:
  - apiGroups: [""]
    resources: ["secrets"]
    verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: planted-foreign-clusterrole-editor
rules:
  - apiGroups: ["rbac.authorization.k8s.io"]
    resources: ["clusterroles"]
    resourceNames: ["someone-elses-role"]
    verbs: ["patch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: planted-foreign-role
  namespace: planted-ns
rules:
  - apiGroups: [""]
    resources: ["configmaps"]
    verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: planted-binding
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: cluster-admin
subjects: []
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: planted-covered-node-patch
rules:
  - apiGroups: [""]
    resources: ["nodes"]
    resourceNames: ["some-node"]
    verbs: ["patch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: gardenio-land-agent-manager
  namespace: planted-ns
rules:
  - apiGroups: [""]
    resources: ["secrets"]
    verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: planted-agent-manager-binding
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: gardenio-land-agent-manager
subjects: []
`
	got := strings.Join(uncoveredChartGrants(t, planted), "\n")
	for _, want := range []string{
		`ClusterRole planted-secrets-reader grants {apiGroup "", resource "secrets", resourceName (any name), verb "get"}`,
		`ClusterRole planted-foreign-clusterrole-editor grants {apiGroup "rbac.authorization.k8s.io", resource "clusterroles", resourceName "someone-elses-role", verb "patch"}`,
		`Role planted-ns/planted-foreign-role grants {apiGroup "", resource "configmaps", resourceName (any name), verb "get"}`,
		`ClusterRoleBinding planted-binding binds ClusterRole "cluster-admin"`,
		`Role planted-ns/gardenio-land-agent-manager grants {apiGroup "", resource "secrets", resourceName (any name), verb "get"}`,
		`ClusterRoleBinding planted-agent-manager-binding binds ClusterRole "gardenio-land-agent-manager", whose rules release-deployer holds only through escalate, but it holds no bind on that role`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("planted role not reported: want a line containing\n  %s\ngot:\n%s", want, got)
		}
	}
	if strings.Contains(got, "planted-covered-node-patch") {
		t.Errorf("a named nodes/patch grant is covered by the deployer's unrestricted nodes/patch rule, but was reported:\n%s", got)
	}
	if n := strings.Count(got, "\n") + 1; n != 6 {
		t.Errorf("expected exactly the 6 planted problems, got %d:\n%s", n, got)
	}
}

// TestReleaseDeployer_LandNamespaceRoles pins how the deployer re-applies the
// Lands land-namespace ClusterRoles (templates/controller/
// lands-namespace-rbac.yaml) without holding what they grant: escalate on
// exactly gardenio-land-agent-manager and <agents>-lands-namespaces, bind on
// exactly <agents>-lands-namespaces, and still nothing on pods or secrets.
// With land namespaces off it holds no Lands escalate or bind. The
// infra-reader's escalate/bind name is pinned by
// TestReleaseDeployer_InfraReaderRoles and left out of the names here.
func TestReleaseDeployer_LandNamespaceRoles(t *testing.T) {
	t.Parallel()
	deployerRules := func(t *testing.T, rendered string) []rbacPolicyRule {
		t.Helper()
		for _, o := range parseRBACObjects(t, rendered) {
			if o.Kind == "ClusterRole" && strings.HasSuffix(o.Metadata.Name, "-release-deployer") {
				return o.Rules
			}
		}
		t.Fatal("release-deployer ClusterRole not rendered")
		return nil
	}
	namesFor := func(t *testing.T, rules []rbacPolicyRule, verb string) []string {
		t.Helper()
		var names []string
		for _, r := range rules {
			if !slices.Contains(r.Verbs, verb) {
				continue
			}
			if len(r.ResourceNames) == 0 || !slices.Equal(r.APIGroups, []string{"rbac.authorization.k8s.io"}) || !slices.Equal(r.Resources, []string{"clusterroles"}) {
				t.Errorf("verb %q must be granted on named clusterroles only; rule: %+v", verb, r)
			}
			for _, n := range r.ResourceNames {
				if n != infraReaderName {
					names = append(names, n)
				}
			}
		}
		sort.Strings(names)
		return names
	}

	t.Run("on", func(t *testing.T) {
		full := helmTemplateFull(t, releaseDeployerRepoRootValues(), "releaseDeployer.enabled=true")
		landsRole := ""
		for _, o := range parseRBACObjects(t, full) {
			if o.Kind == "ClusterRoleBinding" && strings.HasSuffix(o.Metadata.Name, "-agents-lands-namespaces") {
				landsRole = o.RoleRef.Name
			}
		}
		if landsRole == "" {
			t.Fatal("control: expected deploy-values/gardenio-k3s.yaml to render the <agents>-lands-namespaces ClusterRoleBinding (landNamespaces.enabled)")
		}
		rules := deployerRules(t, full)
		wantEscalate := []string{"gardenio-land-agent-manager", landsRole}
		sort.Strings(wantEscalate)
		if got := namesFor(t, rules, "escalate"); !slices.Equal(got, wantEscalate) {
			t.Errorf("escalate names = %v, want %v", got, wantEscalate)
		}
		if got, want := namesFor(t, rules, "bind"), []string{landsRole}; !slices.Equal(got, want) {
			t.Errorf("bind names = %v, want %v", got, want)
		}
		for _, r := range rules {
			for _, res := range r.Resources {
				if res == "pods" || res == "secrets" || strings.HasPrefix(res, "pods/") {
					t.Errorf("release-deployer ClusterRole must not grant %s cluster-wide; rule: %+v", res, r)
				}
			}
		}
	})

	t.Run("off", func(t *testing.T) {
		full := helmTemplateFull(t, releaseDeployerRepoRootValues(), "releaseDeployer.enabled=true", "landNamespaces.enabled=false")
		rules := deployerRules(t, full)
		if got := append(namesFor(t, rules, "escalate"), namesFor(t, rules, "bind")...); len(got) != 0 {
			t.Errorf("with land namespaces off the deployer must hold no Lands escalate or bind; got names %v", got)
		}
		for _, o := range parseRBACObjects(t, full) {
			if o.Metadata.Name == "gardenio-land-agent-manager" || strings.HasSuffix(o.Metadata.Name, "-lands-namespaces") {
				t.Errorf("with land namespaces off, %s %s should not render", o.Kind, o.Metadata.Name)
			}
		}
	})
}

// infraReaderName is agents.infraReader.name's default, which
// deploy-values/gardenio-k3s.yaml keeps.
const infraReaderName = "gardenio-infra-reader"

// TestReleaseDeployer_InfraReaderRoles pins how the deployer re-applies the
// infra-reader roles (templates/controller/infra-reader-rbac.yaml) without
// holding what they grant: escalate + bind on exactly the gardenio-infra-reader
// ClusterRole, and on exactly the gardenio-infra-reader Role in the
// keycloakclients namespace (operators) and the ARC runner namespace
// (arc-runners, gd-6FEbhRnAkd), and none of their reads. With
// agents.infraReader off no deployer object names it at all.
func TestReleaseDeployer_InfraReaderRoles(t *testing.T) {
	t.Parallel()
	const kcNs, arcNs = "operators", "arc-runners"
	escalateBind := rbacPolicyRule{Verbs: []string{"escalate", "bind"}, APIGroups: []string{"rbac.authorization.k8s.io"}, ResourceNames: []string{infraReaderName}}

	t.Run("on", func(t *testing.T) {
		// Pinned: on CI the ambient namespace is arc-runners, where the
		// deployer's */* Role applies and no foreign grant renders.
		full := helmTemplateFullNamespaced(t, "gardenio", releaseDeployerRepoRootValues(), "releaseDeployer.enabled=true")
		var clusterRules, kcRules, arcRules []rbacPolicyRule
		var kcRole, arcRole bool
		for _, o := range parseRBACObjects(t, full) {
			switch {
			case o.Kind == "ClusterRole" && strings.HasSuffix(o.Metadata.Name, "-release-deployer"):
				clusterRules = o.Rules
			case o.Kind == "Role" && o.Metadata.Namespace == kcNs && strings.HasSuffix(o.Metadata.Name, "-release-deployer"):
				kcRules = o.Rules
			case o.Kind == "Role" && o.Metadata.Namespace == kcNs && o.Metadata.Name == infraReaderName:
				kcRole = true
			case o.Kind == "Role" && o.Metadata.Namespace == arcNs && strings.HasSuffix(o.Metadata.Name, "-release-deployer"):
				arcRules = o.Rules
			case o.Kind == "Role" && o.Metadata.Namespace == arcNs && o.Metadata.Name == infraReaderName:
				arcRole = true
			}
		}
		if clusterRules == nil || kcRules == nil || !kcRole {
			t.Fatalf("control: expected deploy-values/gardenio-k3s.yaml to render the deployer ClusterRole (%v), its %s Role (%v) and the infra-reader Role there (%v)", clusterRules != nil, kcNs, kcRules != nil, kcRole)
		}
		if arcRules == nil || !arcRole {
			t.Fatalf("control: expected deploy-values/gardenio-k3s.yaml to render the deployer's %s Role (%v) and the infra-reader Role there (%v)", arcNs, arcRules != nil, arcRole)
		}
		for _, c := range []struct {
			where    string
			rules    []rbacPolicyRule
			resource string
		}{
			{"ClusterRole", clusterRules, "clusterroles"},
			{"Role " + kcNs, kcRules, "roles"},
			{"Role " + arcNs, arcRules, "roles"},
		} {
			want := escalateBind
			want.Resources = []string{c.resource}
			var got []rbacPolicyRule
			for _, r := range c.rules {
				if slices.Contains(r.Verbs, "escalate") || slices.Contains(r.Verbs, "bind") {
					if slices.Contains(r.ResourceNames, infraReaderName) {
						got = append(got, r)
					}
				}
				// The infra-reader's reads are unnamed; a named grant (the keycloak
				// CA ConfigMap in operators) is the deployer's own.
				for _, res := range r.Resources {
					if len(r.ResourceNames) == 0 && (res == "pods" || res == "configmaps" || res == "keycloakclients" || strings.HasSuffix(res, "runnersets") || res == "ephemeralrunners" || strings.HasPrefix(res, "pods/")) {
						t.Errorf("release-deployer %s must not hold the infra-reader's %s read; rule: %+v", c.where, res, r)
					}
				}
			}
			if len(got) != 1 || !reflect.DeepEqual(got[0], want) {
				t.Errorf("release-deployer %s: want exactly one escalate/bind rule %+v naming %s, got %+v", c.where, want, infraReaderName, got)
			}
		}
	})

	t.Run("off", func(t *testing.T) {
		full := helmTemplateFull(t, releaseDeployerRepoRootValues(), "releaseDeployer.enabled=true", "agents.infraReader.enabled=false", "agents.verifier.enabled=false")
		for doc := range strings.SplitSeq(full, "\n---") {
			if strings.Contains(doc, "app.kubernetes.io/component: release-deployer") && strings.Contains(doc, infraReaderName) {
				t.Errorf("with agents.infraReader off no release-deployer object may name %s; got:\n%s", infraReaderName, doc)
			}
		}
	})
}

// TestReleaseDeployer_GitSecretFollowsGithubApp pins gd-q48ldwLspl: with the
// fleet App on, the deployer mounts the controller-minted read-only token
// Secret and the controller is told to mint it; with the App off it keeps the
// shared gardenio-git-credentials; an explicit gitSecret wins and the
// controller then mints nothing.
func TestReleaseDeployer_GitSecretFollowsGithubApp(t *testing.T) {
	t.Parallel()
	app := []string{"releaseDeployer.enabled=true", "agents.enabled=true", "agents.githubApp.enabled=true", "agents.githubApp.appId=5029022"}
	cases := []struct {
		name       string
		set        []string
		wantSecret string
		wantMint   bool
	}{
		{"app off", []string{"releaseDeployer.enabled=true", "agents.enabled=true"}, "gardenio-git-credentials", false},
		{"app on", app, "release-deployer-github-token", true},
		{"explicit secret", append(slices.Clone(app), "releaseDeployer.gitSecret=my-creds"), "my-creds", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cron := helmTemplate(t, "templates/release-deployer/cronjob.yaml", tc.set...)
			if !strings.Contains(cron, "name: "+strconv.Quote(tc.wantSecret)) {
				t.Errorf("git-creds volume must name %q; got:\n%s", tc.wantSecret, cron)
			}
			ctrl := helmTemplate(t, "templates/controller/deployment.yaml", tc.set...)
			gotMint := strings.Contains(ctrl, "- name: RELEASE_DEPLOYER_GITHUB_TOKEN_SECRET")
			if gotMint != tc.wantMint {
				t.Errorf("controller RELEASE_DEPLOYER_GITHUB_TOKEN_SECRET rendered = %v, want %v", gotMint, tc.wantMint)
			}
			if tc.wantMint && !strings.Contains(ctrl, `value: "https://github.com/mattbakerconsulting/gardenio"`) {
				t.Errorf("controller must be told the deployer's repo; got:\n%s", ctrl)
			}
		})
	}
}

// releaseDeployer.edgeNamespace (gd-t6Re9igBAj): release-deploy.sh rule 4c
// applies gardenio-ziti/public-edge.yaml and reloads nginx, so the deployer
// needs a named Role in the edge namespace plus get/patch on that namespace.
// Off by default; on, it holds exactly the edge objects, pods and pods/exec.
func TestReleaseDeployer_EdgeNamespaceRole(t *testing.T) {
	t.Parallel()
	off := helmTemplate(t, "templates/release-deployer/rbac.yaml", "releaseDeployer.enabled=true")
	if strings.Contains(off, "public-edge") {
		t.Errorf("edgeNamespace unset must render no public-edge grant; got:\n%s", off)
	}

	on := helmTemplate(t, "templates/release-deployer/rbac.yaml",
		"releaseDeployer.enabled=true", "releaseDeployer.edgeNamespace=public-edge")
	var role, binding, clusterRole string
	for doc := range strings.SplitSeq(on, "\n---") {
		k := kindLineRE.FindStringSubmatch(doc)
		if k == nil {
			continue
		}
		ns := namespaceValueRE.FindStringSubmatch(doc)
		switch {
		case k[1] == "ClusterRole":
			clusterRole = doc
		case ns != nil && ns[1] == "public-edge" && k[1] == "Role":
			if role != "" {
				t.Fatalf("two release-deployer Roles in public-edge (duplicate name):\n%s\n---\n%s", role, doc)
			}
			role = doc
		case ns != nil && ns[1] == "public-edge" && k[1] == "RoleBinding":
			binding = doc
		}
	}
	if role == "" || binding == "" {
		t.Fatalf("expected a release-deployer Role and RoleBinding in public-edge; got:\n%s", on)
	}
	want := []struct{ resource, name string }{
		{"configmaps", "public-edge"},
		{"configmaps", "public-edge-become"},
		{"services", "public-edge"},
		{"deployments", "public-edge"},
		{"certificates", "public-edge"},
	}
	blocks := clusterRoleRuleBlocks(role)
	for _, w := range want {
		ok := false
		for _, b := range blocks {
			if strings.Contains(b, `"`+w.resource+`"`) && strings.Contains(b, `"`+w.name+`"`) && strings.Contains(b, `"get"`) && strings.Contains(b, `"patch"`) {
				ok = true
			}
		}
		if !ok {
			t.Errorf("public-edge Role does not grant get+patch on %s %q; got:\n%s", w.resource, w.name, role)
		}
	}
	for _, need := range []string{`resources: ["pods"]`, `resources: ["pods/exec"]`} {
		if !strings.Contains(role, need) {
			t.Errorf("public-edge Role is missing %s; got:\n%s", need, role)
		}
	}
	for _, b := range blocks {
		if strings.Contains(b, `"*"`) || strings.Contains(b, `"secrets"`) {
			t.Errorf("public-edge Role must not grant secrets or wildcards; got:\n%s", b)
		}
	}
	nsRule := false
	for _, b := range clusterRoleRuleBlocks(clusterRole) {
		if strings.Contains(b, `"namespaces"`) && strings.Contains(b, `"public-edge"`) && strings.Contains(b, `"patch"`) {
			nsRule = true
		}
	}
	if !nsRule {
		t.Errorf("ClusterRole must grant get/patch on namespace public-edge; got:\n%s", clusterRole)
	}
}
