package beadsapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Tests for the heraldry column on the bead wire shape (heraldry spec §3.3,
// step 1.4): the client carries the key both ways, an absent key decodes to
// "", and HeraldryRun turns the canonical form into the emoji run.

func TestHeraldryRun(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"blue 🌱 . 🔥 . 📦", "🌱🔥📦"},
		{"sand . . 🧭 . .", "🧭"},
		{"🌱 . 🔥 . 📦", "🌱🔥📦"},                   // five tokens: no colour yet
		{"green ⚙️ #️⃣ 1️⃣ © 🐝", "⚙️#️⃣1️⃣©🐝"}, // VS16, keycaps and © keep their bytes
		{"blue . . . . .", ""},
	} {
		if got := HeraldryRun(tc.in); got != tc.want {
			t.Errorf("HeraldryRun(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	var nilBead *BeadDetail
	if got := nilBead.HeraldryRun(); got != "" {
		t.Errorf("nil bead HeraldryRun = %q, want empty", got)
	}
	if got := (&BeadDetail{Heraldry: "red . 🔥 . . ."}).HeraldryRun(); got != "🔥" {
		t.Errorf("bead HeraldryRun = %q, want 🔥", got)
	}
}

func TestHeraldry_GetBeadDecodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"gd-x","title":"t","heraldry":"blue 🌱 . 🔥 . 📦"}`))
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	got, err := c.GetBead(context.Background(), "gd-x")
	if err != nil {
		t.Fatalf("GetBead: %v", err)
	}
	if got.Heraldry != "blue 🌱 . 🔥 . 📦" {
		t.Errorf("Heraldry = %q, want the canonical form", got.Heraldry)
	}
}

func TestHeraldry_AbsentDecodesEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"gd-x","title":"t"}`))
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	got, err := c.GetBead(context.Background(), "gd-x")
	if err != nil {
		t.Fatalf("GetBead: %v", err)
	}
	if got.Heraldry != "" {
		t.Errorf("Heraldry = %q, want empty", got.Heraldry)
	}
}

func TestHeraldry_BatchGetBeadsCarriesIt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results":[
			{"id":"gd-a","bead":{"id":"gd-a","title":"A","type":"epic","heraldry":"sand . . 🧭 . ."}}
		]}`))
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	out, err := c.BatchGetBeads(context.Background(), []string{"gd-a"})
	if err != nil {
		t.Fatalf("BatchGetBeads: %v", err)
	}
	if out["gd-a"] == nil || out["gd-a"].Heraldry != "sand . . 🧭 . ." {
		t.Errorf("out[gd-a] = %+v, want Heraldry \"sand . . 🧭 . .\"", out["gd-a"])
	}
}

func TestHeraldry_CreateBeadRequestMarshals(t *testing.T) {
	b, err := json.Marshal(CreateBeadRequest{Title: "t", Type: "epic", Heraldry: "🌱🔥📦 on blue"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"heraldry":"🌱🔥📦 on blue"`) {
		t.Errorf("marshalled %s, want the heraldry key", b)
	}

	// Empty is none and omitted, so a daemon that predates the column never
	// sees the key.
	b, err = json.Marshal(CreateBeadRequest{Title: "t", Type: "epic"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "heraldry") {
		t.Errorf("marshalled %s, want no heraldry key", b)
	}
}

func TestHeraldry_UpdateBeadSendsIt(t *testing.T) {
	str := func(s string) *string { return &s }
	for _, tc := range []struct {
		name    string
		req     UpdateBeadRequest
		want    any
		wantKey bool
	}{
		{name: "set", req: UpdateBeadRequest{Heraldry: str("🌱🔥📦")}, want: "🌱🔥📦", wantKey: true},
		{name: "remove", req: UpdateBeadRequest{Heraldry: str("none")}, want: "none", wantKey: true},
		{name: "empty removes too", req: UpdateBeadRequest{Heraldry: str("")}, want: "", wantKey: true},
		{name: "nil leaves alone", req: UpdateBeadRequest{Title: str("t")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotBody map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(body, &gotBody)
				w.WriteHeader(http.StatusNoContent)
			}))
			defer srv.Close()

			c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
			if err := c.UpdateBead(context.Background(), "gd-x", tc.req); err != nil {
				t.Fatalf("UpdateBead: %v", err)
			}
			got, has := gotBody["heraldry"]
			if has != tc.wantKey || (has && got != tc.want) {
				t.Errorf("heraldry = %v (present %v), want %v (present %v); body %v", got, has, tc.want, tc.wantKey, gotBody)
			}
		})
	}
}
