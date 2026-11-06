package tests

import (
	"regexp"
	"strings"
	"testing"
)

// ka-0000004926: coopmux is the sole /mux upstream behind edge-auth and runs
// replicas=1, so a single node failure is a full mux outage until the pod is
// rescheduled. K8s' DefaultTolerationSeconds admission controller injects a
// 300s tolerationSeconds for node.kubernetes.io/{unreachable,not-ready}, i.e.
// 5 minutes before the pod is even evicted from a dead node (measured outage
// ka-0000004854). The chart must instead emit EXPLICIT short-tolerationSeconds
// NoExecute tolerations for those two taints so reschedule is fast.

// tolerationSecondsFor returns the tolerationSeconds rendered for the given
// node-condition taint key in the coopmux tolerations block, and whether a
// toleration for that key was found at all. It relies on helm rendering each
// toleration item with alphabetically-ordered keys (effect, key, operator,
// tolerationSeconds).
func tolerationSecondsFor(t *testing.T, out, taintKey string) (string, bool) {
	t.Helper()
	// Match a toleration list item: an effect line, the key line for taintKey,
	// an operator line, then the tolerationSeconds line — as helm renders them.
	re := regexp.MustCompile(
		`(?m)-\s+effect:\s+NoExecute\n\s+key:\s+` + regexp.QuoteMeta(taintKey) +
			`\n\s+operator:\s+Exists\n\s+tolerationSeconds:\s+(\d+)`)
	m := re.FindStringSubmatch(out)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// TestCoopmuxFastReschedule_DefaultShortTolerations verifies the default posture
// emits 30s NoExecute tolerations for both node-condition taints.
func TestCoopmuxFastReschedule_DefaultShortTolerations(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/coopmux/deployment.yaml",
		"coopmux.enabled=true",
	)
	for _, key := range []string{"node.kubernetes.io/unreachable", "node.kubernetes.io/not-ready"} {
		secs, ok := tolerationSecondsFor(t, out, key)
		if !ok {
			t.Fatalf("expected an explicit NoExecute toleration for %q, got:\n%s", key, out)
		}
		if secs != "30" {
			t.Errorf("%s: expected tolerationSeconds 30, got %s", key, secs)
		}
	}
}

// TestCoopmuxFastReschedule_CustomSeconds verifies the tolerationSeconds knob
// flows through to both taints.
func TestCoopmuxFastReschedule_CustomSeconds(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/coopmux/deployment.yaml",
		"coopmux.enabled=true",
		"coopmux.fastReschedule.tolerationSeconds=45",
	)
	for _, key := range []string{"node.kubernetes.io/unreachable", "node.kubernetes.io/not-ready"} {
		secs, ok := tolerationSecondsFor(t, out, key)
		if !ok {
			t.Fatalf("expected a toleration for %q, got:\n%s", key, out)
		}
		if secs != "45" {
			t.Errorf("%s: expected tolerationSeconds 45, got %s", key, secs)
		}
	}
}

// TestCoopmuxFastReschedule_Disabled verifies the opt-out falls back to the
// cluster default: no explicit node-condition tolerations are emitted (the
// tolerations block is absent when nothing else supplies one).
func TestCoopmuxFastReschedule_Disabled(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/coopmux/deployment.yaml",
		"coopmux.enabled=true",
		"coopmux.fastReschedule.enabled=false",
	)
	if strings.Contains(out, "tolerationSeconds") {
		t.Errorf("fastReschedule disabled: no tolerationSeconds should be rendered, got:\n%s", out)
	}
	if strings.Contains(out, "node.kubernetes.io/unreachable") ||
		strings.Contains(out, "node.kubernetes.io/not-ready") {
		t.Errorf("fastReschedule disabled: no explicit node-condition tolerations expected, got:\n%s", out)
	}
}

// TestCoopmuxFastReschedule_UnionsOperatorTolerations verifies fast-reschedule
// tolerations are added ALONGSIDE an operator-supplied coopmux.tolerations
// entry, not in place of it.
func TestCoopmuxFastReschedule_UnionsOperatorTolerations(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/coopmux/deployment.yaml",
		"coopmux.enabled=true",
		"coopmux.tolerations[0].key=dedicated",
		"coopmux.tolerations[0].operator=Exists",
		"coopmux.tolerations[0].effect=NoSchedule",
	)
	if !strings.Contains(out, "key: dedicated") {
		t.Errorf("expected operator-supplied 'dedicated' toleration to survive, got:\n%s", out)
	}
	if _, ok := tolerationSecondsFor(t, out, "node.kubernetes.io/unreachable"); !ok {
		t.Errorf("expected fast-reschedule 'unreachable' toleration alongside operator toleration, got:\n%s", out)
	}
}
