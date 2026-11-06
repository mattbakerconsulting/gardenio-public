package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// short_title gate (plan 2026-10-14-short-titles D2, gd-V5nGseRS5f). These
// tests pin the BEADS_SHORT_TITLE_GATE parse, the gated type set, the
// X-Beads-Warning header in warn mode and the agent-only refusal in enforce.

func TestShortTitleGateModeFromEnv(t *testing.T) {
	cases := []struct {
		val  string
		want shortTitleGateMode
	}{
		{"", shortTitleGateWarn}, // unset → the observation window, not off
		{"  ", shortTitleGateWarn},
		{"warn", shortTitleGateWarn},
		{"WARN", shortTitleGateWarn},
		{"enforce", shortTitleGateEnforce},
		{"1", shortTitleGateEnforce},
		{"true", shortTitleGateEnforce},
		{"on", shortTitleGateEnforce},
		{"off", shortTitleGateOff},
		{"0", shortTitleGateOff},
		{"false", shortTitleGateOff},
		{"bogus", shortTitleGateOff}, // a typo never refuses
	}
	for _, tc := range cases {
		t.Run(tc.val, func(t *testing.T) {
			t.Setenv("BEADS_SHORT_TITLE_GATE", tc.val)
			if got := shortTitleGateModeFromEnv(); got != tc.want {
				t.Errorf("shortTitleGateModeFromEnv() with %q = %v; want %v", tc.val, got, tc.want)
			}
		})
	}
}

func TestIsShortTitleGatedType(t *testing.T) {
	for _, bt := range []model.BeadType{
		model.TypeTask, model.TypeBug, model.TypeFeature, model.TypeEpic,
		model.TypeDecision, model.TypeEscalation,
	} {
		if !isShortTitleGatedType(bt) {
			t.Errorf("isShortTitleGatedType(%q) = false; want true", bt)
		}
	}
	for _, bt := range []model.BeadType{
		"agent", "session", "message", "bundle", "molecule", "trigger", "schedule",
		model.TypeFormula, model.TypeAdvice, "",
	} {
		if isShortTitleGatedType(bt) {
			t.Errorf("isShortTitleGatedType(%q) = true; want false", bt)
		}
	}
}

func TestIsNonHumanCaller(t *testing.T) {
	cases := map[string]bool{
		"static": true, "fleet": true, "machine": true,
		"oidc": false, "edge": false, "anonymous": false, "": false,
	}
	for typ, want := range cases {
		if got := isNonHumanCaller(AuthIdentity{Type: typ}); got != want {
			t.Errorf("isNonHumanCaller(%q) = %v; want %v", typ, got, want)
		}
	}
}

// shortTitleGateCallers are the identities the gate classifies, each carrying
// a human X-Operated-By so the operator resolver attributes the write.
var shortTitleGateCallers = map[string]struct {
	id    AuthIdentity
	agent bool
}{
	"fleet":   {AuthIdentity{Type: "fleet", Subject: "slack-bridge"}, true},
	"machine": {AuthIdentity{Type: "machine", Subject: "machine:gardenio-controller"}, true},
	"oidc":    {AuthIdentity{Type: "oidc", Subject: "operator@example.com"}, false},
}

func shortTitleGateCtx(id AuthIdentity) context.Context {
	ctx := WithAuthIdentity(context.Background(), id)
	if id.Type != "oidc" {
		ctx = WithOperatedByHeader(ctx, "slack:U0OPERATOR1")
	}
	return ctx
}

// postCreate drives handleCreateBead directly with ctx, so a test can pick the
// caller's AuthIdentity without a matching token verifier.
func postCreate(t *testing.T, s *BeadsServer, ctx context.Context, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/beads", bytes.NewReader(b)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleCreateBead(rec, req)
	return rec
}

func requireShortTitleWarning(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	got := rec.Header().Values("X-Beads-Warning")
	if len(got) != 1 {
		t.Fatalf("X-Beads-Warning = %q; want exactly one", got)
	}
	if !strings.HasPrefix(got[0], "short_title missing: ") {
		t.Errorf("X-Beads-Warning = %q; want prefix %q", got[0], "short_title missing: ")
	}
	if !strings.Contains(got[0], model.ShortTitleExample) {
		t.Errorf("X-Beads-Warning = %q; want the rendered example", got[0])
	}
}

func TestCreateBead_ShortTitleGate_WarnAddsHeader(t *testing.T) {
	for _, mode := range []string{"", "warn"} {
		t.Run("mode="+mode, func(t *testing.T) {
			t.Setenv("BEADS_SHORT_TITLE_GATE", mode)
			shortTitleGateTotal.Reset()
			s, _, _ := newTestServer()

			rec := postCreate(t, s, shortTitleGateCtx(shortTitleGateCallers["fleet"].id),
				map[string]any{"title": "Forward duration_ms through the gb hook relay", "type": "task"})
			requireStatus(t, rec, http.StatusCreated)
			requireShortTitleWarning(t, rec)
			if got := testutil.ToFloat64(shortTitleGateTotal.WithLabelValues("task", "agent", "allowed_warn")); got != 1 {
				t.Errorf("task/agent/allowed_warn = %v; want 1", got)
			}

			// Supplying it clears the warning and counts as supplied.
			rec = postCreate(t, s, shortTitleGateCtx(shortTitleGateCallers["fleet"].id),
				map[string]any{"title": "Forward duration_ms", "short_title": "forward duration_ms", "type": "task"})
			requireStatus(t, rec, http.StatusCreated)
			if got := rec.Header().Values("X-Beads-Warning"); len(got) != 0 {
				t.Errorf("X-Beads-Warning = %q; want none when short_title is supplied", got)
			}
			if got := testutil.ToFloat64(shortTitleGateTotal.WithLabelValues("task", "agent", "supplied")); got != 1 {
				t.Errorf("task/agent/supplied = %v; want 1", got)
			}
		})
	}
}

func TestCreateBead_ShortTitleGate_EnforceRefusesAgent(t *testing.T) {
	t.Setenv("BEADS_SHORT_TITLE_GATE", "enforce")
	for _, name := range []string{"fleet", "machine"} {
		t.Run(name, func(t *testing.T) {
			shortTitleGateTotal.Reset()
			s, ms, _ := newTestServer()
			before := len(ms.beads)

			rec := postCreate(t, s, shortTitleGateCtx(shortTitleGateCallers[name].id),
				map[string]any{"title": "Forward duration_ms through the gb hook relay", "type": "task"})
			requireStatus(t, rec, http.StatusBadRequest)
			var body struct {
				Error string `json:"error"`
			}
			decodeJSON(t, rec, &body)
			if want := "short_title is required for type task: " + model.ShortTitleHelp; body.Error != want {
				t.Errorf("error = %q; want %q", body.Error, want)
			}
			if len(ms.beads) != before {
				t.Errorf("refused create persisted a bead")
			}
			if got := testutil.ToFloat64(shortTitleGateTotal.WithLabelValues("task", "agent", "refused")); got != 1 {
				t.Errorf("task/agent/refused = %v; want 1", got)
			}

			// The same caller with a short_title is allowed.
			rec = postCreate(t, s, shortTitleGateCtx(shortTitleGateCallers[name].id),
				map[string]any{"title": "Forward duration_ms", "short_title": "forward duration_ms", "type": "task"})
			requireStatus(t, rec, http.StatusCreated)
		})
	}
}

func TestCreateBead_ShortTitleGate_EnforceWarnsHuman(t *testing.T) {
	t.Setenv("BEADS_SHORT_TITLE_GATE", "enforce")
	shortTitleGateTotal.Reset()
	s, _, _ := newTestServer()

	rec := postCreate(t, s, shortTitleGateCtx(shortTitleGateCallers["oidc"].id),
		map[string]any{"title": "Forward duration_ms through the gb hook relay", "type": "bug"})
	requireStatus(t, rec, http.StatusCreated)
	requireShortTitleWarning(t, rec)
	if got := testutil.ToFloat64(shortTitleGateTotal.WithLabelValues("bug", "human", "allowed_warn")); got != 1 {
		t.Errorf("bug/human/allowed_warn = %v; want 1", got)
	}
}

// TestCreateBead_ShortTitleGate_EnforceRefusesGRPCPath pins that the refusal
// lives at the createBead choke point, so the gRPC transport (no warning
// collector) is held to it too, and a warn-mode create with no collector
// still succeeds.
func TestCreateBead_ShortTitleGate_EnforceRefusesGRPCPath(t *testing.T) {
	s, _, _ := newTestServer()
	ctx := shortTitleGateCtx(shortTitleGateCallers["fleet"].id)

	t.Setenv("BEADS_SHORT_TITLE_GATE", "enforce")
	_, err := s.createBead(ctx, createBeadInput{Title: "an epic", Type: "epic"})
	var ie inputError
	if !errors.As(err, &ie) {
		t.Fatalf("createBead error = %v; want inputError", err)
	}

	t.Setenv("BEADS_SHORT_TITLE_GATE", "warn")
	if _, err := s.createBead(ctx, createBeadInput{Title: "an epic", Type: "epic"}); err != nil {
		t.Fatalf("createBead (warn, no collector) error = %v; want success", err)
	}
}

func TestCreateBead_ShortTitleGate_OffIsSilent(t *testing.T) {
	t.Setenv("BEADS_SHORT_TITLE_GATE", "off")
	shortTitleGateTotal.Reset()
	s, _, _ := newTestServer()

	rec := postCreate(t, s, shortTitleGateCtx(shortTitleGateCallers["fleet"].id),
		map[string]any{"title": "a task", "type": "task"})
	requireStatus(t, rec, http.StatusCreated)
	if got := rec.Header().Values("X-Beads-Warning"); len(got) != 0 {
		t.Errorf("X-Beads-Warning = %q; want none when off", got)
	}
	if got := testutil.CollectAndCount(shortTitleGateTotal); got != 0 {
		t.Errorf("short_title gate series = %d; want 0 when off", got)
	}
}

func TestCreateBead_ShortTitleGate_InternalTypesUntouched(t *testing.T) {
	t.Setenv("BEADS_SHORT_TITLE_GATE", "enforce")
	shortTitleGateTotal.Reset()
	s, _, _ := newTestServer()

	for _, tc := range []struct {
		typ    string
		fields string
	}{
		{"message", ""},
		{"trigger", `{"action":"notify"}`},
	} {
		body := map[string]any{"title": "internal " + tc.typ, "type": tc.typ}
		if tc.fields != "" {
			body["fields"] = json.RawMessage(tc.fields)
		}
		rec := postCreate(t, s, shortTitleGateCtx(shortTitleGateCallers["fleet"].id), body)
		requireStatus(t, rec, http.StatusCreated)
		if got := rec.Header().Values("X-Beads-Warning"); len(got) != 0 {
			t.Errorf("%s: X-Beads-Warning = %q; want none", tc.typ, got)
		}
	}
	if got := testutil.CollectAndCount(shortTitleGateTotal); got != 0 {
		t.Errorf("short_title gate series = %d; want 0 for internal types", got)
	}
}
