package secretreconciler

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/config"
)

func newTestReconciler(objects ...runtime.Object) (*Reconciler, *dynamicfake.FakeDynamicClient) {
	scheme := runtime.NewScheme()
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{
			externalSecretGVR: "ExternalSecretList",
		},
		objects...,
	)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	r := New(client, "test-ns", "secretstore", "ClusterSecretStore", "15m", logger)
	return r, client
}

func TestReconcile_CreatesExternalSecret(t *testing.T) {
	r, client := newTestReconciler()

	projects := map[string]config.ProjectCacheEntry{
		"myproject": {
			Secrets: []beadsapi.SecretEntry{
				{Env: "JIRA_EMAIL", Secret: "myproject-jira", Key: "email"},
				{Env: "JIRA_API_TOKEN", Secret: "myproject-jira", Key: "api-token"},
			},
		},
	}

	err := r.Reconcile(context.Background(), projects, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify the ExternalSecret was created.
	actions := client.Actions()
	var creates []k8stesting.CreateAction
	for _, a := range actions {
		if ca, ok := a.(k8stesting.CreateAction); ok {
			creates = append(creates, ca)
		}
	}
	if len(creates) != 1 {
		t.Fatalf("expected 1 create action, got %d", len(creates))
	}

	obj := creates[0].GetObject().(*unstructured.Unstructured)
	if obj.GetName() != "myproject-jira" {
		t.Errorf("expected name myproject-jira, got %s", obj.GetName())
	}
	if obj.GetNamespace() != "test-ns" {
		t.Errorf("expected namespace test-ns, got %s", obj.GetNamespace())
	}

	// Check labels.
	labels := obj.GetLabels()
	if labels["gardenio.io/managed-by"] != "controller" {
		t.Errorf("expected managed-by label, got %v", labels)
	}
	if labels["gardenio.io/project"] != "myproject" {
		t.Errorf("expected project label myproject, got %v", labels)
	}

	// Check spec.data has 2 entries.
	spec, ok := obj.Object["spec"].(map[string]interface{})
	if !ok {
		t.Fatal("missing spec")
	}
	data, ok := spec["data"].([]interface{})
	if !ok {
		t.Fatal("missing spec.data")
	}
	if len(data) != 2 {
		t.Errorf("expected 2 data entries, got %d", len(data))
	}

	// Check secretStoreRef.
	storeRef, ok := spec["secretStoreRef"].(map[string]interface{})
	if !ok {
		t.Fatal("missing secretStoreRef")
	}
	if storeRef["name"] != "secretstore" {
		t.Errorf("expected store name secretstore, got %v", storeRef["name"])
	}
	if storeRef["kind"] != "ClusterSecretStore" {
		t.Errorf("expected store kind ClusterSecretStore, got %v", storeRef["kind"])
	}

	// Check remoteRef keys use gardenio/ prefix.
	for _, d := range data {
		entry := d.(map[string]interface{})
		remoteRef := entry["remoteRef"].(map[string]interface{})
		key := remoteRef["key"].(string)
		if key != "gardenio/myproject-jira" {
			t.Errorf("expected remoteRef key gardenio/myproject-jira, got %s", key)
		}
	}
}

func TestReconcile_MirrorsIntoExtraNamespaces(t *testing.T) {
	// With an extra (gVisor) namespace configured, each managed ExternalSecret
	// must be created in BOTH the primary namespace and the extra namespace so
	// runsc agents/standbys routed to the gVisor ns find their secrets synced.
	// See ka-0000007922.
	r, client := newTestReconciler()
	r.SetExtraNamespaces("gardenio-gvisor", "", "test-ns") // dups/empties ignored

	projects := map[string]config.ProjectCacheEntry{
		"myproject": {
			Secrets: []beadsapi.SecretEntry{
				{Env: "GIT_TOKEN", Secret: "myproject-git", Key: "token"},
			},
		},
	}

	if err := r.Reconcile(context.Background(), projects, nil, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	nsForName := map[string][]string{}
	for _, a := range client.Actions() {
		ca, ok := a.(k8stesting.CreateAction)
		if !ok {
			continue
		}
		obj := ca.GetObject().(*unstructured.Unstructured)
		nsForName[obj.GetName()] = append(nsForName[obj.GetName()], obj.GetNamespace())
	}

	got := nsForName["myproject-git"]
	if len(got) != 2 {
		t.Fatalf("expected myproject-git created in 2 namespaces, got %d: %v", len(got), got)
	}
	seen := map[string]bool{}
	for _, ns := range got {
		seen[ns] = true
	}
	if !seen["test-ns"] || !seen["gardenio-gvisor"] {
		t.Errorf("expected creation in both test-ns and gardenio-gvisor, got %v", got)
	}
}

// TestReconcile_ExtraNamespaceOptIn is the opt-in guard for the gVisor-ns
// mirror (ka-0000007922): with NO SetExtraNamespaces the managed ExternalSecret
// is created ONLY in the primary namespace, never in the gVisor namespace. This
// mirrors the opt-in guard the reconciler/pvcgc/stalereaper packages each carry
// and pins the "clean no-op when GVisorNamespace is unset" contract the backfill
// task (gd-obdTf3fIcu) requires: a single-namespace deployment keeps exactly its
// pre-mirror behaviour.
func TestReconcile_ExtraNamespaceOptIn(t *testing.T) {
	r, client := newTestReconciler()
	// Deliberately NOT calling SetExtraNamespaces — the gVisor ns is opt-in.

	projects := map[string]config.ProjectCacheEntry{
		"myproject": {
			Secrets: []beadsapi.SecretEntry{
				{Env: "GIT_TOKEN", Secret: "myproject-git", Key: "token"},
			},
		},
	}

	if err := r.Reconcile(context.Background(), projects, nil, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var createNamespaces []string
	for _, a := range client.Actions() {
		ca, ok := a.(k8stesting.CreateAction)
		if !ok {
			continue
		}
		obj := ca.GetObject().(*unstructured.Unstructured)
		createNamespaces = append(createNamespaces, obj.GetNamespace())
	}

	if len(createNamespaces) != 1 {
		t.Fatalf("expected exactly 1 create (primary ns only), got %d: %v", len(createNamespaces), createNamespaces)
	}
	if createNamespaces[0] != "test-ns" {
		t.Errorf("expected create in primary namespace test-ns, got %q", createNamespaces[0])
	}
	for _, ns := range createNamespaces {
		if ns == "gardenio-gvisor" {
			t.Errorf("gVisor namespace must NOT be touched when the mirror is not enabled, got create in %q", ns)
		}
	}
}

func TestReconcile_SkipsExistingExternalSecret(t *testing.T) {
	existing := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "external-secrets.io/v1",
			"kind":       "ExternalSecret",
			"metadata": map[string]interface{}{
				"name":      "myproject-jira",
				"namespace": "test-ns",
			},
		},
	}
	r, client := newTestReconciler(existing)

	projects := map[string]config.ProjectCacheEntry{
		"myproject": {
			Secrets: []beadsapi.SecretEntry{
				{Env: "JIRA_EMAIL", Secret: "myproject-jira", Key: "email"},
			},
		},
	}

	err := r.Reconcile(context.Background(), projects, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should not have called Create (verb=create) for an already-existing ES;
	// an Update is permissible since the existing CRD has no spec and
	// therefore diverges from the desired shape.
	for _, a := range client.Actions() {
		if a.GetVerb() == "create" {
			t.Errorf("expected no create actions for existing ExternalSecret, got %v", a)
		}
	}
}

// TestReconcile_UpdatesExistingExternalSecret_WhenDataDiverges is the
// regression for gd-7vJNfFQt9c: the reconciler used to be create-only, so
// adding a new {env, secret, key} entry to a project bead's secrets[] for
// an *existing* AWS Secrets Manager secret never propagated to the
// ExternalSecret CRD's data[] list. The fix reconciles to desired state —
// when the live ES diverges, Update the live object.
func TestReconcile_UpdatesExistingExternalSecret_WhenDataDiverges(t *testing.T) {
	// Pre-existing ES with a single data entry for "FOO".
	existing := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "external-secrets.io/v1",
			"kind":       "ExternalSecret",
			"metadata": map[string]interface{}{
				"name":      "myproject-shared",
				"namespace": "test-ns",
				"labels": map[string]interface{}{
					"gardenio.io/managed-by": "controller",
					"gardenio.io/project":    "myproject",
				},
			},
			"spec": map[string]interface{}{
				"refreshInterval": "15m",
				"secretStoreRef": map[string]interface{}{
					"kind": "ClusterSecretStore",
					"name": "secretstore",
				},
				"target": map[string]interface{}{
					"name":           "myproject-shared",
					"creationPolicy": "Owner",
				},
				"data": []interface{}{
					map[string]interface{}{
						"secretKey": "FOO",
						"remoteRef": map[string]interface{}{
							"key":      "gardenio/myproject-shared",
							"property": "FOO",
						},
					},
				},
			},
		},
	}
	r, client := newTestReconciler(existing)

	// Project bead now has FOO + BAR pointing at the same secret.
	projects := map[string]config.ProjectCacheEntry{
		"myproject": {
			Secrets: []beadsapi.SecretEntry{
				{Env: "FOO", Secret: "myproject-shared", Key: "FOO"},
				{Env: "BAR", Secret: "myproject-shared", Key: "BAR"},
			},
		},
	}

	if err := r.Reconcile(context.Background(), projects, nil, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Assert exactly one Update was issued, no Creates, no Deletes. Filter by
	// verb because k8stesting.UpdateAction satisfies the CreateAction
	// interface — a type switch on CreateAction would match both verbs.
	var creates, updates, deletes int
	var updated *unstructured.Unstructured
	for _, a := range client.Actions() {
		switch a.GetVerb() {
		case "create":
			creates++
		case "update":
			updates++
			updated = a.(k8stesting.UpdateAction).GetObject().(*unstructured.Unstructured)
		case "delete":
			deletes++
		}
	}
	if creates != 0 {
		t.Errorf("expected 0 creates (ES already exists), got %d", creates)
	}
	if updates != 1 {
		t.Fatalf("expected 1 update (data[] gained BAR), got %d", updates)
	}
	if deletes != 0 {
		t.Errorf("expected 0 deletes, got %d", deletes)
	}

	// The updated ES must now carry both FOO and BAR data entries.
	spec := updated.Object["spec"].(map[string]interface{})
	data := spec["data"].([]interface{})
	if len(data) != 2 {
		t.Fatalf("expected 2 data entries (FOO + BAR), got %d: %v", len(data), data)
	}
	gotKeys := map[string]string{}
	for _, d := range data {
		entry := d.(map[string]interface{})
		key := entry["secretKey"].(string)
		rr := entry["remoteRef"].(map[string]interface{})
		gotKeys[key] = rr["property"].(string)
	}
	if gotKeys["FOO"] != "FOO" || gotKeys["BAR"] != "BAR" {
		t.Errorf("expected secretKey/property FOO and BAR, got %v", gotKeys)
	}
}

// TestReconcile_NoUpdate_WhenAlreadyInSync confirms idempotency: when the
// live ExternalSecret already matches the desired shape exactly, the
// reconciler issues only a Get and no Update.
func TestReconcile_NoUpdate_WhenAlreadyInSync(t *testing.T) {
	existing := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "external-secrets.io/v1",
			"kind":       "ExternalSecret",
			"metadata": map[string]interface{}{
				"name":      "myproject-shared",
				"namespace": "test-ns",
				"labels": map[string]interface{}{
					"gardenio.io/managed-by": "controller",
					"gardenio.io/project":    "myproject",
				},
			},
			"spec": map[string]interface{}{
				"refreshInterval": "15m",
				"secretStoreRef": map[string]interface{}{
					"kind": "ClusterSecretStore",
					"name": "secretstore",
				},
				"target": map[string]interface{}{
					"name":           "myproject-shared",
					"creationPolicy": "Owner",
				},
				"data": []interface{}{
					map[string]interface{}{
						"secretKey": "FOO",
						"remoteRef": map[string]interface{}{
							"key":      "gardenio/myproject-shared",
							"property": "FOO",
						},
					},
				},
			},
		},
	}
	r, client := newTestReconciler(existing)

	projects := map[string]config.ProjectCacheEntry{
		"myproject": {
			Secrets: []beadsapi.SecretEntry{
				{Env: "FOO", Secret: "myproject-shared", Key: "FOO"},
			},
		},
	}

	if err := r.Reconcile(context.Background(), projects, nil, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, a := range client.Actions() {
		switch a.GetVerb() {
		case "update":
			t.Errorf("expected no update when ES is already in sync, got %v", a)
		case "create":
			t.Errorf("expected no create when ES already exists, got %v", a)
		}
	}
}

func TestReconcile_SkipsInvalidPrefix(t *testing.T) {
	r, client := newTestReconciler()

	projects := map[string]config.ProjectCacheEntry{
		"myproject": {
			Secrets: []beadsapi.SecretEntry{
				{Env: "JIRA_EMAIL", Secret: "acme-jira", Key: "email"},
			},
		},
	}

	err := r.Reconcile(context.Background(), projects, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should not have created anything — prefix mismatch.
	for _, a := range client.Actions() {
		if _, ok := a.(k8stesting.CreateAction); ok {
			t.Error("expected no create actions for secret with invalid prefix")
		}
	}
	// Also no get calls since the entry was skipped before checking existence.
	for _, a := range client.Actions() {
		if ga, ok := a.(k8stesting.GetAction); ok {
			t.Errorf("expected no get actions, got get for %s", ga.GetName())
		}
	}
	_ = client // suppress unused
}

func TestReconcile_GroupsMultipleKeysIntoSingleExternalSecret(t *testing.T) {
	r, client := newTestReconciler()

	projects := map[string]config.ProjectCacheEntry{
		"myproject": {
			Secrets: []beadsapi.SecretEntry{
				{Env: "DB_USER", Secret: "myproject-db", Key: "username"},
				{Env: "DB_PASS", Secret: "myproject-db", Key: "password"},
				{Env: "DB_HOST", Secret: "myproject-db", Key: "host"},
			},
		},
	}

	err := r.Reconcile(context.Background(), projects, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should have exactly 1 create (grouped into a single ExternalSecret).
	var creates []k8stesting.CreateAction
	for _, a := range client.Actions() {
		if ca, ok := a.(k8stesting.CreateAction); ok {
			creates = append(creates, ca)
		}
	}
	if len(creates) != 1 {
		t.Fatalf("expected 1 create action (grouped), got %d", len(creates))
	}

	obj := creates[0].GetObject().(*unstructured.Unstructured)
	spec := obj.Object["spec"].(map[string]interface{})
	data := spec["data"].([]interface{})
	if len(data) != 3 {
		t.Errorf("expected 3 data entries in grouped ExternalSecret, got %d", len(data))
	}
}

func TestReconcile_DeduplicatesSameKey(t *testing.T) {
	r, client := newTestReconciler()

	// GITLAB_TOKEN and GLAB_TOKEN both reference the same secret with the same key.
	projects := map[string]config.ProjectCacheEntry{
		"myproject": {
			Secrets: []beadsapi.SecretEntry{
				{Env: "GITLAB_TOKEN", Secret: "myproject-gitlab-token", Key: "token"},
				{Env: "GLAB_TOKEN", Secret: "myproject-gitlab-token", Key: "token"},
			},
		},
	}

	err := r.Reconcile(context.Background(), projects, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var creates []k8stesting.CreateAction
	for _, a := range client.Actions() {
		if ca, ok := a.(k8stesting.CreateAction); ok {
			creates = append(creates, ca)
		}
	}
	if len(creates) != 1 {
		t.Fatalf("expected 1 create action, got %d", len(creates))
	}

	obj := creates[0].GetObject().(*unstructured.Unstructured)
	spec := obj.Object["spec"].(map[string]interface{})
	data := spec["data"].([]interface{})
	if len(data) != 1 {
		t.Errorf("expected 1 data entry (deduped), got %d", len(data))
	}
}

func TestReconcile_CreatesRoleExternalSecret(t *testing.T) {
	r, client := newTestReconciler()

	projects := map[string]config.ProjectCacheEntry{}
	roles := map[string]config.RoleCacheEntry{
		"crew": {
			Secrets: []beadsapi.SecretEntry{
				{Env: "GIT_TOKEN", Secret: "crew-git-creds", Key: "token"},
				{Env: "GIT_USER", Secret: "crew-git-creds", Key: "username"},
			},
		},
	}

	err := r.Reconcile(context.Background(), projects, roles, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var creates []k8stesting.CreateAction
	for _, a := range client.Actions() {
		if ca, ok := a.(k8stesting.CreateAction); ok {
			creates = append(creates, ca)
		}
	}
	if len(creates) != 1 {
		t.Fatalf("expected 1 create action, got %d", len(creates))
	}

	obj := creates[0].GetObject().(*unstructured.Unstructured)
	if obj.GetName() != "crew-git-creds" {
		t.Errorf("expected name crew-git-creds, got %s", obj.GetName())
	}

	labels := obj.GetLabels()
	if labels["gardenio.io/managed-by"] != "controller" {
		t.Errorf("expected managed-by label, got %v", labels)
	}
	if labels["gardenio.io/role"] != "crew" {
		t.Errorf("expected role label crew, got %v", labels["gardenio.io/role"])
	}
	if _, hasProject := labels["gardenio.io/project"]; hasProject {
		t.Error("expected no project label on role-scoped secret")
	}

	spec := obj.Object["spec"].(map[string]interface{})
	data := spec["data"].([]interface{})
	if len(data) != 2 {
		t.Errorf("expected 2 data entries, got %d", len(data))
	}
}

func TestReconcile_CreatesAccountCredentialExternalSecret(t *testing.T) {
	r, client := newTestReconciler()

	projects := map[string]config.ProjectCacheEntry{}
	accounts := map[string]config.AccountCacheEntry{
		"matthewbaker-acme": {Credential: "secret:monorepo-claude-oauth-token"},
		"gardenio":          {Credential: "secret:gardenio-claude-oauth-gardenio"},
		"env-style":         {Credential: "env:CLAUDE_CODE_OAUTH_TOKEN"}, // not a secret: ref — skipped
		"coopmux-style":     {Credential: "coopmux:some-pool"},           // not a secret: ref — skipped
	}

	err := r.Reconcile(context.Background(), projects, nil, accounts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var creates []k8stesting.CreateAction
	for _, a := range client.Actions() {
		if ca, ok := a.(k8stesting.CreateAction); ok {
			creates = append(creates, ca)
		}
	}
	if len(creates) != 2 {
		t.Fatalf("expected 2 creates (one per account with a secret: credential), got %d", len(creates))
	}

	byName := map[string]*unstructured.Unstructured{}
	for _, c := range creates {
		obj := c.GetObject().(*unstructured.Unstructured)
		byName[obj.GetName()] = obj
	}
	for _, want := range []string{"monorepo-claude-oauth-token", "gardenio-claude-oauth-gardenio"} {
		obj, ok := byName[want]
		if !ok {
			t.Fatalf("expected ExternalSecret %q to be created", want)
			continue
		}
		labels := obj.GetLabels()
		if labels["gardenio.io/managed-by"] != "controller" {
			t.Errorf("%s: expected managed-by=controller label, got %v", want, labels)
		}
		if labels["gardenio.io/account"] == "" {
			t.Errorf("%s: expected gardenio.io/account label, got %v", want, labels)
		}
		spec := obj.Object["spec"].(map[string]interface{})
		data := spec["data"].([]interface{})
		if len(data) != 1 {
			t.Errorf("%s: expected 1 data entry (token), got %d", want, len(data))
			continue
		}
		d := data[0].(map[string]interface{})
		if d["secretKey"] != "token" {
			t.Errorf("%s: expected secretKey=token, got %v", want, d["secretKey"])
		}
		rr := d["remoteRef"].(map[string]interface{})
		if rr["property"] != "token" {
			t.Errorf("%s: expected property=token, got %v", want, rr["property"])
		}
		if rr["key"] != "gardenio/"+want {
			t.Errorf("%s: expected key=gardenio/%s, got %v", want, want, rr["key"])
		}
	}
}

func TestReconcile_AccountCredential_NotCleanedUpAsOrphan(t *testing.T) {
	// Regression: before this fix, account-credential ExternalSecrets were
	// deleted by cleanupOrphaned because the desired set was built only
	// from project/role bead secrets. Simulate an existing controller-
	// managed ExternalSecret for an account credential and assert it stays.
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(externalSecretGVR.GroupVersion().WithKind("ExternalSecret"))
	existing.SetName("monorepo-claude-oauth-token")
	existing.SetNamespace("gardenio")
	existing.SetLabels(map[string]string{
		"gardenio.io/managed-by": "controller",
		"gardenio.io/account":    "matthewbaker-acme",
	})

	r, client := newTestReconciler(existing)

	projects := map[string]config.ProjectCacheEntry{} // no project bead references this secret
	accounts := map[string]config.AccountCacheEntry{
		"matthewbaker-acme": {Credential: "secret:monorepo-claude-oauth-token"},
	}

	if err := r.Reconcile(context.Background(), projects, nil, accounts); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, a := range client.Actions() {
		if da, ok := a.(k8stesting.DeleteAction); ok {
			if da.GetName() == "monorepo-claude-oauth-token" {
				t.Fatal("account-credential ExternalSecret was deleted by cleanupOrphaned; expected preservation")
			}
		}
	}
}

func TestReconcile_SkipsInvalidRolePrefix(t *testing.T) {
	r, client := newTestReconciler()

	projects := map[string]config.ProjectCacheEntry{}
	roles := map[string]config.RoleCacheEntry{
		"crew": {
			Secrets: []beadsapi.SecretEntry{
				{Env: "GOOD", Secret: "crew-valid", Key: "token"},
				{Env: "BAD", Secret: "other-invalid", Key: "token"},
			},
		},
	}

	err := r.Reconcile(context.Background(), projects, roles, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var creates []k8stesting.CreateAction
	for _, a := range client.Actions() {
		if ca, ok := a.(k8stesting.CreateAction); ok {
			creates = append(creates, ca)
		}
	}
	if len(creates) != 1 {
		t.Fatalf("expected 1 create (only valid prefix), got %d", len(creates))
	}
	obj := creates[0].GetObject().(*unstructured.Unstructured)
	if obj.GetName() != "crew-valid" {
		t.Errorf("expected name crew-valid, got %s", obj.GetName())
	}
}

func TestReconcile_ProjectAndRoleTogether(t *testing.T) {
	r, client := newTestReconciler()

	projects := map[string]config.ProjectCacheEntry{
		"myproject": {
			Secrets: []beadsapi.SecretEntry{
				{Env: "DB_PASS", Secret: "myproject-db", Key: "password"},
			},
		},
	}
	roles := map[string]config.RoleCacheEntry{
		"ops": {
			Secrets: []beadsapi.SecretEntry{
				{Env: "GIT_TOKEN", Secret: "ops-git", Key: "token"},
			},
		},
	}

	err := r.Reconcile(context.Background(), projects, roles, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var creates []k8stesting.CreateAction
	for _, a := range client.Actions() {
		if ca, ok := a.(k8stesting.CreateAction); ok {
			creates = append(creates, ca)
		}
	}
	if len(creates) != 2 {
		t.Fatalf("expected 2 creates (project + role), got %d", len(creates))
	}

	// Verify we got one project and one role ExternalSecret.
	names := map[string]*unstructured.Unstructured{}
	for _, ca := range creates {
		obj := ca.GetObject().(*unstructured.Unstructured)
		names[obj.GetName()] = obj
	}
	projObj, ok := names["myproject-db"]
	if !ok {
		t.Fatal("expected myproject-db ExternalSecret")
	}
	if projObj.GetLabels()["gardenio.io/project"] != "myproject" {
		t.Error("expected project label on project secret")
	}

	roleObj, ok := names["ops-git"]
	if !ok {
		t.Fatal("expected ops-git ExternalSecret")
	}
	if roleObj.GetLabels()["gardenio.io/role"] != "ops" {
		t.Error("expected role label on role secret")
	}
}

func TestReconcile_BackwardCompatible_NoRoles(t *testing.T) {
	r, client := newTestReconciler()

	projects := map[string]config.ProjectCacheEntry{
		"myproject": {
			Secrets: []beadsapi.SecretEntry{
				{Env: "TOKEN", Secret: "myproject-creds", Key: "token"},
			},
		},
	}

	// Call without role cache — backward compatible.
	err := r.Reconcile(context.Background(), projects, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var creates []k8stesting.CreateAction
	for _, a := range client.Actions() {
		if ca, ok := a.(k8stesting.CreateAction); ok {
			creates = append(creates, ca)
		}
	}
	if len(creates) != 1 {
		t.Fatalf("expected 1 create, got %d", len(creates))
	}
}

func TestReconcile_CleansUpOrphanedExternalSecrets(t *testing.T) {
	// Pre-existing controller-managed ExternalSecret that is no longer in
	// any project or role bead secrets list.
	orphan := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "external-secrets.io/v1",
			"kind":       "ExternalSecret",
			"metadata": map[string]interface{}{
				"name":      "oldproject-stale-creds",
				"namespace": "test-ns",
				"labels": map[string]interface{}{
					"gardenio.io/managed-by": "controller",
					"gardenio.io/project":    "oldproject",
				},
			},
		},
	}
	// A still-desired ExternalSecret (matches current project secrets).
	desired := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "external-secrets.io/v1",
			"kind":       "ExternalSecret",
			"metadata": map[string]interface{}{
				"name":      "myproject-jira",
				"namespace": "test-ns",
				"labels": map[string]interface{}{
					"gardenio.io/managed-by": "controller",
					"gardenio.io/project":    "myproject",
				},
			},
		},
	}
	r, client := newTestReconciler(orphan, desired)

	projects := map[string]config.ProjectCacheEntry{
		"myproject": {
			Secrets: []beadsapi.SecretEntry{
				{Env: "JIRA_EMAIL", Secret: "myproject-jira", Key: "email"},
			},
		},
	}

	err := r.Reconcile(context.Background(), projects, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The orphan should be deleted; the desired one should remain.
	var deletes []k8stesting.DeleteAction
	for _, a := range client.Actions() {
		if da, ok := a.(k8stesting.DeleteAction); ok {
			deletes = append(deletes, da)
		}
	}
	if len(deletes) != 1 {
		t.Fatalf("expected 1 delete action, got %d", len(deletes))
	}
	if deletes[0].GetName() != "oldproject-stale-creds" {
		t.Errorf("expected delete of oldproject-stale-creds, got %s", deletes[0].GetName())
	}
}

func TestReconcile_SkipsNonControllerManagedExternalSecrets(t *testing.T) {
	// ExternalSecret without the controller label — should NOT be deleted
	// even though it's not in the desired set.
	helmManaged := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "external-secrets.io/v1",
			"kind":       "ExternalSecret",
			"metadata": map[string]interface{}{
				"name":      "helm-deployed-secret",
				"namespace": "test-ns",
				"labels": map[string]interface{}{
					"app.kubernetes.io/managed-by": "Helm",
				},
			},
		},
	}
	r, client := newTestReconciler(helmManaged)

	// Empty projects — no desired secrets at all.
	err := r.Reconcile(context.Background(), map[string]config.ProjectCacheEntry{}, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// No deletes — the Helm-managed ExternalSecret should be left alone.
	for _, a := range client.Actions() {
		if da, ok := a.(k8stesting.DeleteAction); ok {
			t.Errorf("unexpected delete of %s — non-controller ExternalSecrets should not be touched", da.GetName())
		}
	}
}

func TestReconcile_CleansUpAllOrphansWhenNoSecretsDesired(t *testing.T) {
	// Two orphaned controller-managed ExternalSecrets, no desired secrets.
	orphan1 := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "external-secrets.io/v1",
			"kind":       "ExternalSecret",
			"metadata": map[string]interface{}{
				"name":      "proj-a-creds",
				"namespace": "test-ns",
				"labels": map[string]interface{}{
					"gardenio.io/managed-by": "controller",
				},
			},
		},
	}
	orphan2 := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "external-secrets.io/v1",
			"kind":       "ExternalSecret",
			"metadata": map[string]interface{}{
				"name":      "proj-b-creds",
				"namespace": "test-ns",
				"labels": map[string]interface{}{
					"gardenio.io/managed-by": "controller",
				},
			},
		},
	}
	r, client := newTestReconciler(orphan1, orphan2)

	err := r.Reconcile(context.Background(), map[string]config.ProjectCacheEntry{}, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var deletes []k8stesting.DeleteAction
	for _, a := range client.Actions() {
		if da, ok := a.(k8stesting.DeleteAction); ok {
			deletes = append(deletes, da)
		}
	}
	if len(deletes) != 2 {
		t.Fatalf("expected 2 delete actions, got %d", len(deletes))
	}

	deleted := map[string]bool{}
	for _, d := range deletes {
		deleted[d.GetName()] = true
	}
	if !deleted["proj-a-creds"] || !deleted["proj-b-creds"] {
		t.Errorf("expected both orphans deleted, got %v", deleted)
	}
}

// TestReconcile_PreservesStaleCacheOwnerWipe is the regression for ka-0000008048:
// a transient cache state (e.g. a failed JSON unmarshal of the project bead's
// `secrets` field) replaced the live project entry with one whose Secrets
// list is empty. Without owner-aware cleanup the reconciler would wipe every
// controller-managed ExternalSecret labeled for that project — and the
// fleet-manager VAP then refuses to let the controller recreate them in its
// own namespace, leaving agent pods stuck on missing secrets.
func TestReconcile_PreservesStaleCacheOwnerWipe(t *testing.T) {
	managed := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "external-secrets.io/v1",
			"kind":       "ExternalSecret",
			"metadata": map[string]interface{}{
				"name":      "monorepo-git-credentials",
				"namespace": "test-ns",
				"labels": map[string]interface{}{
					"gardenio.io/managed-by": "controller",
					"gardenio.io/project":    "monorepo",
				},
			},
		},
	}
	r, client := newTestReconciler(managed)

	// Project cache entry exists but produced an empty Secrets list this cycle.
	projects := map[string]config.ProjectCacheEntry{
		"monorepo": {Secrets: nil},
	}

	if err := r.Reconcile(context.Background(), projects, nil, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, a := range client.Actions() {
		if da, ok := a.(k8stesting.DeleteAction); ok {
			t.Fatalf("unexpected delete of %s — owner is still in cache, deletion should be deferred", da.GetName())
		}
	}
}

// TestReconcile_DeletesOrphanWhenOwnerAbsent confirms cleanup still works
// for the legitimate case: ES owned by a project that no longer appears in
// the cache (deleted bead) gets removed.
func TestReconcile_DeletesOrphanWhenOwnerAbsent(t *testing.T) {
	orphan := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "external-secrets.io/v1",
			"kind":       "ExternalSecret",
			"metadata": map[string]interface{}{
				"name":      "deleted-project-creds",
				"namespace": "test-ns",
				"labels": map[string]interface{}{
					"gardenio.io/managed-by": "controller",
					"gardenio.io/project":    "deleted-project",
				},
			},
		},
	}
	r, client := newTestReconciler(orphan)

	// The owning project is gone from the cache.
	projects := map[string]config.ProjectCacheEntry{}

	if err := r.Reconcile(context.Background(), projects, nil, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var deletes []k8stesting.DeleteAction
	for _, a := range client.Actions() {
		if da, ok := a.(k8stesting.DeleteAction); ok {
			deletes = append(deletes, da)
		}
	}
	if len(deletes) != 1 || deletes[0].GetName() != "deleted-project-creds" {
		t.Fatalf("expected single delete of deleted-project-creds, got %v", deletes)
	}
}

func TestReconcile_MultipleProjects(t *testing.T) {
	r, client := newTestReconciler()

	projects := map[string]config.ProjectCacheEntry{
		"alpha": {
			Secrets: []beadsapi.SecretEntry{
				{Env: "TOKEN", Secret: "alpha-creds", Key: "token"},
			},
		},
		"beta": {
			Secrets: []beadsapi.SecretEntry{
				{Env: "TOKEN", Secret: "beta-creds", Key: "token"},
			},
		},
	}

	err := r.Reconcile(context.Background(), projects, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should create 2 ExternalSecrets (one per project).
	var creates []k8stesting.CreateAction
	for _, a := range client.Actions() {
		if ca, ok := a.(k8stesting.CreateAction); ok {
			creates = append(creates, ca)
		}
	}
	if len(creates) != 2 {
		t.Fatalf("expected 2 create actions, got %d", len(creates))
	}
}

// A provider=openai (Codex subscription) account credential is operator-created
// and keeper-owned: an ExternalSecret would revert the keeper's rotated refresh
// token, so none may be created for it (ADR 0004 §C).
func TestReconcile_SkipsCodexSubscriptionCredential(t *testing.T) {
	r, client := newTestReconciler()

	accounts := map[string]config.AccountCacheEntry{
		"gardenio-codex": {Provider: "openai", Credential: "secret:gardenio-codex-auth#auth.json"},
	}
	if err := r.Reconcile(context.Background(), map[string]config.ProjectCacheEntry{}, nil, accounts); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, a := range client.Actions() {
		if _, ok := a.(k8stesting.CreateAction); ok {
			t.Errorf("no ExternalSecret may be created for a Codex account, got action %v", a)
		}
	}
}

// TestReconcile_SecretFilterSkipsRefusedSecrets: a bead naming a Secret the
// filter refuses (the controller's Keycloak credentials, gd-q8nBnOP65d) gets
// no ExternalSecret on any of the three paths; allowed ones still do.
func TestReconcile_SecretFilterSkipsRefusedSecrets(t *testing.T) {
	r, client := newTestReconciler()
	r.SetSecretFilter(func(name string) bool {
		return name != "gardenio-machine-gardenio-controller" &&
			name != "gardenio-keycloak-runtime-admin" && name != "gardenio-keycloak-first-admin"
	})

	projects := map[string]config.ProjectCacheEntry{
		"gardenio": {Secrets: []beadsapi.SecretEntry{
			{Env: "GIT_TOKEN", Secret: "gardenio-git-credentials", Key: "token"},
			{Env: "KC", Secret: "gardenio-machine-gardenio-controller", Key: "clientSecret"},
		}},
	}
	roles := map[string]config.RoleCacheEntry{
		"gardenio-keycloak": {Secrets: []beadsapi.SecretEntry{
			{Env: "KC_ADMIN", Secret: "gardenio-keycloak-runtime-admin", Key: "password"},
		}},
	}
	accounts := map[string]config.AccountCacheEntry{
		"evil": {Credential: "secret:gardenio-keycloak-first-admin"},
	}
	if err := r.Reconcile(context.Background(), projects, roles, accounts); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var names []string
	for _, a := range client.Actions() {
		if ca, ok := a.(k8stesting.CreateAction); ok {
			names = append(names, ca.GetObject().(*unstructured.Unstructured).GetName())
		}
	}
	if len(names) != 1 || names[0] != "gardenio-git-credentials" {
		t.Fatalf("created ExternalSecrets %v, want only gardenio-git-credentials", names)
	}
}
