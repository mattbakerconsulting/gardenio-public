package main

import (
	"context"
	"errors"
	"testing"

	"gardenio/controller/internal/githubapp"
)

type fakePRStateApp struct {
	installs    int
	gotInstall  int64
	gotRepo     githubapp.RepoRef
	gotNumber   int
	installErr  error
	state       string
	stateErr    error
	stateCalled bool
}

func (f *fakePRStateApp) InstallationFor(_ context.Context, _ githubapp.RepoRef) (int64, error) {
	f.installs++
	return 777, f.installErr
}

func (f *fakePRStateApp) PullRequestState(_ context.Context, installationID int64, repo githubapp.RepoRef, number int) (string, error) {
	f.stateCalled = true
	f.gotInstall, f.gotRepo, f.gotNumber = installationID, repo, number
	return f.state, f.stateErr
}

func TestPRStateLookup_ParsesURLAndUsesPinnedInstallation(t *testing.T) {
	app := &fakePRStateApp{state: "merged"}
	lookup := newPRStateLookup(app, 42)
	got, err := lookup(context.Background(), "https://github.com/mattbakerconsulting/gardenio/pull/504")
	if err != nil || got != "merged" {
		t.Fatalf("lookup = %q, %v", got, err)
	}
	if app.installs != 0 || app.gotInstall != 42 {
		t.Errorf("pinned installation not used: installs=%d got=%d", app.installs, app.gotInstall)
	}
	if app.gotRepo != (githubapp.RepoRef{Owner: "mattbakerconsulting", Name: "gardenio"}) || app.gotNumber != 504 {
		t.Errorf("asked for %s#%d", app.gotRepo, app.gotNumber)
	}
}

func TestPRStateLookup_LooksUpInstallationWhenUnpinned(t *testing.T) {
	app := &fakePRStateApp{state: "open"}
	if _, err := newPRStateLookup(app, 0)(context.Background(), "https://github.com/o/r/pull/1"); err != nil {
		t.Fatal(err)
	}
	if app.installs != 1 || app.gotInstall != 777 {
		t.Errorf("installs=%d got=%d, want one lookup answering 777", app.installs, app.gotInstall)
	}

	app = &fakePRStateApp{installErr: errors.New("HTTP 404")}
	if _, err := newPRStateLookup(app, 0)(context.Background(), "https://github.com/o/r/pull/1"); err == nil {
		t.Error("an installation lookup failure must be an error")
	}
	if app.stateCalled {
		t.Error("no PR read without an installation")
	}
}

func TestPRStateLookup_RefusesNonPRURL(t *testing.T) {
	app := &fakePRStateApp{state: "open"}
	if _, err := newPRStateLookup(app, 42)(context.Background(), "https://gitlab.com/o/r/-/merge_requests/1"); err == nil {
		t.Error("a non-GitHub URL must be an error")
	}
	if app.stateCalled {
		t.Error("no PR read for a URL that does not parse")
	}
}

func TestPRStateLookup_NilWithoutAFleetApp(t *testing.T) {
	var nilIssuer *githubAppIssuer
	if nilIssuer.prStateLookup() != nil {
		t.Error("nil issuer must give no lookup")
	}
	if (&githubAppIssuer{fleetOff: true}).prStateLookup() != nil {
		t.Error("fleet App off must give no lookup")
	}
	if (&githubAppIssuer{initErr: errors.New("no key")}).prStateLookup() != nil {
		t.Error("misconfigured fleet App must give no lookup")
	}
}
