package tests

import (
	"encoding/json"
	"strings"
	"testing"
)

// Edge Traffic Geomap dashboard — Phase-1 CONSOLE for edge intrusion detection
// (design ka-0000007287#c3, epic ka-0000008216, task ka-0000003019). The Grafana
// face of the internet-facing edge: it orients a human by geography/ASN in ~10s
// when the analyst loop escalates. The map is the CONSOLE, not the product
// (ka-0000007287#c2-3) — it ships ZERO alerts (Phase-1 = charts only); detection
// lives in the L2 rules + L3 analyst loop, never here.
//
// The dashboards/*.json source rides the grafana-bridge image and is provisioned
// at runtime as a type=dashboard bead (ka-0000002799), so these guards read the
// file directly from the dashboards/ source dir (readDashboardJSON) — the same
// bytes baked into the image.
//
// Contract enforced here (VictoriaLogs store, honest-dark + field-contract
// disciplines):
//   - the board embeds as PARSEABLE JSON under its stable uid,
//   - every data panel + every target binds the VictoriaLogs datasource,
//   - the task-listed panels exist by their job (a request-geo geomap, an ASN
//     concentration view, an auth-failure geo view, a campaign timeline, and a
//     novel-geo-for-known-principal table),
//   - the geomap panels locate by the bundled `countries` gazetteer (offline,
//     zero-egress) — never an external tile/basemap fetch,
//   - queries reference the edge log-field contract THROUGH constant template
//     vars (f_country / f_asn / f_sourceip / f_status / f_host / f_principal), so
//     a per-env transport field rename is a variable edit, never a query edit,
//   - the operator can scope by $host + $principal,
//   - queryType values are the plugin's real enum (instant / statsRange),
//   - HONEST-DARK (bug gd-qmQ0WfroZi): only a datasource-less text banner sits at
//     the top level; every VictoriaLogs-bound panel is nested in a COLLAPSED row,
//     so an environment with the datasource off does not render a wall of red
//     datasource-error triangles.

const edgeGeomapUID = "gardenio-edge-traffic-geomap"

type edgeGeomapTarget struct {
	Expr       string          `json:"expr"`
	QueryType  string          `json:"queryType"`
	Datasource json.RawMessage `json:"datasource"`
}

type edgeGeomapPanel struct {
	Type       string             `json:"type"`
	Title      string             `json:"title"`
	Collapsed  bool               `json:"collapsed"` // row rows only
	Datasource json.RawMessage    `json:"datasource"`
	Targets    []edgeGeomapTarget `json:"targets"`
	Options    json.RawMessage    `json:"options"` // geomap layers / text content
	Panels     []edgeGeomapPanel  `json:"panels"`  // nested (collapsed rows)
}

type edgeGeomapDashboard struct {
	UID        string `json:"uid"`
	Title      string `json:"title"`
	Templating struct {
		List []struct {
			Name  string `json:"name"`
			Type  string `json:"type"`
			Query string `json:"query"`
		} `json:"list"`
	} `json:"templating"`
	Panels []edgeGeomapPanel `json:"panels"`
}

func parseEdgeGeomapDashboard(t *testing.T) edgeGeomapDashboard {
	t.Helper()
	raw := readDashboardJSON(t, "edge-traffic-geomap.json")
	var d edgeGeomapDashboard
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		t.Fatalf("dashboards/edge-traffic-geomap.json is not valid JSON: %v", err)
	}
	if d.UID != edgeGeomapUID {
		t.Errorf("edge geomap board must carry its stable uid %q (dashboards deep-link + provision on uid); got %q", edgeGeomapUID, d.UID)
	}
	return d
}

// flattenEdgePanels walks the panel tree (rows carry nested panels).
func flattenEdgePanels(panels []edgeGeomapPanel) []edgeGeomapPanel {
	var out []edgeGeomapPanel
	for _, p := range panels {
		out = append(out, p)
		out = append(out, flattenEdgePanels(p.Panels)...)
	}
	return out
}

// TestEdgeGeomapDashboard_ValidAndBindsVictoriaLogs — the board parses and EVERY
// data panel (and every target) binds the VictoriaLogs datasource. A
// prometheus/jaeger binding here would silently return nothing against the log
// store.
func TestEdgeGeomapDashboard_ValidAndBindsVictoriaLogs(t *testing.T) {
	t.Parallel()
	d := parseEdgeGeomapDashboard(t)
	panels := flattenEdgePanels(d.Panels)

	var dataPanels int
	for _, p := range panels {
		switch p.Type {
		case "row", "text":
			continue
		}
		dataPanels++
		if p.Datasource == nil || !strings.Contains(string(p.Datasource), "victoriametrics-logs-datasource") {
			t.Errorf("panel %q must bind the VictoriaLogs datasource; got datasource %s", p.Title, string(p.Datasource))
		}
		if len(p.Targets) == 0 {
			t.Errorf("panel %q has no targets", p.Title)
		}
		for _, tg := range p.Targets {
			if tg.Datasource == nil || !strings.Contains(string(tg.Datasource), "victoriametrics-logs-datasource") {
				t.Errorf("panel %q target must bind the VictoriaLogs datasource; got %s", p.Title, string(tg.Datasource))
			}
			if strings.TrimSpace(tg.Expr) == "" {
				t.Errorf("panel %q has an empty expr", p.Title)
			}
		}
	}
	if dataPanels < 10 {
		t.Errorf("expected the edge geomap board to carry a substantive panel set; got %d data panels", dataPanels)
	}
}

// TestEdgeGeomapDashboard_HasTaskPanels — the panels the task asks for exist by
// their job (design ka-0000007287#c3 console panel list): a request-geo geomap,
// an ASN-concentration view, an auth-failure geomap, a campaign timeline, and a
// novel-geo-for-known-principal table. Bound to the query shape so a future edit
// that guts one is caught.
func TestEdgeGeomapDashboard_HasTaskPanels(t *testing.T) {
	t.Parallel()
	d := parseEdgeGeomapDashboard(t)
	panels := flattenEdgePanels(d.Panels)

	var geomaps int
	var haveCountryGeo, haveAuthFailGeo, haveASNConcentration, haveCampaignTimeline, haveNovelGeo bool
	for _, p := range panels {
		if p.Type == "geomap" {
			geomaps++
		}
		for _, tg := range p.Targets {
			// request geo distribution: a geomap grouped by country on total requests
			if p.Type == "geomap" && strings.Contains(tg.Expr, "as country | stats by (country) count() as requests") {
				haveCountryGeo = true
			}
			// auth-failure geo: a geomap grouped by country on failures (non-2xx/3xx)
			if p.Type == "geomap" && strings.Contains(tg.Expr, "as country | stats by (country) count() as failures") {
				haveAuthFailGeo = true
			}
			// ASN concentration: grouped by asn
			if strings.Contains(tg.Expr, "as asn | stats by (asn)") {
				haveASNConcentration = true
			}
			// campaign timeline: a statsRange over the whole floor
			if p.Type == "timeseries" && tg.QueryType == "statsRange" {
				haveCampaignTimeline = true
			}
			// novel geo for a known principal: successful auth grouped by (principal, country, asn)
			if strings.Contains(tg.Expr, "stats by (principal, country, asn)") {
				haveNovelGeo = true
			}
		}
	}
	if geomaps < 2 {
		t.Errorf("edge board must carry at least the two geomaps the task lists (request geo + auth-failure geo); got %d geomap panels", geomaps)
	}
	if !haveCountryGeo {
		t.Error("edge board must carry a request-by-country geomap (a geomap with `as country | stats by (country) count() as requests`) — task ka-0000003019")
	}
	if !haveAuthFailGeo {
		t.Error("edge board must carry an auth-failure-by-country geomap (a geomap with `as country | stats by (country) count() as failures`) — task ka-0000003019")
	}
	if !haveASNConcentration {
		t.Error("edge board must carry an ASN-concentration view (a `stats by (asn)` breakdown) — task ka-0000003019")
	}
	if !haveCampaignTimeline {
		t.Error("edge board must carry a campaign timeline (a timeseries with queryType statsRange) — task ka-0000003019")
	}
	if !haveNovelGeo {
		t.Error("edge board must carry a novel-geo-for-known-principal table (a `stats by (principal, country, asn)` breakdown) — task ka-0000003019")
	}
}

// TestEdgeGeomapDashboard_GeomapsAreOffline — the geomap panels locate by the
// BUNDLED `countries` gazetteer and draw the bundled country-outline GeoJSON —
// never an external basemap/tile fetch. This is the design's load-bearing
// constraint (ka-0000007287#c1-7): a mirrored-image, default-deny-egress estate
// cannot reach carto/osm/esri tile servers, so a basemap that tried to would
// render a blank/broken map. Guards that (1) every geomap uses lookup+countries
// gazetteer, and (2) no geomap wires an egress-bound basemap type.
// bundledCountriesGazetteer is the path Grafana ships the countries gazetteer
// at, and the exact value its built-in "Countries" gazetteer picker emits.
const bundledCountriesGazetteer = "public/gazetteer/countries.json"

func TestEdgeGeomapDashboard_GeomapsAreOffline(t *testing.T) {
	t.Parallel()
	d := parseEdgeGeomapDashboard(t)
	// Basemap types that fetch tiles off-cluster — forbidden on this estate.
	egressBasemaps := []string{"osm-standard", "carto", "esri-xyz", "\"xyz\"", "maplibre"}

	var geomaps int
	for _, p := range flattenEdgePanels(d.Panels) {
		if p.Type != "geomap" {
			continue
		}
		geomaps++
		opts := string(p.Options)
		// Grafana resolves `gazetteer` as a URL PATH, not a friendly name — a
		// bare "countries" makes the frontend GET /countries, which 404s, the
		// placename lookup fails and every marker is silently dropped (the map
		// renders blank). The bundled countries gazetteer MUST be referenced by
		// its real path, which is what Grafana's own "Countries" picker emits.
		if !strings.Contains(opts, "\"gazetteer\": \""+bundledCountriesGazetteer+"\"") &&
			!strings.Contains(opts, "\"gazetteer\":\""+bundledCountriesGazetteer+"\"") {
			t.Errorf("geomap %q must locate by the bundled countries gazetteer at path %q (offline, zero-egress) — a bare \"countries\" 404s and renders the map blank; options:\n%s", p.Title, bundledCountriesGazetteer, opts)
		}
		if !strings.Contains(opts, "\"mode\": \"lookup\"") && !strings.Contains(opts, "\"mode\":\"lookup\"") {
			t.Errorf("geomap %q must use location mode `lookup` (country code → centroid via gazetteer); options:\n%s", p.Title, opts)
		}
		for _, bad := range egressBasemaps {
			if strings.Contains(opts, bad) {
				t.Errorf("geomap %q wires an egress-bound basemap (%s) — this estate is default-deny-egress and cannot fetch external tiles (ka-0000007287#c1-7). Use basemap type `default` + the bundled country GeoJSON; options:\n%s", p.Title, bad, opts)
			}
		}
	}
	if geomaps == 0 {
		t.Fatal("edge board must carry geomap panels")
	}
}

// TestEdgeGeomapDashboard_FieldContractViaVars — the edge log-field names are
// referenced THROUGH constant template variables, never hard-coded in the
// queries. A transport that flattens the access log to different field names, or
// an L1 enrichment that names the country field differently, is then a variable
// edit — not a query rewrite. The constant vars' DEFAULTS pin the current
// producer contract (auditxform.FieldSourceASN=sourceASN; the enriched scalar
// client_ip; auditxform.FieldPrincipal=principal, the surface-agnostic principal
// both L0 shippers now stamp — ka-0000005007; the Traefik JSON field names).
func TestEdgeGeomapDashboard_FieldContractViaVars(t *testing.T) {
	t.Parallel()
	d := parseEdgeGeomapDashboard(t)

	wantDefaults := map[string]string{
		"f_country":   "sourceCountry",
		"f_asn":       "sourceASN",
		"f_sourceip":  "client_ip",
		"f_status":    "DownstreamStatus",
		"f_host":      "RequestHost",
		"f_principal": "principal",
	}
	got := map[string]string{}
	for _, v := range d.Templating.List {
		if _, ok := wantDefaults[v.Name]; ok {
			if v.Type != "constant" {
				t.Errorf("field-contract var %q must be a constant (hidden, transport-aligned), got type %q", v.Name, v.Type)
			}
			got[v.Name] = v.Query
		}
	}
	for name, def := range wantDefaults {
		if got[name] != def {
			t.Errorf("field-contract var %q default must match the edge producer contract %q; got %q", name, def, got[name])
		}
	}

	// The queries must not hard-code the raw enriched field names — they must go
	// through the f_* vars so a rename is a variable edit. Pick names that would
	// appear ONLY if a query bypassed the contract.
	for _, p := range flattenEdgePanels(d.Panels) {
		for _, tg := range p.Targets {
			if strings.Contains(tg.Expr, "sourceCountry:") || strings.Contains(tg.Expr, "sourceASN:") {
				t.Errorf("panel %q hard-codes an enriched field name — reference ${f_country}/${f_asn} instead so a transport/enrichment rename is a variable edit (ka-0000003019); expr:\n%s", p.Title, tg.Expr)
			}
		}
	}
}

// TestEdgeGeomapDashboard_ScopedByHostAndPrincipal — the operator scopes the
// board by host + principal (the two dimensions that turn "the ambient floor"
// into "this hostname / this identity"). Both must exist as editable template
// vars, and every data panel query must filter on both so the scope narrows the
// whole board.
func TestEdgeGeomapDashboard_ScopedByHostAndPrincipal(t *testing.T) {
	t.Parallel()
	d := parseEdgeGeomapDashboard(t)

	haveVar := map[string]bool{}
	for _, v := range d.Templating.List {
		haveVar[v.Name] = true
	}
	for _, name := range []string{"host", "principal"} {
		if !haveVar[name] {
			t.Errorf("edge board must expose a %q template variable to scope the board (ka-0000003019)", name)
		}
	}

	for _, p := range flattenEdgePanels(d.Panels) {
		switch p.Type {
		case "row", "text":
			continue
		}
		for _, tg := range p.Targets {
			if !strings.Contains(tg.Expr, "${host}") || !strings.Contains(tg.Expr, "${principal}") {
				t.Errorf("panel %q query must filter on both ${host} and ${principal} so the scope vars narrow the whole board; expr:\n%s", p.Title, tg.Expr)
			}
		}
	}
}

// TestEdgeGeomapDashboard_ValidQueryTypes — the queryType on each target is a
// real plugin enum value (instant / statsRange / stats / hits, or empty for raw
// logs). A typo silently makes the panel hit the wrong endpoint / return nothing.
func TestEdgeGeomapDashboard_ValidQueryTypes(t *testing.T) {
	t.Parallel()
	d := parseEdgeGeomapDashboard(t)
	valid := map[string]bool{"": true, "instant": true, "stats": true, "statsRange": true, "hits": true}
	for _, p := range flattenEdgePanels(d.Panels) {
		for _, tg := range p.Targets {
			if !valid[tg.QueryType] {
				t.Errorf("panel %q has an invalid queryType %q (must be one of instant/stats/statsRange/hits or empty); expr:\n%s", p.Title, tg.QueryType, tg.Expr)
			}
		}
	}
}

// TestEdgeGeomapDashboard_HonestDarkWhenDatasourceOff — the honest-dark contract
// (bug gd-qmQ0WfroZi). The board's every data panel binds the VictoriaLogs
// datasource, which is OFF BY DEFAULT (grafana.victoriaLogsDatasource.enabled=false
// + vlsingle.enabled=false), and the edge log store is empty until L0/L1 land; the
// dashboard JSON nonetheless always rides the grafana-bridge image and is
// provisioned regardless. A Grafana that rendered this board with the datasource
// absent would otherwise show a wall of red "datasource not found" triangles —
// indistinguishable from a broken board.
//
// Grafana does NOT mount or query the child panels of a COLLAPSED row until an
// operator expands it. So every VictoriaLogs-bound panel stays nested inside a
// collapsed row and only the (datasource-less) text banner sits at the top level.
// This locks that in: NO top-level panel may bind the datasource, and every row
// must be collapsed.
func TestEdgeGeomapDashboard_HonestDarkWhenDatasourceOff(t *testing.T) {
	t.Parallel()
	d := parseEdgeGeomapDashboard(t)

	for _, p := range d.Panels {
		switch p.Type {
		case "row":
			if !p.Collapsed {
				t.Errorf("row %q must be collapsed so its VictoriaLogs-bound panels are not mounted/queried when the datasource is off-by-default (no red datasource-error triangles on load) — bug gd-qmQ0WfroZi", p.Title)
			}
		case "text":
			// The always-rendering banner — allowed at top level (no datasource).
		default:
			if p.Datasource != nil && strings.Contains(string(p.Datasource), "victoriametrics-logs-datasource") {
				t.Errorf("panel %q binds the VictoriaLogs datasource at the TOP LEVEL — it would render a red datasource-error triangle when the datasource is off-by-default. Nest it inside a collapsed row instead (bug gd-qmQ0WfroZi)", p.Title)
			}
			for _, tg := range p.Targets {
				if tg.Datasource != nil && strings.Contains(string(tg.Datasource), "victoriametrics-logs-datasource") {
					t.Errorf("top-level panel %q has a target binding the VictoriaLogs datasource — it would fire against a missing datasource on load. Nest it inside a collapsed row (bug gd-qmQ0WfroZi)", p.Title)
				}
			}
		}
	}
}

// TestEdgeGeomapDashboard_HonestDarkBanner — the top-of-board text banner reads
// as intentional-dark: it names the exact enable toggles + the upstream L0/L1
// dependencies and frames the empty state as deliberate, so an operator browsing
// Grafana reads "inert by design" rather than "broken board" (bug gd-qmQ0WfroZi).
// It also states the map-is-console framing so nobody mistakes it for the detector.
func TestEdgeGeomapDashboard_HonestDarkBanner(t *testing.T) {
	t.Parallel()
	d := parseEdgeGeomapDashboard(t)
	if len(d.Panels) == 0 || d.Panels[0].Type != "text" {
		t.Fatalf("edge board must lead with a text banner panel; got %d panels", len(d.Panels))
	}
	var opts struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(d.Panels[0].Options, &opts); err != nil {
		t.Fatalf("banner options not parseable: %v", err)
	}
	content := opts.Content
	for _, want := range []string{
		"grafana.victoriaLogsDatasource.enabled",
		"vlsingle.enabled",
		"ka-0000001563", // L0 transport dependency
		"ka-0000000583", // L1 enrichment dependency
	} {
		if !strings.Contains(content, want) {
			t.Errorf("honest-dark banner must name %q so an operator knows how to light the board up / what it depends on; banner:\n%s", want, content)
		}
	}
	if !strings.Contains(content, "default") {
		t.Errorf("honest-dark banner must frame the board as inert/off by default (the word \"default\"); banner:\n%s", content)
	}
	// The map-is-console framing (design ka-0000007287#c2-3): this is orientation,
	// not detection, and it ships zero alerts.
	if !strings.Contains(strings.ToLower(content), "console") {
		t.Errorf("banner must state the map-is-console framing (design ka-0000007287#c2-3); banner:\n%s", content)
	}
}
