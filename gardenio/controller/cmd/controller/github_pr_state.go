package main

import (
	"context"
	"errors"
	"fmt"

	"gardenio/controller/internal/githubapp"
	"gardenio/controller/internal/statusreporter"
)

// githubPRStateAPI is the slice of *githubapp.Client the stale reaper's PR
// state reader uses; faked in tests.
type githubPRStateAPI interface {
	InstallationFor(ctx context.Context, repo githubapp.RepoRef) (int64, error)
	PullRequestState(ctx context.Context, installationID int64, repo githubapp.RepoRef, number int) (string, error)
}

// prStateLookup returns the stale reaper's GitHub PR state reader, backed by
// the fleet App (gd-ovzZSuuHny), or nil when the fleet App cannot mint: the
// reaper then flags every task with a linked PR, as it did before. A PR in a
// repository the fleet App is not installed on (a tenant land's) fails the
// lookup, and the reaper flags that task too.
func (g *githubAppIssuer) prStateLookup() statusreporter.PRStateLookup {
	if g == nil || g.fleetOff || g.initErr != nil || g.app == nil {
		return nil
	}
	app, ok := g.app.(githubPRStateAPI)
	if !ok {
		return nil
	}
	return newPRStateLookup(app, g.installationID)
}

func newPRStateLookup(app githubPRStateAPI, pinnedInstallationID int64) statusreporter.PRStateLookup {
	return func(ctx context.Context, prURL string) (string, error) {
		repo, number, ok := githubapp.ParsePullRequestURL(prURL, githubHost)
		if !ok {
			return "", errors.New("not a github.com pull request URL")
		}
		ctx, cancel := context.WithTimeout(ctx, githubOpTimeout)
		defer cancel()
		installationID := pinnedInstallationID
		if installationID == 0 {
			id, err := app.InstallationFor(ctx, repo)
			if err != nil {
				return "", fmt.Errorf("installation for %s: %w", repo, err)
			}
			installationID = id
		}
		return app.PullRequestState(ctx, installationID, repo, number)
	}
}
