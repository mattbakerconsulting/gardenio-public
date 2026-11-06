package tests

import (
	"strings"
	"testing"
)

// Regression guards for the gVisor mirror of the cluster-admin agent SA.
//
// A project opts into elevated cluster access by setting its project bead's
// `service_account` field to the cluster-admin SA. Its runsc-tagged agents are
// still routed to the gVisor namespace (controller GVISOR_NAMESPACE), so BOTH
// the SA object and the ClusterRoleBinding subject must exist there — a
// ServiceAccount is namespace-scoped identity, and the gVisor copy is a
// different subject than the gardenio-system one.
//
// Without the SA object those pods are rejected at admission; without the
// binding subject they schedule with zero permissions and every kubectl call
// 403s while naming the SA the operator just granted. Both failures are
// per-channel and silent, which is why they are pinned here.

const gvisorNS = "gardenio-gvisor"

func gvisorClusterAdminValues(extra ...string) []string {
	return append([]string{
		"agents.enabled=true",
		"agents.clusterAdmin.enabled=true",
		"gvisor.enabled=true",
		"gvisor.namespace.name=" + gvisorNS,
		"gvisor.namespace.create=true",
	}, extra...)
}

func TestClusterAdminSA_MirroredIntoGvisorNamespace(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/gvisor-namespace.yaml", gvisorClusterAdminValues()...)

	if !strings.Contains(out, "name: test-agent-cluster-admin") {
		t.Fatalf("cluster-admin SA must be mirrored into the gVisor namespace; got:\n%s", out)
	}
	if !strings.Contains(out, "namespace: "+gvisorNS) {
		t.Errorf("mirrored SA must live in the gVisor namespace; got:\n%s", out)
	}
	// The default agent SA mirror must survive alongside it.
	if !strings.Contains(out, "name: test-agent\n") {
		t.Errorf("default agent SA mirror must still render; got:\n%s", out)
	}
}

func TestClusterAdminSA_MirrorCarriesIRSAAnnotation(t *testing.T) {
	t.Parallel()
	// The mirror takes its annotations from agents.clusterAdmin.annotations —
	// the IRSA role-arn. Losing it does not break scheduling, only S3 session
	// archival, so it fails much later than it breaks.
	out := helmTemplate(t, "templates/gvisor-namespace.yaml", gvisorClusterAdminValues(
		`agents.clusterAdmin.annotations.eks\.amazonaws\.com/role-arn=arn:aws:iam::123456789012:role/gardenio-prod-agent`,
	)...)

	if !strings.Contains(out, "arn:aws:iam::123456789012:role/gardenio-prod-agent") {
		t.Errorf("mirrored cluster-admin SA must carry the IRSA role-arn annotation; got:\n%s", out)
	}
}

func TestClusterAdminSA_MirrorOmittedWhenClusterAdminDisabled(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/gvisor-namespace.yaml",
		"agents.enabled=true",
		"agents.clusterAdmin.enabled=false",
		"gvisor.enabled=true",
		"gvisor.namespace.name="+gvisorNS,
		"gvisor.namespace.create=true",
	)

	if strings.Contains(out, "test-agent-cluster-admin") {
		t.Errorf("cluster-admin SA must NOT be mirrored when clusterAdmin is disabled; got:\n%s", out)
	}
	// The default agent SA mirror is unconditional and must remain.
	if !strings.Contains(out, "name: test-agent\n") {
		t.Errorf("default agent SA mirror must still render; got:\n%s", out)
	}
}

// clusterAdminBindingSubjects returns the `subjects:` block of the cluster-admin
// ClusterRoleBinding. The rendered template also contains the SA object itself,
// whose metadata.name matches the subject name — counting over the whole render
// would conflate the two.
func clusterAdminBindingSubjects(t *testing.T, out string) string {
	t.Helper()
	for _, doc := range strings.Split(out, "\n---\n") {
		if !strings.Contains(doc, "kind: ClusterRoleBinding") {
			continue
		}
		i := strings.Index(doc, "\nsubjects:\n")
		if i < 0 {
			t.Fatalf("ClusterRoleBinding has no subjects block; got:\n%s", doc)
		}
		return doc[i:]
	}
	t.Fatalf("no ClusterRoleBinding rendered; got:\n%s", out)
	return ""
}

func TestClusterAdminBinding_IncludesGvisorSubject(t *testing.T) {
	t.Parallel()
	subjects := clusterAdminBindingSubjects(t,
		helmTemplate(t, "templates/controller/agent-cluster-rbac.yaml", gvisorClusterAdminValues()...))

	if !strings.Contains(subjects, "namespace: "+gvisorNS) {
		t.Fatalf("cluster-admin ClusterRoleBinding must bind the gVisor-namespace SA; got:\n%s", subjects)
	}
	// The release-namespace subject must not be displaced by the gVisor one.
	if strings.Count(subjects, "name: test-agent-cluster-admin") != 2 {
		t.Errorf("binding must list BOTH the release-namespace and gVisor-namespace SAs; got:\n%s", subjects)
	}
}

func TestClusterAdminBinding_NoGvisorSubjectWhenGvisorDisabled(t *testing.T) {
	t.Parallel()
	subjects := clusterAdminBindingSubjects(t, helmTemplate(t, "templates/controller/agent-cluster-rbac.yaml",
		"agents.enabled=true",
		"agents.clusterAdmin.enabled=true",
	))

	if strings.Contains(subjects, "namespace: "+gvisorNS) {
		t.Errorf("binding must not reference the gVisor namespace when gvisor is off; got:\n%s", subjects)
	}
	if strings.Count(subjects, "name: test-agent-cluster-admin") != 1 {
		t.Errorf("binding must list exactly one SA subject when gvisor is off; got:\n%s", subjects)
	}
}
