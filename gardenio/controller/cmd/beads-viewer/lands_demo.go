package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/keycloakadmin"
	"gardenio/controller/internal/lands/purse"
	"gardenio/controller/internal/oidcauth"
)

// The demo land's door (task gd-c6LzZkk0qy; decision gd-cGMRpc4uLM chose a
// hosted demo over a docker compose file). Someone with no cluster signs in
// with GitHub, accepts the terms and asks for a guest pass to the one land the
// operator set aside (LANDS_DEMO_LAND). The door files a land_invite for the
// caller's own verified github_id, role guest, as the viewer's machine
// principal, the way the owner's People lightbox does (lands_invites.go). The
// Lands provisioner's invite pass joins them like any invite and ends the pass
// LANDS_DEMO_GUEST_TTL after the join (internal/lands/provision invites.go).
// With the pass, gb login and gb ping reach the daemon as that person.
// Runbook: gardenio/docs/lands/demo-land.md.
//
// The limits, besides the guest tier itself (kbeads action_table.go: a guest
// writes messages and reactions only, and never spawns or approves): the
// current terms accepted first, one open pass per GitHub account, and at most
// LANDS_DEMO_MAX_GUESTS open passes at once. The door checks the last two to
// answer the page, and kbeads holds them on the create (land_invite.go
// admitLandInvite: one open invite per account, open_invite_cap), so they hold
// across viewer replicas (gd-GPvddfQY6y).

const (
	landsDemoLandEnv      = "LANDS_DEMO_LAND"
	landsDemoDaemonURLEnv = "LANDS_DEMO_DAEMON_URL"
	landsDemoMaxGuestsEnv = "LANDS_DEMO_MAX_GUESTS"
	landsDemoGuestTTLEnv  = "LANDS_DEMO_GUEST_TTL"

	landsDemoMaxGuestsDefault = 50
	landsDemoGuestTTLDefault  = 24 * time.Hour
	// landsDemoJoinWindow is how long a pass waits for the provisioner,
	// which runs once a minute; past it the pass expires and closes.
	landsDemoJoinWindow = time.Hour
)

// Demo door states the page renders.
const (
	landsDemoClosed      = "closed"      // no demo land on this server, or it is not open
	landsDemoNoBearer    = "nobearer"    // signed in without a forwardable OIDC token
	landsDemoTerms       = "terms"       // the current terms are not accepted
	landsDemoNoGitHub    = "nogithub"    // the token carries no GitHub account
	landsDemoUnavailable = "unavailable" // the daemon could not say
	landsDemoOpen        = "open"        // may ask for a pass
	landsDemoFull        = "full"        // every pass is taken
	landsDemoPending     = "pending"     // asked; the provisioner has not joined them yet
	landsDemoJoined      = "joined"      // a guest of the demo land
)

// landsDemoLogin is a GitHub login, as kbeads checks an invite's
// github_login (land_invite.go).
var landsDemoLogin = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9]|-[A-Za-z0-9]){0,38}$`)

// errLandsDemoFull refuses a pass when LANDS_DEMO_MAX_GUESTS are open.
var errLandsDemoFull = errors.New("the demo land is full")

// landsDemo is the door. nil on the Server when LANDS_DEMO_LAND is unset.
type landsDemo struct {
	land      string
	daemonURL string
	maxGuests int
	guestTTL  time.Duration
	invites   *landInvites // the daemon, as the machine principal
}

// landsDemoView is what the demo page renders.
type landsDemoView struct {
	State     string
	Land      string
	DaemonURL string
	GuestTTL  string // "24 hours"
	Login     string
	Until     string // when a joined pass ends, "29 October 2026, 15:20 UTC"; "" when unknown
}

// landsDemoFromEnv builds the door, or returns nil with no error when
// LANDS_DEMO_LAND is unset.
func landsDemoFromEnv(getenv func(string) string, daemon landInvitesAPI, projects purse.Source, machine beadsapi.TokenSource, logger *slog.Logger) (*landsDemo, error) {
	land := strings.TrimSpace(getenv(landsDemoLandEnv))
	if land == "" {
		return nil, nil
	}
	if _, err := keycloakadmin.GroupName(land); err != nil {
		return nil, fmt.Errorf("%s=%q: %w", landsDemoLandEnv, land, err)
	}
	raw := strings.TrimSpace(getenv(landsDemoDaemonURLEnv))
	u, err := url.Parse(raw)
	if raw == "" || err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("%s=%q: want the https URL strangers point gb at", landsDemoDaemonURLEnv, raw)
	}
	d := &landsDemo{
		land: land, daemonURL: strings.TrimRight(raw, "/"), maxGuests: landsDemoMaxGuestsDefault, guestTTL: landsDemoGuestTTLDefault,
		invites: &landInvites{daemon: daemon, machine: machine, projects: projects, logger: logger},
	}
	if v := strings.TrimSpace(getenv(landsDemoMaxGuestsEnv)); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("%s=%q: want a positive integer", landsDemoMaxGuestsEnv, v)
		}
		d.maxGuests = n
	}
	if v := strings.TrimSpace(getenv(landsDemoGuestTTLEnv)); v != "" {
		ttl, err := time.ParseDuration(v)
		if err != nil || ttl < time.Hour {
			return nil, fmt.Errorf("%s=%q: want a duration of at least 1h", landsDemoGuestTTLEnv, v)
		}
		d.guestTTL = ttl
	}
	if d.invites.logger == nil {
		d.invites.logger = slog.Default()
	}
	return d, nil
}

// open reports whether the demo land is a provisioned tenant land that has
// not been closed.
func (d *landsDemo) open(ctx context.Context) (bool, error) {
	rctx, cancel := context.WithTimeout(beadsapi.WithSystemRead(context.WithoutCancel(ctx)), landsGateCallTimeout)
	defer cancel()
	projects, err := purse.TenantProjects(rctx, d.invites.projects)
	if err != nil {
		return false, err
	}
	p := projects[d.land]
	return p != nil && p.Fields["land_opened_at"] != "" && p.Fields["land_closed_at"] == "", nil
}

// passes returns the open passes, not counting ones being revoked, and the
// caller's among them (nil for none).
func (d *landsDemo) passes(ctx context.Context, githubID int64) (int, *openInvite, error) {
	open, err := d.invites.open(ctx, d.land)
	if err != nil {
		return 0, nil, err
	}
	n := 0
	var mine *openInvite
	for i := range open {
		if open[i].Revoking {
			continue
		}
		n++
		if open[i].githubID == githubID && mine == nil {
			mine = &open[i]
		}
	}
	return n, mine, nil
}

// ask files a guest pass for the caller. It returns errLandsDemoFull when
// every pass is taken; a caller who already holds one gets theirs back. The
// in-process lock saves a round trip on a double click; the daemon's create
// is what holds the limits when another replica files at the same time.
func (d *landsDemo) ask(ctx context.Context, by string, githubID int64, login string) (*openInvite, error) {
	d.invites.mu.Lock()
	defer d.invites.mu.Unlock()
	n, mine, err := d.passes(ctx, githubID)
	switch {
	case err != nil:
		return nil, err
	case mine != nil:
		return mine, nil
	case n >= d.maxGuests:
		return nil, errLandsDemoFull
	}
	now := d.invites.clock()
	exp := now.Add(landsDemoJoinWindow)
	raw, err := json.Marshal(map[string]any{
		"land": d.land, "github_id": strconv.FormatInt(githubID, 10), "github_login": login, "role": "guest",
		"invited_by": by, "expires_at": exp.UTC().Format(time.RFC3339), "state": "pending",
		"open_invite_cap": d.maxGuests,
	})
	if err != nil {
		return nil, err
	}
	cctx, cancel := d.invites.callCtx(ctx)
	defer cancel()
	b, err := d.invites.daemon.CreateBeadDetail(cctx, beadsapi.CreateBeadRequest{
		Title: "Demo guest pass for @" + login, Type: landInviteType, Kind: "data", Priority: 3,
		Labels: []string{"project:" + d.land}, Fields: raw, CreatedBy: by,
	})
	if err != nil {
		// Another replica filed first: the daemon refuses at the cap. Say
		// full, or hand back a pass of theirs it filed meanwhile.
		switch n, mine, rerr := d.passes(ctx, githubID); {
		case rerr != nil:
		case mine != nil:
			return mine, nil
		case n >= d.maxGuests:
			return nil, errLandsDemoFull
		}
		return nil, err
	}
	// The daemon hands back the caller's open pass when another replica filed
	// it first, so the pass is read from what it returned.
	if got, id, ok := landInviteFrom(b, d.land); ok && id == githubID {
		d.invites.logger.Info("lands demo: pass filed", "land", d.land, "invite", b.ID, "github_id", githubID, "by", by)
		return &openInvite{Invite: got, githubID: githubID, createdAt: b.CreatedAt}, nil
	}
	return nil, fmt.Errorf("filing a demo pass for github_id %d came back as %s, not that account's invite", githubID, b.ID)
}

// fillLandsDemo works out what the caller may do at the door. It reads the
// caller's terms on their own bearer and the passes as the machine principal.
func (s *Server) fillLandsDemo(r *http.Request, v *landsView) (githubID int64) {
	d := s.landsDemo
	if d == nil {
		v.Demo.State = landsDemoClosed
		return 0
	}
	ctx := r.Context()
	v.Demo.Land, v.Demo.DaemonURL, v.Demo.GuestTTL = d.land, d.daemonURL, landsDemoDuration(d.guestTTL)
	open, err := d.open(ctx)
	switch {
	case err != nil:
		s.logger.Warn("lands demo: reading the demo land failed", "land", d.land, "error", err)
		v.Demo.State = landsDemoUnavailable
		return 0
	case !open:
		v.Demo.State = landsDemoClosed
		return 0
	}
	s.fillLandsAcceptState(ctx, v)
	switch v.State {
	case landsStateNoBearer:
		v.Demo.State = landsDemoNoBearer
		return 0
	case landsStateUnavailable:
		v.Demo.State = landsDemoUnavailable
		return 0
	case landsStateCurrent:
	default:
		v.Demo.State = landsDemoTerms
		return 0
	}
	githubID = oidcauth.GitHubIDFromContext(ctx)
	login := oidcauth.GitHubLoginFromContext(ctx)
	if githubID <= 0 || !landsDemoLogin.MatchString(login) {
		v.Demo.State = landsDemoNoGitHub
		return 0
	}
	v.Demo.Login = login
	n, mine, err := d.passes(ctx, githubID)
	switch {
	case err != nil:
		s.logger.Warn("lands demo: reading passes failed", "land", d.land, "error", err)
		v.Demo.State = landsDemoUnavailable
	case mine != nil && mine.State == "joined":
		v.Demo.State = landsDemoJoined
		if !mine.joinedAt.IsZero() {
			v.Demo.Until = mine.joinedAt.Add(d.guestTTL).UTC().Format("2 January 2006, 15:04 UTC")
		}
	case mine != nil:
		v.Demo.State = landsDemoPending
	case n >= d.maxGuests:
		v.Demo.State = landsDemoFull
	default:
		v.Demo.State = landsDemoOpen
	}
	return githubID
}

// handleLandsDemoPage is GET /lands/demo.
func (s *Server) handleLandsDemoPage(w http.ResponseWriter, r *http.Request) {
	v, ok := s.landsBase(r, "Try the demo")
	if !ok {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	githubID := s.fillLandsDemo(r, &v)
	if r.URL.Query().Get("asked") == "1" && v.Demo.State == landsDemoPending {
		v.Notice = "Your guest pass is on its way. It is usually ready within a minute."
	}
	if v.Demo.State == landsDemoOpen {
		v.Offer = s.landsGatehouseTools.offerFor(r.Context(), githubID, "demo")
	}
	s.renderLands(w, http.StatusOK, "lands_demo", v)
}

// handleLandsDemoAsk is POST /lands/demo: file the caller's guest pass.
func (s *Server) handleLandsDemoAsk(w http.ResponseWriter, r *http.Request) {
	v, ok := s.landsBase(r, "Try the demo")
	if !ok {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	// As POST /lands/accept: SameSite already keeps a cross-site form from
	// arriving signed in; refuse one that says it is cross-site anyway.
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	githubID := s.fillLandsDemo(r, &v)
	switch v.Demo.State {
	case landsDemoPending, landsDemoJoined:
		http.Redirect(w, r, s.basePath+"/lands/demo", http.StatusSeeOther)
		return
	case landsDemoOpen:
	case landsDemoFull:
		s.renderLands(w, http.StatusConflict, "lands_demo", v)
		return
	case landsDemoUnavailable, landsDemoClosed:
		s.renderLands(w, http.StatusServiceUnavailable, "lands_demo", v)
		return
	default:
		s.renderLands(w, http.StatusForbidden, "lands_demo", v)
		return
	}
	by := canonicalOIDC(oidcauth.EmailFromContext(r.Context()))
	if !strings.HasPrefix(by, "oidc:") {
		v.Demo.State = landsDemoNoBearer
		s.renderLands(w, http.StatusForbidden, "lands_demo", v)
		return
	}
	inv, err := s.landsDemo.ask(r.Context(), by, githubID, v.Demo.Login)
	if err != nil {
		if errors.Is(err, errLandsDemoFull) {
			v.Demo.State = landsDemoFull
			s.renderLands(w, http.StatusConflict, "lands_demo", v)
			return
		}
		s.logger.Error("lands demo: filing a pass failed", "land", s.landsDemo.land, "error", err)
		v.Demo.State = landsDemoUnavailable
		s.renderLands(w, http.StatusServiceUnavailable, "lands_demo", v)
		return
	}
	s.landsGatehouseTools.settle(r.Context(), githubID, "demo", inv.ID, "They asked for the pass at /lands/demo: it is filed and is usually ready within a minute.")
	http.Redirect(w, r, s.basePath+"/lands/demo?asked=1", http.StatusSeeOther)
}

// landsDemoDuration renders a pass length in words: "24 hours", "3 days".
func landsDemoDuration(d time.Duration) string {
	h := int(d.Round(time.Hour) / time.Hour)
	switch {
	case h%24 == 0 && h > 24:
		return strconv.Itoa(h/24) + " days"
	case h == 1:
		return "1 hour"
	default:
		return strconv.Itoa(h) + " hours"
	}
}

// landsDemoStates is every state, for the template tests.
var landsDemoStates = []string{
	landsDemoClosed, landsDemoNoBearer, landsDemoTerms, landsDemoNoGitHub, landsDemoUnavailable,
	landsDemoOpen, landsDemoFull, landsDemoPending, landsDemoJoined,
}
