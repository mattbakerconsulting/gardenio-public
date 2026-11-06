package tests

import (
	"strings"
	"testing"
)

// The gitlab-bridge VMServiceScrape (bead ka-0000001045).
//
// Both sibling bridges shipped a VMServiceScrape; gitlab-bridge did not, so on
// the prod vm-operator stack — which discovers scrape targets from
// VMServiceScrape objects, NOT from the Deployment's prometheus.io/* pod
// annotations — the GitLab webhook door was never scraped. Every
// gardenio_gitlab_webhook_* series was absent from the TSDB, which made the two
// webhook-liveness panels keyed on it permanently empty and left the shipped
// GitLabWebhookDark deadman alert unable to fire from that pod.
//
// The decoy that hid this for so long: gardenio_gitlab_webhook_dark resolves in
// prod, but only from the jira/slack bridge pods where the gauge is registered
// globally and always reads 0 — so the gap did not look like a gap. These tests
// pin the scrape object itself rather than any query that references it.

func TestGitLabBridgeVMServiceScrape_DefaultDisabled(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "templates/gitlab-bridge/vmservicescrape.yaml",
		"gitlabBridge.enabled=true",
	)
	if err == nil && strings.Contains(out, "kind: VMServiceScrape") {
		t.Errorf("VMServiceScrape must NOT render when victoriaMetrics.enabled=false, got:\n%s", out)
	}
}

func TestGitLabBridgeVMServiceScrape_RequiresBridgeEnabled(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "templates/gitlab-bridge/vmservicescrape.yaml",
		"monitoring.victoriaMetrics.enabled=true",
	)
	if err == nil && strings.Contains(out, "kind: VMServiceScrape") {
		t.Errorf("VMServiceScrape must NOT render when gitlabBridge is disabled, got:\n%s", out)
	}
}

func TestGitLabBridgeVMServiceScrape_EnabledRenders(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/gitlab-bridge/vmservicescrape.yaml",
		"gitlabBridge.enabled=true",
		"monitoring.victoriaMetrics.enabled=true",
	)
	if !strings.Contains(out, "kind: VMServiceScrape") {
		t.Fatalf("VMServiceScrape must render when victoriaMetrics enabled, got:\n%s", out)
	}
	for _, want := range []string{
		"name: test-gitlab-bridge",
		"app.kubernetes.io/component: gitlab-bridge",
		"port: metrics",
		"path: /metrics",
		"interval: 30s",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("VMServiceScrape must contain %q, got:\n%s", want, out)
		}
	}
}

func TestGitLabBridgeVMServiceScrape_IntervalOverride(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/gitlab-bridge/vmservicescrape.yaml",
		"gitlabBridge.enabled=true",
		"monitoring.victoriaMetrics.enabled=true",
		"gitlabBridge.metrics.serviceMonitor.interval=15s",
	)
	if !strings.Contains(out, "interval: 15s") {
		t.Errorf("per-bridge interval override must win, got:\n%s", out)
	}
}

// The scrape selector must match the Service the metrics port actually lives
// on: a selector that matches nothing renders and lints clean but collects
// nothing — which is exactly the failure mode this bead is about.
func TestGitLabBridgeVMServiceScrape_SelectorMatchesService(t *testing.T) {
	t.Parallel()
	scrape := helmTemplate(t, "templates/gitlab-bridge/vmservicescrape.yaml",
		"gitlabBridge.enabled=true",
		"monitoring.victoriaMetrics.enabled=true",
	)
	svc := helmTemplate(t, "templates/gitlab-bridge/service.yaml",
		"gitlabBridge.enabled=true",
	)
	// The Service must expose a port literally named "metrics" — the scrape
	// endpoint references it by name.
	if !strings.Contains(svc, "name: metrics") {
		t.Fatalf("gitlab-bridge Service must expose a port named metrics, got:\n%s", svc)
	}
	// Every selector label the scrape requires must be present on the Service.
	for _, line := range strings.Split(scrape, "\n") {
		l := strings.TrimSpace(line)
		if !strings.HasPrefix(l, "app.kubernetes.io/") && !strings.HasPrefix(l, "helm.sh/") {
			continue
		}
		// selectorLabels only — instance/name/component; skip chart-level
		// labels that live on metadata but not on the Service selector.
		if !strings.HasPrefix(l, "app.kubernetes.io/name:") &&
			!strings.HasPrefix(l, "app.kubernetes.io/instance:") &&
			!strings.HasPrefix(l, "app.kubernetes.io/component:") {
			continue
		}
		if !strings.Contains(svc, l) {
			t.Errorf("scrape selector label %q is absent from the gitlab-bridge Service selector:\n%s", l, svc)
		}
	}
}
