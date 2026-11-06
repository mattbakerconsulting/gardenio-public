package tests

import (
	"strings"
	"testing"
)

// Regression guards for the litellm OOM allocator-swap fix (ka-0000003951;
// RCA ka-0000000169). The ~190Mi/min unbounded RSS growth that OOMKilled the
// proxy is glibc main-arena fragmentation, not a Python leak. The durable fix
// is jemalloc via LD_PRELOAD (+ MALLOC_ARENA_MAX=2 belt-and-suspenders).
//
// Because the pod runs the UPSTREAM ghcr.io/berriai/litellm image (which does
// NOT ship libjemalloc) and an LD_PRELOAD to a missing .so is a SILENT no-op,
// the fix stages the real library via an initContainer into a shared emptyDir.
// These tests pin that the wiring renders end-to-end when enabled and is fully
// absent (default-off, byte-identical to before) when disabled.

//  1. Default-off: none of the jemalloc wiring renders. Guards the "purely
//     additive" claim so existing overlays render unchanged.
func TestLitellmJemalloc_DisabledByDefault(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/litellm/deployment.yaml",
		"litellm.enabled=true",
	)
	for _, marker := range []string{"stage-jemalloc", "libjemalloc", "LD_PRELOAD", "MALLOC_ARENA_MAX"} {
		if strings.Contains(out, marker) {
			t.Errorf("jemalloc marker %q must NOT render when litellm.jemalloc is disabled", marker)
		}
	}
}

//  2. Enabled: the stage-jemalloc initContainer, the LD_PRELOAD + arena env, the
//     read-only mount, and the shared emptyDir all render.
func TestLitellmJemalloc_EnabledRendersFullWiring(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/litellm/deployment.yaml",
		"litellm.enabled=true",
		"litellm.jemalloc.enabled=true",
	)
	wants := []string{
		"name: stage-jemalloc",           // the staging initContainer
		"apk fetch --no-cache",           // fetches libjemalloc2 from the baked-in repo
		"libjemalloc.so.2",               // the shared object
		"name: LD_PRELOAD",               // preloaded on the main container
		"/opt/jemalloc/libjemalloc.so.2", // LD_PRELOAD points at the staged path
		"name: MALLOC_ARENA_MAX",         // belt-and-suspenders tunable
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("jemalloc-enabled render is missing %q", w)
		}
	}
	// The arena cap defaults to "2".
	if !strings.Contains(out, `name: MALLOC_ARENA_MAX`) || !strings.Contains(out, `value: "2"`) {
		t.Errorf("MALLOC_ARENA_MAX should default to \"2\"; render:\n%s", out)
	}
}

//  3. The staged lib is mounted READ-ONLY on the main container — the main
//     container must never be able to mutate the shared object.
func TestLitellmJemalloc_MountIsReadOnly(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/litellm/deployment.yaml",
		"litellm.enabled=true",
		"litellm.jemalloc.enabled=true",
	)
	// Find the jemalloc volumeMount block and assert readOnly: true follows it.
	idx := strings.Index(out, "mountPath: \"/opt/jemalloc\"")
	if idx < 0 {
		t.Fatalf("jemalloc volumeMount not found in render:\n%s", out)
	}
	tail := out[idx:]
	window := tail
	if len(window) > 200 {
		window = window[:200]
	}
	if !strings.Contains(window, "readOnly: true") {
		t.Errorf("jemalloc mount must be readOnly: true; got:\n%s", window)
	}
}

// 4. arenaMax="" omits the MALLOC_ARENA_MAX env (LD_PRELOAD still renders).
func TestLitellmJemalloc_EmptyArenaMaxOmitsEnv(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/litellm/deployment.yaml",
		"litellm.enabled=true",
		"litellm.jemalloc.enabled=true",
		"litellm.jemalloc.arenaMax=",
	)
	if strings.Contains(out, "MALLOC_ARENA_MAX") {
		t.Error("MALLOC_ARENA_MAX must be omitted when litellm.jemalloc.arenaMax is empty")
	}
	if !strings.Contains(out, "name: LD_PRELOAD") {
		t.Error("LD_PRELOAD must still render when only arenaMax is empty")
	}
}
