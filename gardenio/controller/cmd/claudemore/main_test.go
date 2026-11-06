package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// runToReady starts run() in a goroutine and returns once /readyz answers or the
// deadline passes. It cancels the returned context to shut run() down.
func startRun(t *testing.T, cfg config) (cancel context.CancelFunc, done chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done = make(chan error, 1)
	go func() { done <- run(ctx, cfg, testLogger()) }()
	// Poll readiness.
	base := "http://" + strings.TrimPrefix(cfg.listenAddr, ":")
	if strings.HasPrefix(cfg.listenAddr, ":") {
		base = "http://127.0.0.1" + cfg.listenAddr
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/readyz")
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return cancel, done
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	t.Fatal("run() did not become ready in time")
	return cancel, done
}

// TestRun_PassthroughNoRecorder starts the server with no S3 bucket (recorder
// off) and verifies it proxies + shuts down cleanly on context cancel.
func TestRun_PassthroughNoRecorder(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("upstream-ok"))
	}))
	defer upstream.Close()

	cfg := config{
		listenAddr:  "127.0.0.1:18142",
		upstreamURL: upstream.URL,
		logLevel:    "error",
		// s3Bucket empty → strict pass-through branch.
	}
	cancel, done := startRun(t, cfg)

	resp, err := http.Post("http://127.0.0.1:18142/v1/messages", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("proxied request: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "upstream-ok" {
		t.Errorf("pass-through body: got %q", body)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("run returned error: %v", err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("run did not shut down after cancel")
	}
}

// TestRun_RecorderAttachDegradesGracefully sets a bucket but a bogus region/no
// creds path; the recorder attach still succeeds (client build is lazy) and the
// server serves. This exercises the recorder-attach branch of run().
func TestRun_RecorderAttachBranch(t *testing.T) {
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	cfg := config{
		listenAddr:  "127.0.0.1:18143",
		upstreamURL: upstream.URL,
		logLevel:    "error",
		s3Bucket:    "test-bucket",
		s3Region:    "us-east-1",
	}
	cancel, done := startRun(t, cfg)

	resp, err := http.Get("http://127.0.0.1:18143/healthz")
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	cancel()
	select {
	case <-done:
	case <-time.After(6 * time.Second):
		t.Fatal("run did not shut down after cancel")
	}
}

func TestRun_BadUpstreamErrors(t *testing.T) {
	err := run(context.Background(), config{listenAddr: "127.0.0.1:18144", upstreamURL: "://bad"}, testLogger())
	if err == nil {
		t.Error("run with malformed upstream should error")
	}
}
