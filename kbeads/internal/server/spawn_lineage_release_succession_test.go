package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

// A release's gardener the cutting run of the previous release's gardener
// binds is sealed from that gardener (decision gd-cDG0zTiNSk option a,
// gd-9aclijPx85), keeping its author and iat.

const (
	successionPrevRel = "gd-rel1"
	successionNextRel = "gd-rel2"
	successionPrevGdn = "gd-gdn1"
	successionCaptain = "gd-cap1"
)

func releaseGardenerLabels(rel string) []string {
	return []string{"project:gardenio", "role:gardener", "trigger:multi", releaseGardenerMarkerLabel, releaseGardenerLabelPrefix + rel}
}

const releaseGardenerFields = `{"action":"spawn_agent","role":"release-captain","project":"gardenio","prompt":"You are the gardener of gardenio's release"}`

type successionFixture struct {
	*lineageFixture
}

// newSuccessionFixture seeds a cut release with an admin-sealed gardener, a
// release-captain run of it, and the next (open) release.
func newSuccessionFixture(t *testing.T) *successionFixture {
	f := &successionFixture{newLineageFixture(t)}
	makeBypassRoleBead(f.ms, "gd-role-release-captain", releaseCaptainRole)
	f.release(successionPrevRel, releaseStatusCut)
	f.release(successionNextRel, releaseStatusOpen)
	f.bindAs(adminOIDCCtx(), successionPrevGdn, successionPrevRel, releaseGardenerFields)
	if sealedSpawnAuthor(f.ms.beads[successionPrevGdn]) != lineageAdmin {
		t.Fatal("fixture: the operator's gardener must be sealed")
	}
	f.captain(successionCaptain, successionPrevGdn)
	return f
}

func (f *successionFixture) release(id, status string) {
	f.ms.beads[id] = &model.Bead{
		ID: id, Type: model.TypeRelease, Kind: model.KindData, Status: model.StatusOpen,
		Fields: json.RawMessage(`{"project":"gardenio","status":"` + status + `"}`),
	}
	f.ms.labels[id] = []string{"project:gardenio"}
}

// captain seeds a release-captain agent spawned by trigger.
func (f *successionFixture) captain(id, trigger string) {
	seedAgent(f.ms, id, releaseCaptainRole, "gardenio")
	withFields(map[string]any{triggerLineageField: trigger})(f.ms.beads[id])
}

// bindAs creates rel's gardener as ctx, the way releasegardener.Ensure does,
// and stores it under id.
func (f *successionFixture) bindAs(ctx context.Context, id, rel, fields string) {
	f.t.Helper()
	labels := releaseGardenerLabels(rel)
	b, err := f.srv.createBead(ctx, createBeadInput{
		Title: "Release gardener for " + rel, Type: "trigger", Kind: "data",
		Labels: labels, Fields: json.RawMessage(fields),
	})
	if err != nil {
		f.t.Fatalf("bind release gardener: %v", err)
	}
	stored := f.ms.beads[b.ID]
	delete(f.ms.beads, b.ID)
	stored.ID = id
	f.ms.beads[id] = stored
	f.ms.labels[id] = labels
}

func captainCtx(id string) context.Context {
	return ownerAgentCtx(id, "gardenio-release-gardener-for-gd-rel1-ab12")
}

func TestReleaseSuccessionSeal_ValidChainSeals(t *testing.T) {
	f := newSuccessionFixture(t)
	f.now = f.now.Add(2 * time.Hour)
	f.bindAs(captainCtx(successionCaptain), "gd-gdn2", successionNextRel, releaseGardenerFields)

	next := validSpawnSeal(f.ms.beads["gd-gdn2"])
	prev := validSpawnSeal(f.ms.beads[successionPrevGdn])
	if next == nil || next.Author != lineageAdmin {
		t.Fatalf("successor seal = %+v, want one by %s", next, lineageAdmin)
	}
	if next.IAT != prev.IAT {
		t.Errorf("successor iat %d, want the predecessor's %d: succession must not extend a seal", next.IAT, prev.IAT)
	}
	if err := f.spawn(engineCtx(), spawnFieldsWithTrigger("gardenio", releaseCaptainRole, "gd-gdn2")); err != nil {
		t.Fatalf("the successor's first fire must spawn without an escalation, got %v", err)
	}
}

func TestReleaseSuccessionSeal_ChainPastItsIATIsRefused(t *testing.T) {
	f := newSuccessionFixture(t)
	f.now = f.now.Add(defaultSpawnLineageSealTTL + time.Hour)
	f.bindAs(captainCtx(successionCaptain), "gd-gdn2", successionNextRel, releaseGardenerFields)
	if got := sealedSpawnAuthor(f.ms.beads["gd-gdn2"]); got != "" {
		t.Fatalf("a predecessor seal past its TTL sealed a successor to %q", got)
	}
}

func TestReleaseSuccessionSeal_CallerNotARunOfASealedGardener(t *testing.T) {
	cases := map[string]func(f *successionFixture) string{
		"a crew agent": func(f *successionFixture) string { return "gd-crew1" },
		"a release-captain run of an unsealed gardener": func(f *successionFixture) string {
			f.bindAs(captainCtx("gd-crew2"), "gd-gdn0", successionPrevRel, releaseGardenerFields)
			f.captain("gd-cap0", "gd-gdn0")
			return "gd-cap0"
		},
		"a release-captain run of no trigger": func(f *successionFixture) string {
			seedAgent(f.ms, "gd-cap9", releaseCaptainRole, "gardenio")
			return "gd-cap9"
		},
	}
	for name, caller := range cases {
		t.Run(name, func(t *testing.T) {
			f := newSuccessionFixture(t)
			id := caller(f)
			f.bindAs(captainCtx(id), "gd-gdn2", successionNextRel, releaseGardenerFields)
			if got := sealedSpawnAuthor(f.ms.beads["gd-gdn2"]); got != "" {
				t.Fatalf("sealed to %q", got)
			}
		})
	}
}

func TestReleaseSuccessionSeal_OneSealedGardenerPerRelease(t *testing.T) {
	f := newSuccessionFixture(t)
	f.bindAs(captainCtx(successionCaptain), "gd-gdn2", successionNextRel, releaseGardenerFields)
	if sealedSpawnAuthor(f.ms.beads["gd-gdn2"]) == "" {
		t.Fatal("the first successor must be sealed")
	}
	f.bindAs(captainCtx(successionCaptain), "gd-gdn3", successionNextRel, releaseGardenerFields)
	if got := sealedSpawnAuthor(f.ms.beads["gd-gdn3"]); got != "" {
		t.Fatalf("a second gardener for the same release was sealed to %q", got)
	}
}

func TestReleaseSuccessionSeal_RoleAndReleaseAreChecked(t *testing.T) {
	f := newSuccessionFixture(t)
	f.bindAs(captainCtx(successionCaptain), "gd-gdn2", successionNextRel, lineageTriggerFields)
	if got := sealedSpawnAuthor(f.ms.beads["gd-gdn2"]); got != "" {
		t.Fatalf("a successor spawning role reviewer was sealed to %q", got)
	}
	f.release(successionNextRel, releaseStatusLive)
	f.bindAs(captainCtx(successionCaptain), "gd-gdn3", successionNextRel, releaseGardenerFields)
	if got := sealedSpawnAuthor(f.ms.beads["gd-gdn3"]); got != "" {
		t.Fatalf("a gardener for a live release was sealed to %q", got)
	}
}
