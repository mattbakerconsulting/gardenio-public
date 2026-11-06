package bridge

// Garden + Business assertions for the Phase 4 surfaces that the other theme
// tests do not reach (gd-yIFqlezCXc): the decision modal's ephemerals and the
// bare-bead-id preview card. Each case is asserted in both themes, and
// Business output must carry none of Garden's glyphs.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"

	"github.com/slack-go/slack"
)

func modalCallback(channel string) slack.InteractionCallback {
	cb := slack.InteractionCallback{
		Channel: slack.Channel{GroupConversation: slack.GroupConversation{
			Conversation: slack.Conversation{ID: channel},
		}},
		User: slack.User{ID: "U1", Name: "tester"},
	}
	cb.MessageTs = "1234.5678"
	return cb
}

func TestDecisionModalEphemerals_ThemedPerChannel(t *testing.T) {
	cases := []struct {
		name       string
		closeErr   error
		run        func(b *Bot, cb slack.InteractionCallback)
		bizWant    string
		gardenWant string
	}{
		{
			name:     "dismiss failure",
			closeErr: errors.New("daemon down"),
			run: func(b *Bot, cb slack.InteractionCallback) {
				b.handleDismiss(context.Background(), "dec-1", cb)
			},
			bizWant:    "[WARN] Couldn't dismiss",
			gardenWant: Garden.Semantic(SemWarn) + " Couldn't dismiss",
		},
		{
			name:     "modal-less resolve failure",
			closeErr: errors.New("daemon down"),
			run: func(b *Bot, cb slack.InteractionCallback) {
				b.resolveDecisionWithoutModal(context.Background(), "dec-1", "opt-a", "Option A", cb)
			},
			bizWant:    "[WARN] Couldn't record your choice",
			gardenWant: Garden.Semantic(SemWarn) + " Couldn't record your choice",
		},
		{
			name: "modal-less resolve success",
			run: func(b *Bot, cb slack.InteractionCallback) {
				b.resolveDecisionWithoutModal(context.Background(), "dec-1", "opt-a", "Option A", cb)
			},
			bizWant:    "[OK] Recorded your choice",
			gardenWant: Garden.Semantic(SemOK) + " Recorded your choice",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for channel, want := range map[string]string{bizChannel: c.bizWant, gardenChannel: c.gardenWant} {
				slackSrv, texts := captureEphemeral(t)
				daemon := newMockDaemon()
				daemon.closeBeadErr = c.closeErr
				b := newTestBot(daemon, slackSrv)
				b.themes.put(bizChannel, Business, time.Now())
				b.themes.put(gardenChannel, Garden, time.Now())

				c.run(b, modalCallback(channel))
				slackSrv.Close()

				got := lastText(t, texts)
				if !strings.Contains(got, want) {
					t.Errorf("%s: want %q in:\n%s", channel, want, got)
				}
				if channel == bizChannel {
					assertNoGardenGlyph(t, "Business modal reply", got)
				}
			}
		})
	}
}

func TestBareBeadPreview_CardThemedPerChannel(t *testing.T) {
	for _, c := range []struct {
		name  string
		theme Theme
		want  string
		avoid string
	}{
		{"business", Business, "[TASK]", Garden.BeadType["task"]},
		{"garden", Garden, Garden.BeadType["task"], "[TASK]"},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := newPreviewBot(true, map[string]*beadsapi.BeadDetail{
				"gd-open": {ID: "gd-open", Title: "Open task", Type: "task", Status: "open"},
			})
			ev := msg("see gd-open")
			b.themes.put(ev.Channel, c.theme, time.Now())

			atts := b.bareBeadPreviewAttachments(context.Background(), ev)
			if len(atts) != 1 {
				t.Fatalf("len(atts) = %d, want 1", len(atts))
			}
			text := renderBlocksText(atts[0])
			if !strings.Contains(text, c.want) {
				t.Errorf("want %q in card:\n%s", c.want, text)
			}
			if strings.Contains(text, c.avoid) {
				t.Errorf("card leaked %q from the other theme:\n%s", c.avoid, text)
			}
		})
	}
}
