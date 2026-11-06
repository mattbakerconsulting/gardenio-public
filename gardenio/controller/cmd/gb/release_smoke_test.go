package main

import (
	"errors"
	"testing"
)

// running is the steady-state pod phase the probes accept.
const running = "Running"

func TestClassifyPodsProbe(t *testing.T) {
	t.Run("all system pods running -> pass, ephemeral excluded", func(t *testing.T) {
		p := classifyPodsProbe([]smokePod{
			{Name: "gardenio-agents-1", Phase: running, Component: componentAgents},
			{Name: "gardenio-slack-bridge-1", Phase: running, Component: componentSlackBridge},
			// A spawned worker pod mid-startup must NOT flap the probe.
			{Name: "gardenio-worker-abc", Phase: "Pending", Ephemeral: true},
		}, nil)
		if !p.Pass {
			t.Fatalf("expected pass, got fail: %s", p.Detail)
		}
		if p.Signal != "" {
			t.Errorf("passing probe should carry no signal, got %q", p.Signal)
		}
	})

	t.Run("a system pod not running -> fail + uat-regression", func(t *testing.T) {
		p := classifyPodsProbe([]smokePod{
			{Name: "gardenio-agents-1", Phase: running, Component: componentAgents},
			{Name: "gardenio-postgres-0", Phase: "CrashLoopBackOff", Component: "postgres"},
		}, nil)
		if p.Pass {
			t.Fatal("expected fail")
		}
		if p.Signal != signalUATRegression {
			t.Errorf("signal = %q, want %q", p.Signal, signalUATRegression)
		}
	})

	t.Run("succeeded one-shot pod is healthy", func(t *testing.T) {
		p := classifyPodsProbe([]smokePod{
			{Name: "gardenio-agents-1", Phase: running, Component: componentAgents},
			{Name: "gardenio-migrate-xyz", Phase: "Succeeded", Component: "migrate"},
		}, nil)
		if !p.Pass {
			t.Errorf("expected pass (Succeeded is healthy), got fail: %s", p.Detail)
		}
	})

	t.Run("only ephemeral pods present -> fail (no system pods)", func(t *testing.T) {
		p := classifyPodsProbe([]smokePod{
			{Name: "gardenio-worker-1", Phase: running, Ephemeral: true},
		}, nil)
		if p.Pass {
			t.Fatal("expected fail when only ephemeral pods present")
		}
		if p.Signal != signalUATRegression {
			t.Errorf("signal = %q, want %q", p.Signal, signalUATRegression)
		}
	})

	t.Run("list error -> fail + uat-regression", func(t *testing.T) {
		p := classifyPodsProbe(nil, errors.New("forbidden"))
		if p.Pass {
			t.Fatal("expected fail on list error")
		}
		if p.Signal != signalUATRegression {
			t.Errorf("signal = %q, want %q", p.Signal, signalUATRegression)
		}
	})
}

func TestClassifyComponentProbe(t *testing.T) {
	pods := []smokePod{
		{Name: "gardenio-agents-1", Phase: running, Component: componentAgents},
		{Name: "gardenio-slack-bridge-1", Phase: running, Component: componentSlackBridge},
	}

	t.Run("component running -> pass", func(t *testing.T) {
		p := classifyComponentProbe("controller-phase", componentAgents, pods, nil)
		if !p.Pass {
			t.Errorf("expected pass, got fail: %s", p.Detail)
		}
	})

	t.Run("component pod not running (standby/pending phase) -> fail", func(t *testing.T) {
		p := classifyComponentProbe("controller-phase", componentAgents, []smokePod{
			{Name: "gardenio-agents-1", Phase: "Pending", Component: componentAgents},
		}, nil)
		if p.Pass {
			t.Fatal("expected fail when controller not Running")
		}
		if p.Signal != signalUATRegression {
			t.Errorf("signal = %q, want %q", p.Signal, signalUATRegression)
		}
	})

	t.Run("component absent -> fail", func(t *testing.T) {
		p := classifyComponentProbe("slack-bridge", componentSlackBridge, []smokePod{
			{Name: "gardenio-agents-1", Phase: running, Component: componentAgents},
		}, nil)
		if p.Pass {
			t.Fatal("expected fail when slack-bridge pod absent")
		}
		if p.Signal != signalUATRegression {
			t.Errorf("signal = %q, want %q", p.Signal, signalUATRegression)
		}
	})

	t.Run("list error -> fail", func(t *testing.T) {
		p := classifyComponentProbe("controller-phase", componentAgents, nil, errors.New("boom"))
		if p.Pass {
			t.Fatal("expected fail on list error")
		}
	})
}

func TestClassifyRoundtripProbe(t *testing.T) {
	t.Run("nil error -> pass", func(t *testing.T) {
		p := classifyRoundtripProbe("beads-roundtrip", nil)
		if !p.Pass {
			t.Errorf("expected pass, got fail: %s", p.Detail)
		}
		if p.Signal != "" {
			t.Errorf("passing probe should carry no signal, got %q", p.Signal)
		}
	})

	t.Run("error -> fail + uat-regression", func(t *testing.T) {
		p := classifyRoundtripProbe("gb-news-roundtrip", errors.New("connection refused"))
		if p.Pass {
			t.Fatal("expected fail")
		}
		if p.Signal != signalUATRegression {
			t.Errorf("signal = %q, want %q", p.Signal, signalUATRegression)
		}
	})
}

// TestClassifyDriftProbe: the prod-drift probe passes only when the prod
// controller tag matches the golden tag (ka-0000000327).
func TestClassifyDriftProbe(t *testing.T) {
	tests := []struct {
		name             string
		prodTag, golden  string
		fetchErr         error
		wantPass         bool
		wantSignalOnFail bool
	}{
		{"matching tags -> pass", "2026.177.7", "2026.177.7", nil, true, false},
		{"v-prefix normalizes equal -> pass", "2026.177.7", "v2026.177.7", nil, true, false},
		{"mismatch -> fail + prod-drift", "2026.177.6", "2026.177.7", nil, false, true},
		{"missing prod tag -> fail", "", "2026.177.7", nil, false, true},
		{"missing golden -> fail", "2026.177.7", "", nil, false, true},
		{"fetch error -> fail", "2026.177.7", "", errors.New("gitlab 500"), false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := classifyDriftProbe(tt.prodTag, tt.golden, tt.fetchErr)
			if p.Pass != tt.wantPass {
				t.Fatalf("pass = %v, want %v (detail: %s)", p.Pass, tt.wantPass, p.Detail)
			}
			if !tt.wantPass && tt.wantSignalOnFail && p.Signal != signalProdDrift {
				t.Errorf("signal = %q, want %q", p.Signal, signalProdDrift)
			}
		})
	}
}

func TestCombineSmokeResult(t *testing.T) {
	t.Run("all pass -> pass, empty failed/signals", func(t *testing.T) {
		res := combineSmokeResult([]smokeProbe{
			passProbe("pods-present", "ok"),
			passProbe("controller-phase", "ok"),
		})
		if !res.Pass {
			t.Error("expected overall pass")
		}
		if len(res.Failed) != 0 {
			t.Errorf("failed_probes = %v, want empty", res.Failed)
		}
		if len(res.Signals) != 0 {
			t.Errorf("signals = %v, want empty", res.Signals)
		}
	})

	t.Run("one fail -> overall fail, failed name + signal carried", func(t *testing.T) {
		res := combineSmokeResult([]smokeProbe{
			passProbe("pods-present", "ok"),
			failProbe("beads-roundtrip", signalUATRegression, "refused"),
		})
		if res.Pass {
			t.Fatal("expected overall fail")
		}
		if len(res.Failed) != 1 || res.Failed[0] != "beads-roundtrip" {
			t.Errorf("failed_probes = %v, want [beads-roundtrip]", res.Failed)
		}
		if len(res.Signals) != 1 || res.Signals[0] != signalUATRegression {
			t.Errorf("signals = %v, want [%s]", res.Signals, signalUATRegression)
		}
	})

	t.Run("signals de-duplicated in first-seen order", func(t *testing.T) {
		res := combineSmokeResult([]smokeProbe{
			failProbe("pods-present", signalUATRegression, "x"),
			failProbe("prod-drift", signalProdDrift, "y"),
			failProbe("beads-roundtrip", signalUATRegression, "z"),
		})
		if len(res.Signals) != 2 {
			t.Fatalf("signals = %v, want 2 distinct", res.Signals)
		}
		if res.Signals[0] != signalUATRegression || res.Signals[1] != signalProdDrift {
			t.Errorf("signals = %v, want [%s %s] in first-seen order",
				res.Signals, signalUATRegression, signalProdDrift)
		}
		if len(res.Failed) != 3 {
			t.Errorf("failed_probes = %v, want 3", res.Failed)
		}
	})

	t.Run("nil probes normalize to empty slices for stable JSON", func(t *testing.T) {
		res := combineSmokeResult(nil)
		if !res.Pass {
			t.Error("empty probe set should be a vacuous pass")
		}
		if res.Probes == nil || res.Failed == nil || res.Signals == nil {
			t.Error("probes/failed/signals must be non-nil ([] not null)")
		}
	})
}

func TestParseImageTag(t *testing.T) {
	tests := []struct {
		image string
		want  string
	}{
		{"registry.gitlab.com/acme/platform/gardenio/controller:2026.177.7", "2026.177.7"},
		{"controller:2026.177.7", "2026.177.7"},
		{"registry:5000/acme/controller:2026.177.7", "2026.177.7"}, // host port must not be read as tag
		{"registry:5000/acme/controller:2026.177.7@sha256:abc123", "2026.177.7"},
		{"controller", ""}, // no tag
		{"registry:5000/acme/controller", ""},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.image, func(t *testing.T) {
			if got := parseImageTag(tt.image); got != tt.want {
				t.Errorf("parseImageTag(%q) = %q, want %q", tt.image, got, tt.want)
			}
		})
	}
}

func TestNormalizeTag(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"2026.177.7", "2026.177.7"},
		{"v2026.177.7", "2026.177.7"},
		{"  v2026.177.7  ", "2026.177.7"},
		{"", ""},
	} {
		if got := normalizeTag(tt.in); got != tt.want {
			t.Errorf("normalizeTag(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestLatestGoldenTagName(t *testing.T) {
	tagNames := []string{"2026.177.9", "2026.177.8", "2026.177.7", "not-a-tag"}

	t.Run("returns newest golden, skipping unpromoted newer tags", func(t *testing.T) {
		golden := map[string]struct{}{"2026.177.7": {}, "2026.177.8": {}}
		// .9 is unpromoted; newest golden is .8
		if got := latestGoldenTagName(tagNames, golden); got != "2026.177.8" {
			t.Errorf("got %q, want 2026.177.8", got)
		}
	})

	t.Run("no golden -> empty", func(t *testing.T) {
		if got := latestGoldenTagName(tagNames, map[string]struct{}{}); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
}

func TestIsHealthyPodPhase(t *testing.T) {
	for _, tt := range []struct {
		phase string
		want  bool
	}{
		{"Running", true},
		{"Succeeded", true},
		{"Pending", false},
		{"Failed", false},
		{"Unknown", false},
		{"", false},
	} {
		if got := isHealthyPodPhase(tt.phase); got != tt.want {
			t.Errorf("isHealthyPodPhase(%q) = %v, want %v", tt.phase, got, tt.want)
		}
	}
}
