package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// Phase 9 — Incident Intelligence telemetry endpoint.
//
// The inbox row "View report →" link and the rich-report header back-link
// both carry data-ii-telemetry attributes. The static incident_intel_telemetry.js
// module listens for clicks on those elements and POSTs a small JSON event here.
//
// We do NOT persist these events anywhere yet — the v1 endpoint just logs them
// at info-level so the operator can see click-through happen and decide where
// to forward later (kbeads counter? prom counter? structured log sink?).
// Keeping it dumb avoids the ramp work being load-bearing on a future schema
// decision.

// telemetryEvent is the wire shape posted by the static JS module. Fields are
// best-effort — anything missing is dropped before logging so a bot pinging
// the endpoint can't blow up the controller.
type telemetryEvent struct {
	Event  string `json:"event"`
	Target string `json:"target"`
	From   string `json:"from"`
	Path   string `json:"path"`
}

// validTelemetryEvents bounds what gets logged so operators don't drown in
// arbitrary client-supplied event names. Phase 9 ships three, mirroring the
// data-ii-telemetry attributes baked into project_row.html and the rich-report
// header.
var validTelemetryEvents = map[string]struct{}{
	"inbox-rich-report-click":     {},
	"inbox-rich-report-cta-click": {},
	"rich-report-backlink-click":  {},
}

// handleIncidentIntelTelemetry accepts a JSON body of shape telemetryEvent
// and returns 204. Bad JSON or unknown event names yield 400; we'd rather
// the JS module find out it's wrong than silently no-op forever.
func (s *Server) handleIncidentIntelTelemetry(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4096))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	var ev telemetryEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	ev.Event = strings.TrimSpace(ev.Event)
	if _, ok := validTelemetryEvents[ev.Event]; !ok {
		http.Error(w, "unknown event", http.StatusBadRequest)
		return
	}
	if s.logger != nil {
		s.logger.Info("incident-intel telemetry",
			"event", ev.Event,
			"target", ev.Target,
			"from", ev.From,
			"path", ev.Path,
		)
	}
	// Best-effort persist to the daemon's ui_telemetry sink (ka-0000006089).
	// Failures are logged but never propagated — telemetry must never break
	// the click that emitted it.
	if s.daemon != nil {
		row := &beadsapi.UITelemetryRow{
			Event:      ev.Event,
			Target:     ev.Target,
			From:       ev.From,
			Path:       ev.Path,
			OperatorID: oidcauth.EmailFromContext(r.Context()),
		}
		if err := s.daemon.RecordUITelemetry(r.Context(), row); err != nil && s.logger != nil {
			s.logger.Warn("incident-intel telemetry sink failed",
				"event", ev.Event,
				"err", err,
			)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}
