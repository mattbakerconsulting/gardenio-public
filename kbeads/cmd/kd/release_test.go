package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"gardenio/kbeads/internal/client"
	"github.com/spf13/cobra"
)

// execRelease runs `kd release <args>` on a fresh command tree: cobra
// executes a registered subcommand from the root, and fresh flags keep tests
// independent.
func execRelease(t *testing.T, args ...string) error {
	t.Helper()
	root := &cobra.Command{Use: "release", SilenceUsage: true}
	for _, sub := range []struct {
		src   *cobra.Command
		flags func(*cobra.Command)
	}{
		{releaseListCmd, addReleaseListFlags},
		{releaseShowCmd, addReleaseShowFlags},
		{releaseCreateCmd, addReleaseCreateFlags},
		{releaseOpenCmd, func(*cobra.Command) {}},
		{releaseDeployCmd, addReleaseDeployFlags},
		{releaseRepairDeploymentsCmd, addReleaseRepairFlags},
	} {
		c := &cobra.Command{Use: sub.src.Use, Args: sub.src.Args, RunE: sub.src.RunE, SilenceUsage: true}
		sub.flags(c)
		root.AddCommand(c)
	}
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(args)
	return root.Execute()
}

func testRelease() client.Release {
	return client.Release{
		ID: "gd-rel1", Title: "gardenio 2026.300.1", Project: "gardenio", Tag: "2026.300.1", SHA: "abc123",
		PreviousTag: "2026.299.2", Previous: "gd-rel0", CutAt: "2026-10-27T04:00:00Z", CutBy: "gardenio-release-captain-1",
		Source: "script", Status: "live", Contents: json.RawMessage(`[{"pr":1245},{"pr":1261}]`),
		Deployments: []client.ReleaseDeployment{{Target: "prod", Kind: "k3s", Since: "2026-10-27T05:06:00Z", Revision: "412"}},
	}
}

func TestReleaseShow_ByTag(t *testing.T) {
	mc := newMockClient()
	mc.Releases = []client.Release{testRelease()}
	withMockClient(t, mc)

	out := captureStdout(t, func() {
		if err := execRelease(t, "show", "2026.300.1", "--project", "gardenio"); err != nil {
			t.Fatalf("release show: %v", err)
		}
	})
	for _, want := range []string{"gardenio 2026.300.1", "Status:      live", "2026.299.2 (gd-rel0)", "Contents:    2 PRs", "prod", "(running)", "412"} {
		if !strings.Contains(out, want) {
			t.Errorf("show output missing %q:\n%s", want, out)
		}
	}
}

func TestReleaseList_JSON(t *testing.T) {
	mc := newMockClient()
	mc.Releases = []client.Release{testRelease()}
	withMockClient(t, mc)
	jsonOutput = true
	t.Cleanup(func() { jsonOutput = false })

	out := captureStdout(t, func() {
		if err := execRelease(t, "list", "--project", "gardenio", "--status", "live"); err != nil {
			t.Fatalf("release list: %v", err)
		}
	})
	var rows []client.Release
	if err := json.Unmarshal([]byte(out), &rows); err != nil || len(rows) != 1 || rows[0].Tag != "2026.300.1" {
		t.Fatalf("list --json = %v %s", err, out)
	}
	if c := mc.ListReleasesCalls; len(c) != 1 || c[0].Project != "gardenio" || c[0].Status != "live" {
		t.Errorf("ListReleases calls = %+v", c)
	}
}

func TestReleaseCreateAndDeploy_WireRequests(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	captureStdout(t, func() {
		if err := execRelease(t, "create", "--project", "gardenio", "--tag", "2026.301.1", "--sha", "def456",
			"--contents", `[{"pr":1300}]`, "--contents-complete=false"); err != nil {
			t.Fatalf("release create: %v", err)
		}
	})
	if c := mc.CreateReleaseCalls; len(c) != 1 || c[0].Tag != "2026.301.1" || c[0].SHA != "def456" ||
		string(c[0].Contents) != `[{"pr":1300}]` || c[0].ContentsComplete == nil || *c[0].ContentsComplete {
		t.Errorf("CreateRelease calls = %+v", c)
	}
	if err := execRelease(t, "create", "--project", "gardenio", "--tag", "2026.301.2", "--contents", "{not json"); err == nil {
		t.Error("create with invalid --contents JSON succeeded")
	}

	captureStdout(t, func() {
		if err := execRelease(t, "deploy", "2026.301.1", "--project", "gardenio", "--target", "prod", "--revision", "414"); err != nil {
			t.Fatalf("release deploy: %v", err)
		}
	})
	if c := mc.DeployReleaseCalls; len(c) != 1 || c[0].Ref != "2026.301.1" || c[0].Project != "gardenio" ||
		c[0].Req.Target != "prod" || c[0].Req.Revision != "414" {
		t.Errorf("DeployRelease calls = %+v", c)
	}
	if err := execRelease(t, "deploy", "2026.301.1", "--project", "gardenio"); err == nil {
		t.Error("deploy without --target succeeded")
	}
}

func TestReleaseRepairDeployments_DryRunPrintsChanges(t *testing.T) {
	mc := newMockClient()
	mc.RepairResult = &client.RepairDeploymentsResult{
		Project: "gardenio", DryRun: true,
		Releases: []client.ReleaseRepairChange{{
			ID: "gd-rel2", Tag: "2026.305.2", StatusBefore: "live", Status: "live",
			Removed:     []client.ReleaseDeployment{{Target: "prod", Revision: "264", Since: "2026-11-01T17:40:25Z"}},
			Before:      make([]client.ReleaseDeployment, 2),
			Deployments: []client.ReleaseDeployment{{Target: "prod", Revision: "264", Since: "2026-11-01T17:14:35Z", By: "observed"}},
		}},
		Conflicts: []string{"revision 9 on prod is recorded for both a and b"},
	}
	withMockClient(t, mc)

	out := captureStdout(t, func() {
		if err := execRelease(t, "repair-deployments", "--project", "gardenio", "--dry-run"); err != nil {
			t.Fatalf("repair-deployments: %v", err)
		}
	})
	if c := mc.RepairCalls; len(c) != 1 || c[0].Project != "gardenio" || !c[0].DryRun {
		t.Errorf("RepairReleaseDeployments calls = %+v", c)
	}
	for _, want := range []string{"1 releases would repair (dry run", "2026.305.2", "2 rows -> 1 (1 duplicates dropped)", "rev 264", "2026-11-01T17:14:35Z", "(running)", "conflict (left alone): revision 9"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if err := execRelease(t, "repair-deployments"); err == nil {
		t.Error("repair-deployments without --project succeeded")
	}
}

// kd release open creates with the project alone; when the daemon refuses a
// second open release (409) it prints the one there is.
func TestReleaseOpen_CreatesOrShowsTheOpenOne(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)
	captureStdout(t, func() {
		if err := execRelease(t, "open", "gardenio"); err != nil {
			t.Fatalf("release open: %v", err)
		}
	})
	if c := mc.CreateReleaseCalls; len(c) != 1 || c[0].Project != "gardenio" || c[0].Tag != "" {
		t.Errorf("CreateRelease calls = %+v, want project only", c)
	}

	mc = newMockClient()
	mc.Releases = []client.Release{{ID: "gd-next", Project: "gardenio", Status: "open", OpenedAt: "2026-11-03T20:00:00Z", OpenedBy: "captain"}}
	mc.CreateReleaseErr = &client.APIError{StatusCode: 409, Message: "already has an open release"}
	withMockClient(t, mc)
	out := captureStdout(t, func() {
		if err := execRelease(t, "open", "gardenio"); err != nil {
			t.Fatalf("release open on a 409: %v", err)
		}
	})
	for _, want := range []string{"gd-next", "(next, not cut yet)", "Opened:      2026-11-03T20:00:00Z by captain"} {
		if !strings.Contains(out, want) {
			t.Errorf("open output missing %q:\n%s", want, out)
		}
	}
}
