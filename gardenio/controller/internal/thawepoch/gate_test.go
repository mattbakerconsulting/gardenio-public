package thawepoch

import (
	"context"
	"testing"
	"time"
)

func TestGateReleasesWhenAllRequiredAck(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, Marker{Epoch: 1, PodUID: "b"}); err != nil {
		t.Fatal(err)
	}
	// Both required consumers ack OK before the gate even runs.
	mustAck(t, dir, Ack{Epoch: 1, Consumer: ConsumerGB, OK: true})
	mustAck(t, dir, Ack{Epoch: 1, Consumer: ConsumerCoop, OK: true})

	res := WaitForAcks(context.Background(), dir, 1, GateOptions{Deadline: time.Second})
	if !res.Released {
		t.Fatalf("expected release, got %+v (err=%v)", res, res.Err())
	}
	if res.Err() != nil {
		t.Fatalf("Released result should have nil Err, got %v", res.Err())
	}
}

func TestGateReleasesWhenAcksRaceIn(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, Marker{Epoch: 1, PodUID: "b"}); err != nil {
		t.Fatal(err)
	}
	mustAck(t, dir, Ack{Epoch: 1, Consumer: ConsumerGB, OK: true})
	// coop acks after a short delay, while the gate is polling.
	go func() {
		time.Sleep(40 * time.Millisecond)
		_ = WriteAck(dir, Ack{Epoch: 1, Consumer: ConsumerCoop, OK: true})
	}()
	res := WaitForAcks(context.Background(), dir, 1, GateOptions{Deadline: 2 * time.Second, PollInterval: 5 * time.Millisecond})
	if !res.Released {
		t.Fatalf("expected release after racing ack, got %+v", res)
	}
}

func TestGateFailsClosedOnDeadline(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, Marker{Epoch: 1, PodUID: "b"}); err != nil {
		t.Fatal(err)
	}
	// Only gb acks; coop never does.
	mustAck(t, dir, Ack{Epoch: 1, Consumer: ConsumerGB, OK: true})

	res := WaitForAcks(context.Background(), dir, 1, GateOptions{Deadline: 80 * time.Millisecond, PollInterval: 5 * time.Millisecond})
	if res.Released {
		t.Fatal("expected fail-closed, got released")
	}
	if res.Err() == nil {
		t.Fatal("fail-closed result should have non-nil Err")
	}
	if len(res.FailedConsumers) != 1 || res.FailedConsumers[0] != ConsumerCoop {
		t.Fatalf("expected coop in failed consumers, got %v", res.FailedConsumers)
	}
}

func TestGateFailsClosedOnConsumerFailure(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, Marker{Epoch: 1, PodUID: "b"}); err != nil {
		t.Fatal(err)
	}
	mustAck(t, dir, Ack{Epoch: 1, Consumer: ConsumerGB, OK: true})
	mustAck(t, dir, Ack{Epoch: 1, Consumer: ConsumerCoop, OK: false, Error: "sts 403"})

	res := WaitForAcks(context.Background(), dir, 1, GateOptions{Deadline: 80 * time.Millisecond})
	if res.Released {
		t.Fatal("expected fail-closed when a consumer acks OK=false")
	}
	if len(res.FailedConsumers) != 1 || res.FailedConsumers[0] != ConsumerCoop {
		t.Fatalf("expected coop failed, got %v", res.FailedConsumers)
	}
}

func TestGateIgnoresStaleEpochAck(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, Marker{Epoch: 2, PodUID: "b"}); err != nil {
		t.Fatal(err)
	}
	// Acks for the PRIOR epoch must not satisfy the gate for epoch 2.
	mustAck(t, dir, Ack{Epoch: 1, Consumer: ConsumerGB, OK: true})
	mustAck(t, dir, Ack{Epoch: 1, Consumer: ConsumerCoop, OK: true})

	res := WaitForAcks(context.Background(), dir, 2, GateOptions{Deadline: 60 * time.Millisecond})
	if res.Released {
		t.Fatal("stale-epoch acks must not release the gate")
	}
}

func TestGateRespectsMarkerDeadline(t *testing.T) {
	dir := t.TempDir()
	// Marker carries a short deadline; gate with no explicit deadline uses it.
	if err := Write(dir, Marker{Epoch: 1, PodUID: "b", DeadlineMS: 60}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	res := WaitForAcks(context.Background(), dir, 1, GateOptions{PollInterval: 5 * time.Millisecond})
	if res.Released {
		t.Fatal("expected fail-closed (no acks)")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("gate should honor the 60ms marker deadline, waited %s", elapsed)
	}
}

func TestGateCustomRequiredSet(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, Marker{Epoch: 1, PodUID: "b"}); err != nil {
		t.Fatal(err)
	}
	// Only require coop.
	mustAck(t, dir, Ack{Epoch: 1, Consumer: ConsumerCoop, OK: true})
	res := WaitForAcks(context.Background(), dir, 1, GateOptions{
		Required: []string{ConsumerCoop},
		Deadline: 100 * time.Millisecond,
	})
	if !res.Released {
		t.Fatalf("expected release with custom required set, got %+v", res)
	}
}

func TestGateContextCancelAbortsEarly(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, Marker{Epoch: 1, PodUID: "b"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	res := WaitForAcks(ctx, dir, 1, GateOptions{Deadline: 10 * time.Second, PollInterval: 5 * time.Millisecond})
	if res.Released {
		t.Fatal("expected not released on cancel")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("cancel should abort well before the 10s deadline")
	}
}

func mustAck(t *testing.T, dir string, a Ack) {
	t.Helper()
	if err := WriteAck(dir, a); err != nil {
		t.Fatalf("WriteAck: %v", err)
	}
}
