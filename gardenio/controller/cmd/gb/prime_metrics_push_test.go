package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPushPrimeContextMetrics_PostsToVmagent(t *testing.T) {
	var receivedBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/import/prometheus" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		data, _ := io.ReadAll(r.Body)
		receivedBody = string(data)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	t.Setenv("VMAGENT_URL", srv.URL)
	t.Setenv("BOAT_AGENT", "test-agent")
	t.Setenv("BOAT_PROJECT", "gardenio")

	pushPrimeContextMetrics(context.Background(), "crew", primeSectionSizes{
		"advice":   18000,
		"workflow": 4200,
		"roster":   900,
	})

	if receivedBody == "" {
		t.Fatal("vmagent server received no body")
	}
	for _, want := range []string{
		`gardenio_prime_hook_bytes{project="gardenio",role="crew",agent_name="test-agent",source="advice"} 18000`,
		`gardenio_prime_hook_bytes{project="gardenio",role="crew",agent_name="test-agent",source="workflow"} 4200`,
		`gardenio_prime_hook_bytes{project="gardenio",role="crew",agent_name="test-agent",source="roster"} 900`,
	} {
		if !strings.Contains(receivedBody, want) {
			t.Errorf("body missing line:\n  want: %s\n  body:\n%s", want, receivedBody)
		}
	}

	// No synthetic total sample — sum by source IS the total; a total label
	// would double stacked panels.
	if strings.Contains(receivedBody, `source="total"`) {
		t.Errorf("body must not contain a synthetic source=\"total\" sample:\n%s", receivedBody)
	}
}

func TestPushPrimeContextMetrics_SkipsWhenNoURL(t *testing.T) {
	t.Setenv("VMAGENT_URL", "")
	// Should not panic or block — silently skips.
	pushPrimeContextMetrics(context.Background(), "crew", primeSectionSizes{"advice": 1})
}

func TestPushPrimeContextMetrics_SkipsEmptySizes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("no push expected for empty sizes")
	}))
	defer srv.Close()
	t.Setenv("VMAGENT_URL", srv.URL)

	pushPrimeContextMetrics(context.Background(), "crew", primeSectionSizes{})
}

// TestBuildPrimeBody_SectionSizes asserts the per-section accounting sums to
// the full body: every rendered byte is attributed to exactly one source, so
// the stacked gauge equals the real prime size with no gaps or double counts.
func TestBuildPrimeBody_SectionSizes(t *testing.T) {
	cleanup := startBudgetDaemon(t, budgetCorpus(), budgetConfigBeads())
	defer cleanup()

	t.Setenv("BOAT_ROLE", "crew")
	t.Setenv("BOAT_PROJECT", "gardenio")
	t.Setenv("BOAT_TASK_ID", "skip-auto-assign")

	oldNoAdvice := primeNoAdvice
	primeNoAdvice = false
	defer func() { primeNoAdvice = oldNoAdvice }()

	body, _, sizes := buildPrimeBody("gardenio/crews/budget-test-crew", "crew")

	total := 0
	for source, n := range sizes {
		if n <= 0 {
			t.Errorf("section %q recorded non-positive size %d", source, n)
		}
		total += n
	}
	if total != body.Len() {
		t.Errorf("section sizes sum to %d, body is %d bytes — accounting gap", total, body.Len())
	}
}
