package bridge

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// marshalUpdatedBeadEvent encodes a bead UPDATE SSE payload: {"bead":{...},
// "changes":{...}} so the watcher's requireChange path can be exercised.
func marshalUpdatedBeadEvent(t *testing.T, id, btype string, fields map[string]string, changes map[string]any) []byte {
	t.Helper()
	fieldsJSON, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal fields: %v", err)
	}
	inner := map[string]any{"id": id, "type": btype, "fields": json.RawMessage(fieldsJSON)}
	innerJSON, err := json.Marshal(inner)
	if err != nil {
		t.Fatalf("marshal bead: %v", err)
	}
	wrapper := map[string]any{"bead": json.RawMessage(innerJSON)}
	if changes != nil {
		wrapper["changes"] = changes
	}
	out, err := json.Marshal(wrapper)
	if err != nil {
		t.Fatalf("marshal wrapper: %v", err)
	}
	return out
}

func newTestGateAlert(api NoticeSlackAPI) *ScifGateAlert {
	return NewScifGateAlert(ScifGateAlertConfig{
		API:            api,
		Channel:        "C-gate",
		PortalURL:      "https://prod.gardenio.acme.example/scif",
		BeadsViewerURL: "https://viewer.example",
		Logger:         slog.Default(),
	})
}

func TestScifGateAlert_NilWhenChannelUnset(t *testing.T) {
	if NewScifGateAlert(ScifGateAlertConfig{API: &fakeSlackAPI{}}) != nil {
		t.Error("expected nil watcher when channel is empty")
	}
	if NewScifGateAlert(ScifGateAlertConfig{Channel: "C-gate"}) != nil {
		t.Error("expected nil watcher when API is nil")
	}
}

func TestScifGateAlert_PostsOnReleasePending(t *testing.T) {
	api := &fakeSlackAPI{}
	a := newTestGateAlert(api)
	data := marshalUpdatedBeadEvent(t, "gd-REQ1", scifGateAlertBeadType,
		map[string]string{"release_status": "pending", "requested_by": "alice@example"},
		map[string]any{"release_status": "pending"})
	a.handleUpdated(context.Background(), data)
	if api.calls != 1 {
		t.Fatalf("PostMessage calls = %d, want 1", api.calls)
	}
	if api.channel != "C-gate" {
		t.Errorf("channel = %q, want C-gate", api.channel)
	}
}

func TestScifGateAlert_IgnoresNonPendingAndWrongType(t *testing.T) {
	cases := []struct {
		name    string
		btype   string
		fields  map[string]string
		changes map[string]any
	}{
		{"released terminal", scifGateAlertBeadType, map[string]string{"release_status": "released"}, map[string]any{"release_status": "released"}},
		{"denied terminal", scifGateAlertBeadType, map[string]string{"release_status": "denied"}, map[string]any{"release_status": "denied"}},
		{"wrong type", "task", map[string]string{"release_status": "pending"}, map[string]any{"release_status": "pending"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := &fakeSlackAPI{}
			a := newTestGateAlert(api)
			a.handleUpdated(context.Background(), marshalUpdatedBeadEvent(t, "gd-x", tc.btype, tc.fields, tc.changes))
			if api.calls != 0 {
				t.Errorf("expected no post, got %d", api.calls)
			}
		})
	}
}

// A field that is already pending but is NOT in this update's changes must not
// re-alert — only the transition fires (belt-and-braces with id dedup).
func TestScifGateAlert_RequireChangeOnUpdate(t *testing.T) {
	api := &fakeSlackAPI{}
	a := newTestGateAlert(api)
	data := marshalUpdatedBeadEvent(t, "gd-REQ1", scifGateAlertBeadType,
		map[string]string{"release_status": "pending", "confidence": "high"},
		map[string]any{"confidence": "high"}) // release_status NOT in changes
	a.handleUpdated(context.Background(), data)
	if api.calls != 0 {
		t.Errorf("expected no post when release_status not in changes, got %d", api.calls)
	}
}

func TestScifGateAlert_DedupsByBeadID(t *testing.T) {
	api := &fakeSlackAPI{}
	a := newTestGateAlert(api)
	data := marshalUpdatedBeadEvent(t, "gd-REQ1", scifGateAlertBeadType,
		map[string]string{"release_status": "pending"},
		map[string]any{"release_status": "pending"})
	a.handleUpdated(context.Background(), data)
	a.handleUpdated(context.Background(), data)
	if api.calls != 1 {
		t.Errorf("expected 1 post after duplicate, got %d", api.calls)
	}
}

// FAIL-SAFE: a Slack error is swallowed (logged+dropped), never propagated —
// the gate state is untouched by an outage.
func TestScifGateAlert_SlackErrorIsSwallowed(t *testing.T) {
	api := &fakeSlackAPI{err: context.DeadlineExceeded}
	a := newTestGateAlert(api)
	data := marshalUpdatedBeadEvent(t, "gd-REQ1", scifGateAlertBeadType,
		map[string]string{"release_status": "pending"},
		map[string]any{"release_status": "pending"})
	a.handleUpdated(context.Background(), data) // must not panic
	if api.calls != 1 {
		t.Errorf("expected 1 attempted post, got %d", api.calls)
	}
	// A failed post is NOT marked seen — a later tick retries.
	if a.alreadySeen("gd-REQ1") {
		t.Error("a failed post must not mark the bead seen (retry must be possible)")
	}
}

// PHI MINIMISATION: the message carries pointers + a truncated ask, the tenant
// from data_scope, and NEVER the report body. A long prompt is truncated.
func TestScifGateAlert_FormatIsPointerOnly(t *testing.T) {
	a := newTestGateAlert(&fakeSlackAPI{})
	longPrompt := strings.Repeat("sensitive detail ", 40)
	bead := &BeadEvent{
		ID:   "gd-REQ1",
		Type: scifGateAlertBeadType,
		Fields: map[string]string{
			"release_status": "pending",
			"requested_by":   "alice@example",
			"data_scope":     `{"tenant":"acme","db":"clinical_ro"}`,
			"prompt":         longPrompt,
		},
	}
	msg := a.formatGate2(Garden, bead)
	if !strings.Contains(msg, "gd-REQ1") {
		t.Error("message must reference the request bead id")
	}
	if !strings.Contains(msg, "acme") {
		t.Error("message must carry the data_scope tenant")
	}
	if !strings.Contains(msg, "alice@example") {
		t.Error("message must carry the requester")
	}
	if !strings.Contains(msg, "/scif") {
		t.Error("message must link to the portal")
	}
	if !strings.Contains(msg, "…") {
		t.Error("a long prompt must be truncated")
	}
	if strings.Count(msg, "sensitive detail") > 10 {
		t.Errorf("prompt excerpt not truncated: %q", msg)
	}
}

func TestChangedTo(t *testing.T) {
	cases := []struct {
		name    string
		changes map[string]any
		want    bool
	}{
		{"nil", nil, false},
		{"absent", map[string]any{"other": "x"}, false},
		{"direct string match", map[string]any{"release_status": "pending"}, true},
		{"direct string mismatch", map[string]any{"release_status": "released"}, false},
		{"old-new shape", map[string]any{"release_status": map[string]any{"old": "", "new": "pending"}}, true},
		{"to shape", map[string]any{"release_status": map[string]any{"to": "pending"}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := changedTo(tc.changes, "release_status", "pending"); got != tc.want {
				t.Errorf("changedTo = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestTruncateExcerpt(t *testing.T) {
	if got := truncateExcerpt("  a\n b   c ", 100); got != "a b c" {
		t.Errorf("collapse failed: %q", got)
	}
	if got := truncateExcerpt("", 100); got != "" {
		t.Errorf("empty: %q", got)
	}
	got := truncateExcerpt(strings.Repeat("x", 200), 10)
	if len([]rune(got)) != 11 || !strings.HasSuffix(got, "…") {
		t.Errorf("truncate: %q (len %d)", got, len([]rune(got)))
	}
}
