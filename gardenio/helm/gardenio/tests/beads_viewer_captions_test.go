package tests

import (
	"strings"
	"testing"
)

// Bird captions on the /garden farm (plan gd-jMl4oF2cJO, step 10 gd-FOzMlrXga6; spec
// gardenio/docs/superpowers/specs/2026-10-28-bird-captions-design.md sections 8 and 14). beadsViewer.captions.* and
// beadsViewer.transcript.rows reach the viewer as the env gardenhub.CaptionSettingsFromEnv and
// captioner.ConfigFromEnv read. FARM_CAPTIONS=off is the kill switch for every caption, and FARM_CAPTION_HAIKU=on
// needs captions on, haiku.enabled and the key Secret.

func captionsViewer(t *testing.T, valuesFiles []string, sets ...string) string {
	t.Helper()
	return helmTemplateShowOnlyWithValues(t, "templates/beads-viewer/deployment.yaml", valuesFiles,
		append([]string{
			"beadsViewer.enabled=true",
			"beadsViewer.oidc.issuerURL=https://auth.example.com/realms/x",
			"beadsViewer.oidc.clientID=viewer",
		}, sets...)...)
}

func TestBeadsViewer_Captions_Defaults(t *testing.T) {
	t.Parallel()
	out := captionsViewer(t, nil)
	for env, want := range map[string]string{
		"FARM_CAPTIONS":                      "on",
		"FARM_CAPTION_WIRE_CAP":              "80",
		"FARM_CAPTION_HAIKU":                 "off",
		"FARM_CAPTION_HAIKU_RATE_PER_MINUTE": "30",
		"FARM_CAPTION_HAIKU_CONCURRENCY":     "4",
		"FARM_CAPTION_HAIKU_TIMEOUT":         "3s",
		"FARM_CAPTION_HAIKU_DAILY_MAX":       "5000",
		"FARM_CAPTION_HAIKU_CACHE_SIZE":      "4096",
		"FARM_TRANSCRIPT_ROWS":               "200",
	} {
		if got := envValue(t, out, env); got != want {
			t.Errorf("chart default %s = %q, want %q (spec section 14)", env, got, want)
		}
	}
	// No key by default: no secret ref and no LiteLLM URL.
	if strings.Contains(out, "FARM_CAPTION_LITELLM_KEY") || strings.Contains(out, "- name: LITELLM_URL") {
		t.Errorf("the chart default mounts no caption key, got:\n%s", out)
	}
}

func TestBeadsViewer_Captions_HaikuNeedsEverySwitchAndTheKey(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		sets []string
		want string
	}{
		{"enabled with the key", []string{"beadsViewer.captions.haiku.enabled=true", "beadsViewer.captions.haiku.secretName=k"}, "on"},
		{"enabled without the key", []string{"beadsViewer.captions.haiku.enabled=true"}, "off"},
		{"key, not enabled", []string{"beadsViewer.captions.haiku.secretName=k"}, "off"},
		{"captions killed", []string{"beadsViewer.captions.enabled=false", "beadsViewer.captions.haiku.enabled=true", "beadsViewer.captions.haiku.secretName=k"}, "off"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := envValue(t, captionsViewer(t, nil, tc.sets...), "FARM_CAPTION_HAIKU"); got != tc.want {
				t.Errorf("FARM_CAPTION_HAIKU = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBeadsViewer_Captions_KillSwitchAndBounds(t *testing.T) {
	t.Parallel()
	out := captionsViewer(t, nil,
		"beadsViewer.captions.enabled=false",
		"beadsViewer.captions.wireCap=48",
		"beadsViewer.captions.haiku.ratePerMinute=10",
		"beadsViewer.captions.haiku.concurrency=2",
		"beadsViewer.captions.haiku.timeout=2s",
		"beadsViewer.captions.haiku.dailyMax=1000",
		"beadsViewer.captions.haiku.cacheSize=512",
		"beadsViewer.transcript.rows=50",
	)
	for env, want := range map[string]string{
		"FARM_CAPTIONS":                      "off",
		"FARM_CAPTION_WIRE_CAP":              "48",
		"FARM_CAPTION_HAIKU_RATE_PER_MINUTE": "10",
		"FARM_CAPTION_HAIKU_CONCURRENCY":     "2",
		"FARM_CAPTION_HAIKU_TIMEOUT":         "2s",
		"FARM_CAPTION_HAIKU_DAILY_MAX":       "1000",
		"FARM_CAPTION_HAIKU_CACHE_SIZE":      "512",
		"FARM_TRANSCRIPT_ROWS":               "50",
	} {
		if got := envValue(t, out, env); got != want {
			t.Errorf("%s = %q, want %q", env, got, want)
		}
	}
}

// The live k3s deployment keeps captions on but turns the Haiku fallback off
// (owner 10-31: agents describe their own calls; every Haiku call 401'd because
// the LiteLLM routes forward the caller's OAuth login and the viewer has none).
// The scoped key (gd-25uXUE3HJJ) stays mounted and the spec's bounds stay
// rendered, so turning it back on is a one-line values change.
func TestBeadsViewer_Captions_DeployValuesK3sHaikuOff(t *testing.T) {
	t.Parallel()
	out := captionsViewer(t, []string{"../../../deploy-values/gardenio-k3s.yaml"})
	for env, want := range map[string]string{
		"FARM_CAPTIONS":                      "on",
		"FARM_CAPTION_HAIKU":                 "off",
		"FARM_CAPTION_HAIKU_RATE_PER_MINUTE": "30",
		"FARM_CAPTION_HAIKU_DAILY_MAX":       "5000",
	} {
		if got := envValue(t, out, env); got != want {
			t.Errorf("k3s %s = %q, want %q", env, got, want)
		}
	}
	if !strings.Contains(out, `name: "gardenio-beads-viewer-litellm"`) || !strings.Contains(out, "- name: LITELLM_URL") {
		t.Errorf("k3s mounts the caption key and LITELLM_URL, got:\n%s", out)
	}
}
