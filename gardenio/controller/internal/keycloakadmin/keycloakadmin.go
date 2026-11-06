// Package keycloakadmin is the Lands provisioner's Keycloak admin client (Lands
// Track B spec gardenio/docs/superpowers/specs/2026-10-19-lands-track-b-design.md
// B-D6, §3.3 step 3, §3.7; step 8.5).
//
// It signs in as the confidential client gardenio-lands-provisioner and can do
// exactly these things:
//
//   - EnsureProjectGroup creates the top-level group project-<land>, stamped
//     with the land attribute, holding the realm role gardenio-project-member
//     and nothing else;
//   - AddProjectMember / RemoveProjectMember and AddTenant / RemoveTenant manage
//     membership of project-<land> and of the static chart group lands-tenants;
//     AddInvited joins a user to the static chart group invited, which the
//     viewer edge admits to /lands/ (sign-in invites, gd-ycE484kLyk);
//   - FindUserByGitHubID resolves a signed-in user by the github_id attribute
//     the GitHub IdP mapper writes (§2.2 item 1), and FindUserByEmail by their
//     exact email, with whether it is verified and which identity providers
//     the user is linked to;
//   - ProjectMembers lists a land's members, for deprovisioning.
//
// A land's owners and guests (Commons step 4.2, gd-9aCcG1zCCY) are the
// members of its subgroups /project-<land>/owners and /project-<land>/guests;
// they hold gardenio-project-member through the parent. EnsureLandRoleGroup
// creates a role subgroup, stamped with the same marker and holding no role of
// its own; AddLandMember joins a user to the group of a role, RemoveFromLand
// takes a user out of every group of the land, LandMembers lists all three,
// and UserLands names the lands a user holds any role in, so the last land a
// tenant leaves can take them out of lands-tenants too.
//
// The group name is never a caller input: callers pass a land name, which must
// be a lowercase DNS label, and the client builds project-<land> itself, so it
// cannot create or join a group outside the project- prefix. A land whose
// group the chart renders (ReservedGroups, Config.ReservedGroups) is refused,
// and a project- group is only ever used, joined, listed or left when it
// carries LandAttribute naming that land, which only EnsureProjectGroup's
// create writes: a group someone else made is never adopted (ErrNotLandGroup).
// User ids must be Keycloak UUIDs. lands-tenants and invited are only ever
// looked up, never created. The client never creates, edits or
// removes a role; the one role mapping it writes is gardenio-project-member on
// a project- group, and a project- group found holding any other role is
// refused (ErrForeignRoles) rather than repaired. Every operation is
// idempotent, and every membership change logs one audit line.
//
// Every lookup by name (a group, a group's members, a user by github_id) is
// a list call. Keycloak's admin permissions v2 filter list results with a
// partial evaluator that honours user, group and role policies but skips
// client policies, so the client's permissions must name its service-account
// user; granted through a client policy, every list answers 200 [] while
// calls by id still work (gd-7peNzGjJ3C). A create the lookup then cannot see
// fails with ErrGroupHidden.
//
// Runtime groups and the EDP keycloak-operator: see
// gardenio/docs/lands/keycloak-provisioner.md.
package keycloakadmin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// GroupPrefix is the only prefix of a group this client creates or joins
	// users to, besides TenantsGroup and InvitedGroup.
	GroupPrefix = "project-"
	// ProjectMemberRole is the only realm role a project group may hold (B-D2).
	ProjectMemberRole = "gardenio-project-member"
	// TenantsGroup is the static chart group every tenant joins (step 7.8).
	TenantsGroup = "lands-tenants"
	// InvitedGroup is the static chart group a person joins when their
	// sign-in invite is claimed. It holds no role: the gardenio.clonk.tv edge
	// admits it to /lands/, and that is all it does.
	InvitedGroup = "invited"
	// GitHubProvider is the alias of the GitHub identity provider.
	GitHubProvider = "github"
	// GitHubIDAttribute is the user attribute the GitHub IdP mapper writes.
	GitHubIDAttribute = "github_id"
	// LandAttribute is the group attribute EnsureProjectGroup stamps on the
	// project- group it creates. Its one value is the land name. It is the
	// ownership marker: a project- group without it was not made for this
	// land, and the client refuses to touch it.
	LandAttribute = "gardenio_land"
)

// LandRole is a role in one land: which of the land's groups a user is in
// (Commons step 4.2; kbeads land_role.go reads the same groups).
type LandRole string

const (
	// RoleMember is a direct member of project-<land>.
	RoleMember LandRole = "member"
	// RoleOwner is a member of /project-<land>/owners.
	RoleOwner LandRole = "owner"
	// RoleGuest is a member of /project-<land>/guests.
	RoleGuest LandRole = "guest"
)

// subgroup is the name of the role's subgroup under project-<land>, or ""
// for RoleMember, which is the land group itself.
func (r LandRole) subgroup() (string, error) {
	switch r {
	case RoleMember:
		return "", nil
	case RoleOwner:
		return "owners", nil
	case RoleGuest:
		return "guests", nil
	}
	return "", fmt.Errorf("keycloakadmin: unknown land role %q", r)
}

// landRoles are the roles in the order LandMembers reports them.
var landRoles = []LandRole{RoleOwner, RoleMember, RoleGuest}

// ReservedGroups are the groups the chart renders, as keycloak.groups and
// keycloak.projectGroups in the chart's values.yaml and in
// deploy-values/gardenio-k3s.yaml, plus lands-tenants (step 7.8). A land whose
// group is listed is refused before any request (ErrReservedLand), so a land
// named claimlayer can never reach the chart's project-claimlayer.
// TestReservedGroups_CoverChart fails when the chart grows a group that is not
// here; Config.ReservedGroups adds groups rendered elsewhere.
var ReservedGroups = []string{
	"gardenio-admins",
	"gardenio-operators",
	"gardenio-viewers",
	"gardenio-mentioners",
	"gardenio-escalation-approvers",
	"project-claimlayer",
	"project-scopetest", // deploy-values' Task 12 test fixture (gd-ldSxDwYWs1); drop it with the fixture
	TenantsGroup,
	InvitedGroup,
}

var (
	// ErrInvalidLand is returned for a land name that is not a lowercase DNS
	// label (the same rule as the chart's keycloak.projectGroups).
	ErrInvalidLand = errors.New("keycloakadmin: land name must be a lowercase DNS label")
	// ErrReservedLand is returned for a land whose group the chart renders.
	ErrReservedLand = errors.New("keycloakadmin: land group is rendered by the chart")
	// ErrNotLandGroup is returned when project-<land> exists but does not
	// carry LandAttribute naming that land. The client did not create it, so
	// it never adopts, joins, lists or leaves it.
	ErrNotLandGroup = errors.New("keycloakadmin: group exists but is not this land's (no " + LandAttribute + " marker)")
	// ErrInvalidUserID is returned for a user id that is not a Keycloak UUID.
	// It is rejected before any request is sent.
	ErrInvalidUserID = errors.New("keycloakadmin: user id must be a Keycloak UUID")
	// ErrGroupNotFound is returned when project-<land> (or lands-tenants) does
	// not exist. Membership calls never create a group.
	ErrGroupNotFound = errors.New("keycloakadmin: group not found")
	// ErrGroupHidden is returned when a group exists, because the create just
	// made it or answered 409, but the name lookup still cannot see it. Every
	// lookup is a list call, and Keycloak's admin permissions v2 filter list
	// results through a partial evaluator that skips client-type policies, so
	// a provisioner granted through a client policy sees 200 [] for every list
	// and search (gd-7peNzGjJ3C). The grants must use a user policy on the
	// client's service-account user; see keycloak-provisioner.md. It is not
	// ErrGroupNotFound, so no caller can read it as "gone".
	ErrGroupHidden = errors.New("keycloakadmin: group exists but list calls cannot see it (Keycloak admin permissions v2: grant through a user policy on the service-account user, not a client policy)")
	// ErrUserNotFound is returned when no user matches.
	ErrUserNotFound = errors.New("keycloakadmin: user not found")
	// ErrAmbiguousUser is returned when more than one user carries the same
	// github_id, or the same email. Joining either would be a guess, so the
	// caller must not.
	ErrAmbiguousUser = errors.New("keycloakadmin: more than one user matches")
	// ErrForeignRoles is returned when a project- group holds a role other than
	// gardenio-project-member. The client never removes roles, so an operator
	// has to look.
	ErrForeignRoles = errors.New("keycloakadmin: project group holds a role other than " + ProjectMemberRole)
)

// landName is the chart's projectGroups rule (templates/keycloak/groups.yaml).
var landName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// keycloakID is the form of the ids Keycloak gives users and groups.
var keycloakID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// maxLandLen keeps the land namespace, land-<land>, within a DNS label.
const maxLandLen = 63 - len("land-")

// GroupName returns the Keycloak group name for a land, ErrInvalidLand, or
// ErrReservedLand when the group is in ReservedGroups.
func GroupName(land string) (string, error) {
	if len(land) > maxLandLen || !landName.MatchString(land) {
		return "", fmt.Errorf("%w: %q", ErrInvalidLand, land)
	}
	name := GroupPrefix + land
	for _, r := range ReservedGroups {
		if name == r {
			return "", fmt.Errorf("%w: %s", ErrReservedLand, name)
		}
	}
	return name, nil
}

// Config holds the provisioner client's connection settings.
type Config struct {
	BaseURL      string // Keycloak base URL; must be https, e.g. "https://auth.example.com"
	Realm        string // realm name, e.g. "gardenio"; never "master"
	ClientID     string // confidential client, gardenio-lands-provisioner
	ClientSecret string
	// ReservedGroups adds group names to ReservedGroups, e.g. the chart's
	// keycloak.groups and keycloak.projectGroups as deployed.
	ReservedGroups []string
	// HTTPClient is used for every call; nil means a client with a 10s
	// timeout. Give it a Transport whose RootCAs hold a private CA to reach
	// a Keycloak served on one, e.g. the in-cluster Service. The client never follows a redirect, whatever its
	// CheckRedirect says: a token POST carries client_secret.
	HTTPClient *http.Client
}

// IsConfigured reports whether all required fields are set.
func (c Config) IsConfigured() bool {
	return c.BaseURL != "" && c.Realm != "" && c.ClientID != "" && c.ClientSecret != ""
}

// User is the part of a Keycloak user the provisioner needs.
type User struct {
	ID            string
	Username      string
	Email         string
	EmailVerified bool
	GitHubID      int64 // 0 when the user has no github_id attribute
	// FirstName and LastName are the user's profile name; the github identity
	// provider fills them from the GitHub profile's name, when it has one.
	FirstName string
	LastName  string
	// IdentityProviders are the aliases of the identity providers the user
	// is linked to. Only FindUserByEmail reads them; elsewhere it is nil.
	IdentityProviders []string
}

// Client is the Keycloak admin client. It is safe for concurrent use.
type Client struct {
	baseURL      string
	realm        string
	clientID     string
	clientSecret string
	httpClient   *http.Client
	logger       *slog.Logger

	reserved map[string]bool // Config.ReservedGroups

	tokenMu     sync.Mutex
	accessToken string
	tokenExpiry time.Time
}

// checkBaseURL accepts an https URL with a host and no userinfo, query or
// fragment. Plain http is refused, the in-cluster Keycloak Service included:
// it serves https on its own CA (gd-XRsosVFAau), which the caller trusts
// through Config.HTTPClient.
func checkBaseURL(base string) error {
	u, err := url.Parse(base)
	if err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" {
		return nil
	}
	return fmt.Errorf("keycloakadmin: base URL must be an https URL, got %q", base)
}

// New returns a client for cfg, or an error when cfg is incomplete.
func New(cfg Config, logger *slog.Logger) (*Client, error) {
	if !cfg.IsConfigured() {
		return nil, errors.New("keycloakadmin: base URL, realm, client id and client secret are required")
	}
	if err := checkBaseURL(cfg.BaseURL); err != nil {
		return nil, err
	}
	if strings.EqualFold(cfg.Realm, "master") {
		return nil, errors.New("keycloakadmin: refusing the master realm")
	}
	if logger == nil {
		logger = slog.Default()
	}
	hc := &http.Client{Timeout: 10 * time.Second}
	if cfg.HTTPClient != nil {
		copied := *cfg.HTTPClient
		hc = &copied
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	reserved := map[string]bool{}
	for _, g := range cfg.ReservedGroups {
		reserved[g] = true
	}
	return &Client{
		baseURL:      strings.TrimRight(cfg.BaseURL, "/"),
		realm:        cfg.Realm,
		clientID:     cfg.ClientID,
		clientSecret: cfg.ClientSecret,
		httpClient:   hc,
		logger:       logger,
		reserved:     reserved,
	}, nil
}

// EnsureProjectGroup makes sure the top-level group project-<land> exists,
// carries LandAttribute and holds the realm role gardenio-project-member, and
// returns its id. It is safe to re-run: a group this client created for the
// land is reused, and the role is mapped only when missing. An existing group
// without the land's marker is refused with ErrNotLandGroup, and one holding
// any other realm or client role with ErrForeignRoles; both are left as they
// are.
func (c *Client) EnsureProjectGroup(ctx context.Context, land string) (string, error) {
	name, err := c.landGroup(land)
	if err != nil {
		return "", err
	}
	id, err := c.findTopLevelGroup(ctx, name)
	if errors.Is(err, ErrGroupNotFound) {
		id, err = c.createLandGroup(ctx, name, land)
	}
	if err != nil {
		return "", err
	}
	if err := c.checkLandMarker(ctx, name, id, land); err != nil {
		return "", err
	}

	mappings, err := c.groupRoleMappings(ctx, id)
	if err != nil {
		return "", err
	}
	hasRole := false
	for _, r := range mappings.RealmMappings {
		if r.Name != ProjectMemberRole {
			return "", fmt.Errorf("%w: %s holds realm role %q", ErrForeignRoles, name, r.Name)
		}
		hasRole = true
	}
	for client, cm := range mappings.ClientMappings {
		if len(cm.Mappings) > 0 {
			return "", fmt.Errorf("%w: %s holds roles of client %q", ErrForeignRoles, name, client)
		}
	}
	if hasRole {
		return id, nil
	}

	// The role comes from the group's mappable list, not GET /roles/{name}:
	// that endpoint needs view-realm, which this client does not hold, while
	// "available" is filtered by the client's map-role permission on exactly
	// this role (docs/lands/keycloak-provisioner.md, Operator setup step 3).
	var available []roleRepresentation
	if err := c.do(ctx, http.MethodGet, "/groups/"+url.PathEscape(id)+"/role-mappings/realm/available", nil, nil, &available); err != nil {
		return "", fmt.Errorf("keycloakadmin: list mappable roles of %s: %w", name, err)
	}
	var role roleRepresentation
	for _, r := range available {
		if r.Name == ProjectMemberRole {
			role = r
		}
	}
	if role.ID == "" {
		return "", fmt.Errorf("keycloakadmin: role %s is not mappable onto %s (missing map-role admin permission?)", ProjectMemberRole, name)
	}
	body := []roleRepresentation{{ID: role.ID, Name: role.Name}}
	if err := c.do(ctx, http.MethodPost, "/groups/"+url.PathEscape(id)+"/role-mappings/realm", nil, body, nil); err != nil {
		return "", fmt.Errorf("keycloakadmin: map %s on %s: %w", ProjectMemberRole, name, err)
	}
	c.logger.Info("keycloakadmin: project group role mapped", "group", name, "group_id", id, "role", ProjectMemberRole)
	return id, nil
}

// AddProjectMember adds a user to project-<land>. The group must already
// exist (EnsureProjectGroup) and carry the land's marker; a user already in
// it is a no-op.
func (c *Client) AddProjectMember(ctx context.Context, land, userID string) error {
	return c.setProjectMembership(ctx, land, userID, true)
}

// RemoveProjectMember removes a user from project-<land>, which must carry the
// land's marker. A user who is not a member, or no longer exists, is a no-op.
func (c *Client) RemoveProjectMember(ctx context.Context, land, userID string) error {
	return c.setProjectMembership(ctx, land, userID, false)
}

func (c *Client) setProjectMembership(ctx context.Context, land, userID string, add bool) error {
	name, err := c.landGroup(land)
	if err != nil {
		return err
	}
	if err := checkUserID(userID); err != nil {
		return err
	}
	groupID, err := c.ownedLandGroup(ctx, name, land)
	if err != nil {
		return err
	}
	return c.setMembership(ctx, name, groupID, userID, add)
}

// AddTenant adds a user to the static group lands-tenants.
func (c *Client) AddTenant(ctx context.Context, userID string) error {
	return c.setStaticMembership(ctx, TenantsGroup, userID, true)
}

// RemoveTenant removes a user from lands-tenants. Callers remove a user only
// once they hold no land.
func (c *Client) RemoveTenant(ctx context.Context, userID string) error {
	return c.setStaticMembership(ctx, TenantsGroup, userID, false)
}

// AddInvited adds a user to the static group invited.
func (c *Client) AddInvited(ctx context.Context, userID string) error {
	return c.setStaticMembership(ctx, InvitedGroup, userID, true)
}

// InvitedMembers lists the direct members of the static group invited:
// everyone whose sign-in invite was claimed.
func (c *Client) InvitedMembers(ctx context.Context) ([]User, error) {
	id, err := c.findTopLevelGroup(ctx, InvitedGroup)
	if err != nil {
		return nil, err
	}
	return c.groupMembers(ctx, InvitedGroup, id)
}

// setStaticMembership adds or removes userID in group, which is TenantsGroup
// or InvitedGroup: chart groups that are looked up, never created.
func (c *Client) setStaticMembership(ctx context.Context, group, userID string, add bool) error {
	if err := checkUserID(userID); err != nil {
		return err
	}
	groupID, err := c.findTopLevelGroup(ctx, group)
	if err != nil {
		return err
	}
	return c.setMembership(ctx, group, groupID, userID, add)
}

// ProjectMembers lists the direct members of project-<land>, which must carry
// the land's marker.
func (c *Client) ProjectMembers(ctx context.Context, land string) ([]User, error) {
	name, err := c.landGroup(land)
	if err != nil {
		return nil, err
	}
	id, err := c.ownedLandGroup(ctx, name, land)
	if err != nil {
		return nil, err
	}
	return c.groupMembers(ctx, name, id)
}

// groupMembers lists the direct members of the group id, called name in
// errors.
func (c *Client) groupMembers(ctx context.Context, name, id string) ([]User, error) {
	const page = 100
	var out []User
	for first := 0; ; first += page {
		q := url.Values{
			"first":               {strconv.Itoa(first)},
			"max":                 {strconv.Itoa(page)},
			"briefRepresentation": {"false"},
		}
		var users []userRepresentation
		if err := c.do(ctx, http.MethodGet, "/groups/"+url.PathEscape(id)+"/members", q, nil, &users); err != nil {
			return nil, fmt.Errorf("keycloakadmin: list members of %s: %w", name, err)
		}
		for _, u := range users {
			out = append(out, u.user())
		}
		if len(users) < page {
			return out, nil
		}
	}
}

// EnsureLandRoleGroup makes sure the subgroup of role (RoleOwner or
// RoleGuest) exists under project-<land>, carries the land's marker and holds
// no role of its own, and returns its id. The land group must already exist
// (EnsureProjectGroup) and carry the marker. A subgroup of that name without
// the marker is refused with ErrNotLandGroup, and one holding any role with
// ErrForeignRoles; both are left as they are. Safe to re-run.
func (c *Client) EnsureLandRoleGroup(ctx context.Context, land string, role LandRole) (string, error) {
	sub, err := role.subgroup()
	if err != nil {
		return "", err
	}
	if sub == "" {
		return "", fmt.Errorf("keycloakadmin: %s is the land group itself; use EnsureProjectGroup", role)
	}
	name, err := c.landGroup(land)
	if err != nil {
		return "", err
	}
	parentID, err := c.ownedLandGroup(ctx, name, land)
	if err != nil {
		return "", err
	}
	label := name + "/" + sub
	id, err := c.findChildGroup(ctx, label, parentID, sub)
	if errors.Is(err, ErrGroupNotFound) {
		id, err = c.createChildGroup(ctx, label, parentID, sub, land)
	}
	if err != nil {
		return "", err
	}
	if err := c.checkMarker(ctx, label, id, sub, land); err != nil {
		return "", err
	}
	mappings, err := c.groupRoleMappings(ctx, id)
	if err != nil {
		return "", err
	}
	if len(mappings.RealmMappings) > 0 {
		return "", fmt.Errorf("%w: %s holds realm role %q", ErrForeignRoles, label, mappings.RealmMappings[0].Name)
	}
	for client, cm := range mappings.ClientMappings {
		if len(cm.Mappings) > 0 {
			return "", fmt.Errorf("%w: %s holds roles of client %q", ErrForeignRoles, label, client)
		}
	}
	return id, nil
}

// AddLandMember adds a user to the group of role in the land: project-<land>
// for RoleMember, its owners or guests subgroup otherwise. The group must
// already exist (EnsureProjectGroup, EnsureLandRoleGroup) and carry the land's
// marker; a user already in it is a no-op.
func (c *Client) AddLandMember(ctx context.Context, land string, role LandRole, userID string) error {
	sub, err := role.subgroup()
	if err != nil {
		return err
	}
	if sub == "" {
		return c.AddProjectMember(ctx, land, userID)
	}
	name, err := c.landGroup(land)
	if err != nil {
		return err
	}
	if err := checkUserID(userID); err != nil {
		return err
	}
	parentID, err := c.ownedLandGroup(ctx, name, land)
	if err != nil {
		return err
	}
	label := name + "/" + sub
	id, err := c.findChildGroup(ctx, label, parentID, sub)
	if err != nil {
		return err
	}
	if err := c.checkMarker(ctx, label, id, sub, land); err != nil {
		return err
	}
	return c.setMembership(ctx, label, id, userID, true)
}

// RemoveFromLand removes a user from project-<land> and from each of its role
// subgroups that exists and carries the land's marker. A user in none of them,
// or no longer existing, is a no-op.
func (c *Client) RemoveFromLand(ctx context.Context, land, userID string) error {
	name, err := c.landGroup(land)
	if err != nil {
		return err
	}
	if err := checkUserID(userID); err != nil {
		return err
	}
	parentID, err := c.ownedLandGroup(ctx, name, land)
	if err != nil {
		return err
	}
	for _, role := range []LandRole{RoleOwner, RoleGuest} {
		sub, _ := role.subgroup()
		label := name + "/" + sub
		id, err := c.findChildGroup(ctx, label, parentID, sub)
		if errors.Is(err, ErrGroupNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if err := c.checkMarker(ctx, label, id, sub, land); err != nil {
			return err
		}
		if err := c.setMembership(ctx, label, id, userID, false); err != nil {
			return err
		}
	}
	return c.setMembership(ctx, name, parentID, userID, false)
}

// LandMember is one user's role in a land.
type LandMember struct {
	User
	Role LandRole
}

// LandMembers lists everyone holding a role in the land: the members of its
// owners subgroup, of project-<land> itself and of its guests subgroup, in
// that order. A user in several is listed once, with the first role found.
// Every group read must carry the land's marker.
func (c *Client) LandMembers(ctx context.Context, land string) ([]LandMember, error) {
	name, err := c.landGroup(land)
	if err != nil {
		return nil, err
	}
	parentID, err := c.ownedLandGroup(ctx, name, land)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []LandMember
	for _, role := range landRoles {
		sub, _ := role.subgroup()
		label, id := name, parentID
		if sub != "" {
			label = name + "/" + sub
			if id, err = c.findChildGroup(ctx, label, parentID, sub); errors.Is(err, ErrGroupNotFound) {
				continue
			} else if err != nil {
				return nil, err
			}
			if err := c.checkMarker(ctx, label, id, sub, land); err != nil {
				return nil, err
			}
		}
		users, err := c.groupMembers(ctx, label, id)
		if err != nil {
			return nil, err
		}
		for _, u := range users {
			if !seen[u.ID] {
				seen[u.ID] = true
				out = append(out, LandMember{User: u, Role: role})
			}
		}
	}
	return out, nil
}

// UserLands returns the lands whose project-<land> group, or a subgroup of
// it, the user is in, sorted. It reads group paths only, so a project- group
// the chart renders counts too: callers take a user out of lands-tenants only
// when this is empty, and counting more lands can only keep a tenant in.
func (c *Client) UserLands(ctx context.Context, userID string) ([]string, error) {
	if err := checkUserID(userID); err != nil {
		return nil, err
	}
	const page = 100
	set := map[string]bool{}
	for first := 0; ; first += page {
		q := url.Values{
			"first":               {strconv.Itoa(first)},
			"max":                 {strconv.Itoa(page)},
			"briefRepresentation": {"true"},
		}
		var groups []groupRepresentation
		if err := c.do(ctx, http.MethodGet, "/users/"+url.PathEscape(userID)+"/groups", q, nil, &groups); err != nil {
			return nil, fmt.Errorf("keycloakadmin: list groups of %s: %w", userID, err)
		}
		for _, g := range groups {
			top, _, _ := strings.Cut(strings.TrimPrefix(g.Path, "/"), "/")
			if land, ok := strings.CutPrefix(top, GroupPrefix); ok && land != "" {
				set[land] = true
			}
		}
		if len(groups) < page {
			break
		}
	}
	out := make([]string, 0, len(set))
	for land := range set {
		out = append(out, land)
	}
	sort.Strings(out)
	return out, nil
}

// FindUserByGitHubID returns the one user whose github_id attribute is id.
// It returns ErrUserNotFound when none matches and ErrAmbiguousUser when
// several do. The attribute is compared exactly on this side as well, so a
// server that matches attributes loosely can never return the wrong user.
func (c *Client) FindUserByGitHubID(ctx context.Context, id int64) (User, error) {
	if id <= 0 {
		return User{}, fmt.Errorf("%w: github_id %d", ErrUserNotFound, id)
	}
	want := strconv.FormatInt(id, 10)
	q := url.Values{
		"q":                   {GitHubIDAttribute + ":" + want},
		"exact":               {"true"},
		"briefRepresentation": {"false"},
		"max":                 {"10"},
	}
	var users []userRepresentation
	if err := c.do(ctx, http.MethodGet, "/users", q, nil, &users); err != nil {
		return User{}, fmt.Errorf("keycloakadmin: find user by github_id: %w", err)
	}
	var match []userRepresentation
	for _, u := range users {
		vals := u.Attributes[GitHubIDAttribute]
		if len(vals) == 1 && vals[0] == want {
			match = append(match, u)
		}
	}
	switch len(match) {
	case 0:
		return User{}, fmt.Errorf("%w: github_id %s", ErrUserNotFound, want)
	case 1:
		return match[0].user(), nil
	default:
		return User{}, fmt.Errorf("%w: github_id %s", ErrAmbiguousUser, want)
	}
}

// FindUserByEmail returns the one user whose email is exactly email, with
// EmailVerified and IdentityProviders filled in. It returns ErrUserNotFound
// when none matches and ErrAmbiguousUser when several do. Keycloak stores
// emails lowercased and its search may match loosely, so callers pass a
// lowercase address and the comparison here is exact.
func (c *Client) FindUserByEmail(ctx context.Context, email string) (User, error) {
	if email == "" || strings.ContainsAny(email, " \t\r\n") || strings.Count(email, "@") != 1 {
		return User{}, fmt.Errorf("%w: email %q", ErrUserNotFound, email)
	}
	q := url.Values{
		"email":               {email},
		"exact":               {"true"},
		"briefRepresentation": {"false"},
		"max":                 {"10"},
	}
	var users []userRepresentation
	if err := c.do(ctx, http.MethodGet, "/users", q, nil, &users); err != nil {
		return User{}, fmt.Errorf("keycloakadmin: find user by email: %w", err)
	}
	var match []userRepresentation
	for _, u := range users {
		if u.Email == email {
			match = append(match, u)
		}
	}
	switch len(match) {
	case 0:
		return User{}, fmt.Errorf("%w: email %s", ErrUserNotFound, email)
	case 1:
	default:
		return User{}, fmt.Errorf("%w: email %s", ErrAmbiguousUser, email)
	}
	u := match[0].user()
	if err := checkUserID(u.ID); err != nil {
		return User{}, err
	}
	var links []struct {
		IdentityProvider string `json:"identityProvider"`
	}
	if err := c.do(ctx, http.MethodGet, "/users/"+url.PathEscape(u.ID)+"/federated-identity", nil, nil, &links); err != nil {
		return User{}, fmt.Errorf("keycloakadmin: read identity links of %s: %w", u.ID, err)
	}
	u.IdentityProviders = []string{}
	for _, l := range links {
		u.IdentityProviders = append(u.IdentityProviders, l.IdentityProvider)
	}
	return u, nil
}

// landGroup returns the group name for land, refusing an invalid land and
// any reserved group, built in or configured.
func (c *Client) landGroup(land string) (string, error) {
	name, err := GroupName(land)
	if err != nil {
		return "", err
	}
	if c.reserved[name] {
		return "", fmt.Errorf("%w: %s", ErrReservedLand, name)
	}
	return name, nil
}

// ownedLandGroup returns the id of the existing top-level group name, which
// must carry the marker for land.
func (c *Client) ownedLandGroup(ctx context.Context, name, land string) (string, error) {
	id, err := c.findTopLevelGroup(ctx, name)
	if err != nil {
		return "", err
	}
	if err := c.checkLandMarker(ctx, name, id, land); err != nil {
		return "", err
	}
	return id, nil
}

// checkLandMarker reads group id and returns ErrNotLandGroup unless it is name
// and its LandAttribute is exactly [land].
func (c *Client) checkLandMarker(ctx context.Context, name, id, land string) error {
	return c.checkMarker(ctx, name, id, name, land)
}

// checkMarker reads group id and returns ErrNotLandGroup unless its own name
// is want and its LandAttribute is exactly [land]. label names the group in
// errors.
func (c *Client) checkMarker(ctx context.Context, label, id, want, land string) error {
	var g groupRepresentation
	if err := c.do(ctx, http.MethodGet, "/groups/"+url.PathEscape(id), nil, nil, &g); err != nil {
		return fmt.Errorf("keycloakadmin: read group %s: %w", label, err)
	}
	if vals := g.Attributes[LandAttribute]; g.Name != want || len(vals) != 1 || vals[0] != land {
		return fmt.Errorf("%w: %s", ErrNotLandGroup, label)
	}
	return nil
}

// checkUserID refuses anything but a Keycloak UUID, so a user id can never be
// a path segment such as "..".
func checkUserID(userID string) error {
	if !keycloakID.MatchString(userID) {
		return fmt.Errorf("%w: %q", ErrInvalidUserID, userID)
	}
	return nil
}

// setMembership adds or removes userID in the top-level group name whose id
// is groupID. Callers have checked userID, and only an owned land group,
// TenantsGroup or InvitedGroup reaches here.
func (c *Client) setMembership(ctx context.Context, name, groupID, userID string, add bool) error {
	path := "/users/" + url.PathEscape(userID) + "/groups/" + url.PathEscape(groupID)
	action := "add"
	method := http.MethodPut
	if !add {
		action = "remove"
		method = http.MethodDelete
	}
	err := c.do(ctx, method, path, nil, nil, nil)
	var se *statusError
	if errors.As(err, &se) && se.code == http.StatusNotFound {
		if !add {
			// The user is gone, so they are in no group: removal is done.
			err = nil
		} else {
			err = fmt.Errorf("%w: %s", ErrUserNotFound, userID)
		}
	}
	if err != nil {
		return fmt.Errorf("keycloakadmin: %s %s in %s: %w", action, userID, name, err)
	}
	c.logger.Info("keycloakadmin: membership", "action", action, "group", name, "group_id", groupID, "user_id", userID)
	return nil
}

// findTopLevelGroup returns the id of the top-level group whose name is
// exactly name, or ErrGroupNotFound.
func (c *Client) findTopLevelGroup(ctx context.Context, name string) (string, error) {
	q := url.Values{
		"search":              {name},
		"exact":               {"true"},
		"briefRepresentation": {"true"},
		"max":                 {"100"},
	}
	var groups []groupRepresentation
	if err := c.do(ctx, http.MethodGet, "/groups", q, nil, &groups); err != nil {
		return "", fmt.Errorf("keycloakadmin: find group %s: %w", name, err)
	}
	for _, g := range groups {
		// A search can return a parent whose subgroup matched; only the
		// top-level group of exactly this name counts.
		if g.Name == name && g.Path == "/"+name {
			if !keycloakID.MatchString(g.ID) {
				return "", fmt.Errorf("keycloakadmin: find group %s: unexpected id %q", name, g.ID)
			}
			return g.ID, nil
		}
	}
	return "", fmt.Errorf("%w: %s", ErrGroupNotFound, name)
}

// createLandGroup creates name stamped with LandAttribute=land and returns its
// id. A concurrent create (409) is fine too; the caller checks the marker
// either way. The id comes from the same lookup every later call uses, so a
// realm whose list calls hide the group fails here with ErrGroupHidden rather
// than on the next call with ErrGroupNotFound.
func (c *Client) createLandGroup(ctx context.Context, name, land string) (string, error) {
	rep := groupRepresentation{Name: name, Attributes: map[string][]string{LandAttribute: {land}}}
	created, err := c.create(ctx, name, "/groups", rep)
	if err != nil {
		return "", err
	}
	if created {
		c.logger.Info("keycloakadmin: project group created", "group", name)
	}
	id, err := c.findTopLevelGroup(ctx, name)
	if errors.Is(err, ErrGroupNotFound) {
		return "", fmt.Errorf("%w: %s", ErrGroupHidden, name)
	}
	return id, err
}

// findChildGroup returns the id of the direct subgroup of parentID whose name
// is exactly sub, or ErrGroupNotFound. label names it in errors.
func (c *Client) findChildGroup(ctx context.Context, label, parentID, sub string) (string, error) {
	q := url.Values{
		"search":              {sub},
		"exact":               {"true"},
		"briefRepresentation": {"true"},
		"max":                 {"100"},
	}
	var groups []groupRepresentation
	if err := c.do(ctx, http.MethodGet, "/groups/"+url.PathEscape(parentID)+"/children", q, nil, &groups); err != nil {
		return "", fmt.Errorf("keycloakadmin: find group %s: %w", label, err)
	}
	for _, g := range groups {
		if g.Name == sub {
			if !keycloakID.MatchString(g.ID) {
				return "", fmt.Errorf("keycloakadmin: find group %s: unexpected id %q", label, g.ID)
			}
			return g.ID, nil
		}
	}
	return "", fmt.Errorf("%w: %s", ErrGroupNotFound, label)
}

// createChildGroup creates the subgroup sub of parentID stamped with
// LandAttribute=land and returns its id, read back like createLandGroup's.
func (c *Client) createChildGroup(ctx context.Context, label, parentID, sub, land string) (string, error) {
	rep := groupRepresentation{Name: sub, Attributes: map[string][]string{LandAttribute: {land}}}
	created, err := c.create(ctx, label, "/groups/"+url.PathEscape(parentID)+"/children", rep)
	if err != nil {
		return "", err
	}
	if created {
		c.logger.Info("keycloakadmin: land role group created", "group", label)
	}
	id, err := c.findChildGroup(ctx, label, parentID, sub)
	if errors.Is(err, ErrGroupNotFound) {
		return "", fmt.Errorf("%w: %s", ErrGroupHidden, label)
	}
	return id, err
}

// create POSTs a group to path and reports whether it was created (201). A
// 409, the group already exists, is not an error; label names the group in
// errors.
func (c *Client) create(ctx context.Context, label, path string, rep groupRepresentation) (bool, error) {
	resp, err := c.send(ctx, http.MethodPost, path, nil, rep)
	if err != nil {
		return false, fmt.Errorf("keycloakadmin: create group %s: %w", label, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusConflict {
		return false, fmt.Errorf("keycloakadmin: create group %s: %w", label, readStatusError(resp))
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	return resp.StatusCode == http.StatusCreated, nil
}

func (c *Client) groupRoleMappings(ctx context.Context, groupID string) (mappingsRepresentation, error) {
	var m mappingsRepresentation
	if err := c.do(ctx, http.MethodGet, "/groups/"+url.PathEscape(groupID)+"/role-mappings", nil, nil, &m); err != nil {
		return m, fmt.Errorf("keycloakadmin: read role mappings of group %s: %w", groupID, err)
	}
	return m, nil
}

// do sends a request to the realm's admin API and decodes a 2xx JSON body
// into out (when out is non-nil). Any other status is a *statusError.
func (c *Client) do(ctx context.Context, method, path string, q url.Values, body, out any) error {
	resp, err := c.send(ctx, method, path, q, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return readStatusError(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// send issues one admin API request with a bearer token. A 401 drops the
// cached token and retries once with a fresh one.
func (c *Client) send(ctx context.Context, method, path string, q url.Values, body any) (*http.Response, error) {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return nil, err
		}
	}
	u := c.baseURL + "/admin/realms/" + url.PathEscape(c.realm) + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	for attempt := 0; ; attempt++ {
		token, err := c.token(ctx)
		if err != nil {
			return nil, fmt.Errorf("keycloakadmin: token: %w", err)
		}
		req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/json")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			resp.Body.Close()
			c.dropToken(token)
			continue
		}
		return resp, nil
	}
}

// token returns a cached client_credentials access token, fetching a new one
// 30 seconds before the old one expires.
func (c *Client) token(ctx context.Context) (string, error) {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	if c.accessToken != "" && time.Now().Before(c.tokenExpiry) {
		return c.accessToken, nil
	}

	tokenURL := c.baseURL + "/realms/" + url.PathEscape(c.realm) + "/protocol/openid-connect/token"
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {c.clientID},
		"client_secret": {c.clientSecret},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", readStatusError(resp)
	}
	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return "", err
	}
	if tr.AccessToken == "" {
		return "", errors.New("token response has no access_token")
	}
	c.accessToken = tr.AccessToken
	c.tokenExpiry = time.Now().Add(time.Duration(tr.ExpiresIn-30) * time.Second)
	return c.accessToken, nil
}

func (c *Client) dropToken(stale string) {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	if c.accessToken == stale {
		c.accessToken = ""
	}
}

// statusError is a non-2xx admin API response.
type statusError struct {
	code int
	body string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("%d %s: %s", e.code, http.StatusText(e.code), e.body)
}

func readStatusError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return &statusError{code: resp.StatusCode, body: strings.TrimSpace(string(b))}
}

type groupRepresentation struct {
	ID         string              `json:"id,omitempty"`
	Name       string              `json:"name"`
	Path       string              `json:"path,omitempty"`
	Attributes map[string][]string `json:"attributes,omitempty"`
}

type roleRepresentation struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name"`
}

type mappingsRepresentation struct {
	RealmMappings  []roleRepresentation `json:"realmMappings"`
	ClientMappings map[string]struct {
		Mappings []roleRepresentation `json:"mappings"`
	} `json:"clientMappings"`
}

type userRepresentation struct {
	ID            string              `json:"id"`
	Username      string              `json:"username"`
	Email         string              `json:"email"`
	EmailVerified bool                `json:"emailVerified"`
	FirstName     string              `json:"firstName"`
	LastName      string              `json:"lastName"`
	Attributes    map[string][]string `json:"attributes"`
}

func (u userRepresentation) user() User {
	out := User{ID: u.ID, Username: u.Username, Email: u.Email, EmailVerified: u.EmailVerified, FirstName: u.FirstName, LastName: u.LastName}
	if vals := u.Attributes[GitHubIDAttribute]; len(vals) == 1 {
		if id, err := strconv.ParseInt(vals[0], 10, 64); err == nil && id > 0 {
			out.GitHubID = id
		}
	}
	return out
}
