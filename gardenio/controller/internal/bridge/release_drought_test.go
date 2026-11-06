package bridge

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// fakeDroughtGitLab serves canned tag/release listings (or errors) to the
// watchdog under test.
type fakeDroughtGitLab struct {
	mu       sync.Mutex
	tags     []GitLabTag
	releases []GitLabRelease
	tagsErr  error
	relErr   error
}

func (f *fakeDroughtGitLab) ListTags(_ context.Context, _ string) ([]GitLabTag, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tagsErr != nil {
		return nil, f.tagsErr
	}
	return f.tags, nil
}

func (f *fakeDroughtGitLab) ListReleases(_ context.Context, _ string) ([]GitLabRelease, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.relErr != nil {
		return nil, f.relErr
	}
	return f.releases, nil
}

func (f *fakeDroughtGitLab) set(tags []GitLabTag, releases []GitLabRelease) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tags, f.releases = tags, releases
	f.tagsErr, f.relErr = nil, nil
}

// droughtTag builds a calver tag fixture whose commit timestamp is `age`
// before the fixed test clock.
func droughtTag(name string, at time.Time) GitLabTag {
	var t GitLabTag
	t.Name = name
	t.Commit.ID = "sha-" + name
	t.Commit.CreatedAt = at.UTC().Format(time.RFC3339)
	return t
}

func droughtRelease(tag string, at time.Time, upcoming bool) GitLabRelease {
	return GitLabRelease{
		TagName:     tag,
		Name:        "Release " + tag,
		ReleasedAt:  at.UTC().Format(time.RFC3339),
		UpcomingRel: upcoming,
	}
}

// droughtClock is a fixed reference instant for the pure-function tests.
var droughtClock = time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)

func TestAssessReleaseDrought_AllReleased_Healthy(t *testing.T) {
	tags := []GitLabTag{
		droughtTag("2026.183.2", droughtClock.Add(-3*time.Hour)),
		droughtTag("2026.183.1", droughtClock.Add(-8*time.Hour)),
	}
	releases := []GitLabRelease{
		droughtRelease("2026.183.2", droughtClock.Add(-2*time.Hour), false),
	}

	v := assessReleaseDrought(tags, releases, droughtClock, 6*time.Hour)
	if v.Drought {
		t.Fatalf("expected no drought, got %+v", v)
	}
	if v.UnreleasedCount != 0 {
		t.Fatalf("expected 0 unreleased tags, got %d", v.UnreleasedCount)
	}
	if v.LatestReleaseTag != "2026.183.2" {
		t.Fatalf("expected latest release 2026.183.2, got %q", v.LatestReleaseTag)
	}
}

func TestAssessReleaseDrought_StaleUnreleasedTag_Drought(t *testing.T) {
	// The P0 shape: last release long ago, tags piling up since. The anchor
	// must be the OLDEST unreleased tag even when newer tags keep landing.
	tags := []GitLabTag{
		droughtTag("2026.183.5", droughtClock.Add(-10*time.Minute)),
		droughtTag("2026.183.4", droughtClock.Add(-4*time.Hour)),
		droughtTag("2026.183.3", droughtClock.Add(-7*time.Hour)),
		droughtTag("2026.182.6", droughtClock.Add(-26*time.Hour)),
	}
	releases := []GitLabRelease{
		droughtRelease("2026.182.6", droughtClock.Add(-25*time.Hour), false),
	}

	v := assessReleaseDrought(tags, releases, droughtClock, 6*time.Hour)
	if !v.Drought {
		t.Fatalf("expected drought, got %+v", v)
	}
	if v.AnchorTag != "2026.183.3" {
		t.Fatalf("expected anchor 2026.183.3 (oldest unreleased), got %q", v.AnchorTag)
	}
	if v.UnreleasedCount != 3 {
		t.Fatalf("expected 3 unreleased tags, got %d", v.UnreleasedCount)
	}
	if v.AnchorAge != 7*time.Hour {
		t.Fatalf("expected anchor age 7h, got %s", v.AnchorAge)
	}
}

func TestAssessReleaseDrought_FreshTag_NoDrought(t *testing.T) {
	// A tag cut minutes ago with its pipeline still in flight must NOT alert,
	// even when the last release is far older than the threshold — the
	// deadman anchors at tag-push time, not a wall-clock release window.
	tags := []GitLabTag{
		droughtTag("2026.183.1", droughtClock.Add(-20*time.Minute)),
		droughtTag("2026.182.6", droughtClock.Add(-30*time.Hour)),
	}
	releases := []GitLabRelease{
		droughtRelease("2026.182.6", droughtClock.Add(-29*time.Hour), false),
	}

	v := assessReleaseDrought(tags, releases, droughtClock, 6*time.Hour)
	if v.Drought {
		t.Fatalf("expected no drought for 20m-old tag, got %+v", v)
	}
	if v.AnchorTag != "2026.183.1" || v.UnreleasedCount != 1 {
		t.Fatalf("unexpected verdict %+v", v)
	}
}

func TestAssessReleaseDrought_NewerReleaseRetiresOlderTags(t *testing.T) {
	// A release cut for a NEWER calver retires every older waiting tag: a
	// skipped build (183.3 failed, 183.4 shipped) is not a drought.
	tags := []GitLabTag{
		droughtTag("2026.183.4", droughtClock.Add(-8*time.Hour)),
		droughtTag("2026.183.3", droughtClock.Add(-9*time.Hour)),
	}
	releases := []GitLabRelease{
		droughtRelease("2026.183.4", droughtClock.Add(-7*time.Hour), false),
	}

	v := assessReleaseDrought(tags, releases, droughtClock, 6*time.Hour)
	if v.Drought || v.UnreleasedCount != 0 {
		t.Fatalf("expected healthy verdict, got %+v", v)
	}
}

func TestAssessReleaseDrought_UpcomingReleaseIgnored(t *testing.T) {
	// upcoming_release entries are placeholders, not shipped releases — they
	// must not mask a drought.
	tags := []GitLabTag{
		droughtTag("2026.183.1", droughtClock.Add(-8*time.Hour)),
	}
	releases := []GitLabRelease{
		droughtRelease("2026.183.1", droughtClock.Add(-1*time.Hour), true),
	}

	v := assessReleaseDrought(tags, releases, droughtClock, 6*time.Hour)
	if !v.Drought {
		t.Fatalf("expected drought (upcoming release must not count), got %+v", v)
	}
	if v.LatestReleaseTag != "" {
		t.Fatalf("expected no latest release, got %q", v.LatestReleaseTag)
	}
}

func TestAssessReleaseDrought_NonCalverTagsIgnored(t *testing.T) {
	tags := []GitLabTag{
		droughtTag("v1.2.3", droughtClock.Add(-100*time.Hour)),
		droughtTag("nightly-build", droughtClock.Add(-50*time.Hour)),
	}

	v := assessReleaseDrought(tags, nil, droughtClock, 6*time.Hour)
	if v.Drought || v.UnreleasedCount != 0 {
		t.Fatalf("expected non-calver tags to be ignored, got %+v", v)
	}
}

func TestAssessReleaseDrought_TagWithoutTimestampSkipped(t *testing.T) {
	noTS := GitLabTag{Name: "2026.183.9"}
	tags := []GitLabTag{
		noTS,
		droughtTag("2026.183.1", droughtClock.Add(-8*time.Hour)),
	}

	v := assessReleaseDrought(tags, nil, droughtClock, 6*time.Hour)
	if !v.Drought {
		t.Fatalf("expected drought from the timestamped tag, got %+v", v)
	}
	if v.AnchorTag != "2026.183.1" || v.UnreleasedCount != 1 {
		t.Fatalf("expected the unageable tag to be skipped, got %+v", v)
	}
}

// droughtWatchdog wires a watchdog over fakes with a mutable fixed clock.
// Returns the watchdog, the fake GitLab, the mock daemon, the recording
// notifier, and a function to advance the clock.
func droughtWatchdog(t *testing.T, cfg ReleaseDroughtConfig) (*ReleaseDroughtWatchdog, *fakeDroughtGitLab, *mockTriggerDaemon, *recordingNotifier, func(time.Duration)) {
	t.Helper()
	gl := &fakeDroughtGitLab{}
	daemon := &mockTriggerDaemon{}
	rec := &recordingNotifier{}

	var mu sync.Mutex
	now := droughtClock
	advance := func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		now = now.Add(d)
	}

	cfg.GitLab = gl
	cfg.Daemon = daemon
	cfg.Notifier = rec.notify
	cfg.Logger = slog.Default()
	cfg.Now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	w := NewReleaseDroughtWatchdog(cfg)
	if w == nil {
		t.Fatal("NewReleaseDroughtWatchdog returned nil")
	}
	return w, gl, daemon, rec, advance
}

// droughtFixture is the standard in-drought GitLab state: one release 25h
// ago, one tag waiting 7h (past the 6h default threshold).
func droughtFixture() ([]GitLabTag, []GitLabRelease) {
	tags := []GitLabTag{
		droughtTag("2026.183.1", droughtClock.Add(-7*time.Hour)),
		droughtTag("2026.182.6", droughtClock.Add(-26*time.Hour)),
	}
	releases := []GitLabRelease{
		droughtRelease("2026.182.6", droughtClock.Add(-25*time.Hour), false),
	}
	return tags, releases
}

func TestReleaseDroughtWatchdog_AlertsOncePerEpisode(t *testing.T) {
	w, gl, daemon, rec, _ := droughtWatchdog(t, ReleaseDroughtConfig{CreateBead: true})
	gl.set(droughtFixture())

	ctx := context.Background()
	w.Check(ctx)
	w.Check(ctx)
	w.Check(ctx)

	if rec.count() != 1 {
		t.Fatalf("expected exactly 1 notify for a persisting drought within the repeat window, got %d", rec.count())
	}
	if got := len(daemon.createdBeads); got != 1 {
		t.Fatalf("expected exactly 1 drought bug bead, got %d", got)
	}

	bead := daemon.createdBeads[0]
	if bead.Type != "bug" || bead.Priority != 0 {
		t.Fatalf("expected P0 bug bead, got type=%s priority=%d", bead.Type, bead.Priority)
	}
	wantLabels := map[string]bool{
		"release-drought":                 false,
		"source:release-drought-watchdog": false,
		"project:gardenio":                false,
	}
	for _, l := range bead.Labels {
		if _, ok := wantLabels[l]; ok {
			wantLabels[l] = true
		}
	}
	for l, seen := range wantLabels {
		if !seen {
			t.Fatalf("drought bead missing label %q (labels: %v)", l, bead.Labels)
		}
	}
	if !strings.Contains(bead.Title, "2026.183.1") {
		t.Fatalf("drought bead title should name the anchor tag, got %q", bead.Title)
	}

	rec.mu.Lock()
	msg := rec.calls[0]
	rec.mu.Unlock()
	for _, want := range []string{"RELEASE DROUGHT", "2026.183.1", "2026.182.6", "ka-0000007176"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("alert message missing %q: %s", want, msg)
		}
	}
}

func TestReleaseDroughtWatchdog_RepeatsAfterRepeatInterval(t *testing.T) {
	w, gl, daemon, rec, advance := droughtWatchdog(t, ReleaseDroughtConfig{
		CreateBead: true,
		Repeat:     4 * time.Hour,
	})
	gl.set(droughtFixture())

	ctx := context.Background()
	w.Check(ctx) // first alert
	advance(2 * time.Hour)
	w.Check(ctx) // inside repeat window — quiet
	advance(2 * time.Hour)
	w.Check(ctx) // 4h since first alert — re-alert

	if rec.count() != 2 {
		t.Fatalf("expected 2 notifies (initial + one repeat), got %d", rec.count())
	}
	// The bug bead is filed only on the first alert of the episode; the
	// repeat notify must not file another.
	if got := len(daemon.createdBeads); got != 1 {
		t.Fatalf("expected 1 drought bug bead across repeats, got %d", got)
	}
}

func TestReleaseDroughtWatchdog_RecoveryResetsEpisode(t *testing.T) {
	w, gl, _, rec, _ := droughtWatchdog(t, ReleaseDroughtConfig{})
	gl.set(droughtFixture())

	ctx := context.Background()
	w.Check(ctx)
	if rec.count() != 1 {
		t.Fatalf("expected drought alert, got %d notifies", rec.count())
	}

	// A release lands for the newest tag — drought over.
	tags, _ := droughtFixture()
	gl.set(tags, []GitLabRelease{
		droughtRelease("2026.183.1", droughtClock.Add(-time.Minute), false),
	})
	w.Check(ctx)
	if rec.count() != 2 {
		t.Fatalf("expected a recovery notice, got %d notifies", rec.count())
	}
	rec.mu.Lock()
	recovery := rec.calls[1]
	rec.mu.Unlock()
	if !strings.Contains(recovery, "recovered") {
		t.Fatalf("expected recovery message, got %q", recovery)
	}

	// Drought again — a NEW episode must alert again.
	gl.set(droughtFixture())
	w.Check(ctx)
	if rec.count() != 3 {
		t.Fatalf("expected a fresh alert for the new episode, got %d notifies", rec.count())
	}
}

func TestReleaseDroughtWatchdog_DedupsAgainstExistingOpenBead(t *testing.T) {
	w, gl, daemon, _, _ := droughtWatchdog(t, ReleaseDroughtConfig{CreateBead: true})
	gl.set(droughtFixture())

	// An open drought bug already exists (e.g. filed before a bridge restart).
	daemon.beads = append(daemon.beads, &beadsapi.BeadDetail{
		ID:     "gd-existing-drought",
		Type:   "bug",
		Status: "open",
		Title:  "Release drought: 2026.183.1 unreleased",
		Labels: []string{releaseDroughtBeadLabel},
	})

	w.Check(context.Background())

	if got := len(daemon.createdBeads); got != 0 {
		t.Fatalf("expected no new bead when an open release-drought bug exists, got %d", got)
	}
}

func TestReleaseDroughtWatchdog_GitLabErrorKeepsEpisodeState(t *testing.T) {
	w, gl, _, rec, advance := droughtWatchdog(t, ReleaseDroughtConfig{Repeat: 4 * time.Hour})
	gl.set(droughtFixture())

	ctx := context.Background()
	w.Check(ctx) // enters drought, alerts
	if rec.count() != 1 {
		t.Fatalf("expected initial alert, got %d", rec.count())
	}

	// Transient GitLab failure mid-drought: no alert, no state reset.
	gl.mu.Lock()
	gl.tagsErr = errors.New("gitlab 502")
	gl.mu.Unlock()
	advance(4 * time.Hour)
	w.Check(ctx)
	if rec.count() != 1 {
		t.Fatalf("expected no alert on a failed poll, got %d", rec.count())
	}

	// API back: the repeat fires (state survived the error) — and no
	// spurious recovery notice was emitted for the outage.
	gl.set(droughtFixture())
	w.Check(ctx)
	if rec.count() != 2 {
		t.Fatalf("expected repeat alert after recovery of the API, got %d", rec.count())
	}
	rec.mu.Lock()
	second := rec.calls[1]
	rec.mu.Unlock()
	if !strings.Contains(second, "RELEASE DROUGHT") {
		t.Fatalf("expected a drought re-alert, got %q", second)
	}
}

func TestReleaseDroughtWatchdog_HealthyStateStaysQuiet(t *testing.T) {
	w, gl, daemon, rec, _ := droughtWatchdog(t, ReleaseDroughtConfig{CreateBead: true})
	tags, _ := droughtFixture()
	gl.set(tags, []GitLabRelease{
		droughtRelease("2026.183.1", droughtClock.Add(-time.Hour), false),
	})

	w.Check(context.Background())
	if rec.count() != 0 || len(daemon.createdBeads) != 0 {
		t.Fatalf("healthy state must be silent: %d notifies, %d beads",
			rec.count(), len(daemon.createdBeads))
	}
}
