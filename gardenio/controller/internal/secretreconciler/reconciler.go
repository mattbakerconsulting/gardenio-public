// Package secretreconciler reconciles ExternalSecret CRDs from project bead secrets.
//
// When a project bead declares per-project secrets ({env, secret, key} entries),
// this reconciler ensures a matching ExternalSecret CRD exists so that the
// external-secrets-operator provisions the K8s Secret from AWS Secrets Manager.
//
// Naming convention:
//   - K8s Secret name must start with "{project}-" (prefix enforcement)
//   - AWS Secrets Manager path: "gardenio/{k8s-secret-name}"
//
// The reconciler groups multiple env var entries that share the same K8s Secret
// name into a single ExternalSecret with multiple data keys.
package secretreconciler

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"strings"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/config"
)

var externalSecretGVR = schema.GroupVersionResource{
	Group:    "external-secrets.io",
	Version:  "v1",
	Resource: "externalsecrets",
}

// Reconciler ensures ExternalSecret CRDs exist for project bead secrets.
type Reconciler struct {
	dynClient       dynamic.Interface
	namespace       string
	storeName       string
	storeKind       string
	refreshInterval string
	logger          *slog.Logger

	// extraNamespaces are additional namespaces into which every managed
	// ExternalSecret is also materialized. Used for the PSA-privileged gVisor
	// namespace where runsc freeze/thaw agents (and gVisor prewarmed-pool
	// standbys) run: a runsc agent pod routes to cfg.GVisorNamespace
	// (routeGVisorNamespace), so its project/role secrets must exist there too
	// or pod admission stalls on "secret(s) not yet synced" (ka-0000007922).
	extraNamespaces []string

	// warnedPrefix dedupes "skipping secret with invalid prefix" warnings so
	// the same misconfigured (kind, owner, secret) tuple does not spam the
	// log on every reconcile cycle.
	warnedPrefix sync.Map

	// allowSecret, when set, must accept a bead-supplied Secret name before
	// the reconciler materializes it (see SetSecretFilter).
	allowSecret func(secretName string) bool
}

// SetExtraNamespaces configures additional namespaces into which every managed
// ExternalSecret is mirrored, beyond the primary namespace. Empty or
// duplicate-of-primary entries are ignored. Used to include the PSA-privileged
// gVisor namespace so runsc agents/standbys find their project/role secrets
// already synced when the pod routes there (ka-0000007922).
func (r *Reconciler) SetExtraNamespaces(namespaces ...string) {
	for _, ns := range namespaces {
		if ns == "" || ns == r.namespace {
			continue
		}
		r.extraNamespaces = append(r.extraNamespaces, ns)
	}
}

// SetSecretFilter makes the reconciler skip any project, role or account
// Secret that allow rejects. The controller passes the same check that guards
// agent pods (forbidden markers plus AGENT_SECRET_ALLOWLIST), so a bead naming
// a controller-only Secret cannot get an ExternalSecret for it, in the release
// namespace or mirrored into the gVisor namespace (gd-q8nBnOP65d).
func (r *Reconciler) SetSecretFilter(allow func(secretName string) bool) {
	r.allowSecret = allow
}

// secretAllowed applies the SetSecretFilter check, warning once per
// (source, owner, secret).
func (r *Reconciler) secretAllowed(source, owner, secretName string) bool {
	if r.allowSecret == nil || r.allowSecret(secretName) {
		return true
	}
	if _, seen := r.warnedPrefix.LoadOrStore("filter|"+source+"|"+owner+"|"+secretName, struct{}{}); !seen {
		r.logger.Warn("skipping secret refused for agent pods",
			"secret", secretName, source, owner)
	}
	return false
}

// New creates a new ExternalSecret reconciler.
func New(dynClient dynamic.Interface, namespace, storeName, storeKind, refreshInterval string, logger *slog.Logger) *Reconciler {
	return &Reconciler{
		dynClient:       dynClient,
		namespace:       namespace,
		storeName:       storeName,
		storeKind:       storeKind,
		refreshInterval: refreshInterval,
		logger:          logger,
	}
}

// secretGroup collects all keys that belong to the same K8s Secret.
type secretGroup struct {
	project    string // project name (for project-scoped secrets)
	role       string // role name (for role-scoped secrets; empty for project secrets)
	account    string // account name (for account-credential secrets; empty for project/role secrets)
	secretName string
	keys       []keyMapping
}

// keyMapping maps an ExternalSecret data entry: secretKey in the K8s Secret
// to a remote ref property in AWS Secrets Manager.
type keyMapping struct {
	secretKey string // key within the K8s Secret (e.g., "email", "api-token")
	property  string // property within the AWS SM secret (same as secretKey)
}

// Reconcile ensures ExternalSecrets exist for all valid project/role bead
// secrets and account-bead credential references, and removes controller-
// managed ExternalSecrets that are no longer referenced.
//
// Account credentials are included because podspec injects
// CLAUDE_CODE_OAUTH_TOKEN on agent pods from the account bead's
// `credential: secret:<name>` field. Without tracking them here,
// cleanupOrphaned would delete the corresponding ExternalSecret and pods
// would fail with `secret not found`.
func (r *Reconciler) Reconcile(ctx context.Context, projects map[string]config.ProjectCacheEntry, roles map[string]config.RoleCacheEntry, accounts map[string]config.AccountCacheEntry) error {
	groups := r.buildSecretGroups(projects, roles, accounts)

	// Build set of desired secret names for cleanup comparison.
	desired := make(map[string]struct{}, len(groups))
	for _, g := range groups {
		desired[g.secretName] = struct{}{}
	}

	// Build sets of live owners for cleanup safety. An ExternalSecret labeled
	// for a project/role/account that still exists in the cache must not be
	// deleted as an "orphan" — its absence from `desired` likely reflects a
	// transient cache state (e.g. unmarshal hiccup that produced an empty
	// Secrets list), not a real removal. See ka-0000008048 for the incident
	// where a stale-cache window wiped 21 monorepo-* ExternalSecrets that the
	// fleet-manager VAP then refused to let the controller recreate.
	liveOwners := liveOwnerSet{
		projects: projects,
		roles:    roles,
		accounts: accounts,
	}

	// Mirror every managed ExternalSecret into the primary namespace AND any
	// extra namespaces (the gVisor ns), so runsc agents/standbys routed there
	// find their secrets already synced (ka-0000007922).
	var errs []error
	for _, ns := range append([]string{r.namespace}, r.extraNamespaces...) {
		if nsErrs := r.reconcileNamespace(ctx, ns, groups, desired, liveOwners); nsErrs != nil {
			errs = append(errs, nsErrs...)
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("secret reconciliation had %d errors: %w", len(errs), errs[0])
	}
	return nil
}

// reconcileNamespace reconciles all secret groups toward desired state within a
// single namespace: create when missing, update on drift, then clean up
// orphaned controller-managed ExternalSecrets. Factored out of Reconcile so the
// same logic runs once per target namespace (primary + extra gVisor ns).
func (r *Reconciler) reconcileNamespace(ctx context.Context, namespace string, groups []secretGroup, desired map[string]struct{}, liveOwners liveOwnerSet) []error {
	client := r.dynClient.Resource(externalSecretGVR).Namespace(namespace)

	var errs []error

	// Reconcile each ExternalSecret toward desired state: create when missing,
	// update when the existing CRD diverges (e.g. project bead's secrets[]
	// gained a new entry for an already-managed AWS secret), no-op when in
	// sync. ESO is idempotent on the K8s side so a no-diff Update is safe but
	// pointless — we skip it to keep the reconcile loop quiet.
	for _, g := range groups {
		desiredObj := r.buildExternalSecret(g, namespace)

		current, err := client.Get(ctx, g.secretName, metav1.GetOptions{})
		if err != nil {
			if !isNotFound(err) {
				errs = append(errs, fmt.Errorf("fetching ExternalSecret %s: %w", g.secretName, err))
				continue
			}
			if _, err := client.Create(ctx, desiredObj, metav1.CreateOptions{}); err != nil {
				errs = append(errs, fmt.Errorf("creating ExternalSecret %s: %w", g.secretName, err))
				continue
			}
			r.logger.Info("created ExternalSecret",
				"name", g.secretName, "project", g.project, "role", g.role, "account", g.account, "keys", len(g.keys))
			continue
		}

		if externalSecretInSync(current, desiredObj) {
			r.logger.Debug("ExternalSecret in sync, skipping",
				"name", g.secretName, "project", g.project)
			continue
		}

		// Apply desired spec/labels onto the live object so resourceVersion
		// and any controller-set fields (annotations, status) are preserved.
		updated := current.DeepCopy()
		if desiredSpec, ok := desiredObj.Object["spec"]; ok {
			updated.Object["spec"] = desiredSpec
		}
		updated.SetLabels(mergeLabels(current.GetLabels(), desiredObj.GetLabels()))

		if _, err := client.Update(ctx, updated, metav1.UpdateOptions{}); err != nil {
			errs = append(errs, fmt.Errorf("updating ExternalSecret %s: %w", g.secretName, err))
			continue
		}
		r.logger.Info("updated ExternalSecret",
			"name", g.secretName, "project", g.project, "role", g.role, "account", g.account, "keys", len(g.keys))
	}

	// Clean up orphaned controller-managed ExternalSecrets.
	if cleanupErrs := r.cleanupOrphaned(ctx, client, desired, liveOwners); cleanupErrs != nil {
		errs = append(errs, cleanupErrs...)
	}

	return errs
}

// buildSecretGroups groups project/role bead secrets and account-bead credential
// references by K8s Secret name, skipping project/role entries that fail prefix
// validation. Account credentials have no prefix rule (secret name is chosen
// per account, not derivable from account name).
func (r *Reconciler) buildSecretGroups(projects map[string]config.ProjectCacheEntry, roles map[string]config.RoleCacheEntry, accounts map[string]config.AccountCacheEntry) []secretGroup {
	// Map: secretName -> secretGroup
	groupMap := make(map[string]*secretGroup)

	// Process project-scoped secrets with "{project}-" prefix enforcement.
	for projectName, entry := range projects {
		for _, s := range entry.Secrets {
			if !strings.HasPrefix(s.Secret, projectName+"-") {
				if _, seen := r.warnedPrefix.LoadOrStore("project|"+projectName+"|"+s.Secret, struct{}{}); !seen {
					r.logger.Warn("skipping secret with invalid prefix",
						"secret", s.Secret, "project", projectName,
						"expected_prefix", projectName+"-")
				}
				continue
			}
			if !r.secretAllowed("project", projectName, s.Secret) {
				continue
			}
			r.addToGroup(groupMap, s, projectName, "")
		}
	}

	// Process role-scoped secrets with "{role}-" prefix enforcement.
	for roleName, entry := range roles {
		for _, s := range entry.Secrets {
			if !strings.HasPrefix(s.Secret, roleName+"-") {
				if _, seen := r.warnedPrefix.LoadOrStore("role|"+roleName+"|"+s.Secret, struct{}{}); !seen {
					r.logger.Warn("skipping role secret with invalid prefix",
						"secret", s.Secret, "role", roleName,
						"expected_prefix", roleName+"-")
				}
				continue
			}
			if !r.secretAllowed("role", roleName, s.Secret) {
				continue
			}
			r.addToGroup(groupMap, s, "", roleName)
		}
	}

	// Process account-bead credential references of the form
	// `credential: secret:<name>`. The single key is always "token" (podspec
	// injects it as CLAUDE_CODE_OAUTH_TOKEN, SecretKey "token"). AWS SM path
	// is "gardenio/<name>" like other controller-managed secrets.
	for accountName, acct := range accounts {
		const prefix = "secret:"
		if !strings.HasPrefix(acct.Credential, prefix) {
			continue
		}
		// A Codex subscription login (provider=openai) is operator-created and
		// keeper-owned: an ExternalSecret would revert the keeper's rotated
		// refresh token (ADR 0004 §C), and its "#auth.json" suffix is not a Secret
		// name.
		if strings.EqualFold(strings.TrimSpace(acct.Provider), "openai") {
			continue
		}
		secretName := strings.TrimPrefix(acct.Credential, prefix)
		if secretName == "" || !r.secretAllowed("account", accountName, secretName) {
			continue
		}
		g, ok := groupMap[secretName]
		if !ok {
			g = &secretGroup{
				account:    accountName,
				secretName: secretName,
			}
			groupMap[secretName] = g
		}
		hasToken := false
		for _, k := range g.keys {
			if k.secretKey == "token" {
				hasToken = true
				break
			}
		}
		if !hasToken {
			g.keys = append(g.keys, keyMapping{secretKey: "token", property: "token"})
		}
	}

	groups := make([]secretGroup, 0, len(groupMap))
	for _, g := range groupMap {
		groups = append(groups, *g)
	}
	return groups
}

// addToGroup adds a secret entry to the group map, deduplicating keys.
func (r *Reconciler) addToGroup(groupMap map[string]*secretGroup, s beadsapi.SecretEntry, project, role string) {
	g, ok := groupMap[s.Secret]
	if !ok {
		g = &secretGroup{
			project:    project,
			role:       role,
			secretName: s.Secret,
		}
		groupMap[s.Secret] = g
	}
	// Deduplicate keys — multiple env vars may reference the same
	// K8s Secret key (e.g., GITLAB_TOKEN and GLAB_TOKEN both use
	// key "token" from the same secret).
	for _, k := range g.keys {
		if k.secretKey == s.Key {
			return
		}
	}
	g.keys = append(g.keys, keyMapping{
		secretKey: s.Key,
		property:  s.Key,
	})
}

// externalSecretInSync returns true when the live ExternalSecret already
// matches the controller's desired shape: spec.data, spec.refreshInterval,
// spec.secretStoreRef, spec.target, and the controller-managed labels are
// all equal. Other fields the controller does not own (status, annotations
// set by ESO, additional user labels) are ignored so reconciliation stays
// idempotent.
func externalSecretInSync(current, desired *unstructured.Unstructured) bool {
	currentSpec, _ := current.Object["spec"].(map[string]any)
	desiredSpec, _ := desired.Object["spec"].(map[string]any)
	if !reflect.DeepEqual(currentSpec, desiredSpec) {
		return false
	}
	currentLabels := current.GetLabels()
	for k, v := range desired.GetLabels() {
		if currentLabels[k] != v {
			return false
		}
	}
	return true
}

// mergeLabels returns the union of current and desired labels with desired
// taking precedence. This preserves user- or operator-set labels on the
// existing CRD while ensuring the controller-managed labels are present
// and authoritative.
func mergeLabels(current, desired map[string]string) map[string]string {
	out := make(map[string]string, len(current)+len(desired))
	maps.Copy(out, current)
	maps.Copy(out, desired)
	return out
}

// isNotFound returns true if the error is a K8s "not found" error.
func isNotFound(err error) bool {
	// k8s.io/apimachinery/pkg/api/errors would give us a typed check,
	// but we can check the error string to avoid an extra import.
	// The dynamic client returns StatusError with reason NotFound.
	return strings.Contains(err.Error(), "not found") ||
		strings.Contains(err.Error(), "NotFound")
}

// liveOwnerSet wraps the cache snapshots passed into Reconcile so cleanup
// can ask "is the project/role/account that owns this ExternalSecret still
// live?" without reaching into Reconciler state.
type liveOwnerSet struct {
	projects map[string]config.ProjectCacheEntry
	roles    map[string]config.RoleCacheEntry
	accounts map[string]config.AccountCacheEntry
}

// hasLiveOwner returns true if the ExternalSecret's owner labels reference
// a project, role, or account that still exists in the supplied caches.
// An ES with no owner labels (legacy, hand-crafted) returns false — those
// remain eligible for deletion when not in `desired`.
func (s liveOwnerSet) hasLiveOwner(labels map[string]string) bool {
	if proj := labels["gardenio.io/project"]; proj != "" {
		if _, ok := s.projects[proj]; ok {
			return true
		}
	}
	if role := labels["gardenio.io/role"]; role != "" {
		if _, ok := s.roles[role]; ok {
			return true
		}
	}
	if acct := labels["gardenio.io/account"]; acct != "" {
		if _, ok := s.accounts[acct]; ok {
			return true
		}
	}
	return false
}

// cleanupOrphaned lists all controller-managed ExternalSecrets and deletes
// those not in the desired set, *unless* their owner label still points at
// a live project/role/account. Only ExternalSecrets with the
// "gardenio.io/managed-by: controller" label are considered.
//
// The owner-liveness guard prevents cache-staleness wipes: if a cache refresh
// momentarily produces an empty Secrets list for a project (e.g. unmarshal
// failure, partial daemon response), the previously-managed ExternalSecrets
// fall out of `desired` but their owner label still matches a live cache
// entry — we keep them and let the next refresh either restore them to
// `desired` or drop the owner entirely.
func (r *Reconciler) cleanupOrphaned(ctx context.Context, client dynamic.ResourceInterface, desired map[string]struct{}, owners liveOwnerSet) []error {
	list, err := client.List(ctx, metav1.ListOptions{
		LabelSelector: "gardenio.io/managed-by=controller",
	})
	if err != nil {
		return []error{fmt.Errorf("listing controller-managed ExternalSecrets: %w", err)}
	}

	var errs []error
	for _, item := range list.Items {
		name := item.GetName()
		if _, ok := desired[name]; ok {
			continue
		}
		if owners.hasLiveOwner(item.GetLabels()) {
			r.logger.Warn("skipping cleanup of ExternalSecret with live owner — likely stale cache",
				"name", name, "labels", item.GetLabels())
			continue
		}
		if err := client.Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
			errs = append(errs, fmt.Errorf("deleting orphaned ExternalSecret %s: %w", name, err))
			continue
		}
		r.logger.Info("deleted orphaned ExternalSecret", "name", name)
	}
	return errs
}

// buildExternalSecret constructs an unstructured ExternalSecret CRD in the
// given namespace. The namespace is passed in (rather than always r.namespace)
// so the same group can be materialized into the gVisor namespace too — the
// object's metadata.namespace must match the client namespace or the fake/real
// API rejects the create (ka-0000007922).
func (r *Reconciler) buildExternalSecret(g secretGroup, namespace string) *unstructured.Unstructured {
	// Build data entries for each key mapping.
	data := make([]interface{}, 0, len(g.keys))
	for _, k := range g.keys {
		data = append(data, map[string]interface{}{
			"secretKey": k.secretKey,
			"remoteRef": map[string]interface{}{
				"key":      "gardenio/" + g.secretName,
				"property": k.property,
			},
		})
	}

	labels := map[string]interface{}{
		"gardenio.io/managed-by": "controller",
	}
	if g.project != "" {
		labels["gardenio.io/project"] = g.project
	}
	if g.role != "" {
		labels["gardenio.io/role"] = g.role
	}
	if g.account != "" {
		labels["gardenio.io/account"] = g.account
	}

	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "external-secrets.io/v1",
			"kind":       "ExternalSecret",
			"metadata": map[string]interface{}{
				"name":      g.secretName,
				"namespace": namespace,
				"labels":    labels,
			},
			"spec": map[string]interface{}{
				"refreshInterval": r.refreshInterval,
				"secretStoreRef": map[string]interface{}{
					"kind": r.storeKind,
					"name": r.storeName,
				},
				"target": map[string]interface{}{
					"name":           g.secretName,
					"creationPolicy": "Owner",
				},
				"data": data,
			},
		},
	}
	return obj
}
