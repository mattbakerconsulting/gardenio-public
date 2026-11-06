package tests

import (
	"strings"
	"testing"
)

// LiteLLM seed-prisma-engine initContainer path resolution (ka-0000005360).
//
// WHY THIS TEST EXISTS — a near-miss caught while retargeting the litellm pin
// from v1.84.8 to stable v1.95.0. The initContainer copies the Prisma query
// engine out of the image into an emptyDir so the main container can run
// non-root (uid 65534) with a read-only rootfs. It looked up exactly ONE path:
//
//	/root/.cache/prisma-python/binaries/*/*/node_modules/prisma/query-engine-debian-openssl-3.0.x
//
// That path is a v1.84.8-era detail, not a stable contract. v1.95.0 fixed the
// very problem the copy works around (upstream #33650 / #24554) by staging the
// Prisma CLI + engines at /opt/prisma with `chmod -R a+rX` and DROPPING the
// `COPY --from=builder /root/.cache /root/.cache` line entirely.
//
// EMPIRICAL BASIS (both tags run live on prod-gardenio-eks, throwaway pods):
//   - v1.84.8: the /root/.cache glob resolves to .../5.4.2/ac9d7041.../prisma/
//     query-engine-debian-openssl-3.0.x. /opt/prisma does not exist.
//   - v1.95.0: the /root/.cache glob matches NOTHING; the engine lives at
//     /opt/prisma/binaries/node_modules/prisma/query-engine-debian-openssl-3.0.x
//     and IS traversable as uid 65534.
//
// Because the script runs under `set -eu` with an explicit `exit 1` on an empty
// result, the single-path version would have made the initContainer FAIL on
// v1.95.0 — every new pod stuck in Init, the rollout stalled, and (only thanks
// to replicas>=2 on RollingUpdate) the old pods left serving. helm lint, chart
// tests, and a ghcr manifest probe were ALL green on that change: nothing in the
// pre-existing gates reads a path out of the image, which is exactly why this
// test asserts on the script text.
//
// So the lookup probes BOTH layouts, newest first, and these tests pin that —
// a future bump must not silently drop either branch, and a rollback to the old
// tag must keep working.

// seedPrismaEngineScript returns the `sh -c` body of the seed-prisma-engine
// initContainer from the rendered litellm Deployment.
func seedPrismaEngineScript(t *testing.T, rendered string) string {
	t.Helper()
	lines := strings.Split(rendered, "\n")
	start := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == "- name: seed-prisma-engine" {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("rendered Deployment has no seed-prisma-engine initContainer:\n%s", rendered)
	}
	// Take the rest of this container's block: it ends at the next sibling
	// list item at the same indentation.
	indent := len(lines[start]) - len(strings.TrimLeft(lines[start], " "))
	var body []string
	for _, line := range lines[start+1:] {
		if strings.TrimSpace(line) == "" {
			body = append(body, "")
			continue
		}
		cur := len(line) - len(strings.TrimLeft(line, " "))
		if cur <= indent && strings.HasPrefix(strings.TrimSpace(line), "- ") {
			break
		}
		body = append(body, line)
	}
	return strings.Join(body, "\n")
}

// TestLiteLLMSeedPrismaEngineProbesBothImageLayouts is the core regression
// guard: the engine lookup must consider the >= v1.95.0 /opt/prisma layout AND
// the <= v1.84.8 /root/.cache layout.
func TestLiteLLMSeedPrismaEngineProbesBothImageLayouts(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/litellm/deployment.yaml",
		"litellm.enabled=true")
	script := seedPrismaEngineScript(t, out)

	const (
		newLayout = "/opt/prisma/binaries/node_modules/prisma/query-engine-debian-openssl-3.0.x"
		oldLayout = "/root/.cache/prisma-python/binaries/*/*/node_modules/prisma/query-engine-debian-openssl-3.0.x"
	)
	if !strings.Contains(script, newLayout) {
		t.Errorf("seed-prisma-engine does not probe the >= v1.95.0 layout %q — "+
			"the initContainer will exit 1 and the pod will never start on that tag.\nscript:\n%s",
			newLayout, script)
	}
	if !strings.Contains(script, oldLayout) {
		t.Errorf("seed-prisma-engine does not probe the <= v1.84.8 layout %q — "+
			"a rollback to the older pin would break.\nscript:\n%s", oldLayout, script)
	}
}

// TestLiteLLMSeedPrismaEngineFailsLoudlyWhenNoEngineFound pins the fail-closed
// behavior. Silently proceeding with an empty src would produce a pod whose
// main container cannot resolve the query engine, surfacing much later as an
// opaque NotConnectedError at proxy startup instead of a clear Init failure.
func TestLiteLLMSeedPrismaEngineFailsLoudlyWhenNoEngineFound(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/litellm/deployment.yaml",
		"litellm.enabled=true")
	script := seedPrismaEngineScript(t, out)

	if !strings.Contains(script, "exit 1") {
		t.Errorf("seed-prisma-engine must exit non-zero when no engine binary is found; script:\n%s", script)
	}
	// The error message should name both searched locations so an operator
	// reading `kubectl logs -c seed-prisma-engine` knows what moved.
	if !strings.Contains(script, "/opt/prisma/binaries/") || !strings.Contains(script, "/root/.cache/prisma-python/binaries/") {
		t.Errorf("seed-prisma-engine error path should name both candidate locations; script:\n%s", script)
	}
}

// TestLiteLLMSeedPrismaEngineGlobsAreSetEGuarded locks the shell detail that
// makes two-candidate probing work at all. Under `set -e`, a bare `ls` on a
// non-matching glob is a non-zero command and aborts the script BEFORE the
// second candidate is ever tried — which would silently reintroduce the exact
// single-path failure this test guards against, while still textually
// containing both paths.
func TestLiteLLMSeedPrismaEngineGlobsAreSetEGuarded(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/litellm/deployment.yaml",
		"litellm.enabled=true")
	script := seedPrismaEngineScript(t, out)

	for _, line := range strings.Split(script, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.Contains(trimmed, "ls ") || !strings.Contains(trimmed, "query-engine-") {
			continue
		}
		if !strings.Contains(trimmed, "|| true") {
			t.Errorf("engine lookup line is not guarded with `|| true`, so a non-matching "+
				"glob aborts under `set -e` before the other layout is probed:\n  %s", trimmed)
		}
	}
}

// TestLiteLLMMainContainerConsumesSeededEngine ties the two halves together:
// the initContainer's output path must be what the main container points
// PRISMA_QUERY_ENGINE_BINARY at. If these drift, the seed silently does nothing.
func TestLiteLLMMainContainerConsumesSeededEngine(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/litellm/deployment.yaml",
		"litellm.enabled=true")

	const seeded = "/mnt/prisma-engine/query-engine-debian-openssl-3.0.x"
	const consumed = "/tmp/prisma-engine/query-engine-debian-openssl-3.0.x"

	script := seedPrismaEngineScript(t, out)
	if !strings.Contains(script, seeded) {
		t.Errorf("seed-prisma-engine should write the engine to %q; script:\n%s", seeded, script)
	}
	if !strings.Contains(out, "name: PRISMA_QUERY_ENGINE_BINARY") {
		t.Fatalf("main container does not set PRISMA_QUERY_ENGINE_BINARY:\n%s", out)
	}
	if !strings.Contains(out, consumed) {
		t.Errorf("main container should point PRISMA_QUERY_ENGINE_BINARY at %q "+
			"(the read-only mount of what the initContainer seeded)", consumed)
	}
}
