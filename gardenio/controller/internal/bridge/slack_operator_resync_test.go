package bridge

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// fakeIdentityClient is a 2-method fake for slackOperatorIdentityClient.
type fakeIdentityClient struct {
	mu         sync.Mutex
	aliases    []beadsapi.SlackOperatorAlias
	listErr    error
	resyncs    []int64
	updErr     map[int64]error
	missing    map[int64]bool // operator ids the daemon reports as missing
	noUserBead map[int64]bool // operator ids no user bead resolves to
}

func (f *fakeIdentityClient) ListSlackOperatorAliases(_ context.Context) (*beadsapi.ListSlackOperatorAliasesResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	return &beadsapi.ListSlackOperatorAliasesResponse{Aliases: append([]beadsapi.SlackOperatorAlias{}, f.aliases...)}, nil
}

func (f *fakeIdentityClient) ResyncOperatorIdentity(_ context.Context, id int64) (*beadsapi.UpdateOperatorIdentityResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resyncs = append(f.resyncs, id)
	if err := f.updErr[id]; err != nil {
		return nil, err
	}
	if f.noUserBead[id] {
		return &beadsapi.UpdateOperatorIdentityResponse{NoUserBead: true}, nil
	}
	if f.missing[id] {
		return &beadsapi.UpdateOperatorIdentityResponse{Updated: false}, nil
	}
	return &beadsapi.UpdateOperatorIdentityResponse{Updated: true}, nil
}

func newSilentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// Every slack-aliased operator is resynced once, by id only: the bridge sends
// no display or email of its own (gd-iRFGU6xSK7).
func TestRunSlackOperatorResyncCycle_ResyncsEachOperatorOnce(t *testing.T) {
	daemon := &fakeIdentityClient{
		aliases: []beadsapi.SlackOperatorAlias{
			{OperatorID: 1, Subject: "U1", Display: "Old Name", Email: "old@example.com"},
			{OperatorID: 1, Subject: "U1B"},
			{OperatorID: 2, Subject: "U2"},
			{OperatorID: 3, Subject: "U3"},
		},
		noUserBead: map[int64]bool{3: true},
	}
	b := &Bot{logger: newSilentLogger()}
	b.runSlackOperatorResyncCycle(context.Background(), daemon, b.logger)

	want := []int64{1, 2, 3}
	if len(daemon.resyncs) != len(want) {
		t.Fatalf("resyncs = %v, want %v", daemon.resyncs, want)
	}
	for i, id := range want {
		if daemon.resyncs[i] != id {
			t.Errorf("resyncs[%d] = %d, want %d", i, daemon.resyncs[i], id)
		}
	}
}

func TestRunSlackOperatorResyncCycle_ErrorContinues(t *testing.T) {
	daemon := &fakeIdentityClient{
		aliases: []beadsapi.SlackOperatorAlias{
			{OperatorID: 1, Subject: "U1"},
			{OperatorID: 2, Subject: "U2"},
		},
		updErr:  map[int64]error{1: errors.New("daemon 500")},
		missing: map[int64]bool{2: true},
	}
	b := &Bot{logger: newSilentLogger()}
	b.runSlackOperatorResyncCycle(context.Background(), daemon, b.logger)

	if len(daemon.resyncs) != 2 {
		t.Errorf("expected both operators attempted, got %v", daemon.resyncs)
	}
}

func TestRunSlackOperatorResyncCycle_ListErrorReturnsCleanly(t *testing.T) {
	daemon := &fakeIdentityClient{listErr: errors.New("daemon down")}
	b := &Bot{logger: newSilentLogger()}
	b.runSlackOperatorResyncCycle(context.Background(), daemon, b.logger)

	if len(daemon.resyncs) != 0 {
		t.Errorf("expected zero resyncs when list fails, got %d", len(daemon.resyncs))
	}
}

func TestRunSlackOperatorResyncCycle_CtxCanceledAborts(t *testing.T) {
	daemon := &fakeIdentityClient{
		aliases: []beadsapi.SlackOperatorAlias{{OperatorID: 1, Subject: "U1"}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b := &Bot{logger: newSilentLogger()}
	b.runSlackOperatorResyncCycle(ctx, daemon, b.logger)

	if len(daemon.resyncs) != 0 {
		t.Errorf("expected zero resyncs after cancel, got %d", len(daemon.resyncs))
	}
}
