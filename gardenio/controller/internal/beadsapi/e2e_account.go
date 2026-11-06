package beadsapi

// E2E test-account detection (bug ka-0000005167).
//
// The gardenio E2E suite seeds throwaway type=account beads on every shard run
// (tests/e2e/scripts/test-account-auth.sh, test-credential-rotation.sh,
// test-cred-rotation-resume.sh, test-litellm-cost-tracking.sh,
// test-usage-tracking.sh, and tests/e2e/claudeless/trigger-source-account-fires.toml).
// Those beads flow through the SAME /costs projection as real credential accounts
// — the on-page cards, Top spenders / Hottest / Throttled rollups, the explorer's
// by-account rollup, and the Accounts dropdown — so Matt saw "a bunch of ETE test
// accounts in the buy account stuff". This is the account-cost analogue of the
// fixture-gardener bug ka-0000008054 (see beads-viewer/gardener_fixture.go): a test
// fixture polluting a live operator surface. This predicate is the single
// chokepoint that decides whether an account bead is E2E test scaffolding that
// must NOT appear on any live cost surface, and — via the producer (metrics
// collectAccounts) — that must never even become a gauge series.
//
// Signal robustness, matched as ANY-of-three so a fixture can never slip through
// when one marker is missing (the E2E harnesses are NOT uniform — two stamp no
// e2e: label, one uses a title that does not start with "e2e-"):
//
//  1. TITLE PREFIX — "e2e-" or "probe-acct-". Every harness account title starts
//     with one of these (e2e-acct-*, e2e-cred-*, e2e-rot-*, e2e-chain-*, e2e-bulk-*,
//     e2e-drain-*, e2e-cost-tracking-*, e2e-account-*, and probe-acct-$$ which does
//     NOT start with e2e-). This is the ONLY signal available at the PromQL layer
//     (the gauge carries only {account,env} = the title), so it is kept strict and
//     hyphen-anchored: a real account merely beginning with the letters "e2e" (e.g.
//     "e2eflow-prod") is NOT matched — over-filtering real spend is worse than the
//     original bug (ka-0000005167 acceptance criterion 4).
//
//  2. LABEL PREFIX — "e2e:" or "e2e-tag:". The e2e:$TAG / e2e:claudeless / e2e-tag:$TAG
//     labels several harnesses stamp. Robust but NOT universal: test-account-auth.sh
//     and test-usage-tracking.sh stamp no e2e: label at all (the latter carries only
//     project:gardenio, indistinguishable from a real account by label), which is why
//     a label-only filter does not close the bug.
//
//  3. E2E ACTOR — created_by / operated_by of an E2E harness identity: the
//     run-shard oidc handle (oidc:e2e-shard-N@example.com), the KD_ACTOR the
//     harnesses export (e2e-test, e2e-rwx), or the browser-e2e spec runner. Matched
//     precisely (exact match for the bare KD_ACTOR values, substring only for the
//     clearly-namespaced handles) so a REAL account authored by an agent whose name
//     merely contains "e2e-test" — e.g. "gardenio-e2e-test-accounts-c5n" — is not
//     swept in.

import (
	"slices"
	"strings"
)

// e2eAccountTitlePrefixes are the reserved title prefixes every E2E harness names
// its throwaway accounts with. Hyphen-anchored so a real account that merely
// begins with the letters "e2e" is not matched. Compared case-insensitively
// against a trimmed title.
var e2eAccountTitlePrefixes = []string{"e2e-", "probe-acct-"}

// e2eAccountLabelPrefixes are the namespaced label prefixes the E2E harnesses
// stamp (e2e:$TAG, e2e:claudeless, e2e-tag:$TAG, e2e:true). Matched as a prefix so
// any future e2e: variant is excluded automatically. NOT universal — two harnesses
// stamp no e2e: label — hence the belt-and-suspenders title/actor signals.
var e2eAccountLabelPrefixes = []string{"e2e:", "e2e-tag:"}

// e2eActorSubstrings are the clearly-namespaced E2E identity handles matched as a
// substring of created_by / operated_by: the run-shard oidc handle
// (oidc:e2e-shard-N@example.com) and the browser-e2e spec runner
// (gardenio-browser-e2e-spec-*, mirroring gardener_fixture.go). These cannot collide
// with a real agent/human handle.
var e2eActorSubstrings = []string{"e2e-shard", "browser-e2e-spec"}

// e2eActorExact are the bare KD_ACTOR values the account harnesses export
// (test-account-auth.sh / test-usage-tracking.sh set KD_ACTOR=e2e-test;
// run-shard.sh sets KD_ACTOR=e2e-rwx). Matched EXACTLY (not as a substring) so a
// real account authored by an agent whose name merely contains one of these —
// e.g. "gardenio-e2e-test-accounts-c5n" — is not a false positive.
var e2eActorExact = []string{"e2e-test", "e2e-rwx"}

// IsE2EAccountBead reports whether an account bead is E2E test scaffolding that
// must be excluded from every live cost surface and never emitted as a gauge
// series. A nil bead is not-a-fixture. See the file header for the three-signal
// rationale and the no-false-positive guarantee (ka-0000005167).
func IsE2EAccountBead(b *BeadDetail) bool {
	if b == nil {
		return false
	}
	title := strings.ToLower(strings.TrimSpace(b.Title))
	for _, p := range e2eAccountTitlePrefixes {
		if strings.HasPrefix(title, p) {
			return true
		}
	}
	for _, l := range b.Labels {
		ll := strings.ToLower(strings.TrimSpace(l))
		for _, p := range e2eAccountLabelPrefixes {
			if strings.HasPrefix(ll, p) {
				return true
			}
		}
	}
	return slices.ContainsFunc([]string{b.CreatedBy, b.OperatedBy}, isE2EActor)
}

// isE2EActor reports whether a created_by / operated_by handle belongs to an E2E
// harness identity. Namespaced handles match as a substring; the bare KD_ACTOR
// values match only EXACTLY, so a real agent name containing "e2e-test" is not
// swept in (see e2eActorExact).
func isE2EActor(handle string) bool {
	h := strings.ToLower(strings.TrimSpace(handle))
	if h == "" {
		return false
	}
	for _, m := range e2eActorSubstrings {
		if strings.Contains(h, m) {
			return true
		}
	}
	return slices.Contains(e2eActorExact, h)
}
