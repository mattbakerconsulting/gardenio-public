// Prometheus metrics for the release-drought deadman watchdog
// (release_drought.go, ka-0000002594).
//
// Registered globally against the default registerer (promauto), served by
// whichever binary hosts the watchdog — gitlab-bridge's internal :9092
// /metrics endpoint in the standard deployment (scraped via the pod
// annotations on the gitlab-bridge Deployment).
//
// These gauges are the watchdog's third alert leg: a PrometheusRule
// (helm/gardenio/templates/gitlab-bridge/release-drought-alert.yaml) can fire
// off `gardenio_release_drought_active == 1` with zero dependency on the beads
// daemon or Slack, and an absent()/staleness rule over
// `gardenio_release_drought_last_check_timestamp_seconds` watches the watchdog
// itself.
package bridge

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var releaseDroughtActive = promauto.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "gardenio_release_drought_active",
		Help: "1 while the oldest unreleased calver tag has waited longer than the drought threshold, else 0.",
	},
	[]string{"project"},
)

var releaseDroughtOldestUnreleasedAge = promauto.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "gardenio_release_oldest_unreleased_tag_age_seconds",
		Help: "Age of the oldest calver tag newer than the latest GitLab Release; 0 when every calver tag is released.",
	},
	[]string{"project"},
)

var releaseDroughtUnreleasedTags = promauto.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "gardenio_release_unreleased_tags",
		Help: "Count of calver tags newer than the latest GitLab Release (tags awaiting a successful release).",
	},
	[]string{"project"},
)

var releaseDroughtLastReleaseTS = promauto.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "gardenio_release_last_release_timestamp_seconds",
		Help: "released_at of the newest non-upcoming GitLab Release, as a Unix timestamp.",
	},
	[]string{"project"},
)

var releaseDroughtLastCheckTS = promauto.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "gardenio_release_drought_last_check_timestamp_seconds",
		Help: "Unix timestamp of the watchdog's last successful assessment pass (deadman signal for the watchdog itself).",
	},
	[]string{"project"},
)

var releaseDroughtCheckErrors = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "gardenio_release_drought_check_errors_total",
		Help: "GitLab API failures during release-drought watchdog polls.",
	},
	[]string{"project"},
)
