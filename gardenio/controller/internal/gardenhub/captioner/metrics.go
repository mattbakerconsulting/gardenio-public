package captioner

import "github.com/prometheus/client_golang/prometheus"

// The caption sources (spec §5.2), the src label of gardenio_farm_caption_total. The hub counts the deterministic
// ones with Count; the Captioner counts haiku, cache and none for the calls it is asked about.
const (
	SrcDescription = "description"
	SrcPath        = "path"
	SrcPattern     = "pattern"
	SrcHost        = "host"
	SrcAgent       = "agent"
	SrcBuiltin     = "builtin"
	SrcMCP         = "mcp"
	SrcHaiku       = "haiku"
	SrcCache       = "cache"
	SrcNone        = "none"
)

// Why a Haiku call was not made or did not answer: the why label of gardenio_farm_caption_haiku_skipped_total.
const (
	SkipRate        = "rate"
	SkipConcurrency = "concurrency"
	SkipDaily       = "daily"
	SkipOff         = "off"
	SkipTimeout     = "timeout"
	SkipError       = "error"
	SkipPaused      = "paused" // in the pause a run of failures set (breakFailures)
)

var (
	sources = []string{SrcDescription, SrcPath, SrcPattern, SrcHost, SrcAgent, SrcBuiltin, SrcMCP, SrcHaiku, SrcCache, SrcNone}
	skips   = []string{SkipRate, SkipConcurrency, SkipDaily, SkipOff, SkipTimeout, SkipError}
)

// Metrics are the caption counters (spec §8). It is a prometheus.Collector: beads-viewer registers it.
type Metrics struct {
	total   *prometheus.CounterVec
	blank   *prometheus.CounterVec
	seconds prometheus.Histogram
	skipped *prometheus.CounterVec
	tokenN  *prometheus.CounterVec
}

func newMetrics() *Metrics {
	m := &Metrics{
		total: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gardenio_farm_caption_total",
			Help: "Bird captions made, by source (description, path, pattern, host, agent, builtin, mcp, haiku, cache, none).",
		}, []string{"src"}),
		blank: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gardenio_farm_uncaptioned_total",
			Help: "Call starts the hub could not caption from their input, before the Haiku fallback, by tool (a tool the hub knows, mcp or other).",
		}, []string{"tool"}),
		seconds: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "gardenio_farm_caption_haiku_seconds",
			Help:    "How long a Haiku caption call took, answered or not.",
			Buckets: []float64{0.1, 0.25, 0.5, 0.75, 1, 1.5, 2, 3, 5},
		}),
		skipped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gardenio_farm_caption_haiku_skipped_total",
			Help: "Haiku caption calls not made (rate, concurrency, daily, off, paused) or not answered (timeout, error).",
		}, []string{"why"}),
		tokenN: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gardenio_farm_caption_haiku_tokens_total",
			Help: "Tokens the Haiku caption calls' replies said they cost, by type (input, output): the captions' spend.",
		}, []string{"type"}),
	}
	m.tokenN.WithLabelValues("input")
	m.tokenN.WithLabelValues("output")
	for _, s := range sources {
		m.total.WithLabelValues(s)
	}
	for _, s := range skips {
		m.skipped.WithLabelValues(s)
	}
	return m
}

// count adds one caption of src; an unknown src counts as none, so the label stays bounded.
func (m *Metrics) count(src string) {
	for _, s := range sources {
		if s == src {
			m.total.WithLabelValues(src).Inc()
			return
		}
	}
	m.total.WithLabelValues(SrcNone).Inc()
}

// uncaptioned adds one call start with no caption of its own; the caller bounds tool.
func (m *Metrics) uncaptioned(tool string) { m.blank.WithLabelValues(tool).Inc() }

func (m *Metrics) skip(why string) { m.skipped.WithLabelValues(why).Inc() }

// tokens adds a reply's usage.
func (m *Metrics) tokens(u Usage) {
	if u.InputTokens > 0 {
		m.tokenN.WithLabelValues("input").Add(float64(u.InputTokens))
	}
	if u.OutputTokens > 0 {
		m.tokenN.WithLabelValues("output").Add(float64(u.OutputTokens))
	}
}

func (m *Metrics) Describe(ch chan<- *prometheus.Desc) {
	m.total.Describe(ch)
	m.blank.Describe(ch)
	m.seconds.Describe(ch)
	m.skipped.Describe(ch)
	m.tokenN.Describe(ch)
}

func (m *Metrics) Collect(ch chan<- prometheus.Metric) {
	m.total.Collect(ch)
	m.blank.Collect(ch)
	m.seconds.Collect(ch)
	m.skipped.Collect(ch)
	m.tokenN.Collect(ch)
}
