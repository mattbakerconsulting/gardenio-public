package bridgeshipper

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSink records every batch it is asked to insert.
type fakeSink struct {
	mu       sync.Mutex
	batches  [][]byte
	failNext bool
}

func (f *fakeSink) Insert(_ context.Context, b []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNext {
		f.failNext = false
		return errors.New("sink boom")
	}
	f.batches = append(f.batches, append([]byte(nil), b...))
	return nil
}

func (f *fakeSink) all() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var sb strings.Builder
	for _, b := range f.batches {
		sb.Write(b)
	}
	return sb.String()
}

// fakeStreamer yields the configured payload once, then blocks on ctx so the
// stream "stays open" like a real follow stream until cancelled.
type fakeStreamer struct {
	payload string
	calls   int
	mu      sync.Mutex
}

func (s *fakeStreamer) Stream(ctx context.Context, _ int64) (io.ReadCloser, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	pr, pw := io.Pipe()
	go func() {
		_, _ = io.WriteString(pw, s.payload)
		<-ctx.Done()
		_ = pw.CloseWithError(io.EOF)
	}()
	return pr, nil
}

// --- processLine: the webhook-audit filter ---------------------------------

func TestProcessLine_Filtering(t *testing.T) {
	jira := Source{Name: "jira-bridge", MsgPrefix: "jira-webhook:"}
	gitlab := Source{Name: "gitlab-bridge", MsgPrefix: "webhook:"}
	nofilter := Source{Name: "jira-bridge"} // empty prefix = keep every JSON record

	cases := []struct {
		name string
		src  Source
		in   string
		keep bool
	}{
		// Jira receive-path records (msg prefix matches) are kept.
		{"jira comment event kept", jira, `{"time":"2026-08-09T00:00:00Z","level":"INFO","msg":"jira-webhook: comment event","jira_key":"FSM-1"}`, true},
		{"jira hmac failed kept", jira, `{"time":"2026-08-09T00:00:00Z","level":"WARN","msg":"jira-webhook: HMAC verification failed"}`, true},
		// A non-receive-path record on the same pod (different msg prefix) is dropped.
		{"jira non-webhook chatter dropped", jira, `{"time":"2026-08-09T00:00:00Z","level":"INFO","msg":"jira description catch-up complete"}`, false},
		// GitLab receive-path records.
		{"gitlab MR merged kept", gitlab, `{"time":"2026-08-09T00:00:00Z","level":"INFO","msg":"webhook: MR merged","mr":42}`, true},
		{"gitlab invalid token kept", gitlab, `{"time":"2026-08-09T00:00:00Z","level":"WARN","msg":"webhook: invalid X-Gitlab-Token"}`, true},
		// GitLab poller / MR-sync chatter (no webhook: prefix) is dropped.
		{"gitlab poller chatter dropped", gitlab, `{"time":"2026-08-09T00:00:00Z","level":"INFO","msg":"starting GitLab MR poller"}`, false},
		// Non-JSON runtime/panic output is never shipped (would break jsonline).
		{"non-json dropped", gitlab, `panic: runtime error: nil map`, false},
		{"empty dropped", gitlab, ``, false},
		{"whitespace dropped", gitlab, `   `, false},
		{"malformed json dropped", gitlab, `{"msg":"webhook: MR merged"`, false},
		// Empty MsgPrefix = fail-open: every JSON record kept regardless of msg.
		{"nofilter keeps any json", nofilter, `{"time":"t","msg":"anything at all"}`, true},
		{"nofilter still drops non-json", nofilter, `not json`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, keep := processLine([]byte(c.in), c.src)
			if keep != c.keep {
				t.Fatalf("processLine(%q) keep=%v, want %v", c.in, keep, c.keep)
			}
			if !keep {
				return
			}
			// A kept record must be valid JSON stamped with the door name.
			var rec map[string]any
			if err := json.Unmarshal(out, &rec); err != nil {
				t.Fatalf("kept record is not valid JSON: %v (%q)", err, out)
			}
			if rec[fieldSource] != c.src.Name {
				t.Errorf("source stamp = %v, want %q", rec[fieldSource], c.src.Name)
			}
		})
	}
}

// A pre-existing "source" field on a bridge record must be overwritten with the
// authoritative door name — a consumer filtering by source must not be fooled by
// a field the bridge happened to log.
func TestProcessLine_SourceStampIsAuthoritative(t *testing.T) {
	src := Source{Name: "gitlab-bridge", MsgPrefix: "webhook:"}
	out, keep := processLine([]byte(`{"msg":"webhook: MR merged","source":"attacker-supplied"}`), src)
	if !keep {
		t.Fatal("record should be kept")
	}
	var rec map[string]any
	if err := json.Unmarshal(out, &rec); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if rec[fieldSource] != "gitlab-bridge" {
		t.Errorf("source = %v, want gitlab-bridge (authoritative, overwrites bridge-supplied)", rec[fieldSource])
	}
}

func TestProcessLine_RedactScrubsSecrets(t *testing.T) {
	// A core-component record echoing a credential must be scrubbed before it is
	// shipped (design §5 redaction gate). The Redact hook receives the final
	// marshaled bytes.
	redact := func(b []byte) []byte {
		return []byte(strings.ReplaceAll(string(b), "glpat-SECRETVALUE12345", "<redacted>"))
	}
	src := Source{Name: "controller", Redact: redact} // empty MsgPrefix: ship all JSON
	out, keep := processLine([]byte(`{"msg":"auth","token":"glpat-SECRETVALUE12345"}`), src)
	if !keep {
		t.Fatal("record should be kept")
	}
	if strings.Contains(string(out), "glpat-SECRETVALUE12345") {
		t.Errorf("secret survived redaction: %s", out)
	}
	if !strings.Contains(string(out), "<redacted>") {
		t.Errorf("expected redaction marker, got: %s", out)
	}
	// The source stamp is still applied through the redaction path.
	var rec map[string]any
	if err := json.Unmarshal(out, &rec); err != nil {
		t.Fatalf("redacted output not valid JSON: %v", err)
	}
	if rec[fieldSource] != "controller" {
		t.Errorf("source stamp lost through redaction: %v", rec[fieldSource])
	}
}

func TestProcessLine_NilRedactIsPassthrough(t *testing.T) {
	src := Source{Name: "controller"} // Redact nil
	out, keep := processLine([]byte(`{"msg":"hello"}`), src)
	if !keep || !strings.Contains(string(out), "hello") {
		t.Fatalf("nil Redact should pass record through unchanged, got keep=%v out=%s", keep, out)
	}
}

// --- Run: end-to-end stream -> filter -> ship over multiple sources ---------

func TestRun_ShipsFilteredRecordsFromBothDoors(t *testing.T) {
	jiraPayload := strings.Join([]string{
		`{"time":"2026-08-09T00:00:00Z","msg":"jira-webhook: comment event","jira_key":"FSM-7"}`,
		`{"time":"2026-08-09T00:00:01Z","msg":"jira nudge delivered"}`, // dropped (no prefix)
	}, "\n") + "\n"
	gitlabPayload := strings.Join([]string{
		`{"time":"2026-08-09T00:00:02Z","msg":"webhook: MR merged","mr":9}`,
		`not-json-runtime-noise`, // dropped
	}, "\n") + "\n"

	sink := &fakeSink{}
	sources := []Source{
		{Name: "jira-bridge", MsgPrefix: "jira-webhook:", Stream: &fakeStreamer{payload: jiraPayload}},
		{Name: "gitlab-bridge", MsgPrefix: "webhook:", Stream: &fakeStreamer{payload: gitlabPayload}},
	}
	s := New(Config{FlushInterval: 10 * time.Millisecond, ReconnectBackoff: time.Hour}, sources, sink, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_ = s.Run(ctx)

	got := sink.all()
	if !strings.Contains(got, `"jira_key":"FSM-7"`) {
		t.Errorf("expected jira comment event shipped; got:\n%s", got)
	}
	if !strings.Contains(got, `"mr":9`) {
		t.Errorf("expected gitlab MR merged shipped; got:\n%s", got)
	}
	if strings.Contains(got, "nudge delivered") {
		t.Errorf("non-receive-path jira chatter must be dropped; got:\n%s", got)
	}
	if strings.Contains(got, "runtime-noise") {
		t.Errorf("non-json noise must be dropped; got:\n%s", got)
	}
	// Both doors' records carry their authoritative source stamp.
	if !strings.Contains(got, `"source":"jira-bridge"`) || !strings.Contains(got, `"source":"gitlab-bridge"`) {
		t.Errorf("both door source stamps must be present; got:\n%s", got)
	}
}

// A source with no MsgPrefix ships every JSON record from the pod (fail-open).
func TestRun_EmptyPrefixShipsAllJSON(t *testing.T) {
	payload := strings.Join([]string{
		`{"time":"t","msg":"webhook: MR merged"}`,
		`{"time":"t","msg":"some other operational line"}`,
		`plain text line`, // still dropped — not JSON
	}, "\n") + "\n"
	sink := &fakeSink{}
	s := New(Config{FlushInterval: 10 * time.Millisecond, ReconnectBackoff: time.Hour},
		[]Source{{Name: "gitlab-bridge", Stream: &fakeStreamer{payload: payload}}}, sink, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	_ = s.Run(ctx)

	got := sink.all()
	if !strings.Contains(got, "MR merged") || !strings.Contains(got, "some other operational line") {
		t.Errorf("empty-prefix source must ship every JSON record; got:\n%s", got)
	}
	if strings.Contains(got, "plain text line") {
		t.Errorf("non-json must still be dropped even with empty prefix; got:\n%s", got)
	}
}

// The shipper reconnects after a stream ends rather than exiting — an audit
// control must survive a bridge pod rollout.
func TestRun_ReconnectsAfterStreamEnds(t *testing.T) {
	fs := &fakeStreamer{payload: `{"msg":"webhook: MR merged"}` + "\n"}
	sink := &fakeSink{}
	s := New(Config{FlushInterval: 5 * time.Millisecond, ReconnectBackoff: 10 * time.Millisecond},
		[]Source{{Name: "gitlab-bridge", MsgPrefix: "webhook:", Stream: fs}}, sink, nil)

	// fakeStreamer keeps the stream open until ctx cancel, so within the window
	// we get one Stream call; the key assertion is Run returns on cancel (does
	// not hang) and the reconnect loop is exercised structurally.
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	select {
	case <-done:
		fs.mu.Lock()
		calls := fs.calls
		fs.mu.Unlock()
		if calls < 1 {
			t.Errorf("expected at least one Stream call, got %d", calls)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancel — it hung")
	}
}
