// freezethaw_interrupt_handler.go holds the HTTP handler for the C2
// interruptible-freeze ingress endpoint (POST /api/v1/freeze/interrupt,
// task ka-0000005834). It lives in podmanager — next to the FreezeThawManager
// abort machinery it drives — so the controller (cmd/controller) AND the
// dedicated e2e probe (cmd/freeze-interrupt-probe, task ka-0000004367) serve the
// IDENTICAL genuine handler rather than each re-implementing the wire contract.
// A re-implementation in the probe would let a handler regression (status code,
// JSON shape, abort dispatch) pass the e2e untouched — the exact trap the
// rehydrate-ack probe avoids by driving the real thawepoch gate.
package podmanager

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// FreezeInterruptRequest is the POST /api/v1/freeze/interrupt body: the agent
// the inbound user message targets.
type FreezeInterruptRequest struct {
	Agent string `json:"agent"`
}

// FreezeInterruptResponse is the endpoint's JSON reply. Aborted is true iff a
// freeze was in flight and signalled (the agent is left live to serve the
// message); false means nothing was freezing (the caller proceeds with its
// normal route/cold-start decision).
type FreezeInterruptResponse struct {
	Aborted bool   `json:"aborted"`
	Agent   string `json:"agent"`
}

// FreezeInterruptHandler returns the HTTP handler for POST
// /api/v1/freeze/interrupt (C2 interruptible freeze, ka-0000005834). The
// message-ingress seam calls it when a user message targets an agent that may
// be mid-freeze: abort is the supplied callback (the controller passes its
// lifecycle abort, the probe passes FreezeThawManager.AbortFreeze) which aborts
// the in-flight checkpoint, if any, so the freeze path leaves Pod A live to
// serve the message.
//
//   - 200 {"aborted":true}  — a freeze was in flight and signalled.
//   - 404 {"aborted":false} — nothing was freezing (not an error; the agent is
//     either already live or genuinely gone). Inert when abort is nil (freeze/
//     thaw off) or FreezeInterruptEnabled is off (the registry is empty), both
//     yielding 404 — the caller's behaviour is unchanged from today.
//   - 400 — malformed body (missing/empty agent).
//
// abort MUST be safe to call concurrently and on a disabled feature (return
// false); both supplied callbacks are.
func FreezeInterruptHandler(abort func(agent string) bool, logger *slog.Logger) http.HandlerFunc {
	if logger == nil {
		logger = slog.Default()
	}
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req FreezeInterruptRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Agent == "" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": `request body must be {"agent":"<name>"}`,
			})
			return
		}
		if abort != nil && abort(req.Agent) {
			logger.Info("freeze interrupt: aborted in-flight checkpoint via API; agent left live",
				"agent", req.Agent, "actor", r.Header.Get("X-Actor"))
			_ = json.NewEncoder(w).Encode(FreezeInterruptResponse{Aborted: true, Agent: req.Agent})
			return
		}
		// Nothing in flight — not an error, just "no freeze to interrupt".
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(FreezeInterruptResponse{Aborted: false, Agent: req.Agent})
	}
}
