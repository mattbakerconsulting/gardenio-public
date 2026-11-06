package podmanager

import (
	"context"
	"fmt"
	"strings"

	"log/slog"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// verticalPodAutoscalerGVR is the CRD coordinate for VerticalPodAutoscaler
// objects produced by the VPA recommender (installed in the gardenio namespace,
// ka-0000004038). We read .status.recommendation off these objects; we never
// create or write them — the recommender owns their lifecycle and the targeting
// (VPA object layout) is owned by the scoping crew (ka-0000001210).
var verticalPodAutoscalerGVR = schema.GroupVersionResource{
	Group:    "autoscaling.k8s.io",
	Version:  "v1",
	Resource: "verticalpodautoscalers",
}

// VPAMemoryStamper looks up the VPA memory recommendation for a crew pod's
// (project, role) workload class at spawn time and returns the right-sized
// memory request to stamp into the podspec.
//
// This is RECOMMENDATION-ONLY application at CREATE (the staged-rollout step-1
// "reco-only stamp" — operator-aligned, ka-0000004606). It does NOT install a
// mutating webhook, does NOT mid-run resize a live pod, and does NOT trip the
// enforce HARD GATE (that work is gated separately, ka-0000003138 /
// ka-0000008205). The recommender is live in the gardenio namespace with
// updateMode: Off (VPA object gardenio-agents-reco), so reading its
// recommendation has no eviction side effects.
//
// Scoping key (from spike ka-0000006358): (gardenio.io/project, gardenio.io/role),
// per namespace. The VPA object name is resolved from a configurable template so
// the controller matches whatever object layout the scoping crew (ka-0000001210)
// finalizes — single shared object today, per-class objects later — without a
// code change.
type VPAMemoryStamper struct {
	dynClient dynamic.Interface
	logger    *slog.Logger

	// enabled gates the whole feature. When false, Recommend is a no-op and the
	// caller leaves the existing (default / project / role) memory request
	// untouched. Default OFF — enabling it only right-sizes classes that already
	// carry a mature recommendation; everything else stays at the safe default.
	enabled bool

	// objectNameTemplate is the VPA object-name pattern, with {project} and
	// {role} placeholders substituted from the pod's class labels. Empty falls
	// back to defaultVPAObjectName (the single live recommender object). This is
	// the seam shared with the scoping crew's VPA-object-naming contract.
	objectNameTemplate string

	// floor is the SAFE minimum memory request. A recommendation below the floor
	// (immature history, a quiet class) is clamped UP to the floor so we never
	// under-provision and OOM-kill a live agent run. This mirrors the VPA
	// object's own minAllowed bound but is enforced controller-side too, so a
	// missing/loose minAllowed on the VPA can't leak an unsafe value through.
	floor resource.Quantity

	// max caps the stamped request so a pathological recommendation can't request
	// an un-schedulable amount. The stamped request is additionally clamped to
	// the pod's existing memory limit (never request > limit).
	max resource.Quantity
}

// VPAMemoryStamperConfig carries the tunables for NewVPAMemoryStamper, parsed
// from controller config (env). All resource values are Kubernetes quantity
// strings (e.g. "1500Mi", "16Gi").
type VPAMemoryStamperConfig struct {
	Enabled bool
	// ObjectNameTemplate may contain {project} and {role}; empty uses the
	// default single-object name.
	ObjectNameTemplate string
	// Floor / Max are quantity strings; empty uses the built-in safe defaults.
	Floor string
	Max   string
}

const (
	// defaultVPAObjectName is the live recommender object in the gardenio
	// namespace (updateMode: Off, ka-0000004038). Used when no per-class naming
	// template is configured, so step-1 works against the object that exists
	// today.
	defaultVPAObjectName = "gardenio-agents-reco"

	// DefaultVPAMemoryFloor is the safe minimum memory request. Chosen to sit at
	// the low end of the scoping crew's proposed minAllowed band (1.5–2Gi) and
	// comfortably above observed crew peak usage (~400–850Mi runc, ~673Mi
	// gVisor) with headroom, so a freshly-stamped pod is never starved.
	DefaultVPAMemoryFloor = "1500Mi"

	// DefaultVPAMemoryMax caps the stamped request. Matches the default memory
	// limit so requests can range up to the burst ceiling but no further.
	DefaultVPAMemoryMax = DefaultMemoryLimit
)

// NewVPAMemoryStamper builds a stamper from config. A nil dynClient (e.g. tests
// that don't exercise the VPA path) yields a disabled stamper. Invalid quantity
// strings fall back to the safe defaults rather than failing controller
// startup.
func NewVPAMemoryStamper(dynClient dynamic.Interface, cfg VPAMemoryStamperConfig, logger *slog.Logger) *VPAMemoryStamper {
	if logger == nil {
		logger = slog.Default()
	}
	floor := mustQuantityOr(cfg.Floor, DefaultVPAMemoryFloor)
	max := mustQuantityOr(cfg.Max, DefaultVPAMemoryMax)
	s := &VPAMemoryStamper{
		dynClient:          dynClient,
		logger:             logger,
		enabled:            cfg.Enabled && dynClient != nil,
		objectNameTemplate: cfg.ObjectNameTemplate,
		floor:              floor,
		max:                max,
	}
	return s
}

// mustQuantityOr parses a quantity string, falling back to def (which is a
// trusted compile-time constant) when the input is empty or unparseable.
func mustQuantityOr(s, def string) resource.Quantity {
	if strings.TrimSpace(s) != "" {
		if q, err := resource.ParseQuantity(s); err == nil {
			return q
		}
	}
	return resource.MustParse(def)
}

// Enabled reports whether the stamper will attempt a recommendation lookup.
func (s *VPAMemoryStamper) Enabled() bool {
	return s != nil && s.enabled
}

// objectName resolves the VPA object name for a (project, role) class from the
// configured template, falling back to the single default object.
func (s *VPAMemoryStamper) objectName(project, role string) string {
	tmpl := s.objectNameTemplate
	if tmpl == "" {
		return defaultVPAObjectName
	}
	name := strings.ReplaceAll(tmpl, "{project}", project)
	name = strings.ReplaceAll(name, "{role}", SanitizeRole(role))
	return name
}

// roleFamily normalizes a granular role to the priced-in VPA class that shares
// its memory working set, or "" when the role is its own class (already matched
// by the exact object) or has no family bucket.
//
// Today the only multi-variant family is GARDENER: live crew pods carry granular
// gardener roles (cost-gardener, infra-gardener, gardener-delivery, …) that have no
// dedicated VPA object, so the exact-name Get NotFounds and the pod stays at the
// flat 8Gi default (task ka-0000007066). Every gardener variant shares the bare
// gardener working set (~2Gi target), so any role with a dash-separated "gardener"
// segment other than the bare "gardener" role falls back to the "gardener" class.
func roleFamily(role string) string {
	role = SanitizeRole(role)
	if role == "" || role == "gardener" {
		// Bare gardener already has its own object; nothing to fall back to.
		return ""
	}
	for _, seg := range strings.Split(role, "-") {
		if seg == "gardener" {
			return "gardener"
		}
	}
	return ""
}

// Recommend returns the right-sized memory request for the spec's (project,
// role) class, and ok=true, when a usable recommendation exists. It returns
// ok=false (and the caller MUST leave the existing request untouched) when:
//   - the stamper is disabled,
//   - the VPA object does not exist yet,
//   - the object carries no recommendation for the "agent" container,
//   - the lookup errors for any reason.
//
// A returned quantity is always clamped to [floor, max] and never exceeds the
// pod's existing memory limit, so it can be stamped directly without
// re-validating. The recommended value is read from
// .status.recommendation.containerRecommendations[name=="agent"].target.memory
// — the host-side recommendation, NOT in-pod cgroup stats (gVisor reports
// node-total in-sandbox, so cgroup readings are useless here, per spike).
func (s *VPAMemoryStamper) Recommend(ctx context.Context, spec AgentPodSpec, memLimit *resource.Quantity) (resource.Quantity, bool) {
	if !s.Enabled() {
		return resource.Quantity{}, false
	}
	if spec.Project == "" || spec.Role == "" {
		// No class key — cannot match a per-class recommendation.
		return resource.Quantity{}, false
	}

	name := s.objectName(spec.Project, spec.Role)
	obj, err := s.getVPA(ctx, spec.Namespace, name)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			// Transient/RBAC/CRD-missing errors must never block or shrink a
			// spawn — log and fall back to the safe existing request.
			s.logger.Warn("vpa recommendation lookup failed; using existing request",
				"vpa", name, "namespace", spec.Namespace,
				"project", spec.Project, "role", spec.Role, "error", err)
			return resource.Quantity{}, false
		}
		// No exact per-role object. Granular gardener-family roles
		// (cost-gardener, infra-gardener, gardener-delivery, …) have no dedicated
		// VPA object but share the bare gardener working set — retry against the
		// family bucket so they right-size too instead of staying at 8Gi
		// (ka-0000007066). Only a genuine NotFound triggers the family retry;
		// transient/RBAC errors above bail out unchanged.
		fam := roleFamily(spec.Role)
		if fam == "" {
			return resource.Quantity{}, false
		}
		famName := s.objectName(spec.Project, fam)
		if famName == name {
			return resource.Quantity{}, false
		}
		obj, err = s.getVPA(ctx, spec.Namespace, famName)
		if err != nil {
			if !apierrors.IsNotFound(err) {
				s.logger.Warn("vpa family recommendation lookup failed; using existing request",
					"vpa", famName, "namespace", spec.Namespace,
					"project", spec.Project, "role", spec.Role, "error", err)
			}
			return resource.Quantity{}, false
		}
		s.logger.Info("vpa exact-role object missing; using role-family bucket",
			"role", SanitizeRole(spec.Role), "family", fam,
			"vpa", famName, "namespace", spec.Namespace)
		name = famName
	}

	rec, found, err := recommendedMemory(obj, ContainerName)
	if err != nil || !found {
		if err != nil {
			s.logger.Warn("parsing vpa recommendation; using existing request",
				"vpa", name, "namespace", spec.Namespace, "error", err)
		}
		return resource.Quantity{}, false
	}

	clamped := s.clamp(rec, memLimit)
	s.logger.Info("stamping vpa-recommended memory request",
		"vpa", name, "namespace", spec.Namespace,
		"project", spec.Project, "role", SanitizeRole(spec.Role),
		"recommended", rec.String(), "stamped", clamped.String())
	return clamped, true
}

// getVPA fetches a VerticalPodAutoscaler object by name from a namespace.
func (s *VPAMemoryStamper) getVPA(ctx context.Context, namespace, name string) (*unstructured.Unstructured, error) {
	return s.dynClient.Resource(verticalPodAutoscalerGVR).
		Namespace(namespace).
		Get(ctx, name, metav1.GetOptions{})
}

// clamp bounds a recommendation to [floor, max] and additionally to memLimit
// (when set), so the stamped request never exceeds the pod's burst limit.
func (s *VPAMemoryStamper) clamp(rec resource.Quantity, memLimit *resource.Quantity) resource.Quantity {
	out := rec.DeepCopy()
	if out.Cmp(s.floor) < 0 {
		out = s.floor.DeepCopy()
	}
	if out.Cmp(s.max) > 0 {
		out = s.max.DeepCopy()
	}
	if memLimit != nil && !memLimit.IsZero() && out.Cmp(*memLimit) > 0 {
		out = memLimit.DeepCopy()
	}
	return out
}

// recommendedMemory extracts the target memory recommendation for the named
// container from a VPA object's .status.recommendation.containerRecommendations.
// Returns found=false when there is no recommendation block (recommender hasn't
// produced one yet) or no entry for the container.
func recommendedMemory(obj *unstructured.Unstructured, containerName string) (resource.Quantity, bool, error) {
	recs, found, err := unstructured.NestedSlice(obj.Object,
		"status", "recommendation", "containerRecommendations")
	if err != nil {
		return resource.Quantity{}, false, fmt.Errorf("reading containerRecommendations: %w", err)
	}
	if !found {
		return resource.Quantity{}, false, nil
	}
	for _, r := range recs {
		rm, ok := r.(map[string]any)
		if !ok {
			continue
		}
		cn, _, _ := unstructured.NestedString(rm, "containerName")
		if cn != containerName {
			continue
		}
		memStr, ok, err := unstructured.NestedString(rm, "target", "memory")
		if err != nil {
			return resource.Quantity{}, false, fmt.Errorf("reading target.memory: %w", err)
		}
		if !ok || memStr == "" {
			return resource.Quantity{}, false, nil
		}
		q, err := resource.ParseQuantity(memStr)
		if err != nil {
			return resource.Quantity{}, false, fmt.Errorf("parsing target.memory %q: %w", memStr, err)
		}
		return q, true, nil
	}
	return resource.Quantity{}, false, nil
}
