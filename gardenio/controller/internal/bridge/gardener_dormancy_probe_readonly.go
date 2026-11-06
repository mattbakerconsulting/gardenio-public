package bridge

// gardener_dormancy_probe_readonly.go — ReconcileDormancyProbe over a reader
// that can only read (gd-yK7bWh9s3W). The beads viewer's /plans/{id} cockpit
// (gd-f2fc4jcRn3) and gardenhub's gardener face on /garden/bead/{id} both show
// a gardener's live park; neither holds a gardenerDaemon, and neither may write
// through the probe. This adapts the reads they do have and stubs every write.

import (
	"context"
	"errors"
	"log/slog"

	"gardenio/controller/internal/beadsapi"
)

// DormancyReader is the read slice of the daemon client the dormancy probe
// needs: the bead, its forward dependencies (for a task's merge-request bead)
// and the filtered list the frontier predicates count with.
type DormancyReader interface {
	GetBead(ctx context.Context, beadID string) (*beadsapi.BeadDetail, error)
	GetDependencies(ctx context.Context, beadID string) ([]beadsapi.Dependency, error)
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
}

// ErrDormancyProbeReadOnly is what every write on the read-only adapter returns.
var ErrDormancyProbeReadOnly = errors.New("bridge: dormancy probe is read-only")

// ReconcileDormancyProbeReadOnly is ReconcileDormancyProbe over r, with every
// write refused so a future probe change can never write through a reader. A
// nil r probes nothing (Evaluated=false), as a nil daemon does.
func ReconcileDormancyProbeReadOnly(ctx context.Context, r DormancyReader, logger *slog.Logger, bead *beadsapi.BeadDetail) *ReconcileDormancy {
	if r == nil {
		return ReconcileDormancyProbe(ctx, nil, logger, bead)
	}
	return ReconcileDormancyProbe(ctx, readOnlyGardenerDaemon{r}, logger, bead)
}

// readOnlyGardenerDaemon adapts a DormancyReader to the gardener daemon
// interface: reads pass through, writes return ErrDormancyProbeReadOnly, and
// MRBeadForTask mirrors beadsapi.Client.MRBeadForTask over the reader's reads.
// The optional reads gardenerDaemonStopAdapter looks for (batch dependencies,
// MR by URL) are forwarded when the reader has them: without the batch fetch
// the operator_wedge count cannot subtract escalation-blocked children, so the
// probe would never prove that park.
type readOnlyGardenerDaemon struct {
	DormancyReader
}

// ListBeadsFiltered never hands the probe a nil result: the query evaluator
// reads result.Total unguarded.
func (d readOnlyGardenerDaemon) ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	res, err := d.DormancyReader.ListBeadsFiltered(ctx, q)
	if err == nil && res == nil {
		res = &beadsapi.ListBeadsResult{}
	}
	return res, err
}

func (d readOnlyGardenerDaemon) BatchGetDependencies(ctx context.Context, ids []string, direction string) (map[string][]beadsapi.Dependency, error) {
	if b, ok := d.DormancyReader.(gardenerBatchDepDaemon); ok {
		return b.BatchGetDependencies(ctx, ids, direction)
	}
	return nil, errGardenerDaemonNoBatch
}

func (d readOnlyGardenerDaemon) BatchGetBeads(ctx context.Context, ids []string) (map[string]*beadsapi.BeadDetail, error) {
	if b, ok := d.DormancyReader.(gardenerBatchDepDaemon); ok {
		return b.BatchGetBeads(ctx, ids)
	}
	return nil, errGardenerDaemonNoBatch
}

func (d readOnlyGardenerDaemon) MRBeadByURL(ctx context.Context, mrURL string) (*beadsapi.BeadDetail, error) {
	if m, ok := d.DormancyReader.(gardenerMRURLDaemon); ok {
		return m.MRBeadByURL(ctx, mrURL)
	}
	return nil, nil
}

func (readOnlyGardenerDaemon) UpdateBeadFields(context.Context, string, map[string]string) error {
	return ErrDormancyProbeReadOnly
}

func (readOnlyGardenerDaemon) UpdateBeadFieldsTyped(context.Context, string, map[string]any) error {
	return ErrDormancyProbeReadOnly
}

func (readOnlyGardenerDaemon) AddComment(context.Context, string, string, string) error {
	return ErrDormancyProbeReadOnly
}

func (readOnlyGardenerDaemon) AddLabel(context.Context, string, string) error {
	return ErrDormancyProbeReadOnly
}

func (readOnlyGardenerDaemon) CloseBead(context.Context, string, map[string]string) error {
	return ErrDormancyProbeReadOnly
}

func (readOnlyGardenerDaemon) UpdateBead(context.Context, string, beadsapi.UpdateBeadRequest) error {
	return ErrDormancyProbeReadOnly
}

// MRBeadForTask returns the merge-request bead a task produces (its first
// `produces` dependency of type merge-request), or nil when none is linked.
func (d readOnlyGardenerDaemon) MRBeadForTask(ctx context.Context, taskBeadID string) (*beadsapi.BeadDetail, error) {
	if taskBeadID == "" {
		return nil, nil
	}
	deps, err := d.GetDependencies(ctx, taskBeadID)
	if err != nil {
		return nil, err
	}
	for _, dep := range deps {
		if dep.Type != "produces" || dep.DependsOnID == "" {
			continue
		}
		mr, err := d.GetBead(ctx, dep.DependsOnID)
		if err != nil || mr == nil {
			continue
		}
		if mr.Type == "merge-request" {
			return mr, nil
		}
	}
	return nil, nil
}
