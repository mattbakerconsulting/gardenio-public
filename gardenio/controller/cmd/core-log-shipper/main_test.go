package main

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestJSONLineURL(t *testing.T) {
	got := jsonlineURL("http://gardenio-victoria-logs:9428/")
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse %q: %v", got, err)
	}
	if u.Path != "/insert/jsonline" {
		t.Errorf("path = %q, want /insert/jsonline", u.Path)
	}
	q := u.Query()
	// The read side (internal/vlogs) and the sibling shippers partition on these
	// exact stream/time/msg fields — they must match or `gb logs` sees no source.
	if q.Get("_stream_fields") != "source" {
		t.Errorf("_stream_fields = %q, want source", q.Get("_stream_fields"))
	}
	if q.Get("_time_field") != "time" {
		t.Errorf("_time_field = %q, want time", q.Get("_time_field"))
	}
	if q.Get("_msg_field") != "msg" {
		t.Errorf("_msg_field = %q, want msg", q.Get("_msg_field"))
	}
	// The trailing slash on the base must not double up.
	if strings.Contains(u.Path, "//") {
		t.Errorf("double slash in path: %q", u.Path)
	}
}

func TestRequiredEnv(t *testing.T) {
	e := &requiredEnv{}
	_ = e.get("A_DEFINITELY_UNSET_VAR_1")
	_ = e.get("A_DEFINITELY_UNSET_VAR_2")
	err := e.err()
	if err == nil {
		t.Fatal("expected an error listing missing vars")
	}
	for _, want := range []string{"A_DEFINITELY_UNSET_VAR_1", "A_DEFINITELY_UNSET_VAR_2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %s", err.Error(), want)
		}
	}
}

func TestEnvDurationAndInt(t *testing.T) {
	if d := envDuration("A_DEFINITELY_UNSET_DUR", 3*time.Second); d != 3*time.Second {
		t.Errorf("envDuration default = %v, want 3s", d)
	}
	if n := envInt("A_DEFINITELY_UNSET_INT", 42); n != 42 {
		t.Errorf("envInt default = %d, want 42", n)
	}
	t.Setenv("A_SET_DUR", "1m")
	if d := envDuration("A_SET_DUR", time.Second); d != time.Minute {
		t.Errorf("envDuration = %v, want 1m", d)
	}
	t.Setenv("A_SET_INT", "7")
	if n := envInt("A_SET_INT", 0); n != 7 {
		t.Errorf("envInt = %d, want 7", n)
	}
}

// buildSources is the OFF-BY-DEFAULT, per-component gate: only components whose
// selector env is set are streamed, and EVERY built source ships all JSON
// (empty MsgPrefix) with the secret-redaction hook wired. This is the contract
// design §5 requires before a broad shipper may be enabled.
func TestBuildSources_PerComponentGatingAndRedaction(t *testing.T) {
	// Nothing set → no sources (the caller fails fast on this).
	if got := buildSources(nil, "ns"); len(got) != 0 {
		t.Fatalf("no selectors set should yield 0 sources, got %d", len(got))
	}

	// Enable exactly two components; the other two stay off.
	t.Setenv("CORE_CONTROLLER_POD_SELECTOR", "app.kubernetes.io/component=controller")
	t.Setenv("CORE_GRAFANA_BRIDGE_POD_SELECTOR", "app.kubernetes.io/component=grafana-bridge")

	got := buildSources(nil, "ns")
	if len(got) != 2 {
		t.Fatalf("expected 2 enabled sources, got %d", len(got))
	}
	names := map[string]bool{}
	for _, s := range got {
		names[s.Name] = true
		if s.MsgPrefix != "" {
			t.Errorf("%s: MsgPrefix = %q, want empty (ship all JSON)", s.Name, s.MsgPrefix)
		}
		if s.Redact == nil {
			t.Errorf("%s: Redact hook must be wired (the scrub gate)", s.Name)
		}
	}
	if !names["controller"] || !names["grafana-bridge"] {
		t.Errorf("expected controller + grafana-bridge sources, got %v", names)
	}
	if names["slack-bridge"] || names["trigger-engine"] {
		t.Errorf("disabled components must not be streamed, got %v", names)
	}
}

// selectorEnvNames names every gate for the fail-fast error message; it must
// enumerate all four components so the operator sees what to enable.
func TestSelectorEnvNames(t *testing.T) {
	got := selectorEnvNames()
	for _, want := range []string{
		"CORE_CONTROLLER_POD_SELECTOR",
		"CORE_SLACK_BRIDGE_POD_SELECTOR",
		"CORE_TRIGGER_ENGINE_POD_SELECTOR",
		"CORE_GRAFANA_BRIDGE_POD_SELECTOR",
	} {
		found := false
		for _, g := range got {
			if g == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("selectorEnvNames missing %s; got %v", want, got)
		}
	}
}
