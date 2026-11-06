package surfacetest

import (
	"context"
	"errors"
	"testing"

	"gardenio/controller/internal/conversation"
)

// Lookup reads back what a surface shows at a ref.
type Lookup func(conversation.Ref) (Shown, bool)

// Harness is what RunContract needs to drive one Surface implementation.
type Harness struct {
	// New returns a fresh surface and a reader over its backing store (the
	// Recorder itself, or the Slack twin the surface's client points at).
	New func(t *testing.T) (conversation.Surface, Lookup)
	// Channel is a channel with an id on the surface under test.
	Channel conversation.ChannelRef
	// To is a person the surface can DM and send ephemerals to.
	To conversation.Principal
	// Trigger is an inbound trigger from the surface under test.
	Trigger conversation.Trigger
}

// RunContract runs the behaviour every conversation.Surface must have. It is
// run against the Recorder (so the fake cannot drift from the real thing) and
// against the Slack surface over the slackmock twin.
func RunContract(t *testing.T, h Harness) {
	t.Helper()
	ctx := context.Background()
	text := func(s string) conversation.Message { return conversation.Message{Text: s} }

	t.Run("post", func(t *testing.T) {
		s, look := h.New(t)
		if s.Name() == "" {
			t.Fatal("Name() is empty")
		}
		a, err := s.Post(ctx, h.Channel, text("first"))
		if err != nil {
			t.Fatalf("Post: %v", err)
		}
		want := h.Channel.For(s.Name())
		if a.Surface != s.Name() || a.Channel != want || a.TS == "" {
			t.Fatalf("Post ref = %+v, want surface %q channel %q and a ts", a, s.Name(), want)
		}
		b, err := s.Post(ctx, h.Channel, text("second"))
		if err != nil {
			t.Fatalf("second Post: %v", err)
		}
		if a.TS == b.TS {
			t.Fatalf("two posts share ts %q", a.TS)
		}
		got, ok := look(a)
		if !ok || got.Text != "first" || got.ThreadTS != "" {
			t.Fatalf("shown at %+v = %+v (found %v), want top-level %q", a, got, ok, "first")
		}
	})

	t.Run("post rejects empty and unbound", func(t *testing.T) {
		s, _ := h.New(t)
		if _, err := s.Post(ctx, h.Channel, text("  ")); !errors.Is(err, conversation.ErrEmptyMessage) {
			t.Fatalf("Post empty: err = %v, want ErrEmptyMessage", err)
		}
		if _, err := s.Post(ctx, conversation.ChannelRef{Land: h.Channel.Land}, text("x")); !errors.Is(err, conversation.ErrNoChannel) {
			t.Fatalf("Post to unbound channel: err = %v, want ErrNoChannel", err)
		}
	})

	t.Run("reply", func(t *testing.T) {
		s, look := h.New(t)
		root, err := s.Post(ctx, h.Channel, text("root"))
		if err != nil {
			t.Fatalf("Post: %v", err)
		}
		for _, broadcast := range []bool{false, true} {
			r, err := s.Reply(ctx, root, text("reply"), broadcast)
			if err != nil {
				t.Fatalf("Reply(broadcast=%v): %v", broadcast, err)
			}
			if r.Surface != s.Name() || r.Channel != root.Channel || r.TS == "" || r.TS == root.TS {
				t.Fatalf("Reply ref = %+v, want a new ts in %q", r, root.Channel)
			}
			got, ok := look(r)
			if !ok || got.ThreadTS != root.TS || got.Text != "reply" {
				t.Fatalf("shown at %+v = %+v (found %v), want reply in thread %q", r, got, ok, root.TS)
			}
		}
		if _, err := s.Reply(ctx, conversation.Ref{Surface: s.Name()}, text("x"), false); !errors.Is(err, conversation.ErrNoRef) {
			t.Fatalf("Reply to empty ref: err = %v, want ErrNoRef", err)
		}
		if _, err := s.Reply(ctx, root, text(""), false); !errors.Is(err, conversation.ErrEmptyMessage) {
			t.Fatalf("Reply empty: err = %v, want ErrEmptyMessage", err)
		}
	})

	t.Run("update in place", func(t *testing.T) {
		s, look := h.New(t)
		at, err := s.Post(ctx, h.Channel, text("before"))
		if err != nil {
			t.Fatalf("Post: %v", err)
		}
		if err := s.Update(ctx, at, text("after")); err != nil {
			t.Fatalf("Update: %v", err)
		}
		if got, ok := look(at); !ok || got.Text != "after" {
			t.Fatalf("shown after Update = %+v (found %v), want %q", got, ok, "after")
		}
		if err := s.Update(ctx, conversation.Ref{}, text("x")); !errors.Is(err, conversation.ErrNoRef) {
			t.Fatalf("Update empty ref: err = %v, want ErrNoRef", err)
		}
		if err := s.Update(ctx, at, text("")); !errors.Is(err, conversation.ErrEmptyMessage) {
			t.Fatalf("Update empty: err = %v, want ErrEmptyMessage", err)
		}
	})

	t.Run("delete", func(t *testing.T) {
		s, look := h.New(t)
		at, err := s.Post(ctx, h.Channel, text("doomed"))
		if err != nil {
			t.Fatalf("Post: %v", err)
		}
		if err := s.Delete(ctx, at); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if got, ok := look(at); ok && !got.Deleted {
			t.Fatalf("message still shown after Delete: %+v", got)
		}
		if err := s.Delete(ctx, conversation.Ref{}); !errors.Is(err, conversation.ErrNoRef) {
			t.Fatalf("Delete empty ref: err = %v, want ErrNoRef", err)
		}
	})

	t.Run("react", func(t *testing.T) {
		s, look := h.New(t)
		at, err := s.Post(ctx, h.Channel, text("react to me"))
		if err != nil {
			t.Fatalf("Post: %v", err)
		}
		if err := s.React(ctx, at, "eyes", true); err != nil {
			t.Fatalf("React on: %v", err)
		}
		if got, _ := look(at); !got.Reactions["eyes"] {
			t.Fatalf("reactions after on = %v, want eyes", got.Reactions)
		}
		if err := s.React(ctx, at, "eyes", false); err != nil {
			t.Fatalf("React off: %v", err)
		}
		if got, _ := look(at); got.Reactions["eyes"] {
			t.Fatalf("reactions after off = %v, want none", got.Reactions)
		}
		if err := s.React(ctx, conversation.Ref{}, "eyes", true); !errors.Is(err, conversation.ErrNoRef) {
			t.Fatalf("React empty ref: err = %v, want ErrNoRef", err)
		}
	})

	t.Run("dm", func(t *testing.T) {
		s, look := h.New(t)
		at, err := s.DM(ctx, h.To, text("psst"))
		if err != nil {
			t.Fatalf("DM: %v", err)
		}
		if at.Surface != s.Name() || at.IsZero() {
			t.Fatalf("DM ref = %+v", at)
		}
		if got, ok := look(at); !ok || got.Text != "psst" {
			t.Fatalf("shown at DM ref = %+v (found %v)", got, ok)
		}
		if _, err := s.DM(ctx, conversation.Principal{}, text("x")); !errors.Is(err, conversation.ErrNoRecipient) {
			t.Fatalf("DM nobody: err = %v, want ErrNoRecipient", err)
		}
		if _, err := s.DM(ctx, h.To, text("")); !errors.Is(err, conversation.ErrEmptyMessage) {
			t.Fatalf("DM empty: err = %v, want ErrEmptyMessage", err)
		}
	})

	t.Run("ephemeral", func(t *testing.T) {
		s, _ := h.New(t)
		if err := s.Ephemeral(ctx, h.Trigger, h.To, text("only you")); err != nil {
			t.Fatalf("Ephemeral: %v", err)
		}
		foreign := h.Trigger
		foreign.Surface = "elsewhere"
		if err := s.Ephemeral(ctx, foreign, h.To, text("x")); !errors.Is(err, conversation.ErrNoTrigger) {
			t.Fatalf("Ephemeral on a foreign trigger: err = %v, want ErrNoTrigger", err)
		}
		if err := s.Ephemeral(ctx, h.Trigger, conversation.Principal{}, text("x")); !errors.Is(err, conversation.ErrNoRecipient) {
			t.Fatalf("Ephemeral to nobody: err = %v, want ErrNoRecipient", err)
		}
		if err := s.Ephemeral(ctx, h.Trigger, h.To, text("")); !errors.Is(err, conversation.ErrEmptyMessage) {
			t.Fatalf("Ephemeral empty: err = %v, want ErrEmptyMessage", err)
		}
	})

	t.Run("open modal", func(t *testing.T) {
		s, _ := h.New(t)
		v := ContractModal()
		if err := s.OpenModal(ctx, h.Trigger, v); err != nil {
			t.Fatalf("OpenModal: %v", err)
		}
		foreign := h.Trigger
		foreign.Surface = "elsewhere"
		if err := s.OpenModal(ctx, foreign, v); !errors.Is(err, conversation.ErrNoTrigger) {
			t.Fatalf("OpenModal on a foreign trigger: err = %v, want ErrNoTrigger", err)
		}
		bad := v
		bad.CallbackID = ""
		if err := s.OpenModal(ctx, h.Trigger, bad); !errors.Is(err, conversation.ErrBadModal) {
			t.Fatalf("OpenModal invalid: err = %v, want ErrBadModal", err)
		}
	})
}

// ContractModal is the modal RunContract opens: one of every field kind.
func ContractModal() conversation.Modal {
	return conversation.Modal{
		CallbackID: "contract_modal",
		Title:      "Contract",
		Submit:     "Submit",
		Close:      "Cancel",
		Private:    "bead-1|C1|1.2",
		Fields: []conversation.Field{
			{Kind: conversation.FieldNote, Text: "*Pick one*"},
			{Kind: conversation.FieldDivider},
			{Kind: conversation.FieldText, ID: "notes", InputID: "notes_input", Label: "Notes", Placeholder: "Anything else", Multiline: true, Optional: true},
			{Kind: conversation.FieldSelect, ID: "choice", InputID: "choice_input", Label: "Choice", Hint: "What next", Placeholder: "Choose…",
				Options: []conversation.Option{{Value: "a", Label: "A"}, {Value: "b", Label: "B"}}, Initial: "a"},
		},
	}
}
