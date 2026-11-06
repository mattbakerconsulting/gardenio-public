package server

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"gardenio/kbeads/internal/model"
)

// fakeRBAC is an in-memory rbacLister. Namespace "" holds the cluster-scoped
// objects. err fails every call; nsErr fails one namespace.
type fakeRBAC struct {
	mu       sync.Mutex
	bindings map[string][]rbacBinding
	roles    map[string][]rbacRole
	err      error
	nsErr    map[string]error
}

func newFakeRBAC() *fakeRBAC {
	return &fakeRBAC{bindings: map[string][]rbacBinding{}, roles: map[string][]rbacRole{}, nsErr: map[string]error{}}
}

func (f *fakeRBAC) listBindings(_ context.Context, ns string) ([]rbacBinding, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	if err := f.nsErr[ns]; err != nil {
		return nil, err
	}
	return append([]rbacBinding(nil), f.bindings[ns]...), nil
}

func (f *fakeRBAC) listRoles(_ context.Context, ns string) ([]rbacRole, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return append([]rbacRole(nil), f.roles[ns]...), nil
}

func (f *fakeRBAC) addRole(ns, name string, rules ...rbacPolicyRule) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.roles[ns] = append(f.roles[ns], rbacRole{Metadata: rbacMeta{Name: name, Namespace: ns}, Rules: rules})
}

func (f *fakeRBAC) bind(ns, name, roleKind, roleName string, subjects ...rbacSubjectRef) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bindings[ns] = append(f.bindings[ns], rbacBinding{
		Metadata: rbacMeta{Name: name, Namespace: ns},
		Subjects: subjects,
		RoleRef:  rbacRoleRef{Kind: roleKind, Name: roleName},
	})
}

func useFakeRBAC(t *testing.T, f *fakeRBAC) {
	t.Helper()
	prev := newRBACLister
	newRBACLister = func() (rbacLister, error) { return f, nil }
	t.Cleanup(func() { newRBACLister = prev })
}

var liveSA = saSubject{Namespace: "gardenio", Name: "gc-k8s-live"}

func saRef(name string) rbacSubjectRef {
	return rbacSubjectRef{Kind: "ServiceAccount", Name: name, Namespace: "gardenio"}
}

func TestEffectiveRules_ResolvesBindings(t *testing.T) {
	f := newFakeRBAC()
	// Cluster-wide: system:authenticated gets a non-resource URL and SSAR.
	f.addRole("", "basic", rbacPolicyRule{Verbs: []string{"create"}, APIGroups: []string{"authorization.k8s.io"}, Resources: []string{"selfsubjectaccessreviews"}})
	f.addRole("", "discovery", rbacPolicyRule{Verbs: []string{"get"}, NonResourceURLs: []string{"/healthz"}})
	f.bind("", "basic", "ClusterRole", "basic", rbacSubjectRef{Kind: "Group", Name: "system:authenticated"})
	f.bind("", "discovery", "ClusterRole", "discovery", rbacSubjectRef{Kind: "Group", Name: "system:authenticated"})
	// gc-dogfood: a Role, bound to the SA (namespace defaulted).
	f.addRole("gc-dogfood", "session-driver",
		rbacPolicyRule{Verbs: []string{"create", "get"}, APIGroups: []string{""}, Resources: []string{"pods", "pods/exec"}},
		rbacPolicyRule{Verbs: []string{"list"}, APIGroups: []string{""}, Resources: []string{"pods"}},
	)
	f.bind("gc-dogfood", "sd", "Role", "session-driver", rbacSubjectRef{Kind: "ServiceAccount", Name: "gc-k8s-live", Namespace: "gardenio"})
	// A RoleBinding'd ClusterRole's non-resource rules never apply.
	f.addRole("", "mixed", rbacPolicyRule{Verbs: []string{"get"}, APIGroups: []string{"batch"}, Resources: []string{"jobs"}}, rbacPolicyRule{Verbs: []string{"get"}, NonResourceURLs: []string{"/metrics"}})
	f.bind("gc-dogfood", "mixed", "ClusterRole", "mixed", rbacSubjectRef{Kind: "User", Name: "system:serviceaccount:gardenio:gc-k8s-live"})
	// Bindings to someone else are ignored.
	f.bind("gc-dogfood", "other", "Role", "session-driver", saRef("gardenio-agent"))

	useFakeRBAC(t, f)
	got := probeServiceAccountPermissions(context.Background(), liveSA, []string{"gardenio", "gc-dogfood"})
	if len(got) != 2 || got[0].Error != "" || got[1].Error != "" {
		t.Fatalf("probe = %+v", got)
	}
	want := []permissionRule{
		{APIGroup: "", Resource: "pods", Verbs: []string{"create", "get", "list"}},
		{APIGroup: "", Resource: "pods/exec", Verbs: []string{"create", "get"}},
		{APIGroup: "authorization.k8s.io", Resource: "selfsubjectaccessreviews", Verbs: []string{"create"}},
		{APIGroup: "batch", Resource: "jobs", Verbs: []string{"get"}},
		{NonResourceURL: "/healthz", Verbs: []string{"get"}},
	}
	if !reflect.DeepEqual(got[1].Rules, want) {
		t.Errorf("gc-dogfood rules =\n%+v\nwant\n%+v", got[1].Rules, want)
	}
	if len(got[0].Rules) != 2 {
		t.Errorf("gardenio should carry only the cluster-wide rules, got %+v", got[0].Rules)
	}
}

func TestProbe_ErrorsAreRecordedPerNamespace(t *testing.T) {
	f := newFakeRBAC()
	f.nsErr["gc-dogfood"] = errors.New("forbidden")
	useFakeRBAC(t, f)
	got := probeServiceAccountPermissions(context.Background(), liveSA, []string{"gardenio", "gc-dogfood"})
	if got[0].Error != "" || got[1].Error != "forbidden" {
		t.Errorf("probe = %+v", got)
	}

	prev := newRBACLister
	newRBACLister = func() (rbacLister, error) { return nil, errors.New("no ServiceAccount token mounted") }
	defer func() { newRBACLister = prev }()
	got = probeServiceAccountPermissions(context.Background(), liveSA, []string{"gardenio", "gc-dogfood"})
	for _, ns := range got {
		if !strings.Contains(ns.Error, "no ServiceAccount token") {
			t.Errorf("namespace %s: error %q", ns.Namespace, ns.Error)
		}
	}
}

func rows(t *testing.T, approved, live []permissionRule) ([]permissionDiffRow, bool) {
	t.Helper()
	return diffNamespaceRules(flattenFromRules(approved), flattenFromRules(live))
}

func flattenFromRules(in []permissionRule) []permissionRule {
	var raw []rbacPolicyRule
	for _, r := range in {
		if r.NonResourceURL != "" {
			raw = append(raw, rbacPolicyRule{Verbs: r.Verbs, NonResourceURLs: []string{r.NonResourceURL}})
			continue
		}
		raw = append(raw, rbacPolicyRule{Verbs: r.Verbs, APIGroups: []string{r.APIGroup}, Resources: []string{r.Resource}, ResourceNames: r.ResourceNames})
	}
	return flattenRules(raw)
}

func addedFor(rs []permissionDiffRow, match func(permissionDiffRow) bool) []string {
	for _, r := range rs {
		if match(r) {
			return r.AddedVerbs
		}
	}
	return nil
}

// Send-back item 1 on PR #194: a name-scoped approval followed by an
// unrestricted live grant is a widening.
func TestDiff_NameScopedToUnrestrictedIsWidening(t *testing.T) {
	approved := []permissionRule{{Resource: "secrets", ResourceNames: []string{"app-token"}, Verbs: []string{"get"}}}
	live := []permissionRule{{Resource: "secrets", Verbs: []string{"get"}}}
	rs, widened := rows(t, approved, live)
	if !widened {
		t.Fatalf("expected widened, rows %+v", rs)
	}
	got := addedFor(rs, func(r permissionDiffRow) bool { return r.Resource == "secrets" && len(r.ResourceNames) == 0 })
	if !reflect.DeepEqual(got, []string{"get"}) {
		t.Errorf("unrestricted secrets added = %v, want [get]; rows %+v", got, rs)
	}
}

// Send-back item 1: a newly granted non-resource URL is a widening.
func TestDiff_NewNonResourceURLIsWidening(t *testing.T) {
	approved := []permissionRule{{NonResourceURL: "/healthz", Verbs: []string{"get"}}}
	live := []permissionRule{{NonResourceURL: "/healthz", Verbs: []string{"get"}}, {NonResourceURL: "/metrics", Verbs: []string{"get"}}}
	rs, widened := rows(t, approved, live)
	if !widened {
		t.Fatalf("expected widened, rows %+v", rs)
	}
	if got := addedFor(rs, func(r permissionDiffRow) bool { return r.NonResourceURL == "/metrics" }); !reflect.DeepEqual(got, []string{"get"}) {
		t.Errorf("/metrics added = %v", got)
	}
	if got := addedFor(rs, func(r permissionDiffRow) bool { return r.NonResourceURL == "/healthz" }); got != nil {
		t.Errorf("/healthz added = %v, want none", got)
	}
}

func TestDiff_CoverageAndNarrowing(t *testing.T) {
	cases := []struct {
		name     string
		approved []permissionRule
		live     []permissionRule
		widened  bool
	}{
		{"identical", []permissionRule{{Resource: "pods", Verbs: []string{"get", "list"}}}, []permissionRule{{Resource: "pods", Verbs: []string{"get", "list"}}}, false},
		{"narrowed", []permissionRule{{Resource: "pods", Verbs: []string{"get", "list"}}}, []permissionRule{{Resource: "pods", Verbs: []string{"get"}}}, false},
		{"added verb", []permissionRule{{Resource: "pods", Verbs: []string{"get"}}}, []permissionRule{{Resource: "pods", Verbs: []string{"get", "delete"}}}, true},
		{"new resource", []permissionRule{{Resource: "pods", Verbs: []string{"get"}}}, []permissionRule{{Resource: "pods", Verbs: []string{"get"}}, {Resource: "secrets", Verbs: []string{"list"}}}, true},
		{"inside approved wildcard", []permissionRule{{APIGroup: "*", Resource: "*", Verbs: []string{"*"}}}, []permissionRule{{APIGroup: "apps", Resource: "deployments", Verbs: []string{"patch"}}}, false},
		{"name subset of approved names", []permissionRule{{Resource: "secrets", ResourceNames: []string{"a", "b"}, Verbs: []string{"get"}}}, []permissionRule{{Resource: "secrets", ResourceNames: []string{"a"}, Verbs: []string{"get"}}}, false},
		{"name outside approved names", []permissionRule{{Resource: "secrets", ResourceNames: []string{"a"}, Verbs: []string{"get"}}}, []permissionRule{{Resource: "secrets", ResourceNames: []string{"b"}, Verbs: []string{"get"}}}, true},
		{"live wildcard verb", []permissionRule{{Resource: "pods", Verbs: []string{"get", "list"}}}, []permissionRule{{Resource: "pods", Verbs: []string{"*"}}}, true},
		{"other group", []permissionRule{{APIGroup: "apps", Resource: "deployments", Verbs: []string{"get"}}}, []permissionRule{{APIGroup: "extensions", Resource: "deployments", Verbs: []string{"get"}}}, true},
		{"subresource wildcard", []permissionRule{{Resource: "pods/*", Verbs: []string{"get"}}}, []permissionRule{{Resource: "pods/log", Verbs: []string{"get"}}}, false},
		{"subresource wildcard is not the parent", []permissionRule{{Resource: "pods/*", Verbs: []string{"get"}}}, []permissionRule{{Resource: "pods", Verbs: []string{"get"}}}, true},
		{"url prefix", []permissionRule{{NonResourceURL: "/api/*", Verbs: []string{"get"}}}, []permissionRule{{NonResourceURL: "/api/v1", Verbs: []string{"get"}}}, false},
		{"resource rule never covers a url", []permissionRule{{APIGroup: "*", Resource: "*", Verbs: []string{"*"}}}, []permissionRule{{NonResourceURL: "/metrics", Verbs: []string{"get"}}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rs, widened := rows(t, tc.approved, tc.live)
			if widened != tc.widened {
				t.Errorf("widened = %v, want %v; rows %+v", widened, tc.widened, rs)
			}
		})
	}
}

// Send-back item 2: an errored probe on either side is unknown, never "no
// drift", and an errored baseline does not flag every live verb.
func TestDiffPermissionSnapshots_ErrorsAreUnknown(t *testing.T) {
	pods := []permissionRule{{Resource: "pods", Verbs: []string{"get"}}}
	approved := []namespacePermissions{
		{Namespace: "a", Error: "no ServiceAccount token mounted"},
		{Namespace: "b", Rules: pods},
		{Namespace: "c", Rules: pods},
	}
	live := []namespacePermissions{
		{Namespace: "a", Rules: []permissionRule{{Resource: "secrets", Verbs: []string{"get", "list"}}}},
		{Namespace: "b", Error: "forbidden"},
		{Namespace: "c", Rules: pods},
	}
	out, widened, unknown := diffPermissionSnapshots(approved, live)
	if widened || !unknown {
		t.Errorf("widened=%v unknown=%v, want false/true", widened, unknown)
	}
	states := map[string]namespaceDiff{}
	for _, d := range out {
		states[d.Namespace] = d
	}
	if d := states["a"]; d.State != permissionDiffUnknown || len(d.Rows) != 0 || !strings.Contains(d.Reason, "no baseline") {
		t.Errorf("a = %+v", d)
	}
	if d := states["b"]; d.State != permissionDiffUnknown || !strings.Contains(d.Reason, "forbidden") {
		t.Errorf("b = %+v", d)
	}
	if d := states["c"]; d.State != permissionDiffUnchanged {
		t.Errorf("c = %+v", d)
	}
}

// The in-cluster lister reads a rotating token, trusts the cluster CA and
// lists the four RBAC collections.
func TestInClusterRBACLister(t *testing.T) {
	var gotAuth []string
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = append(gotAuth, r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/apis/rbac.authorization.k8s.io/v1/clusterrolebindings":
			io.WriteString(w, `{"items":[{"metadata":{"name":"crb"},"subjects":[{"kind":"ServiceAccount","name":"gc-k8s-live","namespace":"gardenio"}],"roleRef":{"kind":"ClusterRole","name":"viewer"}}]}`)
		case "/apis/rbac.authorization.k8s.io/v1/clusterroles":
			io.WriteString(w, `{"items":[{"metadata":{"name":"viewer"},"rules":[{"verbs":["get"],"apiGroups":[""],"resources":["configmaps"]}]}]}`)
		case "/apis/rbac.authorization.k8s.io/v1/namespaces/gc-dogfood/rolebindings":
			io.WriteString(w, `{"items":[]}`)
		case "/apis/rbac.authorization.k8s.io/v1/namespaces/gc-dogfood/roles":
			io.WriteString(w, `{"items":[]}`)
		default:
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `{"kind":"Status","message":"rolebindings is forbidden"}`)
		}
	}))
	defer ts.Close()

	dir := t.TempDir()
	tokenFile, caFile := filepath.Join(dir, "token"), filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(tokenFile, []byte("tok-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ts.Certificate().Raw})
	if err := os.WriteFile(caFile, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	prevTok, prevCA := inClusterTokenFile, inClusterCAFile
	inClusterTokenFile, inClusterCAFile = tokenFile, caFile
	t.Cleanup(func() { inClusterTokenFile, inClusterCAFile = prevTok, prevCA })
	host := strings.TrimPrefix(ts.URL, "https://")
	h, p, _ := strings.Cut(host, ":")
	t.Setenv("KUBERNETES_SERVICE_HOST", h)
	t.Setenv("KUBERNETES_SERVICE_PORT", p)

	got := probeServiceAccountPermissions(context.Background(), liveSA, []string{"gc-dogfood", "gardenio"})
	if got[0].Error != "" || !reflect.DeepEqual(got[0].Rules, []permissionRule{{Resource: "configmaps", Verbs: []string{"get"}}}) {
		t.Errorf("gc-dogfood = %+v", got[0])
	}
	if !strings.Contains(got[1].Error, "403 rolebindings is forbidden") {
		t.Errorf("gardenio error = %q", got[1].Error)
	}
	for _, a := range gotAuth {
		if a != "Bearer tok-1" {
			t.Errorf("Authorization = %q", a)
		}
	}

	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	if _, err := newInClusterRBACLister(); err == nil {
		t.Error("expected an error outside a cluster")
	}
	t.Setenv("KUBERNETES_SERVICE_HOST", h)
	inClusterTokenFile = filepath.Join(dir, "missing")
	if _, err := newInClusterRBACLister(); err == nil || !strings.Contains(err.Error(), "no ServiceAccount token") {
		t.Errorf("missing token: err = %v", err)
	}
}

func TestSpawnServiceAccountName_Precedence(t *testing.T) {
	srv, ms, ctx := testCtx(t)
	if got := srv.spawnServiceAccountName(ctx, "k8s-live", "gardenio"); got != defaultSpawnServiceAccount {
		t.Errorf("no beads: got %q", got)
	}
	ms.beads["gd-proj"] = &model.Bead{ID: "gd-proj", Title: "gardenio", Type: "project", Kind: "config", Status: model.StatusOpen,
		Fields: json.RawMessage(`{"service_account":"proj-sa"}`)}
	if got := srv.spawnServiceAccountName(ctx, "k8s-live", "gardenio"); got != "proj-sa" {
		t.Errorf("project layer: got %q", got)
	}
	makeRoleBead(ms, "gd-role-live", "k8s-live", true)
	ms.beads["gd-role-live"].Fields = json.RawMessage(`{"privileged":"true","service_account":"gc-k8s-live"}`)
	if got := srv.spawnServiceAccountName(ctx, "k8s-live", "gardenio"); got != "gc-k8s-live" {
		t.Errorf("role layer: got %q", got)
	}
}

// Acceptance (gd-SyEYkqe601): approving a k8s-live spawn stores the snapshot;
// adding a RoleBinding to the SA mid-run makes the permission diff show the
// added verbs on the next read. The snapshot fields are server-only.
func TestSpawnPermissionSnapshot_ApproveThenDriftE2E(t *testing.T) {
	const (
		adminToken   = "admin-perm-snap-e2e"
		adminOIDCTok = "oidc-admin-perm-snap-e2e"
		operator     = "oidc:admin@example.com"
	)
	t.Setenv("POD_NAMESPACE", "gardenio")
	f := newFakeRBAC()
	f.addRole("gc-dogfood", "session-driver", rbacPolicyRule{Verbs: []string{"create", "get"}, APIGroups: []string{""}, Resources: []string{"pods", "pods/exec"}})
	f.bind("gc-dogfood", "sd", "Role", "session-driver", saRef("gc-k8s-live"))
	useFakeRBAC(t, f)

	fc := NewFleetConfig("perm-snap-fleet", nil)
	ts, _, ms := newEscalationE2EServer(t, adminToken, fc, &escOIDCMock{tokens: map[string]escTokenInfo{
		adminOIDCTok: {email: "admin@example.com", roles: []string{RoleAdmin}},
	}})
	makeRoleBead(ms, "gd-role-live", "k8s-live", true)
	ms.beads["gd-role-live"].Fields = json.RawMessage(`{"privileged":"true","needs_k8s_api":"true","service_account":"gc-k8s-live"}`)
	ms.beads["gd-policy-live"] = &model.Bead{ID: "gd-policy-live", Title: "k8s-live policy", Type: "spawn_policy", Kind: "config", Status: model.StatusOpen,
		Fields: json.RawMessage(`{"role":"k8s-live","rules":[{"when":{"namespace":"gc-dogfood"},"mode":"human"}]}`)}

	agentCreate := map[string]any{
		"title":  "gardenio-k8s-live-perm-snap-e2e",
		"type":   "agent",
		"labels": []string{"project:gardenio"},
		"fields": map[string]any{"agent": "gardenio-k8s-live-perm-snap-e2e", "mode": "crew", "role": "k8s-live", "project": "gardenio"},
	}
	resp := escRequest(t, ts, "POST", "/v1/beads", adminToken, agentCreate, map[string]string{"X-Operated-By": operator, "X-Escalate": "true"})
	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("escalate: expected 202, got %d: %s", resp.StatusCode, body)
	}
	var escResp EscalationResponse
	escDecode(t, resp, &escResp)
	resp.Body.Close()
	escID := escResp.EscalationID

	// Before approval there is nothing to diff.
	var diffResp struct {
		Available bool                `json:"available"`
		Diff      spawnPermissionDiff `json:"diff"`
	}
	resp = escRequest(t, ts, "GET", "/v1/escalations/"+escID+"/permission-diff", adminOIDCTok, nil, nil)
	escDecode(t, resp, &diffResp)
	resp.Body.Close()
	if diffResp.Available {
		t.Fatalf("diff available before approval: %+v", diffResp)
	}

	// A non-admin may not plant a snapshot.
	resp = escRequest(t, ts, "PATCH", "/v1/beads/"+escID, adminToken,
		map[string]any{"fields": map[string]any{"permissions_at_approval": map[string]any{"service_account": "x"}}}, nil)
	resp.Body.Close()
	if resp.StatusCode < 400 {
		t.Fatalf("PATCH permissions_at_approval by a fleet token: got %d, want refusal", resp.StatusCode)
	}

	resp = escRequest(t, ts, "POST", "/v1/escalations/"+escID+"/approve", adminOIDCTok, map[string]any{"approval_method": "cli"}, nil)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("approve: expected 200, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()

	esc, err := ms.GetBead(context.Background(), escID)
	if err != nil || esc == nil {
		t.Fatalf("escalation: %v", err)
	}
	var stored struct {
		PermissionsAtApproval permissionSnapshot `json:"permissions_at_approval"`
		SnapshotAt            string             `json:"snapshot_at"`
	}
	if err := json.Unmarshal(esc.Fields, &stored); err != nil {
		t.Fatal(err)
	}
	snap := stored.PermissionsAtApproval
	if stored.SnapshotAt == "" || snap.ServiceAccount != "system:serviceaccount:gardenio:gc-k8s-live" {
		t.Fatalf("snapshot not stored: %s", esc.Fields)
	}
	if len(snap.Namespaces) != 2 || snap.Namespaces[0].Namespace != "gardenio" || snap.Namespaces[1].Namespace != "gc-dogfood" {
		t.Fatalf("snapshot namespaces = %+v, want gardenio then gc-dogfood", snap.Namespaces)
	}
	if len(snap.Namespaces[1].Rules) != 2 {
		t.Errorf("gc-dogfood rules = %+v", snap.Namespaces[1].Rules)
	}

	// Unchanged right after approval.
	resp = escRequest(t, ts, "GET", "/v1/escalations/"+escID+"/permission-diff", adminOIDCTok, nil, nil)
	escDecode(t, resp, &diffResp)
	resp.Body.Close()
	if !diffResp.Available || diffResp.Diff.Widened || diffResp.Diff.Unknown {
		t.Fatalf("diff right after approval = %+v", diffResp)
	}

	// Mid-run, someone binds the SA to secrets in its home namespace.
	f.addRole("gardenio", "secret-reader", rbacPolicyRule{Verbs: []string{"get", "list"}, APIGroups: []string{""}, Resources: []string{"secrets"}})
	f.bind("gardenio", "oops", "Role", "secret-reader", saRef("gc-k8s-live"))

	resp = escRequest(t, ts, "GET", "/v1/escalations/"+escID+"/permission-diff", adminOIDCTok, nil, nil)
	escDecode(t, resp, &diffResp)
	resp.Body.Close()
	if !diffResp.Available || !diffResp.Diff.Widened {
		t.Fatalf("diff after the RoleBinding = %+v, want widened", diffResp)
	}
	var added []string
	for _, ns := range diffResp.Diff.Namespaces {
		if ns.Namespace == "gardenio" && ns.State == permissionDiffWidened {
			added = addedFor(ns.Rows, func(r permissionDiffRow) bool { return r.Resource == "secrets" })
		}
	}
	if !reflect.DeepEqual(added, []string{"get", "list"}) {
		t.Errorf("added secrets verbs = %v, want [get list]; diff %+v", added, diffResp.Diff)
	}
}
