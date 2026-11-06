package microvm

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	ststypes "github.com/aws/aws-sdk-go-v2/service/sts/types"
)

func TestSessionVendorOptionsEnabled(t *testing.T) {
	full := SessionVendorOptions{
		RoleARN:    "arn:aws:iam::123456789012:role/gardenio-prod-microvm-session-vendor",
		Region:     "us-east-1",
		BucketName: "gardenio-sessions-prod",
	}
	cases := []struct {
		name string
		opts SessionVendorOptions
		want bool
	}{
		{"all set", full, true},
		{"no role", SessionVendorOptions{Region: "us-east-1", BucketName: "b"}, false},
		{"no region", SessionVendorOptions{RoleARN: "arn:x", BucketName: "b"}, false},
		{"no bucket", SessionVendorOptions{RoleARN: "arn:x", Region: "us-east-1"}, false},
		{"whitespace role", SessionVendorOptions{RoleARN: "  ", Region: "us-east-1", BucketName: "b"}, false},
		{"zero", SessionVendorOptions{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.opts.Enabled(); got != tc.want {
				t.Errorf("Enabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNewSessionVendorDisabledReturnsNil(t *testing.T) {
	// A disabled options set must yield (nil, nil) — vending simply off, never
	// an error, and never a token-file lookup.
	v, err := NewSessionVendor(context.Background(), SessionVendorOptions{})
	if err != nil {
		t.Fatalf("NewSessionVendor(disabled): unexpected error %v", err)
	}
	if v != nil {
		t.Fatalf("NewSessionVendor(disabled): want nil vendor, got %+v", v)
	}
}

func TestClampSessionDuration(t *testing.T) {
	cases := []struct {
		in   int32
		want int32
	}{
		{0, defaultSessionCredentialDurationSeconds},
		{-5, defaultSessionCredentialDurationSeconds},
		{100, vendorMinSessionSeconds},   // below STS floor → floor
		{900, 900},                       // exactly the floor
		{3600, 3600},                     // in range
		{43200, 43200},                   // exactly the ceiling
		{99999, vendorMaxSessionSeconds}, // above role max → clamp down
	}
	for _, tc := range cases {
		if got := clampSessionDuration(tc.in); got != tc.want {
			t.Errorf("clampSessionDuration(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestSessionNameForBead(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"ka-0000006612", "gardenio-mvm-sess-ka-0000006612"},
		{"gd-abc.def@x", "gardenio-mvm-sess-gd-abc.def@x"}, // all in STS charset
		{"weird/id with spaces", "gardenio-mvm-sess-weird-id-with-spaces"},
	}
	for _, tc := range cases {
		got := sessionNameForBead(tc.in)
		if got != tc.want {
			t.Errorf("sessionNameForBead(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if len(got) > 64 {
			t.Errorf("sessionNameForBead(%q) length %d exceeds STS max 64", tc.in, len(got))
		}
	}

	// A pathologically long bead ID is truncated to STS's 64-char ceiling.
	long := strings.Repeat("a", 200)
	if got := sessionNameForBead(long); len(got) != 64 {
		t.Errorf("sessionNameForBead(long) length = %d, want 64", len(got))
	}
}

// TestBeadSessionPolicyScoping is the security-critical test: the inline policy
// must scope object access to EXACTLY the bead's own prefix, ListBucket to that
// prefix, and never widen to another bead or the bucket at large.
func TestBeadSessionPolicyScoping(t *testing.T) {
	raw, err := beadSessionPolicy("gardenio-sessions-prod", "coop/sessions", "ka-0000006612")
	if err != nil {
		t.Fatalf("beadSessionPolicy: %v", err)
	}

	var policy struct {
		Version   string `json:"Version"`
		Statement []struct {
			Sid       string         `json:"Sid"`
			Effect    string         `json:"Effect"`
			Action    []string       `json:"Action"`
			Resource  string         `json:"Resource"`
			Condition map[string]any `json:"Condition"`
		} `json:"Statement"`
	}
	if err := json.Unmarshal([]byte(raw), &policy); err != nil {
		t.Fatalf("policy is not valid JSON: %v\n%s", err, raw)
	}
	if policy.Version != "2012-10-17" {
		t.Errorf("policy Version = %q, want 2012-10-17", policy.Version)
	}
	if len(policy.Statement) != 3 {
		t.Fatalf("want 3 statements, got %d", len(policy.Statement))
	}

	byS := map[string]int{}
	for i, s := range policy.Statement {
		byS[s.Sid] = i
		if s.Effect != "Allow" {
			t.Errorf("statement %s: Effect = %q, want Allow", s.Sid, s.Effect)
		}
	}

	obj := policy.Statement[byS["BeadSessionObjects"]]
	wantObjResource := "arn:aws:s3:::gardenio-sessions-prod/coop/sessions/ka-0000006612/*"
	if obj.Resource != wantObjResource {
		t.Errorf("object Resource = %q, want %q", obj.Resource, wantObjResource)
	}
	// Must NOT grant a wildcard over the whole sessions prefix or another bead.
	if strings.Contains(obj.Resource, "coop/sessions/*") || strings.HasSuffix(obj.Resource, "coop/sessions/gd-other/*") {
		t.Errorf("object Resource is over-broad: %q", obj.Resource)
	}
	for _, want := range []string{"s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:AbortMultipartUpload"} {
		if !slices.Contains(obj.Action, want) {
			t.Errorf("object actions missing %s: %v", want, obj.Action)
		}
	}

	list := policy.Statement[byS["BeadSessionList"]]
	if list.Resource != "arn:aws:s3:::gardenio-sessions-prod" {
		t.Errorf("list Resource = %q, want the bucket arn", list.Resource)
	}
	sl, _ := list.Condition["StringLike"].(map[string]any)
	prefixes, _ := sl["s3:prefix"].([]any)
	if len(prefixes) != 1 || prefixes[0] != "coop/sessions/ka-0000006612/*" {
		t.Errorf("list prefix condition = %v, want [coop/sessions/ka-0000006612/*]", prefixes)
	}

	kms := policy.Statement[byS["BeadSessionKMS"]]
	if kms.Resource != "*" {
		t.Errorf("kms Resource = %q, want * (intersected with the role's ViaService CMK)", kms.Resource)
	}
}

// TestBeadSessionPolicyPrefixNormalization proves a prefix passed with a
// trailing slash still produces a clean, single-slash key path (the vendor
// normalizes the prefix at construction, but the policy builder is also fed the
// normalized value — this guards the joined shape).
func TestBeadSessionPolicyDefaultPrefix(t *testing.T) {
	v := newSessionVendorWithClient(&fakeAssumeRole{}, "tok", SessionVendorOptions{
		RoleARN:    "arn:aws:iam::1:role/v",
		Region:     "us-east-1",
		BucketName: "b",
		// SessionPrefix omitted → coop default.
	})
	if v.sessionPrefix != defaultSessionPrefix {
		t.Errorf("sessionPrefix = %q, want %q", v.sessionPrefix, defaultSessionPrefix)
	}

	v2 := newSessionVendorWithClient(&fakeAssumeRole{}, "tok", SessionVendorOptions{
		RoleARN:       "arn:aws:iam::1:role/v",
		Region:        "us-east-1",
		BucketName:    "b",
		SessionPrefix: "/custom/prefix/",
	})
	if v2.sessionPrefix != "custom/prefix" {
		t.Errorf("sessionPrefix = %q, want custom/prefix (slashes trimmed)", v2.sessionPrefix)
	}
}

// fakeAssumeRole is a test double for the one STS call the vendor makes.
type fakeAssumeRole struct {
	gotInput *sts.AssumeRoleWithWebIdentityInput
	out      *sts.AssumeRoleWithWebIdentityOutput
	err      error
}

func (f *fakeAssumeRole) AssumeRoleWithWebIdentity(_ context.Context, in *sts.AssumeRoleWithWebIdentityInput, _ ...func(*sts.Options)) (*sts.AssumeRoleWithWebIdentityOutput, error) {
	f.gotInput = in
	return f.out, f.err
}

func writeTempToken(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write temp token: %v", err)
	}
	return path
}

func TestVendSuccess(t *testing.T) {
	expiry := time.Now().Add(12 * time.Hour).UTC().Truncate(time.Second)
	fake := &fakeAssumeRole{
		out: &sts.AssumeRoleWithWebIdentityOutput{
			Credentials: &ststypes.Credentials{
				AccessKeyId:     aws.String("ASIAEXAMPLE"),
				SecretAccessKey: aws.String("secretkey"),
				SessionToken:    aws.String("sessiontoken"),
				Expiration:      aws.Time(expiry),
			},
		},
	}
	tokenFile := writeTempToken(t, "  header.payload.sig  \n")
	v := newSessionVendorWithClient(fake, tokenFile, SessionVendorOptions{
		RoleARN:         "arn:aws:iam::123456789012:role/gardenio-prod-microvm-session-vendor",
		Region:          "us-east-1",
		BucketName:      "gardenio-sessions-prod",
		SessionPrefix:   "coop/sessions",
		DurationSeconds: 0, // → 12h default
	})

	creds, err := v.Vend(context.Background(), "ka-0000006612")
	if err != nil {
		t.Fatalf("Vend: %v", err)
	}
	if creds.AccessKeyID != "ASIAEXAMPLE" || creds.SecretAccessKey != "secretkey" || creds.SessionToken != "sessiontoken" {
		t.Errorf("credentials not passed through: %+v", creds)
	}
	if !creds.Expiration.Equal(expiry) {
		t.Errorf("Expiration = %v, want %v", creds.Expiration, expiry)
	}

	in := fake.gotInput
	if in == nil {
		t.Fatal("AssumeRoleWithWebIdentity was not called")
	}
	if aws.ToString(in.RoleArn) != "arn:aws:iam::123456789012:role/gardenio-prod-microvm-session-vendor" {
		t.Errorf("RoleArn = %q", aws.ToString(in.RoleArn))
	}
	// The token file must be read fresh and trimmed of whitespace.
	if aws.ToString(in.WebIdentityToken) != "header.payload.sig" {
		t.Errorf("WebIdentityToken = %q, want trimmed token", aws.ToString(in.WebIdentityToken))
	}
	if aws.ToInt32(in.DurationSeconds) != defaultSessionCredentialDurationSeconds {
		t.Errorf("DurationSeconds = %d, want %d", aws.ToInt32(in.DurationSeconds), defaultSessionCredentialDurationSeconds)
	}
	if !strings.Contains(aws.ToString(in.RoleSessionName), "ka-0000006612") {
		t.Errorf("RoleSessionName = %q, want it to carry the bead id", aws.ToString(in.RoleSessionName))
	}
	// The inline session policy must scope to this bead.
	if !strings.Contains(aws.ToString(in.Policy), "coop/sessions/ka-0000006612/*") {
		t.Errorf("Policy did not scope to the bead prefix: %s", aws.ToString(in.Policy))
	}
}

func TestVendRejectsEmptyBead(t *testing.T) {
	tokenFile := writeTempToken(t, "tok")
	v := newSessionVendorWithClient(&fakeAssumeRole{}, tokenFile, SessionVendorOptions{
		RoleARN: "arn:x", Region: "us-east-1", BucketName: "b",
	})
	if _, err := v.Vend(context.Background(), "  "); err == nil {
		t.Fatal("Vend with empty bead: want error, got nil")
	}
}

func TestVendMissingTokenFile(t *testing.T) {
	v := newSessionVendorWithClient(&fakeAssumeRole{}, "/nonexistent/token/path", SessionVendorOptions{
		RoleARN: "arn:x", Region: "us-east-1", BucketName: "b",
	})
	if _, err := v.Vend(context.Background(), "gd-abc"); err == nil {
		t.Fatal("Vend with missing token file: want error, got nil")
	}
}

func TestVendEmptyTokenFile(t *testing.T) {
	tokenFile := writeTempToken(t, "   \n  ")
	v := newSessionVendorWithClient(&fakeAssumeRole{}, tokenFile, SessionVendorOptions{
		RoleARN: "arn:x", Region: "us-east-1", BucketName: "b",
	})
	if _, err := v.Vend(context.Background(), "gd-abc"); err == nil {
		t.Fatal("Vend with empty token file: want error, got nil")
	}
}

func TestVendPropagatesSTSError(t *testing.T) {
	tokenFile := writeTempToken(t, "tok")
	v := newSessionVendorWithClient(&fakeAssumeRole{err: errors.New("access denied")}, tokenFile, SessionVendorOptions{
		RoleARN: "arn:x", Region: "us-east-1", BucketName: "b",
	})
	_, err := v.Vend(context.Background(), "gd-abc")
	if err == nil || !strings.Contains(err.Error(), "access denied") {
		t.Fatalf("Vend: want wrapped STS error, got %v", err)
	}
}

func TestVendRejectsIncompleteCredentials(t *testing.T) {
	tokenFile := writeTempToken(t, "tok")
	// SessionToken missing → treated as incomplete, not silently delivered.
	v := newSessionVendorWithClient(&fakeAssumeRole{
		out: &sts.AssumeRoleWithWebIdentityOutput{
			Credentials: &ststypes.Credentials{
				AccessKeyId:     aws.String("AKIA"),
				SecretAccessKey: aws.String("sk"),
			},
		},
	}, tokenFile, SessionVendorOptions{RoleARN: "arn:x", Region: "us-east-1", BucketName: "b"})
	if _, err := v.Vend(context.Background(), "gd-abc"); err == nil {
		t.Fatal("Vend with incomplete creds: want error, got nil")
	}
}
