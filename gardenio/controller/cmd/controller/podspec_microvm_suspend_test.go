package main

import (
	"context"
	"errors"
	"testing"

	"gardenio/controller/internal/config"
	gardeniometrics "gardenio/controller/internal/metrics"
	"gardenio/controller/internal/microvm"
	"gardenio/controller/internal/podmanager"
	"gardenio/controller/internal/subscriber"

	"github.com/prometheus/client_golang/prometheus"
)

// MicroVM native suspend-on-stop eligibility signal (plan ka-0000004518, step 2
// ka-0000007189): the controller injects BOAT_FREEZE_ELIGIBLE=true into the
// forwarded MicroVM env iff MICROVM_SUSPEND_ENABLED is on AND the spec resolved
// to lambda_microvm AND it is a thread agent (bound to a Slack thread). The in-VM
// `gb stop` reads that same signal to transition agent_state=stopping (AgentStop)
// rather than close the bead — the trigger that lets the controller suspend.

// microvmSuspendCfg builds a config whose `gardenio-microvms` role pins the
// lambda_microvm execution target, with the suspend master switch toggled.
func microvmSuspendCfg(suspendEnabled bool) *config.Config {
	return &config.Config{
		Namespace:             "gardenio",
		MicrovmSuspendEnabled: suspendEnabled,
		ProjectCache:          map[string]config.ProjectCacheEntry{},
		RoleCache: map[string]config.RoleCacheEntry{
			"gardenio-microvms": {ExecutionTarget: podmanager.ExecutionTargetLambdaMicroVM},
		},
	}
}

// microvmThreadEvent is a MicroVM thread agent (bound to a Slack thread).
func microvmThreadEvent() subscriber.Event {
	return subscriber.Event{
		Project:   "gardenio",
		Role:      "gardenio-microvms",
		AgentName: "vm-thread",
		Metadata: map[string]string{
			"slack_thread_channel": "C0ABCDEF",
			"slack_thread_ts":      "1784500000.123456",
		},
	}
}

func TestBuildAgentPodSpec_MicrovmSuspend_ThreadAndEnabled(t *testing.T) {
	cfg := microvmSuspendCfg(true)
	spec := buildAgentPodSpec(cfg, microvmThreadEvent())

	if spec.ExecutionTarget != podmanager.ExecutionTargetLambdaMicroVM {
		t.Fatalf("expected lambda_microvm target, got %q", spec.ExecutionTarget)
	}
	if got := spec.Env[EnvFreezeEligible]; got != "true" {
		t.Errorf("%s = %q, want \"true\" for a MicroVM thread agent + suspend enabled", EnvFreezeEligible, got)
	}
}

func TestBuildAgentPodSpec_MicrovmSuspend_ThreadButDisabled(t *testing.T) {
	// MicroVM thread agent, but the master switch is off — NOT eligible.
	cfg := microvmSuspendCfg(false)
	spec := buildAgentPodSpec(cfg, microvmThreadEvent())

	if spec.ExecutionTarget != podmanager.ExecutionTargetLambdaMicroVM {
		t.Fatalf("expected lambda_microvm target, got %q", spec.ExecutionTarget)
	}
	if got, ok := spec.Env[EnvFreezeEligible]; ok {
		t.Errorf("%s set to %q, want UNSET when MICROVM_SUSPEND_ENABLED is off", EnvFreezeEligible, got)
	}
}

func TestBuildAgentPodSpec_MicrovmSuspend_NonThreadStaysIneligible(t *testing.T) {
	// A MicroVM agent with NO thread binding (a one-shot crew/gardener) has no
	// re-mention to resume, so it must NOT be suspend-eligible even with the
	// switch on — it terminates on stop as before.
	cfg := microvmSuspendCfg(true)
	ev := microvmThreadEvent()
	ev.Metadata = map[string]string{} // strip the thread binding

	spec := buildAgentPodSpec(cfg, ev)
	if spec.ExecutionTarget != podmanager.ExecutionTargetLambdaMicroVM {
		t.Fatalf("expected lambda_microvm target, got %q", spec.ExecutionTarget)
	}
	if got, ok := spec.Env[EnvFreezeEligible]; ok {
		t.Errorf("%s set to %q, want UNSET for a non-thread MicroVM agent", EnvFreezeEligible, got)
	}
}

// A Kubernetes (pod) thread agent must NOT get the MicroVM suspend signal from
// the suspend switch — its standby is the gVisor freeze path, gated separately.
func TestBuildAgentPodSpec_MicrovmSuspend_K8sThreadUnaffected(t *testing.T) {
	cfg := microvmSuspendCfg(true) // suspend on, but this agent is not a MicroVM
	ev := subscriber.Event{
		Project:   "gardenio",
		Role:      "crew", // no lambda_microvm role → kubernetes target
		AgentName: "pod-thread",
		Metadata: map[string]string{
			"slack_thread_channel": "C0ABCDEF",
			"slack_thread_ts":      "1784500000.123456",
		},
	}
	spec := buildAgentPodSpec(cfg, ev)
	if !podmanager.IsKubernetesTarget(spec.ExecutionTarget) {
		t.Fatalf("expected a kubernetes target, got %q", spec.ExecutionTarget)
	}
	if got, ok := spec.Env[EnvFreezeEligible]; ok {
		t.Errorf("%s set to %q, want UNSET (MicroVM suspend must not touch a pod agent)", EnvFreezeEligible, got)
	}
}

// The reconciler path (BuildSpecFromBeadInfo) must inject the same signal so a
// MicroVM thread agent reconciled out-of-band still suspends on gb stop.
func TestBuildSpecFromBeadInfo_MicrovmSuspend_ThreadAndEnabled(t *testing.T) {
	cfg := microvmSuspendCfg(true)
	spec := BuildSpecFromBeadInfo(cfg, "gardenio", "", "gardenio-microvms", "vm-thread", map[string]string{
		"slack_thread_channel": "C0ABCDEF",
		"slack_thread_ts":      "1784500000.123456",
	})
	if spec.ExecutionTarget != podmanager.ExecutionTargetLambdaMicroVM {
		t.Fatalf("expected lambda_microvm target, got %q", spec.ExecutionTarget)
	}
	if got := spec.Env[EnvFreezeEligible]; got != "true" {
		t.Errorf("%s = %q, want \"true\" (reconciler path)", EnvFreezeEligible, got)
	}
}

// isMicrovmSuspendEligible truth table, exercised directly.
func TestIsMicrovmSuspendEligible(t *testing.T) {
	threadEnv := map[string]string{"SLACK_THREAD_CHANNEL": "C0AB", "SLACK_THREAD_TS": "1.2"}
	cases := []struct {
		name    string
		enabled bool
		target  string
		env     map[string]string
		want    bool
	}{
		{"enabled+microvm+thread", true, podmanager.ExecutionTargetLambdaMicroVM, threadEnv, true},
		{"disabled+microvm+thread", false, podmanager.ExecutionTargetLambdaMicroVM, threadEnv, false},
		{"enabled+k8s+thread", true, podmanager.ExecutionTargetKubernetes, threadEnv, false},
		{"enabled+microvm+nothread", true, podmanager.ExecutionTargetLambdaMicroVM, map[string]string{}, false},
		{"enabled+microvm+channelonly", true, podmanager.ExecutionTargetLambdaMicroVM,
			map[string]string{"SLACK_THREAD_CHANNEL": "C0AB"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{MicrovmSuspendEnabled: tc.enabled}
			spec := &podmanager.AgentPodSpec{ExecutionTarget: tc.target, Env: tc.env}
			if got := isMicrovmSuspendEligible(cfg, spec); got != tc.want {
				t.Errorf("isMicrovmSuspendEligible = %v, want %v", got, tc.want)
			}
		})
	}
}

// fakeTeardowner is a microvmTeardowner that records Suspend/Terminate calls and
// can be toggled available/unavailable or made to fail either op, so the
// lambda_microvm teardown decision (microvmTeardown) can be exercised without a
// live AWS backend — the teardown-branch twin of fakeResumer.
type fakeTeardowner struct {
	available  bool
	suspErr    error
	termErr    error
	suspended  []string // bead IDs suspended, in order
	terminated []string // bead IDs terminated, in order
}

func (f *fakeTeardowner) Available() bool { return f.available }

func (f *fakeTeardowner) Suspend(_ context.Context, view microvm.AgentBeadView, _ string) error {
	if f.suspErr != nil {
		return f.suspErr
	}
	f.suspended = append(f.suspended, view.ID)
	return nil
}

func (f *fakeTeardowner) Terminate(_ context.Context, view microvm.AgentBeadView, _ string) error {
	if f.termErr != nil {
		return f.termErr
	}
	f.terminated = append(f.terminated, view.ID)
	return nil
}

// microvmStopEvent is a lambda_microvm AgentStop for a thread agent, carrying the
// same metadata the spawn path stamped (execution_target + slack thread binding +
// runtime_id) — the shape handleEvent hands microvmTeardown.
func microvmStopEvent(eventType subscriber.EventType) subscriber.Event {
	return subscriber.Event{
		Type:      eventType,
		Project:   "gardenio",
		Role:      "gardenio-microvms",
		AgentName: "vm-thread",
		BeadID:    "gd-mvm-agent",
		Metadata: map[string]string{
			"execution_target":     podmanager.ExecutionTargetLambdaMicroVM,
			"agent_state":          "stopping",
			"runtime_id":           "mvm-abc123",
			"runtime_state":        "RUNNING",
			"slack_thread_channel": "C0ABCDEF",
			"slack_thread_ts":      "1784500000.123456",
		},
	}
}

// TestMicrovmTeardown covers the full lambda_microvm teardown decision matrix —
// the handleEvent branch that was previously untestable (needed a live AWS
// backend), now reachable via the microvmTeardowner seam.
func TestMicrovmTeardown(t *testing.T) {
	// A freeze-eligible thread agent's graceful stop SUSPENDS (bead left open)
	// and records one microvm_suspends_total{outcome=suspended} sample (step 6).
	t.Run("eligible thread stop suspends", func(t *testing.T) {
		life := &fakeTeardowner{available: true}
		cfg := &config.Config{MicrovmSuspendEnabled: true}
		rm, reg := newTestResumeMetrics(t)
		out := microvmTeardown(context.Background(), discardLogger(), cfg,
			microvmStopEvent(subscriber.AgentStop), "gd-mvm-agent", life, rm)
		if out != microvmTeardownSuspended {
			t.Fatalf("outcome = %v, want suspended", out)
		}
		if len(life.suspended) != 1 || life.suspended[0] != "gd-mvm-agent" {
			t.Errorf("Suspend calls = %v, want [gd-mvm-agent]", life.suspended)
		}
		if len(life.terminated) != 0 {
			t.Errorf("a suspended agent must NOT be terminated, got %v", life.terminated)
		}
		if got := microvmSuspendCount(t, reg, "gardenio-microvms", gardeniometrics.MicrovmSuspendOutcomeSuspended); got != 1 {
			t.Errorf("microvm_suspends_total{outcome=suspended} = %d, want 1", got)
		}
	})

	// AgentKill (forced failure) always terminates — there is no wake to serve.
	t.Run("kill terminates", func(t *testing.T) {
		life := &fakeTeardowner{available: true}
		cfg := &config.Config{MicrovmSuspendEnabled: true}
		out := microvmTeardown(context.Background(), discardLogger(), cfg,
			microvmStopEvent(subscriber.AgentKill), "gd-mvm-agent", life, nil)
		if out != microvmTeardownTerminated {
			t.Fatalf("outcome = %v, want terminated", out)
		}
		if len(life.terminated) != 1 {
			t.Errorf("Terminate calls = %v, want 1", life.terminated)
		}
		if len(life.suspended) != 0 {
			t.Errorf("AgentKill must not suspend, got %v", life.suspended)
		}
	})

	// AgentDone (work complete) always terminates.
	t.Run("done terminates", func(t *testing.T) {
		life := &fakeTeardowner{available: true}
		cfg := &config.Config{MicrovmSuspendEnabled: true}
		out := microvmTeardown(context.Background(), discardLogger(), cfg,
			microvmStopEvent(subscriber.AgentDone), "gd-mvm-agent", life, nil)
		if out != microvmTeardownTerminated {
			t.Fatalf("outcome = %v, want terminated", out)
		}
		if len(life.terminated) != 1 || len(life.suspended) != 0 {
			t.Errorf("AgentDone must terminate only, susp=%v term=%v", life.suspended, life.terminated)
		}
	})

	// Suspend disabled (master switch off) → a stop terminates byte-for-byte as before.
	t.Run("stop with suspend disabled terminates", func(t *testing.T) {
		life := &fakeTeardowner{available: true}
		cfg := &config.Config{MicrovmSuspendEnabled: false}
		out := microvmTeardown(context.Background(), discardLogger(), cfg,
			microvmStopEvent(subscriber.AgentStop), "gd-mvm-agent", life, nil)
		if out != microvmTeardownTerminated {
			t.Fatalf("outcome = %v, want terminated (suspend disabled)", out)
		}
		if len(life.suspended) != 0 {
			t.Errorf("suspend-disabled stop must not suspend, got %v", life.suspended)
		}
	})

	// A non-thread MicroVM stop (no slack binding) terminates even with the switch on.
	t.Run("non-thread stop terminates", func(t *testing.T) {
		life := &fakeTeardowner{available: true}
		cfg := &config.Config{MicrovmSuspendEnabled: true}
		ev := microvmStopEvent(subscriber.AgentStop)
		delete(ev.Metadata, "slack_thread_channel")
		delete(ev.Metadata, "slack_thread_ts")
		out := microvmTeardown(context.Background(), discardLogger(), cfg, ev, "gd-mvm-agent", life, nil)
		if out != microvmTeardownTerminated {
			t.Fatalf("outcome = %v, want terminated (non-thread)", out)
		}
		if len(life.suspended) != 0 {
			t.Errorf("non-thread stop must not suspend, got %v", life.suspended)
		}
	})

	// A failed Suspend falls through to Terminate so a half-suspended runtime with
	// an open bead can't leak, and records one
	// microvm_suspends_total{outcome=fallback-terminated} sample (step 6).
	t.Run("suspend failure falls through to terminate", func(t *testing.T) {
		life := &fakeTeardowner{available: true, suspErr: errors.New("throttled")}
		cfg := &config.Config{MicrovmSuspendEnabled: true}
		rm, reg := newTestResumeMetrics(t)
		out := microvmTeardown(context.Background(), discardLogger(), cfg,
			microvmStopEvent(subscriber.AgentStop), "gd-mvm-agent", life, rm)
		if out != microvmTeardownTerminated {
			t.Fatalf("outcome = %v, want terminated (suspend failed -> fallback)", out)
		}
		if len(life.terminated) != 1 {
			t.Errorf("a failed suspend must fall through to Terminate, term=%v", life.terminated)
		}
		if got := microvmSuspendCount(t, reg, "gardenio-microvms", gardeniometrics.MicrovmSuspendOutcomeFallbackTerminated); got != 1 {
			t.Errorf("microvm_suspends_total{outcome=fallback-terminated} = %d, want 1", got)
		}
		if got := microvmSuspendCount(t, reg, "gardenio-microvms", gardeniometrics.MicrovmSuspendOutcomeSuspended); got != 0 {
			t.Errorf("a fallback-terminated suspend must NOT count as suspended, got %d", got)
		}
	})

	// A Terminate that itself errors is logged but still reports terminated — the
	// leak GC backstops, and the caller must still emit metrics + return.
	t.Run("terminate error still reports terminated", func(t *testing.T) {
		life := &fakeTeardowner{available: true, termErr: errors.New("boom")}
		cfg := &config.Config{MicrovmSuspendEnabled: true}
		out := microvmTeardown(context.Background(), discardLogger(), cfg,
			microvmStopEvent(subscriber.AgentDone), "gd-mvm-agent", life, nil)
		if out != microvmTeardownTerminated {
			t.Fatalf("outcome = %v, want terminated even on terminate error", out)
		}
	})

	// Backend unavailable (default-off) → not applicable; caller falls through to
	// the K8s pod-teardown path.
	t.Run("backend off is not applicable", func(t *testing.T) {
		life := &fakeTeardowner{available: false}
		cfg := &config.Config{MicrovmSuspendEnabled: true}
		out := microvmTeardown(context.Background(), discardLogger(), cfg,
			microvmStopEvent(subscriber.AgentStop), "gd-mvm-agent", life, nil)
		if out != microvmTeardownNotApplicable {
			t.Fatalf("outcome = %v, want not-applicable (backend off)", out)
		}
		if len(life.suspended)+len(life.terminated) != 0 {
			t.Error("an unavailable backend must neither suspend nor terminate")
		}
	})

	// A non-MicroVM (kubernetes) agent → not applicable, so the pod path handles it.
	t.Run("non-microvm agent is not applicable", func(t *testing.T) {
		life := &fakeTeardowner{available: true}
		cfg := &config.Config{MicrovmSuspendEnabled: true}
		ev := microvmStopEvent(subscriber.AgentStop)
		ev.Metadata["execution_target"] = "" // kubernetes
		out := microvmTeardown(context.Background(), discardLogger(), cfg, ev, "gd-mvm-agent", life, nil)
		if out != microvmTeardownNotApplicable {
			t.Fatalf("outcome = %v, want not-applicable (kubernetes agent)", out)
		}
		if len(life.suspended)+len(life.terminated) != 0 {
			t.Error("a kubernetes agent must not hit the MicroVM teardown")
		}
	})
}

// microvmSuspendOnStop (teardown-side twin) truth table — the gate the
// handleEvent AgentStop branch consults, keyed off event metadata.
func TestMicrovmSuspendOnStop(t *testing.T) {
	threadMeta := map[string]string{"slack_thread_channel": "C0AB", "slack_thread_ts": "1.2"}
	cases := []struct {
		name    string
		enabled bool
		meta    map[string]string
		want    bool
	}{
		{"enabled+thread", true, threadMeta, true},
		{"disabled+thread", false, threadMeta, false},
		{"enabled+nothread", true, map[string]string{}, false},
		{"enabled+channelonly", true, map[string]string{"slack_thread_channel": "C0AB"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{MicrovmSuspendEnabled: tc.enabled}
			ev := subscriber.Event{Metadata: tc.meta}
			if got := microvmSuspendOnStop(cfg, ev); got != tc.want {
				t.Errorf("microvmSuspendOnStop = %v, want %v", got, tc.want)
			}
		})
	}
}

// newTestResumeMetrics builds a registry-backed ResumeMetrics for asserting the
// step-6 microvm_suspends_total counter (ka-0000002861).
func newTestResumeMetrics(t *testing.T) (*gardeniometrics.ResumeMetrics, *prometheus.Registry) {
	t.Helper()
	rm := gardeniometrics.NewResumeMetrics("e2e")
	reg := prometheus.NewRegistry()
	if err := rm.Register(reg); err != nil {
		t.Fatalf("register resume metrics: %v", err)
	}
	return rm, reg
}

// microvmSuspendCount returns the current value of the
// gardenio_microvm_suspends_total counter for the given role + outcome (0 if the
// series does not exist yet).
func microvmSuspendCount(t *testing.T, reg *prometheus.Registry, role, outcome string) int {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "gardenio_microvm_suspends_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			var gotRole, gotOutcome string
			for _, l := range m.GetLabel() {
				switch l.GetName() {
				case "role":
					gotRole = l.GetValue()
				case "outcome":
					gotOutcome = l.GetValue()
				}
			}
			if gotRole == role && gotOutcome == outcome {
				return int(m.GetCounter().GetValue())
			}
		}
	}
	return 0
}
