package tests

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Keycloak as code (gd-DAy1iWvO4y, plan gd-P78ygCUgA9 Task 4).
//
// The KeycloakRealm CRD has no brute-force field, so the first-admin hook
// switches detection on with kcadm, gated on
// keycloak.runtime.realmHardening.bruteForce.enabled (default false).

const firstAdminJob = "templates/keycloak-runtime/first-admin-job.yaml"

func TestKeycloakBruteForce_OffByDefault(t *testing.T) {
	t.Parallel()
	out := helmTemplateShowOnlyWithValues(t, firstAdminJob, nil,
		"keycloak.enabled=true", "keycloak.runtime.enabled=true")
	if !strings.Contains(out, "[first-admin] DONE") {
		t.Fatalf("first-admin hook did not render:\n%s", out)
	}
	for _, s := range []string{"bruteForceProtected", "failureFactor", "permanentLockout"} {
		if strings.Contains(out, s) {
			t.Errorf("brute force is off by default: script must not contain %q", s)
		}
	}
}

func TestKeycloakBruteForce_EnabledSetsRealmFlags(t *testing.T) {
	t.Parallel()
	out := helmTemplateShowOnlyWithValues(t, firstAdminJob, nil,
		"keycloak.enabled=true", "keycloak.runtime.enabled=true",
		"keycloak.runtime.realmHardening.bruteForce.enabled=true")
	for _, w := range []string{
		`kcadm update "realms/${KC_REALM}"`,
		"-s bruteForceProtected=true",
		"-s permanentLockout=false",
		"-s failureFactor=10",
		"-s waitIncrementSeconds=60",
		"-s maxFailureWaitSeconds=900",
		"-s maxDeltaTimeSeconds=43200",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("brute force on: script missing %q", w)
		}
	}
	// Ordered after the realm-exists wait, before the group wait, and non-fatal:
	// the hook keeps its exit-0-with-diagnostics behaviour.
	realm := strings.Index(out, "realm ${KC_REALM} present")
	bf := strings.Index(out, "bruteForceProtected=true")
	grp := strings.Index(out, "# 3) Wait for the target group")
	if !(realm >= 0 && realm < bf && bf < grp) {
		t.Errorf("brute-force step must sit between the realm wait and the group wait (realm=%d bf=%d group=%d)", realm, bf, grp)
	}
	if !strings.Contains(out, "WARNING: could not enable brute-force detection") {
		t.Error("a failed brute-force update must warn, not die")
	}
}

// ── k3s overlay: the realm's clients are declared in code ───────────────────

const keycloakClients = "templates/keycloak/clients.yaml"

// clientDoc returns the rendered KeycloakClient document for clientId, or "".
func clientDoc(out, clientId string) string {
	for _, doc := range strings.Split(out, "\n---\n") {
		if strings.Contains(doc, "kind: KeycloakClient\n") &&
			strings.Contains(doc, "clientId: \""+clientId+"\"") {
			return doc
		}
	}
	return ""
}

func k3sClients(t *testing.T) string {
	t.Helper()
	return helmTemplateShowOnlyWithValues(t, keycloakClients, []string{k3sOverlay})
}

func TestKeycloakClients_K3sOverlay_GrafanaMirrorsLiveClient(t *testing.T) {
	t.Parallel()
	doc := clientDoc(k3sClients(t), "grafana")
	if doc == "" {
		t.Fatal("grafana KeycloakClient must render from the overlay")
	}
	for _, w := range []string{
		"public: false",
		"standardFlowEnabled: true",
		"directAccess: false",
		`secret: "$gardenio-grafana-oidc:client-secret"`,
		"pkce.code.challenge.method: S256",
		"post.logout.redirect.uris:",
		"https://grafana.clonk.tv/login/generic_oauth",
		"- https://grafana.clonk.tv\n",
		"claim.name: realm_access.roles",
		// Names and the audience mapper are as live (export 2026-10-14).
		`name: "Grafana (gardenio-k3s, ziti only)"`,
		"name: realm-roles-userinfo",
		"name: grafana-audience",
		"included.client.audience: grafana",
		`post.logout.redirect.uris: https://grafana.clonk.tv/*`,
	} {
		if !strings.Contains(doc, w) {
			t.Errorf("grafana client missing %q:\n%s", w, doc)
		}
	}
	// Ziti is off (gd-xNMLrsgajy): its Grafana name is no redirect target.
	if strings.Contains(doc, "grafana.ziti") {
		t.Errorf("grafana client still names the ziti host:\n%s", doc)
	}
	// Grafana reads realm_access.roles from USERINFO, so the mapper must feed it.
	if !strings.Contains(doc, `userinfo.token.claim: "true"`) {
		t.Error("grafana realm-roles mapper must be ON for userinfo")
	}
}

func TestKeycloakClients_K3sOverlay_ClaimLayerMirrorsLiveClient(t *testing.T) {
	t.Parallel()
	doc := clientDoc(k3sClients(t), "claimlayer-demo")
	if doc == "" {
		t.Fatal("claimlayer-demo KeycloakClient must render from the overlay (the live clientId, not \"claimlayer\")")
	}
	for _, w := range []string{
		"public: false",
		"standardFlowEnabled: true",
		"directAccess: false",
		`secret: "$claimlayer-keycloak-client:client-secret"`,
		"pkce.code.challenge.method: S256",
		"https://claimlayer.clonk.tv/oauth2/callback",
		"- https://claimlayer.clonk.tv\n",
		"name: claimlayer-demo-audience",
		"included.client.audience: claimlayer-demo",
	} {
		if !strings.Contains(doc, w) {
			t.Errorf("claimlayer-demo client missing %q:\n%s", w, doc)
		}
	}
	if clientDoc(k3sClients(t), "claimlayer") != "" {
		t.Error("there is no client named \"claimlayer\"; the live clientId is claimlayer-demo")
	}
}

func TestKeycloakClients_K3sOverlay_NoInlinedClientSecret(t *testing.T) {
	t.Parallel()
	out := k3sClients(t)
	for _, id := range []string{"grafana", "claimlayer-demo"} {
		doc := clientDoc(out, id)
		for _, line := range strings.Split(doc, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "secret:") && !strings.Contains(line, `"$`) {
				t.Errorf("%s: client secret must be a $secret:key reference, got %q", id, line)
			}
		}
	}
}

func TestKeycloakClients_K3sOverlay_ForwardAuthHasEveryPlannedCallback(t *testing.T) {
	t.Parallel()
	doc := clientDoc(k3sClients(t), "gardenio-traefik-forwardauth")
	if doc == "" {
		t.Fatal("forwardauth client must render")
	}
	for _, h := range []string{"gardenio", "grafana", "mux", "dashboard"} {
		for _, w := range []string{
			"https://" + h + ".clonk.tv/oauth2/callback",
			"https://" + h + ".clonk.tv\n",
		} {
			if !strings.Contains(doc, w) {
				t.Errorf("forwardauth client missing %q", w)
			}
		}
	}
	// auth.clonk.tv: the sign-in check in front of the realm admin console
	// (gd-papvgFfwMf). Keycloak is that origin, so it needs no web origin.
	if !strings.Contains(doc, "https://auth.clonk.tv/oauth2/callback") {
		t.Error("forwardauth client missing the auth.clonk.tv callback")
	}
	if strings.Contains(doc, "*") {
		t.Error("callbacks must be exact hosts, never wildcards")
	}
}

// The impersonation banner's Stop (plan gd-0SUIdKXWV3 step 4) ends the session at Keycloak with this client_id and
// post_logout_redirect_uri=https://gardenio.clonk.tv/garden (cmd/beads-viewer defaultStopBecomeURL): the client must allow
// it, and keep every callback "+" allowed before.
func TestKeycloakClients_K3sOverlay_ForwardAuthAllowsStopBecome(t *testing.T) {
	t.Parallel()
	doc := clientDoc(k3sClients(t), "gardenio-traefik-forwardauth")
	var uris []string
	for _, line := range strings.Split(doc, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "post.logout.redirect.uris:"); ok {
			uris = strings.Split(strings.Trim(strings.TrimSpace(v), `"`), "##")
		}
	}
	want := []string{"https://gardenio.clonk.tv/garden"}
	for _, h := range []string{"gardenio", "grafana", "mux", "dashboard", "auth"} {
		want = append(want, "https://"+h+".clonk.tv/oauth2/callback")
	}
	for _, w := range want {
		if !slices.Contains(uris, w) {
			t.Errorf("forwardauth post.logout.redirect.uris %q do not allow %q", uris, w)
		}
	}
}

func TestKeycloakClients_K3sOverlay_CLIClientCarriesGroupsClaim(t *testing.T) {
	t.Parallel()
	doc := clientDoc(k3sClients(t), "gardenio-cli")
	if doc == "" {
		t.Fatal("gardenio-cli client must render")
	}
	// toYaml renders each mapper as "- config: ..." with keys sorted, so pick
	// the list item that holds the group-membership mapper.
	var tail string
	for _, item := range strings.Split(doc, "- config:") {
		if strings.Contains(item, "oidc-group-membership-mapper") {
			tail = item
		}
	}
	if tail == "" {
		t.Fatalf("gardenio-cli needs a group-membership mapper:\n%s", doc)
	}
	for _, w := range []string{"claim.name: groups", `full.path: "true"`, `access.token.claim: "true"`} {
		if !strings.Contains(tail, w) {
			t.Errorf("groups mapper missing %q", w)
		}
	}
}

// Control: only the clients the overlay lists render.
func TestKeycloakClients_K3sOverlay_UnlistedClientDoesNotRender(t *testing.T) {
	t.Parallel()
	out := k3sClients(t)
	for _, id := range []string{"gardenio-mux", "not-a-real-client"} {
		if clientDoc(out, id) != "" {
			t.Errorf("client %q is not in the overlay and must not render", id)
		}
	}
}

func TestKeycloakClients_K3sOverlay_BruteForceOn(t *testing.T) {
	t.Parallel()
	out := helmTemplateShowOnlyWithValues(t, firstAdminJob, []string{k3sOverlay})
	if !strings.Contains(out, "bruteForceProtected=true") {
		t.Error("overlay must turn realm brute-force detection on (needs the chart change in the first-admin hook)")
	}
}

// fullScopeAllowed used to render `{{ .fullScopeAllowed | default true }}`, and
// sprig's default treats false as empty, so an explicit false rendered true.
func TestKeycloakClients_FullScopeAllowedRendersFalse(t *testing.T) {
	t.Parallel()
	render := func(extra string) string {
		return helmTemplateShowOnlyWithValues(t, keycloakClients, nil,
			"keycloak.enabled=true",
			"keycloak.clients[0].clientId=probe", "keycloak.clients[0].name=probe"+extra)
	}
	if doc := clientDoc(render(""), "probe"); !strings.Contains(doc, "fullScopeAllowed: true") {
		t.Errorf("unset must stay true:\n%s", doc)
	}
	if doc := clientDoc(render(",keycloak.clients[0].fullScopeAllowed=false"), "probe"); !strings.Contains(doc, "fullScopeAllowed: false") {
		t.Errorf("an explicit false must render false:\n%s", doc)
	}
}

// ── Keycloak >= 26.5 validation: client session limits vs the realm ─────────

// Keycloak 26.5 rejects a client whose Client Session Idle/Max exceeds the
// realm's SSO Session Idle/Max, and the operator re-PUTs every client on each
// sync, so one oversized client would fail its reconcile for good (design doc
// 12.3, plan gd-QXq1fjYuRD S2b).
func TestKeycloakClients_K3sOverlay_ClientSessionLimitsWithinRealm(t *testing.T) {
	t.Parallel()
	realm := helmTemplateShowOnlyWithValues(t, "templates/keycloak/realm.yaml", []string{k3sOverlay})
	realmVal := func(key string) int {
		m := regexp.MustCompile(`(?m)^\s+` + key + `: (\d+)\s*$`).FindStringSubmatch(realm)
		if m == nil {
			t.Fatalf("realm render has no %s:\n%s", key, realm)
		}
		n, _ := strconv.Atoi(m[1])
		return n
	}
	idle, maxLife := realmVal("idleTimeout"), realmVal("maxLifespan")

	attr := regexp.MustCompile(`(?m)^\s+client\.session\.(idle\.timeout|max\.lifespan): "?(\d+)"?\s*$`)
	seen := 0
	for _, m := range attr.FindAllStringSubmatch(k3sClients(t), -1) {
		seen++
		limit, name := idle, "ssoSessionIdleTimeout"
		if m[1] == "max.lifespan" {
			limit, name = maxLife, "ssoSessionMaxLifespan"
		}
		if n, _ := strconv.Atoi(m[2]); n > limit {
			t.Errorf("client.session.%s=%d exceeds the realm's %s=%d; Keycloak >= 26.5 rejects it", m[1], n, name, limit)
		}
	}
	if seen == 0 {
		t.Error("expected at least the gardenio-cli client.session.* attributes in the render")
	}
}
