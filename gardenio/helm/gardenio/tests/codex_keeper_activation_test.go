package tests

import (
	"strings"
	"testing"
)

// gd-Hk4RBE2MC7: chart activation for the Codex subscription refresh keeper
// (gd-6ggiM3hiV5, escalation gd-rhOpykucrj). The keeper rotates a single-use
// ChatGPT refresh token, so (1) exactly one controller may run it, (2) the pod
// must get time to persist an in-flight rotation on shutdown, and (3) only the
// controller ServiceAccount may hold `update` on the login Secrets.

// docsOfKind splits a multi-doc render and returns the docs containing every
// needle.
func docsContaining(out string, needles ...string) []string {
	var hits []string
	for _, d := range strings.Split(out, "\n---") {
		ok := true
		for _, n := range needles {
			if !strings.Contains(d, n) {
				ok = false
				break
			}
		}
		if ok {
			hits = append(hits, d)
		}
	}
	return hits
}

func TestCodexKeeper_LeaderElectionOnAtSingleReplica(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/controller/deployment.yaml",
		"agents.enabled=true",
		"agents.replicaCount=1",
	)
	if !strings.Contains(out, "- name: ENABLE_LEADER_ELECTION\n              value: \"true\"") {
		t.Errorf("ENABLE_LEADER_ELECTION must be true even at replicaCount=1 (RollingUpdate maxSurge runs two pods); got:\n%s", out)
	}
}

func TestCodexKeeper_LeaderElectionOnAtMultiReplica(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/controller/deployment.yaml",
		"agents.enabled=true",
		"agents.replicaCount=2",
	)
	if !strings.Contains(out, "- name: ENABLE_LEADER_ELECTION\n              value: \"true\"") {
		t.Errorf("ENABLE_LEADER_ELECTION must stay true at replicaCount=2; got:\n%s", out)
	}
}

func TestCodexKeeper_LeaseRBACPresentAtSingleReplica(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/controller/rbac.yaml", "agents.enabled=true")
	if !strings.Contains(out, `resources: ["leases"]`) {
		t.Errorf("leader election is always on, so the Role must always grant leases; got:\n%s", out)
	}
}

func TestCodexKeeper_TerminationGraceDefaultsTo90WithoutFreezeThaw(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/controller/deployment.yaml",
		"agents.enabled=true",
		"gvisor.freezeThaw.enabled=false",
	)
	if !strings.Contains(out, "terminationGracePeriodSeconds: 90") {
		t.Errorf("controller grace period must be 90s (keeper drain budget is 65s); got:\n%s", out)
	}
}

func TestCodexKeeper_TerminationGraceTakesLargerOfFreezeThaw(t *testing.T) {
	t.Parallel()
	// freezeThaw default grace is 120 (> 90): the larger value wins.
	out := helmTemplate(t, "templates/controller/deployment.yaml",
		"agents.enabled=true",
		"gvisor.freezeThaw.enabled=true",
	)
	if !strings.Contains(out, "terminationGracePeriodSeconds: 120") {
		t.Errorf("with freezeThaw on, the larger grace (120) must win; got:\n%s", out)
	}
	// A freezeThaw value below the keeper floor must not lower it.
	out = helmTemplate(t, "templates/controller/deployment.yaml",
		"agents.enabled=true",
		"gvisor.freezeThaw.enabled=true",
		"gvisor.freezeThaw.terminationGracePeriodSeconds=30",
	)
	if !strings.Contains(out, "terminationGracePeriodSeconds: 90") {
		t.Errorf("freezeThaw=30 must not drop the grace below the 90s keeper floor; got:\n%s", out)
	}
}

func TestCodexKeeper_RoleScopedToNamedSecretsUpdateOnly(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/controller/codex-keeper-rbac.yaml", "agents.enabled=true")
	roles := docsContaining(out, "kind: Role\nmetadata:")
	if len(roles) != 1 {
		t.Fatalf("want exactly one codex-keeper Role, got %d:\n%s", len(roles), out)
	}
	role := roles[0]
	for _, want := range []string{
		`resources: ["secrets"]`,
		`- "gardenio-codex-auth"`,
		`- "gardenio-codex-auth-session"`,
		`verbs: ["update"]`,
	} {
		if !strings.Contains(role, want) {
			t.Errorf("codex-keeper Role missing %q; got:\n%s", want, role)
		}
	}
	for _, bad := range []string{`"create"`, `"delete"`, `"list"`, `"get"`, `"patch"`, `"*"`} {
		if strings.Contains(role, bad) {
			t.Errorf("codex-keeper Role must be update-only; found %s in:\n%s", bad, role)
		}
	}
	if strings.Count(role, "rules:\n") != 1 || strings.Count(role, "- apiGroups:") != 1 {
		t.Errorf("codex-keeper Role must have exactly one rule; got:\n%s", role)
	}
}

func TestCodexKeeper_RoleBoundToControllerSAOnly(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/controller/codex-keeper-rbac.yaml", "agents.enabled=true")
	bindings := docsContaining(out, "kind: RoleBinding")
	if len(bindings) != 1 {
		t.Fatalf("want exactly one codex-keeper RoleBinding, got %d", len(bindings))
	}
	b := bindings[0]
	if !strings.Contains(b, "name: test-agents-codex-keeper") || !strings.Contains(b, "kind: Role\n") {
		t.Errorf("binding must reference the codex-keeper Role; got:\n%s", b)
	}
	if n := strings.Count(b, "- kind: ServiceAccount"); n != 1 {
		t.Errorf("codex-keeper binding must have exactly one subject, got %d:\n%s", n, b)
	}
	if !strings.Contains(b, "name: test-agents-sa") {
		t.Errorf("codex-keeper binding must bind the controller SA test-agents-sa; got:\n%s", b)
	}
	if strings.Contains(b, "aux-sa") {
		t.Errorf("codex-keeper binding must NOT include the aux SA; got:\n%s", b)
	}
}

func TestCodexKeeper_RoleHonoursSecretNamesAndEmptyRendersNothing(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/controller/codex-keeper-rbac.yaml",
		"agents.enabled=true",
		"agents.codexAuth.secretNames={a-auth,b-auth}",
	)
	for _, want := range []string{`- "a-auth"`, `- "a-auth-session"`, `- "b-auth"`, `- "b-auth-session"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "gardenio-codex-auth") {
		t.Errorf("custom secretNames must replace the default; got:\n%s", out)
	}
	full := helmTemplateFull(t, nil, "agents.enabled=true", "agents.codexAuth.secretNames=null")
	if strings.Contains(full, "codex-keeper") {
		t.Errorf("empty secretNames must render no codex-keeper Role/Binding")
	}
	full = helmTemplateFull(t, nil, "agents.enabled=false")
	if strings.Contains(full, "codex-keeper") {
		t.Errorf("agents.enabled=false must render no codex-keeper Role/Binding")
	}
}

func TestCodexKeeper_AuxSARendersWithSameAnnotationsAsAgentsSA(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/controller/serviceaccount.yaml",
		"agents.enabled=true",
		"agents.serviceAccount.annotations.eks\\.amazonaws\\.com/role-arn=arn:aws:iam::1:role/x",
	)
	for _, name := range []string{"test-agents-sa", "test-aux-sa"} {
		docs := docsContaining(out, "kind: ServiceAccount", "name: "+name+"\n")
		if len(docs) != 1 {
			t.Fatalf("want one ServiceAccount %s, got %d:\n%s", name, len(docs), out)
		}
		if !strings.Contains(docs[0], "eks.amazonaws.com/role-arn: arn:aws:iam::1:role/x") {
			t.Errorf("%s must carry the IRSA annotation; got:\n%s", name, docs[0])
		}
	}
}

func TestCodexKeeper_AuxWorkloadsUseAuxSA(t *testing.T) {
	t.Parallel()
	cases := []struct {
		tmpl string
		set  []string
	}{
		{"templates/beads-viewer/deployment.yaml", []string{
			"beadsViewer.enabled=true",
			"beadsViewer.oidc.issuerURL=https://auth.example.com/realms/x",
			"beadsViewer.oidc.clientID=viewer",
		}},
	}
	for _, c := range cases {
		out := helmTemplate(t, c.tmpl, c.set...)
		if !strings.Contains(out, "serviceAccountName: test-aux-sa") {
			t.Errorf("%s must run as test-aux-sa; got:\n%s", c.tmpl, out)
		}
		if strings.Contains(out, "serviceAccountName: test-agents-sa") {
			t.Errorf("%s must not run as the controller SA test-agents-sa", c.tmpl)
		}
	}
	// And the controller stays on the agents SA.
	out := helmTemplate(t, "templates/controller/deployment.yaml", "agents.enabled=true")
	if !strings.Contains(out, "serviceAccountName: test-agents-sa") {
		t.Errorf("controller must stay on test-agents-sa; got a render without it")
	}
}

func TestCodexKeeper_AuxSAHasNoRoleBinding(t *testing.T) {
	t.Parallel()
	// gd-P84rleirJ4: the aux SA needs nothing from the namespace Role (get on
	// every Secret, pods create), so no Role/ClusterRole binding may name it.
	out := helmTemplate(t, "templates/controller/rbac.yaml", "agents.enabled=true")
	if strings.Contains(out, "test-aux-sa") {
		t.Errorf("aux SA must not be a subject of the agents RoleBinding; got:\n%s", out)
	}
	if !strings.Contains(out, "name: test-agents-sa") {
		t.Errorf("controller SA must stay bound to the agents Role; got:\n%s", out)
	}
	full := helmTemplateFull(t, nil,
		"agents.enabled=true",
		"beadsViewer.enabled=true",
		"beadsViewer.oidc.issuerURL=https://auth.example.com/realms/x",
		"beadsViewer.oidc.clientID=viewer",
	)
	for _, d := range strings.Split(full, "\n---") {
		if !strings.Contains(d, "kind: RoleBinding") && !strings.Contains(d, "kind: ClusterRoleBinding") {
			continue
		}
		if strings.Contains(d, "name: test-aux-sa") {
			t.Errorf("no RoleBinding/ClusterRoleBinding may name the aux SA:\n%s", d)
		}
	}
}

func TestCodexKeeper_ExternalSAOverrideKeepsAuxWorkloadsOnIt(t *testing.T) {
	t.Parallel()
	// agents.serviceAccount.create=false: operator-supplied SA, no aux SA is
	// created and the aux workloads keep using the supplied one (no dangling ref).
	out := helmTemplate(t, "templates/beads-viewer/deployment.yaml",
		"beadsViewer.enabled=true",
		"beadsViewer.oidc.issuerURL=https://auth.example.com/realms/x",
		"beadsViewer.oidc.clientID=viewer",
		"agents.serviceAccount.create=false",
		"agents.serviceAccount.name=byo-sa",
	)
	if !strings.Contains(out, "serviceAccountName: byo-sa") {
		t.Errorf("aux workload must use the operator-supplied SA when create=false; got:\n%s", out)
	}
}
