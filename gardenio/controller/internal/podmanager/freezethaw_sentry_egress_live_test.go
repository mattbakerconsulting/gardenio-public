package podmanager

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

// freezethaw_sentry_egress_live_test.go — BEHAVIORAL regression guard for the LIVE
// [6c-1] sentry-egress classifier embedded in freezethaw_node.go's thaw template
// (task gd-ADHqX151z9, follow-up of bug ka-0000000514 / MR !4984 ka-0000008207).
//
// WHY THIS EXISTS — the gap it closes
// -----------------------------------
// The thaw template's [6c-1] classifier is the load-bearing link-only floor:
//
//	if [ "$SENTRY_DEFROUTE" = no ] || [ "${SENTRY_NONLO:-0}" = 0 ]; then
//	  log "FATAL: ... LINK-ONLY ..."; echo "FT_ROOT_RC=128"; exit 1
//	fi
//
// Two OTHER test surfaces touch this area but NEITHER executes that live condition:
//
//   - freezethaw_sentry_egress_test.go asserts the rendered script CONTAINS the
//     fragments ("FT_SENTRY_NONLO=", etc.) — a pure string-presence check — and
//     exercises the Go-SIDE post-state parser on synthetic FT_SENTRY_* stdout. A
//     direct edit to the bash CONDITION (e.g. "= 0" weakened to "-gt 9", the "no"
//     branch dropped, or the whole if removed) would still satisfy every substring
//     assertion and the Go parser, so it would pass GREEN while silently diverging.
//
//   - scripts/gvisor/sentry-egress-check.sh is a SEPARATE offline reimplementation
//     (seg_verdict, arithmetic "-lt 1") gated by test-gvisor-sentry-egress. Editing
//     the LIVE template does not touch that script, so that gate stays green too.
//
// This test removes that blind spot: it RENDERS the real thaw template via
// buildThawScript, EXTRACTS the actual [6c-1] if/fi branch out of the rendered
// bytes, and RUNS it under bash with synthetic SENTRY_DEFROUTE / SENTRY_NONLO. It
// asserts the BEHAVIOR — fail-close (FT_ROOT_RC=128, exit 1) when the sentry is
// link-only (non-lo==0 or defroute==no), pass-through when routable (non-lo>=1),
// and pass-through (never fail-close) on a flaked/inconclusive read (unknown/-1).
// Because it executes the LIVE extracted branch rather than re-implementing the
// check, a divergent edit to the real thaw condition is caught HERE, on every
// `make test` / test-gardenio run — not days later on a burned canary.

// extractLive6c1Classifier pulls the [6c-1] link-only classifier if/fi block out of
// the rendered thaw script. It anchors on the live condition line
// (`if [ "$SENTRY_DEFROUTE" ...`) — NOT on a hard-coded operator — so the extracted
// branch is whatever the template actually ships, and the behavioral assertions
// below decide pass/fail. Anchoring is intentionally strict on the leading
// `if [ "$SENTRY_DEFROUTE"`: if a refactor renames/removes that classifier, the
// anchor is gone and the test fails loud (AC#3, "fail if the live classifier is
// changed/removed"). The matching `fi` is found by if/fi depth counting so a future
// nested conditional inside the branch cannot truncate the extraction.
func extractLive6c1Classifier(t *testing.T, script string) string {
	t.Helper()
	lines := strings.Split(script, "\n")

	ifIdx := -1
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), `if [ "$SENTRY_DEFROUTE"`) {
			ifIdx = i
			break
		}
	}
	if ifIdx < 0 {
		t.Fatalf("LIVE [6c-1] classifier not found: no `if [ \"$SENTRY_DEFROUTE\" ...` line in the rendered thaw template — the link-only floor was renamed or removed (this test must be updated to track it, or the classifier was wrongly deleted)")
	}

	fiIdx := -1
	depth := 0
	for i := ifIdx; i < len(lines); i++ {
		tl := strings.TrimSpace(lines[i])
		if tl == "if" || strings.HasPrefix(tl, "if ") {
			depth++
		}
		if tl == "fi" {
			depth--
			if depth == 0 {
				fiIdx = i
				break
			}
		}
	}
	if fiIdx < 0 {
		t.Fatalf("LIVE [6c-1] classifier malformed: no matching `fi` for the `if [ \"$SENTRY_DEFROUTE\" ...` branch at line %d", ifIdx+1)
	}

	block := strings.Join(lines[ifIdx:fiIdx+1], "\n")
	// Sanity: the extracted branch must be the fail-close path. If a divergent edit
	// kept the anchor but dropped the FT_ROOT_RC=128 / exit, the behavioral run below
	// would also catch it, but assert here too for a precise failure message.
	if !strings.Contains(block, "FT_ROOT_RC=128") {
		t.Fatalf("LIVE [6c-1] classifier no longer emits FT_ROOT_RC=128 — its fail-close was weakened/removed. Extracted block:\n%s", block)
	}
	return block
}

// runLive6c1 executes the extracted live classifier branch under bash with the given
// synthetic sentry-read values, returning combined output and whether it fail-closed
// (non-zero exit). A stub `log` mirrors the template's logger so the branch runs
// standalone; a trailing `FT_ROOT_RC=0` marks the routable fall-through path.
func runLive6c1(t *testing.T, classifier, defroute, nonlo string) (out string, failedClosed bool) {
	t.Helper()
	harness := fmt.Sprintf(`set -uo pipefail
log() { :; }                 # stub the thaw template's logger
SENTRY_DEFROUTE='%s'
SENTRY_NONLO='%s'
%s
echo "FT_ROOT_RC=0"          # fall-through: the LIVE [6c-1] classifier did NOT fail-close
`, defroute, nonlo, classifier)

	cmd := exec.Command("bash", "-c", harness)
	b, err := cmd.CombinedOutput()
	return string(b), err != nil
}

// TestThawScript_Live6c1ClassifierBehavior is the core behavioral regression: it runs
// the LIVE extracted [6c-1] branch over the full input matrix and asserts the
// link-only fail-close vs. routable pass-through contract. A divergent edit to the
// real thaw condition flips one of these outcomes and fails the test.
func TestThawScript_Live6c1ClassifierBehavior(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; the live-classifier behavioral test requires bash (CI has it)")
	}

	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	natsHost, natsPort := splitEgressTarget("nats://gardenio-nats:4222", "4222")
	beadsHost, beadsPort := splitEgressTarget("gardenio-beads:8080", "8080")
	tw := m.buildThawScript(thawScriptParams{
		PlaceholderPod: "podb", BPodUID: "buid", ASandboxCID: "asb", AAgentCID: "aag",
		APodUID: "apod", ImagePath: "/img", BundlePath: "/bndl", ContainerCount: 2, RunscTimeout: 90,
		NatsHost: natsHost, NatsPort: natsPort, BeadsHost: beadsHost, BeadsPort: beadsPort,
	})
	classifier := extractLive6c1Classifier(t, tw)

	for _, tc := range []struct {
		name            string
		defroute, nonlo string
		wantFailClose   bool
		reason          string
	}{
		// AC#1 core: FT_SENTRY_NONLO==0 with a default route present MUST fail-close.
		{"non-lo==0 fail-closes (the AC#1 case)", "yes", "0", true,
			"a sentry with zero non-loopback ifaces is link-only — must FT_ROOT_RC=128"},
		// The other half of the OR: no default route MUST fail-close.
		{"defroute==no fail-closes", "no", "1", true,
			"a sentry with no default route is link-only — must FT_ROOT_RC=128"},
		{"defroute==no AND non-lo==0 fail-closes", "no", "0", true,
			"both link-only signals — must FT_ROOT_RC=128"},
		// AC#1 core: non-lo>=1 with a default route MUST pass (routable).
		{"routable non-lo==1 passes", "yes", "1", false,
			"default route + one non-lo iface is routable — must NOT fail-close"},
		{"routable non-lo==2 passes", "yes", "2", false,
			"default route + multiple non-lo ifaces is routable — must NOT fail-close"},
		// Documented inconclusive posture: a flaked in-sentry read (unknown/-1) must
		// NEVER fail-close — the 180s ack gate is the backstop, a blip must not cold-start.
		{"flaked read (unknown/-1) does NOT fail-close", "unknown", "-1", false,
			"an inconclusive in-sentry exec must fall through, not fail-close"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, failedClosed := runLive6c1(t, classifier, tc.defroute, tc.nonlo)
			if failedClosed != tc.wantFailClose {
				t.Fatalf("defroute=%s nonlo=%s: failedClosed=%v want %v (%s)\n--- live classifier output ---\n%s",
					tc.defroute, tc.nonlo, failedClosed, tc.wantFailClose, tc.reason, out)
			}
			if tc.wantFailClose {
				if !strings.Contains(out, "FT_ROOT_RC=128") {
					t.Errorf("expected FT_ROOT_RC=128 on fail-close, got:\n%s", out)
				}
				if strings.Contains(out, "FT_ROOT_RC=0") {
					t.Errorf("fail-close path must NOT reach the routable fall-through marker FT_ROOT_RC=0, got:\n%s", out)
				}
			} else {
				if !strings.Contains(out, "FT_ROOT_RC=0") {
					t.Errorf("expected the routable fall-through marker FT_ROOT_RC=0, got:\n%s", out)
				}
				if strings.Contains(out, "FT_ROOT_RC=128") {
					t.Errorf("routable path must NOT emit the link-only fail-close FT_ROOT_RC=128, got:\n%s", out)
				}
			}
		})
	}
}

// TestThawScript_Live6c1MutationCaught is a meta-assertion that the behavioral test
// above has TEETH: it proves that if the live classifier's condition were silently
// inverted/weakened (the exact divergence the task guards against), the extracted
// branch would change behavior and the assertions would catch it. We simulate a
// divergent edit by running a WEAKENED classifier (one that never fail-closes on
// non-lo==0) through the same harness and asserting it would FAIL the contract —
// confirming the test is not a tautology that passes regardless of the condition.
func TestThawScript_Live6c1MutationCaught(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	// A divergent edit: someone weakens the floor so a link-only sentry (non-lo==0)
	// no longer fail-closes (e.g. flips the operator). The extracted-and-run approach
	// MUST observe the wrong behavior.
	weakened := `if [ "$SENTRY_DEFROUTE" = no ]; then
  log "FATAL: link-only"
  echo "FT_ROOT_RC=128"
  exit 1
fi`
	out, failedClosed := runLive6c1(t, weakened, "yes", "0")
	// With the weakened condition, non-lo==0 + defroute=yes does NOT fail-close — which
	// is exactly the regression. If our behavioral run reported a fail-close here, the
	// harness would be lying; assert it correctly reports the (wrong) pass-through, so
	// the real test above — which demands a fail-close for this input — would catch it.
	if failedClosed {
		t.Fatalf("harness sanity: a weakened classifier that drops the non-lo==0 floor must be observed as NOT fail-closing on non-lo==0, but runLive6c1 reported a fail-close:\n%s", out)
	}
	if !strings.Contains(out, "FT_ROOT_RC=0") {
		t.Fatalf("harness sanity: weakened classifier should fall through to FT_ROOT_RC=0 for non-lo==0, got:\n%s", out)
	}
}

// TestThawScript_Live6c1SentinelEmission proves the rendered template emits the
// FT_SENTRY_* sentinels the classifier reads, immediately before the live [6c-1]
// branch — so the extracted classifier is wired to the values the in-sentry probe
// produces, not to dead vars. (String-presence here is complementary to the
// behavioral run above; together they pin both the wiring and the verdict.)
func TestThawScript_Live6c1SentinelEmission(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	tw := m.buildThawScript(thawScriptParams{
		PlaceholderPod: "podb", BPodUID: "buid", ASandboxCID: "asb", AAgentCID: "aag",
		APodUID: "apod", ImagePath: "/img", BundlePath: "/bndl", ContainerCount: 2, RunscTimeout: 90,
	})
	iDef := strings.Index(tw, `echo "FT_SENTRY_DEFROUTE=$SENTRY_DEFROUTE"`)
	iNon := strings.Index(tw, `echo "FT_SENTRY_NONLO=$SENTRY_NONLO"`)
	iIf := strings.Index(tw, `if [ "$SENTRY_DEFROUTE"`)
	if iDef < 0 || iNon < 0 || iIf < 0 {
		t.Fatalf("missing sentinel/classifier wiring: FT_SENTRY_DEFROUTE idx=%d FT_SENTRY_NONLO idx=%d if idx=%d", iDef, iNon, iIf)
	}
	if !(iDef < iNon && iNon < iIf) {
		t.Errorf("sentinels must be emitted before the [6c-1] classifier reads them: FT_SENTRY_DEFROUTE idx=%d, FT_SENTRY_NONLO idx=%d, if idx=%d", iDef, iNon, iIf)
	}
}
