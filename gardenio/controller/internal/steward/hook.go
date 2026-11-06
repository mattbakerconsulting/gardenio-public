package steward

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/lands/purse"
	"gardenio/controller/internal/plangardener"
	"gardenio/controller/internal/scheduler"
)

// The provisioner hook (spec §3.3 step 6, step 9.3): the Lands provisioner
// binds a new land's steward, then wakes it once so its first run greets
// #general (playbook duty 1). When a person joins a land later, by a claimed
// sign-in invite or a joined member invite, it wakes the steward once more to
// greet them by name in #general (step 9.14, decision D3 gd-NLc4LzmCAE).

const (
	// OriginLandOpened is the origin of the provisioner's wake post, which
	// the steward's land-opened entry fires on.
	OriginLandOpened = "land_opened"
	// OriginPersonJoined is the origin of the provisioner's wake post when a
	// person joins a land, which the steward's person-joined entry fires on.
	OriginPersonJoined = "person_joined"
	// OpenedAuthor signs both wake posts.
	OpenedAuthor = "agent:gardenio-provisioner"
)

// ProvisionHookClient is the daemon the provisioner hook reads and writes
// through: Bind's reads and trigger create, the bound trigger's read, the
// wake post, and the retiring disarm, close and note. *beadsapi.Client
// satisfies it.
type ProvisionHookClient interface {
	plangardener.Client
	beadGetter
	UpdateBeadFieldsTyped(ctx context.Context, beadID string, fields map[string]any) error
	CloseBead(ctx context.Context, beadID string, fields map[string]string) error
	AddComment(ctx context.Context, beadID, author, text string) error
}

// ProvisionHook binds and wakes stewards for the Lands provisioner
// (provision.Stewards). Spec is the bind spec every land's steward gets; its
// Operator is replaced by the land's approver when one is given.
type ProvisionHook struct {
	Client ProvisionHookClient
	Spec   BindSpec
}

// Bind binds land's one steward, or finds the one it has, and returns its
// trigger bead.
func (h ProvisionHook) Bind(ctx context.Context, land, operator string) (*beadsapi.BeadDetail, error) {
	spec := h.Spec
	if operator != "" {
		spec.Operator = operator
	}
	res, err := Bind(ctx, h.Client, land, spec)
	if err != nil {
		return nil, err
	}
	b, err := h.Client.GetBead(ctx, res.ID)
	if err != nil {
		return nil, fmt.Errorf("reading %s's steward %s: %w", land, res.ID, err)
	}
	if b == nil {
		return nil, fmt.Errorf("%s's steward %s is not found", land, res.ID)
	}
	return b, nil
}

// Find is land's open steward trigger, or, when it has none, the one that
// closed last (nil if it never had one): what the provisioner's re-bind
// reads.
func (h ProvisionHook) Find(ctx context.Context, land string) (open, last *beadsapi.BeadDetail, err error) {
	if open, err = FindSteward(ctx, h.Client, land); err != nil || open != nil {
		return open, nil, err
	}
	last, err = LastSteward(ctx, h.Client, land)
	return nil, last, err
}

// Wake posts the land-opened notice in the land's #machines (its #general
// when it has none), which wakes the steward once. A tenant land never
// mirrors to Slack (B-D11), so the bead is the whole delivery. key makes the
// post idempotent.
func (h ProvisionHook) Wake(ctx context.Context, land, key string) error {
	return h.post(ctx, land, OpenedText(land), OriginLandOpened, key)
}

// Greet posts the person-joined notice where Wake posts, which wakes the
// steward once to greet the person in #general. name is what the greeting
// calls them ("" when there is nothing to call them that is not private) and
// role their land role. key makes the post idempotent: one greeting per join.
func (h ProvisionHook) Greet(ctx context.Context, land, name, role, key string) error {
	return h.post(ctx, land, JoinedText(name, role), OriginPersonJoined, key)
}

// post writes one of the provisioner's wake posts.
func (h ProvisionHook) post(ctx context.Context, land, text, origin, key string) error {
	ch, err := channelNamed(ctx, h.Client, land, MachinesChannel)
	if err != nil {
		return err
	}
	if ch == "" {
		proj, _, err := purse.Project(ctx, h.Client, land)
		if err != nil {
			return err
		}
		if ch = proj.Fields["land_default_channel"]; ch == "" {
			return fmt.Errorf("waking %s's steward: %w", land, ErrNoGeneral)
		}
	}
	fields, err := json.Marshal(map[string]string{
		"channel_id": ch,
		"kind":       "notice",
		"author":     OpenedAuthor,
		"text":       text,
		"origin":     origin,
	})
	if err != nil {
		return err
	}
	// The daemon lets a caller name only itself as a message's author unless it
	// is a bridge or a machine principal (kbeads message_author.go). On the
	// machine principal it may name OpenedAuthor; on the static token X-Actor
	// makes the hook gardenio-provisioner, so the author names its writer.
	if _, err := h.Client.CreateBead(beadsapi.WithActor(ctx, strings.TrimPrefix(OpenedAuthor, "agent:")), beadsapi.CreateBeadRequest{
		Title:          text,
		Type:           "message",
		Kind:           "data",
		Status:         "closed",
		Labels:         []string{"project:" + land},
		CreatedBy:      OpenedAuthor,
		Fields:         fields,
		IdempotencyKey: key,
	}); err != nil {
		return fmt.Errorf("posting %s's wake: %w", land, err)
	}
	return nil
}

// Retire retires land's steward when the land closes: it disarms the trigger
// (enabled=false), so the trigger-reviver never reads it as a dark lane to
// revive, then closes it, which takes it out of trigger-engine's load and
// stops every wake and the daily digest. why is noted on the trigger. A land
// with no open steward is done, so a re-run after a crash repeats nothing.
// The provisioner created the trigger, so its machine principal owns it.
func (h ProvisionHook) Retire(ctx context.Context, land, why string) error {
	trig, err := FindSteward(ctx, h.Client, land)
	if err != nil || trig == nil {
		return err
	}
	if err := h.Client.UpdateBeadFieldsTyped(ctx, trig.ID, map[string]any{"enabled": false}); err != nil {
		return fmt.Errorf("disarming %s's steward %s: %w", land, trig.ID, err)
	}
	if err := h.Client.CloseBead(ctx, trig.ID, nil); err != nil {
		return fmt.Errorf("closing %s's steward %s: %w", land, trig.ID, err)
	}
	if why != "" {
		// The steward is retired either way; a lost note is not a failure.
		_ = h.Client.AddComment(ctx, trig.ID, h.Spec.CreatedBy, why)
	}
	return nil
}

// OpenedText is the wake post: what happened, for whoever reads #machines.
func OpenedText(land string) string {
	return fmt.Sprintf("Land %s is open. Its steward is woken to greet #general.", land)
}

// JoinedText is the person-joined wake post. It names the person only by the
// name Greet was given, never an email address: every guest of a land reads
// its #machines too.
func JoinedText(name, role string) string {
	who := "A newcomer"
	if name = oneLine(name); name != "" {
		who = name
	}
	if role = oneLine(role); role == "" {
		role = "guest"
	}
	return fmt.Sprintf("%s joined the land as a %s. Its steward is woken to greet them in #general.", who, role)
}

// Upgrade brings land's open steward trigger trig up to the wake entries a
// fresh bind writes, where a later step added one: the person-joined entry
// (step 9.14) and its post kept out of the #machines delta. A steward bound
// before then never hears a person join, so the provisioner's pass upgrades
// each open steward once; one already up to date is not written.
func (h ProvisionHook) Upgrade(ctx context.Context, land string, trig *beadsapi.BeadDetail) error {
	_, err := h.upgrade(ctx, land, trig, UpgradeWakes)
	return err
}

// UpgradeHome is Upgrade for the home garden's steward (UpgradeHomeWakes),
// which no provisioner pass reaches: gb lands steward gardenio --bind runs
// it. It reports whether it wrote the trigger.
func (h ProvisionHook) UpgradeHome(ctx context.Context, land string, trig *beadsapi.BeadDetail) (bool, error) {
	return h.upgrade(ctx, land, trig, UpgradeHomeWakes)
}

func (h ProvisionHook) upgrade(ctx context.Context, land string, trig *beadsapi.BeadDetail,
	fn func(string, []scheduler.TriggerEntry) ([]scheduler.TriggerEntry, bool),
) (bool, error) {
	if trig == nil {
		return false, nil
	}
	entries, err := scheduler.ParseTriggers(trig.Fields["triggers"])
	if err != nil {
		return false, fmt.Errorf("reading %s's steward %s triggers: %w", land, trig.ID, err)
	}
	up, changed := fn(land, entries)
	if !changed {
		return false, nil
	}
	raw, err := json.Marshal(up)
	if err != nil {
		return false, err
	}
	if err := h.Client.UpdateBeadFieldsTyped(ctx, trig.ID, map[string]any{"triggers": string(raw)}); err != nil {
		return false, fmt.Errorf("upgrading %s's steward %s triggers: %w", land, trig.ID, err)
	}
	return true, nil
}
