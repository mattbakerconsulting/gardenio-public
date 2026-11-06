package releaserecord

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// The event shapes are the ones on the live daemon: the CronJob's template
// and an operator's prose.
func TestParseDeployEvent(t *testing.T) {
	at := time.Date(2026, 10, 30, 16, 23, 47, 153566000, time.UTC)
	for _, tc := range []struct {
		title, desc string
		ok          bool
		tag, rev    string
	}{
		{"release-deploy 2026.294.7: deployed and verified", "outcome: deployed and verified\nhelm revision before: 207\nhelm revision after: 208\n", true, "2026.294.7", "208"},
		{"release-deploy 2026.303.2: deployed and verified", "laptop admin roll 16:05Z 10-30: helm rev 259, tag eb1539512, verify-image-tags 9/9", true, "2026.303.2", "259"},
		{"release-deploy 2026.266.4: deployed and verified", "Helm rev 59 (2026.266.3) became rev 60 (2026.266.4), Upgrade complete.", true, "2026.266.4", "60"},
		{"release-deploy 2026.292.8: deployed and verified", "laptop-claude: 2026.292.8 is live as helm rev 194. The deployer's 00:00Z run (rev 193) applied", true, "2026.292.8", "194"},
		{"release-deploy 2026.304.1: ok", "no revision here", true, "2026.304.1", ""},
		{"release-deploy 2026.294.7: images not ready", "outcome: images not ready", false, "", ""},
		{"release-deploy 2026.293.9: helm busy: pending-upgrade", "", false, "", ""},
		{"release-deploy 2026.300.1: dry-run ok", "", false, "", ""},
		{"release-deploy 2026.300.1: verify failed, rolled back", "helm revision after: 300", false, "", ""},
		{"something else", "helm rev 1", false, "", ""},
	} {
		ev, ok := ParseDeployEvent(tc.title, tc.desc, at)
		if ok != tc.ok || ev.Tag != tc.tag || ev.Revision != tc.rev {
			t.Errorf("%q: got %+v ok=%v, want tag %q rev %q ok=%v", tc.title, ev, ok, tc.tag, tc.rev, tc.ok)
		}
		if ok && ev.At != "2026-10-30T16:23:47Z" {
			t.Errorf("%q: at = %q, want the creation time in UTC seconds", tc.title, ev.At)
		}
	}
}

type deployFake struct {
	beads    []*beadsapi.BeadDetail
	releases map[string]bool
	// taken maps a revision to the other tag the daemon records it for.
	taken   map[string]string
	deploys []beadsapi.DeployReleaseRequest
}

func (f *deployFake) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	res := &beadsapi.ListBeadsResult{}
	for i, b := range f.beads {
		if i >= q.Offset && len(res.Beads) < q.Limit {
			res.Beads = append(res.Beads, b)
		}
	}
	return res, nil
}

func (f *deployFake) DeployRelease(_ context.Context, ref, _ string, req beadsapi.DeployReleaseRequest) (*beadsapi.Release, error) {
	if !f.releases[ref] {
		return nil, &beadsapi.APIError{StatusCode: http.StatusNotFound, Message: "release not found"}
	}
	if other, ok := f.taken[req.Revision]; ok && other != ref {
		return nil, &beadsapi.APIError{StatusCode: http.StatusConflict, Message: "revision is already recorded for " + other}
	}
	f.deploys = append(f.deploys, req)
	return &beadsapi.Release{Tag: ref}, nil
}

func TestBackfillDeployments(t *testing.T) {
	ev := func(id, title, desc, at string) *beadsapi.BeadDetail {
		ts, _ := time.Parse(time.RFC3339, at)
		return &beadsapi.BeadDetail{ID: id, Title: title, Description: desc, CreatedAt: ts}
	}
	f := &deployFake{
		// Newest first, as the daemon lists them.
		beads: []*beadsapi.BeadDetail{
			ev("gd-e5", "release-deploy 2026.303.2: deployed and verified", "helm rev 259", "2026-10-30T16:23:47Z"),
			ev("gd-e4", "release-deploy 2026.284.7: deployed and verified", "Auto-deployer rolled 2026.284.7 at 21:00Z, helm rev 105.", "2026-10-11T21:31:18Z"),
			ev("gd-e3", "release-deploy 2026.284.7: deployed and verified", "helm revision after: 105", "2026-10-11T21:05:58Z"),
			ev("gd-e2", "release-deploy 2026.284.7: images not ready", "", "2026-10-11T20:45:00Z"),
			ev("gd-e1", "release-deploy 2026.200.1: deployed and verified", "helm revision after: 12", "2026-07-19T04:00:00Z"),
		},
		releases: map[string]bool{"2026.284.7": true, "2026.303.2": true},
	}
	opts := DeployBackfillOptions{Project: "gardenio", Target: "prod"}

	items, err := BackfillDeployments(context.Background(), f, opts)
	if err != nil || len(items) != 4 || len(f.deploys) != 0 {
		t.Fatalf("dry run: %d items, %d writes, err %v; want 4 planned, none written", len(items), len(f.deploys), err)
	}

	opts.Write = true
	items, err = BackfillDeployments(context.Background(), f, opts)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, it := range items {
		got = append(got, it.Bead+":"+map[bool]string{true: "recorded", false: "skipped"}[it.Recorded])
	}
	want := []string{"gd-e1:skipped", "gd-e3:recorded", "gd-e4:skipped", "gd-e5:recorded"}
	if len(got) != len(want) {
		t.Fatalf("items = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("items = %v, want %v (oldest first; the duplicate report and the unrecorded tag skipped)", got, want)
			break
		}
	}
	if items[0].Skip != "no release is recorded for this tag" {
		t.Errorf("unrecorded tag skip = %q", items[0].Skip)
	}
	if len(f.deploys) != 2 {
		t.Fatalf("writes = %+v, want 2", f.deploys)
	}
	if d := f.deploys[0]; d.Target != "prod" || d.Since != "2026-10-11T21:05:58Z" || d.Revision != "105" || d.ImageTag != "2026.284.7" || d.By != "backfill" || d.Via != "script" {
		t.Errorf("first write = %+v", d)
	}

	// A revision the daemon already records for another tag is skipped.
	f.deploys = nil
	f.taken = map[string]string{"259": "2026.303.1"}
	items, err = BackfillDeployments(context.Background(), f, opts)
	if err != nil || len(items) != 4 || !strings.HasPrefix(items[3].Skip, "another release records this revision") || items[3].Recorded {
		t.Errorf("409 on rev 259: err %v, items %+v; want it skipped", err, items)
	}
	f.taken = nil

	f.deploys = nil
	opts.Since = time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	if items, _ := BackfillDeployments(context.Background(), f, opts); len(items) != 1 || items[0].Tag != "2026.303.2" {
		t.Errorf("--since: items = %+v, want only the newer event", items)
	}
}
