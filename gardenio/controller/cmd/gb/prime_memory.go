package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// memoryProtocolAdviceID is the bead ID of the global advice bead containing
// the full memory file format and write protocol. Ephemeral agents see only a
// short pointer to this advice bead instead of the ~3 KB Claude Code auto-memory
// preamble. See ka-0000006599.
const memoryProtocolAdviceID = "ka-0000004089"

// isMemoryEligible reports whether the agent should receive Claude Code's full
// auto-memory preamble. Memory-eligible agents are persistent / long-lived and
// are likely to actually exercise the memory system across sessions:
//
//   - any agent whose role contains "gardener" (gardeners drive multi-cycle work)
//   - any agent whose bead has schedule_id set (scheduled-loop agents)
//
// Everyone else (one-shot crew, thread agents, ephemeral spawns) gets a short
// pointer to the memory protocol advice bead in their system prompt and the
// full preamble is suppressed via CLAUDE_CODE_DISABLE_AUTO_MEMORY=1.
//
// agentBead is optional — pass nil and the function will fetch it via daemon
// when needed. Passing a pre-fetched bead avoids a redundant round-trip when
// the caller already has it.
func isMemoryEligible(ctx context.Context, agentID, role string, agentBead *beadsapi.BeadDetail) bool {
	if strings.Contains(role, "gardener") {
		return true
	}
	if agentID == "" {
		return false
	}
	if agentBead == nil {
		var err error
		agentBead, err = daemon.GetBead(ctx, agentID)
		if err != nil || agentBead == nil {
			return false
		}
	}
	if agentBead.Fields["schedule_id"] != "" {
		return true
	}
	return false
}

// outputMemoryPointer writes a short (~500 char) memory pointer to w for
// non-memory-eligible agents. For memory-eligible agents this is a no-op
// because Claude Code injects the full auto-memory preamble itself.
//
// The pointer tells the agent (a) where the memory dir is, (b) that the full
// preamble is suppressed, and (c) how to fetch the full protocol on demand.
func outputMemoryPointer(w io.Writer, agentID, role string) {
	if isMemoryEligible(context.Background(), agentID, role, nil) {
		return
	}
	fmt.Fprintln(w, "\n## Memory")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Per-project memory directory exists under `~/.claude/projects/<workspace>/memory/`.")
	fmt.Fprintln(w, "The full Claude Code auto-memory preamble is suppressed for ephemeral agents to save context.")
	fmt.Fprintf(w, "If you actually need to read or write memory, run `gb advice show %s` for the full file-format and write protocol.\n", memoryProtocolAdviceID)
	fmt.Fprintln(w)
}
