package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/commons"
	"gardenio/controller/internal/githubapp"
	"gardenio/controller/internal/lands/purse"
)

// Lands member invites (Track B spec §3.7, step 8.9): the Commons' People
// lightbox (internal/commons invites.go) over land_invite beads.
//
// The Commons checks the viewer is the land's owner (member_manage) before it
// calls here. land_invite is admin-gated in kbeads, so an owner never writes
// one on their own bearer: the github_id must be the one GitHub answered for
// the login, never one a caller typed. Every daemon call goes out as the
// viewer's machine principal with the gardenio-machine-lands-gate role, on a
// context that carries none of the request's identity (landsGate.callCtx's
// rule), and every bead the daemon returns is re-checked to be a land_invite
// of the land asked about.
//
// The login is resolved through the land's own Lands App installation (its
// project's github_installation_id and git_url), with a metadata-only token
// revoked once read (githubapp.UserByLogin).

const (
	landInviteType   = "land_invite"
	landInviteTTL    = 14 * 24 * time.Hour
	landInviteListMx = 500
)

// landInvitesAPI is the daemon surface the invites use, always as the
// viewer's machine principal. *beadsapi.Client implements it.
type landInvitesAPI interface {
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
	CreateBeadDetail(ctx context.Context, req beadsapi.CreateBeadRequest) (*beadsapi.BeadDetail, error)
	UpdateBeadFieldsTyped(ctx context.Context, beadID string, fields map[string]any) error
	GetBead(ctx context.Context, beadID string) (*beadsapi.BeadDetail, error)
}

// githubUsers resolves a login through a land's installation.
// *githubapp.Client implements it.
type githubUsers interface {
	UserByLogin(ctx context.Context, installationID int64, repo githubapp.RepoRef, login string) (githubapp.GitHubUser, error)
}

// landInvites is the commons.LandInvites implementation.
type landInvites struct {
	daemon   landInvitesAPI
	machine  beadsapi.TokenSource // attached to every call; nil only in tests
	projects purse.Source         // the tenant project beads, read as a system read
	github   githubUsers
	logger   *slog.Logger
	now      func() time.Time

	// mu serializes "already invited?" with the create, so a double click
	// files one invite.
	mu sync.Mutex
}

// commons returns the hooks for commons.Handler.Invites.
func (l *landInvites) commons() commons.LandInvites {
	return commons.LandInvites{List: l.list, Invite: l.invite, Revoke: l.revoke}
}

// callCtx is landsGate.callCtx: the parent's deadline and cancellation, the
// machine principal, and nothing of the request's identity.
func (l *landInvites) callCtx(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), landsGateCallTimeout)
	stop := context.AfterFunc(parent, cancel)
	return beadsapi.WithMachinePrincipal(ctx, l.machine), func() { stop(); cancel() }
}

// landInviteFrom reads b as an invite of land, or reports false.
func landInviteFrom(b *beadsapi.BeadDetail, land string) (commons.Invite, int64, bool) {
	if b == nil || b.Type != landInviteType || b.Fields["land"] != land {
		return commons.Invite{}, 0, false
	}
	var projects []string
	for _, lb := range b.Labels {
		if p, ok := strings.CutPrefix(lb, "project:"); ok {
			projects = append(projects, p)
		}
	}
	if len(projects) != 1 || projects[0] != land {
		return commons.Invite{}, 0, false
	}
	f := b.Fields
	id, _ := strconv.ParseInt(f["github_id"], 10, 64)
	exp, _ := time.Parse(time.RFC3339, f["expires_at"])
	return commons.Invite{
		ID: b.ID, Login: f["github_login"], Role: f["role"], State: f["state"], ExpiresAt: exp,
		Revoking: f["revoke_requested_at"] != "",
	}, id, true
}

type openInvite struct {
	commons.Invite
	githubID  int64
	createdAt time.Time
	joinedAt  time.Time // zero until the provisioner joins the person
}

// open returns land's open invites, pending and joined, newest first.
func (l *landInvites) open(ctx context.Context, land string) ([]openInvite, error) {
	cctx, cancel := l.callCtx(ctx)
	defer cancel()
	res, err := l.daemon.ListBeadsFiltered(cctx, beadsapi.ListBeadsQuery{
		Types: []string{landInviteType}, Labels: []string{"project:" + land},
		Statuses: []string{"open", "in_progress", "blocked", "deferred"}, Limit: landInviteListMx,
	})
	if err != nil {
		return nil, err
	}
	var out []openInvite
	for _, b := range res.Beads {
		inv, id, ok := landInviteFrom(b, land)
		if !ok || (inv.State != "pending" && inv.State != "joined") {
			continue
		}
		joined, _ := time.Parse(time.RFC3339, b.Fields["joined_at"])
		out = append(out, openInvite{Invite: inv, githubID: id, createdAt: b.CreatedAt, joinedAt: joined})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].createdAt.After(out[j].createdAt) })
	return out, nil
}

func (l *landInvites) list(ctx context.Context, land string) ([]commons.Invite, error) {
	open, err := l.open(ctx, land)
	if err != nil {
		return nil, err
	}
	out := make([]commons.Invite, len(open))
	for i, o := range open {
		out[i] = o.Invite
	}
	return out, nil
}

// landRepo is the land's Lands App installation and repo, from its tenant
// project bead.
func (l *landInvites) landRepo(ctx context.Context, land string) (int64, githubapp.RepoRef, error) {
	rctx, cancel := context.WithTimeout(beadsapi.WithSystemRead(context.WithoutCancel(ctx)), landsGateCallTimeout)
	defer cancel()
	projects, err := purse.TenantProjects(rctx, l.projects)
	if err != nil {
		return 0, githubapp.RepoRef{}, err
	}
	p := projects[land]
	if p == nil || p.Fields["land_opened_at"] == "" {
		return 0, githubapp.RepoRef{}, &commons.InviteRefusal{Message: land + " is not open yet; invite people once it is."}
	}
	inst, _ := strconv.ParseInt(strings.TrimSpace(p.Fields["github_installation_id"]), 10, 64)
	repo, ok := githubapp.ParseRepoURL(p.Fields["git_url"], "github.com")
	if inst <= 0 || !ok {
		return 0, githubapp.RepoRef{}, fmt.Errorf("land %s has no Lands App installation or github.com repo on its project", land)
	}
	return inst, repo, nil
}

func (l *landInvites) invite(r *http.Request, land, login, role string) (commons.Invite, error) {
	by := commonsSelf(r)
	if by == "" {
		return commons.Invite{}, &commons.InviteRefusal{Message: "Sign in again to invite people."}
	}
	if l.github == nil {
		return commons.Invite{}, &commons.InviteRefusal{Message: "Invites need the Lands GitHub App, which is not set up here."}
	}
	inst, repo, err := l.landRepo(r.Context(), land)
	if err != nil {
		return commons.Invite{}, err
	}
	u, err := l.github.UserByLogin(r.Context(), inst, repo, login)
	switch {
	case errors.Is(err, githubapp.ErrNoGitHubUser):
		return commons.Invite{}, &commons.InviteRefusal{Message: "GitHub has no account called @" + login + "."}
	case err != nil:
		return commons.Invite{}, fmt.Errorf("looking up @%s on GitHub: %w", login, err)
	case u.Type != "User":
		return commons.Invite{}, &commons.InviteRefusal{Message: "@" + u.Login + " is not a person's account on GitHub. Invite a person."}
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	open, err := l.open(r.Context(), land)
	if err != nil {
		return commons.Invite{}, err
	}
	if i := slices.IndexFunc(open, func(o openInvite) bool { return o.githubID == u.ID && !o.Revoking }); i >= 0 {
		what := "already invited"
		if open[i].State == "joined" {
			what = "already in " + land
		}
		return commons.Invite{}, &commons.InviteRefusal{Message: "@" + u.Login + " is " + what + "."}
	}
	now := l.clock()
	exp := now.Add(landInviteTTL)
	raw, err := json.Marshal(map[string]any{
		"land": land, "github_id": strconv.FormatInt(u.ID, 10), "github_login": u.Login, "role": role,
		"invited_by": by, "expires_at": exp.UTC().Format(time.RFC3339), "state": "pending",
	})
	if err != nil {
		return commons.Invite{}, err
	}
	cctx, cancel := l.callCtx(r.Context())
	defer cancel()
	b, err := l.daemon.CreateBeadDetail(cctx, beadsapi.CreateBeadRequest{
		Title: "Invite @" + u.Login + " to " + land, Type: landInviteType, Kind: "data", Priority: 3,
		Labels: []string{"project:" + land}, Fields: raw, CreatedBy: by,
	})
	if err != nil {
		return commons.Invite{}, err
	}
	l.logger.Info("lands invites: invited", "land", land, "invite", b.ID, "github_id", u.ID, "role", role, "by", by)
	return commons.Invite{ID: b.ID, Login: u.Login, Role: role, State: "pending", ExpiresAt: exp}, nil
}

func (l *landInvites) revoke(r *http.Request, land, id string) (commons.Invite, error) {
	by := commonsSelf(r)
	if by == "" {
		return commons.Invite{}, &commons.InviteRefusal{Message: "Sign in again to revoke."}
	}
	cctx, cancel := l.callCtx(r.Context())
	defer cancel()
	b, err := l.daemon.GetBead(cctx, id)
	var apiErr *beadsapi.APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
		b, err = nil, nil
	}
	if err != nil {
		return commons.Invite{}, err
	}
	inv, _, ok := landInviteFrom(b, land)
	if !ok || b.Status == "closed" || (inv.State != "pending" && inv.State != "joined") {
		return commons.Invite{}, &commons.InviteRefusal{Message: "That invite is no longer open."}
	}
	if inv.Revoking {
		return inv, nil
	}
	if err := l.daemon.UpdateBeadFieldsTyped(cctx, id, map[string]any{
		"revoke_requested_at": l.clock().UTC().Format(time.RFC3339), "revoke_requested_by": by,
	}); err != nil {
		return commons.Invite{}, err
	}
	inv.Revoking = true
	l.logger.Info("lands invites: revoke requested", "land", land, "invite", id, "state", inv.State, "by", by)
	return inv, nil
}

func (l *landInvites) clock() time.Time {
	if l.now != nil {
		return l.now()
	}
	return time.Now()
}
