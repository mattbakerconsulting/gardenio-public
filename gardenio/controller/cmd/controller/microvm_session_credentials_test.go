package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"gardenio/controller/internal/config"
	"gardenio/controller/internal/microvm"
)

// fakeSessionVendor is a test double for the mint seam. The nil-interface
// discipline (handleNonK8sSpawn guards on the concrete pointer) is exercised
// separately by passing a nil sessionVendor to resolveMicrovmSessionCredentials.
type fakeSessionVendor struct {
	creds  *microvm.SessionCredentials
	err    error
	region string
	calls  int
}

func (f *fakeSessionVendor) Vend(_ context.Context, _ string) (*microvm.SessionCredentials, error) {
	f.calls++
	return f.creds, f.err
}

func (f *fakeSessionVendor) Region() string { return f.region }

func TestMicrovmSessionVendorOptions(t *testing.T) {
	// COOP_S3_REGION wins when set.
	cfg := &config.Config{
		MicrovmSessionVendorRoleARN:             "arn:aws:iam::1:role/v",
		CoopS3Bucket:                            "gardenio-sessions-prod",
		CoopS3Prefix:                            "coop/sessions",
		CoopS3Region:                            "us-east-1",
		MicrovmRegion:                           "us-east-2",
		MicrovmSessionCredentialDurationSeconds: 43200,
	}
	opts := microvmSessionVendorOptions(cfg)
	if opts.Region != "us-east-1" {
		t.Errorf("Region = %q, want us-east-1 (COOP_S3_REGION wins)", opts.Region)
	}
	if !opts.Enabled() {
		t.Error("options should be Enabled with role+region+bucket set")
	}
	if opts.DurationSeconds != 43200 {
		t.Errorf("DurationSeconds = %d, want 43200", opts.DurationSeconds)
	}

	// Falls back to the MicroVM region when COOP_S3_REGION is unset.
	cfg.CoopS3Region = ""
	if got := microvmSessionVendorOptions(cfg).Region; got != "us-east-2" {
		t.Errorf("Region fallback = %q, want us-east-2 (MicrovmRegion)", got)
	}

	// No vendor role → disabled.
	cfg.MicrovmSessionVendorRoleARN = ""
	if microvmSessionVendorOptions(cfg).Enabled() {
		t.Error("options must be disabled with no vendor role")
	}
}

func TestResolveMicrovmSessionCredentialsNilVendor(t *testing.T) {
	// A nil vendor (vending disabled) yields nil without panicking.
	if got := resolveMicrovmSessionCredentials(context.Background(), discardLogger(), nil, "gd-abc"); got != nil {
		t.Errorf("nil vendor: want nil creds, got %+v", got)
	}
}

func TestResolveMicrovmSessionCredentialsMintFailureDegrades(t *testing.T) {
	vendor := &fakeSessionVendor{err: errors.New("access denied")}
	got := resolveMicrovmSessionCredentials(context.Background(), discardLogger(), vendor, "gd-abc")
	if got != nil {
		t.Errorf("mint failure: want nil creds (graceful degradation), got %+v", got)
	}
	if vendor.calls != 1 {
		t.Errorf("Vend calls = %d, want 1", vendor.calls)
	}
}

func TestResolveMicrovmSessionCredentialsSuccess(t *testing.T) {
	want := &microvm.SessionCredentials{
		AccessKeyID:     "ASIA",
		SecretAccessKey: "sk",
		SessionToken:    "st",
		Expiration:      time.Now().Add(12 * time.Hour),
	}
	vendor := &fakeSessionVendor{creds: want}
	got := resolveMicrovmSessionCredentials(context.Background(), discardLogger(), vendor, "gd-abc")
	if got != want {
		t.Errorf("resolve: got %+v, want %+v", got, want)
	}
}

func TestApplyMicrovmSessionCredentials(t *testing.T) {
	spec := &microvm.AgentSpec{}
	creds := &microvm.SessionCredentials{
		AccessKeyID:     "ASIAEXAMPLE",
		SecretAccessKey: "secretkey",
		SessionToken:    "sessiontoken",
	}
	applyMicrovmSessionCredentials(spec, creds, "us-east-1")

	if spec.Environment["AWS_ACCESS_KEY_ID"] != "ASIAEXAMPLE" {
		t.Errorf("AWS_ACCESS_KEY_ID = %q", spec.Environment["AWS_ACCESS_KEY_ID"])
	}
	if spec.Environment["AWS_SECRET_ACCESS_KEY"] != "secretkey" {
		t.Errorf("AWS_SECRET_ACCESS_KEY = %q", spec.Environment["AWS_SECRET_ACCESS_KEY"])
	}
	if spec.Environment["AWS_SESSION_TOKEN"] != "sessiontoken" {
		t.Errorf("AWS_SESSION_TOKEN = %q", spec.Environment["AWS_SESSION_TOKEN"])
	}
	if spec.Environment["COOP_S3_REGION"] != "us-east-1" {
		t.Errorf("COOP_S3_REGION = %q, want us-east-1", spec.Environment["COOP_S3_REGION"])
	}
}

func TestApplyMicrovmSessionCredentialsNilNoop(t *testing.T) {
	spec := &microvm.AgentSpec{Environment: map[string]string{"KEEP": "1"}}
	applyMicrovmSessionCredentials(spec, nil, "us-east-1")
	if _, ok := spec.Environment["AWS_ACCESS_KEY_ID"]; ok {
		t.Error("nil creds must set no AWS_ env keys")
	}
	if spec.Environment["KEEP"] != "1" {
		t.Error("nil creds must not disturb existing env")
	}
}

func TestApplyMicrovmSessionCredentialsPreservesExistingRegion(t *testing.T) {
	// A COOP_S3_REGION already forwarded from the pod spec must NOT be clobbered.
	spec := &microvm.AgentSpec{Environment: map[string]string{"COOP_S3_REGION": "eu-west-1"}}
	applyMicrovmSessionCredentials(spec, &microvm.SessionCredentials{
		AccessKeyID: "a", SecretAccessKey: "b", SessionToken: "c",
	}, "us-east-1")
	if spec.Environment["COOP_S3_REGION"] != "eu-west-1" {
		t.Errorf("COOP_S3_REGION = %q, want the pre-existing eu-west-1 (not clobbered)", spec.Environment["COOP_S3_REGION"])
	}
}

func TestApplyMicrovmSessionCredentialsEmptyRegion(t *testing.T) {
	// An empty vendor region stamps no COOP_S3_REGION at all.
	spec := &microvm.AgentSpec{}
	applyMicrovmSessionCredentials(spec, &microvm.SessionCredentials{
		AccessKeyID: "a", SecretAccessKey: "b", SessionToken: "c",
	}, "")
	if _, ok := spec.Environment["COOP_S3_REGION"]; ok {
		t.Error("empty region must not stamp COOP_S3_REGION")
	}
}
