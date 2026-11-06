package main

// session_spend_proxy_e2e_test.go — end-to-end proof that the session-spend
// metering pipeline is BYTE-IDENTICAL whether or not claudemore fronts the
// account via proxy_url. Backfills the coverage gap in task ka-0000005743
// (parent ka-0000000958; guarantee ka-0000007533, transparency-A1).
//
// ── The gap this file closes ──────────────────────────────────────────────────
//   resolveSpendReadBaseURL (podspec.go) deliberately OMITS the proxyURL rung so
//   the controller reads spend from litellm DIRECTLY, bypassing claudemore —
//   commit 251671e45, the load-bearing half of the claudemore transparency
//   guarantee (A1). Before this file, NO e2e anywhere configured an account with
//   a proxy at all: `git grep proxy_url -- tests/e2e/ cmd/*probe*` was EMPTY.
//   The three existing spend modules (litellm-cost-tracking, session-spend-
//   attribution, costs-spend-page) all assert spend end-to-end and are green, but
//   NONE attaches a proxy to an account, so every one exercises only the no-proxy
//   path. A regression that routes the spend READ through claudemore leaves all
//   three GREEN while total_cost_usd / gardenio_agent_spend_usd_total silently zero
//   out for every proxied account — the "born vacuous" shape (advice ka-0000001521):
//   the fixture never has the property (a proxy) that breaks production.
//
// ── What this file drives ─────────────────────────────────────────────────────
//   It runs the SAME spawn→close→served-/metrics flow as session_spend_e2e_test.go
//   TWICE against the REAL sessionSpendReconciler over real HTTP surfaces:
//
//     • CONTROL leg — an account with NO proxy_url. base_url points at the litellm
//       test server (the one that serves /spend/tags).
//     • PROXIED leg — the SAME account config PLUS proxy_url pointing at a
//       claudemore TRAP server that counts every hit and fails it. base_url is the
//       same shape of litellm test server.
//
//   The base URL each leg's reconciler queries is resolved by the REAL production
//   function resolveSpendReadBaseURLForAccount(cfg, project, role) — the exact
//   code spendReconciler.resolveBaseURL calls in main.go — reading a real
//   config.Config AccountCache seeded with base_url + proxy_url. So a regression
//   that made that function return acct.ProxyURL (routing the read through
//   claudemore) is what this test catches: the proxied leg's reconciler would
//   query the claudemore trap, the trap hit-count would go non-zero, and the
//   settled fields would diverge from the control leg.
//
//   The assertion is the COMPARISON, not "spend is non-zero": every settled field
//   the proxied leg writes (total_cost_usd, litellm_spend_close,
//   litellm_spend_first_baseline, the spawn baseline) and its served counter value
//   must equal the control leg's byte-for-byte, AND the claudemore trap must be
//   contacted ZERO times across the whole proxied flow.
//
// ── Hermetic HARD gate ────────────────────────────────────────────────────────
//   No pods, no live cluster — httptest servers + an in-process config. Green
//   today; any divergence between the legs, or any hit on the claudemore trap,
//   reddens it.
//
// The rwDaemon / servedCounter / newSpendMetricsReg / parseCounterLine helpers are
// shared with session_spend_e2e_test.go (same package); this file adds only the
// proxy-specific wiring: a mutable /spend/tags server (the tag total grows across
// the session's life), the claudemore trap, and a reconciler wired to the real
// production base-URL resolver.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/config"
)

// mutableSpendTags is a litellm /spend/tags stand-in whose served entries can be
// swapped between spawn and close, modeling the cumulative tag total GROWING over
// a session's life (spawn total < close total) on ONE stable base_url — which is
// how litellm actually behaves in production, and lets the config's base_url be a
// single fixed URL. It requires the master key on Authorization, matching the live
// proxy contract the reconciler depends on.
type mutableSpendTags struct {
	mu      sync.Mutex
	entries []tagSpendEntry
}

func (m *mutableSpendTags) set(entries []tagSpendEntry) {
	m.mu.Lock()
	m.entries = entries
	m.mu.Unlock()
}

func (m *mutableSpendTags) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/spend/tags") {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer e2e-master-key" {
			http.Error(w, "missing master key", http.StatusForbidden)
			return
		}
		m.mu.Lock()
		entries := m.entries
		m.mu.Unlock()
		_ = json.NewEncoder(w).Encode(entries)
	})
}

// claudemoreTrap is an httptest handler standing in for claudemore: it counts
// every request and answers 502. claudemore is a Messages-API proxy and does NOT
// serve litellm's /spend/tags, so a spend read that ever reaches it fails — the
// exact production break the read-path proxyURL omission prevents. hits() reads
// the counter; any non-zero value means the spend read routed through claudemore.
type claudemoreTrap struct{ n atomic.Int64 }

func (c *claudemoreTrap) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.n.Add(1)
		http.Error(w, "claudemore does not serve /spend/tags", http.StatusBadGateway)
	})
}

func (c *claudemoreTrap) hits() int64 { return c.n.Load() }

// proxyLegResult captures the observable output of one spawn→close flow so the
// two legs can be compared field-by-field.
type proxyLegResult struct {
	baseline      string // litellm_spend_baseline persisted at spawn
	totalCostUSD  string // total_cost_usd written at close (the agent rollup)
	spendClose    string // litellm_spend_close settlement watermark
	firstBaseline string // litellm_spend_first_baseline (write-once lifetime anchor)
	counter       float64
	counterFound  bool
	trapHits      int64 // claudemore trap contacts during this leg (want 0)
}

// spendCfgForAccount builds a minimal real config.Config whose AccountCache maps
// the given account name to an entry with the supplied base_url and proxy_url,
// and whose ProjectCache routes the "gardenio" project (used by the test) to that
// account. This is the SAME structure the controller populates from type=account
// config beads, so resolveSpendReadBaseURLForAccount reads it exactly as in
// production. (Routing via ProjectCache rather than the removed cfg.DefaultAccount
// terminal fallback — account-routing.md §7 Phase 3, ka-0000007959.)
func spendCfgForAccount(account, baseURL, proxyURL string) *config.Config {
	return &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{
			"gardenio": {Account: account},
		},
		AccountCache: map[string]config.AccountCacheEntry{
			account: {
				Provider: "anthropic",
				BaseURL:  baseURL,
				ProxyURL: proxyURL,
				Model:    "claude-opus-4-8",
				Status:   "active",
			},
		},
	}
}

// TestSessionSpendProxyTransparency_ByteIdenticalToUnproxied is the core backfill.
// It runs the identical spawn→close flow twice — once with NO proxy, once with a
// claudemore proxy_url attached to the SAME account — and asserts:
//
//  1. Every settled field the controller writes (the spawn baseline,
//     total_cost_usd, litellm_spend_close, litellm_spend_first_baseline) is
//     byte-for-byte identical between the two legs.
//  2. The served gardenio_agent_spend_usd_total counter reads the identical value.
//  3. The claudemore trap is contacted ZERO times in the proxied leg — the spend
//     read never routes through claudemore.
//
// The comparison IS the assertion (a bare "spend is non-zero" would be born
// vacuous). Point 3 is what a "route spend through claudemore" regression trips.
func TestSessionSpendProxyTransparency_ByteIdenticalToUnproxied(t *testing.T) {
	const (
		project = "gardenio"
		role    = "crew"
		account = "litellm-prod"
	)
	// One shared spend trajectory replayed by both legs: tag reads 4.25 at spawn,
	// 7.75 at close -> settled lifetime 3.50. Both legs replay it, so any
	// divergence is caused by the proxy and nothing else.
	const spawnTotal, closeTotal = 4.25, 7.75

	// Each leg owns a litellm /spend/tags server; the account's base_url points at
	// it, so the production read-path resolver routes the reconciler's reads there.
	// The proxied leg additionally attaches a claudemore trap as proxy_url, which
	// the spend read must NEVER touch.
	controlLitellm := &mutableSpendTags{}
	controlSrv := httptest.NewServer(controlLitellm.handler())
	t.Cleanup(controlSrv.Close)

	proxiedLitellm := &mutableSpendTags{}
	proxiedSrv := httptest.NewServer(proxiedLitellm.handler())
	t.Cleanup(proxiedSrv.Close)

	trap := &claudemoreTrap{}
	trapSrv := httptest.NewServer(trap.handler())
	t.Cleanup(trapSrv.Close)

	control := runLegWithServer(t, controlLitellm, spawnTotal, closeTotal,
		"gardenio-spendcrew-ctl", "crew-gardenio-crew-spendcrew-ctl",
		project, role, account, controlSrv.URL, "")

	proxied := runLegWithServer(t, proxiedLitellm, spawnTotal, closeTotal,
		"gardenio-spendcrew-pxy", "crew-gardenio-crew-spendcrew-pxy",
		project, role, account, proxiedSrv.URL, trapSrv.URL)
	proxied.trapHits = trap.hits()

	// ── (1)+(2): byte-identical settlement + counter. ──
	if control.baseline != proxied.baseline {
		t.Errorf("baseline diverged: control=%q proxied=%q — metering must be identical with/without proxy", control.baseline, proxied.baseline)
	}
	if control.totalCostUSD != proxied.totalCostUSD {
		t.Errorf("total_cost_usd diverged: control=%q proxied=%q — a proxy must NOT change the settled cost (A1)", control.totalCostUSD, proxied.totalCostUSD)
	}
	if control.spendClose != proxied.spendClose {
		t.Errorf("litellm_spend_close diverged: control=%q proxied=%q", control.spendClose, proxied.spendClose)
	}
	if control.firstBaseline != proxied.firstBaseline {
		t.Errorf("litellm_spend_first_baseline diverged: control=%q proxied=%q", control.firstBaseline, proxied.firstBaseline)
	}
	if control.counterFound != proxied.counterFound || control.counter != proxied.counter {
		t.Errorf("served counter diverged: control=(%v,%v) proxied=(%v,%v) — gardenio_agent_spend_usd_total must be identical",
			control.counter, control.counterFound, proxied.counter, proxied.counterFound)
	}

	// Sanity floor: the shared trajectory settles to 3.50, so both legs must have
	// booked a real, non-zero cost — otherwise "identical" could hold vacuously at
	// zero (both legs silently broken the same way).
	if control.totalCostUSD != "3.5" {
		t.Fatalf("control total_cost_usd = %q, want \"3.5\" — the flow must book a real cost, else the identity is vacuous", control.totalCostUSD)
	}
	if !proxied.counterFound || proxied.counter != 3.50 {
		t.Fatalf("proxied served counter = (%v, found=%v), want (3.50, true)", proxied.counter, proxied.counterFound)
	}

	// ── (3): the claudemore trap was NEVER contacted. This is the regression fence:
	// the spend read resolved litellm DIRECTLY (base_url), never the proxy_url. ──
	if proxied.trapHits != 0 {
		t.Fatalf("claudemore trap contacted %d time(s) during the proxied spend flow — the spend READ must bypass claudemore and read litellm directly (transparency A1, ka-0000007533). A non-zero count means resolveSpendReadBaseURLForAccount routed the read through proxy_url.", proxied.trapHits)
	}
}

// runLegWithServer drives one spawn→close flow against a caller-owned litellm
// /spend/tags server (so the caller can read the claudemore trap's hit count from
// the same trap it passed in). It swaps the server's entries from spawnTotal to
// closeTotal between the two phases.
//
// The reconciler's resolveBaseURL is the REAL production resolver over a config
// seeded with (baseURL, proxyURL); the global litellmURL fallback is a dead
// address so a "" resolver result can never masquerade as success.
func runLegWithServer(t *testing.T, litellm *mutableSpendTags, spawnTotal, closeTotal float64, agent, beadID, project, role, account, baseURL, proxyURL string) proxyLegResult {
	t.Helper()

	cfg := spendCfgForAccount(account, baseURL, proxyURL)

	daemon := newRWDaemon()
	daemon.seedAgent(beadID, map[string]any{
		"agent": agent, "project": project, "role": role, "agent_state": "spawning",
	})
	daemonSrv := httptest.NewServer(daemon.handler())
	t.Cleanup(daemonSrv.Close)

	client, err := beadsapi.New(beadsapi.Config{HTTPAddr: daemonSrv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}

	sm, reg := newSpendMetricsReg(t)
	labels := map[string]string{
		"agent": agent, "project": project, "role": role,
		"model": "claude-opus-4-8", "account": account,
	}

	r := newSessionSpendReconciler(client, "http://litellm-global-fallback.invalid", "e2e-master-key", quietLogger())
	r.metrics = sm
	r.resolveLabels = func(_, _ string) (string, string) { return "claude-opus-4-8", account }
	r.resolveBaseURL = func(p, ro string) string {
		return resolveSpendReadBaseURLForAccount(cfg, p, ro)
	}

	tagID := agentTag(agent)

	// Spawn: cumulative tag total = spawnTotal.
	litellm.set([]tagSpendEntry{{IndividualRequestTag: tagID, LogCount: 8, TotalSpend: spawnTotal}})
	r.captureBaseline(context.Background(), beadID, agent, project, role)

	// Close: cumulative tag total has advanced to closeTotal.
	litellm.set([]tagSpendEntry{{IndividualRequestTag: tagID, LogCount: 20, TotalSpend: closeTotal}})
	bead, err := r.daemon.GetBead(context.Background(), beadID)
	if err != nil {
		t.Fatalf("GetBead at close: %v", err)
	}
	r.reconcileAtClose(context.Background(), bead, agent)

	value, found := servedCounter(t, reg, labels)
	return proxyLegResult{
		baseline:      daemon.fieldString(beadID, spendBaselineField),
		totalCostUSD:  daemon.fieldString(beadID, "total_cost_usd"),
		spendClose:    daemon.fieldString(beadID, spendCloseField),
		firstBaseline: daemon.fieldString(beadID, spendFirstBaselineField),
		counter:       value,
		counterFound:  found,
	}
}

// TestSpendReadBaseURLForAccount_IgnoresProxyURL is the unit-level crux the e2e
// above rests on: for an account carrying BOTH base_url and proxy_url, the
// production read-path resolver returns base_url (litellm) and NEVER proxy_url
// (claudemore). It also confirms the write path (resolveAccountBaseURL) DOES
// return proxy_url for the same account, so read and write DIVERGE exactly on
// proxy_url — the divergence that keeps metering litellm-direct while claudemore
// fronts the write path.
//
// This is the fast, deterministic sensor a "unify the resolvers onto proxy_url"
// cleanup would trip long before the heavier e2e above.
func TestSpendReadBaseURLForAccount_IgnoresProxyURL(t *testing.T) {
	const (
		account  = "litellm-prod"
		baseURL  = "http://gardenio-litellm:4000"    // litellm — metering source of truth
		proxyURL = "http://gardenio-claudemore:8100" // claudemore — fronts the write path
	)
	cfg := spendCfgForAccount(account, baseURL, proxyURL)

	// READ path (spend): litellm-direct, proxy_url ignored.
	read := resolveSpendReadBaseURLForAccount(cfg, "gardenio", "crew")
	if read != baseURL {
		t.Fatalf("resolveSpendReadBaseURLForAccount = %q, want litellm base_url %q — spend read must bypass claudemore", read, baseURL)
	}
	if read == proxyURL {
		t.Fatalf("spend read resolved to the claudemore proxy %q — spend must never route through claudemore (A1)", proxyURL)
	}

	// WRITE path (agent traffic): honors proxy_url, so it diverges from the read.
	write := resolveAccountBaseURL(proxyURL, baseURL, "")
	if write != proxyURL {
		t.Fatalf("resolveAccountBaseURL = %q, want claudemore proxy %q — the write path must honor proxy_url", write, proxyURL)
	}
	if read == write {
		t.Fatalf("read and write both resolved to %q — they MUST diverge on proxy_url so metering stays litellm-direct (A1)", read)
	}

	// Resolved account absent from the cache -> "" (reconciler falls back to the
	// global litellmURL). Route the project at a name the AccountCache does not
	// carry so the lookup misses.
	orphanCfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{
			"gardenio": {Account: "account-not-in-cache"},
		},
		AccountCache: map[string]config.AccountCacheEntry{
			account: {Provider: "anthropic", BaseURL: baseURL, ProxyURL: proxyURL},
		},
	}
	if got := resolveSpendReadBaseURLForAccount(orphanCfg, "gardenio", "crew"); got != "" {
		t.Errorf("resolveSpendReadBaseURLForAccount for an account absent from the cache = %q, want \"\" (global fallback)", got)
	}
}
