package tests

import (
	"strings"
	"testing"
)

// Webhooks-edge 404-wall containment-breach alert — the inverse rule
// (task ka-0000001162, epic ka-0000000572, scanner-response block+alert policy).
//
// The public webhooks entrypoint is a containment WALL: only the sanctioned apex
// Host (gardenio.acme.example / prod.gardenio.acme.example) may match a Traefik router; every
// other Host must fall through to no router and be turned away with a bare 404.
// A SINGLE `type: vlogs` rule pages the instant that invariant breaks — a
// default-host router appears (RouterName present on an off-apex request) or the
// wall answers off-apex traffic with a non-404 status.
//
// The rule lives in templates/vmalert-logs/configmap.yaml (shared define
// gardenio.edgeContainment.rules) and rides the SAME dedicated vmalert-logs
// instance + notifier gate as the eks-api LogsQL detections. Off by default; the
// chart is byte-identical until an overlay opts in.

func TestEdgeContainment_DefaultDisabled_NoRuleKey(t *testing.T) {
	t.Parallel()
	// edgeContainment.enabled=false (the default): even with vmalert-logs on and
	// a notifier wired, the containment rule key must NOT render.
	out := helmTemplate(t, "templates/vmalert-logs/configmap.yaml",
		"vmalertLogs.enabled=true",
		"alertmanager.enabled=true",
	)
	if strings.Contains(out, "edge-containment-logsql.yaml") ||
		strings.Contains(out, "EdgeContainmentBreach") {
		t.Errorf("edge-containment rule must NOT render when edgeContainment.enabled=false, got:\n%s", out)
	}
	// With no LogsQL keys active at all, the placeholder must still be present so
	// the Deployment's rules mount always exists.
	if !strings.Contains(out, "placeholder.yaml: |") {
		t.Errorf("empty-but-valid placeholder must render when no LogsQL rules are active, got:\n%s", out)
	}
}

func TestEdgeContainment_EnabledRendersInverseRule(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmalert-logs/configmap.yaml",
		"vmalertLogs.enabled=true",
		"alertmanager.enabled=true",
		"edgeContainment.enabled=true",
	)
	if !strings.Contains(out, "edge-containment-logsql.yaml: |") {
		t.Fatalf("vmalert-logs ConfigMap must carry the edge-containment rule key when enabled + notifier, got:\n%s", out)
	}
	if !strings.Contains(out, "alert: EdgeContainmentBreach") {
		t.Errorf("expected the EdgeContainmentBreach alert, got:\n%s", out)
	}
	// The GROUP must declare type: vlogs so vmalert queries the VictoriaLogs
	// stats API, not vmsingle's PromQL API. type is a group-level field ONLY —
	// vmalert (v1.108.1) rejects a per-rule `type` with "unknown fields in
	// rule: type" and crashloops, which wedges the prod helm --wait (gd-zxhZS96UHl).
	if strings.Count(out, "\n        type: vlogs\n") != 1 { // GROUP field only, never per-rule
		t.Errorf("exactly the group must carry `type: vlogs` (no per-rule type), got:\n%s", out)
	}
	// The expr must be the prototyped inverse rule VERBATIM: negated apex Host
	// predicate, the (router-present OR non-404) breach disjunction, the stats
	// grouping, and the >0 filter. Any drift here silently changes what pages.
	for _, want := range []string{
		`_time:5m entryPointName:webhooks -RequestHost:~"gardenio.example.com" (RouterName:* OR -DownstreamStatus:404)`,
		"| stats by (RequestHost, RouterName, DownstreamStatus) count() as hits",
		"| filter hits:>0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("containment expr must contain %q, got:\n%s", want, out)
		}
	}
}

func TestEdgeContainment_Pages(t *testing.T) {
	t.Parallel()
	// A hard containment invariant with a ~0/week expected rate PAGES: a single
	// fire is a real config regression, not noise.
	out := helmTemplate(t, "templates/vmalert-logs/configmap.yaml",
		"vmalertLogs.enabled=true",
		"alertmanager.enabled=true",
		"edgeContainment.enabled=true",
	)
	idx := strings.Index(out, "alert: EdgeContainmentBreach")
	if idx < 0 {
		t.Fatalf("EdgeContainmentBreach alert missing, got:\n%s", out)
	}
	if !strings.Contains(out[idx:], `severity: "page"`) {
		t.Errorf("EdgeContainmentBreach must default to page severity, got:\n%s", out[idx:])
	}
	if !strings.Contains(out[idx:], "detection: containment-breach") ||
		!strings.Contains(out[idx:], "component: edge") {
		t.Errorf("EdgeContainmentBreach must carry the edge/containment-breach routing labels, got:\n%s", out[idx:])
	}
}

func TestEdgeContainment_FieldsAndApexConfigurable(t *testing.T) {
	t.Parallel()
	// The edge log-field contract + the apex/wall constants are values, aligned
	// with whatever the transport publishes WITHOUT editing LogsQL.
	out := helmTemplate(t, "templates/vmalert-logs/configmap.yaml",
		"vmalertLogs.enabled=true",
		"alertmanager.enabled=true",
		"edgeContainment.enabled=true",
		"edgeContainment.fields.requestHost=req_host",
		"edgeContainment.fields.routerName=router",
		"edgeContainment.fields.downstreamStatus=status",
		"edgeContainment.apexHost=example.test",
		"edgeContainment.wallStatus=418",
		"edgeContainment.thresholds.breach=3",
	)
	if !strings.Contains(out, `_time:5m entryPointName:webhooks -req_host:~"example.test" (router:* OR -status:418)`) {
		t.Errorf("field/apex/wall overrides must flow into the expr predicate, got:\n%s", out)
	}
	if !strings.Contains(out, "| stats by (req_host, router, status) count() as hits") {
		t.Errorf("field overrides must flow into the stats grouping, got:\n%s", out)
	}
	if !strings.Contains(out, "| filter hits:>3") {
		t.Errorf("breach threshold override not applied, got:\n%s", out)
	}
}

func TestEdgeContainment_RuleKeyOmittedWithoutNotifier(t *testing.T) {
	t.Parallel()
	// Alerting rules with no notifier crashloop vmalert; the containment rule key
	// MUST be gated off (placeholder only) when enabled but no notifier is wired.
	out := helmTemplate(t, "templates/vmalert-logs/configmap.yaml",
		"vmalertLogs.enabled=true",
		"alertmanager.enabled=false",
		"vmalertLogs.notifierURL=",
		"vmalertLogs.notifierBlackhole=false",
		"edgeContainment.enabled=true",
	)
	if strings.Contains(out, "edge-containment-logsql.yaml") {
		t.Errorf("containment rule key must be omitted when no notifier is available, got:\n%s", out)
	}
	if !strings.Contains(out, "placeholder.yaml: |") {
		t.Errorf("placeholder must render when enabled but no notifier, got:\n%s", out)
	}
}

func TestEdgeContainment_CatchAllOff_ExprUnchanged(t *testing.T) {
	t.Parallel()
	// COUPLING GUARD (task ka-0000007813). When the webhooks catch-all is OFF
	// (default) the breach expr must be byte-identical to the prototyped rule —
	// no exclusion clause leaks in. This is what keeps the chart byte-identical
	// until the catch-all overlay opts in.
	out := helmTemplate(t, "templates/vmalert-logs/configmap.yaml",
		"vmalertLogs.enabled=true",
		"alertmanager.enabled=true",
		"edgeContainment.enabled=true",
	)
	if !strings.Contains(out, `_time:5m entryPointName:webhooks -RequestHost:~"gardenio.example.com" (RouterName:* OR -DownstreamStatus:404)`) {
		t.Errorf("with the catch-all off the breach expr must be the verbatim prototyped rule, got:\n%s", out)
	}
	if strings.Contains(out, "webhooks-catchall") {
		t.Errorf("no catch-all exclusion may appear when webhooksEdgeCatchall.enabled=false, got:\n%s", out)
	}
}

func TestEdgeContainment_CatchAllOn_ExcludesSanctionedRouter(t *testing.T) {
	t.Parallel()
	// When the catch-all is LIVE it sets RouterName + returns a non-404 on every
	// off-apex request; the breach rule must subtract the sanctioned catch-all
	// router so its own close does not page on every scanner probe. The rule
	// still fires for any OTHER off-apex router or a non-404 status.
	out := helmTemplate(t, "templates/vmalert-logs/configmap.yaml",
		"vmalertLogs.enabled=true",
		"alertmanager.enabled=true",
		"edgeContainment.enabled=true",
		"webhooksEdgeCatchall.enabled=true",
	)
	if !strings.Contains(out, `_time:5m entryPointName:webhooks -RequestHost:~"gardenio.example.com" -RouterName:~"webhooks-catchall" (RouterName:* OR -DownstreamStatus:404)`) {
		t.Errorf("with the catch-all on the breach expr must exclude the sanctioned catch-all router, got:\n%s", out)
	}
	// The stats grouping + threshold must be unchanged: only the predicate gains
	// the exclusion.
	if !strings.Contains(out, "| stats by (RequestHost, RouterName, DownstreamStatus) count() as hits") {
		t.Errorf("stats grouping must be unchanged by the catch-all coupling, got:\n%s", out)
	}
}

func TestEdgeContainment_CatchAllRouterMatchConfigurable(t *testing.T) {
	t.Parallel()
	// The router substring subtracted from the breach rule is a value, kept in
	// lockstep with the IngressRoute name in templates/webhooks-catchall.
	out := helmTemplate(t, "templates/vmalert-logs/configmap.yaml",
		"vmalertLogs.enabled=true",
		"alertmanager.enabled=true",
		"edgeContainment.enabled=true",
		"webhooksEdgeCatchall.enabled=true",
		"edgeContainment.catchAll.routerMatch=my-catchall",
		"edgeContainment.fields.routerName=router",
	)
	if !strings.Contains(out, `-router:~"my-catchall"`) {
		t.Errorf("catchAll.routerMatch + routerName overrides must flow into the exclusion clause, got:\n%s", out)
	}
}

func TestEdgeContainment_ExprIsAnchoredToTheTraefikEntrypoint(t *testing.T) {
	t.Parallel()
	// REGRESSION GUARD (bug ka-0000004436). The breach expr MUST carry a POSITIVE
	// entrypoint anchor. Without one the rule is not scoped to the Traefik edge at
	// all: a negated LogsQL predicate also matches rows where the field is ABSENT,
	// and the AVA OCSF stream in the same vlsingle store has NO RequestHost, NO
	// RouterName and NO DownstreamStatus. Every AVA row therefore satisfied both
	// halves of the breach disjunction (`-RequestHost:~apex` AND
	// `-DownstreamStatus:404`), so this page-severity rule counted ~7.5k rows/day
	// of routine console auth traffic as a containment breach while the wall was
	// provably intact — measured 5,155-9,289 AVA rows/day over 2026-08-18..08-24
	// against 0 genuinely-breaching webhooks rows in the same windows.
	//
	// The prior doc comment asserted the negated apex predicate "excludes the AVA
	// apex by construction", which is why review could not catch this: the claim
	// was plausible and the citation closed-loop. Only the data disproved it.
	out := helmTemplate(t, "templates/vmalert-logs/configmap.yaml",
		"vmalertLogs.enabled=true",
		"alertmanager.enabled=true",
		"edgeContainment.enabled=true",
	)
	idx := strings.Index(out, "alert: EdgeContainmentBreach")
	if idx < 0 {
		t.Fatalf("EdgeContainmentBreach alert missing, got:\n%s", out)
	}
	expr := out[idx:]
	if !strings.Contains(expr, "_time:5m entryPointName:webhooks -") {
		t.Errorf("breach expr MUST anchor on entryPointName:webhooks immediately after the window; "+
			"a negation-only predicate also matches rows where the field is absent and pulls in the "+
			"whole AVA OCSF stream (bug ka-0000004436), got:\n%s", expr)
	}
	// The anchor must precede the negated apex predicate: LogsQL evaluates the
	// filter chain left-to-right, and leading with the stream selector is what
	// keeps the scan cheap as well as correct.
	anchor := strings.Index(expr, "entryPointName:webhooks")
	negApex := strings.Index(expr, `-RequestHost:~"gardenio.example.com"`)
	if anchor < 0 || negApex < 0 || anchor > negApex {
		t.Errorf("entrypoint anchor must precede the negated apex predicate, got:\n%s", expr)
	}
}

func TestEdgeContainment_EntrypointScopeConfigurable(t *testing.T) {
	t.Parallel()
	// The anchor is a value so the rule can follow the transport's entrypoint
	// vocabulary without editing LogsQL — same contract as the field names above.
	out := helmTemplate(t, "templates/vmalert-logs/configmap.yaml",
		"vmalertLogs.enabled=true",
		"alertmanager.enabled=true",
		"edgeContainment.enabled=true",
		"edgeContainment.entryPointField=ep",
		"edgeContainment.entryPoint=public",
	)
	if !strings.Contains(out, "_time:5m ep:public -") {
		t.Errorf("entryPointField/entryPoint overrides must flow into the expr anchor, got:\n%s", out)
	}
}
