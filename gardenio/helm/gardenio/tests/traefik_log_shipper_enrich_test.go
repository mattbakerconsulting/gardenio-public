package tests

import (
	"strings"
	"testing"
)

// traefik-log-shipper L1 geo/ASN enrichment wiring (edge IDP L1, task
// ka-0000001136, epic ka-0000008216; design ka-0000007287).
//
// The shipper extracts the true public client IP from each Traefik access row's
// X-Forwarded-For (design ka-0000007287#c1-3, ipStrategy.depth:1) and, when an
// operator wires a table, stamps a scalar client_ip + sourceASN (+ sourceCountry
// for a geo table) — the group keys the geomap console (ka-0000003019) and the L2
// novel-geo/ASN detections query. These tests pin:
//   - the default-off invariant (no env/mount/ConfigMap => byte-identical RAW),
//   - the opt-in render of the geo + ASN table mounts + env vars,
//   - the mutual-exclusion {{ fail }} guard (a geo table is a superset of ASN).

// traefikShipperEnabledFlags returns the minimal --set flags that render the
// shipper. Uses release name "gardenio" so the fullname resolves to
// gardenio-traefik-log-shipper (the SA/ConfigMap names the templates reference).
func traefikShipperEnabledFlags(extra ...string) []string {
	base := []string{
		"traefikLogShipper.enabled=true",
	}
	return append(base, extra...)
}

// The tables are off by default: with neither set the shipper runs in its
// byte-identical RAW form — no *_TABLE_PATH env, no mount, no ConfigMap. Keeps
// the geo/ASN dimension inert-until-configured (deploy-raw-first, ka-0000001136).
func TestTraefikShipperEnrichmentDefaultOff(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateGardenio(t, "templates/traefik-log-shipper/deployment.yaml",
		traefikShipperEnabledFlags()...)
	if err != nil {
		t.Fatalf("render: %v\n%s", err, out)
	}
	for _, forbidden := range []string{
		"TRAEFIK_ASN_TABLE_PATH", "TRAEFIK_GEO_TABLE_PATH",
		"name: asn-table", "name: geo-table",
	} {
		if strings.Contains(out, forbidden) {
			t.Errorf("%q must be ABSENT when no table is configured (byte-identical RAW shipping); got:\n%s", forbidden, out)
		}
	}
	// Neither ConfigMap renders.
	for _, tmpl := range []string{
		"templates/traefik-log-shipper/asn-table-configmap.yaml",
		"templates/traefik-log-shipper/geo-table-configmap.yaml",
	} {
		cm, cmErr := helmTemplateAllowFail(t, tmpl, traefikShipperEnabledFlags()...)
		if cmErr == nil && strings.Contains(cm, "kind: ConfigMap") {
			t.Errorf("%s must NOT render when no table is configured; got:\n%s", tmpl, cm)
		}
	}
}

// A geo table points the shipper at the mounted file via TRAEFIK_GEO_TABLE_PATH
// and mounts it read-only, so each row's X-Forwarded-For client IP resolves to
// scalar sourceASN + sourceCountry.
func TestTraefikShipperGeoTableWired(t *testing.T) {
	t.Parallel()
	geoData := "203.0.113.0/24 AS64512 US EXAMPLE-NET"
	out, err := helmTemplateGardenio(t, "templates/traefik-log-shipper/deployment.yaml",
		traefikShipperEnabledFlags("traefikLogShipper.geoTable.data="+geoData)...)
	if err != nil {
		t.Fatalf("render: %v\n%s", err, out)
	}
	wants := map[string]string{
		"name: TRAEFIK_GEO_TABLE_PATH":                      "shipper must be pointed at the mounted geo table",
		"value: /etc/traefik-log-shipper/geo/geo-table.txt": "the path the ConfigMap mounts to",
		"name: geo-table":                                   "the geo-table volume + mount",
		"name: gardenio-traefik-log-shipper-geo-table":      "the ConfigMap the volume references",
	}
	for want, why := range wants {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q (%s); got:\n%s", want, why, out)
		}
	}
	// An ASN env must NOT appear when only the geo table is set.
	if strings.Contains(out, "TRAEFIK_ASN_TABLE_PATH") {
		t.Errorf("TRAEFIK_ASN_TABLE_PATH must be absent when only geoTable.data is set; got:\n%s", out)
	}
	// The geo-table ConfigMap renders and keys the table under geo-table.txt.
	cm, cmErr := helmTemplateGardenio(t, "templates/traefik-log-shipper/geo-table-configmap.yaml",
		traefikShipperEnabledFlags("traefikLogShipper.geoTable.data="+geoData)...)
	if cmErr != nil {
		t.Fatalf("geo ConfigMap render: %v\n%s", cmErr, cm)
	}
	if !strings.Contains(cm, "geo-table.txt:") || !strings.Contains(cm, geoData) {
		t.Errorf("geo ConfigMap must carry the table under geo-table.txt; got:\n%s", cm)
	}
}

// An ASN-only table points the shipper at TRAEFIK_ASN_TABLE_PATH; no
// sourceCountry dimension and no geo env.
func TestTraefikShipperAsnTableWired(t *testing.T) {
	t.Parallel()
	asnData := "203.0.113.0/24 AS64512 EXAMPLE-NET"
	out, err := helmTemplateGardenio(t, "templates/traefik-log-shipper/deployment.yaml",
		traefikShipperEnabledFlags("traefikLogShipper.asnTable.data="+asnData)...)
	if err != nil {
		t.Fatalf("render: %v\n%s", err, out)
	}
	for _, want := range []string{
		"name: TRAEFIK_ASN_TABLE_PATH",
		"value: /etc/traefik-log-shipper/asn/asn-table.txt",
		"name: asn-table",
		"name: gardenio-traefik-log-shipper-asn-table",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q; got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "TRAEFIK_GEO_TABLE_PATH") {
		t.Errorf("TRAEFIK_GEO_TABLE_PATH must be absent when only asnTable.data is set; got:\n%s", out)
	}
}

// Setting BOTH asnTable.data and geoTable.data is a configuration error: a geo
// table already emits sourceASN, so the deployment fails render rather than
// silently picking one.
func TestTraefikShipperBothTablesFailRender(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "templates/traefik-log-shipper/deployment.yaml",
		traefikShipperEnabledFlags(
			"traefikLogShipper.asnTable.data=203.0.113.0/24 AS64512",
			"traefikLogShipper.geoTable.data=203.0.113.0/24 AS64512 US",
		)...)
	if err == nil {
		t.Fatalf("expected render to FAIL when both tables are set; got success:\n%s", out)
	}
	if !strings.Contains(out, "set only ONE of asnTable.data / geoTable.data") {
		t.Errorf("both-set error must name the mutual-exclusion guard; got:\n%s", out)
	}
}

// The FULL global geo table (~1M CIDRs / ~26 MiB) cannot fit a ConfigMap (1 MiB
// etcd cap), so geoTable.image carries it in a data-only image seeded by a
// copy-only initContainer into an emptyDir the shipper reads (edge IDP L1b,
// ka-0000005851). The env path is identical to the ConfigMap case — only the
// carriage differs — so the shipper needs no code change.
func TestTraefikShipperGeoTableImageWired(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateGardenio(t, "templates/traefik-log-shipper/deployment.yaml",
		traefikShipperEnabledFlags("traefikLogShipper.geoTable.image.repository=geo-table")...)
	if err != nil {
		t.Fatalf("render: %v\n%s", err, out)
	}
	wants := map[string]string{
		"initContainers:":                                   "the copy-only seed initContainer must render",
		"name: seed-geo-table":                              "the seed initContainer name",
		"cp -r /geo-table/. /mnt/geo-table/":                "a pure cp (zero off-cluster egress)",
		"name: TRAEFIK_GEO_TABLE_PATH":                      "the shipper must be pointed at the seeded table",
		"value: /etc/traefik-log-shipper/geo/geo-table.txt": "same path as the ConfigMap case (no code change)",
	}
	for want, why := range wants {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q (%s); got:\n%s", want, why, out)
		}
	}
	// The carrier image ref resolves to the gardenio registry + repo (tag inherits
	// appVersion via gardenio.imageRef).
	if !strings.Contains(out, "/geo-table:") {
		t.Errorf("seed initContainer must reference the geo-table carrier image; got:\n%s", out)
	}
	// The full-table path uses an emptyDir seeded by the initContainer, NOT a
	// ConfigMap (which cannot hold >1 MiB).
	if strings.Contains(out, "gardenio-traefik-log-shipper-geo-table") {
		t.Errorf("geoTable.image must NOT render a ConfigMap volume (table exceeds 1 MiB); got:\n%s", out)
	}
	// No ASN env when only the geo image is set.
	if strings.Contains(out, "TRAEFIK_ASN_TABLE_PATH") {
		t.Errorf("TRAEFIK_ASN_TABLE_PATH must be absent when only geoTable.image is set; got:\n%s", out)
	}
}

// geoTable.image is off by default: the shipper runs byte-identical RAW with no
// initContainer, no geo env, and no geo volume.
func TestTraefikShipperGeoTableImageDefaultOff(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateGardenio(t, "templates/traefik-log-shipper/deployment.yaml",
		traefikShipperEnabledFlags()...)
	if err != nil {
		t.Fatalf("render: %v\n%s", err, out)
	}
	for _, forbidden := range []string{"initContainers:", "name: seed-geo-table"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("%q must be ABSENT when no geo table is configured; got:\n%s", forbidden, out)
		}
	}
}

// Setting BOTH geoTable.data and geoTable.image is a configuration error: they
// are two different carriage mechanisms for the same enrichment.
func TestTraefikShipperGeoDataAndImageFailRender(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "templates/traefik-log-shipper/deployment.yaml",
		traefikShipperEnabledFlags(
			"traefikLogShipper.geoTable.data=203.0.113.0/24 AS64512 US",
			"traefikLogShipper.geoTable.image.repository=geo-table",
		)...)
	if err == nil {
		t.Fatalf("expected render to FAIL when both geoTable.data and geoTable.image are set; got:\n%s", out)
	}
	if !strings.Contains(out, "set only ONE of geoTable.data / geoTable.image") {
		t.Errorf("both-set error must name the data/image guard; got:\n%s", out)
	}
}

// Setting BOTH asnTable.data and geoTable.image is a configuration error: a geo
// table already emits sourceASN (it is a superset).
func TestTraefikShipperAsnDataAndGeoImageFailRender(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "templates/traefik-log-shipper/deployment.yaml",
		traefikShipperEnabledFlags(
			"traefikLogShipper.asnTable.data=203.0.113.0/24 AS64512",
			"traefikLogShipper.geoTable.image.repository=geo-table",
		)...)
	if err == nil {
		t.Fatalf("expected render to FAIL when both asnTable.data and geoTable.image are set; got:\n%s", out)
	}
	if !strings.Contains(out, "set only ONE of asnTable.data / geoTable.image") {
		t.Errorf("both-set error must name the asn/image guard; got:\n%s", out)
	}
}
