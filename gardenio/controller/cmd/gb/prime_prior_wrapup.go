package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// priorWrapupFieldName is the bead field key where the revive-in-place PATCH
// archives the outgoing turn's wrap-up before it nil-deletes the live `wrapup`
// field. It mirrors bridge/bot_agent_kill.go's reviveFields["prior_wrapup"] and
// the daemon builtin registration (kbeads config.go, ka-0000000811). The field
// stores the full JSON `wrapup` payload, so it parses with the same decoder as
// WrapUpFieldName.
const priorWrapupFieldName = "prior_wrapup"

// outputPriorWrapupOnResume writes the "## Your prior turn" block when THIS
// agent was revived in place — resumed on the same bead id rather than
// cold-minted (ka-0000008136, thread ka-0000005621).
//
// The revive-in-place path (bridge/bot_agent_kill.go) deliberately nil-deletes
// the live `wrapup` field so a resumed turn starts clean and a stale summary
// can never masquerade as the current outcome. But it FIRST archives that
// summary to `prior_wrapup` on the same bead. Nothing on the agent-facing side
// read it back: the Slack card recovers a "Previous turn:" headline from it
// (bridge/bot_agents_wrapup.go), and `gb prime` surfaces a *different*
// predecessor's handoff on a shared task_id (outputPriorWorkOnBead), but the
// resumed agent itself was never shown what it did last turn — the exact data
// the archive exists to preserve.
//
// This closes that gap: on the resumed turn, render the agent's own archived
// tl;dr + accomplishments + handoff_notes so it picks up with continuity
// instead of rediscovering its own prior work.
//
// No-op (silent) when: no agent bead, no archived prior_wrapup, or the archive
// carries no usable summary. It is DISTINCT from outputPriorWorkOnBead, which
// reads a *different closed occupant's* wrap-up keyed on a shared task_id; this
// reads THIS bead's own prior_wrapup, so the two never render the same content.
// All lookups are best-effort — a daemon error just skips the block.
func outputPriorWrapupOnResume(w io.Writer, agentID string) {
	if agentID == "" || daemon == nil {
		return
	}
	ctx := context.Background()
	agentBead, err := daemon.GetBead(ctx, agentID)
	if err != nil || agentBead == nil {
		return
	}
	tldr, accomplishments, handoff := parsePriorWrapup(agentBead.Fields[priorWrapupFieldName])
	if tldr == "" && accomplishments == "" && handoff == "" {
		return
	}

	fmt.Fprintln(w, "\n## Your prior turn")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "You were resumed in place on this same bead. The previous turn's wrap-up")
	fmt.Fprintln(w, "was archived before your live wrap-up was cleared — read it so you continue")
	fmt.Fprintln(w, "your own work rather than rediscovering it.")
	fmt.Fprintln(w)
	if tldr != "" {
		fmt.Fprintln(w, "**Prior tl;dr:**")
		quoteLines(w, tldr)
		fmt.Fprintln(w)
	}
	if accomplishments != "" {
		fmt.Fprintln(w, "**What you accomplished last turn:**")
		quoteLines(w, accomplishments)
		fmt.Fprintln(w)
	}
	if handoff != "" {
		fmt.Fprintln(w, "**Your prior handoff notes:**")
		quoteLines(w, handoff)
		fmt.Fprintln(w)
	}
}

// parsePriorWrapup extracts the tldr, accomplishments, and handoff_notes strings
// from an archived prior_wrapup JSON payload. All three empty on absent field or
// parse failure — an unparseable archive is skipped rather than surfaced raw, so
// a malformed value never leaks into the primed context.
func parsePriorWrapup(raw string) (tldr, accomplishments, handoff string) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return "", "", ""
	}
	var wu struct {
		TLDR            string `json:"tldr"`
		Accomplishments string `json:"accomplishments"`
		HandoffNotes    string `json:"handoff_notes"`
	}
	if err := json.Unmarshal([]byte(raw), &wu); err != nil {
		return "", "", ""
	}
	return strings.TrimSpace(wu.TLDR),
		strings.TrimSpace(wu.Accomplishments),
		strings.TrimSpace(wu.HandoffNotes)
}

// quoteLines writes s to w as a markdown blockquote, one "> "-prefixed line per
// source line, so multi-line wrap-up prose renders as a single quoted block.
func quoteLines(w io.Writer, s string) {
	for line := range strings.SplitSeq(strings.TrimRight(s, "\n"), "\n") {
		fmt.Fprintf(w, "> %s\n", line)
	}
}
