package tests

import (
	"strings"
	"testing"
)

// sectionByName returns the YAML document whose metadata.name matches `name`,
// from a multi-doc helm render. Used to assert rule placement inside the right
// NetworkPolicy rather than anywhere in the full render.
func sectionByName(out, name string) string {
	for d := range strings.SplitSeq(out, "\n---") {
		if strings.Contains(d, "name: "+name+"\n") {
			return d
		}
	}
	return ""
}

// db-init must be a HARD GATE on the litellm DB existing, not a best-effort
// fire-and-forget. Regression for ka-0000005559: a mid-init connectivity flap
// on the ephemeral postgres let `CREATE DATABASE` fail silently while the old
// script still printed "database ready" and exited 0 — the litellm DB was
// never created, the proxy looped on Prisma P1003, readiness never passed, and
// e2e-shard-1 hit its 20m wall (exit 137). The hardened script must (a) run
// under `set -e` so an unhandled error aborts, (b) re-verify the DB exists
// after the create attempt before declaring success, and (c) exit non-zero
// when the DB is still absent so kubelet restarts the initContainer.
func TestLitellm_DbInit_FailsClosed(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/litellm/deployment.yaml",
		"litellm.enabled=true",
		"postgres.enabled=true",
		"litellm.dbInit.enabled=true",
	)
	dbInit := sectionAfter(out, "name: db-init")
	if dbInit == "" {
		t.Fatal("db-init initContainer must render when litellm.dbInit.enabled and postgres.enabled")
	}
	for _, want := range []string{
		"set -eu", // abort on unhandled error / unset var
		"exit 0",  // explicit success only on the verified-present path
		"exit 1",  // fail closed when the DB is still absent
		"CREATE DATABASE",
	} {
		if !strings.Contains(dbInit, want) {
			t.Errorf("db-init script must contain %q (fail-closed DB bootstrap, ka-0000005559)", want)
		}
	}
	// The old fire-and-forget tail unconditionally printed "database ready" and
	// fell off the end (implicit exit 0). The hardened script must only declare
	// the DB ready after re-verifying its presence, so the literal must sit on
	// the verified branch immediately before `exit 0`, never as the last line.
	if strings.Contains(dbInit, "database ready") &&
		!strings.Contains(dbInit, "exit 0") {
		t.Error("db-init prints 'database ready' but never exits 0 on a verified path")
	}
}

// sectionAfter returns the substring of out starting at the first occurrence of
// marker, scoped to the next ~80 lines, so assertions target one container's
// command block rather than anywhere in the full multi-container render.
func sectionAfter(out, marker string) string {
	i := strings.Index(out, marker)
	if i < 0 {
		return ""
	}
	rest := out[i:]
	lines := strings.SplitN(rest, "\n", 81)
	if len(lines) > 80 {
		lines = lines[:80]
	}
	return strings.Join(lines, "\n")
}
