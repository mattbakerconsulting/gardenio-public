package main

// real_catalog_budget_test.go is the Phase-6 durable advice-hygiene guardrail
// for the context-crisis plan (epic gd-wSSncGLBCa / ka-0000003733). Where
// prime_size_budget_test.go pins the *rendered* gb-prime byte budget against a
// synthetic corpus, these tests pin the *matched* advice pool — what
// advice.AuditCatalog churns per (project, role) archetype, before any render
// budget clips it — against a snapshot of the REAL advice catalog.
//
// Why a snapshot and not the live daemon: CI has no beads daemon. To make a
// re-leak of the advice corpus fail CI we commit a frozen catalog snapshot
// (testdata/advice_catalog_snapshot.json) plus a frozen real-fleet archetype
// grid (testdata/fleet_archetypes_snapshot.json), and audit the corpus through
// the exact same production pipeline (advice.AuditCatalog over the real fleet)
// the controller and `gb advice audit` use. Two failure modes are then caught:
//
//   1. A delivery-logic regression — reverting the Phase-3 bare-project
//      broadcast deny, the supersession collapse, or the fixture/hippo drop —
//      re-inflates the same snapshot past budget.
//   2. A snapshot refresh that re-introduces corpus bloat — whoever runs
//      `-update-advice-snapshot` to refresh the fixture cannot land a corpus
//      that busts the per-archetype token budget or the per-project broadcast
//      count without turning this test red.
//
// Refresh the snapshot against a live daemon with:
//
//	go test ./cmd/gb -run TestRealCatalog -update-advice-snapshot \
//	    -args (env BEADS_HTTP_ADDR + BEADS_AUTH_TOKEN must be set)
//
// then commit the regenerated testdata/advice_catalog_snapshot.json. Keep the
// budgets below in step with the plan (ka-0000003733 Part A) and the
// per-project broadcast counts in step with the live corpus.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/advice"
	"gardenio/controller/internal/beadsapi"
)

// updateAdviceSnapshot regenerates testdata/advice_catalog_snapshot.json from
// the live daemon (BEADS_HTTP_ADDR + BEADS_AUTH_TOKEN) instead of asserting
// against the committed fixture. Mirrors the -update golden convention used by
// cmd/edge-auth (ocsf) and internal/bridge (beads-viewer links).
var updateAdviceSnapshot = flag.Bool("update-advice-snapshot", false,
	"regenerate testdata/advice_catalog_snapshot.json from the live daemon (needs BEADS_HTTP_ADDR + BEADS_AUTH_TOKEN)")

const adviceSnapshotPath = "testdata/advice_catalog_snapshot.json"

// adviceSnapshotMetaPath is the sidecar that records WHEN the frozen catalog
// snapshot was last regenerated from the live daemon, plus the bead/archetype
// counts it was captured with. It exists so a freshness assertion can turn a
// silently-drifting fixture into a hard CI failure: the budget test audits
// frozen bytes and cannot see live corpus growth, so without a staleness clock
// the guard can report affirmative green while the real corpus has already
// blown past the hard ceiling it exists to defend (ka-0000006923). The two
// files ALWAYS move together — regenerateAdviceSnapshot rewrites both under the
// same time.Now() stamp.
const adviceSnapshotMetaPath = "testdata/advice_catalog_snapshot_meta.json"

// Snapshot staleness ceilings (ka-0000006923). The budget test measures a
// frozen fixture, so it is blind to live corpus growth between refreshes; these
// bound how far the fixture may drift before CI forces a re-sync with reality.
//
//   - softSnapshotAge: a loud log — the fixture is getting old, refresh soon.
//   - hardSnapshotAge: a CI failure — the fixture is stale enough that the
//     budget verdict can no longer be trusted; someone MUST run
//     -update-advice-snapshot. A soft warning alone was the very trap this bug
//     came from (a "SOFT budget warning" logged inside a PASS that nobody
//     reads), so past the hard ceiling this fails, it does not merely warn.
//
// The soft t.Logf below is STILL invisible in a passing package (go test prints
// nothing without -v, CI shows a green check) — which is exactly how this went
// 0 -> fleet-wide P0 with no observable intermediate (ka-0000004418). The
// OBSERVABLE early-warning channel that fronts the soft ceiling now lives OUT of
// this passing test, in a scheduled GitLab alarm job (advice-snapshot-freshness-
// alarm) driving scripts/check-advice-snapshot-freshness.py, which reddens at
// softSnapshotAge a full week before this hard ceiling reds every MR (ka-0000004917).
// That script mirrors these two constants; scripts/check-advice-snapshot-freshness_test.py's
// DefaultsMirrorGoSourceTest parses this very declaration so the two clocks
// cannot silently diverge — keep the `N * 24 * time.Hour` literal form.
const (
	softSnapshotAge = 7 * 24 * time.Hour
	hardSnapshotAge = 14 * 24 * time.Hour
)

// snapshotMeta is the sidecar fixture recorded alongside the catalog snapshot.
// captured_at is the regeneration wall-clock (UTC, RFC3339); the counts pin the
// fixture shape so a hand-edit that adds/removes beads without a full refresh
// cannot leave a stale-but-passing timestamp.
type snapshotMeta struct {
	CapturedAt          string `json:"captured_at"`
	AdviceBeadCount     int    `json:"advice_bead_count"`
	FleetArchetypeCount int    `json:"fleet_archetype_count"`
}

// loadSnapshotMeta reads and parses the sidecar meta fixture.
func loadSnapshotMeta(t *testing.T) snapshotMeta {
	t.Helper()
	raw, err := os.ReadFile(adviceSnapshotMetaPath)
	if err != nil {
		t.Fatalf("read snapshot meta %s: %v (regenerate with -update-advice-snapshot)", adviceSnapshotMetaPath, err)
	}
	var meta snapshotMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("parse snapshot meta %s: %v", adviceSnapshotMetaPath, err)
	}
	return meta
}

// fleetArchetypesSnapshotPath is the frozen real-fleet archetype grid: the
// distinct (project, role) pairs the fleet actually ran, derived from agent
// beads (advice.ArchetypesFromAgentBeads), NOT the advice-corpus cross-product
// that manufactured phantom archetypes (ka-0000002153). The budget test audits
// the frozen advice snapshot against THIS set, so CI enforces the token ceilings
// on real archetypes only. Refresh it alongside the advice snapshot with
// -update-advice-snapshot (needs BEADS_HTTP_ADDR + BEADS_AUTH_TOKEN).
const fleetArchetypesSnapshotPath = "testdata/fleet_archetypes_snapshot.json"

// Per-archetype matched-token budgets (ka-0000003733 Part A / ka-0000006677).
//
// The matched pool — advice.AuditCatalog's per-archetype description bytes ÷
// TokenBytesRatio — is the number the context-crisis cut budgeted against:
// ≤18k tokens/archetype soft target, 20k hard ceiling. A corpus re-leak that
// pushes any archetype past 20k is a hard CI failure, and one past 18k is a soft
// warning to audit before it becomes one. As of the 2026-11-01 refresh
// (gd-FQqvaIw8SH) the worst archetype, gardenio/cicd, sits at 5300 tokens.
const (
	realCatalogSoftTokenBudget = 18000
	realCatalogHardTokenBudget = 20000
)

// realCatalogArchetypeDebt is the per-archetype RATCHET: a register of the
// archetypes still over realCatalogHardTokenBudget, pinned at their MEASURED
// token count. It opened at 32 entries (2026-08-14 refresh, gd-RrifqICPIn),
// peaked at 34 (2026-09-01 refresh, gd-UDJOY7WZxK) and was emptied at the
// 2026-11-01 refresh (gd-FQqvaIw8SH): the live corpus had shrunk from 365 to 26
// advice beads and the fleet grid from 54 to 16 archetypes, so every remaining
// debtor was either back under 20k or no longer ran.
//
// Why a debt register instead of raising realCatalogHardTokenBudget (gd-RrifqICPIn):
// the 20k ceiling is the number the context-crisis cut budgeted against. Raising
// it to accommodate the worst offender would ratify the bust fleet-wide and
// silently re-arm the exact failure ka-0000006923 was written to end — a guard
// whose green is a lie. Instead a debtor is capped at what it measures, so:
//
//   - growth past its mark is still a HARD failure, per archetype;
//   - the 20k ceiling stays intact for every archetype not listed here;
//   - paying debt down is a one-line edit, and the register is a live worklist —
//     when an entry drops under 20k, or its archetype leaves the fleet grid, the
//     test tells you to delete the line, so the register cannot rot into a
//     permanently-higher ceiling.
//
// Do not add entries to unblock a corpus addition: a NEW archetype busting 20k
// between refreshes means the bead should be role-targeted or trimmed, not pinned.
var realCatalogArchetypeDebt = map[string]int{}

// Universal-global pool budgets (gd-U3Zr5ryUHf). The `global`-labelled pool —
// the beads matched by an archetype that subscribes to nothing but "global"
// (empty project AND empty role; ArchetypeSubscriptions("","") == ["global"])
// — reaches literally every agent of every project and role. That makes it the
// single highest-leverage delivery cost in the corpus: a re-leak into a global
// bead is paid by all ~60 fleet archetypes at once, ~60× the cost of the same
// bytes on a role-scoped bead. The per-archetype budget above catches the SUM
// but never attributes a fleet-wide inflation to its shared root; the broadcast
// guard (TestRealCatalogBroadcastCount) caps per-project broadcast COUNT but
// not the global pool's token WEIGHT. This is the missing member of that guard
// family.
//
// As of the 2026-11-01 refresh (gd-FQqvaIw8SH) the universal-global pool sits at
// ~1.1k tokens (3 beads), so the 7k soft target has ample headroom; the 9k hard
// ceiling is a deliberate re-leak backstop. These are well under the 18k/20k
// per-archetype ceilings because the global pool is one COMPONENT of every
// archetype's total, not the whole budget — it must leave room for the project +
// role pools stacked on top of it. Motivating drift: the live universal-global
// pool doubled from ~5.3k to ~12.2k tokens in 9 days unguarded, driving the whole
// fleet past the 20k per-archetype hard ceiling (gb advice audit, 2026-08-09).
// The pool was over the ceiling, ratcheted as realCatalogGlobalPoolDebt, from
// 2026-08-14 to 2026-08-17 and again from 2026-09-01 until this refresh.
const (
	realCatalogGlobalPoolSoftTokenBudget = 7000
	realCatalogGlobalPoolHardTokenBudget = 9000
)

// snapshotBead is the lean, deterministic, timestamp-free projection of an
// advice bead that the audit pipeline actually reads: identity plus the
// labels/fields that drive matching, supersession, and fixture/hippo/broadcast
// drops. Volatile columns (created_at,
// updated_at, operator attribution, category) are intentionally omitted so the
// fixture diffs stay minimal and stable across refreshes.
type snapshotBead struct {
	ID          string            `json:"id"`
	Title       string            `json:"title"`
	Description string            `json:"description"`
	Type        string            `json:"type"`
	Kind        string            `json:"kind"`
	Status      string            `json:"status"`
	Labels      []string          `json:"labels,omitempty"`
	Fields      map[string]string `json:"fields,omitempty"`
}

// toBeadDetail rehydrates a snapshot row into the beadsapi.BeadDetail shape the
// advice package operates on.
func (s snapshotBead) toBeadDetail() *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID:          s.ID,
		Title:       s.Title,
		Description: s.Description,
		Type:        s.Type,
		Kind:        s.Kind,
		Status:      s.Status,
		Labels:      s.Labels,
		Fields:      s.Fields,
	}
}

// loadAdviceSnapshot reads and rehydrates the committed catalog snapshot.
func loadAdviceSnapshot(t *testing.T) []*beadsapi.BeadDetail {
	t.Helper()
	raw, err := os.ReadFile(adviceSnapshotPath)
	if err != nil {
		t.Fatalf("read advice snapshot %s: %v (regenerate with -update-advice-snapshot)", adviceSnapshotPath, err)
	}
	var rows []snapshotBead
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("parse advice snapshot %s: %v", adviceSnapshotPath, err)
	}
	if len(rows) == 0 {
		t.Fatalf("advice snapshot %s is empty — regenerate with -update-advice-snapshot", adviceSnapshotPath)
	}
	beads := make([]*beadsapi.BeadDetail, len(rows))
	for i, r := range rows {
		beads[i] = r.toBeadDetail()
	}
	return beads
}

// loadFleetArchetypeSnapshot reads the committed real-fleet archetype grid — the
// (project, role) pairs the fleet actually ran (ka-0000002153). This is the set
// the budget test audits against, replacing the phantom advice-corpus
// cross-product advice.DeriveArchetypes used to produce.
func loadFleetArchetypeSnapshot(t *testing.T) []advice.Archetype {
	t.Helper()
	raw, err := os.ReadFile(fleetArchetypesSnapshotPath)
	if err != nil {
		t.Fatalf("read fleet archetype snapshot %s: %v (regenerate with -update-advice-snapshot)", fleetArchetypesSnapshotPath, err)
	}
	var archetypes []advice.Archetype
	if err := json.Unmarshal(raw, &archetypes); err != nil {
		t.Fatalf("parse fleet archetype snapshot %s: %v", fleetArchetypesSnapshotPath, err)
	}
	if len(archetypes) == 0 {
		t.Fatalf("fleet archetype snapshot %s is empty — regenerate with -update-advice-snapshot", fleetArchetypesSnapshotPath)
	}
	return archetypes
}

// regenerateAdviceSnapshot fetches the live open-advice corpus through the same
// ListOpenAdvice path gb prime uses, projects it to the lean snapshot shape,
// sorts by ID for a stable diff, and writes testdata/advice_catalog_snapshot.json.
// It also refreshes the fleet-archetype snapshot from the same daemon so the two
// fixtures — the corpus and the population it is audited against — always move
// together.
func regenerateAdviceSnapshot(t *testing.T) {
	t.Helper()
	addr := os.Getenv("BEADS_HTTP_ADDR")
	if addr == "" {
		t.Fatal("-update-advice-snapshot: BEADS_HTTP_ADDR is unset")
	}
	token := os.Getenv("BEADS_AUTH_TOKEN")
	if token == "" {
		token = os.Getenv("BEADS_DAEMON_TOKEN")
	}
	client, err := beadsapi.New(beadsapi.Config{HTTPAddr: addr, Token: token})
	if err != nil {
		t.Fatalf("-update-advice-snapshot: new beadsapi client: %v", err)
	}
	archetypeCount := regenerateFleetArchetypeSnapshot(t, client)
	beads, err := advice.ListOpenAdvice(context.Background(), client)
	if err != nil {
		t.Fatalf("-update-advice-snapshot: list open advice: %v", err)
	}
	rows := make([]snapshotBead, 0, len(beads))
	for _, b := range beads {
		rows = append(rows, snapshotBead{
			ID:          b.ID,
			Title:       b.Title,
			Description: b.Description,
			Type:        b.Type,
			Kind:        b.Kind,
			Status:      b.Status,
			Labels:      b.Labels,
			Fields:      snapshotFields(b.Fields),
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })

	out, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		t.Fatalf("-update-advice-snapshot: marshal: %v", err)
	}
	out = append(out, '\n')
	if err := os.MkdirAll(filepath.Dir(adviceSnapshotPath), 0o755); err != nil {
		t.Fatalf("-update-advice-snapshot: mkdir: %v", err)
	}
	if err := os.WriteFile(adviceSnapshotPath, out, 0o644); err != nil {
		t.Fatalf("-update-advice-snapshot: write %s: %v", adviceSnapshotPath, err)
	}
	t.Logf("wrote %s (%d advice beads)", adviceSnapshotPath, len(rows))

	// Stamp the freshness sidecar last, under the same wall-clock, so the
	// staleness clock (TestRealCatalogSnapshotFreshness) resets to now and the
	// recorded counts match the fixtures just written.
	writeSnapshotMeta(t, len(rows), archetypeCount)
}

// writeSnapshotMeta records the regeneration wall-clock (UTC, RFC3339, second
// precision) and the fixture counts to testdata/advice_catalog_snapshot_meta.json.
// It is written on every -update-advice-snapshot so the freshness assertion has
// a truthful "last synced with live" timestamp to measure drift against.
func writeSnapshotMeta(t *testing.T, adviceBeadCount, fleetArchetypeCount int) {
	t.Helper()
	meta := snapshotMeta{
		CapturedAt:          time.Now().UTC().Format(time.RFC3339),
		AdviceBeadCount:     adviceBeadCount,
		FleetArchetypeCount: fleetArchetypeCount,
	}
	out, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		t.Fatalf("-update-advice-snapshot: marshal meta: %v", err)
	}
	out = append(out, '\n')
	if err := os.WriteFile(adviceSnapshotMetaPath, out, 0o644); err != nil {
		t.Fatalf("-update-advice-snapshot: write %s: %v", adviceSnapshotMetaPath, err)
	}
	t.Logf("wrote %s (captured_at=%s, %d advice beads, %d archetypes)",
		adviceSnapshotMetaPath, meta.CapturedAt, adviceBeadCount, fleetArchetypeCount)
}

// regenerateFleetArchetypeSnapshot derives the real-fleet archetype grid from the
// live daemon — the agent beads spawned within advice.FleetArchetypeWindow,
// reduced to distinct (project, role) pairs by advice.ArchetypesFromAgentBeads —
// and writes it to testdata/fleet_archetypes_snapshot.json. The output is already
// sorted (ArchetypesFromAgentBeads sorts by project then role) so diffs stay
// minimal across refreshes. It returns the archetype count so the caller can
// record it in the freshness meta sidecar.
func regenerateFleetArchetypeSnapshot(t *testing.T, client *beadsapi.Client) int {
	t.Helper()
	archetypes, err := advice.ListFleetArchetypes(context.Background(), client, time.Now())
	if err != nil {
		t.Fatalf("-update-advice-snapshot: list fleet archetypes: %v", err)
	}
	if len(archetypes) == 0 {
		t.Fatal("-update-advice-snapshot: fleet yielded zero archetypes — expected the live agent-bead grid")
	}
	out, err := json.MarshalIndent(archetypes, "", "  ")
	if err != nil {
		t.Fatalf("-update-advice-snapshot: marshal fleet archetypes: %v", err)
	}
	out = append(out, '\n')
	if err := os.MkdirAll(filepath.Dir(fleetArchetypesSnapshotPath), 0o755); err != nil {
		t.Fatalf("-update-advice-snapshot: mkdir: %v", err)
	}
	if err := os.WriteFile(fleetArchetypesSnapshotPath, out, 0o644); err != nil {
		t.Fatalf("-update-advice-snapshot: write %s: %v", fleetArchetypesSnapshotPath, err)
	}
	t.Logf("wrote %s (%d fleet archetypes)", fleetArchetypesSnapshotPath, len(archetypes))
	return len(archetypes)
}

// snapshotFieldKeys is the allow-list of custom fields the audit pipeline reads
// that are ALSO stable to freeze: the hippo-mirror source label. Everything
// else (operator attribution, hippo_id, raw subscriptions) is volatile churn
// that would destabilise the fixture without changing a single audit verdict,
// so it is dropped at snapshot time.
//
// expires_at is deliberately NOT captured. advice.IsExpired compares it against
// time.Now(), so retaining it would make the frozen fixture's audit verdict
// clock-dependent — a bead with a future expiry would silently drop out of the
// token/broadcast counts on the day it lapses. ListOpenAdvice already excludes
// truly-expired beads at capture time, so every bead in the snapshot is live;
// freezing them as non-expiring keeps the guard a pure function of the
// committed bytes and is conservative (it can only over-count, never falsely
// pass).
var snapshotFieldKeys = []string{"source"}

// snapshotFields projects a bead's fields down to the audit-relevant allow-list,
// returning nil when none are present so the JSON stays compact.
func snapshotFields(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	var out map[string]string
	for _, k := range snapshotFieldKeys {
		if v, ok := in[k]; ok && v != "" {
			if out == nil {
				out = make(map[string]string, len(snapshotFieldKeys))
			}
			out[k] = v
		}
	}
	return out
}

// TestRealCatalogPerArchetypeBudget audits the committed real-catalog snapshot
// through advice.AuditCatalog — the exact pipeline gb prime, the controller
// catalog gauges, and `gb advice audit` share — and fails CI if any archetype's
// matched advice pool busts the per-archetype token budget. This is the durable
// guardrail Phase 6 (gd-iusUy8ySt6) exists to install: a re-leak of the advice
// corpus, or a regression that reverts the Phase-3 broadcast deny / supersession
// collapse, re-inflates the frozen snapshot past budget and turns CI red.
func TestRealCatalogPerArchetypeBudget(t *testing.T) {
	if *updateAdviceSnapshot {
		regenerateAdviceSnapshot(t)
		return
	}
	beads := loadAdviceSnapshot(t)

	// Audit against the REAL fleet grid (frozen from agent beads), not the
	// advice-corpus cross-product that manufactured phantom archetypes and left
	// the highest-volume real pairs unmeasured (ka-0000002153).
	archetypes := loadFleetArchetypeSnapshot(t)
	results := advice.AuditCatalog(beads, archetypes)

	// Worst-first so the log leads with the archetype nearest the ceiling.
	sort.SliceStable(results, func(i, j int) bool { return results[i].Tokens > results[j].Tokens })

	worst := results[0]
	t.Logf("real-catalog audit: %d advice beads, %d archetypes; worst = %s/%s at %d tokens (soft %d / hard %d)",
		len(beads), len(results), worst.Project, worst.Role, worst.Tokens,
		realCatalogSoftTokenBudget, realCatalogHardTokenBudget)

	softOver := 0
	repaid := make([]string, 0, len(realCatalogArchetypeDebt))
	for _, r := range results {
		key := r.Project + "/" + r.Role
		if debt, owed := realCatalogArchetypeDebt[key]; owed {
			// A ratcheted debtor: it must not grow past the mark it was pinned
			// at, and once it comes back under the real ceiling its entry must
			// be deleted so the register stays a shrinking worklist.
			switch {
			case r.Tokens > debt:
				t.Errorf("DEBT ratchet bust: %s matched pool = %d tokens (%d beads / %d bytes) > its pinned debt of %d (grew by %d) — this archetype is already over the %d hard ceiling and just got WORSE; trim the corpus (gb advice audit), do not raise the pin",
					key, r.Tokens, r.Matched, r.Bytes, debt, r.Tokens-debt, realCatalogHardTokenBudget)
			case r.Tokens <= realCatalogHardTokenBudget:
				repaid = append(repaid, fmt.Sprintf("%s (%d tokens, was pinned at %d)", key, r.Tokens, debt))
			default:
				t.Logf("debt: %s = %d tokens (pinned %d, ceiling %d) — still %d over, owes %d more of trim",
					key, r.Tokens, debt, realCatalogHardTokenBudget,
					r.Tokens-realCatalogHardTokenBudget, r.Tokens-realCatalogHardTokenBudget)
			}
			continue
		}
		if r.Tokens > realCatalogHardTokenBudget {
			t.Errorf("HARD budget bust: %s/%s matched pool = %d tokens (%d beads / %d bytes) > %d hard ceiling (over by %dk) — audit the corpus (gb advice audit) or a delivery-logic regression re-admitted advice (broadcast deny / supersession)",
				r.Project, r.Role, r.Tokens, r.Matched, r.Bytes, realCatalogHardTokenBudget,
				(r.Tokens-realCatalogHardTokenBudget)/1000)
		} else if r.Tokens > realCatalogSoftTokenBudget {
			softOver++
			t.Logf("SOFT budget warning: %s/%s matched pool = %d tokens > %d soft target (still under the %d hard ceiling) — audit before it regresses further",
				r.Project, r.Role, r.Tokens, realCatalogSoftTokenBudget, realCatalogHardTokenBudget)
		}
	}
	if softOver > 0 {
		t.Logf("%d/%d archetypes over the %dk soft target (none over the %dk hard ceiling)",
			softOver, len(results), realCatalogSoftTokenBudget/1000, realCatalogHardTokenBudget/1000)
	}

	// Debt that has been repaid must be REMOVED from the register, not left
	// behind as a dormant above-ceiling allowance. Without this the map decays
	// into a permanently-raised ceiling — the very outcome the register exists
	// to avoid — so a repaid entry is a hard failure with a one-line fix.
	// A debt entry whose archetype no longer appears in the fleet grid is a
	// dormant allowance too: it would silently re-admit a bust the day that
	// archetype respawns. 28 such entries outlived their archetypes before the
	// 2026-11-01 refresh (gd-FQqvaIw8SH).
	measured := make(map[string]bool, len(results))
	for _, r := range results {
		measured[r.Project+"/"+r.Role] = true
	}
	for _, key := range sortedIntMapKeys(realCatalogArchetypeDebt) {
		if !measured[key] {
			repaid = append(repaid, fmt.Sprintf("%s (not in %s)", key, fleetArchetypesSnapshotPath))
		}
	}
	if len(repaid) > 0 {
		sort.Strings(repaid)
		t.Errorf("advice debt REPAID for %d archetype(s) — delete these entries from realCatalogArchetypeDebt so the %d ceiling applies to them again:\n    %s",
			len(repaid), realCatalogHardTokenBudget, strings.Join(repaid, "\n    "))
	}
	if len(realCatalogArchetypeDebt) > 0 {
		t.Logf("advice debt register: %d/%d archetypes pinned above the %d ceiling (worklist ka-0000003207; shared root is the universal-global pool)",
			len(realCatalogArchetypeDebt), len(results), realCatalogHardTokenBudget)
	}
}

// TestRealCatalogGlobalPoolBudget guards the universal-global advice pool — the
// beads a globally-scoped archetype (empty project AND empty role) matches,
// i.e. everything reaching every agent of every project and role — against a
// dedicated token budget (gd-U3Zr5ryUHf).
//
// Why a SEPARATE guard, when TestRealCatalogPerArchetypeBudget already pins each
// archetype's TOTAL: the global pool is the ONE component every archetype pays,
// so it is the highest-leverage place for a re-leak to hide. A 2k-token bead
// mistakenly labelled `global` instead of a `role:` scope lands on all ~60 fleet
// archetypes simultaneously and can push several past the per-archetype ceiling
// at once — but the per-archetype test reports "N archetypes over budget" and
// never fingers the shared root. This guard measures that root directly, so a
// fleet-wide inflation reddens HERE with an unambiguous cause, before it has to
// be reverse-engineered from a dozen archetype busts. It is the token-weight
// analog of TestRealCatalogBroadcastCount (which caps per-project broadcast
// COUNT) — the missing member of the delivery-scope guard family.
//
// The archetype is advice.Archetype{} (both fields empty): ArchetypeSubscriptions
// then yields exactly ["global"], so MatchesSubscriptions selects only beads
// with NO project:/role:/agent: gate — a `global, project:monorepo` bead fails
// the project gate and is correctly excluded, since it does not in fact reach
// every project. That is precisely the "reaches literally everyone" pool this
// budget defends.
func TestRealCatalogGlobalPoolBudget(t *testing.T) {
	if *updateAdviceSnapshot {
		t.Skip("snapshot regeneration handled by TestRealCatalogPerArchetypeBudget")
	}
	beads := loadAdviceSnapshot(t)

	// A single archetype that subscribes to nothing but "global".
	results := advice.AuditCatalog(beads, []advice.Archetype{{}})
	if len(results) != 1 {
		t.Fatalf("expected exactly 1 audit result for the universal-global archetype, got %d", len(results))
	}
	pool := results[0]

	t.Logf("universal-global pool: %d beads, %d bytes, %d tokens (soft %d / hard %d)",
		pool.Matched, pool.Bytes, pool.Tokens,
		realCatalogGlobalPoolSoftTokenBudget, realCatalogGlobalPoolHardTokenBudget)

	// Lead the log with the heaviest globals so a refresh that reddens this test
	// points straight at the trim (or the mislabel) candidates.
	for i, it := range pool.Items {
		if i >= 5 {
			break
		}
		t.Logf("  heaviest global #%d: %d B  %s  %q", i+1, it.Bytes, it.ID, it.Title)
	}

	switch {
	case pool.Tokens > realCatalogGlobalPoolHardTokenBudget:
		t.Errorf("HARD global-pool bust: universal-global advice = %d tokens (%d beads / %d bytes) > %d hard ceiling — every byte here is multiplied across all %d fleet archetypes; trim it (gb advice audit --project '') or retarget a mislabelled `global` bead to a role:/project: scope",
			pool.Tokens, pool.Matched, pool.Bytes, realCatalogGlobalPoolHardTokenBudget,
			len(loadFleetArchetypeSnapshot(t)))
	case pool.Tokens > realCatalogGlobalPoolSoftTokenBudget:
		t.Logf("SOFT global-pool warning: universal-global advice = %d tokens > %d soft target (still under the %d hard ceiling) — audit before it regresses further; every global byte is multiplied across the whole fleet",
			pool.Tokens, realCatalogGlobalPoolSoftTokenBudget, realCatalogGlobalPoolHardTokenBudget)
	}
}

// TestRealCatalogSnapshotFreshness closes the drift hole in the per-archetype
// budget guard (ka-0000006923). The budget test audits FROZEN bytes and is
// structurally blind to live corpus growth between refreshes, so on its own it
// can report affirmative green while the real corpus has already blown past the
// 20k hard ceiling it exists to defend — which is exactly what happened (fixture
// gardenio/gardener = 19931 tok PASS while `gb advice audit` measured 20839 tok
// live). A budget guard that can drift arbitrarily far from live is worse than
// no guard because green is a lie.
//
// The fix is a staleness clock, not a live query: CI still has no beads daemon,
// so this stays hermetic. It asserts two things:
//
//  1. The sidecar's recorded capture time is within hardSnapshotAge. Past that
//     the fixture is too old to trust and CI fails HARD, forcing a
//     `-update-advice-snapshot` refresh that re-syncs the budget verdict with
//     reality. (Between soft and hard age it only logs — a nudge, not a block.)
//  2. The recorded counts match the committed fixtures, so a hand-edit that
//     mutates the corpus without a full refresh cannot leave a stale-but-passing
//     timestamp behind.
func TestRealCatalogSnapshotFreshness(t *testing.T) {
	if *updateAdviceSnapshot {
		t.Skip("snapshot + meta regeneration handled by TestRealCatalogPerArchetypeBudget")
	}
	meta := loadSnapshotMeta(t)

	// The recorded counts must match what is actually committed, so the
	// timestamp can only be trusted alongside the fixture it was captured with.
	// A hand-edit to the snapshot that skips -update-advice-snapshot trips here.
	beads := loadAdviceSnapshot(t)
	if meta.AdviceBeadCount != len(beads) {
		t.Errorf("snapshot meta advice_bead_count=%d but %s holds %d beads — the fixture was edited without a full -update-advice-snapshot; regenerate so the freshness stamp matches the corpus",
			meta.AdviceBeadCount, adviceSnapshotPath, len(beads))
	}
	archetypes := loadFleetArchetypeSnapshot(t)
	if meta.FleetArchetypeCount != len(archetypes) {
		t.Errorf("snapshot meta fleet_archetype_count=%d but %s holds %d archetypes — regenerate with -update-advice-snapshot",
			meta.FleetArchetypeCount, fleetArchetypesSnapshotPath, len(archetypes))
	}

	captured, err := time.Parse(time.RFC3339, meta.CapturedAt)
	if err != nil {
		t.Fatalf("snapshot meta captured_at=%q is not RFC3339: %v — regenerate with -update-advice-snapshot", meta.CapturedAt, err)
	}
	age := time.Since(captured)
	t.Logf("advice snapshot captured %s ago (captured_at=%s; soft %s / hard %s)",
		age.Round(time.Hour), meta.CapturedAt, softSnapshotAge, hardSnapshotAge)

	switch {
	case age > hardSnapshotAge:
		// A HARD failure, not a soft log: a warning buried in a PASS is the very
		// failure mode this test exists to end. Refreshing may reveal a real
		// budget breach that grew while the fixture was frozen — that breach is
		// PRE-EXISTING corpus drift (audit it with `gb advice audit`, owned by
		// the advice-cut work ka-0000004903), NOT introduced by whoever runs the
		// refresh. Trim the corpus or bump the ceiling deliberately; do not
		// re-freeze a stale fixture to dodge the red.
		t.Errorf("advice catalog snapshot is STALE: captured %s ago (> %s hard ceiling). The budget guard cannot see live corpus growth past its capture, so its green is no longer trustworthy. Refresh:\n    cd gardenio/controller && go test ./cmd/gb -run TestRealCatalog -update-advice-snapshot\n(needs BEADS_HTTP_ADDR + BEADS_AUTH_TOKEN), then commit the regenerated testdata/. A refresh that reddens the budget test is revealing pre-existing drift (ka-0000006923), not a regression you introduced.",
			age.Round(time.Hour), hardSnapshotAge)
	case age > softSnapshotAge:
		t.Logf("SOFT freshness warning: advice catalog snapshot captured %s ago (> %s soft target, still under the %s hard ceiling) — refresh with -update-advice-snapshot before it turns CI red",
			age.Round(time.Hour), softSnapshotAge, hardSnapshotAge)
	}
}

// realCatalogBroadcastCaps pins the maximum number of DELIVERED (opted-in via
// scope:project-broadcast) bare-project broadcast advice beads allowed per
// project. Broadcasts reach every agent of the project regardless of role, so
// their count is the Phase-3 metric that must not silently regress
// (gd-AlLIyh99kQ). Caps are set at the committed-snapshot counts + 0 headroom:
// adding a broadcast to a capped project, or introducing one for an uncapped
// project, fails CI and forces a deliberate cap bump here (with a look at
// whether the bead should have been role-targeted instead).
//
// Keep in step with `gb advice lint` (opted-in-broadcast findings) and the
// gardenio_advice_broadcast_beads{status="delivered"} gauge.
var realCatalogBroadcastCaps = map[string]int{
	// gardenio: 5 -> 3 at the 2026-08-17 Phase-2 refresh (gd-Oemaz5Bw7w), 3 -> 5
	// at the 2026-09-01 refresh (gd-UDJOY7WZxK), then 5 -> 4 at the 2026-11-01
	// refresh (gd-FQqvaIw8SH). The monorepo, cs, log-analyst and
	// aws-cost-management caps were dropped at that refresh: none of those
	// projects has advice in the live corpus any more. Audit new broadcasts for
	// role: targeting with `gb advice lint` (opted-in-broadcast) before bumping.
	"gardenio": 4,
}

// TestRealCatalogBroadcastCount guards the per-project broadcast-count metric
// (gd-AlLIyh99kQ, Phase-3 guardrail). It counts DELIVERED bare-project
// broadcasts (opted-in via scope:project-broadcast) per project in the frozen
// snapshot, applying the same per-bead delivery drops the audit does, and fails
// if any project exceeds its cap or if a broadcast appears for a project with
// no declared cap. Denied broadcasts (no opt-in) are reported but not budgeted
// — the delivery path drops them and `gb advice lint` flags them as errors.
func TestRealCatalogBroadcastCount(t *testing.T) {
	if *updateAdviceSnapshot {
		t.Skip("snapshot regeneration handled by TestRealCatalogPerArchetypeBudget")
	}
	beads := loadAdviceSnapshot(t)

	delivered := map[string]int{}
	denied := map[string]int{}
	for _, b := range beads {
		// Same unconditional per-bead drops the audit applies before matching.
		if b == nil || b.Status == "closed" || advice.IsExpired(b) || advice.IsFixture(b) || advice.IsHippoMirror(b) {
			continue
		}
		if !advice.IsProjectBroadcast(b.Labels) {
			continue
		}
		target := denied
		if advice.HasBroadcastOptIn(b.Labels) {
			target = delivered
		}
		for _, p := range advice.BroadcastProjects(b.Labels) {
			target[p]++
		}
	}

	for _, p := range sortedIntMapKeys(delivered) {
		t.Logf("delivered broadcasts: %s = %d (cap %d)", p, delivered[p], realCatalogBroadcastCaps[p])
	}
	for _, p := range sortedIntMapKeys(denied) {
		// Denied broadcasts reach nobody; surface them so the count isn't a
		// mystery, but they don't consume a project's delivered cap.
		t.Logf("denied broadcasts (no opt-in, dropped by delivery): %s = %d", p, denied[p])
	}

	for _, p := range sortedIntMapKeys(delivered) {
		limit, ok := realCatalogBroadcastCaps[p]
		if !ok {
			t.Errorf("project %q has %d delivered broadcast advice bead(s) but no declared cap — every %s agent pays for these; add a role: target or, if intentional, add %q to realCatalogBroadcastCaps",
				p, delivered[p], p, p)
			continue
		}
		if delivered[p] > limit {
			t.Errorf("project %q delivered broadcast count %d > cap %d — a re-leak of project-wide advice; audit with `gb advice lint` (opted-in-broadcast) and prefer role: targeting, or bump the cap deliberately",
				p, delivered[p], limit)
		}
	}
}

// sortedIntMapKeys returns the keys of a string-keyed int map in sorted order
// for deterministic logging/iteration.
func sortedIntMapKeys(m map[string]int) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
