package main

import (
	"errors"
	"net/http"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// inlineErrorHeader marks a non-2xx response whose body is a swappable editor
// fragment carrying its own inline error banner. htmx 2's default
// responseHandling discards 4xx/5xx bodies, so the layout's htmx:beforeSwap
// handler opts these responses back in (see layout.html). Without it the
// editors stay open with no feedback when the daemon rejects a write
// (gd-7Ml6sbsdu1).
const inlineErrorHeader = "X-BV-Inline-Error"

// maxInlineDaemonMsg caps the daemon message echoed into an editor banner.
const maxInlineDaemonMsg = 300

// writeInlineStatus writes a non-200 status for an editor fragment and flags
// the response so the browser swaps the in-band error banner instead of
// dropping it. Callers invoke it only for status != 200.
func writeInlineStatus(w http.ResponseWriter, status int) {
	w.Header().Set(inlineErrorHeader, "1")
	w.WriteHeader(status)
}

// daemonWriteFailure maps a failed daemon write onto the editor banner message
// and the HTTP status the viewer should answer with. A daemon 4xx keeps its
// class (403 stays 403, 409 stays 409) and its message is appended to base so
// the operator sees why — e.g. the role a 403 requires. 5xx and transport
// errors stay 502 with the generic base message (internals are not echoed).
//
// A daemon 401 means the viewer's own credential to the daemon failed, not the
// user's session; answering 401 would trip the layout's edge re-auth reload,
// so it stays 502.
func daemonWriteFailure(err error, base string) (string, int) {
	var apiErr *beadsapi.APIError
	if !errors.As(err, &apiErr) {
		return base, http.StatusBadGateway
	}
	code := apiErr.StatusCode
	if code < 400 || code >= 500 || code == http.StatusUnauthorized {
		return base, http.StatusBadGateway
	}
	detail := strings.Join(strings.Fields(apiErr.Message), " ")
	if len(detail) > maxInlineDaemonMsg {
		detail = detail[:maxInlineDaemonMsg] + "…"
	}
	if detail == "" {
		return base, code
	}
	return strings.TrimSuffix(base, ".") + ": " + detail, code
}
