// Claude status-page poller — leg B of the Claude-outage detector
// (task ka-0000003022, epic ka-0000000487).
//
// Motivating incident: on 2026-07-29 the Claude API ran a ~2h partial outage
// ("Elevated errors across all models", status.claude.com incident
// q2kg8n613kr3, impact critical) and nothing in gardenio noticed. Agents ate
// upstream 5xx the whole time while #gardenio-alerts stayed silent. This poller
// is the corroborating half of the detector: it answers "does the provider
// admit it?" so the alert on leg A (agents actually failing —
// gardenio_agent_upstream_error_active) can say whether the fault is ours or
// theirs without the oncall leaving Slack.
//
// Deliberately NOT a pager on its own. A status-page blip that never touches our
// traffic (claude.ai down while api.anthropic.com is fine) must not wake anyone;
// see the ClaudeStatusPageOutage severity note in the alert rules.
package bridge

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// claudeStatusDefaultURL is the Statuspage summary endpoint: page indicator +
	// per-component status + open incidents in ONE call (status.json carries only
	// the indicator).
	//
	// Targets status.claude.com directly. status.anthropic.com now 302s here
	// (verified 2026-07-29: `location: https://status.claude.com/api/v2/...`), so
	// a poller pinned to the old host with redirects disabled would receive a 302
	// body, fail to parse, and never fire — the detector would be silently blind
	// during exactly the outage it exists to catch. Go's default http.Client
	// follows redirects, so either host works; the new one is used to avoid
	// depending on the redirect surviving.
	claudeStatusDefaultURL = "https://status.claude.com/api/v2/summary.json"

	// claudeStatusDefaultInterval is the poll cadence. 60s against a static,
	// CDN-fronted JSON endpoint is well within polite use and keeps detection
	// latency far below the alert's `for:` window.
	claudeStatusDefaultInterval = 60 * time.Second

	// claudeStatusDefaultTimeout bounds one fetch. Short: a hung status page must
	// not wedge the poll loop, and a missed sample is cheap (the next tick is 60s
	// away) while the deadman gauge makes a sustained failure visible.
	claudeStatusDefaultTimeout = 10 * time.Second

	// claudeStatusUserAgent identifies gardenio to Anthropic's infrastructure. A
	// polling client with no meaningful UA is indistinguishable from a scraper.
	claudeStatusUserAgent = "gardenio-claude-status-poller/1.0 (+https://github.com/acme/gardenio)"

	// claudeStatusMaxBody caps the response read. The real payload is a few KB;
	// this bounds memory if the endpoint ever returns something unexpected.
	claudeStatusMaxBody = 1 << 20 // 1 MiB
)

// claudeComponentSlugs maps the Statuspage component NAME to a stable gardenio
// slug used as the `component` metric label.
//
// Keyed on name, not on the Statuspage component id: those ids (`k8w3r06qmzrp`)
// are opaque, meaningless in an alert annotation, and Anthropic can re-issue
// them. An allowlist also bounds cardinality — a newly-added upstream component
// is counted under the unknown bucket rather than silently minting a new series.
var claudeComponentSlugs = map[string]string{
	"claude api (api.anthropic.com)":       "claude_api",
	"claude code":                          "claude_code",
	"claude.ai":                            "claude_ai",
	"claude console (platform.claude.com)": "claude_console",
	"claude cowork":                        "claude_cowork",
	"claude for government":                "claude_gov",
}

// claudeStatusSummary is the subset of /api/v2/summary.json the poller reads.
type claudeStatusSummary struct {
	Status struct {
		Indicator   string `json:"indicator"`
		Description string `json:"description"`
	} `json:"status"`
	Components []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	} `json:"components"`
	Incidents []struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Status string `json:"status"`
		Impact string `json:"impact"`
	} `json:"incidents"`
}

// ClaudeStatusVerdict is one poll's assessment, kept in memory so an alert
// annotation (or a Slack message) can quote the incident WITHOUT the unbounded
// incident id/title ever becoming a metric label.
type ClaudeStatusVerdict struct {
	// Indicator is the raw page indicator ("none"/"minor"/"major"/"critical").
	Indicator string
	// Description is the page's human summary, e.g. "Minor Service Outage".
	Description string
	// Components maps gardenio component slug -> raw upstream status string.
	Components map[string]string
	// Incidents holds the unresolved incidents, most impactful first.
	Incidents []ClaudeStatusIncident
	// CheckedAt is when this verdict was produced.
	CheckedAt time.Time
}

// ClaudeStatusIncident is one unresolved status-page incident.
type ClaudeStatusIncident struct {
	ID     string
	Name   string
	Status string
	Impact string
}

// Suspect reports whether the status page corroborates an outage affecting the
// path our agents actually use. True when the API or Claude Code component is at
// partial_outage or worse, or when an unresolved incident is major/critical.
//
// This is the predicate an alert annotation uses to say "Claude is suspect" —
// note it intentionally ignores claude.ai / Console / Cowork degradation, which
// does not touch agent traffic.
func (v ClaudeStatusVerdict) Suspect() bool {
	for _, slug := range []string{"claude_api", "claude_code"} {
		if claudeComponentSeverity(v.Components[slug]) >= claudeComponentPartialOutage {
			return true
		}
	}
	for _, inc := range v.Incidents {
		if inc.Impact == "major" || inc.Impact == "critical" {
			return true
		}
	}
	return false
}

// Summary renders a one-line description for an alert annotation / Slack
// message, e.g.:
//
//	Claude status: Minor Service Outage (claude_api=partial_outage,
//	claude_code=partial_outage); incident q2kg8n613kr3 (critical): Elevated
//	errors across all models
//
// Returns "" for a zero verdict so a caller can omit the annotation entirely
// rather than rendering "Claude status: unknown" as if it were a reading.
func (v ClaudeStatusVerdict) Summary() string {
	if v.CheckedAt.IsZero() {
		return ""
	}
	var b strings.Builder
	b.WriteString("Claude status: ")
	if v.Description != "" {
		b.WriteString(v.Description)
	} else {
		b.WriteString(v.Indicator)
	}
	var degraded []string
	for _, slug := range []string{"claude_api", "claude_code", "claude_ai"} {
		if s := v.Components[slug]; s != "" && s != "operational" {
			degraded = append(degraded, slug+"="+s)
		}
	}
	if len(degraded) > 0 {
		b.WriteString(" (" + strings.Join(degraded, ", ") + ")")
	}
	if len(v.Incidents) > 0 {
		inc := v.Incidents[0]
		fmt.Fprintf(&b, "; incident %s (%s): %s", inc.ID, inc.Impact, inc.Name)
	}
	return b.String()
}

// ClaudeStatusConfig configures the poller.
type ClaudeStatusConfig struct {
	// URL overrides the primary status endpoint. Empty uses claudeStatusDefaultURL.
	URL string
	// FallbackURLs is an ordered list of alternate status endpoints tried, in
	// order, ONLY when the primary URL's fetch fails. The first one that fetches
	// and parses to a usable reading wins for that poll; if all fail the poll
	// fails and the deadman/error-reason metrics reflect the PRIMARY failure.
	//
	// This exists because leg B has now gone dark twice (ka-0000008266 registry
	// bug; ka-0000007040 provider TLS-cert misprovisioning) and "poller has no
	// fallback" is the durable single-point-of-failure. As of 2026-08-14 no
	// alternate host serves summary.json unauthenticated with a valid cert — the
	// canonical Statuspage slug (tymt9n04zgry.statuspage.io) was deactivated when
	// the page migrated to the custom domain and returns 401 "page is inactive" —
	// so the DEFAULT list is empty. The mechanism ships ready so the moment
	// Anthropic exposes any valid-cert endpoint an operator wires it in with zero
	// redeploy (GARDENIO_CLAUDE_STATUS_FALLBACK_URLS).
	FallbackURLs []string
	// Interval is the poll cadence. Default 60s.
	Interval time.Duration
	// Client overrides the HTTP client (tests inject a stub). Must follow
	// redirects if URL points at status.anthropic.com.
	Client *http.Client
	// Logger; defaults to slog.Default().
	Logger *slog.Logger
	// Now overrides the clock for tests.
	Now func() time.Time
	// Metrics is the Prometheus series set the poller publishes to. The
	// controller passes the instance it registered on its promRegistry (the one
	// :9092 serves). Nil builds a fresh, UNREGISTERED set — fine for a test that
	// only reads gauge values, but a caller that wants the series scraped must
	// register them (see ClaudeStatusMetrics.Register).
	Metrics *ClaudeStatusMetrics
}

// ClaudeStatusPoller polls the Claude status page and publishes the reading as
// Prometheus gauges plus an in-memory verdict.
type ClaudeStatusPoller struct {
	url       string
	fallbacks []string
	interval  time.Duration
	client    *http.Client
	logger    *slog.Logger
	now       func() time.Time
	metrics   *ClaudeStatusMetrics

	mu      sync.RWMutex
	verdict ClaudeStatusVerdict
}

// NewClaudeStatusPoller builds a poller with defaults applied.
func NewClaudeStatusPoller(cfg ClaudeStatusConfig) *ClaudeStatusPoller {
	if cfg.URL == "" {
		cfg.URL = claudeStatusDefaultURL
	}
	if cfg.Interval <= 0 {
		cfg.Interval = claudeStatusDefaultInterval
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: claudeStatusDefaultTimeout}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Metrics == nil {
		cfg.Metrics = NewClaudeStatusMetrics()
	}
	return &ClaudeStatusPoller{
		url:       cfg.URL,
		fallbacks: cfg.FallbackURLs,
		interval:  cfg.Interval,
		client:    cfg.Client,
		logger:    cfg.Logger,
		now:       cfg.Now,
		metrics:   cfg.Metrics,
	}
}

// Verdict returns the most recent successful reading. The zero value means the
// poller has not yet completed a successful poll — callers must treat that as
// "unknown", never as "healthy".
func (p *ClaudeStatusPoller) Verdict() ClaudeStatusVerdict {
	if p == nil {
		return ClaudeStatusVerdict{}
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.verdict
}

// Run polls immediately, then on every tick until ctx is cancelled.
func (p *ClaudeStatusPoller) Run(ctx context.Context) error {
	if p == nil {
		return nil
	}
	p.logger.Info("claude status poller started",
		"url", p.url, "interval", p.interval.String())

	p.Check(ctx)

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			p.Check(ctx)
		}
	}
}

// Check performs one poll pass: fetch, parse, publish. Exported so a probe or a
// test can drive a single pass without the ticker.
//
// On any fetch/parse failure it increments the error counter and returns WITHOUT
// touching the status gauges — leaving the previous reading stale. Zeroing them
// would publish "everything operational" on a failed fetch, so a network
// partition (or the status page itself going down, which correlates with an
// outage) would actively suppress the alert this poller exists to raise.
func (p *ClaudeStatusPoller) Check(ctx context.Context) {
	if p == nil {
		return
	}
	summary, usedURL, err := p.fetchWithFallback(ctx)
	if err != nil {
		reason := classifyClaudeStatusError(err)
		p.metrics.checkErrors.Inc()
		p.metrics.checkErrorsReason.WithLabelValues(reason).Inc()
		// reason routes the oncall: a `tls` streak is a provider cert or a MITM,
		// NOT our pod egress — see the ClaudeStatusPollerStale annotation.
		p.logger.Warn("claude status poll failed (previous reading left stale)",
			"url", p.url, "reason", reason, "error", err)
		return
	}
	if usedURL != p.url {
		// A fallback served this poll — surface it so a fallback quietly carrying
		// the load while the primary is dark is visible in the logs, not silent.
		p.logger.Info("claude status poll served by fallback endpoint",
			"primary", p.url, "used", usedURL)
	}

	verdict := ClaudeStatusVerdict{
		Indicator:   summary.Status.Indicator,
		Description: summary.Status.Description,
		Components:  make(map[string]string, len(summary.Components)),
		CheckedAt:   p.now(),
	}

	p.metrics.indicator.Set(float64(claudeIndicatorSeverity(summary.Status.Indicator)))

	for _, c := range summary.Components {
		slug, known := claudeComponentSlugs[strings.ToLower(strings.TrimSpace(c.Name))]
		if !known {
			// Unknown component: counted in the log, never given a series.
			// Bounds cardinality against upstream adding components.
			p.logger.Debug("claude status: ignoring unknown component",
				"name", c.Name, "status", c.Status)
			continue
		}
		verdict.Components[slug] = c.Status
		p.metrics.component.WithLabelValues(slug).Set(float64(claudeComponentSeverity(c.Status)))
	}

	// Seed every impact bucket at 0 BEFORE counting, so the four series always
	// exist even when the page is clean. A GaugeVec exports no series for a label
	// until it is first observed; the old Reset()-then-set-only-nonzero shape meant
	// a zero-incident page (the steady state) exported NOTHING, and the leg-B "open
	// incidents by impact" System Health panel queried absent series and rendered
	// an empty green — a detector silently dead exactly when it is meant to read
	// all-clear (ka-0000003771). Seeding at 0 makes the panel a flat baseline that
	// steps up the instant an incident opens. We still zero-fill the whole set each
	// poll (not Reset()) so a bucket whose incidents all resolved drops back to 0
	// rather than lingering at its stale count.
	byImpact := map[string]int{}
	for _, impact := range claudeImpactLevels {
		byImpact[impact] = 0
	}
	for _, inc := range summary.Incidents {
		if inc.Status == "resolved" || inc.Status == "postmortem" {
			continue
		}
		impact := inc.Impact
		if !claudeImpactValid(impact) {
			impact = "none"
		}
		byImpact[impact]++
		verdict.Incidents = append(verdict.Incidents, ClaudeStatusIncident{
			ID: inc.ID, Name: inc.Name, Status: inc.Status, Impact: inc.Impact,
		})
	}
	for impact, n := range byImpact {
		p.metrics.openIncidents.WithLabelValues(impact).Set(float64(n))
	}
	sortIncidentsByImpact(verdict.Incidents)

	p.metrics.lastCheckTS.Set(float64(verdict.CheckedAt.Unix()))

	p.mu.Lock()
	p.verdict = verdict
	p.mu.Unlock()

	if verdict.Suspect() {
		p.logger.Warn("claude status page corroborates an outage on the agent path",
			"summary", verdict.Summary())
	}
}

// Sentinel wrappers so classifyClaudeStatusError can tell a failure mode apart
// AFTER it has been wrapped with context. A raw transport error (TLS/DNS/connect)
// is matched structurally; these tag the failures we detect ourselves, above the
// transport, where there is no typed error to match on.
var (
	errClaudeStatusBuildRequest = errors.New("build request")
	errClaudeStatusHTTPStatus   = errors.New("unexpected http status")
	errClaudeStatusReadBody     = errors.New("read body")
	errClaudeStatusParse        = errors.New("parse body")
	errClaudeStatusEmpty        = errors.New("empty reading")
)

// fetchWithFallback fetches the primary URL, then each fallback in order, and
// returns the first usable reading with the URL that served it. If every
// endpoint fails it returns the PRIMARY error — the deadman/reason metrics must
// describe the endpoint we are pinned to, not whichever fallback happened to be
// tried last.
func (p *ClaudeStatusPoller) fetchWithFallback(ctx context.Context) (*claudeStatusSummary, string, error) {
	summary, primaryErr := p.fetchOne(ctx, p.url)
	if primaryErr == nil {
		return summary, p.url, nil
	}
	for _, fb := range p.fallbacks {
		if fb == "" || fb == p.url {
			continue
		}
		if s, err := p.fetchOne(ctx, fb); err == nil {
			return s, fb, nil
		} else {
			p.logger.Debug("claude status fallback endpoint also failed",
				"url", fb, "error", err)
		}
	}
	return nil, p.url, primaryErr
}

func (p *ClaudeStatusPoller) fetchOne(ctx context.Context, url string) (*claudeStatusSummary, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errClaudeStatusBuildRequest, err)
	}
	req.Header.Set("User-Agent", claudeStatusUserAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get %s: %w %d", url, errClaudeStatusHTTPStatus, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, claudeStatusMaxBody))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errClaudeStatusReadBody, err)
	}

	var summary claudeStatusSummary
	if err := json.Unmarshal(body, &summary); err != nil {
		return nil, fmt.Errorf("%w: %w", errClaudeStatusParse, err)
	}
	// A 200 that parses but carries no indicator is not a usable reading —
	// canonically the HTML of a redirect landing page or an error shell. Treat it
	// as a failure so the gauges stay stale rather than being set from garbage.
	if summary.Status.Indicator == "" {
		return nil, fmt.Errorf("%w: response carried no status.indicator (wrong endpoint or unfollowed redirect?)", errClaudeStatusEmpty)
	}
	return &summary, nil
}

// classifyClaudeStatusError maps a poll failure onto a coarse, low-cardinality
// {reason} so the deadman alert routes the oncall to the right suspect instead
// of a generic "poller stale". The transport reasons (tls/dns/connect) are
// matched STRUCTURALLY off the wrapped net/tls error types — the failure that
// motivated this (ka-0000007040) is a provider-side custom-domain cert with no
// SAN for status.claude.com, which surfaces as an x509 verification error, and
// must read as `tls` (their infra / a MITM), never as `connect` (our egress).
func classifyClaudeStatusError(err error) string {
	if err == nil {
		return "none"
	}
	switch {
	case errors.Is(err, errClaudeStatusBuildRequest):
		return "build_request"
	case errors.Is(err, errClaudeStatusHTTPStatus):
		return "http_status"
	case errors.Is(err, errClaudeStatusReadBody):
		return "read_body"
	case errors.Is(err, errClaudeStatusParse):
		return "parse"
	case errors.Is(err, errClaudeStatusEmpty):
		return "empty"
	}
	// Transport-layer failures carry typed errors even after fmt.Errorf wrapping.
	var certErr x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	var certInvalid x509.CertificateInvalidError
	var recordErr *tls.RecordHeaderError
	if errors.As(err, &certErr) || errors.As(err, &hostErr) || errors.As(err, &certInvalid) || errors.As(err, &recordErr) {
		return "tls"
	}
	var tlsAlert *tls.CertificateVerificationError
	if errors.As(err, &tlsAlert) {
		return "tls"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "dns"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "connect"
	}
	// A *net.OpError that is not a DNS lookup is a dial/connect/reset — our egress
	// or the far end refusing, distinct from a cert mismatch above.
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return "connect"
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "connect"
	}
	return "other"
}

// claudeIndicatorSeverity maps a page indicator onto the ordinal ladder. An
// unrecognised indicator maps to `critical`, deliberately failing LOUD: a new
// upstream severity string we do not know about should over-report rather than
// silently read as healthy.
func claudeIndicatorSeverity(indicator string) int {
	switch strings.ToLower(strings.TrimSpace(indicator)) {
	case "none":
		return claudeIndicatorNone
	case "minor":
		return claudeIndicatorMinor
	case "major":
		return claudeIndicatorMajor
	case "critical":
		return claudeIndicatorCritical
	default:
		return claudeIndicatorCritical
	}
}

// claudeComponentSeverity maps a component status onto the ordinal ladder. An
// unrecognised status returns claudeComponentUnknownFallback (-1), which sorts
// BELOW operational so it can never trip a `>= partial_outage` alert on its own
// — an unparseable component is a poller bug, not evidence of an outage, and
// the deadman/error counter is the right signal for that.
func claudeComponentSeverity(status string) int {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "operational":
		return claudeComponentOperational
	case "degraded_performance":
		return claudeComponentDegraded
	case "partial_outage":
		return claudeComponentPartialOutage
	case "major_outage":
		return claudeComponentMajorOutage
	case "under_maintenance":
		return claudeComponentUnderMaintence
	default:
		return claudeComponentUnknownFallback
	}
}

// claudeImpactLevels is the canonical, ordered set of incident-impact buckets the
// leg-B open-incidents gauge exports. Kept in sync with claudeImpactValid — the
// poller seeds every one of these at 0 each scrape so the "open incidents by
// impact" panel has series to plot even when the page is clean (ka-0000003771).
var claudeImpactLevels = []string{"none", "minor", "major", "critical"}

func claudeImpactValid(impact string) bool {
	switch impact {
	case "none", "minor", "major", "critical":
		return true
	default:
		return false
	}
}

// sortIncidentsByImpact orders incidents most-impactful first so Summary() quotes
// the worst one. Insertion sort: the list is at most a handful of entries.
func sortIncidentsByImpact(incidents []ClaudeStatusIncident) {
	rank := func(impact string) int {
		switch impact {
		case "critical":
			return 3
		case "major":
			return 2
		case "minor":
			return 1
		default:
			return 0
		}
	}
	for i := 1; i < len(incidents); i++ {
		for j := i; j > 0 && rank(incidents[j].Impact) > rank(incidents[j-1].Impact); j-- {
			incidents[j], incidents[j-1] = incidents[j-1], incidents[j]
		}
	}
}
