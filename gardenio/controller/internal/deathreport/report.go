package deathreport

import (
	"fmt"
	"io"
	"strings"
	"time"

	"gardenio/controller/internal/deathtranscript"
)

// Report is the complete "why did this agent die" forensic report for one
// involuntarily-dead agent. It is the single value the CLI renders (table or
// JSON) and that a controller-side auto-forensics caller can post.
type Report struct {
	// AgentBeadID / AgentName identify the dead agent (best-effort; may be empty
	// when the caller had only a transcript).
	AgentBeadID string `json:"agent_bead_id,omitempty"`
	AgentName   string `json:"agent_name,omitempty"`
	// CloseReason is the raw close_reason recorded on the dead agent bead.
	CloseReason string `json:"close_reason,omitempty"`
	// TranscriptURL is the location of the FULL transcript the last-N view was
	// distilled from — an `s3://…` URI when the recording was pulled from the
	// coop session bucket, so an operator can fetch the complete recording
	// (auto-forensics acceptance §3, epic ka-0000005447). Empty when the report
	// rested on a local log or on the close_reason alone.
	TranscriptURL string `json:"transcript_url,omitempty"`
	// Classification is the death-family verdict derived from CloseReason.
	Classification Classification `json:"classification"`
	// LastN is the number of trailing turns requested.
	LastN int `json:"last_n"`
	// Transcript is the distilled last-N-turns view. Nil when no transcript was
	// available (the report then rests on the close_reason classification alone).
	Transcript *Transcript `json:"transcript,omitempty"`
	// Cadence is the Stop-hook-cadence-vs-cooldown-math diff. Present whenever a
	// transcript was parsed (even with zero blocks, so the report can positively
	// state "no cooldown-timing kill implicated").
	Cadence *CadenceDiff `json:"cadence,omitempty"`
	// Headline is a one-line human summary suitable for a Slack warning: names
	// the agent and states the cause.
	Headline string `json:"headline"`
	// WipReapRef / WipReapComment surface Tier-3 reap-scratch preservation
	// (gd-PF42yiyH1d): the scratch ref(s) `gb salvage` force-pushed to
	// refs/gardenio/wip/... and/or the target(s) whose diff it attached as a
	// bead comment when the ref push was not possible. Populated from the dead
	// agent bead's wip_reap_ref / wip_reap_comment fields; both empty when the
	// tree was clean at reap time.
	WipReapRef     string `json:"wip_reap_ref,omitempty"`
	WipReapComment string `json:"wip_reap_comment,omitempty"`
}

// Options configures report construction.
type Options struct {
	// AgentBeadID / AgentName identify the dead agent for the headline.
	AgentBeadID string
	AgentName   string
	// CloseReason is the dead bead's close_reason (drives classification).
	CloseReason string
	// LastN is the number of trailing turns to summarize (<=0 → DefaultLastN).
	LastN int
	// BaseCooldownSecs is the stop-gate base cooldown to diff cadence against
	// (<=0 → StopGateDefaultCooldownSecs). Pass the STOP_GATE_COOLDOWN_SECS
	// override when reconstructing a pod that ran with a non-default base.
	BaseCooldownSecs int
	// TranscriptURL is the location of the transcript being summarized (an
	// `s3://…` URI for a pulled recording). Carried through to Report.TranscriptURL
	// so the rendered report can link the full recording (acceptance §3). Purely
	// informational — it does not affect parsing.
	TranscriptURL string
	// Events, when non-nil, replaces the transcript reader: the turns projected
	// from the harness-neutral coop event logs (deathtranscript.ParseEvents) of a
	// Codex agent, which has no Claude-shaped JSONL to parse.
	Events []deathtranscript.Turn
	// Fields is the dead agent bead's Fields map, when the report was built from a
	// bead. It lets the classifier correct a generic context_cancelled close_reason
	// with the credential-failure evidence (error_category / never-authenticated
	// token shape) that was on the bead the whole time — see ClassifyWithFields
	// (ka-0000002985 DEFECT 1). Nil for a transcript-only report; the classifier
	// then rests on the close_reason alone.
	Fields map[string]string
}

// Build assembles a Report from a transcript reader and options. The transcript
// reader may be nil (close_reason-only report). Any transcript parse error is
// returned; a nil reader is not an error.
func Build(transcript io.Reader, opts Options) (*Report, error) {
	lastN := opts.LastN
	if lastN <= 0 {
		lastN = DefaultLastN
	}

	rep := &Report{
		AgentBeadID:    opts.AgentBeadID,
		AgentName:      opts.AgentName,
		CloseReason:    opts.CloseReason,
		TranscriptURL:  opts.TranscriptURL,
		Classification: ClassifyWithFields(opts.CloseReason, opts.Fields),
		LastN:          lastN,
	}
	if opts.Fields != nil {
		rep.WipReapRef = opts.Fields["wip_reap_ref"]
		rep.WipReapComment = opts.Fields["wip_reap_comment"]
	}

	if opts.Events != nil {
		tr := TranscriptFromTurns(opts.Events, lastN)
		rep.Transcript = tr
		cadence := StopHookCadenceDiff(tr.BlockTimestamps, opts.BaseCooldownSecs)
		rep.Cadence = &cadence
	} else if transcript != nil {
		tr, err := ParseTranscript(transcript, lastN)
		if err != nil {
			return nil, fmt.Errorf("parsing transcript: %w", err)
		}
		rep.Transcript = tr
		cadence := StopHookCadenceDiff(tr.BlockTimestamps, opts.BaseCooldownSecs)
		rep.Cadence = &cadence
	}

	rep.Headline = buildHeadline(rep)
	return rep, nil
}

// buildHeadline renders the one-line Slack-ready summary.
func buildHeadline(rep *Report) string {
	name := rep.AgentName
	if name == "" {
		name = rep.AgentBeadID
	}
	if name == "" {
		name = "agent"
	}
	return fmt.Sprintf("%s died (%s): %s", name, rep.Classification.Family, rep.Classification.Summary)
}

// RenderText renders a human-readable, terminal-friendly report.
func (rep *Report) RenderText(w io.Writer) {
	p := func(format string, a ...any) { fmt.Fprintf(w, format, a...) }

	p("=== Why did this agent die? ===\n\n")
	if rep.AgentName != "" || rep.AgentBeadID != "" {
		p("  Agent:            %s", rep.AgentName)
		if rep.AgentBeadID != "" {
			p("  (%s)", rep.AgentBeadID)
		}
		p("\n")
	}

	// Death family / classification.
	p("  Death family:     %s\n", rep.Classification.Family)
	p("  Involuntary:      %t", rep.Classification.Involuntary)
	if rep.Classification.MetricFamily != "" {
		p("  (metric family: %s)", rep.Classification.MetricFamily)
	}
	p("\n")
	p("  Cause:            %s\n", rep.Classification.Summary)

	// Last words / close_reason — quoted verbatim (acceptance criterion).
	p("\n  Close reason (quoted):\n")
	p("    %q\n", rep.CloseReason)
	if rep.Transcript != nil && rep.Transcript.LastWords != "" {
		p("\n  Last words (final assistant turn, quoted):\n")
		for line := range strings.SplitSeq(rep.Transcript.LastWords, "\n") {
			p("    │ %s\n", line)
		}
	}

	if rep.Transcript != nil && rep.Transcript.LastCommand != "" {
		p("\n  Last command (final tool call):\n")
		p("    $ %s\n", firstLine(rep.Transcript.LastCommand, 200))
	}

	// Full-recording link (acceptance §3): where the last-N view was distilled
	// from, so an operator can pull the complete transcript.
	if rep.TranscriptURL != "" {
		p("\n  Full transcript:  %s\n", rep.TranscriptURL)
	}

	// Tier-3 reap-scratch preservation (gd-PF42yiyH1d).
	if rep.WipReapRef != "" || rep.WipReapComment != "" {
		p("\n  Uncommitted work preserved on reap:\n")
		if rep.WipReapRef != "" {
			p("    scratch ref(s):        %s\n", rep.WipReapRef)
		}
		if rep.WipReapComment != "" {
			p("    diff attached as comment for: %s\n", rep.WipReapComment)
		}
	}

	// Last-N-turns summary.
	if rep.Transcript != nil {
		tr := rep.Transcript
		p("\n  Last %d of %d turn(s)", len(tr.Turns), tr.TotalTurns)
		if !tr.FirstTimestamp.IsZero() && !tr.LastTimestamp.IsZero() {
			p(" over %s", tr.LastTimestamp.Sub(tr.FirstTimestamp).Round(time.Second))
		}
		p(":\n")
		for _, turn := range tr.Turns {
			renderTurn(p, turn)
		}
	}

	// Stop-hook cadence vs cooldown math diff.
	if rep.Cadence != nil {
		renderCadence(p, rep.Cadence)
	}

	p("\n  ► %s\n", rep.Headline)
}

func renderTurn(p func(string, ...any), turn Turn) {
	marker := "·"
	if turn.QuietTurnEnd {
		marker = "◇" // quiet turn-end (no tool calls) — the stop-gate-tripping shape
	}
	ts := ""
	if !turn.Timestamp.IsZero() {
		ts = turn.Timestamp.Format("15:04:05") + " "
	}
	tools := "quiet turn-end (no tools)"
	if len(turn.ToolCalls) > 0 {
		tools = fmt.Sprintf("tools: %s", strings.Join(turn.ToolCalls, ", "))
	}
	p("    %s [%02d] %s%s\n", marker, turn.Index, ts, tools)
	if snippet := firstLine(turn.Text, 100); snippet != "" {
		p("         “%s”\n", snippet)
	}
	for _, c := range turn.Commands {
		if c = firstLine(c, 100); c != "" {
			p("         $ %s\n", c)
		}
	}
}

func renderCadence(p func(string, ...any), c *CadenceDiff) {
	p("\n  Stop-hook cadence vs cooldown math (base %ds):\n", c.BaseCooldownSecs)
	if len(c.Events) == 0 {
		p("    (no stop-gate blocks observed)\n")
	}
	for _, ev := range c.Events {
		if ev.BlockNumber == 1 {
			p("    block #%d  (first — no prior window)\n", ev.BlockNumber)
			continue
		}
		verdict := "survives (gap ≥ window)"
		if ev.InCooldown {
			verdict = "◄ IN COOLDOWN → force-exit"
		}
		p("    block #%d  gap %3ds  vs window %3ds  %s\n",
			ev.BlockNumber, ev.GapSecs, ev.CooldownWindowSecs, verdict)
	}
	p("    ⇒ %s\n", c.Anomaly)
}

// firstLine returns the first non-empty line of s, truncated to max runes with
// an ellipsis when longer. Used to keep the per-turn snippet to one tidy line.
func firstLine(s string, max int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	r := []rune(s)
	if len(r) > max {
		return string(r[:max-1]) + "…"
	}
	return string(r)
}
