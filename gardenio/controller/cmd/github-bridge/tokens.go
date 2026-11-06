package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/githubapp"
)

// tokenRefreshMargin re-mints a cached token this long before it expires so a
// caller never receives one that dies mid-request.
const tokenRefreshMargin = 5 * time.Minute

// installationMinter is the slice of *githubapp.Client the token source uses
// (a seam for tests).
type installationMinter interface {
	InstallationFor(ctx context.Context, repo githubapp.RepoRef) (int64, error)
	MintInstallationTokenWithPermissions(ctx context.Context, installationID int64, repoNames []string, permissions map[string]string) (githubapp.Token, error)
}

// tokenSource hands out per-repo installation tokens narrowed to one
// permission set (the bridge's, unless withPermissions), caching each until
// shortly before expiry. The underlying
// client fails closed if GitHub returns a token wider than the one asked for.
type tokenSource struct {
	app            installationMinter
	installationID int64 // optional pin; 0 = look up per repo
	now            func() time.Time
	perms          func() map[string]string

	mu    sync.Mutex
	cache map[string]githubapp.Token // lower-cased owner/repo
}

func newTokenSource(app installationMinter, installationID int64, now func() time.Time) *tokenSource {
	if now == nil {
		now = time.Now
	}
	return &tokenSource{app: app, installationID: installationID, now: now, perms: githubapp.BridgeTokenPermissions, cache: map[string]githubapp.Token{}}
}

// withPermissions is a source on the same App and installation that mints
// tokens with perms instead, cached apart.
func (s *tokenSource) withPermissions(perms func() map[string]string) *tokenSource {
	return &tokenSource{app: s.app, installationID: s.installationID, now: s.now, perms: perms, cache: map[string]githubapp.Token{}}
}

// Token returns a token scoped to exactly repo ("owner/name").
func (s *tokenSource) Token(ctx context.Context, repo string) (githubapp.Token, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return githubapp.Token{}, fmt.Errorf("token source: %q is not owner/name", repo)
	}
	key := strings.ToLower(repo)

	s.mu.Lock()
	tok, hit := s.cache[key]
	s.mu.Unlock()
	if hit && s.now().Add(tokenRefreshMargin).Before(tok.ExpiresAt) {
		return tok, nil
	}

	id := s.installationID
	if id == 0 {
		var err error
		id, err = s.app.InstallationFor(ctx, githubapp.RepoRef{Owner: owner, Name: name})
		if err != nil {
			return githubapp.Token{}, err
		}
	}
	tok, err := s.app.MintInstallationTokenWithPermissions(ctx, id, []string{name}, s.perms())
	if err != nil {
		return githubapp.Token{}, err
	}
	if tok.Reveal() == "" {
		return githubapp.Token{}, errors.New("token source: empty token from minter")
	}
	s.mu.Lock()
	s.cache[key] = tok
	s.mu.Unlock()
	return tok, nil
}
