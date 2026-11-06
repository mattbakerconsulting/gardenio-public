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
