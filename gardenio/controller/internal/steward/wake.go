package steward

import (
	"context"
	"encoding/json"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"gardenio/controller/internal/bridge"
	"gardenio/controller/internal/commons"
	"gardenio/controller/internal/conversation"
)

// The message that woke the steward (step 9.10, gd-IMt3KYl1XZ). A person's
// message in #general, or an @steward mention elsewhere in the land, is
// rendered under "## What woke you" with its files and, for a farm snapshot,
// what the camera framed. The steward fetches the files with gb commons file,
// Reads each picture, and answers in the message's thread, so the picture,
// the ask and the answer stay together.

// DefaultSnapAsk is the ask of a snapshot sent with no words.
const DefaultSnapAsk = "Say what you see on this part of the land and what, if anything, needs the owner."

// wakeTextMax caps the message text in the prompt, in bytes (amendment §6: a
// run under 30k input tokens); the steward reads the rest with gb thread.
const wakeTextMax = 4 << 10

// wakeFilesMax caps the files listed.
const wakeFilesMax = 10

// WakeMessage is the message that woke this run.
type WakeMessage struct {
	ID string
	// Thread is the thread to answer in: the message's thread head, else the
	// message itself (a top-level post heads its own thread).
	Thread string
	// Mention is true for an @steward mention outside #general.
	Mention bool
	// Channel is the channel's name, "" when it could not be read.
	Channel string
	Author  string
	// Text is the person's words, cut to wakeTextMax; Cut says it was.
	Text  string
	Cut   bool
	Files []WakeFile
	// Fetch is true when a file is a Commons attachment gb commons file
	// fetches (a Slack permalink is not).
	Fetch bool
	Snap  *WakeSnap
}

// WakeFile is one attachment of the wake message.
type WakeFile struct {
	Name     string
	MimeType string
	// Local is the name gb commons file writes it under, "" for a file it
	// does not fetch.
	Local string
}

// WakeSnap is a farm snapshot's context: where the camera was and what it
// framed (internal/commons snap.go).
type WakeSnap struct {
	URL string
	// Place names where the camera was ("the wild", gardenio/gd-1).
	Place  string
	Z      int // 0..3, -1 when unknown
	Who    string
	Night  bool
	Framed []commons.SnapFrame
}

// Ask is the ask of a snapshot sent with no words, DefaultSnapAsk; "" when
// the person wrote one (their words are the ask) or sent no snapshot.
func (w WakeMessage) Ask() string {
	if w.Text == "" && w.Snap != nil {
		return DefaultSnapAsk
	}
	return ""
}

// wakeMessage reads the message a fire's event names, for a #general or
// mention wake: nil for any other wake, a bead src cannot read, or one that
// is not a message of land (the wake entries are fenced to the land; this
// read is fenced again so a forged event never renders another land's words).
func wakeMessage(ctx context.Context, src any, land string, ch Channels, ev bridge.FireEvent) *WakeMessage {
	if ev.Entry != WakeGeneral && ev.Entry != WakeMention || ev.BeadID == "" {
		return nil
	}
	g, ok := src.(beadGetter)
	if !ok {
		return nil
	}
	b, err := g.GetBead(ctx, ev.BeadID)
	if err != nil || b == nil || b.Type != "message" || !slices.Contains(b.Labels, "project:"+land) {
		return nil
	}
	f := b.Fields
	w := &WakeMessage{
		ID:      b.ID,
		Thread:  firstNonEmpty(strings.TrimSpace(f[conversation.BeadThreadID]), b.ID),
		Mention: ev.Entry == WakeMention,
		Author:  firstNonEmpty(f[conversation.BeadAuthor], b.CreatedBy),
		Text:    strings.TrimSpace(f[conversation.BeadText]),
	}
	if len(w.Text) > wakeTextMax {
		cut := wakeTextMax
		for cut > 0 && !utf8.RuneStart(w.Text[cut]) {
			cut--
		}
		w.Text, w.Cut = strings.TrimSpace(w.Text[:cut]), true
	}
	switch chID := f[conversation.BeadChannelID]; {
	case chID == ch.General:
		w.Channel = "general"
	case chID != "":
		if c, err := g.GetBead(ctx, chID); err == nil && c != nil && c.Type == "channel" {
			w.Channel = c.Fields["name"]
		}
	}
	var files []struct {
		Name     string `json:"name"`
		MimeType string `json:"mimetype"`
		URL      string `json:"url"`
	}
	_ = json.Unmarshal([]byte(f[conversation.BeadFiles]), &files)
	for _, file := range files {
		if len(w.Files) == wakeFilesMax {
			break
		}
		wf := WakeFile{Name: oneLine(file.Name), MimeType: oneLine(file.MimeType)}
		if _, _, machine, ok := bridge.CommonsFileRoute(file.URL); ok {
			if name, err := url.PathUnescape(path.Base(machine)); err == nil {
				wf.Local, w.Fetch = oneLine(name), true
			}
		}
		wf.Name = firstNonEmpty(wf.Name, wf.Local, "file")
		w.Files = append(w.Files, wf)
	}
	if len(w.Files) > 0 {
		w.Snap = wakeSnap(f[conversation.BeadSnap])
	}
	return w
}

// WakeJoined is the provisioner's person-joined post that woke this run (step
// 9.14): who joined and as what, in the post's words (JoinedText).
type WakeJoined struct {
	ID   string
	Text string
}

// wakeJoined reads the post a person-joined fire names: nil for any other
// wake, or a bead that is not the provisioner's person-joined post in land
// (fenced again, as wakeMessage is, so a forged event names nobody).
func wakeJoined(ctx context.Context, src any, land string, ev bridge.FireEvent) *WakeJoined {
	if ev.Entry != WakePersonJoined || ev.BeadID == "" {
		return nil
	}
	g, ok := src.(beadGetter)
	if !ok {
		return nil
	}
	b, err := g.GetBead(ctx, ev.BeadID)
	if err != nil || b == nil || b.Type != "message" || !slices.Contains(b.Labels, "project:"+land) ||
		b.Fields[conversation.BeadAuthor] != OpenedAuthor || b.Fields[conversation.BeadOrigin] != OriginPersonJoined {
		return nil
	}
	return &WakeJoined{ID: b.ID, Text: oneLine(b.Fields[conversation.BeadText])}
}

// wakeSnap is a snap field as the prompt shows it, nil when there is none.
func wakeSnap(raw string) *WakeSnap {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	c, ok := commons.ReadSnap(raw)
	if !ok {
		return nil
	}
	s := &WakeSnap{Place: commons.SnapPlace(c.At), Z: -1, Who: c.Who, Night: c.Night, Framed: c.Framed}
	if c.Z != nil {
		s.Z = *c.Z
	}
	if u, err := url.Parse(c.URL); err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" {
		s.URL = c.URL
	}
	return s
}

// describeView is how the camera looked: z2, Mine, night.
func describeView(s WakeSnap) string {
	var how []string
	if s.Z >= 0 {
		how = append(how, "zoom level "+strconv.Itoa(s.Z)+" of 3")
	}
	switch s.Who {
	case "mine":
		how = append(how, "showing the viewer's own work")
	case "all":
		how = append(how, "showing everyone's work")
	}
	if s.Night {
		how = append(how, "at night")
	}
	return strings.Join(how, ", ")
}

// quote renders a person's words as a quoted block, one "> " per line.
func quote(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight("> "+l, " ")
	}
	return strings.Join(lines, "\n")
}

// oneLine is a value on one line.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
