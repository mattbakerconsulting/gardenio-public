package server

import (
	"context"
	"encoding/json"
	"testing"

	"gardenio/kbeads/internal/model"
)

// The tests below pin the fleet-branch spawn-chain backstop (ka-0000001404):
// a header-less fleet-token write that references a spawn source in ctx
// (WithSpawnChainSubject, seeded by createBead from the write's own
// fields.spawned_by) is chain-walked back to the ultimate invoking human
// instead of binding the fleet:<subject> sentinel. This is the daemon pair to
// the client stamping in ka-0000007919 (decision respawn sets spawned_by =
// prior agent bead id).

// TestResolveOperatedByPure_Fleet_SpawnChainBackstop_Human proves the happy
// path: a bridge write with no X-Operated-By header but a spawned_by-seeded
// ctx resolves to the human at the chain root rather than the fleet sentinel.
//
// Topology (prior agent bead + its human root):
//
//	prior   Fields: spawned_by=root                (respawn seed; no direct human)
//	root    Fields: slack_user_id=U0OPERATOR1      (human root)
func TestResolveOperatedByPure_Fleet_SpawnChainBackstop_Human(t *testing.T) {
	srv := newChainWalkServer(
		&model.Bead{
			ID: "prior", Type: "agent",
			Fields: []byte(`{"spawned_by":"root"}`),
		},
		&model.Bead{
			ID: "root", Type: "agent",
			Fields: []byte(`{"slack_user_id":"U0OPERATOR1"}`),
		},
	)

	ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "slack-bridge"})
	ctx = WithSpawnChainSubject(ctx, "prior")

	value, source := srv.resolveOperatedByPure(ctx)
	if value != "slack:U0OPERATOR1" {
		t.Fatalf("value = %q; want slack:U0OPERATOR1 (backstop must chain-walk to human)", value)
	}
	if source != operatedBySourceFallback {
		t.Fatalf("source = %q; want %q", source, operatedBySourceFallback)
	}
}

// TestResolveOperatedByPure_Fleet_HeaderStillWinsOverBackstop pins that an
// explicit human X-Operated-By header short-circuits the resolver before the
// chain walk ever runs — the header is the primary attribution, the backstop
// is only a fallback.
func TestResolveOperatedByPure_Fleet_HeaderStillWinsOverBackstop(t *testing.T) {
	srv := newChainWalkServer(
		&model.Bead{
			ID: "prior", Type: "agent",
			Fields: []byte(`{"slack_user_id":"UOTHER1234"}`),
		},
	)

	ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "slack-bridge"})
	ctx = WithOperatedByHeader(ctx, "slack:U0OPERATOR1")
	ctx = WithSpawnChainSubject(ctx, "prior")

	value, source := srv.resolveOperatedByPure(ctx)
	if value != "slack:U0OPERATOR1" {
		t.Fatalf("value = %q; want slack:U0OPERATOR1 (header must win over backstop)", value)
	}
	if source != operatedBySourceHeader {
		t.Fatalf("source = %q; want %q", source, operatedBySourceHeader)
	}
}

// TestResolveOperatedByPure_Fleet_BackstopNonHumanRoot_FallsThrough pins the
// fail-closed boundary (ka-0000005772): a chain whose root carries only a
// machine/sentinel identity yields no human, so the resolver falls through to
// the fleet sentinel exactly as before — the backstop can only ADD a human.
func TestResolveOperatedByPure_Fleet_BackstopNonHumanRoot_FallsThrough(t *testing.T) {
	srv := newChainWalkServer(
		&model.Bead{
			ID: "prior", Type: "agent",
			Fields: []byte(`{"operated_by":"fleet:slack-bridge"}`),
		},
	)

	ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "slack-bridge"})
	ctx = WithSpawnChainSubject(ctx, "prior")

	value, source := srv.resolveOperatedByPure(ctx)
	if value != "fleet:slack-bridge" {
		t.Fatalf("value = %q; want fleet:slack-bridge (non-human root must fall through to sentinel)", value)
	}
	if source != operatedBySourceFleet {
		t.Fatalf("source = %q; want %q", source, operatedBySourceFleet)
	}
}

// TestResolveOperatedByPure_Fleet_NoSeed_FallsBackToSentinel pins that a fleet
// write with NO spawn-chain seed in ctx behaves exactly as before this change:
// the fleet sentinel. Guards against the backstop firing when there is nothing
// to walk.
func TestResolveOperatedByPure_Fleet_NoSeed_FallsBackToSentinel(t *testing.T) {
	srv := newTestServerWithAgentBead(nil)

	ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "slack-bridge"})

	value, source := srv.resolveOperatedByPure(ctx)
	if value != "fleet:slack-bridge" {
		t.Fatalf("value = %q; want fleet:slack-bridge (no seed → unchanged behavior)", value)
	}
	if source != operatedBySourceFleet {
		t.Fatalf("source = %q; want %q", source, operatedBySourceFleet)
	}
}

// TestSpawnedByFromFields covers the createBead field extractor that seeds the
// backstop: present/trimmed value, absent field, empty field, malformed JSON,
// and a non-string value all resolve as documented.
func TestSpawnedByFromFields(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"present", `{"spawned_by":"prior"}`, "prior"},
		{"trimmed", `{"spawned_by":"  prior  "}`, "prior"},
		{"absent", `{"task_id":"gd-x"}`, ""},
		{"empty-string", `{"spawned_by":""}`, ""},
		{"empty-fields", ``, ""},
		{"malformed", `{not json`, ""},
		{"non-string", `{"spawned_by":123}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := spawnedByFromFields([]byte(tc.raw)); got != tc.want {
				t.Fatalf("spawnedByFromFields(%q) = %q; want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// TestCreateBead_Fleet_SpawnChainBackstop_EndToEnd is the end-to-end proof:
// a header-less fleet-token CreateBead whose OWN fields.spawned_by references a
// prior agent bead (the shape a decision respawn produces after ka-0000007919)
// lands attributed to the human at the chain root — not fleet:slack-bridge.
// createBead itself lifts the seed from in.Fields and stamps ctx, so no caller
// wiring beyond the persisted field is required.
func TestCreateBead_Fleet_SpawnChainBackstop_EndToEnd(t *testing.T) {
	s, ms, _ := newTestServer()
	seedOwnerAgentBead(t, ms, "prior", map[string]any{"spawned_by": "root"})
	seedOwnerAgentBead(t, ms, "root", map[string]any{"slack_user_id": "U0OPERATOR1"})

	ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "slack-bridge"})
	bead, err := s.createBead(ctx, createBeadInput{
		Title:  "respawned crew",
		Type:   "agent",
		Fields: json.RawMessage(`{"agent":"respawned-crew","role":"crew","project":"gardenio","spawned_by":"prior"}`),
	})
	if err != nil {
		t.Fatalf("createBead error: %v", err)
	}
	if bead.OperatedBy != "slack:U0OPERATOR1" {
		t.Fatalf("bead.OperatedBy = %q; want slack:U0OPERATOR1 (backstop must attribute the human)", bead.OperatedBy)
	}
}

// TestCreateBead_Fleet_NoSpawnedBy_UnchangedSentinel pins the regression guard:
// a fleet CreateBead with no fields.spawned_by is unaffected by the backstop —
// it still binds the fleet sentinel exactly as before.
func TestCreateBead_Fleet_NoSpawnedBy_UnchangedSentinel(t *testing.T) {
	s, _, _ := newTestServer()

	ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "slack-bridge"})
	bead, err := s.createBead(ctx, createBeadInput{
		Title:  "service write with no lineage",
		Type:   "agent",
		Fields: json.RawMessage(`{"agent":"lonely-svc","role":"crew","project":"gardenio"}`),
	})
	if err != nil {
		t.Fatalf("createBead error: %v", err)
	}
	if bead.OperatedBy != "fleet:slack-bridge" {
		t.Fatalf("bead.OperatedBy = %q; want fleet:slack-bridge (no lineage → unchanged)", bead.OperatedBy)
	}
}

// TestWithSpawnChainSubject_EmptyIsNoOp pins that seeding with an empty/blank
// id does not install a ctx value — spawnChainSubjectFromContext still reads ""
// so the resolver skips the backstop entirely.
func TestWithSpawnChainSubject_EmptyIsNoOp(t *testing.T) {
	if got := spawnChainSubjectFromContext(WithSpawnChainSubject(context.Background(), "")); got != "" {
		t.Fatalf("empty seed installed value %q; want no-op empty", got)
	}
	if got := spawnChainSubjectFromContext(WithSpawnChainSubject(context.Background(), "   ")); got != "" {
		t.Fatalf("blank seed installed value %q; want no-op empty", got)
	}
	if got := spawnChainSubjectFromContext(WithSpawnChainSubject(context.Background(), "prior")); got != "prior" {
		t.Fatalf("seed = %q; want prior", got)
	}
}
