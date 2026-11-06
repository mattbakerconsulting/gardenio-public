package deathreport

import (
	"strings"
	"testing"

	gardeniometrics "gardenio/controller/internal/metrics"
)

func TestClassify_InvoluntaryFamiliesMatchMetricEmitter(t *testing.T) {
	// Every involuntary close_reason must classify with Involuntary=true and a
	// MetricFamily that exactly matches gardeniometrics.InvoluntaryDeathFamily —
	// the report can never disagree with the metric about what counts as a death.
	cases := []struct {
		reason     string
		wantFamily DeathFamily
		wantMetric string
	}{
		{"stop_gate_cooldown_involuntary: claude exited mid-work without gb done", FamilyStopGateCooldown, gardeniometrics.FamilyStopGateCooldown},
		{"stop_gate_escape_hatch_involuntary: ...", FamilyStopGateEscapeHatch, gardeniometrics.FamilyStopGateEscapeHatch},
		{"stop_gate_rate_limited_involuntary: ...", FamilyStopGateRateLimited, gardeniometrics.FamilyStopGateRateLimited},
		{"context_cancelled_involuntary: reopened gd-x", FamilyContextCancelled, gardeniometrics.FamilyContextCancelled},
		{"context_cancelled_unclaimed", FamilyContextCancelledUnclaimed, gardeniometrics.FamilyContextCancelledUnclaimed},
		{"context_cancelled_claim_lookup_failed: pod terminated and the claimed-task lookup could not be verified", FamilyContextCancelledClaimLookupFailed, gardeniometrics.FamilyContextCancelledClaimLookupFailed},
	}
	for _, c := range cases {
		got := Classify(c.reason)
		if !got.Involuntary {
			t.Errorf("Classify(%q).Involuntary = false, want true", c.reason)
		}
		if got.Family != c.wantFamily {
			t.Errorf("Classify(%q).Family = %q, want %q", c.reason, got.Family, c.wantFamily)
		}
		if got.MetricFamily != c.wantMetric {
			t.Errorf("Classify(%q).MetricFamily = %q, want %q", c.reason, got.MetricFamily, c.wantMetric)
		}
		// Cross-check directly against the shared predicate.
		mf, ok := gardeniometrics.InvoluntaryDeathFamily(c.reason)
		if !ok || mf != c.wantMetric {
			t.Errorf("metric emitter disagrees: InvoluntaryDeathFamily(%q) = (%q, %t)", c.reason, mf, ok)
		}
	}
}

func TestClassify_NonInvoluntaryFamilies(t *testing.T) {
	cases := []struct {
		reason     string
		wantFamily DeathFamily
	}{
		{"idle_ttl_expired", FamilyIdleOut},
		{"reaper: stranded open run-instance (never claimed)", FamilyReaper},
		{"stale: no activity for 30m0s", FamilyReaper},
		{"timeout after 10m", FamilyTimeout},
		{"spawn expired before claim", FamilyTimeout},
		{"max_restarts_reached", FamilyMaxRestarts},
		{"not_logged_in", FamilyNotLoggedIn},
		{"stop", FamilyGracefulStop},
		{"stop_requested", FamilyGracefulStop},
		{"wrapup_complete", FamilyGracefulStop},
		{"context_cancelled", FamilyContextCancelled},                        // plain (graceful teardown), NOT involuntary
		{"stop_gate_cooldown: claude exited without gb done", FamilyIdleOut}, // no _involuntary marker → zero-work idle-out
		{"", FamilyUnknown},
		{"some_novel_reason_xyz", FamilyUnknown},
	}
	for _, c := range cases {
		got := Classify(c.reason)
		if got.Involuntary {
			t.Errorf("Classify(%q).Involuntary = true, want false", c.reason)
		}
		if got.Family != c.wantFamily {
			t.Errorf("Classify(%q).Family = %q, want %q", c.reason, got.Family, c.wantFamily)
		}
		if got.Summary == "" {
			t.Errorf("Classify(%q).Summary is empty", c.reason)
		}
	}
}

// TestClassify_OOMAndEvict verifies the broadened taxonomy classifies
// pod-level kill reasons that the metric emitter does not model.
func TestClassify_OOMAndEvict(t *testing.T) {
	if got := Classify("OOMKilled").Family; got != FamilyOOMKilled {
		t.Errorf("OOMKilled → %q, want %q", got, FamilyOOMKilled)
	}
	if got := Classify("pod evicted: node-pressure").Family; got != FamilyPodEvicted {
		t.Errorf("evict → %q, want %q", got, FamilyPodEvicted)
	}
	// An `_involuntary` reason that mentions oom still classifies as OOM under
	// the FamilyOther fallthrough while keeping Involuntary=true.
	c := Classify("stop_gate_oom_involuntary: weird")
	if !c.Involuntary {
		t.Errorf("`_involuntary` reason should be Involuntary=true")
	}
	if c.Family != FamilyOOMKilled {
		t.Errorf("oom `_involuntary` FamilyOther fallthrough → %q, want %q", c.Family, FamilyOOMKilled)
	}
}

// TestClassify_UpstreamAPIError_Involuntary pins the outage-driven death family
// (ka-0000000202): the close_reason init_agent_loop.go stamps when a teardown
// fires while a Claude-upstream outage is active must classify as
// upstream_api_error, Involuntary=true, and carry the matching MetricFamily so
// the report and gardenio_agent_involuntary_deaths_total agree.
func TestClassify_UpstreamAPIError_Involuntary(t *testing.T) {
	cases := []string{
		"upstream_api_error_involuntary: pod terminated during a sustained Claude upstream outage (server_error)",
		"upstream_api_error_involuntary: pod terminated during a sustained Claude upstream outage (no_internet); reopened gd-abc123",
		"upstream_api_error_involuntary: restart loop exhausted during a sustained Claude upstream outage (server_error)",
	}
	for _, reason := range cases {
		got := Classify(reason)
		if got.Family != FamilyUpstreamAPIError {
			t.Errorf("Classify(%q).Family = %q, want %q", reason, got.Family, FamilyUpstreamAPIError)
		}
		if !got.Involuntary {
			t.Errorf("Classify(%q).Involuntary = false, want true", reason)
		}
		if got.MetricFamily != gardeniometrics.FamilyUpstreamAPIError {
			t.Errorf("Classify(%q).MetricFamily = %q, want %q", reason, got.MetricFamily, gardeniometrics.FamilyUpstreamAPIError)
		}
		if got.Summary == "" {
			t.Errorf("Classify(%q).Summary is empty", reason)
		}
		// The report must never disagree with the shared metric predicate.
		mf, ok := gardeniometrics.InvoluntaryDeathFamily(reason)
		if !ok || mf != gardeniometrics.FamilyUpstreamAPIError {
			t.Errorf("metric emitter disagrees: InvoluntaryDeathFamily(%q) = (%q, %t)", reason, mf, ok)
		}
	}
}

// TestClassify_UpstreamAPIError_RawShapes verifies every upstream-error
// substring shape (mirrored from coop's ErrorCategory::ServerError set) routes
// to upstream_api_error even on the NON-involuntary path — a close_reason that
// carries the outage signature without the `_involuntary` marker must still be
// attributed to the outage, not left as unknown. These are the shapes coop's
// classify_error_detail keys on (coop/crates/cli/src/driver/error_category.rs).
func TestClassify_UpstreamAPIError_RawShapes(t *testing.T) {
	shapes := []string{
		"api_error",
		"overloaded_error",
		"overloaded",
		"internal_error",
		"Internal server error",
		"server_error",
		"API Error: 500 Internal server error",
		"API Error: 502 Bad Gateway",
		"API Error: 503 Service Unavailable",
		"API Error: 529",
		"API Error: terminated",
		"socket connection was closed",
		"connection was closed unexpectedly",
		"API Error: Connection error.",
		"API Error: Connection closed mid-response.",
		"The response above may be incomplete.",
	}
	for _, s := range shapes {
		got := Classify(s)
		if got.Family != FamilyUpstreamAPIError {
			t.Errorf("Classify(%q).Family = %q, want %q", s, got.Family, FamilyUpstreamAPIError)
		}
		// These raw shapes carry no `_involuntary` marker, so they are NOT
		// counted as involuntary deaths — the family is a post-mortem label, not
		// a metric increment. (The teardown path stamps the `_involuntary` form
		// for the counted case; see TestClassify_UpstreamAPIError_Involuntary.)
		if got.Involuntary {
			t.Errorf("Classify(%q).Involuntary = true, want false (no _involuntary marker)", s)
		}
		if got.Summary == "" {
			t.Errorf("Classify(%q).Summary is empty", s)
		}
	}
}

// TestClassify_UpstreamPrecedence is the ordering-hazard guard (ka-0000000202):
// classifyByKeyword is substring matching, so an outage-shaped close_reason that
// ALSO contains a generic proximate-cause substring (max_restarts, timeout,
// reaper, …) must resolve to upstream_api_error — the specific attribution has
// to win over the incidental one, or the change is a silent no-op and the
// misattribution continues.
func TestClassify_UpstreamPrecedence(t *testing.T) {
	cases := []struct {
		name   string
		reason string
	}{
		// Contains "restarts" — must NOT fall to FamilyMaxRestarts.
		{"upstream beats max_restarts", "server_error: gave up after max_restarts_reached"},
		// Contains "timeout" — must NOT fall to FamilyTimeout.
		{"upstream beats timeout", "api_error: request timeout during overloaded upstream"},
		// Contains "expired" — must NOT fall to FamilyTimeout.
		{"upstream beats expired", "503 service unavailable; session expired"},
		// Contains "reaper"/"stale" — must NOT fall to FamilyReaper.
		{"upstream beats reaper", "reaper closed an agent stuck on api_error 529 overloaded"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Classify(c.reason).Family; got != FamilyUpstreamAPIError {
				t.Errorf("Classify(%q).Family = %q, want %q (precedence)", c.reason, got, FamilyUpstreamAPIError)
			}
		})
	}
}

// TestClassify_NoUpstreamFalsePositive guards the other direction: a reason with
// NO upstream signature must keep its own family. In particular a bare
// max_restarts / timeout / reaper reason (the buckets outage deaths used to be
// misattributed INTO) must be untouched by the new arm.
func TestClassify_NoUpstreamFalsePositive(t *testing.T) {
	cases := []struct {
		reason string
		want   DeathFamily
	}{
		{"max_restarts_reached", FamilyMaxRestarts},
		{"timeout after 10m", FamilyTimeout},
		{"spawn expired before claim", FamilyTimeout},
		{"reaper: stranded open run-instance (never claimed)", FamilyReaper},
		{"stale: no activity for 30m0s", FamilyReaper},
		{"OOMKilled", FamilyOOMKilled},
		{"idle_ttl_expired", FamilyIdleOut},
		{"context_cancelled", FamilyContextCancelled},
	}
	for _, c := range cases {
		if got := Classify(c.reason).Family; got != c.want {
			t.Errorf("Classify(%q).Family = %q, want %q (no false upstream match)", c.reason, got, c.want)
		}
	}
}

// TestClassify_AllExistingFamiliesUnchanged is the regression gate: every one of
// the 14 pre-existing death families must still classify to exactly the family
// it did before the upstream_api_error arm was added. If the new arm's placement
// ever shadows an existing family, this fails loudly.
func TestClassify_AllExistingFamiliesUnchanged(t *testing.T) {
	cases := []struct {
		reason string
		want   DeathFamily
	}{
		{"stop_gate_cooldown_involuntary: reopened gd-x", FamilyStopGateCooldown},
		{"stop_gate_escape_hatch_involuntary: x", FamilyStopGateEscapeHatch},
		{"stop_gate_rate_limited_involuntary: x", FamilyStopGateRateLimited},
		{"context_cancelled_involuntary: pod terminated mid-work before gb done", FamilyContextCancelled},
		{"context_cancelled_unclaimed: pod terminated with tool calls but no claimed issue", FamilyContextCancelledUnclaimed},
		{"OOMKilled", FamilyOOMKilled},
		{"pod evicted: node-pressure", FamilyPodEvicted},
		{"reaper: stranded open run-instance", FamilyReaper},
		{"timeout after 10m", FamilyTimeout},
		{"max_restarts_reached", FamilyMaxRestarts},
		{"not_logged_in", FamilyNotLoggedIn},
		{"idle_ttl_expired", FamilyIdleOut},
		{"stop", FamilyGracefulStop},
		{"some_novel_reason_xyz", FamilyUnknown},
	}
	if len(cases) != 14 {
		t.Fatalf("expected 14 pre-existing families exercised, got %d", len(cases))
	}
	for _, c := range cases {
		if got := Classify(c.reason).Family; got != c.want {
			t.Errorf("regression: Classify(%q).Family = %q, want %q", c.reason, got, c.want)
		}
	}
}

// --- ClassifyWithFields: credential-failure evidence overrides the generic guess (ka-0000002985 DEFECT 1) ---

func TestClassifyWithFields_AuthOverridesContextCancelled(t *testing.T) {
	// The incident shape: closed context_cancelled_involuntary, but the fields
	// carry the auth-failure signature. The family must flip to credential_failure
	// and the summary must name the credential — not assert a Spot reclaim.
	fields := map[string]string{
		"error_category":      "unauthorized",
		"error_detail":        "authentication_failed",
		"total_output_tokens": "0",
		"total_request_count": "15",
		"total_cost_usd":      "0",
	}
	got := ClassifyWithFields("context_cancelled_involuntary: pod terminated mid-work before gb done", fields)
	if got.Family != FamilyCredentialFailure {
		t.Errorf("Family = %q, want %q", got.Family, FamilyCredentialFailure)
	}
	// The involuntary-death metric verdict is still driven by the close_reason, so
	// the death is not lost from the counted series.
	if !got.Involuntary || got.MetricFamily != gardeniometrics.FamilyContextCancelled {
		t.Errorf("metric verdict changed: Involuntary=%t MetricFamily=%q (want true / %q)",
			got.Involuntary, got.MetricFamily, gardeniometrics.FamilyContextCancelled)
	}
	if !strings.Contains(strings.ToLower(got.Summary), "credential") {
		t.Errorf("summary does not name the credential failure: %q", got.Summary)
	}
	for _, unwanted := range []string{"Spot", "node drain"} {
		if strings.Contains(got.Summary, unwanted) {
			t.Errorf("summary still asserts %q: %q", unwanted, got.Summary)
		}
	}
}

func TestClassifyWithFields_NoOverrideForSpecificFamily(t *testing.T) {
	// A specific, already-evidenced family (upstream_api_error) must NOT be
	// overridden by an auth signature — it carries its own precise cause.
	fields := map[string]string{"error_category": "unauthorized"}
	got := ClassifyWithFields("upstream_api_error_involuntary: 529 after 8 restarts", fields)
	if got.Family != FamilyUpstreamAPIError {
		t.Errorf("Family = %q, want %q (specific family must not be overridden)", got.Family, FamilyUpstreamAPIError)
	}
}

func TestClassifyWithFields_NoSignatureStaysHonest(t *testing.T) {
	got := ClassifyWithFields("context_cancelled_involuntary: pod terminated mid-work before gb done", nil)
	if got.Family != FamilyContextCancelled {
		t.Errorf("Family = %q, want %q", got.Family, FamilyContextCancelled)
	}
	if strings.Contains(got.Summary, "Spot") {
		t.Errorf("summary must not assert Spot reclaim without evidence: %q", got.Summary)
	}
	if !strings.Contains(got.Summary, "not determined") {
		t.Errorf("summary should say the interruption source was not determined: %q", got.Summary)
	}
}

func TestAuthFailureCause(t *testing.T) {
	cases := []struct {
		name    string
		fields  map[string]string
		wantSub string // "" means expect empty result
	}{
		{"nil", nil, ""},
		{"empty", map[string]string{}, ""},
		{
			"explicit category only",
			map[string]string{"error_category": "unauthorized"},
			"credential was rejected",
		},
		{
			"explicit detail only",
			map[string]string{"error_detail": "authentication_failed"},
			"credential was rejected",
		},
		{
			"explicit + never-authenticated corroboration",
			map[string]string{
				"error_category": "unauthorized", "error_detail": "authentication_failed",
				"total_request_count": "15", "total_output_tokens": "0", "total_cost_usd": "0",
			},
			"completed no successful turn",
		},
		{
			"never-authenticated token shape alone (no explicit category)",
			map[string]string{"total_request_count": "9", "total_output_tokens": "0", "total_cost_usd": "0"},
			"completed no successful turn",
		},
		{
			"productive session is NOT flagged",
			map[string]string{"total_request_count": "40", "total_output_tokens": "12000", "total_cost_usd": "3.20"},
			"",
		},
		{
			"absent token accounting is NOT the zero shape",
			map[string]string{"total_request_count": "", "total_output_tokens": "", "total_cost_usd": ""},
			"",
		},
		{
			"zero requests is NOT never-authenticated (nothing was tried)",
			map[string]string{"total_request_count": "0", "total_output_tokens": "0", "total_cost_usd": "0"},
			"",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := AuthFailureCause(c.fields)
			if c.wantSub == "" {
				if got != "" {
					t.Errorf("AuthFailureCause = %q, want empty", got)
				}
				return
			}
			if !strings.Contains(got, c.wantSub) {
				t.Errorf("AuthFailureCause = %q, want substring %q", got, c.wantSub)
			}
			if strings.Contains(got, "\n") {
				t.Errorf("cause must be one line: %q", got)
			}
		})
	}
}
