package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/coopapi"
)

// channelStub is the gardenio-channel /permission endpoint: it records each
// verdict and answers with status.
type channelStub struct {
	mu       sync.Mutex
	verdicts []map[string]string
	auth     []string
	status   int
}

func (c *channelStub) serve(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		c.mu.Lock()
		c.verdicts = append(c.verdicts, body)
		c.auth = append(c.auth, r.URL.Path+" "+r.Header.Get("Authorization"))
		status := c.status
		c.mu.Unlock()
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func permissionMail(id string, fields map[string]string) beadsapi.BeadDetail {
	return beadsapi.BeadDetail{ID: id, Type: "mail", Status: "open", Assignee: "crew-x",
		Labels: []string{"from:slack-bridge", "control:permission"}, Fields: fields}
}

func newTestPermissionControl(d *podMailDaemon, url string) (*permissionControl, *[]string) {
	var logs []string
	return &permissionControl{
		daemon: d, url: url + "/permission", token: "tok", client: http.DefaultClient,
		logf: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) },
	}, &logs
}

// A verdict is POSTed to the channel server with its token, then the mail is
// closed (stamped control_acted) so the next session is not handed it.
func TestPermissionControl_PostsVerdictAndCloses(t *testing.T) {
	ch := &channelStub{}
	d := &podMailDaemon{}
	p, _ := newTestPermissionControl(d, ch.serve(t))

	p.OnControl(context.Background(), "permission", permissionMail("gd-m1", map[string]string{"request_id": "abcde", "behavior": "allow"}))

	if len(ch.verdicts) != 1 || ch.verdicts[0]["request_id"] != "abcde" || ch.verdicts[0]["behavior"] != "allow" {
		t.Fatalf("channel got %v, want one allow for abcde", ch.verdicts)
	}
	if ch.auth[0] != "/permission Bearer tok" {
		t.Errorf("request was %q, want POST /permission with the channel token", ch.auth[0])
	}
	if got := d.closedIDs(); len(got) != 1 || got[0] != "gd-m1" {
		t.Errorf("closed %v, want gd-m1", got)
	}
}

// A malformed verdict and a refusal are not retried, and the mail is still
// closed; a 5xx is retried.
func TestPermissionControl_RefusalsAndRetries(t *testing.T) {
	defer func(a int, d time.Duration) { permissionPostAttempts, permissionPostRetryDelay = a, d }(permissionPostAttempts, permissionPostRetryDelay)
	permissionPostAttempts, permissionPostRetryDelay = 3, time.Millisecond

	for _, c := range []struct {
		name   string
		fields map[string]string
		status int
		posts  int
	}{
		{"no request id", map[string]string{"behavior": "allow"}, 0, 0},
		{"not a behavior", map[string]string{"request_id": "abcde", "behavior": "maybe"}, 0, 0},
		{"refused", map[string]string{"request_id": "abcde", "behavior": "deny"}, http.StatusBadRequest, 1},
		{"server error", map[string]string{"request_id": "abcde", "behavior": "deny"}, http.StatusInternalServerError, 3},
	} {
		ch := &channelStub{status: c.status}
		d := &podMailDaemon{}
		p, logs := newTestPermissionControl(d, ch.serve(t))
		p.OnControl(context.Background(), "permission", permissionMail("gd-m2", c.fields))
		if len(ch.verdicts) != c.posts {
			t.Errorf("%s: %d posts, want %d", c.name, len(ch.verdicts), c.posts)
		}
		if len(d.closedIDs()) != 1 {
			t.Errorf("%s: mail not closed", c.name)
		}
		if len(*logs) != 1 {
			t.Errorf("%s: logs %v", c.name, *logs)
		}
	}
}

// Other control verbs are left alone: not posted, not closed.
func TestPermissionControl_OtherVerbsIgnored(t *testing.T) {
	ch := &channelStub{}
	d := &podMailDaemon{}
	p, logs := newTestPermissionControl(d, ch.serve(t))
	p.OnControl(context.Background(), "escalation-approved", permissionMail("gd-m3", nil))
	if len(ch.verdicts) != 0 || len(d.closedIDs()) != 0 || len(*logs) != 1 {
		t.Errorf("escalation-approved: posts %v closed %v logs %v", ch.verdicts, d.closedIDs(), *logs)
	}
}

// startPodMail wires the hook: control:permission mail on the stream reaches
// the channel server named by GARDENIO_CHANNEL_PORT.
func TestStartPodMail_PermissionReachesChannel(t *testing.T) {
	ch := &channelStub{}
	srv := ch.serve(t)
	u, err := url.Parse(srv)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GARDENIO_CHANNEL_PORT", u.Port())
	t.Setenv("GARDENIO_CHANNEL_TOKEN", "tok")

	d := &podMailDaemon{stream: make(chan beadsapi.SSEEvent, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := startPodMail(ctx, d, coopapi.New(newIdleCoop(t).srv.URL), "gd-agent", "crew-x", func(string, ...any) {})
	defer p.stop()

	data, _ := json.Marshal(map[string]any{"bead": map[string]any{
		"id": "gd-m4", "type": "mail", "status": "open", "assignee": "crew-x",
		"labels": []string{"from:slack-bridge", "control:permission"},
		"fields": map[string]string{"request_id": "abcde", "behavior": "allow"},
	}})
	d.stream <- beadsapi.SSEEvent{ID: "gd-m4", Data: data}

	waitFor(t, func() bool { return len(d.closedIDs()) == 1 })
	ch.mu.Lock()
	defer ch.mu.Unlock()
	if len(ch.verdicts) != 1 || ch.verdicts[0]["behavior"] != "allow" || ch.auth[0] != "/permission Bearer tok" {
		t.Fatalf("channel got %v %v", ch.verdicts, ch.auth)
	}
}
