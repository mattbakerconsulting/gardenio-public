package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/store"
)

// Lands member invites (spec gardenio/docs/superpowers/specs/
// 2026-10-19-lands-track-b-design.md §3.7; step 8.9).
//
// A land's owner invites a person by GitHub login from the Commons. The viewer
// resolves the login to GitHub's numeric id and files one type=land_invite
// bead in the land. When someone signs in whose Keycloak user carries that
// github_id, the Lands provisioner joins them to the land with the invite's
// role and marks the invite joined; an owner's revoke takes them out again.
//
// The type is admin-gated: an owner never writes one on their own bearer, so
// the github_id is always the one the viewer looked up, never one a caller
// typed. The viewer files and revokes as its machine principal after its own
// owner check; the provisioner moves the state. The rules here are about
// shape:
//
//   - exactly one project label, project:<land>, and land names a tenant
//     land;
//   - born pending, with nothing the provisioner writes already set;
//   - who is invited, to what role, by whom and until when never change, and
//     neither does the land label;
//   - state moves pending → joined / revoked / expired, joined → revoked;
//   - one open invite per GitHub account in a land, and a create carrying
//     open_invite_cap is filed only while the land holds fewer open invites
//     (admitLandInvite). The daemon holds both, not the viewer, so they hold
//     across viewer replicas (gd-GPvddfQY6y).

const typeLandInvite model.BeadType = "land_invite"

// Land invite states.
const (
	landInvitePending = "pending"
	landInviteJoined  = "joined"
	landInviteRevoked = "revoked"
	landInviteExpired = "expired"
)

// landInviteStateEdges is the invite lifecycle. A joined invite stays open as
// the record of the membership it made, so an owner's revoke can find it.
var landInviteStateEdges = map[string][]string{
	landInvitePending: {landInviteJoined, landInviteRevoked, landInviteExpired},
	landInviteJoined:  {landInviteRevoked},
	landInviteRevoked: nil,
	landInviteExpired: nil,
}

// landInviteFixedFields are written when the invite is filed and never after.
var landInviteFixedFields = []string{"land", "github_id", "github_login", "role", "invited_by", "expires_at", landInviteCapField}

// landInviteCapField is the open invites a land may hold for this one to be
// filed. Optional; the demo door sets it to LANDS_DEMO_MAX_GUESTS
// (beads-viewer lands_demo.go).
const landInviteCapField = "open_invite_cap"

// landInviteOpenMax bounds the open-invite read behind open_invite_cap; the
// viewer reads no more (lands_invites.go landInviteListMx).
const landInviteOpenMax = 500

// landInviteOutcomeFields are the provisioner's and the revoke's; a new
// invite carries none of them.
var landInviteOutcomeFields = []string{
	"joined_user_id", "joined_at", "revoke_requested_at", "revoke_requested_by", "revoked_at", "outcome_note",
}

// githubLoginPattern is a GitHub login: letters, digits and single hyphens, at
// most 39 characters, not starting or ending with a hyphen.
var githubLoginPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9]|-[A-Za-z0-9]){0,38}$`)

type landInviteFields struct {
	Land        string `json:"land"`
	GitHubID    string `json:"github_id"`
	GitHubLogin string `json:"github_login"`
	Role        string `json:"role"`
	State       string `json:"state"`
}

func decodeLandInviteFields(raw json.RawMessage) (landInviteFields, map[string]any) {
	var f landInviteFields
	all := map[string]any{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &f)
		_ = json.Unmarshal(raw, &all)
	}
	return f, all
}

// landInviteLabelError reports a label set that breaks the rule: exactly one
// project label, and it names the invite's land.
func landInviteLabelError(labels []string, land string) error {
	projects := projectLabelValues(labels)
	if len(projects) == 1 && projects[0] == land {
		return nil
	}
	if len(projects) == 0 {
		return fmt.Errorf("a land_invite needs the label project:%s", land)
	}
	return fmt.Errorf("a land_invite carries exactly one project label, project:%s; got %s", land, strings.Join(projects, ", "))
}

// validateLandInviteShape checks the field rules that hold in every state.
func validateLandInviteShape(f landInviteFields) error {
	id, err := strconv.ParseInt(f.GitHubID, 10, 64)
	if err != nil || id <= 0 || strconv.FormatInt(id, 10) != f.GitHubID {
		return fmt.Errorf("github_id %q must be GitHub's numeric user id", f.GitHubID)
	}
	if !githubLoginPattern.MatchString(f.GitHubLogin) {
		return fmt.Errorf("github_login %q is not a GitHub login", f.GitHubLogin)
	}
	if f.Role != "member" && f.Role != "guest" {
		return fmt.Errorf("an invite's role is member or guest, not %q", f.Role)
	}
	return nil
}

// validateLandInviteCreate enforces the create-time rules: the label, the
// field shape, state pending with no outcome set, and a land that is a tenant
// land. The project lookup reads through the store, never the caller's scope.
func validateLandInviteCreate(ctx context.Context, st store.Store, bead *model.Bead) error {
	f, all := decodeLandInviteFields(bead.Fields)
	land := strings.TrimSpace(f.Land)
	if land == "" || land != f.Land {
		return fmt.Errorf("a land_invite names its land")
	}
	if err := landInviteLabelError(bead.Labels, land); err != nil {
		return err
	}
	if f.State != landInvitePending {
		return fmt.Errorf("a land_invite is created in state %q, not %q", landInvitePending, f.State)
	}
	var set []string
	for _, k := range landInviteOutcomeFields {
		if v, ok := all[k]; ok && !isEmptyFieldValue(v) {
			set = append(set, k)
		}
	}
	if len(set) > 0 {
		return fmt.Errorf("a new land_invite cannot carry %s", strings.Join(set, ", "))
	}
	if err := validateLandInviteShape(f); err != nil {
		return err
	}
	return tenantLandError(ctx, st, land)
}

// tenantLandError reports a land that is not a tenant land's project. The
// lookup reads through the store, never the caller's scope.
func tenantLandError(ctx context.Context, st store.Store, land string) error {
	project, err := st.FindProjectByName(ctx, land)
	switch {
	case errors.Is(err, sql.ErrNoRows) || (err == nil && project == nil):
		return fmt.Errorf("land %q is not a project", land)
	case err != nil:
		return fmt.Errorf("project lookup failed: %w", err)
	}
	var pf struct {
		LandKind string `json:"land_kind"`
	}
	if len(project.Fields) > 0 {
		_ = json.Unmarshal(project.Fields, &pf)
	}
	if pf.LandKind != landKindTenant {
		return fmt.Errorf("land %q is not a tenant land (land_kind=%q); only a tenant land takes invites", land, pf.LandKind)
	}
	return nil
}

// validateLandInviteUpdate checks a fields update against the post-merge
// bead: the fixed fields unchanged, the shape, and a legal state move.
func validateLandInviteUpdate(prior, next json.RawMessage) error {
	from, before := decodeLandInviteFields(prior)
	to, after := decodeLandInviteFields(next)
	for _, k := range landInviteFixedFields {
		if !jsonValueEqual(before[k], after[k]) {
			return fmt.Errorf("a land_invite's %s is fixed when it is written; revoke it and invite again", k)
		}
	}
	if err := validateLandInviteShape(to); err != nil {
		return err
	}
	if from.State == to.State {
		return nil
	}
	if !slices.Contains(landInviteStateEdges[from.State], to.State) {
		legal := "none"
		if edges := landInviteStateEdges[from.State]; len(edges) > 0 {
			legal = strings.Join(edges, ", ")
		}
		return fmt.Errorf("land_invite state cannot move %s → %s; from %s it may move to %s", from.State, to.State, from.State, legal)
	}
	return nil
}

// landInviteOpen reports whether an invite still holds or offers a place: open,
// pending or joined, and not being revoked, as the viewer counts them
// (beads-viewer lands_invites.go open).
func landInviteOpen(b *model.Bead) bool {
	if b == nil || b.Status == model.StatusClosed {
		return false
	}
	f, all := decodeLandInviteFields(b.Fields)
	if f.State != landInvitePending && f.State != landInviteJoined {
		return false
	}
	v, ok := all["revoke_requested_at"]
	return !ok || isEmptyFieldValue(v)
}

// admitLandInvite holds a land to one open invite per GitHub account and to
// the open_invite_cap a create carries. A create for an account that already
// holds an open invite comes back as that invite, so a second press on any
// viewer replica files nothing; at the cap it is refused. The caller holds
// landInviteMu across this and the insert.
func admitLandInvite(ctx context.Context, st store.Store, bead *model.Bead) (*model.Bead, error) {
	f, all := decodeLandInviteFields(bead.Fields)
	if f.Land == "" || f.GitHubID == "" {
		return nil, nil
	}
	label := "project:" + f.Land
	mine, _, err := st.ListBeads(ctx, model.BeadFilter{
		Type:   []model.BeadType{typeLandInvite},
		Labels: []string{label},
		Fields: map[string]string{"github_id": f.GitHubID},
		Fleet:  model.FleetAll,
		Limit:  50,
	})
	if err != nil {
		return nil, fmt.Errorf("land invite lookup failed: %w", err)
	}
	for _, b := range mine {
		if prior, _ := decodeLandInviteFields(b.Fields); prior.Land == f.Land && prior.GitHubID == f.GitHubID && landInviteOpen(b) {
			return b, nil
		}
	}
	limit, ok := all[landInviteCapField].(float64)
	if !ok {
		return nil, nil
	}
	open, _, err := st.ListBeads(ctx, model.BeadFilter{
		Type:   []model.BeadType{typeLandInvite},
		Labels: []string{label},
		Status: []model.Status{model.StatusOpen, model.StatusInProgress, model.StatusBlocked, model.StatusDeferred},
		Fleet:  model.FleetAll,
		Limit:  landInviteOpenMax,
	})
	if err != nil {
		return nil, fmt.Errorf("land invite count failed: %w", err)
	}
	n := 0
	for _, b := range open {
		if inv, _ := decodeLandInviteFields(b.Fields); inv.Land == f.Land && landInviteOpen(b) {
			n++
		}
	}
	if float64(n) >= limit {
		return nil, inputError(fmt.Sprintf("land %s holds %d open invites, the %s this invite carries", f.Land, n, landInviteCapField))
	}
	return nil, nil
}

// jsonValueEqual compares two decoded JSON values.
func jsonValueEqual(a, b any) bool {
	ra, _ := json.Marshal(a)
	rb, _ := json.Marshal(b)
	return string(ra) == string(rb)
}

// landInviteLabelChangeError refuses moving an invite to another land: its
// project labels never change.
func landInviteLabelChangeError(bead *model.Bead, label string, adding bool) error {
	if bead == nil || bead.Type != typeLandInvite || !strings.HasPrefix(label, "project:") {
		return nil
	}
	if adding && slices.Contains(bead.Labels, label) {
		return nil
	}
	return fmt.Errorf("a land_invite's project label is fixed when it is written")
}

// landInviteLabelsUpdateError is the PATCH twin of landInviteLabelChangeError.
func landInviteLabelsUpdateError(prior, next []string) error {
	a, b := projectLabelValues(prior), projectLabelValues(next)
	slices.Sort(a)
	slices.Sort(b)
	if slices.Equal(a, b) {
		return nil
	}
	return fmt.Errorf("a land_invite's project label is fixed when it is written")
}
