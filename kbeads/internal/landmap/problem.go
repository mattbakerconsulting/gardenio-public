package landmap

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"

	"gardenio/lands/contract/landsv1"
)

// problemShape is each code's fixed status and title (the contract's
// fixtures/Problem set).
var problemShape = map[landsv1.ProblemCode]struct {
	status int
	title  string
}{
	landsv1.Unauthenticated:     {http.StatusUnauthorized, "Sign in to continue"},
	landsv1.NotFound:            {http.StatusNotFound, "Not found"},
	landsv1.Forbidden:           {http.StatusForbidden, "Not allowed"},
	landsv1.InvalidRequest:      {http.StatusBadRequest, "Invalid request"},
	landsv1.Conflict:            {http.StatusConflict, "Conflict"},
	landsv1.BudgetExhausted:     {http.StatusPaymentRequired, "Budget exhausted"},
	landsv1.LandPaused:          {http.StatusLocked, "Land paused"},
	landsv1.RateLimited:         {http.StatusTooManyRequests, "Too many requests"},
	landsv1.UpstreamUnavailable: {http.StatusServiceUnavailable, "Temporarily unavailable"},
}

// problemCodeOf is the code of a problem the map wrote with status: each code
// has its own status (problemShape).
func problemCodeOf(status int) landsv1.ProblemCode {
	for code, shape := range problemShape {
		if shape.status == status {
			return code
		}
	}
	return codeForStatus(status)
}

// codeForStatus maps a daemon status onto a problem code. The daemon's text
// is never consulted for the answer and never copied into it.
func codeForStatus(status int) landsv1.ProblemCode {
	switch status {
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
		return landsv1.InvalidRequest
	case http.StatusUnauthorized:
		return landsv1.Unauthenticated
	case http.StatusForbidden:
		return landsv1.Forbidden
	case http.StatusNotFound, http.StatusGone:
		return landsv1.NotFound
	case http.StatusConflict:
		return landsv1.Conflict
	case http.StatusTooManyRequests:
		return landsv1.RateLimited
	}
	return landsv1.UpstreamUnavailable
}

// writeProblem writes problem+json. detail is only ever the map's own
// request-validation text.
func writeProblem(w http.ResponseWriter, code landsv1.ProblemCode, correlationID, detail string) {
	shape := problemShape[code]
	p := landsv1.Problem{
		Type:          "urn:lands:problem:" + string(code),
		Title:         shape.title,
		Status:        shape.status,
		Code:          code,
		CorrelationId: correlationID,
	}
	if detail != "" {
		p.Detail = &detail
	}
	writeBody(w, shape.status, "application/problem+json", p)
}

// writeJSON writes a contract DTO.
func writeJSON(w http.ResponseWriter, status int, v any) {
	writeBody(w, status, "application/json", v)
}

// writeBody enforces ResponseHeaders: whatever else is already on the
// response (the daemon stamps its version on every response) is dropped. The
// answer, whole, goes through the denylist before it is written (scanLive).
func writeBody(w http.ResponseWriter, status int, contentType string, v any) {
	h := w.Header()
	for k := range h {
		if !slices.Contains(ResponseHeaders, k) {
			h.Del(k)
		}
	}
	h.Set("Content-Type", contentType)
	h.Set(landsv1.HeaderContract, landsv1.ContractVersion)
	var body bytes.Buffer
	_ = json.NewEncoder(&body).Encode(v)
	var id string
	if p, ok := v.(landsv1.Problem); ok {
		id = p.CorrelationId
	}
	scanLive(tallyFromWriter(w), h, body.Bytes(), id)
	w.WriteHeader(status)
	_, _ = w.Write(body.Bytes())
}

func newCorrelationID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// fail answers an error from a daemon call: the code from its status, the
// daemon's own words only in the log, under the correlation id.
func fail(w http.ResponseWriter, r *http.Request, op string, err error) {
	id := newCorrelationID()
	var ue *upstreamError
	if errors.As(err, &ue) {
		code := codeForStatus(ue.status)
		slog.Info("landmap: daemon refused", "op", op, "route", ue.route.String(), "status", ue.status,
			"code", string(code), "correlation_id", id, "body", string(ue.body))
		writeProblem(w, code, id, "")
		return
	}
	slog.Warn("landmap: call failed", "op", op, "correlation_id", id, "error", err)
	writeProblem(w, landsv1.UpstreamUnavailable, id, "")
}

// problem answers with a code of the map's own choosing.
func problem(w http.ResponseWriter, code landsv1.ProblemCode, detail string) {
	writeProblem(w, code, newCorrelationID(), detail)
}
