package tests

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

// Resume-latency dashboard R4-gate empty-state wording (bug gd-TPxEUVbzL6).
//
// The gardenio-resume-latency board's headline R4 sub-second-resume gate — the
// three thaw-warm panels (R4 p95, R4 p50, Warm-graft hit rate) — reads empty in
// prod because the gVisor freeze/thaw wake path is a deliberate hard-hold on the
// near-idle gvisor-poc canary, NOT because the emitter is broken (the controller
// thaw path records gardenio_resume_latency_seconds{path=thaw-warm/thaw-fresh} at
// main.go's sole RecordResumeLatency call site; freezethaw_lifecycle_test.go
// covers the path labelling).
//
// The pre-fix null mapping on those panels read "awaiting first sample", which
// implies data is imminent — misleading a grader when the path is simply not
// exercised. The fix relabels the THREE thaw-warm-only panels to
// "warm-resume path not yet exercised" while KEEPING the all-paths liveness
// panel ("Resumes observed (24h)") on "awaiting first sample" — cold-path
// resumes DO emit into that panel, so its wording stays accurate.
//
// Follow-on (ka-0000006178): the liveness panel's zero fallback is now gated on
// `up{component="slack-bridge"} == 1` — the process that actually observes
// resume latency. That gate SPLITS its single empty state in two, and the two
// mappings now carry one reading each:
//
//   - value 0  → the gate held, so the exporter is up and 24h genuinely produced
//     no sample: "0 — awaiting first sample (exporter live)".
//   - null     → the gate was empty too, so the exporter is dark and the count is
//     unknown rather than zero: "exporter dark — slack-bridge scrape down".
//
// So "awaiting first sample" is still asserted below, but it must now be read
// off the value mapping (see emptyStateMappingTexts) — putting it on the null
// mapping would claim a live-but-idle emitter in the one state that cannot
// establish it, the `unsupported-novalue` defect the grafanagrade empty-branch
// detector flags.
//
// These tests guard both halves so a future edit to the embedded JSON cannot
// silently regress either the relabel or the deliberately-kept liveness wording.

const resumeLatencyDashboardKey = "gardenio-resume-latency.json"

// resumeLatencyMapping is one Grafana value mapping. `options` has two shapes:
// a `special` mapping carries {"match":"null","result":{"text":…}}, while a
// `value` mapping keys the result by the matched value, {"0":{"text":…}}. Decode
// it raw so emptyStateMappingTexts can read BOTH (see its comment).
type resumeLatencyMapping struct {
	Type    string                     `json:"type"`
	Options map[string]json.RawMessage `json:"options"`
}

// resumeLatencyPanel captures the per-panel field-config value mappings the
// shared `dashboard` struct does not expose.
type resumeLatencyPanel struct {
	Type        string `json:"type"`
	Title       string `json:"title"`
	FieldConfig struct {
		Defaults struct {
			Mappings []resumeLatencyMapping `json:"mappings"`
		} `json:"defaults"`
	} `json:"fieldConfig"`
	Targets []struct {
		Expr string `json:"expr"`
	} `json:"targets"`
}

type resumeLatencyDashboard struct {
	UID    string               `json:"uid"`
	Panels []resumeLatencyPanel `json:"panels"`
}

func parseResumeLatencyDashboard(t *testing.T) resumeLatencyDashboard {
	t.Helper()
	data := dashboardConfigMapData(t)
	raw, ok := data[resumeLatencyDashboardKey]
	if !ok {
		t.Fatalf("dashboard ConfigMap is missing key %q; keys present: %v", resumeLatencyDashboardKey, keysOf(data))
	}
	var d resumeLatencyDashboard
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		t.Fatalf("embedded %s is not valid JSON (nindent likely broke it): %v", resumeLatencyDashboardKey, err)
	}
	if d.UID != "gardenio-resume-latency" {
		t.Errorf("resume-latency board must carry its stable uid; got %q", d.UID)
	}
	return d
}

// emptyStateMappingTexts returns every value-mapping display text on a panel —
// the `special` null/no-data mapping's result text AND any exact-value ("0")
// mapping texts — so a test can assert the empty-state wording without pinning a
// mapping index.
//
// It must read both shapes. Its predecessor (nullMappingTexts) claimed to, but
// modelled `options` as a struct with match/result fields, which decodes a value
// mapping's {"0":{"text":…}} to the zero value — silently dropping that wording.
// That mattered the moment a panel's zero branch was liveness-gated
// (ka-0000006178): the gate splits one empty state into two distinct readings,
// and the honest "this is a REAL zero" text moves onto the value mapping while
// the null mapping becomes "the exporter is dark". Reading only the null mapping
// would see half the story. Sorted for a deterministic failure message.
func emptyStateMappingTexts(p resumeLatencyPanel) []string {
	var texts []string
	for _, m := range p.FieldConfig.Defaults.Mappings {
		for key, rawOpt := range m.Options {
			if key == "match" {
				continue // the matcher itself ("null"), not a display text
			}
			var v struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(rawOpt, &v); err != nil {
				continue
			}
			if v.Text != "" {
				texts = append(texts, v.Text)
			}
		}
	}
	sort.Strings(texts)
	return texts
}

// TestResumeLatencyDashboard_R4GateEmptyStateRelabelled asserts the three
// thaw-warm-only R4-gate panels carry the "warm-resume path not yet exercised"
// empty-state text and no longer carry the misleading "awaiting first sample".
func TestResumeLatencyDashboard_R4GateEmptyStateRelabelled(t *testing.T) {
	t.Parallel()
	d := parseResumeLatencyDashboard(t)

	const wantText = "warm-resume path not yet exercised"
	const staleText = "awaiting first sample"

	// The R4 gate is exactly these three thaw-warm-only panels. Each queries the
	// path="thaw-warm" series, so its empty state means "the warm path has not
	// run", not "data is imminent".
	r4GatePanels := map[string]bool{
		"R4 (thaw-warm) p95 — gate < 1.5s": false,
		"R4 (thaw-warm) p50 — gate < 1s":   false,
		"Warm-graft hit rate":              false,
	}

	for _, p := range d.Panels {
		if _, isGate := r4GatePanels[p.Title]; !isGate {
			continue
		}
		r4GatePanels[p.Title] = true

		texts := strings.Join(emptyStateMappingTexts(p), " | ")
		if !strings.Contains(texts, wantText) {
			t.Errorf("R4-gate panel %q must relabel its empty state to %q; mappings were: %q",
				p.Title, wantText, texts)
		}
		if strings.Contains(texts, staleText) {
			t.Errorf("R4-gate panel %q must NOT keep the misleading %q empty state; mappings were: %q",
				p.Title, staleText, texts)
		}
		// Sanity: the panel really is a thaw-warm panel (guards against a future
		// rename that moves one of these titles onto an unrelated query).
		var expr strings.Builder
		for _, tg := range p.Targets {
			expr.WriteString(tg.Expr)
		}
		if !strings.Contains(expr.String(), `path="thaw-warm"`) &&
			!strings.Contains(expr.String(), `path=\"thaw-warm\"`) {
			t.Errorf("R4-gate panel %q is expected to query path=thaw-warm; expr was: %q",
				p.Title, expr.String())
		}
	}

	for title, found := range r4GatePanels {
		if !found {
			t.Errorf("expected R4-gate panel %q on the resume-latency board; not found", title)
		}
	}
}

// TestResumeLatencyDashboard_LivenessKeepsAwaitingWording asserts the all-paths
// liveness panel ("Resumes observed (24h)") deliberately KEEPS the "awaiting
// first sample" wording — cold-path resumes emit into it, so a real zero there
// genuinely does mean "no sample yet across any path", which is what the relabel
// must NOT touch. Since ka-0000006178 that reading lives on the value-0 mapping
// (the liveness-gated real zero) rather than the null one; see the file header.
func TestResumeLatencyDashboard_LivenessKeepsAwaitingWording(t *testing.T) {
	t.Parallel()
	d := parseResumeLatencyDashboard(t)

	const wantText = "awaiting first sample"
	found := false
	for _, p := range d.Panels {
		if p.Title != "Resumes observed (24h)" {
			continue
		}
		found = true
		texts := strings.Join(emptyStateMappingTexts(p), " | ")
		if !strings.Contains(texts, wantText) {
			t.Errorf("liveness panel %q must keep the all-paths %q wording; mappings were: %q",
				p.Title, wantText, texts)
		}
		// It must NOT be relabelled to the thaw-warm-specific wording — that would
		// be wrong for an all-paths counter that the cold emitter feeds.
		if strings.Contains(texts, "warm-resume path not yet exercised") {
			t.Errorf("liveness panel %q must NOT use the thaw-warm-only empty state; mappings were: %q",
				p.Title, texts)
		}
	}
	if !found {
		t.Error(`expected the "Resumes observed (24h)" liveness panel on the resume-latency board; not found`)
	}
}
