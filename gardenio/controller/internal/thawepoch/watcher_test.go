package thawepoch

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// runWatcherBriefly runs w until cond returns true or timeout, then cancels.
func runWatcherBriefly(t *testing.T, w *Watcher, cond func() bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { _ = w.Run(ctx); close(done) }()

	deadline := time.After(3 * time.Second)
	for {
		if cond() {
			cancel()
			<-done
			return
		}
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatal("condition not met before timeout")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestWatcherRunsHandlerOncePerEpoch(t *testing.T) {
	dir := t.TempDir()
	var calls atomic.Int32
	var sawEpoch atomic.Int64
	w := &Watcher{
		StateDir:     dir,
		Consumer:     ConsumerCoop,
		PollInterval: 5 * time.Millisecond,
		Handler: func(ctx context.Context, m Marker) (Ack, error) {
			calls.Add(1)
			sawEpoch.Store(m.Epoch)
			return Ack{OK: true}, nil
		},
	}
	if err := Write(dir, Marker{Epoch: 1, PodUID: "b1"}); err != nil {
		t.Fatal(err)
	}

	runWatcherBriefly(t, w, func() bool {
		a, ok, _ := ReadAck(dir, ConsumerCoop)
		return ok && a.Epoch == 1 && a.OK
	})

	if got := calls.Load(); got != 1 {
		t.Fatalf("handler should run exactly once for epoch 1, ran %d", got)
	}
	if sawEpoch.Load() != 1 {
		t.Fatalf("handler saw wrong epoch")
	}
}

func TestWatcherRerunsOnEpochIncrement(t *testing.T) {
	dir := t.TempDir()
	var mu sync.Mutex
	var epochs []int64
	w := &Watcher{
		StateDir:     dir,
		Consumer:     ConsumerGB,
		PollInterval: 5 * time.Millisecond,
		Handler: func(ctx context.Context, m Marker) (Ack, error) {
			mu.Lock()
			epochs = append(epochs, m.Epoch)
			mu.Unlock()
			return Ack{OK: true}, nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { _ = w.Run(ctx); close(done) }()

	for _, e := range []int64{1, 2, 3} {
		if err := Write(dir, Marker{Epoch: e, PodUID: "b"}); err != nil {
			t.Fatal(err)
		}
		// Wait for this epoch to be acked before bumping.
		waitFor(t, func() bool {
			a, ok, _ := ReadAck(dir, ConsumerGB)
			return ok && a.Epoch == e && a.OK
		})
	}
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(epochs) != 3 || epochs[0] != 1 || epochs[1] != 2 || epochs[2] != 3 {
		t.Fatalf("expected epochs [1 2 3], got %v", epochs)
	}
}

func TestWatcherFailClosedDoesNotLatchEpoch(t *testing.T) {
	dir := t.TempDir()
	var calls atomic.Int32
	var fail atomic.Int32
	fail.Store(1)
	w := &Watcher{
		StateDir:     dir,
		Consumer:     ConsumerCoop,
		PollInterval: 5 * time.Millisecond,
		Handler: func(ctx context.Context, m Marker) (Ack, error) {
			calls.Add(1)
			if fail.Load() == 1 {
				return Ack{OK: false, Error: "nats unreachable"}, nil
			}
			return Ack{OK: true}, nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { _ = w.Run(ctx); close(done) }()

	if err := Write(dir, Marker{Epoch: 1, PodUID: "b"}); err != nil {
		t.Fatal(err)
	}
	// Wait until at least one failing ack is written.
	waitFor(t, func() bool {
		a, ok, _ := ReadAck(dir, ConsumerCoop)
		return ok && a.Epoch == 1 && !a.OK
	})
	// Flip to success: because the epoch was NOT latched on fail, the watcher
	// re-runs the same epoch on the next poll and now acks OK.
	fail.Store(0)
	waitFor(t, func() bool {
		a, ok, _ := ReadAck(dir, ConsumerCoop)
		return ok && a.Epoch == 1 && a.OK
	})
	cancel()
	<-done

	if calls.Load() < 2 {
		t.Fatalf("expected handler re-run after fail-closed, calls=%d", calls.Load())
	}
}

func TestWatcherRecoversAckedEpochOnRestart(t *testing.T) {
	dir := t.TempDir()
	// Simulate a prior run that already acked epoch 2.
	if err := Write(dir, Marker{Epoch: 2, PodUID: "b"}); err != nil {
		t.Fatal(err)
	}
	if err := WriteAck(dir, Ack{Epoch: 2, Consumer: ConsumerGB, OK: true}); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	w := &Watcher{
		StateDir:     dir,
		Consumer:     ConsumerGB,
		PollInterval: 5 * time.Millisecond,
		Handler: func(ctx context.Context, m Marker) (Ack, error) {
			calls.Add(1)
			return Ack{OK: true}, nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_ = w.Run(ctx)

	if calls.Load() != 0 {
		t.Fatalf("watcher should not re-run already-acked epoch 2 on restart, calls=%d", calls.Load())
	}
}

func TestWatcherNotifyTriggersImmediateCheck(t *testing.T) {
	dir := t.TempDir()
	var calls atomic.Int32
	w := &Watcher{
		StateDir: dir,
		Consumer: ConsumerCoop,
		// Long poll so only Notify can trigger promptly.
		PollInterval: 10 * time.Second,
		Handler: func(ctx context.Context, m Marker) (Ack, error) {
			calls.Add(1)
			return Ack{OK: true}, nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { _ = w.Run(ctx); close(done) }()

	// Let Run initialize the notify channel.
	time.Sleep(20 * time.Millisecond)
	if err := Write(dir, Marker{Epoch: 1, PodUID: "b"}); err != nil {
		t.Fatal(err)
	}
	w.Notify()
	waitFor(t, func() bool {
		a, ok, _ := ReadAck(dir, ConsumerCoop)
		return ok && a.OK
	})
	cancel()
	<-done
	if calls.Load() == 0 {
		t.Fatal("Notify should have triggered the handler before the long poll tick")
	}
}

// TestWatcherDriftBackstopSelfTriggers guards ka-0000001515: when the observed
// pod identity drifts with no fresher marker, the watcher rehydrates on its
// own instead of running on stale credentials and connections.
func TestWatcherDriftBackstopSelfTriggers(t *testing.T) {
	dir := t.TempDir()
	var calls atomic.Int32
	var mu sync.Mutex
	identity := "pod-a"
	w := &Watcher{
		StateDir:     dir,
		Consumer:     ConsumerCoop,
		PollInterval: 5 * time.Millisecond,
		Drift: func() string {
			mu.Lock()
			defer mu.Unlock()
			return identity
		},
		Handler: func(ctx context.Context, m Marker) (Ack, error) {
			calls.Add(1)
			return Ack{OK: true}, nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { _ = w.Run(ctx); close(done) }()

	// Seed: first observations are pod-a, no marker — must NOT trigger.
	time.Sleep(40 * time.Millisecond)
	if calls.Load() != 0 {
		t.Fatalf("drift should not fire on initial identity, calls=%d", calls.Load())
	}
	// Now identity changes with no marker written — backstop must fire.
	mu.Lock()
	identity = "pod-b"
	mu.Unlock()
	waitFor(t, func() bool { return calls.Load() >= 1 })
	cancel()
	<-done
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		if cond() {
			return
		}
		select {
		case <-deadline:
			t.Fatal("waitFor: condition not met before timeout")
		case <-time.After(5 * time.Millisecond):
		}
	}
}
