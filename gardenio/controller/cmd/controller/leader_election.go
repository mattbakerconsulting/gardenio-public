package main

import (
	"context"
	"log/slog"
	"os"
	"sync/atomic"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"

	"gardenio/controller/internal/config"
)

// leaderTiming is the lease timing of the controller's leader election.
type leaderTiming struct {
	lease, renewDeadline, retryPeriod time.Duration
}

var controllerLeaderTiming = leaderTiming{lease: 15 * time.Second, renewDeadline: 10 * time.Second, retryPeriod: 2 * time.Second}

// runLeaderElection starts the leader election loop. Only the leader runs
// the controller loop (runFn). When leadership is lost, the process exits
// so that Kubernetes restarts it and it can rejoin the election.
//
// On SIGTERM the lease is NOT released until a Codex token rotation in flight has
// reached the Secret (gd-6ggiM3hiV5). The elector therefore runs on its own
// context: signal cancellation stops the controller loop, the drain runs while
// the elector keeps renewing the lease, and only then is the lease released. A
// successor can never take over while the old leader still holds the only copy
// of a rotated single-use refresh token.
func runLeaderElection(ctx context.Context, logger *slog.Logger, cfg *config.Config, k8sClient kubernetes.Interface, runFn func(ctx context.Context)) {
	runLeaderElectionWith(ctx, logger, cfg.LeaderElectionIdentity, cfg.LeaderElectionID, cfg.Namespace, k8sClient, controllerLeaderTiming,
		func() { waitForCodexRotations(logger) }, func() { os.Exit(1) }, runFn)
}

func runLeaderElectionWith(ctx context.Context, logger *slog.Logger, id, leaseName, namespace string, k8sClient kubernetes.Interface, timing leaderTiming, drain func(), exit func(), runFn func(ctx context.Context)) {
	logger.Info("starting leader election",
		"id", id,
		"lease", leaseName,
		"namespace", namespace)

	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{
			Name:      leaseName,
			Namespace: namespace,
		},
		Client: k8sClient.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: id,
		},
	}

	// Detached from ctx: cancelling it is what releases the lease, and that must
	// happen only after the drain below.
	electCtx, stopElection := context.WithCancel(context.WithoutCancel(ctx))
	defer stopElection()
	var leading atomic.Bool
	go func() {
		<-ctx.Done()
		if !leading.Load() {
			stopElection() // still campaigning: nothing to drain
		}
	}()

	leaderelection.RunOrDie(electCtx, leaderelection.LeaderElectionConfig{
		Lock:            lock,
		LeaseDuration:   timing.lease,
		RenewDeadline:   timing.renewDeadline,
		RetryPeriod:     timing.retryPeriod,
		ReleaseOnCancel: true,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(leCtx context.Context) {
				leading.Store(true)
				logger.Info("elected as leader, starting controller")
				workCtx, cancel := context.WithCancel(leCtx)
				defer cancel()
				go func() {
					select {
					case <-ctx.Done():
						cancel()
					case <-workCtx.Done():
					}
				}()
				runFn(workCtx)
				if ctx.Err() != nil {
					// Shutdown: the lease is still held and renewed. Drain, then release.
					drain()
					stopElection()
				}
			},
			OnStoppedLeading: func() {
				logger.Error("lost leader election, exiting")
				// A Codex rotation in flight holds the only copy of a single-use refresh
				// token until it is written. On a graceful shutdown the drain already
				// ran; after an involuntary loss it runs here, and the successor's
				// keeper idles for codexHandoverGrace to cover it.
				drain()
				exit()
			},
			OnNewLeader: func(identity string) {
				if identity == id {
					return
				}
				logger.Info("new leader elected", "leader", identity)
			},
		},
	})
}
