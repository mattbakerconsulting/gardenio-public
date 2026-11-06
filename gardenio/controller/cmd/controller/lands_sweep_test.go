package main

import (
	"slices"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/config"
)

// The status reporter sweeps exactly the tenant lands whose project bead names
// their own land-<land> namespace (gd-Vuu1J7y4J0).
func TestTenantLandNamespaces(t *testing.T) {
	cfg := &config.Config{ProjectCache: map[string]config.ProjectCacheEntry{
		"gardenio": {},
		"ops":      {LandKind: beadsapi.LandKindOperator, Namespace: "land-ops"},
		"acme":     {LandKind: "tenant", Namespace: "land-acme"},
		"beta":     {LandKind: "tenant", Namespace: "land-beta"},
		"fresh":    {LandKind: "tenant"},                             // namespace step not run
		"liar":     {LandKind: "tenant", Namespace: "gardenio"},      // names another namespace
		"Bad_Name": {LandKind: "tenant", Namespace: "land-Bad_Name"}, // not a namespace name
	}}
	got := tenantLandNamespaces(cfg)
	if want := []string{"land-acme", "land-beta"}; !slices.Equal(got, want) {
		t.Fatalf("tenantLandNamespaces = %v, want %v", got, want)
	}
}

func TestLandNamespacesEnabled(t *testing.T) {
	for v, want := range map[string]bool{"true": true, " TRUE ": true, "": false, "false": false, "1": false} {
		getenv := func(k string) string {
			if k == landsNamespacesEnabledEnv {
				return v
			}
			return ""
		}
		if got := landNamespacesEnabled(getenv); got != want {
			t.Errorf("landNamespacesEnabled(%q) = %v, want %v", v, got, want)
		}
	}
}
