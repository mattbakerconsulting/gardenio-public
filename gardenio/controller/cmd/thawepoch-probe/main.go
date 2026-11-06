// Command thawepoch-probe is a TEST-ONLY driver for the gvisor-rehydrate-ack
// E2E module (test-gvisor-rehydrate-ack.sh, bead ka-0000006321). It exercises
// the REAL production thaw-epoch code path — internal/thawepoch.Watcher (the
// agent-side coop + gb consumers) and internal/thawepoch.WaitForAcks (the
// controller-side deadline gate) — against a thaw-state directory on disk,
// inside a runsc-restored agent on the gvisor-poc substrate.
//
// # Why this exists (the gap it closes)
//
// The 4 open P1 bugs on the post-restore rehydrate-ack RELEASE path
// (ka-0000007916 and friends) all share the same shape: a REAL frozen→thawed
// agent on a live-but-cold Pod B netns, where neither required consumer
// (ConsumerCoop, ConsumerGB) acks the current thaw epoch within the deadline,
// so the controller's WaitForAcks fail-closes and tears Pod B down. The
// contract is asserted at the unit layer (gate_test.go), at the byte-compat
// layer (test-gvisor-rehydration.sh re-implements the gate in bash), and in
// Rust (rehydration.rs vs live NATS) — but NOTHING drove the REAL Go gate +
// REAL Go watchers against a marker/ack round-trip on the actual substrate FS
// and asserted RELEASE-within-deadline. This probe is that missing piece: it
// runs the same WaitForAcks the controller runs, fed by the same Watcher loop
// the agent runs, so a regression in either fails this E2E rather than only the
// next prod canary.
//
// # What it does (modes)
//
//	-mode=release    Arm both consumers' watchers, write a thaw-epoch marker for
//	                 the next epoch, let the watchers ack it, and run the REAL
//	                 WaitForAcks gate. Asserts Released==true WITHIN deadline and
//	                 prints the elapsed ack latency. This is the FIXED behaviour
//	                 the 4 bugs break: both consumers ack the current epoch and
//	                 the controller releases the agent inside the budget.
//
//	-mode=failclose  Arm + ack ONLY ConsumerGB, leave ConsumerCoop silent, and
//	                 run WaitForAcks with a short deadline. Asserts Released==false
//	                 and that the failed-consumer list is exactly {coop} — the
//	                 negative control proving the gate genuinely HOLDS when a
//	                 required consumer never acks (the ka-0000007916 symptom), not
//	                 that it always passes.
//
// It is NOT part of any release Makefile / Dockerfile target — it is built from
// source on demand inside the E2E module (the same way trigger-probe is) and
// never ships in a product image.
//
// Output is a single line of `key=value` pairs on stdout (parsed by the shell
// module) plus human-readable detail on stderr. Exit code is 0 iff the asserted
// outcome held.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"gardenio/controller/internal/thawepoch"
)

func main() {
	var (
		mode       = flag.String("mode", "release", "release | failclose")
		stateDir   = flag.String("state-dir", "", "thaw-state dir (marker + ack files live here)")
		deadlineMS = flag.Int64("deadline-ms", int64(thawepoch.DefaultDeadline/time.Millisecond), "rehydration deadline in ms")
		podUID     = flag.String("pod-uid", "pod-B-probe", "pod UID stamped into the marker (Pod B identity)")
		node       = flag.String("node", "node-probe", "node name stamped into the marker")
	)
	flag.Parse()

	if *stateDir == "" {
		fmt.Fprintln(os.Stderr, "thawepoch-probe: -state-dir is required")
		os.Exit(2)
	}
	if err := os.MkdirAll(*stateDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "thawepoch-probe: mkdir state dir: %v\n", err)
		os.Exit(2)
	}

	switch *mode {
	case "release":
		runRelease(*stateDir, *deadlineMS, *podUID, *node)
	case "failclose":
		runFailClose(*stateDir, *podUID, *node)
	default:
		fmt.Fprintf(os.Stderr, "thawepoch-probe: unknown mode %q (want release|failclose)\n", *mode)
		os.Exit(2)
	}
}

// runRelease drives the happy path the 4 P1 bugs break: both required consumers
// arm a REAL Watcher, the controller writes the marker for the next epoch, both
// watchers run their (Stage-0 signal-only) Handler and write an OK ack, and the
// REAL WaitForAcks gate releases the agent strictly within the deadline.
func runRelease(stateDir string, deadlineMS int64, podUID, node string) {
	deadline := time.Duration(deadlineMS) * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 1. Both consumers arm their watchers BEFORE the marker is written — this
	//    is the armed-sentinel the controller's pre-freeze guard keys on
	//    (ArmedPath), and the live Watcher loop that will observe the marker and
	//    ack it. We run the genuine thawepoch.Watcher with a nil Handler, which
	//    is the Stage-0 signal-only behaviour: a present marker for a fresh epoch
	//    is acked OK. (The downstream cred-remint / NATS-reconnect handlers slot
	//    into this same Watcher in prod; here we assert the SIGNAL + GATE
	//    mechanics the release depends on.)
	consumers := []string{thawepoch.ConsumerCoop, thawepoch.ConsumerGB}
	for _, c := range consumers {
		if err := thawepoch.WriteArmed(stateDir, c); err != nil {
			fail("release", fmt.Sprintf("WriteArmed(%s): %v", c, err))
		}
		w := &thawepoch.Watcher{
			StateDir:     stateDir,
			Consumer:     c,
			PollInterval: 100 * time.Millisecond, // tight so the ack lands well inside the deadline
		}
		go func() { _ = w.Run(ctx) }()
	}

	// 2. The controller writes the marker for the NEXT epoch (monotonic), then
	//    blocks on the REAL gate.
	epoch, err := thawepoch.NextEpoch(stateDir)
	if err != nil {
		fail("release", fmt.Sprintf("NextEpoch: %v", err))
	}
	marker := thawepoch.Marker{
		Epoch:      epoch,
		PodUID:     podUID,
		Node:       node,
		TS:         time.Now().UTC(),
		FrozenAt:   time.Now().UTC().Add(-100 * time.Second),
		DeadlineMS: deadlineMS,
	}
	if err := thawepoch.Write(stateDir, marker); err != nil {
		fail("release", fmt.Sprintf("Write marker epoch %d: %v", epoch, err))
	}

	// 3. Run the controller-side gate exactly as freezethaw does: required =
	//    {gb, coop}, deadline from the marker. This is the assertion the bugs
	//    violate on prod — that BOTH consumers ack THIS epoch within the budget.
	start := time.Now()
	res := thawepoch.WaitForAcks(ctx, stateDir, epoch, thawepoch.GateOptions{
		Required: consumers,
		Deadline: deadline,
	})
	elapsed := time.Since(start)

	line := fmt.Sprintf("THAWEPOCH_PROBE mode=release epoch=%d released=%t elapsed_ms=%d deadline_ms=%d failed=[%s]",
		res.Epoch, res.Released, res.Elapsed.Milliseconds(), deadlineMS, strings.Join(res.FailedConsumers, ","))
	fmt.Println(line)

	if !res.Released {
		fail("release", fmt.Sprintf("gate did NOT release epoch %d within %s (failed consumers: %s) — the ka-0000007916 symptom",
			epoch, deadline, strings.Join(res.FailedConsumers, ", ")))
	}
	// Defence-in-depth: the wall-clock the gate actually took must be inside the
	// deadline (Released can only be true within deadline, but assert the number
	// explicitly so a future gate refactor that returns stale state is caught).
	if elapsed > deadline {
		fail("release", fmt.Sprintf("gate released but wall-clock %s exceeded deadline %s", elapsed.Round(time.Millisecond), deadline))
	}
	// Confirm BOTH consumers actually wrote an OK ack for THIS epoch (not a
	// replayed older one) — the precise property WaitForAcks gates on.
	for _, c := range consumers {
		ack, ok, err := thawepoch.ReadAck(stateDir, c)
		if err != nil || !ok {
			fail("release", fmt.Sprintf("consumer %s wrote no ack despite release", c))
		}
		if ack.Epoch != epoch || !ack.OK {
			fail("release", fmt.Sprintf("consumer %s ack epoch=%d ok=%t, want epoch=%d ok=true", c, ack.Epoch, ack.OK, epoch))
		}
	}
	fmt.Fprintf(os.Stderr, "  ✓ release: both consumers (coop, gb) acked epoch %d; WaitForAcks RELEASED in %dms (deadline %dms)\n",
		epoch, res.Elapsed.Milliseconds(), deadlineMS)
	os.Exit(0)
}

// runFailClose is the negative control: only ConsumerGB acks, ConsumerCoop stays
// silent (the exact prod symptom — one consumer never acks the epoch). The REAL
// gate must HOLD (Released==false) and report coop as the failed consumer, NOT
// release a half-rehydrated agent. A short deadline keeps the test fast.
func runFailClose(stateDir, podUID, node string) {
	// Short deadline — we WANT this to time out; no point waiting the full 60s.
	deadline := 2 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	epoch, err := thawepoch.NextEpoch(stateDir)
	if err != nil {
		fail("failclose", fmt.Sprintf("NextEpoch: %v", err))
	}
	marker := thawepoch.Marker{
		Epoch:      epoch,
		PodUID:     podUID,
		Node:       node,
		TS:         time.Now().UTC(),
		DeadlineMS: deadline.Milliseconds(),
	}
	if err := thawepoch.Write(stateDir, marker); err != nil {
		fail("failclose", fmt.Sprintf("Write marker epoch %d: %v", epoch, err))
	}

	// Only gb arms + acks. coop is deliberately absent.
	gbWatcher := &thawepoch.Watcher{
		StateDir:     stateDir,
		Consumer:     thawepoch.ConsumerGB,
		PollInterval: 100 * time.Millisecond,
	}
	go func() { _ = gbWatcher.Run(ctx) }()

	res := thawepoch.WaitForAcks(ctx, stateDir, epoch, thawepoch.GateOptions{
		Required: []string{thawepoch.ConsumerGB, thawepoch.ConsumerCoop},
		Deadline: deadline,
	})

	failed := append([]string(nil), res.FailedConsumers...)
	sort.Strings(failed)
	line := fmt.Sprintf("THAWEPOCH_PROBE mode=failclose epoch=%d released=%t elapsed_ms=%d deadline_ms=%d failed=[%s]",
		res.Epoch, res.Released, res.Elapsed.Milliseconds(), deadline.Milliseconds(), strings.Join(failed, ","))
	fmt.Println(line)

	if res.Released {
		fail("failclose", "gate RELEASED with coop silent — a half-rehydrated agent would resume authenticated work (gate not fail-closing)")
	}
	if len(failed) != 1 || failed[0] != thawepoch.ConsumerCoop {
		fail("failclose", fmt.Sprintf("expected failed consumers == [coop], got [%s]", strings.Join(failed, ", ")))
	}
	fmt.Fprintf(os.Stderr, "  ✓ failclose: coop never acked epoch %d; WaitForAcks HELD the gate (fail-closed) as designed\n", epoch)
	os.Exit(0)
}

func fail(mode, msg string) {
	fmt.Printf("THAWEPOCH_PROBE mode=%s released=false error=%q\n", mode, msg)
	fmt.Fprintf(os.Stderr, "  ✗ %s: %s\n", mode, msg)
	os.Exit(1)
}
