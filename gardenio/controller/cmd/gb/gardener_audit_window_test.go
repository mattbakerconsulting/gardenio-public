package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// gardener_audit_window_test.go — regression tests for ka-0000001500: the
// `--limit` cap used to truncate the FLEET-WIDE agent-bead scan BEFORE the
// --since window and the --role/--schedule-id selectors were applied
// client-side, so a `--since 7d` audit silently collapsed to whatever the
// newest `limit` beads spanned. Measured on the real corpus at fix time:
// --role gardener-delivery --since 7d reported 79 cycles against a true 505,
// and --role plan-gardener reported 4 against a true 294 (the daemon's own
// count_only agrees with the post-fix numbers).
//
// The tests below drive runGardenerAudit against a fake daemon that enforces
// created_after / field / limit / offset exactly as the real one does, so a
// regression to client-side filtering fails here rather than in production.

// windowStub is a fake /v1/beads daemon that honours the query parameters the
// audit depends on. It deliberately applies created_after and the field
// predicates BEFORE limit/offset — matching the daemon — so any caller that
// goes back to filtering client-side under-reports and the test fails.
type windowStub struct {
	beads []map[string]any
	// maxPage mimics the daemon's server-side page-size clamp.
	maxPage int
	// requests records every limit the stub was asked for, so a test can
	// assert the audit paged rather than issuing one capped call.
	requests []string
}

func (s *windowStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/beads") {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"beads":[],"total":0}`))
			return
		}
		q := r.URL.Query()
		s.requests = append(s.requests, r.URL.RawQuery)

		// Type filter.
		wantTypes := map[string]bool{}
		for _, t := range strings.Split(q.Get("type"), ",") {
			if t != "" {
				wantTypes[t] = true
			}
		}

		// created_after — the window predicate this whole fix is about.
		var after time.Time
		if v := q.Get("created_after"); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				http.Error(w, "bad created_after", http.StatusBadRequest)
				return
			}
			after = t
		}

		// field=key=value predicates (role / schedule_id).
		fieldPreds := map[string]string{}
		for _, f := range q["field"] {
			k, v, ok := strings.Cut(f, "=")
			if ok {
				fieldPreds[k] = v
			}
		}

		matched := []map[string]any{}
		for _, b := range s.beads {
			if len(wantTypes) > 0 && !wantTypes[b["type"].(string)] {
				continue
			}
			created, _ := time.Parse(time.RFC3339, b["created_at"].(string))
			if !after.IsZero() && created.Before(after) {
				continue
			}
			flds, _ := b["fields"].(map[string]string)
			skip := false
			for k, v := range fieldPreds {
				if flds[k] != v {
					skip = true
					break
				}
			}
			if !skip {
				matched = append(matched, b)
			}
		}
		total := len(matched)

		// Sort: descending created_at unless the caller asked ascending. The
		// exhaustive walk pins "created_at" (ascending).
		desc := q.Get("sort") != "created_at"
		sortByCreated(matched, desc)

		// offset then limit — applied AFTER filtering, like the daemon.
		off, _ := strconv.Atoi(q.Get("offset"))
		if off > len(matched) {
			off = len(matched)
		}
		matched = matched[off:]
		lim, _ := strconv.Atoi(q.Get("limit"))
		if lim <= 0 || lim > s.maxPage {
			lim = s.maxPage
		}
		if len(matched) > lim {
			matched = matched[:lim]
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": matched, "total": total})
	})
}

func sortByCreated(beads []map[string]any, desc bool) {
	// Simple insertion sort — the fixtures are small and this avoids pulling
	// a comparator closure through the map-typed rows.
	for i := 1; i < len(beads); i++ {
		for j := i; j > 0; j-- {
			a, _ := time.Parse(time.RFC3339, beads[j-1]["created_at"].(string))
			b, _ := time.Parse(time.RFC3339, beads[j]["created_at"].(string))
			swap := b.After(a)
			if !desc {
				swap = b.Before(a)
			}
			if !swap {
				break
			}
			beads[j-1], beads[j] = beads[j], beads[j-1]
		}
	}
}

// seedCycles builds n agent beads with the given role, spaced `spacing` apart
// going back from now, each carrying a valid wrap-up.
func seedCycles(prefix, role string, n int, spacing time.Duration) []map[string]any {
	out := make([]map[string]any, 0, n)
	wrapup := `{"tldr":"t","accomplishments":"did the thing"}`
	for i := range n {
		created := time.Now().Add(-time.Duration(i) * spacing)
		out = append(out, map[string]any{
			"id":         fmt.Sprintf("gd-%s-%d", prefix, i),
			"type":       "agent",
			"status":     "closed",
			"created_at": created.UTC().Format(time.RFC3339),
			"fields": map[string]string{
				"agent":  fmt.Sprintf("%s-%d", prefix, i),
				"role":   role,
				"wrapup": wrapup,
			},
		})
	}
	return out
}

// runAuditForTest points the daemon global at a stub and runs the audit,
// returning the decoded JSON report.
func runAuditForTest(t *testing.T, stub *windowStub, flags map[string]string) gardenerAuditReport {
	t.Helper()
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)

	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	old := daemon
	daemon = c
	t.Cleanup(func() { daemon = old })

	oldJSON := jsonOutput
	jsonOutput = true
	t.Cleanup(func() { jsonOutput = oldJSON })

	cmd := gardenerAuditCmd
	cmd.SetContext(context.Background())
	// Reset every flag to its REGISTERED default, then apply only what the
	// test asked for. Reading DefValue rather than hardcoding it is what makes
	// the truncation test meaningful: a test that pinned --limit itself would
	// mask a bad default, and the bad default (500) IS the bug.
	owned := []string{"role", "schedule-id", "since", "limit"}
	for _, k := range owned {
		f := cmd.Flags().Lookup(k)
		if f == nil {
			t.Fatalf("gardener-audit has no --%s flag", k)
		}
		v := f.DefValue
		if set, ok := flags[k]; ok {
			v = set
		}
		if err := cmd.Flags().Set(k, v); err != nil {
			t.Fatalf("set --%s: %v", k, err)
		}
	}
	t.Cleanup(func() {
		for _, k := range owned {
			if f := cmd.Flags().Lookup(k); f != nil {
				_ = cmd.Flags().Set(k, f.DefValue)
			}
		}
	})

	out := captureStdout(t, func() error { return runGardenerAudit(cmd, nil) })

	var report gardenerAuditReport
	if err := json.Unmarshal(out, &report); err != nil {
		t.Fatalf("decode report: %v\noutput: %s", err, out)
	}
	return report
}

// captureAuditStderr redirects os.Stderr for the duration of fn. The audit
// writes its cap warning there, so a test asserting the warning must read it
// off the real file descriptor the command prints to.
func captureAuditStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	defer func() { os.Stderr = orig }()

	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()

	fn()
	_ = w.Close()
	return <-done
}

// TestGardenerAudit_WindowNotTruncatedByLimit is the core regression test for
// ka-0000001500. It seeds MORE in-window cycles than the old default cap (500)
// AND a body of older, out-of-window cycles, then asserts the audit reports
// the true in-window count. Under the old truncate-before-filter code the
// daemon returned the newest 500 agent beads fleet-wide and the window filter
// ran afterwards, so this would report at most 500 — and far less once the
// out-of-window and other-role beads ate into the cap.
func TestGardenerAudit_WindowNotTruncatedByLimit(t *testing.T) {
	const inWindow = 620 // deliberately > the old default limit of 500

	// In-window target-role cycles, one per minute over ~10h (inside 24h).
	beads := seedCycles("target", "gardener-delivery", inWindow, time.Minute)
	// Out-of-window cycles of the SAME role, starting 30h back. These must
	// never be counted.
	for _, b := range seedCycles("old", "gardener-delivery", 400, time.Minute) {
		created, _ := time.Parse(time.RFC3339, b["created_at"].(string))
		b["created_at"] = created.Add(-30 * time.Hour).UTC().Format(time.RFC3339)
		beads = append(beads, b)
	}
	// In-window cycles of a DIFFERENT role — the noise that ate the cap in
	// the real corpus (3962 in-window agent beads for 294 plan-gardener ones).
	beads = append(beads, seedCycles("other", "plan-gardener", 900, time.Minute)...)

	stub := &windowStub{beads: beads, maxPage: 10000}
	report := runAuditForTest(t, stub, map[string]string{
		"role":  "gardener-delivery",
		"since": "24h",
	})

	if report.CycleCount != inWindow {
		t.Errorf("cycle_count = %d, want %d (the TRUE in-window count; a value near 500 means --limit truncated before the window/role filters — bug ka-0000001500)",
			report.CycleCount, inWindow)
	}
	if len(report.Cycles) != inWindow {
		t.Errorf("len(cycles) = %d, want %d", len(report.Cycles), inWindow)
	}
	for _, c := range report.Cycles {
		if c.Role != "gardener-delivery" {
			t.Fatalf("role filter leaked a %q cycle into the report", c.Role)
		}
	}
}

// TestGardenerAudit_SelectorsPushedServerSide asserts the window and the role
// selector actually reach the daemon as query parameters. Client-side
// filtering would still produce the right count on a small fixture, so the
// count assertion above is not sufficient on its own to pin the fix.
func TestGardenerAudit_SelectorsPushedServerSide(t *testing.T) {
	stub := &windowStub{beads: seedCycles("t", "gardener-release", 3, time.Minute), maxPage: 10000}
	_ = runAuditForTest(t, stub, map[string]string{
		"role":        "gardener-release",
		"schedule-id": "gd-sched-9",
		"since":       "6h",
	})

	if len(stub.requests) == 0 {
		t.Fatal("no daemon requests recorded")
	}
	agentReq := stub.requests[0]
	for _, want := range []string{"created_after=", "field=role%3Dgardener-release", "field=schedule_id%3Dgd-sched-9"} {
		if !strings.Contains(agentReq, want) {
			t.Errorf("agent query missing %q — selector is not pushed server-side\ngot: %s", want, agentReq)
		}
	}
	// The decision query must carry the window too.
	var decReq string
	for _, r := range stub.requests {
		if strings.Contains(r, "type=decision") {
			decReq = r
			break
		}
	}
	if decReq == "" {
		t.Fatal("no decision query issued")
	}
	if !strings.Contains(decReq, "created_after=") {
		t.Errorf("decision query missing created_after — it inherits the same truncation bug\ngot: %s", decReq)
	}
}

// TestGardenerAudit_ExplicitLimitStillCapsAndWarns pins the belt-and-suspenders
// half of the fix: an operator who passes an explicit --limit still gets a
// bounded scan, but the truncation is reported on stderr instead of silently
// shrinking the window.
func TestGardenerAudit_ExplicitLimitStillCapsAndWarns(t *testing.T) {
	stub := &windowStub{beads: seedCycles("t", "gardener-intake", 50, time.Minute), maxPage: 10000}

	var report gardenerAuditReport
	stderr := captureAuditStderr(t, func() {
		report = runAuditForTest(t, stub, map[string]string{
			"role":  "gardener-intake",
			"since": "24h",
			"limit": "10",
		})
	})

	if report.CycleCount != 10 {
		t.Errorf("cycle_count = %d, want 10 (an explicit --limit must still cap)", report.CycleCount)
	}
	if !strings.Contains(stderr, "--limit 10 truncated") {
		t.Errorf("expected a truncation warning on stderr, got: %q", stderr)
	}
	if !strings.Contains(stderr, "50") {
		t.Errorf("warning should report the true in-window match count (50), got: %q", stderr)
	}
}

// TestGardenerAuditFieldFilters pins the selector→daemon-predicate mapping,
// including the fleet-wide case that must stay unfiltered.
func TestGardenerAuditFieldFilters(t *testing.T) {
	if f := gardenerAuditFieldFilters("", ""); f != nil {
		t.Errorf("no selectors should yield a nil field map (fleet-wide), got %v", f)
	}
	if f := gardenerAuditFieldFilters("gardener-intake", ""); len(f) != 1 || f["role"] != "gardener-intake" {
		t.Errorf("role-only filter = %v", f)
	}
	if f := gardenerAuditFieldFilters("", "gd-s1"); len(f) != 1 || f["schedule_id"] != "gd-s1" {
		t.Errorf("schedule-only filter = %v", f)
	}
	f := gardenerAuditFieldFilters("gardener-release", "gd-s2")
	if len(f) != 2 || f["role"] != "gardener-release" || f["schedule_id"] != "gd-s2" {
		t.Errorf("combined filter = %v", f)
	}
}

// TestDecisionScanLimit pins the decision cap derivation: uncapped stays
// uncapped, otherwise keep the historical 2x headroom.
func TestDecisionScanLimit(t *testing.T) {
	for _, tc := range []struct{ in, want int }{{0, 0}, {-1, 0}, {10, 20}, {500, 1000}} {
		if got := decisionScanLimit(tc.in); got != tc.want {
			t.Errorf("decisionScanLimit(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
