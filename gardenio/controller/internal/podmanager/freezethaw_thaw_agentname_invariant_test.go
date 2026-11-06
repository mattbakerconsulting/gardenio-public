// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

package podmanager

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"gardenio/controller/internal/thawepoch"
)

// freezethaw_thaw_agentname_invariant_test.go pins the ka-0000008265 invariant
// (the canary-#7 both-consumer fail-close class, related to bug ka-0000000673 /
// MR !5003 / ka-0000001104):
//
// The gVisor thaw rehydrate handshake has TWO derivations that MUST address the
// SAME per-agent thaw-state dir, or the controller writes+polls one inode while
// the restored coop/gb watchers ack into another and the rehydrate gate
// fail-closes both consumers forever:
//
//   - WRITER/POLL dir  — freezethaw_rehydrate.go NodeRehydrationTrigger.Trigger:
//     stateDir = thawStateHostDir(req.AgentName). Where the epoch marker is
//     written and the .done acks are polled.
//   - MOUNT SOURCE dir — freezethaw_thaw.go thawOnce barrier-restore params:
//     ThawStateDir = thawStateHostDir(req.AgentName), rendered into the thaw
//     script's THAWDIR shell var and forced as the restored agent bundle's
//     MountThawState (/run/gardenio-thaw) bind source.
//
// BOTH key off req.AgentName, which equals snap.AgentName for every thaw today
// (the snapshot store is keyed by agent name). The risk these tests guard: a
// future rename-on-thaw makes req.AgentName != snap.AgentName, and if either
// derivation were forked onto the OTHER name (snap.AgentName), the two dirs
// would diverge and re-introduce the canary-#7 fail-close while the existing
// same-name tests stayed green. thawStateHostDir is the single source of truth;
// these tests fail the moment a derivation forks off req.AgentName.

// divergentSnapStore returns a snapshot whose AgentName DIVERGES from whatever
// key is requested — the rename-on-thaw scenario. The production
// InMemorySnapshotStore keys by snap.AgentName (Get(name) -> snap[name]), so it
// can NEVER yield a snapshot whose AgentName differs from the lookup key; that
// makes a writer-vs-mount-source fork invisible to a same-name fixture. This
// stub is the only way to drive req.AgentName != snap.AgentName through the real
// thawOnce path and observe which name each derivation actually keys off.
type divergentSnapStore struct{ snap Snapshot }

func (s *divergentSnapStore) Get(context.Context, string) (Snapshot, error) { return s.snap, nil }
func (s *divergentSnapStore) Put(context.Context, Snapshot) error           { return nil }
func (s *divergentSnapStore) Delete(context.Context, string) error          { return nil }
func (s *divergentSnapStore) List(context.Context) ([]Snapshot, error) {
	return []Snapshot{s.snap}, nil
}

// extractTHAWDIR slices the value of the THAWDIR='...' shell assignment out of a
// rendered thaw script — the literal dir the restore mount sources from. A
// render that renames or drops the assignment fails the extraction LOUD rather
// than letting the guard silently no-op.
func extractTHAWDIR(t *testing.T, script string) string {
	t.Helper()
	_, after, ok := strings.Cut(script, "THAWDIR='")
	if !ok {
		t.Fatalf("could not locate the THAWDIR=' shell assignment in the rendered thaw script — the mount-source render moved; update this guard (do NOT let it silently no-op)")
	}
	dir, _, ok := strings.Cut(after, "'")
	if !ok {
		t.Fatalf("THAWDIR assignment was not single-quote terminated in the rendered thaw script")
	}
	if dir == "" || !strings.HasPrefix(dir, ThawStateHostBase+"/") {
		t.Fatalf("extracted THAWDIR = %q, want a per-agent dir under %q", dir, ThawStateHostBase)
	}
	return dir
}

// TestThawStateKey_WriterAndMountSourceAgree is the headline ka-0000008265 guard.
// It drives the REAL Thaw() path (warm-B graft) with a snapshot whose AgentName
// DIVERGES from the thaw request's AgentName, then proves BOTH derivations key
// off req.AgentName (NOT snap.AgentName) and resolve to the SAME thaw-state dir:
//
//   - the restore MOUNT SOURCE (freezethaw_thaw.go: ThawStateDir = thawStateHostDir(req.AgentName)),
//     captured as the rendered THAWDIR; and
//   - the rehydrate WRITER key (the RehydrationRequest.AgentName the manager hands
//     the trigger, which derives stateDir = thawStateHostDir(req.AgentName)).
//
// Forking EITHER site onto snap.AgentName makes the two dirs diverge under this
// divergent fixture, failing the test — exactly the canary-#7 mismatch. With
// req.AgentName == snap.AgentName in prod the assertions are trivially satisfied,
// so the guard adds zero runtime constraint; it only fences the derivation key.
func TestThawStateKey_WriterAndMountSourceAgree(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	const reqAgent = "thaw-identity-new"    // the agent THIS thaw is for (req.AgentName)
	const snapAgent = "frozen-identity-old" // a DIFFERENT name baked into the snapshot

	// Warm-B graft hot path: a Ready placeholder Pod B already on the node, so
	// Thaw skips Create+waitReady and grafts straight onto it (the same harness
	// shape as TestThaw_RendersThawDirFromRequestAgent).
	placeholder := livePodA("warm-b-node-1-div", ns, node)
	placeholder.UID = types.UID("b-uid-div")
	placeholder.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	client := fake.NewClientset(placeholder)
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: thawOKStdout}}

	store := &divergentSnapStore{snap: Snapshot{
		AgentName:  snapAgent, // <-- diverges from reqAgent
		Namespace:  ns,
		PodName:    "absent-pod-a", // not in the fake client -> waitPodGone returns immediately
		Node:       node,
		ImagePath:  "/var/lib/gardenio/ckpt/" + snapAgent,
		BundlePath: "/var/lib/gardenio/bundle/" + snapAgent,
		SandboxCID: "sb", AgentCID: "ag", PodUID: "a", ContainerCount: 2,
		PodTemplate: sanitizePodTemplate(livePodA(snapAgent, ns, node).Spec),
	}}
	rehydr := &recordingRehydrator{}
	m := newFreezeThawManager(t, client, exec, store, rehydr)

	if _, err := m.Thaw(context.Background(), ThawRequest{
		AgentName: reqAgent, PlaceholderPod: placeholder.Name,
	}); err != nil {
		t.Fatalf("Thaw: %v", err)
	}

	// MOUNT SOURCE: the dir the restore re-points /run/gardenio-thaw at, taken from
	// the captured barrier-restore script's THAWDIR.
	var thawScript string
	for _, s := range exec.scripts {
		if strings.Contains(s, "<<'PYAGENT'") {
			thawScript = s
			break
		}
	}
	if thawScript == "" {
		t.Fatalf("no thaw barrier-restore script captured (execs=%d)", len(exec.scripts))
	}
	mountSourceDir := extractTHAWDIR(t, thawScript)

	// WRITER: the RehydrationRequest the manager handed the trigger. The trigger
	// derives its marker-write/poll dir as thawStateHostDir(req.AgentName), so this
	// request's AgentName IS the writer key.
	if len(rehydr.reqs) != 1 {
		t.Fatalf("rehydration calls = %d, want 1", len(rehydr.reqs))
	}
	writerAgent := rehydr.reqs[0].AgentName
	writerDir := thawStateHostDir(writerAgent)

	wantDir := thawStateHostDir(reqAgent)
	forkedDir := thawStateHostDir(snapAgent)

	// (1) The mount source must key off req.AgentName, never the snapshot's name.
	if mountSourceDir == forkedDir {
		t.Fatalf("restore MOUNT SOURCE was keyed off snap.AgentName (%q) -> %q, not req.AgentName (%q).\n"+
			"A rename-on-thaw would then point the restored watchers' /run/gardenio-thaw at a dir the controller never writes to "+
			"(the both-consumer fail-close of bug ka-0000000673). Key the mount source off req.AgentName via thawStateHostDir.",
			snapAgent, mountSourceDir, reqAgent)
	}
	if mountSourceDir != wantDir {
		t.Fatalf("restore MOUNT SOURCE = %q, want thawStateHostDir(req.AgentName) = %q", mountSourceDir, wantDir)
	}

	// (2) The rehydrate writer must key off req.AgentName, never the snapshot's name.
	if writerAgent == snapAgent {
		t.Fatalf("rehydrate WRITER keyed off snap.AgentName (%q), not req.AgentName (%q) — the marker would land in a dir "+
			"the restored mount never sources from (bug ka-0000000673). Carry req.AgentName into the RehydrationRequest.", snapAgent, reqAgent)
	}
	if writerAgent != reqAgent {
		t.Fatalf("rehydrate writer AgentName = %q, want req.AgentName %q", writerAgent, reqAgent)
	}

	// (3) The headline invariant: the two derivations resolve to the SAME dir.
	if writerDir != mountSourceDir {
		t.Fatalf("THAW-STATE KEY FORK: rehydrate writer dir %q != restore mount-source dir %q.\n"+
			"These MUST be the same inode or the controller's epoch marker and the restored coop/gb acks never rendezvous "+
			"(canary-#7 both-consumer fail-close, ka-0000008265 / ka-0000000673). thawStateHostDir is the single source of truth — "+
			"both call sites must derive from the same agent-name key (req.AgentName).", writerDir, mountSourceDir)
	}
}

// TestThawStateKey_RehydrateMarkerLandsAtMountSourceDir is the end-to-end
// rendezvous proof: it runs the REAL NodeRehydrationTrigger against a node-FS
// stand-in and asserts the epoch marker it writes is found by reading through
// the MOUNT SOURCE dir that the real thaw-script renderer emits as THAWDIR for
// the same agent. This exercises both derivation code paths (the trigger's
// stateDir AND buildThawScript's THAWDIR render) and proves they address one
// dir. A fork on either side moves the marker off the mount-source dir and the
// Stat below fails. The table covers a plain name and a sanitized one so the
// rendezvous holds across the agent-name -> path normalization too.
func TestThawStateKey_RehydrateMarkerLandsAtMountSourceDir(t *testing.T) {
	for _, agent := range []string{"agentsim", "gardenio-phase-4-implement-zzc"} {
		t.Run(agent, func(t *testing.T) {
			// MOUNT SOURCE: render the real thaw script for this agent and extract THAWDIR.
			m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
			script := m.buildThawScript(thawScriptParams{
				PlaceholderPod: "podb", BPodUID: "buid", ASandboxCID: "asb", AAgentCID: "aag",
				APodUID: "apod", ImagePath: "/img", BundlePath: "/bndl", ContainerCount: 2, RunscTimeout: 90,
				ThawStateDir: thawStateHostDir(agent), ThawStateDest: MountThawState,
			})
			mountSourceDir := extractTHAWDIR(t, script)

			// WRITER: drive the real trigger; onWrite acks both consumers at the dir the
			// trigger actually wrote to so the gate releases.
			fs := newNodeFS(t)
			fs.onWrite = func(fs *nodeFSExecutor) {
				stateDir := thawStateHostDir(agent)
				mk, ok, err := readMarkerRaw(fs, stateDir)
				if err != nil || !ok {
					return
				}
				fs.writeAckToNode(t, stateDir, thawepoch.ConsumerGB, thawepoch.Ack{Epoch: mk.Epoch, Consumer: thawepoch.ConsumerGB, OK: true})
				fs.writeAckToNode(t, stateDir, thawepoch.ConsumerCoop, thawepoch.Ack{Epoch: mk.Epoch, Consumer: thawepoch.ConsumerCoop, OK: true})
			}
			tr := newTrigger(fs)
			if err := tr.Trigger(context.Background(), RehydrationRequest{
				AgentName: agent, Node: "node-1", PodUID: "b-uid", Deadline: time.Second,
			}); err != nil {
				t.Fatalf("Trigger: %v", err)
			}

			// RENDEZVOUS: the marker the WRITER wrote must be findable by reading through
			// the MOUNT SOURCE dir. Use the dir extracted from the script (not a recomputed
			// constant) so a forked render is caught: if the two derivations disagree, the
			// marker is not at the mount source and this Stat fails.
			markerAtMountSource := fs.root + filepath.Join(mountSourceDir, thawepoch.MarkerName)
			if _, err := os.Stat(markerAtMountSource); err != nil {
				t.Fatalf("rehydrate WRITER did not land the epoch marker at the restore MOUNT SOURCE dir.\n"+
					"  mount source (script THAWDIR): %q\n  looked for marker at: %q\n  err: %v\n"+
					"The writer (thawStateHostDir(req.AgentName)) and the mount source (THAWDIR) forked onto different agent-name keys — "+
					"canary-#7 both-consumer fail-close (ka-0000008265 / ka-0000000673).",
					mountSourceDir, markerAtMountSource, err)
			}
		})
	}
}
