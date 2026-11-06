package bridge

// End-to-end leg for the GitLab inbound-webhook RECEIVE-PATH counter
// gardenio_gitlab_webhook_deliveries_total{outcome} (backfill bead ka-0000001762;
// runtime gitlab_webhook_metrics.go, ka-0000005632; carve-out doc
// gardenio/docs/security/non-ava-webhook-carveout.md). Wrapped into the e2e gate by
// gardenio/tests/e2e/scripts/test-gitlab-webhook-deadman.sh as a SECOND run_leg
// ("served-deliveries") — same module, distinct TestGitLabWebhookDeliveriesE2E
// prefix so it is targeted independently of the deadman-verdict leg.
//
// ── What this adds OVER everything that already runs ──────────────────────────
//
// Two coverage locations already touch the deliveries counter, and NEITHER pins
// the two properties an operator's page-triage path actually depends on:
//
//   • gitlab_webhook_metrics_test.go drives the handler and reads the counter with
//     testutil.ToFloat64 over the in-process registry. That proves the VERDICT of
//     recordGitLabWebhookOutcome — but ToFloat64 reads the registry directly and
//     would stay green even if the CounterVec never marshalled to the scrape
//     format. It cannot see the SERVED text exposition.
//   • gitlab_webhook_deadman_e2e_test.go scrapes the served exposition, but only
//     for gardenio_gitlab_webhook_dark / _evaluations_total — a DISJOINT series set.
//     grep 'deliveries_total' over the whole tests/ tree returns zero.
//
// So the documented triage surface for the paging alert
// (helm/gardenio/templates/gitlab-bridge/webhook-deadman-alert.yaml:100 tells the
// on-call to check gardenio_gitlab_webhook_deliveries_total{outcome}) has NOTHING
// asserting it reaches the wire with its full outcome vocabulary — a rename of the
// series or a label value would leave the alert firing but point the on-call at a
// series that does not exist. This leg closes that.
//
// It also pins the ONE load-bearing security property of the outcome→clock mapping
// (recordGitLabWebhookOutcome): token_failed must NOT stamp the receive-liveness
// clock. If it ever did, a WRONG-SECRET hook (every delivery token_failed) would
// read as healthy and the deadman would never page — the exact scenario the
// carve-out's deadman exists to catch. The unit test asserts this over the atomic;
// here we prove it end-to-end through the SERVED gardenio_gitlab_webhook_dark gauge:
// a token_failed stream leaves the clock stale, so the next monitor evaluate()
// folds the served dark gauge to 1 — with a real-authenticated-delivery control
// proving the SAME served endpoint reads 0 when the clock IS stamped (crew-e2e
// anti-vacuity advice ka-0000001521 / gd-4E9o92p0DY: the ==1 is meaningless without
// a proven 0 on the same endpoint).

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// servedDeliveriesValue extracts the value of a single-label
// gardenio_gitlab_webhook_deliveries_total{outcome="<outcome>"} sample from a
// Prometheus text exposition. found=false when the series line is absent (a
// never-marshalled label value). The counter carries exactly one label, so the
// rendered form is deterministic — mirrors servedGaugeValue in the sibling e2e
// file but for the labeled counter form.
func servedDeliveriesValue(text, outcome string) (value float64, found bool) {
	prefix := `gardenio_gitlab_webhook_deliveries_total{outcome="` + outcome + `"} `
	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimSpace(line)
		rest, ok := strings.CutPrefix(line, prefix)
		if !ok {
			continue
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(rest), 64)
		if err != nil {
			return 0, false
		}
		return f, true
	}
	return 0, false
}

// TestGitLabWebhookDeliveriesE2E_ServedAllOutcomesPresent proves the full outcome
// label vocabulary the paging alert's triage instruction names
// (webhook-deadman-alert.yaml:100) reaches the SERVED /metrics exposition. If the
// series or any of its five outcome= label values is renamed, this reddens — the
// only thing that couples the shipped triage instruction to the emitted series.
//
// Asserted by LABEL-SET MEMBERSHIP (all five outcome= values appear on the wire),
// never by an absolute count — the counter is cumulative and order-dependent
// across the other tests in this package.
func TestGitLabWebhookDeliveriesE2E_ServedAllOutcomesPresent(t *testing.T) {
	resetGitLabWebhookDeadmanMetrics()
	// InitGitLabWebhookMetrics touches every outcome series to zero — the same
	// startup baseline the bridge emits (gitlab-bridge/main.go) — so all five must
	// be present on the wire from a cold pod, before any delivery arrives.
	InitGitLabWebhookMetrics()

	served := servedGitLabDeadmanMetrics(t)

	// The five terminal outcomes the alert's triage instruction charts. These are
	// the contract label values; a rename of any drifts the on-call's diagnosis
	// path off a series the bridge no longer emits.
	for _, outcome := range []string{
		gitlabWebhookOutcomeProcessed,
		gitlabWebhookOutcomeIgnored,
		gitlabWebhookOutcomeTokenFailed,
		gitlabWebhookOutcomeBadRequest,
		gitlabWebhookOutcomeError,
	} {
		if _, found := servedDeliveriesValue(served, outcome); !found {
			t.Errorf("gardenio_gitlab_webhook_deliveries_total{outcome=%q} absent from the served exposition — the alert triage instruction (webhook-deadman-alert.yaml) points the on-call at a label value the bridge never emits:\n%s", outcome, served)
		}
	}
}

// TestGitLabWebhookDeliveriesE2E_TokenFailedDoesNotStampLiveness is the
// load-bearing security leg. It proves, end-to-end over the SERVED exposition:
//
//  1. a token_failed delivery (wrong X-Gitlab-Token, the wrong-secret-in-console
//     misconfiguration) increments deliveries_total{outcome="token_failed"} by
//     exactly +1 across the single POST — a still-arriving-but-rejected stream is
//     VISIBLE, not silent (asserted as a RELATIVE DELTA, never an absolute count);
//     and
//  2. that same token_failed delivery does NOT stamp the receive-liveness clock,
//     so the next monitor evaluate() (upstream activity present) folds the SERVED
//     gardenio_gitlab_webhook_dark gauge to 1 — the wrong-secret hook PAGES rather
//     than reading as healthy.
//
// (2) is guarded against vacuity by a real-authenticated-delivery control arm
// proving the SAME served endpoint reads dark 0 when the clock IS stamped: the
// ONLY difference between the two arms is authenticated-vs-token_failed, and it
// flips the served gauge. Mutation check (proven by hand, MR PROOF block): adding
// token_failed to the clock-stamping switch in recordGitLabWebhookOutcome makes
// the treatment arm read dark 0 and this test red.
func TestGitLabWebhookDeliveriesE2E_TokenFailedDoesNotStampLiveness(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	const secret = "e2e-deliveries-secret"

	// ── ANTI-VACUITY control (HEALTHY): a real AUTHENTICATED delivery through the
	// ACTUAL handler stamps the receive-liveness clock, so the next evaluate()
	// reads healthy and the SERVED dark gauge is 0. Without this, the dark==1 in the
	// treatment arm could be a stuck-at-1 artifact rather than a real flip caused
	// specifically by token_failed NOT stamping the clock.
	resetGitLabWebhookDeadmanMetrics()
	lastGitLabWebhookReceivedNano.Store(now.Add(-3 * time.Hour).UnixNano()) // start stale

	authHandler := GitLabWebhookHandlerWithConfig(GitLabWebhookConfig{
		WebhookSecret: secret,
		Logger:        slog.New(slog.NewTextHandler(nopWriter{}, nil)),
	})
	// An unrecognized object_kind is authenticated + decoded → outcome "ignored",
	// which stamps the liveness clock with zero daemon/GitLab dependency — the
	// minimal real "the hook is alive and correctly secreted" signal.
	authReq := httptest.NewRequest(http.MethodPost, "/webhook",
		strings.NewReader(`{"object_kind":"deploy"}`))
	authReq.Header.Set("X-Gitlab-Token", secret)
	authRec := httptest.NewRecorder()
	authHandler.ServeHTTP(authRec, authReq)
	if authRec.Code != http.StatusOK {
		t.Fatalf("authenticated delivery status = %d, want 200 (body: %s)", authRec.Code, authRec.Body.String())
	}
	if lastGitLabWebhookReceivedNano.Load() == 0 {
		t.Fatal("a real authenticated delivery must stamp the receive-liveness clock (control-arm precondition)")
	}
	// Pin the just-stamped clock to within-window (recordGitLabWebhookOutcome stamped
	// time.Now(); the fixed-clock monitor needs it deterministic) and evaluate with
	// upstream activity present, proving the 0 is the healthy early-out, not a
	// quiet-upstream 0.
	lastGitLabWebhookReceivedNano.Store(now.Add(-1 * time.Minute).UnixNano())
	e2eGitLabMonitor(&webhookFakeGroupMRLister{mrs: makeMRs(999)}, 2*time.Hour, now).evaluate(context.Background())

	healthyText := servedGitLabDeadmanMetrics(t)
	if v, found := servedGaugeValue(healthyText, "gardenio_gitlab_webhook_dark"); !found || v != 0 {
		t.Fatalf("served dark gauge (healthy control) = %v found=%v, want 0 — the anti-vacuity control failed; the served exposition must read 0 when the clock is stamped", v, found)
	}

	// ── TREATMENT: token_failed stream, clock left stale (unstamped). ────────────
	resetGitLabWebhookDeadmanMetrics()
	InitGitLabWebhookMetrics()                                              // give token_failed a real 0 baseline line on the wire
	lastGitLabWebhookReceivedNano.Store(now.Add(-3 * time.Hour).UnixNano()) // stale, and it must STAY stale

	// Baseline the token_failed counter from the SERVED exposition BEFORE the POST,
	// so the assertion below is a relative delta across a single delivery.
	before, found := servedDeliveriesValue(servedGitLabDeadmanMetrics(t), gitlabWebhookOutcomeTokenFailed)
	if !found {
		t.Fatalf("token_failed baseline line absent after Init — cannot measure a delta")
	}

	failHandler := GitLabWebhookHandlerWithConfig(GitLabWebhookConfig{
		WebhookSecret: secret,
		Logger:        slog.New(slog.NewTextHandler(nopWriter{}, nil)),
	})
	// Wrong X-Gitlab-Token — the wrong-secret-in-console scenario. Rejected at the
	// perimeter (403) → recorded as token_failed, and must NOT stamp liveness.
	failReq := httptest.NewRequest(http.MethodPost, "/webhook",
		strings.NewReader(`{"object_kind":"merge_request"}`))
	failReq.Header.Set("X-Gitlab-Token", "the-wrong-secret")
	failRec := httptest.NewRecorder()
	failHandler.ServeHTTP(failRec, failReq)
	if failRec.Code != http.StatusForbidden {
		t.Fatalf("wrong-token delivery status = %d, want 403", failRec.Code)
	}

	// (1) VISIBILITY: the rejected delivery is on the wire as a +1 delta.
	after, found := servedDeliveriesValue(servedGitLabDeadmanMetrics(t), gitlabWebhookOutcomeTokenFailed)
	if !found {
		t.Fatalf("token_failed line absent from the served exposition after a wrong-token delivery")
	}
	if delta := after - before; delta != 1 {
		t.Fatalf("served deliveries_total{outcome=token_failed} delta = %v (%v→%v), want +1 — a rejected delivery must be visible on the wire, not silent", delta, before, after)
	}

	// (2) LOAD-BEARING: token_failed did NOT stamp the liveness clock, so with the
	// clock still stale and upstream activity present the monitor folds the SERVED
	// dark gauge to 1 — the wrong-secret hook pages. If token_failed HAD stamped the
	// clock, evaluate() would read healthy and this would be 0 (the mutation check).
	e2eGitLabMonitor(&webhookFakeGroupMRLister{mrs: makeMRs(5)}, 2*time.Hour, now).evaluate(context.Background())
	darkText := servedGitLabDeadmanMetrics(t)
	if v, found := servedGaugeValue(darkText, "gardenio_gitlab_webhook_dark"); !found || v != 1 {
		t.Fatalf("served dark gauge (token_failed-only) = %v found=%v, want 1 — a wrong-secret stream must page, which means token_failed must NOT have stamped the receive-liveness clock", v, found)
	}
}
