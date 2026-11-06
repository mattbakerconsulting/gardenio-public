package bridge

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation"
	"gardenio/controller/internal/conversation/surfacetest"
	"gardenio/controller/internal/oidcauth"
	"gardenio/controller/internal/spawnguard"
)

func TestCommonsSpawnRate_FromEnv(t *testing.T) {
	for _, c := range []struct {
		env  string
		want int
	}{
		{"", commonsSpawnsPerHourDefault},
		{"junk", commonsSpawnsPerHourDefault},
		{"-1", commonsSpawnsPerHourDefault},
		{" 7 ", 7},
		{"0", 0},
	} {
		k := CommonsSpawnRate(func(string) string { return c.env })
		if k.Max() != c.want || k.Window() != time.Hour {
			t.Errorf("env %q: %d per %v, want %d per hour", c.env, k.Max(), k.Window(), c.want)
		}
	}
}

// landMessage is a Commons mention bead of land, in its general channel.
func landMessage(id, thread, who, land, text string) *beadsapi.BeadDetail {
	m := commonsMentionBead(id, thread, who, text)
	m.Labels = []string{"project:" + land}
	m.Fields["channel_id"] = "gd-chgen-" + land
	return m
}

// TestCommonsCompose_PerMemberSpawnRate is gd-qBvHX9jigO: a member of a
// tenant land may start only so many agents an hour by new @gardenio
// mentions. Over the cap the mention is refused with a 429 that says when it
// frees, and no head is minted. Another member, another land, a mention in a
// thread that has its head, and a verb are not counted; an operator land is
// exempt.
func TestCommonsCompose_PerMemberSpawnRate(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	b, daemon := newInboundActionBot(t, twin, "http://127.0.0.1:1")
	for _, land := range []string{"gardenio", "claimlayer"} {
		seedLandProject(daemon, land)
		daemon.beads["gd-proj-"+land].Fields["land_kind"] = "tenant"
		daemon.beads["gd-chgen-"+land] = &beadsapi.BeadDetail{
			ID: "gd-chgen-" + land, Type: "channel", Status: "open", Title: "general", Labels: []string{"project:" + land},
			Fields: map[string]string{"name": "general", "channel_kind": "general", "default_role": "crew"},
		}
	}
	rate := spawnguard.NewKeyed(1, time.Hour)
	clock := time.Date(2026, 10, 21, 6, 0, 0, 0, time.UTC)
	rate.SetNowFunc(func() time.Time { return clock })
	b.SetCommonsSpawnRate(rate)
	mux := http.NewServeMux()
	NewConversationInboundAPI(b, nil).RegisterRoutes(mux)

	mo := cardCaller{"mo@example.com", []string{oidcauth.RoleProjectMember}, []string{"/project-gardenio", "/project-claimlayer"}}
	ann := cardCaller{"ann@example.com", []string{oidcauth.RoleProjectMember}, []string{"/project-gardenio"}}
	compose := func(c cardCaller, m *beadsapi.BeadDetail) (int, string) {
		t.Helper()
		daemon.mu.Lock()
		daemon.beads[m.ID] = m
		daemon.mu.Unlock()
		rr := postCard(t, mux, c, `{"kind":"compose","message":"`+m.ID+`"}`)
		return rr.Code, rr.Body.String()
	}
	headOf := func(root string) *beadsapi.BeadDetail {
		daemon.mu.Lock()
		defer daemon.mu.Unlock()
		for _, d := range daemon.beads {
			if d.Type == "task" && d.Fields[fieldPromotedFrom] == root {
				return d
			}
		}
		return nil
	}

	if code, body := compose(mo, landMessage("gd-a1", "", "mo@example.com", "gardenio", "@gardenio first")); code != http.StatusOK {
		t.Fatalf("first spawn: %d %s", code, body)
	}
	head := headOf("gd-a1")
	if head == nil {
		t.Fatal("the first mention minted no head")
	}

	// Over the cap: refused, saying the limit and when it frees, with no head.
	clock = clock.Add(15 * time.Minute)
	agents := countAgentBeads(daemon)
	code, body := compose(mo, landMessage("gd-a2", "", "mo@example.com", "gardenio", "@gardenio second"))
	if code != http.StatusTooManyRequests ||
		!strings.Contains(body, "You have started 1 agent in gardenio in the last hour") ||
		!strings.Contains(body, "start another in 45 minutes") {
		t.Fatalf("over the cap: %d %s", code, body)
	}
	if headOf("gd-a2") != nil || countAgentBeads(daemon) != agents {
		t.Fatal("a refused mention minted a head or an agent")
	}

	// Not counted: a mention in the thread that has its head, and a verb.
	if code, body := compose(mo, landMessage("gd-a3", head.ID, "mo@example.com", "gardenio", "@gardenio and the other test?")); code != http.StatusOK {
		t.Fatalf("a mention in its own thread: %d %s", code, body)
	}
	if code, body := compose(mo, landMessage("gd-a4", "", "mo@example.com", "gardenio", "@gardenio stop")); code == http.StatusTooManyRequests {
		t.Fatalf("a verb was counted: %d %s", code, body)
	}

	// Another member of the land, and the same member in another land, have
	// their own slots.
	if code, body := compose(ann, landMessage("gd-b1", "", "ann@example.com", "gardenio", "@gardenio hers")); code != http.StatusOK || headOf("gd-b1") == nil {
		t.Fatalf("another member: %d %s", code, body)
	}
	if code, body := compose(mo, landMessage("gd-c1", "", "mo@example.com", "claimlayer", "@gardenio elsewhere")); code != http.StatusOK || headOf("gd-c1") == nil {
		t.Fatalf("another land: %d %s", code, body)
	}

	// The slot frees when the first spawn leaves the hour.
	clock = clock.Add(46 * time.Minute)
	if code, body := compose(mo, landMessage("gd-a5", "", "mo@example.com", "gardenio", "@gardenio later")); code != http.StatusOK || headOf("gd-a5") == nil {
		t.Fatalf("after the hour: %d %s", code, body)
	}

	// An operator land is exempt.
	daemon.mu.Lock()
	daemon.beads["gd-proj-gardenio"].Fields["land_kind"] = beadsapi.LandKindOperator
	daemon.mu.Unlock()
	if code, body := compose(mo, landMessage("gd-a6", "", "mo@example.com", "gardenio", "@gardenio operator land")); code != http.StatusOK || headOf("gd-a6") == nil {
		t.Fatalf("operator land: %d %s", code, body)
	}
}

// forgedHead is a bead a member may create in land (a task is in
// memberActions): labelled thread, so it reads as a wish head, and naming
// promotedFrom when it is not "".
func forgedHead(id, land, promotedFrom string) *beadsapi.BeadDetail {
	f := map[string]string{"channel_id": "gd-chgen-" + land}
	if promotedFrom != "" {
		f[fieldPromotedFrom] = promotedFrom
	}
	return &beadsapi.BeadDetail{
		ID: id, Type: "task", Status: "open", Title: "not the bridge's",
		Labels: []string{threadKindLabel, "project:" + land}, Fields: f,
	}
}

// TestCommonsCompose_SpawnRateHoldsUnderAForgedHead is gd-PTOUU1ufzq: the
// per-member rate counts every mention that starts a fresh agent, not only one
// that promotes a chat root. A member over the cap who roots a mention in a
// head of their own making, or under a message such a head names as its
// promoted_from, is refused with no agent; another member under the cap
// starts one the same way, so the forged head would otherwise spawn.
func TestCommonsCompose_SpawnRateHoldsUnderAForgedHead(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	b, daemon := newInboundActionBot(t, twin, "http://127.0.0.1:1")
	seedLandProject(daemon, "gardenio")
	daemon.beads["gd-proj-gardenio"].Fields["land_kind"] = "tenant"
	daemon.beads["gd-chgen-gardenio"] = &beadsapi.BeadDetail{
		ID: "gd-chgen-gardenio", Type: "channel", Status: "open", Title: "general", Labels: []string{"project:gardenio"},
		Fields: map[string]string{"name": "general", "channel_kind": "general", "default_role": "crew"},
	}
	rate := spawnguard.NewKeyed(1, time.Hour)
	clock := time.Date(2026, 11, 2, 19, 0, 0, 0, time.UTC)
	rate.SetNowFunc(func() time.Time { return clock })
	b.SetCommonsSpawnRate(rate)
	mux := http.NewServeMux()
	NewConversationInboundAPI(b, nil).RegisterRoutes(mux)

	mo := cardCaller{"mo@example.com", []string{oidcauth.RoleProjectMember}, []string{"/project-gardenio"}}
	ann := cardCaller{"ann@example.com", []string{oidcauth.RoleProjectMember}, []string{"/project-gardenio"}}
	seed := func(ds ...*beadsapi.BeadDetail) {
		daemon.mu.Lock()
		defer daemon.mu.Unlock()
		for _, d := range ds {
			daemon.beads[d.ID] = d
		}
	}
	compose := func(c cardCaller, m *beadsapi.BeadDetail) (int, string) {
		t.Helper()
		seed(m)
		rr := postCard(t, mux, c, `{"kind":"compose","message":"`+m.ID+`"}`)
		return rr.Code, rr.Body.String()
	}

	if code, body := compose(mo, landMessage("gd-a1", "", "mo@example.com", "gardenio", "@gardenio first")); code != http.StatusOK {
		t.Fatalf("first spawn: %d %s", code, body)
	}
	clock = clock.Add(10 * time.Minute)

	// A head of mo's own making as the thread's root.
	seed(forgedHead("gd-fake1", "gardenio", ""))
	agents := countAgentBeads(daemon)
	code, body := compose(mo, landMessage("gd-a2", "gd-fake1", "mo@example.com", "gardenio", "@gardenio go"))
	if code != http.StatusTooManyRequests || !strings.Contains(body, "You have started 1 agent in gardenio") {
		t.Fatalf("a mention under a forged head over the cap: %d %s", code, body)
	}
	if countAgentBeads(daemon) != agents {
		t.Fatal("a mention under a forged head over the cap started an agent")
	}

	// A chat root that a forged head names as its promoted_from.
	seed(landMessage("gd-m1", "", "mo@example.com", "gardenio", "a plain message"), forgedHead("gd-fake2", "gardenio", "gd-m1"))
	code, body = compose(mo, landMessage("gd-a3", "gd-m1", "mo@example.com", "gardenio", "@gardenio go"))
	if code != http.StatusTooManyRequests || countAgentBeads(daemon) != agents {
		t.Fatalf("a mention under a root a forged head names, over the cap: %d %s", code, body)
	}

	// Control: under her cap, ann starts an agent under a forged head.
	seed(forgedHead("gd-fake3", "gardenio", ""))
	if code, body := compose(ann, landMessage("gd-b1", "gd-fake3", "ann@example.com", "gardenio", "@gardenio go")); code != http.StatusOK {
		t.Fatalf("a mention under a forged head under the cap: %d %s", code, body)
	}
	if countAgentBeads(daemon) != agents+1 {
		t.Fatal("a mention under a forged head under the cap started no agent; the over-cap cases prove nothing")
	}
}

// TestCommonsReply_SpawnRateHoldsUnderAForgedHead is gd-PTOUU1ufzq for a
// reply that may spawn: under a head with a Slack copy and no agent bound,
// SpawnOrRespawnForViewerReply starts a fresh one, which takes a slot of the
// person's rate as a mention's does.
func TestCommonsReply_SpawnRateHoldsUnderAForgedHead(t *testing.T) {
	b, daemon, _, _ := newCommonsReplyBot(t)
	daemon.seedProjectWithChannel("gardenio", "C0GONE")
	daemon.beads["proj-gardenio"].Fields["land_kind"] = "tenant"
	b.SetCommonsSpawnRate(spawnguard.NewKeyed(1, time.Hour))
	reply := func(id, head, ts string) error {
		t.Helper()
		h := forgedHead(head, "gardenio", "")
		h.Fields["slack_thread_channel"], h.Fields["slack_thread_ts"] = "C0GONE", ts
		daemon.mu.Lock()
		daemon.beads[head] = h
		daemon.beads[id] = commonsReplyBead(id, head, "ada@example.com", time.Time{})
		daemon.mu.Unlock()
		_, dispatched, err := b.dispatchCommonsAs(context.Background(), commonsMessageFromDetail(daemon.beads[id]), conversation.InboundReply, true)
		if !dispatched {
			t.Fatalf("%s was not dispatched", id)
		}
		return err
	}

	agents := countAgentBeads(daemon)
	if err := reply("gd-r1", "gd-fake-r1", "1690000000.000901"); err != nil {
		t.Fatalf("a reply under the cap: %v", err)
	}
	if countAgentBeads(daemon) != agents+1 {
		t.Fatal("a reply under a forged head with a Slack copy started no agent; the over-cap case proves nothing")
	}
	err := reply("gd-r2", "gd-fake-r2", "1690000000.000902")
	var refusal *InboundRefusal
	if !errors.As(err, &refusal) || refusal.Status != http.StatusTooManyRequests {
		t.Fatalf("a reply over the cap: %v, want a 429", err)
	}
	if countAgentBeads(daemon) != agents+1 {
		t.Fatal("a reply over the cap started an agent")
	}
}
