package main

// costs_trust.go — the C6 /costs trust strip (ka-0000008212, plan ka-0000003747,
// operator decision ka-0000000925: trust surfaces on BOTH Grafana and /costs).
//
// The cost-audit x-check sweep (C2, cmd/controller/xcheck_sweep.go) recomputes
// conservation PAIRS — the same quantity derived two independent ways — and
// publishes gardenio_xcheck_agreement{pair,env}, a corroboration score in [0,1]
// (1.0 = the two derivations agree exactly). This file surfaces those scores as
// a compact badge strip on /costs, so every cost number on the page is read
// WITH the corroboration of the pair(s) that qualify it — e.g. "agent
// attribution: 0.98 corroborated" — instead of as an unqualified single-source
// figure.
//
// Data source. One server-side instant query against the SAME vmsingle upstream
// the /costs headline and explorer already use (instantVectorQuery,
// costs_rollup.go). The browser never touches vmsingle directly.
//
// Honest degrade. The sweep publishes a pair ONLY when both sides were derived
// this cycle and actively clears pairs that go underivable (see the C2 doc,
// gardenio/docs/cost-xcheck-audit-sweep.md), so absence from the query result is
// a real "no corroboration right now" signal. An absent pair renders as a muted
// "unverified" badge — never green — and with no upstream at all (local dev)
// every badge reads unverified under a one-line note. A present score is judged
// against the same per-pair floor (or default) the C5 CostXCheckDrift alert uses.

import (
	"context"
	"sort"
	"strconv"
)

// costsTrustQuery selects every published corroboration score in one instant
// query. Per-pair extraction happens on the {pair} label client-side.
const costsTrustQuery = `gardenio_xcheck_agreement`

// Pair label values. These mirror the xcheckPair* constants in
// cmd/controller/xcheck_sweep.go (a different binary, so they cannot be
// imported) — the sweep's doc comment names this strip as a consumer, so a
// rename there must be mirrored here.
const (
	costsTrustPairAgentSpendParity = "agent-spend-parity" // P10 (C3, ka-0000000774)
	costsTrustPairBillConservation = "bill-conservation"  // P1
	// P3 settled-vs-bill was RETIRED (ka-0000000543): its side A (the at-close
	// counter gardenio_agent_spend_usd_total) went dead in prod, so it could not
	// agree with the live bill at any floor. P1 bill-conservation + P10
	// agent-spend-parity cover its ground. The sweep no longer publishes it.
	costsTrustPairTagConservation = "tag-conservation"  // P2
	costsTrustPairSessionsVsAgent = "sessions-vs-agent" // P4
	costsTrustPairRollupVsTree    = "rollup-vs-tree"    // P5
	costsTrustPairTokenParity     = "token-parity"      // P6
	// P7 estimate-vs-settled was retired (ka-0000007296): its side A (session
	// total_cost_usd, the in-pod JSONL estimate) has had no producer since coop
	// went cost-free (2026-06-06), so the pair never published a score. Removed
	// from the sweep, so it is removed here too.
	costsTrustPairInterruptParity = "interrupt-parity" // P9
)

// costsTrustDriftFloor is the DEFAULT agreement score below which a present
// pair with no per-pair override (see costsTrustPairFloors) is badged
// "drifting" instead of "corroborated". Mirrors the C5 vmalert CostXCheckDrift
// default threshold (helm/gardenio/values.yaml
// vmalert.gapAlerts.xcheckDrift.threshold = 0.95). The alert fires on
// `< threshold` sustained, so exactly the floor reads corroborated here too.
const costsTrustDriftFloor = 0.95

// costsTrustPairFloors mirrors helm/gardenio/values.yaml
// vmalert.gapAlerts.xcheckDrift.pairThresholds — the per-pair floor overrides
// the C5 alert judges each pair against. A pair NOT listed here uses
// costsTrustDriftFloor. These MUST stay in lockstep with values.yaml; the
// hardcoded-copy drift they replace is what broke this strip (ka-0000007295:
// c30892e46 retuned the alert's pairThresholds but not this file, so the strip
// judged bill-conservation (and the since-retired settled-vs-bill) against a
// stale 0.95 and painted known-structural gaps red).
// TestCostsTrustFloorsMatchValuesYAML fails if the
// two disagree, so a future retune of values.yaml cannot silently re-open the
// same drift.
//
// A pair whose floor is 0 is opted out of the alert entirely (agreement is
// clamped to [0,1], so `< 0` never fires): floorForPair returns 0 and the
// `< 0` comparison is unreachable, so it never drifts.
func costsTrustPairFloors() map[string]float64 {
	return map[string]float64{
		costsTrustPairBillConservation: 0.85, // P1 — /key/info lifetime spend + shared-key double-count read HIGH
		// P3 settled-vs-bill retired (ka-0000000543) and P7 estimate-vs-settled
		// retired (ka-0000007296) — neither carries a floor because the sweep no
		// longer publishes either; a stray override would fail the lockstep test.
	}
}

// floorForPair returns the drift floor for one pair: its per-pair override from
// costsTrustPairFloors, else the default costsTrustDriftFloor.
func floorForPair(pair string) float64 {
	if f, ok := costsTrustPairFloors()[pair]; ok {
		return f
	}
	return costsTrustDriftFloor
}

// costsTrustDashboardUID is the Grafana uid of the C5 x-check trust dashboard
// (helm/gardenio/dashboards/xcheck-trust.json) — the strip's drill-down target
// for per-pair side A/B history.
const costsTrustDashboardUID = "gardenio-xcheck-trust"

// costsTrustSweepDoc names the sweep's reference doc, surfaced in the
// no-data note so an operator seeing an all-unverified strip knows what feeds
// it and how to switch it on (GARDENIO_XCHECK_SWEEP_INTERVAL).
const costsTrustSweepDoc = "docs/cost-xcheck-audit-sweep.md"

// costsTrustPairSpec is one registered pair: its stable {pair} label value, a
// short human label for the badge, and what /costs number the pair qualifies
// (shown in the badge tooltip).
type costsTrustPairSpec struct {
	Pair      string
	Label     string
	Qualifies string
}

// costsTrustPairSpecs is the registry of known pairs in strip render order:
// the pairs qualifying headline /costs numbers first, sweep-health pairs last.
// A pair the sweep publishes that is NOT listed here still renders (appended
// alphabetically with its raw label value) so the strip never silently drops a
// published score.
func costsTrustPairSpecs() []costsTrustPairSpec {
	return []costsTrustPairSpec{
		{
			Pair:      costsTrustPairAgentSpendParity,
			Label:     "agent attribution",
			Qualifies: "per-agent cost attribution (cost explorer, per-agent chart): stateful settled counter vs stateless /spend/logs recompute",
		},
		{
			Pair:      costsTrustPairBillConservation,
			Label:     "bill conservation",
			Qualifies: "the LiteLLM bill (today total, fleet rate, account cards): per-account bill vs attributed+unattributed",
		},
		// P3 settled-vs-bill retired (ka-0000000543): its side A (the at-close
		// counter gardenio_agent_spend_usd_total) went dead, so it read structurally
		// low at any floor. P1 bill-conservation (the bill) + P10 agent-spend-parity
		// (per-agent settled attribution) cover its ground; the sweep stopped
		// publishing it. If a stale series lingers in vmsingle, the strip renders it
		// as an unregistered pair (appended alphabetically) rather than dropping it.
		{
			Pair:      costsTrustPairTagConservation,
			Label:     "tag conservation",
			Qualifies: "role/project tag rollups: the same bill summed per-role vs per-project",
		},
		{
			Pair:      costsTrustPairSessionsVsAgent,
			Label:     "sessions vs agent",
			Qualifies: "per-session settled costs vs per-agent LiteLLM deltas",
		},
		{
			Pair:      costsTrustPairRollupVsTree,
			Label:     "rollup vs tree",
			Qualifies: "gardener rollup totals vs a live sum over each gardener's agent tree",
		},
		{
			Pair:      costsTrustPairTokenParity,
			Label:     "token parity",
			Qualifies: "session token counts: gb-pushed vs coop-scraped counters",
		},
		{
			Pair:      costsTrustPairInterruptParity,
			Label:     "interrupt parity",
			Qualifies: "involuntary-death accounting (deaths counter vs bead close reasons) — a sweep-health signal, not a cost number on this page",
		},
	}
}

// Badge levels — also the CSS class suffixes (trust-ok / trust-drift /
// trust-unverified in costs.html).
const (
	costsTrustLevelOK         = "ok"
	costsTrustLevelDrift      = "drift"
	costsTrustLevelUnverified = "unverified"
)

// costsTrustBadge is one rendered badge: "<label>: <score> <levelWord>".
type costsTrustBadge struct {
	Pair      string
	Label     string
	Score     string // "0.98" — empty when the pair is absent
	HasScore  bool
	Level     string // costsTrustLevel* (CSS class suffix)
	LevelWord string // "corroborated" / "drifting" / "unverified"
	Title     string // tooltip: what the pair qualifies + how to read the score
}

// costsTrustView is the strip's view model.
type costsTrustView struct {
	// Queried is true when vmsingle answered the agreement query at all (even
	// with an empty vector). False = no upstream wired / upstream error.
	Queried bool
	// AnyPresent is true when at least one pair published a score this cycle.
	// False renders the all-unverified note under the strip.
	AnyPresent bool
	Badges     []costsTrustBadge
	// DashboardURL links the strip to the C5 x-check trust dashboard for
	// per-pair side A/B drill-down; empty when Grafana is unconfigured.
	DashboardURL string
	// SweepDoc backs the no-data note.
	SweepDoc string

	byPair map[string]*costsTrustBadge
}

// Badge returns the badge for a pair label value, or nil when unknown — nil
// renders as nothing under `{{with}}`, so a template callsite for a pair that
// was never registered nor published simply omits its inline badge.
func (v *costsTrustView) Badge(pair string) *costsTrustBadge {
	if v == nil {
		return nil
	}
	return v.byPair[pair]
}

// costsTrustScore is one pair's worst (lowest) agreement across envs, plus the
// env that produced it and how many distinct envs the sweep published for the
// pair. Carrying the worst env is what lets the badge NAME the diverging estate
// instead of silently masking it: the strip still judges on the LOWEST score
// (a healthy 1.0 must never hide another estate's divergence), but now an
// operator can see WHICH env is at fault instead of a bare red badge.
type costsTrustScore struct {
	Score    float64
	WorstEnv string // env label of the lowest sample ("" if the sample carried no env label)
	Envs     int    // distinct env labels seen for this pair (>1 ⇒ the badge names the worst)
}

// costsTrustScoresFromVector extracts pair → worst agreement score from the
// instant query's vector. When a shared vmsingle carries the same pair for more
// than one env, the LOWEST score wins (and its env is recorded) — the strip
// must never let a healthy estate's 1.0 mask another estate's divergence.
func costsTrustScoresFromVector(resp *vmVectorResponse) map[string]costsTrustScore {
	scores := map[string]costsTrustScore{}
	if resp == nil {
		return scores
	}
	envs := map[string]map[string]struct{}{} // pair → set of env labels seen
	for _, r := range resp.Data.Result {
		pair := r.Metric["pair"]
		if pair == "" {
			continue
		}
		v, ok := parseVMSampleValue(r.Value[1])
		if !ok {
			continue
		}
		// Agreement is clamped to [0,1] at the source (metrics.XCheckAgreement);
		// clamp defensively anyway so a rogue sample can't render "1.73".
		if v < 0 {
			v = 0
		}
		if v > 1 {
			v = 1
		}
		env := r.Metric["env"]
		if envs[pair] == nil {
			envs[pair] = map[string]struct{}{}
		}
		envs[pair][env] = struct{}{}
		if prev, seen := scores[pair]; !seen || v < prev.Score {
			scores[pair] = costsTrustScore{Score: v, WorstEnv: env}
		}
	}
	for pair, s := range scores {
		s.Envs = len(envs[pair])
		scores[pair] = s
	}
	return scores
}

// fetchCostsTrustScores runs the agreement instant query. ok=false when no
// upstream is wired or the round-trip failed (→ Queried=false, every badge
// unverified); a successful-but-empty vector is (empty map, true).
func (s *Server) fetchCostsTrustScores(ctx context.Context) (map[string]costsTrustScore, bool) {
	resp, ok := s.instantVectorQuery(ctx, costsTrustQuery)
	if !ok {
		return nil, false
	}
	return costsTrustScoresFromVector(resp), true
}

// buildCostsTrust reduces the fetched scores to the strip view model. PURE (no
// I/O) so badge-level rules are unit-testable. grafanaBase may be empty.
func buildCostsTrust(scores map[string]costsTrustScore, queried bool, grafanaBase string) *costsTrustView {
	v := &costsTrustView{
		Queried:  queried,
		SweepDoc: costsTrustSweepDoc,
		byPair:   map[string]*costsTrustBadge{},
	}
	if grafanaBase != "" {
		v.DashboardURL = grafanaBase + "/d/" + costsTrustDashboardUID
	}

	specs := costsTrustPairSpecs()
	known := map[string]bool{}
	for _, spec := range specs {
		known[spec.Pair] = true
		score, present := scores[spec.Pair]
		v.append(costsTrustBadgeFor(spec, score, present))
		if present {
			v.AnyPresent = true
		}
	}
	// Published-but-unregistered pairs (e.g. a pair added to the sweep before
	// this registry learns its label) still render, appended alphabetically,
	// so the strip never silently drops a live score.
	extra := make([]string, 0)
	for pair := range scores {
		if !known[pair] {
			extra = append(extra, pair)
		}
	}
	sort.Strings(extra)
	for _, pair := range extra {
		v.append(costsTrustBadgeFor(costsTrustPairSpec{
			Pair:      pair,
			Label:     pair,
			Qualifies: "unregistered x-check pair (published by the sweep but not yet described here)",
		}, scores[pair], true))
		v.AnyPresent = true
	}
	return v
}

// append adds a badge to both the ordered slice and the by-pair lookup.
func (v *costsTrustView) append(b costsTrustBadge) {
	v.Badges = append(v.Badges, b)
	v.byPair[b.Pair] = &v.Badges[len(v.Badges)-1]
}

// costsTrustBadgeFor applies the level rules to one pair, judging its score
// against the pair's OWN floor (floorForPair) — the same per-pair floor the C5
// CostXCheckDrift alert uses — not a single 0.95 constant:
//
//	absent                       → unverified (muted — NEVER green; no sweep data is not agreement)
//	present, < pair floor        → drifting (same judgement the CostXCheckDrift alert makes)
//	present, ≥ floor, tuned pair → corroborated, "structurally low within band" copy
//	present, ≥ default floor     → corroborated
//
// A pair with a tuned floor below the 0.95 default (bill-conservation 0.85)
// whose two sides are not built to reconcile within 5%
// reads GREEN when it clears its own floor — so a known structural gap no
// longer paints a red badge that discredits the headline (ka-0000007295). The
// tooltip copy still distinguishes "structurally low, within expected band"
// from a clean 1.00, and from "drifting", so a red badge always means something
// is actually wrong (dimension R). When the pair was published for more than
// one env, the badge names the worst env so a red never silently masks WHICH
// estate diverged.
func costsTrustBadgeFor(spec costsTrustPairSpec, score costsTrustScore, present bool) costsTrustBadge {
	b := costsTrustBadge{Pair: spec.Pair, Label: spec.Label}
	if !present {
		b.Level = costsTrustLevelUnverified
		b.LevelWord = "unverified"
		b.Title = spec.Qualifies + " — no x-check sample this cycle (sweep not publishing, or a side underivable); treat as unverified, not healthy."
		return b
	}
	b.Score = strconv.FormatFloat(score.Score, 'f', 2, 64)
	b.HasScore = true

	// Multi-env: the strip judged the LOWEST score across envs (a healthy 1.0
	// must not mask a diverging estate); name that env so the badge points at
	// the estate at fault instead of hiding which one it was.
	envNote := ""
	if score.Envs > 1 && score.WorstEnv != "" {
		envNote = " (lowest of " + strconv.Itoa(score.Envs) + " envs — worst: " + score.WorstEnv + ")"
	}

	floor := floorForPair(spec.Pair)
	switch {
	case score.Score < floor:
		b.Level = costsTrustLevelDrift
		b.LevelWord = "drifting"
		b.Title = spec.Qualifies + " — agreement " + b.Score + " is below this pair's " +
			strconv.FormatFloat(floor, 'f', 2, 64) +
			" floor: the two independent derivations disagree beyond what this pair tolerates, so this number is suspect until reconciled." + envNote
	case floor < costsTrustDriftFloor:
		// Above its OWN tuned floor but below the 0.95 default — a pair whose
		// two sides are structurally low by construction (bill-conservation).
		// Green, but the copy says so, so a ~0.9 reading is
		// not misread as a clean 1.0 nor as drifting.
		b.Level = costsTrustLevelOK
		b.LevelWord = "corroborated"
		b.Title = spec.Qualifies + " — agreement " + b.Score + " is structurally low but within this pair's expected band (its two sides are not built to reconcile within 5%; floor " +
			strconv.FormatFloat(floor, 'f', 2, 64) + ", not the default " +
			strconv.FormatFloat(costsTrustDriftFloor, 'f', 2, 64) + ")." + envNote
	default:
		b.Level = costsTrustLevelOK
		b.LevelWord = "corroborated"
		b.Title = spec.Qualifies + " — agreement " + b.Score + " (1.00 = two independent derivations agree exactly)." + envNote
	}
	return b
}
