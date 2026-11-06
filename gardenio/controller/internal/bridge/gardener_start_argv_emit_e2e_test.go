//go:build e2e

// Emitter for the `gb gardener start` bridge-argv contract e2e (bead
// ka-0000001242, parent ka-0000000958; related ka-0000006141).
//
// ── The gap this closes ───────────────────────────────────────────────────────
//
// The documented `@gardenio <epic-id>` Slack entrypoint shells out to
// `gb gardener start <epic>`; the argv it hands to exec is built by the PURE
// function gardenerStartArgs (bot_mentions.go), and it MUST carry
// --i-read-the-gardener-instructions or the read-the-guide gate (gardener.go,
// ka-0000007415) refuses and the entrypoint fails for every user — the exact
// bug that shipped as ka-0000006141 and was fixed in 7c6aba74a.
//
// Coverage before this was UNIT-ONLY (bot_mentions_test.go TestGardenerStartArgs,
// a reflect.DeepEqual on the argv slice). The one e2e that LOOKS like it covers
// this — claudeless/gardener-mention-entrypoint.toml — HARDCODES the flag in its
// shell script, a hand-copied replica of the argv that never calls
// gardenerStartArgs. Revert the flag in gardenerStartArgs and that scenario stays
// GREEN. The replica can silently drift from the real argv, which is precisely
// how ka-0000006141 shipped in the first place.
//
// ── Why an emitter, not a hardcoded flag list (the vacuity trap) ──────────────
//
// The e2e's whole value is that the argv is EMITTED BY the production
// gardenerStartArgs — the SAME symbol the bridge's exec path calls — not
// re-typed by the test. So this file simply calls gardenerStartArgs and prints
// its output token-by-token; the shell module (test-gardener-start-argv-contract.sh)
// then runs `gb <that argv>` against the live daemon and asserts it survives the
// real CLI flag parser + gate and reaches a create. If gardenerStartArgs ever
// drops the ack flag (or reorders/renames a token so the real parser rejects
// it), the emitted argv changes and the live `gb` call fails — the drift the
// unit test's own DeepEqual can catch, but which no live surface exercised.
//
// gardenerStartArgs is UNEXPORTED, so only an in-package file can reach it
// without a production-visible export. This file is `//go:build e2e`-tagged so
// it stays off the default build/lint/test surface (`go build ./...`,
// `go test ./...`, golangci-lint — none pass `-tags e2e`) and rides the SAME
// warm e2e-tagged ./internal/bridge/ build that the subagent-stop e2e module
// already compiles on shard 0 (subagentstop_e2e_test.go) — no new probe binary,
// no new CI build step.
//
// Contract: this emitter is the PRODUCER (gardenerStartArgs at HEAD, the symbol
// the bridge calls); the PATH `gb` the shell module invokes is the CONSUMER (the
// real flag parser + gate at HEAD). Feeding producer→consumer pins the seam the
// hardcoded-replica fixture cannot.
//
// Output contract (parsed by test-gardener-start-argv-contract.sh):
//   ARGV_TOKEN <token>        one line per argv token, in order
//   ARGV_END   <n>            token count, terminator
//   HAS_ACK    yes|no         whether --i-read-the-gardener-instructions is present
// The emitter never touches the network or the daemon — it is a pure print of
// the production argv builder. It takes the SAME (beadID, operatedBy) inputs the
// bridge passes so the emitted argv is the real production shape.

package bridge

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
)

// gardenerStartAckFlag is the ack flag the read-the-guide gate (ka-0000007415)
// requires. Pinned as a literal here — NOT sourced from gardenerStartArgs — so
// the HAS_ACK line is an INDEPENDENT check of the emitted argv, not a tautology
// that would report "yes" no matter what gardenerStartArgs emits.
const gardenerStartAckFlag = "--i-read-the-gardener-instructions"

// TestEmitGardenerStartArgv prints the argv the bridge's epic-mention entrypoint
// would hand to `gb` for a gardener start, taken verbatim from the production
// gardenerStartArgs. The shell module reads these lines and runs `gb <argv>`
// against the live daemon.
//
// Inputs are supplied via env so the shell module controls the fresh epic id
// (the idempotency trap: `gb gardener start` is idempotent, so the exit-code
// assertion is only meaningful on a never-before-started epic):
//
//	GARDENER_EMIT_BEAD_ID      required — the (fresh) epic id
//	GARDENER_EMIT_OPERATED_BY  optional — operator alias; empty => ownerless argv
func TestEmitGardenerStartArgv(t *testing.T) {
	beadID := strings.TrimSpace(os.Getenv("GARDENER_EMIT_BEAD_ID"))
	if beadID == "" {
		// PROBE_SKIP mirrors the subagent-stop probe's self-skip contract so a
		// misconfigured harness is loud, never a silent pass.
		fmt.Println("PROBE_SKIP: GARDENER_EMIT_BEAD_ID unset — nothing to emit")
		t.Skip("GARDENER_EMIT_BEAD_ID unset")
	}
	operatedBy := strings.TrimSpace(os.Getenv("GARDENER_EMIT_OPERATED_BY"))

	// THE PRODUCTION CALL. gardenerStartArgs is the exact symbol
	// gardenerStartRunner (bot_mentions.go) invokes before exec'ing `gb`. We do
	// not re-type any token — a drift in the flag here is a drift in what the
	// Slack entrypoint would actually run.
	argv := gardenerStartArgs(beadID, operatedBy)

	// Sanity: the argv must lead with the gardener-start verb pair so the shell
	// module is running the command it thinks it is.
	if len(argv) < 3 || argv[0] != "gardener" || argv[1] != "start" {
		t.Fatalf("emitted argv does not begin with `gardener start <bead>`: %v", argv)
	}
	if argv[2] != beadID {
		t.Fatalf("emitted argv bead token = %q, want %q", argv[2], beadID)
	}

	for _, tok := range argv {
		fmt.Printf("ARGV_TOKEN %s\n", tok)
	}
	fmt.Printf("ARGV_END %d\n", len(argv))

	if slices.Contains(argv, gardenerStartAckFlag) {
		fmt.Println("HAS_ACK yes")
	} else {
		fmt.Println("HAS_ACK no")
	}
}
