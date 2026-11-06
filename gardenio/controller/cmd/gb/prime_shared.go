package main

// prime_shared.go contains the outputPrimeForHook function shared by
// bus_emit.go (SessionStart injection) and hook.go (hook prime command).

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"

	"gardenio/controller/internal/beadsapi"
)

// primeRole returns the agent role from BOAT_ROLE env var.
func primeRole() string {
	return os.Getenv("BOAT_ROLE")
}

// systemPromptThreadContextMaxChars is the cap used when emitting Slack thread
// context as part of the cacheable system-prompt body. Bypasses the hook
// output channel entirely; we can afford a much larger budget here.
//
// This is the ONLY thread-context delivery path. The hook channel used to
// re-emit a 10 KB preview as belt-and-suspenders; measurement (ka-0000007437)
// showed that preview was 99.8% duplicate of this section AND was the reason
// hook stdout blew past the harness cap — see outputPrimeForHook.
const systemPromptThreadContextMaxChars = 50000

// outputPrimeForHook generates the trimmed prime preview wrapped in a
// system-reminder tag for the SessionStart and PreCompact hook channels.
//
// The hook channel is a SMALL, HARD-CAPPED budget: the harness truncates hook
// stdout at ~2 KB, saves the remainder to a file, and injects only the first
// 2 KB inline. Anything emitted past that cap is not free — it is invisible
// until the agent spends a tool call reading the spill file.
//
// So this channel carries ONLY live state the cached system prompt cannot:
// identity-free session facts (active jacks) plus a pointer for on-demand
// recall. Everything durable — workflow context, advice, advice-role-diff,
// roster, auto-assign, user preferences — arrives via
// --append-system-prompt-file at coop session start (buildSystemPromptBody).
//
// Deliberately NOT emitted here (ka-0000007437), because
// buildSystemPromptBody already emits each one at full fidelity through the
// uncapped system-prompt path:
//
//	outputIdentity            — 99% duplicate
//	outputSlackThreadContext  — 99.8% duplicate, and the system-prompt copy is
//	                            LARGER (50 KB cap vs the old 10 KB hook cap)
//	outputThreadSiblings      — 94% duplicate
//
// Measured on a live thread pod, those three pushed hook stdout to 15,475 B
// against a 2,206 B inline budget — so the ONE section that genuinely had to
// ride this channel, `## Assignment` (emitted by `gb hook prime` below), was
// evicted past the cap entirely. Dropping the duplicates is what lets live
// state fit inline again.
func outputPrimeForHook(w io.Writer, agentID string) {
	fmt.Fprintln(w, "<system-reminder>")

	// Per-section fallback: emit a section here ONLY if the system prompt this
	// session actually launched with does not already contain it. On a healthy
	// boot that suppresses all three (the de-dup this function exists for); in
	// the two degraded cases it is what keeps the agent from booting blind.
	//
	// Case 1 — no system prompt at all. writeSystemPromptFile is best-effort:
	// on mkdir/exec/write failure the caller launches WITHOUT
	// --append-system-prompt-file, so every section below has no other channel.
	//
	// Case 2 — WARM POOL, the subtle one. runCoopSession writes the system
	// prompt (init_agent_loop.go:414) BEFORE standbyWait (:569) blocks for an
	// assignment, and SLACK_THREAD_* is only hydrated at assignment
	// (hydrateAssignmentEnv). A pooled pod's system prompt is therefore written
	// while it is still unbound, and outputSlackThreadContext/outputThreadSiblings
	// no-op — verified: `gb prime --system-prompt` with SLACK_THREAD_* unset
	// emits 0 thread-context sections. The write SUCCEEDS, so a boolean
	// "did we write one?" check would wrongly suppress the thread sections and
	// leave a pooled thread agent with no conversation at all. Hooks run after
	// assignment, so re-emitting here is both correct and the only channel.
	inPrompt := systemPromptSections()
	if !inPrompt["## Your Identity"] {
		outputIdentity(w, agentID, primeRole())
	}
	if !inPrompt["## Slack Thread Context"] {
		outputSlackThreadContext(w, systemPromptThreadContextMaxChars)
	}
	if !inPrompt["## Prior beads in THIS Slack thread"] {
		outputThreadSiblings(w)
	}

	// Active jacks — live state. Changes between sessions, so it has to
	// re-render through this channel rather than living in the cached
	// system prompt.
	outputJackSection(w)

	fmt.Fprintln(w, "Identity, Slack thread context, prior thread beads, workflow context,")
	fmt.Fprintln(w, "advice, roster, and auto-assign are loaded in the system prompt.")
	fmt.Fprintln(w, "Run `gb prime` to print the full preamble on demand.")
	fmt.Fprintln(w, "</system-reminder>")
}

// hookDedupSections are the section headings outputPrimeForHook suppresses when
// the current session's system prompt already carries them.
var hookDedupSections = []string{
	"## Your Identity",
	"## Slack Thread Context",
	"## Prior beads in THIS Slack thread",
}

// systemPromptSections reports which of hookDedupSections are present in the
// system-prompt file THIS session was launched with. An absent/unreadable file
// yields an all-false map, so every section falls back to the hook channel.
//
// Keyed off the env var writeSystemPromptFile exports on success
// (envSystemPromptFile) rather than a bare .gb/system-prompt.md stat: a
// warm-pool pod already has a pool-fill-era file on disk before it is ever
// assigned, so the path alone cannot identify the CURRENT session's prompt.
// Reading the file (not just testing existence) is what catches the warm-pool
// case where the write succeeded but the thread sections were empty.
func systemPromptSections() map[string]bool {
	present := make(map[string]bool, len(hookDedupSections))
	p := os.Getenv(envSystemPromptFile)
	if p == "" {
		return present
	}
	body, err := os.ReadFile(p)
	if err != nil {
		return present
	}
	for _, h := range hookDedupSections {
		// Anchor to line start so a mention inside prose (e.g. the advice
		// catalog quoting a heading) cannot masquerade as the real section.
		present[h] = bytes.HasPrefix(body, []byte(h+"\n")) ||
			bytes.Contains(body, []byte("\n"+h+"\n"))
	}
	return present
}

// outputSlackThreadContext fetches and outputs the originating Slack thread
// context for thread-spawned agents. No-op if the agent was not spawned from
// a Slack thread or if the bridge is unreachable.
//
// maxChars is forwarded to formatThreadMarkdown to bound the rendered size.
// Callers pass systemPromptThreadContextMaxChars: the system-prompt path is the
// only routine delivery channel, and the hook's degraded-boot fallback uses the
// same budget because in that case it is the sole channel carrying the thread.
func outputSlackThreadContext(w io.Writer, maxChars int) {
	bridgeURL := os.Getenv("SLACK_BRIDGE_URL")
	if bridgeURL == "" {
		return
	}
	// Resolve via the env→agent-bead fallback rather than reading the env vars
	// directly: a PREWARMED (pool-assigned) pod boots with SLACK_THREAD_* baked
	// empty and only recovers the coords the controller stamped onto its agent
	// bead at assign time. An env-only read (or the env-only isThreadSpawnedAgent
	// gate) leaves such a pod with no thread context at all (bug ka-0000006843).
	channel, threadTS := resolveThreadCoords(context.Background())
	if channel == "" || threadTS == "" {
		return
	}

	msgs := fetchThreadMessages(bridgeURL, channel, threadTS, 50)
	if len(msgs) == 0 {
		return
	}

	fmt.Fprintln(w, "## Slack Thread Context")
	fmt.Fprintln(w, "You were spawned to help with this Slack thread:")
	fmt.Fprintln(w)
	fmt.Fprint(w, formatThreadMarkdown(msgs, maxChars, true, parseExcludeSubtypes(defaultExcludeSubtypes)))
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Reply to the thread using `gb slack reply` for conversational responses.")
	fmt.Fprintln(w, "---")
	fmt.Fprintln(w)
}

// threadSiblingsMax bounds how many sibling beads outputThreadSiblings renders.
// A thread that spawned a lot of work still gets a useful, bounded list; the
// full set is one `kd list -f thread_id=…` away.
const threadSiblingsMax = 40

// outputThreadSiblings lists the beads already created in this agent's own Slack
// thread — its siblings — so a RESUMED thread agent discovers prior work
// (answers, tasks, MRs) instead of re-deriving it from scratch (ka-0000001160).
//
// It resolves the thread (wish) head via lookupSlackThreadBead — which reads the
// agent's own thread coordinates, so it works even on a warm-pool pod whose
// SLACK_THREAD_BEAD_ID env was never hydrated — then queries membership by the
// canonical wish-pointer field thread_id (fields->>'thread_id' == headID). This
// is the same head-safe carrier the daemon auto-stamps on every agent-created
// bead (maybeCopyThreadID); it deliberately does NOT use the head-exclusive
// slack-thread:{channel}:{ts} label, which identifies only the head.
//
// No status filter is applied: a CLOSED answer bead is exactly what a resumed
// agent must find, so closed siblings are included. Best-effort and silent — a
// nil daemon, an unresolved head, or a query error just skips the block.
func outputThreadSiblings(w io.Writer) {
	if daemon == nil {
		return
	}
	ctx := context.Background()
	// lookupSlackThreadBead resolves coords via the env→agent-bead fallback, so
	// this works on a PREWARMED pod whose SLACK_THREAD_* env was baked empty; it
	// returns "" (and we no-op) for any non-thread agent (bug ka-0000006843).
	headID := lookupSlackThreadBead(ctx)
	if headID == "" {
		return
	}

	// Membership = beads whose fields.thread_id points at the head. --all-types
	// equivalent: no Kinds filter, so agent/message/doc/task siblings all match
	// (a default kind=issue filter would hide them). No Statuses filter so
	// closed answers surface.
	res, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Fields: map[string]string{"thread_id": headID},
		Sort:   "created_at",
		Limit:  threadSiblingsMax + 1,
	})
	if err != nil || res == nil || len(res.Beads) == 0 {
		return
	}

	// Drop the agent's own bead from the list — it is not prior work to reuse.
	selfBeadID := os.Getenv("BOAT_AGENT_BEAD_ID")
	if selfBeadID == "" {
		selfBeadID = os.Getenv("KD_AGENT_ID")
	}
	siblings := make([]*beadsapi.BeadDetail, 0, len(res.Beads))
	for _, b := range res.Beads {
		if b == nil || b.ID == selfBeadID || b.ID == headID {
			continue
		}
		siblings = append(siblings, b)
	}
	if len(siblings) == 0 {
		return
	}

	fmt.Fprintln(w, "## Prior beads in THIS Slack thread")
	fmt.Fprintln(w, "Beads already created in this thread (by you or an earlier session on it).")
	fmt.Fprintln(w, "CHECK THESE before re-deriving an answer — an earlier sibling may already hold it:")
	fmt.Fprintln(w)
	truncated := false
	if len(siblings) > threadSiblingsMax {
		siblings = siblings[:threadSiblingsMax]
		truncated = true
	}
	for _, b := range siblings {
		kind := b.Type
		if kind == "" {
			kind = b.Kind
		}
		fmt.Fprintf(w, "- `%s` [%s/%s] %s\n", b.ID, kind, b.Status, truncateSnippet(b.Title, 120))
	}
	if truncated {
		fmt.Fprintf(w, "- … (more; showing first %d)\n", threadSiblingsMax)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Full/updated list: `kd list -f thread_id=%s --all-types --status open --status closed`\n", headID)
	fmt.Fprintln(w, "---")
	fmt.Fprintln(w)
}
