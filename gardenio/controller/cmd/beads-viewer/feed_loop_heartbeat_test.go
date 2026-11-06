package main

import "testing"

// feed_loop_heartbeat_test.go — ka-0000004629. A respawning gardener whose
// remaining work is operator-gated re-posts a near-identical zero-delta "the
// loop ran, all agent-doable work DONE / OPERATOR-GATED, nothing for me to do"
// heartbeat every wake, stamping a wall-clock "run HH:MMZ" instead of a "cycle
// N" cadence marker — so neither feedSquawkGardenerCycleTick (no cadence word)
// nor feedSquawkNoopTick (no "=0" zero count) folds it. feedSquawkLoopHeartbeat
// drops it; feedSquawkGreeting additionally drops thread-agent presence pings.
// Fixtures are verbatim live captures (HEAD 25dd425).

// TestFeedSquawkLoopHeartbeat covers the operator-gated / agent-work-done /
// sweep-start drop and its concrete-delta retain-guard.
func TestFeedSquawkLoopHeartbeat(t *testing.T) {
	cases := []struct {
		name  string
		title string
		want  bool
	}{
		// Dropped: the 3 live Keycloak ECS→EKS run-HH:MMZ operator-gated heartbeats
		// (gd-3CAJWf4Bvu / gd-1OYDTib1qf / gd-vKn55gJ2t0). Each names only an open
		// escalation, bare unmerged MR refs (!384, !1492 — never "merged !NNNN"),
		// and "No crew respawned" — no concrete delta this run.
		{"keycloak run 0017Z no agent work left", "🔑 Keycloak ECS→EKS (epic ka-0000003952) run 00:17Z — loop OPERATOR-GATED, no agent work left. LIVE: ECS auth.e2e/realms/acme=502; EKS keycloak-k8s=404. Blocker = escalation gd-ndWYvwGnpf: operator merge acme-helm-chart !384 + monorepo !1492, then helm upgrade keycloak+tenant-system. No crew respawned (re-hits no-creds wall).", true},
		{"keycloak run 2314Z loop is operator-gated", "🔑 Keycloak ECS→EKS (epic ka-0000003952) run 23:14Z — loop is OPERATOR-GATED, no agent work left. LIVE: legacy ECS auth.e2e/realms/acme=502; EKS keycloak-k8s=404. Blocker = escalation gd-ndWYvwGnpf: operator merge acme-helm-chart !384 + monorepo !1492. Both MRs CI-green & conflict-free. No crew respawned.", true},
		{"keycloak run 2215Z all agent work done", "🔑 Keycloak ECS→EKS (epic ka-0000003952) run 22:15Z — all agent work DONE & CI-green; loop is OPERATOR-GATED on escalation gd-ndWYvwGnpf (P0, 1d). LIVE: legacy ECS auth.e2e=502; EKS keycloak-k8s=404. Blocker: operator merge acme-helm-chart !384 + monorepo !1492. No crew respawned.", true},
		// Dropped: the gardener-run framing variants (gd-l63DqHNB3T / gd-0jrrmgjntk):
		// "All agent-doable work DONE … SOLE BLOCKER = operator-only gated cutover".
		{"keycloak operator-only gated cutover", "🔑 Keycloak ECS→EKS (epic ka-0000003952) — gardener run 21:15Z. All 4 provisioning children CLOSED. Agent-doable work DONE + CI-green: acme-helm-chart !384 + monorepo !1492. SOLE BLOCKER = operator-only gated cutover (escalation gd-ndWYvwGnpf).", true},
		{"keycloak all agent-doable work done", "🔑 Keycloak ECS→EKS (epic ka-0000003952) — gardener run 19:00Z. All 4 provisioning children CLOSED. ALL agent-doable work done + CI-green: acme-helm-chart !384 + monorepo !1492. SOLE BLOCKER = operator-only gated cutover (escalation gd-ndWYvwGnpf).", true},
		// Dropped: triage-intake sweep-start ticks (no outcome).
		{"triage starting sweep", "triage-intake gardener (project=gardenio): starting sweep of orphaned/abandoned beads into needs-intake lane", true},
		{"gardener waking for sweep", "triage-intake gardener (gardenio) waking for the ready-queue sweep", true},
		// Retained: a gated heartbeat that ALSO landed a concrete delta THIS run is
		// a real report — losing one is worse than leaving a heartbeat.
		{"operator-gated but merged this run", "🔑 Keycloak ECS→EKS (epic ka-0000003952) run 02:00Z — loop OPERATOR-GATED, but merged !1492 to monorepo main this run; remaining cutover gated on gd-ndWYvwGnpf", false},
		{"all work done but cut release", "gardener run 03:00Z — all agent work DONE; cut 2026.171.1 (pipeline GREEN) before handing off to operator", false},
		{"operator-gated but spawned crew", "Migration gardener run 04:00Z — operator-gated cutover; spawned 2 crew on the realm-parity follow-up gd-abc", false},
		// Retained: a substantive squawk that merely mentions "operator" in prose,
		// with no heartbeat signature, is untouched.
		{"plain status no heartbeat signature", "release-owner: cut 2026.169.2, merged MR !4575 — pipeline GREEN", false},
		{"finding mentioning operator decision", "found the operator decision gd-x was already resolved — proceeding with cutover", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := &feedEventBead{Type: "message", Title: c.title, Labels: []string{"squawk"}}
			if got := feedSquawkLoopHeartbeat(b); got != c.want {
				t.Errorf("feedSquawkLoopHeartbeat(%q) = %v, want %v", c.title, got, c.want)
			}
		})
	}
	if feedSquawkLoopHeartbeat(nil) {
		t.Errorf("feedSquawkLoopHeartbeat(nil) = true, want false")
	}
}

// TestFeedSquawkGreeting_ThreadAgentPresence covers the thread-agent presence
// pings (Form 6) added in ka-0000004629 — a Slack-spawned thread agent
// announcing it is alive and reading the thread, with no workflow outcome.
func TestFeedSquawkGreeting_ThreadAgentPresence(t *testing.T) {
	cases := []struct {
		name  string
		title string
		want  bool
	}{
		// Dropped: the live thread-agent greetings (gd-MRghZdWyKo et al.).
		{"thread agent on project here", "👋 thread agent on project gardenio here — reading the thread now", true},
		{"thread agent online reading thread", "thread agent online — reading thread before responding", true},
		{"thread agent picking up", "👋 gvisor-poc thread agent on project gardenio, picking up the fast hot resume follow-up (ka-0000000983). Digging into the spawn/resume code now.", true},
		{"role=thread online", "role=thread agent (project gardenio) online — reading thread", true},
		// Retained: a thread agent leading with a concrete outcome keeps its card
		// (the heartbeat outcome retain-guard runs before feedSquawkGreeting).
		{"thread agent merged", "thread agent on project gardenio — merged !4630, fix shipped", false},
		// Retained: a substantive squawk merely mentioning a thread in prose.
		{"mentions thread in prose", "investigated the Slack thread routing bug — fixed gd-abc, tests pass", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := &feedEventBead{Type: "message", Title: c.title, Labels: []string{"squawk"}}
			if got := feedSquawkHeartbeat(b); got != c.want {
				t.Errorf("feedSquawkHeartbeat(%q) = %v, want %v", c.title, got, c.want)
			}
		})
	}
}
