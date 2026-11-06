package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"gardenio/kbeads/internal/model"
)

// Per-land audiences (land_audience.go, task gd-eAfptUBh9Z). The user behind
// every token here is in two lands, claimlayer and other, unless the name says
// otherwise; the token's land- audience says which land it was minted for.
const (
	tokLandAudClaimlayer = "landaud-claimlayer-tok" // aud land-claimlayer
	tokLandAudOther      = "landaud-other-tok"      // aud land-other
	tokLandAudBoth       = "landaud-both-tok"       // aud land-claimlayer and land-other
	tokLandAudNone       = "landaud-none-tok"       // no land audience
	tokLandAudNotMember  = "landaud-notmember-tok"  // aud land-other, member of claimlayer only
	tokLandAudAdmin      = "landaud-admin-tok"      // an admin in claimlayer, aud land-claimlayer
	tokLandAudNoRole     = "landaud-norole-tok"     // aud land-claimlayer, in its group, without project-member
	tokLandAudUnreadable = "landaud-unreadable-tok" // an admin whose aud claim cannot be read
)

func newLandAudienceFixture(t *testing.T) *scopedFixture {
	t.Helper()
	f := newScopedFixture(t)
	member := []string{RoleProjectMember}
	two := []string{"/project-claimlayer", "/project-other"}
	for tok, info := range map[string]tokenInfo{
		tokLandAudClaimlayer: {email: "two@example.com", roles: member, groups: two, aud: []string{"gardenio-traefik-forwardauth", "land-claimlayer"}},
		tokLandAudOther:      {email: "two@example.com", roles: member, groups: two, aud: []string{"land-other"}},
		tokLandAudBoth:       {email: "two@example.com", roles: member, groups: two, aud: []string{"land-claimlayer", "land-other"}},
		tokLandAudNone:       {email: "two@example.com", roles: member, groups: two, aud: []string{"gardenio-traefik-forwardauth"}},
		tokLandAudNotMember:  {email: "cl@example.com", roles: member, groups: []string{"/project-claimlayer"}, aud: []string{"land-other"}},
		tokLandAudAdmin: {email: "owner@example.com", roles: []string{RoleViewer, RoleOperator, RoleAdmin, RoleProjectMember},
			groups: []string{"/project-claimlayer"}, aud: []string{"land-claimlayer"}},
		tokLandAudNoRole: {email: "viewer@example.com", roles: []string{RoleViewer},
			groups: []string{"/project-claimlayer"}, aud: []string{"land-claimlayer"}},
		tokLandAudUnreadable: {email: "owner@example.com", roles: []string{RoleViewer, RoleOperator, RoleAdmin, RoleProjectMember},
			groups: []string{"/project-claimlayer"}, audErr: true},
	} {
		info.azp = testBrowserClient
		f.keycloak.tokens[tok] = info
	}
	return f
}

// landDo issues one request as token, through the land door for door when it
// is not "" (the edge and landgate set X-Land-Door to the land in the path).
func (f *scopedFixture) landDo(t *testing.T, token, door, method, path string, body any) (int, string) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = strings.NewReader(string(b))
	}
	req, err := http.NewRequest(method, f.ts.URL+path, r)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if door != "" {
		req.Header.Set(LandDoorHeader, door)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// landListIDs lists beads (path carries any filter) and returns the
// non-user bead ids, or nil with the status when the list is refused.
func (f *scopedFixture) landListIDs(t *testing.T, token, door, path string) (int, []string) {
	t.Helper()
	code, body := f.landDo(t, token, door, http.MethodGet, path, nil)
	if code != http.StatusOK {
		return code, nil
	}
	var out struct {
		Beads []*model.Bead `json:"beads"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	ids := map[string]bool{}
	for _, b := range out.Beads {
		if b.Type != "user" {
			ids[b.ID] = true
		}
	}
	return code, sortedKeys(ids)
}

// The bug: a user in lands claimlayer and other, holding a token minted for
// claimlayer, reaches claimlayer and nothing of other, through the land door
// for claimlayer or straight at the daemon, by every read and write route.
func TestLandAudience_TwoLandBearerReachesOnlyItsLand(t *testing.T) {
	f := newLandAudienceFixture(t)
	for _, door := range []string{"claimlayer", ""} {
		for _, path := range []string{"/v1/beads?limit=100", "/v1/beads?limit=100&project=other", "/v1/beads?limit=100&labels=project:other"} {
			code, ids := f.landListIDs(t, tokLandAudClaimlayer, door, path)
			for _, id := range ids {
				if id != beadClaimlayer {
					t.Errorf("door %q GET %s: saw %s, want only %s (status %d)", door, path, id, beadClaimlayer, code)
				}
			}
		}
		if code, _ := f.landDo(t, tokLandAudClaimlayer, door, http.MethodGet, "/v1/beads/"+beadOther, nil); code != http.StatusForbidden && code != http.StatusNotFound {
			t.Errorf("door %q GET other's bead: status %d, want 403/404", door, code)
		}
		if code, _ := f.landDo(t, tokLandAudClaimlayer, door, http.MethodGet, "/v1/beads/"+beadClaimlayer, nil); code != http.StatusOK {
			t.Errorf("door %q GET own bead: status %d, want 200", door, code)
		}
		// The Lands map's confinement (landAdmitsCaller) follows the same
		// scope, so /lands/<other>/ through the door for claimlayer is no
		// land at all.
		if code, _ := f.landDo(t, tokLandAudClaimlayer, door, http.MethodGet, "/v1/lands/other/members", nil); code != http.StatusNotFound {
			t.Errorf("door %q members of other: status %d, want 404", door, code)
		}
		// The member list is not on the land door (land_door_routes.txt;
		// a tenant reads its crew through the Lands API).
		wantMembers := http.StatusOK
		if door != "" {
			wantMembers = http.StatusNotFound
		}
		if code, body := f.landDo(t, tokLandAudClaimlayer, door, http.MethodGet, "/v1/lands/claimlayer/members", nil); code != wantMembers {
			t.Errorf("door %q members of claimlayer: status %d, want %d (%s)", door, code, wantMembers, body)
		}
		// Writes into other are refused: a new bead, a comment, a relabel.
		create := map[string]any{"title": "x", "type": "task", "labels": []string{"project:other"}}
		if code, body := f.landDo(t, tokLandAudClaimlayer, door, http.MethodPost, "/v1/beads", create); code != http.StatusForbidden {
			t.Errorf("door %q create in other: status %d, want 403 (%s)", door, code, body)
		}
		if code, body := f.landDo(t, tokLandAudClaimlayer, door, http.MethodPost, "/v1/beads/"+beadOther+"/comments", map[string]any{"text": "hi"}); code != http.StatusForbidden && code != http.StatusNotFound {
			t.Errorf("door %q comment on other: status %d, want 403/404 (%s)", door, code, body)
		}
		if code, body := f.landDo(t, tokLandAudClaimlayer, door, http.MethodPost, "/v1/beads/"+beadClaimlayer+"/labels", map[string]any{"label": "project:other"}); code != http.StatusForbidden {
			t.Errorf("door %q label own bead into other: status %d, want 403 (%s)", door, code, body)
		}
	}
	// Positive control: the same user's token without a land audience still
	// reads both lands off the land door (today's behaviour).
	if _, ids := f.landListIDs(t, tokLandAudNone, "", "/v1/beads?limit=100"); !reflect.DeepEqual(ids, []string{beadClaimlayer, beadOther}) {
		t.Errorf("no-audience token off the door saw %v, want [cl-1 other-1]", ids)
	}
}

// The land door admits only a token minted for exactly the door's land.
func TestLandAudience_LandDoorRequiresMatchingAudience(t *testing.T) {
	f := newLandAudienceFixture(t)
	for _, tc := range []struct {
		name, token, door string
		want              int
	}{
		{"token for the door's land", tokLandAudClaimlayer, "claimlayer", http.StatusOK},
		{"token for another land", tokLandAudOther, "claimlayer", http.StatusForbidden},
		{"token for no land", tokLandAudNone, "claimlayer", http.StatusForbidden},
		{"token for two lands", tokLandAudBoth, "claimlayer", http.StatusForbidden},
		{"single-land user without a land token", tokScopedClaimlayer, "claimlayer", http.StatusForbidden},
		{"admin static token", scopedAdminToken, "claimlayer", http.StatusForbidden},
		{"fleet token", tokFleetBridge, "claimlayer", http.StatusForbidden},
		{"land principal of the door's land", tokLandClaimlayer, "claimlayer", http.StatusOK},
		{"land principal of another land", tokLandClaimlayer, "other", http.StatusForbidden},
		{"malformed door", tokLandAudClaimlayer, "Claim/layer", http.StatusForbidden},
	} {
		code, body := f.landDo(t, tc.token, tc.door, http.MethodGet, "/v1/beads?limit=100", nil)
		if code != tc.want {
			t.Errorf("%s: status %d, want %d (%s)", tc.name, code, tc.want, body)
		}
	}
	// Two X-Land-Door headers are refused rather than one picked.
	req, _ := http.NewRequest(http.MethodGet, f.ts.URL+"/v1/beads", nil)
	req.Header.Set("Authorization", "Bearer "+tokLandAudClaimlayer)
	req.Header.Add(LandDoorHeader, "claimlayer")
	req.Header.Add(LandDoorHeader, "other")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("two door headers: status %d, want 403", resp.StatusCode)
	}
}

// GET /v1/me through the land door is the admission check the edge and
// landgate run for an opaque land token (Door D.18a gd-WTyoLCSMUp): 200 with
// the land principal's own body for a token of the door's land, 403 for one
// of another land or for a fleet token, 401 for an unknown token.
func TestLandAudience_LandDoorMeAdmitsOnlyThisLandsPrincipal(t *testing.T) {
	f := newLandAudienceFixture(t)
	for _, tc := range []struct {
		name, token, door string
		want              int
		body              string
	}{
		{"land principal of the door's land", tokLandClaimlayer, "claimlayer", http.StatusOK, `"operator_id":"land:claimlayer"`},
		{"land principal of another land", tokLandClaimlayer, "other", http.StatusForbidden, "credential is for another land"},
		{"fleet token", tokFleetBridge, "claimlayer", http.StatusForbidden, "a land bearer token is required"},
		{"admin static token", scopedAdminToken, "claimlayer", http.StatusForbidden, "a land bearer token is required"},
		{"unknown token", "no-such-token", "claimlayer", http.StatusUnauthorized, ""},
	} {
		code, body := f.landDo(t, tc.token, tc.door, http.MethodGet, "/v1/me", nil)
		if code != tc.want || !strings.Contains(body, tc.body) {
			t.Errorf("%s: %d %s, want %d containing %q", tc.name, code, body, tc.want, tc.body)
		}
	}
	code, body := f.landDo(t, tokLandClaimlayer, "claimlayer", http.MethodGet, "/v1/me", nil)
	var me meJSON
	if err := json.Unmarshal([]byte(body), &me); code != http.StatusOK || err != nil {
		t.Fatalf("GET /v1/me: %d %s", code, body)
	}
	if me.Unrestricted || len(me.Lands) != 1 || me.Lands[0].Name != "claimlayer" {
		t.Errorf("me = %+v, want restricted to claimlayer alone (landgate checks exactly this)", me)
	}
}

// A token for two lands reaches neither, on or off the door, and a token for a
// land the user is not a member of reaches nothing.
func TestLandAudience_TwoAudiencesOrNonMemberReachNothing(t *testing.T) {
	f := newLandAudienceFixture(t)
	for _, tok := range []string{tokLandAudBoth, tokLandAudNotMember} {
		if _, ids := f.landListIDs(t, tok, "", "/v1/beads?limit=100"); len(ids) != 0 {
			t.Errorf("%s listed %v, want nothing", tok, ids)
		}
		if code, _ := f.landDo(t, tok, "", http.MethodGet, "/v1/beads/"+beadClaimlayer, nil); code != http.StatusNotFound {
			t.Errorf("%s GET cl-1: status %d, want 404 (as missing)", tok, code)
		}
	}
}

// An admin's token minted for a land is a land token: it loses the admin
// role and reaches that land only.
func TestLandAudience_AdminLandTokenIsScoped(t *testing.T) {
	f := newLandAudienceFixture(t)
	if _, ids := f.landListIDs(t, tokLandAudAdmin, "claimlayer", "/v1/beads?limit=100"); !reflect.DeepEqual(ids, []string{beadClaimlayer}) {
		t.Errorf("admin land token saw %v, want [cl-1]", ids)
	}
	if code, _ := f.landDo(t, tokLandAudAdmin, "", http.MethodGet, "/v1/beads/"+beadGardenio, nil); code != http.StatusNotFound {
		t.Errorf("admin land token GET gd-1: status %d, want 404 (as missing)", code)
	}
	// Positive control: the admin's ordinary token still sees everything.
	if _, ids := f.landListIDs(t, tokOwnerAdmin, "", "/v1/beads?limit=100"); len(ids) != 4 {
		t.Errorf("admin control saw %v, want all four", ids)
	}
}

// A land token whose caller lacks gardenio-project-member reaches nothing, on
// or off the door, as landgate refuses it (gate.go admit): its land group
// alone does not make it a land member.
func TestLandAudience_LandTokenWithoutProjectMemberReachesNothing(t *testing.T) {
	f := newLandAudienceFixture(t)
	for _, door := range []string{"claimlayer", ""} {
		if code, ids := f.landListIDs(t, tokLandAudNoRole, door, "/v1/beads?limit=100"); len(ids) != 0 {
			t.Errorf("door %q: listed %v (status %d), want nothing", door, ids, code)
		}
		if code, _ := f.landDo(t, tokLandAudNoRole, door, http.MethodGet, "/v1/beads/"+beadClaimlayer, nil); code != http.StatusForbidden {
			t.Errorf("door %q GET cl-1: status %d, want 403", door, code)
		}
		create := map[string]any{"title": "x", "type": "task", "labels": []string{"project:claimlayer"}}
		if code, body := f.landDo(t, tokLandAudNoRole, door, http.MethodPost, "/v1/beads", create); code != http.StatusForbidden {
			t.Errorf("door %q create in claimlayer: status %d, want 403 (%s)", door, code, body)
		}
	}
	ctx := withAuthMethod(context.Background(), authMethodOIDC)
	ctx = withTokenLands(ctx, []string{"claimlayer"})
	ctx = WithRoles(ctx, []string{})
	r := (&http.Request{Pattern: "GET /v1/beads"}).WithContext(ctx)
	if scopedRouteAdmits(r, RoleViewer) {
		t.Error("scopedRouteAdmits waived the role for a land token without project-member")
	}
}

// A token whose audiences cannot be read is refused (fail closed): it might
// be a land token, and read as one with no land it would keep its admin role.
func TestLandAudience_UnreadableAudienceIsRefused(t *testing.T) {
	f := newLandAudienceFixture(t)
	for _, door := range []string{"claimlayer", ""} {
		if code, body := f.landDo(t, tokLandAudUnreadable, door, http.MethodGet, "/v1/beads?limit=100", nil); code != http.StatusUnauthorized {
			t.Errorf("door %q: status %d, want 401 (%s)", door, code, body)
		}
	}
	verifier := &oidcIssuerMock{tokens: map[string]tokenInfo{"unreadable": {email: "owner@example.com", roles: []string{RoleAdmin}, audErr: true}}}
	handler := func(context.Context, any) (any, error) { return "ok", nil }
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer unreadable"))
	_, err := AuthInterceptor(nil, verifier)(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/beads.v1.BeadsService/ListBeads"}, handler)
	if status.Code(err) != codes.Unauthenticated {
		t.Errorf("gRPC: err = %v, want Unauthenticated", err)
	}
}

// A single-land user's ordinary token is unchanged off the door.
func TestLandAudience_SingleLandUserUnchanged(t *testing.T) {
	f := newLandAudienceFixture(t)
	if _, ids := f.landListIDs(t, tokScopedClaimlayer, "", "/v1/beads?limit=100"); !reflect.DeepEqual(ids, []string{beadClaimlayer}) {
		t.Errorf("single-land user saw %v, want [cl-1]", ids)
	}
}

func TestLandAudiences(t *testing.T) {
	got := landAudiences([]string{"gardenio-traefik-forwardauth", "land-b", "land-a", "land-b", "account", "land-"})
	if want := []string{"", "a", "b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("landAudiences = %q, want %q", got, want)
	}
	if got := landAudiences([]string{"gardenio-cli"}); len(got) != 0 {
		t.Errorf("no land audience = %q, want none", got)
	}
}

func TestLandAudienceRoles(t *testing.T) {
	all := []string{RoleViewer, RoleOperator, RoleAdmin, RoleProjectMember}
	if got := landAudienceRoles(all, nil); !reflect.DeepEqual(got, all) {
		t.Errorf("no land: %v, want unchanged", got)
	}
	if got := landAudienceRoles(all, []string{"a"}); !reflect.DeepEqual(got, []string{RoleProjectMember}) {
		t.Errorf("land token: %v, want only %s", got, RoleProjectMember)
	}
	if got := landAudienceRoles([]string{RoleAdmin}, []string{"a"}); len(got) != 0 {
		t.Errorf("land token without project-member: %v, want none", got)
	}
}

func TestScopeFromContext_LandAudience(t *testing.T) {
	base := withAuthMethod(context.Background(), authMethodOIDC)
	base = WithRoles(base, []string{RoleProjectMember})
	base = WithGroups(base, []string{"/project-a", "/project-b/owners"})
	for _, tc := range []struct {
		name  string
		lands []string
		want  []string
	}{
		{"no land audience", nil, []string{"a", "b"}},
		{"land a", []string{"a"}, []string{"a"}},
		{"land b keeps its tier", []string{"b"}, []string{"b"}},
		{"land not a member of", []string{"c"}, nil},
		{"two lands", []string{"a", "b"}, nil},
	} {
		d := scopeFromContext(withTokenLands(base, tc.lands))
		if d.Unrestricted || !reflect.DeepEqual(d.Projects, tc.want) {
			t.Errorf("%s: scope %+v, want projects %v", tc.name, d, tc.want)
		}
		for _, p := range d.Projects {
			if _, ok := d.Tiers[p]; !ok {
				t.Errorf("%s: no tier for %s", tc.name, p)
			}
		}
	}
	if d := scopeFromContext(withTokenLands(base, []string{"b"})); d.Tiers["b"] != tierOwner {
		t.Errorf("land b tier = %v, want owner", d.Tiers["b"])
	}
}
