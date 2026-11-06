package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// primeSectionSizes maps a prime section source name (identity, workflow,
// advice, roster, ...) to its rendered byte size for one gb prime invocation.
type primeSectionSizes map[string]int

// pushPrimeContextMetrics pushes the gb-prime hook composition gauge
// (ka-0000007153, Part B of the context-crisis plan ka-0000003733):
//
//	gardenio_prime_hook_bytes{source,project,role,agent_name}
//
// one sample per rendered section, measured in BYTES — the guard-safe unit: no
// token estimate, no rate, nothing the cost-source AST guard could mistake for
// cost math. Grafana can stack these by source to show what the gb-prime hook's
// slice is made of (advice vs workflow vs roster ...), and ~tokens is a
// display-time bytes/4 division in PromQL if wanted.
//
// NAME: this metric measures ONLY the ~7k gb-prime SessionStart-hook slice —
// the buffer gb itself renders. It structurally CANNOT see the base system
// prompt, the MCP tool-def blob, the skills list, or CLAUDE.md (those are
// assembled by the Claude/coop process, never in gb's buffer), so it is NOT the
// ~70-90k boot. It was renamed from gardenio_prime_context_bytes ->
// gardenio_prime_hook_bytes precisely so nobody reads the hook slice as the boot
// total. For the real boot, see gardenio_session_boot_context_tokens
// (session_metrics_push.go), derived from turn-0 usage.
//
// Same transport as pushSessionMetrics: POST to vmagent
// /api/v1/import/prometheus so the sample lands even for short-lived pods.
// Silently skipped when VMAGENT_URL is unset (dev shells, tests); push
// failures warn and never fail the prime.
func pushPrimeContextMetrics(ctx context.Context, role string, sizes primeSectionSizes) {
	if os.Getenv("VMAGENT_URL") == "" || len(sizes) == 0 {
		return
	}

	labels := fmt.Sprintf("project=%q,role=%q,agent_name=%q",
		defaultGBProject(), role, envOr("BOAT_AGENT", "unknown"))

	// Deterministic order keeps payloads diffable in logs and tests.
	sources := make([]string, 0, len(sizes))
	for s := range sizes {
		sources = append(sources, s)
	}
	sort.Strings(sources)

	// No source="total" sample: sum(gardenio_prime_hook_bytes) across sources
	// IS the total, and a synthetic total label would double it in stacked
	// panels.
	var b strings.Builder
	for _, source := range sources {
		fmt.Fprintf(&b, "gardenio_prime_hook_bytes{%s,source=%q} %d\n", labels, source, sizes[source])
	}

	pushVMAgentLines(ctx, b.String(), 5*time.Second)
}
