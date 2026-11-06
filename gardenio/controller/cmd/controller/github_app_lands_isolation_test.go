package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/config"
	"gardenio/controller/internal/lands/wall"
	"gardenio/controller/internal/pausegate"
	"gardenio/controller/internal/podmanager"
)

// Two-land isolation and the kill switches for Lands App tokens
// (gd-Ld5r7Ok2rK third review). The fakes and env live in
// github_app_credentials_test.go; the endpoint half of the two-land test is
// kbeads' TestLandMintFields_TwoLands.

// newGHTwoLandEnv is newGHTwoAppEnv with a second tenant land: A is
// "tomato-patch" (applicant/tomatoes on installation 9001) and B is
// "bean-row" (grower/beans on installation 9002), both on the one Lands App.
func newGHTwoLandEnv(t *testing.T, objs ...runtime.Object) (*ghTestEnv, *fakeGithubApp) {
	t.Helper()
	e, lands := newGHTwoAppEnv(t, objs...)
	lands.account = ""
	lands.accounts = map[int64]string{9001: "applicant", 9002: "grower"}
	lands.repoInstalls = map[string]int64{"applicant/tomatoes": 9001, "grower/beans": 9002}
	b := ghTenant()
	b.GithubInstallationID = 9002
	e.iss.project = ghProjects(map[string]config.ProjectCacheEntry{"gardenio": {}, "tomato-patch": ghTenant(), "bean-row": b})
	return e, lands
}

func landBSpec() podmanager.AgentPodSpec {
	return podmanager.AgentPodSpec{
		Project: "bean-row", Role: "crew", AgentName: "b1", Namespace: "land-bean-row",
		GitURL: "https://github.com/grower/beans.git",
	}
}

func addLandAgentPod(t *testing.T, e *ghTestEnv, ns, bead string) {
	t.Helper()
	if _, err := e.k8s.CoreV1().Pods(ns).Create(context.Background(), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "pod-" + bead, Namespace: ns,
		Labels:      map[string]string{podmanager.LabelAgent: "x"},
		Annotations: map[string]string{podmanager.AnnotationBeadID: bead},
	}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}

// Each land's agent gets a token for its own repo from its own installation,
// in its own namespace, and nothing of the other land's.
func TestTwoLands_EachAgentGetsOnlyItsOwnLandsToken(t *testing.T) {
	e, lands := newGHTwoLandEnv(t)
	a, b := tenantSpec(), landBSpec()
	if refuse, reason := e.iss.applyToSpec(context.Background(), &a, "gd-a1"); refuse {
		t.Fatal(reason)
	}
	if refuse, reason := e.iss.applyToSpec(context.Background(), &b, "gd-b1"); refuse {
		t.Fatal(reason)
	}
	if len(lands.minted) != 2 || strings.Join(lands.minted[0], ",") != "tomatoes" || lands.installs[0] != 9001 ||
		strings.Join(lands.minted[1], ",") != "beans" || lands.installs[1] != 9002 {
		t.Fatalf("mints = %v from %v, want [tomatoes] from 9001 then [beans] from 9002", lands.minted, lands.installs)
	}
	for _, c := range []struct{ ns, bead, repos, inst string }{
		{"land-tomato-patch", "gd-a1", "applicant/tomatoes", "9001"},
		{"land-bean-row", "gd-b1", "grower/beans", "9002"},
	} {
		sec := getSecret(t, e.k8s, c.ns, githubTokenSecretName(c.bead))
		if sec.Annotations[annotationGithubRepos] != c.repos || sec.Annotations[annotationGithubInstallation] != c.inst {
			t.Errorf("%s/%s: repos %q installation %q, want %q %q", c.ns, sec.Name,
				sec.Annotations[annotationGithubRepos], sec.Annotations[annotationGithubInstallation], c.repos, c.inst)
		}
	}
	if getSecretOrNil(e.k8s, "land-bean-row", githubTokenSecretName("gd-a1")) != nil ||
		getSecretOrNil(e.k8s, "land-tomato-patch", githubTokenSecretName("gd-b1")) != nil {
		t.Error("a land's token Secret must only be written in its own namespace")
	}
}

// An agent in A cannot reach B's repo through the controller: pointing A's
// spawn at B's repo, or A's pin at B's installation, is refused before any
// mint. (A project bead rewritten to B's repo and pin together is the daemon's
// to refuse: only an admin or the Lands provisioner writes those fields.)
func TestTwoLands_AgentInACannotGetBsTokenThroughTheController(t *testing.T) {
	for name, tc := range map[string]struct {
		gitURL string
		pin    int64
	}{
		"A's spawn names B's repo":                 {gitURL: "https://github.com/grower/beans.git", pin: 9001},
		"A pinned to B's installation":             {gitURL: "https://github.com/applicant/tomatoes.git", pin: 9002},
		"A's spawn names B's repo, B's lower case": {gitURL: "https://github.com/Grower/Beans", pin: 9001},
	} {
		t.Run(name, func(t *testing.T) {
			e, lands := newGHTwoLandEnv(t)
			a := ghTenant()
			a.GithubInstallationID = tc.pin
			b := ghTenant()
			b.GithubInstallationID = 9002
			e.iss.project = ghProjects(map[string]config.ProjectCacheEntry{"tomato-patch": a, "bean-row": b})
			spec := tenantSpec()
			spec.GitURL = tc.gitURL
			refuse, reason := e.iss.applyToSpec(context.Background(), &spec, "gd-a1")
			if !refuse {
				t.Fatal("A's agent was handed a token toward B")
			}
			if lands.n != 0 || e.app.n != 0 {
				t.Fatalf("nothing may be minted (lands %d, fleet %d): %s", lands.n, e.app.n, reason)
			}
			if spec.GitCredentialsSecret != "" || getSecretOrNil(e.k8s, "land-tomato-patch", githubTokenSecretName("gd-a1")) != nil {
				t.Fatal("no credential may be handed out")
			}
		})
	}
}

// A spawn in A carrying B's agent bead id gets A's own token in A's
// namespace. B's Secret, in B's namespace, is neither read into A's pod nor
// revoked.
func TestTwoLands_SpawnInAWithBsBeadIDNeverTouchesBsSecret(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	bsec := tenantGithubSecret("gd-b1", now.Add(50*time.Minute), "lands")
	bsec.Namespace = "land-bean-row"
	bsec.Labels[podmanager.LabelProject] = "bean-row"
	bsec.Annotations[annotationGithubRepos] = "grower/beans"
	bsec.Annotations[annotationGithubInstallation] = "9002"
	e, lands := newGHTwoLandEnv(t, bsec)
	spec := tenantSpec()
	if refuse, reason := e.iss.applyToSpec(context.Background(), &spec, "gd-b1"); refuse {
		t.Fatal(reason)
	}
	if len(lands.installs) != 1 || lands.installs[0] != 9001 || len(lands.revoked) != 0 {
		t.Fatalf("installs %v revoked %v; want one mint from A's 9001 and no revoke", lands.installs, lands.revoked)
	}
	if got := getSecret(t, e.k8s, "land-tomato-patch", githubTokenSecretName("gd-b1")); got.Annotations[annotationGithubRepos] != "applicant/tomatoes" {
		t.Errorf("A's Secret covers %q", got.Annotations[annotationGithubRepos])
	}
	if got := getSecret(t, e.k8s, "land-bean-row", githubTokenSecretName("gd-b1")); string(got.Data["token"]) != "ghs_OLD_gd-b1" {
		t.Error("B's Secret was rewritten")
	}
}

// The sweep refreshes each land from its own installation, and a Secret whose
// token came from another installation than the land's pin is left to lapse.
func TestTwoLands_SweepRefreshesEachLandFromItsOwnInstallation(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	bsec := tenantGithubSecret("gd-b1", now.Add(10*time.Minute), "lands")
	bsec.Namespace = "land-bean-row"
	bsec.Labels[podmanager.LabelProject] = "bean-row"
	bsec.Annotations[annotationGithubRepos] = "grower/beans"
	bsec.Annotations[annotationGithubInstallation] = "9002"
	stray := tenantGithubSecret("gd-a2", now.Add(10*time.Minute), "lands")
	stray.Annotations[annotationGithubInstallation] = "9002" // A's land, B's installation
	e, lands := newGHTwoLandEnv(t, tenantGithubSecret("gd-a1", now.Add(10*time.Minute), "lands"), bsec, stray)
	addLandAgentPod(t, e, "land-tomato-patch", "gd-a1")
	addLandAgentPod(t, e, "land-tomato-patch", "gd-a2")
	addLandAgentPod(t, e, "land-bean-row", "gd-b1")
	d := &fakeJanitorDaemon{beads: map[string]*beadsapi.BeadDetail{
		"gd-a1": {Status: "in_progress"}, "gd-a2": {Status: "in_progress"}, "gd-b1": {Status: "in_progress"},
	}}
	if st := e.iss.sweep(context.Background(), d, []string{"land-tomato-patch", "land-bean-row"}); st.Refreshed != 2 {
		t.Fatalf("stats = %+v, want A's and B's own tokens refreshed", st)
	}
	if len(lands.minted) != 2 || strings.Join(lands.minted[0], ",") != "tomatoes" || lands.installs[0] != 9001 ||
		strings.Join(lands.minted[1], ",") != "beans" || lands.installs[1] != 9002 {
		t.Fatalf("refreshes = %v from %v", lands.minted, lands.installs)
	}
	if got := getSecret(t, e.k8s, "land-tomato-patch", githubTokenSecretName("gd-a2")); got.StringData["token"] != "" {
		t.Error("a token from another installation than the land's pin must be left to lapse")
	}
}

// Review item D: a live Lands Secret is reused only when it came from the
// pinned installation, and only after that installation is read back for the
// repo. A token from another installation (a repin) is retired and replaced.
func TestProvision_LandsReuseNeedsThePinnedInstallation(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)

	t.Run("same installation is reused after verification", func(t *testing.T) {
		e, lands := newGHTwoLandEnv(t, tenantGithubSecret("gd-a1", now.Add(50*time.Minute), "lands"))
		spec := tenantSpec()
		if refuse, reason := e.iss.applyToSpec(context.Background(), &spec, "gd-a1"); refuse {
			t.Fatal(reason)
		}
		if lands.n != 0 || len(lands.revoked) != 0 {
			t.Fatalf("mints %d revokes %v, want the live token reused", lands.n, lands.revoked)
		}
		if lands.instReads != 1 || lands.repoReads != 1 {
			t.Fatalf("installation reads %d, repo reads %d; reuse must verify the pin", lands.instReads, lands.repoReads)
		}
	})

	t.Run("another installation is replaced", func(t *testing.T) {
		old := tenantGithubSecret("gd-a1", now.Add(50*time.Minute), "lands")
		old.Annotations[annotationGithubInstallation] = "9002"
		e, lands := newGHTwoLandEnv(t, old)
		spec := tenantSpec()
		if refuse, reason := e.iss.applyToSpec(context.Background(), &spec, "gd-a1"); refuse {
			t.Fatal(reason)
		}
		if len(lands.installs) != 1 || lands.installs[0] != 9001 {
			t.Fatalf("installs = %v, want a fresh token from the pinned 9001", lands.installs)
		}
		if len(lands.revoked) != 1 || lands.revoked[0] != "ghs_OLD_gd-a1" {
			t.Fatalf("revoked = %v, want the other installation's token retired", lands.revoked)
		}
		if got := getSecret(t, e.k8s, "land-tomato-patch", githubTokenSecretName("gd-a1")); got.Annotations[annotationGithubInstallation] != "9001" {
			t.Errorf("installation annotation = %q", got.Annotations[annotationGithubInstallation])
		}
	})

	t.Run("a live token is not handed out once the repo leaves the installation", func(t *testing.T) {
		e, lands := newGHTwoLandEnv(t, tenantGithubSecret("gd-a1", now.Add(50*time.Minute), "lands"))
		delete(lands.repoInstalls, "applicant/tomatoes")
		spec := tenantSpec()
		if refuse, _ := e.iss.applyToSpec(context.Background(), &spec, "gd-a1"); !refuse || spec.GitCredentialsSecret != "" {
			t.Fatalf("refuse=%v secret=%q; the reuse path must not skip verification", refuse, spec.GitCredentialsSecret)
		}
	})
}

// haltFarm is a wall daemon with tenant land A and the operator's gardenio.
func haltFarm(frozen string) *wallDaemon {
	return &wallDaemon{beads: []*beadsapi.BeadDetail{
		{ID: "kd-1", Type: "project", Title: "tomato-patch", Status: "open", Fields: map[string]string{"land_kind": "tenant", "land_frozen": frozen}},
		{ID: "kd-2", Type: "project", Title: "gardenio", Status: "open", Fields: map[string]string{}},
	}}
}

func pausedFarm() *wallDaemon {
	d := haltFarm("false")
	d.beads = append(d.beads, &beadsapi.BeadDetail{ID: "kd-g", Type: "gate", Title: pausegate.LandsPauseTitle, Status: "open"})
	return d
}

func haltCases() map[string]struct {
	d    *wallDaemon
	code string
} {
	return map[string]struct {
		d    *wallDaemon
		code string
	}{
		"frozen":          {haltFarm("true"), wall.CodeFrozen},
		"lands paused":    {pausedFarm(), wall.CodeLandsPaused},
		"gate unreadable": {&wallDaemon{beads: haltFarm("false").beads, gateErr: errors.New("daemon down")}, wall.CodeLandsPaused},
		"land unreadable": {&wallDaemon{err: errors.New("daemon down")}, wall.CodePurseUnknown},
		"thawed, no gate": {haltFarm("false"), ""},
	}
}

// landHaltRefusal reads only the kill switches: the purse and the tenant
// spawns setting (neither is set up in these farms) do not refuse, and a
// cached operator land is never read.
func TestLandHaltRefusal(t *testing.T) {
	cfg := wallConfig(map[string]string{"tomato-patch": "tenant", "gardenio": ""})
	for name, tc := range haltCases() {
		if r := landHaltRefusal(cfg, tc.d)(context.Background(), "tomato-patch"); r.Code != tc.code {
			t.Errorf("%s: code %q (%s), want %q", name, r.Code, r.Reason, tc.code)
		}
	}
	d := pausedFarm()
	if r := landHaltRefusal(cfg, d)(context.Background(), "gardenio"); r.Refused() || d.reads != 0 {
		t.Errorf("operator land: %+v after %d reads, want no refusal and no read", r, d.reads)
	}
}

// Review item C at the mint: a frozen, paused or unreadable land gets no
// token, new or reused; the operator's projects are unaffected.
func TestProvision_HaltedLandGetsNoToken(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	cfg := wallConfig(map[string]string{"tomato-patch": "tenant", "gardenio": ""})
	for name, tc := range haltCases() {
		t.Run(name, func(t *testing.T) {
			e, lands := newGHTwoLandEnv(t, tenantGithubSecret("gd-a1", now.Add(50*time.Minute), "lands"))
			e.iss.landHalt = landHaltRefusal(cfg, tc.d)
			spec := tenantSpec()
			refuse, reason := e.iss.applyToSpec(context.Background(), &spec, "gd-a1")
			if refuse != (tc.code != "") {
				t.Fatalf("refuse = %v (%s), want %v", refuse, reason, tc.code != "")
			}
			if !refuse {
				return
			}
			if !strings.Contains(reason, tc.code) || lands.n != 0 || lands.instReads != 0 || spec.GitCredentialsSecret != "" {
				t.Fatalf("reason %q, mints %d, installation reads %d, secret %q", reason, lands.n, lands.instReads, spec.GitCredentialsSecret)
			}
			op := ghSpec()
			if refuse, reason := e.iss.applyToSpec(context.Background(), &op, "gd-op"); refuse {
				t.Fatalf("operator project refused while a land is halted: %s", reason)
			}
		})
	}
}

// Review item C at the sweep: a halted land's due token is not refreshed and
// lapses; once the switch is lifted the next sweep refreshes it.
func TestSweep_HaltedLandIsNotRefreshed(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	cfg := wallConfig(map[string]string{"tomato-patch": "tenant", "gardenio": ""})
	d := &fakeJanitorDaemon{beads: map[string]*beadsapi.BeadDetail{"gd-a1": {Status: "in_progress"}}}
	for name, tc := range haltCases() {
		t.Run(name, func(t *testing.T) {
			e, lands := newGHTwoLandEnv(t, tenantGithubSecret("gd-a1", now.Add(10*time.Minute), "lands"))
			addLandAgentPod(t, e, "land-tomato-patch", "gd-a1")
			e.iss.landHalt = landHaltRefusal(cfg, tc.d)
			want := 1
			if tc.code != "" {
				want = 0
			}
			if st := e.iss.sweep(context.Background(), d, []string{"land-tomato-patch"}); st.Refreshed != want || lands.n != want {
				t.Fatalf("stats %+v, mints %d; want %d refresh", st, lands.n, want)
			}
		})
	}
}

// Review item C at the stuck-restart door: the recreate goes through the same
// spawn admission as a fresh spawn before it mints anything, and the issuer
// is wired to the land kill switches.
func TestStuckRestartRecreateIsAdmittedBeforeItMints(t *testing.T) {
	runSrc, err := os.ReadFile("run.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(runSrc), "ghApp.landHalt = landHaltRefusal(") {
		t.Error("run.go does not wire the issuer to the land kill switches")
	}
	b, err := os.ReadFile("event_handler.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	admit := indexAll(src, "spawnAdmission(ctx, cfg,")
	mint := indexAll(src, "ghApp.applyToSpec(")
	if len(admit) != 2 || len(mint) != 2 {
		t.Fatalf("event_handler.go: %d spawnAdmission and %d ghApp.applyToSpec calls, want 2 each (spawn, stuck restart)", len(admit), len(mint))
	}
	if !(admit[0] < mint[0] && mint[0] < admit[1] && admit[1] < mint[1]) {
		t.Error("each pod door in event_handler.go must run spawnAdmission before it mints a GitHub token")
	}
}

func indexAll(s, sub string) []int {
	var out []int
	for off := 0; ; {
		i := strings.Index(s[off:], sub)
		if i < 0 {
			return out
		}
		out = append(out, off+i)
		off += i + len(sub)
	}
}
