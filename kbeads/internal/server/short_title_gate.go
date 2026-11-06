package server

import (
	"context"
	"os"
	"strings"
	"sync"

	"gardenio/kbeads/internal/model"
)

// short_title_gate.go carries the staged warn→enforce short_title gate (plan
// 2026-10-14-short-titles D2, gd-V5nGseRS5f). It is applied at the createBead
// choke point (beads.go) right after the authoring-time human-operator gate,
// and modelled on it (authoring_human_gate.go). Warnings reach an HTTP caller
// as X-Beads-Warning response headers through the per-request collector below.

// shortTitleGateMode is the staged rollout posture of the short_title gate.
type shortTitleGateMode int

const (
	// shortTitleGateOff — the kill switch: no metric, no warning, no refusal.
	shortTitleGateOff shortTitleGateMode = iota
	// shortTitleGateWarn — the observation window and the CODE DEFAULT. A
	// gated create with no short_title is allowed, records
	// action=allowed_warn and returns an X-Beads-Warning to an HTTP caller.
	shortTitleGateWarn
	// shortTitleGateEnforce — a gated create with no short_title is REFUSED
	// for a non-human caller (static/fleet/machine). A human caller (oidc,
	// edge) still only gets the warning: the groomer fills what a human skips.
	shortTitleGateEnforce
)

func (m shortTitleGateMode) String() string {
	switch m {
	case shortTitleGateWarn:
		return "warn"
	case shortTitleGateEnforce:
		return "enforce"
	}
	return "off"
}

// shortTitleGateModeFromEnv parses BEADS_SHORT_TITLE_GATE. Unlike
// BEADS_AUTHORING_HUMAN_GATE the default is WARN: an unset (or empty) env
// starts the observation window the moment the daemon rolls, with no chart
// change. The truthy set ("1"/"true"/"yes"/"on") and "enforce" select
// enforce; any other value, including a typo, is OFF — a typo can never
// silently start refusing creates.
func shortTitleGateModeFromEnv() shortTitleGateMode {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("BEADS_SHORT_TITLE_GATE"))) {
	case "", "warn", "observe", "dry-run":
		return shortTitleGateWarn
	case "1", "true", "yes", "on", "enforce":
		return shortTitleGateEnforce
	}
	return shortTitleGateOff
}

// isShortTitleGatedType reports whether a create of this bead type must carry
// a short_title: the types humans read by name in Slack, beads-viewer and on
// agent cards. Internal plumbing types (agent, session, message, bundle,
// trigger, …) are never gated.
func isShortTitleGatedType(t model.BeadType) bool {
	switch t {
	case model.TypeTask, model.TypeBug, model.TypeFeature, model.TypeEpic,
		model.TypeDecision, model.TypeEscalation:
		return true
	}
	return false
}

// isNonHumanCaller reports whether the authenticated caller is an agent pod,
// bridge or service (static, fleet or machine credentials) rather than a human
// at kd or the viewer (oidc, edge).
func isNonHumanCaller(id AuthIdentity) bool {
	switch id.Type {
	case "static", "fleet", "machine":
		return true
	}
	return false
}

// warningCollector gathers the non-fatal warnings one request produces. The
// HTTP create handler installs one with withWarnings and drains it into
// X-Beads-Warning headers; a gRPC or internal caller installs none, so
// addWarning is a no-op there (the gate's slog.Warn still records it).
type warningCollector struct {
	mu   sync.Mutex
	msgs []string
}

const warningsKey contextKey = "beads_warnings"

// withWarnings returns a context carrying a fresh warning collector.
func withWarnings(ctx context.Context) context.Context {
	return context.WithValue(ctx, warningsKey, &warningCollector{})
}

// addWarning appends msg to the context's collector, if any.
func addWarning(ctx context.Context, msg string) {
	c, ok := ctx.Value(warningsKey).(*warningCollector)
	if !ok {
		return
	}
	c.mu.Lock()
	c.msgs = append(c.msgs, msg)
	c.mu.Unlock()
}

// warningsFromContext returns a copy of the warnings collected so far.
func warningsFromContext(ctx context.Context) []string {
	c, ok := ctx.Value(warningsKey).(*warningCollector)
	if !ok {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.msgs...)
}
