package microvm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms"
	mvtypes "github.com/aws/aws-sdk-go-v2/service/lambdamicrovms/types"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	smtypes "github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
)

// --- fakes -----------------------------------------------------------------

// fakeSecrets records CreateSecret/PutSecretValue/DeleteSecret calls and lets a
// test inject errors per-op. It simulates a name→ARN store so idempotency and
// cleanup can be asserted.
type fakeSecrets struct {
	created  map[string]string // name → ARN
	putCalls []string          // secret ids put
	deleted  []string          // secret ids deleted

	createErr func(name string) error
	putErr    error
	deleteErr error

	lastCreateTags []smtypes.Tag
	lastCreateBody string

	// listSecrets is returned by ListSecrets (for leak-GC tests); listErr
	// overrides it with an error when set.
	listSecrets []smtypes.SecretListEntry
	listErr     error
	listInputs  []*secretsmanager.ListSecretsInput
}

func newFakeSecrets() *fakeSecrets { return &fakeSecrets{created: map[string]string{}} }

func (f *fakeSecrets) CreateSecret(_ context.Context, in *secretsmanager.CreateSecretInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.CreateSecretOutput, error) {
	name := aws.ToString(in.Name)
	if f.createErr != nil {
		if err := f.createErr(name); err != nil {
			return nil, err
		}
	}
	f.lastCreateTags = in.Tags
	f.lastCreateBody = aws.ToString(in.SecretString)
	arn := "arn:aws:secretsmanager:us-east-2:345678901234:secret:" + name + "-AbCdEf"
	f.created[name] = arn
	return &secretsmanager.CreateSecretOutput{ARN: aws.String(arn), Name: aws.String(name)}, nil
}

func (f *fakeSecrets) PutSecretValue(_ context.Context, in *secretsmanager.PutSecretValueInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.PutSecretValueOutput, error) {
	if f.putErr != nil {
		return nil, f.putErr
	}
	id := aws.ToString(in.SecretId)
	f.putCalls = append(f.putCalls, id)
	arn := f.created[id]
	if arn == "" {
		arn = "arn:aws:secretsmanager:us-east-2:345678901234:secret:" + id + "-AbCdEf"
	}
	return &secretsmanager.PutSecretValueOutput{ARN: aws.String(arn)}, nil
}

func (f *fakeSecrets) DeleteSecret(_ context.Context, in *secretsmanager.DeleteSecretInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.DeleteSecretOutput, error) {
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	f.deleted = append(f.deleted, aws.ToString(in.SecretId))
	return &secretsmanager.DeleteSecretOutput{}, nil
}

func (f *fakeSecrets) ListSecrets(_ context.Context, in *secretsmanager.ListSecretsInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.ListSecretsOutput, error) {
	f.listInputs = append(f.listInputs, in)
	if f.listErr != nil {
		return nil, f.listErr
	}
	return &secretsmanager.ListSecretsOutput{SecretList: f.listSecrets}, nil
}

// fakeMicrovm records RunMicrovm/GetMicrovm/TerminateMicrovm/SuspendMicrovm/
// ResumeMicrovm calls.
type fakeMicrovm struct {
	runInput  *lambdamicrovms.RunMicrovmInput
	runErr    error
	runID     string
	runState  mvtypes.MicrovmState
	getErr    error
	getState  mvtypes.MicrovmState
	getReason string
	getStart  *time.Time
	termErr   error
	termCalls []string
	runCalls  int

	// suspErr/resErr override the Suspend/Resume happy path with an error;
	// suspCalls/resCalls record the MicroVM identifiers each was called with.
	suspErr   error
	suspCalls []string
	resErr    error
	resCalls  []string

	// listItems is returned by ListMicrovms (for leak-GC tests); listErr
	// overrides it with an error when set.
	listItems  []mvtypes.MicrovmItem
	listErr    error
	listInputs []*lambdamicrovms.ListMicrovmsInput
}

func (f *fakeMicrovm) RunMicrovm(_ context.Context, in *lambdamicrovms.RunMicrovmInput, _ ...func(*lambdamicrovms.Options)) (*lambdamicrovms.RunMicrovmOutput, error) {
	f.runCalls++
	f.runInput = in
	if f.runErr != nil {
		return nil, f.runErr
	}
	id := f.runID
	if id == "" {
		id = "mvm-abc123"
	}
	state := f.runState
	if state == "" {
		state = mvtypes.MicrovmStatePending
	}
	return &lambdamicrovms.RunMicrovmOutput{
		MicrovmId: aws.String(id),
		State:     state,
		ImageArn:  in.ImageIdentifier,
	}, nil
}

func (f *fakeMicrovm) GetMicrovm(_ context.Context, in *lambdamicrovms.GetMicrovmInput, _ ...func(*lambdamicrovms.Options)) (*lambdamicrovms.GetMicrovmOutput, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return &lambdamicrovms.GetMicrovmOutput{
		MicrovmId:   in.MicrovmIdentifier,
		State:       f.getState,
		StateReason: aws.String(f.getReason),
		StartedAt:   f.getStart,
	}, nil
}

func (f *fakeMicrovm) ListMicrovms(_ context.Context, in *lambdamicrovms.ListMicrovmsInput, _ ...func(*lambdamicrovms.Options)) (*lambdamicrovms.ListMicrovmsOutput, error) {
	f.listInputs = append(f.listInputs, in)
	if f.listErr != nil {
		return nil, f.listErr
	}
	return &lambdamicrovms.ListMicrovmsOutput{Items: f.listItems}, nil
}

func (f *fakeMicrovm) TerminateMicrovm(_ context.Context, in *lambdamicrovms.TerminateMicrovmInput, _ ...func(*lambdamicrovms.Options)) (*lambdamicrovms.TerminateMicrovmOutput, error) {
	f.termCalls = append(f.termCalls, aws.ToString(in.MicrovmIdentifier))
	if f.termErr != nil {
		return nil, f.termErr
	}
	return &lambdamicrovms.TerminateMicrovmOutput{}, nil
}

func (f *fakeMicrovm) SuspendMicrovm(_ context.Context, in *lambdamicrovms.SuspendMicrovmInput, _ ...func(*lambdamicrovms.Options)) (*lambdamicrovms.SuspendMicrovmOutput, error) {
	f.suspCalls = append(f.suspCalls, aws.ToString(in.MicrovmIdentifier))
	if f.suspErr != nil {
		return nil, f.suspErr
	}
	return &lambdamicrovms.SuspendMicrovmOutput{}, nil
}

func (f *fakeMicrovm) ResumeMicrovm(_ context.Context, in *lambdamicrovms.ResumeMicrovmInput, _ ...func(*lambdamicrovms.Options)) (*lambdamicrovms.ResumeMicrovmOutput, error) {
	f.resCalls = append(f.resCalls, aws.ToString(in.MicrovmIdentifier))
	if f.resErr != nil {
		return nil, f.resErr
	}
	return &lambdamicrovms.ResumeMicrovmOutput{}, nil
}

// testBackendConfig is a complete, valid launch config for tests.
func testBackendConfig() BackendConfig {
	return BackendConfig{
		Region:                  "us-east-2",
		ImageIdentifier:         "arn:aws:lambda:us-east-2:345678901234:microvm-image:ziti-microvm-poc",
		ImageVersion:            "7.0",
		ExecutionRoleARN:        "arn:aws:iam::345678901234:role/ziti-microvm-poc-execution",
		SecretPrefix:            "ziti-microvm-agents/",
		StaticIdentitySecretARN: "arn:aws:secretsmanager:us-east-2:345678901234:secret:ziti-microvm-poc/ziti-identity-kCfNSS",
	}
}

func mustBackend(t *testing.T, mv microvmAPI, sm secretsAPI, cfg BackendConfig) *Backend {
	t.Helper()
	b, err := newBackendWithClients(mv, sm, cfg, discardLogger())
	if err != nil {
		t.Fatalf("newBackendWithClients: %v", err)
	}
	return b
}

// --- config validation -----------------------------------------------------

func TestBackendConfigValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*BackendConfig)
		wantErr string
	}{
		{"complete", func(*BackendConfig) {}, ""},
		{"no region", func(c *BackendConfig) { c.Region = "" }, "Region"},
		{"no image", func(c *BackendConfig) { c.ImageIdentifier = "" }, "ImageIdentifier"},
		{"no exec role", func(c *BackendConfig) { c.ExecutionRoleARN = "" }, "ExecutionRoleARN"},
		{"no prefix", func(c *BackendConfig) { c.SecretPrefix = "" }, "SecretPrefix"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testBackendConfig()
			tc.mutate(&cfg)
			cfg.SecretPrefix = normalizeSecretPrefix(cfg.SecretPrefix)
			err := cfg.validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validate: unexpected error %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validate: err = %v, want mention of %q", err, tc.wantErr)
			}
		})
	}
}

func TestNormalizeSecretPrefix(t *testing.T) {
	cases := map[string]string{
		"ziti-microvm-agents/": "ziti-microvm-agents/",
		"ziti-microvm-agents":  "ziti-microvm-agents/",
		"  ziti/  ":            "ziti/",
		"a/b///":               "a/b/",
		"":                     "",
	}
	for in, want := range cases {
		if got := normalizeSecretPrefix(in); got != want {
			t.Errorf("normalizeSecretPrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- Launch happy path -----------------------------------------------------

func TestLaunchHappyPath(t *testing.T) {
	sm := newFakeSecrets()
	mv := &fakeMicrovm{runID: "mvm-xyz789", runState: mvtypes.MicrovmStatePending}
	b := mustBackend(t, mv, sm, testBackendConfig())

	spec := AgentSpec{
		BeadID:            "ka-0000008160",
		Project:           "gardenio",
		Role:              "crew",
		Environment:       map[string]string{"CLAUDE_MODEL": "opus", "BOAT_PROJECTS": "gardenio"},
		ClaudeCredentials: "creds-blob",
	}
	handle, err := b.Launch(context.Background(), spec)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	// One MicroVM, one secret, no deletes.
	if mv.runCalls != 1 {
		t.Errorf("runCalls = %d, want 1", mv.runCalls)
	}
	if len(sm.deleted) != 0 {
		t.Errorf("deleted = %v, want none on happy path", sm.deleted)
	}
	if handle.MicrovmID != "mvm-xyz789" {
		t.Errorf("handle.MicrovmID = %q, want mvm-xyz789", handle.MicrovmID)
	}
	if handle.BeadID != spec.BeadID {
		t.Errorf("handle.BeadID = %q, want %q", handle.BeadID, spec.BeadID)
	}
	if handle.IsPerRunIdentity {
		t.Error("handle.IsPerRunIdentity = true, want false (POC static identity)")
	}
	if handle.IdentitySecretARN != testBackendConfig().StaticIdentitySecretARN {
		t.Errorf("handle.IdentitySecretARN = %q, want static POC identity", handle.IdentitySecretARN)
	}

	// Secret written under the correct per-run path with the bead tag.
	wantName := "ziti-microvm-agents/ka-0000008160/agent-config"
	if _, ok := sm.created[wantName]; !ok {
		t.Errorf("secret %q not created; created=%v", wantName, sm.created)
	}
	if len(sm.lastCreateTags) != 1 || aws.ToString(sm.lastCreateTags[0].Key) != tagResourceBead ||
		aws.ToString(sm.lastCreateTags[0].Value) != spec.BeadID {
		t.Errorf("create tags = %+v, want single %s=%s", sm.lastCreateTags, tagResourceBead, spec.BeadID)
	}
	if handle.AgentSecretARN != sm.created[wantName] {
		t.Errorf("handle.AgentSecretARN = %q, want %q", handle.AgentSecretARN, sm.created[wantName])
	}
}

// TestLaunchClientTokenIsBeadID verifies the AWS idempotency token IS the bead
// ID (so duplicate spawn events collapse onto one MicroVM at the service).
func TestLaunchClientTokenIsBeadID(t *testing.T) {
	sm := newFakeSecrets()
	mv := &fakeMicrovm{}
	b := mustBackend(t, mv, sm, testBackendConfig())

	_, err := b.Launch(context.Background(), AgentSpec{BeadID: "gd-DUP123"})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if got := aws.ToString(mv.runInput.ClientToken); got != "gd-DUP123" {
		t.Errorf("ClientToken = %q, want the bead ID gd-DUP123", got)
	}
}

// TestLaunchRunInputShape asserts the RunMicrovm request matches the handoff
// contract: approved image + version, execution role, INTERNET_EGRESS only (no
// ingress connectors), and a run-hook payload carrying ONLY the secret ARNs.
func TestLaunchRunInputShape(t *testing.T) {
	sm := newFakeSecrets()
	mv := &fakeMicrovm{}
	cfg := testBackendConfig()
	b := mustBackend(t, mv, sm, cfg)

	_, err := b.Launch(context.Background(), AgentSpec{
		BeadID:      "gd-SHAPE1",
		Environment: map[string]string{"K": "V"},
		NATSToken:   "nats-secret", // must NOT appear in the run-hook payload
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	in := mv.runInput
	if aws.ToString(in.ImageIdentifier) != cfg.ImageIdentifier {
		t.Errorf("ImageIdentifier = %q, want %q", aws.ToString(in.ImageIdentifier), cfg.ImageIdentifier)
	}
	if aws.ToString(in.ImageVersion) != cfg.ImageVersion {
		t.Errorf("ImageVersion = %q, want %q", aws.ToString(in.ImageVersion), cfg.ImageVersion)
	}
	if aws.ToString(in.ExecutionRoleArn) != cfg.ExecutionRoleARN {
		t.Errorf("ExecutionRoleArn = %q, want %q", aws.ToString(in.ExecutionRoleArn), cfg.ExecutionRoleARN)
	}
	if len(in.IngressNetworkConnectors) != 0 {
		t.Errorf("IngressNetworkConnectors = %v, want none (outbound-only)", in.IngressNetworkConnectors)
	}
	wantEgress := internetEgressConnectorARN("us-east-2")
	if len(in.EgressNetworkConnectors) != 1 || in.EgressNetworkConnectors[0] != wantEgress {
		t.Errorf("EgressNetworkConnectors = %v, want [%s]", in.EgressNetworkConnectors, wantEgress)
	}
	if aws.ToInt32(in.MaximumDurationInSeconds) != defaultMaximumDurationSeconds {
		t.Errorf("MaximumDurationInSeconds = %d, want %d", aws.ToInt32(in.MaximumDurationInSeconds), defaultMaximumDurationSeconds)
	}

	// Run-hook payload: only ARNs + mode/runAgent, NO credential values.
	var hook runHookPayload
	if err := json.Unmarshal([]byte(aws.ToString(in.RunHookPayload)), &hook); err != nil {
		t.Fatalf("unmarshal run-hook payload: %v", err)
	}
	if hook.IdentitySecretArn != cfg.StaticIdentitySecretARN {
		t.Errorf("hook.IdentitySecretArn = %q, want static identity", hook.IdentitySecretArn)
	}
	if hook.AgentSecretArn == "" {
		t.Error("hook.AgentSecretArn empty; want the per-run secret ARN")
	}
	if hook.Mode != runHookMode || !hook.RunAgent {
		t.Errorf("hook mode/runAgent = %q/%v, want %q/true", hook.Mode, hook.RunAgent, runHookMode)
	}
	if strings.Contains(aws.ToString(in.RunHookPayload), "nats-secret") {
		t.Error("run-hook payload leaked the NATS token; credentials must live only in the secret")
	}
}

// TestLaunchSecretBodyCarriesCredentials verifies the secret VALUE (not the
// run-hook payload) carries the credential document with the file-oriented
// fields, and omits empty optionals.
func TestLaunchSecretBodyCarriesCredentials(t *testing.T) {
	sm := newFakeSecrets()
	mv := &fakeMicrovm{}
	b := mustBackend(t, mv, sm, testBackendConfig())

	_, err := b.Launch(context.Background(), AgentSpec{
		BeadID:            "gd-CRED1",
		Environment:       map[string]string{"CLAUDE_MODEL": "opus"},
		ClaudeCredentials: "claude-blob",
		// NATSToken + SSHPrivateKey intentionally empty → omitted from JSON.
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	var doc runtimeDocument
	if err := json.Unmarshal([]byte(sm.lastCreateBody), &doc); err != nil {
		t.Fatalf("unmarshal secret body: %v", err)
	}
	if doc.Environment["CLAUDE_MODEL"] != "opus" {
		t.Errorf("doc.Environment = %v, want CLAUDE_MODEL=opus", doc.Environment)
	}
	if doc.ClaudeCredentials != "claude-blob" {
		t.Errorf("doc.ClaudeCredentials = %q, want claude-blob", doc.ClaudeCredentials)
	}
	// Empty optionals must be omitted (omitempty) — assert on raw JSON.
	if strings.Contains(sm.lastCreateBody, "natsToken") || strings.Contains(sm.lastCreateBody, "sshPrivateKey") {
		t.Errorf("secret body kept empty optional fields: %s", sm.lastCreateBody)
	}
}

// TestLaunchBeadsTokenAndClaudeCredsRideSecretOnly is the least-privilege guard
// for the credential-delivery fix (bug ka-0000007063): the beads auth token
// (env) and the Claude credential (both env and file field) must land in the
// per-run task-scoped secret and must NEVER appear in the run-hook payload — the
// same exclusion backend_test already enforces for the NATS token. A leak into
// the run-hook payload would put the credential on the RunMicrovm control-plane
// call (logs, CloudTrail) instead of only inside the referenced secret.
func TestLaunchBeadsTokenAndClaudeCredsRideSecretOnly(t *testing.T) {
	sm := newFakeSecrets()
	mv := &fakeMicrovm{}
	b := mustBackend(t, mv, sm, testBackendConfig())

	_, err := b.Launch(context.Background(), AgentSpec{
		BeadID: "gd-CRED2",
		Environment: map[string]string{
			"BEADS_AUTH_TOKEN":        "beads-secret-token",
			"BEADS_DAEMON_TOKEN":      "beads-secret-token",
			"CLAUDE_CODE_OAUTH_TOKEN": "claude-secret-oauth",
			"GITLAB_TOKEN":            "glpat-per-run-git-secret",
			"GIT_USERNAME":            "gardenio-bot",
			"GIT_TOKEN":               "ghp-git-secret",
		},
		ClaudeCredentials: `{"claudeAiOauth":{"accessToken":"claude-secret-oauth"}}`,
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	// Present in the per-run secret body (the runtime document).
	var doc runtimeDocument
	if err := json.Unmarshal([]byte(sm.lastCreateBody), &doc); err != nil {
		t.Fatalf("unmarshal secret body: %v", err)
	}
	if doc.Environment["BEADS_AUTH_TOKEN"] != "beads-secret-token" {
		t.Errorf("secret env missing BEADS_AUTH_TOKEN: %v", doc.Environment)
	}
	if doc.Environment["CLAUDE_CODE_OAUTH_TOKEN"] != "claude-secret-oauth" {
		t.Errorf("secret env missing CLAUDE_CODE_OAUTH_TOKEN: %v", doc.Environment)
	}
	if doc.Environment["GITLAB_TOKEN"] != "glpat-per-run-git-secret" {
		t.Errorf("secret env missing GITLAB_TOKEN: %v", doc.Environment)
	}
	if !strings.Contains(doc.ClaudeCredentials, "claude-secret-oauth") {
		t.Errorf("secret ClaudeCredentials missing the OAuth token: %q", doc.ClaudeCredentials)
	}

	// Absent from the run-hook payload (ARNs + mode/runAgent only). Git
	// credential material (ka-0000004762) gets the same exclusion as the
	// beads/Claude/NATS credentials.
	hookPayload := aws.ToString(mv.runInput.RunHookPayload)
	for _, leak := range []string{
		"beads-secret-token", "claude-secret-oauth", "BEADS_AUTH_TOKEN", "claudeAiOauth",
		"glpat-per-run-git-secret", "ghp-git-secret", "GITLAB_TOKEN", "GIT_TOKEN",
	} {
		if strings.Contains(hookPayload, leak) {
			t.Errorf("run-hook payload leaked credential material %q; must live only in the secret: %s", leak, hookPayload)
		}
	}
}

// TestLaunchNatsTokenRidesSecretOnly is the least-privilege guard for the NATS
// contract delivery (task ka-0000006995): the agent NATS token — delivered in
// both consumable forms, COOP_NATS_TOKEN env and the file-oriented NATSToken
// field — must land in the per-run task-scoped secret and must NEVER appear in
// the run-hook payload (which would put it on the RunMicrovm control-plane
// call: logs, CloudTrail).
func TestLaunchNatsTokenRidesSecretOnly(t *testing.T) {
	sm := newFakeSecrets()
	mv := &fakeMicrovm{}
	b := mustBackend(t, mv, sm, testBackendConfig())

	_, err := b.Launch(context.Background(), AgentSpec{
		BeadID: "gd-NATS1",
		Environment: map[string]string{
			"COOP_NATS_TOKEN": "nats-secret-token",
			"COOP_NATS_URL":   "nats://127.0.0.1:14222",
			"BEADS_NATS_URL":  "nats://127.0.0.1:14222",
		},
		NATSToken: "nats-secret-token",
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	// Present in the per-run secret body: env form AND file-oriented form.
	var doc runtimeDocument
	if err := json.Unmarshal([]byte(sm.lastCreateBody), &doc); err != nil {
		t.Fatalf("unmarshal secret body: %v", err)
	}
	if doc.Environment["COOP_NATS_TOKEN"] != "nats-secret-token" {
		t.Errorf("secret env missing COOP_NATS_TOKEN: %v", doc.Environment)
	}
	if doc.Environment["COOP_NATS_URL"] != "nats://127.0.0.1:14222" {
		t.Errorf("secret env missing the local-proxy COOP_NATS_URL: %v", doc.Environment)
	}
	if doc.NATSToken != "nats-secret-token" {
		t.Errorf("doc.NATSToken = %q, want the file-oriented token", doc.NATSToken)
	}

	// Absent from the run-hook payload (ARNs + mode/runAgent only).
	hookPayload := aws.ToString(mv.runInput.RunHookPayload)
	for _, leak := range []string{"nats-secret-token", "COOP_NATS_TOKEN"} {
		if strings.Contains(hookPayload, leak) {
			t.Errorf("run-hook payload leaked NATS credential material %q; must live only in the secret: %s", leak, hookPayload)
		}
	}
}

// --- CloudWatch runtime logging (ka-0000002120) ------------------------------

// TestLaunchLoggingDefaultOff: with no log group configured, RunMicrovmInput's
// Logging member stays nil — byte-identical launch shape to the pre-logging POC
// (the image-level logging config, if any, is what applies).
func TestLaunchLoggingDefaultOff(t *testing.T) {
	sm := newFakeSecrets()
	mv := &fakeMicrovm{}
	b := mustBackend(t, mv, sm, testBackendConfig())

	if _, err := b.Launch(context.Background(), AgentSpec{BeadID: "gd-NOLOG"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if mv.runInput.Logging != nil {
		t.Errorf("Logging = %#v, want nil when no log group is configured", mv.runInput.Logging)
	}
}

// TestLaunchLoggingPerBeadStream: with a log group configured, every launch
// carries a CloudWatch Logging member targeting that group with a per-bead
// stream ("<prefix><beadID>") so concurrent agents never interleave and a run's
// logs are addressable from its bead ID alone.
func TestLaunchLoggingPerBeadStream(t *testing.T) {
	sm := newFakeSecrets()
	mv := &fakeMicrovm{}
	cfg := testBackendConfig()
	cfg.LogGroup = "/aws/lambda/microvms/ziti-microvm-poc"
	cfg.LogStreamPrefix = "agent/"
	b := mustBackend(t, mv, sm, cfg)

	if _, err := b.Launch(context.Background(), AgentSpec{BeadID: "gd-LOG1"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	cw, ok := mv.runInput.Logging.(*mvtypes.LoggingMemberCloudWatch)
	if !ok {
		t.Fatalf("Logging = %#v, want *LoggingMemberCloudWatch", mv.runInput.Logging)
	}
	if got := aws.ToString(cw.Value.LogGroup); got != cfg.LogGroup {
		t.Errorf("LogGroup = %q, want %q", got, cfg.LogGroup)
	}
	if got := aws.ToString(cw.Value.LogStream); got != "agent/gd-LOG1" {
		t.Errorf("LogStream = %q, want agent/gd-LOG1 (per-bead stream)", got)
	}
}

// TestLaunchLoggingDefaultStreamPrefix: a log group with no explicit stream
// prefix falls back to "agent/" rather than dumping every run into an unnamed
// shared stream.
func TestLaunchLoggingDefaultStreamPrefix(t *testing.T) {
	mv := &fakeMicrovm{}
	cfg := testBackendConfig()
	cfg.LogGroup = "/aws/lambda/microvms/ziti-microvm-poc"
	b := mustBackend(t, mv, newFakeSecrets(), cfg)

	if _, err := b.Launch(context.Background(), AgentSpec{BeadID: "gd-LOG2"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	cw, ok := mv.runInput.Logging.(*mvtypes.LoggingMemberCloudWatch)
	if !ok {
		t.Fatalf("Logging = %#v, want *LoggingMemberCloudWatch", mv.runInput.Logging)
	}
	if got := aws.ToString(cw.Value.LogStream); got != defaultLogStreamPrefix+"gd-LOG2" {
		t.Errorf("LogStream = %q, want %q", got, defaultLogStreamPrefix+"gd-LOG2")
	}
}

// TestBackendConfigRejectsMalformedLogging: CloudWatch stream names forbid ':'
// and '*'; a malformed group/prefix must fail the backend at boot (fail closed)
// instead of erroring on every launch.
func TestBackendConfigRejectsMalformedLogging(t *testing.T) {
	for name, mutate := range map[string]func(*BackendConfig){
		"colon in group":  func(c *BackendConfig) { c.LogGroup = "bad:group" },
		"star in group":   func(c *BackendConfig) { c.LogGroup = "bad*group" },
		"colon in prefix": func(c *BackendConfig) { c.LogGroup = "/ok"; c.LogStreamPrefix = "a:b/" },
		"star in prefix":  func(c *BackendConfig) { c.LogGroup = "/ok"; c.LogStreamPrefix = "a*b/" },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testBackendConfig()
			mutate(&cfg)
			if _, err := newBackendWithClients(&fakeMicrovm{}, newFakeSecrets(), cfg, discardLogger()); err == nil {
				t.Fatal("newBackendWithClients: want error on malformed logging config, got nil")
			}
		})
	}
}

// TestLaunchLoggingCarriesNoCredentialMaterial is the redaction guard for the
// logging config itself: the Logging member carries ONLY the configured group
// name and the prefix+beadID stream — never anything derived from the spec's
// environment or credential fields (beads tokens, Claude OAuth, Ziti identity
// JSON, git creds). A credential can therefore only reach CloudWatch if a
// process INSIDE the VM prints one to stdout/stderr; the controller-side launch
// path is provably clean. (The complementary in-VM guarantee — entrypoint, Ziti
// proxy, and agent never echo their secrets — is asserted against live log
// output in the E2E acceptance run.)
func TestLaunchLoggingCarriesNoCredentialMaterial(t *testing.T) {
	mv := &fakeMicrovm{}
	cfg := testBackendConfig()
	cfg.LogGroup = "/aws/lambda/microvms/ziti-microvm-poc"
	b := mustBackend(t, mv, newFakeSecrets(), cfg)

	leaks := []string{
		"beads-secret-token",
		"claude-secret-oauth",
		`{"ztAPI":"https://ziti","id":{"key":"pem:SECRET"}}`,
		"ssh-private-key-pem",
		"nats-secret",
		"git-secret-cred",
	}
	_, err := b.Launch(context.Background(), AgentSpec{
		BeadID: "gd-LOGSEC",
		Environment: map[string]string{
			"BEADS_AUTH_TOKEN":        leaks[0],
			"CLAUDE_CODE_OAUTH_TOKEN": leaks[1],
			"GIT_TOKEN":               leaks[5],
		},
		ClaudeCredentials: leaks[1],
		SSHPrivateKey:     leaks[3],
		NATSToken:         leaks[4],
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	cw, ok := mv.runInput.Logging.(*mvtypes.LoggingMemberCloudWatch)
	if !ok {
		t.Fatalf("Logging = %#v, want *LoggingMemberCloudWatch", mv.runInput.Logging)
	}
	rendered := aws.ToString(cw.Value.LogGroup) + " " + aws.ToString(cw.Value.LogStream)
	for _, leak := range leaks {
		if strings.Contains(rendered, leak) {
			t.Errorf("Logging member leaked credential material %q: %s", leak, rendered)
		}
	}
	if got := aws.ToString(cw.Value.LogStream); got != "agent/gd-LOGSEC" {
		t.Errorf("LogStream = %q, want agent/gd-LOGSEC (bead ID only, nothing spec-derived)", got)
	}
}

// TestBackendLogsCarryNoCredentialMaterial captures the backend's own slog
// output across a full Launch + Terminate of a credential-laden spec and
// asserts no credential value (beads token, Claude OAuth, Ziti identity JSON,
// git/SSH material, NATS token) ever reaches a controller log line. The launch
// info line now includes log_group/log_stream, so this also guards the new
// fields against ever being derived from secret material.
func TestBackendLogsCarryNoCredentialMaterial(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	sm := newFakeSecrets()
	mv := &fakeMicrovm{}
	cfg := testBackendConfig()
	cfg.LogGroup = "/aws/lambda/microvms/ziti-microvm-poc"
	b, err := newBackendWithClients(mv, sm, cfg, logger)
	if err != nil {
		t.Fatalf("newBackendWithClients: %v", err)
	}

	leaks := []string{
		"beads-secret-token",
		"claude-secret-oauth",
		`pem:ZITI-IDENTITY-SECRET`,
		"ssh-private-key-pem",
		"nats-secret",
		"git-secret-cred",
	}
	handle, err := b.Launch(context.Background(), AgentSpec{
		BeadID: "gd-LOGREDACT",
		Environment: map[string]string{
			"BEADS_AUTH_TOKEN":        leaks[0],
			"CLAUDE_CODE_OAUTH_TOKEN": leaks[1],
			"GIT_TOKEN":               leaks[5],
		},
		ClaudeCredentials: leaks[1],
		SSHPrivateKey:     leaks[3],
		NATSToken:         leaks[4],
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if err := b.Terminate(context.Background(), handle, "test teardown"); err != nil {
		t.Fatalf("Terminate: %v", err)
	}

	out := buf.String()
	if out == "" {
		t.Fatal("no controller log output captured; launch/terminate should log")
	}
	for _, leak := range leaks {
		if strings.Contains(out, leak) {
			t.Errorf("controller log output leaked credential material %q:\n%s", leak, out)
		}
	}
}

// --- Launch failure / rollback ---------------------------------------------

// TestLaunchRollbackOnRunFailure: a RunMicrovm failure must delete the per-run
// secret so a failed launch leaves nothing orphaned.
func TestLaunchRollbackOnRunFailure(t *testing.T) {
	sm := newFakeSecrets()
	mv := &fakeMicrovm{runErr: errors.New("AccessDenied: bad launcher")}
	b := mustBackend(t, mv, sm, testBackendConfig())

	_, err := b.Launch(context.Background(), AgentSpec{BeadID: "gd-FAIL1"})
	if err == nil {
		t.Fatal("Launch: want error on RunMicrovm failure, got nil")
	}
	wantName := "ziti-microvm-agents/gd-FAIL1/agent-config"
	arn := sm.created[wantName]
	if arn == "" {
		t.Fatalf("secret %q not created before rollback", wantName)
	}
	if len(sm.deleted) != 1 || sm.deleted[0] != arn {
		t.Errorf("deleted = %v, want rollback of the created secret ARN %q", sm.deleted, arn)
	}
}

// TestLaunchRollbackDeleteFailureStillReturnsLaunchError: if the rollback
// secret-delete ALSO fails, Launch still returns the original launch error (the
// leaked secret is logged, not masked).
func TestLaunchRollbackDeleteFailureStillReturnsLaunchError(t *testing.T) {
	sm := newFakeSecrets()
	sm.deleteErr = errors.New("secrets briefly unavailable")
	runErr := errors.New("AccessDenied: bad launcher")
	mv := &fakeMicrovm{runErr: runErr}
	b := mustBackend(t, mv, sm, testBackendConfig())

	_, err := b.Launch(context.Background(), AgentSpec{BeadID: "gd-ROLLFAIL"})
	if err == nil || !strings.Contains(err.Error(), "RunMicrovm") {
		t.Fatalf("Launch: err = %v, want the RunMicrovm launch error", err)
	}
}

// TestLaunchSecretFailureNoRun: a secret-write failure must abort before
// RunMicrovm (never launch a VM with no credentials).
func TestLaunchSecretFailureNoRun(t *testing.T) {
	sm := newFakeSecrets()
	sm.createErr = func(string) error { return errors.New("boom: secrets down") }
	mv := &fakeMicrovm{}
	b := mustBackend(t, mv, sm, testBackendConfig())

	_, err := b.Launch(context.Background(), AgentSpec{BeadID: "gd-NOSEC"})
	if err == nil {
		t.Fatal("Launch: want error on secret failure, got nil")
	}
	if mv.runCalls != 0 {
		t.Errorf("runCalls = %d, want 0 (no VM launched without a secret)", mv.runCalls)
	}
}

// TestLaunchIdempotentSecretOverwrite: when the secret already exists (a retry
// after a mid-launch failure), CreateSecret's ResourceExists is handled by
// PutSecretValue rather than surfacing an error.
func TestLaunchIdempotentSecretOverwrite(t *testing.T) {
	sm := newFakeSecrets()
	wantName := "ziti-microvm-agents/gd-RETRY/agent-config"
	// Pre-seed as if a prior attempt created it.
	sm.created[wantName] = "arn:aws:secretsmanager:us-east-2:345678901234:secret:" + wantName + "-Zz"
	sm.createErr = func(name string) error {
		if name == wantName {
			return &smtypes.ResourceExistsException{Message: aws.String("exists")}
		}
		return nil
	}
	mv := &fakeMicrovm{}
	b := mustBackend(t, mv, sm, testBackendConfig())

	handle, err := b.Launch(context.Background(), AgentSpec{BeadID: "gd-RETRY"})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if len(sm.putCalls) != 1 || sm.putCalls[0] != wantName {
		t.Errorf("putCalls = %v, want [%s]", sm.putCalls, wantName)
	}
	if handle.AgentSecretARN != sm.created[wantName] {
		t.Errorf("handle.AgentSecretARN = %q, want %q", handle.AgentSecretARN, sm.created[wantName])
	}
}

func TestLaunchRejectsEmptyBeadID(t *testing.T) {
	b := mustBackend(t, &fakeMicrovm{}, newFakeSecrets(), testBackendConfig())
	if _, err := b.Launch(context.Background(), AgentSpec{}); err == nil {
		t.Fatal("Launch with empty BeadID: want error, got nil")
	}
}

// --- Status ----------------------------------------------------------------

func TestStatusMapping(t *testing.T) {
	cases := []struct {
		state        mvtypes.MicrovmState
		wantLive     bool
		wantTerminal bool
	}{
		{mvtypes.MicrovmStatePending, true, false},
		{mvtypes.MicrovmStateRunning, true, false},
		{mvtypes.MicrovmStateSuspending, true, false},
		{mvtypes.MicrovmStateSuspended, true, false},
		{mvtypes.MicrovmStateTerminating, false, true},
		{mvtypes.MicrovmStateTerminated, false, true},
	}
	for _, tc := range cases {
		t.Run(string(tc.state), func(t *testing.T) {
			mv := &fakeMicrovm{getState: tc.state, getReason: "because"}
			b := mustBackend(t, mv, newFakeSecrets(), testBackendConfig())
			st, err := b.Status(context.Background(), RuntimeHandle{MicrovmID: "mvm-1"})
			if err != nil {
				t.Fatalf("Status: %v", err)
			}
			if st.Live != tc.wantLive || st.Terminal != tc.wantTerminal {
				t.Errorf("state %s → Live=%v Terminal=%v, want Live=%v Terminal=%v",
					tc.state, st.Live, st.Terminal, tc.wantLive, tc.wantTerminal)
			}
			if st.State != string(tc.state) {
				t.Errorf("State = %q, want %q", st.State, tc.state)
			}
		})
	}
}

// TestStatusNoMicrovmID: a handle with no MicroVM (launch failed before Run) is
// reported terminal without an AWS call.
func TestStatusNoMicrovmID(t *testing.T) {
	mv := &fakeMicrovm{getErr: errors.New("should not be called")}
	b := mustBackend(t, mv, newFakeSecrets(), testBackendConfig())
	st, err := b.Status(context.Background(), RuntimeHandle{BeadID: "gd-x"})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !st.Terminal || st.Live {
		t.Errorf("no-microvm status = %+v, want Terminal", st)
	}
}

// TestStatusVanishedMicrovm: a ResourceNotFound from GetMicrovm is reported as
// terminal, not an error.
func TestStatusVanishedMicrovm(t *testing.T) {
	mv := &fakeMicrovm{getErr: &mvtypes.ResourceNotFoundException{Message: aws.String("gone")}}
	b := mustBackend(t, mv, newFakeSecrets(), testBackendConfig())
	st, err := b.Status(context.Background(), RuntimeHandle{MicrovmID: "mvm-gone"})
	if err != nil {
		t.Fatalf("Status: unexpected error %v", err)
	}
	if !st.Terminal {
		t.Errorf("vanished status = %+v, want Terminal", st)
	}
}

// --- Terminate -------------------------------------------------------------

// TestTerminateStaticIdentity: Terminate stops the VM and deletes the per-run
// agent secret, but NEVER the shared static POC identity.
func TestTerminateStaticIdentity(t *testing.T) {
	sm := newFakeSecrets()
	mv := &fakeMicrovm{}
	b := mustBackend(t, mv, sm, testBackendConfig())

	agentARN := "arn:aws:secretsmanager:us-east-2:345678901234:secret:ziti-microvm-agents/gd-T1/agent-config-Aa"
	staticARN := testBackendConfig().StaticIdentitySecretARN
	err := b.Terminate(context.Background(), RuntimeHandle{
		BeadID:            "gd-T1",
		MicrovmID:         "mvm-t1",
		AgentSecretARN:    agentARN,
		IdentitySecretARN: staticARN,
		IsPerRunIdentity:  false,
	}, "stop")
	if err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if len(mv.termCalls) != 1 || mv.termCalls[0] != "mvm-t1" {
		t.Errorf("termCalls = %v, want [mvm-t1]", mv.termCalls)
	}
	if len(sm.deleted) != 1 || sm.deleted[0] != agentARN {
		t.Errorf("deleted = %v, want only the per-run agent secret %q", sm.deleted, agentARN)
	}
	for _, d := range sm.deleted {
		if d == staticARN {
			t.Error("Terminate deleted the shared static POC identity; it must be preserved")
		}
	}
}

// TestTerminatePerRunIdentity: when the identity is per-run (future production),
// Terminate revokes it too.
func TestTerminatePerRunIdentity(t *testing.T) {
	sm := newFakeSecrets()
	b := mustBackend(t, &fakeMicrovm{}, sm, testBackendConfig())

	agentARN := "arn:...:agent-config-Aa"
	idARN := "arn:...:per-run-identity-Bb"
	err := b.Terminate(context.Background(), RuntimeHandle{
		BeadID:            "gd-T2",
		MicrovmID:         "mvm-t2",
		AgentSecretARN:    agentARN,
		IdentitySecretARN: idARN,
		IsPerRunIdentity:  true,
	}, "cancel")
	if err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if len(sm.deleted) != 2 {
		t.Fatalf("deleted = %v, want both agent + per-run identity secrets", sm.deleted)
	}
}

// TestTerminateIdempotentMissingVM: TerminateMicrovm returning ResourceNotFound
// is a success (idempotent teardown).
func TestTerminateIdempotentMissingVM(t *testing.T) {
	sm := newFakeSecrets()
	mv := &fakeMicrovm{termErr: &mvtypes.ResourceNotFoundException{Message: aws.String("already gone")}}
	b := mustBackend(t, mv, sm, testBackendConfig())

	err := b.Terminate(context.Background(), RuntimeHandle{
		BeadID: "gd-T3", MicrovmID: "mvm-gone", AgentSecretARN: "arn:x",
	}, "gc")
	if err != nil {
		t.Fatalf("Terminate on missing VM: want nil, got %v", err)
	}
	// Secret cleanup still proceeds.
	if len(sm.deleted) != 1 {
		t.Errorf("deleted = %v, want the agent secret still cleaned up", sm.deleted)
	}
}

// TestTerminateIdempotentMissingSecret: DeleteSecret returning ResourceNotFound
// is a success.
func TestTerminateIdempotentMissingSecret(t *testing.T) {
	sm := newFakeSecrets()
	sm.deleteErr = &smtypes.ResourceNotFoundException{Message: aws.String("no secret")}
	b := mustBackend(t, &fakeMicrovm{}, sm, testBackendConfig())

	err := b.Terminate(context.Background(), RuntimeHandle{
		BeadID: "gd-T4", MicrovmID: "mvm-t4", AgentSecretARN: "arn:gone",
	}, "gc")
	if err != nil {
		t.Fatalf("Terminate on missing secret: want nil, got %v", err)
	}
}

// TestTerminateReturnsRealError: a non-notfound TerminateMicrovm error is
// surfaced (so C5 can retry), while secret cleanup still proceeds.
func TestTerminateReturnsRealError(t *testing.T) {
	sm := newFakeSecrets()
	mv := &fakeMicrovm{termErr: errors.New("throttled: try again")}
	b := mustBackend(t, mv, sm, testBackendConfig())

	err := b.Terminate(context.Background(), RuntimeHandle{
		BeadID: "gd-T5", MicrovmID: "mvm-t5", AgentSecretARN: "arn:sec",
	}, "stop")
	if err == nil || !strings.Contains(err.Error(), "TerminateMicrovm") {
		t.Fatalf("Terminate: err = %v, want the TerminateMicrovm error", err)
	}
	// Secret cleanup must still have run despite the VM-terminate error.
	if len(sm.deleted) != 1 {
		t.Errorf("deleted = %v, want the agent secret still cleaned up", sm.deleted)
	}
}

// TestTerminateNoHandle: an empty handle (nothing launched) is a no-op success.
func TestTerminateNoHandle(t *testing.T) {
	sm := newFakeSecrets()
	mv := &fakeMicrovm{}
	b := mustBackend(t, mv, sm, testBackendConfig())
	if err := b.Terminate(context.Background(), RuntimeHandle{BeadID: "gd-empty"}, "stop"); err != nil {
		t.Fatalf("Terminate empty handle: %v", err)
	}
	if len(mv.termCalls) != 0 || len(sm.deleted) != 0 {
		t.Errorf("empty-handle Terminate touched AWS: term=%v del=%v", mv.termCalls, sm.deleted)
	}
}

// --- Suspend / Resume ------------------------------------------------------

// TestSuspendHappyPath: Suspend calls SuspendMicrovm with the handle's MicroVM
// identifier and returns nil.
func TestSuspendHappyPath(t *testing.T) {
	mv := &fakeMicrovm{}
	b := mustBackend(t, mv, newFakeSecrets(), testBackendConfig())

	if err := b.Suspend(context.Background(), RuntimeHandle{BeadID: "gd-S1", MicrovmID: "mvm-s1"}); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	if len(mv.suspCalls) != 1 || mv.suspCalls[0] != "mvm-s1" {
		t.Errorf("suspCalls = %v, want [mvm-s1]", mv.suspCalls)
	}
}

// TestResumeHappyPath: Resume calls ResumeMicrovm with the handle's MicroVM
// identifier and returns nil.
func TestResumeHappyPath(t *testing.T) {
	mv := &fakeMicrovm{}
	b := mustBackend(t, mv, newFakeSecrets(), testBackendConfig())

	if err := b.Resume(context.Background(), RuntimeHandle{BeadID: "gd-R1", MicrovmID: "mvm-r1"}); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if len(mv.resCalls) != 1 || mv.resCalls[0] != "mvm-r1" {
		t.Errorf("resCalls = %v, want [mvm-r1]", mv.resCalls)
	}
}

// TestSuspendResumeNoHandle: an empty MicrovmID (nothing launched) is a no-op
// success that never touches AWS — mirrors Terminate's empty-handle contract.
func TestSuspendResumeNoHandle(t *testing.T) {
	mv := &fakeMicrovm{}
	b := mustBackend(t, mv, newFakeSecrets(), testBackendConfig())

	if err := b.Suspend(context.Background(), RuntimeHandle{BeadID: "gd-empty"}); err != nil {
		t.Fatalf("Suspend empty handle: %v", err)
	}
	if err := b.Resume(context.Background(), RuntimeHandle{BeadID: "gd-empty"}); err != nil {
		t.Fatalf("Resume empty handle: %v", err)
	}
	if len(mv.suspCalls) != 0 || len(mv.resCalls) != 0 {
		t.Errorf("empty-handle suspend/resume touched AWS: susp=%v res=%v", mv.suspCalls, mv.resCalls)
	}
}

// TestSuspendResumeIdempotent: a not-found VM (ResourceNotFound) or a wrong-state
// VM (Conflict — already suspended/resumed, mid-transition, or terminating) is a
// success, not a hard error, so a duplicate stop/resume event or a reconciler
// race never surfaces as a spurious failure.
func TestSuspendResumeIdempotent(t *testing.T) {
	notFound := &mvtypes.ResourceNotFoundException{Message: aws.String("gone")}
	conflict := &mvtypes.ConflictException{Message: aws.String("wrong state")}
	cases := []struct {
		name string
		err  error
	}{
		{"not found", notFound},
		{"wrong state", conflict},
	}
	for _, tc := range cases {
		t.Run("suspend/"+tc.name, func(t *testing.T) {
			mv := &fakeMicrovm{suspErr: tc.err}
			b := mustBackend(t, mv, newFakeSecrets(), testBackendConfig())
			if err := b.Suspend(context.Background(), RuntimeHandle{BeadID: "gd-S", MicrovmID: "mvm-s"}); err != nil {
				t.Fatalf("Suspend on %s: want nil, got %v", tc.name, err)
			}
		})
		t.Run("resume/"+tc.name, func(t *testing.T) {
			mv := &fakeMicrovm{resErr: tc.err}
			b := mustBackend(t, mv, newFakeSecrets(), testBackendConfig())
			if err := b.Resume(context.Background(), RuntimeHandle{BeadID: "gd-R", MicrovmID: "mvm-r"}); err != nil {
				t.Fatalf("Resume on %s: want nil, got %v", tc.name, err)
			}
		})
	}
}

// TestSuspendResumeReturnsRealError: a non-benign AWS error (e.g. throttling) is
// surfaced so the caller can retry, rather than being swallowed like the
// idempotent not-found/wrong-state cases.
func TestSuspendResumeReturnsRealError(t *testing.T) {
	t.Run("suspend", func(t *testing.T) {
		mv := &fakeMicrovm{suspErr: errors.New("throttled: try again")}
		b := mustBackend(t, mv, newFakeSecrets(), testBackendConfig())
		err := b.Suspend(context.Background(), RuntimeHandle{BeadID: "gd-S", MicrovmID: "mvm-s"})
		if err == nil || !strings.Contains(err.Error(), "SuspendMicrovm") {
			t.Fatalf("Suspend: err = %v, want the SuspendMicrovm error", err)
		}
	})
	t.Run("resume", func(t *testing.T) {
		mv := &fakeMicrovm{resErr: errors.New("throttled: try again")}
		b := mustBackend(t, mv, newFakeSecrets(), testBackendConfig())
		err := b.Resume(context.Background(), RuntimeHandle{BeadID: "gd-R", MicrovmID: "mvm-r"})
		if err == nil || !strings.Contains(err.Error(), "ResumeMicrovm") {
			t.Fatalf("Resume: err = %v, want the ResumeMicrovm error", err)
		}
	})
}

// --- IdlePolicy ------------------------------------------------------------

// TestLaunchIdlePolicyDefaultOff: with no IdlePolicy configured, RunMicrovmInput's
// IdlePolicy member stays nil, preserving the pre-idle-policy launch shape
// byte-for-byte.
func TestLaunchIdlePolicyDefaultOff(t *testing.T) {
	mv := &fakeMicrovm{}
	b := mustBackend(t, mv, newFakeSecrets(), testBackendConfig())

	if _, err := b.Launch(context.Background(), AgentSpec{BeadID: "gd-NOIDLE"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if mv.runInput.IdlePolicy != nil {
		t.Errorf("IdlePolicy = %#v, want nil when unset", mv.runInput.IdlePolicy)
	}
}

// TestLaunchIdlePolicySet: with an IdlePolicy configured, every launch carries it
// with all three SDK-required members. MaxIdleDurationSeconds is raised by the
// backend constructor to exceed the VM lifetime cap (ka-0000002917), so a config
// value below the cap is normalized up; SuspendedDurationSeconds and
// AutoResumeEnabled pass through verbatim.
func TestLaunchIdlePolicySet(t *testing.T) {
	mv := &fakeMicrovm{}
	cfg := testBackendConfig()
	cfg.MaximumDurationSeconds = 3600
	cfg.IdlePolicy = NewIdlePolicy(true, true, 900, 28800)
	b := mustBackend(t, mv, newFakeSecrets(), cfg)

	if _, err := b.Launch(context.Background(), AgentSpec{BeadID: "gd-IDLE1"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	ip := mv.runInput.IdlePolicy
	if ip == nil {
		t.Fatal("IdlePolicy = nil, want it set on the launch")
	}
	if !aws.ToBool(ip.AutoResumeEnabled) {
		t.Errorf("AutoResumeEnabled = %v, want true", aws.ToBool(ip.AutoResumeEnabled))
	}
	// 900 < 3600 lifetime → raised to 3600 + idleLifetimeSlackSeconds so AWS
	// idle-suspend can never fire before the VM self-terminates.
	if want := int32(3600) + idleLifetimeSlackSeconds; aws.ToInt32(ip.MaxIdleDurationSeconds) != want {
		t.Errorf("MaxIdleDurationSeconds = %d, want %d (raised above VM lifetime)", aws.ToInt32(ip.MaxIdleDurationSeconds), want)
	}
	if got := aws.ToInt32(ip.SuspendedDurationSeconds); got != 28800 {
		t.Errorf("SuspendedDurationSeconds = %d, want 28800", got)
	}
}

// TestNormalizeIdlePolicyRaisesAboveVMLifetime is the direct guard for
// ka-0000002917: in the egress-only topology the AWS idle clock counts from boot
// and never resets, so AWS suspends (and the entrypoint hook then SIGTERM-kills)
// a working agent at ~MaxIdleDurationSeconds. normalizeIdlePolicy must raise
// MaxIdleDurationSeconds strictly above the VM's wall-clock lifetime cap so the
// VM self-terminates first and AWS idle-suspend can never fire mid-turn.
func TestNormalizeIdlePolicyRaisesAboveVMLifetime(t *testing.T) {
	// nil policy (feature disabled) is a no-op and must not panic.
	normalizeIdlePolicy(nil, 3600)

	// The exact prod-overlay shape that reaped working VMs: maxIdle=60, VM cap 3600.
	p := NewIdlePolicy(true, false, 60, 900)
	normalizeIdlePolicy(p, 3600)
	if want := int32(3600) + idleLifetimeSlackSeconds; aws.ToInt32(p.MaxIdleDurationSeconds) != want {
		t.Errorf("MaxIdleDurationSeconds = %d, want %d (> VM lifetime 3600)", aws.ToInt32(p.MaxIdleDurationSeconds), want)
	}
	if aws.ToInt32(p.MaxIdleDurationSeconds) <= 3600 {
		t.Errorf("MaxIdleDurationSeconds = %d must strictly exceed VM lifetime 3600", aws.ToInt32(p.MaxIdleDurationSeconds))
	}
	// SuspendedDurationSeconds (the standby backstop that DOES bite) is untouched.
	if got := aws.ToInt32(p.SuspendedDurationSeconds); got != 900 {
		t.Errorf("SuspendedDurationSeconds = %d, want 900 (untouched)", got)
	}

	// An already-large maxIdle (above the cap) is preserved, not lowered.
	big := NewIdlePolicy(true, false, 999999, 900)
	normalizeIdlePolicy(big, 3600)
	if got := aws.ToInt32(big.MaxIdleDurationSeconds); got != 999999 {
		t.Errorf("MaxIdleDurationSeconds = %d, want 999999 preserved (already > cap)", got)
	}

	// Scales with a raised VM lifetime cap (8h ceiling): floor tracks the cap.
	long := NewIdlePolicy(true, false, 60, 900)
	normalizeIdlePolicy(long, 30600) // 8h30m
	if want := int32(30600) + idleLifetimeSlackSeconds; aws.ToInt32(long.MaxIdleDurationSeconds) != want {
		t.Errorf("MaxIdleDurationSeconds = %d, want %d (tracks raised cap)", aws.ToInt32(long.MaxIdleDurationSeconds), want)
	}
}

// TestNewIdlePolicy: disabled returns nil (launch shape preserved); enabled
// returns all three SDK-required members. Guards the config→SDK mapping used by
// the controller wiring so AWS types stay out of main.go.
func TestNewIdlePolicy(t *testing.T) {
	if got := NewIdlePolicy(false, true, 900, 28800); got != nil {
		t.Errorf("NewIdlePolicy(disabled) = %#v, want nil", got)
	}
	got := NewIdlePolicy(true, false, 600, 7200)
	if got == nil {
		t.Fatal("NewIdlePolicy(enabled) = nil, want a policy")
	}
	if aws.ToBool(got.AutoResumeEnabled) {
		t.Errorf("AutoResumeEnabled = true, want false")
	}
	if v := aws.ToInt32(got.MaxIdleDurationSeconds); v != 600 {
		t.Errorf("MaxIdleDurationSeconds = %d, want 600", v)
	}
	if v := aws.ToInt32(got.SuspendedDurationSeconds); v != 7200 {
		t.Errorf("SuspendedDurationSeconds = %d, want 7200", v)
	}
}

// TestNewIdlePolicyFloorsBelowAWSMinimum: AWS RunMicrovm 400-rejects an IdlePolicy
// whose MaxIdleDurationSeconds or SuspendedDurationSeconds is below 60 (ka-0000005755:
// idlePolicy.maxIdleDurationSeconds=0 blocked every lambda_microvm launch in prod).
// NewIdlePolicy must floor both duration members at 60 so a config 0 — historically
// "no idle deadline" in our egress-only topology — never reaches the wire.
func TestNewIdlePolicyFloorsBelowAWSMinimum(t *testing.T) {
	// The exact prod-overlay shape that 400'd: enabled, maxIdle=0, suspended=900.
	got := NewIdlePolicy(true, false, 0, 900)
	if got == nil {
		t.Fatal("NewIdlePolicy(enabled) = nil, want a policy")
	}
	if v := aws.ToInt32(got.MaxIdleDurationSeconds); v != minIdlePolicyDurationSeconds {
		t.Errorf("MaxIdleDurationSeconds = %d, want floored to %d", v, minIdlePolicyDurationSeconds)
	}
	if v := aws.ToInt32(got.SuspendedDurationSeconds); v != 900 {
		t.Errorf("SuspendedDurationSeconds = %d, want 900 (already legal, unchanged)", v)
	}

	// Both below the floor → both clamped up to the minimum.
	both := NewIdlePolicy(true, false, 0, 30)
	if v := aws.ToInt32(both.MaxIdleDurationSeconds); v != minIdlePolicyDurationSeconds {
		t.Errorf("MaxIdleDurationSeconds = %d, want floored to %d", v, minIdlePolicyDurationSeconds)
	}
	if v := aws.ToInt32(both.SuspendedDurationSeconds); v != minIdlePolicyDurationSeconds {
		t.Errorf("SuspendedDurationSeconds = %d, want floored to %d", v, minIdlePolicyDurationSeconds)
	}

	// Exactly the floor is preserved (boundary).
	at := NewIdlePolicy(true, false, minIdlePolicyDurationSeconds, minIdlePolicyDurationSeconds)
	if v := aws.ToInt32(at.MaxIdleDurationSeconds); v != minIdlePolicyDurationSeconds {
		t.Errorf("MaxIdleDurationSeconds = %d, want %d preserved at boundary", v, minIdlePolicyDurationSeconds)
	}
}
