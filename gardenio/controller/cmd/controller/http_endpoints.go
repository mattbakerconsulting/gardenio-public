package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	gardeniometrics "gardenio/controller/internal/metrics"
	"gardenio/controller/internal/microvm"
	"gardenio/controller/internal/podmanager"
	"gardenio/controller/internal/poolmanager"
	"gardenio/controller/internal/reconciler"
)

// registerAutodestructEndpoints adds the autodestruct HTTP endpoints to the mux.
func registerAutodestructEndpoints(mux *http.ServeMux, rec *reconciler.Reconciler, logger *slog.Logger) {
	token := os.Getenv("AUTODESTRUCT_TOKEN")

	requireAuth := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if token == "" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_ = json.NewEncoder(w).Encode(map[string]string{
					"error": "autodestruct not configured",
				})
				return
			}
			auth := r.Header.Get("Authorization")
			provided := strings.TrimPrefix(auth, "Bearer ")
			if provided == auth || subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
				return
			}
			next(w, r)
		}
	}

	mux.HandleFunc("POST /autodestruct", requireAuth(func(w http.ResponseWriter, r *http.Request) {
		actor := r.Header.Get("X-Actor")
		if actor == "" {
			actor = "unknown"
		}
		killed, err := rec.Autodestruct(r.Context(), actor)
		if err != nil {
			logger.Error("autodestruct failed", "error", err, "actor", actor)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		logger.Warn("autodestruct triggered via HTTP", "actor", actor, "killed", killed)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":    "destructed",
			"killed":    killed,
			"actor":     actor,
			"timestamp": time.Now().UTC().Format(time.RFC3339),
		})
	}))

	mux.HandleFunc("POST /autodestruct/clear", requireAuth(func(w http.ResponseWriter, r *http.Request) {
		actor := r.Header.Get("X-Actor")
		if actor == "" {
			actor = "unknown"
		}
		rec.ClearAutodestruct(actor)
		logger.Warn("autodestruct cleared via HTTP", "actor", actor)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":    "cleared",
			"actor":     actor,
			"timestamp": time.Now().UTC().Format(time.RFC3339),
		})
	}))

	mux.HandleFunc("GET /autodestruct/status", requireAuth(func(w http.ResponseWriter, _ *http.Request) {
		status := "active"
		if !rec.IsDestructed() {
			status = "inactive"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status": status,
		})
	}))
}

// handleFreezeInterrupt returns the HTTP handler for POST
// /api/v1/freeze/interrupt (C2 interruptible freeze, task ka-0000005834). It
// delegates to the shared podmanager.FreezeInterruptHandler so the controller
// and the dedicated e2e probe (cmd/freeze-interrupt-probe, ka-0000004367) serve
// the IDENTICAL genuine endpoint — the abort callback is the lifecycle's
// abortFreeze (nil-safe / no-op when freeze-thaw or FreezeInterruptEnabled is
// off, yielding 404 exactly as before).
func handleFreezeInterrupt(ft *freezeThawLifecycle, logger *slog.Logger) http.HandlerFunc {
	return podmanager.FreezeInterruptHandler(ft.abortFreeze, logger)
}

// handleThawStatus returns an HTTP handler for GET /api/v1/thaw/status?agent=<name>
// (re-nudge mid-THAW coordination, task ka-0000005830, design ka-0000005125). The
// message-ingress seam GETs it to decide buffer-vs-route before delivering a
// nudge: a thaw has no live fallback Pod A (the epic's freeze/thaw asymmetry), so
// a message arriving mid-thaw must WAIT for the rehydrated Pod B rather than race
// a half-restored agent or inject a duplicate respawn. Always returns 200 with
// {"agent":..., "thawing":true|false}; thawing is false (the routing-unchanged
// no-op signal) when the lifecycle is nil (freeze/thaw off) or no thaw is in
// flight for the agent. A missing/empty `agent` query param is a 400 (the caller
// must name an agent), mirroring handleFreezeInterrupt's bad-body 400.
func handleThawStatus(ft *freezeThawLifecycle, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		agent := r.URL.Query().Get("agent")
		if agent == "" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "query param ?agent=<name> is required",
			})
			return
		}
		thawing := ft.isThawing(agent)
		if thawing {
			logger.Debug("thaw status: agent is mid-thaw; ingress seam will buffer the nudge",
				"agent", agent, "actor", r.Header.Get("X-Actor"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"agent": agent, "thawing": thawing})
	}
}

// microvmResumeRequest is the body of POST /api/v1/microvm/resume: the name of
// the thread agent whose suspended MicroVM should be resumed in place.
type microvmResumeRequest struct {
	Agent string `json:"agent"`
}

// agentRosterReader is the narrow presence-roster read surface the resume
// rehydration prober needs. *beadsapi.Client satisfies it; a fake supplies it
// in tests.
type agentRosterReader interface {
	GetAgentRoster(ctx context.Context, staleThresholdSecs int) (*beadsapi.AgentRosterResponse, error)
}

// microvmRosterProber implements microvm.LivenessProber over the beads daemon's
// presence roster (step 5, plan ka-0000004518). It is the MicroVM proof-of-life
// ack: a resumed VM's own `gb agent start` emits SessionStart + heartbeats that
// EGRESS over the freshly-enrolled OpenZiti overlay (using the freshly-read NATS
// token) to reach the beads daemon and advance the agent's presence entry. The
// heartbeat's ARRIVAL is proof that identity + overlay + reachability are ALL
// live — a fresh MicroVM boot cannot partially satisfy them (docs/microvm-resume-rehydration.md M1).
//
// The roster reports idle_secs (a DURATION since the actor's last hook event),
// which is clock-skew-free between controller and daemon: the agent is proven
// live since `since` iff idle_secs <= now - since, i.e. its most recent event
// post-dates the resume instant. A reaped actor, or one absent from the roster,
// is not (yet) proven live.
type microvmRosterProber struct {
	roster agentRosterReader
	nowFn  func() time.Time
}

func (p microvmRosterProber) now() time.Time {
	if p.nowFn != nil {
		return p.nowFn()
	}
	return time.Now()
}

// ProvenLiveSince implements microvm.LivenessProber.
func (p microvmRosterProber) ProvenLiveSince(ctx context.Context, agentName string, since time.Time) (bool, error) {
	if p.roster == nil {
		return false, nil
	}
	// staleThresholdSecs=0 lets the daemon use its generous default (30m) — we do
	// not want the roster to EXCLUDE a freshly-resumed agent on staleness; the
	// idle_secs comparison below is what proves recency.
	resp, err := p.roster.GetAgentRoster(ctx, 0)
	if err != nil {
		return false, err
	}
	elapsedSinceResume := p.now().Sub(since).Seconds()
	for _, a := range resp.Actors {
		if a.Actor != agentName {
			continue
		}
		if a.Reaped {
			// Present but reaped (presence reaper marked it dead) — not live.
			return false, nil
		}
		// Proven live iff the actor's most recent hook event post-dates the resume
		// instant: idle_secs (time since last event) <= time elapsed since resume.
		return a.IdleSecs <= elapsedSinceResume, nil
	}
	// Not in the roster yet — the resumed VM has not egressed a hook event since
	// resume (or ever). Not yet proven; the caller retries under the deadline.
	return false, nil
}

// microvmResumeResponse is the reply the bridge reads to decide whether it
// resumed a suspended VM (and can skip its close+remint+pool path) or must fall
// through to its normal respawn. Resumed is true only when a suspended MicroVM
// agent was found and backend.Resume succeeded. Rehydrating is true when the
// step-5 fail-closed proof-of-life rendezvous was launched (the agent landed at
// agent_state=resuming and will only reach working once its heartbeat re-crosses
// the overlay; else it fails closed to resume_failed) — informational for the
// bridge/operator, the bridge routes on Resumed alone.
type microvmResumeResponse struct {
	Resumed     bool   `json:"resumed"`
	Rehydrating bool   `json:"rehydrating,omitempty"`
	MicrovmID   string `json:"microvm_id,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

// microvmResumer is the narrow slice of *microvm.Lifecycle the resume handler
// drives — the seam that lets the handler be unit-tested with a fake, without an
// AWS backend. *microvm.Lifecycle satisfies it (all methods are nil-safe).
type microvmResumer interface {
	Available() bool
	Resume(ctx context.Context, view microvm.AgentBeadView) error
	// ResumeRehydrateEnabled reports whether the step-5 fail-closed proof-of-life
	// gate is active (false = step-3 stamp-working-immediately rollback).
	ResumeRehydrateEnabled() bool
	// AwaitRehydration is the fail-closed proof-of-life gate run AFTER Resume when
	// the gate is on. It blocks until the resumed VM proves life (→ working) or
	// the deadline expires (→ terminate + resume_failed). The handler runs it on a
	// DETACHED context (it can outlast the bridge's HTTP client), so it takes the
	// agent name + resume instant it needs rather than reading them off r.
	AwaitRehydration(ctx context.Context, view microvm.AgentBeadView, agentName string, resumeAt time.Time) microvm.RehydrationOutcome
	// IsRehydrating reports whether a resume rehydration for the agent is in
	// flight (backs the resume-status endpoint).
	IsRehydrating(agentName string) bool
}

// agentBeadFinder is the narrow bead-read surface the resume handler needs.
// *beadsapi.Client satisfies it; a fake supplies it in tests.
type agentBeadFinder interface {
	FindAgentBead(ctx context.Context, agentName string) (*beadsapi.BeadDetail, error)
}

// handleMicrovmResume returns an HTTP handler for POST /api/v1/microvm/resume
// (step 3, plan ka-0000004518 / epic ka-0000004689). It is the native analog of
// the gVisor thaw wake path: a re-mention of a SUSPENDED MicroVM thread agent
// resumes the EXISTING VM (ResumeMicrovm on the bead's runtime_id) instead of
// launching a fresh one.
//
// The bridge cannot take the gVisor close+remint route for a MicroVM: the VM's
// identity lives in runtime_id ON THE BEAD, so closing the bead would fire
// AgentDone -> Lifecycle.Terminate and tear the suspended VM down, and a fresh
// same-name mint has no runtime_id so it would Launch a brand-new RunMicrovm —
// either way the suspended VM (and its 12h session creds / Ziti identity) is
// abandoned and continuity is lost. So this endpoint resumes the SAME open bead
// in place and leaves it open.
//
// It reports resumed:false (200) — the routing-unchanged signal the bridge
// treats exactly like a gVisor miss, falling through to its normal respawn — in
// every not-applicable case: the MicroVM backend is off, the agent bead is
// missing, it is not a MicroVM agent, it is not suspended (agent_state !=
// stopping), or it has no persisted runtime_id. Only a genuine resume of a
// suspended VM returns resumed:true. A backend.Resume error is a 500 so the
// bridge can decide to retry or fall back; the resume is idempotent so a retry
// is safe. A missing/empty agent name is a 400 (mirrors handleThawStatus).
func handleMicrovmResume(microvmLifecycle microvmResumer, daemon agentBeadFinder, resumeMetrics *gardeniometrics.ResumeMetrics, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		// Resume-latency stopwatch (step 6, task ka-0000002861). t0 is the moment
		// the controller ACCEPTS this resume request; the sample closes when the
		// resumed VM proves life (AwaitRehydration Released=true) — the MicroVM
		// analog of the gVisor thaw's rehydration-acked endpoint. Captured at
		// handler entry so the metric spans the whole controller-observable resume
		// (bead resolve + ResumeMicrovm + whole-userspace warm-boot + Ziti
		// re-enroll + first egress heartbeat), matching resume latency =
		// t(agent responsive) − t(resume request accepted).
		acceptedAt := time.Now()

		var req microvmResumeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "invalid request body: " + err.Error(),
			})
			return
		}
		agent := strings.TrimSpace(req.Agent)
		if agent == "" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "field agent is required",
			})
			return
		}

		// Backend off → routing-unchanged no-op, exactly like a gVisor thaw miss.
		if microvmLifecycle == nil || !microvmLifecycle.Available() {
			_ = json.NewEncoder(w).Encode(microvmResumeResponse{Resumed: false, Reason: "microvm backend unavailable"})
			return
		}
		if daemon == nil {
			_ = json.NewEncoder(w).Encode(microvmResumeResponse{Resumed: false, Reason: "no daemon client"})
			return
		}

		// Resolve the agent bead. FindAgentBead returns the OPEN bead — which is
		// exactly the suspended-awaiting-resume bead (step 2 leaves it open in
		// agent_state=stopping), never a closed/terminated one.
		bead, err := daemon.FindAgentBead(r.Context(), agent)
		if err != nil || bead == nil {
			logger.Debug("microvm resume: agent bead not found; falling through to bridge respawn",
				"agent", agent, "error", err)
			_ = json.NewEncoder(w).Encode(microvmResumeResponse{Resumed: false, Reason: "agent bead not found"})
			return
		}

		// Guard the three conditions that make this a genuine suspended-MicroVM
		// resume; any miss is the routing-unchanged signal.
		//   1. it is a MicroVM agent (execution_target=lambda_microvm),
		//   2. it is suspended-awaiting-resume (agent_state=stopping — the state
		//      step 2's suspend path writes and leaves the bead open in), and
		//   3. it has a persisted runtime_id (the VM to resume).
		if bead.Fields["execution_target"] != podmanager.ExecutionTargetLambdaMicroVM {
			_ = json.NewEncoder(w).Encode(microvmResumeResponse{Resumed: false, Reason: "not a microvm agent"})
			return
		}
		if bead.Fields["agent_state"] != "stopping" {
			_ = json.NewEncoder(w).Encode(microvmResumeResponse{Resumed: false, Reason: "agent not suspended"})
			return
		}
		runtimeID := strings.TrimSpace(bead.Fields["runtime_id"])
		if runtimeID == "" {
			_ = json.NewEncoder(w).Encode(microvmResumeResponse{Resumed: false, Reason: "no runtime_id to resume"})
			return
		}

		// AgentSecretARN is intentionally unset: it is not persisted (matching the
		// reconciler + teardown views), and Resume does not touch per-run secrets —
		// they must survive standby (step 2 leaves them intact on suspend; step 5
		// owns re-credentialing the resumed in-VM process).
		view := microvm.AgentBeadView{
			ID:              bead.ID,
			AgentState:      bead.Fields["agent_state"],
			ExecutionTarget: bead.Fields["execution_target"],
			RuntimeID:       runtimeID,
			RuntimeState:    bead.Fields["runtime_state"],
		}
		// Capture the resume instant BEFORE the AWS call so the proof-of-life gate
		// only accepts a heartbeat that post-dates it — a pre-suspend heartbeat can
		// never be mistaken for proof of the fresh boot.
		resumeAt := time.Now()
		if err := microvmLifecycle.Resume(r.Context(), view); err != nil {
			logger.Error("microvm resume: backend resume failed",
				"agent", agent, "bead", bead.ID, "runtime_id", runtimeID, "error", err)
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(microvmResumeResponse{Resumed: false, Reason: err.Error()})
			return
		}

		// Step 5 correctness boundary: with the rehydration gate on, Resume stamped
		// agent_state=resuming (NOT working). Launch the fail-closed proof-of-life
		// rendezvous on a DETACHED context — it polls until the resumed VM's egress
		// heartbeat re-crosses the overlay to beads (→ working) or the deadline
		// expires (→ terminate + resume_failed), which can far outlast the bridge's
		// 10s HTTP client, so it must not be tied to r.Context(). The bridge routes
		// on Resumed=true regardless; GET .../resume/status reports the gate outcome.
		rehydrating := microvmLifecycle.ResumeRehydrateEnabled()
		role := bead.Fields["role"]
		if rehydrating {
			detached := context.WithoutCancel(r.Context())
			go func() {
				out := microvmLifecycle.AwaitRehydration(detached, view, agent, resumeAt)
				logger.Info("microvm resume: rehydration rendezvous settled",
					"agent", agent, "bead", bead.ID, "runtime_id", runtimeID,
					"released", out.Released, "elapsed", out.Elapsed, "reason", out.Reason)
				// Resume-latency observation (step 6, ka-0000002861): only a
				// PROOF-CONFIRMED resume (Released=true) is a genuine successful
				// resume — a fail-closed rendezvous terminated the VM (resume_failed)
				// and is not a resume that a user's message can flow to, so it is not
				// bucketed (matching the thaw path, which observes only a successful
				// thaw). We record from acceptedAt (request accept) to now (proof of
				// life) so the sample includes the whole controller-observable resume.
				if out.Released {
					resumeMetrics.RecordResumeLatency(time.Since(acceptedAt).Seconds(),
						gardeniometrics.ResumeRegimeMicrovm, role,
						gardeniometrics.ResumeRuntimeMicrovm, gardeniometrics.ResumePathMicrovmResume)
				}
			}()
		}
		logger.Info("microvm resume: resumed suspended agent VM in place",
			"agent", agent, "bead", bead.ID, "runtime_id", runtimeID, "rehydrating", rehydrating)
		_ = json.NewEncoder(w).Encode(microvmResumeResponse{Resumed: true, Rehydrating: rehydrating, MicrovmID: runtimeID})
	}
}

// microvmResumeStatusResponse is the reply of GET /api/v1/microvm/resume/status.
// Mirrors handleThawStatus: rehydrating is true while the step-5 proof-of-life
// rendezvous is in flight for the agent, so an operator/observer can distinguish
// "coming back" from "settled". Best-effort and zero-blast-radius: an unknown
// agent or a settled/never-started rendezvous reports rehydrating:false.
type microvmResumeStatusResponse struct {
	Agent       string `json:"agent"`
	Rehydrating bool   `json:"rehydrating"`
}

// handleMicrovmResumeStatus returns an HTTP handler for GET
// /api/v1/microvm/resume/status?agent=<name> (step 5, plan ka-0000004518). It
// reports whether a resume rehydration proof-of-life rendezvous is currently in
// flight for the agent — the MicroVM analog of GET /api/v1/thaw/status. Reports
// rehydrating:false (the settled/never-started signal) whenever the backend is
// off or no rendezvous is in flight, so callers treat it exactly like a healthy
// settled resume. A missing/empty agent name is a 400 (mirrors handleThawStatus).
func handleMicrovmResumeStatus(microvmLifecycle microvmResumer, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		agent := strings.TrimSpace(r.URL.Query().Get("agent"))
		if agent == "" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "query param agent is required"})
			return
		}
		// The registry is keyed by the exact agent name the resume endpoint passed
		// to AwaitRehydration (the bridge sends the already-extracted name), so
		// consult it verbatim — no re-extraction.
		rehydrating := microvmLifecycle != nil && microvmLifecycle.Available() &&
			microvmLifecycle.IsRehydrating(agent)
		if rehydrating {
			logger.Debug("microvm resume status: agent mid-rehydration", "agent", agent)
		}
		_ = json.NewEncoder(w).Encode(microvmResumeStatusResponse{Agent: agent, Rehydrating: rehydrating})
	}
}

// handlePoolAssign returns an HTTP handler for POST /api/v1/pool/assign.
// It atomically assigns a prewarmed agent from the pool to a thread.
// Returns 200 with the assigned agent info, or 404 if the pool is empty.
func handlePoolAssign(pool *poolmanager.Manager, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if pool == nil {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "prewarmed pool is not enabled",
			})
			return
		}

		var req poolmanager.AssignRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "invalid request body: " + err.Error(),
			})
			return
		}

		result, err := pool.AssignPrewarmed(r.Context(), req)
		if err != nil {
			if errors.Is(err, poolmanager.ErrPoolEmpty) {
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]string{
					"error": "no prewarmed agents available",
				})
				return
			}
			logger.Error("pool assign failed", "error", err)
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": err.Error(),
			})
			return
		}

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(result)
	}
}

// handleTunables exposes the ConfigWatcher cache so operators and E2E tests
// can observe the live effective value for every tunable key. The `resolved`
// section reports the bead > env > compiled-default decision for keys the
// controller actually consumes.
//
// This endpoint was added for the env→config-bead pilot (ka-0000003317 /
// epic ka-0000006607) to make the hot-reload contract testable — writing a
// new value to the config bead should move the resolved value without a
// pod restart.
func handleTunables(w *beadsapi.ConfigWatcher) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		type resolvedEntry struct {
			Value  string `json:"value"`
			Source string `json:"source"`
		}
		resp := map[string]any{}
		if w == nil {
			resp["cache"] = map[string]string{}
			resp["resolved"] = map[string]resolvedEntry{}
			_ = json.NewEncoder(rw).Encode(resp)
			return
		}
		resp["cache"] = w.Snapshot()

		// Record each tunable the controller consumes. When new keys move to
		// ConfigWatcher, extend this list so the admin endpoint keeps its
		// value as a precedence-truth source.
		resolved := map[string]resolvedEntry{}
		d := w.GetDuration("COOP_QUEUED_WARN_AGE", "COOP_QUEUED_WARN_AGE", time.Hour)
		src := "compiled"
		if _, ok := w.Snapshot()["COOP_QUEUED_WARN_AGE"]; ok {
			src = "bead"
		} else if _, ok := os.LookupEnv("COOP_QUEUED_WARN_AGE"); ok {
			src = "env"
		}
		resolved["COOP_QUEUED_WARN_AGE"] = resolvedEntry{Value: d.String(), Source: src}
		resp["resolved"] = resolved

		_ = json.NewEncoder(rw).Encode(resp)
	}
}
