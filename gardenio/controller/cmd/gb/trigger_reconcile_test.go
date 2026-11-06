// Tests for `gb trigger reconcile` (trigger_reconcile.go).
//
// A tiny httptest daemon serves the trigger bead (GET /v1/beads/<id>) and its
// children (GET /v1/beads?labels=trigger:<id>). The test drives the real
// runTriggerReconcile path and asserts the emitted JSON report, so the fire-
// reconstruction wiring (schedule_fire_time + last_run → ReconcileFires) is
// exercised end-to-end.
package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/scheduler"

	"github.com/spf13/cobra"
)

// reconcileBead is a minimal bead the reconcile daemon serves.
type reconcileBead struct {
	ID        string            `json:"id"`
	Type      string            `json:"type"`
	Status    string            `json:"status"`
	Title     string            `json:"title"`
	Labels    []string          `json:"labels"`
	Fields    map[string]string `json:"fields"`
	CreatedAt string            `json:"created_at"`
	ClosedAt  string            `json:"closed_at,omitempty"`
	UpdatedAt string            `json:"updated_at,omitempty"`
}

// reconcileDaemon serves one trigger by ID and a fixed child list for the
// trigger:<id> label query. `gates` are served for the operator-pause overlay's
// type=gate search queries (trigger_reconcile_pause.go): each gate whose title
// matches the query's ?search= is returned, so a test can seed pause windows.
// `agents` are served for the max_concurrent lease overlay's type=agent
// spawned_by scan (trigger_reconcile_lease.go), so a test can seed lease holders.
type reconcileDaemon struct {
	trigger  reconcileBead
	children []reconcileBead
	gates    []reconcileBead
	agents   []reconcileBead
}

func (d *reconcileDaemon) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		// Lease overlay spawned-agent scan (type=agent, ?field=spawned_by=...).
		case r.URL.Path == "/v1/beads" && r.URL.Query().Get("type") == "agent":
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": d.agents, "total": len(d.agents)})
		// Operator-pause overlay gate scan (type=gate, ?search=<title>, no labels).
		case r.URL.Path == "/v1/beads" && r.URL.Query().Get("type") == "gate":
			search := r.URL.Query().Get("search")
			var out []reconcileBead
			for _, g := range d.gates {
				if search == "" || g.Title == search {
					out = append(out, g)
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": out, "total": len(out)})
		// List query for children (has a query string with labels=). Honor
		// sort/offset/limit exactly as the real daemon does, so the fire-
		// reconstruction paging walk (ListAllBeadsFiltered) is exercised end-to-
		// end and a capped single-page query would actually truncate the OLDEST
		// children — the ka-0000007883 regression this suite guards.
		case r.URL.Path == "/v1/beads" && r.URL.Query().Get("labels") != "":
			sorted := make([]reconcileBead, len(d.children))
			copy(sorted, d.children)
			switch r.URL.Query().Get("sort") {
			case "-created_at":
				sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].CreatedAt > sorted[j].CreatedAt })
			case "created_at":
				sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].CreatedAt < sorted[j].CreatedAt })
			}
			total := len(sorted)
			if off, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && off > 0 {
				if off >= len(sorted) {
					sorted = nil
				} else {
					sorted = sorted[off:]
				}
			}
			if lim, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && lim >= 0 && lim < len(sorted) {
				sorted = sorted[:lim]
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"beads": sorted,
				"total": total,
			})
		// GET single bead.
		case strings.HasPrefix(r.URL.Path, "/v1/beads/"):
			id := strings.TrimPrefix(r.URL.Path, "/v1/beads/")
			if id == d.trigger.ID {
				_ = json.NewEncoder(w).Encode(d.trigger)
				return
			}
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	})
}

func setupReconcileDaemon(t *testing.T, d *reconcileDaemon) {
	t.Helper()
	srv := httptest.NewServer(d.handler())
	t.Cleanup(srv.Close)
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	oldDaemon := daemon
	daemon = c
	t.Cleanup(func() { daemon = oldDaemon })
}

// runReconcileJSON runs the command with --json and returns the parsed report.
// It drives the flags directly (window/tolerance are package-level, bound to
// triggerReconcileCmd) rather than re-parsing a fresh cobra command, so each
// test controls the window/tolerance explicitly and deterministically.
func runReconcileJSON(t *testing.T, id string, window, tolerance time.Duration) reconcileReport {
	t.Helper()
	oldJSON := jsonOutput
	jsonOutput = true
	oldWindow, oldTol, oldLim := reconcileWindow, reconcileTolerance, reconcileChildLim
	reconcileWindow, reconcileTolerance, reconcileChildLim = window, tolerance, 500
	t.Cleanup(func() {
		jsonOutput = oldJSON
		reconcileWindow, reconcileTolerance, reconcileChildLim = oldWindow, oldTol, oldLim
	})

	var report reconcileReport
	out := captureStdout(t, func() error {
		cmd := &cobra.Command{}
		cmd.SetContext(t.Context())
		return runTriggerReconcile(cmd, []string{id})
	})
	if err := json.Unmarshal(out, &report); err != nil {
		t.Fatalf("unmarshal report from %q: %v", string(out), err)
	}
	return report
}

func TestTriggerReconcile_HourlyAllMatched(t *testing.T) {
	// Anchor observed fires to the top of recent hours so they align with the
	// "0 * * * *" cron marks regardless of the current wall-clock minute.
	topOfHour := time.Now().UTC().Truncate(time.Hour)
	mk := func(h int) string {
		return topOfHour.Add(time.Duration(-h) * time.Hour).Format(time.RFC3339)
	}
	d := &reconcileDaemon{
		trigger: reconcileBead{
			ID: "gd-EcjHgPeFlU", Type: "trigger", Status: "open", Title: "hourly gardener",
			Fields: map[string]string{
				"cron":        "0 * * * *",
				"enabled":     "true",
				"wake_intent": "every hour on the hour",
				"last_run":    mk(1),
			},
		},
		children: []reconcileBead{
			{ID: "gd-c1", Type: "task", Fields: map[string]string{"schedule_fire_time": mk(1)}},
			{ID: "gd-c2", Type: "task", Fields: map[string]string{"schedule_fire_time": mk(2)}},
			{ID: "gd-c3", Type: "task", Fields: map[string]string{"schedule_fire_time": mk(3)}},
		},
	}
	setupReconcileDaemon(t, d)

	rep := runReconcileJSON(t, "gd-EcjHgPeFlU", 6*time.Hour, 15*time.Minute)
	if rep.WakeIntent != "every hour on the hour" {
		t.Fatalf("wake_intent not surfaced: %q", rep.WakeIntent)
	}
	if rep.Cron != "0 * * * *" {
		t.Fatalf("cron not surfaced: %q", rep.Cron)
	}
	if rep.Fires == nil {
		t.Fatal("expected a Fires reconciliation")
	}
	// The observed fires align to the hourly marks; all should match, none extra.
	if rep.Fires.MatchedCount == 0 {
		t.Fatalf("expected matched fires, got %d matched / %d missed / %d extra",
			rep.Fires.MatchedCount, rep.Fires.MissedCount, rep.Fires.ExtraCount)
	}
	if rep.Fires.ExtraCount != 0 {
		t.Fatalf("expected 0 extra fires, got %d", rep.Fires.ExtraCount)
	}
}

func TestTriggerReconcile_DarkGardenerAllMissed(t *testing.T) {
	// A gardener that is evaluated (looks alive) but never actually fired: no
	// children, no last_run. Every expected cron fire is a candidate miss —
	// exactly the "if you fail you'll never know" case the thread names.
	d := &reconcileDaemon{
		trigger: reconcileBead{
			ID: "gd-trigDARK", Type: "trigger", Status: "open", Title: "dark gardener",
			Fields: map[string]string{
				"cron":        "0 * * * *",
				"enabled":     "true",
				"wake_intent": "hourly sweep",
			},
		},
	}
	setupReconcileDaemon(t, d)

	rep := runReconcileJSON(t, "gd-trigDARK", 6*time.Hour, 15*time.Minute)
	if rep.ObservedFireCount != 0 {
		t.Fatalf("expected 0 observed fires, got %d", rep.ObservedFireCount)
	}
	if rep.Fires == nil || rep.Fires.MatchedCount != 0 || rep.Fires.MissedCount == 0 {
		t.Fatalf("expected all-missed reconciliation, got %+v", rep.Fires)
	}
}

// TestTriggerReconcile_ManyChildrenNoTruncation is the ka-0000007883 regression:
// a busy */30 trigger over 168h spawns more labeled children than any single
// capped page holds, and every child sits exactly on a cron slot. Fire
// reconstruction MUST page the whole set — the pre-fix code fetched one
// -created_at page capped at 500 and silently dropped the OLDEST children, so
// the earliest slots read as phantom in-window misses (the exact
// drift_candidate_missed=14 false positive this trigger produced live). With
// paging, observed == expected and there are zero missed fires.
func TestTriggerReconcile_ManyChildrenNoTruncation(t *testing.T) {
	// 168h of */30 fires = 336 slots; each real fire spawns a work bead AND an
	// audit-ledger bead in prod (~2 beads/fire ⇒ ~672 labeled children — the
	// live trigger carried 528), so seed BOTH per slot. That pushes the labeled
	// set well past the old 500 single-page cap; sorted -created_at, the cap
	// dropped the OLDEST beads = the leading-edge slots, manufacturing the
	// phantom in-window misses. On-slot children anchored to :00/:30 marks so
	// they align with */30.
	topOfHalfHour := time.Now().UTC().Truncate(30 * time.Minute)
	// Seed a few extra slots PAST the 168h window (slots > 336) so every
	// in-window expected fire is definitely covered by a child — the reconcile
	// window clamps the leading edge, and we want to assert on truncation, not
	// on a boundary rounding artifact.
	const slots = 360
	const inWindow = 336
	children := make([]reconcileBead, 0, slots*2)
	for i := 0; i <= slots; i++ {
		ft := topOfHalfHour.Add(time.Duration(-i) * 30 * time.Minute)
		fireKey := ft.Format(time.RFC3339)
		children = append(children, reconcileBead{
			ID:        "gd-fire" + strconv.Itoa(i),
			Type:      "task",
			Status:    "closed",
			Labels:    []string{"trigger:gd-trig30"},
			Fields:    map[string]string{"schedule_fire_time": fireKey, "schedule_cron": "*/30 * * * *"},
			CreatedAt: fireKey,
		})
		// The audit receipt the schedule source writes on every fire (same
		// minute; dedups against the work bead — never a phantom second fire).
		children = append(children, reconcileBead{
			ID:        "gd-audit" + strconv.Itoa(i),
			Type:      "event",
			Status:    "closed",
			Labels:    []string{"auto:trigger-audit", "trigger:gd-trig30"},
			Fields:    map[string]string{"outcome": "allow", "match_key": ft.Format("2006-01-02T15:04")},
			CreatedAt: ft.Add(time.Second).Format(time.RFC3339),
		})
	}
	d := &reconcileDaemon{
		trigger: reconcileBead{
			ID: "gd-trig30", Type: "trigger", Status: "open", Title: "busy half-hour trigger",
			Fields: map[string]string{
				"cron":        "*/30 * * * *",
				"enabled":     "true",
				"wake_intent": "every 30 minutes",
				"last_run":    topOfHalfHour.Add(-30 * time.Minute).Format(time.RFC3339),
			},
			// Created before the whole window so no creation-clamp trims the
			// leading edge — every slot in-window is genuinely expected.
			CreatedAt: topOfHalfHour.Add(-slots * 30 * time.Minute).Format(time.RFC3339),
		},
		children: children,
	}
	setupReconcileDaemon(t, d)

	rep := runReconcileJSON(t, "gd-trig30", 168*time.Hour, 15*time.Minute)
	if rep.Fires == nil {
		t.Fatal("expected a Fires reconciliation")
	}
	// The whole labeled set must be reconstructed — not a single capped page.
	// The pre-fix code capped at 500 sorted -created_at and reconstructed only
	// ~250 fires (dropping every leading-edge slot); paging recovers all but the
	// single oldest slot, which legitimately falls just before the 168h window
	// start (a window-edge clamp, not truncation). Anything near the old cap
	// means the OLDEST children were dropped.
	if rep.ObservedFireCount < inWindow {
		t.Fatalf("truncated fire reconstruction: observed %d, expected >= %d in-window fires (oldest fires were dropped by a capped single-page query)",
			rep.ObservedFireCount, inWindow)
	}
	// With every in-window slot covered, zero candidate misses and zero drift.
	if rep.Fires.MissedCount != 0 {
		t.Fatalf("expected 0 missed fires with a fully-reconstructed child set, got %d missed (%d matched)",
			rep.Fires.MissedCount, rep.Fires.MatchedCount)
	}
	if rep.DriftCandidateMissed != 0 {
		t.Fatalf("expected drift_candidate_missed=0, got %d", rep.DriftCandidateMissed)
	}
}

func TestTriggerReconcile_NoCronEventOnly(t *testing.T) {
	// An event-only trigger (no cron) yields a note, not a Fires diff.
	d := &reconcileDaemon{
		trigger: reconcileBead{
			ID: "gd-trigEVENT", Type: "trigger", Status: "open", Title: "event gardener",
			Fields: map[string]string{
				"enabled":     "true",
				"wake_intent": "whenever an MR goes green",
			},
		},
	}
	setupReconcileDaemon(t, d)

	rep := runReconcileJSON(t, "gd-trigEVENT", 24*time.Hour, 10*time.Minute)
	if rep.Fires != nil {
		t.Fatalf("event-only trigger should have no Fires diff, got %+v", rep.Fires)
	}
	if rep.Note == "" {
		t.Fatal("expected an explanatory note for the event-only trigger")
	}
}

func TestTriggerReconcile_InvalidCronNoted(t *testing.T) {
	// A malformed cron is surfaced as an impossible-mechanism note, not a hard
	// command failure, so the reconciler can file an impossible-config bug.
	d := &reconcileDaemon{
		trigger: reconcileBead{
			ID: "gd-trigBAD", Type: "trigger", Status: "open", Title: "broken cron",
			Fields: map[string]string{
				"cron":    "every blue moon",
				"enabled": "true",
			},
		},
	}
	setupReconcileDaemon(t, d)

	rep := runReconcileJSON(t, "gd-trigBAD", 24*time.Hour, 10*time.Minute)
	if rep.Fires != nil {
		t.Fatalf("invalid cron should produce no Fires diff, got %+v", rep.Fires)
	}
	if !strings.Contains(rep.Note, "impossible") && !strings.Contains(rep.Note, "invalid") {
		t.Fatalf("expected impossible/invalid note, got %q", rep.Note)
	}
}

// setupReconcileDaemonBead installs the httptest daemon and returns the fetched
// BeadDetail (with CreatedAt populated from the served created_at), so a test
// can drive buildReconcileReport directly with a fixed `now` for determinism.
func setupReconcileDaemonBead(t *testing.T, d *reconcileDaemon, id string) *beadsapi.BeadDetail {
	t.Helper()
	setupReconcileDaemon(t, d)
	oldLim := reconcileChildLim
	reconcileChildLim = 500
	t.Cleanup(func() { reconcileChildLim = oldLim })
	bead, err := daemon.GetBead(t.Context(), id)
	if err != nil {
		t.Fatalf("GetBead(%s): %v", id, err)
	}
	if bead == nil {
		t.Fatalf("GetBead(%s) returned nil", id)
	}
	return bead
}

// TestTriggerReconcile_PreCreationFiresNotMissed is the regression for bug
// ka-0000004392. The OSS-Sanitization Audit Gardener (trigger ka-0000007535)
// fires "13 9 * * 1" tz America/Los_Angeles — Monday 09:13 PT — and was created
// 2026-07-29. When the hourly reconciler ran with a 168h window ending
// ~2026-08-02, the ONLY Monday 09:13 PT slot inside that window was
// 2026-07-27T16:13Z — three days BEFORE the trigger existed. The engine
// correctly computed that Monday instant (it is NOT a Saturday, contra the
// drift report), but counting it as a candidate "miss" is a phantom: the
// trigger could not have fired before it was created. That phantom missed=1 is
// what spawned this whole false "habitual drift" bug.
//
// After the creation-time clamp, the effective window starts at the trigger's
// creation time, the pre-creation Monday slot is excluded, and the report shows
// 0 expected / 0 missed with WindowClampedToCreation=true — no phantom drift.
func TestTriggerReconcile_PreCreationFiresNotMissed(t *testing.T) {
	// Real bug coordinates: created 2026-07-29, reconciled ~2026-08-02, 168h.
	created := "2026-07-29T22:23:59Z"
	now := mustTime(t, "2026-08-02T21:49:00Z")

	d := &reconcileDaemon{
		trigger: reconcileBead{
			ID: "ka-0000007535", Type: "trigger", Status: "open",
			Title:     "OSS-Sanitization Audit Gardener",
			CreatedAt: created,
			Fields: map[string]string{
				"cron":        "13 9 * * 1",
				"timezone":    "America/Los_Angeles",
				"enabled":     "true",
				"wake_intent": "Every Monday 09:13 PT, scan the monorepo for drift and land a report.",
				// No last_run inside window, no children: the trigger genuinely
				// has no observed fire in the window, so the only thing that could
				// manufacture a "miss" is a pre-creation expected slot.
			},
		},
	}
	bead := setupReconcileDaemonBead(t, d, "ka-0000007535")

	oldWindow, oldTol := reconcileWindow, reconcileTolerance
	reconcileWindow, reconcileTolerance = 168*time.Hour, scheduler.DefaultReconcileTolerance
	t.Cleanup(func() { reconcileWindow, reconcileTolerance = oldWindow, oldTol })

	rep := buildReconcileReport(t.Context(), bead, now)

	if !rep.WindowClampedToCreation {
		t.Fatal("expected window to be clamped to the trigger's creation time")
	}
	if got, want := rep.WindowStartEffective.UTC(), mustTime(t, created).UTC(); !got.Equal(want) {
		t.Fatalf("effective window start: want %s (creation), got %s", want, got)
	}
	if rep.Fires == nil {
		t.Fatal("expected a Fires reconciliation")
	}
	// The pre-creation 2026-07-27 Monday slot must be excluded. The next Monday
	// after creation (2026-08-03) is past the window end, so zero expected.
	if rep.Fires.ExpectedCount != 0 {
		t.Fatalf("want 0 expected fires after the clamp (pre-creation Monday excluded), got %d: %+v",
			rep.Fires.ExpectedCount, rep.Fires.Expected)
	}
	if rep.Fires.MissedCount != 0 {
		t.Fatalf("want 0 missed fires (no phantom pre-creation miss), got %d", rep.Fires.MissedCount)
	}
}

// TestTriggerReconcile_PreCreationClampKeepsRealMisses proves the clamp does not
// over-correct: a genuine post-creation missed fire is still reported. Same
// trigger, but `now` is advanced past the first post-creation Monday
// (2026-08-03 09:13 PT = 16:13Z) with no observed fire, so that Monday is a
// real, in-lifetime miss the reconciler must still surface.
func TestTriggerReconcile_PreCreationClampKeepsRealMisses(t *testing.T) {
	created := "2026-07-29T22:23:59Z"
	// A Wednesday after the 2026-08-03 Monday fire; 168h window reaches back to
	// ~2026-07-29, so the clamp (creation) barely tightens it and the 08-03
	// Monday slot is well inside the trigger's lifetime.
	now := mustTime(t, "2026-08-05T00:00:00Z")

	d := &reconcileDaemon{
		trigger: reconcileBead{
			ID: "ka-0000007535", Type: "trigger", Status: "open",
			Title:     "OSS-Sanitization Audit Gardener",
			CreatedAt: created,
			Fields: map[string]string{
				"cron":     "13 9 * * 1",
				"timezone": "America/Los_Angeles",
				"enabled":  "true",
			},
		},
	}
	bead := setupReconcileDaemonBead(t, d, "ka-0000007535")

	oldWindow, oldTol := reconcileWindow, reconcileTolerance
	reconcileWindow, reconcileTolerance = 168*time.Hour, scheduler.DefaultReconcileTolerance
	t.Cleanup(func() { reconcileWindow, reconcileTolerance = oldWindow, oldTol })

	rep := buildReconcileReport(t.Context(), bead, now)

	if rep.Fires == nil {
		t.Fatal("expected a Fires reconciliation")
	}
	// The 2026-08-03 Monday fire is inside the trigger's lifetime and inside the
	// window; with no observed fire it is a real miss the clamp must NOT swallow.
	if rep.Fires.ExpectedCount != 1 {
		t.Fatalf("want 1 expected post-creation Monday fire, got %d: %+v",
			rep.Fires.ExpectedCount, rep.Fires.Expected)
	}
	if rep.Fires.MissedCount != 1 {
		t.Fatalf("want the real post-creation miss still reported, got %d missed", rep.Fires.MissedCount)
	}
	if got := rep.Fires.Expected[0].At.UTC(); !got.Equal(mustTime(t, "2026-08-03T16:13:00Z")) {
		t.Fatalf("expected the 2026-08-03 16:13Z Monday fire, got %s", got)
	}
}

// TestTriggerReconcile_HumanOutputClampBanner exercises the human (non-JSON)
// render path and asserts the creation-clamp banner appears when the window was
// clamped. This is the same bug-ka-0000004392 trigger; the terminal spot-check
// surface must tell a human WHY the expected count is lower than a naive
// window/period estimate.
func TestTriggerReconcile_HumanOutputClampBanner(t *testing.T) {
	d := &reconcileDaemon{
		trigger: reconcileBead{
			ID: "ka-0000007535", Type: "trigger", Status: "open",
			Title:     "OSS-Sanitization Audit Gardener",
			CreatedAt: "2026-07-29T22:23:59Z",
			Fields: map[string]string{
				"cron":        "13 9 * * 1",
				"timezone":    "America/Los_Angeles",
				"enabled":     "true",
				"wake_intent": "Every Monday 09:13 PT, scan for drift and land a report.",
			},
		},
	}
	bead := setupReconcileDaemonBead(t, d, "ka-0000007535")

	oldJSON := jsonOutput
	jsonOutput = false
	oldWindow, oldTol := reconcileWindow, reconcileTolerance
	reconcileWindow, reconcileTolerance = 168*time.Hour, scheduler.DefaultReconcileTolerance
	t.Cleanup(func() {
		jsonOutput = oldJSON
		reconcileWindow, reconcileTolerance = oldWindow, oldTol
	})

	report := buildReconcileReport(t.Context(), bead, mustTime(t, "2026-08-02T21:49:00Z"))
	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	cmd.SetOut(&buf)
	if err := emitReconcileReport(cmd, report); err != nil {
		t.Fatalf("emitReconcileReport: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "clamped to trigger creation") {
		t.Fatalf("human output should show the creation-clamp banner, got:\n%s", got)
	}
}

// TestTriggerReconcile_DisabledOneShotEarlyFireNotMissed is the regression for
// bug ka-0000007223, the trailing-edge twin of ka-0000004392's creation clamp.
//
// The one-shot deferred-enable trigger ka-0000000021 (cron `33 2 2 8 *` UTC —
// fire once at 02:33 on Aug 2) exists solely to re-enable a pour trigger, then
// self-disable. Its active hour-window opens at 02:00 (the schedule engine's
// InWindow matches month/day/hour and IGNORES the cron minute), so it fired
// EARLY at 02:00:48Z, served its mission, and the spawned agent set
// enabled=false before the nominal 02:33Z minute ever arrived. Because the
// trigger is now disabled, the engine skips it at load and its last_checked is
// frozen at that 02:00:48Z fire.
//
// The reconciler enumerated the 02:33Z slot — 33 min after the 02:00 observed
// fire, past the 10m tolerance — and reported a phantom missed=1 (+ extra=1)
// that spawned this false "impossible" drift bug. That 02:33Z slot falls AFTER
// the trigger went dark: it could not have fired it. After the disable clamp,
// the effective window end is the disable instant (last_checked), the
// post-disable 02:33Z slot is excluded, and there is no phantom miss.
func TestTriggerReconcile_DisabledOneShotEarlyFireNotMissed(t *testing.T) {
	// Real bug coordinates.
	created := "2026-08-01T18:35:27Z"
	lastCheck := "2026-08-02T02:00:48Z" // frozen when it self-disabled
	now := mustTime(t, "2026-08-02T21:49:00Z")

	d := &reconcileDaemon{
		trigger: reconcileBead{
			ID: "ka-0000000021", Type: "trigger", Status: "open",
			Title:     "Deferred enable: pour trigger (t+8h)",
			CreatedAt: created,
			Fields: map[string]string{
				"cron":         "33 2 2 8 *",
				"timezone":     "UTC",
				"enabled":      "false", // self-disabled after serving its mission
				"wake_intent":  "Fire ONCE at ~2026-08-02 02:33Z to re-enable the pour trigger, then self-disable.",
				"last_checked": lastCheck,
				"last_run":     lastCheck, // the served early fire, reconstructed as the observed fire
			},
		},
	}
	bead := setupReconcileDaemonBead(t, d, "ka-0000000021")

	oldWindow, oldTol := reconcileWindow, reconcileTolerance
	reconcileWindow, reconcileTolerance = 168*time.Hour, scheduler.DefaultReconcileTolerance
	t.Cleanup(func() { reconcileWindow, reconcileTolerance = oldWindow, oldTol })

	rep := buildReconcileReport(t.Context(), bead, now)

	if !rep.WindowClampedToDisable {
		t.Fatal("expected the window end to be clamped to the disabled trigger's last_checked")
	}
	if got, want := rep.WindowEndEffective.UTC(), mustTime(t, lastCheck).UTC(); !got.Equal(want) {
		t.Fatalf("effective window end: want %s (disable instant), got %s", want, got)
	}
	if rep.Fires == nil {
		t.Fatal("expected a Fires reconciliation")
	}
	// The post-disable 02:33Z slot must be excluded (it is after the disable
	// instant), so there is no expected fire and — critically — no phantom miss.
	if rep.Fires.MissedCount != 0 {
		t.Fatalf("want 0 missed (post-disable 02:33Z slot excluded), got %d: %+v",
			rep.Fires.MissedCount, rep.Fires.Expected)
	}
	if rep.Fires.ExpectedCount != 0 {
		t.Fatalf("want 0 expected fires in the live window, got %d: %+v",
			rep.Fires.ExpectedCount, rep.Fires.Expected)
	}
	// The early InWindow fire that self-disabled the trigger is legitimate, not
	// an over-fire: its nominal cron minute (02:33Z) is post-disable and was
	// excluded from expected, so the observed 02:00 fire must NOT be flagged as
	// an extra. This assertion closes the gap that let the phantom extra=1
	// survive MR !6483 and re-file as bug ka-0000001257.
	if rep.Fires.ExtraCount != 0 {
		t.Fatalf("want 0 extra over-fires (early InWindow self-disable fire is not an over-fire), got %d: %v",
			rep.Fires.ExtraCount, rep.Fires.Extra)
	}
}

// TestTriggerReconcile_DisabledOneShotEarlyFireNotOverFire is the extra=1 twin of
// TestTriggerReconcile_DisabledOneShotEarlyFireNotMissed and the direct
// regression for bug ka-0000001257. It reconstructs the exact live coordinates:
// the one-shot deferred-enable trigger ka-0000000021 (cron `33 2 2 8 *` UTC)
// fired EARLY at 02:00:48Z because the schedule engine's InWindow matches
// month/day/hour and ignores the cron minute, spawned an agent that self-disabled
// it at ~02:06, and froze last_checked at the fire instant. The 02:33Z cron slot
// never arrived (post-disable, excluded from expected by the clamp), so the
// observed 02:00 fire has no expected counterpart. Before the fix it was
// misreported as extra=1 (a phantom over-fire); after it, the early InWindow
// fire is recognized as the self-disabling fire and Extra is empty, with a Note
// explaining the exclusion.
func TestTriggerReconcile_DisabledOneShotEarlyFireNotOverFire(t *testing.T) {
	created := "2026-08-01T18:35:27Z"
	lastCheck := "2026-08-02T02:00:48Z" // frozen when it self-disabled
	now := mustTime(t, "2026-08-02T21:49:00Z")

	d := &reconcileDaemon{
		trigger: reconcileBead{
			ID: "ka-0000000021", Type: "trigger", Status: "open",
			Title:     "Deferred enable: pour trigger (t+8h)",
			CreatedAt: created,
			Fields: map[string]string{
				"cron":         "33 2 2 8 *",
				"timezone":     "UTC",
				"enabled":      "false",
				"wake_intent":  "Fire ONCE at ~2026-08-02 02:33Z to re-enable the pour trigger, then self-disable.",
				"last_checked": lastCheck,
				"last_run":     lastCheck, // the served early fire, reconstructed as the observed fire
			},
		},
	}
	bead := setupReconcileDaemonBead(t, d, "ka-0000000021")

	oldWindow, oldTol := reconcileWindow, reconcileTolerance
	reconcileWindow, reconcileTolerance = 168*time.Hour, scheduler.DefaultReconcileTolerance
	t.Cleanup(func() { reconcileWindow, reconcileTolerance = oldWindow, oldTol })

	rep := buildReconcileReport(t.Context(), bead, now)

	if !rep.WindowClampedToDisable {
		t.Fatal("expected the window end to be clamped to the disabled trigger's last_checked")
	}
	if rep.Fires == nil {
		t.Fatal("expected a Fires reconciliation")
	}
	if rep.Fires.ExtraCount != 0 || len(rep.Fires.Extra) != 0 {
		t.Fatalf("want 0 extra over-fires, got %d: %v", rep.Fires.ExtraCount, rep.Fires.Extra)
	}
	// The clamp already excludes the post-disable slot, so nothing is expected
	// or missed either — the trigger fired once, correctly, and self-disabled.
	if rep.Fires.MissedCount != 0 || rep.Fires.ExpectedCount != 0 {
		t.Fatalf("want 0 expected / 0 missed after the clamp, got expected=%d missed=%d",
			rep.Fires.ExpectedCount, rep.Fires.MissedCount)
	}
	if !strings.Contains(rep.Note, "self-disabling InWindow fire") {
		t.Fatalf("want a Note explaining the excluded early fire, got %q", rep.Note)
	}
}

// TestTriggerReconcile_DisabledGenuineOutOfWindowOverFireKept proves the extra
// filter does not swallow a REAL over-fire: a disabled trigger whose observed
// fire landed OUTSIDE any active cron window (InWindow=false) is still flagged as
// an extra. Cron `33 2 2 8 *` is active only in hour 02 on Aug 2; an observed
// fire at 05:10 on Aug 2 is out-of-window and spurious, so it must survive the
// disable-boundary filter as an over-fire signal.
func TestTriggerReconcile_DisabledGenuineOutOfWindowOverFireKept(t *testing.T) {
	created := "2026-08-01T18:35:27Z"
	lastCheck := "2026-08-02T05:15:00Z" // disabled after the spurious out-of-window fire
	now := mustTime(t, "2026-08-02T21:49:00Z")

	d := &reconcileDaemon{
		trigger: reconcileBead{
			ID: "gd-trigOOW", Type: "trigger", Status: "open",
			Title:     "one-shot with a genuine out-of-window over-fire",
			CreatedAt: created,
			Fields: map[string]string{
				"cron":         "33 2 2 8 *",
				"timezone":     "UTC",
				"enabled":      "false",
				"wake_intent":  "Fire ONCE at ~2026-08-02 02:33Z.",
				"last_checked": lastCheck,
				"last_run":     "2026-08-02T05:10:00Z", // spurious fire outside the active hour window
			},
		},
	}
	bead := setupReconcileDaemonBead(t, d, "gd-trigOOW")

	oldWindow, oldTol := reconcileWindow, reconcileTolerance
	reconcileWindow, reconcileTolerance = 168*time.Hour, scheduler.DefaultReconcileTolerance
	t.Cleanup(func() { reconcileWindow, reconcileTolerance = oldWindow, oldTol })

	rep := buildReconcileReport(t.Context(), bead, now)

	if !rep.WindowClampedToDisable {
		t.Fatal("expected the window end to be clamped to the disable instant")
	}
	if rep.Fires == nil {
		t.Fatal("expected a Fires reconciliation")
	}
	if rep.Fires.ExtraCount != 1 || len(rep.Fires.Extra) != 1 {
		t.Fatalf("a genuine out-of-window fire must remain an over-fire, got extra=%d: %v",
			rep.Fires.ExtraCount, rep.Fires.Extra)
	}
	if got := rep.Fires.Extra[0].UTC(); !got.Equal(mustTime(t, "2026-08-02T05:10:00Z").UTC()) {
		t.Fatalf("want the 05:10 out-of-window fire flagged, got %s", got)
	}
}

// TestTriggerReconcile_DisabledClampKeepsPreDisableMisses proves the disable
// clamp does not over-correct: a fire the trigger genuinely missed WHILE STILL
// LIVE (before it was disabled) is still reported. An hourly gardener runs live
// through 03:30, misses its 02:00 slot (no observed fire there), then is
// disabled at 03:40. The 02:00 miss is in-lifetime and must survive the clamp;
// the post-disable 04:00/05:00 slots must be excluded.
func TestTriggerReconcile_DisabledClampKeepsPreDisableMisses(t *testing.T) {
	// Anchor to fixed instants for determinism.
	lastCheck := "2026-07-01T03:40:00Z" // disabled here
	now := mustTime(t, "2026-07-01T05:30:00Z")

	d := &reconcileDaemon{
		trigger: reconcileBead{
			ID: "gd-trigDISABLED", Type: "trigger", Status: "open",
			Title:     "hourly gardener, later disabled",
			CreatedAt: "2026-06-01T00:00:00Z", // old enough that creation does not clamp
			Fields: map[string]string{
				"cron":         "0 * * * *",
				"enabled":      "false",
				"wake_intent":  "every hour on the hour",
				"last_checked": lastCheck,
				// last_run at 03:00 → observed fires at 01:00 and 03:00 via children;
				// the 02:00 slot has no observed fire and is a real in-lifetime miss.
				"last_run": "2026-07-01T03:00:00Z",
			},
		},
		children: []reconcileBead{
			{ID: "gd-h1", Type: "task", Fields: map[string]string{"schedule_fire_time": "2026-07-01T01:00:00Z"}},
			{ID: "gd-h3", Type: "task", Fields: map[string]string{"schedule_fire_time": "2026-07-01T03:00:00Z"}},
		},
	}
	bead := setupReconcileDaemonBead(t, d, "gd-trigDISABLED")

	oldWindow, oldTol := reconcileWindow, reconcileTolerance
	reconcileWindow, reconcileTolerance = 6*time.Hour, scheduler.DefaultReconcileTolerance
	t.Cleanup(func() { reconcileWindow, reconcileTolerance = oldWindow, oldTol })

	rep := buildReconcileReport(t.Context(), bead, now)

	if !rep.WindowClampedToDisable {
		t.Fatal("expected the window end to be clamped to the disable instant")
	}
	if rep.Fires == nil {
		t.Fatal("expected a Fires reconciliation")
	}
	// Live window is (start, 03:40]. Expected hourly slots inside it that the
	// trigger could have fired: 00:00?, 01:00, 02:00, 03:00 — but start is
	// now-6h = 2026-06-30T23:30, so 00:00/01:00/02:00/03:00 are all in-window.
	// 01:00 and 03:00 matched (observed), 02:00 missed. 00:00 also has no
	// observed fire → also a real miss. The post-disable 04:00/05:00 are excluded.
	missed := map[string]bool{}
	for _, ef := range rep.Fires.Expected {
		if !ef.Matched {
			missed[ef.At.UTC().Format(time.RFC3339)] = true
		}
	}
	if !missed["2026-07-01T02:00:00Z"] {
		t.Fatalf("the real in-lifetime 02:00 miss must survive the clamp; missed=%v", missed)
	}
	// The post-disable slots must NOT appear as expected at all.
	for _, ef := range rep.Fires.Expected {
		if ef.At.UTC().After(mustTime(t, lastCheck).UTC()) {
			t.Fatalf("post-disable slot %s must be excluded by the clamp", ef.At.UTC())
		}
	}
	if rep.Fires.MatchedCount != 2 {
		t.Fatalf("want the 01:00 + 03:00 observed fires matched, got %d", rep.Fires.MatchedCount)
	}
}

// TestTriggerReconcile_DisabledBeforeWindowEmptyLiveWindow covers the steady
// state a self-disabled one-shot reaches once its fire ages past the lookback
// window: last_checked is now BEFORE the window start, so the trigger had no
// live interval anywhere in the window. The clamp must report zero-expected via
// an explanatory note rather than resurrecting the phantom miss (which is what a
// plain now-window end would do) or erroring on an inverted window.
func TestTriggerReconcile_DisabledBeforeWindowEmptyLiveWindow(t *testing.T) {
	// The ka-0000000021 fire (02:00:48Z Aug 2) is now >168h in the past.
	lastCheck := "2026-08-02T02:00:48Z"
	now := mustTime(t, "2026-08-12T00:00:00Z") // ~10 days later; window start is Aug 5

	d := &reconcileDaemon{
		trigger: reconcileBead{
			ID: "ka-0000000021", Type: "trigger", Status: "open",
			Title:     "Deferred enable: pour trigger (t+8h)",
			CreatedAt: "2026-08-01T18:35:27Z",
			Fields: map[string]string{
				"cron":         "33 2 2 8 *",
				"timezone":     "UTC",
				"enabled":      "false",
				"wake_intent":  "Fire ONCE at ~2026-08-02 02:33Z to re-enable the pour trigger, then self-disable.",
				"last_checked": lastCheck,
				"last_run":     lastCheck,
			},
		},
	}
	bead := setupReconcileDaemonBead(t, d, "ka-0000000021")

	oldWindow, oldTol := reconcileWindow, reconcileTolerance
	reconcileWindow, reconcileTolerance = 168*time.Hour, scheduler.DefaultReconcileTolerance
	t.Cleanup(func() { reconcileWindow, reconcileTolerance = oldWindow, oldTol })

	rep := buildReconcileReport(t.Context(), bead, now)

	if !rep.WindowClampedToDisable {
		t.Fatal("expected the window end to be clamped to the disable instant")
	}
	// The whole live window is outside the lookback window → no Fires diff, an
	// explanatory note, and NO phantom miss.
	if rep.Fires != nil {
		t.Fatalf("expected no Fires diff for a trigger disabled before the window opened, got %+v", rep.Fires)
	}
	if !strings.Contains(rep.Note, "disabled") || !strings.Contains(rep.Note, "nothing was missed") {
		t.Fatalf("expected an empty-live-window note, got %q", rep.Note)
	}
}

// TestTriggerReconcile_EnabledTriggerNotEndClamped guards that the disable clamp
// is inert for a live (enabled) trigger: its window end stays at `now` and no
// disable banner/flag appears, so the clamp cannot mask a genuine miss on an
// enabled gardener.
func TestTriggerReconcile_EnabledTriggerNotEndClamped(t *testing.T) {
	now := mustTime(t, "2026-07-01T05:30:00Z")
	d := &reconcileDaemon{
		trigger: reconcileBead{
			ID: "gd-trigLIVE", Type: "trigger", Status: "open",
			Title:     "live hourly gardener",
			CreatedAt: "2026-06-01T00:00:00Z",
			Fields: map[string]string{
				"cron":         "0 * * * *",
				"enabled":      "true",
				"wake_intent":  "every hour on the hour",
				"last_checked": "2026-07-01T05:29:30Z", // fresh, but must be ignored while enabled
			},
		},
	}
	bead := setupReconcileDaemonBead(t, d, "gd-trigLIVE")

	oldWindow, oldTol := reconcileWindow, reconcileTolerance
	reconcileWindow, reconcileTolerance = 6*time.Hour, scheduler.DefaultReconcileTolerance
	t.Cleanup(func() { reconcileWindow, reconcileTolerance = oldWindow, oldTol })

	rep := buildReconcileReport(t.Context(), bead, now)

	if rep.WindowClampedToDisable {
		t.Fatal("an enabled trigger must not be end-clamped by the disable logic")
	}
	if got := rep.WindowEndEffective.UTC(); !got.Equal(now.UTC()) {
		t.Fatalf("enabled trigger window end must stay at now (%s), got %s", now.UTC(), got)
	}
}

// TestTriggerReconcile_HumanOutputDisableBanner exercises the human (non-JSON)
// render path for the disable clamp: the terminal spot-check surface must tell a
// human WHY a post-disable cron slot was excluded, mirroring the creation-clamp
// banner. Uses the gd-DisabledClampKeepsPreDisableMisses coordinates so Fires is
// non-nil (the banner only renders alongside a Fires diff).
func TestTriggerReconcile_HumanOutputDisableBanner(t *testing.T) {
	d := &reconcileDaemon{
		trigger: reconcileBead{
			ID: "gd-trigDISABLED", Type: "trigger", Status: "open",
			Title:     "hourly gardener, later disabled",
			CreatedAt: "2026-06-01T00:00:00Z",
			Fields: map[string]string{
				"cron":         "0 * * * *",
				"enabled":      "false",
				"wake_intent":  "every hour on the hour",
				"last_checked": "2026-07-01T03:40:00Z",
				"last_run":     "2026-07-01T03:00:00Z",
			},
		},
	}
	bead := setupReconcileDaemonBead(t, d, "gd-trigDISABLED")

	oldJSON := jsonOutput
	jsonOutput = false
	oldWindow, oldTol := reconcileWindow, reconcileTolerance
	reconcileWindow, reconcileTolerance = 6*time.Hour, scheduler.DefaultReconcileTolerance
	t.Cleanup(func() {
		jsonOutput = oldJSON
		reconcileWindow, reconcileTolerance = oldWindow, oldTol
	})

	report := buildReconcileReport(t.Context(), bead, mustTime(t, "2026-07-01T05:30:00Z"))
	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	cmd.SetOut(&buf)
	if err := emitReconcileReport(cmd, report); err != nil {
		t.Fatalf("emitReconcileReport: %v", err)
	}
	if got := buf.String(); !strings.Contains(got, "end clamped to disable") {
		t.Fatalf("human output should show the disable-clamp banner, got:\n%s", got)
	}
}

func TestTriggerReconcile_RejectsNonTrigger(t *testing.T) {
	d := &reconcileDaemon{
		trigger: reconcileBead{ID: "gd-notatrig", Type: "task", Status: "open", Title: "a task"},
	}
	setupReconcileDaemon(t, d)

	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	err := runTriggerReconcile(cmd, []string{"gd-notatrig"})
	if err == nil || !strings.Contains(err.Error(), "expected type=trigger") {
		t.Fatalf("expected type-mismatch error, got %v", err)
	}
}

// fleetReconcileDaemon serves a whole roster for the --all survey: a roster
// list (GET /v1/beads?type=trigger&status=open, no labels=) plus, per trigger,
// its children keyed by the trigger:<id> label query.
type fleetReconcileDaemon struct {
	roster   []reconcileBead
	children map[string][]reconcileBead // trigger ID → its schedule_fire_time children
}

func (d *fleetReconcileDaemon) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		// Per-trigger child query (labels=trigger:<id>).
		case r.URL.Path == "/v1/beads" && strings.HasPrefix(r.URL.Query().Get("labels"), "trigger:"):
			id := strings.TrimPrefix(r.URL.Query().Get("labels"), "trigger:")
			kids := d.children[id]
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": kids, "total": len(kids)})
		// Roster list (type=trigger, no labels).
		case r.URL.Path == "/v1/beads":
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": d.roster, "total": len(d.roster)})
		default:
			http.NotFound(w, r)
		}
	})
}

func setupFleetReconcileDaemon(t *testing.T, d *fleetReconcileDaemon) {
	t.Helper()
	srv := httptest.NewServer(d.handler())
	t.Cleanup(srv.Close)
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	oldDaemon := daemon
	daemon = c
	t.Cleanup(func() { daemon = oldDaemon })
}

// runReconcileFleetJSON drives `runTriggerReconcile` in --all mode and returns
// the parsed fleet envelope.
func runReconcileFleetJSON(t *testing.T, window, tolerance time.Duration) reconcileFleetReport {
	t.Helper()
	oldJSON := jsonOutput
	jsonOutput = true
	oldWindow, oldTol, oldLim, oldAll, oldRoster := reconcileWindow, reconcileTolerance, reconcileChildLim, reconcileAll, reconcileRosterLim
	reconcileWindow, reconcileTolerance, reconcileChildLim, reconcileAll, reconcileRosterLim = window, tolerance, 500, true, 1000
	t.Cleanup(func() {
		jsonOutput = oldJSON
		reconcileWindow, reconcileTolerance, reconcileChildLim, reconcileAll, reconcileRosterLim = oldWindow, oldTol, oldLim, oldAll, oldRoster
	})

	var fleet reconcileFleetReport
	out := captureStdout(t, func() error {
		cmd := &cobra.Command{}
		cmd.SetContext(t.Context())
		return runTriggerReconcile(cmd, nil)
	})
	if err := json.Unmarshal(out, &fleet); err != nil {
		t.Fatalf("unmarshal fleet from %q: %v", string(out), err)
	}
	return fleet
}

func TestTriggerReconcileAll_SkipsTriggersWithoutWakeIntent(t *testing.T) {
	topOfHour := time.Now().UTC().Truncate(time.Hour)
	mk := func(h int) string { return topOfHour.Add(time.Duration(-h) * time.Hour).Format(time.RFC3339) }

	d := &fleetReconcileDaemon{
		roster: []reconcileBead{
			// Auditable: declares a wake_intent.
			{ID: "gd-trigA", Type: "trigger", Status: "open", Title: "hourly A",
				Fields: map[string]string{"cron": "0 * * * *", "enabled": "true", "wake_intent": "every hour", "last_run": mk(1)}},
			// Not auditable: no wake_intent — must be surveyed but skipped.
			{ID: "gd-trigB", Type: "trigger", Status: "open", Title: "no intent B",
				Fields: map[string]string{"cron": "0 * * * *", "enabled": "true"}},
			// Auditable, event-only: wake_intent set but no cron → note, no Fires.
			{ID: "gd-trigC", Type: "trigger", Status: "open", Title: "event C",
				Fields: map[string]string{"enabled": "true", "wake_intent": "whenever an MR goes green"}},
		},
		children: map[string][]reconcileBead{
			"gd-trigA": {
				{ID: "gd-a1", Type: "task", Fields: map[string]string{"schedule_fire_time": mk(1)}},
				{ID: "gd-a2", Type: "task", Fields: map[string]string{"schedule_fire_time": mk(2)}},
			},
		},
	}
	setupFleetReconcileDaemon(t, d)

	fleet := runReconcileFleetJSON(t, 6*time.Hour, 15*time.Minute)

	if fleet.Surveyed != 3 {
		t.Fatalf("expected 3 surveyed, got %d", fleet.Surveyed)
	}
	if fleet.Reconciled != 2 {
		t.Fatalf("expected 2 reconciled (A + C carry wake_intent), got %d", fleet.Reconciled)
	}
	if fleet.Skipped != 1 {
		t.Fatalf("expected 1 skipped (B has no wake_intent), got %d", fleet.Skipped)
	}
	if len(fleet.Reports) != 2 {
		t.Fatalf("expected 2 reports, got %d", len(fleet.Reports))
	}

	byID := map[string]*reconcileReport{}
	for _, r := range fleet.Reports {
		byID[r.TriggerID] = r
	}
	if byID["gd-trigB"] != nil {
		t.Fatal("gd-trigB has no wake_intent and must not appear in reports")
	}
	a := byID["gd-trigA"]
	if a == nil || a.Fires == nil || a.Fires.MatchedCount == 0 {
		t.Fatalf("gd-trigA should reconcile with matched fires, got %+v", a)
	}
	c := byID["gd-trigC"]
	if c == nil || c.Fires != nil || c.Note == "" {
		t.Fatalf("gd-trigC (event-only) should have a note and no Fires diff, got %+v", c)
	}
}

func TestTriggerReconcileAll_EmptyFleet(t *testing.T) {
	d := &fleetReconcileDaemon{
		roster: []reconcileBead{
			{ID: "gd-trigX", Type: "trigger", Status: "open", Title: "no intent",
				Fields: map[string]string{"cron": "0 * * * *", "enabled": "true"}},
		},
	}
	setupFleetReconcileDaemon(t, d)

	fleet := runReconcileFleetJSON(t, 24*time.Hour, 10*time.Minute)
	if fleet.Surveyed != 1 || fleet.Reconciled != 0 || fleet.Skipped != 1 {
		t.Fatalf("expected 1 surveyed / 0 reconciled / 1 skipped, got %+v", fleet)
	}
	if fleet.Reports == nil {
		t.Fatal("Reports should be a non-nil empty slice so the JSON envelope has a [] not null")
	}
	if len(fleet.Reports) != 0 {
		t.Fatalf("expected 0 reports, got %d", len(fleet.Reports))
	}
}

func TestTriggerReconcileAll_RejectsIDWithAllFlag(t *testing.T) {
	oldAll := reconcileAll
	reconcileAll = true
	t.Cleanup(func() { reconcileAll = oldAll })

	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	err := runTriggerReconcile(cmd, []string{"gd-trigA"})
	if err == nil || !strings.Contains(err.Error(), "do not also pass a trigger id") {
		t.Fatalf("expected error rejecting id+--all, got %v", err)
	}
}

func TestTriggerReconcile_RequiresIDWithoutAllFlag(t *testing.T) {
	oldAll := reconcileAll
	reconcileAll = false
	t.Cleanup(func() { reconcileAll = oldAll })

	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	err := runTriggerReconcile(cmd, nil)
	if err == nil || !strings.Contains(err.Error(), "single trigger id") {
		t.Fatalf("expected error requiring an id, got %v", err)
	}
}

// TestTriggerReconcile_AuditLedgerBeadsNotFires is the regression for the FIRST
// of two over-fire false positives in bug ka-0000004209. Every fire also writes
// an append-only type=event audit-ledger bead (recordTriggerAudit,
// trigger_permissions.go) labeled auto:trigger-audit + trigger:<id>. It carries
// NO schedule_fire_time, so the CreatedAt fallback in reconstructObservedFires
// used to count it as a SECOND observed fire, seconds after the real one — a
// phantom "extra" over-fire that also masqueraded as "two fires inside cooldown".
//
// Real coordinates: the 2026-07-30 fire spawned task gd-G7YKb6g9Ly
// (schedule_fire_time 13:17) AND audit bead gd-MtUA6F4yTX (created 13:19). The
// pre-fix reconciler reported extra=2 (13:17 + 13:19) for what was ONE fire.
// After the fix the audit bead is skipped and only the real fire is observed.
func TestTriggerReconcile_AuditLedgerBeadsNotFires(t *testing.T) {
	created := "2026-07-29T22:23:59Z"
	now := mustTime(t, "2026-08-01T00:00:00Z")

	d := &reconcileDaemon{
		trigger: reconcileBead{
			ID: "ka-0000007535", Type: "trigger", Status: "open",
			Title:     "OSS-Sanitization Audit Gardener",
			CreatedAt: created,
			Fields: map[string]string{
				// Current cron matches the fire's stamped cron, so the ONLY thing
				// that can manufacture an "extra" here is the audit-ledger bead.
				"cron":        "17 6 * * 4",
				"timezone":    "America/Los_Angeles",
				"enabled":     "true",
				"wake_intent": "Thursday audit",
			},
		},
		children: []reconcileBead{
			// The real fire: a spawned task with the canonical fire stamp.
			{
				ID: "gd-G7YKb6g9Ly", Type: "task", Status: "closed",
				Labels:    []string{"project:gardenio", "source:schedule", "trigger:ka-0000007535"},
				CreatedAt: "2026-07-30T13:19:22Z",
				Fields: map[string]string{
					"schedule_fire_time": "2026-07-30T13:17:00Z",
					"schedule_fire_key":  "2026-07-30T13:17",
					"schedule_cron":      "17 6 * * 4",
				},
			},
			// The audit-ledger receipt: same trigger:<id> label, no fire stamp,
			// created ~2 min after. Pre-fix this became a phantom second fire.
			{
				ID: "gd-MtUA6F4yTX", Type: "event", Status: "closed",
				Labels:    []string{"auto:trigger-audit", "source:schedule", "trigger:ka-0000007535"},
				CreatedAt: "2026-07-30T13:19:24Z",
				Fields:    map[string]string{"outcome": "allow", "source": "schedule"},
			},
		},
	}
	bead := setupReconcileDaemonBead(t, d, "ka-0000007535")

	oldWindow, oldTol := reconcileWindow, reconcileTolerance
	reconcileWindow, reconcileTolerance = 168*time.Hour, scheduler.DefaultReconcileTolerance
	t.Cleanup(func() { reconcileWindow, reconcileTolerance = oldWindow, oldTol })

	rep := buildReconcileReport(t.Context(), bead, now)
	if rep.Fires == nil {
		t.Fatal("expected a Fires reconciliation")
	}
	// The one real Thursday fire matches its cron slot; the audit bead is gone.
	if rep.ObservedFireCount != 1 {
		t.Fatalf("want exactly 1 observed fire (audit bead excluded), got %d", rep.ObservedFireCount)
	}
	if rep.Fires.ExtraCount != 0 {
		t.Fatalf("want 0 extra fires (audit-ledger bead is not a fire), got %d: %v",
			rep.Fires.ExtraCount, rep.Fires.Extra)
	}
	if rep.Fires.MatchedCount != 1 {
		t.Fatalf("want the real Thursday fire matched, got %d matched", rep.Fires.MatchedCount)
	}
}

// TestTriggerReconcile_StaleCronFireNotOverFire is the regression for the SECOND
// over-fire false positive in bug ka-0000004209. When an operator edits a
// trigger's cron, a legitimate fire made under the OLD schedule has no
// counterpart in the NEW cron the reconciler diffs against, so it was flagged as
// "extra" (over-firing). A fire the trigger correctly made under its
// then-current mechanism is not over-firing.
//
// Real coordinates: the trigger fired 2026-07-30 13:17Z under cron "17 6 * * 4"
// (Thu 06:17 PT — the spawned task records schedule_cron="17 6 * * 4"). The cron
// was later edited to "13 9 * * 1" (Mon 09:13 PT). Diffing the Thursday fire
// against the Monday cron flagged extra=1. This is the SAME cron-edit-boundary
// class MR !6478 fixed at the creation boundary — which is why the drift
// recurred as "habitual". After the fix, a fire stamped with a structurally
// different cron than the trigger's current one is dropped from the observed set.
func TestTriggerReconcile_StaleCronFireNotOverFire(t *testing.T) {
	created := "2026-07-29T22:23:59Z"
	// A Tuesday after the 2026-08-03 Monday fire, so the current-cron Monday fire
	// is inside the window and matched, and the ONLY candidate "extra" is the
	// pre-edit Thursday fire.
	now := mustTime(t, "2026-08-04T00:00:00Z")

	d := &reconcileDaemon{
		trigger: reconcileBead{
			ID: "ka-0000007535", Type: "trigger", Status: "open",
			Title:     "OSS-Sanitization Audit Gardener",
			CreatedAt: created,
			Fields: map[string]string{
				// CURRENT cron — Monday 09:13 PT. The 07-30 fire predates the edit.
				"cron":        "13 9 * * 1",
				"timezone":    "America/Los_Angeles",
				"enabled":     "true",
				"wake_intent": "Every Monday 09:13 PT, scan for drift and land a report.",
			},
		},
		children: []reconcileBead{
			// Legitimate fire under the OLD cron (Thursday 06:17 PT). The task
			// records the schedule it fired under.
			{
				ID: "gd-G7YKb6g9Ly", Type: "task", Status: "closed",
				Labels:    []string{"project:gardenio", "source:schedule", "trigger:ka-0000007535"},
				CreatedAt: "2026-07-30T13:19:22Z",
				Fields: map[string]string{
					"schedule_fire_time": "2026-07-30T13:17:00Z",
					"schedule_fire_key":  "2026-07-30T13:17",
					"schedule_cron":      "17 6 * * 4",
				},
			},
			// Correct fire under the CURRENT cron (Monday 09:13 PT = 16:13Z).
			{
				ID: "gd-au8xY41Bsf", Type: "task", Status: "closed",
				Labels:    []string{"project:gardenio", "source:schedule", "trigger:ka-0000007535"},
				CreatedAt: "2026-08-03T16:13:03Z",
				Fields: map[string]string{
					"schedule_fire_time": "2026-08-03T16:13:00Z",
					"schedule_fire_key":  "2026-08-03T16:13",
					"schedule_cron":      "13 9 * * 1",
				},
			},
		},
	}
	bead := setupReconcileDaemonBead(t, d, "ka-0000007535")

	oldWindow, oldTol := reconcileWindow, reconcileTolerance
	reconcileWindow, reconcileTolerance = 168*time.Hour, scheduler.DefaultReconcileTolerance
	t.Cleanup(func() { reconcileWindow, reconcileTolerance = oldWindow, oldTol })

	rep := buildReconcileReport(t.Context(), bead, now)
	if rep.Fires == nil {
		t.Fatal("expected a Fires reconciliation")
	}
	// Only the current-cron Monday fire is observed; the pre-edit Thursday fire
	// is dropped as belonging to a superseded schedule.
	if rep.ObservedFireCount != 1 {
		t.Fatalf("want exactly 1 observed fire (stale-cron fire dropped), got %d", rep.ObservedFireCount)
	}
	if rep.Fires.ExtraCount != 0 {
		t.Fatalf("want 0 extra fires (pre-edit fire is not over-firing), got %d: %v",
			rep.Fires.ExtraCount, rep.Fires.Extra)
	}
	if rep.Fires.MatchedCount != 1 {
		t.Fatalf("want the current-cron Monday fire matched, got %d matched", rep.Fires.MatchedCount)
	}
	if rep.Fires.MissedCount != 0 {
		t.Fatalf("want 0 missed (the one expected Monday fire matched), got %d", rep.Fires.MissedCount)
	}
}

// TestTriggerReconcile_FullBugReproduction ties both defects together on the
// EXACT live coordinates of bug ka-0000004209: the 168h reconcile window that
// reported "expected=1 matched=1 extra=2". The two extras were (1) the
// audit-ledger bead at 13:19 and (2) the pre-cron-edit Thursday fire at 13:17.
// Pre-fix this drove the habitual over-fire drift bug; post-fix the report is
// clean: expected=1 matched=1 extra=0.
func TestTriggerReconcile_FullBugReproduction(t *testing.T) {
	created := "2026-07-29T22:23:59Z"
	// The reconcile instant from the bug's 168h window (2026-08-03 ~22:48Z).
	now := mustTime(t, "2026-08-03T22:48:00Z")

	d := &reconcileDaemon{
		trigger: reconcileBead{
			ID: "ka-0000007535", Type: "trigger", Status: "open",
			Title:     "OSS-Sanitization Audit Gardener",
			CreatedAt: created,
			Fields: map[string]string{
				"cron":        "13 9 * * 1",
				"timezone":    "America/Los_Angeles",
				"enabled":     "true",
				"wake_intent": "Every Monday 09:13 PT, scan for drift and land a report.",
			},
		},
		children: []reconcileBead{
			// The 07-30 fire (old cron) + its audit receipt — the two phantom extras.
			{
				ID: "gd-G7YKb6g9Ly", Type: "task", Status: "closed",
				Labels:    []string{"project:gardenio", "source:schedule", "trigger:ka-0000007535"},
				CreatedAt: "2026-07-30T13:19:22Z",
				Fields: map[string]string{
					"schedule_fire_time": "2026-07-30T13:17:00Z",
					"schedule_cron":      "17 6 * * 4",
				},
			},
			{
				ID: "gd-MtUA6F4yTX", Type: "event", Status: "closed",
				Labels:    []string{"auto:trigger-audit", "source:schedule", "trigger:ka-0000007535"},
				CreatedAt: "2026-07-30T13:19:24Z",
				Fields:    map[string]string{"outcome": "allow"},
			},
			// The correct Monday fire + its audit receipt.
			{
				ID: "gd-au8xY41Bsf", Type: "task", Status: "closed",
				Labels:    []string{"project:gardenio", "source:schedule", "trigger:ka-0000007535"},
				CreatedAt: "2026-08-03T16:13:03Z",
				Fields: map[string]string{
					"schedule_fire_time": "2026-08-03T16:13:00Z",
					"schedule_cron":      "13 9 * * 1",
				},
			},
			{
				ID: "gd-XkUMlGzTaV", Type: "event", Status: "closed",
				Labels:    []string{"auto:trigger-audit", "source:schedule", "trigger:ka-0000007535"},
				CreatedAt: "2026-08-03T16:13:04Z",
				Fields:    map[string]string{"outcome": "allow"},
			},
		},
	}
	bead := setupReconcileDaemonBead(t, d, "ka-0000007535")

	oldWindow, oldTol := reconcileWindow, reconcileTolerance
	reconcileWindow, reconcileTolerance = 168*time.Hour, scheduler.DefaultReconcileTolerance
	t.Cleanup(func() { reconcileWindow, reconcileTolerance = oldWindow, oldTol })

	rep := buildReconcileReport(t.Context(), bead, now)
	if rep.Fires == nil {
		t.Fatal("expected a Fires reconciliation")
	}
	f := rep.Fires
	if f.ExpectedCount != 1 || f.MatchedCount != 1 || f.MissedCount != 0 || f.ExtraCount != 0 {
		t.Fatalf("bug ka-0000004209 NOT fixed: want expected=1 matched=1 missed=0 extra=0, got expected=%d matched=%d missed=%d extra=%d (extra=%v)",
			f.ExpectedCount, f.MatchedCount, f.MissedCount, f.ExtraCount, f.Extra)
	}
	if rep.ObservedFireCount != 1 {
		t.Fatalf("want 1 real observed fire (2 audit beads + 1 stale-cron fire excluded), got %d", rep.ObservedFireCount)
	}
}

// TestTriggerReconcile_ApplyFormulaFireFromAuditKey is the regression for bug
// ka-0000007130. An apply_formula trigger (the daily-standup convergence
// gardener) pours a MOLECULE, which is labeled project:<p> only — NO trigger:<id>
// label and NO schedule_fire_time — and the driver agent likewise carries
// neither. So for such a trigger the ONLY durable trigger:<id>-labeled per-fire
// record is the append-only audit-ledger bead recordTriggerAudit writes. Before
// the fix, reconstructObservedFires skipped audit beads entirely and the
// observed set collapsed to last_run (a single timestamp = the LATEST fire), so
// every earlier real fire read as a phantom "unexplained-miss".
//
// Live coordinates: cron "30 8 * * 1-5" tz America/Los_Angeles (8:30 AM PT =
// 15:30 UTC in August). Two weekday fires — Tue 2026-08-04 15:30Z and Wed
// 2026-08-05 15:30Z — each recorded an audit bead (match_key = the fire minute),
// but only 08-05 survived in last_run. Pre-fix: expected=2 matched=1 missed=1.
// Post-fix: the 08-04 audit key reconstructs the earlier fire, so both match.
func TestTriggerReconcile_ApplyFormulaFireFromAuditKey(t *testing.T) {
	created := "2026-08-03T17:24:21Z"
	now := mustTime(t, "2026-08-05T22:25:46Z")

	d := &reconcileDaemon{
		trigger: reconcileBead{
			ID: "gd-yXfVsb9YLx", Type: "trigger", Status: "open",
			Title:     "Daily-standup convergence gardener",
			CreatedAt: created,
			Fields: map[string]string{
				"cron":        "30 8 * * 1-5",
				"timezone":    "America/Los_Angeles",
				"enabled":     "true",
				"wake_intent": "Fire on weekday mornings ~8:30 AM PT.",
				// last_run holds only the LATEST fire — the sole observed fire pre-fix.
				"last_run": "2026-08-05T15:30:19Z",
			},
		},
		children: []reconcileBead{
			// Tue 08-04 fire: audit receipt only (apply_formula molecule carries no
			// trigger label / fire stamp, so the audit bead is the sole record).
			{
				ID: "gd-sUArOv16M1", Type: "event", Status: "closed",
				Labels:    []string{"auto:trigger-audit", "source:schedule", "trigger:gd-yXfVsb9YLx"},
				CreatedAt: "2026-08-04T15:30:16Z",
				Fields: map[string]string{
					"outcome":   "allow",
					"match_key": "2026-08-04T15:30",
					"source":    "schedule",
				},
			},
			// Wed 08-05 fire: audit receipt (same minute as last_run → dedups).
			{
				ID: "gd-FCerKYeQ9z", Type: "event", Status: "closed",
				Labels:    []string{"auto:trigger-audit", "source:schedule", "trigger:gd-yXfVsb9YLx"},
				CreatedAt: "2026-08-05T15:30:17Z",
				Fields: map[string]string{
					"outcome":   "allow",
					"match_key": "2026-08-05T15:30",
					"source":    "schedule",
				},
			},
		},
	}
	bead := setupReconcileDaemonBead(t, d, "gd-yXfVsb9YLx")

	oldWindow, oldTol := reconcileWindow, reconcileTolerance
	reconcileWindow, reconcileTolerance = 168*time.Hour, scheduler.DefaultReconcileTolerance
	t.Cleanup(func() { reconcileWindow, reconcileTolerance = oldWindow, oldTol })

	rep := buildReconcileReport(t.Context(), bead, now)
	if rep.Fires == nil {
		t.Fatal("expected a Fires reconciliation")
	}
	f := rep.Fires
	// Both weekday fires are observed (08-04 from its audit key, 08-05 from
	// last_run + its same-minute audit key deduped), so nothing is missed.
	if f.ExpectedCount != 2 || f.MatchedCount != 2 || f.MissedCount != 0 || f.ExtraCount != 0 {
		t.Fatalf("bug ka-0000007130 NOT fixed: want expected=2 matched=2 missed=0 extra=0, got expected=%d matched=%d missed=%d extra=%d (extra=%v)",
			f.ExpectedCount, f.MatchedCount, f.MissedCount, f.ExtraCount, f.Extra)
	}
	// Two distinct fires reconstructed — the 08-05 audit receipt collapsed into
	// the last_run fire at the same minute rather than becoming a third fire.
	if rep.ObservedFireCount != 2 {
		t.Fatalf("want 2 observed fires (audit 08-04 + last_run/audit 08-05 deduped), got %d", rep.ObservedFireCount)
	}
}

// TestTriggerReconcile_AuditKeyDedupsRealFire guards regression 1 in
// reconstructObservedFires: when a fire spawns BOTH a schedule_fire_time work
// bead AND its audit receipt (both at the same cron minute), the audit key must
// collapse into the real fire in the minute-dedup — never become a phantom
// second fire ~seconds later (the pre-ka-0000004209 CreatedAt-fallback defect).
func TestTriggerReconcile_AuditKeyDedupsRealFire(t *testing.T) {
	created := "2026-07-29T22:23:59Z"
	now := mustTime(t, "2026-08-01T00:00:00Z")

	d := &reconcileDaemon{
		trigger: reconcileBead{
			ID: "ka-0000007535", Type: "trigger", Status: "open",
			Title:     "OSS-Sanitization Audit Gardener",
			CreatedAt: created,
			Fields: map[string]string{
				"cron":        "17 6 * * 4",
				"timezone":    "America/Los_Angeles",
				"enabled":     "true",
				"wake_intent": "Thursday audit",
			},
		},
		children: []reconcileBead{
			// The real fire: a spawned task with the canonical fire stamp.
			{
				ID: "gd-G7YKb6g9Ly", Type: "task", Status: "closed",
				Labels:    []string{"project:gardenio", "source:schedule", "trigger:ka-0000007535"},
				CreatedAt: "2026-07-30T13:19:22Z",
				Fields: map[string]string{
					"schedule_fire_time": "2026-07-30T13:17:00Z",
					"schedule_fire_key":  "2026-07-30T13:17",
					"schedule_cron":      "17 6 * * 4",
				},
			},
			// The audit receipt for the SAME fire (same match_key minute).
			{
				ID: "gd-MtUA6F4yTX", Type: "event", Status: "closed",
				Labels:    []string{"auto:trigger-audit", "source:schedule", "trigger:ka-0000007535"},
				CreatedAt: "2026-07-30T13:19:24Z",
				Fields: map[string]string{
					"outcome":   "allow",
					"match_key": "2026-07-30T13:17",
					"source":    "schedule",
				},
			},
		},
	}
	bead := setupReconcileDaemonBead(t, d, "ka-0000007535")

	oldWindow, oldTol := reconcileWindow, reconcileTolerance
	reconcileWindow, reconcileTolerance = 168*time.Hour, scheduler.DefaultReconcileTolerance
	t.Cleanup(func() { reconcileWindow, reconcileTolerance = oldWindow, oldTol })

	rep := buildReconcileReport(t.Context(), bead, now)
	if rep.Fires == nil {
		t.Fatal("expected a Fires reconciliation")
	}
	if rep.ObservedFireCount != 1 {
		t.Fatalf("want exactly 1 observed fire (audit receipt deduped into the real fire), got %d", rep.ObservedFireCount)
	}
	if rep.Fires.ExtraCount != 0 {
		t.Fatalf("want 0 extra fires (audit receipt is the same fire, not a second), got %d: %v",
			rep.Fires.ExtraCount, rep.Fires.Extra)
	}
	if rep.Fires.MatchedCount != 1 {
		t.Fatalf("want the real Thursday fire matched, got %d matched", rep.Fires.MatchedCount)
	}
}

// TestTriggerReconcile_StaleCronAuditKeyDropped guards regression 2 for the
// audit-fallback path: an audit bead carries no schedule_cron, so the direct
// stale-cron comparison used for work beads cannot apply. Its fallback fire is
// instead validated against the CURRENT cron via cronFiresAt — a fire minute the
// trigger's current cron no longer schedules is dropped, so an audit receipt from
// before an operator's cron edit does not resurrect as a phantom over-fire.
func TestTriggerReconcile_StaleCronAuditKeyDropped(t *testing.T) {
	created := "2026-07-29T22:23:59Z"
	now := mustTime(t, "2026-08-04T00:00:00Z")

	d := &reconcileDaemon{
		trigger: reconcileBead{
			ID: "ka-0000007535", Type: "trigger", Status: "open",
			Title:     "OSS-Sanitization Audit Gardener",
			CreatedAt: created,
			Fields: map[string]string{
				// CURRENT cron — Monday 09:13 PT (16:13Z). The Thursday audit key
				// below (13:17Z) is not a fire minute under this schedule.
				"cron":        "13 9 * * 1",
				"timezone":    "America/Los_Angeles",
				"enabled":     "true",
				"wake_intent": "Every Monday 09:13 PT.",
			},
		},
		children: []reconcileBead{
			// Audit receipt from a fire under the OLD Thursday cron (06:17 PT =
			// 13:17Z). No schedule_cron on an audit bead, so the current-cron
			// cronFiresAt guard is what must drop it.
			{
				ID: "gd-MtUA6F4yTX", Type: "event", Status: "closed",
				Labels:    []string{"auto:trigger-audit", "source:schedule", "trigger:ka-0000007535"},
				CreatedAt: "2026-07-30T13:19:24Z",
				Fields: map[string]string{
					"outcome":   "allow",
					"match_key": "2026-07-30T13:17",
					"source":    "schedule",
				},
			},
			// The correct fire under the CURRENT Monday cron, as a work bead.
			{
				ID: "gd-au8xY41Bsf", Type: "task", Status: "closed",
				Labels:    []string{"project:gardenio", "source:schedule", "trigger:ka-0000007535"},
				CreatedAt: "2026-08-03T16:13:03Z",
				Fields: map[string]string{
					"schedule_fire_time": "2026-08-03T16:13:00Z",
					"schedule_cron":      "13 9 * * 1",
				},
			},
		},
	}
	bead := setupReconcileDaemonBead(t, d, "ka-0000007535")

	oldWindow, oldTol := reconcileWindow, reconcileTolerance
	reconcileWindow, reconcileTolerance = 168*time.Hour, scheduler.DefaultReconcileTolerance
	t.Cleanup(func() { reconcileWindow, reconcileTolerance = oldWindow, oldTol })

	rep := buildReconcileReport(t.Context(), bead, now)
	if rep.Fires == nil {
		t.Fatal("expected a Fires reconciliation")
	}
	// Only the current-cron Monday fire is observed; the stale Thursday audit key
	// is dropped by the cronFiresAt guard rather than flagged extra.
	if rep.ObservedFireCount != 1 {
		t.Fatalf("want exactly 1 observed fire (stale-cron audit key dropped), got %d", rep.ObservedFireCount)
	}
	if rep.Fires.ExtraCount != 0 {
		t.Fatalf("want 0 extra fires (pre-edit audit key is not over-firing), got %d: %v",
			rep.Fires.ExtraCount, rep.Fires.Extra)
	}
	if rep.Fires.MissedCount != 0 {
		t.Fatalf("want 0 missed (the one expected Monday fire matched), got %d", rep.Fires.MissedCount)
	}
}

// TestTriggerReconcile_DeniedAuditKeyNotFire guards that a denied/errored audit
// bead never counts as a fire: outcome != "allow" contributes nothing, so a
// blocked cron tick (denied by the human-operator gate, a condition, etc.) does
// not manufacture a phantom observed fire that masks a real miss.
func TestTriggerReconcile_DeniedAuditKeyNotFire(t *testing.T) {
	created := "2026-08-03T17:24:21Z"
	now := mustTime(t, "2026-08-05T22:25:46Z")

	d := &reconcileDaemon{
		trigger: reconcileBead{
			ID: "gd-yXfVsb9YLx", Type: "trigger", Status: "open",
			Title:     "Daily-standup convergence gardener",
			CreatedAt: created,
			Fields: map[string]string{
				"cron":        "30 8 * * 1-5",
				"timezone":    "America/Los_Angeles",
				"enabled":     "true",
				"wake_intent": "Fire on weekday mornings ~8:30 AM PT.",
				"last_run":    "2026-08-05T15:30:19Z",
			},
		},
		children: []reconcileBead{
			// Tue 08-04: the fire was DENIED — its audit bead must not count, so
			// the 08-04 cron slot correctly reports as missed.
			{
				ID: "gd-denied04", Type: "event", Status: "closed",
				Labels:    []string{"auto:trigger-audit", "source:schedule", "trigger:gd-yXfVsb9YLx"},
				CreatedAt: "2026-08-04T15:30:16Z",
				Fields: map[string]string{
					"outcome":   "deny",
					"match_key": "2026-08-04T15:30",
					"source":    "schedule",
				},
			},
		},
	}
	bead := setupReconcileDaemonBead(t, d, "gd-yXfVsb9YLx")

	oldWindow, oldTol := reconcileWindow, reconcileTolerance
	reconcileWindow, reconcileTolerance = 168*time.Hour, scheduler.DefaultReconcileTolerance
	t.Cleanup(func() { reconcileWindow, reconcileTolerance = oldWindow, oldTol })

	rep := buildReconcileReport(t.Context(), bead, now)
	if rep.Fires == nil {
		t.Fatal("expected a Fires reconciliation")
	}
	// Only the 08-05 fire (from last_run) is observed; the denied 08-04 tick is
	// a real miss, not masked by its audit receipt.
	if rep.ObservedFireCount != 1 {
		t.Fatalf("want exactly 1 observed fire (denied audit bead excluded), got %d", rep.ObservedFireCount)
	}
	if rep.Fires.MissedCount != 1 {
		t.Fatalf("want the denied 08-04 slot reported missed, got %d missed", rep.Fires.MissedCount)
	}
}

// TestTriggerReconcile_LateLastRunSnapsToNominalSlot is the headline regression
// for bug ka-0000002813 — the THIRD recurrence on the intake groomer
// ka-0000006210, and the one MR !6488's gate probe could not catch because it
// explains gated MISSES, never a phantom EXTRA.
//
// The live signature: the groomer fired its 18:03Z cron slot but the engine's
// poll + cooldown + preflight pushed the actual EXECUTION to 18:10:16Z, which is
// what gardener_evaluator.go stamps into last_run (time.Now, wall-clock), while
// the spawned child records the NOMINAL minute 18:03 in schedule_fire_time. The
// two land in different minute buckets, the minute-dedup fails, and the one real
// fire is counted twice: 18:03 matches its cron slot, 18:10 falls through to
// Extra as a phantom over-fire → matched=1 missed=… extra=1, which the reconciler
// (whose gate block explains only misses) classifies HABITUAL and re-files.
//
// After the fix, the within-tolerance late last_run snaps back to 18:03 and
// collapses into the child fire: exactly one observed fire, zero extra.
func TestTriggerReconcile_LateLastRunSnapsToNominalSlot(t *testing.T) {
	now := mustTime(t, "2026-08-07T19:00:00Z")

	d := &reconcileDaemon{
		trigger: reconcileBead{
			ID: "ka-0000006210", Type: "trigger", Status: "open",
			Title:     "Gardener-intake groomer",
			CreatedAt: "2026-08-01T00:00:00Z",
			Fields: map[string]string{
				"cron":        "3,33 * * * *",
				"timezone":    "America/Los_Angeles",
				"enabled":     "true",
				"wake_intent": "de-lane needs-intake beads",
				// Execution wall-clock 7m16s past the 18:03Z nominal slot.
				"last_run": "2026-08-07T18:10:16Z",
			},
		},
		children: []reconcileBead{
			// The SAME fire's canonical record: the spawned child stamps the
			// nominal cron minute, not the execution time.
			{
				ID: "gd-child1803", Type: "task", Status: "closed",
				Labels:    []string{"project:gardenio", "source:schedule", "trigger:ka-0000006210"},
				CreatedAt: "2026-08-07T18:10:20Z",
				Fields: map[string]string{
					"schedule_fire_time": "2026-08-07T18:03:00Z",
					"schedule_fire_key":  "2026-08-07T18:03",
					"schedule_cron":      "3,33 * * * *",
				},
			},
		},
	}
	bead := setupReconcileDaemonBead(t, d, "ka-0000006210")

	oldWindow, oldTol := reconcileWindow, reconcileTolerance
	reconcileWindow, reconcileTolerance = 168*time.Hour, scheduler.DefaultReconcileTolerance
	t.Cleanup(func() { reconcileWindow, reconcileTolerance = oldWindow, oldTol })

	rep := buildReconcileReport(t.Context(), bead, now)
	if rep.Fires == nil {
		t.Fatal("expected a Fires reconciliation")
	}
	if rep.ObservedFireCount != 1 {
		t.Fatalf("want exactly 1 observed fire (late last_run snapped onto its 18:03 slot and deduped), got %d",
			rep.ObservedFireCount)
	}
	if rep.Fires.ExtraCount != 0 {
		t.Fatalf("want 0 extra fires (the 18:10 execution IS the 18:03 fire, not an over-fire), got %d: %v",
			rep.Fires.ExtraCount, rep.Fires.Extra)
	}
	if rep.Fires.MatchedCount != 1 {
		t.Fatalf("want the 18:03 slot matched by the snapped fire, got %d matched", rep.Fires.MatchedCount)
	}
}

// TestTriggerReconcile_LateLastRunNoChildStillOneFire proves the snap works even
// when last_run is the ONLY fire source (the child was reaped): the single late
// execution must reconstruct as exactly one matched fire on its nominal slot, not
// one matched (a phantom nominal) plus one extra.
func TestTriggerReconcile_LateLastRunNoChildStillOneFire(t *testing.T) {
	now := mustTime(t, "2026-08-07T19:00:00Z")

	d := &reconcileDaemon{
		trigger: reconcileBead{
			ID: "ka-0000006210", Type: "trigger", Status: "open",
			Title:     "Gardener-intake groomer",
			CreatedAt: "2026-08-01T00:00:00Z",
			Fields: map[string]string{
				"cron":        "3,33 * * * *",
				"timezone":    "America/Los_Angeles",
				"enabled":     "true",
				"wake_intent": "de-lane needs-intake beads",
				"last_run":    "2026-08-07T18:10:16Z",
			},
		},
	}
	bead := setupReconcileDaemonBead(t, d, "ka-0000006210")

	oldWindow, oldTol := reconcileWindow, reconcileTolerance
	reconcileWindow, reconcileTolerance = 168*time.Hour, scheduler.DefaultReconcileTolerance
	t.Cleanup(func() { reconcileWindow, reconcileTolerance = oldWindow, oldTol })

	rep := buildReconcileReport(t.Context(), bead, now)
	if rep.Fires == nil {
		t.Fatal("expected a Fires reconciliation")
	}
	if rep.ObservedFireCount != 1 {
		t.Fatalf("want exactly 1 observed fire, got %d", rep.ObservedFireCount)
	}
	if rep.Fires.MatchedCount != 1 || rep.Fires.ExtraCount != 0 {
		t.Fatalf("want the late last_run matched to its 18:03 slot with no extra, got matched=%d extra=%d: %v",
			rep.Fires.MatchedCount, rep.Fires.ExtraCount, rep.Fires.Extra)
	}
}

// TestTriggerReconcile_SpuriousLastRunFarFromCronStaysExtra proves the snap is
// BOUNDED: a last_run that is NOT within tolerance of any cron slot (a genuinely
// spurious fire) is left untouched and still surfaces as a real over-fire, so the
// fix never weakens over-fire detection. Cron 3,33 has slots every 30m; a fire at
// :18 is 15m from both neighbours — beyond the 10m default tolerance.
func TestTriggerReconcile_SpuriousLastRunFarFromCronStaysExtra(t *testing.T) {
	now := mustTime(t, "2026-08-07T19:00:00Z")

	d := &reconcileDaemon{
		trigger: reconcileBead{
			ID: "ka-0000006210", Type: "trigger", Status: "open",
			Title:     "Gardener-intake groomer",
			CreatedAt: "2026-08-01T00:00:00Z",
			Fields: map[string]string{
				"cron":        "3,33 * * * *",
				"timezone":    "America/Los_Angeles",
				"enabled":     "true",
				"wake_intent": "de-lane needs-intake beads",
				// :18 is 15m from both :03 and :33 — a genuinely off-schedule fire.
				"last_run": "2026-08-07T18:18:00Z",
			},
		},
	}
	bead := setupReconcileDaemonBead(t, d, "ka-0000006210")

	oldWindow, oldTol := reconcileWindow, reconcileTolerance
	reconcileWindow, reconcileTolerance = 168*time.Hour, scheduler.DefaultReconcileTolerance
	t.Cleanup(func() { reconcileWindow, reconcileTolerance = oldWindow, oldTol })

	rep := buildReconcileReport(t.Context(), bead, now)
	if rep.Fires == nil {
		t.Fatal("expected a Fires reconciliation")
	}
	if rep.Fires.ExtraCount != 1 || len(rep.Fires.Extra) != 1 {
		t.Fatalf("a genuinely off-schedule last_run must remain an over-fire, got extra=%d: %v",
			rep.Fires.ExtraCount, rep.Fires.Extra)
	}
	if got := rep.Fires.Extra[0].UTC(); !got.Equal(mustTime(t, "2026-08-07T18:18:00Z").UTC()) {
		t.Fatalf("want the 18:18 off-schedule fire flagged as extra, got %s", got)
	}
}

// TestTriggerReconcile_VeryLateGatedFireNoPhantomExtra is the ka-0000003959
// regression: a CONDITION-GATED schedule trigger fires its nominal cron slot but
// only executes once the pre-flight gate passes, which can trail the nominal
// minute by MORE than the bounded snap tolerance (10m). Here the 18:08 slot did
// not execute until 18:26:13 — 18m13s late — so last_run at 18:26 is 18m from
// 18:08 and 12m from 18:38, beyond tolerance in BOTH directions. The bounded
// snap alone left it untouched and it fell through to Extra as a phantom
// over-fire (the observed real bug: matched=2 missed=... extra=1). Because the
// SAME fire is already recorded at its nominal 18:08 minute by the spawned work
// bead, the already-observed collapse must fold last_run onto it: 1 observed
// fire, 1 matched, 0 extra.
func TestTriggerReconcile_VeryLateGatedFireNoPhantomExtra(t *testing.T) {
	now := mustTime(t, "2026-08-14T19:00:00Z")

	d := &reconcileDaemon{
		trigger: reconcileBead{
			ID: "ka-0000007515", Type: "trigger", Status: "open",
			Title:     "Gardenio gardener-rescope: drain open needs-rescope beads",
			CreatedAt: "2026-07-25T00:00:00Z",
			Fields: map[string]string{
				"cron":        "8,38 * * * *",
				"timezone":    "America/Los_Angeles",
				"enabled":     "true",
				"wake_intent": "drain open needs-rescope beads",
				// Execution wall-clock 18m13s past the 18:08Z nominal slot — the
				// gated lane was empty until then. Beyond the 10m tolerance snap.
				"last_run": "2026-08-14T18:26:13Z",
			},
		},
		children: []reconcileBead{
			// The SAME fire's canonical record: the spawned work bead stamps the
			// nominal cron minute (18:08), not the 18:26 execution time.
			{
				ID: "gd-VSfX9ky8Y5", Type: "task", Status: "closed",
				Labels:    []string{"project:gardenio", "source:schedule", "trigger:ka-0000007515"},
				CreatedAt: "2026-08-14T18:26:13Z",
				Fields: map[string]string{
					"schedule_fire_time": "2026-08-14T18:08:00Z",
					"schedule_fire_key":  "2026-08-14T18:08",
					"schedule_cron":      "8,38 * * * *",
				},
			},
		},
	}
	bead := setupReconcileDaemonBead(t, d, "ka-0000007515")

	oldWindow, oldTol := reconcileWindow, reconcileTolerance
	reconcileWindow, reconcileTolerance = 168*time.Hour, scheduler.DefaultReconcileTolerance
	t.Cleanup(func() { reconcileWindow, reconcileTolerance = oldWindow, oldTol })

	rep := buildReconcileReport(t.Context(), bead, now)
	if rep.Fires == nil {
		t.Fatal("expected a Fires reconciliation")
	}
	if rep.ObservedFireCount != 1 {
		t.Fatalf("want exactly 1 observed fire (very-late last_run collapsed onto its already-observed 18:08 slot), got %d",
			rep.ObservedFireCount)
	}
	if rep.Fires.ExtraCount != 0 {
		t.Fatalf("want 0 extra fires (the 18:26 execution IS the 18:08 gated fire, not an over-fire), got %d: %v",
			rep.Fires.ExtraCount, rep.Fires.Extra)
	}
	if rep.Fires.MatchedCount != 1 {
		t.Fatalf("want the 18:08 slot matched by the collapsed fire, got %d matched", rep.Fires.MatchedCount)
	}
}

// TestTriggerReconcile_VeryLateLastRunNoChildStaysExtra guards the collapse's
// boundary: when NO durable per-fire bead recorded the slot (the child was
// reaped), a last_run beyond the bounded tolerance is NOT silently attributed to
// a nominal slot — the already-observed collapse only fires when a stronger
// source already saw that minute. Same 18m-late timestamp as the regression
// above but with no work bead: it must remain a real over-fire so genuinely
// off-cadence fires are never hidden.
func TestTriggerReconcile_VeryLateLastRunNoChildStaysExtra(t *testing.T) {
	now := mustTime(t, "2026-08-14T19:00:00Z")

	d := &reconcileDaemon{
		trigger: reconcileBead{
			ID: "ka-0000007515", Type: "trigger", Status: "open",
			Title:     "Gardenio gardener-rescope: drain open needs-rescope beads",
			CreatedAt: "2026-07-25T00:00:00Z",
			Fields: map[string]string{
				"cron":        "8,38 * * * *",
				"timezone":    "America/Los_Angeles",
				"enabled":     "true",
				"wake_intent": "drain open needs-rescope beads",
				"last_run":    "2026-08-14T18:26:13Z",
			},
		},
	}
	bead := setupReconcileDaemonBead(t, d, "ka-0000007515")

	oldWindow, oldTol := reconcileWindow, reconcileTolerance
	reconcileWindow, reconcileTolerance = 168*time.Hour, scheduler.DefaultReconcileTolerance
	t.Cleanup(func() { reconcileWindow, reconcileTolerance = oldWindow, oldTol })

	rep := buildReconcileReport(t.Context(), bead, now)
	if rep.Fires == nil {
		t.Fatal("expected a Fires reconciliation")
	}
	if rep.Fires.ExtraCount != 1 || len(rep.Fires.Extra) != 1 {
		t.Fatalf("a very-late last_run with no per-fire record must stay a candidate over-fire, got extra=%d: %v",
			rep.Fires.ExtraCount, rep.Fires.Extra)
	}
}

// TestTriggerReconcile_InWindowEarlyFireNotOverFire is the regression for bug
// ka-0000003140: a phantom over-firing drift (expected=1 matched=0 missed=1
// extra=1) reported against the OSS-Sanitization Audit Gardener (cron
// `13 9 * * 1` America/Los_Angeles) that in fact fired exactly once, on
// schedule.
//
// Root cause: the schedule engine fires via CronExpr.InWindow (month+day+hour,
// MINUTE IGNORED), so the weekly Monday-09:13-PT fire actually lands at the TOP
// of the 09:00 PDT hour = 16:00Z, 13 minutes BEFORE the nominal 16:13Z slot —
// beyond the flat 10m match tolerance. ReconcileFires then double-counts that
// one real fire as both a MISS (16:13 nominal unmatched) and an EXTRA (16:00
// observed with no counterpart). matchInWindowEarlyFires re-pairs the
// top-of-hour early fire with its unmatched nominal slot, collapsing the phantom
// miss+extra into one matched fire.
func TestTriggerReconcile_InWindowEarlyFireNotOverFire(t *testing.T) {
	created := "2026-07-29T22:23:59Z"
	// now is just after the observed Aug-17 fire, mirroring the live coordinates:
	// window = now-168h .. now, so the 16:13Z Monday slot is the only expected fire.
	now := mustTime(t, "2026-08-17T18:20:25Z")

	d := &reconcileDaemon{
		trigger: reconcileBead{
			ID: "ka-0000007535", Type: "trigger", Status: "open",
			Title:     "OSS-Sanitization Audit Gardener",
			CreatedAt: created,
			Fields: map[string]string{
				"cron":        "13 9 * * 1",
				"timezone":    "America/Los_Angeles",
				"enabled":     "true",
				"wake_intent": "Every Monday 09:13 PT, scan the monorepo and land one report bead.",
				// last_run holds the EXECUTION wall-clock of the top-of-hour fire
				// (16:00Z = 09:00 PDT), 13m before the 16:13Z nominal slot.
				"last_run": "2026-08-17T16:00:10Z",
			},
		},
		children: []reconcileBead{
			// The real fire's spawned work bead, stamped with the nominal cron slot.
			{
				ID: "gd-pv5Oq4g9lV", Type: "task", Status: "open",
				Labels:    []string{"project:gardenio", "source:schedule", "trigger:ka-0000007535"},
				CreatedAt: "2026-08-17T16:00:09Z",
				Fields: map[string]string{
					// Stamped with the PRIOR week's nominal minute (the catch-up slot
					// key); it falls before the 168h window start, so it is dropped as
					// a fire source and only last_run's 16:00 execution stamp survives —
					// the exact live shape that produced the phantom.
					"schedule_fire_time": "2026-08-10T16:13:00Z",
				},
			},
		},
	}
	bead := setupReconcileDaemonBead(t, d, "ka-0000007535")

	oldWindow, oldTol := reconcileWindow, reconcileTolerance
	reconcileWindow, reconcileTolerance = 168*time.Hour, scheduler.DefaultReconcileTolerance
	t.Cleanup(func() { reconcileWindow, reconcileTolerance = oldWindow, oldTol })

	rep := buildReconcileReport(t.Context(), bead, now)
	if rep.Fires == nil {
		t.Fatal("expected a Fires reconciliation")
	}
	// One expected Monday slot, matched by the top-of-hour early fire; no phantom.
	if rep.Fires.ExpectedCount != 1 {
		t.Fatalf("want expected=1 (one Monday 16:13Z slot in window), got %d", rep.Fires.ExpectedCount)
	}
	if rep.Fires.MatchedCount != 1 {
		t.Fatalf("want matched=1 (early InWindow fire re-paired with its nominal slot), got %d", rep.Fires.MatchedCount)
	}
	if rep.Fires.MissedCount != 0 {
		t.Fatalf("want missed=0 (the fire is not a miss), got %d", rep.Fires.MissedCount)
	}
	if rep.Fires.ExtraCount != 0 || len(rep.Fires.Extra) != 0 {
		t.Fatalf("want extra=0 (a scheduled top-of-hour early fire is not an over-fire), got extra=%d: %v",
			rep.Fires.ExtraCount, rep.Fires.Extra)
	}
	// The matched expected fire records the negative lateness (fired early).
	if ef := rep.Fires.Expected[0]; !ef.Matched || ef.LatenessS >= 0 {
		t.Fatalf("want the nominal slot matched with negative lateness (early fire), got matched=%v lateness=%ds",
			ef.Matched, ef.LatenessS)
	}
}

// TestTriggerReconcile_RealSecondFireStaysOverFire guards that the InWindow
// early-fire re-pairing does NOT hide a genuine over-fire: when a window's
// nominal cron slot is ALREADY matched by an on-time fire, a second fire inside
// that same window finds no unmatched slot to pair against and stays in Extra.
func TestTriggerReconcile_RealSecondFireStaysOverFire(t *testing.T) {
	created := "2026-07-29T22:23:59Z"
	now := mustTime(t, "2026-08-17T18:20:25Z")

	d := &reconcileDaemon{
		trigger: reconcileBead{
			ID: "ka-0000007535", Type: "trigger", Status: "open",
			Title:     "OSS-Sanitization Audit Gardener",
			CreatedAt: created,
			Fields: map[string]string{
				"cron":        "13 9 * * 1",
				"timezone":    "America/Los_Angeles",
				"enabled":     "true",
				"wake_intent": "Every Monday 09:13 PT.",
			},
		},
		children: []reconcileBead{
			// On-time fire at the nominal minute (16:13Z) — matches the slot.
			{
				ID: "gd-ontime", Type: "task", Status: "open",
				Labels:    []string{"project:gardenio", "source:schedule", "trigger:ka-0000007535"},
				CreatedAt: "2026-08-17T16:13:05Z",
				Fields:    map[string]string{"schedule_fire_time": "2026-08-17T16:13:00Z"},
			},
			// A genuine SECOND fire in the same active hour-window (16:40Z) — no
			// unmatched nominal slot remains, so it is a real over-fire.
			{
				ID: "gd-double", Type: "task", Status: "open",
				Labels:    []string{"project:gardenio", "source:schedule", "trigger:ka-0000007535"},
				CreatedAt: "2026-08-17T16:40:00Z",
				Fields:    map[string]string{"schedule_fire_time": "2026-08-17T16:40:00Z"},
			},
		},
	}
	bead := setupReconcileDaemonBead(t, d, "ka-0000007535")

	oldWindow, oldTol := reconcileWindow, reconcileTolerance
	reconcileWindow, reconcileTolerance = 168*time.Hour, scheduler.DefaultReconcileTolerance
	t.Cleanup(func() { reconcileWindow, reconcileTolerance = oldWindow, oldTol })

	rep := buildReconcileReport(t.Context(), bead, now)
	if rep.Fires == nil {
		t.Fatal("expected a Fires reconciliation")
	}
	if rep.Fires.MatchedCount != 1 {
		t.Fatalf("want matched=1 (the on-time nominal fire), got %d", rep.Fires.MatchedCount)
	}
	if rep.Fires.ExtraCount != 1 || len(rep.Fires.Extra) != 1 {
		t.Fatalf("a real second fire in an already-matched window must stay a candidate over-fire, got extra=%d: %v",
			rep.Fires.ExtraCount, rep.Fires.Extra)
	}
}
