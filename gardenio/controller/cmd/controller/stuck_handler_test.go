package main

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"gardenio/controller/internal/podmanager"
)

type fakeStatusFetcher struct {
	status string
	err    error
}

func (f *fakeStatusFetcher) GetBeadStatus(_ context.Context, _ string) (string, error) {
	return f.status, f.err
}

// TestBeadIsClosed_ClosedStatus verifies the AgentStuck handler's closed-bead
// guard treats status="closed" as closed. Without this, the controller
// recreates pods for agents whose beads have already been torn down,
// producing "spec.containers[0].image: Required value" errors when the
// project/role cache no longer holds an image entry (ka-0000005268).
func TestBeadIsClosed_ClosedStatus(t *testing.T) {
	f := &fakeStatusFetcher{status: "closed"}
	if !beadIsClosed(context.Background(), f, "gd-foo") {
		t.Fatal("beadIsClosed returned false for status=closed")
	}
}

func TestBeadIsClosed_OpenStatus(t *testing.T) {
	for _, s := range []string{"open", "in_progress", "ready", ""} {
		f := &fakeStatusFetcher{status: s}
		if beadIsClosed(context.Background(), f, "gd-foo") {
			t.Fatalf("beadIsClosed returned true for status=%q", s)
		}
	}
}

// TestBeadIsClosed_ErrorTreatedAsOpen verifies daemon errors fall through to
// the recreate path — the server-side filter is the safety net for true
// closed beads, matching the ka-0000005253 statusreporter precedent.
func TestBeadIsClosed_ErrorTreatedAsOpen(t *testing.T) {
	f := &fakeStatusFetcher{err: errors.New("daemon down")}
	if beadIsClosed(context.Background(), f, "gd-foo") {
		t.Fatal("beadIsClosed returned true on daemon error; expected false")
	}
}

func TestBeadIsClosed_NilOrEmpty(t *testing.T) {
	if beadIsClosed(context.Background(), nil, "gd-foo") {
		t.Fatal("beadIsClosed returned true for nil fetcher")
	}
	f := &fakeStatusFetcher{status: "closed"}
	if beadIsClosed(context.Background(), f, "") {
		t.Fatal("beadIsClosed returned true for empty bead ID")
	}
}

// rewriteTransport redirects every request to targetHost, preserving only the
// path, so a test can assert restartCoopSessionInPlace posts to the right path
// without depending on the hardcoded coop port (the helper builds its own URL
// against podmanager.CoopDefaultPort, which the test server does not listen on).
type rewriteTransport struct {
	targetHost string // host:port of the httptest server
	gotPath    string
	gotMethod  string
}

func (rt *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.gotPath = req.URL.Path
	rt.gotMethod = req.Method
	req.URL.Scheme = "http"
	req.URL.Host = rt.targetHost
	return http.DefaultTransport.RoundTrip(req)
}

// TestRestartCoopSessionInPlace_Success verifies the helper POSTs to coop's
// session-restart endpoint and treats a 202 Accepted as success — the
// non-destructive stuck remedy (ka-0000001648).
func TestRestartCoopSessionInPlace_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	rt := &rewriteTransport{targetHost: strings.TrimPrefix(srv.URL, "http://")}
	client := &http.Client{Transport: rt}
	if err := restartCoopSessionInPlace(context.Background(), client, "10.0.0.1"); err != nil {
		t.Fatalf("expected success on 202, got %v", err)
	}
	if rt.gotMethod != http.MethodPost {
		t.Fatalf("expected POST, got %s", rt.gotMethod)
	}
	if rt.gotPath != "/api/v1/session/restart" {
		t.Fatalf("expected path /api/v1/session/restart, got %s", rt.gotPath)
	}
}

// TestRestartCoopSessionInPlace_Non2xx verifies a non-2xx coop response is a
// failure so the caller falls back to delete+recreate.
func TestRestartCoopSessionInPlace_Non2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// 409 SwitchInProgress is the realistic non-2xx coop can return.
		w.WriteHeader(http.StatusConflict)
	}))
	defer srv.Close()

	rt := &rewriteTransport{targetHost: strings.TrimPrefix(srv.URL, "http://")}
	client := &http.Client{Transport: rt}
	if err := restartCoopSessionInPlace(context.Background(), client, "10.0.0.1"); err == nil {
		t.Fatal("expected error on 409, got nil")
	}
}

// TestRestartCoopSessionInPlace_EmptyIP verifies an empty pod IP is a failure
// (nothing to POST to) so the caller falls back rather than posting to a
// malformed URL.
func TestRestartCoopSessionInPlace_EmptyIP(t *testing.T) {
	if err := restartCoopSessionInPlace(context.Background(), http.DefaultClient, ""); err == nil {
		t.Fatal("expected error for empty pod IP, got nil")
	}
}

// TestRestartCoopSessionInPlace_Unreachable verifies a transport error (coop
// dead / unreachable) is surfaced so the caller falls back to the destructive
// remedy — the only path that can replace a pod whose coop is gone.
func TestRestartCoopSessionInPlace_Unreachable(t *testing.T) {
	// 203.0.113.0/24 is TEST-NET-3 (RFC 5737) — guaranteed unroutable. A short
	// client timeout keeps the test fast.
	client := &http.Client{Timeout: 500_000_000} // 500ms
	if err := restartCoopSessionInPlace(context.Background(), client, "203.0.113.1"); err == nil {
		t.Fatal("expected transport error for unreachable coop, got nil")
	}
}

// --- tryStuckRestartInPlace (the remedy decision) ---------------------------

type stubPodGetter struct {
	pod *corev1.Pod
	err error
}

func (s *stubPodGetter) GetAgentPod(_ context.Context, _, _ string) (*corev1.Pod, error) {
	return s.pod, s.err
}

type stubFieldUpdater struct {
	updated map[string]string
	err     error
}

func (s *stubFieldUpdater) UpdateBeadFields(_ context.Context, _ string, fields map[string]string) error {
	if s.err != nil {
		return s.err
	}
	if s.updated == nil {
		s.updated = map[string]string{}
	}
	maps.Copy(s.updated, fields)
	return nil
}

func runningPodWithIP(ip string) *corev1.Pod {
	return &corev1.Pod{Status: corev1.PodStatus{PodIP: ip}}
}

// coopStubServer returns an httptest server plus a client whose transport
// rewrites every request to it, so tryStuckRestartInPlace (which builds its URL
// against podmanager.CoopDefaultPort) reaches the test server regardless of the
// pod IP or port it computes.
func coopStubServer(t *testing.T, status int) (*httptest.Server, *http.Client) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
	}))
	rt := &rewriteTransport{targetHost: strings.TrimPrefix(srv.URL, "http://")}
	return srv, &http.Client{Transport: rt}
}

// TestTryStuckRestartInPlace_Success is the happy path: a running pod with an IP
// whose coop accepts the restart. The helper returns true (caller must NOT
// delete the pod) and stamps the reason on the bead (ka-0000001648).
func TestTryStuckRestartInPlace_Success(t *testing.T) {
	srv, client := coopStubServer(t, http.StatusAccepted)
	defer srv.Close()

	pods := &stubPodGetter{pod: runningPodWithIP("10.1.2.3")}
	beads := &stubFieldUpdater{}
	ok := tryStuckRestartInPlace(context.Background(), discardLogger(), pods, beads, client,
		"crew-gardenio-crew-x", "ns", "gardenio-x", "gd-x")
	if !ok {
		t.Fatal("expected true (in-place restart succeeded), got false")
	}
	if beads.updated[controllerRestartReasonField] != stuckInPlaceRestartReason {
		t.Fatalf("expected restart reason stamped, got %q", beads.updated[controllerRestartReasonField])
	}
}

// TestTryStuckRestartInPlace_GetPodError falls back (returns false) when the pod
// cannot be fetched — the destructive path is the only remedy then.
func TestTryStuckRestartInPlace_GetPodError(t *testing.T) {
	pods := &stubPodGetter{err: errors.New("api down")}
	ok := tryStuckRestartInPlace(context.Background(), discardLogger(), pods, &stubFieldUpdater{}, http.DefaultClient,
		"crew-gardenio-crew-x", "ns", "gardenio-x", "gd-x")
	if ok {
		t.Fatal("expected false (fall back to delete+recreate) on GetAgentPod error")
	}
}

// TestTryStuckRestartInPlace_NoIP falls back when the pod has no routable IP —
// there is nothing to POST to.
func TestTryStuckRestartInPlace_NoIP(t *testing.T) {
	pods := &stubPodGetter{pod: runningPodWithIP("")}
	ok := tryStuckRestartInPlace(context.Background(), discardLogger(), pods, &stubFieldUpdater{}, http.DefaultClient,
		"crew-gardenio-crew-x", "ns", "gardenio-x", "gd-x")
	if ok {
		t.Fatal("expected false (fall back) when pod has no IP")
	}
}

// TestTryStuckRestartInPlace_CoopRejects falls back when coop returns a non-2xx
// (e.g. 409 SwitchInProgress or coop is unreachable).
func TestTryStuckRestartInPlace_CoopRejects(t *testing.T) {
	srv, client := coopStubServer(t, http.StatusConflict)
	defer srv.Close()

	pods := &stubPodGetter{pod: runningPodWithIP("10.1.2.3")}
	ok := tryStuckRestartInPlace(context.Background(), discardLogger(), pods, &stubFieldUpdater{}, client,
		"crew-gardenio-crew-x", "ns", "gardenio-x", "gd-x")
	if ok {
		t.Fatal("expected false (fall back) when coop rejects the restart")
	}
}

// TestTryStuckRestartInPlace_StampFailureStillSucceeds verifies a failed reason
// stamp does not undo a successful in-place restart — the pod was already
// restarted, so the caller must still skip the delete.
func TestTryStuckRestartInPlace_StampFailureStillSucceeds(t *testing.T) {
	srv, client := coopStubServer(t, http.StatusAccepted)
	defer srv.Close()

	pods := &stubPodGetter{pod: runningPodWithIP("10.1.2.3")}
	beads := &stubFieldUpdater{err: errors.New("daemon down")}
	ok := tryStuckRestartInPlace(context.Background(), discardLogger(), pods, beads, client,
		"crew-gardenio-crew-x", "ns", "gardenio-x", "gd-x")
	if !ok {
		t.Fatal("expected true even when reason stamp fails; the pod was restarted in place")
	}
}

// assert the narrowed interfaces are satisfied by the real types, so a
// signature drift is caught at compile time.
var (
	_ stuckPodGetter = (podmanager.Manager)(nil)
)
