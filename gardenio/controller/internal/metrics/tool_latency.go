package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

// ToolLatencyMetrics holds the bounded metric-plane histograms for the P2
// HOOK_EVENTS consumer (plan gardenio/plans/agent-tool-latency-attribution.md
// §P2, bead gd-sWwPnwkZtw). The consumer derives two series per PostToolUse
// event:
//
//   - gardenio_tool_latency_seconds{tool,project,harness,source,env}: one sample per
//     tool call, regardless of tool.
//   - gardenio_bash_cmd_latency_seconds{cmd_class,project,harness,source,env}: an
//     ADDITIONAL sample emitted only for Bash calls, broken out by the
//     normalized command class (plan §4.1) so "git push was slow today" is a
//     dashboard query rather than a ledger grep.
//
// source is "claude" when Claude's own duration_ms accompanied the event, or
// "derived" when the consumer computed the duration itself by pairing
// PreToolUse/PostToolUse via tool_use_id (duration_ms/duration_source
// semantics landed in P0).
//
// Label cardinality is bounded by construction, not by caller discipline:
// tool is a short fixed Claude tool-name set, cmd_class is capped to the
// §4.1 allowlist-derived domain (~40 values, enforced in
// internal/toollatency/cmdclass.go), project is the small number of gardenio
// projects, harness is the 3-value agent CLI enum (claude|codex|gemini, ADR 0004/0005),
// source is 2 values. Combined this keeps total new series well
// under the <2,000 acceptance ceiling.
type ToolLatencyMetrics struct {
	// env is baked in at construction (GARDENIO_ENV), the cross-estate join
	// key every gardenio series carries.
	env string

	toolLatency    *prometheus.HistogramVec
	bashCmdLatency *prometheus.HistogramVec
}

// Harness label values — the agent CLI that produced the hook event (ADR 0004
// §4). Anything else (an old gb that predates the field, a future harness the
// controller has not learned) collapses to HarnessUnknown so the label stays a
// bounded enum rather than echoing a payload string.
const (
	HarnessClaude  = "claude"
	HarnessCodex   = "codex"
	HarnessGemini  = "gemini"
	HarnessUnknown = "unknown"
)

// normalizeHarness maps a wire harness string onto the bounded label enum. An
// empty value is "claude": every hook relay that predates the harness field was
// a Claude pod.
func normalizeHarness(h string) string {
	switch h {
	case "", HarnessClaude:
		return HarnessClaude
	case HarnessCodex:
		return HarnessCodex
	case HarnessGemini:
		return HarnessGemini
	default:
		return HarnessUnknown
	}
}

// Tool-latency source enum — the only accepted `source` label values.
const (
	// ToolLatencySourceClaude: Claude's own native duration_ms accompanied
	// the PostToolUse event.
	ToolLatencySourceClaude = "claude"
	// ToolLatencySourceDerived: the consumer computed the duration itself by
	// pairing PreToolUse/PostToolUse via tool_use_id.
	ToolLatencySourceDerived = "derived"
)

func toolLatencySourceValid(s string) bool {
	switch s {
	case ToolLatencySourceClaude, ToolLatencySourceDerived:
		return true
	default:
		return false
	}
}

// toolLatencyBuckets covers sub-second tool calls (a Read/Grep) through the
// multi-minute tail a slow build or network-bound Bash command can hit. The
// P2 ledger (slow_tool_calls) exists precisely because this histogram's tail
// buckets are coarse — anything past ~2s is also recorded as a full row with
// the redacted argv, so "which exact call" never has to come from a bucket
// boundary.
var toolLatencyBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 20, 30, 60, 120, 300}

// NewToolLatencyMetrics constructs the tool-latency histograms. env is
// attached to every series as the cross-estate join key. Register() must be
// called before the metric is served.
func NewToolLatencyMetrics(env string) *ToolLatencyMetrics {
	return &ToolLatencyMetrics{
		env: env,
		toolLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "tool_latency_seconds",
			Help:      "Agent tool-call latency in seconds, one sample per PostToolUse event. Labels {tool,project,harness,source,env}; harness=claude|codex|gemini|unknown (the agent CLI, ADR 0004); source=claude|derived (P0 duration_ms/duration_source: Claude's own timing vs. consumer-paired PreToolUse/PostToolUse via tool_use_id). Calls over the slow-call threshold are also recorded with full redacted argv in the slow_tool_calls ledger (P2, gd-sWwPnwkZtw) — this histogram is the bounded metric plane, the ledger is the record plane.",
			Buckets:   toolLatencyBuckets,
		}, []string{"tool", "project", "harness", "source", "env"}),
		bashCmdLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "bash_cmd_latency_seconds",
			Help:      "Agent Bash tool-call latency in seconds, broken out by normalized command class (plan §4.1: strip leading `cd ... &&`/`VAR=...` prefixes, binary basename, allowlisted subcommand or \"other\"). Labels {cmd_class,project,harness,source,env}; harness=claude|codex|gemini|unknown; source=claude|derived. Emitted in addition to tool_latency_seconds for Bash calls only.",
			Buckets:   toolLatencyBuckets,
		}, []string{"cmd_class", "project", "harness", "source", "env"}),
	}
}

// Register registers the tool-latency histograms with reg.
func (m *ToolLatencyMetrics) Register(reg prometheus.Registerer) error {
	if m == nil {
		return nil
	}
	if err := reg.Register(m.toolLatency); err != nil {
		return err
	}
	return reg.Register(m.bashCmdLatency)
}

// RecordToolLatency observes one tool call in seconds. A negative duration or
// an out-of-enum source is DROPPED (returns false) so cardinality and
// correctness stay bounded. An empty tool or project collapses to "unknown"; harness is normalized to claude|codex|gemini|unknown (empty = claude).
// A nil receiver is a no-op.
func (m *ToolLatencyMetrics) RecordToolLatency(seconds float64, tool, project, harness, source string) bool {
	if m == nil {
		return false
	}
	if seconds < 0 {
		return false
	}
	if !toolLatencySourceValid(source) {
		return false
	}
	if tool == "" {
		tool = "unknown"
	}
	if project == "" {
		project = "unknown"
	}
	m.toolLatency.WithLabelValues(tool, project, normalizeHarness(harness), source, m.env).Observe(seconds)
	return true
}

// RecordBashCmdLatency observes one Bash call in seconds under its normalized
// cmd_class. A negative duration, empty cmd_class, or out-of-enum source is
// DROPPED (returns false) — cmdClass is expected to already be normalized and
// bounded by internal/toollatency.ClassifyCmd before this is called, so an
// empty value here signals a caller bug rather than a legitimate class. An
// empty project collapses to "unknown". A nil receiver is a no-op.
func (m *ToolLatencyMetrics) RecordBashCmdLatency(seconds float64, cmdClass, project, harness, source string) bool {
	if m == nil {
		return false
	}
	if seconds < 0 || cmdClass == "" {
		return false
	}
	if !toolLatencySourceValid(source) {
		return false
	}
	if project == "" {
		project = "unknown"
	}
	m.bashCmdLatency.WithLabelValues(cmdClass, project, normalizeHarness(harness), source, m.env).Observe(seconds)
	return true
}
