// Package landscope is a land's per-land Keycloak audience (owner decision
// gd-zf6eceweED, task gd-eAfptUBh9Z): the optional client scope land-<name>
// whose one protocol mapper adds the audience land-<name> to an access token.
// A sign-in that requests the scope (kd/gb login against /land/<name>) gets a
// token minted for that land alone; kbeads and landgate narrow such a token to
// the land, and the land door refuses any other.
//
// The provisioner never holds a Keycloak credential that can write client
// scopes or clients (realm-management manage-clients is effectively realm
// admin while kbeads trusts the realm_roles claim). It writes a
// KeycloakClientScope custom resource (keycloakclientscopes.v1.edp.epam.com)
// in the release namespace instead, and the EDP keycloak-operator makes the
// scope. The chart's ValidatingAdmissionPolicy <agents>-land-scopes
// (templates/controller/lands-scope-rbac.yaml) holds every write the
// controller's ServiceAccount makes to those resources to exactly the shape
// Render builds, so the grant cannot add a claim to any token. The chart
// attaches each scope to keycloak.landScopeClients (templates/keycloak/
// clients.yaml looks the resources up on the next helm upgrade).
package landscope

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

const (
	// Prefix prefixes a land's client scope and the audience its mapper adds:
	// land-acme for the land acme.
	Prefix = "land-"
	// ObjectPrefix prefixes the KeycloakClientScope resource's name:
	// gardenio-land-scope-acme for the land acme.
	ObjectPrefix = "gardenio-land-scope-"

	// LabelLand names the land a resource belongs to; LabelScope marks a land
	// scope resource, the label clients.yaml looks them up by.
	LabelLand  = "gardenio.io/land"
	LabelScope = "gardenio.io/land-scope"
	// LabelManagedBy marks what the controller made.
	LabelManagedBy = "app.kubernetes.io/managed-by"
	ManagedBy      = "gardenio-controller"

	// LandAttribute is the scope attribute naming its land.
	LandAttribute = "gardenio_land"
	// MapperName is the name of the one mapper a land scope holds.
	MapperName = "land-audience"
)

// GVR is the EDP keycloak-operator's KeycloakClientScope resource.
var GVR = schema.GroupVersionResource{Group: "v1.edp.epam.com", Version: "v1", Resource: "keycloakclientscopes"}

// landName is the Lands contract's LandName, cut to what fits a Keycloak
// group and a land- scope (keycloakadmin.GroupName and the chart's
// client-scopes.yaml check the same).
var landName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,57}$`)

// ErrNotOurs is returned for a resource of the land scope's name that the
// controller did not make for this land. It is never adopted.
var ErrNotOurs = errors.New("keycloak client scope resource is not this land's")

// Name returns the client scope (and audience) name for a land.
func Name(land string) (string, error) {
	if !landName.MatchString(land) {
		return "", fmt.Errorf("land scope: %q is not a land name", land)
	}
	return Prefix + land, nil
}

// Render builds land's KeycloakClientScope resource in namespace for the
// KeycloakRealm resource realm. Its shape matches the chart's land scopes
// (client-scopes.yaml), and the admission policy allows nothing else.
func Render(land, namespace, realm string) (*unstructured.Unstructured, error) {
	name, err := Name(land)
	if err != nil {
		return nil, err
	}
	o := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": GVR.GroupVersion().String(),
		"kind":       "KeycloakClientScope",
		"metadata": map[string]any{
			"name":      ObjectPrefix + land,
			"namespace": namespace,
			"labels": map[string]any{
				LabelLand: land, LabelScope: "true", LabelManagedBy: ManagedBy,
			},
			// Deleting the resource leaves the scope in Keycloak, as for the
			// chart's own scopes.
			"annotations": map[string]any{"edp.epam.com/preserve-resources-on-deletion": "true"},
		},
		"spec": map[string]any{
			"name":        name,
			"realmRef":    map[string]any{"name": realm, "kind": "KeycloakRealm"},
			"protocol":    "openid-connect",
			"type":        "optional",
			"description": fmt.Sprintf("Mints a token for the land %s alone (aud %s)", land, name),
			"attributes": map[string]any{
				LandAttribute:               land,
				"include.in.token.scope":    "true",
				"display.on.consent.screen": "false",
			},
			"protocolMappers": []any{map[string]any{
				"name":           MapperName,
				"protocol":       "openid-connect",
				"protocolMapper": "oidc-audience-mapper",
				"config": map[string]any{
					"included.custom.audience":  name,
					"access.token.claim":        "true",
					"id.token.claim":            "false",
					"introspection.token.claim": "true",
				},
			}},
		},
	}}
	return o, nil
}

// Applier writes land scope resources through the Kubernetes API.
type Applier struct {
	Client dynamic.Interface
	// Namespace is the release namespace, where the realm resource lives.
	Namespace string
	// Realm is the KeycloakRealm resource the scopes belong to.
	Realm string
}

// Ensure makes sure land's KeycloakClientScope resource exists with the shape
// Render builds, creating it or putting its spec and labels back. A resource
// of that name the controller did not make for land is refused with
// ErrNotOurs and left as it is. It is safe to re-run.
func (a *Applier) Ensure(ctx context.Context, land string) error {
	want, err := Render(land, a.Namespace, a.Realm)
	if err != nil {
		return err
	}
	rc := a.Client.Resource(GVR).Namespace(a.Namespace)
	have, err := rc.Get(ctx, want.GetName(), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if _, err := rc.Create(ctx, want, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create %s: %w", want.GetName(), err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("get %s: %w", want.GetName(), err)
	}
	labels := have.GetLabels()
	if labels[LabelLand] != land || labels[LabelManagedBy] != ManagedBy {
		return fmt.Errorf("%s: %w (labels %s=%q, %s=%q)", want.GetName(), ErrNotOurs, LabelLand, labels[LabelLand], LabelManagedBy, labels[LabelManagedBy])
	}
	wantLabels := want.GetLabels()
	if equality.Semantic.DeepEqual(have.Object["spec"], want.Object["spec"]) && hasAll(labels, wantLabels) {
		return nil
	}
	merged := maps.Clone(labels)
	maps.Copy(merged, wantLabels)
	have.SetLabels(merged)
	have.Object["spec"] = want.Object["spec"]
	if _, err := rc.Update(ctx, have, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update %s: %w", want.GetName(), err)
	}
	return nil
}

func hasAll(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}
