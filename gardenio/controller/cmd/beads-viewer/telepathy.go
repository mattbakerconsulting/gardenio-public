package main

import (
	"fmt"
	"hash/fnv"
	"html"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// telepathy.go — the unified cross-agent "thinking" feed (epic ka-0000007154,
// task ka-0000002159). A single live SSE page that streams every agent's
// thinking-trace blocks onto one screen so an operator gets a "telepathic"
// sense of what the whole fleet is reasoning about right now.
//
// Division of labour: the NATS subscriber (task ka-0000007528) owns ThinkingHub
// (thinking_hub.go) — it connects to NATS, subscribes
// coop.events.messages.*.thinking, drops signature-only empties, and fans out
// ThinkingEvent values with a backfilled ring on Subscribe. This file owns the
// consumer side: the page render, the SSE handler, and the per-event row HTML.

// telepathyDisplayName resolves the best display label for an event's agent,
// falling back through Agent → "unknown".
func telepathyDisplayName(evt ThinkingEvent) string {
	name := strings.TrimSpace(evt.Agent)
	if name == "" {
		name = "unknown"
	}
	return name
}

// handleTelepathy renders the unified telepathy page shell. The live feed is
// streamed separately over /telepathy/events (SSE); this handler only emits the
// page chrome + empty lane the client script fills.
func (s *Server) handleTelepathy(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{
		"Title":       "Telepathy",
		"FeedEnabled": s.thinking != nil,
	}
	s.render(w, r, "telepathy.html", data)
}

// handleTelepathyEvents serves the cross-agent thinking feed as Server-Sent
// Events. Each event is a single pre-rendered HTML row (see renderThinkingRow)
// the client script prepends into the lane — same idiom as the per-agent
// activity lane (agent_events.go), so no htmx-sse extension is needed.
//
// The hub's Subscribe backfills its recent ring (oldest-first) before going
// live, so a freshly-connected browser paints immediately instead of waiting
// for the next agent to think. A nil hub (NATS not configured) yields an
// already-closed channel — the handler emits a "feed disabled" comment and
// returns, which keeps the browser EventSource in onopen rather than a
// reconnect loop.
func (s *Server) handleTelepathyEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher.Flush()

	if s.thinking == nil {
		fmt.Fprintf(w, ": telepathy feed disabled (NATS not configured)\n\n")
		flusher.Flush()
		return
	}

	ch, cancel := s.thinking.SubscribeFor(r.Context(), viewerScopeFromContext(r.Context()))
	defer cancel()

	// Push a comment line so intermediate proxies (Traefik) flush the header
	// chunk and the browser EventSource fires onopen.
	fmt.Fprintf(w, ": connected\n\n")
	flusher.Flush()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-ch:
			if !ok {
				return
			}
			s.writeThinkingEvent(w, flusher, evt)
		}
	}
}

// writeThinkingEvent renders one event and writes it as an SSE `thinking`
// message. A thought can span many lines, so the rendered row may contain
// newlines (inside the .telepathy-text span). We MUST NOT flatten them — the
// operator has to see the ENTIRE thought with its structure intact (the row's
// .telepathy-text uses white-space:pre-wrap). SSE expresses a multi-line
// payload by repeating the `data:` field: the browser's EventSource rejoins
// the values with "\n", so splitting the row on newlines and emitting one
// `data:` line each round-trips the original text exactly. A single-line row
// emits one `data:` line — byte-for-byte identical to the prior behaviour for
// short thoughts.
func (s *Server) writeThinkingEvent(w http.ResponseWriter, flusher http.Flusher, evt ThinkingEvent) {
	row := renderThinkingRow(s.basePath, evt)
	row = strings.ReplaceAll(row, "\r", "")
	fmt.Fprint(w, "event: thinking\n")
	for line := range strings.SplitSeq(row, "\n") {
		fmt.Fprintf(w, "data: %s\n", line)
	}
	fmt.Fprint(w, "\n")
	flusher.Flush()
}

// renderThinkingRow produces the HTML for one row in the telepathy feed. Kept
// inline (not templated) so the SSE writer can drop the result straight into a
// `data:` line, mirroring renderAgentEventRow. Each row is colour-keyed per
// agent so an operator can track one agent's stream of consciousness at a
// glance.
//
// The display timestamp uses ReceivedAt (the hub's viewer-side receive time),
// which is monotonic across agents even when producer clocks skew; it falls
// back to the producer TS (unix millis) and finally to now.
func renderThinkingRow(basePath string, evt ThinkingEvent) string {
	name := telepathyDisplayName(evt)
	ts := thinkingEventTime(evt)

	// Deep-link to the agent's own bead page when known; otherwise to /agents.
	href := basePath + "/agents"
	if evt.AgentBeadID != "" {
		href = basePath + "/" + evt.AgentBeadID
	}

	var sb strings.Builder
	sb.WriteString(`<div class="telepathy-event" data-agent="`)
	sb.WriteString(html.EscapeString(name))
	sb.WriteString(`" style="--agent-hue:`)
	sb.WriteString(strconv.Itoa(agentHue(name)))
	sb.WriteString(`">`)

	sb.WriteString(`<time class="telepathy-time" datetime="`)
	sb.WriteString(ts.UTC().Format(time.RFC3339Nano))
	sb.WriteString(`">`)
	sb.WriteString(ts.UTC().Format("15:04:05"))
	sb.WriteString(`</time>`)

	// Agent chip.
	sb.WriteString(`<a class="telepathy-agent" href="`)
	sb.WriteString(html.EscapeString(href))
	sb.WriteString(`" title="`)
	sb.WriteString(html.EscapeString(name))
	if evt.PodName != "" {
		sb.WriteString(html.EscapeString(" · " + evt.PodName))
	}
	sb.WriteString(`">`)
	sb.WriteString(html.EscapeString(name))
	sb.WriteString(`</a>`)

	if evt.Role != "" {
		sb.WriteString(`<span class="telepathy-role">`)
		sb.WriteString(html.EscapeString(evt.Role))
		sb.WriteString(`</span>`)
	}
	if evt.Project != "" {
		sb.WriteString(`<span class="telepathy-project">`)
		sb.WriteString(html.EscapeString(evt.Project))
		sb.WriteString(`</span>`)
	}

	sb.WriteString(`<span class="telepathy-text">`)
	text := strings.TrimSpace(evt.Text)
	if text == "" {
		// The hub drops signature-only empties, so this is belt-and-braces.
		sb.WriteString(`<em class="telepathy-empty">(thinking — no plaintext captured)</em>`)
	} else {
		sb.WriteString(html.EscapeString(text))
	}
	sb.WriteString(`</span>`)

	// Surface a "+N chars" hint when the trace was clipped before forwarding
	// (coop truncates to MAX_TEXT_BYTES; FullLength carries the original size).
	if evt.FullLength > len(evt.Text) && len(evt.Text) > 0 {
		sb.WriteString(`<span class="telepathy-clip">+`)
		sb.WriteString(strconv.Itoa(evt.FullLength - len(evt.Text)))
		sb.WriteString(` chars</span>`)
	}

	sb.WriteString(`</div>`)
	return sb.String()
}

// thinkingEventTime picks the display timestamp for an event: the hub's
// viewer-side ReceivedAt when set (monotonic across producers), then the
// producer-stamped TS (unix millis), then wall-clock now.
func thinkingEventTime(evt ThinkingEvent) time.Time {
	if !evt.ReceivedAt.IsZero() {
		return evt.ReceivedAt
	}
	if evt.TS > 0 {
		return time.UnixMilli(evt.TS).UTC()
	}
	return time.Now()
}

// agentHue maps an agent name onto a stable hue (0–359) for per-agent colour
// coding. FNV-1a keeps the same agent the same colour across reconnects and
// across clients without any server-side palette state.
func agentHue(name string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	return int(h.Sum32() % 360)
}
