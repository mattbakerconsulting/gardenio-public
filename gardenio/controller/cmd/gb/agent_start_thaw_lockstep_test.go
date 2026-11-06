package main

// agent_start_thaw_lockstep_test.go — cross-language lockstep canary for the
// per-attempt thaw connect-clamp default.
//
// gb (thawConnectTimeoutDefault, this package) and coop
// (THAW_CONNECT_TIMEOUT, coop/crates/cli/src/transport/nats_supervisor.rs) both
// clamp each per-attempt connect on the SAME rehydrate-ack deadline (the C3
// gate releases iff BOTH consumers ack within THAW_REHYDRATE_DEADLINE). If the
// two defaults drift, the un-widened side becomes the one that fail-closes the
// gate, so the defaults MUST be kept in lockstep. This test reads the coop
// const out of source and asserts it equals gb's default, failing loudly on
// drift. See gardenio/docs/gvisor-runsc-prod.md "Per-attempt thaw connect clamp".
//
// The test is CWD-independent (it locates coop via runtime.Caller, not the
// process working dir) and skips — never fails — when the coop source is absent
// (e.g. a controller-only checkout), so it can only catch a real value drift,
// never break an unrelated build.

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"testing"
	"time"
)

// coopThawConnectTimeoutRe matches the coop const, e.g.
//
//	const THAW_CONNECT_TIMEOUT: Duration = Duration::from_millis(2000);
var coopThawConnectTimeoutRe = regexp.MustCompile(
	`THAW_CONNECT_TIMEOUT\s*:\s*Duration\s*=\s*Duration::from_millis\(\s*(\d+)\s*\)`)

// coopRetryFractionRe matches the coop const, e.g.
//
//	const RECONNECT_RETRY_FRACTION: f32 = 0.85;
var coopRetryFractionRe = regexp.MustCompile(
	`RECONNECT_RETRY_FRACTION\s*:\s*f32\s*=\s*([0-9.]+)\s*;`)

func TestThawConnectTimeoutLockstepWithCoop(t *testing.T) {
	// Resolve coop's nats_supervisor.rs relative to THIS source file, not the
	// test's working directory (go test runs with CWD == package dir, but we do
	// not want to depend on that). From gardenio/controller/cmd/gb/ the monorepo
	// root is four levels up.
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Skip("runtime.Caller unavailable; cannot locate coop source")
	}
	gbDir := filepath.Dir(thisFile)
	coopPath := filepath.Join(gbDir,
		"..", "..", "..", "..",
		"coop", "crates", "cli", "src", "transport", "nats_supervisor.rs")

	src, err := os.ReadFile(coopPath)
	if err != nil {
		// Controller-only checkout (no coop tree): this canary cannot run, but
		// it must never break the build. The doc note and the in-source comments
		// on both consts remain the backstop.
		t.Skipf("coop source not present at %s (%v); skipping cross-lang lockstep check", coopPath, err)
	}

	m := coopThawConnectTimeoutRe.FindSubmatch(src)
	if m == nil {
		t.Fatalf("could not find THAW_CONNECT_TIMEOUT in %s — did coop rename the const or change its form? "+
			"Update coopThawConnectTimeoutRe and keep the lockstep with gb's thawConnectTimeoutDefault.", coopPath)
	}

	coopMS, err := strconv.Atoi(string(m[1]))
	if err != nil {
		t.Fatalf("parsing coop THAW_CONNECT_TIMEOUT millis %q: %v", m[1], err)
	}
	coopDefault := time.Duration(coopMS) * time.Millisecond

	if coopDefault != thawConnectTimeoutDefault {
		t.Fatalf("thaw per-attempt connect clamp defaults have DRIFTED — they MUST stay in lockstep "+
			"(both gate the same rehydrate-ack deadline; see docs/gvisor-runsc-prod.md):\n"+
			"  gb   thawConnectTimeoutDefault (cmd/gb/agent_start_thaw.go)              = %v\n"+
			"  coop THAW_CONNECT_TIMEOUT      (crates/cli/src/transport/nats_supervisor.rs) = %v\n"+
			"Change BOTH consts together, or update this test if the divergence is intentional.",
			thawConnectTimeoutDefault, coopDefault)
	}
}

// TestThawAckHeadroomFractionLockstepWithCoop pins gb's ack-headroom fraction
// to coop's RECONNECT_RETRY_FRACTION (ka-0000003746 RANK5). Both consumers cap
// their retry budget below the marker deadline for the same reason — the ack
// write must land while the controller gate is still looking — and if the two
// drift, the consumer with the larger fraction becomes the one whose late-but-
// OK ack the gate misses.
func TestThawAckHeadroomFractionLockstepWithCoop(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Skip("runtime.Caller unavailable; cannot locate coop source")
	}
	coopPath := filepath.Join(filepath.Dir(thisFile),
		"..", "..", "..", "..",
		"coop", "crates", "cli", "src", "transport", "nats_supervisor.rs")

	src, err := os.ReadFile(coopPath)
	if err != nil {
		t.Skipf("coop source not present at %s (%v); skipping cross-lang lockstep check", coopPath, err)
	}

	m := coopRetryFractionRe.FindSubmatch(src)
	if m == nil {
		t.Fatalf("could not find RECONNECT_RETRY_FRACTION in %s — did coop rename the const or change its form? "+
			"Update coopRetryFractionRe and keep the lockstep with gb's thawAckHeadroomFraction.", coopPath)
	}
	coopFraction, err := strconv.ParseFloat(string(m[1]), 64)
	if err != nil {
		t.Fatalf("parsing coop RECONNECT_RETRY_FRACTION %q: %v", m[1], err)
	}

	if coopFraction != thawAckHeadroomFraction {
		t.Fatalf("thaw ack-headroom fractions have DRIFTED — they MUST stay in lockstep "+
			"(both reserve ack-write headroom under the same rehydrate gate):\n"+
			"  gb   thawAckHeadroomFraction   (cmd/gb/agent_start_thaw.go)                  = %v\n"+
			"  coop RECONNECT_RETRY_FRACTION  (crates/cli/src/transport/nats_supervisor.rs) = %v\n"+
			"Change BOTH consts together, or update this test if the divergence is intentional.",
			thawAckHeadroomFraction, coopFraction)
	}
}
