// Command freeze-interrupt-probe is a TEST-ONLY driver for the interruptible-
// freeze E2E module (test-gvisor-freeze-interrupt.sh, task ka-0000004367). It
// exercises the REAL production C2 control-plane — podmanager.FreezeThawManager's
// in-flight registry + abort defer + discardAbortedFreeze image reap, its
// AbortFreeze, and the shared podmanager.FreezeInterruptHandler that backs the
// controller's POST /api/v1/freeze/interrupt endpoint — against a REAL
// `runsc checkpoint --leave-running` of a live gVisor sandbox on the gvisor-poc
// substrate.
//
// # Why this exists (the gap it closes)
//
// The C2 interruptible-freeze path (ka-0000005834) was UNIT-tested only
// (freezethaw_interrupt_test.go, freeze_interrupt_endpoint_test.go). The
// standing user-response-latency gate's MID-FREEZE leg only times a
// LEAVE_RUNNING_ONLY node checkpoint — it never injects a message, never hits
// the abort endpoint, and never asserts the post-abort invariants. NOTHING drove
// a REAL freeze on real runsc, fired a REAL interrupt through the genuine handler
// mid-checkpoint, and asserted the five C2 guarantees end-to-end:
//
//	(a) the freeze is ABORTED — Freeze returns podmanager.ErrFreezeAborted, the
//	    node-checkpoint exec ctx was cancelled by the interrupt;
//	(b) the live sandbox (Pod A's analogue) is STILL RUNNING — `--leave-running`
//	    kept it alive across the checkpoint + abort;
//	(c) the agent is still SERVING — its in-RAM monotonic counter advances past
//	    its checkpoint-time value (it never went dark), and the endpoint reported
//	    aborted=true (the message would route to this live agent, no thaw);
//	(d) NO orphaned checkpoint image is left on the node — discardAbortedFreeze
//	    reaped the partial/complete image at podmanager.FreezeImagePath(agent);
//	(e) NO snapshot was persisted — the store has no thaw target for the agent.
//
// This probe is that missing piece: a regression in the registry/abort/defer/
// reap machinery or the endpoint contract fails THIS E2E, not just the next prod
// canary.
//
// # What it does
//
// The probe runs ON THE NODE (shipped + invoked the same way thawepoch-probe is).
// It drives the genuine FreezeThawManager.Freeze in a goroutine with a custom
// NodeExecutor that performs a REAL `runsc checkpoint --leave-running` of the
// pre-built live sandbox SC_CID (the freeze-interrupt-probe NodeExecutor is the
// one necessary deviation from the controller's own freeze script, which resolves
// a CRI pod sandbox by name — the bare PoC `sc-agent` cid has no CRI annotations.
// Every OTHER piece is the genuine controller code). Once the real checkpoint has
// written its image and left the sandbox running, the exec BLOCKS — modelling the
// controller still inside its post-checkpoint relocate/verify window, the exact
// window the C2 interrupt targets. The probe then fires a REAL HTTP
// POST /api/v1/freeze/interrupt against an in-process server wired to the SHARED
// podmanager.FreezeInterruptHandler(mgr.AbortFreeze), which cancels the freeze
// ctx; the genuine abort defer maps the cancelled exec to ErrFreezeAborted and
// runs discardAbortedFreeze → reapImageAt. The probe then asserts (a)–(e) by
// querying live node state (runsc state, the agent log counter, the image path)
// and the genuine Freeze return + store.
//
// It is NOT part of any release Makefile / Dockerfile target — it is built from
// source on demand inside the E2E module (the same way thawepoch-probe is) and
// never ships in a product image.
//
// Output is a single `FREEZE_INTERRUPT_PROBE …` line of key=value pairs on
// stdout (parsed by the shell module) plus human-readable detail on stderr. Exit
// code is 0 iff every asserted invariant held.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"gardenio/controller/internal/podmanager"
)

func main() {
	var (
		agent      = flag.String("agent", "freeze-interrupt-probe-agent", "agent identity (snapshot + image-path key)")
		scCID      = flag.String("sc-cid", "sc-agent", "live runsc sandbox cid to checkpoint (built by RUN_ONLY)")
		bdir       = flag.String("bundle-dir", "/var/tmp/sc", "node bundle dir whose work/agent.log carries the liveness counter")
		runscRoot  = flag.String("runsc-root", "/run/containerd/runsc/k8s.io", "runsc state root (CRI runtime root)")
		node       = flag.String("node", "gvisor-poc-node", "node name stamped on the fake Pod A (cosmetic; the exec is node-local)")
		runscTOsec = flag.Int("runsc-timeout-sec", 60, "per runsc-call wall-clock cap (seconds)")
		settleMS   = flag.Int64("checkpoint-settle-ms", 1500, "after the real checkpoint, advance the live agent this long before the interrupt, so (c) has a counter delta to assert")
		waitMS     = flag.Int64("interrupt-wait-ms", 15000, "max wait for the in-flight freeze to register before the interrupt")
	)
	flag.Parse()

	p := &prober{
		agent:     *agent,
		scCID:     *scCID,
		bdir:      *bdir,
		runscRoot: *runscRoot,
		node:      *node,
		runscTO:   time.Duration(*runscTOsec) * time.Second,
		settle:    time.Duration(*settleMS) * time.Millisecond,
		wait:      time.Duration(*waitMS) * time.Millisecond,
		logger:    slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	}
	p.run()
}

type prober struct {
	agent     string
	scCID     string
	bdir      string
	runscRoot string
	node      string
	runscTO   time.Duration
	settle    time.Duration
	wait      time.Duration
	logger    *slog.Logger
}

// nsenterRunsc runs `runsc --root=<root> <args...>` in PID 1's host namespaces
// (the CRI runtime root) under a wall-clock timeout — the same invocation shape
// the controller's freeze script and the PoC node scripts use. Returns combined
// output.
func (p *prober) nsenterRunsc(ctx context.Context, args ...string) (string, error) {
	full := append([]string{"-t", "1", "-m", "-p", "-n", "--",
		"timeout", strconv.Itoa(int(p.runscTO.Seconds())),
		"runsc", "--root=" + p.runscRoot}, args...)
	cmd := exec.CommandContext(ctx, "nsenter", full...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

// sandboxStatus returns the runsc `state` status string for the cid ("running",
// "paused", …) or "" if it cannot be resolved.
func (p *prober) sandboxStatus(ctx context.Context) string {
	out, err := p.nsenterRunsc(ctx, "state", p.scCID)
	if err != nil {
		return ""
	}
	var st struct {
		Status string `json:"status"`
	}
	if jerr := json.Unmarshal([]byte(out), &st); jerr != nil {
		return ""
	}
	return st.Status
}

// agentCounter reads the last line of the live agent's log and returns its
// monotonic counter — the liveness signal. -1 if unreadable.
func (p *prober) agentCounter() int64 {
	data, err := os.ReadFile(p.bdir + "/work/agent.log")
	if err != nil {
		return -1
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		var rec struct {
			Counter int64 `json:"counter"`
		}
		if json.Unmarshal([]byte(line), &rec) == nil {
			return rec.Counter
		}
	}
	return -1
}

// imagePresent reports whether the checkpoint image dir holds a checkpoint.img,
// queried in the host mount ns (the ns runsc wrote it through).
func (p *prober) imagePresent(ctx context.Context, imagePath string) bool {
	cmd := exec.CommandContext(ctx, "nsenter", "-t", "1", "-m", "--",
		"test", "-f", imagePath+"/checkpoint.img")
	return cmd.Run() == nil
}

func (p *prober) run() {
	imagePath := podmanager.FreezeImagePath(p.agent)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Genuine FreezeThawManager wired exactly as the controller's lifecycle wires
	// it for an interrupt-enabled freeze: real in-memory snapshot store, interrupt
	// armed. The NodeExecutor is the probe's real-runsc checkpoint driver.
	store := podmanager.NewInMemorySnapshotStore()
	xc := &checkpointExec{
		prober:    p,
		imagePath: imagePath,
		started:   make(chan struct{}),
	}
	mgr := podmanager.NewFreezeThawManager(
		fake.NewClientset(livePodA(p.agent, p.node)),
		xc,
		store,
		nil, // RehydrationTrigger unused on the freeze/abort path
		p.logger,
	)
	mgr.InterruptEnabled = true
	mgr.RunscTimeout = p.runscTO

	// Stand up the GENUINE endpoint handler (the same code the controller serves)
	// over an in-process HTTP server, so the interrupt travels the real wire path.
	srv := httptest.NewServer(podmanager.FreezeInterruptHandler(mgr.AbortFreeze, p.logger))
	defer srv.Close()

	// Drive the genuine Freeze in the background. It registers the in-flight
	// freeze, execs our real `runsc checkpoint --leave-running`, then blocks in the
	// exec until the interrupt cancels its ctx (or the checkpoint genuinely fails).
	type freezeResult struct {
		snap *podmanager.Snapshot
		err  error
	}
	done := make(chan freezeResult, 1)
	go func() {
		snap, _, err := mgr.Freeze(ctx, podmanager.FreezeRequest{
			AgentName: p.agent,
			BeadID:    "ka-0000004367",
			PodName:   p.agent,
			Namespace: "gvisor-poc",
			Node:      p.node,
		})
		done <- freezeResult{snap, err}
	}()

	// Wait until the real checkpoint has been taken and the exec is now blocking
	// (the in-flight window the interrupt targets). xc.started closes once the
	// checkpoint returned and the sandbox was left running.
	select {
	case <-xc.started:
	case <-time.After(p.wait):
		p.fail("freeze did not reach the in-flight blocking window within %s (checkpoint never started?)", p.wait)
	}

	// Precondition for a meaningful (d): the real checkpoint actually wrote an
	// image at the controller-owned path. If it did not, (d) would pass vacuously.
	imgBefore := p.imagePresent(ctx, imagePath)

	// (b)-pre and (c)-pre: the sandbox is running and the agent counter is live.
	statusMid := p.sandboxStatus(ctx)
	if !mgr.IsFreezing(p.agent) {
		p.fail("IsFreezing(%s)=false while the checkpoint exec is blocking — the in-flight registry was not populated (InterruptEnabled off?)", p.agent)
	}
	// Let the still-live agent advance its counter so (c) has a real delta: the
	// agent must keep serving WHILE the freeze sits in flight.
	counterPre := p.agentCounter()
	time.Sleep(p.settle)
	counterMid := p.agentCounter()

	// Fire the REAL interrupt through the genuine endpoint.
	aborted, code := p.postInterrupt(srv.URL)
	if code != http.StatusOK {
		p.fail("POST /api/v1/freeze/interrupt returned %d, want 200 (in-flight freeze should abort)", code)
	}
	if !aborted {
		p.fail("endpoint returned aborted=false; want true (an in-flight freeze was present)")
	}

	// (a): the genuine Freeze must return ErrFreezeAborted, and its node exec ctx
	// must have been cancelled by the interrupt.
	var res freezeResult
	select {
	case res = <-done:
	case <-time.After(p.runscTO + 10*time.Second):
		p.fail("Freeze did not return after the interrupt (exec ctx not cancelled?)")
	}
	abortedErr := errors.Is(res.err, podmanager.ErrFreezeAborted)
	execCanceled := xc.wasCanceled()
	snapNil := res.snap == nil

	// (b): the sandbox must still be running after the abort (--leave-running kept
	// Pod A's analogue alive; the abort is a clean no-op on the live agent).
	statusAfter := p.sandboxStatus(ctx)

	// (c): the agent kept serving — its counter advanced past the checkpoint-time
	// value while the freeze was in flight, and again after the abort.
	counterPost := p.agentCounter()
	advancedDuringFreeze := counterMid > counterPre && counterPre >= 0
	advancedAfterAbort := counterPost > counterMid

	// (d): no orphaned image — discardAbortedFreeze reaped FreezeImagePath. Give
	// the reap (a node exec under a fresh ctx) a moment to land.
	imgAfter := p.imagePresent(ctx, imagePath)
	for i := 0; i < 20 && imgAfter; i++ {
		time.Sleep(150 * time.Millisecond)
		imgAfter = p.imagePresent(ctx, imagePath)
	}

	// (e): no snapshot persisted — the store has no thaw target for the agent.
	_, getErr := store.Get(ctx, p.agent)
	noSnapshot := getErr != nil

	// Emit the machine-readable result line.
	fmt.Printf("FREEZE_INTERRUPT_PROBE agent=%s aborted_err=%t exec_canceled=%t snap_nil=%t "+
		"endpoint_aborted=%t status_mid=%s status_after=%s img_before=%t img_after=%t "+
		"counter_pre=%d counter_mid=%d counter_post=%d no_snapshot=%t\n",
		p.agent, abortedErr, execCanceled, snapNil,
		aborted, orUnknown(statusMid), orUnknown(statusAfter), imgBefore, imgAfter,
		counterPre, counterMid, counterPost, noSnapshot)

	// Assert every invariant; collect all failures so one run reports them all.
	var fails []string
	if !abortedErr {
		fails = append(fails, fmt.Sprintf("(a) Freeze err=%v, want ErrFreezeAborted", res.err))
	}
	if !execCanceled {
		fails = append(fails, "(a) node-checkpoint exec ctx was not cancelled by the interrupt")
	}
	if !snapNil {
		fails = append(fails, fmt.Sprintf("(a) Freeze returned a non-nil snapshot %+v; want nil on abort", res.snap))
	}
	if statusAfter != "running" {
		fails = append(fails, fmt.Sprintf("(b) sandbox status after abort=%q, want \"running\" (--leave-running must keep the live agent up)", statusAfter))
	}
	if !imgBefore {
		fails = append(fails, "(d-pre) the real checkpoint wrote NO image — (d) would be vacuous; the checkpoint substrate is broken")
	}
	if !advancedDuringFreeze {
		fails = append(fails, fmt.Sprintf("(c) agent counter did not advance while the freeze was in flight (pre=%d mid=%d) — the agent went dark", counterPre, counterMid))
	}
	if !advancedAfterAbort {
		fails = append(fails, fmt.Sprintf("(c) agent counter did not advance after the abort (mid=%d post=%d) — the agent is not serving live", counterMid, counterPost))
	}
	if imgAfter {
		fails = append(fails, fmt.Sprintf("(d) orphaned checkpoint image STILL present at %s after the abort — discardAbortedFreeze did not reap it", imagePath))
	}
	if !noSnapshot {
		fails = append(fails, "(e) a snapshot was persisted despite the abort; want none (no thaw target for a live agent)")
	}

	if len(fails) > 0 {
		for _, f := range fails {
			fmt.Fprintf(os.Stderr, "  ✗ %s\n", f)
		}
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "  ✓ interruptible freeze proven end-to-end: real checkpoint aborted via the genuine endpoint; "+
		"sandbox stayed running (status=%s), agent counter advanced %d→%d→%d, image reaped at %s, no snapshot persisted\n",
		statusAfter, counterPre, counterMid, counterPost, imagePath)
	os.Exit(0)
}

// postInterrupt fires the real POST /api/v1/freeze/interrupt and returns
// (aborted, statusCode).
func (p *prober) postInterrupt(base string) (bool, int) {
	body, _ := json.Marshal(podmanager.FreezeInterruptRequest{Agent: p.agent})
	req, err := http.NewRequest(http.MethodPost, base+"/api/v1/freeze/interrupt", bytes.NewReader(body))
	if err != nil {
		p.fail("build interrupt request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Actor", "freeze-interrupt-probe")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		p.fail("POST interrupt: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	rb, _ := io.ReadAll(resp.Body)
	var out podmanager.FreezeInterruptResponse
	_ = json.Unmarshal(rb, &out)
	return out.Aborted, resp.StatusCode
}

func (p *prober) fail(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	fmt.Printf("FREEZE_INTERRUPT_PROBE error=%q\n", msg)
	fmt.Fprintf(os.Stderr, "  ✗ %s\n", msg)
	os.Exit(1)
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// livePodA is the minimal fake Pod A the genuine Freeze resolves for node
// resolution + the duplicate-snapshot guard. Running phase, assigned node.
func livePodA(name, node string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "gvisor-poc"},
		Spec:       corev1.PodSpec{NodeName: node},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// checkpointExec is the probe's NodeExecutor. It is the ONE necessary deviation
// from the controller's own freeze script: that script resolves a CRI pod
// sandbox by name, but the PoC `sc-agent` is a bare `runsc run` cid with no CRI
// annotations. So instead of running the controller's resolution script, the
// freeze branch performs a REAL `runsc checkpoint --leave-running` of the live
// sandbox directly, then BLOCKS until the freeze ctx is cancelled by the
// interrupt — modelling the controller still inside its post-checkpoint
// relocate/verify window. The REAP branch (the script the genuine
// discardAbortedFreeze builds) is executed verbatim via bash, so the actual
// image reclaim is the genuine controller reaper, not a re-implementation.
type checkpointExec struct {
	prober    *prober
	imagePath string
	started   chan struct{} // closed once the real checkpoint returned + sandbox left running

	mu          sync.Mutex
	startedOnce sync.Once
	execCount   int
	ctxCanceled bool
}

func (x *checkpointExec) Exec(ctx context.Context, node, script string) (podmanager.NodeExecResult, error) {
	x.mu.Lock()
	x.execCount++
	first := x.execCount == 1
	x.mu.Unlock()

	// The reaper script (built by discardAbortedFreeze → buildReapImageScript)
	// carries the FT_IMAGE_REAPED sentinel. Run it for real so the genuine reaper
	// actually rm's the image on the node, and surface its output.
	if !first || strings.Contains(script, "FT_IMAGE_REAPED") {
		cmd := exec.Command("bash", "-c", script) //nolint:gosec // test-only probe; script is the controller's own reaper template
		var buf bytes.Buffer
		cmd.Stdout = &buf
		cmd.Stderr = &buf
		_ = cmd.Run()
		return podmanager.NodeExecResult{Stdout: buf.String()}, nil
	}

	// FREEZE branch: a REAL `runsc checkpoint --leave-running` of the live sandbox
	// into the controller-owned image path. Use a background ctx for the checkpoint
	// itself so the cancel does not abort the (fast) image write mid-flight — we
	// want a fully-written real image at imagePath, so (d)'s reap assertion is
	// strong (a complete checkpoint image really does get reaped by the abort
	// path). The ABORT is then asserted by BLOCKING on the freeze ctx below.
	mkctx, mkcancel := context.WithTimeout(context.Background(), x.prober.runscTO)
	_ = exec.CommandContext(mkctx, "nsenter", "-t", "1", "-m", "--", "mkdir", "-p", x.imagePath).Run()
	ckArgs := []string{"checkpoint", "--leave-running", "--image-path=" + x.imagePath, x.prober.scCID}
	if out, err := x.prober.nsenterRunsc(mkctx, ckArgs...); err != nil {
		mkcancel()
		// A real checkpoint failure is NOT the abort path — surface it so the probe
		// fails loudly rather than masquerading as a clean interrupt.
		return podmanager.NodeExecResult{Stdout: out, Stderr: out, ExitCode: 1},
			fmt.Errorf("real runsc checkpoint of %s failed: %w: %s", x.prober.scCID, err, tail(out, 400))
	}
	mkcancel()

	// The in-flight checkpoint has been written and the sandbox left running — we
	// are now in the post-checkpoint window the C2 interrupt targets. Signal the
	// probe and block until the interrupt cancels the freeze ctx.
	x.startedOnce.Do(func() { close(x.started) })
	<-ctx.Done()
	x.mu.Lock()
	x.ctxCanceled = true
	x.mu.Unlock()
	return podmanager.NodeExecResult{}, ctx.Err()
}

func (x *checkpointExec) wasCanceled() bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.ctxCanceled
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
