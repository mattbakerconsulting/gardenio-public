package tests

import (
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"sigs.k8s.io/yaml"
)

// keycloakDescriptionMax is the longest description Keycloak stores for a
// realm role, group or client (the column is varchar(255)). The 2026.288.3 roll
// failed because gardenio-machine-trigger-ops carried a 330-char description:
// the Keycloak operator reported the role as failed and the whole release
// stalled behind it (gd-cr3BJRmyTr). Nothing in helm lint or template catches
// that, so this test does.
const keycloakDescriptionMax = 255

var keycloakDescriptionTemplates = []string{
	"templates/keycloak/roles.yaml",
	"templates/keycloak/groups.yaml",
	"templates/keycloak/clients.yaml",
	"templates/keycloak/client-scopes.yaml",
}

// overlongDescriptions returns "<kind>/<metadata.name>: N chars" for every
// document in rendered whose spec.description exceeds keycloakDescriptionMax.
func overlongDescriptions(t *testing.T, rendered string) (problems []string, checked int) {
	t.Helper()
	for _, doc := range strings.Split(rendered, "\n---") {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		var obj struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Description string `json:"description"`
			} `json:"spec"`
		}
		if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
			t.Fatalf("unparseable rendered document: %v\n%s", err, doc)
		}
		if obj.Kind == "" {
			continue
		}
		if obj.Spec.Description != "" {
			checked++
		}
		if n := utf8.RuneCountInString(obj.Spec.Description); n > keycloakDescriptionMax {
			problems = append(problems, obj.Kind+"/"+obj.Metadata.Name+": "+strconv.Itoa(n)+" chars")
		}
	}
	return problems, checked
}

// Every Keycloak realm role, group, client and client-scope description the
// chart renders must fit Keycloak's 255-char limit, for the defaults and for
// the k3s deploy overlay (which can add roles of its own).
func TestKeycloakDescriptionsFitKeycloakLimit(t *testing.T) {
	t.Parallel()
	overlays := map[string][]string{
		"defaults": nil,
		"k3s":      {"../../../deploy-values/gardenio-k3s.yaml"},
	}
	for name, files := range overlays {
		t.Run(name, func(t *testing.T) {
			var total int
			for _, tpl := range keycloakDescriptionTemplates {
				args := []string{"template", "test", chartDir(t), "--show-only", tpl}
				for _, f := range files {
					args = append(args, "-f", chartDir(t)+"/"+f)
				}
				out, err := exec.Command("helm", args...).CombinedOutput()
				if err != nil {
					// An unrendered template (disabled in this overlay) is not a failure.
					if strings.Contains(string(out), "could not find template") {
						continue
					}
					t.Fatalf("helm template %s failed: %v\n%s", tpl, err, out)
				}
				problems, checked := overlongDescriptions(t, string(out))
				total += checked
				for _, p := range problems {
					t.Errorf("%s: %s exceeds Keycloak's %d-char description limit; shorten it", tpl, p, keycloakDescriptionMax)
				}
			}
			if total == 0 {
				t.Fatal("no descriptions were checked: the test is not exercising the chart")
			}
		})
	}
}

// The checker itself: a 330-char description (the 2026.288.3 failure) is caught
// and a 255-char one is not.
func TestOverlongDescriptions_CatchesTooLong(t *testing.T) {
	t.Parallel()
	doc := func(n int) string {
		return "kind: KeycloakRealmRole\nmetadata:\n  name: r\nspec:\n  description: \"" + strings.Repeat("x", n) + "\"\n"
	}
	if p, _ := overlongDescriptions(t, doc(330)); len(p) != 1 {
		t.Errorf("330-char description must be flagged, got %v", p)
	}
	if p, c := overlongDescriptions(t, doc(255)); len(p) != 0 || c != 1 {
		t.Errorf("255-char description must pass, got problems=%v checked=%d", p, c)
	}
	if p, _ := overlongDescriptions(t, doc(256)); len(p) != 1 {
		t.Errorf("256-char description must be flagged, got %v", p)
	}
}
