package bridge

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"gardenio/controller/internal/conversation"

	"github.com/slack-go/slack"
)

// slackSurface is conversation.Surface implementation #1: the Slack Web API
// over the *slack.Client the bridge already builds (NewBot). It is the seam
// the bot_*.go Block Kit code moves behind, slice by slice (design spec
// 2026-10-19-lands-commons-design.md, section 5.2); each op sends exactly the
// payload today's direct call sends, which surface_slack_test.go pins against
// the existing Bot paths.
//
// Slice 0 wires it nowhere: no caller changes.
type slackSurface struct {
	api *slack.Client
	// render turns a Card (always) or a Notice (when it has blocks) into
	// Block Kit. The card builders need Bot state (theme, agent maps), so the
	// slice that moves a card registers its builder here. nil: no cards.
	render slackRenderer
	// openDM resolves a Slack user to their IM channel. nil: one
	// conversations.open per DM; the Bot passes its cached openDMChannel.
	openDM func(ctx context.Context, userID string) (string, error)
}

// slackRenderer returns the Block Kit for m in a Slack channel, or no blocks
// for a plain-text message. m.Text stays the notification/fallback text (U12).
type slackRenderer func(ctx context.Context, channel string, m conversation.Message) ([]slack.Block, error)

var _ conversation.Surface = (*slackSurface)(nil)

func newSlackSurface(api *slack.Client) *slackSurface {
	return &slackSurface{api: api}
}

func (s *slackSurface) Name() string { return conversation.SurfaceSlack }

// msgOptions is the chat.postMessage / chat.update body for m.
func (s *slackSurface) msgOptions(ctx context.Context, channel string, m conversation.Message) ([]slack.MsgOption, error) {
	if m.Empty() {
		return nil, conversation.ErrEmptyMessage
	}
	if len(m.Files) > 0 {
		// post sends a text message with files as an upload; a card or notice
		// has no file face.
		return nil, fmt.Errorf("slack: files on a %s message: %w", m.KindOrText(), conversation.ErrUnsupported)
	}
	kind := m.KindOrText()
	var opts []slack.MsgOption
	if kind == conversation.KindNotice && m.Detail != "" {
		return noticeOptions(m), nil
	}
	if m.Text != "" || kind != conversation.KindCard {
		// A card with no fallback text (the permission prompt) sends only its
		// blocks, as it always has.
		opts = append(opts, slack.MsgOptionText(m.Text, false))
	}
	if kind != conversation.KindCard && kind != conversation.KindNotice {
		return opts, nil
	}
	if s.render == nil {
		if kind == conversation.KindCard {
			return nil, fmt.Errorf("slack: no card renderer for %s: %w", m.SubjectID, conversation.ErrUnsupported)
		}
		return opts, nil
	}
	blocks, err := s.render(ctx, channel, m)
	if err != nil {
		return nil, fmt.Errorf("slack: render %s %s: %w", kind, m.SubjectID, err)
	}
	if len(blocks) > 0 {
		opts = append(opts, slack.MsgOptionBlocks(blocks...))
	}
	return opts, nil
}

// noticeOptions renders a Notice with a Detail. The rich face is Text as the
// fallback plus a section block of Detail and a context block of Footnote.
// The terse face is a plain message whose one text param is Detail, so the
// Cause / Pod lines always show. slack-go's MsgOptionText appends, so sending
// Text too would put two text params on the wire and leave which one Slack
// shows to its form parser (gd-aMglRLWRfn).
func noticeOptions(m conversation.Message) []slack.MsgOption {
	if !m.Rich {
		return []slack.MsgOption{slack.MsgOptionText(m.Detail, false)}
	}
	blocks := []slack.Block{slack.NewSectionBlock(
		slack.NewTextBlockObject("mrkdwn", m.Detail, false, false), nil, nil)}
	if m.Footnote != "" {
		blocks = append(blocks, slack.NewContextBlock("",
			slack.NewTextBlockObject("mrkdwn", m.Footnote, false, false)))
	}
	return []slack.MsgOption{slack.MsgOptionText(m.Text, false), slack.MsgOptionBlocks(blocks...)}
}

// personaOptions shows a post as as (chat:write.customize). Only a post takes
// them: chat.update keeps the name and avatar the message was posted with.
func personaOptions(as *conversation.Persona) []slack.MsgOption {
	if as == nil || strings.TrimSpace(as.Name) == "" {
		return nil
	}
	opts := []slack.MsgOption{slack.MsgOptionUsername(as.Name)}
	if as.IconURL != "" {
		opts = append(opts, slack.MsgOptionIconURL(as.IconURL))
	}
	return opts
}

func (s *slackSurface) ref(channel, ts string) conversation.Ref {
	return conversation.Ref{Surface: conversation.SurfaceSlack, Channel: channel, TS: ts}
}

func (s *slackSurface) post(ctx context.Context, channel string, m conversation.Message, extra ...slack.MsgOption) (conversation.Ref, error) {
	if len(m.Files) > 0 && m.KindOrText() == conversation.KindText {
		return s.upload(ctx, channel, "", m)
	}
	opts, err := s.msgOptions(ctx, channel, m)
	if err != nil {
		return conversation.Ref{}, err
	}
	opts = append(opts, personaOptions(m.As)...)
	gotChannel, ts, err := s.api.PostMessageContext(ctx, channel, append(opts, extra...)...)
	if err != nil {
		return conversation.Ref{}, fmt.Errorf("slack: post to %s: %w", channel, err)
	}
	if gotChannel == "" {
		gotChannel = channel
	}
	return s.ref(gotChannel, ts), nil
}

func (s *slackSurface) Post(ctx context.Context, ch conversation.ChannelRef, m conversation.Message) (conversation.Ref, error) {
	if m.Empty() {
		return conversation.Ref{}, conversation.ErrEmptyMessage
	}
	if ch.Slack == "" {
		return conversation.Ref{}, conversation.ErrNoChannel
	}
	return s.post(ctx, ch.Slack, m)
}

func (s *slackSurface) Reply(ctx context.Context, thread conversation.Ref, m conversation.Message, broadcast bool) (conversation.Ref, error) {
	if m.Empty() {
		return conversation.Ref{}, conversation.ErrEmptyMessage
	}
	if thread.IsZero() {
		return conversation.Ref{}, conversation.ErrNoRef
	}
	if len(m.Files) > 0 && m.KindOrText() == conversation.KindText {
		return s.upload(ctx, thread.Channel, thread.TS, m)
	}
	extra := []slack.MsgOption{slack.MsgOptionTS(thread.TS)}
	if broadcast {
		extra = append(extra, slack.MsgOptionBroadcast())
	}
	return s.post(ctx, thread.Channel, m, extra...)
}

// upload shares m's files into channel (into the threadTS thread when set)
// through the files.upload v2 flow, with m.Text as the comment posted
// alongside them. For one file the calls are exactly slack-go's
// UploadFileContext, which the agent upload API made before slice 5
// (getUploadURLExternal, the upload, completeUploadExternal); several files
// share one completeUploadExternal, so they land as one message. It needs the
// files:write scope.
//
// The upload API returns the files, not the message that shares them, so the
// ref carries no ts: there is no copy to edit or delete later.
func (s *slackSurface) upload(ctx context.Context, channel, threadTS string, m conversation.Message) (conversation.Ref, error) {
	if channel == "" {
		return conversation.Ref{}, conversation.ErrNoChannel
	}
	shared := make([]slack.FileSummary, 0, len(m.Files))
	for _, f := range m.Files {
		if f.Name == "" || len(f.Data) == 0 {
			return conversation.Ref{}, fmt.Errorf("slack: file %q is empty: %w", f.Name, conversation.ErrEmptyMessage)
		}
		u, err := s.api.GetUploadURLExternalContext(ctx, slack.GetUploadURLExternalParameters{
			AltTxt:   f.AltText,
			FileName: f.Name,
			FileSize: len(f.Data),
		})
		if err != nil {
			return conversation.Ref{}, fmt.Errorf("slack: upload %s: GetUploadURLExternal: %w", f.Name, err)
		}
		if err := s.api.UploadToURL(ctx, slack.UploadToURLParameters{
			UploadURL: u.UploadURL,
			Reader:    bytes.NewReader(f.Data),
			Filename:  f.Name,
		}); err != nil {
			return conversation.Ref{}, fmt.Errorf("slack: upload %s: UploadToURL: %w", f.Name, err)
		}
		title := f.Title
		if title == "" {
			title = f.Name
		}
		shared = append(shared, slack.FileSummary{ID: u.FileID, Title: title})
	}
	if _, err := s.api.CompleteUploadExternalContext(ctx, slack.CompleteUploadExternalParameters{
		Files:           shared,
		Channel:         channel,
		InitialComment:  m.Text,
		ThreadTimestamp: threadTS,
	}); err != nil {
		return conversation.Ref{}, fmt.Errorf("slack: share files into %s: CompleteUploadExternal: %w", channel, err)
	}
	return s.ref(channel, ""), nil
}

func (s *slackSurface) Update(ctx context.Context, at conversation.Ref, m conversation.Message) error {
	if at.IsZero() {
		return conversation.ErrNoRef
	}
	opts, err := s.msgOptions(ctx, at.Channel, m)
	if err != nil {
		return err
	}
	if _, _, _, err := s.api.UpdateMessageContext(ctx, at.Channel, at.TS, opts...); err != nil {
		return fmt.Errorf("slack: update %s/%s: %w", at.Channel, at.TS, err)
	}
	return nil
}

func (s *slackSurface) Delete(ctx context.Context, at conversation.Ref) error {
	if at.IsZero() {
		return conversation.ErrNoRef
	}
	if _, _, err := s.api.DeleteMessageContext(ctx, at.Channel, at.TS); err != nil {
		return fmt.Errorf("slack: delete %s/%s: %w", at.Channel, at.TS, err)
	}
	return nil
}

func (s *slackSurface) React(ctx context.Context, at conversation.Ref, reaction string, on bool) error {
	if at.IsZero() {
		return conversation.ErrNoRef
	}
	reaction = strings.Trim(reaction, ":")
	if reaction == "" {
		return fmt.Errorf("slack: empty reaction")
	}
	item := slack.NewRefToMessage(at.Channel, at.TS)
	var err error
	if on {
		err = s.api.AddReactionContext(ctx, reaction, item)
	} else {
		err = s.api.RemoveReactionContext(ctx, reaction, item)
	}
	if err != nil {
		return fmt.Errorf("slack: react %s on %s/%s: %w", reaction, at.Channel, at.TS, err)
	}
	return nil
}

func (s *slackSurface) DM(ctx context.Context, to conversation.Principal, m conversation.Message) (conversation.Ref, error) {
	if m.Empty() {
		return conversation.Ref{}, conversation.ErrEmptyMessage
	}
	if to.SlackUser == "" {
		return conversation.Ref{}, conversation.ErrNoRecipient
	}
	channel, err := s.imChannel(ctx, to.SlackUser)
	if err != nil {
		return conversation.Ref{}, err
	}
	return s.post(ctx, channel, m)
}

// imChannel opens the IM with userID exactly as Bot.openDMChannel does.
func (s *slackSurface) imChannel(ctx context.Context, userID string) (string, error) {
	if s.openDM != nil {
		return s.openDM(ctx, userID)
	}
	ch, _, _, err := s.api.OpenConversationContext(ctx, &slack.OpenConversationParameters{
		Users:    []string{userID},
		ReturnIM: true,
	})
	if err != nil {
		return "", fmt.Errorf("open conversation with %s: %w", userID, err)
	}
	return ch.ID, nil
}

func (s *slackSurface) Ephemeral(ctx context.Context, t conversation.Trigger, to conversation.Principal, m conversation.Message) error {
	if m.Empty() {
		return conversation.ErrEmptyMessage
	}
	if t.Surface != conversation.SurfaceSlack || t.Channel == "" {
		return conversation.ErrNoTrigger
	}
	if to.SlackUser == "" {
		return conversation.ErrNoRecipient
	}
	opts, err := s.msgOptions(ctx, t.Channel, m)
	if err != nil {
		return err
	}
	if t.ThreadTS != "" {
		opts = append(opts, slack.MsgOptionTS(t.ThreadTS))
	}
	if _, err := s.api.PostEphemeralContext(ctx, t.Channel, to.SlackUser, opts...); err != nil {
		return fmt.Errorf("slack: ephemeral to %s in %s: %w", to.SlackUser, t.Channel, err)
	}
	return nil
}

func (s *slackSurface) OpenModal(ctx context.Context, t conversation.Trigger, v conversation.Modal) error {
	if t.Surface != conversation.SurfaceSlack || t.TriggerID == "" {
		return conversation.ErrNoTrigger
	}
	if err := v.Validate(); err != nil {
		return err
	}
	if _, err := s.api.OpenViewContext(ctx, t.TriggerID, slackModalView(v)); err != nil {
		return fmt.Errorf("slack: open modal %s: %w", v.CallbackID, err)
	}
	return nil
}

// slackModalView builds the views.open modal for v, block for block as the
// hand-built modals in bot_decisions_modal.go and bot_human_survey_manual.go.
func slackModalView(v conversation.Modal) slack.ModalViewRequest {
	plain := func(s string) *slack.TextBlockObject {
		if s == "" {
			return nil
		}
		return slack.NewTextBlockObject("plain_text", s, false, false)
	}
	var blocks []slack.Block
	for _, f := range v.Fields {
		switch f.Kind {
		case conversation.FieldNote:
			blocks = append(blocks, slack.NewSectionBlock(
				slack.NewTextBlockObject("mrkdwn", f.Text, false, false), nil, nil))
		case conversation.FieldDivider:
			blocks = append(blocks, slack.NewDividerBlock())
		case conversation.FieldText:
			in := slack.NewInputBlock(f.ID, plain(f.Label), plain(f.Hint), &slack.PlainTextInputBlockElement{
				Type:        slack.METPlainTextInput,
				ActionID:    f.InputID,
				Multiline:   f.Multiline,
				Placeholder: plain(f.Placeholder),
			})
			in.Optional = f.Optional
			blocks = append(blocks, in)
		case conversation.FieldSelect:
			opts := make([]*slack.OptionBlockObject, len(f.Options))
			for i, o := range f.Options {
				opts[i] = slack.NewOptionBlockObject(o.Value, plain(o.Label), nil)
			}
			sel := slack.NewOptionsSelectBlockElement(slack.OptTypeStatic, plain(f.Placeholder), f.InputID, opts...)
			for i, o := range f.Options {
				if o.Value == f.Initial {
					sel.InitialOption = opts[i]
				}
			}
			in := slack.NewInputBlock(f.ID, plain(f.Label), plain(f.Hint), sel)
			in.Optional = f.Optional
			blocks = append(blocks, in)
		}
	}
	return slack.ModalViewRequest{
		Type:            slack.VTModal,
		Title:           plain(v.Title),
		Submit:          plain(v.Submit),
		Close:           plain(v.Close),
		Blocks:          slack.Blocks{BlockSet: blocks},
		PrivateMetadata: v.Private,
		CallbackID:      v.CallbackID,
	}
}
