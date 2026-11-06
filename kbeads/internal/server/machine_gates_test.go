package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

// The machine-gates table (design §4.1, gd-gQ4iLIpWCm): every non-test site that
// compares the auth method or the identity type, with what a machine principal
// does there. authMethodMachine != authMethodOIDC and Type "machine" matches none
// of "oidc" / "static" / "fleet", so most gates fall to the static/fleet outcome
// by construction; this table makes that a reviewed statement, and
// TestMachineGates_TableCoversEverySite fails when a new gate lands without a row,
// the way TestRPCMinRole_CoversEveryDescriptorMethod does for new RPCs.

// Machine outcomes.
const (
	// gateStatic: the gate keys on "is this an OIDC human"; a machine takes the
	// same non-OIDC branch the static token takes.
	gateStatic = "same-as-static"
	// gateFleet: the gate has a static-only branch (agent subject, primary
	// token) or a fleet-only branch; a machine misses it and is treated as an
	// ordinary unprivileged fleet token.
	gateFleet = "same-as-fleet"
	// gateWiring: auth wiring or edge-auth plumbing; a machine is stamped or
	// diverted before it reaches the line, and the row says how.
	gateWiring = "auth-wiring"
)

type machineGateRow struct {
	file    string
	text    string // trimmed line text
	count   int    // occurrences of (file, text)
	outcome string
	note    string
}

var machineGateTable = []machineGateRow{
	{"authz_denials_metrics.go", `if id.Type == "fleet" && isAgentFleet(FleetFromContext(ctx)) {`, 1, gateWiring, "authzPrincipalKind: metric label only, no authorization; a machine is labelled machine:<clientId> one branch earlier and never reaches the agent test (gd-Hk70sS1XuS)"},
	{"agent_identity_authz.go", `case authMethodOIDC:`, 1, gateStatic, "agent identity fields (service_account, token) denied like static; only an OIDC admin, edge, or approved replay passes"},
	{"channel_binding_authz.go", `case authMethodOIDC:`, 1, gateStatic, "channel Slack binding (slack_channel_id, mirror) denied like static; only an OIDC admin, edge, or approved replay passes (gd-ecFKkjtdvS)"},
	{"commons_origin.go", `if authMethodFromContext(ctx) != authMethodOIDC {`, 1, gateStatic, "origin=commons message create/patch refused like static: only an OIDC human writes a person's words (gd-Wu4U4hZbY0)"},
	{"impersonation.go", `if authMethodFromContext(ctx) != authMethodOIDC {`, 1, gateStatic, "impersonatedByFromContext: only a verified OIDC session can be impersonated; a machine, like static, stamps no impersonated_by and writes no session audit (gd-L8LeYDLACc)"},
	{"commons_origin.go", `if id.Type != "oidc" {`, 1, gateStatic, "commonsHumanSelf: a v2 edge envelope (authMethodOIDC, Type edge) is not the person either; a machine is refused one line earlier (gd-74wnmpQgyT)"},
	{"message_author.go", `case authMethodNone, authMethodOIDC, authMethodMachine:`, 1, gateWiring, "messageAuthorRelay: the authMethodMachine arm lets a machine principal (the controller's purse notices) name another principal as a message's author, as a bridge may; static and fleet are held to their own subject (gd-PjNjRhAHPn)"},
	{"message_author.go", `if id.Type == "fleet" && isBridgeFleetIdentity(FleetFromContext(ctx), id.Subject) {`, 1, gateFleet, "messageAuthorRelay: only a bridge fleet names another author; a machine was admitted one arm earlier (gd-PjNjRhAHPn)"},
	{"land_switch_authz.go", `case authMethodOIDC:`, 1, gateStatic, "Lands kill switches (land_frozen*, land_kind, project status/rename, lands-pause gate close/rename/delete) denied like static; only an OIDC admin, edge, or approved replay passes (gd-QVP0Q6tzYB); the one machine exception is machine:gardenio-machine-lands-provisioner opening (land_kind=tenant on create) and closing (land_frozen=true, close of a frozen tenant project) a land (landsProvisioner, gd-t92sawEzFJ)"},
	{"land_frozen.go", `if !isScopedPrincipal(ctx) || authMethodFromContext(ctx) != authMethodOIDC || bead.Type != typeMessage {`, 1, gateStatic, "frozenSupportPost: a machine is not a scoped principal and never frozen, so it gets no #support exemption and needs none; a land principal gets none either (gd-mJw9pxK2wu)"},
	{"config_key_authz.go", `case authMethodOIDC, authMethodNone:`, 1, gateStatic, "SetConfig key gate: sensitive keys and non-allowlisted type:* denied like static; only an OIDC admin or auth-disabled caller passes"},
	{"releases.go", `case roleBearing(method) && hasMinimumRole(RolesFromContext(ctx), RoleAdmin):`, 1, gateStatic, "releaseWriterDenial: a machine is not roleBearing and misses the admin arm; it writes a release only through its own grants arm (machineGrantsWrite on type release) one line later, like any admin-gated type"},
	{"spawn_lineage_seal.go", `return authMethodFromContext(ctx) == authMethodOIDC && hasMinimumRole(RolesFromContext(ctx), RoleAdmin)`, 1, gateStatic, "isOIDCAdminSession: only an OIDC gardenio-admin session can seal a trigger/schedule to itself; a machine writes it unsealed and its spawns never get trigger lineage (TestSpawnLineage_SealIsServerOwned)"},
	{"agent_identity.go", `if id.Type != "fleet" {`, 1, gateFleet, "not a per-agent-token identity: agentBeadIDFromContext misses a machine, so no agent bead id for taint context/grants, spawn-chain walk, operated_by walk or ownership role bypass"},
	{"agent_identity.go", `if id.Type != "fleet" || id.Subject == "" || !isAgentFleet(FleetFromContext(ctx)) {`, 1, gateFleet, "not a per-agent-token identity: no requesting_agent stamp, no agent-bead operated_by fallback; a machine client named agent-<x> is still Type machine"},
	{"edge_auth.go", `if method == authMethodOIDC && !legacyEdgeReadAllowed(r) && !roleExempt && !hasMinimumRole(roles, RoleOperator) {`, 1, gateWiring, "edge envelope only; a machine never arrives with one"},
	{"edge_auth.go", `method = authMethodOIDC`, 2, gateWiring, "edge envelope only"},
	{"land_audience.go", `if authMethodFromContext(ctx) != authMethodOIDC {`, 2, gateStatic, "tokenLandsFromContext: a machine has no land audience, so it is no land token; landDoorDenial: a machine on the land door is refused 403, like static (the edge never forwards one: the door admits only a land bearer)"},
	{"land_audience.go", `if land, ok := landPrincipalFromContext(ctx); ok && authMethodFromContext(ctx) == authMethodLand {`, 1, gateStatic, "landDoorDenial: only a land principal of the door's land passes this arm; a machine is not one and falls to the 403 below"},
	{"land_door_presence.go", `if land, ok := landPrincipalFromContext(ctx); ok && authMethodFromContext(ctx) == authMethodLand {`, 1, gateStatic, "rosterLand: only a land principal (or a door caller) is held to one land's roster; a machine is neither and sees the fleet roster, like static (TestMachineGates_RosterLand_SameAsStatic)"},
	{"land_principal.go", `ctx = withAuthMethod(ctx, authMethodLand)`, 1, gateWiring, "land principal only: a land-* bridge token is stamped authMethodLand; a machine is a JWT, never a registered bridge token, so it never reaches it"},
	{"lock_bead.go", `if authMethodFromContext(ctx) == authMethodOIDC {`, 1, gateStatic, "lockTakeRefusal: only a signed-in OIDC human may take or clear a lock a live agent holds (break-glass); a machine is refused with lockHeldError, like static (gd-SlLPKtUGWZ)"},
	{"land_principal.go", `const authMethodLand authMethod = "land"`, 1, gateWiring, "constant declaration"},
	{"land_principal.go", `func roleBearing(method authMethod) bool {`, 1, gateWiring, "roleBearing declaration: false for a machine, so every !roleBearing site behaves for it as != authMethodOIDC did"},
	{"land_principal.go", `return method == authMethodOIDC || method == authMethodLand || method == authMethodWindow`, 1, gateStatic, "roleBearing: a machine is none of them, the same non-OIDC answer the static token gets"},
	{"lands_agents.go", `case authMethodOIDC:`, 2, gateStatic, "landAgentMayStart, landAgentMayRevoke: a machine falls to the default branch and may not mint, refresh for or revoke a member's own-machine agent, like static (TestLandAgents_ServiceTokensNeverMint)"},
	{"window_routes.go", `case authMethodOIDC:`, 1, gateStatic, "windowManageDenial: a machine falls to the default branch and may not mint, list or revoke a window, like static (TestWindow_MintAuthorization)"},
	{"window_routes.go", `if isScopedPrincipal(ctx) && authMethodFromContext(ctx) == authMethodOIDC {`, 1, gateStatic, "windowCallerDenial: a machine is not a scoped OIDC owner and falls to windowManageDenial's refusal"},
	{"escalation_result_gate.go", `if authMethodFromContext(ctx) == authMethodOIDC && groupsToLandRoles(GroupsFromContext(ctx))[lane.Name] == landRoleOwner {`, 1, gateStatic, "callerHasLaneAuthority land-owner arm: a machine is not authMethodOIDC and has no land-owner authority for a result (gd-JvbxHtR6bx)"},
	{"escalation_approval_authz.go", `case authMethodOIDC:`, 1, gateStatic, "callerMayApproveAsAdmin: a machine falls to the default branch and is not an admin approver (and escalationApprover refuses it before this runs)"},
	{"escalation_audience_authz.go", `case authMethodOIDC:`, 1, gateStatic, "callerMayRouteEscalation: a machine principal falls to the default branch and may not re-route an escalation's audience, like static (TestMachineGates_EscalationRoute_SameAsStatic; gd-bmOiteQi36)"},
	{"escalation_approval_authz.go", `if authMethodFromContext(ctx) == authMethodOIDC && hasMinimumRole(RolesFromContext(ctx), RoleOperator) {`, 1, gateWiring, "escalationApprover edge arm: only a verified v2 envelope (authMethodOIDC, operator role) approves; a machine is Type machine and never reaches the edge arm (gd-dMof1PW1oJ)"},
	{"caller_class.go", `case method == authMethodOIDC && (id.Type == "oidc" || id.Type == "edge") && id.Subject != "" &&`, 1, gateWiring, "callerClassFromContext operator arm: a machine is authMethodMachine, never authMethodOIDC, and takes the explicit classMachine arm (admitted on escalation result, refused on reject; TestCallerClassFromContext)"},
	{"caller_class.go", `case id.Type == "fleet" && isBridgeFleetIdentity(FleetFromContext(ctx), id.Subject):`, 1, gateWiring, "callerClassFromContext bridge arm: the classMachine arm precedes it, so a machine is never a bridge here"},
	{"http_admin_operator_aliases_link.go", `case authMethodOIDC, authMethodNone:`, 1, gateStatic, "not an admin caller: restricted machine-token path"},
	{"http_escalations.go", `if cid := AuthIdentityFromContext(r.Context()); cid.Type == "oidc" {`, 2, gateStatic, "approve and reject refuse a non-page OIDC token only (presenceRefusal, gd-yRGb2j1s56); a machine skips both: it approves solely through a coordinator lane (checkMayApprove, D4: listed in approvers, client in BEADS_ESCALATION_COORDINATOR_CLIENTS, not its own request, not operator-only) and its reject is attributed to its principal by escalationRejector"},
	{"http_devices.go", `case authMethodOIDC:`, 1, gateWiring, "pushDeviceScope: the authMethodMachine arm admits only machine:gardenio-push-bridge to every device; any other machine is refused, like static and fleet (TestPushDevices_NonOIDCRefusedExceptPushBridge)"},
	{"http_devices.go", `if authMethodFromContext(ctx) != authMethodOIDC || id.Type != "oidc" {`, 1, gateStatic, "pushDeviceCallerEmail: no OIDC human, so device registration is refused, like static"},
	{"http_hooks.go", `if !roleBearing(authMethodFromContext(ctx)) {`, 2, gateStatic, "service-token branch, hook execute allowed"},
	{"http_identity_link_slack.go", `if authMethodFromContext(r.Context()) != authMethodOIDC || id.Type != "oidc" || id.Subject == "" {`, 2, gateStatic, "slack identity link/unlink refused: only the person's own OIDC token binds their user bead"},
	{"http_user_terms.go", `if authMethodFromContext(r.Context()) != authMethodOIDC || id.Type != "oidc" || id.Subject == "" {`, 2, gateStatic, "terms acceptance and its read refused: only the person's own OIDC token records or reads it on their user bead (TestAcceptTerms_Refusals and TestMyTerms_RefusesNonOIDC drive the machine case)"},
	{"notification.go", `if authMethodFromContext(r.Context()) == authMethodOIDC && len(RolesFromContext(r.Context())) == 0 && len(GroupsFromContext(r.Context())) == 0 {`, 1, gateStatic, "land members: only a roleless OIDC applicant is refused here; a machine is not one and reaches the land check like static (TestLandMembers_OnlyTheLandsPeople)"},
	{"lands_api.go", `return authMethodFromContext(ctx) == authMethodOIDC && len(RolesFromContext(ctx)) == 0 && len(GroupsFromContext(ctx)) == 0`, 1, gateStatic, "Lands map: only a roleless OIDC applicant is refused every map route; a machine is not one and reaches the land check and the daemon routes' own gates like static (TestApplicant_ForbiddenOnEveryBeadRoute)"},
	{"lands_api.go", `case authMethodFromContext(ctx) == authMethodLand:`, 1, gateStatic, "landsForMap.Caller: only a land principal is labelled agent on the map's request counter; a machine is not one, nor a scoped principal, so it is an operator like static (TestLandsForMap_Caller); a label only, it gates nothing (gd-AQQaiE8IMd)"},
	{"http_welcome_tour.go", `if authMethodFromContext(r.Context()) != authMethodOIDC || id.Type != "oidc" || id.Subject == "" {`, 1, gateStatic, "welcome tour state refused: only the person's own OIDC token reads or stamps welcome_tour_seen_at on their user bead (TestMyWelcomeTour_Refusals drives the static, agent, machine and land cases, GET and POST)"},
	{"http_read_cursors.go", `if authMethodFromContext(r.Context()) != authMethodOIDC || id.Type != "oidc" || id.Subject == "" {`, 1, gateStatic, "read cursors refused: only the person's own OIDC token reads or moves them on their user bead (TestReadCursors_RefusesNonOIDCAndBadIDs drives the machine and land cases)"},
	{"user_standing_consent.go", `if authMethodFromContext(r.Context()) != authMethodOIDC || id.Type != "oidc" || id.Subject == "" {`, 1, gateStatic, "standing consent to push refused: only the person's own OIDC token reads or sets it on their user bead (TestMyConsentPush_Refusals drives the static, agent and machine cases)"},
	{"http_operator_link.go", `if id.Type != "oidc" || id.Subject == "" {`, 1, gateStatic, "identity link refused"},
	{"http_operator_link.go", `return authMethodFromContext(ctx) == authMethodOIDC && hasMinimumRole(RolesFromContext(ctx), RoleAdmin)`, 1, gateStatic, "not an OIDC admin"},
	{"http_scrub_requests.go", `if id.Type == "oidc" && id.Subject != "" {`, 1, gateStatic, "not a scrub approver"},
	{"http_taint_grants.go", `case id.Type == "fleet" && id.Subject != "":`, 1, gateFleet, "no fleet grant lookup under machine:<id>; no operator alias"},
	{"relay_trust.go", `return id.Type == "fleet" && !isAgentFleet(fleet) && strings.HasSuffix(fleet, bridgeFleetSuffix) &&`, 1, gateFleet, "relayTrusted: a machine is not a bridge fleet, so its X-Operated-By (never honored for a machine anyway) resolves no team, grant or decision responder (P7 Task 3)"},
	{"http_taint_grants.go", `if authMethodFromContext(ctx) != authMethodOIDC {`, 1, gateStatic, "may not manage taint policy"},
	{"http_taints.go", `return authMethodFromContext(ctx) == authMethodOIDC && hasMinimumRole(RolesFromContext(ctx), RoleAdmin)`, 1, gateStatic, "may not create taints"},
	{"http_undo.go", `if !roleBearing(authMethodFromContext(r.Context())) {`, 1, gateStatic, "undo admin guard passes the non-OIDC branch like static"},
	{"http_fleet_tokens.go", `case authMethodOIDC:`, 1, gateStatic, "not an embeddings admin: the machine falls to the false default"},
	{"interceptors.go", `authMethodOIDC   authMethod = "oidc"`, 1, gateWiring, "constant declaration"},
	{"interceptors.go", `ctx := withAuthMethod(r.Context(), authMethodOIDC)`, 2, gateWiring, "OIDC branch of AuthMiddleware; the machine classifier runs first"},
	{"interceptors.go", `ctx = withAuthMethod(ctx, authMethodOIDC)`, 2, gateWiring, "OIDC branch of AuthInterceptor; the machine classifier runs first (PermissionDenied)"},
	{"interceptors.go", `if id.Type == "fleet" {`, 2, gateWiring, "fleet header honoring; the machine branch returns before it, so no header is honored"},
	{"interceptors.go", `if id.Type == "fleet" && isAgentFleet(FleetFromContext(ctx)) {`, 2, gateWiring, "per-agent-token header honoring (X-Trigger-Bead, X-No-Auto-Parent); the machine branch returns before it, so no header is honored"},
	{"interceptors.go", `if !roleBearing(method) {`, 4, gateStatic, "requireRole / RBAC interceptors: bypass below admin, deny at admin; requireRoleMachineAllowed passes"},
	{"read_authz.go", `if !roleBearing(authMethodFromContext(ctx)) {`, 1, gateStatic, "unrestricted reads"},
	{"runners.go", `case roleBearing(method):`, 1, gateFleet, "callerMayRevokeRunner: a machine is not a person, so not an owner or admin, and is neither the static token nor a control-mail fleet: refused like a per-agent fleet token (TestRunnerRevoke_RefusesNonOwnerAndAgentTokens)"},
	{"scoped_principal.go", `case authMethodOIDC:`, 1, gateStatic, "isScopedPrincipal: a machine falls to the default branch, not a scoped principal: unrestricted here (fleet scoping applies)"},
	{"scoped_principal.go", `case authMethodLand:`, 1, gateStatic, "isScopedPrincipal: land principal only; a machine is not one"},
	{"scoped_principal.go", `if authMethodFromContext(ctx) == authMethodLand {`, 1, gateStatic, "scopeFromContext: land principal only; a machine never reaches it (not a scoped principal)"},
	{"bridge_message_stamp.go", `if AuthIdentityFromContext(ctx).Type != "fleet" {`, 1, gateFleet, "bridge message stamp: only the slack-bridge fleet token; a machine, the slack-bridge machine principal included, stays ownership-gated (gd-2IEp8qx7bq)"},
	{"scif_approval_authz.go", `if id.Type != "fleet" || id.Subject == "" {`, 1, gateFleet, "not the scif approval portal"},
	{"service_field_policy.go", `if id.Type != "fleet" || id.Privileged {`, 1, gateFleet, "explicit machine arm falls back to the ownership gate (ordinary-fleet fallback); static keeps the unconditional bypass"},
	{"spawn_continuation_authz.go", `case id.Type == "fleet" && FleetFromContext(ctx) == slackBridgeFleet:`, 1, gateFleet, "same-role continuation: only the slack-bridge fleet token (a reserved name no untrusted minter can register) or the slack-bridge machine subject may name a prior agent bead; any other machine gets no continuation"},
	{"spawn_authz.go", `if !roleBearing(authMethodFromContext(ctx)) {`, 1, gateStatic, "privileged-role spawn refused absent an approved replay"},
	{"spawn_authz.go", `case authMethodOIDC, authMethodNone:`, 2, gateStatic, "agent role/project key via a field-mutation endpoint, and a project change by update, denied like static"},
	{"taint_grant_request.go", `case id.Type == "fleet" && id.Subject != "":`, 1, gateFleet, "no requester principal"},
	{"taint_read_gate.go", `if id.Type == "fleet" && id.Subject != "" {`, 1, gateFleet, "else: operator grants, none for a machine"},
	{"user_identity_authz.go", `case authMethodOIDC:`, 1, gateStatic, "user identity fields denied like static; only an OIDC admin, edge, or approved replay passes"},
	{"user_identity_authz.go", `case authMethodOIDC, authMethodNone:`, 1, gateStatic, "SetConfig on a policy-bearing type:* key denied like static"},
	{"user_provision.go", `if authMethodFromContext(r.Context()) != authMethodOIDC {`, 1, gateStatic, "no user bead provisioned"},
	{"ownership_grants.go", `if roleBearing(method) {`, 1, gateWiring, "resolveGrantCaller: tells a v2 edge envelope from a v1 one inside the Type edge arm; a machine never reaches it and resolves to the machine kind (grant table, gd-Ccp01OYo3N)"},
	{"write_authz.go", `if beadType == typeGatehouseRequest && action == "create" && !roleBearing(method) && gatehouseRequestCarveOut(ctx, s.store, labels) {`, 1, gateFleet, "gatehouse_request carve-out: only a lands-tenant agent bead on its per-agent token qualifies; a machine names no agent bead and falls to its capability grants (gd-K62coPtKkX)"},
	{"gatehouse_request.go", `case authMethodOIDC, authMethodLand:`, 1, gateStatic, "stampMessageGitHubAuthor: only a person's verified token names a GitHub account; a machine, like static, stamps none and may set none (gd-K62coPtKkX)"},
	{"write_authz.go", `if !roleBearing(method) {`, 1, gateStatic, "token scopes checked (a no-op for a machine)"},
	{"write_authz.go", `if wp.ProjectScoped && method == authMethodOIDC {`, 1, gateStatic, "project scope is OIDC-only"},
	{"write_authz.go", `return !roleBearing(method) && method != authMethodRunner && isMachineAllowedWrite(action, beadType)`, 1, gateWiring, "nonOIDCWriteAllowed: the machine arm returns first: machineAllowedWrites is not consulted for a machine (nor a runner)"},
}

var machineGatePattern = regexp.MustCompile(`authMethodOIDC|authMethodLand|roleBearing\(|Type (==|!=) "(oidc|static|fleet)"`)

// scanMachineGateSites returns "file\ttext" -> count for every non-comment line
// of a non-test source in this package that matches the gate pattern.
func scanMachineGateSites(t *testing.T) map[string]int {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(src), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "//") || !machineGatePattern.MatchString(line) {
				continue
			}
			got[f+"\t"+line]++
		}
	}
	return got
}

func TestMachineGates_TableCoversEverySite(t *testing.T) {
	got := scanMachineGateSites(t)
	want := map[string]int{}
	for _, r := range machineGateTable {
		if r.outcome == "" || r.note == "" {
			t.Errorf("%s %q: every row states an outcome and a note", r.file, r.text)
		}
		want[r.file+"\t"+r.text] += r.count
	}
	var problems []string
	for k, n := range got {
		if want[k] != n {
			problems = append(problems, fmt.Sprintf("source has %d, table has %d: ", n, want[k])+strings.ReplaceAll(k, "\t", " | "))
		}
	}
	for k, n := range want {
		if _, ok := got[k]; !ok {
			problems = append(problems, fmt.Sprintf("table has %d, source has none: ", n)+strings.ReplaceAll(k, "\t", " | "))
		}
	}
	sort.Strings(problems)
	if len(problems) > 0 {
		t.Fatalf("machineGateTable is out of sync with the auth-method/identity-type comparisons in the package.\n"+
			"A new or changed gate must state what a machine principal does there (add or edit a row, and exercise it in the tests below):\n  %s",
			strings.Join(problems, "\n  "))
	}
}

// --- reference identities ---

func gateMachineCtx() context.Context { return machineCtx("gardenio-controller") }

func gateStaticCtx() context.Context {
	return WithAuthIdentity(withAuthMethod(context.Background(), authMethodStatic), AuthIdentity{Type: "static", Subject: "static"})
}

func gateAgentCtx() context.Context {
	return withTestAgent(withAuthMethod(context.Background(), authMethodStatic), "gd-agent-1")
}

func gateFleetCtx() context.Context {
	return WithAuthIdentity(withAuthMethod(context.Background(), authMethodStatic), AuthIdentity{Type: "fleet", Subject: "some-bridge"})
}

func gateStatus(h http.HandlerFunc, ctx context.Context) int {
	req := httptest.NewRequest(http.MethodGet, "/x", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec.Code
}

func TestMachineGates_RouteWrappers_SameAsStatic(t *testing.T) {
	ok := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }
	for _, role := range []string{RoleViewer, RoleOperator, RoleAdmin} {
		m := gateStatus(requireRole(role, ok), gateMachineCtx())
		s := gateStatus(requireRole(role, ok), gateStaticCtx())
		if m != s {
			t.Errorf("requireRole(%s): machine=%d static=%d", role, m, s)
		}
		m = gateStatus(requireRoleMachineAllowed(role, ok), gateMachineCtx())
		s = gateStatus(requireRoleMachineAllowed(role, ok), gateStaticCtx())
		if m != s || m != http.StatusNoContent {
			t.Errorf("requireRoleMachineAllowed(%s): machine=%d static=%d; want both 204", role, m, s)
		}
	}
	if got := gateStatus(requireRole(RoleAdmin, ok), gateMachineCtx()); got != http.StatusForbidden {
		t.Errorf("a machine on an admin route = %d, want 403", got)
	}
}

func TestMachineGates_OIDCOnlyPredicates_SameAsStatic(t *testing.T) {
	srv, _, _ := testCtx(t)
	for name, ctx := range map[string]context.Context{"machine": gateMachineCtx(), "static": gateStaticCtx()} {
		if isAdminCaller(ctx) {
			t.Errorf("%s: isAdminCaller = true", name)
		}
		if callerIsOIDCAdmin(ctx) {
			t.Errorf("%s: callerIsOIDCAdmin = true", name)
		}
		if callerMayCreateTaint(ctx) {
			t.Errorf("%s: callerMayCreateTaint = true", name)
		}
		if ok, _ := srv.callerMayManageTaintPolicy(ctx, nil); ok {
			t.Errorf("%s: callerMayManageTaintPolicy = true", name)
		}
		if _, ok := scrubApprover(ctx); ok {
			t.Errorf("%s: scrubApprover ok", name)
		}
		if _, ok := srv.escalationApprover(ctx); ok {
			t.Errorf("%s: escalationApprover ok", name)
		}
		// A machine with no coordinator lane on an unstamped escalation is refused (D4).
		esc := &model.Bead{ID: "gd-gate-esc", Type: model.TypeEscalation, Status: model.StatusOpen, Fields: json.RawMessage(`{"operation_type":"github_pr_merge"}`)}
		if c := srv.checkMayApprove(ctx, esc); c.Allowed {
			t.Errorf("%s: checkMayApprove allowed a lane-less escalation: %+v", name, c)
		}
		if got := oidcAllowedProjects(ctx, nil); got != nil {
			t.Errorf("%s: oidcAllowedProjects = %v, want nil (unrestricted)", name, got)
		}
		if !scopeFromContext(ctx).Unrestricted {
			t.Errorf("%s: scopeFromContext restricted a non-OIDC caller", name)
		}
		if !srv.authorizeHookExecute(ctx, "anything") {
			t.Errorf("%s: authorizeHookExecute = false, want the service-token branch", name)
		}
	}
	if d := srv.authorizeUserIdentityFields(gateMachineCtx(), "update", "user", json.RawMessage(`{"email":"x@example.com"}`), nil); d == nil {
		t.Error("a machine must be denied user identity fields (like static)")
	}
	if d := srv.denyAgentIdentityFields(gateMachineCtx(), "update", "agent", []string{"service_account"}); d == nil {
		t.Error("a machine must be denied agent identity fields (like static)")
	}
	if d := srv.denyChannelBindingFields(gateMachineCtx(), "update", []string{"slack_channel_id"}); d == nil {
		t.Error("a machine must be denied a channel's Slack binding (like static)")
	}
	if d := srv.denyAgentRoleKeyWrite(gateMachineCtx(), "update", []string{agentRoleField}); d == nil {
		t.Error("a machine must be denied the agent role key on field-mutation endpoints (like static)")
	}
	if d := srv.denyAgentIdentityFields(gateStaticCtx(), "update", "agent", []string{"service_account"}); d == nil {
		t.Error("static must be denied agent identity fields")
	}
}

func TestMachineGates_StaticOnlyBranches_SameAsFleet(t *testing.T) {
	m, f := gateMachineCtx(), gateFleetCtx()

	if _, ok := resolveTaintAgentBeadID(m); ok {
		t.Error("machine resolves a taint agent bead; fleet does not")
	}
	if _, ok := resolveTaintAgentBeadID(f); ok {
		t.Fatal("control: fleet resolves a taint agent bead")
	}
	if _, ok := resolveTaintAgentBeadID(gateAgentCtx()); !ok {
		t.Fatal("control: a per-agent fleet identity must resolve a taint agent bead")
	}

	// Per-agent-token identity (agent_identity.go): a machine is not an agent,
	// like a bridge fleet token; a static agent identity is not one either.
	if name, ok := agentNameFromContext(m); ok {
		t.Errorf("machine resolves as agent %q", name)
	}
	if name, ok := agentNameFromContext(f); ok {
		t.Fatalf("control: bridge fleet resolves as agent %q", name)
	}
	agentFleet := WithFleet(WithAuthIdentity(withAuthMethod(context.Background(), authMethodStatic), AuthIdentity{Type: "fleet", Subject: "crew-a"}), "agent-gd-a1")
	if name, ok := agentNameFromContext(agentFleet); !ok || name != "crew-a" {
		t.Fatalf("control: per-agent fleet token must resolve as agent crew-a, got (%q,%v)", name, ok)
	}
	// A machine client whose ID carries the agent-fleet prefix, or whose
	// default fleet does, is still Type "machine".
	for _, spoof := range []context.Context{
		machineContext(context.Background(), NewFleetConfig("default", nil), machinePrincipal{ClientID: "agent-gd-a1"}),
		machineContext(context.Background(), NewFleetConfig("agent-gd-a1", nil), machinePrincipal{ClientID: "gardenio-controller"}),
	} {
		if name, ok := agentNameFromContext(spoof); ok {
			t.Errorf("machine spoofing the agent-fleet prefix resolves as agent %q", name)
		}
	}

	if callerIsScifApprovalPortal(m) {
		t.Error("machine is the scif approval portal")
	}
	for subj := range scifApprovalPortalSubjects {
		portal := WithAuthIdentity(withAuthMethod(context.Background(), authMethodStatic), AuthIdentity{Type: "fleet", Subject: subj})
		if !callerIsScifApprovalPortal(portal) {
			t.Fatalf("control: fleet %q must be the portal", subj)
		}
		spoof := machineContext(context.Background(), NewFleetConfig("default", nil), machinePrincipal{ClientID: subj})
		if callerIsScifApprovalPortal(spoof) {
			t.Errorf("machine client named %q passes as the scif portal", subj)
		}
	}

	req := func(ctx context.Context) string {
		r := httptest.NewRequest(http.MethodGet, "/x", nil).WithContext(ctx)
		return rateLimitIdentityKey(r, nil)
	}
	if got := req(m); got != "id:machine:machine:gardenio-controller" {
		t.Errorf("machine rate-limit key = %q; want a per-identity bucket like fleet", got)
	}
	if got := req(f); !strings.HasPrefix(got, "id:fleet:") {
		t.Fatalf("control: fleet key = %q", got)
	}

	// Escalation requesting_agent stamp and auto-owner: no-ops for a machine.
	srv, _, _ := testCtx(t)
	b := &model.Bead{ID: "gd-e1", Type: "escalation"}
	srv.applyEscalationRequestingAgentGuard(m, b)
	if len(b.Fields) != 0 {
		t.Errorf("machine got a requesting_agent stamp: %s", b.Fields)
	}
	ab := &model.Bead{ID: "gd-e2", Type: "escalation"}
	srv.applyEscalationRequestingAgentGuard(agentFleet, ab)
	if got := ab.Fields; !strings.Contains(string(got), `"requesting_agent":"gd-a1"`) ||
		!strings.Contains(string(got), `"requesting_agent_verified":"true"`) {
		t.Fatalf("control: per-agent fleet token must stamp its bead id as a verified requesting_agent, got %s", got)
	}
	if got, src := srv.resolveOperatedByPure(m); got != "machine:gardenio-controller" || src != operatedBySourceMachine {
		t.Errorf("machine operated_by=(%q,%q); must not take the agent-bead fallback", got, src)
	}
	if k, sub := operatorAliasForIdentity(AuthIdentityFromContext(m)); k != "" || sub != "" {
		t.Errorf("machine resolves an operator alias (%q,%q)", k, sub)
	}
}

func TestMachineGates_Ownership_SameAsFleet(t *testing.T) {
	srv, _, _ := testCtx(t)
	m, f, s := gateMachineCtx(), gateFleetCtx(), gateStaticCtx()
	other := ownedBead("gd-own1", "someone@example.com")

	if d := srv.authorizeOwnership(s, "update", other); d == nil {
		t.Fatal("control: nothing is a blanket ownership bypass any more; a legacy static identity is gated")
	}
	if d := srv.authorizeOwnership(f, "update", other); d == nil {
		t.Fatal("control: an ordinary fleet token is ownership-gated")
	}
	d := srv.authorizeOwnership(m, "update", other)
	if d == nil {
		t.Fatal("a machine principal must be ownership-gated on another's bead")
	}
	if d.CallerSubject != "machine:gardenio-controller" {
		t.Errorf("CallerSubject=%q want machine:gardenio-controller", d.CallerSubject)
	}

	// The owner themselves passes (assignee = the machine subject).
	own := ownedBead("gd-own2", "someone@example.com")
	own.Assignee = "machine:gardenio-controller"
	if d := srv.authorizeOwnership(m, "update", own); d != nil {
		t.Errorf("a machine assigned the bead must pass, got %+v", d)
	}

	// Config-kind beads, account beads included, skip the gate.
	acct := ownedBead("gd-acct", "someone@example.com")
	acct.Type, acct.Kind = "account", model.KindConfig
	if d := srv.authorizeOwnership(m, "update", acct); d != nil {
		t.Errorf("config-kind bead must skip ownership, got %+v", d)
	}

	// No dm carve-out, no privileged-fleet bypass.
	dm := ownedBead("gd-dm1", "agent-1")
	dm.Type = "dm"
	if d := srv.authorizeOwnership(f, "update", dm); d != nil {
		t.Fatalf("control: fleet has the dm carve-out, got %+v", d)
	}
	if d := srv.authorizeOwnership(m, "update", dm); d == nil {
		t.Error("a machine must not get the dm carve-out")
	}
	priv := WithAuthIdentity(withAuthMethod(context.Background(), authMethodStatic), AuthIdentity{Type: "fleet", Subject: "gitlab-bridge", Privileged: true})
	if d := srv.authorizeOwnership(priv, "update", other); d != nil {
		t.Fatalf("control: a privileged fleet bypasses ownership, got %+v", d)
	}

	// Shared-queue carve-out: escalation/decision beads, like fleet. The
	// static identity is retired (gd-ke6PC7bRto) and has no queue row.
	esc := ownedBead("gd-esc1", "agent-1")
	esc.Type = model.TypeEscalation
	for name, ctx := range map[string]context.Context{"machine": m, "fleet": f} {
		if d := srv.authorizeOwnership(ctx, "close", esc); d != nil {
			t.Errorf("%s: escalation shared-queue carve-out denied: %+v", name, d)
		}
	}
}

func TestMachineGates_ServiceFieldBypass_NotUnconditionalForMachine(t *testing.T) {
	srv, _, _ := testCtx(t)
	other := ownedBead("gd-trig1", "someone@example.com")
	other.Type = "trigger"
	if d := srv.authorizeServiceFieldBypass(gateStaticCtx(), "update", other); d != nil {
		t.Fatalf("control: static keeps the unconditional service-field bypass, got %+v", d)
	}
	if d := srv.authorizeServiceFieldBypass(gateMachineCtx(), "update", other); d == nil {
		t.Error("a machine must fall back to the ownership gate, not get the non-fleet unconditional bypass")
	}
}

func TestMachineGates_Whoami(t *testing.T) {
	op, src := computeWhoami(gateMachineCtx())
	if op != "machine:gardenio-controller" || src != whoamiSourceToken {
		t.Errorf("whoami = (%q,%q)", op, src)
	}
}

// Agent-pod headers honoured for a per-agent fleet token (interceptors.go) are
// not honoured for a machine: the machine branch returns before that wiring.
func TestMachineGates_AgentPodHeaders_NotHonoredForMachine(t *testing.T) {
	h := newMachineHarness(t, []string{"gardenio-controller"})
	rec, seen := h.serve(t, h.saToken(t, "gardenio-controller"), map[string]string{
		"X-Trigger-Bead":   "gd-trig1",
		"X-No-Auto-Parent": "1",
	})
	if rec.Code != http.StatusNoContent || !seen.called {
		t.Fatalf("status=%d called=%v body=%s", rec.Code, seen.called, rec.Body.String())
	}
	if seen.identity.Type != "machine" {
		t.Fatalf("identity type=%q, want machine", seen.identity.Type)
	}
	if got := triggerBeadFromContext(seen.ctx); got != "" {
		t.Errorf("X-Trigger-Bead honoured for a machine: %q", got)
	}
	if noAutoParentFromContext(seen.ctx) {
		t.Error("X-No-Auto-Parent honoured for a machine")
	}
}

// TestMachineGates_EscalationRoute_SameAsStatic: re-routing an escalation's
// audience is refused to a machine principal exactly as to the static token
// (gd-bmOiteQi36); only the daemon's own machine write passes.
func TestMachineGates_EscalationRoute_SameAsStatic(t *testing.T) {
	for name, ctx := range map[string]context.Context{"machine": gateMachineCtx(), "static": gateStaticCtx(), "agent": gateAgentCtx(), "fleet": gateFleetCtx()} {
		if callerMayRouteEscalation(ctx) {
			t.Errorf("%s: callerMayRouteEscalation = true", name)
		}
	}
	if !callerMayRouteEscalation(withMachineWrite(gateStaticCtx())) {
		t.Error("the daemon's own machine write may set an audience")
	}
}

// The lock-take break-glass is OIDC-only: a machine taking or clearing a lock a
// live agent holds is refused like the static token; an OIDC human passes.
func TestMachineGates_LockTake_SameAsStatic(t *testing.T) {
	s, ms, _ := newTestServer()
	seedLock(ms, "desk-a")
	seedDeskAgent(ms, "desk-a", model.StatusInProgress, "working")
	lock := *ms.beads[lockID]
	lock.Labels = ms.labels[lockID]

	for name, ctx := range map[string]context.Context{"machine": gateMachineCtx(), "static": gateStaticCtx()} {
		for op, in := range map[string]updateBeadInput{"take": take("desk-b"), "release": release()} {
			var held *lockHeldError
			if err := s.lockTakeRefusal(ctx, &lock, &in); !errors.As(err, &held) {
				t.Errorf("%s %s of a live holder's lock: err = %v, want a *lockHeldError", name, op, err)
			}
		}
	}
	in := release()
	if err := s.lockTakeRefusal(oidcCtx("operator@example.com", []string{RoleAdmin}), &lock, &in); err != nil {
		t.Errorf("an OIDC human clearing a live holder's lock: %v", err)
	}
}

// rosterLand holds a land principal, or any caller through a land door, to
// its own land's roster (gd-2iqSr9ANOH). A machine is neither, so it reads the
// fleet roster exactly as the static token does; a land principal's land
// without the land auth method is not one.
func TestMachineGates_RosterLand_SameAsStatic(t *testing.T) {
	for name, ctx := range map[string]context.Context{"machine": gateMachineCtx(), "static": gateStaticCtx(), "agent": gateAgentCtx(), "fleet": gateFleetCtx()} {
		if land, ok := rosterLand(ctx); ok {
			t.Errorf("%s: rosterLand = %q, true; want the fleet roster", name, land)
		}
	}
	forged := context.WithValue(gateStaticCtx(), landPrincipalContextKey, "claimlayer")
	if land, ok := rosterLand(forged); ok {
		t.Errorf("a land without authMethodLand: rosterLand = %q, true", land)
	}
	if land, ok := rosterLand(landPrincipalContext(context.Background(), nil, "claimlayer")); !ok || land != "claimlayer" {
		t.Errorf("a land principal: rosterLand = %q, %v; want claimlayer, true", land, ok)
	}
	if land, ok := rosterLand(withLandDoor(gateMachineCtx(), "claimlayer")); !ok || land != "claimlayer" {
		t.Errorf("through claimlayer's door: rosterLand = %q, %v; want claimlayer, true", land, ok)
	}
}
