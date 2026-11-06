package main

// vmagent_push.go — shared best-effort push of Prometheus text-format lines to
// vmagent's import endpoint, for short-lived gb invocations that cannot be
// scraped, and the one transport every gb metric push uses: POST text/plain to
// $VMAGENT_URL/api/v1/import/prometheus, silently skipped when VMAGENT_URL is
// unset, warn-only on any failure — an observability push must never fail the
// command doing real work.

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// pushVMAgentLines POSTs one or more newline-terminated Prometheus exposition
// lines to vmagent. Best-effort: no VMAGENT_URL → silently skip; any error →
// stderr warning only. The context is detached (WithoutCancel) under a fresh
// short deadline so a caller whose own deadline already expired (e.g. salvage
// at the end of its 25s budget) can still land the push, bounded by timeout.
func pushVMAgentLines(ctx context.Context, lines string, timeout time.Duration) {
	vmagentURL := os.Getenv("VMAGENT_URL")
	if vmagentURL == "" || lines == "" {
		return
	}

	endpoint := strings.TrimRight(vmagentURL, "/") + "/api/v1/import/prometheus"
	pushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(pushCtx, http.MethodPost, endpoint, strings.NewReader(lines))
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: building vmagent metric request: %v\n", err)
		return
	}
	req.Header.Set("Content-Type", "text/plain")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: pushing metrics to vmagent: %v\n", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		fmt.Fprintf(os.Stderr, "warning: vmagent returned HTTP %d on metric push\n", resp.StatusCode)
	}
}
