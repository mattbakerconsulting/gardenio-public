// Package watcher is the Lands purse watcher (Lands Track B spec
// gardenio/docs/superpowers/specs/2026-10-19-lands-track-b-design.md §3.4 and
// §3.5, step 8.4): the controller loop that acts on a tenant land's purse
// (internal/lands/purse) once a minute.
//
//   - Parking. A land at the wall has its running agents parked: a nudge
//     ("the purse is empty, commit and push what you have, then stop"), a
//     5-minute grace, then the suspend path (agent_state=stopping, as gb stop
//     writes it), so the session resumes if credits arrive. Each parked agent
//     carries park_reason=purse_empty for the reason model (gd-TvJosTKfAt).
//     A nudged agent whose land is no longer at the wall is told to carry on.
//     An unknown purse parks nothing: unknown refuses new spawns (step 8.3),
//     but running agents continue.
//   - Freezing. A frozen land (land_frozen, the per-land kill switch of §5.5,
//     gb lands freeze) has every running agent parked the same way, purse or
//     no purse, with park_reason=land_frozen. A thaw inside the grace calls
//     the suspend off; agents already suspended stay parked. After the grace
//     the land's LiteLLM key is blocked, and a thaw unblocks it (freezeKey).
//   - Notices. A line in the land's #general at 50, 80 and 100 percent used,
//     and mail to the owner at 80 (purse low) and 100 (purse empty). 100 is
//     the wall, which the margin puts a little before every credit is spent.
//     Each level is said once; a refill re-arms the levels it falls below.
//   - Refill. A rise in the land's granted credits (a credit grant, gb lands
//     credit add) posts "purse refilled" in #general and calls the steward
//     hook. The steward itself wakes in trigger-engine, on the credit_grant
//     bead (internal/steward), and decides what to resume (§4.3); the hook is
//     a seam for anything the watcher must do beside that. Nothing resumes on
//     its own.
//   - The steward parks (step 9.7, §4.2 item 5). At the wall the steward
//     cannot run (the spawn wall refuses it), so the watcher has the steward
//     hook say its empty-purse line once and records the park on the
//     account bead (steward_park_reason=purse_empty) until the purse has
//     credit again. The steward sleeps until the credit grant wakes it.
//   - Snapshot. The purse's figures and state, stamped on the land's account
//     bead when they change and every 5 minutes otherwise, for the daemon's
//     Lands API to serve as the land's Budget (D.6c).
//
// Which agents run on a land is the controller's own view, never the agent
// beads': the pods in the land's namespace, each mapped to its bead by the
// bead-id annotation the controller stamped at create (gd-Nc7yMpW7U6). An
// agent writes its own bead, so a park chosen by agent_state, the agent field
// or the project label could be dodged by the agent it parks.
//
// What the watcher has said and done is kept on beads, so a controller restart
// neither repeats nor forgets it: the notice level and the granted credits
// last seen on the land's lands-<land> account bead, and the nudge time and
// park reason on each agent bead. Notice and refill posts also carry an
// idempotency key, so a post whose bookkeeping write failed is not doubled.
package watcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/lands/landns"
	"gardenio/controller/internal/lands/purse"
	"gardenio/controller/internal/mailer"
	"gardenio/controller/internal/podmanager"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

const (
	// Interval is how often the watcher looks at every tenant land's purse.
	Interval = time.Minute
	// Grace is how long a nudged agent has to push its work before it is
	// suspended.
	Grace = 5 * time.Minute

	// Account bead fields (on lands-<land>): the highest notice level said
	// since the last refill, and the granted credits the watcher last saw.
	FieldNoticeLevel = "purse_notice_level"
	FieldGrantedSeen = "purse_granted_seen"
	// The purse snapshot, also on lands-<land>, which the daemon's Lands API
	// serves as the land's Budget (D.6c, gd-uAiEaCwsJJ): the daemon cannot
	// import this module's purse arithmetic, so it reads these instead.
	// Granted and used (spent + in flight) credits, used empty when spend
	// cannot be computed; the purse state (ok, wall or unknown); and when the
	// purse was read (RFC3339).
	FieldGrantedCredits = "purse_granted_credits"
	FieldUsedCredits    = "purse_used_credits"
	FieldState          = "purse_state"
	FieldReadAt         = "purse_read_at"
	// FieldStewardPark, also on lands-<land>, is why the land's steward is
	// parked (ParkReason at the wall), or empty while it may run: set once
	// the steward hook has said its empty-purse line, cleared when the purse
	// has credit again.
	FieldStewardPark = "steward_park_reason"
	// FieldKeyBlocked, also on lands-<land>, is "true" while the watcher
	// holds the land's LiteLLM key blocked for a freeze (freezeKey), so a
	// thaw lifts only the watcher's own block.
	FieldKeyBlocked = "land_key_blocked"
	// SnapshotHeartbeat is how old purse_read_at may grow before an unchanged
	// snapshot is rewritten: the spend sync's cadence. Each write is an update
	// event on the account bead, which the land feed's budget.updated reads,
	// so an unchanged purse is not rewritten every pass.
	SnapshotHeartbeat = 5 * time.Minute
	// Agent bead fields: when the purse-empty nudge was sent (RFC3339), and
	// why the agent was parked.
	FieldNudgedAt   = "purse_nudged_at"
	FieldParkReason = "park_reason"
	// ParkReason is the reason-model kind an agent parked at the wall
	// carries, and ParkFrozen the one an agent of a frozen land carries.
	ParkReason = "purse_empty"
	ParkFrozen = "land_frozen"

	// Author signs the watcher's #general posts and agent comments.
	Author        = "agent:gardenio-purse"
	CommentAuthor = "gardenio-controller"

	// OriginNotice and OriginRefill are the origin field of the watcher's
	// threshold and refill posts, so a land's steward trigger can wake on a
	// threshold without also waking on the refill post a credit grant already
	// wakes it for (internal/steward, spec §4.3).
	OriginNotice = "purse_notice"
	OriginRefill = "purse_refill"
)

// Notice levels, in percent of granted credits used.
const (
	LevelNone  = 0
	LevelHalf  = 50
	LevelLow   = 80
	LevelEmpty = 100
)

// liveStatuses are the bead statuses of an agent that may still be running.
var liveStatuses = []string{"open", "in_progress", "blocked", "deferred"}

// Daemon is the slice of the daemon client the watcher reads and writes
// through. *beadsapi.Client satisfies it.
type Daemon interface {
	purse.Source
	GetBead(ctx context.Context, beadID string) (*beadsapi.BeadDetail, error)
	CreateBead(ctx context.Context, req beadsapi.CreateBeadRequest) (string, error)
	UpdateBeadFields(ctx context.Context, beadID string, fields map[string]string) error
	AddComment(ctx context.Context, beadID, author, text string) error
}

// Pods lists agent pods. podmanager.Manager satisfies it.
type Pods interface {
	ListAgentPods(ctx context.Context, namespace string, labelSelector map[string]string) ([]corev1.Pod, error)
}

// Keys blocks and unblocks a land's LiteLLM key (provision.LiteLLM in
// production). A blocked key refuses every call and keeps its max_budget and
// spend. A key LiteLLM does not know is neither an error to block nor to
// unblock.
type Keys interface {
	Block(ctx context.Context, key string) error
	Unblock(ctx context.Context, key string) error
}

// Nudger delivers a message to a running agent by name.
type Nudger interface {
	Nudge(ctx context.Context, agentName, message string) error
}

// Steward is the land's steward as the watcher drives it (internal/steward
// Hook in production).
type Steward interface {
	// PurseEmpty is called when the land's purse is at the wall and its
	// steward is not parked yet: the steward cannot run there, so the hook
	// says the steward's empty-purse line for it. A nil return parks the
	// steward (steward_park_reason); an error is retried on the next pass,
	// so the hook must be idempotent.
	PurseEmpty(ctx context.Context, land string, p purse.Purse) error
	// PurseRefilled is called on a rise in granted credits. The land's
	// steward is woken by its trigger on the credit_grant bead
	// (internal/steward, §4.3), not through here.
	PurseRefilled(ctx context.Context, land string, p purse.Purse) error
}

// NoSteward is the steward hook that does nothing.
type NoSteward struct{}

// PurseEmpty does nothing.
func (NoSteward) PurseEmpty(context.Context, string, purse.Purse) error { return nil }

// PurseRefilled does nothing.
func (NoSteward) PurseRefilled(context.Context, string, purse.Purse) error { return nil }

// Watcher is the purse watcher. Daemon and Pods are required (with no Pods
// nothing is parked, and each land's pass reports it); a nil Nudger skips the
// nudge (the grace still runs), a nil or disabled Mailer sends no mail, a nil
// Steward is NoSteward, and a nil Keys leaves a frozen land's key alone.
type Watcher struct {
	Daemon    Daemon
	Pods      Pods
	Keys      Keys
	Nudger    Nudger
	Mailer    mailer.Mailer
	Templates *mailer.Templates
	Steward   Steward
	Logger    *slog.Logger
	// Now and Grace default to time.Now and the package Grace.
	Now   func() time.Time
	Grace time.Duration
}

// Run passes every interval until ctx is cancelled, with one pass on start.
func (w *Watcher) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = Interval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := w.Pass(ctx); err != nil {
			w.logger().Warn("purse watcher: pass failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (w *Watcher) logger() *slog.Logger {
	if w.Logger == nil {
		return slog.Default()
	}
	return w.Logger
}

func (w *Watcher) now() time.Time {
	if w.Now == nil {
		return time.Now()
	}
	return w.Now()
}

func (w *Watcher) grace() time.Duration {
	if w.Grace <= 0 {
		return Grace
	}
	return w.Grace
}

func (w *Watcher) steward() Steward {
	if w.Steward == nil {
		return NoSteward{}
	}
	return w.Steward
}

// Pass looks at every tenant land once. One land's failure is logged and
// does not stop the others; the error is a failed read of the lands
// themselves, or of the purses or accounts. A failed purse read leaves every
// purse unknown, which parks nothing on a land that is not frozen, and a
// failed account read skips the notices: neither stops a frozen land being
// parked, since the freeze is read off the project bead alone.
func (w *Watcher) Pass(ctx context.Context) error {
	projects, err := purse.TenantProjects(ctx, w.Daemon)
	if err != nil {
		return fmt.Errorf("reading tenant lands: %w", err)
	}
	if len(projects) == 0 {
		return nil
	}
	names := make([]string, 0, len(projects))
	for name := range projects {
		names = append(names, name)
	}
	sort.Strings(names)
	now := w.now()
	var errs []error
	purses, err := purse.ReadMany(ctx, w.Daemon, names, now)
	if err != nil {
		err = fmt.Errorf("reading purses: %w", err)
		errs = append(errs, err)
	}
	accounts, aerr := w.accounts(ctx)
	if aerr != nil {
		errs = append(errs, aerr)
	}
	for _, name := range names {
		p, ok := purses[name]
		if !ok {
			p = purse.Unreadable(name, err, now)
		}
		l := land{name: name, project: projects[name], purse: p, account: accounts[purse.AccountName(name)]}
		if err := w.land(ctx, l, now); err != nil {
			w.logger().Warn("purse watcher: land failed", "land", name, "error", err)
		}
	}
	return errors.Join(errs...)
}

// land is one tenant land as a pass sees it.
type land struct {
	name    string
	project *beadsapi.BeadDetail
	purse   purse.Purse
	// account is the lands-<land> account bead, nil when there is none.
	account *beadsapi.BeadDetail
}

// frozen reports whether the land's kill switch is on (land_frozen).
func (l land) frozen() bool {
	return l.project != nil && strings.TrimSpace(l.project.Fields["land_frozen"]) == "true"
}

// parkCause is why the land's running agents are parked now: ParkFrozen for
// a frozen land, ParkReason at the wall, "" when they may run. hold is true
// when the purse is unknown on a land that is not frozen: nothing is parked,
// and nothing pending is called off either.
func (l land) parkCause() (cause string, hold bool) {
	switch {
	case l.frozen():
		return ParkFrozen, false
	case l.purse.State == purse.StateUnknown:
		return "", true
	case l.purse.State == purse.StateWall:
		return ParkReason, false
	}
	return "", false
}

func (w *Watcher) land(ctx context.Context, l land, now time.Time) error {
	var errs []string
	if l.account != nil {
		if err := w.ledger(ctx, l, now); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if err := w.park(ctx, l, now); err != nil {
		errs = append(errs, err.Error())
	}
	if err := w.freezeKey(ctx, l, now); err != nil {
		errs = append(errs, err.Error())
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// freezeKey blocks a frozen land's LiteLLM key once the freeze's grace is
// over (gd-nHh7M0k3ps), so the land spends nothing more of its credit until
// it is thawed, and unblocks it on the thaw. The grace leaves the land's
// agents the model calls to push their work before park suspends them; a
// freeze with no land_frozen_at blocks at once. Only the block is touched,
// so a credit grant during the freeze cannot reopen the key.
func (w *Watcher) freezeKey(ctx context.Context, l land, now time.Time) error {
	if w.Keys == nil || l.account == nil {
		return nil
	}
	key := strings.TrimSpace(l.account.Fields["litellm_key"])
	blocked := strings.TrimSpace(l.account.Fields[FieldKeyBlocked]) == "true"
	switch {
	case key == "":
		return nil
	case l.frozen() && !blocked:
		at, err := time.Parse(time.RFC3339, strings.TrimSpace(l.project.Fields["land_frozen_at"]))
		if err == nil && now.Sub(at) < w.grace() {
			return nil
		}
		if err := w.Keys.Block(ctx, key); err != nil {
			return fmt.Errorf("blocking %s's LiteLLM key: %w", l.name, err)
		}
		w.logger().Info("purse watcher: frozen land's key blocked", "land", l.name)
		return w.markKey(ctx, l, "true")
	case !l.frozen() && blocked:
		if err := w.Keys.Unblock(ctx, key); err != nil {
			return fmt.Errorf("unblocking %s's LiteLLM key: %w", l.name, err)
		}
		w.logger().Info("purse watcher: thawed land's key unblocked", "land", l.name)
		return w.markKey(ctx, l, "")
	}
	return nil
}

func (w *Watcher) markKey(ctx context.Context, l land, v string) error {
	if err := w.Daemon.UpdateBeadFields(ctx, l.account.ID, map[string]string{FieldKeyBlocked: v}); err != nil {
		return fmt.Errorf("recording %s's key block on %s: %w", l.name, l.account.Title, err)
	}
	return nil
}

// ledger posts the refill and threshold notices and records what was said,
// and the purse snapshot, on the account bead.
func (w *Watcher) ledger(ctx context.Context, l land, now time.Time) error {
	p, f := l.purse, l.account.Fields
	update := map[string]string{}
	stored := purse.FieldInt(f, FieldNoticeLevel)

	seenRaw := strings.TrimSpace(f[FieldGrantedSeen])
	seen := purse.FieldFloat(f, FieldGrantedSeen)
	switch {
	case seenRaw == "":
		// First sight of the land: nothing to compare a refill against.
		update[FieldGrantedSeen] = credits(p.GrantedCredits)
	case p.GrantedCredits > seen+0.005:
		if err := w.refilled(ctx, l, p.GrantedCredits-seen); err != nil {
			return err
		}
		update[FieldGrantedSeen] = credits(p.GrantedCredits)
	case p.GrantedCredits < seen-0.005:
		// A negative grant (a refund or a correction) is not a refill.
		update[FieldGrantedSeen] = credits(p.GrantedCredits)
	}

	if p.State != purse.StateUnknown {
		switch level := Level(p); {
		case level > stored:
			if err := w.notice(ctx, l, level, stored); err != nil {
				return err
			}
			update[FieldNoticeLevel] = strconv.Itoa(level)
		case level < stored:
			// A refill: re-arm the levels the purse is below again.
			update[FieldNoticeLevel] = strconv.Itoa(level)
		}
	}
	w.parkSteward(ctx, l, update)
	snapshot(p, f, now, update)
	if len(update) == 0 {
		return nil
	}
	if err := w.Daemon.UpdateBeadFields(ctx, l.account.ID, update); err != nil {
		return fmt.Errorf("recording the purse watcher's state on %s: %w", l.account.Title, err)
	}
	return nil
}

// parkSteward parks the land's steward at the wall, once, and unparks it when
// the purse has credit again (the credit grant's own wake is what runs it).
// An unknown purse changes nothing.
func (w *Watcher) parkSteward(ctx context.Context, l land, update map[string]string) {
	parked := strings.TrimSpace(l.account.Fields[FieldStewardPark]) != ""
	switch {
	case l.purse.State == purse.StateWall && !parked:
		if err := w.steward().PurseEmpty(ctx, l.name, l.purse); err != nil {
			w.logger().Warn("purse watcher: parking the steward failed, retrying next pass", "land", l.name, "error", err)
			return
		}
		update[FieldStewardPark] = ParkReason
		w.logger().Info("purse watcher: steward parked", "land", l.name)
	case l.purse.State == purse.StateOK && parked:
		update[FieldStewardPark] = ""
		w.logger().Info("purse watcher: steward unparked", "land", l.name)
	}
}

// snapshot adds the purse snapshot to update when a figure has changed (at
// two decimals, as credits formats it) or purse_read_at is SnapshotHeartbeat
// old, and leaves update alone otherwise.
func snapshot(p purse.Purse, f map[string]string, now time.Time, update map[string]string) {
	want := map[string]string{
		FieldGrantedCredits: credits(p.GrantedCredits),
		FieldUsedCredits:    "",
		FieldState:          p.State,
	}
	if p.SpentCredits != nil {
		used := *p.SpentCredits
		if p.InFlightCredits != nil {
			used += *p.InFlightCredits
		}
		want[FieldUsedCredits] = credits(used)
	}
	changed := false
	for k, v := range want {
		changed = changed || strings.TrimSpace(f[k]) != v
	}
	readAt, err := time.Parse(time.RFC3339, strings.TrimSpace(f[FieldReadAt]))
	if !changed && err == nil && now.Sub(readAt) < SnapshotHeartbeat {
		return
	}
	maps.Copy(update, want)
	update[FieldReadAt] = now.UTC().Format(time.RFC3339)
}

// Level is the notice level a purse has reached: 100 at the wall, else the
// highest of 80 and 50 its used percentage has crossed.
func Level(p purse.Purse) int {
	if p.State == purse.StateWall {
		return LevelEmpty
	}
	if p.UsedPct == nil {
		return LevelNone
	}
	switch used := *p.UsedPct; {
	case used >= LevelEmpty:
		return LevelEmpty
	case used >= LevelLow:
		return LevelLow
	case used >= LevelHalf:
		return LevelHalf
	}
	return LevelNone
}

// notice says one threshold in #general, and mails the owner when it crosses
// 80 or 100. A land with no #general channel yet still records the level, so
// it is not retried every minute; any other failed post is retried.
func (w *Watcher) notice(ctx context.Context, l land, level, stored int) error {
	p := l.purse
	text := NoticeText(l.name, level, p)
	key := fmt.Sprintf("purse-notice:%s:%d:%s", l.name, level, credits(p.GrantedCredits))
	if err := w.post(ctx, l.name, text, key, OriginNotice); err != nil && !isNoChannel(err) {
		return err
	}
	w.logger().Info("purse watcher: notice", "land", l.name, "level", level, "used_pct", p.UsedPct, "state", p.State)
	switch {
	case level >= LevelEmpty && stored < LevelEmpty:
		w.mail(ctx, l, mailer.KindPurseEmpty)
	case level >= LevelLow && stored < LevelLow:
		w.mail(ctx, l, mailer.KindPurseLow)
	}
	return nil
}

// refilled posts "purse refilled" and wakes the steward.
func (w *Watcher) refilled(ctx context.Context, l land, added float64) error {
	p := l.purse
	key := fmt.Sprintf("purse-refill:%s:%s", l.name, credits(p.GrantedCredits))
	if err := w.post(ctx, l.name, RefillText(l.name, added, p), key, OriginRefill); err != nil && !isNoChannel(err) {
		return err
	}
	w.logger().Info("purse watcher: purse refilled", "land", l.name, "added_credits", added, "granted_credits", p.GrantedCredits)
	if err := w.steward().PurseRefilled(ctx, l.name, p); err != nil {
		w.logger().Warn("purse watcher: waking the steward failed", "land", l.name, "error", err)
	}
	return nil
}

// NoticeText is the #general line for a notice level.
func NoticeText(land string, level int, p purse.Purse) string {
	left := "?"
	if p.RemainingCredits != nil {
		left = credits(math.Max(0, *p.RemainingCredits))
	}
	granted := credits(p.GrantedCredits)
	switch level {
	case LevelEmpty:
		return fmt.Sprintf("The purse for %s is empty (%s of %s credits left). No new agents will start, and running agents have been asked to push their work and pause. When credit is added, the steward decides what to pick up again.", land, left, granted)
	case LevelLow:
		return fmt.Sprintf("The purse for %s is running low: %s of %s credits left. When it runs out, no new agents start and running agents pause.", land, left, granted)
	}
	return fmt.Sprintf("The purse for %s is half spent: %s of %s credits left.", land, left, granted)
}

// RefillText is the #general line for a refill.
func RefillText(land string, added float64, p purse.Purse) string {
	left := ""
	if p.RemainingCredits != nil {
		left = fmt.Sprintf(", %s of %s credits left", credits(math.Max(0, *p.RemainingCredits)), credits(p.GrantedCredits))
	}
	return fmt.Sprintf("The purse for %s was refilled: %s credits added%s.", land, credits(added), left)
}

// NudgeText is the nudge a running agent gets when its land hits the wall.
func NudgeText(land string, grace time.Duration) string {
	return fmt.Sprintf("The purse for land %s is empty. Commit and push what you have now, then stop (gb stop). In %s this agent is suspended; its session resumes if credits arrive.", land, grace)
}

// FrozenNudgeText is the nudge a running agent gets when its land is frozen.
func FrozenNudgeText(land string, grace time.Duration) string {
	return fmt.Sprintf("Land %s has been frozen by the operator. Commit and push what you have now, then stop (gb stop). In %s this agent is suspended; its session can resume when the land is thawed.", land, grace)
}

// CarryOnText is the nudge a nudged agent gets when the wall or the freeze
// lifts first.
func CarryOnText(land string) string {
	return fmt.Sprintf("Land %s can run again: its purse has credit and it is not frozen. Carry on with your task.", land)
}

// credits formats a credit figure with at most two decimals.
func credits(v float64) string {
	return strconv.FormatFloat(math.Round(v*100)/100, 'f', -1, 64)
}

// errNoChannel: the land has no #general channel bead (its Commons is not
// seeded yet).
type errNoChannel struct{ land string }

func (e errNoChannel) Error() string { return fmt.Sprintf("land %q has no #general channel", e.land) }

func isNoChannel(err error) bool {
	_, ok := err.(errNoChannel)
	return ok
}

// post writes a notice message bead in the land's #general. A tenant land
// never mirrors to Slack (B-D11), so the bead is the whole delivery: the
// Commons shows it. key makes the create idempotent; origin tells a threshold
// post from a refill post.
func (w *Watcher) post(ctx context.Context, land, text, key, origin string) error {
	ch, err := w.general(ctx, land)
	if err != nil {
		return err
	}
	fields, _ := json.Marshal(map[string]string{ // a map[string]string always marshals
		"channel_id": ch,
		"kind":       "notice",
		"author":     Author,
		"text":       text,
		"origin":     origin,
	})
	title := text
	if len(title) > 200 {
		title = title[:197] + "..."
	}
	// The daemon lets a caller name only itself as a message's author unless it
	// is a bridge or a machine principal (kbeads message_author.go). On the
	// machine principal it may name agent:gardenio-purse; on the static token
	// X-Actor makes the watcher gardenio-purse, so the author names its writer.
	_, err = w.Daemon.CreateBead(beadsapi.WithActor(ctx, strings.TrimPrefix(Author, "agent:")), beadsapi.CreateBeadRequest{
		Title:          title,
		Type:           "message",
		Kind:           "data",
		Status:         "closed",
		Labels:         []string{"project:" + land},
		CreatedBy:      Author,
		Fields:         fields,
		IdempotencyKey: key,
	})
	if err != nil {
		return fmt.Errorf("posting in %s's #general: %w", land, err)
	}
	return nil
}

// general is the id of the land's open #general channel bead.
func (w *Watcher) general(ctx context.Context, land string) (string, error) {
	res, err := w.Daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"channel"},
		Labels:   []string{"project:" + land},
		Statuses: liveStatuses,
		Fields:   map[string]string{"channel_kind": "general"},
	})
	if err != nil {
		return "", fmt.Errorf("finding %s's #general: %w", land, err)
	}
	var pick *beadsapi.BeadDetail
	for _, b := range res.Beads {
		if b == nil || b.Status == "closed" || b.Fields["channel_kind"] != "general" {
			continue
		}
		if pick == nil || (b.Fields["name"] == "general" && pick.Fields["name"] != "general") || (b.Fields["name"] == pick.Fields["name"] && b.ID < pick.ID) {
			pick = b
		}
	}
	if pick == nil {
		return "", errNoChannel{land}
	}
	return pick.ID, nil
}

// mail sends k to the land's owner: the applicant on the plot application the
// land came from. Mail is best effort: a failure is logged, never retried, so
// the #general line stays the one record of the notice.
func (w *Watcher) mail(ctx context.Context, l land, k mailer.Kind) {
	if !mailer.Enabled(w.Mailer) || w.Templates == nil {
		return
	}
	log := w.logger().With("land", l.name, "kind", k)
	appID := ""
	if l.project != nil {
		appID = strings.TrimSpace(l.project.Fields["land_application_id"])
	}
	if appID == "" {
		log.Warn("purse watcher: no mail, the land names no plot application")
		return
	}
	app, err := w.Daemon.GetBead(ctx, appID)
	if err != nil || app == nil {
		log.Warn("purse watcher: no mail, the plot application cannot be read", "application", appID, "error", err)
		return
	}
	to := strings.TrimSpace(app.Fields["applicant_email"])
	if to == "" {
		log.Warn("purse watcher: no mail, the plot application has no applicant_email", "application", appID)
		return
	}
	p := l.purse
	d := mailer.Data{Name: app.Fields["github_login"], Land: l.name, GrantedUSD: p.GrantedCredits}
	if p.RemainingCredits != nil {
		d.RemainingUSD = math.Max(0, *p.RemainingCredits)
	}
	m, err := w.Templates.Render(k, to, d)
	if err != nil {
		log.Warn("purse watcher: rendering mail failed", "error", err)
		return
	}
	if err := w.Mailer.Send(ctx, m); err != nil {
		log.Warn("purse watcher: sending mail failed", "error", err)
		return
	}
	log.Info("purse watcher: mailed the owner", "application", appID)
}

// park nudges, then suspends, the running agents of a land at the wall or
// frozen, and tells nudged agents to carry on when the cause lifts before
// their grace ends.
func (w *Watcher) park(ctx context.Context, l land, now time.Time) error {
	cause, hold := l.parkCause()
	if hold {
		return nil
	}
	agents, err := w.agents(ctx, l.name)
	var errs []string
	if err != nil {
		errs = append(errs, err.Error())
	}
	for _, a := range agents {
		var err error
		switch nudged := nudgedAt(a, now); {
		case cause != "" && nudged.IsZero():
			err = w.nudge(ctx, l.name, cause, a, now)
		case cause != "" && now.Sub(nudged) >= w.grace():
			err = w.suspend(ctx, l.name, cause, a)
		case cause == "" && !nudged.IsZero():
			err = w.carryOn(ctx, l.name, a)
		}
		if err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// nudgedAt is when a was nudged, or zero. A stamp after now is no nudge: it
// would hold the grace open for ever, so the agent is nudged again and its
// grace starts now (gd-qpFpWcWucD).
func nudgedAt(a *beadsapi.BeadDetail, now time.Time) time.Time {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(a.Fields[FieldNudgedAt]))
	if err != nil || t.After(now) {
		return time.Time{}
	}
	return t
}

// nudge asks the agent to push and stop, and starts its grace. The grace
// starts even when the nudge cannot be delivered (no coop, pod gone): the
// suspend still comes, it is the wall (or the freeze) that matters.
func (w *Watcher) nudge(ctx context.Context, land, cause string, a *beadsapi.BeadDetail, now time.Time) error {
	name := a.Fields["agent"]
	text, why := NudgeText(land, w.grace()), fmt.Sprintf("The purse for land %s is at the wall. Nudged to push its work; suspended in %s unless credit arrives.", land, w.grace())
	if cause == ParkFrozen {
		text, why = FrozenNudgeText(land, w.grace()), fmt.Sprintf("Land %s is frozen. Nudged to push its work; suspended in %s unless the land is thawed.", land, w.grace())
	}
	if w.Nudger != nil {
		if err := w.Nudger.Nudge(ctx, name, text); err != nil {
			w.logger().Warn("purse watcher: nudge not delivered", "land", land, "agent", name, "error", err)
		}
	}
	if err := w.Daemon.UpdateBeadFields(ctx, a.ID, map[string]string{FieldNudgedAt: now.UTC().Format(time.RFC3339)}); err != nil {
		return fmt.Errorf("stamping the nudge on %s: %w", a.ID, err)
	}
	w.comment(ctx, a.ID, why)
	w.logger().Info("purse watcher: nudged", "land", land, "agent", name, "bead", a.ID, "cause", cause)
	return nil
}

// suspend parks the agent on the suspend path gb stop takes: agent_state
// stopping, which the controller turns into a checkpoint (or a clean stop)
// whose session a later spawn resumes.
func (w *Watcher) suspend(ctx context.Context, land, cause string, a *beadsapi.BeadDetail) error {
	if err := w.Daemon.UpdateBeadFields(ctx, a.ID, map[string]string{
		"agent_state":   "stopping",
		FieldParkReason: cause,
		// A later spawn thaws this bead; its grace is over, not pending.
		FieldNudgedAt: "",
	}); err != nil {
		return fmt.Errorf("suspending %s: %w", a.ID, err)
	}
	why := fmt.Sprintf("Suspended: the purse for land %s is empty (park_reason=%s). The steward decides what to resume when credit arrives.", land, cause)
	if cause == ParkFrozen {
		why = fmt.Sprintf("Suspended: land %s is frozen (park_reason=%s). Nothing resumes on its own when it is thawed.", land, cause)
	}
	w.comment(ctx, a.ID, why)
	w.logger().Info("purse watcher: suspended", "land", land, "agent", a.Fields["agent"], "bead", a.ID, "cause", cause)
	return nil
}

// carryOn clears a nudge whose wall or freeze lifted inside the grace.
func (w *Watcher) carryOn(ctx context.Context, land string, a *beadsapi.BeadDetail) error {
	if err := w.Daemon.UpdateBeadFields(ctx, a.ID, map[string]string{FieldNudgedAt: ""}); err != nil {
		return fmt.Errorf("clearing the nudge on %s: %w", a.ID, err)
	}
	name := a.Fields["agent"]
	if w.Nudger != nil {
		if err := w.Nudger.Nudge(ctx, name, CarryOnText(land)); err != nil {
			w.logger().Warn("purse watcher: carry-on nudge not delivered", "land", land, "agent", name, "error", err)
		}
	}
	w.comment(ctx, a.ID, fmt.Sprintf("Land %s can run again (its purse has credit and it is not frozen); the suspend is called off.", land))
	return nil
}

func (w *Watcher) comment(ctx context.Context, beadID, text string) {
	if err := w.Daemon.AddComment(ctx, beadID, CommentAuthor, text); err != nil {
		w.logger().Warn("purse watcher: comment failed", "bead", beadID, "error", err)
	}
}

// agents are the land's running agents: the beads of the agent pods in the
// land's namespace that are not terminating, whatever those beads say of
// themselves. The one exception is an agent the watcher has suspended
// (agent_state=stopping with a park_reason, a field only the controller
// writes, gd-qpFpWcWucD) whose pod has not gone yet: it is not nudged or suspended again.
// A pod with no bead-id annotation (a warm placeholder) runs no agent.
func (w *Watcher) agents(ctx context.Context, land string) ([]*beadsapi.BeadDetail, error) {
	if w.Pods == nil {
		return nil, fmt.Errorf("reading %s's agents: the watcher has no pod lister", land)
	}
	ns, err := landns.Name(land)
	if err != nil {
		return nil, fmt.Errorf("reading %s's agents: %w", land, err)
	}
	pods, err := w.Pods.ListAgentPods(ctx, ns, nil)
	switch {
	case apierrors.IsNotFound(err) || apierrors.IsForbidden(err):
		// Not made yet, or not bound to the controller: it cannot have put a
		// pod there either (the reconciler skips it the same way).
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("reading %s's agent pods: %w", land, err)
	}
	seen := map[string]bool{}
	var out []*beadsapi.BeadDetail
	var errs []error
	for _, pod := range pods {
		id := strings.TrimSpace(pod.Annotations[podmanager.AnnotationBeadID])
		if id == "" || seen[id] || pod.DeletionTimestamp != nil ||
			pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		seen[id] = true
		b, err := w.Daemon.GetBead(ctx, id)
		if err != nil {
			errs = append(errs, fmt.Errorf("reading the bead of %s's pod %s: %w", land, pod.Name, err))
			continue
		}
		if b == nil || b.Status == "closed" || parked(b) {
			continue
		}
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, errors.Join(errs...)
}

// parked reports an agent the watcher has already suspended.
func parked(b *beadsapi.BeadDetail) bool {
	return b.Fields["agent_state"] == "stopping" && strings.TrimSpace(b.Fields[FieldParkReason]) != ""
}

// accounts are the live account beads by title, the lowest id winning a
// duplicate as the purse reads them.
func (w *Watcher) accounts(ctx context.Context) (map[string]*beadsapi.BeadDetail, error) {
	beads, err := listAll(ctx, w.Daemon, beadsapi.ListBeadsQuery{Types: []string{"account"}, Statuses: liveStatuses})
	if err != nil {
		return nil, fmt.Errorf("reading accounts: %w", err)
	}
	out := map[string]*beadsapi.BeadDetail{}
	for _, b := range beads {
		if cur, ok := out[b.Title]; ok && cur.ID < b.ID {
			continue
		}
		out[b.Title] = b
	}
	return out, nil
}

const (
	pageSize = 1000
	maxPages = 100
)

// listAll pages a query to the end, and refuses a partial read.
func listAll(ctx context.Context, src purse.Source, q beadsapi.ListBeadsQuery) ([]*beadsapi.BeadDetail, error) {
	q.Limit, q.Sort = pageSize, "created_at"
	var out []*beadsapi.BeadDetail
	seen := map[string]bool{}
	for page := range maxPages {
		q.Offset = page * pageSize
		res, err := src.ListBeadsFiltered(ctx, q)
		if err != nil {
			return nil, err
		}
		if res == nil {
			return out, nil
		}
		for _, b := range res.Beads {
			if b != nil && !seen[b.ID] {
				seen[b.ID] = true
				out = append(out, b)
			}
		}
		if len(res.Beads) < pageSize {
			return out, nil
		}
	}
	return nil, fmt.Errorf("more than %d %v beads; refusing a partial read", maxPages*pageSize, q.Types)
}
