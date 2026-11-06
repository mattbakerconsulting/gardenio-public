package microvm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	mvtypes "github.com/aws/aws-sdk-go-v2/service/lambdamicrovms/types"
	smtypes "github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
)

// --- fake issuer -------------------------------------------------------------

// fakeIssuer records IssueLease/RevokeLeases calls and lets a test inject
// errors and lease shapes — the per-run identity counterpart of fakeSecrets.
type fakeIssuer struct {
	lease    *Lease
	issueErr error
	issued   []string // task ids leases were minted for
	issueTTL time.Duration

	revokeErr error
	revoked   []string // task ids revoked
}

func (f *fakeIssuer) IssueLease(_ context.Context, taskID string, ttl time.Duration) (*Lease, error) {
	if f.issueErr != nil {
		return nil, f.issueErr
	}
	f.issued = append(f.issued, taskID)
	f.issueTTL = ttl
	if f.lease != nil {
		return f.lease, nil
	}
	return &Lease{
		LeaseID:       "lease-" + taskID,
		IdentityID:    "identity-" + taskID,
		EnrollmentJWT: "jwt-" + taskID,
		ExpiresAt:     "2026-07-18T12:00:00Z",
		GrantedTTL:    ttl.String(),
	}, nil
}

func (f *fakeIssuer) RevokeLeases(_ context.Context, taskID string) error {
	if f.revokeErr != nil {
		return f.revokeErr
	}
	f.revoked = append(f.revoked, taskID)
	return nil
}

// perRunBackend builds a Backend with the fake issuer wired (per-run identity
// regime on).
func perRunBackend(t *testing.T, mv microvmAPI, sm secretsAPI, iss identityIssuer) *Backend {
	t.Helper()
	b := mustBackend(t, mv, sm, testBackendConfig())
	b.SetIssuer(iss)
	return b
}

// --- IdentitySecretValue -----------------------------------------------------

func TestIdentitySecretValueShapes(t *testing.T) {
	// Option A: enrollment JWT → typed document the image enrolls at boot.
	got, err := IdentitySecretValue(&Lease{EnrollmentJWT: "ey.abc"})
	if err != nil {
		t.Fatalf("jwt lease: %v", err)
	}
	var doc struct {
		Type string `json:"type"`
		JWT  string `json:"jwt"`
	}
	if err := json.Unmarshal([]byte(got), &doc); err != nil {
		t.Fatalf("unmarshal jwt secret: %v", err)
	}
	if doc.Type != "ziti_enrollment_jwt" || doc.JWT != "ey.abc" {
		t.Errorf("jwt secret = %+v, want type=ziti_enrollment_jwt jwt=ey.abc", doc)
	}

	// Option B: broker-enrolled identity JSON → written verbatim.
	idJSON := `{"ztAPI":"https://ziti:1280","id":{"cert":"pem:..."}}`
	got, err = IdentitySecretValue(&Lease{Identity: json.RawMessage(idJSON)})
	if err != nil {
		t.Fatalf("identity lease: %v", err)
	}
	if got != idJSON {
		t.Errorf("identity secret = %q, want verbatim %q", got, idJSON)
	}

	// Identity JSON wins when both are present (already enrolled — the JWT
	// would be consumed/useless).
	got, err = IdentitySecretValue(&Lease{Identity: json.RawMessage(idJSON), EnrollmentJWT: "ey.abc"})
	if err != nil || got != idJSON {
		t.Errorf("both-material lease = (%q, %v), want identity JSON", got, err)
	}

	// Neither → error (fail closed).
	if _, err := IdentitySecretValue(&Lease{}); err == nil {
		t.Error("empty lease: want error, got nil")
	}
	if _, err := IdentitySecretValue(nil); err == nil {
		t.Error("nil lease: want error, got nil")
	}
}

// --- Launch: per-run identity regime -----------------------------------------

// TestLaunchPerRunIdentityHappyPath: with the issuer wired, Launch mints a
// lease, writes BOTH per-run secrets, points the run-hook identitySecretArn at
// the per-run identity secret (NOT the static one), and returns a handle that
// Terminate will fully revoke.
func TestLaunchPerRunIdentityHappyPath(t *testing.T) {
	sm := newFakeSecrets()
	mv := &fakeMicrovm{runID: "mvm-pr1"}
	iss := &fakeIssuer{}
	b := perRunBackend(t, mv, sm, iss)

	handle, err := b.Launch(context.Background(), AgentSpec{BeadID: "gd-PR1"})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if len(iss.issued) != 1 || iss.issued[0] != "gd-PR1" {
		t.Errorf("issued = %v, want [gd-PR1]", iss.issued)
	}
	// Lease TTL covers the max duration plus slack.
	wantTTL := time.Duration(defaultMaximumDurationSeconds)*time.Second + leaseTTLSlack
	if iss.issueTTL != wantTTL {
		t.Errorf("lease ttl = %s, want %s", iss.issueTTL, wantTTL)
	}

	// Both per-run secrets written under the bead's prefix.
	idName := "ziti-microvm-agents/gd-PR1/ziti-identity"
	cfgName := "ziti-microvm-agents/gd-PR1/agent-config"
	if _, ok := sm.created[idName]; !ok {
		t.Errorf("identity secret %q not created; created=%v", idName, sm.created)
	}
	if _, ok := sm.created[cfgName]; !ok {
		t.Errorf("agent-config secret %q not created; created=%v", cfgName, sm.created)
	}

	// Handle marks the identity as per-run so every teardown path revokes it.
	if !handle.IsPerRunIdentity {
		t.Error("handle.IsPerRunIdentity = false, want true under the per-run regime")
	}
	if handle.IdentitySecretARN != sm.created[idName] {
		t.Errorf("handle.IdentitySecretARN = %q, want per-run %q", handle.IdentitySecretARN, sm.created[idName])
	}

	// Run-hook payload points at the per-run identity secret, never the static.
	var hook runHookPayload
	if err := json.Unmarshal([]byte(aws.ToString(mv.runInput.RunHookPayload)), &hook); err != nil {
		t.Fatalf("unmarshal run-hook payload: %v", err)
	}
	if hook.IdentitySecretArn != sm.created[idName] {
		t.Errorf("hook.IdentitySecretArn = %q, want per-run secret ARN", hook.IdentitySecretArn)
	}
	if hook.IdentitySecretArn == testBackendConfig().StaticIdentitySecretARN {
		t.Error("run-hook still points at the static POC identity under the per-run regime")
	}
	// The identity material must never ride the run-hook payload.
	if strings.Contains(aws.ToString(mv.runInput.RunHookPayload), "jwt-gd-PR1") {
		t.Error("run-hook payload leaked the enrollment JWT; material must live only in the secret")
	}
}

// TestLaunchFailsClosedOnIssuanceError: issuance failure aborts the launch —
// no secret written, no RunMicrovm, and NEVER a fallback to the static
// identity (contract §5.3).
func TestLaunchFailsClosedOnIssuanceError(t *testing.T) {
	sm := newFakeSecrets()
	mv := &fakeMicrovm{}
	iss := &fakeIssuer{issueErr: errors.New("broker 403: role not allowed")}
	b := perRunBackend(t, mv, sm, iss)

	_, err := b.Launch(context.Background(), AgentSpec{BeadID: "gd-FCLOSED"})
	if err == nil {
		t.Fatal("Launch: want error on issuance failure, got nil")
	}
	if mv.runCalls != 0 {
		t.Errorf("runCalls = %d, want 0 (fail closed: no launch without a fresh identity)", mv.runCalls)
	}
	if len(sm.created) != 0 {
		t.Errorf("created = %v, want no secrets written on issuance failure", sm.created)
	}
}

// TestLaunchFailsClosedOnUnusableLease: a lease with no material aborts the
// launch and revokes the just-minted lease.
func TestLaunchFailsClosedOnUnusableLease(t *testing.T) {
	sm := newFakeSecrets()
	mv := &fakeMicrovm{}
	iss := &fakeIssuer{lease: &Lease{LeaseID: "lease-empty"}} // no JWT, no identity
	b := perRunBackend(t, mv, sm, iss)

	_, err := b.Launch(context.Background(), AgentSpec{BeadID: "gd-EMPTY"})
	if err == nil {
		t.Fatal("Launch: want error on unusable lease, got nil")
	}
	if mv.runCalls != 0 {
		t.Errorf("runCalls = %d, want 0", mv.runCalls)
	}
	if len(iss.revoked) != 1 || iss.revoked[0] != "gd-EMPTY" {
		t.Errorf("revoked = %v, want the unusable lease revoked", iss.revoked)
	}
}

// TestLaunchIdentitySecretWriteFailureRevokesLease: the identity secret write
// failing after issuance revokes the lease and aborts before RunMicrovm.
func TestLaunchIdentitySecretWriteFailureRevokesLease(t *testing.T) {
	sm := newFakeSecrets()
	sm.createErr = func(name string) error {
		if strings.HasSuffix(name, "/ziti-identity") {
			return errors.New("secrets down")
		}
		return nil
	}
	mv := &fakeMicrovm{}
	iss := &fakeIssuer{}
	b := perRunBackend(t, mv, sm, iss)

	_, err := b.Launch(context.Background(), AgentSpec{BeadID: "gd-IDWF"})
	if err == nil {
		t.Fatal("Launch: want error on identity secret write failure, got nil")
	}
	if mv.runCalls != 0 {
		t.Errorf("runCalls = %d, want 0", mv.runCalls)
	}
	if len(iss.revoked) != 1 || iss.revoked[0] != "gd-IDWF" {
		t.Errorf("revoked = %v, want lease revoked on secret-write rollback", iss.revoked)
	}
}

// TestLaunchRunFailureRevokesIdentityAndSecrets is the failed-launch row of the
// revocation matrix: a RunMicrovm failure must delete BOTH per-run secrets and
// revoke the identity lease.
func TestLaunchRunFailureRevokesIdentityAndSecrets(t *testing.T) {
	sm := newFakeSecrets()
	mv := &fakeMicrovm{runErr: errors.New("AccessDenied")}
	iss := &fakeIssuer{}
	b := perRunBackend(t, mv, sm, iss)

	_, err := b.Launch(context.Background(), AgentSpec{BeadID: "gd-RUNF"})
	if err == nil {
		t.Fatal("Launch: want error, got nil")
	}
	idARN := sm.created["ziti-microvm-agents/gd-RUNF/ziti-identity"]
	cfgARN := sm.created["ziti-microvm-agents/gd-RUNF/agent-config"]
	deleted := map[string]bool{}
	for _, d := range sm.deleted {
		deleted[d] = true
	}
	if !deleted[idARN] || !deleted[cfgARN] {
		t.Errorf("deleted = %v, want both %q and %q rolled back", sm.deleted, idARN, cfgARN)
	}
	if len(iss.revoked) != 1 || iss.revoked[0] != "gd-RUNF" {
		t.Errorf("revoked = %v, want the identity lease revoked on failed launch", iss.revoked)
	}
}

// --- Terminate: per-run identity regime ---------------------------------------

// TestTerminateRevokesPerRunIdentityAndLease is the normal-termination row of
// the revocation matrix: Terminate deletes both per-run secrets AND revokes the
// broker lease by bead ID.
func TestTerminateRevokesPerRunIdentityAndLease(t *testing.T) {
	sm := newFakeSecrets()
	mv := &fakeMicrovm{}
	iss := &fakeIssuer{}
	b := perRunBackend(t, mv, sm, iss)

	err := b.Terminate(context.Background(), RuntimeHandle{
		BeadID:            "gd-TPR",
		MicrovmID:         "mvm-tpr",
		AgentSecretARN:    "arn:cfg",
		IdentitySecretARN: "arn:identity",
		IsPerRunIdentity:  true,
	}, "AgentStop")
	if err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	deleted := map[string]bool{}
	for _, d := range sm.deleted {
		deleted[d] = true
	}
	if !deleted["arn:cfg"] || !deleted["arn:identity"] {
		t.Errorf("deleted = %v, want both per-run secrets", sm.deleted)
	}
	if len(iss.revoked) != 1 || iss.revoked[0] != "gd-TPR" {
		t.Errorf("revoked = %v, want [gd-TPR]", iss.revoked)
	}
}

// TestTerminateSurfacesRevokeError: a broker revoke failure is returned (so C5
// retries) but never blocks the VM/secret teardown that already happened.
func TestTerminateSurfacesRevokeError(t *testing.T) {
	sm := newFakeSecrets()
	mv := &fakeMicrovm{}
	iss := &fakeIssuer{revokeErr: errors.New("broker unreachable")}
	b := perRunBackend(t, mv, sm, iss)

	err := b.Terminate(context.Background(), RuntimeHandle{
		BeadID: "gd-TRE", MicrovmID: "mvm-tre", AgentSecretARN: "arn:cfg", IsPerRunIdentity: false,
	}, "stop")
	if err == nil || !strings.Contains(err.Error(), "broker unreachable") {
		t.Fatalf("Terminate: err = %v, want the revoke error surfaced", err)
	}
	if len(mv.termCalls) != 1 || len(sm.deleted) != 1 {
		t.Errorf("teardown must proceed despite revoke failure: term=%v del=%v", mv.termCalls, sm.deleted)
	}
}

// TestTerminateStaticRegimeUntouchedByIssuer: without an issuer the behavior is
// byte-identical to the POC path (no revoke calls attempted).
func TestTerminateStaticRegimeUntouchedByIssuer(t *testing.T) {
	sm := newFakeSecrets()
	b := mustBackend(t, &fakeMicrovm{}, sm, testBackendConfig())
	err := b.Terminate(context.Background(), RuntimeHandle{
		BeadID: "gd-STAT", MicrovmID: "mvm-s", AgentSecretARN: "arn:cfg",
	}, "stop")
	if err != nil {
		t.Fatalf("Terminate: %v", err)
	}
}

// --- Lifecycle: timeout + GC rows of the revocation matrix --------------------

func mustPerRunLifecycle(t *testing.T, mv microvmAPI, sm secretsAPI, iss identityIssuer, store BeadRuntimeStore) *Lifecycle {
	t.Helper()
	b := mustBackend(t, mv, sm, testBackendConfig())
	b.SetLauncherAccount("345678901234")
	b.SetIssuer(iss)
	return NewLifecycle(b, store, discardLogger())
}

// TestLifecycle_Reconcile_TerminalRevokesPerRunIdentity is the timeout row: a
// MaximumDurationInSeconds expiry surfaces as a terminal runtime in Reconcile
// (no AgentStop event ever fires), which must revoke the per-run secrets and
// broker lease before closing the bead.
func TestLifecycle_Reconcile_TerminalRevokesPerRunIdentity(t *testing.T) {
	mv := &fakeMicrovm{getState: mvtypes.MicrovmStateTerminated}
	sm := newFakeSecrets()
	iss := &fakeIssuer{}
	store := newFakeBeadStore()
	l := mustPerRunLifecycle(t, mv, sm, iss, store)

	out, err := l.Reconcile(context.Background(), AgentBeadView{
		ID: "gd-timeout", ExecutionTarget: ExecutionTargetLambdaMicroVM, RuntimeID: "mvm-to", AgentState: "working",
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !out.Closed {
		t.Fatalf("terminal runtime should close the bead, got %+v", out)
	}
	if len(iss.revoked) != 1 || iss.revoked[0] != "gd-timeout" {
		t.Errorf("revoked = %v, want the timed-out run's lease revoked", iss.revoked)
	}
	// Both deterministic per-run secret names must be deleted.
	wantDeleted := map[string]bool{
		"ziti-microvm-agents/gd-timeout/agent-config":  false,
		"ziti-microvm-agents/gd-timeout/ziti-identity": false,
	}
	for _, d := range sm.deleted {
		if _, ok := wantDeleted[d]; ok {
			wantDeleted[d] = true
		}
	}
	for name, seen := range wantDeleted {
		if !seen {
			t.Errorf("per-run secret %q not deleted on terminal reconcile; deleted=%v", name, sm.deleted)
		}
	}
	if _, closed := store.closed["gd-timeout"]; !closed {
		t.Error("bead must still be closed after cleanup")
	}
}

// TestLifecycle_Reconcile_CleanupFailureStillCloses: a cleanup error on the
// terminal path must not block closing the bead (leak GC + broker reaper
// backstop the cleanup, nothing backstops a bead that never closes).
func TestLifecycle_Reconcile_CleanupFailureStillCloses(t *testing.T) {
	mv := &fakeMicrovm{getState: mvtypes.MicrovmStateTerminated}
	sm := newFakeSecrets()
	sm.deleteErr = errors.New("secrets down")
	iss := &fakeIssuer{revokeErr: errors.New("broker down")}
	store := newFakeBeadStore()
	l := mustPerRunLifecycle(t, mv, sm, iss, store)

	out, err := l.Reconcile(context.Background(), AgentBeadView{
		ID: "gd-cleanfail", ExecutionTarget: ExecutionTargetLambdaMicroVM, RuntimeID: "mvm-cf", AgentState: "working",
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !out.Closed {
		t.Fatalf("bead must close despite cleanup failure, got %+v", out)
	}
	if _, closed := store.closed["gd-cleanfail"]; !closed {
		t.Error("bead must be closed despite cleanup failure")
	}
}

// TestLifecycle_Terminate_PerRunRevokes: the stop/cancel teardown path under
// the per-run regime revokes the deterministic identity secret + lease even
// when the view carries no ARNs (controller restarted since launch).
func TestLifecycle_Terminate_PerRunRevokes(t *testing.T) {
	mv := &fakeMicrovm{}
	sm := newFakeSecrets()
	iss := &fakeIssuer{}
	l := mustPerRunLifecycle(t, mv, sm, iss, newFakeBeadStore())

	err := l.Terminate(context.Background(), AgentBeadView{
		ID: "gd-lstop", ExecutionTarget: ExecutionTargetLambdaMicroVM, RuntimeID: "mvm-lstop",
	}, "AgentStop")
	if err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if len(iss.revoked) != 1 || iss.revoked[0] != "gd-lstop" {
		t.Errorf("revoked = %v, want [gd-lstop]", iss.revoked)
	}
	found := false
	for _, d := range sm.deleted {
		if d == "ziti-microvm-agents/gd-lstop/ziti-identity" {
			found = true
		}
	}
	if !found {
		t.Errorf("deleted = %v, want the deterministic per-run identity secret", sm.deleted)
	}
}

// TestLifecycle_GC_RevokesLeakedIdentityLeases is the leak-GC row: a leaked
// bead's secrets are swept AND its broker leases revoked, deduped per bead.
func TestLifecycle_GC_RevokesLeakedIdentityLeases(t *testing.T) {
	old := time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)
	now := old.Add(time.Hour)
	sm := newFakeSecrets()
	sm.listSecrets = []smtypes.SecretListEntry{
		{Name: aws.String("ziti-microvm-agents/gd-leak/agent-config"), ARN: aws.String("arn:leak-cfg"), CreatedDate: aws.Time(old),
			Tags: []smtypes.Tag{{Key: aws.String(tagResourceBead), Value: aws.String("gd-leak")}}},
		{Name: aws.String("ziti-microvm-agents/gd-leak/ziti-identity"), ARN: aws.String("arn:leak-id"), CreatedDate: aws.Time(old),
			Tags: []smtypes.Tag{{Key: aws.String(tagResourceBead), Value: aws.String("gd-leak")}}},
		{Name: aws.String("ziti-microvm-agents/gd-live/agent-config"), ARN: aws.String("arn:live"), CreatedDate: aws.Time(old),
			Tags: []smtypes.Tag{{Key: aws.String(tagResourceBead), Value: aws.String("gd-live")}}},
	}
	iss := &fakeIssuer{}
	l := mustPerRunLifecycle(t, &fakeMicrovm{}, sm, iss, newFakeBeadStore())

	stats, err := l.GC(context.Background(), map[string]bool{}, map[string]bool{"gd-live": true}, nil, now)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if stats.SecretsDeleted != 2 {
		t.Errorf("SecretsDeleted = %d, want 2 (both leaked secrets)", stats.SecretsDeleted)
	}
	// One revoke for the leaked bead (deduped across its two secrets), none for
	// the live one.
	if len(iss.revoked) != 1 || iss.revoked[0] != "gd-leak" {
		t.Errorf("revoked = %v, want exactly [gd-leak]", iss.revoked)
	}
}

// --- BrokerClient HTTP contract ----------------------------------------------

// staticCreds is a fixed aws.CredentialsProvider for signing tests.
type staticCreds struct{}

func (staticCreds) Retrieve(context.Context) (aws.Credentials, error) {
	return aws.Credentials{AccessKeyID: "AKIAIOSFODNN7EXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"}, nil
}

func testBrokerClient(t *testing.T, serverURL string, opts BrokerOptions) *BrokerClient {
	t.Helper()
	opts.URL = serverURL
	c, err := newBrokerClientWithCreds(opts, staticCreds{}, "us-east-1", discardLogger())
	if err != nil {
		t.Fatalf("newBrokerClientWithCreds: %v", err)
	}
	return c
}

// TestBrokerIssueLeaseRequestShape asserts the POST /lease body matches the
// contract: signed STS proof (with the bound server-id header inside the
// signed headers), role-scoped grant, task_id, ttl.
func TestBrokerIssueLeaseRequestShape(t *testing.T) {
	var gotBody leaseRequest
	var gotPath, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode lease request: %v", err)
		}
		json.NewEncoder(w).Encode(Lease{LeaseID: "l1", IdentityID: "i1", EnrollmentJWT: "ey.jwt"})
	}))
	defer srv.Close()

	c := testBrokerClient(t, srv.URL, BrokerOptions{ServerID: "broker.test"})
	lease, err := c.IssueLease(context.Background(), "gd-REQ1", time.Hour)
	if err != nil {
		t.Fatalf("IssueLease: %v", err)
	}
	if lease.LeaseID != "l1" || lease.EnrollmentJWT != "ey.jwt" {
		t.Errorf("lease = %+v", lease)
	}
	if gotMethod != http.MethodPost || gotPath != "/lease" {
		t.Errorf("request = %s %s, want POST /lease", gotMethod, gotPath)
	}
	if gotBody.TaskID != "gd-REQ1" || gotBody.TTL != "1h0m0s" {
		t.Errorf("body task/ttl = %q/%q", gotBody.TaskID, gotBody.TTL)
	}
	if len(gotBody.IdentityRoles) != 1 || gotBody.IdentityRoles[0] != defaultIdentityRole {
		t.Errorf("identity_roles = %v, want [%s]", gotBody.IdentityRoles, defaultIdentityRole)
	}
	if gotBody.Enroll {
		t.Error("enroll = true, want false by default (Option A)")
	}
	// Signed STS proof shape.
	auth := gotBody.Auth
	if auth.Kind != "aws_sts_get_caller_identity" {
		t.Errorf("auth.kind = %q", auth.Kind)
	}
	if auth.STSRequest.Body != stsGetCallerIdentityBody {
		t.Errorf("sts body = %q", auth.STSRequest.Body)
	}
	if got := auth.STSRequest.Headers[brokerServerIDHeader]; got != "broker.test" {
		t.Errorf("bound server-id header = %q, want broker.test", got)
	}
	authz := auth.STSRequest.Headers["Authorization"]
	if !strings.Contains(authz, "AWS4-HMAC-SHA256") || !strings.Contains(authz, "SignedHeaders=") {
		t.Errorf("Authorization header not sigv4-signed: %q", authz)
	}
	// The bound header must be covered by the signature.
	if !strings.Contains(strings.ToLower(authz), strings.ToLower(brokerServerIDHeader)) {
		t.Errorf("bound header not in SignedHeaders: %q", authz)
	}
}

// TestBrokerIssueLeaseErrors: non-200 and unusable-material responses are
// errors (the launch path treats both as fail-closed).
func TestBrokerIssueLeaseErrors(t *testing.T) {
	t.Run("http 403", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, `{"error":"role not allowed"}`, http.StatusForbidden)
		}))
		defer srv.Close()
		c := testBrokerClient(t, srv.URL, BrokerOptions{})
		if _, err := c.IssueLease(context.Background(), "gd-E1", time.Hour); err == nil || !strings.Contains(err.Error(), "403") {
			t.Fatalf("err = %v, want http 403 surfaced", err)
		}
	})
	t.Run("no material", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			json.NewEncoder(w).Encode(Lease{LeaseID: "l2"})
		}))
		defer srv.Close()
		c := testBrokerClient(t, srv.URL, BrokerOptions{})
		if _, err := c.IssueLease(context.Background(), "gd-E2", time.Hour); err == nil || !strings.Contains(err.Error(), "unusable") {
			t.Fatalf("err = %v, want unusable-lease error", err)
		}
	})
	t.Run("unreachable", func(t *testing.T) {
		c := testBrokerClient(t, "http://127.0.0.1:1", BrokerOptions{ServerID: "x"})
		if _, err := c.IssueLease(context.Background(), "gd-E3", time.Hour); err == nil {
			t.Fatal("want error on unreachable broker")
		}
	})
	t.Run("empty task id", func(t *testing.T) {
		c := testBrokerClient(t, "http://127.0.0.1:1", BrokerOptions{ServerID: "x"})
		if _, err := c.IssueLease(context.Background(), " ", time.Hour); err == nil {
			t.Fatal("want error on empty task id")
		}
	})
}

// TestBrokerRevokeLeases: DELETE /lease?task_id=… with a signed proof; 204 and
// 404 are success (idempotent), anything else is an error.
func TestBrokerRevokeLeases(t *testing.T) {
	var gotTask, gotMethod string
	var gotAuthKind string
	status := http.StatusNoContent
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotTask = r.URL.Query().Get("task_id")
		var body struct {
			Auth authProof `json:"auth"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotAuthKind = body.Auth.Kind
		w.WriteHeader(status)
	}))
	defer srv.Close()

	c := testBrokerClient(t, srv.URL, BrokerOptions{ServerID: "broker.test"})
	if err := c.RevokeLeases(context.Background(), "gd-RV1"); err != nil {
		t.Fatalf("RevokeLeases: %v", err)
	}
	if gotMethod != http.MethodDelete || gotTask != "gd-RV1" {
		t.Errorf("request = %s task_id=%s, want DELETE gd-RV1", gotMethod, gotTask)
	}
	if gotAuthKind != "aws_sts_get_caller_identity" {
		t.Errorf("revoke auth kind = %q, want signed proof", gotAuthKind)
	}

	// 404 (no leases) is idempotent success.
	status = http.StatusNotFound
	if err := c.RevokeLeases(context.Background(), "gd-RV2"); err != nil {
		t.Fatalf("RevokeLeases on 404: %v", err)
	}

	// 500 is a real failure.
	status = http.StatusInternalServerError
	if err := c.RevokeLeases(context.Background(), "gd-RV3"); err == nil {
		t.Fatal("RevokeLeases on 500: want error")
	}

	// Empty task id is a no-op.
	if err := c.RevokeLeases(context.Background(), " "); err != nil {
		t.Fatalf("RevokeLeases empty task: %v", err)
	}
}

// TestBrokerOptionsDefaults: server id derives from the URL host; roles and
// enrollment TTL default per the contract.
func TestBrokerOptionsDefaults(t *testing.T) {
	c, err := newBrokerClientWithCreds(BrokerOptions{URL: "http://broker.fabric:8080/"}, staticCreds{}, "us-east-1", discardLogger())
	if err != nil {
		t.Fatalf("newBrokerClientWithCreds: %v", err)
	}
	if c.opts.URL != "http://broker.fabric:8080" {
		t.Errorf("URL = %q, want trailing slash trimmed", c.opts.URL)
	}
	if c.opts.ServerID != "broker.fabric:8080" {
		t.Errorf("ServerID = %q, want derived from URL host", c.opts.ServerID)
	}
	if len(c.opts.IdentityRoles) != 1 || c.opts.IdentityRoles[0] != defaultIdentityRole {
		t.Errorf("IdentityRoles = %v", c.opts.IdentityRoles)
	}
	if c.opts.EnrollmentTTL != defaultEnrollmentTTL {
		t.Errorf("EnrollmentTTL = %s, want %s", c.opts.EnrollmentTTL, defaultEnrollmentTTL)
	}

	if _, err := newBrokerClientWithCreds(BrokerOptions{}, staticCreds{}, "us-east-1", nil); err == nil {
		t.Error("empty URL: want error")
	}
	if _, err := newBrokerClientWithCreds(BrokerOptions{URL: "http://x"}, nil, "us-east-1", nil); err == nil {
		t.Error("nil creds: want error")
	}
}

// TestNewBrokerClientRequiresOriginCredentials: the exported constructor must
// refuse a nil credentials provider rather than fall back to resolving its own
// default chain — the proof would then attest the shared agent role instead of
// the identity's origin (the dedicated controller role in production,
// ka-0000006814).
func TestNewBrokerClientRequiresOriginCredentials(t *testing.T) {
	_, err := NewBrokerClient(context.Background(), BrokerOptions{URL: "http://broker.fabric:8080"}, nil, discardLogger())
	if err == nil {
		t.Fatal("NewBrokerClient(nil creds): want error, got nil")
	}
	if !strings.Contains(err.Error(), "origin credentials") {
		t.Errorf("error %q should name the missing origin credentials", err)
	}
}
