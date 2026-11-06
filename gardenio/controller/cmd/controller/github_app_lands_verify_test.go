package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/githubapp"
	"gardenio/controller/internal/podmanager"
)

// Lands mint verification (gd-Ld5r7Ok2rK review): before a Lands mint or
// refresh the pinned installation is read back (verifyLandsInstallation). The
// fakes and env live in github_app_credentials_test.go.

// gd-Ld5r7Ok2rK review: the pinned installation and git_url are both project
// fields, so a Lands mint first reads the installation back and refuses one on
// any account but the repo's owner (or a suspended one). Nothing is minted.
func TestProvision_LandsInstallationMustBeOnTheRepoOwner(t *testing.T) {
	for name, tc := range map[string]struct {
		account   string
		suspended bool
		refuse    bool
	}{
		"owner":             {account: "applicant"},
		"owner, other case": {account: "Applicant"},
		"another account":   {account: "joyousaversion", refuse: true},
		"no account":        {account: "", refuse: true},
		"suspended":         {account: "applicant", suspended: true, refuse: true},
	} {
		e, lands := newGHTwoAppEnv(t)
		lands.account, lands.suspended = tc.account, tc.suspended
		spec := tenantSpec()
		refuse, reason := e.iss.applyToSpec(context.Background(), &spec, "gd-t1")
		if lands.instReads != 1 {
			t.Errorf("%s: installation reads = %d, want 1", name, lands.instReads)
		}
		if refuse != tc.refuse {
			t.Fatalf("%s: refuse = %v (%s), want %v", name, refuse, reason, tc.refuse)
		}
		if !tc.refuse {
			continue
		}
		if lands.n != 0 || e.app.n != 0 {
			t.Fatalf("%s: nothing may be minted (lands %d, fleet %d)", name, lands.n, e.app.n)
		}
		if spec.GitCredentialsSecret != "" || getSecretOrNil(e.k8s, "land-tomato-patch", "agent-github-token-gd-t1") != nil {
			t.Fatalf("%s: no credential may be handed out", name)
		}
	}
}

// gd-Ld5r7Ok2rK review: the installation must also be the one GitHub places
// the repo in. An installation on the owner's account that was never granted
// the repo (a selected-repositories install that leaves it out: GitHub's 404),
// or a repo that sits in a different installation, is refused before any mint.
func TestProvision_LandsRepoMustBeInThePinnedInstallation(t *testing.T) {
	for name, tc := range map[string]struct {
		repoInstalls map[string]int64
		refuse       bool
	}{
		"in the pin":                   {repoInstalls: map[string]int64{"applicant/tomatoes": 9001}},
		"not in any installation":      {repoInstalls: map[string]int64{}, refuse: true},
		"in another installation":      {repoInstalls: map[string]int64{"applicant/tomatoes": 9002}, refuse: true},
		"only another repo is granted": {repoInstalls: map[string]int64{"applicant/private-notes": 9001}, refuse: true},
	} {
		e, lands := newGHTwoAppEnv(t)
		lands.repoInstalls = tc.repoInstalls
		spec := tenantSpec()
		refuse, reason := e.iss.applyToSpec(context.Background(), &spec, "gd-t1")
		if lands.repoReads != 1 {
			t.Errorf("%s: repo installation reads = %d, want 1", name, lands.repoReads)
		}
		if refuse != tc.refuse {
			t.Fatalf("%s: refuse = %v (%s), want %v", name, refuse, reason, tc.refuse)
		}
		if !tc.refuse {
			if len(lands.installs) != 1 || lands.installs[0] != 9001 {
				t.Fatalf("%s: mints = %v, want one from 9001", name, lands.installs)
			}
			continue
		}
		if lands.n != 0 || e.app.n != 0 {
			t.Fatalf("%s: nothing may be minted (lands %d, fleet %d)", name, lands.n, e.app.n)
		}
		if spec.GitCredentialsSecret != "" || getSecretOrNil(e.k8s, "land-tomato-patch", "agent-github-token-gd-t1") != nil {
			t.Fatalf("%s: no credential may be handed out", name)
		}
	}
}

// The repo check runs on refresh too: a repo dropped from the installation
// after the spawn gets no new token, and the old one lapses.
func TestSweep_LandsRefreshRefusesRepoOutsideInstallation(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	e, lands := newGHTwoAppEnv(t, tenantGithubSecret("gd-lands", now.Add(10*time.Minute), "lands"))
	lands.repoInstalls = map[string]int64{}
	d := &fakeJanitorDaemon{beads: map[string]*beadsapi.BeadDetail{"gd-lands": {Status: "in_progress"}}}
	if _, err := e.k8s.CoreV1().Pods("land-tomato-patch").Create(context.Background(), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "pod-gd-lands", Namespace: "land-tomato-patch",
		Labels:      map[string]string{podmanager.LabelAgent: "x"},
		Annotations: map[string]string{podmanager.AnnotationBeadID: "gd-lands"},
	}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if st := e.iss.sweep(context.Background(), d, []string{"land-tomato-patch"}); st.Refreshed != 0 {
		t.Fatalf("stats = %+v, want no refresh", st)
	}
	if lands.repoReads != 1 || lands.n != 0 {
		t.Fatalf("repo installation reads %d, mints %d; want 1 read and no mint", lands.repoReads, lands.n)
	}

	// Control: with the repo in the pinned installation the same sweep refreshes.
	lands.repoInstalls = map[string]int64{"applicant/tomatoes": 9001}
	if st := e.iss.sweep(context.Background(), d, []string{"land-tomato-patch"}); st.Refreshed != 1 {
		t.Fatalf("stats = %+v, want one refresh", st)
	}
}

// The fleet App resolves its installation from the repo itself, so it is not
// read back.
func TestProvision_FleetMintDoesNotReadInstallation(t *testing.T) {
	e, _ := newGHTwoAppEnv(t)
	spec := ghSpec()
	if refuse, reason := e.iss.applyToSpec(context.Background(), &spec, "gd-op"); refuse {
		t.Fatal(reason)
	}
	if e.app.instReads != 0 || e.app.repoReads != 0 {
		t.Fatalf("fleet installation reads = %d, repo installation reads = %d", e.app.instReads, e.app.repoReads)
	}
}

func TestSweep_LandsRefreshRefusesInstallationOnAnotherAccount(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	e, lands := newGHTwoAppEnv(t, tenantGithubSecret("gd-lands", now.Add(10*time.Minute), "lands"))
	lands.account = "someone-else"
	d := &fakeJanitorDaemon{beads: map[string]*beadsapi.BeadDetail{"gd-lands": {Status: "in_progress"}}}
	if _, err := e.k8s.CoreV1().Pods("land-tomato-patch").Create(context.Background(), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "pod-gd-lands", Namespace: "land-tomato-patch",
		Labels:      map[string]string{podmanager.LabelAgent: "x"},
		Annotations: map[string]string{podmanager.AnnotationBeadID: "gd-lands"},
	}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if st := e.iss.sweep(context.Background(), d, []string{"land-tomato-patch"}); st.Refreshed != 0 {
		t.Fatalf("stats = %+v, want no refresh", st)
	}
	if lands.instReads != 1 || lands.n != 0 {
		t.Fatalf("installation reads %d, mints %d; want 1 read and no mint", lands.instReads, lands.n)
	}
}

// gd-72m3STiqya: a Lands installation on an operator org never mints, even
// when an admin pinned it and it really does cover the repo. The repo's owner
// is refused before GitHub is asked; the installation's account is refused
// after it is read back. GitHub logins are case-insensitive.
func TestProvision_LandsRefusesOperatorOrg(t *testing.T) {
	for name, tc := range map[string]struct {
		orgs      string
		instReads int
		refuse    bool
	}{
		"not listed":      {orgs: "mattbakerconsulting", instReads: 1},
		"no denylist":     {orgs: "", instReads: 1},
		"owner listed":    {orgs: " MattBakerConsulting , Applicant ,", refuse: true},
		"owner, any case": {orgs: "APPLICANT", refuse: true},
	} {
		e, lands := newGHTwoAppEnv(t)
		e.iss.operatorOrgs = parseOperatorOrgs(tc.orgs)
		spec := tenantSpec()
		refuse, reason := e.iss.applyToSpec(context.Background(), &spec, "gd-t1")
		if refuse != tc.refuse {
			t.Fatalf("%s: refuse = %v (%s), want %v", name, refuse, reason, tc.refuse)
		}
		if lands.instReads != tc.instReads {
			t.Errorf("%s: installation reads = %d, want %d", name, lands.instReads, tc.instReads)
		}
		if !tc.refuse {
			continue
		}
		if !strings.Contains(reason, "operator GitHub account") {
			t.Errorf("%s: reason %q does not name the operator org", name, reason)
		}
		if lands.n != 0 || e.app.n != 0 {
			t.Fatalf("%s: nothing may be minted (lands %d, fleet %d)", name, lands.n, e.app.n)
		}
		if spec.GitCredentialsSecret != "" || getSecretOrNil(e.k8s, "land-tomato-patch", "agent-github-token-gd-t1") != nil {
			t.Fatalf("%s: no credential may be handed out", name)
		}
	}
}

// The installation's own account is checked against the denylist, not only
// the repo owner: an installation GitHub reports on an operator org is named
// as such.
func TestVerifyLandsInstallation_OperatorOrgAccount(t *testing.T) {
	orgs := parseOperatorOrgs("mattbakerconsulting,claimlayer")
	app := &fakeGithubApp{account: "ClaimLayer", repoInstalls: map[string]int64{"applicant/tomatoes": 9001}}
	err := verifyLandsInstallation(context.Background(), app, 9001, githubapp.RepoRef{Owner: "applicant", Name: "tomatoes"}, orgs, 0)
	if err == nil || !strings.Contains(err.Error(), `operator GitHub account "ClaimLayer"`) {
		t.Fatalf("err = %v, want the operator-org refusal", err)
	}
	if app.repoReads != 0 {
		t.Fatalf("repo installation reads = %d, want 0 after the refusal", app.repoReads)
	}

	app = &fakeGithubApp{account: "mattbakerconsulting", repoInstalls: map[string]int64{"mattbakerconsulting/gardenio": 9001}}
	err = verifyLandsInstallation(context.Background(), app, 9001, githubapp.RepoRef{Owner: "MattBakerConsulting", Name: "gardenio"}, orgs, 0)
	if err == nil || !strings.Contains(err.Error(), "operator GitHub account") {
		t.Fatalf("err = %v, want the operator-org refusal", err)
	}
	if app.instReads != 0 {
		t.Fatalf("installation reads = %d, want 0: an operator repo is refused before GitHub is asked", app.instReads)
	}
}

// The refresh path refuses too: a live Lands token whose land now names an
// operator org is not re-minted and lapses.
func TestSweep_LandsRefreshRefusesOperatorOrg(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	e, lands := newGHTwoAppEnv(t, tenantGithubSecret("gd-lands", now.Add(10*time.Minute), "lands"))
	e.iss.operatorOrgs = parseOperatorOrgs("applicant")
	d := &fakeJanitorDaemon{beads: map[string]*beadsapi.BeadDetail{"gd-lands": {Status: "in_progress"}}}
	if _, err := e.k8s.CoreV1().Pods("land-tomato-patch").Create(context.Background(), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "pod-gd-lands", Namespace: "land-tomato-patch",
		Labels:      map[string]string{podmanager.LabelAgent: "x"},
		Annotations: map[string]string{podmanager.AnnotationBeadID: "gd-lands"},
	}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if st := e.iss.sweep(context.Background(), d, []string{"land-tomato-patch"}); st.Refreshed != 0 {
		t.Fatalf("stats = %+v, want no refresh", st)
	}
	if lands.n != 0 {
		t.Fatalf("mints = %d, want none", lands.n)
	}

	// Control: off the denylist the same sweep refreshes.
	e.iss.operatorOrgs = parseOperatorOrgs("mattbakerconsulting")
	if st := e.iss.sweep(context.Background(), d, []string{"land-tomato-patch"}); st.Refreshed != 1 {
		t.Fatalf("stats = %+v, want one refresh", st)
	}
}

// A repository is held to the size limit at every Lands mint, not only at
// setup: a tenant can push gigabytes after approval (gd-i74esUHCTg item 4).
// A size GitHub will not report refuses too, and 0 checks nothing.
func TestProvision_LandsRefusesRepoOverSizeLimit(t *testing.T) {
	for name, tc := range map[string]struct {
		limitKB, sizeKB int64
		sizeErr         error
		reads           int
		refuse          bool
	}{
		"within the limit": {limitKB: 1024 * 1024, sizeKB: 1024, reads: 1},
		"at the limit":     {limitKB: 1024, sizeKB: 1024, reads: 1},
		"over the limit":   {limitKB: 1024, sizeKB: 5 * 1024 * 1024, reads: 1, refuse: true},
		"size unreadable":  {limitKB: 1024, sizeErr: errors.New("boom"), reads: 1, refuse: true},
		"no limit":         {limitKB: 0, sizeKB: 5 * 1024 * 1024},
	} {
		e, lands := newGHTwoAppEnv(t)
		e.iss.maxRepoSizeKB = tc.limitKB
		lands.sizeKB, lands.sizeErr = tc.sizeKB, tc.sizeErr
		spec := tenantSpec()
		refuse, reason := e.iss.applyToSpec(context.Background(), &spec, "gd-t1")
		if refuse != tc.refuse {
			t.Fatalf("%s: refuse = %v (%s), want %v", name, refuse, reason, tc.refuse)
		}
		if lands.sizeReads != tc.reads {
			t.Errorf("%s: size reads = %d, want %d", name, lands.sizeReads, tc.reads)
		}
		if tc.refuse && (lands.n != 0 || spec.GitCredentialsSecret != "") {
			t.Fatalf("%s: a token was handed out (mints %d, secret %q)", name, lands.n, spec.GitCredentialsSecret)
		}
	}
}

// The refresh path holds the limit too: a live Lands token on a repo that
// grew past it is not re-minted and lapses.
func TestSweep_LandsRefreshRefusesRepoOverSizeLimit(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	e, lands := newGHTwoAppEnv(t, tenantGithubSecret("gd-lands", now.Add(10*time.Minute), "lands"))
	e.iss.maxRepoSizeKB = 1024
	lands.sizeKB = 5 * 1024 * 1024
	d := &fakeJanitorDaemon{beads: map[string]*beadsapi.BeadDetail{"gd-lands": {Status: "in_progress"}}}
	if _, err := e.k8s.CoreV1().Pods("land-tomato-patch").Create(context.Background(), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "pod-gd-lands", Namespace: "land-tomato-patch",
		Labels:      map[string]string{podmanager.LabelAgent: "x"},
		Annotations: map[string]string{podmanager.AnnotationBeadID: "gd-lands"},
	}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if st := e.iss.sweep(context.Background(), d, []string{"land-tomato-patch"}); st.Refreshed != 0 {
		t.Fatalf("stats = %+v, want no refresh", st)
	}
	if lands.n != 0 {
		t.Fatalf("mints = %d, want none", lands.n)
	}

	lands.sizeKB = 512
	if st := e.iss.sweep(context.Background(), d, []string{"land-tomato-patch"}); st.Refreshed != 1 {
		t.Fatalf("stats = %+v, want one refresh once the repo is within the limit", st)
	}
}
