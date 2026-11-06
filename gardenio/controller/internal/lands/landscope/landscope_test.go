package landscope

import (
	"context"
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

func fakeClient(objects ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{GVR: "KeycloakClientScopeList"}, objects...)
}

func applier(c *dynamicfake.FakeDynamicClient) *Applier {
	return &Applier{Client: c, Namespace: "gardenio", Realm: "gardenio-realm"}
}

func get(t *testing.T, c *dynamicfake.FakeDynamicClient, name string) *unstructured.Unstructured {
	t.Helper()
	o, err := c.Resource(GVR).Namespace("gardenio").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get %s: %v", name, err)
	}
	return o
}

func TestName(t *testing.T) {
	if n, err := Name("acme"); err != nil || n != "land-acme" {
		t.Fatalf("Name(acme) = %q, %v", n, err)
	}
	for _, bad := range []string{"", "Acme", "a_b", "-a", strings.Repeat("x", 59)} {
		if _, err := Name(bad); err == nil {
			t.Errorf("Name(%q) = nil error, want one", bad)
		}
	}
}

// The resource is the shape the chart's land scopes have and the admission
// policy allows: one audience mapper for land-<name>, optional, in the realm.
func TestRender(t *testing.T) {
	o, err := Render("acme", "gardenio", "gardenio-realm")
	if err != nil {
		t.Fatal(err)
	}
	if o.GetName() != "gardenio-land-scope-acme" || o.GetNamespace() != "gardenio" {
		t.Errorf("object %s/%s", o.GetNamespace(), o.GetName())
	}
	if l := o.GetLabels(); l[LabelLand] != "acme" || l[LabelScope] != "true" || l[LabelManagedBy] != ManagedBy {
		t.Errorf("labels %v", l)
	}
	for path, want := range map[string]string{
		"spec.name": "land-acme", "spec.type": "optional", "spec.protocol": "openid-connect",
		"spec.realmRef.name": "gardenio-realm", "spec.realmRef.kind": "KeycloakRealm",
		"spec.attributes." + LandAttribute: "acme",
	} {
		if got, _, _ := unstructured.NestedString(o.Object, strings.Split(path, ".")...); got != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
	mappers, _, _ := unstructured.NestedSlice(o.Object, "spec", "protocolMappers")
	if len(mappers) != 1 {
		t.Fatalf("mappers = %v, want one", mappers)
	}
	m := mappers[0].(map[string]any)
	cfg := m["config"].(map[string]any)
	if m["protocolMapper"] != "oidc-audience-mapper" || cfg["included.custom.audience"] != "land-acme" || cfg["access.token.claim"] != "true" {
		t.Errorf("mapper = %v", m)
	}
	if _, err := Render("Bad", "gardenio", "gardenio-realm"); err == nil {
		t.Error("Render(Bad) = nil error")
	}
}

func TestEnsure_CreatesThenIsIdempotent(t *testing.T) {
	c := fakeClient()
	a := applier(c)
	if err := a.Ensure(context.Background(), "acme"); err != nil {
		t.Fatal(err)
	}
	if n, _, _ := unstructured.NestedString(get(t, c, "gardenio-land-scope-acme").Object, "spec", "name"); n != "land-acme" {
		t.Errorf("spec.name = %q", n)
	}
	c.ClearActions()
	if err := a.Ensure(context.Background(), "acme"); err != nil {
		t.Fatal(err)
	}
	for _, act := range c.Actions() {
		if act.GetVerb() != "get" {
			t.Errorf("second Ensure wrote: %s", act.GetVerb())
		}
	}
}

// A resource of ours whose spec drifted is put back (gd-eAfptUBh9Z).
func TestEnsure_RestoresDriftedSpec(t *testing.T) {
	o, _ := Render("acme", "gardenio", "gardenio-realm")
	_ = unstructured.SetNestedField(o.Object, "default", "spec", "type")
	c := fakeClient(o)
	if err := applier(c).Ensure(context.Background(), "acme"); err != nil {
		t.Fatal(err)
	}
	if typ, _, _ := unstructured.NestedString(get(t, c, o.GetName()).Object, "spec", "type"); typ != "optional" {
		t.Errorf("spec.type = %q, want optional", typ)
	}
}

// A resource of the name that the controller did not make for this land is
// never adopted.
func TestEnsure_RefusesForeignResource(t *testing.T) {
	for name, labels := range map[string]map[string]any{
		"unlabelled":       nil,
		"another land":     {LabelLand: "beta", LabelManagedBy: ManagedBy},
		"helm's, not ours": {LabelLand: "acme", LabelManagedBy: "Helm"},
	} {
		t.Run(name, func(t *testing.T) {
			o, _ := Render("acme", "gardenio", "gardenio-realm")
			o.Object["metadata"].(map[string]any)["labels"] = labels
			_ = unstructured.SetNestedField(o.Object, "default", "spec", "type")
			c := fakeClient(o)
			if err := applier(c).Ensure(context.Background(), "acme"); !errors.Is(err, ErrNotOurs) {
				t.Fatalf("err = %v, want ErrNotOurs", err)
			}
			if typ, _, _ := unstructured.NestedString(get(t, c, o.GetName()).Object, "spec", "type"); typ != "default" {
				t.Error("a foreign resource was rewritten")
			}
		})
	}
}

// A refused write (RBAC or the admission policy) is the caller's error.
func TestEnsure_ReturnsAPIError(t *testing.T) {
	c := fakeClient()
	c.PrependReactor("create", "keycloakclientscopes", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("admission denied")
	})
	if err := applier(c).Ensure(context.Background(), "acme"); err == nil || !strings.Contains(err.Error(), "admission denied") {
		t.Fatalf("err = %v", err)
	}
}
