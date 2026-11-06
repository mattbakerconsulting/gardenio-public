package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"gardenio/controller/internal/githubapp"
)

type fakeMinter struct {
	lookups int
	mints   []mintCall
	ttl     time.Duration
	now     func() time.Time
	err     error
}

type mintCall struct {
	install int64
	repos   []string
	perms   map[string]string
}

func (f *fakeMinter) InstallationFor(_ context.Context, repo githubapp.RepoRef) (int64, error) {
	f.lookups++
	return 42, nil
}

func (f *fakeMinter) MintInstallationTokenWithPermissions(_ context.Context, id int64, repos []string, perms map[string]string) (githubapp.Token, error) {
	if f.err != nil {
		return githubapp.Token{}, f.err
	}
	f.mints = append(f.mints, mintCall{id, repos, perms})
	return githubapp.NewToken("ghs_fake", f.now().Add(f.ttl)), nil
}

func TestTokenSource_ScopesToRepoAndBridgePermissions(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	f := &fakeMinter{ttl: time.Hour, now: func() time.Time { return now }}
	s := newTokenSource(f, 0, func() time.Time { return now })
	if _, err := s.Token(context.Background(), "joyousaversion/gardenio"); err != nil {
		t.Fatal(err)
	}
	m := f.mints[0]
	if m.install != 42 || len(m.repos) != 1 || m.repos[0] != "gardenio" {
		t.Errorf("mint = %+v", m)
	}
	if m.perms["contents"] != "read" || m.perms["pull_requests"] != "write" {
		t.Errorf("perms = %v", m.perms)
	}
	if m.perms["contents"] == "write" || m.perms["workflows"] != "" {
		t.Errorf("bridge token must not be able to push code: %v", m.perms)
	}
}

func TestTokenSource_CachesUntilRefreshMargin(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	f := &fakeMinter{ttl: time.Hour, now: func() time.Time { return now }}
	s := newTokenSource(f, 0, func() time.Time { return now })
	ctx := context.Background()
	s.Token(ctx, "o/r")
	s.Token(ctx, "O/R") // same repo, different case
	if len(f.mints) != 1 || f.lookups != 1 {
		t.Fatalf("mints=%d lookups=%d, want 1/1", len(f.mints), f.lookups)
	}
	now = now.Add(time.Hour - tokenRefreshMargin + time.Second)
	s.Token(ctx, "o/r")
	if len(f.mints) != 2 {
		t.Errorf("token inside the refresh margin must be re-minted, mints=%d", len(f.mints))
	}
	s.Token(ctx, "o/other")
	if len(f.mints) != 3 {
		t.Errorf("a different repo needs its own token, mints=%d", len(f.mints))
	}
}

func TestTokenSource_PinnedInstallationSkipsLookup(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	f := &fakeMinter{ttl: time.Hour, now: func() time.Time { return now }}
	s := newTokenSource(f, 7, func() time.Time { return now })
	s.Token(context.Background(), "o/r")
	if f.lookups != 0 || f.mints[0].install != 7 {
		t.Errorf("lookups=%d install=%d", f.lookups, f.mints[0].install)
	}
}

func TestTokenSource_RejectsMalformedRepoAndPropagatesErrors(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	f := &fakeMinter{ttl: time.Hour, now: func() time.Time { return now }}
	s := newTokenSource(f, 0, func() time.Time { return now })
	for _, bad := range []string{"", "noslash", "/r", "o/", "a/b/c"} {
		if _, err := s.Token(context.Background(), bad); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
	if f.lookups != 0 {
		t.Error("a malformed repo must not reach GitHub")
	}
	f.err = errors.New("HTTP 403: nope")
	if _, err := s.Token(context.Background(), "o/r"); err == nil {
		t.Error("mint failure must propagate")
	}
}
