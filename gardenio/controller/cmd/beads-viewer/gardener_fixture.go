package main

// Fixture-gardener detection (bug ka-0000008054).
//
// The beads-viewer /gardeners index, /gardener/{id} detail page, and the /maps
// gardener picker all render any `role:gardener` trigger/schedule bead as a live
// gardener. That swept in ka-0000004925 — the deterministic E2E fixture bead
// whose true title is "[E2E fixture] gardener bead" but which renders as "E2E
// demo-drive gardener" with entirely hardcoded synthetic run data (agent_ids
// run1/run2/run3, epic gd-demoEpic, MR gd-demoMR, fixed 2026-05-01 timestamps)
// presented as if it were a real gardener. A test fixture polluting the live
// fleet surface is a correctness/accuracy bug: an operator reading /gardeners
// cannot tell the synthetic row from the real ones.
//
// isFixtureGardenerBead is the single chokepoint that decides whether a gardener
// bead is a test fixture that must NOT appear on any live gardeners surface. It
// is consulted by loadGardenerRows (so the fixture never lands on /gardeners or
// the /maps picker, which share that projection) and by the per-gardener detail
// + map handlers (so a deep link to the fixture id 404s instead of rendering
// synthetic data).
//
// Signal robustness, strongest first. We match on ANY of three independent
// signals so a fixture can never slip through if one marker is missing, and —
// critically — so we never exclude a REAL gardener (verified against the live
// fleet: 0 of 58 real gardeners carry any of these):
//
//  1. A `fixture:` LABEL prefix (e.g. "fixture:e2e:gardener"). This is the most
//     robust signal: it is an intentional, stable marker the E2E seeder stamps
//     on its fixtures, it is namespaced so it cannot collide with an operational
//     label, and it is the one signal a fixture author would never forget. Real
//     gardeners never carry a fixture: label.
//
//  2. The "[E2E fixture]" TITLE marker. The fixture's true bead title literally
//     begins with this bracketed tag; it is the human-facing "this is not real"
//     stamp. Belt-and-suspenders in case a future fixture is seeded without the
//     label.
//
//  3. A `created_by` of the browser-e2e spec runner (gardenio-browser-e2e-spec).
//     Anything that agent authors is by definition test scaffolding. This
//     catches a fixture that somehow shipped with neither the label nor the
//     title marker.

import (
	"os"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// renderFixtureGardenersEnv is the opt-in flag that disables the fixture-gardener
// exclusion so the BEHAVIORAL E2E harness (gardenio/tests/beads-viewer-e2e) can
// render the very fixture gardener it deliberately seeds (label
// "fixture:e2e:gardener", title "[E2E fixture] gardener bead") and navigate to
// /gardener/{id} + /gardeners expecting a 200 render.
//
// Without this, bug ka-0000008054's exclusion (which keeps test fixtures off the
// LIVE production gardeners surface) 404s the suite's own seeded fixture, red-
// masking every gardener_detail.spec.js + gardeners_index.spec.js test (bug
// gd-R8wc4mOHL3). The flag is OFF by default, so production behavior — and every
// Go unit test that asserts the exclusion — is unchanged; only the e2e
// playwright.config.js webServer env sets it. It pairs naturally with
// BEADS_VIEWER_DEV_AUTH_BYPASS, the suite's other harness-only opt-in.
const renderFixtureGardenersEnv = "BEADS_VIEWER_RENDER_FIXTURE_GARDENERS"

// renderFixtureGardenersEnabled reports whether the caller opted into rendering
// fixture gardeners on the live surfaces (E2E harness only — see the env doc).
func renderFixtureGardenersEnabled() bool {
	return parseBoolEnv(os.Getenv(renderFixtureGardenersEnv))
}

// excludeFixtureGardener reports whether a gardener bead is a test fixture that
// must be EXCLUDED from the live gardeners surfaces right now. It is the
// env-aware wrapper around the pure isFixtureGardenerBead predicate: a fixture is
// excluded unless the behavioral-e2e render opt-in is set. All live-surface call
// sites (detail handler, /gardeners index projection, /maps picker) go through
// this so the opt-in is honored consistently.
func excludeFixtureGardener(b *beadsapi.BeadDetail) bool {
	return isFixtureGardenerBead(b) && !renderFixtureGardenersEnabled()
}

// fixtureLabelPrefix is the namespaced label prefix the E2E seeder stamps on its
// fixture beads (e.g. "fixture:e2e:gardener"). Matching on the prefix — not the
// exact label — means any future fixture: variant is excluded automatically.
const fixtureLabelPrefix = "fixture:"

// fixtureTitleMarker is the bracketed human-facing tag a fixture bead's title
// carries ("[E2E fixture] gardener bead"). Compared case-insensitively against a
// prefix-trimmed title so leading whitespace or case drift cannot defeat it.
const fixtureTitleMarker = "[e2e fixture]"

// fixtureCreatedByMarker is a substring of the browser-e2e spec runner's
// created_by handle (gardenio-browser-e2e-spec-*). Anything that account authors
// is test scaffolding, never a live gardener.
const fixtureCreatedByMarker = "browser-e2e-spec"

// isFixtureGardenerBead reports whether a gardener bead is an E2E/test fixture
// that must be excluded from every live gardeners surface (index, detail, map).
// A nil bead is treated as not-a-fixture (nothing to exclude — callers handle
// nil separately). See the file header for the three-signal rationale.
func isFixtureGardenerBead(b *beadsapi.BeadDetail) bool {
	if b == nil {
		return false
	}
	for _, l := range b.Labels {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(l)), fixtureLabelPrefix) {
			return true
		}
	}
	if strings.Contains(strings.ToLower(strings.TrimSpace(b.Title)), fixtureTitleMarker) {
		return true
	}
	if strings.Contains(strings.ToLower(b.CreatedBy), fixtureCreatedByMarker) {
		return true
	}
	return false
}
