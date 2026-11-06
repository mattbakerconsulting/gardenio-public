package bridge

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/conversation"
	"gardenio/controller/internal/spawnguard"
)

// The Commons per-member spawn rate (gd-qBvHX9jigO, item 4 of the #784
// review): every new top-level @gardenio mention promotes a wish head and
// starts an agent, so without it one member could start any number. The
// controller's per-land rate (LANDS_SPAWNS_PER_HOUR) still catches a tenant
// land's spawns at the pod door, but one member could spend the whole land's
// hour and their bead would wait there with no answer. This cap is per member
// of a land, is checked before the head is minted, and answers in the Commons.
//
// Operator lands are exempt, as they are from the per-land rate: their members
// are the operator's own crew.
const (
	commonsSpawnsPerHourEnv     = "COMMONS_SPAWNS_PER_HOUR_PER_MEMBER"
	commonsSpawnsPerHourDefault = 4
)

// CommonsSpawnRate is the Commons per-member spawn rate from getenv
// (COMMONS_SPAWNS_PER_HOUR_PER_MEMBER). Unset or unparseable is
// commonsSpawnsPerHourDefault; 0 turns the cap off.
func CommonsSpawnRate(getenv func(string) string) *spawnguard.Keyed {
	n := commonsSpawnsPerHourDefault
	if v, err := strconv.Atoi(strings.TrimSpace(getenv(commonsSpawnsPerHourEnv))); err == nil && v >= 0 {
		n = v
	}
	return spawnguard.NewKeyed(n, time.Hour)
}

// SetCommonsSpawnRate sets the Commons per-member spawn rate. nil, the
// default, admits every spawn.
func (b *Bot) SetCommonsSpawnRate(k *spawnguard.Keyed) {
	b.commonsSpawnRate = k
}

// admitCommonsSpawn takes a slot of the person's spawn rate in in.Land for a
// mention or reply that is about to start a fresh agent in the thread rooted
// at rootID (promoting the root to a wish head first, or under a head found),
// or refuses it with a 429 that says the limit and when it frees. The slot is
// keyed by root, so a retried mention of the same root takes no second one.
// A verb (kill, stop, restart, clear) starts no agent and is not counted.
func (b *Bot) admitCommonsSpawn(ctx context.Context, in conversation.Inbound, rootID string) error {
	rate := b.commonsSpawnRate
	if rate.Max() <= 0 || rate.Window() <= 0 {
		return nil
	}
	if verb, _ := commonsVerb(in.Text); verb != "" {
		return nil
	}
	// A land the daemon cannot read is capped: counting a member who is
	// under the cap costs nothing.
	if kind, err := b.tenantKind(ctx, in.Land); err == nil && kind == "" {
		return nil
	}
	key := in.Land + "/" + in.Actor.ID
	if ok, _ := rate.Allow(key, rootID); ok {
		return nil
	}
	b.logger.Info("compose: refused a Commons spawn over the member's rate",
		"land", in.Land, "actor", in.Actor.ID, "root", rootID, "max", rate.Max())
	return inboundRefuse(http.StatusTooManyRequests,
		"You have started %s in %s in the last %s, the most one person may. You can start another in %s.",
		countOf(rate.Max(), "agent"), in.Land, rateWindowWords(rate.Window()), waitWords(rate.RetryAfter(key)))
}

// countOf is n and noun, with an s unless n is 1.
func countOf(n int, noun string) string {
	return fmt.Sprintf("%d %s%s", n, noun, plural(n))
}

// rateWindowWords is a rate window as a person reads it: "hour" for one hour.
func rateWindowWords(d time.Duration) string {
	if d == time.Hour {
		return "hour"
	}
	return waitWords(d)
}

// waitWords is a wait rounded up to whole minutes, at least one.
func waitWords(d time.Duration) string {
	m := int((d + time.Minute - 1) / time.Minute)
	if m < 1 {
		m = 1
	}
	return countOf(m, "minute")
}
