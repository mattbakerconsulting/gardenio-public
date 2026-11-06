package podmanager

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func sampleSnapshot(agent string) Snapshot {
	return Snapshot{
		AgentName:      agent,
		BeadID:         "gd-abc123",
		Namespace:      "gardenio-gvisor",
		PodName:        "crew-" + agent,
		Node:           "ip-10-30-1-2.ec2.internal",
		ImagePath:      "/var/lib/gardenio/ckpt/" + agent,
		BundlePath:     "/var/lib/gardenio/bundle/" + agent,
		SandboxCID:     "sandboxcid",
		AgentCID:       "agentcid",
		PodUID:         "pod-uid-1",
		ContainerCount: 2,
		SpecFiles: map[string][]byte{
			"agent/config.json":   []byte(`{"process":{"cwd":"/home/agent/workspace"}}`),
			"agent/.overlay-opts": []byte("lowerdir=/var/lib/gardenio/bundle/" + agent + "/agent/lower\n"),
		},
		PodTemplate: corev1.PodSpec{NodeName: "ip-10-30-1-2.ec2.internal"},
		CreatedAt:   time.Date(2026, 6, 9, 23, 12, 45, 0, time.UTC),
	}
}

func TestConfigMapSnapshotStore_RoundTrip(t *testing.T) {
	client := fake.NewClientset()
	store := NewConfigMapSnapshotStore(client, "gardenio")
	ctx := context.Background()

	snap := sampleSnapshot("gvisor-canary-3mo")
	if err := store.Put(ctx, snap); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := store.Get(ctx, snap.AgentName)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.AgentName != snap.AgentName || got.BeadID != snap.BeadID || got.Node != snap.Node {
		t.Errorf("round-trip mismatch: got %+v", got)
	}
	if got.ContainerCount != 2 {
		t.Errorf("ContainerCount = %d, want 2", got.ContainerCount)
	}
	if string(got.SpecFiles["agent/.overlay-opts"]) != string(snap.SpecFiles["agent/.overlay-opts"]) {
		t.Errorf("SpecFiles overlay-opts not preserved: %q", got.SpecFiles["agent/.overlay-opts"])
	}
	if !got.CreatedAt.Equal(snap.CreatedAt) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, snap.CreatedAt)
	}
	// Consumed must round-trip (ka-0000001878): the re-freeze guard and the
	// existence predicates read it off the durable record on a fresh controller.
	if got.Consumed {
		t.Errorf("Consumed should round-trip as false for an unconsumed snapshot, got true")
	}
	consumed := snap
	consumed.Consumed = true
	if err := store.Put(ctx, consumed); err != nil {
		t.Fatalf("Put consumed: %v", err)
	}
	got2, err := store.Get(ctx, snap.AgentName)
	if err != nil {
		t.Fatalf("Get consumed: %v", err)
	}
	if !got2.Consumed {
		t.Errorf("Consumed=true must round-trip through the durable ConfigMap store")
	}
}

// TestConfigMapSnapshotStore_SurvivesRestart is the regression test for
// ka-0000001694: a SECOND store instance over the SAME API server (modeling a
// controller pod restart that drops all in-process state) must still observe the
// snapshot. The in-memory store failed exactly here.
func TestConfigMapSnapshotStore_SurvivesRestart(t *testing.T) {
	client := fake.NewClientset()
	ctx := context.Background()

	// Freeze on "instance 1".
	store1 := NewConfigMapSnapshotStore(client, "gardenio")
	if err := store1.Put(ctx, sampleSnapshot("agentX")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Controller restart: a brand-new store with no in-process map.
	store2 := NewConfigMapSnapshotStore(client, "gardenio")
	got, err := store2.Get(ctx, "agentX")
	if err != nil {
		t.Fatalf("Get after restart: %v (snapshot lost — the bug)", err)
	}
	if got.AgentName != "agentX" {
		t.Errorf("got %q, want agentX", got.AgentName)
	}
}

func TestConfigMapSnapshotStore_PutOverwrites(t *testing.T) {
	client := fake.NewClientset()
	store := NewConfigMapSnapshotStore(client, "gardenio")
	ctx := context.Background()

	snap := sampleSnapshot("agentY")
	if err := store.Put(ctx, snap); err != nil {
		t.Fatalf("first Put: %v", err)
	}
	snap.Node = "ip-10-30-9-9.ec2.internal"
	snap.BeadID = "gd-updated"
	if err := store.Put(ctx, snap); err != nil {
		t.Fatalf("second Put (overwrite): %v", err)
	}
	got, err := store.Get(ctx, "agentY")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Node != "ip-10-30-9-9.ec2.internal" || got.BeadID != "gd-updated" {
		t.Errorf("overwrite not applied: got node=%q bead=%q", got.Node, got.BeadID)
	}
}

func TestConfigMapSnapshotStore_GetMissing(t *testing.T) {
	store := NewConfigMapSnapshotStore(fake.NewClientset(), "gardenio")
	if _, err := store.Get(context.Background(), "nope"); err == nil {
		t.Fatal("Get of absent snapshot should error")
	}
}

func TestConfigMapSnapshotStore_DeleteAbsentIsNoop(t *testing.T) {
	store := NewConfigMapSnapshotStore(fake.NewClientset(), "gardenio")
	if err := store.Delete(context.Background(), "nope"); err != nil {
		t.Errorf("Delete of absent snapshot should be a no-op, got %v", err)
	}
}

func TestConfigMapSnapshotStore_ListAndDelete(t *testing.T) {
	client := fake.NewClientset()
	store := NewConfigMapSnapshotStore(client, "gardenio")
	ctx := context.Background()

	for _, a := range []string{"a1", "a2", "a3"} {
		if err := store.Put(ctx, sampleSnapshot(a)); err != nil {
			t.Fatalf("Put %s: %v", a, err)
		}
	}
	list, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("List len = %d, want 3", len(list))
	}

	if err := store.Delete(ctx, "a2"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Get(ctx, "a2"); err == nil {
		t.Error("a2 should be gone after Delete")
	}
	list, err = store.List(ctx)
	if err != nil {
		t.Fatalf("List after delete: %v", err)
	}
	if len(list) != 2 {
		t.Errorf("List len after delete = %d, want 2", len(list))
	}
}

func TestConfigMapToken(t *testing.T) {
	// The token feeds a ConfigMap name, so it must be DNS-1123-safe: lowercase,
	// no '_', no '/'. sanitizeIdent flattens non-alnum to '_'; configMapToken
	// then lowercases and maps '_' → '-'.
	for _, in := range []string{"gvisor-canary-3mo", "Agent_Mixed", "", "weird/name.x"} {
		got := configMapToken(in)
		if got == "" {
			t.Errorf("configMapToken(%q) returned empty", in)
		}
		for _, c := range got {
			if c == '_' || c == '/' || (c >= 'A' && c <= 'Z') {
				t.Errorf("configMapToken(%q)=%q contains a disallowed char %q", in, got, c)
			}
		}
	}
}
