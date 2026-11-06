package conversation_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation"
)

func TestDMMembersSortsAndDedups(t *testing.T) {
	got := conversation.DMMembers(" oidc:ann@example.com", "agent:crew-1", "", "oidc:ann@example.com")
	want := []string{"agent:crew-1", "oidc:ann@example.com"}
	if !slices.Equal(got, want) {
		t.Fatalf("DMMembers = %v, want %v", got, want)
	}
}

func TestDMNameIsStableAndASlug(t *testing.T) {
	a := conversation.DMName(conversation.DMMembers("agent:crew-1", "oidc:ann@example.com"))
	b := conversation.DMName(conversation.DMMembers("oidc:ann@example.com", "agent:crew-1"))
	if a != b {
		t.Fatalf("member order changed the name: %q vs %q", a, b)
	}
	if c := conversation.DMName(conversation.DMMembers("agent:crew-2", "oidc:ann@example.com")); c == a {
		t.Fatalf("two different DMs share the name %q", a)
	}
	if !strings.HasPrefix(a, "dm-") || len(a) != len("dm-")+16 || strings.Contains(a, "ann") {
		t.Fatalf("name %q must be dm- and 16 hex digits, naming nobody", a)
	}
}

func TestEnsureDMCreatesOnceThenFinds(t *testing.T) {
	store := newFakeStore()
	ctx := context.Background()
	ch, err := conversation.EnsureDM(ctx, store, "gardenio", "crew-1, Ann", "oidc:ann@example.com", "agent:crew-1")
	if err != nil {
		t.Fatalf("EnsureDM: %v", err)
	}
	if ch.ID == "" || ch.Land != "gardenio" || ch.Slack != "" {
		t.Fatalf("channel = %+v, want a gardenio channel bead with no Slack binding", ch)
	}
	if len(store.creates) != 1 {
		t.Fatalf("creates = %d, want 1", len(store.creates))
	}
	req := store.creates[0]
	if req.Type != "channel" || req.Title != "crew-1, Ann" || !slices.Equal(req.Labels, []string{"project:gardenio"}) {
		t.Fatalf("create = %+v", req)
	}
	b := store.beads[ch.ID]
	if b.Fields["channel_kind"] != "dm" || !strings.HasPrefix(b.Fields["name"], "dm-") {
		t.Fatalf("fields = %v", b.Fields)
	}
	if got := conversation.MembersOf(b.Fields); !slices.Equal(got, []string{"agent:crew-1", "oidc:ann@example.com"}) {
		t.Fatalf("members = %v", got)
	}

	again, err := conversation.EnsureDM(ctx, store, "gardenio", "", "agent:crew-1", "oidc:ann@example.com")
	if err != nil || again.ID != ch.ID {
		t.Fatalf("second EnsureDM = %+v, %v; want %s", again, err, ch.ID)
	}
	if len(store.creates) != 1 {
		t.Fatalf("the second EnsureDM created again (%d creates)", len(store.creates))
	}
	// The same members in another land are another DM.
	other, err := conversation.EnsureDM(ctx, store, "claimlayer", "", "agent:crew-1", "oidc:ann@example.com")
	if err != nil || other.ID == ch.ID || other.Land != "claimlayer" {
		t.Fatalf("other land = %+v, %v", other, err)
	}
}

func TestEnsureDMRefusesTooFewMembersAndNoLand(t *testing.T) {
	store := newFakeStore()
	ctx := context.Background()
	if _, err := conversation.EnsureDM(ctx, store, "gardenio", "", "agent:crew-1", "agent:crew-1"); !errors.Is(err, conversation.ErrDMMembers) {
		t.Fatalf("one member: err = %v, want ErrDMMembers", err)
	}
	if _, err := conversation.EnsureDM(ctx, store, "", "", "agent:crew-1", "oidc:ann@example.com"); !errors.Is(err, conversation.ErrNoLand) {
		t.Fatalf("no land: err = %v, want ErrNoLand", err)
	}
	if len(store.creates) != 0 {
		t.Fatalf("a refused DM was created")
	}
}

// A create the daemon refuses because a racer made the channel first finds the
// racer's channel.
func TestEnsureDMLosingARaceFindsTheWinner(t *testing.T) {
	store := newFakeStore()
	ctx := context.Background()
	members := conversation.DMMembers("agent:crew-1", "oidc:ann@example.com")
	winner, _ := store.CreateBead(ctx, beadsapi.CreateBeadRequest{Type: "channel", Status: "open", Labels: []string{"project:gardenio"},
		Fields: []byte(`{"name":"` + conversation.DMName(members) + `","channel_kind":"dm"}`)})
	racing := &racingStore{fakeStore: store, hideFirstList: true}
	store.createErr = errors.New("land gardenio already has an open channel named dm-…")
	got, err := conversation.EnsureDM(ctx, racing, "gardenio", "", members...)
	if err != nil || got.ID != winner {
		t.Fatalf("EnsureDM = %+v, %v; want the winner %s", got, err, winner)
	}
}

// racingStore hides the channel from the first lookup, as a racer's create
// that lands between the lookup and the create would.
type racingStore struct {
	*fakeStore
	hideFirstList bool
}

func (s *racingStore) ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	if s.hideFirstList {
		s.hideFirstList = false
		return &beadsapi.ListBeadsResult{}, nil
	}
	return s.fakeStore.ListBeadsFiltered(ctx, q)
}
