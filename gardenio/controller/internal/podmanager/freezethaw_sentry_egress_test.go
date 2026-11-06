package podmanager

import (
	"context"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes/fake"
)

// TestSplitEgressTarget covers the URL / bare-host:port / malformed shapes the
// in-sentry egress dial (fix B, ka-0000008207) must accept. A URL (coop's NATS
// "nats://host:4222") has its scheme + any path stripped; a bare "host:port" (the
// beads daemon BEADS_HTTP_ADDR) passes through; a host with no port falls back to
// the default; and an empty/garbage value returns ("","") so the script skips the
// dial for that target rather than rendering a broken probe.
func TestSplitEgressTarget(t *testing.T) {
	for _, tc := range []struct {
		name, raw, def, host, port string
	}{
		{"nats url with port", "nats://gardenio-nats:4222", "4222", "gardenio-nats", "4222"},
		{"nats url no port uses default", "nats://gardenio-nats", "4222", "gardenio-nats", "4222"},
		{"bare host:port", "gardenio-beads:8080", "8080", "gardenio-beads", "8080"},
		{"bare host no port uses default", "localhost", "8080", "localhost", "8080"},
		{"http url with path stripped", "http://beads:8080/api/v1", "8080", "beads", "8080"},
		{"url with path and explicit port", "nats://h:5222/x", "4222", "h", "5222"},
		{"empty -> empty (dial skipped)", "", "4222", "", ""},
		{"whitespace -> empty", "   ", "4222", "", ""},
		{"scheme only -> empty", "nats://", "4222", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, p := splitEgressTarget(tc.raw, tc.def)
			if h != tc.host || p != tc.port {
				t.Errorf("splitEgressTarget(%q,%q) = (%q,%q), want (%q,%q)", tc.raw, tc.def, h, p, tc.host, tc.port)
			}
		})
	}
}

// TestThawScript_PreRestoreRoutableAndSentryEgress proves the rendered thaw
// script carries both new layers for the restored-SENTRY-link-only frontier
// (ka-0000008207 / bug ka-0000000514) that b714bf11b did NOT close:
//
//	Fix A — a FINAL pre-restore routability re-verify + re-apply into the SAME
//	pinned restore-target inode the spec's network-namespace path points at,
//	IMMEDIATELY before the backgrounded `runsc restore` detaches and setupNetwork()
//	scrapes the netns into the sentry netstack. It closes the window where a late
//	kubelet CNI DEL re-strips the addr/route after the existing pre-restore apply.
//
//	Fix B — a SENTRY-SIDE egress assertion in [6c]: it reads the restored SENTRY's
//	OWN netstack (runsc exec, not the host kernel's view the [6a] check is blind to)
//	for a default route + non-lo iface, and TCP-dials the rehydrate consumers'
//	endpoints (NATS/beads) from inside the sandbox, failing-closed FT_ROOT_RC=128 on
//	a definitively link-only sentry / ENETUNREACH dial.
func TestThawScript_PreRestoreRoutableAndSentryEgress(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	natsHost, natsPort := splitEgressTarget("nats://gardenio-nats:4222", "4222")
	beadsHost, beadsPort := splitEgressTarget("gardenio-beads:8080", "8080")
	tw := m.buildThawScript(thawScriptParams{
		PlaceholderPod: "podb", BPodUID: "buid", ASandboxCID: "asb", AAgentCID: "aag",
		APodUID: "apod", ImagePath: "/img", BundlePath: "/bndl", ContainerCount: 2, RunscTimeout: 90,
		NatsHost: natsHost, NatsPort: natsPort, BeadsHost: beadsHost, BeadsPort: beadsPort,
	})

	// Fix A: the routability re-verify helper + the final pre-restore re-apply block.
	for _, want := range []string{
		"netns_routable",                // the routable re-verify helper
		"FT_NETNS_PRERESTORE_ROUTABLE=", // the fix-A outcome sentinel
		`netns_routable "$BNETNS_PATH"`, // probe the pinned restore-target inode
		`reassert_l3 "$BNETNS_PATH"`,    // re-apply L3 if a late DEL stripped it
		`self_ipam "$BNETNS_PATH"`,      // re-apply self-IPAM into the same inode
	} {
		if !strings.Contains(tw, want) {
			t.Errorf("thaw script missing fix-A pre-restore-routable fragment %q", want)
		}
	}

	// Fix B: the in-sentry egress assertion machinery.
	for _, want := range []string{
		"FT_SENTRY_DEFROUTE=", // the sentry's own default-route read via runsc exec
		"FT_SENTRY_NONLO=",    // the sentry's non-lo iface count
		"FT_SENTRY_EGRESS=",   // the end-to-end dial outcome
		"sentry_dial",         // the dial helper
		`RUNSC exec "$ABC"`,   // exec INTO the restored sentry (not host nsenter)
		"/proc/net/route",     // read the sentry netstack's own route table
		"ENETUNREACH",         // fail-closed only on the network-unreachable errno
		"create_connection",   // the in-sentry TCP dial
		"EGRESS_NATS_HOST='gardenio-nats'",
		"EGRESS_BEADS_HOST='gardenio-beads'",
	} {
		if !strings.Contains(tw, want) {
			t.Errorf("thaw script missing fix-B in-sentry egress fragment %q", want)
		}
	}

	// Fix A ordering: the final routability re-verify must run AFTER the existing
	// pre-restore reassert_l3/self_ipam (lines ~1552/1579) and BEFORE the
	// backgrounded barrier restore — that is the whole point (scrape a routable netns).
	iRoutable := strings.Index(tw, "FT_NETNS_PRERESTORE_ROUTABLE=")
	iRestore := strings.Index(tw, `restore --image-path="$IMG" --bundle "$BUNDLE/sandbox" --detach`)
	if iRoutable < 0 || iRestore < 0 || iRoutable > iRestore {
		t.Errorf("fix-A pre-restore routability re-verify (idx=%d) must precede the backgrounded barrier restore (idx=%d)", iRoutable, iRestore)
	}

	// Fix B ordering: the sentry-side egress assertion must run AFTER the restore
	// (the sentry only exists post-restore) and is read via the agent sub cid.
	iSentry := strings.Index(tw, "FT_SENTRY_DEFROUTE=")
	if iSentry < 0 || iSentry < iRestore {
		t.Errorf("fix-B sentry egress assertion (idx=%d) must run AFTER the barrier restore (idx=%d)", iSentry, iRestore)
	}

	// Fix B must read the SENTRY's own netstack, not just the host kernel netns the
	// [6a] check inspects — i.e. it must runsc-exec, not only nsenter the host PID.
	if !strings.Contains(tw, `RUNSC exec "$ABC" python3`) {
		t.Error("fix-B must read the restored sentry's OWN netstack via runsc exec into the agent sub-container")
	}
}

// TestThawScript_SentryEgressDialSkippedWhenNoTargets proves the end-to-end dial
// is OFF when no egress endpoints are configured (older controller / NATS unset),
// while the in-sentry default-route check (which needs no target) still renders —
// so an unset endpoint degrades to the link-only route detector, never breaks thaw.
func TestThawScript_SentryEgressDialSkippedWhenNoTargets(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	tw := m.buildThawScript(thawScriptParams{
		PlaceholderPod: "podb", BPodUID: "buid", ASandboxCID: "asb", AAgentCID: "aag",
		APodUID: "apod", ImagePath: "/img", BundlePath: "/bndl", ContainerCount: 2, RunscTimeout: 90,
		// No NatsHost/BeadsHost — empty egress targets.
	})
	if !strings.Contains(tw, "EGRESS_NATS_HOST=''") || !strings.Contains(tw, "EGRESS_BEADS_HOST=''") {
		t.Error("empty egress targets must render empty EGRESS_*_HOST vars")
	}
	// With no resolved ClusterIP literals the dial-addr vars are empty too.
	if !strings.Contains(tw, "EGRESS_NATS_ADDR=''") || !strings.Contains(tw, "EGRESS_BEADS_ADDR=''") {
		t.Error("unresolved egress targets must render empty EGRESS_*_ADDR dial vars")
	}
	// The route-based link-only detector (FT_SENTRY_DEFROUTE) is target-free and must
	// still be present — the load-bearing floor that catches the prod-.13 bug.
	if !strings.Contains(tw, "FT_SENTRY_DEFROUTE=") {
		t.Error("the target-free in-sentry default-route check must render even with no egress targets")
	}
	// The dial is guarded behind a non-empty resolved-ClusterIP-literal test (bug
	// ka-0000002907), so with no literals the FT_SENTRY_EGRESS sentinel still emits
	// (defaulting to skipped) but no dial fires. It is the ADDR vars, not the DNS-name
	// HOST vars, because a name-dial from a link-only sentry gaierrors to inconclusive.
	if !strings.Contains(tw, `if [ -n "$EGRESS_NATS_ADDR$EGRESS_BEADS_ADDR" ]; then`) {
		t.Error("the end-to-end dial must be guarded behind a non-empty resolved-ClusterIP-literal test")
	}
}

// thawStdout returns the canonical OK thaw stdout with a single sentinel replaced,
// for the link-only / egress defense-in-depth cases below.
func thawStdoutReplace(old, new string) string {
	return strings.Replace(thawOKStdout, old, new, 1)
}

// TestThaw_LinkOnlySentryFailsClosed proves the Go-side defense-in-depth for fix B
// (ka-0000008207): a restore whose HOST-view netns check passed (FT_NETNS_IFACES=1,
// FT_NETNS_DEFROUTE=yes — the [6a] check is blind to a link-only sentry) but whose
// SENTRY-side read reports no default route (FT_SENTRY_DEFROUTE=no) or an
// ENETUNREACH dial (FT_SENTRY_EGRESS=link-only) must fail loud and NOT fire
// rehydration. This is the prod .13 signature the node script fails-closes on; here
// we assert the controller is defense-in-depth if a verified-but-link-only restore
// ever reaches the post-state check with rc=0.
func TestThaw_LinkOnlySentryFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name, mutated, errFrag string
	}{
		{
			name:    "sentry has no default route",
			mutated: thawStdoutReplace("FT_SENTRY_DEFROUTE=yes", "FT_SENTRY_DEFROUTE=no"),
			errFrag: "restored SENTRY netstack is link-only",
		},
		{
			name:    "in-sentry egress dial network-unreachable",
			mutated: thawStdoutReplace("FT_SENTRY_EGRESS=ok", "FT_SENTRY_EGRESS=link-only"),
			errFrag: "cannot egress to the rehydrate consumers' endpoints",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ns, node := "gvisor-poc", "node-1"
			client := fake.NewClientset()
			client.PrependReactor("create", "pods", readyPodReactor("b-uid-456"))
			exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: tc.mutated}}
			store := NewInMemorySnapshotStore()
			_ = store.Put(context.Background(), Snapshot{
				AgentName: "agentsim", Namespace: ns, PodName: "agentsim", Node: node,
				SandboxCID: "sb", AgentCID: "ag", PodUID: "a", ContainerCount: 2,
				PodTemplate: sanitizePodTemplate(livePodA("agentsim", ns, node).Spec),
			})
			rehydr := &recordingRehydrator{}
			m := newFreezeThawManager(t, client, exec, store, rehydr)

			_, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim"})
			if err == nil || !strings.Contains(err.Error(), tc.errFrag) {
				t.Fatalf("want link-only-sentry fail-closed error containing %q, got %v", tc.errFrag, err)
			}
			if len(rehydr.reqs) != 0 {
				t.Error("rehydration must not fire when the restored sentry is link-only")
			}
		})
	}
}

// TestThaw_Canary11SignatureFailsClosed is the MOST IMPORTANT red-bar for bug
// ka-0000002907: the exact canary #11 restore signature must fail-CLOSED at restore
// time, NOT be mis-declared "restore verified" and die 180s later on the rehydrate
// ack gate. The signature: the HOST-view netns check passes (FT_NETNS_IFACES=1,
// FT_NETNS_DEFROUTE=yes — the [6a] check is blind to a link-only sentry) AND the
// script exited rc=0, but the SENTRY-side read never positively verified a route
// (FT_SENTRY_DEFROUTE=unknown, FT_SENTRY_NONLO=-1) and the in-sentry dial was
// FT_SENTRY_EGRESS=inconclusive (the pre-fix bare-DNS gaierror the #11 guards
// flaked open on). Before the fix the Go acceptance treated unknown/inconclusive as
// not-a-failure, so this PASSED thaw (the red bar). After the fix the DEFROUTE
// positive-proof requirement rejects a sentry that did not positively verify a
// default route, so Thaw fails and rehydration NEVER fires.
func TestThaw_Canary11SignatureFailsClosed(t *testing.T) {
	mutated := thawOKStdout
	// host-view [6a] passes — leave FT_NETNS_IFACES=1 / FT_NETNS_DEFROUTE=yes intact.
	mutated = strings.Replace(mutated, "FT_SENTRY_DEFROUTE=yes", "FT_SENTRY_DEFROUTE=unknown", 1)
	mutated = strings.Replace(mutated, "FT_SENTRY_NONLO=1", "FT_SENTRY_NONLO=-1", 1)
	mutated = strings.Replace(mutated, "FT_SENTRY_EGRESS=ok", "FT_SENTRY_EGRESS=inconclusive", 1)

	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset()
	client.PrependReactor("create", "pods", readyPodReactor("b-uid-456"))
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: mutated}}
	store := NewInMemorySnapshotStore()
	_ = store.Put(context.Background(), Snapshot{
		AgentName: "agentsim", Namespace: ns, PodName: "agentsim", Node: node,
		SandboxCID: "sb", AgentCID: "ag", PodUID: "a", ContainerCount: 2,
		PodTemplate: sanitizePodTemplate(livePodA("agentsim", ns, node).Spec),
	})
	rehydr := &recordingRehydrator{}
	m := newFreezeThawManager(t, client, exec, store, rehydr)

	_, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim"})
	if err == nil {
		t.Fatal("the canary #11 signature (host-view passes, sentry defroute=unknown + egress=inconclusive) must FAIL the thaw — a sentry that never positively verified a default route must not be declared restore-verified")
	}
	if len(rehydr.reqs) != 0 {
		t.Errorf("rehydration must not fire on the canary #11 signature; got %d requests", len(rehydr.reqs))
	}
}

// TestThaw_PreRestoreRoutableNoFailsClosed is the fix-A red bar: a restore whose
// FINAL pre-restore routability verdict is FT_NETNS_PRERESTORE_ROUTABLE=no (a late
// kubelet CNI DEL re-stripped L3 after the re-apply, so gVisor setupNetwork() would
// scrape a link-only netns into the sentry) must fail-CLOSED. The node script now
// fails-closed FT_ROOT_RC=128 on this verdict, but here we drive rc=0 to prove the
// Go acceptance is defense-in-depth: a "prerestore not routable" verdict is never
// declared restore-verified even if the script's own fail-close were bypassed. This
// PASSED thaw before the fix (the Go side ignored FT_NETNS_PRERESTORE_ROUTABLE).
func TestThaw_PreRestoreRoutableNoFailsClosed(t *testing.T) {
	mutated := strings.Replace(thawOKStdout, "FT_NETNS_PRERESTORE_ROUTABLE=yes", "FT_NETNS_PRERESTORE_ROUTABLE=no", 1)

	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset()
	client.PrependReactor("create", "pods", readyPodReactor("b-uid-456"))
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: mutated}}
	store := NewInMemorySnapshotStore()
	_ = store.Put(context.Background(), Snapshot{
		AgentName: "agentsim", Namespace: ns, PodName: "agentsim", Node: node,
		SandboxCID: "sb", AgentCID: "ag", PodUID: "a", ContainerCount: 2,
		PodTemplate: sanitizePodTemplate(livePodA("agentsim", ns, node).Spec),
	})
	rehydr := &recordingRehydrator{}
	m := newFreezeThawManager(t, client, exec, store, rehydr)

	_, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim"})
	if err == nil {
		t.Fatal("FT_NETNS_PRERESTORE_ROUTABLE=no must FAIL the thaw — a netns that was not routable at scrape time boots a link-only sentry")
	}
	if len(rehydr.reqs) != 0 {
		t.Errorf("rehydration must not fire when the pre-restore netns was not routable; got %d requests", len(rehydr.reqs))
	}
}

// TestThaw_SentryEgressInconclusiveStillThaws proves the egress assertion never
// false-fail-closes a GOOD resume (carve-out ii, bug ka-0000002907): when the sentry
// DID positively verify its default route (FT_SENTRY_DEFROUTE=yes) but the in-sentry
// end-to-end dial was ambiguous — FT_SENTRY_EGRESS=inconclusive (a connection-refused
// or timeout: the netstack ROUTED the SYN, the backend was just down) or =skipped (no
// targets resolved) — the thaw must STILL succeed. The 180s ack gate remains the
// backstop; a service-down backend must never cold-start an otherwise-routable resume.
// (The DEFROUTE=unknown flake case moved to TestThaw_Canary11SignatureFailsClosed —
// after the fix an un-verified route is a fail-close, not a tolerated flake.)
func TestThaw_SentryEgressInconclusiveStillThaws(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(string) string
	}{
		{"dial inconclusive (refused/timeout) with route verified", func(s string) string {
			return strings.Replace(s, "FT_SENTRY_EGRESS=ok", "FT_SENTRY_EGRESS=inconclusive", 1)
		}},
		{"dial skipped (no targets) with route verified", func(s string) string {
			return strings.Replace(s, "FT_SENTRY_EGRESS=ok", "FT_SENTRY_EGRESS=skipped", 1)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ns, node := "gvisor-poc", "node-1"
			client := fake.NewClientset()
			client.PrependReactor("create", "pods", readyPodReactor("b-uid-456"))
			exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: tc.mutate(thawOKStdout)}}
			store := NewInMemorySnapshotStore()
			_ = store.Put(context.Background(), Snapshot{
				AgentName: "agentsim", Namespace: ns, PodName: "agentsim", Node: node,
				SandboxCID: "sb", AgentCID: "ag", PodUID: "a", ContainerCount: 2,
				PodTemplate: sanitizePodTemplate(livePodA("agentsim", ns, node).Spec),
			})
			m := newFreezeThawManager(t, client, exec, store, &recordingRehydrator{})

			if _, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim"}); err != nil {
				t.Fatalf("an inconclusive/flaked in-sentry egress probe must NOT fail the thaw, got %v", err)
			}
		})
	}
}

// TestThaw_LegacySnapshotWithoutSentryEgressSentinels proves backward-compatibility:
// a snapshot restored by an older node script that does not emit FT_SENTRY_* /
// FT_NETNS_PRERESTORE_ROUTABLE is treated as not-asserted (the [6a] inode + content
// checks still gate it), NOT as a failure — so older in-flight snapshots still thaw.
func TestThaw_LegacySnapshotWithoutSentryEgressSentinels(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset()
	client.PrependReactor("create", "pods", readyPodReactor("b-uid-456"))
	stdout := thawOKStdout
	for _, line := range []string{
		"FT_NETNS_PRERESTORE_ROUTABLE=yes\n",
		"FT_SENTRY_DEFROUTE=yes\n",
		"FT_SENTRY_NONLO=1\n",
		"FT_SENTRY_EGRESS=ok\n",
	} {
		stdout = strings.Replace(stdout, line, "", 1)
	}
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: stdout}}
	store := NewInMemorySnapshotStore()
	_ = store.Put(context.Background(), Snapshot{
		AgentName: "agentsim", Namespace: ns, PodName: "agentsim", Node: node,
		SandboxCID: "sb", AgentCID: "ag", PodUID: "a", ContainerCount: 2,
		PodTemplate: sanitizePodTemplate(livePodA("agentsim", ns, node).Spec),
	})
	m := newFreezeThawManager(t, client, exec, store, &recordingRehydrator{})

	if _, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim"}); err != nil {
		t.Fatalf("legacy snapshot without sentry-egress sentinels should still thaw, got %v", err)
	}
}

// TestThawScript_SentryDialTargetsClusterIPLiteral is the fix-B [6c-2] template red
// bar for bug ka-0000002907: the in-sentry end-to-end dial must target the ClusterIP
// LITERAL (resolved in Go, passed via NatsDialAddr/BeadsDialAddr) — NOT the bare
// service DNS name. On the pre-fix script the dial used $EGRESS_NATS_HOST (a bare name
// like "gardenio-nats"); a link-only sentry's kube-dns lookup then failed gaierror ->
// classified "other" -> FT_SENTRY_EGRESS=inconclusive -> flake-open PASS (the exact
// #11 miss). Dialing the ClusterIP literal makes a link-only sentry no-route with
// EHOSTUNREACH -> FT_SENTRY_EGRESS=link-only -> fail-closed. This asserts the rendered
// [6c-2] dials $EGRESS_NATS_ADDR / $EGRESS_BEADS_ADDR (the literals), that the literals
// render, and that a bounded readiness+retry loop wraps the [6c-1] in-sentry read so a
// not-yet-joinable sentry is retried before its read is accepted as unknown.
func TestThawScript_SentryDialTargetsClusterIPLiteral(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	tw := m.buildThawScript(thawScriptParams{
		PlaceholderPod: "podb", BPodUID: "buid", ASandboxCID: "asb", AAgentCID: "aag",
		APodUID: "apod", ImagePath: "/img", BundlePath: "/bndl", ContainerCount: 2, RunscTimeout: 90,
		NatsHost: "gardenio-nats", NatsPort: "4222", BeadsHost: "gardenio-beads", BeadsPort: "8080",
		NatsDialAddr: "172.20.176.47", BeadsDialAddr: "172.20.162.152",
	})

	// The resolved ClusterIP literals must render into dedicated dial-addr vars.
	for _, want := range []string{
		"EGRESS_NATS_ADDR='172.20.176.47'",
		"EGRESS_BEADS_ADDR='172.20.162.152'",
	} {
		if !strings.Contains(tw, want) {
			t.Errorf("thaw script missing rendered ClusterIP-literal dial var %q", want)
		}
	}
	// The [6c-2] dial must target the literal ADDR vars, NOT the bare-DNS HOST vars.
	if !strings.Contains(tw, `sentry_dial "$EGRESS_NATS_ADDR"`) {
		t.Error("the [6c-2] NATS dial must target the ClusterIP literal $EGRESS_NATS_ADDR, not the bare DNS $EGRESS_NATS_HOST")
	}
	if !strings.Contains(tw, `sentry_dial "$EGRESS_BEADS_ADDR"`) {
		t.Error("the [6c-2] beads dial must target the ClusterIP literal $EGRESS_BEADS_ADDR, not the bare DNS $EGRESS_BEADS_HOST")
	}
	if strings.Contains(tw, `sentry_dial "$EGRESS_NATS_HOST"`) || strings.Contains(tw, `sentry_dial "$EGRESS_BEADS_HOST"`) {
		t.Error("the [6c-2] dial must NOT target the bare service DNS name — a link-only sentry gaierrors on it (the #11 flake-open)")
	}
	// The dial is guarded behind non-empty resolved LITERALS (not the DNS-name vars):
	// with no ClusterIP resolved the dial skips and the [6c-1] route check is the floor.
	if !strings.Contains(tw, `if [ -n "$EGRESS_NATS_ADDR$EGRESS_BEADS_ADDR" ]; then`) {
		t.Error("the [6c-2] dial must be guarded behind a non-empty resolved-ClusterIP-literal test")
	}
	// Bounded readiness+retry around the [6c-1] in-sentry read: retry until the sentry
	// is joinable (a definitive yes/no read) before accepting an unknown. The retry
	// must sit BEFORE the [6c-1] classifier that reads $SENTRY_DEFROUTE.
	iRetry := strings.Index(tw, "for _sretry in $(seq 1")
	iClassify := strings.Index(tw, `if [ "$SENTRY_DEFROUTE"`)
	if iRetry < 0 {
		t.Error("the [6c-1] in-sentry read must be wrapped in a bounded readiness+retry loop (for _sretry in $(seq 1 ...))")
	}
	if iRetry >= 0 && iClassify >= 0 && iRetry > iClassify {
		t.Error("the readiness+retry loop must run BEFORE the [6c-1] link-only classifier reads $SENTRY_DEFROUTE")
	}
}

// TestThaw_SentryDefRouteUnknownFailsClosed is the Go-acceptance twin of the fix-B
// positive-proof rule (bug ka-0000002907): the DEFROUTE sentinel must be positively
// "yes" to be declared restore-verified. An "unknown" read (the retry-bounded probe
// still could not join the sentry / read its netstack) is NO LONGER tolerated as a
// benign flake — it is the #11 flake-open and must fail-close, exactly as a definitive
// "no" does. Absent (legacy) stays tolerated (covered by the legacy test above).
func TestThaw_SentryDefRouteUnknownFailsClosed(t *testing.T) {
	mutated := thawStdoutReplace("FT_SENTRY_DEFROUTE=yes", "FT_SENTRY_DEFROUTE=unknown")

	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset()
	client.PrependReactor("create", "pods", readyPodReactor("b-uid-456"))
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: mutated}}
	store := NewInMemorySnapshotStore()
	_ = store.Put(context.Background(), Snapshot{
		AgentName: "agentsim", Namespace: ns, PodName: "agentsim", Node: node,
		SandboxCID: "sb", AgentCID: "ag", PodUID: "a", ContainerCount: 2,
		PodTemplate: sanitizePodTemplate(livePodA("agentsim", ns, node).Spec),
	})
	rehydr := &recordingRehydrator{}
	m := newFreezeThawManager(t, client, exec, store, rehydr)

	_, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim"})
	if err == nil {
		t.Fatal("FT_SENTRY_DEFROUTE=unknown must fail the thaw — positive proof of a default route is required, an un-verified route is the #11 signature")
	}
	if len(rehydr.reqs) != 0 {
		t.Errorf("rehydration must not fire when the sentry route is unverified; got %d requests", len(rehydr.reqs))
	}
}

// TestResolveDialAddrViaDNS_IPPassthrough proves the default DNS resolver returns an
// IP literal verbatim (no lookup) and returns "" for empty/unparseable input — the
// two hermetic branches of resolveDialAddrViaDNS that never touch the network. The
// name-lookup branch is exercised in Thaw via the injectable dialAddrResolver seam
// (TestThaw_ResolvesClusterIPLiteralIntoDialAddr), not here, to keep this test
// network-free.
func TestResolveDialAddrViaDNS_IPPassthrough(t *testing.T) {
	if got := resolveDialAddrViaDNS(context.Background(), "172.20.176.47"); got != "172.20.176.47" {
		t.Errorf("an IP literal must pass through unchanged, got %q", got)
	}
	if got := resolveDialAddrViaDNS(context.Background(), ""); got != "" {
		t.Errorf("empty host must resolve to empty, got %q", got)
	}
	if got := resolveDialAddrViaDNS(context.Background(), "   "); got != "" {
		t.Errorf("whitespace host must resolve to empty, got %q", got)
	}
}

// TestThaw_ResolvesClusterIPLiteralIntoDialAddr proves the Thaw path threads the
// controller's own NatsURL/BeadsHTTPAddr through the injectable dialAddrResolver and
// renders the resolved ClusterIP LITERALS into the node script's [6c-2] dial vars
// (bug ka-0000002907). It stubs the resolver (no real DNS) and inspects the script the
// fakeNodeExecutor recorded.
func TestThaw_ResolvesClusterIPLiteralIntoDialAddr(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset()
	client.PrependReactor("create", "pods", readyPodReactor("b-uid-456"))
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: thawOKStdout}}
	store := NewInMemorySnapshotStore()
	_ = store.Put(context.Background(), Snapshot{
		AgentName: "agentsim", Namespace: ns, PodName: "agentsim", Node: node,
		SandboxCID: "sb", AgentCID: "ag", PodUID: "a", ContainerCount: 2,
		PodTemplate: sanitizePodTemplate(livePodA("agentsim", ns, node).Spec),
	})
	m := newFreezeThawManager(t, client, exec, store, &recordingRehydrator{})
	m.NatsURL = "nats://gardenio-nats:4222"
	m.BeadsHTTPAddr = "http://gardenio-beads.gardenio.svc.cluster.local:8080"
	// Hermetic resolver: map the two known service hosts to their ClusterIP VIPs (the
	// exact literals the live forensics observed), everything else "".
	m.dialAddrResolver = func(_ context.Context, host string) string {
		switch host {
		case "gardenio-nats":
			return "172.20.176.47"
		case "gardenio-beads.gardenio.svc.cluster.local":
			return "172.20.162.152"
		}
		return ""
	}

	if _, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim"}); err != nil {
		t.Fatalf("thaw with a stubbed ClusterIP resolver should succeed, got %v", err)
	}
	if len(exec.scripts) == 0 {
		t.Fatal("no thaw script recorded")
	}
	script := exec.scripts[len(exec.scripts)-1]
	for _, want := range []string{
		"EGRESS_NATS_ADDR='172.20.176.47'",
		"EGRESS_BEADS_ADDR='172.20.162.152'",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("thaw script missing resolved ClusterIP-literal dial var %q", want)
		}
	}
}

// TestThaw_UnresolvableEgressSkipsDial proves a resolver that returns "" (service
// absent / no DNS) renders EMPTY dial-addr vars so the [6c-2] dial SKIPS — the
// [6c-1] in-sentry route check remains the load-bearing floor, and the thaw still
// succeeds (an unresolvable VIP must never cold-start an otherwise-good resume).
func TestThaw_UnresolvableEgressSkipsDial(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset()
	client.PrependReactor("create", "pods", readyPodReactor("b-uid-456"))
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: thawOKStdout}}
	store := NewInMemorySnapshotStore()
	_ = store.Put(context.Background(), Snapshot{
		AgentName: "agentsim", Namespace: ns, PodName: "agentsim", Node: node,
		SandboxCID: "sb", AgentCID: "ag", PodUID: "a", ContainerCount: 2,
		PodTemplate: sanitizePodTemplate(livePodA("agentsim", ns, node).Spec),
	})
	m := newFreezeThawManager(t, client, exec, store, &recordingRehydrator{})
	m.NatsURL = "nats://gardenio-nats:4222"
	m.BeadsHTTPAddr = "gardenio-beads:8080"
	m.dialAddrResolver = func(_ context.Context, _ string) string { return "" } // never resolves

	if _, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim"}); err != nil {
		t.Fatalf("thaw with an unresolvable egress VIP should still succeed (dial skipped), got %v", err)
	}
	script := exec.scripts[len(exec.scripts)-1]
	if !strings.Contains(script, "EGRESS_NATS_ADDR=''") || !strings.Contains(script, "EGRESS_BEADS_ADDR=''") {
		t.Error("an unresolvable egress host must render empty EGRESS_*_ADDR vars so the [6c-2] dial skips")
	}
}
