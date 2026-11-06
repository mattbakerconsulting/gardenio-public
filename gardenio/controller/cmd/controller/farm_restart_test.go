package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"gardenio/controller/internal/beadsapi"
)

// A request is pending while its stamp differs from the last one answered.
func TestFarmRestartPending(t *testing.T) {
	for _, tc := range []struct {
		fields map[string]string
		want   string
	}{
		{map[string]string{}, ""},
		{map[string]string{beadsapi.FieldRestartRequestedAt: "t1"}, "t1"},
		{map[string]string{beadsapi.FieldRestartRequestedAt: "t2", beadsapi.FieldRestartAnswered: "t1"}, "t2"},
		{map[string]string{beadsapi.FieldRestartRequestedAt: "t1", beadsapi.FieldRestartAnswered: "t1"}, ""},
	} {
		if got := farmRestartPending(tc.fields); got != tc.want {
			t.Errorf("farmRestartPending(%v) = %q, want %q", tc.fields, got, tc.want)
		}
	}
}

// The garden's restart of a stale gardener (gd-LUmZlr6CKM) is answered on the agent bead: coop's in-place session restart
// when its pod answers, else a sentence saying why not, and never a pod delete. Each request is acted on once, however many
// queued AgentUpdates still carry it unanswered, and a request seen too late is answered without acting.
func TestHandleFarmRestart(t *testing.T) {
	now := time.Now()
	stamp := func(d time.Duration) string { return now.Add(-d).UTC().Format(time.RFC3339Nano) }
	for _, tc := range []struct {
		name   string
		req    string
		pods   *stubPodGetter
		coop   int
		posts  int32
		result string
	}{
		{"taken", stamp(5 * time.Second), &stubPodGetter{pod: runningPodWithIP("10.0.0.7")}, http.StatusAccepted, 1, beadsapi.RestartResultOK},
		{"coop refuses", stamp(5 * time.Second), &stubPodGetter{pod: runningPodWithIP("10.0.0.7")}, http.StatusConflict, 1, "Its session did not answer the restart"},
		{"no pod", stamp(5 * time.Second), &stubPodGetter{err: errors.New("not found")}, http.StatusAccepted, 0, "Its pod is gone"},
		{"no address", stamp(5 * time.Second), &stubPodGetter{pod: &corev1.Pod{}}, http.StatusAccepted, 0, "Its pod has no address yet"},
		{"too late", stamp(3 * time.Minute), &stubPodGetter{pod: runningPodWithIP("10.0.0.7")}, http.StatusAccepted, 0, "too late"},
		{"unreadable", "soon", &stubPodGetter{pod: runningPodWithIP("10.0.0.7")}, http.StatusAccepted, 0, "no time"},
	} {
		var posts atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && r.URL.Path == "/api/v1/session/restart" {
				posts.Add(1)
			}
			w.WriteHeader(tc.coop)
		}))
		client := &http.Client{Transport: &rewriteTransport{targetHost: strings.TrimPrefix(srv.URL, "http://")}}
		beads := &stubFieldUpdater{}
		id := "gd-" + strings.ReplaceAll(tc.name, " ", "-")
		for range 3 { // the request's own AgentUpdate and two queued behind it
			handleFarmRestart(context.Background(), slog.Default(), tc.pods, beads, client, id, tc.req, "oidc:op@example.com",
				"crew-gardenio-crew-x", "gardenio", "gardenio-x", now)
		}
		srv.Close()
		if got := posts.Load(); got != tc.posts {
			t.Errorf("%s: %d session restarts, want %d", tc.name, got, tc.posts)
		}
		if beads.updated[beadsapi.FieldRestartAnswered] != tc.req {
			t.Errorf("%s: answered %q, want the request %q", tc.name, beads.updated[beadsapi.FieldRestartAnswered], tc.req)
		}
		if r := beads.updated[beadsapi.FieldRestartResult]; !strings.Contains(r, tc.result) {
			t.Errorf("%s: result %q, want %q", tc.name, r, tc.result)
		}
		if _, stamped := beads.updated[controllerRestartReasonField]; stamped != (tc.result == beadsapi.RestartResultOK) {
			t.Errorf("%s: controller_restart_reason stamped=%v, want only on a restart taken", tc.name, stamped)
		}
	}

	// A new request on the same bead is a new restart.
	srv, client := coopStubServer(t, http.StatusAccepted)
	defer srv.Close()
	beads := &stubFieldUpdater{}
	pods := &stubPodGetter{pod: runningPodWithIP("10.0.0.7")}
	for _, req := range []string{stamp(20 * time.Second), stamp(10 * time.Second)} {
		handleFarmRestart(context.Background(), slog.Default(), pods, beads, client, "gd-again", req, "", "p", "ns", "a", now)
		if beads.updated[beadsapi.FieldRestartAnswered] != req || beads.updated[beadsapi.FieldRestartResult] != beadsapi.RestartResultOK {
			t.Errorf("request %s: answer %v", req, beads.updated)
		}
	}
}
