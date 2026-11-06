package main

import (
	"log/slog"
	"net/http"
	"net/url"
	"strings"
)

// Homepage telemetry — M2 follow-up (ka-0000000097), §9 of
// `gardenio/docs/research/per-user-homepage-design.md`.
//
// We emit structured slog events from three surfaces:
//
//   - homepage.user_home.view  on GET /u/{slug}
//   - homepage.inbox.view      on GET /inbox
//   - homepage.search.view     on GET /search   (operated_by filter present)
//
// Approach (b) from the task notes: lean on info-level structured logs that
// the existing observability pipeline already scrapes. No new Prometheus
// dependency. Customization-adoption (% of operators with any non-default
// ProfilePref) is computed offline from `profile_prefs` JSON, so it is not
// instrumented here.
//
// Event names share the `homepage.` prefix so a single log selector pulls all
// the per-user-homepage telemetry into one stream for §9's success metrics.
const (
	telemetryEventUserHomeView = "homepage.user_home.view"
	telemetryEventInboxView    = "homepage.inbox.view"
	telemetryEventSearchView   = "homepage.search.view"
)

// refererPath returns just the path component of the Referer header so the
// log field is bounded and free of query-string PII. Empty string when the
// header is absent or unparseable — downstream queries treat empty as "direct
// navigation / no referer".
func refererPath(r *http.Request) string {
	ref := r.Header.Get("Referer")
	if ref == "" {
		return ""
	}
	u, err := url.Parse(ref)
	if err != nil {
		return ""
	}
	return u.Path
}

// emitTelemetry is the single info-level call all surfaces go through. Kept
// as one function so attribute names stay stable across emit sites — log
// consumers join on `event` and field keys, so consistency matters more than
// the call-site ergonomics.
func emitTelemetry(logger *slog.Logger, event string, attrs ...any) {
	if logger == nil {
		return
	}
	logger.Info(event, attrs...)
}

// trimEmail folds an email to lower-case and trims surrounding whitespace.
// Returns "" if the input is empty. Used to normalize the viewer_email
// dimension so the offline aggregator does not have to defensively lower-case.
func trimEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}
