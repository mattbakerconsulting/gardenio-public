package server

import "testing"

// A JSON-wrapped bearer ({"token":"<hex>"}) is what an ExternalSecret
// mirror without `property` materializes; the same logical token can then
// circulate wrapped on one side (daemon env or client env) and raw on the
// other. resolveFleetToken must match both forms (bug ka-0000003595
// lineage; staging E2E 401s on release 2026.190.5).
func TestNormalizeBearerToken(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"raw-passthrough", "abc123", "abc123"},
		{"json-wrapped", `{"token":"abc123"}`, "abc123"},
		{"json-wrapped-whitespace", ` {"token":"abc123"} `, "abc123"},
		{"json-empty-token", `{"token":""}`, `{"token":""}`},
		{"json-no-token-field", `{"other":"x"}`, `{"other":"x"}`},
		{"malformed-json", `{"token":`, `{"token":`},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeBearerToken(tc.in); got != tc.want {
				t.Errorf("normalizeBearerToken(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestResolveFleetTokenWrappedRaw(t *testing.T) {
	const raw = "deadbeefcafe"
	const wrapped = `{"token":"deadbeefcafe"}`

	fc := NewFleetConfig("default", nil)
	fc.RegisterToken(raw, "svc")

	cases := []struct {
		name     string
		provided string
		wantTok  string
		want     bool
	}{
		{"raw", raw, raw, true},
		{"wrapped-matches-raw", wrapped, raw, true},
		{"mismatch", `{"token":"other"}`, `{"token":"other"}`, false},
		{"empty", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotTok, got := resolveFleetToken(tc.provided, fc)
			if got != tc.want || gotTok != tc.wantTok {
				t.Errorf("resolveFleetToken(%q) = (%q, %v), want (%q, %v)", tc.provided, gotTok, got, tc.wantTok, tc.want)
			}
		})
	}

	t.Run("nil-config", func(t *testing.T) {
		if _, ok := resolveFleetToken(raw, nil); ok {
			t.Error("a nil FleetConfig must match nothing")
		}
	})
}
