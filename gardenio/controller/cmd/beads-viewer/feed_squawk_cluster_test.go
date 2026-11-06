package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// feed_squawk_cluster_test.go — ka-0000005857. A scheduled gardener re-posts a
// near-identical status squawk about ONE incident/topic every respawn. Each is
// a DISTINCT message bead, so the same-bead collapse passes never fold them and
// the feed fans the same status out into N cards. collapseSquawkClusters folds a
// run keyed on (pod-suffix-normalized author, referenced epic/decision id) into
// one evolving "+N"/"×N" card. Different agents, different topics, and squawks
// naming no topic stay distinct.

// feedSquawkEventData builds a created-squawk event payload (message bead with
// the squawk label) carrying a created_by author and a title.
func feedSquawkEventData(t *testing.T, id, author, title string) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"bead": map[string]any{
			"id":         id,
			"type":       "message",
			"title":      title,
			"status":     "open",
			"created_by": author,
			"labels":     []string{"project:gardenio", "squawk"},
		},
	})
	if err != nil {
		t.Fatalf("marshal squawk event: %v", err)
	}
	return data
}

func TestFeedNormalizeAgentAuthor(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"strips 4-char pod suffix", "gardenio-nat-elimination-research-gardener-6zxh", "gardenio-nat-elimination-research-gardener"},
		{"strips a different pod suffix to same identity", "gardenio-nat-elimination-research-gardener-95dz", "gardenio-nat-elimination-research-gardener"},
		{"human email untouched", "matthew.baker@acme.example", "matthew.baker@acme.example"},
		{"non-agent name untouched", "thread-1781830866-842059", "thread-1781830866-842059"},
		{"lowercases", "Gardenio-Foo-ABCD", "gardenio-foo"},
		{"empty stays empty", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := feedNormalizeAgentAuthor(c.in); got != c.want {
				t.Errorf("feedNormalizeAgentAuthor(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
	// The two NAT-research respawns must normalize to the SAME identity so their
	// squawks cluster.
	a := feedNormalizeAgentAuthor("gardenio-nat-elimination-research-gardener-6zxh")
	b := feedNormalizeAgentAuthor("gardenio-nat-elimination-research-gardener-owrb")
	if a != b {
		t.Errorf("respawns should normalize to one identity: %q vs %q", a, b)
	}
}

func TestFeedSquawkTopicRef(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"epic id", "NAT-elimination gardener online (epic ka-0000007498, project gardenio).", "ka-0000007498"},
		{"decision id", "blocked on operator decision ka-0000003998 — pick topology", "ka-0000003998"},
		{"epic wins when both present", "epic gd-AAA, decision gd-BBB", "gd-aaa"},
		{"no topic ref", "release-owner: cut 2026.169.11, merged MR 4625", ""},
		{"bare kd mention is not a topic", "crew reporting in (gd-abc123)", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := feedSquawkTopicRef(c.in); got != c.want {
				t.Errorf("feedSquawkTopicRef(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestFeedSquawkZeroDeltaClass covers the zero-delta class classifier that lets
// topicless status pings fold: each known noise family gets its own coarse class,
// a squawk naming a concrete delta returns "" (never folded by class), and an
// ordinary topicless squawk returns "" so it always renders.
func TestFeedSquawkZeroDeltaClass(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"thread-agent ping", "👋 thread agent on project gardenio here, reading thread", "thread-agent-ping"},
		{"role=thread ping", "role=thread online — reading thread", "thread-agent-ping"},
		{"sweep start", "triage-intake gardener waking, starting sweep", "sweep-start"},
		{"gardener waking", "first-mate gardener waking for the morning sweep", "sweep-start"},
		{"gated loop heartbeat", "Keycloak ECS→EKS (epic gd-x) run 14:02Z — loop OPERATOR-GATED, no agent work left", "loop-heartbeat"},
		{"noop cycle tick", "currency cycle: 0 closed, NO drift, all green", "noop-tick"},
		{"concrete delta not classed", "release-owner: cut 2026.169.11, merged MR 4630", ""},
		{"spawned-N delta not classed", "delivery gardener: spawned 3 crew this run", ""},
		{"ordinary topicless squawk passes", "investigating the flaky e2e module locally", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := feedSquawkZeroDeltaClass(c.in); got != c.want {
				t.Errorf("feedSquawkZeroDeltaClass(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestCollapseSquawkClusters_ZeroDeltaClass proves the topicless zero-delta fold
// (ka-0000006458): a run of same-class status pings that name NO epic/decision
// topic — gated loop heartbeats from different respawns, "gardener waking /
// starting sweep" announcements, and "👋 thread agent … here" presence pings from
// DIFFERENT thread agents — each collapse to ONE representative per class
// (cross-author, by class alone), while a topicless squawk that names a concrete
// delta and an ordinary no-class squawk both render on their own.
func TestCollapseSquawkClusters_ZeroDeltaClass(t *testing.T) {
	t0 := mustTime(t, "2026-06-20T14:00:00Z")
	sq := func(id, author, title string, ts time.Time) feedCardItem {
		return feedCardItem{
			kind:  feedKindSquawk,
			bead:  &feedEventBead{ID: id, Type: "message", Title: title, CreatedBy: author},
			ts:    ts,
			count: 1,
		}
	}
	in := []feedCardItem{
		// 3 gated loop heartbeats — different respawns, no topic ref.
		sq("gd-h1", "monorepo-keycloak-ecs-eks-93az", "Keycloak ECS→EKS run 14:02Z — loop OPERATOR-GATED, no agent work left", t0),
		sq("gd-h2", "monorepo-keycloak-ecs-eks-7b2c", "Keycloak ECS→EKS run 14:22Z — loop OPERATOR-GATED, no agent work left", t0.Add(1*time.Minute)),
		sq("gd-h3", "monorepo-keycloak-ecs-eks-9xy1", "Keycloak ECS→EKS run 14:42Z — loop OPERATOR-GATED, no agent work left", t0.Add(2*time.Minute)),
		// 2 sweep-start announcements.
		sq("gd-w1", "gardenio-triage-intake-aaaa", "triage-intake gardener waking, starting sweep", t0.Add(3*time.Minute)),
		sq("gd-w2", "gardenio-triage-intake-bbbb", "triage-intake gardener (project gardenio) starting sweep", t0.Add(4*time.Minute)),
		// 2 thread-agent presence pings from DIFFERENT thread agents.
		sq("gd-t1", "thread-111", "👋 thread agent on project gardenio here, reading thread", t0.Add(5*time.Minute)),
		sq("gd-t2", "thread-222", "👋 thread agent on project gardenio here", t0.Add(6*time.Minute)),
		// A topicless squawk naming a concrete delta — renders on its own.
		sq("gd-d1", "gardenio-release-owner-cccc", "release-owner: cut 2026.169.11, merged MR 4630", t0.Add(7*time.Minute)),
		// An ordinary topicless squawk with no class — renders on its own.
		sq("gd-o1", "gardenio-crew-dddd", "investigating the flaky e2e module locally", t0.Add(8*time.Minute)),
	}
	out := collapseSquawkClusters(in)

	// 5 survivors: 1 loop-heartbeat rep + 1 sweep-start rep + 1 thread-ping rep +
	// the distinct delta + the ordinary squawk (the 3 zero-delta classes collapse
	// 7 cards → 3).
	if len(out) != 5 {
		var ids []string
		for _, it := range out {
			ids = append(ids, it.bead.ID)
		}
		t.Fatalf("expected 5 cards (3 class reps + delta + ordinary), got %d: %v", len(out), ids)
	}

	byID := map[string]*feedCardItem{}
	for i := range out {
		byID[out[i].bead.ID] = &out[i]
	}
	// Newest per class is the representative; older same-class siblings folded away.
	for _, folded := range []string{"gd-h1", "gd-h2", "gd-w1", "gd-t1"} {
		if _, ok := byID[folded]; ok {
			t.Errorf("older zero-delta sibling %s should have folded away", folded)
		}
	}
	if h := byID["gd-h3"]; h == nil || h.count != 3 {
		t.Errorf("loop-heartbeat rep gd-h3 should carry count 3, got %+v", h)
	}
	if w := byID["gd-w2"]; w == nil || w.count != 2 {
		t.Errorf("sweep-start rep gd-w2 should carry count 2, got %+v", w)
	}
	if tp := byID["gd-t2"]; tp == nil || tp.count != 2 {
		t.Errorf("thread-ping rep gd-t2 should carry count 2, got %+v", tp)
	}
	// The distinct-delta squawk and the ordinary squawk stay un-folded (count 1).
	if d := byID["gd-d1"]; d == nil || d.count != 1 {
		t.Errorf("distinct-delta squawk gd-d1 should render on its own, got %+v", d)
	}
	if o := byID["gd-o1"]; o == nil || o.count != 1 {
		t.Errorf("ordinary topicless squawk gd-o1 should render on its own, got %+v", o)
	}
}

// TestCollapseSquawkClusters covers the pure cluster-collapse pass: same
// normalized author + same topic folds (summing counts, taking the newest
// position), while a different agent, a different topic, a no-topic squawk, and
// non-squawk cards stay distinct.
func TestCollapseSquawkClusters(t *testing.T) {
	t0 := mustTime(t, "2026-06-15T05:00:00Z")
	sq := func(id, author, title string, ts time.Time) feedCardItem {
		return feedCardItem{
			kind:  feedKindSquawk,
			bead:  &feedEventBead{ID: id, Type: "message", Title: title, CreatedBy: author},
			ts:    ts,
			count: 1,
		}
	}

	// Three NAT-research gardener respawns post the same ZERO-DELTA epic-status
	// framing (no merged !NNNN / closed gd- / spawned N), interleaved with a
	// FOURTH gardener pod's zero-delta framing of the same epic and a work card.
	// Under the widened cross-author fold (ka-0000004629 FIX DIRECTION 3) all four
	// redundant framings of epic ka-0000007498 collapse to ONE card — the
	// ka-0000000398 "keep 1 cross-gardener framing" requirement — regardless of
	// which pod posted them, because none names a concrete delta.
	in := []feedCardItem{
		sq("gd-s1", "gardenio-nat-elimination-research-gardener-6zxh", "NAT gardener online (epic ka-0000007498). blocked on decision ka-0000003998", t0),
		{kind: feedKindClaimed, bead: &feedEventBead{ID: "gd-work"}, ts: t0.Add(time.Minute), count: 1},
		sq("gd-s2", "gardenio-nat-elimination-research-gardener-95dz", "NAT gardener online (epic ka-0000007498). research children CLOSED", t0.Add(2*time.Minute)),
		sq("gd-other", "gardenio-some-other-gardener-aaaa", "Other gardener (epic ka-0000007498) different agent framing", t0.Add(3*time.Minute)),
		sq("gd-s3", "gardenio-nat-elimination-research-gardener-owrb", "NAT gardener (epic ka-0000007498) run 3/12", t0.Add(4*time.Minute)),
	}
	out := collapseSquawkClusters(in)

	// gd-work + ONE collapsed cross-author framing cluster = 2 items.
	if len(out) != 2 {
		var ids []string
		for _, it := range out {
			ids = append(ids, it.bead.ID)
		}
		t.Fatalf("expected 2 items (work + 1 collapsed cross-author framing cluster), got %d: %v", len(out), ids)
	}

	var nat *feedCardItem
	for i := range out {
		if out[i].bead.ID == "gd-s3" {
			nat = &out[i]
		}
		if out[i].bead.ID == "gd-s1" || out[i].bead.ID == "gd-s2" || out[i].bead.ID == "gd-other" {
			t.Errorf("older framing %s should have folded away", out[i].bead.ID)
		}
	}
	if nat == nil {
		t.Fatalf("collapsed cluster should keep the newest framing gd-s3")
	}
	if nat.count != 4 {
		t.Errorf("collapsed cross-author cluster count = %d, want 4", nat.count)
	}
	if !nat.ts.Equal(t0.Add(4 * time.Minute)) {
		t.Errorf("collapsed cluster ts = %v, want newest (05:04)", nat.ts)
	}

	// SUBSTANTIVE squawks (naming a concrete delta) on ONE topic from DIFFERENT
	// gardeners each render — folding a real distinct status update is worse than
	// leaving a heartbeat (ka-0000004629). Same-author identical-outcome respawns
	// still fold.
	delta := []feedCardItem{
		sq("gd-d1", "gardenio-foo-gardener-aaaa", "epic gd-zzz: merged !4625, children closed", t0),
		sq("gd-d2", "gardenio-bar-gardener-bbbb", "epic gd-zzz: cut 2026.169.2, deployed prod", t0.Add(time.Minute)),
		sq("gd-d3", "gardenio-foo-gardener-cccc", "epic gd-zzz: merged !4625, children closed (respawn)", t0.Add(2*time.Minute)),
	}
	gotDelta := collapseSquawkClusters(delta)
	// gd-d1 and gd-d3 are the SAME logical author (gardenio-foo-gardener) → fold;
	// gd-d2 is a different gardener with a distinct delta → stays. = 2 items.
	if len(gotDelta) != 2 {
		var ids []string
		for _, it := range gotDelta {
			ids = append(ids, it.bead.ID)
		}
		t.Fatalf("substantive cross-author deltas: expected 2 (foo-gardener fold + distinct bar-gardener), got %d: %v", len(gotDelta), ids)
	}

	// A squawk naming NO topic is never clustered.
	noTopic := []feedCardItem{
		sq("gd-a", "gardenio-foo-gardener-aaaa", "release-owner: cut 2026.169.11", t0),
		sq("gd-b", "gardenio-foo-gardener-bbbb", "release-owner: cut 2026.169.12", t0.Add(time.Minute)),
	}
	if got := collapseSquawkClusters(noTopic); len(got) != 2 {
		t.Errorf("no-topic squawks must not cluster, got %d items", len(got))
	}

	// A nil-bead item and a single-item input short-circuit / pass through.
	if got := collapseSquawkClusters(in[:1]); len(got) != 1 {
		t.Errorf("single-item input should pass through, got %d", len(got))
	}
}

// TestCollectFeedCards_CollapsesSquawkCluster drives the durable-seed scan
// end-to-end with the live NAT-research failure mode: three gardener respawns
// each post a near-identical epic-status squawk (distinct message beads). The
// cluster pass folds them into ONE ×3 card; a genuinely-distinct squawk from a
// different gardener still renders on its own.
func TestCollectFeedCards_CollapsesSquawkCluster(t *testing.T) {
	fake := newFake()
	t0 := mustTime(t, "2026-06-15T05:00:00Z")
	fake.listGlobalEventsResp = &beadsapi.ListGlobalEventsResponse{
		Events: []beadsapi.Event{
			// Faithful to the live NAT-research squawks (ka-0000002345 et al.): each
			// names the epic ka-0000007498 AND carries the concrete "merged !4625"
			// outcome that earns it a card past the heartbeat/cycle-tick noise
			// filter — so the only thing that should collapse them is the cluster
			// pass under test.
			{ID: 1, Topic: "beads.bead.created", CreatedAt: t0,
				Payload: feedSquawkEventData(t, "gd-nat1", "gardenio-nat-elimination-research-gardener-6zxh",
					"NAT-elimination research gardener (epic ka-0000007498). Both research children CLOSED (merged !4625). Blocked on operator decision ka-0000003998")},
			{ID: 2, Topic: "beads.bead.created", CreatedAt: t0.Add(1 * time.Minute),
				Payload: feedSquawkEventData(t, "gd-rel", "gardenio-release-owner-gardener-zzzz",
					"release-owner: cut 2026.169.11, merged MR 4630")},
			{ID: 3, Topic: "beads.bead.created", CreatedAt: t0.Add(2 * time.Minute),
				Payload: feedSquawkEventData(t, "gd-nat2", "gardenio-nat-elimination-research-gardener-95dz",
					"NAT-elimination research gardener (epic ka-0000007498). NAT-free INFEASIBLE; children CLOSED (merged !4625). Blocked on operator decision ka-0000003998")},
			{ID: 4, Topic: "beads.bead.created", CreatedAt: t0.Add(3 * time.Minute),
				Payload: feedSquawkEventData(t, "gd-nat3", "gardenio-nat-elimination-research-gardener-owrb",
					"NAT-elimination gardener (epic ka-0000007498): edge-map ka-0000005663 DONE (merged !4625). Blocked on operator decision ka-0000003998")},
		},
	}
	srv := NewServer(fake, testLogger(), "", "", "", &fakeScreenClient{})

	cards := srv.collectFeedCards(context.Background(), t0.Add(-time.Hour), t0.Add(time.Hour), 30)

	// Expect 2 cards: the release-owner squawk and the single collapsed NAT ×3.
	if len(cards) != 2 {
		t.Fatalf("expected 2 cards (1 distinct squawk + 1 collapsed NAT cluster), got %d:\n%s", len(cards), strings.Join(cards, "\n"))
	}

	joined := strings.Join(cards, "")
	// The newest NAT squawk (gd-nat3) is the cluster representative, carrying ×3.
	var nat string
	for _, c := range cards {
		if strings.Contains(c, `data-bead-id="gd-nat3"`) {
			nat = c
		}
	}
	if nat == "" {
		t.Fatalf("collapsed cluster should keep the newest NAT squawk gd-nat3, got:\n%s", joined)
	}
	if !strings.Contains(nat, `class="feed-card-count"`) || !strings.Contains(nat, `×3`) {
		t.Errorf("collapsed NAT card should carry the ×3 badge, got:\n%s", nat)
	}
	// The two older NAT squawks folded away.
	if strings.Contains(joined, `data-bead-id="gd-nat1"`) || strings.Contains(joined, `data-bead-id="gd-nat2"`) {
		t.Errorf("older NAT squawks should have folded away, got:\n%s", joined)
	}
	// The release-owner squawk renders on its own, with no badge.
	if !strings.Contains(joined, `data-bead-id="gd-rel"`) {
		t.Errorf("distinct release-owner squawk should still render, got:\n%s", joined)
	}
	// Exactly one count badge across the whole render.
	if n := strings.Count(joined, `class="feed-card-count"`); n != 1 {
		t.Errorf("expected exactly 1 cluster badge across all cards, got %d", n)
	}
}
