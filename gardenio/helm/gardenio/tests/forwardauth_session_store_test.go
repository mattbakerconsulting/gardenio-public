package tests

import (
	"strings"
	"testing"
)

// gd-C2Sl6ClxAY: with the cookie session store, two concurrent oauth2-proxy
// refreshes each Set-Cookie their own ~4KB chunks, the browser keeps a mix, and
// every later request fails "cookie signature not valid". The fix is a
// server-side (Redis) session store, opt-in via
// keycloak.forwardAuth.sessionStore.type=redis. These tests pin:
//   - default stays the cookie store and renders no Redis surface;
//   - redis renders the oauth2-proxy flags, the Redis workload + Service and the
//     two NetworkPolicy legs;
//   - the render-time guard rejects bad type / an endpointless config.

const (
	forwardAuthRedisDeployment = "templates/forwardauth/redis-deployment.yaml"
	forwardAuthRedisService    = "templates/forwardauth/redis-service.yaml"
)

func forwardAuthRedisBase() []string {
	return append(forwardAuthBase(), "keycloak.forwardAuth.sessionStore.type=redis")
}

func TestForwardAuthSessionStore_DefaultIsCookie(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, forwardAuthDeployment, forwardAuthBase()...)
	for _, bad := range []string{"--session-store-type", "--redis-connection-url", "OAUTH2_PROXY_REDIS_PASSWORD"} {
		if strings.Contains(out, bad) {
			t.Errorf("default (cookie) store must not render %q:\n%s", bad, out)
		}
	}
	all := helmTemplate(t, "", append(forwardAuthBase(), "networkPolicies.enabled=true")...)
	if strings.Contains(all, "forwardauth-redis") {
		t.Errorf("default (cookie) store must render nothing named forwardauth-redis")
	}
}

func TestForwardAuthSessionStore_RedisFlags(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, forwardAuthDeployment, forwardAuthRedisBase()...)
	for _, want := range []string{
		"- --session-store-type=redis",
		"- --redis-connection-url=redis://test-forwardauth-redis:6379",
		"- --cookie-refresh=3m",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "OAUTH2_PROXY_REDIS_PASSWORD") {
		t.Errorf("no passwordSecret => no password env:\n%s", out)
	}
}

func TestForwardAuthSessionStore_ExternalHostAndPassword(t *testing.T) {
	t.Parallel()
	args := append(forwardAuthRedisBase(),
		"keycloak.forwardAuth.sessionStore.redis.deploy.enabled=false",
		"keycloak.forwardAuth.sessionStore.redis.host=sessions.example.test",
		"keycloak.forwardAuth.sessionStore.redis.port=6380",
		"keycloak.forwardAuth.sessionStore.redis.passwordSecret=sess-redis",
	)
	out := helmTemplate(t, forwardAuthDeployment, args...)
	for _, want := range []string{
		"- --redis-connection-url=redis://sessions.example.test:6380",
		"name: OAUTH2_PROXY_REDIS_PASSWORD",
		"name: sess-redis",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	// External endpoint => no in-chart Redis.
	all := helmTemplate(t, "", args...)
	if strings.Contains(all, "component: forwardauth-redis") {
		t.Errorf("deploy.enabled=false must not render the in-chart Redis")
	}
}

func TestForwardAuthSessionStore_InChartRedisWorkload(t *testing.T) {
	t.Parallel()
	dep := helmTemplate(t, forwardAuthRedisDeployment, forwardAuthRedisBase()...)
	for _, want := range []string{
		"name: test-forwardauth-redis",
		"app.kubernetes.io/component: forwardauth-redis",
		"readOnlyRootFilesystem: true",
		"runAsNonRoot: true",
		"- --appendonly",
		"image: \"",
		"library/redis:7.4-alpine",
	} {
		if !strings.Contains(dep, want) {
			t.Errorf("redis Deployment missing %q:\n%s", want, dep)
		}
	}
	if strings.Contains(dep, "--requirepass") {
		t.Errorf("no passwordSecret => no --requirepass:\n%s", dep)
	}

	withPW := helmTemplate(t, forwardAuthRedisDeployment,
		append(forwardAuthRedisBase(), "keycloak.forwardAuth.sessionStore.redis.passwordSecret=sess-redis")...)
	if !strings.Contains(withPW, "--requirepass") || !strings.Contains(withPW, "name: REDIS_PASSWORD") {
		t.Errorf("passwordSecret must enable --requirepass from REDIS_PASSWORD:\n%s", withPW)
	}

	svc := helmTemplate(t, forwardAuthRedisService, forwardAuthRedisBase()...)
	for _, want := range []string{"kind: Service", "type: ClusterIP", "port: 6379", "component: forwardauth-redis"} {
		if !strings.Contains(svc, want) {
			t.Errorf("redis Service missing %q:\n%s", want, svc)
		}
	}
}

func TestForwardAuthSessionStore_NetworkPolicyLegs(t *testing.T) {
	t.Parallel()
	args := append(forwardAuthRedisBase(), "networkPolicies.enabled=true", "networkPolicies.defaultDenyEgress=true")

	ingress := sliceNetPol(helmTemplate(t, "templates/networkpolicies/networkpolicies.yaml", args...), "-forwardauth-redis-ingress")
	if ingress == "" {
		t.Fatal("expected a forwardauth-redis-ingress NetworkPolicy")
	}
	for _, want := range []string{"component: forwardauth-redis", "-forwardauth", "port: 6379"} {
		if !strings.Contains(ingress, want) {
			t.Errorf("redis ingress policy missing %q:\n%s", want, ingress)
		}
	}

	egress := sliceNetPol(helmTemplate(t, "templates/networkpolicies/default-deny-egress.yaml", args...), "-forwardauth-egress")
	if egress == "" {
		t.Fatal("expected the forwardauth-egress NetworkPolicy")
	}
	if !strings.Contains(egress, "component: forwardauth-redis") || !strings.Contains(egress, "port: 6379") {
		t.Errorf("forwardauth egress must allow the session Redis on 6379:\n%s", egress)
	}
}

func TestForwardAuthSessionStore_Guards(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "", append(forwardAuthBase(), "keycloak.forwardAuth.sessionStore.type=memcached")...)
	if err == nil || !strings.Contains(out, "sessionStore.type must be") {
		t.Errorf("unknown type must fail closed; err=%v out:\n%s", err, out)
	}
	out, err = helmTemplateAllowFail(t, "", append(forwardAuthRedisBase(),
		"keycloak.forwardAuth.sessionStore.redis.deploy.enabled=false")...)
	if err == nil || !strings.Contains(out, "no Redis endpoint") {
		t.Errorf("redis without an endpoint must fail closed; err=%v out:\n%s", err, out)
	}
}
