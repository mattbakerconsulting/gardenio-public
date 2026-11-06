package tests

import (
	"strings"
	"testing"
)

// LiteLLM Redis router coordination (H4, ka-0000003463, epic ka-0000005228).
//
// At replicaCount>=2 (H2, ka-0000007221) LiteLLM's per-process router state —
// rate-limit counters, model cooldowns, the router cache — diverges across
// replicas. Wiring redis_host/redis_port under router_settings flips the Router
// onto a SHARED backend so the two replicas coordinate. These tests pin:
//   - default-OFF: NOTHING redis renders (byte-identical to pre-H4);
//   - the redis_* keys land under router_settings (the only place LiteLLM
//     honors them — same rule as model_group_alias / fallbacks, ka-0000005911);
//   - the AUTH password is os.environ-referenced (never in the ConfigMap) and
//     injected as LITELLM_REDIS_PASSWORD env;
//   - the in-chart Redis workload + NetworkPolicies render only when deployed;
//   - the render-time guard fails an enabled-but-endpointless config.

// TestLitellmRedis_DefaultOff asserts the whole redis surface is absent by
// default: no config keys, no env, no workload, no policies.
func TestLitellmRedis_DefaultOff(t *testing.T) {
	t.Parallel()
	cfg := extractConfigYAML(t, helmTemplate(t, "templates/litellm/configmap.yaml",
		"litellm.enabled=true",
	))
	if strings.Contains(cfg, "redis_host") || strings.Contains(cfg, "redis_port") ||
		strings.Contains(cfg, "redis_password") {
		t.Errorf("redis disabled must render NO redis_* keys; got config:\n%s", cfg)
	}

	// The in-chart Redis Deployment/Service templates must not render.
	if _, err := helmTemplateAllowFail(t, "templates/litellm/redis-deployment.yaml",
		"litellm.enabled=true"); err == nil {
		t.Errorf("redis Deployment must not render when redis disabled")
	}
}

// TestLitellmRedis_KeysUnderRouterSettings pins the redis_* keys under
// router_settings — the only placement LiteLLM honors.
func TestLitellmRedis_KeysUnderRouterSettings(t *testing.T) {
	t.Parallel()
	cfg := extractConfigYAML(t, helmTemplate(t, "templates/litellm/configmap.yaml",
		"litellm.enabled=true",
		"litellm.redis.enabled=true",
		"litellm.redis.deploy.enabled=true",
	))
	if !redisKeyUnderRouterSettings(cfg, "redis_host") {
		t.Errorf("redis_host must render under router_settings; got config:\n%s", cfg)
	}
	if !redisKeyUnderRouterSettings(cfg, "redis_port") {
		t.Errorf("redis_port must render under router_settings; got config:\n%s", cfg)
	}
	// In-chart deploy: host defaults to the in-chart Redis Service name.
	if !strings.Contains(cfg, `redis_host: "test-litellm-redis"`) {
		t.Errorf("in-chart redis host must default to the Service name; got config:\n%s", cfg)
	}
}

// TestLitellmRedis_ExternalHost pins the managed/external path: host is honored
// verbatim, no in-chart workload renders.
func TestLitellmRedis_ExternalHost(t *testing.T) {
	t.Parallel()
	cfg := extractConfigYAML(t, helmTemplate(t, "templates/litellm/configmap.yaml",
		"litellm.enabled=true",
		"litellm.redis.enabled=true",
		"litellm.redis.host=my-cache.example.internal",
		"litellm.redis.port=6380",
	))
	if !strings.Contains(cfg, `redis_host: "my-cache.example.internal"`) {
		t.Errorf("external redis host must be honored verbatim; got config:\n%s", cfg)
	}
	if !strings.Contains(cfg, "redis_port: 6380") {
		t.Errorf("external redis port must be honored; got config:\n%s", cfg)
	}
	if _, err := helmTemplateAllowFail(t, "templates/litellm/redis-deployment.yaml",
		"litellm.enabled=true",
		"litellm.redis.enabled=true",
		"litellm.redis.host=my-cache.example.internal"); err == nil {
		t.Errorf("no in-chart Redis Deployment when deploy.enabled=false")
	}
}

// TestLitellmRedis_PasswordViaEnv pins the AUTH contract: the password is
// os.environ-referenced in the ConfigMap (never a literal) and injected as the
// LITELLM_REDIS_PASSWORD env from the named Secret.
func TestLitellmRedis_PasswordViaEnv(t *testing.T) {
	t.Parallel()
	cfg := extractConfigYAML(t, helmTemplate(t, "templates/litellm/configmap.yaml",
		"litellm.enabled=true",
		"litellm.redis.enabled=true",
		"litellm.redis.host=ext",
		"litellm.redis.passwordSecret=my-redis-secret",
	))
	if !strings.Contains(cfg, "redis_password: os.environ/LITELLM_REDIS_PASSWORD") {
		t.Errorf("redis_password must be os.environ-referenced; got config:\n%s", cfg)
	}

	dep := helmTemplate(t, "templates/litellm/deployment.yaml",
		"litellm.enabled=true",
		"litellm.redis.enabled=true",
		"litellm.redis.host=ext",
		"litellm.redis.passwordSecret=my-redis-secret",
		"litellm.redis.passwordKey=redis-token",
	)
	if !strings.Contains(dep, "name: LITELLM_REDIS_PASSWORD") {
		t.Errorf("deployment must inject LITELLM_REDIS_PASSWORD env; got:\n%s", dep)
	}
	if !strings.Contains(dep, "name: my-redis-secret") || !strings.Contains(dep, "key: redis-token") {
		t.Errorf("LITELLM_REDIS_PASSWORD must source the named Secret/key; got:\n%s", dep)
	}

	// No auth => no env, no redis_password key.
	depNoAuth := helmTemplate(t, "templates/litellm/deployment.yaml",
		"litellm.enabled=true",
		"litellm.redis.enabled=true",
		"litellm.redis.deploy.enabled=true",
	)
	if strings.Contains(depNoAuth, "LITELLM_REDIS_PASSWORD") {
		t.Errorf("no passwordSecret must render no LITELLM_REDIS_PASSWORD env; got:\n%s", depNoAuth)
	}
}

// TestLitellmRedis_InChartWorkload pins the in-chart Redis Deployment + Service
// render (PSA-restricted, no persistence).
func TestLitellmRedis_InChartWorkload(t *testing.T) {
	t.Parallel()
	dep := helmTemplate(t, "templates/litellm/redis-deployment.yaml",
		"litellm.enabled=true",
		"litellm.redis.enabled=true",
		"litellm.redis.deploy.enabled=true",
	)
	if !strings.Contains(dep, "kind: Deployment") {
		t.Fatalf("expected a Redis Deployment; got:\n%s", dep)
	}
	// PSA-restricted: non-root, read-only rootfs, drop ALL caps.
	for _, want := range []string{"runAsNonRoot: true", "readOnlyRootFilesystem: true", "drop:"} {
		if !strings.Contains(dep, want) {
			t.Errorf("Redis Deployment must be PSA-restricted (missing %q); got:\n%s", want, dep)
		}
	}
	// No persistence: RDB/AOF off so the ephemeral coordination state stays on
	// an emptyDir under the read-only rootfs.
	if !strings.Contains(dep, "--appendonly") {
		t.Errorf("Redis must disable AOF persistence; got:\n%s", dep)
	}
	if strings.Contains(dep, "persistentVolumeClaim") {
		t.Errorf("router-coordination Redis must NOT claim a PVC; got:\n%s", dep)
	}

	svc := helmTemplate(t, "templates/litellm/redis-service.yaml",
		"litellm.enabled=true",
		"litellm.redis.enabled=true",
		"litellm.redis.deploy.enabled=true",
	)
	if !strings.Contains(svc, "kind: Service") || !strings.Contains(svc, "type: ClusterIP") {
		t.Errorf("expected a ClusterIP Redis Service; got:\n%s", svc)
	}
}

// TestLitellmRedis_NetworkPolicies pins the paired ingress/egress legs — a
// NetworkPolicy flow needs BOTH sides.
func TestLitellmRedis_NetworkPolicies(t *testing.T) {
	t.Parallel()
	ingress := helmTemplate(t, "templates/networkpolicies/networkpolicies.yaml",
		"litellm.enabled=true",
		"litellm.redis.enabled=true",
		"litellm.redis.deploy.enabled=true",
		"networkPolicies.enabled=true",
	)
	if !strings.Contains(ingress, "litellm-redis-ingress") {
		t.Errorf("expected a litellm-redis-ingress NetworkPolicy; got:\n%s", ingress)
	}

	egress := helmTemplate(t, "templates/networkpolicies/default-deny-egress.yaml",
		"litellm.enabled=true",
		"litellm.redis.enabled=true",
		"litellm.redis.deploy.enabled=true",
		"networkPolicies.enabled=true",
		"networkPolicies.defaultDenyEgress=true",
	)
	if !strings.Contains(egress, "component: litellm-redis") {
		t.Errorf("litellm-egress must allow the redis leg; got:\n%s", egress)
	}
}

// TestLitellmRedis_GuardRejectsEndpointless pins the render-time guard: redis
// enabled with neither in-chart deploy nor an external host must fail closed.
func TestLitellmRedis_GuardRejectsEndpointless(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "",
		"litellm.enabled=true",
		"litellm.redis.enabled=true",
	)
	if err == nil {
		t.Fatalf("expected render failure when redis has no endpoint; got success:\n%s", out)
	}
	if !strings.Contains(out, "no Redis endpoint") {
		t.Errorf("guard message should name the missing endpoint; got:\n%s", out)
	}
}

// redisKeyUnderRouterSettings reports whether `key` appears nested under a
// top-level `router_settings:` block. Mirrors the indentation reasoning in
// litellm_router_alias_test.go: after extractConfigYAML strips the block-scalar
// indent, router_settings is at 0-indent and its children are more-indented.
func redisKeyUnderRouterSettings(cfg, key string) bool {
	lines := strings.Split(cfg, "\n")
	inRouter := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if indent == 0 {
			inRouter = trimmed == "router_settings:"
			continue
		}
		if inRouter && strings.HasPrefix(trimmed, key+":") {
			return true
		}
	}
	return false
}
