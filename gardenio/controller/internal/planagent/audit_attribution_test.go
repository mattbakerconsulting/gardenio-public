package planagent

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// audit_attribution_test.go pins the third invariant the P3 relocation had to
// PRESERVE when the spawn/wake helper moved out of cmd/beads-viewer into this
// shared engine (task ka-0000001801): operator/system audit stamping. The engine
// attributes every Spawn/Wake write ENTIRELY from the caller's ctx (it never
// auto-stamps — see the package doc's AUDIT-ATTRIBUTION CALLER CONTRACT), so
// these tests lock two things:
//
//  1. Threading: an operator stamped on ctx via beadsapi.WithOperator /
//     WithSystemOperator reaches EVERY daemon write the engine makes (create +
//     assigned edge on a fresh spawn; the field refresh + status flip on a wake).
//     A future refactor that dropped the ctx (e.g. context.Background()) — which
//     would silently strip attribution and fail-closed the daemon with an R2 400
//     — breaks these.
//  2. Diagnosis: entering a write path with NO operator on ctx emits the
//     plan_agent_write_unattributed warn, so the fail-closed 400 the caller is
//     about to hit is traceable to "the caller forgot to stamp" rather than a
//     mystery upstream error. The happy path stays silent.

// engineWithLogBuf builds an engine whose logger writes to buf, so a test can
// assert the presence/absence of the unattributed-write diagnostic. Same fixed
// clock as testEngine so liveness is deterministic.
func engineWithLogBuf(f *fakeDaemon, buf *bytes.Buffer) *Engine {
	return NewEngine(Config{
		Daemon:  f,
		NameGen: testNameGen,
		Logger:  slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Now:     func() time.Time { return testNow },
	})
}

// operatorFromWriteCtxs collects the X-Operated-By operator the beadsapi client
// WOULD send for each captured write ctx (nil ctxs skipped). Mirrors sendOnce's
// read of OperatorFromContext so the test asserts the exact wire attribution.
func operatorFromWriteCtxs(ctxs ...context.Context) []string {
	var out []string
	for _, c := range ctxs {
		if c == nil {
			continue
		}
		out = append(out, beadsapi.OperatorFromContext(c))
	}
	return out
}

func TestSpawn_FreshThreadsOperatorToEveryWrite(t *testing.T) {
	f := newFakeDaemon()
	epic := seedPlanEpic(f, "gd-plan1")

	const op = "oidc:matthew.baker@acme.example"
	ctx := beadsapi.WithOperator(context.Background(), op)
	if _, _, err := testEngine(f, nil).Spawn(ctx, "matthew.baker@acme.example", epic, "help me plan"); err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	// Both attributed writes on the fresh-spawn path — CreateBead and the
	// load-bearing assigned-edge AddDependency — must carry the operator.
	for _, got := range operatorFromWriteCtxs(f.createLastCtx, f.addDepLastCtx) {
		if got != op {
			t.Errorf("fresh-spawn write operator = %q, want %q (ctx must thread to every write)", got, op)
		}
	}
}

func TestWake_ThreadsOperatorToEveryWrite(t *testing.T) {
	f := newFakeDaemon()
	epic := seedPlanEpic(f, "gd-plan1")
	seedAgent(f, "gd-prior", "gardenio-plan-agent-prior", "gd-plan1", "rate_limited", testNow.Add(-30*time.Minute))

	const op = "slack:U0OPERATOR1"
	ctx := beadsapi.WithOperator(context.Background(), op)
	if _, _, err := testEngine(f, nil).Spawn(ctx, "reactor", epic, "wake and continue"); err != nil {
		t.Fatalf("Spawn(wake): %v", err)
	}

	// The single atomic wake write (status flip + field refresh carrying
	// session_resume / cleared agent_state; ka-0000001583) must carry the operator,
	// or the daemon fail-closes the revive with an R2 400 and the wake is swallowed.
	// updateFieldsCtx is nil now — the wake no longer makes a separate fields-only
	// write; operatorFromWriteCtxs skips nil ctxs.
	if f.updateFieldsCtx != nil {
		t.Errorf("wake must not make a separate fields-only write (ka-0000001583)")
	}
	for _, got := range operatorFromWriteCtxs(f.updateLastCtx) {
		if got != op {
			t.Errorf("wake write operator = %q, want %q (ctx must thread to the write)", got, op)
		}
	}
}

func TestSpawn_ThreadsSystemOperatorToEveryWrite(t *testing.T) {
	f := newFakeDaemon()
	epic := seedPlanEpic(f, "gd-plan1")

	// A true machine-origin caller (no resolvable human) stamps WithSystemOperator
	// instead — the daemon binds the narrow machine sink rather than fail-closing.
	const sysOp = "gardenio-plan-reactor"
	ctx := beadsapi.WithSystemOperator(context.Background(), sysOp)
	if _, _, err := testEngine(f, nil).Spawn(ctx, "reactor", epic, "first message"); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if got := beadsapi.SystemOperatorFromContext(f.createLastCtx); got != sysOp {
		t.Errorf("create system-operator = %q, want %q", got, sysOp)
	}
	if got := beadsapi.SystemOperatorFromContext(f.addDepLastCtx); got != sysOp {
		t.Errorf("assigned-edge system-operator = %q, want %q", got, sysOp)
	}
}

func TestSpawn_UnattributedCtxWarns(t *testing.T) {
	f := newFakeDaemon()
	epic := seedPlanEpic(f, "gd-plan1")
	var buf bytes.Buffer

	// context.Background() carries NO operator on any channel — the caller forgot
	// to stamp. The engine still performs the write (the daemon is the authority
	// on server-side fallback) but must emit the diagnostic so the impending R2
	// 400 is traceable.
	if _, _, err := engineWithLogBuf(f, &buf).Spawn(context.Background(), "someone", epic, "hi"); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if !strings.Contains(buf.String(), "plan_agent_write_unattributed") {
		t.Errorf("unattributed fresh spawn did not warn; log = %q", buf.String())
	}
	if !strings.Contains(buf.String(), "where=spawn") {
		t.Errorf("diagnostic should name where=spawn; log = %q", buf.String())
	}
}

func TestWake_UnattributedCtxWarns(t *testing.T) {
	f := newFakeDaemon()
	epic := seedPlanEpic(f, "gd-plan1")
	seedAgent(f, "gd-prior", "prior", "gd-plan1", "rate_limited", testNow.Add(-30*time.Minute))
	var buf bytes.Buffer

	if _, _, err := engineWithLogBuf(f, &buf).Spawn(context.Background(), "someone", epic, "wake up"); err != nil {
		t.Fatalf("Spawn(wake): %v", err)
	}
	if !strings.Contains(buf.String(), "plan_agent_write_unattributed") {
		t.Errorf("unattributed wake did not warn; log = %q", buf.String())
	}
	if !strings.Contains(buf.String(), "where=wake") {
		t.Errorf("diagnostic should name where=wake; log = %q", buf.String())
	}
}

func TestSpawn_AttributedCtxDoesNotWarn(t *testing.T) {
	f := newFakeDaemon()
	epic := seedPlanEpic(f, "gd-plan1")
	var buf bytes.Buffer

	ctx := beadsapi.WithOperator(context.Background(), "oidc:matthew.baker@acme.example")
	if _, _, err := engineWithLogBuf(f, &buf).Spawn(ctx, "matthew.baker@acme.example", epic, "hi"); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if strings.Contains(buf.String(), "plan_agent_write_unattributed") {
		t.Errorf("a properly-attributed spawn must NOT warn; log = %q", buf.String())
	}
}

// TestRequireAuditAttribution_RecognizesEdgeAuthChannel pins that the diagnostic
// treats the edge-auth envelope (WithEdgeAuthOperator) as valid attribution too —
// it is the third channel sendOnce reads, so a caller asserting the operator via
// the HMAC envelope must not trip a false "unattributed" warn.
func TestRequireAuditAttribution_RecognizesEdgeAuthChannel(t *testing.T) {
	f := newFakeDaemon()
	var buf bytes.Buffer
	e := engineWithLogBuf(f, &buf)

	ctx := beadsapi.WithEdgeAuthOperator(context.Background(), "slack:U0OPERATOR1")
	e.requireAuditAttribution(ctx, "spawn", "gd-plan1", "actor")
	if strings.Contains(buf.String(), "plan_agent_write_unattributed") {
		t.Errorf("edge-auth-stamped ctx must count as attributed; log = %q", buf.String())
	}
}
