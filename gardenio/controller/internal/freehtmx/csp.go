package freehtmx

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

// StrictCSP is the Content-Security-Policy header value applied to widget-
// rendering routes. See gardenio/docs/research/free-htmx-widget-security.md §3.
//
// `default-src 'none'` makes every directive explicit; `script-src 'self'`
// finally drops the long-standing `'unsafe-inline'` (audit F6 in
// gardenio/docs/audits/ingress-tls-static.md). Widgets cannot include
// <script>, <style>, or style= attributes (sanitizer strips), so the strict
// directive is finally tightenable. `style-src` keeps `'unsafe-inline'` for
// beads-viewer's existing inline styles in the layout — widgets themselves
// cannot benefit from it.
//
// The report-uri directs CSP-violation reports to the same-origin
// /htmx/widget/csp-report endpoint, which the report handler in this package
// receives. (`report-uri` is the legacy directive supported by every
// browser; `report-to` is omitted intentionally — it requires the
// Reporting-Endpoints / Report-To headers and we keep a single delivery
// path for v1.)
const StrictCSP = "default-src 'none'; " +
	"script-src 'self'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data: https://s3.amazonaws.com https://*.s3.amazonaws.com; " +
	"font-src 'self' data:; " +
	"connect-src 'self'; " +
	"form-action 'self'; " +
	"frame-ancestors 'none'; " +
	"frame-src 'none'; " +
	"object-src 'none'; " +
	"base-uri 'none'; " +
	"manifest-src 'none'; " +
	"worker-src 'none'; " +
	"upgrade-insecure-requests; " +
	"report-uri /htmx/widget/csp-report"

// CSPMiddleware sets the strict CSP on every response for widget-rendering
// routes. It is in addition to the Traefik-level header (helm
// templates/security-headers.yaml) — the in-process middleware exists so
// tests and dev (BEADS_VIEWER_DEV_AUTH_BYPASS) get the same CSP without
// depending on Traefik, and so a Traefik misconfig can't silently downgrade
// the policy on widget routes.
//
// If both Traefik and this middleware set Content-Security-Policy with
// identical strings, the browser still applies a single union of policies
// (multiple CSP headers are intersected, never weakened), so the doubled
// header is safe.
func CSPMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", StrictCSP)
		next.ServeHTTP(w, r)
	})
}

// CSPReport is the report-uri payload shape sent by browsers as
// Content-Type: application/csp-report. Only the documented fields are
// captured — extra fields are ignored.
type CSPReport struct {
	Report struct {
		DocumentURI        string `json:"document-uri"`
		Referrer           string `json:"referrer"`
		ViolatedDirective  string `json:"violated-directive"`
		EffectiveDirective string `json:"effective-directive"`
		OriginalPolicy     string `json:"original-policy"`
		Disposition        string `json:"disposition"`
		BlockedURI         string `json:"blocked-uri"`
		StatusCode         int    `json:"status-code"`
		LineNumber         int    `json:"line-number,omitempty"`
		ColumnNumber       int    `json:"column-number,omitempty"`
		SourceFile         string `json:"source-file,omitempty"`
		ScriptSample       string `json:"script-sample,omitempty"`
	} `json:"csp-report"`
}

// Sink is the destination for parsed CSP violation reports. The default
// sink in CSPReportHandler logs to slog; production wires this to a
// kbeads bug-bead emitter (cron summarizes per-day) — see plan §3
// "report-uri" notes and follow-up on ka-0000007136.
type Sink interface {
	Record(r *http.Request, report CSPReport)
}

// SinkFunc adapts a function to the Sink interface.
type SinkFunc func(r *http.Request, report CSPReport)

func (f SinkFunc) Record(r *http.Request, report CSPReport) { f(r, report) }

// maxCSPReportBytes caps the report body. Real reports are well under 4 KB;
// 64 KB is generous and forecloses memory-amplification attempts.
const maxCSPReportBytes = 64 * 1024

// CSPReportHandler returns an HTTP handler for the /htmx/widget/csp-report
// endpoint. It reads up to maxCSPReportBytes, decodes the JSON, and forwards
// to sink. A nil sink falls back to a slog default that logs at WARN.
//
// The handler always returns 204 No Content on success and on parse failure
// (a malformed report is not an error condition for the reporting agent;
// emitting non-2xx would teach browsers to back off).
func CSPReportHandler(logger *slog.Logger, sink Sink) http.HandlerFunc {
	if sink == nil {
		sink = SinkFunc(func(r *http.Request, report CSPReport) {
			logger.Warn("csp violation",
				"document_uri", report.Report.DocumentURI,
				"violated_directive", report.Report.ViolatedDirective,
				"blocked_uri", report.Report.BlockedURI,
				"source_file", report.Report.SourceFile,
				"line_number", report.Report.LineNumber,
				"remote_addr", r.RemoteAddr,
				"user_agent", r.UserAgent(),
			)
		})
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		ct := r.Header.Get("Content-Type")
		// Accept application/csp-report (legacy report-uri), application/json
		// (some browsers, e.g. Firefox >=93 with reportingapi), and
		// application/reports+json (Reporting API). Be liberal in what we
		// accept here — the parser does the strict check.
		if ct != "" {
			ct = strings.SplitN(ct, ";", 2)[0]
			ct = strings.TrimSpace(strings.ToLower(ct))
			switch ct {
			case "application/csp-report", "application/json", "application/reports+json":
				// ok
			default:
				// Unknown content type — accept the body anyway but flag.
				logger.Debug("csp report with unexpected content-type", "content_type", ct)
			}
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxCSPReportBytes))
		if err != nil {
			logger.Warn("csp report read error", "error", err)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var report CSPReport
		if err := json.Unmarshal(body, &report); err != nil {
			logger.Warn("csp report parse error", "error", err, "body_size", len(body))
			w.WriteHeader(http.StatusNoContent)
			return
		}
		sink.Record(r, report)
		w.WriteHeader(http.StatusNoContent)
	}
}
