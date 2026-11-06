package bridge

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// These tests lock in the role-preservation fix (ka-0000001567): a session
// resume must keep the role the agent was ORIGINALLY spawned with, rather than
// silently re-deriving it from the channel_roles default. A role can pin a
// distinct MODEL (e.g. crew-lite → sonnet vs monorepo-fix → Opus) plus
// role-specific advice/tooling, so re-deriving from the channel default on an
// involuntary pod death downgrades the model mid-ticket — a correctness risk,
// not a cosmetic label nit. The pod's role (and thus its model/account/advice)
// is driven by fields.role (subscriber/sse.go buildEvent), so the resume bead's
// fields.role is exactly what must be preserved.

// seedPriorSpecialistAgentBead seeds a CLOSED prior agent bead the way a
// specialist spawn (or a prior resume) leaves it: a role: LABEL recording the
// role the agent was spawned as, plus a fields.role that may already have
// drifted. The label is the ground truth the fix reads.
func seedPriorSpecialistAgentBead(daemon *mockDaemon, agentName, project, roleLabel, fieldRole string) {
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	daemon.beads[agentName+"-prior"] = &beadsapi.BeadDetail{
		ID:     agentName + "-prior",
		Title:  agentName,
		Type:   "agent",
		Status: "closed",
		// Non-zero CreatedAt so the most-recent selector picks this bead.
		CreatedAt: time.Now(),
		Labels:    []string{"project:" + project, "role:" + roleLabel},
		Fields: map[string]string{
			"agent":   agentName,
			"project": project,
			"role":    fieldRole,
		},
	}
}

func newResumeTestBot(t *testing.T, daemon *mockDaemon) *Bot {
	t.Helper()
	slackSrv := newFakeSlackServer(t)
	t.Cleanup(slackSrv.Close)
	bot := newTestBot(daemon, slackSrv)
	bot.botUserID = "U-BOT"
	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	bot.state = state
	return bot
}

// firstResumeBead returns the single cold-resume agent bead minted (or revived)
// by respawnThreadAgent, failing the test if there is not exactly one.
func firstResumeBead(t *testing.T, daemon *mockDaemon) *beadsapi.BeadDetail {
	t.Helper()
	var resume []*beadsapi.BeadDetail
	for _, b := range filterAgentBeads(daemon.beads) {
		if b.Fields["spawn_source"] == "slack-thread-resume" {
			resume = append(resume, b)
		}
	}
	if len(resume) != 1 {
		t.Fatalf("expected exactly 1 cold-resume agent bead, got %d", len(resume))
	}
	return resume[0]
}

// TestRespawnThreadAgent_PreservesSpecialistRoleOverChannelDefault is the core
// regression for ka-0000001567: a monorepo-fix agent resumed from a channel
// whose channel_roles default is crew-lite must come back as monorepo-fix, NOT
// crew-lite. Before the fix, the resume blindly seeded threadRole from the
// channel default, silently downgrading the specialist role (and its model).
func TestRespawnThreadAgent_PreservesSpecialistRoleOverChannelDefault(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannelRole("monorepo", "C-MONO", "crew-lite")
	seedPriorSpecialistAgentBead(daemon, "thread-1111-2222", "monorepo", "monorepo-fix", "monorepo-fix")

	bot := newResumeTestBot(t, daemon)
	bot.respawnThreadAgent(context.Background(), "C-MONO", "1111.2222",
		"thread-1111-2222", "continue..", "U-HUMAN")

	got := firstResumeBead(t, daemon).Fields["role"]
	if got != "monorepo-fix" {
		t.Errorf("resume role = %q, want \"monorepo-fix\" — channel default crew-lite must NOT overwrite the specialist role the agent was spawned with (ka-0000001567)", got)
	}
}

// TestRespawnThreadAgent_LabelWinsOverDriftedFieldRole covers the exact bead
// shape from the bug report (agent gd-1HyPEQ9g9g): the role: LABEL is correct
// (monorepo-fix) but fields.role has ALREADY drifted to crew-lite from an
// earlier bad resume. The label is authoritative, so the resume must restore
// monorepo-fix rather than propagate the corrupted field value.
func TestRespawnThreadAgent_LabelWinsOverDriftedFieldRole(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannelRole("monorepo", "C-MONO", "crew-lite")
	seedPriorSpecialistAgentBead(daemon, "thread-3333-4444", "monorepo", "monorepo-fix", "crew-lite")

	bot := newResumeTestBot(t, daemon)
	bot.respawnThreadAgent(context.Background(), "C-MONO", "3333.4444",
		"thread-3333-4444", "continue..", "U-HUMAN")

	got := firstResumeBead(t, daemon).Fields["role"]
	if got != "monorepo-fix" {
		t.Errorf("resume role = %q, want \"monorepo-fix\" — the role: label is authoritative over a drifted fields.role (ka-0000001567)", got)
	}
}

// TestRespawnThreadAgent_NoPriorRoleFallsBackToChannelDefault pins the fallback:
// when the prior bead carries NO role: label and NO fields.role (a genuinely
// role-less prior spawn), the resume still adopts the channel_roles default —
// the pre-fix behavior for the fresh case, which the fix must not regress.
func TestRespawnThreadAgent_NoPriorRoleFallsBackToChannelDefault(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannelRole("monorepo", "C-MONO", "crew-lite")
	// Prior bead with a project label but no role: label and empty fields.role.
	daemon.mu.Lock()
	daemon.beads["thread-5555-6666-prior"] = &beadsapi.BeadDetail{
		ID:        "thread-5555-6666-prior",
		Title:     "thread-5555-6666",
		Type:      "agent",
		Status:    "closed",
		CreatedAt: time.Now(),
		Labels:    []string{"project:monorepo"},
		Fields: map[string]string{
			"agent":   "thread-5555-6666",
			"project": "monorepo",
			"role":    "",
		},
	}
	daemon.mu.Unlock()

	bot := newResumeTestBot(t, daemon)
	bot.respawnThreadAgent(context.Background(), "C-MONO", "5555.6666",
		"thread-5555-6666", "continue..", "U-HUMAN")

	got := firstResumeBead(t, daemon).Fields["role"]
	if got != "crew-lite" {
		t.Errorf("resume role = %q, want \"crew-lite\" — with no prior role recorded, the channel_roles default still applies", got)
	}
}

// TestRespawnThreadAgent_MultiRoleLabelReconstructsCommaForm pins that a
// multi-role spawn (role:thread + role:crew labels) is reconstructed to the
// comma-joined field form threadRole expects, not a single arbitrary label.
func TestRespawnThreadAgent_MultiRoleLabelReconstructsCommaForm(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannelRole("gardenio", "C-GB", "crew-lite")
	daemon.mu.Lock()
	daemon.beads["thread-7777-8888-prior"] = &beadsapi.BeadDetail{
		ID:        "thread-7777-8888-prior",
		Title:     "thread-7777-8888",
		Type:      "agent",
		Status:    "closed",
		CreatedAt: time.Now(),
		Labels:    []string{"project:gardenio", "role:thread", "role:crew"},
		Fields: map[string]string{
			"agent":   "thread-7777-8888",
			"project": "gardenio",
			"role":    "thread,crew",
		},
	}
	daemon.mu.Unlock()

	bot := newResumeTestBot(t, daemon)
	bot.respawnThreadAgent(context.Background(), "C-GB", "7777.8888",
		"thread-7777-8888", "continue..", "U-HUMAN")

	got := firstResumeBead(t, daemon).Fields["role"]
	if got != "thread,crew" {
		t.Errorf("resume role = %q, want \"thread,crew\" — both role: labels must be reconstructed to the comma field form", got)
	}
}

// TestRoleFromPriorAgentBead is a focused unit test of the role-recovery helper,
// covering label-priority, field-fallback, empty, and nil.
func TestRoleFromPriorAgentBead(t *testing.T) {
	cases := []struct {
		name string
		bead *beadsapi.BeadDetail
		want string
	}{
		{
			name: "label wins over drifted field",
			bead: &beadsapi.BeadDetail{
				Labels: []string{"project:monorepo", "role:monorepo-fix"},
				Fields: map[string]string{"role": "crew-lite"},
			},
			want: "monorepo-fix",
		},
		{
			name: "multiple role labels rejoined with comma",
			bead: &beadsapi.BeadDetail{
				Labels: []string{"role:thread", "role:crew"},
				Fields: map[string]string{"role": "ignored"},
			},
			want: "thread,crew",
		},
		{
			name: "no label falls back to fields.role",
			bead: &beadsapi.BeadDetail{
				Labels: []string{"project:monorepo"},
				Fields: map[string]string{"role": "crew-lite"},
			},
			want: "crew-lite",
		},
		{
			name: "no role anywhere returns empty",
			bead: &beadsapi.BeadDetail{
				Labels: []string{"project:monorepo"},
				Fields: map[string]string{},
			},
			want: "",
		},
		{
			name: "empty role: label is ignored (bare prefix)",
			bead: &beadsapi.BeadDetail{
				Labels: []string{"role:"},
				Fields: map[string]string{"role": "crew-lite"},
			},
			want: "crew-lite",
		},
		{
			name: "nil bead returns empty",
			bead: nil,
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := roleFromPriorAgentBead(tc.bead); got != tc.want {
				t.Errorf("roleFromPriorAgentBead = %q, want %q", got, tc.want)
			}
		})
	}
}
