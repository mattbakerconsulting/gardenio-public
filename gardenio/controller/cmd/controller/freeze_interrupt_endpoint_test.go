package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"gardenio/controller/internal/config"
	"gardenio/controller/internal/podmanager"
)

// interruptLivePod is a minimal runsc pod on a node — enough for Freeze's pod
// Get + node resolution before it blocks in the (faked) node exec.
func interruptLivePod(name, ns, node string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       corev1.PodSpec{NodeName: node},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// TestHandleFreezeInterrupt covers the C2 ingress endpoint (ka-0000005834):
//   - nil lifecycle / flag-off → 404 {"aborted":false} (no freeze to interrupt)
//   - bad body → 400
//   - in-flight freeze → 200 {"aborted":true} and the freeze is signalled
func TestHandleFreezeInterrupt(t *testing.T) {
	t.Run("nil lifecycle returns 404 aborted=false", func(t *testing.T) {
		h := handleFreezeInterrupt(nil, discardLogger())
		rr := doInterrupt(t, h, `{"agent":"a"}`)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rr.Code)
		}
		if got := decodeAborted(t, rr); got {
			t.Errorf("aborted = true, want false on nil lifecycle")
		}
	})

	t.Run("bad body returns 400", func(t *testing.T) {
		h := handleFreezeInterrupt(nil, discardLogger())
		rr := doInterrupt(t, h, `{"not_agent":"x"}`)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rr.Code)
		}
	})

	t.Run("flag off returns 404", func(t *testing.T) {
		// Lifecycle present but InterruptEnabled off → registry empty → 404.
		cfg := &config.Config{FreezeThawEnabled: true}
		ft := newFreezeThawLifecycle(cfg, fake.NewClientset(), fakeNodeExec{}, discardLogger())
		h := handleFreezeInterrupt(ft, discardLogger())
		rr := doInterrupt(t, h, `{"agent":"a"}`)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 when interrupt disabled", rr.Code)
		}
	})

	t.Run("in-flight freeze aborts and returns 200", func(t *testing.T) {
		cfg := &config.Config{FreezeThawEnabled: true, FreezeInterruptEnabled: true}
		exec := &interruptBlockingExec{started: make(chan struct{})}
		client := fake.NewClientset(interruptLivePod("agentsim", "ns", "node-1"))
		ft := newFreezeThawLifecycle(cfg, client, exec, discardLogger())

		// Drive a freeze that blocks inside the node exec, so the agent is
		// registered in the in-flight registry while we POST the interrupt.
		freezeDone := make(chan struct{})
		go func() {
			_, _, _ = ft.freeze(context.Background(), "agentsim", "gd-bead", "agentsim", "ns")
			close(freezeDone)
		}()
		<-exec.started

		h := handleFreezeInterrupt(ft, discardLogger())
		rr := doInterrupt(t, h, `{"agent":"agentsim"}`)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (in-flight freeze aborted)", rr.Code)
		}
		if !decodeAborted(t, rr) {
			t.Errorf("aborted = false, want true")
		}
		<-freezeDone // the abort must unblock the freeze
	})
}

// interruptBlockingExec blocks the first Exec until its ctx is cancelled (the
// abort), so the freeze stays registered in flight while the test POSTs the
// interrupt endpoint. Later Exec calls (image reaper, if any) return promptly.
type interruptBlockingExec struct {
	started chan struct{}
	once    bool
}

func (e *interruptBlockingExec) Exec(ctx context.Context, _ string, _ string) (podmanager.NodeExecResult, error) {
	if e.once {
		return podmanager.NodeExecResult{Stdout: "FT_IMAGE_REAPED=yes"}, nil
	}
	e.once = true
	close(e.started)
	<-ctx.Done()
	return podmanager.NodeExecResult{}, ctx.Err()
}

func doInterrupt(t *testing.T, h http.HandlerFunc, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/freeze/interrupt", strings.NewReader(body))
	rr := httptest.NewRecorder()
	h(rr, req)
	return rr
}

func decodeAborted(t *testing.T, rr *httptest.ResponseRecorder) bool {
	t.Helper()
	var resp struct {
		Aborted bool `json:"aborted"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body=%q)", err, rr.Body.String())
	}
	return resp.Aborted
}

// ensure podmanager import is used even if the in-flight assertions change.
var _ = podmanager.ErrFreezeAborted
