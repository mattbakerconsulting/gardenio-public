package beadsapi

import (
	"context"
	"fmt"
	"strings"
)

// Agent harness names: the agent CLI a pod runs (ADR 0004, ADR 0005). Mirrors the enum on the
// type:project, type:role and type:agent `harness` field in kbeads
// (internal/server/config.go), which rejects any other value at write time.
const (
	HarnessClaude = "claude"
	HarnessCodex  = "codex"
	HarnessGemini = "gemini"

	// DefaultHarness is what an agent runs when no layer sets a harness, so
	// every pre-existing role keeps launching Claude Code.
	DefaultHarness = HarnessClaude
)

// ParseHarness trims raw and validates it against the supported harnesses.
// Empty is valid and means "unset" (the caller falls back to the next layer);
// anything else must match exactly. kbeads rejects "Codex" at write time, so a
// case-folded match here would only mask a bead written around that gate.
func ParseHarness(raw string) (string, error) {
	h := strings.TrimSpace(raw)
	switch h {
	case "", HarnessClaude, HarnessCodex, HarnessGemini:
		return h, nil
	}
	return "", fmt.Errorf("unknown harness %q (want %q, %q or %q)", h, HarnessClaude, HarnessCodex, HarnessGemini)
}

// FieldResolvedHarness is the agent-bead field the controller writes the
// resolved harness to at spawn (ADR 0004 section 4). It is deliberately NOT the
// "harness" field: that one is the agent-level override and the top layer of
// resolution, so stamping the result there would turn it into a sticky override
// that later role/project edits could never change. Consumers (Slack card,
// metrics label, gb init, coop launch) read this field and never re-resolve.
const FieldResolvedHarness = "resolved_harness"

// StampResolvedHarness writes the resolved harness onto an agent bead. harness
// must be a concrete supported value: an empty or unknown one is refused rather
// than written, so a consumer reading the field never sees a value the
// resolver would not have produced.
func (c *Client) StampResolvedHarness(ctx context.Context, beadID, harness string) error {
	if beadID == "" {
		return fmt.Errorf("stamp resolved harness: empty bead id")
	}
	h, err := ParseHarness(harness)
	if err != nil {
		return fmt.Errorf("stamp resolved harness on %s: %w", beadID, err)
	}
	if h == "" {
		return fmt.Errorf("stamp resolved harness on %s: empty harness", beadID)
	}
	return c.UpdateBeadFields(ctx, beadID, map[string]string{FieldResolvedHarness: h})
}

// parseAllowedHarnesses splits a comma/space-separated role field into the
// supported harness names, in first-seen order, dropping blanks, duplicates and
// anything ParseHarness rejects (the bridge reports the operator-facing error
// at the point a harness is actually requested, so a typo in a role bead
// narrows the allow-list instead of blanking the whole role cache).
func parseAllowedHarnesses(raw string) []string {
	var out []string
	seen := map[string]bool{}
	for _, f := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' || r == ';' }) {
		h, err := ParseHarness(f)
		if err != nil || h == "" || seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, h)
	}
	return out
}
