package main

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"gardenio/controller/internal/beadsapi"
)

// fakeJanitorDaemon satisfies janitorDaemon for tests: it maps a bead id to a
// canned BeadDetail and records the fleet names it was asked to revoke.
type fakeJanitorDaemon struct {
	beads   map[string]*beadsapi.BeadDetail
	getErr  map[string]error
	revoked []string
}

func (f *fakeJanitorDaemon) GetBead(_ context.Context, beadID string) (*beadsapi.BeadDetail, error) {
	if err := f.getErr[beadID]; err != nil {
		return nil, err
	}
	return f.beads[beadID], nil
}

func (f *fakeJanitorDaemon) RevokeBridgeToken(_ context.Context, fleet string) error {
	f.revoked = append(f.revoked, fleet)
	return nil
}

// agentTokenSecret builds a per-agent token Secret as provisionAgentToken would.
func agentTokenSecret(beadID, namespace string, withBeadLabel bool) *corev1.Secret {
	labels := map[string]string{
		"gardenio.io/type":     "agent-token",
		"gardenio.io/provider": tokenProviderBeads,
	}
	if withBeadLabel {
		labels["gardenio.io/bead"] = beadID
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      agentTokenSecretName(beadID),
			Namespace: namespace,
			Labels:    labels,
		},
	}
}

func TestTokenJanitor_RevokesClosedBeadOrphan(t *testing.T) {
	const ns = "gardenio-system"
	closedAt := time.Now().Add(-3 * time.Minute).UTC().Format(time.RFC3339)

	k8s := fake.NewSimpleClientset(
		agentTokenSecret("gd-closed", ns, true),
		agentTokenSecret("gd-open", ns, true),
	)
	daemon := &fakeJanitorDaemon{
		beads: map[string]*beadsapi.BeadDetail{
			"gd-closed": {ID: "gd-closed", Status: "closed", ClosedAt: closedAt},
			"gd-open":   {ID: "gd-open", Status: "in_progress"},
		},
	}
	tm := newTokenMetrics()
	j := newTokenJanitor(daemon, k8s, ns, discardLogger(), tm)

	stats := j.sweep(context.Background())
	if stats.Scanned != 2 {
		t.Errorf("scanned = %d, want 2", stats.Scanned)
	}
	if stats.Revoked != 1 {
		t.Errorf("revoked = %d, want 1", stats.Revoked)
	}
	if len(daemon.revoked) != 1 || daemon.revoked[0] != agentTokenBridgeName("gd-closed") {
		t.Errorf("revoked fleets = %v, want [%s]", daemon.revoked, agentTokenBridgeName("gd-closed"))
	}

	// The closed orphan's Secret must be deleted; the open one must survive.
	if _, err := k8s.CoreV1().Secrets(ns).Get(context.Background(), agentTokenSecretName("gd-closed"), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("expected gd-closed secret deleted, got err=%v", err)
	}
	if _, err := k8s.CoreV1().Secrets(ns).Get(context.Background(), agentTokenSecretName("gd-open"), metav1.GetOptions{}); err != nil {
		t.Errorf("expected gd-open secret to survive, got err=%v", err)
	}
}

func TestTokenJanitor_SkipsSecretWithoutBeadLabel(t *testing.T) {
	const ns = "gardenio-system"
	k8s := fake.NewSimpleClientset(agentTokenSecret("gd-nolabel", ns, false))
	daemon := &fakeJanitorDaemon{beads: map[string]*beadsapi.BeadDetail{}}
	j := newTokenJanitor(daemon, k8s, ns, discardLogger(), newTokenMetrics())

	stats := j.sweep(context.Background())
	if stats.Scanned != 0 || stats.Revoked != 0 {
		t.Errorf("pre-label secret must be skipped: scanned=%d revoked=%d", stats.Scanned, stats.Revoked)
	}
	if len(daemon.revoked) != 0 {
		t.Errorf("must not revoke a bead-less secret, got %v", daemon.revoked)
	}
}

func TestTokenJanitor_SkipsOnBeadFetchError(t *testing.T) {
	const ns = "gardenio-system"
	k8s := fake.NewSimpleClientset(agentTokenSecret("gd-err", ns, true))
	daemon := &fakeJanitorDaemon{
		beads:  map[string]*beadsapi.BeadDetail{},
		getErr: map[string]error{"gd-err": errors.New("daemon down")},
	}
	j := newTokenJanitor(daemon, k8s, ns, discardLogger(), newTokenMetrics())

	stats := j.sweep(context.Background())
	if stats.Revoked != 0 {
		t.Errorf("a fetch error must not revoke: revoked=%d", stats.Revoked)
	}
	// Secret must survive so the next sweep retries.
	if _, err := k8s.CoreV1().Secrets(ns).Get(context.Background(), agentTokenSecretName("gd-err"), metav1.GetOptions{}); err != nil {
		t.Errorf("expected gd-err secret to survive fetch error, got err=%v", err)
	}
}

func TestTokenJanitor_NilSafe(t *testing.T) {
	var j *tokenJanitor
	if got := j.sweep(context.Background()); got.Scanned != 0 || got.Revoked != 0 {
		t.Errorf("nil janitor sweep must be a no-op, got %+v", got)
	}
	// A janitor with a nil daemon is also a no-op.
	j2 := newTokenJanitor(nil, fake.NewSimpleClientset(), "ns", discardLogger(), nil)
	if got := j2.sweep(context.Background()); got.Scanned != 0 || got.Revoked != 0 {
		t.Errorf("nil-daemon janitor sweep must be a no-op, got %+v", got)
	}
}

func TestParseBeadCloseTime(t *testing.T) {
	if !parseBeadCloseTime("").IsZero() {
		t.Error("empty closed_at must yield zero time")
	}
	if !parseBeadCloseTime("not-a-time").IsZero() {
		t.Error("unparseable closed_at must yield zero time")
	}
	ts := "2026-05-29T18:30:25Z"
	got := parseBeadCloseTime(ts)
	if got.IsZero() {
		t.Fatalf("valid RFC3339 %q parsed to zero", ts)
	}
	if want, _ := time.Parse(time.RFC3339, ts); !got.Equal(want) {
		t.Errorf("parseBeadCloseTime(%q) = %v, want %v", ts, got, want)
	}
}
