package microvm

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	mvtypes "github.com/aws/aws-sdk-go-v2/service/lambdamicrovms/types"
	smtypes "github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
)

// TestStatusExistsAndStartedAt covers the C5 additions to Status: a live runtime
// reports Exists=true and carries its StartedAt; a vanished / no-VM handle
// reports Exists=false.
func TestStatusExistsAndStartedAt(t *testing.T) {
	start := time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC)
	mv := &fakeMicrovm{getState: mvtypes.MicrovmStateRunning, getStart: aws.Time(start)}
	b := mustBackend(t, mv, newFakeSecrets(), testBackendConfig())

	st, err := b.Status(context.Background(), RuntimeHandle{MicrovmID: "mvm-1"})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !st.Exists {
		t.Error("live runtime should report Exists=true")
	}
	if !st.StartedAt.Equal(start) {
		t.Errorf("StartedAt = %v, want %v", st.StartedAt, start)
	}

	// Vanished → Exists=false.
	mvGone := &fakeMicrovm{getErr: &mvtypes.ResourceNotFoundException{Message: aws.String("gone")}}
	bGone := mustBackend(t, mvGone, newFakeSecrets(), testBackendConfig())
	stGone, err := bGone.Status(context.Background(), RuntimeHandle{MicrovmID: "mvm-gone"})
	if err != nil {
		t.Fatalf("Status(gone): %v", err)
	}
	if stGone.Exists {
		t.Error("vanished runtime should report Exists=false")
	}

	// No MicroVM id → Exists=false, no AWS call.
	stNone, err := b.Status(context.Background(), RuntimeHandle{BeadID: "gd-x"})
	if err != nil {
		t.Fatalf("Status(no id): %v", err)
	}
	if stNone.Exists {
		t.Error("no-microvm handle should report Exists=false")
	}
}

// TestListRuntimes enumerates MicroVMs, mapping live/terminal + start.
func TestListRuntimes(t *testing.T) {
	start := time.Date(2026, 7, 17, 8, 0, 0, 0, time.UTC)
	mv := &fakeMicrovm{listItems: []mvtypes.MicrovmItem{
		{MicrovmId: aws.String("mvm-a"), State: mvtypes.MicrovmStateRunning, StartedAt: aws.Time(start)},
		{MicrovmId: aws.String("mvm-b"), State: mvtypes.MicrovmStateTerminated, StartedAt: aws.Time(start)},
	}}
	b := mustBackend(t, mv, newFakeSecrets(), testBackendConfig())
	rts, err := b.ListRuntimes(context.Background())
	if err != nil {
		t.Fatalf("ListRuntimes: %v", err)
	}
	if len(rts) != 2 {
		t.Fatalf("got %d runtimes, want 2", len(rts))
	}
	if !rts[0].Live || rts[1].Live {
		t.Errorf("expected mvm-a Live, mvm-b not Live; got %+v", rts)
	}
	// The image filter is passed so GC only ever considers the approved image.
	if len(mv.listInputs) == 0 || aws.ToString(mv.listInputs[0].ImageIdentifier) != testBackendConfig().ImageIdentifier {
		t.Errorf("ListMicrovms should be scoped to the approved image, got %+v", mv.listInputs)
	}
}

// TestListPerRunSecrets enumerates per-run secrets and reads the owning bead tag.
func TestListPerRunSecrets(t *testing.T) {
	sm := newFakeSecrets()
	sm.listSecrets = []smtypes.SecretListEntry{
		{Name: aws.String("ziti-microvm-agents/gd-1/agent-config"), ARN: aws.String("arn:1"),
			Tags: []smtypes.Tag{{Key: aws.String(tagResourceBead), Value: aws.String("gd-1")}}},
		{Name: aws.String("ziti-microvm-agents/gd-2/agent-config"), ARN: aws.String("arn:2")},
		// Outside the prefix — the belt-and-braces re-check drops it.
		{Name: aws.String("other/thing"), ARN: aws.String("arn:x")},
	}
	b := mustBackend(t, &fakeMicrovm{}, sm, testBackendConfig())
	secs, err := b.ListPerRunSecrets(context.Background())
	if err != nil {
		t.Fatalf("ListPerRunSecrets: %v", err)
	}
	if len(secs) != 2 {
		t.Fatalf("got %d secrets, want 2 (prefix-filtered), %+v", len(secs), secs)
	}
	if secs[0].BeadID != "gd-1" {
		t.Errorf("secret 0 BeadID = %q, want gd-1", secs[0].BeadID)
	}
	if secs[1].BeadID != "" {
		t.Errorf("untagged secret should have empty BeadID, got %q", secs[1].BeadID)
	}
	// The server-side name filter must be set to the prefix.
	if len(sm.listInputs) == 0 || len(sm.listInputs[0].Filters) == 0 {
		t.Fatal("ListSecrets should carry a name-prefix filter")
	}
	if sm.listInputs[0].Filters[0].Values[0] != testBackendConfig().SecretPrefix {
		t.Errorf("ListSecrets filter = %v, want prefix %q", sm.listInputs[0].Filters[0].Values, testBackendConfig().SecretPrefix)
	}
}

// TestLauncherAccount round-trips the account recorded from the verified caller.
func TestLauncherAccount(t *testing.T) {
	b := mustBackend(t, &fakeMicrovm{}, newFakeSecrets(), testBackendConfig())
	if b.LauncherAccount() != "" {
		t.Errorf("fresh backend LauncherAccount = %q, want empty", b.LauncherAccount())
	}
	b.SetLauncherAccount("345678901234")
	if b.LauncherAccount() != "345678901234" {
		t.Errorf("LauncherAccount = %q, want 345678901234", b.LauncherAccount())
	}
	if b.Region() != "us-east-2" {
		t.Errorf("Region = %q, want us-east-2", b.Region())
	}
}
