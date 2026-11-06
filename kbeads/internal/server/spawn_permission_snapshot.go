// Snapshot of a privileged spawn's ServiceAccount permissions at approval
// time, and the live-vs-approved diff the beads-viewer agent page polls
// (design doc gd-sGW5Y1B8sg §8 point 9, task gd-SyEYkqe601):
//
//	"Snapshot effective permissions at approval. When an escalation for a
//	 privileged spawn is approved (auto or human), the evaluator records
//	 `kubectl auth can-i --list` for the role's ServiceAccount in each
//	 namespace the policy names, as a field on the escalation. The agent
//	 page shows a diff against the live answer while the agent runs, and
//	 any widening is flagged. This does not block; it makes drift visible
//	 where the approval is read."
//
// The probe computes what `auth can-i --list` would print by reading RBAC
// directly (clusterrolebindings, clusterroles, and each namespace's
// rolebindings and roles) and resolving the rules that bind the
// ServiceAccount, its groups and system:authenticated. It needs only
// get/list on those four rbac.authorization.k8s.io resources. It does not
// impersonate: an impersonate grant on a privileged ServiceAccount would hand
// the beads daemon that account's powers (pods/exec in gc-dogfood for
// gc-k8s-live), the very thing this plan is fencing off.
//
// Best-effort throughout: a probe failure (no ServiceAccount token mounted in
// the daemon, no RBAC to read bindings, a transient API error) is recorded as
// an error on that namespace's entry and never blocks the approval it rides
// on. The diff renders such a namespace as unknown, never as "no drift".
package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"gardenio/kbeads/internal/model"
)

// spawnPermissionProbeTimeout bounds one whole probe (every namespace). It
// runs inside an approval request and on every agent-page poll, so a hung
// API server must not stall either for long.
const spawnPermissionProbeTimeout = 8 * time.Second

// defaultSpawnServiceAccount is the controller's default agent ServiceAccount
// (coopServiceAccount in the chart), used when neither the role bead nor the
// project bead sets service_account.
const defaultSpawnServiceAccount = "gardenio-agent"

// In-cluster credentials. Package vars so tests can point them at a temp dir.
var (
	inClusterTokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	inClusterCAFile    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
)

// permissionRule is one row of the effective rule set, in the shape
// `kubectl auth can-i --list` prints: a resource rule (APIGroup + Resource,
// optionally narrowed to ResourceNames) or a non-resource rule (one URL).
type permissionRule struct {
	APIGroup       string   `json:"api_group,omitempty"`
	Resource       string   `json:"resource,omitempty"`
	ResourceNames  []string `json:"resource_names,omitempty"`
	NonResourceURL string   `json:"non_resource_url,omitempty"`
	Verbs          []string `json:"verbs"`
}

// key identifies a row. It covers every dimension, so a name-scoped grant
// and an unrestricted grant of the same resource are distinct rows, and each
// non-resource URL is its own row.
func (r permissionRule) key() string {
	if r.NonResourceURL != "" {
		return "url\x00" + r.NonResourceURL
	}
	return "res\x00" + r.APIGroup + "\x00" + r.Resource + "\x00" + strings.Join(r.ResourceNames, ",")
}

// namespacePermissions is the effective rule set in one namespace, or the
// probe error that stood in for it.
type namespacePermissions struct {
	Namespace string           `json:"namespace"`
	Rules     []permissionRule `json:"rules,omitempty"`
	Error     string           `json:"error,omitempty"`
}

// permissionSnapshot is the permissions_at_approval field: the subject probed
// and its rules per namespace. The diff re-probes the recorded subject, since
// the running pod keeps the ServiceAccount it was created with even if the
// role bead's service_account changes afterwards.
type permissionSnapshot struct {
	ServiceAccount string                 `json:"service_account"`
	Namespaces     []namespacePermissions `json:"namespaces"`
}

// saSubject names a ServiceAccount.
type saSubject struct {
	Namespace string
	Name      string
}

func (s saSubject) username() string {
	return "system:serviceaccount:" + s.Namespace + ":" + s.Name
}

// groups are the groups the API server puts a ServiceAccount token in.
func (s saSubject) groups() []string {
	return []string{"system:serviceaccounts", "system:serviceaccounts:" + s.Namespace, "system:authenticated"}
}

func parseSASubject(username string) (saSubject, bool) {
	rest, ok := strings.CutPrefix(username, "system:serviceaccount:")
	if !ok {
		return saSubject{}, false
	}
	ns, name, ok := strings.Cut(rest, ":")
	if !ok || ns == "" || name == "" {
		return saSubject{}, false
	}
	return saSubject{Namespace: ns, Name: name}, true
}

// ── RBAC resolution ──────────────────────────────────────────────────

type rbacMeta struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
}

type rbacSubjectRef struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
}

type rbacRoleRef struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

type rbacBinding struct {
	Metadata rbacMeta         `json:"metadata"`
	Subjects []rbacSubjectRef `json:"subjects"`
	RoleRef  rbacRoleRef      `json:"roleRef"`
}

type rbacPolicyRule struct {
	Verbs           []string `json:"verbs"`
	APIGroups       []string `json:"apiGroups,omitempty"`
	Resources       []string `json:"resources,omitempty"`
	ResourceNames   []string `json:"resourceNames,omitempty"`
	NonResourceURLs []string `json:"nonResourceURLs,omitempty"`
}

type rbacRole struct {
	Metadata rbacMeta         `json:"metadata"`
	Rules    []rbacPolicyRule `json:"rules"`
}

// bindingApplies mirrors the RBAC authorizer's appliesToUser: a subject
// matches by user name, by group, or as a ServiceAccount whose empty
// namespace defaults to the binding's own (bindingNamespace is "" for a
// ClusterRoleBinding).
func bindingApplies(b rbacBinding, sa saSubject, bindingNamespace string) bool {
	groups := sa.groups()
	for _, subj := range b.Subjects {
		switch subj.Kind {
		case "User":
			if subj.Name == sa.username() {
				return true
			}
		case "Group":
			for _, g := range groups {
				if subj.Name == g {
					return true
				}
			}
		case "ServiceAccount":
			ns := subj.Namespace
			if ns == "" {
				ns = bindingNamespace
			}
			if ns == sa.Namespace && subj.Name == sa.Name {
				return true
			}
		}
	}
	return false
}

// rbacState is everything one probe read from the API.
type rbacState struct {
	clusterRoleBindings []rbacBinding
	clusterRoles        map[string][]rbacPolicyRule
	// per namespace
	roleBindings map[string][]rbacBinding
	roles        map[string]map[string][]rbacPolicyRule
}

// effectiveRules resolves the rules that apply to sa in namespace: every
// rule of a ClusterRole bound by a ClusterRoleBinding, plus the resource
// rules of each Role or ClusterRole a RoleBinding in namespace binds.
// Non-resource rules reached through a RoleBinding never authorize anything
// and are dropped. A binding whose role is missing contributes nothing, as
// in the authorizer.
func effectiveRules(st *rbacState, sa saSubject, namespace string) []permissionRule {
	var raw []rbacPolicyRule
	for _, b := range st.clusterRoleBindings {
		if b.RoleRef.Kind == "ClusterRole" && bindingApplies(b, sa, "") {
			raw = append(raw, st.clusterRoles[b.RoleRef.Name]...)
		}
	}
	for _, b := range st.roleBindings[namespace] {
		if !bindingApplies(b, sa, namespace) {
			continue
		}
		var rules []rbacPolicyRule
		switch b.RoleRef.Kind {
		case "Role":
			rules = st.roles[namespace][b.RoleRef.Name]
		case "ClusterRole":
			rules = st.clusterRoles[b.RoleRef.Name]
		}
		for _, r := range rules {
			if len(r.Resources) > 0 {
				r.NonResourceURLs = nil
				raw = append(raw, r)
			}
		}
	}
	return flattenRules(raw)
}

// flattenRules expands RBAC rules into one permissionRule per (group,
// resource, names) or per URL, merging verbs across rules that share a key,
// sorted for a stable render and a stable stored snapshot.
func flattenRules(raw []rbacPolicyRule) []permissionRule {
	byKey := map[string]*permissionRule{}
	add := func(r permissionRule) {
		k := r.key()
		if have, ok := byKey[k]; ok {
			have.Verbs = append(have.Verbs, r.Verbs...)
			return
		}
		cp := r
		byKey[k] = &cp
	}
	for _, pr := range raw {
		names := append([]string(nil), pr.ResourceNames...)
		sort.Strings(names)
		groups := pr.APIGroups
		if len(pr.Resources) > 0 && len(groups) == 0 {
			groups = []string{""}
		}
		for _, g := range groups {
			for _, res := range pr.Resources {
				add(permissionRule{APIGroup: g, Resource: res, ResourceNames: names, Verbs: pr.Verbs})
			}
		}
		for _, u := range pr.NonResourceURLs {
			add(permissionRule{NonResourceURL: u, Verbs: pr.Verbs})
		}
	}
	out := make([]permissionRule, 0, len(byKey))
	for _, r := range byKey {
		r.Verbs = sortedUnique(r.Verbs)
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key() < out[j].key() })
	return out
}

func sortedUnique(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// ── API client ───────────────────────────────────────────────────────

// rbacLister reads RBAC objects. The production one talks to the in-cluster
// API server; tests substitute a fake via newRBACLister.
type rbacLister interface {
	listBindings(ctx context.Context, namespace string) ([]rbacBinding, error)
	listRoles(ctx context.Context, namespace string) ([]rbacRole, error)
}

// newRBACLister builds the lister for one probe. A package var so tests can
// substitute a fake without a cluster.
var newRBACLister = newInClusterRBACLister

type inClusterRBACLister struct {
	base   string
	token  string
	client *http.Client
}

func newInClusterRBACLister() (rbacLister, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, errors.New("not running in a cluster (KUBERNETES_SERVICE_HOST unset)")
	}
	// Re-read every probe: a projected token rotates.
	token, err := os.ReadFile(inClusterTokenFile)
	if err != nil {
		return nil, fmt.Errorf("no ServiceAccount token mounted in the beads daemon: %w", err)
	}
	ca, err := os.ReadFile(inClusterCAFile)
	if err != nil {
		return nil, fmt.Errorf("reading cluster CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("cluster CA file holds no certificate")
	}
	return &inClusterRBACLister{
		base:  "https://" + net.JoinHostPort(host, port),
		token: strings.TrimSpace(string(token)),
		client: &http.Client{Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		}},
	}, nil
}

func rbacPath(kind, namespace string) string {
	if namespace == "" {
		return "/apis/rbac.authorization.k8s.io/v1/cluster" + kind
	}
	return "/apis/rbac.authorization.k8s.io/v1/namespaces/" + url.PathEscape(namespace) + "/" + kind
}

func (l *inClusterRBACLister) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+l.token)
	req.Header.Set("Accept", "application/json")
	resp, err := l.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		var status struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(body, &status)
		if status.Message == "" {
			status.Message = http.StatusText(resp.StatusCode)
		}
		return fmt.Errorf("GET %s: %d %s", path, resp.StatusCode, status.Message)
	}
	return json.Unmarshal(body, out)
}

func (l *inClusterRBACLister) listBindings(ctx context.Context, namespace string) ([]rbacBinding, error) {
	var list struct {
		Items []rbacBinding `json:"items"`
	}
	err := l.get(ctx, rbacPath("rolebindings", namespace), &list)
	return list.Items, err
}

func (l *inClusterRBACLister) listRoles(ctx context.Context, namespace string) ([]rbacRole, error) {
	var list struct {
		Items []rbacRole `json:"items"`
	}
	err := l.get(ctx, rbacPath("roles", namespace), &list)
	return list.Items, err
}

func rolesByName(roles []rbacRole) map[string][]rbacPolicyRule {
	m := make(map[string][]rbacPolicyRule, len(roles))
	for _, r := range roles {
		m[r.Metadata.Name] = r.Rules
	}
	return m
}

// probeServiceAccountPermissions resolves sa's effective rules in each of
// namespaces, in order. It never returns an error: a failure is recorded on
// every namespace it affected.
func probeServiceAccountPermissions(ctx context.Context, sa saSubject, namespaces []string) []namespacePermissions {
	ctx, cancel := context.WithTimeout(ctx, spawnPermissionProbeTimeout)
	defer cancel()

	out := make([]namespacePermissions, len(namespaces))
	for i, ns := range namespaces {
		out[i].Namespace = ns
	}
	failAll := func(err error) []namespacePermissions {
		for i := range out {
			out[i].Error = err.Error()
		}
		return out
	}

	lister, err := newRBACLister()
	if err != nil {
		return failAll(err)
	}
	st := &rbacState{
		roleBindings: map[string][]rbacBinding{},
		roles:        map[string]map[string][]rbacPolicyRule{},
	}
	if st.clusterRoleBindings, err = lister.listBindings(ctx, ""); err != nil {
		return failAll(err)
	}
	clusterRoles, err := lister.listRoles(ctx, "")
	if err != nil {
		return failAll(err)
	}
	st.clusterRoles = rolesByName(clusterRoles)

	for i, ns := range namespaces {
		bindings, err := lister.listBindings(ctx, ns)
		if err != nil {
			out[i].Error = err.Error()
			continue
		}
		roles, err := lister.listRoles(ctx, ns)
		if err != nil {
			out[i].Error = err.Error()
			continue
		}
		st.roleBindings[ns] = bindings
		st.roles[ns] = rolesByName(roles)
		out[i].Rules = effectiveRules(st, sa, ns)
	}
	return out
}

// spawnPermissionProbe is the probe the approve paths and the diff call. A
// package var so tests can stub the whole probe.
var spawnPermissionProbe = probeServiceAccountPermissions

// ── subject and namespace resolution ─────────────────────────────────

// spawnServiceAccountNamespace is the namespace agent pods, and so their
// ServiceAccounts, live in: the release namespace the daemon runs in.
func spawnServiceAccountNamespace() string {
	if ns := strings.TrimSpace(os.Getenv("POD_NAMESPACE")); ns != "" {
		return ns
	}
	return "gardenio"
}

// spawnServiceAccountName resolves the ServiceAccount the spawned agent's pod
// will run under, with the controller's precedence (podspec.go): the role
// bead's service_account, else the project bead's, else the controller
// default. Agent metadata may only narrow it, never select one, so the
// escalation's operation_spec is not consulted.
func (s *BeadsServer) spawnServiceAccountName(ctx context.Context, role, project string) string {
	if beads, err := roleBeadsByTitle(ctx, s.store, []string{role}); err == nil {
		for _, b := range beads {
			if sa := strings.TrimSpace(parseFieldsToStringMap(b.Fields)["service_account"]); sa != "" {
				return sa
			}
		}
	} else {
		slog.Warn("spawn permission snapshot: listing role beads", "role", role, "error", err)
	}
	if project != "" {
		beads, _, err := s.store.ListBeads(ctx, model.BeadFilter{
			Type:   []model.BeadType{"project"},
			Titles: []string{project},
		})
		if err != nil {
			slog.Warn("spawn permission snapshot: listing project beads", "project", project, "error", err)
		}
		for _, b := range beads {
			if sa := strings.TrimSpace(parseFieldsToStringMap(b.Fields)["service_account"]); sa != "" {
				return sa
			}
		}
	}
	return defaultSpawnServiceAccount
}

// spawnPolicyNamespaces collects the distinct `when.namespace` values across
// policy's rules, in first-seen order.
func spawnPolicyNamespaces(policy *model.Bead) []string {
	if policy == nil {
		return nil
	}
	rules, err := parseSpawnPolicyRules(policy.Fields)
	if err != nil {
		return nil
	}
	var out []string
	for _, rule := range rules {
		if ns := strings.TrimSpace(rule.When.Namespace); ns != "" && !containsString(out, ns) {
			out = append(out, ns)
		}
	}
	return out
}

func containsString(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// spawnSnapshotNamespaces lists the namespaces to probe: the ServiceAccount's
// own namespace (where the pod runs, and where a stray RoleBinding would reach
// the release's secrets), then every namespace the role's spawn_policy names,
// or, when it names none, the requesting agent's pod_namespace — the same
// fallback evaluateSpawnEscalation matches on. requestingAgent is the
// escalation's CreatedBy, never the client-shaped requesting_agent field.
func (s *BeadsServer) spawnSnapshotNamespaces(ctx context.Context, role, requestingAgent string, sa saSubject) []string {
	out := []string{sa.Namespace}
	policy, err := s.lookupSpawnPolicy(ctx, role)
	if err != nil {
		slog.Warn("spawn permission snapshot: looking up spawn_policy", "role", role, "error", err)
	}
	named := spawnPolicyNamespaces(policy)
	if len(named) == 0 {
		if _, ns := s.resolveRequesterContext(ctx, requestingAgent); ns != "" {
			named = []string{ns}
		}
	}
	for _, ns := range named {
		if !containsString(out, ns) {
			out = append(out, ns)
		}
	}
	return out
}

// spawnOperationProject reads fields.project from a spawn escalation's
// operation_spec.
func spawnOperationProject(spec json.RawMessage) string {
	var op struct {
		Fields map[string]any `json:"fields"`
	}
	if len(spec) == 0 || json.Unmarshal(spec, &op) != nil {
		return ""
	}
	p, _ := op.Fields["project"].(string)
	return p
}

// addSpawnPermissionSnapshot records permissions_at_approval and snapshot_at
// into extra for a privileged spawn escalation (a no-op when the bead has no
// spawn_role). Both approve paths, handleApproveEscalation and
// autoApproveSpawnEscalation, call it just before their mergeBeadFields, so
// the snapshot lands in the same write as the approval stamp.
func (s *BeadsServer) addSpawnPermissionSnapshot(ctx context.Context, bead *model.Bead, extra map[string]any) {
	f := parseSpawnEscalationFields(bead)
	if f.SpawnRole == "" {
		return
	}
	sa := saSubject{
		Namespace: spawnServiceAccountNamespace(),
		Name:      s.spawnServiceAccountName(ctx, f.SpawnRole, spawnOperationProject(f.OperationSpec)),
	}
	snap := permissionSnapshot{
		ServiceAccount: sa.username(),
		Namespaces:     spawnPermissionProbe(ctx, sa, s.spawnSnapshotNamespaces(ctx, f.SpawnRole, bead.CreatedBy, sa)),
	}
	for _, ns := range snap.Namespaces {
		if ns.Error != "" {
			slog.Warn("spawn permission snapshot: probe failed; the agent page will show this namespace as unknown",
				"escalation", bead.ID, "service_account", snap.ServiceAccount, "namespace", ns.Namespace, "error", ns.Error)
		}
	}
	payload, err := json.Marshal(snap)
	if err != nil {
		slog.Warn("spawn permission snapshot: encoding", "escalation", bead.ID, "error", err)
		return
	}
	extra["permissions_at_approval"] = json.RawMessage(payload)
	extra["snapshot_at"] = time.Now().UTC().Format(time.RFC3339)
}

// ── live-vs-approved diff ────────────────────────────────────────────

// Namespace diff states.
const (
	permissionDiffUnchanged = "unchanged"
	permissionDiffWidened   = "widened"
	permissionDiffUnknown   = "unknown"
)

// permissionDiffRow is one rule key's approved and live verbs. AddedVerbs are
// the live verbs no approved rule covers.
type permissionDiffRow struct {
	APIGroup       string   `json:"api_group,omitempty"`
	Resource       string   `json:"resource,omitempty"`
	ResourceNames  []string `json:"resource_names,omitempty"`
	NonResourceURL string   `json:"non_resource_url,omitempty"`
	ApprovedVerbs  []string `json:"approved_verbs,omitempty"`
	LiveVerbs      []string `json:"live_verbs,omitempty"`
	AddedVerbs     []string `json:"added_verbs,omitempty"`
}

// namespaceDiff is one namespace's comparison. State is unknown, with
// Reason, when either side's probe failed: there is then nothing honest to
// compare, so no row is flagged and Rows is empty.
type namespaceDiff struct {
	Namespace string              `json:"namespace"`
	State     string              `json:"state"`
	Reason    string              `json:"reason,omitempty"`
	Rows      []permissionDiffRow `json:"rows,omitempty"`
}

// spawnPermissionDiff is the diff payload of
// GET /v1/escalations/{id}/permission-diff. Widened is set by a namespace
// whose state is widened; Unknown by one whose state is unknown. Both can be
// true at once.
type spawnPermissionDiff struct {
	SnapshotAt     string          `json:"snapshot_at"`
	ServiceAccount string          `json:"service_account"`
	Widened        bool            `json:"widened"`
	Unknown        bool            `json:"unknown"`
	Namespaces     []namespaceDiff `json:"namespaces"`
}

func verbMatches(granted []string, verb string) bool {
	for _, g := range granted {
		if g == "*" || g == verb {
			return true
		}
	}
	return false
}

// resourceMatches reports whether an approved resource pattern covers a live
// one, with RBAC's wildcards: "*" is every resource, "*/scale" a subresource
// of any resource, "pods/*" every subresource of pods (but not pods itself).
func resourceMatches(approved, live string) bool {
	if approved == "*" || approved == live {
		return true
	}
	if sub, ok := strings.CutPrefix(approved, "*/"); ok {
		_, liveSub, hasSub := strings.Cut(live, "/")
		return hasSub && (sub == "*" || liveSub == sub)
	}
	if parent, ok := strings.CutSuffix(approved, "/*"); ok {
		liveParent, _, hasSub := strings.Cut(live, "/")
		return hasSub && liveParent == parent
	}
	return false
}

// namesCovered: an approved rule with no names covers every name; one with
// names covers only a live rule restricted to a subset of them.
func namesCovered(approved, live []string) bool {
	if len(approved) == 0 {
		return true
	}
	if len(live) == 0 {
		return false
	}
	for _, n := range live {
		if !containsString(approved, n) {
			return false
		}
	}
	return true
}

func urlMatches(approved, live string) bool {
	if prefix, ok := strings.CutSuffix(approved, "*"); ok {
		return strings.HasPrefix(live, prefix)
	}
	return approved == live
}

// ruleCovers reports whether approved grants verb over everything live names.
// A live wildcard verb is covered only by an approved wildcard verb.
func ruleCovers(approved, live permissionRule, verb string) bool {
	if !verbMatches(approved.Verbs, verb) || (verb == "*" && !containsString(approved.Verbs, "*")) {
		return false
	}
	if live.NonResourceURL != "" || approved.NonResourceURL != "" {
		return live.NonResourceURL != "" && approved.NonResourceURL != "" && urlMatches(approved.NonResourceURL, live.NonResourceURL)
	}
	if approved.APIGroup != "*" && approved.APIGroup != live.APIGroup {
		return false
	}
	if live.Resource == "*" && approved.Resource != "*" {
		return false
	}
	return resourceMatches(approved.Resource, live.Resource) && namesCovered(approved.ResourceNames, live.ResourceNames)
}

// diffNamespaceRules compares one namespace's rule sets. A live verb is added
// when no approved rule covers it, so a name-scoped approval followed by an
// unrestricted live grant, or a new non-resource URL, is a widening, while a
// live grant already inside an approved wildcard is not.
func diffNamespaceRules(approved, live []permissionRule) (rows []permissionDiffRow, widened bool) {
	approvedByKey := map[string]permissionRule{}
	liveByKey := map[string]permissionRule{}
	var keys []string
	for _, r := range approved {
		approvedByKey[r.key()] = r
		keys = append(keys, r.key())
	}
	for _, r := range live {
		if _, ok := approvedByKey[r.key()]; !ok {
			keys = append(keys, r.key())
		}
		liveByKey[r.key()] = r
	}
	sort.Strings(keys)
	for _, k := range keys {
		ap, haveAp := approvedByKey[k]
		lv, haveLv := liveByKey[k]
		base := lv
		if !haveLv {
			base = ap
		}
		row := permissionDiffRow{
			APIGroup:       base.APIGroup,
			Resource:       base.Resource,
			ResourceNames:  base.ResourceNames,
			NonResourceURL: base.NonResourceURL,
		}
		if haveAp {
			row.ApprovedVerbs = ap.Verbs
		}
		if haveLv {
			row.LiveVerbs = lv.Verbs
			for _, v := range lv.Verbs {
				covered := false
				for _, a := range approved {
					if ruleCovers(a, lv, v) {
						covered = true
						break
					}
				}
				if !covered {
					row.AddedVerbs = append(row.AddedVerbs, v)
				}
			}
		}
		if len(row.AddedVerbs) > 0 {
			widened = true
		}
		rows = append(rows, row)
	}
	return rows, widened
}

// diffPermissionSnapshots compares the approval-time snapshot with a live
// probe of the same namespaces.
func diffPermissionSnapshots(approved, live []namespacePermissions) (out []namespaceDiff, widened, unknown bool) {
	liveByNS := make(map[string]namespacePermissions, len(live))
	for _, ns := range live {
		liveByNS[ns.Namespace] = ns
	}
	for _, ap := range approved {
		d := namespaceDiff{Namespace: ap.Namespace}
		lv, ok := liveByNS[ap.Namespace]
		switch {
		case ap.Error != "":
			d.State, d.Reason = permissionDiffUnknown, "no baseline: the probe at approval failed: "+ap.Error
		case !ok:
			d.State, d.Reason = permissionDiffUnknown, "the live probe did not cover this namespace"
		case lv.Error != "":
			d.State, d.Reason = permissionDiffUnknown, "the live probe failed: "+lv.Error
		default:
			var w bool
			d.Rows, w = diffNamespaceRules(ap.Rules, lv.Rules)
			d.State = permissionDiffUnchanged
			if w {
				d.State = permissionDiffWidened
				widened = true
			}
		}
		if d.State == permissionDiffUnknown {
			unknown = true
		}
		out = append(out, d)
	}
	return out, widened, unknown
}

// computeSpawnPermissionDiff re-probes the ServiceAccount recorded in bead's
// permissions_at_approval and diffs it against that snapshot. ok is false
// when the bead is not a spawn escalation or stored no snapshot.
func (s *BeadsServer) computeSpawnPermissionDiff(ctx context.Context, bead *model.Bead) (diff spawnPermissionDiff, ok bool) {
	if parseSpawnEscalationFields(bead).SpawnRole == "" || len(bead.Fields) == 0 {
		return spawnPermissionDiff{}, false
	}
	var fields struct {
		PermissionsAtApproval *permissionSnapshot `json:"permissions_at_approval"`
		SnapshotAt            string              `json:"snapshot_at"`
	}
	if err := json.Unmarshal(bead.Fields, &fields); err != nil {
		slog.Warn("spawn permission diff: malformed permissions_at_approval", "escalation", bead.ID, "error", err)
		return spawnPermissionDiff{}, false
	}
	snap := fields.PermissionsAtApproval
	if snap == nil || len(snap.Namespaces) == 0 {
		return spawnPermissionDiff{}, false
	}
	sa, valid := parseSASubject(snap.ServiceAccount)
	if !valid {
		slog.Warn("spawn permission diff: snapshot names no ServiceAccount", "escalation", bead.ID, "service_account", snap.ServiceAccount)
		return spawnPermissionDiff{}, false
	}
	namespaces := make([]string, 0, len(snap.Namespaces))
	for _, ns := range snap.Namespaces {
		namespaces = append(namespaces, ns.Namespace)
	}
	live := spawnPermissionProbe(ctx, sa, namespaces)
	nsDiffs, widened, unknown := diffPermissionSnapshots(snap.Namespaces, live)
	return spawnPermissionDiff{
		SnapshotAt:     fields.SnapshotAt,
		ServiceAccount: snap.ServiceAccount,
		Widened:        widened,
		Unknown:        unknown,
		Namespaces:     nsDiffs,
	}, true
}

// handleGetEscalationPermissionDiff handles
// GET /v1/escalations/{id}/permission-diff: the live-vs-approved diff the
// beads-viewer agent page polls, so a RoleBinding that widens the
// ServiceAccount mid-run shows where the approval is read, without blocking
// the agent. Same read-authz as handleGetEscalation.
func (s *BeadsServer) handleGetEscalationPermissionDiff(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	bead, err := s.store.GetBead(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && bead == nil) {
		writeError(w, http.StatusNotFound, "escalation not found")
		return
	}
	if err != nil {
		writeServerError(r.Context(), w, "handleGetEscalationPermissionDiff", err, "failed to get escalation")
		return
	}
	if bead.Type != model.TypeEscalation {
		writeError(w, http.StatusNotFound, "escalation not found")
		return
	}
	if !s.authorizeBeadRead(w, r, bead, "escalation not found") {
		return
	}
	diff, ok := s.computeSpawnPermissionDiff(r.Context(), bead)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"available": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"available": true, "diff": diff})
}
