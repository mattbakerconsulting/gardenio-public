package tests

import (
	"strings"
	"testing"
)

// Regression guards for bug ka-0000008129 — scheduled embedding backfill.
//
// Coverage (gardenio_beads_embedding_coverage_ratio) drifts down as new/edited
// beads outpace the write-path embedder; a recurring `kd embed sync
// --only-missing` CronJob drains the backlog so coverage stays >= the SLO and
// stale_count trends to 0. The CronJob reuses the kbeads image + the daemon's
// admin token, and — because the namespace runs default-deny-egress and the
// beads daemon has a scoped ingress — MUST also open a NetworkPolicy egress
// (beads:8080 + kube-dns) and a beads-ingress `from` entry, else every run is
// silently network-denied.

const (
	backfillCronTemplate    = "templates/beads/embedding-backfill-cronjob.yaml"
	backfillEgressTemplate  = "templates/networkpolicies/default-deny-egress.yaml"
	backfillIngressTemplate = "templates/networkpolicies/networkpolicies.yaml"
)

// --- CronJob rendering + gating ---

func TestEmbeddingBackfill_DisabledByDefault(t *testing.T) {
	t.Parallel()
	// Embedding on, backfill off -> nothing renders.
	out, err := helmTemplateAllowFail(t, backfillCronTemplate,
		"beads.embedding.enabled=true",
	)
	rendersNothing(t, out, err)
}

func TestEmbeddingBackfill_RequiresEmbeddingEnabled(t *testing.T) {
	t.Parallel()
	// backfill flag on but embedding off -> nothing renders (a backfill with
	// no server-side backend would just 503 every run).
	out, err := helmTemplateAllowFail(t, backfillCronTemplate,
		"beads.embedding.backfill.enabled=true",
	)
	rendersNothing(t, out, err)
}

func TestEmbeddingBackfill_EnabledRendersCronJob(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, backfillCronTemplate,
		"beads.embedding.enabled=true",
		"beads.embedding.backfill.enabled=true",
	)
	// The shared static admin token is retired (gd-ke6PC7bRto): the Job must not
	// mount it, because the daemon would reject it anyway.
	if strings.Contains(out, "BEADS_AUTH_TOKEN") || strings.Contains(out, "kd-beads-token") {
		t.Errorf("backfill CronJob must not reference the retired static token; got:\n%s", out)
	}
	for _, want := range []string{
		"kind: CronJob",
		"name: test-beads-embedding-backfill",
		"app.kubernetes.io/component: beads-embedding-backfill",
		// Resumable server-side backfill invocation.
		"- kd",
		"- embed",
		"- sync",
		"- --only-missing",
		// Reuses the kbeads image (kd is its entrypoint) — no new image.
		"/kbeads:",
		// Targets the in-cluster daemon over HTTP.
		"- name: BEADS_HTTP_URL",
		`value: "http://test-beads:8080"`,
		// A slow run must be skipped, never stacked.
		"concurrencyPolicy: Forbid",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected rendered backfill CronJob to contain %q; got:\n%s", want, out)
		}
	}
}

func TestEmbeddingBackfill_NoDuplicateComponentLabel(t *testing.T) {
	t.Parallel()
	// The pod/metadata labels must carry exactly one
	// app.kubernetes.io/component key (beads-embedding-backfill) — a duplicate
	// (component: beads AND component: beads-embedding-backfill) is invalid
	// YAML that a strict apiserver rejects.
	out := helmTemplate(t, backfillCronTemplate,
		"beads.embedding.enabled=true",
		"beads.embedding.backfill.enabled=true",
	)
	if strings.Contains(out, "app.kubernetes.io/component: beads\n") {
		t.Errorf("backfill CronJob must not emit the bare component: beads label (duplicate-key risk); got:\n%s", out)
	}
	if n := strings.Count(out, "app.kubernetes.io/component: beads-embedding-backfill"); n < 2 {
		t.Errorf("expected the backfill component label on both metadata and pod template (>=2 occurrences), got %d:\n%s", n, out)
	}
}

func TestEmbeddingBackfill_ScheduleAndBatchOverridable(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, backfillCronTemplate,
		"beads.embedding.enabled=true",
		"beads.embedding.backfill.enabled=true",
		"beads.embedding.backfill.schedule=*/30 * * * *",
		"beads.embedding.backfill.batchSize=250",
	)
	if !strings.Contains(out, `schedule: "*/30 * * * *"`) {
		t.Errorf("expected overridden schedule; got:\n%s", out)
	}
	if !strings.Contains(out, `"250"`) {
		t.Errorf("expected overridden batch-size 250; got:\n%s", out)
	}
}

func TestEmbeddingBackfill_LimitFlagOnlyWhenSet(t *testing.T) {
	t.Parallel()
	// Default limit=0 -> no --limit flag (drain until exhausted).
	def := helmTemplate(t, backfillCronTemplate,
		"beads.embedding.enabled=true",
		"beads.embedding.backfill.enabled=true",
	)
	if strings.Contains(def, "--limit") {
		t.Errorf("expected no --limit flag when limit=0 (drain-until-exhausted); got:\n%s", def)
	}
	// limit>0 -> flag present.
	capped := helmTemplate(t, backfillCronTemplate,
		"beads.embedding.enabled=true",
		"beads.embedding.backfill.enabled=true",
		"beads.embedding.backfill.limit=5000",
	)
	if !strings.Contains(capped, "--limit") || !strings.Contains(capped, `"5000"`) {
		t.Errorf("expected --limit 5000 when limit is set; got:\n%s", capped)
	}
}

func TestEmbeddingBackfill_TypeFilterFallsBackToDeploymentDefault(t *testing.T) {
	t.Parallel()
	// backfill.allowedTypes empty -> inherit embedding.allowedTypes so the cron
	// mirrors interactive `kd embed sync` runs.
	out := helmTemplate(t, backfillCronTemplate,
		"beads.embedding.enabled=true",
		"beads.embedding.backfill.enabled=true",
		"beads.embedding.allowedTypes={task,bug}",
	)
	if !strings.Contains(out, "- name: BEADS_EMBED_TYPES") {
		t.Errorf("expected backfill to inherit embedding.allowedTypes as BEADS_EMBED_TYPES; got:\n%s", out)
	}
	if !strings.Contains(out, "task,bug") {
		t.Errorf("expected inherited allowlist task,bug; got:\n%s", out)
	}
}

// --- NetworkPolicy wiring (the load-bearing part on a default-deny cluster) ---

func TestEmbeddingBackfill_EgressPolicyRendersWhenEnabled(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, backfillEgressTemplate,
		"networkPolicies.enabled=true",
		"networkPolicies.defaultDenyEgress=true",
		"beads.embedding.enabled=true",
		"beads.embedding.backfill.enabled=true",
	)
	for _, want := range []string{
		"name: test-beads-embedding-backfill-egress",
		"app.kubernetes.io/component: beads-embedding-backfill",
		// Must allow egress to the beads daemon on http.
		"app.kubernetes.io/component: beads",
		"port: 8080",
		// And DNS.
		"k8s-app: kube-dns",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected backfill egress policy to contain %q; got:\n%s", want, out)
		}
	}
}

func TestEmbeddingBackfill_EgressPolicyAbsentWhenDisabled(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, backfillEgressTemplate,
		"networkPolicies.enabled=true",
		"networkPolicies.defaultDenyEgress=true",
		"beads.embedding.enabled=true",
	)
	if strings.Contains(out, "beads-embedding-backfill-egress") {
		t.Errorf("backfill egress policy must not render when backfill is disabled; got:\n%s", out)
	}
}

func TestEmbeddingBackfill_BeadsIngressAllowsBackfill(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, backfillIngressTemplate,
		"networkPolicies.enabled=true",
		"beads.embedding.enabled=true",
		"beads.embedding.backfill.enabled=true",
	)
	// The beads daemon ingress must name the backfill component as a source,
	// or the default-deny-ingress blocks the CronJob's POST.
	if !strings.Contains(out, "app.kubernetes.io/component: beads-embedding-backfill") {
		t.Errorf("beads-ingress must allow the backfill component as a source; got:\n%s", out)
	}
}

func TestEmbeddingBackfill_BeadsIngressUnchangedWhenDisabled(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, backfillIngressTemplate,
		"networkPolicies.enabled=true",
		"beads.embedding.enabled=true",
	)
	if strings.Contains(out, "beads-embedding-backfill") {
		t.Errorf("beads-ingress must not name the backfill component when backfill is disabled; got:\n%s", out)
	}
}
