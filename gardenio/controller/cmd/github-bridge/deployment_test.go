package main

import (
	"context"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// deployCall is one DeployRelease the recorder made.
type deployCall struct {
	ref, project string
	req          beadsapi.DeployReleaseRequest
}

// fakeDeployer is the machine principal's deploy route.
type fakeDeployer struct{ calls []deployCall }

func (f *fakeDeployer) DeployRelease(_ context.Context, ref, project string, req beadsapi.DeployReleaseRequest) (*beadsapi.Release, error) {
	f.calls = append(f.calls, deployCall{ref: ref, project: project, req: req})
	return &beadsapi.Release{ID: ref, Status: "live"}, nil
}

func newTestDeployRecorder(t *testing.T) (*releaseRecorder, *relDaemon, *fakeDeployer, map[string]int) {
	t.Helper()
	h, d, hits := newTestRecorder(t)
	f := &fakeDeployer{}
	h.deploys = f
	d.releases["widget/v1.1.0"] = &beadsapi.Release{ID: "gd-rel-v1.1.0", Project: "widget", Tag: "v1.1.0", SHA: shaV110,
		Contents: []byte(`[{"title":"Fix the hinge"}]`)}
	return h, d, f, hits
}

// deploymentEvent is the success fixture with replace applied to its payload.
func deploymentEvent(t *testing.T, replace ...string) Event {
	t.Helper()
	payload := string(fixture(t, "deployment_status_success"))
	for i := 0; i+1 < len(replace); i += 2 {
		if !strings.Contains(payload, replace[i]) {
			t.Fatalf("fixture has no %q", replace[i])
		}
		payload = strings.ReplaceAll(payload, replace[i], replace[i+1])
	}
	ev := Event{DeliveryID: "deployment-status", Type: "deployment_status", Payload: []byte(payload)}
	fillEnvelope(&ev)
	return ev
}

// A successful GitHub Deployment of tag v1.1.0 writes one row on that release:
// the environment as the target, via=github, by the deployment's creator, at
// the deployment's id, from the status's time.
func TestDeployment_SuccessWritesOneRow(t *testing.T) {
	h, _, f, _ := newTestDeployRecorder(t)
	if err := h.Handle(context.Background(), deploymentEvent(t)); err != nil {
		t.Fatal(err)
	}
	want := deployCall{ref: "gd-rel-v1.1.0", req: beadsapi.DeployReleaseRequest{
		Target: "production", Since: "2026-10-31T21:04:05Z", Via: "github", By: "github:octocat", Revision: "1104"}}
	if len(f.calls) != 1 || f.calls[0] != want {
		t.Errorf("deploys = %+v, want [%+v]", f.calls, want)
	}
}

// failure, error, inactive and the in-between states write nothing.
func TestDeployment_NonSuccessWritesNothing(t *testing.T) {
	for _, state := range []string{"failure", "error", "inactive", "pending", "in_progress", "queued"} {
		h, _, f, hits := newTestDeployRecorder(t)
		if err := h.Handle(context.Background(), deploymentEvent(t, `"state": "success"`, `"state": "`+state+`"`)); err != nil {
			t.Fatalf("%s: %v", state, err)
		}
		if len(f.calls) != 0 || len(hits) != 0 {
			t.Errorf("%s: deploys = %+v, GitHub hits = %v, want none", state, f.calls, hits)
		}
	}
}

// An environment the project does not declare is still sent, via=github and
// with no kind: the daemon declares it as a github-environment target
// (kbeads TestRelease_DeployGitHubDeclaresUnknownEnvironment), so nothing a
// project's CD deployed is lost.
func TestDeployment_UnknownEnvironmentIsSentForTheDaemonToDeclare(t *testing.T) {
	h, d, f, _ := newTestDeployRecorder(t)
	d.projects["widget"].ReleaseSource.Targets = []beadsapi.ReleaseTarget{{Name: "prod", Kind: "land"}}
	if err := h.Handle(context.Background(), deploymentEvent(t, `"environment": "production"`, `"environment": "staging-eu"`)); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 || f.calls[0].req.Target != "staging-eu" || f.calls[0].req.Via != "github" || f.calls[0].req.Kind != "" {
		t.Errorf("deploys = %+v, want one to staging-eu, via github, no kind", f.calls)
	}
}

// A deployment of a branch finds its release by sha; one whose sha no release
// carries writes nothing and is not an error.
func TestDeployment_BranchRefResolvesBySHA(t *testing.T) {
	h, _, f, _ := newTestDeployRecorder(t)
	if err := h.Handle(context.Background(), deploymentEvent(t, `"ref": "v1.1.0"`, `"ref": "main"`)); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 || f.calls[0].ref != "gd-rel-v1.1.0" {
		t.Errorf("deploys = %+v, want one on gd-rel-v1.1.0", f.calls)
	}

	h, _, f, _ = newTestDeployRecorder(t)
	ev := deploymentEvent(t, `"ref": "v1.1.0"`, `"ref": "main"`, shaV110, shaMerge)
	if err := h.Handle(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 0 {
		t.Errorf("deploys = %+v, want none for a sha no release carries", f.calls)
	}
}

// A deployment of a release tag the tag webhook has not recorded yet records
// the release first, then the row.
func TestDeployment_UnrecordedTagIsRecordedFirst(t *testing.T) {
	h, d, f, _ := newTestDeployRecorder(t)
	delete(d.releases, "widget/v1.1.0")
	if err := h.Handle(context.Background(), deploymentEvent(t)); err != nil {
		t.Fatal(err)
	}
	if d.creates != 2 || d.releases["widget/v1.1.0"] == nil || d.releases["widget/"] == nil {
		t.Fatalf("creates = %d releases = %v, want v1.1.0 recorded and the next release opened", d.creates, d.releases)
	}
	if len(f.calls) != 1 || f.calls[0].ref != d.releases["widget/v1.1.0"].ID {
		t.Errorf("deploys = %+v, want one on the recorded v1.1.0", f.calls)
	}
}

// Without the machine principal's deploy writer the event is left alone.
func TestDeployment_NoWriterIgnoresTheEvent(t *testing.T) {
	h, _, _, hits := newTestDeployRecorder(t)
	h.deploys = nil
	if err := h.Handle(context.Background(), deploymentEvent(t)); err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Errorf("GitHub hits = %v, want none", hits)
	}
}
