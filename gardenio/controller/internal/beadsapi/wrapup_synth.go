package beadsapi

import (
	"encoding/json"
	"fmt"
	"strings"
)

// WrapUpFieldName is the bead field key where the structured wrap-up JSON is
// stored. It mirrors the constant of the same name in cmd/gb (wrapup.go); both
// must agree on the field name. Controller-side close paths (the stale-reaper)
// have no in-pod gb binary, so they synthesize the fallback wrap-up here in the
// beadsapi layer instead. See Fix A, ka-0000007639.
const WrapUpFieldName = "wrapup"

// synthWrapUp is the minimal subset of the agent wrap-up JSON the controller
// synthesizes when reaping a bead that has none. It matches the shape gb writes
// (cmd/gb wrapup.go WrapUp): {tldr, accomplishments, custom:{...}}.
type synthWrapUp struct {
	TLDR            string            `json:"tldr,omitempty"`
	Accomplishments string            `json:"accomplishments"`
	Custom          map[string]string `json:"custom,omitempty"`
}

// reapReasonTLDR maps a stale-reaper reason string to a one-line tl;dr for the
// synthesized fallback wrap-up. Reasons are the free-text strings produced by
// StaleReaper.reapReason (e.g. "stale: no activity for 10m0s", "orphan: source
// task ka-X already closed", "stale: unschedulable, cluster capacity ..."). The
// match is prefix-based on the reason family so the phrasing stays stable as
// the suffix detail varies. An unrecognized reason is surfaced verbatim so no
// signal is lost; an empty reason yields a generic line.
func reapReasonTLDR(reason string) string {
	r := strings.TrimSpace(reason)
	lower := strings.ToLower(r)
	switch {
	case r == "":
		return "Reaped by the controller before an explicit wrap-up"
	case strings.HasPrefix(lower, "orphan:"):
		return "Reaped as orphaned (source task already closed) before an explicit wrap-up"
	case strings.HasPrefix(lower, "podless_leak"):
		return "Reaped: agent pod was gone before it wrapped up"
	case strings.HasPrefix(lower, "stale:"), strings.HasPrefix(lower, "stale "):
		return "Reaped as stale before an explicit wrap-up"
	default:
		return fmt.Sprintf("Reaped (%s) before an explicit wrap-up", r)
	}
}

// ReapWrapUpTitle returns a short human label for a bead the controller is
// reaping — the agent name when set, otherwise the bead title — used to give
// the synthesized fallback tl;dr some specificity beyond the generic reason.
// Mirrors cmd/gb beadDisplayTitle. Safe on a nil detail.
func ReapWrapUpTitle(detail *BeadDetail) string {
	if detail == nil {
		return ""
	}
	if agent := strings.TrimSpace(detail.Fields["agent"]); agent != "" {
		return agent
	}
	return strings.TrimSpace(detail.Title)
}

// HasRealWrapUp reports whether raw is a genuine agent-recorded wrap-up — i.e.
// non-empty JSON with populated accomplishments that was NOT synthesized by the
// controller (custom.synthesized != "true"). The stale-reaper uses this to tell
// an agent that stopped cleanly via `gb stop` (real wrap-up present) from one
// that crashed/OOMed (no wrap-up, or only a controller-synthesized fallback) —
// so it does not stamp a stranded run-instance task with false crash provenance
// (ka-0000006105). A malformed/empty wrap-up returns false (treated as absent).
func HasRealWrapUp(raw string) bool {
	if strings.TrimSpace(raw) == "" {
		return false
	}
	var wu synthWrapUp
	if err := json.Unmarshal([]byte(raw), &wu); err != nil {
		return false
	}
	if strings.TrimSpace(wu.Accomplishments) == "" {
		return false
	}
	return wu.Custom["synthesized"] != "true"
}

// SynthesizeReapWrapUp builds a minimal wrap-up JSON ({tldr, accomplishments,
// custom:{synthesized:true, close_reason}}) for an agent bead the controller is
// reaping without one, so the Slack agent card renders a tl;dr headline instead
// of metadata-only (Fix A, ka-0000007639). The tl;dr is derived from the reap
// reason; title (when non-empty) is appended for context. This is the
// controller-side mirror of gb's synthesizeFallbackWrapUp. Callers must only
// invoke it when the bead has no existing wrap-up — a real wrap-up always wins.
// Returns "" only if the JSON could not be marshaled (defensive).
func SynthesizeReapWrapUp(reason, title string) string {
	tldr := reapReasonTLDR(reason)
	if t := strings.TrimSpace(title); t != "" {
		tldr = fmt.Sprintf("%s — %s", tldr, t)
	}
	wu := synthWrapUp{
		TLDR:            tldr,
		Accomplishments: tldr,
		Custom: map[string]string{
			"synthesized":  "true",
			"close_reason": strings.TrimSpace(reason),
		},
	}
	data, err := json.Marshal(wu)
	if err != nil {
		return ""
	}
	return string(data)
}
