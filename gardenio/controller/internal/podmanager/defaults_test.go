package podmanager

import (
	"slices"
	"strings"
	"testing"

	"gardenio/controller/internal/lands/landns"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// --- ApplyDefaults tests ---

func TestApplyDefaults_NilDefaults(t *testing.T) {
	spec := &AgentPodSpec{Image: "img:v1"}
	ApplyDefaults(spec, nil)
	if spec.Image != "img:v1" {
		t.Errorf("image should remain img:v1, got %s", spec.Image)
	}
}

func TestApplyDefaults_FillsEmptyFields(t *testing.T) {
	spec := &AgentPodSpec{}
	defaults := &PodDefaults{
		Image:              "default-img:v1",
		ServiceAccountName: "default-sa",
		ConfigMapName:      "default-cfg",
		NodeSelector:       map[string]string{"arch": "amd64"},
		Tolerations:        []corev1.Toleration{{Key: "gpu"}},
		WorkspaceStorage:   &WorkspaceStorageSpec{Size: "10Gi"},
	}

	ApplyDefaults(spec, defaults)

	if spec.Image != "default-img:v1" {
		t.Errorf("Image = %s, want default-img:v1", spec.Image)
	}
	if spec.ServiceAccountName != "default-sa" {
		t.Errorf("ServiceAccountName = %s, want default-sa", spec.ServiceAccountName)
	}
	if spec.ConfigMapName != "default-cfg" {
		t.Errorf("ConfigMapName = %s, want default-cfg", spec.ConfigMapName)
	}
	if spec.NodeSelector["arch"] != "amd64" {
		t.Error("expected NodeSelector from defaults")
	}
	if len(spec.Tolerations) != 1 {
		t.Errorf("expected 1 toleration, got %d", len(spec.Tolerations))
	}
	if spec.WorkspaceStorage == nil || spec.WorkspaceStorage.Size != "10Gi" {
		t.Error("expected WorkspaceStorage from defaults")
	}
}

func TestApplyDefaults_DoesNotOverwriteExisting(t *testing.T) {
	spec := &AgentPodSpec{
		Image:              "spec-img:v2",
		ServiceAccountName: "spec-sa",
		ConfigMapName:      "spec-cfg",
		NodeSelector:       map[string]string{"zone": "us-east"},
		Tolerations:        []corev1.Toleration{{Key: "spot"}},
		WorkspaceStorage:   &WorkspaceStorageSpec{Size: "50Gi"},
	}
	defaults := &PodDefaults{
		Image:              "default-img:v1",
		ServiceAccountName: "default-sa",
		ConfigMapName:      "default-cfg",
		NodeSelector:       map[string]string{"arch": "amd64"},
		Tolerations:        []corev1.Toleration{{Key: "gpu"}},
		WorkspaceStorage:   &WorkspaceStorageSpec{Size: "10Gi"},
	}

	ApplyDefaults(spec, defaults)

	if spec.Image != "spec-img:v2" {
		t.Errorf("Image should not be overwritten, got %s", spec.Image)
	}
	if spec.ServiceAccountName != "spec-sa" {
		t.Errorf("ServiceAccountName should not be overwritten, got %s", spec.ServiceAccountName)
	}
	if spec.NodeSelector["zone"] != "us-east" {
		t.Error("NodeSelector should not be overwritten")
	}
	if spec.Tolerations[0].Key != "spot" {
		t.Error("Tolerations should not be overwritten")
	}
	if spec.WorkspaceStorage.Size != "50Gi" {
		t.Error("WorkspaceStorage should not be overwritten")
	}
}

func TestApplyDefaults_Resources(t *testing.T) {
	spec := &AgentPodSpec{}
	defaults := &PodDefaults{
		Resources: &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("1"),
			},
		},
	}

	ApplyDefaults(spec, defaults)
	if spec.Resources == nil {
		t.Fatal("expected Resources from defaults")
	}
	cpu := spec.Resources.Requests[corev1.ResourceCPU]
	if cpu.String() != "1" {
		t.Errorf("CPU request = %s, want 1", cpu.String())
	}
}

func TestApplyDefaults_ResourcesNotOverwritten(t *testing.T) {
	specRes := &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("4"),
		},
	}
	spec := &AgentPodSpec{Resources: specRes}
	defaults := &PodDefaults{
		Resources: &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("1"),
			},
		},
	}

	ApplyDefaults(spec, defaults)
	cpu := spec.Resources.Requests[corev1.ResourceCPU]
	if cpu.String() != "4" {
		t.Errorf("CPU request should not be overwritten, got %s", cpu.String())
	}
}

func TestApplyDefaults_Affinity(t *testing.T) {
	spec := &AgentPodSpec{}
	defaults := &PodDefaults{
		Affinity: &corev1.Affinity{
			NodeAffinity: &corev1.NodeAffinity{},
		},
	}

	ApplyDefaults(spec, defaults)
	if spec.Affinity == nil {
		t.Error("expected Affinity from defaults")
	}
}

func TestApplyDefaults_EnvMerge(t *testing.T) {
	spec := &AgentPodSpec{
		Env: map[string]string{"EXISTING": "keep"},
	}
	defaults := &PodDefaults{
		Env: map[string]string{
			"EXISTING": "ignored",
			"NEW_VAR":  "added",
		},
	}

	ApplyDefaults(spec, defaults)

	if spec.Env["EXISTING"] != "keep" {
		t.Errorf("existing env should not be overwritten, got %s", spec.Env["EXISTING"])
	}
	if spec.Env["NEW_VAR"] != "added" {
		t.Errorf("new env should be added, got %s", spec.Env["NEW_VAR"])
	}
}

func TestApplyDefaults_EnvMerge_NilSpecEnv(t *testing.T) {
	spec := &AgentPodSpec{} // Env is nil
	defaults := &PodDefaults{
		Env: map[string]string{"VAR": "val"},
	}

	ApplyDefaults(spec, defaults)
	if spec.Env["VAR"] != "val" {
		t.Errorf("expected VAR=val, got %s", spec.Env["VAR"])
	}
}

func TestApplyDefaults_SecretEnvDedup(t *testing.T) {
	spec := &AgentPodSpec{
		SecretEnv: []SecretEnvSource{
			{EnvName: "TOKEN", SecretName: "s1", SecretKey: "k1"},
		},
	}
	defaults := &PodDefaults{
		SecretEnv: []SecretEnvSource{
			{EnvName: "TOKEN", SecretName: "s2", SecretKey: "k2"},   // duplicate, should skip
			{EnvName: "API_KEY", SecretName: "s3", SecretKey: "k3"}, // new, should add
		},
	}

	ApplyDefaults(spec, defaults)

	if len(spec.SecretEnv) != 2 {
		t.Fatalf("expected 2 secret env entries, got %d", len(spec.SecretEnv))
	}

	// TOKEN should still point to s1 (not overwritten).
	if spec.SecretEnv[0].SecretName != "s1" {
		t.Errorf("TOKEN should keep original secret s1, got %s", spec.SecretEnv[0].SecretName)
	}
	// API_KEY should be added from defaults.
	if spec.SecretEnv[1].EnvName != "API_KEY" {
		t.Errorf("expected API_KEY appended, got %s", spec.SecretEnv[1].EnvName)
	}
}

// --- DefaultPodDefaults tests ---

func TestDefaultPodDefaults_Crew(t *testing.T) {
	d := DefaultPodDefaults("crew", "crew")

	if d.Resources == nil {
		t.Fatal("expected Resources")
	}
	if d.NodeSelector["kubernetes.io/arch"] != "amd64" {
		t.Error("expected amd64 NodeSelector")
	}
	if d.Affinity == nil {
		t.Error("expected Affinity")
	}
	// Crew pods use EmptyDir — S3 handles session persistence.
	if d.WorkspaceStorage != nil {
		t.Error("expected no WorkspaceStorage for crew mode (uses EmptyDir + S3)")
	}
}

func TestDefaultPodDefaults_AgentPoolToleration(t *testing.T) {
	for _, mode := range []string{"crew", "job"} {
		d := DefaultPodDefaults(mode, mode)

		var found bool
		for _, tol := range d.Tolerations {
			if tol.Key == "gardenio.agent" &&
				tol.Operator == corev1.TolerationOpExists &&
				tol.Effect == corev1.TaintEffectNoSchedule {
				found = true
			}
		}
		if !found {
			t.Errorf("mode %q: expected gardenio.agent=Exists:NoSchedule toleration in defaults", mode)
		}
	}
}

func TestDefaultPodDefaults_Job(t *testing.T) {
	d := DefaultPodDefaults("job", "job")

	if d.Resources == nil {
		t.Fatal("expected Resources")
	}
	if d.WorkspaceStorage != nil {
		t.Error("expected no WorkspaceStorage for job mode")
	}
}

func TestDefaultPodDefaults_Affinity_ExcludesPerMRNodes(t *testing.T) {
	d := DefaultPodDefaults("crew", "crew")

	if d.Affinity.NodeAffinity == nil {
		t.Fatal("expected NodeAffinity")
	}

	required := d.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	if required == nil {
		t.Fatal("expected required node selector")
	}

	// Should exclude per-mr nodes.
	var foundPerMR bool
	for _, term := range required.NodeSelectorTerms {
		for _, expr := range term.MatchExpressions {
			if expr.Key == "per-mr" && expr.Operator == corev1.NodeSelectorOpDoesNotExist {
				foundPerMR = true
			}
		}
	}
	if !foundPerMR {
		t.Error("expected per-mr DoesNotExist in required scheduling")
	}
}

// requiredCapacityType returns the capacity-type values HARD-required by the
// affinity (empty if capacity-type is not a required constraint).
func requiredCapacityType(d *PodDefaults) []string {
	var out []string
	req := d.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	if req == nil {
		return out
	}
	for _, term := range req.NodeSelectorTerms {
		for _, expr := range term.MatchExpressions {
			if expr.Key == "karpenter.sh/capacity-type" {
				out = append(out, expr.Values...)
			}
		}
	}
	return out
}

// preferredCapacityType returns the capacity-type values the affinity PREFERS
// (empty if capacity-type is not a preferred term).
func preferredCapacityType(d *PodDefaults) []string {
	var out []string
	for _, term := range d.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution {
		for _, expr := range term.Preference.MatchExpressions {
			if expr.Key == "karpenter.sh/capacity-type" {
				out = append(out, expr.Values...)
			}
		}
	}
	return out
}

func contains(vals []string, want string) bool {
	for _, v := range vals {
		if v == want {
			return true
		}
	}
	return false
}

// Role-split (ka-0000002487): interactive roles (thread/dm) must HARD-require
// on-demand nodes so a Spot reclaim can never kill a human-waiting agent
// mid-task; batch roles (crew/job/gardener) must NOT require on-demand and
// should prefer cheap Spot.
func TestDefaultPodDefaults_Affinity_InteractiveRequiresOnDemand(t *testing.T) {
	for _, role := range []string{"thread", "dm", "crew,thread", "thread,crew", "gardener,thread"} {
		d := DefaultPodDefaults("crew", role)

		req := requiredCapacityType(d)
		if !contains(req, "on-demand") {
			t.Errorf("role %q: expected on-demand as a REQUIRED capacity-type, got required=%v", role, req)
		}
		// Interactive pods must not carry a Spot preference (they are pinned
		// on-demand); a stray Spot preference would be dead weight and
		// misleading.
		if pref := preferredCapacityType(d); contains(pref, "spot") {
			t.Errorf("role %q: interactive pods should not prefer spot, got preferred=%v", role, pref)
		}
		// per-mr exclusion must survive alongside the on-demand requirement.
		if !requiredExcludesPerMR(d) {
			t.Errorf("role %q: expected per-mr DoesNotExist to remain in required scheduling", role)
		}
	}
}

func TestDefaultPodDefaults_Affinity_BatchPrefersSpot(t *testing.T) {
	// Gardeners take the batch affinity (gd-Fyz3w0tOnf): they resume from S3
	// after a Spot reclaim, so pinning them on-demand only starved them of CPU.
	for _, role := range []string{"crew", "job", "polecat", "", "gardener", "plan-gardener", "crew,plan-gardener"} {
		d := DefaultPodDefaults("crew", role)

		// Batch pods must NOT hard-require on-demand — that is what lets them
		// land on cheap Spot capacity.
		if req := requiredCapacityType(d); contains(req, "on-demand") {
			t.Errorf("role %q: batch pods must not REQUIRE on-demand, got required=%v", role, req)
		}
		// Batch pods should prefer Spot.
		if pref := preferredCapacityType(d); !contains(pref, "spot") {
			t.Errorf("role %q: expected spot preference for batch, got preferred=%v", role, pref)
		}
		if !requiredExcludesPerMR(d) {
			t.Errorf("role %q: expected per-mr DoesNotExist in required scheduling", role)
		}
		if !requiredExcludesOnDemandLifecycle(d) {
			t.Errorf("role %q: batch pods must hard-exclude %s=on-demand nodes", role, lifecycleLabel)
		}
	}
}

// Batch pods stay off the k3s on-demand platform pair (gd-sDztRMaAag), but
// thread/dm pods must still be able to land there: it is the only on-demand
// capacity that cluster has.
func TestDefaultPodDefaults_Affinity_OnDemandLifecycleExclusion(t *testing.T) {
	for _, role := range []string{"thread", "dm", "crew,thread"} {
		if requiredExcludesOnDemandLifecycle(DefaultPodDefaults("crew", role)) {
			t.Errorf("role %q: interactive pods must not exclude %s=on-demand nodes", role, lifecycleLabel)
		}
	}

	d := DefaultPodDefaults("crew", "crew")
	node := func(labels map[string]string) bool { return matchesRequired(d, labels) }
	for _, tc := range []struct {
		name   string
		labels map[string]string
		want   bool
	}{
		{"k3s on-demand pair", map[string]string{lifecycleLabel: "on-demand", "karpenter.sh/capacity-type": "on-demand"}, false},
		{"k3s spot node", map[string]string{lifecycleLabel: "spot", "karpenter.sh/capacity-type": "spot"}, true},
		// Karpenter / gVisor / kind nodes carry no gardenio.io/lifecycle.
		{"karpenter on-demand node", map[string]string{"karpenter.sh/capacity-type": "on-demand"}, true},
		{"unlabeled node", map[string]string{}, true},
		{"per-mr node", map[string]string{"per-mr": "true"}, false},
	} {
		if got := node(tc.labels); got != tc.want {
			t.Errorf("%s: crew schedulable = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// requiredExcludesOnDemandLifecycle reports whether every required term
// carries lifecycleLabel NotIn [on-demand].
func requiredExcludesOnDemandLifecycle(d *PodDefaults) bool {
	req := d.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	if req == nil || len(req.NodeSelectorTerms) == 0 {
		return false
	}
	for _, term := range req.NodeSelectorTerms {
		found := false
		for _, expr := range term.MatchExpressions {
			if expr.Key == lifecycleLabel && expr.Operator == corev1.NodeSelectorOpNotIn && contains(expr.Values, "on-demand") {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// Thread/dm pods on k3s (gd-4nac3mPMgo): the on-demand pair holds one thread
// agent, so a k3s Spot node must also satisfy the required affinity (with
// on-demand only preferred); everywhere else the on-demand pin is unchanged.
func TestDefaultPodDefaults_Affinity_InteractiveK3sSpot(t *testing.T) {
	for _, role := range []string{"thread", "dm", "crew,thread"} {
		d := DefaultPodDefaults("crew", role)
		for _, tc := range []struct {
			name   string
			labels map[string]string
			want   bool
		}{
			{"k3s on-demand pair", map[string]string{lifecycleLabel: "on-demand", "karpenter.sh/capacity-type": "on-demand"}, true},
			{"k3s spot node", map[string]string{lifecycleLabel: "spot", "karpenter.sh/capacity-type": "spot"}, true},
			{"karpenter on-demand node", map[string]string{"karpenter.sh/capacity-type": "on-demand"}, true},
			{"karpenter spot node", map[string]string{"karpenter.sh/capacity-type": "spot"}, false},
			{"unlabeled node", map[string]string{}, false},
			{"k3s spot per-mr node", map[string]string{lifecycleLabel: "spot", "per-mr": "true"}, false},
			{"on-demand per-mr node", map[string]string{"karpenter.sh/capacity-type": "on-demand", "per-mr": "true"}, false},
		} {
			if got := matchesRequired(d, tc.labels); got != tc.want {
				t.Errorf("role %q, %s: schedulable = %v, want %v", role, tc.name, got, tc.want)
			}
		}
		if pref := preferredCapacityType(d); !contains(pref, "on-demand") {
			t.Errorf("role %q: expected on-demand preference, got preferred=%v", role, pref)
		}
	}
}

// matchesRequired evaluates the required node affinity against a node's
// labels for the operators capacityAffinity uses (terms are ORed,
// expressions ANDed, as the scheduler does).
func matchesRequired(d *PodDefaults, labels map[string]string) bool {
	req := d.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	if req == nil {
		return true
	}
	for _, term := range req.NodeSelectorTerms {
		ok := true
		for _, expr := range term.MatchExpressions {
			v, has := labels[expr.Key]
			switch expr.Operator {
			case corev1.NodeSelectorOpIn:
				ok = ok && has && contains(expr.Values, v)
			case corev1.NodeSelectorOpNotIn:
				ok = ok && (!has || !contains(expr.Values, v))
			case corev1.NodeSelectorOpDoesNotExist:
				ok = ok && !has
			case corev1.NodeSelectorOpExists:
				ok = ok && has
			default:
				ok = false
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// requiredExcludesPerMR reports whether the affinity keeps the hard per-mr
// exclusion.
func requiredExcludesPerMR(d *PodDefaults) bool {
	req := d.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	if req == nil {
		return false
	}
	for _, term := range req.NodeSelectorTerms {
		for _, expr := range term.MatchExpressions {
			if expr.Key == "per-mr" && expr.Operator == corev1.NodeSelectorOpDoesNotExist {
				return true
			}
		}
	}
	return false
}

func TestRoleRequiresOnDemandAndInteractivePriority(t *testing.T) {
	tests := []struct {
		role         string
		wantOnDemand bool
		wantPriority bool
	}{
		{"thread", true, true},
		{"dm", true, true},
		// Gardeners run on Spot but keep preemption priority (gd-Fyz3w0tOnf).
		{"gardener", false, true},
		{"plan-gardener", false, true},
		{"crew", false, false},
		{"job", false, false},
		{"polecat", false, false},
		{"", false, false},
		{"crew,thread", true, true}, // any interactive token wins
		{"thread,crew", true, true}, // primary interactive
		{"gardener,thread", true, true},
		{"crew,plan-gardener", false, true},
		{"crew,job", false, false},
		{" thread ", true, true}, // whitespace tolerant
		{" plan-gardener ", false, true},
	}
	for _, tc := range tests {
		if got := roleRequiresOnDemand(tc.role); got != tc.wantOnDemand {
			t.Errorf("roleRequiresOnDemand(%q) = %v, want %v", tc.role, got, tc.wantOnDemand)
		}
		if got := roleHasInteractivePriority(tc.role); got != tc.wantPriority {
			t.Errorf("roleHasInteractivePriority(%q) = %v, want %v", tc.role, got, tc.wantPriority)
		}
	}
}

// A tenant land's pod (gd-NDJIOutLsv) is required off the platform pair, the
// control plane and per-MR nodes, and off every node that holds a fleet agent
// pod: an agent pod in any namespace without the land label.
func TestLandTenantAffinity(t *testing.T) {
	aff := LandTenantAffinity()

	req := aff.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	if req == nil || len(req.NodeSelectorTerms) != 1 {
		t.Fatalf("required node terms = %+v, want exactly one (terms are ORed)", req)
	}
	want := map[string]string{
		"per-mr":          "DoesNotExist",
		lifecycleLabel:    "NotIn[on-demand]",
		LabelControlPlane: "DoesNotExist",
	}
	got := map[string]string{}
	for _, e := range req.NodeSelectorTerms[0].MatchExpressions {
		v := string(e.Operator)
		if len(e.Values) > 0 {
			v += "[" + strings.Join(e.Values, ",") + "]"
		}
		got[e.Key] = v
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("required node expression %s = %q, want %q", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("required node expressions = %v, want only %v", got, want)
	}
	for _, p := range aff.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution {
		for _, e := range p.Preference.MatchExpressions {
			if slices.Contains(e.Values, "on-demand") {
				t.Errorf("tenant pod prefers on-demand: %+v", e)
			}
		}
	}

	if aff.PodAntiAffinity == nil || len(aff.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution) != 1 {
		t.Fatalf("required pod anti-affinity = %+v, want one term", aff.PodAntiAffinity)
	}
	term := aff.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution[0]
	if term.TopologyKey != corev1.LabelHostname {
		t.Errorf("anti-affinity topology = %q, want per node", term.TopologyKey)
	}
	sel, err := metav1.LabelSelectorAsSelector(term.LabelSelector)
	if err != nil {
		t.Fatal(err)
	}
	if !sel.Matches(labels.Set{LabelAgent: "gd-crew-1"}) || sel.Matches(labels.Set{"app": "postgres"}) {
		t.Errorf("anti-affinity pod selector %q: want agent pods only", sel)
	}
	if term.NamespaceSelector == nil {
		t.Fatal("no namespace selector: anti-affinity would only see the land's own namespace")
	}
	nsSel, err := metav1.LabelSelectorAsSelector(term.NamespaceSelector)
	if err != nil {
		t.Fatal(err)
	}
	for ns, fleet := range map[string]bool{
		"gardenio":        true,
		"gardenio-gvisor": true,
		"land-acme":       false,
	} {
		l := labels.Set{"kubernetes.io/metadata.name": ns}
		if !fleet {
			l[landns.LabelLand] = "acme"
		}
		if nsSel.Matches(l) != fleet {
			t.Errorf("namespace %s: anti-affinity applies = %v, want %v", ns, nsSel.Matches(l), fleet)
		}
	}
}
