package main

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/auditxform"
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
	if q.Get("_stream_fields") != "entryPointName" {
		t.Errorf("_stream_fields = %q, want entryPointName", q.Get("_stream_fields"))
	}
	if q.Get("_time_field") != "StartUTC" {
		t.Errorf("_time_field = %q, want StartUTC", q.Get("_time_field"))
	}
	if q.Get("_msg_field") != "RequestPath" {
		t.Errorf("_msg_field = %q, want RequestPath", q.Get("_msg_field"))
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

// writeTable writes rows to a temp file and returns its path.
func writeTable(t *testing.T, rows string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "table.txt")
	if err := os.WriteFile(p, []byte(rows), 0o600); err != nil {
		t.Fatalf("write table: %v", err)
	}
	return p
}

func TestBuildResolver(t *testing.T) {
	geoPath := writeTable(t, "203.0.113.0/24 AS64512 US EXAMPLE-NET\n")
	asnPath := writeTable(t, "203.0.113.0/24 AS64512 EXAMPLE-NET\n")

	t.Run("neither set => nil (raw shipping)", func(t *testing.T) {
		r, geo, err := buildResolver("", "")
		if err != nil || r != nil || geo {
			t.Fatalf("buildResolver(\"\",\"\") = (%v,%v,%v), want (nil,false,nil)", r, geo, err)
		}
	})

	t.Run("geo table => geoEnabled + GeoResolver", func(t *testing.T) {
		r, geo, err := buildResolver("", geoPath)
		if err != nil {
			t.Fatalf("buildResolver geo: %v", err)
		}
		if !geo {
			t.Error("geoEnabled must be true for a geo table")
		}
		if _, ok := r.(auditxform.GeoResolver); !ok {
			t.Errorf("geo table must yield a GeoResolver, got %T", r)
		}
	})

	t.Run("asn table => not geoEnabled", func(t *testing.T) {
		r, geo, err := buildResolver(asnPath, "")
		if err != nil {
			t.Fatalf("buildResolver asn: %v", err)
		}
		if geo {
			t.Error("geoEnabled must be false for an ASN-only table")
		}
		if r == nil {
			t.Error("asn table must yield a resolver")
		}
	})

	t.Run("both set => error (mutually exclusive)", func(t *testing.T) {
		_, _, err := buildResolver(asnPath, geoPath)
		if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
			t.Fatalf("both-set must error with 'mutually exclusive', got %v", err)
		}
	})

	t.Run("missing table => fail fast", func(t *testing.T) {
		_, _, err := buildResolver("/no/such/asn-table.txt", "")
		if err == nil {
			t.Fatal("a missing table must fail fast, not silently ship without enrichment")
		}
	})

	t.Run("garbled table => fail fast", func(t *testing.T) {
		bad := writeTable(t, "this-is-not-a-cidr\n")
		if _, _, err := buildResolver(bad, ""); err == nil {
			t.Fatal("a garbled table must fail fast")
		}
	})
}
