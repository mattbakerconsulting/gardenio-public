package thawepoch

import (
	"os"
	"testing"
	"time"
)

func TestWriteReadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := Marker{Epoch: 1, PodUID: "pod-b-uid", Node: "node-7", FrozenAt: time.Now().Add(-time.Minute).UTC()}
	if err := Write(dir, want); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, ok, err := Read(dir)
	if err != nil || !ok {
		t.Fatalf("Read: ok=%v err=%v", ok, err)
	}
	if got.Epoch != want.Epoch || got.PodUID != want.PodUID || got.Node != want.Node {
		t.Fatalf("round-trip mismatch: got %+v want %+v", got, want)
	}
	if got.TS.IsZero() {
		t.Fatal("Write should have stamped TS")
	}
}

func TestReadAbsentMarker(t *testing.T) {
	_, ok, err := Read(t.TempDir())
	if err != nil {
		t.Fatalf("Read absent: %v", err)
	}
	if ok {
		t.Fatal("expected ok=false for absent marker")
	}
}

func TestWriteRejectsNonPositiveEpoch(t *testing.T) {
	if err := Write(t.TempDir(), Marker{Epoch: 0}); err == nil {
		t.Fatal("expected error for epoch 0")
	}
}

func TestWriteEnforcesMonotonicity(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, Marker{Epoch: 5, PodUID: "a"}); err != nil {
		t.Fatalf("Write 5: %v", err)
	}
	// Equal epoch rejected.
	if err := Write(dir, Marker{Epoch: 5, PodUID: "b"}); err == nil {
		t.Fatal("expected rejection of equal epoch")
	}
	// Lower epoch rejected (re-frozen agent must not replay).
	if err := Write(dir, Marker{Epoch: 4, PodUID: "c"}); err == nil {
		t.Fatal("expected rejection of lower epoch")
	}
	// Higher epoch accepted.
	if err := Write(dir, Marker{Epoch: 6, PodUID: "d"}); err != nil {
		t.Fatalf("Write 6: %v", err)
	}
	got, _, _ := Read(dir)
	if got.Epoch != 6 || got.PodUID != "d" {
		t.Fatalf("expected epoch 6 pod d, got %+v", got)
	}
}

func TestNextEpoch(t *testing.T) {
	dir := t.TempDir()
	if n, err := NextEpoch(dir); err != nil || n != 1 {
		t.Fatalf("NextEpoch empty: n=%d err=%v", n, err)
	}
	if err := Write(dir, Marker{Epoch: 1}); err != nil {
		t.Fatal(err)
	}
	if n, err := NextEpoch(dir); err != nil || n != 2 {
		t.Fatalf("NextEpoch after 1: n=%d err=%v", n, err)
	}
}

func TestMarkerDeadline(t *testing.T) {
	if d := (Marker{}).Deadline(); d != DefaultDeadline {
		t.Fatalf("default deadline: got %s", d)
	}
	if d := (Marker{DeadlineMS: 2500}).Deadline(); d != 2500*time.Millisecond {
		t.Fatalf("override deadline: got %s", d)
	}
}

func TestAckRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := Ack{Epoch: 3, Consumer: ConsumerCoop, OK: true, Stages: map[string]string{"reconnect": "ok"}}
	if err := WriteAck(dir, want); err != nil {
		t.Fatalf("WriteAck: %v", err)
	}
	got, ok, err := ReadAck(dir, ConsumerCoop)
	if err != nil || !ok {
		t.Fatalf("ReadAck: ok=%v err=%v", ok, err)
	}
	if got.Epoch != 3 || !got.OK || got.Stages["reconnect"] != "ok" {
		t.Fatalf("ack mismatch: %+v", got)
	}
}

func TestWriteAckRequiresConsumer(t *testing.T) {
	if err := WriteAck(t.TempDir(), Ack{Epoch: 1}); err == nil {
		t.Fatal("expected error for empty consumer")
	}
}

func TestAtomicWriteLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, Marker{Epoch: 1, PodUID: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := WriteAck(dir, Ack{Epoch: 1, Consumer: ConsumerGB, OK: true}); err != nil {
		t.Fatal(err)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if len(e.Name()) > 0 && e.Name()[0] == '.' {
			t.Fatalf("leftover temp file: %s", e.Name())
		}
	}
	// Exactly the marker + one ack.
	if len(ents) != 2 {
		t.Fatalf("expected 2 files, got %d: %v", len(ents), ents)
	}
}

func TestArmedSentinelRoundTrip(t *testing.T) {
	dir := t.TempDir()

	// Absent before writing.
	if armed, err := IsArmed(dir, ConsumerCoop); err != nil || armed {
		t.Fatalf("IsArmed before write: armed=%v err=%v", armed, err)
	}

	if err := WriteArmed(dir, ConsumerCoop); err != nil {
		t.Fatalf("WriteArmed: %v", err)
	}
	if armed, err := IsArmed(dir, ConsumerCoop); err != nil || !armed {
		t.Fatalf("IsArmed after write: armed=%v err=%v", armed, err)
	}

	// Per-consumer isolation: coop's sentinel does not imply gb's.
	if armed, err := IsArmed(dir, ConsumerGB); err != nil || armed {
		t.Fatalf("IsArmed gb (never written): armed=%v err=%v", armed, err)
	}

	// Idempotent re-arm (restarted-not-thawed watcher).
	if err := WriteArmed(dir, ConsumerCoop); err != nil {
		t.Fatalf("WriteArmed (re-arm): %v", err)
	}

	// Stable filename matches the cross-language contract coop writes.
	if got := ArmedName(ConsumerCoop); got != "thaw-epoch.coop.armed" {
		t.Fatalf("ArmedName(coop) = %q, want thaw-epoch.coop.armed", got)
	}
}

func TestWriteArmedRequiresConsumer(t *testing.T) {
	if err := WriteArmed(t.TempDir(), "  "); err == nil {
		t.Fatal("expected error for blank consumer")
	}
}

func TestWriteArmedLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	if err := WriteArmed(dir, ConsumerCoop); err != nil {
		t.Fatal(err)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].Name() != ArmedName(ConsumerCoop) {
		t.Fatalf("expected exactly the armed sentinel, got %v", ents)
	}
}
