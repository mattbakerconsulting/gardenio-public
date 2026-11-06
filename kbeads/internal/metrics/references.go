package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Reference source-field label values. Exact field names map to source_field
// values stored in the bead_references table. Comment rows use the
// "comment:<id>" form per design § 1, but are reported here under the bucket
// label "comment" to keep cardinality bounded.
const (
	RefSourceTitle       = "title"
	RefSourceDescription = "description"
	RefSourceNotes       = "notes"
	RefSourceFields      = "fields"
	RefSourceComment     = "comment"
)

var (
	// ReferencesExtracted counts unique bead-id references emitted into
	// bead_references on every write, broken down by source field.
	ReferencesExtracted = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gardenio_beads_references_extracted_total",
			Help: "Count of bead-id references extracted and persisted, by source field.",
		},
		[]string{"source_field"},
	)

	// ReferencesExtractDuration measures the wall time spent extracting and
	// replacing bead_references rows for one mutation (CreateBead/UpdateBead/
	// AddComment). Includes the regex pass and the DELETE+INSERT round trip.
	ReferencesExtractDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "gardenio_beads_references_extract_duration_seconds",
			Help:    "Wall time spent extracting bead-id references and replacing rows in bead_references.",
			Buckets: prometheus.ExponentialBuckets(0.0001, 2, 14), // 100us .. ~1.6s
		},
	)
)

// RegisterReferences registers the reference-extraction metrics on the given
// registry. Safe to call once per registry.
func RegisterReferences(reg prometheus.Registerer) {
	reg.MustRegister(ReferencesExtracted, ReferencesExtractDuration)
}

// ObserveReferencesExtract records the wall time of a reference-extraction
// pass and bumps the per-source_field counter by the number of references
// emitted for each field. counts is keyed by the metric label value
// (RefSourceTitle/RefSourceDescription/RefSourceNotes/RefSourceFields/RefSourceComment).
func ObserveReferencesExtract(start time.Time, counts map[string]int) {
	ReferencesExtractDuration.Observe(time.Since(start).Seconds())
	for field, n := range counts {
		if n <= 0 {
			continue
		}
		ReferencesExtracted.WithLabelValues(field).Add(float64(n))
	}
}
